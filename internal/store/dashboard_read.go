package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// RunDigest is a run as the dashboard lists it.
type RunDigest struct {
	ID                                          string
	MR                                          MRKey
	ProjectPath, HeadSHA, BaseSHA, TargetBranch string
	State                                       RunState
	FailureReason                               string
	// Title and WebURL fall back to the newest ones another run of the
	// merge request under the same project path has, for a run whose
	// dispatcher never read them (never from another path, which may be
	// one the reader can't see).
	Title, WebURL, SourceBranch, Author string
	// DispatchedAt and ReportedAt are zero until the run gets there.
	CreatedAt, DispatchedAt, ReportedAt time.Time
	// UpdatedAt is the latest of CreatedAt, DispatchedAt and ReportedAt.
	UpdatedAt time.Time
	// Outcome is the recorded decision's while the run is published, and
	// follows from the state otherwise (see outcomeSQL).
	Outcome string
	// OutcomeReason, DecisionStatus and Approved are the recorded
	// decision's ("", "", false before one).
	OutcomeReason, DecisionStatus string
	Approved                      bool
}

// ReviewCursor is where a page of ReviewDigests starts: after the review
// updated at At whose newest run is ID.
type ReviewCursor struct {
	At time.Time
	ID string
}

// ReviewFilter selects the merge requests ReviewDigests lists. Projects
// holds lower-cased project paths: only runs of those, on Instance, are
// read, and an empty list reads none.
type ReviewFilter struct {
	Instance string
	Projects []string
	// Since, when not zero, leaves out reviews last updated before it.
	Since time.Time
	// After, when not nil, starts the page after that review.
	After *ReviewCursor
	// Outcome, when not "", keeps only reviews with that outcome.
	Outcome string
	Limit   int
}

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// outcomeSQL is run r's outcome: the recorded decision's once it is
// published, otherwise what its state says. A published run without a
// recorded decision (published before decisions were recorded) is
// "unknown": nothing is guessed.
const outcomeSQL = `(CASE r.state
	WHEN 'queued' THEN 'running'
	WHEN 'running' THEN 'running'
	WHEN 'reported' THEN CASE WHEN r.failure_reason <> '' THEN 'failed' ELSE 'running' END
	WHEN 'superseded' THEN 'superseded'
	WHEN 'publish_failed' THEN 'failed'
	ELSE COALESCE(NULLIF(r.outcome, ''), 'unknown') END)`

// digestColumns are a RunDigest's columns, of runs r.
const digestColumns = `r.id::text, r.instance, r.project_id, r.mr_iid, r.project_path, r.head_sha, r.base_sha,
	r.target_branch, r.state, r.failure_reason,
	COALESCE(NULLIF(r.title, ''), (SELECT t.title FROM runs t
		WHERE t.instance = r.instance AND t.project_id = r.project_id AND t.mr_iid = r.mr_iid AND t.title <> ''
		  AND lower(t.project_path) = lower(r.project_path)
		ORDER BY t.created_at DESC, t.id DESC LIMIT 1), ''),
	COALESCE(NULLIF(r.web_url, ''), (SELECT t.web_url FROM runs t
		WHERE t.instance = r.instance AND t.project_id = r.project_id AND t.mr_iid = r.mr_iid AND t.web_url <> ''
		  AND lower(t.project_path) = lower(r.project_path)
		ORDER BY t.created_at DESC, t.id DESC LIMIT 1), ''),
	r.source_branch, r.author, r.created_at, r.dispatched_at, r.reported_at, r.updated_at,
	` + outcomeSQL + `, r.outcome_reason, r.decision_status, r.approved`

// reviewDigestsSQL lists the newest run of each merge request, read through
// runs_project_updated_idx: only the runs of the asked-for projects updated
// in the window ($3, before the cursor $4/$5) are looked at, each kept when
// no newer run of its merge request exists (runs_mr_idx). $6 is the
// outcome filter (” for any), $7 the limit.
const reviewDigestsSQL = `SELECT ` + digestColumns + ` FROM runs r
	WHERE r.instance = $1 AND lower(r.project_path) = ANY($2::text[])
	  AND r.updated_at >= $3::timestamptz
	  AND ($4::timestamptz IS NULL OR (r.updated_at, r.id) < ($4::timestamptz, $5::uuid))
	  AND NOT EXISTS (SELECT 1 FROM runs n
		WHERE n.instance = r.instance AND n.project_id = r.project_id AND n.mr_iid = r.mr_iid
		  AND (n.created_at, n.id) > (r.created_at, r.id))
	  AND ($6 = '' OR ` + outcomeSQL + ` = $6)
	ORDER BY r.updated_at DESC, r.id DESC
	LIMIT $7`

