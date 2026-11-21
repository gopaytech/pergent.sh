package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Where a chain entry comes from: the global list (llm_models, or
// ConfigModelID) or a team's own entries (team_models).
const (
	SourceGlobal = "global"
	SourceTeam   = "team"
)

// The scopes a chain belongs to.
const (
	ScopeGlobal = "global"
	ScopeTeam   = "team"
	ScopeRepo   = "repo"
)

// EntryRef names a chain entry: its source and its ID there.
type EntryRef struct{ Source, ID string }

// Chain is an ordered list of entries, Entries[0] being position 1. TeamID is
// set for ScopeTeam, Instance and Project for ScopeRepo. SavedByAdmin is
// whether an admin saved it last: a repository chain an admin saved may name
// global entries teams may not use.
type Chain struct {
	ID                int64
	Scope             string
	TeamID            int64
	Instance, Project string
	Entries           []EntryRef
	SavedByAdmin      bool
	UpdatedAt         time.Time
}

// ChainSet is which chain a chain row belongs to: ChainA, the review's model
// chain, or ChainB, a review panel's second chain. Its table and column names
// are fixed here and never come from input; the chain SQL is built from them.
type ChainSet struct{ chains, entries, mode string }

var (
	ChainA = ChainSet{chains: "model_chains", entries: "model_chain_entries", mode: "chain_mode"}
	ChainB = ChainSet{chains: "panel_chains", entries: "panel_chain_entries", mode: "panel_chain_mode"}
)

// A repository's chain B mode (DashboardRepo.PanelChainMode).
const (
	PanelInherit = "inherit"
	PanelCustom  = "custom"
	PanelOff     = "off"
)

type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// loadChains reads the chains of set matching where (on the set's chains
// table as c, with args), by ID, with their entries in position order.
func loadChains(ctx context.Context, q rowsQuerier, set ChainSet, where string, args ...any) ([]Chain, error) {
	rows, err := q.Query(ctx, `SELECT c.id, c.scope, COALESCE(c.team_id, 0), c.instance, c.project, c.saved_by_admin, c.updated_at
		FROM `+set.chains+` c `+where+` ORDER BY c.id`, args...)
	if err != nil {
		return nil, err
	}
	chains, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Chain, error) {
		var c Chain
		err := row.Scan(&c.ID, &c.Scope, &c.TeamID, &c.Instance, &c.Project, &c.SavedByAdmin, &c.UpdatedAt)
		return c, err
	})
	if err != nil || len(chains) == 0 {
		return chains, err
	}
	ids := make([]int64, len(chains))
	byID := make(map[int64]*Chain, len(chains))
	for i := range chains {
		ids[i] = chains[i].ID
		byID[chains[i].ID] = &chains[i]
	}
	rows, err = q.Query(ctx, `SELECT chain_id, source, model_id FROM `+set.entries+`
		WHERE chain_id = ANY($1) ORDER BY chain_id, position`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var e EntryRef
		if err := rows.Scan(&id, &e.Source, &e.ID); err != nil {
			return nil, err
		}
		byID[id].Entries = append(byID[id].Entries, e)
	}
	return chains, rows.Err()
}

// writeChainEntries replaces a chain's entries with entries, numbered from 1.
// It deletes and inserts rather than moving positions, which the primary key
// would refuse one row at a time.
func writeChainEntries(ctx context.Context, tx pgx.Tx, set ChainSet, chainID int64, entries []EntryRef) error {
	if _, err := tx.Exec(ctx, `DELETE FROM `+set.entries+` WHERE chain_id = $1`, chainID); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	sources, ids := make([]string, len(entries)), make([]string, len(entries))
	for i, e := range entries {
		sources[i], ids[i] = e.Source, e.ID
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+set.entries+` (chain_id, position, source, model_id)
		SELECT $1::bigint, u.n, u.source, u.id FROM unnest($2::text[], $3::text[]) WITH ORDINALITY AS u(source, id, n)`,
		chainID, sources, ids)
	return err
}

