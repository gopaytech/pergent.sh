package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
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
