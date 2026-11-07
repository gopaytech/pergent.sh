package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/skillfetch"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

const (
	testDBEnv    = "PERGENT_SH_TEST_DATABASE_URL"
	dashboardURL = "https://pergent.example.com"

	// The instances of the test config: GitLab "main" and GitHub "gh".
	glURL = "https://gitlab.example.com"
	ghURL = "https://github.example.com/api/v3"
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

// testID is the platform ID tests give a project at path when they set none:
// the same for every spelling of it.
func testID(path string) int64 {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(path)))
	return int64(h.Sum32()%1_000_000) + 1000
}

// env is a dashboard over the instances "main" (GitLab) and "gh" (GitHub),
// on a test clock, recording every response and log line.
type env struct {
	t   *testing.T
	st  *store.Store
	db  *pgxpool.Pool // the store's schema, for raw SQL
	cfg *config.Config
	src map[string]*fakeSource  // by instance
	cat map[string]*fakeCatalog // by instance
	// readers are the instances' readers of skill sources.
	readers map[string]*fakeReader
	// reconciles counts the dashboard's ReconcileSoon calls; reconciled
	// lists the instance/project each named.
	reconciles atomic.Int32
	// wakes counts the dashboard's Wake calls.
	wakes      atomic.Int32
	reconciled []string
	clock      *clock
	logs       *lockedBuffer
	key        string // PERGENT_SH_SECRET_KEY
	reg        *instances.Registry
	conn       *fakeConnector
	sources    *syncmap.Map[RepoSource]
	catalogs   *syncmap.Map[RepoCatalog]
	d          *Dashboard
	h          http.Handler

	mu        sync.Mutex
	responses []*httptest.ResponseRecorder
	// seen is the session the test route was last given.
	seen Session
}

// newEnv builds the env. The repositories are acme/widgets and
// acme/payments on main and acme/widgets on gh. There are no teams: a user
// waits until a test puts them on one (team, aliceOn). mutate may change the
// config first.
func newEnv(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	st, db := newStoreDB(t)
	cfg := &config.Config{
		GitLab: []config.Instance{{Name: "main", URL: glURL}},
		GitHub: []config.GitHubInstance{{Name: "gh", APIURL: ghURL}},
		Repositories: []config.Repository{
			{GitLab: "main", Project: "acme/widgets"},
			{GitLab: "main", Project: "acme/payments"},
			{GitHub: "gh", Project: "acme/widgets"},
		},
		Jobs:      config.Jobs{Timeout: 10 * time.Minute, MaxTurns: config.DefaultMaxTurns},
		Dashboard: config.Dashboard{URL: dashboardURL},
	}
	if mutate != nil {
		mutate(cfg)
	}
	e := &env{t: t, st: st, db: db, cfg: cfg, logs: &lockedBuffer{}, key: testKey(t),
		clock:   &clock{t: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)},
		src:     map[string]*fakeSource{"main": newFakeSource(), "gh": newFakeSource()},
		cat:     map[string]*fakeCatalog{"main": {}, "gh": {}},
		readers: map[string]*fakeReader{"main": {repos: map[string]*fakeRepo{}}, "gh": {repos: map[string]*fakeRepo{}}},
	}
	sources := syncmap.New[RepoSource](nil)
	for name, s := range e.src {
		sources.Set(name, s)
	}
	catalogs := syncmap.New[RepoCatalog](nil)
	for name, c := range e.cat {
		catalogs.Set(name, c)
	}
	box, err := secretbox.New(e.key)
	if err != nil {
		t.Fatal(err)
	}
	e.reg, e.sources, e.catalogs = instances.New(cfg, st, box), sources, catalogs
	e.reg.SetLog(slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	e.conn = newFakeConnector(sources, catalogs)
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fetcher := &skillfetch.Fetcher{Store: st, Secrets: e.reg.HeldSecrets, Now: e.clock.now, Log: log,
		Readers: func(instance string) (forge.SourceReader, bool) {
			r, ok := e.readers[instance]
			return r, ok
		}}
	d, err := New(Options{
		Config: cfg, Store: st, SecretKey: e.key, Now: e.clock.now, Sources: sources,
		Instances: e.reg, Connector: e.conn, Fetcher: fetcher,
		Catalogs: catalogs, Wake: func() { e.wakes.Add(1) }, ReconcileSoon: func(instance, project string) {
			e.reconciles.Add(1)
			e.mu.Lock()
			e.reconciled = append(e.reconciled, instance+"/"+project)
			e.mu.Unlock()
		},
		Log: log,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.d = d
	mux := http.NewServeMux()
	mux.Handle("/", d.Handler())
	// A route behind Require that keeps the session it was given.
	mux.Handle("/test/session", d.Auth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		se, ok := SessionFrom(r.Context())
		if !ok {
			http.Error(w, "no session in context", 500)
			return
		}
		e.mu.Lock()
		e.seen = se
		e.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})))
	e.h = mux
	return e
}

