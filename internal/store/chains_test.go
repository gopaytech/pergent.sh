package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func mustChain(t *testing.T, s *Store, scope string, teamID int64, instance, project string) (Chain, bool) {
	t.Helper()
	c, found, err := s.Chain(ctx, scope, teamID, instance, project)
	if err != nil {
		t.Fatalf("Chain(%s): %v", scope, err)
	}
	return c, found
}

func TestSaveChainLimit(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	var six []EntryRef
	for i := 0; i < 6; i++ {
		six = append(six, EntryRef{SourceTeam, newTeamModel(t, s, team, string(rune('a'+i)), nil).ID})
	}
	if err := s.SaveChain(ctx, Chain{Scope: ScopeTeam, TeamID: team, Entries: six}, t0); !errors.Is(err, ErrChainTooLong) {
		t.Errorf("6 entries = %v; want ErrChainTooLong", err)
	}
	if _, found := mustChain(t, s, ScopeTeam, team, "", ""); found {
		t.Error("a refused chain was stored")
	}
	if err := s.SaveChain(ctx, Chain{Scope: ScopeTeam, TeamID: team, Entries: six[:MaxChain]}, t0); err != nil {
		t.Fatalf("5 entries: %v", err)
	}
	c, found := mustChain(t, s, ScopeTeam, team, "", "")
	if !found || !slices.Equal(c.Entries, six[:5]) || c.TeamID != team || !c.UpdatedAt.Equal(t0) {
		t.Errorf("chain = %+v, %v", c, found)
	}
	// Saving again replaces the entries.
	if err := s.SaveChain(ctx, Chain{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{six[4], six[0]}}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c, _ := mustChain(t, s, ScopeTeam, team, "", ""); !slices.Equal(c.Entries, []EntryRef{six[4], six[0]}) || !c.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("chain after a second save = %+v", c)
	}
	// A team entry that is not there is refused.
	if err := s.SaveChain(ctx, Chain{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{{SourceTeam, "no-such-id"}}}, t0); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("a missing team entry = %v; want ErrModelNotFound", err)
	}
}

// Position 1 of the global chain is the default, which the previous release
// reads from llm_settings; the config entry leaves the stored default alone.
func TestSaveGlobalChainSyncsDefault(t *testing.T) {
	s := newStore(t)
	a := newModel(t, s, "anthropic", "sonnet", "", []byte("ka"))
	b := newModel(t, s, "openai", "gpt-5", "https://llm.example.com", []byte("kb"))
	A, B := EntryRef{SourceGlobal, a.ID}, EntryRef{SourceGlobal, b.ID}
	if err := s.SaveChain(ctx, Chain{Scope: ScopeGlobal, Entries: []EntryRef{B, A}}, t0); err != nil {
		t.Fatal(err)
	}
	if got := defaultsOf(mustModels(t, s)); !slices.Equal(got, []string{b.ID}) {
		t.Errorf("defaults = %v; want b", got)
	}
	if r, found := settings(t, s); !found || r.Provider != "openai" || r.Model != "gpt-5" || r.BaseURL != "https://llm.example.com" {
		t.Errorf("llm_settings = %+v, %v; want b", r, found)
	}
	if err := s.SaveChain(ctx, Chain{Scope: ScopeGlobal, Entries: []EntryRef{{SourceGlobal, ConfigModelID}, A}}, t0); err != nil {
		t.Fatal(err)
	}
	if got := defaultsOf(mustModels(t, s)); !slices.Equal(got, []string{b.ID}) {
		t.Errorf("defaults after config first = %v; want b kept", got)
	}
	c, found := mustChain(t, s, ScopeGlobal, 0, "", "")
	if !found || !slices.Equal(c.Entries, []EntryRef{{SourceGlobal, ConfigModelID}, A}) {
		t.Errorf("global chain = %+v, %v", c, found)
	}
}

