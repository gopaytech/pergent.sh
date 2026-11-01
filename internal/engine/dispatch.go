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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// maxDispatchAttempts is how many times a run is tried before a GitLab or
	// Kubernetes error becomes its failure.
	maxDispatchAttempts = 5
	// dispatchBackoff is the first retry delay; it doubles each attempt.
	dispatchBackoff = 10 * time.Second
	// waitDelay is how long a run that has to wait, for its instance to be
	// connected or for an LLM to be set, waits before it is looked at again.
	waitDelay = 30 * time.Second
	// diffRefsDelay is how long to wait for GitLab to compute diff refs.
	diffRefsDelay = 10 * time.Second
	// diffRefsPatience is how long after a run is queued GitLab may take to
	// compute the merge request's diff before the run fails.
	diffRefsPatience = 10 * time.Minute
	// tokenGrace keeps a result token valid a little past the Job deadline.
	tokenGrace = 60 * time.Second

	// msgNoLLM is logged while no LLM is set and runs wait for one.
	unconnectedMsg = "queued reviews wait: pergent.sh isn't connected to their instance; an operator fixes it on Admin → Integrations"
	msgNoLLM       = "no LLM is set: an operator sets it on Admin → Settings"

	// reasonNoSkills is why a run is skipped and what "Needs attention" says.
	reasonNoSkills     = "this repository has no skills"
	reasonTooLarge     = "the skills and instructions for this repository are larger than 900 KiB"
	reasonNoCloneToken = "pergent.sh has no clone token for this project yet"
	// reasonNoRepository is a GitHub repository reconcile hasn't stored.
	reasonNoRepository = "pergent.sh hasn't looked this repository up on GitHub yet"
	reasonNotInstalled = "the pergent review App isn't installed on this repository"
	// gitHubCloneUser is the git username of a GitHub installation token.
	gitHubCloneUser    = "x-access-token"
	reasonNoDiff       = "GitLab hasn't computed this merge request's diff yet"
	reasonNoMergeBase  = "GitHub hasn't computed this pull request's merge base yet"
	reasonCouldntStart = "pergent.sh couldn't start the review: "
	runningDescription = "pergent review running"
)

// settleTimeout bounds the store and cleanup calls that record a run's
// outcome; they run even when the dispatch context is cancelled, so a
// claimed run is never left behind half-handled. It is a variable so tests
// can shorten it.
var settleTimeout = 10 * time.Second

