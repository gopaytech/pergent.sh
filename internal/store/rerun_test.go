package store

import (
	"reflect"
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

	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerunFailed, []string{"security", "style"}, &base, false)
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
	id, _, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, []string{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, id); r.SkillSubset != nil || r.Trigger != TriggerRerun {
		t.Errorf("run = %q %v", r.Trigger, r.SkillSubset)
	}
}

// A head and target already queued or running is not queued twice, and
// nothing changes. The run is a re-run, which even a full review (replace)
// doesn't supersede: only a push's run can be replaced.
func TestEnqueueRerunDeduplicates(t *testing.T) {
	s := newStore(t)
	queued, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil, false)
	if err != nil || !created {
		t.Fatalf("setup EnqueueRerun = %s, %v, %v", queued, created, err)
	}
	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil, true)
	if err != nil || created || id != queued {
		t.Fatalf("EnqueueRerun of a queued head = %s, %v, %v; want %s, false", id, created, err, queued)
	}

	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	id, created, err = s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil, true)
	if err != nil || created || id != queued {
		t.Fatalf("EnqueueRerun of a running head = %s, %v, %v; want %s, false", id, created, err, queued)
	}
	if r := getRun(t, s, queued); r.State != Running || r.Trigger != TriggerRerun {
		t.Errorf("run changed: %s %s", r.State, r.Trigger)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&n); err != nil || n != 1 {
		t.Errorf("runs = %d, %v; want 1", n, err)
	}
}

// A full review (replace) supersedes a push's run that hasn't been dispatched
// (queued, or claimed and not yet dispatched) or that is incremental, whatever
// its state; it leaves a dispatched run that reviews everything, and a run that
// isn't from a push, alone. Without replace (a re-run of the failed skills,
// including its whole-review path) no live run is replaced.
func TestEnqueueRerunReplacesIncrementalRun(t *testing.T) {
	const (
		claimed = RunState("claimed") // running, but not yet dispatched
	)
	// live brings a push run of mr1 at "aaa" to the state.
	live := func(t *testing.T, s *Store, state RunState, incremental bool) string {
		t.Helper()
		base := published(t, s, mr1, "base", "main")
		var id string
		switch state {
		case Queued:
			id = enqueue(t, s, mr1, "aaa")
		case claimed:
			id = enqueue(t, s, mr1, "aaa")
			if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
				t.Fatal(err)
			}
		case Running:
			id = dispatched(t, s, mr1, "aaa", "tok")
		default:
			id = reported(t, s, mr1, "aaa", `{"status":"completed"}`)
		}
		if incremental {
			if state == Reported {
				// SetIncremental only edits a run being dispatched.
				if _, err := s.pool.Exec(ctx, `UPDATE runs SET incremental = true WHERE id = $1`, id); err != nil {
					t.Fatal(err)
				}
			} else if err := s.SetIncremental(ctx, id, &base); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}

	for _, tc := range []struct {
		name        string
		state       RunState
		incremental bool
		trigger     string
		replace     bool
		replaced    bool
	}{
		{"queued push run", Queued, false, TriggerRerun, true, true},
		{"claimed push run", claimed, false, TriggerRerun, true, true},
		{"queued incremental run", Queued, true, TriggerRerun, true, true},
		{"running incremental run", Running, true, TriggerRerun, true, true},
		{"reported incremental run", Reported, true, TriggerRerun, true, true},
		{"running full run", Running, false, TriggerRerun, true, false},
		{"reported full run", Reported, false, TriggerRerun, true, false},
		{"queued run, failed skills only", Queued, false, TriggerRerunFailed, false, false},
		{"running incremental run, failed skills only", Running, true, TriggerRerunFailed, false, false},
		{"queued run, failed whole review", Queued, false, TriggerRerun, false, false},
		{"claimed run, failed whole review", claimed, false, TriggerRerun, false, false},
		{"running incremental run, failed whole review", Running, true, TriggerRerun, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			old := live(t, s, tc.state, tc.incremental)
			id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", tc.trigger, nil, nil, tc.replace)
			if err != nil || created != tc.replaced {
				t.Fatalf("EnqueueRerun = %s, %v, %v; want created %v", id, created, err, tc.replaced)
			}
			if tc.replaced {
				if id == old || getRun(t, s, old).State != Superseded || getRun(t, s, id).State != Queued {
					t.Errorf("old run %s, new run %s (%s); want the old superseded and a queued one", getRun(t, s, old).State, id, getRun(t, s, id).State)
				}
				return
			}
			want := tc.state
			if want == claimed {
				want = Running
			}
			if id != old || getRun(t, s, old).State != want {
				t.Errorf("id = %s (%s), want the live run %s still %s", id, getRun(t, s, old).State, old, want)
			}
		})
	}
}

