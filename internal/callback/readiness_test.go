package callback

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/readiness"
)

// fakeScans is one scan with one token: the first result is stored, a later
// one conflicts, any other token is unauthorized.
type fakeScans struct {
	token    string
	done     bool
	err      error
	stored   []byte
	tooLarge []string
}

func (f *fakeScans) CompleteScan(_ context.Context, _, token string, body []byte) (readiness.ScanOutcome, error) {
	switch {
	case f.err != nil:
		return 0, f.err
	case token != f.token:
		return readiness.ScanUnauthorized, nil
	case f.done:
		return readiness.ScanConflict, nil
	}
	f.done = true
	f.stored = body
	return readiness.ScanStored, nil
}

func (f *fakeScans) ScanTooLarge(_ context.Context, _, token string) error {
	f.tooLarge = append(f.tooLarge, token)
	return nil
}

func postScan(h http.Handler, id, token string, body []byte) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.Handle("POST /readiness-scans/{id}/result", h)
	req := httptest.NewRequest(http.MethodPost, "/readiness-scans/"+id+"/result", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// The scan callback takes one result per token: no or a wrong token is 401,
// the first post is 204, a second is 409, GET is 405.
func TestScanCallbackOneTime(t *testing.T) {
	f := &fakeScans{token: "good-token"}
	h := &ScanHandler{Scans: f, Log: slog.New(slog.DiscardHandler)}
	body := []byte(`{"files":[]}`)

	if rec := postScan(h, "s1", "", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("no bearer = %d; want 401", rec.Code)
	}
	if rec := postScan(h, "s1", "wrong", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d; want 401", rec.Code)
	}
	if f.done {
		t.Fatal("a bad token stored a result")
	}
	if rec := postScan(h, "s1", "good-token", body); rec.Code != http.StatusNoContent {
		t.Fatalf("first post = %d; want 204", rec.Code)
	}
	if string(f.stored) != string(body) {
		t.Errorf("stored %q; want the posted body", f.stored)
	}
	if rec := postScan(h, "s1", "good-token", body); rec.Code != http.StatusConflict {
		t.Errorf("second post = %d; want 409", rec.Code)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readiness-scans/s1/result", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d; want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("Allow = %q; want POST", rec.Header().Get("Allow"))
	}
}

// A body over readiness.MaxResultBytes is 413 and fails the scan at once, with
// the token the Job holds.
func TestScanCallbackTooLarge(t *testing.T) {
	f := &fakeScans{token: "good-token"}
	h := &ScanHandler{Scans: f, Log: slog.New(slog.DiscardHandler)}
	big := []byte(strings.Repeat("x", readiness.MaxResultBytes+1))

	if rec := postScan(h, "s1", "good-token", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over 1 MiB = %d; want 413", rec.Code)
	}
	if len(f.tooLarge) != 1 || f.tooLarge[0] != "good-token" {
		t.Errorf("ScanTooLarge calls = %v; want one, with the token", f.tooLarge)
	}
	if f.done {
		t.Error("an oversized body was stored")
	}
	// Exactly the limit is still a result.
	exact := []byte(strings.Repeat("x", readiness.MaxResultBytes))
	if rec := postScan(h, "s2", "good-token", exact); rec.Code != http.StatusNoContent {
		t.Errorf("exactly 1 MiB = %d; want 204", rec.Code)
	}
}

// An engine error is a 500 with no error text, and the log carries neither the
// token nor the body.
func TestScanCallbackEngineError(t *testing.T) {
	f := &fakeScans{token: "good-token", err: errors.New("db password=hunter2 down")}
	var logs bytes.Buffer
	h := &ScanHandler{Scans: f, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	rec := postScan(h, "s1", "good-token", []byte(`secret-body`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("engine error = %d; want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "down") {
		t.Errorf("the response carries the error text: %q", rec.Body.String())
	}
	if strings.Contains(logs.String(), "good-token") || strings.Contains(logs.String(), "secret-body") {
		t.Errorf("the log carries the token or the body: %s", logs.String())
	}
}
