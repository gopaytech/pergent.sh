package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// savePanelChain adds one entry per name to the team that owns acme/widgets,
// each with the key "key-" + name, saves them as the team's chain B and
// returns their refs.
func savePanelChain(t *testing.T, env *testEnv, names ...string) []store.EntryRef {
	t.Helper()
	team := ownerTeam(t, env)
	var refs []store.EntryRef
	for _, n := range names {
		refs = append(refs, addTeamModel(t, env, team, n, "gpt-"+n, "key-"+n))
	}
	saveChainB(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: refs})
	return refs
}

// ownerTeam is the team that owns acme/widgets.
func ownerTeam(t *testing.T, env *testEnv) int64 {
	t.Helper()
	team, ok, err := env.Store.OwnerTeam(env.ctx, testInstance, testProjectPath)
	if err != nil || !ok {
		t.Fatalf("OwnerTeam = %v, %v", ok, err)
	}
	return team
}

// newPanelE2E is newFallbackE2E with chain A of the names a and chain B of
// the names b, on an image that has the panel.
func newPanelE2E(t *testing.T, a, b []string) *e2e {
	t.Helper()
	panelFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, a...)
	e.env.Engine.ScanImage = testScanImage
	savePanelChain(t, e.env, b...)
	return e
}

// splitPanel queues a run of head on merge request iid, serves it and
// dispatches once: the run is split. It returns the run.
func splitPanel(t *testing.T, env *testEnv, iid int64, head string) string {
	t.Helper()
	servePublish(env, iid, head)
	id := env.Enqueue(iid, head)
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitPanel {
		t.Fatalf("run = %s/%q; want a panel\n%s", r.State, r.SkipReason, env.Logs())
	}
	if n := len(env.Jobs()); n != 0 {
		t.Fatalf("%d Jobs after the split; want none", n)
	}
	return id
}

// memberOfRole is the newest member of role of run id's review.
func memberOfRole(t *testing.T, env *testEnv, id, role string) store.PanelMember {
	t.Helper()
	var out store.PanelMember
	for _, m := range members(t, env, id) {
		if m.Role == role && m.State != store.MemberOff {
			out = m
		}
	}
	if out.ID == "" {
		t.Fatalf("no member %s of %s", role, id)
	}
	return out
}

// wantMemberKey checks member m has a Job whose Secret holds key.
func wantMemberKey(t *testing.T, env *testEnv, m store.PanelMember, key string) {
	t.Helper()
	if env.Job(m.ID) == nil {
		t.Fatalf("member %s has no Job (%+v)\n%s", m.Role, m, env.Logs())
	}
	if got := string(env.Secret(m.ID).Data["llm-api-key"]); got != key {
		t.Fatalf("member %s's key = %q, want %q", m.Role, got, key)
	}
}

// wantSuperseded checks run id and all its members are superseded, and no
// member has a Job.
func wantSuperseded(t *testing.T, env *testEnv, id string) {
	t.Helper()
	if r := env.Run(id); r.State != store.Superseded {
		t.Fatalf("run = %s; want superseded\n%s", r.State, env.Logs())
	}
	ms := members(t, env, id)
	if len(ms) != 2 {
		t.Fatalf("members = %+v; want 2", ms)
	}
	for _, m := range ms {
		if m.State != store.MemberSuperseded || m.JobName != "" {
			t.Errorf("member %s = %s job %q; want superseded with no Job", m.Role, m.State, m.JobName)
		}
	}
	if n := len(env.Jobs()); n != 0 {
		t.Errorf("%d Jobs; want none", n)
	}
}

// The split, then a member Job per role: each Secret holds its own entry's
// key alone, the Job skips the skill verification, its labels name the run,
// the member and the role, and it reports on the member's route. No key is
// in a label, a log or a ConfigMap. The running status is set once, and the
// run is not written.
func TestE2EPanelMembersStart(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id := splitPanel(t, env, 1, head)
	before := env.Run(id)

	dispatch(t, env)
	if n := len(env.Jobs()); n != 2 {
		t.Fatalf("%d Jobs; want the 2 members'\n%s", n, env.Logs())
	}
	keys := map[string]string{"a": "key-team-a", "b": "key-team-b"}
	for role, key := range keys {
		other := keys[map[string]string{"a": "b", "b": "a"}[role]]
		m := memberOfRole(t, env, id, role)
		if m.State != store.MemberRunning || m.JobName != kube.JobName(m.ID) || m.Entry.Name != "team-"+role ||
			m.Model != "gpt-team-"+role || m.DispatchedAt.IsZero() {
			t.Errorf("member %s = %+v; want running with its Job and entry", role, m)
		}
		wantMemberKey(t, env, m, key)
		sec := env.Secret(m.ID)
		if got := slices.Sorted(maps.Keys(sec.Data)); !slices.Equal(got, []string{"git-token", "llm-api-key", "result-token"}) {
			t.Errorf("member %s's Secret keys = %v; want one LLM key", role, got)
		}
		for k, v := range sec.Data {
			if strings.Contains(string(v), other) {
				t.Errorf("member %s's Secret %s holds the other member's key", role, k)
			}
		}
		job := env.Job(m.ID)
		args := job.Spec.Template.Spec.Containers[0].Args
		for _, want := range []string{"--no-skill-verify",
			"--result-url=" + testCallbackURL + "/runs/" + id + "/members/" + m.ID + "/result"} {
			if !hasArg(args, want) {
				t.Errorf("member %s's args %v lack %q", role, args, want)
			}
		}
		wantLabels := map[string]string{"pergent.sh/run": id, kube.LabelPanelMember: m.ID, kube.LabelPanelRole: role}
		for k, v := range wantLabels {
			if job.Labels[k] != v {
				t.Errorf("member %s's label %s = %q, want %q", role, k, job.Labels[k], v)
			}
		}
		meta := maps.Clone(job.Labels)
		maps.Copy(meta, job.Annotations)
		maps.Copy(meta, env.ConfigMap(m.ID).Data)
		for k, v := range meta {
			for _, key := range keys {
				if strings.Contains(k, key) || strings.Contains(v, key) {
					t.Errorf("member %s's label, annotation or ConfigMap %s holds a key", role, k)
				}
			}
		}
		// The Secret's token is the member's own.
		token := string(sec.Data["result-token"])
		if out, err := env.Store.StoreMemberResult(env.ctx, m.RunID, m.ID, token, clean(t, head), env.Now()); err != nil || out != store.ResultStored {
			t.Errorf("member %s's result = %v, %v; want stored", role, out, err)
		}
	}
	for _, key := range keys {
		if strings.Contains(env.Logs(), key) {
			t.Errorf("a key reached the log:\n%s", env.Logs())
		}
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "running: "+runningDescription {
		t.Errorf("statuses = %v; want the running status once", got)
	}
	if after := env.Run(id); !reflect.DeepEqual(before, after) {
		t.Errorf("the members wrote the run:\n before %+v\n after  %+v", before, after)
	}
}

// The checks a run passed before it was split are not done again: an author
// no longer trusted, a CI job still running and a used-up repository quota
// hold neither member.
func TestE2EPanelMemberSkipsChecks(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id := splitPanel(t, env, 3, head)

	env.Engine.Config.GitLab[0].TrustedAuthorsOnly = config.TrustOn
	mr := openMR(3, head)
	mr.Author = gitlab.User{ID: strangerID, Username: strangerName}
	env.GitLab.SetMR(mr)
	strangerMember(env, 0)
	env.GitLab.Reply(http.MethodPost, notesPath(3), http.StatusCreated, `{"id": 900}`)
	env.Engine.Config.Repositories[0].CIJobs = &store.CIJobs{WaitSeconds: 1800, Jobs: []store.CIJob{
		{Name: "test", Read: "log", BlocksApproval: true},
	}}
	serveCI(env, head, "running", "running")
	q := int64(100)
	env.Engine.Config.Repositories[0].MonthlyTokenQuota = &q
	seedQuotaUsage(t, env, 100)

	dispatch(t, env)
	wantMemberKey(t, env, memberOfRole(t, env, id, "a"), "key-team-a")
	wantMemberKey(t, env, memberOfRole(t, env, id, "b"), "key-team-b")
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitPanel {
		t.Errorf("run = %s/%q; want still waiting for its members", r.State, r.SkipReason)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(3))); n != 0 {
		t.Errorf("%d notes; want none", n)
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "running: "+runningDescription {
		t.Errorf("statuses = %v; want the running status alone", got)
	}
}

// Chain B's first entry is A's model (another key, another case): B runs on
// the next one, whichever member starts first.
func TestE2EPanelBSkipsAModel(t *testing.T) {
	for _, first := range []string{"a", "b"} {
		t.Run(first+" first", func(t *testing.T) {
			panelFloor(t, "v0.5.0")
			e, _ := newFallbackE2E(t, "team-a")
			env := e.env
			team := ownerTeam(t, env)
			same := addTeamModel(t, env, team, "team-same", "GPT-TEAM-A", "key-same")
			other := addTeamModel(t, env, team, "team-other", "gpt-other", "key-other")
			saveChainB(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{same, other}})
			id := splitPanel(t, env, 1, sha('a'))

			later := memberOfRole(t, env, id, map[string]string{"a": "b", "b": "a"}[first])
			execSQL(t, env, `UPDATE panel_members SET not_before = now() + interval '1 hour' WHERE id = '`+later.ID+`'`)
			dispatch(t, env)
			if n := len(env.Jobs()); n != 1 {
				t.Fatalf("%d Jobs; want member %s's alone", n, first)
			}
			env.SetNow(env.Now().Add(2 * time.Hour))
			dispatch(t, env)

			wantMemberKey(t, env, memberOfRole(t, env, id, "a"), "key-team-a")
			b := memberOfRole(t, env, id, "b")
			wantMemberKey(t, env, b, "key-other")
			if b.Entry.ID != other.ID || b.Entry.Position != 2 || b.Entry.FallbackClass != "same_model" {
				t.Errorf("B's entry = %+v; want the second, past A's model", b.Entry)
			}
		})
	}
}

