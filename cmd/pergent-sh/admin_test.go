package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/store/storetest"
)

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want command
	}{
		{[]string{"serve"}, command{name: "serve", configPath: "/etc/pergent-sh/config.yaml"}},
		{[]string{"serve", "--config", "x"}, command{name: "serve", configPath: "x"}},
		{[]string{"admin", "reset-password", "bob"}, command{name: "reset-password", username: "bob"}},
	} {
		if got, err := parseArgs(tc.args, io.Discard); err != nil || got != tc.want {
			t.Errorf("parseArgs(%q) = %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
	for _, args := range [][]string{nil, {"run"}, {"serve", "extra"}, {"serve", "--nope"},
		{"admin"}, {"admin", "reset-password"}, {"admin", "nope", "x"}, {"admin", "reset-password", "bob", "alice"},
		{"admin", "reset-password", ""}} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Errorf("parseArgs(%q) succeeded, want an error", args)
		}
	}
}

// testDatabaseURL returns a database URL of a migrated database of its own
// (back to the pool when the test ends), bound to its schema, and a store on
// it. It skips when no test database is set.
func testDatabaseURL(t *testing.T) (string, *store.Store) {
	t.Helper()
	dbURL := storetest.URL(t, store.OpenInSchema)
	st, err := store.OpenInSchema(context.Background(), dbURL, storetest.Schema)
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	t.Cleanup(st.Close)
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", storetest.Schema)
	u.RawQuery = q.Encode()
	return u.String(), st
}

func TestResetPasswordCommand(t *testing.T) {
	dbURL, st := testDatabaseURL(t)
	ctx := context.Background()
	cfg := &config.Config{Dashboard: config.Dashboard{URL: "https://pergent.example.com"}}
	if err := dashboard.Bootstrap(ctx, st, cfg, time.Now(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertPlatformAccount(ctx, "gitlab", "main", 9, "alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", dbURL)

	var stdout, stderr bytes.Buffer
	if code := resetPassword(ctx, "admin", &stdout, &stderr); code != 0 {
		t.Fatalf("reset-password admin = %d, stderr %q", code, stderr.String())
	}
	m := regexp.MustCompile(`^new password for admin: ([A-Za-z0-9]{24})\n$`).FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("stdout = %q, want the new password", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr.String())
	}

	// The new password signs in, and must be changed.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	d, err := dashboard.New(dashboard.Options{Config: cfg, Store: st, SecretKey: base64.StdEncoding.EncodeToString(key)})
	if err != nil {
		t.Fatal(err)
	}
	h := d.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/csrf", nil))
	var tok struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/auth/password",
		strings.NewReader(`{"username":"admin","password":"`+m[1]+`"}`))
	req.Header.Set(dashboard.CSRFHeader, tok.CSRFToken)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("sign-in with the new password = %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest("GET", "/api/me", nil)
	for _, c := range rec.Result().Cookies() {
		if c.Name == dashboard.SessionCookieName {
			req.AddCookie(c)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"mustChangePassword":true`) {
		t.Errorf("/api/me = %d %s, want mustChangePassword", rec.Code, rec.Body)
	}

	for _, name := range []string{"alice", "nobody"} {
		stdout.Reset()
		stderr.Reset()
		if code := resetPassword(ctx, name, &stdout, &stderr); code != 1 {
			t.Errorf("reset-password %s = %d, want 1", name, code)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "no password account") {
			t.Errorf("reset-password %s: stdout %q, stderr %q", name, stdout.String(), stderr.String())
		}
	}
	t.Setenv("DATABASE_URL", "")
	if code := resetPassword(ctx, "admin", &stdout, &stderr); code != 1 {
		t.Errorf("reset-password without DATABASE_URL = %d, want 1", code)
	}
}
