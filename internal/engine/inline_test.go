package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/forge/gitlabforge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/threads"
)

// The tests here need no database: they run the inline sync against a fake
// platform (fakeInline, or the fake GitLab behind the real GitLab adapter) and
// an in-memory record of the threads. The engine tests further down run the
// same work through a publish.

const (
	fpBlocking   = "aaaaaaaaaaaa"
	fpSuggestion = "bbbbbbbbbbbb"
	fpThird      = "cccccccccccc"
	secretValue  = "tok-secret-value"
)

// queryPatch is the diff of db/query.go: new lines 10 to 14, with 11 and 12
// added; 10, 13 and 14 are unchanged (old lines 10, 11 and 12).
const queryPatch = "@@ -10,3 +10,5 @@\n a\n+b\n+c\n d\n e"

// redactSecret is a redactor that knows one secret.
func redactSecret(s string) string { return strings.ReplaceAll(s, secretValue, "[redacted]") }

// memThreads is the thread record in memory.
type memThreads struct {
	mu     sync.Mutex
	rows   map[string]store.InlineThread
	events *[]string
}

func newMemThreads(events *[]string, rows ...store.InlineThread) *memThreads {
	m := &memThreads{rows: map[string]store.InlineThread{}, events: events}
	for _, r := range rows {
		m.rows[r.Fingerprint] = r
	}
	return m
}

func (m *memThreads) note(format string, args ...any) {
	if m.events != nil {
		*m.events = append(*m.events, fmt.Sprintf(format, args...))
	}
}

func (m *memThreads) InlineThreads(_ context.Context, _ store.MRKey) ([]store.InlineThread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.InlineThread
	for _, r := range m.rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out, nil
}

func (m *memThreads) PutInlineThread(_ context.Context, t store.InlineThread) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.note("store put %s %s", t.Fingerprint, t.State)
	m.rows[t.Fingerprint] = t
	return nil
}

func (m *memThreads) RebindInlineThread(_ context.Context, _ store.MRKey, from, to string, line, endLine int, commit string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.note("store rebind %s %s", from, to)
	r, ok := m.rows[from]
	if !ok {
		return nil
	}
	if _, taken := m.rows[to]; taken {
		return errors.New("a thread has that fingerprint")
	}
	delete(m.rows, from)
	r.Fingerprint, r.Line, r.EndLine, r.Commit = to, line, endLine, commit
	m.rows[to] = r
	return nil
}

func (m *memThreads) SetInlineThreadState(_ context.Context, _ store.MRKey, fp, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.note("store state %s %s", fp, state)
	if r, ok := m.rows[fp]; ok {
		r.State = state
		m.rows[fp] = r
	}
	return nil
}

func (m *memThreads) DeleteInlineThread(_ context.Context, _ store.MRKey, fp string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.note("store delete %s", fp)
	delete(m.rows, fp)
	return nil
}

func (m *memThreads) Row(fp string) (store.InlineThread, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[fp]
	return r, ok
}

// fakeInline is a forge.Inline that records its calls in order.
type fakeInline struct {
	mu     sync.Mutex
	events *[]string

	diff    []forge.FileDiff
	diffErr error
	// compare answers CompareDiff; nil is an empty comparison.
	compare  func(from, to string) ([]forge.FileDiff, error)
	compares []string
	// changed and changedErr answer ChangedSince.
	changed    []string
	changedErr error
	threads    []forge.Thread
	threadsErr error

	posted    [][]forge.NewThread
	postRefs  []forge.ThreadRef // for the threads of a post, in order; missing ones get a generated ref
	postErrs  []error
	postErr   error
	replyErr  map[string]error // by thread ID
	resolveOn map[string]error // by thread ID, for resolving
	reopenOn  map[string]error // by thread ID, for unresolving

	block    bool
	blockErr error
}

func (f *fakeInline) note(format string, args ...any) {
	if f.events != nil {
		*f.events = append(*f.events, fmt.Sprintf(format, args...))
	}
}

func (f *fakeInline) ChangeDiff(context.Context, int64, int64) ([]forge.FileDiff, error) {
	return f.diff, f.diffErr
}

func (f *fakeInline) CompareDiff(_ context.Context, _ int64, from, to string) ([]forge.FileDiff, error) {
	f.mu.Lock()
	f.compares = append(f.compares, from+".."+to)
	f.mu.Unlock()
	if f.compare == nil {
		return nil, nil
	}
	return f.compare(from, to)
}

func (f *fakeInline) ChangedSince(context.Context, int64, string, string) ([]string, error) {
	return f.changed, f.changedErr
}

func (f *fakeInline) Threads(context.Context, int64, int64) ([]forge.Thread, error) {
	return f.threads, f.threadsErr
}

func (f *fakeInline) PostThreads(_ context.Context, _ int64, _ forge.Change, ts []forge.NewThread) ([]forge.ThreadRef, []error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("post %d", len(ts))
	f.posted = append(f.posted, ts)
	if f.postErr != nil {
		return make([]forge.ThreadRef, len(ts)), make([]error, len(ts)), f.postErr
	}
	refs := make([]forge.ThreadRef, len(ts))
	errs := make([]error, len(ts))
	for i := range ts {
		if i < len(f.postErrs) && f.postErrs[i] != nil {
			errs[i] = f.postErrs[i]
			continue
		}
		if i < len(f.postRefs) {
			refs[i] = f.postRefs[i]
			continue
		}
		refs[i] = forge.ThreadRef{ID: fmt.Sprintf("new%d", i+1), CommentID: int64(900 + i + 1)}
	}
	return refs, errs, nil
}

func (f *fakeInline) ReplyThread(_ context.Context, _, _ int64, t forge.ThreadRef, body string) error {
	f.note("reply %s %s", t.ID, body)
	return f.replyErr[t.ID]
}

func (f *fakeInline) ResolveThread(_ context.Context, _, _ int64, t forge.ThreadRef, resolved bool) error {
	f.note("resolve %s %t", t.ID, resolved)
	if resolved {
		return f.resolveOn[t.ID]
	}
	return f.reopenOn[t.ID]
}

func (f *fakeInline) ReactThread(context.Context, int64, int64, int64, forge.Reaction) error {
	return nil
}

func (f *fakeInline) ThreadsBlockMerge(context.Context, int64, string) (bool, error) {
	return f.block, f.blockErr
}

// changeForge is the part of a Forge that the inline sync reads.
type changeForge struct {
	forge.Forge
	change  forge.Change
	err     error
	mention string
}

func (c changeForge) GetChange(context.Context, int64, int64) (forge.Change, error) {
	return c.change, c.err
}
func (c changeForge) Mention() string { return c.mention }

// harness runs the inline sync for merge request 1 at head.
type harness struct {
	t      *testing.T
	head   string
	events []string
	fi     *fakeInline
	st     *memThreads
	cf     changeForge
	github bool
	logs   lockedWriter
	redact func(string) string
	blocks bool
}

func newHarness(t *testing.T, rows ...store.InlineThread) *harness {
	t.Helper()
	h := &harness{t: t, head: sha('a'), redact: redactSecret}
	h.fi = &fakeInline{events: &h.events, diff: []forge.FileDiff{{Path: "db/query.go", Patch: queryPatch}}}
	h.st = newMemThreads(&h.events, rows...)
	h.cf = changeForge{
		change:  forge.Change{Number: 1, State: forge.StateOpen, HeadSHA: h.head, TargetBranch: "main", BaseSHA: sha('b'), StartSHA: sha('5')},
		mention: "pergent-review",
	}
	return h
}

func (h *harness) mr() store.MRKey {
	return store.MRKey{Instance: testInstance, ProjectID: testProjectID, IID: 1}
}

func (h *harness) run() store.Run {
	return store.Run{MR: h.mr(), ProjectPath: testProjectPath, HeadSHA: h.head, TargetBranch: "main"}
}

func (h *harness) sync(o policy.Outcome) inlineView {
	h.t.Helper()
	d := inlineDeps{
		forge: h.cf, inline: h.fi, store: h.st, github: h.github,
		blocks: func(context.Context) bool { return h.blocks },
		redact: h.redact, log: slog.New(slog.NewTextHandler(&h.logs, nil)),
	}
	return runInline(context.Background(), d, h.run(), o)
}

// row is a recorded thread of the harness's merge request.
func (h *harness) row(fp, id string, line int, state string) store.InlineThread {
	return store.InlineThread{
		MR: h.mr(), Fingerprint: fp, Skill: "security", File: "db/query.go", Line: line, EndLine: line,
		Commit: h.head, ThreadID: id, CommentID: 500, State: state,
	}
}

// platform is the thread of a recorded row as the platform shows it.
func (h *harness) platform(row store.InlineThread, resolved bool) forge.Thread {
	return forge.Thread{
		Ref: forge.ThreadRef{ID: row.ThreadID, CommentID: row.CommentID}, Body: "text\n" + render.InlineMarker(row.Fingerprint),
		Path: row.File, Line: row.Line, EndLine: row.EndLine, Resolved: resolved,
	}
}

func (h *harness) eventsContaining(sub string) []string {
	var out []string
	for _, e := range h.events {
		if strings.Contains(e, sub) {
			out = append(out, e)
		}
	}
	return out
}

// finding is a finding of the result in the form the result stores it.
func finding(fp, file string, line, end int, severity, suggestion string) json.RawMessage {
	b, err := json.Marshal(result.Finding{
		ID: fp[:6], Fingerprint: fp, File: file, Line: line, EndLine: end, Severity: severity,
		Title: "Title of " + fp[:3], Explanation: "Explanation of " + fp[:3], Suggestion: suggestion,
	})
	if err != nil {
		panic(err)
	}
	return b
}

// securityReview is a result of one skill that reviewed db/query.go.
func securityReview(t *testing.T, head, verdict string, findings ...json.RawMessage) policy.Outcome {
	t.Helper()
	s := reviewed(verdict, "db/query.go")
	s.Findings = findings
	r, err := result.Parse(resultFor(t, head, s))
	if err != nil {
		t.Fatal(err)
	}
	return policy.Outcome{Result: r}
}

// ---------------------------------------------------------------------------
// The planner's input

