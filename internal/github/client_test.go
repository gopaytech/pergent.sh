package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/forge"
)

const testToken = "github_pat_super-secret-token"

type recorded struct {
	Method string
	Path   string // escaped path
	Query  string
	Body   string
	Header http.Header
}

type recorder struct {
	mu   sync.Mutex
	reqs []recorded
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

func (r *recorder) get(i int) recorded {
	all := r.all()
	if i >= len(all) {
		panic("request " + strconv.Itoa(i) + " was not made")
	}
	return all[i]
}

// server starts an httptest server that records each request before handing
// it to h.
func server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, recorded{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(b), r.Header.Clone()})
		rec.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// serve returns a token client against a recording server, with sleeps skipped.
func serve(t *testing.T, h http.HandlerFunc) (*Client, *recorder) {
	t.Helper()
	srv, rec := server(t, h)
	c := NewTokenClient(srv.URL, testToken, srv.Client())
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c, rec
}

func jsonReply(v any) http.HandlerFunc {
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
}

// jsonEq compares a JSON request body with want, ignoring key order.
func jsonEq(t *testing.T, body string, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(body), &g); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("body = %s, want %s", body, want)
	}
}

var ctx = context.Background()

func TestHeaders(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{"id": 5, "login": "pergent-approver"}))
	u, err := c.CurrentUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != 5 || u.Login != "pergent-approver" {
		t.Fatalf("user = %+v", u)
	}
	got := rec.get(0)
	expect(t, got, "GET", "/user", "")
	for k, want := range map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-Github-Api-Version": "2022-11-28",
		"User-Agent":           "pergent-sh",
		"Authorization":        "Bearer " + testToken,
	} {
		if g := got.Header.Get(k); g != want {
			t.Errorf("%s = %q, want %q", k, g, want)
		}
	}
	if got.Header.Get("Content-Type") != "" {
		t.Errorf("Content-Type on a GET without a body = %q", got.Header.Get("Content-Type"))
	}
}

func TestBaseURLTrailingSlashAndDefault(t *testing.T) {
	srv, rec := server(t, jsonReply(map[string]any{"id": 5}))
	c := NewTokenClient(srv.URL+"/api/v3/", testToken, srv.Client())
	if _, err := c.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	expect(t, rec.get(0), "GET", "/api/v3/user", "")
	if d := NewTokenClient("", testToken, nil); d.base != "https://api.github.com" {
		t.Fatalf("default base = %q", d.base)
	}
}

func TestRepo(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{"id": 99, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git", "private": true, "default_branch": "main"}))
	r, err := c.Repo(ctx, "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	want := Repository{ID: 99, FullName: "acme/widgets", CloneURL: "https://github.com/acme/widgets.git", Private: true, DefaultBranch: "main"}
	if r != want {
		t.Fatalf("repo = %+v", r)
	}
	expect(t, rec.get(0), "GET", "/repos/acme/widgets", "")
}

func TestRepoNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"message":"Not Found"}`, 404) })
	_, err := c.Repo(ctx, "acme", "none")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// Owner and repo are single path segments: a "/" or a space is escaped, and
// a segment that would change the path ("", ".", "..") is refused unsent.
func TestPathSegmentsEscaped(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{"id": 1}))
	if _, err := c.Repo(ctx, "ac/me", "wid gets"); err != nil {
		t.Fatal(err)
	}
	expect(t, rec.get(0), "GET", "/repos/ac%2Fme/wid%20gets", "")
	for _, bad := range [][2]string{{"", "widgets"}, {"acme", ".."}, {".", "widgets"}} {
		if _, err := c.Repo(ctx, bad[0], bad[1]); err == nil {
			t.Errorf("Repo(%q, %q) succeeded", bad[0], bad[1])
		}
	}
	if _, _, err := c.File(ctx, "acme", "widgets", ".pergent/../secret", "abc"); err == nil {
		t.Error("File accepted a .. segment")
	}
	if n := len(rec.all()); n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
}

func TestPullRequest(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{
		"number": 3, "state": "closed", "merged": true, "draft": true,
		"head": map[string]any{"ref": "feature", "sha": "h1"},
		"base": map[string]any{"ref": "main", "sha": "b1"},
	}))
	pr, err := c.PullRequest(ctx, "acme", "widgets", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := PullRequest{Number: 3, State: "closed", Merged: true, Draft: true, Head: Ref{Ref: "feature", SHA: "h1"}, Base: Ref{Ref: "main", SHA: "b1"}}
	if pr != want {
		t.Fatalf("pr = %+v", pr)
	}
	expect(t, rec.get(0), "GET", "/repos/acme/widgets/pulls/3", "")
}

func TestCompare(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{"merge_base_commit": map[string]any{"sha": "mb"}, "status": "ahead"}))
	mb, err := c.Compare(ctx, "acme", "widgets", "b1", "h1")
	if err != nil || mb != "mb" {
		t.Fatalf("Compare = %q, %v", mb, err)
	}
	expect(t, rec.get(0), "GET", "/repos/acme/widgets/compare/b1...h1", "")
}

func TestCompareNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "{}", 404) })
	if _, err := c.Compare(ctx, "acme", "widgets", "b1", "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestFile(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "be kind\n") })
	b, ok, err := c.File(ctx, "acme", "widgets", ".pergent/instructions.md", sha)
	if err != nil || !ok || string(b) != "be kind\n" {
		t.Fatalf("File = %q, %v, %v", b, ok, err)
	}
	got := rec.get(0)
	expect(t, got, "GET", "/repos/acme/widgets/contents/.pergent/instructions.md", "ref="+sha)
	if a := got.Header.Get("Accept"); a != "application/vnd.github.raw+json" {
		t.Fatalf("Accept = %q", a)
	}
}

func TestFilePathSegmentsEscaped(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "x") })
	if _, _, err := c.File(ctx, "acme", "widgets", ".pergent/skills/a b#?.md", "feature/x"); err != nil {
		t.Fatal(err)
	}
	expect(t, rec.get(0), "GET", "/repos/acme/widgets/contents/.pergent/skills/a%20b%23%3F.md", "ref=feature%2Fx")
}

func TestFileNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"message":"Not Found"}`, 404) })
	b, ok, err := c.File(ctx, "acme", "widgets", ".pergent/instructions.md", "abc")
	if b != nil || ok || err != nil {
		t.Fatalf("File = %q, %v, %v", b, ok, err)
	}
}

