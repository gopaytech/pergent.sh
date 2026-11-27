package security

import (
	"errors"
	"fmt"
	"time"
)

// Start is a person's request to scan a repository: who asks, and whether
// they are an admin, who may go past a used-up quota and the admin limits.
// The dashboard has checked that they are a maintainer or an admin.
type Start struct {
	Instance, Project, Actor string
	Admin                    bool
}

// Why a start, or a cancel, is refused. The dashboard answers each with its
// text.
var (
	// ErrOff: the chart did not give the scan Jobs a way to the network, so
	// no scan may start.
	ErrOff = errors.New("Start scan is off: an admin must set securityJobEgress.cidrs in the Helm values first")
	// ErrPaused: the repository is paused.
	ErrPaused = errors.New("the repository is paused")
	// ErrNotOn: the repository is not switched on.
	ErrNotOn = errors.New("the repository is not switched on")
	// ErrBusy comes with the repository's queued or running scan.
	ErrBusy = errors.New("a security scan of the repository is already queued or running")
	// ErrQuota: a quota or a limit stops every model of the chain.
	ErrQuota = errors.New("the monthly token quota is used up")
	// ErrEnded comes with a scan that has already ended: it can't be
	// cancelled.
	ErrEnded = errors.New("the scan has already ended")
)

// TeamLimitError refuses an admin's start: every model of the chain left is
// at the limit Team set on it, which an admin does not go past.
type TeamLimitError struct{ Team string }

func (e *TeamLimitError) Error() string {
	return fmt.Sprintf("every model of the chain is at the team's own limit: a maintainer of %s can raise it on the team page", e.Team)
}

// RefusedError refuses a start for Reason, such as a default branch the
// platform did not give.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return e.Reason }

// The ranges of a scan's step limits; MinTimeout and MaxTimeout are the
// timeout's.
const (
	MinSteps         = 5
	MaxMainSteps     = 1000
	MaxSubagentSteps = 300
)

// CheckLimits refuses step limits and a timeout out of their ranges. A nil
// step limit is the skill's own. The store and the dashboard check them too;
// this is the last check before a Job gets them.
func CheckLimits(maxSteps, subagentMaxSteps *int, timeout time.Duration) error {
	if maxSteps != nil && (*maxSteps < MinSteps || *maxSteps > MaxMainSteps) {
		return fmt.Errorf("the max steps of the main agent must be from %d to %d, not %d", MinSteps, MaxMainSteps, *maxSteps)
	}
	if subagentMaxSteps != nil && (*subagentMaxSteps < MinSteps || *subagentMaxSteps > MaxSubagentSteps) {
		return fmt.Errorf("the max steps of each sub-agent must be from %d to %d, not %d", MinSteps, MaxSubagentSteps,
			*subagentMaxSteps)
	}
	if timeout < MinTimeout || timeout > MaxTimeout {
		return fmt.Errorf("the scan timeout must be from %s to %s, not %s", MinTimeout, MaxTimeout, timeout)
	}
	return nil
}