// The head moves before the members start: a review of the new head is
// queued, the panel's run is superseded, and the sweep supersedes its
// members. No member Job starts.
func TestE2EPanelMemberHeadMoved(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head, moved := sha('a'), sha('c')
	id := splitPanel(t, env, 1, head)
	servePublish(env, 1, moved)

	dispatch(t, env)
	next, created, err := env.Store.EnqueueRun(env.ctx, env.MRKey(1), testProjectPath, moved, "main")
	if err != nil || created || next == id {
		t.Fatalf("EnqueueRun = %s, %v, %v; want the new head's run already there", next, created, err)
	}
	if r := env.Run(next); r.HeadSHA != moved || r.State == store.Superseded {
		t.Errorf("new head's run = %s %s; want a live run of the new head", r.HeadSHA, r.State)
	}
	// The sweep (CleanupOnce, S3.6) takes the members of a run that is gone.
	if _, err := env.Store.SupersedeOrphanMembers(env.ctx); err != nil {
		t.Fatal(err)
	}
	wantSuperseded(t, env, id)
	if got := statusesOf(t, env, head); len(got) != 0 {
		t.Errorf("statuses on the old head = %v; want none", got)
	}
}

// The merge request is closed before the members start: the panel's run and
// its members are superseded, and no member Job starts.
func TestE2EPanelMemberMRClosed(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id := splitPanel(t, env, 1, head)
	mr := openMR(1, head)
	mr.State = "closed"
	env.GitLab.SetMR(mr)

	dispatch(t, env)
	wantSuperseded(t, env, id)
	if got := statusesOf(t, env, head); len(got) != 0 {
		t.Errorf("statuses = %v; want none", got)
	}
}

// Member A's Job can't be created: it is tried 5 times with backoff, then
// fails. Only the member fails: the run still waits, and B's Job runs.
func TestE2EPanelMemberRetryGivesUp(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	id := splitPanel(t, env, 1, sha('a'))
	a := memberOfRole(t, env, id, "a")
	fails := 0
	env.Kube.PrependReactor("create", "jobs", func(act k8stesting.Action) (bool, runtime.Object, error) {
		if act.(k8stesting.CreateAction).GetObject().(*batchv1.Job).Name == kube.JobName(a.ID) {
			fails++
			return true, nil, errors.New("the API server is down")
		}
		return false, nil, nil
	})

	for i := 0; i < 6; i++ {
		dispatch(t, env)
		env.SetNow(env.Now().Add(time.Hour))
	}
	if fails != 5 {
		t.Errorf("%d launches of A; want 5", fails)
	}
	a = memberOfRole(t, env, id, "a")
	if a.State != store.MemberReported || !strings.HasPrefix(a.FailureReason, reasonCouldntStart) || a.JobName != "" {
		t.Errorf("member A = %s %q job %q; want reported as failed to start", a.State, a.FailureReason, a.JobName)
	}
	wantMemberKey(t, env, memberOfRole(t, env, id, "b"), "key-team-b")
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitPanel || r.FailureReason != "" {
		t.Errorf("run = %s/%q %q; want still waiting", r.State, r.SkipReason, r.FailureReason)
	}
}

// jobs.max_running 1: one member Job at a time. The second starts once the
// first has reported.
func TestE2EPanelMaxRunning(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	env.Engine.Config.Jobs.MaxRunning = 1
	head := sha('a')
	id := splitPanel(t, env, 1, head)

	dispatch(t, env)
	dispatch(t, env)
	if n := len(env.Jobs()); n != 1 {
		t.Fatalf("%d Jobs; want one", n)
	}
	var started store.PanelMember
	for _, m := range members(t, env, id) {
		if m.JobName != "" {
			started = m
		}
	}
	token := string(env.Secret(started.ID).Data["result-token"])
	if out, err := env.Store.StoreMemberResult(env.ctx, started.RunID, started.ID, token, clean(t, head), env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreMemberResult = %v, %v", out, err)
	}
	dispatch(t, env)
	if n := len(env.Jobs()); n != 2 {
		t.Errorf("%d Jobs; want the second member's too", n)
	}
}

// A member's Job gets the review's rules and built-in switch, as the review
// Job does: rules.json in its ConfigMap, PERGENT_RULES and
// PERGENT_NO_BUILT_IN_RULES in its env.
func TestE2EPanelMemberRules(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	off := false
	env.Engine.Config.Repositories[0].RuleSets = []string{"a"}
	env.Engine.Config.Repositories[0].BuiltInRules = &off
	addRuleSet(t, env, "a", setAYAML)
	env.GitLab.Reply(http.MethodGet, filePath(rule.RepoFile), http.StatusOK, repoRulesYAML)
	id := splitPanel(t, env, 1, sha('a'))

	dispatch(t, env)
	for _, role := range []string{"a", "b"} {
		m := memberOfRole(t, env, id, role)
		var labels []string
		for _, r := range jobRules(t, env, m.ID) {
			labels = append(labels, r.Label())
		}
		if want := []string{"repo:*.go", "dashboard:a:*.py"}; !slices.Equal(labels, want) {
			t.Errorf("member %s's rules = %v, want %v", role, labels, want)
		}
		if v, ok := jobVar(t, env, m.ID, "PERGENT_RULES"); !ok || v != "/in/rules.json" {
			t.Errorf("member %s's PERGENT_RULES = %q, %v", role, v, ok)
		}
		if v, ok := jobVar(t, env, m.ID, "PERGENT_NO_BUILT_IN_RULES"); !ok || v != "true" {
			t.Errorf("member %s's PERGENT_NO_BUILT_IN_RULES = %q, %v", role, v, ok)
		}
	}
}

// The panel's run is superseded while a member's Job starts: the Job is
// deleted and the member goes back to the queue, so it holds no
// jobs.max_running slot until the sweep supersedes it.
func TestE2EPanelMemberSupersededAtLaunch(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	id := splitPanel(t, env, 1, sha('a'))
	once := false
	env.Kube.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !once {
			once = true
			// A push of a new head supersedes the waiting run.
			if _, _, err := env.Store.EnqueueRun(env.ctx, env.MRKey(1), testProjectPath, sha('c'), "main"); err != nil {
				t.Error(err)
			}
		}
		return false, nil, nil
	})

	dispatch(t, env)
	if r := env.Run(id); r.State != store.Superseded {
		t.Fatalf("run = %s; want superseded", r.State)
	}
	for _, m := range members(t, env, id) {
		if m.State == store.MemberRunning || m.JobName != "" || env.Job(m.ID) != nil {
			t.Errorf("member %s = %s job %q; want back in the queue with no Job", m.Role, m.State, m.JobName)
		}
	}
	running, err := env.Store.RunningMembers(env.ctx)
	if err != nil || len(running) != 0 {
		t.Errorf("running members = %d, %v; want none", len(running), err)
	}
}

// With the code graph on, a member's Job has no graph (the graph route takes
// only the run's token), and each member logs that once, with no URL.
func TestE2EPanelMemberNoGraph(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	env.Engine.ScanImage = testScanImage
	env.Engine.GraphImage = testGraphImage
	env.Engine.GraphDir = t.TempDir()
	env.Engine.Config.Repositories = nil
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: testProjectPath, CreatedBy: "alice"})
	if _, _, err := env.Store.SetDashboardRepoGraph(env.ctx, testInstance, testProjectPath, true); err != nil {
		t.Fatal(err)
	}
	storeTestGraph(t, env, "main", sha('d'))
	id := splitPanel(t, env, 1, sha('a'))

	dispatch(t, env)
	for _, role := range []string{"a", "b"} {
		m := memberOfRole(t, env, id, role)
		job := env.Job(m.ID)
		if job == nil {
			t.Fatalf("member %s has no Job\n%s", role, env.Logs())
		}
		for _, c := range job.Spec.Template.Spec.InitContainers {
			if c.Image == testScanImage {
				t.Errorf("member %s's Job has the graph container %s", role, c.Name)
			}
		}
		if _, ok := jobVar(t, env, m.ID, "PERGENT_GRAPH"); ok {
			t.Errorf("member %s's Job reads a graph", role)
		}
	}
	logs := env.Logs()
	if n := strings.Count(logs, "the code graph is left out for a panel member"); n != 2 {
		t.Errorf("the graph note was logged %d times; want once per member\n%s", n, logs)
	}
	if strings.Contains(logs, "/graph") {
		t.Errorf("a graph URL reached the log:\n%s", logs)
	}
}