// DispatchOnce claims as many queued runs as jobs.max_running leaves room for
// and starts a review Job for each. A run whose merge request is closed, or
// whose head or target branch moved, is superseded (a moved head or target
// gets a run of its own); a run that can never be reviewed is reported with a
// failure reason; a GitLab or Kubernetes error requeues it with backoff until
// the fifth attempt, which reports it. Waiting for GitLab to compute the diff,
// and a pass cut short by a cancelled ctx, postpone the run without counting
// an attempt. One run's trouble never stops the others. The returned error
// joins store failures only.
func (e *Engine) DispatchOnce(ctx context.Context) error {
	if _, _, _, ok := e.registry().LLM(); !ok {
		// Nothing can be reviewed: runs stay queued, not claimed, until an
		// operator sets the LLM.
		e.warnEvery("no-llm", msgNoLLM)
		return nil
	}
	running, err := e.Store.CountRunning(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: count running runs: %w", err)
	}
	limit := e.Config.Jobs.MaxRunning - running
	if limit <= 0 {
		return nil
	}
	skip := e.unconnected()
	if len(skip) > 0 {
		e.warnUnconnected(ctx, skip)
	}
	runs, err := e.Store.ClaimQueued(ctx, limit, e.now(), skip...)
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

// unconnected returns the registered instances (not being removed) that
// pergent.sh isn't connected to, whose runs wait: they are not claimed, so
// however many wait, they take no slot from the other instances.
func (e *Engine) unconnected() []string {
	var out []string
	for _, inst := range e.registry().All() {
		if inst.Removing {
			continue
		}
		if plat, ok := e.Platforms.Get(inst.Name); !ok || plat.Forge == nil {
			out = append(out, inst.Name)
		}
	}
	return out
}

// warnUnconnected logs, at most once a minute per instance, that the queued
// runs of the instances in names wait for a connection. Instances with none
// queued are not mentioned.
func (e *Engine) warnUnconnected(ctx context.Context, names []string) {
	queued, err := e.Store.CountQueued(ctx, names)
	if err != nil {
		e.log().Warn("couldn't count the queued runs of unconnected instances", "error", e.redact(err.Error()))
		return
	}
	for _, name := range names {
		if queued[name] > 0 {
			e.warnEvery("unconnected/"+name, unconnectedMsg, "instance", name, "queued_runs", queued[name])
		}
	}
}

// dispatcher handles one claimed run. Its methods return only store errors;
// everything else is turned into the run's next state.
type dispatcher struct {
	e   *Engine
	run store.Run
	log *slog.Logger
	// secrets are values that must never reach a failure reason or a log.
	secrets []string
	// keptSources are the sources of the skills a re-run of some skills keeps
	// from the earlier run.
	keptSources map[string]string
}

func (d *dispatcher) dispatch(ctx context.Context) error {
	e, run := d.e, d.run

	plat, havePlat := e.Platforms.Get(run.MR.Instance)
	repo, haveRepo, err := e.settings().Get(ctx, run.MR.Instance, run.ProjectPath)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load repository settings: %w", err))
	}
	inst, registered := e.registry().Get(run.MR.Instance)
	if !haveRepo || !registered || inst.Removing {
		// The repository was switched off (or its instance removed) after
		// the run was queued: nothing is reviewed or posted there any more.
		d.log.Warn("dropping run: its repository is no longer configured")
		return d.supersede(ctx)
	}
	if !havePlat || plat.Forge == nil {
		// The instance exists but pergent.sh isn't connected to it (it was
		// unreachable at startup, and an operator fixes that in the
		// dashboard): the run waits, as a review of a platform that is down
		// does, and goes on once the instance is connected.
		e.warnEvery("unconnected/"+run.MR.Instance, unconnectedMsg, "instance", run.MR.Instance)
		return d.postpone(ctx, e.now().Add(waitDelay))
	}
	if plat.isGitHub() {
		// The review App reads the pull request through its installation
		// on the repository: without one nothing can be read.
		project, found, err := e.Store.Project(ctx, run.MR.Instance, run.MR.ProjectID)
		if err != nil {
			return d.retry(ctx, fmt.Errorf("load project: %w", err))
		}
		if !found {
			return d.fail(ctx, reasonNoRepository)
		}
		if project.InstallationID == 0 {
			return d.fail(ctx, reasonNotInstalled)
		}
	}

	mr, err := plat.Forge.GetChange(ctx, run.MR.ProjectID, run.MR.IID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("get merge request: %w", err))
	}
	d.recordChange(ctx, mr)
	if mr.State != forge.StateOpen {
		d.log.Info("merge request is no longer open; run superseded", "state", mr.State)
		return d.supersede(ctx)
	}
	if mr.Draft && !repo.ReviewDrafts {
		// The webhook skips drafts of such a repository, so this run was
		// queued before the merge request became a draft or the setting
		// changed. Nothing is posted: the review starts when the merge
		// request is marked ready. This comes before the moved-head check,
		// so a newer head of a draft isn't queued either.
		d.log.Info("merge request is a draft and the repository doesn't review drafts; run superseded")
		return d.supersedeDraft(ctx)
	}
	if mr.HeadSHA != "" && (mr.HeadSHA != run.HeadSHA || mr.TargetBranch != run.TargetBranch) {
		// Push webhooks can arrive out of order, and a retarget's may not
		// have arrived yet, so the newest head and target may have no run:
		// queue one. EnqueueRun supersedes this run (and any other live run
		// of another head or target) in the same transaction.
		d.log.Info("merge request head or target branch moved; run superseded",
			"new_head", mr.HeadSHA, "new_target", mr.TargetBranch, "run_target", run.TargetBranch)
		sctx, cancel := settleCtx(ctx)
		defer cancel()
		if _, _, err := e.Store.EnqueueRun(sctx, run.MR, run.ProjectPath, mr.HeadSHA, mr.TargetBranch); err != nil {
			return fmt.Errorf("enqueue new head %s: %w", mr.HeadSHA, err)
		}
		e.Wake()
		return nil
	}
	if mr.HeadSHA == "" || mr.BaseSHA == "" || mr.StartSHA == "" {
		// GitLab is still computing the merge request's diff (for this
		// head: the forge leaves BaseSHA and StartSHA empty until then).
		// Waiting is not a failed attempt, but it can't go on for ever.
		if e.now().Sub(run.CreatedAt) > diffRefsPatience {
			if plat.isGitHub() {
				return d.fail(ctx, reasonNoMergeBase)
			}
			return d.fail(ctx, reasonNoDiff)
		}
		d.log.Info("merge request diff is not ready; waiting", "delay", diffRefsDelay)
		return d.postpone(ctx, e.now().Add(diffRefsDelay))
	}

	project, found, err := e.Store.Project(ctx, run.MR.Instance, run.MR.ProjectID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load project: %w", err))
	}
	// A GitLab project's clone token is its sealed deploy token; a GitHub
	// repository's is minted for the Job right before it starts.
	var gitToken []byte
	gitUser := project.DeployTokenUsername
	switch {
	case plat.isGitHub():
		if !found || project.InstallationID == 0 {
			return d.fail(ctx, reasonNotInstalled)
		}
		gitUser = gitHubCloneUser
	case !found || len(project.DeployTokenSealed) == 0:
		return d.fail(ctx, reasonNoCloneToken)
	default:
		if gitToken, err = e.Box.Open(project.DeployTokenSealed); err != nil {
			return d.retry(ctx, fmt.Errorf("open the clone token: %w", err))
		}
	}
	llm, llmKey, _, ok := e.registry().LLM()
	if !ok {
		// Set to nothing since the pass began: the run waits.
		d.log.Warn(msgNoLLM)
		return d.postpone(ctx, e.now().Add(waitDelay))
	}
	d.secrets = append(d.secrets, string(gitToken), llmKey)

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

	// The skills are read now, not when the run was queued: an edit made
	// meanwhile is what reviews. A repository that names none uses the
	// default list; one that names a skill the library lacks fails, with no
	// fallback to other skills.
	resolved, err := e.Store.ResolveLibrary(ctx, repo.Skills)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("read the skill library: %w", err))
	}
	if len(resolved.Missing) > 0 {
		return d.failSkills(ctx, repo, fmt.Sprintf("skill %q is not in the skill library", resolved.Missing[0]))
	}
	lib := make(map[string]inputs.LibraryEntry, len(resolved.Skills))
	for name, sk := range resolved.Skills {
		lib[name] = inputs.LibraryEntry{Content: []byte(sk.Content), Version: sk.Version}
	}
	in, err := inputs.ResolveSkills(ctx, source{plat.Forge}, lib, repo.RepoSkills,
		run.MR.ProjectID, mr.StartSHA, previous)
	if err != nil {
		var se *sourceError
		var nf *inputs.NotFoundError
		switch {
		case errors.As(err, &se):
			return d.retry(ctx, fmt.Errorf("read review inputs: %w", err))
		case errors.As(err, &nf):
			return d.failSkills(ctx, repo, err.Error())
		}
		return d.fail(ctx, err.Error())
	}
	if in, err = d.subset(ctx, in, mr); err != nil {
		return d.retry(ctx, err)
	}
	if len(in.Skills) == 0 {
		return d.skipNoSkills(ctx, plat, repo)
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
	if plat.isGitHub() {
		if gitToken, err = d.gitHubCloneToken(ctx, plat, project); err != nil {
			if github.InstallationGone(err) {
				// The installation is gone since reconcile stored it.
				return d.fail(ctx, reasonNotInstalled)
			}
			return d.retry(ctx, err)
		}
	}

	now := e.now()
	allowance := kube.CloneAllowance(project.CloneSeconds)
	expires := now.Add(kube.Deadline(repo.JobTimeout, allowance) + tokenGrace)
	name := kube.JobName(run.ID)
	commentLimit := kube.CommentLimitGitLab
	if plat.isGitHub() {
		commentLimit = kube.CommentLimitGitHub
	}
	params := kube.JobParams{
		Name:           name,
		RunID:          run.ID,
		Image:          e.Config.Jobs.Image,
		Timeout:        repo.JobTimeout,
		CloneAllowance: allowance,
		Concurrency:    e.Config.Jobs.Concurrency,
		RepoURL:        project.HTTPURLToRepo,
		ChangeRef:      plat.Forge.ChangeRef(run.MR.IID),
		TargetBranch:   mr.TargetBranch,
		BaseSHA:        mr.BaseSHA,
		HeadSHA:        run.HeadSHA,
		GitUsername:    gitUser,
		ResultURL:      strings.TrimRight(e.Config.CallbackURL, "/") + "/runs/" + run.ID + "/result",
		MaxCommentSize: commentLimit,
		LLM:            llm,
		Resources:      jobResources(e.Config.Jobs.Resources, repo.JobMemory),
		Inputs:         in,
	}
	secrets := kube.Secrets{GitToken: string(gitToken), LLMKey: llmKey, ResultToken: token}
	if err := e.Launcher.Launch(ctx, params, secrets); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A Job left over from an earlier attempt of this run that never
			// got recorded: remove it so the next attempt can start afresh.
			d.deleteJob(ctx, name)
		}
		return d.retry(ctx, fmt.Errorf("launch job: %w", err))
	}

	err = e.Store.MarkDispatched(ctx, run.ID, name, mr.BaseSHA, mr.StartSHA, tokenHash, expires, now)
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
	// A Job started: the repository has skills and every one is there.
	e.clearAttention(ctx, repo.Instance, repo.Project, attSkillMissing)
	e.clearAttention(ctx, repo.Instance, repo.Project, attNoSkills)
	// For the dashboard, best effort: the model and where each skill came
	// from.
	sources := in.Sources
	if len(d.keptSources) > 0 {
		// A re-run of some skills shows the whole review's sources.
		sources = map[string]string{}
		for name, src := range d.keptSources {
			sources[name] = src
		}
		for name, src := range in.Sources {
			sources[name] = src
		}
	}
	if err := e.Store.SetRunInputs(ctx, run.ID, llm.Model, sources, nil); err != nil {
		d.log.Info("couldn't record the review's model and skills for the dashboard", "error", d.redact(err.Error()))
	}

	if err := plat.Forge.SetStatus(ctx, run.MR.ProjectID, run.HeadSHA, forge.StatusRunning, runningDescription); err != nil {
		d.log.Warn("couldn't set the running commit status", "error", d.redact(err.Error()))
	}
	return nil
}