// SetIncremental marks a run being dispatched as incremental on a base run, or
// clears that; any other run is an error.
func TestSetIncremental(t *testing.T) {
	s := newStore(t)
	base := published(t, s, mr1, "aaa", "main")
	id := enqueue(t, s, mr1, "bbb")

	if err := s.SetIncremental(ctx, id, &base); err != nil {
		t.Fatalf("SetIncremental of a queued run: %v", err)
	}
	if r := getRun(t, s, id); !r.Incremental || r.BaseRunID == nil || *r.BaseRunID != base {
		t.Errorf("run = incremental %v base %v; want true, %s", r.Incremental, r.BaseRunID, base)
	}

	if err := s.SetIncremental(ctx, id, nil); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if r := getRun(t, s, id); r.Incremental || r.BaseRunID != nil {
		t.Errorf("cleared run = incremental %v base %v; want false, nil", r.Incremental, r.BaseRunID)
	}

	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	tok := "tok-incremental"
	if err := s.MarkDispatched(ctx, id, "job-"+id, "base", "start", hash(tok), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIncremental(ctx, id, &base); err != nil {
		t.Errorf("SetIncremental of a running run: %v", err)
	}
	if out, err := s.StoreResult(ctx, id, tok, []byte(`{"status":"completed"}`), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := s.SetIncremental(ctx, id, nil); err == nil {
		t.Error("SetIncremental of a reported run succeeded")
	}
	if r := getRun(t, s, id); !r.Incremental {
		t.Error("the reported run changed")
	}
	if err := s.SetIncremental(ctx, "bad", nil); err == nil {
		t.Error("a malformed id was accepted")
	}
}

// NewerRunOfHead is true for a run of the same merge request, head and target
// created after the run and not superseded.
func TestNewerRunOfHead(t *testing.T) {
	s := newStore(t)
	old := dispatched(t, s, mr1, "aaa", "tok")
	if newer, err := s.NewerRunOfHead(ctx, getRun(t, s, old)); err != nil || newer {
		t.Fatalf("alone = %v, %v; want false", newer, err)
	}
	// Superseded by a push of another head: nothing newer is of this head.
	other := enqueue(t, s, mr1, "bbb")
	if newer, err := s.NewerRunOfHead(ctx, getRun(t, s, old)); err != nil || newer {
		t.Errorf("newer run of another head = %v, %v; want false", newer, err)
	}
	if newer, err := s.NewerRunOfHead(ctx, getRun(t, s, other)); err != nil || newer {
		t.Errorf("the newest run = %v, %v; want false", newer, err)
	}

	// A full review of the same head, replacing the first.
	s2 := newStore(t)
	base := published(t, s2, mr1, "base", "main")
	first := dispatched(t, s2, mr1, "aaa", "tok")
	if err := s2.SetIncremental(ctx, first, &base); err != nil {
		t.Fatal(err)
	}
	full, created, err := s2.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil, true)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}
	if newer, err := s2.NewerRunOfHead(ctx, getRun(t, s2, first)); err != nil || !newer {
		t.Errorf("the replaced run = %v, %v; want true", newer, err)
	}
	if newer, err := s2.NewerRunOfHead(ctx, getRun(t, s2, full)); err != nil || newer {
		t.Errorf("the full review = %v, %v; want false", newer, err)
	}
	// Once the full review is itself superseded it no longer counts.
	if _, err := s2.pool.Exec(ctx, `UPDATE runs SET state = 'superseded' WHERE id = $1`, full); err != nil {
		t.Fatal(err)
	}
	if newer, err := s2.NewerRunOfHead(ctx, getRun(t, s2, first)); err != nil || newer {
		t.Errorf("after the full review was superseded = %v, %v; want false", newer, err)
	}
}

