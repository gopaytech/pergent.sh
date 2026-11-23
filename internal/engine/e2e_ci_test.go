package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// [CI] These need the test database, like every test that builds a testEnv.

// ciTestLog is the end of the test job's log.
const ciTestLog = "--- FAIL: TestWidget (0.01s)\nwidget_test.go:12: got 1, want 2\n"

// ciEnv is a GitLab environment whose repository waits up to 30 minutes for
// its job test (log read, blocking). Merge request 3 accepts statuses and
// notes.
func ciEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	env.Engine.Config.Repositories[0].CIJobs = &store.CIJobs{WaitSeconds: 1800, Jobs: []store.CIJob{
		{Name: "test", Read: "log", BlocksApproval: true},
	}}
	env.GitLab.Reply(http.MethodPost, notesPath(3), http.StatusCreated, `{"id": 900}`)
	return env
}

// serveCI serves one pipeline on head (pipeline status pipe) whose job test
// has status job, with ciTestLog as its log.
func serveCI(env *testEnv, head, pipe, job string) {
	env.GitLab.Reply(http.MethodGet, "/projects/7/pipelines", http.StatusOK,
		fmt.Sprintf(`[{"id":1,"sha":%q,"ref":"feature","status":%q}]`, head, pipe))
	env.GitLab.Reply(http.MethodGet, mrPath(3)+"/pipelines", http.StatusOK, `[]`)
	env.GitLab.Reply(http.MethodGet, "/projects/7/pipelines/1/jobs", http.StatusOK,
		fmt.Sprintf(`[{"id":5,"name":"test","status":%q}]`, job))
	env.GitLab.Reply(http.MethodGet, "/projects/7/pipelines/1/bridges", http.StatusOK, `[]`)
	env.GitLab.Handle(http.MethodGet, "/projects/7/jobs/5/trace", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(ciTestLog))
	})
}

// wantCIHeld checks run id waits for CI, with no Job.
func wantCIHeld(t *testing.T, env *testEnv, id string) store.Run {
	t.Helper()
	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitCIPending {
		t.Fatalf("run = %s/%q, want waiting/%s; logs:\n%s", r.State, r.SkipReason, store.WaitCIPending, env.Logs())
	}
	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Fatalf("%d jobs were created", len(jobs))
	}
	return r
}

// ciFile is the ci-results.md the Job of run id was given ("" when none).
func ciFile(env *testEnv, id string) string { return env.ConfigMap(id).Data["ci-results.md"] }

// A push waits while its chosen job runs: no Job, the CI pending status and
// the deadline. The sweep releases it once the job ended, and the review gets
// the job's result and log.
func TestCIWaitHoldsThenReleasesWhenDone(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	servePush(env, head)
	serveCI(env, head, "running", "running")
	id := env.Enqueue(3, head)
	dispatch(t, env)

	r := wantCIHeld(t, env, id)
	if want := r.CreatedAt.Add(30 * time.Minute); r.CIDeadline == nil || !r.CIDeadline.Equal(want) {
		t.Errorf("deadline = %v, want %v", r.CIDeadline, want)
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "pending: Waiting for CI: test" {
		t.Errorf("statuses = %v", got)
	}
	if n := len(notes(env, 3)); n != 0 {
		t.Errorf("%d notes, want none", n)
	}

	// Still running: the sweep leaves it.
	env.Engine.sweepCI(env.ctx)
	wantCIHeld(t, env, id)

	serveCI(env, head, "failed", "failed")
	env.Engine.sweepCI(env.ctx)
	r = env.Run(id)
	if r.State != store.Queued || !r.CIChecked {
		t.Fatalf("after the sweep run = %s (ci_checked %v), want queued and checked; logs:\n%s", r.State, r.CIChecked, env.Logs())
	}
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
	file := ciFile(env, id)
	if !strings.Contains(file, "## test: failed (blocks approval)") || !strings.Contains(file, "got 1, want 2") {
		t.Errorf("ci-results.md = %q", file)
	}
	r = env.Run(id)
	if len(r.CIJobs) != 1 || r.CIJobs[0].Result != "failed" || r.CIJobs[0].Size == 0 {
		t.Errorf("snapshot = %+v", r.CIJobs)
	}
	if strings.Contains(env.Logs(), "got 1, want 2") {
		t.Errorf("CI text reached the log:\n%s", env.Logs())
	}
}

// The deadline releases a run whose job never ends, and the review sees the
// job as running.
func TestCIDeadlineReleases(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	servePush(env, head)
	serveCI(env, head, "running", "running")
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantCIHeld(t, env, id)

	env.SetNow(env.Now().Add(31 * time.Minute))
	env.GitLab.Reset()
	env.Engine.sweepCI(env.ctx)
	if r := env.Run(id); r.State != store.Queued {
		t.Fatalf("run = %s, want queued; logs:\n%s", r.State, env.Logs())
	}
	// Past the deadline the sweep doesn't read CI.
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/pipelines")); n != 0 {
		t.Errorf("the sweep read CI %d times past the deadline", n)
	}
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
	if file := ciFile(env, id); !strings.Contains(file, "## test: running (blocks approval)") {
		t.Errorf("ci-results.md = %q", file)
	}
}

