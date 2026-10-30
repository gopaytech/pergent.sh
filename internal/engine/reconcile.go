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

// Reconcile makes the platforms match the config. For each configured GitLab
// repository it stores the project, makes sure its merge request webhook
// exists with the instance's secret, and creates a clone token when there is
// none, the one it has expires within 7 days, or the stored one can't be
// decrypted (the secret key changed). The new token is stored before the old
// one is revoked, so a dispatch never finds none. For each configured GitHub
// repository it stores the review App's installation, the repository ID and
// clone URL, and whether the approve side reaches it (see
// reconcileGitHubRepo). For each stored GitLab project no longer configured
// it deletes the webhook and the deploy token, then the row; a GitHub
// repository has nothing to delete but its row. One repository's failure is
// logged and the rest continue; the returned error joins them all. Tokens
// and keys are never logged.
func (e *Engine) Reconcile(ctx context.Context) error {
	now := e.now()
	var errs []error
	fail := func(err error, args ...any) {
		e.log().Error("reconcile failed", append(args, "error", e.redact(err.Error()))...)
		errs = append(errs, err)
	}

	// The project id each configured repository resolved to on this pass.
	resolved := map[repoKey]int64{}
	for _, repo := range e.Config.Repositories {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		var (
			id  int64
			err error
		)
		if repo.GitHub != "" {
			id, err = e.reconcileGitHubRepo(ctx, repo)
		} else {
			id, err = e.reconcileRepo(ctx, repo, now)
		}
		if err != nil {
			fail(fmt.Errorf("reconcile %s on %s: %w", repo.Project, repo.Instance(), err),
				"instance", repo.Instance(), "project", repo.Project)
			continue
		}
		resolved[keyOf(repo.Instance(), repo.Project)] = id
	}

	stored, err := e.Store.Projects(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("reconcile: list projects: %w", err))...)
	}
	for _, p := range stored {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		if e.keep(p, resolved) {
			continue
		}
		if err := e.removeProject(ctx, p); err != nil {
			fail(fmt.Errorf("remove %s (project %d) on %s: %w", p.Path, p.ProjectID, p.Instance, err),
				"instance", p.Instance, "project", p.Path, "project_id", p.ProjectID)
		}
	}
	return errors.Join(errs...)
}

// repoKey identifies a repository by instance and path; GitLab and GitHub
// paths are case-insensitive.
type repoKey struct{ instance, path string }

func keyOf(instance, path string) repoKey { return repoKey{instance, strings.ToLower(path)} }

// keep reports whether a stored project is still wanted: it is configured,
// and its path didn't resolve to another project on this pass (one deleted
// and recreated under the same path). A configured project whose lookup
// failed is kept.
func (e *Engine) keep(p store.Project, resolved map[repoKey]int64) bool {
	key := keyOf(p.Instance, p.Path)
	configured := false
	for _, repo := range e.Config.Repositories {
		if keyOf(repo.Instance(), repo.Project) == key {
			configured = true
			break
		}
	}
	if !configured {
		return false
	}
	id, ok := resolved[key]
	return !ok || id == p.ProjectID
}

// reconcileRepo brings one configured repository in line and returns its
// project id.
func (e *Engine) reconcileRepo(ctx context.Context, repo config.Repository, now time.Time) (int64, error) {
	gl := e.gitLab(repo.GitLab)
	if gl == nil {
		return 0, fmt.Errorf("no bots for instance %q", repo.GitLab)
	}
	secret := e.WebhookSecrets[repo.GitLab]
	if secret == "" {
		return 0, fmt.Errorf("no webhook secret for instance %q", repo.GitLab)
	}

	gp, err := gl.GetProject(ctx, repo.Project)
	if err != nil {
		return 0, fmt.Errorf("get project: %w", err)
	}
	p, _, err := e.Store.Project(ctx, repo.GitLab, gp.ID)
	if err != nil {
		return 0, fmt.Errorf("load project: %w", err)
	}
	p.Instance, p.ProjectID, p.Path, p.HTTPURLToRepo = repo.GitLab, gp.ID, repo.Project, gp.HTTPURLToRepo
	if err := e.Store.UpsertProject(ctx, p); err != nil {
		return 0, fmt.Errorf("store project: %w", err)
	}

	hookID, err := gl.EnsureHook(ctx, gp.ID, HookURL(e.Config.PublicURL, repo.GitLab), secret)
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
			"instance", repo.GitLab, "project", repo.Project, "deploy_token_id", p.DeployTokenID)
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
			e.log().Warn("couldn't revoke the deploy token that wasn't stored", "instance", repo.GitLab,
				"project", repo.Project, "deploy_token_id", dt.ID, "error", e.redact(derr.Error()))
		}
		return 0, fmt.Errorf("store deploy token: %w", err)
	}
	e.log().Info("deploy token created", "instance", repo.GitLab, "project", repo.Project,
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
// A project of a GitLab instance no longer in the config can't be reached:
// its row is dropped with a warning, and its deploy token expires on its own
// within 30 days. A GitHub repository has no webhook or stored token of its
// own: only its row is deleted.
func (e *Engine) removeProject(ctx context.Context, p store.Project) error {
	platform, configured := e.Config.Platform(p.Instance)
	if platform == config.PlatformGitHub || (!configured && p.InstallationID != 0) {
		if err := e.Store.DeleteProject(ctx, p.Instance, p.ProjectID); err != nil {
			return fmt.Errorf("delete project row: %w", err)
		}
		e.log().Info("repository switched off: its row is deleted",
			"instance", p.Instance, "project", p.Path, "project_id", p.ProjectID)
		return nil
	}
	if _, ok := e.Config.Instance(p.Instance); !ok {
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
	if _, ok := e.Config.Instance(instance); !ok {
		return nil
	}
	return e.Platforms[instance].GitLab
}

// reconcileGitHubRepo stores one configured GitHub repository and returns its
// ID: the review App's installation on it (an App not installed there
// leaves any stored row without one, so its runs are reported as such), the
// repository ID and clone URL read through that installation, and whether
// the approve side reaches it. An approve side that doesn't is a warning, not
// an error: reviews still run and are published without approval. When the
// approve side can't be asked, the stored answer is kept (a new repository
// is assumed reachable: a failed approval is retried, never recorded as
// made).
func (e *Engine) reconcileGitHubRepo(ctx context.Context, repo config.Repository) (int64, error) {
	instance := repo.GitHub
	plat, ok := e.Platforms[instance]
	if _, configured := e.Config.GitHubInstance(instance); !configured || !ok || !plat.isGitHub() ||
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
	}
	return gr.ID, nil
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
