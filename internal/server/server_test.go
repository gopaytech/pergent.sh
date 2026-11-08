package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/skillfetch"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
	"github.com/gopaytech/pergent.sh/internal/webhook"
)

// Both webhook routes are registered; an unsigned request reaches the handler
// (401) before it touches the store, which is nil here.
func TestWebhookRoutesRegistered(t *testing.T) {
	cfg := &config.Config{
		GitLab: []config.Instance{{Name: "main"}},
		GitHub: []config.GitHubInstance{{Name: "gh"}},
	}
	mux := Routes(Deps{
		Config:               cfg,
		Instances:            instances.New(cfg, nil, nil),
		WebhookSecrets:       syncmap.New(map[string]string{"main": "gitlab-secret"}),
		GitHubWebhookSecrets: syncmap.New(map[string]string{"gh": "github-secret"}),
	})
	for path, want := range map[string]int{
		"/webhooks/gitlab/main":  http.StatusUnauthorized,
		"/webhooks/github/gh":    http.StatusUnauthorized,
		"/webhooks/github/main":  http.StatusNotFound,
		"/webhooks/gitlab/gh":    http.StatusNotFound,
		"/webhooks/github/other": http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if rec.Code != want {
			t.Errorf("POST %s = %d, want %d", path, rec.Code, want)
		}
	}
}

func dashboardDeps(url string, dash http.Handler) Deps {
	cfg := &config.Config{
		GitLab:    []config.Instance{{Name: "main"}},
		Dashboard: config.Dashboard{URL: url},
	}
	return Deps{
		Config:         cfg,
		Instances:      instances.New(cfg, nil, nil),
		WebhookSecrets: syncmap.New(map[string]string{"main": "gitlab-secret"}),
		Dashboard:      dash,
	}
}

func status(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader("{}")))
	return rec
}

// Without a dashboard handler (or without dashboard.url) there is no /, /api/
// or /auth/; the webhook and health routes are as before.
func TestDashboardRoutesOffWhenNotConfigured(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "dashboard") })
	for name, d := range map[string]Deps{
		"no handler":       dashboardDeps("https://pergent.example.com", nil),
		"no dashboard.url": dashboardDeps("", stub),
	} {
		mux := Routes(d)
		for _, p := range []string{"/", "/repos", "/api/me", "/auth/login", "/index.html"} {
			if rec := status(mux, http.MethodGet, p); rec.Code != http.StatusNotFound {
				t.Errorf("%s: GET %s = %d, want 404", name, p, rec.Code)
			}
		}
		if rec := status(mux, http.MethodPost, "/webhooks/gitlab/main"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: webhook = %d, want 401", name, rec.Code)
		}
		if rec := status(mux, http.MethodGet, "/healthz"); rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Errorf("%s: healthz = %d %q", name, rec.Code, rec.Body.String())
		}
	}
}

