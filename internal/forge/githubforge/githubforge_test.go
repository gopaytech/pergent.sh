package githubforge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/github"
)

// None of these is a real secret.
const (
	repoID         = int64(900)
	installationID = int64(55)
	reviewBotID    = int64(4001)
	approveBotID   = int64(4002)
	installToken   = "ghs_installation-token-example"
	patToken       = "github_pat_approve-token-example"
)

var (
	keyOnce sync.Once
	keyPEM  string
)

// appKey is one RSA key for every test App (generating one is slow).
func appKey(t *testing.T) string {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	})
	return keyPEM
}

type request struct {
	Method, Path, Query, Body, Auth string
}

// fakeGitHub routes on "METHOD /escaped/path" and records every request. An
// unrouted request is 404. Every App's installation token request succeeds.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	reqs   []request
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, request{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(b), r.Header.Get("Authorization")})
		h := f.routes[r.Method+" "+r.URL.EscapedPath()]
		f.mu.Unlock()
		if h == nil && r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") {
			h = reply(http.StatusCreated, map[string]any{"token": installToken, "expires_at": time.Now().Add(time.Hour)})
		}
		if h == nil {
			h = reply(http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func reply(status int, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *fakeGitHub) handle(method, path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+path] = h
}

func (f *fakeGitHub) reply(method, path string, status int, v any) {
	f.handle(method, path, reply(status, v))
}

// requests returns the recorded requests for method and path, without the
// installation token requests.
func (f *fakeGitHub) requests(method, path string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.reqs {
		if (method == "" || r.Method == method) && (path == "" || r.Path == path) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGitHub) app(t *testing.T) *github.App {
	t.Helper()
	a, err := github.NewApp(f.srv.URL, 123, appKey(t), f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// repos knows acme/widgets as repoID, installed as installationID.
func repos() ReposFunc {
	return func(_ context.Context, id int64) (Repo, error) {
		switch id {
		case repoID:
			return Repo{Owner: "acme", Name: "widgets", InstallationID: installationID}, nil
		case 901:
			return Repo{Owner: "acme", Name: "gadgets"}, nil // review App not installed
		}
		return Repo{}, ErrUnknownRepo
	}
}

func (f *fakeGitHub) forge(t *testing.T) *Forge {
	return NewForge(f.app(t), reviewBotID, repos())
}

const prPath = "/repos/acme/widgets/pulls/3"

func sha(c byte) string { return strings.Repeat(string(c), 40) }

func pr(state string, merged, draft bool) map[string]any {
	return map[string]any{
		"number": 3, "state": state, "merged": merged, "draft": draft,
		"head": map[string]any{"ref": "feature", "sha": sha('a')},
		"base": map[string]any{"ref": "main", "sha": sha('5')},
	}
}

var ctx = context.Background()

func TestGetChangeOpen(t *testing.T) {
	f := newFake(t)
	f.reply("GET", prPath, 200, pr("open", false, true))
	f.reply("GET", "/repos/acme/widgets/compare/"+sha('5')+"..."+sha('a'), 200,
		map[string]any{"merge_base_commit": map[string]any{"sha": sha('b')}})

	got, err := f.forge(t).GetChange(ctx, repoID, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := forge.Change{Number: 3, State: forge.StateOpen, Draft: true, HeadSHA: sha('a'),
		TargetBranch: "main", BaseSHA: sha('b'), StartSHA: sha('5')}
	if got != want {
		t.Fatalf("GetChange = %+v\nwant %+v", got, want)
	}
	if reqs := f.requests("GET", prPath); len(reqs) != 1 || reqs[0].Auth != "Bearer "+installToken {
		t.Errorf("pull request read = %+v; want one with the installation token", reqs)
	}
}

func TestGetChangeStates(t *testing.T) {
	for _, tc := range []struct {
		state  string
		merged bool
		want   forge.ChangeState
	}{
		{"closed", true, forge.StateMerged},
		{"closed", false, forge.StateClosed},
	} {
		f := newFake(t)
		f.reply("GET", prPath, 200, pr(tc.state, tc.merged, false))
		got, err := f.forge(t).GetChange(ctx, repoID, 3)
		if err != nil || got.State != tc.want {
			t.Errorf("state %q merged %v: GetChange = %+v, %v; want %s", tc.state, tc.merged, got, err, tc.want)
		}
		// A change that isn't open is never dispatched: no compare.
		if n := len(f.requests("GET", "")); n != 1 { // the installation token is a POST
			t.Errorf("state %q: %d GET requests, want the pull request only", tc.state, n)
		}
	}
}

// A compare GitHub can't answer (404: a head or base it doesn't have yet;
// 422: no common history) leaves the change not ready, not an error.
func TestGetChangeCompareUnavailableIsNotReady(t *testing.T) {
	for _, status := range []int{404, 422} {
		f := newFake(t)
		f.reply("GET", prPath, 200, pr("open", false, false))
		f.reply("GET", "/repos/acme/widgets/compare/"+sha('5')+"..."+sha('a'), status, map[string]string{"message": "nope"})
		got, err := f.forge(t).GetChange(ctx, repoID, 3)
		if err != nil {
			t.Fatalf("compare %d: error %v", status, err)
		}
		if got.BaseSHA != "" || got.HeadSHA != sha('a') || got.StartSHA != sha('5') {
			t.Errorf("compare %d: GetChange = %+v; want an empty BaseSHA", status, got)
		}
	}
}

func TestGetChangeCompareErrorIsError(t *testing.T) {
	f := newFake(t)
	f.reply("GET", prPath, 200, pr("open", false, false))
	f.reply("GET", "/repos/acme/widgets/compare/"+sha('5')+"..."+sha('a'), 403, map[string]string{"message": "forbidden"})
	if _, err := f.forge(t).GetChange(ctx, repoID, 3); err == nil {
		t.Fatal("GetChange with a refused compare = nil error")
	}
}

func TestGetChangeNotFound(t *testing.T) {
	f := newFake(t)
	_, err := f.forge(t).GetChange(ctx, repoID, 3)
	if !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("GetChange of a missing pull request = %v; want forge.ErrNotFound", err)
	}
}

// A repository the review App isn't installed on, or one pergent.sh doesn't
// know, can't be read; neither is "not found" (which would forget an
// approval).
func TestRepoLookupErrors(t *testing.T) {
	f := newFake(t)
	fg := f.forge(t)
	if _, err := fg.GetChange(ctx, 901, 3); !errors.Is(err, ErrNotInstalled) || errors.Is(err, forge.ErrNotFound) {
		t.Errorf("not installed: %v", err)
	}
	if _, err := fg.GetChange(ctx, 999, 3); !errors.Is(err, ErrUnknownRepo) || errors.Is(err, forge.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if n := len(f.requests("", "")); n != 0 {
		t.Errorf("%d requests made for repositories that can't be reached", n)
	}
}

func TestSetStatusCreatesCheckRun(t *testing.T) {
	for _, tc := range []struct {
		state forge.StatusState
		want  string
	}{
		{forge.StatusRunning, `{"name":"pergent","head_sha":"` + sha('a') + `","status":"in_progress","output":{"title":"pergent review running","summary":"See the pergent comment on this pull request."}}`},
		{forge.StatusSuccess, `{"name":"pergent","head_sha":"` + sha('a') + `","status":"completed","conclusion":"success","output":{"title":"desc","summary":"See the pergent comment on this pull request."}}`},
		{forge.StatusFailed, `{"name":"pergent","head_sha":"` + sha('a') + `","status":"completed","conclusion":"failure","output":{"title":"desc","summary":"See the pergent comment on this pull request."}}`},
		{forge.StatusNeutral, `{"name":"pergent","head_sha":"` + sha('a') + `","status":"completed","conclusion":"neutral","output":{"title":"desc","summary":"See the pergent comment on this pull request."}}`},
		{forge.StatusCanceled, `{"name":"pergent","head_sha":"` + sha('a') + `","status":"completed","conclusion":"cancelled","output":{"title":"desc","summary":"See the pergent comment on this pull request."}}`},
		{"weird", `{"name":"pergent","head_sha":"` + sha('a') + `","status":"completed","conclusion":"failure","output":{"title":"desc","summary":"See the pergent comment on this pull request."}}`},
	} {
		f := newFake(t)
		f.reply("POST", "/repos/acme/widgets/check-runs", 201, map[string]any{"id": 1})
		if err := f.forge(t).SetStatus(ctx, repoID, sha('a'), tc.state, "desc"); err != nil {
			t.Fatalf("%s: %v", tc.state, err)
		}
		reqs := f.requests("POST", "/repos/acme/widgets/check-runs")
		if len(reqs) != 1 {
			t.Fatalf("%s: %d check runs", tc.state, len(reqs))
		}
		jsonEq(t, reqs[0].Body, tc.want)
	}
}

func TestFindCommentOnlyTheReviewBots(t *testing.T) {
	f := newFake(t)
	f.reply("GET", "/repos/acme/widgets/issues/3/comments", 200, []map[string]any{
		{"id": 1, "body": "<!-- pergent --> fake", "user": map[string]any{"id": 77}},
		{"id": 2, "body": "unrelated", "user": map[string]any{"id": reviewBotID}},
		{"id": 3, "body": "x <!-- pergent --> y", "user": map[string]any{"id": reviewBotID}},
	})
	id, ok, err := f.forge(t).FindComment(ctx, repoID, 3, "<!-- pergent -->")
	if err != nil || !ok || id != 3 {
		t.Fatalf("FindComment = %d, %v, %v; want 3, true", id, ok, err)
	}
	id, ok, err = f.forge(t).FindComment(ctx, repoID, 3, "<!-- other -->")
	if err != nil || ok || id != 0 {
		t.Fatalf("FindComment(missing) = %d, %v, %v", id, ok, err)
	}
}

func TestCreateAndUpdateComment(t *testing.T) {
	f := newFake(t)
	f.reply("POST", "/repos/acme/widgets/issues/3/comments", 201, map[string]any{"id": 31})
	f.reply("PATCH", "/repos/acme/widgets/issues/comments/31", 200, map[string]any{"id": 31})
	fg := f.forge(t)
	id, err := fg.CreateComment(ctx, repoID, 3, "hello")
	if err != nil || id != 31 {
		t.Fatalf("CreateComment = %d, %v", id, err)
	}
	if err := fg.UpdateComment(ctx, repoID, 3, 31, "again"); err != nil {
		t.Fatal(err)
	}
	jsonEq(t, f.requests("PATCH", "/repos/acme/widgets/issues/comments/31")[0].Body, `{"body":"again"}`)
	if err := fg.UpdateComment(ctx, repoID, 3, 32, "gone"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("UpdateComment(deleted) = %v; want forge.ErrNotFound", err)
	}
}

func TestGetFileAndListTree(t *testing.T) {
	f := newFake(t)
	f.handle("GET", "/repos/acme/widgets/contents/.pergent/instructions.md", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "be kind\n")
	})
	f.reply("GET", "/repos/acme/widgets/contents/.pergent/skills", 200, []map[string]any{
		{"name": "a.md", "path": ".pergent/skills/a.md", "type": "file"},
		{"name": "b", "path": ".pergent/skills/b", "type": "dir"},
	})
	fg := f.forge(t)
	b, ok, err := fg.GetFile(ctx, repoID, ".pergent/instructions.md", sha('5'))
	if err != nil || !ok || string(b) != "be kind\n" {
		t.Fatalf("GetFile = %q, %v, %v", b, ok, err)
	}
	if r := f.requests("GET", "/repos/acme/widgets/contents/.pergent/instructions.md"); r[0].Query != "ref="+sha('5') {
		t.Errorf("GetFile query = %q", r[0].Query)
	}
	// A directory is not a file.
	if b, ok, err := fg.GetFile(ctx, repoID, ".pergent/skills", sha('5')); b != nil || ok || err != nil {
		t.Errorf("GetFile(directory) = %q, %v, %v", b, ok, err)
	}
	if b, ok, err := fg.GetFile(ctx, repoID, "missing.md", sha('5')); b != nil || ok || err != nil {
		t.Errorf("GetFile(missing) = %q, %v, %v", b, ok, err)
	}
	entries, err := fg.ListTree(ctx, repoID, ".pergent/skills", sha('5'))
	want := []forge.TreeEntry{{Name: "a.md", Path: ".pergent/skills/a.md", Type: "blob"}, {Name: "b", Path: ".pergent/skills/b", Type: "tree"}}
	if err != nil || !reflect.DeepEqual(entries, want) {
		t.Fatalf("ListTree = %+v, %v", entries, err)
	}
}

func TestChangeRef(t *testing.T) {
	if got := (&Forge{}).ChangeRef(42); got != "refs/pull/42/head" {
		t.Fatalf("ChangeRef = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Approvers

// approvers returns the App approver and the token approver against f, both
// with the approve bot approveBotID.
func approvers(t *testing.T, f *fakeGitHub) map[string]*Approver {
	f.reply("GET", "/repos/acme/widgets/installation", 200, map[string]any{"id": 66})
	return map[string]*Approver{
		"app":   NewAppApprover(f.app(t), approveBotID, repos()),
		"token": NewTokenApprover(github.NewTokenClient(f.srv.URL, patToken, f.srv.Client()), approveBotID, repos(), nil),
	}
}

func TestApprove(t *testing.T) {
	for name := range approvers(t, newFake(t)) {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			a := approvers(t, f)[name]
			f.reply("GET", prPath, 200, pr("open", false, false))
			f.reply("POST", prPath+"/reviews", 200, map[string]any{"id": 5})
			if err := a.Approve(ctx, repoID, 3, sha('a')); err != nil {
				t.Fatal(err)
			}
			reqs := f.requests("POST", prPath+"/reviews")
			if len(reqs) != 1 {
				t.Fatalf("%d reviews posted", len(reqs))
			}
			jsonEq(t, reqs[0].Body, `{"event":"APPROVE","commit_id":"`+sha('a')+`"}`)
			wantAuth := "Bearer " + installToken
			if name == "token" {
				wantAuth = "Bearer " + patToken
			}
			if reqs[0].Auth != wantAuth {
				t.Errorf("approved with %q", reqs[0].Auth)
			}
		})
	}
}

func TestApproveHeadMoved(t *testing.T) {
	for name := range approvers(t, newFake(t)) {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			a := approvers(t, f)[name]
			f.reply("GET", prPath, 200, pr("open", false, false)) // head is sha('a')
			err := a.Approve(ctx, repoID, 3, sha('c'))
			if !errors.Is(err, forge.ErrHeadMoved) {
				t.Fatalf("Approve of an old head = %v; want forge.ErrHeadMoved", err)
			}
			if n := len(f.requests("POST", prPath+"/reviews")); n != 0 {
				t.Errorf("%d reviews posted for a moved head", n)
			}
		})
	}
}

func reviews(list ...map[string]any) []map[string]any { return list }

func review(id int64, user int64, state, commit string) map[string]any {
	return map[string]any{"id": id, "user": map[string]any{"id": user}, "state": state, "commit_id": commit}
}

func TestHasApproved(t *testing.T) {
	for _, tc := range []struct {
		name string
		list []map[string]any
		want bool
	}{
		{"latest approves this head", reviews(review(1, approveBotID, "DISMISSED", sha('9')), review(2, 77, "CHANGES_REQUESTED", sha('a')), review(3, approveBotID, "APPROVED", sha('a'))), true},
		{"approved another head", reviews(review(1, approveBotID, "APPROVED", sha('9'))), false},
		{"latest is dismissed", reviews(review(1, approveBotID, "APPROVED", sha('a')), review(2, approveBotID, "DISMISSED", sha('a'))), false},
		{"someone else approved", reviews(review(1, 77, "APPROVED", sha('a'))), false},
		{"none", reviews(), false},
	} {
		for name := range approvers(t, newFake(t)) {
			f := newFake(t)
			a := approvers(t, f)[name]
			f.reply("GET", prPath+"/reviews", 200, tc.list)
			got, err := a.HasApproved(ctx, repoID, 3, sha('a'))
			if err != nil || got != tc.want {
				t.Errorf("%s/%s: HasApproved = %v, %v; want %v", tc.name, name, got, err, tc.want)
			}
		}
	}
}

func TestUnapproveDismissesTheBotsApprovals(t *testing.T) {
	for name := range approvers(t, newFake(t)) {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			a := approvers(t, f)[name]
			f.reply("GET", prPath+"/reviews", 200, reviews(
				review(1, approveBotID, "APPROVED", sha('9')),
				review(2, approveBotID, "DISMISSED", sha('8')),
				review(3, 77, "APPROVED", sha('a')),
				review(4, approveBotID, "APPROVED", sha('a')),
				review(5, approveBotID, "APPROVED", sha('a')),
			))
			f.reply("PUT", prPath+"/reviews/1/dismissals", 200, map[string]any{"id": 1})
			f.handle("PUT", prPath+"/reviews/4/dismissals", func(w http.ResponseWriter, r *http.Request) {
				// Already dismissed meanwhile: the next listing says so.
				f.reply("GET", prPath+"/reviews", 200, reviews(
					review(1, approveBotID, "DISMISSED", sha('9')),
					review(3, 77, "APPROVED", sha('a')),
					review(4, approveBotID, "DISMISSED", sha('a')),
				))
				reply(422, map[string]any{"message": "already dismissed"})(w, r)
			})
			// review 5: no route, 404 (gone meanwhile)
			if err := a.Unapprove(ctx, repoID, 3); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range f.requests("PUT", "") {
				got = append(got, r.Path)
				jsonEq(t, r.Body, `{"event":"DISMISS","message":"pergent: the reviewed commit or target changed"}`)
			}
			want := []string{prPath + "/reviews/1/dismissals", prPath + "/reviews/4/dismissals", prPath + "/reviews/5/dismissals"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("dismissed %v, want %v", got, want)
			}
		})
	}
}

