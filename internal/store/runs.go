package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// MRKey identifies a merge request.
type MRKey struct {
	Instance  string
	ProjectID int64
	IID       int64
}

// RunState is where a run is in its life.
type RunState string

const (
	Queued        RunState = "queued"
	Running       RunState = "running"
	Reported      RunState = "reported"
	Published     RunState = "published"
	PublishFailed RunState = "publish_failed"
	Superseded    RunState = "superseded"
)

var (
	// ErrInvalidResult is returned by StoreResult when the body is not valid
	// JSON. The run is left untouched.
	ErrInvalidResult = errors.New("store: result is not valid JSON")
	// ErrNotRunning is returned by MarkDispatched when the run is not in
	// state running (it was superseded, or does not exist).
	ErrNotRunning = errors.New("store: run is not running")
	// ErrNotReported is returned by ReplaceWithFailure when the run is not in
	// state reported with a result (or does not exist).
	ErrNotReported = errors.New("store: run is not reported with a result")
)

// maxPublishAttempts is the number of failed publishes after which a run
// becomes publish_failed.
const maxPublishAttempts = 10

// maxPublishError bounds the stored publish error text.
const maxPublishError = 2000

// Run is one review of one head commit of a merge request.
type Run struct {
	ID                                                    string
	MR                                                    MRKey
	ProjectPath, HeadSHA, BaseSHA, StartSHA, TargetBranch string
	State                                                 RunState
	JobName                                               string
	TokenExpiresAt                                        time.Time
	Result                                                []byte
	FailureReason                                         string
	// PublishError is the text of the last failed publish, if any.
	PublishError string
	// Attempts counts dispatch retries while the run is queued or running,
	// and failed publishes once it is reported (it restarts at 0 then).
	Attempts                                       int
	CreatedAt, DispatchedAt, ReportedAt, NotBefore time.Time
	CleanedUp                                      bool
}

// ResultOutcome is what StoreResult did with a posted result.
type ResultOutcome int

const (
	ResultStored ResultOutcome = iota
	ResultDiscarded
	ResultConflict
	ResultUnauthorized
)

// StaleApproval is an approval of a commit that is no longer the MR's head.
type StaleApproval struct {
	MR          MRKey
	ApprovedSHA string
}

const runColumns = `id::text, instance, project_id, mr_iid, project_path, head_sha, base_sha, start_sha, target_branch,
	state, job_name, token_expires_at, result, failure_reason, publish_error, attempts,
	created_at, dispatched_at, reported_at, not_before, cleaned_up`

