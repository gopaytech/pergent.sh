package dashboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// checkCookie asserts the attributes every pergent cookie has.
// Both are __Host- cookies: Secure, Path=/ and no Domain, so no other host
// (a sibling subdomain) can set or shadow them.
func checkCookie(t *testing.T, c *http.Cookie, name string) {
	t.Helper()
	if c.Name != name || !strings.HasPrefix(c.Name, "__Host-") {
		t.Errorf("cookie name %q, want %q (a __Host- cookie)", c.Name, name)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Errorf("cookie %s: HttpOnly %v Secure %v SameSite %v Path %q Domain %q; want HttpOnly, Secure, Lax, /, no Domain",
			c.Name, c.HttpOnly, c.Secure, c.SameSite, c.Path, c.Domain)
	}
	if strings.Contains(c.String(), "Domain=") {
		t.Errorf("cookie %s is sent with a Domain: %s", c.Name, c.String())
	}
}

// s256 is the PKCE S256 challenge of a verifier.
func s256(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func TestGitLabLoginCreatesSession(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	e.aliceOn("main", store.RoleAdmin)

	loc, sc := e.startLogin("gitlab", "main")
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != e.gl.srv.URL+"/oauth/authorize" {
		t.Fatalf("authorize URL = %s", loc)
	}
	q := loc.Query()
	for k, want := range map[string]string{
		"client_id": glClientID, "redirect_uri": dashboardURL + "/auth/gitlab/main/callback",
		"response_type": "code", "scope": "read_user read_api", "code_challenge_method": "S256",
	} {
		if q.Get(k) != want {
			t.Errorf("authorize %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if len(q.Get("state")) < 40 || q.Get("code_challenge") == "" {
		t.Errorf("state %q, code_challenge %q", q.Get("state"), q.Get("code_challenge"))
	}
	checkCookie(t, sc, "__Host-pergent_oauth_state")
	if sc.MaxAge <= 0 || sc.MaxAge > 600 {
		t.Errorf("state cookie MaxAge = %d, want at most 10 minutes", sc.MaxAge)
	}
	if strings.Contains(sc.Value, glClientSecret) {
		t.Error("state cookie holds the client secret")
	}

	rec := e.callback("gitlab", "main", goodCode, q.Get("state"), sc)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if v := e.gl.lastVerifier(); v == "" || s256(v) != q.Get("code_challenge") {
		t.Errorf("code_verifier %q does not match the challenge", v)
	}
	if cleared := cookie(rec, stateCookieName); cleared == nil || cleared.MaxAge >= 0 {
		t.Error("callback did not clear the state cookie")
	} else {
		checkCookie(t, cleared, "__Host-pergent_oauth_state")
	}
	c := cookie(rec, SessionCookieName)
	if c == nil {
		t.Fatal("no session cookie")
	}
	checkCookie(t, c, "__Host-pergent_session")
	if id, err := base64.RawURLEncoding.DecodeString(c.Value); err != nil || len(id) != 32 {
		t.Fatalf("session ID %q is not 32 random bytes", c.Value)
	}

	// Stored: the hash of the ID, never the ID; the token sealed.
	se, found := e.storedSession(c)
	if !found {
		t.Fatal("no session stored under the ID's SHA-256")
	}
	if _, found, _ := e.st.Session(ctx, []byte(c.Value)); found {
		t.Fatal("session stored under the raw ID")
	}
	if se.Platform != "gitlab" || se.Instance != "main" || se.UserID != 9 || se.Username != "alice" || se.AccessScope == "" ||
		se.AccessCheckedAt == nil || !se.AccessCheckedAt.Equal(e.clock.now()) {
		t.Errorf("stored session = %+v", se)
	}
	tok := e.gl.tokens()[0]
	if bytes.Contains(se.TokenSealed, []byte(tok)) || bytes.Contains(se.TokenSealed, []byte("access")) {
		t.Fatal("the token is stored in the clear")
	}
	box, _ := secretbox.New(e.key)
	if plain, err := box.Open(se.TokenSealed); err != nil || !bytes.Contains(plain, []byte(tok)) {
		t.Fatalf("sealed token does not open to the token: %v", err)
	}

	code, me := e.me(c)
	if code != 200 || me.Platform != "gitlab" || me.Instance != "main" || me.Username != "alice" || !me.Admin || me.Kind != "platform" || me.CSRFToken != se.CSRF || len(me.CSRFToken) < 40 {
		t.Fatalf("/api/me = %d %+v", code, me)
	}
	for _, r := range e.responses {
		if strings.Contains(r.Body.String(), tok) || strings.Contains(r.Header().Get("Set-Cookie"), tok) {
			t.Fatalf("a response holds the token: %d %s", r.Code, r.Body)
		}
	}
}

func TestGitHubLoginCreatesSession(t *testing.T) {
	e := newEnv(t, nil)
	e.gh.setRepo("acme/widgets", githubRepo(true, true))
	e.aliceOn("gh", store.RoleAdmin)

	loc, sc := e.startLogin("github", "gh")
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != e.gh.srv.URL+"/login/oauth/authorize" {
		t.Fatalf("authorize URL = %s", loc)
	}
	q := loc.Query()
	if q.Get("client_id") != ghClientID || q.Get("redirect_uri") != dashboardURL+"/auth/github/gh/callback" ||
		q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
		t.Errorf("authorize query = %v", q)
	}
	rec := e.callback("github", "gh", goodCode, q.Get("state"), sc)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body)
	}
	if v := e.gh.lastVerifier(); s256(v) != q.Get("code_challenge") {
		t.Errorf("code_verifier %q does not match the challenge", v)
	}
	c := cookie(rec, SessionCookieName)
	checkCookie(t, c, "__Host-pergent_session")
	code, me := e.me(c)
	if code != 200 || me.Platform != "github" || me.Instance != "gh" || me.Username != "alice" || !me.Admin {
		t.Fatalf("/api/me = %d %+v", code, me)
	}
	_, se := e.session(c)
	if se.UserID != 7 || !se.CanSee("gh", "acme/widgets") || !se.CanSee("gh", "Acme/Widgets") || len(se.Visible) != 1 {
		t.Fatalf("session = %+v", se)
	}
}

