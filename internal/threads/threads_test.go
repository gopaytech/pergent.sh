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

// A thread told that its finding was left out says so when the finding is
// reported again: its last message is wrong. The finding stays on the diff.
func TestLeftOutFindingComesBack(t *testing.T) {
	back := func(noted, resolved bool) Plan {
		in := base()
		in.Findings = []Finding{finding("f1", "a.go", "blocking", 42, 0)}
		th := thread("f1", "a.go", 42, StateOpen)
		th.LeftOutNoted, th.Resolved = noted, resolved
		in.Threads = []Thread{th}
		return Decide(in)
	}

	p := back(true, false)
	want := []Note{{Thread: thread("f1", "a.go", 42, StateOpen), Reply: "Back in `abcdef1`."}}
	want[0].Thread.LeftOutNoted = true
	if !reflect.DeepEqual(p.Back, want) || !p.Inline["f1"] || len(p.Post)+len(p.Reopen)+len(p.Note) != 0 {
		t.Errorf("told and matched: plan = %+v; want one Back entry, still inline", p)
	}
	if p = back(true, true); len(p.Back) != 0 || !p.Inline["f1"] {
		t.Errorf("resolved by a person: plan = %+v; want no Back entry, still inline", p)
	}
	if p = back(false, false); len(p.Back) != 0 || !p.Inline["f1"] {
		t.Errorf("never told: plan = %+v; want no Back entry, still inline", p)
	}
}

