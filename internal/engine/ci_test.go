package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/ci"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// These need no database: the CI wait's store calls go to memCI and its
// platform calls to ciForge.

// ciNow is the unit tests' clock; ciCreated is when their run was queued.
var (
	ciNow     = time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	ciCreated = ciNow.Add(-5 * time.Minute)
)

// ciJobsSet is a repository's choice: test (its log, blocking) and
// cycode-scan (its status), waiting 30 minutes.
var ciJobsSet = store.CIJobs{WaitSeconds: 1800, Jobs: []store.CIJob{
	{Name: "test", Read: "log", BlocksApproval: true},
	{Name: "cycode-scan", Read: "status"},
}}

// memCI records the CI wait's store calls.
type memCI struct {
	mu        sync.Mutex
	calls     []string
	deadline  time.Time
	held      bool // what HoldForCI answers
	released  bool // what ReleaseCI answers
	republish bool // what RepublishNewest answers
	setErr    error
	republErr error
	stored    []store.CIJobResult
	setCalled bool

	republishedHead, republishedTarget string
}

var _ ciStore = (*memCI)(nil)

func (m *memCI) note(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, s)
}

func (m *memCI) HoldForCI(_ context.Context, _ string, deadline time.Time) (bool, error) {
	m.note("hold")
	m.deadline = deadline
	return m.held, nil
}

func (m *memCI) ReleaseCI(context.Context, string) (bool, error) {
	m.note("release")
	return m.released, nil
}

func (m *memCI) MarkCIChecked(context.Context, string) error {
	m.note("checked")
	return nil
}

func (m *memCI) SetRunCI(_ context.Context, _ string, jobs []store.CIJobResult) error {
	m.note("set")
	m.setCalled = true
	m.stored = jobs
	return m.setErr
}

func (m *memCI) Republish(_ context.Context, _ store.MRKey, head, target string) (bool, error) {
	m.note("republish")
	m.republishedHead, m.republishedTarget = head, target
	return m.republish, m.republErr
}

func (m *memCI) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// ciForge is a platform that reads CI: snap, or err. The embedded Forge is
// nil: only SetStatus and the CIReader methods are called.
type ciForge struct {
	forge.Forge
	snap     ci.Snapshot
	err      error
	reads    []bool // withText of each ReadCI
	targets  []forge.CITarget
	statuses []string
}

var _ forge.CIReader = (*ciForge)(nil)

func (f *ciForge) ReadCI(_ context.Context, t forge.CITarget, _ []store.CIJob, withText bool) (ci.Snapshot, error) {
	f.reads = append(f.reads, withText)
	f.targets = append(f.targets, t)
	return f.snap, f.err
}

func (f *ciForge) RecentCINames(context.Context, forge.CITarget) ([]string, error) { return nil, nil }

func (f *ciForge) SetStatus(_ context.Context, _ int64, _ string, s forge.StatusState, description string) error {
	f.statuses = append(f.statuses, string(s)+": "+description)
	return nil
}

// noCIForge is a platform that can't read CI.
type noCIForge struct {
	forge.Forge
	statuses []string
}

func (f *noCIForge) SetStatus(_ context.Context, _ int64, _ string, s forge.StatusState, description string) error {
	f.statuses = append(f.statuses, string(s)+": "+description)
	return nil
}

// ciRun is a merge request's push run of head 'a', queued at ciCreated.
func ciRun() store.Run {
	return store.Run{
		ID: "11111111-1111-1111-1111-111111111111", MR: store.MRKey{Instance: testInstance, ProjectID: testProjectID, IID: 3},
		ProjectPath: testProjectPath, HeadSHA: sha('a'), TargetBranch: "main", State: store.Running,
		Trigger: store.TriggerPush, Kind: store.KindMR, CreatedAt: ciCreated,
	}
}

