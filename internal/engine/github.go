package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/settings"
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

// settings resolves repositories from the config and the dashboard.
func (e *Engine) settings() *settings.Resolver {
	return &settings.Resolver{Config: e.Config, Store: e.Store, Instances: e.registry()}
}
