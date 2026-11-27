package quota

import (
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The budget is the smallest room left under every limit that applies; an
// admin's start keeps only the team's own limit on its team entry.
func TestBudgetFrom(t *testing.T) {
	g := store.EntryRef{Source: store.SourceGlobal, ID: "g"}
	tm := store.EntryRef{Source: store.SourceTeam, ID: "t"}
	use := func(rows ...store.EntryTokens) EntryUsage { return usageOf(rows) }
	for _, c := range []struct {
		name    string
		u       Usage
		limits  []store.ModelLimit
		eu      EntryUsage
		ref     store.EntryRef
		team    int64
		admin   bool
		want    int64
		limited bool
	}{
		{name: "no limits", ref: g},
		{name: "repository quota", u: Usage{Repository: 40, RepositoryLimit: i64(100)}, ref: g, want: 60, limited: true},
		{
			name: "smallest of quota and model limit",
			u:    Usage{Repository: 40, RepositoryLimit: i64(100)}, ref: g,
			limits: []store.ModelLimit{{Ref: g, MonthlyTokens: 1000}}, eu: use(store.EntryTokens{Ref: g, Tokens: 900}),
			want: 60, limited: true,
		},
		{
			name: "model limit is the smallest",
			u:    Usage{Repository: 10, RepositoryLimit: i64(100)}, ref: g,
			limits: []store.ModelLimit{{Ref: g, MonthlyTokens: 1000}}, eu: use(store.EntryTokens{Ref: g, Tokens: 990}),
			want: 10, limited: true,
		},
		{name: "instance quota counts for a global entry", u: Usage{Instance: 70, InstanceLimit: i64(100)}, ref: g, want: 30, limited: true},
		{name: "instance quota does not count for a team entry", u: Usage{Instance: 70, InstanceLimit: i64(100)}, ref: tm},
		{
			name:   "owner team limit counts",
			limits: []store.ModelLimit{{Ref: g, TeamID: 7, MonthlyTokens: 500}}, eu: use(store.EntryTokens{Ref: g, OwnerTeam: 7, Tokens: 200}),
			ref: g, team: 7, want: 300, limited: true,
		},
		{
			name:   "another team's limit is ignored",
			limits: []store.ModelLimit{{Ref: g, TeamID: 8, MonthlyTokens: 500}}, eu: use(store.EntryTokens{Ref: g, OwnerTeam: 8, Tokens: 200}),
			ref: g, team: 7,
		},
		{
			name:   "no owner team ignores team limits",
			limits: []store.ModelLimit{{Ref: g, TeamID: 8, MonthlyTokens: 500}},
			ref:    g,
		},
		{
			name:   "limit on another entry is ignored",
			limits: []store.ModelLimit{{Ref: tm, MonthlyTokens: 5}}, ref: g,
		},
		{
			name:   "admin drops quotas, model limit and team limit on a global entry",
			u:      Usage{Repository: 99, RepositoryLimit: i64(100), Instance: 99, InstanceLimit: i64(100)},
			limits: []store.ModelLimit{{Ref: g, MonthlyTokens: 10}, {Ref: g, TeamID: 7, MonthlyTokens: 10}},
			ref:    g, team: 7, admin: true,
		},
		{
			name:   "admin keeps the team's own limit on its team entry",
			u:      Usage{Repository: 99, RepositoryLimit: i64(100)},
			limits: []store.ModelLimit{{Ref: tm, TeamID: 7, MonthlyTokens: 500}}, eu: use(store.EntryTokens{Ref: tm, OwnerTeam: 7, Tokens: 450}),
			ref: tm, team: 7, admin: true, want: 50, limited: true,
		},
		{name: "used up gives zero", u: Usage{Repository: 100, RepositoryLimit: i64(100)}, ref: g, want: 0, limited: true},
		{name: "over the quota gives a negative", u: Usage{Repository: 130, RepositoryLimit: i64(100)}, ref: g, want: -30, limited: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			left, limited := budgetFrom(c.u, c.limits, c.eu, c.ref, c.team, c.admin)
			if left != c.want || limited != c.limited {
				t.Errorf("budgetFrom = %d, %v; want %d, %v", left, limited, c.want, c.limited)
			}
		})
	}
}

// ScanBudget reads the repository's use, the limits and the entries' use,
// then takes the smallest room: 1000 - 960 = 40 against 100 - 30 = 70. An
// admin's start goes past both.
func TestScanBudgetReads(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	g := store.EntryRef{Source: store.SourceGlobal, ID: "g"}
	entry := &store.RunEntry{Source: g.Source, ID: g.ID, Name: "g", Position: 1, Length: 1}
	seedUsageOn(t, st, "main", 1, 1, now, 30, 0, entry)
	seedUsageOn(t, st, "main", 2, 1, now, 930, 0, entry) // another repository, same entry
	if err := st.SetModelLimit(ctx, store.ModelLimit{Ref: g, MonthlyTokens: 1000, UpdatedAt: now, UpdatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	repo := settings.Effective{Instance: "main", Project: "acme/p1", MonthlyTokenQuota: i64(100)}
	left, limited, err := ScanBudget(ctx, st, now, repo, 1, g, 0, false)
	if err != nil || !limited || left != 40 {
		t.Fatalf("ScanBudget = %d, %v, %v; want 40, true, nil", left, limited, err)
	}
	left, limited, err = ScanBudget(ctx, st, now, repo, 1, g, 0, true)
	if err != nil || limited {
		t.Fatalf("admin: ScanBudget = %d, %v, %v; want not limited", left, limited, err)
	}
}
