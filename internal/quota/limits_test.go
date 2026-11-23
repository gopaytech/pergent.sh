package quota

import (
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// Usage per entry: the total counts every row, a team's sum its own rows
// only; a limit is used up at or above it; only the owner team's limits are
// read, and with no owner team none is.
func TestLimitsFrom(t *testing.T) {
	g := store.EntryRef{Source: store.SourceGlobal, ID: "g"}
	h := store.EntryRef{Source: store.SourceGlobal, ID: "h"}
	tm := store.EntryRef{Source: store.SourceTeam, ID: "t"}
	u := usageOf([]store.EntryTokens{
		{Ref: g, OwnerTeam: 7, Tokens: 60}, {Ref: g, Tokens: 40}, {Ref: tm, OwnerTeam: 7, Tokens: 9}, {Ref: h, OwnerTeam: 8, Tokens: 5},
	})
	for _, c := range []struct {
		ref  store.EntryRef
		team int64
		want int64
	}{{g, 0, 100}, {g, 7, 60}, {g, 8, 0}, {tm, 7, 9}, {tm, 0, 9}, {h, 7, 0}} {
		if got := u.Used(c.ref, c.team); got != c.want {
			t.Errorf("Used(%s, %d) = %d, want %d", c.ref.ID, c.team, got, c.want)
		}
	}
	limits := []store.ModelLimit{
		{Ref: g, MonthlyTokens: 100},           // at the limit: used up
		{Ref: g, TeamID: 7, MonthlyTokens: 61}, // one under: room
		{Ref: tm, TeamID: 7, MonthlyTokens: 9}, // used up
		{Ref: h, TeamID: 8, MonthlyTokens: 5},  // another team's
		{Ref: h, MonthlyTokens: 6},             // room
	}
	l := limitsFrom(limits, u, 7)
	if !l.Model[g] || l.Model[h] || l.Team[g] || !l.Team[tm] || l.Team[h] {
		t.Errorf("team 7: %+v", l)
	}
	if l := limitsFrom(limits, u, 0); len(l.Team) != 0 || !l.Model[g] {
		t.Errorf("no owner team: %+v; want the model limit only", l)
	}
	if l := limitsFrom(limits, u, 8); !l.Team[h] {
		t.Errorf("team 8: %+v; want its limit on h used up", l)
	}
	if !needsUsage(limits, 0) || needsUsage([]store.ModelLimit{{Ref: h, TeamID: 8, MonthlyTokens: 5}}, 7) {
		t.Error("needsUsage: a model limit needs the usage, another team's limit alone does not")
	}
}
