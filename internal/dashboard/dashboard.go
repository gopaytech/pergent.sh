// Package dashboard serves the dashboard's sign-in routes (/auth/) and JSON
// API (/api/).
//
// Operators sign in with a password and see everything. Everyone else signs
// in with GitLab or GitHub (OAuth, one platform per session) and sees the
// repositories their teams cover that they can read there: every API
// handler runs behind Auth.Require, which puts the session, with the
// repositories its user can see and their role on each, in the request's
// context. The user's OAuth token is stored sealed and never leaves the
// server.
//
// Nothing here runs on the engine loop. Calls to GitLab and GitHub are made
// only for sign-in, access checks, the "add repository" picker and the
// Skills & instructions view, each with a 10 s timeout.
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
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// platformTimeout bounds each call to GitLab or GitHub.
const platformTimeout = 10 * time.Second

// Options is what New needs.
type Options struct {
	Config *config.Config
	Store  *store.Store
	// Instances tells which instances exist, the config's and the
	// dashboard's. Nil means the config's alone.
	Instances *instances.Registry
	// Connector connects and disconnects instances as they are added and
	// removed in the dashboard. Nil leaves the instances as they are.
	Connector Connector
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
	Sources *syncmap.Map[RepoSource]
	// Catalogs list, by instance name, the repositories the review bot
	// (GitLab) or the review App (GitHub) can set up, for the "add
	// repository" picker. An instance without one can't add any (502).
	Catalogs *syncmap.Map[RepoCatalog]
	// ReconcileSoon asks the engine for an early reconcile of the
	// repository project on instance after it is switched on or off
	// (engine.Engine.ReconcileSoon). It must not block. nil leaves it to
	// the hourly reconcile.
	ReconcileSoon func(instance, project string)
}

// Connector connects instances to GitLab and GitHub while pergent.sh runs;
// the binary implements it. Check tries an instance's credentials and has no
// effect; Connect checks them, then puts the instance in use (reviews,
// webhooks and sign-in); Disconnect takes it out of use.
type Connector interface {
	Check(ctx context.Context, inst instances.Instance, s instances.Secrets) error
	Connect(ctx context.Context, inst instances.Instance, s instances.Secrets) error
	Disconnect(name string)
}

// RepoCatalog is what the review side of an instance can set up: on GitLab
// the review bot's projects where it is Maintainer or above (it creates
// their webhooks and deploy tokens), on GitHub the repositories of the
// review App's installations. gitlabforge.Forge and githubforge.Forge are
// ones. Each request it makes has its own 10 s.
type RepoCatalog interface {
	// ReachableRepos lists them, those that hold search ("" for all), up
	// to limit; truncated is true when there were more. On an error it
	// also returns those read before it.
	ReachableRepos(ctx context.Context, search string, limit int) (repos []string, truncated bool, err error)
	// CanReach reports whether project is one; a failure to tell is an
	// error.
	CanReach(ctx context.Context, project string) (bool, error)
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

	cfg           *config.Config
	registry      *instances.Registry
	connector     Connector
	library       inputs.Library
	sources       *syncmap.Map[RepoSource]
	catalogs      *syncmap.Map[RepoCatalog]
	reconcileSoon func(instance, project string)
	// availableBudget bounds the whole picker request; listBudget bounds
	// each side's listing in it; listLimit is the most repositories read
	// from each side.
	availableBudget time.Duration
	listBudget      time.Duration
	listLimit       int
	now             func() time.Time
	inputs          *inputsCache
	catalog         *catalogCache
}

// New builds the dashboard. The config must have dashboard.url. Sign-in
// through an instance starts when the instance is connected
// (Auth.SetProvider).
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
	if o.Instances == nil {
		o.Instances = instances.New(o.Config, nil, nil)
	}
	repos := &settings.Resolver{Config: o.Config, Store: o.Store, Instances: o.Instances}
	auth, err := newAuth(o, repos)
	if err != nil {
		return nil, err
	}
	if o.ReconcileSoon == nil {
		o.ReconcileSoon = func(string, string) {}
	}
	return &Dashboard{Auth: auth, Repos: repos, Store: o.Store, Log: o.Log,
		cfg: o.Config, registry: o.Instances, connector: o.Connector,
		library: o.Library, sources: o.Sources, catalogs: o.Catalogs,
		reconcileSoon: o.ReconcileSoon, now: o.Now,
		availableBudget: AvailableBudget, listBudget: listBudget, listLimit: listLimit,
		inputs:  &inputsCache{entries: map[string]inputsEntry{}, flights: map[string]*inputsFlight{}},
		catalog: &catalogCache{entries: map[string]catalogEntry{}, flights: map[string]*catalogFlight{}}}, nil
}

