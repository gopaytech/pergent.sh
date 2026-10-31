package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
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
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/server"
)

func TestCheckSkills(t *testing.T) {
	cfg := &config.Config{SkillsDir: "/skills", Repositories: []config.Repository{
		{GitLab: "main", Project: "acme/widgets", Skills: []string{"security"}},
		{GitLab: "main", Project: "acme/gadgets"},
	}}
	if err := checkSkills(cfg, inputs.Library{"security": nil}); err != nil {
		t.Errorf("checkSkills = %v, want nil", err)
	}
	err := checkSkills(cfg, inputs.Library{"style": nil})
	if err == nil || !strings.Contains(err.Error(), `skill "security"`) || !strings.Contains(err.Error(), "acme/widgets") {
		t.Errorf("checkSkills = %v, want an error naming the skill and repository", err)
	}
}

// The routes answer while the loops are stuck (as in a startup reconcile
// against a GitLab that doesn't answer), and cancelling stops both.
func TestServeUntilDoneAnswersWhileLoopsBlock(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Routes(server.Deps{Config: &config.Config{}})
	loopsStarted := make(chan struct{})
	loopsStopped := make(chan struct{})
	loops := func(ctx context.Context) {
		close(loopsStarted)
		<-ctx.Done() // blocked, like a reconcile waiting on GitLab
		close(loopsStopped)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveUntilDone(ctx, ln, handler, loops, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	<-loopsStarted
	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz while the loops block: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("GET /healthz = %d %q, want 200 ok", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveUntilDone = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntilDone didn't return after cancel")
	}
	select {
	case <-loopsStopped:
	default:
		t.Errorf("the loops were not stopped")
	}
}

// A GitHub repository's missing skill names its instance, whichever platform
// it is on.
func TestCheckSkillsGitHub(t *testing.T) {
	cfg := &config.Config{SkillsDir: "/skills", Repositories: []config.Repository{
		{GitHub: "gh", Project: "acme/widgets", Skills: []string{"security"}},
	}}
	err := checkSkills(cfg, inputs.Library{})
	if err == nil || !strings.Contains(err.Error(), `instance "gh"`) || strings.Contains(err.Error(), "gitlab") {
		t.Errorf("checkSkills = %v, want an error naming instance gh", err)
	}
}

// ---------------------------------------------------------------------------
// GitHub wiring

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

	platforms, secrets, err := connectGitHub(context.Background(), cfg, testRepos, srv.Client(), discard())
	if err != nil {
		t.Fatalf("connectGitHub: %v", err)
	}
	if secrets["gh"] != ghHookSecret || len(secrets) != 1 {
		t.Errorf("webhook secrets = %v", secrets)
	}
	p, ok := platforms["gh"]
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

	platforms, _, err := connectGitHub(context.Background(), cfg, testRepos, srv.Client(), discard())
	if err != nil {
		t.Fatalf("connectGitHub: %v", err)
	}
	p := platforms["gh"]
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
			_, _, err := connectGitHub(context.Background(), cfg, testRepos, srv.Client(), discard())
			if err == nil {
				t.Fatal("connectGitHub succeeded")
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
	platforms, secrets, err := connectGitLab(context.Background(), gitlabConfig(t, srv.URL))
	if err != nil {
		t.Fatalf("connectGitLab: %v", err)
	}
	if p, ok := platforms["corp"]; !ok || p.Kind != config.PlatformGitLab || p.Forge == nil || p.Approver == nil {
		t.Errorf("platform = %+v", platforms["corp"])
	}
	if secrets["corp"] != "hook-secret-example" {
		t.Errorf("secret = %q", secrets["corp"])
	}
}

// The review and approve tokens must be two users: one user's approval of
// the reviews it posts itself would make the two-identity split pointless.
func TestConnectGitLabSameUserRefused(t *testing.T) {
	srv := fakeGitLabUsers(t, map[string]int64{"glpat-review-example": 11, "glpat-approve-example": 11})
	_, _, err := connectGitLab(context.Background(), gitlabConfig(t, srv.URL))
	if err == nil {
		t.Fatal("connectGitLab accepted two tokens of the same user")
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
// Dashboard wiring

// dashboardConfig is a config with GitLab instance main and GitHub instance
// gh, both set up for sign-in through the env vars named, and the dashboard
// on unless url is "".
func dashboardConfig(url string) *config.Config {
	return &config.Config{
		GitLab: []config.Instance{{Name: "main", URL: "https://gitlab.example.com",
			OAuthClientIDEnv: "GL_OAUTH_ID", OAuthClientSecretEnv: "GL_OAUTH_SECRET"}},
		GitHub: []config.GitHubInstance{{Name: "gh", APIURL: "https://api.github.com",
			OAuthClientIDEnv: "GH_OAUTH_ID", OAuthClientSecretEnv: "GH_OAUTH_SECRET"}},
		Dashboard: config.Dashboard{URL: url},
	}
}

func setOAuthEnv(t *testing.T) {
	t.Setenv("GL_OAUTH_ID", "gl-oauth-id-example")
	t.Setenv("GL_OAUTH_SECRET", "gl-oauth-secret-example")
	t.Setenv("GH_OAUTH_ID", "gh-oauth-id-example")
	t.Setenv("GH_OAUTH_SECRET", "gh-oauth-secret-example")
}

// With the dashboard on, an instance used for sign-in whose OAuth env var is
// unset stops pergent.sh before it connects to anything, naming the
// instance and the variable, never a value. With it off, nothing is needed.
func TestCheckDashboardEnv(t *testing.T) {
	setOAuthEnv(t)
	if err := checkDashboardEnv(dashboardConfig("https://dashboard.example.com")); err != nil {
		t.Fatalf("checkDashboardEnv with every variable set = %v", err)
	}
	for _, tc := range []struct{ unset, instance string }{
		{"GL_OAUTH_ID", `gitlab instance "main"`},
		{"GL_OAUTH_SECRET", `gitlab instance "main"`},
		{"GH_OAUTH_ID", `github instance "gh"`},
		{"GH_OAUTH_SECRET", `github instance "gh"`},
	} {
		t.Run(tc.unset, func(t *testing.T) {
			setOAuthEnv(t)
			t.Setenv(tc.unset, "")
			err := checkDashboardEnv(dashboardConfig("https://dashboard.example.com"))
			if err == nil {
				t.Fatal("checkDashboardEnv succeeded")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.instance) || !strings.Contains(msg, tc.unset) || !strings.Contains(msg, "dashboard") {
				t.Errorf("error = %q; want it to name the dashboard, %s and %s", msg, tc.instance, tc.unset)
			}
			if strings.Contains(msg, "-example") {
				t.Errorf("error %q holds a value", msg)
			}
		})
	}
	t.Run("dashboard off", func(t *testing.T) {
		for _, name := range []string{"GL_OAUTH_ID", "GL_OAUTH_SECRET", "GH_OAUTH_ID", "GH_OAUTH_SECRET"} {
			t.Setenv(name, "")
		}
		if err := checkDashboardEnv(dashboardConfig("")); err != nil {
			t.Errorf("checkDashboardEnv with the dashboard off = %v", err)
		}
	})
}

// The LLM key is read only when the file has an llm: block; without one
// (the dashboard adds the LLM) there is no env var name to read.
func TestReadLLMKey(t *testing.T) {
	cfg := &config.Config{}
	if key, err := readLLMKey(cfg); err != nil || key != "" {
		t.Errorf("readLLMKey without an llm block = %q, %v", key, err)
	}

	cfg.LLM = config.LLM{Provider: "anthropic", Model: "m", APIKeyEnv: "PERGENT_TEST_LLM_KEY"}
	t.Setenv("PERGENT_TEST_LLM_KEY", "")
	err := func() error { _, err := readLLMKey(cfg); return err }()
	if err == nil || !strings.Contains(err.Error(), "PERGENT_TEST_LLM_KEY") {
		t.Errorf("readLLMKey with the variable unset = %v", err)
	}
	t.Setenv("PERGENT_TEST_LLM_KEY", "k")
	if key, err := readLLMKey(cfg); err != nil || key != "k" {
		t.Errorf("readLLMKey = %q, %v", key, err)
	}
}

// testSecretKey is a fixed 32-byte PERGENT_SH_SECRET_KEY, standard base64.
const testSecretKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// get serves GET path on h and returns the status and body.
func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// With dashboard.url set the routes serve the sign-in instances, the API
// and the app; without it there is no dashboard and none of them exist.
func TestNewDashboardRoutes(t *testing.T) {
	setOAuthEnv(t)
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := dashboardConfig("https://dashboard.example.com")
	d, err := newDashboard(cfg, nil, testSecretKey, inputs.Library{}, nil, func(string, string) {}, log)
	if err != nil || d == nil {
		t.Fatalf("newDashboard = %v, %v", d, err)
	}
	h := server.Routes(server.Deps{Config: cfg, Dashboard: dashboardHandler(d), Log: log})
	code, body := get(t, h, "/api/instances")
	if code != http.StatusOK || !strings.Contains(body, `"instance":"main"`) || !strings.Contains(body, `"instance":"gh"`) {
		t.Errorf("GET /api/instances = %d %s; want both instances", code, body)
	}
	if strings.Contains(body, "example") {
		t.Errorf("GET /api/instances holds a value from the environment: %s", body)
	}
	if code, _ := get(t, h, "/api/me"); code != http.StatusUnauthorized {
		t.Errorf("GET /api/me without a session = %d, want 401", code)
	}
	if code, _ := get(t, h, "/"); code != http.StatusOK {
		t.Errorf("GET / = %d, want the app", code)
	}

	off := dashboardConfig("")
	d, err = newDashboard(off, nil, testSecretKey, inputs.Library{}, nil, func(string, string) {}, log)
	if err != nil || d != nil {
		t.Fatalf("newDashboard with the dashboard off = %v, %v; want nil, nil", d, err)
	}
	h = server.Routes(server.Deps{Config: off, Dashboard: dashboardHandler(d), Log: log})
	for _, path := range []string{"/", "/api/instances", "/auth/gitlab/main/login"} {
		if code, _ := get(t, h, path); code != http.StatusNotFound {
			t.Errorf("GET %s with the dashboard off = %d, want 404", path, code)
		}
	}
}

// The review bot's or App's Forge of every instance reads repositories for
// Skills & instructions and lists what can be switched on; a platform
// without such a Forge is left out.
func TestDashboardSources(t *testing.T) {
	gl := gitlabforge.NewForge(gitlab.New("https://gitlab.example.com", "glpat-review-example", nil), gitlab.User{ID: 1})
	app, err := github.NewApp("https://api.github.com", ghReviewAppID, testPEM(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	gh := githubforge.NewForge(app, ghReviewBot, testRepos("gh"))
	var logs strings.Builder
	sources, catalogs := dashboardSources(map[string]engine.Platform{
		"main":  {Kind: config.PlatformGitLab, Forge: gl},
		"gh":    {Kind: config.PlatformGitHub, Forge: gh},
		"other": {Kind: config.PlatformGitLab},
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if sources["main"] != gl || sources["gh"] != gh || len(sources) != 2 {
		t.Errorf("sources = %v", sources)
	}
	if catalogs["main"] != gl || catalogs["gh"] != gh || len(catalogs) != 2 {
		t.Errorf("catalogs = %v", catalogs)
	}
	// An instance whose Forge can't do either is named, with its platform.
	out := logs.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "instance=other") || !strings.Contains(out, "platform=gitlab") {
		t.Errorf("startup warnings = %q; want one naming instance other on gitlab", out)
	}
}

// every runs f at once and then every interval until ctx is done.
func TestEvery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan struct{}, 10)
	done := make(chan struct{})
	go func() {
		every(ctx, 10*time.Millisecond, func(context.Context) { calls <- struct{}{} })
		close(done)
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d runs", i)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("every didn't return after cancel")
	}
}

// pruneSessions logs what it pruned, and a failure as a warning.
func TestPruneSessions(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var got time.Time
	prune := pruneSessions(func(_ context.Context, now time.Time) (int, error) {
		got = now
		return 3, nil
	}, func() time.Time { return at }, log)
	prune(context.Background())
	if !got.Equal(at) || !strings.Contains(logs.String(), "sessions=3") {
		t.Errorf("pruned at %s, logs:\n%s", got, logs.String())
	}
	logs.Reset()
	pruneSessions(func(context.Context, time.Time) (int, error) { return 0, errors.New("database down") },
		time.Now, log)(context.Background())
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "database down") {
		t.Errorf("a failed prune isn't a warning:\n%s", logs.String())
	}
	logs.Reset()
	pruneSessions(func(context.Context, time.Time) (int, error) { return 0, nil }, time.Now, log)(context.Background())
	if logs.Len() != 0 {
		t.Errorf("logged with nothing pruned:\n%s", logs.String())
	}
}

// together runs every loop and returns once all of them have returned.
func TestTogether(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	stopped := 0
	loop := func(ctx context.Context) {
		<-ctx.Done()
		mu.Lock()
		stopped++
		mu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		together(loop, loop, loop)(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("together didn't return")
	}
	if stopped != 3 {
		t.Errorf("%d loops stopped, want 3", stopped)
	}
}

// The picker's whole budget, token renewal included, ends well before the
// server's write timeout, so even its truncated answer is written.
func TestPickerBudgetBelowWriteTimeout(t *testing.T) {
	if dashboard.AvailableBudget > writeTimeout-10*time.Second {
		t.Errorf("dashboard.AvailableBudget %v is not well below the write timeout %v", dashboard.AvailableBudget, writeTimeout)
	}
}
