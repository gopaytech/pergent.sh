package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// deployTokenName is the name of the deploy tokens pergent.sh creates.
	deployTokenName = "pergent-sh"
	// deployTokenLife is how long a new deploy token is valid.
	deployTokenLife = 30 * 24 * time.Hour
	// deployTokenRenew is how close to expiry a deploy token is replaced.
	deployTokenRenew = 7 * 24 * time.Hour
)

// HookURL is where the webhook of instance's projects points.
func HookURL(publicURL, instance string) string {
	return strings.TrimRight(publicURL, "/") + "/webhooks/gitlab/" + instance
}

// Reconcile makes the platforms match the repositories switched on, in the
// config or the dashboard (settings.Resolver). First, a dashboard record of a
// repository the config lists is deleted: the config has taken it over. For
// each GitLab repository switched on it stores the project, makes sure its merge request webhook
// exists with the instance's secret, and creates a clone token when there is
// none, the one it has expires within 7 days, or the stored one can't be
// decrypted (the secret key changed). The new token is stored before the old
// one is revoked, so a dispatch never finds none. For each configured GitHub
// repository it stores the review App's installation, the repository ID and
// clone URL, and whether the approve side reaches it (see
// reconcileGitHubRepo). For each stored GitLab project no longer switched on
// it deletes the webhook and the deploy token, then the row; a GitHub
// repository has nothing to delete but its row. One repository's failure is
// logged and the rest continue; the returned error joins them all. When the
// dashboard's repositories can't be read, nothing is removed. Tokens and keys
// are never logged.
func (e *Engine) Reconcile(ctx context.Context) error {
	return e.reconcile(ctx, nil)
}

// RepoKey names a repository: its instance and its path (any case).
type RepoKey struct{ Instance, Project string }

// ReconcileRepos is Reconcile for the repositories keys names alone: each
// one switched on is set up, and the stored project of each one no longer
// switched on is removed, as Reconcile does. No other repository's platform
// is asked anything. The dashboard's switches use it (ReconcileSoon).
func (e *Engine) ReconcileRepos(ctx context.Context, keys []RepoKey) error {
	only := make(map[repoKey]bool, len(keys))
	for _, k := range keys {
		only[keyOf(k.Instance, k.Project)] = true
	}
	return e.reconcile(ctx, only)
}

// reconcile is Reconcile over every repository (only nil) or those in only.
func (e *Engine) reconcile(ctx context.Context, only map[repoKey]bool) error {
	now := e.now()
	var errs []error
	fail := func(err error, args ...any) {
		e.log().Error("reconcile failed", append(args, "error", e.redact(err.Error()))...)
		errs = append(errs, err)
	}

	resolver := e.settings()
	if err := e.dropTakenOver(ctx, resolver); err != nil {
		fail(err)
	}
	repos, err := resolver.List(ctx)
	if err != nil {
		// Without the dashboard's repositories every one of them would
		// look switched off: remove nothing until they can be read.
		return errors.Join(append(errs, fmt.Errorf("reconcile: %w", err))...)
	}

	// The project id each repository switched on resolved to on this pass.
	wanted := make(map[repoKey]bool, len(repos))
	resolved := map[repoKey]int64{}
	for _, repo := range repos {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		if only != nil && !only[keyOf(repo.Instance, repo.Project)] {
			continue
		}
		wanted[keyOf(repo.Instance, repo.Project)] = true
		e.refreshOutOfMemory(ctx, repo.Instance, repo.Project)
		var id int64
		if repo.Platform == config.PlatformGitHub {
			id, err = e.reconcileGitHubRepo(ctx, repo)
		} else {
			id, err = e.reconcileRepo(ctx, repo, now)
		}
		if err != nil {
			fail(fmt.Errorf("reconcile %s on %s: %w", repo.Project, repo.Instance, err),
				"instance", repo.Instance, "project", repo.Project)
			continue
		}
		resolved[keyOf(repo.Instance, repo.Project)] = id
	}

	stored, err := e.Store.Projects(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("reconcile: list projects: %w", err))...)
	}
	for _, p := range stored {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		if only != nil && !only[keyOf(p.Instance, p.Path)] {
			continue
		}
		if keep(p, wanted, resolved) {
			continue
		}
		if err := e.removeProject(ctx, p); err != nil {
			fail(fmt.Errorf("remove %s (project %d) on %s: %w", p.Path, p.ProjectID, p.Instance, err),
				"instance", p.Instance, "project", p.Path, "project_id", p.ProjectID)
			continue
		}
		// Switched off: its setup problems go too. (A project row dropped
		// because its path now resolves to another project belongs to a
		// repository still wanted, whose items stay.)
		if !wanted[keyOf(p.Instance, p.Path)] {
			e.dropAttention(ctx, p.Instance, p.Path)
		}
	}
	return errors.Join(errs...)
}

