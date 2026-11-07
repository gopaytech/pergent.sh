package dashboard

// JWT validation: sign-in for an install behind something that has already
// logged the person in (an auth proxy, Istio, Cloudflare Access). The proxy
// puts a signed token in a header on every request. pergent.sh checks it and
// keeps the proxy in charge:
//
//   - Only a GET of a page or of /api/me, with a valid token and no session,
//     starts a session. A write never does: the proxy adds the header to any
//     request the browser sends, so a cross-site POST must start nothing.
//   - A session that jwt started needs, on every request, a valid token for
//     the same email. Otherwise it ends. A key set that cannot be fetched
//     ends nothing: the request gets 503.
//   - A local admin's session has no token and skips all of it.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// headerStatus is what the proxy's header says about a request.
type headerStatus int

const (
	// headerNone: no header, or the sign-in mode is not jwt. The header means
	// nothing.
	headerNone headerStatus = iota
	// headerValid: a token that passed every check, with an email that parses.
	headerValid
	// headerInvalid: a token is there and is not good: bad signature, wrong
	// issuer or audience, expired, a refused algorithm, no usable email, or
	// the sign-in is not set up to check one.
	headerInvalid
	// headerUnavailable: it could not be decided because the signing keys
	// could not be fetched. This says nothing about the token.
	headerUnavailable
	// headerNoSettings: the sign-in settings could not be read, so whether a
	// header means anything is not known. A session that jwt started is kept
	// (503); a request with no session starts none, and gets what it would get
	// on an install that does not use a header (401), so a database blip does
	// not turn the sign-in page into an error.
	headerNoSettings
)

const (
	msgSignInUnavailable = "sign-in is unavailable: the signing keys could not be read; try again in a moment"
	msgNotAllowed        = "ask an admin to add you"
	// signedOutPath is the page that shows a person has signed out. It starts
	// nothing, or signing out would sign the person straight in again.
	signedOutPath = "/signed-out"
	// adminSignInPath is the local admins' sign-in page. It starts nothing
	// either: a local admin whose proxy identity is allowed would otherwise be
	// signed in as that person before the password form could show.
	adminSignInPath = "/admin/sign-in"
	// probeParam on GET /api/me asks who is signed in without starting a
	// session from the header (the admin sign-in page's quiet read).
	probeParam = "probe"
)

// jwtVerifier is the one verifier of the tokens, kept while the issuer, the
// audience, the key set's URL and the client stay the same (its keys are
// cached inside).
type jwtVerifier struct {
	mu  sync.Mutex
	key string
	hc  *http.Client
	v   *tokenVerifier
}

func (a *Auth) jwtVerifierFor(s signIn) *tokenVerifier {
	hc := a.idpClient()
	key := s.issuer + "\x00" + s.audience + "\x00" + s.jwksURL
	a.jwt.mu.Lock()
	defer a.jwt.mu.Unlock()
	if a.jwt.v == nil || a.jwt.key != key || a.jwt.hc != hc {
		a.jwt.key, a.jwt.hc = key, hc
		a.jwt.v = newTokenVerifier(hc, s.issuer, s.audience, s.jwksURL, a.now)
	}
	return a.jwt.v
}

// headerToken is the token in a header value: "Bearer <token>" (the word in
// any case) or the bare token.
func headerToken(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > len("Bearer ") && strings.EqualFold(v[:len("Bearer ")], "Bearer ") {
		v = strings.TrimSpace(v[len("Bearer "):])
	}
	return v
}

// rawHeader finds the token in the request without checking it. found is
// true, with the token in raw, when there is one to check; otherwise st says
// why not: headerNone (no header, or the mode is not jwt), headerInvalid (the
// header is there twice, so the browser could have sent one of them) or
// headerNoSettings (the settings could not be read).
func (a *Auth) rawHeader(r *http.Request) (s signIn, raw string, found bool, st headerStatus) {
	if a.settings == nil {
		return signIn{}, "", false, headerNone
	}
	s, err := a.settings(r.Context())
	if err != nil {
		a.log.Error("dashboard JWT sign-in: reading the settings failed", "error", err.Error())
		return signIn{}, "", false, headerNoSettings
	}
	if s.mode != config.SignInJWT || s.header == "" {
		return s, "", false, headerNone
	}
	values := r.Header.Values(s.header)
	if len(values) == 0 {
		return s, "", false, headerNone
	}
	if len(values) > 1 {
		a.log.Warn("dashboard JWT sign-in: the header came more than once; refused")
		return s, "", false, headerInvalid
	}
	raw = headerToken(values[0])
	if raw == "" {
		return s, "", false, headerNone
	}
	return s, raw, true, headerNone
}

