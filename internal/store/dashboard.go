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
