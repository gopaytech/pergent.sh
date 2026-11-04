package threads

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/diffmap"
)

const head = "abcdef1234"

// diffOf builds a diff whose files each have the given hunks, as
// [start, length] pairs on the new side.
func diffOf(files map[string][][2]int) map[string]diffmap.File {
	out := map[string]diffmap.File{}
	for path, hs := range files {
		f := diffmap.File{Path: path}
		for _, h := range hs {
			f.Hunks = append(f.Hunks, diffmap.Hunk{NewStart: h[0], NewLines: h[1]})
		}
		out[path] = f
	}
	return out
}

func ok(files ...string) Coverage {
	c := Coverage{Reviewed: map[string]bool{}}
	for _, f := range files {
		c.Reviewed[f] = true
	}
	return c
}

func finding(fp, file, sev string, line, end int) Finding {
	return Finding{Fingerprint: fp, ID: "id-" + fp, Skill: "go", File: file, Severity: sev, Line: line, EndLine: end}
}

func thread(fp, file string, line int, state State) Thread {
	return Thread{Fingerprint: fp, Skill: "go", File: file, Line: line, EndLine: line, Commit: "0000000", State: state}
}

// base is an input with skill go reviewing a.go and z.go, both with a hunk at
// lines 40-59.
func base() Input {
	return Input{
		Head:   head,
		Skills: map[string]Coverage{"go": ok("a.go", "z.go")},
		Diff:   diffOf(map[string][][2]int{"a.go": {{40, 20}}, "z.go": {{40, 20}}}),
	}
}

func TestNewFindingPostedWhenOnDiff(t *testing.T) {
	in := base()
	in.Findings = []Finding{finding("f1", "a.go", "blocking", 42, 43)}
	p := Decide(in)
	if len(p.Post) != 1 || p.Post[0].Line != 42 || p.Post[0].EndLine != 43 || p.Post[0].Suggestion {
		t.Fatalf("post = %+v", p.Post)
	}
	if !p.Inline["f1"] {
		t.Errorf("Inline = %v", p.Inline)
	}
	if len(p.Notes) != 0 || len(p.Resolve)+len(p.Reopen)+len(p.Rebind) != 0 {
		t.Errorf("unexpected actions: %+v", p)
	}
}

func TestSuggestionOnlyWithinOneHunk(t *testing.T) {
	in := base()
	in.Diff = diffOf(map[string][][2]int{"a.go": {{10, 5}, {40, 5}}})
	mk := func(fp string, line, end int) Finding {
		f := finding(fp, "a.go", "suggestion", line, end)
		f.HasSuggestion = true
		return f
	}
	tests := []struct {
		name              string
		f                 Finding
		wantLine, wantEnd int
		wantSuggestion    bool
		wantNote          string
	}{
		{"one hunk", mk("one", 41, 43), 41, 43, true, ""},
		{"single line", mk("single", 41, 0), 41, 41, true, ""},
		{"two hunks", mk("two", 12, 41), 12, 12, false, ""},
		{"end off diff", mk("off", 12, 80), 0, 0, false, NoteOffDiff},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in.Findings = []Finding{tc.f}
			p := Decide(in)
			if tc.wantNote != "" {
				if len(p.Post) != 0 || p.Notes[tc.f.Fingerprint] != tc.wantNote || p.Inline[tc.f.Fingerprint] {
					t.Fatalf("plan = %+v", p)
				}
				return
			}
			if len(p.Post) != 1 {
				t.Fatalf("post = %+v", p.Post)
			}
			got := p.Post[0]
			if got.Line != tc.wantLine || got.EndLine != tc.wantEnd || got.Suggestion != tc.wantSuggestion {
				t.Errorf("post = %+v", got)
			}
		})
	}
}

func TestSuggestionNeedsHasSuggestion(t *testing.T) {
	in := base()
	in.Findings = []Finding{finding("f1", "a.go", "blocking", 42, 43)}
	if p := Decide(in); p.Post[0].Suggestion {
		t.Error("Suggestion true without HasSuggestion")
	}
}

func TestOffDiffAndNoLine(t *testing.T) {
	in := base()
	in.Findings = []Finding{
		finding("noline", "a.go", "blocking", 0, 0),
		finding("outside", "a.go", "blocking", 5, 0),
		finding("nofile", "q.go", "blocking", 42, 0),
	}
	p := Decide(in)
	if len(p.Post) != 0 {
		t.Fatalf("post = %+v", p.Post)
	}
	for _, fp := range []string{"noline", "outside", "nofile"} {
		if p.Notes[fp] != NoteOffDiff || p.Inline[fp] {
			t.Errorf("%s: notes = %q inline = %v", fp, p.Notes[fp], p.Inline[fp])
		}
	}
}

