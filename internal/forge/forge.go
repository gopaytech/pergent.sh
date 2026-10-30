// Package forge is how the engine talks to a code-hosting platform: a Forge
// reads changes (GitLab merge requests, GitHub pull requests) and posts the
// review with the review bot, an Approver approves with the approve bot. The
// adapters (gitlabforge, later githubforge) map one platform onto them.
package forge

import (
	"context"
	"errors"
)

// ChangeState is the state of a change.
type ChangeState string

const (
	StateOpen   ChangeState = "open"
	StateClosed ChangeState = "closed"
	StateMerged ChangeState = "merged"
	// StateLocked is GitLab's merge in progress.
	StateLocked ChangeState = "locked"
)

// Change is a merge request or pull request as the engine sees it. BaseSHA is
// the merge base and StartSHA is where the repository's review inputs are
// read. An empty BaseSHA or StartSHA means the platform hasn't computed the
// change's diff for HeadSHA yet.
//
// Title, WebURL (its page on the platform), SourceBranch and Author (the
// username of whoever opened it) are only shown in the dashboard; the
// engine decides nothing on them.
type Change struct {
	Number       int64
	State        ChangeState
	Draft        bool
	HeadSHA      string
	TargetBranch string
	BaseSHA      string
	StartSHA     string
	Title        string
	WebURL       string
	SourceBranch string
	Author       string
}

// StatusState is the state of pergent's status on a commit.
type StatusState string

const (
	StatusRunning  StatusState = "running"
	StatusSuccess  StatusState = "success"
	StatusFailed   StatusState = "failed"
	StatusNeutral  StatusState = "neutral"
	StatusCanceled StatusState = "canceled"
)

// TreeEntry is one entry of a repository tree. Type is "blob" or "tree".
type TreeEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
}

// Forge is the review bot's side of one platform instance. Its GetFile and
// ListTree make it an inputs.Source (declared here rather than embedded, as
// inputs depends on this package for TreeEntry).
type Forge interface {
	// GetFile returns a file's raw content at ref. A missing file is nil,
	// false, nil.
	GetFile(ctx context.Context, projectID int64, path, ref string) ([]byte, bool, error)
	// ListTree lists one directory at ref (non-recursive). A missing
	// directory is nil, nil.
	ListTree(ctx context.Context, projectID int64, path, ref string) ([]TreeEntry, error)

	// GetChange returns one change. A change that doesn't exist is
	// ErrNotFound.
	GetChange(ctx context.Context, projectID, number int64) (Change, error)
	// SetStatus sets pergent's status on sha.
	SetStatus(ctx context.Context, projectID int64, sha string, s StatusState, description string) error
	// FindComment returns the ID of the review bot's own comment on the
	// change that contains marker.
	FindComment(ctx context.Context, projectID, number int64, marker string) (int64, bool, error)
	// CreateComment adds a comment to the change and returns its ID.
	CreateComment(ctx context.Context, projectID, number int64, body string) (int64, error)
	// UpdateComment replaces a comment's body. A comment that is gone is
	// ErrNotFound.
	UpdateComment(ctx context.Context, projectID, number, commentID int64, body string) error
	// ChangeRef is the ref a change's head can be fetched from.
	ChangeRef(number int64) string
}

// Approver is the approve bot's side of one platform instance.
type Approver interface {
	// Approve approves the change at sha. A head that is no longer sha is
	// ErrHeadMoved.
	Approve(ctx context.Context, projectID, number int64, sha string) error
	// Unapprove withdraws the approve bot's approval. Nothing to withdraw is
	// not an error.
	Unapprove(ctx context.Context, projectID, number int64) error
	// HasApproved reports whether the approve bot approves the change at
	// sha now. A platform whose approvals aren't tied to a commit (GitLab,
	// which withdraws them itself on a push) ignores sha.
	HasApproved(ctx context.Context, projectID, number int64, sha string) (bool, error)
}

var (
	// ErrNotFound is matched (errors.Is) by an error for something that
	// doesn't exist.
	ErrNotFound = errors.New("forge: not found")
	// ErrHeadMoved is matched by Approve's error when the change's head is no
	// longer the SHA it was asked to approve.
	ErrHeadMoved = errors.New("forge: change head moved")
)
