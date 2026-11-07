package dashboard

// JWT validation (a proxy in front has logged the person in). The tests
// that need no database run everywhere, with a dashboard that has no store:
// if the code reached for one it would panic, so "starts nothing" is proved
// by them. The tests with a session need PERGENT_SH_TEST_DATABASE_URL and are
// marked [CI].

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	jwtAudience = "pergent-jwt-audience"
	jwtHeader   = "X-Forwarded-Token"
)

// jwtToken is a valid token from f for alice at now; edit may change its
// claims.
func jwtToken(f *fakeIssuer, now time.Time, edit func(c map[string]any)) string {
	c := map[string]any{
		"iss": f.url(), "aud": jwtAudience, "sub": "user-1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"email": "alice@example.com", "email_verified": true,
	}
	if edit != nil {
		edit(c)
	}
	return f.sign(c)
}

// forgetJWTVerifier drops the verifier and the keys it cached.
func (a *Auth) forgetJWTVerifier() {
	a.jwt.mu.Lock()
	a.jwt.v, a.jwt.key, a.jwt.hc = nil, "", nil
	a.jwt.mu.Unlock()
}

// jwtHarness is a dashboard whose config file sets JWT validation against a
// fake issuer, with no store.
type jwtHarness struct {
	t     *testing.T
	f     *fakeIssuer
	d     *Dashboard
	h     http.Handler
	pages http.Handler
	log   *lockedBuffer
	// pageHits counts the requests the page handler behind Pages was given.
	pageHits *atomic.Int32
}

func newJWTHarness(t *testing.T, tweak func(s *config.SignIn)) *jwtHarness {
	t.Helper()
	f := newFakeIssuer(t)
	si := config.SignIn{Mode: config.SignInJWT, Issuer: f.url(), Audience: jwtAudience, JWKSURL: f.url() + "/jwks",
		Header: jwtHeader, AllowedDomains: []string{"example.com"}}
	if tweak != nil {
		tweak(&si)
	}
	logs := &lockedBuffer{}
	d, err := New(Options{Config: &config.Config{Dashboard: config.Dashboard{URL: dashboardURL, SignIn: si}},
		SecretKey: testKey(t), HTTPClient: f.srv.Client(),
		Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hits := &atomic.Int32{}
	pages := d.Pages(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("page"))
	}))
	return &jwtHarness{t: t, f: f, d: d, h: d.Handler(), pages: pages, log: logs, pageHits: hits}
}

func (j *jwtHarness) token(edit func(c map[string]any)) string {
	return jwtToken(j.f, time.Now(), edit)
}

// request is a request with the proxy's header set to value ("" sets none).
func jwtRequest(method, target, value string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if value != "" {
		req.Header.Set(jwtHeader, value)
	}
	return req
}

func (j *jwtHarness) read(value string) (string, headerStatus) {
	return j.d.Auth.fromHeader(jwtRequest("GET", "/api/me", value))
}

func (j *jwtHarness) serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (j *jwtHarness) noTokenLogged() {
	j.t.Helper()
	logs := j.log.String()
	for _, bad := range []string{"eyJ", "JWKS-BODY-MUST-NOT-LEAK"} {
		if strings.Contains(logs, bad) {
			j.t.Errorf("the log holds %q:\n%s", bad, logs)
		}
	}
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
}

func wantHeader(t *testing.T, what, email string, st headerStatus, wantEmail string, want headerStatus) {
	t.Helper()
	if st != want || email != wantEmail {
		t.Errorf("%s: fromHeader = %q, %d; want %q, %d", what, email, st, wantEmail, want)
	}
}

// The header may hold "Bearer <token>" (any case) or the bare token.
func TestJWTReadsBearerAndBareToken(t *testing.T) {
	j := newJWTHarness(t, nil)
	tok := j.token(nil)
	for _, v := range []string{tok, "Bearer " + tok, "bearer " + tok, "BEARER   " + tok, "  Bearer " + tok + "  "} {
		email, st := j.read(v)
		wantHeader(t, "header "+v[:min(len(v), 12)], email, st, "alice@example.com", headerValid)
	}
	// Nothing, or an empty value, is no header at all.
	email, st := j.read("")
	wantHeader(t, "no header", email, st, "", headerNone)
	for _, bad := range []string{"Bearer", "Bearer ", "Basic " + tok, "Bearer " + tok + " extra", "Bearer a.b.c", "not a token"} {
		email, st := j.read(bad)
		wantHeader(t, "bad header "+bad[:min(len(bad), 12)], email, st, "", headerInvalid)
	}
	// Two values of the header are ambiguous: the browser could have sent one.
	req := jwtRequest("GET", "/api/me", tok)
	req.Header.Add(jwtHeader, tok)
	email, st = j.d.Auth.fromHeader(req)
	wantHeader(t, "header twice", email, st, "", headerInvalid)
	j.noTokenLogged()
}

func TestJWTRefusesNone(t *testing.T) {
	j := newJWTHarness(t, nil)
	j.f.alg = "none"
	email, st := j.read(j.token(nil))
	wantHeader(t, "alg none", email, st, "", headerInvalid)
	// Even with the signature part left off altogether.
	j.f.alg = "RS256"
	parts := strings.Split(j.token(nil), ".")
	email, st = j.read(parts[0] + "." + parts[1] + ".")
	wantHeader(t, "empty signature", email, st, "", headerInvalid)
}

func TestJWTRefusesHS256(t *testing.T) {
	j := newJWTHarness(t, nil)
	pub, err := x509.MarshalPKIXPublicKey(&j.f.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string][]byte{"a secret": []byte("shared-secret"), "the public key as the secret": pub} {
		j.f.alg, j.f.hmacKey = "HS256", key
		email, st := j.read(j.token(nil))
		wantHeader(t, "HS256 with "+name, email, st, "", headerInvalid)
	}
}