func TestOnlyBlockingKeepsSuggestions(t *testing.T) {
	in := base()
	in.OnlyBlocking = true
	in.Findings = []Finding{
		finding("b", "a.go", "blocking", 42, 0),
		finding("s", "a.go", "suggestion", 44, 0),
	}
	p := Decide(in)
	if len(p.Post) != 1 || p.Post[0].Finding.Fingerprint != "b" {
		t.Fatalf("post = %+v", p.Post)
	}
	if p.Notes["s"] != NoteKeptRule || p.Inline["s"] {
		t.Errorf("notes = %v inline = %v", p.Notes, p.Inline)
	}
}

// openThreads makes n open, unresolved threads in file other.go, none of which
// has a finding; their skill is other, which failed, so they are left alone.
func openThreads(n int) []Thread {
	var ts []Thread
	for i := 0; i < n; i++ {
		ts = append(ts, Thread{Fingerprint: fmt.Sprintf("t%02d", i), Skill: "other", File: "other.go", Line: i + 1, State: StateOpen})
	}
	return ts
}

func TestCapCountsOpenThreads(t *testing.T) {
	in := base()
	// 18 open threads, each with its finding found, so they are matched.
	for i := 0; i < 18; i++ {
		fp := fmt.Sprintf("m%02d", i)
		in.Threads = append(in.Threads, thread(fp, "a.go", 40+i%20, StateOpen))
		in.Findings = append(in.Findings, finding(fp, "a.go", "suggestion", 40+i%20, 0))
	}
	in.Findings = append(in.Findings,
		finding("s1", "a.go", "suggestion", 41, 0),
		finding("s2", "a.go", "suggestion", 42, 0),
		finding("b1", "z.go", "blocking", 41, 0),
		finding("b2", "z.go", "blocking", 42, 0),
	)
	p := Decide(in)
	if len(p.Post) != 2 || p.Post[0].Finding.Fingerprint != "b1" || p.Post[1].Finding.Fingerprint != "b2" {
		t.Fatalf("post = %+v", p.Post)
	}
	for _, fp := range []string{"s1", "s2"} {
		if p.Notes[fp] != NoteOverCap || p.Inline[fp] {
			t.Errorf("%s: notes = %q inline = %v", fp, p.Notes[fp], p.Inline[fp])
		}
	}
}

func TestCapCountsUnmatchedOpenThreads(t *testing.T) {
	in := base()
	in.Skills["other"] = Coverage{Failed: true}
	in.Diff["other.go"] = diffmap.File{Path: "other.go"}
	in.Threads = openThreads(19)
	in.Findings = []Finding{
		finding("n1", "a.go", "suggestion", 41, 0),
		finding("n2", "a.go", "suggestion", 42, 0),
		finding("n3", "a.go", "suggestion", 43, 0),
	}
	p := Decide(in)
	if len(p.Post) != 1 || p.Post[0].Finding.Fingerprint != "n1" {
		t.Fatalf("post = %+v", p.Post)
	}
	if len(p.Resolve)+len(p.Reopen)+len(p.Rebind) != 0 {
		t.Errorf("threads of a failed skill were touched: %+v", p)
	}
	for _, fp := range []string{"n2", "n3"} {
		if p.Notes[fp] != NoteOverCap {
			t.Errorf("%s: notes = %q", fp, p.Notes[fp])
		}
	}
}