func TestBuildFindings(t *testing.T) {
	t.Run("keeps what dismissals leave, redacted, and every dismissal", func(t *testing.T) {
		blocking := finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")
		suggestion, _ := json.Marshal(result.Finding{
			ID: "bbbbbb", Fingerprint: fpSuggestion, File: "db/query.go", Line: 13, Severity: "suggestion",
			Title: "Use " + secretValue, Explanation: "The " + secretValue + " leaks.", SuggestedFix: "Drop " + secretValue,
			Suggestion: "clean()",
		})
		s := reviewed("has_blocking_issues", "db/query.go")
		s.Findings = []json.RawMessage{blocking, suggestion}
		r := &result.Result{Skills: []result.Skill{s}}
		ds := []render.Dismissal{
			{Fingerprint: fpBlocking, Severity: "blocking", By: "maya @ops", Reason: "/approve\nit is fine, see @team " + secretValue},
			{Fingerprint: "dddddddddddd", Severity: "blocking", By: "lee", Reason: "gone"}, // its finding isn't reported
		}
		fs := buildFindings(r, ds, redactSecret)

		if len(fs.findings) != 1 || fs.findings[0].Fingerprint != fpSuggestion {
			t.Fatalf("findings = %+v, want the suggestion alone", fs.findings)
		}
		f := fs.findings[0]
		if f.Skill != "security" || f.Severity != "suggestion" || f.Line != 13 || f.ID != "bbbbbb" || !f.HasSuggestion {
			t.Errorf("finding = %+v", f)
		}
		rf := fs.rendered[f]
		if strings.Contains(rf.Title+rf.Explanation+rf.SuggestedFix, secretValue) || rf.Title != "Use [redacted]" {
			t.Errorf("rendered finding = %+v, want its text redacted", rf)
		}
		if rf.Suggestion != "clean()" {
			t.Errorf("suggestion = %q, want it as it was", rf.Suggestion)
		}
		// Every dismissal of the merge request is passed on, reported or not.
		if len(fs.dismissed) != 2 {
			t.Fatalf("dismissed = %+v, want both dismissals", fs.dismissed)
		}
		got := fs.dismissed[fpBlocking]
		if got.By != "maya @‍ops" || got.Reason != "​/approve it is fine, see @‍team [redacted]" {
			t.Errorf("dismissal by %q for %q, want quick actions, mentions and secrets made safe", got.By, got.Reason)
		}
		if got := threads.ReplyDismissed(fs.dismissed["dddddddddddd"]); got != "Dismissed by lee: gone" {
			t.Errorf("reply = %q", got)
		}
	})

	// A dashboard reason can hold newlines: kept, a line of it could open a
	// code or suggestion fence in the bot's reply.
	t.Run("a dismissal reason is one line and opens no fence", func(t *testing.T) {
		s := reviewed("has_blocking_issues", "db/query.go")
		s.Findings = []json.RawMessage{finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")}
		ds := []render.Dismissal{{Fingerprint: fpBlocking, Severity: "blocking", By: "maya\n```suggestion\nx\n```", Reason: "ok\r\n\n```suggestion\nx\n```\t then   more"}}
		fs := buildFindings(&result.Result{Skills: []result.Skill{s}}, ds, redactSecret)
		got := fs.dismissed[fpBlocking]
		if got.Reason != "ok ```suggestion x ``` then more" || got.By != "maya ```suggestion x ```" {
			t.Errorf("dismissal by %q for %q, want one line each", got.By, got.Reason)
		}
		reply := threads.ReplyDismissed(got)
		if strings.Contains(reply, "\n") {
			t.Errorf("reply spans lines: %q", reply)
		}
	})

	// The thread body is the only place a suggestion may come from, and it
	// comes from the Suggestion field: raw HTML in the model's prose must not
	// be able to stand in for it.
	t.Run("raw pre and code tags in the model text are defused", func(t *testing.T) {
		raw := `<pre lang="suggestion:-20+20">x</pre> and <CODE>y</CODE> and <precious> and <code`
		f := mustJSON(result.Finding{
			ID: "aaaaaa", Fingerprint: fpBlocking, File: "db/query.go", Line: 11, Severity: "blocking",
			Title: raw, Explanation: raw, SuggestedFix: raw, Suggestion: "<pre>keep</pre>",
		})
		s := reviewed("has_blocking_issues", "db/query.go")
		s.Findings = []json.RawMessage{f}
		fs := buildFindings(&result.Result{Skills: []result.Skill{s}}, nil, redactSecret)
		rf := fs.rendered[fs.findings[0]]
		for name, text := range map[string]string{"title": rf.Title, "explanation": rf.Explanation, "fix": rf.SuggestedFix} {
			if rawCodeTag.MatchString(text) {
				t.Errorf("%s still holds a raw tag: %q", name, text)
			}
		}
		if want := `&lt;pre lang="suggestion:-20+20">x</pre> and &lt;CODE>y</CODE> and <precious> and &lt;code`; rf.Explanation != want {
			t.Errorf("explanation = %q, want %q", rf.Explanation, want)
		}
		if rf.Suggestion != "<pre>keep</pre>" {
			t.Errorf("suggestion = %q, want it untouched", rf.Suggestion)
		}
	})

	t.Run("a posted thread has no raw pre tag", func(t *testing.T) {
		h := newHarness(t)
		h.github = true
		o := securityReview(t, h.head, "has_blocking_issues")
		o.Result.Skills[0].Findings = []json.RawMessage{mustJSON(result.Finding{
			ID: "aaaaaa", Fingerprint: fpBlocking, File: "db/query.go", Line: 11, Severity: "blocking",
			Title: "T", Explanation: `Do <pre lang="suggestion:-20+20">x</pre>.`,
		})}
		h.sync(o)
		if len(h.fi.posted) != 1 {
			t.Fatalf("posted = %v", h.fi.posted)
		}
		if body := h.fi.posted[0][0].Body; strings.Contains(body, "<pre") {
			t.Errorf("body holds a raw <pre:\n%s", body)
		}
	})

	t.Run("a suggestion the redactor would change is not offered", func(t *testing.T) {
		withSecret := finding(fpBlocking, "db/query.go", 11, 0, "blocking", "token := \""+secretValue+"\"")
		plain := finding(fpSuggestion, "db/query.go", 13, 0, "suggestion", "x := 1")
		none := finding(fpThird, "db/query.go", 14, 0, "suggestion", "")
		s := reviewed("has_blocking_issues", "db/query.go")
		s.Findings = []json.RawMessage{withSecret, plain, none}
		fs := buildFindings(&result.Result{Skills: []result.Skill{s}}, nil, redactSecret)
		has := map[string]bool{}
		for _, f := range fs.findings {
			has[f.Fingerprint] = f.HasSuggestion
			if rf := fs.rendered[f]; rf.Suggestion != "" && !f.HasSuggestion {
				t.Errorf("%s keeps a suggestion it must not offer: %q", f.Fingerprint, rf.Suggestion)
			}
		}
		want := map[string]bool{fpBlocking: false, fpSuggestion: true, fpThird: false}
		if !reflect.DeepEqual(has, want) {
			t.Errorf("HasSuggestion = %v, want %v", has, want)
		}
	})

	t.Run("coverage", func(t *testing.T) {
		notReviewed := reviewed("no_blocking_issues", "a.go", "b.go")
		notReviewed.Name = "not-reviewed"
		notReviewed.Files.NotReviewed = []json.RawMessage{json.RawMessage(`{"path":"c.go"}`)}
		failed := reviewed("failed", "a.go")
		failed.Name = "failed"
		unknown := reviewed("something-new", "a.go")
		unknown.Name = "unknown"
		ok := reviewed("no_blocking_issues", "a.go")
		ok.Name = "ok"
		fs := buildFindings(&result.Result{Skills: []result.Skill{notReviewed, failed, unknown, ok}}, nil, redactSecret)
		for name, wantFailed := range map[string]bool{"not-reviewed": true, "failed": true, "unknown": true, "ok": false} {
			if got := fs.skills[name].Failed; got != wantFailed {
				t.Errorf("skill %s Failed = %t, want %t", name, got, wantFailed)
			}
		}
		if !fs.skills["not-reviewed"].Reviewed["b.go"] || fs.skills["not-reviewed"].Reviewed["c.go"] {
			t.Errorf("reviewed = %v, want the reviewed files only", fs.skills["not-reviewed"].Reviewed)
		}
		if !fs.multi {
			t.Errorf("multi = false for four skills")
		}
	})

	t.Run("a verdict recomputed after dismissals is the one read", func(t *testing.T) {
		s := reviewed("has_blocking_issues", "db/query.go")
		s.Findings = []json.RawMessage{finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")}
		fs := buildFindings(&result.Result{Skills: []result.Skill{s}}, []render.Dismissal{{Fingerprint: fpBlocking, Severity: "blocking", By: "m"}}, redactSecret)
		if fs.skills["security"].Failed || len(fs.findings) != 0 {
			t.Errorf("skills = %+v, findings = %+v, want a clean skill", fs.skills, fs.findings)
		}
	})

	t.Run("fingerprints a thread can't carry stay in the comment", func(t *testing.T) {
		s := reviewed("has_blocking_issues", "db/query.go")
		s.Findings = []json.RawMessage{
			finding("has space abcd", "db/query.go", 11, 0, "blocking", ""),
			finding(strings.Repeat("a", 129), "db/query.go", 11, 0, "blocking", ""),
			finding(fpBlocking, "db/query.go", 11, 0, "other", ""),
		}
		s.Findings = append(s.Findings, json.RawMessage(`{"file":"x.go","severity":"blocking"}`))
		fs := buildFindings(&result.Result{Skills: []result.Skill{s}}, nil, redactSecret)
		if len(fs.findings) != 1 || fs.findings[0].Fingerprint != fpBlocking || fs.findings[0].Severity != "blocking" {
			t.Errorf("findings = %+v, want the one valid fingerprint, an unknown severity counting as blocking", fs.findings)
		}
	})
}

func TestBuildDiff(t *testing.T) {
	files := []forge.FileDiff{
		{Path: "a.go", Patch: "@@ -1 +1,2 @@\n x\n+y"},
		{Path: "new.go", OldPath: "old.go", Patch: queryPatch},
		{Path: "gone.go", Deleted: true, Patch: "@@ -1 +0,0 @@\n-x"},
	}
	// The review saw b.go and c.go, which the platform's diff lacks.
	d := buildDiff(files, []string{"a.go", "b.go", "c.go"})
	if len(d) != 5 {
		t.Fatalf("diff has %d files, want 5: %v", len(d), d)
	}
	if h, ok := d["a.go"].HunkOf(2); !ok || h != 0 {
		t.Errorf("a.go not parsed: %+v", d["a.go"])
	}
	if d["new.go"].OldPath != "old.go" || !d["gone.go"].Deleted {
		t.Errorf("new.go = %+v, gone.go = %+v", d["new.go"], d["gone.go"])
	}
	for _, p := range []string{"b.go", "c.go"} {
		if f, ok := d[p]; !ok || len(f.Hunks) != 0 || f.Deleted || f.Path != p {
			t.Errorf("%s = %+v, %v; want a file with no hunks", p, f, ok)
		}
	}
}

func TestJoinThreads(t *testing.T) {
	mr := store.MRKey{Instance: testInstance, ProjectID: testProjectID, IID: 1}
	head := sha('a')
	marker := func(fp string) string { return "body\n" + render.InlineMarker(fp) }
	row := func(fp, id string, state string) store.InlineThread {
		return store.InlineThread{MR: mr, Fingerprint: fp, Skill: "security", File: "f.go", Line: 5, EndLine: 6, Commit: sha('1'), ThreadID: id, CommentID: 7, State: state}
	}
	found := map[string]threads.Finding{
		"cccccccccccc": {Fingerprint: "cccccccccccc", Skill: "style", File: "f.go", Line: 20, EndLine: 22},
	}

	t.Run("resolved comes from the platform, a deleted thread is stale", func(t *testing.T) {
		rows := []store.InlineThread{row("aaaaaaaaaaaa", "t1", "open"), row("bbbbbbbbbbbb", "t2", "fixed"), row("dddddddddddd", "t3", "open")}
		plat := []forge.Thread{
			{Ref: forge.ThreadRef{ID: "t1"}, Body: marker("aaaaaaaaaaaa"), Resolved: true},
			{Ref: forge.ThreadRef{ID: "t2"}, Body: marker("bbbbbbbbbbbb"), Resolved: false},
		}
		set := joinThreads(mr, head, rows, plat, found)
		if !slices.Equal(set.stale, []string{"dddddddddddd"}) {
			t.Errorf("stale = %v, want the deleted thread's", set.stale)
		}
		if len(set.adopted) != 0 || len(set.list) != 2 {
			t.Fatalf("list = %+v, adopted = %+v", set.list, set.adopted)
		}
		byFP := map[string]threads.Thread{}
		for _, th := range set.list {
			byFP[th.Fingerprint] = th
		}
		a, b := byFP["aaaaaaaaaaaa"], byFP["bbbbbbbbbbbb"]
		if !a.Resolved || a.State != threads.StateOpen || a.Skill != "security" || a.File != "f.go" || a.Line != 5 || a.EndLine != 6 || a.Commit != sha('1') {
			t.Errorf("thread a = %+v: a person-resolved thread stays open in pergent's record", a)
		}
		if b.Resolved || b.State != threads.StateFixed {
			t.Errorf("thread b = %+v", b)
		}
		if set.refs["aaaaaaaaaaaa"] != (forge.ThreadRef{ID: "t1", CommentID: 7}) {
			t.Errorf("ref = %+v", set.refs["aaaaaaaaaaaa"])
		}
	})

	t.Run("a thread with a marker and no record is adopted", func(t *testing.T) {
		plat := []forge.Thread{
			{Ref: forge.ThreadRef{ID: "x1", CommentID: 11}, Body: marker("cccccccccccc"), Path: "f.go", Line: 0, EndLine: 0},
			{Ref: forge.ThreadRef{ID: "x2", CommentID: 12}, Body: marker("eeeeeeeeeeee"), Path: "g.go", Line: 30, EndLine: 31, Resolved: true},
			{Ref: forge.ThreadRef{ID: "x3", CommentID: 13}, Body: "no marker here", Path: "g.go", Line: 3},
			{Ref: forge.ThreadRef{ID: "x4", CommentID: 14}, Body: marker("cccccccccccc"), Path: "f.go", Line: 40}, // a second thread for the same finding
		}
		set := joinThreads(mr, head, nil, plat, found)
		if len(set.adopted) != 2 {
			t.Fatalf("adopted = %+v, want 2", set.adopted)
		}
		want := []store.InlineThread{
			// The platform says no line: the finding's lines.
			{MR: mr, Fingerprint: "cccccccccccc", Skill: "style", File: "f.go", Line: 20, EndLine: 22, Commit: head, ThreadID: "x1", CommentID: 11, State: "open"},
			// No finding has this fingerprint: no skill; the platform's lines.
			{MR: mr, Fingerprint: "eeeeeeeeeeee", Skill: "", File: "g.go", Line: 30, EndLine: 31, Commit: head, ThreadID: "x2", CommentID: 12, State: "open"},
		}
		if !reflect.DeepEqual(set.adopted, want) {
			t.Errorf("adopted =\n%+v\nwant\n%+v", set.adopted, want)
		}
		for _, th := range set.list {
			if th.Fingerprint == "eeeeeeeeeeee" && !th.Resolved {
				t.Errorf("the platform's Resolved is lost on an adopted thread")
			}
		}
	})

	t.Run("a marker for a fingerprint that has a record is not adopted", func(t *testing.T) {
		rows := []store.InlineThread{row("aaaaaaaaaaaa", "t1", "open")}
		plat := []forge.Thread{
			{Ref: forge.ThreadRef{ID: "t1"}, Body: marker("aaaaaaaaaaaa")},
			{Ref: forge.ThreadRef{ID: "dup"}, Body: marker("aaaaaaaaaaaa")},
		}
		set := joinThreads(mr, head, rows, plat, found)
		if len(set.adopted) != 0 || len(set.list) != 1 || set.refs["aaaaaaaaaaaa"].ID != "t1" {
			t.Errorf("list = %+v, adopted = %+v, refs = %+v", set.list, set.adopted, set.refs)
		}
	})

	t.Run("the finding of a deleted thread is a candidate again, its marker alone is adopted", func(t *testing.T) {
		// The record's thread is gone, and a thread with the same marker is on
		// the platform under another ID: the record is stale, the thread is adopted.
		rows := []store.InlineThread{row("aaaaaaaaaaaa", "old", "open")}
		plat := []forge.Thread{{Ref: forge.ThreadRef{ID: "fresh", CommentID: 3}, Body: marker("aaaaaaaaaaaa"), Path: "f.go", Line: 9}}
		set := joinThreads(mr, head, rows, plat, found)
		if !slices.Equal(set.stale, []string{"aaaaaaaaaaaa"}) || len(set.adopted) != 1 || set.adopted[0].ThreadID != "fresh" {
			t.Errorf("stale = %v, adopted = %+v", set.stale, set.adopted)
		}
	})
}

func TestThreadMapper(t *testing.T) {
	head := sha('a')
	old := sha('1')
	thread := func(file string, line, end int, commit string) threads.Thread {
		return threads.Thread{Fingerprint: "f", File: file, Line: line, EndLine: end, Commit: commit}
	}

	t.Run("a thread of the head keeps its lines and asks nothing", func(t *testing.T) {
		fi := &fakeInline{}
		m := newThreadMapper(context.Background(), fi, testProjectID, head)
		if l, e, ok := m.Map(thread("a.go", 7, 9, head)); !ok || l != 7 || e != 9 {
			t.Errorf("Map = %d, %d, %t; want 7, 9, true", l, e, ok)
		}
		if len(fi.compares) != 0 {
			t.Errorf("compared %v", fi.compares)
		}
	})

	t.Run("lines are followed through the comparison, which is fetched once per commit", func(t *testing.T) {
		fi := &fakeInline{compare: func(from, to string) ([]forge.FileDiff, error) {
			if from != old || to != head {
				t.Errorf("compared %s..%s", from, to)
			}
			return []forge.FileDiff{
				// Two lines inserted after old line 10.
				{Path: "a.go", Patch: "@@ -10,3 +10,5 @@\n a\n+b\n+c\n d\n e"},
				// Renamed: the thread's file is the old name.
				{Path: "moved.go", OldPath: "was.go", Patch: "@@ -1,2 +1,3 @@\n x\n+y\n z"},
				{Path: "bin.dat"}, // no patch
			}, nil
		}}
		m := newThreadMapper(context.Background(), fi, testProjectID, head)
		cases := []struct {
			name      string
			t         threads.Thread
			line, end int
			wantOK    bool
		}{
			{"above the change", thread("a.go", 3, 4, old), 3, 4, true},
			{"below the change", thread("a.go", 40, 41, old), 42, 43, true},
			{"an unchanged line inside a hunk", thread("a.go", 11, 12, old), 13, 14, true},
			{"a single line", thread("a.go", 40, 0, old), 42, 42, true},
			{"a file the comparison doesn't list", thread("other.go", 5, 6, old), 5, 6, true},
			{"through a rename", thread("was.go", 2, 0, old), 3, 3, true},
			{"a file with no patch", thread("bin.dat", 2, 3, old), 0, 0, false},
			{"no line recorded", thread("a.go", 0, 0, old), 0, 0, false},
		}
		for _, tc := range cases {
			l, e, ok := m.Map(tc.t)
			if ok != tc.wantOK || (ok && (l != tc.line || e != tc.end)) {
				t.Errorf("%s: Map = %d, %d, %t; want %d, %d, %t", tc.name, l, e, ok, tc.line, tc.end, tc.wantOK)
			}
		}
		if len(fi.compares) != 1 {
			t.Errorf("compared %v, want the commit once", fi.compares)
		}
	})

	t.Run("a removed line is not mapped", func(t *testing.T) {
		fi := &fakeInline{compare: func(string, string) ([]forge.FileDiff, error) {
			return []forge.FileDiff{{Path: "a.go", Patch: "@@ -5,3 +5,2 @@\n a\n-b\n c"}}, nil
		}}
		m := newThreadMapper(context.Background(), fi, testProjectID, head)
		if _, _, ok := m.Map(thread("a.go", 6, 6, old)); ok {
			t.Errorf("the removed line was mapped")
		}
		if _, _, ok := m.Map(thread("a.go", 5, 6, old)); ok {
			t.Errorf("a range that ends on a removed line was mapped")
		}
		if l, _, ok := m.Map(thread("a.go", 7, 7, old)); !ok || l != 6 {
			t.Errorf("the line below = %d, %t, want 6", l, ok)
		}
	})

	t.Run("a failed comparison maps nothing, and is asked for once", func(t *testing.T) {
		fi := &fakeInline{compare: func(string, string) ([]forge.FileDiff, error) { return nil, errors.New("timed out") }}
		m := newThreadMapper(context.Background(), fi, testProjectID, head)
		for i := 0; i < 3; i++ {
			if _, _, ok := m.Map(thread("a.go", 5, 5, old)); ok {
				t.Fatalf("mapped through a failed comparison")
			}
		}
		if len(fi.compares) != 1 {
			t.Errorf("asked %d times, want 1", len(fi.compares))
		}
	})

	t.Run("each commit is compared on its own", func(t *testing.T) {
		fi := &fakeInline{}
		m := newThreadMapper(context.Background(), fi, testProjectID, head)
		m.Map(thread("a.go", 5, 5, sha('1')))
		m.Map(thread("a.go", 5, 5, sha('2')))
		m.Map(thread("b.go", 5, 5, sha('1')))
		if !slices.Equal(fi.compares, []string{sha('1') + ".." + head, sha('2') + ".." + head}) {
			t.Errorf("compares = %v", fi.compares)
		}
	})
}

// ---------------------------------------------------------------------------
// Whether open threads block a merge

func TestThreadsBlockMergeCachesAndLogsOnce(t *testing.T) {
	var logs lockedWriter
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e := &Engine{Config: &config.Config{}, Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	run := store.Run{MR: store.MRKey{Instance: "main", ProjectID: 7}, ProjectPath: "acme/widgets", TargetBranch: "main"}

	calls := 0
	fi := &countingBlock{block: true, calls: &calls}
	if !e.threadsBlockMerge(context.Background(), fi, run) || calls != 1 {
		t.Fatalf("first read: calls = %d", calls)
	}
	now = now.Add(9 * time.Minute)
	fi.block = false // the rule changed, but the answer is kept
	if !e.threadsBlockMerge(context.Background(), fi, run) || calls != 1 {
		t.Errorf("within 10 minutes: calls = %d, want the kept answer", calls)
	}
	now = now.Add(2 * time.Minute)
	if e.threadsBlockMerge(context.Background(), fi, run) || calls != 2 {
		t.Errorf("after 10 minutes: calls = %d, want a new read", calls)
	}
	// Another branch and another project are other keys.
	other := run
	other.TargetBranch = "release"
	e.threadsBlockMerge(context.Background(), fi, other)
	other = run
	other.MR.ProjectID = 8
	e.threadsBlockMerge(context.Background(), fi, other)
	if calls != 4 {
		t.Errorf("calls = %d, want 4 with a branch and a project of their own", calls)
	}

	// An error counts as false, is not kept, and is logged once per repository.
	failing := &countingBlock{err: errors.New("403 forbidden"), calls: &calls}
	broken := store.Run{MR: store.MRKey{Instance: "main", ProjectID: 9}, ProjectPath: "acme/Gadgets", TargetBranch: "main"}
	before := calls
	for i := 0; i < 3; i++ {
		if e.threadsBlockMerge(context.Background(), failing, broken) {
			t.Fatal("an error counted as a rule")
		}
	}
	if calls-before != 3 {
		t.Errorf("an error was kept: %d reads, want one per call", calls-before)
	}
	if n := strings.Count(logs.buf.String(), "couldn't read whether threads must be resolved"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, logs.buf.String())
	}
	broken.ProjectPath = "acme/gadgets" // paths are case-insensitive
	e.threadsBlockMerge(context.Background(), failing, broken)
	if n := strings.Count(logs.buf.String(), "couldn't read whether threads must be resolved"); n != 1 {
		t.Errorf("logged %d times for the same repository in another case", n)
	}
	if strings.Contains(logs.buf.String(), "level=WARN") {
		t.Errorf("logged above info:\n%s", logs.buf.String())
	}
}

type countingBlock struct {
	fakeInline
	block bool
	err   error
	calls *int
}

func (c *countingBlock) ThreadsBlockMerge(context.Context, int64, string) (bool, error) {
	*c.calls++
	return c.block, c.err
}

// ---------------------------------------------------------------------------
// Whether to post at all

func TestSyncInlineOffWhen(t *testing.T) {
	good := securityReview(t, sha('a'), "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""))
	on := settings.Effective{InlineFindings: true}
	fi := &fakeInline{diff: []forge.FileDiff{{Path: "db/query.go", Patch: queryPatch}}}
	gitlabLike := Platform{Kind: "gitlab", Forge: struct {
		forge.Forge
		forge.Inline
	}{changeForge{change: forge.Change{State: forge.StateOpen, HeadSHA: sha('a')}}, fi}}
	run := store.Run{MR: store.MRKey{Instance: testInstance, ProjectID: testProjectID, IID: 1}, HeadSHA: sha('a'), TargetBranch: "main"}
	e := &Engine{} // no store: the checks that come first need none

	mergeNote := securityReview(t, sha('a'), "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""))
	mergeNote.Result.MergeNote = result.NoteBaseUnreadable
	errored := securityReview(t, sha('a'), "has_blocking_issues")
	errored.Result.Status = "error"

	cases := map[string]struct {
		plat   Platform
		repo   settings.Effective
		repoOK bool
		o      policy.Outcome
	}{
		"the repository is not known":        {gitlabLike, on, false, good},
		"the setting is off":                 {gitlabLike, settings.Effective{}, true, good},
		"the platform has no inline threads": {Platform{Kind: "gitlab", Forge: changeForge{}}, on, true, good},
		"no usable result":                   {gitlabLike, on, true, policy.Outcome{FailureReason: "the job died"}},
		"no result":                          {gitlabLike, on, true, policy.Outcome{}},
		"an error result":                    {gitlabLike, on, true, errored},
		"a re-run kept alone":                {gitlabLike, on, true, mergeNote},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if v := e.syncInline(context.Background(), tc.plat, run, tc.repo, tc.repoOK, tc.o, slog.New(slog.NewTextHandler(io.Discard, nil))); !reflect.DeepEqual(v, inlineView{}) {
				t.Errorf("view = %+v, want inline off", v)
			}
			if len(fi.posted) != 0 {
				t.Errorf("posted threads")
			}
		})
	}
}

func TestInlineSkippedForAChangeThatMoved(t *testing.T) {
	good := func(h *harness) policy.Outcome {
		return securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""))
	}
	t.Run("a newer head", func(t *testing.T) {
		h := newHarness(t)
		h.cf.change.HeadSHA = sha('c')
		if v := h.sync(good(h)); !reflect.DeepEqual(v, inlineView{}) {
			t.Errorf("view = %+v, want inline off", v)
		}
		if len(h.events) != 0 || len(h.fi.posted) != 0 {
			t.Errorf("events = %v", h.events)
		}
	})
	for _, state := range []forge.ChangeState{forge.StateClosed, forge.StateMerged, forge.StateLocked} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t)
			h.cf.change.State = state
			if v := h.sync(good(h)); !reflect.DeepEqual(v, inlineView{}) || len(h.events) != 0 {
				t.Errorf("view = %+v, events = %v", v, h.events)
			}
		})
	}
	t.Run("the change can't be read", func(t *testing.T) {
		h := newHarness(t)
		h.cf.err = forge.ErrNotFound
		if v := h.sync(good(h)); !reflect.DeepEqual(v, inlineView{}) || len(h.events) != 0 {
			t.Errorf("view = %+v, events = %v", v, h.events)
		}
	})
	// GitLab doesn't compute a head's diff right away: its diff_refs are empty.
	for name, set := range map[string]func(*forge.Change){
		"no base sha":  func(c *forge.Change) { c.BaseSHA = "" },
		"no start sha": func(c *forge.Change) { c.StartSHA = "" },
	} {
		t.Run("the diff isn't ready: "+name, func(t *testing.T) {
			h := newHarness(t)
			set(&h.cf.change)
			if v := h.sync(good(h)); !reflect.DeepEqual(v, inlineView{}) {
				t.Errorf("view = %+v, want inline off", v)
			}
			if len(h.events) != 0 || len(h.fi.posted) != 0 {
				t.Errorf("events = %v, want the diff not read and nothing posted", h.events)
			}
		})
	}
	t.Run("the diff can't be read", func(t *testing.T) {
		h := newHarness(t)
		h.fi.diffErr = gitlab.ErrPartialDiff
		if v := h.sync(good(h)); !reflect.DeepEqual(v, inlineView{}) || len(h.events) != 0 {
			t.Errorf("view = %+v, events = %v", v, h.events)
		}
		if !strings.Contains(h.logs.buf.String(), "level=WARN") {
			t.Errorf("not logged at warn:\n%s", h.logs.buf.String())
		}
	})
	t.Run("the threads can't be listed", func(t *testing.T) {
		h := newHarness(t)
		h.fi.threadsErr = errors.New("boom")
		if v := h.sync(good(h)); !reflect.DeepEqual(v, inlineView{}) || len(h.events) != 0 {
			t.Errorf("view = %+v, events = %v", v, h.events)
		}
		if !strings.Contains(h.logs.buf.String(), "level=WARN") {
			t.Errorf("not logged at warn:\n%s", h.logs.buf.String())
		}
	})
}

