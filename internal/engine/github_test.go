package engine

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The GitHub instance the GitHub tests add next to the GitLab one. None of
// these is a real secret.
const (
	ghInstance            = "gh"
	ghRepoID              = int64(900)
	ghRepoPath            = "acme/gadgets"
	ghCloneURL            = "https://github.com/acme/gadgets.git"
	ghReviewApp           = int64(1)
	ghApproveApp          = int64(2)
	ghInstallation        = int64(55)
	ghApproveInstallation = int64(66)
	ghReviewBot           = int64(4001)
	ghApproveBot          = int64(4002)

	ghReviewToken  = "ghs_review-installation-token-example"
	ghApproveToken = "ghs_approve-installation-token-example"
	ghCloneToken   = "ghs_clone-token-example"
)

var (
	ghKeyOnce sync.Once
	ghKeyPEM  string
)

// ghPEM is one App private key for every test (generating one is slow).
func ghPEM() string {
	ghKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		ghKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	})
	return ghKeyPEM
}

// githubRequest is one request the fake GitHub received. App is the App ID
// of a request made with an App JWT, else 0; Token is the bearer token of
// any other.
type githubRequest struct {
	Method, Path string
	App          int64
	Token        string
	Body         []byte
}

func (r githubRequest) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("request %s %s body %q: %v", r.Method, r.Path, r.Body, err)
	}
	return m
}

// fakeGitHub is an httptest GitHub. It mints installation tokens (a clone
// token when the request is scoped to repositories), answers each App's
// installation lookups from installs, and routes everything else on method
// and escaped path; an unrouted request is 404. Use 4xx codes other than 404
// and 429 for errors: 5xx and 429 make the client retry with real sleeps.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	routes   map[string]http.HandlerFunc
	installs map[int64]map[string]int64 // app → "owner/name" → installation
	requests []githubRequest
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, routes: map[string]http.HandlerFunc{}, installs: map[int64]map[string]int64{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// jwtApp is the iss of a JWT bearer token, or 0.
func jwtApp(auth string) int64 {
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) != 3 {
		return 0
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var c struct {
		ISS string `json:"iss"`
	}
	if json.Unmarshal(b, &c) != nil {
		return 0
	}
	id, _ := strconv.ParseInt(c.ISS, 10, 64)
	return id
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	auth := r.Header.Get("Authorization")
	req := githubRequest{Method: r.Method, Path: r.URL.EscapedPath(), App: jwtApp(auth), Body: body}
	if req.App == 0 {
		req.Token = strings.TrimPrefix(auth, "Bearer ")
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	h := f.routes[r.Method+" "+req.Path]
	installs := f.installs[req.App]
	f.mu.Unlock()

	r.Body = io.NopCloser(bytes.NewReader(body))
	switch {
	case h != nil:
		h(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(req.Path, "/app/installations/") && req.App != 0:
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(req.Path, "/app/installations/"), "/access_tokens"), 10, 64)
		tok := map[int64]string{ghInstallation: ghReviewToken, ghApproveInstallation: ghApproveToken}[id]
		out := map[string]any{"token": tok, "expires_at": time.Now().Add(time.Hour).UTC()}
		if bytes.Contains(body, []byte("repository_ids")) {
			out["token"], out["permissions"] = ghCloneToken, map[string]string{"contents": "read", "metadata": "read"}
		}
		writeJSON(w, http.StatusCreated, out)
	case r.Method == http.MethodGet && strings.HasPrefix(req.Path, "/repos/") && strings.HasSuffix(req.Path, "/installation") && req.App != 0:
		repo := strings.TrimSuffix(strings.TrimPrefix(req.Path, "/repos/"), "/installation")
		if id, ok := installs[repo]; ok {
			writeJSON(w, http.StatusOK, map[string]any{"id": id})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Install makes app's installation lookup of owner/name answer id.
func (f *fakeGitHub) Install(app int64, repo string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installs[app] == nil {
		f.installs[app] = map[string]int64{}
	}
	f.installs[app][repo] = id
}

// Uninstall removes app's installation on owner/name.
func (f *fakeGitHub) Uninstall(app int64, repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.installs[app], repo)
}

func (f *fakeGitHub) Handle(method, path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+path] = h
}

func (f *fakeGitHub) Reply(method, path string, status int, v any) {
	f.Handle(method, path, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, v) })
}

