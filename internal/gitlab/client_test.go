package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "glpat-super-secret-token"

type recorded struct {
	Method string
	Path   string // escaped path
	Query  string
	Body   string
	Token  string
}

// serve starts a server; handler gets each request after it is recorded.
func serve(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*Client, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recorded{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(b), r.Header.Get("PRIVATE-TOKEN")})
		mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, testToken, srv.Client())
	c.sleep = func(ctx context.Context, d time.Duration) error { return nil }
	return c, &reqs
}

func jsonReply(v any) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func expect(t *testing.T, got recorded, method, path, query string) {
	t.Helper()
	if got.Method != method || got.Path != path || got.Query != query {
		t.Fatalf("request = %s %s ?%s, want %s %s ?%s", got.Method, got.Path, got.Query, method, path, query)
	}
	if got.Token != testToken {
		t.Fatalf("PRIVATE-TOKEN = %q", got.Token)
	}
}

func TestCurrentUser(t *testing.T) {
	c, reqs := serve(t, jsonReply(map[string]any{"id": 5, "username": "pergent-bot"}))
	u, err := c.CurrentUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != 5 || u.Username != "pergent-bot" {
		t.Fatalf("user = %+v", u)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/user", "")
}

func TestBaseURLTrailingSlash(t *testing.T) {
	c, reqs := serve(t, jsonReply(map[string]any{"id": 5}))
	c2 := New(c.baseURL+"/", testToken, nil)
	c2.sleep = c.sleep
	c = c2
	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/user", "")
}

func TestGetProject(t *testing.T) {
	c, reqs := serve(t, jsonReply(map[string]any{"id": 7, "path_with_namespace": "acme/widgets", "http_url_to_repo": "https://gitlab.example.com/acme/widgets.git"}))
	p, err := c.GetProject(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != 7 || p.PathWithNamespace != "acme/widgets" || p.HTTPURLToRepo != "https://gitlab.example.com/acme/widgets.git" {
		t.Fatalf("project = %+v", p)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/acme%2Fwidgets", "")
}

func TestGetProjectNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"404 Project Not Found"}`, 404)
	})
	_, err := c.GetProject(context.Background(), "acme/none")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetMergeRequest(t *testing.T) {
	c, reqs := serve(t, jsonReply(map[string]any{
		"iid": 3, "state": "opened", "sha": "abc", "target_branch": "main",
		"diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"},
	}))
	mr, err := c.GetMergeRequest(context.Background(), 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := MergeRequest{IID: 3, State: "opened", SHA: "abc", TargetBranch: "main", DiffRefs: DiffRefs{BaseSHA: "b", HeadSHA: "h", StartSHA: "s"}}
	if mr != want {
		t.Fatalf("mr = %+v", mr)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/7/merge_requests/3", "")
}

// GitLab versions whose merge request API reports work_in_progress instead of
// draft: either flag makes a draft, so a draft is never read as ready.
func TestGetMergeRequestDraftFlags(t *testing.T) {
	for name, tc := range map[string]struct {
		fields map[string]any
		want   bool
	}{
		"neither":               {map[string]any{}, false},
		"draft":                 {map[string]any{"draft": true}, true},
		"work_in_progress":      {map[string]any{"work_in_progress": true}, true},
		"both":                  {map[string]any{"draft": true, "work_in_progress": true}, true},
		"both false":            {map[string]any{"draft": false, "work_in_progress": false}, false},
		"draft false, wip true": {map[string]any{"draft": false, "work_in_progress": true}, true},
		"draft true, wip false": {map[string]any{"draft": true, "work_in_progress": false}, true},
	} {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"iid": 3, "state": "opened", "sha": "abc"}
			for k, v := range tc.fields {
				body[k] = v
			}
			c, _ := serve(t, jsonReply(body))
			mr, err := c.GetMergeRequest(context.Background(), 7, 3)
			if err != nil {
				t.Fatal(err)
			}
			if mr.Draft != tc.want {
				t.Errorf("Draft = %v, want %v", mr.Draft, tc.want)
			}
		})
	}
}

func TestGetFile(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("be careful\n")) })
	b, ok, err := c.GetFile(context.Background(), 7, ".pergent/instructions.md", "deadbeef")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if string(b) != "be careful\n" {
		t.Fatalf("body = %q", b)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/7/repository/files/.pergent%2Finstructions.md/raw", "ref=deadbeef")
}

func TestGetFileNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 404) })
	b, ok, err := c.GetFile(context.Background(), 7, "AGENTS.md", "x")
	if err != nil || ok || b != nil {
		t.Fatalf("b=%v ok=%v err=%v", b, ok, err)
	}
}

func TestListTreePaginates(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "b.md", "path": ".pergent/skills/b.md", "type": "blob"}})
			return
		}
		w.Header().Set("X-Next-Page", "2")
		_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "a.md", "path": ".pergent/skills/a.md", "type": "blob"}, {"name": "dir", "path": ".pergent/skills/dir", "type": "tree"}})
	})
	got, err := c.ListTree(context.Background(), 7, ".pergent/skills", "sha1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "a.md" || got[1].Type != "tree" || got[2].Path != ".pergent/skills/b.md" {
		t.Fatalf("entries = %+v", got)
	}
	if len(*reqs) != 2 {
		t.Fatalf("requests = %d", len(*reqs))
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/7/repository/tree", "path=.pergent%2Fskills&per_page=100&ref=sha1")
	expect(t, (*reqs)[1], "GET", "/api/v4/projects/7/repository/tree", "page=2&path=.pergent%2Fskills&per_page=100&ref=sha1")
}

func TestListTreeNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 404) })
	got, err := c.ListTree(context.Background(), 7, ".pergent/skills", "sha1")
	if err != nil || got != nil {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

func TestFindNoteMatchesAuthorAndMarker(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 40, "body": "<!-- pergent --> review", "author": map[string]any{"id": 5}},
			})
			return
		}
		w.Header().Set("X-Next-Page", "2")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 10, "body": "<!-- pergent --> impostor", "author": map[string]any{"id": 99}},
			{"id": 20, "body": "hello", "author": map[string]any{"id": 5}},
		})
	})
	id, ok, err := c.FindNote(context.Background(), 7, 3, 5, "<!-- pergent -->")
	if err != nil || !ok || id != 40 {
		t.Fatalf("id=%d ok=%v err=%v", id, ok, err)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/7/merge_requests/3/notes", "per_page=100")
	expect(t, (*reqs)[1], "GET", "/api/v4/projects/7/merge_requests/3/notes", "page=2&per_page=100")
}

func TestFindNoteNone(t *testing.T) {
	c, _ := serve(t, jsonReply([]map[string]any{{"id": 10, "body": "<!-- pergent -->", "author": map[string]any{"id": 99}}}))
	id, ok, err := c.FindNote(context.Background(), 7, 3, 5, "<!-- pergent -->")
	if err != nil || ok || id != 0 {
		t.Fatalf("id=%d ok=%v err=%v", id, ok, err)
	}
}

func TestCreateNote(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":123}`))
	})
	id, err := c.CreateNote(context.Background(), 7, 3, "hi")
	if err != nil || id != 123 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	expect(t, (*reqs)[0], "POST", "/api/v4/projects/7/merge_requests/3/notes", "")
	if (*reqs)[0].Body != `{"body":"hi"}` {
		t.Fatalf("body = %s", (*reqs)[0].Body)
	}
}

