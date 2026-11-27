package quota

import (
	"context"
	"time"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// ScanBudget is how many tokens a scan that starts now on ref may still use:
// the smallest room left under the quotas and limits that apply (budgetFrom).
// limited is false when none applies. It is passed to the scan Job as
// PERGENT_TOKEN_BUDGET. pastAdmin is an admin's start, which goes past the
// quotas and the admin limits, as it does for a review.
func ScanBudget(ctx context.Context, st *store.Store, now time.Time, repo settings.Effective, projectID int64, ref store.EntryRef, ownerTeam int64, pastAdmin bool) (left int64, limited bool, err error) {
	u, err := Read(ctx, st, now, repo.Instance, projectID, repo)
	if err != nil {
		return 0, false, err
	}
	limits, err := st.ModelLimits(ctx)
	if err != nil {
		return 0, false, err
	}
	eu, err := ReadEntryUsage(ctx, st, now)
	if err != nil {
		return 0, false, err
	}
	left, limited = budgetFrom(u, limits, eu, ref, ownerTeam, pastAdmin)
	return left, limited, nil
}

// budgetFrom is the smallest of: the repository quota, the instance quota
// (only for a global entry: a team pays for its own), the model limit on ref,
// the owner team's limit on ref, each minus its use. A used-up one gives 0 or
// less. With pastAdmin only a team's own limit on its team entry counts, as
// that is the team's money. limited is false when none applies.
func budgetFrom(u Usage, limits []store.ModelLimit, eu EntryUsage, ref store.EntryRef, ownerTeam int64, pastAdmin bool) (left int64, limited bool) {
	take := func(limit, used int64) {
		if room := limit - used; !limited || room < left {
			left, limited = room, true
		}
	}
	if !pastAdmin {
		if u.RepositoryLimit != nil {
			take(*u.RepositoryLimit, u.Repository)
		}
		if u.InstanceLimit != nil && ref.Source == store.SourceGlobal {
			take(*u.InstanceLimit, u.Instance)
		}
	}
	for _, l := range limits {
		if l.Ref != ref {
			continue
		}
		switch {
		case l.TeamID == 0:
			if !pastAdmin {
				take(l.MonthlyTokens, eu.Used(ref, 0))
			}
		case l.TeamID == ownerTeam:
			if !pastAdmin || ref.Source == store.SourceTeam {
				take(l.MonthlyTokens, eu.Used(ref, ownerTeam))
			}
		}
	}
	return left, limited
}
