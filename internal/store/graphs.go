package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// A repository's code graph (docs/concepts/code-graph.md): one repo_graphs
// row per repository whose Code graph switch is on. It holds the stored graph
// (the file itself is on the graphs volume, which the store never touches),
// when the default branch head was last read, and at most one build.

// GraphBuildState is where a repository's graph build stands.
type GraphBuildState string

const (
	GraphBuildNone    GraphBuildState = ""
	GraphBuildQueued  GraphBuildState = "queued"
	GraphBuildRunning GraphBuildState = "running"
)

// ErrGraphBuildNotFound is returned for a build that does not exist or is not
// running.
var ErrGraphBuildNotFound = errors.New("store: graph build not found")

// graphBuildLock serialises ClaimGraphBuild, so two claimers cannot both
// count a free place and take it.
const graphBuildLock = 0x6772_6170 // "grap"

// RepoGraph is one repository's row. Project keeps the spelling it was first
// written with; lookups ignore case. Commit is "" until a graph is stored. A
// time that is not set is the zero time.
type RepoGraph struct {
	Instance, Project string
	// The stored graph: built from Commit of Branch.
	Branch, Commit string
	BuiltAt        time.Time
	SizeBytes      int64
	Nodes, Edges   int
	// CheckedAt is when the default branch head was last read.
	CheckedAt time.Time
	// The build, when BuildState is not GraphBuildNone. BuildDeadline is
	// when a running build is given up on.
	BuildState                                  GraphBuildState
	BuildID, BuildBranch, BuildCommit, BuildJob string
	QueuedAt, BuildDeadline                     time.Time
	// Error is the last build's failure; "" after a build stored a graph.
	Error    string
	FailedAt time.Time
}

// StoredGraph is what a finished build stored.
type StoredGraph struct {
	SizeBytes    int64
	Nodes, Edges int
}

const repoGraphColumns = `instance, project, branch, commit, built_at, size_bytes, nodes, edges, checked_at,
	build_state, coalesce(build_id::text, ''), build_branch, build_commit, build_job, queued_at, build_deadline,
	error, failed_at`

// clearBuild is the SET list that ends a build.
const clearBuild = `build_state = '', build_id = NULL, build_branch = '', build_commit = '', build_job = '',
	build_token_hash = NULL, build_deadline = NULL, queued_at = NULL`

func scanRepoGraph(row pgx.Row) (RepoGraph, error) {
	var g RepoGraph
	var built, checked, queued, deadline, failed *time.Time
	var state string
	err := row.Scan(&g.Instance, &g.Project, &g.Branch, &g.Commit, &built, &g.SizeBytes, &g.Nodes, &g.Edges, &checked,
		&state, &g.BuildID, &g.BuildBranch, &g.BuildCommit, &g.BuildJob, &queued, &deadline, &g.Error, &failed)
	g.BuildState = GraphBuildState(state)
	for _, t := range []struct {
		dst *time.Time
		src *time.Time
	}{{&g.BuiltAt, built}, {&g.CheckedAt, checked}, {&g.QueuedAt, queued}, {&g.BuildDeadline, deadline}, {&g.FailedAt, failed}} {
		if t.src != nil {
			*t.dst = *t.src
		}
	}
	return g, err
}

// SetDashboardRepoGraph turns the Code graph switch of the dashboard
// repository project on instance (matched ignoring case, paused or not) on or
// off. changed is false when it was so already; found is false when there is
// no such row.
func (s *Store) SetDashboardRepoGraph(ctx context.Context, instance, project string, on bool) (found, changed bool, err error) {
	tag, err := s.pool.Exec(ctx, `UPDATE dashboard_repos SET graph_context = $3, updated_at = now()
		WHERE instance = $1 AND lower(project) = lower($2) AND graph_context <> $3`, instance, project, on)
	if err != nil {
		return false, false, err
	}
	if tag.RowsAffected() > 0 {
		return true, true, nil
	}
	_, found, err = s.AnyDashboardRepo(ctx, instance, project)
	return found, false, err
}

