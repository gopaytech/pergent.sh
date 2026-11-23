package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// entryNamed is the ref of the model entry named name the engine knows, or
// a new team entry of the owner team with that name, the model "gpt-" +
// name and the key "key-" + name.
func entryNamed(t *testing.T, env *testEnv, name string) store.EntryRef {
	t.Helper()
	for _, m := range env.Engine.registry().AllModels() {
		if m.Name == name {
			return chain.RefOf(m)
		}
	}
	return addTeamModel(t, env, ownerTeam(t, env), name, "gpt-"+name, "key-"+name)
}

// saveReviewers stores reviewers 2.. of acme/widgets, one chain of the named
// entries per item of extra (entryNamed), and the judge chain of the names
// in judge.
func saveReviewers(t *testing.T, env *testEnv, extra [][]string, judge []string) {
	t.Helper()
	refs := func(names []string) []store.EntryRef {
		out := []store.EntryRef{}
		for _, n := range names {
			out = append(out, entryNamed(t, env, n))
		}
		return out
	}
	var chains [][]store.EntryRef
	for _, names := range extra {
		chains = append(chains, refs(names))
	}
	saveExtraReviewers(t, env, chains, refs(judge))
}

// newMultiReviewE2E is newFallbackE2E with chain A of the names one, the
// reviewers 2.. of extra and the judge of judge (saveReviewers), on an image
// that has multi review and with pergent.sh's own image set.
func newMultiReviewE2E(t *testing.T, one []string, extra [][]string, judge []string) *e2e {
	t.Helper()
	multiReviewFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, one...)
	e.env.Engine.ScanImage = testScanImage
	saveReviewers(t, e.env, extra, judge)
	return e
}

// newThreeReviewers is newMultiReviewE2E with reviewers team-a, team-b and
// team-c, judged by team-j.
func newThreeReviewers(t *testing.T) *e2e {
	t.Helper()
	return newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b"}, {"team-c"}}, []string{"team-j"})
}

// splitReview queues a run of head on merge request iid, serves it and
// dispatches once: the run is split. It returns the run.
func splitReview(t *testing.T, env *testEnv, iid int64, head string) string {
	t.Helper()
	servePublish(env, iid, head)
	id := env.Enqueue(iid, head)
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitReviewers {
		t.Fatalf("run = %s/%q; want a multi review\n%s", r.State, r.SkipReason, env.Logs())
	}
	if n := len(env.Jobs()); n != 0 {
		t.Fatalf("%d Jobs after the split; want none", n)
	}
	return id
}

// memberOf is the newest member of reviewer n of run id's review.
func memberOf(t *testing.T, env *testEnv, id string, n int) store.ReviewMember {
	t.Helper()
	var out store.ReviewMember
	for _, m := range reviewMembers(t, env, id) {
		if m.Reviewer == n && m.State != store.MemberOff {
			out = m
		}
	}
	if out.ID == "" {
		t.Fatalf("no reviewer %d of %s", n, id)
	}
	return out
}

// wantMemberKey checks member m has a Job whose Secret holds key.
func wantMemberKey(t *testing.T, env *testEnv, m store.ReviewMember, key string) {
	t.Helper()
	if env.Job(m.ID) == nil {
		t.Fatalf("reviewer %d has no Job (%+v)\n%s", m.Reviewer, m, env.Logs())
	}
	if got := string(env.Secret(m.ID).Data["llm-api-key"]); got != key {
		t.Fatalf("reviewer %d's key = %q, want %q", m.Reviewer, got, key)
	}
}

// startReviewers splits a review of head on merge request 1 with reviewers
// team-a, team-b and team-c, and starts their Jobs. It returns the run and
// the 3 members.
func startReviewers(t *testing.T, e *e2e, head string) (string, []store.ReviewMember) {
	t.Helper()
	id := splitReview(t, e.env, 1, head)
	dispatch(t, e.env)
	var ms []store.ReviewMember
	for n, name := range []string{"team-a", "team-b", "team-c"} {
		m := memberOf(t, e.env, id, n+1)
		wantMemberKey(t, e.env, m, "key-"+name)
		ms = append(ms, m)
	}
	return id, ms
}

// wantMultiSuperseded checks run id and its n members are superseded, and
// no member has a Job.
func wantMultiSuperseded(t *testing.T, env *testEnv, id string, n int) {
	t.Helper()
	if r := env.Run(id); r.State != store.Superseded {
		t.Fatalf("run = %s; want superseded\n%s", r.State, env.Logs())
	}
	ms := reviewMembers(t, env, id)
	if len(ms) != n {
		t.Fatalf("members = %+v; want %d", ms, n)
	}
	for _, m := range ms {
		if m.State != store.MemberSuperseded || m.JobName != "" {
			t.Errorf("reviewer %d = %s job %q; want superseded with no Job", m.Reviewer, m.State, m.JobName)
		}
	}
	if n := len(env.Jobs()); n != 0 {
		t.Errorf("%d Jobs; want none", n)
	}
}

// wantWaiting checks run id still waits for its members, with no failure.
func wantWaiting(t *testing.T, env *testEnv, id string) {
	t.Helper()
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitReviewers || r.FailureReason != "" {
		t.Errorf("run = %s/%q %q; want still waiting for its members", r.State, r.SkipReason, r.FailureReason)
	}
}

// wantMemberFailure checks member m was reported with reason at step.
func wantMemberFailure(t *testing.T, env *testEnv, run string, m store.ReviewMember, reason, step string) {
	t.Helper()
	got := memberOf(t, env, run, m.Reviewer)
	if got.ID != m.ID || got.State != store.MemberReported || got.FailureReason != reason || got.FailureStep != step {
		t.Errorf("reviewer %d = %s %q at %q; want reported with %q at %q", m.Reviewer, got.State, got.FailureReason, got.FailureStep, reason, step)
	}
}

