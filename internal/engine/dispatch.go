package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/quota"
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
	reasonNoSkills = "this repository has no skills"
	// reasonImpactReserved is why a review is refused when the repository has
	// linked repositories turned on and a skill named like their check.
	reasonImpactReserved = "the name impact is reserved for the linked repositories' check, and a skill of this repository has it: rename the skill, or turn off linked repositories here"
	reasonTooLarge       = "the skills and instructions for this repository are larger than 900 KiB"
	reasonNoCloneToken   = "pergent.sh has no clone token for this repository yet"
	// reasonNoRepository is a GitHub repository reconcile hasn't stored.
	reasonNoRepository = "pergent.sh hasn't looked this repository up on GitHub yet"
	reasonNotInstalled = "the pergent review App isn't installed on this repository"
	// gitHubCloneUser is the git username of a GitHub installation token.
	gitHubCloneUser    = "x-access-token"
	reasonNoDiff       = "GitLab hasn't computed this merge request's diff yet"
	reasonNoMergeBase  = "GitHub hasn't computed this pull request's merge base yet"
	reasonCouldntStart = "pergent.sh couldn't start the review: "
	// reasonModelUnusable reaches the comment: it names neither the model nor
	// the provider.
	reasonModelUnusable = "the model chosen for this repository can't be used: an admin can change it on the repository's Settings tab"
	runningDescription  = "pergent review running"
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
// an attempt. An API review shares the queue and the limit, and is started by
// dispatchAPI. One run's trouble never stops the others. The returned error
// joins store failures only.
func (e *Engine) DispatchOnce(ctx context.Context) error {
	if !e.registry().HasAnyModel() {
		// Nothing can be reviewed: runs stay queued, not claimed, until an
		// operator or a team adds a model.
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
	// A panel's members first: their runs passed every check already, and
	// each holds a slot as a run does (CountRunning counts both).
	members, err := e.Store.ClaimQueuedMembers(ctx, limit, e.now(), skip...)
	if err != nil {
		return fmt.Errorf("dispatch: claim queued panel members: %w", err)
	}
	var errs []error
	for _, m := range members {
		if err := e.dispatchMember(ctx, m); err != nil {
			errs = append(errs, fmt.Errorf("dispatch panel member %s: %w", m.ID, err))
		}
	}
	if limit -= len(members); limit <= 0 {
		return errors.Join(errs...)
	}
	runs, err := e.Store.ClaimQueued(ctx, limit, e.now(), skip...)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("dispatch: claim queued runs: %w", err))...)
	}
	for _, run := range runs {
		d := &dispatcher{e: e, run: run, log: e.log().With(
			"run", run.ID, "instance", run.MR.Instance, "project", run.ProjectPath,
			"mr", run.MR.IID, "head", run.HeadSHA)}
		var err error
		if run.Kind == store.KindAPI {
			err = d.dispatchAPI(ctx)
		} else {
			err = d.dispatchRun(ctx)
		}
		if err != nil {
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

// stage is what a dispatch starts: a review (today's), a panel member's Job
// or a panel's merge (Job P).
type stage int

const (
	stageReview stage = iota
	stageMember
	stageMerge
)

// dispatcher handles one claimed run, or one claimed panel member of a run
// that waits for it. Its methods return only store errors; everything else is
// turned into the run's (or the member's) next state.
type dispatcher struct {
	e   *Engine
	run store.Run
	log *slog.Logger
	// stage is what the dispatch starts. In stageMember, member is the
	// claimed member and run the run waiting for it, which is never written.
	// In stageMerge, members are the review's members and merge what Job P
	// runs with (resolveMerge).
	stage   stage
	member  store.PanelMember
	members []store.PanelMember
	merge   mergePlan
	// secrets are values that must never reach a failure reason or a log.
	secrets []string
	// keptSources are the sources of the skills a re-run of some skills keeps
	// from the earlier run.
	keptSources map[string]string
	// keptVersions are the library versions of those kept skills, as the
	// earlier run recorded them.
	keptVersions map[string]int
	// keptHashes are the content hashes of those kept skills, as the earlier
	// run recorded them.
	keptHashes map[string]string
	// partial says the run re-runs some of the skills, merged with the earlier
	// run's results for the others. rerunImpact says the linked repositories'
	// check is one of the things it re-runs. Both are set by subset.
	partial, rerunImpact bool
	// provider and model are those the run's Job reviews with, set once they
	// are resolved. Empty, the run is not compared with the last review's.
	provider, model string
	// entry is the chain's choice for the run (resolve sets it), scope the
	// chain's, attempt the run's number in its review (1 for the first).
	entry   chain.Decision
	scope   string
	attempt int
	// api is true for an API review (dispatchAPI sets it). Its failure
	// reasons reach the caller, so a retry that gives up says only that the
	// review couldn't start; the cause is logged.
	api bool
	// cis is where the CI wait's store calls go; nil is the engine's Store.
	// Tests set it.
	cis ciStore
	// rulesHash is the hash of the run's rule list, chosen built-in rules
	// and Jobs image, set once the list is built; the next push compares it
	// with its own.
	rulesHash string
	// builtInRules are the built-in rules the Job loads (nil: none, and it
	// gets PERGENT_NO_BUILT_IN_RULES); builtInNote is why the chosen ones
	// were left out, "" when they were not (builtInRulesFor).
	builtInRules []string
	builtInNote  string
	// clientModel is the model an API client chose ("" for none): its chain
	// A is that model alone (chainA).
	clientModel string
}

// dispatchRun starts the claimed merge request run: today's review, or the
// merge (Job P) of a review panel, which is a run whose review has a member
// that is not off and whose members are final (the run was released). A
// run with a live member goes back to wait for them (waitForMembers). The
// run that was split is dispatched at its split and has what it recorded
// then. A later attempt of Job P (not dispatched yet) first takes those
// values from the run it fell back from (InheritPanelSplit), since Job P
// writes nothing of them.
func (d *dispatcher) dispatchRun(ctx context.Context) error {
	ms, err := d.e.Store.PanelMembers(ctx, d.run.ID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("read the review's panel members: %w", err))
	}
	if merge, live := panelStage(ms); live {
		return d.waitForMembers(ctx)
	} else if merge {
		d.stage, d.members = stageMerge, ms
	}
	if d.stage == stageMerge && d.run.AttemptOf != nil && d.run.DispatchedAt.IsZero() {
		if err := d.e.Store.InheritPanelSplit(ctx, d.run.ID); err != nil {
			return d.retry(ctx, fmt.Errorf("take the review panel's split: %w", err))
		}
		run, found, err := d.e.Store.Run(ctx, d.run.ID)
		if err != nil {
			return d.retry(ctx, fmt.Errorf("load the run: %w", err))
		}
		if !found {
			return nil
		}
		d.run = run
	}
	return d.dispatch(ctx)
}

// panelStage reads the members ms of a claimed run's review: merge is true
// when one is not off (the run is a panel's), live when one is queued,
// running or reported (it is not final yet).
func panelStage(ms []store.PanelMember) (merge, live bool) {
	for _, m := range ms {
		switch m.State {
		case store.MemberOff:
			continue
		case store.MemberQueued, store.MemberRunning, store.MemberReported:
			live = true
		}
		merge = true
	}
	return merge, live
}

// waitForMembers puts the claimed run back to wait for its panel members,
// which are not all final: ReleasePanel queues it once they are. Only an
// older release queues such a run (its StartRun, during a rolling deploy);
// merging now would merge what is done so far, and the sweep would
// supersede the live members.
func (d *dispatcher) waitForMembers(ctx context.Context) error {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	back, err := d.e.Store.WaitForPanel(sctx, d.run.ID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("put the run back to wait for its panel members: %w", err))
	}
	if back {
		d.log.Warn("not merging: a review panel member is not final yet; the run waits for its members again")
	}
	return nil
}