// GitHub refuses a dismissal with 422 also when branch protection doesn't let
// the approve side dismiss: the approval is still there, which is an error
// (the record is kept and the dismissal retried).
func TestUnapprove422StillApprovedIsError(t *testing.T) {
	for name := range approvers(t, newFake(t)) {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			a := approvers(t, f)[name]
			f.reply("GET", prPath+"/reviews", 200, reviews(review(4, approveBotID, "APPROVED", sha('a'))))
			f.reply("PUT", prPath+"/reviews/4/dismissals", 422, map[string]any{"message": "not allowed to dismiss"})
			if err := a.Unapprove(ctx, repoID, 3); err == nil {
				t.Fatal("Unapprove with a refused dismissal of a standing approval = nil error")
			}
			if n := len(f.requests("GET", prPath+"/reviews")); n != 2 {
				t.Errorf("reviews listed %d times, want 2 (again after the 422)", n)
			}
		})
	}
}

func TestUnapproveErrorIsReturned(t *testing.T) {
	f := newFake(t)
	a := approvers(t, f)["app"]
	f.reply("GET", prPath+"/reviews", 200, reviews(review(1, approveBotID, "APPROVED", sha('9'))))
	f.reply("PUT", prPath+"/reviews/1/dismissals", 403, map[string]any{"message": "no"})
	if err := a.Unapprove(ctx, repoID, 3); err == nil {
		t.Fatal("Unapprove with a refused dismissal = nil error")
	}
}