// postMember posts body as member m's result on the member route of run,
// with its token, and returns the status.
func (e *e2e) postMember(t *testing.T, run string, m store.ReviewMember, body []byte) int {
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

// storeMemberResult stores a clean result of head for member m with the
// token its Secret holds.
func storeMemberResult(t *testing.T, env *testEnv, m store.ReviewMember, head string) {
	t.Helper()
	token := string(env.Secret(m.ID).Data["result-token"])
	if out, err := env.Store.StoreReviewMemberResult(env.ctx, m.RunID, m.ID, token, clean(t, head), env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("reviewer %d's result = %v, %v; want stored", m.Reviewer, out, err)
	}
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

// withBase is the result body with its base set to base.
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

// Why no extra reviewer can start: the quota only when the quota stopped
// every one of them; no skips, or any other reason among them, is no other
// reviewer.
func TestOffReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		skips []string
		want  string
	}{
		{"no skips", nil, offNone},
		{"all quota", []string{skipQuota, skipQuota}, offQuota},
		{"quota and same model", []string{skipQuota, skipSame}, offNone},
		{"quota and no usable entry", []string{skipQuota, skipNone}, offNone},
	} {
		if got := offReason(tc.skips); got != tc.want {
			t.Errorf("%s: offReason(%q) = %q, want %q", tc.name, tc.skips, got, tc.want)
		}
	}
}

// With a floor that accepts the image and no extra reviewer, a review's
// dispatch is still byte for byte today's: the golden of TestDispatchJobUnchanged.
func TestDispatchJobUnchangedWithFloor(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env, id := goldenEnv(t, nil)
	dispatch(t, env)
	checkDispatchGolden(t, env, id)
	if ms := reviewMembers(t, env, id); len(ms) != 0 {
		t.Errorf("review members = %+v; want none", ms)
	}
}

// The split, then a Job per reviewer: each Secret holds its own entry's key
// alone, the Job skips the skill verification, its labels name the run, the
// member and the number, and it reports on the member's route. No key is in
// a label, a log or a ConfigMap. The running status is set once, and the run
// is not written.
func TestE2EMultiReviewMembersStart(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id := splitReview(t, env, 1, head)
	before := env.Run(id)

	dispatch(t, env)
	if n := len(env.Jobs()); n != 3 {
		t.Fatalf("%d Jobs; want the 3 reviewers'\n%s", n, env.Logs())
	}
	keys := []string{"key-team-a", "key-team-b", "key-team-c", "key-team-j"}
	for n := 1; n <= 3; n++ {
		key := keys[n-1]
		m := memberOf(t, env, id, n)
		name := "team-" + string(rune('a'+n-1))
		if m.State != store.MemberRunning || m.JobName != kube.JobName(m.ID) || m.Entry.Name != name ||
			m.Model != "gpt-"+name || m.DispatchedAt.IsZero() {
			t.Errorf("reviewer %d = %+v; want running with its Job and entry", n, m)
		}
		wantMemberKey(t, env, m, key)
		sec := env.Secret(m.ID)
		if got := slices.Sorted(maps.Keys(sec.Data)); !slices.Equal(got, []string{"git-token", "llm-api-key", "result-token"}) {
			t.Errorf("reviewer %d's Secret keys = %v; want one LLM key", n, got)
		}
		for k, v := range sec.Data {
			for _, other := range keys {
				if other != key && strings.Contains(string(v), other) {
					t.Errorf("reviewer %d's Secret %s holds another entry's key", n, k)
				}
			}
		}
		job := env.Job(m.ID)
		args := job.Spec.Template.Spec.Containers[0].Args
		for _, want := range []string{"--no-skill-verify",
			"--result-url=" + testCallbackURL + "/runs/" + id + "/members/" + m.ID + "/result"} {
			if !hasArg(args, want) {
				t.Errorf("reviewer %d's args %v lack %q", n, args, want)
			}
		}
		wantLabels := map[string]string{"pergent.sh/run": id, kube.LabelReviewMember: m.ID, kube.LabelReviewer: strconv.Itoa(n)}
		for k, v := range wantLabels {
			if job.Labels[k] != v {
				t.Errorf("reviewer %d's label %s = %q, want %q", n, k, job.Labels[k], v)
			}
		}
		for _, k := range []string{kube.LabelPanelMember, kube.LabelPanelRole} {
			if v, ok := job.Labels[k]; ok {
				t.Errorf("reviewer %d has the panel label %s=%q", n, k, v)
			}
		}
		meta := maps.Clone(job.Labels)
		maps.Copy(meta, job.Annotations)
		maps.Copy(meta, env.ConfigMap(m.ID).Data)
		for k, v := range meta {
			for _, key := range keys {
				if strings.Contains(k, key) || strings.Contains(v, key) {
					t.Errorf("reviewer %d's label, annotation or ConfigMap %s holds a key", n, k)
				}
			}
		}
		// The Secret's token is the member's own.
		storeMemberResult(t, env, m, head)
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
		t.Errorf("the reviewers wrote the run:\n before %+v\n after  %+v", before, after)
	}
}

// The checks a run passed before it was split are not done again: an author
// no longer trusted and a CI job still running hold no reviewer.
func TestE2EMultiReviewMemberSkipsChecks(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id := splitReview(t, env, 3, head)

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
	for n, name := range []string{"team-a", "team-b", "team-c"} {
		wantMemberKey(t, env, memberOf(t, env, id, n+1), "key-"+name)
	}
	wantWaiting(t, env, id)
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(3))); n != 0 {
		t.Errorf("%d notes; want none", n)
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "running: "+runningDescription {
		t.Errorf("statuses = %v; want the running status alone", got)
	}
}

