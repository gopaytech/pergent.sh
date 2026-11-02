package callback

import (
	"context"
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
	oneShot{
		noun: "skill fetch", idKey: "fetch", limit: skillfetch.MaxFetchBytes,
		complete: func(ctx context.Context, id, token string, body []byte) (oneShotOutcome, error) {
			outcome, err := h.Fetches.CompleteJob(ctx, id, token, body)
			switch {
			case err != nil:
				return 0, err
			case outcome == skillfetch.JobStored:
				return oneShotStored, nil
			case outcome == skillfetch.JobConflict:
				return oneShotConflict, nil
			}
			return oneShotUnauthorized, nil
		},
		tooLarge: func(ctx context.Context, id, token string) error {
			return h.Fetches.JobTooLarge(ctx, id, token)
		},
		log: h.log(),
	}.serve(w, r)
}

func (h *FetchHandler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}
