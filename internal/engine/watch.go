package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// watchGrace is how long a finished or vanished Job's result may still be
	// on its way before the run is reported as failed.
	watchGrace = 60 * time.Second
	// unlaunchedAfter is how old a claim without a Job must be before the
	// watcher puts the run back in the queue. The dispatcher runs in the
	// same goroutine as the watcher, so no claim it holds is ever seen; the
	// margin covers clock skew with the database.
	unlaunchedAfter = 5 * time.Minute

	reasonDisappeared    = "the review job disappeared before sending a result"
	reasonFinishedNoSend = "the review job finished without sending a result"
)

// WatchOnce looks at the Job of every running run and turns a Job that can no
// longer send a result into the run's failure reason, so the publisher posts
// it instead of the merge request hearing nothing:
//   - a Job stuck on an unrecoverable start error is deleted, then reported;
//   - a Job gone for 60 s after dispatch is reported;
//   - a Job finished for 60 s without a result is reported, with why it
//     failed when the cluster says.
//
// It also records the checkout time a Job measured, and requeues runs claimed
// over 5 minutes ago that never got a Job. A result that wins the race with a
// report is kept. One Job the cluster can't be asked about is logged and
// skipped; the returned error joins store failures.
func (e *Engine) WatchOnce(ctx context.Context) error {
	now := e.now()
	var errs []error
	wake := false

	if n, err := e.Store.RecoverUnlaunched(ctx, now.Add(-unlaunchedAfter)); err != nil {
		errs = append(errs, fmt.Errorf("watch: requeue stranded runs: %w", err))
	} else if n > 0 {
		e.log().Warn("requeued runs stranded before their job started", "runs", n)
		wake = true
	}

	runs, err := e.Store.RunningRuns(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("watch: list running runs: %w", err))
	}
	for _, run := range runs {
		if ctx.Err() != nil {
			break
		}
		if run.JobName == "" {
			continue
		}
		reported, err := e.watch(ctx, run, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("watch run %s: %w", run.ID, err))
		}
		wake = wake || reported
	}
	if wake {
		e.Wake()
	}
	return errors.Join(errs...)
}

// watch handles one running run. It reports whether it reported the run.
func (e *Engine) watch(ctx context.Context, run store.Run, now time.Time) (bool, error) {
	log := e.log().With("run", run.ID, "instance", run.MR.Instance, "project", run.ProjectPath,
		"mr", run.MR.IID, "head", run.HeadSHA, "job", run.JobName)
	obs, err := e.Launcher.Inspect(ctx, run.JobName, now)
	if err != nil {
		log.Warn("couldn't inspect the review job; will look again", "error", e.redact(err.Error()))
		return false, nil
	}

	if obs.Clone > 0 {
		seconds := int((obs.Clone + time.Second - 1) / time.Second)
		if err := e.Store.SetCloneSeconds(ctx, run.MR.Instance, run.MR.ProjectID, seconds); err != nil {
			log.Warn("couldn't record the checkout time", "error", e.redact(err.Error()))
		}
	}

	var reason string
	switch {
	case obs.Stuck != "":
		if err := e.Launcher.Delete(ctx, run.JobName); err != nil {
			log.Warn("couldn't delete the stuck review job; will try again", "reason", obs.Stuck, "error", e.redact(err.Error()))
			return false, nil
		}
		reason = obs.Stuck
	case !obs.Exists:
		if now.Sub(run.DispatchedAt) < watchGrace {
			return false, nil
		}
		reason = reasonDisappeared
	case obs.Finished:
		if now.Sub(obs.FinishedAt) < watchGrace {
			return false, nil
		}
		reason = obs.Failure
		if reason == "" {
			reason = reasonFinishedNoSend
		}
	default:
		return false, nil
	}

	changed, err := e.Store.ReportFailure(ctx, run.ID, reason, now)
	if err != nil {
		return false, fmt.Errorf("report failure: %w", err)
	}
	if !changed {
		log.Info("the run got its result meanwhile; not reporting the job", "reason", reason)
		return false, nil
	}
	log.Warn("review job ended without a result; run reported as failed", "reason", reason)
	return true, nil
}
