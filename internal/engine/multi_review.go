package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/gopaytech/pergent.sh/internal/callback"
	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/quota"
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
// never split, so such a member is left over (a bug).
const reasonAPINoMultiReview = "an API review never runs multi review"

// reasonInputsChanged fails a member whose dispatch would change what its run
// was dispatched with: a member never writes its run.
const reasonInputsChanged = "the review's inputs changed while the multi review ran"

// errInputsChanged is how subset says it would write to a member's run.
var errInputsChanged = errors.New(reasonInputsChanged)

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

// resolveMember picks member m's entry from its frozen chain
// (reviewerDecision) on the base read now (chain.Load): the entries, marks
// and limits of this dispatch.
func (d *dispatcher) resolveMember(ctx context.Context, repo settings.Effective, repoUsedUp, instanceUsedUp bool) (chain.Decision, error) {
	ms, err := d.e.Store.ReviewMembers(ctx, d.member.RunID)
	if err != nil {
		return chain.Decision{}, fmt.Errorf("read the multi review's members: %w", err)
	}
	base, err := d.e.chainFor(ctx, d.run, repo)
	if err != nil {
		return chain.Decision{}, err
	}
	dec, in, err := reviewerDecision(base, ms, d.member, repoUsedUp, instanceUsedUp)
	if err != nil {
		return chain.Decision{}, err
	}
	d.attempt = len(in.Tried) + 1
	d.scope = in.Scope
	d.entry = dec
	return dec, nil
}

// errNoBase is the error of a chain helper given a base with no entries
// read: a zero chain.Input, never a chain.Load result.
var errNoBase = errors.New("the model entries were not read")

// reviewerDecision resolves member m (ms are its review's members) on base,
// a chain.Load result: its frozen chain with the entries its reviewer's
// other attempts ran tried (memberInput). in is the input it resolved. One
// rule for a member's dispatch and for the settle's "no entry left" check
// (Rulings L5 and L6).
//
// Reviewer 1 avoids no model and resolves as chain A did for its run: a
// hold goes past the instance quota and the admin limits (pastAdmin), and
// a team's own limit still stops it (Exhausted). Reviewer k > 1 avoids
// modelsBefore(k), never goes past a quota or a limit (a hold does not
// run), and has no entry while the repository quota is used up
// (repoUsedUp; spec "Quota").
func reviewerDecision(base chain.Input, ms []store.ReviewMember, m store.ReviewMember, repoUsedUp, instanceUsedUp bool) (chain.Decision, chain.Input, error) {
	in, err := memberInput(base, ms, m)
	if err != nil {
		return chain.Decision{}, chain.Input{}, err
	}
	k := m.Reviewer
	if k > 1 && repoUsedUp {
		// No entry runs: memberCannotRun fails it for the quota.
		return chain.Decision{Length: len(in.Refs), Exhausted: true}, in, nil
	}
	if k > 1 {
		if in.Avoid, err = modelsBefore(base, ms, k, instanceUsedUp); err != nil {
			return chain.Decision{}, chain.Input{}, err
		}
	}
	in.InstanceQuotaUsedUp = instanceUsedUp
	dec := chain.Resolve(in)
	if k == 1 && dec.Hold {
		dec = pastAdmin(in)
	}
	return dec, in, nil
}

// memberInput is the chain input of member m (ms are its review's members):
// its frozen chain read with base, and the entries the other attempts of
// its reviewer ran tried. Reviewer 1's also has the run's earlier attempts
// tried, as chain A has for the run. Avoid and the instance quota are left
// to the caller. A base with no entries read is errNoBase.
func memberInput(base chain.Input, ms []store.ReviewMember, m store.ReviewMember) (chain.Input, error) {
	if base.Entries == nil {
		return chain.Input{}, errNoBase
	}
	in := chain.FromFrozen(m.Chain, base)
	var tried map[store.EntryRef]bool
	if m.Reviewer == 1 {
		tried = maps.Clone(base.Tried)
	}
	for ref := range triedBy(ms, m) {
		if tried == nil {
			tried = map[store.EntryRef]bool{}
		}
		tried[ref] = true
	}
	in.Tried = tried
	return in, nil
}

