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
// Author (the username of whoever opened it) and AuthorID (their numeric
// user ID, 0 when the platform didn't say) tell who the change's author is:
// the engine compares AuthorID with a commenter's, never usernames, to keep
// the author from dismissing blocking findings of their own change. Title,
// WebURL (its page on the platform) and SourceBranch are only shown in the
// dashboard; the engine decides nothing on them.
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
	AuthorID     int64
	// AuthorIsBot is set when the platform said, in the change itself, that a
	// bot opened it: GitHub's user type. GitLab leaves it false and is asked
	// with IsBot.
	AuthorIsBot bool
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

// Identity is a bot user on a platform: its numeric ID and its username
// (GitHub: the login, "<slug>[bot]").
type Identity struct {
	ID       int64
	Username string
}

// Reaction is how the engine answers a comment of a person: the platform
// shows it as an emoji on that comment.
type Reaction string

const (
	// ReactSeen is eyes: the comment was picked up.
	ReactSeen Reaction = "seen"
	// ReactDone is thumbs up: the command was carried out.
	ReactDone Reaction = "done"
	// ReactRefused is thumbs down: the command was refused.
	ReactRefused Reaction = "refused"
)

// Access is what a person may do in a repository, from the platform's own
// roles. The levels are ordered: compare with >=.
type Access int

const (
	// AccessNone is not a member, or not known to the platform.
	AccessNone Access = iota
	// AccessRead is GitLab Guest and Reporter, GitHub read and triage.
	AccessRead
	// AccessWrite is GitLab Developer, GitHub write.
	AccessWrite
	// AccessMaintain is GitLab Maintainer and Owner, GitHub maintain and
	// admin.
	AccessMaintain
)

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

	// Bot is the review bot (GitLab) or the review App's bot user (GitHub).
	Bot() Identity
	// Mention is the handle people type after @ to address the review bot:
	// the GitLab bot's username, the GitHub App's slug.
	Mention() string
	// React adds the emoji for reaction to a comment as the review bot. A
	// comment that is gone is ErrNotFound.
	React(ctx context.Context, projectID, number, commentID int64, reaction Reaction) error
	// Reply adds a new plain comment to the change (it carries no pergent
	// marker, so it is never taken for the review comment).
	Reply(ctx context.Context, projectID, number int64, body string) error
	// AccessLevel is what the user may do in the project. username is who
	// GitHub looks up, userID who GitLab does; a user the platform doesn't
	// know is AccessNone.
	AccessLevel(ctx context.Context, projectID int64, username string, userID int64) (Access, error)
	// IsBot reports whether the platform marks the user as an automated
	// account: GitLab's project, group and service-account bots, which a
	// comment's username only suggests. An error means it could not be told
	// (a command from such an author must not be acted on then). GitHub's
	// webhook says it itself and never asks.
	IsBot(ctx context.Context, userID int64) (bool, error)
}

// MaxSourceFile is the most a SourceReader reads of one file: 900 KiB, the
// same as inputs.ConfigMapLimit (a test keeps the two equal; forge can't
// import inputs). Nothing a review reads can be larger, and a repository is
// not trusted to keep its files small.
const MaxSourceFile = 900 * 1024

// SourceReader reads any repository the review bot (GitLab, Reporter access
// is enough) or the review App's installation (GitHub) can see, by its path
// ("group/project", "owner/name") and at a commit. The repository need not be
// switched on for review: installing skills from a repository uses this.
//
// ListTreeAt and GetFileAt answer as Forge's ListTree and GetFile do: a
// missing folder is an empty list and a missing file (or a directory) is
// ok == false. A repository or ref the bot or App can't read is
// ErrSourceNotFound, a file over MaxSourceFile is ErrFileTooLarge.
type SourceReader interface {
	// ResolveRef returns the commit a branch, tag or commit ref points at.
	ResolveRef(ctx context.Context, project, ref string) (commit string, err error)
	// ListTreeAt lists one directory at commit (non-recursive).
	ListTreeAt(ctx context.Context, project, path, commit string) ([]TreeEntry, error)
	// GetFileAt returns a file's raw content at commit.
	GetFileAt(ctx context.Context, project, path, commit string) ([]byte, bool, error)
}