func TestStateMismatchRejected(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))

	tamper := func(v string, i int) string {
		b := []byte(v)
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
		return string(b)
	}
	tests := []struct {
		name string
		call func() *http.Response
	}{
		{"state differs from the cookie", func() *http.Response {
			_, sc := e.startLogin("gitlab", "main")
			loc2, _ := e.startLogin("gitlab", "main")
			return e.callback("gitlab", "main", goodCode, loc2.Query().Get("state"), sc).Result()
		}},
		{"no state cookie", func() *http.Response {
			loc, _ := e.startLogin("gitlab", "main")
			return e.callback("gitlab", "main", goodCode, loc.Query().Get("state")).Result()
		}},
		{"empty state", func() *http.Response {
			_, sc := e.startLogin("gitlab", "main")
			return e.callback("gitlab", "main", goodCode, "", sc).Result()
		}},
		{"cookie payload tampered", func() *http.Response {
			// A well-formed payload with a later expiry, under the old
			// signature: only the signature check refuses it.
			loc, sc := e.startLogin("gitlab", "main")
			payload, sig, _ := strings.Cut(sc.Value, ".")
			raw, _ := base64.RawURLEncoding.DecodeString(payload)
			var st map[string]any
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatalf("state cookie payload: %v", err)
			}
			st["e"] = st["e"].(float64) + 3600
			raw, _ = json.Marshal(st)
			sc.Value = base64.RawURLEncoding.EncodeToString(raw) + "." + sig
			e.clock.advance(10*time.Minute + time.Second)
			return e.callback("gitlab", "main", goodCode, loc.Query().Get("state"), sc).Result()
		}},
		{"cookie payload garbled", func() *http.Response {
			loc, sc := e.startLogin("gitlab", "main")
			sc.Value = tamper(sc.Value, 3)
			return e.callback("gitlab", "main", goodCode, loc.Query().Get("state"), sc).Result()
		}},
		{"cookie signature tampered", func() *http.Response {
			loc, sc := e.startLogin("gitlab", "main")
			sc.Value = tamper(sc.Value, len(sc.Value)-2)
			return e.callback("gitlab", "main", goodCode, loc.Query().Get("state"), sc).Result()
		}},
		{"cookie of another instance", func() *http.Response {
			loc, sc := e.startLogin("github", "gh")
			return e.callback("gitlab", "main", goodCode, loc.Query().Get("state"), sc).Result()
		}},
		{"cookie older than 10 minutes", func() *http.Response {
			loc, sc := e.startLogin("gitlab", "main")
			e.clock.advance(10*time.Minute + time.Second)
			return e.callback("gitlab", "main", goodCode, loc.Query().Get("state"), sc).Result()
		}},
		{"cookie signed with another key", func() *http.Response {
			other := newEnv(t, nil)
			other.clock.t = e.clock.now()
			loc, sc := other.startLogin("gitlab", "main")
			return e.callback("gitlab", "main", goodCode, loc.Query().Get("state"), sc).Result()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := tt.call()
			if res.StatusCode != http.StatusBadRequest {
				t.Errorf("callback = %d, want 400", res.StatusCode)
			}
			for _, c := range res.Cookies() {
				if c.Name == SessionCookieName {
					t.Error("a session cookie was set")
				}
			}
		})
	}
	if n := e.gl.calls() + e.gh.calls(); n != 0 {
		t.Errorf("%d token requests were made for a bad state", n)
	}
}

