package store

import (
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// sqlState is the Postgres error code of err, or "".
func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// 0048 only adds: nothing of 0046 changes, and chain A is untouched.
func TestPanelChainsMigrationOnlyAdds(t *testing.T) {
	b, err := os.ReadFile("migrations/0048_panel_chains.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sql.WriteString(line + "\n")
		}
	}
	// A foreign key's ON DELETE is not a delete.
	text := regexp.MustCompile(`(?i)ON\s+DELETE\s+CASCADE`).ReplaceAllString(sql.String(), "")
	for _, bad := range []string{`(?i)\bDROP\b`, `(?i)\bRENAME\b`, `(?i)\bTRUNCATE\b`, `(?i)\bDELETE\b`,
		`(?i)\bUPDATE\b`, `(?i)\bALTER\s+COLUMN\b`, `(?i)\bSET\s+NOT\s+NULL\b`} {
		if regexp.MustCompile(bad).MatchString(text) {
			t.Errorf("migration 0048 matches %s; it may only add", bad)
		}
	}
	for _, stmt := range strings.Split(text, ";") {
		up := strings.ToUpper(stmt)
		if strings.Contains(up, "ADD COLUMN") && strings.Contains(up, "NOT NULL") && !strings.Contains(up, "DEFAULT") {
			t.Errorf("%q adds a NOT NULL column with no default", strings.TrimSpace(stmt))
		}
	}

	s := newStoreAt(t, 46)
	before, beforeIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for _, col := range []string{"model_chains.saved_by_admin", "model_chain_entries.model_id"} {
		if _, ok := before[col]; !ok {
			t.Fatalf("column %s is missing at 0046", col)
		}
	}
	applyUpTo(t, s, 48)
	after, afterIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0048 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	for name, def := range beforeIdx {
		if got, ok := afterIdx[name]; !ok || got != def {
			t.Errorf("index %s after 0048 = %q (present %v); want %q", name, got, ok, def)
		}
	}
	for _, col := range []string{"panel_chains.saved_by_admin", "panel_chain_entries.model_id", "dashboard_repos.panel_chain_mode"} {
		if _, ok := after[col]; !ok {
			t.Errorf("column %s is missing after 0048", col)
		}
	}
	if c := after["dashboard_repos.panel_chain_mode"]; c.Nullable != "NO" {
		t.Errorf("dashboard_repos.panel_chain_mode = %+v; want NOT NULL", c)
	}
}

// The previous release's inserts (0046 era) still work after 0048.
func TestPanelChainsMigrationPreviousRelease(t *testing.T) {
	s := newStoreAt(t, 46)
	applyUpTo(t, s, 48)
	if _, err := s.pool.Exec(ctx, `INSERT INTO dashboard_repos (instance, project, created_by) VALUES ('main', 'acme/api', 'alice')`); err != nil {
		t.Fatalf("0046-era dashboard_repos insert: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO model_chains (scope, instance, project, updated_at)
		VALUES ('repo', 'main', 'acme/api', $1), ('global', '', '', $1)`, t0); err != nil {
		t.Fatalf("0046-era model_chains insert: %v", err)
	}
	var mode string
	if err := s.pool.QueryRow(ctx, `SELECT panel_chain_mode FROM dashboard_repos WHERE project = 'acme/api'`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "inherit" {
		t.Errorf("panel_chain_mode = %q; want inherit", mode)
	}
}

func TestPanelChainOneRowPerTarget(t *testing.T) {
	s := newStoreAt(t, 48)
	ins := `INSERT INTO panel_chains (scope, instance, project, updated_at) VALUES ($1, $2, $3, $4)`
	if _, err := s.pool.Exec(ctx, ins, "global", "", "", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, "global", "", "", t0); sqlState(err) != "23505" {
		t.Errorf("second global row: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ins, "repo", "main", "acme/api", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, "repo", "main", "ACME/api", t0); sqlState(err) != "23505" {
		t.Errorf("same repo, other case: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ins, "team", "", "", t0); sqlState(err) != "23514" {
		t.Errorf("team row with no team: %v; want 23514", err)
	}
}

func TestPanelChainModeValues(t *testing.T) {
	s := newStoreAt(t, 48)
	for i, mode := range []string{"inherit", "custom", "off"} {
		project := []string{"a/a", "b/b", "c/c"}[i]
		if _, err := s.pool.Exec(ctx, `INSERT INTO dashboard_repos (instance, project, panel_chain_mode) VALUES ('main', $1, $2)`, project, mode); err != nil {
			t.Errorf("mode %q: %v", mode, err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO dashboard_repos (instance, project, panel_chain_mode) VALUES ('main', 'd/d', 'other')`); sqlState(err) != "23514" {
		t.Errorf("mode other: %v; want 23514", err)
	}
}

func mustChainB(t *testing.T, s *Store, scope string, teamID int64, instance, project string) (Chain, bool) {
	t.Helper()
	c, found, err := s.ChainOf(ctx, ChainB, scope, teamID, instance, project)
	if err != nil {
		t.Fatalf("ChainOf(ChainB, %s): %v", scope, err)
	}
	return c, found
}

func mustRepo(t *testing.T, s *Store, project string) DashboardRepo {
	t.Helper()
	r, found, err := s.AnyDashboardRepo(ctx, "main", project)
	if err != nil || !found {
		t.Fatalf("AnyDashboardRepo(%s) = %v, %v", project, found, err)
	}
	return r
}

func mustPanelMode(t *testing.T, s *Store, project string) string {
	t.Helper()
	mode, err := s.RepoPanelMode(ctx, "main", project)
	if err != nil {
		t.Fatalf("RepoPanelMode(%s): %v", project, err)
	}
	return mode
}

func addRepos(t *testing.T, s *Store, projects ...string) {
	t.Helper()
	for _, p := range projects {
		if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: p}); err != nil {
			t.Fatal(err)
		}
	}
}

