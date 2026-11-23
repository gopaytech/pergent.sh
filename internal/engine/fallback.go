package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/gopaytech/pergent.sh/internal/callback"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// fallbackActor is the audit actor of a mark pergent.sh set itself.
const fallbackActor = "pergent"

// providerError is the provider_error of a stored result, nil when it has
// none (an older pergent, or no classified error) or can't be read: the
// publisher then handles the result as before.
func providerError(stored []byte) *result.ProviderError {
	if len(stored) == 0 {
		return nil
	}
	var r struct {
		ProviderError *result.ProviderError `json:"provider_error"`
	}
	if json.Unmarshal(stored, &r) != nil {
		return nil
	}
	return r.ProviderError
}

// roleJudge is the provider_error role of a multi review's judge.
const roleJudge = "judge"

// fallBack moves a review to the next entry of its chain when the provider
// said its entry is out of quota or credit, or rejected its key: the entry
// is marked (until the cooldown ends, or for auth until its key is edited),
// the run closes as fell_back and the next attempt is queued, in one
// transaction, and the shared status says it retries. Nothing else is posted
// for the dropped attempt. It acts only on a merge request's review that ran
// on an entry of a chain of at least 2 (a 1-entry chain never falls back),
// and handled is true only then: the caller publishes the run otherwise. An
// API review is marked but not retried, since its inputs are gone once its
// Job has them; it is published as before (markOnly).
//
// A multi review's judge run (it has a review_judges row) falls back by its
// judge instead (judgeRefused), or runs again with --no-judge (retryJudge).
// A provider error with a role on any other run is a judge's (a legacy Job
// P's), never the run's entry: the run is published as it is.
func (e *Engine) fallBack(ctx context.Context, run store.Run, log *slog.Logger) (handled bool, err error) {
	if run.Kind == store.KindMR {
		j, judged, err := e.Store.JudgeOf(ctx, run.ID)
		if err != nil {
			return false, fmt.Errorf("read the multi review's judge: %w", err)
		}
		if judged {
			return e.judgeRefused(ctx, run, j, log)
		}
	}
	if pe := providerError(run.Result); pe != nil && pe.Role != "" {
		return false, nil
	}
	m, ok := e.refusedMark(store.EntryRef{Source: run.ModelSource, ID: run.ModelEntryID}, run.ChainLength, run.Result)
	if !ok {
		return false, nil
	}
	log = log.With("model_entry", run.ModelEntryID, "class", m.Class)
	if run.Kind != store.KindMR {
		// The audit entry is written on the first try only.
		return false, e.markOnly(ctx, run, run.ModelEntryName, m, run.PublishError == "", log)
	}
	next, created, err := e.Store.EnqueueAttempt(ctx, run, m)
	if err != nil {
		return false, fmt.Errorf("queue the next attempt: %w", err)
	}
	if !created {
		// Superseded by a push meanwhile, or this result was handled
		// already: there is nothing to post for it.
		log.Info("not falling back: the run is no longer waiting to be published")
		return true, nil
	}
	log.Warn("the provider refused the review's model entry; trying the next entry of its chain", "next_run", next)
	e.auditMark(ctx, run, run.ModelEntryName, m.Class, log)
	e.Wake()
	root := run.ID
	if run.AttemptOf != nil {
		root = *run.AttemptOf
	}
	description := runningDescription
	if attempts, err := e.Store.Attempts(ctx, root); err != nil {
		log.Warn("couldn't count the review's attempts", "error", e.redact(err.Error()))
	} else if n := len(attempts); n <= run.ChainLength {
		description = fmt.Sprintf(backupDescription, n, run.ChainLength)
	}
	// Best effort: the next attempt sets the running status again.
	if plat, ok := e.Platforms.Get(run.MR.Instance); ok && plat.Forge != nil {
		if err := e.setStatus(ctx, plat, run, forge.StatusRunning, description); err != nil {
			log.Warn("couldn't set the running commit status", "error", e.redact(err.Error()))
		}
	}
	return true, nil
}

// judgeRefused acts on the reported judge run, which judged with j. When
// the provider refused the judge entry (provider_error.role "judge") and the
// frozen judge chain has 2 entries or more, the entry is marked and audited,
// the run closes as fell_back and the review's next attempt is queued, in
// one transaction (EnqueueAttempt): that attempt resolves the judge again,
// skipping the entry (resolveJudge). The members are not touched, and
// nothing is posted: the review's status already says it runs. With a
// 1-entry chain the CLI did the exact merge, and its result is published
// (D7). Any other judge run goes on to retryJudge.
func (e *Engine) judgeRefused(ctx context.Context, run store.Run, j store.JudgeRow, log *slog.Logger) (handled bool, err error) {
	pe := providerError(run.Result)
	if pe == nil || pe.Role != roleJudge || j.NoJudge {
		return e.retryJudge(ctx, run, log)
	}
	m, ok := e.refusedMark(store.EntryRef{Source: j.Entry.Source, ID: j.Entry.ID}, j.Entry.Length, run.Result)
	if !ok {
		return e.retryJudge(ctx, run, log)
	}
	log = log.With("model_entry", j.Entry.ID, "class", m.Class)
	next, created, err := e.Store.EnqueueAttempt(ctx, run, m)
	if err != nil {
		return false, fmt.Errorf("queue the next attempt: %w", err)
	}
	if !created {
		log.Info("not falling back: the run is no longer waiting to be published")
		return true, nil
	}
	log.Warn("the provider refused the multi review's judge entry; the judge runs again on the next entry of its chain", "next_run", next)
	e.auditMark(ctx, run, j.Entry.Name, m.Class, log)
	e.Wake()
	return true, nil
}

