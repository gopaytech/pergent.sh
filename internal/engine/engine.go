// Package engine runs pergent.sh's background work: it dispatches queued runs
// as review Jobs, publishes their results, cleans up superseded Jobs and
// watches running ones. Anything that stops a review becomes a failure reason
// the publisher posts, never silence.
package engine

import (
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
// set statuses and approve, so they never reach a Job.
type Bots struct {
	Review, Approve *gitlab.Client
	ReviewUser      gitlab.User
}

// Engine holds what the loops share. Bots is keyed by instance name.
type Engine struct {
	Store    *store.Store
	Config   *config.Config
	Bots     map[string]Bots
	Launcher *kube.Launcher
	Library  inputs.Library
	Box      *secretbox.Box
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
