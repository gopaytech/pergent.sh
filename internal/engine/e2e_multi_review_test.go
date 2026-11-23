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

	"github.com/jackc/pgx/v5"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/actions"
	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/instances"
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
	// The sweep leaves a live review alone when the run that waits is a
	// later attempt and the members belong to the first run.
	cleanup(t, env)
	if n := len(env.Jobs()); n != 3 {
		t.Errorf("%d Jobs after the sweep; want the 3 reviewers'", n)
	}
	for _, m := range reviewMembers(t, env, first) {
		if m.State != store.MemberRunning {
			t.Errorf("reviewer %d = %s after the sweep; want running", m.Reviewer, m.State)
		}
	}
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

	// The settle pass still fails such a review: it never waits for ever,
	// and it is never a pass.
	publish(t, env)
	if r := env.Run(id); r.State != store.Published || !strings.Contains(r.FailureReason, reasonAPINoMultiReview) {
		t.Errorf("run = %s %q; want published as failed", r.State, r.FailureReason)
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
			t.Errorf("%s %s reached the forge; want nothing posted for a reviewer", r.Method, r.Path)
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

// marksAudited is the number of model_marked entries in the audit log.
func marksAudited(t *testing.T, env *testEnv) int {
	t.Helper()
	entries, err := env.Store.AuditLog(env.ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == "model_marked" {
			n++
		}
	}
	return n
}

// wantReleased checks run id was released for its judge: queued again with
// no skip reason, due now, and otherwise as it was before (no failure, no
// Job, no result).
func wantReleased(t *testing.T, env *testEnv, id string, before store.Run) {
	t.Helper()
	after := env.Run(id)
	if after.State != store.Queued || after.SkipReason != "" || !after.NotBefore.IsZero() || after.FailureReason != "" {
		t.Fatalf("run = %s/%q %q; want released for its judge\n%s", after.State, after.SkipReason, after.FailureReason, env.Logs())
	}
	after.State, after.SkipReason, after.NotBefore = before.State, before.SkipReason, before.NotBefore
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the release wrote more than the run's state:\n before %+v\n after  %+v", before, after)
	}
}

// attemptsOfReviewer is every attempt of reviewer n of run id's review,
// oldest first.
func attemptsOfReviewer(t *testing.T, env *testEnv, id string, n int) []store.ReviewMember {
	t.Helper()
	var out []store.ReviewMember
	for _, m := range reviewMembers(t, env, id) {
		if m.Reviewer == n {
			out = append(out, m)
		}
	}
	return out
}

// wantFailedKeepingResult checks reviewer n of run id's review has
// attempts attempts and its newest one is failed for reason with its
// result kept.
func wantFailedKeepingResult(t *testing.T, env *testEnv, id string, n, attempts int, reason string) {
	t.Helper()
	as := attemptsOfReviewer(t, env, id, n)
	if len(as) != attempts {
		t.Fatalf("reviewer %d has %d attempts; want %d (no next attempt)\n%s", n, len(as), attempts, env.Logs())
	}
	got := as[len(as)-1]
	if got.State != store.MemberFailed || got.FailureReason != reason || got.Result == nil {
		t.Errorf("reviewer %d = %s %q (result kept: %v); want failed %q with its result kept",
			n, got.State, got.FailureReason, got.Result != nil, reason)
	}
}

// Between the reviewers' results and the judge Job nothing is posted for a
// reviewer: no comment, note, thread, approval or status. Settling writes
// nothing to the platform: reviewer 1 is done, reviewer 2 failed (its
// result is for another commit), reviewer 3 failed (its review failed), and
// the run is released (queued again, its other fields as the split left
// them). The judge Job is M3.7's.
func TestE2EMultiReviewMemberPostsNothing(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	before := env.Run(id)
	env.GitLab.Reset()

	if code := e.postMember(t, id, ms[0], clean(t, head)); code != http.StatusOK {
		t.Fatalf("reviewer 1's result: status %d\n%s", code, env.Logs())
	}
	if code := e.postMember(t, id, ms[1], clean(t, sha('f'))); code != http.StatusBadRequest {
		t.Fatalf("reviewer 2's result: status %d; want 400", code)
	}
	if code := e.postMember(t, id, ms[2], errorResult(t, head, "the model gave no answer")); code != http.StatusOK {
		t.Fatalf("reviewer 3's result: status %d", code)
	}
	for i := 0; i < 2; i++ {
		watch(t, env)
		publish(t, env)
		cleanup(t, env)
	}
	wantNothingPosted(t, env)
	if got := memberOf(t, env, id, 1); got.State != store.MemberDone || got.Result == nil {
		t.Errorf("reviewer 1 = %s; want done with its result", got.State)
	}
	if got := memberOf(t, env, id, 2); got.State != store.MemberFailed ||
		!strings.Contains(got.FailureReason, "the result is for a different commit than the one this run reviews") {
		t.Errorf("reviewer 2 = %s %q; want failed for the commit", got.State, got.FailureReason)
	}
	if got := memberOf(t, env, id, 3); got.State != store.MemberFailed || got.FailureReason != "the model gave no answer" || got.Result != nil {
		t.Errorf("reviewer 3 = %s %q; want failed with the review's message", got.State, got.FailureReason)
	}
	wantReleased(t, env, id, before)
}