// syncRepoModelID keeps dashboard_repos.model_id, which the previous release
// reads, in step with a custom repository chain: position 1 when it is a
// global entry, NULL when it is a team entry or the chain is empty.
func syncRepoModelID(ctx context.Context, tx pgx.Tx, c Chain) error {
	var modelID *string
	if len(c.Entries) > 0 && c.Entries[0].Source == SourceGlobal {
		modelID = &c.Entries[0].ID
	}
	_, err := tx.Exec(ctx, `UPDATE dashboard_repos SET model_id = $3
		WHERE instance = $1 AND lower(project) = lower($2) AND chain_mode = 'custom'`, c.Instance, c.Project, modelID)
	return err
}

// removeFromChains takes the entries of source with the given IDs out of
// every chain of set that names them, the later entries moved up, and
// returns those chains as they are now. The chains are locked first. A caller
// runs it once per set, ChainA first.
func removeFromChains(ctx context.Context, tx pgx.Tx, set ChainSet, source string, ids []string, at time.Time) ([]Chain, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	naming := `id IN (SELECT chain_id FROM ` + set.entries + ` WHERE source = $1 AND model_id = ANY($2))`
	if _, err := tx.Exec(ctx, `SELECT id FROM `+set.chains+` WHERE `+naming+` ORDER BY id FOR UPDATE`, source, ids); err != nil {
		return nil, err
	}
	chains, err := loadChains(ctx, tx, set, `WHERE c.`+naming, source, ids)
	if err != nil {
		return nil, err
	}
	gone := make(map[string]bool, len(ids))
	for _, id := range ids {
		gone[id] = true
	}
	for i := range chains {
		c := &chains[i]
		var kept []EntryRef
		for _, e := range c.Entries {
			if e.Source != source || !gone[e.ID] {
				kept = append(kept, e)
			}
		}
		c.Entries, c.UpdatedAt = kept, at
		if err := writeChainEntries(ctx, tx, set, c.ID, kept); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE `+set.chains+` SET updated_at = $2 WHERE id = $1`, c.ID, at); err != nil {
			return nil, err
		}
		if set == ChainA && c.Scope == ScopeRepo {
			if err := syncRepoModelID(ctx, tx, *c); err != nil {
				return nil, err
			}
		}
	}
	return chains, nil
}

// MaxChain is the most entries a chain holds.
const MaxChain = 5

// The classes of a mark: what the provider said about the entry.
const (
	ClassQuota   = "quota"
	ClassBilling = "billing"
	ClassAuth    = "auth"
)

// ErrChainTooLong is returned when a chain has more than MaxChain entries.
var ErrChainTooLong = errors.New("store: the chain has more than 5 entries")

// chainWhere matches the chain of one scope target, the project ignoring
// case.
const chainWhere = `WHERE c.scope = $1 AND COALESCE(c.team_id, 0) = $2 AND c.instance = $3 AND lower(c.project) = lower($4)`

// Chain is ChainOf for ChainA.
func (s *Store) Chain(ctx context.Context, scope string, teamID int64, instance, project string) (Chain, bool, error) {
	return s.ChainOf(ctx, ChainA, scope, teamID, instance, project)
}

// ChainOf returns the chain of set at scope (teamID for ScopeTeam, instance
// and project for ScopeRepo, zero values otherwise); found is false when
// none is stored.
func (s *Store) ChainOf(ctx context.Context, set ChainSet, scope string, teamID int64, instance, project string) (Chain, bool, error) {
	chains, err := loadChains(ctx, s.pool, set, chainWhere, scope, teamID, instance, project)
	if err != nil || len(chains) == 0 {
		return Chain{}, false, err
	}
	return chains[0], true, nil
}

// Chains is ChainsOf for ChainA.
func (s *Store) Chains(ctx context.Context) ([]Chain, error) {
	return s.ChainsOf(ctx, ChainA)
}

// ChainsOf returns every chain of set, by ID.
func (s *Store) ChainsOf(ctx context.Context, set ChainSet) ([]Chain, error) {
	return loadChains(ctx, s.pool, set, ``)
}

// ChainsNaming is ChainsNamingOf for ChainA.
func (s *Store) ChainsNaming(ctx context.Context, ref EntryRef) ([]Chain, error) {
	return s.ChainsNamingOf(ctx, ChainA, ref)
}

// ChainsNamingOf returns every chain of set that names ref, by ID.
func (s *Store) ChainsNamingOf(ctx context.Context, set ChainSet, ref EntryRef) ([]Chain, error) {
	return loadChains(ctx, s.pool, set, `WHERE c.id IN (SELECT chain_id FROM `+set.entries+`
		WHERE source = $1 AND model_id = $2)`, ref.Source, ref.ID)
}

// SaveChain is SaveChainOf for ChainA.
func (s *Store) SaveChain(ctx context.Context, c Chain, at time.Time) error {
	return s.SaveChainOf(ctx, ChainA, c, at)
}

// SaveChainOf stores c's entries in place of the ones its scope target had
// in set, creating the chain when there is none; at is its updated_at, and
// c.SavedByAdmin replaces what the chain had. More than MaxChain entries is
// ErrChainTooLong; a team entry that is not there is ErrModelNotFound (it is
// locked, so it can't be removed meanwhile). Nothing else is checked here:
// who may name which entry is the caller's. A repository save makes the
// set's mode of the repository custom.
//
// The previous release reads the default and model_id, so for ChainA they
// follow the chain. Global: a stored entry at position 1 becomes the default
// and is written to llm_settings (ConfigModelID or a removed entry changes
// nothing). Repo: model_id is position 1 when it is global, NULL otherwise.
// ChainB writes neither: the previous release has no panel.
func (s *Store) SaveChainOf(ctx context.Context, set ChainSet, c Chain, at time.Time) error {
	if len(c.Entries) > MaxChain {
		return ErrChainTooLong
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if set == ChainA && c.Scope == ScopeGlobal {
		if err := lockModelList(ctx, tx); err != nil {
			return err
		}
	}
	// Entries before the chain, the order DeleteTeamModel takes them in.
	for _, e := range c.Entries {
		if e.Source != SourceTeam {
			continue
		}
		var found string
		err := tx.QueryRow(ctx, `SELECT id FROM team_models WHERE id = $1 FOR KEY SHARE`, e.ID).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrModelNotFound
		}
		if err != nil {
			return err
		}
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO `+set.chains+` (scope, team_id, instance, project, saved_by_admin, updated_at)
		VALUES ($1, NULLIF($2::bigint, 0), $3, $4, $5, $6)
		ON CONFLICT (scope, COALESCE(team_id, 0), instance, lower(project))
		DO UPDATE SET saved_by_admin = EXCLUDED.saved_by_admin, updated_at = EXCLUDED.updated_at
		RETURNING id`, c.Scope, c.TeamID, c.Instance, c.Project, c.SavedByAdmin, at).Scan(&id); err != nil {
		return err
	}
	if err := writeChainEntries(ctx, tx, set, id, c.Entries); err != nil {
		return err
	}
	switch c.Scope {
	case ScopeGlobal:
		if set == ChainA && len(c.Entries) > 0 && c.Entries[0].Source == SourceGlobal && c.Entries[0].ID != ConfigModelID {
			if _, err := setDefault(ctx, tx, c.Entries[0].ID, at); err != nil {
				return err
			}
		}
	case ScopeRepo:
		if _, err := tx.Exec(ctx, `UPDATE dashboard_repos SET `+set.mode+` = 'custom'
			WHERE instance = $1 AND lower(project) = lower($2)`, c.Instance, c.Project); err != nil {
			return err
		}
		if set == ChainA {
			if err := syncRepoModelID(ctx, tx, c); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// ResetRepoChain is ResetRepoChainOf for ChainA.
func (s *Store) ResetRepoChain(ctx context.Context, instance, project string) error {
	return s.ResetRepoChainOf(ctx, ChainA, instance, project)
}

// ResetRepoChainOf drops a repository's own chain of set: it inherits
// again. For ChainA its model_id is NULL (the default, for the previous
// release); for ChainB the mode is inherit from custom or off.
func (s *Store) ResetRepoChainOf(ctx context.Context, set ChainSet, instance, project string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if set == ChainA {
		err = setRepoModelChain(ctx, tx, instance, project, nil)
	} else {
		err = setRepoPanelMode(ctx, tx, instance, project, PanelInherit)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RepoPanelMode is the repository's chain B mode; PanelInherit for a
// repository with no dashboard row.
func (s *Store) RepoPanelMode(ctx context.Context, instance, project string) (string, error) {
	var mode string
	err := s.pool.QueryRow(ctx, `SELECT panel_chain_mode FROM dashboard_repos
		WHERE instance = $1 AND lower(project) = lower($2)`, instance, project).Scan(&mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return PanelInherit, nil
	}
	return mode, err
}

// SetRepoPanelOff turns the panel off for the repository: its chain B row
// goes and its mode is PanelOff, in one transaction. A repository with no
// dashboard row keeps no mode, as a chain save keeps none. at is not stored:
// with the row gone there is nothing to date.
func (s *Store) SetRepoPanelOff(ctx context.Context, instance, project string, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := setRepoPanelMode(ctx, tx, instance, project, PanelOff); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// setRepoPanelMode drops the repository's chain B row and sets its chain B
// mode to mode (PanelInherit or PanelOff, neither of which keeps a row).
func setRepoPanelMode(ctx context.Context, tx pgx.Tx, instance, project, mode string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM `+ChainB.chains+` WHERE scope = 'repo' AND instance = $1 AND lower(project) = lower($2)`,
		instance, project); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE dashboard_repos SET `+ChainB.mode+` = $3
		WHERE instance = $1 AND lower(project) = lower($2)`, instance, project, mode)
	return err
}

// setRepoModelChain writes the repository chain the old model setting means:
// a model is a custom chain of that 1 entry, none is no chain and inherit.
// model_id is set to match. Only an admin changes that setting, so the chain
// is admin-saved.
func setRepoModelChain(ctx context.Context, tx pgx.Tx, instance, project string, modelID *string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM model_chains WHERE scope = 'repo' AND instance = $1 AND lower(project) = lower($2)`,
		instance, project); err != nil {
		return err
	}
	mode := "inherit"
	if modelID != nil {
		mode = "custom"
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO model_chains (scope, instance, project, saved_by_admin, updated_at)
			VALUES ('repo', $1, $2, true, now()) RETURNING id`, instance, project).Scan(&id); err != nil {
			return err
		}
		if err := writeChainEntries(ctx, tx, ChainA, id, []EntryRef{{SourceGlobal, *modelID}}); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE dashboard_repos SET chain_mode = $3, model_id = $4
		WHERE instance = $1 AND lower(project) = lower($2)`, instance, project, mode, modelID)
	return err
}

// Mark is what the provider said about an entry: out of quota or credit until
// Until, or its key rejected (ClassAuth, Until nil: until the key is edited).
type Mark struct {
	Ref   EntryRef
	Class string
	Until *time.Time
	SetAt time.Time
}

// Marks returns every mark, live or past its until, by source and ID.
func (s *Store) Marks(ctx context.Context) ([]Mark, error) {
	rows, err := s.pool.Query(ctx, `SELECT source, model_id, class, until, set_at FROM model_entry_marks ORDER BY source, model_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Mark, error) {
		var m Mark
		err := row.Scan(&m.Ref.Source, &m.Ref.ID, &m.Class, &m.Until, &m.SetAt)
		return m, err
	})
}

// ClearMark drops ref's mark; cleared is false when it had none.
func (s *Store) ClearMark(ctx context.Context, ref EntryRef) (cleared bool, err error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM model_entry_marks WHERE source = $1 AND model_id = $2`, ref.Source, ref.ID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// upsertMark stores m, so 2 runs that mark the same entry both succeed. An
// auth mark wins over a quota or billing one (the key is wrong whatever the
// quota), and of two others the later until wins.
func upsertMark(ctx context.Context, tx pgx.Tx, m Mark) error {
	_, err := tx.Exec(ctx, `INSERT INTO model_entry_marks AS k (source, model_id, class, until, set_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (source, model_id) DO UPDATE SET
			class = CASE WHEN k.class = 'auth' OR EXCLUDED.class = 'auth' THEN 'auth'
				WHEN EXCLUDED.until >= k.until THEN EXCLUDED.class ELSE k.class END,
			until = CASE WHEN k.class = 'auth' OR EXCLUDED.class = 'auth' THEN NULL
				ELSE GREATEST(k.until, EXCLUDED.until) END,
			set_at = GREATEST(k.set_at, EXCLUDED.set_at)`,
		m.Ref.Source, m.Ref.ID, m.Class, m.Until, m.SetAt)
	return err
}

// SetMark stores m on its own (upsertMark's rule): an API review marks the
// entry the provider refused with no attempt to queue.
func (s *Store) SetMark(ctx context.Context, m Mark) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := upsertMark(ctx, tx, m); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