// ---------------------------------------------------------------------------
// Posting

func TestInlinePostsBlockingAndNotesTheRest(t *testing.T) {
	h := newHarness(t)
	o := securityReview(t, h.head, "has_blocking_issues",
		finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""),
		finding(fpSuggestion, "db/query.go", 99, 0, "suggestion", ""), // not on the diff
		finding(fpThird, "db/query.go", 10, 0, "suggestion", ""),      // an unchanged line
	)
	v := h.sync(o)

	if !v.On || !reflect.DeepEqual(v.Inline, map[string]bool{fpBlocking: true, fpThird: true}) {
		t.Errorf("view = %+v, want the two findings on the diff", v)
	}
	if !reflect.DeepEqual(v.Notes, map[string]string{fpSuggestion: threads.NoteOffDiff}) {
		t.Errorf("notes = %v, want the off-diff note", v.Notes)
	}
	if len(h.fi.posted) != 1 || len(h.fi.posted[0]) != 2 {
		t.Fatalf("posted = %+v, want one call of two threads", h.fi.posted)
	}
	first, second := h.fi.posted[0][0], h.fi.posted[0][1]
	if first.Path != "db/query.go" || first.Line != 11 || first.EndLine != 11 || first.OldLine != 0 {
		t.Errorf("first thread = %+v: blocking first, on the added line", first)
	}
	if second.Line != 10 || second.OldLine != 10 {
		t.Errorf("second thread = %+v, want the unchanged line with its old line", second)
	}
	for _, th := range h.fi.posted[0] {
		fp, ok := render.InlineFingerprint(th.Body)
		if !ok || !strings.HasSuffix(th.Body, render.InlineMarker(fp)) {
			t.Errorf("body doesn't end with its marker:\n%s", th.Body)
		}
	}
	if !strings.Contains(first.Body, "**❌ Blocking: Title of aaa**") || !strings.Contains(first.Body, "`@pergent-review dismiss <reason>`") {
		t.Errorf("body = %s", first.Body)
	}
	if strings.Contains(first.Body, "`security`") {
		t.Errorf("the skill is named although the review ran one skill:\n%s", first.Body)
	}
	if first.Body != "" && !strings.Contains(first.Body, "id `aaaaaa`") {
		t.Errorf("no finding ID:\n%s", first.Body)
	}

	row, ok := h.st.Row(fpBlocking)
	want := store.InlineThread{MR: h.mr(), Fingerprint: fpBlocking, Skill: "security", File: "db/query.go", Line: 11, EndLine: 11,
		Commit: h.head, ThreadID: "new1", CommentID: 901, State: "open"}
	if !ok || row != want {
		t.Errorf("row = %+v, %t; want %+v", row, ok, want)
	}
	if _, ok := h.st.Row(fpSuggestion); ok {
		t.Errorf("a finding that wasn't posted has a row")
	}
}

