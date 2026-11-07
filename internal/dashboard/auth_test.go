package dashboard

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
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

func TestIdleSessionExpires(t *testing.T) {
	e := newEnv(t, nil)
	c := e.aliceSession()
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

	// An SSO session in use every few hours still ends after 24 hours.
	sso := e.aliceSession()
	for i := 0; i < 3; i++ {
		e.clock.advance(6 * time.Hour)
		if code, _ := e.me(sso); code != 200 {
			t.Fatalf("SSO /api/me at %d h = %d", (i+1)*6, code)
		}
	}
	e.clock.advance(7 * time.Hour)
	if code, _ := e.me(sso); code != http.StatusUnauthorized {
		t.Fatalf("SSO /api/me at 25 h = %d, want 401", code)
	}

	// A local admin's session in use every few hours still ends after 7 days.
	c = e.signInOperator()
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
	e.aliceOn("main", store.RoleViewer)
	c := e.aliceSession()
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
	other := e.aliceSession()
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

// A local admin's sessions: 7 days at most, 8 hours idle. (The SSO maximum of
// 24 hours is in the store's TestPruneSessionsSSOMaximum.)
func TestPruneSessions(t *testing.T) {
	e := newEnv(t, nil)
	old := e.signInOperator()
	idle := e.signInPassword("root", operatorPassword)
	for i := 0; i < 7*24/6-1; i++ { // old stays in use, idle doesn't
		e.clock.advance(6 * time.Hour)
		e.me(old)
	}
	fresh := e.signInPassword("root", operatorPassword)
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
		GitLab:    []config.Instance{{Name: "main", URL: "https://gitlab.example.com"}},
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
