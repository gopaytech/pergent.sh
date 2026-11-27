package callback

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/security"
)

// fakeSecurityResults is one scan with one token: the first result is
// stored, a later one conflicts, any other token is unauthorized.
type fakeSecurityResults struct {
	token    string
	done     bool
	stored   []byte
	tooLarge []string
}

func (f *fakeSecurityResults) CompleteSecurityScan(_ context.Context, _, token string, body []byte) (security.Outcome, error) {
	switch {
	case token != f.token:
		return security.Unauthorized, nil
	case f.done:
		return security.Conflict, nil
	}
	f.done, f.stored = true, body
	return security.Stored, nil
}

func (f *fakeSecurityResults) SecurityTooLarge(_ context.Context, _, token string) error {
	f.tooLarge = append(f.tooLarge, token)
	return nil
}

func postSecurity(h http.Handler, method, token string, body []byte) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.Handle("/security-scans/{id}/result", h)
	req := httptest.NewRequest(method, "/security-scans/s1/result", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// The result route takes one result per token: GET is 405, no or a wrong
// token 401, a body over security.MaxBodyBytes 413 (and the scan fails at
// once), the first post 204 and a second 409. The log never holds the token.
func TestSecurityHandler(t *testing.T) {
	t.Parallel()
	const token = "scan-token-1234567890"
	f := &fakeSecurityResults{token: token}
	var logs bytes.Buffer
	h := &SecurityHandler{Scans: f, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	body := []byte(`{"kind":"security"}`)

	if rec := postSecurity(h, http.MethodGet, token, nil); rec.Code != http.StatusMethodNotAllowed ||
		rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET = %d, Allow %q; want 405, POST", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := postSecurity(h, http.MethodPost, "", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("no bearer = %d; want 401", rec.Code)
	}
	big := []byte(strings.Repeat("x", security.MaxBodyBytes+1))
	if rec := postSecurity(h, http.MethodPost, token, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over 12 MiB = %d; want 413", rec.Code)
	}
	if len(f.tooLarge) != 1 || f.tooLarge[0] != token || f.done {
		t.Errorf("SecurityTooLarge calls = %d, stored %v; want one with the token, nothing stored", len(f.tooLarge), f.done)
	}
	if rec := postSecurity(h, http.MethodPost, "wrong", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d; want 401", rec.Code)
	}
	if rec := postSecurity(h, http.MethodPost, token, body); rec.Code != http.StatusNoContent {
		t.Fatalf("first post = %d; want 204", rec.Code)
	}
	if !bytes.Equal(f.stored, body) {
		t.Errorf("stored %q; want the posted body", f.stored)
	}
	if rec := postSecurity(h, http.MethodPost, token, body); rec.Code != http.StatusConflict {
		t.Errorf("second post = %d; want 409", rec.Code)
	}
	if strings.Contains(logs.String(), token) {
		t.Errorf("the log holds the token: %s", logs.String())
	}
}