// Requests returns the recorded requests for method and path; "" matches any.
func (f *fakeGitHub) Requests(method, path string) []githubRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []githubRequest
	for _, r := range f.requests {
		if (method == "" || r.Method == method) && (path == "" || r.Path == path) {
			out = append(out, r)
		}
	}
	return out
}

// Reset forgets the recorded requests (routes and installations stay).
func (f *fakeGitHub) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

// Writes are the requests that change something on GitHub, minting tokens
// aside.
func (f *fakeGitHub) Writes() []githubRequest {
	var out []githubRequest
	for _, r := range f.Requests("", "") {
		if r.Method != http.MethodGet && !strings.HasSuffix(r.Path, "/access_tokens") {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGitHub) app(t *testing.T, id int64) *github.App {
	t.Helper()
	a, err := github.NewApp(f.srv.URL, id, ghPEM(), f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

const ghRepoAPI = "/repos/" + ghRepoPath

func ghPRPath(n int64) string { return ghRepoAPI + "/pulls/" + strconv.FormatInt(n, 10) }

// ghPR is an open pull request at head against main, whose recorded base is
// sha('5').
func ghPR(n int64, head string) map[string]any {
	return map[string]any{
		"number": n, "state": "open", "merged": false, "draft": false,
		"head": map[string]any{"ref": "feature", "sha": head},
		"base": map[string]any{"ref": "main", "sha": sha('5')},
	}
}

// ServePR serves pull request n and a merge base of sha('b') for it.
func (f *fakeGitHub) ServePR(n int64, pr map[string]any) {
	f.Reply(http.MethodGet, ghPRPath(n), http.StatusOK, pr)
	head := pr["head"].(map[string]any)["sha"].(string)
	f.Reply(http.MethodGet, ghRepoAPI+"/compare/"+sha('5')+"..."+head, http.StatusOK,
		map[string]any{"merge_base_commit": map[string]any{"sha": sha('b')}})
}

// addGitHub adds GitHub instance gh with repository acme/gadgets to env: the
// review App installed as ghInstallation and, when approveInstalled, the
// approve App as ghApproveInstallation. The repository is stored as
// reconcile would, with approve_available = approveInstalled.
func addGitHub(t *testing.T, env *testEnv, approveInstalled bool) *fakeGitHub {
	t.Helper()
	f := newFakeGitHub(t)
	f.Install(ghReviewApp, ghRepoPath, ghInstallation)
	if approveInstalled {
		f.Install(ghApproveApp, ghRepoPath, ghApproveInstallation)
	}
	cfg := env.Engine.Config
	cfg.GitHub = append(cfg.GitHub, config.GitHubInstance{
		Name: ghInstance, APIURL: f.srv.URL,
		ReviewApp:        config.App{AppID: ghReviewApp, PrivateKeyEnv: "GITHUB_REVIEW_KEY"},
		ApproveApp:       &config.App{AppID: ghApproveApp, PrivateKeyEnv: "GITHUB_APPROVE_KEY"},
		WebhookSecretEnv: "GITHUB_WEBHOOK_SECRET",
	})
	cfg.Repositories = append(cfg.Repositories, config.Repository{
		GitHub: ghInstance, Project: ghRepoPath, FailureStatus: config.FailureStatusFailed,
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config with GitHub is invalid: %v", err)
	}
	repos := GitHubRepos(env.Store, ghInstance)
	review := f.app(t, ghReviewApp)
	approver := githubforge.NewAppApprover(f.app(t, ghApproveApp), ghApproveBot, repos)
	env.Engine.Platforms[ghInstance] = Platform{
		Kind:     config.PlatformGitHub,
		Forge:    githubforge.NewForge(review, ghReviewBot, repos),
		Approver: approver,
		GitHub:   &GitHub{App: review, Approve: approver},
	}
	env.SeedGitHub(func(p *store.Project) { p.ApproveAvailable = approveInstalled })
	return f
}

// SeedGitHub stores acme/gadgets as reconcile would; edit, when not nil,
// changes the row first.
func (e *testEnv) SeedGitHub(edit func(p *store.Project)) {
	e.t.Helper()
	p := store.Project{
		Instance: ghInstance, ProjectID: ghRepoID, Path: ghRepoPath, HTTPURLToRepo: ghCloneURL,
		InstallationID: ghInstallation, ApproveAvailable: true,
	}
	if edit != nil {
		edit(&p)
	}
	if err := e.Store.UpsertGitHubProject(e.ctx, p); err != nil {
		e.t.Fatalf("UpsertGitHubProject: %v", err)
	}
}

func ghKey(n int64) store.MRKey {
	return store.MRKey{Instance: ghInstance, ProjectID: ghRepoID, IID: n}
}

// ghEnqueue queues a run of head against main on pull request n.
func ghEnqueue(t *testing.T, env *testEnv, n int64, head string) string {
	t.Helper()
	id, _, err := env.Store.EnqueueRun(env.ctx, ghKey(n), ghRepoPath, head, "main")
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	return id
}

// ghReport moves a run of head on pull request n through dispatch to
// reported with result stored, as Report does for GitLab.
func ghReport(t *testing.T, env *testEnv, n int64, head string, result []byte) string {
	t.Helper()
	id := ghEnqueue(t, env, n, head)
	if _, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.MarkDispatched(env.ctx, id, "job-"+id[:8], sha('b'), sha('5'), hashToken("seed-token"), env.Now().Add(time.Hour), env.Now()); err != nil {
		t.Fatal(err)
	}
	if out, err := env.Store.StoreResult(env.ctx, id, "seed-token", result, env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Dispatch

// A GitHub run's Job clones with a token minted for it alone: scoped to the
// one repository with contents:read, as x-access-token, from the pull
// request's ref. The token is in the Job's Secret and nowhere else.
func TestDispatchGitHub(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	head := sha('a')
	id := ghEnqueue(t, env, 3, head)
	gh.ServePR(3, ghPR(3, head))
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})

	dispatch(t, env)

	job := env.Job(id)
	if job == nil {
		t.Fatalf("no job; run %+v; logs:\n%s", env.Run(id), env.Logs())
	}
	checkout := map[string]string{}
	for _, v := range job.Spec.Template.Spec.InitContainers[0].Env {
		checkout[v.Name] = v.Value
	}
	if checkout["REPO_URL"] != ghCloneURL || checkout["GIT_USERNAME"] != "x-access-token" ||
		checkout["MR_REF"] != "refs/pull/3/head" || checkout["TARGET_BRANCH"] != "main" {
		t.Errorf("checkout env = %v", checkout)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	if !hasArg(args, "--base="+sha('b')) || !hasArg(args, "--head="+head) {
		t.Errorf("job args %v lack the merge base or head", args)
	}
	if got := string(env.Secret(id).Data["git-token"]); got != ghCloneToken {
		t.Errorf("git-token = %q, want the minted clone token", got)
	}

	var scoped []githubRequest
	for _, r := range gh.Requests(http.MethodPost, "/app/installations/55/access_tokens") {
		if len(r.Body) > 0 {
			scoped = append(scoped, r)
		}
	}
	if len(scoped) != 1 {
		t.Fatalf("%d scoped token requests, want 1", len(scoped))
	}
	if scoped[0].App != ghReviewApp {
		t.Errorf("clone token minted by app %d, want the review App", scoped[0].App)
	}
	got, _ := json.Marshal(scoped[0].JSON(t))
	if string(got) != `{"permissions":{"contents":"read"},"repository_ids":[900]}` {
		t.Errorf("clone token request = %s", got)
	}

	spec, _ := json.Marshal(job)
	if bytes.Contains(spec, []byte(ghCloneToken)) {
		t.Error("the clone token is in the Job spec")
	}
	if strings.Contains(env.Logs(), ghCloneToken) || strings.Contains(env.Logs(), ghReviewToken) {
		t.Error("a GitHub token was logged")
	}

	runs := gh.Requests(http.MethodPost, ghRepoAPI+"/check-runs")
	if len(runs) != 1 || runs[0].Token != ghReviewToken {
		t.Fatalf("check runs = %+v; want one by the review App", runs)
	}
	if body := runs[0].JSON(t); body["status"] != "in_progress" || body["head_sha"] != head || body["name"] != "pergent" {
		t.Errorf("running check run = %v", body)
	}
	if r := env.Run(id); r.State != store.Running {
		t.Errorf("run state = %s, want running", r.State)
	}
}

// The review App isn't installed on the repository (any more): the run is
// reported with that reason, and no clone token is asked for.
func TestDispatchGitHubReviewAppNotInstalled(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	env.SeedGitHub(func(p *store.Project) { p.InstallationID = 0 })
	id := ghEnqueue(t, env, 3, sha('a'))

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Reported || r.FailureReason != "the pergent review App isn't installed on this repository" {
		t.Fatalf("run = %s %q; want reported as not installed", r.State, r.FailureReason)
	}
	if env.Job(id) != nil {
		t.Error("a Job was started")
	}
	if n := len(gh.Requests("", "")); n != 0 {
		t.Errorf("%d GitHub requests for a repository the review App can't reach", n)
	}
}

// The installation was removed after reconcile stored it: GitHub refuses the
// clone token with 404, which is the same failure, at once.
func TestDispatchGitHubCloneTokenNotFound(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	head := sha('a')
	id := ghEnqueue(t, env, 3, head)
	gh.ServePR(3, ghPR(3, head))
	gh.Handle(http.MethodPost, "/app/installations/55/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("repository_ids")) {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": ghReviewToken, "expires_at": time.Now().Add(time.Hour).UTC()})
	})

	dispatch(t, env)

	if r := env.Run(id); r.State != store.Reported || r.FailureReason != "the pergent review App isn't installed on this repository" {
		t.Fatalf("run = %s %q; want reported as not installed\n%s", r.State, r.FailureReason, env.Logs())
	}
	if env.Job(id) != nil {
		t.Error("a Job was started")
	}
}

// ---------------------------------------------------------------------------
// Reconcile

func ghProject(t *testing.T, env *testEnv) (store.Project, bool) {
	t.Helper()
	p, ok, err := env.Store.Project(env.ctx, ghInstance, ghRepoID)
	if err != nil {
		t.Fatal(err)
	}
	return p, ok
}

// Reconcile stores the review App's installation, the repository ID and clone
// URL; an approve App that isn't installed is stored as approve_available =
// false with a warning, and is not an error.
func TestReconcileGitHub(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, false)
	if err := env.Store.DeleteProject(env.ctx, ghInstance, ghRepoID); err != nil {
		t.Fatal(err)
	}
	serveProject(env, testProjectID, testProjectPath)
	gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, map[string]any{"id": ghRepoID, "full_name": ghRepoPath, "clone_url": ghCloneURL})

	reconcile(t, env)

	p, ok := ghProject(t, env)
	if !ok || p.InstallationID != ghInstallation || p.ApproveAvailable || p.HTTPURLToRepo != ghCloneURL || p.Path != ghRepoPath {
		t.Fatalf("stored = %+v, %v; want installation 55, approve unavailable, the clone URL", p, ok)
	}
	if reqs := gh.Requests(http.MethodGet, ghRepoAPI); len(reqs) != 1 || reqs[0].Token != ghReviewToken {
		t.Errorf("repository read = %+v; want once with the review installation's token", reqs)
	}
	if !strings.Contains(env.Logs(), "level=WARN") || !strings.Contains(env.Logs(), "approve") {
		t.Errorf("no warning about the approve side:\n%s", env.Logs())
	}
	if w := gh.Writes(); len(w) != 0 {
		t.Errorf("reconcile wrote to GitHub: %+v", w)
	}
	for _, tok := range []string{ghReviewToken, ghApproveToken, ghCloneToken} {
		if strings.Contains(env.Logs(), tok) {
			t.Errorf("token %q logged", tok)
		}
	}
	if _, ok := project(t, env, testProjectID); !ok {
		t.Error("the GitLab project is gone")
	}

	// The approve App gets installed: the next pass says so.
	gh.Install(ghApproveApp, ghRepoPath, ghApproveInstallation)
	reconcile(t, env)
	if p, _ := ghProject(t, env); !p.ApproveAvailable || p.InstallationID != ghInstallation {
		t.Fatalf("after installing the approve App = %+v; want approve available", p)
	}
}

// The token approver reaches a repository its token can read.
func TestReconcileGitHubTokenApprover(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, false)
	plat := env.Engine.Platforms[ghInstance]
	approver := githubforge.NewTokenApprover(github.NewTokenClient(gh.srv.URL, "github_pat_example", gh.srv.Client()), ghApproveBot, GitHubRepos(env.Store, ghInstance), nil)
	plat.Approver, plat.GitHub.Approve = approver, approver
	env.Engine.Platforms[ghInstance] = plat
	serveProject(env, testProjectID, testProjectPath)
	gh.Handle(http.MethodGet, ghRepoAPI, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": ghRepoID, "full_name": ghRepoPath, "clone_url": ghCloneURL})
	})

	reconcile(t, env)

	if p, _ := ghProject(t, env); !p.ApproveAvailable {
		t.Fatalf("stored = %+v; want approve available", p)
	}
	var byToken bool
	for _, r := range gh.Requests(http.MethodGet, ghRepoAPI) {
		byToken = byToken || r.Token == "github_pat_example"
	}
	if !byToken {
		t.Error("the approve token was never checked against the repository")
	}
}