// ciDispatcher is a dispatcher of run on an engine with the unit clock and
// a log the test reads, its CI store calls going to st.
func ciDispatcher(run store.Run, st ciStore) (*dispatcher, *bytes.Buffer) {
	var logs bytes.Buffer
	e := &Engine{
		Config: testConfig("https://gitlab.example.com"),
		Now:    func() time.Time { return ciNow },
		Log:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return &dispatcher{e: e, run: run, log: e.log(), cis: st}, &logs
}

func ciRepo() settings.Effective { return settings.Effective{CIJobs: ciJobsSet} }

// snapshot is the chosen jobs read with results: test then cycode-scan.
func snapshot(finished bool, test, scan ci.Job) ci.Snapshot {
	test.Name, test.Read, test.Blocks = "test", "log", true
	scan.Name, scan.Read = "cycode-scan", "status"
	return ci.Snapshot{Head: sha('a'), Finished: finished, Jobs: []ci.Job{test, scan}}
}

func TestWaitingText(t *testing.T) {
	if got, want := waitingText(ciJobsSet.Jobs), "Waiting for CI: test, cycode-scan"; got != want {
		t.Errorf("waitingText = %q, want %q", got, want)
	}
	if got, want := waitingText([]store.CIJob{{Name: "lint"}}), "Waiting for CI: lint"; got != want {
		t.Errorf("waitingText = %q, want %q", got, want)
	}
}

func TestCITarget(t *testing.T) {
	got := ciTarget(ciRun(), Platform{})
	want := forge.CITarget{ProjectID: testProjectID, Project: testProjectPath, Change: 3, Head: sha('a')}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ciTarget = %+v, want %+v", got, want)
	}
}

