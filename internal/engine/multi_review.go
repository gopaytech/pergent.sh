package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

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

// reasonAPINoMultiReview fails a member of an API review: an API review is
// never split, so such a member is left over (a legacy panel's, or a bug).
const reasonAPINoMultiReview = "an API review never runs multi review"

// detailRepoQuota is the admin-only detail of an extra reviewer that starts
// no attempt because the repository's token quota is used up.
const detailRepoQuota = "the repository's token quota is used up"

// isMember reports whether the dispatch starts a reviewer's Job.
func (d *dispatcher) isMember() bool { return d.stage == stageMember }

// dispatchMember starts the Job of the claimed reviewer m, with the same
// dispatch as a review, in stageMember: its run is the one waiting for m's
// review. A member whose run no longer waits goes back to the queue, where
// nothing claims it, and the sweep supersedes it. A member of an API review
// fails: an API review never runs multi review.
func (e *Engine) dispatchMember(ctx context.Context, m store.ReviewMember) error {
	d := &dispatcher{e: e, stage: stageMember, member: m, run: store.Run{ID: m.RunID},
		log: e.log().With("run", m.RunID, "member", m.ID, "reviewer", m.Reviewer)}
	run, found, err := e.Store.MultiReviewRun(ctx, m.RunID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load the multi review's run: %w", err))
	}
	if !found {
		d.log.Info("the multi review's run no longer waits for its reviewers; the member is left to the sweep")
		return d.postpone(ctx, e.now())
	}
	d.run = run
	d.log = e.log().With("run", run.ID, "member", m.ID, "reviewer", m.Reviewer, "instance", run.MR.Instance,
		"project", run.ProjectPath, "mr", run.MR.IID, "head", run.HeadSHA)
	if run.Kind == store.KindAPI {
		return d.fail(ctx, reasonAPINoMultiReview)
	}
	return d.dispatch(ctx)
}

// dispatchLegacyMembers handles up to limit queued members of a review panel
// that a main build left (panel_members). None gets a Job: the member
// result route takes review_members only, so its result would be refused. A
// member of an API review fails, and its run is left as it is. A member of a
// merge request's review makes the engine queue that review again as one
// review (RecoverLegacyPanels, as at start). The returned error joins store
// failures.
func (e *Engine) dispatchLegacyMembers(ctx context.Context, limit int, skip []string) error {
	ms, err := e.Store.ClaimQueuedMembers(ctx, limit, e.now(), skip...)
	if err != nil {
		return fmt.Errorf("dispatch: claim queued legacy panel members: %w", err)
	}
	var errs []error
	recoverPanels := false
	for _, m := range ms {
		log := e.log().With("run", m.RunID, "member", m.ID, "role", m.Role)
		run, found, err := e.Store.PanelRun(ctx, m.RunID)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("dispatch legacy panel member %s: load its run: %w", m.ID, err))
			if err := e.Store.PostponeMember(ctx, m.ID, e.now().Add(waitDelay)); err != nil {
				errs = append(errs, fmt.Errorf("postpone legacy panel member %s: %w", m.ID, err))
			}
		case !found:
			// The legacy run took the run meanwhile; the member waits for
			// the recovery at the next start.
			if err := e.Store.PostponeMember(ctx, m.ID, e.now().Add(waitDelay)); err != nil {
				errs = append(errs, fmt.Errorf("postpone legacy panel member %s: %w", m.ID, err))
			}
		case run.Kind == store.KindAPI:
			log.Warn("a legacy panel member of an API review can't start; reporting it as failed", "reason", reasonAPINoMultiReview)
			if _, err := e.Store.ReportMemberFailure(ctx, m.ID, store.Failure{Reason: reasonAPINoMultiReview}, e.now()); err != nil {
				errs = append(errs, fmt.Errorf("report legacy panel member %s: %w", m.ID, err))
			}
		default:
			recoverPanels = true
		}
	}
	if recoverPanels {
		e.RecoverLegacyPanels(ctx)
	}
	return errors.Join(errs...)
}

