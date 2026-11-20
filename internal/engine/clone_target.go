package engine

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The default branch head and the read-only clone token of a repository, read
// the same way for a readiness scan and a code graph build. Each helper logs
// what the platform answered (redacted) with the caller's log and prefix
// ("readiness scan: ", "code graph: ") and returns one of the scan's reasons.

// gitLabHead reads the project's default branch and its head commit with the
// review bot. The project is returned too, for the reads that need more of
// it. The error is errNoDefaultBranch or errDefaultBranch.
func (e *Engine) gitLabHead(ctx context.Context, log *slog.Logger, prefix string, gl *gitlab.Client, p store.Project) (gitlab.Project, string, error) {
	gp, err := gl.GetProject(ctx, strconv.FormatInt(p.ProjectID, 10))
	if err != nil {
		log.Info(prefix+"couldn't read the project", "error", e.redact(err.Error()))
		return gp, "", errDefaultBranch
	}
	if gp.DefaultBranch == "" {
		return gp, "", errNoDefaultBranch
	}
	commit, err := gl.CommitSHA(ctx, p.ProjectID, gp.DefaultBranch)
	if errors.Is(err, gitlab.ErrNotFound) {
		return gp, "", errNoDefaultBranch
	}
	if err != nil {
		log.Info(prefix+"couldn't read the default branch", "branch", gp.DefaultBranch,
			"error", e.redact(err.Error()))
		return gp, "", errDefaultBranch
	}
	return gp, commit, nil
}

// gitLabCloneToken is dispatch's clone token: the project's sealed deploy
// token, opened. The error is errNoCloneToken.
func (e *Engine) gitLabCloneToken(log *slog.Logger, prefix string, p store.Project) ([]byte, error) {
	if len(p.DeployTokenSealed) == 0 {
		return nil, errNoCloneToken
	}
	tok, err := e.Box.Open(p.DeployTokenSealed)
	if err != nil {
		// Sealed with another key: reconcile replaces it.
		log.Warn(prefix+"the stored deploy token can't be decrypted", "error", e.redact(err.Error()))
		return nil, errNoCloneToken
	}
	return tok, nil
}

// gitHubHead reads the repository's default branch, its name and the branch
// (head commit and protection), with the review App's installation client c.
// The error is errNoDefaultBranch or errDefaultBranch.
func (e *Engine) gitHubHead(ctx context.Context, log *slog.Logger, prefix string, c *github.Client, owner, name string) (string, github.Branch, error) {
	repo, err := c.Repo(ctx, owner, name)
	if err != nil {
		log.Info(prefix+"couldn't read the repository", "error", e.redact(err.Error()))
		return "", github.Branch{}, errDefaultBranch
	}
	if repo.DefaultBranch == "" {
		return "", github.Branch{}, errNoDefaultBranch
	}
	branch, err := c.Branch(ctx, owner, name, repo.DefaultBranch)
	if errors.Is(err, github.ErrNotFound) {
		// A repository with no commits names a default branch that isn't there.
		return "", github.Branch{}, errNoDefaultBranch
	}
	if err == nil && branch.Commit == "" {
		err = errors.New("GitHub returned no commit")
	}
	if err != nil {
		log.Info(prefix+"couldn't read the default branch", "branch", repo.DefaultBranch,
			"error", e.redact(err.Error()))
		return "", github.Branch{}, errDefaultBranch
	}
	return repo.DefaultBranch, branch, nil
}

// gitHubCloneToken mints the clone token as dispatch does: this one
// repository, contents:read, never stored. The error is errNoCloneToken.
func (e *Engine) gitHubCloneToken(ctx context.Context, log *slog.Logger, prefix string, app *github.App, p store.Project) ([]byte, error) {
	tok, _, err := app.CloneToken(ctx, p.InstallationID, p.ProjectID)
	if err != nil {
		log.Warn(prefix+"couldn't mint the clone token", "error", e.redact(err.Error()))
		return nil, errNoCloneToken
	}
	return []byte(tok), nil
}