// keepsRun reports whether the dispatch writes nothing to its run: a
// member's (its run waits) or a merge's (the run keeps what its split
// recorded, which both members reviewed).
func (d *dispatcher) keepsRun() bool { return d.stage != stageReview }

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
	if d.stage == stageReview {
		// A member's and Job P's run keep what the review recorded.
		d.recordChange(ctx, mr)
	}
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
		if d.isMember() {
			// The panel's run is superseded with the old head: the member
			// goes back to the queue, so it holds no slot, and the sweep
			// supersedes it.
			return d.postpone(ctx, e.now())
		}
		return nil
	}
	// A member's run passed trust, the quota hold and CI before it was split.
	if d.stage == stageReview && run.Trigger == store.TriggerPush && run.AttemptOf == nil {
		// Before any token, Job or model call. A later attempt is the same
		// review of the same head, its author checked by the first. The retries of a check that
		// fails run for trustPatience from its first failure: a push of an author the
		// repository doesn't trust waits for someone with write access to
		// start it. A run asked for (a re-run, a start) was asked for by
		// someone allowed to.
		trusted, err := authorTrust(ctx, plat.Forge, repo, run.MR.ProjectID, run.ProjectPath, mr, d.log, d.redact)
		if err != nil {
			if ctx.Err() != nil {
				// Shutting down: not a failed check.
				return d.postpone(ctx, e.now())
			}
			sctx, cancel := settleCtx(ctx)
			since, serr := e.Store.TrustCheckFailing(sctx, run.ID, e.now())
			cancel()
			if serr != nil {
				return d.retry(ctx, fmt.Errorf("record the failed trust check: %w", serr))
			}
			if e.now().Sub(since) < trustPatience {
				d.log.Info("the author's access can't be read yet; trying again", "delay", trustRetryDelay, "error", d.redact(err.Error()))
				return d.postpone(ctx, e.now().Add(trustRetryDelay))
			}
		}
		switch {
		case err != nil:
			d.log.Warn("the author's access can't be read; the run waits for someone to start it", "error", d.redact(err.Error()))
			return d.hold(ctx, plat, store.WaitCheckFailed, checkFailedDescription(plat.Forge.Mention()), false)
		case !trusted:
			return d.hold(ctx, plat, store.WaitUntrusted, waitingDescription, true)
		}
	}
	// An automatic run (a push, pergent.sh's own full re-run) on a used-up
	// monthly token quota waits. A run somebody asked for (quota_checked)
	// was checked when they asked, and goes ahead. A start is always a
	// person's request, even when the previous release queued it without
	// the flag: HoldForQuota refuses it too, so holding it would postpone it
	// and check it again, for ever.
	// The repository's quota counts every token and holds the run; the
	// instance's counts global keys only, so it just stops global entries
	// (the resolver holds when nothing else is left). A run asked for is
	// held by neither, but still runs on a team's entry when one is left
	// under a used-up instance quota: that is why it was let start (owner
	// ruling 2026-11-20). The resolver also stops entries at their model or
	// team limit, and holds when only quotas and limits are left.
	askedFor := run.QuotaChecked || run.Trigger == store.TriggerStart
	repoUsedUp, instanceUsedUp, err := quota.Check(ctx, e.Store, e.now(), run.MR.Instance, run.MR.ProjectID, repo)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("read the token quota: %w", err))
	}
	if repoUsedUp && !askedFor && d.stage == stageReview {
		return d.holdForQuota(ctx, plat, false)
	}
	// The model is picked from the chain in effect, read now, so an edit made
	// meanwhile is what runs. It falls back only on what the provider said
	// (quota, billing, auth: a mark, or an earlier attempt of this review),
	// never because an entry is missing: a 1-entry chain runs on its entry or
	// fails, never on another model the repository did not ask for.
	var dec chain.Decision
	switch d.stage {
	case stageMember:
		dec, err = d.resolveMember(ctx, repo, instanceUsedUp)
	case stageMerge:
		if e.ScanImage == "" {
			return d.fail(ctx, reasonNoPanelImage)
		}
		err = d.resolveMerge(ctx, repo, repoUsedUp, instanceUsedUp)
	default:
		dec, err = d.resolve(ctx, repo, instanceUsedUp, askedFor)
	}
	if err != nil {
		return d.retry(ctx, fmt.Errorf("read the model chain: %w", err))
	}
	switch {
	case d.stage == stageMerge:
		// Job P always starts: a role with no entry left does not judge.
	case d.isMember() && !dec.Runs():
		return d.memberCannotRun(ctx, dec)
	case dec.Wait:
		// No entry at all, or no usable one in the chain of before chains:
		// the run waits, as when the list is empty.
		e.warnEvery("no-llm", msgNoLLM)
		return d.postpone(ctx, e.now().Add(waitDelay))
	case dec.Hold:
		return d.holdForQuota(ctx, plat, dec.Limited)
	case dec.Exhausted:
		return d.failExhausted(ctx, dec)
	}
	if d.stage == stageReview && ciApplies(run, repo) && !run.CIChecked {
		// After trust and quota: a run held for either doesn't also wait for
		// CI until it is released. Each run checks CI once: a run released
		// from the wait, or started during it, is marked.
		if held, err := d.gateCI(ctx, plat, repo); held || err != nil {
			return err
		}
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
	if d.keepsRun() && run.BaseSHA != "" {
		// Both members, and Job P, review the base their run recorded at the
		// split, not the merge request's base now (GitHub's follows the base
		// branch), and their results are checked against it.
		mr.BaseSHA, mr.StartSHA = run.BaseSHA, run.StartSHA
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
	// The entry resolve picked; only its key goes in the Job.
	llm, llmKey := d.entry.Entry.LLM, d.entry.Entry.Key
	d.provider, d.model = llm.Provider, llm.Model
	d.secrets = append(d.secrets, string(gitToken), llmKey)
	if d.stage == stageMerge {
		// Job P judges with up to 2 keys. What it carries is decided as at
		// the split, by the model the run recorded then.
		d.secrets = append(d.secrets, d.merge.key(kube.PanelRoleB))
		d.provider, d.model = deref(run.Provider), deref(run.Model)
	}

	var previous []byte
	last, havePrev, err := e.Store.LastReview(ctx, run.MR, run.ID)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("load previous result: %w", err))
	}
	if havePrev {
		if previous, err = result.Trim(last.Result); err != nil {
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
		if errors.Is(err, errInputsChanged) {
			return d.fail(ctx, reasonInputsChanged)
		}
		return d.retry(ctx, err)
	}
	if len(in.Skills) == 0 {
		if d.keepsRun() {
			// The run had skills when it was split.
			return d.fail(ctx, reasonInputsChanged)
		}
		return d.skipNoSkills(ctx, plat, repo)
	}
	// The rules are read at the same ref as the repository skills, never at
	// the change's head, so a change cannot edit its own rules.
	if err := d.addRules(ctx, source{plat.Forge}, repo, mr.StartSHA, true, in); err != nil {
		var se *sourceError
		if errors.As(err, &se) {
			return d.retry(ctx, fmt.Errorf("read the rules: %w", err))
		}
		var bad *rulesProblem
		if errors.As(err, &bad) {
			return d.failRules(ctx, repo.Instance, repo.Project, bad.reason)
		}
		return d.retry(ctx, err)
	}
	if _, clash := in.Skills[impactSkill]; clash && d.mayLink(plat, repo) {
		// The name belongs to the linked repositories' check while they are
		// linked: a skill of that name is never run in its place, where its
		// result could read as the check's.
		d.log.Warn("not reviewing: a skill is named impact, which the linked repositories' check holds")
		return d.fail(ctx, reasonImpactReserved)
	}
	// Only a push can be incremental: a re-run on request is a full review,
	// and a re-run of the failed skills keeps the base run it has.
	push := run.Trigger == store.TriggerPush
	if push {
		carried, reason, err := d.planCarried(ctx, plat, repo, last, havePrev, in)
		if err != nil {
			return d.retry(ctx, err)
		}
		if len(carried) > 0 {
			if in.Carried, err = json.Marshal(carried); err != nil {
				return d.retry(ctx, fmt.Errorf("encode the carried files: %w", err))
			}
		} else {
			d.log.Info("full review", "reason", reason)
		}
	}
	// What people rejected earlier is the same for a full and an incremental
	// review, and no skill's hash counts it.
	// The linked repositories' check is told its own rejected findings, when
	// the run may have one (the plan below can still find nothing to check).
	var dismissalCounts map[string]int
	withImpact := d.mayLink(plat, repo)
	in.Dismissals, dismissalCounts = d.pastDismissals(ctx, repo, in, mr.AuthorID, withImpact)
	in.AddDescription(mr.Title, mr.Description)
	// The CI results are read now, with their text, and added before Fit,
	// which drops them first.
	if ciApplies(run, repo) && d.stage != stageMerge {
		// Job P takes no CI results (forPanel), so none are read for it.
		if err := d.addCIResults(ctx, plat, repo, in); err != nil {
			if errors.Is(err, store.ErrRunGone) {
				d.log.Debug("run superseded while its CI jobs were read")
				return nil
			}
			return d.retry(ctx, fmt.Errorf("record the CI jobs' results: %w", err))
		}
	}
	hadCI := len(in.CIResults) > 0
	hadCarried := len(in.Carried) > 0
	hadDismissals := len(in.Dismissals) > 0
	hadDescription := len(in.Description) > 0
	// The quiet line counts toward the budget, so it is added before Fit. A
	// member follows its run, which kept the value it was dispatched with.
	quiet := repo.QuietReviews
	if d.keepsRun() {
		quiet = run.Quiet
	}
	if quiet {
		in.AddQuietLine()
	}
	if err := in.Fit(inputs.ConfigMapLimit); err != nil {
		if errors.Is(err, inputs.ErrTooLarge) {
			return d.fail(ctx, reasonTooLarge)
		}
		return d.fail(ctx, err.Error())
	}
	if hadCI && in.CIResults == nil {
		d.log.Warn(ciDroppedMsg)
	}
	if hadDescription && in.Description == nil {
		d.log.Warn("reviewing without the change's title and description: the inputs are too large for them")
	}
	switch {
	case hadDismissals && in.Dismissals == nil:
		d.log.Warn("reviewing without past dismissals: the inputs are too large for them", "entries", dismissalCounts)
	case hadDismissals:
		d.log.Info("past dismissals", "entries", dismissalCounts)
	}
	if push && !d.keepsRun() {
		if err := d.setIncremental(ctx, last, in, hadCarried); err != nil {
			return d.retry(ctx, err)
		}
	}
	// Linked repositories: none (the zero plan, nothing recorded and nothing
	// added to the Job) unless the setting is on and the change touches one.
	// The rules are checked here again, whatever was checked on save.
	links := d.linkedRepos(ctx, plat, repo, project, mr, last, havePrev, in)
	if in.Linked, err = links.encode(); err != nil {
		return d.retry(ctx, err)
	}
	if len(links.Entries) == 0 && dismissalCounts[impactSkill] > 0 && in.Dismissals != nil {
		// The check does not run on this change: what people rejected in it
		// goes nowhere, and the Job is the one it would be without links.
		d.log.Info("the linked repositories' check does not run; its past dismissals are left out", "entries", dismissalCounts[impactSkill])
		in.Dismissals, _ = d.pastDismissals(ctx, repo, in, mr.AuthorID, false)
	}
	if len(links.Entries) > 0 {
		if in.Size() > inputs.ConfigMapLimit && in.CIResults != nil {
			// The CI results go first, as in Fit.
			in.CIResults = nil
			d.log.Warn(ciDroppedMsg)
		}
		if in.Size() > inputs.ConfigMapLimit {
			return d.fail(ctx, reasonTooLarge)
		}
		// Written before the Job starts, like the layout: a lost write would
		// hide what the review was started with. A member's or a merge's
		// run has them (memberInputsMatch).
		if !d.keepsRun() {
			if err := e.Store.SetRunLinks(ctx, run.ID, links.Matched); err != nil {
				return d.retry(ctx, fmt.Errorf("record the linked repositories: %w", err))
			}
		}
	} else {
		if len(run.LinkedRepos) > 0 && !d.keepsRun() {
			// An earlier attempt of this run recorded links the Job will not
			// have: the result is checked against what the Job was given.
			if err := e.Store.SetRunLinks(ctx, run.ID, nil); err != nil {
				return d.retry(ctx, fmt.Errorf("forget the linked repositories: %w", err))
			}
		}
		if d.rerunImpact {
			// Turned off, or the links gone, since the re-run was asked for:
			// the check can't be repeated, and the review that has it can only
			// be replaced by one that doesn't.
			if d.keepsRun() {
				return d.fail(ctx, reasonInputsChanged)
			}
			d.log.Warn("reviewing with all skills: the linked repositories' check can't run any more")
			if err := e.Store.MakeFullRerun(ctx, run.ID); err != nil {
				return d.retry(ctx, fmt.Errorf("record the full re-run: %w", err))
			}
			return d.postpone(ctx, e.now())
		}
		if len(in.Carried) > 0 && d.impactIsOpen(ctx, last, havePrev) {
			// The earlier review's check is open and this run will not repeat
			// it: carrying the rest over would keep the open check in the
			// result for good (result.MergeIncremental), so review everything.
			d.log.Info("full review", "reason", fullImpactOpen)
			in.Carried = nil
			if !d.keepsRun() {
				if err := e.Store.SetIncremental(ctx, run.ID, nil); err != nil {
					return d.retry(ctx, fmt.Errorf("record the review as a full one: %w", err))
				}
			}
		}
	}
	if d.keepsRun() {
		// A member, or Job P, writes nothing to its run, which was dispatched
		// with these inputs: a member that would review other ones can't be
		// merged with the other member's, and Job P merges what the members
		// reviewed.
		if !d.memberInputsMatch(in, last, links) {
			return d.fail(ctx, reasonInputsChanged)
		}
		return d.launch(ctx, plat, repo, project, mr, in, links, gitToken, gitUser)
	}
	// The run keeps the value it was dispatched with: the prompt, the threads
	// and the comment follow the run, never the repository's later setting.
	// It is written before the Job starts, not with SetRunInputs (best effort,
	// after the launch): a lost write would give a quiet prompt and a long
	// comment.
	if err := e.Store.SetRunQuiet(ctx, run.ID, repo.QuietReviews); err != nil {
		return d.retry(ctx, fmt.Errorf("record whether the review is quiet: %w", err))
	}
	// The layout is recorded the same way, for the same reason: a lost write
	// would give a default comment to a repository that asked for another.
	if err := e.Store.SetRunLayout(ctx, run.ID, repo.CommentLayout); err != nil {
		return d.retry(ctx, fmt.Errorf("record the comment layout: %w", err))
	}
	// So is the provider and model: the next push compares them with its own
	// to decide whether it can be incremental, and a lost write would carry
	// files reviewed by another model.
	if err := e.Store.SetRunModel(ctx, run.ID, d.provider, d.model); err != nil {
		return d.retry(ctx, fmt.Errorf("record the review's model: %w", err))
	}
	// And the rules hash: a lost write would make the next push carry files
	// reviewed under other rules.
	if err := e.Store.SetRunRulesHash(ctx, run.ID, d.rulesHash, d.builtInNote); err != nil {
		return d.retry(ctx, fmt.Errorf("record the review's rules: %w", err))
	}
	// And the entry: a later attempt skips it, and the footer and the
	// review page name it.
	if err := e.Store.SetRunEntry(ctx, run.ID, runEntry(d.entry)); err != nil {
		return d.retry(ctx, fmt.Errorf("record the review's model entry: %w", err))
	}
	// A repository with chain B in effect gets a review panel: the run waits
	// for its members, whose Jobs start apart. With no chain B this writes
	// nothing and the review goes on as before.
	split, err := d.panel(ctx, mr, in, repo, repoUsedUp, instanceUsedUp)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("start the review panel: %w", err))
	}
	if split {
		return nil
	}

	return d.launch(ctx, plat, repo, project, mr, in, links, gitToken, gitUser)
}

