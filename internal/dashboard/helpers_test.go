package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	testDBEnv    = "PERGENT_SH_TEST_DATABASE_URL"
	dashboardURL = "https://pergent.example.com"

	glClientID     = "gl-client-id"
	glClientSecret = "gl-client-secret-SECRET"
	ghClientID     = "Iv1.gh-client-id"
	ghClientSecret = "gh-client-secret-SECRET"
	goodCode       = "good-code"
)

var ctx = context.Background()

// newStore opens a migrated store in a fresh schema, dropped when the test
// ends. It skips when no test database is set.
func newStore(t *testing.T) *store.Store {
	st, _ := newStoreDB(t)
	return st
}

// newStoreDB is newStore with a raw connection pool on the same schema, for
// tests that set columns no store method sets (a run's times).
func newStoreDB(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	dbURL := os.Getenv(testDBEnv)
	if dbURL == "" {
		t.Skipf("%s is not set", testDBEnv)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "test_dashboard_" + hex.EncodeToString(b)
	st, err := store.OpenInSchema(ctx, dbURL, schema)
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	pcfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		if err := st.DropSchema(context.Background()); err != nil {
			t.Errorf("DropSchema: %v", err)
		}
		st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st, db
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lockedBuffer is a log sink safe for concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// oauthServer is the part of a fake provider that hands out and checks
// tokens. Every token it issues is recorded so tests can look for leaks.
type oauthServer struct {
	mu          sync.Mutex
	prefix      string // "gl" or "gh"
	n           int
	access      string
	refresh     string
	expiresIn   int  // 0: no expiry, no refresh token
	failAPI     bool // every API call is refused (401)
	failAccess  bool // API calls other than GET /user are refused (401)
	failRefresh bool
	tokenCalls  int
	verifiers   []string
	issued      []string
}

// issue makes a new access (and, with an expiry, refresh) token; the old
// ones stop working. Called with mu held.
func (o *oauthServer) issue() map[string]any {
	o.n++
	o.access = fmt.Sprintf("%s-access-%d-SECRET", o.prefix, o.n)
	out := map[string]any{"access_token": o.access, "token_type": "bearer"}
	o.issued = append(o.issued, o.access)
	o.refresh = ""
	if o.expiresIn > 0 {
		o.refresh = fmt.Sprintf("%s-refresh-%d-SECRET", o.prefix, o.n)
		o.issued = append(o.issued, o.refresh)
		out["refresh_token"] = o.refresh
		out["expires_in"] = o.expiresIn
	}
	return out
}

// authorized reports whether r carries the current access token and the
// API is up.
func (o *oauthServer) authorized(r *http.Request) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failAccess && !strings.HasSuffix(r.URL.Path, "/user") {
		return false
	}
	return !o.failAPI && o.access != "" && r.Header.Get("Authorization") == "Bearer "+o.access
}

func (o *oauthServer) set(f func(o *oauthServer)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f(o)
}

func (o *oauthServer) tokens() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.issued...)
}

func (o *oauthServer) calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.tokenCalls
}

func (o *oauthServer) lastVerifier() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.verifiers) == 0 {
		return ""
	}
	return o.verifiers[len(o.verifiers)-1]
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fakeGitLab is a GitLab instance: OAuth, /user, projects and group members.
type fakeGitLab struct {
	oauthServer
	srv      *httptest.Server
	projects map[string]map[string]any // path → GET /projects/:path body
	groups   map[string]bool           // group path → user 9 is a member
	checks   int                       // GET /projects/:path calls
	listings []string                  // GET /projects queries
	// listFail makes GET /projects answer 400; listFailFrom makes pages
	// from listFailFrom on (1-based; 0: none) answer 403.
	listFail     bool
	listFailFrom int
	// slowFrom makes GET /projects pages from slowFrom on (1-based; 0:
	// none) wait slow, or until the request ends.
	slowFrom int
	slow     time.Duration
	// tokenSlow makes POST /oauth/token wait that long first.
	tokenSlow time.Duration
	username  string         // user 9's username (default alice)
	member    map[string]any // extra fields of a member answer
}

func (f *fakeGitLab) setUsername(name string) { f.set(func(*oauthServer) { f.username = name }) }