// A custom repository chain keeps model_id at its first entry when global,
// NULL when a team's.
func TestSaveRepoChainSyncsModelID(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	tm := newTeamModel(t, s, team, "Team key", nil)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "Acme/API"}); err != nil {
		t.Fatal(err)
	}
	repo := func() DashboardRepo {
		t.Helper()
		r, _, err := s.AnyDashboardRepo(ctx, "main", "acme/api")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := repo(); r.ChainMode != "inherit" {
		t.Errorf("chain mode of a new repository = %q; want inherit", r.ChainMode)
	}
	if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api",
		Entries: []EntryRef{{SourceGlobal, g.ID}, {SourceTeam, tm.ID}}}, t0); err != nil {
		t.Fatal(err)
	}
	if r := repo(); r.ChainMode != "custom" || r.ModelID == nil || *r.ModelID != g.ID {
		t.Errorf("repository = %s, %v; want custom and %s", r.ChainMode, r.ModelID, g.ID)
	}
	if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: "ACME/api",
		Entries: []EntryRef{{SourceTeam, tm.ID}, {SourceGlobal, g.ID}}}, t0); err != nil {
		t.Fatal(err)
	}
	if r := repo(); r.ChainMode != "custom" || r.ModelID != nil {
		t.Errorf("repository = %s, %v; want custom and no model", r.ChainMode, r.ModelID)
	}
	c, found := mustChain(t, s, ScopeRepo, 0, "main", "acme/API")
	if !found || len(c.Entries) != 2 || c.Instance != "main" {
		t.Errorf("repository chain = %+v, %v; want one chain, matched ignoring case", c, found)
	}
}

func TestResetRepoChain(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{{SourceGlobal, g.ID}}}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetRepoChain(ctx, "main", "ACME/api"); err != nil {
		t.Fatal(err)
	}
	if _, found := mustChain(t, s, ScopeRepo, 0, "main", "acme/api"); found {
		t.Error("the repository chain is still there")
	}
	r, _, err := s.AnyDashboardRepo(ctx, "main", "acme/api")
	if err != nil || r.ChainMode != "inherit" || r.ModelID != nil {
		t.Errorf("repository = %s, %v, %v; want inherit and no model", r.ChainMode, r.ModelID, err)
	}
}

// Until the chain editor replaces it, a change of the old model setting
// rewrites the repository chain: a model is a custom 1-entry chain, none is
// inherit. A write that leaves the model alone leaves the chain alone.
func TestModelSettingWritesRepoChain(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	a := newModel(t, s, "anthropic", "sonnet", "", nil)
	b := newModel(t, s, "openai", "gpt-5", "", nil)
	check := func(project, mode string, want []EntryRef) {
		t.Helper()
		r, _, err := s.AnyDashboardRepo(ctx, "main", project)
		if err != nil || r.ChainMode != mode {
			t.Errorf("%s chain mode = %q, %v; want %s", project, r.ChainMode, err, mode)
		}
		c, found := mustChain(t, s, ScopeRepo, 0, "main", project)
		if found != (want != nil) || !slices.Equal(c.Entries, want) {
			t.Errorf("%s chain = %+v, %v; want %+v", project, c.Entries, found, want)
		}
	}
	if _, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/ins", ModelID: &a.ID}); err != nil {
		t.Fatal(err)
	}
	check("acme/ins", "custom", []EntryRef{{SourceGlobal, a.ID}})
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/up", ModelID: &b.ID}); err != nil {
		t.Fatal(err)
	}
	check("acme/up", "custom", []EntryRef{{SourceGlobal, b.ID}})
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/up"}); err != nil {
		t.Fatal(err)
	}
	check("acme/up", "inherit", nil)

	if _, err := s.UpdateDashboardRepo(ctx, "main", "acme/ins", func(r *DashboardRepo) { r.ModelID = &b.ID }); err != nil {
		t.Fatal(err)
	}
	check("acme/ins", "custom", []EntryRef{{SourceGlobal, b.ID}})
	if _, err := s.UpdateDashboardRepo(ctx, "main", "acme/ins", func(r *DashboardRepo) { r.ModelID = nil }); err != nil {
		t.Fatal(err)
	}
	check("acme/ins", "inherit", nil)

	// A chain that starts with a team entry has no model_id; saving another
	// setting keeps it.
	tm := newTeamModel(t, s, team, "Team key", nil)
	custom := []EntryRef{{SourceTeam, tm.ID}, {SourceGlobal, a.ID}}
	if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/ins", Entries: custom}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDashboardRepo(ctx, "main", "acme/ins", func(r *DashboardRepo) { r.Approve = new(bool) }); err != nil {
		t.Fatal(err)
	}
	check("acme/ins", "custom", custom)
}

