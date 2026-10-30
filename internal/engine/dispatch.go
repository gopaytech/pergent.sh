package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// maxDispatchAttempts is how many times a run is tried before a GitLab or
	// Kubernetes error becomes its failure.
	maxDispatchAttempts = 5
	// dispatchBackoff is the first retry delay; it doubles each attempt.
	dispatchBackoff = 10 * time.Second
	// diffRefsDelay is how long to wait for GitLab to compute diff refs.
	diffRefsDelay = 10 * time.Second
	// tokenGrace keeps a result token valid a little past the Job deadline.
	tokenGrace = 60 * time.Second
	// settleTimeout bounds the store and cleanup calls that record a run's
	// outcome; they run even when the dispatch context is cancelled, so a
	// claimed run is never left behind half-handled.
	settleTimeout = 10 * time.Second

	reasonTooLarge     = "the skills and instructions for this repository are larger than 900 KiB"
	reasonNoCloneToken = "pergent.sh has no clone token for this project yet"
	reasonCouldntStart = "pergent.sh couldn't start the review: "
	runningDescription = "pergent review running"
)

// DispatchOnce claims as many queued runs as jobs.max_running leaves room for
// and starts a review Job for each. A run whose merge request is closed, or
// whose head moved, is superseded (a moved head gets a run of its own); a run
// that can never be reviewed is reported with a failure reason; a GitLab or
// Kubernetes error requeues it with backoff until the fifth attempt, which
// reports it. One run's trouble never stops the others. The returned error
// joins store failures only.
func (e *Engine) DispatchOnce(ctx context.Context) error {
	running, err := e.Store.CountRunning(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: count running runs: %w", err)
	}
	limit := e.Config.Jobs.MaxRunning - running
	if limit <= 0 {
		return nil
	}
	runs, err := e.Store.ClaimQueued(ctx, limit, e.now())
	if err != nil {
		return fmt.Errorf("dispatch: claim queued runs: %w", err)
	}
	var errs []error
	for _, run := range runs {
		d := &dispatcher{e: e, run: run, log: e.log().With(
			"run", run.ID, "instance", run.MR.Instance, "project", run.ProjectPath,
			"mr", run.MR.IID, "head", run.HeadSHA)}
		if err := d.dispatch(ctx); err != nil {
			errs = append(errs, fmt.Errorf("dispatch run %s: %w", run.ID, err))
		}
	}
	return errors.Join(errs...)
}

// dispatcher handles one claimed run. Its methods return only store errors;
// everything else is turned into the run's next state.
type dispatcher struct {
	e   *Engine
	run store.Run
	log *slog.Logger
	// secrets are values that must never reach a failure reason or a log.
	secrets []string
}

