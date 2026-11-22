package engine

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// startRunOld does what the StartRun of a release before review panels does
// to a waiting run: it queues it, keeping its dispatched_at.
func startRunOld(t *testing.T, env *testEnv, id string) {
	t.Helper()
	execSQL(t, env, fmt.Sprintf(`UPDATE runs SET state = 'queued', trigger = 'start', skip_reason = '',
		not_before = NULL WHERE id = '%s'`, id))
}

// mergeJobs is the number of Job P of run id.
func mergeJobs(env *testEnv, id string) int {
	n := 0
	for _, j := range env.Jobs() {
		if j.Labels[kube.LabelPanelRole] == kube.PanelRoleMerge && j.Labels["pergent.sh/run"] == id {
			n++
		}
	}
	return n
}

// wantMembersLive checks the members are still those given, in state.
func wantMembersLive(t *testing.T, env *testEnv, id string, state store.MemberState, ms ...store.PanelMember) {
	t.Helper()
	for _, m := range ms {
		if got := memberOfRole(t, env, id, m.Role); got.ID != m.ID || got.State != state {
			t.Errorf("member %s = %s %s; want %s %s", m.Role, got.ID, got.State, m.ID, state)
		}
	}
}

// During a rolling deploy an older pod's StartRun queues a panel run whose
// members still run. The dispatch starts no Job P: the run waits for its
// members again, and the sweep leaves them alone. Once they are done the
// release queues the run, and Job P starts once.
func TestE2EPanelMergeWaitsForLiveMembers(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, a, b := startMembers(t, e, head)
	before := env.Run(id)

	startRunOld(t, env, id)
	dispatch(t, env)
	if n := mergeJobs(env, id); n != 0 {
		t.Fatalf("%d Job P while the members run; want none\n%s", n, env.Logs())
	}
	wantWaiting(t, env, id)
	if r := env.Run(id); r.JobName != "" || !r.DispatchedAt.Equal(before.DispatchedAt) {
		t.Errorf("run = job %q dispatched %v; want no Job, dispatched at the split (%v)", r.JobName, r.DispatchedAt, before.DispatchedAt)
	}
	cleanup(t, env)
	wantMembersLive(t, env, id, store.MemberRunning, a, b)

	for _, m := range []store.PanelMember{a, b} {
		if code := e.postMember(t, id, m, clean(t, head)); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d\n%s", m.Role, code, env.Logs())
		}
	}
	publish(t, env)
	if r := env.Run(id); r.State != store.Queued {
		t.Fatalf("run = %s; want released for Job P\n%s", r.State, env.Logs())
	}
	dispatch(t, env)
	dispatch(t, env)
	if n := mergeJobs(env, id); n != 1 {
		t.Fatalf("%d Job P; want one\n%s", n, env.Logs())
	}
	if js := panelJudges(t, env, id); len(js) != 2 {
		t.Errorf("judges = %+v; want both roles", js)
	}
}

// The same for an API review: the dispatch starts no Job P while the
// members run, and the stored files stay.
func TestE2EAPIPanelMergeWaitsForLiveMembers(t *testing.T) {
	a, _ := newAPIPanelE2E(t, "b")
	env := a.env
	id, im := a.splitAPI(t, "route:1")
	dispatch(t, env)
	ma, mb := memberOfRole(t, env, id, "a"), memberOfRole(t, env, id, "b")

	startRunOld(t, env, id)
	dispatch(t, env)
	if n := mergeJobs(env, id); n != 0 {
		t.Fatalf("%d Job P while the members run; want none\n%s", n, env.Logs())
	}
	wantWaiting(t, env, id)
	cleanup(t, env)
	wantMembersLive(t, env, id, store.MemberRunning, ma, mb)
	wantFiles(t, env, id, true, "after the early dispatch")

	clean := apiResultFor(t, im.Base, im.Head, reviewed("no_blocking_issues", "routes/api.yaml", "routes/new.yaml"))
	for _, m := range []store.PanelMember{ma, mb} {
		if code := a.postMember(t, id, m, clean); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d\n%s", m.Role, code, env.Logs())
		}
	}
	publish(t, env)
	dispatch(t, env)
	dispatch(t, env)
	if n := mergeJobs(env, id); n != 1 {
		t.Fatalf("%d Job P; want one\n%s", n, env.Logs())
	}
	if js := panelJudges(t, env, id); len(js) != 2 {
		t.Errorf("judges = %+v; want both roles", js)
	}
}
