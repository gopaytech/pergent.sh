package store

import (
	"slices"
	"testing"
)

// A start finds a multi review's waiting run on its way and leaves it as it
// is; a held run is started as before.
func TestStartRunLeavesMultiReviewAlone(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	multi := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	held := heldRun(t, s, mr2, "b1", WaitUntrusted)

	id, started, err := s.StartRun(ctx, mr1, "acme/widgets", "a1", "main")
	if err != nil || id != multi || started {
		t.Fatalf("StartRun(multi review) = %s, %v, %v; want %s, false", id, started, err, multi)
	}
	if r := getRun(t, s, multi); r.State != Waiting || r.SkipReason != WaitReviewers || r.Trigger != TriggerPush {
		t.Errorf("run = %s %q %s; want still waiting for its members", r.State, r.SkipReason, r.Trigger)
	}
	got := reviewerStates(t, s, multi)
	if !slices.Equal(got[1], []MemberState{MemberQueued}) || !slices.Equal(got[2], []MemberState{MemberQueued}) {
		t.Errorf("members = %v; want both still queued", got)
	}

	id, started, err = s.StartRun(ctx, mr2, "acme/widgets", "b1", "main")
	if err != nil || id != held || !started {
		t.Fatalf("StartRun(held) = %s, %v, %v; want %s, true", id, started, err, held)
	}
	if r := getRun(t, s, held); r.State != Queued || r.Trigger != TriggerStart {
		t.Errorf("held run = %s %s; want queued as a start", r.State, r.Trigger)
	}
}

// A re-run while the members run lands on the review's run; a held run is
// replaced by a new one as before.
func TestRerunWhileMultiReviewRuns(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	multi := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	held := heldRun(t, s, mr2, "b1", WaitUntrusted)

	if active, err := s.ReviewActiveForRerun(ctx, mr1, "a1", "main", true); err != nil || !active {
		t.Errorf("ReviewActiveForRerun(multi review) = %v, %v; want true", active, err)
	}
	if active, err := s.ReviewActiveForStart(ctx, mr1, "a1", "main"); err != nil || !active {
		t.Errorf("ReviewActiveForStart(multi review) = %v, %v; want true", active, err)
	}
	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "a1", "main", TriggerRerun, nil, nil, true, false)
	if err != nil || id != multi || created {
		t.Fatalf("EnqueueRerun(multi review) = %s, %v, %v; want %s, false", id, created, err, multi)
	}
	if r := getRun(t, s, multi); r.State != Waiting || r.SkipReason != WaitReviewers {
		t.Errorf("run = %s %q; want still waiting for its members", r.State, r.SkipReason)
	}

	if active, err := s.ReviewActiveForRerun(ctx, mr2, "b1", "main", true); err != nil || active {
		t.Errorf("ReviewActiveForRerun(held) = %v, %v; want false", active, err)
	}
	if active, err := s.ReviewActiveForStart(ctx, mr2, "b1", "main"); err != nil || active {
		t.Errorf("ReviewActiveForStart(held) = %v, %v; want false", active, err)
	}
	id, created, err = s.EnqueueRerun(ctx, mr2, "acme/widgets", "b1", "main", TriggerRerun, nil, nil, true, false)
	if err != nil || id == held || !created {
		t.Fatalf("EnqueueRerun(held) = %s, %v, %v; want a new run", id, created, err)
	}
	if r := getRun(t, s, held); r.State != Superseded {
		t.Errorf("held run = %s; want superseded by the re-run", r.State)
	}
}

// The changes waiting for a start leave out a multi review waiting for its
// members.
func TestWaitingChangesHidesMultiReview(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	heldRun(t, s, mr2, "b1", WaitUntrusted)
	got, err := s.WaitingChanges(ctx, mr1.Instance, "acme/widgets")
	if err != nil || len(got) != 1 || got[0].IID != mr2.IID || got[0].Reason != WaitUntrusted {
		t.Errorf("WaitingChanges = %+v, %v; want only !%d", got, err, mr2.IID)
	}
}

// The dashboard reads a multi review waiting for its members as running, and
// a held run as waiting. A run held for a model limit still reads as waiting.
func TestDashboardOutcomeMultiReviewRunning(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	multi := multiRun(t, s, mr1, "a1", multiSplit(2, nil))
	held := heldRun(t, s, mr2, "b1", WaitUntrusted)
	limited := heldRun(t, s, mr3, "c1", WaitLimit)
	for id, want := range map[string]string{multi: "running", held: "waiting", limited: "waiting"} {
		d, found, err := s.RunDigest(ctx, id)
		if err != nil || !found || d.State != Waiting || d.Outcome != want {
			t.Errorf("RunDigest(%s) = %s %q, %v, %v; want waiting, %q", id, d.State, d.Outcome, found, err, want)
		}
	}
	for _, c := range []struct {
		outcome string
		want    []string
	}{{"running", []string{multi}}, {"waiting", []string{limited, held}}} {
		ds, err := s.ReviewDigests(ctx, ReviewFilter{Visible: Visibility{All: true}, Outcome: c.outcome, Limit: 10})
		var ids []string
		for _, d := range ds {
			ids = append(ids, d.ID)
		}
		slices.Sort(ids)
		want := slices.Clone(c.want)
		slices.Sort(want)
		if err != nil || !slices.Equal(ids, want) {
			t.Errorf("ReviewDigests(%s) = %v, %v; want %v", c.outcome, ids, err, want)
		}
	}
}

