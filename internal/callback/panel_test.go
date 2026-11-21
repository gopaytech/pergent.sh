package callback

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

type fakePanel struct {
	token   string
	results map[string][]byte
	err     error
}

func (f *fakePanel) PanelMemberResult(_ context.Context, runID, token, role string) ([]byte, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	b, ok := f.results[runID+"/"+role]
	if token != f.token || !ok {
		return nil, false, nil
	}
	return b, true, nil
}

func panelMux(f *fakePanel, log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /runs/{id}/panel/{role}", &PanelHandler{Panel: f, Log: log})
	return mux
}

func TestPanelHandler(t *testing.T) {
	f := &fakePanel{token: "tok", results: map[string][]byte{"r1/a": []byte(`{"a":1}`), "r1/b": []byte(`{"b":2}`)}}
	mux := panelMux(f, nil)
	for role, want := range map[string]string{"a": `{"a":1}`, "b": `{"b":2}`} {
		rec := do(mux, http.MethodGet, "/runs/r1/panel/"+role, "tok", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != want ||
			rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("role %s = %d %q %v", role, rec.Code, rec.Body.String(), rec.Header())
		}
	}
	for name, c := range map[string]struct{ path, token string }{
		"no token":    {"/runs/r1/panel/a", ""},
		"bad token":   {"/runs/r1/panel/a", "other"},
		"other role":  {"/runs/r1/panel/c", "tok"},
		"merge role":  {"/runs/r1/panel/merge", "tok"},
		"unknown run": {"/runs/zz/panel/a", "tok"},
	} {
		if rec := do(mux, http.MethodGet, c.path, c.token, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d; want 404", name, rec.Code)
		}
	}
	delete(f.results, "r1/b")
	if rec := do(mux, http.MethodGet, "/runs/r1/panel/b", "tok", nil); rec.Code != http.StatusNotFound {
		t.Errorf("no result = %d; want 404", rec.Code)
	}
	if rec := do(mux, http.MethodPost, "/runs/r1/panel/a", "tok", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", rec.Code)
	}
}

// A store error is a 500 with no detail; the log has neither the token nor
// a result body.
func TestPanelHandlerStoreError(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	f := &fakePanel{token: "secret-token", err: errors.New("db down"), results: map[string][]byte{"r1/a": []byte("SECRETBODY")}}
	rec := do(panelMux(f, log), http.MethodGet, "/runs/r1/panel/a", "secret-token", nil)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "db down") {
		t.Errorf("error = %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), "secret-token") || strings.Contains(logs.String(), "SECRETBODY") {
		t.Errorf("log leaks: %s", logs.String())
	}
}