func TestInlinePostBodies(t *testing.T) {
	multi := func(t *testing.T, h *harness, findings ...json.RawMessage) policy.Outcome {
		s1 := reviewed("has_blocking_issues", "db/query.go")
		s1.Findings = findings
		s2 := reviewed("no_blocking_issues", "db/query.go")
		s2.Name = "style"
		r, err := result.Parse(resultFor(t, h.head, s1, s2))
		if err != nil {
			t.Fatal(err)
		}
		return policy.Outcome{Result: r}
	}

	t.Run("a GitLab suggestion carries its line count", func(t *testing.T) {
		h := newHarness(t)
		v := h.sync(securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 12, "blocking", "b2\nc2")))
		body := h.fi.posted[0][0].Body
		if !strings.Contains(body, "```suggestion:-0+1\nb2\nc2\n```") {
			t.Errorf("body lacks the 2-line suggestion block:\n%s", body)
		}
		if h.fi.posted[0][0].Line != 11 || h.fi.posted[0][0].EndLine != 12 || !v.Inline[fpBlocking] {
			t.Errorf("thread = %+v", h.fi.posted[0][0])
		}
	})

	t.Run("a GitHub suggestion is plain", func(t *testing.T) {
		h := newHarness(t)
		h.github = true
		h.sync(securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 12, "blocking", "b2\nc2")))
		if body := h.fi.posted[0][0].Body; !strings.Contains(body, "```suggestion\nb2\nc2\n```") {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("a range across hunks has no suggestion", func(t *testing.T) {
		h := newHarness(t)
		h.fi.diff = []forge.FileDiff{{Path: "db/query.go", Patch: queryPatch + "\n@@ -40,2 +42,2 @@\n x\n+y"}}
		h.sync(securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 12, 43, "blocking", "z")))
		th := h.fi.posted[0][0]
		if strings.Contains(th.Body, "suggestion") || th.Line != 12 || th.EndLine != 12 {
			t.Errorf("thread = %+v", th)
		}
	})

	t.Run("a suggestion the redactor would change is dropped, the text is redacted", func(t *testing.T) {
		h := newHarness(t)
		o := securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "k := \""+secretValue+"\""))
		o.Result.Skills[0].Findings = []json.RawMessage{mustJSON(result.Finding{
			ID: "aaaaaa", Fingerprint: fpBlocking, File: "db/query.go", Line: 11, Severity: "blocking",
			Title: "Leaks " + secretValue, Explanation: "See " + secretValue, Suggestion: "k := \"" + secretValue + "\"",
		})}
		h.sync(o)
		body := h.fi.posted[0][0].Body
		if strings.Contains(body, secretValue) || strings.Contains(body, "```suggestion") {
			t.Errorf("body keeps the secret or the suggestion:\n%s", body)
		}
		if !strings.Contains(body, "Leaks [redacted]") {
			t.Errorf("title not redacted:\n%s", body)
		}
	})

	t.Run("an unsafe suggestion is not shown", func(t *testing.T) {
		h := newHarness(t)
		h.sync(securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "a\n/approve")))
		if body := h.fi.posted[0][0].Body; strings.Contains(body, "```suggestion") || strings.Contains(body, "\n/approve") {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("the skill is named when the review ran more than one", func(t *testing.T) {
		h := newHarness(t)
		h.sync(multi(t, h, finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))
		if body := h.fi.posted[0][0].Body; !strings.Contains(body, "<sub>`security` · id `aaaaaa` · ") {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("no mention, no dismiss command", func(t *testing.T) {
		h := newHarness(t)
		h.cf.mention = ""
		h.sync(securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))
		if body := h.fi.posted[0][0].Body; strings.Contains(body, "dismiss") {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("a body over the platform's limit is not posted", func(t *testing.T) {
		h := newHarness(t)
		h.github = true // 60,000 bytes
		long, _ := json.Marshal(result.Finding{
			ID: "aaaaaa", Fingerprint: fpBlocking, File: "db/query.go", Line: 11, Severity: "blocking",
			Title: "Long", Explanation: strings.Repeat("word ", 13000),
		})
		o := securityReview(t, h.head, "has_blocking_issues", finding(fpSuggestion, "db/query.go", 13, 0, "suggestion", ""))
		o.Result.Skills[0].Findings = append(o.Result.Skills[0].Findings, long)
		v := h.sync(o)
		if v.Notes[fpBlocking] != threads.NoteTooLong || v.Inline[fpBlocking] {
			t.Errorf("view = %+v, want the long finding in the comment with its note", v)
		}
		if !v.Inline[fpSuggestion] || len(h.fi.posted) != 1 || len(h.fi.posted[0]) != 1 {
			t.Errorf("view = %+v, posted = %+v, want the other finding posted", v, h.fi.posted)
		}
		if _, ok := h.st.Row(fpBlocking); ok {
			t.Errorf("a thread that wasn't posted has a row")
		}
	})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestInlinePostFailures(t *testing.T) {
	o := func(h *harness) policy.Outcome {
		return securityReview(t, h.head, "has_blocking_issues",
			finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""),
			finding(fpSuggestion, "db/query.go", 12, 0, "suggestion", ""))
	}
	t.Run("the whole call failed", func(t *testing.T) {
		h := newHarness(t)
		h.fi.postErr = errors.New("500 " + secretValue)
		v := h.sync(o(h))
		want := map[string]string{fpBlocking: threads.NoteFailed, fpSuggestion: threads.NoteFailed}
		if !v.On || len(v.Inline) != 0 || !reflect.DeepEqual(v.Notes, want) {
			t.Errorf("view = %+v, want both findings in the comment with the failure noted", v)
		}
		if len(h.st.rows) != 0 {
			t.Errorf("rows = %v", h.st.rows)
		}
		if strings.Contains(h.logs.buf.String(), secretValue) {
			t.Errorf("a secret reached the log")
		}
	})
	t.Run("one thread failed", func(t *testing.T) {
		h := newHarness(t)
		h.fi.postErrs = []error{errors.New("line outside the diff"), nil}
		v := h.sync(o(h))
		if v.Inline[fpBlocking] || v.Notes[fpBlocking] != threads.NoteFailed || !v.Inline[fpSuggestion] {
			t.Errorf("view = %+v", v)
		}
		if _, ok := h.st.Row(fpBlocking); ok {
			t.Errorf("the failed thread has a row")
		}
		if r, ok := h.st.Row(fpSuggestion); !ok || r.ThreadID != "new2" {
			t.Errorf("row = %+v, %t, want the thread that was posted", r, ok)
		}
	})
	t.Run("a zero ref is a failure", func(t *testing.T) {
		h := newHarness(t)
		h.fi.postRefs = []forge.ThreadRef{{}, {ID: "ok", CommentID: 1}}
		v := h.sync(o(h))
		if v.Notes[fpBlocking] != threads.NoteFailed || !v.Inline[fpSuggestion] {
			t.Errorf("view = %+v", v)
		}
	})
}

func TestInlineSyncRuleKeepsSuggestions(t *testing.T) {
	h := newHarness(t)
	h.blocks = true
	v := h.sync(securityReview(t, h.head, "has_blocking_issues",
		finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""),
		finding(fpSuggestion, "db/query.go", 12, 0, "suggestion", "x")))
	if !reflect.DeepEqual(v.Inline, map[string]bool{fpBlocking: true}) || v.Notes[fpSuggestion] != threads.NoteKeptRule {
		t.Errorf("view = %+v, want the blocking finding posted and the suggestion kept with its note", v)
	}
	if len(h.fi.posted) != 1 || len(h.fi.posted[0]) != 1 {
		t.Errorf("posted = %+v", h.fi.posted)
	}
}

// ---------------------------------------------------------------------------
// Keeping the threads

func TestInlineSyncAdoptsUnrecordedThreads(t *testing.T) {
	h := newHarness(t)
	h.fi.threads = []forge.Thread{{
		Ref: forge.ThreadRef{ID: "t9", CommentID: 77}, Body: "Blocking\n" + render.InlineMarker(fpBlocking), Path: "db/query.go", Line: 11, EndLine: 11,
	}}
	v := h.sync(securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))

	if len(h.fi.posted) != 0 {
		t.Errorf("posted %+v, want nothing: the thread is there", h.fi.posted)
	}
	if !v.Inline[fpBlocking] {
		t.Errorf("view = %+v, want the finding on the diff", v)
	}
	want := store.InlineThread{MR: h.mr(), Fingerprint: fpBlocking, Skill: "security", File: "db/query.go", Line: 11, EndLine: 11,
		Commit: h.head, ThreadID: "t9", CommentID: 77, State: "open"}
	if r, ok := h.st.Row(fpBlocking); !ok || r != want {
		t.Errorf("row = %+v, %t; want %+v", r, ok, want)
	}
}

func TestInlineStaleRecordIsDroppedAndPostedAgain(t *testing.T) {
	h := newHarness(t)
	h.st = newMemThreads(&h.events, h.row(fpBlocking, "gone", 11, "open"))
	// The platform lists no thread: a person deleted it.
	v := h.sync(securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))
	if len(h.fi.posted) != 1 || !v.Inline[fpBlocking] {
		t.Errorf("posted = %+v, view = %+v, want the finding posted again", h.fi.posted, v)
	}
	if r, ok := h.st.Row(fpBlocking); !ok || r.ThreadID != "new1" {
		t.Errorf("row = %+v, %t, want the new thread's", r, ok)
	}
	if len(h.eventsContaining("store delete "+fpBlocking)) != 1 {
		t.Errorf("events = %v, want the stale record deleted", h.events)
	}
}

func TestInlineResolvesFixedFindings(t *testing.T) {
	row := func(h *harness) store.InlineThread { return h.row(fpBlocking, "t1", 11, "open") }
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		r := row(h)
		h.st = newMemThreads(&h.events, r)
		h.fi.threads = []forge.Thread{h.platform(r, false)}
		return h
	}
	clean := func(h *harness) policy.Outcome { return securityReview(t, h.head, "no_blocking_issues") }

	t.Run("the finding is gone", func(t *testing.T) {
		h := setup(t)
		v := h.sync(clean(h))
		want := []string{"reply t1 Fixed in `aaaaaaa`.", "resolve t1 true", "store state " + fpBlocking + " fixed"}
		if !slices.Equal(h.events, want) {
			t.Errorf("events =\n%q\nwant\n%q", h.events, want)
		}
		if r, _ := h.st.Row(fpBlocking); r.State != "fixed" {
			t.Errorf("state = %q", r.State)
		}
		if !v.On || len(v.Inline) != 0 {
			t.Errorf("view = %+v", v)
		}
	})

	t.Run("a person resolved the thread: nothing is sent", func(t *testing.T) {
		h := setup(t)
		h.fi.threads = []forge.Thread{h.platform(row(h), true)}
		h.sync(clean(h))
		if len(h.events) != 0 {
			t.Errorf("events = %v, want none for a thread the platform shows resolved", h.events)
		}
	})

	t.Run("the skill failed: the thread stays", func(t *testing.T) {
		h := setup(t)
		h.sync(securityReview(t, h.head, "failed"))
		if len(h.events) != 0 {
			t.Errorf("events = %v", h.events)
		}
	})

	t.Run("a reply that fails leaves the thread for the next time", func(t *testing.T) {
		h := setup(t)
		h.fi.replyErr = map[string]error{"t1": errors.New("down")}
		h.sync(clean(h))
		if slices.Contains(h.events, "resolve t1 true") {
			t.Errorf("resolved without the reply: %v", h.events)
		}
		if r, _ := h.st.Row(fpBlocking); r.State != "open" {
			t.Errorf("state = %q, want open", r.State)
		}
	})

	t.Run("a resolve that fails is not recorded", func(t *testing.T) {
		h := setup(t)
		h.fi.resolveOn = map[string]error{"t1": errors.New("down")}
		h.sync(clean(h))
		if r, _ := h.st.Row(fpBlocking); r.State != "open" {
			t.Errorf("state = %q, want open", r.State)
		}
	})

	t.Run("the file left the change", func(t *testing.T) {
		h := setup(t)
		h.fi.diff = nil
		// The review saw no such file either.
		o := securityReview(t, h.head, "no_blocking_issues")
		o.Result.Skills[0].Files.Reviewed = []string{"other.go"}
		h.fi.diff = []forge.FileDiff{{Path: "other.go", Patch: queryPatch}}
		h.sync(o)
		if !slices.Contains(h.events, "reply t1 No longer in this change.") {
			t.Errorf("events = %v", h.events)
		}
		if r, _ := h.st.Row(fpBlocking); r.State != "gone" {
			t.Errorf("state = %q", r.State)
		}
	})
}

func TestInlineDismissAndRestore(t *testing.T) {
	h := newHarness(t)
	r := h.row(fpBlocking, "t1", 11, "open")
	h.st = newMemThreads(&h.events, r)
	h.fi.threads = []forge.Thread{h.platform(r, false)}
	dismissal := render.Dismissal{Fingerprint: fpBlocking, Severity: "blocking", By: "maya (gitlab main)", Reason: "false alarm @team"}
	withFinding := securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""))
	withFinding.Dismissals = []render.Dismissal{dismissal}

	// The finding is dismissed: it is not reported, the thread is resolved.
	v := h.sync(withFinding)
	want := []string{"reply t1 Dismissed by maya (gitlab main): false alarm @‍team", "resolve t1 true", "store state " + fpBlocking + " dismissed"}
	if !slices.Equal(h.events, want) {
		t.Fatalf("events =\n%q\nwant\n%q", h.events, want)
	}
	if len(v.Inline) != 0 {
		t.Errorf("view = %+v", v)
	}

	// The dismissal is withdrawn: the platform shows the thread resolved, the
	// finding is back, the thread opens again.
	h.events = nil
	h.fi.threads = []forge.Thread{h.platform(h.st.rows[fpBlocking], true)}
	back := securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""))
	v = h.sync(back)
	want = []string{"resolve t1 false", "reply t1 " + threads.ReplyRestored, "store put " + fpBlocking + " open"}
	if !slices.Equal(h.events, want) {
		t.Fatalf("events =\n%q\nwant\n%q", h.events, want)
	}
	if !v.Inline[fpBlocking] {
		t.Errorf("view = %+v, want the finding on the diff", v)
	}
	if r, _ := h.st.Row(fpBlocking); r.State != "open" || r.Commit != h.head {
		t.Errorf("row = %+v", r)
	}
}

