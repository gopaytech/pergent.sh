// Command pergent-sh runs pergent code reviews on GitLab merge requests and
// GitHub pull requests as Kubernetes Jobs.
//
//	pergent-sh serve [--config /etc/pergent-sh/config.yaml]
//	pergent-sh admin reset-password <username>
//	pergent-sh fetch-skills --dir <clone> --path <folder> --commit-file <file> --error-file <file> --result-url <url>
//	pergent-sh scan-repo --dir <clone> --commit-file <file> --error-file <file> --input-file <file> --result-url <url>
//
// serve needs DATABASE_URL and PERGENT_SH_SECRET_KEY in the environment, plus
// the LLM key, the GitLab tokens, the GitHub App private keys (or approve
// token) and the webhook secrets the config file names. With dashboard.url
// set it also serves the dashboard.
//
// admin reset-password gives a dashboard password account a new random
// password, printed on stdout, which must be changed at the next sign-in. It
// needs only DATABASE_URL.
//
// fetch-skills runs in a skill fetch Job, not in the pergent.sh pod: it
// posts the skills of a clone of a public URL back to pergent.sh. It needs
// only PERGENT_SH_FETCH_TOKEN.
//
// scan-repo runs in a readiness scan Job, likewise: it scans a clone of one
// repository's default branch and posts what it found back to pergent.sh. It
// needs only PERGENT_SH_SCAN_TOKEN; its input is the file --input-file names.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/gopaytech/pergent.sh/internal/callback"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/engine"
	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/server"
	"github.com/gopaytech/pergent.sh/internal/skillfetch"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
	"github.com/gopaytech/pergent.sh/internal/webhook"
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
	// auditPruneEvery is how often the audit entries older than
	// auditRetention are deleted.
	auditPruneEvery = time.Hour
	auditRetention  = 90 * 24 * time.Hour
	// commandPruneEvery is how often the finished comment commands older
	// than commandRetention are deleted.
	commandPruneEvery = time.Hour
	commandRetention  = 90 * 24 * time.Hour
	// fetchSweepEvery is how often skill fetches that never ended are failed
	// and expired ones deleted.
	fetchSweepEvery = time.Minute
	usage           = "usage: pergent-sh serve [--config <path>]\n       pergent-sh admin reset-password <username>\n" +
		"       pergent-sh fetch-skills --dir <clone> [--path <folder>] --commit-file <file> --error-file <file> --result-url <url>\n" +
		"       pergent-sh scan-repo --dir <clone> --commit-file <file> --error-file <file> --input-file <file> --result-url <url>"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cmd, err := parseArgs(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	switch cmd.name {
	case "reset-password":
		os.Exit(resetPassword(ctx, cmd.username, os.Stdout, os.Stderr))
	case "fetch-skills":
		os.Exit(runFetchSkills(ctx, cmd.fetch, os.Stderr))
	case "scan-repo":
		os.Exit(runScanRepo(ctx, cmd.scan, os.Stderr))
	}
	if err := serve(ctx, cmd.configPath, log); err != nil {
		log.Error("pergent-sh stopped", "error", err.Error())
		os.Exit(1)
	}
}

// command is what the arguments ask for: name "serve", with configPath,
// "reset-password", with username, "fetch-skills", with fetch, or "scan-repo",
// with scan.
type command struct {
	name       string
	configPath string
	username   string
	fetch      fetchOptions
	scan       scanOptions
}

// parseArgs reads "serve [--config <path>]", "admin reset-password
// <username>", "fetch-skills <flags>" or "scan-repo <flags>".
func parseArgs(args []string, stderr io.Writer) (cmd command, err error) {
	if len(args) == 0 {
		return command{}, errors.New(usage)
	}
	switch args[0] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(stderr)
		path := fs.String("config", defaultConfigPath, "path to the config file")
		if err := fs.Parse(args[1:]); err != nil {
			return command{}, err
		}
		if fs.NArg() > 0 {
			return command{}, fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), usage)
		}
		return command{name: "serve", configPath: *path}, nil
	case "admin":
		if len(args) != 3 || args[1] != "reset-password" || args[2] == "" {
			return command{}, errors.New(usage)
		}
		return command{name: "reset-password", username: args[2]}, nil
	case "fetch-skills":
		o, err := parseFetchArgs(args[1:], stderr)
		if err != nil {
			return command{}, err
		}
		return command{name: "fetch-skills", fetch: o}, nil
	case "scan-repo":
		o, err := parseScanArgs(args[1:], stderr)
		if err != nil {
			return command{}, err
		}
		return command{name: "scan-repo", scan: o}, nil
	}
	return command{}, errors.New(usage)
}