func (d *dispatcher) dispatch(ctx context.Context) error {
	e, run := d.e, d.run

	bots, haveBots := e.Bots[run.MR.Instance]
	repo, haveRepo := e.Config.Repository(run.MR.Instance, run.ProjectPath)
	if !haveBots || bots.Review == nil || !haveRepo {
		// The repository was switched off (or its instance removed) after
		// the run was queued: nothing is reviewed or posted there any more.
		d.log.Warn("dropping run: its repository is no longer configured")
		return d.supersede(ctx)
	}

	mr, err := bots.Review.GetMergeRequest(ctx, run.MR.ProjectID, run.MR.IID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("get merge request: %w", err))
	}
	if mr.State != "opened" {
		d.log.Info("merge request is no longer open; run superseded", "state", mr.State)
		return d.supersede(ctx)
	}
	if mr.SHA != "" && mr.SHA != run.HeadSHA {
		// Push webhooks can arrive out of order, so the newest head may have
		// no run yet: queue one.
		d.log.Info("merge request head moved; run superseded", "new_head", mr.SHA)
		if err := d.supersede(ctx); err != nil {
			return err
		}
		sctx, cancel := settleCtx(ctx)
		defer cancel()
		if _, _, err := e.Store.EnqueueRun(sctx, run.MR, run.ProjectPath, mr.SHA, mr.TargetBranch); err != nil {
			return fmt.Errorf("enqueue new head %s: %w", mr.SHA, err)
		}
		e.Wake()
		return nil
	}
	refs := mr.DiffRefs
	if mr.SHA == "" || refs.BaseSHA == "" || refs.StartSHA == "" || refs.HeadSHA != run.HeadSHA {
		// GitLab is still computing the merge request's diff.
		d.log.Info("merge request diff is not ready; retrying", "delay", diffRefsDelay)
		return d.requeue(ctx, e.now().Add(diffRefsDelay))
	}

	project, found, err := e.Store.Project(ctx, run.MR.Instance, run.MR.ProjectID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load project: %w", err))
	}
	if !found || len(project.DeployTokenSealed) == 0 {
		return d.fail(ctx, reasonNoCloneToken)
	}
	gitToken, err := e.Box.Open(project.DeployTokenSealed)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("open the clone token: %w", err))
	}
	d.secrets = append(d.secrets, string(gitToken), e.LLMKey)

	var previous []byte
	prev, havePrev, err := e.Store.PreviousResult(ctx, run.MR, run.ID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load previous result: %w", err))
	}
	if havePrev {
		if previous, err = result.Trim(prev); err != nil {
			d.log.Warn("reviewing without the previous result: it can't be read", "error", err)
			previous = nil
		}
	}

	in, err := inputs.Resolve(ctx, source{bots.Review}, e.Library, repo, run.MR.ProjectID, refs.StartSHA, previous)
	if err != nil {
		var se *sourceError
		if errors.As(err, &se) {
			return d.retry(ctx, fmt.Errorf("read review inputs: %w", err))
		}
		return d.fail(ctx, err.Error())
	}
	if err := in.Fit(inputs.ConfigMapLimit); err != nil {
		if errors.Is(err, inputs.ErrTooLarge) {
			return d.fail(ctx, reasonTooLarge)
		}
		return d.fail(ctx, err.Error())
	}

	token, tokenHash, err := mintToken()
	if err != nil {
		return d.retry(ctx, err)
	}
	d.secrets = append(d.secrets, token)

	now := e.now()
	allowance := kube.CloneAllowance(project.CloneSeconds)
	expires := now.Add(kube.Deadline(e.Config.Jobs.Timeout, allowance) + tokenGrace)
	name := kube.JobName(run.ID)
	params := kube.JobParams{
		Name:           name,
		RunID:          run.ID,
		Image:          e.Config.Jobs.Image,
		Timeout:        e.Config.Jobs.Timeout,
		CloneAllowance: allowance,
		Concurrency:    e.Config.Jobs.Concurrency,
		RepoURL:        project.HTTPURLToRepo,
		MRIID:          run.MR.IID,
		TargetBranch:   mr.TargetBranch,
		BaseSHA:        refs.BaseSHA,
		HeadSHA:        run.HeadSHA,
		GitUsername:    project.DeployTokenUsername,
		ResultURL:      strings.TrimRight(e.Config.CallbackURL, "/") + "/runs/" + run.ID + "/result",
		LLM:            e.Config.LLM,
		Resources:      e.Config.Jobs.Resources,
		Inputs:         in,
	}
	secrets := kube.Secrets{GitToken: string(gitToken), LLMKey: e.LLMKey, ResultToken: token}
	if err := e.Launcher.Launch(ctx, params, secrets); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A Job left over from an earlier attempt of this run that never
			// got recorded: remove it so the next attempt can start afresh.
			d.deleteJob(ctx, name)
		}
		return d.retry(ctx, fmt.Errorf("launch job: %w", err))
	}

	err = e.Store.MarkDispatched(ctx, run.ID, name, refs.BaseSHA, refs.StartSHA, tokenHash, expires, now)
	if errors.Is(err, store.ErrNotRunning) {
		d.log.Info("run was superseded while its job started; deleting the job", "job", name)
		d.deleteJob(ctx, name)
		return nil
	}
	if err != nil {
		// The Job's result token isn't stored, so its result would be
		// refused: stop it and try again later.
		d.deleteJob(ctx, name)
		return d.retry(ctx, fmt.Errorf("record dispatch: %w", err))
	}
	d.log.Info("review job started", "job", name)

	if err := bots.Review.SetCommitStatus(ctx, run.MR.ProjectID, run.HeadSHA, gitlab.StatusRunning, runningDescription); err != nil {
		d.log.Warn("couldn't set the running commit status", "error", d.redact(err.Error()))
	}
	return nil
}