// skill adds a skill written in the dashboard to the library, as version 1.
func (e *env) skill(name, content string) {
	e.t.Helper()
	if _, err := e.st.CreateLibrarySkill(ctx, store.NewSkill{Name: name, Content: content, Actor: "seed", At: e.clock.now()}); err != nil {
		e.t.Fatal(err)
	}
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

type meBody struct {
	Kind               string `json:"kind"`
	Username           string `json:"username"`
	Email              string `json:"email"`
	Role               string `json:"role"`
	SignIn             string `json:"signIn"`
	Platform           string `json:"platform"`
	Instance           string `json:"instance"`
	Admin              bool   `json:"admin"`
	Operator           bool   `json:"operator"`
	Waiting            bool   `json:"waiting"`
	MustChangePassword bool   `json:"mustChangePassword"`
	WaitingUsers       int    `json:"waitingUsers"`
	CSRFToken          string `json:"csrfToken"`
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

// session calls the Require'd test route and returns the session it was
// given (none unless 200). Concurrent calls each get one of the sessions.
func (e *env) session(c *http.Cookie) (int, Session) {
	e.t.Helper()
	rec := e.do("GET", "/test/session", []*http.Cookie{c}, nil)
	if rec.Code != 200 {
		return rec.Code, Session{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return rec.Code, e.seen
}

const aliceEmail = "alice@example.com"

// sso returns the SSO account of email, made as a first sign-in makes it (a
// plain user, signed in), so a team can name it.
func (e *env) sso(email string) int64 {
	e.t.Helper()
	a, err := e.st.SignInSSOAccount(ctx, email, e.clock.now())
	if err != nil {
		e.t.Fatal(err)
	}
	return a.ID
}

// alice returns alice's SSO account.
func (e *env) alice() int64 { return e.sso(aliceEmail) }

// admin makes the SSO account of email an admin and returns its ID.
func (e *env) ssoAdmin(email string) int64 {
	e.t.Helper()
	id := e.sso(email)
	if err := e.st.SetAccountRole(ctx, id, store.RoleAdmin); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// sessionFor stores a session of the account (how an OIDC sign-in begins one)
// and returns its cookie. Each call is another session.
func (e *env) sessionFor(account int64) *http.Cookie {
	e.t.Helper()
	id := randomString(sessionIDBytes)
	err := e.st.CreateSession(ctx, store.Session{IDHash: hashID(id), AccountID: account, Username: "x",
		SignIn: store.SignInOIDC, Issuer: "https://issuer.example.com", CSRF: randomString(32),
		LastSeenAt: e.clock.now(), CreatedAt: e.clock.now()})
	if err != nil {
		e.t.Fatal(err)
	}
	return &http.Cookie{Name: SessionCookieName, Value: id}
}

// aliceSession is a session of alice's account.
func (e *env) aliceSession() *http.Cookie { return e.sessionFor(e.alice()) }

// viewerMembers makes viewer members of the accounts ids.
func viewerMembers(ids ...int64) []store.TeamMember {
	out := make([]store.TeamMember, len(ids))
	for i, id := range ids {
		out[i] = store.TeamMember{AccountID: id, Role: store.RoleViewer}
	}
	return out
}

// memberIDsOf are the account IDs of members.
func memberIDsOf(ms []store.TeamMember) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.AccountID
	}
	return out
}

// team stores a team with members and repos and returns its ID.
func (e *env) team(name string, members []store.TeamMember, repos ...store.TeamRepo) int64 {
	e.t.Helper()
	t, err := e.st.CreateTeam(ctx, name, e.clock.now())
	if err != nil {
		e.t.Fatal(err)
	}
	ok, err := e.st.UpdateTeam(ctx, t.ID, func(tm *store.Team) error {
		tm.Members, tm.Repos = members, repos
		return nil
	})
	if err != nil || !ok {
		e.t.Fatalf("UpdateTeam(%s) = %v, %v", name, ok, err)
	}
	return t.ID
}

// editTeam changes team id with edit.
func (e *env) editTeam(id int64, edit func(*store.Team)) {
	e.t.Helper()
	ok, err := e.st.UpdateTeam(ctx, id, func(tm *store.Team) error { edit(tm); return nil })
	if err != nil || !ok {
		e.t.Fatalf("UpdateTeam(%d) = %v, %v", id, ok, err)
	}
}

// aliceOn gives alice the role on every repository of instance: "admin" makes
// her account an admin (and returns 0); "viewer" or "maintainer" puts her on a
// new team with that role covering the instance, whose ID it returns.
func (e *env) aliceOn(instance, role string) int64 {
	e.t.Helper()
	if role == store.RoleAdmin {
		e.ssoAdmin(aliceEmail)
		return 0
	}
	return e.team(role+"s of "+instance, []store.TeamMember{{AccountID: e.alice(), Role: role}}, store.TeamRepo{Instance: instance})
}

// accountOf is the account of a session cookie.
func (e *env) accountOf(c *http.Cookie) int64 {
	e.t.Helper()
	se, found := e.storedSession(c)
	if !found {
		e.t.Fatal("no stored session for the cookie")
	}
	return se.AccountID
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

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
