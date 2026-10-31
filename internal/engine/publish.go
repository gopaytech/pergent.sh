package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// publishBatch is how many reported runs one publish pass takes.
	publishBatch = 20
	// publishBackoffBase is the first publish retry delay; it doubles each
	// attempt up to publishBackoffMax.
	publishBackoffBase = 10 * time.Second
	publishBackoffMax  = 10 * time.Minute

	reasonUnusableResult  = "pergent sent a result pergent.sh couldn't use: "
	supersededDescription = "Superseded by a newer push"
	// gaveUpDescription is the final status of a run whose review couldn't
	// be posted.
	gaveUpDescription = "pergent couldn't post its review"
)

// StatusState maps a policy status to the status set on the head. Anything
// but success and neutral, unknown statuses included, is failed: a status
// pergent.sh doesn't know is never success.
func StatusState(s policy.Status) forge.StatusState {
	switch s {
	case policy.StatusSuccess:
		return forge.StatusSuccess
	case policy.StatusNeutral:
		return forge.StatusNeutral
	default:
		return forge.StatusFailed
	}
}

// publishBackoff is the delay before the next publish of a run that has
// failed attempts times before: 10 s × 2^attempts, at most 10 minutes.
func publishBackoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 6 { // 10 s << 6 is already past the cap
		return publishBackoffMax
	}
	return min(publishBackoffBase<<attempts, publishBackoffMax)
}

// PublishOnce posts up to 20 due reported runs: for each, the pergent comment
// (updated in place), the commit status on the run's head and, only for a
// clean review of the merge request's current head, the approve bot's
// approval. A GitLab or store error puts the run back with backoff (the
// store gives up after the 10th attempt). One run's trouble never stops the
// others; the returned error is a failure to list the runs.
func (e *Engine) PublishOnce(ctx context.Context) error {
	runs, err := e.Store.RunsToPublish(ctx, e.now(), publishBatch)
	if err != nil {
		return fmt.Errorf("publish: list reported runs: %w", err)
	}
	for _, run := range runs {
		log := e.log().With("run", run.ID, "instance", run.MR.Instance, "project", run.ProjectPath,
			"mr", run.MR.IID, "head", run.HeadSHA)
		err := e.publish(ctx, run, log)
		if err == nil {
			continue
		}
		text := e.redact(err.Error())
		if ctx.Err() != nil {
			// Shutting down: the error is ours, not GitLab's. The run stays
			// reported and due, and no attempt is counted.
			log.Info("publish interrupted; the run will be published later", "error", text)
			return nil
		}
		e.retryPublish(ctx, run, text, log)
	}
	return nil
}

// publish posts one run. Any error means the run was not published.
func (e *Engine) publish(ctx context.Context, run store.Run, log *slog.Logger) error {
	plat, ok := e.Platforms.Get(run.MR.Instance)
	if !ok || plat.Forge == nil {
		return fmt.Errorf("no bots for instance %q", run.MR.Instance)
	}
	failure := config.FailureStatusFailed
	repo, ok, err := e.settings().Get(ctx, run.MR.Instance, run.ProjectPath)
	if err != nil {
		return fmt.Errorf("load repository settings: %w", err)
	}
	if ok {
		failure = repo.FailureStatus
	}
	o := e.outcome(run, log)
	// A repository switched off since the run was queued approves nothing
	// more: its review is posted as with approval turned off.
	o.ApprovalOff = !ok || !repo.Approve
	if policy.Decide(o, failure).Approve {
		// Only a review that would be approved depends on whether the
		// change is a draft and the approve side can reach the repository.
		var err error
		if o, err = e.approveInputs(ctx, plat, run, o); err != nil {
			return err
		}
	}
	d := policy.Decide(o, failure)

	if err := e.upsertComment(ctx, plat.Forge, run, d.Comment); err != nil {
		return fmt.Errorf("post comment: %w", err)
	}
	if err := plat.Forge.SetStatus(ctx, run.MR.ProjectID, run.HeadSHA, StatusState(d.Status), d.Description); err != nil {
		return fmt.Errorf("set commit status: %w", err)
	}
	approved, held := false, approvalMade
	if d.Approve {
		var err error
		if approved, held, err = e.approve(ctx, plat, run, log); err != nil {
			return err
		}
	}
	if err := e.Store.MarkPublished(ctx, run.ID, decisionOf(o, d, approved, held)); err != nil {
		return fmt.Errorf("mark published: %w", err)
	}
	log.Info("review published", "status", d.Status, "approved", approved)
	return nil
}

