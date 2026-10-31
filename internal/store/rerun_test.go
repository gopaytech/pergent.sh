package store

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// A re-run of a head whose review was published queues a new run: the
// published one is superseded and, being finished and posted, cleaned up.
func TestEnqueueRerunSameHead(t *testing.T) {
	s := newStore(t)
	old := published(t, s, mr1, "aaa", "main")
	base := old

	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerunFailed, []string{"security", "style"}, &base)
	if err != nil || !created || id == old {
		t.Fatalf("EnqueueRerun = %s, %v, %v; want a new run", id, created, err)
	}
	if r := getRun(t, s, old); r.State != Superseded || !r.CleanedUp {
		t.Errorf("old run = %s cleaned_up=%v; want superseded and cleaned up", r.State, r.CleanedUp)
	}
	r := getRun(t, s, id)
	if r.State != Queued || r.HeadSHA != "aaa" || r.TargetBranch != "main" || r.ProjectPath != "acme/widgets" {
		t.Errorf("new run = %+v", r)
	}
	if r.Trigger != TriggerRerunFailed || !slices.Equal(r.SkillSubset, []string{"security", "style"}) ||
		r.BaseRunID == nil || *r.BaseRunID != old {
		t.Errorf("trigger, subset, base = %q %v %v", r.Trigger, r.SkillSubset, r.BaseRunID)
	}
}

// A run from a push has the push trigger, every skill and no base run.
func TestEnqueueRunDefaults(t *testing.T) {
	s := newStore(t)
	r := getRun(t, s, enqueue(t, s, mr1, "aaa"))
	if r.Trigger != TriggerPush || r.SkillSubset != nil || r.BaseRunID != nil {
		t.Errorf("run = %q %v %v; want push, all skills, no base", r.Trigger, r.SkillSubset, r.BaseRunID)
	}
}

// An empty skill list means every skill, like nil.
func TestEnqueueRerunEmptySubsetIsAllSkills(t *testing.T) {
	s := newStore(t)
	published(t, s, mr1, "aaa", "main")
	id, _, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, []string{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, id); r.SkillSubset != nil || r.Trigger != TriggerRerun {
		t.Errorf("run = %q %v", r.Trigger, r.SkillSubset)
	}
}

// A head and target already queued or running is not queued twice, and
// nothing changes.
func TestEnqueueRerunDeduplicates(t *testing.T) {
	s := newStore(t)
	queued := enqueue(t, s, mr1, "aaa")
	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil)
	if err != nil || created || id != queued {
		t.Fatalf("EnqueueRerun of a queued head = %s, %v, %v; want %s, false", id, created, err, queued)
	}

	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	id, created, err = s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil)
	if err != nil || created || id != queued {
		t.Fatalf("EnqueueRerun of a running head = %s, %v, %v; want %s, false", id, created, err, queued)
	}
	if r := getRun(t, s, queued); r.State != Running || r.Trigger != TriggerPush {
		t.Errorf("run changed: %s %s", r.State, r.Trigger)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&n); err != nil || n != 1 {
		t.Errorf("runs = %d, %v; want 1", n, err)
	}
}

// A review reported and not yet published is in flight: a re-run neither
// replaces it nor queues a second one.
func TestEnqueueRerunDeduplicatesReported(t *testing.T) {
	s := newStore(t)
	rep := reported(t, s, mr1, "aaa", `{"status":"completed"}`)
	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil)
	if err != nil || created || id != rep {
		t.Fatalf("EnqueueRerun of a reported head = %s, %v, %v; want %s, false", id, created, err, rep)
	}
	if r := getRun(t, s, rep); r.State != Reported || r.CleanedUp {
		t.Errorf("reported run changed: %s cleaned_up=%v", r.State, r.CleanedUp)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&n); err != nil || n != 1 {
		t.Errorf("runs = %d, %v; want 1", n, err)
	}
}

// A review that failed to publish is finished: a re-run supersedes it, marked
// cleaned up, and queues a new run.
func TestEnqueueRerunSupersedesPublishFailed(t *testing.T) {
	s := newStore(t)
	old := published(t, s, mr1, "aaa", "main")
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'publish_failed' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil)
	if err != nil || !created || id == old {
		t.Fatalf("EnqueueRerun = %s, %v, %v; want a new run", id, created, err)
	}
	if r := getRun(t, s, old); r.State != Superseded || !r.CleanedUp {
		t.Errorf("old run = %s cleaned_up=%v; want superseded and cleaned up", r.State, r.CleanedUp)
	}
	if getRun(t, s, id).State != Queued {
		t.Errorf("new run = %s, want queued", getRun(t, s, id).State)
	}
}