// resolveMember picks member m's entry from its frozen chain (FromFrozen
// with the registry now): its reviewer's earlier attempts are tried, and
// reviewer k > 1 avoids modelsBefore(k). Reviewer 1 avoids none and is
// never held by a quota (the run passed the hold). Reviewer k > 1 starts no
// attempt while the repository quota is used up (spec "Quota").
//
// The base is read now (chain.Load): the entries, marks and limits of this
// dispatch. Reviewer 1 resolves as chain A did for its run: the run's
// earlier attempts are tried too, and a hold goes past the instance quota
// and the admin limits (pastAdmin); a team's own limit still fails it.
// Reviewer k > 1 never goes past a limit: a hold does not run.
func (d *dispatcher) resolveMember(ctx context.Context, repo settings.Effective, repoUsedUp, instanceUsedUp bool) (chain.Decision, error) {
	ms, err := d.e.Store.ReviewMembers(ctx, d.member.RunID)
	if err != nil {
		return chain.Decision{}, fmt.Errorf("read the multi review's members: %w", err)
	}
	base, err := d.e.chainFor(ctx, d.run, repo)
	if err != nil {
		return chain.Decision{}, err
	}
	d.members, d.base = ms, base
	k := d.member.Reviewer
	in := d.memberInput(ms, d.member)
	d.attempt = len(in.Tried) + 1
	d.scope = in.Scope
	if k > 1 && repoUsedUp {
		// No entry runs: memberCannotRun fails it for the quota.
		d.entry = chain.Decision{Length: len(in.Refs), Exhausted: true}
		return d.entry, nil
	}
	if k > 1 {
		if in.Avoid, err = d.modelsBefore(ctx, ms, k, instanceUsedUp); err != nil {
			return chain.Decision{}, err
		}
	}
	in.InstanceQuotaUsedUp = instanceUsedUp
	dec := chain.Resolve(in)
	if k == 1 && dec.Hold {
		dec = pastAdmin(in)
	}
	d.entry = dec
	return dec, nil
}

// memberInput is the chain input of member m (ms are its review's members):
// its frozen chain read with d.base, and the entries the other attempts of
// its reviewer ran tried. Reviewer 1's also has the run's earlier attempts
// tried, as chain A has for the run. Avoid and the instance quota are left
// to the caller.
func (d *dispatcher) memberInput(ms []store.ReviewMember, m store.ReviewMember) chain.Input {
	in := chain.FromFrozen(m.Chain, d.base)
	var tried map[store.EntryRef]bool
	if m.Reviewer == 1 {
		tried = maps.Clone(d.base.Tried)
	}
	for ref := range triedBy(ms, m) {
		if tried == nil {
			tried = map[store.EntryRef]bool{}
		}
		tried[ref] = true
	}
	in.Tried = tried
	return in
}

// modelsBefore are the models reviewers 1 to k-1 run on now: sameModels
// (M3.4) of each one's newest attempt with an entry, else of what its
// frozen chain resolves to now. A reviewer with no entry yet is resolved
// only while its newest attempt is live (queued or running): a skipped or
// failed one runs on nothing. The resolve is the one its own dispatch does
// (resolveMember) with the repository quota not used up: the members of a
// review are claimed in reviewer order, so an earlier one usually has its
// entry already.
func (d *dispatcher) modelsBefore(ctx context.Context, ms []store.ReviewMember, k int, instanceUsedUp bool) ([]chain.SameModel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []chain.SameModel
	for j := 1; j < k; j++ {
		newest, withEntry, ok := attemptsOf(ms, j)
		if !ok {
			continue
		}
		if withEntry != nil {
			out = append(out, chain.SameModel{Provider: withEntry.Provider, Model: withEntry.Model})
			continue
		}
		if newest.State != store.MemberQueued && newest.State != store.MemberRunning {
			continue
		}
		in := d.memberInput(ms, newest)
		if j > 1 {
			in.Avoid = out
		}
		in.InstanceQuotaUsedUp = instanceUsedUp
		dec := chain.Resolve(in)
		if j == 1 && dec.Hold {
			dec = pastAdmin(in)
		}
		out = append(out, sameModels([]chain.Decision{dec})...)
	}
	return out, nil
}

// attemptsOf finds reviewer n's attempts among ms (oldest first): its newest
// attempt, and its newest attempt with an entry (nil for none). ok is false
// when reviewer n has none.
func attemptsOf(ms []store.ReviewMember, n int) (newest store.ReviewMember, withEntry *store.ReviewMember, ok bool) {
	for i := range ms {
		m := &ms[i]
		if m.Reviewer != n || m.State == store.MemberOff {
			continue
		}
		newest, ok = *m, true
		if m.Entry.ID != "" {
			withEntry = m
		}
	}
	return newest, withEntry, ok
}

// triedBy is the entries the other attempts of m's reviewer ran (ms are the
// review's members).
func triedBy(ms []store.ReviewMember, m store.ReviewMember) map[store.EntryRef]bool {
	var out map[store.EntryRef]bool
	for _, o := range ms {
		if o.Reviewer != m.Reviewer || o.ID == m.ID || o.Entry.ID == "" {
			continue
		}
		if out == nil {
			out = map[store.EntryRef]bool{}
		}
		out[store.EntryRef{Source: o.Entry.Source, ID: o.Entry.ID}] = true
	}
	return out
}