// The main handler, which public_url reaches, never serves the metrics:
// they have a port of their own. With the dashboard on, /metrics falls to the
// app like any other path, and what it returns is not the exposition format.
func TestMainHandlerHasNoMetrics(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "dashboard") })
	for name, d := range map[string]Deps{
		"no dashboard":   dashboardDeps("", nil),
		"with dashboard": dashboardDeps("https://pergent.example.com", stub),
	} {
		rec := status(Routes(d), http.MethodGet, "/metrics")
		if strings.Contains(rec.Header().Get("Content-Type"), "version=0.0.4") || strings.Contains(rec.Body.String(), "# TYPE") ||
			strings.Contains(rec.Body.String(), "pergent_") {
			t.Errorf("%s: GET /metrics = %d %q %q, want no metrics", name, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
	if rec := status(Routes(dashboardDeps("", nil)), http.MethodGet, "/metrics"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /metrics without the dashboard = %d, want 404", rec.Code)
	}
}

// With a handler and dashboard.url, /auth/ and /api/ go to the handler, other
// paths get the embedded app, and the other routes still win.
func TestDashboardRoutesMounted(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "dashboard "+r.URL.Path) })
	mux := Routes(dashboardDeps("https://pergent.example.com", stub))
	for path, want := range map[string]string{"/api/me": "dashboard /api/me", "/auth/login": "dashboard /auth/login"} {
		rec := status(mux, http.MethodGet, path)
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("GET %s = %d %q, want %q", path, rec.Code, rec.Body.String(), want)
		}
	}
	if rec := status(mux, http.MethodPost, "/api/repos"); rec.Body.String() != "dashboard /api/repos" {
		t.Errorf("POST /api/repos = %q", rec.Body.String())
	}
	for _, p := range []string{"/", "/repos/acme/widgets"} {
		rec := status(mux, http.MethodGet, p)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("GET %s = %d %q", p, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("GET %s has no CSP", p)
		}
	}
	if rec := status(mux, http.MethodPost, "/webhooks/gitlab/main"); rec.Code != http.StatusUnauthorized {
		t.Errorf("webhook = %d, want 401", rec.Code)
	}
	if rec := status(mux, http.MethodGet, "/healthz"); rec.Body.String() != "ok" {
		t.Errorf("healthz = %q", rec.Body.String())
	}
	// Server routes are never answered with the HTML shell.
	if rec := status(mux, http.MethodGet, "/runs/1/result"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /runs/1/result = %d, want 404", rec.Code)
	}
}

// Every /api/ and /auth/ response, errors included, is uncacheable and not
// content-sniffed, whatever the handler set (or forgot to).
func TestDashboardResponsesNotCachedNorSniffed(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/json":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=60")
			_, _ = io.WriteString(w, "{}")
		case "/api/err":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/auth/redirect":
			http.Redirect(w, r, "/", http.StatusFound)
		default:
			// No headers at all: the body would be sniffed.
			_, _ = io.WriteString(w, "<html>x</html>")
		}
	})
	mux := Routes(dashboardDeps("https://pergent.example.com", stub))
	for _, p := range []string{"/api/json", "/api/err", "/api/plain", "/auth/redirect", "/auth/login"} {
		rec := status(mux, http.MethodGet, p)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", p, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q, want nosniff", p, got)
		}
	}
	// A handler that sets no Content-Type still gets one (never left to sniffing).
	rec := status(mux, http.MethodGet, "/api/plain")
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Errorf("GET /api/plain has no Content-Type")
	}
}

// fetchResults answers every fetch result as stored.
type fetchResults struct{ calls int }

func (f *fetchResults) CompleteJob(context.Context, string, string, []byte) (skillfetch.JobOutcome, error) {
	f.calls++
	return skillfetch.JobStored, nil
}

func (f *fetchResults) JobTooLarge(context.Context, string, string) error { return nil }

