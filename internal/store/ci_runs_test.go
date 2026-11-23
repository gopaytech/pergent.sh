package store

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

var ciDeadline = t0.Add(30 * time.Minute)

// A queued push run and a start run (claimed, not dispatched) are held for
// CI; a dispatched run and one CI was already looked at for are not.
func TestHoldForCIHoldsQueuedAndStartedRuns(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	push := enqueue(t, s, mr1, sha('a'))
	hold := func(id string) {
		t.Helper()
		ok, err := s.HoldForCI(ctx, id, ciDeadline)
		if err != nil || !ok {
			t.Fatalf("HoldForCI(%s) = %v, %v; want true", id, ok, err)
		}
		r := mustRun(t, s, id)
		if r.State != Waiting || r.SkipReason != WaitCIPending {
			t.Errorf("run %s = %s/%q, want waiting/%q", id, r.State, r.SkipReason, WaitCIPending)
		}
		if r.CIDeadline == nil || !r.CIDeadline.Equal(ciDeadline) {
			t.Errorf("run %s deadline = %v, want %v", id, r.CIDeadline, ciDeadline)
		}
	}
	hold(push)

	start, created, err := s.EnqueueRerun(ctx, mr2, "acme/widgets", sha('b'), "main", TriggerStart, nil, nil, true, true)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun(start) = %v, %v", created, err)
	}
	claimed, err := s.ClaimQueued(ctx, 10, t0.Add(time.Hour))
	if err != nil || len(claimed) != 1 || claimed[0].ID != start || claimed[0].State != Running {
		t.Fatalf("ClaimQueued = %+v, %v; want the start run, running", claimed, err)
	}
	hold(start)

	// A dispatched run is not held.
	dispatchedID := dispatched(t, s, MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: 50}, sha('c'), "tok-c")
	if ok, err := s.HoldForCI(ctx, dispatchedID, ciDeadline); err != nil || ok {
		t.Errorf("HoldForCI of a dispatched run = %v, %v; want false", ok, err)
	}
	if r := mustRun(t, s, dispatchedID); r.State != Running || r.CIDeadline != nil {
		t.Errorf("dispatched run = %s, deadline %v; want running, none", r.State, r.CIDeadline)
	}

	// A run CI was looked at for (released once) is not held again.
	again := enqueue(t, s, MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: 51}, sha('d'))
	if err := s.MarkCIChecked(ctx, again); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HoldForCI(ctx, again, ciDeadline); err != nil || ok {
		t.Errorf("HoldForCI of a ci_checked run = %v, %v; want false", ok, err)
	}
	if r := mustRun(t, s, again); r.State != Queued || !r.CIChecked {
		t.Errorf("checked run = %s, ci_checked %v; want queued, true", r.State, r.CIChecked)
	}
	// An API review has no merge request and no CI: never held.
	c := newAPIClient(t, s, "fastgateway")
	api := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:ci"))
	if ok, err := s.HoldForCI(ctx, api, ciDeadline); err != nil || ok {
		t.Errorf("HoldForCI of an API review = %v, %v; want false", ok, err)
	}
	if r := mustRun(t, s, api); r.State == Waiting || r.CIDeadline != nil {
		t.Errorf("API review = %s, deadline %v; want not held", r.State, r.CIDeadline)
	}
	if ok, err := s.HoldForCI(ctx, "not-a-uuid", ciDeadline); err == nil || ok {
		t.Errorf("HoldForCI of a malformed id = %v, %v; want an error", ok, err)
	}
}

