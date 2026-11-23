package callback

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
)

// ReviewerResults is the part of the store the reviewer route needs.
type ReviewerResults interface {
	// ReviewerResultAuth is the result of reviewer n of run runID's review
	// for the run's live result token (store.ReviewerResultAuth): its newest
	// attempt that is done, or failed with its result kept. authed is false
	// when the token is not the live token of a running run; found is true
	// only when authed is true and the reviewer has a result.
	ReviewerResultAuth(ctx context.Context, runID, token string, n int) (body []byte, authed, found bool, err error)
}

// NoResultHeader and NoResultValue mark the reviewers route's own "this
// reviewer has no result" 404. get-reviewers trusts only a 404 with this mark.
const (
	NoResultHeader = "Pergent-Result"
	NoResultValue  = "none"
)

// ReviewersHandler serves GET /runs/{id}/reviewers/{n}: the judge Job of a
// multi review reads reviewer n's result with its own result token. The
// store checks that the token is the live token of run id, and it reads only
// the members of that run's own review. The answers:
//   - 401: no token, a wrong or expired token, a run with no stored token
//     yet, or an unknown run. All look the same, so the answer never shows
//     whether a run exists;
//   - 404 with the header NoResultHeader: NoResultValue: only to the run's
//     own judge Job, for a reviewer that has no result;
//   - 404 with no mark: n is not 1 to 5, as the other routes answer a bad
//     path value;
//   - 200: the result.
//
// The judge Job reads only a marked 404 as "no result". Any other 404 (a
// pergent.sh with no such route, a proxy's default answer) fails the judge
// Job, so a lost reviewer never becomes a pass. An auth failure must never be
// a 404. The token is not spent. The body and the token are never logged.
type ReviewersHandler struct {
	Reviewers ReviewerResults
	Log       *slog.Logger
}

func (h *ReviewersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	raw := r.PathValue("n")
	n, err := strconv.Atoi(raw)
	// Only the plain digits 1 to 5: no sign, no leading zero. The token is
	// still checked, so a wrong token is a 401 for any n.
	valid := err == nil && n >= 1 && n <= 5 && strconv.Itoa(n) == raw
	if !valid {
		n = 0
	}
	body, authed, found, err := h.Reviewers.ReviewerResultAuth(r.Context(), id, token, n)
	if err != nil {
		logOr(h.Log).Error("reviewers: serving a reviewer result failed", "run", id, "reviewer", n, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !authed {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !valid {
		http.NotFound(w, r)
		return
	}
	if !found {
		w.Header().Set(NoResultHeader, NoResultValue)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
