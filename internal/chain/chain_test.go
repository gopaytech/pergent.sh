package chain

import (
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
)

var now = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func g(id string) store.EntryRef  { return store.EntryRef{Source: store.SourceGlobal, ID: id} }
func tm(id string) store.EntryRef { return store.EntryRef{Source: store.SourceTeam, ID: id} }

// entries is a known entry for each ref, named after its ID, with a key; a
// team entry belongs to team 7 and a global one teams may use.
func entries(refs ...store.EntryRef) map[store.EntryRef]instances.Model {
	out := map[store.EntryRef]instances.Model{}
	for _, r := range refs {
		m := instances.Model{ID: r.ID, Name: "n-" + r.ID, Key: "key-" + r.ID,
			LLM: config.LLM{Provider: "openai", Model: "gpt"}}
		if r.Source == store.SourceTeam {
			m.TeamID = 7
		} else {
			m.TeamsMayUse = true
		}
		out[r] = m
	}
	return out
}

func input(scope string, refs ...store.EntryRef) Input {
	return Input{Scope: scope, Refs: refs, Entries: entries(refs...), OwnerTeam: 7, Now: now}
}

func mark(ref store.EntryRef, class string, until *time.Time) store.Mark {
	return store.Mark{Ref: ref, Class: class, Until: until, SetAt: now.Add(-time.Minute)}
}

func at(d time.Duration) *time.Time { t := now.Add(d); return &t }

func TestResolveFirstUsable(t *testing.T) {
	d := Resolve(input(store.ScopeTeam, tm("a"), g("b")))
	if d.Ref != tm("a") || d.Position != 1 || d.Length != 2 || d.FirstSkip != "" || len(d.Skips) != 0 {
		t.Fatalf("decision = %+v", d)
	}
	if d.Entry.Key != "key-a" || d.Wait || d.Hold || d.Exhausted {
		t.Fatalf("decision = %+v", d)
	}
}

// Each reason an entry is skipped for, at position 1 of a 2-entry chain: the
// second runs, and the first's reason is the first skip.
func TestResolveSkipReasons(t *testing.T) {
	cases := []struct {
		reason string
		edit   func(in *Input)
	}{
		{ReasonTried, func(in *Input) { in.Tried = map[store.EntryRef]bool{g("a"): true} }},
		{store.ClassQuota, func(in *Input) {
			in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassQuota, at(time.Hour))}
		}},
		{store.ClassBilling, func(in *Input) {
			in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassBilling, at(time.Minute))}
		}},
		{store.ClassAuth, func(in *Input) {
			in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassAuth, nil)}
		}},
		{ReasonRemoved, func(in *Input) { delete(in.Entries, g("a")) }},
		{ReasonNotAllowed, func(in *Input) {
			m := in.Entries[g("a")]
			m.TeamsMayUse = false
			in.Entries[g("a")] = m
		}},
		{ReasonKeyUnusable, func(in *Input) {
			m := in.Entries[g("a")]
			m.Key = ""
			in.Entries[g("a")] = m
		}},
		{ReasonInstanceQuota, func(in *Input) { in.InstanceQuotaUsedUp = true }},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			in := input(store.ScopeTeam, g("a"), tm("b"))
			c.edit(&in)
			d := Resolve(in)
			if d.Ref != tm("b") || d.Position != 2 || d.Length != 2 {
				t.Fatalf("decision = %+v, want entry b at 2 of 2", d)
			}
			if d.FirstSkip != c.reason || len(d.Skips) != 1 || d.Skips[0].Reason != c.reason || d.Skips[0].Ref != g("a") {
				t.Fatalf("first skip = %q, skips = %+v, want %q", d.FirstSkip, d.Skips, c.reason)
			}
		})
	}
}

func TestResolveNotOwner(t *testing.T) {
	in := input(store.ScopeRepo, tm("a"), tm("b"))
	m := in.Entries[tm("a")]
	m.TeamID = 8 // another team's entry: team 7 owns the repository
	in.Entries[tm("a")] = m
	d := Resolve(in)
	if d.Ref != tm("b") || d.FirstSkip != ReasonNotOwner {
		t.Fatalf("decision = %+v", d)
	}
	in.OwnerTeam = 0 // no owner at all: no team entry runs
	if d := Resolve(in); !d.Exhausted || len(d.Skips) != 2 || d.Skips[1].Reason != ReasonNotOwner {
		t.Fatalf("no owner: decision = %+v", d)
	}
}

func TestResolveMarkExpired(t *testing.T) {
	in := input(store.ScopeTeam, g("a"), g("b"))
	in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassQuota, at(-time.Second))}
	if d := Resolve(in); d.Ref != g("a") || d.FirstSkip != "" {
		t.Fatalf("an expired mark still skipped: %+v", d)
	}
	in.Marks[g("a")] = mark(g("a"), store.ClassQuota, at(0))
	if d := Resolve(in); d.Ref != g("a") {
		t.Fatalf("a mark ending now still skipped: %+v", d)
	}
}

func TestResolveAuthMarkHasNoExpiry(t *testing.T) {
	in := input(store.ScopeTeam, g("a"), g("b"))
	in.Now = now.Add(365 * 24 * time.Hour)
	in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassAuth, nil)}
	if d := Resolve(in); d.Ref != g("b") || d.FirstSkip != store.ClassAuth {
		t.Fatalf("an auth mark expired: %+v", d)
	}
}