func TestJWTRefusesWrongAudience(t *testing.T) {
	j := newJWTHarness(t, nil)
	for name, aud := range map[string]any{"other": "another-app", "empty": "", "list without ours": []string{"a", "b"}} {
		email, st := j.read(j.token(func(c map[string]any) { c["aud"] = aud }))
		wantHeader(t, "audience "+name, email, st, "", headerInvalid)
	}
	// One of several audiences is enough.
	email, st := j.read(j.token(func(c map[string]any) { c["aud"] = []string{"other", jwtAudience} }))
	wantHeader(t, "audience list with ours", email, st, "alice@example.com", headerValid)
}

func TestJWTRefusesWrongIssuer(t *testing.T) {
	j := newJWTHarness(t, nil)
	email, st := j.read(j.token(func(c map[string]any) { c["iss"] = "https://other.example.com" }))
	wantHeader(t, "other issuer", email, st, "", headerInvalid)
	// Signed by a key the issuer does not publish.
	other := newFakeIssuer(t)
	j.f.signWith = other.key
	email, st = j.read(j.token(nil))
	wantHeader(t, "other key", email, st, "", headerInvalid)
}

// A token meant for another app behind the proxy must be refused, so an issuer
// and an audience are both required: without either, every token is refused.
func TestJWTNeedsIssuerAndAudience(t *testing.T) {
	for name, tweak := range map[string]func(s *config.SignIn){
		"no audience": func(s *config.SignIn) { s.Audience = "" },
		"no issuer":   func(s *config.SignIn) { s.Issuer = "" },
		"no jwks url": func(s *config.SignIn) { s.JWKSURL = "" },
	} {
		j := newJWTHarness(t, tweak)
		email, st := j.read(j.token(nil))
		wantHeader(t, name, email, st, "", headerInvalid)
		// And with no header there is nothing to refuse.
		email, st = j.read("")
		wantHeader(t, name+", no header", email, st, "", headerNone)
	}
}

func TestJWTRefusesExpiredAndNotYetValid(t *testing.T) {
	j := newJWTHarness(t, nil)
	now := time.Now()
	for name, edit := range map[string]func(c map[string]any){
		"expired":   func(c map[string]any) { c["exp"] = now.Add(-time.Hour).Unix() },
		"no expiry": func(c map[string]any) { delete(c, "exp") },
		"not yet":   func(c map[string]any) { c["nbf"] = now.Add(time.Hour).Unix() },
	} {
		email, st := j.read(j.token(edit))
		wantHeader(t, name, email, st, "", headerInvalid)
	}
}

func TestJWTEmailClaim(t *testing.T) {
	j := newJWTHarness(t, nil)
	cases := []struct {
		name string
		edit func(c map[string]any)
		want string
		st   headerStatus
	}{
		{"upper case is folded", func(c map[string]any) { c["email"] = "Alice@Example.COM" }, "alice@example.com", headerValid},
		{"no email", func(c map[string]any) { delete(c, "email") }, "", headerInvalid},
		{"email is not a string", func(c map[string]any) { c["email"] = 7 }, "", headerInvalid},
		{"not an address", func(c map[string]any) { c["email"] = "alice" }, "", headerInvalid},
		{"two addresses", func(c map[string]any) { c["email"] = "a@example.com, b@example.com" }, "", headerInvalid},
		{"verified false", func(c map[string]any) { c["email_verified"] = false }, "", headerInvalid},
		{"verified the word false", func(c map[string]any) { c["email_verified"] = "false" }, "", headerInvalid},
		{"verified the string true", func(c map[string]any) { c["email_verified"] = "true" }, "alice@example.com", headerValid},
		{"verified absent", func(c map[string]any) { delete(c, "email_verified") }, "alice@example.com", headerValid},
	}
	for _, c := range cases {
		email, st := j.read(j.token(c.edit))
		wantHeader(t, c.name, email, st, c.want, c.st)
	}
	// The email_claim setting names the claim; "email" is then ignored.
	j = newJWTHarness(t, func(s *config.SignIn) { s.EmailClaim = "upn" })
	email, st := j.read(j.token(func(c map[string]any) { c["upn"] = "Bob@Example.com" }))
	wantHeader(t, "claim upn", email, st, "bob@example.com", headerValid)
	email, st = j.read(j.token(nil))
	wantHeader(t, "claim upn missing", email, st, "", headerInvalid)
}

// The config path leaves email_claim empty when the file does; it means
// "email", as it does when it is saved in the dashboard.
func TestJWTEmailClaimDefaultsToEmailFromTheConfig(t *testing.T) {
	j := newJWTHarness(t, nil)
	s, err := j.d.signInSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.emailClaim != "email" {
		t.Errorf("email claim of a JWT config with none set = %q; want \"email\"", s.emailClaim)
	}
	rec := httptest.NewRecorder()
	j.d.getSignIn(rec, httptest.NewRequest("GET", "/api/admin/sign-in", nil))
	if v := decode[SignInAdminView](t, rec, "GET /api/admin/sign-in", http.StatusOK); v.EmailClaim != "email" {
		t.Errorf("the view's emailClaim = %q; want \"email\"", v.EmailClaim)
	}
	email, st := j.read(j.token(nil))
	wantHeader(t, "default claim", email, st, "alice@example.com", headerValid)
	// A named claim is kept, and OIDC has no such default.
	j = newJWTHarness(t, func(s *config.SignIn) { s.EmailClaim = "upn" })
	if s, _ := j.d.signInSettings(ctx); s.emailClaim != "upn" {
		t.Errorf("email claim = %q; want upn", s.emailClaim)
	}
	o := newOIDCHarness(t, nil)
	if s, _ := o.d.signInSettings(ctx); s.emailClaim != "" {
		t.Errorf("an OIDC config got the email claim %q", s.emailClaim)
	}
}

// With any other mode, or none, the header means nothing.
func TestJWTHeaderIsIgnoredWhenTheModeIsNotJWT(t *testing.T) {
	o := newOIDCHarness(t, nil)
	tok := jwtToken(o.f, time.Now(), nil)
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set(jwtHeader, tok)
	req.Header.Set("Authorization", "Bearer "+tok)
	email, st := o.d.Auth.fromHeader(req)
	wantHeader(t, "oidc mode", email, st, "", headerNone)
	if rec := o.serve(req); rec.Code != http.StatusUnauthorized || cookie(rec, SessionCookieName) != nil {
		t.Errorf("GET /api/me with a header in OIDC mode = %d, cookie %v; want 401 and none", rec.Code, cookie(rec, SessionCookieName))
	}
}

