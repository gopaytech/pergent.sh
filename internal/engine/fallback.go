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
// Job has them; it is published as before.
func (e *Engine) fallBack(ctx context.Context, run store.Run, log *slog.Logger) (handled bool, err error) {
	if run.ModelEntryID == "" || run.ChainLength < 2 {
		return false, nil
	}
	pe := providerError(run.Result)
	if pe == nil {
		return false, nil
	}
	switch pe.Class {
	case store.ClassQuota, store.ClassBilling, store.ClassAuth:
	default:
		// A plain rate limit, or a class this release doesn't know: as
		// before, no fallback.
		return false, nil
	}
	now := e.now()
	m := store.Mark{Ref: store.EntryRef{Source: run.ModelSource, ID: run.ModelEntryID}, Class: pe.Class, SetAt: now}
	if pe.Class != store.ClassAuth {
		until := now.Add(e.Config.Fallback.Cooldown())
		m.Until = &until
	}
	log = log.With("model_entry", run.ModelEntryID, "class", pe.Class)
	if run.Kind != store.KindMR {
		if err := e.Store.SetMark(ctx, m); err != nil {
			return false, fmt.Errorf("mark the model entry: %w", err)
		}
		// The mark is set again on a publish retry (the same upsert); the
		// audit entry is written on the first try only.
		if run.PublishError == "" {
			log.Warn("the provider refused the review's model entry; it is skipped until the mark ends")
			e.auditMark(ctx, run, log)
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
	e.auditMark(ctx, run, log)
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
		if err := plat.Forge.SetStatus(ctx, run.MR.ProjectID, run.HeadSHA, forge.StatusRunning, description); err != nil {
			log.Warn("couldn't set the running commit status", "error", e.redact(err.Error()))
		}
	}
	return true, nil
}

// auditMark records the mark of run's entry: its name and class, never a
// key. A failure is logged and changes nothing.
func (e *Engine) auditMark(ctx context.Context, run store.Run, log *slog.Logger) {
	actx, cancel := settle(ctx)
	defer cancel()
	var class string
	if pe := providerError(run.Result); pe != nil {
		class = pe.Class
	}
	if err := e.Store.Audit(actx, store.AuditEntry{
		At: e.now(), Actor: fallbackActor, Action: "model_marked",
		Instance: run.MR.Instance, Project: run.ProjectPath,
		Detail: fmt.Sprintf("model=%s class=%s", run.ModelEntryName, class),
	}); err != nil {
		log.Error("recording an audit entry failed", "action", "model_marked", "error", e.redact(err.Error()))
	}
}