// subset reduces the inputs of a run that reviews some of the skills (a
// re-run of the failed ones) to those skills. This happens after the inputs
// are resolved, as a repository's own skills replace library ones inside
// Resolve.
//
// A review of some skills is only ever published merged with the earlier
// results of the others, and only a subset of the same diff by the same skills
// can be. So when the earlier run's result can't be read, is not a completed
// review of the commits this run reviews, or was made by other skills than are
// configured now, the run is turned into a full re-run (recorded on the run)
// and reviews every skill. A skill asked for that is gone is left out of the
// subset, which is recorded.
func (d *dispatcher) subset(ctx context.Context, in *inputs.Inputs, mr forge.Change) (*inputs.Inputs, error) {
	run := d.run
	names := run.SkillSubset
	if len(names) == 0 {
		return in, nil
	}
	base, reason, err := d.keptRun(ctx, in, mr)
	if err != nil {
		return nil, err
	}
	only := in.Only(names)
	if reason == "" && len(only.Skills) == 0 {
		reason = "none of the skills to re-run exist any more"
	}
	if reason != "" {
		d.log.Warn("reviewing with all skills: the skills to re-run can't be kept apart from the rest of the review", "reason", reason)
		if err := d.e.Store.MakeFullRerun(ctx, run.ID); err != nil {
			return nil, fmt.Errorf("record the full re-run: %w", err)
		}
		return in, nil
	}
	if len(only.Skills) < len(names) {
		var kept, gone []string
		for _, n := range names {
			if _, ok := only.Skills[n]; ok {
				kept = append(kept, n)
			} else {
				gone = append(gone, n)
			}
		}
		d.log.Warn("re-running without skills that don't exist any more", "skills", strings.Join(gone, ","))
		if err := d.e.Store.SetSkillSubset(ctx, run.ID, kept); err != nil {
			return nil, fmt.Errorf("record the skills to re-run: %w", err)
		}
	}
	d.keptSources = map[string]string{}
	for name, src := range base.SkillSources {
		if _, rerun := only.Skills[name]; !rerun {
			d.keptSources[name] = src
		}
	}
	return only, nil
}

