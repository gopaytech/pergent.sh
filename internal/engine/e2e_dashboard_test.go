package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/server"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Values of the dashboard end-to-end test. None of them is a real secret.
const (
	dashURL          = "https://dashboard.example.com"
	dashClientID     = "gl-oauth-client-id-example"
	dashClientSecret = "gl-oauth-client-secret-example"

	// alice is on an admin team; bob is on a viewer team.
	aliceID, bobID       = int64(31), int64(32)
	aliceCode, bobCode   = "alice-code", "bob-code"
	aliceToken, bobToken = "alice-oauth-token-example", "bob-oauth-token-example"

	// acme/gadgets is the repository the admin switches on in the
	// dashboard; acme/payments is in the config, and neither user can see
	// it.
	gadgetsID    = int64(9)
	gadgetsPath  = "acme/gadgets"
	paymentsID   = int64(8)
	paymentsPath = "acme/payments"
)

// dashE2E is e2e with the dashboard on: the routes main serves (webhooks,
// result callback, /auth/, /api/ and the app) with the real store and
// dashboard, in front of a testEnv whose fake GitLab also does OAuth and
// answers the users' API calls.
type dashE2E struct {
	*e2e
	// client never follows redirects: the test reads them.
	client *http.Client

	mu     sync.Mutex
	bodies []string // every response body the dashboard sent
}