func TestInlineReopenFailureKeepsTheFindingInTheComment(t *testing.T) {
	h := newHarness(t)
	r := h.row(fpBlocking, "t1", 11, "fixed")
	h.st = newMemThreads(&h.events, r)
	h.fi.threads = []forge.Thread{h.platform(r, true)}
	h.fi.reopenOn = map[string]error{"t1": errors.New("down")}
	v := h.sync(securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))
	if v.Inline[fpBlocking] || v.Notes[fpBlocking] != threads.NoteFailed {
		t.Errorf("view = %+v, want the finding in the comment, noted", v)
	}
	if r, _ := h.st.Row(fpBlocking); r.State != "fixed" {
		t.Errorf("state = %q, want fixed until it is opened", r.State)
	}
}

func TestInlineReopenFollowsTheLinesToTheHead(t *testing.T) {
	h := newHarness(t)
	oldCommit := sha('1')
	r := h.row(fpBlocking, "t1", 20, "fixed")
	r.Commit = oldCommit
	h.st = newMemThreads(&h.events, r)
	h.fi.threads = []forge.Thread{h.platform(r, true)}
	h.fi.diff = []forge.FileDiff{{Path: "db/query.go", Patch: "@@ -20,2 +22,3 @@\n a\n+b\n c"}}
	h.fi.compare = func(from, to string) ([]forge.FileDiff, error) {
		return []forge.FileDiff{{Path: "db/query.go", Patch: "@@ -10,1 +10,3 @@\n x\n+y\n+z"}}, nil // two lines inserted above
	}
	h.sync(securityReview(t, h.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 22, 0, "blocking", "")))
	row, _ := h.st.Row(fpBlocking)
	if row.State != "open" || row.Commit != h.head || row.Line != 22 {
		t.Errorf("row = %+v, want open at the head, line 22", row)
	}
}

