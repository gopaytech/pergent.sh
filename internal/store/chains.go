package store

import (
	"context"
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
		SELECT $1, u.n, u.source, u.id FROM unnest($2::text[], $3::text[]) WITH ORDINALITY AS u(source, id, n)`,
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