// warnTimeouts logs, once each, the timeouts in cfg that are short for their
// step limit. They are not errors: the review may still finish in time.
func warnTimeouts(cfg *config.Config, log *slog.Logger) {
	for _, w := range config.TimeoutWarnings(cfg) {
		log.Warn("job timeout may be too short", "config", w)
	}
}

// serve starts pergent.sh and runs it until ctx is done. Anything wrong with
// the config, the secrets, the database, or a GitLab token or GitHub App or
// token of an instance in the config stops it before it serves; secrets are
// never part of the error. An instance added in the dashboard that can't be
// connected is logged and shown in "Needs attention" instead.
func serve(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	warnTimeouts(cfg, log)

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
	// The config's llm: block must have its key; an LLM set in the dashboard
	// is read from the database below.
	if _, err := readLLMKey(cfg); err != nil {
		return err
	}

	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	// Dismissals made before their finding's details were stored get them
	// from the stored results. Not a migration: a result of an odd shape
	// must not keep the server from starting, and neither may a slow one: it
	// is given 2 minutes, and one that does not finish is tried again at the next start.
	backfillCtx, cancelBackfill := context.WithTimeout(ctx, 2*time.Minute)
	n, err := st.BackfillDismissals(backfillCtx, time.Now().AddDate(0, 0, -90))
	cancelBackfill()
	if err != nil {
		log.Error("filling in the details of earlier dismissals failed", "error", err.Error())
	} else if n > 0 {
		log.Info("filled in the details of earlier dismissals", "count", n)
	}
	if err := checkSkills(ctx, cfg, st, log); err != nil {
		return err
	}
	if cfg.DashboardEnabled() {
		if err := dashboard.Bootstrap(ctx, st, cfg, time.Now(), log); err != nil {
			return err
		}
	}

	reg := instances.New(cfg, st, box)
	reg.SetLog(log)
	if err := reg.LoadLLM(ctx); err != nil {
		return err
	}
	conn := &connector{
		reg: reg, store: st, log: log,
		platforms:   syncmap.New[engine.Platform](nil),
		gitlabHooks: syncmap.New[string](nil),
		githubHooks: syncmap.New[string](nil),
		githubRepos: func(instance string) githubforge.Repos { return engine.GitHubRepos(st, instance) },
	}
	if cfg.DashboardEnabled() {
		conn.sources = syncmap.New[dashboard.RepoSource](nil)
		conn.catalogs = syncmap.New[dashboard.RepoCatalog](nil)
	}
	restCfg, err := kubeConfig()
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	launcher := &kube.Launcher{Client: clientset, Namespace: cfg.Jobs.Namespace}
	eng := &engine.Engine{
		Store:          st,
		Config:         cfg,
		Instances:      reg,
		Platforms:      conn.platforms,
		WebhookSecrets: conn.gitlabHooks,
		Launcher:       launcher,
		Box:            box,
		Log:            log,
		// An instance removed in the dashboard is taken out of use once the
		// engine has finished removing it.
		OnInstanceRemoved: conn.Disconnect,
	}
	var (
		fetcher      *skillfetch.Fetcher
		fetchResults callback.FetchResults
		scanResults  callback.ScanResults
	)
	if cfg.DashboardEnabled() {
		// The readiness scans need the dashboard too (it shows and asks for
		// them), and the same image to send their result back.
		eng.Scans = true
		eng.ScanImage = os.Getenv("PERGENT_SH_IMAGE")
		scanResults = eng
		// Skills are installed from a connected instance's repositories with
		// its review bot or App, the reader the dashboard has for it, and
		// from a public URL by a Job, which needs pergent.sh's own image
		// (PERGENT_SH_IMAGE, set by the chart) to send them back.
		fetcher = &skillfetch.Fetcher{Store: st, Readers: conn.sourceReader, Secrets: reg.HeldSecrets,
			CallbackBase: cfg.CallbackURL, Now: time.Now, Log: log,
			Launcher: launcher, Namespace: cfg.Jobs.Namespace, GitImage: cfg.Jobs.Image, Image: os.Getenv("PERGENT_SH_IMAGE")}
		fetchResults = fetcher
	}
	dash, err := newDashboard(dashboard.Options{
		Config: cfg, Instances: reg, Connector: conn, Store: st, SecretKey: key, Log: log,
		Sources: conn.sources, Catalogs: conn.catalogs, ReconcileSoon: eng.ReconcileSoon,
		// The engine queues the readiness scans (eng.Scans is on whenever
		// the dashboard is) and rechecks them after a skill setting is saved.
		Wake: eng.Wake, Fetcher: fetcher, Readiness: eng,
	})
	if err != nil {
		return err
	}
	// Connecting reaches GitLab and GitHub: a config instance that doesn't
	// work stops startup; one added in the dashboard is recorded instead.
	if err := conn.connectAll(ctx); err != nil {
		return err
	}
	handler := server.Routes(server.Deps{
		Config: cfg, Instances: reg, WebhookSecrets: conn.gitlabHooks, GitHubWebhookSecrets: conn.githubHooks,
		Store: st, Bots: commandBots(conn.platforms), Wake: eng.Wake, Dashboard: dashboardHandler(dash),
		SkillFetches: fetchResults, ReadinessScans: scanResults, Log: log,
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
	// The audit log is kept 90 days, whether the dashboard is on or not
	// (the reset-password command records there too).
	pruneOld := pruneAudit(st.PruneAudit, time.Now, log)
	loops = append(loops, func(ctx context.Context) { every(ctx, auditPruneEvery, pruneOld) })
	pruneCmds := pruneCommands(st.PruneCommands, time.Now, log)
	loops = append(loops, func(ctx context.Context) { every(ctx, commandPruneEvery, pruneCmds) })
	pruneHooks := pruneDeliveries(st.PruneDeliveries, time.Now, log)
	loops = append(loops, func(ctx context.Context) { every(ctx, commandPruneEvery, pruneHooks) })
	if eng.Scans {
		loops = append(loops, eng.RunScans)
	}
	if dash != nil {
		prune := pruneSessions(dash.Auth.PruneSessions, time.Now, log)
		loops = append(loops, func(ctx context.Context) { every(ctx, sessionPruneEvery, prune) })
		sweep := sweepFetches(fetcher.Sweep, log)
		loops = append(loops, func(ctx context.Context) { every(ctx, fetchSweepEvery, sweep) })
	}
	return serveUntilDone(ctx, ln, handler, together(loops...), log)
}

// newDashboard builds the dashboard when dashboard.url is set, and returns
// nil, nil when it isn't. Its Skills & instructions view and "add
// repository" picker use each instance's review bot or review App (the
// platform's Forge), which the connector gives it as instances connect;
// switching a repository on or off asks the engine for a reconcile of that
// repository (o.ReconcileSoon).
func newDashboard(o dashboard.Options) (*dashboard.Dashboard, error) {
	if !o.Config.DashboardEnabled() {
		return nil, nil
	}
	return dashboard.New(o)
}

// dashboardHandler is d's routes, or nil (no dashboard) when d is nil.
func dashboardHandler(d *dashboard.Dashboard) http.Handler {
	if d == nil {
		return nil
	}
	return d.Handler()
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

// sweepFetches returns a pass that fails the skill fetches that never ended
// and deletes the expired ones (sweep is skillfetch.Fetcher.Sweep). It only
// touches the database; a failure is logged and tried again next time.
func sweepFetches(sweep func(context.Context) error, log *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		if err := sweep(ctx); err != nil && ctx.Err() == nil {
			log.Warn("couldn't sweep the skill fetches", "error", err.Error())
		}
	}
}

// pruneCommands returns a pass that deletes the finished comment commands
// older than commandRetention (prune is store.Store.PruneCommands). It only
// touches the database; a failure is logged and tried again next time.
func pruneCommands(prune func(context.Context, time.Time) (int, error), now func() time.Time, log *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		n, err := prune(ctx, now().Add(-commandRetention))
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("couldn't prune the comment commands", "error", err.Error())
		case n > 0:
			log.Info("pruned the comment commands", "commands", n)
		}
	}
}