func TestUpdateNote(t *testing.T) {
	c, reqs := serve(t, jsonReply(map[string]any{"id": 123}))
	if err := c.UpdateNote(context.Background(), 7, 3, 123, "new"); err != nil {
		t.Fatal(err)
	}
	expect(t, (*reqs)[0], "PUT", "/api/v4/projects/7/merge_requests/3/notes/123", "")
	if (*reqs)[0].Body != `{"body":"new"}` {
		t.Fatalf("body = %s", (*reqs)[0].Body)
	}
}

func TestUpdateNoteNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "gone", 404) })
	if err := c.UpdateNote(context.Background(), 7, 3, 123, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestSetCommitStatus(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); _, _ = w.Write([]byte(`{}`)) })
	if err := c.SetCommitStatus(context.Background(), 7, "abc123", StatusSuccess, "all good"); err != nil {
		t.Fatal(err)
	}
	expect(t, (*reqs)[0], "POST", "/api/v4/projects/7/statuses/abc123", "")
	var body map[string]string
	if err := json.Unmarshal([]byte((*reqs)[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["state"] != "success" || body["name"] != "pergent" || body["description"] != "all good" {
		t.Fatalf("body = %v", body)
	}
}

func TestSetCommitStatusTruncatesOnRuneBoundary(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); _, _ = w.Write([]byte(`{}`)) })
	// 254 ASCII bytes then a 3-byte rune: byte 255 would split the rune.
	desc := strings.Repeat("a", 254) + "€€"
	if err := c.SetCommitStatus(context.Background(), 7, "abc", StatusFailed, desc); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	_ = json.Unmarshal([]byte((*reqs)[0].Body), &body)
	got := body["description"]
	if got != strings.Repeat("a", 254) {
		t.Fatalf("len=%d desc=%q", len(got), got)
	}
}

