package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/diffmap"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/threads"
)

// blockRuleTTL is how long threadsBlockMerge keeps what it read.
const blockRuleTTL = 10 * time.Minute

// inlineView is what the comment needs from the inline threads.
type inlineView struct {
	On     bool
	Inline map[string]bool
	Notes  map[string]string
}

// threadStore is what keeping the threads needs from the store.
type threadStore interface {
	InlineThreads(ctx context.Context, mr store.MRKey) ([]store.InlineThread, error)
	PutInlineThread(ctx context.Context, t store.InlineThread) error
	RebindInlineThread(ctx context.Context, mr store.MRKey, from, to string, line, endLine int, commit string) error
	SetInlineThreadState(ctx context.Context, mr store.MRKey, fingerprint, state string) error
	NoteInlineThreadLeftOut(ctx context.Context, mr store.MRKey, fingerprint string) error
	ClearInlineThreadLeftOut(ctx context.Context, mr store.MRKey, fingerprint string) error
	DeleteInlineThread(ctx context.Context, mr store.MRKey, fingerprint string) error
}

var _ threadStore = (*store.Store)(nil)

// inlineDeps are the collaborators of one inline sync, so that the work can be
// tested without a database or a platform.
type inlineDeps struct {
	forge  forge.Forge
	inline forge.Inline
	store  threadStore
	github bool
	// blocks reports whether the target branch requires threads resolved
	// before merge.
	blocks func(ctx context.Context) bool
	redact func(string) string
	log    *slog.Logger
}

// syncInline posts the findings of the run's review as threads on the lines
// they are about, and keeps the threads of the change in step with the review
// (resolving the ones whose finding is gone, opening them again when it
// comes back). It returns what the comment needs; the zero inlineView means
// the comment lists every finding, as it did before inline threads. It never
// returns an error and decides nothing about the status or the approval: what
// can't be done now is logged and left for the next publish, which starts
// again from what the platform shows.
func (e *Engine) syncInline(ctx context.Context, plat Platform, run store.Run, repo settings.Effective, repoOK bool, o policy.Outcome, log *slog.Logger) inlineView {
	if !repoOK || !repo.InlineFindings || o.FailureReason != "" || o.Result == nil ||
		o.Result.Status != "completed" || o.Result.MergeNote != "" {
		return inlineView{}
	}
	ft, ok := plat.Forge.(forge.Inline)
	if !ok {
		return inlineView{}
	}
	if plat.isGitHub() {
		p, found, err := e.Store.Project(ctx, run.MR.Instance, run.MR.ProjectID)
		if err != nil {
			log.Warn("not posting inline threads: couldn't read the repository's record", "error", e.redact(err.Error()))
			return inlineView{}
		}
		if !found || !p.InlineAvailable {
			log.Debug("not posting inline threads: the review App can't post them in this repository")
			return inlineView{}
		}
	}
	return runInline(ctx, inlineDeps{
		forge: plat.Forge, inline: ft, store: e.Store, github: plat.isGitHub(),
		blocks: func(ctx context.Context) bool { return e.threadsBlockMerge(ctx, ft, run) },
		redact: e.redact, log: log,
	}, run, o)
}

