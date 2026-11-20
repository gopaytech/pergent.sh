package callback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/codegraph"
)

// fakeGraphs takes one build's post with one token, and serves one run's
// graph file with another.
type fakeGraphs struct {
	buildToken, runToken, file string
	err                        error
	posted                     [][]byte
	tooLarge                   int
}

func (f *fakeGraphs) CompleteGraph(_ context.Context, _, token string, body []byte) (codegraph.Outcome, error) {
	switch {
	case f.err != nil:
		return 0, f.err
	case token != f.buildToken:
		return codegraph.Unauthorized, nil
	}
	f.posted = append(f.posted, body)
	return codegraph.Stored, nil
}

func (f *fakeGraphs) GraphTooLarge(_ context.Context, _, token string) error {
	if token == f.buildToken {
		f.tooLarge++
	}
	return nil
}

func (f *fakeGraphs) RunGraph(_ context.Context, _, token string) (*os.File, error) {
	if f.err != nil {
		return nil, f.err
	}
	if token != f.runToken {
		return nil, codegraph.ErrNoGraph
	}
	return os.Open(f.file)
}

func graphMux(f *fakeGraphs) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("POST /graphs/{id}/result", &GraphHandler{Graphs: f})
	mux.Handle("GET /runs/{id}/graph", &RunGraphHandler{Graphs: f})
	return mux
}

func do(mux http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// A build's post: no or a wrong token is 401, the build's is 204, a body
// over the limit is 413 and fails the build, an error is 500 with no text.
func TestGraphCallback(t *testing.T) {
	f := &fakeGraphs{buildToken: "build"}
	mux := graphMux(f)
	if rec := do(mux, http.MethodPost, "/graphs/b1/result", "", []byte("{}")); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d", rec.Code)
	}
	if rec := do(mux, http.MethodPost, "/graphs/b1/result", "other", []byte("{}")); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", rec.Code)
	}
	if rec := do(mux, http.MethodPost, "/graphs/b1/result", "build", []byte(`{"error":"x"}`)); rec.Code != http.StatusNoContent || len(f.posted) != 1 {
		t.Errorf("post = %d, %d posted", rec.Code, len(f.posted))
	}
	old := maxGraphBody
	maxGraphBody = 8
	t.Cleanup(func() { maxGraphBody = old })
	if rec := do(mux, http.MethodPost, "/graphs/b1/result", "build", []byte(`{"error":"too long"}`)); rec.Code != http.StatusRequestEntityTooLarge || f.tooLarge != 1 {
		t.Errorf("too large = %d, %d failed", rec.Code, f.tooLarge)
	}
	maxGraphBody = old
	f.err = errors.New("database down: secret detail")
	if rec := do(mux, http.MethodPost, "/graphs/b1/result", "build", []byte("{}")); rec.Code != http.StatusInternalServerError ||
		strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("error = %d %q", rec.Code, rec.Body.String())
	}
}

// A review's download: the run's token gets the file as JSON; no token or
// another is 404 (never a hint that a graph exists); a POST is 405.
func TestRunGraphHandler(t *testing.T) {
	file := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(file, []byte(`{"format":"pergent-graph/v1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeGraphs{runToken: "run", file: file}
	mux := graphMux(f)
	rec := do(mux, http.MethodGet, "/runs/r1/graph", "run", nil)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || string(body) != `{"format":"pergent-graph/v1"}`+"\n" ||
		rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("download = %d %q %q", rec.Code, body, rec.Header().Get("Content-Type"))
	}
	for _, token := range []string{"", "other"} {
		if rec := do(mux, http.MethodGet, "/runs/r1/graph", token, nil); rec.Code != http.StatusNotFound {
			t.Errorf("token %q = %d; want 404", token, rec.Code)
		}
	}
	if rec := do(mux, http.MethodPost, "/runs/r1/graph", "run", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", rec.Code)
	}
	f.err = errors.New("boom")
	if rec := do(mux, http.MethodGet, "/runs/r1/graph", "run", nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("error = %d", rec.Code)
	}
}
