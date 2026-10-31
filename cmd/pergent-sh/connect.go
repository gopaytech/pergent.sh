package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/engine"
	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/forge/gitlabforge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// attInstanceUnreachable is the kind of the instance-wide "Needs attention"
// item recorded for an instance of the dashboard that couldn't be connected
// at startup.
const attInstanceUnreachable = "instance_unreachable"

// connector puts instances in use and takes them out again, while pergent.sh
// runs: it connects to the platform with the instance's credentials and sets
// the result in the maps the engine, the webhooks and the dashboard read.
type connector struct {
	reg   *instances.Registry
	store *store.Store
	hc    *http.Client // the platforms' HTTP client; nil means the default
	log   *slog.Logger

	platforms   *syncmap.Map[engine.Platform]
	gitlabHooks *syncmap.Map[string] // webhook secret by GitLab instance
	githubHooks *syncmap.Map[string] // webhook secret by GitHub instance
	// sources and catalogs are the dashboard's readers of repositories, by
	// instance; nil without the dashboard.
	sources  *syncmap.Map[dashboard.RepoSource]
	catalogs *syncmap.Map[dashboard.RepoCatalog]
	auth     *dashboard.Auth // nil without the dashboard
	// githubRepos resolves a GitHub instance's repository IDs.
	githubRepos func(instance string) githubforge.Repos
}

var _ dashboard.Connector = (*connector)(nil)

// Check tries the credentials of inst: the platform must accept them. It
// changes nothing. An error names the instance and what failed, never a
// secret.
func (c *connector) Check(ctx context.Context, inst instances.Instance, s instances.Secrets) error {
	_, err := c.build(ctx, inst, s)
	return err
}

// Connect checks inst, then puts it in use: its platform, its webhook secret
// and, with an OAuth client, its sign-in. Connecting again replaces what was
// connected.
func (c *connector) Connect(ctx context.Context, inst instances.Instance, s instances.Secrets) error {
	plat, err := c.build(ctx, inst, s)
	if err != nil {
		return err
	}
	// A token approver tells the dashboard when its token is about to expire.
	if a, ok := plat.Approver.(*githubforge.Approver); ok && inst.GitHub != nil && inst.GitHub.ApproveApp == nil && c.store != nil {
		a.OnTokenExpiry(engine.TokenExpiryRecorder(c.store, inst.Name, c.log.With("instance", inst.Name)))
	}
	c.platforms.Set(inst.Name, plat)
	if inst.Platform == config.PlatformGitHub {
		c.githubHooks.Set(inst.Name, s.WebhookSecret)
	} else {
		c.gitlabHooks.Set(inst.Name, s.WebhookSecret)
	}
	c.serveDashboard(inst.Name, plat)
	if c.auth != nil {
		c.auth.SetProvider(inst, s)
	}
	return nil
}

// Disconnect takes the instance called name out of use. It is not an error
// for it not to be connected.
func (c *connector) Disconnect(name string) {
	c.platforms.Delete(name)
	c.gitlabHooks.Delete(name)
	c.githubHooks.Delete(name)
	if c.sources != nil {
		c.sources.Delete(name)
	}
	if c.catalogs != nil {
		c.catalogs.Delete(name)
	}
	if c.auth != nil {
		c.auth.RemoveProvider(name)
	}
}

// serveDashboard gives the dashboard the Forge of plat as the reader of the
// instance's repositories and the lister of what can be switched on. A
// platform whose Forge can't do either is named in a warning, with its
// platform.
func (c *connector) serveDashboard(name string, plat engine.Platform) {
	if c.sources == nil || c.catalogs == nil {
		return
	}
	var missing []string
	if s, ok := plat.Forge.(dashboard.RepoSource); ok {
		c.sources.Set(name, s)
	} else {
		missing = append(missing, "Skills & instructions")
	}
	if cat, ok := plat.Forge.(dashboard.RepoCatalog); ok {
		c.catalogs.Set(name, cat)
	} else {
		missing = append(missing, "the add-repository picker")
	}
	if len(missing) > 0 {
		c.log.Warn("dashboard: the instance's review side can't serve "+strings.Join(missing, " or ")+"; they answer 502 there",
			"instance", name, "platform", plat.Kind)
	}
}