// repoKey identifies a repository by instance and path; GitLab and GitHub
// paths are case-insensitive.
type repoKey struct{ instance, path string }

func keyOf(instance, path string) repoKey { return repoKey{instance, strings.ToLower(path)} }

// keep reports whether a stored project is still wanted: it is switched on,
// and its path didn't resolve to another project on this pass (one deleted
// and recreated under the same path). A project switched on whose lookup
// failed is kept.
func keep(p store.Project, wanted map[repoKey]bool, resolved map[repoKey]int64) bool {
	key := keyOf(p.Instance, p.Path)
	if !wanted[key] {
		return false
	}
	id, ok := resolved[key]
	return !ok || id == p.ProjectID
}

// dropTakenOver deletes the dashboard record of every repository the config
// lists: the config has taken it over, and the record would only mislead.
func (e *Engine) dropTakenOver(ctx context.Context, resolver *settings.Resolver) error {
	rows, err := e.Store.DashboardRepos(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: list dashboard repositories: %w", err)
	}
	var errs []error
	for _, row := range rows {
		if !resolver.InConfig(row.Instance, row.Project) {
			continue
		}
		if err := e.Store.DeleteDashboardRepo(ctx, row.Instance, row.Project); err != nil {
			errs = append(errs, fmt.Errorf("delete the dashboard record of %s on %s: %w", row.Project, row.Instance, err))
			continue
		}
		e.log().Info("the config lists this repository: its dashboard record is deleted",
			"instance", row.Instance, "project", row.Project, "created_by", row.CreatedBy)
	}
	return errors.Join(errs...)
}

// reconcileRepo brings one GitLab repository switched on in line and returns
// its project id.
func (e *Engine) reconcileRepo(ctx context.Context, repo settings.Effective, now time.Time) (int64, error) {
	instance := repo.Instance
	gl := e.gitLab(instance)
	if gl == nil {
		return 0, fmt.Errorf("no bots for instance %q", instance)
	}
	secret, _ := e.WebhookSecrets.Get(instance)
	if secret == "" {
		return 0, fmt.Errorf("no webhook secret for instance %q", instance)
	}

	gp, err := gl.GetProject(ctx, repo.Project)
	if err != nil {
		return 0, fmt.Errorf("get project: %w", err)
	}
	p, _, err := e.Store.Project(ctx, instance, gp.ID)
	if err != nil {
		return 0, fmt.Errorf("load project: %w", err)
	}
	p.Instance, p.ProjectID, p.Path, p.HTTPURLToRepo = instance, gp.ID, repo.Project, gp.HTTPURLToRepo
	if err := e.Store.UpsertProject(ctx, p); err != nil {
		return 0, fmt.Errorf("store project: %w", err)
	}
	// The approve bot is a GitLab user that can always approve.
	n, err := gitLabRequiredApprovals(ctx, gl, gp.ID, gp.DefaultBranch)
	e.checkRequiredApprovals(ctx, instance, repo.Project, gp.DefaultBranch, n, err)

	hookID, err := gl.EnsureHook(ctx, gp.ID, HookURL(e.Config.PublicURL, instance), secret)
	if err != nil {
		return 0, fmt.Errorf("ensure webhook: %w", err)
	}
	if hookID != p.HookID {
		p.HookID = hookID
		if err := e.Store.UpsertProject(ctx, p); err != nil {
			return 0, fmt.Errorf("store webhook id: %w", err)
		}
	}

	if len(p.DeployTokenSealed) > 0 && p.DeployTokenID != 0 && p.DeployTokenExpiresAt.After(now.Add(deployTokenRenew)) {
		if _, err := e.Box.Open(p.DeployTokenSealed); err == nil {
			return gp.ID, nil
		}
		// Sealed with another key (PERGENT_SH_SECRET_KEY changed): the
		// dispatcher can't use it, so replace it now rather than at expiry.
		e.log().Warn("the stored deploy token can't be decrypted with the current secret key; replacing it",
			"instance", instance, "project", repo.Project, "deploy_token_id", p.DeployTokenID)
	}
	old := p.DeployTokenID
	dt, err := gl.CreateDeployToken(ctx, gp.ID, deployTokenName, now.Add(deployTokenLife))
	if err != nil {
		return 0, fmt.Errorf("create deploy token: %w", err)
	}
	if dt.Token == "" || dt.ID == 0 {
		return 0, errors.New("create deploy token: GitLab returned no token")
	}
	p.DeployTokenID, p.DeployTokenUsername = dt.ID, dt.Username
	p.DeployTokenSealed = e.Box.Seal([]byte(dt.Token))
	p.DeployTokenExpiresAt = dt.ExpiresAt
	if p.DeployTokenExpiresAt.IsZero() {
		p.DeployTokenExpiresAt = now.Add(deployTokenLife)
	}
	if err := e.Store.UpsertProject(ctx, p); err != nil {
		// The new token is unknown to the store: revoke it, keep the old.
		sctx, cancel := settleCtx(ctx)
		defer cancel()
		if derr := gl.DeleteDeployToken(sctx, gp.ID, dt.ID); derr != nil {
			e.log().Warn("couldn't revoke the deploy token that wasn't stored", "instance", instance,
				"project", repo.Project, "deploy_token_id", dt.ID, "error", e.redact(derr.Error()))
		}
		return 0, fmt.Errorf("store deploy token: %w", err)
	}
	e.log().Info("deploy token created", "instance", instance, "project", repo.Project,
		"deploy_token_id", dt.ID, "expires_at", p.DeployTokenExpiresAt, "replaced", old)
	if old != 0 && old != dt.ID {
		if err := gl.DeleteDeployToken(ctx, gp.ID, old); err != nil {
			return 0, fmt.Errorf("revoke the replaced deploy token %d: %w", old, err)
		}
	}
	return gp.ID, nil
}

