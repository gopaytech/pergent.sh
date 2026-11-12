package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The kinds of setup problem recorded for the dashboard's "Needs attention"
// list (store.Attention.Kind).
const (
	attSingleApproval      = "single_approval"
	attApproverUnreachable = "approver_unreachable"
	attOutOfMemory         = "out_of_memory"
	attCloneFailing        = "clone_failing"
	attTokenExpiry         = "token_expiry"
	attSkillMissing        = "skill_missing"
	attNoSkills            = "no_skills"
	// attCallbackFailing is an API client's (instance "", its name as the
	// project), shown to admins only.
	attCallbackFailing = store.AttentionCallbackFailing

	// The readiness scan's items (readinessItems): set or cleared by a done
	// scan, and by a recheck when the skills change.
	attNoSkillCoverage  = "no_skill_coverage"
	attInputsOverBudget = "inputs_over_budget"
	attNoProjectContext = "no_project_context"
)

const (
	singleApprovalNote = "One approval is enough to merge here, so pergent's approval alone can unlock it. Add an approval rule that needs a human."
	approveAppNote     = "The pergent approve App isn't installed on this repository or lacks Pull requests: write, so nothing is approved."
	reviewAppNote      = "The pergent review App lacks Pull requests: write on this repository, so nothing is approved."
	approveBotNote     = "The pergent approve bot can't reach this repository, so nothing is approved."

	noSkillCoverageNote  = "No skill this repository uses matches its files, so its reviews check nothing useful. The Readiness tab suggests skills that fit."
	noProjectContextNote = "The repository has no AGENTS.md, CLAUDE.md or README, so reviews don't know what the project is. Add one."

	// oomWindow is how far back out-of-memory failures are counted, and
	// oomThreshold how many of them make a problem.
	oomWindow    = 7 * 24 * time.Hour
	oomThreshold = 2
	// cloneWindow and cloneThreshold are the same for failures at the clone
	// step.
	cloneWindow    = 7 * 24 * time.Hour
	cloneThreshold = 2

	// attentionTimeout bounds one best-effort write of an item.
	attentionTimeout = 5 * time.Second
)

// setAttention records a setup problem for the dashboard. It is best effort:
// a store error is logged at info and never reaches the caller, so a review
// or reconcile never depends on it.
func (e *Engine) setAttention(ctx context.Context, instance, project, kind, message string) {
	if err := e.Store.SetAttention(ctx, instance, project, kind, message); err != nil {
		e.log().Info("couldn't record a setup problem for the dashboard",
			"instance", instance, "project", project, "kind", kind, "error", e.redact(err.Error()))
	}
}

// clearAttention removes a setup problem that is gone, best effort like
// setAttention.
func (e *Engine) clearAttention(ctx context.Context, instance, project, kind string) {
	if err := e.Store.ClearAttention(ctx, instance, project, kind); err != nil {
		e.log().Info("couldn't clear a setup problem for the dashboard",
			"instance", instance, "project", project, "kind", kind, "error", e.redact(err.Error()))
	}
}

// dropAttention removes every item of a repository that was switched off,
// best effort like setAttention.
func (e *Engine) dropAttention(ctx context.Context, instance, project string) {
	if err := e.Store.DeleteAttentionForProject(ctx, instance, project); err != nil {
		e.log().Info("couldn't clear the setup problems of a repository that was switched off",
			"instance", instance, "project", project, "error", e.redact(err.Error()))
	}
}

// overBudgetNote words the inputs_over_budget item for skills and
// instructions of size bytes.
func overBudgetNote(size int) string {
	return fmt.Sprintf("Skills and instructions come to %d KiB, over the %d KiB a review can take, so every review fails. "+
		"Use fewer skills or shorten the instructions.", (size+1023)/1024, inputs.ConfigMapLimit/1024)
}

// approverUnreachableNote words the approver_unreachable item for instance:
// the bot when it uses a token, the review App's missing permission when it
// approves, else the approve App.
func (e *Engine) approverUnreachableNote(instance string) string {
	if inst, ok := e.registry().Get(instance); ok && inst.GitHub != nil {
		switch inst.GitHub.Approver() {
		case config.ApproverToken:
			return approveBotNote
		case config.ApproverReviewApp:
			return reviewAppNote
		}
	}
	return approveAppNote
}