// The fetch callback exists only when skill fetches are set, and takes a
// bearer token.
func TestSkillFetchRoute(t *testing.T) {
	cfg := &config.Config{}
	post := func(mux *http.ServeMux, auth string) int {
		req := httptest.NewRequest(http.MethodPost, "/skill-fetches/f1/result", strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(Routes(Deps{Config: cfg, Instances: instances.New(cfg, nil, nil)}), "Bearer t"); code != http.StatusNotFound {
		t.Errorf("without skill fetches = %d; want 404", code)
	}
	fr := &fetchResults{}
	mux := Routes(Deps{Config: cfg, Instances: instances.New(cfg, nil, nil), SkillFetches: fr})
	if code := post(mux, ""); code != http.StatusUnauthorized || fr.calls != 0 {
		t.Errorf("no token = %d (%d calls); want 401", code, fr.calls)
	}
	if code := post(mux, "Bearer t"); code != http.StatusNoContent || fr.calls != 1 {
		t.Errorf("with a token = %d (%d calls); want 204", code, fr.calls)
	}
}

// scanResults answers every scan result as stored.
type scanResults struct{ calls int }

func (s *scanResults) CompleteScan(context.Context, string, string, []byte) (readiness.ScanOutcome, error) {
	s.calls++
	return readiness.ScanStored, nil
}

func (s *scanResults) ScanTooLarge(context.Context, string, string) error { return nil }

// The scan callback exists only when ReadinessScans is set (the dashboard is
// on), and takes a bearer token.
func TestRoutesScanCallbackOnlyWithDashboard(t *testing.T) {
	cfg := &config.Config{}
	post := func(mux *http.ServeMux, auth string) int {
		req := httptest.NewRequest(http.MethodPost, "/readiness-scans/s1/result", strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(Routes(Deps{Config: cfg, Instances: instances.New(cfg, nil, nil)}), "Bearer t"); code != http.StatusNotFound {
		t.Errorf("without readiness scans = %d; want 404", code)
	}
	sr := &scanResults{}
	mux := Routes(Deps{Config: cfg, Instances: instances.New(cfg, nil, nil), ReadinessScans: sr})
	if code := post(mux, ""); code != http.StatusUnauthorized || sr.calls != 0 {
		t.Errorf("no token = %d (%d calls); want 401", code, sr.calls)
	}
	if code := post(mux, "Bearer t"); code != http.StatusNoContent || sr.calls != 1 {
		t.Errorf("with a token = %d (%d calls); want 204", code, sr.calls)
	}
}

// The webhooks check a project's ID and remember deliveries only through the
// store; without one they must hold nil, not a nil store in an interface.
func TestWebhooksGetTheirStores(t *testing.T) {
	handlers := func(st *store.Store) (*webhook.Handler, *webhook.GitHubHandler) {
		cfg := &config.Config{}
		mux := Routes(Deps{Config: cfg, Instances: instances.New(cfg, nil, nil), Store: st})
		gl, _ := mux.Handler(httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/x", nil))
		gh, _ := mux.Handler(httptest.NewRequest(http.MethodPost, "/webhooks/github/x", nil))
		return gl.(*webhook.Handler), gh.(*webhook.GitHubHandler)
	}
	gl, gh := handlers(&store.Store{})
	if gl.Projects == nil || gh.Projects == nil || gh.Deliveries == nil {
		t.Errorf("with a store: GitLab Projects %v, GitHub Projects %v, Deliveries %v", gl.Projects, gh.Projects, gh.Deliveries)
	}
	gl, gh = handlers(nil)
	if gl.Projects != nil || gh.Projects != nil || gh.Deliveries != nil {
		t.Errorf("without a store: GitLab Projects %v, GitHub Projects %v, Deliveries %v", gl.Projects, gh.Projects, gh.Deliveries)
	}
}

// Deps.Pages wraps the web app's pages and nothing else: not /api/, /auth/ or
// the webhooks. (With JWT sign-in a page load starts a session.)
func TestPagesWrapTheAppOnly(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "dashboard") })
	var seen []string
	d := dashboardDeps("https://pergent.example.com", stub)
	d.Pages = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.URL.Path)
			w.Header().Set("X-Wrapped", "yes")
			next.ServeHTTP(w, r)
		})
	}
	mux := Routes(d)
	for _, p := range []string{"/", "/repos/acme/widgets", "/signed-out"} {
		rec := status(mux, http.MethodGet, p)
		if rec.Code != http.StatusOK || rec.Header().Get("X-Wrapped") != "yes" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("GET %s = %d, wrapped %q, type %q; want the app, wrapped", p, rec.Code, rec.Header().Get("X-Wrapped"), rec.Header().Get("Content-Type"))
		}
	}
	for _, p := range []string{"/api/me", "/auth/logout", "/healthz"} {
		if rec := status(mux, http.MethodGet, p); rec.Header().Get("X-Wrapped") != "" {
			t.Errorf("GET %s went through Pages", p)
		}
	}
	if len(seen) != 3 {
		t.Errorf("Pages saw %v; want the three pages", seen)
	}
}
