// Package dashboard serves the dashboard's sign-in routes (/auth/) and JSON
// API (/api/).
//
// A session's access comes from its account alone. An admin (a local admin
// who signs in with a password, or an SSO account an admin made one) sees and
// does everything. Any other account sees the repositories its teams cover,
// with the highest role its teams give on each. Every API handler runs behind
// Auth.Require, which puts the session, with the repositories its user can
// see and their role on each, in the request's context. Nothing asks GitLab
// or GitHub what a person may read.
//
// Nothing here runs on the engine loop. Calls to GitLab and GitHub are made
// only by the review bot or App, for the "add repository" picker and the
// Skills & instructions view, each with a 10 s timeout; reading a skill
// source to install from runs in the background (skillfetch).
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
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/skillfetch"
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
	// SecretKey is PERGENT_SH_SECRET_KEY: it seals the secrets saved in the
	// dashboard.
	SecretKey string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	Log *slog.Logger
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
	// Wake asks the engine to look for work now (engine.Engine.Wake): the
	// re-run, dismiss and restore buttons call it after they queue a run or
	// ask for a review to be decided again. It must not block. nil leaves
	// the engine to its next pass.
	Wake func()
	// Fetcher reads the skill sources admins install from and check for
	// updates. Nil answers those routes 503.
	Fetcher *skillfetch.Fetcher
	// Readiness queues readiness scans (Scan now) and re-judges the last
	// scans after a skill setting is saved; the engine is one. Nil answers
	// Scan now 404, as with scans off, and saves recheck nothing.
	Readiness Readiness
}

// Readiness is the engine's side of the Readiness tab. RequestScan returns
// readiness.ErrScanBusy or a *readiness.ScanTooSoonError with the scan that
// refused it, and readiness.ErrNotSwitchedOn or readiness.ErrScansOff when
// there is nothing to scan. RecheckReadiness must not block; project ""
// rechecks every repository that follows the default list.
// RecheckAllReadiness rechecks every repository (a library skill's text
// changed); it must not block either.
type Readiness interface {
	RequestScan(ctx context.Context, instance, project, actor string) (store.ReadinessScan, error)
	RecheckReadiness(instance, project string)
	RecheckAllReadiness()
}

// Connector connects instances to GitLab and GitHub while pergent.sh runs;
// the binary implements it. Check tries an instance's credentials and has no
// effect; Connect checks them, then puts the instance in use (reviews and
// webhooks); Disconnect takes it out of use.
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
	// GetChange reads one merge request or pull request: the buttons ask
	// for its head, state and author.
	GetChange(ctx context.Context, projectID, number int64) (forge.Change, error)
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
	sources       *syncmap.Map[RepoSource]
	catalogs      *syncmap.Map[RepoCatalog]
	reconcileSoon func(instance, project string)
	wake          func()
	fetcher       *skillfetch.Fetcher
	readiness     Readiness
	// loadLLM reads the LLM saved in the dashboard again
	// (instances.Registry.LoadLLM); a field so a test can make it fail.
	loadLLM func(ctx context.Context) error
	// heldSecrets lists the secrets a skill may not contain
	// (instances.Registry.HeldSecrets); a field so a test can make it fail.
	heldSecrets func(ctx context.Context) ([]string, error)
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

// New builds the dashboard. The config must have dashboard.url.
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
	if o.Wake == nil {
		o.Wake = func() {}
	}
	return &Dashboard{Auth: auth, Repos: repos, Store: o.Store, Log: o.Log,
		cfg: o.Config, registry: o.Instances, connector: o.Connector,
		sources: o.Sources, catalogs: o.Catalogs, fetcher: o.Fetcher, readiness: o.Readiness,
		reconcileSoon: o.ReconcileSoon, wake: o.Wake, loadLLM: o.Instances.LoadLLM, heldSecrets: o.Instances.HeldSecrets, now: o.Now,
		availableBudget: AvailableBudget, listBudget: listBudget, listLimit: listLimit,
		inputs:  &inputsCache{entries: map[string]inputsEntry{}, flights: map[string]*inputsFlight{}},
		catalog: &catalogCache{entries: map[string]catalogEntry{}, flights: map[string]*catalogFlight{}}}, nil
}