// postMember posts body as member m's result on its route, with its token,
// and returns the status.
func (e *e2e) postMember(t *testing.T, run string, m store.PanelMember, body []byte) int {
	t.Helper()
	token := string(e.env.Secret(m.ID).Data["result-token"])
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/runs/"+run+"/members/"+m.ID+"/result", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// startMembers splits a review of head on merge request 1 and starts both
// member Jobs. It returns the run and members A and B.
func startMembers(t *testing.T, e *e2e, head string) (string, store.PanelMember, store.PanelMember) {
	t.Helper()
	id := splitPanel(t, e.env, 1, head)
	dispatch(t, e.env)
	a, b := memberOfRole(t, e.env, id, "a"), memberOfRole(t, e.env, id, "b")
	wantMemberKey(t, e.env, a, "key-team-a")
	wantMemberKey(t, e.env, b, "key-team-b")
	return id, a, b
}

// replaceJob puts member or run id's Job back with the given pod and
// conditions (putJob), in place of the one dispatch created. The Secret
// stays.
func replaceJob(t *testing.T, env *testEnv, id string, podCreated time.Time, init, main []corev1.ContainerStatus, conds ...batchv1.JobCondition) {
	t.Helper()
	if err := env.Kube.BatchV1().Jobs(testNamespace).Delete(env.ctx, kube.JobName(id), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	putJob(t, env, id, podCreated, init, main, conds...)
}

// wantWaiting checks run id still waits for its members, with no failure.
func wantWaiting(t *testing.T, env *testEnv, id string) {
	t.Helper()
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitPanel || r.FailureReason != "" {
		t.Errorf("run = %s/%q %q; want still waiting for its members", r.State, r.SkipReason, r.FailureReason)
	}
}

// wantMemberFailure checks member m was reported with reason at step.
func wantMemberFailure(t *testing.T, env *testEnv, run string, m store.PanelMember, reason, step string) {
	t.Helper()
	got := memberOfRole(t, env, run, m.Role)
	if got.ID != m.ID || got.State != store.MemberReported || got.FailureReason != reason || got.FailureStep != step {
		t.Errorf("member %s = %s %q at %q; want reported with %q at %q", m.Role, got.State, got.FailureReason, got.FailureStep, reason, step)
	}
}

// Member A's Job vanishes: after the grace A fails with reasonDisappeared,
// the run still waits, B runs on, and nothing is posted.
func TestE2EPanelMemberGone(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, a, b := startMembers(t, e, head)
	before := env.Run(id)
	if err := env.Kube.BatchV1().Jobs(testNamespace).Delete(env.ctx, kube.JobName(a.ID), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	woken(env)

	env.SetNow(env.Now().Add(30 * time.Second))
	watch(t, env)
	if got := memberOfRole(t, env, id, "a"); got.State != store.MemberRunning {
		t.Errorf("member A = %s inside the grace; want running", got.State)
	}

	env.SetNow(env.Now().Add(31 * time.Second))
	env.GitLab.Reset()
	watch(t, env)
	wantMemberFailure(t, env, id, a, reasonDisappeared, kube.StepUnknown)
	if got := memberOfRole(t, env, id, "b"); got.ID != b.ID || got.State != store.MemberRunning {
		t.Errorf("member B = %s; want running", got.State)
	}
	if !woken(env) {
		t.Errorf("the engine was not woken after the member's report")
	}
	wantWaiting(t, env, id)
	if after := env.Run(id); !reflect.DeepEqual(before, after) {
		t.Errorf("the member's failure wrote the run:\n before %+v\n after  %+v", before, after)
	}
	if reqs := env.GitLab.Requests(http.MethodPost, ""); len(reqs) != 0 {
		t.Errorf("%d posts to the forge; want none", len(reqs))
	}
}

// Member B's Job hits its deadline: B fails with the deadline's reason once
// the grace is over; the run still waits.
func TestE2EPanelMemberDeadline(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	id, _, b := startMembers(t, e, sha('a'))
	if err := env.Store.AdvanceMemberCheckoutSeen(env.ctx, b.ID, 2); err != nil {
		t.Fatal(err)
	}
	finished := env.Now()
	if err := env.Kube.BatchV1().Jobs(testNamespace).Delete(env.ctx, kube.JobName(b.ID), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	putBareJob(t, env, b.ID, finished.Add(-time.Hour), jobCond(batchv1.JobFailed, "DeadlineExceeded", finished))

	env.SetNow(finished.Add(30 * time.Second))
	watch(t, env)
	if got := memberOfRole(t, env, id, "b"); got.State != store.MemberRunning {
		t.Errorf("member B = %s inside the grace; want running", got.State)
	}
	env.SetNow(finished.Add(61 * time.Second))
	watch(t, env)
	wantMemberFailure(t, env, id, b, "the review didn't finish before the deadline (DeadlineExceeded)", kube.StepReview)
	if got := memberOfRole(t, env, id, "a"); got.State != store.MemberRunning {
		t.Errorf("member A = %s; want running", got.State)
	}
	wantWaiting(t, env, id)
}

// Member A's Job is stuck on an image pull: it is deleted and A fails at the
// start step; the run still waits and no "Needs attention" item is raised.
func TestE2EPanelMemberStuck(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	id, a, _ := startMembers(t, e, sha('a'))
	replaceJob(t, env, a.ID, env.Now().Add(-6*time.Minute),
		[]corev1.ContainerStatus{waitingStatus("checkout", "ImagePullBackOff")}, nil)

	watch(t, env)
	if env.Job(a.ID) != nil {
		t.Errorf("the stuck member Job was not deleted")
	}
	got := memberOfRole(t, env, id, "a")
	if got.State != store.MemberReported || got.FailureReason != "the review couldn't start (ImagePullBackOff)" {
		t.Errorf("member A = %s %q; want reported as stuck", got.State, got.FailureReason)
	}
	wantWaiting(t, env, id)
	if items, err := env.Store.Attention(env.ctx); err != nil || len(items) != 0 {
		t.Errorf("attention = %+v, %v; want none", items, err)
	}
}

// A member's log and diagnostics go to its own tables, redacted with its
// Job's secrets, never to the run's. A's result comes on its route; its Job
// then finishes. B's Job runs out of memory.
func TestE2EPanelMemberLog(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, a, b := startMembers(t, e, head)
	tokenA := string(env.Secret(a.ID).Data["result-token"])
	serveContainerLogs(env, map[string]string{
		"pergent":  "member log with key-team-a and key-team-b and " + tokenA + "\n",
		"checkout": "cloning\n",
	})

	if code := e.postMember(t, id, a, clean(t, head)); code != http.StatusOK {
		t.Fatalf("member A's result: status %d\n%s", code, env.Logs())
	}
	now := env.Now()
	replaceJob(t, env, a.ID, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now))
	replaceJob(t, env, b.ID, now.Add(-5*time.Minute), nil,
		[]corev1.ContainerStatus{terminatedStatus("pergent", 137, "OOMKilled", now.Add(-4*time.Minute), now.Add(-2*time.Minute))},
		jobCond(batchv1.JobFailed, "BackoffLimitExceeded", now.Add(-2*time.Minute)))
	watch(t, env)
	watch(t, env)

	// Each log hides its own Job's secrets and every model key the engine
	// knows, as a run's does. A's result token is not B's secret.
	for _, c := range []struct {
		m    store.PanelMember
		want string
	}{
		{a, "member log with [REDACTED] and [REDACTED] and [REDACTED]\n"},
		{b, "member log with [REDACTED] and [REDACTED] and " + tokenA + "\n"},
	} {
		m, want := c.m, c.want
		if text, found, err := env.Store.MemberLog(env.ctx, m.ID); err != nil || !found || text != want {
			t.Errorf("member %s's log = %q, %v, %v; want %q", m.Role, text, found, err, want)
		}
	}
	if _, found, _ := env.Store.MemberDiagnostics(env.ctx, a.ID); found {
		t.Errorf("member A completed: it has diagnostics")
	}
	if d, found, _ := env.Store.MemberDiagnostics(env.ctx, b.ID); !found || d.CloneLog != "cloning\n" || d.PodState == "" {
		t.Errorf("member B's diagnostics = %+v, %v; want the clone log and pod state", d, found)
	}
	wantMemberFailure(t, env, id, b, "the review job ran out of memory (OOMKilled)", kube.StepReview)
	if _, found := storedLog(t, env, id); found {
		t.Errorf("a member's log went to the run")
	}
	if _, found := storedDiagnostics(t, env, id); found {
		t.Errorf("a member's diagnostics went to the run")
	}
	wantWaiting(t, env, id)
}

// Between the members' results and Job P nothing is posted for a member: no
// comment, note, thread, approval or status. Settling writes nothing to the
// platform: A is done, B failed, and the run is released (queued again, its
// other fields as the split left them). The dispatch that starts Job P is
// S3.8's.
func TestE2EPanelMemberPostsNothing(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, a, b := startMembers(t, e, head)
	before := env.Run(id)
	env.GitLab.Reset()

	if code := e.postMember(t, id, a, clean(t, head)); code != http.StatusOK {
		t.Fatalf("member A's result: status %d\n%s", code, env.Logs())
	}
	// B's result is for another commit: B fails, the run does not.
	if code := e.postMember(t, id, b, clean(t, sha('f'))); code != http.StatusBadRequest {
		t.Fatalf("member B's result: status %d; want 400", code)
	}
	for i := 0; i < 2; i++ {
		watch(t, env)
		publish(t, env)
		cleanup(t, env)
	}
	for _, r := range env.GitLab.Requests("", "") {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s reached the forge; want nothing posted for a member", r.Method, r.Path)
		}
	}
	if got := memberOfRole(t, env, id, "a"); got.State != store.MemberDone || got.Result == nil {
		t.Errorf("member A = %s; want done with its result", got.State)
	}
	if got := memberOfRole(t, env, id, "b"); got.State != store.MemberFailed ||
		!strings.Contains(got.FailureReason, "the result is for a different commit than the one this run reviews") {
		t.Errorf("member B = %s %q; want failed for the commit", got.State, got.FailureReason)
	}
	wantReleased(t, env, id, before)

	// Job P starts: still nothing is posted, not even its running status
	// (the first member set it). Only P's own publish posts.
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no Job P\n%s", env.Logs())
	}
	wantNothingPosted(t, env)
	if code := e.postResult(t, id, string(env.Secret(id).Data["result-token"]), clean(t, head)); code != http.StatusOK {
		t.Fatalf("P's result: status %d\n%s", code, env.Logs())
	}
	wantNothingPosted(t, env)
	publish(t, env)
	wantPublished(t, env, id)
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(1))); n != 1 {
		t.Errorf("%d comments after P's publish; want 1", n)
	}
}

// wantReleased checks run id was released for Job P: queued again with no
// skip reason, due now, and otherwise as it was before (no failure, no Job,
// no result).
func wantReleased(t *testing.T, env *testEnv, id string, before store.Run) {
	t.Helper()
	after := env.Run(id)
	if after.State != store.Queued || after.SkipReason != "" || !after.NotBefore.IsZero() || after.FailureReason != "" {
		t.Fatalf("run = %s/%q %q; want released for its merge\n%s", after.State, after.SkipReason, after.FailureReason, env.Logs())
	}
	after.State, after.SkipReason, after.NotBefore = before.State, before.SkipReason, before.NotBefore
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the release wrote more than the run's state:\n before %+v\n after  %+v", before, after)
	}
}

