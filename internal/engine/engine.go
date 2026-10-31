// Package engine runs pergent.sh's background work: it dispatches queued runs
// as review Jobs, publishes their results, cleans up superseded Jobs and
// watches running ones. Anything that stops a review becomes a failure reason
// the publisher posts, never silence.
package engine

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// Platform is one configured instance as the engine reaches it. Kind is
// config.PlatformGitLab or config.PlatformGitHub. Forge is the review bot and
// Approver the approve bot; their tokens can comment, set statuses and
// approve, so they never reach a Job. GitLab is the review bot's own client on
// a GitLab instance, for reconcile's webhooks and deploy tokens; GitHub is a
// GitHub instance's review App and approve side, for reconcile and the
// per-Job clone tokens. Reviewing, publishing, cleaning up and the approval
// sweep go through Forge and Approver alone.
type Platform struct {
	Kind     string
	Forge    forge.Forge
	Approver forge.Approver
	GitLab   *gitlab.Client
	GitHub   *GitHub
}

// GitHub is what a GitHub instance needs beyond Forge and Approver.
type GitHub struct {
	// App is the review App: it finds its installation on a repository,
	// reads the repository and mints each Job's clone token.
	App *github.App
	// Approve tells whether the approve side reaches a repository.
	Approve ApproveReach
}

// ApproveReach is the approve side's reachability check
// (githubforge.Approver.CanReach).
type ApproveReach interface {
	CanReach(ctx context.Context, owner, name string) (bool, error)
}

// isGitHub reports whether the platform is a GitHub instance.
func (p Platform) isGitHub() bool { return p.Kind == config.PlatformGitHub }

// Engine holds what the loops share. Platforms is keyed by instance name; it
// and WebhookSecrets change while the engine runs, as instances are
// connected and disconnected.
type Engine struct {
	Store  *store.Store
	Config *config.Config
	// Instances tells which instances exist (the config's and the
	// dashboard's) and their platform; nil means the config's alone.
	Instances *instances.Registry
	Platforms *syncmap.Map[Platform]
	// WebhookSecrets maps an instance name to the secret its project hooks
	// are created with.
	WebhookSecrets *syncmap.Map[string]
	Launcher       *kube.Launcher
	Library        inputs.Library
	Box            *secretbox.Box
	// LLMKey is the model provider's API key, handed to each Job's Secret.
	LLMKey string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log may be nil.
	Log *slog.Logger

	registryOnce sync.Once

	wakeOnce sync.Once
	wake     chan struct{}

	reconcileOnce sync.Once
	reconcileReq  chan struct{}
	// soonMu guards soonKeys, the repositories ReconcileSoon asked for that
	// the loop hasn't reconciled yet.
	soonMu   sync.Mutex
	soonKeys map[repoKey]RepoKey

	// unchecked holds the repositories whose required approvals couldn't
	// be read, so that is logged once (see checkRequiredApprovals).
	uncheckedMu sync.Mutex
	unchecked   map[repoKey]bool

	// warned holds when each repeated warning was last logged (warnEvery).
	warnMu sync.Mutex
	warned map[string]time.Time

	// logTried holds the runs whose Job log was taken or tried (see
	// captureLog), with when, so each is tried once.
	logMu    sync.Mutex
	logTried map[string]time.Time
}

// registry is Instances, set to the config's instances alone when it was nil.
func (e *Engine) registry() *instances.Registry {
	e.registryOnce.Do(func() {
		if e.Instances == nil {
			e.Instances = instances.New(e.Config, nil, nil)
		}
	})
	return e.Instances
}

// Wake asks the loops for an early pass. It never blocks; wake-ups that
// arrive while one is pending are merged into it.
func (e *Engine) Wake() {
	select {
	case e.wakeCh() <- struct{}{}:
	default:
	}
}

// wakeCh is the wake-up channel, created on first use with room for one.
func (e *Engine) wakeCh() chan struct{} {
	e.wakeOnce.Do(func() { e.wake = make(chan struct{}, 1) })
	return e.wake
}

// ReconcileSoon asks the loop to reconcile the repository project on
// instance ahead of the hourly Reconcile: the dashboard calls it when a
// repository is switched on or off. Only that repository is reconciled
// (ReconcileRepos), never all of them. It never blocks; requests that
// arrive while one is pending are merged into it, their repositories
// together. The loop starts such a reconcile at most once every 10 s
// (reconcileSoonGap): one asked for sooner waits for the gap to pass.
func (e *Engine) ReconcileSoon(instance, project string) {
	e.soonMu.Lock()
	if e.soonKeys == nil {
		e.soonKeys = map[repoKey]RepoKey{}
	}
	e.soonKeys[keyOf(instance, project)] = RepoKey{Instance: instance, Project: project}
	e.soonMu.Unlock()
	select {
	case e.reconcileCh() <- struct{}{}:
	default:
	}
}

// takeSoonKeys returns the repositories ReconcileSoon asked for since the
// last call, and forgets them.
func (e *Engine) takeSoonKeys() []RepoKey {
	e.soonMu.Lock()
	defer e.soonMu.Unlock()
	keys := make([]RepoKey, 0, len(e.soonKeys))
	for _, k := range e.soonKeys {
		keys = append(keys, k)
	}
	e.soonKeys = nil
	return keys
}