func scanRun(row pgx.Row) (Run, error) {
	var (
		r                                    Run
		state                                string
		tokenExp, dispatched, rep, notBefore *time.Time
	)
	err := row.Scan(&r.ID, &r.MR.Instance, &r.MR.ProjectID, &r.MR.IID, &r.ProjectPath, &r.HeadSHA, &r.BaseSHA, &r.StartSHA,
		&r.TargetBranch, &state, &r.JobName, &tokenExp, &r.Result, &r.FailureReason, &r.PublishError, &r.Attempts,
		&r.CreatedAt, &dispatched, &rep, &notBefore, &r.CleanedUp)
	if err != nil {
		return Run{}, err
	}
	r.State = RunState(state)
	r.TokenExpiresAt = deref(tokenExp)
	r.DispatchedAt = deref(dispatched)
	r.ReportedAt = deref(rep)
	r.NotBefore = deref(notBefore)
	return r, nil
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func scanRuns(rows pgx.Rows) ([]Run, error) {
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) getRun(ctx context.Context, id string) (Run, error) {
	return scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE id = $1::uuid`, id))
}

// lockMR takes a transaction-scoped lock on one merge request, so concurrent
// enqueues for it cannot both leave a live run behind.
func lockMR(ctx context.Context, tx pgx.Tx, mr MRKey) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("mr/%s/%d/%d", mr.Instance, mr.ProjectID, mr.IID))
	return err
}

// EnqueueRun queues a review of headSHA. It supersedes the queued, running
// and reported (not yet published) runs of the same merge request for any
// other head, so an older head's result is never posted over a newer review,
// and does not create a second run for a head that already has a non-superseded
// run: then created is false and id is that run's.
func (s *Store) EnqueueRun(ctx context.Context, mr MRKey, projectPath, headSHA, targetBranch string) (id string, created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockMR(ctx, tx, mr); err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded'
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha <> $4 AND state IN ('queued', 'running', 'reported')`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA); err != nil {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `SELECT id::text FROM runs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4 AND state <> 'superseded'`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA).Scan(&id)
	switch {
	case err == nil:
		created = false
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx, `INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, target_branch)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
			mr.Instance, mr.ProjectID, mr.IID, projectPath, headSHA, targetBranch).Scan(&id)
		if err != nil {
			return "", false, err
		}
		created = true
	default:
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return id, created, nil
}

// SupersedeMR supersedes the queued, running and reported (not yet published)
// runs of a merge request (for example when it is closed or merged) and
// returns how many it changed. It takes the same per-MR lock as EnqueueRun, so
// a close racing an enqueue cannot leave a queued run alive.
func (s *Store) SupersedeMR(ctx context.Context, mr MRKey) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockMR(ctx, tx, mr); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded'
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND state IN ('queued', 'running', 'reported')`,
		mr.Instance, mr.ProjectID, mr.IID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Supersede moves one queued or running run to superseded, for example when
// the dispatcher finds its merge request closed or its head moved. A run in
// any other state, or an unknown id, is left alone and is not an error.
func (s *Store) Supersede(ctx context.Context, id string) error {
	if !validUUID(id) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'superseded'
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id)
	return err
}

// CountRunning returns the number of runs in state running.
func (s *Store) CountRunning(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE state = 'running'`).Scan(&n)
	return n, err
}

// ClaimQueued moves up to limit queued runs that are due at now to running and
// returns them, oldest first. Concurrent callers never claim the same run.
func (s *Store) ClaimQueued(ctx context.Context, limit int, now time.Time) ([]Run, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `UPDATE runs SET state = 'running' WHERE id IN (
			SELECT id FROM runs WHERE state = 'queued' AND (not_before IS NULL OR not_before <= $2)
			ORDER BY created_at, id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING `+runColumns, limit, now)
	if err != nil {
		return nil, err
	}
	runs, err := scanRuns(rows)
	if err != nil {
		return nil, err
	}
	sortRuns(runs)
	return runs, nil
}

func sortRuns(runs []Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		return runs[i].ID < runs[j].ID
	})
}

// Requeue puts a claimed run back in the queue, not to be claimed before
// notBefore, and counts the attempt.
func (s *Store) Requeue(ctx context.Context, id string, notBefore time.Time) error {
	return s.mustUpdate(ctx, id, `UPDATE runs SET state = 'queued', not_before = $2, attempts = attempts + 1
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id, notBefore)
}

// Postpone puts a claimed run back in the queue, not to be claimed before
// notBefore, without counting an attempt: nothing failed, the run only has to
// wait (GitLab is still computing the diff, or the dispatcher is shutting
// down). A run that is no longer queued or running, or an unknown id, is left
// alone and is not an error.
func (s *Store) Postpone(ctx context.Context, id string, notBefore time.Time) error {
	if !validUUID(id) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'queued', not_before = $2
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id, notBefore)
	return err
}

