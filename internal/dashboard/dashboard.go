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
	// HTTPClient talks to the identity provider: its discovery document, key
	// set and token endpoint, and the sign-in test button. Nil means a client
	// with a 10 s timeout. Either way no redirect is followed. A test sets one
	// that trusts its fake provider.
	HTTPClient *http.Client
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
	// CanReach reports whether ReachableRepos would list project; a
	// failure to tell is an error.
	CanReach(ctx context.Context, project string) (bool, error)
}

// RepoSource reads a repository, named by its platform ID, with the review
// bot or the review App; gitlabforge.Forge and githubforge.Forge are ones.
type RepoSource interface {
	inputs.Source
	DefaultBranch(ctx context.Context, projectID int64) (string, error)
	// GetChange reads one merge request or pull request: the buttons ask
	// for its head and state.
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
	// idp is the client for the identity provider (idpClient), oidc its last
	// discovery document.
	idp     *http.Client
	oidc    *oidcCache
	inputs  *inputsCache
	catalog *catalogCache
	// linkInfo keeps what the list of repositories to link read about each.
	linkInfo linkInfoCache
	// adminRoutes are the patterns Handler registered through adminRoute: a
	// test walks them to prove each is not found for a user.
	adminRoutes []string
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
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{}
	}
	d := &Dashboard{Auth: auth, Repos: repos, Store: o.Store, Log: o.Log,
		cfg: o.Config, registry: o.Instances, connector: o.Connector,
		sources: o.Sources, catalogs: o.Catalogs, fetcher: o.Fetcher, readiness: o.Readiness,
		reconcileSoon: o.ReconcileSoon, wake: o.Wake, loadLLM: o.Instances.LoadLLM, heldSecrets: o.Instances.HeldSecrets, now: o.Now,
		availableBudget: AvailableBudget, listBudget: listBudget, listLimit: listLimit,
		idp: idpClient(o.HTTPClient), oidc: &oidcCache{},
		inputs:  &inputsCache{entries: map[string]inputsEntry{}, flights: map[string]*inputsFlight{}},
		catalog: &catalogCache{entries: map[string]catalogEntry{}, flights: map[string]*catalogFlight{}}}
	auth.settings = d.signInSettings
	auth.idpClient = func() *http.Client { return d.idp }
	return d, nil
}

// Handler serves:
//   - POST /auth/logout: end the session (X-CSRF-Token required);
//   - GET /auth/csrf: the password sign-in form's CSRF token, in a cookie
//     and as {"csrfToken"};
//   - POST /auth/password: password sign-in, {username, password} with the
//     form's token in X-CSRF-Token (204 and the session cookie);
//   - POST /auth/password/change: {current, new}, for a password session
//     (204);
//   - GET /auth/oidc/login: send the browser to the OIDC provider (state,
//     nonce and PKCE verifier in a sealed, short-lived cookie);
//   - GET /auth/oidc/callback: finish OIDC sign-in, then the allowlist; every
//     refusal redirects to /sign-in?error=<code>;
//   - GET /api/sign-in-options: what the sign-in pages offer, without a
//     session;
//   - GET /api/me: the signed-in user (with JWT sign-in it also starts a
//     session, for a valid token and no session: see jwt.go; Pages does the
//     same for the web app's pages);
//   - the read API, the write API, the users, teams and audit log API, the
//     instances, LLM and defaults API, and the skill library API (with
//     installs and updates from git sources), by role (see view.go for each
//     endpoint's body).
//
// Every response is marked no-store.
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	d.adminRoutes = nil
	mux.HandleFunc("POST /auth/logout", d.Auth.logout)
	mux.HandleFunc("GET /auth/csrf", d.Auth.loginCSRF)
	mux.HandleFunc("POST /auth/password", d.Auth.passwordSignIn)
	mux.Handle("POST /auth/password/change", d.Auth.Require(http.HandlerFunc(d.Auth.changePassword)))
	mux.HandleFunc("GET /api/sign-in-options", d.signInOptions)
	mux.HandleFunc("GET /auth/oidc/login", d.oidcLogin)
	mux.HandleFunc("GET /auth/oidc/callback", d.oidcCallback)
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
		// The OIDC callback's URL holds the authorization code and the state:
		// never pass it on.
		h.Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

// adminRoute registers an API route only admins may use, and is the only way
// one is registered. Auth.RequireAdmin answers anyone else 404 with the body
// of an unknown path, so the server enforces the admin area's data and a user
// can't tell that the route exists. A route that serves users too is
// registered with Auth.Require, and checks in its handler what each may do.
// A repository's .../teams is the one admin-only route that is not
// registered here: its path pattern is shared with the repository's other pages,
// so its handler answers a non-admin 404 itself (TestRepositoryTeamsAreForAdmins).
func (d *Dashboard) adminRoute(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	d.adminRoutes = append(d.adminRoutes, pattern)
	mux.Handle(pattern, d.Auth.RequireAdmin(h))
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
	// Admin: the account's role is admin; it sees and may do everything.
	Admin bool `json:"admin"`
	// Waiting: a user on no team, which sees nothing.
	Waiting bool `json:"waiting"`
	// MustChangePassword: nothing else works until the password is changed.
	MustChangePassword bool `json:"mustChangePassword"`
	// WaitingUsers is, for an admin, how many users are waiting for a team
	// (the Users list marks each of them waiting); 0 for anyone else.
	WaitingUsers int    `json:"waitingUsers"`
	CSRFToken    string `json:"csrfToken"`
}

func (d *Dashboard) me(w http.ResponseWriter, r *http.Request) {
	se, _ := SessionFrom(r.Context())
	role, waiting := store.RoleUser, 0
	if se.Admin {
		role = store.RoleAdmin
		n, err := d.Store.CountWaitingAccounts(r.Context())
		if err != nil {
			d.failed(w, r, "counting the users waiting for a team", err)
			return
		}
		waiting = n
	}
	writeJSON(w, http.StatusOK, meView{
		Kind: se.Kind, Username: se.Username, Email: se.Email, Role: role, SignIn: se.SignIn,
		Admin: se.Admin, Waiting: se.Waiting, MustChangePassword: se.MustChangePassword,
		WaitingUsers: waiting, CSRFToken: se.CSRFToken,
	})
}

// signInOptionsView is the body of GET /api/sign-in-options.
type signInOptionsView struct {
	// Password: the password form is shown, on /admin/sign-in. It is always
	// true: a dashboard that answers has local admins.
	Password bool `json:"password"`
	// SSO is the OIDC button, on /sign-in and /admin/sign-in; absent when
	// OIDC sign-in is not set up.
	SSO *ssoView `json:"sso,omitempty"`
	// JWT: a proxy signs people in. There is no button; loading a page signs
	// the person in.
	JWT bool `json:"jwt"`
}

// signInOptions answers the sign-in pages' question. It needs no session. It
// names no instance, client or address: only whether the password form is
// there, the label of the SSO button, and whether a proxy signs people in.
func (d *Dashboard) signInOptions(w http.ResponseWriter, r *http.Request) {
	sso, jwt := d.signInChoices(r.Context())
	writeJSON(w, http.StatusOK, signInOptionsView{Password: true, SSO: sso, JWT: jwt})
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

// lower is a project path as RepoKey holds it.
func lower(project string) string { return strings.ToLower(project) }
