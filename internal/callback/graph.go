package callback

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/gopaytech/pergent/graph"

	"github.com/gopaytech/pergent.sh/internal/codegraph"
)

// maxGraphBody is the largest post of a graph build Job: a pergent-graph/v1
// file is at most graph.MaxBytes. A variable, so a test can lower it.
var maxGraphBody int64 = graph.MaxBytes

// GraphResults is the part of the engine the graph routes need.
type GraphResults interface {
	CompleteGraph(ctx context.Context, id, token string, body []byte) (codegraph.Outcome, error)
	GraphTooLarge(ctx context.Context, id, token string) error
	RunGraph(ctx context.Context, runID, token string) (*os.File, error)
}

// GraphHandler serves POST /graphs/{id}/result: what a graph build Job
// posted, with its one-time bearer token. A missing or wrong token is 401 and
// a body over the limit 413; a 204 recorded the graph or the build's failure.
type GraphHandler struct {
	Graphs GraphResults
	Log    *slog.Logger
}

func (h *GraphHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	oneShot{
		noun: "code graph", idKey: "build", limit: maxGraphBody,
		complete: func(ctx context.Context, id, token string, body []byte) (oneShotOutcome, error) {
			outcome, err := h.Graphs.CompleteGraph(ctx, id, token, body)
			switch {
			case err != nil:
				return 0, err
			case outcome == codegraph.Stored:
				return oneShotStored, nil
			}
			return oneShotUnauthorized, nil
		},
		tooLarge: func(ctx context.Context, id, token string) error {
			return h.Graphs.GraphTooLarge(ctx, id, token)
		},
		log: logOr(h.Log),
	}.serve(w, r)
}

// RunGraphHandler serves GET /runs/{id}/graph: the code graph a review Job
// downloads with its run's result token. A missing or wrong token, or a run
// with no graph, is 404, so the answer never tells whether a graph exists.
// The token is not spent.
type RunGraphHandler struct {
	Graphs GraphResults
	Log    *slog.Logger
}

func (h *RunGraphHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := h.Graphs.RunGraph(r.Context(), id, token)
	switch {
	case errors.Is(err, codegraph.ErrNoGraph):
		http.NotFound(w, r)
		return
	case err != nil:
		logOr(h.Log).Error("code graph: serving a review's graph failed", "run", id, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/json")
	if st, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func logOr(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.Default()
}