// A path that is a directory: even with the raw media type GitHub answers
// with the JSON listing. That is not a file (as on GitLab).
func TestFileDirectoryIsNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, ` [{"name":"a.md","path":".pergent/skills/a.md","type":"file"}]`)
	})
	b, ok, err := c.File(ctx, "acme", "widgets", ".pergent/skills", "abc")
	if b != nil || ok || err != nil {
		t.Fatalf("File(directory) = %q, %v, %v; want nil, false, nil", b, ok, err)
	}
}

// A file whose content happens to start with '[' is still a file: the raw
// media type is not JSON.
func TestFileStartingWithBracket(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.github.raw+json")
		_, _ = io.WriteString(w, "[section]\n")
	})
	b, ok, err := c.File(ctx, "acme", "widgets", "a.ini", "abc")
	if string(b) != "[section]\n" || !ok || err != nil {
		t.Fatalf("File = %q, %v, %v", b, ok, err)
	}
}

func TestTree(t *testing.T) {
	c, rec := serve(t, jsonReply([]map[string]any{
		{"name": "a.md", "path": ".pergent/skills/a.md", "type": "file"},
		{"name": "b", "path": ".pergent/skills/b", "type": "dir"},
		{"name": "l.md", "path": ".pergent/skills/l.md", "type": "symlink"},
		{"name": "sub", "path": ".pergent/skills/sub", "type": "submodule"},
	}))
	got, err := c.Tree(ctx, "acme", "widgets", ".pergent/skills", "abc")
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.TreeEntry{
		{Name: "a.md", Path: ".pergent/skills/a.md", Type: "blob"},
		{Name: "b", Path: ".pergent/skills/b", Type: "tree"},
		{Name: "l.md", Path: ".pergent/skills/l.md", Type: "symlink"},
		{Name: "sub", Path: ".pergent/skills/sub", Type: "commit"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree = %+v", got)
	}
	r := rec.get(0)
	expect(t, r, "GET", "/repos/acme/widgets/contents/.pergent/skills", "ref=abc")
	if a := r.Header.Get("Accept"); a != "application/vnd.github+json" {
		t.Fatalf("Accept = %q", a)
	}
}

func TestTreeNotFoundOrNotADirectory(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "{}", 404) })
	got, err := c.Tree(ctx, "acme", "widgets", ".pergent/skills", "abc")
	if got != nil || err != nil {
		t.Fatalf("Tree = %v, %v", got, err)
	}
	// A file at the path is an object, not a listing: no directory there.
	c, _ = serve(t, jsonReply(map[string]any{"type": "file", "name": "skills", "path": ".pergent/skills"}))
	got, err = c.Tree(ctx, "acme", "widgets", ".pergent/skills", "abc")
	if got != nil || err != nil {
		t.Fatalf("Tree on a file = %v, %v", got, err)
	}
}