func newDashboardE2E(t *testing.T) *dashE2E {
	env := newTestEnv(t)
	cfg := env.Engine.Config
	t.Setenv("GITLAB_OAUTH_CLIENT_ID", dashClientID)
	t.Setenv("GITLAB_OAUTH_CLIENT_SECRET", dashClientSecret)
	cfg.GitLab[0].OAuthClientIDEnv = "GITLAB_OAUTH_CLIENT_ID"
	cfg.GitLab[0].OAuthClientSecretEnv = "GITLAB_OAUTH_CLIENT_SECRET"
	cfg.Dashboard = config.Dashboard{URL: dashURL}
	cfg.Repositories = append(cfg.Repositories, config.Repository{
		GitLab: testInstance, Project: paymentsPath, FailureStatus: config.FailureStatusFailed,
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("dashboard test config is invalid: %v", err)
	}
	serveDashboardGitLab(t, env)

	// What main passes: the review bot's Forge reads repositories and
	// lists what it can set up; the engine reconciles soon on request.
	sources := map[string]dashboard.RepoSource{}
	catalogs := map[string]dashboard.RepoCatalog{}
	for name, p := range env.Engine.Platforms {
		sources[name] = p.Forge.(dashboard.RepoSource)
		catalogs[name] = p.Forge.(dashboard.RepoCatalog)
	}
	d, err := dashboard.New(dashboard.Options{
		Config: cfg, Store: env.Store, SecretKey: testBoxKey, HTTPClient: env.GitLab.srv.Client(),
		Now: env.Now, Log: env.Engine.Log, Library: env.Engine.Library,
		Sources: sources, Catalogs: catalogs, ReconcileSoon: env.Engine.ReconcileSoon,
	})
	if err != nil {
		t.Fatalf("dashboard.New: %v", err)
	}
	mux := server.Routes(server.Deps{
		Config: cfg, WebhookSecrets: env.Engine.WebhookSecrets, Store: env.Store,
		Wake: env.Engine.Wake, Now: env.Now, Dashboard: d.Handler(), Log: env.Engine.Log,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &dashE2E{
		e2e: &e2e{env: env, srv: srv},
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

// dashAccess is the GitLab access level of each user (by OAuth token) and of
// the review bot in each project; 0 is no access (GitLab answers 404).
var dashAccess = map[string]map[string]int{
	testReviewToken: {testProjectPath: 40, paymentsPath: 40, gadgetsPath: 40},
	aliceToken:      {testProjectPath: 40, gadgetsPath: 40},
	bobToken:        {testProjectPath: 30, gadgetsPath: 30},
}

var dashProjectIDs = map[string]int64{testProjectPath: testProjectID, paymentsPath: paymentsID, gadgetsPath: gadgetsID}

// serveDashboardGitLab makes env's fake GitLab an OAuth provider with users
// alice (admin) and bob, answer GET /projects/:path for the users and the
// review bot alike, and set up the webhooks and deploy tokens of every
// project.
func serveDashboardGitLab(t *testing.T, env *testEnv) {
	gl := env.GitLab
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	bearer := func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }

	gl.Handle(http.MethodPost, "/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != dashClientID || r.Form.Get("client_secret") != dashClientSecret ||
			r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("redirect_uri") != dashURL+"/auth/gitlab/"+testInstance+"/callback" ||
			r.Form.Get("code_verifier") == "" {
			reply(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
			return
		}
		tok := map[string]string{aliceCode: aliceToken, bobCode: bobToken}[r.Form.Get("code")]
		if tok == "" {
			reply(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		reply(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer"})
	})
	gl.Handle(http.MethodGet, "/user", func(w http.ResponseWriter, r *http.Request) {
		switch bearer(r) {
		case aliceToken:
			reply(w, http.StatusOK, map[string]any{"id": aliceID, "username": "alice"})
		case bobToken:
			reply(w, http.StatusOK, map[string]any{"id": bobID, "username": "bob"})
		default:
			reply(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
		}
	})
	for path, id := range dashProjectIDs {
		pid := "/projects/" + strconv.FormatInt(id, 10)
		gl.Handle(http.MethodGet, "/projects/"+url.PathEscape(path), func(w http.ResponseWriter, r *http.Request) {
			who := r.Header.Get("PRIVATE-TOKEN")
			if who == "" {
				who = bearer(r)
			}
			levels, ok := dashAccess[who]
			if !ok {
				reply(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
				return
			}
			level := levels[path]
			if level == 0 {
				reply(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
				return
			}
			reply(w, http.StatusOK, map[string]any{
				"id": id, "path_with_namespace": path, "visibility": "private",
				"http_url_to_repo":        "https://gitlab.example.com/" + path + ".git",
				"default_branch":          "main",
				"repository_access_level": "enabled", "merge_requests_access_level": "enabled",
				"permissions": map[string]any{"project_access": map[string]any{"access_level": level}},
			})
		})
		gl.Reply(http.MethodGet, pid+"/hooks", http.StatusOK, `[]`)
		gl.Reply(http.MethodPost, pid+"/hooks", http.StatusCreated, map[string]any{"id": newHookID, "url": testHookURL})
		gl.Reply(http.MethodPost, pid+"/deploy_tokens", http.StatusCreated, map[string]any{
			"id": newDeployTokenID, "username": newDeployUser, "token": newDeployToken,
			"expires_at": env.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339),
		})
	}
}

// do sends a request to the routes with cookies and headers and returns the
// status, the body and the cookies set.
func (e *dashE2E) do(t *testing.T, method, path string, body any, cookies []*http.Cookie, header map[string]string) (int, []byte, http.Header, []*http.Cookie) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.bodies = append(e.bodies, string(data)+"\n"+strings.Join(resp.Header.Values("Location"), " "))
	e.mu.Unlock()
	return resp.StatusCode, data, resp.Header, resp.Cookies()
}

// dashUser is a signed-in browser: its session cookie and CSRF token.
type dashUser struct {
	session *http.Cookie
	csrf    string
	isAdmin bool
}

// team puts the GitLab user userID (username) on a new team with role
// covering repos, making their account as their first sign-in would.
func (e *dashE2E) team(t *testing.T, name, role string, userID int64, username string, repos ...store.TeamRepo) {
	t.Helper()
	env := e.env
	a, err := env.Store.UpsertPlatformAccount(env.ctx, config.PlatformGitLab, testInstance, userID, username, env.Now())
	if err != nil {
		t.Fatal(err)
	}
	tm, err := env.Store.CreateTeam(env.ctx, name, role, env.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := env.Store.UpdateTeam(env.ctx, tm.ID, func(tm *store.Team) error {
		tm.Members, tm.Repos = []int64{a.ID}, repos
		return nil
	}); err != nil || !ok {
		t.Fatalf("UpdateTeam = %v, %v", ok, err)
	}
}

// signIn goes through GitLab sign-in with code, as the browser would: the
// login route sends it to GitLab's authorize page, and GitLab sends it back
// to the callback with the code and the state.
func (e *dashE2E) signIn(t *testing.T, code string) dashUser {
	t.Helper()
	status, _, h, cookies := e.do(t, http.MethodGet, "/auth/gitlab/"+testInstance+"/login", nil, nil, nil)
	if status != http.StatusFound {
		t.Fatalf("login = %d, want 302", status)
	}
	loc, err := url.Parse(h.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), e.env.GitLab.URL()+"/oauth/authorize?") || q.Get("scope") != "read_user read_api" ||
		q.Get("client_id") != dashClientID || q.Get("redirect_uri") != dashURL+"/auth/gitlab/"+testInstance+"/callback" {
		t.Fatalf("login sends the browser to %s", loc)
	}
	cb := url.Values{"code": {code}, "state": {q.Get("state")}}
	status, body, h, cookies := e.do(t, http.MethodGet, "/auth/gitlab/"+testInstance+"/callback?"+cb.Encode(), nil, cookies, nil)
	if status != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("callback = %d to %q: %s", status, h.Get("Location"), body)
	}
	var u dashUser
	for _, c := range cookies {
		if c.Name == dashboard.SessionCookieName && c.Value != "" {
			u.session = c
		}
	}
	if u.session == nil || !u.session.HttpOnly || !u.session.Secure || u.session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("callback set session cookie %+v; want HttpOnly, Secure, SameSite=Lax", u.session)
	}
	var me struct {
		Username  string `json:"username"`
		Admin     bool   `json:"admin"`
		CSRFToken string `json:"csrfToken"`
	}
	e.getJSON(t, u, "/api/me", &me)
	if me.CSRFToken == "" {
		t.Fatalf("/api/me has no CSRF token")
	}
	u.csrf, u.isAdmin = me.CSRFToken, me.Admin
	return u
}

// getJSON GETs path as u, wants 200 and decodes the body into v.
func (e *dashE2E) getJSON(t *testing.T, u dashUser, path string, v any) {
	t.Helper()
	status, body, _, _ := e.do(t, http.MethodGet, path, nil, []*http.Cookie{u.session}, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, status, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("GET %s body %s: %v", path, body, err)
	}
}

// write sends a write as u with its CSRF token and returns the status and
// body.
func (e *dashE2E) write(t *testing.T, u dashUser, method, path string, body any) (int, []byte) {
	t.Helper()
	status, data, _, _ := e.do(t, method, path, body, []*http.Cookie{u.session}, map[string]string{
		dashboard.CSRFHeader: u.csrf, "Content-Type": "application/json",
	})
	return status, data
}

// seedPublished stores a published clean review of head on merge request
// iid of project, as if pergent.sh had reviewed it.
func seedPublished(t *testing.T, env *testEnv, projectID int64, path string, iid int64, head string) {
	t.Helper()
	key := store.MRKey{Instance: testInstance, ProjectID: projectID, IID: iid}
	id, _, err := env.Store.EnqueueRun(env.ctx, key, path, head, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.MarkDispatched(env.ctx, id, kube.JobName(id), sha('b'), sha('5'), hashToken("seed-token"), env.Now().Add(time.Hour), env.Now()); err != nil {
		t.Fatal(err)
	}
	if out, err := env.Store.StoreResult(env.ctx, id, "seed-token", resultFor(t, head, reviewed("no_blocking_issues", "a.go")), env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := env.Store.MarkPublished(env.ctx, id, store.RunDecision{}); err != nil {
		t.Fatal(err)
	}
}

// gadgetsPath and its merge request paths on the fake GitLab.
func gadgetsAPI(suffix string) string {
	return "/projects/" + strconv.FormatInt(gadgetsID, 10) + suffix
}

// A user signs in with GitLab and sees only what they can see there; an
// admin switches a repository on, which is set up and reviewed, and turns
// its approval off, which withdraws the approval; a developer can't write.
func TestEndToEndDashboard(t *testing.T) {
	e := newDashboardE2E(t)
	env := e.env
	gl := env.GitLab

	// Earlier reviews: one in acme/widgets, which alice can see, one in
	// acme/payments, which she can't. Each has a setup problem.
	seedPublished(t, env, testProjectID, testProjectPath, 1, sha('1'))
	seedPublished(t, env, paymentsID, paymentsPath, 2, sha('2'))
	for _, p := range []string{testProjectPath, paymentsPath} {
		if err := env.Store.SetAttention(env.ctx, testInstance, p, attSingleApproval, "One approval is enough to merge in "+p+"."); err != nil {
			t.Fatal(err)
		}
	}

	// 1. alice signs in with GitLab; she is an admin. bob views every
	// repository of the instance.
	e.team(t, "admins", store.RoleAdmin, aliceID, "alice")
	e.team(t, "developers", store.RoleViewer, bobID, "bob", store.TeamRepo{Instance: testInstance})
	alice := e.signIn(t, aliceCode)
	if !alice.isAdmin {
		t.Fatalf("alice is not an admin")
	}

	// 2. The overview shows only the repositories she can see.
	var overview dashboard.OverviewView
	e.getJSON(t, alice, "/api/overview", &overview)
	if len(overview.Recent) != 1 || overview.Recent[0].Project != testProjectPath || overview.Recent[0].Number != 1 {
		t.Errorf("recent reviews = %+v, want only acme/widgets !1", overview.Recent)
	}
	if len(overview.Attention) != 1 || overview.Attention[0].Project != testProjectPath {
		t.Errorf("attention = %+v, want only acme/widgets", overview.Attention)
	}
	if overview.Stats.Reviews != 1 {
		t.Errorf("stats = %+v, want 1 review", overview.Stats)
	}
	for _, path := range []string{"/api/repositories/" + testInstance + "/" + paymentsPath, "/api/reviews/" + testInstance + "/" + paymentsPath + "/2"} {
		if status, _, _, _ := e.do(t, http.MethodGet, path, nil, []*http.Cookie{alice.session}, nil); status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, status)
		}
	}

	// 3. alice switches acme/gadgets on; the engine is asked to reconcile.
	if status, body := e.write(t, alice, http.MethodPost, "/api/repositories", map[string]string{"project": gadgetsPath}); status != http.StatusCreated {
		t.Fatalf("switch on = %d: %s", status, body)
	}
	if _, found, err := env.Store.DashboardRepo(env.ctx, testInstance, gadgetsPath); err != nil || !found {
		t.Fatalf("acme/gadgets isn't switched on (%v)", err)
	}
	select {
	case <-env.Engine.reconcileCh():
	default:
		t.Fatalf("switching on didn't ask the engine to reconcile")
	}
	keys := env.Engine.takeSoonKeys()
	if len(keys) != 1 || !strings.EqualFold(keys[0].Project, gadgetsPath) || keys[0].Instance != testInstance {
		t.Fatalf("reconcile asked for %+v, want acme/gadgets alone", keys)
	}

	// 4. The reconcile of that repository creates its webhook and deploy
	// token, and asks nothing about any other.
	gl.Reset()
	if err := env.Engine.ReconcileRepos(env.ctx, keys); err != nil {
		t.Fatalf("ReconcileRepos: %v", err)
	}
	for _, r := range gl.Requests("", "") {
		if !strings.HasPrefix(r.Path, gadgetsAPI("/")) && !strings.Contains(r.Path, "acme%2Fgadgets") {
			t.Errorf("the reconcile of acme/gadgets asked about another repository: %s %s", r.Method, r.Path)
		}
	}
	hooks := gl.Requests(http.MethodPost, gadgetsAPI("/hooks"))
	if len(hooks) != 1 || hooks[0].Token != testReviewToken || hooks[0].JSON(t)["url"] != testHookURL {
		t.Fatalf("webhook creations on acme/gadgets = %d, want 1 by the review bot", len(hooks))
	}
	if p, ok := project(t, env, gadgetsID); !ok || p.Path != gadgetsPath || p.DeployTokenID != newDeployTokenID {
		t.Fatalf("acme/gadgets project = %+v, %v; want set up", p, ok)
	}

	// 5. A merge request is opened: its webhook queues a review, the Job's
	// result has a finding, the review is published and approved, and the
	// dashboard shows the run with its finding and its redacted log.
	head := sha('a')
	mr := openMR(1, head)
	mr.Title = "Add the gadget counter"
	gl.Reply(http.MethodGet, gadgetsAPI("/merge_requests/1"), http.StatusOK, mr)
	gl.Reply(http.MethodPost, gadgetsAPI("/statuses/"+head), http.StatusCreated, `{}`)
	gl.Reply(http.MethodGet, gadgetsAPI("/merge_requests/1/notes"), http.StatusOK, `[]`)
	gl.Reply(http.MethodPost, gadgetsAPI("/merge_requests/1/notes"), http.StatusCreated, map[string]any{"id": createdNote})
	gl.Reply(http.MethodPost, gadgetsAPI("/merge_requests/1/approve"), http.StatusCreated, `{}`)
	gl.Reply(http.MethodPost, gadgetsAPI("/merge_requests/1/unapprove"), http.StatusCreated, `{}`)

	hook, _ := json.Marshal(map[string]any{
		"object_kind": "merge_request",
		"project":     map[string]any{"id": gadgetsID, "path_with_namespace": gadgetsPath},
		"object_attributes": map[string]any{
			"iid": 1, "action": "open", "target_branch": "main", "last_commit": map[string]any{"id": head},
		},
	})
	status, body, _, _ := e.do(t, http.MethodPost, "/webhooks/gitlab/"+testInstance, json.RawMessage(hook), nil, map[string]string{
		"X-Gitlab-Token": testWebhookSecret, "X-Gitlab-Event": "Merge Request Hook",
	})
	if status != http.StatusOK {
		t.Fatalf("webhook = %d: %s", status, body)
	}
	dispatch(t, env)
	var runID string
	for _, j := range env.Jobs() {
		if id := j.Labels["pergent.sh/run"]; env.Run(id).MR.ProjectID == gadgetsID {
			runID = id
		}
	}
	if runID == "" {
		t.Fatalf("no review Job for acme/gadgets")
	}
	resultToken := string(env.Secret(runID).Data["result-token"])
	finding, _ := json.Marshal(map[string]any{
		"id": "F1", "severity": "suggestion", "title": "Name the magic number", "file": "counter.go", "line": 12,
		"explanation": "42 has no name.", "suggested_fix": "const maxGadgets = 42",
	})
	skill := reviewed("no_blocking_issues", "counter.go")
	skill.Findings = []json.RawMessage{finding}
	if code := e.postResult(t, runID, resultToken, resultFor(t, head, skill)); code != http.StatusOK {
		t.Fatalf("result = %d", code)
	}

	// The Job finishes; the watcher keeps its log, with the secrets hidden.
	job := env.Job(runID)
	job.Status.Conditions = []batchv1.JobCondition{jobCond(batchv1.JobComplete, "", env.Now())}
	if _, err := env.Kube.BatchV1().Jobs(testNamespace).UpdateStatus(env.ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: job.Name + "-abcde", Namespace: testNamespace, Labels: map[string]string{"job-name": job.Name},
		CreationTimestamp: metav1.NewTime(env.Now().Add(-time.Minute)),
	}}
	if _, err := env.Kube.CoreV1().Pods(testNamespace).Create(env.ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	serveLog(env, "cloning with "+newDeployToken+"\nmodel key "+testLLMKey+"\nposting with "+resultToken+"\nreview done\n")
	watch(t, env)

	publish(t, env)
	wantPublished(t, env, runID)
	approves := gl.Requests(http.MethodPost, gadgetsAPI("/merge_requests/1/approve"))
	if len(approves) != 1 || approves[0].Token != testApproveToken {
		t.Fatalf("approvals = %d, want 1 by the approve bot", len(approves))
	}

	var run dashboard.RunDetailView
	e.getJSON(t, alice, "/api/runs/"+runID, &run)
	if run.Project != gadgetsPath || run.Run.Outcome != dashboard.OutcomeApproved || !run.Approved {
		t.Errorf("run = %+v, want an approved run of acme/gadgets", run)
	}
	if len(run.Skills) != 1 || len(run.Skills[0].Findings) != 1 {
		t.Fatalf("skills = %+v, want one skill with one finding", run.Skills)
	}
	if f := run.Skills[0].Findings[0]; f.ID != "F1" || f.Severity != "suggestion" || f.File != "counter.go" || f.Line != 12 ||
		f.SuggestedFix != "const maxGadgets = 42" {
		t.Errorf("finding = %+v", f)
	}
	var logView dashboard.LogView
	e.getJSON(t, alice, "/api/runs/"+runID+"/log", &logView)
	if !strings.Contains(logView.Text, "review done") || strings.Count(logView.Text, "[REDACTED]") != 3 {
		t.Errorf("log = %q, want it with three secrets redacted", logView.Text)
	}
	var review dashboard.ReviewDetailView
	e.getJSON(t, alice, "/api/reviews/"+testInstance+"/"+gadgetsPath+"/1", &review)
	if review.Header.Title != "Add the gadget counter" || len(review.Runs) != 1 || review.Runs[0].ID != runID {
		t.Errorf("review = %+v", review)
	}

	// 6. alice turns approval off for acme/gadgets.
	status, body = e.write(t, alice, http.MethodPut, "/api/repositories/"+testInstance+"/"+gadgetsPath+"/settings", map[string]any{"approve": false})
	if status != http.StatusOK {
		t.Fatalf("settings = %d: %s", status, body)
	}
	var detail dashboard.RepositoryDetailView
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	if s := detail.Settings["approve"]; s.Value != false || !s.Overridden {
		t.Errorf("approve setting = %+v, want false, overridden", s)
	}

	// 7. The next cleanup pass withdraws the approval.
	gl.Reset()
	cleanup(t, env)
	unapprovals := gl.Requests(http.MethodPost, gadgetsAPI("/merge_requests/1/unapprove"))
	if len(unapprovals) != 1 || unapprovals[0].Token != testApproveToken {
		t.Fatalf("unapprovals = %d, want 1 by the approve bot", len(unapprovals))
	}
	for _, a := range approvals(t, env) {
		if a.MR.ProjectID == gadgetsID && a.ApprovedSHA != "" {
			t.Errorf("approval of acme/gadgets still recorded: %+v", a)
		}
	}

	// 8. bob, a developer, sees the review but can't change anything.
	bob := e.signIn(t, bobCode)
	if bob.isAdmin {
		t.Fatalf("bob is an admin")
	}
	e.getJSON(t, bob, "/api/runs/"+runID, &run)
	settingsPath := "/api/repositories/" + testInstance + "/" + gadgetsPath
	for _, w := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, settingsPath + "/settings", map[string]any{"approve": true}},
		{http.MethodPost, settingsPath + "/settings/reset", nil},
		{http.MethodDelete, settingsPath, nil},
		{http.MethodPost, "/api/repositories", map[string]string{"project": testProjectPath}},
	} {
		if status, body := e.write(t, bob, w.method, w.path, w.body); status != http.StatusForbidden {
			t.Errorf("bob's %s %s = %d: %s; want 403", w.method, w.path, status, body)
		}
	}
	repo, found, err := env.Store.DashboardRepo(env.ctx, testInstance, gadgetsPath)
	if err != nil || !found || repo.Approve == nil || *repo.Approve {
		t.Errorf("acme/gadgets after bob's writes = %+v, %v, %v; want on with approval off", repo, found, err)
	}
	select {
	case <-env.Engine.reconcileCh():
		t.Errorf("a developer's write asked for a reconcile")
	default:
	}

	// Nothing the dashboard sent holds a token or secret.
	e.mu.Lock()
	sent := strings.Join(e.bodies, "\n")
	e.mu.Unlock()
	for _, secret := range []string{aliceToken, bobToken, dashClientSecret, newDeployToken, testLLMKey, resultToken,
		testReviewToken, testApproveToken, testWebhookSecret} {
		if strings.Contains(sent, secret) {
			t.Errorf("a dashboard response holds %q", secret)
		}
	}
}
