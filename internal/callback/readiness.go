package callback

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gopaytech/pergent.sh/internal/readiness"
)

// ScanResults is the part of the engine the scan handler needs.
type ScanResults interface {
	CompleteScan(ctx context.Context, id, token string, body []byte) (readiness.ScanOutcome, error)
	ScanTooLarge(ctx context.Context, id, token string) error
}

// ScanHandler serves POST /readiness-scans/{id}/result: what a readiness scan
// Job found in a repository, with its one-time bearer token. A missing or
// wrong token is 401, a scan that has ended 409 and a body over
// readiness.MaxResultBytes 413; only a 204 recorded anything.
type ScanHandler struct {
	Scans ScanResults
	Log   *slog.Logger
}

func (h *ScanHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	oneShot{
		noun: "readiness scan", idKey: "scan", limit: readiness.MaxResultBytes,
		complete: func(ctx context.Context, id, token string, body []byte) (oneShotOutcome, error) {
			outcome, err := h.Scans.CompleteScan(ctx, id, token, body)
			switch {
			case err != nil:
				return 0, err
			case outcome == readiness.ScanStored:
				return oneShotStored, nil
			case outcome == readiness.ScanConflict:
				return oneShotConflict, nil
			}
			return oneShotUnauthorized, nil
		},
		tooLarge: func(ctx context.Context, id, token string) error {
			return h.Scans.ScanTooLarge(ctx, id, token)
		},
		log: h.log(),
	}.serve(w, r)
}

func (h *ScanHandler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}