// A push while the members run: the run is superseded, then the sweep
// supersedes the members, deletes their Jobs (keeping their logs) and
// cleans them up. The canceled status is set once, by the run.
func TestE2EPanelCleanup(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head, moved := sha('a'), sha('c')
	id, a, b := startMembers(t, e, head)
	serveLog(env, "member log\n")
	for _, m := range []store.PanelMember{a, b} {
		replaceJob(t, env, m.ID, env.Now().Add(-time.Minute), nil, []corev1.ContainerStatus{runningStatus("pergent")})
	}
	servePublish(env, 1, moved)
	e.hook(t, "update", 1, moved, head)
	if r := env.Run(id); r.State != store.Superseded {
		t.Fatalf("run = %s after the push; want superseded", r.State)
	}

	cleanup(t, env)
	cleanup(t, env)
	for _, m := range []store.PanelMember{a, b} {
		if env.Job(m.ID) != nil {
			t.Errorf("member %s's Job still exists", m.Role)
		}
		if text, found, err := env.Store.MemberLog(env.ctx, m.ID); err != nil || !found || text != "member log\n" {
			t.Errorf("member %s's log = %q, %v, %v; want kept before the deletion", m.Role, text, found, err)
		}
	}
	for _, m := range members(t, env, id) {
		if m.State != store.MemberSuperseded {
			t.Errorf("member %s = %s; want superseded", m.Role, m.State)
		}
	}
	if left, err := env.Store.MembersToCleanUp(env.ctx); err != nil || len(left) != 0 {
		t.Errorf("members to clean up = %d, %v; want none", len(left), err)
	}
	if got := statusesOf(t, env, head); len(got) != 2 || got[0] != "running: "+runningDescription || !strings.HasPrefix(got[1], "canceled: ") {
		t.Errorf("statuses = %v; want running, then canceled once", got)
	}
	if _, found := storedLog(t, env, id); found {
		t.Errorf("a member's log went to the run")
	}
}

// The merge request's base moves after the split (on GitHub the base SHA
// follows the base branch): both members still review the base the run
// recorded at the split, and their results for it are accepted.
func TestE2EPanelMemberSplitBase(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id := splitPanel(t, env, 1, head)
	if r := env.Run(id); r.BaseSHA != sha('b') || r.StartSHA != sha('5') {
		t.Fatalf("run base %q start %q; want the split's", r.BaseSHA, r.StartSHA)
	}
	mr := openMR(1, head)
	mr.DiffRefs.BaseSHA, mr.DiffRefs.StartSHA = sha('9'), sha('8')
	env.GitLab.SetMR(mr)

	dispatch(t, env)
	for _, role := range []string{"a", "b"} {
		m := memberOfRole(t, env, id, role)
		if job := env.Job(m.ID); job == nil || !hasArg(job.Spec.Template.Spec.Containers[0].Args, "--base="+sha('b')) {
			t.Fatalf("member %s's Job does not review the split's base\n%s", role, env.Logs())
		}
		if code := e.postMember(t, id, m, clean(t, head)); code != http.StatusOK {
			t.Errorf("member %s's result for the split's base: status %d\n%s", role, code, env.Logs())
		}
		if got := memberOfRole(t, env, id, role); got.State != store.MemberReported || got.Result == nil || got.FailureReason != "" {
			t.Errorf("member %s = %s %q; want reported with its result", role, got.State, got.FailureReason)
		}
	}
}

// unusable is how the callback starts the failure of a result it can't use.
const unusable = "pergent sent a result pergent.sh couldn't use: "

// skillsResult is a clean completed result of head by the named skills.
func skillsResult(t *testing.T, head string, names ...string) []byte {
	t.Helper()
	var skills []result.Skill
	for _, n := range names {
		skills = append(skills, result.Skill{Name: n, Verdict: "no_blocking_issues", Files: result.Files{Reviewed: []string{"a.go"}}})
	}
	return resultFor(t, head, skills...)
}

// A whole review of 2 skills is split: the run records its skills as a
// review does, and each member's result is checked against them. A member
// with a skill more, or one less, fails; the right set is accepted. A
// member's result on another review's route is refused.
func TestE2EPanelMemberSkills(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	env.AddSkill("style", "---\nname: style\n---\nCheck the style.\n")
	head := sha('a')
	// Both reviews are split by the same dispatch pass.
	servePublish(env, 2, head)
	two := env.Enqueue(2, head)
	one := splitPanel(t, env, 1, head)
	for _, id := range []string{one, two} {
		if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitPanel {
			t.Fatalf("run %s = %s/%q; want a panel", id, r.State, r.SkipReason)
		}
		if r := env.Run(id); r.SkillSources["security"] != "library" || r.SkillSources["style"] != "library" || len(r.SkillSources) != 2 ||
			len(r.SkillHashes) != 2 || r.SkillSubset != nil {
			t.Fatalf("run %s skills = %v %v %v; want the 2 skills recorded at the split", id, r.SkillSources, r.SkillHashes, r.SkillSubset)
		}
	}
	dispatch(t, env)
	a1, b1 := memberOfRole(t, env, one, "a"), memberOfRole(t, env, one, "b")
	a2, b2 := memberOfRole(t, env, two, "a"), memberOfRole(t, env, two, "b")

	if code := e.postMember(t, one, a1, skillsResult(t, head, "security", "style", "extra")); code != http.StatusBadRequest {
		t.Errorf("a skill more: status %d; want 400", code)
	}
	wantMemberFailure(t, env, one, a1, unusable+"the result has skills the Job was not started with", "delivery")
	if code := e.postMember(t, two, a2, skillsResult(t, head, "security")); code != http.StatusBadRequest {
		t.Errorf("a skill less: status %d; want 400", code)
	}
	wantMemberFailure(t, env, two, a2, unusable+`the result lacks the skills the Job was started with ("style")`, "delivery")
	if code := e.postMember(t, one, b1, skillsResult(t, head, "style", "security")); code != http.StatusOK {
		t.Errorf("the right skills: status %d; want 200", code)
	}
	if got := memberOfRole(t, env, one, "b"); got.State != store.MemberReported || got.Result == nil || got.FailureReason != "" {
		t.Errorf("member B = %s %q; want reported with its result", got.State, got.FailureReason)
	}
	// B of review 2 with its own token, on review 1's route.
	if code := e.postMember(t, one, b2, skillsResult(t, head, "security", "style")); code != http.StatusUnauthorized {
		t.Errorf("another review's route: status %d; want 401", code)
	}
	if got := memberOfRole(t, env, two, "b"); got.State != store.MemberRunning || got.Result != nil {
		t.Errorf("member B of review 2 = %s; want still running with no result", got.State)
	}
	for _, id := range []string{one, two} {
		wantWaiting(t, env, id)
	}
}

// The sweep leaves a live panel alone: its run waits, so its members keep
// their Jobs and no status is set.
func TestE2EPanelCleanupLeavesLivePanel(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, a, b := startMembers(t, e, head)
	cleanup(t, env)
	cleanup(t, env)
	for _, m := range []store.PanelMember{a, b} {
		if env.Job(m.ID) == nil {
			t.Errorf("member %s's Job was deleted", m.Role)
		}
		if got := memberOfRole(t, env, id, m.Role); got.State != store.MemberRunning {
			t.Errorf("member %s = %s; want running", m.Role, got.State)
		}
	}
	if left, err := env.Store.MembersToCleanUp(env.ctx); err != nil || len(left) != 0 {
		t.Errorf("members to clean up = %d, %v; want none", len(left), err)
	}
	wantWaiting(t, env, id)
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "running: "+runningDescription {
		t.Errorf("statuses = %v; want the running status alone", got)
	}
}

// errorResult is a result of head whose review failed with message.
func errorResult(t *testing.T, head, message string) []byte {
	t.Helper()
	b, err := json.Marshal(result.Result{
		SchemaVersion: result.SchemaVersion, Status: "error", PergentVersion: "0.1.0",
		Base: sha('b'), Head: head, Error: &result.Error{Code: "review_failed", Message: message},
		Comment: result.Comment{Markdown: "The review failed."},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wantNothingPosted checks no request but a read reached the forge.
func wantNothingPosted(t *testing.T, env *testEnv) {
	t.Helper()
	for _, r := range env.GitLab.Requests("", "") {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s reached the forge; want nothing posted for a member", r.Method, r.Path)
		}
	}
}

// jobDeadline is the active deadline of member or run id's Job.
func jobDeadline(t *testing.T, env *testEnv, id string) int64 {
	t.Helper()
	job := env.Job(id)
	if job == nil || job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatalf("%s has no Job with a deadline", id)
	}
	return *job.Spec.ActiveDeadlineSeconds
}

// B's entry is out of quota: the entry is marked and audited as a run's is,
// the member is fell_back, and B's next attempt (attempt_of B) runs on B's
// next entry in a new Job with the full timeout. A is untouched, the run
// still waits, and nothing is posted, not even a status.
func TestE2EPanelMemberFallsBack(t *testing.T) {
	panelFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a")
	env := e.env
	refs := savePanelChain(t, env, "team-b", "team-c")
	head := sha('a')
	id, a, b := startMembers(t, e, head)
	deadline := jobDeadline(t, env, b.ID)
	before := env.Run(id)
	env.GitLab.Reset()

	if code := e.postMember(t, id, a, clean(t, head)); code != http.StatusOK {
		t.Fatalf("member A's result: status %d", code)
	}
	if code := e.postMember(t, id, b, failedOn(t, head, "quota")); code != http.StatusOK {
		t.Fatalf("member B's result: status %d\n%s", code, env.Logs())
	}
	publish(t, env)

	if got := memberOfRole(t, env, id, "a"); got.ID != a.ID || got.State != store.MemberDone {
		t.Errorf("member A = %s %s; want the same member, done", got.ID, got.State)
	}
	if m, ok := markOf(t, env, refs[0]); !ok || m.Class != store.ClassQuota || m.Until == nil || !m.Until.Equal(env.Now().Add(time.Hour)) {
		t.Errorf("B's mark = %+v, %v; want quota until now+1h", m, ok)
	}
	if _, ok := markOf(t, env, refs[1]); ok {
		t.Errorf("B's next entry is marked")
	}
	entries, err := env.Store.AuditLog(env.ctx, 0, 10)
	if err != nil || len(entries) == 0 || entries[0].Action != "model_marked" || entries[0].Actor != "pergent" ||
		entries[0].Detail != "model=team-b class=quota" || entries[0].Project != testProjectPath {
		t.Errorf("audit = %+v, %v", entries, err)
	}
	var next store.PanelMember
	for _, m := range members(t, env, id) {
		switch {
		case m.ID == b.ID && m.State != store.MemberFellBack:
			t.Errorf("member B's first attempt = %s; want fell_back", m.State)
		case m.Role == "b" && m.ID != b.ID:
			next = m
		}
	}
	if next.ID == "" || next.State != store.MemberQueued || next.AttemptOf == nil || *next.AttemptOf != b.ID {
		t.Fatalf("B's next attempt = %+v; want queued, attempt of B", next)
	}
	wantWaiting(t, env, id)
	if !woken(env) {
		t.Errorf("the dispatcher was not woken for B's next attempt")
	}

	dispatch(t, env)
	next = memberOfRole(t, env, id, "b")
	wantMemberKey(t, env, next, "key-team-c")
	if next.Entry.ID != refs[1].ID || next.Entry.Position != 2 || next.Entry.Length != 2 {
		t.Errorf("B's next entry = %+v; want the second of 2", next.Entry)
	}
	if got := jobDeadline(t, env, next.ID); got != deadline {
		t.Errorf("B's next Job's deadline = %ds; want the full %ds", got, deadline)
	}
	if n := len(members(t, env, id)); n != 3 {
		t.Errorf("%d members; want A and B's 2 attempts", n)
	}
	wantNothingPosted(t, env)
	if after := env.Run(id); !reflect.DeepEqual(before, after) {
		t.Errorf("the fallback wrote the run:\n before %+v\n after  %+v", before, after)
	}
}

// B's next entry is the model A runs on: B skips it and takes the one after.
func TestE2EPanelBFallsBackToAModel(t *testing.T) {
	panelFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a")
	env := e.env
	team := ownerTeam(t, env)
	first := addTeamModel(t, env, team, "team-b", "gpt-b", "key-b")
	same := addTeamModel(t, env, team, "team-same", "GPT-TEAM-A", "key-same")
	after := addTeamModel(t, env, team, "team-after", "gpt-after", "key-after")
	saveChainB(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{first, same, after}})
	head := sha('a')
	id := splitPanel(t, env, 1, head)
	dispatch(t, env)
	b := memberOfRole(t, env, id, "b")
	wantMemberKey(t, env, b, "key-b")

	if code := e.postMember(t, id, b, failedOn(t, head, "billing")); code != http.StatusOK {
		t.Fatalf("member B's result: status %d", code)
	}
	publish(t, env)
	dispatch(t, env)
	next := memberOfRole(t, env, id, "b")
	if next.ID == b.ID {
		t.Fatalf("B did not fall back\n%s", env.Logs())
	}
	wantMemberKey(t, env, next, "key-after")
	if next.Entry.ID != after.ID || next.Entry.Position != 3 {
		t.Errorf("B's next entry = %+v; want the third, past A's model", next.Entry)
	}
	if env.Job(memberOfRole(t, env, id, "a").ID) == nil {
		t.Errorf("member A has no Job")
	}
}