// launch starts the Job of the run, or of the member in stageMember, with
// the fitted inputs in, and records it. gitToken is the clone token (a
// GitHub one is minted here). A member's Job has its own name, result route
// and token, its own entry's key alone and --no-skill-verify; it records
// itself on the member and sets the running status only when it is the
// review's first member Job. It writes nothing to the run, with one
// exception: on GitHub that first status stores its Check Run's ref on the
// run (status_ref, best effort, in setStatus), so Job P updates the same one.
// Job P (stageMerge) is the run's own Job with its result route, and
// MarkDispatched keeps the split's base: it holds the judges' keys
// (resolveMerge), fetches the member results from the panel route, has its
// own timeout and no linked repository, records the judges before it
// starts, and sets no status (the first member set it) and no inputs.
func (d *dispatcher) launch(ctx context.Context, plat Platform, repo settings.Effective, project store.Project,
	mr forge.Change, in *inputs.Inputs, links linkPlan, gitToken []byte, gitUser string) error {
	e, run := d.e, d.run
	llm, llmKey := d.entry.Entry.LLM, d.entry.Entry.Key
	token, tokenHash, err := mintToken()
	if err != nil {
		return d.retry(ctx, err)
	}
	d.secrets = append(d.secrets, token)
	merging := d.stage == stageMerge
	if merging {
		// Job P clones the repository alone: its checkout has no linked one.
		links = linkPlan{}
	}
	if plat.isGitHub() {
		if gitToken, err = d.gitHubCloneToken(ctx, plat, project, links.RepoIDs...); err != nil {
			if github.InstallationGone(err) {
				// The installation is gone since reconcile stored it.
				return d.fail(ctx, reasonNotInstalled)
			}
			return d.retry(ctx, err)
		}
	}

	now := e.now()
	allowance := kube.CloneAllowance(project.CloneSeconds, len(links.Clones))
	timeout, budget := repo.JobTimeout, repo.JobTimeout
	if merging {
		// Job P's deadline counts the member results' fetch too.
		timeout, budget = kube.PanelTimeout, kube.PanelTimeout+kube.PanelFetchTimeout
	}
	expires := now.Add(kube.Deadline(budget, allowance) + tokenGrace)
	name, runID := kube.JobName(run.ID), run.ID
	resultURL := strings.TrimRight(e.Config.CallbackURL, "/") + "/runs/" + run.ID + "/result"
	if d.isMember() {
		// The member's Job is named and reports by the member; it belongs to
		// the review's first run.
		m := d.member
		name, runID = kube.JobName(m.ID), m.RunID
		resultURL = strings.TrimRight(e.Config.CallbackURL, "/") + "/runs/" + m.RunID + "/members/" + m.ID + "/result"
	}
	commentLimit := kube.CommentLimitGitLab
	if plat.isGitHub() {
		commentLimit = kube.CommentLimitGitHub
	}
	// The code graph: only with the repository's switch on does anything
	// read or ask for one, so with it off the Job is the Job of before. The
	// graph route takes the run's own result token, so a member reviews
	// without it.
	var graphURL, graphImage string
	if repo.GraphContext {
		e.WantGraph(repo.Instance, repo.Project)
		switch {
		case merging:
			// Job P takes no graph (forPanel).
		case d.isMember():
			d.log.Info("the code graph is left out for a panel member")
		default:
			graphURL = e.reviewGraphURL(ctx, run.ID, repo.Instance, repo.Project, mr.TargetBranch)
		}
		if graphURL != "" {
			graphImage = e.ScanImage
		}
	}
	params := kube.JobParams{
		Name:           name,
		RunID:          runID,
		Image:          e.Config.Jobs.Image,
		Timeout:        timeout,
		CloneAllowance: allowance,
		Concurrency:    e.Config.Jobs.Concurrency,
		MaxTurns:       repo.MaxTurns,
		RepoURL:        project.HTTPURLToRepo,
		ChangeRef:      plat.Forge.ChangeRef(run.MR.IID),
		TargetBranch:   mr.TargetBranch,
		BaseSHA:        mr.BaseSHA,
		HeadSHA:        run.HeadSHA,
		GitUsername:    gitUser,
		ResultURL:      resultURL,
		MaxCommentSize: commentLimit,
		LLM:            llm,
		Resources:      jobResources(e.Config.Jobs.Resources, repo.JobMemory),
		WorkSize:       e.Config.Jobs.WorkSize,
		Inputs:         in,
		LinkedClones:   links.Clones,
		CheckDeleted:   repo.CheckDeleted,
		NoBuiltInRules: len(d.builtInRules) == 0,
		BuiltInRules:   d.builtInRules,
		Instance:       run.MR.Instance,
		ProjectID:      run.MR.ProjectID,
		IID:            run.MR.IID,
		ProjectPath:    run.ProjectPath,
		MRURL:          mr.WebURL,
		GraphURL:       graphURL,
		GraphImage:     graphImage,
	}
	// The entry's key alone: a member's Job never holds the other model's.
	secrets := kube.Secrets{GitToken: string(gitToken), LLMKey: llmKey, ResultToken: token}
	if merging {
		callback := strings.TrimRight(e.Config.CallbackURL, "/") + "/runs/" + run.ID + "/panel/"
		params.PanelRole = kube.PanelRoleMerge
		params.Panel = &kube.PanelParams{AURL: callback + kube.PanelRoleA, BURL: callback + kube.PanelRoleB,
			B: d.merge.b, NoJudge: d.merge.noJudge, Image: e.ScanImage}
		// Each judge's key, and with --panel-no-judge none (BuildSecret).
		secrets.LLMKeyB = d.merge.key(kube.PanelRoleB)
		// Before the Job starts, as a run's entry: a judge's provider error
		// falls back along that role's chain from it.
		if err := d.recordJudges(ctx); err != nil {
			return d.retry(ctx, err)
		}
	}
	if d.isMember() {
		params.SkipSkillVerify = true
		params.PanelMemberID, params.PanelRole = d.member.ID, d.member.Role
		// Before the Job starts, as for a run: the other member avoids it,
		// and the role's next attempt skips it.
		if err := e.Store.SetMemberEntry(ctx, d.member.ID, runEntry(d.entry), d.provider, d.model); err != nil {
			return d.retry(ctx, fmt.Errorf("record the member's model entry: %w", err))
		}
	}
	if err := e.Launcher.Launch(ctx, params, secrets); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A Job left over from an earlier attempt of this run that never
			// got recorded: remove it so the next attempt can start afresh.
			d.deleteJob(ctx, name)
		}
		return d.retry(ctx, fmt.Errorf("launch job: %w", err))
	}

	first := true
	if d.isMember() {
		first, err = e.Store.MarkMemberDispatched(ctx, d.member.ID, name, tokenHash, expires, now)
	} else {
		err = e.Store.MarkDispatched(ctx, run.ID, name, mr.BaseSHA, mr.StartSHA, tokenHash, expires, now)
	}
	if errors.Is(err, store.ErrNotRunning) {
		d.log.Info("run was superseded while its job started; deleting the job", "job", name)
		d.deleteJob(ctx, name)
		if d.isMember() {
			// As for a moved head: back to the queue, so the member holds no
			// slot until the sweep supersedes it.
			return d.postpone(ctx, e.now())
		}
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
	e.clearAttention(ctx, repo.Instance, repo.Project, attRulesInvalid)
	if merging {
		// The running status is set (the review's first member did), and
		// the run keeps the inputs its split recorded.
		return nil
	}
	if d.isMember() {
		// The review's running status, once: the other member's Job, and
		// Job P later, find it set.
		if first {
			if err := e.setStatus(ctx, plat, run, forge.StatusRunning, d.memberRunningText(ctx)); err != nil {
				d.log.Warn("couldn't set the running commit status", "error", d.redact(err.Error()))
			}
		}
		return nil
	}
	// For the dashboard, best effort: the model, where each skill came from
	// and the library version of each library skill. The hash of each skill
	// goes along: the next push carries nothing for a skill that changed.
	sources, versions, hashes := d.runInputs(in)
	if err := e.Store.SetRunInputs(ctx, run.ID, llm.Model, repo.MaxTurns, sources, versions, hashes); err != nil {
		d.log.Info("couldn't record the review's model, step limit and skills for the dashboard", "error", d.redact(err.Error()))
	}

	if err := e.setStatus(ctx, plat, run, forge.StatusRunning, d.runningText()); err != nil {
		d.log.Warn("couldn't set the running commit status", "error", d.redact(err.Error()))
	}
	return nil
}