// A provider's error (the user said no) makes no session and no token call.
func TestCallbackProviderErrorRejected(t *testing.T) {
	e := newEnv(t, nil)
	loc, sc := e.startLogin("gitlab", "main")
	rec := e.do("GET", "/auth/gitlab/main/callback?error=access_denied&error_description=%3Cscript%3E&state="+loc.Query().Get("state"),
		[]*http.Cookie{sc}, nil)
	if rec.Code != http.StatusForbidden || cookie(rec, SessionCookieName) != nil || e.gl.calls() != 0 {
		t.Fatalf("callback with error = %d, token calls %d", rec.Code, e.gl.calls())
	}
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Error("the provider's error description is echoed")
	}
}

// A code the provider refuses, or an access check that fails at sign-in,
// makes no session.
func TestFailedSignInMakesNoSession(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	loc, sc := e.startLogin("gitlab", "main")
	rec := e.callback("gitlab", "main", "bad-code", loc.Query().Get("state"), sc)
	if rec.Code != http.StatusBadGateway || cookie(rec, SessionCookieName) != nil {
		t.Fatalf("bad code: callback = %d", rec.Code)
	}

	// The token and /user work, but the access check of what alice's team
	// covers is refused.
	e.aliceOn("main", store.RoleViewer)
	e.gl.set(func(o *oauthServer) { o.failAccess = true })
	loc, sc = e.startLogin("gitlab", "main")
	rec = e.callback("gitlab", "main", goodCode, loc.Query().Get("state"), sc)
	if rec.Code != http.StatusBadGateway || cookie(rec, SessionCookieName) != nil {
		t.Fatalf("failed access check: callback = %d %s", rec.Code, rec.Body)
	}
	far := e.clock.now().Add(100 * 365 * 24 * time.Hour)
	if n, err := e.st.PruneSessions(ctx, far, far); err != nil || n != 0 {
		t.Fatalf("%d sessions stored after failed sign-ins (%v)", n, err)
	}
}

// Signing in always issues a new session ID, and the session the browser
// arrived with is ended: an ID planted before sign-in is worthless.
func TestSessionFixation(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))

	planted := e.signIn("gitlab", "main") // the attacker's own session
	victim := e.signIn("gitlab", "main", planted)
	if victim.Value == planted.Value {
		t.Fatal("sign-in kept the session ID the browser arrived with")
	}
	if code, _ := e.me(planted); code != http.StatusUnauthorized {
		t.Fatalf("the planted session still works: %d", code)
	}
	if code, _ := e.me(victim); code != 200 {
		t.Fatalf("the new session does not work: %d", code)
	}

	// A made-up ID is not adopted either.
	madeUp := &http.Cookie{Name: SessionCookieName, Value: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))}
	c := e.signIn("gitlab", "main", madeUp)
	if c.Value == madeUp.Value {
		t.Fatal("sign-in adopted a made-up session ID")
	}
}