// The JWKS cannot be fetched and nothing is cached: that is an outage
// (headerUnavailable), not a bad token. A token that is bad by itself is
// still invalid.
func TestJWTKeyOutageIs503(t *testing.T) {
	j := newJWTHarness(t, nil)
	j.f.setJWKSDown(true)
	email, st := j.read(j.token(nil))
	wantHeader(t, "keys down", email, st, "", headerUnavailable)
	// A token that fails before any key is needed is not an outage.
	j.f.alg = "none"
	email, st = j.read(j.token(nil))
	wantHeader(t, "none with keys down", email, st, "", headerInvalid)
	j.f.alg = ""
	// The same through the API: 503, no cookie.
	req := jwtRequest("GET", "/api/me", j.token(nil))
	if rec := j.serve(j.h, req); rec.Code != http.StatusServiceUnavailable || cookie(rec, SessionCookieName) != nil {
		t.Errorf("GET /api/me with the keys down = %d %s, cookie %v; want 503 and no session", rec.Code, rec.Body, cookie(rec, SessionCookieName))
	}
	// The keys are back: valid. Then down again: the cached key still works.
	j.f.setJWKSDown(false)
	email, st = j.read(j.token(nil))
	wantHeader(t, "keys back", email, st, "alice@example.com", headerValid)
	j.f.setJWKSDown(true)
	email, st = j.read(j.token(nil))
	wantHeader(t, "keys down, cached", email, st, "alice@example.com", headerValid)
	// A forged signature while the keys are down can't be told from a rotated
	// key: an outage, never a pass.
	j.f.signWith = newFakeIssuer(t).key
	email, st = j.read(j.token(nil))
	if st == headerValid {
		t.Errorf("a forged token passed while the keys were down: %q", email)
	}
	j.noTokenLogged()
}

// A write never starts a session, however valid the header: the proxy adds it
// to any request the browser sends, so a cross-site POST must start nothing.
// The dashboard here has no store, so an attempt would panic.
func TestJWTPostStartsNothing(t *testing.T) {
	j := newJWTHarness(t, nil)
	tok := j.token(nil)
	for _, c := range []struct{ method, target string }{
		{"POST", "/api/me"}, {"POST", "/api/teams"}, {"PUT", "/api/admin/sign-in"}, {"DELETE", "/api/teams/1"},
		{"POST", "/auth/logout"}, {"POST", "/auth/password/change"}, {"HEAD", "/api/me"}, {"OPTIONS", "/api/me"},
	} {
		rec := j.serve(j.h, jwtRequest(c.method, c.target, "Bearer "+tok))
		if s := cookie(rec, SessionCookieName); s != nil && s.MaxAge >= 0 {
			t.Errorf("%s %s started a session: %+v", c.method, c.target, s)
		}
		if rec.Code < 400 && c.target != "/auth/logout" {
			t.Errorf("%s %s with a valid header and no session = %d; want a refusal", c.method, c.target, rec.Code)
		}
	}
	// And the same for a page.
	for _, method := range []string{"POST", "PUT", "DELETE", "HEAD", "OPTIONS"} {
		rec := j.serve(j.pages, jwtRequest(method, "/", "Bearer "+tok))
		if cookie(rec, SessionCookieName) != nil {
			t.Errorf("%s / started a session", method)
		}
	}
}

// A request another site makes (an image, a script, a fetch) carries the
// proxy's header too and starts nothing; the dashboard has no store, so an
// attempt would panic. A navigation, or a request with no Sec-Fetch headers, is
// not turned away by this rule.
func TestJWTCrossSiteSubrequestStartsNothing(t *testing.T) {
	j := newJWTHarness(t, nil)
	tok := "Bearer " + j.token(nil)
	for _, mode := range []string{"no-cors", "cors", "same-origin", ""} {
		for name, h := range map[string]http.Handler{"/api/me": j.h, "a page": j.pages} {
			target := "/api/me"
			if name == "a page" {
				target = "/repos"
			}
			req := jwtRequest("GET", target, tok)
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			if mode != "" {
				req.Header.Set("Sec-Fetch-Mode", mode)
			}
			rec := j.serve(h, req)
			if cookie(rec, SessionCookieName) != nil {
				t.Errorf("a cross-site %q request for %s started a session", mode, name)
			}
			if name == "/api/me" && rec.Code != http.StatusUnauthorized {
				t.Errorf("GET /api/me cross-site (%q) = %d, want 401", mode, rec.Code)
			}
		}
	}
	for _, c := range []struct {
		site, mode string
		want       bool
	}{
		{"cross-site", "navigate", false}, {"cross-site", "cors", true}, {"cross-site", "", true},
		{"same-site", "cors", false}, {"same-origin", "no-cors", false}, {"none", "navigate", false}, {"", "", false},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Sec-Fetch-Site", c.site)
		req.Header.Set("Sec-Fetch-Mode", c.mode)
		if got := crossSiteSubrequest(req); got != c.want {
			t.Errorf("crossSiteSubrequest(%q, %q) = %v, want %v", c.site, c.mode, got, c.want)
		}
	}
}

// A local admin whose proxy identity is allowed must reach the password form
// through the proxy: loading /admin/sign-in and the page's quiet read of
// /api/me?probe=1 start no session, whatever the header holds (the dashboard
// here has no store: a start would panic). Without the probe, /api/me still
// starts one.
func TestJWTAdminSignInStartsNothing(t *testing.T) {
	j := newJWTHarness(t, nil)
	tok := "Bearer " + j.token(nil)
	for _, target := range []string{"/admin/sign-in", "/admin/sign-in/", "/Admin/Sign-In"} {
		rec := j.serve(j.pages, jwtRequest("GET", target, tok))
		if rec.Code != http.StatusOK || cookie(rec, SessionCookieName) != nil {
			t.Errorf("GET %s with a valid header = %d, cookie %v; want the page and no session", target, rec.Code, cookie(rec, SessionCookieName))
		}
	}
	for _, target := range []string{"/api/me?probe=1", "/api/me?x=1&probe=1"} {
		rec := j.serve(j.h, jwtRequest("GET", target, tok))
		if rec.Code != http.StatusUnauthorized || cookie(rec, SessionCookieName) != nil {
			t.Errorf("GET %s with a valid header = %d, cookie %v; want 401 and no session", target, rec.Code, cookie(rec, SessionCookieName))
		}
	}
}

