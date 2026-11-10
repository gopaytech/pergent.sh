package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// HoldForQuota moves a queued, or claimed and not yet dispatched, run to
// Waiting with the reason WaitQuota. A run the quota was looked at for
// (QuotaChecked: asked for, or started) is not held, nor is a start, whatever
// its flag: the previous release makes starts without it. ok is false when the
// run was in another state or unknown; a malformed id is an error.
func (s *Store) HoldForQuota(ctx context.Context, id string) (ok bool, err error) {
	if !validUUID(id) {
		return false, fmt.Errorf("store: run %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'waiting', skip_reason = $2
		WHERE id = $1::uuid AND state IN ('queued', 'running') AND dispatched_at IS NULL AND NOT quota_checked
		  AND trigger <> 'start'`, id, WaitQuota)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// markQuotaChecked marks run id, if it is not dispatched yet, as one the quota
// was looked at for: a request that landed on it was asked for by a person.
func markQuotaChecked(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE runs SET quota_checked = true WHERE id = $1::uuid AND dispatched_at IS NULL`, id)
	return err
}

// ClaimQuotaNote records that the note about a used-up quota is posted on
// mr, at now. It is true when no note was posted this month (none yet, or
// one before monthStart), so the note is posted once a month;
// ReleaseQuotaNote undoes a claim whose note couldn't be posted.
func (s *Store) ClaimQuotaNote(ctx context.Context, mr MRKey, monthStart, now time.Time) (bool, error) {
	var first bool
	err := s.pool.QueryRow(ctx, `INSERT INTO merge_requests (instance, project_id, mr_iid, quota_noted_at)
		VALUES ($1, $2, $3, $5)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET quota_noted_at = EXCLUDED.quota_noted_at
			WHERE merge_requests.quota_noted_at IS NULL OR merge_requests.quota_noted_at < $4
		RETURNING true`, mr.Instance, mr.ProjectID, mr.IID, monthStart, now).Scan(&first)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return first, err
}

// ReleaseQuotaNote forgets that the note about a used-up quota is posted on mr.
func (s *Store) ReleaseQuotaNote(ctx context.Context, mr MRKey) error {
	_, err := s.pool.Exec(ctx, `UPDATE merge_requests SET quota_noted_at = NULL
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3`, mr.Instance, mr.ProjectID, mr.IID)
	return err
}

// TokensSince returns the tokens (in plus out) the reviews of instance used
// from since on, by reported_at: those of project projectID, or, with 0, of
// every project of the instance.
func (s *Store) TokensSince(ctx context.Context, instance string, projectID int64, since time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(tokens_in), 0)::bigint + COALESCE(sum(tokens_out), 0)::bigint FROM run_usage
		WHERE instance = $1 AND ($2::bigint = 0 OR project_id = $2) AND reported_at >= $3`,
		instance, projectID, since).Scan(&n)
	return n, err
}
