// Package gitlabforge maps a GitLab instance onto forge.Forge and
// forge.Approver. Each method is one call of the gitlab client, so the
// requests GitLab sees are the ones pergent.sh made before the engine spoke
// through forge.
package gitlabforge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
)

// Forge is the review bot of one GitLab instance.
type Forge struct {
	c *gitlab.Client
	// user is the review bot: FindComment only finds its notes.
	user gitlab.User
}

// NewForge returns the review bot's Forge; user is who c's token belongs to.
func NewForge(c *gitlab.Client, user gitlab.User) *Forge { return &Forge{c: c, user: user} }

// GetFile returns a file's raw content at ref. A missing file is nil, false, nil.
func (f *Forge) GetFile(ctx context.Context, projectID int64, path, ref string) ([]byte, bool, error) {
	b, ok, err := f.c.GetFile(ctx, projectID, path, ref)
	return b, ok, mapErr(err)
}

// ListTree lists one directory at ref. A missing directory is nil, nil.
func (f *Forge) ListTree(ctx context.Context, projectID int64, path, ref string) ([]forge.TreeEntry, error) {
	entries, err := f.c.ListTree(ctx, projectID, path, ref)
	return entries, mapErr(err)
}

// GetChange reads merge request iid. GitLab's "opened" is forge.StateOpen;
// the other states keep their names. Diff refs GitLab computed for an older
// head are not this head's, so BaseSHA and StartSHA stay empty (not ready)
// until diff_refs.head_sha is the merge request's sha.
func (f *Forge) GetChange(ctx context.Context, projectID, iid int64) (forge.Change, error) {
	mr, err := f.c.GetMergeRequest(ctx, projectID, iid)
	if err != nil {
		return forge.Change{}, mapErr(err)
	}
	ch := forge.Change{
		Number:       mr.IID,
		State:        forge.ChangeState(mr.State),
		Draft:        mr.Draft,
		HeadSHA:      mr.SHA,
		TargetBranch: mr.TargetBranch,
		Title:        mr.Title,
		Description:  mr.Description,
		WebURL:       mr.WebURL,
		SourceBranch: mr.SourceBranch,
		Author:       mr.Author.Username,
		AuthorID:     mr.Author.ID,
	}
	if mr.State == "opened" {
		ch.State = forge.StateOpen
	}
	if mr.DiffRefs.HeadSHA == mr.SHA {
		ch.BaseSHA, ch.StartSHA = mr.DiffRefs.BaseSHA, mr.DiffRefs.StartSHA
	}
	return ch, nil
}

// DefaultBranch returns the project's default branch, read with the review
// bot. A project it can't see is forge.ErrNotFound.
func (f *Forge) DefaultBranch(ctx context.Context, projectID int64) (string, error) {
	p, err := f.c.GetProject(ctx, strconv.FormatInt(projectID, 10))
	if err != nil {
		return "", mapErr(err)
	}
	return p.DefaultBranch, nil
}

var _ forge.SourceReader = (*Forge)(nil)

var _ forge.CommentReader = (*Forge)(nil)

// unreadable reports whether err says the review bot may not read what it
// asked for: GitLab's 404 (it hides what a user can't see) or 403 (a bot below
// Reporter, a restricted repository feature).
func unreadable(err error) bool {
	var ae *gitlab.APIError
	return errors.Is(err, gitlab.ErrNotFound) || (errors.As(err, &ae) && ae.Status == http.StatusForbidden)
}

// projectID resolves a project's full path ("group/project") with the review
// bot. A project the bot can't see (GitLab answers 404) is
// forge.ErrSourceNotFound.
func (f *Forge) projectID(ctx context.Context, project string) (int64, error) {
	project = strings.Trim(project, "/")
	if project == "" {
		return 0, forge.ErrSourceNotFound
	}
	p, err := f.c.GetProject(ctx, project)
	if unreadable(err) {
		return 0, forge.ErrSourceNotFound
	}
	if err != nil {
		return 0, err
	}
	if p.ID == 0 {
		return 0, errors.New("gitlabforge: project has no id")
	}
	return p.ID, nil
}