// RequeueLegacyPanels queues a run that waits on the old panel reason again,
// keeps its dispatched_at, supersedes its live panel members and returns their
// Job names. A multi review, and a held run, stay as they are.
func TestRequeueLegacyPanels(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	legacy := legacyPanelRun(t, s, mr1, "a1")
	a := legacyMemberOf(t, s, legacy, "a")
	b := legacyMemberOf(t, s, legacy, "b")
	runLegacyMember(t, s, a.ID, "ta")
	doneLegacyMember(t, s, b.ID, `{"status":"completed"}`)
	multi := multiRun(t, s, mr2, "b1", multiSplit(2, nil))
	first := reviewerOf(t, s, multi, 1)
	held := heldRun(t, s, mr3, "c1", WaitUntrusted)
	limited := heldRun(t, s, mrLimit, "d1", WaitLimit)

	chains, members, err := s.LegacyPanel(ctx)
	if err != nil || chains != 0 || members != 2 {
		t.Errorf("LegacyPanel = %d, %d, %v; want 0, 2", chains, members, err)
	}

	before := getRun(t, s, legacy)
	runs, jobs, err := s.RequeueLegacyPanels(ctx)
	if err != nil || runs != 1 || !slices.Equal(jobs, []string{"job-ta"}) {
		t.Fatalf("RequeueLegacyPanels = %d, %v, %v; want 1, [job-ta]", runs, jobs, err)
	}
	r := getRun(t, s, legacy)
	if r.State != Queued || r.SkipReason != "" || !r.NotBefore.IsZero() || !r.DispatchedAt.Equal(before.DispatchedAt) || r.DispatchedAt.IsZero() {
		t.Errorf("legacy run = %s %q not_before %v dispatched %v; want queued, dispatched_at kept %v",
			r.State, r.SkipReason, r.NotBefore, r.DispatchedAt, before.DispatchedAt)
	}
	for _, m := range legacyMembers(t, s, legacy) {
		if m.State == string(MemberQueued) || m.State == string(MemberRunning) {
			t.Errorf("panel member %s %s is still live", m.Role, m.State)
		}
	}
	if r := getRun(t, s, multi); r.State != Waiting || r.SkipReason != WaitReviewers {
		t.Errorf("multi review run = %s %q; want untouched", r.State, r.SkipReason)
	}
	if got := reviewerOf(t, s, multi, 1); got.State != first.State {
		t.Errorf("reviewer 1 = %s; want %s, untouched", got.State, first.State)
	}
	if r := getRun(t, s, held); r.State != Waiting || r.SkipReason != WaitUntrusted {
		t.Errorf("held run = %s %q; want untouched", r.State, r.SkipReason)
	}

	if r := getRun(t, s, limited); r.State != Waiting || r.SkipReason != WaitLimit {
		t.Errorf("model limit run = %s %q; want untouched, waiting for the limit", r.State, r.SkipReason)
	}

	// A second call finds nothing.
	runs, jobs, err = s.RequeueLegacyPanels(ctx)
	if err != nil || runs != 0 || len(jobs) != 0 {
		t.Errorf("second RequeueLegacyPanels = %d, %v, %v; want 0", runs, jobs, err)
	}
}

// mrLimit is a merge request of its own for the run held at a model limit.
var mrLimit = MRKey{Instance: "https://gitlab.example.com", ProjectID: 7, IID: 99}

// LegacyPanel counts a chain B that has entries, and none that has not.
func TestLegacyPanelCountsChainsWithEntries(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	var id int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO panel_chains (scope, updated_at) VALUES ('global', now()) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if c, m, err := s.LegacyPanel(ctx); err != nil || c != 0 || m != 0 {
		t.Errorf("chain with no entry: LegacyPanel = %d, %d, %v; want 0, 0", c, m, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO panel_chain_entries (chain_id, position, source, model_id) VALUES ($1, 1, 'global', 'x')`, id); err != nil {
		t.Fatal(err)
	}
	if c, m, err := s.LegacyPanel(ctx); err != nil || c != 1 || m != 0 {
		t.Errorf("chain with an entry: LegacyPanel = %d, %d, %v; want 1, 0", c, m, err)
	}
}
