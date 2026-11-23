package chain

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/quota"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Load reads what Resolve needs to pick run's entry: the chain in effect for
// repo, every entry reg knows now, the marks, the owner team and the entries
// earlier attempts of this review ran. The chain is the repository's own
// (custom), else its owner team's, else the global one. An API review has no
// repository and no team: it gets the global chain. The caller sets
// InstanceQuotaUsedUp; the limits are read here. The dispatcher and the requests that start a review
// (re-run, start) read it here, so they apply the same rules.
func Load(ctx context.Context, st *store.Store, reg *instances.Registry, now time.Time, run store.Run, repo settings.Effective) (Input, error) {
	return LoadSet(ctx, st, reg, now, run, repo, store.ChainA)
}

// LoadSet is Load for one chain set. ChainA is Load. ChainB is a review
// panel's second chain: a repository in mode off has none, before its team or
// the global level is read; the global level is the stored chain B only (no
// config entries, no default entry); and no chain at all is Refs nil with
// Scope "", which means no panel. An API review has no repository, so it
// gets the global chain B whatever a repository's mode is. For chain B the
// caller sets Tried (the panel member's earlier attempts), not run.AttemptOf.
// Both carry the limits used up this month (quota.ReadLimits): the model
// limits, and the owner team's limits (none without an owner team, so none
// for an API review). A limit read that fails is the load's error, so no
// review starts unchecked.
func LoadSet(ctx context.Context, st *store.Store, reg *instances.Registry, now time.Time, run store.Run, repo settings.Effective, set store.ChainSet) (Input, error) {
	in, err := loadSet(ctx, st, reg, now, run, repo, set)
	if err != nil {
		return Input{}, err
	}
	if len(in.Refs) == 0 {
		// No entry, nothing a limit could stop (most chain B lists).
		return in, nil
	}
	lim, err := quota.ReadLimits(ctx, st, now, in.OwnerTeam)
	if err != nil {
		return Input{}, fmt.Errorf("read the model limits: %w", err)
	}
	in.ModelLimitUsedUp, in.TeamLimitUsedUp = lim.Model, lim.Team
	return in, nil
}

func loadSet(ctx context.Context, st *store.Store, reg *instances.Registry, now time.Time, run store.Run, repo settings.Effective, set store.ChainSet) (Input, error) {
	in := Input{Entries: map[store.EntryRef]instances.Model{}, Now: now}
	for _, m := range reg.AllModels() {
		in.Entries[RefOf(m)] = m
	}
	marks, err := st.Marks(ctx)
	if err != nil {
		return Input{}, fmt.Errorf("read the marks: %w", err)
	}
	if len(marks) > 0 {
		in.Marks = make(map[store.EntryRef]store.Mark, len(marks))
		for _, m := range marks {
			in.Marks[m.Ref] = m
		}
	}
	if run.AttemptOf != nil && set == store.ChainA {
		attempts, err := st.Attempts(ctx, *run.AttemptOf)
		if err != nil {
			return Input{}, fmt.Errorf("read the review's attempts: %w", err)
		}
		in.Tried = map[store.EntryRef]bool{}
		for _, a := range attempts {
			if a.ID != run.ID && a.ModelEntryID != "" {
				in.Tried[store.EntryRef{Source: a.ModelSource, ID: a.ModelEntryID}] = true
			}
		}
	}
	if run.Kind != store.KindAPI {
		if set == store.ChainB {
			mode, err := st.RepoPanelMode(ctx, repo.Instance, repo.Project)
			if err != nil {
				return Input{}, fmt.Errorf("read the repository's panel mode: %w", err)
			}
			if mode == store.PanelOff {
				return in, nil
			}
		}
		if err := repoChain(ctx, st, repo, set, &in); err != nil {
			return Input{}, err
		}
	}
	if in.Refs == nil {
		if set == store.ChainB {
			c, found, err := st.ChainOf(ctx, set, store.ScopeGlobal, 0, "", "")
			if err != nil {
				return Input{}, fmt.Errorf("read the global chain B: %w", err)
			}
			if found && len(c.Entries) > 0 {
				in.Scope, in.Refs = store.ScopeGlobal, c.Entries
			}
			return in, nil
		}
		in.Scope = store.ScopeGlobal
		if in.Refs, err = GlobalChain(ctx, st, reg); err != nil {
			return Input{}, err
		}
	}
	return in, nil
}

