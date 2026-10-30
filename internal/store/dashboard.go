package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// DashboardRepo is a repository switched on in the dashboard, a row of
// dashboard_repos. A nil setting means "use the default"; Skills is nil for
// the same (an empty list is stored as nil). Project keeps the spelling it
// was stored with; lookups ignore case.
type DashboardRepo struct {
	Instance, Project string
	Approve           *bool
	ReviewDrafts      *bool
	// FailureStatus is "failed" or "neutral".
	FailureStatus     *string
	Skills            []string
	JobTimeoutSeconds *int
	// JobMemory is a Kubernetes quantity such as "6Gi".
	JobMemory            *string
	CreatedBy            string
	CreatedAt, UpdatedAt time.Time
}

const dashboardRepoColumns = `instance, project, approve, review_drafts, failure_status, skills,
	job_timeout_seconds, job_memory, created_by, created_at, updated_at`

func scanDashboardRepo(row pgx.Row) (DashboardRepo, error) {
	var r DashboardRepo
	err := row.Scan(&r.Instance, &r.Project, &r.Approve, &r.ReviewDrafts, &r.FailureStatus, &r.Skills,
		&r.JobTimeoutSeconds, &r.JobMemory, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if len(r.Skills) == 0 {
		r.Skills = nil
	}
	return r, err
}

// DashboardRepos returns every dashboard repository, ordered by instance and
// project (ignoring case).
func (s *Store) DashboardRepos(ctx context.Context) ([]DashboardRepo, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+dashboardRepoColumns+` FROM dashboard_repos
		ORDER BY instance, lower(project)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DashboardRepo
	for rows.Next() {
		r, err := scanDashboardRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DashboardRepo returns the dashboard repository project on instance, the
// project matched ignoring case; ok is false when there is none.
func (s *Store) DashboardRepo(ctx context.Context, instance, project string) (DashboardRepo, bool, error) {
	r, err := scanDashboardRepo(s.pool.QueryRow(ctx, `SELECT `+dashboardRepoColumns+` FROM dashboard_repos
		WHERE instance = $1 AND lower(project) = lower($2)`, instance, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return DashboardRepo{}, false, nil
	}
	if err != nil {
		return DashboardRepo{}, false, err
	}
	return r, true, nil
}

// UpsertDashboardRepo stores r. An existing row of the same repository
// (ignoring case) gets r's spelling and every setting of r, NULLs included;
// its CreatedBy and CreatedAt stay. UpdatedAt is the database's now.
func (s *Store) UpsertDashboardRepo(ctx context.Context, r DashboardRepo) error {
	skills := r.Skills
	if len(skills) == 0 {
		skills = nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO dashboard_repos (instance, project, approve, review_drafts,
			failure_status, skills, job_timeout_seconds, job_memory, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (instance, lower(project)) DO UPDATE SET
			project = EXCLUDED.project, approve = EXCLUDED.approve, review_drafts = EXCLUDED.review_drafts,
			failure_status = EXCLUDED.failure_status, skills = EXCLUDED.skills,
			job_timeout_seconds = EXCLUDED.job_timeout_seconds, job_memory = EXCLUDED.job_memory,
			updated_at = now()`,
		r.Instance, r.Project, r.Approve, r.ReviewDrafts, r.FailureStatus, skills,
		r.JobTimeoutSeconds, r.JobMemory, r.CreatedBy)
	return err
}

// DeleteDashboardRepo removes the dashboard repository project on instance,
// matched ignoring case. Deleting one that isn't there is not an error.
func (s *Store) DeleteDashboardRepo(ctx context.Context, instance, project string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM dashboard_repos WHERE instance = $1 AND lower(project) = lower($2)`,
		instance, project)
	return err
}

// PutRunLog stores the redacted log of a run, replacing an earlier one. An
// empty text is stored too: it records that the log was taken.
func (s *Store) PutRunLog(ctx context.Context, runID, text string) error {
	if !validUUID(runID) {
		return errors.New("store: malformed run id")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO run_logs (run_id, text) VALUES ($1::uuid, $2)
		ON CONFLICT (run_id) DO UPDATE SET text = EXCLUDED.text, created_at = now()`, runID, text)
	return err
}

// RunLog returns a run's stored log. A run without one, or a malformed id,
// is reported as not found.
func (s *Store) RunLog(ctx context.Context, runID string) (string, bool, error) {
	if !validUUID(runID) {
		return "", false, nil
	}
	var text string
	err := s.pool.QueryRow(ctx, `SELECT text FROM run_logs WHERE run_id = $1::uuid`, runID).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

// PruneRunLogs deletes the logs stored before the given time and returns how
// many there were.
func (s *Store) PruneRunLogs(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM run_logs WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// RunsToLog returns the runs that have a result or a failure reported since
// the given time and a Job, but no stored log, oldest first.
func (s *Store) RunsToLog(ctx context.Context, since time.Time) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE state IN ('reported', 'published', 'publish_failed') AND job_name <> ''
		  AND reported_at >= $1
		  AND NOT EXISTS (SELECT 1 FROM run_logs WHERE run_logs.run_id = runs.id)
		ORDER BY reported_at, id`, since)
}