// runInline is syncInline once the review is known to be one to post inline.
func runInline(ctx context.Context, d inlineDeps, run store.Run, o policy.Outcome) inlineView {
	pid, number := run.MR.ProjectID, run.MR.IID
	ch, err := d.forge.GetChange(ctx, pid, number)
	if err != nil {
		d.log.Warn("not posting inline threads: couldn't read the change", "error", d.redact(err.Error()))
		return inlineView{}
	}
	if ch.State != forge.StateOpen || ch.HeadSHA != run.HeadSHA {
		// The review is of a commit the change has left: its lines may not be
		// there any more, and the newer head gets its own review.
		d.log.Info("not posting inline threads: the change is closed or has a newer head", "state", ch.State, "change_head", ch.HeadSHA)
		return inlineView{}
	}
	if ch.BaseSHA == "" || ch.StartSHA == "" {
		// GitLab hasn't computed the diff for this head yet (GitHub: no merge
		// base): there is nothing to anchor a thread to. The next publish
		// tries again.
		d.log.Info("not posting inline threads: the platform hasn't computed the diff of the change yet")
		return inlineView{}
	}
	files, err := d.inline.ChangeDiff(ctx, pid, number)
	if err != nil {
		d.log.Warn("not posting inline threads: couldn't read the diff of the change", "error", d.redact(err.Error()))
		return inlineView{}
	}
	onPlatform, err := d.inline.Threads(ctx, pid, number)
	if err != nil {
		d.log.Warn("not posting inline threads: couldn't list the threads on the change", "error", d.redact(err.Error()))
		return inlineView{}
	}
	rows, err := d.store.InlineThreads(ctx, run.MR)
	if err != nil {
		d.log.Warn("not posting inline threads: couldn't load the recorded threads", "error", d.redact(err.Error()))
		return inlineView{}
	}

	fs := buildFindings(o.Result, o.Dismissals, d.redact)
	diff := buildDiff(files, result.ChangedFiles(o.Result))
	set := joinThreads(run.MR, run.HeadSHA, rows, onPlatform, fs.byFP)
	for _, fp := range set.stale {
		// A person deleted the thread: the finding is a candidate again.
		if err := d.store.DeleteInlineThread(ctx, run.MR, fp); err != nil {
			d.log.Warn("couldn't forget a thread that is gone from the change", "error", d.redact(err.Error()))
		}
	}
	for _, t := range set.adopted {
		// Posted by an earlier publish whose record was lost.
		if err := d.store.PutInlineThread(ctx, t); err != nil {
			d.log.Warn("couldn't record a thread found on the change", "error", d.redact(err.Error()))
		}
	}

	mapper := newThreadMapper(ctx, d.inline, pid, run.HeadSHA)
	plan := threads.Decide(threads.Input{
		Head: run.HeadSHA, Findings: fs.findings, Dismissed: fs.dismissed, Skills: fs.skills,
		Diff: diff, Threads: set.list, Map: mapper.Map, OnlyBlocking: d.blocks(ctx),
	})
	x := &inlineRun{ctx: ctx, d: d, run: run, change: ch, fs: fs, diff: diff, set: set, mapper: mapper}
	return x.apply(plan)
}

// findingSet is the review's findings, as the planner and the renderer take
// them.
type findingSet struct {
	findings  []threads.Finding
	byFP      map[string]threads.Finding
	rendered  map[threads.Finding]render.Finding // redacted, to render a thread's body from
	skills    map[string]threads.Coverage
	dismissed map[string]threads.Dismissal
	multi     bool // the review ran more than one skill
}

// fingerprintOK matches the fingerprints a thread can carry (see
// render.InlineMarker). A finding with another one stays in the comment.
var fingerprintOK = regexp.MustCompile(`^[0-9a-zA-Z:_-]{1,128}$`)

// buildFindings reads the planner's findings, coverage and dismissals from a
// result. The findings are those left after the dismissals, with every text
// field redacted. A suggestion is kept for a suggestion block only when the
// redactor would leave it as it is (it can't be altered: what a thread applies
// is what it shows).
func buildFindings(r *result.Result, ds []render.Dismissal, redact func(string) string) findingSet {
	fs := findingSet{
		byFP:      map[string]threads.Finding{},
		rendered:  map[threads.Finding]render.Finding{},
		skills:    map[string]threads.Coverage{},
		dismissed: map[string]threads.Dismissal{},
		multi:     len(r.Skills) > 1,
	}
	for _, d := range ds {
		if _, ok := fs.dismissed[d.Fingerprint]; !ok {
			fs.dismissed[d.Fingerprint] = threads.Dismissal{By: safeReplyText(redact, d.By), Reason: safeReplyText(redact, d.Reason)}
		}
	}
	for i, s := range render.ApplyDismissals(result.ToRender(r), ds) {
		cov := threads.Coverage{Reviewed: map[string]bool{}}
		for _, p := range s.Reviewed {
			cov.Reviewed[p] = true
		}
		// Anything but a verdict that covers its files counts as failed, as the
		// policy reads it.
		cov.Failed = (s.Verdict != render.VerdictHasBlockingIssues && s.Verdict != render.VerdictNoBlockingIssues) ||
			len(r.Skills[i].Files.NotReviewed) > 0
		for _, n := range r.Skills[i].Notes {
			if strings.HasPrefix(n, result.LeftOutNotePrefix) {
				cov.LeftOut = true
			}
		}
		fs.skills[s.Name] = cov

		for _, f := range s.Findings {
			if !fingerprintOK.MatchString(f.Fingerprint) {
				continue
			}
			rf := f
			rf.Title, rf.Explanation, rf.SuggestedFix = neutralizeHTML(redact(f.Title)), neutralizeHTML(redact(f.Explanation)), neutralizeHTML(redact(f.SuggestedFix))
			rf.File = redact(f.File)
			hasSuggestion := f.Suggestion != "" && redact(f.Suggestion) == f.Suggestion
			if !hasSuggestion {
				rf.Suggestion = ""
			}
			tf := threads.Finding{
				Fingerprint: f.Fingerprint, ID: f.ID, Skill: s.Name, File: f.File,
				Severity: "blocking", Line: f.Line, EndLine: f.EndLine, HasSuggestion: hasSuggestion,
			}
			if f.Severity == render.Suggestion {
				tf.Severity = "suggestion"
			}
			fs.findings = append(fs.findings, tf)
			if _, ok := fs.byFP[tf.Fingerprint]; !ok {
				fs.byFP[tf.Fingerprint] = tf
			}
			fs.rendered[tf] = rf
		}
	}
	return fs
}

