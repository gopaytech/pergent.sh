package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ScanState is where a readiness scan stands.
type ScanState string

const (
	ScanQueued  ScanState = "queued"
	ScanRunning ScanState = "running"
	ScanDone    ScanState = "done"
	ScanFailed  ScanState = "failed"
)

// What started a scan nobody asked for (ReadinessScan.StartedBy); a scan
// someone asked for holds that actor.
const (
	StartedBySwitchOn = "switch-on"
	StartedByUpgrade  = "upgrade"
)

// ErrReadinessScanNotFound is returned for a scan that does not exist, or is
// not in the state the call needs (running, for SetReadinessPod and
// StartReadinessJob).
var ErrReadinessScanNotFound = errors.New("store: readiness scan not found")

// readinessScanLock serialises ClaimReadinessScan, so two claimers cannot
// both count a free place and take it. It is a transaction-level advisory
// lock, so it is gone with the transaction.
const readinessScanLock = 0x7265_6164 // "read"

// ReadinessScan is one scan of a repository. Project keeps the spelling it
// was queued with; lookups ignore case. Branch and Commit are set once the
// pod resolved them. Pod is what the pod read through the platform API and
// Job what the scan Job posted; nil until stored. A time that is not set yet
// is the zero time; Deadline is when the running scan is given up on.
type ReadinessScan struct {
	ID, Instance, Project, Branch, Commit, StartedBy, JobName, Error string
	State                                                            ScanState
	QueuedAt, StartedAt, FinishedAt, Deadline                        time.Time
	Pod, Job                                                         json.RawMessage
}

// QueueOutcome says what QueueReadinessScan did.
type QueueOutcome int

const (
	// ScanQueuedNow: a new scan was queued.
	ScanQueuedNow QueueOutcome = iota + 1
	// ScanBusy: the repository has a queued or running scan, which is returned.
	ScanBusy
	// ScanTooSoon: the latest scan was queued less than the gap ago, and is returned.
	ScanTooSoon
)

const readinessColumns = `id::text, instance, project, branch, commit, state, started_by, queued_at, started_at,
	finished_at, deadline, job_name, error, pod_result, job_result`

func scanReadiness(row pgx.Row) (ReadinessScan, error) {
	var (
		sc                          ReadinessScan
		state                       string
		started, finished, deadline *time.Time
		pod, job                    []byte
	)
	err := row.Scan(&sc.ID, &sc.Instance, &sc.Project, &sc.Branch, &sc.Commit, &state, &sc.StartedBy, &sc.QueuedAt,
		&started, &finished, &deadline, &sc.JobName, &sc.Error, &pod, &job)
	if err != nil {
		return ReadinessScan{}, err
	}
	sc.State = ScanState(state)
	sc.StartedAt, sc.FinishedAt, sc.Deadline = deref(started), deref(finished), deref(deadline)
	if pod != nil {
		sc.Pod = pod
	}
	if job != nil {
		sc.Job = job
	}
	return sc, nil
}

// QueueReadinessScan queues a scan of a repository, started by by (an actor,
// StartedBySwitchOn or StartedByUpgrade). A repository (ignoring the case of
// project) has at most one queued or running scan: asking while there is one
// returns it with ScanBusy, whoever asks. Otherwise, when the latest scan was
// queued less than gap before now, that scan is returned with ScanTooSoon
// (whatever started it); a gap of 0 never refuses. Requests at once queue one
// scan: the unique index decides, and the loser gets ScanBusy.
func (s *Store) QueueReadinessScan(ctx context.Context, instance, project, by string, now time.Time, gap time.Duration) (ReadinessScan, QueueOutcome, error) {
	// The loop covers the one race left: the scan that beat this insert
	// finished before it could be read back.
	for range 3 {
		sc, out, err := s.tryQueueReadinessScan(ctx, instance, project, by, now, gap)
		if err != nil {
			return ReadinessScan{}, 0, err
		}
		if out != 0 {
			return sc, out, nil
		}
	}
	return ReadinessScan{}, 0, errors.New("store: readiness scan keeps changing under a queue request")
}