func TestCapNetsOutResolvesAndReopens(t *testing.T) {
	in := base()
	// 20 open threads; one is fixed by this plan, so one slot is free.
	for i := 0; i < 20; i++ {
		in.Threads = append(in.Threads, thread(fmt.Sprintf("t%02d", i), "z.go", 41+i%10, StateOpen))
		if i > 0 {
			in.Findings = append(in.Findings, finding(fmt.Sprintf("t%02d", i), "z.go", "suggestion", 41+i%10, 0))
		}
	}
	in.Findings = append(in.Findings, finding("n1", "a.go", "suggestion", 41, 0), finding("n2", "a.go", "suggestion", 42, 0))
	p := Decide(in)
	if len(p.Resolve) != 1 || len(p.Post) != 1 || p.Notes["n2"] != NoteOverCap {
		t.Fatalf("plan = %+v", p)
	}

	// Reopening a thread takes a slot.
	in = base()
	in.Threads = append(openThreads(0), thread("back", "z.go", 41, StateFixed))
	in.Findings = []Finding{finding("back", "z.go", "suggestion", 41, 0), finding("n1", "a.go", "suggestion", 41, 0)}
	for i := 0; i < 19; i++ {
		fp := fmt.Sprintf("m%02d", i)
		in.Threads = append(in.Threads, thread(fp, "z.go", 42+i%10, StateOpen))
		in.Findings = append(in.Findings, finding(fp, "z.go", "suggestion", 42+i%10, 0))
	}
	p = Decide(in)
	if len(p.Reopen) != 1 || len(p.Post) != 0 || p.Notes["n1"] != NoteOverCap {
		t.Fatalf("plan = %+v", p)
	}
}

func TestCapIgnoresPersonResolvedAndDismissed(t *testing.T) {
	in := base()
	// Threads live in file old.go with no findings: the skill failed there.
	in.Skills["go"] = Coverage{Failed: true}
	for i := 0; i < 20; i++ {
		th := Thread{Fingerprint: fmt.Sprintf("t%02d", i), Skill: "go", File: "old.go", Line: i + 1, State: StateOpen}
		switch {
		case i == 0:
		case i%2 == 0:
			th.Resolved = true
		default:
			th.State = StateDismissed
			th.Resolved = true
		}
		in.Threads = append(in.Threads, th)
	}
	in.Diff["old.go"] = diffmap.File{Path: "old.go"}
	for i := 0; i < 25; i++ {
		in.Findings = append(in.Findings, finding(fmt.Sprintf("n%02d", i), "a.go", "suggestion", 40+i%20, 0))
	}
	p := Decide(in)
	if len(p.Post) != 19 {
		t.Fatalf("posted %d, want 19", len(p.Post))
	}
}

func TestExactMatchKept(t *testing.T) {
	in := base()
	in.Threads = []Thread{thread("f1", "a.go", 42, StateOpen)}
	in.Findings = []Finding{finding("f1", "a.go", "blocking", 42, 0)}
	p := Decide(in)
	if len(p.Post)+len(p.Resolve)+len(p.Reopen)+len(p.Rebind) != 0 || !p.Inline["f1"] || len(p.Notes) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestRewordedFindingRebound(t *testing.T) {
	old := thread("fpOld", "a.go", 40, StateOpen)
	old.EndLine = 41
	tests := []struct {
		name      string
		mapTo     func(Thread) (int, int, bool)
		findLine  int
		wantBound bool
	}{
		{"overlaps mapped range", func(Thread) (int, int, bool) { return 41, 42, true }, 42, true},
		{"mapped range misses it", func(Thread) (int, int, bool) { return 41, 42, true }, 45, false},
		{"unmappable, within 5", func(Thread) (int, int, bool) { return 0, 0, false }, 45, true},
		{"unmappable, too far", func(Thread) (int, int, bool) { return 0, 0, false }, 47, false},
		{"no Map func", nil, 45, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			in.Threads = []Thread{old}
			in.Findings = []Finding{finding("fpNew", "a.go", "blocking", tc.findLine, 0)}
			in.Map = tc.mapTo
			p := Decide(in)
			if tc.wantBound {
				want := []Rebind{{Thread: old, Fingerprint: "fpNew", Line: tc.findLine, EndLine: tc.findLine}}
				if !reflect.DeepEqual(p.Rebind, want) || len(p.Post) != 0 || len(p.Resolve) != 0 || !p.Inline["fpNew"] {
					t.Fatalf("plan = %+v", p)
				}
				return
			}
			if len(p.Rebind) != 0 || len(p.Post) != 1 || len(p.Resolve) != 1 ||
				p.Resolve[0].State != StateFixed || p.Resolve[0].Reply != "Fixed in `abcdef1`." {
				t.Fatalf("plan = %+v", p)
			}
		})
	}
}

func TestRewordedClosestWins(t *testing.T) {
	in := base()
	in.Threads = []Thread{thread("old", "a.go", 50, StateOpen)}
	in.Findings = []Finding{
		finding("far", "a.go", "blocking", 46, 0),
		finding("near", "a.go", "blocking", 52, 0),
	}
	p := Decide(in)
	if len(p.Rebind) != 1 || p.Rebind[0].Fingerprint != "near" {
		t.Fatalf("rebind = %+v", p.Rebind)
	}
	if len(p.Post) != 1 || p.Post[0].Finding.Fingerprint != "far" {
		t.Errorf("post = %+v", p.Post)
	}
}

