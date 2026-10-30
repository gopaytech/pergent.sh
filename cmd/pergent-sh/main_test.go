package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	"github.com/gopaytech/pergent.sh/internal/engine"
	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/server"
)

func TestParseArgs(t *testing.T) {
	if got, err := parseArgs([]string{"serve"}, io.Discard); err != nil || got != "/etc/pergent-sh/config.yaml" {
		t.Errorf("serve = %q, %v; want the default path", got, err)
	}
	if got, err := parseArgs([]string{"serve", "--config", "/tmp/c.yaml"}, io.Discard); err != nil || got != "/tmp/c.yaml" {
		t.Errorf("serve --config = %q, %v", got, err)
	}
	for _, args := range [][]string{nil, {"run"}, {"serve", "extra"}, {"serve", "--nope"}} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Errorf("parseArgs(%q) succeeded, want an error", args)
		}
	}
}

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
