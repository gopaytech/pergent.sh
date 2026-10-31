package dashboard

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/password"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	wrongCredentialsBody = `{"error":"wrong username or password"}`
	tooManyAttemptsBody  = `{"error":"too many sign-in attempts, try again later"}`
)

// doJSON serves one request with body as its JSON body (nil: none) and
// records the response.
func (e *env) doJSON(method, target string, cookies []*http.Cookie, header map[string]string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			e.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	e.mu.Lock()
	e.responses = append(e.responses, rec)
	e.mu.Unlock()
	return rec
}

// loginCSRF fetches the sign-in form's CSRF cookie and token.
func (e *env) loginCSRF() (*http.Cookie, string) {
	e.t.Helper()
	rec := e.do("GET", "/auth/csrf", nil, nil)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET /auth/csrf = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		e.t.Fatal(err)
	}
	c := cookie(rec, loginCSRFCookieName)
	if c == nil {
		e.t.Fatal("GET /auth/csrf set no cookie")
	}
	return c, body.CSRFToken
}

// passwordSignIn posts username and pw with a fresh login CSRF token, from
// the client IP ip ("" for the request's default address).
func (e *env) passwordSignIn(username, pw, ip string) *httptest.ResponseRecorder {
	e.t.Helper()
	c, token := e.loginCSRF()
	header := map[string]string{CSRFHeader: token}
	if ip != "" {
		header["X-Forwarded-For"] = ip
	}
	return e.doJSON("POST", "/auth/password", []*http.Cookie{c}, header,
		map[string]string{"username": username, "password": pw})
}

// signInPassword signs username in with pw and returns the session cookie.
func (e *env) signInPassword(username, pw string) *http.Cookie {
	e.t.Helper()
	rec := e.passwordSignIn(username, pw, "")
	if rec.Code != http.StatusNoContent {
		e.t.Fatalf("POST /auth/password = %d %s", rec.Code, rec.Body)
	}
	c := cookie(rec, SessionCookieName)
	if c == nil {
		e.t.Fatal("password sign-in set no session cookie")
	}
	return c
}

// addPasswordAccount stores a password account with password pw.
func (e *env) addPasswordAccount(username, pw string, mustChange bool) store.Account {
	e.t.Helper()
	h, err := password.Hash(pw)
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.st.CreatePasswordAccount(ctx, username, h, mustChange, e.clock.now())
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

// audits returns the audit log's actions, oldest first, as "actor action".
func (e *env) audits() []string {
	e.t.Helper()
	entries, err := e.st.AuditLog(ctx, 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		out = append(out, entries[i].Actor+" "+entries[i].Action)
	}
	return out
}

func body(rec *httptest.ResponseRecorder) string { return strings.TrimSpace(rec.Body.String()) }

func TestBootstrapFromEnv(t *testing.T) {
	e := newEnv(t, nil)
	e.cfg.Dashboard.AdminPasswordEnv = "PERGENT_TEST_ADMIN_PASSWORD"
	t.Setenv("PERGENT_TEST_ADMIN_PASSWORD", "first-admin-password-1")
	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	if err := Bootstrap(ctx, e.st, e.cfg, e.clock.now(), log); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if strings.Contains(logs.String(), "first-admin-password-1") || strings.Contains(logs.String(), "initial admin password") {
		t.Errorf("Bootstrap logged the password from the environment: %s", logs)
	}
	a, found, err := e.st.PasswordAccount(ctx, "admin")
	if err != nil || !found || !a.MustChangePassword {
		t.Fatalf("admin account = %+v, %v, %v; want one that must change its password", a, found, err)
	}
	c := e.signInPassword("admin", "first-admin-password-1")
	if code, m := e.me(c); code != 200 || m.Username != "admin" || !m.MustChangePassword {
		t.Errorf("/api/me = %d %+v, want admin with mustChangePassword", code, m)
	}
	// A later change of the variable does nothing.
	t.Setenv("PERGENT_TEST_ADMIN_PASSWORD", "another-admin-password")
	if err := Bootstrap(ctx, e.st, e.cfg, e.clock.now(), log); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if rec := e.passwordSignIn("admin", "another-admin-password", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("sign-in with the changed variable = %d, want 401", rec.Code)
	}
}

func TestBootstrapGenerated(t *testing.T) {
	for name, envName := range map[string]string{"no variable": "", "empty variable": "PERGENT_TEST_ADMIN_PASSWORD"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.cfg.Dashboard.AdminPasswordEnv = envName
			t.Setenv("PERGENT_TEST_ADMIN_PASSWORD", "")
			logs := &lockedBuffer{}
			log := slog.New(slog.NewJSONHandler(logs, nil))
			if err := Bootstrap(ctx, e.st, e.cfg, e.clock.now(), log); err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}
			var lines []map[string]any
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				if !strings.Contains(line, "initial admin password") {
					continue
				}
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Fatal(err)
				}
				lines = append(lines, m)
			}
			if len(lines) != 1 {
				t.Fatalf("%d log lines hold the initial admin password, want 1: %s", len(lines), logs)
			}
			pw, _ := lines[0]["password"].(string)
			if lines[0]["level"] != "WARN" || lines[0]["msg"] != "initial admin password" || len(pw) < 24 {
				t.Errorf("log line = %v, want a Warn with a password of 24+ characters", lines[0])
			}
			c := e.signInPassword("admin", pw)
			if code, m := e.me(c); code != 200 || !m.MustChangePassword {
				t.Errorf("/api/me = %d %+v, want mustChangePassword", code, m)
			}

			before := logs.String()
			if err := Bootstrap(ctx, e.st, e.cfg, e.clock.now(), log); err != nil {
				t.Fatalf("second Bootstrap: %v", err)
			}
			if logs.String() != before {
				t.Errorf("second Bootstrap logged %q", strings.TrimPrefix(logs.String(), before))
			}
			if n, err := e.st.CountPasswordAccounts(ctx); err != nil || n != 1 {
				t.Errorf("password accounts = %d, %v; want 1", n, err)
			}
		})
	}
}

