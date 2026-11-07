package threads

import "testing"

func impactThread(fp string) Thread {
	t := thread(fp, "a.go", 42, StateOpen)
	t.Skill = "impact"
	return t
}

// The impact skill lists no file it reviewed, but a run that finished covers
// the change: a finding it no longer reports is fixed.
func TestImpactThreadResolvesWhenTheCheckFinished(t *testing.T) {
	in := base()
	in.Skills["impact"] = Coverage{AllFiles: true}
	in.Threads = []Thread{impactThread("f1")}
	p := Decide(in)
	if len(p.Resolve) != 1 || p.Resolve[0].State != StateFixed || p.Resolve[0].Thread.Fingerprint != "f1" {
		t.Fatalf("resolve = %+v; want the thread fixed", p.Resolve)
	}
}

// It does not reach a file that left the change, or one that is not in it: the
// thread is gone then, for the reason it always is.
func TestImpactThreadOfAGoneFileIsGone(t *testing.T) {
	in := base()
	in.Skills["impact"] = Coverage{AllFiles: true}
	th := impactThread("f1")
	th.File = "removed.go"
	in.Threads = []Thread{th}
	p := Decide(in)
	if len(p.Resolve) != 1 || p.Resolve[0].State != StateGone {
		t.Fatalf("resolve = %+v; want gone", p.Resolve)
	}
}

// A check that failed, or did not run, says nothing: the thread stays.
func TestImpactThreadStaysWhenTheCheckDidNotFinish(t *testing.T) {
	for name, skills := range map[string]map[string]Coverage{
		"failed":      {"go": ok("a.go"), "impact": {AllFiles: true, Failed: true}},
		"did not run": {"go": ok("a.go")},
	} {
		t.Run(name, func(t *testing.T) {
			in := base()
			in.Skills = skills
			in.Threads = []Thread{impactThread("f1")}
			p := Decide(in)
			if len(p.Resolve)+len(p.Reopen)+len(p.Rebind)+len(p.Post) != 0 || len(p.Inline) != 0 || len(p.Notes) != 0 {
				t.Fatalf("plan = %+v", p)
			}
		})
	}
}

// AllFiles belongs to the skill it is set on: another skill's thread on the
// same file is not covered by it.
func TestAllFilesCoversOnlyItsSkill(t *testing.T) {
	in := base()
	in.Skills = map[string]Coverage{"go": {}, "impact": {AllFiles: true}}
	in.Threads = []Thread{thread("f1", "a.go", 42, StateOpen)}
	if p := Decide(in); len(p.Resolve) != 0 {
		t.Fatalf("resolve = %+v; want none", p.Resolve)
	}
}

// A quiet review keeps the impact suggestions in the comment, and posts no
// thread for them, like any skill's.
func TestQuietKeepsImpactSuggestionsInTheComment(t *testing.T) {
	in := base()
	in.Quiet = true
	in.Skills["impact"] = Coverage{AllFiles: true}
	f := finding("f1", "a.go", "suggestion", 42, 42)
	f.Skill = "impact"
	in.Findings = []Finding{f}
	p := Decide(in)
	if len(p.Post) != 0 || p.Notes["f1"] != NoteKeptQuiet || p.Inline["f1"] {
		t.Fatalf("plan = %+v; want the suggestion kept in the comment", p)
	}
}