func TestApprove(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); _, _ = w.Write([]byte(`{}`)) })
	if err := c.Approve(context.Background(), 7, 3, "abc"); err != nil {
		t.Fatal(err)
	}
	expect(t, (*reqs)[0], "POST", "/api/v4/projects/7/merge_requests/3/approve", "")
	if (*reqs)[0].Body != `{"sha":"abc"}` {
		t.Fatalf("body = %s", (*reqs)[0].Body)
	}
}

func TestApproveHeadMoved(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"SHA does not match HEAD of source branch"}`, 409)
	})
	if err := c.Approve(context.Background(), 7, 3, "old"); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("err = %v", err)
	}
}

func TestApprovedBy(t *testing.T) {
	c, reqs := serve(t, jsonReply(map[string]any{
		"approved": true,
		"approved_by": []map[string]any{
			{"user": map[string]any{"id": 200, "username": "pergent-approve"}},
			{"user": map[string]any{"id": 31, "username": "alice"}},
		},
	}))
	ids, err := c.ApprovedBy(context.Background(), 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/7/merge_requests/3/approvals", "")
	if len(ids) != 2 || ids[0] != 200 || ids[1] != 31 {
		t.Fatalf("ids = %v, want [200 31]", ids)
	}
}

func TestApprovedByNone(t *testing.T) {
	c, _ := serve(t, jsonReply(map[string]any{"approved_by": []any{}}))
	ids, err := c.ApprovedBy(context.Background(), 7, 3)
	if err != nil || len(ids) != 0 {
		t.Fatalf("ids = %v, %v; want none", ids, err)
	}
}

func TestApprovedByError(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"message":"403 Forbidden"}`, 403) })
	if _, err := c.ApprovedBy(context.Background(), 7, 3); err == nil {
		t.Fatal("want an error for a 403")
	}
}

func TestUnapprove(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); _, _ = w.Write([]byte(`{}`)) })
	if err := c.Unapprove(context.Background(), 7, 3); err != nil {
		t.Fatal(err)
	}
	expect(t, (*reqs)[0], "POST", "/api/v4/projects/7/merge_requests/3/unapprove", "")
}

func TestUnapproveNotApproved(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "404 Not Found", 404) })
	if err := c.Unapprove(context.Background(), 7, 3); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func hookBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEnsureHookCreates(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`[{"id":1,"url":"https://other.example.com/x"}]`))
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":55,"url":"https://pergent.example.com/webhook"}`))
	})
	id, err := c.EnsureHook(context.Background(), 7, "https://pergent.example.com/webhook", "s3cret")
	if err != nil || id != 55 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/7/hooks", "per_page=100")
	expect(t, (*reqs)[1], "POST", "/api/v4/projects/7/hooks", "")
	b := hookBody(t, (*reqs)[1].Body)
	if b["url"] != "https://pergent.example.com/webhook" || b["token"] != "s3cret" ||
		b["merge_requests_events"] != true || b["push_events"] != false || b["enable_ssl_verification"] != true {
		t.Fatalf("body = %v", b)
	}
}

