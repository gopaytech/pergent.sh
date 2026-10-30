package engine

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// Run first puts back a run stranded between claim and dispatch by an earlier
// stop, then dispatches it, and returns once its context is done.
func TestRunRecoversStrandedRunAndStops(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(1, head)
	if _, err := env.Store.ClaimQueued(env.ctx, 10, env.Now()); err != nil {
		t.Fatal(err)
	}
	env.GitLab.SetMR(openMR(1, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	// The clock runs ahead of the claim, as it would after a restart, and of
	// the database's now(), which the recovered run is due at.
	env.SetNow(env.Now().Add(time.Minute))

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
			t.Fatalf("the stranded run was not dispatched; run = %+v\n%s", env.Run(id), env.Logs())
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// Run reconciles once at start, from its own goroutine.
func TestRunReconcilesAtStart(t *testing.T) {
	env := newTestEnv(t)
	serveProject(env, testProjectID, testProjectPath)

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		env.Engine.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for len(env.GitLab.Requests(http.MethodPost, "/projects/7/hooks")) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Run didn't reconcile at start:\n%s", env.Logs())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if p, _ := project(t, env, testProjectID); p.HookID != newHookID {
		t.Errorf("hook id = %d, want %d", p.HookID, newHookID)
	}
}

// Run sweeps the approvals once at start, so a push missed while pergent.sh
// was down withdraws the approval without waiting for the watch tick.
func TestRunSweepsApprovalsAtStart(t *testing.T) {
	env := newTestEnv(t)
	approved(t, env, 1, sha('a'))
	env.GitLab.SetMR(openMR(1, sha('c')))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		env.Engine.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Run didn't sweep the approvals at start:\n%s", env.Logs())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

// runFor runs the engine for d, then stops it.
func runFor(t *testing.T, env *testEnv, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(env.ctx)
	done := make(chan struct{})
	go func() {
		env.Engine.Run(ctx)
		close(done)
	}()
	time.Sleep(d)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// setIntervals shortens Run's loop intervals for one test.
func setIntervals(t *testing.T, watch, sweep time.Duration) {
	t.Helper()
	oldWatch, oldSweep := watchInterval, sweepInterval
	watchInterval, sweepInterval = watch, sweep
	t.Cleanup(func() { watchInterval, sweepInterval = oldWatch, oldSweep })
}

// The approval sweep reads every approved MR, so it runs on its own interval
// (5 minutes) and not on the 15 s watch tick: about 20 approved PRs would
// otherwise use up GitHub's rate limit.
func TestRunSweepsOnItsOwnInterval(t *testing.T) {
	if sweepInterval != 5*time.Minute {
		t.Errorf("sweepInterval = %v, want 5m", sweepInterval)
	}

	t.Run("not on the watch tick", func(t *testing.T) {
		env := newTestEnv(t)
		approved(t, env, 1, sha('a'))
		env.GitLab.SetMR(openMR(1, sha('a')))
		setIntervals(t, 10*time.Millisecond, time.Hour)

		runFor(t, env, 300*time.Millisecond)

		// The one at startup only.
		if n := len(env.GitLab.Requests(http.MethodGet, mrPath(1))); n != 1 {
			t.Errorf("swept %d times in ~30 watch ticks, want 1 (at startup)", n)
		}
	})

	t.Run("on its own ticker", func(t *testing.T) {
		env := newTestEnv(t)
		approved(t, env, 1, sha('a'))
		env.GitLab.SetMR(openMR(1, sha('a')))
		setIntervals(t, time.Hour, 20*time.Millisecond)

		runFor(t, env, 300*time.Millisecond)

		if n := len(env.GitLab.Requests(http.MethodGet, mrPath(1))); n < 3 {
			t.Errorf("swept %d times in ~15 sweep intervals, want several", n)
		}
	})
}

// ReconcileSoon never blocks, however often it is called with no loop
// draining it.
func TestReconcileSoonNeverBlocks(t *testing.T) {
	e := &Engine{}
	done := make(chan struct{})
	go func() {
		for range 100 {
			e.ReconcileSoon()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReconcileSoon blocked")
	}
}

// ReconcileSoon makes the running loop reconcile on its next select, long
// before the hourly tick: a repository switched on in the dashboard gets its
// webhook within seconds.
func TestRunReconcilesSoonOnRequest(t *testing.T) {
	env := newTestEnv(t)
	serveProject(env, testProjectID, testProjectPath)
	serveProject(env, 9, "acme/gadgets")

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		env.Engine.Run(ctx)
		close(done)
	}()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s:\n%s", what, env.Logs())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// The startup reconcile has stored acme/widgets: it is done with the
	// list of repositories.
	waitFor("Run didn't reconcile at start", func() bool {
		p, ok := project(t, env, testProjectID)
		return ok && p.HookID == newHookID
	})

	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", CreatedBy: "alice"})
	time.Sleep(100 * time.Millisecond)
	if n := len(env.GitLab.Requests(http.MethodPost, "/projects/9/hooks")); n != 0 {
		t.Fatalf("acme/gadgets got a webhook before ReconcileSoon")
	}
	env.Engine.ReconcileSoon()
	waitFor("ReconcileSoon didn't reconcile acme/gadgets", func() bool {
		return len(env.GitLab.Requests(http.MethodPost, "/projects/9/hooks")) > 0
	})
	cancel()
	<-done
}