// pruneDeliveries returns a pass that deletes the webhook deliveries too old
// to be accepted (prune is store.Store.PruneDeliveries). It only touches the
// database; a failure is logged and tried again next time.
func pruneDeliveries(prune func(context.Context, time.Time) (int, error), now func() time.Time, log *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		n, err := prune(ctx, now().Add(-webhook.DeliveryRetention))
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("couldn't prune the webhook deliveries", "error", err.Error())
		case n > 0:
			log.Info("pruned the webhook deliveries", "deliveries", n)
		}
	}
}

// pruneAudit returns a pass that deletes the audit entries older than
// auditRetention (prune is store.Store.PruneAudit). It only touches the
// database; a failure is logged and tried again next time.
func pruneAudit(prune func(context.Context, time.Time) (int, error), now func() time.Time, log *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		n, err := prune(ctx, now().Add(-auditRetention))
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("couldn't prune the audit log", "error", err.Error())
		case n > 0:
			log.Info("pruned the audit log", "entries", n)
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

// attSkillMissing is the "Needs attention" kind of a repository that names a
// skill the library lacks (the engine records the same kind).
const attSkillMissing = "skill_missing"

// skillChecker is the part of the store checkSkills reads and writes.
type skillChecker interface {
	ResolveLibrary(ctx context.Context, names []string) (store.ResolvedLibrary, error)
	SetAttention(ctx context.Context, instance, project, kind, message string) error
}

// checkSkills looks for a skill a config repository names that the library
// lacks and lists it under "Needs attention" (skill_missing, the reason the
// engine gives when a run fails for it). It never stops startup: the library
// is edited in the dashboard, so a skill can be added later, and a repository
// whose skill is missing fails its own runs. A store error is logged.
func checkSkills(ctx context.Context, cfg *config.Config, st skillChecker, log *slog.Logger) error {
	for _, repo := range cfg.Repositories {
		if repo.Skills == nil {
			continue // the default list is checked when the admin saves it
		}
		resolved, err := st.ResolveLibrary(ctx, repo.Skills)
		if err != nil {
			log.Warn("couldn't check the skills of a repository", "instance", repo.Instance(), "project", repo.Project, "error", err.Error())
			continue
		}
		if len(resolved.Missing) == 0 {
			continue
		}
		reason := fmt.Sprintf("skill %q is not in the skill library", resolved.Missing[0])
		log.Warn("a repository names a skill the library lacks", "instance", repo.Instance(), "project", repo.Project, "skill", resolved.Missing[0])
		if err := st.SetAttention(ctx, repo.Instance(), repo.Project, attSkillMissing, reason); err != nil {
			log.Warn("couldn't record a missing skill for the dashboard", "instance", repo.Instance(), "project", repo.Project, "error", err.Error())
		}
	}
	return nil
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

// readLLMKey returns the key of the llm: block, or "" when the file has none
// (the LLM is then added in the dashboard).
func readLLMKey(cfg *config.Config) (string, error) {
	if !cfg.HasLLM() {
		return "", nil
	}
	key, err := requireEnv(cfg.LLM.APIKeyEnv)
	if err != nil {
		return "", fmt.Errorf("llm.api_key_env: %w", err)
	}
	return key, nil
}

// requireEnv reads an environment variable that must be set and not empty.
func requireEnv(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("environment variable %s is not set or empty", name)
}