// ReleaseCI queues a ci_pending run once, and touches nothing else: not an
// untrusted author's waiting run, not a superseded run.
func TestReleaseCIOnlyCIPending(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := enqueue(t, s, mr1, sha('a'))
	if ok, err := s.HoldForCI(ctx, id, ciDeadline); err != nil || !ok {
		t.Fatalf("HoldForCI = %v, %v", ok, err)
	}
	if ok, err := s.ReleaseCI(ctx, id); err != nil || !ok {
		t.Fatalf("ReleaseCI = %v, %v; want true", ok, err)
	}
	if r := mustRun(t, s, id); r.State != Queued || r.SkipReason != "" || !r.CIChecked {
		t.Errorf("released run = %s/%q ci_checked %v; want queued/empty/true", r.State, r.SkipReason, r.CIChecked)
	}
	if ok, err := s.ReleaseCI(ctx, id); err != nil || ok {
		t.Errorf("second ReleaseCI = %v, %v; want false", ok, err)
	}

	untrusted := enqueue(t, s, mr2, sha('b'))
	if ok, err := s.HoldRun(ctx, untrusted, WaitUntrusted); err != nil || !ok {
		t.Fatalf("HoldRun = %v, %v", ok, err)
	}
	if ok, err := s.ReleaseCI(ctx, untrusted); err != nil || ok {
		t.Errorf("ReleaseCI of an untrusted run = %v, %v; want false", ok, err)
	}
	if r := mustRun(t, s, untrusted); r.State != Waiting || r.SkipReason != WaitUntrusted || r.CIChecked {
		t.Errorf("untrusted run = %s/%q ci_checked %v; want waiting/untrusted/false", r.State, r.SkipReason, r.CIChecked)
	}

	// A run superseded while its CI check was in flight stays superseded.
	old := enqueue(t, s, mr1, sha('c'))
	if ok, err := s.HoldForCI(ctx, old, ciDeadline); err != nil || !ok {
		t.Fatalf("HoldForCI = %v, %v", ok, err)
	}
	enqueue(t, s, mr1, sha('d'))
	if r := mustRun(t, s, old); r.State != Superseded {
		t.Fatalf("old run = %s, want superseded", r.State)
	}
	if ok, err := s.ReleaseCI(ctx, old); err != nil || ok {
		t.Errorf("ReleaseCI of a superseded run = %v, %v; want false", ok, err)
	}
	if err := s.SetRunCI(ctx, old, []CIJobResult{{Name: "test", Result: "passed", Read: "status"}}); !errors.Is(err, ErrRunGone) {
		t.Errorf("SetRunCI of a superseded run = %v, want ErrRunGone", err)
	}
	if r := mustRun(t, s, old); r.State != Superseded {
		t.Errorf("old run = %s after release and snapshot, want superseded", r.State)
	}
	if ok, err := s.ReleaseCI(ctx, "not-a-uuid"); err == nil || ok {
		t.Errorf("ReleaseCI of a malformed id = %v, %v; want an error", ok, err)
	}
}

// Starting a ci_pending run marks CI looked at; starting an untrusted
// author's run does not.
func TestStartRunOnCIPendingMarksChecked(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := enqueue(t, s, mr1, sha('a'))
	if ok, err := s.HoldForCI(ctx, id, ciDeadline); err != nil || !ok {
		t.Fatalf("HoldForCI = %v, %v", ok, err)
	}
	if got, started, err := s.StartRun(ctx, mr1, "acme/widgets", sha('a'), "main"); err != nil || !started || got != id {
		t.Fatalf("StartRun = %s, %v, %v; want %s, true", got, started, err, id)
	}
	if r := mustRun(t, s, id); r.State != Queued || r.Trigger != TriggerStart || !r.CIChecked || r.SkipReason != "" {
		t.Errorf("run = %s/%s ci_checked %v reason %q; want queued/start/true/empty", r.State, r.Trigger, r.CIChecked, r.SkipReason)
	}

	untrusted := enqueue(t, s, mr2, sha('b'))
	if ok, err := s.HoldRun(ctx, untrusted, WaitUntrusted); err != nil || !ok {
		t.Fatalf("HoldRun = %v, %v", ok, err)
	}
	if got, started, err := s.StartRun(ctx, mr2, "acme/widgets", sha('b'), "main"); err != nil || !started || got != untrusted {
		t.Fatalf("StartRun = %s, %v, %v", got, started, err)
	}
	if r := mustRun(t, s, untrusted); r.CIChecked {
		t.Error("starting an untrusted author's run marked it ci_checked")
	}
}

// A start while the dispatcher checks a push's run (claimed, not dispatched)
// marks CI looked at, as it does the quota: the CI hold then leaves the run
// alone, and the start skips the wait.
func TestStartRunDuringGateMarksCIChecked(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := enqueue(t, s, mr1, sha('a'))
	if claimed, err := s.ClaimQueued(ctx, 10, t0.Add(time.Hour)); err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimQueued = %+v, %v", claimed, err)
	}
	if got, started, err := s.StartRun(ctx, mr1, "acme/widgets", sha('a'), "main"); err != nil || !started || got != id {
		t.Fatalf("StartRun = %s, %v, %v; want %s, true", got, started, err, id)
	}
	if r := mustRun(t, s, id); r.Trigger != TriggerStart || !r.CIChecked || !r.QuotaChecked {
		t.Errorf("run trigger %s ci_checked %v quota_checked %v; want start/true/true", r.Trigger, r.CIChecked, r.QuotaChecked)
	}
	if ok, err := s.HoldForCI(ctx, id, ciDeadline); err != nil || ok {
		t.Errorf("HoldForCI of a run started during the gate = %v, %v; want false", ok, err)
	}
}

func TestSetRunCIRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := enqueue(t, s, mr1, sha('a'))
	if r := mustRun(t, s, id); r.CIJobs != nil || r.CIChecked || r.CIDeadline != nil {
		t.Errorf("new run CI fields = %v/%v/%v; want zero", r.CIJobs, r.CIChecked, r.CIDeadline)
	}
	jobs := []CIJobResult{
		{Name: "test", Result: "failed", Read: "log", Size: 1200, Cut: true, Blocks: true},
		{Name: "lint", Result: "unreadable", Reason: "no permission", Read: "report"},
	}
	if err := s.SetRunCI(ctx, id, jobs); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, s, id).CIJobs; !reflect.DeepEqual(got, jobs) {
		t.Errorf("CIJobs = %+v, want %+v", got, jobs)
	}
	if err := s.SetRunCI(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, s, id).CIJobs; got != nil {
		t.Errorf("CIJobs after nil = %+v, want nil", got)
	}
}

// ciSnapshot gives run id a snapshot of one job with the given result.
func ciSnapshot(t *testing.T, s *Store, id, result string) {
	t.Helper()
	if err := s.SetRunCI(ctx, id, []CIJobResult{{Name: "test", Result: result, Read: "status", Blocks: true}}); err != nil {
		t.Fatal(err)
	}
}

func ids(runs []Run) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

func TestCIPendingRuns(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	later := enqueue(t, s, mr1, sha('a'))
	sooner := enqueue(t, s, mr2, sha('b'))
	plain := enqueue(t, s, MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: 60}, sha('c'))
	_ = plain
	if _, err := s.HoldForCI(ctx, later, ciDeadline.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HoldForCI(ctx, sooner, ciDeadline); err != nil {
		t.Fatal(err)
	}
	got, err := s.CIPendingRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{sooner, later}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("CIPendingRuns = %v, want %v (by deadline)", ids(got), want)
	}
}

// CIRunsForHead gives the ci_pending runs of the head and the newest published
// run of each merge request of that head; no other.
func TestCIRunsForHead(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	head := sha('a')
	pending := enqueue(t, s, mr1, head)
	if _, err := s.HoldForCI(ctx, pending, ciDeadline); err != nil {
		t.Fatal(err)
	}
	pub := published(t, s, mr2, head, "main")
	// Another merge request whose newest published run is of another head.
	otherMR := MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: 70}
	published(t, s, otherMR, sha('b'), "main")
	// A merge request that was reviewed at this head, then moved on.
	movedMR := MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: 71}
	published(t, s, movedMR, head, "main")
	published(t, s, movedMR, sha('c'), "main")
	// A pending run of another head and one of another project.
	otherPending := enqueue(t, s, MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: 72}, sha('d'))
	if _, err := s.HoldForCI(ctx, otherPending, ciDeadline); err != nil {
		t.Fatal(err)
	}

	got, err := s.CIRunsForHead(ctx, mr1.Instance, "acme/widgets", head)
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := map[string]bool{}
	for _, r := range got {
		gotIDs[r.ID] = true
	}
	if len(got) != 2 || !gotIDs[pending] || !gotIDs[pub] {
		t.Errorf("CIRunsForHead = %v, want %v and %v", ids(got), pending, pub)
	}
	if none, err := s.CIRunsForHead(ctx, mr1.Instance, "acme/other", head); err != nil || len(none) != 0 {
		t.Errorf("CIRunsForHead of another project = %v, %v; want none", ids(none), err)
	}
	if none, err := s.CIRunsForHead(ctx, "https://other.example.com", "acme/widgets", head); err != nil || len(none) != 0 {
		t.Errorf("CIRunsForHead of another instance = %v, %v; want none", ids(none), err)
	}
}

