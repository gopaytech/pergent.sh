package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// RunDigest is a run as the dashboard lists it: everything it shows of a
// run in a list, with a summary of the result instead of the result.
type RunDigest struct {
	ID                                          string
	MR                                          MRKey
	ProjectPath, HeadSHA, BaseSHA, TargetBranch string
	State                                       RunState
	FailureReason                               string
	// Title and WebURL fall back to the newest ones another run of the
	// merge request has, for a run whose dispatcher never read them.
	Title, WebURL, SourceBranch, Author string
	// DispatchedAt and ReportedAt are zero until the run gets there.
	CreatedAt, DispatchedAt, ReportedAt time.Time
	// UpdatedAt is the latest of CreatedAt, DispatchedAt and ReportedAt.
	UpdatedAt time.Time
	// Summary is the stored result without its findings, checks,
	// summaries, failures, notes and comment text (the comment's markdown
	// is "-" when it had any): what deciding the run's outcome needs. Nil
	// for a run without a result.
	Summary []byte
	// Approved is true when the approve bot's recorded approval of the
	// merge request is of this run's head and target branch.
	Approved bool
	// Newest is true for the merge request's newest run that isn't
	// superseded.
	Newest bool
	// ApproveAvailable is the stored project's (true when the project
	// isn't stored, as on GitLab).
	ApproveAvailable bool
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
	Limit int
}

// summarySQL is the result of run r without what deciding its outcome
// doesn't need. A result that isn't an object, or whose skills aren't an
// array, is kept as it is or left without skills: it can't be parsed into a
// review either way.
const summarySQL = `CASE
	WHEN r.result IS NULL THEN NULL
	WHEN jsonb_typeof(r.result) <> 'object' THEN r.result
	ELSE (r.result - 'comment' - 'skills')
		|| jsonb_build_object('comment', jsonb_build_object('markdown',
			CASE WHEN COALESCE(r.result->'comment'->>'markdown', '') = '' THEN '' ELSE '-' END))
		|| CASE WHEN jsonb_typeof(r.result->'skills') = 'array' THEN jsonb_build_object('skills',
			(SELECT COALESCE(jsonb_agg(CASE WHEN jsonb_typeof(s) = 'object'
					THEN s - 'findings' - 'checks' - 'summaries' - 'failures' - 'notes' ELSE s END), '[]'::jsonb)
			 FROM jsonb_array_elements(r.result->'skills') s))
		ELSE '{}'::jsonb END
END`

// digestColumns are a RunDigest's columns, of runs r joined by digestJoins.
const digestColumns = `r.id::text, r.instance, r.project_id, r.mr_iid, r.project_path, r.head_sha, r.base_sha,
	r.target_branch, r.state, r.failure_reason,
	COALESCE(NULLIF(r.title, ''), (SELECT t.title FROM runs t
		WHERE t.instance = r.instance AND t.project_id = r.project_id AND t.mr_iid = r.mr_iid AND t.title <> ''
		ORDER BY t.created_at DESC, t.id DESC LIMIT 1), ''),
	COALESCE(NULLIF(r.web_url, ''), (SELECT t.web_url FROM runs t
		WHERE t.instance = r.instance AND t.project_id = r.project_id AND t.mr_iid = r.mr_iid AND t.web_url <> ''
		ORDER BY t.created_at DESC, t.id DESC LIMIT 1), ''),
	r.source_branch, r.author, r.created_at, r.dispatched_at, r.reported_at,
	GREATEST(r.created_at, r.dispatched_at, r.reported_at),
	` + summarySQL + `,
	COALESCE(m.approved_sha <> '' AND m.approved_sha = r.head_sha
		AND (m.approved_target = '' OR m.approved_target = r.target_branch), false),
	COALESCE(r.id = (SELECT n.id FROM runs n
		WHERE n.instance = r.instance AND n.project_id = r.project_id AND n.mr_iid = r.mr_iid
		  AND n.state <> 'superseded'
		ORDER BY n.created_at DESC, n.id DESC LIMIT 1), false),
	COALESCE(p.approve_available, true)`

const digestJoins = `LEFT JOIN merge_requests m
		ON m.instance = r.instance AND m.project_id = r.project_id AND m.mr_iid = r.mr_iid
	LEFT JOIN projects p ON p.instance = r.instance AND p.project_id = r.project_id`

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
			&d.CreatedAt, &dispatched, &rep, &d.UpdatedAt, &d.Summary, &d.Approved, &d.Newest,
			&d.ApproveAvailable); err != nil {
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
// descending), at most f.Limit of them.
func (s *Store) ReviewDigests(ctx context.Context, f ReviewFilter) ([]RunDigest, error) {
	if len(f.Projects) == 0 || f.Limit <= 0 {
		return nil, nil
	}
	var since, afterAt *time.Time
	afterID := "00000000-0000-0000-0000-000000000000"
	if !f.Since.IsZero() {
		since = &f.Since
	}
	if f.After != nil {
		if !validUUID(f.After.ID) {
			return nil, errors.New("store: malformed review cursor")
		}
		afterAt, afterID = &f.After.At, f.After.ID
	}
	return s.queryDigests(ctx, `WITH latest AS (
			SELECT DISTINCT ON (r.project_id, r.mr_iid) r.id FROM runs r
			WHERE r.instance = $1 AND lower(r.project_path) = ANY($2::text[])
			ORDER BY r.project_id, r.mr_iid, r.created_at DESC, r.id DESC)
		SELECT `+digestColumns+` FROM runs r JOIN latest USING (id) `+digestJoins+`
		WHERE ($3::timestamptz IS NULL OR GREATEST(r.created_at, r.dispatched_at, r.reported_at) >= $3)
		  AND ($4::timestamptz IS NULL OR (GREATEST(r.created_at, r.dispatched_at, r.reported_at), r.id) < ($4, $5::uuid))
		ORDER BY GREATEST(r.created_at, r.dispatched_at, r.reported_at) DESC, r.id DESC
		LIMIT $6`, f.Instance, f.Projects, since, afterAt, afterID, f.Limit)
}

// RunDigestsSince returns the runs of projects (lower-cased paths) on
// instance created at or after since that aren't superseded, newest first.
func (s *Store) RunDigestsSince(ctx context.Context, instance string, projects []string, since time.Time) ([]RunDigest, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	return s.queryDigests(ctx, `SELECT `+digestColumns+` FROM runs r `+digestJoins+`
		WHERE r.instance = $1 AND lower(r.project_path) = ANY($2::text[]) AND r.created_at >= $3
		  AND r.state <> 'superseded'
		ORDER BY r.created_at DESC, r.id DESC`, instance, projects, since)
}

// MRRunDigests returns every run of a merge request, oldest first.
func (s *Store) MRRunDigests(ctx context.Context, mr MRKey) ([]RunDigest, error) {
	return s.queryDigests(ctx, `SELECT `+digestColumns+` FROM runs r `+digestJoins+`
		WHERE r.instance = $1 AND r.project_id = $2 AND r.mr_iid = $3
		ORDER BY r.created_at, r.id`, mr.Instance, mr.ProjectID, mr.IID)
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