// The review App was uninstalled: the stored repository keeps its row with
// no installation, so its runs are reported as not installed, and the pass
// reports the error.
func TestReconcileGitHubReviewAppUninstalled(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	gh.Uninstall(ghReviewApp, ghRepoPath)
	serveProject(env, testProjectID, testProjectPath)

	err := env.Engine.Reconcile(env.ctx)
	if err == nil || !strings.Contains(err.Error(), "isn't installed") {
		t.Fatalf("Reconcile = %v; want the review App not installed", err)
	}
	p, ok := ghProject(t, env)
	if !ok || p.InstallationID != 0 {
		t.Fatalf("stored = %+v, %v; want the row kept with installation 0", p, ok)
	}
}

// A GitHub repository removed from the config loses its row and nothing
// else: there is no webhook or token to delete.
func TestReconcileGitHubRemoved(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	env.Engine.Config.Repositories = env.Engine.Config.Repositories[:1]
	serveProject(env, testProjectID, testProjectPath)

	reconcile(t, env)

	if _, ok := ghProject(t, env); ok {
		t.Fatal("the removed repository's row is still stored")
	}
	if n := len(gh.Requests("", "")); n != 0 {
		t.Errorf("%d GitHub requests to remove a repository", n)
	}
	if logs := env.Logs(); !strings.Contains(logs, "repository switched off") || strings.Contains(logs, "deploy token") {
		t.Errorf("removal logged as something else than a switched-off GitHub repository:\n%s", logs)
	}
	if _, ok := project(t, env, testProjectID); !ok {
		t.Error("the GitLab project is gone")
	}
}