func TestLoginCSRFCookie(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.do("GET", "/auth/csrf", nil, nil)
	c := cookie(rec, loginCSRFCookieName)
	if rec.Code != 200 || c == nil {
		t.Fatalf("GET /auth/csrf = %d, cookie %v", rec.Code, c)
	}
	if c.Name != "__Host-pergent_login_csrf" || c.Path != "/" || c.MaxAge != 600 || !c.HttpOnly || !c.Secure ||
		c.SameSite != http.SameSiteStrictMode || c.Domain != "" {
		t.Errorf("login CSRF cookie = %+v", c)
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(raw) != 32 {
		t.Errorf("login CSRF token %q is not 32 random bytes", c.Value)
	}
	if body(rec) != `{"csrfToken":"`+c.Value+`"}` {
		t.Errorf("body = %s, want the cookie's token", rec.Body)
	}
	if _, other := e.loginCSRF(); other == c.Value {
		t.Error("two GET /auth/csrf gave the same token")
	}
}

func TestPasswordSignIn(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("ops", "ops-password-123", false)
	creds := map[string]string{"username": "ops", "password": "ops-password-123"}

	c, token := e.loginCSRF()
	other, otherToken := e.loginCSRF()
	for name, req := range map[string]struct {
		cookies []*http.Cookie
		header  map[string]string
	}{
		"no cookie":        {nil, map[string]string{CSRFHeader: token}},
		"no header":        {[]*http.Cookie{c}, nil},
		"wrong header":     {[]*http.Cookie{c}, map[string]string{CSRFHeader: otherToken}},
		"another's cookie": {[]*http.Cookie{other}, map[string]string{CSRFHeader: token}},
	} {
		rec := e.doJSON("POST", "/auth/password", req.cookies, req.header, creds)
		if rec.Code != http.StatusForbidden || !strings.Contains(body(rec), `"code":"csrf"`) {
			t.Errorf("%s: POST /auth/password = %d %s, want 403 csrf", name, rec.Code, rec.Body)
		}
		if cookie(rec, SessionCookieName) != nil {
			t.Errorf("%s: a session cookie was set", name)
		}
	}
	if rec := e.doJSON("POST", "/auth/password", []*http.Cookie{c}, map[string]string{CSRFHeader: token}, "not an object"); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed body = %d, want 400", rec.Code)
	}

	rec := e.doJSON("POST", "/auth/password", []*http.Cookie{c}, map[string]string{CSRFHeader: token}, creds)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /auth/password = %d %s, want 204", rec.Code, rec.Body)
	}
	sc := cookie(rec, SessionCookieName)
	if sc == nil || sc.Path != "/" || !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteLaxMode ||
		sc.MaxAge != int(MaxSessionAge/time.Second) {
		t.Fatalf("session cookie = %+v", sc)
	}
	code, m := e.me(sc)
	if code != 200 || m.Kind != "password" || m.Username != "ops" || !m.Admin || !m.Operator || m.Waiting || m.MustChangePassword ||
		m.Platform != "" || m.Instance != "" || m.CSRFToken == "" {
		t.Errorf("/api/me = %d %+v", code, m)
	}
	se, found := e.storedSession(sc)
	acc, _, _ := e.st.PasswordAccount(ctx, "ops")
	if !found || se.AccountID != acc.ID || se.TokenSealed != nil || se.Instance != "" {
		t.Errorf("stored session = %+v, %v; want ops's, with no token", se, found)
	}
	if acc.LastSignInAt == nil {
		t.Error("the sign-in was not recorded on the account")
	}
	if got := e.audits(); len(got) != 1 || got[0] != "ops sign_in" {
		t.Errorf("audit = %q, want [ops sign_in]", got)
	}
	// A platform user's /api/me says so.
	if code, m := e.me(e.signIn("gitlab", "main")); code != 200 || m.Kind != "platform" || m.MustChangePassword {
		t.Errorf("platform /api/me = %d %+v", code, m)
	}

	// Signing in again ends the session the browser came with.
	c2, token2 := e.loginCSRF()
	rec = e.doJSON("POST", "/auth/password", []*http.Cookie{c2, sc}, map[string]string{CSRFHeader: token2}, creds)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second sign-in = %d", rec.Code)
	}
	if _, found := e.storedSession(sc); found {
		t.Error("the session the browser came with is still there")
	}
	for _, r := range e.responses {
		if strings.Contains(r.Body.String(), "ops-password-123") {
			t.Errorf("a response holds the password: %s", r.Body)
		}
	}
	if strings.Contains(e.logs.String(), "ops-password-123") {
		t.Error("the log holds the password")
	}
}