// A 1-entry chain runs and fails as today: another chain's mark on its entry
// doesn't stop it.
func TestResolveOneEntryIgnoresMarks(t *testing.T) {
	in := input(store.ScopeRepo, g("a"))
	in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassQuota, at(time.Hour))}
	if d := Resolve(in); d.Ref != g("a") || d.Position != 1 || d.Length != 1 || d.Exhausted {
		t.Fatalf("decision = %+v", d)
	}
	// Other reasons still apply to it.
	m := in.Entries[g("a")]
	m.Key = ""
	in.Entries[g("a")] = m
	if d := Resolve(in); !d.Exhausted || d.Skips[0].Reason != ReasonKeyUnusable {
		t.Fatalf("no key: decision = %+v", d)
	}
}

// The instance quota counts only global keys: a team entry still runs.
func TestResolveInstanceQuotaSkipsGlobalOnly(t *testing.T) {
	in := input(store.ScopeTeam, g("a"), tm("b"), g("c"))
	in.InstanceQuotaUsedUp = true
	d := Resolve(in)
	if d.Ref != tm("b") || d.Position != 2 || d.FirstSkip != ReasonInstanceQuota {
		t.Fatalf("decision = %+v", d)
	}
}

func TestResolveOnlyQuotaBlockedHolds(t *testing.T) {
	in := input(store.ScopeGlobal, g("a"), g("b"))
	in.InstanceQuotaUsedUp = true
	if d := Resolve(in); !d.Hold || d.Exhausted || d.Wait {
		t.Fatalf("decision = %+v, want a hold", d)
	}
	// One entry skipped for something else: the review fails, it doesn't
	// wait for a quota that would not let it run either.
	in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassAuth, nil)}
	if d := Resolve(in); d.Hold || !d.Exhausted {
		t.Fatalf("mixed reasons: decision = %+v, want exhausted", d)
	}
}

func TestResolveEmptyWaits(t *testing.T) {
	d := Resolve(Input{Scope: store.ScopeGlobal, Now: now})
	if !d.Wait || d.Exhausted || d.Hold || d.Length != 0 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestResolveExhausted(t *testing.T) {
	in := input(store.ScopeTeam, g("a"), tm("b"), g("c"))
	delete(in.Entries, g("c"))
	in.Marks = map[store.EntryRef]store.Mark{
		g("a"):  mark(g("a"), store.ClassQuota, at(time.Hour)),
		tm("b"): mark(tm("b"), store.ClassAuth, nil),
	}
	d := Resolve(in)
	if !d.Exhausted || d.Hold || d.Wait || d.Position != 0 || d.Length != 3 || len(d.Skips) != 3 {
		t.Fatalf("decision = %+v", d)
	}
	want := "all models exhausted: n-a (out of quota), n-b (key rejected), a removed model (removed)"
	if got := FailureText(d); got != want {
		t.Fatalf("FailureText = %q\nwant %q", got, want)
	}
	if strings.Contains(FailureText(d), "key-") {
		t.Fatal("the failure text holds a key")
	}
}

// Two keys of the same model are 2 entries, told apart by ID: the first out
// of quota, the second runs.
func TestResolveSameModelTwoKeys(t *testing.T) {
	in := input(store.ScopeTeam, tm("k1"), tm("k2"))
	for _, r := range in.Refs {
		m := in.Entries[r]
		m.LLM = config.LLM{Provider: "anthropic", Model: "claude"}
		in.Entries[r] = m
	}
	in.Marks = map[store.EntryRef]store.Mark{tm("k1"): mark(tm("k1"), store.ClassQuota, at(time.Hour))}
	d := Resolve(in)
	if d.Ref != tm("k2") || d.Entry.Key != "key-k2" || d.Entry.LLM != in.Entries[tm("k1")].LLM {
		t.Fatalf("decision = %+v", d)
	}
}

// A key that can't be opened skips that entry only, and nothing the resolver
// says holds a key.
func TestResolveUnusableKey(t *testing.T) {
	in := input(store.ScopeGlobal, g("a"), g("b"))
	m := in.Entries[g("a")]
	m.Key = ""
	in.Entries[g("a")] = m
	d := Resolve(in)
	if d.Ref != g("b") || d.FirstSkip != ReasonKeyUnusable || d.Skips[0].Name != "n-a" {
		t.Fatalf("decision = %+v", d)
	}
}

func TestResolveNotAllowedAnyMore(t *testing.T) {
	in := input(store.ScopeTeam, g("a"))
	m := in.Entries[g("a")]
	m.TeamsMayUse = false
	in.Entries[g("a")] = m
	if d := Resolve(in); !d.Exhausted || d.Skips[0].Reason != ReasonNotAllowed {
		t.Fatalf("decision = %+v", d)
	}
}

// The global chain is the admin's: any global entry runs in it.
func TestResolveGlobalChainAllowsAnyGlobal(t *testing.T) {
	in := input(store.ScopeGlobal, g("a"))
	m := in.Entries[g("a")]
	m.TeamsMayUse = false
	in.Entries[g("a")] = m
	if d := Resolve(in); d.Ref != g("a") {
		t.Fatalf("decision = %+v", d)
	}
}

// A repository chain is the admin's when no team owns the repository (only an
// admin can save it then), and the seeded 1-entry chains of the upgrade name
// entries no team may use: those run. With an owner, its maintainers save the
// chain, and a global entry no team may use any more is skipped.
func TestResolveRepoChainTeamsMayUse(t *testing.T) {
	in := input(store.ScopeRepo, g("a"), g("b"))
	m := in.Entries[g("a")]
	m.TeamsMayUse = false
	in.Entries[g("a")] = m
	in.OwnerTeam = 0
	if d := Resolve(in); d.Ref != g("a") {
		t.Fatalf("no owner: decision = %+v", d)
	}
	in.OwnerTeam = 7
	if d := Resolve(in); d.Ref != g("b") || d.FirstSkip != ReasonNotAllowed {
		t.Fatalf("owned: decision = %+v", d)
	}
}