// ---------------------------------------------------------------------------
// Publish

// servePublishGitHub routes what a publish of pull request n at head needs.
func servePublishGitHub(gh *fakeGitHub, n int64, head string) {
	gh.ServePR(n, ghPR(n, head))
	comments := ghRepoAPI + "/issues/" + strconv.FormatInt(n, 10) + "/comments"
	gh.Reply(http.MethodGet, comments, http.StatusOK, []any{})
	gh.Reply(http.MethodPost, comments, http.StatusCreated, map[string]any{"id": 77})
	gh.Reply(http.MethodPost, ghRepoAPI+"/check-runs", http.StatusCreated, map[string]any{"id": 1})
	gh.Reply(http.MethodPost, ghPRPath(n)+"/reviews", http.StatusOK, map[string]any{"id": 9})
}

func onlyCheckRun(t *testing.T, gh *fakeGitHub) map[string]any {
	t.Helper()
	runs := gh.Requests(http.MethodPost, ghRepoAPI+"/check-runs")
	if len(runs) != 1 || runs[0].Token != ghReviewToken {
		t.Fatalf("check runs = %+v; want one by the review App", runs)
	}
	return runs[0].JSON(t)
}

func TestPublishGitHubApproves(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	head := sha('a')
	id := ghReport(t, env, 3, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublishGitHub(gh, 3, head)

	publish(t, env)

	wantPublished(t, env, id)
	run := onlyCheckRun(t, gh)
	out, _ := run["output"].(map[string]any)
	if run["conclusion"] != "success" || run["status"] != "completed" || out["title"] != "No blocking issues" {
		t.Errorf("check run = %v", run)
	}
	reviews := gh.Requests(http.MethodPost, ghPRPath(3)+"/reviews")
	if len(reviews) != 1 || reviews[0].Token != ghApproveToken {
		t.Fatalf("reviews = %+v; want one by the approve App", reviews)
	}
	if body := reviews[0].JSON(t); body["event"] != "APPROVE" || body["commit_id"] != head {
		t.Errorf("review = %v", body)
	}
	if a := approvals(t, env); len(a) != 1 || a[0].MR != ghKey(3) || a[0].ApprovedSHA != head {
		t.Errorf("approvals = %+v", a)
	}
}

// The approve App isn't installed on the repository: the review is
// published with its status and a note, never approved and never retried.
func TestPublishGitHubApproveUnavailable(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, false)
	head := sha('a')
	id := ghReport(t, env, 3, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublishGitHub(gh, 3, head)

	publish(t, env)

	wantPublished(t, env, id)
	if r := env.Run(id); r.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", r.Attempts)
	}
	run := onlyCheckRun(t, gh)
	out, _ := run["output"].(map[string]any)
	if run["conclusion"] != "success" || out["title"] != "No blocking issues; not approved: the pergent approve App isn't installed on this repository" {
		t.Errorf("check run = %v", run)
	}
	comments := gh.Requests(http.MethodPost, ghRepoAPI+"/issues/3/comments")
	if len(comments) != 1 {
		t.Fatalf("%d comments posted", len(comments))
	}
	if body, _ := comments[0].JSON(t)["body"].(string); !strings.Contains(body, "**Not approved:** the pergent approve App isn't installed on this repository.") {
		t.Errorf("comment lacks the note: %q", body)
	}
	for _, r := range gh.Requests("", "") {
		if r.App == ghApproveApp || r.Token == ghApproveToken || strings.HasSuffix(r.Path, "/reviews") {
			t.Errorf("approve side called: %s %s", r.Method, r.Path)
		}
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %+v, want none", a)
	}
}

