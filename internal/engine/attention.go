package engine

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// The kinds of setup problem recorded for the dashboard's "Needs attention"
// list (store.Attention.Kind).
const (
	attSingleApproval      = "single_approval"
	attApproverUnreachable = "approver_unreachable"
	attOutOfMemory         = "out_of_memory"
	attTokenExpiry         = "token_expiry"
)

const (
	singleApprovalNote = "One approval is enough to merge here, so pergent's approval alone can unlock it. Add an approval rule that needs a human."
	approveAppNote     = "The pergent approve App isn't installed on this repository, so nothing is approved."
	approveBotNote     = "The pergent approve bot can't reach this repository, so nothing is approved."

	// oomWindow is how far back out-of-memory failures are counted, and
	// oomThreshold how many of them make a problem.
	oomWindow    = 7 * 24 * time.Hour
	oomThreshold = 2

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

// approverUnreachableNote words the approver_unreachable item for instance:
// the App when the instance approves with one, the bot when it uses a token.
func (e *Engine) approverUnreachableNote(instance string) string {
	if gi, ok := e.Config.GitHubInstance(instance); ok && gi.ApproveApp == nil {
		return approveBotNote
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

// TokenExpiryRecorder returns the function a token approver of instance calls
// when it learns its token's expiry (githubforge.Approver.OnTokenExpiry): it
// records the instance-wide token_expiry item while the token expires within
// 14 days, and clears it otherwise. Writes are best effort; log may be nil.
func TokenExpiryRecorder(st *store.Store, instance string, log *slog.Logger) func(expires time.Time, soon bool) {
	e := &Engine{Store: st, Log: log}
	return func(expires time.Time, soon bool) {
		ctx, cancel := context.WithTimeout(context.Background(), attentionTimeout)
		defer cancel()
		if !soon {
			e.clearAttention(ctx, instance, "", attTokenExpiry)
			return
		}
		e.setAttention(ctx, instance, "", attTokenExpiry,
			"The approve bot's token expires on "+expires.UTC().Format("2006-01-02")+".")
	}
}