func TestInlineCarriesOutRebindsFirst(t *testing.T) {
	h := newHarness(t)
	// A fixed thread whose finding came back reworded (another fingerprint, the
	// same place), an open thread whose finding is gone, a fixed thread whose
	// finding is back as it was, and a new finding.
	const oldFP, newFP = "dddddddddddd", "eeeeeeeeeeee"
	reworded := h.row(oldFP, "t-reworded", 11, "fixed")
	gone := h.row("ffffffffffff", "t-gone", 13, "open")
	back := h.row(fpThird, "t-back", 14, "fixed")
	h.st = newMemThreads(&h.events, reworded, gone, back)
	h.fi.threads = []forge.Thread{h.platform(reworded, true), h.platform(gone, false), h.platform(back, true)}
	o := securityReview(t, h.head, "has_blocking_issues",
		finding(newFP, "db/query.go", 11, 0, "blocking", ""),
		finding(fpThird, "db/query.go", 14, 0, "blocking", ""),
		finding(fpBlocking, "db/query.go", 12, 0, "blocking", ""))
	v := h.sync(o)

	want := []string{
		"store rebind " + oldFP + " " + newFP, // before anything else
		"reply t-gone Fixed in `aaaaaaa`.",
		"resolve t-gone true",
		"store state ffffffffffff fixed",
		"resolve t-reworded false", // found under its new fingerprint
		"reply t-reworded Back in `aaaaaaa`.",
		"store put " + newFP + " open",
		"resolve t-back false",
		"reply t-back Back in `aaaaaaa`.",
		"store put " + fpThird + " open",
		"post 1",
		"store put " + fpBlocking + " open",
	}
	if !slices.Equal(h.events, want) {
		t.Fatalf("events =\n%q\nwant\n%q", h.events, want)
	}
	if _, ok := h.st.Row(oldFP); ok {
		t.Errorf("the old fingerprint still has a row")
	}
	if r, ok := h.st.Row(newFP); !ok || r.ThreadID != "t-reworded" || r.State != "open" {
		t.Errorf("row = %+v, %t", r, ok)
	}
	if !reflect.DeepEqual(v.Inline, map[string]bool{newFP: true, fpThird: true, fpBlocking: true}) {
		t.Errorf("view = %+v", v)
	}
}

// ---------------------------------------------------------------------------
// The real GitLab adapter, on the fake GitLab

type gitlabHarness struct {
	*harness
	gl   *fakeGitLab
	disc *fakeDiscussions
}

// newGitLabHarness runs the inline sync of merge request 1 through the GitLab
// adapter against the fake GitLab, which serves the diff of db/query.go.
func newGitLabHarness(t *testing.T) *gitlabHarness {
	t.Helper()
	gl := newFakeGitLab(t)
	h := newHarness(t)
	f := gitlabforge.NewForge(gl.Client(testReviewToken), gitlab.User{ID: testBotID, Username: "pergent-review"})
	h.cf = changeForge{Forge: f, mention: "pergent-review"}
	gh := &gitlabHarness{harness: h, gl: gl, disc: gl.ServeDiscussions(t, 1)}
	gl.ServeDiff(1, h.head, fileDiff{OldPath: "db/query.go", NewPath: "db/query.go", Diff: queryPatch})
	gl.ServeCompare()
	gh.useAdapter(f)
	return gh
}

// useAdapter makes the harness talk to the fake GitLab through f.
func (g *gitlabHarness) useAdapter(f *gitlabforge.Forge) {
	g.cf = changeForge{Forge: f, mention: "pergent-review"}
	g.st = newMemThreads(&g.events)
}

func (g *gitlabHarness) syncReal(o policy.Outcome) inlineView {
	g.t.Helper()
	f := g.cf.Forge.(*gitlabforge.Forge)
	d := inlineDeps{
		forge: f, inline: f, store: g.st, blocks: func(context.Context) bool { return false },
		redact: g.redact, log: slog.New(slog.NewTextHandler(&g.logs, nil)),
	}
	return runInline(context.Background(), d, g.run(), o)
}