// runInputs is what a run records of the skills of its inputs in: where each
// came from, the library version of each library skill and the hash of each.
// A re-run of some skills records the whole review's.
func (d *dispatcher) runInputs(in *inputs.Inputs) (sources map[string]string, versions map[string]int, hashes map[string]string) {
	sources, versions = in.Sources, in.Versions
	if len(d.keptSources) > 0 {
		// A re-run of some skills shows the whole review's sources and
		// versions.
		sources = map[string]string{}
		for name, src := range d.keptSources {
			sources[name] = src
		}
		for name, src := range in.Sources {
			sources[name] = src
		}
		versions = map[string]int{}
		for name, v := range d.keptVersions {
			versions[name] = v
		}
		for name, v := range in.Versions {
			versions[name] = v
		}
	}
	hashes = skillHashes(in.Skills)
	if len(d.keptHashes) > 0 {
		hashes = map[string]string{}
		for name, h := range d.keptHashes {
			hashes[name] = h
		}
		for name, h := range skillHashes(in.Skills) {
			hashes[name] = h
		}
	}
	return sources, versions, hashes
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
	if len(run.SkillSubset) == 0 {
		return in, nil
	}
	// The linked repositories' check is part of the run, not one of its skills:
	// it has no entry in the inputs, so it is set apart from the names that
	// have.
	// A skill of the repository named impact is an ordinary skill: only
	// without one is that name the check.
	_, own := in.Skills[impactSkill]
	names, impact := run.SkillSubset, false
	if !own {
		names, impact = splitImpact(run.SkillSubset)
	}
	base, reason, err := d.keptRun(ctx, in, mr)
	if err != nil {
		return nil, err
	}
	only := in.Only(names)
	if reason == "" && len(only.Skills) == 0 {
		if carrier, ok := carrierSkill(in); ok && impact {
			// pergent takes at least one skill. The check re-runs with the
			// first of them, which is then re-run as well, and merged as a skill
			// of the subset.
			only, names = in.Only([]string{carrier}), []string{carrier}
			d.log.Info("re-running the linked repositories' check with one skill: a review needs at least one", "skill", carrier)
		} else {
			reason = "none of the skills to re-run exist any more"
		}
	}
	if reason != "" {
		if d.keepsRun() {
			return nil, errInputsChanged
		}
		d.log.Warn("reviewing with all skills: the skills to re-run can't be kept apart from the rest of the review", "reason", reason)
		if err := d.e.Store.MakeFullRerun(ctx, run.ID); err != nil {
			return nil, fmt.Errorf("record the full re-run: %w", err)
		}
		return in, nil
	}
	kept := recordedSubset(only, names, impact)
	if !slices.Equal(slices.Sorted(slices.Values(kept)), slices.Sorted(slices.Values(run.SkillSubset))) {
		var gone []string
		for _, n := range run.SkillSubset {
			if (own || n != impactSkill) && !slices.Contains(kept, n) {
				gone = append(gone, n)
			}
		}
		if d.keepsRun() {
			return nil, errInputsChanged
		}
		if len(gone) > 0 {
			d.log.Warn("re-running without skills that don't exist any more", "skills", strings.Join(gone, ","))
		}
		if err := d.e.Store.SetSkillSubset(ctx, run.ID, kept); err != nil {
			return nil, fmt.Errorf("record the skills to re-run: %w", err)
		}
	}
	d.partial, d.rerunImpact = true, impact
	d.keptSources = map[string]string{}
	for name, src := range base.SkillSources {
		if _, rerun := only.Skills[name]; !rerun {
			d.keptSources[name] = src
		}
	}
	d.keptVersions = map[string]int{}
	for name, v := range base.SkillVersions {
		if _, rerun := only.Skills[name]; !rerun {
			d.keptVersions[name] = v
		}
	}
	d.keptHashes = map[string]string{}
	for name, h := range base.SkillHashes {
		if _, rerun := only.Skills[name]; !rerun {
			d.keptHashes[name] = h
		}
	}
	return only, nil
}