// MarkDispatched records the Job started for a running run and the SHA-256 of
// its result token, valid until expires. It returns ErrNotRunning when the run
// is not in state running (for example it was superseded after it was claimed).
func (s *Store) MarkDispatched(ctx context.Context, id, jobName, baseSHA, startSHA string, tokenHash []byte, expires, now time.Time) error {
	if !validUUID(id) {
		return ErrNotRunning
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET job_name = $2, base_sha = $3, start_sha = $4,
		token_hash = $5, token_expires_at = $6, dispatched_at = $7 WHERE id = $1::uuid AND state = 'running'`,
		id, jobName, baseSHA, startSHA, tokenHash, expires, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotRunning
	}
	return nil
}

// mustUpdate runs an UPDATE that has to touch exactly one run.
func (s *Store) mustUpdate(ctx context.Context, id, sql string, args ...any) error {
	if !validUUID(id) {
		return fmt.Errorf("store: run %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: run %s not found or in the wrong state", id)
	}
	return nil
}

// validUUID reports whether id is in the canonical 8-4-4-4-12 hex form.
func validUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// StoreResult accepts the result a Job posted. The token is checked against
// the stored SHA-256 in constant time and must not be expired. The token hash
// is kept after the result, so single use is enforced by the run's state. The
// outcome, in order:
//   - ResultUnauthorized: unknown run, no token, wrong token or expired;
//   - ResultDiscarded: the run was superseded; the result is dropped;
//   - ResultConflict: the run already has a result or failure, or is not running;
//   - ErrInvalidResult: body is not valid JSON; nothing is stored;
//   - ResultStored: the run is now reported.
func (s *Store) StoreResult(ctx context.Context, id string, token string, body []byte, now time.Time) (ResultOutcome, error) {
	if !validUUID(id) {
		return ResultUnauthorized, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var (
		stored  []byte
		expires *time.Time
		state   string
		hasRes  bool
		failure string
	)
	err = tx.QueryRow(ctx, `SELECT token_hash, token_expires_at, state, result IS NOT NULL, failure_reason
		FROM runs WHERE id = $1::uuid FOR UPDATE`, id).Scan(&stored, &expires, &state, &hasRes, &failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResultUnauthorized, nil
	}
	if err != nil {
		return 0, err
	}
	presented := sha256.Sum256([]byte(token))
	if stored == nil || subtle.ConstantTimeCompare(presented[:], stored) != 1 {
		return ResultUnauthorized, nil
	}
	if expires == nil || now.After(*expires) {
		return ResultUnauthorized, nil
	}

	switch {
	case RunState(state) == Superseded:
		return ResultDiscarded, nil
	case hasRes || failure != "" || RunState(state) != Running:
		return ResultConflict, nil
	}
	if !json.Valid(body) {
		return 0, ErrInvalidResult
	}

	if _, err := tx.Exec(ctx, `UPDATE runs SET result = $2::jsonb, state = 'reported',
		reported_at = $3, not_before = $3, attempts = 0 WHERE id = $1::uuid`, id, string(body), now); err != nil {
		return 0, fmt.Errorf("store: save result: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return ResultStored, nil
}

// ReportFailure records that a queued or running run will never produce a
// result (the Job died, the inputs did not fit, ...) so it is reported with
// reason. It reports whether it changed the run; a run that already has a
// result or a failure is left alone.
func (s *Store) ReportFailure(ctx context.Context, id, reason string, now time.Time) (bool, error) {
	if !validUUID(id) {
		return false, nil
	}
	if reason == "" {
		reason = "review failed"
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'reported', failure_reason = $2,
		reported_at = $3, not_before = $3, attempts = 0
		WHERE id = $1::uuid AND state IN ('queued', 'running') AND result IS NULL AND failure_reason = ''`,
		id, reason, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Run returns one run. A malformed or unknown id is reported as not found.
func (s *Store) Run(ctx context.Context, id string) (Run, bool, error) {
	if !validUUID(id) {
		return Run{}, false, nil
	}
	r, err := s.getRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return r, true, nil
}

// ReplaceWithFailure turns the stored result of a reported run into a failure
// with reason: it clears the result and keeps the state reported, so the
// publisher posts an error comment. It returns ErrNotReported when the run is
// not reported with a result.
func (s *Store) ReplaceWithFailure(ctx context.Context, id, reason string) error {
	if !validUUID(id) {
		return ErrNotReported
	}
	if reason == "" {
		reason = "review failed"
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET result = NULL, failure_reason = $2
		WHERE id = $1::uuid AND state = 'reported' AND result IS NOT NULL`, id, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotReported
	}
	return nil
}

// RunsToPublish returns reported runs that are due at now, oldest first.
func (s *Store) RunsToPublish(ctx context.Context, now time.Time, limit int) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE state = 'reported' AND (not_before IS NULL OR not_before <= $1)
		ORDER BY reported_at, created_at, id LIMIT $2`, now, limit)
}

func (s *Store) queryRuns(ctx context.Context, sql string, args ...any) ([]Run, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// MarkPublished records that a reported run's comment and status are out.
func (s *Store) MarkPublished(ctx context.Context, id string) error {
	return s.mustUpdate(ctx, id, `UPDATE runs SET state = 'published', publish_error = ''
		WHERE id = $1::uuid AND state = 'reported'`, id)
}

// PublishRetry records a failed publish: the run is tried again at notBefore,
// or becomes publish_failed when this was the 10th failed attempt.
func (s *Store) PublishRetry(ctx context.Context, id, errText string, notBefore time.Time) error {
	if len(errText) > maxPublishError {
		errText = errText[:maxPublishError]
	}
	return s.mustUpdate(ctx, id, `UPDATE runs SET attempts = attempts + 1, publish_error = $2, not_before = $3,
		state = CASE WHEN attempts + 1 >= $4 THEN 'publish_failed' ELSE state END
		WHERE id = $1::uuid AND state = 'reported'`, id, errText, notBefore, maxPublishAttempts)
}

// RunsToCleanUp returns superseded runs whose Job has not been cleaned up.
func (s *Store) RunsToCleanUp(ctx context.Context) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE state = 'superseded' AND NOT cleaned_up ORDER BY created_at, id`)
}

// MarkCleanedUp records that a superseded run's Job is gone.
func (s *Store) MarkCleanedUp(ctx context.Context, id string) error {
	return s.mustUpdate(ctx, id, `UPDATE runs SET cleaned_up = true WHERE id = $1::uuid`, id)
}

// RunningRuns returns every run in state running.
func (s *Store) RunningRuns(ctx context.Context) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs WHERE state = 'running' ORDER BY created_at, id`)
}

// PreviousResult returns the result of the latest published run of the merge
// request whose status is "completed", other than beforeRunID.
func (s *Store) PreviousResult(ctx context.Context, mr MRKey, beforeRunID string) ([]byte, bool, error) {
	var res []byte
	err := s.pool.QueryRow(ctx, `SELECT result FROM runs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND state = 'published'
		  AND result->>'status' = 'completed' AND id::text <> $4
		ORDER BY created_at DESC, id DESC LIMIT 1`,
		mr.Instance, mr.ProjectID, mr.IID, beforeRunID).Scan(&res)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return res, true, nil
}

// NoteID returns the id of the merge request's pergent comment, or 0.
func (s *Store) NoteID(ctx context.Context, mr MRKey) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT note_id FROM merge_requests
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3`, mr.Instance, mr.ProjectID, mr.IID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// SetNoteID records the id of the merge request's pergent comment.
func (s *Store) SetNoteID(ctx context.Context, mr MRKey, noteID int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO merge_requests (instance, project_id, mr_iid, note_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET note_id = EXCLUDED.note_id`,
		mr.Instance, mr.ProjectID, mr.IID, noteID)
	return err
}

// SetApprovedSHA records the commit the bot approved; "" clears it.
func (s *Store) SetApprovedSHA(ctx context.Context, mr MRKey, sha string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO merge_requests (instance, project_id, mr_iid, approved_sha)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET approved_sha = EXCLUDED.approved_sha`,
		mr.Instance, mr.ProjectID, mr.IID, sha)
	return err
}

// StaleApprovals returns merge requests whose recorded approval is for a
// commit other than the head of their newest non-superseded run.
func (s *Store) StaleApprovals(ctx context.Context) ([]StaleApproval, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.instance, m.project_id, m.mr_iid, m.approved_sha
		FROM merge_requests m
		WHERE m.approved_sha <> '' AND m.approved_sha <> (
			SELECT r.head_sha FROM runs r
			WHERE r.instance = m.instance AND r.project_id = m.project_id AND r.mr_iid = m.mr_iid
			  AND r.state <> 'superseded'
			ORDER BY r.created_at DESC, r.id DESC LIMIT 1)
		ORDER BY m.instance, m.project_id, m.mr_iid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaleApproval
	for rows.Next() {
		var a StaleApproval
		if err := rows.Scan(&a.MR.Instance, &a.MR.ProjectID, &a.MR.IID, &a.ApprovedSHA); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