// heldBack is why approve didn't approve a review it was asked to.
type heldBack int

const (
	approvalMade heldBack = iota
	// heldDraft: the change became a draft again.
	heldDraft
	// heldMoved: the change was closed or merged, or its head or target
	// branch moved.
	heldMoved
)

const (
	reasonHeldDraft = "No blocking issues; not approved: draft"
	reasonHeldMoved = "No blocking issues; not approved: the change was closed, or its head or target branch moved, before it could be approved"
)

// decisionOf is what is recorded on a run for the dashboard: the outcome,
// why, the status set and whether the approve bot approved it.
func decisionOf(o policy.Outcome, d policy.Decision, approved bool, held heldBack) store.RunDecision {
	rd := store.RunDecision{Status: string(d.Status), Approved: approved, Reason: d.Description}
	switch d.Kind {
	case policy.KindBlocking:
		rd.Outcome = store.OutcomeBlocking
	case policy.KindNotApproved:
		rd.Outcome = store.OutcomeNoBlocking
	case policy.KindDraft:
		rd.Outcome = store.OutcomeDraft
	case policy.KindApprove:
		switch {
		case approved:
			rd.Outcome, rd.Reason = store.OutcomeApproved, ""
		case held == heldDraft:
			rd.Outcome, rd.Reason = store.OutcomeDraft, reasonHeldDraft
		default:
			rd.Outcome, rd.Reason = store.OutcomeNoBlocking, reasonHeldMoved
		}
	default: // policy.KindFailed and anything unknown
		rd.Outcome = store.OutcomeFailed
		switch {
		case o.FailureReason != "":
			rd.Reason = o.FailureReason
		case o.Result != nil && o.Result.Status != "completed" && o.Result.Error != nil && o.Result.Error.Message != "":
			rd.Reason = o.Result.Error.Message
		}
	}
	return rd
}

// approveInputs adds to o what decides whether a clean review is approved:
// the change being a draft (read from the platform) and, on GitHub, whose
// approve App or bot can be missing from a repository, the stored project
// saying so (and which of the two the instance uses). On GitLab the approve bot is a user that can always approve.
func (e *Engine) approveInputs(ctx context.Context, plat Platform, run store.Run, o policy.Outcome) (policy.Outcome, error) {
	ch, err := plat.Forge.GetChange(ctx, run.MR.ProjectID, run.MR.IID)
	if err != nil {
		return o, fmt.Errorf("get merge request: %w", err)
	}
	o.Draft = ch.Draft
	if plat.isGitHub() {
		p, found, err := e.Store.Project(ctx, run.MR.Instance, run.MR.ProjectID)
		if err != nil {
			return o, fmt.Errorf("load project: %w", err)
		}
		o.ApproveUnavailable = found && !p.ApproveAvailable
		if inst, ok := e.registry().Get(run.MR.Instance); ok && inst.GitHub != nil {
			o.ApproveBot = inst.GitHub.ApproveApp == nil
		}
	}
	return o, nil
}

// outcome is what the run produced. A stored result is checked again: it has
// to parse and be for the run's own base and head, or the run is decided as a
// failure.
func (e *Engine) outcome(run store.Run, log *slog.Logger) policy.Outcome {
	if run.FailureReason != "" || run.Result == nil {
		return policy.Outcome{FailureReason: run.FailureReason}
	}
	r, err := result.Parse(run.Result)
	if err != nil {
		log.Warn("stored result can't be used", "error", err)
		return policy.Outcome{FailureReason: reasonUnusableResult + err.Error()}
	}
	if r.Base != run.BaseSHA || r.Head != run.HeadSHA {
		log.Warn("stored result is for another commit", "result_base", r.Base, "result_head", r.Head)
		return policy.Outcome{FailureReason: reasonUnusableResult + fmt.Sprintf(
			"it is for a different commit (base %q, head %q) than the one reviewed (base %s, head %s)",
			r.Base, r.Head, run.BaseSHA, run.HeadSHA)}
	}
	return policy.Outcome{Result: r}
}

