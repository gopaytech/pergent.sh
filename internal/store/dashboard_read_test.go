package store

import (
	"encoding/json"
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

func TestRunDigestSummaryDropsFindings(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	result := `{"schema_version":1,"status":"completed","base":"b","head":"a1",
		"skills":[{"name":"x","verdict":"has_blocking_issues","findings":[{"title":"leak"}],
			"checks":[{"batch":1}],"files":{"reviewed":["a.go"]}}, "odd"],
		"comment":{"markdown":"the whole comment","findings_block":"<!-- -->"}}`
	setRun(t, s, id, `state = 'published', result = $2::jsonb, base_sha = 'b'`, result)

	got, err := s.MRRunDigests(ctx, mr1)
	if err != nil || len(got) != 1 {
		t.Fatalf("MRRunDigests = %v, %v", got, err)
	}
	var sum map[string]any
	if err := json.Unmarshal(got[0].Summary, &sum); err != nil {
		t.Fatalf("summary %s: %v", got[0].Summary, err)
	}
	skills := sum["skills"].([]any)
	skill := skills[0].(map[string]any)
	if _, ok := skill["findings"]; ok {
		t.Errorf("summary keeps findings: %s", got[0].Summary)
	}
	if _, ok := skill["checks"]; ok {
		t.Errorf("summary keeps checks: %s", got[0].Summary)
	}
	if skill["verdict"] != "has_blocking_issues" || skills[1] != "odd" {
		t.Errorf("summary skills = %v", skills)
	}
	if c := sum["comment"].(map[string]any); c["markdown"] != "-" || c["findings_block"] != nil {
		t.Errorf("summary comment = %v", c)
	}

	// A result that isn't an object, or whose skills aren't a list, is
	// read without an error.
	for _, odd := range []string{`"text"`, `[1,2]`, `{"status":"completed","skills":"no"}`, `{"comment":5}`} {
		setRun(t, s, id, `result = $2::jsonb`, odd)
		if _, err := s.MRRunDigests(ctx, mr1); err != nil {
			t.Errorf("result %s: %v", odd, err)
		}
	}
}

func TestReviewDigests(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// mr1: two runs; the second replaces the first.
	old := enqueue(t, s, mr1, "a1")
	setRun(t, s, old, `created_at = $2, title = 'First title', web_url = 'https://gitlab.example.com/mr/3'`, base)
	newer := enqueue(t, s, mr1, "a2")
	setRun(t, s, newer, `created_at = $2, reported_at = $3, state = 'published', failure_reason = 'boom'`,
		base.Add(time.Hour), base.Add(5*time.Hour))
	// mr2: one run, updated in between.
	other := enqueue(t, s, mr2, "b1")
	setRun(t, s, other, `created_at = $2, title = 'Second'`, base.Add(2*time.Hour))
	// Another project, not asked for.
	hidden := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 1}
	if _, _, err := s.EnqueueRun(ctx, hidden, "acme/payments", "c1", "main"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetApprovedSHA(ctx, mr2, "b1", "main"); err != nil {
		t.Fatal(err)
	}

	f := ReviewFilter{Instance: mr1.Instance, Projects: []string{"acme/widgets"}, Limit: 10}
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
	if !got[0].UpdatedAt.Equal(base.Add(5*time.Hour)) || !got[0].Newest || got[0].Approved {
		t.Errorf("mr1 digest = %+v", got[0])
	}
	if !got[1].Approved || !got[1].ApproveAvailable {
		t.Errorf("mr2 digest = %+v; want approved", got[1])
	}

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
	f = ReviewFilter{Instance: mr1.Instance, Projects: []string{"acme/widgets"}, Limit: 10, Since: base.Add(3 * time.Hour)}
	if got, err := s.ReviewDigests(ctx, f); err != nil || len(got) != 1 || got[0].ID != newer {
		t.Fatalf("since = %+v, %v", got, err)
	}
	// No projects, no runs; a malformed cursor is an error.
	if got, err := s.ReviewDigests(ctx, ReviewFilter{Instance: mr1.Instance, Limit: 10}); err != nil || len(got) != 0 {
		t.Fatalf("no projects = %+v, %v", got, err)
	}
	f.After = &ReviewCursor{At: base, ID: "x'; --"}
	if _, err := s.ReviewDigests(ctx, f); err == nil {
		t.Fatal("malformed cursor = nil error")
	}
}

func TestRunDigestsSinceAndFindMR(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a := enqueue(t, s, mr1, "a1")
	setRun(t, s, a, `created_at = $2`, base)
	b := enqueue(t, s, mr1, "a2") // supersedes a
	setRun(t, s, b, `created_at = $2`, base.Add(time.Hour))
	c := enqueue(t, s, mr2, "b1")
	setRun(t, s, c, `created_at = $2`, base.Add(-time.Hour))

	got, err := s.RunDigestsSince(ctx, mr1.Instance, []string{"acme/widgets"}, base)
	if err != nil || len(got) != 1 || got[0].ID != b {
		t.Fatalf("RunDigestsSince = %+v, %v; want only the live run since base", got, err)
	}

	key, ok, err := s.FindMR(ctx, mr1.Instance, "ACME/Widgets", 3)
	if err != nil || !ok || key != mr1 {
		t.Fatalf("FindMR = %+v, %v, %v", key, ok, err)
	}
	if _, ok, err := s.FindMR(ctx, mr1.Instance, "acme/widgets", 99); ok || err != nil {
		t.Fatalf("FindMR of a missing MR = %v, %v", ok, err)
	}

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
