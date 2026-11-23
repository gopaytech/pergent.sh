package chain

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func (e *panelEnv) saveReviewers(t *testing.T, extra [][]store.EntryRef, judge []store.EntryRef, admin bool) {
	t.Helper()
	if _, err := e.st.SaveReviewers(ctx, store.ReviewersSave{Instance: inst, Project: project, Extra: extra, Judge: judge, SavedByAdmin: admin}, now); err != nil {
		t.Fatalf("SaveReviewers: %v", err)
	}
}

func (e *panelEnv) loadReviewers(t *testing.T, run store.Run) Reviewers {
	t.Helper()
	r, err := LoadReviewers(ctx, e.st, e.reg, now, run, repo)
	if err != nil {
		t.Fatalf("LoadReviewers: %v", err)
	}
	return r
}

// With no stored reviewer, reviewer 1 is Load's input and there is no other
// reviewer and no judge.
func TestLoadReviewersNoneIsOne(t *testing.T) {
	e := newPanelEnv(t, &config.Config{})
	e.save(t, store.ChainA, store.Chain{Scope: store.ScopeTeam, TeamID: e.team, Entries: []store.EntryRef{e.a, e.b}})
	r := e.loadReviewers(t, store.Run{})
	want, err := Load(ctx, e.st, e.reg, now, store.Run{}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.One, want) {
		t.Fatalf("One = %+v\nwant %+v", r.One, want)
	}
	if len(r.Extra) != 0 || r.Judge.Refs != nil {
		t.Fatalf("Extra = %+v, Judge.Refs = %v; want none", r.Extra, r.Judge.Refs)
	}
}

// The stored reviewers come in slot order, then the judge, each with its own
// saved_by_admin and the entries, marks and owner team of reviewer 1.
func TestLoadReviewersOrder(t *testing.T) {
	e := newPanelEnv(t, &config.Config{})
	e.repoRow(t)
	if err := e.st.SetMark(ctx, store.Mark{Ref: e.b, Class: store.ClassQuota, Until: at(time.Hour), SetAt: now}); err != nil {
		t.Fatal(err)
	}
	e.saveReviewers(t, [][]store.EntryRef{{e.a}, {e.b}}, []store.EntryRef{e.a}, true)
	// A maintainer edits reviewer 3 only: reviewer 2 and the judge keep the
	// admin's flag.
	e.saveReviewers(t, [][]store.EntryRef{{e.a}, {e.b, e.a}}, []store.EntryRef{e.a}, false)
	r := e.loadReviewers(t, store.Run{})

	if len(r.Extra) != 2 {
		t.Fatalf("Extra = %+v; want 2", r.Extra)
	}
	checks := []struct {
		name  string
		in    Input
		refs  []store.EntryRef
		admin bool
	}{
		{"reviewer 2", r.Extra[0], []store.EntryRef{e.a}, true},
		{"reviewer 3", r.Extra[1], []store.EntryRef{e.b, e.a}, false},
		{"judge", r.Judge, []store.EntryRef{e.a}, true},
	}
	for _, c := range checks {
		if c.in.Scope != store.ScopeRepo || !slices.Equal(c.in.Refs, c.refs) || c.in.SavedByAdmin != c.admin {
			t.Errorf("%s: scope %q refs %v admin %v; want repo %v %v", c.name, c.in.Scope, c.in.Refs, c.in.SavedByAdmin, c.refs, c.admin)
		}
		if c.in.OwnerTeam != e.team || !reflect.DeepEqual(c.in.Entries, r.One.Entries) || !reflect.DeepEqual(c.in.Marks, r.One.Marks) {
			t.Errorf("%s: owner %d, entries or marks differ from reviewer 1", c.name, c.in.OwnerTeam)
		}
		if c.in.Tried != nil || c.in.Avoid != nil || c.in.InstanceQuotaUsedUp || !c.in.Now.Equal(now) {
			t.Errorf("%s: %+v; want no Tried, no Avoid, no quota, now", c.name, c.in)
		}
	}
	if _, ok := r.Extra[1].Marks[e.b]; !ok {
		t.Errorf("reviewer 3 has no mark for %v", e.b)
	}
	// Each reviewer has its own maps: a caller that edits one changes no
	// other.
	r.Extra[0].Entries[g("x")] = instances.Model{ID: "x"}
	if _, ok := r.One.Entries[g("x")]; ok {
		t.Error("reviewer 2 shares its entries with reviewer 1")
	}
}

// An API review is never a multi review: it reads no stored reviewer.
func TestLoadReviewersAPIHasNone(t *testing.T) {
	e := newPanelEnv(t, &config.Config{})
	e.repoRow(t)
	e.saveReviewers(t, [][]store.EntryRef{{e.b}}, []store.EntryRef{e.a}, true)
	r := e.loadReviewers(t, store.Run{Kind: store.KindAPI})
	if len(r.Extra) != 0 || r.Judge.Refs != nil {
		t.Fatalf("Extra = %+v, Judge.Refs = %v; want none", r.Extra, r.Judge.Refs)
	}
	if r.One.Scope != store.ScopeGlobal {
		t.Fatalf("One.Scope = %q; want global", r.One.Scope)
	}
}