// The approve App reaches a repository through its own installation, looked
// up once per repository.
func TestAppApproverInstallationCached(t *testing.T) {
	f := newFake(t)
	a := approvers(t, f)["app"]
	f.reply("GET", prPath+"/reviews", 200, reviews())
	for range 3 {
		if _, err := a.HasApproved(ctx, repoID, 3, sha('a')); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.requests("GET", "/repos/acme/widgets/installation")); n != 1 {
		t.Errorf("installation looked up %d times, want 1", n)
	}
	if n := len(f.requests("POST", "/app/installations/66/access_tokens")); n != 1 {
		t.Errorf("approve installation token minted %d times, want 1", n)
	}
}

func TestCanReach(t *testing.T) {
	f := newFake(t)
	as := approvers(t, f)
	f.reply("GET", "/repos/acme/widgets", 200, map[string]any{"id": repoID})
	for name, a := range as {
		ok, err := a.CanReach(ctx, "acme", "widgets")
		if err != nil || !ok {
			t.Errorf("%s: CanReach(installed) = %v, %v", name, ok, err)
		}
		ok, err = a.CanReach(ctx, "acme", "gadgets") // 404 for both
		if err != nil || ok {
			t.Errorf("%s: CanReach(not installed) = %v, %v; want false, nil", name, ok, err)
		}
	}
	f.reply("GET", "/repos/acme/broken/installation", 401, map[string]any{"message": "Bad credentials"})
	if _, err := as["app"].CanReach(ctx, "acme", "broken"); err == nil {
		t.Error("CanReach on a GitHub error = nil error")
	}
}