// After sign-in the browser always goes to /: no parameter and no Host
// header can send it, or the provider's redirect, anywhere else.
func TestNoOpenRedirect(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))

	rec := e.do("GET", "/auth/gitlab/main/login?next=https://evil.example.com/&redirect_uri=https://evil.example.com/",
		nil, map[string]string{"Host": "evil.example.com", "X-Forwarded-Host": "evil.example.com"})
	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, e.gl.srv.URL+"/oauth/authorize?") || strings.Contains(loc, "evil") {
		t.Fatalf("login redirects to %s", loc)
	}
	sc := cookie(rec, stateCookieName)
	state := strings.SplitN(strings.SplitN(loc, "state=", 2)[1], "&", 2)[0]
	q := "code=" + goodCode + "&state=" + state + "&next=https%3A%2F%2Fevil.example.com%2F&redirect=%2F%2Fevil.example.com"
	rec = e.do("GET", "/auth/gitlab/main/callback?"+q, []*http.Cookie{sc}, map[string]string{"Host": "evil.example.com"})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d Location %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestUnknownSignInRoutes(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.GitLab = append(c.GitLab, config.Instance{Name: "nooauth", URL: "https://gitlab.example.com"})
	})
	for _, p := range []string{
		"/auth/gitlab/nope/login", "/auth/github/main/login", "/auth/gitlab/gh/callback",
		"/auth/gitlab/nooauth/login", "/auth/bitbucket/main/login",
	} {
		if rec := e.do("GET", p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}

func TestIdleSessionExpires(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	c := e.signIn("gitlab", "main")
	signedIn := e.clock.now()

	// last_seen_at moves at most once a minute.
	e.clock.advance(30 * time.Second)
	if code, _ := e.me(c); code != 200 {
		t.Fatalf("/api/me = %d", code)
	}
	if se, _ := e.storedSession(c); !se.LastSeenAt.Equal(signedIn) {
		t.Errorf("last_seen_at moved after 30 s: %s", se.LastSeenAt)
	}
	e.clock.advance(31 * time.Second)
	e.me(c)
	if se, _ := e.storedSession(c); !se.LastSeenAt.Equal(e.clock.now()) {
		t.Errorf("last_seen_at = %s after 61 s, want %s", se.LastSeenAt, e.clock.now())
	}

	// Used within 8 hours: still signed in.
	e.clock.advance(8 * time.Hour)
	if code, _ := e.me(c); code != 200 {
		t.Fatalf("/api/me after exactly 8 h idle = %d", code)
	}
	// Idle more than 8 hours: gone.
	e.clock.advance(8*time.Hour + time.Second)
	rec := e.do("GET", "/api/me", []*http.Cookie{c}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me after 8 h idle = %d", rec.Code)
	}
	if cl := cookie(rec, SessionCookieName); cl == nil || cl.MaxAge >= 0 {
		t.Error("the expired session's cookie was not cleared")
	}
	if _, found := e.storedSession(c); found {
		t.Error("the idle session is still stored")
	}

	// A session in use every few hours still ends after 7 days.
	c = e.signIn("gitlab", "main")
	for i := 0; i < 7*24/6; i++ {
		e.clock.advance(6 * time.Hour)
		if code, _ := e.me(c); code != 200 {
			t.Fatalf("/api/me at %d h = %d", (i+1)*6, code)
		}
	}
	e.clock.advance(6 * time.Hour)
	if code, _ := e.me(c); code != http.StatusUnauthorized {
		t.Fatalf("/api/me after 7 days = %d", code)
	}
}

// wantCSRFCode checks a CSRF refusal says so in a field the client can
// test: {"error": ..., "code": "csrf"}.
func wantCSRFCode(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body struct{ Error, Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "csrf" || body.Error == "" {
		t.Errorf("CSRF refusal body = %s; want an error and code \"csrf\"", rec.Body)
	}
}

func TestCSRFRequiredOnWrites(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	e.aliceOn("main", store.RoleViewer)
	c := e.signIn("gitlab", "main")
	_, me := e.me(c)

	for _, tt := range []struct {
		name, method, token string
		want                int
	}{
		{"GET needs none", "GET", "", 200},
		{"POST without", "POST", "", http.StatusForbidden},
		{"POST with a wrong token", "POST", "wrong", http.StatusForbidden},
		{"PUT with a near miss", "PUT", me.CSRFToken[:len(me.CSRFToken)-1], http.StatusForbidden},
		{"DELETE without", "DELETE", "", http.StatusForbidden},
		{"POST with the token", "POST", me.CSRFToken, 200},
		{"PATCH with the token", "PATCH", me.CSRFToken, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := map[string]string{}
			if tt.token != "" {
				h["X-CSRF-Token"] = tt.token
			}
			rec := e.do(tt.method, "/test/session", []*http.Cookie{c}, h)
			if rec.Code != tt.want {
				t.Errorf("%s = %d, want %d", tt.method, rec.Code, tt.want)
			}
			if tt.want == http.StatusForbidden {
				wantCSRFCode(t, rec)
			}
		})
	}

	// Another session's token is not this one's.
	other := e.signIn("gitlab", "main")
	_, otherMe := e.me(other)
	if rec := e.do("POST", "/test/session", []*http.Cookie{c}, map[string]string{"X-CSRF-Token": otherMe.CSRFToken}); rec.Code != http.StatusForbidden {
		t.Errorf("POST with another session's token = %d", rec.Code)
	}

	// Logout is a write too.
	if rec := e.do("POST", "/auth/logout", []*http.Cookie{c}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("logout without a CSRF token = %d", rec.Code)
	} else {
		wantCSRFCode(t, rec)
	}
	if code, _ := e.me(c); code != 200 {
		t.Fatal("logout without a CSRF token ended the session")
	}
	rec := e.do("POST", "/auth/logout", []*http.Cookie{c}, map[string]string{"X-CSRF-Token": me.CSRFToken})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", rec.Code)
	}
	if cl := cookie(rec, SessionCookieName); cl == nil || cl.MaxAge >= 0 {
		t.Error("logout did not clear the cookie")
	} else {
		checkCookie(t, cl, "__Host-pergent_session")
	}
	if _, found := e.storedSession(c); found {
		t.Error("logout left the session stored")
	}
	if code, _ := e.me(c); code != http.StatusUnauthorized {
		t.Errorf("/api/me after logout = %d", code)
	}
	// Signed out already: logout still answers 204.
	if rec := e.do("POST", "/auth/logout", nil, nil); rec.Code != http.StatusNoContent {
		t.Errorf("logout without a session = %d", rec.Code)
	}
}

