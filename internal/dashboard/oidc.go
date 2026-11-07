package dashboard

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// oidcStateCookie carries the state, nonce and PKCE verifier from the
	// login to the callback, sealed with PERGENT_SH_SECRET_KEY. The __Host-
	// prefix makes browsers take it only Secure, on Path=/ and with no
	// Domain.
	oidcStateCookie = "__Host-pergent_oidc"
	oidcStateTTL    = 10 * time.Minute
	// oidcTimeout bounds everything one callback asks of the provider: the
	// discovery, the code exchange and the keys.
	oidcTimeout = 20 * time.Second
	// providerTTL is how long a provider's discovery document is kept.
	providerTTL = 10 * time.Minute
	// idpTimeout bounds one request to an identity provider.
	idpTimeout = 10 * time.Second
	// maxIDToken bounds an ID token pergent.sh will parse.
	maxIDToken = 16 << 10

	// The codes the sign-in page turns into a message (`/signin?error=`).
	signInErrExpired    = "expired"
	signInErrFailed     = "failed"
	signInErrDenied     = "denied"
	signInErrNotAllowed = "not_allowed"
	signInErrNotSetUp   = "not_set_up"
	signInErrUnavail    = "unavailable"
)

// asymmetricAlgs are the only signature algorithms a token may use. A
// symmetric one (HS256) would let a token be signed with the client secret,
// and "none" is no signature: neither is ever accepted.
var asymmetricAlgs = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "EdDSA"}

var (
	errNoIDToken  = errors.New("the token endpoint answered without an ID token")
	errNonce      = errors.New("the ID token's nonce is not the one sent")
	errNoSubject  = errors.New("the ID token has no subject")
	errNoEmail    = errors.New("the token has no email")
	errUnverified = errors.New("the token's email is not verified")
)

// tokenVerifier checks a signed token's signature against a provider's keys,
// and its issuer, audience and expiry. OIDC calls it on the ID token it got
// from the token endpoint; the JWT sign-in calls it on a header, with a JWKS
// URL. It never skips the issuer or the audience, and accepts asymmetric
// algorithms only.
type tokenVerifier struct{ v *oidc.IDTokenVerifier }

// newTokenVerifier verifies tokens issued by issuer for audience, with the
// keys at jwksURL (fetched with hc, cached, fetched again for a key it has
// not seen). An empty audience refuses every token.
func newTokenVerifier(hc *http.Client, issuer, audience, jwksURL string, now func() time.Time) *tokenVerifier {
	// The key set outlives the request that made it: it gets its own context,
	// which only carries the client.
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), hc), jwksURL)
	return &tokenVerifier{v: oidc.NewVerifier(issuer, keys, &oidc.Config{
		ClientID: audience, SupportedSigningAlgs: asymmetricAlgs, Now: now,
	})}
}

// tokenClaims is a verified token.
type tokenClaims struct {
	Subject string
	Nonce   string
	Expiry  time.Time
	// Claims is every claim, as encoding/json reads them (numbers are
	// float64).
	Claims map[string]any
}

// Verify checks raw and returns its claims; any failure is an error and
// gives nothing.
func (t *tokenVerifier) Verify(ctx context.Context, raw string) (*tokenClaims, error) {
	if raw == "" || len(raw) > maxIDToken {
		return nil, errors.New("no token, or one too large")
	}
	tok, err := t.v.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("reading the token's claims: %w", err)
	}
	return &tokenClaims{Subject: tok.Subject, Nonce: tok.Nonce, Expiry: tok.Expiry, Claims: claims}, nil
}

// emailFrom reads the email out of a verified token's claims: the claim
// named claim must be one email address (store.NormalizeEmail), and
// email_verified must be true. A token without email_verified is refused
// when requireVerified is set (OIDC), and accepted otherwise (a JWT from a
// proxy that does not carry it); one that says anything but true is always
// refused.
func emailFrom(claims map[string]any, claim string, requireVerified bool) (string, error) {
	raw, _ := claims[claim].(string)
	if raw == "" {
		return "", errNoEmail
	}
	email, err := store.NormalizeEmail(raw)
	if err != nil {
		return "", err
	}
	v, present := claims["email_verified"]
	if !present {
		if requireVerified {
			return "", errUnverified
		}
		return email, nil
	}
	// Some providers write the boolean as a string.
	if b, ok := v.(bool); !(ok && b) && v != "true" {
		return "", errUnverified
	}
	return email, nil
}

