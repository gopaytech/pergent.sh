package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gopaytech/pergent.sh/internal/password"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// SessionCookieName holds the session ID: 32 random bytes, base64url.
	// The __Host- prefix makes browsers take it only Secure, on Path=/ and
	// without a Domain, so no other host can set or shadow it.
	SessionCookieName = "__Host-pergent_session"
	// CSRFHeader must carry the session's CSRF token on every request
	// that isn't GET, HEAD or OPTIONS.
	CSRFHeader = "X-CSRF-Token"

	// IdleTimeout ends a session not used for this long.
	IdleTimeout = 8 * time.Hour
	// MaxSessionAge ends a local admin's session this long after sign-in,
	// used or not.
	MaxSessionAge = 7 * 24 * time.Hour
	// SSOMaxSessionAge is the same for a session that began with OIDC or a
	// JWT: someone disabled at the identity provider is gone within a day.
	SSOMaxSessionAge = 24 * time.Hour

	touchEvery     = time.Minute
	sessionIDBytes = 32
)

// Session is the signed-in user of a request, put in its context by
// Auth.Require.
type Session struct {
	// AccountID is the account signed in; Kind is its kind,
	// store.AccountPassword (a local admin) or store.AccountSSO.
	AccountID int64
	Kind      string
	// Username is the name shown for the account: the email of an SSO
	// account, the username of a local admin.
	Username string
	// Email is an SSO account's lower-cased email; "" for a local admin.
	Email string
	// SignIn is how the session began: store.SignInPassword, SignInOIDC or
	// SignInJWT.
	SignIn string
	// Admin is set when the account's role is admin (a local admin always
	// is): the session sees and may do everything.
	Admin bool
	// Waiting is set for an account that is not an admin and is on no team:
	// it sees nothing until an admin puts it on one.
	Waiting bool
	// MustChangePassword is set on a local admin that must change its
	// password before anything else.
	MustChangePassword bool
	// All is set when the session sees every repository (an admin).
	All bool
	// Visible holds, for an account that is not an admin, the repositories
	// its teams cover, each with the platform ID of the project stored at
	// that path (0 for one switched on that reconcile has not stored yet).
	// Use CanSee for a repository's own pages and CanSeeRun for its runs.
	Visible map[RepoKey]int64
	// Roles holds the highest role the teams give on each repository they
	// cover; use Role.
	Roles map[RepoKey]Role
	// CSRFToken is the token writes must send in X-CSRF-Token.
	CSRFToken string

	idHash []byte // the stored session's key
}

// CanSee reports whether the user can see project on instance, ignoring
// case.
func (s Session) CanSee(instance, project string) bool {
	if s.All {
		return true
	}
	_, ok := s.Visible[repoKey(instance, project)]
	return ok
}

// CanSeeRun reports whether the user can see a run of project on instance
// under that path: the path must be visible and hold the project with that
// ID, not a project that had the path before (deleted, renamed or
// transferred).
func (s Session) CanSeeRun(instance, project string, id int64) bool {
	return s.All || (id > 0 && s.Visible[repoKey(instance, project)] == id)
}

// Role is the user's role on project on instance: RoleAdmin everywhere for
// an admin, RoleNone where the user can't see it, else the highest of the
// teams covering it.
func (s Session) Role(instance, project string) Role {
	if s.Admin {
		return RoleAdmin
	}
	if !s.CanSee(instance, project) {
		return RoleNone
	}
	return s.Roles[repoKey(instance, project)]
}

type sessionKey struct{}

// SessionFrom returns the session Auth.Require put in ctx.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(Session)
	return s, ok
}

// Auth runs sign-in and sessions.
type Auth struct {
	store *store.Store
	repos *settings.Resolver
	box   *secretbox.Box
	now   func() time.Time
	log   *slog.Logger

	// limit counts password attempts by client IP.
	limit *limiter
	// slots holds a token per argon2id hash or check running (hashSlots);
	// slotWait is how long a request waits for one.
	slots    chan struct{}
	slotWait time.Duration
	// verify checks a password against its hash: password.Verify; hash
	// makes a new password's hash: password.Hash.
	verify func(encoded, pw string) bool
	hash   func(pw string) (string, error)
	// afterVerify, when set (tests), runs after a sign-in's password was
	// found right and before the sign-in is recorded.
	afterVerify func()

	// settings reads the sign-in settings in force and idpClient gives the
	// client for the identity provider (the dashboard's); jwt is the verifier
	// of the proxy's tokens. New fills the first two in.
	settings  func(ctx context.Context) (signIn, error)
	idpClient func() *http.Client
	jwt       jwtVerifier
}