// repoChain sets in's owner team and, when the repository has its own chain
// or its owner team has one, the chain. A chain saved with no entries counts
// as none, so the next level is used.
func repoChain(ctx context.Context, st *store.Store, repo settings.Effective, set store.ChainSet, in *Input) error {
	owner, owned, err := st.OwnerTeam(ctx, repo.Instance, repo.Project)
	if err != nil {
		return fmt.Errorf("read the owner team: %w", err)
	}
	if owned {
		in.OwnerTeam = owner
	}
	c, found, err := st.ChainOf(ctx, set, store.ScopeRepo, 0, repo.Instance, repo.Project)
	if err != nil {
		return fmt.Errorf("read the repository's chain: %w", err)
	}
	switch {
	case found && len(c.Entries) > 0:
		in.Scope, in.Refs, in.SavedByAdmin = store.ScopeRepo, c.Entries, c.SavedByAdmin
		return nil
	case set == store.ChainA && !found && repo.ModelID != "":
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
	c, found, err = st.ChainOf(ctx, set, store.ScopeTeam, owner, "", "")
	if err != nil {
		return fmt.Errorf("read the owner team's chain: %w", err)
	}
	if found && len(c.Entries) > 0 {
		in.Scope, in.Refs = store.ScopeTeam, c.Entries
	}
	return nil
}

// GlobalChain is the global chain: the config's entry and llm.fallbacks
// first while the config has an llm: block, then the stored chain's other
// entries. With no stored chain (or an empty one) it is the config's entries,
// or else the dashboard's default; when none of those has a usable key it is
// empty, so the run waits as it did before chains.
func GlobalChain(ctx context.Context, st *store.Store, reg *instances.Registry) ([]store.EntryRef, error) {
	refs, _, stored, err := GlobalRefs(ctx, st, reg)
	if err != nil || stored {
		return refs, err
	}
	for _, m := range reg.GlobalModels() {
		if m.Key != "" && slices.Contains(refs, RefOf(m)) {
			return refs, nil
		}
	}
	return nil, nil
}

// GlobalRefs is the global chain as it is written, keys or not: the config's
// entries first (fixed of them, which no save moves), then the stored
// chain's others; with no stored chain (stored false) the config's entries,
// or else the dashboard's default, or none.
func GlobalRefs(ctx context.Context, st *store.Store, reg *instances.Registry) (refs []store.EntryRef, fixed int, stored bool, err error) {
	fromConfig := map[string]bool{}
	for _, m := range reg.GlobalModels() {
		if m.Source == instances.SourceConfig {
			refs = append(refs, RefOf(m))
			fromConfig[m.ID] = true
		}
	}
	fixed = len(refs)
	c, found, err := st.Chain(ctx, store.ScopeGlobal, 0, "", "")
	if err != nil {
		return nil, 0, false, fmt.Errorf("read the global chain: %w", err)
	}
	if found && len(c.Entries) > 0 {
		for _, r := range c.Entries {
			if !(r.Source == store.SourceGlobal && fromConfig[r.ID]) {
				refs = append(refs, r)
			}
		}
		return refs, fixed, true, nil
	}
	if refs == nil {
		if m, _ := reg.DefaultModel(); m.ID != "" {
			refs = []store.EntryRef{RefOf(m)}
		}
	}
	return refs, fixed, false, nil
}

// RefOf is how a chain names m.
func RefOf(m instances.Model) store.EntryRef {
	if m.Source == instances.SourceTeam {
		return store.EntryRef{Source: store.SourceTeam, ID: m.ID}
	}
	return store.EntryRef{Source: store.SourceGlobal, ID: m.ID}
}

// Runs reports whether d picked an entry to run.
func (d Decision) Runs() bool { return !d.Wait && !d.Hold && !d.Exhausted }