// Handler serves:
//   - POST /auth/logout: end the session (X-CSRF-Token required);
//   - GET /auth/csrf: the password sign-in form's CSRF token, in a cookie
//     and as {"csrfToken"};
//   - POST /auth/password: password sign-in, {username, password} with the
//     form's token in X-CSRF-Token (204 and the session cookie);
//   - POST /auth/password/change: {current, new}, for a password session
//     (204);
//   - GET /api/instances: what the sign-in page offers, without a session;
//   - GET /api/me: the signed-in user;
//   - the read API, the write API, the users, teams and audit log API, the
//     instances, LLM and defaults API, and the skill library API (with
//     installs and updates from git sources), by role (see view.go for each
//     endpoint's body).
//
// Every response is marked no-store.
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/logout", d.Auth.logout)
	mux.HandleFunc("GET /auth/csrf", d.Auth.loginCSRF)
	mux.HandleFunc("POST /auth/password", d.Auth.passwordSignIn)
	mux.Handle("POST /auth/password/change", d.Auth.Require(http.HandlerFunc(d.Auth.changePassword)))
	mux.HandleFunc("GET /api/instances", d.instances)
	mux.Handle("GET /api/me", d.Auth.Require(http.HandlerFunc(d.me)))
	d.writeRoutes(mux)
	d.orgRoutes(mux)
	d.commandRoutes(mux)
	d.operatorRoutes(mux)
	d.readRoutes(mux)
	d.skillWriteRoutes(mux)
	d.skillFetchRoutes(mux)
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
	// Kind is "password" (a local admin) or "sso".
	Kind string `json:"kind"`
	// Username is the name shown: the email of an SSO account.
	Username string `json:"username"`
	// Email is an SSO account's email; "" for a local admin.
	Email string `json:"email"`
	// Role is the account's role: "admin" or "user".
	Role string `json:"role"`
	// SignIn is how the session began: "password", "oidc" or "jwt".
	SignIn string `json:"signIn"`
	// Platform and Instance are "" always. The pages still read them; they
	// go with the pages' next change.
	Platform string `json:"platform"`
	Instance string `json:"instance"`
	// Admin: the account's role is admin; it sees and may do everything.
	Admin bool `json:"admin"`
	// Operator is Admin, for the pages that still ask for it.
	Operator bool `json:"operator"`
	// Waiting: a user on no team, which sees nothing.
	Waiting bool `json:"waiting"`
	// MustChangePassword: nothing else works until the password is changed.
	MustChangePassword bool `json:"mustChangePassword"`
	// WaitingUsers is 0 always (the pages still read it).
	WaitingUsers int    `json:"waitingUsers"`
	CSRFToken    string `json:"csrfToken"`
}

func (d *Dashboard) me(w http.ResponseWriter, r *http.Request) {
	se, _ := SessionFrom(r.Context())
	role := store.RoleUser
	if se.Admin {
		role = store.RoleAdmin
	}
	writeJSON(w, http.StatusOK, meView{
		Kind: se.Kind, Username: se.Username, Email: se.Email, Role: role, SignIn: se.SignIn,
		Admin: se.Admin, Operator: se.Admin, Waiting: se.Waiting, MustChangePassword: se.MustChangePassword,
		CSRFToken: se.CSRFToken,
	})
}

// instancesView is the body of GET /api/instances.
type instancesView struct {
	// Password: the password form is shown. It is always true: a dashboard
	// that answers has local admins.
	Password bool `json:"password"`
	// Instances are always empty: no instance offers a sign-in any more.
	Instances []InstanceView `json:"instances"`
}

// instances answers the sign-in page's question. It needs no session and
// says nothing but that the password form is there.
func (d *Dashboard) instances(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, instancesView{Password: true, Instances: []InstanceView{}})
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