func TestEnsureHookUpdatesExisting(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`[{"id":1,"url":"https://other.example.com/x"},{"id":9,"url":"https://pergent.example.com/webhook"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"id":9}`))
	})
	id, err := c.EnsureHook(context.Background(), 7, "https://pergent.example.com/webhook", "s3cret")
	if err != nil || id != 9 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("requests = %d", len(*reqs))
	}
	expect(t, (*reqs)[1], "PUT", "/api/v4/projects/7/hooks/9", "")
	b := hookBody(t, (*reqs)[1].Body)
	if b["token"] != "s3cret" || b["merge_requests_events"] != true || b["push_events"] != false || b["enable_ssl_verification"] != true {
		t.Fatalf("body = %v", b)
	}
}

func TestDeleteHook(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	if err := c.DeleteHook(context.Background(), 7, 9); err != nil {
		t.Fatal(err)
	}
	expect(t, (*reqs)[0], "DELETE", "/api/v4/projects/7/hooks/9", "")
}

func TestDeleteHookNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 404) })
	if err := c.DeleteHook(context.Background(), 7, 9); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateDeployToken(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":31,"username":"gitlab+deploy-token-31","token":"gldt-abc","expires_at":"2026-11-29T12:00:00.000Z"}`))
	})
	exp := time.Date(2026, 11, 29, 12, 0, 0, 0, time.UTC)
	dt, err := c.CreateDeployToken(context.Background(), 7, "pergent", exp)
	if err != nil {
		t.Fatal(err)
	}
	if dt.ID != 31 || dt.Username != "gitlab+deploy-token-31" || dt.Token != "gldt-abc" || !dt.ExpiresAt.Equal(exp) {
		t.Fatalf("token = %+v", dt)
	}
	expect(t, (*reqs)[0], "POST", "/api/v4/projects/7/deploy_tokens", "")
	b := hookBody(t, (*reqs)[0].Body)
	scopes, _ := b["scopes"].([]any)
	if b["name"] != "pergent" || b["expires_at"] != "2026-11-29T12:00:00Z" || len(scopes) != 1 || scopes[0] != "read_repository" {
		t.Fatalf("body = %v", b)
	}
}

func TestDeleteDeployToken(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	if err := c.DeleteDeployToken(context.Background(), 7, 31); err != nil {
		t.Fatal(err)
	}
	expect(t, (*reqs)[0], "DELETE", "/api/v4/projects/7/deploy_tokens/31", "")
}

func TestDeleteDeployTokenNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 404) })
	if err := c.DeleteDeployToken(context.Background(), 7, 31); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestRetryOn503ThenSuccess(t *testing.T) {
	n := 0
	var slept []time.Duration
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			http.Error(w, "unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":5,"username":"bot"}`))
	})
	c.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	u, err := c.CurrentUser(context.Background())
	if err != nil || u.ID != 5 {
		t.Fatalf("u=%+v err=%v", u, err)
	}
	if len(*reqs) != 3 {
		t.Fatalf("requests = %d", len(*reqs))
	}
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 2*time.Second {
		t.Fatalf("slept = %v", slept)
	}
}

func TestRetryResendsBody(t *testing.T) {
	n := 0
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			http.Error(w, "x", 502)
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	if _, err := c.CreateNote(context.Background(), 7, 3, "hi"); err != nil {
		t.Fatal(err)
	}
	if (*reqs)[1].Body != `{"body":"hi"}` {
		t.Fatalf("retry body = %q", (*reqs)[1].Body)
	}
}

func TestRetryGivesUpAfterThree(t *testing.T) {
	var slept []time.Duration
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 500) })
	c.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	_, err := c.CurrentUser(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 {
		t.Fatalf("err = %v", err)
	}
	if len(*reqs) != 4 {
		t.Fatalf("requests = %d", len(*reqs))
	}
	if len(slept) != 3 || slept[0] != time.Second || slept[1] != 2*time.Second || slept[2] != 4*time.Second {
		t.Fatalf("slept = %v", slept)
	}
}

func TestRetryAfterHonoredAndCapped(t *testing.T) {
	n := 0
	var slept []time.Duration
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		switch n {
		case 1:
			w.Header().Set("Retry-After", "7")
			http.Error(w, "slow down", 429)
		case 2:
			w.Header().Set("Retry-After", "600")
			http.Error(w, "slow down", 429)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":5}`))
		}
	})
	c.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 2 || slept[0] != 7*time.Second || slept[1] != 30*time.Second {
		t.Fatalf("slept = %v", slept)
	}
}

func TestRetryOnConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused
	c := New(url, testToken, nil)
	calls := 0
	c.sleep = func(ctx context.Context, d time.Duration) error { calls++; return nil }
	_, err := c.CurrentUser(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 3 {
		t.Fatalf("sleeps = %d", calls)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error contains token: %v", err)
	}
}

func TestNoRetryOn400(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"message":"bad"}`, 400) })
	_, err := c.CurrentUser(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("err = %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %d", len(*reqs))
	}
}

func TestRetryStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) })
	c.sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	_, err := c.CurrentUser(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %d", len(*reqs))
	}
}

// GitLab (or a proxy in front of it) echoing the token in an error body must
// not put it in the error, even where the stored body is cut at 4096 bytes.
func TestAPIErrorNeverContainsToken(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("PRIVATE-TOKEN")
		// The second copy straddles the 4096-byte cut.
		pad := strings.Repeat("x", storedBody-len("echo: ")-len(tok)-len(tok)/2)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "echo: "+tok+pad+tok)
	})
	_, err := c.CurrentUser(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if len(*reqs) == 0 || (*reqs)[0].Token != testToken {
		t.Fatal("the server didn't see the token, so the test proves nothing")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %T %v, want *APIError", err, err)
	}
	half := testToken[:len(testToken)/2]
	for name, text := range map[string]string{"err.Error()": err.Error(), "APIError.Body": ae.Body} {
		if strings.Contains(text, testToken) || strings.Contains(text, half) {
			t.Errorf("%s contains (part of) the token, ending %q", name, text[max(0, len(text)-40):])
		}
	}
	if !strings.Contains(ae.Body, "echo: ***") {
		t.Errorf("body = %q, want the token masked", ae.Body[:40])
	}
}

// The token travels in a header Go forwards on every redirect, so a redirect
// to another host (or port) is refused and nothing reaches that host.
func TestRedirectToAnotherHostRefused(t *testing.T) {
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = append(leaked, r.Header.Get("PRIVATE-TOKEN"))
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	t.Cleanup(other.Close)
	for _, client := range []struct {
		name string
		hc   func(srv *httptest.Server) *http.Client
	}{
		{"given client", func(srv *httptest.Server) *http.Client { return srv.Client() }},
		{"default client", func(*httptest.Server) *http.Client { return nil }},
	} {
		t.Run(client.name, func(t *testing.T) {
			leaked = nil
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+"/api/v4/user", http.StatusFound)
			}))
			t.Cleanup(srv.Close)
			hc := client.hc(srv)
			c := New(srv.URL, testToken, hc)
			slept := 0
			c.sleep = func(context.Context, time.Duration) error { slept++; return nil }
			_, err := c.CurrentUser(context.Background())
			if err == nil {
				t.Fatal("followed a redirect to another host")
			}
			if len(leaked) != 0 {
				t.Fatalf("the other host got %d requests", len(leaked))
			}
			if strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "redirect") {
				t.Errorf("err = %v", err)
			}
			if slept != 0 {
				t.Errorf("retried a refused redirect %d times", slept)
			}
			if hc != nil && hc.CheckRedirect != nil {
				t.Error("New changed the caller's http.Client")
			}
		})
	}
}

// A redirect on the same host is followed, with the token.
func TestRedirectOnSameHostFollowed(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/user" {
			http.Redirect(w, r, "/api/v4/user/moved", http.StatusMovedPermanently)
			return
		}
		jsonReply(map[string]any{"id": 5, "username": "bot"})(w, r)
	})
	u, err := c.CurrentUser(context.Background())
	if err != nil || u.ID != 5 {
		t.Fatalf("CurrentUser = %+v, %v", u, err)
	}
	if len(*reqs) != 2 || (*reqs)[1].Token != testToken {
		t.Fatalf("requests = %+v", *reqs)
	}
}

func TestAPIErrorTruncatesBody(t *testing.T) {
	e := &APIError{Status: 500, Body: strings.Repeat("x", 1000)}
	msg := e.Error()
	if !strings.Contains(msg, "500") {
		t.Fatalf("msg = %s", msg)
	}
	if len(msg) > 300 {
		t.Fatalf("message too long: %d", len(msg))
	}
}

func TestMalformedJSONErrorHasNoToken(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) })
	_, err := c.CurrentUser(context.Background())
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestApprovalRules(t *testing.T) {
	c, reqs := serve(t, jsonReply([]map[string]any{{
		"rule_type": "regular", "approvals_required": 2, "applies_to_all_protected_branches": false,
		"protected_branches": []map[string]any{{"name": "main"}},
	}}))
	rules, err := c.ApprovalRules(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, (*reqs)[0], http.MethodGet, "/api/v4/projects/7/approval_rules", "per_page=100")
	if len(rules) != 1 || rules[0].RuleType != "regular" || rules[0].ApprovalsRequired != 2 ||
		len(rules[0].ProtectedBranches) != 1 || rules[0].ProtectedBranches[0].Name != "main" {
		t.Errorf("rules = %+v", rules)
	}
}

func TestApprovalsBeforeMerge(t *testing.T) {
	c, reqs := serve(t, jsonReply(map[string]any{"approvals_before_merge": 2}))
	n, err := c.ApprovalsBeforeMerge(context.Background(), 7)
	if err != nil || n != 2 {
		t.Fatalf("ApprovalsBeforeMerge = %d, %v", n, err)
	}
	expect(t, (*reqs)[0], http.MethodGet, "/api/v4/projects/7/approvals", "")
}

// A user's OAuth token travels as a Bearer token, never as PRIVATE-TOKEN.
func TestNewOAuthSendsBearer(t *testing.T) {
	var auth, private string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, private = r.Header.Get("Authorization"), r.Header.Get("PRIVATE-TOKEN")
		jsonReply(map[string]any{"id": 9, "username": "alice"})(w, r)
	}))
	t.Cleanup(srv.Close)
	c := NewOAuth(srv.URL, "oauth-user-token", srv.Client())
	u, err := c.CurrentUser(context.Background())
	if err != nil || u.ID != 9 || u.Username != "alice" {
		t.Fatalf("CurrentUser = %+v, %v", u, err)
	}
	if auth != "Bearer oauth-user-token" || private != "" {
		t.Fatalf("Authorization = %q, PRIVATE-TOKEN = %q", auth, private)
	}
}

func TestProjectAccess(t *testing.T) {
	c, reqs := serve(t, jsonReply(map[string]any{
		"id": 7, "path_with_namespace": "acme/widgets", "visibility": "private",
		"repository_access_level": "enabled", "merge_requests_access_level": "private",
		"permissions": map[string]any{
			"project_access": map[string]any{"access_level": 10},
			"group_access":   map[string]any{"access_level": 30},
		},
	}))
	a, err := c.ProjectAccess(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	want := ProjectAccess{ID: 7, Path: "acme/widgets", Visibility: "private", RepositoryAccess: "enabled", MergeRequestsAccess: "private",
		ProjectAccessLevel: 10, GroupAccessLevel: 30}
	if a != want {
		t.Fatalf("ProjectAccess = %+v, want %+v", a, want)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/acme%2Fwidgets", "")

	// No membership: GitLab sends null permissions.
	c, _ = serve(t, jsonReply(map[string]any{"visibility": "public",
		"permissions": map[string]any{"project_access": nil, "group_access": nil}}))
	if a, err := c.ProjectAccess(context.Background(), "acme/widgets"); err != nil || a.ProjectAccessLevel != 0 || a.GroupAccessLevel != 0 {
		t.Fatalf("ProjectAccess without membership = %+v, %v", a, err)
	}
}

// HasMemberAccess asks the project listing, which counts shares, narrowed
// to the one project ID.
func TestHasMemberAccess(t *testing.T) {
	c, reqs := serve(t, jsonReply([]map[string]any{{"id": 7, "path_with_namespace": "acme/widgets"}}))
	ok, err := c.HasMemberAccess(context.Background(), 7, 20)
	if err != nil || !ok {
		t.Fatalf("HasMemberAccess = %v, %v; want true", ok, err)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects", "id_after=6&id_before=8&min_access_level=20&simple=true")

	// Another project in the answer is not this one.
	c, _ = serve(t, jsonReply([]map[string]any{{"id": 8, "path_with_namespace": "acme/other"}}))
	if ok, err := c.HasMemberAccess(context.Background(), 7, 20); err != nil || ok {
		t.Fatalf("HasMemberAccess with another project = %v, %v; want false", ok, err)
	}
	c, _ = serve(t, jsonReply([]map[string]any{}))
	if ok, err := c.HasMemberAccess(context.Background(), 7, 20); err != nil || ok {
		t.Fatalf("HasMemberAccess with none = %v, %v; want false", ok, err)
	}
}

func TestProjectAccessNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"404 Project Not Found"}`, 404)
	})
	if _, err := c.ProjectAccess(context.Background(), "acme/widgets"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestProjectPathsPaginatesWithMinAccessLevel(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 3, "path_with_namespace": "acme/payments"}})
			return
		}
		w.Header().Set("X-Next-Page", "2")
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "path_with_namespace": "acme/widgets"}, {"id": 2}})
	})
	got, truncated, err := c.ProjectPaths(context.Background(), ProjectListOptions{MinAccessLevel: 40, Limit: 10})
	if err != nil || truncated {
		t.Fatalf("truncated=%v err=%v", truncated, err)
	}
	if strings.Join(got, ",") != "acme/widgets,acme/payments" {
		t.Fatalf("paths = %v", got)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects", "min_access_level=40&per_page=100&simple=true")
	expect(t, (*reqs)[1], "GET", "/api/v4/projects", "min_access_level=40&page=2&per_page=100&simple=true")
}

