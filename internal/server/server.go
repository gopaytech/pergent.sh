// Package server holds pergent.sh's HTTP routes, so the binary and the
// end-to-end test serve the same ones.
package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gopaytech/pergent.sh/internal/callback"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
	"github.com/gopaytech/pergent.sh/internal/web"
	"github.com/gopaytech/pergent.sh/internal/webhook"
)

// Deps is what the routes need.
type Deps struct {
	Config *config.Config
	// Instances tells which instances exist, the config's and the
	// dashboard's.
	Instances *instances.Registry
	// WebhookSecrets maps a GitLab instance name to its webhook secret.
	WebhookSecrets *syncmap.Map[string]
	// GitHubWebhookSecrets maps a GitHub instance name to its review App's
	// webhook secret.
	GitHubWebhookSecrets *syncmap.Map[string]
	Store                *store.Store
	// Wake nudges the engine's loops. May be nil.
	Wake func()
	// Now is the clock of the result callback; nil means time.Now.
	Now func() time.Time
	// Dashboard serves /auth/ and /api/. Nil turns the dashboard off.
	Dashboard http.Handler
	Log       *slog.Logger
}

// Routes serves:
//   - POST /webhooks/gitlab/{instance}: GitLab merge request webhooks;
//   - POST /webhooks/github/{instance}: GitHub pull_request webhooks;
//   - POST /runs/{id}/result: a review Job's result;
//   - GET /healthz: 200 "ok";
//   - when Dashboard is set and dashboard.url is configured: /auth/ and /api/
//     (the Dashboard handler) and everything else the embedded app. Without
//     them none of /, /api/ and /auth/ exist.
func Routes(d Deps) *http.ServeMux {
	mux := http.NewServeMux()
	// Webhooks accept repositories switched on in the config or the dashboard.
	repos := &settings.Resolver{Config: d.Config, Store: d.Store, Instances: d.Instances}
	mux.Handle("POST /webhooks/gitlab/{instance}", &webhook.Handler{
		Config: d.Config, Instances: d.Instances, Secrets: d.WebhookSecrets, Queue: d.Store, Repos: repos, Wake: d.Wake, Log: d.Log,
	})
	mux.Handle("POST /webhooks/github/{instance}", &webhook.GitHubHandler{
		Config: d.Config, Instances: d.Instances, Secrets: d.GitHubWebhookSecrets, Queue: d.Store, Repos: repos, Wake: d.Wake, Log: d.Log,
	})
	mux.Handle("POST /runs/{id}/result", &callback.Handler{
		Results: d.Store, Now: d.Now, Wake: d.Wake, Log: d.Log,
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	if d.Dashboard != nil && d.Config != nil && d.Config.DashboardEnabled() {
		guarded := noStore(d.Dashboard)
		mux.Handle("/auth/", guarded)
		mux.Handle("/api/", guarded)
		mux.Handle("/", web.Handler())
	}
	return mux
}

// noStore makes every response of h uncacheable and not content-sniffed:
// /api/ and /auth/ answers are per user. The headers are forced when the
// response starts, so a handler (or an error path) can't leave them out; a
// response with no Content-Type gets text/plain instead of being sniffed.
func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&noStoreWriter{ResponseWriter: w}, r)
	})
}

type noStoreWriter struct {
	http.ResponseWriter
	started bool
}

func (w *noStoreWriter) start(code int) {
	if w.started {
		return
	}
	w.started = true
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if h.Get("Content-Type") == "" && code != http.StatusNoContent && code != http.StatusNotModified {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
}

func (w *noStoreWriter) WriteHeader(code int) {
	// 1xx responses are informational; the real one follows.
	if code >= 200 || code == http.StatusSwitchingProtocols {
		w.start(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *noStoreWriter) Write(b []byte) (int, error) {
	w.start(http.StatusOK)
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer (Flush, etc.).
func (w *noStoreWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