// When the settings cannot be read, a request with no session gets what it
// would get on an install that does not use a header: 401 on /api/me and the
// page as it is (the dashboard here has no store: a start would panic). A
// session that jwt started is not ended, and not kept: 503 (jwtSessionHolds).
func TestJWTSettingsReadFailure(t *testing.T) {
	j := newJWTHarness(t, nil)
	tok := "Bearer " + j.token(nil)
	j.d.Auth.settings = func(context.Context) (signIn, error) { return signIn{}, errors.New("database is down") }
	rec := j.serve(j.h, jwtRequest("GET", "/api/me", tok))
	if rec.Code != http.StatusUnauthorized || cookie(rec, SessionCookieName) != nil {
		t.Errorf("GET /api/me with the settings unreadable = %d %s; want 401", rec.Code, rec.Body)
	}
	rec = j.serve(j.pages, jwtRequest("GET", "/", tok))
	if rec.Code != http.StatusOK || cookie(rec, SessionCookieName) != nil || rec.Header().Get("Cache-Control") != "" {
		t.Errorf("GET / with the settings unreadable = %d, cookie %v; want the page", rec.Code, cookie(rec, SessionCookieName))
	}
	holds, st := j.d.Auth.jwtSessionHolds(jwtRequest("GET", "/api/me", tok), store.Session{Issuer: j.f.url()}, "alice@example.com")
	if holds || st != headerUnavailable {
		t.Errorf("jwtSessionHolds with the settings unreadable = %v, %d; want false, headerUnavailable (503, nothing ended)", holds, st)
	}
}

// A token that cannot pass is turned away from its own claims, before the key
// set is asked for anything: one with an unknown key id would otherwise cost a
// fetch of the key set on each request.
func TestJWTTurnsAwayMisfitsBeforeFetchingKeys(t *testing.T) {
	j := newJWTHarness(t, nil)
	j.f.signWith = newFakeIssuer(t).key // no key of the issuer's signed it
	for name, edit := range map[string]func(c map[string]any){
		"other issuer":   func(c map[string]any) { c["iss"] = "https://other.example.com" },
		"other audience": func(c map[string]any) { c["aud"] = "another-app" },
		"audience list":  func(c map[string]any) { c["aud"] = []string{"a", "b"} },
		"expired":        func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"no expiry":      func(c map[string]any) { delete(c, "exp") },
		"no audience":    func(c map[string]any) { delete(c, "aud") },
	} {
		email, st := j.read(j.token(edit))
		wantHeader(t, name, email, st, "", headerInvalid)
	}
	if n := j.f.jwksHits.Load(); n != 0 {
		t.Errorf("the key set was fetched %d times for tokens that could not pass", n)
	}
	// One that fits goes to the keys (and here fails the signature).
	email, st := j.read(j.token(nil))
	wantHeader(t, "fits, wrong key", email, st, "", headerInvalid)
	if j.f.jwksHits.Load() == 0 {
		t.Error("a token that fits was not checked against the keys")
	}
}

func TestClaimsFit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	tok := func(payload string) string { return seg(`{"alg":"RS256"}`) + "." + seg(payload) + ".sig" }
	good := `{"iss":"https://i","aud":"a","exp":1800000100}`
	for name, c := range map[string]struct {
		raw  string
		want bool
	}{
		"fits":               {tok(good), true},
		"audience in a list": {tok(`{"iss":"https://i","aud":["x","a"],"exp":1800000100}`), true},
		"expired":            {tok(`{"iss":"https://i","aud":"a","exp":1799999999}`), false},
		"other issuer":       {tok(`{"iss":"https://j","aud":"a","exp":1800000100}`), false},
		"other audience":     {tok(`{"iss":"https://i","aud":"b","exp":1800000100}`), false},
		"audience a number":  {tok(`{"iss":"https://i","aud":7,"exp":1800000100}`), false},
		"no expiry":          {tok(`{"iss":"https://i","aud":"a"}`), false},
		"payload not JSON":   {tok(`nope`), false},
		"payload not base64": {seg(`{}`) + ".%%%." + "sig", false},
		"two parts":          {seg(`{}`) + "." + seg(good), false},
		"empty":              {"", false},
	} {
		if got := claimsFit(c.raw, "https://i", "a", now); got != c.want {
			t.Errorf("%s: claimsFit = %v, want %v", name, got, c.want)
		}
	}
}

// /signed-out starts nothing, even with a valid header, and neither do the
// paths that are not pages.
func TestSignOutShowsNoRestart(t *testing.T) {
	j := newJWTHarness(t, nil)
	tok := "Bearer " + j.token(nil)
	for _, p := range []string{"/signed-out", "/signed-out/", "/signed-out?x=1", "/assets/index-abc.js", "/favicon.svg", "/webhooks/gitlab/main",
		"/runs/1/result", "/healthz", "/auth/oidc/login", "/api/overview", "/api/anything", "/skill-fetches/1/result"} {
		before := j.pageHits.Load()
		rec := j.serve(j.pages, jwtRequest("GET", p, tok))
		if cookie(rec, SessionCookieName) != nil {
			t.Errorf("GET %s started a session", p)
		}
		if j.pageHits.Load() != before+1 {
			t.Errorf("GET %s did not reach the page handler", p)
		}
	}
	// Signing out in JWT mode does not need the header; it answers 204 and
	// clears the cookie (the web then goes to /signed-out).
	rec := j.serve(j.h, jwtRequest("POST", "/auth/logout", tok))
	if rec.Code != http.StatusNoContent {
		t.Errorf("POST /auth/logout = %d; want 204", rec.Code)
	}
}