// upsertComment updates the merge request's pergent comment, or creates it,
// with the review bot. The stored note is tried first; when it was deleted,
// the review bot's note carrying the marker is looked up, and only when there
// is none is a new one created.
func (e *Engine) upsertComment(ctx context.Context, f forge.Forge, run store.Run, body string) error {
	pid, iid := run.MR.ProjectID, run.MR.IID
	stored, err := e.Store.NoteID(ctx, run.MR)
	if err != nil {
		return fmt.Errorf("load note id: %w", err)
	}
	if stored != 0 {
		err := f.UpdateComment(ctx, pid, iid, stored, body)
		if err == nil {
			return nil
		}
		if !errors.Is(err, forge.ErrNotFound) {
			return err
		}
	}

	noteID, found, err := f.FindComment(ctx, pid, iid, policy.Marker)
	if err != nil {
		return fmt.Errorf("find note: %w", err)
	}
	if found && noteID != stored {
		err := f.UpdateComment(ctx, pid, iid, noteID, body)
		if err != nil && !errors.Is(err, forge.ErrNotFound) {
			return err
		}
		found = err == nil
	} else {
		found = false
	}
	if !found {
		if noteID, err = f.CreateComment(ctx, pid, iid, body); err != nil {
			return fmt.Errorf("create note: %w", err)
		}
	}
	if err := e.Store.SetNoteID(ctx, run.MR, noteID); err != nil {
		return fmt.Errorf("record note id: %w", err)
	}
	return nil
}

// approve approves the run's head with the approve bot, only while the merge
// request is open, not a draft, still at that head and still targets the
// run's branch. It reads the merge request again right before approving (the
// read that decided the comment came before the comment and status were
// posted). A head that moved is not an error:
// the review stands for its own commit and the newer head gets its own run.
//
// The approval is recorded before it is made, so an approval that reaches
// GitLab is always known and can be removed when it goes stale, even if
// pergent.sh stops right after it. GitLab refuses a second approval by the
// same user, so a refused approval counts as made when the approve bot
// already approves (a retry after an approval that landed).
//
// It reports whether the approval was made and, when not, why.
func (e *Engine) approve(ctx context.Context, plat Platform, run store.Run, log *slog.Logger) (bool, heldBack, error) {
	if plat.Approver == nil {
		return false, approvalMade, fmt.Errorf("no approve bot for instance %q", run.MR.Instance)
	}
	mr, err := plat.Forge.GetChange(ctx, run.MR.ProjectID, run.MR.IID)
	if err != nil {
		return false, approvalMade, fmt.Errorf("get merge request: %w", err)
	}
	if mr.State != forge.StateOpen || mr.HeadSHA != run.HeadSHA || mr.TargetBranch != run.TargetBranch || mr.Draft {
		log.Info("not approving: the merge request is closed or a draft again, or its head or target branch moved",
			"state", mr.State, "draft", mr.Draft, "mr_head", mr.HeadSHA, "mr_target", mr.TargetBranch, "run_target", run.TargetBranch)
		if mr.State == forge.StateOpen && mr.HeadSHA == run.HeadSHA && mr.TargetBranch == run.TargetBranch {
			return false, heldDraft, nil
		}
		return false, heldMoved, nil
	}

	sctx, cancel := settleCtx(ctx)
	err = e.Store.SetApprovedSHA(sctx, run.MR, run.HeadSHA, run.TargetBranch)
	cancel()
	if err != nil {
		return false, approvalMade, fmt.Errorf("record approval: %w", err)
	}

	err = plat.Approver.Approve(ctx, run.MR.ProjectID, run.MR.IID, run.HeadSHA)
	if errors.Is(err, forge.ErrHeadMoved) {
		log.Info("not approving: the merge request head moved")
		return false, heldMoved, nil
	}
	if err != nil {
		if approved, lerr := plat.Approver.HasApproved(ctx, run.MR.ProjectID, run.MR.IID, run.HeadSHA); lerr == nil && approved {
			log.Info("the approve bot already approves the merge request", "refused_with", e.redact(err.Error()))
			return true, approvalMade, nil
		}
		return false, approvalMade, fmt.Errorf("approve: %w", err)
	}
	return true, approvalMade, nil
}

// retryPublish records a failed publish; the run is tried again after the
// backoff, or becomes publish_failed on the 10th failure. A run superseded
// meanwhile can't be retried, and that is fine.
func (e *Engine) retryPublish(ctx context.Context, run store.Run, text string, log *slog.Logger) {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	notBefore := e.now().Add(publishBackoff(run.Attempts))
	gaveUp, err := e.Store.PublishRetry(sctx, run.ID, text, notBefore)
	if err == nil && gaveUp {
		log.Error("couldn't publish the review; giving up", "attempts", run.Attempts+1, "error", text)
		e.giveUpStatus(sctx, run, log)
		return
	}
	if err == nil {
		log.Warn("couldn't publish the review; will retry", "attempt", run.Attempts+1, "retry_at", notBefore, "error", text)
		return
	}
	if r, ok, rerr := e.Store.Run(sctx, run.ID); rerr == nil && ok && r.State != store.Reported {
		log.Info("run changed while it was published; not retrying", "state", r.State, "error", text)
		return
	}
	log.Error("couldn't record the failed publish", "error", e.redact(err.Error()), "publish_error", text)
}