// ---------------------------------------------------------------------------
// Approval sweep

func ghApproved(t *testing.T, env *testEnv, n int64, head string) {
	t.Helper()
	id := ghReport(t, env, n, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.MarkPublished(env.ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.SetApprovedSHA(env.ctx, ghKey(n), head, "main"); err != nil {
		t.Fatal(err)
	}
}

// A closed pull request GitHub reports as merged: the approval record is
// cleared without any GitHub write, as for GitLab's merged.
func TestSweepGitHubMerged(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	ghApproved(t, env, 3, sha('a'))
	pr := ghPR(3, sha('a'))
	pr["state"], pr["merged"] = "closed", true
	gh.Reply(http.MethodGet, ghPRPath(3), http.StatusOK, pr)

	sweep(t, env)

	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %+v, want none", a)
	}
	if w := gh.Writes(); len(w) != 0 {
		t.Errorf("GitHub writes for a merged pull request: %+v", w)
	}
}

// The head moved without a webhook: the approve App's approval is dismissed
// and the current head is queued.
func TestSweepGitHubMovedHead(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	ghApproved(t, env, 3, sha('a'))
	gh.ServePR(3, ghPR(3, sha('c')))
	gh.Reply(http.MethodGet, ghPRPath(3)+"/reviews", http.StatusOK, []map[string]any{
		{"id": 41, "user": map[string]any{"id": ghApproveBot}, "state": "APPROVED", "commit_id": sha('a')},
	})
	gh.Reply(http.MethodPut, ghPRPath(3)+"/reviews/41/dismissals", http.StatusOK, map[string]any{"id": 41})

	sweep(t, env)

	d := gh.Requests(http.MethodPut, ghPRPath(3)+"/reviews/41/dismissals")
	if len(d) != 1 || d[0].Token != ghApproveToken {
		t.Fatalf("dismissals = %+v; want one by the approve App", d)
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %+v, want none", a)
	}
	if _, created, err := env.Store.EnqueueRun(env.ctx, ghKey(3), ghRepoPath, sha('c'), "main"); err != nil || created {
		t.Errorf("no run of the current head queued (created=%v, %v)", created, err)
	}
}

// ---------------------------------------------------------------------------
// Fix round 1

// The review installation's token can't be minted (the review App was
// uninstalled or reinstalled under another id): the sweep can't read the
// pull request, so it keeps the approval record and tries again; it never
// forgets an approval that may still stand.
func TestSweepGitHubReviewMintFailsKeepsApproval(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	ghApproved(t, env, 3, sha('a'))
	gh.Reply(http.MethodPost, "/app/installations/55/access_tokens", http.StatusNotFound, map[string]string{"message": "Not Found"})

	sweep(t, env)

	if a := approvals(t, env); len(a) != 1 || a[0].ApprovedSHA != sha('a') {
		t.Errorf("approvals = %+v; want the record kept", a)
	}
	if !strings.Contains(env.Logs(), "couldn't check the approval") {
		t.Errorf("the failure wasn't logged:\n%s", env.Logs())
	}
	if w := gh.Writes(); len(w) != 0 {
		t.Errorf("GitHub writes: %+v", w)
	}
}

// reconcileGitHubSetup routes the GitLab project and the GitHub repository
// for a reconcile pass.
func reconcileGitHubSetup(env *testEnv, gh *fakeGitHub) {
	serveProject(env, testProjectID, testProjectPath)
	gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, map[string]any{"id": ghRepoID, "full_name": ghRepoPath, "clone_url": ghCloneURL})
}