// Reviewer 3's chain is [reviewer 1's model, reviewer 2's model, other] (the
// same models on other entries, in other case): it runs on other, whichever
// reviewer starts first.
func TestE2EMultiReviewSkipsEarlierModels(t *testing.T) {
	for _, first := range []int{1, 3} {
		t.Run("reviewer "+strconv.Itoa(first)+" first", func(t *testing.T) {
			e := newMultiReviewE2E(t, []string{"team-a"}, nil, nil)
			env := e.env
			team := ownerTeam(t, env)
			b := addTeamModel(t, env, team, "team-b", "gpt-team-b", "key-team-b")
			sameA := addTeamModel(t, env, team, "same-a", "GPT-TEAM-A", "key-same-a")
			sameB := addTeamModel(t, env, team, "same-b", "GPT-TEAM-B", "key-same-b")
			other := addTeamModel(t, env, team, "other", "gpt-other", "key-other")
			saveExtraReviewers(t, env, [][]store.EntryRef{{b}, {sameA, sameB, other}}, []store.EntryRef{b})
			id := splitReview(t, env, 1, sha('a'))

			if first == 3 {
				for _, n := range []int{1, 2} {
					execSQL(t, env, `UPDATE review_members SET not_before = now() + interval '1 hour' WHERE id = '`+memberOf(t, env, id, n).ID+`'`)
				}
				dispatch(t, env)
				if n := len(env.Jobs()); n != 1 {
					t.Fatalf("%d Jobs; want reviewer 3's alone", n)
				}
				env.SetNow(env.Now().Add(2 * time.Hour))
			}
			dispatch(t, env)

			wantMemberKey(t, env, memberOf(t, env, id, 1), "key-team-a")
			wantMemberKey(t, env, memberOf(t, env, id, 2), "key-team-b")
			m := memberOf(t, env, id, 3)
			wantMemberKey(t, env, m, "key-other")
			if m.Entry.ID != other.ID || m.Entry.Position != 3 || m.Entry.FallbackClass != chain.ReasonSameModel {
				t.Errorf("reviewer 3's entry = %+v; want the third, past the earlier models", m.Entry)
			}
		})
	}
}

// The head moves before the reviewers start: a review of the new head is
// queued, the multi review's run is superseded, and the sweep supersedes its
// members. No reviewer Job starts.
func TestE2EMultiReviewHeadMoved(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head, moved := sha('a'), sha('c')
	id := splitReview(t, env, 1, head)
	servePublish(env, 1, moved)

	dispatch(t, env)
	next, created, err := env.Store.EnqueueRun(env.ctx, env.MRKey(1), testProjectPath, moved, "main")
	if err != nil || created || next == id {
		t.Fatalf("EnqueueRun = %s, %v, %v; want the new head's run already there", next, created, err)
	}
	if r := env.Run(next); r.HeadSHA != moved || r.State == store.Superseded {
		t.Errorf("new head's run = %s %s; want a live run of the new head", r.HeadSHA, r.State)
	}
	// The sweep (CleanupOnce) takes the members of a run that is gone.
	if _, err := env.Store.SupersedeOrphanReviewMembers(env.ctx); err != nil {
		t.Fatal(err)
	}
	wantMultiSuperseded(t, env, id, 3)
	if got := statusesOf(t, env, head); len(got) != 0 {
		t.Errorf("statuses on the old head = %v; want none", got)
	}
}

// The merge request is closed before the reviewers start: the multi
// review's run and its members are superseded, and no reviewer Job starts.
func TestE2EMultiReviewMRClosed(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id := splitReview(t, env, 1, head)
	mr := openMR(1, head)
	mr.State = "closed"
	env.GitLab.SetMR(mr)

	dispatch(t, env)
	wantMultiSuperseded(t, env, id, 3)
	if got := statusesOf(t, env, head); len(got) != 0 {
		t.Errorf("statuses = %v; want none", got)
	}
}