// tryQueueReadinessScan is one attempt of QueueReadinessScan; it returns the
// zero outcome when it lost the insert race to a scan that has already gone.
func (s *Store) tryQueueReadinessScan(ctx context.Context, instance, project, by string, now time.Time, gap time.Duration) (ReadinessScan, QueueOutcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReadinessScan{}, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	active, err := scanReadiness(tx.QueryRow(ctx, `SELECT `+readinessColumns+` FROM readiness_scans
		WHERE instance = $1 AND lower(project) = lower($2) AND state IN ('queued', 'running')`, instance, project))
	if err == nil {
		return active, ScanBusy, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReadinessScan{}, 0, err
	}
	if gap > 0 {
		latest, err := scanReadiness(tx.QueryRow(ctx, `SELECT `+readinessColumns+` FROM readiness_scans
			WHERE instance = $1 AND lower(project) = lower($2) ORDER BY queued_at DESC, id LIMIT 1`, instance, project))
		if err == nil {
			if now.Sub(latest.QueuedAt) < gap {
				return latest, ScanTooSoon, nil
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return ReadinessScan{}, 0, err
		}
	}
	sc, err := scanReadiness(tx.QueryRow(ctx, `INSERT INTO readiness_scans (id, instance, project, state, started_by, queued_at)
		VALUES (gen_random_uuid(), $1, $2, 'queued', $3, $4) RETURNING `+readinessColumns, instance, project, by, now))
	if err != nil {
		if !isUniqueViolation(err) {
			return ReadinessScan{}, 0, err
		}
		// A request at once won: the transaction is dead, so read its scan
		// back on the pool.
		_ = tx.Rollback(ctx)
		active, err := scanReadiness(s.pool.QueryRow(ctx, `SELECT `+readinessColumns+` FROM readiness_scans
			WHERE instance = $1 AND lower(project) = lower($2) AND state IN ('queued', 'running')`, instance, project))
		if errors.Is(err, pgx.ErrNoRows) {
			return ReadinessScan{}, 0, nil
		}
		if err != nil {
			return ReadinessScan{}, 0, err
		}
		return active, ScanBusy, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return ReadinessScan{}, 0, err
	}
	return sc, ScanQueuedNow, nil
}

// ClaimReadinessScan moves the oldest queued scan (by queued_at) to running,
// stamping started_at with now and deadline, and returns it; ok is false when
// none is queued or limit scans already run. The count and the claim are one
// transaction under a lock, so claimers at once never run more than limit.
func (s *Store) ClaimReadinessScan(ctx context.Context, limit int, now, deadline time.Time) (ReadinessScan, bool, error) {
	if limit <= 0 {
		return ReadinessScan{}, false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReadinessScan{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(readinessScanLock)); err != nil {
		return ReadinessScan{}, false, err
	}
	var running int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM readiness_scans WHERE state = 'running'`).Scan(&running); err != nil {
		return ReadinessScan{}, false, err
	}
	if running >= limit {
		return ReadinessScan{}, false, nil
	}
	sc, err := scanReadiness(tx.QueryRow(ctx, `UPDATE readiness_scans SET state = 'running', started_at = $1, deadline = $2
		WHERE id = (SELECT id FROM readiness_scans WHERE state = 'queued' ORDER BY queued_at, id LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING `+readinessColumns, now, deadline))
	if errors.Is(err, pgx.ErrNoRows) {
		return ReadinessScan{}, false, nil
	}
	if err != nil {
		return ReadinessScan{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReadinessScan{}, false, err
	}
	return sc, true, nil
}

// SetReadinessPod records the branch and commit the pod resolved and what it
// read through the platform API, on a running scan; any other is
// ErrReadinessScanNotFound.
func (s *Store) SetReadinessPod(ctx context.Context, id, branch, commit string, pod json.RawMessage) error {
	if !fetchID.MatchString(id) {
		return ErrReadinessScanNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE readiness_scans SET branch = $2, commit = $3, pod_result = $4
		WHERE id = $1::uuid AND state = 'running'`, id, branch, commit, []byte(pod))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrReadinessScanNotFound
	}
	return nil
}

// StartReadinessJob records the name of a running scan's Job, the hash of its
// one-time result token and the new deadline, before the Job is created; any
// other scan is ErrReadinessScanNotFound.
func (s *Store) StartReadinessJob(ctx context.Context, id, jobName string, tokenHash []byte, deadline time.Time) error {
	if !fetchID.MatchString(id) {
		return ErrReadinessScanNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE readiness_scans SET job_name = $2, token_hash = $3, deadline = $4
		WHERE id = $1::uuid AND state = 'running'`, id, jobName, tokenHash, deadline)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrReadinessScanNotFound
	}
	return nil
}

// ReadinessScan returns a scan; a missing one is ErrReadinessScanNotFound.
func (s *Store) ReadinessScan(ctx context.Context, id string) (ReadinessScan, error) {
	if !fetchID.MatchString(id) {
		return ReadinessScan{}, ErrReadinessScanNotFound
	}
	sc, err := scanReadiness(s.pool.QueryRow(ctx, `SELECT `+readinessColumns+` FROM readiness_scans WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ReadinessScan{}, ErrReadinessScanNotFound
	}
	return sc, err
}

// ReadinessTokenIs reports whether hash is the token of the scan, which is
// still running. It spends nothing: it is the cheap check before the work
// FinishReadinessScan's atomic spend then guards.
func (s *Store) ReadinessTokenIs(ctx context.Context, id string, hash []byte) (bool, error) {
	if !fetchID.MatchString(id) || hash == nil {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM readiness_scans
		WHERE id = $1::uuid AND state = 'running' AND token_hash = $2)`, id, hash).Scan(&ok)
	return ok, err
}

// FinishReadinessScan ends a running scan (done when errText is "", else
// failed), spending the token when hash is set (nil: a pod-side end), and
// deletes the repository's older finished scans in the same transaction, but
// for a failed end the newest done scan, which stays beside it. job
// is the Job's result (nil: none). It reports whether it did: a scan that is
// not running, does not exist, or whose token is not hash is false and left
// as it is.
func (s *Store) FinishReadinessScan(ctx context.Context, id string, hash []byte, job json.RawMessage, errText string, now time.Time) (bool, error) {
	if !fetchID.MatchString(id) {
		return false, nil
	}
	state := ScanDone
	if errText != "" {
		state = ScanFailed
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var instance, project string
	err = tx.QueryRow(ctx, `UPDATE readiness_scans SET state = $2, finished_at = $3, error = $4, job_result = $5, token_hash = NULL
		WHERE id = $1::uuid AND state = 'running' AND ($6::bytea IS NULL OR token_hash = $6)
		RETURNING instance, project`, id, string(state), now, errText, []byte(job), hash).Scan(&instance, &project)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A failed scan keeps the newest done one: rechecks judge the skills
	// against its file result. A done scan replaces them all.
	keepDone := state == ScanFailed
	if _, err := tx.Exec(ctx, `DELETE FROM readiness_scans
		WHERE instance = $1 AND lower(project) = lower($2) AND state IN ('done', 'failed') AND id <> $3::uuid
		AND NOT ($4 AND id IS NOT DISTINCT FROM (SELECT d.id FROM readiness_scans d
			WHERE d.instance = $1 AND lower(d.project) = lower($2) AND d.state = 'done'
			ORDER BY d.queued_at DESC, d.id LIMIT 1))`,
		instance, project, id, keepDone); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// RunningReadinessScans returns the running scans, oldest first.
func (s *Store) RunningReadinessScans(ctx context.Context) ([]ReadinessScan, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+readinessColumns+` FROM readiness_scans
		WHERE state = 'running' ORDER BY queued_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReadinessScan
	for rows.Next() {
		sc, err := scanReadiness(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ReadinessScans returns a repository's latest finished scan (done or failed)
// and its queued or running one, each nil when there is none. The project is
// matched ignoring case.
func (s *Store) ReadinessScans(ctx context.Context, instance, project string) (latest, active *ReadinessScan, err error) {
	rows, err := s.pool.Query(ctx, `SELECT `+readinessColumns+` FROM readiness_scans
		WHERE instance = $1 AND lower(project) = lower($2) ORDER BY queued_at DESC, id`, instance, project)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		sc, err := scanReadiness(rows)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case sc.State == ScanQueued || sc.State == ScanRunning:
			if active == nil {
				active = &sc
			}
		case latest == nil:
			latest = &sc
		}
	}
	return latest, active, rows.Err()
}

// DoneReadinessScan returns a repository's newest done scan, nil when there is
// none. It may be older than the latest finished scan ReadinessScans returns:
// a failed scan keeps the done one before it. The project is matched ignoring
// case.
func (s *Store) DoneReadinessScan(ctx context.Context, instance, project string) (*ReadinessScan, error) {
	sc, err := scanReadiness(s.pool.QueryRow(ctx, `SELECT `+readinessColumns+` FROM readiness_scans
		WHERE instance = $1 AND lower(project) = lower($2) AND state = 'done' ORDER BY queued_at DESC, id LIMIT 1`,
		instance, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

// DeleteReadinessScans removes every scan of a repository, active ones too;
// deleting none is not an error. The project is matched ignoring case.
func (s *Store) DeleteReadinessScans(ctx context.Context, instance, project string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM readiness_scans WHERE instance = $1 AND lower(project) = lower($2)`,
		instance, project)
	return err
}

// ProjectRef names a repository by instance and path.
type ProjectRef struct {
	Instance, Project string
}

// UnscannedProjects returns the switched-on repositories that have no
// readiness_scans row at all (a queued or running scan counts), in the order
// a startup sweep should queue them: the config-file repositories first, then
// the dashboard repositories by when they were switched on
// (dashboard_repos.created_at); ties go by instance, then project id.
// A repository is switched on when it has a project row, which reconcile
// creates for a dashboard or config-file repository and deletes when it is
// switched off, so a repository that is switched off but whose row reconcile
// has not removed yet is still listed (a config-file one, as it has no
// dashboard row).
func (s *Store) UnscannedProjects(ctx context.Context) ([]ProjectRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.instance, p.path FROM projects p
		LEFT JOIN dashboard_repos d ON d.instance = p.instance AND lower(d.project) = lower(p.path)
		WHERE NOT EXISTS (SELECT 1 FROM readiness_scans r
			WHERE r.instance = p.instance AND lower(r.project) = lower(p.path))
		ORDER BY d.created_at ASC NULLS FIRST, p.instance, p.project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectRef
	for rows.Next() {
		var r ProjectRef
		if err := rows.Scan(&r.Instance, &r.Project); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