// splitImpact is names without the linked repositories' check, and whether it
// was among them.
func splitImpact(names []string) (rest []string, impact bool) {
	for _, n := range names {
		if n == impactSkill {
			impact = true
			continue
		}
		rest = append(rest, n)
	}
	return rest, impact
}

// carrierSkill is the skill a re-run of the impact check alone runs with: the
// first by name.
func carrierSkill(in *inputs.Inputs) (string, bool) {
	names := slices.Sorted(maps.Keys(in.Skills))
	if len(names) == 0 {
		return "", false
	}
	return names[0], true
}

// recordedSubset is the subset a run records: the skills that are in the
// inputs, in the order they were asked for, and the impact check last when it
// is part of the run.
func recordedSubset(only *inputs.Inputs, names []string, impact bool) []string {
	var out []string
	for _, n := range names {
		if _, ok := only.Skills[n]; ok {
			out = append(out, n)
		}
	}
	if impact {
		out = append(out, impactSkill)
	}
	return out
}

// keptRun is the run whose results a re-run of some skills keeps, or the
// reason it can't: it has to be a completed review of the base and head
// commits this run reviews, by the skills in in, and the skills it keeps (all
// but the ones this run re-runs) have to be at the library version the earlier
// run used, so a review never mixes results made under old and new skill
// text. An earlier run that recorded no versions can't show that. An error is
// the store's.
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
	r, err := parsedFor(base, base.Result)
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
	if reason := skillsChanged(r, in); reason != "" {
		return store.Run{}, reason, nil
	}
	if base.SkillVersions == nil {
		return store.Run{}, "a skill changed since the earlier result", nil
	}
	rerun := map[string]bool{}
	for _, n := range run.SkillSubset {
		rerun[n] = true
	}
	for name := range in.Skills {
		if rerun[name] {
			continue
		}
		// A kept skill is at the same version, or in the repository's
		// folder both times (no version).
		cur, curOK := in.Versions[name]
		was, wasOK := base.SkillVersions[name]
		if cur != was || curOK != wasOK {
			return store.Run{}, "a skill changed since the earlier result", nil
		}
	}
	return base, "", nil
}