// modelsBefore are the models reviewers 1 to k-1 run on now: sameModels
// (M3.4) of each one's newest attempt with an entry, else of what its
// frozen chain resolves to on base. A reviewer with no entry yet is
// resolved only while its newest attempt is live (queued or running): a
// skipped or failed one runs on nothing. The resolve is the one its own
// dispatch does (reviewerDecision) with the repository quota not used up:
// the members of a review are claimed in reviewer order, so an earlier one
// usually has its entry already. A base with no entries read is errNoBase.
func modelsBefore(base chain.Input, ms []store.ReviewMember, k int, instanceUsedUp bool) ([]chain.SameModel, error) {
	if base.Entries == nil {
		return nil, errNoBase
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
		in, err := memberInput(base, ms, newest)
		if err != nil {
			return nil, err
		}
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
// the judge, are what its run recorded when it was dispatched: the rules, the
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

// reasonMemberError fails a member whose result says its review failed and
// gives no message.
const reasonMemberError = "the review failed"

// reasonMemberRefused, then why (chain.ReasonWords), fails an API review's
// member whose provider refused its model: it does not fall back.
const reasonMemberRefused = "the provider refused the model: "

// settleBatch is how many reported members one settle pass takes, as
// publishBatch is for runs.
const settleBatch = 20

// settleMembers decides up to settleBatch reported reviewers, then releases
// each multi review whose reviewers are all final (ReleaseMultiReview): to
// its judge when a reviewer is done, else it fails. It runs in PublishOnce
// before the runs are listed, so a review whose reviewers all failed is
// published as failed in the same pass. Nothing is posted for a member: no
// comment, note, thread, approval or status. A member it can't settle now is
// tried again on the next pass.
func (e *Engine) settleMembers(ctx context.Context) error {
	ms, err := e.Store.ReviewMembersToSettle(ctx, settleBatch)
	if err != nil {
		return fmt.Errorf("list the reported reviewers: %w", err)
	}
	for _, m := range ms {
		if err := e.settleMember(ctx, m); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			e.log().Warn("couldn't settle a reviewer; trying again on the next pass",
				"run", m.RunID, "member", m.ID, "reviewer", m.Reviewer, "error", e.redact(err.Error()))
		}
	}
	// Every waiting review with no live member, not only those settled
	// above: a pass that stopped after a settle releases on the next one.
	roots, err := e.Store.MultiReviewsToRelease(ctx)
	if err != nil {
		return fmt.Errorf("list the multi reviews to release: %w", err)
	}
	for _, root := range roots {
		log := e.log().With("run", root)
		released, failed, err := e.Store.ReleaseMultiReview(ctx, root, e.now())
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("couldn't release the multi review; trying again on the next pass", "error", e.redact(err.Error()))
		case released:
			// The dispatcher starts the judge.
			log.Info("the reviewers are final and one is done; the review is queued for its judge")
			e.Wake()
		case failed:
			// The publisher publishes the failure, as for any run.
			log.Warn("every reviewer failed; the review fails")
			e.Wake()
		}
	}
	return nil
}

// settleMember decides the reported member m. Its stored result is checked
// again as the result handler checks it (callback.CheckStored), since the
// handler stores a result before it checks it: a result that fails the
// check, or says its review failed, fails the member. A result whose
// provider refused the member's entry falls back along the member's frozen
// chain on a merge request's review (fallBackMember); a 1-entry chain never
// falls back (refusedMark). On an API review the entry is marked and the
// member fails. Otherwise the member is done with its result, or failed
// with its failure.
func (e *Engine) settleMember(ctx context.Context, m store.ReviewMember) error {
	log := e.log().With("run", m.RunID, "member", m.ID, "reviewer", m.Reviewer)
	run, found, err := e.Store.MultiReviewRun(ctx, m.RunID)
	if err != nil {
		return fmt.Errorf("load the multi review's run: %w", err)
	}
	if !found {
		// A push or a close took the run: the sweep supersedes the member.
		return nil
	}
	log = log.With("instance", run.MR.Instance, "project", run.ProjectPath, "mr", run.MR.IID, "head", run.HeadSHA)
	if m.Result != nil {
		res, f, ok := callback.CheckStored(m.Result, run)
		if ok && res.Status != "completed" {
			f, ok = store.Failure{Reason: reasonMemberError}, false
			if res.Error != nil && res.Error.Message != "" {
				f.Reason = res.Error.Message
			}
		}
		ref := store.EntryRef{Source: m.Entry.Source, ID: m.Entry.ID}
		if !ok {
			// ErrNotReported: the handler's own check failed it first.
			if err := e.Store.ReplaceReviewMemberWithFailure(ctx, m.ID, f); err != nil && !errors.Is(err, store.ErrNotReported) {
				return fmt.Errorf("fail the member's result: %w", err)
			}
			log.Warn("the reviewer's result can't be used; the reviewer fails", "step", f.Step)
		} else if mark, refused := e.refusedMark(ref, len(m.Chain.Entries), m.Result); refused {
			if run.Kind == store.KindMR {
				return e.fallBackMember(ctx, run, m, mark, log)
			}
			// An API review's member does not fall back (an API review is
			// never split, so it is a left-over one): its entry is marked as
			// the review's would be, and it fails. The audit entry is written
			// once, by the settle that fails the member.
			mlog := log.With("model_entry", m.Entry.ID, "class", mark.Class)
			if err := e.markOnly(ctx, run, m.Entry.Name, mark, false, mlog); err != nil {
				return err
			}
			f := store.Failure{Reason: reasonMemberRefused + chain.ReasonWords(mark.Class)}
			switch err := e.Store.ReplaceReviewMemberWithFailure(ctx, m.ID, f); {
			case err == nil:
				mlog.Warn("the provider refused the reviewer's model entry; it is skipped until the mark ends")
				e.auditMark(ctx, run, m.Entry.Name, mark.Class, mlog)
			case !errors.Is(err, store.ErrNotReported):
				return fmt.Errorf("fail the member's result: %w", err)
			}
		}
	}
	state, err := e.Store.SettleReviewMember(ctx, m.ID)
	if err != nil {
		return fmt.Errorf("settle the member: %w", err)
	}
	if state != "" {
		log.Info("reviewer settled", "state", state)
	}
	return nil
}