// Chain B is saved and read at the 3 levels in its own tables. A repository
// save makes its chain B custom and leaves chain A's mode and model_id; a
// global save leaves the default.
func TestChainBRoundTrip(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "anthropic", "sonnet", "", nil)
	g2 := newModel(t, s, "openai", "gpt-5", "", nil)
	tm := newTeamModel(t, s, team, "Team key", nil)
	G, G2, T := EntryRef{SourceGlobal, g.ID}, EntryRef{SourceGlobal, g2.ID}, EntryRef{SourceTeam, tm.ID}
	addRepos(t, s, "acme/api", "acme/web")
	if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{G}}, t0); err != nil {
		t.Fatal(err)
	}
	defaultsBefore := defaultsOf(mustModels(t, s))
	settingsBefore, _ := settings(t, s)

	at := t0.Add(time.Hour)
	for _, c := range []Chain{
		{Scope: ScopeGlobal, Entries: []EntryRef{G2, G}, SavedByAdmin: true},
		{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{T, G2}},
		{Scope: ScopeRepo, Instance: "main", Project: "ACME/api", Entries: []EntryRef{G2, T}},
		{Scope: ScopeRepo, Instance: "main", Project: "acme/web", Entries: []EntryRef{T}, SavedByAdmin: true},
	} {
		if err := s.SaveChainOf(ctx, ChainB, c, at); err != nil {
			t.Fatalf("SaveChainOf(ChainB, %s): %v", c.Scope, err)
		}
	}
	if c, found := mustChainB(t, s, ScopeGlobal, 0, "", ""); !found || !slices.Equal(c.Entries, []EntryRef{G2, G}) ||
		!c.SavedByAdmin || !c.UpdatedAt.Equal(at) {
		t.Errorf("global chain B = %+v, %v", c, found)
	}
	if c, found := mustChainB(t, s, ScopeTeam, team, "", ""); !found || !slices.Equal(c.Entries, []EntryRef{T, G2}) ||
		c.TeamID != team || c.SavedByAdmin {
		t.Errorf("team chain B = %+v, %v", c, found)
	}
	if c, found := mustChainB(t, s, ScopeRepo, 0, "main", "acme/API"); !found || !slices.Equal(c.Entries, []EntryRef{G2, T}) {
		t.Errorf("repo chain B = %+v, %v", c, found)
	}

	// Chain A is untouched: no global or team chain A, the repository's
	// chain A is still [G].
	if _, found := mustChain(t, s, ScopeGlobal, 0, "", ""); found {
		t.Error("a chain B save wrote a global chain A")
	}
	if _, found := mustChain(t, s, ScopeTeam, team, "", ""); found {
		t.Error("a chain B save wrote a team chain A")
	}
	if c, found := mustChain(t, s, ScopeRepo, 0, "main", "acme/api"); !found || !slices.Equal(c.Entries, []EntryRef{G}) {
		t.Errorf("repo chain A = %+v, %v; want [G]", c, found)
	}
	if got := defaultsOf(mustModels(t, s)); !slices.Equal(got, defaultsBefore) {
		t.Errorf("defaults = %v; want %v (chain B has no default)", got, defaultsBefore)
	}
	if r, _ := settings(t, s); r.Provider != settingsBefore.Provider || r.Model != settingsBefore.Model || !r.UpdatedAt.Equal(settingsBefore.UpdatedAt) {
		t.Errorf("llm_settings = %+v; want %+v", r, settingsBefore)
	}
	if r := mustRepo(t, s, "acme/api"); r.PanelChainMode != PanelCustom || r.ChainMode != "custom" || r.ModelID == nil || *r.ModelID != g.ID {
		t.Errorf("acme/api = panel %s, chain %s, model %v; want custom, custom, %s", r.PanelChainMode, r.ChainMode, r.ModelID, g.ID)
	}
	if r := mustRepo(t, s, "acme/web"); r.PanelChainMode != PanelCustom || r.ChainMode != "inherit" || r.ModelID != nil {
		t.Errorf("acme/web = panel %s, chain %s, model %v; want custom, inherit, none", r.PanelChainMode, r.ChainMode, r.ModelID)
	}
	if mode := mustPanelMode(t, s, "ACME/web"); mode != PanelCustom {
		t.Errorf("RepoPanelMode(acme/web) = %q; want custom", mode)
	}

	all, err := s.ChainsOf(ctx, ChainB)
	if err != nil || len(all) != 4 || all[0].Scope != ScopeGlobal || all[1].Scope != ScopeTeam {
		t.Errorf("ChainsOf(ChainB) = %+v, %v; want 4 by ID", all, err)
	}
	if a, err := s.Chains(ctx); err != nil || len(a) != 1 || a[0].Scope != ScopeRepo {
		t.Errorf("Chains = %+v, %v; want only the repo chain A", a, err)
	}
	naming, err := s.ChainsNamingOf(ctx, ChainB, T)
	if err != nil || len(naming) != 3 || naming[0].Scope != ScopeTeam {
		t.Errorf("ChainsNamingOf(ChainB, T) = %+v, %v; want team and 2 repos", naming, err)
	}
	if a, err := s.ChainsNaming(ctx, T); err != nil || len(a) != 0 {
		t.Errorf("ChainsNaming(T) = %+v, %v; want none in chain A", a, err)
	}

	// The limit and the team entry check are chain A's.
	six := []EntryRef{G, G, G, G, G, G}
	if err := s.SaveChainOf(ctx, ChainB, Chain{Scope: ScopeGlobal, Entries: six}, at); !errors.Is(err, ErrChainTooLong) {
		t.Errorf("6 entries = %v; want ErrChainTooLong", err)
	}
	if err := s.SaveChainOf(ctx, ChainB, Chain{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{{SourceTeam, "no-such-id"}}}, at); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("a missing team entry = %v; want ErrModelNotFound", err)
	}
}