// Handler serves:
//   - GET /auth/{platform}/{instance}/login: redirect to the provider;
//   - GET /auth/{platform}/{instance}/callback: finish sign-in, redirect to /;
//   - POST /auth/logout: end the session (X-CSRF-Token required);
//   - GET /auth/csrf: the password sign-in form's CSRF token, in a cookie
//     and as {"csrfToken"};
//   - POST /auth/password: password sign-in, {username, password} with the
//     form's token in X-CSRF-Token (204 and the session cookie);
//   - POST /auth/password/change: {current, new}, for a password session
//     (204);
//   - GET /api/instances: the instances to sign in with, without a session;
//   - GET /api/me: the signed-in user;
//   - the read API, the write API and the users, teams and audit log API,
//     by role (see view.go for each endpoint's body).
//
// Every response is marked no-store.
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/{platform}/{instance}/login", d.Auth.login)
	mux.HandleFunc("GET /auth/{platform}/{instance}/callback", d.Auth.callback)
	mux.HandleFunc("POST /auth/logout", d.Auth.logout)
	mux.HandleFunc("GET /auth/csrf", d.Auth.loginCSRF)
	mux.HandleFunc("POST /auth/password", d.Auth.passwordSignIn)
	mux.Handle("POST /auth/password/change", d.Auth.Require(http.HandlerFunc(d.Auth.changePassword)))
	mux.HandleFunc("GET /api/instances", d.instances)
	mux.Handle("GET /api/me", d.Auth.Require(http.HandlerFunc(d.me)))
	d.writeRoutes(mux)
	d.orgRoutes(mux)
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
	// Kind is "password" or "platform".
	Kind     string `json:"kind"`
	Username string `json:"username"`
	// Platform and Instance are "" for a password account.
	Platform string `json:"platform"`
	Instance string `json:"instance"`
	// Admin: an operator or a member of an admin team.
	Admin bool `json:"admin"`
	// Operator: a password account, which sees everything.
	Operator bool `json:"operator"`
	// Waiting: a platform account on no team, which sees nothing.
	Waiting bool `json:"waiting"`
	// MustChangePassword: nothing else works until the password is changed.
	MustChangePassword bool `json:"mustChangePassword"`
	// WaitingUsers counts the platform accounts on no team: of every
	// instance for an operator, of their own for a platform admin; 0 for
	// everyone else.
	WaitingUsers int    `json:"waitingUsers"`
	CSRFToken    string `json:"csrfToken"`
}

func (d *Dashboard) me(w http.ResponseWriter, r *http.Request) {
	se, _ := SessionFrom(r.Context())
	view := meView{
		Kind: se.Kind, Username: se.Username, Platform: se.Platform, Instance: se.Instance,
		Admin: se.Admin, Operator: se.Operator, Waiting: se.Waiting, MustChangePassword: se.MustChangePassword,
		CSRFToken: se.CSRFToken,
	}
	if se.Admin {
		// An operator has no instance: "" counts every one.
		n, err := d.Store.CountWaitingAccounts(r.Context(), se.Instance)
		if err != nil {
			d.failed(w, r, "counting waiting accounts", err)
			return
		}
		view.WaitingUsers = n
	}
	writeJSON(w, http.StatusOK, view)
}

// instancesView is the body of GET /api/instances.
type instancesView struct {
	// Password: the password form is shown. It is always true: a dashboard
	// that answers has password accounts.
	Password bool `json:"password"`
	// Instances are those with sign-in, the config's first.
	Instances []InstanceView `json:"instances"`
}

// instances lists the instances people can sign in through now (those with a
// provider), GitLab first, each platform's in the registry's order (the
// config's, then the dashboard's): the sign-in page shows a button for each,
// and the password form. It needs no session and says nothing but their
// names.
func (d *Dashboard) instances(w http.ResponseWriter, _ *http.Request) {
	view := instancesView{Password: true, Instances: []InstanceView{}}
	all := d.registry.All()
	for _, platform := range []struct{ id, label string }{
		{config.PlatformGitLab, "GitLab"}, {config.PlatformGitHub, "GitHub"},
	} {
		for _, inst := range all {
			if inst.Platform == platform.id && !inst.Removing && d.Auth.canSignIn(inst.Platform, inst.Name) {
				view.Instances = append(view.Instances,
					InstanceView{Platform: inst.Platform, Instance: inst.Name, Label: platform.label + " (" + inst.Name + ")"})
			}
		}
	}
	writeJSON(w, http.StatusOK, view)
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

// lower is a project path as RepoKey holds it.
func lower(project string) string { return strings.ToLower(project) }