func TestIssueCommentsPaginates(t *testing.T) {
	var srvURL string
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+srvURL+`/repositories/99/issues/3/comments?per_page=100&page=2>; rel="next", <`+srvURL+`/repositories/99/issues/3/comments?per_page=100&page=2>; rel="last"`)
			_, _ = io.WriteString(w, `[{"id":1,"body":"a","user":{"id":7,"login":"x"}}]`)
			return
		}
		w.Header().Set("Link", `<`+srvURL+`/repositories/99/issues/3/comments?per_page=100&page=1>; rel="prev", <`+srvURL+`/repositories/99/issues/3/comments?per_page=100&page=1>; rel="first"`)
		_, _ = io.WriteString(w, `[{"id":2,"body":"<!-- pergent -->","user":{"id":8,"login":"y"}}]`)
	})
	srvURL = c.base
	got, err := c.IssueComments(ctx, "acme", "widgets", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []Comment{{ID: 1, Body: "a", User: User{ID: 7, Login: "x"}}, {ID: 2, Body: "<!-- pergent -->", User: User{ID: 8, Login: "y"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("comments = %+v", got)
	}
	all := rec.all()
	if len(all) != 2 {
		t.Fatalf("requests = %d", len(all))
	}
	expect(t, all[0], "GET", "/repos/acme/widgets/issues/3/comments", "per_page=100")
	expect(t, all[1], "GET", "/repositories/99/issues/3/comments", "per_page=100&page=2")
	if all[1].Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatal("page 2 sent without the token")
	}
}

// A next link to another host would carry the token there: refused.
func TestPaginationLinkToAnotherHostRefused(t *testing.T) {
	var leaked int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked++
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(other.Close)
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+other.URL+`/repos/acme/widgets/issues/3/comments?page=2>; rel="next"`)
		_, _ = io.WriteString(w, `[]`)
	})
	_, err := c.IssueComments(ctx, "acme", "widgets", 3)
	if err == nil || leaked != 0 {
		t.Fatalf("err = %v, other host requests = %d", err, leaked)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateIssueComment(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":44}`)
	})
	id, err := c.CreateIssueComment(ctx, "acme", "widgets", 3, "hello")
	if err != nil || id != 44 {
		t.Fatalf("CreateIssueComment = %d, %v", id, err)
	}
	got := rec.get(0)
	expect(t, got, "POST", "/repos/acme/widgets/issues/3/comments", "")
	jsonEq(t, got.Body, `{"body":"hello"}`)
	if got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", got.Header.Get("Content-Type"))
	}
}

func TestUpdateIssueComment(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{"id": 44}))
	if err := c.UpdateIssueComment(ctx, "acme", "widgets", 44, "new"); err != nil {
		t.Fatal(err)
	}
	got := rec.get(0)
	expect(t, got, "PATCH", "/repos/acme/widgets/issues/comments/44", "")
	jsonEq(t, got.Body, `{"body":"new"}`)
}

func TestUpdateIssueCommentNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "{}", 404) })
	if err := c.UpdateIssueComment(ctx, "acme", "widgets", 44, "new"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateCheckRun(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":555}`)
	})
	id, err := c.CreateCheckRun(ctx, "acme", "widgets", "pergent", "h1", "in_progress", "success", "pergent review running", "See the PR comment.")
	if err != nil || id != 555 {
		t.Fatalf("CreateCheckRun = %d, %v", id, err)
	}
	got := rec.get(0)
	expect(t, got, "POST", "/repos/acme/widgets/check-runs", "")
	// Not completed: the conclusion is left out even when given.
	jsonEq(t, got.Body, `{"name":"pergent","head_sha":"h1","status":"in_progress","output":{"title":"pergent review running","summary":"See the PR comment."}}`)

	if _, err := c.CreateCheckRun(ctx, "acme", "widgets", "pergent", "h1", "completed", "neutral", "No blocking issues", "See the PR comment."); err != nil {
		t.Fatal(err)
	}
	jsonEq(t, rec.get(1).Body, `{"name":"pergent","head_sha":"h1","status":"completed","conclusion":"neutral","output":{"title":"No blocking issues","summary":"See the PR comment."}}`)
}