// fallBackMember moves member m, whose provider refused its entry, to the
// next entry of its frozen chain, as fallBack does for a run, with the same
// mark and audit entry: m is fell_back and its reviewer's next attempt is
// queued with the same copy, in one transaction. The next attempt is a new
// Job with the full review timeout; it resolves at its own dispatch. When
// the copy has no entry left to run (memberEntryLeft), the entry is marked
// and audited but no attempt is queued: m fails with its result kept
// (FailReviewMemberKeepingResult), so the judge still gets that result and
// pergent carries its blocking findings. The audit entry is written once, by
// the settle that changes the member: a settle tried again after an error
// marks the entry again, and audits nothing more. Nothing is posted, not
// even a status.
func (e *Engine) fallBackMember(ctx context.Context, run store.Run, m store.ReviewMember, mark store.Mark, log *slog.Logger) error {
	log = log.With("model_entry", m.Entry.ID, "class", mark.Class)
	left, err := e.memberEntryLeft(ctx, run, m, mark)
	if err != nil {
		return err
	}
	if !left {
		if err := e.markOnly(ctx, run, m.Entry.Name, mark, false, log); err != nil {
			return err
		}
		failed, err := e.Store.FailReviewMemberKeepingResult(ctx, m.ID, chain.ReasonWords(mark.Class))
		if err != nil {
			return fmt.Errorf("fail the member keeping its result: %w", err)
		}
		if !failed {
			// Superseded by a push meanwhile, or settled already.
			log.Info("not failing the reviewer: it is no longer waiting to be settled")
			return nil
		}
		log.Warn("the provider refused the reviewer's model entry and its chain has no entry left; the reviewer fails and keeps its result")
		e.auditMark(ctx, run, m.Entry.Name, mark.Class, log)
		return nil
	}
	next, created, err := e.Store.EnqueueReviewMemberAttempt(ctx, m, mark)
	if err != nil {
		return fmt.Errorf("queue the member's next attempt: %w", err)
	}
	if !created {
		// Superseded by a push meanwhile, or settled already.
		log.Info("not falling back: the reviewer is no longer waiting to be settled")
		return nil
	}
	log.Warn("the provider refused the reviewer's model entry; trying the next entry of its chain", "next_member", next)
	e.auditMark(ctx, run, m.Entry.Name, mark.Class, log)
	e.Wake()
	return nil
}