// giveUpStatus makes one best-effort attempt to set a failed status on the
// head of a run that became publish_failed, so a required check isn't left
// running (GitHub's Check Run stays in_progress from dispatch otherwise). It
// runs once, on the transition the store reports; nothing is approved.
func (e *Engine) giveUpStatus(ctx context.Context, run store.Run, log *slog.Logger) {
	plat, ok := e.Platforms.Get(run.MR.Instance)
	if !ok || plat.Forge == nil {
		log.Warn("couldn't set the final status: no bots for the instance")
		return
	}
	if err := plat.Forge.SetStatus(ctx, run.MR.ProjectID, run.HeadSHA, forge.StatusFailed, gaveUpDescription); err != nil {
		log.Warn("couldn't set the final status of the review that wasn't posted", "error", e.redact(err.Error()))
	}
}

// CleanupOnce deletes the Jobs of superseded runs (keeping each one's log
// first, best effort, within 10 s), sets their commit status
// to canceled when they had been dispatched, and removes the approve bot's
// approvals of a head or target branch that is no longer the MR's newest
// run's, and every approval it holds in a repository whose "approve clean
// reviews" is turned off. Anything that fails is left for a later pass: a
// run for the next one, an approval the bot couldn't remove for after its
// backoff (30 s × 2^failures, at most 5 minutes), as a refused removal would
// otherwise cost GitHub calls on every pass. The returned error joins
// failures to list the work.
func (e *Engine) CleanupOnce(ctx context.Context) error {
	var errs []error
	runs, err := e.Store.RunsToCleanUp(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("cleanup: list superseded runs: %w", err))
	}
	for _, run := range runs {
		if ctx.Err() != nil {
			return errors.Join(errs...)
		}
		log := e.log().With("run", run.ID, "instance", run.MR.Instance, "project", run.ProjectPath,
			"mr", run.MR.IID, "head", run.HeadSHA)
		if err := e.cleanUp(ctx, run, log); err != nil {
			log.Warn("couldn't clean up the superseded run; will try again", "error", e.redact(err.Error()))
		}
	}

	stale, err := e.Store.StaleApprovals(ctx, e.now())
	if err != nil {
		errs = append(errs, fmt.Errorf("cleanup: list stale approvals: %w", err))
	}
	tried := make(map[store.Approval]bool, len(stale))
	for _, a := range stale {
		if ctx.Err() != nil {
			return errors.Join(errs...)
		}
		tried[a] = true
		e.withdraw(ctx, a, "stale approval removed", "couldn't remove the stale approval; will try again")
	}

	// Listed after the stale ones were handled: one whose removal just
	// failed is in its backoff, one removed is gone.
	off, err := e.approvalOffApprovals(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, a := range off {
		if ctx.Err() != nil {
			break
		}
		if tried[a] {
			continue
		}
		e.withdrawApprovalOff(ctx, a)
	}
	return errors.Join(errs...)
}

// withdrawApprovalOff withdraws approval a of a repository with approval
// turned off, reading its merge request first as the approval sweep does:
// merged or gone, the record is only cleared, as there is nothing to
// withdraw; a merge in progress is left for the next pass. A failure to read
// it waits out the unapprove backoff.
func (e *Engine) withdrawApprovalOff(ctx context.Context, a store.Approval) {
	log := e.log().With("instance", a.MR.Instance, "project_id", a.MR.ProjectID, "mr", a.MR.IID, "approved", a.ApprovedSHA)
	plat, ok := e.Platforms.Get(a.MR.Instance)
	if !ok || plat.Forge == nil || plat.Approver == nil {
		log.Debug("skipping the approval: no bots for the instance")
		return
	}
	mr, err := plat.Forge.GetChange(ctx, a.MR.ProjectID, a.MR.IID)
	switch {
	case errors.Is(err, forge.ErrNotFound) || (err == nil && mr.State == forge.StateMerged):
		if err := e.clearApproval(ctx, a); err != nil {
			log.Warn("couldn't forget the approval of a merged merge request; will try again", "error", e.redact(err.Error()))
			return
		}
		log.Info("approval forgotten: the merge request is merged or gone")
		return
	case err != nil:
		attrs := []any{"error", e.redact(err.Error())}
		if ctx.Err() == nil {
			retryAt, ferr := e.Store.UnapproveFailed(ctx, a.MR, a.ApprovedSHA, a.ApprovedTarget, e.now())
			if ferr != nil {
				attrs = append(attrs, "record_error", e.redact(ferr.Error()))
			} else if !retryAt.IsZero() {
				attrs = append(attrs, "retry_at", retryAt)
			}
		}
		log.Warn("couldn't read the merge request of an approval to withdraw; will try again", attrs...)
		return
	case mr.State != forge.StateOpen && mr.State != forge.StateClosed:
		return // "locked": a merge is in progress; look again next time
	}
	e.withdraw(ctx, a, "approval withdrawn: approval is turned off for the repository",
		"couldn't withdraw the approval of a repository with approval turned off; will try again")
}

