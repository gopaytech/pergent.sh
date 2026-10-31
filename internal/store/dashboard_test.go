package store

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func ptrTo[T any](v T) *T { return &v }

func TestDashboardReposCRUD(t *testing.T) {
	s := newStore(t)

	if rows, err := s.DashboardRepos(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("DashboardRepos on an empty store = %v, %v", rows, err)
	}
	if _, found, err := s.DashboardRepo(ctx, "main", "acme/widgets"); found || err != nil {
		t.Fatalf("DashboardRepo of a missing row = %v, %v", found, err)
	}

	// Only approve is set: every other setting is NULL, "use the default".
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{
		Instance: "main", Project: "Acme/Widgets", Approve: ptrTo(false), CreatedBy: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	r, found, err := s.DashboardRepo(ctx, "main", "acme/WIDGETS")
	if err != nil || !found {
		t.Fatalf("DashboardRepo = %v, %v", found, err)
	}
	if r.Instance != "main" || r.Project != "Acme/Widgets" || r.CreatedBy != "alice" {
		t.Errorf("row = %+v", r)
	}
	if r.Approve == nil || *r.Approve || r.ReviewDrafts != nil || r.FailureStatus != nil ||
		r.Skills != nil || r.JobTimeoutSeconds != nil || r.JobMemory != nil {
		t.Errorf("nullable columns = %+v", r)
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		t.Errorf("times = %s %s", r.CreatedAt, r.UpdatedAt)
	}

	// An upsert in another case updates the same row: every setting is
	// replaced, the creator and creation time stay.
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{
		Instance: "main", Project: "acme/widgets",
		ReviewDrafts: ptrTo(false), FailureStatus: ptrTo("neutral"), Skills: []string{"security", "style"},
		JobTimeoutSeconds: ptrTo(1200), JobMemory: ptrTo("6Gi"), CreatedBy: "bob",
	}); err != nil {
		t.Fatal(err)
	}
	u, _, err := s.DashboardRepo(ctx, "main", "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if u.Approve != nil || u.ReviewDrafts == nil || *u.ReviewDrafts || u.FailureStatus == nil || *u.FailureStatus != "neutral" ||
		!slices.Equal(u.Skills, []string{"security", "style"}) || u.JobTimeoutSeconds == nil || *u.JobTimeoutSeconds != 1200 ||
		u.JobMemory == nil || *u.JobMemory != "6Gi" {
		t.Errorf("updated row = %+v", u)
	}
	if u.CreatedBy != "alice" || !u.CreatedAt.Equal(r.CreatedAt) {
		t.Errorf("creator changed: %q %s, want alice %s", u.CreatedBy, u.CreatedAt, r.CreatedAt)
	}

	// An empty skill list is stored as NULL: there is no "no skills".
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "gh", Project: "acme/payments", Skills: []string{}}); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.DashboardRepo(ctx, "gh", "acme/payments"); p.Skills != nil {
		t.Errorf("empty skills stored as %#v, want NULL", p.Skills)
	}

	// Bad values are refused by the table.
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "gh", Project: "acme/x", FailureStatus: ptrTo("maybe")}); err == nil {
		t.Errorf("failure_status maybe was stored")
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "gh", Project: "acme/x", JobTimeoutSeconds: ptrTo(0)}); err == nil {
		t.Errorf("job_timeout_seconds 0 was stored")
	}

	rows, err := s.DashboardRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, row := range rows {
		keys = append(keys, row.Instance+":"+row.Project)
	}
	if !slices.Equal(keys, []string{"gh:acme/payments", "main:acme/widgets"}) {
		t.Errorf("DashboardRepos = %v", keys)
	}

	if err := s.DeleteDashboardRepo(ctx, "main", "ACME/widgets"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.DashboardRepo(ctx, "main", "acme/widgets"); found {
		t.Errorf("row still there after delete")
	}
	if err := s.DeleteDashboardRepo(ctx, "main", "acme/widgets"); err != nil {
		t.Errorf("deleting a missing row: %v", err)
	}
}