// claimsFit reads the payload of raw WITHOUT checking its signature and
// reports whether its issuer, audience and expiry are the ones wanted. It is
// only a way to turn away a token that cannot pass, before the key set is
// asked for anything (a token with an unknown key id costs a fetch of the key
// set): a token that fits is checked in full by the verifier, and nothing read
// here is trusted. A claim it cannot read counts as a misfit.
func claimsFit(raw, issuer, audience string, now time.Time) bool {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var c struct {
		Iss string          `json:"iss"`
		Aud json.RawMessage `json:"aud"`
		Exp *float64        `json:"exp"`
	}
	if json.Unmarshal(payload, &c) != nil || c.Iss != issuer || c.Exp == nil || time.Unix(int64(*c.Exp), 0).Before(now) {
		return false
	}
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		return one == audience
	}
	var many []string
	if json.Unmarshal(c.Aud, &many) != nil {
		return false
	}
	for _, a := range many {
		if a == audience {
			return true
		}
	}
	return false
}

// checkToken verifies raw for settings s and reads the email out of it. It
// returns the email (normalised), the token's claims and headerValid, or
// headerInvalid or headerUnavailable. Why a token is refused is logged without
// the token.
func (a *Auth) checkToken(r *http.Request, s signIn, raw string) (string, map[string]any, headerStatus) {
	if s.issuer == "" || s.audience == "" || s.jwksURL == "" {
		// Without an issuer and an audience a token meant for another app
		// would pass: refuse every token until they are set.
		a.log.Warn("dashboard JWT sign-in: the issuer, the audience and the JWKS URL must all be set; every token is refused")
		return "", nil, headerInvalid
	}
	if !claimsFit(raw, s.issuer, s.audience, a.now()) {
		a.log.Debug("dashboard JWT sign-in: the token is for another issuer or audience, or has expired")
		return "", nil, headerInvalid
	}
	ctx, cancel := context.WithTimeout(r.Context(), idpTimeout)
	defer cancel()
	claims, err := a.jwtVerifierFor(s).Verify(ctx, raw)
	if err != nil {
		if errors.Is(err, errKeysUnavailable) {
			a.log.Warn("dashboard JWT sign-in: the signing keys could not be fetched", "jwks", s.jwksURL)
			return "", nil, headerUnavailable
		}
		a.log.Debug("dashboard JWT sign-in: the token was refused", "error", safeText(err))
		return "", nil, headerInvalid
	}
	// email_verified is checked when present; a proxy's token may not carry it.
	email, err := emailFrom(claims.Claims, s.emailClaim, false)
	if err != nil {
		a.log.Debug("dashboard JWT sign-in: the token's email was refused", "error", err.Error())
		return "", nil, headerInvalid
	}
	return email, claims.Claims, headerValid
}

// readHeader is fromHeader with the settings and the token's claims too.
func (a *Auth) readHeader(r *http.Request) (s signIn, email string, claims map[string]any, st headerStatus) {
	s, raw, found, st := a.rawHeader(r)
	if !found {
		return s, "", nil, st
	}
	email, claims, st = a.checkToken(r, s, raw)
	return s, email, claims, st
}

// fromHeader checks the proxy's header on r: the signature, issuer, audience
// and expiry of the token, with asymmetric algorithms only, then that the
// email claim is one email address (and email_verified is not false). It
// returns the email, lower-cased, with headerValid; headerNone when there is
// no header or the mode is not jwt; headerInvalid for a bad token; and
// headerUnavailable when the signing keys cannot be fetched and none is
// cached. It never logs the header.
func (a *Auth) fromHeader(r *http.Request) (email string, status headerStatus) {
	_, email, _, status = a.readHeader(r)
	return email, status
}

// reservedPrefixes are the paths other routes of the server own, as
// web.Handler lists them and the Job callbacks: never pages.
var reservedPrefixes = []string{"/api", "/auth", "/webhooks", "/runs", "/healthz", "/skill-fetches", "/readiness-scans"}

// isPagePath reports whether p is a page of the web app: the shell the
// browser's router takes over. It is not a path another route owns, a hashed
// asset, a file at the root (favicon.svg), or /signed-out and /admin/sign-in,
// which start nothing (whatever the case: the router ignores it).
func isPagePath(p string) bool {
	p = path.Clean("/" + p)
	for _, prefix := range reservedPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return false
		}
	}
	if strings.EqualFold(p, signedOutPath) || strings.EqualFold(p, adminSignInPath) || strings.HasPrefix(p, "/assets/") {
		return false
	}
	if strings.Count(p, "/") == 1 && path.Ext(p) != "" {
		return false
	}
	return true
}

// crossSiteSubrequest reports whether r came from another site and is not a
// navigation, as the browser's Sec-Fetch headers say. (A client that sends none
// is not a browser, or is an old one: not refused.)
func crossSiteSubrequest(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") == "cross-site" && r.Header.Get("Sec-Fetch-Mode") != "navigate"
}

