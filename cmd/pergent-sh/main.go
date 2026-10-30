// Command pergent-sh runs pergent code reviews on GitLab merge requests and
// GitHub pull requests as Kubernetes Jobs.
//
//	pergent-sh serve [--config /etc/pergent-sh/config.yaml]
//
// It needs DATABASE_URL and PERGENT_SH_SECRET_KEY in the environment, plus the
// LLM key, the GitLab tokens, the GitHub App private keys (or approve token)
// and the webhook secrets the config file names. With dashboard.url set it
// also serves the dashboard, and needs the OAuth client ID and secret of
// every instance the config sets up for sign-in.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/engine"
	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/forge/gitlabforge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/server"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	defaultConfigPath = "/etc/pergent-sh/config.yaml"
	shutdownTimeout   = 20 * time.Second
	// writeTimeout bounds writing a response. It must leave room above the
	// longest dashboard request (dashboard.AvailableBudget).
	writeTimeout = 60 * time.Second
	// sessionPruneEvery is how often the dashboard's expired sessions are
	// deleted.
	sessionPruneEvery = time.Hour
	usage             = "usage: pergent-sh serve [--config <path>]"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	configPath, err := parseArgs(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := serve(ctx, configPath, log); err != nil {
		log.Error("pergent-sh stopped", "error", err.Error())
		os.Exit(1)
	}
}

// parseArgs reads "serve [--config <path>]" and returns the config path.
func parseArgs(args []string, stderr io.Writer) (string, error) {
	if len(args) == 0 || args[0] != "serve" {
		return "", errors.New(usage)
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", defaultConfigPath, "path to the config file")
	if err := fs.Parse(args[1:]); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), usage)
	}
	return *path, nil
}

// serve starts pergent.sh and runs it until ctx is done. Anything wrong with
// the config, the secrets, the database, a GitLab token or a GitHub App or
// token stops it before it serves; secrets are never part of the error.
func serve(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	lib, err := inputs.LoadLibrary(cfg.SkillsDir)
	if err != nil {
		return fmt.Errorf("skill library %s: %w", cfg.SkillsDir, err)
	}
	if err := checkSkills(cfg, lib); err != nil {
		return err
	}
	if err := checkDashboardEnv(cfg); err != nil {
		return err
	}

	dbURL, err := requireEnv("DATABASE_URL")
	if err != nil {
		return err
	}
	key, err := requireEnv("PERGENT_SH_SECRET_KEY")
	if err != nil {
		return err
	}
	box, err := secretbox.New(key)
	if err != nil {
		return fmt.Errorf("PERGENT_SH_SECRET_KEY: %w", err)
	}
	llmKey, err := requireEnv(cfg.LLM.APIKeyEnv)
	if err != nil {
		return fmt.Errorf("llm.api_key_env: %w", err)
	}

	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	platforms, hookSecrets, err := connectGitLab(ctx, cfg)
	if err != nil {
		return err
	}
	githubRepos := func(instance string) githubforge.Repos { return engine.GitHubRepos(st, instance) }
	githubPlatforms, githubSecrets, err := connectGitHub(ctx, cfg, githubRepos, nil, log)
	if err != nil {
		return err
	}
	// Instance names are unique across gitlab: and github: (config.Validate).
	maps.Copy(platforms, githubPlatforms)
	// A token approver tells the dashboard when its token is about to expire.
	for _, inst := range cfg.GitHub {
		if a, ok := githubPlatforms[inst.Name].Approver.(*githubforge.Approver); ok && inst.ApproveTokenEnv != "" {
			a.OnTokenExpiry(engine.TokenExpiryRecorder(st, inst.Name, log.With("instance", inst.Name)))
		}
	}
	restCfg, err := kubeConfig()
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	eng := &engine.Engine{
		Store:          st,
		Config:         cfg,
		Platforms:      platforms,
		WebhookSecrets: hookSecrets,
		Launcher:       &kube.Launcher{Client: clientset, Namespace: cfg.Jobs.Namespace},
		Library:        lib,
		Box:            box,
		LLMKey:         llmKey,
		Log:            log,
	}
	dash, err := newDashboard(cfg, st, key, lib, platforms, eng.ReconcileSoon, log)
	if err != nil {
		return err
	}
	handler := server.Routes(server.Deps{
		Config: cfg, WebhookSecrets: hookSecrets, GitHubWebhookSecrets: githubSecrets,
		Store: st, Wake: eng.Wake, Dashboard: dashboardHandler(dash), Log: log,
	})
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	log.Info("pergent-sh serving", "listen", cfg.Listen, "repositories", len(cfg.Repositories),
		"dashboard", cfg.DashboardEnabled())
	// Engine.Run reconciles first, in its own goroutine, so a slow GitLab
	// at startup never keeps the routes down.
	loops := []func(context.Context){eng.Run}
	if dash != nil {
		prune := pruneSessions(dash.Auth.PruneSessions, time.Now, log)
		loops = append(loops, func(ctx context.Context) { every(ctx, sessionPruneEvery, prune) })
	}
	return serveUntilDone(ctx, ln, handler, together(loops...), log)
}

