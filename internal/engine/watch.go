package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/kube"
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
//   - a Job stuck on an unrecoverable start error has what it left kept, is
//     deleted, then reported;
//   - a Job gone for 60 s after dispatch is reported;
//   - a Job finished for 60 s without a result is reported, with why it
//     failed when the cluster says.
//
// Each report names the step the Job failed at. Along the way it keeps the
// log of each finished Job once, and the diagnostics of a failed one (see
// captureLog), whether its run is still running or already reported.
//
// It also records the checkout time a Job measured, the Job's pod and how far
// its checkout got, deletes an API review's files once its checkout is done
// (or its Job is reported), and requeues runs claimed
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
	if err := e.captureReportedLogs(ctx, now); err != nil && ctx.Err() == nil {
		// The log is a convenience: a failure here is logged, never returned.
		e.log().Info("couldn't list the runs whose log is to be kept", "error", e.redact(err.Error()))
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

	// The pod and how far its checkout got, for the review page and for the
	// step a deadline hit (the deadline takes the pod with it).
	if obs.PodName != "" && obs.PodName != run.PodName {
		if err := e.Store.SetRunPod(ctx, run.ID, obs.PodName); err != nil {
			log.Info("couldn't record the review job's pod", "error", e.redact(err.Error()))
		}
	}
	seen := max(run.CheckoutSeen, obs.Checkout)
	if seen > run.CheckoutSeen {
		if err := e.Store.AdvanceCheckoutSeen(ctx, run.ID, seen); err != nil {
			log.Info("couldn't record how far the checkout got", "error", e.redact(err.Error()))
		}
	}
	// An API review's files are read by its checkout alone: they go once it
	// is done.
	isAPI := run.Kind == store.KindAPI
	if isAPI && run.CheckoutSeen < 2 && seen >= 2 {
		e.deleteFiles(ctx, run, log)
	}
	if obs.Failure.Deadline {
		if obs.Failure.Step == kube.StepClone {
			// The deadline killed the checkout.
			seen = max(seen, 1)
		}
		obs.Failure = kube.DeadlineFailure(seen)
	}

	if obs.Exists && obs.Finished {
		// The Job is deleted an hour after it finishes: keep its log now.
		e.captureLog(ctx, run, obs, log)
	}

	// The checkout container also clones the linked repositories of a run that
	// has them: that time is not the repository's, and would lengthen the
	// deadline of a later review that clones nothing else. An API review
	// clones nothing and has no repository.
	if obs.Clone > 0 && len(run.LinkedRepos) == 0 && !isAPI {
		seconds := int((obs.Clone + time.Second - 1) / time.Second)
		if err := e.Store.SetCloneSeconds(ctx, run.MR.Instance, run.MR.ProjectID, seconds); err != nil {
			log.Warn("couldn't record the checkout time", "error", e.redact(err.Error()))
		}
	}

	var f kube.Failure
	switch {
	case obs.Stuck.Reason != "":
		// Its pod, events and Secret go with the Job: keep what it left, and
		// redact the detail, first.
		e.captureLog(ctx, run, obs, log)
		f = obs.Stuck
		f.Detail = e.redactDetail(ctx, run, f.Detail, log)
		if err := e.Launcher.Delete(ctx, run.JobName); err != nil {
			log.Warn("couldn't delete the stuck review job; will try again", "reason", f.Reason, "error", e.redact(err.Error()))
			return false, nil
		}
	case !obs.Exists:
		if now.Sub(run.DispatchedAt) < watchGrace {
			return false, nil
		}
		f = kube.Failure{Step: kube.StepUnknown, Reason: reasonDisappeared}
	case obs.Finished:
		if now.Sub(obs.FinishedAt) < watchGrace {
			return false, nil
		}
		f = obs.Failure
		if f.Reason == "" {
			f = kube.Failure{Step: kube.StepDelivery, Reason: reasonFinishedNoSend}
		}
		f.Detail = e.redactDetail(ctx, run, f.Detail, log)
	default:
		return false, nil
	}

	if isAPI {
		// The Job can't read them any more; its deletion would remove them
		// only later.
		e.deleteFiles(ctx, run, log)
	}
	changed, err := e.Store.ReportFailure(ctx, run.ID, store.Failure{Reason: f.Reason, Step: f.Step, Detail: f.Detail}, now)
	if err != nil {
		return false, fmt.Errorf("report failure: %w", err)
	}
	if !changed {
		log.Info("the run got its result meanwhile; not reporting the job", "reason", f.Reason)
		return false, nil
	}
	log.Warn("review job ended without a result; run reported as failed", "reason", f.Reason, "step", f.Step)
	if isAPI {
		// No repository to raise a "Needs attention" item on.
		return true, nil
	}
	if strings.Contains(f.Reason, "OOMKilled") {
		e.refreshOutOfMemory(ctx, run.MR.Instance, run.ProjectPath)
	}
	if f.Step == kube.StepClone {
		e.refreshCloneFailing(ctx, run.MR.Instance, run.ProjectPath)
	}
	return true, nil
}