func (f *fakeGitLab) projectChecks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	f := &fakeGitLab{oauthServer: oauthServer{prefix: "gl", expiresIn: 7200},
		projects: map[string]map[string]any{}, groups: map[string]bool{}, username: "alice"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// setProject sets path's GET /projects/:path body; without an "id" it gets
// testID(path). A "shared_access" level is access through a share: the
// listing counts it, the project's permissions don't show it.
func (f *fakeGitLab) setProject(path string, body map[string]any) {
	f.set(func(*oauthServer) { f.projects[path] = withID(path, body) })
}

// testID is the platform ID the fakes give path when a test sets none: the
// same for every spelling of it.
func testID(path string) int64 {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(path)))
	return int64(h.Sum32()%1_000_000) + 1000
}

// idOf is a fake body's "id" as an int64.
func idOf(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

// withID is a copy of body with an "id", testID(path) unless it has one.
func withID(path string, body map[string]any) map[string]any {
	out := map[string]any{"id": testID(path)}
	for k, v := range body {
		out[k] = v
	}
	return out
}

func (f *fakeGitLab) setMember(group string, member bool) {
	f.set(func(*oauthServer) { f.groups[group] = member })
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/oauth/token" {
		f.mu.Lock()
		slow := f.tokenSlow
		f.mu.Unlock()
		time.Sleep(slow)
		f.token(w, r)
		return
	}
	if !f.authorized(r) {
		reply(w, 401, map[string]any{"message": "401 Unauthorized"})
		return
	}
	p := r.URL.EscapedPath()
	if p == "/api/v4/projects" {
		f.mu.Lock()
		slowFrom, slow := f.slowFrom, f.slow
		f.mu.Unlock()
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if slowFrom > 0 && max(page, 1) >= slowFrom {
			select {
			case <-time.After(slow):
			case <-r.Context().Done():
				return
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case p == "/api/v4/user":
		reply(w, 200, map[string]any{"id": 9, "username": f.username})
	case p == "/api/v4/projects":
		f.listings = append(f.listings, r.URL.RawQuery)
		if f.listFail {
			reply(w, 400, map[string]any{"message": "400 Bad request"})
			return
		}
		if page, _ := strconv.Atoi(r.URL.Query().Get("page")); f.listFailFrom > 0 && max(page, 1) >= f.listFailFrom {
			reply(w, 403, map[string]any{"message": "403 Forbidden"})
			return
		}
		f.listProjects(w, r)
	case strings.HasPrefix(p, "/api/v4/projects/"):
		f.checks++
		path, _ := url.PathUnescape(strings.TrimPrefix(p, "/api/v4/projects/"))
		// GitLab finds a project whatever the case of its path.
		for key, body := range f.projects {
			if strings.EqualFold(key, path) {
				out := map[string]any{"path_with_namespace": key}
				for k, v := range body {
					out[k] = v
				}
				reply(w, 200, out)
				return
			}
		}
		reply(w, 404, map[string]any{"message": "404 Project Not Found"})
	case strings.HasPrefix(p, "/api/v4/groups/") && strings.HasSuffix(p, "/members/all/9"):
		group, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(p, "/api/v4/groups/"), "/members/all/9"))
		if f.groups[group] {
			body := map[string]any{"id": 9, "username": f.username, "access_level": 10, "state": "active", "membership_state": "active"}
			for k, v := range f.member {
				body[k] = v
			}
			reply(w, 200, body)
			return
		}
		reply(w, 404, map[string]any{"message": "404 Not found"})
	default:
		reply(w, 404, map[string]any{"message": "404 Not found"})
	}
}

// listProjects answers GET /projects?min_access_level=N&search=S like
// GitLab: the projects whose member (project or group) access level is at
// least N and whose path holds S, by path, per_page at a time. Called with
// mu held.
func (f *fakeGitLab) listProjects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minLevel, _ := strconv.Atoi(q.Get("min_access_level"))
	search := strings.ToLower(q.Get("search"))
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	if perPage <= 0 {
		perPage = 20
	}
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 1)
	level := func(v any) int {
		m, _ := v.(map[string]any)
		n, _ := m["access_level"].(int)
		return n
	}
	idAfter, _ := strconv.ParseInt(q.Get("id_after"), 10, 64)
	idBefore, _ := strconv.ParseInt(q.Get("id_before"), 10, 64)
	paths := make([]string, 0, len(f.projects))
	for path := range f.projects {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	all := []map[string]any{}
	for _, path := range paths {
		body := f.projects[path]
		perms, _ := body["permissions"].(map[string]any)
		shared, _ := body["shared_access"].(int)
		id := idOf(body["id"])
		if max(level(perms["project_access"]), level(perms["group_access"]), shared) >= minLevel && minLevel > 0 &&
			strings.Contains(strings.ToLower(path), search) &&
			(q.Get("id_after") == "" || id > idAfter) && (q.Get("id_before") == "" || id < idBefore) {
			all = append(all, map[string]any{"id": id, "path_with_namespace": path})
		}
	}
	from := min(len(all), (page-1)*perPage)
	to := min(len(all), page*perPage)
	if to < len(all) {
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
	}
	reply(w, 200, all[from:to])
}