// A review reported and not yet published is in flight: a re-run neither
// replaces it nor queues a second one.
func TestEnqueueRerunDeduplicatesReported(t *testing.T) {
	s := newStore(t)
	rep := reported(t, s, mr1, "aaa", `{"status":"completed"}`)
	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil, false)
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
	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil, false)
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
	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil, false)
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
		id, created, err := s.EnqueueRerun(ctx, mr, "acme/widgets", head, "main", trigger, nil, nil, false)
		if err != nil || !created {
			t.Fatalf("EnqueueRerun(%v, %s, false) = %v, %v", mr, head, created, err)
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

	// The runs were stamped with the database's clock: put them at t0, and
	// count against t0 alone, so this process's clock doesn't matter.
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET created_at = $1`, t0); err != nil {
		t.Fatal(err)
	}
	before := t0.Add(-time.Hour)
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

	// since is respected: nothing was created after t0.
	if got, err := s.CountReruns(ctx, nil, mr1.Instance, mr1.ProjectID, t0.Add(time.Hour)); err != nil || got != 0 {
		t.Errorf("since in the future: CountReruns = %d, %v; want 0", got, err)
	}
	// And a run created long ago is not counted.
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET created_at = $1 WHERE trigger = 'rerun' AND mr_iid = $2`,
		t0.Add(-48*time.Hour), mr2.IID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CountReruns(ctx, nil, mr1.Instance, mr1.ProjectID, t0.Add(-24*time.Hour)); err != nil || got != 2 {
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
			id, ok, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerun, nil, nil, false)
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
	id, _, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerunFailed, []string{"gone"}, &old, false)
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
	id, _, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "aaa", "main", TriggerRerunFailed, []string{"a", "b"}, &old, false)
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

// A run that never had SetRunQuiet called (one from before the column, or one
// that never started) has a NULL quiet_reviews. It reads as off through every
// path that scans a run.
func TestRunQuietNullReadsOff(t *testing.T) {
	s := newStore(t)
	queued := enqueue(t, s, mr1, "aaa")
	if r, found, err := s.Run(ctx, queued); err != nil || !found || r.Quiet {
		t.Fatalf("Run of a queued run = quiet %v, found %v, %v; want false, true, nil", r.Quiet, found, err)
	}
	claimed, err := s.ClaimQueued(ctx, 10, t0)
	if err != nil || len(claimed) != 1 || claimed[0].ID != queued || claimed[0].Quiet {
		t.Fatalf("ClaimQueued = %+v, %v; want the run, not quiet", claimed, err)
	}
	rep := reported(t, s, mr2, "bbb", `{"status":"completed"}`)
	toPublish, err := s.RunsToPublish(ctx, t0.Add(time.Hour), 10)
	if err != nil || len(toPublish) != 1 || toPublish[0].ID != rep || toPublish[0].Quiet {
		t.Fatalf("RunsToPublish = %+v, %v; want the reported run, not quiet", toPublish, err)
	}
}

// SetRunQuiet records whether a run being dispatched (queued or running) is
// quiet, and reads back each way; any other run is an error.
func TestSetRunQuiet(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "aaa")

	for _, quiet := range []bool{true, false, true} {
		if err := s.SetRunQuiet(ctx, id, quiet); err != nil {
			t.Fatalf("SetRunQuiet(%v) of a queued run: %v", quiet, err)
		}
		if r := getRun(t, s, id); r.Quiet != quiet {
			t.Errorf("queued run: Quiet = %v, want %v", r.Quiet, quiet)
		}
	}

	claimed, err := s.ClaimQueued(ctx, 10, t0)
	if err != nil || len(claimed) != 1 || !claimed[0].Quiet {
		t.Fatalf("ClaimQueued = %+v, %v; want the run, quiet", claimed, err)
	}
	if err := s.SetRunQuiet(ctx, id, false); err != nil {
		t.Fatalf("SetRunQuiet of a running run: %v", err)
	}
	if r := getRun(t, s, id); r.Quiet {
		t.Error("running run: Quiet = true after SetRunQuiet(false)")
	}
	if err := s.SetRunQuiet(ctx, id, true); err != nil {
		t.Fatal(err)
	}

	tok := "tok-quiet"
	if err := s.MarkDispatched(ctx, id, "job-"+id, "base", "start", hash(tok), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, tok, []byte(`{"status":"completed"}`), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := s.SetRunQuiet(ctx, id, false); err == nil {
		t.Error("SetRunQuiet of a reported run succeeded")
	}
	if r := getRun(t, s, id); !r.Quiet {
		t.Error("the reported run changed")
	}
	if err := s.SetRunQuiet(ctx, "bad", true); err == nil {
		t.Error("a malformed id was accepted")
	}
}

