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
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Bots are the two GitLab users of one instance. Their tokens can comment,
// set statuses and approve, so they never reach a Job. ReviewUser and
// ApproveUser are the users the Review and Approve tokens belong to.
type Bots struct {
	Review, Approve         *gitlab.Client
	ReviewUser, ApproveUser gitlab.User
}

// Engine holds what the loops share. Bots is keyed by instance name.
type Engine struct {
	Store  *store.Store
	Config *config.Config
	Bots   map[string]Bots
	// WebhookSecrets maps an instance name to the secret its project hooks
	// are created with.
	WebhookSecrets map[string]string
	Launcher       *kube.Launcher
	Library        inputs.Library
	Box            *secretbox.Box
	// LLMKey is the model provider's API key, handed to each Job's Secret.
	LLMKey string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log may be nil.
	Log *slog.Logger

	wakeOnce sync.Once
	wake     chan struct{}
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

// Loop intervals of Run.
const (
	passInterval      = 2 * time.Second
	watchInterval     = 15 * time.Second
	reconcileInterval = time.Hour
)

// Run runs the loops until ctx is done: dispatch, publish and cleanup every
// 2 s or on Wake, watch and sweep the approvals every 15 s and reconcile
// every hour. They all run in
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

	pass := time.NewTicker(passInterval)
	defer pass.Stop()
	watch := time.NewTicker(watchInterval)
	defer watch.Stop()
	reconcile := time.NewTicker(reconcileInterval)
	defer reconcile.Stop()

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
			e.sweepApprovals(ctx)
		case <-reconcile.C:
			if err := e.Reconcile(ctx); err != nil && ctx.Err() == nil {
				log.Error("reconcile failed", "error", e.redact(err.Error()))
			}
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