// withdraw removes the approve bot's approval a and logs done, or failed
// with when it is tried again. An instance without an approve bot (removed
// from the config) is skipped quietly: it stays listed on every pass.
func (e *Engine) withdraw(ctx context.Context, a store.Approval, done, failed string) {
	log := e.log().With("instance", a.MR.Instance, "project_id", a.MR.ProjectID, "mr", a.MR.IID, "approved", a.ApprovedSHA)
	plat, ok := e.Platforms.Get(a.MR.Instance)
	if !ok || plat.Approver == nil {
		log.Debug("skipping the approval: no approve bot for the instance")
		return
	}
	if retryAt, err := e.unapprove(ctx, plat.Approver, a); err != nil {
		attrs := []any{"error", e.redact(err.Error())}
		if !retryAt.IsZero() {
			attrs = append(attrs, "retry_at", retryAt)
		}
		log.Warn(failed, attrs...)
		return
	}
	log.Info(done)
}

func (e *Engine) cleanUp(ctx context.Context, run store.Run, log *slog.Logger) error {
	if run.JobName != "" {
		// Its log goes with the Job: keep it first, best effort and once.
		e.captureLogWithin(ctx, run, log.With("job", run.JobName), supersededLogTimeout)
		if err := e.Launcher.Delete(ctx, run.JobName); err != nil {
			return err
		}
	}
	if !run.DispatchedAt.IsZero() {
		plat, ok := e.Platforms.Get(run.MR.Instance)
		if ok && plat.Forge != nil {
			if err := plat.Forge.SetStatus(ctx, run.MR.ProjectID, run.HeadSHA, forge.StatusCanceled, supersededDescription); err != nil {
				return fmt.Errorf("set canceled status: %w", err)
			}
		} else {
			// The instance was removed from the config: nothing can be
			// posted there any more, so don't keep trying.
			log.Warn("not setting the canceled status: no bots for the instance")
		}
	}
	if err := e.Store.MarkCleanedUp(ctx, run.ID); err != nil {
		return fmt.Errorf("mark cleaned up: %w", err)
	}
	log.Info("superseded run cleaned up", "job", run.JobName)
	return nil
}

// unapprove removes the approve bot's stale approval, then clears the record
// only if it still names the stale commit: an approval of a newer head
// recorded meanwhile is kept. When the bot can't remove it, the record is
// kept and put aside until the returned time (zero when the record changed
// meanwhile, or pergent.sh is shutting down: that failure isn't the
// platform's and doesn't count).
func (e *Engine) unapprove(ctx context.Context, ap forge.Approver, a store.Approval) (time.Time, error) {
	if err := ap.Unapprove(ctx, a.MR.ProjectID, a.MR.IID); err != nil {
		err = fmt.Errorf("unapprove: %w", err)
		if ctx.Err() != nil {
			return time.Time{}, err
		}
		retryAt, ferr := e.Store.UnapproveFailed(ctx, a.MR, a.ApprovedSHA, a.ApprovedTarget, e.now())
		if ferr != nil {
			return time.Time{}, errors.Join(err, fmt.Errorf("record the failed unapprove: %w", ferr))
		}
		return retryAt, err
	}
	if err := e.Store.ClearApprovedSHA(ctx, a.MR, a.ApprovedSHA, a.ApprovedTarget); err != nil {
		return time.Time{}, fmt.Errorf("clear recorded approval: %w", err)
	}
	return time.Time{}, nil
}

// redact removes the secrets the engine knows from text bound for the store
// or a log. The GitLab clients already mask their own tokens.
func (e *Engine) redact(text string) string {
	if e.LLMKey != "" {
		text = strings.ReplaceAll(text, e.LLMKey, "***")
	}
	return text
}
