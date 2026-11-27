package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Where a security scan stands (SecurityScan.State). Complete, partial and
// failed are the ends.
const (
	SecurityQueued   = "queued"
	SecurityRunning  = "running"
	SecurityComplete = "complete"
	SecurityPartial  = "partial"
	SecurityFailed   = "failed"
)

// securityCancelled is the reason of a scan cancelled before it ran.
const securityCancelled = "cancelled"

var (
	// ErrSecurityScanBusy is returned by QueueSecurityScan, with the scan,
	// when the repository has a queued or running scan.
	ErrSecurityScanBusy = errors.New("store: the repository has a security scan queued or running")
	// ErrSecurityScanNotFound is returned for a scan that does not exist, or
	// is not in the state the call needs (running, for StartSecurityJob).
	ErrSecurityScanNotFound = errors.New("store: security scan not found")
)

// securityScanLock serialises ClaimSecurityScan, so two claimers cannot both
// count a free place and take it. It is a transaction-level advisory lock,
// so it is gone with the transaction.
const securityScanLock = 0x7365_6375 // "secu"

// SecurityScan is one security scan of a repository. Project keeps the
// spelling it was queued with; lookups ignore case. Number counts the
// repository's scans from 1. RunID is the scan's hidden run (KindScan), which
// carries its usage and the entry it ran on. A time that is not set yet is
// nil; Deadline is when the running scan is given up on. MaxSteps and
// SubagentMaxSteps are nil for the skill's own limits; TokenBudget is nil
// when no quota or limit applies. HasSARIF is whether the scan still keeps
// its SARIF.
type SecurityScan struct {
	ID, RunID                                          string
	Number                                             int64
	Instance, Project                                  string
	ProjectID                                          int64
	Branch, Commit, State, Reason, StartedBy           string
	PastQuota                                          bool
	QueuedAt                                           time.Time
	StartedAt, FinishedAt, Deadline, CancelRequestedAt *time.Time
	JobName, Provider, Model, EntryName                string
	MaxSteps, SubagentMaxSteps                         *int
	TimeoutSeconds                                     int
	TokenBudget                                        *int64
	SkillName, SkillCommit                             string
	SkillUpdated                                       bool
	CoverageDone, CoveragePlanned                      *int
	UsageSource                                        string
	NewCount, FixedCount                               int
	HasSARIF                                           bool
}

// SecurityScanRequest is what a person's Start asks for: the repository, the
// default branch and the commit read from the platform at Start, who asked,
// whether they started it past a used-up quota, and the settings it runs
// with (nil step limits: the skill's own).
type SecurityScanRequest struct {
	Instance, Project          string
	ProjectID                  int64
	Branch, Commit, StartedBy  string
	PastQuota                  bool
	MaxSteps, SubagentMaxSteps *int
	TimeoutSeconds             int
}

// SecurityJobStart is what StartSecurityJob records before the Job is
// created: the Job's name, the hash of its result token, its deadline, and
// the chain entry it runs on with that entry's provider and model. A nil
// TokenBudget is no budget.
type SecurityJobStart struct {
	JobName         string
	TokenHash       []byte
	Deadline        time.Time
	Entry           RunEntry
	Provider, Model string
	TokenBudget     *int64
}

const securityColumns = `id::text, run_id::text, number, instance, project, project_id, branch, commit, state, reason,
	started_by, past_quota, queued_at, started_at, finished_at, deadline, cancel_requested_at, job_name, provider,
	model, entry_name, max_steps, subagent_max_steps, timeout_seconds, token_budget, skill_name, skill_commit,
	skill_updated, coverage_done, coverage_planned, usage_source, new_count, fixed_count, sarif IS NOT NULL`

func scanSecurity(row pgx.Row) (SecurityScan, error) {
	var sc SecurityScan
	err := row.Scan(&sc.ID, &sc.RunID, &sc.Number, &sc.Instance, &sc.Project, &sc.ProjectID, &sc.Branch, &sc.Commit,
		&sc.State, &sc.Reason, &sc.StartedBy, &sc.PastQuota, &sc.QueuedAt, &sc.StartedAt, &sc.FinishedAt, &sc.Deadline,
		&sc.CancelRequestedAt, &sc.JobName, &sc.Provider, &sc.Model, &sc.EntryName, &sc.MaxSteps, &sc.SubagentMaxSteps,
		&sc.TimeoutSeconds, &sc.TokenBudget, &sc.SkillName, &sc.SkillCommit, &sc.SkillUpdated, &sc.CoverageDone,
		&sc.CoveragePlanned, &sc.UsageSource, &sc.NewCount, &sc.FixedCount, &sc.HasSARIF)
	return sc, err
}