// The frozen copy has each entry's name, provider and model, and never a key,
// a base URL or a key's env var.
func TestFreezeHoldsNoKey(t *testing.T) {
	in := input(store.ScopeRepo, g("a"), tm("b"), g("gone"))
	in.SavedByAdmin = true
	delete(in.Entries, g("gone"))
	for ref, m := range in.Entries {
		m.Key = "sk-secret-" + ref.ID
		m.LLM.BaseURL = "https://llm.example.test/" + ref.ID
		m.LLM.APIKeyEnv = "SECRET_ENV_" + ref.ID
		in.Entries[ref] = m
	}
	fc := Freeze(in)
	want := store.FrozenChain{Scope: store.ScopeRepo, SavedByAdmin: true, Entries: []store.FrozenEntry{
		{Source: store.SourceGlobal, ID: "a", Name: "n-a", Provider: "openai", Model: "gpt"},
		{Source: store.SourceTeam, ID: "b", Name: "n-b", Provider: "openai", Model: "gpt"},
		{Source: store.SourceGlobal, ID: "gone"},
	}}
	if !reflect.DeepEqual(fc, want) {
		t.Fatalf("Freeze = %+v\nwant %+v", fc, want)
	}
	b, err := json.Marshal(fc)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"sk-secret", "llm.example.test", "SECRET_ENV", "key"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("frozen JSON %s holds %q", b, bad)
		}
	}
}

// A frozen ref that is no longer in the registry is skipped as removed, and
// the next ref runs.
func TestFromFrozenRemovedEntry(t *testing.T) {
	base := input(store.ScopeRepo, g("b"))
	fc := store.FrozenChain{Scope: store.ScopeRepo, Entries: []store.FrozenEntry{
		{Source: store.SourceGlobal, ID: "a", Name: "n-a", Provider: "openai", Model: "gpt"},
		{Source: store.SourceGlobal, ID: "b", Name: "n-b", Provider: "openai", Model: "gpt"},
	}}
	d := Resolve(FromFrozen(fc, base))
	if d.Ref != g("b") || d.Position != 2 || d.Length != 2 || d.FirstSkip != ReasonRemoved {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.Skips) != 1 || d.Skips[0].Ref != g("a") || d.Skips[0].Name != "" {
		t.Fatalf("skips = %+v", d.Skips)
	}
}

// The frozen chain is what runs: an edit of the live chain after the split,
// its refs, scope or saved_by_admin, changes nothing.
func TestFromFrozenIgnoresLiveChain(t *testing.T) {
	base := input(store.ScopeRepo, g("live"), g("a"))
	m := base.Entries[g("a")]
	m.TeamsMayUse = false
	base.Entries[g("a")] = m
	base.SavedByAdmin = false
	fc := Freeze(Input{Scope: store.ScopeRepo, SavedByAdmin: true, Refs: []store.EntryRef{g("a")}, Entries: base.Entries})

	in := FromFrozen(fc, base)
	if in.Scope != store.ScopeRepo || !in.SavedByAdmin || !slices.Equal(in.Refs, []store.EntryRef{g("a")}) {
		t.Fatalf("in = scope %q admin %v refs %v", in.Scope, in.SavedByAdmin, in.Refs)
	}
	// An admin saved the frozen chain, so its entry teams may not use runs,
	// though a maintainer saved the live one since.
	if d := Resolve(in); !d.Runs() || d.Ref != g("a") || d.Length != 1 {
		t.Fatalf("decision = %+v", d)
	}
	base.Scope, base.Refs = store.ScopeTeam, []store.EntryRef{g("live")}
	if d := Resolve(FromFrozen(fc, base)); d.Ref != g("a") {
		t.Fatalf("after a live edit: decision = %+v", d)
	}
}

// FromFrozen keeps base's entries, marks, owner team and now, and starts with
// no tried entry, no avoided model and no instance quota.
func TestFromFrozenDropsTried(t *testing.T) {
	base := input(store.ScopeRepo, g("a"), g("b"))
	base.Tried = map[store.EntryRef]bool{g("a"): true}
	base.Avoid = []SameModel{{Provider: "openai", Model: "gpt"}}
	base.InstanceQuotaUsedUp = true
	base.Marks = map[store.EntryRef]store.Mark{g("b"): mark(g("b"), store.ClassQuota, at(time.Hour))}
	fc := Freeze(base)

	in := FromFrozen(fc, base)
	if in.Tried != nil || in.Avoid != nil || in.InstanceQuotaUsedUp {
		t.Fatalf("in = %+v; want no Tried, no Avoid, no quota", in)
	}
	if !reflect.DeepEqual(in.Entries, base.Entries) || !reflect.DeepEqual(in.Marks, base.Marks) ||
		in.OwnerTeam != base.OwnerTeam || !in.Now.Equal(base.Now) {
		t.Fatalf("in = %+v; want base's entries, marks, owner team and now", in)
	}
	if d := Resolve(in); d.Ref != g("a") || d.Position != 1 {
		t.Fatalf("decision = %+v", d)
	}
}
