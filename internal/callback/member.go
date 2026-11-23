package callback

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/usage"
)

// MemberResults is the part of the store the member handler needs.
type MemberResults interface {
	MultiReviewRun(ctx context.Context, root string) (store.Run, bool, error)
	StoreReviewMemberResult(ctx context.Context, runID, id, token string, body []byte, now time.Time) (store.ResultOutcome, error)
	ReportReviewMemberFailure(ctx context.Context, id string, f store.Failure, now time.Time) (bool, error)
	ReplaceReviewMemberWithFailure(ctx context.Context, id string, f store.Failure) error
	RecordReviewMemberUsage(ctx context.Context, id string, skills []store.SkillUsage) error
}

// MemberHandler serves POST /runs/{id}/members/{member}/result: a multi
// review's reviewer Job posts its result with the member's own token. The body is
// checked as a review Job's is (checkResult), with the same size cap and
// answers. A result that can't be used fails the member, never the run, and
// nothing is ever published from here. A member that is not the run's, an
// unknown one and a bad token get the same 401, so the answer never tells
// whether a member exists. The body and the token are never logged.
type MemberHandler struct {
	Members MemberResults
	Now     func() time.Time
	// Wake nudges the engine after a result or failure was recorded. May be nil.
	Wake func()
	Log  *slog.Logger
	// Background, when set, counts the usage writes that outlive a request,
	// as Handler.Background does. Nil: the handler counts them itself.
	Background *sync.WaitGroup

	// usageWork counts the usage writes still running, so a test can wait.
	usageWork sync.WaitGroup
}

func (h *MemberHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID, id := r.PathValue("id"), r.PathValue("member")
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "result too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read the body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	log := h.log().With("run", runID, "member", id)
	now := h.now()
	// The token is checked first, on the raw body, and with it that the
	// member is the run's: its Job's URL names the run the member belongs
	// to (StoreReviewMemberResult). Every refusal takes the same steps there.
	outcome, err := h.Members.StoreReviewMemberResult(ctx, runID, id, token, body, now)
	switch {
	case errors.Is(err, store.ErrInvalidResult):
		h.fail(w, ctx, log, id, now, "the body is not valid JSON")
		return
	case err != nil:
		log.Error("store member result", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	switch outcome {
	case store.ResultUnauthorized:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case store.ResultConflict:
		http.Error(w, "result already received", http.StatusConflict)
	case store.ResultDiscarded:
		w.WriteHeader(http.StatusOK)
	case store.ResultStored:
		h.stored(w, ctx, log, runID, id, body)
	default:
		log.Error("unknown result outcome", "outcome", int(outcome))
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// stored checks a member result that was just stored and turns an unusable
// one into the member's failure.
func (h *MemberHandler) stored(w http.ResponseWriter, ctx context.Context, log *slog.Logger, runID, id string, body []byte) {
	reason := ""
	var run store.Run
	if res, err := result.Parse(body); err != nil {
		reason = err.Error()
	} else {
		var found bool
		run, found, err = h.Members.MultiReviewRun(ctx, runID)
		if err != nil {
			log.Error("read the review's run", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !found {
			// A push or a close took the run after the result was stored:
			// the sweep supersedes the member, and its result is never used.
			log.Info("the review's run no longer waits; the member's result is left to the sweep")
			w.WriteHeader(http.StatusOK)
			return
		}
		reason = checkResult(res, run)
	}
	if reason != "" {
		// ErrNotReported: the engine's settle pass ran the same check on the
		// stored result first and already failed the member.
		if err := h.Members.ReplaceReviewMemberWithFailure(ctx, id, unusableFailure(reason)); err != nil && !errors.Is(err, store.ErrNotReported) {
			log.Error("replace member result with failure", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		log.Warn("unusable member result", "reason", reason)
		h.wake()
		http.Error(w, "unusable result: "+reason, http.StatusBadRequest)
		return
	}
	h.wake()
	h.recordUsageLater(ctx, log, id, body, run)
	w.WriteHeader(http.StatusOK)
}

// recordUsageLater writes what the member's skills used (the skills run
// ran), as Handler.recordUsageLater does for a run: after the answer, outside
// the request, never changing the member. A result that settles as a
// failure (an error result, a provider's error) still records the tokens it
// reported. A write lost to a restart is picked up by the fill at the next
// start (usage.BackfillReviewMembers).
func (h *MemberHandler) recordUsageLater(ctx context.Context, log *slog.Logger, id string, body []byte, run store.Run) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageTimeout)
	work := backgroundOr(h.Background, &h.usageWork)
	work.Add(1)
	go func() {
		defer work.Done()
		defer cancel()
		defer func() {
			if p := recover(); p != nil {
				log.Error("recording a member result's usage panicked", "panic", p)
			}
		}()
		skills, err := usage.FromResult(body, run)
		if err != nil {
			// Marked all the same, so it isn't read again later.
			log.Warn("couldn't read the member result's usage", "error", err.Error())
			skills = nil
		}
		if err := h.Members.RecordReviewMemberUsage(ctx, id, skills); err != nil {
			log.Warn("couldn't record the member result's usage", "error", err.Error())
		}
	}()
}

// fail reports the member failed with an unusable result.
func (h *MemberHandler) fail(w http.ResponseWriter, ctx context.Context, log *slog.Logger, id string, now time.Time, reason string) {
	if _, err := h.Members.ReportReviewMemberFailure(ctx, id, unusableFailure(reason), now); err != nil {
		log.Error("report member failure", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Warn("unusable member result", "reason", reason)
	h.wake()
	http.Error(w, "unusable result: "+reason, http.StatusBadRequest)
}

func (h *MemberHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *MemberHandler) wake() {
	if h.Wake != nil {
		h.Wake()
	}
}

func (h *MemberHandler) log() *slog.Logger { return logOr(h.Log) }