// keptRun is the run whose results a re-run of some skills keeps, or the
// reason it can't: it has to be a completed review of the base and head
// commits this run reviews, by the skills in in. An error is the store's.
func (d *dispatcher) keptRun(ctx context.Context, in *inputs.Inputs, mr forge.Change) (store.Run, string, error) {
	run := d.run
	if run.BaseRunID == nil {
		return store.Run{}, "no earlier run to keep the other skills' results from", nil
	}
	base, found, err := d.e.Store.Run(ctx, *run.BaseRunID)
	if err != nil {
		return store.Run{}, "", fmt.Errorf("load the earlier run: %w", err)
	}
	if !found || base.Result == nil {
		return store.Run{}, "the earlier run or its result is gone", nil
	}
	r, err := result.Parse(base.Result)
	switch {
	case err != nil:
		return store.Run{}, "the earlier result can't be read", nil
	case r.Status != "completed":
		return store.Run{}, "the earlier result is not a completed review", nil
	case r.Base != mr.BaseSHA || r.Head != run.HeadSHA:
		return store.Run{}, "the earlier result is of other commits", nil
	}
	// The same skills by name, each once: the earlier skills are kept by
	// name, so a repeated name would stand in for one that is missing.
	seen := map[string]bool{}
	for _, s := range r.Skills {
		if seen[s.Name] {
			return store.Run{}, "the earlier result has a skill more than once", nil
		}
		seen[s.Name] = true
		if _, ok := in.Skills[s.Name]; !ok {
			return store.Run{}, "the skills changed since the earlier result", nil
		}
	}
	if len(seen) != len(in.Skills) {
		return store.Run{}, "the skills changed since the earlier result", nil
	}
	return base, "", nil
}