// memberEntryLeft reports whether the next attempt of member m's reviewer
// would run on an entry: m's frozen chain resolved as that attempt's
// dispatch resolves it (reviewerDecision, Ruling L6), on the entries, marks,
// limits and token quotas read now, with mark (the one m's refusal sets)
// already in place, and the entries m and the earlier attempts of its
// reviewer ran tried. So reviewer 1 goes past the instance quota and the
// admin limits on a hold, and for reviewer k > 1 a hold (a quota or a
// limit) is no entry left. The live mark reads before "tried" (as for a
// run's fallback), so an entry left that a quota or a limit stops is no
// entry left for reviewer 1 too: its next dispatch would fail it. A
// repository that is no longer configured has an entry left: the next
// attempt's dispatch drops the review.
func (e *Engine) memberEntryLeft(ctx context.Context, run store.Run, m store.ReviewMember, mark store.Mark) (bool, error) {
	repo, haveRepo, err := e.settings().Get(ctx, run.MR.Instance, run.ProjectPath)
	if err != nil {
		return false, fmt.Errorf("load repository settings: %w", err)
	}
	if !haveRepo {
		return true, nil
	}
	repoUsedUp, instanceUsedUp, err := quota.Check(ctx, e.Store, e.now(), run.MR.Instance, run.MR.ProjectID, repo)
	if err != nil {
		return false, fmt.Errorf("read the token quota: %w", err)
	}
	ms, err := e.Store.ReviewMembers(ctx, m.RunID)
	if err != nil {
		return false, fmt.Errorf("read the multi review's members: %w", err)
	}
	base, err := e.chainFor(ctx, run, repo)
	if err != nil {
		return false, fmt.Errorf("read the model chain: %w", err)
	}
	base.Marks = maps.Clone(base.Marks)
	if base.Marks == nil {
		base.Marks = map[store.EntryRef]store.Mark{}
	}
	base.Marks[mark.Ref] = mark
	// The next attempt: every attempt of the reviewer with an entry, m too,
	// is another attempt of it (triedBy).
	next := store.ReviewMember{RunID: m.RunID, Reviewer: m.Reviewer, Chain: m.Chain}
	dec, _, err := reviewerDecision(base, ms, next, repoUsedUp, instanceUsedUp)
	if err != nil {
		return false, err
	}
	return dec.Runs(), nil
}

// reasonNoJudgeImage fails a multi review whose judge Job can't fetch its
// reviewers' results: pergent.sh's own image is not set.
const reasonNoJudgeImage = "a multi review needs pergent.sh's own image (PERGENT_SH_IMAGE, set by the chart) for its judge"

// Why a judge Job runs --no-judge (review_judges.no_judge_reason).
const (
	noJudgeChain    = "no judge chain"
	noJudgeFailed   = "the judge Job failed"
	noJudgeRepo     = "the repository's token quota is used up"
	noJudgeInstance = "the instance's token quota is used up"
	noJudgeEntry    = "no judge entry can be used"
)

// judgePlan is what the judge Job runs with: every reviewer of the frozen
// set, in number order (one with no stored result gets no file); the judge
// entry, or no judge and why.
type judgePlan struct {
	reviewers []int
	entry     chain.Decision // the judge's entry; zero with noJudge
	noJudge   bool
	why       string
}

// key is the only LLM key the judge Job holds: the judge entry's, "" with
// no judge.
func (p judgePlan) key() string {
	if p.noJudge {
		return ""
	}
	return p.entry.Entry.Key
}

// resolveJudge reads the frozen judge chain (JudgeChainOf) with the
// registry now, skipping the entries earlier attempts of this judge fell
// back from (judgedBefore). No judge when: the copy has no entries ("no
// judge chain"); an earlier attempt of the review closed with reason
// judge_failed ("the judge Job failed"); the repository quota is used up;
// or the chain resolves to no entry (marks, keys, the instance quota on
// global entries). d.entry is the judge (OPENCODE_*); with no judge it is
// empty: no LLM and no key (D4).
//
// The judge ends a review that has started, so no model or team limit stops
// it (Ruling L7), and it never holds: a quota gives --no-judge. It may run
// on a reviewer's model. The reviewers are those of d.members.
func (d *dispatcher) resolveJudge(ctx context.Context, repo settings.Effective, repoUsedUp, instanceUsedUp bool) error {
	plan := judgePlan{reviewers: reviewerNumbers(d.members)}
	fc, found, err := d.e.Store.JudgeChainOf(ctx, d.run.ID)
	if err != nil {
		return fmt.Errorf("read the frozen judge chain: %w", err)
	}
	tried, failedBefore, err := d.judgedBefore(ctx)
	if err != nil {
		return err
	}
	switch {
	case !found || len(fc.Entries) == 0:
		plan.why = noJudgeChain
	case failedBefore:
		plan.why = noJudgeFailed
	case repoUsedUp:
		plan.why = noJudgeRepo
	default:
		base, err := d.e.chainFor(ctx, d.run, repo)
		if err != nil {
			return err
		}
		in := chain.FromFrozen(fc, base)
		in.Tried = tried
		in.IgnoreLimits()
		in.InstanceQuotaUsedUp = instanceUsedUp
		d.scope = in.Scope
		dec := chain.Resolve(in)
		if dec.Runs() {
			plan.entry = dec
		} else {
			plan.why = judgeSkipWords(dec)
		}
	}
	plan.noJudge = plan.why != ""
	d.judge, d.entry = plan, plan.entry
	d.attempt = len(tried) + 1
	d.log.Info("multi review judge", "judge", plan.entry.Entry.Name, "reviewers", plan.reviewers, "no_judge", plan.why)
	return nil
}