// The approve App is uninstalled after a pass found it (and the approver
// remembered its installation): the next pass records it as unreachable.
func TestReconcileGitHubApproveAppUninstalled(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	reconcileGitHubSetup(env, gh)
	reconcile(t, env)
	if p, _ := ghProject(t, env); !p.ApproveAvailable {
		t.Fatalf("stored = %+v; want approve available", p)
	}

	gh.Uninstall(ghApproveApp, ghRepoPath)
	reconcile(t, env)

	if p, _ := ghProject(t, env); p.ApproveAvailable {
		t.Fatalf("stored = %+v; want approve unavailable after the uninstall", p)
	}
}

// The approve side can't be asked (GitHub refuses the lookup): the pass
// reports it and keeps the stored answer.
func TestReconcileGitHubApproveCheckErrorKeepsStored(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, false)
	reconcileGitHubSetup(env, gh)
	gh.Handle(http.MethodGet, ghRepoAPI+"/installation", func(w http.ResponseWriter, r *http.Request) {
		switch jwtApp(r.Header.Get("Authorization")) {
		case ghReviewApp:
			writeJSON(w, http.StatusOK, map[string]any{"id": ghInstallation})
		default:
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		}
	})

	if err := env.Engine.Reconcile(env.ctx); err == nil || !strings.Contains(err.Error(), "approve side") {
		t.Fatalf("Reconcile = %v; want the approve check's error", err)
	}
	if p, _ := ghProject(t, env); p.ApproveAvailable || p.InstallationID != ghInstallation {
		t.Fatalf("stored = %+v; want approve unavailable kept", p)
	}
}