// RepoGraph returns the row of instance's repository project.
func (s *Store) RepoGraph(ctx context.Context, instance, project string) (RepoGraph, bool, error) {
	g, err := scanRepoGraph(s.pool.QueryRow(ctx, `SELECT `+repoGraphColumns+` FROM repo_graphs
		WHERE instance = $1 AND lower(project) = lower($2)`, instance, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoGraph{}, false, nil
	}
	if err != nil {
		return RepoGraph{}, false, err
	}
	return g, true, nil
}

// TakeGraphCheck records a head check of instance's repository project at now
// and reports true, when none was recorded in the gap before now; otherwise it
// changes nothing and reports false. It makes the row when there is none. The
// decision and the write are one statement, so two callers never both get it.
func (s *Store) TakeGraphCheck(ctx context.Context, instance, project string, now time.Time, gap time.Duration) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `INSERT INTO repo_graphs (instance, project, checked_at) VALUES ($1, $2, $3)
		ON CONFLICT (instance, lower(project)) DO UPDATE SET checked_at = $3
			WHERE repo_graphs.checked_at IS NULL OR repo_graphs.checked_at <= $3 - make_interval(secs => $4)
		RETURNING true`, instance, project, now, gap.Seconds()).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// QueueGraphBuild asks for a build of commit of branch for instance's
// repository project. A repository with no build gets a new queued one; a
// queued build is given the newer commit and keeps its place; a running build
// is left alone. It reports whether it queued or changed one: false also when
// there is no row.
func (s *Store) QueueGraphBuild(ctx context.Context, instance, project, branch, commit string, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE repo_graphs SET
			build_id = CASE WHEN build_state = '' THEN gen_random_uuid() ELSE build_id END,
			queued_at = CASE WHEN build_state = '' THEN $5 ELSE queued_at END,
			build_state = 'queued', build_branch = $3, build_commit = $4
		WHERE instance = $1 AND lower(project) = lower($2)
			AND (build_state = '' OR (build_state = 'queued' AND (build_branch <> $3 OR build_commit <> $4)))`,
		instance, project, branch, commit, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ClaimGraphBuild marks the oldest queued build running, with deadline, while
// fewer than limit run, and returns its row; ok is false when none is queued
// or limit builds run.
func (s *Store) ClaimGraphBuild(ctx context.Context, limit int, deadline time.Time) (RepoGraph, bool, error) {
	if limit <= 0 {
		return RepoGraph{}, false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RepoGraph{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(graphBuildLock)); err != nil {
		return RepoGraph{}, false, err
	}
	var running int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM repo_graphs WHERE build_state = 'running'`).Scan(&running); err != nil {
		return RepoGraph{}, false, err
	}
	if running >= limit {
		return RepoGraph{}, false, nil
	}
	g, err := scanRepoGraph(tx.QueryRow(ctx, `UPDATE repo_graphs SET build_state = 'running', build_deadline = $1
		WHERE build_id = (SELECT build_id FROM repo_graphs WHERE build_state = 'queued' ORDER BY queued_at, build_id LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING `+repoGraphColumns, deadline))
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoGraph{}, false, nil
	}
	if err != nil {
		return RepoGraph{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RepoGraph{}, false, err
	}
	return g, true, nil
}

