package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/engine"
	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/forge/gitlabforge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/server"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// newConnector is a connector over cfg (and st, which may be nil), with
// platform clients on hc (nil: the default) and no dashboard.
func newConnector(t *testing.T, cfg *config.Config, hc *http.Client, st *store.Store) *connector {
	t.Helper()
	box, err := secretbox.New(testSecretKey)
	if err != nil {
		t.Fatal(err)
	}
	return &connector{
		reg: instances.New(cfg, st, box), store: st, hc: hc, log: discard(),
		platforms:   syncmap.New[engine.Platform](nil),
		gitlabHooks: syncmap.New[string](nil),
		githubHooks: syncmap.New[string](nil),
		sources:     syncmap.New[dashboard.RepoSource](nil),
		catalogs:    syncmap.New[dashboard.RepoCatalog](nil),
		githubRepos: testRepos,
	}
}

// connectConfigured connects the instance called name the way startup does:
// with the secrets the registry reads for it.
func (c *connector) connectConfigured(ctx context.Context, name string) error {
	inst, ok := c.reg.Get(name)
	if !ok {
		return fmt.Errorf("no instance %q", name)
	}
	s, _, err := c.reg.Secrets(ctx, name)
	if err != nil {
		return err
	}
	return c.Connect(ctx, inst, s)
}

// Values of the GitHub wiring tests. None of them is a real secret.
const (
	ghReviewAppID  = int64(1)
	ghApproveAppID = int64(2)
	ghReviewBot    = int64(4001)
	ghApproveBot   = int64(4002)
	ghTokenBot     = int64(5001)
	ghPAT          = "github_pat_example-approve-token"
	ghHookSecret   = "github-webhook-secret-example"
	ghHead         = "cccccccccccccccccccccccccccccccccccccccc"
)

var (
	ghKeyOnce sync.Once
	ghKey     string
)

// testPEM is one RSA App private key for every test.
func testPEM(t *testing.T) string {
	t.Helper()
	ghKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		ghKey = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	})
	return ghKey
}

// jwtIssuer is the iss claim of a JWT bearer token, or "".
func jwtIssuer(auth string) string {
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		ISS string `json:"iss"`
	}
	_ = json.Unmarshal(b, &c)
	return c.ISS
}