func (s *Store) querySecurityScans(ctx context.Context, sql string, args ...any) ([]SecurityScan, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (SecurityScan, error) { return scanSecurity(row) })
}

// activeSecurityWhere matches a repository's queued or running scan, the
// project ignoring case.
const activeSecurityWhere = `WHERE instance = $1 AND lower(project) = lower($2) AND state IN ('queued', 'running')`

// QueueSecurityScan queues a scan of a repository and its hidden run, in one
// transaction. The run is of kind KindScan with no repository (instance ”,
// project 0, a number of its own as mr_iid), stored superseded and with its
// usage recorded, so the review loop never claims it, no review page lists
// it and the usage fill never reads it. The scan's Number is the
// repository's last plus 1. A repository (ignoring the case of project) has
// at most one queued or running scan: asking while there is one returns it
// with ErrSecurityScanBusy. Requests at once queue one scan: the unique index
// decides, and the loser gets ErrSecurityScanBusy.
func (s *Store) QueueSecurityScan(ctx context.Context, q SecurityScanRequest, now time.Time) (SecurityScan, error) {
	// The loop covers the one race left: the scan that beat this insert
	// finished before it could be read back.
	for range 3 {
		sc, done, err := s.tryQueueSecurityScan(ctx, q, now)
		if done || err != nil {
			return sc, err
		}
	}
	return SecurityScan{}, errors.New("store: security scan keeps changing under a queue request")
}

