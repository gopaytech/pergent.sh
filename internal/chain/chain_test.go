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
// admin can save it then): any global entry runs. With an owner, a chain its
// maintainers saved skips a global entry no team may use any more.
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

// A repository chain an admin saved keeps its global entries usable when a
// team owns the repository, teams may use or not (owner ruling 2026-11-20);
// once a team's maintainer saves it, the rule of team chains applies. A team
// chain an admin saved is still a team chain.
func TestResolveAdminSavedRepoChain(t *testing.T) {
	in := input(store.ScopeRepo, g("a"), g("b"))
	m := in.Entries[g("a")]
	m.TeamsMayUse = false
	in.Entries[g("a")] = m
	in.SavedByAdmin = true
	if d := Resolve(in); d.Ref != g("a") || d.FirstSkip != "" {
		t.Fatalf("admin-saved: decision = %+v", d)
	}
	in.SavedByAdmin = false
	if d := Resolve(in); d.Ref != g("b") || d.FirstSkip != ReasonNotAllowed {
		t.Fatalf("team-saved: decision = %+v", d)
	}
	in.Scope, in.SavedByAdmin = store.ScopeTeam, true
	if d := Resolve(in); d.Ref != g("b") || d.FirstSkip != ReasonNotAllowed {
		t.Fatalf("team chain an admin saved: decision = %+v", d)
	}
	// A team entry of another team is still skipped in an admin's chain.
	in = input(store.ScopeRepo, tm("x"), g("b"))
	in.SavedByAdmin, in.OwnerTeam = true, 8
	if d := Resolve(in); d.Ref != g("b") || d.FirstSkip != ReasonNotOwner {
		t.Fatalf("admin-saved, foreign team entry: decision = %+v", d)
	}
}

// Runs is what the re-run and start requests ask under a used-up instance
// quota: only a team's entry is left to run.
func TestDecisionRunsUnderInstanceQuota(t *testing.T) {
	in := input(store.ScopeTeam, g("a"), tm("b"))
	in.InstanceQuotaUsedUp = true
	if d := Resolve(in); !d.Runs() || d.Ref != tm("b") {
		t.Fatalf("with a team entry: decision = %+v", d)
	}
	in = input(store.ScopeTeam, g("a"))
	in.InstanceQuotaUsedUp = true
	if d := Resolve(in); d.Runs() || !d.Hold {
		t.Fatalf("global only: decision = %+v", d)
	}
	if d := Resolve(Input{}); d.Runs() {
		t.Fatalf("empty chain: decision = %+v", d)
	}
}

// Avoid skips an entry on the same provider and model, whatever the case and
// the key: a panel's member B never runs on the model A runs on. A chain of
// only that entry is exhausted, never held or waited on.
func TestResolveAvoid(t *testing.T) {
	in := input(store.ScopeGlobal, g("a"), g("b"))
	in.Entries[g("a")] = instances.Model{ID: "a", Name: "n-a", Key: "key-a", TeamsMayUse: true,
		LLM: config.LLM{Provider: "OpenAI", Model: "GPT-5"}}
	in.Entries[g("b")] = instances.Model{ID: "b", Name: "n-b", Key: "key-b", TeamsMayUse: true,
		LLM: config.LLM{Provider: "anthropic", Model: "claude"}}
	in.Avoid = []SameModel{{Provider: "openai", Model: "gpt-5"}}
	d := Resolve(in)
	if d.Ref != g("b") || d.Position != 2 || d.FirstSkip != ReasonSameModel {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.Skips) != 1 || d.Skips[0].Reason != ReasonSameModel || d.Skips[0].Name != "n-a" {
		t.Fatalf("skips = %+v", d.Skips)
	}

	only := input(store.ScopeGlobal, g("a"))
	only.Entries[g("a")] = in.Entries[g("a")]
	only.Avoid = in.Avoid
	d = Resolve(only)
	if !d.Exhausted || d.Hold || d.Wait || d.Runs() || len(d.Skips) != 1 || d.Skips[0].Reason != ReasonSameModel {
		t.Fatalf("decision = %+v", d)
	}
	if got := FailureText(d); !strings.Contains(got, "n-a (the model another reviewer runs on)") {
		t.Errorf("failure text = %q", got)
	}

	// Another model of the same provider runs, and no Avoid avoids nothing.
	other := only
	other.Avoid = []SameModel{{Provider: "openai", Model: "gpt-4"}}
	if d := Resolve(other); !d.Runs() || d.Ref != g("a") {
		t.Errorf("another model: decision = %+v", d)
	}
	other.Avoid = []SameModel{{}}
	if d := Resolve(other); !d.Runs() || d.Ref != g("a") {
		t.Errorf("a zero Avoid: decision = %+v", d)
	}
	other.Avoid = nil
	if d := Resolve(other); !d.Runs() || d.Ref != g("a") {
		t.Errorf("no Avoid: decision = %+v", d)
	}
}