func newAuth(o Options, repos *settings.Resolver) (*Auth, error) {
	box, err := secretbox.New(o.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("dashboard: PERGENT_SH_SECRET_KEY: %w", err)
	}
	return &Auth{
		store: o.Store, repos: repos, box: box,
		now: o.Now, log: o.Log, limit: newLimiter(),
		slots: make(chan struct{}, hashSlots), slotWait: hashSlotWait, verify: password.Verify, hash: password.Hash,
	}, nil
}

// randomString is n random bytes, base64url without padding.
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the platform is broken; never fall back.
		panic("dashboard: reading random bytes: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

// logout ends the session. It is a write: it needs the CSRF token.
func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	hash, ok := sessionHash(r)
	if ok {
		se, found, err := a.store.Session(r.Context(), hash)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not sign out")
			return
		}
		if found {
			if !csrfOK(r, se.CSRF) {
				writeCSRFError(w)
				return
			}
			if err := a.store.DeleteSession(r.Context(), hash); err != nil {
				writeError(w, http.StatusInternalServerError, "could not sign out")
				return
			}
		}
	}
	clearCookie(w, SessionCookieName, "/")
	w.WriteHeader(http.StatusNoContent)
}

// maxAge is the longest a session lives after sign-in: 7 days for a local
// admin's (a password sign-in), 24 hours for one that began with OIDC or a
// JWT.
func maxAge(signIn string) time.Duration {
	if signIn == store.SignInPassword || signIn == "" {
		return MaxSessionAge
	}
	return SSOMaxSessionAge
}

// sessionExpired reports whether a session is past its limits at now: idle
// for more than 8 hours, or older than maxAge for how it began.
func sessionExpired(se store.Session, now time.Time) bool {
	return now.Sub(se.LastSeenAt) > IdleTimeout || now.Sub(se.CreatedAt) > maxAge(se.SignIn)
}

// Require serves next only for a signed-in session that is neither idle
// for 8 hours nor older than its maximum, 7 days for a local admin and 24
// hours for an SSO session (401 otherwise), checks X-CSRF-Token on
// every request other than GET, HEAD and OPTIONS (403), and puts the
// Session in the context. A session whose account is gone ends (401), and so
// does one whose account is of the old platform kind, which no sign-in
// reaches any more; a local admin that must change its password gets 403
// "password_change_required" for everything but GET /api/me, POST
// /auth/logout and POST /auth/password/change.
//
// What the session sees is read from the account on every request, so a role
// or team change applies at once: an admin sees everything; any other account
// sees the repositories its teams cover, with the highest role they give
// (load). On no team it is waiting: 403 "waiting" for everything but GET
// /api/me.
//
// With JWT validation (jwt.go) two things are added. A request with no session
// starts one only if it is a GET of /api/me with a valid token in the proxy's
// header; any other method or path gets 401, however valid the header. A
// session that jwt started needs, on every request, a valid token for its
// account's email, and the mode and issuer it began under (otherwise it
// ends, 401); when the signing keys cannot be fetched the answer is 503 and
// nothing ends. A local admin's session skips all of this.
func (a *Auth) Require(next http.Handler) http.Handler { return a.require(next, false) }

// RequireAdmin is Require for a route only admins may use. Anyone else, an
// account waiting for a team included, gets 404 with the body of an unknown
// path: a user can't tell an admin route from a path that isn't there. It is
// checked after the session, the CSRF token and the sign-in checks, so what
// those say is what an unknown path says too.
func (a *Auth) RequireAdmin(next http.Handler) http.Handler { return a.require(next, true) }