// B's only other entry is the model A runs on: B's next attempt can't run
// and fails; that fails B, not the review, which is released for Job P with
// A alone.
func TestE2EPanelBNoneLeft(t *testing.T) {
	panelFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a")
	env := e.env
	team := ownerTeam(t, env)
	first := addTeamModel(t, env, team, "team-b", "gpt-b", "key-b")
	same := addTeamModel(t, env, team, "team-same", "gpt-team-a", "key-same")
	saveChainB(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{first, same}})
	head := sha('a')
	id := splitPanel(t, env, 1, head)
	dispatch(t, env)
	a, b := memberOfRole(t, env, id, "a"), memberOfRole(t, env, id, "b")
	before := env.Run(id)
	env.GitLab.Reset()

	if code := e.postMember(t, id, a, clean(t, head)); code != http.StatusOK {
		t.Fatalf("member A's result: status %d", code)
	}
	if code := e.postMember(t, id, b, failedOn(t, head, "auth")); code != http.StatusOK {
		t.Fatalf("member B's result: status %d", code)
	}
	publish(t, env)
	wantWaiting(t, env, id)
	dispatch(t, env)
	next := memberOfRole(t, env, id, "b")
	if next.ID == b.ID || next.State != store.MemberReported || next.FailureReason == "" || env.Job(next.ID) != nil {
		t.Fatalf("B's next attempt = %s %q; want reported failed, with no Job\n%s", next.State, next.FailureReason, env.Logs())
	}
	publish(t, env)
	if got := memberOfRole(t, env, id, "b"); got.State != store.MemberFailed || got.FailureReason != next.FailureReason {
		t.Errorf("member B = %s %q; want failed with its reason kept", got.State, got.FailureReason)
	}
	if got := memberOfRole(t, env, id, "a"); got.ID != a.ID || got.State != store.MemberDone {
		t.Errorf("member A = %s; want done", got.State)
	}
	wantReleased(t, env, id, before)
	wantNothingPosted(t, env)
}

// Both members fail (A's review failed, B's result is for another commit):
// the review is published as failed with both reasons in the same pass. The
// status is the failure status, nothing is approved, and success is never
// set.
func TestE2EPanelBothFail(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, a, b := startMembers(t, e, head)

	if code := e.postMember(t, id, a, errorResult(t, head, "the model gave no answer")); code != http.StatusOK {
		t.Fatalf("member A's result: status %d\n%s", code, env.Logs())
	}
	if code := e.postMember(t, id, b, clean(t, sha('f'))); code != http.StatusBadRequest {
		t.Fatalf("member B's result: status %d; want 400", code)
	}
	publish(t, env)

	for _, role := range []string{"a", "b"} {
		if got := memberOfRole(t, env, id, role); got.State != store.MemberFailed {
			t.Errorf("member %s = %s; want failed", role, got.State)
		}
	}
	wantPublished(t, env, id)
	r := env.Run(id)
	want := "both review panel members failed: A: the model gave no answer; B: " + unusable +
		"the result is for a different commit than the one this run reviews"
	if r.FailureReason != want || r.Result != nil {
		t.Errorf("run failure = %q; want %q", r.FailureReason, want)
	}
	if body := createdComment(t, env, 1); !strings.Contains(body, "the model gave no answer") ||
		!strings.Contains(body, "the result is for a different commit") {
		t.Errorf("the comment lacks both reasons:\n%s", body)
	}
	got := statusesOf(t, env, head)
	if len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "failed: ") {
		t.Errorf("statuses = %v; want the failure status last", got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "success") {
			t.Errorf("statuses = %v; a failed panel set success", got)
		}
	}
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 0 {
		t.Errorf("%d approvals; want none", n)
	}
	// Nothing more on the next pass.
	env.GitLab.Reset()
	publish(t, env)
	wantNothingPosted(t, env)
}

// A's review fails and B's passes: the review is released for Job P, B done
// and A failed with its reason. The release fires once: a second settle
// pass changes nothing and wakes nothing.
func TestE2EPanelAFailsBPasses(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, a, b := startMembers(t, e, head)
	before := env.Run(id)
	env.GitLab.Reset()

	if code := e.postMember(t, id, a, errorResult(t, head, "")); code != http.StatusOK {
		t.Fatalf("member A's result: status %d", code)
	}
	if code := e.postMember(t, id, b, clean(t, head)); code != http.StatusOK {
		t.Fatalf("member B's result: status %d", code)
	}
	woken(env)
	publish(t, env)
	if got := memberOfRole(t, env, id, "a"); got.ID != a.ID || got.State != store.MemberFailed || got.FailureReason != reasonMemberError || got.Result != nil {
		t.Errorf("member A = %s %q; want failed with the review's failure", got.State, got.FailureReason)
	}
	if got := memberOfRole(t, env, id, "b"); got.ID != b.ID || got.State != store.MemberDone || got.Result == nil {
		t.Errorf("member B = %s; want done with its result", got.State)
	}
	wantReleased(t, env, id, before)
	if !woken(env) {
		t.Errorf("the dispatcher was not woken for Job P")
	}

	released := env.Run(id)
	ms := members(t, env, id)
	publish(t, env)
	if again := env.Run(id); !reflect.DeepEqual(released, again) {
		t.Errorf("a second pass changed the run:\n before %+v\n after  %+v", released, again)
	}
	if again := members(t, env, id); !reflect.DeepEqual(ms, again) {
		t.Errorf("a second pass changed the members")
	}
	if woken(env) {
		t.Errorf("a second pass woke the engine again")
	}
	wantNothingPosted(t, env)
}

// The result handler stores a result, then checks it. A result stored but
// not yet checked is checked again when its member is settled: one that
// fails the check fails the member with the check's reason and never
// becomes done; a good one is done.
func TestE2EPanelSettleChecksResult(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, a, b := startMembers(t, e, head)
	// Stored as the handler stores them, before its check.
	for _, c := range []struct {
		m    store.PanelMember
		body []byte
	}{{a, clean(t, sha('f'))}, {b, clean(t, head)}} {
		token := string(env.Secret(c.m.ID).Data["result-token"])
		if out, err := env.Store.StoreMemberResult(env.ctx, id, c.m.ID, token, c.body, env.Now()); err != nil || out != store.ResultStored {
			t.Fatalf("StoreMemberResult = %v, %v", out, err)
		}
	}

	publish(t, env)
	got := memberOfRole(t, env, id, "a")
	if got.State != store.MemberFailed || got.Result != nil || got.FailureStep != "delivery" ||
		got.FailureReason != unusable+"the result is for a different commit than the one this run reviews" {
		t.Errorf("member A = %s %q at %q; want failed by the check", got.State, got.FailureReason, got.FailureStep)
	}
	if got := memberOfRole(t, env, id, "b"); got.State != store.MemberDone || got.Result == nil {
		t.Errorf("member B = %s; want done", got.State)
	}
	if r := env.Run(id); r.State != store.Queued {
		t.Errorf("run = %s; want released with B alone", r.State)
	}
}

