package ci

import "github.com/gopaytech/pergent.sh/internal/store"

// Result is how one chosen CI job ended, in the spec's spellings.
type Result string

const (
	Passed     Result = "passed"
	Failed     Result = "failed"
	Cancelled  Result = "cancelled"
	Skipped    Result = "skipped"
	Missing    Result = "missing"
	Running    Result = "running"
	Unreadable Result = "unreadable"
)

// Job is one chosen job as read from the CI. Reason is "", "no permission" or
// "error". Log and Report are filled only by a read that asked for text, and
// Cut says the text was cut to its cap.
type Job struct {
	Name, Read  string
	Blocks      bool
	Result      Result
	Ended       bool
	Reason      string
	Log, Report string
	Cut         bool
	// TextFailed: a read that asked for text failed, and Result is Unreadable
	// for that reason. A status-only read never sets it.
	TextFailed bool
}

// Snapshot is the chosen jobs on one head. Finished: every pipeline (GitLab)
// or every check suite with check runs (GitHub) on Head has ended.
type Snapshot struct {
	Head     string
	Jobs     []Job
	Finished bool
}

// FromGitLab maps a GitLab job or bridge status. The bool says the job has
// ended. A status not listed is Failed.
func FromGitLab(status string) (Result, bool) {
	switch status {
	case "success":
		return Passed, true
	case "failed":
		return Failed, true
	case "canceled", "canceling":
		return Cancelled, true
	case "skipped", "manual":
		return Skipped, true
	case "created", "pending", "preparing", "waiting_for_resource", "scheduled", "running":
		return Running, false
	}
	return Failed, true
}

// FromGitHub maps a check run's status and conclusion. neutral passes, as
// GitHub's own branch protection treats it. Anything not listed is Failed.
func FromGitHub(status, conclusion string) (Result, bool) {
	switch status {
	case "queued", "in_progress", "waiting", "requested", "pending":
		return Running, false
	case "completed":
		switch conclusion {
		case "success", "neutral":
			return Passed, true
		case "cancelled":
			return Cancelled, true
		case "skipped":
			return Skipped, true
		}
	}
	return Failed, true
}

// Done reports that the wait can stop: every job ended, or the head's CI is
// finished and every job not ended is Missing.
func (s Snapshot) Done() bool {
	for _, j := range s.Jobs {
		if j.Ended || (s.Finished && j.Result == Missing) {
			continue
		}
		return false
	}
	return true
}

// Results is the stored snapshot: size and cut flag, never the text.
func (s Snapshot) Results() []store.CIJobResult {
	out := make([]store.CIJobResult, 0, len(s.Jobs))
	for _, j := range s.Jobs {
		out = append(out, store.CIJobResult{
			Name: j.Name, Result: string(j.Result), Reason: j.Reason, Read: j.Read,
			Size: len(j.Log) + len(j.Report), Cut: j.Cut, Blocks: j.Blocks, TextFailed: j.TextFailed,
		})
	}
	return out
}