// ResolveRef returns the commit ref points at in the project, read with the
// review bot (Reporter access is enough; the project need not be switched on).
// A project or ref the bot can't read (404 or 403) is forge.ErrSourceNotFound.
func (f *Forge) ResolveRef(ctx context.Context, project, ref string) (string, error) {
	id, err := f.projectID(ctx, project)
	if err != nil {
		return "", err
	}
	commit, err := f.c.CommitSHA(ctx, id, ref)
	if unreadable(err) {
		return "", forge.ErrSourceNotFound
	}
	return commit, err
}

// ListTreeAt lists one directory of the project at commit. A missing
// directory is nil, nil; a project the bot can't read is
// forge.ErrSourceNotFound.
func (f *Forge) ListTreeAt(ctx context.Context, project, path, commit string) ([]forge.TreeEntry, error) {
	id, err := f.projectID(ctx, project)
	if err != nil {
		return nil, err
	}
	entries, err := f.c.ListTree(ctx, id, path, commit)
	if unreadable(err) {
		return nil, forge.ErrSourceNotFound
	}
	return entries, mapErr(err)
}

// GetFileAt returns a file of the project at commit, at most
// forge.MaxSourceFile bytes of it: a larger one is forge.ErrFileTooLarge. A
// missing file is nil, false, nil; a project the bot can't read is
// forge.ErrSourceNotFound.
func (f *Forge) GetFileAt(ctx context.Context, project, path, commit string) ([]byte, bool, error) {
	id, err := f.projectID(ctx, project)
	if err != nil {
		return nil, false, err
	}
	b, ok, err := f.c.GetFileMax(ctx, id, path, commit, forge.MaxSourceFile)
	if errors.Is(err, gitlab.ErrTooLarge) {
		return nil, false, forge.ErrFileTooLarge
	}
	if unreadable(err) {
		return nil, false, forge.ErrSourceNotFound
	}
	return b, ok, mapErr(err)
}

// SetStatus sets the "pergent" commit status on sha.
func (f *Forge) SetStatus(ctx context.Context, projectID int64, sha string, s forge.StatusState, description string) error {
	return mapErr(f.c.SetCommitStatus(ctx, projectID, sha, State(s), description))
}

// State maps a status onto GitLab's commit status states. GitLab has no
// neutral state, so neutral is "skipped". Anything else, unknown states
// included, is "failed": a state pergent.sh doesn't know is never success.
func State(s forge.StatusState) gitlab.StatusState {
	switch s {
	case forge.StatusRunning:
		return gitlab.StatusRunning
	case forge.StatusSuccess:
		return gitlab.StatusSuccess
	case forge.StatusNeutral:
		return gitlab.StatusSkipped
	case forge.StatusCanceled:
		return gitlab.StatusCanceled
	default:
		return gitlab.StatusFailed
	}
}

// FindComment returns the review bot's first note on the merge request that
// contains marker; notes of other users are never matched.
func (f *Forge) FindComment(ctx context.Context, projectID, iid int64, marker string) (int64, bool, error) {
	id, found, err := f.c.FindNote(ctx, projectID, iid, f.user.ID, marker)
	return id, found, mapErr(err)
}

// CreateComment adds a note to the merge request and returns its ID.
func (f *Forge) CreateComment(ctx context.Context, projectID, iid int64, body string) (int64, error) {
	id, err := f.c.CreateNote(ctx, projectID, iid, body)
	return id, mapErr(err)
}

// UpdateComment replaces a note's body. A deleted note is forge.ErrNotFound.
func (f *Forge) UpdateComment(ctx context.Context, projectID, iid, noteID int64, body string) error {
	return mapErr(f.c.UpdateNote(ctx, projectID, iid, noteID, body))
}

// Bot is the review bot.
func (f *Forge) Bot() forge.Identity {
	return forge.Identity{ID: f.user.ID, Username: f.user.Username}
}

// Mention is the review bot's username: people write @username.
func (f *Forge) Mention() string { return f.user.Username }

// Award emoji names of the reactions.
var emoji = map[forge.Reaction]string{
	forge.ReactSeen:    "eyes",
	forge.ReactDone:    "thumbsup",
	forge.ReactRefused: "thumbsdown",
}