// memberCannotRun acts on a member whose chain gives it no entry (dec does
// not run). Reviewer 1 with no entry at all waits, as a run does; with none
// usable it fails as a run does (failExhausted). An extra reviewer fails
// with the fixed words of skipWords, or skipQuota while the repository
// quota is used up (repoUsedUp): the review goes on without it. Its
// chain.FailureText goes in the admin-only detail only.
func (d *dispatcher) memberCannotRun(ctx context.Context, dec chain.Decision, repoUsedUp bool) error {
	switch {
	case d.member.Reviewer == 1 && dec.Wait:
		d.e.warnEvery("no-llm", msgNoLLM)
		return d.postpone(ctx, d.e.now().Add(waitDelay))
	case d.member.Reviewer == 1:
		return d.failExhausted(ctx, dec)
	case repoUsedUp:
		return d.failMember(ctx, store.Failure{Reason: skipQuota, Detail: detailRepoQuota})
	case dec.Wait:
		return d.failMember(ctx, store.Failure{Reason: skipWords(dec)})
	}
	return d.failMember(ctx, store.Failure{Reason: skipWords(dec), Detail: chain.FailureText(dec)})
}

// failMember reports the member with the failure f, its reason and detail
// redacted. It never touches the run, which goes on waiting for its
// members.
func (d *dispatcher) failMember(ctx context.Context, f store.Failure) error {
	f.Reason, f.Detail = d.redact(f.Reason), d.redact(f.Detail)
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	d.log.Warn("the reviewer can't start; reporting it as failed", "reason", f.Reason)
	if _, err := d.e.Store.ReportReviewMemberFailure(sctx, d.member.ID, f, d.e.now()); err != nil {
		return fmt.Errorf("report the member's failure: %w", err)
	}
	return nil
}

// memberInputsMatch reports whether the fitted inputs of a reviewer, or of
// Job P, are what its run recorded when it was dispatched: the rules, the
// skills (where each came from and its content's hash), whether it is
// incremental and since which run, and the linked repositories. A reviewer
// whose skills changed stops here, before its Job, as its result would fail
// its check.
func (d *dispatcher) memberInputsMatch(in *inputs.Inputs, last store.Run, links linkPlan) bool {
	run := d.run
	if run.SkillSources != nil {
		sources, _, hashes := d.runInputs(in)
		if !maps.Equal(sources, run.SkillSources) || (run.SkillHashes != nil && !maps.Equal(hashes, run.SkillHashes)) {
			d.log.Info("the review's skills changed since its multi review started")
			return false
		}
	}
	if run.RulesHash != nil && *run.RulesHash != d.rulesHash {
		d.log.Info("the review's rules changed since its multi review started")
		return false
	}
	incremental := len(in.Carried) > 0
	if incremental != run.Incremental || (incremental && (run.BaseRunID == nil || *run.BaseRunID != last.ID)) {
		d.log.Info("the files the review carries changed since its multi review started")
		return false
	}
	if !slices.EqualFunc(links.Matched, run.LinkedRepos, sameLink) {
		d.log.Info("the review's linked repositories changed since its multi review started")
		return false
	}
	return true
}

// sameLink reports whether a and b are one link (a nil and an empty watched
// list are the same).
func sameLink(a, b store.Link) bool {
	return a.Target == b.Target && a.CanBlock == b.CanBlock && slices.Equal(a.Watched, b.Watched)
}

// requeueMember puts the member back in the queue with an attempt counted. A
// member that is no longer queued or running (superseded meanwhile) can't be,
// and that is not an error.
func (d *dispatcher) requeueMember(ctx context.Context, notBefore time.Time) error {
	err := d.e.Store.RequeueReviewMember(ctx, d.member.ID, notBefore)
	if err == nil {
		return nil
	}
	ms, lerr := d.e.Store.ReviewMembers(ctx, d.member.RunID)
	if lerr == nil {
		for _, m := range ms {
			if m.ID == d.member.ID && m.State != store.MemberQueued && m.State != store.MemberRunning {
				return nil
			}
		}
	}
	return fmt.Errorf("requeue the member: %w", err)
}

// memberRunningText is the running status the review's first member Job
// sets: the run's own, which says a backup for an attempt after the first.
func (d *dispatcher) memberRunningText(ctx context.Context) string {
	run := d.run
	if run.AttemptOf == nil {
		return runningDescription
	}
	as, err := d.e.Store.Attempts(ctx, *run.AttemptOf)
	if err != nil {
		d.log.Info("couldn't read the review's attempts for its status", "error", d.redact(err.Error()))
		return runningDescription
	}
	for i, a := range as {
		if a.ID == run.ID && i+1 <= run.ChainLength {
			return fmt.Sprintf(backupDescription, i+1, run.ChainLength)
		}
	}
	return runningDescription
}
