package store

import (
	"context"
	"fmt"
)

// LegacyPanel counts what a main build of the review panel left: chain B
// rows with entries, and panel_members rows. Nothing reads them any more.
func (s *Store) LegacyPanel(ctx context.Context) (chains, members int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM panel_chains c WHERE EXISTS (SELECT 1 FROM panel_chain_entries e WHERE e.chain_id = c.id)),
		(SELECT count(*) FROM panel_members)`).Scan(&chains, &members)
	return chains, members, err
}

// RequeueLegacyPanels queues each run waiting on 'panel_members' again as a
// single review (no skip reason, not_before NULL; dispatched_at stays, so
// it is never split), supersedes the live panel_members of those reviews,
// and returns their Job names, in one transaction. Runs of a multi review
// (WaitReviewers) are not touched.
func (s *Store) RequeueLegacyPanels(ctx context.Context) (runs int, jobs []string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	rows, err := tx.Query(ctx, `UPDATE runs SET state = 'queued', skip_reason = '', not_before = NULL
		WHERE state = 'waiting' AND skip_reason = $1
		RETURNING COALESCE(attempt_of, id)::text`, WaitPanel)
	if err != nil {
		return 0, nil, err
	}
	var roots []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			rows.Close()
			return 0, nil, err
		}
		roots = append(roots, root)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if len(roots) == 0 {
		return 0, nil, nil
	}
	rows, err = tx.Query(ctx, `UPDATE panel_members SET state = 'superseded'
		WHERE run_id = ANY($1::uuid[]) AND state IN ('queued', 'running', 'reported')
		RETURNING job_name`, roots)
	if err != nil {
		return 0, nil, err
	}
	for rows.Next() {
		var job string
		if err := rows.Scan(&job); err != nil {
			rows.Close()
			return 0, nil, err
		}
		if job != "" {
			jobs = append(jobs, job)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, fmt.Errorf("store: requeue legacy panels: %w", err)
	}
	return len(roots), jobs, nil
}