// pergent.sh down past a run's deadline: the sweep at start releases it, and
// the first pass reviews it.
func TestCIDeadlinePassedWhileDownReleasesAtStart(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	servePush(env, head)
	serveCI(env, head, "running", "running")
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantCIHeld(t, env, id)
	env.SetNow(env.Now().Add(2 * time.Hour))

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		env.Engine.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for env.Job(id) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the run past its deadline was not reviewed; run = %+v\n%s", env.Run(id), env.Logs())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

// Emptying the repository's ci_jobs releases the runs that wait, and they
// review as without CI jobs.
func TestCIJobsEmptiedReleases(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	servePush(env, head)
	serveCI(env, head, "running", "running")
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantCIHeld(t, env, id)

	env.Engine.Config.Repositories[0].CIJobs = nil
	env.GitLab.Reset()
	env.Engine.sweepCI(env.ctx)
	if r := env.Run(id); r.State != store.Queued {
		t.Fatalf("run = %s, want queued; logs:\n%s", r.State, env.Logs())
	}
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
	if _, ok := env.ConfigMap(id).Data["ci-results.md"]; ok {
		t.Errorf("the Job got ci-results.md with no jobs chosen")
	}
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/pipelines")); n != 0 {
		t.Errorf("CI was read %d times with no jobs chosen", n)
	}
}

// Start review skips the rest of the wait and uses the results there are.
func TestStartReviewSkipsCIWait(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	servePush(env, head)
	serveCI(env, head, "running", "running")
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantCIHeld(t, env, id)

	got, started, err := env.Store.StartRun(env.ctx, env.MRKey(3), testProjectPath, head, "main")
	if err != nil || !started || got != id {
		t.Fatalf("StartRun = %s, %v, %v", got, started, err)
	}
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
	if file := ciFile(env, id); !strings.Contains(file, "## test: running (blocks approval)") {
		t.Errorf("ci-results.md = %q", file)
	}
	if got := statusesOf(t, env, head); len(got) != 2 || got[0] != "pending: Waiting for CI: test" || got[1] != "running: "+runningDescription {
		t.Errorf("statuses = %v", got)
	}
}

// A new push supersedes a run waiting for CI, and the sweep never revives it.
func TestPushSupersedesCIPending(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	servePush(env, head)
	serveCI(env, head, "running", "running")
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantCIHeld(t, env, id)

	head2 := sha('c')
	servePush(env, head2)
	id2 := env.Enqueue(3, head2)
	if r := env.Run(id); r.State != store.Superseded {
		t.Fatalf("first head = %s, want superseded", r.State)
	}
	serveCI(env, head, "success", "success")
	env.Engine.sweepCI(env.ctx)
	if r := env.Run(id); r.State != store.Superseded {
		t.Errorf("after the sweep the first head = %s, want superseded", r.State)
	}
	if r := env.Run(id2); r.State != store.Queued {
		t.Errorf("second head = %s, want queued", r.State)
	}
}

