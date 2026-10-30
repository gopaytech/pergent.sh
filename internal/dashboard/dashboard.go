// Package dashboard serves the dashboard's sign-in routes (/auth/) and JSON
// API (/api/).
//
// Users sign in with GitLab or GitHub (OAuth, one platform per session) and
// see only the repositories they can read there: every API handler runs
// behind Auth.Require, which puts the session, with the repositories its
// user can see and whether they are an admin, in the request's context. The
// user's OAuth token is stored sealed and never leaves the server.
//
// Nothing here runs on the engine loop. Calls to GitLab and GitHub are made
// only for sign-in, access checks and the Skills & instructions view, each
// with a 10 s timeout.
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// platformTimeout bounds each call to GitLab or GitHub.
const platformTimeout = 10 * time.Second

// Options is what New needs.
type Options struct {
	Config *config.Config
	Store  *store.Store
	// SecretKey is PERGENT_SH_SECRET_KEY: it seals the users' OAuth tokens
	// and signs the OAuth state cookie.
	SecretKey string
	// HTTPClient makes the calls to GitLab and GitHub; nil means a client
	// with a 10 s timeout.
	HTTPClient *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	Log *slog.Logger
	// Library is the skill library: GET /api/skills shows it, and the
	// Skills & instructions view resolves repositories' skills from it.
	Library inputs.Library
	// Sources read repositories' files and default branches, by instance
	// name: the review bot's Forge on GitLab, the review App's on GitHub.
	// An instance without one has no Skills & instructions view (502).
	Sources map[string]RepoSource
}

// RepoSource reads a repository, named by its platform ID, with the review
// bot or the review App; gitlabforge.Forge and githubforge.Forge are ones.
type RepoSource interface {
	inputs.Source
	DefaultBranch(ctx context.Context, projectID int64) (string, error)
}

// Dashboard is the dashboard's routes and what they share.
type Dashboard struct {
	Auth *Auth
	// Repos resolves the repositories switched on, from the config and
	// the dashboard.
	Repos *settings.Resolver
	Store *store.Store
	Log   *slog.Logger

	cfg     *config.Config
	library inputs.Library
	sources map[string]RepoSource
	now     func() time.Time
	inputs  *inputsCache
}

// New builds the dashboard. The config must have dashboard.url; every
// instance with OAuth env names must have them set in the environment. With
// no admins configured it logs a warning: nobody gets the Admin menu.
func New(o Options) (*Dashboard, error) {
	if o.Config == nil || !o.Config.DashboardEnabled() {
		return nil, errors.New("dashboard: dashboard.url is not set")
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: platformTimeout}
	}
	repos := &settings.Resolver{Config: o.Config, Store: o.Store}
	auth, err := newAuth(o, repos)
	if err != nil {
		return nil, err
	}
	if len(o.Config.Dashboard.Admins) == 0 {
		o.Log.Warn("dashboard has no admins configured")
	}
	return &Dashboard{Auth: auth, Repos: repos, Store: o.Store, Log: o.Log,
		cfg: o.Config, library: o.Library, sources: o.Sources, now: o.Now,
		inputs: &inputsCache{entries: map[string]inputsEntry{}, flights: map[string]*inputsFlight{}}}, nil
}

// Handler serves:
//   - GET /auth/{platform}/{instance}/login: redirect to the provider;
//   - GET /auth/{platform}/{instance}/callback: finish sign-in, redirect to /;
//   - POST /auth/logout: end the session (X-CSRF-Token required);
//   - GET /api/me: the signed-in user;
//   - the read API (see view.go for each endpoint's body).
//
// Every response is marked no-store.
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/{platform}/{instance}/login", d.Auth.login)
	mux.HandleFunc("GET /auth/{platform}/{instance}/callback", d.Auth.callback)
	mux.HandleFunc("POST /auth/logout", d.Auth.logout)
	mux.Handle("GET /api/me", d.Auth.Require(http.HandlerFunc(d.me)))
	d.readRoutes(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		// The callback's URL holds the OAuth code: never pass it on.
		h.Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

// meView is the body of GET /api/me.
type meView struct {
	Platform  string `json:"platform"`
	Instance  string `json:"instance"`
	Username  string `json:"username"`
	IsAdmin   bool   `json:"isAdmin"`
	CSRFToken string `json:"csrfToken"`
}

func (d *Dashboard) me(w http.ResponseWriter, r *http.Request) {
	se, _ := SessionFrom(r.Context())
	writeJSON(w, http.StatusOK, meView{
		Platform: se.Platform, Instance: se.Instance, Username: se.Username,
		IsAdmin: se.IsAdmin, CSRFToken: se.CSRFToken,
	})
}

// writeJSON writes v as the JSON body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes {"error": message}. The message is for the user: it
// never holds a token or an upstream response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeText writes a short plain-text answer, for the browser-facing
// sign-in routes.
func writeText(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// lower is the key of a project path in Session.Visible.
func lower(project string) string { return strings.ToLower(project) }