// build connects to the platform with the credentials in s and returns the
// instance's platform, changing nothing.
func (c *connector) build(ctx context.Context, inst instances.Instance, s instances.Secrets) (engine.Platform, error) {
	switch {
	case inst.GitLab != nil:
		return c.buildGitLab(ctx, inst.GitLab, s)
	case inst.GitHub != nil:
		return c.buildGitHub(ctx, inst.GitHub, s)
	}
	return engine.Platform{}, fmt.Errorf("instance %q has no platform", inst.Name)
}

// inEnv says where a secret of a config instance comes from: " (VAR)", or
// nothing for an instance of the dashboard, which has no variable.
func inEnv(env string) string {
	if env == "" {
		return ""
	}
	return " (" + env + ")"
}

// missing is an error for the first of the named secrets that is empty;
// secrets are the label and value of each.
func missing(kind, name string, secrets ...[2]string) error {
	for _, s := range secrets {
		if s[1] == "" {
			return fmt.Errorf("%s instance %q: the %s is not set", kind, name, s[0])
		}
	}
	return nil
}

// buildGitLab builds the review and approve clients and checks each token by
// asking who it belongs to; the two must be different users.
func (c *connector) buildGitLab(ctx context.Context, inst *config.Instance, s instances.Secrets) (engine.Platform, error) {
	if err := missing("gitlab", inst.Name, [2]string{"review token", s.ReviewToken},
		[2]string{"approve token", s.ApproveToken}, [2]string{"webhook secret", s.WebhookSecret}); err != nil {
		return engine.Platform{}, err
	}
	review := gitlab.New(inst.URL, s.ReviewToken, nil)
	approve := gitlab.New(inst.URL, s.ApproveToken, nil)
	reviewUser, err := review.CurrentUser(ctx)
	if err != nil {
		return engine.Platform{}, fmt.Errorf("gitlab instance %q: the review token%s doesn't work: %w", inst.Name, inEnv(inst.ReviewTokenEnv), err)
	}
	approveUser, err := approve.CurrentUser(ctx)
	if err != nil {
		return engine.Platform{}, fmt.Errorf("gitlab instance %q: the approve token%s doesn't work: %w", inst.Name, inEnv(inst.ApproveTokenEnv), err)
	}
	if reviewUser.ID == approveUser.ID {
		// One user would approve the reviews it posts itself: the
		// approval has to come from a second identity.
		return engine.Platform{}, fmt.Errorf("gitlab instance %q: the review token%s and the approve token%s belong to the same user; the approve bot must be a separate user",
			inst.Name, inEnv(inst.ReviewTokenEnv), inEnv(inst.ApproveTokenEnv))
	}
	return engine.Platform{
		Kind:     config.PlatformGitLab,
		Forge:    gitlabforge.NewForge(review, reviewUser),
		Approver: gitlabforge.NewApprover(approve, approveUser),
		GitLab:   review,
	}, nil
}

// buildGitHub builds the review App and the approve side (the approve App, or
// a bot user's token) and looks up their bot users once: the review App's
// comments and the approve side's reviews are found by them. A key, App or
// token that doesn't work is an error naming the instance and what failed,
// never the secret.
func (c *connector) buildGitHub(ctx context.Context, inst *config.GitHubInstance, s instances.Secrets) (engine.Platform, error) {
	approveKind, approveSecret := "approve token", s.ApproveToken
	if inst.ApproveApp != nil {
		approveKind, approveSecret = "approve App private key", s.ApproveAppKey
	}
	if err := missing("github", inst.Name, [2]string{"review App private key", s.ReviewAppKey},
		[2]string{approveKind, approveSecret}, [2]string{"webhook secret", s.WebhookSecret}); err != nil {
		return engine.Platform{}, err
	}
	reviewApp, reviewBot, err := connectApp(ctx, inst.APIURL, inst.ReviewApp.AppID, s.ReviewAppKey, c.hc)
	if err != nil {
		return engine.Platform{}, fmt.Errorf("github instance %q: the review App (app_id %d) doesn't work: %w",
			inst.Name, inst.ReviewApp.AppID, err)
	}
	instRepos := c.githubRepos(inst.Name)

	var approver *githubforge.Approver
	if inst.ApproveApp != nil {
		approveApp, approveBot, err := connectApp(ctx, inst.APIURL, inst.ApproveApp.AppID, s.ApproveAppKey, c.hc)
		if err != nil {
			return engine.Platform{}, fmt.Errorf("github instance %q: the approve App (app_id %d) doesn't work: %w",
				inst.Name, inst.ApproveApp.AppID, err)
		}
		approver = githubforge.NewAppApprover(approveApp, approveBot, instRepos)
	} else {
		client := github.NewTokenClient(inst.APIURL, s.ApproveToken, c.hc)
		user, err := client.CurrentUser(ctx)
		if err != nil {
			return engine.Platform{}, fmt.Errorf("github instance %q: the approve token%s doesn't work: %w",
				inst.Name, inEnv(inst.ApproveTokenEnv), err)
		}
		approver = githubforge.NewTokenApprover(client, user.ID, instRepos, c.log.With("instance", inst.Name))
	}
	return engine.Platform{
		Kind:     config.PlatformGitHub,
		Forge:    githubforge.NewForge(reviewApp, reviewBot, instRepos),
		Approver: approver,
		GitHub:   &engine.GitHub{App: reviewApp, Approve: approver},
	}, nil
}

