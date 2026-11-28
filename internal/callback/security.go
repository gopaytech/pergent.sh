package callback

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gopaytech/pergent.sh/internal/security"
)

// SecurityResults is the part of the engine the security scan result route
// needs.
type SecurityResults interface {
	CompleteSecurityScan(ctx context.Context, id, token string, body []byte) (security.Outcome, error)
	SecurityTooLarge(ctx context.Context, id, token string) error
}

// SecurityHandler serves POST /security-scans/{id}/result: the envelope a
// security scan Job posts when the scan ends, with the scan's one-time
// bearer token. A missing or wrong token is 401, a scan that is not running
// 409 and a body over security.MaxBodyBytes 413 (the scan fails at once). A
// result pergent.sh can't use is a 204 too, and fails the scan: pergent
// never tries such a result again.
type SecurityHandler struct {
	Scans SecurityResults
	Log   *slog.Logger
}

func (h *SecurityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	oneShot{
		noun: "security scan", idKey: "scan", limit: security.MaxBodyBytes,
		complete: func(ctx context.Context, id, token string, body []byte) (oneShotOutcome, error) {
			outcome, err := h.Scans.CompleteSecurityScan(ctx, id, token, body)
			switch {
			case err != nil:
				return 0, err
			case outcome == security.Stored:
				return oneShotStored, nil
			case outcome == security.Conflict:
				return oneShotConflict, nil
			}
			return oneShotUnauthorized, nil
		},
		tooLarge: func(ctx context.Context, id, token string) error {
			return h.Scans.SecurityTooLarge(ctx, id, token)
		},
		log: logOr(h.Log),
	}.serve(w, r)
}