// React awards the reaction's emoji to a note as the review bot. A reaction
// pergent.sh doesn't know is an error and nothing is sent.
func (f *Forge) React(ctx context.Context, projectID, iid, noteID int64, r forge.Reaction) error {
	name, ok := emoji[r]
	if !ok {
		return fmt.Errorf("gitlabforge: unknown reaction %q", r)
	}
	return mapErr(f.c.AwardNoteEmoji(ctx, projectID, iid, noteID, name))
}

// Reply adds a plain note to the merge request.
func (f *Forge) Reply(ctx context.Context, projectID, iid int64, body string) error {
	_, err := f.CreateComment(ctx, projectID, iid, body)
	return err
}

// developer is GitLab's Developer access level.
const developer = 30

// AccessLevel maps the user's level in the project, inherited from its
// groups included, onto forge.Access: below Developer reads, Developer
// writes, Maintainer and Owner maintain. A user who isn't a member, or
// without an ID, is AccessNone. username is not used.
func (f *Forge) AccessLevel(ctx context.Context, projectID int64, _ string, userID int64) (forge.Access, error) {
	if userID == 0 {
		return forge.AccessNone, nil
	}
	n, err := f.c.MemberAccessLevel(ctx, projectID, userID)
	if err != nil {
		return forge.AccessNone, mapErr(err)
	}
	switch {
	case n >= maintainer:
		return forge.AccessMaintain, nil
	case n >= developer:
		return forge.AccessWrite, nil
	case n > 0:
		return forge.AccessRead, nil
	}
	return forge.AccessNone, nil
}

// IsBot reads the user from GitLab and tells whether it marks it as a bot. A
// user without an ID is not one (it has no access either).
func (f *Forge) IsBot(ctx context.Context, userID int64) (bool, error) {
	if userID == 0 {
		return false, nil
	}
	bot, err := f.c.UserIsBot(ctx, userID)
	return bot, mapErr(err)
}

// Visibility is not supported on GitLab: linked repositories are GitHub only.
func (f *Forge) Visibility(context.Context, string) (forge.Visibility, error) {
	return "", forge.ErrUnsupported
}

// Comment reads a note of merge request iid back from GitLab.
func (f *Forge) Comment(ctx context.Context, projectID, iid, noteID int64) (forge.Comment, error) {
	n, err := f.c.GetNote(ctx, projectID, iid, noteID)
	if err != nil {
		return forge.Comment{}, mapErr(err)
	}
	return forge.Comment{AuthorID: n.Author.ID, Author: n.Author.Username, Body: n.Body, System: n.System}, nil
}

// ChangeRef is the ref GitLab keeps merge request iid's head at.
func (f *Forge) ChangeRef(iid int64) string {
	return "refs/merge-requests/" + strconv.FormatInt(iid, 10) + "/head"
}

// Approver is the approve bot of one GitLab instance.
type Approver struct {
	c *gitlab.Client
	// user is the approve bot, looked for among the approvers.
	user gitlab.User
}

// NewApprover returns the approve bot's Approver; user is who c's token
// belongs to.
func NewApprover(c *gitlab.Client, user gitlab.User) *Approver { return &Approver{c: c, user: user} }

// Approve approves the merge request at sha. GitLab refuses (409) when the
// head is no longer sha: forge.ErrHeadMoved.
func (a *Approver) Approve(ctx context.Context, projectID, iid int64, sha string) error {
	return mapErr(a.c.Approve(ctx, projectID, iid, sha))
}

// Unapprove withdraws the approve bot's approval; none to withdraw is nil.
func (a *Approver) Unapprove(ctx context.Context, projectID, iid int64) error {
	return mapErr(a.c.Unapprove(ctx, projectID, iid))
}

// Bot is the approve bot.
func (a *Approver) Bot() forge.Identity {
	return forge.Identity{ID: a.user.ID, Username: a.user.Username}
}