// StartGraphJob records the Job of the running build id: its name, the hash
// of its one-time token and its deadline. Any other build is
// ErrGraphBuildNotFound.
func (s *Store) StartGraphJob(ctx context.Context, id, job string, tokenHash []byte, deadline time.Time) error {
	if !validUUID(id) {
		return ErrGraphBuildNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE repo_graphs SET build_job = $2, build_token_hash = $3, build_deadline = $4
		WHERE build_id = $1::uuid AND build_state = 'running'`, id, job, tokenHash, deadline)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrGraphBuildNotFound
	}
	return nil
}

// GraphBuild returns the row of the running build id whose token hash is
// hash; ok is false for any other build, and for a nil hash.
func (s *Store) GraphBuild(ctx context.Context, id string, hash []byte) (RepoGraph, bool, error) {
	if !validUUID(id) || hash == nil {
		return RepoGraph{}, false, nil
	}
	g, err := scanRepoGraph(s.pool.QueryRow(ctx, `SELECT `+repoGraphColumns+` FROM repo_graphs
		WHERE build_id = $1::uuid AND build_state = 'running' AND build_token_hash = $2`, id, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoGraph{}, false, nil
	}
	if err != nil {
		return RepoGraph{}, false, err
	}
	return g, true, nil
}

// StoreGraph ends the running build id whose token hash is hash: the graph
// of its commit is now the stored one, and the last error is cleared. It
// reports false, and changes nothing, for any other build.
func (s *Store) StoreGraph(ctx context.Context, id string, hash []byte, g StoredGraph, now time.Time) (bool, error) {
	if !validUUID(id) || hash == nil {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE repo_graphs SET branch = build_branch, commit = build_commit, built_at = $3,
			size_bytes = $4, nodes = $5, edges = $6, error = '', failed_at = NULL, `+clearBuild+`
		WHERE build_id = $1::uuid AND build_state = 'running' AND build_token_hash = $2`,
		id, hash, now, g.SizeBytes, g.Nodes, g.Edges)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// FailGraphBuild ends the build id, queued or running, with reason. The
// stored graph stays. It reports false for a build that is not there.
func (s *Store) FailGraphBuild(ctx context.Context, id, reason string, now time.Time) (bool, error) {
	if !validUUID(id) {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE repo_graphs SET error = $2, failed_at = $3, `+clearBuild+`
		WHERE build_id = $1::uuid AND build_state <> ''`, id, reason, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ExpiredGraphBuilds returns the running builds whose deadline is before now.
func (s *Store) ExpiredGraphBuilds(ctx context.Context, now time.Time) ([]RepoGraph, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+repoGraphColumns+` FROM repo_graphs
		WHERE build_state = 'running' AND build_deadline < $1 ORDER BY build_deadline`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoGraph
	for rows.Next() {
		g, err := scanRepoGraph(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteRepoGraph deletes the row of instance's repository project and
// returns its build's Job name ("" when none started). found is false when
// there was no row.
func (s *Store) DeleteRepoGraph(ctx context.Context, instance, project string) (job string, found bool, err error) {
	err = s.pool.QueryRow(ctx, `DELETE FROM repo_graphs WHERE instance = $1 AND lower(project) = lower($2)
		RETURNING build_job`, instance, project).Scan(&job)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return job, true, nil
}

// GraphRunRef is the change a review downloads a graph for.
type GraphRunRef struct{ Instance, Project, TargetBranch string }

// GraphRun returns the review run id when its result token's hash is hash,
// the token has not expired at now, and the run is running. It does not spend
// the token. An API review (no repository) is never returned.
func (s *Store) GraphRun(ctx context.Context, id string, hash []byte, now time.Time) (GraphRunRef, bool, error) {
	if !validUUID(id) || hash == nil {
		return GraphRunRef{}, false, nil
	}
	var r GraphRunRef
	err := s.pool.QueryRow(ctx, `SELECT instance, project_path, target_branch FROM runs
		WHERE id = $1::uuid AND kind = 'mr' AND state = 'running' AND token_hash = $2 AND token_expires_at >= $3`,
		id, hash, now).Scan(&r.Instance, &r.Project, &r.TargetBranch)
	if errors.Is(err, pgx.ErrNoRows) {
		return GraphRunRef{}, false, nil
	}
	if err != nil {
		return GraphRunRef{}, false, err
	}
	return r, true, nil
}