func (a *Auth) require(next http.Handler, adminOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		hash, hasCookie := sessionHash(r)
		var (
			se    store.Session
			found bool
			err   error
		)
		if hasCookie {
			se, found, err = a.store.Session(ctx, hash)
			if err != nil {
				a.log.Error("dashboard: loading a session failed", "error", err.Error())
				writeError(w, http.StatusInternalServerError, "could not load the session")
				return
			}
		}
		// fresh: the header just started this session, so it was just checked.
		fresh := false
		if !found {
			started, ok := a.startOnAPI(w, r, hasCookie)
			if !ok {
				return
			}
			se, hash, fresh = started, started.IDHash, true
		}
		now := a.now()
		if sessionExpired(se, now) {
			if err := a.store.DeleteSession(ctx, hash); err != nil {
				a.log.Error("dashboard: deleting an expired session failed", "error", err.Error())
			}
			clearCookie(w, SessionCookieName, "/")
			writeError(w, http.StatusUnauthorized, "session expired")
			return
		}
		if !safeMethod(r.Method) && !csrfOK(r, se.CSRF) {
			writeCSRFError(w)
			return
		}
		account, found, err := a.store.Account(ctx, se.AccountID)
		if err != nil {
			a.log.Error("dashboard: loading a session's account failed", "error", err.Error())
			writeError(w, http.StatusInternalServerError, "could not load the session")
			return
		}
		// A session of the old platform kind (a previous release's pod can
		// still make one after the upgrade) gets nothing: end it. (A missing
		// account is defensive: sessions are deleted with their account, ON
		// DELETE CASCADE, so the lookup above already answers 401.)
		if !found || account.Kind == store.AccountPlatform {
			if err := a.store.DeleteSession(ctx, hash); err != nil {
				a.log.Error("dashboard: deleting the session of an unusable account failed", "error", err.Error())
			}
			clearCookie(w, SessionCookieName, "/")
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		// A session that jwt started stays only while the proxy keeps vouching
		// for the same person. (A password session has no token: skipped.)
		if se.SignIn == store.SignInJWT && !fresh {
			holds, st := a.jwtSessionHolds(r, se, account.Email)
			if st == headerUnavailable {
				a.log.Warn("dashboard JWT sign-in: the session could not be checked; nothing ended")
				writeError(w, http.StatusServiceUnavailable, msgSignInUnavailable)
				return
			}
			if !holds {
				if err := a.store.DeleteSession(ctx, hash); err != nil {
					a.log.Error("dashboard: ending a JWT session failed", "error", err.Error())
				}
				clearCookie(w, SessionCookieName, "/")
				writeError(w, http.StatusUnauthorized, "not signed in")
				return
			}
		}
		if account.MustChangePassword && !allowedBeforeChange(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "change your password first", "code": "password_change_required"})
			return
		}
		if now.Sub(se.LastSeenAt) >= touchEvery {
			if err := a.store.TouchSession(ctx, hash, now); err != nil {
				a.log.Warn("dashboard: recording session use failed", "error", err.Error())
			}
		}
		acc, err := a.load(ctx, account)
		if err != nil {
			a.log.Error("dashboard: loading session access failed", "error", err.Error())
			writeError(w, http.StatusInternalServerError, "could not load the session")
			return
		}
		if adminOnly && !acc.admin {
			notFound(w)
			return
		}
		if acc.waiting && !allowedWhileWaiting(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "an admin needs to add you to a team", "code": "waiting"})
			return
		}
		s := Session{
			AccountID: account.ID, Kind: account.Kind, Username: account.Username, Email: account.Email,
			SignIn: se.SignIn, Admin: acc.admin, All: acc.admin, Waiting: acc.waiting,
			MustChangePassword: account.MustChangePassword, Visible: acc.visible, Roles: acc.roles,
			CSRFToken: se.CSRF, idHash: se.IDHash,
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, sessionKey{}, s)))
	})
}

// PruneSessions deletes the sessions past their maximum age (7 days, 24
// hours for an SSO session) and those idle for more than 8 hours, which can
// no longer be used. It returns how many.
func (a *Auth) PruneSessions(ctx context.Context, now time.Time) (int, error) {
	n, err := a.store.PruneSessions(ctx, now.Add(-MaxSessionAge), now.Add(-SSOMaxSessionAge), now.Add(-IdleTimeout))
	if err != nil {
		return 0, fmt.Errorf("prune dashboard sessions: %w", err)
	}
	return n, nil
}

// allowedBeforeChange reports whether r is one of the requests a local admin
// that must change its password may make: who am I, sign out and the change
// itself.
func allowedBeforeChange(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/me", "POST /auth/logout", "POST /auth/password/change":
		return true
	}
	return false
}

// allowedWhileWaiting reports whether r is one an account on no team may
// make: who am I. Signing out isn't behind Require.
func allowedWhileWaiting(r *http.Request) bool {
	return r.Method+" "+r.URL.Path == "GET /api/me"
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// writeCSRFError refuses a write whose X-CSRF-Token is missing or wrong:
// 403 with code "csrf", which tells the client to fetch the token again,
// unlike the 403 of a user who isn't allowed.
func writeCSRFError(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing or wrong CSRF token", "code": "csrf"})
}

// csrfOK compares the request's X-CSRF-Token with the session's in
// constant time.
func csrfOK(r *http.Request, want string) bool {
	got := r.Header.Get(CSRFHeader)
	return got != "" && want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// sessionHash is the stored hash of the request's session cookie; ok is
// false without a well-formed one.
func sessionHash(r *http.Request) ([]byte, bool) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(raw) != sessionIDBytes {
		return nil, false
	}
	h := sha256.Sum256(raw)
	return h[:], true
}

// hashID is the stored hash of a session ID made by randomString.
func hashID(id string) []byte {
	raw, _ := base64.RawURLEncoding.DecodeString(id)
	h := sha256.Sum256(raw)
	return h[:]
}