// runStatsSQL counts the runs started since $3 that aren't superseded, per
// project, by outcome, through runs_project_created_idx.
const runStatsSQL = `SELECT lower(r.project_path), count(*),
		count(*) FILTER (WHERE r.state = 'published' AND r.approved),
		count(*) FILTER (WHERE ` + outcomeSQL + ` = 'blocking'),
		count(*) FILTER (WHERE ` + outcomeSQL + ` = 'failed')
	FROM runs r
	WHERE r.instance = $1 AND lower(r.project_path) = ANY($2::text[]) AND r.created_at >= $3
	  AND r.state <> 'superseded'
	GROUP BY lower(r.project_path)`

func (s *Store) queryDigests(ctx context.Context, sql string, args ...any) ([]RunDigest, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunDigest
	for rows.Next() {
		var (
			d               RunDigest
			state           string
			dispatched, rep *time.Time
		)
		if err := rows.Scan(&d.ID, &d.MR.Instance, &d.MR.ProjectID, &d.MR.IID, &d.ProjectPath, &d.HeadSHA, &d.BaseSHA,
			&d.TargetBranch, &state, &d.FailureReason, &d.Title, &d.WebURL, &d.SourceBranch, &d.Author,
			&d.CreatedAt, &dispatched, &rep, &d.UpdatedAt, &d.Outcome, &d.OutcomeReason, &d.DecisionStatus,
			&d.Approved); err != nil {
			return nil, err
		}
		d.State = RunState(state)
		d.DispatchedAt, d.ReportedAt = deref(dispatched), deref(rep)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReviewDigests returns the newest run of each merge request of f's
// projects on f.Instance, the most recently updated first (ties by run ID,
// descending), at most f.Limit of them. A merge request whose newest run
// is of a project not in f.Projects is left out.
func (s *Store) ReviewDigests(ctx context.Context, f ReviewFilter) ([]RunDigest, error) {
	if len(f.Projects) == 0 || f.Limit <= 0 {
		return nil, nil
	}
	var afterAt *time.Time
	afterID := zeroUUID
	if f.After != nil {
		if !validUUID(f.After.ID) {
			return nil, errors.New("store: malformed review cursor")
		}
		afterAt, afterID = &f.After.At, f.After.ID
	}
	return s.queryDigests(ctx, reviewDigestsSQL, f.Instance, f.Projects, f.Since, afterAt, afterID, f.Outcome, f.Limit)
}

// RunCounts counts a project's runs by outcome.
type RunCounts struct {
	Reviews, Approved, Blocking, Failed int
}

// RunStats counts the runs of projects (lower-cased paths) on instance
// started at or after since that aren't superseded, by lower-cased
// project. Approved counts the runs the approve bot approved, also when the
// approval was withdrawn later.
func (s *Store) RunStats(ctx context.Context, instance string, projects []string, since time.Time) (map[string]RunCounts, error) {
	out := map[string]RunCounts{}
	if len(projects) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, runStatsSQL, instance, projects, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			project string
			c       RunCounts
		)
		if err := rows.Scan(&project, &c.Reviews, &c.Approved, &c.Blocking, &c.Failed); err != nil {
			return nil, err
		}
		out[project] = c
	}
	return out, rows.Err()
}

// MRRunDigests returns the runs of a merge request whose project is one of
// projects (lower-cased paths), oldest first.
func (s *Store) MRRunDigests(ctx context.Context, mr MRKey, projects []string) ([]RunDigest, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	return s.queryDigests(ctx, `SELECT `+digestColumns+` FROM runs r
		WHERE r.instance = $1 AND r.project_id = $2 AND r.mr_iid = $3 AND lower(r.project_path) = ANY($4::text[])
		ORDER BY r.created_at, r.id`, mr.Instance, mr.ProjectID, mr.IID, projects)
}

// FindMR returns the merge request number of project (matched ignoring
// case) on instance has runs for: the one of its newest such run, as a
// project can be deleted and created again under the same path. ok is false
// when there is none.
func (s *Store) FindMR(ctx context.Context, instance, project string, number int64) (MRKey, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT project_id FROM runs
		WHERE instance = $1 AND lower(project_path) = lower($2) AND mr_iid = $3
		ORDER BY created_at DESC, id DESC LIMIT 1`, instance, project, number).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return MRKey{}, false, nil
	}
	if err != nil {
		return MRKey{}, false, err
	}
	return MRKey{Instance: instance, ProjectID: id, IID: number}, true, nil
}

// ProjectByPath returns the stored project whose path is project (matched
// ignoring case) on instance; ok is false when there is none.
func (s *Store) ProjectByPath(ctx context.Context, instance, project string) (Project, bool, error) {
	p, err := scanProject(s.pool.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects
		WHERE instance = $1 AND lower(path) = lower($2) ORDER BY project_id DESC LIMIT 1`, instance, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, err
	}
	return p, true, nil
}