// Two marks of the same entry both succeed; the later until wins, and an
// auth mark (no until) stays until its key is edited.
func TestMarksUpsertTwice(t *testing.T) {
	s := newStore(t)
	upsert := func(m Mark) {
		t.Helper()
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
		if err := upsertMark(ctx, tx, m); err != nil {
			t.Fatalf("upsertMark(%+v): %v", m, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	at := func(d time.Duration) *time.Time { u := t0.Add(d); return &u }
	ref := EntryRef{SourceGlobal, "m1"}
	upsert(Mark{Ref: ref, Class: ClassQuota, Until: at(2 * time.Hour), SetAt: t0})
	upsert(Mark{Ref: ref, Class: ClassBilling, Until: at(time.Hour), SetAt: t0.Add(time.Minute)})
	marks, err := s.Marks(ctx)
	if err != nil || len(marks) != 1 {
		t.Fatalf("Marks = %+v, %v; want one", marks, err)
	}
	if m := marks[0]; m.Ref != ref || m.Class != ClassQuota || m.Until == nil || !m.Until.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("mark = %+v; want quota until +2h (the later until)", m)
	}
	upsert(Mark{Ref: ref, Class: ClassBilling, Until: at(3 * time.Hour), SetAt: t0.Add(2 * time.Minute)})
	if marks, _ := s.Marks(ctx); marks[0].Class != ClassBilling || !marks[0].Until.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("mark = %+v; want billing until +3h", marks[0])
	}
	upsert(Mark{Ref: ref, Class: ClassAuth, SetAt: t0.Add(3 * time.Minute)})
	upsert(Mark{Ref: ref, Class: ClassQuota, Until: at(5 * time.Hour), SetAt: t0.Add(4 * time.Minute)})
	if marks, _ := s.Marks(ctx); marks[0].Class != ClassAuth || marks[0].Until != nil {
		t.Errorf("mark = %+v; want auth with no until", marks[0])
	}

	team := EntryRef{SourceTeam, "m1"}
	upsert(Mark{Ref: team, Class: ClassQuota, Until: at(time.Hour), SetAt: t0})
	if marks, _ := s.Marks(ctx); len(marks) != 2 {
		t.Errorf("marks = %+v; want the team entry apart from the global one", marks)
	}
	if cleared, err := s.ClearMark(ctx, ref); err != nil || !cleared {
		t.Errorf("ClearMark = %v, %v", cleared, err)
	}
	if cleared, err := s.ClearMark(ctx, ref); err != nil || cleared {
		t.Errorf("ClearMark twice = %v, %v; want nothing to clear", cleared, err)
	}
	if marks, _ := s.Marks(ctx); len(marks) != 1 || marks[0].Ref != team {
		t.Errorf("marks = %+v; want the team mark left", marks)
	}
}

func TestChainsNaming(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	tm := newTeamModel(t, s, team, "Team key", nil)
	G, T := EntryRef{SourceGlobal, g.ID}, EntryRef{SourceTeam, tm.ID}
	for _, c := range []Chain{
		{Scope: ScopeGlobal, Entries: []EntryRef{G}},
		{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{T, G}},
		{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{T}},
	} {
		if err := s.SaveChain(ctx, c, t0); err != nil {
			t.Fatal(err)
		}
	}
	scopes := func(ref EntryRef) []string {
		t.Helper()
		cs, err := s.ChainsNaming(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range cs {
			out = append(out, c.Scope)
		}
		return out
	}
	if got := scopes(G); !slices.Equal(got, []string{ScopeGlobal, ScopeTeam}) {
		t.Errorf("chains naming the global entry = %v", got)
	}
	if got := scopes(T); !slices.Equal(got, []string{ScopeTeam, ScopeRepo}) {
		t.Errorf("chains naming the team entry = %v", got)
	}
	// The same ID under the other source is another entry.
	if got := scopes(EntryRef{SourceTeam, g.ID}); got != nil {
		t.Errorf("chains naming a team entry with the global ID = %v; want none", got)
	}
}

// SetMark stores a mark on its own, as upsertMark does inside a transaction:
// an API review marks the entry it ran on with no attempt to queue.
func TestSetMark(t *testing.T) {
	s := newStore(t)
	ref := EntryRef{SourceGlobal, "m1"}
	until := t0.Add(time.Hour)
	if err := s.SetMark(ctx, Mark{Ref: ref, Class: ClassQuota, Until: &until, SetAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMark(ctx, Mark{Ref: ref, Class: ClassAuth, SetAt: t0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	marks, err := s.Marks(ctx)
	if err != nil || len(marks) != 1 || marks[0].Class != ClassAuth || marks[0].Until != nil {
		t.Fatalf("Marks = %+v, %v; want one auth mark", marks, err)
	}
}

// A chain records whether an admin saved it; each save says so again. The
// old model setting is an admin's, so the chain it writes is admin-saved.
func TestSaveChainSavedByAdmin(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	save := func(admin bool) {
		t.Helper()
		if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", SavedByAdmin: admin,
			Entries: []EntryRef{{SourceGlobal, g.ID}}}, t0); err != nil {
			t.Fatal(err)
		}
		if c, found := mustChain(t, s, ScopeRepo, 0, "main", "acme/api"); !found || c.SavedByAdmin != admin {
			t.Errorf("after a save by admin=%v: chain = %+v, %v", admin, c, found)
		}
	}
	save(true)
	save(false)
	save(true)

	if _, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/web", ModelID: &g.ID}); err != nil {
		t.Fatal(err)
	}
	if c, found := mustChain(t, s, ScopeRepo, 0, "main", "acme/web"); !found || !c.SavedByAdmin {
		t.Errorf("chain of the model setting = %+v, %v; want admin-saved", c, found)
	}
}

// 0046 only adds, and the chains written before it (the upgrade's seeded
// repository chains and the admin's model setting) read as admin-saved.
func TestSavedByAdminMigration(t *testing.T) {
	s := newStoreAt(t, 45)
	if _, err := s.pool.Exec(ctx, `INSERT INTO model_chains (scope, instance, project, updated_at)
		VALUES ('repo', 'main', 'acme/api', $1), ('global', '', '', $1)`, t0); err != nil {
		t.Fatal(err)
	}
	before := schemaColumns(t, s)
	applyUpTo(t, s, 46)
	after := schemaColumns(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0046 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	if c := after["model_chains.saved_by_admin"]; c.Nullable != "NO" {
		t.Errorf("model_chains.saved_by_admin = %+v; want NOT NULL", c)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM model_chains WHERE saved_by_admin`).Scan(&n); err != nil || n != 2 {
		t.Errorf("admin-saved chains after 0046 = %d, %v; want 2", n, err)
	}
}

// Chains reads every chain with its entries, by ID.
func TestChains(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	for _, c := range []Chain{
		{Scope: ScopeGlobal, Entries: []EntryRef{{SourceGlobal, g.ID}}, SavedByAdmin: true},
		{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{{SourceGlobal, g.ID}}},
	} {
		if err := s.SaveChain(ctx, c, t0); err != nil {
			t.Fatal(err)
		}
	}
	chains, err := s.Chains(ctx)
	if err != nil || len(chains) != 2 || chains[0].Scope != ScopeGlobal || !chains[0].SavedByAdmin ||
		chains[1].TeamID != team || len(chains[1].Entries) != 1 {
		t.Errorf("Chains = %+v, %v", chains, err)
	}
}

// Removing a repository drops its own chain with its settings and team
// links, so one added again starts by inheriting, as its settings do.
func TestRemoveRepoDropsItsChain(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	for _, p := range []string{"acme/api", "acme/web"} {
		if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: p}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: p, Entries: []EntryRef{{SourceGlobal, g.ID}}}, t0); err != nil {
			t.Fatal(err)
		}
	}
	if found, err := s.RemoveDashboardRepo(ctx, "main", "ACME/api"); err != nil || !found {
		t.Fatalf("RemoveDashboardRepo = %v, %v", found, err)
	}
	if _, found := mustChain(t, s, ScopeRepo, 0, "main", "acme/api"); found {
		t.Error("the removed repository's chain is still there")
	}
	if _, found := mustChain(t, s, ScopeRepo, 0, "main", "acme/web"); !found {
		t.Error("another repository's chain went too")
	}
}

// Once the global chain is saved, it decides what reviews try first, so
// SetDefaultLLMModel puts the entry at position 1 of it, the others moved down
// in their order. An entry not in a full chain is ErrChainTooLong, and then
// nothing changes. With no stored chain there is nothing to move.
func TestSetDefaultMovesGlobalChain(t *testing.T) {
	s := newStore(t)
	var refs []EntryRef
	for _, m := range []string{"m1", "m2", "m3", "m4", "m5", "m6"} {
		refs = append(refs, EntryRef{SourceGlobal, newModel(t, s, "openai", m, "", []byte("k")).ID})
	}
	if found, err := s.SetDefaultLLMModel(ctx, refs[1].ID, t0); err != nil || !found {
		t.Fatalf("SetDefaultLLMModel with no chain = %v, %v", found, err)
	}
	if _, found := mustChain(t, s, ScopeGlobal, 0, "", ""); found {
		t.Errorf("a default set with no stored chain created one")
	}

	if err := s.SaveChain(ctx, Chain{Scope: ScopeGlobal, Entries: []EntryRef{refs[0], refs[1]}}, t0); err != nil {
		t.Fatal(err)
	}
	if found, err := s.SetDefaultLLMModel(ctx, refs[1].ID, t0.Add(time.Minute)); err != nil || !found {
		t.Fatalf("SetDefaultLLMModel(m2) = %v, %v", found, err)
	}
	c, _ := mustChain(t, s, ScopeGlobal, 0, "", "")
	if !slices.Equal(c.Entries, []EntryRef{refs[1], refs[0]}) || !c.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("chain after m2 = %+v; want m2, m1", c)
	}
	// An entry not in the chain goes in at position 1.
	if _, err := s.SetDefaultLLMModel(ctx, refs[2].ID, t0); err != nil {
		t.Fatal(err)
	}
	if c, _ := mustChain(t, s, ScopeGlobal, 0, "", ""); !slices.Equal(c.Entries, []EntryRef{refs[2], refs[1], refs[0]}) {
		t.Errorf("chain after m3 = %+v; want m3, m2, m1", c.Entries)
	}
	if got := defaultsOf(mustModels(t, s)); !slices.Equal(got, []string{refs[2].ID}) {
		t.Errorf("defaults = %v; want m3", got)
	}

	full := refs[:5]
	if err := s.SaveChain(ctx, Chain{Scope: ScopeGlobal, Entries: full}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDefaultLLMModel(ctx, refs[5].ID, t0); !errors.Is(err, ErrChainTooLong) {
		t.Errorf("m6 into a full chain = %v; want ErrChainTooLong", err)
	}
	if c, _ := mustChain(t, s, ScopeGlobal, 0, "", ""); !slices.Equal(c.Entries, full) {
		t.Errorf("full chain after the refusal = %+v; want it unchanged", c.Entries)
	}
	if got := defaultsOf(mustModels(t, s)); !slices.Equal(got, []string{refs[0].ID}) {
		t.Errorf("defaults after the refusal = %v; want m1 kept", got)
	}
	// An entry already in a full chain still moves up.
	if _, err := s.SetDefaultLLMModel(ctx, refs[4].ID, t0); err != nil {
		t.Fatal(err)
	}
	want := []EntryRef{refs[4], refs[0], refs[1], refs[2], refs[3]}
	if c, _ := mustChain(t, s, ScopeGlobal, 0, "", ""); !slices.Equal(c.Entries, want) {
		t.Errorf("full chain after m5 = %+v; want m5 first", c.Entries)
	}
}