// checkDashboardEnv makes sure, when the dashboard is on, that every
// instance set up for sign-in has its OAuth client ID and secret in the
// environment. The error names the instance and the variable, never a
// value. With the dashboard off nothing is read.
func checkDashboardEnv(cfg *config.Config) error {
	if !cfg.DashboardEnabled() {
		return nil
	}
	var errs []error
	for _, inst := range cfg.GitLab {
		if inst.HasOAuth() {
			if _, _, err := inst.OAuthClient(); err != nil {
				errs = append(errs, fmt.Errorf("dashboard sign-in: %w", err))
			}
		}
	}
	for _, inst := range cfg.GitHub {
		if inst.HasOAuth() {
			if _, _, err := inst.OAuthClient(); err != nil {
				errs = append(errs, fmt.Errorf("dashboard sign-in: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// newDashboard builds the dashboard when dashboard.url is set, and returns
// nil, nil when it isn't. Its Skills & instructions view and "add
// repository" picker use each instance's review bot or review App (the
// platform's Forge); switching a repository on or off asks the engine for
// a reconcile (reconcileSoon). It warns when no admins are configured.
func newDashboard(cfg *config.Config, st *store.Store, secretKey string, lib inputs.Library,
	platforms map[string]engine.Platform, reconcileSoon func(), log *slog.Logger) (*dashboard.Dashboard, error) {
	if !cfg.DashboardEnabled() {
		return nil, nil
	}
	sources, catalogs := dashboardSources(platforms, log)
	d, err := dashboard.New(dashboard.Options{
		Config: cfg, Store: st, SecretKey: secretKey, Log: log, Library: lib,
		Sources: sources, Catalogs: catalogs, ReconcileSoon: reconcileSoon,
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// dashboardHandler is d's routes, or nil (no dashboard) when d is nil.
func dashboardHandler(d *dashboard.Dashboard) http.Handler {
	if d == nil {
		return nil
	}
	return d.Handler()
}

// dashboardSources returns, by instance, the platforms' Forges that read
// repositories (Skills & instructions) and list the repositories the review
// bot or App can set up (the "add repository" picker). gitlabforge.Forge
// and githubforge.Forge are both.
func dashboardSources(platforms map[string]engine.Platform, log *slog.Logger) (map[string]dashboard.RepoSource, map[string]dashboard.RepoCatalog) {
	sources := make(map[string]dashboard.RepoSource, len(platforms))
	catalogs := make(map[string]dashboard.RepoCatalog, len(platforms))
	for name, p := range platforms {
		var missing []string
		if s, ok := p.Forge.(dashboard.RepoSource); ok {
			sources[name] = s
		} else {
			missing = append(missing, "Skills & instructions")
		}
		if c, ok := p.Forge.(dashboard.RepoCatalog); ok {
			catalogs[name] = c
		} else {
			missing = append(missing, "the add-repository picker")
		}
		if len(missing) > 0 {
			log.Warn("dashboard: the instance's review side can't serve "+strings.Join(missing, " or ")+"; they answer 502 there",
				"instance", name, "platform", p.Kind)
		}
	}
	return sources, catalogs
}

// pruneSessions returns a pass that deletes the dashboard sessions that can
// no longer be used (prune is dashboard.Auth.PruneSessions). It only
// touches the database; a failure is logged and tried again next time.
func pruneSessions(prune func(context.Context, time.Time) (int, error), now func() time.Time, log *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		n, err := prune(ctx, now())
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("couldn't prune dashboard sessions", "error", err.Error())
		case n > 0:
			log.Info("pruned dashboard sessions", "sessions", n)
		}
	}
}

// every runs f at once and then every interval, until ctx is done.
func every(ctx context.Context, interval time.Duration, f func(context.Context)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		f(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// together runs loops side by side and returns when all have returned.
func together(loops ...func(context.Context)) func(context.Context) {
	return func(ctx context.Context) {
		var wg sync.WaitGroup
		for _, loop := range loops {
			wg.Add(1)
			go func() {
				defer wg.Done()
				loop(ctx)
			}()
		}
		wg.Wait()
	}
}

// serveUntilDone serves handler on ln and runs loops next to it until ctx is
// done or the server fails. Then the server shuts down (20 s for requests in
// flight) and only after that are the loops stopped and waited for, so a
// result received during shutdown is still woken for.
func serveUntilDone(ctx context.Context, ln net.Listener, handler http.Handler, loops func(context.Context), log *slog.Logger) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      writeTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	loopCtx, stopLoops := context.WithCancel(context.WithoutCancel(ctx))
	defer stopLoops()
	loopsDone := make(chan struct{})
	go func() {
		loops(loopCtx)
		close(loopsDone)
	}()

	var serveErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-served:
		serveErr = fmt.Errorf("http server: %w", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("http server didn't shut down cleanly", "error", err.Error())
	}
	stopLoops()
	<-loopsDone
	return serveErr
}

// checkSkills makes sure every skill a repository picks is in the library.
func checkSkills(cfg *config.Config, lib inputs.Library) error {
	var errs []error
	for _, repo := range cfg.Repositories {
		for _, name := range repo.Skills {
			if _, ok := lib[name]; !ok {
				errs = append(errs, fmt.Errorf("repository %s on instance %q: skill %q is not in the skill library %s",
					repo.Project, repo.Instance(), name, cfg.SkillsDir))
			}
		}
	}
	return errors.Join(errs...)
}

// connectGitLab builds the review and approve clients of every instance and
// checks each token by asking who it belongs to; the two must be different
// users. It returns the platforms and the webhook secrets by instance.
func connectGitLab(ctx context.Context, cfg *config.Config) (map[string]engine.Platform, map[string]string, error) {
	platforms := make(map[string]engine.Platform, len(cfg.GitLab))
	secrets := make(map[string]string, len(cfg.GitLab))
	for _, inst := range cfg.GitLab {
		reviewToken, approveToken, hookSecret, err := inst.Secrets()
		if err != nil {
			return nil, nil, err
		}
		review := gitlab.New(inst.URL, reviewToken, nil)
		approve := gitlab.New(inst.URL, approveToken, nil)
		reviewUser, err := review.CurrentUser(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("gitlab instance %q: the review token (%s) doesn't work: %w", inst.Name, inst.ReviewTokenEnv, err)
		}
		approveUser, err := approve.CurrentUser(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("gitlab instance %q: the approve token (%s) doesn't work: %w", inst.Name, inst.ApproveTokenEnv, err)
		}
		if reviewUser.ID == approveUser.ID {
			// One user would approve the reviews it posts itself: the
			// approval has to come from a second identity.
			return nil, nil, fmt.Errorf("gitlab instance %q: the review token (%s) and the approve token (%s) belong to the same user; the approve bot must be a separate user",
				inst.Name, inst.ReviewTokenEnv, inst.ApproveTokenEnv)
		}
		platforms[inst.Name] = engine.Platform{
			Kind:     config.PlatformGitLab,
			Forge:    gitlabforge.NewForge(review, reviewUser),
			Approver: gitlabforge.NewApprover(approve, approveUser),
			GitLab:   review,
		}
		secrets[inst.Name] = hookSecret
	}
	return platforms, secrets, nil
}

// connectGitHub builds every GitHub instance's review App and approve side
// (the approve App, or a bot user's token) and looks up their bot users once:
// the review App's comments and the approve side's reviews are found by
// them. A key, App or token that doesn't work is an error naming the
// instance and what failed, never the secret. repos resolves an instance's
// repository IDs; hc is the HTTP client (nil: the default). It returns the
// platforms and the review App webhook secrets by instance.
func connectGitHub(ctx context.Context, cfg *config.Config, repos func(instance string) githubforge.Repos, hc *http.Client, log *slog.Logger) (map[string]engine.Platform, map[string]string, error) {
	platforms := make(map[string]engine.Platform, len(cfg.GitHub))
	secrets := make(map[string]string, len(cfg.GitHub))
	for _, inst := range cfg.GitHub {
		reviewKey, approveSecret, hookSecret, err := inst.Secrets()
		if err != nil {
			return nil, nil, err
		}
		reviewApp, reviewBot, err := connectApp(ctx, inst.APIURL, inst.ReviewApp.AppID, reviewKey, hc)
		if err != nil {
			return nil, nil, fmt.Errorf("github instance %q: the review App (app_id %d) doesn't work: %w",
				inst.Name, inst.ReviewApp.AppID, err)
		}
		instRepos := repos(inst.Name)

		var approver *githubforge.Approver
		if inst.ApproveApp != nil {
			approveApp, approveBot, err := connectApp(ctx, inst.APIURL, inst.ApproveApp.AppID, approveSecret, hc)
			if err != nil {
				return nil, nil, fmt.Errorf("github instance %q: the approve App (app_id %d) doesn't work: %w",
					inst.Name, inst.ApproveApp.AppID, err)
			}
			approver = githubforge.NewAppApprover(approveApp, approveBot, instRepos)
		} else {
			client := github.NewTokenClient(inst.APIURL, approveSecret, hc)
			user, err := client.CurrentUser(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("github instance %q: the approve token (%s) doesn't work: %w",
					inst.Name, inst.ApproveTokenEnv, err)
			}
			approver = githubforge.NewTokenApprover(client, user.ID, instRepos, log.With("instance", inst.Name))
		}

		platforms[inst.Name] = engine.Platform{
			Kind:     config.PlatformGitHub,
			Forge:    githubforge.NewForge(reviewApp, reviewBot, instRepos),
			Approver: approver,
			GitHub:   &engine.GitHub{App: reviewApp, Approve: approver},
		}
		secrets[inst.Name] = hookSecret
	}
	return platforms, secrets, nil
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

// kubeConfig is the in-cluster config or, outside a cluster, the kubeconfig
// KUBECONFIG names (handy for running pergent-sh locally).
func kubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, rest.ErrNotInCluster) && os.Getenv("KUBECONFIG") != "" {
		cfg, kerr := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if kerr != nil {
			return nil, fmt.Errorf("kubernetes config from KUBECONFIG: %w", kerr)
		}
		return cfg, nil
	}
	return nil, fmt.Errorf("kubernetes config: %w", err)
}

// requireEnv reads an environment variable that must be set and not empty.
func requireEnv(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("environment variable %s is not set or empty", name)
}
