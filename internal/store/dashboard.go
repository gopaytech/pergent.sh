package store

import (
	"context"
	"errors"
	"strings"
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

// InsertDashboardRepo stores r unless the repository (ignoring case) already
// has a row, which is left as it is; created tells which.
func (s *Store) InsertDashboardRepo(ctx context.Context, r DashboardRepo) (created bool, err error) {
	skills := r.Skills
	if len(skills) == 0 {
		skills = nil
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO dashboard_repos (instance, project, approve, review_drafts,
			failure_status, skills, job_timeout_seconds, job_memory, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (instance, lower(project)) DO NOTHING`,
		r.Instance, r.Project, r.Approve, r.ReviewDrafts, r.FailureStatus, skills,
		r.JobTimeoutSeconds, r.JobMemory, r.CreatedBy)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateDashboardRepo changes the settings of the dashboard repository
// project on instance (matched ignoring case): apply gets the stored row,
// locked until the change is stored, and its settings (approve through
// job_memory) are written back; the rest of the row stays. found is false,
// and nothing is created, when there is no such row.
func (s *Store) UpdateDashboardRepo(ctx context.Context, instance, project string, apply func(*DashboardRepo)) (found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	r, err := scanDashboardRepo(tx.QueryRow(ctx, `SELECT `+dashboardRepoColumns+` FROM dashboard_repos
		WHERE instance = $1 AND lower(project) = lower($2) FOR UPDATE`, instance, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	apply(&r)
	skills := r.Skills
	if len(skills) == 0 {
		skills = nil
	}
	if _, err := tx.Exec(ctx, `UPDATE dashboard_repos SET approve = $3, review_drafts = $4, failure_status = $5,
			skills = $6, job_timeout_seconds = $7, job_memory = $8, updated_at = now()
		WHERE instance = $1 AND lower(project) = lower($2)`,
		instance, project, r.Approve, r.ReviewDrafts, r.FailureStatus, skills, r.JobTimeoutSeconds, r.JobMemory); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// DeleteDashboardRepo removes the dashboard repository project on instance,
// matched ignoring case. Deleting one that isn't there is not an error.
func (s *Store) DeleteDashboardRepo(ctx context.Context, instance, project string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM dashboard_repos WHERE instance = $1 AND lower(project) = lower($2)`,
		instance, project)
	return err
}

// PutRunLog stores the redacted log of a run, replacing an earlier one,
// made fit for a text column with CleanText. An empty text is stored too:
// it records that the log was taken.
func (s *Store) PutRunLog(ctx context.Context, runID, text string) error {
	if !validUUID(runID) {
		return errors.New("store: malformed run id")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO run_logs (run_id, text) VALUES ($1::uuid, $2)
		ON CONFLICT (run_id) DO UPDATE SET text = EXCLUDED.text, created_at = now()`, runID, CleanText(text))
	return err
}

// CleanText makes text something Postgres stores as text: each invalid
// UTF-8 sequence becomes U+FFFD and NUL bytes are dropped.
func CleanText(text string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(text, "\uFFFD"), "\x00", "")
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

// Attention is one setup problem of a repository ("Needs attention"): Kind
// names it and Message tells the user. Project is "" for a problem of the
// whole instance (an expiring token).
type Attention struct {
	Instance, Project, Kind, Message string
	UpdatedAt                        time.Time
}

// SetAttention records the problem kind of project on instance, replacing the
// message of one already recorded (the project matched ignoring case).
func (s *Store) SetAttention(ctx context.Context, instance, project, kind, message string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO repo_attention (instance, project, kind, message)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance, lower(project), kind) DO UPDATE SET
			project = EXCLUDED.project, message = EXCLUDED.message, updated_at = now()`,
		instance, project, kind, message)
	return err
}

// ClearAttention removes the problem kind of project on instance, the project
// matched ignoring case. Clearing one that isn't recorded is not an error.
func (s *Store) ClearAttention(ctx context.Context, instance, project, kind string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM repo_attention
		WHERE instance = $1 AND lower(project) = lower($2) AND kind = $3`, instance, project, kind)
	return err
}

// DeleteAttentionForProject removes every problem recorded for project on
// instance, matched ignoring case: the repository is switched off. The
// instance-wide items (project "") stay.
func (s *Store) DeleteAttentionForProject(ctx context.Context, instance, project string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM repo_attention
		WHERE instance = $1 AND lower(project) = lower($2)`, instance, project)
	return err
}

// Attention returns every recorded problem, ordered by instance, project
// (ignoring case) and kind.
func (s *Store) Attention(ctx context.Context) ([]Attention, error) {
	rows, err := s.pool.Query(ctx, `SELECT instance, project, kind, message, updated_at FROM repo_attention
		ORDER BY instance, lower(project), kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attention
	for rows.Next() {
		var a Attention
		if err := rows.Scan(&a.Instance, &a.Project, &a.Kind, &a.Message, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountFailedRuns counts the runs of project on instance (matched ignoring
// case) reported since the given time whose failure reason contains
// substring.
func (s *Store) CountFailedRuns(ctx context.Context, instance, project, substring string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs
		WHERE instance = $1 AND lower(project_path) = lower($2)
		  AND reported_at >= $4 AND strpos(failure_reason, $3) > 0`,
		instance, project, substring, since).Scan(&n)
	return n, err
}