// recordChange stores the merge request's title, web address, source branch
// and author on the run for the dashboard, when they changed. It is best
// effort: a store error is logged and the dispatch goes on.
func (d *dispatcher) recordChange(ctx context.Context, mr forge.Change) {
	cd := store.ChangeDetails{Title: mr.Title, WebURL: mr.WebURL, SourceBranch: mr.SourceBranch, Author: mr.Author}
	run := d.run
	if cd == (store.ChangeDetails{Title: run.Title, WebURL: run.WebURL, SourceBranch: run.SourceBranch, Author: run.Author}) {
		return
	}
	if err := d.e.Store.SetRunChange(ctx, run.ID, cd); err != nil {
		d.log.Info("couldn't record the merge request's details for the dashboard", "error", d.redact(err.Error()))
	}
}

// jobResources is jobs.resources with the memory limit set to memory, the
// repository's effective one; a zero memory leaves the limits as they are.
// The config's own lists are never changed.
func jobResources(base corev1.ResourceRequirements, memory resource.Quantity) corev1.ResourceRequirements {
	res := *base.DeepCopy()
	if memory.IsZero() {
		return res
	}
	if res.Limits == nil {
		res.Limits = corev1.ResourceList{}
	}
	res.Limits[corev1.ResourceMemory] = memory.DeepCopy()
	return res
}

