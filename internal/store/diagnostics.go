package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Failure is why a run will never produce a result. Reason is the text for
// the comment. Step is where it failed ("" for a failure before any Job) and
// Detail is more text; both are for admins only.
type Failure struct {
	Reason, Step, Detail string
}

// SetRunPod records the name of the pod that runs a run's Job.
func (s *Store) SetRunPod(ctx context.Context, id, pod string) error {
	if !validUUID(id) {
		return errors.New("store: malformed run id")
	}
	_, err := s.pool.Exec(ctx, `UPDATE runs SET pod_name = $2 WHERE id = $1::uuid`, id, pod)
	return err
}

// AdvanceCheckoutSeen records how far a run's checkout was seen to get: 1
// running, 2 done. It only raises the value.
func (s *Store) AdvanceCheckoutSeen(ctx context.Context, id string, seen int) error {
	if !validUUID(id) {
		return errors.New("store: malformed run id")
	}
	_, err := s.pool.Exec(ctx, `UPDATE runs SET checkout_seen = $2
		WHERE id = $1::uuid AND checkout_seen < $2`, id, seen)
	return err
}

// Diagnostics is what a failed review Job left: the clone log, the same log
// with linked repositories' URLs masked, and the pod's state with its events.
// All of it is redacted before it gets here.
type Diagnostics struct {
	CloneLog, CloneLogMasked, PodState string
}

// PutRunDiagnostics stores a run's diagnostics, replacing earlier ones, made
// fit for a text column with CleanText.
func (s *Store) PutRunDiagnostics(ctx context.Context, runID string, d Diagnostics) error {
	if !validUUID(runID) {
		return errors.New("store: malformed run id")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO run_diagnostics (run_id, clone_log, clone_log_masked, pod_state)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (run_id) DO UPDATE SET clone_log = EXCLUDED.clone_log,
			clone_log_masked = EXCLUDED.clone_log_masked, pod_state = EXCLUDED.pod_state, created_at = now()`,
		runID, CleanText(d.CloneLog), CleanText(d.CloneLogMasked), CleanText(d.PodState))
	return err
}

// RunDiagnostics returns a run's stored diagnostics. A run without any, or a
// malformed id, is reported as not found.
func (s *Store) RunDiagnostics(ctx context.Context, runID string) (Diagnostics, bool, error) {
	if !validUUID(runID) {
		return Diagnostics{}, false, nil
	}
	var d Diagnostics
	err := s.pool.QueryRow(ctx, `SELECT clone_log, clone_log_masked, pod_state
		FROM run_diagnostics WHERE run_id = $1::uuid`, runID).Scan(&d.CloneLog, &d.CloneLogMasked, &d.PodState)
	if errors.Is(err, pgx.ErrNoRows) {
		return Diagnostics{}, false, nil
	}
	if err != nil {
		return Diagnostics{}, false, err
	}
	return d, true, nil
}

// CountFailedRunsAtStep counts the runs of project on instance (matched
// ignoring case) reported since the given time that failed at step.
func (s *Store) CountFailedRunsAtStep(ctx context.Context, instance, project, step string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs
		WHERE instance = $1 AND lower(project_path) = lower($2)
		  AND reported_at >= $4 AND failure_step = $3`,
		instance, project, step, since).Scan(&n)
	return n, err
}