func TestPasswordGuessing(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("ops", "ops-password-123", false)
	attempts := 0
	try := func(username, pw, ip string) *httptest.ResponseRecorder {
		t.Helper()
		attempts++
		return e.passwordSignIn(username, pw, ip)
	}
	for i := range 5 {
		if rec := try("ops", "wrong-password-"+string(rune('a'+i)), ""); rec.Code != 401 || body(rec) != wrongCredentialsBody {
			t.Fatalf("wrong password %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if rec := try("ops", "ops-password-123", ""); rec.Code != 401 || body(rec) != wrongCredentialsBody {
		t.Errorf("the right password while locked = %d %s, want 401 %s", rec.Code, rec.Body, wrongCredentialsBody)
	}
	if rec := try("OPS", "ops-password-123", ""); rec.Code != 401 || body(rec) != wrongCredentialsBody {
		t.Errorf("the right password, other case, while locked = %d %s", rec.Code, rec.Body)
	}
	if rec := try("nobody", "whatever-password", ""); rec.Code != 401 || body(rec) != wrongCredentialsBody {
		t.Errorf("an unknown username = %d %s, want 401 %s", rec.Code, rec.Body, wrongCredentialsBody)
	}
	var accounts int
	if err := e.db.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&accounts); err != nil || accounts != 1 {
		t.Errorf("accounts = %d, %v; want only ops", accounts, err)
	}
	got := e.audits()
	want := []string{"ops sign_in_failed", "ops sign_in_failed", "ops sign_in_failed", "ops sign_in_failed",
		"ops sign_in_failed", "ops locked", "ops sign_in_failed", "ops sign_in_failed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("audit = %q, want %q", got, want)
	}
	if strings.Contains(e.logs.String(), "nobody") {
		t.Error("the log names the unknown username")
	}

	// 20 attempts per IP in 5 minutes: the 21st is refused.
	for attempts < 20 {
		if rec := try("nobody", "whatever-password", ""); rec.Code != 401 {
			t.Fatalf("attempt %d = %d %s, want 401", attempts, rec.Code, rec.Body)
		}
	}
	if rec := try("ops", "ops-password-123", ""); rec.Code != http.StatusTooManyRequests || body(rec) != tooManyAttemptsBody {
		t.Errorf("attempt 21 = %d %s, want 429 %s", rec.Code, rec.Body, tooManyAttemptsBody)
	}
	// The limit is per IP: the last X-Forwarded-For entry is the client.
	if rec := try("nobody", "whatever-password", "192.0.2.1, 198.51.100.7"); rec.Code != 401 {
		t.Errorf("another IP = %d %s, want 401", rec.Code, rec.Body)
	}
	if rec := try("nobody", "whatever-password", "198.51.100.7, 192.0.2.1"); rec.Code != 429 {
		t.Errorf("the limited IP behind a proxy = %d, want 429", rec.Code)
	}

	e.clock.advance(15 * time.Minute)
	c := e.signInPassword("ops", "ops-password-123")
	if code, m := e.me(c); code != 200 || m.Username != "ops" {
		t.Errorf("/api/me after the lock = %d %+v", code, m)
	}
	if acc, _, _ := e.st.PasswordAccount(ctx, "ops"); acc.FailedAttempts != 0 || acc.LockedUntil != nil {
		t.Errorf("after a good sign-in the account = %+v, want no failures or lock", acc)
	}
}

func TestClientIP(t *testing.T) {
	for _, tc := range []struct{ remote, xff, want string }{
		{"192.0.2.1:1234", "", "192.0.2.1"},
		{"[2001:db8::1]:443", "", "2001:db8::1"},
		{"192.0.2.1:1234", "198.51.100.7", "198.51.100.7"},
		{"192.0.2.1:1234", "203.0.113.5, 198.51.100.7 ", "198.51.100.7"},
		{"192.0.2.1", "", "192.0.2.1"},
	} {
		r := httptest.NewRequest("POST", "/auth/password", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := clientIP(r); got != tc.want {
			t.Errorf("clientIP(%q, XFF %q) = %q, want %q", tc.remote, tc.xff, got, tc.want)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter()
	t0 := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	for i := range 20 {
		if !l.allow("a", t0.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if l.allow("a", t0.Add(30*time.Second)) {
		t.Error("attempt 21 allowed")
	}
	if !l.allow("b", t0.Add(30*time.Second)) {
		t.Error("another IP refused")
	}
	// The first attempt leaves the window 5 minutes later.
	if !l.allow("a", t0.Add(5*time.Minute+time.Second)) {
		t.Error("an attempt after the oldest left the window was refused")
	}
	// Old IPs are dropped.
	l.allow("c", t0.Add(time.Hour))
	l.mu.Lock()
	n := len(l.hits)
	l.mu.Unlock()
	if n != 1 {
		t.Errorf("limiter holds %d IPs an hour later, want 1", n)
	}
}

// meCSRF returns the session's CSRF token.
func (e *env) meCSRF(c *http.Cookie) string {
	e.t.Helper()
	code, m := e.me(c)
	if code != 200 {
		e.t.Fatalf("/api/me = %d", code)
	}
	return m.CSRFToken
}

func (e *env) changePassword(c *http.Cookie, current, next string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.doJSON("POST", "/auth/password/change", []*http.Cookie{c}, map[string]string{CSRFHeader: e.meCSRF(c)},
		map[string]string{"current": current, "new": next})
}

func TestMustChangePassword(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("admin", "first-password-123", true)
	a := e.signInPassword("admin", "first-password-123")
	b := e.signInPassword("admin", "first-password-123")

	rec := e.do("GET", "/api/overview", []*http.Cookie{a}, nil)
	if rec.Code != http.StatusForbidden ||
		body(rec) != `{"code":"password_change_required","error":"change your password first"}` {
		t.Errorf("GET /api/overview before the change = %d %s", rec.Code, rec.Body)
	}
	if code, m := e.me(a); code != 200 || !m.MustChangePassword {
		t.Errorf("/api/me = %d %+v, want mustChangePassword", code, m)
	}

	if rec := e.changePassword(a, "first-password-123", "second-password-456"); rec.Code != http.StatusNoContent {
		t.Fatalf("POST /auth/password/change = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/api/overview", []*http.Cookie{a}, nil); rec.Code != 200 {
		t.Errorf("GET /api/overview after the change = %d %s, want 200", rec.Code, rec.Body)
	}
	if code, m := e.me(a); code != 200 || m.MustChangePassword {
		t.Errorf("/api/me after the change = %d %+v", code, m)
	}
	if code, _ := e.me(b); code != http.StatusUnauthorized {
		t.Errorf("the account's other session = %d, want 401", code)
	}
	if rec := e.passwordSignIn("admin", "first-password-123", ""); rec.Code != 401 {
		t.Errorf("the old password = %d, want 401", rec.Code)
	}
	e.signInPassword("admin", "second-password-456")
	got := e.audits()
	if len(got) != 5 || got[2] != "admin password_changed" {
		t.Errorf("audit = %q, want sign-ins, password_changed, a failure and a sign-in", got)
	}
	// Logging out works while a change is due.
	e.addPasswordAccount("ops", "first-password-123", true)
	c := e.signInPassword("ops", "first-password-123")
	if rec := e.do("POST", "/auth/logout", []*http.Cookie{c}, map[string]string{CSRFHeader: e.meCSRF(c)}); rec.Code != http.StatusNoContent {
		t.Errorf("POST /auth/logout = %d %s", rec.Code, rec.Body)
	}
}

func TestChangePasswordRules(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("ops", "ops-password-123", false)
	c := e.signInPassword("ops", "ops-password-123")

	if rec := e.changePassword(c, "ops-password-123", "elevenchars"); rec.Code != http.StatusBadRequest {
		t.Errorf("an 11-character password = %d %s, want 400", rec.Code, rec.Body)
	}
	if rec := e.changePassword(c, "ops-password-123", "ops-password-123"); rec.Code != http.StatusBadRequest {
		t.Errorf("the same password = %d %s, want 400", rec.Code, rec.Body)
	}
	if rec := e.changePassword(c, "not-the-password", "new-password-456"); rec.Code != http.StatusUnauthorized ||
		body(rec) != `{"error":"wrong password"}` {
		t.Errorf("a wrong current password = %d %s, want 401", rec.Code, rec.Body)
	}
	if rec := e.doJSON("POST", "/auth/password/change", []*http.Cookie{c}, nil,
		map[string]string{"current": "ops-password-123", "new": "new-password-456"}); rec.Code != http.StatusForbidden ||
		!strings.Contains(body(rec), `"code":"csrf"`) {
		t.Errorf("a change without the CSRF token = %d %s, want 403 csrf", rec.Code, rec.Body)
	}
	// 12 characters, counted as characters, not bytes.
	if rec := e.changePassword(c, "ops-password-123", "ééééééééééé"); rec.Code != http.StatusBadRequest {
		t.Errorf("11 two-byte characters = %d, want 400", rec.Code)
	}
	if rec := e.changePassword(c, "ops-password-123", "éééééééééééé"); rec.Code != http.StatusNoContent {
		t.Errorf("12 two-byte characters = %d %s, want 204", rec.Code, rec.Body)
	}
	// A platform account has no password to change.
	pc := e.signIn("gitlab", "main")
	if rec := e.changePassword(pc, "x", "new-password-456"); rec.Code != http.StatusForbidden {
		t.Errorf("a platform account's change = %d, want 403", rec.Code)
	}
}

func TestDeletedAccountEndsSession(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("admin", "admin-password-123", false)
	ops := e.addPasswordAccount("ops", "ops-password-123", false)
	c := e.signInPassword("ops", "ops-password-123")
	if code, _ := e.me(c); code != 200 {
		t.Fatalf("/api/me = %d", code)
	}
	if _, err := e.st.DeleteAccount(ctx, ops.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.me(c); code != http.StatusUnauthorized {
		t.Errorf("/api/me after the account was deleted = %d, want 401", code)
	}

	pc := e.signIn("gitlab", "main")
	se, _ := e.storedSession(pc)
	if _, err := e.st.DeleteAccount(ctx, se.AccountID); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.me(pc); code != http.StatusUnauthorized {
		t.Errorf("a platform session after its account was deleted = %d, want 401", code)
	}
}

func TestResetPassword(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("ops", "ops-password-123", false)
	c := e.signInPassword("ops", "ops-password-123")
	for range 5 {
		e.passwordSignIn("ops", "wrong-password-123", "")
	}
	pw, err := ResetPassword(ctx, e.st, "OPS", e.clock.now())
	if err != nil || len(pw) < 24 {
		t.Fatalf("ResetPassword = %q, %v", pw, err)
	}
	if code, _ := e.me(c); code != http.StatusUnauthorized {
		t.Errorf("a session from before the reset = %d, want 401", code)
	}
	c = e.signInPassword("ops", pw) // the lock is gone
	if code, m := e.me(c); code != 200 || !m.MustChangePassword {
		t.Errorf("/api/me after the reset = %d %+v, want mustChangePassword", code, m)
	}
	if got := e.audits(); !strings.Contains(strings.Join(got, ","), "cli password_reset") {
		t.Errorf("audit = %q, want a cli password_reset", got)
	}

	e.signIn("gitlab", "main") // alice, a platform account
	for _, name := range []string{"alice", "nobody"} {
		if _, err := ResetPassword(ctx, e.st, name, e.clock.now()); !errors.Is(err, ErrNotPasswordAccount) {
			t.Errorf("ResetPassword(%s) = %v, want ErrNotPasswordAccount", name, err)
		}
	}
}

func TestBootstrapShortEnvPassword(t *testing.T) {
	e := newEnv(t, nil)
	e.cfg.Dashboard.AdminPasswordEnv = "PERGENT_TEST_ADMIN_PASSWORD"
	t.Setenv("PERGENT_TEST_ADMIN_PASSWORD", "elevenchars")
	err := Bootstrap(ctx, e.st, e.cfg, e.clock.now(), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "PERGENT_TEST_ADMIN_PASSWORD") || !strings.Contains(err.Error(), "12") ||
		strings.Contains(err.Error(), "elevenchars") {
		t.Errorf("Bootstrap = %v, want an error naming the variable and the minimum, not the value", err)
	}
	if n, _ := e.st.CountPasswordAccounts(ctx); n != 0 {
		t.Errorf("password accounts = %d, want none", n)
	}
}

// A lock set by failures that land between reading the account and
// recording the sign-in still refuses the right password, and stays.
func TestSignInRaceWithLock(t *testing.T) {
	e := newEnv(t, nil)
	acc := e.addPasswordAccount("ops", "ops-password-123", false)
	e.d.Auth.afterVerify = func() {
		e.d.Auth.afterVerify = nil
		for range 5 {
			if _, err := e.st.RecordFailedSignIn(ctx, acc.ID, e.clock.now(), 5, 15*time.Minute); err != nil {
				t.Error(err)
			}
		}
	}
	rec := e.passwordSignIn("ops", "ops-password-123", "")
	if rec.Code != http.StatusUnauthorized || body(rec) != wrongCredentialsBody || cookie(rec, SessionCookieName) != nil {
		t.Fatalf("the right password, locked meanwhile = %d %s, want 401 and no session", rec.Code, rec.Body)
	}
	got, _, _ := e.st.Account(ctx, acc.ID)
	if got.LockedUntil == nil || !got.LockedUntil.After(e.clock.now()) || got.LastSignInAt != nil {
		t.Errorf("account = %+v, want it still locked and no sign-in", got)
	}
	if a := e.audits(); len(a) != 1 || a[0] != "ops sign_in_failed" {
		t.Errorf("audit = %q, want one sign_in_failed", a)
	}
}

// With every argon2id slot taken, a sign-in answers 503 without checking
// the password, and works again once a slot is free.
func TestHashSlotsFull(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("ops", "ops-password-123", false)
	c := e.signInPassword("ops", "ops-password-123")
	verifies := 0
	e.d.Auth.verify = func(hash, pw string) bool { verifies++; return password.Verify(hash, pw) }
	e.d.Auth.slotWait = 20 * time.Millisecond
	for range cap(e.d.Auth.slots) {
		e.d.Auth.slots <- struct{}{}
	}
	if rec := e.passwordSignIn("ops", "ops-password-123", ""); rec.Code != http.StatusServiceUnavailable || body(rec) != tooManyAttemptsBody {
		t.Errorf("sign-in with the slots taken = %d %s, want 503 %s", rec.Code, rec.Body, tooManyAttemptsBody)
	}
	if rec := e.passwordSignIn("nobody", "whatever-password", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("an unknown username with the slots taken = %d, want 503", rec.Code)
	}
	if rec := e.changePassword(c, "ops-password-123", "new-password-456"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a change with the slots taken = %d, want 503", rec.Code)
	}
	if verifies != 0 {
		t.Errorf("Verify ran %d times with no slot free", verifies)
	}
	if a := e.audits(); len(a) != 1 {
		t.Errorf("audit = %q, want only the first sign-in", a)
	}
	<-e.d.Auth.slots
	e.signInPassword("ops", "ops-password-123")
	if verifies != 1 {
		t.Errorf("Verify ran %d times, want 1", verifies)
	}
}

// While the account is locked, a change with the right current password
// is refused without hashing the new one: it costs no more than a wrong
// one.
func TestChangePasswordLockedHashesNothing(t *testing.T) {
	e := newEnv(t, nil)
	acc := e.addPasswordAccount("ops", "ops-password-123", false)
	c := e.signInPassword("ops", "ops-password-123")
	hashes := 0
	e.d.Auth.hash = func(pw string) (string, error) { hashes++; return password.Hash(pw) }
	for range 5 {
		if _, err := e.st.RecordFailedSignIn(ctx, acc.ID, e.clock.now(), 5, 15*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if rec := e.changePassword(c, "ops-password-123", "new-password-456"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the right current password while locked = %d %s, want 401", rec.Code, rec.Body)
	}
	if hashes != 0 {
		t.Errorf("the new password was hashed %d times while locked", hashes)
	}
	e.clock.advance(15 * time.Minute)
	if rec := e.changePassword(c, "ops-password-123", "new-password-456"); rec.Code != http.StatusNoContent {
		t.Fatalf("the change after the lock = %d %s, want 204", rec.Code, rec.Body)
	}
	if hashes != 1 {
		t.Errorf("the new password was hashed %d times, want 1", hashes)
	}
}

// Wrong passwords that land at once past the limit record the lock once.
func TestConcurrentFailuresAuditLockOnce(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("ops", "ops-password-123", false)
	for i := range 4 {
		if rec := e.passwordSignIn("ops", "wrong-password-"+string(rune('a'+i)), ""); rec.Code != 401 {
			t.Fatalf("wrong password %d = %d", i+1, rec.Code)
		}
	}
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.passwordSignIn("ops", "wrong-password-x"+string(rune('a'+i)), "")
		}()
	}
	wg.Wait()
	locks := 0
	for _, a := range e.audits() {
		if a == "ops locked" {
			locks++
		}
	}
	if locks != 1 {
		t.Errorf("the lock was audited %d times, want 1: %q", locks, e.audits())
	}
	if n := strings.Count(e.logs.String(), "account locked after"); n != 1 {
		t.Errorf("the lock was logged %d times, want 1", n)
	}
}

// A wrong current password at a change counts towards the lock; while
// locked, even the right one is refused.
func TestChangePasswordCountsTowardsLock(t *testing.T) {
	e := newEnv(t, nil)
	acc := e.addPasswordAccount("ops", "ops-password-123", false)
	c := e.signInPassword("ops", "ops-password-123")
	for i := range 5 {
		if rec := e.changePassword(c, "not-the-password", "new-password-456"); rec.Code != 401 || body(rec) != `{"error":"wrong password"}` {
			t.Fatalf("wrong current password %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if got, _, _ := e.st.Account(ctx, acc.ID); got.LockedUntil == nil {
		t.Fatalf("after 5 wrong current passwords = %+v, want locked", got)
	}
	if rec := e.changePassword(c, "ops-password-123", "new-password-456"); rec.Code != 401 || body(rec) != `{"error":"wrong password"}` {
		t.Errorf("the right current password while locked = %d %s, want 401", rec.Code, rec.Body)
	}
	if got, _, _ := e.st.Account(ctx, acc.ID); !password.Verify(got.PasswordHash, "ops-password-123") {
		t.Error("the password changed while locked")
	}
	if a := e.audits(); !strings.Contains(strings.Join(a, ","), "ops locked") {
		t.Errorf("audit = %q, want the lock", a)
	}
	e.clock.advance(15 * time.Minute)
	if rec := e.changePassword(c, "ops-password-123", "new-password-456"); rec.Code != http.StatusNoContent {
		t.Errorf("the change after the lock = %d %s, want 204", rec.Code, rec.Body)
	}
}