// judgeSkipWords is why a judge chain that resolves to no entry (dec) does
// not judge: the instance quota when only it stopped the entries an earlier
// attempt did not try, else no judge entry can be used.
func judgeSkipWords(dec chain.Decision) string {
	quota := false
	for _, s := range dec.Skips {
		switch s.Reason {
		case chain.ReasonInstanceQuota:
			quota = true
		case chain.ReasonTried:
		default:
			return noJudgeEntry
		}
	}
	if quota {
		return noJudgeInstance
	}
	return noJudgeEntry
}

// reviewerNumbers is every reviewer number of the members ms (the off row
// left out), each once, in number order: the frozen set, a skipped or
// failed reviewer too (D2).
func reviewerNumbers(ms []store.ReviewMember) []int {
	var out []int
	for _, m := range ms {
		if m.State != store.MemberOff && !slices.Contains(out, m.Reviewer) {
			out = append(out, m.Reviewer)
		}
	}
	slices.Sort(out)
	return out
}

// judgedBefore reads the earlier attempts of the review: tried is the judge
// entries the provider refused on an attempt that fell back from its judge
// (a judge row with an entry, closed as fell_back for a class). The judge
// does not run on them again, even once their mark ends. failed is whether
// an attempt closed with reason judge_failed (EnqueueJudgeRetry): the next
// judge Job runs --no-judge.
func (d *dispatcher) judgedBefore(ctx context.Context) (tried map[store.EntryRef]bool, failed bool, err error) {
	if d.run.AttemptOf == nil {
		return nil, false, nil
	}
	as, err := d.e.Store.Attempts(ctx, *d.run.AttemptOf)
	if err != nil {
		return nil, false, fmt.Errorf("read the review's attempts: %w", err)
	}
	for _, a := range as {
		if a.ID == d.run.ID || a.Outcome != store.OutcomeFellBack {
			continue
		}
		if a.OutcomeReason == store.OutcomeJudgeFailed {
			failed = true
			continue
		}
		j, found, err := d.e.Store.JudgeOf(ctx, a.ID)
		if err != nil {
			return nil, false, fmt.Errorf("read the judge of an earlier attempt: %w", err)
		}
		if !found || j.NoJudge || j.Entry.ID == "" {
			continue
		}
		if tried == nil {
			tried = map[store.EntryRef]bool{}
		}
		tried[store.EntryRef{Source: j.Entry.Source, ID: j.Entry.ID}] = true
	}
	return tried, failed, nil
}

// recordJudge records what the judge Job judges with (review_judges),
// replacing what an earlier try of this run recorded.
func (d *dispatcher) recordJudge(ctx context.Context) error {
	p := d.judge
	j := store.JudgeRow{Reviewers: p.reviewers, NoJudge: p.noJudge, NoJudgeReason: p.why}
	if !p.noJudge {
		j.Provider, j.Model, j.Entry = p.entry.Entry.LLM.Provider, p.entry.Entry.LLM.Model, runEntry(p.entry)
	}
	if err := d.e.Store.SetJudge(ctx, d.run.ID, j); err != nil {
		return fmt.Errorf("record the multi review's judge: %w", err)
	}
	return nil
}

// mrDismissalsJSON is the --mr-dismissals file (D11): one
// {"fingerprint","severity"} per stored dismissal with a fingerprint, in
// store order, no actor or reason; [] for none.
func mrDismissalsJSON(ds []store.Dismissal) []byte {
	type dismissal struct {
		Fingerprint string `json:"fingerprint"`
		Severity    string `json:"severity"`
	}
	out := make([]dismissal, 0, len(ds))
	for _, d := range ds {
		if d.Fingerprint != "" {
			out = append(out, dismissal{Fingerprint: d.Fingerprint, Severity: d.Severity})
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		// Two strings per entry always encode.
		return []byte("[]")
	}
	return b
}