func TestRewordedNeedsSameSkillAndFile(t *testing.T) {
	tests := []struct {
		name string
		f    Finding
	}{
		{"other skill", Finding{Fingerprint: "n", Skill: "sec", File: "a.go", Severity: "blocking", Line: 42}},
		{"other file", Finding{Fingerprint: "n", Skill: "go", File: "z.go", Severity: "blocking", Line: 42}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			in.Skills["sec"] = ok("a.go")
			in.Threads = []Thread{thread("old", "a.go", 42, StateOpen)}
			in.Findings = []Finding{tc.f}
			p := Decide(in)
			if len(p.Rebind) != 0 || len(p.Post) != 1 {
				t.Fatalf("plan = %+v", p)
			}
		})
	}
}

func TestGoneFindingFixed(t *testing.T) {
	in := base()
	in.Threads = []Thread{thread("f1", "a.go", 42, StateOpen)}
	p := Decide(in)
	want := []Resolve{{Thread: in.Threads[0], State: StateFixed, Reply: "Fixed in `abcdef1`."}}
	if !reflect.DeepEqual(p.Resolve, want) {
		t.Fatalf("resolve = %+v", p.Resolve)
	}
	if p.Inline["f1"] || len(p.Notes) != 0 {
		t.Errorf("plan = %+v", p)
	}
}

func TestFileLeftChange(t *testing.T) {
	in := base()
	in.Threads = []Thread{thread("f1", "gone.go", 42, StateOpen)}
	p := Decide(in)
	want := []Resolve{{Thread: in.Threads[0], State: StateGone, Reply: "No longer in this change."}}
	if !reflect.DeepEqual(p.Resolve, want) {
		t.Fatalf("resolve = %+v", p.Resolve)
	}

	// Even when the skill failed, a file that left the change is gone.
	in.Skills["go"] = Coverage{Failed: true}
	if p = Decide(in); !reflect.DeepEqual(p.Resolve, want) {
		t.Fatalf("failed skill: resolve = %+v", p.Resolve)
	}

	// A deleted file has no lines left to comment on.
	in = base()
	in.Diff["a.go"] = diffmap.File{Path: "a.go", Deleted: true}
	in.Threads = []Thread{thread("f1", "a.go", 42, StateOpen)}
	p = Decide(in)
	if len(p.Resolve) != 1 || p.Resolve[0].State != StateGone {
		t.Fatalf("deleted file: resolve = %+v", p.Resolve)
	}
}

