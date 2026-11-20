package engine

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The reasons a readiness scan stops before its Job. Each is the whole of the
// error scanSettings returns, so it can be stored and shown as it is.
var (
	errNoDefaultBranch = errors.New("the repository has no default branch yet")
	errNotSetUp        = errors.New("pergent.sh hasn't set this repository up yet")
	// errNoCloneToken is dispatch's reason for the same gap.
	errNoCloneToken  = errors.New(reasonNoCloneToken)
	errDefaultBranch = errors.New("couldn't read the default branch")
)

// The couldn't-check reasons of the settings items. A read the platform
// refused or failed is unknown with one of these, never passed; the platform's
// own error is only logged.
const (
	cantReadGitLabApprovals  = "the review bot can't read the approval rules here"
	cantReadGitLabProtection = "the review bot can't read branch protection here"
	noPipelineSetting        = "GitLab didn't show whether pipelines must succeed"
	cantReadGitHubRules      = "the review App can't read this branch's rules"
	cantSeeGitHubChecks      = "the review App can't see this branch's required checks"
	// needsAdminRead names the optional permission that lets the review App
	// read classic branch protection.
	needsAdminRead          = " (it needs the optional Administration: Read permission)"
	cantReadGitHubClassic   = "no ruleset requires reviews, and the review App can't read the classic branch protection" + needsAdminRead
	cantSeeGitHubChecksDark = cantSeeGitHubChecks + needsAdminRead
	// cantReadGitHubProtection: GitHub calls the branch protected, no rule
	// stops a push, and the classic protection that may can't be read.
	cantReadGitHubProtection = "the review App can't read the classic branch protection" + needsAdminRead
	// The CI access reasons, read for a repository that has ci_jobs.
	noActionsRead   = "the review App can't read Actions (it needs the Actions: Read permission)"
	noCheckRunEvent = "the review App doesn't get the Check run event (turn it on in the App's settings)"
	ciAccessUnknown = "pergent.sh hasn't checked the review App's access yet; it does at the next sync"
)

const (
	// scanLog starts a readiness scan's log lines.
	scanLog = "readiness scan: "
	// podResultVersion is the version of the readiness.PodResult a scan stores.
	podResultVersion = 1
	// gitLabDeveloper is GitLab's Developer access level.
	gitLabDeveloper = 30
)

// scanTarget is what a readiness scan reads in the pod: the default branch and
// its commit, where and how the Job clones it, and the settings items. GitToken
// is the read-only clone token (GitLab's deploy token, or a GitHub token
// minted for this one repository); it goes to the Job's clone container only
// and is never logged or stored.
type scanTarget struct {
	Branch, Commit, RepoURL, GitUser string
	GitToken                         []byte
	Pod                              readiness.PodResult
}

// scanSettings resolves the default branch of instance's repository project
// to a commit and reads the 3 settings items with the review bot (GitLab) or
// the review App (GitHub): approvals needed to merge, branch protection and
// required checks. It writes nothing. Pod is filled as far as it was read,
// also with an error, so settings read before a missing clone token are kept.
// The error is one of errNoDefaultBranch, errNotSetUp, errNoCloneToken and
// errDefaultBranch; what the platform answered is logged (redacted), never
// returned.
func (e *Engine) scanSettings(ctx context.Context, instance, project string) (scanTarget, error) {
	log := e.log().With("instance", instance, "project", project)
	p, found, err := e.Store.ProjectByPath(ctx, instance, project)
	if err != nil {
		log.Warn("readiness scan: couldn't load the project", "error", e.redact(err.Error()))
		return scanTarget{Pod: readiness.PodResult{Version: podResultVersion}}, errDefaultBranch
	}
	if !found {
		return scanTarget{Pod: readiness.PodResult{Version: podResultVersion}}, errNotSetUp
	}
	plat, ok := e.Platforms.Get(instance)
	switch {
	case ok && plat.isGitHub() && plat.GitHub != nil && plat.GitHub.App != nil:
		return e.scanGitHubSettings(ctx, instance, plat.GitHub.App, p)
	case ok && !plat.isGitHub() && plat.GitLab != nil:
		return e.scanGitLabSettings(ctx, plat.GitLab, p)
	}
	log.Info("readiness scan: pergent.sh isn't connected to the instance")
	return scanTarget{Pod: readiness.PodResult{Version: podResultVersion}}, errDefaultBranch
}

