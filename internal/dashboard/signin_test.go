package dashboard

// The tests here that do not touch the database run everywhere: the
// allowlist, the token verifier, and the whole OIDC callback up to the point
// where an account would be made (a refusal never gets there). The tests that
// make accounts and sessions need PERGENT_SH_TEST_DATABASE_URL and are marked
// [CI].

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	testClientID     = "pergent-client"
	testClientSecret = "client-secret-SECRET-value"
	testSecretEnv    = "PERGENT_TEST_OIDC_SECRET"
)

// --- a fake identity provider ---

// fakeIssuer is an OIDC provider with its own RSA key: discovery, a key set
// and a token endpoint over TLS. A test "approves" an authorization request
// by handing its parameters to approve, which returns the code the token
// endpoint will accept once, with the PKCE verifier that matches the
// challenge.
type fakeIssuer struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]issuedCode
	// claims edits the default ID token's claims; alg is how it is signed:
	// "RS256" (default), "HS256" (with hmacKey) or "none". signWith signs
	// with another RSA key.
	claims   func(c map[string]any)
	alg      string
	hmacKey  []byte
	signWith *rsa.PrivateKey
	// httpTokenEndpoint advertises an http:// token endpoint.
	httpTokenEndpoint bool
	// tokenForms are the forms the token endpoint received.
	tokenForms []url.Values
	// failToken makes the token endpoint answer 400.
	failToken bool
	// now is the clock the tokens are made by; nil is time.Now.
	now func() time.Time
}

type issuedCode struct{ nonce, challenge, redirect string }

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{t: t, key: key, codes: map[string]issuedCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /jwks", f.jwks)
	mux.HandleFunc("POST /token", f.token)
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) url() string { return f.srv.URL }