// An untrusted author's run started by someone checks CI once: it waits for
// its jobs (not through postpone, again and again), and once released it is
// reviewed without another wait.
func TestStartedUntrustedRunChecksCIOnce(t *testing.T) {
	t.Parallel()
	head := sha('a')
	env := trustEnv(t, 3, head, 20)
	env.Engine.Config.Repositories[0].CIJobs = &store.CIJobs{WaitSeconds: 1800, Jobs: []store.CIJob{{Name: "test", Read: "status"}}}
	serveCI(env, head, "running", "running")
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitUntrusted {
		t.Fatalf("run = %s/%q, want waiting for trust", r.State, r.SkipReason)
	}
	// The trust hold comes first: CI isn't read for a run nobody started.
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/pipelines")); n != 0 {
		t.Errorf("CI was read %d times before the start", n)
	}

	if _, _, err := env.Store.StartRun(env.ctx, env.MRKey(3), testProjectPath, head, "main"); err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	wantCIHeld(t, env, id)
	// Waiting, it is not claimed again.
	env.GitLab.Reset()
	dispatch(t, env)
	wantCIHeld(t, env, id)
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/pipelines")); n != 0 {
		t.Errorf("a waiting run's CI was read %d times by the dispatcher", n)
	}

	serveCI(env, head, "success", "success")
	env.Engine.sweepCI(env.ctx)
	env.GitLab.Reset()
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "running: "+runningDescription {
		t.Errorf("statuses after the release = %v, want only the running one", got)
	}
}