func TestInlineGitLabWire(t *testing.T) {
	g := newGitLabHarness(t)
	o := securityReview(t, g.head, "has_blocking_issues",
		finding(fpBlocking, "db/query.go", 11, 12, "blocking", "b2\nc2"),
		finding(fpThird, "db/query.go", 10, 0, "suggestion", ""),
		finding(fpSuggestion, "db/query.go", 99, 0, "suggestion", ""))
	v := g.syncReal(o)
	if !reflect.DeepEqual(v.Inline, map[string]bool{fpBlocking: true, fpThird: true}) || v.Notes[fpSuggestion] != threads.NoteOffDiff {
		t.Fatalf("view = %+v", v)
	}
	reqs := g.disc.Posted()
	if len(reqs) != 2 {
		t.Fatalf("%d discussions started, want 2", len(reqs))
	}
	for _, r := range reqs {
		if r.Token != testReviewToken {
			t.Errorf("started with token %q, want the review bot's", r.Token)
		}
	}
	first := reqs[0].JSON(t)
	pos := first["position"].(map[string]any)
	if pos["new_path"] != "db/query.go" || pos["new_line"] != float64(11) || pos["position_type"] != "text" ||
		pos["base_sha"] != sha('b') || pos["head_sha"] != g.head || pos["start_sha"] != sha('5') {
		t.Errorf("position = %v", pos)
	}
	if _, has := pos["old_line"]; has {
		t.Errorf("an added line has an old line: %v", pos)
	}
	body := first["body"].(string)
	if !strings.HasSuffix(body, render.InlineMarker(fpBlocking)) || !strings.Contains(body, "```suggestion:-0+1\nb2\nc2\n```") {
		t.Errorf("body = %s", body)
	}
	if second := reqs[1].JSON(t)["position"].(map[string]any); second["new_line"] != float64(10) || second["old_line"] != float64(10) {
		t.Errorf("an unchanged line's position = %v, want its old line too", second)
	}
	if row, ok := g.st.Row(fpBlocking); !ok || row.ThreadID != "disc1" || row.CommentID != 1001 || row.State != "open" {
		t.Errorf("row = %+v, %t", row, ok)
	}

	// The next review has no blocking finding: the thread gets the reply and is resolved.
	g.events = nil
	next := securityReview(t, g.head, "no_blocking_issues", finding(fpThird, "db/query.go", 10, 0, "suggestion", ""))
	v = g.syncReal(next)
	if g.disc.Count() != 2 {
		t.Errorf("%d discussions, want no new one", g.disc.Count())
	}
	if replies := g.disc.Replies(t); len(replies) != 1 || replies[0] != "Fixed in `aaaaaaa`." {
		t.Errorf("replies = %q", replies)
	}
	if !g.disc.Resolved("disc1") || g.disc.Resolved("disc2") {
		t.Errorf("resolved: disc1 = %t, disc2 = %t", g.disc.Resolved("disc1"), g.disc.Resolved("disc2"))
	}
	puts := g.gl.Requests(http.MethodPut, discussionPath(1, "disc1"))
	if len(puts) != 1 || puts[0].Query.Get("resolved") != "true" {
		t.Errorf("resolve requests = %+v", puts)
	}
	if row, _ := g.st.Row(fpBlocking); row.State != "fixed" {
		t.Errorf("state = %q", row.State)
	}
	if !reflect.DeepEqual(v.Inline, map[string]bool{fpThird: true}) {
		t.Errorf("view = %+v", v)
	}
}

func TestInlineGitLabFailure(t *testing.T) {
	g := newGitLabHarness(t)
	g.disc.fail = http.StatusInternalServerError
	v := g.syncReal(securityReview(t, g.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))
	if !v.On || v.Inline[fpBlocking] || v.Notes[fpBlocking] != threads.NoteFailed {
		t.Errorf("view = %+v, want the finding in the comment with the failure noted", v)
	}
	if n := len(g.disc.Posted()); n != 1 {
		t.Errorf("tried %d times, want once: a retry could start a second thread", n)
	}
	if _, ok := g.st.Row(fpBlocking); ok {
		t.Errorf("a thread that wasn't posted has a row")
	}
}

func TestInlineGitLabAdoptsAndKeeps(t *testing.T) {
	g := newGitLabHarness(t)
	id := g.disc.Seed("Earlier\n"+render.InlineMarker(fpBlocking), "db/query.go", 11, false)
	v := g.syncReal(securityReview(t, g.head, "has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))
	if len(g.disc.Posted()) != 0 || !v.Inline[fpBlocking] {
		t.Errorf("posted %d, view = %+v, want the found thread kept", len(g.disc.Posted()), v)
	}
	if row, ok := g.st.Row(fpBlocking); !ok || row.ThreadID != id || row.State != "open" || row.Skill != "security" || row.Commit != g.head {
		t.Errorf("row = %+v, %t", row, ok)
	}
}

// ---------------------------------------------------------------------------
// What the comment says, and what apply never sends

// The view the sync returns, copied into the outcome as publish does, makes
// the comment list the findings that are not on the diff.
func TestInlineViewShapesTheComment(t *testing.T) {
	h := newHarness(t)
	o := securityReview(t, h.head, "has_blocking_issues",
		finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""),
		finding(fpSuggestion, "db/query.go", 99, 0, "suggestion", ""))
	v := h.sync(o)
	o.Mention = "pergent-review"
	o.InlineOn, o.Inline, o.FindingNotes = v.On, v.Inline, v.Notes
	d := policy.Decide(o, config.FailureStatusFailed)
	for _, want := range []string{
		"> 1 finding posted as inline comments on the diff.",
		"#### Not on the diff (1)",
		"_" + threads.NoteOffDiff + "_",
		"or reply `@pergent-review dismiss <reason>` in a finding's thread",
	} {
		if !strings.Contains(d.Comment, want) {
			t.Errorf("comment lacks %q:\n%s", want, d.Comment)
		}
	}
	if strings.Contains(d.Comment, "Title of aaa") {
		t.Errorf("the finding on the diff is listed in the comment too:\n%s", d.Comment)
	}
	if d.Status != policy.StatusFailed || d.Approve {
		t.Errorf("decision = %+v: the blocking finding still blocks", d)
	}
}

// A thread the platform shows resolved is sent nothing, even when a plan
// names it.
func TestInlineApplyNeverTouchesAResolvedThread(t *testing.T) {
	h := newHarness(t)
	r := h.row(fpBlocking, "t1", 11, "open")
	h.st = newMemThreads(&h.events, r)
	th := threads.Thread{Fingerprint: fpBlocking, Skill: "security", File: "db/query.go", Line: 11, EndLine: 11, Commit: h.head, State: threads.StateOpen, Resolved: true}
	x := &inlineRun{
		ctx: context.Background(),
		d:   inlineDeps{forge: h.cf, inline: h.fi, store: h.st, redact: redactSecret, log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		run: h.run(), change: h.cf.change,
		set: threadSet{refs: map[string]forge.ThreadRef{fpBlocking: {ID: "t1"}}, rows: map[string]store.InlineThread{fpBlocking: r}, plat: map[string]forge.Thread{}},
	}
	x.apply(threads.Plan{
		Resolve: []threads.Resolve{{Thread: th, State: threads.StateFixed, Reply: "Fixed."}},
		Inline:  map[string]bool{}, Notes: map[string]string{},
	})
	if len(h.events) != 0 {
		t.Errorf("events = %v, want none", h.events)
	}
}

// ---------------------------------------------------------------------------
// Through a publish (these need the test database)

// queryFile is the diff of db/query.go the publish tests serve for every head.
var queryFile = fileDiff{OldPath: "db/query.go", NewPath: "db/query.go", Diff: queryPatch}

// inlineEnv is a test environment whose fake GitLab serves the diff, the
// comparison, the discussions and the project of merge request 1.
type inlineEnv struct {
	env  *testEnv
	disc *fakeDiscussions
}

func newInlineEnv(t *testing.T) *inlineEnv {
	t.Helper()
	env := newTestEnv(t)
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, map[string]any{"id": createdNote})
	env.GitLab.Reply(http.MethodGet, projectPath(), http.StatusOK, map[string]any{"id": testProjectID})
	env.GitLab.ServeCompare()
	return &inlineEnv{env: env, disc: env.GitLab.ServeDiscussions(t, 1)}
}

// with is a skill that reviewed db/query.go with verdict and findings.
func with(verdict string, findings ...json.RawMessage) result.Skill {
	s := reviewed(verdict, "db/query.go")
	s.Findings = findings
	return s
}

// review publishes a review of head, which the change is at, made of skills.
func (e *inlineEnv) review(t *testing.T, head string, skills ...result.Skill) {
	t.Helper()
	servePublish(e.env, 1, head)
	e.env.GitLab.ServeDiff(1, head, queryFile)
	e.env.Report(1, head, resultFor(t, head, skills...))
	publish(t, e.env)
}

// comment is the review comment as last written.
func (e *inlineEnv) comment(t *testing.T) string {
	t.Helper()
	var last string
	for _, r := range e.env.GitLab.Requests("", "") {
		if (r.Method == http.MethodPost && r.Path == notesPath(1)) || (r.Method == http.MethodPut && r.Path == notePath(1, createdNote)) {
			if body, _ := r.JSON(t)["body"].(string); strings.Contains(body, policy.Marker) {
				last = body
			}
		}
	}
	if last == "" {
		t.Fatal("no review comment was posted")
	}
	return last
}

// rows are the recorded threads of merge request 1.
func (e *inlineEnv) rows(t *testing.T) map[string]store.InlineThread {
	t.Helper()
	rows, err := e.env.Store.InlineThreads(e.env.ctx, e.env.MRKey(1))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.InlineThread{}
	for _, r := range rows {
		out[r.Fingerprint] = r
	}
	return out
}

func TestInlinePostsFindingsOnDiff(t *testing.T) {
	e := newInlineEnv(t)
	head := sha('a')
	e.review(t, head, with("has_blocking_issues",
		finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""),
		finding(fpSuggestion, "db/query.go", 99, 0, "suggestion", ""))) // not on the diff

	posts := e.disc.Posted()
	if len(posts) != 1 {
		t.Fatalf("%d discussions started, want 1", len(posts))
	}
	if posts[0].Token != testReviewToken {
		t.Errorf("started with token %q, want the review bot's", posts[0].Token)
	}
	req := posts[0].JSON(t)
	pos, _ := req["position"].(map[string]any)
	if pos["new_path"] != "db/query.go" || pos["new_line"] != float64(11) || pos["head_sha"] != head || pos["base_sha"] != sha('b') {
		t.Errorf("position = %v", pos)
	}
	if body, _ := req["body"].(string); !strings.HasSuffix(body, render.InlineMarker(fpBlocking)) || !strings.Contains(body, "Title of aaa") {
		t.Errorf("body = %q, want the finding and, last, its marker", body)
	}
	rows := e.rows(t)
	if len(rows) != 1 || rows[fpBlocking].State != "open" || rows[fpBlocking].ThreadID != "disc1" || rows[fpBlocking].Commit != head ||
		rows[fpBlocking].Skill != "security" || rows[fpBlocking].Line != 11 {
		t.Errorf("rows = %+v, want the one thread posted", rows)
	}

	body := createdComment(t, e.env, 1)
	for _, want := range []string{
		"> 1 finding posted as inline comments on the diff.",
		"#### Not on the diff (1)",
		threads.NoteOffDiff,
		"Title of bbb",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Title of aaa") {
		t.Errorf("the finding on the diff is in the comment too:\n%s", body)
	}
	wantStatus(t, e.env, head, gitlab.StatusFailed, "Blocking issues found")
	wantNoApproval(t, e.env, 1)
}