// rawCodeTag matches the start of a raw HTML <pre> or <code> element.
var rawCodeTag = regexp.MustCompile(`(?i)<(pre|code)\b`)

// neutralizeHTML defuses raw <pre> and <code> tags in model text, so that
// `<pre lang="suggestion:-20+20">` can't become a live suggestion block on
// GitHub or GitLab. The Suggestion field is the only text that may become one.
func neutralizeHTML(s string) string { return rawCodeTag.ReplaceAllString(s, "&lt;$1") }

// safeReplyText makes who dismissed a finding, or why, safe to post in a
// thread: on one line (a dashboard reason can hold newlines, which would let
// it open a code or suggestion fence in the bot's reply), quick actions
// defused, mentions broken and credentials redacted.
func safeReplyText(redact func(string) string, s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = policy.NeutralizeQuickActions(s)
	s = strings.ReplaceAll(s, "@", "@‍")
	return redact(s)
}

// buildDiff parses the change's files by path. Every file the review saw
// (changed) that the platform's diff lacks is added without hunks: a file the
// review looked at never counts as having left the change.
func buildDiff(files []forge.FileDiff, changed []string) map[string]diffmap.File {
	out := make(map[string]diffmap.File, len(files)+len(changed))
	for _, f := range files {
		out[f.Path] = diffmap.Parse(f.Path, f.OldPath, f.Patch, f.Deleted)
	}
	for _, p := range changed {
		if _, ok := out[p]; !ok {
			out[p] = diffmap.File{Path: p}
		}
	}
	return out
}

// threadSet is the pergent-owned threads of a change: the recorded ones the
// platform still shows, and those found on the platform with no record.
type threadSet struct {
	list    []threads.Thread
	refs    map[string]forge.ThreadRef    // by fingerprint
	rows    map[string]store.InlineThread // by fingerprint
	plat    map[string]forge.Thread       // by fingerprint, as the platform shows them
	stale   []string                      // fingerprints of recorded threads the platform no longer has
	adopted []store.InlineThread          // threads found without a record
}

