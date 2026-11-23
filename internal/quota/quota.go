// Package quota answers whether a repository's or an instance's monthly token
// quota is used up. The month is the calendar month in UTC, and what counts
// is the tokens (in plus out) the reviews reported in it: every token for a
// repository, only those spent on global keys for an instance (a team pays
// for its own). For model limits it also answers which entries a model limit
// or a team limit stops (limits.go).
package quota

import (
	"context"
	"time"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// MonthStart returns the first instant of t's month in UTC.
func MonthStart(t time.Time) time.Time {
	y, m, _ := t.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// Usage is what a repository and its instance used this month against their
// limits. A nil limit means none.
type Usage struct {
	Repository, Instance           int64
	RepositoryLimit, InstanceLimit *int64
}

// UsedUp reports whether either limit is reached.
func (u Usage) UsedUp() bool {
	return (u.RepositoryLimit != nil && u.Repository >= *u.RepositoryLimit) ||
		(u.InstanceLimit != nil && u.Instance >= *u.InstanceLimit)
}

// Read returns both sums for the month of now, whatever the limits, with the
// limits copied from repo. A projectID of 0 (unknown project) gives a
// Repository of 0 without a query.
func Read(ctx context.Context, st *store.Store, now time.Time, instance string, projectID int64, repo settings.Effective) (Usage, error) {
	since := MonthStart(now)
	u := Usage{RepositoryLimit: repo.MonthlyTokenQuota, InstanceLimit: repo.InstanceMonthlyTokenQuota}
	var err error
	if projectID != 0 {
		if u.Repository, err = st.TokensSince(ctx, instance, projectID, since); err != nil {
			return Usage{}, err
		}
	}
	if u.Instance, err = st.InstanceGlobalTokensSince(ctx, instance, since); err != nil {
		return Usage{}, err
	}
	return u, nil
}

// Check reports whether repo's quota (repoUsedUp) or its instance's
// (instanceUsedUp) is used up this month. A used-up repository holds the
// whole run; a used-up instance only stops entries on global keys. The
// repository is read first, and when it is used up the instance is not read
// (false). It asks only for the sums that have a limit, so with none it makes
// no query.
func Check(ctx context.Context, st *store.Store, now time.Time, instance string, projectID int64, repo settings.Effective) (repoUsedUp, instanceUsedUp bool, err error) {
	since := MonthStart(now)
	if repo.MonthlyTokenQuota != nil && projectID != 0 {
		n, err := st.TokensSince(ctx, instance, projectID, since)
		if err != nil {
			return false, false, err
		}
		if n >= *repo.MonthlyTokenQuota {
			return true, false, nil
		}
	}
	if repo.InstanceMonthlyTokenQuota != nil {
		n, err := st.InstanceGlobalTokensSince(ctx, instance, since)
		if err != nil {
			return false, false, err
		}
		if n >= *repo.InstanceMonthlyTokenQuota {
			return false, true, nil
		}
	}
	return false, false, nil
}
