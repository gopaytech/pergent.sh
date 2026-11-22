package chain

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Reviewers is what a split reads: reviewer 1 (chain A, as Load), the extra
// reviewers in order, and the judge chain (Judge.Refs nil: none).
type Reviewers struct {
	One   Input
	Extra []Input
	Judge Input
}

// LoadReviewers reads chain A as Load does, and the repository's stored
// reviewers 2 to 5 and judge (store.Reviewers) with the same entries, marks
// and owner team. An API review has none: Extra and Judge are empty.
func LoadReviewers(ctx context.Context, st *store.Store, reg *instances.Registry, now time.Time, run store.Run, repo settings.Effective) (Reviewers, error) {
	one, err := Load(ctx, st, reg, now, run, repo)
	if err != nil {
		return Reviewers{}, err
	}
	r := Reviewers{One: one, Judge: blank(one)}
	if run.Kind == store.KindAPI {
		return r, nil
	}
	stored, err := st.Reviewers(ctx, repo.Instance, repo.Project)
	if err != nil {
		return Reviewers{}, fmt.Errorf("read the repository's reviewers: %w", err)
	}
	for _, c := range stored.Extra {
		r.Extra = append(r.Extra, withChain(blank(one), c))
	}
	if stored.JudgeFound {
		r.Judge = withChain(r.Judge, stored.Judge)
	}
	return r, nil
}

// blank is base's entries, marks, owner team and now, with no chain, no
// tried entry, no avoided model and no instance quota. Its maps are copies,
// so a caller that edits one Input changes no other.
func blank(base Input) Input {
	return Input{Entries: maps.Clone(base.Entries), Marks: maps.Clone(base.Marks), OwnerTeam: base.OwnerTeam, Now: base.Now}
}

// withChain is in with the stored chain c of a repository. A chain with no
// entries leaves Refs nil.
func withChain(in Input, c store.Chain) Input {
	in.Scope, in.SavedByAdmin = store.ScopeRepo, c.SavedByAdmin
	if len(c.Entries) > 0 {
		in.Refs = c.Entries
	}
	return in
}

// Freeze is the copy of in's chain a split stores: each ref with the name,
// provider and model it has now ("" for a removed one). Never a key or a
// base URL.
func Freeze(in Input) store.FrozenChain {
	fc := store.FrozenChain{Scope: in.Scope, SavedByAdmin: in.SavedByAdmin, Entries: make([]store.FrozenEntry, 0, len(in.Refs))}
	for _, ref := range in.Refs {
		e := store.FrozenEntry{Source: ref.Source, ID: ref.ID}
		if m, ok := in.Entries[ref]; ok {
			e.Name, e.Provider, e.Model = m.Name, m.LLM.Provider, m.LLM.Model
		}
		fc.Entries = append(fc.Entries, e)
	}
	return fc
}

// FromFrozen is base (entries, marks, owner team, now) with fc's chain:
// its refs, scope and saved_by_admin. A ref no longer in base.Entries is
// skipped as removed. Tried, Avoid and InstanceQuotaUsedUp start empty: the
// caller sets them.
func FromFrozen(fc store.FrozenChain, base Input) Input {
	in := blank(base)
	in.Scope, in.SavedByAdmin = fc.Scope, fc.SavedByAdmin
	for _, e := range fc.Entries {
		in.Refs = append(in.Refs, store.EntryRef{Source: e.Source, ID: e.ID})
	}
	return in
}