func TestBackIsSorted(t *testing.T) {
	in := base()
	in.Findings = []Finding{finding("f1", "z.go", "blocking", 42, 0), finding("f2", "a.go", "blocking", 50, 0), finding("f3", "a.go", "blocking", 41, 0)}
	for _, t := range []Thread{thread("f1", "z.go", 42, StateOpen), thread("f2", "a.go", 50, StateOpen), thread("f3", "a.go", 41, StateOpen)} {
		t.LeftOutNoted = true
		in.Threads = append(in.Threads, t)
	}
	var got []string
	for _, n := range Decide(in).Back {
		got = append(got, n.Thread.Fingerprint)
	}
	if want := []string{"f3", "f2", "f1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("back = %v, want %v (file, then line)", got, want)
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

// A skill that left a finding out because of a past dismissal says nothing
// about the code, so its thread isn't resolved as fixed. It is told once.
func TestLeftOutFindingIsNotFixed(t *testing.T) {
	leftOut := func() Input {
		in := base()
		c := in.Skills["go"]
		c.LeftOut = true
		in.Skills["go"] = c
		in.Threads = []Thread{thread("f1", "a.go", 42, StateOpen)}
		return in
	}

	in := leftOut()
	p := Decide(in)
	wantNote := []Note{{Thread: in.Threads[0], Reply: "No longer reported in `abcdef1`; see the note on past dismissals in the comment."}}
	if len(p.Resolve) != 0 || !reflect.DeepEqual(p.Note, wantNote) {
		t.Fatalf("resolve = %+v, note = %+v; want a note and no resolve", p.Resolve, p.Note)
	}

	// Already told: nothing.
	in = leftOut()
	in.Threads[0].LeftOutNoted = true
	if p = Decide(in); len(p.Resolve)+len(p.Note) != 0 {
		t.Errorf("told already: resolve = %+v, note = %+v; want nothing", p.Resolve, p.Note)
	}

	// Without the note on the skill the finding is fixed, as before.
	in = leftOut()
	in.Skills["go"] = ok("a.go", "z.go")
	if p = Decide(in); len(p.Note) != 0 || len(p.Resolve) != 1 || p.Resolve[0].State != StateFixed {
		t.Errorf("no left-out note: resolve = %+v, note = %+v; want fixed", p.Resolve, p.Note)
	}

	// A failed skill, or a file it didn't review, says nothing either way.
	in = leftOut()
	in.Skills["go"] = Coverage{Failed: true, LeftOut: true, Reviewed: map[string]bool{"a.go": true}}
	if p = Decide(in); len(p.Resolve)+len(p.Note) != 0 {
		t.Errorf("failed skill: resolve = %+v, note = %+v; want nothing", p.Resolve, p.Note)
	}
	in = leftOut()
	in.Skills["go"] = Coverage{LeftOut: true, Reviewed: map[string]bool{"z.go": true}}
	if p = Decide(in); len(p.Resolve)+len(p.Note) != 0 {
		t.Errorf("file not reviewed: resolve = %+v, note = %+v; want nothing", p.Resolve, p.Note)
	}

	// A file that left the change is gone all the same.
	in = leftOut()
	in.Threads = []Thread{thread("f1", "gone.go", 42, StateOpen)}
	if p = Decide(in); len(p.Note) != 0 || len(p.Resolve) != 1 || p.Resolve[0].State != StateGone {
		t.Errorf("gone file: resolve = %+v, note = %+v; want gone", p.Resolve, p.Note)
	}

	// A thread that stays open counts toward the cap.
	in = leftOut()
	in.Skills["other"] = Coverage{Failed: true}
	in.Diff["other.go"] = diffmap.File{Path: "other.go"}
	in.Threads = append(openThreads(Cap-1), in.Threads...)
	in.Findings = []Finding{finding("new", "z.go", "blocking", 45, 0)}
	if p = Decide(in); len(p.Post) != 0 || p.Notes["new"] != NoteOverCap {
		t.Errorf("post = %+v, notes = %v; want the left-out thread to count as open", p.Post, p.Notes)
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

func TestLiveThreadClaimsRewordedFindingBeforeFixedOne(t *testing.T) {
	in := base()
	in.Map = func(t Thread) (int, int, bool) { return t.Line, t.EndLine, true }
	old := thread("aaOld", "a.go", 42, StateFixed)
	live := thread("zzLive", "a.go", 42, StateOpen)
	in.Threads = []Thread{old, live}
	in.Findings = []Finding{finding("fpNew", "a.go", "blocking", 42, 0)}
	p := Decide(in)
	want := []Rebind{{Thread: live, Fingerprint: "fpNew", Line: 42, EndLine: 42}}
	if !reflect.DeepEqual(p.Rebind, want) {
		t.Fatalf("rebind = %+v", p.Rebind)
	}
	if len(p.Resolve)+len(p.Reopen)+len(p.Post) != 0 || !p.Inline["fpNew"] {
		t.Errorf("plan = %+v", p)
	}

	// An open thread a person resolved comes before a fixed one too.
	live.Resolved = true
	in.Threads = []Thread{old, live}
	p = Decide(in)
	if len(p.Rebind) != 1 || p.Rebind[0].Thread.Fingerprint != "zzLive" || len(p.Reopen) != 0 {
		t.Fatalf("person-resolved: plan = %+v", p)
	}

	// Without a live thread the fixed one is rebound and reopened.
	in.Threads = []Thread{old}
	p = Decide(in)
	if len(p.Rebind) != 1 || p.Rebind[0].Thread.Fingerprint != "aaOld" || len(p.Reopen) != 1 {
		t.Fatalf("fixed only: plan = %+v", p)
	}
}

func TestDuplicateFingerprintsKeepFirst(t *testing.T) {
	dups := func() []Finding {
		return []Finding{
			finding("dup", "z.go", "suggestion", 41, 0),
			finding("dup", "a.go", "suggestion", 50, 0),
			finding("dup", "a.go", "suggestion", 45, 0),
		}
	}

	in := base()
	in.Findings = dups()
	p := Decide(in)
	if len(p.Post) != 1 || p.Post[0].Line != 45 || p.Post[0].Finding.File != "a.go" {
		t.Fatalf("post = %+v", p.Post)
	}
	if len(p.Notes) != 0 {
		t.Errorf("notes = %v", p.Notes)
	}

	// A blocking duplicate comes first whatever its place.
	in.Findings = append(dups(), finding("dup", "z.go", "blocking", 59, 0))
	if p = Decide(in); len(p.Post) != 1 || p.Post[0].Line != 59 {
		t.Fatalf("blocking first: post = %+v", p.Post)
	}

	// 19 open threads leave one slot: one Post, no note.
	in = base()
	in.Skills["other"] = Coverage{Failed: true}
	in.Diff["other.go"] = diffmap.File{Path: "other.go"}
	in.Threads = openThreads(19)
	in.Findings = dups()
	p = Decide(in)
	if len(p.Post) != 1 || len(p.Notes) != 0 || !p.Inline["dup"] {
		t.Fatalf("19 open: plan = %+v", p)
	}

	// 20 open threads leave none: one note, and it is not inline.
	in.Threads = openThreads(20)
	p = Decide(in)
	if len(p.Post) != 0 || p.Notes["dup"] != NoteOverCap || p.Inline["dup"] {
		t.Fatalf("20 open: plan = %+v", p)
	}
	checkDisjoint(t, p)
}

func TestDuplicateFingerprintsMatchOneThread(t *testing.T) {
	in := base()
	in.Threads = []Thread{thread("dup", "a.go", 42, StateFixed)}
	in.Findings = []Finding{finding("dup", "a.go", "blocking", 42, 0), finding("dup", "a.go", "blocking", 50, 0)}
	p := Decide(in)
	if len(p.Reopen) != 1 || len(p.Post) != 0 || len(p.Notes) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestOnlyBlockingDoesNotReopenSuggestions(t *testing.T) {
	for _, st := range []State{StateFixed, StateGone, StateDismissed} {
		t.Run(string(st), func(t *testing.T) {
			in := base()
			in.OnlyBlocking = true
			in.Threads = []Thread{
				thread("s", "a.go", 42, st),
				thread("b", "a.go", 44, st),
				thread("open", "a.go", 46, StateOpen),
			}
			in.Findings = []Finding{
				finding("s", "a.go", "suggestion", 42, 0),
				finding("b", "a.go", "blocking", 44, 0),
				finding("open", "a.go", "suggestion", 46, 0),
			}
			p := Decide(in)
			if len(p.Reopen) != 1 || p.Reopen[0].Thread.Fingerprint != "b" {
				t.Fatalf("reopen = %+v", p.Reopen)
			}
			if p.Notes["s"] != NoteKeptRule || p.Inline["s"] {
				t.Errorf("suggestion: notes = %v inline = %v", p.Notes, p.Inline)
			}
			checkDisjoint(t, p)
			if !p.Inline["open"] || p.Notes["open"] != "" || !p.Inline["b"] {
				t.Errorf("inline = %v notes = %v", p.Inline, p.Notes)
			}
		})
	}
}

func TestOnlyBlockingOffDiffSuggestionIsOffDiff(t *testing.T) {
	in := base()
	in.OnlyBlocking = true
	in.Findings = []Finding{finding("s", "a.go", "suggestion", 5, 0)}
	p := Decide(in)
	if p.Notes["s"] != NoteOffDiff || len(p.Post) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPersonResolvedThreadWithGoneFindingLeftAlone(t *testing.T) {
	for _, file := range []string{"a.go", "gone.go"} {
		t.Run(file, func(t *testing.T) {
			in := base()
			th := thread("f1", file, 42, StateOpen)
			th.Resolved = true
			in.Threads = []Thread{th}
			p := Decide(in)
			if len(p.Resolve)+len(p.Reopen)+len(p.Rebind)+len(p.Post) != 0 || len(p.Inline) != 0 || len(p.Notes) != 0 {
				t.Fatalf("plan = %+v", p)
			}
		})
	}
}

func TestUnknownStateTreatedAsOpen(t *testing.T) {
	in := base()
	in.Threads = []Thread{thread("f1", "a.go", 42, State("weird"))}
	in.Findings = []Finding{finding("f1", "a.go", "blocking", 42, 0)}
	p := Decide(in)
	if len(p.Post)+len(p.Resolve)+len(p.Reopen)+len(p.Rebind) != 0 || !p.Inline["f1"] || len(p.Notes) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

// checkDisjoint fails when a fingerprint is both on the diff and in the summary.
func checkDisjoint(t *testing.T, p Plan) {
	t.Helper()
	for fp := range p.Notes {
		if p.Inline[fp] {
			t.Errorf("%s is in both Inline and Notes", fp)
		}
	}
}

func TestOnlyBlockingRewordedSuggestionNotInlineAndNotes(t *testing.T) {
	for _, st := range []State{StateFixed, StateGone} {
		t.Run(string(st), func(t *testing.T) {
			in := base()
			in.OnlyBlocking = true
			in.Map = func(t Thread) (int, int, bool) { return t.Line, t.EndLine, true }
			in.Threads = []Thread{thread("old", "a.go", 42, st)}
			in.Findings = []Finding{finding("fpNew", "a.go", "suggestion", 42, 0)}
			p := Decide(in)
			if len(p.Rebind) != 1 || len(p.Reopen) != 0 || len(p.Post) != 0 {
				t.Fatalf("plan = %+v", p)
			}
			if p.Notes["fpNew"] != NoteKeptRule || p.Inline["fpNew"] {
				t.Errorf("notes = %v inline = %v", p.Notes, p.Inline)
			}
			checkDisjoint(t, p)
		})
	}
}

func TestUnknownStateCountsInCap(t *testing.T) {
	in := base()
	in.Threads = []Thread{thread("weird", "z.go", 41, State("weird"))}
	in.Findings = []Finding{finding("weird", "z.go", "blocking", 41, 0)}
	for i := 0; i < 20; i++ {
		in.Findings = append(in.Findings, finding(fmt.Sprintf("n%02d", i), "a.go", "blocking", 40+i, 0))
	}
	p := Decide(in)
	if len(p.Post) != 19 || len(p.Inline) != 20 {
		t.Fatalf("posts = %d, inline = %d", len(p.Post), len(p.Inline))
	}
	checkDisjoint(t, p)

	// A person-resolved one does not count.
	in.Threads[0].Resolved = true
	if p = Decide(in); len(p.Post) != 20 {
		t.Fatalf("resolved: posts = %d", len(p.Post))
	}
}

func TestDismissedStateThreadNeverRewordedCandidate(t *testing.T) {
	in := base()
	in.Map = func(t Thread) (int, int, bool) { return t.Line, t.EndLine, true }
	in.Threads = []Thread{thread("old", "a.go", 42, StateDismissed)}
	in.Findings = []Finding{finding("fpNew", "a.go", "blocking", 42, 0)}
	p := Decide(in)
	if len(p.Rebind) != 0 || len(p.Reopen) != 0 || len(p.Post) != 1 || p.Post[0].Finding.Fingerprint != "fpNew" {
		t.Fatalf("plan = %+v", p)
	}
	checkDisjoint(t, p)
}
