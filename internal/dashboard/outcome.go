package dashboard

import (
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	reasonWaiting        = "Waiting to start."
	reasonReviewing      = "Reviewing."
	reasonPosting        = "Posting the review."
	reasonSuperseded     = "Replaced by a newer push, or the change was closed."
	reasonNotPosted      = "pergent.sh couldn't post the review."
	reasonNoResult       = "No result was received."
	reasonUnusable       = "pergent sent a result pergent.sh couldn't use."
	reasonErrorResult    = "The review failed."
	reasonApprovalOff    = "Not approved: approval is turned off for this repository."
	reasonReplaced       = "Not approved: a newer review replaced this one."
	reasonDraft          = "Not approved: this is a draft. It will be approved when it's marked ready."
	reasonNoApproveApp   = "Not approved: the pergent approve App isn't installed on this repository."
	reasonNoApproveBot   = "Not approved: the pergent approve bot can't reach this repository."
	statusRunning        = "running"
	statusCanceled       = "canceled"
	verdictHasBlocking   = "has_blocking_issues"
	approverKindApp      = "app"
	approverKindToken    = "token"
	sourceKindLibrary    = "library"
	sourceKindRepo       = "repo"
	healthOK             = "ok"
	healthNeedsAttention = "attention"
)

// runFacts is what a run's outcome is decided on.
type runFacts struct {
	state      store.RunState
	failure    string
	result     []byte // the result or its summary
	base, head string
	dispatched bool
	// approved: the approve bot's recorded approval is of this head and
	// target; newest: the change's newest run that isn't superseded;
	// approveAvailable: the approve side reaches the repository.
	approved, newest, approveAvailable bool
}

func digestFacts(r store.RunDigest) runFacts {
	return runFacts{
		state: r.State, failure: r.FailureReason, result: r.Summary, base: r.BaseSHA, head: r.HeadSHA,
		dispatched: !r.DispatchedAt.IsZero(), approved: r.Approved, newest: r.Newest,
		approveAvailable: r.ApproveAvailable,
	}
}

// judged is a run's outcome, the reason for it ("" when approved) and the
// status pergent.sh sets on the commit for it.
type judged struct {
	outcome, reason, status string
}

// repoPolicy is what a run's repository settings change about its
// outcome: whether clean reviews are approved, the status of a failure and
// whether the approve side is a bot user (its wording).
type repoPolicy struct {
	approve    bool
	failure    config.FailureStatus
	approveBot bool
}

// policyFor is the policy of repository project on instance: its effective
// settings when it is switched on, else the defaults the publisher uses.
func (d *Dashboard) policyFor(instance string, repo settings.Effective, ok bool) repoPolicy {
	p := repoPolicy{approve: true, failure: config.FailureStatusFailed}
	if ok {
		p.approve, p.failure = repo.Approve, repo.FailureStatus
	}
	if gi, found := d.cfg.GitHubInstance(instance); found && gi.ApproveApp == nil {
		p.approveBot = true
	}
	return p
}

// judge decides a run's outcome the way the publisher decides what to post
// (policy.Decide on the stored result), then reads whether the approval was
// made from what pergent.sh recorded. A clean review that isn't approved is
// explained by the first that applies: approval turned off, a newer review,
// the approve side unable to reach the repository, otherwise a draft (the
// only other reason the publisher holds an approval back).
func judge(f runFacts, p repoPolicy) judged {
	switch f.state {
	case store.Queued:
		return judged{OutcomeRunning, reasonWaiting, ""}
	case store.Running:
		return judged{OutcomeRunning, reasonReviewing, statusRunning}
	case store.Superseded:
		status := ""
		if f.dispatched {
			status = statusCanceled
		}
		return judged{OutcomeSuperseded, reasonSuperseded, status}
	case store.PublishFailed:
		return judged{OutcomeFailed, reasonNotPosted, string(policy.StatusFailed)}
	}

	o, why := policyOutcome(f)
	dec := policy.Decide(o, p.failure)
	status := string(dec.Status)
	switch {
	case o.FailureReason != "":
		return judged{OutcomeFailed, why, status}
	case o.Result.Status != "completed":
		return judged{OutcomeFailed, why, status}
	case hasBlocking(o.Result):
		return judged{OutcomeBlocking, dec.Description + ".", status}
	case dec.Status != policy.StatusSuccess:
		return judged{OutcomeFailed, dec.Description + ".", status}
	case !dec.Approve:
		return judged{OutcomeNoBlocking, dec.Description + ".", status}
	case f.state == store.Reported:
		return judged{OutcomeRunning, reasonPosting, statusRunning}
	case f.approved:
		return judged{OutcomeApproved, "", status}
	case !p.approve:
		return judged{OutcomeNoBlocking, reasonApprovalOff, status}
	case !f.newest:
		return judged{OutcomeNoBlocking, reasonReplaced, status}
	case !f.approveAvailable && p.approveBot:
		return judged{OutcomeNoBlocking, reasonNoApproveBot, status}
	case !f.approveAvailable:
		return judged{OutcomeNoBlocking, reasonNoApproveApp, status}
	}
	return judged{OutcomeDraft, reasonDraft, status}
}

// policyOutcome is the run's outcome as the publisher reads it: its failure
// reason, or its result if that parses and is for the run's own base and
// head. why is the reason a run that isn't a completed review failed.
func policyOutcome(f runFacts) (policy.Outcome, string) {
	if f.failure != "" {
		return policy.Outcome{FailureReason: f.failure}, f.failure
	}
	if f.result == nil {
		return policy.Outcome{FailureReason: reasonNoResult}, reasonNoResult
	}
	r, err := result.Parse(f.result)
	if err != nil || r.Base != f.base || r.Head != f.head {
		return policy.Outcome{FailureReason: reasonUnusable}, reasonUnusable
	}
	if r.Status != "completed" {
		why := reasonErrorResult
		if r.Error != nil && r.Error.Message != "" {
			why = r.Error.Message
		}
		return policy.Outcome{Result: r}, why
	}
	return policy.Outcome{Result: r}, ""
}

func hasBlocking(r *result.Result) bool {
	for _, s := range r.Skills {
		if s.Verdict == verdictHasBlocking {
			return true
		}
	}
	return false
}