// Reviewer 2's entry is out of quota: the entry is marked and audited as a
// run's is, the member is fell_back, and its next attempt (attempt_of the
// first) runs on reviewer 2's next frozen entry in a new Job with the full
// timeout. Reviewers 1 and 3 are untouched, the run still waits, and
// nothing is posted, not even a status.
func TestE2EMultiReviewMemberFallsBack(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b", "team-b2"}, {"team-c"}}, []string{"team-j"})
	env := e.env
	b, b2 := entryNamed(t, env, "team-b"), entryNamed(t, env, "team-b2")
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	two := ms[1]
	deadline := jobDeadline(t, env, two.ID)
	before := env.Run(id)
	env.GitLab.Reset()

	if code := e.postMember(t, id, ms[0], clean(t, head)); code != http.StatusOK {
		t.Fatalf("reviewer 1's result: status %d", code)
	}
	if code := e.postMember(t, id, two, failedOn(t, head, "quota")); code != http.StatusOK {
		t.Fatalf("reviewer 2's result: status %d\n%s", code, env.Logs())
	}
	woken(env)
	publish(t, env)

	if got := memberOf(t, env, id, 1); got.ID != ms[0].ID || got.State != store.MemberDone {
		t.Errorf("reviewer 1 = %s %s; want the same member, done", got.ID, got.State)
	}
	if got := memberOf(t, env, id, 3); got.ID != ms[2].ID || got.State != store.MemberRunning {
		t.Errorf("reviewer 3 = %s %s; want the same member, running", got.ID, got.State)
	}
	if m, ok := markOf(t, env, b); !ok || m.Class != store.ClassQuota || m.Until == nil || !m.Until.Equal(env.Now().Add(time.Hour)) {
		t.Errorf("reviewer 2's mark = %+v, %v; want quota until now+1h", m, ok)
	}
	if _, ok := markOf(t, env, b2); ok {
		t.Errorf("reviewer 2's next entry is marked")
	}
	entries, err := env.Store.AuditLog(env.ctx, 0, 10)
	if err != nil || len(entries) == 0 || entries[0].Action != "model_marked" || entries[0].Actor != "pergent" ||
		entries[0].Detail != "model=team-b class=quota" || entries[0].Project != testProjectPath {
		t.Errorf("audit = %+v, %v", entries, err)
	}
	as := attemptsOfReviewer(t, env, id, 2)
	if len(as) != 2 || as[0].ID != two.ID || as[0].State != store.MemberFellBack {
		t.Fatalf("reviewer 2's attempts = %+v; want the first fell_back and a next one", as)
	}
	next := as[1]
	if next.State != store.MemberQueued || next.AttemptOf == nil || *next.AttemptOf != two.ID || !reflect.DeepEqual(next.Chain, two.Chain) {
		t.Fatalf("reviewer 2's next attempt = %+v; want queued, attempt of the first, with its frozen chain", next)
	}
	wantWaiting(t, env, id)
	if !woken(env) {
		t.Errorf("the dispatcher was not woken for reviewer 2's next attempt")
	}

	dispatch(t, env)
	next = memberOf(t, env, id, 2)
	wantMemberKey(t, env, next, "key-team-b2")
	if next.Entry.ID != b2.ID || next.Entry.Position != 2 || next.Entry.Length != 2 {
		t.Errorf("reviewer 2's next entry = %+v; want the second of 2", next.Entry)
	}
	if got := jobDeadline(t, env, next.ID); got != deadline {
		t.Errorf("reviewer 2's next Job's deadline = %ds; want the full %ds", got, deadline)
	}
	if n := len(reviewMembers(t, env, id)); n != 4 {
		t.Errorf("%d members; want 3 reviewers and reviewer 2's second attempt", n)
	}
	wantNothingPosted(t, env)
	if after := env.Run(id); !reflect.DeepEqual(before, after) {
		t.Errorf("the fallback wrote the run:\n before %+v\n after  %+v", before, after)
	}
}

// Reviewer 2's next frozen entry runs on reviewer 1's model: its next
// attempt skips it and takes the one after. With no entry after it, no
// different model is left: reviewer 2 fails with its result kept, and no
// next attempt is queued.
func TestE2EMultiReviewFallbackSkipsEarlierModel(t *testing.T) {
	setup := func(t *testing.T, names ...string) (*e2e, string, store.ReviewMember, store.ReviewMember) {
		e := newMultiReviewE2E(t, []string{"team-a"}, nil, nil)
		env := e.env
		team := ownerTeam(t, env)
		var refs []store.EntryRef
		for _, n := range names {
			model := "gpt-" + n
			if n == "team-same" {
				model = "GPT-TEAM-A"
			}
			refs = append(refs, addTeamModel(t, env, team, n, model, "key-"+n))
		}
		saveExtraReviewers(t, env, [][]store.EntryRef{refs}, refs[:1])
		id := splitReview(t, env, 1, sha('a'))
		dispatch(t, env)
		one, two := memberOf(t, env, id, 1), memberOf(t, env, id, 2)
		wantMemberKey(t, env, one, "key-team-a")
		wantMemberKey(t, env, two, "key-"+names[0])
		return e, id, one, two
	}
	head := sha('a')
	t.Run("the one after", func(t *testing.T) {
		e, id, one, two := setup(t, "team-b", "team-same", "team-after")
		env := e.env
		if code := e.postMember(t, id, two, failedOn(t, head, "billing")); code != http.StatusOK {
			t.Fatalf("reviewer 2's result: status %d", code)
		}
		publish(t, env)
		dispatch(t, env)
		next := memberOf(t, env, id, 2)
		if next.ID == two.ID {
			t.Fatalf("reviewer 2 did not fall back\n%s", env.Logs())
		}
		wantMemberKey(t, env, next, "key-team-after")
		if next.Entry.Position != 3 {
			t.Errorf("reviewer 2's next entry = %+v; want the third, past reviewer 1's model", next.Entry)
		}
		if env.Job(one.ID) == nil {
			t.Errorf("reviewer 1 has no Job")
		}
	})
	t.Run("no different model left", func(t *testing.T) {
		e, id, one, two := setup(t, "team-b", "team-same")
		env := e.env
		before := env.Run(id)
		env.GitLab.Reset()
		if code := e.postMember(t, id, one, clean(t, head)); code != http.StatusOK {
			t.Fatalf("reviewer 1's result: status %d", code)
		}
		if code := e.postMember(t, id, two, failedOn(t, head, "auth")); code != http.StatusOK {
			t.Fatalf("reviewer 2's result: status %d", code)
		}
		publish(t, env)
		wantFailedKeepingResult(t, env, id, 2, 1, "key rejected")
		if m, ok := markOf(t, env, entryNamed(t, env, "team-b")); !ok || m.Class != store.ClassAuth {
			t.Errorf("reviewer 2's mark = %+v, %v; want auth", m, ok)
		}
		if n := marksAudited(t, env); n != 1 {
			t.Errorf("%d model_marked entries; want 1", n)
		}
		wantReleased(t, env, id, before)
		wantNothingPosted(t, env)
	})
}

// An extra reviewer out of tokens. (a) Reviewer 3's 2-entry chain: both
// entries are refused; after the second refusal reviewer 3 is failed with
// its result kept, no third attempt is queued, and the run is released.
// (b) Reviewer 3's 1-entry chain is refused: it never falls back, so
// nothing is marked and the member is done with its result.
func TestE2EMultiReviewExtraOutOfTokens(t *testing.T) {
	head := sha('a')
	t.Run("2 entries", func(t *testing.T) {
		e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b"}, {"team-c", "team-c2"}}, []string{"team-j"})
		env := e.env
		c, c2 := entryNamed(t, env, "team-c"), entryNamed(t, env, "team-c2")
		id, ms := startReviewers(t, e, head)
		before := env.Run(id)
		for _, m := range ms[:2] {
			if code := e.postMember(t, id, m, clean(t, head)); code != http.StatusOK {
				t.Fatalf("reviewer %d's result: status %d", m.Reviewer, code)
			}
		}
		if code := e.postMember(t, id, ms[2], failedOn(t, head, "quota")); code != http.StatusOK {
			t.Fatalf("reviewer 3's result: status %d", code)
		}
		publish(t, env)
		wantWaiting(t, env, id)
		dispatch(t, env)
		next := memberOf(t, env, id, 3)
		wantMemberKey(t, env, next, "key-team-c2")
		if code := e.postMember(t, id, next, failedOn(t, head, "quota")); code != http.StatusOK {
			t.Fatalf("reviewer 3's second result: status %d", code)
		}
		publish(t, env)

		wantFailedKeepingResult(t, env, id, 3, 2, "out of quota")
		for _, ref := range []store.EntryRef{c, c2} {
			if m, ok := markOf(t, env, ref); !ok || m.Class != store.ClassQuota {
				t.Errorf("mark of %s = %+v, %v; want quota", ref.ID, m, ok)
			}
		}
		if n := marksAudited(t, env); n != 2 {
			t.Errorf("%d model_marked entries; want 2", n)
		}
		wantReleased(t, env, id, before)
	})
	t.Run("1 entry", func(t *testing.T) {
		e := newThreeReviewers(t)
		env := e.env
		id, ms := startReviewers(t, e, head)
		before := env.Run(id)
		for _, m := range ms[:2] {
			if code := e.postMember(t, id, m, clean(t, head)); code != http.StatusOK {
				t.Fatalf("reviewer %d's result: status %d", m.Reviewer, code)
			}
		}
		if code := e.postMember(t, id, ms[2], failedOn(t, head, "quota")); code != http.StatusOK {
			t.Fatalf("reviewer 3's result: status %d", code)
		}
		publish(t, env)
		as := attemptsOfReviewer(t, env, id, 3)
		if len(as) != 1 || as[0].State != store.MemberDone || as[0].Result == nil {
			t.Fatalf("reviewer 3 = %+v; want one attempt, done with its result", as)
		}
		if _, ok := markOf(t, env, entryNamed(t, env, "team-c")); ok {
			t.Errorf("a 1-entry chain's entry is marked")
		}
		if n := marksAudited(t, env); n != 0 {
			t.Errorf("%d model_marked entries; want none", n)
		}
		wantReleased(t, env, id, before)
	})
}

