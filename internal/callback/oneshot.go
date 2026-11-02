package callback

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// oneShotOutcome is what became of a result posted with a one-time token.
type oneShotOutcome int

const (
	oneShotStored oneShotOutcome = iota + 1
	oneShotConflict
	oneShotUnauthorized
)

// oneShot is a result callback of a Job that posts once with a one-time bearer
// token (a skill fetch, a readiness scan): the handlers differ only in what
// they record, so the method check, the bearer, the body limit and the
// mapping of the outcome to a status live here.
type oneShot struct {
	// noun and idKey only name the Job in the log ("skill fetch", "fetch").
	noun, idKey string
	// limit is the largest body taken; a bigger one is 413.
	limit int64
	// complete records the body when token is the Job's.
	complete func(ctx context.Context, id, token string, body []byte) (oneShotOutcome, error)
	// tooLarge ends the Job at once, when token is the Job's.
	tooLarge func(ctx context.Context, id, token string) error
	log      *slog.Logger
}

// serve answers 405 for a method but POST, 401 for a missing bearer, 413 for
// a body over the limit (after tooLarge), 500 (with no error text) for an
// error, then 204, 409 or 401 by the outcome. Neither the token nor the body
// is ever logged.
func (o oneShot) serve(w http.ResponseWriter, r *http.Request) {
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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, o.limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			// The Job fails at once (with the right token only), rather
			// than when the sweep gives up on it.
			if err := o.tooLarge(ctx, id, token); err != nil {
				o.log.Error(o.noun+": recording a result that is too large failed", o.idKey, id, "error", err.Error())
			}
			http.Error(w, "result too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read the body", http.StatusBadRequest)
		return
	}

	outcome, err := o.complete(ctx, id, token, body)
	if err != nil {
		o.log.Error(o.noun+": recording a Job's result failed", o.idKey, id, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	switch outcome {
	case oneShotStored:
		w.WriteHeader(http.StatusNoContent)
	case oneShotConflict:
		http.Error(w, "result already received", http.StatusConflict)
	default:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}
