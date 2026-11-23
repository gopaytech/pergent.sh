package engine

import (
	"context"
	"fmt"

	"github.com/gopaytech/pergent.sh/internal/quota"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// What people read on a change when a monthly token quota is used up. None of
// it holds a number, a limit or the name of a repository or instance: on a
// public repository anyone reads it.
const (
	// quotaDescription is the pending status on the head of a held change.
	quotaDescription = "Monthly token quota used up. An admin can start the review in the dashboard."
	// quotaNote is the one note a change gets in a month, and the reply of a
	// command that was refused while the note wasn't posted yet.
	quotaNote = "Not reviewed: the monthly token quota is used up. Reviews start again with the first push after the 1st, or an admin can start this one from the pergent.sh dashboard."
	// whyQuota is the outcome of a command refused for the quota, as the
	// commands table says it ("refused: " + why).
	whyQuota = "the monthly token quota is used up"
)

// holdForQuota makes the run wait because the monthly token quotas stop it,
// or with limited, the quotas and the limits with a model or team limit among
// them: no Job, the pending status quotaDescription on its head and, the
// first time in the month on this change, quotaNote. The texts are the same
// either way, since anyone reads them on a public repository; only the wait
// reason (store.WaitLimit) tells the dashboard that a limit took part. A push
// or a request after the quota has room starts the review. A status or note
// that can't be posted is logged: the run waits either way.
func (d *dispatcher) holdForQuota(ctx context.Context, plat Platform, limited bool) error {
	e, run := d.e, d.run
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	hold, reason := e.Store.HoldForQuota, store.WaitQuota
	if limited {
		hold, reason = e.Store.HoldForLimit, store.WaitLimit
	}
	held, err := hold(sctx, run.ID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("hold the run: %w", err))
	}
	if !held {
		// Superseded meanwhile, or asked for while it was being checked: back
		// to the queue, which a superseded run doesn't go to.
		return d.postpone(ctx, e.now())
	}
	d.log.Info("run waits: quotas or limits stop every entry", "reason", reason)
	now := e.now()
	d.announceHold(sctx, plat, quotaDescription, &holdNote{
		what: "a used-up token quota",
		text: quotaNote,
		claim: func(c context.Context) (bool, error) {
			return e.Store.ClaimQuotaNote(c, run.MR, quota.MonthStart(now), now)
		},
		release: func(c context.Context) error { return e.Store.ReleaseQuotaNote(c, run.MR) },
	})
	return nil
}