func TestFailedSkillKeepsThreads(t *testing.T) {
	in := base()
	in.Skills["go"] = Coverage{Failed: true, Reviewed: map[string]bool{"a.go": true}}
	in.Threads = []Thread{thread("f1", "a.go", 42, StateOpen)}
	p := Decide(in)
	if len(p.Resolve)+len(p.Reopen)+len(p.Rebind)+len(p.Post) != 0 || len(p.Inline) != 0 || len(p.Notes) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestSkippedOrRemovedSkillKeepsThreads(t *testing.T) {
	tests := []struct {
		name   string
		skills map[string]Coverage
	}{
		{"skill removed", map[string]Coverage{}},
		{"file not reviewed", map[string]Coverage{"go": ok("z.go")}},
		{"no reviewed set", map[string]Coverage{"go": {}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			in.Skills = tc.skills
			in.Threads = []Thread{thread("f1", "a.go", 42, StateOpen)}
			p := Decide(in)
			if len(p.Resolve)+len(p.Reopen)+len(p.Rebind)+len(p.Post) != 0 || len(p.Inline) != 0 || len(p.Notes) != 0 {
				t.Fatalf("plan = %+v", p)
			}
		})
	}
}

func TestPersonResolvedThreadLeftAlone(t *testing.T) {
	in := base()
	th := thread("f1", "a.go", 42, StateOpen)
	th.Resolved = true
	in.Threads = []Thread{th}
	in.Findings = []Finding{finding("f1", "a.go", "blocking", 42, 0)}
	p := Decide(in)
	if len(p.Post)+len(p.Resolve)+len(p.Reopen)+len(p.Rebind) != 0 || !p.Inline["f1"] || len(p.Notes) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestReturningFindingReopens(t *testing.T) {
	for _, st := range []State{StateFixed, StateGone} {
		t.Run(string(st), func(t *testing.T) {
			in := base()
			in.Threads = []Thread{thread("f1", "a.go", 42, st)}
			in.Findings = []Finding{finding("f1", "a.go", "blocking", 42, 0)}
			p := Decide(in)
			want := []Reopen{{Thread: in.Threads[0], Reply: "Back in `abcdef1`."}}
			if !reflect.DeepEqual(p.Reopen, want) || len(p.Post) != 0 || !p.Inline["f1"] {
				t.Fatalf("plan = %+v", p)
			}
		})
	}
}

func TestDismissedResolvesThread(t *testing.T) {
	tests := []struct {
		name      string
		d         Dismissal
		state     State
		wantReply string // empty: no action
	}{
		{"with reason", Dismissal{By: "mo (gitlab main)", Reason: "false positive"}, StateOpen, "Dismissed by mo (gitlab main): false positive"},
		{"no reason", Dismissal{By: "mo (gitlab main)"}, StateOpen, "Dismissed by mo (gitlab main)."},
		{"already dismissed", Dismissal{By: "mo (gitlab main)"}, StateDismissed, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			in.Threads = []Thread{thread("f1", "a.go", 42, tc.state)}
			in.Dismissed = map[string]Dismissal{"f1": tc.d}
			p := Decide(in)
			if tc.wantReply == "" {
				if len(p.Resolve)+len(p.Reopen)+len(p.Rebind)+len(p.Post) != 0 {
					t.Fatalf("plan = %+v", p)
				}
				return
			}
			want := []Resolve{{Thread: in.Threads[0], State: StateDismissed, Reply: tc.wantReply}}
			if !reflect.DeepEqual(p.Resolve, want) || p.Inline["f1"] {
				t.Fatalf("plan = %+v", p)
			}
		})
	}
}

func TestRestoredDismissalReopens(t *testing.T) {
	in := base()
	in.Threads = []Thread{thread("f1", "a.go", 42, StateDismissed)}
	in.Findings = []Finding{finding("f1", "a.go", "blocking", 42, 0)}
	p := Decide(in)
	want := []Reopen{{Thread: in.Threads[0], Reply: ReplyRestored}}
	if !reflect.DeepEqual(p.Reopen, want) || !p.Inline["f1"] || len(p.Post) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestOrderBlockingThenFileThenLine(t *testing.T) {
	in := base()
	in.Findings = []Finding{
		finding("s-a-41", "a.go", "suggestion", 41, 0),
		finding("b-z-41", "z.go", "blocking", 41, 0),
		finding("b-a-50", "a.go", "blocking", 50, 0),
		finding("b-a-45", "a.go", "blocking", 45, 0),
		finding("s-a-40", "a.go", "suggestion", 40, 0),
	}
	p := Decide(in)
	var got []string
	for _, post := range p.Post {
		got = append(got, post.Finding.Fingerprint)
	}
	want := []string{"b-a-45", "b-a-50", "b-z-41", "s-a-40", "s-a-41"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("posts = %v, want %v", got, want)
	}
}

func TestResolvesSortedByFileLineFingerprint(t *testing.T) {
	in := base()
	in.Threads = []Thread{
		thread("c", "z.go", 41, StateOpen),
		thread("b", "a.go", 50, StateOpen),
		thread("a", "a.go", 45, StateOpen),
		thread("d", "gone.go", 1, StateOpen),
	}
	p := Decide(in)
	var got []string
	for _, r := range p.Resolve {
		got = append(got, r.Thread.Fingerprint)
	}
	want := []string{"a", "b", "d", "c"}
	// a.go:45, a.go:50, gone.go:1, z.go:41
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolves = %v, want %v", got, want)
	}
}

func TestReplies(t *testing.T) {
	if got := ShortSHA("abcdef1234"); got != "abcdef1" {
		t.Errorf("ShortSHA = %q", got)
	}
	if got := ShortSHA("abc"); got != "abc" {
		t.Errorf("ShortSHA short = %q", got)
	}
	if got := ReplyFixed(head); got != "Fixed in `abcdef1`." {
		t.Errorf("ReplyFixed = %q", got)
	}
	if got := ReplyBack(head); got != "Back in `abcdef1`." {
		t.Errorf("ReplyBack = %q", got)
	}
}
