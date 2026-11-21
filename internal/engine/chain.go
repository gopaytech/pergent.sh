package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// backupDescription is the running status of an attempt after the first:
// its number in the review and the length of the chain in effect.
const backupDescription = "retrying on backup model (%d of %d)"

// chainFor loads what chain.Resolve needs to pick run's entry (chain.Load).
// The caller sets InstanceQuotaUsedUp.
func (e *Engine) chainFor(ctx context.Context, run store.Run, repo settings.Effective) (chain.Input, error) {
	return chain.Load(ctx, e.Store, e.registry(), e.now(), run, repo)
}

// chainA is the run's chain A as a panel reads it: the chain in effect
// (chainFor), or for an API client that chose a model, that model alone, a
// chain of 1 that never falls back. A model that is gone leaves the chain
// empty.
func (d *dispatcher) chainA(ctx context.Context, repo settings.Effective) (chain.Input, error) {
	in, err := d.e.chainFor(ctx, d.run, repo)
	if err != nil || d.clientModel == "" {
		return in, err
	}
	in.Scope, in.Refs, in.Tried = store.ScopeGlobal, nil, nil
	if m, ok := d.e.registry().ModelByID(d.clientModel); ok {
		ref := entryRef(m)
		in.Refs, in.Entries[ref] = []store.EntryRef{ref}, m
	}
	return in, nil
}

// entryRef is how a chain names m.
func entryRef(m instances.Model) store.EntryRef { return chain.RefOf(m) }

// resolve picks the run's entry. instanceUsedUp is whether the instance's
// token quota stops global entries. A run somebody asked for (askedFor) is
// never held for it: when only the quota stops its entries, it runs on the
// first global one, as before chains.
func (d *dispatcher) resolve(ctx context.Context, repo settings.Effective, instanceUsedUp, askedFor bool) (chain.Decision, error) {
	in, err := d.e.chainFor(ctx, d.run, repo)
	if err != nil {
		return chain.Decision{}, err
	}
	in.InstanceQuotaUsedUp = instanceUsedUp
	dec := chain.Resolve(in)
	if dec.Hold && askedFor {
		in.InstanceQuotaUsedUp = false
		dec = chain.Resolve(in)
	}
	d.attempt = len(in.Tried) + 1
	d.entry, d.scope = dec, in.Scope
	return dec, nil
}

// failExhausted fails a run no entry of its chain can run: each entry and
// why, never a key. A repository's 1-entry chain is the model it chose
// before chains, and fails as it did then: the reason names no entry, and an
// entry that exists is recorded as the model the run tried, for the
// dashboard (best effort, since the run fails either way).
func (d *dispatcher) failExhausted(ctx context.Context, dec chain.Decision) error {
	skips := make([]string, 0, len(dec.Skips))
	for _, s := range dec.Skips {
		skips = append(skips, s.Ref.Source+"/"+s.Ref.ID+":"+s.Reason)
	}
	if dec.Length != 1 || d.scope != store.ScopeRepo {
		d.log.Warn("no entry of the model chain can be used", "chain", d.scope, "skipped", strings.Join(skips, ","))
		return d.fail(ctx, chain.FailureText(dec))
	}
	ref := dec.Skips[0].Ref
	d.log.Warn("the model chosen for this repository can't be used", "model_id", ref.ID, "reason", dec.Skips[0].Reason)
	if d.isMember() {
		// A member writes nothing to its run.
		return d.fail(ctx, reasonModelUnusable)
	}
	for _, m := range d.e.registry().AllModels() {
		if entryRef(m) == ref {
			if err := d.e.Store.SetRunModel(ctx, d.run.ID, m.LLM.Provider, m.LLM.Model); err != nil {
				d.log.Error("couldn't record the model the failed review tried", "model_id", ref.ID, "error", d.e.redact(err.Error()))
			}
			break
		}
	}
	return d.fail(ctx, reasonModelUnusable)
}

// runEntry is what the run records of the entry it runs on.
func runEntry(dec chain.Decision) store.RunEntry {
	return store.RunEntry{Source: dec.Ref.Source, ID: dec.Ref.ID, Name: dec.Entry.Name, TeamID: dec.Entry.TeamID,
		Position: dec.Position, Length: dec.Length, FallbackClass: dec.FirstSkip}
}

// runningText is the running status of the run: an attempt after the first
// says it is a backup.
func (d *dispatcher) runningText() string {
	if d.run.AttemptOf != nil && d.attempt <= d.entry.Length {
		return fmt.Sprintf(backupDescription, d.attempt, d.entry.Length)
	}
	return runningDescription
}

// resolveB picks member B's entry from chain B (in, from chain.LoadSet): never
// the model A runs on. The instance quota stops B's global entries even for
// a run somebody asked for: they let the review start, not a second model.
func (d *dispatcher) resolveB(in chain.Input, instanceUsedUp bool) chain.Decision {
	in.InstanceQuotaUsedUp = instanceUsedUp
	in.Avoid = chain.SameModel{Provider: d.provider, Model: d.model}
	return chain.Resolve(in)
}
