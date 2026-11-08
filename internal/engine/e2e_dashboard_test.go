package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// Values of the dashboard end-to-end test. None of them is a real secret.
const (
	dashURL = "https://dashboard.example.com"

	// The people of the test, SSO accounts: alice is made an admin, bob is
	// put on a viewer team, carol on a viewer team of another repository.
	aliceEmail, bobEmail, carolEmail = "alice@example.com", "bob@example.com", "carol@example.com"

	// The first password admin: the env var holds its password, which it
	// changes at its first sign-in.
	adminPasswordEnv  = "PERGENT_SH_E2E_ADMIN_PASSWORD"
	bootstrapPassword = "bootstrap-password-example"
	changedPassword   = "changed-password-example"

	// acme/gadgets is the repository the admin switches on in the
	// dashboard; acme/payments is in the config, and no team covers it.
	gadgetsID    = int64(9)
	gadgetsPath  = "acme/gadgets"
	paymentsID   = int64(8)
	paymentsPath = "acme/payments"
)

// dashE2E is e2e with the dashboard on: the routes main serves (webhooks,
// result callback, /auth/, /api/ and the app) with the real store and
// dashboard, in front of a testEnv whose fake GitLab answers the review bot.
type dashE2E struct {
	*e2e
	// client never follows redirects: the test reads them.
	client *http.Client
	dash   *dashboard.Dashboard

	mu     sync.Mutex
	bodies []string // every response body the dashboard sent
}