// settleCtx outlives a cancelled dispatch context, so the outcome of a claimed
// run is still recorded during shutdown.
func settleCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
}

func (d *dispatcher) supersede(ctx context.Context) error {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if err := d.e.Store.Supersede(sctx, d.run.ID); err != nil {
		return fmt.Errorf("supersede: %w", err)
	}
	return nil
}

// fail reports the run with reason; the publisher posts it.
func (d *dispatcher) fail(ctx context.Context, reason string) error {
	reason = d.redact(reason)
	d.log.Warn("review can't start; reporting the run as failed", "reason", reason)
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if _, err := d.e.Store.ReportFailure(sctx, d.run.ID, reason, d.e.now()); err != nil {
		return fmt.Errorf("report failure: %w", err)
	}
	return nil
}

// retry requeues the run after a GitLab or Kubernetes error with backoff
// 10 s × 2^attempts, or reports it as failed on the fifth attempt.
func (d *dispatcher) retry(ctx context.Context, cause error) error {
	text := d.redact(cause.Error())
	attempt := d.run.Attempts + 1
	if attempt >= maxDispatchAttempts {
		d.log.Error("couldn't start the review; giving up", "attempt", attempt, "error", text)
		return d.fail(ctx, reasonCouldntStart+text)
	}
	notBefore := d.e.now().Add(dispatchBackoff << d.run.Attempts)
	d.log.Warn("couldn't start the review; will retry", "attempt", attempt, "retry_at", notBefore, "error", text)
	return d.requeue(ctx, notBefore)
}

// requeue puts the run back in the queue. A run that was superseded or
// reported meanwhile can't be requeued, and that is not an error.
func (d *dispatcher) requeue(ctx context.Context, notBefore time.Time) error {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	err := d.e.Store.Requeue(sctx, d.run.ID, notBefore)
	if err == nil {
		return nil
	}
	if r, ok, rerr := d.e.Store.Run(sctx, d.run.ID); rerr == nil && ok && r.State != store.Queued && r.State != store.Running {
		return nil
	}
	return fmt.Errorf("requeue: %w", err)
}

func (d *dispatcher) deleteJob(ctx context.Context, name string) {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if err := d.e.Launcher.Delete(sctx, name); err != nil {
		d.log.Warn("couldn't delete the job", "job", name, "error", d.redact(err.Error()))
	}
}

// redact removes the run's secrets from text bound for a reason or a log.
func (d *dispatcher) redact(text string) string {
	for _, s := range d.secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}

// mintToken returns a fresh result token (32 random bytes, base64url without
// padding) and its SHA-256, which is all the store keeps.
func mintToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("mint result token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// source marks the errors of the underlying Source, so a GitLab failure
// while reading inputs is retried while a bad skill file fails the run.
type source struct{ src inputs.Source }

type sourceError struct{ err error }

func (e *sourceError) Error() string { return e.err.Error() }
func (e *sourceError) Unwrap() error { return e.err }

func (s source) GetFile(ctx context.Context, projectID int64, path, ref string) ([]byte, bool, error) {
	b, ok, err := s.src.GetFile(ctx, projectID, path, ref)
	if err != nil {
		return nil, false, &sourceError{err}
	}
	return b, ok, nil
}

func (s source) ListTree(ctx context.Context, projectID int64, path, ref string) ([]gitlab.TreeEntry, error) {
	entries, err := s.src.ListTree(ctx, projectID, path, ref)
	if err != nil {
		return nil, &sourceError{err}
	}
	return entries, nil
}
