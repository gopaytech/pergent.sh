// Package gitlabforge maps a GitLab instance onto forge.Forge and
// forge.Approver. Each method is one call of the gitlab client, so the
// requests GitLab sees are the ones pergent.sh made before the engine spoke
// through forge.
package gitlabforge

import (
	"context"
	"errors"
	"strconv"

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
		WebURL:       mr.WebURL,
		SourceBranch: mr.SourceBranch,
		Author:       mr.Author.Username,
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