func (f *fakeIssuer) discovery(w http.ResponseWriter, _ *http.Request) {
	tokenURL := f.srv.URL + "/token"
	if f.httpTokenEndpoint {
		tokenURL = "http://idp.example.com/token"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": tokenURL,
		"jwks_uri": f.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (f *fakeIssuer) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := f.key.PublicKey
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "k1",
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (f *fakeIssuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.tokenForms = append(f.tokenForms, r.PostForm)
	failing := f.failToken
	issued, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code")) // a code works once
	f.mu.Unlock()

	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	verifier := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case failing, !ok, r.PostForm.Get("grant_type") != "authorization_code",
		id != testClientID || secret != testClientSecret,
		b64(verifier[:]) != issued.challenge, r.PostForm.Get("redirect_uri") != issued.redirect:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"BODY-FROM-IDP-MUST-NOT-LEAK"}`))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": "access-token-SECRET", "token_type": "Bearer", "expires_in": 3600,
		"id_token": f.idToken(issued.nonce),
	})
}

// goodClaims are the claims of a valid ID token for nonce.
func (f *fakeIssuer) goodClaims(nonce string) map[string]any {
	now := time.Now()
	if f.now != nil {
		now = f.now()
	}
	return map[string]any{
		"iss": f.srv.URL, "aud": testClientID, "sub": "subject-1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": nonce, "email": "alice@example.com", "email_verified": true,
	}
}

func (f *fakeIssuer) idToken(nonce string) string {
	c := f.goodClaims(nonce)
	if f.claims != nil {
		f.claims(c)
	}
	return f.sign(c)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// sign makes a token over claims, as f.alg says.
func (f *fakeIssuer) sign(claims map[string]any) string {
	alg := f.alg
	if alg == "" {
		alg = "RS256"
	}
	header, _ := json.Marshal(map[string]any{"alg": alg, "typ": "JWT", "kid": "k1"})
	payload, _ := json.Marshal(claims)
	signing := b64(header) + "." + b64(payload)
	var sig []byte
	switch alg {
	case "RS256":
		key := f.key
		if f.signWith != nil {
			key = f.signWith
		}
		h := sha256.Sum256([]byte(signing))
		var err error
		if sig, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:]); err != nil {
			f.t.Fatal(err)
		}
	case "HS256":
		mac := hmac.New(sha256.New, f.hmacKey)
		mac.Write([]byte(signing))
		sig = mac.Sum(nil)
	case "none":
	default:
		f.t.Fatalf("fakeIssuer: no way to sign with %s", alg)
	}
	return signing + "." + b64(sig)
}

// approve plays the provider's authorization endpoint for the request at
// location: it checks the request and returns the code to hand to the
// callback, and the state.
func (f *fakeIssuer) approve(location string) (code, state string) {
	f.t.Helper()
	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, f.srv.URL+"/authorize?") {
		f.t.Fatalf("login redirected to %q, want the provider's authorization endpoint", location)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != testClientID || q.Get("scope") != "openid email" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" ||
		q.Get("redirect_uri") != dashboardURL+"/auth/oidc/callback" {
		f.t.Fatalf("authorization request = %v", q)
	}
	code = "code-" + randomString(8)
	f.mu.Lock()
	f.codes[code] = issuedCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	f.mu.Unlock()
	return code, q.Get("state")
}

// --- a dashboard with OIDC set in the config file, and no database ---

type oidcHarness struct {
	t   *testing.T
	f   *fakeIssuer
	d   *Dashboard
	h   http.Handler
	log *lockedBuffer
}

// newOIDCHarness is a dashboard whose config file sets OIDC against a fake
// provider. It has no store: everything a refusal does runs, and an account
// is never made. tweak may change the sign-in config.
func newOIDCHarness(t *testing.T, tweak func(s *config.SignIn)) *oidcHarness {
	t.Helper()
	f := newFakeIssuer(t)
	t.Setenv(testSecretEnv, testClientSecret)
	si := config.SignIn{Mode: config.SignInOIDC, Label: "Google", Issuer: f.url(), ClientID: testClientID,
		ClientSecretEnv: testSecretEnv, AllowedDomains: []string{"example.com"}}
	if tweak != nil {
		tweak(&si)
	}
	cfg := &config.Config{Dashboard: config.Dashboard{URL: dashboardURL, SignIn: si}}
	logs := &lockedBuffer{}
	d, err := New(Options{Config: cfg, SecretKey: testKey(t), HTTPClient: f.srv.Client(),
		Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &oidcHarness{t: t, f: f, d: d, h: d.Handler(), log: logs}
}

func (o *oidcHarness) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	o.h.ServeHTTP(rec, req)
	return rec
}

// login starts a sign-in: it returns the state cookie and the provider's
// authorization request location.
func (o *oidcHarness) login() (*http.Cookie, string) {
	o.t.Helper()
	rec := o.serve(httptest.NewRequest("GET", "/auth/oidc/login", nil))
	if rec.Code != http.StatusFound {
		o.t.Fatalf("GET /auth/oidc/login = %d %s; want 302", rec.Code, rec.Body)
	}
	return cookie(rec, oidcStateCookie), rec.Header().Get("Location")
}

// callbackRequest is the request the browser makes when the provider sends
// it back with code and state, carrying the state cookie.
func callbackRequest(code, state string, c *http.Cookie) *http.Request {
	req := httptest.NewRequest("GET", "/auth/oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	if c != nil {
		req.AddCookie(c)
	}
	return req
}

// signInPageCode is the error code of a redirect to the sign-in page, "" when
// the response is something else.
func signInPageCode(rec *httptest.ResponseRecorder) string {
	loc := rec.Header().Get("Location")
	rest, ok := strings.CutPrefix(loc, "/signin?error=")
	if rec.Code != http.StatusSeeOther || !ok {
		return ""
	}
	return rest
}

// refusedCallback runs a whole sign-in with the fake provider as edit makes
// it, and returns the callback's response, which must refuse: a redirect to
// the sign-in page with code, no session and no account (this harness has no
// store to make one in, so one would panic).
func (o *oidcHarness) refusedCallback(edit func(f *fakeIssuer), want string) {
	o.t.Helper()
	o.f.claims, o.f.alg, o.f.signWith, o.f.hmacKey = nil, "", nil, nil
	if edit != nil {
		edit(o.f)
	}
	c, loc := o.login()
	code, state := o.f.approve(loc)
	rec := o.serve(callbackRequest(code, state, c))
	if got := signInPageCode(rec); got != want {
		o.t.Fatalf("callback = %d, Location %q; want a redirect to /signin?error=%s", rec.Code, rec.Header().Get("Location"), want)
	}
	if s := cookie(rec, SessionCookieName); s != nil {
		o.t.Errorf("a refused sign-in set a session cookie: %+v", s)
	}
	if c := cookie(rec, oidcStateCookie); c == nil || c.MaxAge >= 0 {
		o.t.Errorf("the state cookie was not cleared: %+v", c)
	}
	for _, secret := range []string{testClientSecret, "access-token-SECRET", "BODY-FROM-IDP-MUST-NOT-LEAK"} {
		if strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Header().Get("Location"), secret) {
			o.t.Errorf("the refusal shows %q", secret)
		}
	}
	o.noSecretsLogged()
}

// noSecretsLogged fails if the logs hold a token, the client secret or a
// provider's response body.
func (o *oidcHarness) noSecretsLogged() {
	o.t.Helper()
	logs := o.log.String()
	for _, secret := range []string{testClientSecret, "access-token-SECRET", "BODY-FROM-IDP-MUST-NOT-LEAK", "eyJ"} {
		if strings.Contains(logs, secret) {
			o.t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}
}

// --- login ---

func TestOIDCLoginRedirect(t *testing.T) {
	o := newOIDCHarness(t, nil)
	c, loc := o.login()
	_, state := o.f.approve(loc) // checks the request's parameters
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" ||
		c.MaxAge <= 0 || c.MaxAge > int(oidcStateTTL/time.Second) || c.Domain != "" {
		t.Errorf("state cookie = %+v; want __Host-, HttpOnly, Secure, SameSite=Lax, Path=/, short-lived", c)
	}
	if c.Name != "__Host-pergent_oidc" {
		t.Errorf("state cookie name = %q", c.Name)
	}
	u, _ := url.Parse(loc)
	nonce, challenge := u.Query().Get("nonce"), u.Query().Get("code_challenge")
	// The cookie is sealed: the state, the nonce and the verifier are not in it
	// in the clear, and the secret is in neither it nor the URL.
	for _, plain := range []string{state, nonce, testClientSecret} {
		if strings.Contains(c.Value, plain) || (plain == testClientSecret && strings.Contains(loc, plain)) {
			t.Errorf("state cookie or URL holds %q", plain)
		}
	}
	// Every login has its own state, nonce and verifier.
	c2, loc2 := o.login()
	u2, _ := url.Parse(loc2)
	if u2.Query().Get("state") == state || u2.Query().Get("nonce") == nonce || u2.Query().Get("code_challenge") == challenge || c2.Value == c.Value {
		t.Error("two logins share a state, nonce, challenge or cookie")
	}
	// Nothing from the request chooses where the browser goes.
	req := httptest.NewRequest("GET", "/auth/oidc/login?next=https://evil.example.com&redirect_uri=https://evil.example.com", nil)
	req.Host = "evil.example.com"
	rec := o.serve(req)
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, o.f.url()+"/authorize?") || strings.Contains(loc, "evil") {
		t.Errorf("login with a hostile request redirected to %q", loc)
	}
	// The redirect and the discovery document are not cached by the browser.
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

func TestOIDCLoginWhenNotSetUp(t *testing.T) {
	for name, tweak := range map[string]func(s *config.SignIn){
		"no secret in the env": func(s *config.SignIn) { s.ClientSecretEnv = "PERGENT_TEST_OIDC_UNSET" },
		"jwt mode":             func(s *config.SignIn) { *s = config.SignIn{Mode: config.SignInJWT, Issuer: s.Issuer} },
	} {
		t.Run(name, func(t *testing.T) {
			o := newOIDCHarness(t, tweak)
			rec := o.serve(httptest.NewRequest("GET", "/auth/oidc/login", nil))
			if got := signInPageCode(rec); got != signInErrNotSetUp {
				t.Errorf("login = %d %q; want /signin?error=not_set_up", rec.Code, rec.Header().Get("Location"))
			}
			if cookie(rec, oidcStateCookie) != nil {
				t.Error("login that is not set up set a state cookie")
			}
		})
	}
	// No sign-in at all (and no store).
	d, err := New(Options{Config: &config.Config{Dashboard: config.Dashboard{URL: dashboardURL}}, SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/auth/oidc/login", nil))
	if got := signInPageCode(rec); got != signInErrNotSetUp {
		t.Errorf("login with no sign-in = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestOIDCProviderDown(t *testing.T) {
	o := newOIDCHarness(t, nil)
	o.f.srv.Close()
	rec := o.serve(httptest.NewRequest("GET", "/auth/oidc/login", nil))
	if got := signInPageCode(rec); got != signInErrUnavail {
		t.Errorf("login with the provider down = %d %q; want unavailable", rec.Code, rec.Header().Get("Location"))
	}
}

// A discovery document that names an http token endpoint would send the
// client secret in the clear.
func TestOIDCRefusesPlainHTTPEndpoints(t *testing.T) {
	o := newOIDCHarness(t, nil)
	o.f.httpTokenEndpoint = true
	rec := o.serve(httptest.NewRequest("GET", "/auth/oidc/login", nil))
	if got := signInPageCode(rec); got != signInErrUnavail {
		t.Errorf("login = %d %q; want unavailable", rec.Code, rec.Header().Get("Location"))
	}
}

// --- the callback ---

// A good token proves an email; nothing is stored by this half of the
// callback.
func TestOIDCIdentityOfAGoodToken(t *testing.T) {
	o := newOIDCHarness(t, nil)
	o.f.claims = func(c map[string]any) { c["email"] = "Alice@Example.COM" }
	c, loc := o.login()
	code, state := o.f.approve(loc)
	s, email, claims, errCode := o.d.oidcIdentity(ctx, callbackRequest(code, state, c))
	if errCode != "" || email != "alice@example.com" || claims["sub"] != "subject-1" || s.issuer != o.f.url() || !s.fromConfig {
		t.Fatalf("oidcIdentity = %+v, %q, %v, %q; want alice@example.com", s, email, claims, errCode)
	}
	// The token endpoint was asked once, with the client secret and the
	// verifier (the fake checks both against the login), and the exchange
	// sent no state.
	if len(o.f.tokenForms) == 0 {
		t.Fatal("the token endpoint was never asked")
	}
	if o.f.tokenForms[0].Get("code_verifier") == "" {
		t.Error("the code exchange had no PKCE verifier")
	}
	// The same code cannot be used twice.
	c2, loc2 := o.login()
	code2, state2 := o.f.approve(loc2)
	req := callbackRequest(code2, state2, c2)
	if _, _, _, errCode := o.d.oidcIdentity(ctx, req); errCode != "" {
		t.Fatalf("second sign-in: %q", errCode)
	}
	if _, _, _, errCode := o.d.oidcIdentity(ctx, req); errCode != signInErrFailed {
		t.Errorf("a code used twice = %q; want failed", errCode)
	}
	o.noSecretsLogged()
}

func TestOIDCRefusesWrongAudience(t *testing.T) {
	o := newOIDCHarness(t, nil)
	o.refusedCallback(func(f *fakeIssuer) { f.claims = func(c map[string]any) { c["aud"] = "another-app" } }, signInErrFailed)
	// A list that does not hold our client ID is no better.
	o.refusedCallback(func(f *fakeIssuer) { f.claims = func(c map[string]any) { c["aud"] = []string{"a", "b"} } }, signInErrFailed)
}

func TestOIDCRefusesExpired(t *testing.T) {
	o := newOIDCHarness(t, nil)
	o.refusedCallback(func(f *fakeIssuer) {
		f.claims = func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }
	}, signInErrFailed)
}

func TestOIDCRefusesWrongNonce(t *testing.T) {
	o := newOIDCHarness(t, nil)
	o.refusedCallback(func(f *fakeIssuer) { f.claims = func(c map[string]any) { c["nonce"] = "a-nonce-from-another-login" } }, signInErrFailed)
	o.refusedCallback(func(f *fakeIssuer) { f.claims = func(c map[string]any) { delete(c, "nonce") } }, signInErrFailed)
}

// A token signed with the client secret as an HMAC key would pass a verifier
// that took the algorithm from the token. Only asymmetric algorithms are
// accepted.
func TestOIDCRefusesHS256SignedWithSecret(t *testing.T) {
	o := newOIDCHarness(t, nil)
	o.refusedCallback(func(f *fakeIssuer) { f.alg, f.hmacKey = "HS256", []byte(testClientSecret) }, signInErrFailed)
	// The classic confusion: the provider's public key as the HMAC key.
	o.refusedCallback(func(f *fakeIssuer) {
		der, err := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		f.alg, f.hmacKey = "HS256", der
	}, signInErrFailed)
}

func TestOIDCRefusesForgedTokens(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	o := newOIDCHarness(t, nil)
	for name, edit := range map[string]func(f *fakeIssuer){
		"alg none":         func(f *fakeIssuer) { f.alg = "none" },
		"another key":      func(f *fakeIssuer) { f.signWith = other },
		"another issuer":   func(f *fakeIssuer) { f.claims = func(c map[string]any) { c["iss"] = "https://evil.example.com" } },
		"no subject":       func(f *fakeIssuer) { f.claims = func(c map[string]any) { delete(c, "sub") } },
		"no expiry":        func(f *fakeIssuer) { f.claims = func(c map[string]any) { delete(c, "exp") } },
		"not yet valid":    func(f *fakeIssuer) { f.claims = func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() } },
		"token endpoint 4": func(f *fakeIssuer) { f.failToken = true },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() { o.f.failToken = false }()
			o.refusedCallback(edit, signInErrFailed)
		})
	}
}

func TestOIDCRefusesUnverifiedEmail(t *testing.T) {
	o := newOIDCHarness(t, nil)
	for name, edit := range map[string]func(c map[string]any){
		"false":         func(c map[string]any) { c["email_verified"] = false },
		"the string no": func(c map[string]any) { c["email_verified"] = "false" },
		"absent":        func(c map[string]any) { delete(c, "email_verified") },
		"null":          func(c map[string]any) { c["email_verified"] = nil },
		"a number":      func(c map[string]any) { c["email_verified"] = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			o.refusedCallback(func(f *fakeIssuer) { f.claims = edit }, signInErrFailed)
		})
	}
	// A provider that writes the boolean as a string is read.
	o.f.claims = func(c map[string]any) { c["email_verified"] = "true" }
	c, loc := o.login()
	code, state := o.f.approve(loc)
	if _, email, _, errCode := o.d.oidcIdentity(ctx, callbackRequest(code, state, c)); errCode != "" || email == "" {
		t.Errorf(`email_verified "true" = %q, %q; want accepted`, email, errCode)
	}
}

func TestOIDCRefusesMissingEmail(t *testing.T) {
	o := newOIDCHarness(t, nil)
	for name, edit := range map[string]func(c map[string]any){
		"absent":              func(c map[string]any) { delete(c, "email") },
		"empty":               func(c map[string]any) { c["email"] = "" },
		"not a string":        func(c map[string]any) { c["email"] = 42 },
		"two addresses":       func(c map[string]any) { c["email"] = "a@example.com, b@example.com" },
		"a name and address":  func(c map[string]any) { c["email"] = "Alice <alice@example.com>" },
		"two at signs":        func(c map[string]any) { c["email"] = "alice@example.com@evil.com" },
		"no at sign":          func(c map[string]any) { c["email"] = "alice" },
		"a space":             func(c map[string]any) { c["email"] = "alice @example.com" },
		"a zero-width joiner": func(c map[string]any) { c["email"] = "ali\u200dce@example.com" },
	} {
		t.Run(name, func(t *testing.T) {
			o.refusedCallback(func(f *fakeIssuer) { f.claims = edit }, signInErrFailed)
		})
	}
}

func TestOIDCRefusesBadState(t *testing.T) {
	o := newOIDCHarness(t, nil)
	c, loc := o.login()
	code, state := o.f.approve(loc)
	expect := func(what string, req *http.Request, want string) {
		t.Helper()
		rec := o.serve(req)
		if got := signInPageCode(rec); got != want {
			t.Errorf("%s: callback = %d %q; want /signin?error=%s", what, rec.Code, rec.Header().Get("Location"), want)
		}
		if cookie(rec, SessionCookieName) != nil {
			t.Errorf("%s: set a session", what)
		}
	}
	expect("no cookie", callbackRequest(code, state, nil), signInErrExpired)
	expect("wrong state", callbackRequest(code, "another-state", c), signInErrExpired)
	expect("empty state", callbackRequest(code, "", c), signInErrExpired)
	expect("a cookie that is not sealed", callbackRequest(code, state, &http.Cookie{Name: oidcStateCookie, Value: state}), signInErrExpired)
	tampered := *c
	tampered.Value = c.Value[:len(c.Value)-3] + "AAA"
	expect("a changed cookie", callbackRequest(code, state, &tampered), signInErrExpired)
	// A cookie sealed with another key is no cookie.
	other := newOIDCHarness(t, nil)
	oc, oloc := other.login()
	_, ostate := other.f.approve(oloc)
	expect("a cookie sealed elsewhere", callbackRequest(code, ostate, oc), signInErrExpired)
	// No code, or the provider's error: nothing of it is shown.
	expect("no code", httptest.NewRequestWithContext(ctx, "GET", "/auth/oidc/callback?state="+url.QueryEscape(state), nil), signInErrExpired)
	denied := httptest.NewRequest("GET", "/auth/oidc/callback?error=access_denied&error_description=%3Cscript%3E&state="+url.QueryEscape(state), nil)
	denied.AddCookie(c)
	rec := o.serve(denied)
	if got := signInPageCode(rec); got != signInErrDenied || strings.Contains(rec.Header().Get("Location"), "script") {
		t.Errorf("error from the provider: %d %q; want denied", rec.Code, rec.Header().Get("Location"))
	}
	// A state cookie that has expired.
	st := oidcState{State: state, Nonce: "n", Verifier: "v", Expires: time.Now().Add(-time.Minute).Unix()}
	rec2 := httptest.NewRecorder()
	o.d.Auth.setOIDCState(rec2, st)
	expect("an expired cookie", callbackRequest(code, state, cookie(rec2, oidcStateCookie)), signInErrExpired)
	// The good request still works after all that (nothing was consumed).
	if _, email, _, errCode := o.d.oidcIdentity(ctx, callbackRequest(code, state, c)); errCode != "" || email == "" {
		t.Errorf("the good callback after the refusals = %q, %q", email, errCode)
	}
}

// The state is compared in constant time.
func TestOIDCStateComparisonIsConstantTime(t *testing.T) {
	src := readFile(t, "oidc.go")
	if !strings.Contains(src, "subtle.ConstantTimeCompare([]byte(q.Get(\"state\")), []byte(st.State))") {
		t.Error("the callback does not compare the state with subtle.ConstantTimeCompare")
	}
	if strings.Contains(src, "SkipIssuerCheck") || strings.Contains(src, "SkipClientIDCheck") ||
		strings.Contains(src, "InsecureSkipSignatureCheck") || strings.Contains(src, "SkipExpiryCheck") {
		t.Error("oidc.go switches off one of the verifier's checks")
	}
}

// --- the verifier ---

func TestVerifierAcceptsAsymmetricOnly(t *testing.T) {
	f := newFakeIssuer(t)
	v := newTokenVerifier(f.srv.Client(), f.url(), "aud-1", f.url()+"/jwks", time.Now)
	claims := func(mut func(c map[string]any)) string {
		c := f.goodClaims("n")
		c["aud"] = "aud-1"
		if mut != nil {
			mut(c)
		}
		return f.sign(c)
	}
	tok, err := v.Verify(ctx, claims(nil))
	if err != nil || tok.Claims["email"] != "alice@example.com" || tok.Subject != "subject-1" || tok.Nonce != "n" {
		t.Fatalf("Verify of a good token = %+v, %v", tok, err)
	}
	for name, mut := range map[string]func(c map[string]any){
		"wrong issuer":   func(c map[string]any) { c["iss"] = "https://evil.example.com" },
		"wrong audience": func(c map[string]any) { c["aud"] = "aud-2" },
		"expired":        func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
	} {
		if _, err := v.Verify(ctx, claims(mut)); err == nil {
			t.Errorf("Verify accepted a token with a %s", name)
		}
	}
	for name, setup := range map[string]func(){
		"HS256 with a shared secret": func() { f.alg, f.hmacKey = "HS256", []byte("shared-secret") },
		"alg none":                   func() { f.alg = "none" },
	} {
		setup()
		if _, err := v.Verify(ctx, claims(nil)); err == nil {
			t.Errorf("Verify accepted %s", name)
		}
	}
	f.alg = ""
	for _, raw := range []string{"", "x", "a.b.c", strings.Repeat("a", maxIDToken+1)} {
		if _, err := v.Verify(ctx, raw); err == nil {
			t.Errorf("Verify(%.20q) = nil error", raw)
		}
	}
	// No audience configured: every token is refused (the check is never
	// skipped).
	none := newTokenVerifier(f.srv.Client(), f.url(), "", f.url()+"/jwks", time.Now)
	if _, err := none.Verify(ctx, claims(nil)); err == nil {
		t.Error("a verifier with no audience accepted a token")
	}
	for _, alg := range asymmetricAlgs {
		if strings.HasPrefix(alg, "HS") || alg == "none" {
			t.Errorf("asymmetricAlgs lists %s", alg)
		}
	}
}

// --- the allowlist ---

func TestAllowlist(t *testing.T) {
	list := signIn{issuer: "https://login.example.com", domains: []string{"example.com"}, emails: []string{"boss@example.org", "Mixed@Case.org"}}
	empty := signIn{issuer: "https://login.example.com"}
	tests := []struct {
		name  string
		s     signIn
		email string
		added bool
		want  bool
	}{
		{"domain", list, "alice@example.com", false, true},
		{"exact email", list, "boss@example.org", false, true},
		{"exact email, other case in the list", list, "mixed@case.org", false, true},
		{"subdomain of an allowed domain", list, "a@evil.example.com", false, false},
		{"allowed domain as a prefix", list, "a@example.com.evil.com", false, false},
		{"allowed domain as a suffix", list, "a@notexample.com", false, false},
		{"two at signs", list, "a@example.com@evil.com", false, false},
		{"another address at the email's domain", list, "other@example.org", false, false},
		{"domain in the local part", list, "example.com@evil.com", false, false},
		{"the Kelvin sign is not a k", signIn{domains: []string{"kelvin.com"}}, "a@\u212Aelvin.com", false, false},
		{"empty lists", empty, "alice@example.com", false, false},
		{"empty lists, added", empty, "alice@example.com", true, true},
		{"lists that do not match, added", list, "nobody@elsewhere.net", true, true},
		{"no at sign", list, "alice", false, false},
		{"empty email", list, "", false, false},
		{"empty email, added", empty, "", true, true}, // the caller never asks with an empty email
		{"trailing at sign", list, "alice@", false, false},
	}
	for _, tt := range tests {
		if got := tt.s.allowed(tt.email, map[string]any{}, tt.added); got != tt.want {
			t.Errorf("%s: allowed(%q, added=%v) = %v, want %v", tt.name, tt.email, tt.added, got, tt.want)
		}
	}
}

func TestGoogleNeedsHostedDomain(t *testing.T) {
	s := signIn{issuer: "https://accounts.google.com", domains: []string{"example.com"}, emails: []string{"friend@gmail.com"}}
	tests := []struct {
		name   string
		email  string
		claims map[string]any
		added  bool
		want   bool
	}{
		{"no hd: a personal account on a company address", "alice@example.com", map[string]any{}, false, false},
		{"hd of another domain", "alice@example.com", map[string]any{"hd": "evil.com"}, false, false},
		{"hd is not a string", "alice@example.com", map[string]any{"hd": 5}, false, false},
		{"empty hd", "alice@example.com", map[string]any{"hd": ""}, false, false},
		{"hd equals the domain", "alice@example.com", map[string]any{"hd": "example.com"}, false, true},
		{"hd in another case", "alice@example.com", map[string]any{"hd": "Example.COM"}, false, true},
		{"no hd, but added", "alice@example.com", map[string]any{}, true, true},
		{"an exact email needs no hd", "friend@gmail.com", map[string]any{}, false, true},
		{"nil claims", "alice@example.com", nil, false, false},
	}
	for _, tt := range tests {
		if got := s.allowed(tt.email, tt.claims, tt.added); got != tt.want {
			t.Errorf("%s: allowed = %v, want %v", tt.name, got, tt.want)
		}
	}
	// The trailing-slash spelling of the issuer is Google too; another
	// provider with a domain rule needs no hd.
	slash := s
	slash.issuer = "https://accounts.google.com/"
	if slash.allowed("alice@example.com", map[string]any{}, false) {
		t.Error("the issuer with a trailing slash skipped the hd rule")
	}
	other := signIn{issuer: "https://login.example.com", domains: []string{"example.com"}}
	if !other.allowed("alice@example.com", map[string]any{}, false) {
		t.Error("a provider that is not Google was asked for hd")
	}
}

func TestEmailFromClaims(t *testing.T) {
	good := map[string]any{"email": "Alice@Example.com", "email_verified": true}
	if e, err := emailFrom(good, "email", true); err != nil || e != "alice@example.com" {
		t.Errorf("emailFrom = %q, %v", e, err)
	}
	// A JWT may leave email_verified out, but never say it is false.
	noVerified := map[string]any{"mail": "alice@example.com"}
	if e, err := emailFrom(noVerified, "mail", false); err != nil || e != "alice@example.com" {
		t.Errorf("emailFrom without email_verified = %q, %v", e, err)
	}
	if _, err := emailFrom(noVerified, "mail", true); err == nil {
		t.Error("OIDC accepted a token without email_verified")
	}
	if _, err := emailFrom(map[string]any{"mail": "a@example.com", "email_verified": false}, "mail", false); err == nil {
		t.Error("an email_verified of false was accepted")
	}
	if _, err := emailFrom(good, "mail", true); err == nil {
		t.Error("a missing claim was accepted")
	}
	if _, err := emailFrom(map[string]any{"email": "not-an-address", "email_verified": true}, "email", true); err == nil {
		t.Error("a bad address was accepted")
	}
}

// --- sessions ---

func TestSSOSessionEndsAt24Hours(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	mk := func(signIn string, age, idle time.Duration) store.Session {
		now := t0.Add(age)
		return store.Session{SignIn: signIn, CreatedAt: t0, LastSeenAt: now.Add(-idle)}
	}
	tests := []struct {
		name string
		se   store.Session
		now  time.Duration // since sign-in
		want bool
	}{
		{"sso, used a minute ago, 23h in", mk(store.SignInOIDC, 23*time.Hour, time.Minute), 23 * time.Hour, false},
		{"sso, used a minute ago, 25h in", mk(store.SignInOIDC, 25*time.Hour, time.Minute), 25 * time.Hour, true},
		{"jwt, 25h in", mk(store.SignInJWT, 25*time.Hour, time.Minute), 25 * time.Hour, true},
		{"sso, idle 9h", mk(store.SignInOIDC, 10*time.Hour, 9*time.Hour), 10 * time.Hour, true},
		{"password, 25h in", mk(store.SignInPassword, 25*time.Hour, time.Minute), 25 * time.Hour, false},
		{"password, 6 days in", mk(store.SignInPassword, 6*24*time.Hour, time.Minute), 6 * 24 * time.Hour, false},
		{"password, 8 days in", mk(store.SignInPassword, 8*24*time.Hour, time.Minute), 8 * 24 * time.Hour, true},
		{"password, idle 9h", mk(store.SignInPassword, 10*time.Hour, 9*time.Hour), 10 * time.Hour, true},
		{"a session that does not say how it began is a password one", mk("", 25*time.Hour, time.Minute), 25 * time.Hour, false},
	}
	for _, tt := range tests {
		if got := sessionExpired(tt.se, t0.Add(tt.now)); got != tt.want {
			t.Errorf("%s: sessionExpired = %v, want %v", tt.name, got, tt.want)
		}
	}
	if maxAge(store.SignInOIDC) != 24*time.Hour || maxAge(store.SignInPassword) != 7*24*time.Hour {
		t.Errorf("maxAge: oidc %v, password %v", maxAge(store.SignInOIDC), maxAge(store.SignInPassword))
	}
}

// --- the settings ---

// GET /api/admin/sign-in for settings from the config file: read-only, with
// the secret only as "set".
func TestConfigSignInView(t *testing.T) {
	o := newOIDCHarness(t, func(s *config.SignIn) {
		s.AllowedEmails = []string{"Boss@Example.org"}
		s.AllowedDomains = []string{"Example.com"}
	})
	rec := httptest.NewRecorder()
	o.d.getSignIn(rec, httptest.NewRequest("GET", "/api/admin/sign-in", nil))
	v := decode[SignInAdminView](t, rec, "GET /api/admin/sign-in", http.StatusOK)
	if v.Source != "config" || v.Editable || v.Mode != "oidc" || v.Label != "Google" || v.Issuer != o.f.url() || v.ClientID != testClientID ||
		!v.ClientSecret.Set || v.ClientSecret.UpdatedAt != nil || v.CallbackURL != dashboardURL+"/auth/oidc/callback" ||
		len(v.AllowedDomains) != 1 || v.AllowedDomains[0] != "example.com" || len(v.AllowedEmails) != 1 || v.AllowedEmails[0] != "boss@example.org" {
		t.Errorf("view = %+v", v)
	}
	if strings.Contains(rec.Body.String(), testClientSecret) || strings.Contains(rec.Body.String(), testSecretEnv) {
		t.Errorf("the view shows the secret or its env var: %s", rec.Body)
	}
	// The secret's env var unset: not set.
	t.Setenv(testSecretEnv, "")
	rec = httptest.NewRecorder()
	o.d.getSignIn(rec, httptest.NewRequest("GET", "/api/admin/sign-in", nil))
	if v := decode[SignInAdminView](t, rec, "GET", http.StatusOK); v.ClientSecret.Set {
		t.Error("the secret counts as set with its env var empty")
	}
}

func TestInstancesOffersTheSSOButton(t *testing.T) {
	get := func(h http.Handler) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/instances", nil))
		if rec.Code != 200 {
			t.Fatalf("GET /api/instances = %d %s", rec.Code, rec.Body)
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	o := newOIDCHarness(t, nil)
	m := get(o.h)
	sso, _ := m["sso"].(map[string]any)
	if m["password"] != true || sso["label"] != "Google" || len(sso) != 1 {
		t.Errorf("answer = %v; want the password form and an sso label", m)
	}
	body, _ := json.Marshal(m)
	for _, secret := range []string{testClientSecret, testClientID, o.f.url(), testSecretEnv} {
		if strings.Contains(string(body), secret) {
			t.Errorf("GET /api/instances shows %q", secret)
		}
	}
	// No label: a default one. Not OIDC, or not ready: no button, and the
	// answer is what it was before.
	o2 := newOIDCHarness(t, func(s *config.SignIn) { s.Label = "" })
	if sso, _ := get(o2.h)["sso"].(map[string]any); sso["label"] != "SSO" {
		t.Errorf("default label = %v", sso)
	}
	o3 := newOIDCHarness(t, func(s *config.SignIn) { s.ClientSecretEnv = "PERGENT_TEST_OIDC_UNSET" })
	if _, has := get(o3.h)["sso"]; has {
		t.Error("a button without a secret")
	}
	o4 := newOIDCHarness(t, func(s *config.SignIn) {
		*s = config.SignIn{Mode: config.SignInJWT, Issuer: "https://p.example.com", Audience: "a", JWKSURL: "https://p.example.com/k", Header: "X-Jwt"}
	})
	if m := get(o4.h); m["sso"] != nil || m["password"] != true {
		t.Errorf("answer in jwt mode = %v", m)
	}
}

// The test button answers whether it worked and nothing else: not the
// status, an error, the document, nor an address.
func TestTestButtonSaysOnlyWhetherItWorked(t *testing.T) {
	o := newOIDCHarness(t, nil)
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		o.d.testSignIn(rec, httptest.NewRequest("POST", "/api/admin/sign-in/test", strings.NewReader(body)))
		return rec
	}
	exactly := func(what string, rec *httptest.ResponseRecorder, code int, ok bool) {
		t.Helper()
		want := fmt.Sprintf("{\"ok\":%v}\n", ok)
		if rec.Code != code || rec.Body.String() != want {
			t.Errorf("%s = %d %q; want %d %q", what, rec.Code, rec.Body, code, want)
		}
	}
	exactly("saved OIDC settings", post(""), 200, true)
	exactly("an empty object", post("{}"), 200, true)

	// A provider that answers 500 with a body, or a document with secrets in
	// it, shows nothing.
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "openid-configuration") {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("INTERNAL-DETAIL-10.9.8.7 secret-stack-trace"))
			return
		}
		writeJSON(w, 200, map[string]any{"keys": []any{}, "leak": "INTERNAL-DETAIL"})
	}))
	defer bad.Close()
	o.d.idp = idpClient(bad.Client())
	rec := post(fmt.Sprintf(`{"mode": "oidc", "issuer": %q}`, bad.URL))
	exactly("a provider that answers 500", rec, 200, false)
	for _, leak := range []string{"INTERNAL-DETAIL", "10.9.8.7", "500", bad.URL, "127.0.0.1"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the answer shows %q: %s", leak, rec.Body)
		}
	}
	// A key set with no key is no key set.
	exactly("an empty key set", post(fmt.Sprintf(`{"mode": "jwt", "jwksURL": %q}`, bad.URL+"/keys")), 200, false)
	// Unreachable.
	bad.Close()
	exactly("a provider that is gone", post(fmt.Sprintf(`{"mode": "oidc", "issuer": %q}`, bad.URL)), 200, false)

	// The JWKS of the fake provider works.
	o.d.idp = idpClient(o.f.srv.Client())
	exactly("a working key set", post(fmt.Sprintf(`{"mode": "jwt", "jwksURL": %q}`, o.f.url()+"/jwks")), 200, true)
	// The discovery document must name the issuer it was asked for.
	exactly("an issuer that is not the document's", post(fmt.Sprintf(`{"mode": "oidc", "issuer": %q}`, o.f.url()+"/")), 200, false)

	// Bad requests say so in fixed words.
	for name, body := range map[string]string{
		"http issuer":   `{"mode": "oidc", "issuer": "http://idp.example.com"}`,
		"http jwks":     `{"mode": "jwt", "jwksURL": "http://idp.example.com/keys"}`,
		"unknown field": `{"nope": 1}`,
		"not json":      `{`,
		"unknown mode":  `{"mode": "saml", "issuer": "https://idp.example.com"}`,
		"credentials":   `{"mode": "oidc", "issuer": "https://u:p@idp.example.com"}`,
	} {
		rec := post(body)
		if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "idp.example.com") {
			t.Errorf("%s: %d %s; want 400 without echoing the address", name, rec.Code, rec.Body)
		}
	}
}

// With nothing saved and no body there is nothing to test.
func TestTestButtonWithNothingSet(t *testing.T) {
	d, err := New(Options{Config: &config.Config{Dashboard: config.Dashboard{URL: dashboardURL}}, SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	d.testSignIn(rec, httptest.NewRequest("POST", "/api/admin/sign-in/test", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("test with nothing set = %d %s; want 400", rec.Code, rec.Body)
	}
}

// Redirects are never followed to an identity provider, and the sign-in
// routes take GET only.
func TestIdentityProviderClientFollowsNoRedirect(t *testing.T) {
	hit := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer target.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()
	c := idpClient(redirector.Client())
	res, err := c.Get(redirector.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if hit || res.StatusCode != http.StatusFound {
		t.Errorf("the client followed a redirect (hit=%v, status %d)", hit, res.StatusCode)
	}
	if c.Timeout <= 0 {
		t.Error("the client has no timeout")
	}
	o := newOIDCHarness(t, nil)
	for _, p := range []string{"/auth/oidc/login", "/auth/oidc/callback"} {
		rec := o.serve(httptest.NewRequest("POST", p, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d; want 405", p, rec.Code)
		}
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- with a database [CI] ---

// useOIDC saves OIDC settings in the dashboard against f (secret sealed,
// example.com allowed) and points the dashboard at f. edit may change them.
func (e *env) useOIDC(f *fakeIssuer, edit func(s *store.SignInSettings)) {
	e.t.Helper()
	f.now = e.clock.now
	e.d.idp = idpClient(f.srv.Client())
	s := store.SignInSettings{Mode: store.SignInModeOIDC, Label: "Google", Issuer: f.url(), ClientID: testClientID,
		ClientSecretSealed: e.d.Auth.box.Seal([]byte(testClientSecret)), AllowedDomains: []string{"example.com"},
		UpdatedAt: e.clock.now()}
	if edit != nil {
		edit(&s)
	}
	if err := e.st.PutSignInSettings(ctx, s); err != nil {
		e.t.Fatal(err)
	}
	e.d.forgetProvider()
}

// oidcSignIn runs a sign-in of the person f will vouch for and returns the
// callback's response. browser, when set, is the session cookie the browser
// arrives with.
func (e *env) oidcSignIn(f *fakeIssuer, browser *http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := e.do("GET", "/auth/oidc/login", nil, nil)
	if rec.Code != http.StatusFound {
		e.t.Fatalf("GET /auth/oidc/login = %d %s", rec.Code, rec.Body)
	}
	code, state := f.approve(rec.Header().Get("Location"))
	cookies := []*http.Cookie{cookie(rec, oidcStateCookie)}
	if browser != nil {
		cookies = append(cookies, browser)
	}
	return e.do("GET", "/auth/oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), cookies, nil)
}

func (e *env) ssoAccounts() []store.Account {
	e.t.Helper()
	all, err := e.st.Accounts(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.Account
	for _, a := range all {
		if a.Kind == store.AccountSSO {
			out = append(out, a)
		}
	}
	return out
}

func (e *env) sessionCount() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// A person on an allowed domain signs in: an account is made (a plain user
// with nothing), a session begins that says how, and the browser lands on /.
func TestOIDCSignIn(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useOIDC(f, nil)
	f.claims = func(c map[string]any) { c["email"] = "Alice@Example.COM" }
	rec := e.oidcSignIn(f, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %q %s; want 303 to /", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	c := cookie(rec, SessionCookieName)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != int(SSOMaxSessionAge/time.Second) {
		t.Fatalf("session cookie = %+v; want __Host-, HttpOnly, Secure, Lax, 24 hours", c)
	}
	accounts := e.ssoAccounts()
	if len(accounts) != 1 || accounts[0].Email != "alice@example.com" || accounts[0].Role != store.RoleUser || accounts[0].Added ||
		accounts[0].LastSignInAt == nil {
		t.Fatalf("accounts = %+v; want alice, a plain user who has signed in", accounts)
	}
	se, found := e.storedSession(c)
	if !found || se.SignIn != store.SignInOIDC || se.Issuer != f.url() || se.AccountID != accounts[0].ID || se.Platform != "" {
		t.Fatalf("stored session = %+v, %v", se, found)
	}
	code, me := e.me(c)
	if code != 200 || me.Kind != "sso" || me.Email != "alice@example.com" || me.Role != "user" || me.SignIn != "oidc" || me.Admin || me.Operator {
		t.Errorf("/api/me = %d %+v", code, me)
	}
	if !strings.Contains(strings.Join(e.audits(), "\n"), "alice@example.com sign_in") {
		t.Errorf("audit log = %q; want alice's sign_in", e.audits())
	}
	// A new user can do nothing: she waits for a team.
	if rec := e.get(c, "/api/overview?days=7"); rec.Code != http.StatusForbidden {
		t.Errorf("GET /api/overview = %d %s; want 403 waiting", rec.Code, rec.Body)
	}
	// Signing in again is the same account.
	e.clock.advance(time.Hour)
	if rec := e.oidcSignIn(f, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("second sign-in = %d", rec.Code)
	}
	if got := e.ssoAccounts(); len(got) != 1 || !got[0].LastSignInAt.After(*accounts[0].LastSignInAt) {
		t.Errorf("after a second sign-in: %+v", got)
	}
	for _, r := range e.responses {
		for _, secret := range []string{testClientSecret, "access-token-SECRET"} {
			if strings.Contains(r.Body.String(), secret) {
				t.Errorf("a response shows %q", secret)
			}
		}
	}
	if logs := e.logs.String(); strings.Contains(logs, testClientSecret) || strings.Contains(logs, "access-token-SECRET") || strings.Contains(logs, "eyJ") {
		t.Errorf("the log holds a token or the secret:\n%s", logs)
	}
}

// An admin added the email first: it signs in whatever the allowlist says,
// and stays Added.
func TestOIDCSignInOfAnAddedEmail(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useOIDC(f, nil)
	if _, err := e.st.AddSSOAccount(ctx, "Bob@Other.NET", e.clock.now()); err != nil {
		t.Fatal(err)
	}
	f.claims = func(c map[string]any) { c["email"] = "bob@other.net" }
	rec := e.oidcSignIn(f, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	acc, found, err := e.st.SSOAccountByEmail(ctx, "bob@other.net")
	if err != nil || !found || !acc.Added || acc.LastSignInAt == nil || len(e.ssoAccounts()) != 1 {
		t.Errorf("account = %+v, %v, %v", acc, found, err)
	}
}

// The session the browser arrived with is ended, never adopted.
func TestOIDCSignInEndsTheBrowsersEarlierSession(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useOIDC(f, nil)
	old := e.sessionFor(e.sso("carol@example.com"))
	rec := e.oidcSignIn(f, old)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d", rec.Code)
	}
	if _, found := e.storedSession(old); found {
		t.Error("the earlier session is still there")
	}
	if c := cookie(rec, SessionCookieName); c == nil || c.Value == old.Value {
		t.Errorf("the new session reuses the earlier ID: %+v", c)
	}
}

// Nobody off the allowlist gets an account, a session or a hint of why: the
// sign-in page says to ask an admin.
func TestRefusedCreatesNoAccount(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useOIDC(f, func(s *store.SignInSettings) {
		s.AllowedDomains = []string{"example.com"}
		s.AllowedEmails = []string{"friend@gmail.com"}
	})
	for _, email := range []string{"carol@other.net", "a@evil.example.com", "alice@example.com.evil.com", "boss@gmail.com"} {
		f.claims = func(c map[string]any) { c["email"] = email }
		rec := e.oidcSignIn(f, nil)
		if got := signInPageCode(rec); got != signInErrNotAllowed {
			t.Errorf("%s: callback = %d %q; want /signin?error=not_allowed", email, rec.Code, rec.Header().Get("Location"))
		}
		if cookie(rec, SessionCookieName) != nil {
			t.Errorf("%s: a session cookie was set", email)
		}
	}
	if got := e.ssoAccounts(); len(got) != 0 {
		t.Errorf("accounts after the refusals = %+v; want none", got)
	}
	if n := e.sessionCount(); n != 0 {
		t.Errorf("%d sessions after the refusals; want none", n)
	}
	// With both lists empty only added emails get in.
	e.useOIDC(f, func(s *store.SignInSettings) { s.AllowedDomains = nil })
	f.claims = func(c map[string]any) { c["email"] = "alice@example.com" }
	if got := signInPageCode(e.oidcSignIn(f, nil)); got != signInErrNotAllowed {
		t.Errorf("empty lists: %q; want not_allowed", got)
	}
	if got := e.ssoAccounts(); len(got) != 0 {
		t.Errorf("accounts with empty lists = %+v", got)
	}
}

// A refused sign-in does not end the session the browser arrived with.
func TestRefusedSignInKeepsTheEarlierSession(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useOIDC(f, nil)
	keep := e.sessionFor(e.sso("carol@example.com"))
	f.claims = func(c map[string]any) { c["email"] = "mallory@evil.net" }
	if got := signInPageCode(e.oidcSignIn(f, keep)); got != signInErrNotAllowed {
		t.Fatalf("callback = %q", got)
	}
	if _, found := e.storedSession(keep); !found {
		t.Error("a refused sign-in ended the browser's session")
	}
}

// An SSO session ends 24 hours after sign-in however much it is used.
func TestSSOSessionEndsAt24HoursThroughRequire(t *testing.T) {
	e := newEnv(t, nil)
	sso := e.sessionFor(e.ssoAdmin("root@example.com"))
	for hours := 5; hours <= 20; hours += 5 {
		e.clock.advance(5 * time.Hour) // used every 5 hours: never idle
		if code, _ := e.session(sso); code != http.StatusOK {
			t.Fatalf("SSO session after %d hours = %d; want 200", hours, code)
		}
	}
	e.clock.advance(5 * time.Hour) // 25 hours in, used 5 hours ago
	if code, _ := e.session(sso); code != http.StatusUnauthorized {
		t.Errorf("SSO session after 25 hours = %d; want 401", code)
	}
	if _, found := e.storedSession(sso); found {
		t.Error("the expired SSO session was not deleted")
	}
}

// A local admin keeps today's limits: 7 days at most, 8 hours idle.
func TestLocalAdminSessionLastsBeyond24Hours(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.operator()
	for hours := 7; hours <= 168; hours += 7 { // used every 7 hours: never idle
		e.clock.advance(7 * time.Hour)
		if code, _ := e.session(admin); code != http.StatusOK {
			t.Fatalf("local admin session after %d hours = %d; want 200", hours, code)
		}
	}
	e.clock.advance(7 * time.Hour) // 175 hours in, past 7 days
	if code, _ := e.session(admin); code != http.StatusUnauthorized {
		t.Errorf("a local admin session after 7 days = %d; want 401", code)
	}
}

func TestPruneSessionsEndsSSOAt24Hours(t *testing.T) {
	e := newEnv(t, nil)
	sso := e.sessionFor(e.sso("carol@example.com"))
	admin := e.operator()
	e.clock.advance(23 * time.Hour)
	for _, c := range []*http.Cookie{sso, admin} {
		if err := e.st.TouchSession(ctx, hashOf(t, c), e.clock.now()); err != nil {
			t.Fatal(err)
		}
	}
	e.clock.advance(2 * time.Hour) // 25 hours in, used 2 hours ago
	n, err := e.d.Auth.PruneSessions(ctx, e.clock.now())
	if err != nil || n != 1 {
		t.Fatalf("PruneSessions = %d, %v; want only the SSO session", n, err)
	}
	if _, found := e.storedSession(admin); !found {
		t.Error("the local admin's session was pruned")
	}
}

// --- the settings through the API [CI] ---

func (e *env) putSignIn(c *http.Cookie, body map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.sendJSON(c, "PUT", "/api/admin/sign-in", body)
}

func oidcBody(f *fakeIssuer, edit func(m map[string]any)) map[string]any {
	m := map[string]any{"mode": "oidc", "label": "Google", "issuer": f.url(), "clientID": testClientID,
		"clientSecret": testClientSecret, "allowedDomains": []string{"example.com"}, "allowedEmails": []string{"Boss@Example.org"}}
	if edit != nil {
		edit(m)
	}
	return m
}

func TestSignInSettingsRoundTripThroughTheAPI(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	f := newFakeIssuer(t)
	e.d.idp = idpClient(f.srv.Client())
	v := getJSON[SignInAdminView](e, c, "/api/admin/sign-in")
	if v.Source != "dashboard" || !v.Editable || v.Mode != "" || v.ClientSecret.Set || v.AllowedDomains == nil || v.AllowedEmails == nil ||
		v.CallbackURL != dashboardURL+"/auth/oidc/callback" {
		t.Errorf("empty view = %+v", v)
	}
	rec := e.putSignIn(c, oidcBody(f, nil))
	v = decode[SignInAdminView](t, rec, "PUT /api/admin/sign-in", http.StatusOK)
	if v.Mode != "oidc" || v.Label != "Google" || v.Issuer != f.url() || v.ClientID != testClientID || !v.ClientSecret.Set ||
		len(v.AllowedDomains) != 1 || len(v.AllowedEmails) != 1 || v.AllowedEmails[0] != "boss@example.org" || !v.Editable {
		t.Errorf("view after PUT = %+v", v)
	}
	// The sign-in page learns about the button.
	var inst struct {
		SSO *ssoView `json:"sso"`
	}
	if err := json.Unmarshal(e.get(nil, "/api/instances").Body.Bytes(), &inst); err != nil || inst.SSO == nil || inst.SSO.Label != "Google" {
		t.Errorf("GET /api/instances sso = %+v, %v", inst.SSO, err)
	}
	// JWT's fields are stored too; the email claim defaults.
	rec = e.putSignIn(c, map[string]any{"mode": "jwt", "issuer": "https://proxy.example.com", "audience": "pergent",
		"jwksURL": "https://proxy.example.com/keys", "header": "X-Auth-Token"})
	v = decode[SignInAdminView](t, rec, "PUT jwt", http.StatusOK)
	if v.Mode != "jwt" || v.EmailClaim != "email" || v.Audience != "pergent" || v.JWKSURL != "https://proxy.example.com/keys" || v.Header != "X-Auth-Token" {
		t.Errorf("jwt view = %+v", v)
	}
	var jwtInst struct {
		SSO *ssoView `json:"sso"`
	}
	if err := json.Unmarshal(e.get(nil, "/api/instances").Body.Bytes(), &jwtInst); err != nil || jwtInst.SSO != nil {
		t.Errorf("the OIDC button is offered in jwt mode: %+v", jwtInst.SSO)
	}
	// The save is audited, without values.
	var found bool
	for _, a := range e.auditOf("sign_in_updated") {
		found = found || strings.Contains(a.Detail, "mode")
		if strings.Contains(a.Detail, testClientSecret) || strings.Contains(a.Detail, "example.com") {
			t.Errorf("audit detail shows a value: %q", a.Detail)
		}
	}
	if !found {
		t.Error("no sign_in_updated entry naming the mode")
	}
}

func TestSignInSettingsAreHttpsOnly(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	f := newFakeIssuer(t)
	for name, edit := range map[string]func(m map[string]any){
		"http issuer":              func(m map[string]any) { m["issuer"] = "http://accounts.google.com" },
		"issuer with a login":      func(m map[string]any) { m["issuer"] = "https://u:p@accounts.google.com" },
		"issuer with a query":      func(m map[string]any) { m["issuer"] = "https://accounts.google.com?x=1" },
		"issuer is not a URL":      func(m map[string]any) { m["issuer"] = "accounts.google.com" },
		"issuer javascript:":       func(m map[string]any) { m["issuer"] = "javascript:alert(1)" },
		"http jwks":                func(m map[string]any) { m["jwksURL"] = "http://proxy.example.com/keys" },
		"file jwks":                func(m map[string]any) { m["jwksURL"] = "file:///etc/passwd" },
		"jwks with a fragment":     func(m map[string]any) { m["jwksURL"] = "https://proxy.example.com/keys#x" },
		"unknown mode":             func(m map[string]any) { m["mode"] = "saml" },
		"bad domain":               func(m map[string]any) { m["allowedDomains"] = []string{"example.com/evil"} },
		"bad email":                func(m map[string]any) { m["allowedEmails"] = []string{"a@b@c.com"} },
		"no client id":             func(m map[string]any) { m["clientID"] = "" },
		"no secret the first time": func(m map[string]any) { delete(m, "clientSecret") },
		"unknown field":            func(m map[string]any) { m["oauthClientID"] = "x" },
	} {
		rec := e.putSignIn(c, oidcBody(f, edit))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d %s; want 400", name, rec.Code, rec.Body)
		}
	}
	rec := e.putSignIn(c, map[string]any{"mode": "jwt", "issuer": "https://p.example.com", "jwksURL": "https://p.example.com/k", "header": "X-A"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, rec), "audience") {
		t.Errorf("jwt without an audience: %d %s; want 400 naming it", rec.Code, rec.Body)
	}
	row, err := e.st.SignInSettings(ctx)
	if err != nil || row.Mode != "" || row.Issuer != "" {
		t.Errorf("settings after the refusals = %+v, %v; want nothing stored", row, err)
	}
	if n := len(e.auditOf("sign_in_updated")); n != 0 {
		t.Errorf("%d audit entries for refused saves", n)
	}
}

// The client secret goes in and never comes out.
func TestClientSecretIsWriteOnly(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	f := newFakeIssuer(t)
	e.d.idp = idpClient(f.srv.Client())
	rec := e.putSignIn(c, oidcBody(f, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{rec.Body.String(), e.get(c, "/api/admin/sign-in").Body.String(), e.get(nil, "/api/instances").Body.String(),
		e.get(c, "/api/admin/settings").Body.String(), e.get(c, "/api/me").Body.String()} {
		if strings.Contains(body, testClientSecret) {
			t.Errorf("a response shows the secret: %s", body)
		}
	}
	// Stored sealed.
	row, err := e.st.SignInSettings(ctx)
	if err != nil || len(row.ClientSecretSealed) == 0 || strings.Contains(string(row.ClientSecretSealed), testClientSecret) {
		t.Fatalf("stored secret = %q, %v; want sealed bytes", row.ClientSecretSealed, err)
	}
	if plain, err := e.d.Auth.box.Open(row.ClientSecretSealed); err != nil || string(plain) != testClientSecret {
		t.Errorf("sealed secret opens to %q, %v", plain, err)
	}
	// Without a clientSecret the stored one stays.
	rec = e.putSignIn(c, oidcBody(f, func(m map[string]any) { delete(m, "clientSecret"); m["label"] = "Company" }))
	if v := decode[SignInAdminView](t, rec, "PUT without a secret", http.StatusOK); !v.ClientSecret.Set || v.Label != "Company" {
		t.Errorf("view = %+v; want the secret kept", v)
	}
	if s, _ := e.d.signInSettings(ctx); s.clientSecret != testClientSecret {
		t.Errorf("the secret in force after a PUT without one = %q", s.clientSecret)
	}
	// A new one replaces it.
	rec = e.putSignIn(c, oidcBody(f, func(m map[string]any) { m["clientSecret"] = "a-second-secret" }))
	decode[SignInAdminView](t, rec, "PUT with a new secret", http.StatusOK)
	if s, _ := e.d.signInSettings(ctx); s.clientSecret != "a-second-secret" {
		t.Errorf("the secret in force = %q; want the new one", s.clientSecret)
	}
	// OIDC without a secret is refused; "" removes it where it is not needed.
	rec = e.putSignIn(c, oidcBody(f, func(m map[string]any) { m["clientSecret"] = "" }))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("OIDC with the secret removed = %d %s; want 400", rec.Code, rec.Body)
	}
	rec = e.putSignIn(c, oidcBody(f, func(m map[string]any) { m["clientSecret"] = ""; m["mode"] = "" }))
	if v := decode[SignInAdminView](t, rec, "PUT removing the secret", http.StatusOK); v.ClientSecret.Set || v.Mode != "" {
		t.Errorf("view after removing the secret = %+v", v)
	}
	// Neither the audit log nor the log holds a secret.
	for _, a := range e.auditEntries() {
		if strings.Contains(a.Detail, testClientSecret) || strings.Contains(a.Detail, "a-second-secret") {
			t.Errorf("audit entry shows a secret: %+v", a)
		}
	}
	if logs := e.logs.String(); strings.Contains(logs, testClientSecret) || strings.Contains(logs, "a-second-secret") {
		t.Errorf("the log shows a secret:\n%s", logs)
	}
}

// A new issuer needs the secret entered again, and a stored secret is never
// kept for it, not even after a detour through another mode.
func TestIssuerChangeNeedsSecretAgain(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	f, g := newFakeIssuer(t), newFakeIssuer(t)
	e.d.idp = idpClient(f.srv.Client())
	if rec := e.putSignIn(c, oidcBody(f, nil)); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	before, _ := e.st.SignInSettings(ctx)

	// New issuer, no secret: refused, nothing changed.
	rec := e.putSignIn(c, oidcBody(g, func(m map[string]any) { delete(m, "clientSecret") }))
	if rec.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, rec), "secret") {
		t.Errorf("new issuer without the secret = %d %s; want 400 about the secret", rec.Code, rec.Body)
	}
	rec = e.putSignIn(c, oidcBody(g, func(m map[string]any) { m["clientSecret"] = "" }))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("new issuer with an empty secret = %d; want 400", rec.Code)
	}
	if after, _ := e.st.SignInSettings(ctx); after.Issuer != before.Issuer || string(after.ClientSecretSealed) != string(before.ClientSecretSealed) {
		t.Errorf("a refused change altered the settings: %+v", after)
	}

	// The detour: another issuer in jwt mode drops the stored secret, so
	// coming back to OIDC on that issuer still needs a secret.
	rec = e.putSignIn(c, map[string]any{"mode": "jwt", "issuer": g.url(), "audience": "a", "jwksURL": g.url() + "/jwks", "header": "X-A"})
	if rec.Code != http.StatusOK {
		t.Fatalf("jwt PUT = %d %s", rec.Code, rec.Body)
	}
	if mid, _ := e.st.SignInSettings(ctx); len(mid.ClientSecretSealed) != 0 {
		t.Error("the secret of the old issuer was kept for the new one")
	}
	rec = e.putSignIn(c, oidcBody(g, func(m map[string]any) { delete(m, "clientSecret") }))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("OIDC on the new issuer without a secret = %d %s; want 400", rec.Code, rec.Body)
	}
	if rec := e.putSignIn(c, oidcBody(g, nil)); rec.Code != http.StatusOK {
		t.Errorf("with the secret = %d %s", rec.Code, rec.Body)
	}
}

// Changing the mode or the issuer ends every SSO session; a local admin's
// stays; changing something else ends nothing.
func TestIssuerChangeEndsSSOSessions(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	f, g := newFakeIssuer(t), newFakeIssuer(t)
	e.d.idp = idpClient(f.srv.Client())
	if rec := e.putSignIn(c, oidcBody(f, nil)); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	sso := func() *http.Cookie { return e.sessionFor(e.sso("carol@example.com")) }
	alive := func(what string, cookie *http.Cookie, want bool) {
		t.Helper()
		if _, found := e.storedSession(cookie); found != want {
			t.Errorf("%s: session present = %v, want %v", what, found, want)
		}
	}

	a := sso()
	if rec := e.putSignIn(c, oidcBody(f, func(m map[string]any) { m["label"] = "Company"; m["allowedDomains"] = []string{"example.org"} })); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	alive("after a change of label and allowlist", a, true)

	rec := e.putSignIn(c, oidcBody(g, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT new issuer = %d %s", rec.Code, rec.Body)
	}
	alive("after a new issuer", a, false)
	alive("the local admin's session after a new issuer", c, true)
	if code, _ := e.session(c); code != http.StatusOK {
		t.Errorf("the local admin's session = %d after the change", code)
	}

	b := sso()
	if rec := e.putSignIn(c, map[string]any{"mode": "jwt", "issuer": g.url(), "audience": "a", "jwksURL": g.url() + "/jwks", "header": "X-A"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT jwt = %d %s", rec.Code, rec.Body)
	}
	alive("after a new mode", b, false)
	alive("the local admin's session after a new mode", c, true)
	var ended bool
	for _, a := range e.auditOf("sign_in_updated") {
		ended = ended || strings.Contains(a.Detail, "SSO sessions ended")
	}
	if !ended {
		t.Error("the audit log does not say that sessions ended")
	}
}

// What the config file sets wins, and the dashboard cannot change it.
func TestConfigSignInWinsOverTheDashboard(t *testing.T) {
	f := newFakeIssuer(t)
	t.Setenv(testSecretEnv, testClientSecret)
	e := newEnv(t, func(cfg *config.Config) {
		cfg.Dashboard.SignIn = config.SignIn{Mode: config.SignInOIDC, Label: "From file", Issuer: f.url(), ClientID: testClientID,
			ClientSecretEnv: testSecretEnv, AllowedDomains: []string{"file.example"}}
	})
	e.useOIDC(f, func(s *store.SignInSettings) {
		s.Label = "From dashboard"
		s.AllowedDomains = []string{"dashboard.example"}
	})
	c := e.operator()
	v := getJSON[SignInAdminView](e, c, "/api/admin/sign-in")
	if v.Source != "config" || v.Editable || v.Label != "From file" || len(v.AllowedDomains) != 1 || v.AllowedDomains[0] != "file.example" {
		t.Errorf("view = %+v; want the config's, read-only", v)
	}
	rec := e.putSignIn(c, oidcBody(f, nil))
	if rec.Code != http.StatusConflict || !strings.Contains(errorOf(t, rec), "config file") {
		t.Errorf("PUT = %d %s; want 409 naming the config file", rec.Code, rec.Body)
	}
	if row, _ := e.st.SignInSettings(ctx); row.Label != "From dashboard" {
		t.Errorf("the 409 still changed the stored settings: %+v", row)
	}
	s, err := e.d.signInSettings(ctx)
	if err != nil || s.label != "From file" || !s.fromConfig || s.clientSecret != testClientSecret {
		t.Errorf("settings in force = %+v, %v", s, err)
	}
}

func TestSignInRoutesAreForAdminsOnly(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	alice := e.aliceSession()
	for _, r := range [][2]string{{"GET", "/api/admin/sign-in"}, {"PUT", "/api/admin/sign-in"}, {"POST", "/api/admin/sign-in/test"}} {
		if rec := e.do(r[0], r[1], nil, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d; want 401", r[0], r[1], rec.Code)
		}
	}
	if rec := e.get(alice, "/api/admin/sign-in"); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("GET as a user = %d; want 403 or 404", rec.Code)
	}
	e.team("viewers", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "main"})
	for _, r := range [][2]string{{"PUT", "/api/admin/sign-in"}, {"POST", "/api/admin/sign-in/test"}} {
		body, _ := json.Marshal(oidcBody(f, nil))
		if rec := e.send(alice, r[0], r[1], string(body)); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Errorf("%s as a maintainer = %d; want 403 or 404", r[0], rec.Code)
		}
	}
	if row, _ := e.st.SignInSettings(ctx); row.Mode != "" {
		t.Errorf("a user changed the sign-in settings: %+v", row)
	}
}

func TestTestButtonThroughTheAPI(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	f := newFakeIssuer(t)
	e.d.idp = idpClient(f.srv.Client())
	rec := e.sendJSON(c, "POST", "/api/admin/sign-in/test", map[string]any{"mode": "oidc", "issuer": f.url()})
	if rec.Code != 200 || rec.Body.String() != "{\"ok\":true}\n" {
		t.Errorf("test = %d %q", rec.Code, rec.Body)
	}
	f.srv.Close()
	rec = e.sendJSON(c, "POST", "/api/admin/sign-in/test", map[string]any{"mode": "oidc", "issuer": f.url()})
	if rec.Code != 200 || rec.Body.String() != "{\"ok\":false}\n" {
		t.Errorf("test of a dead provider = %d %q", rec.Code, rec.Body)
	}
}
