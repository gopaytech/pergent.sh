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

type fakeReviewers struct {
	token   string
	results map[string][]byte // by run + "/" + reviewer
	err     error
}

func (f *fakeReviewers) ReviewerResult(_ context.Context, runID, token string, n int) ([]byte, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	b, ok := f.results[runID+"/"+strconv.Itoa(n)]
	if token != f.token || !ok {
		return nil, false, nil
	}
	return b, true, nil
}

func reviewersMux(f *fakeReviewers, log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /runs/{id}/reviewers/{n}", &ReviewersHandler{Reviewers: f, Log: log})
	return mux
}

func TestReviewersHandler(t *testing.T) {
	f := &fakeReviewers{token: "tok", results: map[string][]byte{}}
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
	for name, c := range map[string]struct{ path, token string }{
		"no token":    {"/runs/r1/reviewers/1", ""},
		"bad token":   {"/runs/r1/reviewers/1", "other"},
		"zero":        {"/runs/r1/reviewers/0", "tok"},
		"six":         {"/runs/r1/reviewers/6", "tok"},
		"letter":      {"/runs/r1/reviewers/x", "tok"},
		"signed":      {"/runs/r1/reviewers/+1", "tok"},
		"padded":      {"/runs/r1/reviewers/01", "tok"},
		"unknown run": {"/runs/zz/reviewers/1", "tok"},
	} {
		if rec := do(mux, http.MethodGet, c.path, c.token, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d; want 404", name, rec.Code)
		}
	}
	delete(f.results, "r1/2")
	if rec := do(mux, http.MethodGet, "/runs/r1/reviewers/2", "tok", nil); rec.Code != http.StatusNotFound {
		t.Errorf("no result = %d; want 404", rec.Code)
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
	f := &fakeReviewers{token: "secret-token", err: errors.New("db down"), results: map[string][]byte{"r1/1": []byte("SECRETBODY")}}
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