// HasApproved reports whether the approve bot's user is among the merge
// request's approvers. Without a known user ID it is false, and GitLab isn't
// asked. sha is ignored: GitLab lists approvals of the merge request, not of
// a commit.
func (a *Approver) HasApproved(ctx context.Context, projectID, iid int64, _ string) (bool, error) {
	if a.user.ID == 0 {
		return false, nil
	}
	ids, err := a.c.ApprovedBy(ctx, projectID, iid)
	if err != nil {
		return false, mapErr(err)
	}
	for _, id := range ids {
		if id == a.user.ID {
			return true, nil
		}
	}
	return false, nil
}

// mapErr makes the client's not-found and head-moved errors match forge's
// sentinels too. The message stays the client's, since it can end up in a
// failure reason or a log line.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitlab.ErrNotFound):
		return &mapped{err: err, sentinel: forge.ErrNotFound}
	case errors.Is(err, gitlab.ErrHeadMoved):
		return &mapped{err: err, sentinel: forge.ErrHeadMoved}
	default:
		return err
	}
}

type mapped struct{ err, sentinel error }

func (m *mapped) Error() string   { return m.err.Error() }
func (m *mapped) Unwrap() []error { return []error{m.err, m.sentinel} }

// maintainer is GitLab's Maintainer access level: the review bot needs it to
// create a project's webhook and deploy token.
const maintainer = 40

// callTimeout bounds each request of ReachableRepos and CanReach, which
// serve the dashboard.
const callTimeout = 10 * time.Second

// ReachableRepos returns the paths of the projects the review bot can set up
// (it is Maintainer or above there) whose path, name or namespace holds
// search ("" for all), up to limit; truncated is true when there were more.
// Each page's request has 10 s; on an error it also returns the paths read
// before it.
func (f *Forge) ReachableRepos(ctx context.Context, search string, limit int) ([]string, bool, error) {
	return f.c.ProjectPaths(ctx, gitlab.ProjectListOptions{MinAccessLevel: maintainer, Search: search,
		Limit: limit, CallTimeout: callTimeout})
}

// ReachablePage returns the page (1-based) of perPage of the projects
// ReachableRepos lists, ordered by ID, and whether GitLab has a later page.
// It makes the one request, which has 10 s.
func (f *Forge) ReachablePage(ctx context.Context, search string, page, perPage int) ([]string, bool, error) {
	return f.c.ProjectPage(ctx, gitlab.ProjectPageOptions{MinAccessLevel: maintainer, Search: search,
		Page: page, PerPage: perPage, CallTimeout: callTimeout})
}

// CanReach reports whether the review bot can set project up: whether
// ReachableRepos would list it. See ReachableName.
func (f *Forge) CanReach(ctx context.Context, project string) (bool, error) {
	_, ok, err := f.ReachableName(ctx, project)
	return ok, err
}

// ReachableName is CanReach that also returns the project's path as GitLab
// spells it (GitLab paths are unique whatever the case), the name to switch
// it on under. It looks the project up by its path, and then asks the project
// listing the picker reads (min_access_level, narrowed to that one ID), because
// the project's own permissions leave out Maintainer access gained through an
// ancestor group or a share, and the picker shows those. Two requests, with no
// search and no paging. A project it can't see is false; so is a path GitLab
// answers with another project's (an old path of a renamed project, or an ID);
// a failure to tell is an error.
func (f *Forge) ReachableName(ctx context.Context, project string) (string, bool, error) {
	if project == "" || strings.HasSuffix(project, "/") {
		return "", false, nil
	}
	notReached := func(err error) bool {
		var ae *gitlab.APIError
		return errors.Is(err, gitlab.ErrNotFound) || (errors.As(err, &ae) && ae.Status == http.StatusForbidden)
	}
	pctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	p, err := f.c.GetProject(pctx, project)
	if notReached(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if p.ID == 0 || !strings.EqualFold(p.PathWithNamespace, project) {
		return "", false, nil
	}
	lctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	ok, err := f.c.HasMemberAccess(lctx, p.ID, maintainer)
	if notReached(err) {
		return "", false, nil
	}
	if err != nil || !ok {
		return "", false, err
	}
	return p.PathWithNamespace, true, nil
}
