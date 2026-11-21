package engine

import (
	"context"
	"fmt"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Why a panel review runs as one review (the 'off' row's reason, after
// "panel off: " in the footer).
const (
	panelOffImage = "the review image does not support it"
	panelOffQuota = "the token quota stopped model B"
	panelOffSame  = "model B is the model A runs on"
	panelOffNone  = "no entry of chain B can be used"
	panelOffRead  = "chain B could not be read"
)

// panel decides whether the run is a panel review, after every check a
// review passes and the run's writes, before its token. split is true when
// the dispatch ends here: StartPanel made the run a panel (its members get
// their Jobs apart), or the run was gone. With no chain B in effect nothing
// is written and split is false: the review is today's. When B can't start,
// the reason goes in the review's one 'off' row and split is false: the
// review runs A only, as today. repoUsedUp and instanceUsedUp are the token
// quotas (quota.Check); a run under a used-up repository quota is here only
// because somebody asked for it, and B does not start then. err is a store
// error; the caller retries.
func (d *dispatcher) panel(ctx context.Context, repo settings.Effective, repoUsedUp, instanceUsedUp bool) (split bool, err error) {
	e := d.e
	in, err := chain.LoadSet(ctx, e.Store, e.registry(), e.now(), d.run, repo, store.ChainB)
	if err != nil {
		// Never the review's failure, and never a silent change to it.
		d.log.Error("the review runs without a panel: chain B could not be read", "error", d.redact(err.Error()))
		return false, d.panelOff(ctx, panelOffRead)
	}
	if len(in.Refs) == 0 {
		return false, nil
	}
	if ok, why := config.PanelImage(e.Config.Jobs.Image); !ok {
		d.log.Info("the review runs without a panel: the review image does not support it", "image_check", why)
		return false, d.panelOff(ctx, panelOffImage)
	}
	if repoUsedUp {
		return false, d.panelOff(ctx, panelOffQuota)
	}
	if dec := d.resolveB(in, instanceUsedUp); !dec.Runs() {
		return false, d.panelOff(ctx, panelOffReason(dec))
	}
	started, err := e.Store.StartPanel(ctx, d.run.ID, e.now())
	if err != nil {
		return false, err
	}
	if !started {
		d.log.Info("run was superseded while its panel started")
		return true, nil
	}
	d.log.Info("review panel started", "chain_b", in.Scope)
	e.Wake()
	return true, nil
}

// panelOff records why the review runs A only.
func (d *dispatcher) panelOff(ctx context.Context, reason string) error {
	d.log.Info("the review runs without a panel", "reason", reason)
	if err := d.e.Store.RecordPanelOff(ctx, d.run.ID, reason); err != nil {
		return fmt.Errorf("record why the review has no panel: %w", err)
	}
	return nil
}

// panelOffReason is why B can't run on any entry of chain B (dec does not
// run). The quota is the reason when only it and A's model stopped the
// entries, A's model when only it did; anything else is no usable entry.
func panelOffReason(dec chain.Decision) string {
	quota, same := false, false
	for _, s := range dec.Skips {
		switch s.Reason {
		case chain.ReasonInstanceQuota:
			quota = true
		case chain.ReasonSameModel:
			same = true
		default:
			return panelOffNone
		}
	}
	switch {
	case quota:
		return panelOffQuota
	case same:
		return panelOffSame
	}
	return panelOffNone
}