func TestInlineSuggestionBlockGitLab(t *testing.T) {
	e := newInlineEnv(t)
	e.review(t, sha('a'), with("has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 12, "blocking", "b2\nc2")))
	posts := e.disc.Posted()
	if len(posts) != 1 {
		t.Fatalf("%d discussions started, want 1", len(posts))
	}
	if body, _ := posts[0].JSON(t)["body"].(string); !strings.Contains(body, "```suggestion:-0+1\nb2\nc2\n```") {
		t.Errorf("body lacks the suggestion block for 2 lines:\n%s", body)
	}
}

func TestInlineResolvesFixedOnNextReview(t *testing.T) {
	e := newInlineEnv(t)
	e.review(t, sha('a'), with("has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))
	next := sha('c')
	e.review(t, next, with("no_blocking_issues"))

	if replies := e.disc.Replies(t); len(replies) != 1 || replies[0] != "Fixed in `ccccccc`." {
		t.Errorf("replies = %q", replies)
	}
	puts := e.env.GitLab.Requests(http.MethodPut, discussionPath(1, "disc1"))
	if len(puts) != 1 || puts[0].Query.Get("resolved") != "true" {
		t.Errorf("resolve requests = %+v", puts)
	}
	if !e.disc.Resolved("disc1") {
		t.Errorf("the thread is not resolved")
	}
	if r := e.rows(t)[fpBlocking]; r.State != "fixed" {
		t.Errorf("state = %q, want fixed", r.State)
	}
	if body := e.comment(t); strings.Contains(body, "posted as inline") || strings.Contains(body, "Title of aaa") {
		t.Errorf("the second comment still lists the finding:\n%s", body)
	}
	wantStatus(t, e.env, next, gitlab.StatusSuccess, "No blocking issues")
}

func TestInlineSkippedForOldHead(t *testing.T) {
	e := newInlineEnv(t)
	head := sha('a')
	servePublish(e.env, 1, head)
	e.env.GitLab.SetMR(openMR(1, sha('c'))) // the change moved on
	e.env.Report(1, head, resultFor(t, head, with("has_blocking_issues",
		finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""),
		finding(fpSuggestion, "db/query.go", 99, 0, "suggestion", ""))))
	publish(t, e.env)

	if n := len(e.env.GitLab.Requests("", discussionsPath(1))); n != 0 {
		t.Errorf("%d discussion requests, want none for an old head", n)
	}
	if n := len(e.env.GitLab.Requests("", diffsPath(1))); n != 0 {
		t.Errorf("%d diff requests, want none for an old head", n)
	}
	body := createdComment(t, e.env, 1)
	if strings.Contains(body, "posted as inline") || !strings.Contains(body, "Title of aaa") || !strings.Contains(body, "Title of bbb") {
		t.Errorf("the comment must list every finding:\n%s", body)
	}
	if len(e.rows(t)) != 0 {
		t.Errorf("rows = %+v", e.rows(t))
	}
}

func TestInlineAdoptsUnrecordedThreads(t *testing.T) {
	e := newInlineEnv(t)
	e.disc.Seed("Earlier\n"+render.InlineMarker(fpBlocking), "db/query.go", 11, false)
	e.review(t, sha('a'), with("has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))

	if n := len(e.disc.Posted()); n != 0 {
		t.Errorf("%d discussions started, want none: the thread is there", n)
	}
	if r, ok := e.rows(t)[fpBlocking]; !ok || r.ThreadID != "disc1" || r.State != "open" || r.Skill != "security" {
		t.Errorf("rows = %+v, want the found thread recorded", e.rows(t))
	}
	if body := createdComment(t, e.env, 1); !strings.Contains(body, "> 1 finding posted as inline comments on the diff.") {
		t.Errorf("comment = %s", body)
	}
}

func TestInlineDropsRedactedSuggestion(t *testing.T) {
	e := newInlineEnv(t)
	if e.env.Engine.redact(testLLMKey) == testLLMKey {
		t.Fatal("the redactor doesn't know the LLM key")
	}
	e.review(t, sha('a'), with("has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", `key := "`+testLLMKey+`"`)))
	posts := e.disc.Posted()
	if len(posts) != 1 {
		t.Fatalf("%d discussions started, want 1", len(posts))
	}
	body, _ := posts[0].JSON(t)["body"].(string)
	if strings.Contains(body, "```suggestion") || strings.Contains(body, testLLMKey) {
		t.Errorf("body = %s, want no suggestion and no key", body)
	}
}

func TestInlineFailureKeepsPublish(t *testing.T) {
	e := newInlineEnv(t)
	e.disc.fail = http.StatusInternalServerError
	head := sha('a')
	servePublish(e.env, 1, head)
	e.env.GitLab.ServeDiff(1, head, queryFile)
	id := e.env.Report(1, head, resultFor(t, head, with("has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""))))
	publish(t, e.env)

	wantPublished(t, e.env, id)
	wantStatus(t, e.env, head, gitlab.StatusFailed, "Blocking issues found")
	body := createdComment(t, e.env, 1)
	if strings.Contains(body, "posted as inline") || !strings.Contains(body, "Title of aaa") || !strings.Contains(body, threads.NoteFailed) {
		t.Errorf("the finding must be in the comment with the failure noted:\n%s", body)
	}
	if len(e.rows(t)) != 0 {
		t.Errorf("rows = %+v", e.rows(t))
	}
}

func TestInlineOffBySetting(t *testing.T) {
	e := newInlineEnv(t)
	off := false
	e.env.Engine.Config.Repositories[0].InlineFindings = &off
	head := sha('a')
	servePublish(e.env, 1, head)
	e.env.GitLab.ServeDiff(1, head, queryFile)
	e.env.Report(1, head, resultFor(t, head, with("has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""))))
	publish(t, e.env)

	for _, p := range []string{diffsPath(1), discussionsPath(1), comparePath()} {
		if n := len(e.env.GitLab.Requests("", p)); n != 0 {
			t.Errorf("%d requests to %s, want none with the setting off", n, p)
		}
	}
	body := createdComment(t, e.env, 1)
	if strings.Contains(body, "posted as inline") || !strings.Contains(body, "Title of aaa") || strings.Contains(body, "Not on the diff") ||
		!strings.Contains(body, "Run again: `@pergent-review review`") || strings.Contains(body, "in a finding's thread") {
		t.Errorf("the comment must be as before inline threads:\n%s", body)
	}
}

func TestInlineDismissResolvesThread(t *testing.T) {
	e := newInlineEnv(t)
	e.review(t, sha('a'), with("has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))
	if _, err := e.env.Store.Dismiss(e.env.ctx, store.Dismissal{
		MR: e.env.MRKey(1), Fingerprint: fpBlocking, Severity: "blocking", Reason: "not a bug", Actor: "maya (gitlab main)",
	}); err != nil {
		t.Fatal(err)
	}
	e.review(t, sha('c'), with("has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))

	if replies := e.disc.Replies(t); len(replies) != 1 || replies[0] != "Dismissed by maya (gitlab main): not a bug" {
		t.Errorf("replies = %q", replies)
	}
	if !e.disc.Resolved("disc1") || e.rows(t)[fpBlocking].State != "dismissed" {
		t.Errorf("resolved = %t, state = %q, want the thread resolved and dismissed", e.disc.Resolved("disc1"), e.rows(t)[fpBlocking].State)
	}
	if n := len(e.disc.Posted()); n != 1 {
		t.Errorf("%d discussions started, want only the first", n)
	}

	// The dismissal is withdrawn: the thread opens again, with the reply.
	if ok, err := e.env.Store.Restore(e.env.ctx, e.env.MRKey(1), fpBlocking); err != nil || !ok {
		t.Fatalf("Restore = %v, %v", ok, err)
	}
	e.review(t, sha('d'), with("has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))
	replies := e.disc.Replies(t)
	if len(replies) != 2 || replies[1] != threads.ReplyRestored {
		t.Errorf("replies = %q, want the restored reply last", replies)
	}
	if e.disc.Resolved("disc1") || e.rows(t)[fpBlocking].State != "open" {
		t.Errorf("resolved = %t, state = %q, want the thread open again", e.disc.Resolved("disc1"), e.rows(t)[fpBlocking].State)
	}
	if body := e.comment(t); !strings.Contains(body, "> 1 finding posted as inline comments on the diff.") {
		t.Errorf("comment = %s", body)
	}
}

func TestInlineRuleKeepsSuggestions(t *testing.T) {
	e := newInlineEnv(t)
	e.env.GitLab.Reply(http.MethodGet, projectPath(), http.StatusOK,
		map[string]any{"id": testProjectID, "only_allow_merge_if_all_discussions_are_resolved": true})
	e.review(t, sha('a'), with("has_blocking_issues",
		finding(fpBlocking, "db/query.go", 11, 0, "blocking", ""),
		finding(fpSuggestion, "db/query.go", 12, 0, "suggestion", "")))

	if n := len(e.disc.Posted()); n != 1 {
		t.Fatalf("%d discussions started, want only the blocking finding's", n)
	}
	if rows := e.rows(t); len(rows) != 1 || rows[fpBlocking].ThreadID == "" {
		t.Errorf("rows = %+v", rows)
	}
	body := createdComment(t, e.env, 1)
	if !strings.Contains(body, threads.NoteKeptRule) || !strings.Contains(body, "Title of bbb") {
		t.Errorf("the suggestion must be in the comment with its note:\n%s", body)
	}
}
