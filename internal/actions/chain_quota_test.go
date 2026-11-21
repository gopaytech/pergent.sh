package actions

import (
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// teamChainModels is a registry over st where a team owns main/acme/web and
// its chain is a global entry teams may use, then (with teamEntry) the
// team's own entry with its key.
func teamChainModels(t *testing.T, st *store.Store, teamEntry bool) *instances.Registry {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(k))
	if err != nil {
		t.Fatal(err)
	}
	team, err := st.CreateTeam(ctx, "Payments", quotaNow)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UpdateTeam(ctx, team.ID, func(tm *store.Team) error {
		tm.Repos = []store.TeamRepo{{Instance: "main", Project: "acme/web", Owner: true}}
		return nil
	}); err != nil || !ok {
		t.Fatalf("UpdateTeam = %v, %v", ok, err)
	}
	g, err := st.CreateLLMModel(ctx, store.LLMModel{Name: "global", Provider: "openai", Model: "gpt-global",
		APIKeySealed: box.Seal([]byte("key-global")), TeamsMayUse: true}, quotaNow)
	if err != nil {
		t.Fatal(err)
	}
	refs := []store.EntryRef{{Source: store.SourceGlobal, ID: g.ID}}
	if teamEntry {
		m, err := st.CreateTeamModel(ctx, store.TeamModel{TeamID: team.ID, Name: "team-a", Provider: "openai", Model: "gpt-a",
			APIKeySealed: box.Seal([]byte("key-team-a"))}, quotaNow)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, store.EntryRef{Source: store.SourceTeam, ID: m.ID})
	}
	if err := st.SaveChain(ctx, store.Chain{Scope: store.ScopeTeam, TeamID: team.ID, Entries: refs}, quotaNow); err != nil {
		t.Fatal(err)
	}
	reg := instances.New(&config.Config{}, st, box)
	if err := reg.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}
	return reg
}

// instanceQuotaRepo is main/acme/web with its instance's quota of 10 tokens
// a month and roomy caps.
func instanceQuotaRepo() settings.Effective {
	q := int64(10)
	return settings.Effective{Instance: "main", Project: "acme/web", InstanceMonthlyTokenQuota: &q, RerunLimitPerMR: 10, RerunLimitPerRepo: 50}
}

// With the instance quota used up, a re-run and a start go ahead when the
// resolver still finds a team entry (owner ruling 2026-11-20), and are
// refused as before when it finds none.
func TestRerunAndStartUnderInstanceQuotaAskTheChain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		teamEntry bool
		want      RerunStatus
	}{
		{"team entry left", true, RerunQueued},
		{"global entries only", false, RerunQuotaUsedUp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			seedUsage(t, st, 1000, quotaNow.Add(-time.Hour), 10)
			reg := teamChainModels(t, st, tc.teamEntry)

			req := rerunRequest(instanceQuotaRepo())
			req.Models = reg
			if res, err := Rerun(ctx, st, req); err != nil || res.Status != tc.want {
				t.Fatalf("Rerun = %+v, %v; want %v", res, err, tc.want)
			}

			st2 := newStore(t)
			seedUsage(t, st2, 1000, quotaNow.Add(-time.Hour), 10)
			sreq := startRequest(instanceQuotaRepo())
			sreq.Models = teamChainModels(t, st2, tc.teamEntry)
			if res, err := Start(ctx, st2, sreq); err != nil || res.Status != tc.want {
				t.Fatalf("Start = %+v, %v; want %v", res, err, tc.want)
			}
		})
	}
}

// A team entry left does not help a used-up repository quota: it counts
// every token.
func TestRerunUnderRepoQuotaStillRefused(t *testing.T) {
	st := newStore(t)
	seedUsage(t, st, 1000, quotaNow.Add(-time.Hour), 10)
	repo := quotaRepo()
	repo.Instance, repo.Project = "main", "acme/web"
	req := rerunRequest(repo)
	req.Models = teamChainModels(t, st, true)
	if res, err := Rerun(ctx, st, req); err != nil || res.Status != RerunQuotaUsedUp {
		t.Fatalf("Rerun = %+v, %v; want RerunQuotaUsedUp", res, err)
	}
}
