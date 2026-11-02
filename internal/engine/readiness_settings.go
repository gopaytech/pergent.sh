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
)

const (
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

	gp, err := gl.GetProject(ctx, strconv.FormatInt(p.ProjectID, 10))
	if err != nil {
		log.Info("readiness scan: couldn't read the project", "error", e.redact(err.Error()))
		return t, errDefaultBranch
	}
	if gp.DefaultBranch == "" {
		return t, errNoDefaultBranch
	}
	commit, err := gl.CommitSHA(ctx, p.ProjectID, gp.DefaultBranch)
	if errors.Is(err, gitlab.ErrNotFound) {
		return t, errNoDefaultBranch
	}
	if err != nil {
		log.Info("readiness scan: couldn't read the default branch", "branch", gp.DefaultBranch,
			"error", e.redact(err.Error()))
		return t, errDefaultBranch
	}
	t.Branch, t.Commit = gp.DefaultBranch, commit

	n, err := gitLabRequiredApprovals(ctx, gl, p.ProjectID, t.Branch)
	if err != nil {
		log.Info("readiness scan: couldn't read the approvals needed", "error", e.redact(err.Error()))
		t.Pod.Approvals = unknownItem(cantReadGitLabApprovals)
	} else {
		t.Pod.Approvals = approvalsItem(n)
	}

	pb, protected, err := gl.ProtectedBranch(ctx, p.ProjectID, t.Branch)
	switch {
	case err != nil:
		log.Info("readiness scan: couldn't read branch protection", "error", e.redact(err.Error()))
		t.Pod.Protected = unknownItem(cantReadGitLabProtection)
	case !protected:
		t.Pod.Protected = readiness.PodItem{State: readiness.Missing, Reason: t.Branch + " is not protected"}
	case slices.ContainsFunc(pb.PushAccessLevels, func(l gitlab.AccessLevel) bool { return l.AccessLevel == gitLabDeveloper }):
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

	// The clone token is dispatch's: the project's sealed deploy token.
	if len(p.DeployTokenSealed) == 0 {
		return t, errNoCloneToken
	}
	tok, err := e.Box.Open(p.DeployTokenSealed)
	if err != nil {
		// Sealed with another key: reconcile replaces it.
		log.Warn("readiness scan: the stored deploy token can't be decrypted", "error", e.redact(err.Error()))
		return t, errNoCloneToken
	}
	t.GitToken = tok
	return t, nil
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

	repo, err := c.Repo(ctx, owner, name)
	if err != nil {
		log.Info("readiness scan: couldn't read the repository", "error", e.redact(err.Error()))
		return t, errDefaultBranch
	}
	if repo.DefaultBranch == "" {
		return t, errNoDefaultBranch
	}
	branch, err := c.Branch(ctx, owner, name, repo.DefaultBranch)
	if errors.Is(err, github.ErrNotFound) {
		// A repository with no commits names a default branch that isn't there.
		return t, errNoDefaultBranch
	}
	if err == nil && branch.Commit == "" {
		err = errors.New("GitHub returned no commit")
	}
	if err != nil {
		log.Info("readiness scan: couldn't read the default branch", "branch", repo.DefaultBranch,
			"error", e.redact(err.Error()))
		return t, errDefaultBranch
	}
	t.Branch, t.Commit = repo.DefaultBranch, branch.Commit

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
		case errors.Is(err, errClassicProtection):
			t.Pod.Approvals = unknownItem(cantReadGitHubClassic)
		default:
			t.Pod.Approvals = unknownItem(cantReadGitHubRules)
		}
		if err != nil {
			log.Info("readiness scan: couldn't read the approvals needed", "error", e.redact(err.Error()))
		}
	}

	t.Pod.Protected = gitHubProtection(t.Branch, branch, types, rulesErr)
	t.Pod.RequiredChecks = gitHubRequiredChecks(branch, types, rulesErr)

	// The clone token is minted as dispatch does: this one repository,
	// contents:read, never stored.
	tok, _, err := app.CloneToken(ctx, p.InstallationID, p.ProjectID)
	if err != nil {
		log.Warn("readiness scan: couldn't mint the clone token", "error", e.redact(err.Error()))
		return t, errNoCloneToken
	}
	t.GitToken = []byte(tok)
	return t, nil
}

// gitHubProtection: a pull_request or update rule, or classic protection,
// protects the branch. Without either it is missing, unless the rules
// couldn't be read.
func gitHubProtection(name string, b github.Branch, types []string, rulesErr error) readiness.PodItem {
	switch {
	case slices.Contains(types, "pull_request") || slices.Contains(types, "update") || b.Protected:
		return readiness.PodItem{State: readiness.Passed, Reason: name + " is protected"}
	case rulesErr != nil:
		return unknownItem(cantReadGitHubRules)
	}
	return readiness.PodItem{State: readiness.Missing, Reason: name + " is not protected"}
}

// gitHubRequiredChecks: a required_status_checks rule, or required checks in
// the classic protection the branch shows, pass. None is a warning only when
// both are known to have none: the rules were read, and the branch has no
// classic protection or shows that it requires no check.
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
	case !b.Protected || b.ProtectionShown:
		return readiness.PodItem{State: readiness.Warning, Reason: "no status checks are required"}
	}
	// Classic protection is on but GitHub didn't show it.
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