// With no jobs chosen a run is reviewed as before: no CI read, no CI file,
// no CI snapshot, the running status only.
func TestNoCIJobsUnchanged(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
	if _, ok := env.ConfigMap(id).Data["ci-results.md"]; ok {
		t.Errorf("the Job got ci-results.md")
	}
	for _, c := range env.Job(id).Spec.Template.Spec.Containers {
		for _, v := range c.Env {
			if v.Name == "PERGENT_CI_RESULTS" {
				t.Errorf("container %s has PERGENT_CI_RESULTS", c.Name)
			}
		}
	}
	for _, path := range []string{"/projects/7/pipelines", mrPath(3) + "/pipelines"} {
		if n := len(env.GitLab.Requests(http.MethodGet, path)); n != 0 {
			t.Errorf("GET %s %d times", path, n)
		}
	}
	r := env.Run(id)
	if r.CIJobs != nil || r.CIChecked {
		t.Errorf("run CI = %+v (checked %v), want none", r.CIJobs, r.CIChecked)
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "running: "+runningDescription {
		t.Errorf("statuses = %v", got)
	}
}

// When the inputs are too large for the CI results, the review goes on
// without them, with a warning; the snapshot is still recorded for the gate.
func TestCIResultsDroppedByFitWarns(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	// The skill leaves less room than the CI file needs.
	env.AddSkill("huge", string(bytes.Repeat([]byte("x"), inputs.ConfigMapLimit-40)))
	env.SetDefaultSkills("huge")
	head := sha('a')
	servePush(env, head)
	serveCI(env, head, "failed", "failed")
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
	if _, ok := env.ConfigMap(id).Data["ci-results.md"]; ok {
		t.Errorf("the Job got ci-results.md")
	}
	if !strings.Contains(env.Logs(), "reviewing without the CI job results") {
		t.Errorf("no warning; logs:\n%s", env.Logs())
	}
	if r := env.Run(id); len(r.CIJobs) != 1 || r.CIJobs[0].Result != "failed" {
		t.Errorf("snapshot = %+v", r.CIJobs)
	}
}

// A published review whose blocking job failed is decided again when the job
// passes later: the run goes back to be published, with the new snapshot.
func TestCISweepRedecidesPublished(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	env.GitLab.SetMR(openMR(3, head))
	id := env.Publish(3, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetRunCI(env.ctx, id, []store.CIJobResult{{Name: "test", Result: "failed", Read: "log", Size: 30, Blocks: true}}); err != nil {
		t.Fatal(err)
	}
	serveCI(env, head, "success", "success")
	env.Engine.sweepCI(env.ctx)
	r := env.Run(id)
	if r.State != store.Reported {
		t.Fatalf("run = %s, want reported to be decided again; logs:\n%s", r.State, env.Logs())
	}
	if want := []store.CIJobResult{{Name: "test", Result: "passed", Read: "log", Size: 30, Blocks: true}}; len(r.CIJobs) != 1 || r.CIJobs[0] != want[0] {
		t.Errorf("snapshot = %+v, want %+v", r.CIJobs, want)
	}
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/jobs/5/trace")); n != 0 {
		t.Errorf("the re-check read the log %d times", n)
	}
}

// A published review whose only job that didn't pass failed its report read
// is not checked again by the sweep: a status-only read can't say the report
// could be read now.
func TestCISweepSkipsTextFailedOnly(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	env.GitLab.SetMR(openMR(3, head))
	id := env.Publish(3, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetRunCI(env.ctx, id, []store.CIJobResult{{Name: "test", Result: "unreadable", Reason: "error", Read: "report", Blocks: true, TextFailed: true}}); err != nil {
		t.Fatal(err)
	}
	serveCI(env, head, "success", "success")
	env.Engine.sweepCI(env.ctx)
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/pipelines")); n != 0 {
		t.Errorf("CI was read %d times", n)
	}
	if r := env.Run(id); r.State != store.Published {
		t.Errorf("run = %s, want published", r.State)
	}
}

// A published review whose log job failed its log read is checked again by the
// sweep, and the job turns passed once it passes: a passed job's log is never
// read, so the review lacked nothing.
func TestCISweepFlipsTextFailedLog(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	env.GitLab.SetMR(openMR(3, head))
	id := env.Publish(3, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetRunCI(env.ctx, id, []store.CIJobResult{{Name: "test", Result: "unreadable", Reason: "error", Read: "log", Blocks: true, TextFailed: true}}); err != nil {
		t.Fatal(err)
	}
	serveCI(env, head, "success", "success")
	env.Engine.sweepCI(env.ctx)
	r := env.Run(id)
	if r.State != store.Reported {
		t.Fatalf("run = %s, want reported to be decided again; logs:\n%s", r.State, env.Logs())
	}
	if len(r.CIJobs) != 1 || r.CIJobs[0].Result != "passed" || r.CIJobs[0].TextFailed {
		t.Errorf("snapshot = %+v, want passed with TextFailed cleared", r.CIJobs)
	}
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/jobs/5/trace")); n != 0 {
		t.Errorf("the re-check read the log %d times", n)
	}
}

// A CI event releases a waiting run at once, with no sweep.
func TestCIEventReleasesWaitingRun(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	servePush(env, head)
	serveCI(env, head, "running", "running")
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantCIHeld(t, env, id)

	serveCI(env, head, "success", "success")
	env.Engine.CIChanged("main", "acme/widgets", head)
	env.Engine.checkCIEvent(env.ctx, <-env.Engine.ciCh())
	if r := env.Run(id); r.State != store.Queued || !r.CIChecked {
		t.Fatalf("run = %s (ci_checked %v), want queued and checked; logs:\n%s", r.State, r.CIChecked, env.Logs())
	}
}

// An all-passed published review is decided again by an event after a retried
// job fails: the sweep skips such a review, an event does not.
func TestCIEventRedecidesAllPassedPublished(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	env.GitLab.SetMR(openMR(3, head))
	id := env.Publish(3, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetRunCI(env.ctx, id, []store.CIJobResult{{Name: "test", Result: "passed", Read: "log", Size: 30, Blocks: true}}); err != nil {
		t.Fatal(err)
	}
	serveCI(env, head, "failed", "failed")
	env.Engine.CIChanged("main", "acme/widgets", head)
	env.Engine.checkCIEvent(env.ctx, <-env.Engine.ciCh())
	r := env.Run(id)
	if r.State != store.Reported {
		t.Fatalf("run = %s, want reported to be decided again; logs:\n%s", r.State, env.Logs())
	}
	if len(r.CIJobs) != 1 || r.CIJobs[0].Result != "failed" {
		t.Errorf("snapshot = %+v, want failed", r.CIJobs)
	}
}

// A merged-results event names the merge request, whose head the run has.
func TestCIEventByMergeRequest(t *testing.T) {
	t.Parallel()
	env := ciEnv(t)
	head := sha('a')
	env.GitLab.SetMR(openMR(3, head))
	id := env.Publish(3, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetRunCI(env.ctx, id, []store.CIJobResult{{Name: "test", Result: "passed", Read: "log", Size: 30, Blocks: true}}); err != nil {
		t.Fatal(err)
	}
	serveCI(env, head, "failed", "failed")
	env.Engine.CIChangedMR("main", "acme/widgets", 3)
	env.Engine.checkCIEvent(env.ctx, <-env.Engine.ciCh())
	if r := env.Run(id); r.State != store.Reported {
		t.Fatalf("run = %s, want reported; logs:\n%s", r.State, env.Logs())
	}
}