// With approve_token_env the approve side is a bot user: a repository it
// can't reach is published with that note, not the App's.
func TestPublishGitHubTokenApproverUnavailable(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, false)
	inst := &env.Engine.Config.GitHub[len(env.Engine.Config.GitHub)-1]
	inst.ApproveApp, inst.ApproveTokenEnv = nil, "GITHUB_APPROVE_TOKEN"
	if err := env.Engine.Config.Validate(); err != nil {
		t.Fatal(err)
	}
	head := sha('a')
	id := ghReport(t, env, 3, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublishGitHub(gh, 3, head)

	publish(t, env)

	wantPublished(t, env, id)
	run := onlyCheckRun(t, gh)
	if out, _ := run["output"].(map[string]any); out["title"] != "No blocking issues; not approved: the pergent approve bot can't reach this repository" {
		t.Errorf("check run = %v", run)
	}
	comments := gh.Requests(http.MethodPost, ghRepoAPI+"/issues/3/comments")
	if len(comments) != 1 {
		t.Fatalf("%d comments posted", len(comments))
	}
	body, _ := comments[0].JSON(t)["body"].(string)
	if !strings.Contains(body, "**Not approved:** the pergent approve bot can't reach this repository.") || strings.Contains(body, "approve App") {
		t.Errorf("comment note: %q", body)
	}
}
