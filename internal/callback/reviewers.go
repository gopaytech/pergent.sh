package callback

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
)

// ReviewerResults is the part of the store the reviewer route needs.
type ReviewerResults interface {
	// ReviewerResult is the result of reviewer n of run runID's review for
	// the run's live result token (store.ReviewerResult): its newest attempt
	// that is done, or failed with its result kept.
	ReviewerResult(ctx context.Context, runID, token string, n int) ([]byte, bool, error)
}

// ReviewersHandler serves GET /runs/{id}/reviewers/{n}: the judge Job of a
// multi review reads reviewer n's result with its own result token. The
// store checks that the token is the live token of run id, and it reads only
// the members of that run's own review. Every refusal is the same 404 (a bad
// token, n that is not 1 to 5, an unknown run, a reviewer with no result),
// so the answer never tells whether a run or a result exists. The token is
// not spent. The body and the token are never logged.
type ReviewersHandler struct {
	Reviewers ReviewerResults
	Log       *slog.Logger
}

func (h *ReviewersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token, ok := bearer(r.Header.Get("Authorization"))
	n, err := strconv.Atoi(r.PathValue("n"))
	// Only the plain digits 1 to 5: no sign, no leading zero.
	if !ok || err != nil || n < 1 || n > 5 || strconv.Itoa(n) != r.PathValue("n") {
		http.NotFound(w, r)
		return
	}
	body, found, err := h.Reviewers.ReviewerResult(r.Context(), id, token, n)
	if err != nil {
		logOr(h.Log).Error("reviewers: serving a reviewer result failed", "run", id, "reviewer", n, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
