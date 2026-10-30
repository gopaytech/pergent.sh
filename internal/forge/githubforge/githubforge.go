// Package githubforge maps a GitHub instance onto forge.Forge and
// forge.Approver. The engine names a repository by its numeric ID (the
// forge's projectID); a Repos lookup turns that into the owner/name GitHub's
// REST paths take and the review App's installation that reaches it.
//
// The review side is the review App's installation; the approve side is a
// second App (NewAppApprover) or a bot user's token (NewTokenApprover).
package githubforge

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/github"
)

const (
	// checkName is the name of pergent's Check Run.
	checkName = "pergent"
	// runningTitle is the running Check Run's title.
	runningTitle = "pergent review running"
	// checkSummary is every Check Run's summary: the findings are in the
	// comment, never duplicated here.
	checkSummary = "See the pergent comment on this pull request."
)

// Repo is a repository as the adapters reach it. InstallationID is the review
// App's installation on it; 0 means the review App isn't installed there.
type Repo struct {
	Owner, Name    string
	InstallationID int64
}

// Repos resolves a repository ID. A repository it doesn't know is
// ErrUnknownRepo.
type Repos interface {
	Repo(ctx context.Context, repoID int64) (Repo, error)
}

// ReposFunc is a function that is a Repos.
type ReposFunc func(ctx context.Context, repoID int64) (Repo, error)

// Repo calls f.
func (f ReposFunc) Repo(ctx context.Context, repoID int64) (Repo, error) { return f(ctx, repoID) }

var (
	// ErrUnknownRepo is a repository ID pergent.sh has no repository for
	// (reconcile hasn't stored it). It is not forge.ErrNotFound: the change
	// may well exist.
	ErrUnknownRepo = errors.New("githubforge: unknown repository")
	// ErrNotInstalled is a repository the review App isn't installed on.
	ErrNotInstalled = errors.New("the pergent review App isn't installed on this repository")
)

// Forge is the review App of one GitHub instance.
type Forge struct {
	app *github.App
	// bot is the review App's bot user: FindComment only finds its comments.
	bot   int64
	repos Repos
}

// NewForge returns the review App's Forge. botUserID is the App's bot user
// (app.BotUserID), resolved once by the caller: that lookup is
// unauthenticated and rate-limited.
func NewForge(app *github.App, botUserID int64, repos Repos) *Forge {
	return &Forge{app: app, bot: botUserID, repos: repos}
}

// client returns the review installation's client for repoID.
func (f *Forge) client(ctx context.Context, repoID int64) (*github.Client, Repo, error) {
	r, err := f.repos.Repo(ctx, repoID)
	if err != nil {
		return nil, Repo{}, err
	}
	if r.InstallationID == 0 {
		return nil, Repo{}, ErrNotInstalled
	}
	return f.app.Client(r.InstallationID), r, nil
}

// GetFile returns a file's raw content at ref. A missing file, or a
// directory, is nil, false, nil.
func (f *Forge) GetFile(ctx context.Context, repoID int64, path, ref string) ([]byte, bool, error) {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return nil, false, err
	}
	b, ok, err := c.File(ctx, r.Owner, r.Name, path, ref)
	return b, ok, mapErr(err)
}

// ListTree lists one directory at ref. A missing directory is nil, nil.
func (f *Forge) ListTree(ctx context.Context, repoID int64, path, ref string) ([]forge.TreeEntry, error) {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return nil, err
	}
	entries, err := c.Tree(ctx, r.Owner, r.Name, path, ref)
	return entries, mapErr(err)
}

// GetChange reads pull request number. A closed pull request GitHub reports
// as merged is forge.StateMerged. TargetBranch and StartSHA are the base ref
// and the base commit GitHub recorded for the pull request (inputs are read
// there); BaseSHA is the merge base of that commit and the head, asked of
// GitHub only for an open pull request. A compare GitHub can't answer yet
// (404 or 422) leaves BaseSHA empty: not ready.
func (f *Forge) GetChange(ctx context.Context, repoID, number int64) (forge.Change, error) {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return forge.Change{}, err
	}
	pr, err := c.PullRequest(ctx, r.Owner, r.Name, number)
	if err != nil {
		return forge.Change{}, mapErr(err)
	}
	ch := forge.Change{
		Number:       pr.Number,
		State:        forge.ChangeState(pr.State),
		Draft:        pr.Draft,
		HeadSHA:      pr.Head.SHA,
		TargetBranch: pr.Base.Ref,
		StartSHA:     pr.Base.SHA,
		Title:        pr.Title,
		WebURL:       pr.HTMLURL,
		SourceBranch: pr.Head.Ref,
		Author:       pr.User.Login,
	}
	if ch.Number == 0 {
		ch.Number = number
	}
	switch {
	case pr.State == "open":
		ch.State = forge.StateOpen
	case pr.Merged:
		ch.State = forge.StateMerged
	case pr.State == "closed":
		ch.State = forge.StateClosed
	}
	if ch.State != forge.StateOpen || pr.Head.SHA == "" || pr.Base.SHA == "" {
		return ch, nil
	}
	base, err := c.Compare(ctx, r.Owner, r.Name, pr.Base.SHA, pr.Head.SHA)
	if status(err) == http.StatusNotFound || status(err) == http.StatusUnprocessableEntity {
		return ch, nil
	}
	if err != nil {
		return forge.Change{}, mapErr(err)
	}
	ch.BaseSHA = base
	return ch, nil
}