// refreshOutOfMemory records out_of_memory for a repository when at least two
// of its runs failed with OOMKilled in the last 7 days, and clears it when
// fewer did. It is one count query, run when the watcher reports a failure
// and once an hour by reconcile (which is also what lets the item go away as
// failures age out). A count that can't be read leaves the item as it was.
func (e *Engine) refreshOutOfMemory(ctx context.Context, instance, project string) {
	n, err := e.Store.CountFailedRuns(ctx, instance, project, "OOMKilled", e.now().Add(-oomWindow))
	if err != nil {
		e.log().Info("couldn't count the reviews that ran out of memory",
			"instance", instance, "project", project, "error", e.redact(err.Error()))
		return
	}
	if n < oomThreshold {
		e.clearAttention(ctx, instance, project, attOutOfMemory)
		return
	}
	e.setAttention(ctx, instance, project, attOutOfMemory,
		"Reviews ran out of memory "+strconv.Itoa(n)+" times in the last 7 days. Raise the Job memory limit.")
}

// refreshCloneFailing records clone_failing for a repository when at least
// two of its runs failed at the clone step in the last 7 days, and clears it
// when fewer did. It runs when and as refreshOutOfMemory does, and counts the
// step, not the text. A count that can't be read leaves the item as it was.
func (e *Engine) refreshCloneFailing(ctx context.Context, instance, project string) {
	n, err := e.Store.CountFailedRunsAtStep(ctx, instance, project, kube.StepClone, e.now().Add(-cloneWindow))
	if err != nil {
		e.log().Info("couldn't count the reviews whose clone failed",
			"instance", instance, "project", project, "error", e.redact(err.Error()))
		return
	}
	if n < cloneThreshold {
		e.clearAttention(ctx, instance, project, attCloneFailing)
		return
	}
	e.setAttention(ctx, instance, project, attCloneFailing,
		"Clones of this repository failed "+strconv.Itoa(n)+" times in the last 7 days. Check the clone token's access and the network to the platform, or the Job memory limit if a clone ran out of memory.")
}

// attentionStore is the part of the store the token-expiry recorder writes.
type attentionStore interface {
	SetAttention(ctx context.Context, instance, project, kind, message string) error
	ClearAttention(ctx context.Context, instance, project, kind string) error
}

// tokenExpiryRetry is how long the recorder waits before writing again after
// a write failed. A variable so tests can shorten it.
var tokenExpiryRetry = 30 * time.Second

// tokenExpiry is one state the recorder is to write.
type tokenExpiry struct {
	expires time.Time
	soon    bool
}

// expiryRecorder writes the token_expiry item from one goroutine, off the
// approver's call path. Only the latest state is kept while a write runs.
type expiryRecorder struct {
	st       attentionStore
	instance string
	log      *slog.Logger

	mu      sync.Mutex
	pending *tokenExpiry
	started bool
	wake    chan struct{}
}

// TokenExpiryRecorder returns the function a token approver of instance
// calls when it learns its token's expiry (githubforge.Approver.OnTokenExpiry).
// It records the instance-wide token_expiry item while the token expires
// within 14 days and clears it otherwise. The call only hands the state over
// and returns: a single background goroutine writes it, in order, keeping
// only the latest state when several arrive during a write, and writes again
// after a pause when one fails. A store error is logged at info and never
// reaches the approver. log may be nil.
func TokenExpiryRecorder(st attentionStore, instance string, log *slog.Logger) func(expires time.Time, soon bool) error {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r := &expiryRecorder{st: st, instance: instance, log: log, wake: make(chan struct{}, 1)}
	return func(expires time.Time, soon bool) error {
		r.mu.Lock()
		r.pending = &tokenExpiry{expires, soon}
		if !r.started {
			r.started = true
			go r.run()
		}
		r.mu.Unlock()
		select {
		case r.wake <- struct{}{}:
		default:
		}
		return nil
	}
}

func (r *expiryRecorder) run() {
	for range r.wake {
		for {
			r.mu.Lock()
			s := r.pending
			r.pending = nil
			r.mu.Unlock()
			if s == nil {
				break
			}
			err := r.write(*s)
			if err == nil {
				continue
			}
			r.log.Info("couldn't record the approve token's expiry for the dashboard; will try again",
				"instance", r.instance, "error", err.Error())
			time.Sleep(tokenExpiryRetry)
			r.mu.Lock()
			if r.pending == nil { // nothing newer arrived meanwhile
				r.pending = s
			}
			r.mu.Unlock()
		}
	}
}

func (r *expiryRecorder) write(s tokenExpiry) error {
	ctx, cancel := context.WithTimeout(context.Background(), attentionTimeout)
	defer cancel()
	if !s.soon {
		return r.st.ClearAttention(ctx, r.instance, "", attTokenExpiry)
	}
	return r.st.SetAttention(ctx, r.instance, "", attTokenExpiry,
		"The approve bot's token expires on "+s.expires.UTC().Format("2006-01-02")+".")
}