// CanReach asks GitHub every time, so reconcile sees the approve App being
// uninstalled, and drops the remembered installation with it.
func TestAppCanReachSeesUninstall(t *testing.T) {
	f := newFake(t)
	a := approvers(t, f)["app"]
	f.reply("GET", prPath+"/reviews", 200, reviews())
	if _, err := a.HasApproved(ctx, repoID, 3, sha('a')); err != nil { // remembers installation 66
		t.Fatal(err)
	}
	if ok, err := a.CanReach(ctx, "acme", "widgets"); err != nil || !ok {
		t.Fatalf("CanReach(installed) = %v, %v", ok, err)
	}
	f.reply("GET", "/repos/acme/widgets/installation", 404, map[string]any{"message": "Not Found"})
	if ok, err := a.CanReach(ctx, "acme", "widgets"); err != nil || ok {
		t.Fatalf("CanReach(uninstalled) = %v, %v; want false, nil", ok, err)
	}
	if n := len(f.requests("GET", "/repos/acme/widgets/installation")); n != 3 {
		t.Errorf("installation looked up %d times, want 3 (each CanReach asks)", n)
	}
	// The remembered installation is gone: the next call looks it up again
	// and finds none.
	if _, err := a.HasApproved(ctx, repoID, 3, sha('a')); err == nil {
		t.Error("HasApproved after the uninstall = nil error")
	}
	if n := len(f.requests("GET", "/repos/acme/widgets/installation")); n != 4 {
		t.Errorf("installation looked up %d times, want 4", n)
	}
	// Reinstalled under another id: CanReach remembers the new one.
	f.reply("GET", "/repos/acme/widgets/installation", 200, map[string]any{"id": 67})
	if ok, err := a.CanReach(ctx, "acme", "widgets"); err != nil || !ok {
		t.Fatalf("CanReach(reinstalled) = %v, %v", ok, err)
	}
	if _, err := a.HasApproved(ctx, repoID, 3, sha('a')); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests("POST", "/app/installations/67/access_tokens")); n != 1 {
		t.Errorf("new installation's token minted %d times, want 1", n)
	}
}

