package githubforge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/github"
)

var _ forge.Inline = (*Forge)(nil)

// ChangeDiff returns the pull request's files at its head.
func (f *Forge) ChangeDiff(ctx context.Context, repoID, number int64) ([]forge.FileDiff, error) {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return nil, err
	}
	files, err := c.PullFiles(ctx, r.Owner, r.Name, number)
	return files, mapErr(err)
}

// CompareDiff returns the files that differ between two commits.
func (f *Forge) CompareDiff(ctx context.Context, repoID int64, from, to string) ([]forge.FileDiff, error) {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return nil, err
	}
	files, err := c.CompareFiles(ctx, r.Owner, r.Name, from, to)
	return files, mapErr(err)
}

// ChangedSince returns the paths changed from one commit to another. It is an
// error when from is not an ancestor of to (github.ErrNotAncestor) or when
// GitHub may not list every file (github.ErrPartialDiff).
func (f *Forge) ChangedSince(ctx context.Context, repoID int64, from, to string) ([]string, error) {
	files, err := f.CompareDiff(ctx, repoID, from, to)
	if err != nil {
		return nil, err
	}
	return forge.Paths(files), nil
}

// isBot reports whether an author is the review App's bot: a Bot account
// (a person can register a User named like the App's slug) whose login is the
// slug. REST shows it as slug[bot]; GraphQL may leave the [bot] off an App's
// login. GitHub logins are case-insensitive.
func (f *Forge) isBot(login, typename string) bool {
	login = strings.TrimSuffix(strings.ToLower(login), "[bot]")
	return typename == "Bot" && login != "" && login == strings.ToLower(f.slug)
}

// ours reports whether t is a thread of the review App's with a finding
// marker: anyone can paste a marker, so the first comment must be the App's.
func (f *Forge) ours(t github.ReviewThread) (fingerprint string, ok bool) {
	if !f.isBot(t.Author, t.AuthorType) {
		return "", false
	}
	return render.InlineFingerprint(t.Body)
}

// Threads lists the review App's own threads on the pull request: those whose
// first comment is the App's and carries a finding marker. Line and EndLine
// are 0 for an outdated thread.
func (f *Forge) Threads(ctx context.Context, repoID, number int64) ([]forge.Thread, error) {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return nil, err
	}
	all, err := c.ReviewThreads(ctx, r.Owner, r.Name, number)
	if err != nil {
		return nil, mapErr(err)
	}
	var out []forge.Thread
	for _, t := range all {
		if _, ok := f.ours(t); !ok {
			continue
		}
		th := forge.Thread{
			Ref:      forge.ThreadRef{ID: t.ID, CommentID: t.CommentID},
			Body:     t.Body,
			Path:     t.Path,
			Resolved: t.Resolved,
		}
		if !t.Outdated {
			th.EndLine = t.Line
			th.Line = t.Line
			if t.StartLine > 0 {
				th.Line = t.StartLine
			}
		}
		out = append(out, th)
	}
	return out, nil
}

// reviewBody is the body of the review that carries the threads; sha is the
// head the threads were posted at.
func reviewBody(sha string) string {
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return "Findings on the changed lines, from the review of `" + sha + "`."
}