// Reviewer 2's Job can't be created: it is tried 5 times with backoff, then
// fails. Only the member fails: the run still waits, and the other Jobs run.
func TestE2EMultiReviewRetryGivesUp(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	id := splitReview(t, env, 1, sha('a'))
	two := memberOf(t, env, id, 2)
	fails := 0
	env.Kube.PrependReactor("create", "jobs", func(act k8stesting.Action) (bool, runtime.Object, error) {
		if act.(k8stesting.CreateAction).GetObject().(*batchv1.Job).Name == kube.JobName(two.ID) {
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
		t.Errorf("%d launches of reviewer 2; want 5", fails)
	}
	two = memberOf(t, env, id, 2)
	if two.State != store.MemberReported || !strings.HasPrefix(two.FailureReason, reasonCouldntStart) || two.JobName != "" {
		t.Errorf("reviewer 2 = %s %q job %q; want reported as failed to start", two.State, two.FailureReason, two.JobName)
	}
	wantMemberKey(t, env, memberOf(t, env, id, 1), "key-team-a")
	wantMemberKey(t, env, memberOf(t, env, id, 3), "key-team-c")
	wantWaiting(t, env, id)
}

// jobs.max_running 2 and 3 reviewers: 2 Jobs at a time. The third starts
// once one of them has reported.
func TestE2EMultiReviewMaxRunning(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	env.Engine.Config.Jobs.MaxRunning = 2
	head := sha('a')
	id := splitReview(t, env, 1, head)

	dispatch(t, env)
	dispatch(t, env)
	if n := len(env.Jobs()); n != 2 {
		t.Fatalf("%d Jobs; want 2", n)
	}
	if m := memberOf(t, env, id, 3); m.State != store.MemberQueued || m.JobName != "" {
		t.Fatalf("reviewer 3 = %s job %q; want queued", m.State, m.JobName)
	}
	storeMemberResult(t, env, memberOf(t, env, id, 1), head)
	dispatch(t, env)
	if n := len(env.Jobs()); n != 3 {
		t.Errorf("%d Jobs; want the third reviewer's too", n)
	}
	wantMemberKey(t, env, memberOf(t, env, id, 3), "key-team-c")
}

// A reviewer's Job gets the review's rules and built-in switch, as the
// review Job does: rules.json in its ConfigMap, PERGENT_RULES and
// PERGENT_NO_BUILT_IN_RULES in its env.
func TestE2EMultiReviewRules(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	off := false
	env.Engine.Config.Repositories[0].RuleSets = []string{"a"}
	env.Engine.Config.Repositories[0].BuiltInRules = &off
	addRuleSet(t, env, "a", setAYAML)
	env.GitLab.Reply(http.MethodGet, filePath(rule.RepoFile), http.StatusOK, repoRulesYAML)
	id := splitReview(t, env, 1, sha('a'))

	dispatch(t, env)
	for n := 1; n <= 3; n++ {
		m := memberOf(t, env, id, n)
		var labels []string
		for _, r := range jobRules(t, env, m.ID) {
			labels = append(labels, r.Label())
		}
		if want := []string{"repo:*.go", "dashboard:a:*.py"}; !slices.Equal(labels, want) {
			t.Errorf("reviewer %d's rules = %v, want %v", n, labels, want)
		}
		if v, ok := jobVar(t, env, m.ID, "PERGENT_RULES"); !ok || v != "/in/rules.json" {
			t.Errorf("reviewer %d's PERGENT_RULES = %q, %v", n, v, ok)
		}
		if v, ok := jobVar(t, env, m.ID, "PERGENT_NO_BUILT_IN_RULES"); !ok || v != "true" {
			t.Errorf("reviewer %d's PERGENT_NO_BUILT_IN_RULES = %q, %v", n, v, ok)
		}
	}
}

// The multi review's run is superseded while a reviewer's Job starts: the
// Job is deleted and the member goes back to the queue, so it holds no
// jobs.max_running slot until the sweep supersedes it.
func TestE2EMultiReviewMemberSupersededAtLaunch(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	id := splitReview(t, env, 1, sha('a'))
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
	for _, m := range reviewMembers(t, env, id) {
		if m.State == store.MemberRunning || m.JobName != "" || env.Job(m.ID) != nil {
			t.Errorf("reviewer %d = %s job %q; want back in the queue with no Job", m.Reviewer, m.State, m.JobName)
		}
	}
	running, err := env.Store.RunningReviewMembers(env.ctx)
	if err != nil || len(running) != 0 {
		t.Errorf("running members = %d, %v; want none", len(running), err)
	}
}

// With the code graph on, a reviewer's Job has no graph (the graph route
// takes only the run's token), and each reviewer logs that once, with no
// URL.
func TestE2EMultiReviewNoGraph(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a")
	env := e.env
	env.Engine.ScanImage = testScanImage
	env.Engine.GraphImage = testGraphImage
	env.Engine.GraphDir = t.TempDir()
	env.Engine.Config.Repositories = nil
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: testProjectPath, CreatedBy: "alice"})
	if _, _, err := env.Store.SetDashboardRepoGraph(env.ctx, testInstance, testProjectPath, true); err != nil {
		t.Fatal(err)
	}
	saveReviewers(t, env, [][]string{{"team-b"}, {"team-c"}}, []string{"team-j"})
	storeTestGraph(t, env, "main", sha('d'))
	id := splitReview(t, env, 1, sha('a'))

	dispatch(t, env)
	for n := 1; n <= 3; n++ {
		m := memberOf(t, env, id, n)
		job := env.Job(m.ID)
		if job == nil {
			t.Fatalf("reviewer %d has no Job\n%s", n, env.Logs())
		}
		for _, c := range job.Spec.Template.Spec.InitContainers {
			if c.Image == testScanImage {
				t.Errorf("reviewer %d's Job has the graph container %s", n, c.Name)
			}
		}
		if _, ok := jobVar(t, env, m.ID, "PERGENT_GRAPH"); ok {
			t.Errorf("reviewer %d's Job reads a graph", n)
		}
	}
	logs := env.Logs()
	if n := strings.Count(logs, "the code graph is left out for a reviewer of a multi review"); n != 3 {
		t.Errorf("the graph note was logged %d times; want once per reviewer\n%s", n, logs)
	}
	if strings.Contains(logs, "/graph") {
		t.Errorf("a graph URL reached the log:\n%s", logs)
	}
}