func TestGateCI(t *testing.T) {
	ended := snapshot(true, ci.Job{Result: ci.Failed, Ended: true}, ci.Job{Result: ci.Passed, Ended: true})
	running := snapshot(false, ci.Job{Result: ci.Running}, ci.Job{Result: ci.Passed, Ended: true})

	t.Run("all ended", func(t *testing.T) {
		st := &memCI{held: true}
		f := &ciForge{snap: ended}
		d, _ := ciDispatcher(ciRun(), st)
		held, err := d.gateCI(context.Background(), Platform{Forge: f}, ciRepo())
		if held || err != nil {
			t.Fatalf("gateCI = %v, %v; want not held", held, err)
		}
		if got := st.Calls(); !reflect.DeepEqual(got, []string{"checked"}) {
			t.Errorf("store calls = %v, want [checked]", got)
		}
		if !reflect.DeepEqual(f.reads, []bool{false}) {
			t.Errorf("reads (withText) = %v, want one status-only read", f.reads)
		}
		if len(f.statuses) != 0 {
			t.Errorf("statuses = %v, want none", f.statuses)
		}
	})

	t.Run("missing on a finished head", func(t *testing.T) {
		st := &memCI{held: true}
		f := &ciForge{snap: snapshot(true, ci.Job{Result: ci.Missing}, ci.Job{Result: ci.Passed, Ended: true})}
		d, _ := ciDispatcher(ciRun(), st)
		if held, err := d.gateCI(context.Background(), Platform{Forge: f}, ciRepo()); held || err != nil {
			t.Fatalf("gateCI = %v, %v; want not held", held, err)
		}
	})

	t.Run("a job running", func(t *testing.T) {
		st := &memCI{held: true}
		f := &ciForge{snap: running}
		d, _ := ciDispatcher(ciRun(), st)
		held, err := d.gateCI(context.Background(), Platform{Forge: f}, ciRepo())
		if !held || err != nil {
			t.Fatalf("gateCI = %v, %v; want held", held, err)
		}
		if got := st.Calls(); !reflect.DeepEqual(got, []string{"hold"}) {
			t.Errorf("store calls = %v, want [hold]", got)
		}
		if want := ciCreated.Add(30 * time.Minute); !st.deadline.Equal(want) {
			t.Errorf("deadline = %v, want created + wait = %v", st.deadline, want)
		}
		if want := []string{"pending: Waiting for CI: test, cycode-scan"}; !reflect.DeepEqual(f.statuses, want) {
			t.Errorf("statuses = %v, want %v", f.statuses, want)
		}
	})

	t.Run("missing on a head still running CI", func(t *testing.T) {
		st := &memCI{held: true}
		f := &ciForge{snap: snapshot(false, ci.Job{Result: ci.Missing}, ci.Job{Result: ci.Passed, Ended: true})}
		d, _ := ciDispatcher(ciRun(), st)
		if held, err := d.gateCI(context.Background(), Platform{Forge: f}, ciRepo()); !held || err != nil {
			t.Fatalf("gateCI = %v, %v; want held", held, err)
		}
	})

	allMissing := snapshot(true, ci.Job{Result: ci.Missing}, ci.Job{Result: ci.Missing})

	t.Run("no pipeline yet, within the grace", func(t *testing.T) {
		// GitLab creates the pipeline after the merge request's hook: a head
		// with none of the jobs yet waits a little for it.
		st := &memCI{held: true}
		f := &ciForge{snap: allMissing}
		run := ciRun()
		run.CreatedAt = ciNow.Add(-time.Minute)
		d, _ := ciDispatcher(run, st)
		if held, err := d.gateCI(context.Background(), Platform{Forge: f}, ciRepo()); !held || err != nil {
			t.Fatalf("gateCI = %v, %v; want held", held, err)
		}
		if want := run.CreatedAt.Add(30 * time.Minute); !st.deadline.Equal(want) {
			t.Errorf("deadline = %v, want %v", st.deadline, want)
		}
	})

	t.Run("no pipeline after the grace", func(t *testing.T) {
		st := &memCI{held: true}
		f := &ciForge{snap: allMissing}
		run := ciRun()
		run.CreatedAt = ciNow.Add(-ciNoPipelineGrace)
		d, _ := ciDispatcher(run, st)
		if held, err := d.gateCI(context.Background(), Platform{Forge: f}, ciRepo()); held || err != nil {
			t.Fatalf("gateCI = %v, %v; want not held", held, err)
		}
	})

	t.Run("one job found within the grace", func(t *testing.T) {
		st := &memCI{held: true}
		f := &ciForge{snap: snapshot(true, ci.Job{Result: ci.Missing}, ci.Job{Result: ci.Passed, Ended: true})}
		run := ciRun()
		run.CreatedAt = ciNow.Add(-time.Minute)
		d, _ := ciDispatcher(run, st)
		if held, err := d.gateCI(context.Background(), Platform{Forge: f}, ciRepo()); held || err != nil {
			t.Fatalf("gateCI = %v, %v; want not held (the head's CI is finished)", held, err)
		}
	})

	t.Run("a reader error holds", func(t *testing.T) {
		st := &memCI{held: true}
		f := &ciForge{err: errors.New("gitlab: 502 from " + testLLMKey)}
		d, logs := ciDispatcher(ciRun(), st)
		d.secrets = []string{testLLMKey}
		held, err := d.gateCI(context.Background(), Platform{Forge: f}, ciRepo())
		if !held || err != nil {
			t.Fatalf("gateCI = %v, %v; want held, not failed", held, err)
		}
		if got := st.Calls(); !reflect.DeepEqual(got, []string{"hold"}) {
			t.Errorf("store calls = %v, want [hold]", got)
		}
		if strings.Contains(logs.String(), testLLMKey) {
			t.Errorf("the log holds a secret:\n%s", logs.String())
		}
	})

	t.Run("the deadline already past", func(t *testing.T) {
		// A run started long after it was queued (an untrusted author's):
		// the wait is over, so CI isn't read and nothing holds it.
		st := &memCI{held: true}
		f := &ciForge{snap: running}
		run := ciRun()
		run.CreatedAt = ciNow.Add(-31 * time.Minute)
		d, _ := ciDispatcher(run, st)
		if held, err := d.gateCI(context.Background(), Platform{Forge: f}, ciRepo()); held || err != nil {
			t.Fatalf("gateCI = %v, %v; want not held", held, err)
		}
		if got := st.Calls(); !reflect.DeepEqual(got, []string{"checked"}) {
			t.Errorf("store calls = %v, want [checked]", got)
		}
		if len(f.reads) != 0 {
			t.Errorf("CI was read %d times, want none", len(f.reads))
		}
	})

	t.Run("a forge without a CI reader", func(t *testing.T) {
		st := &memCI{held: true}
		f := &noCIForge{}
		d, _ := ciDispatcher(ciRun(), st)
		held, err := d.gateCI(context.Background(), Platform{Forge: f}, ciRepo())
		if held || err != nil {
			t.Fatalf("gateCI = %v, %v; want not held", held, err)
		}
		if got := st.Calls(); !reflect.DeepEqual(got, []string{"checked"}) {
			t.Errorf("store calls = %v, want [checked]", got)
		}
		// At dispatch every job reads unreadable (error), and the file is
		// still written.
		in := &inputs.Inputs{}
		if err := d.addCIResults(context.Background(), Platform{Forge: f}, ciRepo(), in); err != nil {
			t.Fatal(err)
		}
		for _, j := range st.stored {
			if j.Result != string(ci.Unreadable) || j.Reason != "error" {
				t.Errorf("job %s = %s (%s), want unreadable (error)", j.Name, j.Result, j.Reason)
			}
		}
		if len(st.stored) != 2 {
			t.Errorf("stored %d jobs, want 2", len(st.stored))
		}
		if !strings.Contains(string(in.CIResults), "## test: unreadable (error) (blocks approval)") {
			t.Errorf("ci-results.md = %q", in.CIResults)
		}
	})
}