func TestCreateReview(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{"id": 80}))
	id, err := c.CreateReview(ctx, "acme", "widgets", 3, "h1", "APPROVE")
	if err != nil || id != 80 {
		t.Fatalf("CreateReview = %d, %v", id, err)
	}
	got := rec.get(0)
	expect(t, got, "POST", "/repos/acme/widgets/pulls/3/reviews", "")
	jsonEq(t, got.Body, `{"commit_id":"h1","event":"APPROVE"}`)
}

func TestListReviewsPaginates(t *testing.T) {
	var base string
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+base+`/repos/acme/widgets/pulls/3/reviews?per_page=100&page=2>; rel="next"`)
			_, _ = io.WriteString(w, `[{"id":1,"user":{"id":7},"state":"APPROVED","commit_id":"h0"}]`)
			return
		}
		_, _ = io.WriteString(w, `[{"id":2,"user":{"id":7},"state":"DISMISSED","commit_id":"h1"}]`)
	})
	base = c.base
	got, err := c.ListReviews(ctx, "acme", "widgets", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []Review{{ID: 1, User: User{ID: 7}, State: "APPROVED", CommitID: "h0"}, {ID: 2, User: User{ID: 7}, State: "DISMISSED", CommitID: "h1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reviews = %+v", got)
	}
	expect(t, rec.get(0), "GET", "/repos/acme/widgets/pulls/3/reviews", "per_page=100")
	expect(t, rec.get(1), "GET", "/repos/acme/widgets/pulls/3/reviews", "per_page=100&page=2")
}

func TestDismissReview(t *testing.T) {
	c, rec := serve(t, jsonReply(map[string]any{"id": 80, "state": "DISMISSED"}))
	if err := c.DismissReview(ctx, "acme", "widgets", 3, 80, "pergent: the reviewed commit or target changed"); err != nil {
		t.Fatal(err)
	}
	got := rec.get(0)
	expect(t, got, "PUT", "/repos/acme/widgets/pulls/3/reviews/80/dismissals", "")
	jsonEq(t, got.Body, `{"message":"pergent: the reviewed commit or target changed","event":"DISMISS"}`)
}

func TestDismissReviewNotFound(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "{}", 404) })
	if err := c.DismissReview(ctx, "acme", "widgets", 3, 80, "m"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenExpiry(t *testing.T) {
	n := 0
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("GitHub-Authentication-Token-Expiration", "2026-11-10 08:00:00 UTC")
		} else {
			w.Header().Set("GitHub-Authentication-Token-Expiration", "2026-12-01 10:00:00 +0700")
		}
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	if _, ok := c.TokenExpiry(); ok {
		t.Fatal("expiry known before any response")
	}
	if _, err := c.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	exp, ok := c.TokenExpiry()
	if !ok || !exp.Equal(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("expiry = %v, %v", exp, ok)
	}
	if _, err := c.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	exp, _ = c.TokenExpiry()
	if !exp.Equal(time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("expiry = %v", exp)
	}
}

func TestRetryOn503ThenSuccess(t *testing.T) {
	n := 0
	var slept []time.Duration
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			http.Error(w, "unavailable", 503)
			return
		}
		_, _ = io.WriteString(w, `{"id":5}`)
	})
	c.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	u, err := c.CurrentUser(ctx)
	if err != nil || u.ID != 5 {
		t.Fatalf("u=%+v err=%v", u, err)
	}
	if len(rec.all()) != 3 {
		t.Fatalf("requests = %d", len(rec.all()))
	}
	if !reflect.DeepEqual(slept, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("slept = %v", slept)
	}
}

func TestRetryResendsBody(t *testing.T) {
	n := 0
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			http.Error(w, "x", 502)
			return
		}
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	if _, err := c.CreateIssueComment(ctx, "acme", "widgets", 3, "hi"); err != nil {
		t.Fatal(err)
	}
	jsonEq(t, rec.get(1).Body, `{"body":"hi"}`)
}

func TestRetryGivesUpAfterThree(t *testing.T) {
	var slept []time.Duration
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 500) })
	c.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	_, err := c.CurrentUser(ctx)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 {
		t.Fatalf("err = %v", err)
	}
	if len(rec.all()) != 4 {
		t.Fatalf("requests = %d", len(rec.all()))
	}
	if !reflect.DeepEqual(slept, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
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
			_, _ = io.WriteString(w, `{"id":5}`)
		}
	})
	c.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	if _, err := c.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(slept, []time.Duration{7 * time.Second, 30 * time.Second}) {
		t.Fatalf("slept = %v", slept)
	}
}