// Reviewer 1's Job vanishes: after the grace it fails with
// reasonDisappeared, the run still waits, the others run on, and nothing is
// posted.
func TestE2EMultiReviewMemberGone(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	before := env.Run(id)
	if err := env.Kube.BatchV1().Jobs(testNamespace).Delete(env.ctx, kube.JobName(ms[0].ID), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	woken(env)

	env.SetNow(env.Now().Add(30 * time.Second))
	watch(t, env)
	if got := memberOf(t, env, id, 1); got.State != store.MemberRunning {
		t.Errorf("reviewer 1 = %s inside the grace; want running", got.State)
	}

	env.SetNow(env.Now().Add(31 * time.Second))
	env.GitLab.Reset()
	watch(t, env)
	wantMemberFailure(t, env, id, ms[0], reasonDisappeared, kube.StepUnknown)
	for _, n := range []int{2, 3} {
		if got := memberOf(t, env, id, n); got.ID != ms[n-1].ID || got.State != store.MemberRunning {
			t.Errorf("reviewer %d = %s; want running", n, got.State)
		}
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

// Reviewer 2's Job hits its deadline: it fails with the deadline's reason
// once the grace is over; the run still waits.
func TestE2EMultiReviewMemberDeadline(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	id, ms := startReviewers(t, e, sha('a'))
	two := ms[1]
	if err := env.Store.AdvanceReviewMemberCheckoutSeen(env.ctx, two.ID, 2); err != nil {
		t.Fatal(err)
	}
	finished := env.Now()
	if err := env.Kube.BatchV1().Jobs(testNamespace).Delete(env.ctx, kube.JobName(two.ID), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	putBareJob(t, env, two.ID, finished.Add(-time.Hour), jobCond(batchv1.JobFailed, "DeadlineExceeded", finished))

	env.SetNow(finished.Add(30 * time.Second))
	watch(t, env)
	if got := memberOf(t, env, id, 2); got.State != store.MemberRunning {
		t.Errorf("reviewer 2 = %s inside the grace; want running", got.State)
	}
	env.SetNow(finished.Add(61 * time.Second))
	watch(t, env)
	wantMemberFailure(t, env, id, two, "the review didn't finish before the deadline (DeadlineExceeded)", kube.StepReview)
	if got := memberOf(t, env, id, 1); got.State != store.MemberRunning {
		t.Errorf("reviewer 1 = %s; want running", got.State)
	}
	wantWaiting(t, env, id)
}

// Reviewer 1's Job is stuck on an image pull: it is deleted and the member
// fails at the start step; the run still waits and no "Needs attention" item
// is raised.
func TestE2EMultiReviewMemberStuck(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	id, ms := startReviewers(t, e, sha('a'))
	replaceJob(t, env, ms[0].ID, env.Now().Add(-6*time.Minute),
		[]corev1.ContainerStatus{waitingStatus("checkout", "ImagePullBackOff")}, nil)

	watch(t, env)
	if env.Job(ms[0].ID) != nil {
		t.Errorf("the stuck reviewer Job was not deleted")
	}
	got := memberOf(t, env, id, 1)
	if got.State != store.MemberReported || got.FailureReason != "the review couldn't start (ImagePullBackOff)" {
		t.Errorf("reviewer 1 = %s %q; want reported as stuck", got.State, got.FailureReason)
	}
	wantWaiting(t, env, id)
	if items, err := env.Store.Attention(env.ctx); err != nil || len(items) != 0 {
		t.Errorf("attention = %+v, %v; want none", items, err)
	}
}

// A reviewer's log and diagnostics go to its own tables (review_member_logs),
// redacted with its Job's secrets, never to the run's. Reviewer 1's result
// comes on its route; its Job then finishes. Reviewer 2's Job runs out of
// memory.
func TestE2EMultiReviewMemberLog(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	one, two := ms[0], ms[1]
	tokenOne := string(env.Secret(one.ID).Data["result-token"])
	serveContainerLogs(env, map[string]string{
		"pergent":  "member log with key-team-a and key-team-b and " + tokenOne + "\n",
		"checkout": "cloning\n",
	})

	if code := e.postMember(t, id, one, clean(t, head)); code != http.StatusOK {
		t.Fatalf("reviewer 1's result: status %d\n%s", code, env.Logs())
	}
	now := env.Now()
	replaceJob(t, env, one.ID, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now))
	replaceJob(t, env, two.ID, now.Add(-5*time.Minute), nil,
		[]corev1.ContainerStatus{terminatedStatus("pergent", 137, "OOMKilled", now.Add(-4*time.Minute), now.Add(-2*time.Minute))},
		jobCond(batchv1.JobFailed, "BackoffLimitExceeded", now.Add(-2*time.Minute)))
	watch(t, env)
	watch(t, env)

	// Each log hides its own Job's secrets and every model key the engine
	// knows, as a run's does. Reviewer 1's result token is not reviewer 2's
	// secret.
	for _, c := range []struct {
		m    store.ReviewMember
		want string
	}{
		{one, "member log with [REDACTED] and [REDACTED] and [REDACTED]\n"},
		{two, "member log with [REDACTED] and [REDACTED] and " + tokenOne + "\n"},
	} {
		if text, found, err := env.Store.ReviewMemberLog(env.ctx, c.m.ID); err != nil || !found || text != c.want {
			t.Errorf("reviewer %d's log = %q, %v, %v; want %q", c.m.Reviewer, text, found, err, c.want)
		}
	}
	if _, found, _ := env.Store.ReviewMemberDiagnostics(env.ctx, one.ID); found {
		t.Errorf("reviewer 1 completed: it has diagnostics")
	}
	if d, found, _ := env.Store.ReviewMemberDiagnostics(env.ctx, two.ID); !found || d.CloneLog != "cloning\n" || d.PodState == "" {
		t.Errorf("reviewer 2's diagnostics = %+v, %v; want the clone log and pod state", d, found)
	}
	wantMemberFailure(t, env, id, two, "the review job ran out of memory (OOMKilled)", kube.StepReview)
	if _, found := storedLog(t, env, id); found {
		t.Errorf("a member's log went to the run")
	}
	if _, found := storedDiagnostics(t, env, id); found {
		t.Errorf("a member's diagnostics went to the run")
	}
	wantWaiting(t, env, id)
}

// A push while the reviewers run: the run is superseded, then the sweep
// supersedes the members, deletes their Jobs (keeping their logs) and
// cleans them up. The canceled status is set once, by the run.
func TestE2EMultiReviewCleanup(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head, moved := sha('a'), sha('c')
	id, ms := startReviewers(t, e, head)
	serveLog(env, "member log\n")
	for _, m := range ms {
		replaceJob(t, env, m.ID, env.Now().Add(-time.Minute), nil, []corev1.ContainerStatus{runningStatus("pergent")})
	}
	servePublish(env, 1, moved)
	e.hook(t, "update", 1, moved, head)
	if r := env.Run(id); r.State != store.Superseded {
		t.Fatalf("run = %s after the push; want superseded", r.State)
	}

	cleanup(t, env)
	cleanup(t, env)
	for _, m := range ms {
		if env.Job(m.ID) != nil {
			t.Errorf("reviewer %d's Job still exists", m.Reviewer)
		}
		if text, found, err := env.Store.ReviewMemberLog(env.ctx, m.ID); err != nil || !found || text != "member log\n" {
			t.Errorf("reviewer %d's log = %q, %v, %v; want kept before the deletion", m.Reviewer, text, found, err)
		}
	}
	for _, m := range reviewMembers(t, env, id) {
		if m.State != store.MemberSuperseded {
			t.Errorf("reviewer %d = %s; want superseded", m.Reviewer, m.State)
		}
	}
	if left, err := env.Store.ReviewMembersToCleanUp(env.ctx); err != nil || len(left) != 0 {
		t.Errorf("members to clean up = %d, %v; want none", len(left), err)
	}
	if got := statusesOf(t, env, head); len(got) != 2 || got[0] != "running: "+runningDescription || !strings.HasPrefix(got[1], "canceled: ") {
		t.Errorf("statuses = %v; want running, then canceled once", got)
	}
	if _, found := storedLog(t, env, id); found {
		t.Errorf("a member's log went to the run")
	}
}

// The sweep leaves a live multi review alone: its run waits, so its
// reviewers keep their Jobs and no status is set.
func TestE2EMultiReviewCleanupLeavesLive(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	cleanup(t, env)
	cleanup(t, env)
	for _, m := range ms {
		if env.Job(m.ID) == nil {
			t.Errorf("reviewer %d's Job was deleted", m.Reviewer)
		}
		if got := memberOf(t, env, id, m.Reviewer); got.State != store.MemberRunning {
			t.Errorf("reviewer %d = %s; want running", m.Reviewer, got.State)
		}
	}
	if left, err := env.Store.ReviewMembersToCleanUp(env.ctx); err != nil || len(left) != 0 {
		t.Errorf("members to clean up = %d, %v; want none", len(left), err)
	}
	wantWaiting(t, env, id)
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "running: "+runningDescription {
		t.Errorf("statuses = %v; want the running status alone", got)
	}
}

// The merge request's base moves after the split (on GitHub the base SHA
// follows the base branch): every reviewer still reviews the base the run
// recorded at the split, and their results for it are accepted.
func TestE2EMultiReviewSplitBase(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id := splitReview(t, env, 1, head)
	if r := env.Run(id); r.BaseSHA != sha('b') || r.StartSHA != sha('5') {
		t.Fatalf("run base %q start %q; want the split's", r.BaseSHA, r.StartSHA)
	}
	mr := openMR(1, head)
	mr.DiffRefs.BaseSHA, mr.DiffRefs.StartSHA = sha('9'), sha('8')
	env.GitLab.SetMR(mr)

	dispatch(t, env)
	for n := 1; n <= 3; n++ {
		m := memberOf(t, env, id, n)
		if job := env.Job(m.ID); job == nil || !hasArg(job.Spec.Template.Spec.Containers[0].Args, "--base="+sha('b')) {
			t.Fatalf("reviewer %d's Job does not review the split's base\n%s", n, env.Logs())
		}
		if code := e.postMember(t, id, m, clean(t, head)); code != http.StatusOK {
			t.Errorf("reviewer %d's result for the split's base: status %d\n%s", n, code, env.Logs())
		}
		if got := memberOf(t, env, id, n); got.State != store.MemberReported || got.Result == nil || got.FailureReason != "" {
			t.Errorf("reviewer %d = %s %q; want reported with its result", n, got.State, got.FailureReason)
		}
	}
}

// A whole review of 2 skills is split: the run records its skills as a
// review does, and each reviewer's result is checked against them. A
// reviewer with a skill more, or one less, fails; the right set is accepted.
// A reviewer's result on another review's route is refused.
func TestE2EMultiReviewSkills(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	env.AddSkill("style", "---\nname: style\n---\nCheck the style.\n")
	head := sha('a')
	// Both reviews are split by the same dispatch pass.
	servePublish(env, 2, head)
	two := env.Enqueue(2, head)
	one := splitReview(t, env, 1, head)
	for _, id := range []string{one, two} {
		if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitReviewers {
			t.Fatalf("run %s = %s/%q; want a multi review", id, r.State, r.SkipReason)
		}
		if r := env.Run(id); r.SkillSources["security"] != "library" || r.SkillSources["style"] != "library" || len(r.SkillSources) != 2 ||
			len(r.SkillHashes) != 2 || r.SkillSubset != nil {
			t.Fatalf("run %s skills = %v %v %v; want the 2 skills recorded at the split", id, r.SkillSources, r.SkillHashes, r.SkillSubset)
		}
	}
	dispatch(t, env)
	one1, one2 := memberOf(t, env, one, 1), memberOf(t, env, one, 2)
	two1, two2 := memberOf(t, env, two, 1), memberOf(t, env, two, 2)

	if code := e.postMember(t, one, one1, skillsResult(t, head, "security", "style", "extra")); code != http.StatusBadRequest {
		t.Errorf("a skill more: status %d; want 400", code)
	}
	wantMemberFailure(t, env, one, one1, unusable+"the result has skills the Job was not started with", "delivery")
	if code := e.postMember(t, two, two1, skillsResult(t, head, "security")); code != http.StatusBadRequest {
		t.Errorf("a skill less: status %d; want 400", code)
	}
	wantMemberFailure(t, env, two, two1, unusable+`the result lacks the skills the Job was started with ("style")`, "delivery")
	if code := e.postMember(t, one, one2, skillsResult(t, head, "style", "security")); code != http.StatusOK {
		t.Errorf("the right skills: status %d; want 200", code)
	}
	if got := memberOf(t, env, one, 2); got.State != store.MemberReported || got.Result == nil || got.FailureReason != "" {
		t.Errorf("reviewer 2 = %s %q; want reported with its result", got.State, got.FailureReason)
	}
	// Reviewer 2 of review 2 with its own token, on review 1's route.
	if code := e.postMember(t, one, two2, skillsResult(t, head, "security", "style")); code != http.StatusUnauthorized {
		t.Errorf("another review's route: status %d; want 401", code)
	}
	if got := memberOf(t, env, two, 2); got.State != store.MemberRunning || got.Result != nil {
		t.Errorf("reviewer 2 of review 2 = %s; want still running with no result", got.State)
	}
	for _, id := range []string{one, two} {
		wantWaiting(t, env, id)
	}
}

// A reviewer claimed but never given a Job goes back to the queue once its
// claim is five minutes old, with no attempt counted; a recent claim is left
// alone. The run still waits.
func TestRecoverUnlaunchedReviewMembers(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b"}}, []string{"team-b"})
	env := e.env
	id := splitReview(t, env, 1, sha('a'))
	two := memberOf(t, env, id, 2)
	execSQL(t, env, `UPDATE review_members SET not_before = now() + interval '1 day' WHERE id = '`+two.ID+`'`)
	if ms, err := env.Store.ClaimQueuedReviewMembers(env.ctx, 10, env.Now().Add(-6*time.Minute)); err != nil || len(ms) != 1 {
		t.Fatalf("ClaimQueuedReviewMembers = %d, %v; want reviewer 1", len(ms), err)
	}
	execSQL(t, env, `UPDATE review_members SET not_before = NULL WHERE id = '`+two.ID+`'`)
	if ms, err := env.Store.ClaimQueuedReviewMembers(env.ctx, 10, env.Now().Add(-time.Minute)); err != nil || len(ms) != 1 {
		t.Fatalf("ClaimQueuedReviewMembers = %d, %v; want reviewer 2", len(ms), err)
	}
	woken(env)

	watch(t, env)

	if got := memberOf(t, env, id, 1); got.State != store.MemberQueued || got.Attempts != 0 {
		t.Errorf("stranded reviewer 1 = %s with %d attempts; want queued with 0", got.State, got.Attempts)
	}
	if got := memberOf(t, env, id, 2); got.State != store.MemberRunning {
		t.Errorf("recent reviewer 2 = %s; want still running", got.State)
	}
	if !woken(env) {
		t.Errorf("the dispatcher was not woken after the recovery")
	}
	wantWaiting(t, env, id)
}

// The repository's token quota is used up after the split, before reviewer
// 3's Job starts: reviewer 3 fails with "the token quota" (the quota in its
// detail), while reviewers 1 and 2 started. The run still waits.
func TestE2EMultiReviewRepoQuotaStopsExtra(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	q := int64(100)
	env.Engine.Config.Repositories[0].MonthlyTokenQuota = &q
	id := splitReview(t, env, 1, sha('a'))
	three := memberOf(t, env, id, 3)
	execSQL(t, env, `UPDATE review_members SET not_before = now() + interval '1 day' WHERE id = '`+three.ID+`'`)
	dispatch(t, env)
	wantMemberKey(t, env, memberOf(t, env, id, 1), "key-team-a")
	wantMemberKey(t, env, memberOf(t, env, id, 2), "key-team-b")

	seedQuotaUsage(t, env, 100)
	execSQL(t, env, `UPDATE review_members SET not_before = NULL WHERE id = '`+three.ID+`'`)
	dispatch(t, env)

	got := memberOf(t, env, id, 3)
	if got.State != store.MemberReported || got.FailureReason != "the token quota" || got.FailureDetail != detailRepoQuota || got.JobName != "" {
		t.Errorf("reviewer 3 = %s %q (%q) job %q; want reported for the token quota with no Job",
			got.State, got.FailureReason, got.FailureDetail, got.JobName)
	}
	if env.Job(three.ID) != nil {
		t.Errorf("reviewer 3 has a Job")
	}
	wantWaiting(t, env, id)
}

// An extra reviewer reads the limits at its own dispatch: reviewer 2's only
// entry reaches its model limit after the split, and reviewer 2 fails with
// "the token quota" (each entry and why in the detail), never going past
// the limit. Reviewers 1 and 3 start.
func TestE2EMultiReviewExtraAtLimitAfterSplit(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, nil, nil)
	env := e.env
	env.SetNow(novemberMid)
	b := globalRef(t, env, "gpt-b", "key-b")
	c := globalRef(t, env, "gpt-c", "key-c")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}, {c}}, []store.EntryRef{c})
	seedEntryUsage(t, env, b, 10)
	id := splitReview(t, env, 1, sha('a'))
	setLimit(t, env, b, 0, 10)

	dispatch(t, env)
	got := memberOf(t, env, id, 2)
	if got.State != store.MemberReported || got.FailureReason != "the token quota" || got.JobName != "" {
		t.Errorf("reviewer 2 = %s %q job %q; want reported for the token quota with no Job", got.State, got.FailureReason, got.JobName)
	}
	if !strings.Contains(got.FailureDetail, "model limit used up") || strings.Contains(got.FailureDetail, "key-b") {
		t.Errorf("reviewer 2's detail = %q; want the limit, no key", got.FailureDetail)
	}
	wantMemberKey(t, env, memberOf(t, env, id, 1), "key-team-a")
	wantMemberKey(t, env, memberOf(t, env, id, 3), "key-c")
	wantWaiting(t, env, id)
}