func TestIsPagePath(t *testing.T) {
	for p, want := range map[string]bool{
		"/": true, "/repos": true, "/repos/acme/widgets.io": true, "/admin": true, "/signin": true, "/sign-in": true,
		"/signed-out": false, "/signed-out/": false, "/assets/index-abc.js": false, "/favicon.svg": false, "/index.html": false,
		"/api": false, "/api/me": false, "/auth/oidc/login": false, "/webhooks/gitlab/x": false, "/runs/1/result": false,
		"/healthz": false, "/skill-fetches/1/result": false, "/readiness-scans/1/result": false, "/a/../signed-out": false,
		"//signed-out": false, "/./signed-out": false, "/Signed-Out": false, "/SIGNED-OUT/": false,
		"/admin/sign-in": false, "/admin/sign-in/": false, "/Admin/Sign-In": false, "/admin/../admin/sign-in": false,
		"/admin/sign-in/x": true, "/admin/users": true,
	} {
		if got := isPagePath(p); got != want {
			t.Errorf("isPagePath(%q) = %v; want %v", p, got, want)
		}
	}
}

// --- with a database [CI] ---

// useJWT saves JWT settings in the dashboard against f (example.com
// allowed) and points the dashboard at f. edit may change them.
func (e *env) useJWT(f *fakeIssuer, edit func(s *store.SignInSettings)) {
	e.t.Helper()
	f.now = e.clock.now
	e.d.idp = idpClient(f.srv.Client())
	s := store.SignInSettings{Mode: store.SignInModeJWT, Issuer: f.url(), Audience: jwtAudience, JWKSURL: f.url() + "/jwks",
		Header: jwtHeader, AllowedDomains: []string{"example.com"}, UpdatedAt: e.clock.now()}
	if edit != nil {
		edit(&s)
	}
	if err := e.st.PutSignInSettings(ctx, s); err != nil {
		e.t.Fatal(err)
	}
}

// jwtDo serves a request with the proxy's header (a "Bearer " token when
// token is not empty) and the session cookie c.
func (e *env) jwtDo(method, target, token string, c *http.Cookie, header map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	h := map[string]string{}
	for k, v := range header {
		h[k] = v
	}
	if token != "" {
		h[jwtHeader] = token
	}
	var cookies []*http.Cookie
	if c != nil {
		cookies = []*http.Cookie{c}
	}
	return e.do(method, target, cookies, h)
}

func (e *env) jwtToken(f *fakeIssuer, email string) string {
	return jwtToken(f, e.clock.now(), func(c map[string]any) { c["email"] = email })
}

// jwtSession starts a session for email by GET /api/me and returns its
// cookie.
func (e *env) jwtSession(f *fakeIssuer, email string) *http.Cookie {
	e.t.Helper()
	rec := e.jwtDo("GET", "/api/me", "Bearer "+e.jwtToken(f, email), nil, nil)
	c := cookie(rec, SessionCookieName)
	if rec.Code != http.StatusOK || c == nil {
		e.t.Fatalf("GET /api/me with a valid header = %d %s, cookie %v; want 200 and a session", rec.Code, rec.Body, c)
	}
	return c
}

// A GET of /api/me with a valid token and no session starts one: an SSO
// account, a session that says jwt with the issuer, the 24 hour cookie and an
// audit entry.
func TestJWTStartsASessionOnGET(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	rec := e.jwtDo("GET", "/api/me", "Bearer "+e.jwtToken(f, "Alice@Example.com"), nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me = %d %s; want 200", rec.Code, rec.Body)
	}
	c := cookie(rec, SessionCookieName)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != int(SSOMaxSessionAge/time.Second) {
		t.Fatalf("session cookie = %+v; want __Host-, HttpOnly, Secure, Lax, 24 hours", c)
	}
	var me meBody
	decodeInto(t, rec, &me)
	if me.Kind != "sso" || me.Email != "alice@example.com" || me.SignIn != "jwt" || me.Role != "user" || me.Admin || me.CSRFToken == "" {
		t.Errorf("/api/me = %+v", me)
	}
	accounts := e.ssoAccounts()
	if len(accounts) != 1 || accounts[0].Email != "alice@example.com" || accounts[0].Added || accounts[0].Role != store.RoleUser {
		t.Fatalf("accounts = %+v; want alice, a plain user", accounts)
	}
	se, found := e.storedSession(c)
	if !found || se.SignIn != store.SignInJWT || se.Issuer != f.url() || se.AccountID != accounts[0].ID {
		t.Errorf("stored session = %+v, %v", se, found)
	}
	if !strings.Contains(strings.Join(e.audits(), "\n"), "alice@example.com sign_in") {
		t.Errorf("audit log = %q; want alice's sign_in", e.audits())
	}
	// Later requests use the cookie, and the header keeps being checked.
	for range 3 {
		rec := e.jwtDo("GET", "/api/me", "Bearer "+e.jwtToken(f, "alice@example.com"), c, nil)
		if rec.Code != http.StatusOK || cookie(rec, SessionCookieName) != nil {
			t.Fatalf("GET /api/me with the cookie = %d, new cookie %v; want 200 and no new session", rec.Code, cookie(rec, SessionCookieName))
		}
	}
	if n := e.sessionCount(); n != 1 {
		t.Errorf("sessions = %d; want 1", n)
	}
	// A new user waits for a team, as with OIDC.
	if rec := e.jwtDo("GET", "/api/overview?days=7", "Bearer "+e.jwtToken(f, "alice@example.com"), c, nil); rec.Code != http.StatusForbidden {
		t.Errorf("GET /api/overview = %d %s; want 403 waiting", rec.Code, rec.Body)
	}
	// Neither the cookie nor a response nor the log holds the token.
	for _, r := range e.responses {
		if strings.Contains(r.Body.String(), "eyJ") {
			t.Errorf("a response shows a token: %s", r.Body)
		}
	}
	if strings.Contains(e.logs.String(), "eyJ") {
		t.Errorf("the log holds a token:\n%s", e.logs.String())
	}
}