// A run that never had SetRunLayout called has a NULL comment_layout. It
// reads as nil (the default layout) through every path that scans a run.
func TestRunLayoutNullReadsNil(t *testing.T) {
	s := newStore(t)
	queued := enqueue(t, s, mr1, "aaa")
	if r, found, err := s.Run(ctx, queued); err != nil || !found || r.CommentLayout != nil {
		t.Fatalf("Run of a queued run = layout %#v, found %v, %v; want nil, true, nil", r.CommentLayout, found, err)
	}
	claimed, err := s.ClaimQueued(ctx, 10, t0)
	if err != nil || len(claimed) != 1 || claimed[0].ID != queued || claimed[0].CommentLayout != nil {
		t.Fatalf("ClaimQueued = %+v, %v; want the run, no layout", claimed, err)
	}
	rep := reported(t, s, mr2, "bbb", `{"status":"completed"}`)
	toPublish, err := s.RunsToPublish(ctx, t0.Add(time.Hour), 10)
	if err != nil || len(toPublish) != 1 || toPublish[0].ID != rep || toPublish[0].CommentLayout != nil {
		t.Fatalf("RunsToPublish = %+v, %v; want the reported run, no layout", toPublish, err)
	}
}

// SetRunLayout records the layout of a run being dispatched (queued or
// running), in order, and reads back through every path that scans a run; an
// empty list clears it; any other run is an error.
func TestSetRunLayout(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "aaa")
	a := []string{"summary:open", "findings:collapsed", "checks:open"}
	b := []string{"findings:open", "summary:collapsed"}

	for _, layout := range [][]string{a, b, nil, a} {
		if err := s.SetRunLayout(ctx, id, layout); err != nil {
			t.Fatalf("SetRunLayout(%v) of a queued run: %v", layout, err)
		}
		got := getRun(t, s, id).CommentLayout
		if !slices.Equal(got, layout) || (layout == nil) != (got == nil) {
			t.Errorf("queued run: CommentLayout = %#v, want %#v", got, layout)
		}
	}

	claimed, err := s.ClaimQueued(ctx, 10, t0)
	if err != nil || len(claimed) != 1 || !slices.Equal(claimed[0].CommentLayout, a) {
		t.Fatalf("ClaimQueued = %+v, %v; want the run with layout %v", claimed, err, a)
	}
	if err := s.SetRunLayout(ctx, id, b); err != nil {
		t.Fatalf("SetRunLayout of a running run: %v", err)
	}
	if got := getRun(t, s, id).CommentLayout; !slices.Equal(got, b) {
		t.Errorf("running run: CommentLayout = %v, want %v", got, b)
	}
	if err := s.SetRunLayout(ctx, id, []string{}); err != nil {
		t.Fatal(err)
	}
	if got := getRun(t, s, id).CommentLayout; got != nil {
		t.Errorf("an empty list: CommentLayout = %#v, want nil", got)
	}
	if err := s.SetRunLayout(ctx, id, a); err != nil {
		t.Fatal(err)
	}

	tok := "tok-layout"
	if err := s.MarkDispatched(ctx, id, "job-"+id, "base", "start", hash(tok), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, tok, []byte(`{"status":"completed"}`), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := s.SetRunLayout(ctx, id, b); err == nil {
		t.Error("SetRunLayout of a reported run succeeded")
	}
	if got := getRun(t, s, id).CommentLayout; !slices.Equal(got, a) {
		t.Errorf("the reported run changed: CommentLayout = %v, want %v", got, a)
	}
	if err := s.SetRunLayout(ctx, "bad", a); err == nil {
		t.Error("a malformed id was accepted")
	}
}