func TestDashboardMigrationTables(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "aaaa")
	var title, webURL string
	if err := s.pool.QueryRow(ctx, `SELECT title, web_url FROM runs WHERE id = $1`, id).Scan(&title, &webURL); err != nil {
		t.Fatalf("runs.title/web_url: %v", err)
	}
	if title != "" || webURL != "" {
		t.Errorf("title, web_url = %q, %q; want empty", title, webURL)
	}
	account := mustPlatform(t, s, "main", 1, "alice")
	for _, stmt := range []string{
		`INSERT INTO run_logs (run_id, text) VALUES ('` + id + `', 'log')`,
		`INSERT INTO repo_attention (instance, project, kind, message) VALUES ('main', 'acme/widgets', 'no_hook', 'x')`,
		fmt.Sprintf(`INSERT INTO sessions (id_hash, account_id, platform, instance, user_id, username, token_sealed, csrf)
			VALUES ('\x01', %d, 'gitlab', 'main', 1, 'alice', '\x02', 'csrf')`, account.ID),
		`INSERT INTO user_access (session_hash, instance, project, project_id) VALUES ('\x01', 'main', 'acme/widgets', 7)`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// repo_attention is keyed case-insensitively on the project.
	if _, err := s.pool.Exec(ctx, `INSERT INTO repo_attention (instance, project, kind, message)
		VALUES ('main', 'ACME/widgets', 'no_hook', 'y')`); err == nil {
		t.Errorf("a second repo_attention row differing only in case was stored")
	}
	// Deleting a session drops its access rows.
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM user_access`).Scan(&n); err != nil || n != 0 {
		t.Errorf("user_access rows after the session went = %d, %v", n, err)
	}
}

func TestRunLogRoundTrip(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "aaaa")

	if _, found, err := s.RunLog(ctx, id); err != nil || found {
		t.Fatalf("RunLog before = found %v, %v; want not found", found, err)
	}
	if _, found, err := s.RunLog(ctx, "not-a-uuid"); err != nil || found {
		t.Fatalf("RunLog of a malformed id = found %v, %v; want not found", found, err)
	}
	if err := s.PutRunLog(ctx, id, "first\n"); err != nil {
		t.Fatal(err)
	}
	// An empty log is still a log: it marks the run as recorded.
	if err := s.PutRunLog(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	if text, found, err := s.RunLog(ctx, id); err != nil || !found || text != "" {
		t.Errorf("RunLog = %q, %v, %v; want empty and found", text, found, err)
	}
	if err := s.PutRunLog(ctx, id, "second\n"); err != nil {
		t.Fatal(err)
	}
	if text, _, _ := s.RunLog(ctx, id); text != "second\n" {
		t.Errorf("RunLog = %q, want the latest", text)
	}
}

// Postgres text takes neither NUL nor invalid UTF-8: such a log is stored
// with them made safe instead of failing (it is never tried again).
func TestRunLogNULAndInvalidUTF8(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "aaaa")
	if err := s.PutRunLog(ctx, id, "a\x00b \xff\xfe c\n"); err != nil {
		t.Fatalf("PutRunLog with a NUL and invalid bytes: %v", err)
	}
	if text, found, err := s.RunLog(ctx, id); err != nil || !found || text != "ab \uFFFD c\n" {
		t.Errorf("RunLog = %q, %v, %v; want the NUL dropped and the invalid bytes replaced", text, found, err)
	}
}

func TestPruneRunLogs(t *testing.T) {
	s := newStore(t)
	oldID := enqueue(t, s, mr1, "aaaa")
	newID := enqueue(t, s, MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 4}, "bbbb")
	for _, id := range []string{oldID, newID} {
		if err := s.PutRunLog(ctx, id, "log "+id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE run_logs SET created_at = now() - interval '31 days' WHERE run_id = $1::uuid`, oldID); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneRunLogs(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("PruneRunLogs = %d, %v; want 1", n, err)
	}
	if _, found, _ := s.RunLog(ctx, oldID); found {
		t.Errorf("the 31-day-old log is still there")
	}
	if _, found, _ := s.RunLog(ctx, newID); !found {
		t.Errorf("the fresh log was pruned")
	}
	if n, err := s.PruneRunLogs(ctx, time.Now().Add(-30*24*time.Hour)); err != nil || n != 0 {
		t.Errorf("second PruneRunLogs = %d, %v; want 0", n, err)
	}
}