// DefaultBranch returns the repository's default branch, read through the
// review App's installation.
func (f *Forge) DefaultBranch(ctx context.Context, repoID int64) (string, error) {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return "", err
	}
	repo, err := c.Repo(ctx, r.Owner, r.Name)
	if err != nil {
		return "", mapErr(err)
	}
	return repo.DefaultBranch, nil
}

// SetStatus creates a "pergent" Check Run on sha. Running is in_progress;
// everything else is completed, with the description as its title.
func (f *Forge) SetStatus(ctx context.Context, repoID int64, sha string, s forge.StatusState, description string) error {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return err
	}
	status, conclusion, title := "completed", Conclusion(s), description
	if s == forge.StatusRunning {
		status, conclusion, title = "in_progress", "", runningTitle
	}
	_, err = c.CreateCheckRun(ctx, r.Owner, r.Name, checkName, sha, status, conclusion, title, checkSummary)
	return mapErr(err)
}

// Conclusion maps a finished status onto a Check Run conclusion. Anything
// else, unknown states included, is "failure": a state pergent.sh doesn't
// know is never success.
func Conclusion(s forge.StatusState) string {
	switch s {
	case forge.StatusSuccess:
		return "success"
	case forge.StatusNeutral:
		return "neutral"
	case forge.StatusCanceled:
		return "cancelled"
	default:
		return "failure"
	}
}

// FindComment returns the review bot's first conversation comment on the
// pull request that contains marker; comments of other users are never
// matched.
func (f *Forge) FindComment(ctx context.Context, repoID, number int64, marker string) (int64, bool, error) {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return 0, false, err
	}
	comments, err := c.IssueComments(ctx, r.Owner, r.Name, number)
	if err != nil {
		return 0, false, mapErr(err)
	}
	for _, cm := range comments {
		if f.bot != 0 && cm.User.ID == f.bot && strings.Contains(cm.Body, marker) {
			return cm.ID, true, nil
		}
	}
	return 0, false, nil
}

// CreateComment adds a conversation comment to the pull request and returns
// its ID.
func (f *Forge) CreateComment(ctx context.Context, repoID, number int64, body string) (int64, error) {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return 0, err
	}
	id, err := c.CreateIssueComment(ctx, r.Owner, r.Name, number, body)
	return id, mapErr(err)
}

// UpdateComment replaces a comment's body. A deleted comment is
// forge.ErrNotFound.
func (f *Forge) UpdateComment(ctx context.Context, repoID, _, commentID int64, body string) error {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return err
	}
	return mapErr(c.UpdateIssueComment(ctx, r.Owner, r.Name, commentID, body))
}

// ChangeRef is the ref GitHub keeps pull request number's head at, forks
// included.
func (f *Forge) ChangeRef(number int64) string {
	return "refs/pull/" + strconv.FormatInt(number, 10) + "/head"
}

// status is the HTTP status of a GitHub API error, or 0.
func status(err error) int {
	var ae *github.APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// mapErr makes the client's not-found error match forge.ErrNotFound too. The
// message stays the client's.
func mapErr(err error) error {
	if err != nil && errors.Is(err, github.ErrNotFound) {
		return &mapped{err: err, sentinel: forge.ErrNotFound}
	}
	return err
}

type mapped struct{ err, sentinel error }

func (m *mapped) Error() string   { return m.err.Error() }
func (m *mapped) Unwrap() []error { return []error{m.err, m.sentinel} }

// callTimeout bounds each request of ReachableRepos and CanReach, which
// serve the dashboard.
const callTimeout = 10 * time.Second

// ReachableRepos returns the full names of the repositories of every
// installation of the review App that hold search, ignoring case ("" for
// all), up to limit; truncated is true when there were more. Each page's
// request has 10 s; on an error it also returns the names read before it.
func (f *Forge) ReachableRepos(ctx context.Context, search string, limit int) ([]string, bool, error) {
	ids, err := f.app.Installations(ctx, callTimeout)
	if err != nil {
		return nil, false, err
	}
	var out []string
	for _, id := range ids {
		names, truncated, err := f.app.Client(id).InstallationRepos(ctx,
			github.ListOptions{Match: search, Limit: limit - len(out), CallTimeout: callTimeout})
		out = append(out, names...)
		if err != nil {
			return out, false, err
		}
		if truncated {
			return out, true, nil
		}
	}
	return out, false, nil
}

// CanReach reports whether the review App is installed on project
// (owner/name): GET /repos/{owner}/{repo}/installation with the App's JWT.
// Not installed, or a malformed name, is false; a failure to tell is an
// error.
func (f *Forge) CanReach(ctx context.Context, project string) (bool, error) {
	owner, name, ok := strings.Cut(project, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, err := f.app.Installation(ctx, owner, name)
	if errors.Is(err, github.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