// A run that never had SetRunLinks called has a NULL linked_repos. It reads as
// nil (no links) through every path that scans a run.
func TestRunLinksNullReadsNil(t *testing.T) {
	s := newStore(t)
	queued := enqueue(t, s, mr1, "aaa")
	if r, found, err := s.Run(ctx, queued); err != nil || !found || r.LinkedRepos != nil {
		t.Fatalf("Run of a queued run = links %#v, found %v, %v; want nil, true, nil", r.LinkedRepos, found, err)
	}
	claimed, err := s.ClaimQueued(ctx, 10, t0)
	if err != nil || len(claimed) != 1 || claimed[0].ID != queued || claimed[0].LinkedRepos != nil {
		t.Fatalf("ClaimQueued = %+v, %v; want the run, no links", claimed, err)
	}
	rep := reported(t, s, mr2, "bbb", `{"status":"completed"}`)
	toPublish, err := s.RunsToPublish(ctx, t0.Add(time.Hour), 10)
	if err != nil || len(toPublish) != 1 || toPublish[0].ID != rep || toPublish[0].LinkedRepos != nil {
		t.Fatalf("RunsToPublish = %+v, %v; want the reported run, no links", toPublish, err)
	}
}

// SetRunLinks records the links of a run being dispatched (queued or
// running), in order, and reads back through every path that scans a run; an
// empty list clears it; any other run is an error.
func TestSetRunLinks(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "aaa")
	a := []Link{
		{Target: "acme/pergent.sh", Watched: []string{"render/**"}, CanBlock: false},
		{Target: "acme/docs", CanBlock: true},
	}
	b := []Link{{Target: "acme/api", Watched: []string{"*.proto"}, CanBlock: true}}

	for _, links := range [][]Link{a, b, nil, a} {
		if err := s.SetRunLinks(ctx, id, links); err != nil {
			t.Fatalf("SetRunLinks(%v) of a queued run: %v", links, err)
		}
		got := getRun(t, s, id).LinkedRepos
		if !reflect.DeepEqual(got, links) {
			t.Errorf("queued run: LinkedRepos = %#v, want %#v", got, links)
		}
	}

	claimed, err := s.ClaimQueued(ctx, 10, t0)
	if err != nil || len(claimed) != 1 || !reflect.DeepEqual(claimed[0].LinkedRepos, a) {
		t.Fatalf("ClaimQueued = %+v, %v; want the run with links %v", claimed, err, a)
	}
	if err := s.SetRunLinks(ctx, id, b); err != nil {
		t.Fatalf("SetRunLinks of a running run: %v", err)
	}
	if got := getRun(t, s, id).LinkedRepos; !reflect.DeepEqual(got, b) {
		t.Errorf("running run: LinkedRepos = %v, want %v", got, b)
	}
	if err := s.SetRunLinks(ctx, id, []Link{}); err != nil {
		t.Fatal(err)
	}
	if got := getRun(t, s, id).LinkedRepos; got != nil {
		t.Errorf("an empty list: LinkedRepos = %#v, want nil", got)
	}
	if err := s.SetRunLinks(ctx, id, a); err != nil {
		t.Fatal(err)
	}

	tok := "tok-links"
	if err := s.MarkDispatched(ctx, id, "job-"+id, "base", "start", hash(tok), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, tok, []byte(`{"status":"completed"}`), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := s.SetRunLinks(ctx, id, b); err == nil {
		t.Error("SetRunLinks of a reported run succeeded")
	}
	if got := getRun(t, s, id).LinkedRepos; !reflect.DeepEqual(got, a) {
		t.Errorf("the reported run changed: LinkedRepos = %v, want %v", got, a)
	}
	if err := s.SetRunLinks(ctx, "bad", a); err == nil {
		t.Error("a malformed id was accepted")
	}
}
