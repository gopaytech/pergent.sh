package gitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	StatusPending  StatusState = "pending"
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
	Description    string   `json:"description"`
	WebURL         string   `json:"web_url"`
	Author         User     `json:"author"`
	DiffRefs       DiffRefs `json:"diff_refs"`
	// ChangesCount is how many files the merge request changes: "" while
	// GitLab hasn't counted and "1000+" when it stops counting.
	ChangesCount ChangesCount `json:"changes_count"`
}

// ChangesCount is a merge request's changes_count as text. GitLab sends a
// string ("12", "1000+"), but a number (12) or null must decode too: a
// decode failure would break every GetMergeRequest caller. A number becomes
// its decimal text, null and any other shape become "" (not counted), which
// MergeRequestDiffs treats as a count that doesn't match.
type ChangesCount string

func (n *ChangesCount) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch v := v.(type) {
	case string:
		*n = ChangesCount(v)
	case float64:
		*n = ChangesCount(strconv.FormatFloat(v, 'f', -1, 64))
	default:
		*n = ""
	}
	return nil
}

// Project holds the fields pergent.sh reads. OnlyAllowMergeIfPipelineSucceeds
// ("pipelines must succeed") and OnlyAllowMergeIfAllDiscussionsAreResolved
// ("all threads must be resolved") are nil when GitLab doesn't show them.
type Project struct {
	ID                                        int64  `json:"id"`
	PathWithNamespace                         string `json:"path_with_namespace"`
	HTTPURLToRepo                             string `json:"http_url_to_repo"`
	DefaultBranch                             string `json:"default_branch"`
	OnlyAllowMergeIfPipelineSucceeds          *bool  `json:"only_allow_merge_if_pipeline_succeeds"`
	OnlyAllowMergeIfAllDiscussionsAreResolved *bool  `json:"only_allow_merge_if_all_discussions_are_resolved"`
	// Visibility is "public", "internal" or "private".
	Visibility string `json:"visibility"`
}

// Diff is one file of a merge request's or a comparison's diff. Diff holds
// the hunks, from the first "@@" on; GitLab leaves it empty for a binary
// file or one too large to show (Collapsed: left out but fetchable; TooLarge:
// left out for good).
type Diff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	DeletedFile bool   `json:"deleted_file"`
	Diff        string `json:"diff"`
	Collapsed   bool   `json:"collapsed"`
	TooLarge    bool   `json:"too_large"`
}

// Position is where a diff discussion is anchored. NewLine alone is an added
// line; OldLine is set too for an unchanged line (0 leaves it out). The three
// SHAs are the merge request's diff_refs.
type Position struct {
	BaseSHA, StartSHA, HeadSHA string
	OldPath, NewPath           string
	NewLine, OldLine           int
}

// NotePosition is where a discussion note sits in the diff, as GitLab shows
// it. LineRange is nil for a note on a single line.
type NotePosition struct {
	NewPath   string `json:"new_path"`
	NewLine   int    `json:"new_line"`
	LineRange *struct {
		End struct {
			NewLine int `json:"new_line"`
		} `json:"end"`
	} `json:"line_range"`
}

// DiscussionNote is one note of a discussion. Position is nil unless it is a
// note on the diff.
type DiscussionNote struct {
	ID         int64         `json:"id"`
	Body       string        `json:"body"`
	Author     User          `json:"author"`
	Position   *NotePosition `json:"position"`
	Resolvable bool          `json:"resolvable"`
	Resolved   bool          `json:"resolved"`
}

// Discussion is a thread of notes on a merge request; ID is GitLab's
// discussion ID (a hash string).
type Discussion struct {
	ID    string           `json:"id"`
	Notes []DiscussionNote `json:"notes"`
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
	// ErrPartialDiff is returned when a diff may not list every changed
	// file: using it as the whole diff would drop files silently.
	ErrPartialDiff = errors.New("gitlab: the diff may be incomplete")
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
