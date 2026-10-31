package main

import (
	"context"
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
	d, err := newDashboard(dashboard.Options{Config: cfg, SecretKey: testSecretKey, Log: log})
	if err != nil || d == nil {
		t.Fatalf("newDashboard = %v, %v", d, err)
	}
	h := server.Routes(server.Deps{Config: cfg, Dashboard: dashboardHandler(d), Log: log})
	code, body := get(t, h, "/api/instances")
	if code != http.StatusOK || !strings.Contains(body, `"password":true`) ||
		!strings.Contains(body, `"instance":"main"`) || !strings.Contains(body, `"instance":"gh"`) {
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
	d, err = newDashboard(dashboard.Options{Config: off, SecretKey: testSecretKey, Log: log})
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

// pruneAudit deletes the audit entries older than 90 days, logs what it
// pruned, and a failure as a warning.
func TestPruneAudit(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var got time.Time
	pruneAudit(func(_ context.Context, before time.Time) (int, error) {
		got = before
		return 2, nil
	}, func() time.Time { return at }, log)(context.Background())
	if want := at.Add(-90 * 24 * time.Hour); !got.Equal(want) || !strings.Contains(logs.String(), "entries=2") {
		t.Errorf("pruned before %s, want %s; logs:\n%s", got, want, logs.String())
	}
	logs.Reset()
	pruneAudit(func(context.Context, time.Time) (int, error) { return 0, errors.New("database down") },
		time.Now, log)(context.Background())
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "database down") {
		t.Errorf("a failed prune isn't a warning:\n%s", logs.String())
	}
	logs.Reset()
	pruneAudit(func(context.Context, time.Time) (int, error) { return 0, nil }, time.Now, log)(context.Background())
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
