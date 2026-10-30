package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// SweepApprovals checks every approval the approve bot holds against GitLab,
// so an approval never outlives the commit it was for when a webhook was
// lost (GitLab doesn't retry failed deliveries: pergent.sh down during a
// rollout, a disabled hook). For each recorded approval it reads the merge
// request with the review bot:
//   - merged, or gone (404): the record is cleared, nothing is written;
//   - closed: the approval is withdrawn (it could be reopened), then cleared;
//   - open at another head, or retargeted: the approval is withdrawn and
//     cleared, and while the repository is still configured a run of the
//     current head against the current target is queued;
//   - open at the approved head and target: nothing changes.
//
// One merge request's trouble is logged and left for the next sweep; the
// returned error is a failure to list the approvals.
func (e *Engine) SweepApprovals(ctx context.Context) error {
	approvals, err := e.Store.Approvals(ctx)
	if err != nil {
		return fmt.Errorf("sweep approvals: list approvals: %w", err)
	}
	wake := false
	for _, a := range approvals {
		if ctx.Err() != nil {
			break
		}
		log := e.log().With("instance", a.MR.Instance, "project_id", a.MR.ProjectID, "mr", a.MR.IID,
			"approved", a.ApprovedSHA, "approved_target", a.ApprovedTarget)
		bots, ok := e.Bots[a.MR.Instance]
		if !ok || bots.Review == nil || bots.Approve == nil {
			// The instance was removed from the config; it is listed on every
			// sweep, so don't log it above debug.
			log.Debug("skipping the approval: no GitLab bots for the instance")
			continue
		}
		queued, err := e.sweepApproval(ctx, bots, a, log)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Warn("couldn't check the approval; will try again", "error", e.redact(err.Error()))
			continue
		}
		wake = wake || queued
	}
	if wake {
		e.Wake()
	}
	return nil
}

// sweepApproval handles one approval and reports whether it queued a run.
func (e *Engine) sweepApproval(ctx context.Context, bots Bots, a store.Approval, log *slog.Logger) (bool, error) {
	mr, err := bots.Review.GetMergeRequest(ctx, a.MR.ProjectID, a.MR.IID)
	if errors.Is(err, gitlab.ErrNotFound) {
		log.Info("approval forgotten: the merge request is gone")
		return false, e.clearApproval(ctx, a)
	}
	if err != nil {
		return false, fmt.Errorf("get merge request: %w", err)
	}
	switch mr.State {
	case "opened":
	case "merged":
		log.Info("approval forgotten: the merge request is merged")
		return false, e.clearApproval(ctx, a)
	case "closed":
		if err := e.unapprove(ctx, bots, a); err != nil {
			return false, err
		}
		log.Info("approval of a closed merge request removed")
		return false, nil
	default: // "locked": a merge is in progress; look again next time
		return false, nil
	}
	targetMoved := a.ApprovedTarget != "" && mr.TargetBranch != a.ApprovedTarget
	if mr.SHA == a.ApprovedSHA && !targetMoved {
		return false, nil
	}

	if err := e.unapprove(ctx, bots, a); err != nil {
		return false, err
	}
	log.Info("stale approval removed: the merge request moved without a webhook pergent.sh saw",
		"mr_head", mr.SHA, "mr_target", mr.TargetBranch)
	if mr.SHA == "" {
		return false, nil
	}
	project, found, err := e.Store.Project(ctx, a.MR.Instance, a.MR.ProjectID)
	if err != nil {
		return false, fmt.Errorf("load project: %w", err)
	}
	if !found {
		return false, nil
	}
	repo, ok := e.Config.Repository(a.MR.Instance, project.Path)
	if !ok {
		return false, nil
	}
	id, created, err := e.Store.EnqueueRun(ctx, a.MR, repo.Project, mr.SHA, mr.TargetBranch)
	if err != nil {
		return false, fmt.Errorf("enqueue the current head: %w", err)
	}
	log.Info("queued a review of the current head", "run", id, "created", created, "head", mr.SHA, "target", mr.TargetBranch)
	return created, nil
}

func (e *Engine) clearApproval(ctx context.Context, a store.Approval) error {
	if err := e.Store.ClearApprovedSHA(ctx, a.MR, a.ApprovedSHA, a.ApprovedTarget); err != nil {
		return fmt.Errorf("clear recorded approval: %w", err)
	}
	return nil
}