// The review's skills change between the split and a member's start (a
// library skill is edited, or one is added): the member stops before its
// Job with the inputs-changed reason, as its result would fail its check.
func TestE2EPanelMemberSkillsChanged(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(env *testEnv)
	}{
		{"edited", func(env *testEnv) {
			if _, err := env.Store.UpdateLibrarySkill(env.ctx, "security", "---\nname: security\n---\nLook again.\n", "test", 1, env.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{"added", func(env *testEnv) { env.AddSkill("style", "---\nname: style\n---\nCheck the style.\n") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
			env := e.env
			id := splitPanel(t, env, 1, sha('a'))
			c.change(env)

			dispatch(t, env)
			for _, role := range []string{"a", "b"} {
				m := memberOfRole(t, env, id, role)
				if m.State != store.MemberReported || m.FailureReason != reasonInputsChanged || env.Job(m.ID) != nil {
					t.Errorf("member %s = %s %q; want failed before its Job, inputs changed\n%s", role, m.State, m.FailureReason, env.Logs())
				}
			}
		})
	}
}

// Member A falls back along chain A the same way; B is untouched.
func TestE2EPanelAFallsBack(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a", "team-a2"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, a, b := startMembers(t, e, head)
	if code := e.postMember(t, id, a, failedOn(t, head, "quota")); code != http.StatusOK {
		t.Fatalf("member A's result: status %d", code)
	}
	publish(t, env)
	dispatch(t, env)
	next := memberOfRole(t, env, id, "a")
	if next.ID == a.ID || next.AttemptOf == nil || *next.AttemptOf != a.ID {
		t.Fatalf("A's next attempt = %+v; want an attempt of A", next)
	}
	wantMemberKey(t, env, next, "key-team-a2")
	if got := memberOfRole(t, env, id, "b"); got.ID != b.ID || got.State != store.MemberRunning {
		t.Errorf("member B = %s; want the same member, running", got.State)
	}
	wantWaiting(t, env, id)
}

// releasedPanel splits a review of head on merge request 1, starts both
// member Jobs, posts bodyA and bodyB as their results (nil: none) and
// publishes, which settles them and releases the run for Job P. It returns
// the run and members A and B.
func releasedPanel(t *testing.T, e *e2e, head string, bodyA, bodyB []byte) (string, store.PanelMember, store.PanelMember) {
	t.Helper()
	id, a, b := startMembers(t, e, head)
	for _, c := range []struct {
		m    store.PanelMember
		body []byte
	}{{a, bodyA}, {b, bodyB}} {
		if c.body == nil {
			continue
		}
		if code := e.postMember(t, id, c.m, c.body); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d\n%s", c.m.Role, code, e.env.Logs())
		}
	}
	publish(t, e.env)
	if r := e.env.Run(id); r.State != store.Queued || r.SkipReason != "" {
		t.Fatalf("run = %s/%q; want released for Job P\n%s", r.State, r.SkipReason, e.env.Logs())
	}
	return id, a, b
}

// getPanel reads role's member result on run id's panel route with token,
// and returns the status and the body.
func (e *e2e) getPanel(t *testing.T, id, role, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/runs/"+id+"/panel/"+role, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

// panelJudges is the judge entries of Job P run id, by role.
func panelJudges(t *testing.T, env *testEnv, id string) map[string]store.RunEntry {
	t.Helper()
	js, err := env.Store.PanelJudges(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// secretKeys is the sorted keys of run or member id's Secret.
func secretKeys(env *testEnv, id string) []string {
	return slices.Sorted(maps.Keys(env.Secret(id).Data))
}

// hasEnv reports whether the pergent container of id's Job names the
// variable name.
func hasEnv(env *testEnv, id, name string) bool {
	for _, v := range env.Job(id).Spec.Template.Spec.Containers[0].Env {
		if v.Name == name {
			return true
		}
	}
	return false
}

// withRoleError is body with a provider_error of class on the judge run of
// role.
func withRoleError(t *testing.T, body []byte, class, role string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	m["provider_error"] = map[string]any{"class": class, "status": 429, "provider": "openai", "model": "gpt", "role": role}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Both members are done: the run's next dispatch starts one Job P on the
// run, with --panel and its own 10 minute timeout, model A's key as
// llm-api-key and model B's as llm-api-key-b, model B in PERGENT_PANEL_B_*,
// and the panel-inputs container fetching both results from the panel
// route with P's own token. Each role's judge entry is recorded. No key is
// in a label, an annotation, the ConfigMap or the log, and nothing is
// posted. The route serves each done member's result to P's token alone.
func TestE2EPanelJobP(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	bodyA, bodyB := clean(t, head), skillsResult(t, head, "security")
	id, a, b := releasedPanel(t, e, head, bodyA, bodyB)
	env.GitLab.Reset()

	dispatch(t, env)
	job := env.Job(id)
	if job == nil {
		t.Fatalf("no Job P\n%s", env.Logs())
	}
	merges := 0
	for _, j := range env.Jobs() {
		if j.Labels[kube.LabelPanelRole] == kube.PanelRoleMerge {
			merges++
		}
	}
	if merges != 1 || job.Labels[kube.LabelPanelRole] != kube.PanelRoleMerge || job.Labels["pergent.sh/run"] != id {
		t.Errorf("%d merge Jobs, labels %v; want one Job P of the run", merges, job.Labels)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	for _, want := range []string{"--panel", "--panel-a=" + kube.PanelAFile, "--panel-b=" + kube.PanelBFile,
		"--timeout=10m0s", "--base=" + sha('b'), "--result-url=" + testCallbackURL + "/runs/" + id + "/result"} {
		if !hasArg(args, want) {
			t.Errorf("P's args %v lack %q", args, want)
		}
	}
	for _, bad := range []string{"--panel-no-judge", "--no-skill-verify"} {
		if hasArg(args, bad) {
			t.Errorf("P's args %v hold %q", args, bad)
		}
	}
	if got := secretKeys(env, id); !slices.Equal(got, []string{"git-token", "llm-api-key", "llm-api-key-b", "result-token"}) {
		t.Errorf("P's Secret keys = %v; want both LLM keys", got)
	}
	sec := env.Secret(id)
	if string(sec.Data["llm-api-key"]) != "key-team-a" || string(sec.Data["llm-api-key-b"]) != "key-team-b" {
		t.Errorf("P's keys = %q, %q; want A's and B's", sec.Data["llm-api-key"], sec.Data["llm-api-key-b"])
	}
	vars := jobEnv(env, id)
	for k, want := range map[string]string{"OPENCODE_PROVIDER": "openai", "OPENCODE_MODEL": "gpt-team-a",
		"PERGENT_PANEL_B_PROVIDER": "openai", "PERGENT_PANEL_B_MODEL": "gpt-team-b"} {
		if vars[k] != want {
			t.Errorf("P's %s = %q, want %q", k, vars[k], want)
		}
	}
	for _, k := range []string{"OPENCODE_API_KEY", "PERGENT_PANEL_B_API_KEY"} {
		if !hasEnv(env, id, k) {
			t.Errorf("P's env lacks %s", k)
		}
	}
	var fetch *corev1.Container
	for i, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name == "panel-inputs" {
			fetch = &job.Spec.Template.Spec.InitContainers[i]
		}
	}
	if fetch == nil || fetch.Image != testScanImage ||
		!hasArg(fetch.Args, "--a-url="+testCallbackURL+"/runs/"+id+"/panel/a") ||
		!hasArg(fetch.Args, "--b-url="+testCallbackURL+"/runs/"+id+"/panel/b") {
		t.Errorf("panel-inputs container = %+v; want get-panel of both roles on pergent.sh's image", fetch)
	}
	wantDeadline := int64(kube.Deadline(kube.PanelTimeout+kube.PanelFetchTimeout, kube.CloneAllowance(0, 0)) / time.Second)
	if got := jobDeadline(t, env, id); got != wantDeadline {
		t.Errorf("P's deadline = %ds; want %ds (10 min, the fetch, a clone with no linked repository)", got, wantDeadline)
	}
	js := panelJudges(t, env, id)
	if len(js) != 2 || js["a"].ID != a.Entry.ID || js["a"].Name != "team-a" || js["b"].ID != b.Entry.ID ||
		js["b"].Name != "team-b" || js["b"].Length != 1 {
		t.Errorf("judges = %+v; want A's and B's entries", js)
	}
	if r := env.Run(id); r.State != store.Running || r.JobName != kube.JobName(id) || r.BaseSHA != sha('b') {
		t.Errorf("run = %s job %q base %q; want running with Job P on the split's base", r.State, r.JobName, r.BaseSHA)
	}
	meta := maps.Clone(job.Labels)
	maps.Copy(meta, job.Annotations)
	maps.Copy(meta, env.ConfigMap(id).Data)
	for k, v := range meta {
		for _, key := range []string{"key-team-a", "key-team-b"} {
			if strings.Contains(k, key) || strings.Contains(v, key) || strings.Contains(env.Logs(), key) {
				t.Errorf("a key is in P's label, annotation or ConfigMap %s, or the log", k)
			}
		}
	}
	wantNothingPosted(t, env)

	token := string(sec.Data["result-token"])
	for role, want := range map[string][]byte{"a": bodyA, "b": bodyB} {
		if code, body := e.getPanel(t, id, role, token); code != http.StatusOK || !jsonEqual(t, body, want) {
			t.Errorf("panel %s = %d %s; want the member's result", role, code, body)
		}
		if code, _ := e.getPanel(t, id, role, string(env.Secret(a.ID).Data["result-token"])); code != http.StatusNotFound {
			t.Errorf("panel %s with a member's token = %d; want 404", role, code)
		}
	}
	if code := e.postResult(t, id, token, clean(t, head)); code != http.StatusOK {
		t.Fatalf("P's result: status %d\n%s", code, env.Logs())
	}
	publish(t, env)
	wantPublished(t, env, id)
}

// jsonEqual reports whether got and want hold the same JSON value.
func jsonEqual(t *testing.T, got string, want []byte) bool {
	t.Helper()
	var g, w any
	if json.Unmarshal([]byte(got), &g) != nil || json.Unmarshal(want, &w) != nil {
		return false
	}
	return reflect.DeepEqual(g, w)
}

// A failed, B passed: Job P runs B only. Its Secret holds B's key alone, as
// llm-api-key-b; model A has no key and no judge entry. The route serves B's
// result and not A's.
func TestE2EPanelPBOnly(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, errorResult(t, head, "A broke"), clean(t, head))

	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no Job P\n%s", env.Logs())
	}
	sec := env.Secret(id)
	if string(sec.Data["llm-api-key-b"]) != "key-team-b" || len(sec.Data["llm-api-key"]) != 0 {
		t.Errorf("P's keys = %q, %q; want B's alone", sec.Data["llm-api-key"], sec.Data["llm-api-key-b"])
	}
	for k, v := range sec.Data {
		if strings.Contains(string(v), "key-team-a") {
			t.Errorf("P's Secret %s holds A's key", k)
		}
	}
	if hasArg(env.Job(id).Spec.Template.Spec.Containers[0].Args, "--panel-no-judge") {
		t.Errorf("P runs with --panel-no-judge; want B's findings verified")
	}
	if v := jobEnv(env, id)["PERGENT_PANEL_B_MODEL"]; v != "gpt-team-b" {
		t.Errorf("PERGENT_PANEL_B_MODEL = %q", v)
	}
	if js := panelJudges(t, env, id); len(js) != 1 || js["b"].Name != "team-b" {
		t.Errorf("judges = %+v; want B's alone", js)
	}
	token := string(sec.Data["result-token"])
	if code, _ := e.getPanel(t, id, "a", token); code != http.StatusNotFound {
		t.Errorf("panel a = %d; want 404 for a failed member", code)
	}
	if code, _ := e.getPanel(t, id, "b", token); code != http.StatusOK {
		t.Errorf("panel b = %d; want B's result", code)
	}
}

// The instance quota is used up and both judge entries are global: Job P
// merges with --panel-no-judge and gets no LLM key at all.
func TestE2EPanelPQuotaNoJudge(t *testing.T) {
	panelFloor(t, "v0.5.0")
	e := newE2E(t)
	env := e.env
	env.SetNow(novemberMid)
	env.Engine.ScanImage = testScanImage
	globalChainB(t, env)
	head := sha('a')
	id := splitPanel(t, env, 1, head)
	dispatch(t, env)
	for _, role := range []string{"a", "b"} {
		m := memberOfRole(t, env, id, role)
		if code := e.postMember(t, id, m, clean(t, head)); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d\n%s", role, code, env.Logs())
		}
	}
	q := int64(500)
	env.Engine.Config.GitLab[0].MonthlyTokenQuota = &q
	seedQuotaUsage(t, env, 500)
	publish(t, env)
	if r := env.Run(id); r.State != store.Queued {
		t.Fatalf("run = %s; want released\n%s", r.State, env.Logs())
	}

	dispatch(t, env)
	job := env.Job(id)
	if job == nil {
		t.Fatalf("no Job P\n%s", env.Logs())
	}
	if !hasArg(job.Spec.Template.Spec.Containers[0].Args, "--panel-no-judge") {
		t.Errorf("P's args lack --panel-no-judge")
	}
	if got := secretKeys(env, id); !slices.Equal(got, []string{"git-token", "result-token"}) {
		t.Errorf("P's Secret keys = %v; want no LLM key", got)
	}
	for _, k := range []string{"OPENCODE_API_KEY", "PERGENT_PANEL_B_API_KEY"} {
		if hasEnv(env, id, k) {
			t.Errorf("P's env names %s", k)
		}
	}
	if js := panelJudges(t, env, id); len(js) != 0 {
		t.Errorf("judges = %+v; want none", js)
	}
	if strings.Contains(env.Logs(), testLLMKey) || strings.Contains(env.Logs(), "key-b") {
		t.Errorf("a key reached the log")
	}
}

// Job P's judge run on role b is out of quota: B's judge entry is marked and
// audited, the P run is dropped and its next attempt runs on B's next
// entry, from the split's base, with A's judge as before. The members are
// untouched and nothing is posted for the dropped attempt.
func TestE2EPanelPFallsBackRoleB(t *testing.T) {
	panelFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a")
	env := e.env
	env.Engine.ScanImage = testScanImage
	refs := savePanelChain(t, env, "team-b", "team-c")
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, clean(t, head), clean(t, head))
	dispatch(t, env)
	if js := panelJudges(t, env, id); js["b"].ID != refs[0].ID || js["b"].Length != 2 {
		t.Fatalf("B's judge = %+v; want team-b of 2", js["b"])
	}
	ms := members(t, env, id)
	root := env.Run(id)
	env.GitLab.Reset()

	token := string(env.Secret(id).Data["result-token"])
	if code := e.postResult(t, id, token, withRoleError(t, clean(t, head), "quota", "b")); code != http.StatusOK {
		t.Fatalf("P's result: status %d\n%s", code, env.Logs())
	}
	publish(t, env)
	wantFellBack(t, env, id, store.ClassQuota)
	if m, ok := markOf(t, env, refs[0]); !ok || m.Class != store.ClassQuota {
		t.Errorf("B's judge mark = %+v, %v; want quota", m, ok)
	}
	if _, ok := markOf(t, env, refs[1]); ok {
		t.Errorf("B's next entry is marked")
	}
	if entries, err := env.Store.AuditLog(env.ctx, 0, 10); err != nil || len(entries) == 0 ||
		entries[0].Action != "model_marked" || entries[0].Detail != "model=team-b class=quota" {
		t.Errorf("audit = %+v, %v; want B's judge entry marked", entries, err)
	}
	wantNothingPosted(t, env)

	dispatch(t, env)
	next := e.newJob(t, id)
	if r := env.Run(next); r.AttemptOf == nil || *r.AttemptOf != id || r.BaseSHA != root.BaseSHA ||
		!maps.Equal(r.SkillSources, root.SkillSources) {
		t.Fatalf("next attempt = %+v; want an attempt of the run with the split's base and skills", r)
	}
	sec := env.Secret(next)
	if string(sec.Data["llm-api-key"]) != "key-team-a" || string(sec.Data["llm-api-key-b"]) != "key-team-c" {
		t.Errorf("next P's keys = %q, %q; want A's and B's next entry's", sec.Data["llm-api-key"], sec.Data["llm-api-key-b"])
	}
	if v := jobEnv(env, next)["PERGENT_PANEL_B_MODEL"]; v != "gpt-team-c" {
		t.Errorf("next P's PERGENT_PANEL_B_MODEL = %q, want gpt-team-c", v)
	}
	if js := panelJudges(t, env, next); js["a"].Name != "team-a" || js["b"].ID != refs[1].ID {
		t.Errorf("next P's judges = %+v; want team-a and team-c", js)
	}
	if after := members(t, env, id); !reflect.DeepEqual(ms, after) {
		t.Errorf("the members changed:\n before %+v\n after  %+v", ms, after)
	}
	token = string(sec.Data["result-token"])
	if code, _ := e.getPanel(t, next, "a", token); code != http.StatusOK {
		t.Errorf("the next attempt's panel a = %d; want the root's member", code)
	}
	if code := e.postResult(t, next, token, clean(t, head)); code != http.StatusOK {
		t.Fatalf("next P's result: status %d\n%s", code, env.Logs())
	}
	publish(t, env)
	wantPublished(t, env, next)
}

// Job P hits its deadline: the watcher fails the run as it fails any
// review, and the review is published as failed, never success.
func TestE2EPanelPTimeout(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, clean(t, head), clean(t, head))
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no Job P\n%s", env.Logs())
	}
	if err := env.Store.AdvanceCheckoutSeen(env.ctx, id, 2); err != nil {
		t.Fatal(err)
	}
	if err := env.Kube.BatchV1().Jobs(testNamespace).Delete(env.ctx, kube.JobName(id), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	putBareJob(t, env, id, env.Now().Add(-time.Hour), jobCond(batchv1.JobFailed, "DeadlineExceeded", env.Now().Add(-2*time.Minute)))

	watch(t, env)
	wantFailure(t, env, id, "the review didn't finish before the deadline (DeadlineExceeded)")
	publish(t, env)
	got := statusesOf(t, env, head)
	if len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "failed: ") {
		t.Errorf("statuses = %v; want the failure status last", got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "success") {
			t.Errorf("statuses = %v; a timed-out Job P set success", got)
		}
	}
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 0 {
		t.Errorf("%d approvals; want none", n)
	}
}

