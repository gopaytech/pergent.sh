package github

import (
	"archive/zip"
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckRunsQuery(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{"total_count": 2, "check_runs": []map[string]any{
		{"id": 1, "name": "test (1.22)", "status": "completed", "conclusion": "success",
			"app": map[string]any{"id": 15368, "slug": "github-actions"}, "output": map[string]any{"title": "T", "summary": "S", "text": "X"}},
		{"id": 2, "name": "test", "status": "completed", "conclusion": "failure"},
	}}))
	got, err := c.CheckRuns(ctx, "acme", "widgets", "abc", "test (1.22)")
	if err != nil {
		t.Fatal(err)
	}
	r := rec.get(0)
	if r.Path != "/repos/acme/widgets/commits/abc/check-runs" {
		t.Errorf("path = %s", r.Path)
	}
	if !strings.Contains(r.Query, "check_name=test+%281.22%29") && !strings.Contains(r.Query, "check_name=test%20%281.22%29") {
		t.Errorf("query = %s; want the name query-encoded", r.Query)
	}
	if !strings.Contains(r.Query, "filter=latest") {
		t.Errorf("query = %s; want filter=latest", r.Query)
	}
	// The exact name is kept: "test" doesn't match "test (1.22)".
	if len(got) != 1 || got[0].ID != 1 || got[0].AppSlug != "github-actions" || got[0].AppID != 15368 || got[0].Title != "T" || got[0].Text != "X" {
		t.Errorf("runs = %+v; want only test (1.22)", got)
	}
}

func TestCheckSuites(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{"total_count": 1, "check_suites": []map[string]any{
		{"id": 9, "status": "queued", "latest_check_runs_count": 3, "app": map[string]any{"id": 77}},
	}}))
	got, err := c.CheckSuites(ctx, "acme", "widgets", "abc")
	if err != nil || len(got) != 1 || got[0] != (CheckSuite{ID: 9, Status: "queued", RunCount: 3, AppID: 77}) {
		t.Fatalf("suites = %+v, %v", got, err)
	}
	expect(t, rec.get(0), "GET", "/repos/acme/widgets/commits/abc/check-suites", "per_page=100")
}

func TestCheckRunAnnotationsCapped(t *testing.T) {
	c, rec := serve(t, jsonReply([]map[string]any{
		{"path": "a.go", "start_line": 3, "annotation_level": "failure", "title": "bad", "message": "oops"},
		{"path": "b.go", "start_line": 4, "annotation_level": "warning", "message": "hmm"},
	}))
	got, err := c.CheckRunAnnotations(ctx, "acme", "widgets", 5, 1)
	if err != nil || len(got) != 1 || !strings.Contains(got[0], "a.go:3") || !strings.Contains(got[0], "oops") {
		t.Fatalf("annotations = %q, %v", got, err)
	}
	if rec.get(0).Path != "/repos/acme/widgets/check-runs/5/annotations" {
		t.Errorf("path = %s", rec.get(0).Path)
	}
}

func TestActionsJobRunID(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{"id": 5, "run_id": 77}))
	id, err := c.ActionsJobRunID(ctx, "acme", "widgets", 5)
	if err != nil || id != 77 {
		t.Fatalf("run id = %d, %v", id, err)
	}
	if rec.get(0).Path != "/repos/acme/widgets/actions/jobs/5" {
		t.Errorf("path = %s", rec.get(0).Path)
	}
}

func TestJobLogsFollowsRedirect(t *testing.T) {
	var gotAuth = "unset"
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("line1\nline2\n"))
	}))
	defer store.Close()
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, store.URL+"/blob?sig=x", http.StatusFound)
	})
	rc, err := c.JobLogs(ctx, "acme", "widgets", 5)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(rc)
	if b.String() != "line1\nline2\n" {
		t.Errorf("body = %q", b.String())
	}
	if gotAuth != "" {
		t.Errorf("the storage host got Authorization %q", gotAuth)
	}
	if rec.get(0).Path != "/repos/acme/widgets/actions/jobs/5/logs" || rec.get(0).Header.Get("Authorization") == "" {
		t.Errorf("first request = %+v", rec.get(0))
	}
}

func TestJobLogsForbidden(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"message":"no"}`, 403) })
	if _, err := c.JobLogs(ctx, "acme", "widgets", 5); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden", err)
	}
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// artifactServer lists one artifact "report" and serves zipBody for it.
func artifactServer(t *testing.T, zipBody []byte) (*Client, *recorder) {
	t.Helper()
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/actions/runs/77/artifacts":
			jsonReply(map[string]any{"artifacts": []map[string]any{
				{"id": 1, "name": "old", "expired": true},
				{"id": 4, "name": "report", "expired": false},
			}})(w, r)
		case "/repos/acme/widgets/actions/artifacts/4/zip":
			_, _ = w.Write(zipBody)
		default:
			http.NotFound(w, r)
		}
	})
}

func TestRunArtifactFile(t *testing.T) {
	z := makeZip(t, map[string]string{"cycode.json": `{"ok":true}`, "dir/": "", "sub/x.json": "x"})
	c, rec := artifactServer(t, z)
	got, err := c.RunArtifactFile(ctx, "acme", "widgets", 77, "report", "cycode.json")
	if err != nil || got != `{"ok":true}` {
		t.Fatalf("file = %q, %v", got, err)
	}
	if !strings.Contains(rec.get(0).Query, "name=report") {
		t.Errorf("query = %s", rec.get(0).Query)
	}
	if _, err := c.RunArtifactFile(ctx, "acme", "widgets", 77, "report", "nope.json"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file err = %v; want ErrNotFound", err)
	}
	if _, err := c.RunArtifactFile(ctx, "acme", "widgets", 77, "report", "dir/"); !errors.Is(err, ErrNotFound) {
		t.Errorf("directory err = %v; want ErrNotFound", err)
	}
	if _, err := c.RunArtifactFile(ctx, "acme", "widgets", 77, "report", "dir"); !errors.Is(err, ErrNotFound) {
		t.Errorf("directory (no slash) err = %v; want ErrNotFound", err)
	}
	if _, err := c.RunArtifactFile(ctx, "acme", "widgets", 77, "other", "cycode.json"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown artifact err = %v; want ErrNotFound", err)
	}
}

func TestRunArtifactFileTooLarge(t *testing.T) {
	c, _ := artifactServer(t, make([]byte, 20<<20+1))
	_, err := c.RunArtifactFile(ctx, "acme", "widgets", 77, "report", "cycode.json")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want an error that is not ErrNotFound", err)
	}
}

func TestRecentCheckRunNames(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/commits/a/") {
			jsonReply(map[string]any{"check_runs": []map[string]any{{"name": "test"}, {"name": "lint"}}})(w, r)
			return
		}
		jsonReply(map[string]any{"check_runs": []map[string]any{{"name": "test"}, {"name": "build"}}})(w, r)
	})
	got, err := c.RecentCheckRunNames(ctx, "acme", "widgets", []string{"a", "b"})
	if err != nil || strings.Join(got, ",") != "build,lint,test" {
		t.Fatalf("names = %v, %v", got, err)
	}
	if len(rec.all()) != 2 {
		t.Errorf("requests = %d", len(rec.all()))
	}
}