// Reviewer 1 passed the hold with its run: an admin's model limit reached
// after the split does not stop it (it goes past, as a start does), but a
// team's own limit on its only entry fails it, naming the limit.
func TestE2EMultiReviewReviewer1PastAdminLimits(t *testing.T) {
	t.Run("admin limit", func(t *testing.T) {
		multiReviewFloor(t, "v0.5.0")
		e := newE2E(t)
		env := e.env
		env.SetNow(novemberMid)
		env.Engine.ScanImage = testScanImage
		teamOwning(t, env, "Payments")
		saveReviewers(t, env, [][]string{{"team-b"}}, []string{"team-b"})
		seedEntryUsage(t, env, configRef(), 10)
		id := splitReview(t, env, 1, sha('a'))
		setLimit(t, env, configRef(), 0, 10)
		dispatch(t, env)
		wantMemberKey(t, env, memberOf(t, env, id, 1), testLLMKey)
		wantMemberKey(t, env, memberOf(t, env, id, 2), "key-team-b")
	})
	t.Run("team's own limit", func(t *testing.T) {
		e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b"}}, []string{"team-b"})
		env := e.env
		env.SetNow(novemberMid)
		a := entryNamed(t, env, "team-a")
		seedEntryUsage(t, env, a, 10)
		id := splitReview(t, env, 1, sha('a'))
		setLimit(t, env, a, ownerTeam(t, env), 10)
		dispatch(t, env)
		got := memberOf(t, env, id, 1)
		if got.State != store.MemberReported || got.FailureReason != "all models exhausted: team-a (team limit used up)" || got.JobName != "" {
			t.Errorf("reviewer 1 = %s %q job %q; want reported as exhausted at the team's limit", got.State, got.FailureReason, got.JobName)
		}
		wantMemberKey(t, env, memberOf(t, env, id, 2), "key-team-b")
		wantWaiting(t, env, id)
	})
}

