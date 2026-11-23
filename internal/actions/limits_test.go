package actions

import (
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// limitRepo is main/acme/web with roomy caps and no quota.
func limitRepo() settings.Effective {
	return settings.Effective{Instance: "main", Project: "acme/web", RerunLimitPerMR: 10, RerunLimitPerRepo: 50}
}

func setLimit(t *testing.T, st *store.Store, ref store.EntryRef, team, tokens int64) {
	t.Helper()
	if err := st.SetModelLimit(ctx, store.ModelLimit{Ref: ref, TeamID: team, MonthlyTokens: tokens, UpdatedAt: quotaNow, UpdatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
}

// With no quota used up, a maintainer's re-run and start ask the limits: they
// go ahead while an entry has room, and are refused as for the quota when
// limits alone stop every entry.
func TestRequestsByHandAskTheLimits(t *testing.T) {
	st := newStore(t)
	m := newChainModels(t, st, true)
	seedUsageOn(t, st, 1000, quotaNow.Add(-time.Hour), 10, &m.global)
	seedUsageOn(t, st, 1001, quotaNow.Add(-time.Hour), 10, &m.own)
	setLimit(t, st, m.global, 0, 10)

	req := rerunRequest(limitRepo())
	req.Models = m.reg
	if res, err := Rerun(ctx, st, req); err != nil || res.Status != RerunQueued {
		t.Fatalf("team entry with room: Rerun = %+v, %v; want queued", res, err)
	}
	setLimit(t, st, m.own, m.team, 10)
	req.Head = "head2"
	if res, err := Rerun(ctx, st, req); err != nil || res.Status != RerunQuotaUsedUp {
		t.Fatalf("every entry at a limit: Rerun = %+v, %v; want RerunQuotaUsedUp", res, err)
	}
	sreq := startRequest(limitRepo())
	sreq.Models, sreq.Head = m.reg, "head3"
	if res, err := Start(ctx, st, sreq); err != nil || res.Status != RerunQuotaUsedUp {
		t.Fatalf("Start = %+v, %v; want RerunQuotaUsedUp", res, err)
	}
}

// An admin's request goes past the admin limits to the first global entry
// with no mark, and is refused, naming the team, only when nothing but team
// entries at their team's own limit is left.
func TestAdminRequestStopsAtTeamOwnLimit(t *testing.T) {
	st := newStore(t)
	m := newChainModels(t, st, true)
	seedUsageOn(t, st, 1000, quotaNow.Add(-time.Hour), 10, &m.global)
	seedUsageOn(t, st, 1001, quotaNow.Add(-time.Hour), 10, &m.own)
	setLimit(t, st, m.global, 0, 10)
	setLimit(t, st, m.global, m.team, 10)
	setLimit(t, st, m.own, m.team, 10)

	req := rerunRequest(limitRepo())
	req.Models, req.PastQuota = m.reg, true
	if res, err := Rerun(ctx, st, req); err != nil || res.Status != RerunQueued {
		t.Fatalf("a global entry past the admin limits: Rerun = %+v, %v; want queued", res, err)
	}
	if err := st.SaveChain(ctx, store.Chain{Scope: store.ScopeTeam, TeamID: m.team, Entries: []store.EntryRef{m.own}}, quotaNow); err != nil {
		t.Fatal(err)
	}
	req.Head = "head2"
	if res, err := Rerun(ctx, st, req); err != nil || res.Status != RerunTeamLimit || res.Team != "Payments" {
		t.Fatalf("only the team's entry: Rerun = %+v, %v; want RerunTeamLimit for Payments", res, err)
	}
	sreq := startRequest(limitRepo())
	sreq.Models, sreq.PastQuota, sreq.Head = m.reg, true, "head3"
	if res, err := Start(ctx, st, sreq); err != nil || res.Status != RerunTeamLimit || res.Team != "Payments" {
		t.Fatalf("Start = %+v, %v; want RerunTeamLimit for Payments", res, err)
	}
}