func TestUnauthenticatedAPI(t *testing.T) {
	e := newEnv(t, nil)
	for _, cs := range [][]*http.Cookie{
		nil,
		{{Name: SessionCookieName, Value: "not-base64-!!"}},
		{{Name: SessionCookieName, Value: base64.RawURLEncoding.EncodeToString([]byte("short"))}},
		{{Name: SessionCookieName, Value: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))}},
	} {
		if rec := e.do("GET", "/api/me", cs, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("/api/me with %v = %d", cs, rec.Code)
		}
	}
}

// Every response the dashboard sends during sign-in, use, token refresh,
// failures and logout, on both platforms, is free of every OAuth token and
// client secret, and so is the log.
func TestNoTokenInAnyResponse(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	e.aliceOn("main", store.RoleAdmin)
	e.gh.setRepo("acme/widgets", githubRepo(true, true))
	e.aliceOn("gh", store.RoleAdmin)
	e.gh.set(func(o *oauthServer) { o.expiresIn = 600 })

	for _, p := range []struct{ platform, instance string }{{"gitlab", "main"}, {"github", "gh"}} {
		c := e.signIn(p.platform, p.instance)
		_, me := e.me(c)
		e.session(c)
		// Token and access refreshed (the GitHub token expires in 10 min).
		e.clock.advance(11 * time.Minute)
		e.session(c)
		// A refused code and a refused refresh.
		loc, sc := e.startLogin(p.platform, p.instance)
		e.callback(p.platform, p.instance, "bad-code", loc.Query().Get("state"), sc)
		e.gl.set(func(o *oauthServer) { o.failRefresh = true })
		e.gh.set(func(o *oauthServer) { o.failRefresh = true })
		e.clock.advance(3 * time.Hour)
		e.session(c)
		e.me(c)
		e.do("POST", "/test/session", []*http.Cookie{c}, map[string]string{"X-CSRF-Token": me.CSRFToken})
		e.do("POST", "/auth/logout", []*http.Cookie{c}, map[string]string{"X-CSRF-Token": me.CSRFToken})
		e.gl.set(func(o *oauthServer) { o.failRefresh = false })
		e.gh.set(func(o *oauthServer) { o.failRefresh = false })
	}

	secrets := append(append(e.gl.tokens(), e.gh.tokens()...), glClientSecret, ghClientSecret)
	if len(secrets) < 6 {
		t.Fatalf("only %d secrets issued: the flows did not run", len(secrets))
	}
	for _, r := range e.responses {
		var hdr strings.Builder
		_ = r.Header().Write(&hdr)
		for _, s := range secrets {
			if strings.Contains(r.Body.String(), s) || strings.Contains(hdr.String(), s) {
				t.Fatalf("a %d response holds %q:\n%s\n%s", r.Code, s, hdr.String(), r.Body)
			}
		}
	}
	logs := e.logs.String()
	for _, s := range secrets {
		if strings.Contains(logs, s) {
			t.Fatalf("the log holds %q:\n%s", s, logs)
		}
	}
	// "SECRET" ends every token and secret: a part of one is a leak too.
	if strings.Contains(logs, "SECRET") {
		t.Fatalf("the log holds part of a secret:\n%s", logs)
	}
}