func (f *fakeGitLab) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls++
	if r.Form.Get("client_id") != glClientID || r.Form.Get("client_secret") != glClientSecret {
		reply(w, 401, map[string]any{"error": "invalid_client"})
		return
	}
	wantRedirect := dashboardURL + "/auth/gitlab/main/callback"
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		if r.Form.Get("code") != goodCode || r.Form.Get("redirect_uri") != wantRedirect {
			reply(w, 400, map[string]any{"error": "invalid_grant", "error_description": "echo " + glClientSecret})
			return
		}
		f.verifiers = append(f.verifiers, r.Form.Get("code_verifier"))
		reply(w, 200, f.issue())
	case "refresh_token":
		if f.failRefresh || f.refresh == "" || r.Form.Get("refresh_token") != f.refresh {
			reply(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		reply(w, 200, f.issue())
	default:
		reply(w, 400, map[string]any{"error": "unsupported_grant_type"})
	}
}

// fakeGitHub is GitHub: the web OAuth endpoints and the REST API under
// /api/v3, as on GitHub Enterprise Server.
type fakeGitHub struct {
	oauthServer
	srv   *httptest.Server
	repos map[string]map[string]any // owner/name → GET /repos body
	teams map[string]string         // org/slug/login → membership state
	login string                    // user 7's login (default alice)
}

func (f *fakeGitHub) setLogin(login string) { f.set(func(*oauthServer) { f.login = login }) }

// setTeamFor sets login's membership state of team org/slug.
func (f *fakeGitHub) setTeamFor(team, login, state string) {
	f.set(func(*oauthServer) { f.teams[team+"/"+login] = state })
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{oauthServer: oauthServer{prefix: "gh"},
		repos: map[string]map[string]any{}, teams: map[string]string{}, login: "alice"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// setRepo sets full's GET /repos body; without an "id" it gets
// testID(full).
func (f *fakeGitHub) setRepo(full string, body map[string]any) {
	f.set(func(*oauthServer) { f.repos[full] = withID(full, body) })
}

func (f *fakeGitHub) setTeam(team, state string) {
	f.setTeamFor(team, "alice", state)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/login/oauth/access_token" {
		f.token(w, r)
		return
	}
	if !f.authorized(r) {
		reply(w, 401, map[string]any{"message": "Bad credentials"})
		return
	}
	p := r.URL.EscapedPath()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case p == "/api/v3/user":
		reply(w, 200, map[string]any{"id": 7, "login": f.login})
	case p == "/api/v3/user/repos":
		names := make([]string, 0, len(f.repos))
		for name := range f.repos {
			names = append(names, name)
		}
		sort.Strings(names)
		out := []map[string]any{}
		for _, name := range names {
			body := map[string]any{"full_name": name}
			for k, v := range f.repos[name] {
				body[k] = v
			}
			out = append(out, body)
		}
		reply(w, 200, out)
	case strings.HasPrefix(p, "/api/v3/repos/"):
		// GitHub finds a repository whatever the case of its name.
		for key, body := range f.repos {
			if strings.EqualFold(key, strings.TrimPrefix(p, "/api/v3/repos/")) {
				out := map[string]any{"full_name": key}
				for k, v := range body {
					out[k] = v
				}
				reply(w, 200, out)
				return
			}
		}
		reply(w, 404, map[string]any{"message": "Not Found"})
	case strings.HasPrefix(p, "/api/v3/orgs/") && strings.Contains(p, "/memberships/"):
		parts := strings.Split(strings.TrimPrefix(p, "/api/v3/orgs/"), "/") // org teams slug memberships login
		if state, ok := f.teams[parts[0]+"/"+parts[2]+"/"+parts[4]]; ok {
			reply(w, 200, map[string]any{"state": state, "role": "member"})
			return
		}
		reply(w, 404, map[string]any{"message": "Not Found"})
	default:
		reply(w, 404, map[string]any{"message": "Not Found"})
	}
}

