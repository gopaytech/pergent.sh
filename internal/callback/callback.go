// Package callback receives the result a review Job posts when it is done.
// The Job holds a one-time bearer token; the token is checked before anything
// else, so a bad token can never fail anyone's run.
package callback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/usage"
)

// maxBodyBytes is a little above the 10 MB cap pergent puts on its result.
const maxBodyBytes = 11 << 20

const unusable = "pergent sent a result pergent.sh couldn't use: "

// usageTimeout bounds writing one result's usage.
const usageTimeout = 30 * time.Second

// Results is the part of the store the handler needs.
type Results interface {
	Run(ctx context.Context, id string) (store.Run, bool, error)
	StoreResult(ctx context.Context, id, token string, body []byte, now time.Time) (store.ResultOutcome, error)
	ReportFailure(ctx context.Context, id string, f store.Failure, now time.Time) (bool, error)
	ReplaceWithFailure(ctx context.Context, id string, f store.Failure) error
	RecordUsage(ctx context.Context, id string, skills []store.SkillUsage) error
}

// Handler serves POST /runs/{id}/result.
type Handler struct {
	Results Results
	Now     func() time.Time
	// Wake nudges the publisher after a result or failure was recorded. May be nil.
	Wake func()
	Log  *slog.Logger

	// usageWork counts the usage writes still running, so a test can wait.
	usageWork sync.WaitGroup
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	now := h.now()
	// The token is checked first, on the raw body.
	outcome, err := h.Results.StoreResult(ctx, id, token, body, now)
	switch {
	case errors.Is(err, store.ErrInvalidResult):
		// The token was good, but the body is not JSON: the run gets a failure.
		h.invalid(w, ctx, id, now, "the body is not valid JSON")
		return
	case err != nil:
		h.log().Error("store result", "run", id, "error", err)
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
		h.stored(w, ctx, id, body)
	default:
		h.log().Error("unknown result outcome", "run", id, "outcome", int(outcome))
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// stored checks a result that was just stored and turns an unusable one into a
// failure, so the run is never published as a pass.
func (h *Handler) stored(w http.ResponseWriter, ctx context.Context, id string, body []byte) {
	reason, err := h.check(ctx, id, body)
	if err != nil {
		h.log().Error("check result", "run", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if reason != "" {
		if err := h.Results.ReplaceWithFailure(ctx, id, store.Failure{Reason: unusable + reason, Step: "delivery"}); err != nil {
			h.log().Error("replace result with failure", "run", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h.log().Warn("unusable result", "run", id, "reason", reason)
		h.wake()
		http.Error(w, "unusable result: "+reason, http.StatusBadRequest)
		return
	}
	h.wake()
	h.recordUsageLater(ctx, id, body)
	w.WriteHeader(http.StatusOK)
}

// recordUsageLater writes the usage without holding up publishing or the
// answer to the Job. It outlives the request, so a Job that hangs up once it
// has its answer doesn't lose it; a write lost to a restart is picked up by
// the background fill at the next start.
func (h *Handler) recordUsageLater(ctx context.Context, id string, body []byte) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageTimeout)
	h.usageWork.Add(1)
	go func() {
		defer h.usageWork.Done()
		defer cancel()
		// Outside the request, net/http no longer stops a panic from
		// ending the process.
		defer func() {
			if p := recover(); p != nil {
				h.log().Error("recording a result's usage panicked", "run", id, "panic", p)
			}
		}()
		h.recordUsage(ctx, id, body)
	}()
}

// recordUsage stores what the accepted result's skills used. Usage is for
// the admin views only: a failure is logged and never changes the review.
func (h *Handler) recordUsage(ctx context.Context, id string, body []byte) {
	run, found, err := h.Results.Run(ctx, id)
	if err != nil || !found {
		h.log().Warn("couldn't record the result's usage", "run", id, "found", found, "error", errText(err))
		return
	}
	skills, err := usage.FromResult(body, run)
	if err != nil {
		// The run is still marked, so it isn't read again later.
		h.log().Warn("couldn't read the result's usage", "run", id, "error", err.Error())
		skills = nil
	}
	if err := h.Results.RecordUsage(ctx, id, skills); err != nil {
		h.log().Warn("couldn't record the result's usage", "run", id, "error", err.Error())
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// check returns why a stored result cannot be used, or "".
func (h *Handler) check(ctx context.Context, id string, body []byte) (string, error) {
	res, err := result.Parse(body)
	if err != nil {
		return err.Error(), nil
	}
	if res.Merged || res.MergeNote != "" {
		// Only pergent.sh marks a result as merged, after it merged it.
		return "the result carries merge fields, which only pergent.sh sets", nil
	}
	run, found, err := h.Results.Run(ctx, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("run %s vanished after its result was stored", id)
	}
	if res.Base != run.BaseSHA || res.Head != run.HeadSHA {
		return "the result is for a different commit than the one this run reviews", nil
	}
	return checkSkills(res, run), nil
}

// checkSkills returns why the skills of a completed result are not those the
// Job was started with, or "". A skill is reported once; a name repeated
// can't stand in for another that is missing. The dispatched skills are the
// run's subset or, for a whole review, the skills recorded at dispatch; a run
// with neither recorded (older, or recording failed) is not compared.
func checkSkills(res *result.Result, run store.Run) string {
	if res.Status != "completed" {
		return ""
	}
	got := map[string]bool{}
	for _, s := range res.Skills {
		if got[s.Name] {
			return "the result reports a skill more than once"
		}
		got[s.Name] = true
	}
	// The linked repositories' check is not one of the skills, unless the
	// repository dispatched a skill of that name: that one is ordinary.
	check := result.IsImpactCheck(result.ImpactSkill, run.SkillSources)
	if check {
		if got[result.ImpactSkill] && len(run.LinkedRepos) == 0 {
			return "the result has skills the Job was not started with"
		}
		delete(got, result.ImpactSkill)
	}
	want := map[string]bool{}
	if len(run.SkillSubset) > 0 {
		for _, n := range run.SkillSubset {
			want[n] = true
		}
	} else if run.SkillSources != nil {
		for n := range run.SkillSources {
			want[n] = true
		}
	} else {
		return ""
	}
	if check {
		delete(want, result.ImpactSkill)
	}
	for n := range want {
		if !got[n] {
			return fmt.Sprintf("the result lacks the skills the Job was started with (%q)", n)
		}
	}
	for n := range got {
		// (The names are the Job's, kept out of the failure text.)
		if !want[n] {
			return "the result has skills the Job was not started with"
		}
	}
	return ""
}

func (h *Handler) invalid(w http.ResponseWriter, ctx context.Context, id string, now time.Time, reason string) {
	if _, err := h.Results.ReportFailure(ctx, id, store.Failure{Reason: unusable + reason, Step: "delivery"}, now); err != nil {
		h.log().Error("report failure", "run", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.log().Warn("unusable result", "run", id, "reason", reason)
	h.wake()
	http.Error(w, "unusable result: "+reason, http.StatusBadRequest)
}

// bearer extracts the token of an "Authorization: Bearer <token>" header.
func bearer(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) wake() {
	if h.Wake != nil {
		h.Wake()
	}
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}
