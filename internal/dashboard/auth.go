package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
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
	// MaxSessionAge ends a session this long after sign-in, used or not.
	MaxSessionAge = 7 * 24 * time.Hour
	// AccessTTL is how long a check of the user's repositories and admin
	// flag is used before it is made again. A failed check keeps the last
	// one for one more AccessTTL, then the session sees nothing.
	AccessTTL = 5 * time.Minute

	stateCookieName = "__Host-pergent_oauth_state"
	stateTTL        = 10 * time.Minute
	touchEvery      = time.Minute
	sessionIDBytes  = 32
	// signInTimeout bounds the whole sign-in or access check; each call to
	// the platform has its own 10 s timeout.
	signInTimeout = 30 * time.Second
)

// Session is the signed-in user of a request, put in its context by
// Auth.Require.
type Session struct {
	// AccountID is the account signed in; Kind is its kind,
	// store.AccountPassword or store.AccountPlatform.
	AccountID int64
	Kind      string
	// Platform is config.PlatformGitLab or config.PlatformGitHub; a
	// password session has none, no Instance and no UserID.
	Platform, Instance string
	UserID             int64
	Username           string
	// IsAdmin is true for a member of the instance's admin group or team,
	// and for every password account.
	IsAdmin bool
	// MustChangePassword is set on a password account that must change its
	// password before anything else.
	MustChangePassword bool
	// Visible holds the repositories on Instance the user can see: their
	// platform IDs by lower-cased path. Use CanSee for a repository's own
	// pages and CanSeeRun for its runs.
	Visible map[string]int64
	// CSRFToken is the token writes must send in X-CSRF-Token.
	CSRFToken string

	idHash []byte // the stored session's key
}

// CanSee reports whether the user can see project (on the session's
// instance), ignoring case.
func (s Session) CanSee(project string) bool { return s.Visible[lower(project)] > 0 }

// CanSeeRun reports whether the user can see a run of project under that
// path: the path must be visible and hold the project with that ID, not a
// project that had the path before (deleted, renamed or transferred).
func (s Session) CanSeeRun(project string, id int64) bool {
	return id > 0 && s.Visible[lower(project)] == id
}

type sessionKey struct{}

// SessionFrom returns the session Auth.Require put in ctx.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(Session)
	return s, ok
}

// Auth runs sign-in and sessions.
type Auth struct {
	cfg       *config.Config
	store     *store.Store
	repos     *settings.Resolver
	box       *secretbox.Box
	stateKey  []byte
	providers map[string]provider // by instance name
	now       func() time.Time
	log       *slog.Logger

	// limit counts password attempts by client IP.
	limit *limiter

	mu       sync.Mutex
	flights  map[string]*flight   // access checks in progress, by session hash
	failedAt map[string]time.Time // last failed access check, by session hash

	// renewMu is held while a token is renewed: a refresh token may be
	// good for one use only, so two requests must not both spend it.
	renewMu sync.Mutex
}