// CIRunsForMR gives the ci_pending runs of one merge request and its newest
// published run, whatever their head; no other merge request's.
func TestCIRunsForMR(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	published(t, s, mr1, sha('b'), "main")
	newest := published(t, s, mr1, sha('c'), "main")
	published(t, s, mr2, sha('d'), "main")
	// The push after the reviews: enqueuing a head supersedes the merge
	// request's older waiting runs, so the waiting run comes last.
	pending := enqueue(t, s, mr1, sha('e'))
	if _, err := s.HoldForCI(ctx, pending, ciDeadline); err != nil {
		t.Fatal(err)
	}

	got, err := s.CIRunsForMR(ctx, mr1.Instance, "acme/widgets", mr1.IID)
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := map[string]bool{}
	for _, r := range got {
		gotIDs[r.ID] = true
	}
	if len(got) != 2 || !gotIDs[pending] || !gotIDs[newest] {
		t.Errorf("CIRunsForMR = %v, want %v and %v", ids(got), pending, newest)
	}
	if none, err := s.CIRunsForMR(ctx, mr1.Instance, "acme/other", mr1.IID); err != nil || len(none) != 0 {
		t.Errorf("CIRunsForMR of another project = %v, %v; want none", ids(none), err)
	}
}

// CIRecheckRuns gives the newest published run of each merge request, since a
// time, whose snapshot has a result other than passed.
func TestCIRecheckRuns(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mk := func(iid int64) MRKey { return MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: iid} }
	failed := published(t, s, mk(80), sha('a'), "main")
	ciSnapshot(t, s, failed, "failed")
	passed := published(t, s, mk(81), sha('b'), "main")
	ciSnapshot(t, s, passed, "passed")
	nosnap := published(t, s, mk(82), sha('c'), "main")
	_ = nosnap
	// An older published run with a failed job is not the newest of its MR.
	older := published(t, s, mk(83), sha('d'), "main")
	ciSnapshot(t, s, older, "running")
	newer := published(t, s, mk(83), sha('e'), "main")
	ciSnapshot(t, s, newer, "passed")

	got, err := s.CIRecheckRuns(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{failed}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("CIRecheckRuns = %v, want %v", ids(got), want)
	}
	// Published runs here were reported at t0+1m: a later `since` leaves them out.
	if none, err := s.CIRecheckRuns(ctx, t0.Add(time.Hour)); err != nil || len(none) != 0 {
		t.Errorf("CIRecheckRuns since later = %v, %v; want none", ids(none), err)
	}
}

func TestSetProjectCIAccess(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if err := s.UpsertProject(ctx, Project{Instance: "gh", ProjectID: 9, Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.Project(ctx, "gh", 9)
	if err != nil || !ok || p.ActionsReadable != nil || p.CheckRunEvents != nil {
		t.Fatalf("new project = %+v, %v, %v; want NULL access", p, ok, err)
	}
	if err := s.SetProjectCIAccess(ctx, "gh", 9, true, false); err != nil {
		t.Fatal(err)
	}
	p, _, err = s.Project(ctx, "gh", 9)
	if err != nil || p.ActionsReadable == nil || !*p.ActionsReadable || p.CheckRunEvents == nil || *p.CheckRunEvents {
		t.Errorf("project = %+v, %v; want actions readable true, check run events false", p, err)
	}
	// A later upsert (a reconcile) keeps what was stored.
	if err := s.UpsertProject(ctx, Project{Instance: "gh", ProjectID: 9, Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	if p, _, err = s.Project(ctx, "gh", 9); err != nil || p.ActionsReadable == nil || !*p.ActionsReadable {
		t.Errorf("project after upsert = %+v, %v; want the access kept", p, err)
	}
	// A project that is not stored is not an error: nothing to record.
	if err := s.SetProjectCIAccess(ctx, "gh", 404, true, true); err != nil {
		t.Errorf("SetProjectCIAccess of an unknown project = %v, want nil", err)
	}
}

func TestDashboardRepoCIJobsRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	jobs := &CIJobs{WaitSeconds: 1800, Jobs: []CIJob{
		{Name: "test", Read: "log", BlocksApproval: true},
		{Name: "lint", Read: "report", Report: &CIReport{Artifact: "reports", Path: "out/lint.txt"}},
	}}
	read := func() DashboardRepo {
		t.Helper()
		r, ok, err := s.DashboardRepo(ctx, "gh", "acme/widgets")
		if err != nil || !ok {
			t.Fatalf("DashboardRepo = %v, %v", ok, err)
		}
		return r
	}

	if created, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "gh", Project: "acme/widgets", CIJobs: jobs}); err != nil || !created {
		t.Fatalf("InsertDashboardRepo = %v, %v", created, err)
	}
	if got := read().CIJobs; !reflect.DeepEqual(got, jobs) {
		t.Errorf("after create CIJobs = %+v, want %+v", got, jobs)
	}

	if found, err := s.UpdateDashboardRepo(ctx, "gh", "acme/widgets", func(r *DashboardRepo) { r.CIJobs = nil }); err != nil || !found {
		t.Fatalf("UpdateDashboardRepo = %v, %v", found, err)
	}
	if got := read().CIJobs; got != nil {
		t.Errorf("after update to nil CIJobs = %+v, want nil", got)
	}

	if found, err := s.UpdateDashboardRepo(ctx, "gh", "acme/widgets", func(r *DashboardRepo) { r.CIJobs = jobs }); err != nil || !found {
		t.Fatalf("UpdateDashboardRepo = %v, %v", found, err)
	}
	if got := read().CIJobs; !reflect.DeepEqual(got, jobs) {
		t.Errorf("after update CIJobs = %+v, want %+v", got, jobs)
	}

	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "gh", Project: "Acme/Widgets"}); err != nil {
		t.Fatal(err)
	}
	if got := read().CIJobs; got != nil {
		t.Errorf("after upsert without CIJobs = %+v, want nil", got)
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "gh", Project: "acme/widgets", CIJobs: jobs}); err != nil {
		t.Fatal(err)
	}
	if got := read().CIJobs; !reflect.DeepEqual(got, jobs) {
		t.Errorf("after upsert CIJobs = %+v, want %+v", got, jobs)
	}
}