// Only a 404 means the token can't reach the repository; a final 403 (a rate
// limit GitHub kept answering, say) is an error, so reconcile keeps what it
// stored.
func TestTokenCanReach403IsError(t *testing.T) {
	f := newFake(t)
	a := approvers(t, f)["token"]
	f.reply("GET", "/repos/acme/widgets", 403, map[string]any{"message": "Resource not accessible"})
	if ok, err := a.CanReach(ctx, "acme", "widgets"); err == nil {
		t.Fatalf("CanReach on 403 = %v, nil; want an error", ok)
	}
}

// A cached approve installation whose token can no longer be minted (the App
// was uninstalled): the call fails and the installation is looked up again
// next time.
func TestAppApproverForgetsInstallationOnMintFailure(t *testing.T) {
	f := newFake(t)
	a := approvers(t, f)["app"]
	f.reply("GET", prPath+"/reviews", 200, reviews())
	f.reply("POST", "/app/installations/66/access_tokens", 404, map[string]any{"message": "Not Found"})
	if _, err := a.HasApproved(ctx, repoID, 3, sha('a')); err == nil {
		t.Fatal("HasApproved with an unmintable token = nil error")
	}
	_, _ = a.HasApproved(ctx, repoID, 3, sha('a'))
	if n := len(f.requests("GET", "/repos/acme/widgets/installation")); n != 2 {
		t.Errorf("installation looked up %d times, want 2 (forgotten after the failed mint)", n)
	}
}