func newAuth(o Options, repos *settings.Resolver) (*Auth, error) {
	box, err := secretbox.New(o.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("dashboard: PERGENT_SH_SECRET_KEY: %w", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(o.SecretKey) // secretbox.New checked it
	mac := hmac.New(sha256.New, raw)
	mac.Write([]byte("pergent-sh dashboard oauth state v1"))

	a := &Auth{
		cfg: o.Config, store: o.Store, repos: repos, box: box, stateKey: mac.Sum(nil),
		providers: map[string]provider{}, now: o.Now, log: o.Log, limit: newLimiter(),
		flights: map[string]*flight{}, failedAt: map[string]time.Time{},
	}
	base := o.Config.Dashboard.URL
	for _, inst := range o.Config.GitLab {
		if !inst.HasOAuth() {
			continue
		}
		id, secret, err := inst.OAuthClient()
		if err != nil {
			return nil, err
		}
		a.providers[inst.Name] = &gitlabProvider{
			baseURL: strings.TrimRight(inst.URL, "/"), clientID: id, clientSecret: secret,
			redirectURI: base + "/auth/gitlab/" + inst.Name + "/callback", hc: o.HTTPClient, now: o.Now,
			timeout: platformTimeout,
		}
	}
	for _, inst := range o.Config.GitHub {
		if !inst.HasOAuth() {
			continue
		}
		id, secret, err := inst.OAuthClient()
		if err != nil {
			return nil, err
		}
		a.providers[inst.Name] = &githubProvider{
			webURL: githubWebURL(inst.APIURL), apiURL: inst.APIURL, clientID: id, clientSecret: secret,
			redirectURI: base + "/auth/github/" + inst.Name + "/callback", hc: o.HTTPClient, now: o.Now,
			timeout: platformTimeout,
		}
	}
	return a, nil
}

// provider returns the sign-in of the request's {platform} and {instance};
// ok is false for an instance without sign-in or on another platform.
func (a *Auth) provider(r *http.Request) (provider, string, string, bool) {
	platform, instance := r.PathValue("platform"), r.PathValue("instance")
	p, ok := a.providers[instance]
	if !ok || p.platform() != platform {
		return nil, "", "", false
	}
	return p, platform, instance, true
}

// --- OAuth state cookie ---

// oauthState is what the state cookie carries between login and callback.
type oauthState struct {
	Platform string `json:"p"`
	Instance string `json:"i"`
	State    string `json:"s"`
	Verifier string `json:"v"`
	Expires  int64  `json:"e"`
}

func (a *Auth) sign(payload string) string {
	mac := hmac.New(sha256.New, a.stateKey)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Auth) setStateCookie(w http.ResponseWriter, st oauthState) {
	data, _ := json.Marshal(st)
	payload := base64.RawURLEncoding.EncodeToString(data)
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: payload + "." + a.sign(payload), Path: "/",
		MaxAge: int(stateTTL / time.Second), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

// readState returns the state cookie's content if its signature is good,
// it was made for platform/instance and it hasn't expired.
func (a *Auth) readState(r *http.Request, platform, instance string) (oauthState, bool) {
	c, err := r.Cookie(stateCookieName)
	if err != nil {
		return oauthState{}, false
	}
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.sign(payload))) {
		return oauthState{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return oauthState{}, false
	}
	var st oauthState
	if json.Unmarshal(data, &st) != nil {
		return oauthState{}, false
	}
	if st.Platform != platform || st.Instance != instance || st.State == "" || st.Verifier == "" ||
		a.now().Unix() > st.Expires {
		return oauthState{}, false
	}
	return st, true
}

// --- routes ---

// login sends the browser to the provider. Nothing from the request but
// the route chooses where: no "next" parameter, no Host header.
func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	p, platform, instance, ok := a.provider(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	st := oauthState{
		Platform: platform, Instance: instance,
		State: randomString(32), Verifier: randomString(32),
		Expires: a.now().Add(stateTTL).Unix(),
	}
	a.setStateCookie(w, st)
	http.Redirect(w, r, p.authorizeURL(st.State, pkceChallenge(st.Verifier)), http.StatusFound)
}

// callback finishes sign-in: it checks the state, trades the code for a
// token, makes a new session (ending the one the browser came with), checks
// the user's access and sends the browser to /.
func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	p, platform, instance, ok := a.provider(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	clearCookie(w, stateCookieName, "/")
	q := r.URL.Query()
	st, ok := a.readState(r, platform, instance)
	if !ok || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		writeText(w, http.StatusBadRequest, "Sign-in expired or was started in another window. Please sign in again.")
		return
	}
	if q.Get("error") != "" {
		writeText(w, http.StatusForbidden, "Sign-in was not completed.")
		return
	}
	code := q.Get("code")
	if code == "" {
		writeText(w, http.StatusBadRequest, "Sign-in answer without a code. Please sign in again.")
		return
	}
	log := a.log.With("platform", platform, "instance", instance)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), signInTimeout)
	defer cancel()

	tok, err := p.exchange(ctx, code, st.Verifier)
	if err != nil {
		log.Warn("dashboard sign-in: code exchange failed", "error", err.Error())
		writeText(w, http.StatusBadGateway, "Sign-in failed. Please try again.")
		return
	}
	userID, username, err := p.user(ctx, tok.AccessToken)
	if err != nil {
		log.Warn("dashboard sign-in: reading the user failed", "error", err.Error())
		writeText(w, http.StatusBadGateway, "Sign-in failed. Please try again.")
		return
	}
	log = log.With("user", username)

	// Never adopt the session the browser arrived with: end it.
	if hash, ok := sessionHash(r); ok {
		if err := a.store.DeleteSession(ctx, hash); err != nil {
			log.Error("dashboard sign-in: ending the previous session failed", "error", err.Error())
			writeText(w, http.StatusInternalServerError, "Sign-in failed. Please try again.")
			return
		}
	}
	sealed, err := a.seal(tok)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "Sign-in failed. Please try again.")
		return
	}
	id := randomString(sessionIDBytes)
	hash := hashID(id)
	now := a.now()
	account, err := a.store.UpsertPlatformAccount(ctx, platform, instance, userID, username, now)
	if err != nil {
		log.Error("dashboard sign-in: storing the account failed", "error", err.Error())
		writeText(w, http.StatusInternalServerError, "Sign-in failed. Please try again.")
		return
	}
	se := store.Session{
		IDHash: hash, AccountID: account.ID, Platform: platform, Instance: instance, UserID: userID, Username: username,
		TokenSealed: sealed, CSRF: randomString(32), LastSeenAt: now, CreatedAt: now,
	}
	if err := a.store.CreateSession(ctx, se); err != nil {
		log.Error("dashboard sign-in: storing the session failed", "error", err.Error())
		writeText(w, http.StatusInternalServerError, "Sign-in failed. Please try again.")
		return
	}
	if _, err := a.checkAccess(ctx, se, tok); err != nil {
		log.Warn("dashboard sign-in: access check failed", "error", err.Error())
		if err := a.store.DeleteSession(ctx, hash); err != nil {
			log.Error("dashboard sign-in: removing the unchecked session failed", "error", err.Error())
		}
		writeText(w, http.StatusBadGateway, "Could not check which repositories you can see. Please try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: id, Path: "/", MaxAge: int(MaxSessionAge / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	log.Info("dashboard sign-in")
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
			a.forget(hash)
		}
	}
	clearCookie(w, SessionCookieName, "/")
	w.WriteHeader(http.StatusNoContent)
}