func TestProjectPathsSearch(t *testing.T) {
	c, reqs := serve(t, jsonReply([]map[string]any{{"path_with_namespace": "acme/widgets"}}))
	if _, _, err := c.ProjectPaths(context.Background(), ProjectListOptions{MinAccessLevel: 20, Search: "wid gets", Limit: 10}); err != nil {
		t.Fatal(err)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects",
		"min_access_level=20&per_page=100&search=wid+gets&search_namespaces=true&simple=true")
}

func TestProjectPathsStopsAtLimit(t *testing.T) {
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Next-Page", "2")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"path_with_namespace": "acme/a"}, {"path_with_namespace": "acme/b"}, {"path_with_namespace": "acme/c"}})
	})
	got, truncated, err := c.ProjectPaths(context.Background(), ProjectListOptions{MinAccessLevel: 20, Limit: 2})
	if err != nil || !truncated || len(got) != 2 {
		t.Fatalf("got=%v truncated=%v err=%v", got, truncated, err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(*reqs))
	}
}

// Each page has its own deadline; a failed page returns the pages read
// before it with the error.
func TestProjectPathsPageTimeoutKeepsWhatWasRead(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("X-Next-Page", "2")
		_ = json.NewEncoder(w).Encode([]map[string]any{{"path_with_namespace": "acme/a"}})
	})
	start := time.Now()
	got, _, err := c.ProjectPaths(context.Background(), ProjectListOptions{MinAccessLevel: 20, Limit: 10,
		CallTimeout: 100 * time.Millisecond})
	if err == nil || strings.Join(got, ",") != "acme/a" {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}
