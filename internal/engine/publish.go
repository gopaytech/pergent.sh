package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
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
)

// GitLabState maps a policy status to a GitLab commit status state. GitLab
// has no neutral state, so neutral is "skipped". Anything else, unknown
// statuses included, is "failed": a status pergent.sh doesn't know is never
// success.
func GitLabState(s policy.Status) gitlab.StatusState {
	switch s {
	case policy.StatusSuccess:
		return gitlab.StatusSuccess
	case policy.StatusNeutral:
		return gitlab.StatusSkipped
	default:
		return gitlab.StatusFailed
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
	bots, ok := e.Bots[run.MR.Instance]
	if !ok || bots.Review == nil {
		return fmt.Errorf("no GitLab bots for instance %q", run.MR.Instance)
	}
	failure := config.FailureStatusFailed
	if repo, ok := e.Config.Repository(run.MR.Instance, run.ProjectPath); ok {
		failure = repo.FailureStatus
	}
	d := policy.Decide(e.outcome(run, log), failure)

	if err := e.upsertComment(ctx, bots, run, d.Comment); err != nil {
		return fmt.Errorf("post comment: %w", err)
	}
	if err := bots.Review.SetCommitStatus(ctx, run.MR.ProjectID, run.HeadSHA, GitLabState(d.Status), d.Description); err != nil {
		return fmt.Errorf("set commit status: %w", err)
	}
	if d.Approve {
		if err := e.approve(ctx, bots, run, log); err != nil {
			return err
		}
	}
	if err := e.Store.MarkPublished(ctx, run.ID); err != nil {
		return fmt.Errorf("mark published: %w", err)
	}
	log.Info("review published", "status", d.Status, "approved", d.Approve)
	return nil
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
func (e *Engine) upsertComment(ctx context.Context, bots Bots, run store.Run, body string) error {
	pid, iid := run.MR.ProjectID, run.MR.IID
	stored, err := e.Store.NoteID(ctx, run.MR)
	if err != nil {
		return fmt.Errorf("load note id: %w", err)
	}
	if stored != 0 {
		err := bots.Review.UpdateNote(ctx, pid, iid, stored, body)
		if err == nil {
			return nil
		}
		if !errors.Is(err, gitlab.ErrNotFound) {
			return err
		}
	}

	noteID, found, err := bots.Review.FindNote(ctx, pid, iid, bots.ReviewUser.ID, policy.Marker)
	if err != nil {
		return fmt.Errorf("find note: %w", err)
	}
	if found && noteID != stored {
		err := bots.Review.UpdateNote(ctx, pid, iid, noteID, body)
		if err != nil && !errors.Is(err, gitlab.ErrNotFound) {
			return err
		}
		found = err == nil
	} else {
		found = false
	}
	if !found {
		if noteID, err = bots.Review.CreateNote(ctx, pid, iid, body); err != nil {
			return fmt.Errorf("create note: %w", err)
		}
	}
	if err := e.Store.SetNoteID(ctx, run.MR, noteID); err != nil {
		return fmt.Errorf("record note id: %w", err)
	}
	return nil
}

// approve approves the run's head with the approve bot, only while the merge
// request is open and still at that head. A head that moved is not an error:
// the review stands for its own commit and the newer head gets its own run.
//
// The approval is recorded before it is made, so an approval that reaches
// GitLab is always known and can be removed when it goes stale, even if
// pergent.sh stops right after it. GitLab refuses a second approval by the
// same user, so a refused approval counts as made when the approve bot is
// already among the approvers (a retry after an approval that landed).
func (e *Engine) approve(ctx context.Context, bots Bots, run store.Run, log *slog.Logger) error {
	if bots.Approve == nil {
		return fmt.Errorf("no approve bot for instance %q", run.MR.Instance)
	}
	mr, err := bots.Review.GetMergeRequest(ctx, run.MR.ProjectID, run.MR.IID)
	if err != nil {
		return fmt.Errorf("get merge request: %w", err)
	}
	if mr.State != "opened" || mr.SHA != run.HeadSHA {
		log.Info("not approving: the merge request is closed or its head moved", "state", mr.State, "mr_head", mr.SHA)
		return nil
	}

	sctx, cancel := settleCtx(ctx)
	err = e.Store.SetApprovedSHA(sctx, run.MR, run.HeadSHA)
	cancel()
	if err != nil {
		return fmt.Errorf("record approval: %w", err)
	}

	err = bots.Approve.Approve(ctx, run.MR.ProjectID, run.MR.IID, run.HeadSHA)
	if errors.Is(err, gitlab.ErrHeadMoved) {
		log.Info("not approving: the merge request head moved")
		return nil
	}
	if err != nil {
		if bots.ApproveUser.ID != 0 {
			ids, lerr := bots.Approve.ApprovedBy(ctx, run.MR.ProjectID, run.MR.IID)
			if lerr == nil && slices.Contains(ids, bots.ApproveUser.ID) {
				log.Info("the approve bot already approves the merge request", "refused_with", e.redact(err.Error()))
				return nil
			}
		}
		return fmt.Errorf("approve: %w", err)
	}
	return nil
}

// retryPublish records a failed publish; the run is tried again after the
// backoff, or becomes publish_failed on the 10th failure. A run superseded
// meanwhile can't be retried, and that is fine.
func (e *Engine) retryPublish(ctx context.Context, run store.Run, text string, log *slog.Logger) {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	notBefore := e.now().Add(publishBackoff(run.Attempts))
	err := e.Store.PublishRetry(sctx, run.ID, text, notBefore)
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

// CleanupOnce deletes the Jobs of superseded runs, sets their commit status
// to canceled when they had been dispatched, and removes the approve bot's
// approvals of commits that are no longer an MR's head. Anything that fails is
// left for the next pass; the returned error joins failures to list the work.
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

	stale, err := e.Store.StaleApprovals(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("cleanup: list stale approvals: %w", err))
	}
	for _, a := range stale {
		if ctx.Err() != nil {
			break
		}
		log := e.log().With("instance", a.MR.Instance, "project_id", a.MR.ProjectID, "mr", a.MR.IID, "approved", a.ApprovedSHA)
		bots, ok := e.Bots[a.MR.Instance]
		if !ok || bots.Approve == nil {
			// The instance was removed from the config; it stays listed on
			// every pass, so don't log it above debug.
			log.Debug("skipping the stale approval: no approve bot for the instance")
			continue
		}
		if err := e.unapprove(ctx, bots, a); err != nil {
			log.Warn("couldn't remove the stale approval; will try again", "error", e.redact(err.Error()))
			continue
		}
		log.Info("stale approval removed")
	}
	return errors.Join(errs...)
}

func (e *Engine) cleanUp(ctx context.Context, run store.Run, log *slog.Logger) error {
	if run.JobName != "" {
		if err := e.Launcher.Delete(ctx, run.JobName); err != nil {
			return err
		}
	}
	if !run.DispatchedAt.IsZero() {
		bots, ok := e.Bots[run.MR.Instance]
		if ok && bots.Review != nil {
			if err := bots.Review.SetCommitStatus(ctx, run.MR.ProjectID, run.HeadSHA, gitlab.StatusCanceled, supersededDescription); err != nil {
				return fmt.Errorf("set canceled status: %w", err)
			}
		} else {
			// The instance was removed from the config: nothing can be
			// posted there any more, so don't keep trying.
			log.Warn("not setting the canceled status: no GitLab bots for the instance")
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
// recorded meanwhile is kept.
func (e *Engine) unapprove(ctx context.Context, bots Bots, a store.StaleApproval) error {
	if err := bots.Approve.Unapprove(ctx, a.MR.ProjectID, a.MR.IID); err != nil {
		return fmt.Errorf("unapprove: %w", err)
	}
	if err := e.Store.ClearApprovedSHA(ctx, a.MR, a.ApprovedSHA); err != nil {
		return fmt.Errorf("clear recorded approval: %w", err)
	}
	return nil
}

// redact removes the secrets the engine knows from text bound for the store
// or a log. The GitLab clients already mask their own tokens.
func (e *Engine) redact(text string) string {
	if e.LLMKey != "" {
		text = strings.ReplaceAll(text, e.LLMKey, "***")
	}
	return text
}