// The checks the run passed before its split are not done again for Job P:
// an author no longer trusted and a CI job still running hold neither the
// members nor P.
func TestE2EPanelPSkipsChecks(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id := splitPanel(t, env, 3, head)
	env.Engine.Config.GitLab[0].TrustedAuthorsOnly = config.TrustOn
	mr := openMR(3, head)
	mr.Author = gitlab.User{ID: strangerID, Username: strangerName}
	env.GitLab.SetMR(mr)
	strangerMember(env, 0)
	env.GitLab.Reply(http.MethodPost, notesPath(3), http.StatusCreated, `{"id": 900}`)
	env.Engine.Config.Repositories[0].CIJobs = &store.CIJobs{WaitSeconds: 1800, Jobs: []store.CIJob{
		{Name: "test", Read: "log", BlocksApproval: true},
	}}
	serveCI(env, head, "running", "running")
	dispatch(t, env)
	for _, role := range []string{"a", "b"} {
		m := memberOfRole(t, env, id, role)
		if code := e.postMember(t, id, m, clean(t, head)); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d\n%s", role, code, env.Logs())
		}
	}
	publish(t, env)

	dispatch(t, env)
	if r := env.Run(id); r.State != store.Running || env.Job(id) == nil {
		t.Fatalf("run = %s/%q; want Job P started\n%s", r.State, r.SkipReason, env.Logs())
	}
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(3))); n != 0 {
		t.Errorf("%d notes; want none", n)
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "running: "+runningDescription {
		t.Errorf("statuses = %v; want the members' running status alone", got)
	}
}

// Job P starts from the base its run recorded at the split, which both
// members reviewed, even when the merge request's base moved since: its
// Job reviews that base, the run keeps it, and P's result for it is taken.
func TestE2EPanelPKeepsSplitBase(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, clean(t, head), clean(t, head))
	mr := openMR(1, head)
	mr.DiffRefs.BaseSHA, mr.DiffRefs.StartSHA = sha('9'), sha('8')
	env.GitLab.SetMR(mr)

	dispatch(t, env)
	job := env.Job(id)
	if job == nil || !hasArg(job.Spec.Template.Spec.Containers[0].Args, "--base="+sha('b')) {
		t.Fatalf("Job P does not review the split's base\n%s", env.Logs())
	}
	if r := env.Run(id); r.BaseSHA != sha('b') || r.StartSHA != sha('5') {
		t.Errorf("run base %q start %q; want the split's", r.BaseSHA, r.StartSHA)
	}
	if code := e.postResult(t, id, string(env.Secret(id).Data["result-token"]), clean(t, head)); code != http.StatusOK {
		t.Errorf("P's result for the split's base: status %d\n%s", code, env.Logs())
	}
}