// The jsonb helpers need no database: NULL for nothing, and a snapshot or a
// repository's jobs read back as written.
func TestCIJSONHelpers(t *testing.T) {
	if v, err := ciJobsParam(nil); err != nil || v != nil {
		t.Errorf("ciJobsParam(nil) = %v, %v; want nil", v, err)
	}
	if v, err := ciResultsParam(nil); err != nil || v != nil {
		t.Errorf("ciResultsParam(nil) = %v, %v; want nil", v, err)
	}
	if got, err := readCIJobs(nil); err != nil || got != nil {
		t.Errorf("readCIJobs(nil) = %v, %v; want nil", got, err)
	}
	if got, err := readCIResults(nil); err != nil || got != nil {
		t.Errorf("readCIResults(nil) = %v, %v; want nil", got, err)
	}
	jobs := &CIJobs{WaitSeconds: 300, Jobs: []CIJob{{Name: "a", Read: "report", Report: &CIReport{Path: "r.txt"}, BlocksApproval: true}}}
	v, err := ciJobsParam(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readCIJobs(v.([]byte)); err != nil || !reflect.DeepEqual(got, jobs) {
		t.Errorf("readCIJobs = %+v, %v; want %+v", got, err, jobs)
	}
	res := []CIJobResult{{Name: "a", Result: "unreadable", Reason: "error", Read: "log", Size: 3, Cut: true, Blocks: true}}
	v, err = ciResultsParam(res)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readCIResults(v.([]byte)); err != nil || !reflect.DeepEqual(got, res) {
		t.Errorf("readCIResults = %+v, %v; want %+v", got, err, res)
	}
	if _, err := readCIResults([]byte(`{`)); err == nil {
		t.Error("readCIResults of bad JSON = nil error")
	}
}

func TestRecentRunHeads(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	enqueue(t, s, mr1, sha('a'))
	enqueue(t, s, mr1, sha('b'))
	enqueue(t, s, mr1, sha('c'))
	got, err := s.RecentRunHeads(ctx, mr1.Instance, mr1.ProjectID, 2)
	if err != nil || len(got) != 2 || got[0] != sha('c') || got[1] != sha('b') {
		t.Errorf("RecentRunHeads(2) = %v, %v; want the 2 newest heads, newest first", got, err)
	}
	if none, err := s.RecentRunHeads(ctx, mr1.Instance, mr1.ProjectID+1, 10); err != nil || len(none) != 0 {
		t.Errorf("RecentRunHeads of another project = %v, %v; want none", none, err)
	}
}
