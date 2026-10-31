package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

// Visibility is which runs a reader may see: every run (All: an operator),
// or only the runs of Projects, each matched on its instance, lower-cased
// path and project ID together (see visibleCond). An empty list reads none.
type Visibility struct {
	All      bool
	Projects []VisibleProject
}

// none reports whether v lets nothing be read.
func (v Visibility) none() bool { return !v.All && len(v.Projects) == 0 }

// PathKey is a repository by its instance and lower-cased path.
type PathKey struct{ Instance, Path string }

// ReviewFilter selects the merge requests ReviewDigests lists, of the
// projects Visible lets be read.
type ReviewFilter struct {
	Visible Visibility
	// Since, when not zero, leaves out reviews last updated before it.
	Since time.Time
	// After, when not nil, starts the page after that review.
	After *ReviewCursor
	// Outcome, when not "", keeps only reviews with that outcome.
	Outcome string
	Limit   int
}

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// visibleCond is the condition, ANDed to a query of runs r, that keeps the
// runs v lets be read, with its arrays as parameters $n to $n+3: "" and
// none for All. A run is read only when its instance, path and project ID
// are those of one visible project together (the last array, of
// "instance\npath\nID" keys), so a run of an earlier project at a path now
// held by another (deleted, renamed or transferred), or of a project with
// the same path or ID on another instance, stays hidden. The three lists
// before it, each matched apart, show nothing more; they let the
// per-project indexes serve the query.
func visibleCond(v Visibility, n int) (string, []any) {
	if v.All {
		return "", nil
	}
	instances := make([]string, 0, len(v.Projects))
	paths := make([]string, 0, len(v.Projects))
	ids := make([]int64, 0, len(v.Projects))
	keys := make([]string, 0, len(v.Projects))
	for _, p := range v.Projects {
		path := strings.ToLower(p.Path)
		instances, paths, ids = append(instances, p.Instance), append(paths, path), append(ids, p.ID)
		keys = append(keys, p.Instance+"\n"+path+"\n"+strconv.FormatInt(p.ID, 10))
	}
	return fmt.Sprintf(` AND r.instance = ANY($%d::text[]) AND lower(r.project_path) = ANY($%d::text[])
	  AND r.project_id = ANY($%d::bigint[])
	  AND (r.instance || E'\n' || lower(r.project_path) || E'\n' || r.project_id::text) = ANY($%d::text[])`,
		n, n+1, n+2, n+3), []any{instances, paths, ids, keys}
}

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
// runs_project_updated_idx: only the runs of the visible projects (visible,
// from visibleCond with n = 6) updated in the window ($1, before the cursor
// $2/$3) are looked at, each kept when no newer run of its merge request
// exists (runs_mr_idx). $4 is the outcome filter (empty for any), $5 the limit.
func reviewDigestsSQL(visible string) string {
	return `SELECT ` + digestColumns + ` FROM runs r
	WHERE r.updated_at >= $1::timestamptz` + visible + `
	  AND ($2::timestamptz IS NULL OR (r.updated_at, r.id) < ($2::timestamptz, $3::uuid))
	  AND NOT EXISTS (SELECT 1 FROM runs n
		WHERE n.instance = r.instance AND n.project_id = r.project_id AND n.mr_iid = r.mr_iid
		  AND (n.created_at, n.id) > (r.created_at, r.id))
	  AND ($4 = '' OR ` + outcomeSQL + ` = $4)
	ORDER BY r.updated_at DESC, r.id DESC
	LIMIT $5`
}

// runStatsSQL counts the runs started since $1 that aren't superseded, per
// project, by outcome, through runs_project_created_idx; visible is from
// visibleCond with n = 2.
func runStatsSQL(visible string) string {
	return `SELECT r.instance, lower(r.project_path), count(*),
		count(*) FILTER (WHERE r.state = 'published' AND r.approved),
		count(*) FILTER (WHERE ` + outcomeSQL + ` = 'blocking'),
		count(*) FILTER (WHERE ` + outcomeSQL + ` = 'failed')
	FROM runs r
	WHERE r.created_at >= $1 AND r.state <> 'superseded'` + visible + `
	GROUP BY r.instance, lower(r.project_path)`
}

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

// ReviewDigests returns the newest run of each merge request of the
// projects f.Visible lets be read, the most recently updated first (ties by
// run ID, descending), at most f.Limit of them. A merge request whose
// newest run can't be read is left out.
func (s *Store) ReviewDigests(ctx context.Context, f ReviewFilter) ([]RunDigest, error) {
	if f.Visible.none() || f.Limit <= 0 {
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
	cond, args := visibleCond(f.Visible, 6)
	return s.queryDigests(ctx, reviewDigestsSQL(cond), append([]any{f.Since, afterAt, afterID, f.Outcome, f.Limit}, args...)...)
}

// RunCounts counts a project's runs by outcome.
type RunCounts struct {
	Reviews, Approved, Blocking, Failed int
}

// RunStats counts the runs v lets be read started at or after since that
// aren't superseded, by project. Approved counts the runs the approve bot
// approved, also when the approval was withdrawn later.
func (s *Store) RunStats(ctx context.Context, v Visibility, since time.Time) (map[PathKey]RunCounts, error) {
	out := map[PathKey]RunCounts{}
	if v.none() {
		return out, nil
	}
	cond, args := visibleCond(v, 2)
	rows, err := s.pool.Query(ctx, runStatsSQL(cond), append([]any{since}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			k PathKey
			c RunCounts
		)
		if err := rows.Scan(&k.Instance, &k.Path, &c.Reviews, &c.Approved, &c.Blocking, &c.Failed); err != nil {
			return nil, err
		}
		out[k] = c
	}
	return out, rows.Err()
}

// MRRunDigests returns the runs of a merge request that v lets be read,
// oldest first.
func (s *Store) MRRunDigests(ctx context.Context, mr MRKey, v Visibility) ([]RunDigest, error) {
	if v.none() {
		return nil, nil
	}
	cond, args := visibleCond(v, 4)
	return s.queryDigests(ctx, `SELECT `+digestColumns+` FROM runs r
		WHERE r.instance = $1 AND r.project_id = $2 AND r.mr_iid = $3`+cond+`
		ORDER BY r.created_at, r.id`, append([]any{mr.Instance, mr.ProjectID, mr.IID}, args...)...)
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