// idpClient is a copy of base for talking to an identity provider: it
// follows no redirect (a token request carries the client secret, and a
// discovery document or key set never redirects) and gives up after
// idpTimeout when base has no timeout of its own.
func idpClient(base *http.Client) *http.Client {
	c := *base
	if c.Timeout == 0 {
		c.Timeout = idpTimeout
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// oidcProvider is what discovery found out about the provider, and the
// verifier of its ID tokens.
type oidcProvider struct {
	endpoint oauth2.Endpoint
	verifier *tokenVerifier
	at       time.Time
}

// oidcCache keeps the last provider, for its issuer and client ID, for
// providerTTL.
type oidcCache struct {
	mu  sync.Mutex
	key string
	p   *oidcProvider
}

// httpsURL reports whether raw is an absolute https URL.
func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// provider reads the provider's discovery document (kept for 10 minutes) and
// builds the ID token verifier. The authorization endpoint, the token endpoint
// and the key set must be https, or the client secret and the code would
// travel in the clear.
func (d *Dashboard) provider(ctx context.Context, s signIn) (*oidcProvider, error) {
	key := s.issuer + "\x00" + s.clientID
	d.oidc.mu.Lock()
	if p := d.oidc.p; p != nil && d.oidc.key == key && d.now().Sub(p.at) < providerTTL {
		d.oidc.mu.Unlock()
		return p, nil
	}
	d.oidc.mu.Unlock()

	prov, err := oidc.NewProvider(oidc.ClientContext(ctx, d.idp), s.issuer)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := prov.Claims(&doc); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	ep := prov.Endpoint()
	if !httpsURL(ep.AuthURL) || !httpsURL(ep.TokenURL) || !httpsURL(doc.JWKSURI) {
		return nil, errors.New("discovery: the authorization endpoint, token endpoint and jwks_uri must be https")
	}
	p := &oidcProvider{endpoint: ep, at: d.now(),
		verifier: newTokenVerifier(d.idp, s.issuer, s.clientID, doc.JWKSURI, d.now)}
	d.oidc.mu.Lock()
	d.oidc.key, d.oidc.p = key, p
	d.oidc.mu.Unlock()
	return p, nil
}

// forgetProvider drops the cached provider (the settings changed).
func (d *Dashboard) forgetProvider() {
	d.oidc.mu.Lock()
	d.oidc.key, d.oidc.p = "", nil
	d.oidc.mu.Unlock()
}

// oauthConfig is the OAuth client for s at provider p. The redirect URI is
// dashboard.url, fixed, never taken from the request.
func (d *Dashboard) oauthConfig(s signIn, p *oidcProvider) *oauth2.Config {
	return &oauth2.Config{
		ClientID: s.clientID, ClientSecret: s.clientSecret, Endpoint: p.endpoint,
		RedirectURL: d.callbackURL(), Scopes: []string{oidc.ScopeOpenID, "email"},
	}
}

// callbackURL is where the provider sends the browser back: dashboard.url
// and the callback's path.
func (d *Dashboard) callbackURL() string {
	return strings.TrimRight(d.cfg.Dashboard.URL, "/") + "/auth/oidc/callback"
}

// oidcState is what the state cookie carries between login and callback.
type oidcState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Expires  int64  `json:"e"`
}

func (a *Auth) setOIDCState(w http.ResponseWriter, st oidcState) {
	data, _ := json.Marshal(st)
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: base64.RawURLEncoding.EncodeToString(a.box.Seal(data)), Path: "/",
		MaxAge: int(oidcStateTTL / time.Second), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

// readOIDCState returns the state cookie's content if it opens (it is
// sealed, so a changed or forged one does not), is complete and has not
// expired.
func (a *Auth) readOIDCState(r *http.Request) (oidcState, bool) {
	c, err := r.Cookie(oidcStateCookie)
	if err != nil {
		return oidcState{}, false
	}
	sealed, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return oidcState{}, false
	}
	data, err := a.box.Open(sealed)
	if err != nil {
		return oidcState{}, false
	}
	var st oidcState
	if json.Unmarshal(data, &st) != nil || st.State == "" || st.Nonce == "" || st.Verifier == "" || a.now().Unix() > st.Expires {
		return oidcState{}, false
	}
	return st, true
}

// signInPage sends the browser to the sign-in page with a code it shows as a
// message. The code is one of ours: nothing from the provider is passed on.
func signInPage(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/signin?error="+url.QueryEscape(code), http.StatusSeeOther)
}

// oidcLogin sends the browser to the provider. Nothing from the request but
// the route chooses where: no "next" parameter, no Host header.
func (d *Dashboard) oidcLogin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), oidcTimeout)
	defer cancel()
	s, err := d.signInSettings(ctx)
	if err != nil {
		d.Log.Error("dashboard OIDC sign-in: reading the settings failed", "error", err.Error())
		signInPage(w, r, signInErrFailed)
		return
	}
	if !s.oidcReady() {
		signInPage(w, r, signInErrNotSetUp)
		return
	}
	p, err := d.provider(ctx, s)
	if err != nil {
		d.Log.Warn("dashboard OIDC sign-in: the provider could not be read", "issuer", s.issuer, "error", safeText(err))
		signInPage(w, r, signInErrUnavail)
		return
	}
	st := oidcState{State: randomString(32), Nonce: randomString(32), Verifier: oauth2.GenerateVerifier(),
		Expires: d.now().Add(oidcStateTTL).Unix()}
	d.Auth.setOIDCState(w, st)
	http.Redirect(w, r, d.oauthConfig(s, p).AuthCodeURL(st.State, oauth2.S256ChallengeOption(st.Verifier), oidc.Nonce(st.Nonce)),
		http.StatusFound)
}