// joinThreads joins the recorded threads with the platform's by thread ID. The
// platform says whether a thread is resolved. A recorded thread the platform
// no longer has was deleted by a person: it is stale. A thread of the
// platform with a finding marker and no record is adopted: open, with the
// skill of the finding that has its fingerprint (none if the review has no
// such finding), the platform's lines (the finding's if the platform has none)
// and the head as its commit.
func joinThreads(mr store.MRKey, head string, rows []store.InlineThread, onPlatform []forge.Thread, found map[string]threads.Finding) threadSet {
	set := threadSet{refs: map[string]forge.ThreadRef{}, rows: map[string]store.InlineThread{}, plat: map[string]forge.Thread{}}
	byID := map[string]forge.Thread{}
	for _, p := range onPlatform {
		if _, dup := byID[p.Ref.ID]; p.Ref.ID != "" && !dup {
			byID[p.Ref.ID] = p
		}
	}
	add := func(row store.InlineThread, p forge.Thread) {
		set.rows[row.Fingerprint] = row
		set.refs[row.Fingerprint] = forge.ThreadRef{ID: row.ThreadID, CommentID: row.CommentID}
		set.plat[row.Fingerprint] = p
		set.list = append(set.list, threads.Thread{
			Fingerprint: row.Fingerprint, Skill: row.Skill, File: row.File, Line: row.Line, EndLine: row.EndLine,
			Commit: row.Commit, State: threads.State(row.State), Resolved: p.Resolved, LeftOutNoted: row.LeftOutNoted,
		})
	}
	used := map[string]bool{}
	for _, row := range rows {
		p, ok := byID[row.ThreadID]
		if !ok {
			set.stale = append(set.stale, row.Fingerprint)
			continue
		}
		used[row.ThreadID] = true
		add(row, p)
	}
	for _, p := range onPlatform {
		if p.Ref.ID == "" || used[p.Ref.ID] {
			continue
		}
		fp, ok := render.InlineFingerprint(p.Body)
		if !ok {
			continue
		}
		if _, taken := set.rows[fp]; taken {
			continue
		}
		skill, line, end := "", p.Line, p.EndLine
		if f, ok := found[fp]; ok {
			skill = f.Skill
			if line <= 0 {
				line, end = f.Line, f.EndLine
			}
		}
		if end < line {
			end = line
		}
		used[p.Ref.ID] = true
		row := store.InlineThread{
			MR: mr, Fingerprint: fp, Skill: skill, File: p.Path, Line: line, EndLine: end, Commit: head,
			ThreadID: p.Ref.ID, CommentID: p.Ref.CommentID, State: string(threads.StateOpen),
		}
		set.adopted = append(set.adopted, row)
		add(row, p)
	}
	return set
}

// threadMapper gives the lines of a recorded thread at the head of the change.
type threadMapper struct {
	ctx       context.Context
	ft        forge.Inline
	projectID int64
	head      string
	compared  map[string]*comparison // by commit; nil when the compare failed
}

// comparison is the diff from one commit to the head, by the path a file had
// at that commit and by the path it has now.
type comparison struct {
	byOld map[string]cmpFile
}

type cmpFile struct {
	diffmap.File
	noPatch bool // a binary or too large file: its lines can't be followed
}

func newThreadMapper(ctx context.Context, ft forge.Inline, projectID int64, head string) *threadMapper {
	return &threadMapper{ctx: ctx, ft: ft, projectID: projectID, head: head, compared: map[string]*comparison{}}
}

// Map is threads.Input.Map. A thread of the head keeps its recorded lines.
// Otherwise the lines are followed through the diff from the thread's commit
// to the head, which is fetched once per commit and only when a thread of
// that commit is looked at. A file the comparison doesn't list is unchanged.
// A failed comparison, a file with no patch to follow and a line the change
// removed can't be mapped.
func (m *threadMapper) Map(t threads.Thread) (int, int, bool) {
	if t.Line <= 0 {
		return 0, 0, false
	}
	if t.Commit == m.head {
		return t.Line, t.EndLine, true
	}
	if t.Commit == "" {
		return 0, 0, false
	}
	cmp := m.compare(t.Commit)
	if cmp == nil {
		return 0, 0, false
	}
	f, changed := cmp.byOld[t.File]
	if !changed {
		return t.Line, t.EndLine, true
	}
	if f.noPatch {
		return 0, 0, false
	}
	line, ok := f.MapOld(t.Line)
	if !ok {
		return 0, 0, false
	}
	end := line
	if t.EndLine > t.Line {
		if end, ok = f.MapOld(t.EndLine); !ok {
			return 0, 0, false
		}
	}
	return line, end, true
}

// compare returns the comparison of commit with the head, nil when it can't be
// had. A commit is asked for once.
func (m *threadMapper) compare(commit string) *comparison {
	if c, done := m.compared[commit]; done {
		return c
	}
	var c *comparison
	if files, err := m.ft.CompareDiff(m.ctx, m.projectID, commit, m.head); err == nil {
		c = &comparison{byOld: map[string]cmpFile{}}
		for _, f := range files {
			old := f.OldPath
			if old == "" {
				old = f.Path
			}
			c.byOld[old] = cmpFile{File: diffmap.Parse(f.Path, f.OldPath, f.Patch, f.Deleted), noPatch: f.Patch == "" && !f.Deleted}
		}
	}
	m.compared[commit] = c
	return c
}