// A page load starts one too (the proxy adds the header to it); a page with no
// header, or with a bad one, is still served and starts nothing.
func TestJWTStartsASessionOnAPage(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	hits := 0
	pages := e.d.Pages(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; _, _ = w.Write([]byte("page")) }))
	serve := func(target, token string, c *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		if token != "" {
			req.Header.Set(jwtHeader, token)
		}
		if c != nil {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		pages.ServeHTTP(rec, req)
		return rec
	}
	rec := serve("/repos/acme/widgets", e.jwtToken(f, "alice@example.com"), nil)
	c := cookie(rec, SessionCookieName)
	if rec.Code != http.StatusOK || rec.Body.String() != "page" || c == nil {
		t.Fatalf("GET page with a valid header = %d %q, cookie %v; want the page and a session", rec.Code, rec.Body, c)
	}
	// A response that sets the session cookie is not for a shared cache.
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control of a page that set a session = %q, want private, no-store", cc)
	}
	if se, found := e.storedSession(c); !found || se.SignIn != store.SignInJWT {
		t.Errorf("stored session = %+v, %v", se, found)
	}
	// With the session, no second one starts.
	if rec := serve("/", e.jwtToken(f, "alice@example.com"), c); cookie(rec, SessionCookieName) != nil || rec.Header().Get("Cache-Control") != "" {
		t.Error("a page request that has a session started another")
	}
	// A session row past its limit is no session: the page starts a new one.
	e.clock.advance(SSOMaxSessionAge + time.Minute)
	f.now = e.clock.now
	rec2 := serve("/", e.jwtToken(f, "alice@example.com"), c)
	if c2 := cookie(rec2, SessionCookieName); c2 == nil || c2.Value == c.Value {
		t.Errorf("a page request with an expired session row = cookie %v; want a new session", c2)
	}
	for name, token := range map[string]string{"no header": "", "bad header": "not-a-token", "other audience": jwtToken(f, e.clock.now(), func(c map[string]any) { c["aud"] = "x" })} {
		before := e.sessionCount()
		rec := serve("/", token, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "page" || cookie(rec, SessionCookieName) != nil || e.sessionCount() != before {
			t.Errorf("%s: GET / = %d, cookie %v, sessions %d -> %d; want the page and no session", name, rec.Code, cookie(rec, SessionCookieName), before, e.sessionCount())
		}
	}
	// /signed-out starts nothing even with a valid header.
	before := e.sessionCount()
	if rec := serve("/signed-out", e.jwtToken(f, "bob@example.com"), nil); cookie(rec, SessionCookieName) != nil || e.sessionCount() != before {
		t.Error("/signed-out started a session")
	}
	if hits == 0 {
		t.Error("the page handler was never reached")
	}
}

// A local admin's password session is not touched by a valid header for
// someone else, on a page or on /api/me: no session starts, and the account is
// still the admin's.
func TestPasswordSessionWithAValidHeader(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	admin := e.operator()
	before := e.sessionCount()
	pages := e.d.Pages(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("page")) }))
	req := httptest.NewRequest("GET", "/repos", nil)
	req.Header.Set(jwtHeader, "Bearer "+e.jwtToken(f, "bob@example.com"))
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	pages.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || cookie(rec, SessionCookieName) != nil || e.sessionCount() != before {
		t.Errorf("a page with a password session and bob's header = %d, cookie %v, sessions %d -> %d; want the page and nothing new",
			rec.Code, cookie(rec, SessionCookieName), before, e.sessionCount())
	}
	rec = e.jwtDo("GET", "/api/me", "Bearer "+e.jwtToken(f, "bob@example.com"), admin, nil)
	var me meBody
	decodeInto(t, rec, &me)
	if rec.Code != http.StatusOK || me.Kind != "password" || me.Username != "ops" || cookie(rec, SessionCookieName) != nil || e.sessionCount() != before {
		t.Errorf("GET /api/me with a password session and bob's header = %d %+v, sessions %d; want ops", rec.Code, me, e.sessionCount())
	}
}

// Both forms of the header sign in.
func TestJWTBearerAndBareTokenStartSessions(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	for token, email := range map[string]string{
		"Bearer " + e.jwtToken(f, "alice@example.com"): "alice@example.com",
		e.jwtToken(f, "bob@example.com"):               "bob@example.com",
	} {
		rec := e.jwtDo("GET", "/api/me", token, nil, nil)
		var me meBody
		decodeInto(t, rec, &me)
		if rec.Code != http.StatusOK || me.Email != email {
			t.Errorf("GET /api/me = %d, email %q; want 200, %q", rec.Code, me.Email, email)
		}
	}
}

// A valid header and no cookie on a write: refused, and nothing changes.
func TestJWTPostLeavesTheDatabaseAlone(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	tok := "Bearer " + e.jwtToken(f, "alice@example.com")
	for _, c := range []struct{ method, target string }{
		{"POST", "/test/session"}, {"PUT", "/test/session"}, {"DELETE", "/test/session"}, {"POST", "/api/me"}, {"POST", "/auth/password/change"},
	} {
		rec := e.jwtDo(c.method, c.target, tok, nil, nil)
		if rec.Code < 400 || cookie(rec, SessionCookieName) != nil {
			t.Errorf("%s %s with a valid header and no session = %d, cookie %v; want a refusal", c.method, c.target, rec.Code, cookie(rec, SessionCookieName))
		}
	}
	if n := e.sessionCount(); n != 0 {
		t.Errorf("sessions = %d; want 0", n)
	}
	if got := e.ssoAccounts(); len(got) != 0 {
		t.Errorf("accounts = %+v; want none", got)
	}
	if got := e.audits(); len(got) != 0 {
		t.Errorf("audit log = %q; want nothing", got)
	}
	// With a stale cookie (no such session) a write still starts nothing.
	rec := e.jwtDo("POST", "/test/session", tok, &http.Cookie{Name: SessionCookieName, Value: randomString(sessionIDBytes)}, nil)
	if rec.Code != http.StatusUnauthorized || e.sessionCount() != 0 {
		t.Errorf("POST with a stale cookie = %d, sessions %d; want 401 and none", rec.Code, e.sessionCount())
	}
}

