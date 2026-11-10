// Package quota answers whether a repository's or an instance's monthly token
// quota is used up. The month is the calendar month in UTC, and what counts
// is the tokens (in plus out) the reviews reported in it.
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
	if u.Instance, err = st.TokensSince(ctx, instance, 0, since); err != nil {
		return Usage{}, err
	}
	return u, nil
}

// Check reports whether a quota of repo is used up this month. It asks only
// for the sums that have a limit, so with none it makes no query.
func Check(ctx context.Context, st *store.Store, now time.Time, instance string, projectID int64, repo settings.Effective) (usedUp bool, err error) {
	since := MonthStart(now)
	if repo.MonthlyTokenQuota != nil && projectID != 0 {
		n, err := st.TokensSince(ctx, instance, projectID, since)
		if err != nil {
			return false, err
		}
		if n >= *repo.MonthlyTokenQuota {
			return true, nil
		}
	}
	if repo.InstanceMonthlyTokenQuota != nil {
		n, err := st.TokensSince(ctx, instance, 0, since)
		if err != nil {
			return false, err
		}
		if n >= *repo.InstanceMonthlyTokenQuota {
			return true, nil
		}
	}
	return false, nil
}
