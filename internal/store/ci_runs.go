package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ciJobsParam is the jsonb value of a repository's ci_jobs; nil is NULL. A
// nil Jobs is stored as [], never null.
func ciJobsParam(c *CIJobs) (any, error) {
	if c == nil {
		return nil, nil
	}
	v := *c
	if v.Jobs == nil {
		v.Jobs = []CIJob{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("store: ci jobs: %w", err)
	}
	return b, nil
}

// readCIJobs decodes a stored ci_jobs; NULL is nil.
func readCIJobs(b []byte) (*CIJobs, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var c CIJobs
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("store: ci jobs: %w", err)
	}
	if len(c.Jobs) == 0 {
		c.Jobs = nil
	}
	return &c, nil
}

// ciResultsParam is the jsonb value of a run's snapshot; an empty list is
// NULL.
func ciResultsParam(js []CIJobResult) (any, error) {
	if len(js) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(js)
	if err != nil {
		return nil, fmt.Errorf("store: ci results: %w", err)
	}
	return b, nil
}

// readCIResults decodes a stored snapshot; NULL and an empty list are nil.
func readCIResults(b []byte) ([]CIJobResult, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var js []CIJobResult
	if err := json.Unmarshal(b, &js); err != nil {
		return nil, fmt.Errorf("store: ci results: %w", err)
	}
	if len(js) == 0 {
		return nil, nil
	}
	return js, nil
}

// HoldForCI moves a queued, or claimed and not yet dispatched, run to Waiting
// with the reason WaitCIPending, to wait for its CI jobs until deadline. A run
// CI was already looked at for (CIChecked) is not held. ok is false when the
// run was in another state or unknown, or is an API review (no merge request,
// no CI); a malformed id is an error.
func (s *Store) HoldForCI(ctx context.Context, id string, deadline time.Time) (ok bool, err error) {
	if !validUUID(id) {
		return false, fmt.Errorf("store: run %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'waiting', skip_reason = $2, ci_deadline = $3
		WHERE id = $1::uuid AND state IN ('queued', 'running') AND dispatched_at IS NULL AND NOT ci_checked
		  AND kind = 'mr'`,
		id, WaitCIPending, deadline)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseCI queues a run waiting for CI, marked as one CI was looked at for,
// so it is not held again. ok is false when the run is in another state, has
// another reason to wait or is unknown (a superseded run is never revived); a
// malformed id is an error.
func (s *Store) ReleaseCI(ctx context.Context, id string) (ok bool, err error) {
	if !validUUID(id) {
		return false, fmt.Errorf("store: run %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'queued', skip_reason = '', ci_checked = true
		WHERE id = $1::uuid AND state = 'waiting' AND skip_reason = $2`, id, WaitCIPending)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkCIChecked marks a queued or running run, not dispatched yet, as one CI
// was looked at for (the repository has no CI jobs, or none to wait for), so
// the dispatcher never holds it for CI. Any other run is left alone.
func (s *Store) MarkCIChecked(ctx context.Context, id string) error {
	if !validUUID(id) {
		return fmt.Errorf("store: run %q not found", id)
	}
	_, err := s.pool.Exec(ctx, `UPDATE runs SET ci_checked = true WHERE id = $1::uuid AND dispatched_at IS NULL
		AND state IN ('queued', 'running')`, id)
	return err
}

// SetRunCI records the snapshot of the CI jobs a run was decided with; nil or
// an empty list clears it. A superseded run, or an unknown one, is left as it
// is and gives ErrRunGone; a malformed id is another error.
func (s *Store) SetRunCI(ctx context.Context, id string, jobs []CIJobResult) error {
	v, err := ciResultsParam(jobs)
	if err != nil {
		return err
	}
	if !validUUID(id) {
		return fmt.Errorf("store: run %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET ci_jobs = $2::jsonb
		WHERE id = $1::uuid AND state IN ('queued', 'running', 'waiting', 'reported', 'published', 'publish_failed')`, id, v)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrRunGone
	}
	return nil
}

// CIPendingRuns returns the runs waiting for CI, the nearest deadline first.
func (s *Store) CIPendingRuns(ctx context.Context) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE state = 'waiting' AND skip_reason = $1 ORDER BY ci_deadline, id`, WaitCIPending)
}

// CIRunsForHead returns the runs a CI event on head of project (its path,
// matched ignoring case) concerns: the runs waiting for CI, and the newest
// published run of each merge request when it is of that head.
func (s *Store) CIRunsForHead(ctx context.Context, instance, project, head string) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE instance = $1 AND lower(project_path) = lower($2) AND head_sha = $3 AND kind = 'mr'
		  AND ((state = 'waiting' AND skip_reason = $4)
		    OR (state = 'published' AND id = (SELECT n.id FROM runs n
				WHERE n.instance = runs.instance AND n.project_id = runs.project_id AND n.mr_iid = runs.mr_iid
				  AND n.kind = 'mr' AND n.state = 'published'
				ORDER BY n.created_at DESC, n.id DESC LIMIT 1)))
		ORDER BY created_at, id`, instance, project, head, WaitCIPending)
}

// CIRecheckRuns returns the newest published run of each merge request that
// was reported since, when its snapshot has a job whose result is not
// "passed": a lost CI event then only delays the gate.
func (s *Store) CIRecheckRuns(ctx context.Context, since time.Time) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE kind = 'mr' AND state = 'published' AND reported_at >= $1
		  AND id = (SELECT n.id FROM runs n
			WHERE n.instance = runs.instance AND n.project_id = runs.project_id AND n.mr_iid = runs.mr_iid
			  AND n.kind = 'mr' AND n.state = 'published'
			ORDER BY n.created_at DESC, n.id DESC LIMIT 1)
		  AND jsonb_typeof(ci_jobs) = 'array'
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(ci_jobs) = 'array' THEN ci_jobs ELSE '[]'::jsonb END) j WHERE j->>'result' IS DISTINCT FROM 'passed')
		ORDER BY created_at, id`, since)
}

// SetProjectCIAccess records what the review App could do on a project when
// it was last reconciled: read its Actions (workflow runs, jobs, artifacts)
// and receive check_run events. A project that is not stored is left alone.
func (s *Store) SetProjectCIAccess(ctx context.Context, instance string, projectID int64, actionsReadable, checkRunEvents bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE projects SET actions_readable = $3, check_run_events = $4
		WHERE instance = $1 AND project_id = $2`, instance, projectID, actionsReadable, checkRunEvents)
	return err
}
