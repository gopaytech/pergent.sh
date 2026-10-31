package github

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/forge"
)

// User is a GitHub user or bot.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// Repository holds the fields pergent.sh reads.
type Repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// Ref is one side of a pull request: its branch and the commit GitHub
// recorded for it.
type Ref struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// PullRequest holds the fields pergent.sh reads. State is "open" or "closed";
// a merged pull request is "closed" with Merged true.
type PullRequest struct {
	Number  int64  `json:"number"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Draft   bool   `json:"draft"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	User    User   `json:"user"`
	Head    Ref    `json:"head"`
	Base    Ref    `json:"base"`
}

// Comment is an issue (pull request conversation) comment.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User User   `json:"user"`
}

// Review is a pull request review. State is APPROVED, CHANGES_REQUESTED,
// COMMENTED, DISMISSED or PENDING; CommitID is the head it was given on.
type Review struct {
	ID       int64  `json:"id"`
	User     User   `json:"user"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
}

// TreeEntry is the platform-neutral forge.TreeEntry. Tree maps GitHub's
// types onto GitLab's: file → blob, dir → tree, submodule → commit; a
// symlink stays "symlink", which callers looking for blobs and trees skip.
type TreeEntry = forge.TreeEntry

// ErrNotFound is matched (errors.Is) by the *APIError of any 404 response.
var ErrNotFound = errors.New("github: not found")

// ErrForbidden is matched (errors.Is) by the *APIError of a 403 response
// that isn't a rate limit: the credential lacks a permission.
var ErrForbidden = errors.New("github: forbidden")

const maxErrorBody = 200

// APIError is a non-2xx GitHub response. Body is masked (no token) and cut
// to 4096 bytes; Error shows at most 200 bytes of it.
type APIError struct {
	Status int
	Body   string
	// RateLimited is true for a 403 that is a rate limit, not a refusal.
	RateLimited bool
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > maxErrorBody {
		body = truncateUTF8(body, maxErrorBody) + "..."
	}
	if body == "" {
		return fmt.Sprintf("github: status %d", e.Status)
	}
	return fmt.Sprintf("github: status %d: %s", e.Status, body)
}

// Is lets errors.Is(err, ErrNotFound) match any 404 response, and
// errors.Is(err, ErrForbidden) a 403 that isn't a rate limit.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == 404
	case ErrForbidden:
		return e.Status == 403 && !e.RateLimited
	}
	return false
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