// exchange trades the callback's code for the ID token and verifies it: the
// signature, issuer, audience and expiry (the verifier), then the nonce.
func (d *Dashboard) exchange(ctx context.Context, s signIn, p *oidcProvider, code string, st oidcState) (*tokenClaims, error) {
	tok, err := d.oauthConfig(s, p).Exchange(oidc.ClientContext(ctx, d.idp), code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return nil, err
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, errNoIDToken
	}
	claims, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(st.Nonce)) != 1 {
		return nil, errNonce
	}
	if claims.Subject == "" {
		return nil, errNoSubject
	}
	return claims, nil
}

// oidcIdentity is the first half of the callback: it proves who the browser
// is, with no database write. In order: the state (constant-time), the code
// exchange, the ID token's checks (signature, issuer, audience, expiry, nonce)
// and the email. It returns the settings in force, the email (normalised) and
// the token's claims, or the code of the sign-in page's message.
func (d *Dashboard) oidcIdentity(ctx context.Context, r *http.Request) (s signIn, email string, claims map[string]any, code string) {
	q := r.URL.Query()
	st, ok := d.Auth.readOIDCState(r)
	if !ok || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		return signIn{}, "", nil, signInErrExpired
	}
	if q.Get("error") != "" {
		return signIn{}, "", nil, signInErrDenied
	}
	authCode := q.Get("code")
	if authCode == "" {
		return signIn{}, "", nil, signInErrExpired
	}
	s, err := d.signInSettings(ctx)
	if err != nil {
		d.Log.Error("dashboard OIDC sign-in: reading the settings failed", "error", err.Error())
		return signIn{}, "", nil, signInErrFailed
	}
	if !s.oidcReady() {
		return signIn{}, "", nil, signInErrNotSetUp
	}
	p, err := d.provider(ctx, s)
	if err != nil {
		d.Log.Warn("dashboard OIDC sign-in: the provider could not be read", "issuer", s.issuer, "error", safeText(err))
		return signIn{}, "", nil, signInErrUnavail
	}
	tok, err := d.exchange(ctx, s, p, authCode, st)
	if err != nil {
		d.Log.Warn("dashboard OIDC sign-in: the token was refused", "error", safeText(err))
		return signIn{}, "", nil, signInErrFailed
	}
	email, err = emailFrom(tok.Claims, "email", true)
	if err != nil {
		d.Log.Warn("dashboard OIDC sign-in: the token's email was refused", "error", err.Error())
		return signIn{}, "", nil, signInErrFailed
	}
	return s, email, tok.Claims, ""
}

