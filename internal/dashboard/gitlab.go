package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// gitlabReporter is GitLab's Reporter access level, the lowest that reads
// code and merge requests of a project that isn't public.
const gitlabReporter = 20

// gitlabProvider signs in with an instance's OAuth application (scopes
// read_user read_api) and checks access with the user's token.
type gitlabProvider struct {
	baseURL      string
	clientID     string
	clientSecret string
	redirectURI  string
	hc           *http.Client
	now          func() time.Time
	timeout      time.Duration // each call's deadline
}

func (p *gitlabProvider) platform() string { return config.PlatformGitLab }

func (p *gitlabProvider) authorizeURL(state, challenge string) string {
	q := url.Values{
		"client_id":             {p.clientID},
		"redirect_uri":          {p.redirectURI},
		"response_type":         {"code"},
		"scope":                 {"read_user read_api"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return p.baseURL + "/oauth/authorize?" + q.Encode()
}

func (p *gitlabProvider) exchange(ctx context.Context, code, verifier string) (oauthToken, error) {
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	return postToken(ctx, p.hc, p.baseURL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.redirectURI},
		"client_id":     {p.clientID},
		"client_secret": {p.clientSecret},
		"code_verifier": {verifier},
	}, p.now())
}

func (p *gitlabProvider) refresh(ctx context.Context, refreshToken string) (oauthToken, error) {
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	return postToken(ctx, p.hc, p.baseURL+"/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"redirect_uri":  {p.redirectURI},
		"client_id":     {p.clientID},
		"client_secret": {p.clientSecret},
	}, p.now())
}

func (p *gitlabProvider) client(accessToken string) *gitlab.Client {
	return gitlab.NewOAuth(p.baseURL, accessToken, p.hc)
}

func (p *gitlabProvider) user(ctx context.Context, accessToken string) (int64, string, error) {
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	u, err := p.client(accessToken).CurrentUser(ctx)
	if err != nil {
		return 0, "", err
	}
	if u.ID == 0 || u.Username == "" {
		return 0, "", errors.New("gitlab: /user answered without an id or username")
	}
	return u.ID, u.Username, nil
}

// canSee: Reporter or above (through the project, its group, or a share
// with another group), or a public project whose code and merge requests
// are open to everyone who can see it.
func (p *gitlabProvider) canSee(ctx context.Context, accessToken, project string) (int64, bool, error) {
	a, err := p.projectAccess(ctx, accessToken, project)
	if errors.Is(err, gitlab.ErrNotFound) || isStatus(err, http.StatusForbidden) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if a.ID <= 0 {
		return 0, false, errors.New("gitlab: the project answer has no id")
	}
	if a.Visibility == "public" && a.RepositoryAccess == "enabled" && a.MergeRequestsAccess == "enabled" {
		return a.ID, true, nil
	}
	ok, err := p.reporter(ctx, accessToken, a)
	return a.ID, ok, err
}

// projectAccess reads the token user's access to project.
func (p *gitlabProvider) projectAccess(ctx context.Context, accessToken, project string) (gitlab.ProjectAccess, error) {
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	return p.client(accessToken).ProjectAccess(ctx, project)
}

// reporter reports whether the token's user is Reporter or above on the
// project a describes: as its permissions show (the project or its group),
// or else through a share, which only the project listing shows (one more
// call).
func (p *gitlabProvider) reporter(ctx context.Context, accessToken string, a gitlab.ProjectAccess) (bool, error) {
	if a.ProjectAccessLevel >= gitlabReporter || a.GroupAccessLevel >= gitlabReporter {
		return true, nil
	}
	if a.ID <= 0 {
		return false, errors.New("gitlab: the project answer has no id")
	}
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	return p.client(accessToken).HasMemberAccess(ctx, a.ID, gitlabReporter)
}

func (p *gitlabProvider) isAdmin(ctx context.Context, accessToken string, userID int64, _ string, admin config.DashboardAdmin) (bool, error) {
	if admin.Group == "" {
		return false, nil
	}
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	return p.client(accessToken).IsGroupMember(ctx, admin.Group, userID)
}

// isStatus reports whether err is a GitLab or GitHub API error with status.
func isStatus(err error, status int) bool {
	var gl *gitlab.APIError
	if errors.As(err, &gl) {
		return gl.Status == status
	}
	var gh *githubAPIError
	return errors.As(err, &gh) && gh.Status == status
}

func (p *gitlabProvider) memberRepos(ctx context.Context, accessToken, search string, limit int) ([]string, bool, error) {
	return p.client(accessToken).ProjectPaths(ctx, gitlab.ProjectListOptions{MinAccessLevel: gitlabReporter,
		Search: search, Limit: limit, CallTimeout: p.callTimeout()})
}

// memberRepo: Reporter or above through the project, its group or a share,
// as the picker's listing counts it; being able to see a public project
// isn't enough.
func (p *gitlabProvider) memberRepo(ctx context.Context, accessToken, project string) (store.VisibleProject, bool, error) {
	a, err := p.projectAccess(ctx, accessToken, project)
	if errors.Is(err, gitlab.ErrNotFound) || isStatus(err, http.StatusForbidden) {
		return store.VisibleProject{}, false, nil
	}
	if err != nil {
		return store.VisibleProject{}, false, err
	}
	if ok, err := p.reporter(ctx, accessToken, a); err != nil || !ok {
		return store.VisibleProject{}, false, err
	}
	if a.Path == "" {
		return store.VisibleProject{}, false, errors.New("gitlab: the project answer has no path")
	}
	return store.VisibleProject{Path: a.Path, ID: a.ID}, true, nil
}

func (p *gitlabProvider) callTimeout() time.Duration {
	if p.timeout <= 0 {
		return platformTimeout
	}
	return p.timeout
}