// startOutcome is what trying to start a session from the header came to.
type startOutcome int

const (
	// startedSession: a session is stored; the caller gives the cookie.
	startedSession startOutcome = iota
	// startNothing: no header, the mode is not jwt, or the token is bad.
	startNothing
	// startRefused: the token is good and the email is not allowed.
	startRefused
	startUnavailable
	startFailed
)

// startFromHeader starts a session for the person the header proves, for a
// request with no session. The caller has already decided that this request
// may start one (a GET of a page or /api/me). On startedSession the cookie is
// set on w and the session is returned.
func (a *Auth) startFromHeader(w http.ResponseWriter, r *http.Request) (store.Session, startOutcome) {
	// A page another site loads as an image, a script or a fetch carries the
	// proxy's header too: only a navigation may start a session.
	if crossSiteSubrequest(r) {
		return store.Session{}, startNothing
	}
	s, email, claims, st := a.readHeader(r)
	switch st {
	case headerUnavailable:
		return store.Session{}, startUnavailable
	case headerValid:
	default:
		return store.Session{}, startNothing
	}
	se, id, _, code := a.beginSession(r.Context(), r, s, email, claims, store.SignInJWT)
	switch code {
	case "":
		a.setSessionCookie(w, id)
		return se, startedSession
	case signInErrNotAllowed:
		return store.Session{}, startRefused
	default:
		return store.Session{}, startFailed
	}
}

// startOnAPI is Require's way for a request with no session: only GET
// /api/me (without ?probe=1) can start one. It returns the new session and true, or has
// answered the request itself and returns false. staleCookie is set when the
// request carried a cookie for a session that is gone.
func (a *Auth) startOnAPI(w http.ResponseWriter, r *http.Request, staleCookie bool) (store.Session, bool) {
	var (
		se      store.Session
		outcome = startNothing
	)
	if r.Method == http.MethodGet && r.URL.Path == "/api/me" && r.URL.Query().Get(probeParam) != "1" {
		se, outcome = a.startFromHeader(w, r)
	}
	switch outcome {
	case startedSession:
		return se, true
	case startUnavailable:
		writeError(w, http.StatusServiceUnavailable, msgSignInUnavailable)
	case startFailed:
		writeError(w, http.StatusInternalServerError, "could not sign in")
	case startRefused:
		if staleCookie {
			clearCookie(w, SessionCookieName, "/")
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": msgNotAllowed, "code": signInErrNotAllowed})
	default:
		if staleCookie {
			clearCookie(w, SessionCookieName, "/")
		}
		writeError(w, http.StatusUnauthorized, "not signed in")
	}
	return store.Session{}, false
}

// jwtSessionHolds reports, for a session that jwt started, whether the request
// still proves it: the mode is still jwt, the issuer is the one that vouched
// for the session, and the header carries a valid token for the session's
// email. It returns false with headerUnavailable when it cannot tell (the
// caller answers 503 and ends nothing); in every other false case the session
// must end.
func (a *Auth) jwtSessionHolds(r *http.Request, se store.Session, accountEmail string) (bool, headerStatus) {
	s, email, _, st := a.readHeader(r)
	switch {
	case st == headerUnavailable || st == headerNoSettings:
		return false, headerUnavailable
	case s.mode != config.SignInJWT || s.issuer != se.Issuer:
		return false, st
	case st != headerValid || accountEmail == "" || email != accountEmail:
		return false, st
	}
	return true, st
}

// Pages wraps the handler of the web app's pages: a GET of a page by someone
// with no session and a valid token starts a session, and the page is served
// either way. A bad token, a refusal or an outage only means the page comes
// with no session (the app asks /api/me, which says why).
func (d *Dashboard) Pages(next http.Handler) http.Handler { return d.Auth.pages(next) }

func (a *Auth) pages(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && isPagePath(r.URL.Path) && a.startOnPage(w, r) {
			// The response sets the session cookie: no cache may keep it.
			w.Header().Set("Cache-Control", "private, no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// startOnPage starts a session for the page request if the header proves a
// person and the request has no live session. It reports whether it did.
func (a *Auth) startOnPage(w http.ResponseWriter, r *http.Request) bool {
	// The cheap questions first: is jwt on, and is there a token at all?
	if _, _, found, _ := a.rawHeader(r); !found {
		return false
	}
	if hash, ok := sessionHash(r); ok {
		se, found, err := a.store.Session(r.Context(), hash)
		if err != nil {
			a.log.Error("dashboard: loading a session failed", "error", err.Error())
			return false
		}
		// An expired row is no session: Require would delete it on its next
		// request, and the person must not be turned away meanwhile.
		if found && !sessionExpired(se, a.now()) {
			return false
		}
	}
	_, outcome := a.startFromHeader(w, r)
	return outcome == startedSession
}