// inlineRun carries out a plan.
type inlineRun struct {
	ctx    context.Context
	d      inlineDeps
	run    store.Run
	change forge.Change
	fs     findingSet
	diff   map[string]diffmap.File
	set    threadSet
	mapper *threadMapper

	// moved maps the old fingerprint of a thread rebound in this plan to its
	// new one: Resolve and Reopen carry the thread as it was stored.
	moved map[string]string
}

// apply carries out plan, each action on its own and best effort: rebinds
// first (the other actions find the thread under its new fingerprint), then
// resolves, reopens and the new posts. An action that fails is logged and left
// for the next publish.
func (x *inlineRun) apply(plan threads.Plan) inlineView {
	v := inlineView{On: true, Inline: map[string]bool{}, Notes: map[string]string{}}
	for fp := range plan.Inline {
		v.Inline[fp] = true
	}
	for fp, n := range plan.Notes {
		v.Notes[fp] = n
	}
	x.moved = map[string]string{}
	x.rebind(plan.Rebind)
	x.resolve(plan.Resolve)
	x.note(plan.Note)
	x.back(plan.Back)
	x.reopen(plan.Reopen, v)
	x.post(plan.Post, v)
	return v
}

func (x *inlineRun) warn(msg string, err error, args ...any) {
	x.d.log.Warn(msg, append([]any{"error", x.d.redact(err.Error())}, args...)...)
}

// current is the fingerprint a plan's thread has now.
func (x *inlineRun) current(fp string) string {
	if to, ok := x.moved[fp]; ok {
		return to
	}
	return fp
}

func (x *inlineRun) rebind(rs []threads.Rebind) {
	for _, rb := range rs {
		from := rb.Thread.Fingerprint
		ref, ok := x.set.refs[from]
		if !ok {
			continue
		}
		if err := x.d.store.RebindInlineThread(x.ctx, x.run.MR, from, rb.Fingerprint, rb.Line, rb.EndLine, x.run.HeadSHA); err != nil {
			x.warn("couldn't move a thread to its reworded finding; will try again", err)
			continue
		}
		x.moved[from] = rb.Fingerprint
		x.set.refs[rb.Fingerprint] = ref
		delete(x.set.refs, from)
		x.set.plat[rb.Fingerprint] = x.set.plat[from]
		delete(x.set.plat, from)
		row := x.set.rows[from]
		row.Fingerprint, row.Line, row.EndLine, row.Commit = rb.Fingerprint, rb.Line, rb.EndLine, x.run.HeadSHA
		x.set.rows[rb.Fingerprint] = row
		delete(x.set.rows, from)
	}
}

func (x *inlineRun) resolve(rs []threads.Resolve) {
	pid, number := x.run.MR.ProjectID, x.run.MR.IID
	for _, r := range rs {
		if r.Thread.Resolved {
			continue // resolved on the platform already: not ours to touch
		}
		fp := x.current(r.Thread.Fingerprint)
		ref, ok := x.set.refs[fp]
		if !ok {
			continue
		}
		if err := x.d.inline.ReplyThread(x.ctx, pid, number, ref, r.Reply); err != nil {
			x.warn("couldn't reply on a thread to resolve; will try again", err)
			continue
		}
		if err := x.d.inline.ResolveThread(x.ctx, pid, number, ref, true); err != nil {
			x.warn("couldn't resolve a thread; will try again", err)
			continue
		}
		if err := x.d.store.SetInlineThreadState(x.ctx, x.run.MR, fp, string(r.State)); err != nil {
			x.warn("couldn't record a resolved thread", err)
		}
	}
}

// note tells a thread, once, that its finding was left out of the review. The
// thread stays open. A reply that fails is not recorded, so the next publish
// tries again.
func (x *inlineRun) note(ns []threads.Note) {
	pid, number := x.run.MR.ProjectID, x.run.MR.IID
	for _, n := range ns {
		if n.Thread.Resolved {
			continue // resolved on the platform already: not ours to touch
		}
		fp := x.current(n.Thread.Fingerprint)
		ref, ok := x.set.refs[fp]
		if !ok {
			continue
		}
		if err := x.d.inline.ReplyThread(x.ctx, pid, number, ref, n.Reply); err != nil {
			x.warn("couldn't reply on a thread whose finding was left out; will try again", err)
			continue
		}
		if err := x.d.store.NoteInlineThreadLeftOut(x.ctx, x.run.MR, fp); err != nil {
			x.warn("couldn't record a reply on a thread whose finding was left out", err)
		}
	}
}

