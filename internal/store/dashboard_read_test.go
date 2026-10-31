package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// setRun overwrites columns of a run for a test.
func setRun(t *testing.T, s *Store, id, set string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET `+set+` WHERE id = $1::uuid`, append([]any{id}, args...)...); err != nil {
		t.Fatalf("update run %s: %v", id, err)
	}
}

func TestMarkPublishedRecordsDecision(t *testing.T) {
	s := newStore(t)
	id := dispatched(t, s, mr1, "a1", "tok")
	if out, err := s.StoreResult(ctx, id, "tok", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if r := getRun(t, s, id); r.Outcome != "" || r.Approved || r.DecisionStatus != "" {
		t.Fatalf("undecided run = %+v", r)
	}
	draft := RunDecision{Outcome: OutcomeDraft, Reason: "No blocking issues; not approved: draft", Status: "success"}
	if err := s.MarkPublished(ctx, id, draft); err != nil {
		t.Fatal(err)
	}
	r := getRun(t, s, id)
	if r.Outcome != OutcomeDraft || r.OutcomeReason != draft.Reason || r.DecisionStatus != "success" || r.Approved {
		t.Errorf("decided run = %+v", r)
	}
	// Marked ready: decided again, approved.
	if ok, err := s.Republish(ctx, mr1, "a1", "main"); err != nil || !ok {
		t.Fatalf("Republish = %v, %v", ok, err)
	}
	if err := s.MarkPublished(ctx, id, RunDecision{Outcome: OutcomeApproved, Status: "success", Approved: true}); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, id); r.Outcome != OutcomeApproved || r.OutcomeReason != "" || !r.Approved {
		t.Errorf("redecided run = %+v", r)
	}
}

func TestSetRunInputs(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if r := getRun(t, s, id); r.Model != nil || r.SkillSources != nil {
		t.Fatalf("new run = model %v sources %v; want none", r.Model, r.SkillSources)
	}
	if err := s.SetRunInputs(ctx, id, "claude-sonnet-4", map[string]string{"security": "repo", "style": "library"}); err != nil {
		t.Fatal(err)
	}
	r := getRun(t, s, id)
	if r.Model == nil || *r.Model != "claude-sonnet-4" || r.SkillSources["security"] != "repo" || r.SkillSources["style"] != "library" {
		t.Errorf("run = model %v sources %v", r.Model, r.SkillSources)
	}
	if err := s.SetRunInputs(ctx, "bad", "", nil); err == nil {
		t.Error("SetRunInputs of a malformed id = nil error")
	}
}

// decided sets a run published with a recorded outcome.
func decided(t *testing.T, s *Store, id, outcome string, approved bool, at time.Time) {
	t.Helper()
	setRun(t, s, id, `state = 'published', outcome = $2, outcome_reason = 'why', decision_status = 'success',
		approved = $3, created_at = $4::timestamptz, dispatched_at = $4::timestamptz, reported_at = $4::timestamptz + interval '2 minutes'`, outcome, approved, at)
}

func TestReviewDigests(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// mr1: two runs; the second replaces the first.
	old := enqueue(t, s, mr1, "a1")
	setRun(t, s, old, `created_at = $2, title = 'First title', web_url = 'https://gitlab.example.com/mr/3'`, base)
	newer := enqueue(t, s, mr1, "a2")
	setRun(t, s, newer, `created_at = $2, reported_at = $3, state = 'published', failure_reason = 'boom', outcome = 'failed'`,
		base.Add(time.Hour), base.Add(5*time.Hour))
	// mr2: one run, approved, updated in between.
	other := enqueue(t, s, mr2, "b1")
	decided(t, s, other, OutcomeApproved, true, base.Add(2*time.Hour))
	// Another project, not asked for.
	hidden := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 1}
	if _, _, err := s.EnqueueRun(ctx, hidden, "acme/payments", "c1", "main"); err != nil {
		t.Fatal(err)
	}

	f := ReviewFilter{Visible: seen(mr1.Instance, 7, "acme/widgets"), Limit: 10}
	got, err := s.ReviewDigests(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != newer || got[1].ID != other {
		t.Fatalf("ReviewDigests = %+v; want the newer run of mr1, then mr2", got)
	}
	if got[0].Title != "First title" || got[0].WebURL != "https://gitlab.example.com/mr/3" {
		t.Errorf("title fallback = %q %q", got[0].Title, got[0].WebURL)
	}
	if !got[0].UpdatedAt.Equal(base.Add(5*time.Hour)) || got[0].Outcome != OutcomeFailed {
		t.Errorf("mr1 digest = %+v", got[0])
	}
	if got[1].Outcome != OutcomeApproved || !got[1].Approved || got[1].OutcomeReason != "why" || got[1].DecisionStatus != "success" {
		t.Errorf("mr2 digest = %+v", got[1])
	}

	// The outcome filter is applied by the query.
	f.Outcome = OutcomeApproved
	if got, err := s.ReviewDigests(ctx, f); err != nil || len(got) != 1 || got[0].ID != other {
		t.Fatalf("approved only = %+v, %v", got, err)
	}
	f.Outcome = ""

	// Paging: one at a time, then nothing after the last.
	f.Limit = 1
	page, err := s.ReviewDigests(ctx, f)
	if err != nil || len(page) != 1 || page[0].ID != newer {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	f.After = &ReviewCursor{At: page[0].UpdatedAt, ID: page[0].ID}
	page, err = s.ReviewDigests(ctx, f)
	if err != nil || len(page) != 1 || page[0].ID != other {
		t.Fatalf("second page = %+v, %v", page, err)
	}
	f.After = &ReviewCursor{At: page[0].UpdatedAt, ID: page[0].ID}
	if page, err = s.ReviewDigests(ctx, f); err != nil || len(page) != 0 {
		t.Fatalf("third page = %+v, %v", page, err)
	}

	// Since leaves out what was last updated before it.
	f = ReviewFilter{Visible: seen(mr1.Instance, 7, "acme/widgets"), Limit: 10, Since: base.Add(3 * time.Hour)}
	if got, err := s.ReviewDigests(ctx, f); err != nil || len(got) != 1 || got[0].ID != newer {
		t.Fatalf("since = %+v, %v", got, err)
	}
	// No projects, no runs; a malformed cursor is an error.
	if got, err := s.ReviewDigests(ctx, ReviewFilter{Limit: 10}); err != nil || len(got) != 0 {
		t.Fatalf("no projects = %+v, %v", got, err)
	}
	f.After = &ReviewCursor{At: base, ID: "x'; --"}
	if _, err := s.ReviewDigests(ctx, f); err == nil {
		t.Fatal("malformed cursor = nil error")
	}
}

// seen is a Visibility of paths on instance, each with project ID id.
func seen(instance string, id int64, paths ...string) Visibility {
	v := Visibility{}
	for _, p := range paths {
		v.Projects = append(v.Projects, VisibleProject{Instance: instance, Path: p, ID: id})
	}
	return v
}

// A run's outcome follows its state until a decision is recorded; a
// published run without one is "unknown", never guessed.
func TestDigestOutcomes(t *testing.T) {
	s := newStore(t)
	cases := []struct {
		set  string
		want string
	}{
		{`state = 'queued'`, OutcomeRunning},
		{`state = 'running'`, OutcomeRunning},
		{`state = 'reported'`, OutcomeRunning},
		{`state = 'reported', failure_reason = 'boom'`, OutcomeFailed},
		{`state = 'superseded', outcome = 'approved'`, OutcomeSuperseded},
		{`state = 'publish_failed'`, OutcomeFailed},
		{`state = 'published'`, OutcomeUnknown},
		{`state = 'published', outcome = 'draft'`, OutcomeDraft},
	}
	for i, tc := range cases {
		mr := MRKey{Instance: "main", ProjectID: 7, IID: int64(100 + i)}
		id, _, err := s.EnqueueRun(ctx, mr, "acme/widgets", "h", "main")
		if err != nil {
			t.Fatal(err)
		}
		setRun(t, s, id, tc.set)
		got, err := s.MRRunDigests(ctx, mr, seen("main", 7, "acme/widgets"))
		if err != nil || len(got) != 1 || got[0].Outcome != tc.want {
			t.Errorf("%s: digests = %+v, %v; want %s", tc.set, got, err, tc.want)
		}
	}
}

func TestRunStats(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i, tc := range []struct {
		project, outcome string
		approved         bool
		at               time.Time
	}{
		{"acme/widgets", OutcomeApproved, true, base},
		{"Acme/Widgets", OutcomeBlocking, false, base},
		{"acme/widgets", OutcomeFailed, false, base},
		{"acme/widgets", OutcomeApproved, true, base.Add(-time.Hour)}, // too old
		{"acme/tools", OutcomeDraft, false, base},
		{"acme/payments", OutcomeApproved, true, base}, // not asked for
	} {
		mr := MRKey{Instance: "main", ProjectID: 7, IID: int64(i + 1)}
		id, _, err := s.EnqueueRun(ctx, mr, tc.project, "h", "main")
		if err != nil {
			t.Fatal(err)
		}
		decided(t, s, id, tc.outcome, tc.approved, tc.at)
	}
	running, _, _ := s.EnqueueRun(ctx, MRKey{Instance: "main", ProjectID: 7, IID: 50}, "acme/widgets", "h", "main")
	setRun(t, s, running, `created_at = $2`, base)
	superseded, _, _ := s.EnqueueRun(ctx, MRKey{Instance: "main", ProjectID: 7, IID: 51}, "acme/widgets", "h", "main")
	setRun(t, s, superseded, `created_at = $2, state = 'superseded'`, base)

	got, err := s.RunStats(ctx, seen("main", 7, "acme/widgets", "acme/tools"), base)
	if err != nil {
		t.Fatal(err)
	}
	widgets, tools := PathKey{"main", "acme/widgets"}, PathKey{"main", "acme/tools"}
	want := map[PathKey]RunCounts{
		widgets: {Reviews: 4, Approved: 1, Blocking: 1, Failed: 1},
		tools:   {Reviews: 1},
	}
	if len(got) != len(want) || got[widgets] != want[widgets] || got[tools] != want[tools] {
		t.Errorf("RunStats = %+v, want %+v", got, want)
	}
	if got, err := s.RunStats(ctx, Visibility{}, base); err != nil || len(got) != 0 {
		t.Errorf("RunStats without projects = %+v, %v", got, err)
	}
}

// A merge request's runs are listed only for the paths asked for: a run
// whose project moved somewhere hidden is left out.
func TestMRRunDigestsVisibility(t *testing.T) {
	s := newStore(t)
	a := enqueue(t, s, mr1, "a1")
	b, _, err := s.EnqueueRun(ctx, mr1, "secret-group/widgets", "a2", "main")
	if err != nil {
		t.Fatal(err)
	}
	setRun(t, s, b, `title = 'Secret title', web_url = 'https://gitlab.example.com/secret-group/widgets/-/merge_requests/3'`)
	got, err := s.MRRunDigests(ctx, mr1, seen(mr1.Instance, 7, "acme/widgets"))
	if err != nil || len(got) != 1 || got[0].ID != a {
		t.Fatalf("MRRunDigests = %+v, %v; want the acme/widgets run only", got, err)
	}
	if got[0].Title != "" || got[0].WebURL != "" {
		t.Errorf("visible run borrows the hidden run's title or link: %q %q", got[0].Title, got[0].WebURL)
	}
	if got, err := s.MRRunDigests(ctx, mr1, Visibility{}); err != nil || len(got) != 0 {
		t.Fatalf("MRRunDigests without projects = %+v, %v", got, err)
	}
	// The MR's newest run is hidden, so the review isn't listed.
	if got, err := s.ReviewDigests(ctx, ReviewFilter{Visible: seen(mr1.Instance, 7, "acme/widgets"), Limit: 5}); err != nil || len(got) != 0 {
		t.Fatalf("ReviewDigests = %+v, %v; want none", got, err)
	}
}

// A path can pass from a project to a later one (deleted, renamed or
// transferred): only the runs of the project ID that is visible are read.
func TestReadsNeedVisibleProjectID(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	oldMR := MRKey{Instance: "main", ProjectID: 7, IID: 1}
	newMR := MRKey{Instance: "main", ProjectID: 70, IID: 1}
	oldRun := enqueue(t, s, oldMR, "a1")
	decided(t, s, oldRun, OutcomeBlocking, false, base)
	newRun := enqueue(t, s, newMR, "b1")
	decided(t, s, newRun, OutcomeApproved, true, base.Add(time.Hour))
	visible := seen("main", 70, "acme/widgets")

	got, err := s.ReviewDigests(ctx, ReviewFilter{Visible: visible, Limit: 10})
	if err != nil || len(got) != 1 || got[0].ID != newRun {
		t.Errorf("ReviewDigests = %+v, %v; want only the new project's run", got, err)
	}
	stats, err := s.RunStats(ctx, visible, base)
	if want := (RunCounts{Reviews: 1, Approved: 1}); err != nil || stats[PathKey{"main", "acme/widgets"}] != want {
		t.Errorf("RunStats = %+v, %v; want %+v", stats, err, want)
	}
	if got, err := s.MRRunDigests(ctx, oldMR, visible); err != nil || len(got) != 0 {
		t.Errorf("MRRunDigests of the old project = %+v, %v; want none", got, err)
	}
	if got, err := s.MRRunDigests(ctx, newMR, visible); err != nil || len(got) != 1 || got[0].ID != newRun {
		t.Errorf("MRRunDigests of the new project = %+v, %v", got, err)
	}
}

// A run is read only when its instance, path and project ID are those of
// one visible project together: the same path and ID on another instance,
// or the ID of a project visible on another instance, show nothing.
func TestReadsMatchInstancePathAndID(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mainRun := enqueue(t, s, MRKey{Instance: "main", ProjectID: 7, IID: 1}, "a1")
	decided(t, s, mainRun, OutcomeApproved, true, base)
	ghMR := MRKey{Instance: "gh", ProjectID: 7, IID: 1}
	ghRun, _, err := s.EnqueueRun(ctx, ghMR, "acme/widgets", "b1", "main")
	if err != nil {
		t.Fatal(err)
	}
	decided(t, s, ghRun, OutcomeBlocking, false, base.Add(time.Hour))
	// main's acme/widgets (7), and gh's acme/tools with ID 7 too.
	v := Visibility{Projects: []VisibleProject{{"main", "acme/widgets", 7}, {"gh", "acme/tools", 7}}}

	got, err := s.ReviewDigests(ctx, ReviewFilter{Visible: v, Limit: 10})
	if err != nil || len(got) != 1 || got[0].ID != mainRun {
		t.Errorf("ReviewDigests = %+v, %v; want main's run only", got, err)
	}
	stats, err := s.RunStats(ctx, v, base)
	if err != nil || len(stats) != 1 || stats[PathKey{"main", "acme/widgets"}].Reviews != 1 {
		t.Errorf("RunStats = %+v, %v; want main's acme/widgets only", stats, err)
	}
	if got, err := s.MRRunDigests(ctx, ghMR, v); err != nil || len(got) != 0 {
		t.Errorf("MRRunDigests of gh's run = %+v, %v; want none", got, err)
	}
}

// All reads every run, of every instance and project.
func TestVisibilityAll(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mainRun := enqueue(t, s, MRKey{Instance: "main", ProjectID: 7, IID: 1}, "a1")
	decided(t, s, mainRun, OutcomeApproved, true, base)
	ghMR := MRKey{Instance: "gh", ProjectID: 9, IID: 2}
	ghRun, _, err := s.EnqueueRun(ctx, ghMR, "acme/tools", "b1", "main")
	if err != nil {
		t.Fatal(err)
	}
	decided(t, s, ghRun, OutcomeBlocking, false, base.Add(time.Hour))
	all := Visibility{All: true}

	got, err := s.ReviewDigests(ctx, ReviewFilter{Visible: all, Limit: 10})
	if err != nil || len(got) != 2 || got[0].ID != ghRun || got[1].ID != mainRun {
		t.Errorf("ReviewDigests = %+v, %v; want both runs, newest first", got, err)
	}
	stats, err := s.RunStats(ctx, all, base)
	if err != nil || len(stats) != 2 || stats[PathKey{"gh", "acme/tools"}].Blocking != 1 || stats[PathKey{"main", "acme/widgets"}].Approved != 1 {
		t.Errorf("RunStats = %+v, %v", stats, err)
	}
	if got, err := s.MRRunDigests(ctx, ghMR, all); err != nil || len(got) != 1 || got[0].ID != ghRun {
		t.Errorf("MRRunDigests = %+v, %v", got, err)
	}
}

func TestProjectByPath(t *testing.T) {
	s := newStore(t)
	if err := s.UpsertProject(ctx, Project{Instance: "main", ProjectID: 7, Path: "Acme/Widgets"}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.ProjectByPath(ctx, "main", "acme/widgets")
	if err != nil || !ok || p.ProjectID != 7 {
		t.Fatalf("ProjectByPath = %+v, %v, %v", p, ok, err)
	}
	if _, ok, err := s.ProjectByPath(ctx, "other", "acme/widgets"); ok || err != nil {
		t.Fatalf("ProjectByPath on another instance = %v, %v", ok, err)
	}
}

// The dashboard's list queries can use the per-project indexes: they read a
// bounded range of a repository's runs, never the whole table.
func TestDashboardQueriesUseIndexes(t *testing.T) {
	s := newStore(t)
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// A year of runs over 200 repositories, so the planner has a table
	// worth choosing an index for.
	if _, err := s.pool.Exec(ctx, `INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, state, created_at)
		SELECT 'main', i % 200, i, 'acme/repo-' || (i % 200), md5(i::text), 'published',
			$1::timestamptz - (i || ' minutes')::interval
		FROM generate_series(1, 20000) i`, since); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ANALYZE runs`); err != nil {
		t.Fatal(err)
	}
	repo := func(n int64) VisibleProject { return VisibleProject{"main", fmt.Sprintf("acme/repo-%d", n), n} }
	one, oneArgs := visibleCond(Visibility{Projects: []VisibleProject{repo(7)}}, 6)
	two, twoArgs := visibleCond(Visibility{Projects: []VisibleProject{repo(7), repo(8)}}, 6)
	oneStats, oneStatsArgs := visibleCond(Visibility{Projects: []VisibleProject{repo(7)}}, 2)
	for _, q := range []struct {
		name, sql, index string
		args             []any
	}{
		{"reviews", reviewDigestsSQL(one), "runs_project_updated_idx",
			append([]any{since.Add(-7 * 24 * time.Hour), nil, zeroUUID, "", 51}, oneArgs...)},
		{"reviews after a cursor", reviewDigestsSQL(two), "runs_project_updated_idx",
			append([]any{since.Add(-90 * 24 * time.Hour), since.Add(-time.Hour), zeroUUID, "approved", 51}, twoArgs...)},
		{"stats", runStatsSQL(oneStats), "runs_project_created_idx", append([]any{since.Add(-7 * 24 * time.Hour)}, oneStatsArgs...)},
	} {
		plan := explain(t, s, q.sql, q.args...)
		if !strings.Contains(plan, q.index) {
			t.Errorf("%s: plan doesn't use %s:\n%s", q.name, q.index, plan)
		}
		if strings.Contains(plan, "Seq Scan on runs") {
			t.Errorf("%s: plan scans the runs table:\n%s", q.name, plan)
		}
	}
}

// explain returns the plan of sql with sequential scans discouraged, so
// the plan shows the index the query can use even on a tiny table.
func explain(t *testing.T, s *Store, sql string, args ...any) string {
	t.Helper()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN `+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
