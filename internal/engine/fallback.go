package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

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

// fallBack moves a review to the next entry of its chain when the provider
// said its entry is out of quota or credit, or rejected its key: the entry
// is marked (until the cooldown ends, or for auth until its key is edited),
// the run closes as fell_back and the next attempt is queued, in one
// transaction, and the shared status says it retries. Nothing else is posted
// for the dropped attempt. It acts only on a merge request's review that ran
// on an entry of a chain of at least 2 (a 1-entry chain never falls back),
// and handled is true only then: the caller publishes the run otherwise. An
// API review is marked but not retried, since its inputs are gone once its
// Job has them; it is published as before. A review panel's merge (Job P)
// falls back by its judges instead (fallBackJudge).
func (e *Engine) fallBack(ctx context.Context, run store.Run, log *slog.Logger) (handled bool, err error) {
	if pe := providerError(run.Result); pe != nil && run.Kind == store.KindMR {
		merge, err := e.isPanelMerge(ctx, run)
		if err != nil {
			return false, err
		}
		if merge {
			return e.fallBackJudge(ctx, run, pe.Role, log)
		}
	}
	m, ok := e.refusedMark(store.EntryRef{Source: run.ModelSource, ID: run.ModelEntryID}, run.ChainLength, run.Result)
	if !ok {
		return false, nil
	}
	log = log.With("model_entry", run.ModelEntryID, "class", m.Class)
	if run.Kind != store.KindMR {
		if err := e.Store.SetMark(ctx, m); err != nil {
			return false, fmt.Errorf("mark the model entry: %w", err)
		}
		// The mark is set again on a publish retry (the same upsert); the
		// audit entry is written on the first try only.
		if run.PublishError == "" {
			log.Warn("the provider refused the review's model entry; it is skipped until the mark ends")
			e.auditMark(ctx, run, run.ModelEntryName, m.Class, log)
		}
		return false, nil
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

// isPanelMerge reports whether run is a review panel's merge (Job P): its
// review has a member that is not off.
func (e *Engine) isPanelMerge(ctx context.Context, run store.Run) (bool, error) {
	ms, err := e.Store.PanelMembers(ctx, run.ID)
	if err != nil {
		return false, fmt.Errorf("read the review's panel members: %w", err)
	}
	for _, m := range ms {
		if m.State != store.MemberOff {
			return true, nil
		}
	}
	return false, nil
}

// fallBackJudge moves the judge of role ("a" or "b") of Job P run to the
// next entry of that role's chain when the provider refused its judge
// entry, as fallBack does for a run: the entry is marked and audited, the
// run closes as fell_back and the next attempt of P is queued, in one
// transaction. That attempt picks the role's judge again (resolveMerge);
// the members are not touched. It acts only on a role that judged with an
// entry of a chain of at least 2. A provider error with no role is not a
// judge's: the run is published as it is. When the role has no entry left,
// the next attempt runs without that judge and pergent's merge keeps its
// findings. Nothing is posted for the dropped attempt: the review's status
// already says it runs.
func (e *Engine) fallBackJudge(ctx context.Context, run store.Run, role string, log *slog.Logger) (handled bool, err error) {
	if role == "" {
		return false, nil
	}
	js, err := e.Store.PanelJudges(ctx, run.ID)
	if err != nil {
		return false, fmt.Errorf("read the review panel's judges: %w", err)
	}
	j, ok := js[role]
	if !ok {
		return false, nil
	}
	m, ok := e.refusedMark(store.EntryRef{Source: j.Source, ID: j.ID}, j.Length, run.Result)
	if !ok {
		return false, nil
	}
	log = log.With("role", role, "model_entry", j.ID, "class", m.Class)
	next, created, err := e.Store.EnqueueAttempt(ctx, run, m)
	if err != nil {
		return false, fmt.Errorf("queue the next attempt: %w", err)
	}
	if !created {
		log.Info("not falling back: the run is no longer waiting to be published")
		return true, nil
	}
	log.Warn("the provider refused the review panel's judge entry; the merge runs again on the next entry of its chain", "next_run", next)
	e.auditMark(ctx, run, j.Name, m.Class, log)
	e.Wake()
	return true, nil
}

// refusedMark is the mark of the entry ref when the stored result says the
// provider refused it: out of quota or credit, or its key rejected. The mark
// ends after the cooldown, or for auth when its key is edited. ok is false
// when nothing falls back: no entry, a chain of 1 (it never falls back), no
// provider_error, or a plain rate limit or a class this release doesn't
// know. A review and a panel member decide it the same way.
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