// FileDiff is one file of a diff as the platform returns it: Patch is the
// file's hunks, from the first "@@" on; empty for a binary or too large file.
type FileDiff struct {
	Path, OldPath string
	Deleted       bool
	Patch         string
}

// Paths returns the path of each file and, for a renamed one, its old path:
// every path the files touch, each once, in order.
func Paths(files []FileDiff) []string {
	var out []string
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		for _, p := range []string{f.Path, f.OldPath} {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// ThreadRef names a thread: GitHub's review thread node ID and its first
// comment's ID, GitLab's discussion ID and its first note's ID.
type ThreadRef struct {
	ID        string
	CommentID int64
}

// Thread is a thread of the review bot's on a change, as the platform shows
// it now. Line and EndLine are where it is anchored at the change's head (0
// when the platform can't say, e.g. GitHub marks it outdated).
type Thread struct {
	Ref           ThreadRef
	Body          string // its first comment
	Path          string
	Line, EndLine int
	Resolved      bool
}

// NewThread is a thread to post at a change's head. OldLine is the base
// line of Line when Line is an unchanged line (GitLab positions need it); 0
// for an added line.
type NewThread struct {
	Path, OldPath string
	Line, EndLine int
	OldLine       int
	Body          string
}

// Inline is the review bot's inline threads on one platform.
type Inline interface {
	// ChangeDiff returns the change's diff at its head.
	ChangeDiff(ctx context.Context, projectID, number int64) ([]FileDiff, error)
	// CompareDiff returns the diff from one commit to another.
	CompareDiff(ctx context.Context, projectID int64, from, to string) ([]FileDiff, error)
	// ChangedSince returns the paths changed from one commit to another,
	// both sides of a rename. It is an error when from is not an ancestor of
	// to, or when the list may be partial.
	ChangedSince(ctx context.Context, projectID int64, from, to string) ([]string, error)
	// Threads lists the review bot's own threads on the change.
	Threads(ctx context.Context, projectID, number int64) ([]Thread, error)
	// PostThreads posts threads at ch's head. It returns one ref per thread,
	// in order; a thread that could not be posted has a zero ref and its
	// error in errs. err is a failure of the whole call.
	PostThreads(ctx context.Context, projectID int64, ch Change, threads []NewThread) (refs []ThreadRef, errs []error, err error)
	// ReplyThread adds a reply to a thread.
	ReplyThread(ctx context.Context, projectID, number int64, t ThreadRef, body string) error
	// ResolveThread resolves the thread, or unresolves it when resolved is false.
	ResolveThread(ctx context.Context, projectID, number int64, t ThreadRef, resolved bool) error
	// ReactThread adds the emoji for reaction to a reply in a thread.
	ReactThread(ctx context.Context, projectID, number, commentID int64, reaction Reaction) error
	// ThreadsBlockMerge reports whether open threads block merging into branch.
	ThreadsBlockMerge(ctx context.Context, projectID int64, branch string) (bool, error)
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
	// Bot is the approve bot, whose comments are never commands.
	Bot() Identity
}

var (
	// ErrNotFound is matched (errors.Is) by an error for something that
	// doesn't exist.
	ErrNotFound = errors.New("forge: not found")
	// ErrHeadMoved is matched by Approve's error when the change's head is no
	// longer the SHA it was asked to approve.
	ErrHeadMoved = errors.New("forge: change head moved")
	// ErrSourceNotFound is matched by a SourceReader's error for a
	// repository or ref the review bot or App can't read. The platform says
	// the same for a repository that doesn't exist and one it may not see.
	ErrSourceNotFound = errors.New("the review bot or App can't read this repository or ref")
	// ErrFileTooLarge is matched by GetFileAt's error for a file over
	// MaxSourceFile.
	ErrFileTooLarge = errors.New("forge: file is too large")
)
