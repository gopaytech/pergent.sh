package store

import (
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
	for _, stmt := range []string{
		`INSERT INTO run_logs (run_id, text) VALUES ('` + id + `', 'log')`,
		`INSERT INTO repo_attention (instance, project, kind, message) VALUES ('main', 'acme/widgets', 'no_hook', 'x')`,
		`INSERT INTO sessions (id_hash, platform, instance, user_id, username, is_admin, token_sealed, csrf)
			VALUES ('\x01', 'gitlab', 'main', 1, 'alice', false, '\x02', 'csrf')`,
		`INSERT INTO user_access (session_hash, instance, project) VALUES ('\x01', 'main', 'acme/widgets')`,
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
	_, _, _ = running, logged, old

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
