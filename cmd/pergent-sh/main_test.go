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
	"github.com/gopaytech/pergent.sh/internal/server"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/webhook"
)

// A skill a config repository names that the library lacks never stops
// startup: it is recorded under "Needs attention" as skill_missing, for the
// repository's own instance, whichever platform it is on. A repository whose
// skills are all there, or that names none, gets no item.
func TestUnknownConfigSkillDoesNotStopStartup(t *testing.T) {
	_, st := testDatabaseURL(t)
	ctx := context.Background()
	if _, err := st.CreateLibrarySkill(ctx, store.NewSkill{Name: "security", Content: "Look for security problems.", Actor: "test", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Repositories: []config.Repository{
		{GitLab: "main", Project: "acme/widgets", Skills: []string{"security", "gone"}},
		{GitHub: "gh", Project: "acme/gadgets", Skills: []string{"security", "also-gone"}},
		{GitLab: "main", Project: "acme/fine", Skills: []string{"security"}},
		{GitLab: "main", Project: "acme/default"},
	}}
	if err := checkSkills(ctx, cfg, st, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("checkSkills = %v, want nil: a missing skill never stops startup", err)
	}

	items, err := st.Attention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.Attention{}
	for _, a := range items {
		got[a.Instance+" "+a.Project] = a
	}
	if len(got) != 2 {
		t.Errorf("attention = %+v, want exactly the 2 repositories with a missing skill", items)
	}
	for key, skill := range map[string]string{"main acme/widgets": "gone", "gh acme/gadgets": "also-gone"} {
		a, ok := got[key]
		want := `skill "` + skill + `" is not in the skill library`
		if !ok || a.Kind != "skill_missing" || a.Message != want {
			t.Errorf("attention for %s = %+v, want skill_missing %q", key, a, want)
		}
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

// ---------------------------------------------------------------------------
// Dashboard wiring

// dashboardConfig is a config with GitLab instance main and GitHub instance
// gh, and the dashboard on unless url is "".
func dashboardConfig(url string) *config.Config {
	return &config.Config{
		GitLab:    []config.Instance{{Name: "main", URL: "https://gitlab.example.com"}},
		GitHub:    []config.GitHubInstance{{Name: "gh", APIURL: "https://api.github.com"}},
		Dashboard: config.Dashboard{URL: url},
	}
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

// An OIDC sign-in in the config file stops startup when the variable that
// holds its client secret is unset or empty, and names the variable (never a
// value). JWT mode and no sign-in have no such secret.
func TestCheckSignInSecret(t *testing.T) {
	cfg := &config.Config{}
	if err := checkSignInSecret(cfg); err != nil {
		t.Errorf("checkSignInSecret with no sign-in = %v", err)
	}
	cfg.Dashboard.SignIn = config.SignIn{Mode: config.SignInJWT, ClientSecretEnv: "PERGENT_TEST_OIDC_SECRET"}
	t.Setenv("PERGENT_TEST_OIDC_SECRET", "")
	if err := checkSignInSecret(cfg); err != nil {
		t.Errorf("checkSignInSecret in jwt mode = %v", err)
	}

	cfg.Dashboard.SignIn = config.SignIn{Mode: config.SignInOIDC, ClientSecretEnv: "PERGENT_TEST_OIDC_SECRET"}
	err := checkSignInSecret(cfg)
	if err == nil || !strings.Contains(err.Error(), "dashboard.sign_in.client_secret_env") || !strings.Contains(err.Error(), "PERGENT_TEST_OIDC_SECRET") {
		t.Errorf("checkSignInSecret with the variable empty = %v; want an error naming the key and the variable", err)
	}
	t.Setenv("PERGENT_TEST_OIDC_SECRET", "s3cret-value")
	if err := checkSignInSecret(cfg); err != nil {
		t.Errorf("checkSignInSecret with the variable set = %v", err)
	}
	// The secret is never in an error.
	t.Setenv("PERGENT_TEST_OIDC_SECRET", "")
	if err := checkSignInSecret(cfg); err != nil && strings.Contains(err.Error(), "s3cret-value") {
		t.Errorf("error holds the secret: %v", err)
	}
}

// The start-up sweep logs how many sessions it ended, says nothing when there
// were none, and stops startup when it fails.
func TestEndStaleSSOSessions(t *testing.T) {
	run := func(n int, err error) (string, error) {
		var logs strings.Builder
		log := slog.New(slog.NewTextHandler(&logs, nil))
		got := endStaleSSOSessions(context.Background(), func(context.Context) (int, error) { return n, err }, log)
		return logs.String(), got
	}
	if out, err := run(0, nil); err != nil || out != "" {
		t.Errorf("none ended = %q, %v; want no log and no error", out, err)
	}
	if out, err := run(3, nil); err != nil || !strings.Contains(out, "sessions=3") {
		t.Errorf("3 ended = %q, %v; want the count logged", out, err)
	}
	boom := errors.New("database down")
	if _, err := run(0, boom); !errors.Is(err, boom) {
		t.Errorf("a failed sweep = %v; want it to stop startup", err)
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

// With dashboard.url set the routes serve the sign-in page's question, the
// API and the app; without it there is no dashboard and none of them exist.
// The platforms' sign-in routes exist in neither case.
func TestNewDashboardRoutes(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := dashboardConfig("https://dashboard.example.com")
	d, err := newDashboard(dashboard.Options{Config: cfg, SecretKey: testSecretKey, Log: log})
	if err != nil || d == nil {
		t.Fatalf("newDashboard = %v, %v", d, err)
	}
	h := server.Routes(server.Deps{Config: cfg, Dashboard: dashboardHandler(d), Log: log})
	code, body := get(t, h, "/api/sign-in-options")
	if code != http.StatusOK || !strings.Contains(body, `"password":true`) || strings.Contains(body, "instances") {
		t.Errorf("GET /api/sign-in-options = %d %s; want the password form and no instance", code, body)
	}
	if code, _ := get(t, h, "/auth/gitlab/main/login"); code != http.StatusNotFound {
		t.Errorf("GET /auth/gitlab/main/login = %d, want 404", code)
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
	for _, path := range []string{"/", "/api/sign-in-options", "/auth/gitlab/main/login"} {
		if code, _ := get(t, h, path); code != http.StatusNotFound {
			t.Errorf("GET %s with the dashboard off = %d, want 404", path, code)
		}
	}
}

// The pages wrapper (JWT sign-in) is the dashboard's, and absent without one.
func TestDashboardPages(t *testing.T) {
	if dashboardPages(nil) != nil {
		t.Error("dashboardPages(nil) is not nil")
	}
	d, err := newDashboard(dashboard.Options{Config: dashboardConfig("https://dashboard.example.com"), SecretKey: testSecretKey})
	if err != nil || d == nil {
		t.Fatalf("newDashboard = %v, %v", d, err)
	}
	if dashboardPages(d) == nil {
		t.Fatal("dashboardPages(d) is nil")
	}
	// With no JWT sign-in set up the pages come as they are.
	rec := httptest.NewRecorder()
	dashboardPages(d)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("app")) })).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "app" || len(rec.Result().Cookies()) != 0 {
		t.Errorf("GET / = %d %q, cookies %v", rec.Code, rec.Body, rec.Result().Cookies())
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

// pruneCommands deletes the finished commands older than 90 days, logs what
// it pruned, and a failure as a warning.
func TestPruneCommands(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var got time.Time
	pruneCommands(func(_ context.Context, before time.Time) (int, error) {
		got = before
		return 3, nil
	}, func() time.Time { return at }, log)(context.Background())
	if want := at.Add(-90 * 24 * time.Hour); !got.Equal(want) || !strings.Contains(logs.String(), "commands=3") {
		t.Errorf("pruned before %s, want %s; logs:\n%s", got, want, logs.String())
	}
	logs.Reset()
	pruneCommands(func(context.Context, time.Time) (int, error) { return 0, errors.New("database down") },
		time.Now, log)(context.Background())
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "database down") {
		t.Errorf("a failed prune isn't a warning:\n%s", logs.String())
	}
	logs.Reset()
	pruneCommands(func(context.Context, time.Time) (int, error) { return 0, nil }, time.Now, log)(context.Background())
	if logs.Len() != 0 {
		t.Errorf("logged with nothing pruned:\n%s", logs.String())
	}
}

// sweepFetches logs a failed sweep as a warning, and nothing else.
func TestSweepFetches(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	sweepFetches(func(context.Context) error { return errors.New("database down") }, log)(context.Background())
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "database down") {
		t.Errorf("a failed sweep isn't a warning:\n%s", logs.String())
	}
	logs.Reset()
	sweepFetches(func(context.Context) error { return nil }, log)(context.Background())
	if logs.Len() != 0 {
		t.Errorf("logged a sweep that worked:\n%s", logs.String())
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

// warnTimeouts logs each short timeout once, at warn, and nothing when all fit.
func TestWarnTimeouts(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := &config.Config{Jobs: config.Jobs{Timeout: 10 * time.Minute, MaxTurns: 40}}
	warnTimeouts(cfg, log)
	out := logs.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "jobs.timeout 10m0s is short for jobs.max_turns 40") {
		t.Errorf("logs:\n%s", out)
	}
	logs.Reset()
	cfg.Jobs.Timeout = 30 * time.Minute
	warnTimeouts(cfg, log)
	if logs.Len() != 0 {
		t.Errorf("logged for a timeout that fits:\n%s", logs.String())
	}
}

// The OAuth keys of an instance only warn: the log names the instance and
// never an env var's value.
func TestWarnTimeoutsWarnsOldOAuthKeys(t *testing.T) {
	t.Setenv("OLD_OAUTH_SECRET", "must-not-be-logged")
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := &config.Config{
		Jobs:   config.Jobs{Timeout: 30 * time.Minute, MaxTurns: 40},
		GitLab: []config.Instance{{Name: "main", OAuthClientIDEnv: "OLD_OAUTH_ID", OAuthClientSecretEnv: "OLD_OAUTH_SECRET"}},
	}
	warnTimeouts(cfg, log)
	out := logs.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "gitlab[0] (main)") || strings.Contains(out, "must-not-be-logged") {
		t.Errorf("logs:\n%s", out)
	}
}

// pruneDeliveries forgets a delivery only once it is too old to be accepted.
func TestPruneDeliveries(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var got time.Time
	pruneDeliveries(func(_ context.Context, before time.Time) (int, error) {
		got = before
		return 3, nil
	}, func() time.Time { return at }, log)(context.Background())
	if want := at.Add(-webhook.DeliveryRetention); !got.Equal(want) || !strings.Contains(logs.String(), "deliveries=3") {
		t.Errorf("pruned before %s, want %s; logs:\n%s", got, want, logs.String())
	}
	logs.Reset()
	pruneDeliveries(func(context.Context, time.Time) (int, error) { return 0, errors.New("database down") },
		time.Now, log)(context.Background())
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "database down") {
		t.Errorf("a failed prune isn't a warning:\n%s", logs.String())
	}
}
