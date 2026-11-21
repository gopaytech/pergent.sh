package engine

import (
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/kube"
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
		if out, err := env.Store.StoreMemberResult(env.ctx, m.ID, token, clean(t, head), env.Now()); err != nil || out != store.ResultStored {
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
	if out, err := env.Store.StoreMemberResult(env.ctx, started.ID, token, clean(t, head), env.Now()); err != nil || out != store.ResultStored {
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