// With a session, a write needs its CSRF token even when the header is valid.
func TestJWTWriteNeedsCSRF(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	c := e.jwtSession(f, "alice@example.com")
	se, _ := e.storedSession(c)
	tok := "Bearer " + e.jwtToken(f, "alice@example.com")
	for name, header := range map[string]map[string]string{"none": nil, "wrong": {CSRFHeader: "nope"}} {
		rec := e.jwtDo("POST", "/test/session", tok, c, header)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"csrf"`) {
			t.Errorf("POST with %s CSRF token = %d %s; want 403 csrf", name, rec.Code, rec.Body)
		}
	}
	if rec := e.jwtDo("POST", "/test/session", tok, c, map[string]string{CSRFHeader: se.CSRF}); rec.Code != http.StatusOK {
		t.Errorf("POST with the CSRF token = %d %s; want 200", rec.Code, rec.Body)
	}
	// The token alone is not enough either: the header must be valid.
	if rec := e.jwtDo("POST", "/test/session", "", c, map[string]string{CSRFHeader: se.CSRF}); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST with the CSRF token and no header = %d; want 401", rec.Code)
	}
}

// A different person's token ends the session, and it can't be used again.
func TestJWTEmailChangeEndsSession(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	c := e.jwtSession(f, "alice@example.com")
	rec := e.jwtDo("GET", "/test/session", "Bearer "+e.jwtToken(f, "bob@example.com"), c, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("alice's cookie with bob's token = %d %s; want 401", rec.Code, rec.Body)
	}
	if got := cookie(rec, SessionCookieName); got == nil || got.MaxAge >= 0 {
		t.Errorf("the cookie was not cleared: %+v", got)
	}
	if _, found := e.storedSession(c); found {
		t.Error("the session survived another person's token")
	}
	if rec := e.jwtDo("GET", "/test/session", "Bearer "+e.jwtToken(f, "alice@example.com"), c, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the ended session answered %d with alice's own token; want 401", rec.Code)
	}
	// The email is compared as an address: only case differs.
	c = e.jwtSession(f, "alice@example.com")
	if rec := e.jwtDo("GET", "/test/session", "Bearer "+e.jwtToken(f, "ALICE@EXAMPLE.COM"), c, nil); rec.Code != http.StatusOK {
		t.Errorf("same email in capitals = %d; want 200", rec.Code)
	}
}

// No header, or an invalid one, ends the session at once.
func TestJWTMissingHeaderEndsSession(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	good := func() string { return "Bearer " + e.jwtToken(f, "alice@example.com") }
	for name, token := range map[string]string{
		"no header":      "",
		"garbage":        "Bearer garbage",
		"other audience": jwtToken(f, e.clock.now(), func(c map[string]any) { c["aud"] = "x" }),
		"expired":        jwtToken(f, e.clock.now(), func(c map[string]any) { c["exp"] = e.clock.now().Add(-time.Minute).Unix() }),
		"email missing":  jwtToken(f, e.clock.now(), func(c map[string]any) { delete(c, "email") }),
	} {
		c := e.jwtSession(f, "alice@example.com")
		if rec := e.jwtDo("GET", "/test/session", token, c, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s; want 401", name, rec.Code, rec.Body)
		}
		if _, found := e.storedSession(c); found {
			t.Errorf("%s: the session survived", name)
		}
		if rec := e.jwtDo("GET", "/test/session", good(), c, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: the ended session answered %d afterwards; want 401", name, rec.Code)
		}
	}
}

// An outage of the key set ends nothing: requests get 503, and the session
// is there when the keys come back.
func TestJWTKeyOutageKeepsTheSession(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	c := e.jwtSession(f, "alice@example.com")
	e.d.Auth.forgetJWTVerifier() // nothing cached
	f.setJWKSDown(true)
	for _, path := range []string{"/test/session", "/api/me"} {
		rec := e.jwtDo("GET", path, "Bearer "+e.jwtToken(f, "alice@example.com"), c, nil)
		if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "JWKS-BODY") {
			t.Errorf("GET %s with the keys down = %d %s; want 503", path, rec.Code, rec.Body)
		}
		if got := cookie(rec, SessionCookieName); got != nil {
			t.Errorf("GET %s with the keys down touched the cookie: %+v", path, got)
		}
	}
	if rec := e.jwtDo("POST", "/test/session", "Bearer "+e.jwtToken(f, "alice@example.com"), c, nil); rec.Code == http.StatusOK {
		t.Error("a write passed while the keys were down")
	}
	if _, found := e.storedSession(c); !found {
		t.Fatal("the outage ended the session")
	}
	// No session and the keys down: 503 too, and nothing starts.
	if rec := e.jwtDo("GET", "/api/me", "Bearer "+e.jwtToken(f, "bob@example.com"), nil, nil); rec.Code != http.StatusServiceUnavailable || e.sessionCount() != 1 {
		t.Errorf("a new GET /api/me with the keys down = %d, sessions %d; want 503 and 1", rec.Code, e.sessionCount())
	}
	f.setJWKSDown(false)
	if rec := e.jwtDo("GET", "/test/session", "Bearer "+e.jwtToken(f, "alice@example.com"), c, nil); rec.Code != http.StatusOK {
		t.Errorf("after the outage: %d; want 200", rec.Code)
	}
	if strings.Contains(e.logs.String(), "JWKS-BODY-MUST-NOT-LEAK") || strings.Contains(e.logs.String(), "eyJ") {
		t.Errorf("the log holds a response body or a token:\n%s", e.logs.String())
	}
}

// A local admin's session has no token: it skips all of this, whatever the
// header says, and the password sign-in works with no header.
func TestPasswordSessionSkipsTheHeader(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	admin := e.operator() // /auth/password, no header
	for name, token := range map[string]string{
		"no header": "", "garbage": "Bearer garbage", "bob's token": "Bearer " + e.jwtToken(f, "bob@example.com"),
	} {
		if rec := e.jwtDo("GET", "/test/session", token, admin, nil); rec.Code != http.StatusOK {
			t.Errorf("local admin with %s = %d %s; want 200", name, rec.Code, rec.Body)
		}
	}
	f.setJWKSDown(true)
	e.d.Auth.forgetJWTVerifier()
	if rec := e.jwtDo("GET", "/test/session", "Bearer "+e.jwtToken(f, "bob@example.com"), admin, nil); rec.Code != http.StatusOK {
		t.Errorf("local admin with the keys down = %d; want 200", rec.Code)
	}
	// A valid header does not turn it into an SSO session or replace it.
	if n := len(e.ssoAccounts()); n != 0 {
		t.Errorf("SSO accounts = %d; want 0", n)
	}
}

// A token that passes but whose email is not allowed makes no account and no
// session; an admin-added email passes whatever the lists say.
func TestJWTRefusedAddsNothing(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	rec := e.jwtDo("GET", "/api/me", "Bearer "+e.jwtToken(f, "mallory@other.org"), nil, nil)
	if rec.Code != http.StatusUnauthorized || cookie(rec, SessionCookieName) != nil || !strings.Contains(rec.Body.String(), `"not_allowed"`) {
		t.Errorf("GET /api/me for an email not on the lists = %d %s, cookie %v; want 401 not_allowed", rec.Code, rec.Body, cookie(rec, SessionCookieName))
	}
	if n := len(e.ssoAccounts()); n != 0 || e.sessionCount() != 0 || len(e.audits()) != 0 {
		t.Errorf("a refusal left %d accounts, %d sessions, audit %q", n, e.sessionCount(), e.audits())
	}
	// Both lists empty: only added emails.
	e.useJWT(f, func(s *store.SignInSettings) { s.AllowedDomains = nil })
	if rec := e.jwtDo("GET", "/api/me", "Bearer "+e.jwtToken(f, "alice@example.com"), nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("empty allowlist = %d; want 401", rec.Code)
	}
	if _, err := e.st.AddSSOAccount(ctx, "alice@example.com", e.clock.now()); err != nil {
		t.Fatal(err)
	}
	if rec := e.jwtDo("GET", "/api/me", "Bearer "+e.jwtToken(f, "alice@example.com"), nil, nil); rec.Code != http.StatusOK {
		t.Errorf("an added email = %d %s; want 200", rec.Code, rec.Body)
	}
}

// Changing the mode in the dashboard ends the sessions that jwt started, and
// leaves a local admin's.
func TestModeOffEndsJWTSessions(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	admin := e.operator()
	c := e.jwtSession(f, "alice@example.com")
	rec := e.putSignIn(admin, map[string]any{"mode": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/admin/sign-in = %d %s", rec.Code, rec.Body)
	}
	if _, found := e.storedSession(c); found {
		t.Error("the jwt session survived the mode being turned off")
	}
	if code, _ := e.session(c); code != http.StatusUnauthorized {
		t.Errorf("the ended session answered %d; want 401", code)
	}
	if code, _ := e.session(admin); code != http.StatusOK {
		t.Errorf("the local admin's session = %d; want 200", code)
	}
	// The header means nothing now: no session starts.
	if rec := e.jwtDo("GET", "/api/me", "Bearer "+e.jwtToken(f, "alice@example.com"), nil, nil); rec.Code != http.StatusUnauthorized || cookie(rec, SessionCookieName) != nil {
		t.Errorf("GET /api/me with mode off = %d, cookie %v; want 401 and none", rec.Code, cookie(rec, SessionCookieName))
	}
}

// If the mode (or the issuer) changed without the sessions being ended, as a
// change in the config file does, Require refuses a jwt session all the same:
// it must not live on as a plain cookie.
func TestRequireRefusesAJWTSessionWhenTheModeChanged(t *testing.T) {
	for name, change := range map[string]func(s *store.SignInSettings){
		"mode off": func(s *store.SignInSettings) { *s = store.SignInSettings{UpdatedAt: s.UpdatedAt} },
		"mode oidc": func(s *store.SignInSettings) {
			s.Mode, s.Issuer, s.ClientID = store.SignInModeOIDC, "https://idp.example.com", "c"
		},
		"issuer change": func(s *store.SignInSettings) { s.Issuer = "https://other.example.com" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			f := newFakeIssuer(t)
			e.useJWT(f, nil)
			c := e.jwtSession(f, "alice@example.com")
			e.useJWT(f, change) // PutSignInSettings ends no session
			if _, found := e.storedSession(c); !found {
				t.Fatal("the test setting ended the session itself")
			}
			if rec := e.jwtDo("GET", "/test/session", "Bearer "+e.jwtToken(f, "alice@example.com"), c, nil); rec.Code != http.StatusUnauthorized {
				t.Errorf("a jwt session after the sign-in settings changed = %d %s; want 401", rec.Code, rec.Body)
			}
			if _, found := e.storedSession(c); found {
				t.Error("the session was not deleted")
			}
		})
	}
}

// Signing out ends the session; the header can't start another on /signed-out.
func TestJWTSignOut(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useJWT(f, nil)
	c := e.jwtSession(f, "alice@example.com")
	se, _ := e.storedSession(c)
	rec := e.jwtDo("POST", "/auth/logout", "Bearer "+e.jwtToken(f, "alice@example.com"), c, map[string]string{CSRFHeader: se.CSRF})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /auth/logout = %d %s; want 204", rec.Code, rec.Body)
	}
	if _, found := e.storedSession(c); found {
		t.Error("the session survived signing out")
	}
	// Signing out with no session and a valid header starts none.
	rec = e.jwtDo("POST", "/auth/logout", "Bearer "+e.jwtToken(f, "alice@example.com"), nil, nil)
	if rec.Code != http.StatusNoContent || e.sessionCount() != 0 {
		t.Errorf("POST /auth/logout with no session = %d, sessions %d; want 204 and none", rec.Code, e.sessionCount())
	}
}