// token answers like GitHub: errors are 200 with an "error" field.
func (f *fakeGitHub) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls++
	if r.Form.Get("client_id") != ghClientID || r.Form.Get("client_secret") != ghClientSecret {
		reply(w, 200, map[string]any{"error": "incorrect_client_credentials"})
		return
	}
	switch {
	case r.Form.Get("grant_type") == "refresh_token":
		if f.failRefresh || f.refresh == "" || r.Form.Get("refresh_token") != f.refresh {
			reply(w, 200, map[string]any{"error": "bad_refresh_token"})
			return
		}
		reply(w, 200, f.issue())
	case r.Form.Get("code") == goodCode && r.Form.Get("redirect_uri") == dashboardURL+"/auth/github/gh/callback":
		f.verifiers = append(f.verifiers, r.Form.Get("code_verifier"))
		reply(w, 200, f.issue())
	default:
		reply(w, 200, map[string]any{"error": "bad_verification_code"})
	}
}

// env is a dashboard against fake GitLab "main" and GitHub "gh" instances,
// on a test clock, recording every response and log line.
type env struct {
	t   *testing.T
	st  *store.Store
	db  *pgxpool.Pool // the store's schema, for raw SQL
	cfg *config.Config
	lib inputs.Library
	src map[string]*fakeSource  // by instance
	cat map[string]*fakeCatalog // by instance
	// reconciles counts the dashboard's ReconcileSoon calls.
	reconciles atomic.Int32
	gl         *fakeGitLab
	gh         *fakeGitHub
	clock      *clock
	logs       *lockedBuffer
	key        string // PERGENT_SH_SECRET_KEY
	d          *Dashboard
	h          http.Handler

	mu        sync.Mutex
	responses []*httptest.ResponseRecorder
}

// newEnv builds the env. The repositories are acme/widgets and
// acme/payments on main and acme/widgets on gh; the admins are the group
// acme/platform on main and the team acme/platform on gh. mutate may change
// the config first.
func newEnv(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	st, db := newStoreDB(t)
	gl, gh := newFakeGitLab(t), newFakeGitHub(t)
	t.Setenv("PERGENT_TEST_GL_ID", glClientID)
	t.Setenv("PERGENT_TEST_GL_SECRET", glClientSecret)
	t.Setenv("PERGENT_TEST_GH_ID", ghClientID)
	t.Setenv("PERGENT_TEST_GH_SECRET", ghClientSecret)
	cfg := &config.Config{
		GitLab: []config.Instance{{Name: "main", URL: gl.srv.URL,
			OAuthClientIDEnv: "PERGENT_TEST_GL_ID", OAuthClientSecretEnv: "PERGENT_TEST_GL_SECRET"}},
		GitHub: []config.GitHubInstance{{Name: "gh", APIURL: gh.srv.URL + "/api/v3",
			OAuthClientIDEnv: "PERGENT_TEST_GH_ID", OAuthClientSecretEnv: "PERGENT_TEST_GH_SECRET"}},
		Repositories: []config.Repository{
			{GitLab: "main", Project: "acme/widgets"},
			{GitLab: "main", Project: "acme/payments"},
			{GitHub: "gh", Project: "acme/widgets"},
		},
		Jobs: config.Jobs{Timeout: 10 * time.Minute},
		Dashboard: config.Dashboard{URL: dashboardURL, Admins: []config.DashboardAdmin{
			{Instance: "main", Group: "acme/platform"},
			{Instance: "gh", Team: "acme/platform"},
		}},
	}
	if mutate != nil {
		mutate(cfg)
	}
	e := &env{t: t, st: st, db: db, cfg: cfg, gl: gl, gh: gh, logs: &lockedBuffer{}, key: testKey(t),
		clock: &clock{t: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)},
		lib: inputs.Library{
			"code-review": []byte("---\nname: code-review\n---\nReview the code.\n"),
			"style":       []byte("---\nname: style\n---\nCheck the style.\n"),
		},
		src: map[string]*fakeSource{"main": newFakeSource(), "gh": newFakeSource()},
		cat: map[string]*fakeCatalog{"main": {}, "gh": {}},
	}
	sources := map[string]RepoSource{}
	for name, s := range e.src {
		sources[name] = s
	}
	catalogs := map[string]RepoCatalog{}
	for name, c := range e.cat {
		catalogs[name] = c
	}
	d, err := New(Options{
		Config: cfg, Store: st, SecretKey: e.key, Now: e.clock.now, Library: e.lib, Sources: sources,
		Catalogs: catalogs, ReconcileSoon: func() { e.reconciles.Add(1) },
		Log: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.d = d
	mux := http.NewServeMux()
	mux.Handle("/", d.Handler())
	// A route behind Require that shows the session it was given.
	mux.Handle("/test/session", d.Auth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		se, ok := SessionFrom(r.Context())
		if !ok {
			http.Error(w, "no session in context", 500)
			return
		}
		reply(w, 200, se)
	})))
	e.h = mux
	return e
}

func testKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

// do serves one request and records the response.
func (e *env) do(method, target string, cookies []*http.Cookie, header map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	e.mu.Lock()
	e.responses = append(e.responses, rec)
	e.mu.Unlock()
	return rec
}

// cookie returns the cookie name set by rec, or nil.
func cookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// startLogin calls the login route and returns the authorize URL and the
// state cookie.
func (e *env) startLogin(platform, instance string) (*url.URL, *http.Cookie) {
	e.t.Helper()
	rec := e.do("GET", "/auth/"+platform+"/"+instance+"/login", nil, nil)
	if rec.Code != http.StatusFound {
		e.t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	sc := cookie(rec, stateCookieName)
	if sc == nil {
		e.t.Fatal("login set no state cookie")
	}
	return loc, sc
}

// callback calls the callback route with code and state and the given
// cookies.
func (e *env) callback(platform, instance, code, state string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	q := url.Values{"code": {code}, "state": {state}}
	return e.do("GET", "/auth/"+platform+"/"+instance+"/callback?"+q.Encode(), cookies, nil)
}

// signIn signs alice in on platform/instance and returns her session
// cookie. extra cookies (an old session) go with the callback.
func (e *env) signIn(platform, instance string, extra ...*http.Cookie) *http.Cookie {
	e.t.Helper()
	loc, sc := e.startLogin(platform, instance)
	rec := e.callback(platform, instance, goodCode, loc.Query().Get("state"), append([]*http.Cookie{sc}, extra...)...)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		e.t.Fatalf("callback = %d Location %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	c := cookie(rec, SessionCookieName)
	if c == nil {
		e.t.Fatal("callback set no session cookie")
	}
	return c
}

type meBody struct {
	Platform  string `json:"platform"`
	Instance  string `json:"instance"`
	Username  string `json:"username"`
	IsAdmin   bool   `json:"isAdmin"`
	CSRFToken string `json:"csrfToken"`
}

func (e *env) me(c *http.Cookie) (int, meBody) {
	e.t.Helper()
	rec := e.do("GET", "/api/me", []*http.Cookie{c}, nil)
	var m meBody
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			e.t.Fatalf("/api/me body %s: %v", rec.Body, err)
		}
	}
	return rec.Code, m
}

// session calls the Require'd test route.
func (e *env) session(c *http.Cookie) (int, Session) {
	e.t.Helper()
	rec := e.do("GET", "/test/session", []*http.Cookie{c}, nil)
	var se Session
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &se); err != nil {
			e.t.Fatalf("session body %s: %v", rec.Body, err)
		}
	}
	return rec.Code, se
}

// hashOf is the stored hash of a session cookie.
func hashOf(t *testing.T, c *http.Cookie) []byte {
	t.Helper()
	id, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		t.Fatalf("session cookie %q is not base64url: %v", c.Value, err)
	}
	h := sha256.Sum256(id)
	return h[:]
}

func (e *env) storedSession(c *http.Cookie) (store.Session, bool) {
	e.t.Helper()
	se, found, err := e.st.Session(ctx, hashOf(e.t, c))
	if err != nil {
		e.t.Fatal(err)
	}
	return se, found
}

// gitlabProject is a GET /projects/:path body.
func gitlabProject(visibility string, projectLevel, groupLevel int) map[string]any {
	perm := func(level int) any {
		if level == 0 {
			return nil
		}
		return map[string]any{"access_level": level, "notification_level": 3}
	}
	return map[string]any{
		"visibility":              visibility,
		"repository_access_level": "enabled", "merge_requests_access_level": "enabled",
		"permissions": map[string]any{"project_access": perm(projectLevel), "group_access": perm(groupLevel)},
	}
}

// githubRepo is a GET /repos body seen with a user token.
func githubRepo(private, pull bool) map[string]any {
	return map[string]any{"private": private,
		"permissions": map[string]any{"admin": false, "push": false, "pull": pull}}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