// PostThreads posts the threads as one COMMENT review on ch's head. When
// GitHub refuses it with a 422 (a line outside the diff), each thread is
// posted as its own review, so a bad one fails alone. A GitHub review shows
// no ID per thread, so the refs are found by listing the pull request's
// threads afterwards and matching each posted body's finding marker. A thread
// without a marker is not posted: nothing could find it again.
func (f *Forge) PostThreads(ctx context.Context, repoID int64, ch forge.Change, threads []forge.NewThread) ([]forge.ThreadRef, []error, error) {
	refs := make([]forge.ThreadRef, len(threads))
	errs := make([]error, len(threads))
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return refs, errs, err
	}
	fps := make([]string, len(threads))
	var post []int // indexes of the threads to post
	for i, t := range threads {
		fp, ok := render.InlineFingerprint(t.Body)
		if !ok {
			errs[i] = errors.New("githubforge: the thread has no finding marker, so it would not be found again")
			continue
		}
		fps[i] = fp
		post = append(post, i)
	}
	if len(post) == 0 {
		return refs, errs, nil
	}
	comment := func(i int) github.ReviewComment {
		t := threads[i]
		return github.ReviewComment{Path: t.Path, Line: t.Line, EndLine: t.EndLine, Body: t.Body}
	}
	body := reviewBody(ch.HeadSHA)
	all := make([]github.ReviewComment, len(post))
	for k, i := range post {
		all[k] = comment(i)
	}
	posted := post
	_, err = c.CreateReviewComments(ctx, r.Owner, r.Name, ch.Number, ch.HeadSHA, body, all)
	switch {
	case err == nil:
	case status(err) == http.StatusUnprocessableEntity && len(post) == 1:
		errs[post[0]] = mapErr(err)
		return refs, errs, nil
	case status(err) == http.StatusUnprocessableEntity:
		posted = nil
		for _, i := range post {
			// A thread that is not attempted has its error in errs.
			if ctx.Err() != nil {
				errs[i] = ctx.Err()
				continue
			}
			_, perr := c.CreateReviewComments(ctx, r.Owner, r.Name, ch.Number, ch.HeadSHA, body, []github.ReviewComment{comment(i)})
			if perr != nil {
				errs[i] = mapErr(perr)
				continue
			}
			posted = append(posted, i)
		}
	default:
		return refs, errs, mapErr(err)
	}
	if len(posted) == 0 {
		return refs, errs, nil
	}

	listed, err := c.ReviewThreads(ctx, r.Owner, r.Name, ch.Number)
	if err != nil {
		return refs, errs, fmt.Errorf("githubforge: threads were posted but listing them failed: %w", mapErr(err))
	}
	for _, i := range posted {
		var found *github.ReviewThread
		for k := range listed {
			t := &listed[k]
			if fp, ok := f.ours(*t); !ok || fp != fps[i] || t.Path != threads[i].Path {
				continue
			}
			// The same finding may have an older thread; the new one is the
			// latest comment.
			if found == nil || t.CommentID > found.CommentID {
				found = t
			}
		}
		if found == nil {
			errs[i] = errors.New("githubforge: the thread was posted but GitHub does not list it")
			continue
		}
		refs[i] = forge.ThreadRef{ID: found.ID, CommentID: found.CommentID}
	}
	return refs, errs, nil
}

// ReplyThread replies to the thread's first comment.
func (f *Forge) ReplyThread(ctx context.Context, repoID, number int64, t forge.ThreadRef, body string) error {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return err
	}
	return mapErr(c.ReplyReviewComment(ctx, r.Owner, r.Name, number, t.CommentID, body))
}

// ResolveThread resolves the thread, or reopens it when resolved is false.
func (f *Forge) ResolveThread(ctx context.Context, repoID, _ int64, t forge.ThreadRef, resolved bool) error {
	c, _, err := f.client(ctx, repoID)
	if err != nil {
		return err
	}
	if resolved {
		return mapErr(c.ResolveReviewThread(ctx, t.ID))
	}
	return mapErr(c.UnresolveReviewThread(ctx, t.ID))
}

// ReactThread adds the reaction to a review comment as the review App. A
// reaction pergent.sh doesn't know is an error and nothing is sent.
func (f *Forge) ReactThread(ctx context.Context, repoID, _, commentID int64, reaction forge.Reaction) error {
	emoji, ok := content[reaction]
	if !ok {
		return fmt.Errorf("githubforge: unknown reaction %q", reaction)
	}
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return err
	}
	return mapErr(c.ReactToReviewComment(ctx, r.Owner, r.Name, commentID, emoji))
}

// ThreadsBlockMerge reports whether a ruleset or the branch's classic
// protection requires every review thread resolved before merging into
// branch. Reading classic protection needs the Administration permission, so
// an App without it gets an error when no ruleset says yes.
func (f *Forge) ThreadsBlockMerge(ctx context.Context, repoID int64, branch string) (bool, error) {
	c, r, err := f.client(ctx, repoID)
	if err != nil {
		return false, err
	}
	required, err := c.ThreadResolutionRequired(ctx, r.Owner, r.Name, branch)
	return required, mapErr(err)
}