func TestPruneSessions(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	old := e.signIn("gitlab", "main")
	idle := e.signIn("gitlab", "main")
	for i := 0; i < 7*24/6-1; i++ { // old stays in use, idle doesn't
		e.clock.advance(6 * time.Hour)
		e.me(old)
	}
	fresh := e.signIn("gitlab", "main")
	e.clock.advance(6*time.Hour + time.Second) // old is 7 days and 1 s old

	n, err := e.d.Auth.PruneSessions(ctx, e.clock.now())
	if err != nil || n != 2 {
		t.Fatalf("PruneSessions = %d, %v; want 2", n, err)
	}
	if _, found := e.storedSession(old); found {
		t.Error("the 7-day-old session was kept")
	}
	if _, found := e.storedSession(idle); found {
		t.Error("the idle session was kept")
	}
	if _, found := e.storedSession(fresh); !found {
		t.Error("the fresh session was pruned")
	}
}

func TestNewRejects(t *testing.T) {
	cfg := &config.Config{
		GitLab: []config.Instance{{Name: "main", URL: "https://gitlab.example.com",
			OAuthClientIDEnv: "PERGENT_TEST_GL_ID", OAuthClientSecretEnv: "PERGENT_TEST_GL_SECRET"}},
		Dashboard: config.Dashboard{URL: dashboardURL},
	}
	if _, err := New(Options{Config: cfg, SecretKey: "short"}); err == nil {
		t.Error("New with a bad secret key = nil error")
	}
	cfg.Dashboard.URL = ""
	if _, err := New(Options{Config: cfg, SecretKey: testKey(t)}); err == nil {
		t.Error("New without dashboard.url = nil error")
	}
}