// retryJudge acts on a reported judge run with no usable result (no result,
// a result whose status is not completed, or one that fails its checks)
// whose review_judges row has no_judge false: EnqueueJudgeRetry. handled is
// false otherwise, and the run is published as it is.
//
// The retry closes the run as fell_back with reason judge_failed and queues
// the review's next attempt, whose judge runs --no-judge (resolveJudge): the
// CLI's exact merge keeps every blocking finding. When that Job also gives
// no usable result, the review is published as failed. Nothing is posted
// for the dropped attempt.
func (e *Engine) retryJudge(ctx context.Context, run store.Run, log *slog.Logger) (handled bool, err error) {
	j, found, err := e.Store.JudgeOf(ctx, run.ID)
	if err != nil {
		return false, fmt.Errorf("read the multi review's judge: %w", err)
	}
	if !found || j.NoJudge || judgeResultUsable(run) {
		return false, nil
	}
	next, created, err := e.Store.EnqueueJudgeRetry(ctx, run)
	if err != nil {
		return false, fmt.Errorf("queue the judge's retry: %w", err)
	}
	if !created {
		log.Info("not retrying the judge: the run is no longer waiting to be published")
		return true, nil
	}
	log.Warn("the judge Job gave no usable result; the review runs again with no judge", "next_run", next,
		"failure", e.redact(run.FailureReason))
	e.Wake()
	return true, nil
}

// judgeResultUsable reports whether the judge run has a result the publish
// can take: stored, completed, and passing the checks its handler made
// (callback.CheckStored).
func judgeResultUsable(run store.Run) bool {
	if run.Result == nil {
		return false
	}
	res, _, ok := callback.CheckStored(run.Result, run)
	return ok && res.Status == "completed"
}

// markOnly marks the entry the provider refused, with no next attempt: the
// rule of an API review, whose inputs are gone once its Job has them, and of
// a member of one. The mark is set again on a retry (the same
// upsert); with audit the audit entry is written too.
func (e *Engine) markOnly(ctx context.Context, run store.Run, entryName string, m store.Mark, audit bool, log *slog.Logger) error {
	if err := e.Store.SetMark(ctx, m); err != nil {
		return fmt.Errorf("mark the model entry: %w", err)
	}
	if audit {
		log.Warn("the provider refused the review's model entry; it is skipped until the mark ends")
		e.auditMark(ctx, run, entryName, m.Class, log)
	}
	return nil
}

// refusedMark is the mark of the entry ref when the stored result says the
// provider refused it: out of quota or credit, or its key rejected. The mark
// ends after the cooldown, or for auth when its key is edited. ok is false
// when nothing falls back: no entry, a chain of 1 (it never falls back), no
// provider_error, or a plain rate limit or a class this release doesn't
// know. A review and a multi review's member decide it the same way.
func (e *Engine) refusedMark(ref store.EntryRef, chainLength int, stored []byte) (m store.Mark, ok bool) {
	if ref.ID == "" || chainLength < 2 {
		return store.Mark{}, false
	}
	pe := providerError(stored)
	if pe == nil {
		return store.Mark{}, false
	}
	switch pe.Class {
	case store.ClassQuota, store.ClassBilling, store.ClassAuth:
	default:
		return store.Mark{}, false
	}
	now := e.now()
	m = store.Mark{Ref: ref, Class: pe.Class, SetAt: now}
	if pe.Class != store.ClassAuth {
		until := now.Add(e.Config.Fallback.Cooldown())
		m.Until = &until
	}
	return m, true
}

// auditMark records the mark of an entry of run's review: the entry's name
// and the class, never a key. A failure is logged and changes nothing.
func (e *Engine) auditMark(ctx context.Context, run store.Run, entryName, class string, log *slog.Logger) {
	actx, cancel := settle(ctx)
	defer cancel()
	if err := e.Store.Audit(actx, store.AuditEntry{
		At: e.now(), Actor: fallbackActor, Action: "model_marked",
		Instance: run.MR.Instance, Project: run.ProjectPath,
		Detail: fmt.Sprintf("model=%s class=%s", entryName, class),
	}); err != nil {
		log.Error("recording an audit entry failed", "action", "model_marked", "error", e.redact(err.Error()))
	}
}

// setStatus sets run's status on its head. Where the platform's status can
// be changed in place (forge.StatusUpdater: a GitHub Check Run), the run's
// own is changed, so every attempt of a review shares one: an attempt copies
// the ref. A new ref is stored on the run, best effort: a lost write only
// makes the next call create another.
func (e *Engine) setStatus(ctx context.Context, plat Platform, run store.Run, s forge.StatusState, description string) error {
	su, ok := plat.Forge.(forge.StatusUpdater)
	if !ok {
		return plat.Forge.SetStatus(ctx, run.MR.ProjectID, run.HeadSHA, s, description)
	}
	ref, err := su.SetStatusRef(ctx, run.MR.ProjectID, run.HeadSHA, run.StatusRef, s, description)
	if err != nil {
		return err
	}
	if ref != run.StatusRef {
		if err := e.Store.SetRunStatusRef(ctx, run.ID, ref); err != nil {
			e.log().Warn("couldn't record the run's status ID", "run", run.ID, "error", e.redact(err.Error()))
		}
	}
	return nil
}