// oidcCallback finishes sign-in: oidcIdentity proves who the browser is, then
// the allowlist decides and only then is an account made (or found), the
// browser's earlier session ended and a new one made (finishSignIn). Every
// refusal sends the browser to the sign-in page with a code, and creates no
// account and no session.
func (d *Dashboard) oidcCallback(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, oidcStateCookie, "/")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), oidcTimeout)
	defer cancel()
	s, email, claims, code := d.oidcIdentity(ctx, r)
	if code != "" {
		signInPage(w, r, code)
		return
	}
	d.finishSignIn(ctx, w, r, s, email, claims, store.SignInOIDC)
}

// finishSignIn is the end of every SSO sign-in once the identity is proved:
// the allowlist decides, then the account, then the session. signIn is how
// the session began (store.SignInOIDC). It answers the browser itself.
func (d *Dashboard) finishSignIn(ctx context.Context, w http.ResponseWriter, r *http.Request, s signIn, email string, claims map[string]any, how string) {
	a := d.Auth
	existing, found, err := a.store.SSOAccountByEmail(ctx, email)
	if err != nil {
		d.Log.Error("dashboard sign-in: reading the account failed", "error", err.Error())
		signInPage(w, r, signInErrFailed)
		return
	}
	if !s.allowed(email, claims, found && existing.Added) {
		// No account, no session, and no audit entry: anyone with an account
		// at the provider can reach this, and the log is not theirs to fill.
		d.Log.Info("dashboard sign-in refused: not on the allowlist", "domain", emailDomain(email))
		signInPage(w, r, signInErrNotAllowed)
		return
	}
	now := a.now()
	acc, err := a.store.SignInSSOAccount(ctx, email, now)
	if err != nil {
		d.Log.Error("dashboard sign-in: storing the account failed", "error", err.Error())
		signInPage(w, r, signInErrFailed)
		return
	}
	// Never adopt the session the browser arrived with: end it.
	if hash, ok := sessionHash(r); ok {
		if err := a.store.DeleteSession(ctx, hash); err != nil {
			d.Log.Error("dashboard sign-in: ending the previous session failed", "error", err.Error())
			signInPage(w, r, signInErrFailed)
			return
		}
	}
	id := randomString(sessionIDBytes)
	if err := a.store.CreateSession(ctx, store.Session{
		IDHash: hashID(id), AccountID: acc.ID, Username: acc.Username, CSRF: randomString(32),
		SignIn: how, Issuer: s.issuer, LastSeenAt: now, CreatedAt: now,
	}); err != nil {
		d.Log.Error("dashboard sign-in: storing the session failed", "error", err.Error())
		signInPage(w, r, signInErrFailed)
		return
	}
	a.audit(ctx, acc, "sign_in", how, now)
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: id, Path: "/", MaxAge: int(SSOMaxSessionAge / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	d.Log.Info("dashboard sign-in", "how", how)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// emailDomain is the part of email after its last "@".
func emailDomain(email string) string {
	return email[strings.LastIndex(email, "@")+1:]
}

var oauthErrorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// safeText is err as a short line for the log. An OAuth error from the token
// endpoint keeps its status and its error code only if it looks like one, not
// the body, and any other error is cut at 300 characters with its line breaks
// removed (it may quote a response from the provider). A token or a client
// secret is in none of these: the library never puts one in an error.
func safeText(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		code := "unknown"
		if oauthErrorCode.MatchString(re.ErrorCode) {
			code = re.ErrorCode
		}
		status := 0
		if re.Response != nil {
			status = re.Response.StatusCode
		}
		return fmt.Sprintf("token endpoint answered %d, error %s", status, code)
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	return text
}
