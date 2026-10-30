package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// GitHubRepos resolves a GitHub instance's repository IDs from the projects
// reconcile stored: owner/name from the path, and the review App's
// installation. A repository that isn't stored is githubforge.ErrUnknownRepo.
func GitHubRepos(st *store.Store, instance string) githubforge.Repos {
	return githubforge.ReposFunc(func(ctx context.Context, repoID int64) (githubforge.Repo, error) {
		p, found, err := st.Project(ctx, instance, repoID)
		if err != nil {
			return githubforge.Repo{}, fmt.Errorf("load repository %d: %w", repoID, err)
		}
		if !found {
			return githubforge.Repo{}, fmt.Errorf("repository %d on %s: %w", repoID, instance, githubforge.ErrUnknownRepo)
		}
		owner, name, ok := strings.Cut(p.Path, "/")
		if !ok || owner == "" || name == "" {
			return githubforge.Repo{}, fmt.Errorf("repository %d on %s: stored path %q is not owner/name", repoID, instance, p.Path)
		}
		return githubforge.Repo{Owner: owner, Name: name, InstallationID: p.InstallationID}, nil
	})
}

// repository returns the configured repository path names on instance,
// whichever platform the instance is on.
func (e *Engine) repository(instance, path string) (config.Repository, bool) {
	if platform, ok := e.Config.Platform(instance); ok && platform == config.PlatformGitHub {
		return e.Config.GitHubRepository(instance, path)
	}
	return e.Config.Repository(instance, path)
}