// back tells a thread that was told its finding was left out that the finding
// is reported again, and clears the record. A reply that fails leaves the
// record, so the next publish tries again.
func (x *inlineRun) back(ns []threads.Note) {
	pid, number := x.run.MR.ProjectID, x.run.MR.IID
	for _, n := range ns {
		if n.Thread.Resolved {
			continue // resolved on the platform already: not ours to touch
		}
		fp := x.current(n.Thread.Fingerprint)
		ref, ok := x.set.refs[fp]
		if !ok {
			continue
		}
		if err := x.d.inline.ReplyThread(x.ctx, pid, number, ref, n.Reply); err != nil {
			x.warn("couldn't reply on a thread whose finding is back; will try again", err)
			continue
		}
		x.clearLeftOut(fp)
	}
}

// clearLeftOut forgets that the thread of fp was told its finding was left out.
func (x *inlineRun) clearLeftOut(fp string) {
	if err := x.d.store.ClearInlineThreadLeftOut(x.ctx, x.run.MR, fp); err != nil {
		x.warn("couldn't record that a thread's finding is back", err)
	}
}

// reopen opens the threads of findings that came back. When the thread can't
// be opened its finding goes back to the comment, with the failure noted.
func (x *inlineRun) reopen(rs []threads.Reopen, v inlineView) {
	pid, number := x.run.MR.ProjectID, x.run.MR.IID
	for _, r := range rs {
		fp := x.current(r.Thread.Fingerprint)
		ref, ok := x.set.refs[fp]
		if !ok {
			continue
		}
		if err := x.d.inline.ResolveThread(x.ctx, pid, number, ref, false); err != nil {
			x.warn("couldn't open a thread again; will try again", err)
			delete(v.Inline, fp)
			v.Notes[fp] = threads.NoteFailed
			continue
		}
		if err := x.d.inline.ReplyThread(x.ctx, pid, number, ref, r.Reply); err != nil {
			x.warn("couldn't reply on a thread opened again", err)
		}
		if r.Thread.LeftOutNoted {
			x.clearLeftOut(fp)
		}
		// Back to open. The commit moves to the head only along with the lines
		// at it, so a later look at the thread follows them from the right
		// commit; when they can't be had it stays where it was.
		row, haveRow := x.set.rows[fp]
		line, end, known := x.mapper.Map(r.Thread)
		if p := x.set.plat[fp]; !known && p.Line > 0 {
			line, end, known = p.Line, p.EndLine, true
		}
		if haveRow && known {
			if end < line {
				end = line
			}
			row.State, row.Line, row.EndLine, row.Commit = string(threads.StateOpen), line, end, x.run.HeadSHA
			if err := x.d.store.PutInlineThread(x.ctx, row); err != nil {
				x.warn("couldn't record a thread opened again", err)
			}
			continue
		}
		if err := x.d.store.SetInlineThreadState(x.ctx, x.run.MR, fp, string(threads.StateOpen)); err != nil {
			x.warn("couldn't record a thread opened again", err)
		}
	}
}