// A re-run of another head or target supersedes the live runs of the
// merge request like a push does; other merge requests are left alone.
func TestEnqueueRerunSupersedesOtherLiveRuns(t *testing.T) {
	s := newStore(t)
	live := enqueue(t, s, mr1, "bbb")
	other := enqueue(t, s, mr2, "bbb")
	published(t, s, mr1, "aaa", "main") // supersedes bbb above (it was live)
	if getRun(t, s, live).State != Superseded {
		t.Fatal("setup: bbb should be superseded")
	}
	live = enqueue(t, s, mr1, "ccc")
	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}
	if getRun(t, s, live).State != Superseded {
		t.Errorf("live run of another head = %s, want superseded", getRun(t, s, live).State)
	}
	if getRun(t, s, id).State != Queued {
		t.Errorf("new run = %s, want queued", getRun(t, s, id).State)
	}
	if getRun(t, s, other).State == Superseded {
		t.Errorf("the other merge request's run was superseded")
	}
}

func TestCountReruns(t *testing.T) {
	s := newStore(t)
	other := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 3} // another repository
	// rerun asks for a re-run and then publishes it, so the next one is
	// not a duplicate of a run still in flight.
	rerun := func(mr MRKey, head string, trigger string) {
		t.Helper()
		id, created, err := s.EnqueueRerun(ctx, mr, "acme/widgets", head, "main", trigger, nil, nil)
		if err != nil || !created {
			t.Fatalf("EnqueueRerun(%v, %s) = %v, %v", mr, head, created, err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'published' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	// mr1: a push run and two re-runs (the first is superseded by the
	// second: superseded runs still count). mr2: one re-run. other: one.
	published(t, s, mr1, "aaa", "main")
	rerun(mr1, "aaa", TriggerRerun)
	rerun(mr1, "aaa", TriggerRerunFailed)
	published(t, s, mr2, "ooo", "main")
	rerun(mr2, "ooo", TriggerRerun)
	rerun(other, "zzz", TriggerRerun)

	before := time.Now().Add(-time.Hour)
	for _, c := range []struct {
		name string
		mr   *MRKey
		want int
	}{
		{"one merge request", &mr1, 2},
		{"another merge request", &mr2, 1},
		{"the whole repository", nil, 3},
		{"a merge request without re-runs", &MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 99}, 0},
	} {
		got, err := s.CountReruns(ctx, c.mr, mr1.Instance, mr1.ProjectID, before)
		if err != nil || got != c.want {
			t.Errorf("%s: CountReruns = %d, %v; want %d", c.name, got, err, c.want)
		}
	}
	if got, err := s.CountReruns(ctx, nil, "https://other.example.com", mr1.ProjectID, before); err != nil || got != 0 {
		t.Errorf("another instance: CountReruns = %d, %v; want 0", got, err)
	}

	// since is respected: nothing was created after now.
	if got, err := s.CountReruns(ctx, nil, mr1.Instance, mr1.ProjectID, time.Now().Add(time.Hour)); err != nil || got != 0 {
		t.Errorf("since in the future: CountReruns = %d, %v; want 0", got, err)
	}
	// And a run created long ago is not counted.
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET created_at = $1 WHERE trigger = 'rerun' AND mr_iid = $2`,
		time.Now().Add(-48*time.Hour), mr2.IID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CountReruns(ctx, nil, mr1.Instance, mr1.ProjectID, time.Now().Add(-24*time.Hour)); err != nil || got != 2 {
		t.Errorf("repository since yesterday: CountReruns = %d, %v; want 2", got, err)
	}
}

// Two re-runs asked for at once queue one run.
func TestEnqueueRerunConcurrent(t *testing.T) {
	s := newStore(t)
	published(t, s, mr1, "aaa", "main")
	const callers = 6
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     = map[string]bool{}
		created int
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, ok, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[id] = true
			if ok {
				created++
			}
		}()
	}
	wg.Wait()
	if created != 1 || len(ids) != 1 {
		t.Errorf("created %d runs, saw ids %v; want one run", created, ids)
	}
}

func TestStoreMergedResult(t *testing.T) {
	s := newStore(t)
	id := reported(t, s, mr1, "aaa", `{"status":"completed","n":1}`)

	if err := s.StoreMergedResult(ctx, id, []byte(`{"status":"completed","n":2}`)); err != nil {
		t.Fatalf("StoreMergedResult: %v", err)
	}
	r := getRun(t, s, id)
	if string(r.Result) != `{"n": 2, "status": "completed"}` || r.State != Reported {
		t.Errorf("run = %s, %s; want the merged result, still reported", r.State, r.Result)
	}

	// Only a reported run takes a merged result; nothing else is touched.
	if err := s.MarkPublished(ctx, id, RunDecision{}); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMergedResult(ctx, id, []byte(`{"n":3}`)); err == nil {
		t.Error("a published run took a merged result")
	}
	if err := s.StoreMergedResult(ctx, "not-an-id", []byte(`{}`)); err == nil {
		t.Error("a malformed id was accepted")
	}
	if err := s.StoreMergedResult(ctx, id, []byte(`nope`)); err == nil {
		t.Error("invalid JSON was accepted")
	}
}

func TestMakeFullRerun(t *testing.T) {
	s := newStore(t)
	old := published(t, s, mr1, "aaa", "main")
	id, _, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerunFailed, []string{"gone"}, &old)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.MakeFullRerun(ctx, id); err != nil {
		t.Fatalf("MakeFullRerun: %v", err)
	}
	if r := getRun(t, s, id); r.Trigger != TriggerRerun || r.SkillSubset != nil || r.BaseRunID != nil {
		t.Errorf("run = %q %v %v; want rerun, all skills, no base", r.Trigger, r.SkillSubset, r.BaseRunID)
	}
	if err := s.MakeFullRerun(ctx, "not-an-id"); err == nil {
		t.Error("a malformed id was accepted")
	}
}

// Only a run being dispatched is turned into a full re-run or has its subset
// narrowed; a reported, published or superseded run is left as it is.
func TestRerunEditsNeedARunBeingDispatched(t *testing.T) {
	s := newStore(t)
	old := published(t, s, mr1, "aaa", "main")
	id, _, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerunFailed, []string{"a", "b"}, &old)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSkillSubset(ctx, id, []string{"a"}); err != nil {
		t.Fatalf("SetSkillSubset of a queued run: %v", err)
	}
	if r := getRun(t, s, id); !slices.Equal(r.SkillSubset, []string{"a"}) {
		t.Errorf("subset = %v", r.SkillSubset)
	}

	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	tok := "tok-rerun"
	if err := s.MarkDispatched(ctx, id, "job-"+id, "base", "start", hash(tok), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, tok, []byte(`{"status":"completed"}`), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	for name, run := range map[string]string{"reported": id, "superseded": old} {
		if err := s.MakeFullRerun(ctx, run); err == nil {
			t.Errorf("%s run: MakeFullRerun succeeded", name)
		}
		if err := s.SetSkillSubset(ctx, run, []string{"z"}); err == nil {
			t.Errorf("%s run: SetSkillSubset succeeded", name)
		}
	}
	if r := getRun(t, s, id); r.Trigger != TriggerRerunFailed || !slices.Equal(r.SkillSubset, []string{"a"}) || r.BaseRunID == nil {
		t.Errorf("reported run changed: %q %v %v", r.Trigger, r.SkillSubset, r.BaseRunID)
	}
	if r := getRun(t, s, old); r.Trigger != TriggerPush {
		t.Errorf("old run changed: %q", r.Trigger)
	}
}

// The review a command refers to is the newest published one, or one
// published before and waiting to be decided again; a first review still
// waiting to be published, and superseded ones, are not it.
func TestLatestDecidedRun(t *testing.T) {
	s := newStore(t)
	if _, found, err := s.LatestDecidedRun(ctx, mr1); err != nil || found {
		t.Fatalf("LatestDecidedRun with no run = %v, %v", found, err)
	}

	first := published(t, s, mr1, "aaa", "main")
	got, found, err := s.LatestDecidedRun(ctx, mr1)
	if err != nil || !found || got.ID != first {
		t.Fatalf("LatestDecidedRun = %s, %v, %v; want %s", got.ID, found, err, first)
	}
	if _, found, _ := s.LatestDecidedRun(ctx, mr2); found {
		t.Error("another merge request's run was returned")
	}

	// Decided again (a dismissal): reported, but published before.
	if ok, err := s.Republish(ctx, mr1, "aaa", "main"); err != nil || !ok {
		t.Fatalf("Republish = %v, %v", ok, err)
	}
	// Published with no outcome in this test, so nothing was decided yet.
	if _, found, _ := s.LatestDecidedRun(ctx, mr1); found {
		t.Error("a reported run that was never decided counted as decided")
	}
	if err := s.MarkPublished(ctx, first, RunDecision{Outcome: OutcomeNoBlocking}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Republish(ctx, mr1, "aaa", "main"); err != nil || !ok {
		t.Fatalf("Republish = %v, %v", ok, err)
	}
	if got, found, _ := s.LatestDecidedRun(ctx, mr1); !found || got.ID != first || got.State != Reported {
		t.Errorf("a run to be decided again = %s %s %v; want %s reported", got.ID, got.State, found, first)
	}

	// A push supersedes it; the new run is not decided until published.
	enqueue(t, s, mr1, "bbb")
	if got, found, _ := s.LatestDecidedRun(ctx, mr1); found {
		t.Errorf("LatestDecidedRun = %s after a push; want none", got.ID)
	}
}
