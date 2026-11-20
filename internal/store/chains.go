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
// set for ScopeTeam, Instance and Project for ScopeRepo.
type Chain struct {
	ID                int64
	Scope             string
	TeamID            int64
	Instance, Project string
	Entries           []EntryRef
	UpdatedAt         time.Time
}

type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// loadChains reads the chains matching where (on model_chains c, with args),
// by ID, with their entries in position order.
func loadChains(ctx context.Context, q rowsQuerier, where string, args ...any) ([]Chain, error) {
	rows, err := q.Query(ctx, `SELECT c.id, c.scope, COALESCE(c.team_id, 0), c.instance, c.project, c.updated_at
		FROM model_chains c `+where+` ORDER BY c.id`, args...)
	if err != nil {
		return nil, err
	}
	chains, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Chain, error) {
		var c Chain
		err := row.Scan(&c.ID, &c.Scope, &c.TeamID, &c.Instance, &c.Project, &c.UpdatedAt)
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
	rows, err = q.Query(ctx, `SELECT chain_id, source, model_id FROM model_chain_entries
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
func writeChainEntries(ctx context.Context, tx pgx.Tx, chainID int64, entries []EntryRef) error {
	if _, err := tx.Exec(ctx, `DELETE FROM model_chain_entries WHERE chain_id = $1`, chainID); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	sources, ids := make([]string, len(entries)), make([]string, len(entries))
	for i, e := range entries {
		sources[i], ids[i] = e.Source, e.ID
	}
	_, err := tx.Exec(ctx, `INSERT INTO model_chain_entries (chain_id, position, source, model_id)
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
// every chain that names them, the later entries moved up, and returns those
// chains as they are now. The chains are locked first.
func removeFromChains(ctx context.Context, tx pgx.Tx, source string, ids []string, at time.Time) ([]Chain, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM model_chains WHERE id IN (SELECT chain_id FROM model_chain_entries
		WHERE source = $1 AND model_id = ANY($2)) ORDER BY id FOR UPDATE`, source, ids); err != nil {
		return nil, err
	}
	chains, err := loadChains(ctx, tx, `WHERE c.id IN (SELECT chain_id FROM model_chain_entries
		WHERE source = $1 AND model_id = ANY($2))`, source, ids)
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
		if err := writeChainEntries(ctx, tx, c.ID, kept); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE model_chains SET updated_at = $2 WHERE id = $1`, c.ID, at); err != nil {
			return nil, err
		}
		if c.Scope == ScopeRepo {
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

// Chain returns the chain of scope (teamID for ScopeTeam, instance and
// project for ScopeRepo, zero values otherwise); found is false when none is
// stored.
func (s *Store) Chain(ctx context.Context, scope string, teamID int64, instance, project string) (Chain, bool, error) {
	chains, err := loadChains(ctx, s.pool, chainWhere, scope, teamID, instance, project)
	if err != nil || len(chains) == 0 {
		return Chain{}, false, err
	}
	return chains[0], true, nil
}

// ChainsNaming returns every chain that names ref, by ID.
func (s *Store) ChainsNaming(ctx context.Context, ref EntryRef) ([]Chain, error) {
	return loadChains(ctx, s.pool, `WHERE c.id IN (SELECT chain_id FROM model_chain_entries
		WHERE source = $1 AND model_id = $2)`, ref.Source, ref.ID)
}

// SaveChain stores c's entries in place of the ones its scope target had,
// creating the chain when there is none; at is its updated_at. More than
// MaxChain entries is ErrChainTooLong; a team entry that is not there is
// ErrModelNotFound (it is locked, so it can't be removed meanwhile). Nothing
// else is checked here: who may name which entry is the caller's.
//
// The previous release reads the default and model_id, so they follow the
// chain. Global: a stored entry at position 1 becomes the default and is
// written to llm_settings (ConfigModelID or a removed entry changes nothing).
// Repo: the repository becomes custom, and model_id is position 1 when it is
// global, NULL otherwise.
func (s *Store) SaveChain(ctx context.Context, c Chain, at time.Time) error {
	if len(c.Entries) > MaxChain {
		return ErrChainTooLong
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if c.Scope == ScopeGlobal {
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
	if err := tx.QueryRow(ctx, `INSERT INTO model_chains (scope, team_id, instance, project, updated_at)
		VALUES ($1, NULLIF($2::bigint, 0), $3, $4, $5)
		ON CONFLICT (scope, COALESCE(team_id, 0), instance, lower(project)) DO UPDATE SET updated_at = EXCLUDED.updated_at
		RETURNING id`, c.Scope, c.TeamID, c.Instance, c.Project, at).Scan(&id); err != nil {
		return err
	}
	if err := writeChainEntries(ctx, tx, id, c.Entries); err != nil {
		return err
	}
	switch c.Scope {
	case ScopeGlobal:
		if len(c.Entries) > 0 && c.Entries[0].Source == SourceGlobal && c.Entries[0].ID != ConfigModelID {
			if _, err := setDefault(ctx, tx, c.Entries[0].ID, at); err != nil {
				return err
			}
		}
	case ScopeRepo:
		if _, err := tx.Exec(ctx, `UPDATE dashboard_repos SET chain_mode = 'custom'
			WHERE instance = $1 AND lower(project) = lower($2)`, c.Instance, c.Project); err != nil {
			return err
		}
		if err := syncRepoModelID(ctx, tx, c); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ResetRepoChain drops a repository's own chain: it inherits again, and its
// model_id is NULL (the default, for the previous release).
func (s *Store) ResetRepoChain(ctx context.Context, instance, project string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := setRepoModelChain(ctx, tx, instance, project, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// setRepoModelChain writes the repository chain the old model setting means:
// a model is a custom chain of that 1 entry, none is no chain and inherit.
// model_id is set to match.
func setRepoModelChain(ctx context.Context, tx pgx.Tx, instance, project string, modelID *string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM model_chains WHERE scope = 'repo' AND instance = $1 AND lower(project) = lower($2)`,
		instance, project); err != nil {
		return err
	}
	mode := "inherit"
	if modelID != nil {
		mode = "custom"
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO model_chains (scope, instance, project, updated_at)
			VALUES ('repo', $1, $2, now()) RETURNING id`, instance, project).Scan(&id); err != nil {
			return err
		}
		if err := writeChainEntries(ctx, tx, id, []EntryRef{{SourceGlobal, *modelID}}); err != nil {
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