// skillsChanged says why the skills of the earlier result r are not the skills
// in in: the same names, each once. The impact check is not a skill of in, so
// it is left out of the comparison (it may be in r, once).
func skillsChanged(r *result.Result, in *inputs.Inputs) string {
	seen := map[string]bool{}
	skills := 0
	for _, s := range r.Skills {
		if seen[s.Name] {
			return "the earlier result has a skill more than once"
		}
		seen[s.Name] = true
		if r.IsCheck(s.Name) {
			continue
		}
		skills++
		if _, ok := in.Skills[s.Name]; !ok {
			return "the skills changed since the earlier result"
		}
	}
	if skills != len(in.Skills) {
		return "the skills changed since the earlier result"
	}
	return ""
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
// scoped to that one repository and the linked repositories in linked (all in
// the same installation), contents:read, never stored. It is added to the
// run's secrets before anything can log it.
func (d *dispatcher) gitHubCloneToken(ctx context.Context, plat Platform, project store.Project, linked ...int64) ([]byte, error) {
	if plat.GitHub == nil || plat.GitHub.App == nil {
		return nil, fmt.Errorf("no review App for instance %q", project.Instance)
	}
	tok, _, err := plat.GitHub.App.CloneToken(ctx, project.InstallationID, append([]int64{project.ProjectID}, linked...)...)
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

// supersede supersedes the run; in stageMember, the panel's run and its live
// members.
func (d *dispatcher) supersede(ctx context.Context) error {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if d.isMember() {
		if err := d.e.Store.SupersedePanel(sctx, d.run.ID, ""); err != nil {
			return fmt.Errorf("supersede the panel: %w", err)
		}
		return nil
	}
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
	if d.isMember() {
		if err := d.e.Store.SupersedePanel(sctx, d.run.ID, store.SupersededDraft); err != nil {
			return fmt.Errorf("supersede the panel: %w", err)
		}
		return nil
	}
	if err := d.e.Store.SupersedeDraft(sctx, d.run.ID); err != nil {
		return fmt.Errorf("supersede: %w", err)
	}
	return nil
}

// fail reports the run with reason; the publisher posts it. In stageMember it
// reports the member only: the run goes on waiting for its members.
func (d *dispatcher) fail(ctx context.Context, reason string) error {
	reason = d.redact(reason)
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if d.isMember() {
		d.log.Warn("the panel member can't start; reporting it as failed", "reason", reason)
		if _, err := d.e.Store.ReportMemberFailure(sctx, d.member.ID, store.Failure{Reason: reason}, d.e.now()); err != nil {
			return fmt.Errorf("report the member's failure: %w", err)
		}
		return nil
	}
	d.log.Warn("review can't start; reporting the run as failed", "reason", reason)
	if _, err := d.e.Store.ReportFailure(sctx, d.run.ID, store.Failure{Reason: reason}, d.e.now()); err != nil {
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
	attempts := d.run.Attempts
	if d.isMember() {
		attempts = d.member.Attempts
	}
	attempt := attempts + 1
	if attempt >= maxDispatchAttempts {
		d.log.Error("couldn't start the review; giving up", "attempt", attempt, "error", text)
		if d.api {
			return d.fail(ctx, reasonAPICouldntStart)
		}
		return d.fail(ctx, reasonCouldntStart+text)
	}
	notBefore := d.e.now().Add(dispatchBackoff << attempts)
	d.log.Warn("couldn't start the review; will retry", "attempt", attempt, "retry_at", notBefore, "error", text)
	return d.requeue(ctx, notBefore)
}

// requeue puts the run back in the queue. A run that was superseded or
// reported meanwhile can't be requeued, and that is not an error.
func (d *dispatcher) requeue(ctx context.Context, notBefore time.Time) error {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if d.isMember() {
		return d.requeueMember(sctx, notBefore)
	}
	err := d.e.Store.Requeue(sctx, d.run.ID, notBefore)
	if err == nil {
		return nil
	}
	if r, ok, rerr := d.e.Store.Run(sctx, d.run.ID); rerr == nil && ok && r.State != store.Queued && r.State != store.Running {
		return nil
	}
	return fmt.Errorf("requeue: %w", err)
}

// hold makes the run wait for someone with write access to start it, for
// reason, sets the pending status with description on its head and, with
// note, posts the note about an untrusted author once per merge request. A
// status or note that can't be posted is logged: the run waits either way.
func (d *dispatcher) hold(ctx context.Context, plat Platform, reason, description string, note bool) error {
	e, run := d.e, d.run
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	held, err := e.Store.HoldRun(sctx, run.ID, reason)
	if err != nil {
		return d.retry(ctx, fmt.Errorf("hold the run: %w", err))
	}
	if !held {
		// Superseded meanwhile, or started while it was being checked (it
		// is a start now): back to the queue, which a superseded run
		// doesn't go to, so the start is dispatched.
		return d.postpone(ctx, e.now())
	}
	d.log.Info("run waits for someone with write access to start it", "reason", reason)
	var n *holdNote
	if note {
		n = &holdNote{
			what:    "an untrusted author",
			text:    untrustedNote(plat.Forge.Mention()),
			claim:   func(c context.Context) (bool, error) { return e.Store.ClaimUntrustedNote(c, run.MR, e.now()) },
			release: func(c context.Context) error { return e.Store.ReleaseUntrustedNote(c, run.MR) },
		}
	}
	d.announceHold(sctx, plat, description, n)
	return nil
}

// holdNote is the note a held run's change gets once: claim says whether it is
// the first (and records it), release forgets a claim whose note wasn't
// posted. what names the reason in the log.
type holdNote struct {
	what    string
	text    string
	claim   func(context.Context) (bool, error)
	release func(context.Context) error
}

// announceHold tells people a run was held: the pending status with
// description on its head and, with note, its note if it is the first. What
// can't be set or posted is logged: the run waits either way.
func (d *dispatcher) announceHold(ctx context.Context, plat Platform, description string, note *holdNote) {
	run := d.run
	if err := d.e.setStatus(ctx, plat, run, forge.StatusPending, description); err != nil {
		d.log.Warn("the pending status can't be set", "error", d.redact(err.Error()))
	}
	if note == nil {
		return
	}
	first, err := note.claim(ctx)
	if err != nil {
		d.log.Warn("the note about "+note.what+" can't be recorded", "error", d.redact(err.Error()))
		return
	}
	if !first {
		return
	}
	if err := plat.Forge.Reply(ctx, run.MR.ProjectID, run.MR.IID, note.text); err != nil {
		d.log.Warn("the note about "+note.what+" can't be posted", "error", d.redact(err.Error()))
		if err := note.release(ctx); err != nil {
			d.log.Warn("the note about "+note.what+" can't be forgotten", "error", d.redact(err.Error()))
		}
	}
}

// postpone puts the run (in stageMember, the member) back in the queue
// without counting an attempt.
func (d *dispatcher) postpone(ctx context.Context, notBefore time.Time) error {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if d.isMember() {
		if err := d.e.Store.PostponeMember(sctx, d.member.ID, notBefore); err != nil {
			return fmt.Errorf("postpone the member: %w", err)
		}
		return nil
	}
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
		// The skills did resolve (there are none): a skill that was missing
		// is no longer the problem.
		d.e.clearAttention(sctx, repo.Instance, repo.Project, attSkillMissing)
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
