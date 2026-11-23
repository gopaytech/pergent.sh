package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RunEntry is the chain entry a run is dispatched on: its source and ID, its
// name then, the team of a team entry (0 otherwise), its place in the chain
// in effect and that chain's length, and why position 1 was not used ("" when
// it was).
type RunEntry struct {
	Source, ID, Name string
	TeamID           int64
	Position, Length int
	FallbackClass    string
}

// SetRunEntry records the entry of a run being dispatched (queued or
// running), before its Job starts. Any other run is an error.
func (s *Store) SetRunEntry(ctx context.Context, id string, e RunEntry) error {
	return s.mustUpdate(ctx, id, `UPDATE runs SET model_source = $2, model_entry_id = $3, model_entry_name = $4,
			model_team_id = NULLIF($5::bigint, 0), chain_position = $6, chain_length = $7, fallback_class = $8
		WHERE id = $1::uuid AND state IN ('queued', 'running')`,
		id, e.Source, e.ID, cutText(e.Name), e.TeamID, e.Position, e.Length, e.FallbackClass)
}

// SetRunStatusRef records the platform status a run set (a GitHub Check Run
// ID), so its later attempts update the same one. Any run may get it.
func (s *Store) SetRunStatusRef(ctx context.Context, id string, ref int64) error {
	return s.mustUpdate(ctx, id, `UPDATE runs SET status_ref = $2 WHERE id = $1::uuid`, id, ref)
}

// EnqueueAttempt drops run from for the next entry of its chain, under the
// merge request's lock, in one transaction: it stores m (the entry the
// provider refused), closes from as superseded with OutcomeFellBack and the
// class as its reason, and queues the next attempt. The attempt reviews the
// same head and target, copies what the first attempt was asked for (kind
// and API client, trigger, skill subset, base run, quota and CI checked, the
// shared status)
// and points at the review's first run. That is only done while from is
// still reported: a newer push, or a result posted twice, finds it otherwise,
// and then created is false and nothing changes.
func (s *Store) EnqueueAttempt(ctx context.Context, from Run, m Mark) (id string, created bool, err error) {
	return s.enqueueNextAttempt(ctx, from, m.Class, &m)
}

// enqueueNextAttempt is EnqueueAttempt and EnqueueJudgeRetry: under the merge
// request's lock, in one transaction, while from is reported, it stores the
// mark m (none when m is nil), closes from as superseded with
// OutcomeFellBack and reason, and queues the review's next attempt with
// what from was asked for. Both callers copy the same columns because they
// share this one INSERT.
func (s *Store) enqueueNextAttempt(ctx context.Context, from Run, reason string, m *Mark) (id string, created bool, err error) {
	if !validUUID(from.ID) {
		return "", false, fmt.Errorf("store: run %q not found", from.ID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockMR(ctx, tx, from.MR); err != nil {
		return "", false, err
	}
	var state string
	err = tx.QueryRow(ctx, `SELECT state FROM runs WHERE id = $1::uuid FOR UPDATE`, from.ID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if RunState(state) != Reported {
		return "", false, nil
	}
	if m != nil {
		if err := upsertMark(ctx, tx, *m); err != nil {
			return "", false, err
		}
	}
	// Superseded first: one live run per head and target.
	if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded', outcome = $2, outcome_reason = $3 WHERE id = $1::uuid`,
		from.ID, OutcomeFellBack, reason); err != nil {
		return "", false, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, target_branch,
			kind, api_client_id, trigger, skill_subset, base_run_id, quota_checked, ci_checked, status_ref, attempt_of)
		SELECT instance, project_id, mr_iid, project_path, head_sha, target_branch,
			kind, api_client_id, trigger, skill_subset, base_run_id, quota_checked, ci_checked, status_ref, COALESCE(attempt_of, id)
		FROM runs WHERE id = $1::uuid
		RETURNING id::text`, from.ID).Scan(&id); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// Attempts returns the review whose first run is root: root and its later
// attempts, oldest first.
func (s *Store) Attempts(ctx context.Context, root string) ([]Run, error) {
	if !validUUID(root) {
		return nil, nil
	}
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs WHERE id = $1::uuid OR attempt_of = $1::uuid
		ORDER BY created_at, id`, root)
}