// A used-up model limit skips a global entry, a used-up team limit any
// entry: the next entry runs.
func TestResolveLimitsSkip(t *testing.T) {
	in := input(store.ScopeTeam, g("a"), tm("b"), g("c"))
	in.ModelLimitUsedUp = map[store.EntryRef]bool{g("a"): true}
	in.TeamLimitUsedUp = map[store.EntryRef]bool{tm("b"): true}
	d := Resolve(in)
	if d.Ref != g("c") || d.Position != 3 || d.FirstSkip != ReasonModelLimit || d.Skips[1].Reason != ReasonTeamLimit {
		t.Fatalf("decision = %+v", d)
	}
}

// The order: a mark, then the instance quota, then the model limit, then the
// team limit.
func TestResolveLimitOrder(t *testing.T) {
	in := input(store.ScopeTeam, g("a"), tm("b"))
	in.ModelLimitUsedUp = map[store.EntryRef]bool{g("a"): true}
	in.TeamLimitUsedUp = map[store.EntryRef]bool{g("a"): true}
	if d := Resolve(in); d.FirstSkip != ReasonModelLimit {
		t.Errorf("model before team: first skip %q", d.FirstSkip)
	}
	in.InstanceQuotaUsedUp = true
	if d := Resolve(in); d.FirstSkip != ReasonInstanceQuota {
		t.Errorf("quota before limits: first skip %q", d.FirstSkip)
	}
	in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassQuota, at(time.Hour))}
	if d := Resolve(in); d.FirstSkip != store.ClassQuota {
		t.Errorf("mark first: first skip %q", d.FirstSkip)
	}
}

// A team entry has no model limit: only its team's limit stops it.
func TestResolveModelLimitIsGlobalOnly(t *testing.T) {
	in := input(store.ScopeTeam, tm("a"))
	in.ModelLimitUsedUp = map[store.EntryRef]bool{tm("a"): true}
	if d := Resolve(in); d.Ref != tm("a") {
		t.Fatalf("decision = %+v; want the team entry to run", d)
	}
}

// A 1-entry chain ignores marks, not limits: at a limit it is held.
func TestResolveLimitsHoldOneEntryChain(t *testing.T) {
	in := input(store.ScopeGlobal, g("a"))
	in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassAuth, nil)}
	if d := Resolve(in); !d.Runs() {
		t.Fatalf("a mark stopped a 1-entry chain: %+v", d)
	}
	in.ModelLimitUsedUp = map[store.EntryRef]bool{g("a"): true}
	if d := Resolve(in); !d.Hold || !d.Limited || d.Exhausted {
		t.Fatalf("decision = %+v; want a hold a limit took part in", d)
	}
}

// Held when quotas and limits alone stop the entries this review has not
// tried; Limited when a limit is one of them. A mark or a bad key fails it,
// and names the limits too.
func TestResolveHoldRules(t *testing.T) {
	for _, c := range []struct {
		name          string
		edit          func(in *Input)
		hold, limited bool
		text          string
	}{
		{"quota only", func(in *Input) {
			in.InstanceQuotaUsedUp = true
			in.Tried = map[store.EntryRef]bool{tm("b"): true}
		}, true, false, ""},
		{"quota and a team limit", func(in *Input) {
			in.InstanceQuotaUsedUp = true
			in.TeamLimitUsedUp = map[store.EntryRef]bool{tm("b"): true}
		}, true, true, ""},
		{"tried and a team limit", func(in *Input) {
			in.Tried = map[store.EntryRef]bool{g("a"): true}
			in.TeamLimitUsedUp = map[store.EntryRef]bool{tm("b"): true}
		}, true, true, ""},
		{"tried only", func(in *Input) {
			in.Tried = map[store.EntryRef]bool{g("a"): true, tm("b"): true}
		}, false, false, "all models exhausted: n-a (already tried), n-b (already tried)"},
		{"a mark and a team limit", func(in *Input) {
			in.Marks = map[store.EntryRef]store.Mark{g("a"): mark(g("a"), store.ClassQuota, at(time.Hour))}
			in.TeamLimitUsedUp = map[store.EntryRef]bool{tm("b"): true}
		}, false, false, "all models exhausted: n-a (out of quota), n-b (team limit used up)"},
		{"no key and a model limit", func(in *Input) {
			m := in.Entries[tm("b")]
			m.Key = ""
			in.Entries[tm("b")] = m
			in.ModelLimitUsedUp = map[store.EntryRef]bool{g("a"): true}
		}, false, false, "all models exhausted: n-a (model limit used up), n-b (no usable key)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := input(store.ScopeTeam, g("a"), tm("b"))
			c.edit(&in)
			d := Resolve(in)
			if d.Hold != c.hold || d.Limited != c.limited || d.Exhausted == c.hold || d.Runs() {
				t.Fatalf("decision = %+v; want hold %v, limited %v", d, c.hold, c.limited)
			}
			if c.text != "" && FailureText(d) != c.text {
				t.Errorf("FailureText = %q\nwant %q", FailureText(d), c.text)
			}
		})
	}
}