// New reads no OAuth env var: sign-in starts when an instance is connected
// (SetProvider), and an instance without a client ID has none.
func TestSetProviderAndRemoveProvider(t *testing.T) {
	cfg := &config.Config{Dashboard: config.Dashboard{URL: dashboardURL}}
	d, err := New(Options{Config: cfg, SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	gl := instances.Instance{Name: "lab", Platform: config.PlatformGitLab, Source: instances.SourceDashboard,
		GitLab: &config.Instance{Name: "lab", URL: "https://lab.example.com/"}, OAuthClientID: "lab-id"}
	d.Auth.SetProvider(gl, instances.Secrets{OAuthClientSecret: "lab-secret"})
	p, ok := d.Auth.providers.Get("lab")
	if !ok || p.platform() != config.PlatformGitLab {
		t.Fatalf("provider = %v, %v", p, ok)
	}
	if u := p.authorizeURL("s", "c"); !strings.HasPrefix(u, "https://lab.example.com/oauth/authorize?") ||
		!strings.Contains(u, "client_id=lab-id") || !strings.Contains(u, url.QueryEscape(dashboardURL+"/auth/gitlab/lab/callback")) {
		t.Errorf("authorize URL = %s", u)
	}

	gh := instances.Instance{Name: "hub", Platform: config.PlatformGitHub, Source: instances.SourceDashboard,
		GitHub: &config.GitHubInstance{Name: "hub", APIURL: "https://api.github.com"}, OAuthClientID: "hub-id"}
	d.Auth.SetProvider(gh, instances.Secrets{OAuthClientSecret: "hub-secret"})
	if p, ok := d.Auth.providers.Get("hub"); !ok || p.platform() != config.PlatformGitHub {
		t.Errorf("github provider = %v, %v", p, ok)
	}

	// A client ID taken away ends the sign-in; so does RemoveProvider.
	gl.OAuthClientID = ""
	d.Auth.SetProvider(gl, instances.Secrets{})
	if _, ok := d.Auth.providers.Get("lab"); ok {
		t.Error("lab kept its provider without a client ID")
	}
	d.Auth.RemoveProvider("hub")
	d.Auth.RemoveProvider("never-there")
	if _, ok := d.Auth.providers.Get("hub"); ok {
		t.Error("hub kept its provider after RemoveProvider")
	}
}

func TestGitHubWebURL(t *testing.T) {
	for api, want := range map[string]string{
		"":                                    "https://github.com",
		"https://api.github.com":              "https://github.com",
		"https://github.example.com/api/v3":   "https://github.example.com",
		"https://github.example.com/api/v3/":  "https://github.example.com",
		"https://ghe.example.com:8443/api/v3": "https://ghe.example.com:8443",
	} {
		if got := githubWebURL(api); got != want {
			t.Errorf("githubWebURL(%q) = %q, want %q", api, got, want)
		}
	}
}

// A platform sign-in makes the user's account the first time and reuses it
// after, with the username the platform reports now; the session belongs to
// that account.
func TestPlatformSignInCreatesAccount(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))

	first := e.signIn("gitlab", "main")
	accounts, err := e.st.Accounts(ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts after the first sign-in = %+v, %v", accounts, err)
	}
	a := accounts[0]
	if a.Kind != store.AccountPlatform || a.Platform != "gitlab" || a.Instance != "main" || a.PlatformUserID != 9 ||
		a.Username != "alice" || a.LastSignInAt == nil || !a.LastSignInAt.Equal(e.clock.now()) {
		t.Fatalf("account = %+v", a)
	}
	if se, found := e.storedSession(first); !found || se.AccountID != a.ID {
		t.Fatalf("session = %+v, %v; want account %d", se, found, a.ID)
	}

	e.gl.setUsername("alice-renamed")
	e.clock.advance(time.Hour)
	second := e.signIn("gitlab", "main")
	accounts, _ = e.st.Accounts(ctx)
	if len(accounts) != 1 || accounts[0].ID != a.ID || accounts[0].Username != "alice-renamed" ||
		!accounts[0].LastSignInAt.Equal(e.clock.now()) {
		t.Fatalf("accounts after the second sign-in = %+v; want the same account, renamed", accounts)
	}
	if se, found := e.storedSession(second); !found || se.AccountID != a.ID {
		t.Fatalf("second session = %+v, %v", se, found)
	}

	// The same user on a GitHub instance is another account.
	e.signIn("github", "gh")
	if accounts, _ = e.st.Accounts(ctx); len(accounts) != 2 {
		t.Fatalf("accounts after a GitHub sign-in = %d; want 2", len(accounts))
	}
}