// A review falls back on chain A before it has extra reviewers, and its next
// attempt is split, on a base the merge request moved to meanwhile: the
// members go to the first run, the attempt waits on the moved base, and the
// reviewers review that base. Reviewer 1 skips the entry the first attempt
// tried, as the attempt did.
func TestE2EMultiReviewSplitAfterFallBack(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a", "team-a2")
	env := e.env
	env.Engine.ScanImage = testScanImage
	head, base := sha('a'), sha('9')
	servePublish(env, 1, head)
	env.Enqueue(1, head)
	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	wantFellBack(t, env, first, store.ClassQuota)

	saveReviewers(t, env, [][]string{{"team-b"}, {"team-c"}}, []string{"team-j"})
	mr := openMR(1, head)
	mr.DiffRefs.BaseSHA, mr.DiffRefs.StartSHA = base, sha('8')
	env.GitLab.SetMR(mr)
	dispatch(t, env)
	as, err := env.Store.Attempts(env.ctx, first)
	if err != nil || len(as) != 2 {
		t.Fatalf("attempts = %d, %v; want the first run and its split attempt", len(as), err)
	}
	split := as[1].ID
	if r := env.Run(split); r.State != store.Waiting || r.SkipReason != store.WaitReviewers || r.BaseSHA != base {
		t.Fatalf("split attempt = %s/%q base %q; want a multi review on the moved base\n%s", r.State, r.SkipReason, r.BaseSHA, env.Logs())
	}
	if r := env.Run(first); r.BaseSHA == base {
		t.Fatalf("the first run's base is the new one; the test needs an older one")
	}
	ms := reviewMembers(t, env, split)
	if len(ms) != 3 {
		t.Fatalf("members = %+v; want 3", ms)
	}
	for _, m := range ms {
		if m.RunID != first {
			t.Errorf("reviewer %d belongs to %s; want the first run %s", m.Reviewer, m.RunID, first)
		}
	}

	dispatch(t, env)
	for n, key := range []string{"key-team-a2", "key-team-b", "key-team-c"} {
		m := memberOf(t, env, first, n+1)
		wantMemberKey(t, env, m, key)
		if !hasArg(env.Job(m.ID).Spec.Template.Spec.Containers[0].Args, "--base="+base) {
			t.Errorf("reviewer %d's Job does not review the split's base", n+1)
		}
		if code := e.postMember(t, m.RunID, m, withBase(t, clean(t, head), base)); code != http.StatusOK {
			t.Fatalf("reviewer %d's result: status %d\n%s", n+1, code, env.Logs())
		}
	}
	wantWaiting(t, env, split)
}