// The review installation's token can't be minted (404: the App was
// uninstalled or reinstalled under another id): that is an error, never
// "not found", or the sweep would forget a live approval and a missing token
// would read as a missing file.
func TestForgeMintFailureIsNotNotFound(t *testing.T) {
	f := newFake(t)
	f.reply("POST", "/app/installations/55/access_tokens", 404, map[string]any{"message": "Not Found"})
	fg := f.forge(t)
	if _, err := fg.GetChange(ctx, repoID, 3); err == nil || errors.Is(err, forge.ErrNotFound) {
		t.Errorf("GetChange = %v; want an error that isn't forge.ErrNotFound", err)
	}
	if b, ok, err := fg.GetFile(ctx, repoID, ".pergent/instructions.md", sha('5')); err == nil || ok || b != nil {
		t.Errorf("GetFile = %q, %v, %v; want an error, not a missing file", b, ok, err)
	}
	if entries, err := fg.ListTree(ctx, repoID, ".pergent/skills", sha('5')); err == nil {
		t.Errorf("ListTree = %v, nil; want an error, not a missing directory", entries)
	}
	if err := fg.UpdateComment(ctx, repoID, 3, 31, "x"); err == nil || errors.Is(err, forge.ErrNotFound) {
		t.Errorf("UpdateComment = %v; want an error that isn't forge.ErrNotFound", err)
	}
}

