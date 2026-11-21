package chain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

var ctx = context.Background()

const testDBEnv = "PERGENT_SH_TEST_DATABASE_URL"

// newStore opens a migrated store in a fresh schema, dropped when the test
// ends. It skips when no test database is set.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	dbURL := os.Getenv(testDBEnv)
	if dbURL == "" {
		t.Skipf("%s is not set", testDBEnv)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenInSchema(ctx, dbURL, "test_chain_"+hex.EncodeToString(b))
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	t.Cleanup(func() {
		_ = st.DropSchema(context.Background())
		st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

const (
	inst    = "main"
	project = "acme/api"
)

var repo = settings.Effective{Instance: inst, Project: project}

// panelEnv is a store with a team that owns the repository, 3 stored global
// entries (the first is the default) and a registry over them.
type panelEnv struct {
	st   *store.Store
	reg  *instances.Registry
	team int64
	a, b store.EntryRef
}

func newPanelEnv(t *testing.T, cfg *config.Config) *panelEnv {
	t.Helper()
	st := newStore(t)
	e := &panelEnv{st: st}
	mk := func(model string) store.EntryRef {
		m, err := st.CreateLLMModel(ctx, store.LLMModel{Provider: "openai", Model: model}, now)
		if err != nil {
			t.Fatal(err)
		}
		return g(m.ID)
	}
	e.a, e.b = mk("a"), mk("b")
	tm, err := st.CreateTeam(ctx, "t", now)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := st.UpdateTeam(ctx, tm.ID, func(team *store.Team) error {
		team.Repos = append(team.Repos, store.TeamRepo{Instance: inst, Project: project, Owner: true})
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("UpdateTeam = %v, %v", ok, err)
	}
	e.team = tm.ID
	e.reg = instances.New(cfg, st, nil)
	if err := e.reg.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *panelEnv) save(t *testing.T, set store.ChainSet, c store.Chain) {
	t.Helper()
	if err := e.st.SaveChainOf(ctx, set, c, now); err != nil {
		t.Fatalf("SaveChainOf: %v", err)
	}
}

func (e *panelEnv) repoRow(t *testing.T) {
	t.Helper()
	if err := e.st.UpsertDashboardRepo(ctx, store.DashboardRepo{Instance: inst, Project: project, CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
}

func (e *panelEnv) loadB(t *testing.T, run store.Run) Input {
	t.Helper()
	in, err := LoadSet(ctx, e.st, e.reg, now, run, repo, store.ChainB)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	return in
}

func TestLoadChainBNoneIsOff(t *testing.T) {
	e := newPanelEnv(t, &config.Config{LLM: config.LLM{Provider: "openai", Model: "cfg"}})
	in := e.loadB(t, store.Run{})
	if in.Refs != nil || in.Scope != "" {
		t.Fatalf("in = %+v, %q; want no chain", in.Refs, in.Scope)
	}
	// Chain A still falls to the global level, where the config entry is.
	a, err := LoadSet(ctx, e.st, e.reg, now, store.Run{}, repo, store.ChainA)
	if err != nil || a.Scope != store.ScopeGlobal {
		t.Fatalf("chain A scope = %q, %v", a.Scope, err)
	}
}

func TestLoadChainBRepoBeforeTeamBeforeGlobal(t *testing.T) {
	e := newPanelEnv(t, &config.Config{})
	e.save(t, store.ChainB, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{e.a}})
	in := e.loadB(t, store.Run{})
	if in.Scope != store.ScopeGlobal || !slices.Equal(in.Refs, []store.EntryRef{e.a}) || in.OwnerTeam != e.team {
		t.Fatalf("global: %+v %q team %d", in.Refs, in.Scope, in.OwnerTeam)
	}
	e.save(t, store.ChainB, store.Chain{Scope: store.ScopeTeam, TeamID: e.team, Entries: []store.EntryRef{e.b}})
	in = e.loadB(t, store.Run{})
	if in.Scope != store.ScopeTeam || !slices.Equal(in.Refs, []store.EntryRef{e.b}) {
		t.Fatalf("team: %+v %q", in.Refs, in.Scope)
	}
	e.repoRow(t)
	e.save(t, store.ChainB, store.Chain{Scope: store.ScopeRepo, Instance: inst, Project: project, Entries: []store.EntryRef{e.a, e.b}})
	in = e.loadB(t, store.Run{})
	if in.Scope != store.ScopeRepo || !slices.Equal(in.Refs, []store.EntryRef{e.a, e.b}) {
		t.Fatalf("repo: %+v %q", in.Refs, in.Scope)
	}
	// Chain A is untouched by all of this.
	a, err := LoadSet(ctx, e.st, e.reg, now, store.Run{}, repo, store.ChainA)
	if err != nil || a.Scope != store.ScopeGlobal {
		t.Fatalf("chain A scope = %q, %v", a.Scope, err)
	}
}

func TestLoadChainBRepoOffBeatsTeam(t *testing.T) {
	e := newPanelEnv(t, &config.Config{})
	e.repoRow(t)
	e.save(t, store.ChainB, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{e.a}})
	e.save(t, store.ChainB, store.Chain{Scope: store.ScopeTeam, TeamID: e.team, Entries: []store.EntryRef{e.b}})
	if err := e.st.SetRepoPanelOff(ctx, inst, project, now); err != nil {
		t.Fatal(err)
	}
	in := e.loadB(t, store.Run{})
	if in.Refs != nil || in.Scope != "" {
		t.Fatalf("off: %+v %q", in.Refs, in.Scope)
	}
}

func TestLoadChainBAPIReadsGlobal(t *testing.T) {
	e := newPanelEnv(t, &config.Config{})
	e.repoRow(t)
	e.save(t, store.ChainB, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{e.a}})
	e.save(t, store.ChainB, store.Chain{Scope: store.ScopeTeam, TeamID: e.team, Entries: []store.EntryRef{e.b}})
	if err := e.st.SetRepoPanelOff(ctx, inst, project, now); err != nil {
		t.Fatal(err)
	}
	in := e.loadB(t, store.Run{Kind: store.KindAPI})
	if in.Scope != store.ScopeGlobal || !slices.Equal(in.Refs, []store.EntryRef{e.a}) {
		t.Fatalf("api: %+v %q", in.Refs, in.Scope)
	}
}

func TestLoadChainBTriedIsTheCallers(t *testing.T) {
	e := newPanelEnv(t, &config.Config{})
	other := "7"
	in := e.loadB(t, store.Run{AttemptOf: &other})
	if in.Tried != nil {
		t.Fatalf("Tried = %v, want nil", in.Tried)
	}
}

func TestLoadChainAUnchanged(t *testing.T) {
	e := newPanelEnv(t, &config.Config{})
	e.save(t, store.ChainB, store.Chain{Scope: store.ScopeTeam, TeamID: e.team, Entries: []store.EntryRef{e.b}})
	e.save(t, store.ChainA, store.Chain{Scope: store.ScopeTeam, TeamID: e.team, Entries: []store.EntryRef{e.a}})
	in, err := Load(ctx, e.st, e.reg, now, store.Run{}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if in.Scope != store.ScopeTeam || !slices.Equal(in.Refs, []store.EntryRef{e.a}) {
		t.Fatalf("chain A: %+v %q", in.Refs, in.Scope)
	}
}
