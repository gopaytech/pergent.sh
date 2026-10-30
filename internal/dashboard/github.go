package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/store"
)

type githubAPIError = github.APIError

// githubProvider signs in with the review App's user authorization (its
// client ID and secret) and checks access with the user's token. Such a
// token reaches only what both the user and the App's installations reach,
// and may expire (8 h), with a refresh token to renew it.
type githubProvider struct {
	webURL       string // https://github.com, or a GitHub Enterprise Server host
	apiURL       string
	clientID     string
	clientSecret string
	redirectURI  string
	hc           *http.Client
	now          func() time.Time
	timeout      time.Duration // each call's deadline
}

// githubWebURL is the web address that serves /login/oauth for the API at
// apiURL: github.com for api.github.com, the host for GitHub Enterprise
// Server's /api/v3.
func githubWebURL(apiURL string) string {
	apiURL = strings.TrimRight(apiURL, "/")
	if apiURL == "" || apiURL == github.DefaultAPIURL {
		return "https://github.com"
	}
	return strings.TrimSuffix(apiURL, "/api/v3")
}

func (p *githubProvider) platform() string { return config.PlatformGitHub }

func (p *githubProvider) authorizeURL(state, challenge string) string {
	q := url.Values{
		"client_id":             {p.clientID},
		"redirect_uri":          {p.redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"allow_signup":          {"false"},
	}
	return p.webURL + "/login/oauth/authorize?" + q.Encode()
}

func (p *githubProvider) exchange(ctx context.Context, code, verifier string) (oauthToken, error) {
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	return postToken(ctx, p.hc, p.webURL+"/login/oauth/access_token", url.Values{
		"client_id":     {p.clientID},
		"client_secret": {p.clientSecret},
		"code":          {code},
		"redirect_uri":  {p.redirectURI},
		"code_verifier": {verifier},
	}, p.now())
}

func (p *githubProvider) refresh(ctx context.Context, refreshToken string) (oauthToken, error) {
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	return postToken(ctx, p.hc, p.webURL+"/login/oauth/access_token", url.Values{
		"client_id":     {p.clientID},
		"client_secret": {p.clientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}, p.now())
}

func (p *githubProvider) client(accessToken string) *github.Client {
	return github.NewTokenClient(p.apiURL, accessToken, p.hc)
}

func (p *githubProvider) user(ctx context.Context, accessToken string) (int64, string, error) {
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	u, err := p.client(accessToken).CurrentUser(ctx)
	if err != nil {
		return 0, "", err
	}
	if u.ID == 0 || u.Login == "" {
		return 0, "", errors.New("github: /user answered without an id or login")
	}
	return u.ID, u.Login, nil
}

// canSee: a public repository, or read (pull) access to a private or
// internal one.
func (p *githubProvider) canSee(ctx context.Context, accessToken, project string) (int64, bool, error) {
	owner, name, ok := strings.Cut(project, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return 0, false, nil
	}
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	a, err := p.client(accessToken).RepoAccess(ctx, owner, name)
	if errors.Is(err, github.ErrNotFound) || isStatus(err, http.StatusForbidden) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if a.ID <= 0 {
		return 0, false, errors.New("github: the repository answer has no id")
	}
	return a.ID, !a.Private || a.Pull, nil
}

func (p *githubProvider) isAdmin(ctx context.Context, accessToken string, _ int64, username string, admin config.DashboardAdmin) (bool, error) {
	org, slug, ok := strings.Cut(admin.Team, "/")
	if !ok || org == "" || slug == "" {
		return false, nil
	}
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	return p.client(accessToken).TeamMembershipActive(ctx, org, slug, username)
}

func (p *githubProvider) memberRepos(ctx context.Context, accessToken, search string, limit int) ([]string, bool, error) {
	timeout := p.timeout
	if timeout <= 0 {
		timeout = platformTimeout
	}
	return p.client(accessToken).UserRepos(ctx, github.ListOptions{Match: search, Limit: limit, CallTimeout: timeout})
}

// memberRepo: read (pull) access, as GET /repos/{owner}/{repo} reports it
// to the user.
func (p *githubProvider) memberRepo(ctx context.Context, accessToken, project string) (store.VisibleProject, bool, error) {
	owner, name, ok := strings.Cut(project, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return store.VisibleProject{}, false, nil
	}
	ctx, cancel := callContext(ctx, p.timeout)
	defer cancel()
	a, err := p.client(accessToken).RepoAccess(ctx, owner, name)
	if errors.Is(err, github.ErrNotFound) || isStatus(err, http.StatusForbidden) {
		return store.VisibleProject{}, false, nil
	}
	if err != nil {
		return store.VisibleProject{}, false, err
	}
	if !a.Pull {
		return store.VisibleProject{}, false, nil
	}
	if a.FullName == "" || a.ID <= 0 {
		return store.VisibleProject{}, false, errors.New("github: the repository answer has no full name or id")
	}
	return store.VisibleProject{Path: a.FullName, ID: a.ID}, true, nil
}