// fakeGitHubAPI answers what connecting and a few calls through the
// platforms need: each App's slug by the JWT's issuer, the bot users, the
// token's user, installation tokens, the approve App's installation, and a
// pull request's comments and reviews. fail maps "METHOD path" to a status
// that path answers instead.
func fakeGitHubAPI(t *testing.T, fail map[string]int) *httptest.Server {
	t.Helper()
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		key := r.Method + " " + r.URL.EscapedPath()
		if code, ok := fail[key]; ok {
			reply(w, code, map[string]string{"message": "Bad credentials"})
			return
		}
		switch key {
		case "GET /app":
			slug := map[string]string{"1": "acme-review", "2": "acme-approve"}[jwtIssuer(auth)]
			if slug == "" {
				reply(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
				return
			}
			reply(w, http.StatusOK, map[string]string{"slug": slug})
		case "GET /users/acme-review%5Bbot%5D":
			reply(w, http.StatusOK, map[string]any{"id": ghReviewBot, "login": "acme-review[bot]"})
		case "GET /users/acme-approve%5Bbot%5D":
			reply(w, http.StatusOK, map[string]any{"id": ghApproveBot, "login": "acme-approve[bot]"})
		case "GET /user":
			if auth != "Bearer "+ghPAT {
				reply(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
				return
			}
			reply(w, http.StatusOK, map[string]any{"id": ghTokenBot, "login": "acme-approver"})
		case "POST /app/installations/55/access_tokens", "POST /app/installations/66/access_tokens":
			tok := "ghs_installation-" + jwtIssuer(auth)
			reply(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": time.Now().Add(time.Hour).UTC()})
		case "GET /repos/acme/widgets/installation":
			reply(w, http.StatusOK, map[string]any{"id": 66})
		case "GET /repos/acme/widgets/issues/1/comments":
			reply(w, http.StatusOK, []map[string]any{
				{"id": 10, "user": map[string]any{"id": ghApproveBot}, "body": "<!-- pergent -->"},
				{"id": 11, "user": map[string]any{"id": ghReviewBot}, "body": "<!-- pergent -->"},
			})
		case "GET /repos/acme/widgets/pulls/1/reviews":
			reply(w, http.StatusOK, []map[string]any{
				{"id": 1, "user": map[string]any{"id": ghApproveBot}, "state": "APPROVED", "commit_id": ghHead},
				{"id": 2, "user": map[string]any{"id": ghTokenBot}, "state": "APPROVED", "commit_id": ghHead},
				{"id": 3, "user": map[string]any{"id": ghReviewBot}, "state": "COMMENTED", "commit_id": ghHead},
			})
		default:
			reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// githubConfig is a config with GitHub instance gh at apiURL. With token
// true the approve side is a bot user's token, else the approve App.
func githubConfig(t *testing.T, apiURL string, token bool) *config.Config {
	t.Helper()
	t.Setenv("GITHUB_REVIEW_KEY", testPEM(t))
	t.Setenv("GITHUB_APPROVE_KEY", testPEM(t))
	t.Setenv("GITHUB_APPROVE_TOKEN", ghPAT)
	t.Setenv("GITHUB_WEBHOOK_SECRET", ghHookSecret)
	inst := config.GitHubInstance{
		Name: "gh", APIURL: apiURL,
		ReviewApp:        config.App{AppID: ghReviewAppID, PrivateKeyEnv: "GITHUB_REVIEW_KEY"},
		ApproveApp:       &config.App{AppID: ghApproveAppID, PrivateKeyEnv: "GITHUB_APPROVE_KEY"},
		WebhookSecretEnv: "GITHUB_WEBHOOK_SECRET",
	}
	if token {
		inst.ApproveApp, inst.ApproveTokenEnv = nil, "GITHUB_APPROVE_TOKEN"
	}
	return &config.Config{GitHub: []config.GitHubInstance{inst}}
}

// testRepos resolves every repository ID to acme/widgets, reached by the
// review App's installation 55.
func testRepos(string) githubforge.Repos {
	return githubforge.ReposFunc(func(context.Context, int64) (githubforge.Repo, error) {
		return githubforge.Repo{Owner: "acme", Name: "widgets", InstallationID: 55}, nil
	})
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Each GitHub instance becomes a github platform whose review App finds only
// its own bot's comment and whose approve App counts only its own bot's
// review; the webhook secret is returned by instance.
func TestConnectGitHubApps(t *testing.T) {
	srv := fakeGitHubAPI(t, nil)
	cfg := githubConfig(t, srv.URL, false)

	c := newConnector(t, cfg, srv.Client(), nil)
	if err := c.connectConfigured(context.Background(), "gh"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if secret, _ := c.githubHooks.Get("gh"); secret != ghHookSecret || len(c.githubHooks.Keys()) != 1 || len(c.gitlabHooks.Keys()) != 0 {
		t.Errorf("webhook secrets = %v / %v", c.githubHooks.Keys(), c.gitlabHooks.Keys())
	}
	p, ok := c.platforms.Get("gh")
	if !ok || p.Kind != config.PlatformGitHub || p.GitLab != nil || p.GitHub == nil || p.GitHub.App == nil || p.GitHub.Approve == nil {
		t.Fatalf("platform = %+v, %v", p, ok)
	}
	if p.GitHub.Approve != p.Approver.(engine.ApproveReach) {
		t.Error("the reach check isn't the approver's")
	}
	ctx := context.Background()
	if id, found, err := p.Forge.FindComment(ctx, 900, 1, "<!-- pergent -->"); err != nil || !found || id != 11 {
		t.Errorf("FindComment = %d, %v, %v; want the review bot's comment 11", id, found, err)
	}
	if ok, err := p.Approver.HasApproved(ctx, 900, 1, ghHead); err != nil || !ok {
		t.Errorf("HasApproved = %v, %v; want the approve App bot's approval", ok, err)
	}
}

// With approve_token_env the approve side is the token's user.
func TestConnectGitHubTokenApprover(t *testing.T) {
	srv := fakeGitHubAPI(t, map[string]int{
		// The approve App is never asked about: there is none.
		"GET /users/acme-approve%5Bbot%5D": http.StatusTeapot,
	})
	cfg := githubConfig(t, srv.URL, true)

	c := newConnector(t, cfg, srv.Client(), nil)
	if err := c.connectConfigured(context.Background(), "gh"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	p, _ := c.platforms.Get("gh")
	if p.Kind != config.PlatformGitHub || p.Approver == nil || p.GitHub == nil || p.GitHub.Approve == nil {
		t.Fatalf("platform = %+v", p)
	}
	// The approve App bot's review comes first; the token user's is the one
	// that counts.
	if ok, err := p.Approver.HasApproved(context.Background(), 900, 1, ghHead); err != nil || !ok {
		t.Errorf("HasApproved = %v, %v; want the token user's approval", ok, err)
	}
}

// A credential that doesn't work stops pergent.sh at startup with an error
// that names the instance and what failed, never the key or token.
func TestConnectGitHubFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token bool
		fail  map[string]int
		env   map[string]string
		want  string
	}{
		{name: "review App refused", fail: map[string]int{"GET /app": http.StatusUnauthorized}, want: "review App (app_id 1)"},
		{name: "approve App bot unknown", fail: map[string]int{"GET /users/acme-approve%5Bbot%5D": http.StatusForbidden}, want: "approve App (app_id 2)"},
		{name: "approve token refused", token: true, fail: map[string]int{"GET /user": http.StatusUnauthorized}, want: "approve token (GITHUB_APPROVE_TOKEN)"},
		{name: "review key not PEM", env: map[string]string{"GITHUB_REVIEW_KEY": "not-a-key-example"}, want: "review App (app_id 1)"},
		{name: "approve key unset", env: map[string]string{"GITHUB_APPROVE_KEY": ""}, want: "GITHUB_APPROVE_KEY"},
		{name: "webhook secret unset", env: map[string]string{"GITHUB_WEBHOOK_SECRET": ""}, want: "GITHUB_WEBHOOK_SECRET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeGitHubAPI(t, tc.fail)
			cfg := githubConfig(t, srv.URL, tc.token)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			err := newConnector(t, cfg, srv.Client(), nil).connectConfigured(context.Background(), "gh")
			if err == nil {
				t.Fatal("Connect succeeded")
			}
			msg := err.Error()
			if !strings.Contains(msg, `github instance "gh"`) || !strings.Contains(msg, tc.want) {
				t.Errorf("error = %q; want it to name instance gh and %q", msg, tc.want)
			}
			for _, secret := range []string{"PRIVATE KEY", "not-a-key-example", ghPAT, ghHookSecret} {
				if strings.Contains(msg, secret) {
					t.Errorf("error %q contains %q", msg, secret)
				}
			}
		})
	}
}

// fakeGitLabUsers answers GET /api/v4/user with the user id each token
// belongs to.
func fakeGitLabUsers(t *testing.T, users map[string]int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := users[r.Header.Get("PRIVATE-TOKEN")]
		if r.URL.Path != "/api/v4/user" || !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "username": "user"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func gitlabConfig(t *testing.T, url string) *config.Config {
	t.Helper()
	t.Setenv("GL_REVIEW", "glpat-review-example")
	t.Setenv("GL_APPROVE", "glpat-approve-example")
	t.Setenv("GL_HOOK", "hook-secret-example")
	return &config.Config{GitLab: []config.Instance{{
		Name: "corp", URL: url, ReviewTokenEnv: "GL_REVIEW", ApproveTokenEnv: "GL_APPROVE", WebhookSecretEnv: "GL_HOOK",
	}}}
}

func TestConnectGitLab(t *testing.T) {
	srv := fakeGitLabUsers(t, map[string]int64{"glpat-review-example": 11, "glpat-approve-example": 12})
	c := newConnector(t, gitlabConfig(t, srv.URL), nil, nil)
	if err := c.connectConfigured(context.Background(), "corp"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if p, ok := c.platforms.Get("corp"); !ok || p.Kind != config.PlatformGitLab || p.Forge == nil || p.Approver == nil {
		t.Errorf("platform = %+v", p)
	}
	if secret, _ := c.gitlabHooks.Get("corp"); secret != "hook-secret-example" || len(c.githubHooks.Keys()) != 0 {
		t.Errorf("secret = %q", secret)
	}
}

// The review and approve tokens must be two users: one user's approval of
// the reviews it posts itself would make the two-identity split pointless.
func TestConnectGitLabSameUserRefused(t *testing.T) {
	srv := fakeGitLabUsers(t, map[string]int64{"glpat-review-example": 11, "glpat-approve-example": 11})
	c := newConnector(t, gitlabConfig(t, srv.URL), nil, nil)
	err := c.connectConfigured(context.Background(), "corp")
	if err == nil {
		t.Fatal("Connect accepted two tokens of the same user")
	}
	if _, ok := c.platforms.Get("corp"); ok {
		t.Error("a refused instance was connected")
	}
	for _, want := range []string{`"corp"`, "GL_REVIEW", "GL_APPROVE", "same user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	for _, secret := range []string{"glpat-review-example", "glpat-approve-example"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error carries a token: %q", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Connecting at runtime

// A dashboard instance has no env var: its errors name no variable, and a
// secret that isn't set is said so before the platform is asked anything.
func TestConnectDashboardInstanceErrors(t *testing.T) {
	srv := fakeGitLabUsers(t, map[string]int64{"glpat-review-example": 11, "glpat-approve-example": 12})
	cfg := &config.Config{}
	c := newConnector(t, cfg, nil, nil)
	lab := dashboardGitLab("lab", srv.URL)
	for _, tc := range []struct {
		name string
		s    instances.Secrets
		want string
	}{
		{"review token wrong", instances.Secrets{ReviewToken: "nope", ApproveToken: "glpat-approve-example", WebhookSecret: "w"}, `gitlab instance "lab": the review token doesn't work`},
		{"approve token wrong", instances.Secrets{ReviewToken: "glpat-review-example", ApproveToken: "nope", WebhookSecret: "w"}, `gitlab instance "lab": the approve token doesn't work`},
		{"same user", instances.Secrets{ReviewToken: "glpat-review-example", ApproveToken: "glpat-review-example", WebhookSecret: "w"}, "same user"},
		{"approve token not set", instances.Secrets{ReviewToken: "glpat-review-example", WebhookSecret: "w"}, `gitlab instance "lab": the approve token is not set`},
		{"webhook secret not set", instances.Secrets{ReviewToken: "glpat-review-example", ApproveToken: "glpat-approve-example"}, `gitlab instance "lab": the webhook secret is not set`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Check(context.Background(), lab, tc.s)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Check = %v, want %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "()") {
				t.Errorf("error %q names an empty variable", err)
			}
		})
	}
}

func dashboardGitLab(name, url string) instances.Instance {
	return instances.Instance{Name: name, Platform: config.PlatformGitLab, Source: instances.SourceDashboard,
		GitLab: &config.Instance{Name: name, URL: url}}
}

// Check has no effect; Connect puts the instance in use, so its webhooks are
// accepted with the stored secret, and Disconnect (with the registry
// forgetting it) takes it out again.
func TestConnectDashboardInstanceAtRuntime(t *testing.T) {
	srv := fakeGitLabUsers(t, map[string]int64{"glpat-review-example": 11, "glpat-approve-example": 12})
	cfg := &config.Config{}
	c := newConnector(t, cfg, nil, nil)
	h := server.Routes(server.Deps{
		Config: cfg, Instances: c.reg, WebhookSecrets: c.gitlabHooks, GitHubWebhookSecrets: c.githubHooks,
	})
	hook := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/lab", strings.NewReader("{}"))
		req.Header.Set("X-Gitlab-Token", token)
		req.Header.Set("X-Gitlab-Event", "Push Hook")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	lab := dashboardGitLab("lab", srv.URL)
	s := instances.Secrets{ReviewToken: "glpat-review-example", ApproveToken: "glpat-approve-example", WebhookSecret: "hook-secret-example"}
	ctx := context.Background()

	if got := hook("hook-secret-example"); got != http.StatusNotFound {
		t.Errorf("webhook of an unknown instance = %d, want 404", got)
	}
	c.reg.Put(lab)
	if got := hook("hook-secret-example"); got != http.StatusUnauthorized {
		t.Errorf("webhook of an instance not connected = %d, want 401", got)
	}
	if err := c.Check(ctx, lab, s); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(c.platforms.Keys()) != 0 || len(c.gitlabHooks.Keys()) != 0 || len(c.sources.Keys()) != 0 {
		t.Errorf("Check connected the instance: %v %v %v", c.platforms.Keys(), c.gitlabHooks.Keys(), c.sources.Keys())
	}

	if err := c.Connect(ctx, lab, s); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := hook("hook-secret-example"); got != http.StatusOK {
		t.Errorf("webhook with the stored secret = %d, want 200", got)
	}
	if got := hook("wrong"); got != http.StatusUnauthorized {
		t.Errorf("webhook with another secret = %d, want 401", got)
	}
	if p, ok := c.platforms.Get("lab"); !ok || p.GitLab == nil {
		t.Errorf("platform = %+v, %v", p, ok)
	}
	if _, ok := c.sources.Get("lab"); !ok {
		t.Error("the dashboard has no reader for the instance")
	}
	if _, ok := c.catalogs.Get("lab"); !ok {
		t.Error("the dashboard has no repository list for the instance")
	}

	c.Disconnect("lab")
	c.Disconnect("never-there")
	c.reg.Remove("lab")
	if got := hook("hook-secret-example"); got != http.StatusNotFound {
		t.Errorf("webhook after Disconnect = %d, want 404", got)
	}
	if _, ok := c.platforms.Get("lab"); ok {
		t.Error("the platform outlived Disconnect")
	}
	if _, ok := c.sources.Get("lab"); ok {
		t.Error("the reader outlived Disconnect")
	}
}

// Connect gives the dashboard a sign-in for an instance with an OAuth client,
// and Disconnect takes it away.
func TestConnectSetsSignIn(t *testing.T) {
	srv := fakeGitLabUsers(t, map[string]int64{"glpat-review-example": 11, "glpat-approve-example": 12})
	cfg := &config.Config{Dashboard: config.Dashboard{URL: "https://pergent.example.com"}}
	c := newConnector(t, cfg, nil, nil)
	d, err := dashboard.New(dashboard.Options{Config: cfg, Instances: c.reg, SecretKey: testSecretKey})
	if err != nil {
		t.Fatal(err)
	}
	c.auth = d.Auth
	lab := dashboardGitLab("lab", srv.URL)
	lab.OAuthClientID = "lab-client"
	s := instances.Secrets{ReviewToken: "glpat-review-example", ApproveToken: "glpat-approve-example", WebhookSecret: "w", OAuthClientSecret: "lab-oauth-secret"}
	if err := c.Connect(context.Background(), lab, s); err != nil {
		t.Fatal(err)
	}
	h := d.Handler()
	login := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/gitlab/lab/login", nil))
		return rec.Code
	}
	if got := login(); got != http.StatusFound {
		t.Errorf("login after Connect = %d, want 302", got)
	}
	c.Disconnect("lab")
	if got := login(); got != http.StatusNotFound {
		t.Errorf("login after Disconnect = %d, want 404", got)
	}
}

// The review bot's or App's Forge of every instance reads repositories for
// Skills & instructions and lists what can be switched on; a platform
// without such a Forge is left out, with a warning naming it.
func TestConnectServesDashboardSources(t *testing.T) {
	gl := gitlabforge.NewForge(gitlab.New("https://gitlab.example.com", "glpat-review-example", nil), gitlab.User{ID: 1})
	app, err := github.NewApp("https://api.github.com", ghReviewAppID, testPEM(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	gh := githubforge.NewForge(app, ghReviewBot, testRepos("gh"))
	var logs strings.Builder
	c := newConnector(t, &config.Config{}, nil, nil)
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	c.serveDashboard("main", engine.Platform{Kind: config.PlatformGitLab, Forge: gl})
	c.serveDashboard("gh", engine.Platform{Kind: config.PlatformGitHub, Forge: gh})
	c.serveDashboard("other", engine.Platform{Kind: config.PlatformGitLab})
	if s, _ := c.sources.Get("main"); s != gl {
		t.Errorf("source of main = %v", s)
	}
	if s, _ := c.sources.Get("gh"); s != gh {
		t.Errorf("source of gh = %v", s)
	}
	if cat, _ := c.catalogs.Get("main"); cat != gl {
		t.Errorf("catalog of main = %v", cat)
	}
	if len(c.sources.Keys()) != 2 || len(c.catalogs.Keys()) != 2 {
		t.Errorf("sources %v, catalogs %v; want main and gh", c.sources.Keys(), c.catalogs.Keys())
	}
	out := logs.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "instance=other") || !strings.Contains(out, "platform=gitlab") {
		t.Errorf("warnings = %q; want one naming instance other on gitlab", out)
	}
}

// ---------------------------------------------------------------------------
// Startup

// addInstance stores a GitLab instance of the dashboard at url with the
// given secrets, sealed.
func addInstance(t *testing.T, st *store.Store, name, url string, secrets map[string]string) {
	t.Helper()
	box, err := secretbox.New(testSecretKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed := map[string][]byte{}
	for field, v := range secrets {
		sealed[field] = box.Seal([]byte(v))
	}
	row := store.InstanceRow{Name: name, Platform: config.PlatformGitLab, URL: url}
	if err := st.CreateInstance(context.Background(), row, sealed, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// An instance added in the dashboard that can't be connected is logged at
// Error and recorded as a "Needs attention" item, without its secret, and
// never stops startup; one that can be is connected, and its old item goes.
func TestStartupDashboardInstanceFailureNotFatal(t *testing.T) {
	_, st := testDatabaseURL(t)
	ctx := context.Background()
	srv := fakeGitLabUsers(t, map[string]int64{"glpat-review-example": 11, "glpat-approve-example": 12})
	addInstance(t, st, "bad", srv.URL, map[string]string{
		instances.FieldReviewToken: "glpat-bad-secret-example", instances.FieldApproveToken: "glpat-approve-example",
		instances.FieldWebhookSecret: "hook-secret-example"})
	addInstance(t, st, "good", srv.URL, map[string]string{
		instances.FieldReviewToken: "glpat-review-example", instances.FieldApproveToken: "glpat-approve-example",
		instances.FieldWebhookSecret: "hook-secret-example"})
	if err := st.SetAttention(ctx, "good", "", attInstanceUnreachable, "an old failure"); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	c := newConnector(t, cfg, nil, st)
	var logs strings.Builder
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	if err := c.connectAll(ctx); err != nil {
		t.Fatalf("connectAll = %v, want nil: a dashboard instance never stops startup", err)
	}

	if _, ok := c.platforms.Get("good"); !ok {
		t.Error("the good instance isn't connected")
	}
	if _, ok := c.platforms.Get("bad"); ok {
		t.Error("the bad instance is connected")
	}
	items, err := st.Attention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Instance != "bad" || items[0].Project != "" || items[0].Kind != "instance_unreachable" ||
		!strings.Contains(items[0].Message, `gitlab instance "bad": the review token doesn't work`) {
		t.Fatalf("attention = %+v; want one instance_unreachable item for bad", items)
	}
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "instance=bad") {
		t.Errorf("logs = %q; want an Error naming instance bad", out)
	}
	if strings.Contains(out+items[0].Message, "glpat-bad-secret-example") {
		t.Error("the secret reached the log or the item")
	}
}

// An instance of the config that can't be connected stops startup; so does a
// name in both the config and the dashboard.
func TestStartupConfigFailureIsFatal(t *testing.T) {
	ctx := context.Background()
	srv := fakeGitLabUsers(t, map[string]int64{"glpat-review-example": 11, "glpat-approve-example": 11})
	c := newConnector(t, gitlabConfig(t, srv.URL), nil, nil)
	if err := c.connectAll(ctx); err == nil || !strings.Contains(err.Error(), "same user") {
		t.Errorf("connectAll with a bad config instance = %v", err)
	}

	_, st := testDatabaseURL(t)
	good := fakeGitLabUsers(t, map[string]int64{"glpat-review-example": 11, "glpat-approve-example": 12})
	addInstance(t, st, "corp", good.URL, nil)
	c = newConnector(t, gitlabConfig(t, good.URL), nil, st)
	err := c.connectAll(ctx)
	if err == nil || !strings.Contains(err.Error(), `instance "corp" is in both the config file and the dashboard`) {
		t.Errorf("connectAll with a clash = %v", err)
	}
}

// Config instances connect as before, then the dashboard's.
func TestStartupConnectsConfigThenDashboard(t *testing.T) {
	_, st := testDatabaseURL(t)
	srv := fakeGitLabUsers(t, map[string]int64{"glpat-review-example": 11, "glpat-approve-example": 12})
	addInstance(t, st, "lab", srv.URL, map[string]string{
		instances.FieldReviewToken: "glpat-review-example", instances.FieldApproveToken: "glpat-approve-example",
		instances.FieldWebhookSecret: "lab-hook"})
	c := newConnector(t, gitlabConfig(t, srv.URL), nil, st)
	if err := c.connectAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.platforms.Keys(); len(got) != 2 || got[0] != "corp" || got[1] != "lab" {
		t.Errorf("connected = %v, want corp and lab", got)
	}
	if secret, _ := c.gitlabHooks.Get("lab"); secret != "lab-hook" {
		t.Errorf("lab's webhook secret = %q", secret)
	}
}

// An error message that embeds a secret value, as a platform's error can,
// is cleaned of every secret of the instance, and only of those.
func TestRedactSecrets(t *testing.T) {
	s := instances.Secrets{ReviewToken: "glpat-REVIEW-1234", ApproveToken: "glpat-APPROVE-5678", WebhookSecret: "hook-secret-9"}
	msg := `Get "https://gitlab.example.com/api/v4/user?private_token=glpat-REVIEW-1234": 401 (approve glpat-APPROVE-5678, hook-secret-9, again glpat-REVIEW-1234)`
	got := redactSecrets(msg, s)
	for _, secret := range []string{s.ReviewToken, s.ApproveToken, s.WebhookSecret} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted message still holds %q: %s", secret, got)
		}
	}
	if want := `Get "https://gitlab.example.com/api/v4/user?private_token=[REDACTED]": 401 (approve [REDACTED], [REDACTED], again [REDACTED])`; got != want {
		t.Errorf("redactSecrets = %q\nwant %q", got, want)
	}
	if got := redactSecrets(msg, instances.Secrets{}); got != msg {
		t.Errorf("with no secrets the message changed: %q", got)
	}
}
