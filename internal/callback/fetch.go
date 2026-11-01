package callback

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gopaytech/pergent.sh/internal/skillfetch"
)

// FetchResults is the part of skillfetch.Fetcher the fetch handler needs.
type FetchResults interface {
	CompleteJob(ctx context.Context, id, token string, body []byte) (skillfetch.JobOutcome, error)
	JobTooLarge(ctx context.Context, id, token string) error
}

// FetchHandler serves POST /skill-fetches/{id}/result: the skills a fetch
// Job read from a public URL, with its one-time bearer token. A missing or
// wrong token is 401, a fetch that has ended 409 and a body over
// skillfetch.MaxFetchBytes 413; only a 204 recorded anything.
type FetchHandler struct {
	Fetches FetchResults
	Log     *slog.Logger
}

func (h *FetchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, skillfetch.MaxFetchBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			// The fetch fails at once (with the right token only), rather
			// than when the sweep gives up on it.
			if err := h.Fetches.JobTooLarge(ctx, id, token); err != nil {
				h.log().Error("skill fetch: recording a result that is too large failed", "fetch", id, "error", err.Error())
			}
			http.Error(w, "result too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read the body", http.StatusBadRequest)
		return
	}

	outcome, err := h.Fetches.CompleteJob(ctx, id, token, body)
	if err != nil {
		h.log().Error("skill fetch: recording a Job's result failed", "fetch", id, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	switch outcome {
	case skillfetch.JobStored:
		w.WriteHeader(http.StatusNoContent)
	case skillfetch.JobConflict:
		http.Error(w, "result already received", http.StatusConflict)
	default:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (h *FetchHandler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}