// GitHub's primary rate limit is a 403 (or 429) with X-RateLimit-Remaining: 0;
// the wait runs until X-RateLimit-Reset, capped at 30 s.
func TestRetryOnRateLimit403UntilReset(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	n := 0
	var slept []time.Duration
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		switch n {
		case 1:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(12*time.Second).Unix(), 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, 403)
		case 2:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, 429)
		default:
			_, _ = io.WriteString(w, `{"id":5}`)
		}
	})
	c.now = func() time.Time { return now }
	c.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	if _, err := c.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.all()) != 3 || !reflect.DeepEqual(slept, []time.Duration{12 * time.Second, 30 * time.Second}) {
		t.Fatalf("requests = %d, slept = %v", len(rec.all()), slept)
	}
}

// A 403 that isn't a rate limit is a permission answer: not retried.
func TestNoRetryOnPlain403Or400(t *testing.T) {
	for _, status := range []int{400, 403, 422} {
		c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "4999")
			http.Error(w, `{"message":"no"}`, status)
		})
		_, err := c.CurrentUser(ctx)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != status {
			t.Fatalf("err = %v", err)
		}
		if len(rec.all()) != 1 {
			t.Fatalf("%d: requests = %d", status, len(rec.all()))
		}
	}
}

func TestRetryOnConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u := srv.URL
	srv.Close()
	c := NewTokenClient(u, testToken, nil)
	calls := 0
	c.sleep = func(ctx context.Context, d time.Duration) error { calls++; return nil }
	_, err := c.CurrentUser(ctx)
	if err == nil || calls != 3 {
		t.Fatalf("err = %v, sleeps = %d", err, calls)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error contains token: %v", err)
	}
}

func TestRetryStopsOnContextCancel(t *testing.T) {
	cctx, cancel := context.WithCancel(ctx)
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) })
	c.sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	if _, err := c.CurrentUser(cctx); err == nil {
		t.Fatal("expected error")
	}
	if len(rec.all()) != 1 {
		t.Fatalf("requests = %d", len(rec.all()))
	}
}

// GitHub (or a proxy) echoing the token in an error body must not put it in
// the error, even where the stored body is cut at 4096 bytes.
func TestAPIErrorNeverContainsToken(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		pad := strings.Repeat("x", storedBody-len("echo: ")-len(tok)-len(tok)/2)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "echo: "+tok+pad+tok)
	})
	_, err := c.CurrentUser(ctx)
	if rec.get(0).Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatal("the server didn't see the token, so the test proves nothing")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %T %v, want *APIError", err, err)
	}
	half := testToken[:len(testToken)/2]
	for name, text := range map[string]string{"err.Error()": err.Error(), "APIError.Body": ae.Body} {
		if strings.Contains(text, testToken) || strings.Contains(text, half) {
			t.Errorf("%s contains (part of) the token", name)
		}
	}
	if !strings.HasPrefix(ae.Body, "echo: ***") {
		t.Errorf("body = %q, want the token masked", ae.Body[:40])
	}
}

// The token travels in a header, so a redirect to another host (or port) is
// refused and nothing reaches that host.
func TestRedirectToAnotherHostRefused(t *testing.T) {
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = append(leaked, r.Header.Get("Authorization"))
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
				http.Redirect(w, r, other.URL+"/user", http.StatusFound)
			}))
			t.Cleanup(srv.Close)
			hc := client.hc(srv)
			c := NewTokenClient(srv.URL, testToken, hc)
			slept := 0
			c.sleep = func(context.Context, time.Duration) error { slept++; return nil }
			_, err := c.CurrentUser(ctx)
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
				t.Error("NewTokenClient changed the caller's http.Client")
			}
		})
	}
}

func TestRedirectOnSameHostFollowed(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets" {
			http.Redirect(w, r, "/repositories/99", http.StatusMovedPermanently)
			return
		}
		_, _ = io.WriteString(w, `{"id":99}`)
	})
	r, err := c.Repo(ctx, "acme", "widgets")
	if err != nil || r.ID != 99 {
		t.Fatalf("Repo = %+v, %v", r, err)
	}
	if all := rec.all(); len(all) != 2 || all[1].Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatalf("requests = %d", len(all))
	}
}

func TestAPIErrorTruncatesBody(t *testing.T) {
	e := &APIError{Status: 500, Body: strings.Repeat("x", 1000)}
	if msg := e.Error(); !strings.Contains(msg, "500") || len(msg) > 300 {
		t.Fatalf("msg = %q", msg)
	}
}

func TestMalformedJSONErrorHasNoToken(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "not json "+testToken) })
	_, err := c.CurrentUser(ctx)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err = %v", err)
	}
}