// gitHubCloneToken mints the token the Job clones repository project with:
// scoped to that one repository, contents:read, never stored. It is added to
// the run's secrets before anything can log it.
func (d *dispatcher) gitHubCloneToken(ctx context.Context, plat Platform, project store.Project) ([]byte, error) {
	if plat.GitHub == nil || plat.GitHub.App == nil {
		return nil, fmt.Errorf("no review App for instance %q", project.Instance)
	}
	tok, _, err := plat.GitHub.App.CloneToken(ctx, project.InstallationID, project.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("mint the clone token: %w", err)
	}
	d.secrets = append(d.secrets, tok)
	return []byte(tok), nil
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

// supersedeDraft supersedes the run of a draft the repository doesn't
// review, with that as its reason.
func (d *dispatcher) supersedeDraft(ctx context.Context) error {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if err := d.e.Store.SupersedeDraft(sctx, d.run.ID); err != nil {
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
// 10 s × 2^attempts, or reports it as failed on the fifth attempt. When the
// dispatch context was cancelled (shutdown), the error is ours, not GitLab's
// or Kubernetes': the run is postponed to now and no attempt is counted.
func (d *dispatcher) retry(ctx context.Context, cause error) error {
	text := d.redact(cause.Error())
	if ctx.Err() != nil {
		d.log.Info("dispatch interrupted; the run goes back to the queue", "error", text)
		return d.postpone(ctx, d.e.now())
	}
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

// postpone puts the run back in the queue without counting an attempt.
func (d *dispatcher) postpone(ctx context.Context, notBefore time.Time) error {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if err := d.e.Store.Postpone(sctx, d.run.ID, notBefore); err != nil {
		return fmt.Errorf("postpone: %w", err)
	}
	return nil
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

// failSkills reports a run that can't review because of its skills: a named
// skill is gone. The reason is also listed under "Needs attention" until a
// run of the repository starts a Job.
func (d *dispatcher) failSkills(ctx context.Context, repo settings.Effective, reason string) error {
	d.e.setAttention(ctx, repo.Instance, repo.Project, attSkillMissing, reason)
	return d.fail(ctx, reason)
}

// skipNoSkills ends a run that has no skill to review with as skipped: no Job
// runs and nothing is posted, no comment and no status, so a required pergent
// status keeps the change blocked. A skipped review decided nothing, so an
// approval of this very head is withdrawn (one of an older head is stale, and
// the cleanup withdraws it). The withdrawal is recorded first: when that
// fails the run stays claimed and is tried again, rather than skipped with
// the approval left standing.
func (d *dispatcher) skipNoSkills(ctx context.Context, plat Platform, repo settings.Effective) error {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	d.log.Warn("skipping the review: " + reasonNoSkills)
	if err := d.e.withdrawRedecided(sctx, plat, d.run, d.log); err != nil {
		return err
	}
	ok, err := d.e.Store.SkipRun(sctx, d.run.ID, reasonNoSkills, d.e.now())
	if err != nil {
		return fmt.Errorf("skip run: %w", err)
	}
	if ok {
		d.e.setAttention(sctx, repo.Instance, repo.Project, attNoSkills, reasonNoSkills)
	}
	return nil
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

func (s source) ListTree(ctx context.Context, projectID int64, path, ref string) ([]forge.TreeEntry, error) {
	entries, err := s.src.ListTree(ctx, projectID, path, ref)
	if err != nil {
		return nil, &sourceError{err}
	}
	return entries, nil
}