// connectApp builds the GitHub App appID and looks up its bot user's ID,
// which also proves the key belongs to the App.
func connectApp(ctx context.Context, apiURL string, appID int64, key string, hc *http.Client) (*github.App, int64, error) {
	app, err := github.NewApp(apiURL, appID, key, hc)
	if err != nil {
		return nil, 0, err
	}
	bot, err := app.BotUserID(ctx)
	if err != nil {
		return nil, 0, err
	}
	return app, bot, nil
}

// connectAll connects every instance at startup. The config's come first
// and one that can't be connected stops startup. Then the dashboard's are
// loaded: a name that is also in the config stops startup. Those are
// connected next, and one that can't be is logged at Error and recorded as an
// instance-wide "Needs attention" item (instance_unreachable); it never
// stops startup, so a platform that is down doesn't take the dashboard with
// it, which is where the instance is fixed.
func (c *connector) connectAll(ctx context.Context) error {
	for _, inst := range c.reg.All() {
		if inst.Source != instances.SourceConfig {
			continue
		}
		s, _, err := c.reg.Secrets(ctx, inst.Name)
		if err != nil {
			return err
		}
		if err := c.Connect(ctx, inst, s); err != nil {
			return err
		}
	}
	if err := c.reg.Load(ctx); err != nil {
		return err
	}
	for _, inst := range c.reg.All() {
		// One being removed is not connected again: the engine finishes
		// removing it.
		if inst.Source == instances.SourceDashboard && !inst.Removing {
			c.connectFromDashboard(ctx, inst)
		}
	}
	return nil
}

// connectFromDashboard connects one instance of the dashboard at startup,
// recording a failure instead of returning it.
func (c *connector) connectFromDashboard(ctx context.Context, inst instances.Instance) {
	s, unopened, err := c.reg.Secrets(ctx, inst.Name)
	if err == nil {
		if len(unopened) > 0 {
			c.log.Warn("secrets of an instance couldn't be opened with PERGENT_SH_SECRET_KEY; they count as not set",
				"instance", inst.Name, "fields", unopened)
		}
		err = c.Connect(ctx, inst, s)
	}
	if err != nil {
		msg := redactSecrets(err.Error(), s)
		c.log.Error("couldn't connect an instance added in the dashboard; it stays unconnected until fixed there",
			"instance", inst.Name, "error", msg)
		if aerr := c.store.SetAttention(ctx, inst.Name, "", attInstanceUnreachable, msg); aerr != nil {
			c.log.Warn("couldn't record that an instance is unreachable", "instance", inst.Name, "error", redactSecrets(aerr.Error(), s))
		}
		return
	}
	if err := c.store.ClearAttention(ctx, inst.Name, "", attInstanceUnreachable); err != nil {
		c.log.Warn("couldn't clear the unreachable instance item", "instance", inst.Name, "error", err.Error())
	}
}

// redactSecrets replaces every secret of s found in text with [REDACTED].
func redactSecrets(text string, s instances.Secrets) string {
	for _, v := range s.Fields() {
		text = strings.ReplaceAll(text, v, "[REDACTED]")
	}
	return text
}