// removeProject deletes a project's webhook and deploy token from GitLab,
// then its row. The row stays when GitLab refuses, so the next pass retries.
// A project of a GitLab instance that is gone (from the config and the
// dashboard) can't be reached:
// its row is dropped with a warning, and its deploy token expires on its own
// within 30 days. A GitHub repository has no webhook or stored token of its
// own: only its row is deleted.
func (e *Engine) removeProject(ctx context.Context, p store.Project) error {
	platform, configured := e.Instances.Platform(p.Instance)
	if platform == config.PlatformGitHub || (!configured && p.InstallationID != 0) {
		if err := e.Store.DeleteProject(ctx, p.Instance, p.ProjectID); err != nil {
			return fmt.Errorf("delete project row: %w", err)
		}
		e.log().Info("repository switched off: its row is deleted",
			"instance", p.Instance, "project", p.Path, "project_id", p.ProjectID)
		return nil
	}
	if !configured {
		if err := e.Store.DeleteProject(ctx, p.Instance, p.ProjectID); err != nil {
			return fmt.Errorf("delete project row: %w", err)
		}
		e.log().Warn("forgot a project of a GitLab instance no longer configured; its webhook stays and its deploy token expires on its own",
			"instance", p.Instance, "project", p.Path, "project_id", p.ProjectID,
			"hook_id", p.HookID, "deploy_token_id", p.DeployTokenID, "deploy_token_expires_at", p.DeployTokenExpiresAt)
		return nil
	}
	gl := e.gitLab(p.Instance)
	if gl == nil {
		return fmt.Errorf("no bots for instance %q", p.Instance)
	}
	var errs []error
	if p.HookID != 0 {
		if err := gl.DeleteHook(ctx, p.ProjectID, p.HookID); err != nil {
			errs = append(errs, fmt.Errorf("delete webhook: %w", err))
		}
	}
	if p.DeployTokenID != 0 {
		if err := gl.DeleteDeployToken(ctx, p.ProjectID, p.DeployTokenID); err != nil {
			errs = append(errs, fmt.Errorf("revoke deploy token: %w", err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := e.Store.DeleteProject(ctx, p.Instance, p.ProjectID); err != nil {
		return fmt.Errorf("delete project row: %w", err)
	}
	e.log().Info("repository switched off: webhook and deploy token removed",
		"instance", p.Instance, "project", p.Path, "project_id", p.ProjectID)
	return nil
}

// gitLab is the review bot's GitLab client of a configured GitLab instance,
// or nil. Reconcile's webhooks and deploy tokens are GitLab's alone.
func (e *Engine) gitLab(instance string) *gitlab.Client {
	if platform, _ := e.Instances.Platform(instance); platform != config.PlatformGitLab {
		return nil
	}
	plat, _ := e.Platforms.Get(instance)
	return plat.GitLab
}

// reconcileGitHubRepo stores one GitHub repository switched on and returns its
// ID: the review App's installation on it (an App not installed there
// leaves any stored row without one, so its runs are reported as such), the
// repository ID and clone URL read through that installation, and whether
// the approve side reaches it. An approve side that doesn't is a warning, not
// an error: reviews still run and are published without approval. When the
// approve side can't be asked, the stored answer is kept (a new repository
// is assumed reachable: a failed approval is retried, never recorded as
// made).
func (e *Engine) reconcileGitHubRepo(ctx context.Context, repo settings.Effective) (int64, error) {
	instance := repo.Instance
	plat, ok := e.Platforms.Get(instance)
	if platform, _ := e.Instances.Platform(instance); platform != config.PlatformGitHub || !ok || !plat.isGitHub() ||
		plat.GitHub == nil || plat.GitHub.App == nil || plat.GitHub.Approve == nil {
		return 0, fmt.Errorf("no bots for instance %q", instance)
	}
	owner, name, _ := strings.Cut(repo.Project, "/")
	app := plat.GitHub.App

	installation, err := app.Installation(ctx, owner, name)
	if errors.Is(err, github.ErrNotFound) {
		if ferr := e.forgetInstallation(ctx, instance, repo.Project); ferr != nil {
			return 0, ferr
		}
		return 0, errors.New("the pergent review App isn't installed on the repository")
	}
	if err != nil {
		return 0, fmt.Errorf("find the review App's installation: %w", err)
	}
	gr, err := app.Client(installation).Repo(ctx, owner, name)
	if err != nil {
		return 0, fmt.Errorf("get repository: %w", err)
	}
	if gr.ID == 0 || gr.CloneURL == "" {
		return 0, errors.New("get repository: GitHub returned no id or clone URL")
	}
	p, found, err := e.Store.Project(ctx, instance, gr.ID)
	if err != nil {
		return 0, fmt.Errorf("load project: %w", err)
	}
	reach, reachErr := plat.GitHub.Approve.CanReach(ctx, owner, name)
	if reachErr != nil {
		reach = !found || p.ApproveAvailable
	}
	p.Instance, p.ProjectID, p.Path, p.HTTPURLToRepo = instance, gr.ID, repo.Project, gr.CloneURL
	p.InstallationID, p.ApproveAvailable = installation, reach
	if err := e.Store.UpsertGitHubProject(ctx, p); err != nil {
		return 0, fmt.Errorf("store project: %w", err)
	}
	if reachErr != nil {
		return 0, fmt.Errorf("check whether the approve side reaches the repository: %w", reachErr)
	}
	if !reach {
		e.log().Warn("the approve side can't reach the repository: its reviews are published without approval",
			"instance", instance, "project", repo.Project, "project_id", gr.ID)
		e.setAttention(ctx, instance, repo.Project, attApproverUnreachable, e.approverUnreachableNote(instance))
		// Nothing is approved here, so how many approvals merging needs
		// can't matter: its item would only go stale.
		e.clearAttention(ctx, instance, repo.Project, attSingleApproval)
		return gr.ID, nil
	}
	e.clearAttention(ctx, instance, repo.Project, attApproverUnreachable)
	n, err := gitHubRequiredApprovals(ctx, app.Client(installation), owner, name, gr.DefaultBranch)
	e.checkRequiredApprovals(ctx, instance, repo.Project, gr.DefaultBranch, n, err)
	return gr.ID, nil
}

// singleApproval is the warning for a repository whose default branch can be
// merged with one approval (or none).
const singleApproval = "one approval is enough to merge here, so pergent's approval alone can unlock it; add a rule requiring a human approval"

// checkRequiredApprovals logs what reading the approvals needed to merge a
// repository's default branch found: a warning when n is at most 1, as the
// approve bot's approval alone then unlocks the merge. The dashboard's
// single_approval item is recorded with that warning and cleared once n is 2
// or more. A read that failed (err) is logged at info, once per repository
// while pergent.sh runs, and leaves the item as it was: the check never fails
// reconcile.
func (e *Engine) checkRequiredApprovals(ctx context.Context, instance, path, branch string, n int, err error) {
	if err != nil {
		key := keyOf(instance, path)
		e.uncheckedMu.Lock()
		seen := e.unchecked[key]
		if e.unchecked == nil {
			e.unchecked = map[repoKey]bool{}
		}
		e.unchecked[key] = true
		e.uncheckedMu.Unlock()
		if !seen {
			e.log().Info("couldn't check how many approvals merging needs; make sure a human approval is required",
				"instance", instance, "project", path, "branch", branch, "error", e.redact(err.Error()))
		}
		return
	}
	if n <= 1 {
		e.log().Warn(path+": "+singleApproval,
			"instance", instance, "project", path, "branch", branch, "required_approvals", n)
		e.setAttention(ctx, instance, path, attSingleApproval, singleApprovalNote)
		return
	}
	e.clearAttention(ctx, instance, path, attSingleApproval)
}

// gitLabRequiredApprovals is the most approvals any approval rule that
// applies to branch requires (security report rules aside); with none, the
// project's own approvals_before_merge.
func gitLabRequiredApprovals(ctx context.Context, gl *gitlab.Client, projectID int64, branch string) (int, error) {
	rules, err := gl.ApprovalRules(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("read the approval rules: %w", err)
	}
	n, found := 0, false
	for _, r := range rules {
		if r.RuleType == "report_approver" || !ruleApplies(r, branch) {
			continue
		}
		n, found = max(n, r.ApprovalsRequired), true
	}
	if found {
		return n, nil
	}
	n, err = gl.ApprovalsBeforeMerge(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("read the project's approvals: %w", err)
	}
	return n, nil
}

// ruleApplies reports whether a GitLab approval rule applies to branch: one
// with no protected branches applies to all branches, one for all protected
// branches is taken to apply (a default branch is protected by default), and
// otherwise one of its protected branch names, "*" matching anything, has to
// match.
func ruleApplies(r gitlab.ApprovalRule, branch string) bool {
	if r.AppliesToAllProtectedBranches || len(r.ProtectedBranches) == 0 {
		return true
	}
	for _, pb := range r.ProtectedBranches {
		if wildcardMatch(pb.Name, branch) {
			return true
		}
	}
	return false
}

// wildcardMatch matches name against GitLab's protected branch pattern, in
// which "*" matches any run of characters ("/" included).
func wildcardMatch(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(name, mid)
		if i < 0 {
			return false
		}
		name = name[i+len(mid):]
	}
	return strings.HasSuffix(name, parts[len(parts)-1])
}

// gitHubRequiredApprovals is the most approving reviews branch needs: from
// the rulesets that apply to it and from its classic protection rule, which
// GitHub both enforces. The classic rule needs the Administration
// permission to read; when it can't be read, the rulesets' count stands, and
// with no ruleset count either, the check fails.
func gitHubRequiredApprovals(ctx context.Context, c *github.Client, owner, name, branch string) (int, error) {
	if branch == "" {
		return 0, errors.New("GitHub returned no default branch")
	}
	n, found, err := c.RulesetApprovals(ctx, owner, name, branch)
	if err != nil {
		return 0, fmt.Errorf("read the branch's rules: %w", err)
	}
	if found && n > 1 {
		return n, nil
	}
	classic, err := c.ProtectionApprovals(ctx, owner, name, branch)
	switch {
	case err == nil:
		return max(n, classic), nil
	case found:
		return n, nil
	default:
		return 0, fmt.Errorf("no ruleset requires reviews, and the branch protection can't be read: %w", err)
	}
}

// forgetInstallation clears the stored installation of instance's repository
// path (the review App was uninstalled), so dispatch reports its runs instead
// of trying a token that no longer works.
func (e *Engine) forgetInstallation(ctx context.Context, instance, path string) error {
	stored, err := e.Store.Projects(ctx)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	for _, p := range stored {
		if keyOf(p.Instance, p.Path) != keyOf(instance, path) || p.InstallationID == 0 {
			continue
		}
		p.InstallationID = 0
		if err := e.Store.UpsertGitHubProject(ctx, p); err != nil {
			return fmt.Errorf("store project: %w", err)
		}
	}
	return nil
}
