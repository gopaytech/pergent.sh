package gitlabforge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
)

var _ forge.Inline = (*Forge)(nil)

// fileDiffs maps GitLab's diffs onto forge.FileDiff. GitLab leaves a binary
// or too large file's diff empty, so its Patch is empty.
func fileDiffs(in []gitlab.Diff) []forge.FileDiff {
	out := make([]forge.FileDiff, len(in))
	for i, d := range in {
		out[i] = forge.FileDiff{Path: d.NewPath, OldPath: d.OldPath, Deleted: d.DeletedFile, Patch: d.Diff}
	}
	return out
}

// ChangeDiff returns the merge request's files at its head.
func (f *Forge) ChangeDiff(ctx context.Context, projectID, iid int64) ([]forge.FileDiff, error) {
	diffs, err := f.c.MergeRequestDiffs(ctx, projectID, iid)
	if err != nil {
		return nil, mapErr(err)
	}
	return fileDiffs(diffs), nil
}

// CompareDiff returns the files that differ between two commits.
func (f *Forge) CompareDiff(ctx context.Context, projectID int64, from, to string) ([]forge.FileDiff, error) {
	diffs, err := f.c.Compare(ctx, projectID, from, to)
	if err != nil {
		return nil, mapErr(err)
	}
	return fileDiffs(diffs), nil
}

// ChangedSince returns the paths changed from one commit to another. It is an
// error when from is not an ancestor of to (their merge base is not from) or
// when GitLab may not list every file (gitlab.ErrPartialDiff).
func (f *Forge) ChangedSince(ctx context.Context, projectID int64, from, to string) ([]string, error) {
	base, err := f.c.MergeBase(ctx, projectID, from, to)
	if err != nil {
		return nil, mapErr(err)
	}
	if !strings.EqualFold(base, from) {
		return nil, fmt.Errorf("gitlabforge: %s is not an ancestor of %s (their merge base is %s)", from, to, base)
	}
	files, err := f.CompareDiff(ctx, projectID, from, to)
	if err != nil {
		return nil, err
	}
	return forge.Paths(files), nil
}

// Threads lists the review bot's own threads on the merge request: diff
// discussions whose first note is the bot's and carries a finding marker
// (anyone can paste a marker, so the author is checked). Line is the first
// note's new line and EndLine the end of its line range, or Line when it has
// none. A discussion is resolved when its first note is.
func (f *Forge) Threads(ctx context.Context, projectID, iid int64) ([]forge.Thread, error) {
	if f.user.ID == 0 {
		// Every author would pass for the bot, or none: not telling is better
		// than a wrong list.
		return nil, errors.New("gitlabforge: the review bot's user ID is not known, so its threads can't be told from others")
	}
	all, err := f.c.Discussions(ctx, projectID, iid)
	if err != nil {
		return nil, mapErr(err)
	}
	var out []forge.Thread
	for _, d := range all {
		if len(d.Notes) == 0 {
			continue
		}
		n := d.Notes[0]
		if n.Author.ID != f.user.ID || n.Position == nil {
			continue
		}
		if _, ok := render.InlineFingerprint(n.Body); !ok {
			continue
		}
		th := forge.Thread{
			Ref:      forge.ThreadRef{ID: d.ID, CommentID: n.ID},
			Body:     n.Body,
			Path:     n.Position.NewPath,
			Line:     n.Position.NewLine,
			EndLine:  n.Position.NewLine,
			Resolved: n.Resolvable && n.Resolved,
		}
		if lr := n.Position.LineRange; lr != nil && lr.End.NewLine >= th.Line {
			th.EndLine = lr.End.NewLine
		}
		out = append(out, th)
	}
	return out, nil
}

// PostThreads starts one discussion per thread at ch's head, anchored on
// Line alone (a multi-line finding's range lives in its suggestion, not in
// the position): new_line, plus old_line for an unchanged line. Without all
// three of ch's diff refs GitLab would anchor the discussion to nothing, so
// the call fails and nothing is posted. A thread without a finding marker is
// not posted either (nothing could find it again). Each discussion is created
// with one request, never retried: a retry after a lost reply would start a
// second thread. A thread GitLab refuses (a line
// outside the diff) fails alone, in errs; once ctx is done the threads not
// yet tried have its error.
func (f *Forge) PostThreads(ctx context.Context, projectID int64, ch forge.Change, threads []forge.NewThread) ([]forge.ThreadRef, []error, error) {
	refs := make([]forge.ThreadRef, len(threads))
	errs := make([]error, len(threads))
	if ch.BaseSHA == "" || ch.StartSHA == "" || ch.HeadSHA == "" {
		return refs, errs, errors.New("gitlabforge: the merge request's diff refs are not ready yet")
	}
	for i, t := range threads {
		if err := ctx.Err(); err != nil {
			for j := i; j < len(threads); j++ {
				errs[j] = err
			}
			break
		}
		if _, ok := render.InlineFingerprint(t.Body); !ok {
			errs[i] = errors.New("gitlabforge: the thread has no finding marker, so it would not be found again")
			continue
		}
		if t.Path == "" || t.Line <= 0 {
			errs[i] = errors.New("gitlabforge: the thread has no file or line to anchor on")
			continue
		}
		oldPath := t.OldPath
		if oldPath == "" {
			oldPath = t.Path
		}
		id, noteID, err := f.c.CreateDiffDiscussion(ctx, projectID, ch.Number, t.Body, gitlab.Position{
			BaseSHA: ch.BaseSHA, StartSHA: ch.StartSHA, HeadSHA: ch.HeadSHA,
			OldPath: oldPath, NewPath: t.Path, NewLine: t.Line, OldLine: t.OldLine,
		})
		if err != nil {
			errs[i] = mapErr(err)
			continue
		}
		refs[i] = forge.ThreadRef{ID: id, CommentID: noteID}
	}
	return refs, errs, nil
}

// ReplyThread adds a note to the discussion.
func (f *Forge) ReplyThread(ctx context.Context, projectID, iid int64, t forge.ThreadRef, body string) error {
	return mapErr(f.c.ReplyDiscussion(ctx, projectID, iid, t.ID, body))
}

// ResolveThread resolves the discussion, or reopens it when resolved is false.
func (f *Forge) ResolveThread(ctx context.Context, projectID, iid int64, t forge.ThreadRef, resolved bool) error {
	return mapErr(f.c.ResolveDiscussion(ctx, projectID, iid, t.ID, resolved))
}

// ReactThread awards the reaction's emoji to a note: GitLab's award emoji
// work on any note, a discussion's included.
func (f *Forge) ReactThread(ctx context.Context, projectID, iid, noteID int64, reaction forge.Reaction) error {
	return f.React(ctx, projectID, iid, noteID, reaction)
}

// ThreadsBlockMerge reports whether the project requires every thread
// resolved before a merge ("all threads must be resolved"). It is a project
// setting: the branch makes no difference on GitLab. A project that does not
// show the setting is false.
func (f *Forge) ThreadsBlockMerge(ctx context.Context, projectID int64, _ string) (bool, error) {
	p, err := f.c.GetProject(ctx, strconv.FormatInt(projectID, 10))
	if err != nil {
		return false, mapErr(err)
	}
	return p.OnlyAllowMergeIfAllDiscussionsAreResolved != nil && *p.OnlyAllowMergeIfAllDiscussionsAreResolved, nil
}