// Every reviewer fails (reviewer 1's review failed, reviewer 2's result is
// for another commit, reviewer 3 ran out of tokens on both entries and kept
// its partial result): the review is published as failed with each
// reviewer's reason in the same pass. The status is the failure status,
// nothing is approved, and success is never set.
func TestE2EMultiReviewEveryReviewerFails(t *testing.T) {
	e := newHiddenE2E(t, []string{"rev-one"}, [][]string{{"rev-two"}, {"rev-three", "rev-three-b"}}, []string{"the-judge"})
	env := e.env
	head := sha('a')
	id := splitReview(t, env, 1, head)
	dispatch(t, env)
	var ms []store.ReviewMember
	for n, name := range []string{"rev-one", "rev-two", "rev-three"} {
		m := memberOf(t, env, id, n+1)
		wantMemberKey(t, env, m, "key-"+name)
		ms = append(ms, m)
	}
	if code := e.postMember(t, id, ms[2], failedOn(t, head, "quota")); code != http.StatusOK {
		t.Fatalf("reviewer 3's result: status %d", code)
	}
	publish(t, env)
	dispatch(t, env)
	if code := e.postMember(t, id, memberOf(t, env, id, 3), failedOn(t, head, "quota")); code != http.StatusOK {
		t.Fatalf("reviewer 3's second result: status %d", code)
	}
	// Reviewer 1's error is the CLI's own text: it names every reviewer's
	// provider, model and base URL.
	if code := e.postMember(t, id, ms[0], errorResult(t, head, modelText("rev-one", "rev-two", "rev-three", "rev-three-b")+" gave no answer")); code != http.StatusOK {
		t.Fatalf("reviewer 1's result: status %d\n%s", code, env.Logs())
	}
	if code := e.postMember(t, id, ms[1], clean(t, sha('f'))); code != http.StatusBadRequest {
		t.Fatalf("reviewer 2's result: status %d; want 400", code)
	}
	publish(t, env)

	wantFailedKeepingResult(t, env, id, 3, 2, "out of quota")
	for n := 1; n <= 3; n++ {
		if got := memberOf(t, env, id, n); got.State != store.MemberFailed {
			t.Errorf("reviewer %d = %s; want failed", n, got.State)
		}
	}
	wantPublished(t, env, id)
	r := env.Run(id)
	want := "every reviewer failed: reviewer 1 (rev-one): " + modelText("rev-one", "rev-two", "rev-three", "rev-three-b") + " gave no answer; reviewer 2 (rev-two): " + unusable +
		"the result is for a different commit than the one this run reviews; reviewer 3 (rev-three-b): out of quota"
	if r.FailureReason != want || r.Result != nil {
		t.Errorf("run failure = %q (result %v); want %q and no result", r.FailureReason, r.Result != nil, want)
	}
	// The comment states the failure in fixed words and names no reviewer's
	// provider, model or base URL (Ruling P55); the stored reason keeps
	// them for the run page.
	body := createdComment(t, env, 1)
	wantNoModelText(t, body)
	for _, words := range []string{"every reviewer failed", "gave no answer", "the result is for a different commit", "out of quota"} {
		if !strings.Contains(body, words) {
			t.Errorf("the comment lacks %q:\n%s", words, body)
		}
	}
	got := statusesOf(t, env, head)
	if len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "failed: ") {
		t.Errorf("statuses = %v; want the failure status last", got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "success") {
			t.Errorf("statuses = %v; a failed multi review set success", got)
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

// Reviewer 1's review fails and the others pass: the review is released for
// its judge, reviewer 1 failed with its reason. The release fires once: a
// second settle pass changes nothing and wakes nothing.
func TestE2EMultiReviewReviewer1FailsOthersDone(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	before := env.Run(id)
	env.GitLab.Reset()

	if code := e.postMember(t, id, ms[0], errorResult(t, head, "")); code != http.StatusOK {
		t.Fatalf("reviewer 1's result: status %d", code)
	}
	for _, m := range ms[1:] {
		if code := e.postMember(t, id, m, clean(t, head)); code != http.StatusOK {
			t.Fatalf("reviewer %d's result: status %d", m.Reviewer, code)
		}
	}
	woken(env)
	publish(t, env)
	if got := memberOf(t, env, id, 1); got.ID != ms[0].ID || got.State != store.MemberFailed || got.FailureReason != reasonMemberError || got.Result != nil {
		t.Errorf("reviewer 1 = %s %q; want failed with the review's failure", got.State, got.FailureReason)
	}
	for _, m := range ms[1:] {
		if got := memberOf(t, env, id, m.Reviewer); got.ID != m.ID || got.State != store.MemberDone || got.Result == nil {
			t.Errorf("reviewer %d = %s; want done with its result", m.Reviewer, got.State)
		}
	}
	wantReleased(t, env, id, before)
	if !woken(env) {
		t.Errorf("the dispatcher was not woken for the judge")
	}

	released := env.Run(id)
	members := reviewMembers(t, env, id)
	publish(t, env)
	if again := env.Run(id); !reflect.DeepEqual(released, again) {
		t.Errorf("a second pass changed the run:\n before %+v\n after  %+v", released, again)
	}
	if again := reviewMembers(t, env, id); !reflect.DeepEqual(members, again) {
		t.Errorf("a second pass changed the members")
	}
	if woken(env) {
		t.Errorf("a second pass woke the engine again")
	}
	wantNothingPosted(t, env)
}

// Review Focus 3: a team entry is deleted while a review runs. The live
// reviewer chains lose it and renumber (reviewer 2's only entry was it, so
// reviewer 3's chain moves to r2); the running review keeps it in its
// copies. Reviewer 2 still finishes on it, and reviewer 3's next attempt
// skips it as removed.
func TestE2EMultiReviewTeamEntryDeletedMidReview(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-x"}, {"team-b", "team-x", "team-c"}}, []string{"team-j"})
	env := e.env
	b, x, c := entryNamed(t, env, "team-b"), entryNamed(t, env, "team-x"), entryNamed(t, env, "team-c")
	head := sha('a')
	id := splitReview(t, env, 1, head)
	dispatch(t, env)
	one, two, three := memberOf(t, env, id, 1), memberOf(t, env, id, 2), memberOf(t, env, id, 3)
	wantMemberKey(t, env, two, "key-team-x")
	wantMemberKey(t, env, three, "key-team-b")

	if _, _, err := env.Store.DeleteTeamModel(env.ctx, ownerTeam(t, env), x.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.Instances.LoadLLM(env.ctx); err != nil {
		t.Fatal(err)
	}
	live, err := env.Store.Reviewers(env.ctx, testInstance, testProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Extra) != 1 || live.Extra[0].Slot != store.SlotOf(2) || !slices.Equal(live.Extra[0].Entries, []store.EntryRef{b, c}) {
		t.Fatalf("live reviewers = %+v; want reviewer 3's chain at r2 without the entry", live.Extra)
	}
	if frozen := memberOf(t, env, id, 3).Chain; len(frozen.Entries) != 3 || frozen.Entries[1].ID != x.ID || frozen.Entries[1].Name != "team-x" {
		t.Fatalf("reviewer 3's copy = %+v; want it to keep the entry", frozen)
	}

	for _, m := range []store.ReviewMember{one, two} {
		if code := e.postMember(t, id, m, clean(t, head)); code != http.StatusOK {
			t.Fatalf("reviewer %d's result: status %d", m.Reviewer, code)
		}
	}
	if code := e.postMember(t, id, three, failedOn(t, head, "quota")); code != http.StatusOK {
		t.Fatalf("reviewer 3's result: status %d", code)
	}
	publish(t, env)
	if got := memberOf(t, env, id, 2); got.State != store.MemberDone {
		t.Errorf("reviewer 2 = %s; want done on the deleted entry", got.State)
	}
	dispatch(t, env)
	next := memberOf(t, env, id, 3)
	wantMemberKey(t, env, next, "key-team-c")
	if next.Entry.ID != c.ID || next.Entry.Position != 3 || next.Entry.Length != 3 {
		t.Errorf("reviewer 3's next entry = %+v; want the third of its 3, past the removed one", next.Entry)
	}
	known := map[store.EntryRef]instances.Model{}
	for _, m := range env.Engine.registry().AllModels() {
		known[chain.RefOf(m)] = m
	}
	if why := chain.EntryReason(chain.FromFrozen(next.Chain, chain.Input{Entries: known}), x); why != chain.ReasonRemoved {
		t.Errorf("the deleted entry reads as %q in reviewer 3's copy; want %q", why, chain.ReasonRemoved)
	}
	wantWaiting(t, env, id)
}

// A stored result is checked again when its member is settled, since the
// result handler stores a result before it checks it: one for another
// commit fails the member with the check's reason and never becomes done;
// good ones are done, and the review is released.
func TestE2EMultiReviewSettleChecksResult(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	// Stored as the handler stores them, before its check.
	for i, body := range [][]byte{clean(t, sha('f')), clean(t, head), clean(t, head)} {
		m := ms[i]
		token := string(env.Secret(m.ID).Data["result-token"])
		if out, err := env.Store.StoreReviewMemberResult(env.ctx, id, m.ID, token, body, env.Now()); err != nil || out != store.ResultStored {
			t.Fatalf("StoreReviewMemberResult = %v, %v", out, err)
		}
	}

	publish(t, env)
	got := memberOf(t, env, id, 1)
	if got.State != store.MemberFailed || got.Result != nil || got.FailureStep != "delivery" ||
		got.FailureReason != unusable+"the result is for a different commit than the one this run reviews" {
		t.Errorf("reviewer 1 = %s %q at %q; want failed by the check", got.State, got.FailureReason, got.FailureStep)
	}
	for _, n := range []int{2, 3} {
		if got := memberOf(t, env, id, n); got.State != store.MemberDone || got.Result == nil {
			t.Errorf("reviewer %d = %s; want done", n, got.State)
		}
	}
	if r := env.Run(id); r.State != store.Queued {
		t.Errorf("run = %s; want released with reviewers 2 and 3", r.State)
	}
}

// A reviewer's entry is refused with no entry left, and the settle that
// fails the member errors after the mark: the next settle marks it again,
// but the audit entry is written once, by the settle that failed the
// member.
func TestE2EMultiReviewMemberMarkAuditedOnce(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b", "team-a"}}, []string{"team-b"})
	env := e.env
	head := sha('a')
	id := splitReview(t, env, 1, head)
	dispatch(t, env)
	one, two := memberOf(t, env, id, 1), memberOf(t, env, id, 2)
	if code := e.postMember(t, id, one, clean(t, head)); code != http.StatusOK {
		t.Fatalf("reviewer 1's result: status %d", code)
	}
	if code := e.postMember(t, id, two, failedOn(t, head, "quota")); code != http.StatusOK {
		t.Fatalf("reviewer 2's result: status %d", code)
	}

	// The member's failure can't be written: the settle errors after the mark.
	execSQL(t, env, `CREATE FUNCTION fail_keep() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF OLD.state = 'reported' AND NEW.state = 'failed' AND NEW.result IS NOT NULL THEN RAISE EXCEPTION 'fail failed'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_keep BEFORE UPDATE ON review_members FOR EACH ROW EXECUTE FUNCTION fail_keep()`)
	publish(t, env)
	if got := memberOf(t, env, id, 2); got.State != store.MemberReported {
		t.Fatalf("reviewer 2 = %s; want still reported after the failed settle\n%s", got.State, env.Logs())
	}
	if m, ok := markOf(t, env, entryNamed(t, env, "team-b")); !ok || m.Class != store.ClassQuota {
		t.Errorf("reviewer 2's mark = %+v, %v; want quota", m, ok)
	}
	execSQL(t, env, `DROP TRIGGER fail_keep ON review_members; DROP FUNCTION fail_keep()`)
	publish(t, env)

	wantFailedKeepingResult(t, env, id, 2, 1, "out of quota")
	if n := marksAudited(t, env); n != 1 {
		t.Errorf("%d model_marked entries; want 1", n)
	}
}

// Ruling L6: the settle's "no entry left" check resolves as the reviewer's
// next dispatch does, with the new mark in place. Reviewer 2 is refused and
// its backup is at its model limit: reviewer 2 never goes past a limit, so
// no entry is left; it is failed with its result kept and no next attempt
// starts. Reviewer 1 is refused with its backup at the same limit: the live
// mark of the entry it fell back from fails such a fallback, as a run's
// (TestFallbackFailsOnMarkAndLimit), so its next dispatch would fail too;
// it is failed with its result kept, never with its result lost.
func TestE2EMultiReviewFallbackBackupAtLimit(t *testing.T) {
	head := sha('a')
	t.Run("reviewer 2", func(t *testing.T) {
		e := newMultiReviewE2E(t, []string{"team-a"}, nil, nil)
		env := e.env
		env.SetNow(novemberMid)
		b := globalRef(t, env, "gpt-b", "key-b")
		b2 := globalRef(t, env, "gpt-b2", "key-b2")
		saveExtraReviewers(t, env, [][]store.EntryRef{{b, b2}}, []store.EntryRef{b})
		seedEntryUsage(t, env, b2, 10)
		id := splitReview(t, env, 1, head)
		setLimit(t, env, b2, 0, 10)
		dispatch(t, env)
		one, two := memberOf(t, env, id, 1), memberOf(t, env, id, 2)
		wantMemberKey(t, env, two, "key-b")
		before := env.Run(id)
		if code := e.postMember(t, id, one, clean(t, head)); code != http.StatusOK {
			t.Fatalf("reviewer 1's result: status %d", code)
		}
		if code := e.postMember(t, id, two, failedOn(t, head, "quota")); code != http.StatusOK {
			t.Fatalf("reviewer 2's result: status %d", code)
		}
		publish(t, env)
		wantFailedKeepingResult(t, env, id, 2, 1, "out of quota")
		if m, ok := markOf(t, env, b); !ok || m.Class != store.ClassQuota {
			t.Errorf("reviewer 2's mark = %+v, %v; want quota", m, ok)
		}
		if n := marksAudited(t, env); n != 1 {
			t.Errorf("%d model_marked entries; want 1", n)
		}
		wantReleased(t, env, id, before)
	})
	t.Run("reviewer 1", func(t *testing.T) {
		multiReviewFloor(t, "v0.5.0")
		e := newE2E(t)
		env := e.env
		env.SetNow(novemberMid)
		env.Engine.ScanImage = testScanImage
		teamOwning(t, env, "Payments")
		a2 := globalRef(t, env, "gpt-a2", "key-a2")
		saveChain(t, env, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{configRef(), a2}})
		saveReviewers(t, env, [][]string{{"team-b"}}, []string{"team-b"})
		seedEntryUsage(t, env, a2, 10)
		id := splitReview(t, env, 1, head)
		setLimit(t, env, a2, 0, 10)
		dispatch(t, env)
		one, two := memberOf(t, env, id, 1), memberOf(t, env, id, 2)
		wantMemberKey(t, env, one, testLLMKey)
		before := env.Run(id)
		if code := e.postMember(t, id, two, clean(t, head)); code != http.StatusOK {
			t.Fatalf("reviewer 2's result: status %d", code)
		}
		if code := e.postMember(t, id, one, failedOn(t, head, "quota")); code != http.StatusOK {
			t.Fatalf("reviewer 1's result: status %d", code)
		}
		publish(t, env)
		wantFailedKeepingResult(t, env, id, 1, 1, "out of quota")
		if m, ok := markOf(t, env, configRef()); !ok || m.Class != store.ClassQuota {
			t.Errorf("reviewer 1's mark = %+v, %v; want quota", m, ok)
		}
		wantReleased(t, env, id, before)
	})
}

// A split whose extra reviewers were all skipped has reviewer 1 alone (M3.4
// never makes one, the store accepts it). It never waits for ever: with
// reviewer 1 done it is released for its judge (one result to merge); with
// reviewer 1 failed the review fails, naming each reviewer.
func TestE2EMultiReviewOnlyReviewer1Left(t *testing.T) {
	head := sha('a')
	setup := func(t *testing.T) (*e2e, string, store.ReviewMember) {
		e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b"}}, []string{"team-b"})
		env := e.env
		id := splitReview(t, env, 1, head)
		two := memberOf(t, env, id, 2)
		execSQL(t, env, `UPDATE review_members SET state = 'skipped', failure_reason = 'no usable entry' WHERE id = '`+two.ID+`'`)
		dispatch(t, env)
		one := memberOf(t, env, id, 1)
		wantMemberKey(t, env, one, "key-team-a")
		if env.Job(two.ID) != nil {
			t.Fatalf("the skipped reviewer has a Job")
		}
		return e, id, one
	}
	t.Run("done", func(t *testing.T) {
		e, id, one := setup(t)
		before := e.env.Run(id)
		if code := e.postMember(t, id, one, clean(t, head)); code != http.StatusOK {
			t.Fatalf("reviewer 1's result: status %d", code)
		}
		publish(t, e.env)
		wantReleased(t, e.env, id, before)
	})
	t.Run("failed", func(t *testing.T) {
		e, id, one := setup(t)
		if code := e.postMember(t, id, one, errorResult(t, head, "the model gave no answer")); code != http.StatusOK {
			t.Fatalf("reviewer 1's result: status %d", code)
		}
		publish(t, e.env)
		wantPublished(t, e.env, id)
		want := "every reviewer failed: reviewer 1 (team-a): the model gave no answer; reviewer 2 (team-b): no usable entry"
		if r := e.env.Run(id); r.FailureReason != want {
			t.Errorf("run failure = %q; want %q", r.FailureReason, want)
		}
	})
}

// Ruling P44: the chain helpers read the base they are given. A zero base
// (no entries read) is an error, never a quiet "no entry" or "no model
// before".
func TestReviewerDecisionNeedsBase(t *testing.T) {
	ms := []store.ReviewMember{{ID: "1", Reviewer: 1, State: store.MemberRunning,
		Chain: store.FrozenChain{Entries: []store.FrozenEntry{{Source: store.SourceGlobal, ID: "a"}}}}}
	m := store.ReviewMember{ID: "2", Reviewer: 2, State: store.MemberQueued,
		Chain: store.FrozenChain{Entries: []store.FrozenEntry{{Source: store.SourceGlobal, ID: "b"}}}}
	if _, err := memberInput(chain.Input{}, ms, m); !errors.Is(err, errNoBase) {
		t.Errorf("memberInput with no base: %v; want errNoBase", err)
	}
	if _, err := modelsBefore(chain.Input{}, ms, 2, false); !errors.Is(err, errNoBase) {
		t.Errorf("modelsBefore with no base: %v; want errNoBase", err)
	}
	if _, _, err := reviewerDecision(chain.Input{}, ms, m, false, false); !errors.Is(err, errNoBase) {
		t.Errorf("reviewerDecision with no base: %v; want errNoBase", err)
	}
	base := chain.Input{Entries: map[store.EntryRef]instances.Model{
		{Source: store.SourceGlobal, ID: "a"}: {Name: "a", Key: "k", LLM: config.LLM{Provider: "openai", Model: "gpt-a"}},
		{Source: store.SourceGlobal, ID: "b"}: {Name: "b", Key: "k", LLM: config.LLM{Provider: "openai", Model: "gpt-b"}},
	}}
	dec, _, err := reviewerDecision(base, ms, m, false, false)
	if err != nil || !dec.Runs() || dec.Ref.ID != "b" {
		t.Errorf("reviewerDecision = %+v, %v; want entry b", dec, err)
	}

	// One rule for a dispatch and the settle (Rulings L5, L6): at an admin's
	// model limit reviewer 1 goes past it, and reviewer 2 holds, which does
	// not run.
	base.ModelLimitUsedUp = map[store.EntryRef]bool{{Source: store.SourceGlobal, ID: "a"}: true, {Source: store.SourceGlobal, ID: "b"}: true}
	if dec, _, err := reviewerDecision(base, nil, ms[0], false, false); err != nil || !dec.Runs() || dec.Ref.ID != "a" {
		t.Errorf("reviewer 1 at an admin limit = %+v, %v; want it past the limit on a", dec, err)
	}
	if dec, _, err := reviewerDecision(base, ms, m, false, false); err != nil || dec.Runs() || !dec.Hold {
		t.Errorf("reviewer 2 at a limit = %+v, %v; want a hold, which does not run", dec, err)
	}
	if dec, _, err := reviewerDecision(base, ms, m, true, false); err != nil || dec.Runs() {
		t.Errorf("reviewer 2 under a used-up repository quota = %+v, %v; want no entry", dec, err)
	}
}

// Reviewer 2's Job completes without posting a result: after the grace it
// is reported with reasonFinishedNoSend at the delivery step; the run still
// waits and the others run on.
func TestE2EMultiReviewMemberFinishedNoResult(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	id, ms := startReviewers(t, e, sha('a'))
	two := ms[1]
	finished := env.Now()
	replaceJob(t, env, two.ID, finished.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", finished))

	env.SetNow(finished.Add(30 * time.Second))
	watch(t, env)
	if got := memberOf(t, env, id, 2); got.State != store.MemberRunning {
		t.Errorf("reviewer 2 = %s inside the grace; want running", got.State)
	}
	env.SetNow(finished.Add(61 * time.Second))
	watch(t, env)
	wantMemberFailure(t, env, id, two, reasonFinishedNoSend, kube.StepDelivery)
	for _, n := range []int{1, 3} {
		if got := memberOf(t, env, id, n); got.State != store.MemberRunning {
			t.Errorf("reviewer %d = %s; want running", n, got.State)
		}
	}
	wantWaiting(t, env, id)
}

// rerunRun is the one re-run (a "rerun" or "rerun_failed" run) of the test's
// store.
func rerunRun(t *testing.T, env *testEnv) string {
	t.Helper()
	cfg, err := pgx.ParseConfig(env.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.schema
	conn, err := pgx.ConnectConfig(env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(env.ctx)
	var ids []string
	rows, err := conn.Query(env.ctx, `SELECT id::text FROM runs WHERE trigger IN ($1, $2)`, store.TriggerRerun, store.TriggerRerunFailed)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("re-runs = %v; want one\n%s", ids, env.Logs())
	}
	return ids[0]
}

// askRerun asks for a re-run of merge request 1's head, as via says: the
// command "review" (or "review_failed" with failedOnly) written by mo, a
// maintainer, and answered by the command step; or the dashboard's button,
// which queues through the same actions.Rerun. It returns the re-run.
func (e *e2e) askRerun(t *testing.T, via, head string, failedOnly bool) string {
	t.Helper()
	env := e.env
	switch via {
	case "command":
		off := false
		env.Engine.Config.Repositories[0].CommandsAuthorAndMaintainersOnly = &off
		env.GitLab.Reply(http.MethodGet, membersPath(userMaintainer), http.StatusOK, member(40))
		env.GitLab.Reply(http.MethodGet, userPath(userMaintainer), http.StatusOK, map[string]any{"id": userMaintainer, "bot": false})
		const comment = 3001
		kind := "review"
		if failedOnly {
			kind = "review_failed"
		}
		env.GitLab.Reply(http.MethodPost, emojiPath(1, comment), http.StatusCreated, `{}`)
		env.GitLab.Reply(http.MethodGet, notePath(1, comment), http.StatusOK, map[string]any{
			"id": comment, "body": commandText(kind, "", ""), "system": false,
			"author": map[string]any{"id": userMaintainer, "username": "mo"},
		})
		if _, err := env.Store.InsertCommand(env.ctx, store.Command{
			MR: env.MRKey(1), ProjectPath: testProjectPath, Platform: "gitlab", Kind: kind,
			CommentID: comment, Author: "mo", AuthorID: userMaintainer, CreatedAt: env.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := env.Engine.CommandsOnce(env.ctx); err != nil {
			t.Fatalf("CommandsOnce: %v", err)
		}
		var got []string
		for _, r := range env.GitLab.Requests(http.MethodPost, emojiPath(1, comment)) {
			got = append(got, r.JSON(t)["name"].(string))
		}
		if !slices.Equal(got, []string{eyes, up}) {
			t.Fatalf("reactions on the command = %v; want it carried out\n%s", got, env.Logs())
		}
	case "dashboard":
		repo, found, err := env.Engine.settings().Get(env.ctx, testInstance, testProjectPath)
		if err != nil || !found {
			t.Fatalf("settings = %v, %v", found, err)
		}
		res, err := actions.Rerun(env.ctx, env.Store, actions.RerunRequest{
			MR: env.MRKey(1), ProjectPath: testProjectPath, Head: head, TargetBranch: "main",
			FailedOnly: failedOnly, Repo: repo, Now: env.Now(), Models: env.Engine.registry(),
		})
		if err != nil || res.Status != actions.RerunQueued {
			t.Fatalf("Rerun = %+v, %v; want queued", res, err)
		}
	default:
		t.Fatalf("no way to ask for a re-run %q", via)
	}
	return rerunRun(t, env)
}

// judgeAndPublish dispatches the judge Job of released run id, posts body as
// its result and publishes the run.
func (e *e2e) judgeAndPublish(t *testing.T, id string, body []byte) {
	t.Helper()
	dispatch(t, e.env)
	judgeJob(t, e.env, id)
	e.postJudge(t, id, body)
	publish(t, e.env)
	wantPublished(t, e.env, id)
}

// jobsOf is the number of Jobs that belong to run id (its own, its
// reviewers' and its judge's).
func jobsOf(env *testEnv, id string) int {
	n := 0
	for _, j := range env.Jobs() {
		if j.Labels["pergent.sh/run"] == id {
			n++
		}
	}
	return n
}

// configText is every value of the ConfigMap of run or member id, joined.
func configText(env *testEnv, id string) string {
	var b strings.Builder
	for _, v := range env.ConfigMap(id).Data {
		b.WriteString(v)
	}
	return b.String()
}

// A re-run, asked for by a command or by the dashboard's button, is split
// again as a new multi review: new reviewers with their own Jobs, and its
// own judge. It reuses nothing of the earlier multi review: the earlier
// reviewers' results and judge's result are neither read nor touched. Its
// judge gets the merge request's dismissals as they are now (D11), and the
// re-run reviews the merge request's base. Its own judge's result is what is
// published.
func TestE2ERerunIsMultiReview(t *testing.T) {
	for _, via := range []string{"command", "dashboard"} {
		t.Run(via, func(t *testing.T) {
			e := newThreeReviewers(t)
			env := e.env
			head := sha('a')
			first, oldMs := releasedReview(t, e, head)
			e.judgeAndPublish(t, first, clean(t, head))
			oldMs = reviewMembers(t, env, first)
			oldJudge := judgeOf(t, env, first)
			if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{
				MR: env.MRKey(1), Fingerprint: "fp-later", Severity: "blocking", Reason: "not ours", Actor: "mo",
			}); err != nil {
				t.Fatal(err)
			}

			id := e.askRerun(t, via, head, false)
			if id == first {
				t.Fatalf("the re-run is the first run")
			}
			dispatch(t, env)
			r := env.Run(id)
			if r.State != store.Waiting || r.SkipReason != store.WaitReviewers || r.Trigger != store.TriggerRerun {
				t.Fatalf("re-run = %s/%q %q; want a multi review of a re-run\n%s", r.State, r.SkipReason, r.Trigger, env.Logs())
			}
			if r.BaseSHA != sha('b') || r.StartSHA == "" || r.StartSHA != env.Run(first).StartSHA || r.SkillSubset != nil || r.BaseRunID != nil || r.Incremental {
				t.Errorf("re-run = base %q start %q subset %v base run %v incremental %v; want a full review on the base",
					r.BaseSHA, r.StartSHA, r.SkillSubset, r.BaseRunID, r.Incremental)
			}
			if n := jobsOf(env, id); n != 0 {
				t.Fatalf("%d Jobs of the re-run after its split; want none", n)
			}
			ms := reviewMembers(t, env, id)
			if len(ms) != 3 {
				t.Fatalf("re-run members = %+v; want 3 new reviewers", ms)
			}
			for _, m := range ms {
				if m.State != store.MemberQueued || m.Result != nil || m.Entry.ID != "" {
					t.Errorf("re-run reviewer %d = %s result %s entry %q; want queued with nothing", m.Reviewer, m.State, m.Result, m.Entry.ID)
				}
				for _, o := range oldMs {
					if o.ID == m.ID {
						t.Errorf("re-run reviewer %d is the earlier review's member %s", m.Reviewer, o.ID)
					}
				}
			}
			if _, found, err := env.Store.JudgeOf(env.ctx, id); err != nil || found {
				t.Errorf("re-run judge row before its judge = %v, %v; want none", found, err)
			}

			// New results: the earlier ones reviewed a.go, these z.go.
			fresh := resultFor(t, head, reviewed("no_blocking_issues", "z.go"))
			dispatch(t, env)
			var newMs []store.ReviewMember
			for n, name := range []string{"team-a", "team-b", "team-c"} {
				m := memberOf(t, env, id, n+1)
				wantMemberKey(t, env, m, "key-"+name)
				if got := env.Job(m.ID).Labels["pergent.sh/run"]; got != id {
					t.Errorf("re-run reviewer %d's Job is of run %q; want %s", n+1, got, id)
				}
				newMs = append(newMs, m)
			}
			releaseReviewers(t, e, id, newMs, fresh, fresh, fresh)
			if r := env.Run(id); r.Result != nil {
				t.Errorf("released re-run has a result before its judge: %s", r.Result)
			}

			dispatch(t, env)
			args := judgeArgs(t, env, id)
			for _, want := range []string{"--base=" + sha('b'), "--mr-dismissals=" + kube.MRDismissalsFile,
				"--result-url=" + testCallbackURL + "/runs/" + id + "/result"} {
				if !hasArg(args, want) {
					t.Errorf("re-run judge args %v lack %q", args, want)
				}
			}
			if got, want := env.ConfigMap(id).Data["mr-dismissals.json"], `[{"fingerprint":"fp-later","severity":"blocking"}]`; got != want {
				t.Errorf("re-run judge mr-dismissals.json = %s; want %s", got, want)
			}
			if j := judgeOf(t, env, id); !slices.Equal(j.Reviewers, []int{1, 2, 3}) || j.NoJudge || j.Entry.Name != "team-j" {
				t.Errorf("re-run judge row = %+v; want reviewers 1 to 3 judged by team-j", j)
			}
			token := string(env.Secret(id).Data["result-token"])
			for n := 1; n <= 3; n++ {
				if code, body := e.getReviewer(t, id, n, token); code != http.StatusOK || !jsonEqual(t, body, fresh) {
					t.Errorf("re-run reviewer %d on its judge's route = %d %s; want the re-run's own result", n, code, body)
				}
			}

			e.postJudge(t, id, fresh)
			publish(t, env)
			wantPublished(t, env, id)
			got, err := result.Parse(env.Run(id).Result)
			if err != nil {
				t.Fatal(err)
			}
			if got.Merged || len(got.Skills) != 1 || !slices.Equal(got.Skills[0].Files.Reviewed, []string{"z.go"}) {
				t.Errorf("published re-run result = %+v; want its own judge's result alone", got)
			}

			// The earlier review is as it was.
			for i, m := range reviewMembers(t, env, first) {
				if o := oldMs[i]; m.ID != o.ID || m.State != o.State || !bytes.Equal(m.Result, o.Result) {
					t.Errorf("earlier reviewer %d changed: %s -> %s", m.Reviewer, o.State, m.State)
				}
			}
			if j := judgeOf(t, env, first); !reflect.DeepEqual(j, oldJudge) {
				t.Errorf("earlier judge row changed: %+v -> %+v", oldJudge, j)
			}
			if !jsonEqual(t, string(env.Run(first).Result), withMultiReview(t, clean(t, head), 3)) {
				t.Errorf("earlier result changed: %s", env.Run(first).Result)
			}
		})
	}
}

// twoSkillReview is newThreeReviewers with the skills security and style,
// and a published multi review of head on merge request 1 in which style
// failed and security found nothing. It returns the run.
func twoSkillReview(t *testing.T, e *e2e, head string) string {
	t.Helper()
	env := e.env
	env.AddSkill("style", "---\nname: style\n---\nCheck the style.\n")
	env.SetDefaultSkills("security", "style")
	body := resultFor(t, head, namedSkill(t, "security", "no_blocking_issues", "", ""), namedSkill(t, "style", "failed", "", ""))
	id, ms := startReviewers(t, e, head)
	releaseReviewers(t, e, id, ms, body, body, body)
	e.judgeAndPublish(t, id, body)
	if r := env.Run(id); r.SkillVersions == nil || len(r.SkillSources) != 2 || len(r.SkillHashes) != 2 {
		t.Fatalf("the multi review recorded skills %v %v %v; want both with their versions", r.SkillSources, r.SkillVersions, r.SkillHashes)
	}
	return id
}

// A re-run of the failed skills of a multi review is split again: its
// reviewers and its judge get the failed skill alone, the run keeps the
// subset and the base run it was queued with, and records the whole
// review's skills. The judge's result is merged with the base run's results
// of the other skills, as for a review on one model.
func TestE2ERerunFailedSkillsMultiReview(t *testing.T) {
	for _, via := range []string{"command", "dashboard"} {
		t.Run(via, func(t *testing.T) {
			e := newThreeReviewers(t)
			env := e.env
			head := sha('a')
			base := twoSkillReview(t, e, head)
			baseRun := env.Run(base)

			id := e.askRerun(t, via, head, true)
			dispatch(t, env)
			r := env.Run(id)
			if r.State != store.Waiting || r.SkipReason != store.WaitReviewers {
				t.Fatalf("re-run = %s/%q; want a multi review\n%s", r.State, r.SkipReason, env.Logs())
			}
			if r.Trigger != store.TriggerRerunFailed || !slices.Equal(r.SkillSubset, []string{"style"}) || r.BaseRunID == nil || *r.BaseRunID != base {
				t.Errorf("re-run = %q %v %v; want the re-run of style on %s", r.Trigger, r.SkillSubset, r.BaseRunID, base)
			}
			if r.SkillSources["security"] != baseRun.SkillSources["security"] || r.SkillVersions["security"] != baseRun.SkillVersions["security"] ||
				r.SkillHashes["security"] != baseRun.SkillHashes["security"] || r.SkillHashes["style"] == "" {
				t.Errorf("re-run skills = %v %v %v; want security kept from %s and style recorded", r.SkillSources, r.SkillVersions, r.SkillHashes, base)
			}
			if r.BaseSHA != sha('b') {
				t.Errorf("re-run base = %q; want the merge request's", r.BaseSHA)
			}

			dispatch(t, env)
			style := resultFor(t, head, namedSkill(t, "style", "no_blocking_issues", "suggestion", "cccccc000001"))
			var ms []store.ReviewMember
			for n := 1; n <= 3; n++ {
				m := memberOf(t, env, id, n)
				if env.Job(m.ID) == nil {
					t.Fatalf("re-run reviewer %d has no Job\n%s", n, env.Logs())
				}
				text := configText(env, m.ID)
				if !strings.Contains(text, "Check the style.") || strings.Contains(text, "Look for security problems.") {
					t.Errorf("re-run reviewer %d's inputs are not the style skill alone:\n%s", n, text)
				}
				ms = append(ms, m)
			}
			releaseReviewers(t, e, id, ms, style, style, style)

			dispatch(t, env)
			judgeJob(t, env, id)
			text := configText(env, id)
			if !strings.Contains(text, "Check the style.") || strings.Contains(text, "Look for security problems.") {
				t.Errorf("re-run judge's inputs are not the style skill alone:\n%s", text)
			}
			e.postJudge(t, id, style)
			publish(t, env)
			wantPublished(t, env, id)

			got, err := result.Parse(env.Run(id).Result)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Merged || len(got.Skills) != 2 || got.Skills[0].Name != "security" || got.Skills[0].Verdict != "no_blocking_issues" ||
				got.Skills[1].Name != "style" || got.Skills[1].Verdict != "no_blocking_issues" {
				t.Errorf("published result = %+v; want the kept security and the re-run style", got)
			}
			posts := env.GitLab.Requests(http.MethodPost, notesPath(1))
			if len(posts) == 0 {
				t.Fatalf("no comment was posted")
			}
			body, _ := posts[len(posts)-1].JSON(t)["body"].(string)
			for _, want := range []string{"| `security` |", "| `style` |", "finding of style"} {
				if !strings.Contains(body, want) {
					t.Errorf("comment lacks %q:\n%s", want, body)
				}
			}
		})
	}
}

// carriedResult is a completed result of head by pergent 0.5.0 (which takes
// --carried): the security skill reviewed reviewed and carried carried.
func carriedResult(t *testing.T, head string, reviewed, carried []string) []byte {
	t.Helper()
	b, err := json.Marshal(result.Result{
		SchemaVersion: result.SchemaVersion, Status: "completed", PergentVersion: "0.5.0",
		Base: sha('b'), Head: head, Skills: []result.Skill{{Name: "security", Verdict: "no_blocking_issues",
			Files: result.Files{Reviewed: reviewed, Carried: carried}}},
		Comment: result.Comment{Markdown: testMarkdown, FindingsBlock: "<!-- pergent:findings [] -->"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A push after a published multi review is an incremental multi review: the
// split records it as incremental on the earlier review, each reviewer gets
// --carried with the files that review covers, and the judge does not
// (pergent refuses it with --multi-review). The published result keeps the
// carried files, merged from the earlier review.
func TestE2EIncrementalMultiReview(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	headA, headB := sha('1'), sha('2')
	all := []string{"a.go", "b.go", "c.go"}
	first, ms := startReviewers(t, e, headA)
	full := carriedResult(t, headA, all, nil)
	releaseReviewers(t, e, first, ms, full, full, full)
	e.judgeAndPublish(t, first, full)

	servePublish(env, 1, headB)
	serveChangedSince(env, "b.go")
	id := env.Enqueue(1, headB)
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitReviewers || jobsOf(env, id) != 0 {
		t.Fatalf("run = %s/%q with %d Jobs; want a multi review\n%s", r.State, r.SkipReason, jobsOf(env, id), env.Logs())
	}
	if r := env.Run(id); !r.Incremental || r.BaseRunID == nil || *r.BaseRunID != first {
		t.Fatalf("run incremental = %v, base = %v; want an incremental multi review on %s\n%s", r.Incremental, r.BaseRunID, first, env.Logs())
	}

	dispatch(t, env)
	var newMs []store.ReviewMember
	for n := 1; n <= 3; n++ {
		m := memberOf(t, env, id, n)
		job := env.Job(m.ID)
		if job == nil {
			t.Fatalf("reviewer %d has no Job\n%s", n, env.Logs())
		}
		if !hasArg(job.Spec.Template.Spec.Containers[0].Args, "--carried=/in/carried.json") {
			t.Errorf("reviewer %d's args %v lack --carried", n, job.Spec.Template.Spec.Containers[0].Args)
		}
		if got, want := env.ConfigMap(m.ID).Data["carried.json"], `{"security":["a.go","c.go"]}`; got != want {
			t.Errorf("reviewer %d's carried.json = %s, want %s", n, got, want)
		}
		newMs = append(newMs, m)
	}
	part := carriedResult(t, headB, []string{"b.go"}, []string{"a.go", "c.go"})
	releaseReviewers(t, e, id, newMs, part, part, part)

	dispatch(t, env)
	for _, a := range judgeArgs(t, env, id) {
		if strings.HasPrefix(a, "--carried") {
			t.Errorf("the judge got %q", a)
		}
	}
	if _, ok := env.ConfigMap(id).Data["carried.json"]; ok {
		t.Errorf("the judge got carried.json")
	}
	if r := env.Run(id); !r.Incremental || r.BaseRunID == nil || *r.BaseRunID != first {
		t.Errorf("judged run incremental = %v, base = %v; want it kept", r.Incremental, r.BaseRunID)
	}
	// The CLI's judge keeps the first reviewer's files, the carried ones too.
	e.postJudge(t, id, part)
	publish(t, env)
	wantPublished(t, env, id)
	got, err := result.Parse(env.Run(id).Result)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Merged || len(got.Skills) != 1 || !slices.Equal(slices.Sorted(slices.Values(got.Skills[0].Files.Reviewed)), all) {
		t.Errorf("published result = %+v; want security with the carried files kept", got)
	}
}