// newDashboardE2E builds the dashboard e2e; edit, when given, changes the
// dashboard's options before it is built.
func newDashboardE2E(t *testing.T, edit ...func(o *dashboard.Options)) *dashE2E {
	env := newTestEnv(t)
	cfg := env.Engine.Config
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
	sources := syncmap.New[dashboard.RepoSource](nil)
	catalogs := syncmap.New[dashboard.RepoCatalog](nil)
	for _, name := range env.Engine.Platforms.Keys() {
		p, _ := env.Engine.Platforms.Get(name)
		sources.Set(name, p.Forge.(dashboard.RepoSource))
		catalogs.Set(name, p.Forge.(dashboard.RepoCatalog))
	}
	opts := dashboard.Options{
		Config: cfg, Instances: env.Engine.Instances, Store: env.Store, SecretKey: testBoxKey,
		Now: env.Now, Log: env.Engine.Log,
		Sources: sources, Catalogs: catalogs, ReconcileSoon: env.Engine.ReconcileSoon,
		// The readiness scans are the engine's; they run only when a test
		// turns env.Engine.Scans on, as main does with the dashboard.
		Readiness: env.Engine,
	}
	for _, f := range edit {
		f(&opts)
	}
	d, err := dashboard.New(opts)
	if err != nil {
		t.Fatalf("dashboard.New: %v", err)
	}
	mux := server.Routes(server.Deps{
		Config: cfg, Instances: env.Engine.Instances, WebhookSecrets: env.Engine.WebhookSecrets, Store: env.Store,
		Wake: env.Engine.Wake, Now: env.Now, Dashboard: d.Handler(), ReadinessScans: env.Engine, Log: env.Engine.Log,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &dashE2E{
		e2e: &e2e{env: env, srv: srv}, dash: d,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

// dashAccess is the GitLab access level of the review bot in each project; 0
// is no access (GitLab answers 404).
var dashAccess = map[string]map[string]int{
	testReviewToken: {testProjectPath: 40, paymentsPath: 40, gadgetsPath: 40},
}

var dashProjectIDs = map[string]int64{testProjectPath: testProjectID, paymentsPath: paymentsID, gadgetsPath: gadgetsID}

// serveDashboardGitLab makes env's fake GitLab answer GET /projects/:path for
// the review bot, and set up the webhooks and deploy tokens of every project.
func serveDashboardGitLab(t *testing.T, env *testEnv) {
	gl := env.GitLab
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	bearer := func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }

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

	// GET /projects lists the projects the token's user has min_access_level
	// on, narrowed by id_after and id_before as GitLab does: switch-on asks it
	// for the one project's ID.
	gl.Handle(http.MethodGet, "/projects", func(w http.ResponseWriter, r *http.Request) {
		levels, ok := dashAccess[r.Header.Get("PRIVATE-TOKEN")]
		if !ok {
			reply(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
			return
		}
		q := r.URL.Query()
		num := func(key string, def int64) int64 {
			if n, err := strconv.ParseInt(q.Get(key), 10, 64); err == nil {
				return n
			}
			return def
		}
		minLevel, after, before := num("min_access_level", 1), num("id_after", 0), num("id_before", math.MaxInt64)
		page := []map[string]any{}
		for path, id := range dashProjectIDs {
			if level := int64(levels[path]); level > 0 && level >= minLevel && id > after && id < before {
				page = append(page, map[string]any{"id": id, "path_with_namespace": path})
			}
		}
		reply(w, http.StatusOK, page)
	})
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
	// waiting: an account on no team.
	waiting bool
}

// signIn is what an OIDC sign-in leaves, made directly in the store (the
// sign-in itself needs an identity provider): the SSO account of email, as a
// first sign-in makes it, and a session of it.
func (e *dashE2E) signIn(t *testing.T, email string) dashUser {
	t.Helper()
	acc, err := e.env.Store.SignInSSOAccount(e.env.ctx, email, e.env.Now())
	if err != nil {
		t.Fatal(err)
	}
	u := randomSession(t, e.env, store.Session{AccountID: acc.ID, Username: email, SignIn: store.SignInOIDC,
		Issuer: "https://issuer.example.com"})
	var me struct {
		Username  string `json:"username"`
		Admin     bool   `json:"admin"`
		Waiting   bool   `json:"waiting"`
		CSRFToken string `json:"csrfToken"`
	}
	e.getJSON(t, u, "/api/me", &me)
	if me.Username != email || me.CSRFToken != u.csrf {
		t.Fatalf("/api/me of %s = %+v", email, me)
	}
	u.isAdmin, u.waiting = me.Admin, me.Waiting
	return u
}

// startSession reads the session a sign-in set in cookies, checks its
// attributes and asks who it is.
func (e *dashE2E) startSession(t *testing.T, cookies []*http.Cookie) dashUser {
	t.Helper()
	var u dashUser
	for _, c := range cookies {
		if c.Name == dashboard.SessionCookieName && c.Value != "" {
			u.session = c
		}
	}
	if u.session == nil || !u.session.HttpOnly || !u.session.Secure || u.session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("sign-in set session cookie %+v; want HttpOnly, Secure, SameSite=Lax", u.session)
	}
	var me struct {
		Username  string `json:"username"`
		Admin     bool   `json:"admin"`
		Waiting   bool   `json:"waiting"`
		CSRFToken string `json:"csrfToken"`
	}
	e.getJSON(t, u, "/api/me", &me)
	if me.CSRFToken == "" {
		t.Fatalf("/api/me has no CSRF token")
	}
	u.csrf, u.isAdmin, u.waiting = me.CSRFToken, me.Admin, me.Waiting
	return u
}

// tryPassword sends a password sign-in as the browser does: it fetches the
// form's CSRF token, then posts the credentials with it. It returns the
// status, the body and the cookies of the sign-in itself.
func (e *dashE2E) tryPassword(t *testing.T, username, pw string) (int, []byte, []*http.Cookie) {
	t.Helper()
	status, body, _, cookies := e.do(t, http.MethodGet, "/auth/csrf", nil, nil, nil)
	var form struct {
		CSRFToken string `json:"csrfToken"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &form) != nil || form.CSRFToken == "" {
		t.Fatalf("GET /auth/csrf = %d: %s", status, body)
	}
	status, body, _, cookies = e.do(t, http.MethodPost, "/auth/password", map[string]string{"username": username, "password": pw},
		cookies, map[string]string{dashboard.CSRFHeader: form.CSRFToken, "Content-Type": "application/json"})
	return status, body, cookies
}

// passwordSignIn signs a password account in.
func (e *dashE2E) passwordSignIn(t *testing.T, username, pw string) dashUser {
	t.Helper()
	status, body, cookies := e.tryPassword(t, username, pw)
	if status != http.StatusNoContent {
		t.Fatalf("password sign-in of %s = %d: %s", username, status, body)
	}
	return e.startSession(t, cookies)
}

// expectStatus fails the test unless a request as u answers want.
func (e *dashE2E) expectStatus(t *testing.T, u dashUser, method, path string, want int) []byte {
	t.Helper()
	status, body, _, _ := e.do(t, method, path, nil, []*http.Cookie{u.session}, nil)
	if status != want {
		t.Fatalf("%s %s = %d: %s; want %d", method, path, status, body, want)
	}
	return body
}

// accountID is the ID of the SSO account of email in the users list.
func (e *dashE2E) accountID(t *testing.T, admin dashUser, email string) int64 {
	t.Helper()
	var users []dashboard.UserView
	e.getJSON(t, admin, "/api/users", &users)
	for _, u := range users {
		if u.Email == email && u.Kind == store.AccountSSO {
			return u.ID
		}
	}
	t.Fatalf("no SSO account %q in %+v", email, users)
	return 0
}

// makeAdmin has admin make the SSO account of email an admin.
func (e *dashE2E) makeAdmin(t *testing.T, admin dashUser, email string) {
	t.Helper()
	status, body := e.write(t, admin, http.MethodPut, "/api/admin/users/"+strconv.FormatInt(e.accountID(t, admin, email), 10)+"/role",
		map[string]string{"role": store.RoleAdmin})
	if status != http.StatusNoContent {
		t.Fatalf("making %s an admin = %d: %s", email, status, body)
	}
}

// newTeam creates a team through the API and returns its ID.
func (e *dashE2E) newTeam(t *testing.T, admin dashUser, name string) int64 {
	t.Helper()
	status, body := e.write(t, admin, http.MethodPost, "/api/teams", map[string]string{"name": name})
	var team dashboard.TeamView
	if status != http.StatusCreated || json.Unmarshal(body, &team) != nil || team.ID == 0 {
		t.Fatalf("creating team %s = %d: %s", name, status, body)
	}
	return team.ID
}

// teamMember is a member in a team change: an account and its role there.
type teamMember struct {
	AccountID int64  `json:"accountId"`
	Role      string `json:"role"`
}

// setTeam replaces a team's members and repositories through the API.
func (e *dashE2E) setTeam(t *testing.T, admin dashUser, id int64, name string, members []teamMember, repos []dashboard.TeamRepoView) {
	t.Helper()
	if members == nil {
		members = []teamMember{}
	}
	if repos == nil {
		repos = []dashboard.TeamRepoView{}
	}
	status, body := e.write(t, admin, http.MethodPut, "/api/teams/"+strconv.FormatInt(id, 10), map[string]any{
		"name": name, "members": members, "repos": repos,
	})
	if status != http.StatusOK {
		t.Fatalf("changing team %s = %d: %s", name, status, body)
	}
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

// The operator signs in with the first password admin, changes its password
// and makes an SSO account an admin; users who sign in wait until they are put
// on a team, then see only what their team covers. An admin switches a
// repository on, which is set up and reviewed, and turns its approval off,
// which withdraws the approval; a viewer can't write.
func TestEndToEndDashboard(t *testing.T) {
	e := newDashboardE2E(t)
	env := e.env
	gl := env.GitLab

	// Earlier reviews: one in acme/widgets and one in acme/payments. Each has
	// a setup problem.
	seedPublished(t, env, testProjectID, testProjectPath, 1, sha('1'))
	seedPublished(t, env, paymentsID, paymentsPath, 2, sha('2'))
	for _, p := range []string{testProjectPath, paymentsPath} {
		if err := env.Store.SetAttention(env.ctx, testInstance, p, attSingleApproval, "One approval is enough to merge in "+p+"."); err != nil {
			t.Fatal(err)
		}
	}

	// 1. The first start makes the password admin from the env var. Its
	// first sign-in must change the password before anything else works.
	t.Setenv(adminPasswordEnv, bootstrapPassword)
	env.Engine.Config.Dashboard.AdminPasswordEnv = adminPasswordEnv
	if err := dashboard.Bootstrap(env.ctx, env.Store, env.Engine.Config, env.Now(), env.Engine.Log); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if status, body, _ := e.tryPassword(t, "admin", "not-the-password-example"); status != http.StatusUnauthorized ||
		!strings.Contains(string(body), "wrong username or password") {
		t.Fatalf("a wrong password = %d: %s, want 401 wrong username or password", status, body)
	}
	operator := e.passwordSignIn(t, "admin", bootstrapPassword)
	if !operator.isAdmin {
		t.Fatalf("the password admin is not an admin")
	}
	if body := e.expectStatus(t, operator, http.MethodGet, "/api/overview?days=7", http.StatusForbidden); !strings.Contains(string(body), "password_change_required") {
		t.Errorf("before the change the overview says %s, want password_change_required", body)
	}
	if status, body := e.write(t, operator, http.MethodPost, "/auth/password/change",
		map[string]string{"current": bootstrapPassword, "new": changedPassword}); status != http.StatusNoContent {
		t.Fatalf("password change = %d: %s", status, body)
	}
	e.expectStatus(t, operator, http.MethodGet, "/api/overview?days=7", http.StatusOK)
	if status, _, _ := e.tryPassword(t, "admin", bootstrapPassword); status != http.StatusUnauthorized {
		t.Errorf("the first password still signs in (%d)", status)
	}

	// alice signs in and waits: she is on no team.
	alice := e.signIn(t, aliceEmail)
	if !alice.waiting || alice.isAdmin {
		t.Fatalf("alice before any team: waiting=%v admin=%v, want waiting and not an admin", alice.waiting, alice.isAdmin)
	}
	if body := e.expectStatus(t, alice, http.MethodGet, "/api/overview?days=7", http.StatusForbidden); !strings.Contains(string(body), "an admin needs to add you to a team") {
		t.Errorf("a waiting user gets %s", body)
	}
	// The operator makes her an admin: no team needed.
	e.makeAdmin(t, operator, aliceEmail)
	var me struct {
		Admin   bool `json:"admin"`
		Waiting bool `json:"waiting"`
	}
	e.getJSON(t, alice, "/api/me", &me)
	if !me.Admin || me.Waiting {
		t.Fatalf("alice after being made an admin: %+v, want an admin who isn't waiting", me)
	}
	alice.isAdmin, alice.waiting = true, false

	// 2. An admin's overview shows every repository; a viewer's only what
	// their team covers.
	var overview dashboard.OverviewView
	e.getJSON(t, alice, "/api/overview", &overview)
	var recent []string
	for _, r := range overview.Recent {
		recent = append(recent, r.Project+"#"+strconv.FormatInt(r.Number, 10))
	}
	slices.Sort(recent)
	if !slices.Equal(recent, []string{paymentsPath + "#2", testProjectPath + "#1"}) {
		t.Errorf("an admin's recent reviews = %v, want both", recent)
	}
	if len(overview.Attention) != 2 || overview.Stats.Reviews != 2 {
		t.Errorf("an admin's attention = %+v, stats = %+v; want both repositories", overview.Attention, overview.Stats)
	}
	carol := e.signIn(t, carolEmail)
	widgetsViewers := e.newTeam(t, operator, "widgets viewers")
	e.setTeam(t, operator, widgetsViewers, "widgets viewers", []teamMember{{e.accountID(t, operator, carolEmail), store.RoleViewer}},
		[]dashboard.TeamRepoView{{Instance: testInstance, Project: testProjectPath}})
	e.getJSON(t, carol, "/api/overview", &overview)
	if len(overview.Recent) != 1 || overview.Recent[0].Project != testProjectPath || overview.Recent[0].Number != 1 {
		t.Errorf("carol's recent reviews = %+v, want only acme/widgets !1", overview.Recent)
	}
	if len(overview.Attention) != 1 || overview.Attention[0].Project != testProjectPath {
		t.Errorf("carol's attention = %+v, want only acme/widgets", overview.Attention)
	}
	if overview.Stats.Reviews != 1 {
		t.Errorf("carol's stats = %+v, want 1 review", overview.Stats)
	}
	for _, path := range []string{"/api/repositories/" + testInstance + "/" + paymentsPath, "/api/reviews/" + testInstance + "/" + paymentsPath + "/2"} {
		if status, _, _, _ := e.do(t, http.MethodGet, path, nil, []*http.Cookie{carol.session}, nil); status != http.StatusNotFound {
			t.Errorf("carol's GET %s = %d, want 404", path, status)
		}
	}

	// 3. alice switches acme/gadgets on; the engine is asked to reconcile.
	if status, body := e.write(t, alice, http.MethodPost, "/api/repositories", map[string]string{"instance": testInstance, "project": gadgetsPath}); status != http.StatusCreated {
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

	// 8. bob signs in and waits; the operator puts him on a viewer team
	// that covers acme/gadgets, which he then sees but can't change.
	bob := e.signIn(t, bobEmail)
	if !bob.waiting || bob.isAdmin {
		t.Fatalf("bob before any team: waiting=%v admin=%v", bob.waiting, bob.isAdmin)
	}
	e.expectStatus(t, bob, http.MethodGet, "/api/runs/"+runID, http.StatusForbidden)
	developers := e.newTeam(t, operator, "developers")
	e.setTeam(t, operator, developers, "developers", []teamMember{{e.accountID(t, operator, bobEmail), store.RoleViewer}},
		[]dashboard.TeamRepoView{{Instance: testInstance, Project: gadgetsPath}})
	e.getJSON(t, bob, "/api/runs/"+runID, &run)
	if run.Project != gadgetsPath {
		t.Errorf("bob sees run %+v, want a run of acme/gadgets", run)
	}
	// His team covers acme/gadgets alone: acme/widgets and acme/payments are
	// not his.
	e.expectStatus(t, bob, http.MethodGet, "/api/repositories/"+testInstance+"/"+testProjectPath, http.StatusNotFound)
	e.expectStatus(t, bob, http.MethodGet, "/api/repositories/"+testInstance+"/"+paymentsPath, http.StatusNotFound)
	settingsPath := "/api/repositories/" + testInstance + "/" + gadgetsPath
	// Settings are his team's to change by role (403 for a viewer); switching
	// a repository on or off is an admin route, which answers "not found".
	for _, w := range []struct {
		method, path string
		body         any
		want         int
	}{
		{http.MethodPut, settingsPath + "/settings", map[string]any{"approve": true}, http.StatusForbidden},
		{http.MethodPost, settingsPath + "/settings/reset", nil, http.StatusForbidden},
		{http.MethodDelete, settingsPath, nil, http.StatusNotFound},
		{http.MethodPost, "/api/repositories", map[string]string{"instance": testInstance, "project": testProjectPath}, http.StatusNotFound},
	} {
		if status, body := e.write(t, bob, w.method, w.path, w.body); status != w.want {
			t.Errorf("bob's %s %s = %d: %s; want %d", w.method, w.path, status, body, w.want)
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

	// The audit log tells who did what, and holds no password either.
	var audit dashboard.AuditPageView
	e.getJSON(t, operator, "/api/audit", &audit)
	actions := map[string]bool{}
	for _, entry := range audit.Entries {
		actions[entry.Action] = true
	}
	for _, want := range []string{"sign_in", "sign_in_failed", "password_changed", "account_role_changed", "team_created", "team_updated"} {
		if !actions[want] {
			t.Errorf("the audit log has no %s entry: %+v", want, audit.Entries)
		}
	}
	// Nothing the dashboard sent holds a token or secret.
	e.mu.Lock()
	sent := strings.Join(e.bodies, "\n")
	e.mu.Unlock()
	logged := env.Logs()
	for _, secret := range []string{newDeployToken, testLLMKey, resultToken,
		testReviewToken, testApproveToken, testWebhookSecret, bootstrapPassword, changedPassword} {
		if strings.Contains(sent, secret) {
			t.Errorf("a dashboard response holds %q", secret)
		}
		if strings.Contains(logged, secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
}

// A dismissal in the dashboard decides the review again from the run, not the
// setting: a quiet run whose repository has since turned quiet off is
// republished with no Files list, says quiet and lists the dismissal, and its
// thread says who dismissed the finding.
func TestDismissRepublishKeepsTheRunsShape(t *testing.T) {
	e := newDashboardE2E(t)
	env := e.env
	t.Setenv(adminPasswordEnv, bootstrapPassword)
	env.Engine.Config.Dashboard.AdminPasswordEnv = adminPasswordEnv
	if err := dashboard.Bootstrap(env.ctx, env.Store, env.Engine.Config, env.Now(), env.Engine.Log); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	operator := e.passwordSignIn(t, "admin", bootstrapPassword)
	if status, body := e.write(t, operator, http.MethodPost, "/auth/password/change",
		map[string]string{"current": bootstrapPassword, "new": changedPassword}); status != http.StatusNoContent {
		t.Fatalf("password change = %d: %s", status, body)
	}

	// The fake GitLab of the inline tests: the diff, the comparison, the
	// discussions and the project of merge request 1.
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, map[string]any{"id": createdNote})
	env.GitLab.Reply(http.MethodGet, projectPath(), http.StatusOK, map[string]any{"id": testProjectID})
	env.GitLab.ServeCompare()
	disc := env.GitLab.ServeDiscussions(t, 1)
	ie := &inlineEnv{env: env, disc: disc}

	// A quiet run, published while the repository was quiet.
	head := sha('a')
	servePublish(env, 1, head)
	env.GitLab.ServeDiff(1, head, queryFile)
	quietOn(env, true)
	reportQuiet(t, env, 1, head, resultFor(t, head, quietPair()), boolPtr(true))
	publish(t, env)
	first := ie.comment(t)
	wantQuietComment(t, first, true)
	if n := len(disc.Posted()); n != 1 {
		t.Fatalf("%d discussions started, want the blocking finding's alone", n)
	}

	// The setting is turned off; then the blocking finding is dismissed.
	quietOn(env, false)
	status, body := e.write(t, operator, http.MethodPost, "/api/reviews/"+testInstance+"/"+testProjectPath+"/1/dismiss",
		map[string]string{"fingerprint": fpBlocking, "reason": "the input is a constant"})
	if status != http.StatusOK || !strings.Contains(string(body), "dismissed") {
		t.Fatalf("dismiss = %d: %s", status, body)
	}
	publish(t, env)

	second := ie.comment(t)
	if second == first {
		t.Fatalf("the comment was not written again after the dismissal:\n%s", second)
	}
	wantQuietComment(t, second, true)
	for _, want := range []string{"Dismissed (1)", "the input is a constant", "Title of bbb"} {
		if !strings.Contains(second, want) {
			t.Errorf("comment after the dismissal lacks %q:\n%s", want, second)
		}
	}
	replies := disc.Replies(t)
	if len(replies) != 1 || !strings.HasPrefix(replies[0], "Dismissed by ") || !strings.Contains(replies[0], "the input is a constant") {
		t.Errorf("replies = %q, want one saying who dismissed the finding", replies)
	}
	if n := len(disc.Posted()); n != 1 {
		t.Errorf("%d discussions started, want no new thread after the dismissal", n)
	}
}

// A dismissal in the dashboard decides the review again from the run, not the
// setting: a run that recorded a findings-only layout, whose repository has
// since cleared it, is republished with the same shape. The Dismissed block is
// not in that layout, so it is not written either.
func TestDismissRepublishKeepsTheLayout(t *testing.T) {
	e := newDashboardE2E(t)
	env := e.env
	t.Setenv(adminPasswordEnv, bootstrapPassword)
	env.Engine.Config.Dashboard.AdminPasswordEnv = adminPasswordEnv
	if err := dashboard.Bootstrap(env.ctx, env.Store, env.Engine.Config, env.Now(), env.Engine.Log); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	operator := e.passwordSignIn(t, "admin", bootstrapPassword)
	if status, body := e.write(t, operator, http.MethodPost, "/auth/password/change",
		map[string]string{"current": bootstrapPassword, "new": changedPassword}); status != http.StatusNoContent {
		t.Fatalf("password change = %d: %s", status, body)
	}

	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, map[string]any{"id": createdNote})
	env.GitLab.Reply(http.MethodGet, projectPath(), http.StatusOK, map[string]any{"id": testProjectID})
	env.GitLab.ServeCompare()
	disc := env.GitLab.ServeDiscussions(t, 1)
	ie := &inlineEnv{env: env, disc: disc}

	// A run that recorded the layout, published while the repository had it.
	head := sha('a')
	servePublish(env, 1, head)
	env.GitLab.ServeDiff(1, head, queryFile)
	layoutOn(env, layoutFindingsOnly...)
	reportLayout(t, env, 1, head, resultFor(t, head, quietPair()), layoutFindingsOnly)
	publish(t, env)
	first := ie.comment(t)
	wantFindingsOnlyComment(t, first)

	// The setting is cleared; then the blocking finding is dismissed.
	layoutOn(env)
	status, body := e.write(t, operator, http.MethodPost, "/api/reviews/"+testInstance+"/"+testProjectPath+"/1/dismiss",
		map[string]string{"fingerprint": fpBlocking, "reason": "the input is a constant"})
	if status != http.StatusOK || !strings.Contains(string(body), "dismissed") {
		t.Fatalf("dismiss = %d: %s", status, body)
	}
	publish(t, env)

	second := ie.comment(t)
	if second == first {
		t.Fatalf("the comment was not written again after the dismissal:\n%s", second)
	}
	wantFindingsOnlyComment(t, second)
	if strings.Contains(second, "Dismissed (") {
		t.Errorf("a findings-only layout writes the Dismissed block:\n%s", second)
	}
}