// Require serves next only for a signed-in session that is neither idle
// for 8 hours nor older than 7 days (401 otherwise), checks X-CSRF-Token on
// every request other than GET, HEAD and OPTIONS (403), and puts the
// Session in the context, with its access checked within the last 5
// minutes or, failing that, as described at AccessTTL. A session whose
// account is gone ends (401); a password account that must change its
// password gets 403 "password_change_required" for everything but
// GET /api/me, POST /auth/logout and POST /auth/password/change. A password
// session has no platform check: it is an admin.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		hash, ok := sessionHash(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		se, found, err := a.store.Session(ctx, hash)
		if err != nil {
			a.log.Error("dashboard: loading a session failed", "error", err.Error())
			writeError(w, http.StatusInternalServerError, "could not load the session")
			return
		}
		if !found {
			clearCookie(w, SessionCookieName, "/")
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		now := a.now()
		if now.Sub(se.LastSeenAt) > IdleTimeout || now.Sub(se.CreatedAt) > MaxSessionAge {
			if err := a.store.DeleteSession(ctx, hash); err != nil {
				a.log.Error("dashboard: deleting an expired session failed", "error", err.Error())
			}
			a.forget(hash)
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
		if !found {
			if err := a.store.DeleteSession(ctx, hash); err != nil {
				a.log.Error("dashboard: deleting the session of a removed account failed", "error", err.Error())
			}
			a.forget(hash)
			clearCookie(w, SessionCookieName, "/")
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
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
		s := Session{
			AccountID: account.ID, Kind: account.Kind, MustChangePassword: account.MustChangePassword,
			CSRFToken: se.CSRF, idHash: se.IDHash,
		}
		if account.Kind == store.AccountPassword {
			// No platform and no token to check with. Until teams decide
			// what each account sees, a password account is an admin.
			s.Username, s.IsAdmin, s.Visible = account.Username, true, map[string]int64{}
		} else {
			acc, err := a.access(ctx, se, now)
			if err != nil {
				a.log.Error("dashboard: loading session access failed", "error", err.Error())
				writeError(w, http.StatusInternalServerError, "could not load the session")
				return
			}
			visible := make(map[string]int64, len(acc.projects))
			for _, p := range acc.projects {
				visible[lower(p.Path)] = p.ID
			}
			s.Platform, s.Instance, s.UserID, s.Username = se.Platform, se.Instance, se.UserID, acc.username
			s.IsAdmin, s.Visible = acc.isAdmin, visible
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, sessionKey{}, s)))
	})
}

// PruneSessions deletes the sessions older than 7 days, and those idle for
// more than 8 hours, which can no longer be used. It returns how many.
func (a *Auth) PruneSessions(ctx context.Context, now time.Time) (int, error) {
	n, err := a.store.PruneSessions(ctx, now.Add(-MaxSessionAge), now.Add(-IdleTimeout))
	if err != nil {
		return 0, fmt.Errorf("prune dashboard sessions: %w", err)
	}
	return n, nil
}

// allowedBeforeChange reports whether r is one of the requests a password
// account that must change its password may make: who am I, sign out and
// the change itself.
func allowedBeforeChange(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/me", "POST /auth/logout", "POST /auth/password/change":
		return true
	}
	return false
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

func (a *Auth) seal(tok oauthToken) ([]byte, error) {
	data, err := json.Marshal(tok)
	if err != nil {
		return nil, errors.New("dashboard: encoding the token")
	}
	return a.box.Seal(data), nil
}

func (a *Auth) open(sealed []byte) (oauthToken, error) {
	data, err := a.box.Open(sealed)
	if err != nil {
		return oauthToken{}, errors.New("dashboard: the stored token can't be opened (PERGENT_SH_SECRET_KEY changed?)")
	}
	var tok oauthToken
	if json.Unmarshal(data, &tok) != nil || tok.AccessToken == "" {
		return oauthToken{}, errors.New("dashboard: the stored token is malformed")
	}
	return tok, nil
}