// reconcileCh is the reconcile request channel, created on first use with
// room for one.
func (e *Engine) reconcileCh() chan struct{} {
	e.reconcileOnce.Do(func() { e.reconcileReq = make(chan struct{}, 1) })
	return e.reconcileReq
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// warnEvery logs msg at Warn unless a warning under key was logged within
// the last minute, so a condition that lasts is not logged every pass.
func (e *Engine) warnEvery(key, msg string, args ...any) {
	e.warnMu.Lock()
	now := e.now()
	if last, ok := e.warned[key]; ok && now.Sub(last) < warnGap && !now.Before(last) {
		e.warnMu.Unlock()
		return
	}
	if e.warned == nil {
		e.warned = map[string]time.Time{}
	}
	e.warned[key] = now
	e.warnMu.Unlock()
	e.log().Warn(msg, args...)
}

// warnGap is the least time between two warnings under one key (warnEvery).
const warnGap = time.Minute

// Loop intervals of Run. They are variables so tests can shorten them.
var (
	passInterval  = 2 * time.Second
	watchInterval = 15 * time.Second
	// sweepInterval is the approval sweep's. The sweep reads every approved
	// change from its platform, so it doesn't run on the watch tick: about
	// 20 approved pull requests would use up GitHub's rate limit.
	sweepInterval     = 5 * time.Minute
	reconcileInterval = time.Hour
	// reconcileSoonGap is the least time between two reconciles
	// ReconcileSoon starts; requests in between are merged into one.
	reconcileSoonGap = 10 * time.Second
)

// Run runs the loops until ctx is done: dispatch, publish and cleanup every
// 2 s or on Wake, watch every 15 s, sweep the approvals every 5 minutes and
// reconcile every hour (when it also prunes the review logs older than
// 30 days), and reconcile the repositories ReconcileSoon names (at most
// once every 10 s: requests within that gap of the last such reconcile make
// one more when it ends, of all their repositories). They all run in
// this one goroutine, one after another, so no two of them ever overlap (a
// publish and a cleanup of the same merge request least of all). Errors are
// logged and never stop the loop. Before the first pass, runs stranded
// between claim and dispatch by an earlier stop go back to the queue (with
// one replica, nothing is being dispatched yet), then Reconcile and
// SweepApprovals run once.
// Run is started next to the HTTP server, never before it, so a slow GitLab
// at startup delays only the loops.
func (e *Engine) Run(ctx context.Context) {
	log := e.log()
	if n, err := e.Store.RecoverUnlaunched(ctx, e.now()); err != nil {
		log.Error("couldn't requeue runs stranded before their job started", "error", e.redact(err.Error()))
	} else if n > 0 {
		log.Warn("requeued runs stranded before their job started", "runs", n)
	}
	if err := e.Reconcile(ctx); err != nil && ctx.Err() == nil {
		// Each failure is logged already; the hourly pass tries again.
		log.Error("startup reconcile failed", "error", e.redact(err.Error()))
	}
	e.sweepApprovals(ctx)
	e.pruneRunLogs(ctx)

	pass := time.NewTicker(passInterval)
	defer pass.Stop()
	watch := time.NewTicker(watchInterval)
	defer watch.Stop()
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()
	reconcile := time.NewTicker(reconcileInterval)
	defer reconcile.Stop()

	// Dashboard requests (ReconcileSoon): the last one's start and, while
	// one waits for the gap to pass, its timer.
	var (
		lastSoon  time.Time
		soonTimer *time.Timer
		soon      <-chan time.Time
	)
	defer func() {
		if soonTimer != nil {
			soonTimer.Stop()
		}
	}()
	reconcileNow := func() {
		keys := e.takeSoonKeys()
		if len(keys) == 0 {
			return
		}
		if err := e.ReconcileRepos(ctx, keys); err != nil && ctx.Err() == nil {
			log.Error("reconcile failed", "error", e.redact(err.Error()))
		}
	}

	e.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-pass.C:
			e.pass(ctx)
		case <-e.wakeCh():
			e.pass(ctx)
		case <-watch.C:
			if err := e.WatchOnce(ctx); err != nil && ctx.Err() == nil {
				log.Error("watch failed", "error", e.redact(err.Error()))
			}
		case <-sweep.C:
			e.sweepApprovals(ctx)
		case <-reconcile.C:
			if err := e.Reconcile(ctx); err != nil && ctx.Err() == nil {
				log.Error("reconcile failed", "error", e.redact(err.Error()))
			}
			e.pruneRunLogs(ctx)
		case <-e.reconcileCh():
			// Within the gap of the last one, wait for its end; requests
			// arriving meanwhile are merged into that one.
			if wait := reconcileSoonGap - time.Since(lastSoon); !lastSoon.IsZero() && wait > 0 {
				if soonTimer == nil {
					soonTimer = time.NewTimer(wait)
					soon = soonTimer.C
				}
				continue
			}
			lastSoon = time.Now()
			reconcileNow()
		case <-soon:
			soonTimer, soon = nil, nil
			lastSoon = time.Now()
			reconcileNow()
		}
	}
}

// sweepApprovals runs SweepApprovals, logging a failure.
func (e *Engine) sweepApprovals(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if err := e.SweepApprovals(ctx); err != nil && ctx.Err() == nil {
		e.log().Error("approval sweep failed", "error", e.redact(err.Error()))
	}
}

// pass dispatches, publishes and cleans up once, logging failures.
func (e *Engine) pass(ctx context.Context) {
	for _, step := range []struct {
		name string
		fn   func(context.Context) error
	}{
		{"dispatch", e.DispatchOnce},
		{"publish", e.PublishOnce},
		{"cleanup", e.CleanupOnce},
	} {
		if ctx.Err() != nil {
			return
		}
		if err := step.fn(ctx); err != nil && ctx.Err() == nil {
			e.log().Error(step.name+" failed", "error", e.redact(err.Error()))
		}
	}
}
