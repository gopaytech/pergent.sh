package quota

import (
	"context"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// EntryUsage is what each chain entry used in a month: in all (every row)
// and in the repositories of each owner team. A row with no owner team (an
// API review, a repository with none, a row from before model limits) counts
// in the total only.
type EntryUsage struct {
	total  map[store.EntryRef]int64
	byTeam map[store.EntryRef]map[int64]int64
}

// Used is what ref used in the repositories team owns, or with team 0 in all.
func (u EntryUsage) Used(ref store.EntryRef, team int64) int64 {
	if team == 0 {
		return u.total[ref]
	}
	return u.byTeam[ref][team]
}

// UsedUp reports whether l is reached: usage at or above it, as for the
// quotas.
func (u EntryUsage) UsedUp(l store.ModelLimit) bool {
	return u.Used(l.Ref, l.TeamID) >= l.MonthlyTokens
}

func usageOf(rows []store.EntryTokens) EntryUsage {
	u := EntryUsage{total: map[store.EntryRef]int64{}, byTeam: map[store.EntryRef]map[int64]int64{}}
	for _, r := range rows {
		u.total[r.Ref] += r.Tokens
		if r.OwnerTeam == 0 {
			continue
		}
		if u.byTeam[r.Ref] == nil {
			u.byTeam[r.Ref] = map[int64]int64{}
		}
		u.byTeam[r.Ref][r.OwnerTeam] += r.Tokens
	}
	return u
}

// ReadEntryUsage reads what each entry used in the month of now.
func ReadEntryUsage(ctx context.Context, st *store.Store, now time.Time) (EntryUsage, error) {
	rows, err := st.EntryTokensSince(ctx, MonthStart(now))
	if err != nil {
		return EntryUsage{}, err
	}
	return usageOf(rows), nil
}

// Limits is which entries a limit stops this month for one owner team's
// repositories: Model, the entries whose model limit is used up; Team, those
// whose limit for the team is used up.
type Limits struct {
	Model map[store.EntryRef]bool
	Team  map[store.EntryRef]bool
}

// ReadLimits reads which limits are used up in the month of now for the
// repositories ownerTeam owns. With ownerTeam 0 (no owner team, an API
// review) no team limit applies. It reads the usage only when a limit
// applies, so with none it makes one query.
func ReadLimits(ctx context.Context, st *store.Store, now time.Time, ownerTeam int64) (Limits, error) {
	limits, err := st.ModelLimits(ctx)
	if err != nil || !needsUsage(limits, ownerTeam) {
		return Limits{}, err
	}
	u, err := ReadEntryUsage(ctx, st, now)
	if err != nil {
		return Limits{}, err
	}
	return limitsFrom(limits, u, ownerTeam), nil
}

// needsUsage reports whether one of limits applies to ownerTeam's
// repositories: a model limit, or the team's own.
func needsUsage(limits []store.ModelLimit, ownerTeam int64) bool {
	for _, l := range limits {
		if l.TeamID == 0 || l.TeamID == ownerTeam {
			return true
		}
	}
	return false
}

func limitsFrom(limits []store.ModelLimit, u EntryUsage, ownerTeam int64) Limits {
	var out Limits
	add := func(m map[store.EntryRef]bool, ref store.EntryRef) map[store.EntryRef]bool {
		if m == nil {
			m = map[store.EntryRef]bool{}
		}
		m[ref] = true
		return m
	}
	for _, l := range limits {
		switch {
		case !u.UsedUp(l):
		case l.TeamID == 0:
			out.Model = add(out.Model, l.Ref)
		case l.TeamID == ownerTeam:
			out.Team = add(out.Team, l.Ref)
		}
	}
	return out
}