// post starts a thread for each finding of the plan. A finding whose thread
// is too long, or could not be posted, goes back to the comment with a note.
func (x *inlineRun) post(ps []threads.Post, v inlineView) {
	if len(ps) == 0 {
		return
	}
	limit := kube.CommentLimitGitLab
	if x.d.github {
		limit = kube.CommentLimitGitHub
	}
	mention := x.d.forge.Mention()

	var news []forge.NewThread
	var posted []threads.Post
	for _, p := range ps {
		fp := p.Finding.Fingerprint
		rf, ok := x.fs.rendered[p.Finding]
		if !ok {
			delete(v.Inline, fp)
			v.Notes[fp] = threads.NoteFailed
			continue
		}
		opts := render.InlineOptions{}
		if mention != "" {
			opts.DismissCommand = "@" + mention + " dismiss"
		}
		if x.fs.multi {
			opts.Skill = p.Finding.Skill
		}
		if p.Suggestion {
			opts.SuggestionInfo = "suggestion"
			if !x.d.github {
				opts.SuggestionInfo = fmt.Sprintf("suggestion:-0+%d", p.EndLine-p.Line)
			}
		}
		body := render.InlineComment(rf, opts)
		if len(body) > limit {
			delete(v.Inline, fp)
			v.Notes[fp] = threads.NoteTooLong
			continue
		}
		f := x.diff[p.Finding.File]
		oldLine, _ := f.OldLine(p.Line)
		news = append(news, forge.NewThread{
			Path: p.Finding.File, OldPath: f.OldPath, Line: p.Line, EndLine: p.EndLine, OldLine: oldLine, Body: body,
		})
		posted = append(posted, p)
	}
	if len(news) == 0 {
		return
	}

	refs, errs, err := x.d.inline.PostThreads(x.ctx, x.run.MR.ProjectID, x.change, news)
	if err != nil {
		x.warn("couldn't post the inline threads; the findings stay in the comment", err)
	}
	for i, p := range posted {
		fp := p.Finding.Fingerprint
		var perr error
		switch {
		case err != nil:
			perr = err
		case i >= len(errs) || i >= len(refs):
			perr = fmt.Errorf("the platform answered for %d of %d threads", min(len(errs), len(refs)), len(posted))
		case errs[i] != nil:
			perr = errs[i]
		case refs[i].ID == "":
			perr = errors.New("the platform gave the thread no ID")
		}
		if perr != nil {
			if err == nil {
				x.warn("couldn't post an inline thread; the finding stays in the comment", perr, "file", p.Finding.File, "line", p.Line)
			}
			delete(v.Inline, fp)
			v.Notes[fp] = threads.NoteFailed
			continue
		}
		row := store.InlineThread{
			MR: x.run.MR, Fingerprint: fp, Skill: p.Finding.Skill, File: p.Finding.File, Line: p.Line, EndLine: p.EndLine,
			Commit: x.run.HeadSHA, ThreadID: refs[i].ID, CommentID: refs[i].CommentID, State: string(threads.StateOpen),
		}
		if err := x.d.store.PutInlineThread(x.ctx, row); err != nil {
			// The thread is out; the next publish finds it on the platform.
			x.warn("couldn't record a posted thread", err)
		}
	}
}

// blockKey is what threadsBlockMerge caches by.
type blockKey struct {
	instance  string
	projectID int64
	branch    string
}

type blockRule struct {
	block bool
	at    time.Time
}

// threadsBlockMerge reports whether the run's target branch requires threads
// resolved before a merge, which makes pergent keep suggestions out of
// threads. The answer is kept for 10 minutes per instance, project and branch.
// A rule that can't be read counts as no rule, and is logged once per
// repository while pergent.sh runs; it isn't kept, so the next publish asks
// again.
func (e *Engine) threadsBlockMerge(ctx context.Context, ft forge.Inline, run store.Run) bool {
	key := blockKey{run.MR.Instance, run.MR.ProjectID, run.TargetBranch}
	now := e.now()
	e.blockMu.Lock()
	c, ok := e.blockRules[key]
	e.blockMu.Unlock()
	if ok && !now.Before(c.at) && now.Sub(c.at) < blockRuleTTL {
		return c.block
	}
	block, err := ft.ThreadsBlockMerge(ctx, run.MR.ProjectID, run.TargetBranch)
	if err != nil {
		rk := keyOf(run.MR.Instance, run.ProjectPath)
		e.blockMu.Lock()
		seen := e.blockUnread[rk]
		if e.blockUnread == nil {
			e.blockUnread = map[repoKey]bool{}
		}
		e.blockUnread[rk] = true
		e.blockMu.Unlock()
		if !seen {
			e.log().Info("couldn't read whether threads must be resolved before merge; treating it as not required",
				"instance", run.MR.Instance, "project", run.ProjectPath, "branch", run.TargetBranch, "error", e.redact(err.Error()))
		}
		return false
	}
	e.blockMu.Lock()
	if e.blockRules == nil {
		e.blockRules = map[blockKey]blockRule{}
	}
	e.blockRules[key] = blockRule{block: block, at: now}
	e.blockMu.Unlock()
	return block
}
