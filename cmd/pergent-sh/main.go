// Command pergent-sh runs pergent code reviews on GitLab merge requests as
// Kubernetes Jobs.
//
//	pergent-sh serve [--config /etc/pergent-sh/config.yaml]
//
// It needs DATABASE_URL and PERGENT_SH_SECRET_KEY in the environment, plus the
// LLM key and the GitLab tokens and webhook secrets the config file names.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/gopaytech/pergent.sh/internal/callback"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/engine"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/webhook"
)

const (
	defaultConfigPath = "/etc/pergent-sh/config.yaml"
	shutdownTimeout   = 20 * time.Second
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
// the config, the secrets, the database or a GitLab token stops it before it
// serves; secrets are never part of the error.
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

	bots, hookSecrets, err := connectGitLab(ctx, cfg)
	if err != nil {
		return err
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
		Bots:           bots,
		WebhookSecrets: hookSecrets,
		Launcher:       &kube.Launcher{Client: clientset, Namespace: cfg.Jobs.Namespace},
		Library:        lib,
		Box:            box,
		LLMKey:         llmKey,
		Log:            log,
	}
	if err := eng.Reconcile(ctx); err != nil {
		// Each failure is logged already; the repositories that did
		// reconcile are served, and the hourly pass tries the rest again.
		log.Warn("startup reconcile didn't finish for every repository; serving anyway")
	}

	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/gitlab/{instance}", &webhook.Handler{
		Config: cfg, Secrets: hookSecrets, Queue: st, Wake: eng.Wake, Log: log,
	})
	mux.Handle("POST /runs/{id}/result", &callback.Handler{Results: st, Wake: eng.Wake, Log: log})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// The loops outlive the signal until the HTTP server has drained, so a
	// result received during shutdown is still woken for.
	loopCtx, stopLoops := context.WithCancel(context.WithoutCancel(ctx))
	defer stopLoops()
	loopsDone := make(chan struct{})
	go func() {
		eng.Run(loopCtx)
		close(loopsDone)
	}()

	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	log.Info("pergent-sh serving", "listen", cfg.Listen, "repositories", len(cfg.Repositories))

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
				errs = append(errs, fmt.Errorf("repository %s on gitlab instance %q: skill %q is not in the skill library %s",
					repo.Project, repo.GitLab, name, cfg.SkillsDir))
			}
		}
	}
	return errors.Join(errs...)
}

// connectGitLab builds the review and approve clients of every instance and
// checks each token by asking who it belongs to. It returns the bots and the
// webhook secrets by instance.
func connectGitLab(ctx context.Context, cfg *config.Config) (map[string]engine.Bots, map[string]string, error) {
	bots := make(map[string]engine.Bots, len(cfg.GitLab))
	secrets := make(map[string]string, len(cfg.GitLab))
	for _, inst := range cfg.GitLab {
		review, approve, hookSecret, err := inst.Secrets()
		if err != nil {
			return nil, nil, err
		}
		b := engine.Bots{
			Review:  gitlab.New(inst.URL, review, nil),
			Approve: gitlab.New(inst.URL, approve, nil),
		}
		if b.ReviewUser, err = b.Review.CurrentUser(ctx); err != nil {
			return nil, nil, fmt.Errorf("gitlab instance %q: the review token (%s) doesn't work: %w", inst.Name, inst.ReviewTokenEnv, err)
		}
		if b.ApproveUser, err = b.Approve.CurrentUser(ctx); err != nil {
			return nil, nil, fmt.Errorf("gitlab instance %q: the approve token (%s) doesn't work: %w", inst.Name, inst.ApproveTokenEnv, err)
		}
		bots[inst.Name] = b
		secrets[inst.Name] = hookSecret
	}
	return bots, secrets, nil
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
