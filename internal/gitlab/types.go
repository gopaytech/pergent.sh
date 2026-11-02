package gitlab

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/forge"
)

// StatusState is a GitLab commit status state.
type StatusState string

const (
	StatusRunning  StatusState = "running"
	StatusSuccess  StatusState = "success"
	StatusFailed   StatusState = "failed"
	StatusCanceled StatusState = "canceled"
	StatusSkipped  StatusState = "skipped"
)

// DiffRefs are the three SHAs GitLab computes for a merge request.
type DiffRefs struct {
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	StartSHA string `json:"start_sha"`
}

// MergeRequest holds the fields pergent.sh reads. GetMergeRequest sets
// Draft when either draft or work_in_progress (the name some GitLab versions
// report it under) is true, so a draft is never read as ready.
type MergeRequest struct {
	IID            int64    `json:"iid"`
	State          string   `json:"state"`
	Draft          bool     `json:"draft"`
	WorkInProgress bool     `json:"work_in_progress"`
	SHA            string   `json:"sha"`
	TargetBranch   string   `json:"target_branch"`
	SourceBranch   string   `json:"source_branch"`
	Title          string   `json:"title"`
	WebURL         string   `json:"web_url"`
	Author         User     `json:"author"`
	DiffRefs       DiffRefs `json:"diff_refs"`
}

// Project holds the fields pergent.sh reads. OnlyAllowMergeIfPipelineSucceeds
// ("pipelines must succeed") is nil when GitLab doesn't show it.
type Project struct {
	ID                               int64  `json:"id"`
	PathWithNamespace                string `json:"path_with_namespace"`
	HTTPURLToRepo                    string `json:"http_url_to_repo"`
	DefaultBranch                    string `json:"default_branch"`
	OnlyAllowMergeIfPipelineSucceeds *bool  `json:"only_allow_merge_if_pipeline_succeeds"`
}

// ApprovalRule is a project approval rule. A rule with no protected branches
// that doesn't apply to all protected branches applies to every branch.
type ApprovalRule struct {
	RuleType                      string            `json:"rule_type"`
	ApprovalsRequired             int               `json:"approvals_required"`
	AppliesToAllProtectedBranches bool              `json:"applies_to_all_protected_branches"`
	ProtectedBranches             []ProtectedBranch `json:"protected_branches"`
}

// ProtectedBranch is a protected branch: its name, which may hold "*"
// wildcards, and who may push to it. An approval rule lists the protected
// branches it applies to in the same shape.
type ProtectedBranch struct {
	Name             string        `json:"name"`
	PushAccessLevels []AccessLevel `json:"push_access_levels"`
}

// AccessLevel is one grant of a protected branch. Developer is 30,
// Maintainer 40, and 0 is no one.
type AccessLevel struct {
	AccessLevel int `json:"access_level"`
}

// TreeEntry is one entry of a repository tree: the platform-neutral
// forge.TreeEntry, which ListTree decodes GitLab's tree entries into.
type TreeEntry = forge.TreeEntry

// Hook is a project webhook.
type Hook struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

// DeployToken is a project deploy token. Token is only returned on creation.
type DeployToken struct {
	ID        int64
	Username  string
	Token     string
	ExpiresAt time.Time
}

// User is a GitLab user.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

var (
	// ErrNotFound is returned (or matched with errors.Is) for a 404 response.
	ErrNotFound = errors.New("gitlab: not found")
	// ErrHeadMoved is returned by Approve when the MR head is no longer the given SHA.
	ErrHeadMoved = errors.New("gitlab: merge request head moved")
	// ErrTooLarge is returned when a reply is bigger than the limit set for
	// the request. It is not retried.
	ErrTooLarge = errors.New("gitlab: response too large")
)

const maxErrorBody = 200

// APIError is a non-2xx GitLab response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > maxErrorBody {
		body = truncateUTF8(body, maxErrorBody) + "..."
	}
	if body == "" {
		return fmt.Sprintf("gitlab: status %d", e.Status)
	}
	return fmt.Sprintf("gitlab: status %d: %s", e.Status, body)
}

// Is lets errors.Is(err, ErrNotFound) match any 404 response.
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.Status == 404
}

// truncateUTF8 cuts s to at most max bytes without splitting a rune.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