// Job P gets the review's rule list and built-in switch, as a review Job.
func TestE2EPanelPRules(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	off := false
	env.Engine.Config.Repositories[0].RuleSets = []string{"a"}
	env.Engine.Config.Repositories[0].BuiltInRules = &off
	addRuleSet(t, env, "a", setAYAML)
	env.GitLab.Reply(http.MethodGet, filePath(rule.RepoFile), http.StatusOK, repoRulesYAML)
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, clean(t, head), clean(t, head))

	dispatch(t, env)
	var labels []string
	for _, r := range jobRules(t, env, id) {
		labels = append(labels, r.Label())
	}
	if want := []string{"repo:*.go", "dashboard:a:*.py"}; !slices.Equal(labels, want) {
		t.Errorf("P's rules = %v, want %v", labels, want)
	}
	if v, ok := jobVar(t, env, id, "PERGENT_RULES"); !ok || v != "/in/rules.json" {
		t.Errorf("P's PERGENT_RULES = %q, %v", v, ok)
	}
	if v, ok := jobVar(t, env, id, "PERGENT_NO_BUILT_IN_RULES"); !ok || v != "true" {
		t.Errorf("P's PERGENT_NO_BUILT_IN_RULES = %q, %v", v, ok)
	}
}

// Without pergent.sh's own image (PERGENT_SH_IMAGE) Job P can't fetch the
// member results: the review fails with that reason, never a pass.
func TestE2EPanelPNoImage(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, clean(t, head), clean(t, head))
	env.Engine.ScanImage = ""

	dispatch(t, env)
	if env.Job(id) != nil {
		t.Errorf("Job P started without pergent.sh's image")
	}
	if r := env.Run(id); r.State != store.Reported || r.FailureReason != reasonNoPanelImage {
		t.Errorf("run = %s %q; want failed for the image", r.State, r.FailureReason)
	}
}

// B's judge entry is refused and B's chain has no other usable entry (the
// one left is the model A ran on): the next attempt of P runs without B's
// judge, on A's key alone, and pergent's merge keeps what B's judge would
// have checked. Its result is published, with no further fallback.
func TestE2EPanelPJudgeNoneLeft(t *testing.T) {
	panelFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a")
	env := e.env
	env.Engine.ScanImage = testScanImage
	team := ownerTeam(t, env)
	first := addTeamModel(t, env, team, "team-b", "gpt-b", "key-b")
	same := addTeamModel(t, env, team, "team-same", "gpt-team-a", "key-same")
	saveChainB(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{first, same}})
	head := sha('a')
	id := splitPanel(t, env, 1, head)
	dispatch(t, env)
	for _, role := range []string{"a", "b"} {
		if code := e.postMember(t, id, memberOfRole(t, env, id, role), clean(t, head)); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d", role, code)
		}
	}
	publish(t, env)
	dispatch(t, env)
	token := string(env.Secret(id).Data["result-token"])
	if code := e.postResult(t, id, token, withRoleError(t, clean(t, head), "auth", "b")); code != http.StatusOK {
		t.Fatalf("P's result: status %d", code)
	}
	publish(t, env)
	wantFellBack(t, env, id, store.ClassAuth)

	dispatch(t, env)
	next := e.newJob(t, id)
	sec := env.Secret(next)
	if string(sec.Data["llm-api-key"]) != "key-team-a" || len(sec.Data["llm-api-key-b"]) != 0 {
		t.Errorf("next P's keys = %q, %q; want A's alone", sec.Data["llm-api-key"], sec.Data["llm-api-key-b"])
	}
	if hasArg(env.Job(next).Spec.Template.Spec.Containers[0].Args, "--panel-no-judge") {
		t.Errorf("next P runs with --panel-no-judge; want A's judge to run")
	}
	if js := panelJudges(t, env, next); len(js) != 1 || js["a"].Name != "team-a" {
		t.Errorf("next P's judges = %+v; want A's alone", js)
	}
	// B's judge run fails for its missing key: there is no B judge entry to
	// fall back from, so the result is published.
	if code := e.postResult(t, next, string(sec.Data["result-token"]), withRoleError(t, clean(t, head), "auth", "b")); code != http.StatusOK {
		t.Fatalf("next P's result: status %d", code)
	}
	publish(t, env)
	wantPublished(t, env, next)
}

// A provider error with no role in Job P's result is not a judge's: the
// review is published as it is, and the entry member A ran on (the run's)
// is neither marked nor fallen back from.
func TestE2EPanelPNoRoleNoFallBack(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a", "team-a2"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, a, _ := releasedPanel(t, e, head, clean(t, head), clean(t, head))
	dispatch(t, env)
	if code := e.postResult(t, id, string(env.Secret(id).Data["result-token"]), failedOn(t, head, "quota")); code != http.StatusOK {
		t.Fatalf("P's result: status %d", code)
	}
	publish(t, env)
	wantPublished(t, env, id)
	if _, ok := markOf(t, env, store.EntryRef{Source: a.Entry.Source, ID: a.Entry.ID}); ok {
		t.Errorf("A's entry is marked")
	}
}

// withBase is body with its base commit set to base.
func withBase(t *testing.T, body []byte, base string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	m["base"] = base
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A review falls back on chain A before chain B exists, and its next attempt
// is split, on a base the merge request moved to meanwhile. Job P runs on
// that split run and reviews the split's base, and P's own later attempt
// takes the split run's values, never the first run's older ones.
func TestE2EPanelSplitAfterFallBack(t *testing.T) {
	panelFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a", "team-a2")
	env := e.env
	head, base := sha('a'), sha('9')
	servePublish(env, 1, head)
	env.Enqueue(1, head)
	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	wantFellBack(t, env, first, store.ClassQuota)

	savePanelChain(t, env, "team-b", "team-c")
	mr := openMR(1, head)
	mr.DiffRefs.BaseSHA, mr.DiffRefs.StartSHA = base, sha('8')
	env.GitLab.SetMR(mr)
	dispatch(t, env)
	as, err := env.Store.Attempts(env.ctx, first)
	if err != nil || len(as) != 2 {
		t.Fatalf("attempts = %d, %v; want the first run and its split attempt", len(as), err)
	}
	split := as[1].ID
	if r := env.Run(split); r.State != store.Waiting || r.SkipReason != store.WaitPanel || r.BaseSHA != base {
		t.Fatalf("split attempt = %s/%q base %q; want a panel on the moved base\n%s", r.State, r.SkipReason, r.BaseSHA, env.Logs())
	}
	if r := env.Run(first); r.BaseSHA == base {
		t.Fatalf("the first run's base is the new one; the test needs an older one")
	}
	dispatch(t, env)
	for _, role := range []string{"a", "b"} {
		m := memberOfRole(t, env, first, role)
		if code := e.postMember(t, m.RunID, m, withBase(t, clean(t, head), base)); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d\n%s", role, code, env.Logs())
		}
	}
	publish(t, env)

	dispatch(t, env)
	if job := env.Job(split); job == nil || !hasArg(job.Spec.Template.Spec.Containers[0].Args, "--base="+base) {
		t.Fatalf("Job P does not review the split's base\n%s", env.Logs())
	}
	if r := env.Run(split); r.BaseSHA != base || r.StartSHA != sha('8') {
		t.Errorf("split run base %q start %q; want the split's", r.BaseSHA, r.StartSHA)
	}
	token := string(env.Secret(split).Data["result-token"])
	if code := e.postResult(t, split, token, withRoleError(t, withBase(t, clean(t, head), base), "quota", "b")); code != http.StatusOK {
		t.Fatalf("P's result: status %d\n%s", code, env.Logs())
	}
	publish(t, env)
	wantFellBack(t, env, split, store.ClassQuota)

	dispatch(t, env)
	next := e.newJob(t, first, split)
	r, want := env.Run(next), env.Run(split)
	if r.BaseSHA != base || r.StartSHA != sha('8') || !maps.Equal(r.SkillSources, want.SkillSources) ||
		r.ModelEntryID != want.ModelEntryID {
		t.Errorf("P's next attempt = base %q start %q entry %q; want the split run's", r.BaseSHA, r.StartSHA, r.ModelEntryID)
	}
	if !hasArg(env.Job(next).Spec.Template.Spec.Containers[0].Args, "--base="+base) {
		t.Errorf("P's next Job does not review the split's base")
	}
	if code := e.postResult(t, next, string(env.Secret(next).Data["result-token"]), withBase(t, clean(t, head), base)); code != http.StatusOK {
		t.Fatalf("next P's result: status %d\n%s", code, env.Logs())
	}
	publish(t, env)
	wantPublished(t, env, next)
}

// B's member entry left chain B after the split (it still exists, with its
// key): B's judge takes the chain's entry now, not the one that left.
func TestE2EPanelPJudgeLeftChain(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, clean(t, head), clean(t, head))
	refs := savePanelChain(t, env, "team-c")

	dispatch(t, env)
	if got := string(env.Secret(id).Data["llm-api-key-b"]); got != "key-team-c" {
		t.Errorf("P's B key = %q; want the chain's entry now", got)
	}
	if js := panelJudges(t, env, id); js["b"].ID != refs[0].ID || js["b"].Length != 1 {
		t.Errorf("B's judge = %+v; want team-c of a chain of 1", js["b"])
	}
}

// Job P's captured log hides model B's key from its own Secret, even when
// the engine no longer knows that key.
func TestE2EPanelPLogHidesKeyB(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id, _, _ := releasedPanel(t, e, head, clean(t, head), clean(t, head))
	dispatch(t, env)
	sec := env.Secret(id)
	sec.Data["llm-api-key-b"] = []byte("key-only-in-the-secret")
	if _, err := env.Kube.CoreV1().Secrets(testNamespace).Update(env.ctx, sec, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	hide, err := env.Engine.jobHide(env.ctx, kube.JobName(id))
	if err != nil || !slices.Contains(hide, "key-only-in-the-secret") {
		t.Errorf("jobHide = %v; want model B's key from the Secret", err)
	}
}
