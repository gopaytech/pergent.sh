package dashboard

import (
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	reasonWaiting    = "Waiting to start."
	reasonReviewing  = "Reviewing."
	reasonPosting    = "Posting the review."
	reasonSuperseded = "Replaced by a newer push, or the change was closed."
	reasonDraft      = "Not reviewed: the change is a draft and drafts are not reviewed here."
	reasonNotPosted  = "pergent.sh couldn't post the review."
	reasonUnknown    = "Published before pergent.sh recorded review outcomes."

	statusRunning  = "running"
	statusCanceled = "canceled"
	statusFailed   = "failed"

	approverKindApp      = "app"
	approverKindToken    = "token"
	healthOK             = "ok"
	healthNeedsAttention = "attention"
)

// judged is a run's outcome, the reason for it ("" when approved) and the
// status pergent.sh set on the commit for it.
type judged struct {
	outcome, reason, status string
}

// judge reads a run's outcome. A published run's is what the publisher
// recorded when it posted the review, whatever happened since (an approval
// withdrawn when the change merged, settings changed); any other run's
// follows from its state. Nothing is guessed: a run published before
// outcomes were recorded is "unknown".
func judge(r store.RunDigest) judged {
	switch r.Outcome {
	case OutcomeRunning:
		switch r.State {
		case store.Queued:
			return judged{OutcomeRunning, reasonWaiting, ""}
		case store.Reported:
			return judged{OutcomeRunning, reasonPosting, statusRunning}
		}
		return judged{OutcomeRunning, reasonReviewing, statusRunning}
	case OutcomeSuperseded:
		status := ""
		if !r.DispatchedAt.IsZero() {
			status = statusCanceled
		}
		reason := reasonSuperseded
		if r.OutcomeReason == store.SupersededDraft {
			reason = reasonDraft
		}
		return judged{OutcomeSuperseded, reason, status}
	case OutcomeFailed:
		switch r.State {
		case store.PublishFailed:
			return judged{OutcomeFailed, reasonNotPosted, statusFailed}
		case store.Reported:
			// Decided as failed, not posted yet.
			return judged{OutcomeFailed, r.FailureReason, statusRunning}
		}
	case OutcomeUnknown:
		return judged{OutcomeUnknown, reasonUnknown, r.DecisionStatus}
	}
	return judged{r.Outcome, r.OutcomeReason, r.DecisionStatus}
}
