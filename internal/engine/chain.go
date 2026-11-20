package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// backupDescription is the running status of an attempt after the first:
// its number in the review and the length of the chain in effect.
const backupDescription = "retrying on backup model (%d of %d)"

// chainFor loads what chain.Resolve needs to pick run's entry: the chain in
// effect for repo, every entry known now, the marks, the owner team and the
// entries earlier attempts of this review ran. The chain is the repository's
// own (custom), else its owner team's, else the global one. An API review
// has no repository and no team: it gets the global chain. The caller sets
// InstanceQuotaUsedUp.
func (e *Engine) chainFor(ctx context.Context, run store.Run, repo settings.Effective) (chain.Input, error) {
	reg := e.registry()
	in := chain.Input{Entries: map[store.EntryRef]instances.Model{}, Now: e.now()}
	for _, m := range reg.AllModels() {
		in.Entries[entryRef(m)] = m
	}
	marks, err := e.Store.Marks(ctx)
	if err != nil {
		return chain.Input{}, fmt.Errorf("read the marks: %w", err)
	}
	if len(marks) > 0 {
		in.Marks = make(map[store.EntryRef]store.Mark, len(marks))
		for _, m := range marks {
			in.Marks[m.Ref] = m
		}
	}
	if run.AttemptOf != nil {
		attempts, err := e.Store.Attempts(ctx, *run.AttemptOf)
		if err != nil {
			return chain.Input{}, fmt.Errorf("read the review's attempts: %w", err)
		}
		in.Tried = map[store.EntryRef]bool{}
		for _, a := range attempts {
			if a.ID != run.ID && a.ModelEntryID != "" {
				in.Tried[store.EntryRef{Source: a.ModelSource, ID: a.ModelEntryID}] = true
			}
		}
	}
	if run.Kind != store.KindAPI {
		if err := e.repoChain(ctx, repo, &in); err != nil {
			return chain.Input{}, err
		}
	}
	if in.Refs == nil {
		in.Scope = store.ScopeGlobal
		if in.Refs, err = e.globalChain(ctx); err != nil {
			return chain.Input{}, err
		}
	}
	return in, nil
}

// repoChain sets in's owner team and, when the repository has its own chain
// or its owner team has one, the chain. A chain saved with no entries counts
// as none, so the next level is used.
func (e *Engine) repoChain(ctx context.Context, repo settings.Effective, in *chain.Input) error {
	owner, owned, err := e.Store.OwnerTeam(ctx, repo.Instance, repo.Project)
	if err != nil {
		return fmt.Errorf("read the owner team: %w", err)
	}
	if owned {
		in.OwnerTeam = owner
	}
	c, found, err := e.Store.Chain(ctx, store.ScopeRepo, 0, repo.Instance, repo.Project)
	if err != nil {
		return fmt.Errorf("read the repository's chain: %w", err)
	}
	switch {
	case found && len(c.Entries) > 0:
		in.Scope, in.Refs, in.SavedByAdmin = store.ScopeRepo, c.Entries, c.SavedByAdmin
		return nil
	case !found && repo.ModelID != "":
		// A model set with no chain row: written by the previous release
		// after a rollback. It is the 1-entry chain the upgrade would have
		// made, so the repository keeps the model it asked for; only an admin
		// sets it there.
		in.Scope, in.Refs, in.SavedByAdmin = store.ScopeRepo, []store.EntryRef{{Source: store.SourceGlobal, ID: repo.ModelID}}, true
		return nil
	}
	if !owned {
		return nil
	}
	c, found, err = e.Store.Chain(ctx, store.ScopeTeam, owner, "", "")
	if err != nil {
		return fmt.Errorf("read the owner team's chain: %w", err)
	}
	if found && len(c.Entries) > 0 {
		in.Scope, in.Refs = store.ScopeTeam, c.Entries
	}
	return nil
}

// globalChain is the global chain: the config's entry and llm.fallbacks
// first while the config has an llm: block, then the stored chain's other
// entries. With no stored chain (or an empty one) it is the config's entries,
// or else the dashboard's default; when none of those has a usable key it is
// empty, so the run waits as it did before chains.
func (e *Engine) globalChain(ctx context.Context) ([]store.EntryRef, error) {
	reg := e.registry()
	global := reg.GlobalModels()
	var refs []store.EntryRef
	fromConfig := map[string]bool{}
	for _, m := range global {
		if m.Source == instances.SourceConfig {
			refs = append(refs, entryRef(m))
			fromConfig[m.ID] = true
		}
	}
	c, found, err := e.Store.Chain(ctx, store.ScopeGlobal, 0, "", "")
	if err != nil {
		return nil, fmt.Errorf("read the global chain: %w", err)
	}
	if found && len(c.Entries) > 0 {
		for _, r := range c.Entries {
			if !(r.Source == store.SourceGlobal && fromConfig[r.ID]) {
				refs = append(refs, r)
			}
		}
		return refs, nil
	}
	if refs == nil {
		if m, ok := reg.DefaultModel(); ok {
			refs = []store.EntryRef{entryRef(m)}
		}
	}
	for _, m := range global {
		if m.Key != "" && slices.Contains(refs, entryRef(m)) {
			return refs, nil
		}
	}
	return nil, nil
}

// entryRef is how a chain names m.
func entryRef(m instances.Model) store.EntryRef {
	if m.Source == instances.SourceTeam {
		return store.EntryRef{Source: store.SourceTeam, ID: m.ID}
	}
	return store.EntryRef{Source: store.SourceGlobal, ID: m.ID}
}

// resolve picks the run's entry. instanceUsedUp is whether the instance's
// token quota stops global entries.
func (d *dispatcher) resolve(ctx context.Context, repo settings.Effective, instanceUsedUp bool) (chain.Decision, error) {
	in, err := d.e.chainFor(ctx, d.run, repo)
	if err != nil {
		return chain.Decision{}, err
	}
	in.InstanceQuotaUsedUp = instanceUsedUp
	dec := chain.Resolve(in)
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
