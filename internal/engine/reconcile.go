package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
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

// Reconcile makes GitLab match the config. For each configured repository it
// stores the project, makes sure its merge request webhook exists with the
// instance's secret, and creates a clone token when there is none, the one it
// has expires within 7 days, or the stored one can't be decrypted (the secret
// key changed). The new token is stored before the old one is revoked, so a
// dispatch never finds none. For each stored project no longer
// configured it deletes the webhook and the deploy token, then the row. One
// repository's failure is logged and the rest continue; the returned error
// joins them all. Tokens are never logged.
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
		id, err := e.reconcileRepo(ctx, repo, now)
		if err != nil {
			fail(fmt.Errorf("reconcile %s on %s: %w", repo.Project, repo.GitLab, err),
				"instance", repo.GitLab, "project", repo.Project)
			continue
		}
		resolved[keyOf(repo.GitLab, repo.Project)] = id
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

// repoKey identifies a repository by instance and path; GitLab paths are
// case-insensitive.
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
		if keyOf(repo.GitLab, repo.Project) == key {
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
	bots, ok := e.Bots[repo.GitLab]
	if !ok || bots.Review == nil {
		return 0, fmt.Errorf("no GitLab bots for instance %q", repo.GitLab)
	}
	secret := e.WebhookSecrets[repo.GitLab]
	if secret == "" {
		return 0, fmt.Errorf("no webhook secret for instance %q", repo.GitLab)
	}

	gp, err := bots.Review.GetProject(ctx, repo.Project)
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

	hookID, err := bots.Review.EnsureHook(ctx, gp.ID, HookURL(e.Config.PublicURL, repo.GitLab), secret)
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
	dt, err := bots.Review.CreateDeployToken(ctx, gp.ID, deployTokenName, now.Add(deployTokenLife))
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
		if derr := bots.Review.DeleteDeployToken(sctx, gp.ID, dt.ID); derr != nil {
			e.log().Warn("couldn't revoke the deploy token that wasn't stored", "instance", repo.GitLab,
				"project", repo.Project, "deploy_token_id", dt.ID, "error", e.redact(derr.Error()))
		}
		return 0, fmt.Errorf("store deploy token: %w", err)
	}
	e.log().Info("deploy token created", "instance", repo.GitLab, "project", repo.Project,
		"deploy_token_id", dt.ID, "expires_at", p.DeployTokenExpiresAt, "replaced", old)
	if old != 0 && old != dt.ID {
		if err := bots.Review.DeleteDeployToken(ctx, gp.ID, old); err != nil {
			return 0, fmt.Errorf("revoke the replaced deploy token %d: %w", old, err)
		}
	}
	return gp.ID, nil
}

// removeProject deletes a project's webhook and deploy token from GitLab,
// then its row. The row stays when GitLab refuses, so the next pass retries.
// A project of a GitLab instance no longer in the config can't be reached:
// its row is dropped with a warning, and its deploy token expires on its own
// within 30 days.
func (e *Engine) removeProject(ctx context.Context, p store.Project) error {
	if _, ok := e.Config.Instance(p.Instance); !ok {
		if err := e.Store.DeleteProject(ctx, p.Instance, p.ProjectID); err != nil {
			return fmt.Errorf("delete project row: %w", err)
		}
		e.log().Warn("forgot a project of a GitLab instance no longer configured; its webhook stays and its deploy token expires on its own",
			"instance", p.Instance, "project", p.Path, "project_id", p.ProjectID,
			"hook_id", p.HookID, "deploy_token_id", p.DeployTokenID, "deploy_token_expires_at", p.DeployTokenExpiresAt)
		return nil
	}
	bots, ok := e.Bots[p.Instance]
	if !ok || bots.Review == nil {
		return fmt.Errorf("no GitLab bots for instance %q", p.Instance)
	}
	var errs []error
	if p.HookID != 0 {
		if err := bots.Review.DeleteHook(ctx, p.ProjectID, p.HookID); err != nil {
			errs = append(errs, fmt.Errorf("delete webhook: %w", err))
		}
	}
	if p.DeployTokenID != 0 {
		if err := bots.Review.DeleteDeployToken(ctx, p.ProjectID, p.DeployTokenID); err != nil {
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