// An API run waits on a queued legacy panel_members row (a main build left
// it): its dispatch reports the member failed with "an API review never runs
// multi review", starts no Job and leaves the run untouched.
func TestDispatchLegacyAPIPanelMemberFails(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	id := a.ask(t, "route:1", twoFiles())
	now := env.Now()
	if ms, err := env.Store.ClaimQueued(env.ctx, 10, now.Add(time.Hour)); err != nil || len(ms) != 1 {
		t.Fatalf("ClaimQueued = %d, %v", len(ms), err)
	}
	if started, err := env.Store.StartPanel(env.ctx, id, store.PanelSplit{BaseSHA: "base", StartSHA: "start"}, now); err != nil || !started {
		t.Fatalf("StartPanel = %v, %v", started, err)
	}
	before := env.Run(id)

	dispatch(t, env)
	ms, err := env.Store.PanelMembers(env.ctx, id)
	if err != nil || len(ms) != 2 {
		t.Fatalf("PanelMembers = %d, %v", len(ms), err)
	}
	for _, m := range ms {
		if m.State != store.MemberReported || m.FailureReason != reasonAPINoMultiReview || m.JobName != "" {
			t.Errorf("member %s = %s %q job %q; want reported %q with no Job", m.Role, m.State, m.FailureReason, m.JobName, reasonAPINoMultiReview)
		}
	}
	if n := len(env.Jobs()); n != 0 {
		t.Errorf("%d Jobs; want none", n)
	}
	if after := env.Run(id); !reflect.DeepEqual(before, after) {
		t.Errorf("the legacy member's dispatch wrote the run:\n before %+v\n after  %+v", before, after)
	}
}

// A merge request's legacy panel member found at dispatch starts no Job:
// its review is queued again as one review, as at start.
func TestDispatchLegacyPanelMemberRequeuesReview(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	id := env.Enqueue(3, sha('a'))
	if _, err := env.Store.ClaimQueued(env.ctx, 10, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if started, err := env.Store.StartPanel(env.ctx, id, store.PanelSplit{BaseSHA: "base", StartSHA: "start"}, now); err != nil || !started {
		t.Fatalf("StartPanel = %v, %v", started, err)
	}

	// The legacy step of DispatchOnce alone: the pass would then claim the
	// queued run as one review.
	if err := env.Engine.dispatchLegacyMembers(env.ctx, 10, nil); err != nil {
		t.Fatal(err)
	}
	if r := env.Run(id); r.State != store.Queued || r.SkipReason != "" {
		t.Errorf("legacy run = %s %q; want queued again as one review", r.State, r.SkipReason)
	}
	ms, err := env.Store.PanelMembers(env.ctx, id)
	if err != nil || len(ms) != 2 {
		t.Fatalf("PanelMembers = %d, %v", len(ms), err)
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
