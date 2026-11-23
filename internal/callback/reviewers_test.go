package callback

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// fakeReviewers knows runs by their stored token: "" is a run whose Job has
// no stored token yet. An unknown run is authed for nothing.
type fakeReviewers struct {
	tokens  map[string]string // run -> its live result token
	results map[string][]byte // by run + "/" + reviewer
	err     error
}

func (f *fakeReviewers) ReviewerResultAuth(_ context.Context, runID, token string, n int) ([]byte, bool, bool, error) {
	if f.err != nil {
		return nil, false, false, f.err
	}
	want, known := f.tokens[runID]
	if !known || want == "" || token != want {
		return nil, false, false, nil
	}
	b, ok := f.results[runID+"/"+strconv.Itoa(n)]
	return b, true, ok, nil
}

func reviewersMux(f *fakeReviewers, log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /runs/{id}/reviewers/{n}", &ReviewersHandler{Reviewers: f, Log: log})
	return mux
}

func TestReviewersHandler(t *testing.T) {
	f := &fakeReviewers{
		tokens:  map[string]string{"r1": "tok", "r0": "", "r2": "tok2"},
		results: map[string][]byte{},
	}
	for n := 1; n <= 5; n++ {
		f.results["r1/"+strconv.Itoa(n)] = []byte(`{"n":` + strconv.Itoa(n) + `}`)
	}
	mux := reviewersMux(f, nil)
	for n := 1; n <= 5; n++ {
		rec := do(mux, http.MethodGet, "/runs/r1/reviewers/"+strconv.Itoa(n), "tok", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != string(f.results["r1/"+strconv.Itoa(n)]) ||
			rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("reviewer %d = %d %q %v", n, rec.Code, rec.Body.String(), rec.Header())
		}
	}
	// An auth failure is a 401, never a 404: the judge reads 404 as "no result".
	for name, c := range map[string]struct{ path, token string }{
		"no token":            {"/runs/r1/reviewers/1", ""},
		"bad token":           {"/runs/r1/reviewers/1", "other"},
		"a reviewer's token":  {"/runs/r1/reviewers/1", "member-token"},
		"another run's token": {"/runs/r1/reviewers/1", "tok2"},
		"run with no token":   {"/runs/r0/reviewers/1", "tok"},
		"unknown run":         {"/runs/zz/reviewers/1", "tok"},
		"bad token, bad n":    {"/runs/r1/reviewers/9", "other"},
	} {
		if rec := do(mux, http.MethodGet, c.path, c.token, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d; want 401", name, rec.Code)
		}
	}
	// A 404 only to the authenticated judge run.
	for name, path := range map[string]string{
		"zero": "/runs/r1/reviewers/0", "six": "/runs/r1/reviewers/6", "letter": "/runs/r1/reviewers/x",
		"signed": "/runs/r1/reviewers/+1", "padded": "/runs/r1/reviewers/01",
	} {
		if rec := do(mux, http.MethodGet, path, "tok", nil); rec.Code != http.StatusNotFound || rec.Header().Get(NoResultHeader) != "" {
			t.Errorf("%s = %d %v; want 404 with no mark", name, rec.Code, rec.Header())
		}
	}
	delete(f.results, "r1/2")
	// Only the route's own "no result" carries the mark.
	if rec := do(mux, http.MethodGet, "/runs/r1/reviewers/2", "tok", nil); rec.Code != http.StatusNotFound ||
		rec.Header().Get("Pergent-Result") != "none" {
		t.Errorf("no result = %d %v; want 404 with Pergent-Result: none", rec.Code, rec.Header())
	}
	// The mark never goes with a 200 or a 401.
	if rec := do(mux, http.MethodGet, "/runs/r1/reviewers/1", "tok", nil); rec.Header().Get(NoResultHeader) != "" {
		t.Errorf("200 has the mark: %v", rec.Header())
	}
	if rec := do(mux, http.MethodGet, "/runs/r1/reviewers/2", "other", nil); rec.Header().Get(NoResultHeader) != "" {
		t.Errorf("401 has the mark: %v", rec.Header())
	}
	if rec := do(mux, http.MethodPost, "/runs/r1/reviewers/1", "tok", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", rec.Code)
	}
}

// A store error is a 500 with no detail; the log has neither the token nor a
// result body, and a good answer logs nothing of them either.
func TestReviewersHandlerNeverLogs(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	f := &fakeReviewers{tokens: map[string]string{"r1": "secret-token"}, err: errors.New("db down"), results: map[string][]byte{"r1/1": []byte("SECRETBODY")}}
	rec := do(reviewersMux(f, log), http.MethodGet, "/runs/r1/reviewers/1", "secret-token", nil)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "db down") {
		t.Errorf("error = %d %q", rec.Code, rec.Body.String())
	}
	f.err = nil
	do(reviewersMux(f, log), http.MethodGet, "/runs/r1/reviewers/1", "secret-token", nil)
	if strings.Contains(logs.String(), "secret-token") || strings.Contains(logs.String(), "SECRETBODY") {
		t.Errorf("log leaks: %s", logs.String())
	}
}
