package store

import "time"

// CIReport is where a job's report file is: Path inside the job's artifacts
// and, on GitHub only, the workflow artifact's name.
type CIReport struct {
	Artifact string `json:"artifact,omitempty"`
	Path     string `json:"path"`
}

// CIJob is one CI job a repository chose to read. Read is "status", "log" or
// "report"; Report is set only for "report". The rules live in the ci package.
type CIJob struct {
	Name           string    `json:"name"`
	Read           string    `json:"read"`
	Report         *CIReport `json:"report,omitempty"`
	BlocksApproval bool      `json:"blocks_approval"`
}

// CIJobs is a repository's ci_jobs setting: how long a run waits for the
// jobs, and the jobs.
type CIJobs struct {
	WaitSeconds int64   `json:"wait_seconds"`
	Jobs        []CIJob `json:"jobs"`
}

// Wait is how long a run waits for the jobs to end.
func (c CIJobs) Wait() time.Duration { return time.Duration(c.WaitSeconds) * time.Second }

// CIJobResult is the stored snapshot of one chosen job: its name, result
// (passed, failed, cancelled, skipped, missing, running or unreadable), the
// reason for an unreadable one, what was read, the size of the text read, whether
// it was cut and whether the job blocks approval. Never any CI text.
type CIJobResult struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	Read   string `json:"read"`
	Size   int    `json:"size"`
	Cut    bool   `json:"cut"`
	Blocks bool   `json:"blocks"`
	// TextFailed: the log or report read failed, so Result is unreadable
	// although a status-only read would say otherwise.
	TextFailed bool `json:"text_failed,omitempty"`
}