// Resetting chain B drops the repository's chain B only: chain A stays.
func TestChainBResetInherits(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	G := EntryRef{SourceGlobal, g.ID}
	addRepos(t, s, "acme/api")
	if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{G}}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChainOf(ctx, ChainB, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{G}}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetRepoChainOf(ctx, ChainB, "main", "ACME/api"); err != nil {
		t.Fatal(err)
	}
	if _, found := mustChainB(t, s, ScopeRepo, 0, "main", "acme/api"); found {
		t.Error("the repository chain B is still there")
	}
	if r := mustRepo(t, s, "acme/api"); r.PanelChainMode != PanelInherit || r.ChainMode != "custom" || r.ModelID == nil || *r.ModelID != g.ID {
		t.Errorf("repository = panel %s, chain %s, model %v; want inherit, custom, %s", r.PanelChainMode, r.ChainMode, r.ModelID, g.ID)
	}
	if c, found := mustChain(t, s, ScopeRepo, 0, "main", "acme/api"); !found || !slices.Equal(c.Entries, []EntryRef{G}) {
		t.Errorf("chain A = %+v, %v; want kept", c, found)
	}
}

// custom, then off (the row goes), then reset (inherit), then a save
// (custom again).
func TestChainBRepoOffRoundTrip(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	repoB := Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{{SourceGlobal, g.ID}}}
	addRepos(t, s, "acme/api")
	if mode := mustPanelMode(t, s, "acme/api"); mode != PanelInherit {
		t.Errorf("mode of a new repository = %q; want inherit", mode)
	}
	if mode := mustPanelMode(t, s, "acme/none"); mode != PanelInherit {
		t.Errorf("mode of a repository with no row = %q; want inherit", mode)
	}
	if err := s.SaveChainOf(ctx, ChainB, repoB, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoPanelOff(ctx, "main", "ACME/api", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, found := mustChainB(t, s, ScopeRepo, 0, "main", "acme/api"); found {
		t.Error("off kept the repository chain B")
	}
	if mode := mustPanelMode(t, s, "acme/api"); mode != PanelOff {
		t.Errorf("mode after off = %q; want off", mode)
	}
	if r := mustRepo(t, s, "acme/api"); r.PanelChainMode != PanelOff {
		t.Errorf("PanelChainMode after off = %q; want off", r.PanelChainMode)
	}
	// Off twice is still off.
	if err := s.SetRepoPanelOff(ctx, "main", "acme/api", t0.Add(2*time.Hour)); err != nil || mustPanelMode(t, s, "acme/api") != PanelOff {
		t.Errorf("off twice: %v, mode %q", err, mustPanelMode(t, s, "acme/api"))
	}
	if err := s.ResetRepoChainOf(ctx, ChainB, "main", "acme/api"); err != nil {
		t.Fatal(err)
	}
	if mode := mustPanelMode(t, s, "acme/api"); mode != PanelInherit {
		t.Errorf("mode after reset from off = %q; want inherit", mode)
	}
	if err := s.SetRepoPanelOff(ctx, "main", "acme/api", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChainOf(ctx, ChainB, repoB, t0); err != nil {
		t.Fatal(err)
	}
	if mode := mustPanelMode(t, s, "acme/api"); mode != PanelCustom {
		t.Errorf("mode after a save from off = %q; want custom", mode)
	}
	if _, found := mustChainB(t, s, ScopeRepo, 0, "main", "acme/api"); !found {
		t.Error("the saved chain B is missing")
	}
}

// Turning the panel off writes nothing of chain A, nor the other levels of
// chain B.
func TestChainBOffLeavesChainA(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	G := EntryRef{SourceGlobal, g.ID}
	addRepos(t, s, "acme/api")
	for _, c := range []Chain{
		{Scope: ScopeGlobal, Entries: []EntryRef{G}},
		{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{G}},
		{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{G}},
	} {
		if err := s.SaveChain(ctx, c, t0); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveChainOf(ctx, ChainB, c, t0); err != nil {
			t.Fatal(err)
		}
	}
	chainsBefore, err := s.Chains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := mustRepo(t, s, "acme/api")
	if err := s.SetRepoPanelOff(ctx, "main", "acme/api", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	chainsAfter, err := s.Chains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(chainsAfter) != len(chainsBefore) {
		t.Fatalf("chain A after off = %+v; want %+v", chainsAfter, chainsBefore)
	}
	for i := range chainsBefore {
		a, b := chainsBefore[i], chainsAfter[i]
		if a.ID != b.ID || !slices.Equal(a.Entries, b.Entries) || !a.UpdatedAt.Equal(b.UpdatedAt) || a.SavedByAdmin != b.SavedByAdmin {
			t.Errorf("chain A %d after off = %+v; want %+v", i, b, a)
		}
	}
	after := mustRepo(t, s, "acme/api")
	if after.ChainMode != before.ChainMode || !samePtr(after.ModelID, before.ModelID) {
		t.Errorf("repository after off = chain %s, model %v; want %s, %v", after.ChainMode, after.ModelID, before.ChainMode, before.ModelID)
	}
	if b, err := s.ChainsOf(ctx, ChainB); err != nil || len(b) != 2 || b[0].Scope != ScopeGlobal || b[1].Scope != ScopeTeam {
		t.Errorf("chain B after off = %+v, %v; want the global and team chains kept", b, err)
	}
}

// Deleting a team entry takes it out of chain B too, the later entries moved
// up; a repository that is off stays off.
func TestDeleteTeamModelLeavesNoChainBEntry(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	tm1 := newTeamModel(t, s, team, "One", nil)
	tm2 := newTeamModel(t, s, team, "Two", nil)
	G, T1, T2 := EntryRef{SourceGlobal, g.ID}, EntryRef{SourceTeam, tm1.ID}, EntryRef{SourceTeam, tm2.ID}
	addRepos(t, s, "acme/api", "acme/off")
	if err := s.SaveChain(ctx, Chain{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{T1, G}}, t0); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Chain{
		{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{T1, G, T2}},
		{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{G, T1}},
		{Scope: ScopeRepo, Instance: "main", Project: "acme/off", Entries: []EntryRef{T1}},
	} {
		if err := s.SaveChainOf(ctx, ChainB, c, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetRepoPanelOff(ctx, "main", "acme/off", t0); err != nil {
		t.Fatal(err)
	}
	changed, changedB, err := s.DeleteTeamModel(ctx, team, tm1.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Chain A's changed chains are returned as before, chain B's beside them.
	if len(changed) != 1 || changed[0].Scope != ScopeTeam || !slices.Equal(changed[0].Entries, []EntryRef{G}) {
		t.Errorf("DeleteTeamModel = %+v; want the team chain A", changed)
	}
	if len(changedB) != 2 || changedB[0].Scope != ScopeTeam || !slices.Equal(changedB[0].Entries, []EntryRef{G, T2}) ||
		changedB[1].Scope != ScopeRepo || changedB[1].Project != "acme/api" || !slices.Equal(changedB[1].Entries, []EntryRef{G}) {
		t.Errorf("DeleteTeamModel chain B = %+v; want the team chain B and acme/api's", changedB)
	}
	if c, _ := mustChainB(t, s, ScopeTeam, team, "", ""); !slices.Equal(c.Entries, []EntryRef{G, T2}) {
		t.Errorf("team chain B = %+v; want [G T2]", c.Entries)
	}
	if c, _ := mustChainB(t, s, ScopeRepo, 0, "main", "acme/api"); !slices.Equal(c.Entries, []EntryRef{G}) {
		t.Errorf("repo chain B = %+v; want [G]", c.Entries)
	}
	if mode := mustPanelMode(t, s, "acme/api"); mode != PanelCustom {
		t.Errorf("acme/api mode = %q; want custom", mode)
	}
	if mode := mustPanelMode(t, s, "acme/off"); mode != PanelOff {
		t.Errorf("acme/off mode = %q; want off", mode)
	}
	if cs, err := s.ChainsNamingOf(ctx, ChainB, T1); err != nil || len(cs) != 0 {
		t.Errorf("chain B naming the deleted entry = %+v, %v", cs, err)
	}
}

// Deleting a team takes its entries out of the repository chain B, and its
// own chain B goes with it.
func TestDeleteTeamLeavesNoChainBEntry(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	tm := newTeamModel(t, s, team, "One", nil)
	G, T := EntryRef{SourceGlobal, g.ID}, EntryRef{SourceTeam, tm.ID}
	addRepos(t, s, "acme/api")
	for _, c := range []Chain{
		{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{T}},
		{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{T, G}},
	} {
		if err := s.SaveChainOf(ctx, ChainB, c, t0); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.DeleteTeam(ctx, team, nil); err != nil || !ok {
		t.Fatalf("DeleteTeam = %v, %v", ok, err)
	}
	all, err := s.ChainsOf(ctx, ChainB)
	if err != nil || len(all) != 1 || all[0].Scope != ScopeRepo || !slices.Equal(all[0].Entries, []EntryRef{G}) {
		t.Errorf("chain B after DeleteTeam = %+v, %v; want the repo chain [G]", all, err)
	}
}

// A removed global entry stays in chain B, as in chain A: the resolver skips
// it.
func TestRemoveGlobalModelKeepsChainBEntry(t *testing.T) {
	s := newStore(t)
	g1 := newModel(t, s, "anthropic", "sonnet", "", nil)
	g2 := newModel(t, s, "openai", "gpt-5", "", nil)
	want := []EntryRef{{SourceGlobal, g2.ID}, {SourceGlobal, g1.ID}}
	addRepos(t, s, "acme/api")
	for _, c := range []Chain{
		{Scope: ScopeGlobal, Entries: want},
		{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: want},
	} {
		if err := s.SaveChainOf(ctx, ChainB, c, t0); err != nil {
			t.Fatal(err)
		}
	}
	if defaultsOf(mustModels(t, s))[0] == g2.ID {
		t.Fatal("g2 is the default; the test needs a non-default entry")
	}
	if err := s.DeleteLLMModel(ctx, g2.ID, false); err != nil {
		t.Fatal(err)
	}
	if c, _ := mustChainB(t, s, ScopeGlobal, 0, "", ""); !slices.Equal(c.Entries, want) {
		t.Errorf("global chain B = %+v; want %+v kept", c.Entries, want)
	}
	if c, _ := mustChainB(t, s, ScopeRepo, 0, "main", "acme/api"); !slices.Equal(c.Entries, want) {
		t.Errorf("repo chain B = %+v; want %+v kept", c.Entries, want)
	}
}

// Removing a repository drops its own chain B too, so one added again
// starts by inheriting.
func TestRemoveRepoDropsItsChainB(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	addRepos(t, s, "acme/api")
	if err := s.SaveChainOf(ctx, ChainB, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{{SourceGlobal, g.ID}}}, t0); err != nil {
		t.Fatal(err)
	}
	if found, err := s.RemoveDashboardRepo(ctx, "main", "ACME/api"); err != nil || !found {
		t.Fatalf("RemoveDashboardRepo = %v, %v", found, err)
	}
	if _, found := mustChainB(t, s, ScopeRepo, 0, "main", "acme/api"); found {
		t.Error("the removed repository's chain B is still there")
	}
}

// Deleting a team entry no chain B names returns no chain B.
func TestDeleteTeamModelNoChainB(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	tm := newTeamModel(t, s, team, "One", nil)
	if err := s.SaveChain(ctx, Chain{Scope: ScopeTeam, TeamID: team, Entries: []EntryRef{{SourceTeam, tm.ID}}}, t0); err != nil {
		t.Fatal(err)
	}
	changed, changedB, err := s.DeleteTeamModel(ctx, team, tm.ID)
	if err != nil || len(changed) != 1 || len(changedB) != 0 {
		t.Errorf("DeleteTeamModel = %+v, %+v, %v; want the team chain A only", changed, changedB, err)
	}
}

// [Ruling 3c] A chain set that is neither ChainA nor ChainB is an error, and
// nothing changes.
func TestResetRepoChainOfUnknownSet(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	addRepos(t, s, "acme/api")
	repoB := Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{{SourceGlobal, g.ID}}}
	if err := s.SaveChainOf(ctx, ChainB, repoB, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetRepoChainOf(ctx, ChainSet{}, "main", "acme/api"); err == nil {
		t.Error("ResetRepoChainOf(ChainSet{}) = nil; want an error")
	}
	if _, found := mustChainB(t, s, ScopeRepo, 0, "main", "acme/api"); !found || mustPanelMode(t, s, "acme/api") != PanelCustom {
		t.Error("an unknown set changed the repository's chain B")
	}
}

// [Ruling 4] Off on a repository with no dashboard row (the config file's)
// keeps its chain B row with no entries, which reads as off; a save then
// gives it entries again (custom) and a reset drops the row (inherit).
func TestSetRepoPanelOffNoRow(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	repoB := Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/config", Entries: []EntryRef{{SourceGlobal, g.ID}}}
	if err := s.SaveChainOf(ctx, ChainB, repoB, t0); err != nil {
		t.Fatal(err)
	}
	if mode := mustPanelMode(t, s, "acme/config"); mode != PanelInherit {
		t.Errorf("mode with entries and no dashboard row = %q; want inherit (its column)", mode)
	}
	if err := s.SetRepoPanelOff(ctx, "main", "ACME/config", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	c, found := mustChainB(t, s, ScopeRepo, 0, "main", "acme/config")
	if !found || len(c.Entries) != 0 || !c.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("chain B after off = %+v, %v; want the row with no entries", c, found)
	}
	if mode := mustPanelMode(t, s, "acme/config"); mode != PanelOff {
		t.Errorf("mode after off = %q; want off", mode)
	}
	// Off twice is one row, still off.
	if err := s.SetRepoPanelOff(ctx, "main", "acme/config", t0); err != nil || mustPanelMode(t, s, "acme/config") != PanelOff {
		t.Errorf("off twice: %v", err)
	}
	if all, _ := s.ChainsOf(ctx, ChainB); len(all) != 1 {
		t.Errorf("chain B rows = %+v; want 1", all)
	}
	if err := s.SaveChainOf(ctx, ChainB, repoB, t0); err != nil {
		t.Fatal(err)
	}
	if mode := mustPanelMode(t, s, "acme/config"); mode != PanelInherit {
		t.Errorf("mode after a save from off = %q; want not off", mode)
	}
	if err := s.SetRepoPanelOff(ctx, "main", "acme/config", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetRepoChainOf(ctx, ChainB, "main", "acme/config"); err != nil {
		t.Fatal(err)
	}
	if _, found := mustChainB(t, s, ScopeRepo, 0, "main", "acme/config"); found {
		t.Error("the reset kept the chain B row")
	}
	if mode := mustPanelMode(t, s, "acme/config"); mode != PanelInherit {
		t.Errorf("mode after reset = %q; want inherit", mode)
	}
}

// [Ruling 4] An empty repository chain B means off, so a repository chain B
// that loses its last entry to a removed team entry goes: the repository
// inherits, it is not turned off.
func TestDeleteTeamModelEmptiesNoRepoChainB(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	tm := newTeamModel(t, s, team, "One", nil)
	addRepos(t, s, "acme/api")
	for _, project := range []string{"acme/api", "acme/config"} {
		if err := s.SaveChainOf(ctx, ChainB, Chain{Scope: ScopeRepo, Instance: "main", Project: project, Entries: []EntryRef{{SourceTeam, tm.ID}}}, t0); err != nil {
			t.Fatal(err)
		}
	}
	_, changedB, err := s.DeleteTeamModel(ctx, team, tm.ID)
	if err != nil || len(changedB) != 2 {
		t.Fatalf("DeleteTeamModel chain B = %+v, %v; want both repositories", changedB, err)
	}
	for _, project := range []string{"acme/api", "acme/config"} {
		if _, found := mustChainB(t, s, ScopeRepo, 0, "main", project); found {
			t.Errorf("%s: the emptied chain B row is still there", project)
		}
		if mode := mustPanelMode(t, s, project); mode != PanelInherit {
			t.Errorf("%s: mode = %q after its last entry went; want inherit", project, mode)
		}
	}
	if r := mustRepo(t, s, "acme/api"); r.PanelChainMode != PanelInherit {
		t.Errorf("acme/api's row = %q; want inherit, not custom with no chain", r.PanelChainMode)
	}
}

// A team entry's removal locks the repository's row before its chain B, as
// a save does, so a save that holds the row and then writes the chain
// does not deadlock with it.
func TestDeleteTeamModelLocksTheRepoFirst(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	tm := newTeamModel(t, s, team, "One", nil)
	addRepos(t, s, "acme/api")
	if err := s.SaveChainOf(ctx, ChainB, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{{SourceTeam, tm.ID}}}, t0); err != nil {
		t.Fatal(err)
	}
	holder, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // committed below
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM dashboard_repos WHERE instance = 'main' AND project = 'acme/api' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, _, err := s.DeleteTeamModel(ctx, team, tm.ID)
		deleted <- err
	}()
	waitBlocked(t, s, 0, holderPID)
	if _, err := holder.Exec(ctx, `UPDATE `+ChainB.chains+` SET updated_at = now()
		WHERE scope = 'repo' AND instance = 'main' AND project = 'acme/api'`); err != nil {
		t.Fatalf("the save's chain write: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteTeamModel: %v", err)
	}
	if mode := mustPanelMode(t, s, "acme/api"); mode != PanelInherit {
		t.Errorf("mode = %q; want inherit", mode)
	}
}

// Two team entries of one team, removed at once, with the repositories
// crosswise: repository a has chain A [m1] and chain B [m2], repository b
// has chain A [m2] and chain B [m1]. Without a team lock each removal holds
// one repository row from its chain A pass and wants the other for its
// chain B pass: a deadlock (40P01). The holder stalls both removals between
// the 2 passes (it holds the chain B entries table), so the order is fixed.
func TestDeleteTeamModelOneAtATimePerTeam(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	m1 := newTeamModel(t, s, team, "One", nil)
	m2 := newTeamModel(t, s, team, "Two", nil)
	addRepos(t, s, "acme/a", "acme/b")
	save := func(set ChainSet, project string, m TeamModel) {
		t.Helper()
		if err := s.SaveChainOf(ctx, set, Chain{Scope: ScopeRepo, Instance: "main", Project: project, Entries: []EntryRef{{SourceTeam, m.ID}}}, t0); err != nil {
			t.Fatal(err)
		}
	}
	save(ChainA, "acme/a", m1)
	save(ChainB, "acme/a", m2)
	save(ChainA, "acme/b", m2)
	save(ChainB, "acme/b", m1)
	holder, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // committed below
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE `+ChainB.entries+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := s.DeleteTeamModel(ctx, team, m1.ID)
		first <- err
	}()
	firstPID := waitBlocked(t, s, 0, holderPID)
	go func() {
		_, _, err := s.DeleteTeamModel(ctx, team, m2.ID)
		second <- err
	}()
	// The second waits on the holder (no team lock) or on the first (team lock).
	waitBlocked(t, s, firstPID, holderPID, firstPID)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Errorf("first DeleteTeamModel: %v", err)
	}
	if err := <-second; err != nil {
		t.Errorf("second DeleteTeamModel: %v", err)
	}
	for _, project := range []string{"acme/a", "acme/b"} {
		if _, found := mustChainB(t, s, ScopeRepo, 0, "main", project); found {
			t.Errorf("%s: chain B row is still there", project)
		}
	}
}

// waitBlocked waits until a backend other than skip is blocked by one of
// pids, and returns it.
func waitBlocked(t *testing.T, s *Store, skip int32, pids ...int32) int32 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var pid int32
		err := s.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
			WHERE pid <> $1 AND pg_blocking_pids(pid) && $2::int[] LIMIT 1`, skip, pids).Scan(&pid)
		if err == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no backend waited on the repository's row")
	return 0
}

// Only SetRepoPanelOff makes the empty repository chain B that means off: a
// repository save with no entries is refused, and stores nothing. A global
// or team chain B may be saved empty.
func TestChainBEmptyRepoSaveRefused(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	addRepos(t, s, "acme/api")
	for _, project := range []string{"acme/api", "acme/config"} {
		err := s.SaveChainOf(ctx, ChainB, Chain{Scope: ScopeRepo, Instance: "main", Project: project}, t0)
		if !errors.Is(err, ErrEmptyRepoPanelChain) {
			t.Errorf("%s: empty save = %v; want ErrEmptyRepoPanelChain", project, err)
		}
		if _, found := mustChainB(t, s, ScopeRepo, 0, "main", project); found {
			t.Errorf("%s: a refused save stored a row", project)
		}
		if mode := mustPanelMode(t, s, project); mode != PanelInherit {
			t.Errorf("%s: mode = %q; want inherit", project, mode)
		}
	}
	for _, c := range []Chain{{Scope: ScopeGlobal}, {Scope: ScopeTeam, TeamID: team}} {
		if err := s.SaveChainOf(ctx, ChainB, c, t0); err != nil {
			t.Errorf("empty %s chain B = %v", c.Scope, err)
		}
	}
	if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api"}, t0); err != nil {
		t.Errorf("empty repository chain A = %v; chain A is unchanged", err)
	}
}

// [Ruling 3d] A chain B save and an off that wait on the repository's row,
// save first, end as off with no chain B row: both lock the row before they
// touch the chain.
func TestChainBSaveThenOffLocksTheRepoFirst(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	addRepos(t, s, "acme/api")
	holder, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // released below
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM dashboard_repos WHERE instance = 'main' AND project = 'acme/api' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	blockedBy := func(skip int32, pids ...int32) int32 { return waitBlocked(t, s, skip, pids...) }
	saved, off := make(chan error, 1), make(chan error, 1)
	go func() {
		saved <- s.SaveChainOf(ctx, ChainB, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api",
			Entries: []EntryRef{{SourceGlobal, g.ID}}}, t0)
	}()
	savePID := blockedBy(0, holderPID)
	go func() { off <- s.SetRepoPanelOff(ctx, "main", "acme/api", t0) }()
	blockedBy(savePID, holderPID, savePID)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := <-off; err != nil {
		t.Fatalf("off: %v", err)
	}
	if mode := mustPanelMode(t, s, "acme/api"); mode != PanelOff {
		t.Errorf("mode = %q; want off", mode)
	}
	if _, found := mustChainB(t, s, ScopeRepo, 0, "main", "acme/api"); found {
		t.Error("off with a chain B row left")
	}
}
