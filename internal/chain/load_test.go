package chain

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/store/storetest"
)

var ctx = context.Background()

const testDBEnv = "PERGENT_SH_TEST_DATABASE_URL"

// newStore opens a migrated store on a database of its own, which goes back
// to the pool when the test ends. It skips when no test database is set.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t, store.OpenInSchema)
}

const (
	inst    = "main"
	project = "acme/api"
)

var repo = settings.Effective{Instance: inst, Project: project}

// loadEnv is a store with a team that owns the repository, 2 stored global
// entries (the first is the default) and a registry over them.
type loadEnv struct {
	st   *store.Store
	reg  *instances.Registry
	team int64
	a, b store.EntryRef
}

func newLoadEnv(t *testing.T, cfg *config.Config) *loadEnv {
	t.Helper()
	st := newStore(t)
	e := &loadEnv{st: st}
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

func (e *loadEnv) repoRow(t *testing.T) {
	t.Helper()
	if err := e.st.UpsertDashboardRepo(ctx, store.DashboardRepo{Instance: inst, Project: project, CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
}

func (e *loadEnv) save(t *testing.T, c store.Chain) {
	t.Helper()
	if err := e.st.SaveChain(ctx, c, now); err != nil {
		t.Fatalf("SaveChain: %v", err)
	}
}

// The owner team's chain is in effect when the repository has none.
func TestLoadTeamChain(t *testing.T) {
	e := newLoadEnv(t, &config.Config{})
	e.save(t, store.Chain{Scope: store.ScopeTeam, TeamID: e.team, Entries: []store.EntryRef{e.a}})
	in, err := Load(ctx, e.st, e.reg, now, store.Run{}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if in.Scope != store.ScopeTeam || !slices.Equal(in.Refs, []store.EntryRef{e.a}) {
		t.Fatalf("chain A: %+v %q", in.Refs, in.Scope)
	}
}

// spent records a review of acme/api that ran on ref and used tokens now.
func spent(t *testing.T, st *store.Store, iid int64, ref store.EntryRef, tokens int64) {
	t.Helper()
	mr := store.MRKey{Instance: inst, ProjectID: 1, IID: iid}
	id, _, err := st.EnqueueRun(ctx, mr, project, fmt.Sprintf("spent%d", iid), "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimQueued(ctx, 10, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunEntry(ctx, id, store.RunEntry{Source: ref.Source, ID: ref.ID, Name: ref.ID, Position: 1, Length: 1}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("tok-" + id))
	if err := st.MarkDispatched(ctx, id, "job-"+id, "base", "start", sum[:], now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if out, err := st.StoreResult(ctx, id, "tok-"+id, []byte(`{"schema_version":1,"status":"completed","skills":[]}`), now); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := st.RecordUsage(ctx, id, []store.SkillUsage{{Skill: "a", Verdict: "no_blocking_issues", In: tokens}}); err != nil {
		t.Fatal(err)
	}
}

func limit(t *testing.T, st *store.Store, ref store.EntryRef, team, tokens int64) {
	t.Helper()
	if err := st.SetModelLimit(ctx, store.ModelLimit{Ref: ref, TeamID: team, MonthlyTokens: tokens, UpdatedAt: now, UpdatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
}

// Load reads the limits of this month: the model limits, and the owner
// team's limits only. A repository with no owner team gets no team limit.
func TestLoadReadsLimits(t *testing.T) {
	e := newLoadEnv(t, &config.Config{})
	spent(t, e.st, 1, e.a, 10)
	spent(t, e.st, 2, e.b, 10)
	limit(t, e.st, e.a, 0, 10)      // used up
	limit(t, e.st, e.b, e.team, 10) // the owner team's, used up
	in, err := Load(ctx, e.st, e.reg, now, store.Run{}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !in.ModelLimitUsedUp[e.a] || in.ModelLimitUsedUp[e.b] || !in.TeamLimitUsedUp[e.b] {
		t.Fatalf("owned: model %v, team %v", in.ModelLimitUsedUp, in.TeamLimitUsedUp)
	}
	other := settings.Effective{Instance: inst, Project: "acme/other"}
	in, err = Load(ctx, e.st, e.reg, now, store.Run{}, other)
	if err != nil {
		t.Fatal(err)
	}
	if !in.ModelLimitUsedUp[e.a] || len(in.TeamLimitUsedUp) != 0 {
		t.Fatalf("no owner: model %v, team %v; want the model limit only", in.ModelLimitUsedUp, in.TeamLimitUsedUp)
	}
}