// The token approver warns when GitHub says its token expires within 14
// days, at most once an hour, and never logs the token.
func TestTokenApproverExpiryWarning(t *testing.T) {
	f := newFake(t)
	var expiry string
	f.handle("GET", prPath+"/reviews", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("GitHub-Authentication-Token-Expiration", expiry)
		reply(200, reviews())(w, nil)
	})
	var logs bytes.Buffer
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	a := NewTokenApprover(github.NewTokenClient(f.srv.URL, patToken, f.srv.Client()), approveBotID, repos(),
		slog.New(slog.NewTextHandler(&logs, nil)))
	a.now = func() time.Time { return now }

	call := func() {
		t.Helper()
		if _, err := a.HasApproved(ctx, repoID, 3, sha('a')); err != nil {
			t.Fatal(err)
		}
	}
	expiry = now.Add(30 * 24 * time.Hour).Format("2006-01-02 15:04:05 MST")
	call()
	if logs.Len() != 0 {
		t.Fatalf("warned 30 days ahead: %s", logs.String())
	}
	expiry = now.Add(10 * 24 * time.Hour).Format("2006-01-02 15:04:05 MST")
	call()
	call()
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings, want 1: %s", n, logs.String())
	}
	now = now.Add(61 * time.Minute)
	call()
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 {
		t.Fatalf("%d warnings after an hour, want 2: %s", n, logs.String())
	}
	if strings.Contains(logs.String(), patToken) {
		t.Fatal("the token was logged")
	}
}

func jsonEq(t *testing.T, body, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(body), &g); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("body = %s\nwant %s", body, want)
	}
}
