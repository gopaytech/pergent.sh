package engine

import (
	"context"
	"fmt"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Why a multi review runs as one review (after "multi review off: ").
const (
	offImage = "the review image does not support it"
	// offFetch: the judge fetches the reviewers' results with pergent.sh's
	// own image (PERGENT_SH_IMAGE, set by the chart).
	offFetch = "pergent.sh's own image is not set"
	offQuota = "the token quota is used up"
	offNone  = "no other reviewer can start"
	offRead  = "the reviewers could not be read"
)

// Why an extra reviewer can't start (its skipped row's reason).
const (
	skipSame  = "no different model left"
	skipQuota = "the token quota"
	skipNone  = "no usable entry"
)

// split decides whether the run is a multi review, after every check a
// review passes and the run's writes, before its token. done is true when
// the dispatch ends here: StartMultiReview split it (its members get their
// Jobs apart), or the run was gone. With no extra reviewer nothing is
// written and done is false: the review is today's. When no extra reviewer
// can start, the reason goes in the review's one 'off' row and done is
// false: the review runs on chain A alone, as today. repoUsedUp and
// instanceUsedUp are the token quotas (quota.Check); a run under a used-up
// repository quota is here only because somebody asked for it, and no extra
// reviewer starts then. An extra reviewer never goes past a quota or a
// limit, not even on an admin's start: those let the review start, not more
// models. err is a store error; the caller retries.
func (d *dispatcher) split(ctx context.Context, mr forge.Change, fitted *inputs.Inputs, repo settings.Effective, repoUsedUp, instanceUsedUp bool) (done bool, err error) {
	e := d.e
	if !d.run.DispatchedAt.IsZero() {
		// Split before, and released: it is never split again, and its off
		// row (none) is not written.
		return false, nil
	}
	rs, err := chain.LoadReviewers(ctx, e.Store, e.registry(), e.now(), d.run, repo)
	if err != nil {
		// Never the review's failure, and never a silent change to it.
		d.log.Error("the review runs as one review: the reviewers could not be read", "error", d.redact(err.Error()))
		return false, d.recordOff(ctx, offRead)
	}
	if len(rs.Extra) == 0 {
		return false, nil
	}
	if ok, why := config.PanelImage(e.Config.Jobs.Image); !ok {
		d.log.Info("the review runs as one review: the review image does not support multi review", "image_check", why)
		return false, d.recordOff(ctx, offImage)
	}
	if e.ScanImage == "" {
		d.log.Info("the review runs as one review: pergent.sh's own image (PERGENT_SH_IMAGE) is not set")
		return false, d.recordOff(ctx, offFetch)
	}
	if repoUsedUp {
		return false, d.recordOff(ctx, offQuota)
	}

	reviewers := []store.SplitReviewer{{Number: 1, Chain: chain.Freeze(rs.One)}}
	running := []chain.Decision{d.entry}
	var skips []string
	for i, extra := range rs.Extra {
		fc := chain.Freeze(extra)
		in := chain.FromFrozen(fc, rs.One)
		in.Avoid = sameModels(running)
		in.InstanceQuotaUsedUp = instanceUsedUp
		r := store.SplitReviewer{Number: i + 2, Chain: fc}
		if dec := chain.Resolve(in); dec.Runs() {
			running = append(running, dec)
		} else {
			r.Skipped = skipWords(dec)
			skips = append(skips, r.Skipped)
		}
		reviewers = append(reviewers, r)
	}
	if len(running) == 1 {
		return false, d.recordOff(ctx, offReason(skips))
	}

	// The run keeps the merge request's base, which every reviewer reviews,
	// and its skills as a review records them, which their results are
	// checked against.
	sources, versions, hashes := d.runInputs(fitted)
	started, err := e.Store.StartMultiReview(ctx, d.run.ID, store.MultiReviewSplit{
		BaseSHA: mr.BaseSHA, StartSHA: mr.StartSHA,
		Inputs: store.RunInputs{Model: d.model, MaxTurns: repo.MaxTurns,
			Sources: sources, Versions: versions, Hashes: hashes},
		Reviewers: reviewers,
		Judge:     chain.Freeze(rs.Judge),
	}, e.now())
	if err != nil {
		return false, err
	}
	if !started {
		// The run is no longer claimed: superseded, or split by another
		// dispatch, meanwhile.
		d.log.Info("the multi review did not start: the run is no longer claimed")
		return true, nil
	}
	d.log.Info("multi review started", "reviewers", len(reviewers), "starting", len(running))
	e.Wake()
	return true, nil
}

// recordOff records why the review runs as one review.
func (d *dispatcher) recordOff(ctx context.Context, reason string) error {
	d.log.Info("the review runs as one review", "reason", reason)
	if err := d.e.Store.RecordMultiReviewOff(ctx, d.run.ID, reason); err != nil {
		return fmt.Errorf("record why the review is not a multi review: %w", err)
	}
	return nil
}

// offReason is why no extra reviewer can start, from the reasons each one
// was skipped for: the quota when the quota stopped every one of them, else
// no other reviewer can start.
func offReason(skips []string) string {
	for _, s := range skips {
		if s != skipQuota {
			return offNone
		}
	}
	if len(skips) == 0 {
		return offNone
	}
	return offQuota
}

// skipWords is why an extra reviewer can't start (dec does not run), in the
// fixed words: skipQuota when only quotas, limits and models in use stopped
// its entries, and at least one quota or limit did; skipSame when only
// models in use did; else skipNone. An entry an earlier attempt tried
// neither stops nor names the reason. The split and memberCannotRun both
// use it.
func skipWords(dec chain.Decision) string {
	quota, same := false, false
	for _, s := range dec.Skips {
		switch s.Reason {
		case chain.ReasonInstanceQuota, chain.ReasonModelLimit, chain.ReasonTeamLimit:
			quota = true
		case chain.ReasonSameModel:
			same = true
		case chain.ReasonTried:
		default:
			return skipNone
		}
	}
	switch {
	case quota:
		return skipQuota
	case same:
		return skipSame
	}
	return skipNone
}

// sameModels are the models of the decisions in ds that run, in order: the
// models a later reviewer avoids (chain.Input.Avoid). The split and
// modelsBefore both use it.
func sameModels(ds []chain.Decision) []chain.SameModel {
	var out []chain.SameModel
	for _, dec := range ds {
		if dec.Runs() {
			out = append(out, chain.SameModel{Provider: dec.Entry.LLM.Provider, Model: dec.Entry.LLM.Model})
		}
	}
	return out
}