// An admin's start goes past the instance quota, the model limits and the
// team limits on global entries; a team's own limit on its team entry stays.
func TestPastAdminLimits(t *testing.T) {
	in := input(store.ScopeRepo, tm("a"), g("b"))
	in.SavedByAdmin, in.InstanceQuotaUsedUp = true, true
	in.ModelLimitUsedUp = map[store.EntryRef]bool{g("b"): true}
	in.TeamLimitUsedUp = map[store.EntryRef]bool{tm("a"): true, g("b"): true}
	if d := Resolve(in); !d.Hold {
		t.Fatalf("before: %+v; want a hold", d)
	}
	past := in
	past.PastAdminLimits()
	if d := Resolve(past); d.Ref != g("b") || d.FirstSkip != ReasonTeamLimit {
		t.Fatalf("past the admin limits: %+v; want the global entry, the team's own limit still skipping its entry", d)
	}
	if !in.TeamLimitUsedUp[g("b")] || !in.ModelLimitUsedUp[g("b")] {
		t.Error("PastAdminLimits changed the maps of the Input it was copied from")
	}
	own := input(store.ScopeTeam, tm("a"))
	own.TeamLimitUsedUp = map[store.EntryRef]bool{tm("a"): true}
	own.PastAdminLimits()
	if d := Resolve(own); !d.Hold {
		t.Fatalf("only the team's own entry: %+v; want still held", d)
	}
	own.IgnoreLimits()
	if d := Resolve(own); !d.Runs() {
		t.Fatalf("IgnoreLimits: %+v; want it to run", d)
	}
}

func TestReasonWordsLimits(t *testing.T) {
	if ReasonWords(ReasonModelLimit) != "model limit used up" || ReasonWords(ReasonTeamLimit) != "team limit used up" {
		t.Errorf("words = %q, %q", ReasonWords(ReasonModelLimit), ReasonWords(ReasonTeamLimit))
	}
}

// Avoid holds the models of every earlier reviewer: each is skipped, in any
// case, and a chain of only those models is exhausted.
func TestResolveAvoidSeveral(t *testing.T) {
	model := func(id, provider, name string) instances.Model {
		return instances.Model{ID: id, Name: "n-" + id, Key: "key-" + id, TeamsMayUse: true,
			LLM: config.LLM{Provider: provider, Model: name}}
	}
	in := input(store.ScopeGlobal, g("a"), g("b"), g("c"))
	in.Entries[g("a")] = model("a", "OpenAI", "GPT-5")
	in.Entries[g("b")] = model("b", "anthropic", "Claude")
	in.Entries[g("c")] = model("c", "alibaba", "qwen")
	in.Avoid = []SameModel{{Provider: "openai", Model: "gpt-5"}, {Provider: "ANTHROPIC", Model: "claude"}}
	d := Resolve(in)
	if d.Ref != g("c") || d.Position != 3 || d.FirstSkip != ReasonSameModel {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.Skips) != 2 || d.Skips[0].Reason != ReasonSameModel || d.Skips[1].Reason != ReasonSameModel {
		t.Fatalf("skips = %+v", d.Skips)
	}
	for _, ref := range []store.EntryRef{g("a"), g("b")} {
		if got := EntryReason(in, ref); got != ReasonSameModel {
			t.Errorf("EntryReason(%v) = %q", ref, got)
		}
	}

	only := in
	only.Refs = []store.EntryRef{g("a"), g("b")}
	d = Resolve(only)
	if !d.Exhausted || d.Hold || d.Wait || d.Runs() || len(d.Skips) != 2 {
		t.Fatalf("only avoided models: decision = %+v", d)
	}
	want := "all models exhausted: n-a (the model another reviewer runs on), n-b (the model another reviewer runs on)"
	if got := FailureText(d); got != want {
		t.Errorf("failure text = %q", got)
	}
}
