package store

import (
	"slices"
	"testing"
)

func inlineThread(fingerprint, thread string, comment int64) InlineThread {
	return InlineThread{MR: mr1, Fingerprint: fingerprint, Skill: "security", File: "pkg/db/query.go", Line: 42, EndLine: 44,
		Commit: "aaaa", ThreadID: thread, CommentID: comment, State: "open"}
}

func fingerprints(list []InlineThread) []string {
	var out []string
	for _, t := range list {
		out = append(out, t.Fingerprint)
	}
	return out
}

func TestInlineThreadsRoundTrip(t *testing.T) {
	s := newStore(t)

	if got, err := s.InlineThreads(ctx, mr1); err != nil || len(got) != 0 {
		t.Fatalf("InlineThreads on an empty store = %v, %v", got, err)
	}

	one := inlineThread("f1", "PRRT_one", 101)
	two := inlineThread("f2", "PRRT_two", 102)
	two.File, two.Line, two.EndLine, two.Skill = "cmd/main.go", 7, 0, ""
	other := inlineThread("f1", "PRRT_other", 201) // the same fingerprint on another MR
	other.MR = mr2
	for _, th := range []InlineThread{one, two, other} {
		if err := s.PutInlineThread(ctx, th); err != nil {
			t.Fatalf("PutInlineThread(%s): %v", th.Fingerprint, err)
		}
	}

	got, err := s.InlineThreads(ctx, mr1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fingerprints(got), []string{"f2", "f1"}) { // by file, then line
		t.Fatalf("InlineThreads = %v, want f2 (cmd/main.go) then f1", fingerprints(got))
	}
	if got[0] != two || got[1] != one {
		t.Errorf("InlineThreads = %+v, want %+v and %+v", got, two, one)
	}
	if onOther, _ := s.InlineThreads(ctx, mr2); len(onOther) != 1 || onOther[0] != other {
		t.Errorf("InlineThreads of the other MR = %+v", onOther)
	}

	// A put on a known fingerprint replaces the row.
	one.Line, one.EndLine, one.Commit, one.CommentID = 50, 0, "bbbb", 111
	if err := s.PutInlineThread(ctx, one); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.InlineThreads(ctx, mr1); len(got) != 2 || got[1] != one {
		t.Errorf("after the second put = %+v, want %+v", got, one)
	}

	// States.
	if err := s.SetInlineThreadState(ctx, mr1, "f1", "fixed"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.InlineThreads(ctx, mr1); got[1].State != "fixed" || got[0].State != "open" {
		t.Errorf("states = %q, %q; want open, fixed", got[0].State, got[1].State)
	}
	if err := s.SetInlineThreadState(ctx, mr1, "f1", "resolved"); err == nil {
		t.Error("SetInlineThreadState accepted an unknown state")
	}
	bad := inlineThread("f3", "PRRT_three", 103)
	bad.State = "maybe"
	if err := s.PutInlineThread(ctx, bad); err == nil {
		t.Error("PutInlineThread accepted an unknown state")
	}
	if err := s.SetInlineThreadState(ctx, mr1, "nope", "gone"); err != nil {
		t.Errorf("SetInlineThreadState of an unknown fingerprint: %v", err)
	}

	// Rebind: the thread follows a reworded finding to a new fingerprint and place.
	if err := s.RebindInlineThread(ctx, mr1, "f1", "f1b", 60, 62, "cccc"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.InlineThreads(ctx, mr1)
	if !slices.Equal(fingerprints(got), []string{"f2", "f1b"}) {
		t.Fatalf("after the rebind = %v", fingerprints(got))
	}
	if r := got[1]; r.Line != 60 || r.EndLine != 62 || r.Commit != "cccc" || r.ThreadID != "PRRT_one" ||
		r.CommentID != 111 || r.State != "fixed" || r.File != "pkg/db/query.go" {
		t.Errorf("rebound row = %+v", r)
	}
	// Rebinding onto a fingerprint that has a thread is refused, and changes nothing.
	if err := s.RebindInlineThread(ctx, mr1, "f1b", "f2", 1, 0, "dddd"); err == nil {
		t.Error("RebindInlineThread onto an existing fingerprint was accepted")
	}
	if err := s.RebindInlineThread(ctx, mr1, "nope", "nope2", 1, 0, "dddd"); err != nil {
		t.Errorf("RebindInlineThread of an unknown fingerprint: %v", err)
	}

	// InlineThreadFor: by discussion (GitLab) or by the first comment (GitHub).
	th, found, err := s.InlineThreadFor(ctx, mr1, "PRRT_one", 0)
	if err != nil || !found || th.Fingerprint != "f1b" {
		t.Errorf("InlineThreadFor(discussion) = %+v, %v, %v", th, found, err)
	}
	th, found, err = s.InlineThreadFor(ctx, mr1, "", 102)
	if err != nil || !found || th.Fingerprint != "f2" {
		t.Errorf("InlineThreadFor(reply to) = %+v, %v, %v", th, found, err)
	}
	// The discussion wins when both are given.
	th, found, _ = s.InlineThreadFor(ctx, mr1, "PRRT_one", 102)
	if !found || th.Fingerprint != "f1b" {
		t.Errorf("InlineThreadFor(both) = %+v, %v; want the discussion's thread", th, found)
	}
	for _, c := range []struct {
		mr         MRKey
		discussion string
		replyTo    int64
	}{
		{mr1, "PRRT_unknown", 0},
		{mr1, "", 999},
		{mr1, "", 0},         // not a reply: nothing to find
		{mr2, "PRRT_one", 0}, // another MR's thread
		{mr2, "", 102},
	} {
		if th, found, err := s.InlineThreadFor(ctx, c.mr, c.discussion, c.replyTo); err != nil || found {
			t.Errorf("InlineThreadFor(%+v) = %+v, %v, %v; want not found", c, th, found, err)
		}
	}

	// Delete.
	if err := s.DeleteInlineThread(ctx, mr1, "f1b"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.InlineThreads(ctx, mr1); !slices.Equal(fingerprints(got), []string{"f2"}) {
		t.Errorf("after the delete = %v", fingerprints(got))
	}
	if err := s.DeleteInlineThread(ctx, mr1, "f1b"); err != nil {
		t.Errorf("deleting a missing thread: %v", err)
	}
	if onOther, _ := s.InlineThreads(ctx, mr2); len(onOther) != 1 {
		t.Errorf("the other MR's thread went with it: %+v", onOther)
	}
}

func TestInlineThreadsUniqueThreadID(t *testing.T) {
	s := newStore(t)
	if err := s.PutInlineThread(ctx, inlineThread("f1", "PRRT_one", 101)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutInlineThread(ctx, inlineThread("f2", "PRRT_one", 102)); err == nil {
		t.Error("a second fingerprint with the same thread_id was stored")
	}
	// The same thread ID on another MR is another thread.
	other := inlineThread("f2", "PRRT_one", 102)
	other.MR = mr2
	if err := s.PutInlineThread(ctx, other); err != nil {
		t.Errorf("the same thread_id on another MR: %v", err)
	}
	if got, _ := s.InlineThreads(ctx, mr1); len(got) != 1 {
		t.Errorf("InlineThreads = %+v", got)
	}
}

func TestDeleteProjectDeletesInlineThreads(t *testing.T) {
	s := newStore(t)
	inst := mr1.Instance
	if err := s.UpsertProject(ctx, Project{Instance: inst, ProjectID: mr1.ProjectID, Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutInlineThread(ctx, inlineThread("f1", "d1", 101)); err != nil {
		t.Fatal(err)
	}
	keep := inlineThread("f1", "d9", 901)
	keep.MR = MRKey{Instance: inst, ProjectID: 8, IID: 1}
	if err := s.PutInlineThread(ctx, keep); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteProject(ctx, inst, mr1.ProjectID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.InlineThreads(ctx, mr1); err != nil || len(got) != 0 {
		t.Errorf("threads after DeleteProject = %+v, %v; want none", got, err)
	}
	if got, _ := s.InlineThreads(ctx, keep.MR); len(got) != 1 {
		t.Errorf("another project's threads = %+v; want kept", got)
	}
}

func TestNoteInlineThreadLeftOut(t *testing.T) {
	s := newStore(t)
	th := inlineThread("f1", "PRRT_one", 101)
	if err := s.PutInlineThread(ctx, th); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.InlineThreads(ctx, mr1); len(got) != 1 || got[0].LeftOutNoted {
		t.Fatalf("threads = %+v; want one not yet told", got)
	}
	if err := s.NoteInlineThreadLeftOut(ctx, mr1, "f1"); err != nil {
		t.Fatal(err)
	}
	// Putting the thread again (a reopen) keeps what it was told.
	if err := s.PutInlineThread(ctx, th); err != nil {
		t.Fatal(err)
	}
	got, err := s.InlineThreads(ctx, mr1)
	if err != nil || len(got) != 1 || !got[0].LeftOutNoted {
		t.Fatalf("threads = %+v, %v; want it told", got, err)
	}
	// A fingerprint with no thread is not an error.
	if err := s.NoteInlineThreadLeftOut(ctx, mr1, "nope"); err != nil {
		t.Errorf("NoteInlineThreadLeftOut with no thread = %v", err)
	}
}

func TestClearInlineThreadLeftOut(t *testing.T) {
	s := newStore(t)
	th := inlineThread("f1", "PRRT_one", 101)
	if err := s.PutInlineThread(ctx, th); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteInlineThreadLeftOut(ctx, mr1, "f1"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutInlineThread(ctx, inlineThread("f2", "PRRT_two", 102)); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteInlineThreadLeftOut(ctx, mr1, "f2"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearInlineThreadLeftOut(ctx, mr1, "f1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.InlineThreads(ctx, mr1)
	if err != nil || len(got) != 2 {
		t.Fatalf("threads = %+v, %v", got, err)
	}
	for _, g := range got {
		if want := g.Fingerprint == "f2"; g.LeftOutNoted != want {
			t.Errorf("%s: LeftOutNoted = %t, want %t (only f1 is cleared)", g.Fingerprint, g.LeftOutNoted, want)
		}
	}
	// It can be told again, and a fingerprint with no thread is not an error.
	if err := s.NoteInlineThreadLeftOut(ctx, mr1, "f1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.InlineThreads(ctx, mr1); !got[0].LeftOutNoted || got[0].Fingerprint != "f1" {
		t.Errorf("threads = %+v; want f1 told again", got)
	}
	if err := s.ClearInlineThreadLeftOut(ctx, mr1, "nope"); err != nil {
		t.Errorf("ClearInlineThreadLeftOut with no thread = %v", err)
	}
}
