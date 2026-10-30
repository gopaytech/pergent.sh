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
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/web"
	"github.com/gopaytech/pergent.sh/internal/webhook"
)

// Deps is what the routes need.
type Deps struct {
	Config *config.Config
	// WebhookSecrets maps an instance name to its webhook secret.
	WebhookSecrets map[string]string
	// GitHubWebhookSecrets maps a GitHub instance name to its review App's
	// webhook secret.
	GitHubWebhookSecrets map[string]string
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
	repos := &settings.Resolver{Config: d.Config, Store: d.Store}
	mux.Handle("POST /webhooks/gitlab/{instance}", &webhook.Handler{
		Config: d.Config, Secrets: d.WebhookSecrets, Queue: d.Store, Repos: repos, Wake: d.Wake, Log: d.Log,
	})
	mux.Handle("POST /webhooks/github/{instance}", &webhook.GitHubHandler{
		Config: d.Config, Secrets: d.GitHubWebhookSecrets, Queue: d.Store, Repos: repos, Wake: d.Wake, Log: d.Log,
	})
	mux.Handle("POST /runs/{id}/result", &callback.Handler{
		Results: d.Store, Now: d.Now, Wake: d.Wake, Log: d.Log,
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	if d.Dashboard != nil && d.Config != nil && d.Config.DashboardEnabled() {
		mux.Handle("/auth/", d.Dashboard)
		mux.Handle("/api/", d.Dashboard)
		mux.Handle("/", web.Handler())
	}
	return mux
}