func TestRunsToLog(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	mk := func(iid int64, head string, finish func(id string)) string {
		mr := MRKey{Instance: mr1.Instance, ProjectID: 7, IID: iid}
		id := enqueue(t, s, mr, head)
		if _, err := s.ClaimQueued(ctx, 100, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDispatched(ctx, id, "job-"+head, "b", "s", hash("tok"), now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		finish(id)
		return id
	}
	running := mk(10, "r", func(string) {})
	reported := mk(11, "p", func(id string) {
		if out, err := s.StoreResult(ctx, id, "tok", []byte(`{}`), now); err != nil || out != ResultStored {
			t.Fatalf("StoreResult = %v, %v", out, err)
		}
	})
	logged := mk(12, "l", func(id string) {
		if _, err := s.ReportFailure(ctx, id, "x", now); err != nil {
			t.Fatal(err)
		}
		if err := s.PutRunLog(ctx, id, "have it"); err != nil {
			t.Fatal(err)
		}
	})
	old := mk(13, "o", func(id string) {
		if _, err := s.ReportFailure(ctx, id, "x", now.Add(-5*time.Hour)); err != nil {
			t.Fatal(err)
		}
	})
	// Reported just now, but queued more than a week before: past the
	// lookback that keeps the query on recent rows.
	ancient := mk(14, "a", func(id string) {
		if out, err := s.StoreResult(ctx, id, "tok", []byte(`{}`), now); err != nil || out != ResultStored {
			t.Fatalf("StoreResult = %v, %v", out, err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE runs SET created_at = $2 WHERE id = $1::uuid`, id, now.Add(-8*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	})
	_, _, _, _ = running, logged, old, ancient

	runs, err := s.RunsToLog(ctx, now.Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != reported {
		var ids []string
		for _, r := range runs {
			ids = append(ids, r.ID+":"+string(r.State))
		}
		t.Errorf("RunsToLog = %v; want only the recently reported run %s without a log", ids, reported)
	}
}

func TestAttentionSetClearList(t *testing.T) {
	s := newStore(t)

	if rows, err := s.Attention(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("Attention on an empty store = %v, %v", rows, err)
	}
	if err := s.SetAttention(ctx, "main", "Acme/Widgets", "single_approval", "first"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttention(ctx, "main", "acme/payments", "out_of_memory", "oom"); err != nil {
		t.Fatal(err)
	}
	// Instance-wide: the project is empty.
	if err := s.SetAttention(ctx, "gh", "", "token_expiry", "token"); err != nil {
		t.Fatal(err)
	}
	// Setting it again, in another case, replaces the message.
	if err := s.SetAttention(ctx, "main", "acme/widgets", "single_approval", "second"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Attention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("Attention = %+v; want 3 rows", rows)
	}
	got := map[string]Attention{}
	for _, r := range rows {
		got[r.Instance+"|"+r.Kind] = r
		if r.UpdatedAt.IsZero() {
			t.Errorf("row without a time: %+v", r)
		}
	}
	if r := got["main|single_approval"]; r.Message != "second" || r.Project != "acme/widgets" {
		t.Errorf("single_approval = %+v; want the new message and spelling", r)
	}
	if r := got["gh|token_expiry"]; r.Project != "" || r.Message != "token" {
		t.Errorf("token_expiry = %+v", r)
	}

	// Clearing ignores case and a missing row is not an error.
	if err := s.ClearAttention(ctx, "main", "ACME/WIDGETS", "single_approval"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearAttention(ctx, "main", "acme/widgets", "single_approval"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearAttention(ctx, "gh", "", "token_expiry"); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.Attention(ctx)
	if len(rows) != 1 || rows[0].Kind != "out_of_memory" {
		t.Errorf("after clearing = %+v", rows)
	}
}

func TestCountFailedRuns(t *testing.T) {
	s := newStore(t)
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	fail := func(iid int64, project, reason string, at time.Time) {
		t.Helper()
		mr := MRKey{"main", 7, iid}
		id := dispatched(t, s, mr, "aaa", "tok")
		if _, err := s.pool.Exec(ctx, `UPDATE runs SET project_path = $2 WHERE id = $1::uuid`, id, project); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.ReportFailure(ctx, id, reason, at); err != nil || !ok {
			t.Fatalf("ReportFailure = %v, %v", ok, err)
		}
	}
	oom := "the review job ran out of memory (OOMKilled)"
	fail(1, "acme/widgets", oom, t0.Add(-time.Hour))
	fail(2, "Acme/Widgets", oom, t0.Add(-24*time.Hour))
	fail(3, "acme/widgets", oom, t0.Add(-8*24*time.Hour)) // too old
	fail(4, "acme/widgets", "the review job hit its deadline", t0.Add(-time.Hour))
	fail(5, "acme/payments", oom, t0.Add(-time.Hour))

	n, err := s.CountFailedRuns(ctx, "main", "acme/widgets", "OOMKilled", t0.Add(-7*24*time.Hour))
	if err != nil || n != 2 {
		t.Errorf("CountFailedRuns = %d, %v; want 2", n, err)
	}
	if n, err := s.CountFailedRuns(ctx, "other", "acme/widgets", "OOMKilled", t0.Add(-7*24*time.Hour)); err != nil || n != 0 {
		t.Errorf("another instance = %d, %v; want 0", n, err)
	}
}

func TestDeleteAttentionForProject(t *testing.T) {
	s := newStore(t)
	for _, a := range []Attention{
		{Instance: "main", Project: "Acme/Widgets", Kind: "single_approval"},
		{Instance: "main", Project: "acme/widgets", Kind: "out_of_memory"},
		{Instance: "main", Project: "acme/payments", Kind: "single_approval"},
		{Instance: "other", Project: "acme/widgets", Kind: "single_approval"},
		{Instance: "main", Project: "", Kind: "token_expiry"},
	} {
		if err := s.SetAttention(ctx, a.Instance, a.Project, a.Kind, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteAttentionForProject(ctx, "main", "ACME/widgets"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAttentionForProject(ctx, "main", "acme/none"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Attention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Instance+"|"+r.Project+"|"+r.Kind)
	}
	want := []string{"main||token_expiry", "main|acme/payments|single_approval", "other|acme/widgets|single_approval"}
	if !slices.Equal(got, want) {
		t.Errorf("left = %v; want %v", got, want)
	}
}

func TestSetRunChange(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if err := s.SetRunChange(ctx, id, ChangeDetails{Title: "Add login", WebURL: "https://gitlab.example.com/acme/widgets/-/merge_requests/3",
		SourceBranch: "feature/login", Author: "alice"}); err != nil {
		t.Fatal(err)
	}
	r := getRun(t, s, id)
	if r.Title != "Add login" || r.WebURL != "https://gitlab.example.com/acme/widgets/-/merge_requests/3" ||
		r.SourceBranch != "feature/login" || r.Author != "alice" {
		t.Errorf("run = %+v", r)
	}
	if err := s.SetRunChange(ctx, "not-a-uuid", ChangeDetails{}); err == nil {
		t.Error("SetRunChange of a malformed id = nil error")
	}
}

// InsertDashboardRepo creates a row only when the repository (ignoring
// case) has none: a second admin switching it on doesn't reset its settings.
func TestInsertDashboardRepo(t *testing.T) {
	s := newStore(t)
	created, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/widgets", CreatedBy: "alice"})
	if err != nil || !created {
		t.Fatalf("first insert = %v, %v", created, err)
	}
	if _, err := s.UpdateDashboardRepo(ctx, "main", "acme/widgets", func(r *DashboardRepo) { r.Approve = ptrTo(false) }); err != nil {
		t.Fatal(err)
	}
	created, err = s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "ACME/widgets", CreatedBy: "bob"})
	if err != nil || created {
		t.Fatalf("second insert = %v, %v; want not created", created, err)
	}
	r, _, err := s.DashboardRepo(ctx, "main", "acme/widgets")
	if err != nil || r.CreatedBy != "alice" || r.Project != "acme/widgets" || r.Approve == nil || *r.Approve {
		t.Errorf("row = %+v, %v", r, err)
	}
}

// UpdateDashboardRepo changes an existing row's settings and never creates
// one: a repository switched off meanwhile stays off.
func TestUpdateDashboardRepo(t *testing.T) {
	s := newStore(t)
	found, err := s.UpdateDashboardRepo(ctx, "main", "acme/widgets", func(r *DashboardRepo) { r.Approve = ptrTo(true) })
	if err != nil || found {
		t.Fatalf("update of a missing row = %v, %v", found, err)
	}
	if _, ok, _ := s.DashboardRepo(ctx, "main", "acme/widgets"); ok {
		t.Fatal("update created a row")
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "Acme/Widgets",
		JobMemory: ptrTo("6Gi"), CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	found, err = s.UpdateDashboardRepo(ctx, "main", "acme/WIDGETS", func(r *DashboardRepo) {
		if r.JobMemory == nil || *r.JobMemory != "6Gi" {
			t.Errorf("apply got %+v", r)
		}
		r.Skills = []string{"style"}
		r.JobMemory = nil
		r.Project, r.CreatedBy = "evil/other", "mallory" // not settings: ignored
	})
	if err != nil || !found {
		t.Fatalf("update = %v, %v", found, err)
	}
	r, _, _ := s.DashboardRepo(ctx, "main", "acme/widgets")
	if r.Project != "Acme/Widgets" || r.CreatedBy != "alice" || !slices.Equal(r.Skills, []string{"style"}) || r.JobMemory != nil {
		t.Errorf("row = %+v", r)
	}
}