// An API review is never gated and gets no CI file: dispatch() only gates
// and reads CI for a merge request's run. ciApplies says so.
func TestCIAppliesNotToAPIReviews(t *testing.T) {
	run := ciRun()
	if !ciApplies(run, ciRepo()) {
		t.Errorf("a merge request's run with jobs chosen: ciApplies = false")
	}
	run.Kind = store.KindAPI
	if ciApplies(run, ciRepo()) {
		t.Errorf("an API review: ciApplies = true")
	}
	if ciApplies(ciRun(), settings.Effective{}) {
		t.Errorf("no jobs chosen: ciApplies = true")
	}
}

func TestAddCIResults(t *testing.T) {
	// otherKey is an LLM key the registry knows that isn't this run's.
	const otherKey = "other-llm-key-example"
	t.Setenv("OPENCODE_API_KEY", otherKey)
	snap := snapshot(true,
		ci.Job{Result: ci.Failed, Ended: true, Log: "FAIL TestX\n" + testLLMKey + " " + otherKey + "\n", Cut: true},
		ci.Job{Result: ci.Passed, Ended: true})

	t.Run("read with text", func(t *testing.T) {
		st := &memCI{}
		f := &ciForge{snap: snap}
		d, logs := ciDispatcher(ciRun(), st)
		d.secrets = []string{testLLMKey}
		in := &inputs.Inputs{}
		if err := d.addCIResults(context.Background(), Platform{Forge: f}, ciRepo(), in); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(f.reads, []bool{true}) {
			t.Errorf("reads (withText) = %v, want one read with text", f.reads)
		}
		if want := (forge.CITarget{ProjectID: testProjectID, Project: testProjectPath, Change: 3, Head: sha('a')}); !reflect.DeepEqual(f.targets[0], want) {
			t.Errorf("target = %+v, want %+v", f.targets[0], want)
		}
		want := strings.NewReplacer(testLLMKey, "***", otherKey, "***").Replace(string(ci.Build(snap)))
		if string(in.CIResults) != want {
			t.Errorf("ci-results.md =\n%s\nwant\n%s", in.CIResults, want)
		}
		if s := string(in.CIResults); strings.Contains(s, testLLMKey) || strings.Contains(s, otherKey) {
			t.Errorf("ci-results.md holds an LLM key")
		}
		if !reflect.DeepEqual(st.stored, snap.Results()) {
			t.Errorf("SetRunCI got %+v, want %+v", st.stored, snap.Results())
		}
		if strings.Contains(logs.String(), "FAIL TestX") {
			t.Errorf("CI text reached the log:\n%s", logs.String())
		}
	})

	t.Run("a reader error", func(t *testing.T) {
		st := &memCI{}
		f := &ciForge{err: errors.New("gitlab: 502")}
		d, _ := ciDispatcher(ciRun(), st)
		in := &inputs.Inputs{}
		if err := d.addCIResults(context.Background(), Platform{Forge: f}, ciRepo(), in); err != nil {
			t.Fatal(err)
		}
		if len(st.stored) != 2 {
			t.Fatalf("stored %+v, want both jobs", st.stored)
		}
		for _, j := range st.stored {
			if j.Result != string(ci.Unreadable) || j.Reason != "error" || j.Size != 0 {
				t.Errorf("job %+v, want unreadable (error) with no text", j)
			}
		}
		if s := string(in.CIResults); !strings.Contains(s, "## test: unreadable (error) (blocks approval)") ||
			!strings.Contains(s, "## cycode-scan: unreadable (error)") {
			t.Errorf("ci-results.md = %q", s)
		}
	})

	t.Run("the run is gone", func(t *testing.T) {
		st := &memCI{setErr: store.ErrRunGone}
		f := &ciForge{snap: snap}
		d, _ := ciDispatcher(ciRun(), st)
		in := &inputs.Inputs{}
		if err := d.addCIResults(context.Background(), Platform{Forge: f}, ciRepo(), in); !errors.Is(err, store.ErrRunGone) {
			t.Fatalf("addCIResults = %v, want ErrRunGone", err)
		}
		if in.CIResults != nil {
			t.Errorf("a gone run's inputs got the file")
		}
	})

	t.Run("a cancelled read", func(t *testing.T) {
		st := &memCI{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		f := &ciForge{err: context.Canceled}
		d, _ := ciDispatcher(ciRun(), st)
		if err := d.addCIResults(ctx, Platform{Forge: f}, ciRepo(), &inputs.Inputs{}); err == nil {
			t.Fatal("addCIResults = nil, want an error that sends the run back to the queue")
		}
		if st.setCalled {
			t.Errorf("a cancelled read stored a snapshot of unreadable jobs")
		}
	})
}

// ciEngine is an engine with the unit clock.
func ciEngine() (*Engine, *bytes.Buffer) {
	var logs bytes.Buffer
	return &Engine{
		Config: testConfig("https://gitlab.example.com"),
		Now:    func() time.Time { return ciNow },
		Log:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, &logs
}

// pendingRun is a run waiting for CI until deadline.
func pendingRun(deadline time.Time) store.Run {
	r := ciRun()
	r.State, r.SkipReason, r.CIDeadline = store.Waiting, store.WaitCIPending, &deadline
	return r
}

func engineWoken(e *Engine) bool {
	select {
	case <-e.wakeCh():
		return true
	default:
		return false
	}
}

func TestCheckCIReleasesPending(t *testing.T) {
	running := snapshot(false, ci.Job{Result: ci.Running}, ci.Job{Result: ci.Passed, Ended: true})
	ended := snapshot(true, ci.Job{Result: ci.Failed, Ended: true}, ci.Job{Result: ci.Passed, Ended: true})
	allMissing := snapshot(true, ci.Job{Result: ci.Missing}, ci.Job{Result: ci.Missing})
	later := ciNow.Add(10 * time.Minute)
	for _, tc := range []struct {
		name        string
		run         store.Run
		jobs        []store.CIJob
		reader      bool
		snap        ci.Snapshot
		err         error
		wantRelease bool
		wantReads   int
		wantErr     bool
	}{
		{name: "still running", run: pendingRun(later), jobs: ciJobsSet.Jobs, reader: true, snap: running, wantReads: 1},
		{name: "every job ended", run: pendingRun(later), jobs: ciJobsSet.Jobs, reader: true, snap: ended, wantRelease: true, wantReads: 1},
		{name: "deadline passed", run: pendingRun(ciNow), jobs: ciJobsSet.Jobs, reader: true, snap: running, wantRelease: true},
		{name: "no deadline recorded", run: func() store.Run { r := pendingRun(later); r.CIDeadline = nil; return r }(), jobs: ciJobsSet.Jobs, reader: true, snap: running, wantRelease: true},
		{name: "jobs emptied", run: pendingRun(later), reader: true, snap: running, wantRelease: true},
		{name: "a read error waits", run: pendingRun(later), jobs: ciJobsSet.Jobs, reader: true, err: errors.New("gitlab: 502"), wantReads: 1, wantErr: true},
		{name: "not connected waits for the deadline", run: pendingRun(later), jobs: ciJobsSet.Jobs},
		{name: "not connected past the deadline", run: pendingRun(ciNow.Add(-time.Second)), jobs: ciJobsSet.Jobs, wantRelease: true},
		{name: "no pipeline yet, within the grace", run: func() store.Run { r := pendingRun(later); r.CreatedAt = ciNow.Add(-time.Minute); return r }(),
			jobs: ciJobsSet.Jobs, reader: true, snap: allMissing, wantReads: 1},
		{name: "no pipeline after the grace", run: pendingRun(later), jobs: ciJobsSet.Jobs, reader: true, snap: allMissing, wantRelease: true, wantReads: 1},
		{name: "one job found within the grace", run: func() store.Run { r := pendingRun(later); r.CreatedAt = ciNow.Add(-time.Minute); return r }(),
			jobs: ciJobsSet.Jobs, reader: true, snap: snapshot(true, ci.Job{Result: ci.Missing}, ci.Job{Result: ci.Passed, Ended: true}), wantRelease: true, wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := ciEngine()
			st := &memCI{released: true}
			var reader forge.CIReader
			f := &ciForge{snap: tc.snap, err: tc.err}
			if tc.reader {
				reader = f
			}
			err := e.checkCI(context.Background(), st, reader, Platform{}, tc.run, tc.jobs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkCI = %v, want error %v", err, tc.wantErr)
			}
			released := reflect.DeepEqual(st.Calls(), []string{"release"})
			if released != tc.wantRelease || (!tc.wantRelease && len(st.Calls()) != 0) {
				t.Errorf("store calls = %v, want release %v", st.Calls(), tc.wantRelease)
			}
			if engineWoken(e) != tc.wantRelease {
				t.Errorf("dispatcher woken = %v, want %v", !tc.wantRelease, tc.wantRelease)
			}
			if len(f.reads) != tc.wantReads {
				t.Errorf("%d reads, want %d", len(f.reads), tc.wantReads)
			}
			for _, w := range f.reads {
				if w {
					t.Errorf("a sweep read CI text")
				}
			}
		})
	}
}

// publishedRun is a published review whose snapshot is stored.
func publishedRun(stored ...store.CIJobResult) store.Run {
	r := ciRun()
	r.State, r.CIJobs = store.Published, stored
	return r
}

func TestCheckCIRedecidesPublished(t *testing.T) {
	testFailed := store.CIJobResult{Name: "test", Result: "failed", Read: "log", Size: 120, Cut: true, Blocks: true}
	scanRunning := store.CIJobResult{Name: "cycode-scan", Result: "running", Read: "status"}
	read := snapshot(true, ci.Job{Result: ci.Passed, Ended: true}, ci.Job{Result: ci.Passed, Ended: true})

	t.Run("a result changed", func(t *testing.T) {
		e, _ := ciEngine()
		st := &memCI{republish: true}
		f := &ciForge{snap: read}
		if err := e.checkCI(context.Background(), st, f, Platform{}, publishedRun(testFailed, scanRunning), ciJobsSet.Jobs); err != nil {
			t.Fatal(err)
		}
		if got := st.Calls(); !reflect.DeepEqual(got, []string{"republish", "set"}) {
			t.Fatalf("store calls = %v, want republish then set", got)
		}
		if st.republishedHead != sha('a') || st.republishedTarget != "main" {
			t.Errorf("republished %s onto %s, want this run's head and target", st.republishedHead, st.republishedTarget)
		}
		// What the review was given (read, size, cut, blocks) stays; the
		// result is the new one.
		want := []store.CIJobResult{
			{Name: "test", Result: "passed", Read: "log", Size: 120, Cut: true, Blocks: true},
			{Name: "cycode-scan", Result: "passed", Read: "status"},
		}
		if !reflect.DeepEqual(st.stored, want) {
			t.Errorf("stored %+v, want %+v", st.stored, want)
		}
		if !reflect.DeepEqual(f.reads, []bool{false}) {
			t.Errorf("reads (withText) = %v, want one status-only read", f.reads)
		}
		if !engineWoken(e) {
			t.Errorf("the publisher wasn't woken")
		}
	})

	t.Run("nothing changed", func(t *testing.T) {
		e, _ := ciEngine()
		st := &memCI{republish: true}
		f := &ciForge{snap: snapshot(true, ci.Job{Result: ci.Failed, Ended: true}, ci.Job{Result: ci.Running})}
		if err := e.checkCI(context.Background(), st, f, Platform{}, publishedRun(testFailed, scanRunning), ciJobsSet.Jobs); err != nil {
			t.Fatal(err)
		}
		if got := st.Calls(); len(got) != 0 {
			t.Errorf("store calls = %v, want none", got)
		}
	})

	t.Run("a failed text read is kept", func(t *testing.T) {
		// The log couldn't be read at dispatch: a status-only read says
		// nothing about that, so the job stays unreadable.
		textFailed := store.CIJobResult{Name: "test", Result: "unreadable", Reason: "error", Read: "log", Blocks: true, TextFailed: true}
		e, _ := ciEngine()
		st := &memCI{republish: true}
		f := &ciForge{snap: snapshot(true, ci.Job{Result: ci.Failed, Ended: true}, ci.Job{Result: ci.Running})}
		if err := e.checkCI(context.Background(), st, f, Platform{}, publishedRun(textFailed, scanRunning), ciJobsSet.Jobs); err != nil {
			t.Fatal(err)
		}
		if got := st.Calls(); len(got) != 0 {
			t.Errorf("store calls = %v, want none", got)
		}

		// When another job changes, the kept entry is written as it was.
		st = &memCI{republish: true}
		f = &ciForge{snap: snapshot(true, ci.Job{Result: ci.Passed, Ended: true}, ci.Job{Result: ci.Passed, Ended: true})}
		if err := e.checkCI(context.Background(), st, f, Platform{}, publishedRun(textFailed, scanRunning), ciJobsSet.Jobs); err != nil {
			t.Fatal(err)
		}
		want := []store.CIJobResult{textFailed, {Name: "cycode-scan", Result: "passed", Read: "status"}}
		if !reflect.DeepEqual(st.stored, want) {
			t.Errorf("stored %+v, want %+v", st.stored, want)
		}
	})

	t.Run("a failed republish stores nothing", func(t *testing.T) {
		e, _ := ciEngine()
		st := &memCI{republErr: errors.New("db down")}
		f := &ciForge{snap: read}
		if err := e.checkCI(context.Background(), st, f, Platform{}, publishedRun(testFailed, scanRunning), ciJobsSet.Jobs); err == nil {
			t.Fatal("checkCI = nil, want the store error")
		}
		if st.setCalled {
			t.Errorf("the snapshot was stored although the review wasn't decided again")
		}
	})

	t.Run("not the newest review any more", func(t *testing.T) {
		e, _ := ciEngine()
		st := &memCI{republish: false}
		f := &ciForge{snap: read}
		if err := e.checkCI(context.Background(), st, f, Platform{}, publishedRun(testFailed, scanRunning), ciJobsSet.Jobs); err != nil {
			t.Fatal(err)
		}
		if st.setCalled {
			t.Errorf("the snapshot was stored on a review that isn't re-decided")
		}
	})

	t.Run("superseded meanwhile", func(t *testing.T) {
		e, logs := ciEngine()
		st := &memCI{republish: true, setErr: store.ErrRunGone}
		f := &ciForge{snap: read}
		if err := e.checkCI(context.Background(), st, f, Platform{}, publishedRun(testFailed, scanRunning), ciJobsSet.Jobs); err != nil {
			t.Fatalf("checkCI = %v, want a gone run skipped quietly", err)
		}
		if strings.Contains(logs.String(), "level=ERROR") || strings.Contains(logs.String(), "level=WARN") {
			t.Errorf("a gone run was logged as trouble:\n%s", logs.String())
		}
	})

	t.Run("the repository chooses no jobs any more", func(t *testing.T) {
		e, _ := ciEngine()
		st := &memCI{republish: true}
		f := &ciForge{snap: read}
		if err := e.checkCI(context.Background(), st, f, Platform{}, publishedRun(testFailed, scanRunning), nil); err != nil {
			t.Fatal(err)
		}
		if len(f.reads) != 0 || len(st.Calls()) != 0 {
			t.Errorf("reads %d, store calls %v; want none", len(f.reads), st.Calls())
		}
	})

	t.Run("a read error", func(t *testing.T) {
		e, _ := ciEngine()
		st := &memCI{republish: true}
		f := &ciForge{err: errors.New("gitlab: 502")}
		if err := e.checkCI(context.Background(), st, f, Platform{}, publishedRun(testFailed, scanRunning), ciJobsSet.Jobs); err == nil {
			t.Fatal("checkCI = nil, want the read error")
		}
		if len(st.Calls()) != 0 {
			t.Errorf("store calls = %v, want none", st.Calls())
		}
	})
}

// A run in any other state is left alone.
func TestCheckCILeavesOtherRuns(t *testing.T) {
	for _, r := range []store.Run{
		func() store.Run { r := ciRun(); r.State, r.SkipReason = store.Waiting, store.WaitUntrusted; return r }(),
		func() store.Run { r := ciRun(); r.State = store.Superseded; return r }(),
		func() store.Run { r := ciRun(); r.State = store.Queued; return r }(),
	} {
		e, _ := ciEngine()
		st := &memCI{released: true, republish: true}
		f := &ciForge{snap: snapshot(true, ci.Job{Result: ci.Passed, Ended: true}, ci.Job{Result: ci.Passed, Ended: true})}
		if err := e.checkCI(context.Background(), st, f, Platform{}, r, ciJobsSet.Jobs); err != nil {
			t.Fatal(err)
		}
		if len(st.Calls()) != 0 || len(f.reads) != 0 {
			t.Errorf("%s/%s: store calls %v, reads %d; want none", r.State, r.SkipReason, st.Calls(), len(f.reads))
		}
	}
}

// The published reviews are checked again on every ciRecheckEvery-th sweep,
// the first (at start) included.
func TestCISweepRechecksEveryFifth(t *testing.T) {
	e, _ := ciEngine()
	var got []bool
	for range 11 {
		got = append(got, e.ciSweepRechecks())
	}
	want := []bool{true, false, false, false, false, true, false, false, false, false, true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rechecks = %v, want %v", got, want)
	}
}

// A sweep stops at its budget: the run being checked is cut off, the rest is
// left for the next sweep, and running out of time is no error.
func TestCISweepStopsAtBudget(t *testing.T) {
	old := ciSweepBudget
	ciSweepBudget = 50 * time.Millisecond
	t.Cleanup(func() { ciSweepBudget = old })
	e, logs := ciEngine()
	runs := []store.Run{ciRun(), ciRun(), ciRun()}
	runs[1].ID, runs[2].ID = "22222222-2222-2222-2222-222222222222", "33333333-3333-3333-3333-333333333333"
	var checked []string
	e.sweepCIRuns(context.Background(), runs, func(ctx context.Context, r store.Run) error {
		checked = append(checked, r.ID)
		<-ctx.Done() // a platform call that hangs until the budget ends
		return ctx.Err()
	})
	if len(checked) != 1 {
		t.Errorf("checked %v, want only the first run", checked)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a sweep out of time was logged as an error:\n%s", logs.String())
	}
}

// A run's failure is logged as an error and the sweep goes on; a shutdown
// stops it quietly.
func TestCISweepLogsFailuresAndStopsOnShutdown(t *testing.T) {
	e, logs := ciEngine()
	runs := []store.Run{ciRun(), ciRun()}
	var n int
	e.sweepCIRuns(context.Background(), runs, func(context.Context, store.Run) error {
		n++
		return errors.New("gitlab: 502")
	})
	if n != 2 || !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("checked %d runs, logs:\n%s; want both checked and the failures logged", n, logs.String())
	}

	e, logs = ciEngine()
	ctx, cancel := context.WithCancel(context.Background())
	n = 0
	e.sweepCIRuns(ctx, runs, func(context.Context, store.Run) error {
		n++
		cancel()
		return context.Canceled
	})
	if n != 1 || strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("checked %d runs, logs:\n%s; want one, quietly", n, logs.String())
	}
}

// The sweep checks a published review again only when a job that didn't pass
// may still change: one whose text read failed is kept as it is.
func TestCISweepWorth(t *testing.T) {
	textFailed := store.CIJobResult{Name: "test", Result: "unreadable", Reason: "error", Read: "log", Blocks: true, TextFailed: true}
	passed := store.CIJobResult{Name: "cycode-scan", Result: "passed", Read: "status"}
	failed := store.CIJobResult{Name: "lint", Result: "failed", Read: "status"}
	for _, tc := range []struct {
		name string
		jobs []store.CIJobResult
		want bool
	}{
		{"only failed text reads left", []store.CIJobResult{textFailed, passed}, false},
		{"a failed job too", []store.CIJobResult{textFailed, passed, failed}, true},
		{"a failed job", []store.CIJobResult{failed}, true},
		{"all passed", []store.CIJobResult{passed}, false},
	} {
		if got := ciSweepWorth(tc.jobs); got != tc.want {
			t.Errorf("%s: ciSweepWorth = %v, want %v", tc.name, got, tc.want)
		}
	}
}