// scanGitLabSettings is scanSettings on GitLab, where the review bot is a
// Maintainer of the project.
func (e *Engine) scanGitLabSettings(ctx context.Context, gl *gitlab.Client, p store.Project) (scanTarget, error) {
	log := e.log().With("instance", p.Instance, "project", p.Path, "project_id", p.ProjectID)
	t := scanTarget{RepoURL: p.HTTPURLToRepo, GitUser: p.DeployTokenUsername,
		Pod: readiness.PodResult{Version: podResultVersion}}

	gp, commit, err := e.gitLabHead(ctx, log, scanLog, gl, p)
	if err != nil {
		return t, err
	}
	t.Branch, t.Commit = gp.DefaultBranch, commit

	n, err := gitLabRequiredApprovals(ctx, gl, p.ProjectID, t.Branch)
	if err != nil {
		log.Info("readiness scan: couldn't read the approvals needed", "error", e.redact(err.Error()))
		t.Pod.Approvals = unknownItem(cantReadGitLabApprovals)
	} else {
		t.Pod.Approvals = approvalsItem(n)
	}

	rules, err := gitLabProtection(ctx, gl, p.ProjectID, t.Branch)
	switch {
	case err != nil:
		log.Info("readiness scan: couldn't read branch protection", "error", e.redact(err.Error()))
		t.Pod.Protected = unknownItem(cantReadGitLabProtection)
	case len(rules) == 0:
		t.Pod.Protected = readiness.PodItem{State: readiness.Missing, Reason: t.Branch + " is not protected"}
	case slices.ContainsFunc(rules, developersPush):
		t.Pod.Protected = readiness.PodItem{State: readiness.Warning, Reason: "Developers can push to " + t.Branch + " directly"}
	default:
		t.Pod.Protected = readiness.PodItem{State: readiness.Passed, Reason: t.Branch + " is protected"}
	}

	switch must := gp.OnlyAllowMergeIfPipelineSucceeds; {
	case must == nil:
		t.Pod.RequiredChecks = unknownItem(noPipelineSetting)
	case *must:
		t.Pod.RequiredChecks = readiness.PodItem{State: readiness.Passed, Reason: "pipelines must succeed before merging"}
	default:
		t.Pod.RequiredChecks = readiness.PodItem{State: readiness.Warning, Reason: "pipelines don't have to succeed before merging"}
	}

	t.GitToken, err = e.gitLabCloneToken(log, scanLog, p)
	return t, err
}