// tryQueueSecurityScan is one attempt of QueueSecurityScan; done is false
// when it lost the insert race to a scan that has already gone.
func (s *Store) tryQueueSecurityScan(ctx context.Context, q SecurityScanRequest, now time.Time) (sc SecurityScan, done bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SecurityScan{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	active, err := scanSecurity(tx.QueryRow(ctx, `SELECT `+securityColumns+` FROM security_scans `+activeSecurityWhere,
		q.Instance, q.Project))
	if err == nil {
		return active, true, ErrSecurityScanBusy
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SecurityScan{}, false, err
	}
	var runID string
	if err := tx.QueryRow(ctx, `INSERT INTO runs (instance, project_id, mr_iid, head_sha, kind, state, usage_recorded,
			quota_checked, created_at)
		VALUES ('', 0, nextval('security_scan_numbers'), '', 'scan', 'superseded', true, true, $1)
		RETURNING id::text`, now).Scan(&runID); err != nil {
		return SecurityScan{}, false, err
	}
	sc, err = scanSecurity(tx.QueryRow(ctx, `INSERT INTO security_scans (id, run_id, number, instance, project,
			project_id, branch, commit, state, started_by, past_quota, queued_at, max_steps, subagent_max_steps,
			timeout_seconds)
		VALUES (gen_random_uuid(), $1::uuid,
			(SELECT COALESCE(max(number), 0) + 1 FROM security_scans WHERE instance = $2 AND lower(project) = lower($3)),
			$2, $3, $4, $5, $6, 'queued', $7, $8, $9, $10, $11, $12)
		RETURNING `+securityColumns,
		runID, q.Instance, q.Project, q.ProjectID, q.Branch, q.Commit, q.StartedBy, q.PastQuota, now,
		q.MaxSteps, q.SubagentMaxSteps, q.TimeoutSeconds))
	if err != nil {
		if !isUniqueViolation(err) {
			return SecurityScan{}, false, err
		}
		// A request at once won: the transaction is dead, so read its scan
		// back on the pool.
		_ = tx.Rollback(ctx)
		active, err := scanSecurity(s.pool.QueryRow(ctx, `SELECT `+securityColumns+` FROM security_scans `+activeSecurityWhere,
			q.Instance, q.Project))
		if errors.Is(err, pgx.ErrNoRows) {
			return SecurityScan{}, false, nil
		}
		if err != nil {
			return SecurityScan{}, false, err
		}
		return active, true, ErrSecurityScanBusy
	}
	if err := tx.Commit(ctx); err != nil {
		return SecurityScan{}, false, err
	}
	return sc, true, nil
}

// ClaimSecurityScan moves the oldest queued scan (by queued_at) to running,
// stamping started_at with now and deadline, and returns it; ok is false when
// none is queued or limit scans already run. The count and the claim are one
// transaction under a lock, so claimers at once never run more than limit.
func (s *Store) ClaimSecurityScan(ctx context.Context, limit int, now, deadline time.Time) (SecurityScan, bool, error) {
	if limit <= 0 {
		return SecurityScan{}, false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SecurityScan{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(securityScanLock)); err != nil {
		return SecurityScan{}, false, err
	}
	var running int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM security_scans WHERE state = 'running'`).Scan(&running); err != nil {
		return SecurityScan{}, false, err
	}
	if running >= limit {
		return SecurityScan{}, false, nil
	}
	sc, err := scanSecurity(tx.QueryRow(ctx, `UPDATE security_scans SET state = 'running', started_at = $1, deadline = $2
		WHERE id = (SELECT id FROM security_scans WHERE state = 'queued' ORDER BY queued_at, id LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING `+securityColumns, now, deadline))
	if errors.Is(err, pgx.ErrNoRows) {
		return SecurityScan{}, false, nil
	}
	if err != nil {
		return SecurityScan{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SecurityScan{}, false, err
	}
	return sc, true, nil
}

// StartSecurityJob records, before the Job is created, the Job of a running
// scan (its name, the hash of its result token, the new deadline, the
// entry's name, provider and model, and the token budget) and, in the same
// transaction, the entry on the scan's hidden run, as SetRunEntry and
// SetRunModel write them on a review's run. Any other scan is
// ErrSecurityScanNotFound.
func (s *Store) StartSecurityJob(ctx context.Context, id string, j SecurityJobStart) error {
	if !fetchID.MatchString(id) {
		return ErrSecurityScanNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	provider, model := cutText(j.Provider), cutText(j.Model)
	var runID string
	err = tx.QueryRow(ctx, `UPDATE security_scans SET job_name = $2, token_hash = $3, deadline = $4, provider = $5,
			model = $6, entry_name = $7, token_budget = $8
		WHERE id = $1::uuid AND state = 'running' RETURNING run_id::text`,
		id, j.JobName, j.TokenHash, j.Deadline, provider, model, cutText(j.Entry.Name), j.TokenBudget).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSecurityScanNotFound
	}
	if err != nil {
		return err
	}
	e := j.Entry
	tag, err := tx.Exec(ctx, `UPDATE runs SET provider = $2, model = $3, model_source = $4, model_entry_id = $5,
			model_entry_name = $6, model_team_id = NULLIF($7::bigint, 0), chain_position = $8, chain_length = $9,
			fallback_class = $10
		WHERE id = $1::uuid AND kind = 'scan'`,
		runID, provider, model, e.Source, e.ID, cutText(e.Name), e.TeamID, e.Position, e.Length, e.FallbackClass)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("store: the security scan's run is gone")
	}
	return tx.Commit(ctx)
}

// SecurityScan returns a scan; a missing one is ErrSecurityScanNotFound.
func (s *Store) SecurityScan(ctx context.Context, id string) (SecurityScan, error) {
	if !fetchID.MatchString(id) {
		return SecurityScan{}, ErrSecurityScanNotFound
	}
	sc, err := scanSecurity(s.pool.QueryRow(ctx, `SELECT `+securityColumns+` FROM security_scans WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return SecurityScan{}, ErrSecurityScanNotFound
	}
	return sc, err
}

// SecurityTokenIs reports whether hash is the token of the scan, which is
// still running. It spends nothing: it is the cheap check before the work
// the finish's atomic spend then guards.
func (s *Store) SecurityTokenIs(ctx context.Context, id string, hash []byte) (bool, error) {
	if !fetchID.MatchString(id) || hash == nil {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM security_scans
		WHERE id = $1::uuid AND state = 'running' AND token_hash = $2)`, id, hash).Scan(&ok)
	return ok, err
}

// RunningSecurityScans returns the running scans, oldest first.
func (s *Store) RunningSecurityScans(ctx context.Context) ([]SecurityScan, error) {
	return s.querySecurityScans(ctx, `SELECT `+securityColumns+` FROM security_scans
		WHERE state = 'running' ORDER BY queued_at, id`)
}

// ActiveSecurityScan returns a repository's queued or running scan, nil when
// there is none. The project is matched ignoring case.
func (s *Store) ActiveSecurityScan(ctx context.Context, instance, project string) (*SecurityScan, error) {
	sc, err := scanSecurity(s.pool.QueryRow(ctx, `SELECT `+securityColumns+` FROM security_scans `+activeSecurityWhere,
		instance, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

// SecurityScans returns up to limit scans of a repository, newest first. The
// project is matched ignoring case.
func (s *Store) SecurityScans(ctx context.Context, instance, project string, limit int) ([]SecurityScan, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.querySecurityScans(ctx, `SELECT `+securityColumns+` FROM security_scans
		WHERE instance = $1 AND lower(project) = lower($2) ORDER BY number DESC LIMIT $3`, instance, project, limit)
}

// RequestSecurityCancel asks a scan to stop at now. A running scan records
// when the cancel was first asked for and keeps running until its Job posts
// or the sweep ends it. A queued scan, which has no Job, ends at once:
// partial, reason cancelled. ok is whether the scan was queued or running;
// a finished scan is returned as it is with ok false. A missing one is
// ErrSecurityScanNotFound.
func (s *Store) RequestSecurityCancel(ctx context.Context, id string, now time.Time) (SecurityScan, bool, error) {
	if !fetchID.MatchString(id) {
		return SecurityScan{}, false, ErrSecurityScanNotFound
	}
	sc, err := scanSecurity(s.pool.QueryRow(ctx, `UPDATE security_scans SET
			cancel_requested_at = COALESCE(cancel_requested_at, $2),
			state = CASE WHEN state = 'queued' THEN 'partial' ELSE state END,
			reason = CASE WHEN state = 'queued' THEN $3 ELSE reason END,
			finished_at = CASE WHEN state = 'queued' THEN $2 ELSE finished_at END
		WHERE id = $1::uuid AND state IN ('queued', 'running')
		RETURNING `+securityColumns, id, now, securityCancelled))
	if err == nil {
		return sc, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SecurityScan{}, false, err
	}
	sc, err = s.SecurityScan(ctx, id)
	return sc, false, err
}

// SecurityScanSettings are a repository's scan settings; a nil value is the
// default (the skill's step limits, the default timeout). FindingsInReviews
// is whether its reviews are given the open findings of the files a change
// touches (off by default).
type SecurityScanSettings struct {
	MaxSteps, SubagentMaxSteps, TimeoutSeconds *int
	FindingsInReviews                          bool
}

// SecurityScanSettingsOf returns a repository's scan settings, the zero
// value when it has none. The project is matched ignoring case.
func (s *Store) SecurityScanSettingsOf(ctx context.Context, instance, project string) (SecurityScanSettings, error) {
	var v SecurityScanSettings
	err := s.pool.QueryRow(ctx, `SELECT max_steps, subagent_max_steps, timeout_seconds, COALESCE(findings_in_reviews, false)
		FROM security_scan_settings WHERE instance = $1 AND lower(project) = lower($2)`, instance, project).
		Scan(&v.MaxSteps, &v.SubagentMaxSteps, &v.TimeoutSeconds, &v.FindingsInReviews)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecurityScanSettings{}, nil
	}
	return v, err
}

// PutSecurityScanSettings replaces every scan setting of a repository with
// v. The table refuses a value out of its range; the caller checks them
// first, to say which. The project is matched ignoring case and keeps the
// spelling it was first stored with.
func (s *Store) PutSecurityScanSettings(ctx context.Context, instance, project string, v SecurityScanSettings, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO security_scan_settings (instance, project, max_steps, subagent_max_steps,
			timeout_seconds, findings_in_reviews, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (instance, (lower(project))) DO UPDATE SET max_steps = EXCLUDED.max_steps,
			subagent_max_steps = EXCLUDED.subagent_max_steps, timeout_seconds = EXCLUDED.timeout_seconds,
			findings_in_reviews = EXCLUDED.findings_in_reviews, updated_at = EXCLUDED.updated_at`,
		instance, project, v.MaxSteps, v.SubagentMaxSteps, v.TimeoutSeconds, v.FindingsInReviews, now)
	return err
}

// ResetScanChain drops a repository's own scan chain; dropping none is not
// an error. The project is matched ignoring case.
func (s *Store) ResetScanChain(ctx context.Context, instance, project string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM `+ChainScan.chains+` WHERE scope = 'repo' AND instance = $1
		AND lower(project) = lower($2)`, instance, project)
	return err
}
