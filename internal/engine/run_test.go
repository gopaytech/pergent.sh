package engine

import (
	"context"
	"net/http"
	"testing"
	"time"
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