// gitLabProtection returns the protected branch rules that apply to branch:
// every rule whose name matches it, its own and wildcards ("*", "main*")
// alike, since GitLab applies the most permissive of them. None means the
// branch isn't protected.
func gitLabProtection(ctx context.Context, gl *gitlab.Client, projectID int64, branch string) ([]gitlab.ProtectedBranch, error) {
	all, err := gl.ProtectedBranches(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var out []gitlab.ProtectedBranch
	for _, r := range all {
		if wildcardMatch(r.Name, branch) {
			out = append(out, r)
		}
	}
	return out, nil
}

// developersPush reports whether a protected branch rule lets Developers push.
func developersPush(r gitlab.ProtectedBranch) bool {
	return slices.ContainsFunc(r.PushAccessLevels, func(l gitlab.AccessLevel) bool { return l.AccessLevel == gitLabDeveloper })
}

// scanGitHubSettings is scanSettings on GitHub, read through the review App's
// installation (Contents and Metadata read). Classic branch protection rows
// need the optional Administration: Read; without it they say so.
func (e *Engine) scanGitHubSettings(ctx context.Context, instance string, app *github.App, p store.Project) (scanTarget, error) {
	log := e.log().With("instance", p.Instance, "project", p.Path, "project_id", p.ProjectID)
	t := scanTarget{RepoURL: p.HTTPURLToRepo, GitUser: gitHubCloneUser,
		Pod: readiness.PodResult{Version: podResultVersion}}
	if p.InstallationID == 0 {
		// The review App isn't installed there (any more).
		return t, errNotSetUp
	}
	owner, name, _ := strings.Cut(p.Path, "/")
	c := app.Client(p.InstallationID)

	defaultBranch, branch, err := e.gitHubHead(ctx, log, scanLog, c, owner, name)
	if err != nil {
		return t, err
	}
	t.Branch, t.Commit = defaultBranch, branch.Commit

	types, rulesErr := c.BranchRuleTypes(ctx, owner, name, t.Branch)
	if rulesErr != nil {
		log.Info("readiness scan: couldn't read the branch's rules", "error", e.redact(rulesErr.Error()))
	}

	switch {
	case !p.ApproveAvailable:
		// Nothing is approved here, so the count isn't what decides.
		t.Pod.Approvals = unknownItem(e.approverUnreachableNote(instance))
	default:
		n, err := gitHubRequiredApprovals(ctx, c, owner, name, t.Branch)
		switch {
		case err == nil:
			t.Pod.Approvals = approvalsItem(n)
		case errors.Is(err, errClassicProtection) && !branch.Protected:
			// No ruleset requires reviews and the branch has no classic
			// protection: none is needed, which is known.
			t.Pod.Approvals = approvalsItem(0)
			err = nil
		case errors.Is(err, errClassicProtection):
			t.Pod.Approvals = unknownItem(cantReadGitHubClassic)
		default:
			t.Pod.Approvals = unknownItem(cantReadGitHubRules)
		}
		if err != nil {
			log.Info("readiness scan: couldn't read the approvals needed", "error", e.redact(err.Error()))
		}
	}

	t.Pod.Protected = gitHubProtection(t.Branch, branch, types, rulesErr, func() (github.Protection, error) {
		cp, err := c.BranchProtection(ctx, owner, name, t.Branch)
		if err != nil {
			log.Info("readiness scan: couldn't read the classic branch protection", "error", e.redact(err.Error()))
		}
		return cp, err
	})
	t.Pod.RequiredChecks = gitHubRequiredChecks(branch, types, rulesErr)

	// The review App's CI access matters only to a repository that chose CI
	// jobs. A failed lookup of its settings leaves the items out.
	if eff, ok, err := e.settings().Get(ctx, instance, p.Path); err != nil {
		log.Info("readiness scan: couldn't read the repository's settings", "error", e.redact(err.Error()))
	} else if ok && len(eff.CIJobs.Jobs) > 0 {
		a := ciAccessItem(p.ActionsReadable, "the review App can read Actions", noActionsRead, ciAccessUnknown)
		r := ciAccessItem(p.CheckRunEvents, "the review App gets the Check run event", noCheckRunEvent, ciAccessUnknown)
		t.Pod.ActionsRead, t.Pod.CheckRunEvent = &a, &r
	}

	t.GitToken, err = e.gitHubCloneToken(ctx, log, scanLog, app, p)
	return t, err
}

// gitHubProtection: the branch is protected when something stops a direct
// push: a pull_request or update rule, or classic protection that requires
// pull requests, restricts who may push, locks the branch or requires status
// checks (those the branch shows count). GitHub's protected flag alone is
// not enough, as a classic rule may block only force pushes or deletion, so
// classic, which reads the classic rule, is called for a branch it decides.
// A rule that can't be read is unknown, never a warning or missing.
func gitHubProtection(name string, b github.Branch, types []string, rulesErr error,
	classic func() (github.Protection, error)) readiness.PodItem {
	passed := readiness.PodItem{State: readiness.Passed, Reason: name + " is protected"}
	switch {
	case slices.Contains(types, "pull_request") || slices.Contains(types, "update"):
		return passed
	case b.ProtectionShown && b.RequiredChecks > 0:
		return passed
	case !b.Protected && rulesErr != nil:
		return unknownItem(cantReadGitHubRules)
	case !b.Protected:
		return readiness.PodItem{State: readiness.Missing, Reason: name + " is not protected"}
	}
	cp, err := classic()
	switch {
	case err == nil && (cp.PullRequestReviews || cp.Restrictions || cp.Locked || cp.RequiredChecks > 0):
		return passed
	case rulesErr != nil:
		// A rule the App can't read may still stop pushes.
		return unknownItem(cantReadGitHubRules)
	case err != nil:
		return unknownItem(cantReadGitHubProtection)
	}
	return readiness.PodItem{State: readiness.Warning, Reason: name + " has protection but accepts direct pushes"}
}

// gitHubRequiredChecks: a required_status_checks rule, or required checks in
// the classic protection the branch shows, pass. None is a warning only when
// both are known to have none: the rules were read, and the branch has no
// classic protection. A protected branch that shows no required check may
// not be showing the App its real setting, so that is unknown, never a
// warning.
func gitHubRequiredChecks(b github.Branch, types []string, rulesErr error) readiness.PodItem {
	switch {
	case slices.Contains(types, "required_status_checks"):
		return readiness.PodItem{State: readiness.Passed, Reason: "a ruleset requires status checks"}
	case b.ProtectionShown && b.RequiredChecks > 0:
		reason := strconv.Itoa(b.RequiredChecks) + " status checks are required"
		if b.RequiredChecks == 1 {
			reason = "1 status check is required"
		}
		return readiness.PodItem{State: readiness.Passed, Reason: reason}
	case rulesErr != nil:
		return unknownItem(cantSeeGitHubChecks)
	case !b.Protected:
		return readiness.PodItem{State: readiness.Warning, Reason: "no status checks are required"}
	}
	// Classic protection is on, and GitHub showed no check in it.
	return unknownItem(cantSeeGitHubChecksDark)
}

// approvalsItem is the approvals row for n approvals needed: 2 or more pass.
func approvalsItem(n int) readiness.PodItem {
	if n >= 2 {
		return readiness.PodItem{State: readiness.Passed, Count: n}
	}
	return readiness.PodItem{State: readiness.Warning, Count: n}
}

func unknownItem(reason string) readiness.PodItem {
	return readiness.PodItem{State: readiness.Unknown, Reason: reason}
}

// ciAccessItem is one CI access the review App has or lacks, as the last
// reconcile stored it: nil is never checked, which is unknown and never
// passed.
func ciAccessItem(allowed *bool, passed, missing, unchecked string) readiness.PodItem {
	switch {
	case allowed == nil:
		return unknownItem(unchecked)
	case *allowed:
		return readiness.PodItem{State: readiness.Passed, Reason: passed}
	}
	return readiness.PodItem{State: readiness.Missing, Reason: missing}
}
