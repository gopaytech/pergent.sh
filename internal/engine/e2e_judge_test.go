package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
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

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// usageMessage is the CLI's message when no reviewer result can be used.
const usageMessage = "--multi-review needs at least one usable reviewer result"

// releaseReviewers posts bodies[i] as the result of reviewer i+1 of run id
// (ms are the members; a nil body posts nothing) and publishes, which
// settles them and releases the run for its judge.
func releaseReviewers(t *testing.T, e *e2e, id string, ms []store.ReviewMember, bodies ...[]byte) {
	t.Helper()
	for i, body := range bodies {
		if body == nil {
			continue
		}
		if code := e.postMember(t, ms[i].RunID, ms[i], body); code != http.StatusOK {
			t.Fatalf("reviewer %d's result: status %d\n%s", i+1, code, e.env.Logs())
		}
	}
	publish(t, e.env)
	if r := e.env.Run(id); r.State != store.Queued || r.SkipReason != "" {
		t.Fatalf("run = %s/%q %q; want released for its judge\n%s", r.State, r.SkipReason, r.FailureReason, e.env.Logs())
	}
}

// releasedReview starts the 3 reviewers of a review of head on merge request
// 1 (startReviewers), each with a clean result, and releases it.
func releasedReview(t *testing.T, e *e2e, head string) (string, []store.ReviewMember) {
	t.Helper()
	id, ms := startReviewers(t, e, head)
	releaseReviewers(t, e, id, ms, clean(t, head), clean(t, head), clean(t, head))
	return id, ms
}

// judgeJob is the judge Job of run id: it fails the test when there is none,
// or when it is not labelled as the judge's.
func judgeJob(t *testing.T, env *testEnv, id string) *batchv1.Job {
	t.Helper()
	job := env.Job(id)
	if job == nil {
		t.Fatalf("no judge Job for %s; run = %+v\n%s", id, env.Run(id), env.Logs())
	}
	if job.Labels[kube.LabelReviewer] != kube.ReviewerJudge || job.Labels["pergent.sh/run"] != id {
		t.Fatalf("judge Job labels = %v; want %s=%s of run %s", job.Labels, kube.LabelReviewer, kube.ReviewerJudge, id)
	}
	return job
}

// judgeJobs is the number of judge Jobs.
func judgeJobs(env *testEnv) int {
	n := 0
	for _, j := range env.Jobs() {
		if j.Labels[kube.LabelReviewer] == kube.ReviewerJudge {
			n++
		}
	}
	return n
}

// judgeArgs is the pergent container's args of run id's judge Job.
func judgeArgs(t *testing.T, env *testEnv, id string) []string {
	t.Helper()
	return judgeJob(t, env, id).Spec.Template.Spec.Containers[0].Args
}

// judgeOf is the review_judges row of run id.
func judgeOf(t *testing.T, env *testEnv, id string) store.JudgeRow {
	t.Helper()
	j, found, err := env.Store.JudgeOf(env.ctx, id)
	if err != nil || !found {
		t.Fatalf("JudgeOf(%s) = %v, %v; want a row\n%s", id, found, err, env.Logs())
	}
	return j
}

// wantNoJudge checks run id's judge Job runs --no-judge with no LLM key, for
// reason.
func wantNoJudge(t *testing.T, env *testEnv, id, reason string) {
	t.Helper()
	args := judgeArgs(t, env, id)
	if !hasArg(args, "--no-judge") || !hasArg(args, "--mr-dismissals="+kube.MRDismissalsFile) {
		t.Errorf("judge args %v; want --no-judge and --mr-dismissals", args)
	}
	if got := secretKeys(env, id); !slices.Equal(got, []string{"git-token", "result-token"}) {
		t.Errorf("judge Secret keys = %v; want no LLM key", got)
	}
	if hasEnv(env, id, "OPENCODE_API_KEY") {
		t.Errorf("the --no-judge Job names OPENCODE_API_KEY")
	}
	if j := judgeOf(t, env, id); !j.NoJudge || j.NoJudgeReason != reason || j.Entry.ID != "" {
		t.Errorf("judge row = %+v; want no judge for %q", j, reason)
	}
}

// nextJudge is the run of the one new Job: not run id's, a member's (their
// label is the first run's) or a bare Job a test put back (no label).
func (e *e2e) nextJudge(t *testing.T, known ...string) string {
	t.Helper()
	return e.newJob(t, append(known, "")...)
}

// postJudge posts body as run id's judge result with its Job's token.
func (e *e2e) postJudge(t *testing.T, id string, body []byte) {
	t.Helper()
	if code := e.postResult(t, id, string(e.env.Secret(id).Data["result-token"]), body); code != http.StatusOK {
		t.Fatalf("the judge's result: status %d\n%s", code, e.env.Logs())
	}
}

// getReviewer reads reviewer n's result on judge run id's reviewers route
// with token: what get-reviewers asks for.
func (e *e2e) getReviewer(t *testing.T, id string, n int, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/runs/%s/reviewers/%d", e.srv.URL, id, n), nil)
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

// wantReviewerFiles checks the reviewers route serves judge run id exactly
// the reviewers in have (get-reviewers writes their files) and 404 for the
// others of 1 to n.
func (e *e2e) wantReviewerFiles(t *testing.T, id string, n int, have ...int) {
	t.Helper()
	token := string(e.env.Secret(id).Data["result-token"])
	for k := 1; k <= n; k++ {
		want := http.StatusNotFound
		if slices.Contains(have, k) {
			want = http.StatusOK
		}
		if code, _ := e.getReviewer(t, id, k, token); code != want {
			t.Errorf("reviewer %d on the judge's route = %d; want %d", k, code, want)
		}
	}
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

// usageResult is the judge's result of head when no reviewer result can be
// used (D3: exit 2, code usage).
func usageResult(t *testing.T, head string) []byte {
	t.Helper()
	b, err := json.Marshal(result.Result{
		SchemaVersion: result.SchemaVersion, Status: "error", PergentVersion: "0.1.0",
		Base: sha('b'), Head: head, Error: &result.Error{Code: "usage", Message: usageMessage},
		Comment: result.Comment{Markdown: "The review failed: " + usageMessage},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
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

// jobAtDeadline puts run id's Job back as one that hit its deadline after
// its checkout finished, 2 minutes ago (past the watch grace).
func jobAtDeadline(t *testing.T, env *testEnv, id string) {
	t.Helper()
	if err := env.Store.AdvanceCheckoutSeen(env.ctx, id, 2); err != nil {
		t.Fatal(err)
	}
	if err := env.Kube.BatchV1().Jobs(testNamespace).Delete(env.ctx, kube.JobName(id), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	putBareJob(t, env, id, env.Now().Add(-time.Hour), jobCond(batchv1.JobFailed, "DeadlineExceeded", env.Now().Add(-2*time.Minute)))
}

// fetchFails puts run id's Job back as one whose reviewers container exited
// 1 (get-reviewers could not fetch a result, Ruling P38), 2 minutes ago.
func fetchFails(t *testing.T, env *testEnv, id string) {
	t.Helper()
	now := env.Now()
	replaceJob(t, env, id, now.Add(-time.Hour), []corev1.ContainerStatus{
		terminatedStatus("checkout", 0, "Completed", now.Add(-time.Hour), now.Add(-50*time.Minute)),
		terminatedStatus("reviewers", 1, "Error", now.Add(-50*time.Minute), now.Add(-3*time.Minute)),
	}, nil, jobCond(batchv1.JobFailed, "BackoffLimitExceeded", now.Add(-2*time.Minute)))
}

// wantFailedNeverPass checks run id was published as failed: the failure
// status last, never success, and no approval.
func wantFailedNeverPass(t *testing.T, env *testEnv, id, head string) {
	t.Helper()
	wantPublished(t, env, id)
	got := statusesOf(t, env, head)
	if len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "failed: ") {
		t.Errorf("statuses = %v; want the failure status last", got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "success") {
			t.Errorf("statuses = %v; a failed judge set success", got)
		}
	}
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 0 {
		t.Errorf("%d approvals; want none", n)
	}
}

// The reviewers are done: the run's next dispatch starts one judge Job on
// the run, with --multi-review, one --reviewer per reviewer, the
// --mr-dismissals file and its own 10 minute timeout. Its Secret holds the
// judge entry's key alone, its reviewers container fetches the results from
// the reviewers route with the judge's own token, and one review_judges row
// records what it judges with. No key is in a label, an annotation, the
// ConfigMap or the log, and nothing is posted. Its result is published.
func TestE2EJudgeJob(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := releasedReview(t, e, head)
	env.GitLab.Reset()

	dispatch(t, env)
	job := judgeJob(t, env, id)
	if n := judgeJobs(env); n != 1 {
		t.Errorf("%d judge Jobs; want one", n)
	}
	if v, ok := job.Labels[kube.LabelReviewMember]; ok {
		t.Errorf("the judge Job has a member label %q", v)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	for _, want := range []string{"--multi-review", "--reviewer=/panel/reviewer-1.json", "--reviewer=/panel/reviewer-2.json",
		"--reviewer=/panel/reviewer-3.json", "--mr-dismissals=/in/mr-dismissals.json", "--timeout=10m0s",
		"--base=" + sha('b'), "--result-url=" + testCallbackURL + "/runs/" + id + "/result"} {
		if !hasArg(args, want) {
			t.Errorf("judge args %v lack %q", args, want)
		}
	}
	for _, bad := range []string{"--no-judge", "--no-skill-verify", "--panel"} {
		if hasArg(args, bad) {
			t.Errorf("judge args %v hold %q", args, bad)
		}
	}
	if got := secretKeys(env, id); !slices.Equal(got, []string{"git-token", "llm-api-key", "result-token"}) {
		t.Errorf("judge Secret keys = %v; want one LLM key", got)
	}
	wantJobKey(t, env, id, "key-team-j")
	if v := jobEnv(env, id)["OPENCODE_MODEL"]; v != "gpt-team-j" {
		t.Errorf("judge OPENCODE_MODEL = %q, want gpt-team-j", v)
	}
	var fetch *corev1.Container
	for i, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name == "reviewers" {
			fetch = &job.Spec.Template.Spec.InitContainers[i]
		}
	}
	if fetch == nil || fetch.Image != testScanImage ||
		!hasArg(fetch.Args, "--base-url="+testCallbackURL+"/runs/"+id+"/reviewers") || !hasArg(fetch.Args, "--reviewers=1,2,3") {
		t.Errorf("reviewers container = %+v; want get-reviewers of 1 to 3 on pergent.sh's image", fetch)
	}
	wantDeadline := int64(kube.Deadline(kube.JudgeTimeout+kube.JudgeFetchTimeout, kube.CloneAllowance(0, 0)) / time.Second)
	if got := jobDeadline(t, env, id); got != wantDeadline {
		t.Errorf("judge deadline = %ds; want %ds (10 min, the fetch, a clone with no linked repository)", got, wantDeadline)
	}
	j := judgeOf(t, env, id)
	if !slices.Equal(j.Reviewers, []int{1, 2, 3}) || j.NoJudge || j.Entry.Name != "team-j" || j.Model != "gpt-team-j" ||
		j.Entry.Position != 1 || j.Entry.Length != 1 {
		t.Errorf("judge row = %+v; want reviewers 1 to 3 judged by team-j", j)
	}
	if r := env.Run(id); r.State != store.Running || r.JobName != kube.JobName(id) || r.BaseSHA != sha('b') {
		t.Errorf("run = %s job %q base %q; want running with the judge Job on the split's base", r.State, r.JobName, r.BaseSHA)
	}
	if _, ok := env.ConfigMap(id).Data["mr-dismissals.json"]; !ok {
		t.Errorf("the judge ConfigMap has no mr-dismissals.json")
	}
	meta := maps.Clone(job.Labels)
	maps.Copy(meta, job.Annotations)
	maps.Copy(meta, env.ConfigMap(id).Data)
	for k, v := range meta {
		for _, key := range []string{"key-team-a", "key-team-b", "key-team-c", "key-team-j"} {
			if strings.Contains(k, key) || strings.Contains(v, key) {
				t.Errorf("a key is in the judge's label, annotation or ConfigMap %s", k)
			}
		}
	}
	for _, key := range []string{"key-team-a", "key-team-b", "key-team-c", "key-team-j"} {
		if strings.Contains(env.Logs(), key) {
			t.Errorf("a key reached the log")
		}
	}
	wantNothingPosted(t, env)

	// The route serves each reviewer's result to the judge's token alone.
	token := string(env.Secret(id).Data["result-token"])
	for _, m := range ms {
		if code, body := e.getReviewer(t, id, m.Reviewer, token); code != http.StatusOK || !jsonEqual(t, body, clean(t, head)) {
			t.Errorf("reviewer %d on the route = %d %s; want its result", m.Reviewer, code, body)
		}
		if code, _ := e.getReviewer(t, id, m.Reviewer, string(env.Secret(m.ID).Data["result-token"])); code != http.StatusUnauthorized {
			t.Errorf("reviewer %d with a member's token = %d; want 401", m.Reviewer, code)
		}
	}
	e.postJudge(t, id, clean(t, head))
	publish(t, env)
	wantPublished(t, env, id)
}

// The judge's --mr-dismissals file is the merge request's stored
// dismissals: each one's fingerprint and severity, nothing else, in store
// order; [] for none. The --no-judge retry Job has it too.
func TestE2EJudgeJobMRDismissals(t *testing.T) {
	t.Run("two dismissals", func(t *testing.T) {
		e := newThreeReviewers(t)
		env := e.env
		head := sha('a')
		id, _ := releasedReview(t, e, head)
		at := env.Now()
		for i, d := range []store.Dismissal{
			{Fingerprint: "fp-one", Severity: "blocking", Reason: "not ours", Actor: "alice"},
			{Fingerprint: "fp-two", Severity: "suggestion", Reason: "fine", Actor: "bob"},
		} {
			d.MR, d.CreatedAt = env.MRKey(1), at.Add(time.Duration(i)*time.Second)
			if _, err := env.Store.Dismiss(env.ctx, d); err != nil {
				t.Fatal(err)
			}
		}
		want := `[{"fingerprint":"fp-one","severity":"blocking"},{"fingerprint":"fp-two","severity":"suggestion"}]`
		dispatch(t, env)
		if got := env.ConfigMap(id).Data["mr-dismissals.json"]; got != want {
			t.Errorf("mr-dismissals.json = %s; want %s", got, want)
		}
		jobAtDeadline(t, env, id)
		watch(t, env)
		publish(t, env)
		dispatch(t, env)
		next := e.nextJudge(t, id)
		wantNoJudge(t, env, next, noJudgeFailed)
		if got := env.ConfigMap(next).Data["mr-dismissals.json"]; got != want {
			t.Errorf("the --no-judge Job's mr-dismissals.json = %s; want %s", got, want)
		}
	})
	t.Run("none", func(t *testing.T) {
		e := newThreeReviewers(t)
		env := e.env
		id, _ := releasedReview(t, e, sha('a'))
		dispatch(t, env)
		if got := env.ConfigMap(id).Data["mr-dismissals.json"]; got != "[]" {
			t.Errorf("mr-dismissals.json = %q; want []", got)
		}
	})
}

// The file holds only dismissals with a fingerprint, and never an actor or a
// reason.
func TestMRDismissalsJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		ds   []store.Dismissal
		want string
	}{
		{"none", nil, `[]`},
		{"one", []store.Dismissal{{Fingerprint: "f", Severity: "blocking", Actor: "alice", Reason: "why"}},
			`[{"fingerprint":"f","severity":"blocking"}]`},
		{"no fingerprint", []store.Dismissal{{Severity: "blocking"}, {Fingerprint: "g", Severity: "suggestion"}},
			`[{"fingerprint":"g","severity":"suggestion"}]`},
	} {
		if got := string(mrDismissalsJSON(tc.ds)); got != tc.want {
			t.Errorf("%s: mrDismissalsJSON = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// What a claimed run's reviewers say about its dispatch, by the newest
// attempt of each reviewer.
func TestJudgeStage(t *testing.T) {
	m := func(n int, s store.MemberState) store.ReviewMember { return store.ReviewMember{Reviewer: n, State: s} }
	for _, tc := range []struct {
		name string
		ms   []store.ReviewMember
		want int
	}{
		{"no members", nil, reviewersNone},
		{"off row only", []store.ReviewMember{m(1, store.MemberOff)}, reviewersNone},
		{"one running", []store.ReviewMember{m(1, store.MemberDone), m(2, store.MemberRunning)}, reviewersLive},
		{"reported", []store.ReviewMember{m(1, store.MemberDone), m(2, store.MemberReported)}, reviewersLive},
		{"done and failed", []store.ReviewMember{m(1, store.MemberFailed), m(2, store.MemberDone), m(3, store.MemberSkipped)}, reviewersJudge},
		{"fell back, then done", []store.ReviewMember{m(1, store.MemberDone), m(2, store.MemberFellBack), m(2, store.MemberDone)}, reviewersJudge},
		{"fell back, next queued", []store.ReviewMember{m(1, store.MemberDone), m(2, store.MemberFellBack), m(2, store.MemberQueued)}, reviewersLive},
		{"all failed", []store.ReviewMember{m(1, store.MemberFailed), m(2, store.MemberSkipped)}, reviewersFailed},
		{"superseded", []store.ReviewMember{m(1, store.MemberDone), m(2, store.MemberSuperseded)}, reviewersNone},
	} {
		if got := judgeStage(tc.ms); got != tc.want {
			t.Errorf("%s: judgeStage = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The judge's result says no reviewer result can be used (D3): the run is
// retried once with --no-judge. The same result again is published as
// failed with the CLI's message, never success.
func TestE2EJudgeNoUsableReviewer(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, _ := releasedReview(t, e, head)
	dispatch(t, env)
	judgeJob(t, env, id)
	e.postJudge(t, id, usageResult(t, head))
	env.GitLab.Reset()
	publish(t, env)
	wantFellBack(t, env, id, store.OutcomeJudgeFailed)
	wantNothingPosted(t, env)

	dispatch(t, env)
	next := e.nextJudge(t, id)
	wantNoJudge(t, env, next, noJudgeFailed)
	e.postJudge(t, next, usageResult(t, head))
	publish(t, env)
	wantFailedNeverPass(t, env, next, head)
	if body := createdComment(t, env, 1); !strings.Contains(body, usageMessage) {
		t.Errorf("the comment lacks the CLI's message:\n%s", body)
	}
	dispatch(t, env)
	if n := judgeJobs(env); n != 2 {
		t.Errorf("%d judge Jobs; want 2, no third", n)
	}
}

// Reviewer 3 ran out of tokens on both entries and kept its partial result
// (M3.6): the judge still gets it (get-reviewers writes reviewer-3.json).
func TestE2EJudgeGetsPartialReviewer(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b"}, {"team-c", "team-c2"}}, []string{"team-j"})
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	if code := e.postMember(t, id, ms[2], failedOn(t, head, "quota")); code != http.StatusOK {
		t.Fatalf("reviewer 3's result: status %d", code)
	}
	publish(t, env)
	dispatch(t, env)
	partial := failedOn(t, head, "quota")
	if code := e.postMember(t, id, memberOf(t, env, id, 3), partial); code != http.StatusOK {
		t.Fatalf("reviewer 3's second result: status %d", code)
	}
	releaseReviewers(t, e, id, ms, clean(t, head), clean(t, head))
	wantFailedKeepingResult(t, env, id, 3, 2, "out of quota")

	dispatch(t, env)
	if args := judgeArgs(t, env, id); !hasArg(args, "--reviewer=/panel/reviewer-3.json") {
		t.Errorf("judge args %v lack reviewer 3", args)
	}
	e.wantReviewerFiles(t, id, 3, 1, 2, 3)
	token := string(env.Secret(id).Data["result-token"])
	if code, body := e.getReviewer(t, id, 3, token); code != http.StatusOK || !jsonEqual(t, body, partial) {
		t.Errorf("reviewer 3 on the route = %d; want its partial result", code)
	}
}

// Review Focus 5: the judge Job holds the judge entry's key alone, never a
// reviewer's: not in its Secret, its env, its args or its metadata. With the
// judge on reviewer 2's entry, the one key is that entry's.
func TestE2EJudgeJobHoldsOnlyJudgeKey(t *testing.T) {
	for _, tc := range []struct {
		name, judge, key string
		others           []string
	}{
		{"own entry", "team-j", "key-team-j", []string{"key-team-a", "key-team-b", "key-team-c"}},
		{"reviewer 2's entry", "team-b", "key-team-b", []string{"key-team-a", "key-team-c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b"}, {"team-c"}}, []string{tc.judge})
			env := e.env
			id, _ := releasedReview(t, e, sha('a'))
			dispatch(t, env)
			job := judgeJob(t, env, id)
			wantJobKey(t, env, id, tc.key)
			var seen []string
			for _, v := range env.Secret(id).Data {
				seen = append(seen, string(v))
			}
			for _, c := range append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers...) {
				seen = append(seen, c.Args...)
				for _, v := range c.Env {
					seen = append(seen, v.Value)
				}
			}
			for k, v := range job.Labels {
				seen = append(seen, k, v)
			}
			for k, v := range job.Annotations {
				seen = append(seen, k, v)
			}
			for _, v := range seen {
				for _, other := range tc.others {
					if strings.Contains(v, other) {
						t.Errorf("the judge Job holds %s", other)
					}
				}
			}
		})
	}
}

// Reviewer 2 failed with no result and reviewer 3 was skipped at the split:
// the judge still names reviewers 1 to 3 (D2), and only reviewer 1 has a
// file (the route gives 404 for 2 and 3).
func TestE2EJudgeFailedReviewerKeepsNumber(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, nil, nil)
	env := e.env
	b := entryNamed(t, env, "team-b")
	same := addTeamModel(t, env, ownerTeam(t, env), "team-same", "gpt-team-a", "key-same")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}, {same}}, []store.EntryRef{entryNamed(t, env, "team-j")})
	head := sha('a')
	id := splitReview(t, env, 1, head)
	wantSkipped(t, memberOf(t, env, id, 3), skipSame)
	dispatch(t, env)
	ms := []store.ReviewMember{memberOf(t, env, id, 1), memberOf(t, env, id, 2)}
	releaseReviewers(t, e, id, ms, clean(t, head), errorResult(t, head, "reviewer 2 broke"))

	dispatch(t, env)
	args := judgeArgs(t, env, id)
	for n := 1; n <= 3; n++ {
		if !hasArg(args, fmt.Sprintf("--reviewer=/panel/reviewer-%d.json", n)) {
			t.Errorf("judge args %v lack reviewer %d", args, n)
		}
	}
	if j := judgeOf(t, env, id); !slices.Equal(j.Reviewers, []int{1, 2, 3}) {
		t.Errorf("judge reviewers = %v; want 1 to 3", j.Reviewers)
	}
	e.wantReviewerFiles(t, id, 3, 1)
}

// Only reviewer 1 of 2 is done: the judge names both, gets one file, and
// still runs.
func TestE2EJudgeOneReviewer(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b"}}, []string{"team-j"})
	env := e.env
	head := sha('a')
	id := splitReview(t, env, 1, head)
	dispatch(t, env)
	ms := []store.ReviewMember{memberOf(t, env, id, 1), memberOf(t, env, id, 2)}
	releaseReviewers(t, e, id, ms, clean(t, head), errorResult(t, head, "reviewer 2 broke"))

	dispatch(t, env)
	args := judgeArgs(t, env, id)
	if !hasArg(args, "--reviewer=/panel/reviewer-1.json") || !hasArg(args, "--reviewer=/panel/reviewer-2.json") || hasArg(args, "--no-judge") {
		t.Errorf("judge args %v; want both reviewers and a judge", args)
	}
	wantJobKey(t, env, id, "key-team-j")
	e.wantReviewerFiles(t, id, 2, 1)
}

// The repository quota is used up when the judge starts: it runs --no-judge
// with no key, and the row says why.
func TestE2EJudgeQuotaNoJudge(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	// Before the release: the seed claims every queued run.
	q := int64(100)
	env.Engine.Config.Repositories[0].MonthlyTokenQuota = &q
	seedQuotaUsage(t, env, 100)
	releaseReviewers(t, e, id, ms, clean(t, head), clean(t, head), clean(t, head))

	dispatch(t, env)
	wantNoJudge(t, env, id, noJudgeRepo)
	for _, key := range []string{"key-team-j", "key-team-a"} {
		if strings.Contains(env.Logs(), key) {
			t.Errorf("a key reached the log")
		}
	}
}

// A judge copy with no entries (they were removed before the split) runs
// --no-judge.
func TestE2EJudgeEmptyChainNoJudge(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	execSQL(t, env, `DELETE FROM reviewer_chain_entries WHERE chain_id IN (SELECT id FROM reviewer_chains WHERE slot = 'judge')`)
	id, _ := releasedReview(t, e, sha('a'))
	dispatch(t, env)
	wantNoJudge(t, env, id, noJudgeChain)
}

// The provider refused the judge entry (provider_error.role judge, quota):
// the entry is marked and audited, the run closes as fell_back, and the
// next attempt judges on the next entry of the frozen judge chain. The
// members are untouched and nothing is posted for the dropped attempt.
func TestE2EJudgeFallsBack(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b"}, {"team-c"}}, []string{"team-j", "team-j2"})
	env := e.env
	j1, j2 := entryNamed(t, env, "team-j"), entryNamed(t, env, "team-j2")
	head := sha('a')
	id, _ := releasedReview(t, e, head)
	dispatch(t, env)
	wantJobKey(t, env, id, "key-team-j")
	if j := judgeOf(t, env, id); j.Entry.ID != j1.ID || j.Entry.Length != 2 {
		t.Fatalf("judge = %+v; want team-j of 2", j.Entry)
	}
	members := reviewMembers(t, env, id)
	root := env.Run(id)
	env.GitLab.Reset()

	e.postJudge(t, id, withRoleError(t, clean(t, head), "quota", roleJudge))
	publish(t, env)
	wantFellBack(t, env, id, store.ClassQuota)
	if m, ok := markOf(t, env, j1); !ok || m.Class != store.ClassQuota {
		t.Errorf("the judge's mark = %+v, %v; want quota", m, ok)
	}
	if _, ok := markOf(t, env, j2); ok {
		t.Errorf("the judge's next entry is marked")
	}
	if entries, err := env.Store.AuditLog(env.ctx, 0, 10); err != nil || len(entries) == 0 ||
		entries[0].Action != "model_marked" || entries[0].Detail != "model=team-j class=quota" {
		t.Errorf("audit = %+v, %v; want the judge entry marked", entries, err)
	}
	wantNothingPosted(t, env)

	dispatch(t, env)
	next := e.nextJudge(t, id)
	judgeJob(t, env, next)
	wantJobKey(t, env, next, "key-team-j2")
	if j := judgeOf(t, env, next); j.Entry.ID != j2.ID || j.Entry.Position != 2 || j.NoJudge {
		t.Errorf("next judge = %+v; want team-j2, the second", j)
	}
	if r := env.Run(next); r.AttemptOf == nil || *r.AttemptOf != id || r.BaseSHA != root.BaseSHA ||
		!maps.Equal(r.SkillSources, root.SkillSources) {
		t.Errorf("next attempt = %+v; want an attempt of the run with the split's base and skills", r)
	}
	if after := reviewMembers(t, env, id); !reflect.DeepEqual(members, after) {
		t.Errorf("the members changed:\n before %+v\n after  %+v", members, after)
	}
	e.wantReviewerFiles(t, next, 3, 1, 2, 3)
	e.postJudge(t, next, clean(t, head))
	publish(t, env)
	wantPublished(t, env, next)
}

// A 2-entry judge copy: both judge Jobs are refused for quota. The third
// judge Job runs --no-judge with no key, and its result is published.
func TestE2EJudgeOutOfTokensNoBackup(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b"}, {"team-c"}}, []string{"team-j", "team-j2"})
	env := e.env
	head := sha('a')
	id, _ := releasedReview(t, e, head)
	known := []string{id}
	dispatch(t, env)
	run := id
	for _, key := range []string{"key-team-j", "key-team-j2"} {
		wantJobKey(t, env, run, key)
		e.postJudge(t, run, withRoleError(t, clean(t, head), "quota", roleJudge))
		publish(t, env)
		wantFellBack(t, env, run, store.ClassQuota)
		dispatch(t, env)
		run = e.nextJudge(t, known...)
		known = append(known, run)
	}
	wantNoJudge(t, env, run, noJudgeEntry)
	e.postJudge(t, run, clean(t, head))
	publish(t, env)
	wantPublished(t, env, run)
}

// A 1-entry judge copy whose entry another chain marked: the judge still
// runs on it (a 1-entry chain ignores marks). Its refusal is published as
// it is: the CLI did the exact merge (D7).
func TestE2EJudgeOneEntryMarkedStillJudges(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	j := entryNamed(t, env, "team-j")
	head := sha('a')
	id, _ := releasedReview(t, e, head)
	until := env.Now().Add(time.Hour)
	if err := env.Store.SetMark(env.ctx, store.Mark{Ref: j, Class: store.ClassQuota, SetAt: env.Now(), Until: &until}); err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	wantJobKey(t, env, id, "key-team-j")
	e.postJudge(t, id, withRoleError(t, clean(t, head), "quota", roleJudge))
	publish(t, env)
	wantPublished(t, env, id)
	if as, err := env.Store.Attempts(env.ctx, id); err != nil || len(as) != 1 {
		t.Errorf("attempts = %d, %v; want no next attempt", len(as), err)
	}
}

// Review Focus 4: the judge Job passes its deadline. The run closes as
// fell_back with reason judge_failed, and one new judge Job runs --no-judge
// with no key. Nothing is posted yet.
func TestE2EJudgeCrashRunsNoJudgeOnce(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	id, _ := releasedReview(t, e, sha('a'))
	dispatch(t, env)
	judgeJob(t, env, id)
	jobAtDeadline(t, env, id)
	env.GitLab.Reset()
	watch(t, env)
	wantFailure(t, env, id, "the review didn't finish before the deadline (DeadlineExceeded)")
	publish(t, env)
	wantFellBack(t, env, id, store.OutcomeJudgeFailed)
	wantNothingPosted(t, env)

	dispatch(t, env)
	next := e.nextJudge(t, id)
	wantNoJudge(t, env, next, noJudgeFailed)
	dispatch(t, env)
	// The first Job was put back bare by the test: the labelled one is the
	// --no-judge Job, and there is no other attempt.
	if n := judgeJobs(env); n != 1 {
		t.Errorf("%d labelled judge Jobs; want the one --no-judge", n)
	}
	if as, err := env.Store.Attempts(env.ctx, id); err != nil || len(as) != 2 {
		t.Errorf("attempts = %d, %v; want the first and one retry", len(as), err)
	}
	wantNothingPosted(t, env)
}

// The --no-judge Job finishes without a result too: the review is published
// as failed, never success, with no approval and no third Job.
func TestE2EJudgeNoJudgeFailsReview(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, _ := releasedReview(t, e, head)
	dispatch(t, env)
	jobAtDeadline(t, env, id)
	watch(t, env)
	publish(t, env)
	dispatch(t, env)
	next := e.nextJudge(t, id)
	wantNoJudge(t, env, next, noJudgeFailed)

	replaceJob(t, env, next, env.Now().Add(-time.Hour), nil, nil, jobCond(batchv1.JobComplete, "", env.Now().Add(-2*time.Minute)))
	watch(t, env)
	wantFailure(t, env, next, reasonFinishedNoSend)
	publish(t, env)
	wantFailedNeverPass(t, env, next, head)
	dispatch(t, env)
	if as, err := env.Store.Attempts(env.ctx, id); err != nil || len(as) != 2 {
		t.Errorf("attempts = %d, %v; want 2, no third judge", len(as), err)
	}
}

// Ported from TestE2EPanelPTimeout (Ruling P43): a judge Job at its deadline
// fails the review once its --no-judge retry is at its deadline too. The
// review is published as failed: no success and no approval.
func TestE2EJudgeTimeout(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, _ := releasedReview(t, e, head)
	dispatch(t, env)
	run := id
	for i := 0; i < 2; i++ {
		jobAtDeadline(t, env, run)
		watch(t, env)
		wantFailure(t, env, run, "the review didn't finish before the deadline (DeadlineExceeded)")
		publish(t, env)
		if i == 0 {
			dispatch(t, env)
			run = e.nextJudge(t, id)
		}
	}
	wantFailedNeverPass(t, env, run, head)
}

// Ruling P38: get-reviewers exits non-zero on any failure but a 404, so the
// judge Job fails in its reviewers container. The run takes the judge
// retry; when the --no-judge Job's fetch fails too, the review fails. It is
// never a success.
func TestE2EJudgeFetchFails(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, _ := releasedReview(t, e, head)
	dispatch(t, env)
	fetchFails(t, env, id)
	watch(t, env)
	if r := env.Run(id); r.State != store.Reported || r.Result != nil || r.FailureReason == "" {
		t.Fatalf("run = %s %q (result %v); want reported as failed with no result\n%s", r.State, r.FailureReason, r.Result != nil, env.Logs())
	}
	publish(t, env)
	wantFellBack(t, env, id, store.OutcomeJudgeFailed)
	dispatch(t, env)
	next := e.nextJudge(t, id)
	wantNoJudge(t, env, next, noJudgeFailed)

	fetchFails(t, env, next)
	watch(t, env)
	publish(t, env)
	wantFailedNeverPass(t, env, next, head)
}

// Review Focus 2: after the split, reviewer 2's chain and the judge chain
// are replaced on the card. Reviewer 2's fallback and the judge use the
// copies the split froze.
func TestE2EMultiReviewFrozenAtSplit(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, [][]string{{"team-b", "team-b2"}, {"team-c"}}, []string{"team-j"})
	env := e.env
	head := sha('a')
	id := splitReview(t, env, 1, head)
	saveReviewers(t, env, [][]string{{"team-x"}, {"team-y"}}, []string{"team-z"})
	dispatch(t, env)
	var ms []store.ReviewMember
	for n, key := range []string{"key-team-a", "key-team-b", "key-team-c"} {
		m := memberOf(t, env, id, n+1)
		wantMemberKey(t, env, m, key)
		ms = append(ms, m)
	}
	if code := e.postMember(t, id, ms[1], failedOn(t, head, "quota")); code != http.StatusOK {
		t.Fatalf("reviewer 2's result: status %d", code)
	}
	publish(t, env)
	dispatch(t, env)
	two := memberOf(t, env, id, 2)
	wantMemberKey(t, env, two, "key-team-b2")
	releaseReviewers(t, e, id, []store.ReviewMember{ms[0], two, ms[2]}, clean(t, head), clean(t, head), clean(t, head))

	dispatch(t, env)
	wantJobKey(t, env, id, "key-team-j")
	if j := judgeOf(t, env, id); j.Entry.Name != "team-j" {
		t.Errorf("judge = %+v; want the frozen team-j", j.Entry)
	}
}

// The checks the run passed before its split are not done again for the
// judge: an author no longer trusted and a CI job still running hold
// neither the reviewers nor the judge.
func TestE2EJudgeSkipsChecks(t *testing.T) {
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
	var ms []store.ReviewMember
	for n := 1; n <= 3; n++ {
		ms = append(ms, memberOf(t, env, id, n))
	}
	releaseReviewers(t, e, id, ms, clean(t, head), clean(t, head), clean(t, head))

	dispatch(t, env)
	if r := env.Run(id); r.State != store.Running {
		t.Fatalf("run = %s/%q; want the judge Job started\n%s", r.State, r.SkipReason, env.Logs())
	}
	judgeJob(t, env, id)
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(3))); n != 0 {
		t.Errorf("%d notes; want none", n)
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "running: "+runningDescription {
		t.Errorf("statuses = %v; want the reviewers' running status alone", got)
	}
}

// During a rolling deploy an older pod's StartRun queues a multi review
// whose reviewers still run. The dispatch starts no judge Job: the run
// waits for its reviewers again, and the sweep leaves them alone. Once they
// are done the release queues the run, and the judge Job starts once.
func TestE2EJudgeWaitsForLiveMembers(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	before := env.Run(id)

	startRunOld(t, env, id)
	dispatch(t, env)
	if n := judgeJobs(env); n != 0 {
		t.Fatalf("%d judge Jobs while the reviewers run; want none\n%s", n, env.Logs())
	}
	wantWaiting(t, env, id)
	if r := env.Run(id); r.JobName != "" || !r.DispatchedAt.Equal(before.DispatchedAt) {
		t.Errorf("run = job %q dispatched %v; want no Job, dispatched at the split (%v)", r.JobName, r.DispatchedAt, before.DispatchedAt)
	}
	cleanup(t, env)
	for _, m := range ms {
		if got := memberOf(t, env, id, m.Reviewer); got.ID != m.ID || got.State != store.MemberRunning {
			t.Errorf("reviewer %d = %s %s; want %s running", m.Reviewer, got.ID, got.State, m.ID)
		}
	}

	releaseReviewers(t, e, id, ms, clean(t, head), clean(t, head), clean(t, head))
	dispatch(t, env)
	dispatch(t, env)
	if n := judgeJobs(env); n != 1 {
		t.Fatalf("%d judge Jobs; want one\n%s", n, env.Logs())
	}
	judgeOf(t, env, id)
}

// Review Focus 1: an older pod's StartRun queues a multi review whose
// reviewers run. A new pod that claims it puts it back to wait
// (WaitForReviewMembers), with no Job. An older pod dispatches it as one
// review (here by SQL: running with a Job): the sweep supersedes the live
// reviewers and deletes their Jobs, and no judge Job ever starts.
func TestE2EMultiReviewOldStartIsSingleReview(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)

	startRunOld(t, env, id)
	dispatch(t, env)
	wantWaiting(t, env, id)
	if env.Job(id) != nil || judgeJobs(env) != 0 {
		t.Fatalf("a Job of the run started while its reviewers run\n%s", env.Logs())
	}

	// The older pod's single review.
	startRunOld(t, env, id)
	execSQL(t, env, fmt.Sprintf(`UPDATE runs SET state = 'running', job_name = '%s' WHERE id = '%s'`, kube.JobName(id), id))
	putBareJob(t, env, id, env.Now())
	cleanup(t, env)
	for _, m := range ms {
		got := memberOf(t, env, id, m.Reviewer)
		if got.State != store.MemberSuperseded {
			t.Errorf("reviewer %d = %s; want superseded", m.Reviewer, got.State)
		}
		if env.Job(m.ID) != nil {
			t.Errorf("reviewer %d's Job is left", m.Reviewer)
		}
	}
	dispatch(t, env)
	if n := judgeJobs(env); n != 0 {
		t.Errorf("%d judge Jobs; want none", n)
	}
	if _, found, err := env.Store.JudgeOf(env.ctx, id); err != nil || found {
		t.Errorf("JudgeOf = %v, %v; want no judge row", found, err)
	}
}

// Ported from the judge half of TestE2EPanelSplitAfterFallBack (Ruling P43):
// a review falls back on chain A before it has extra reviewers, and its next
// attempt is split on a base the merge request moved to. The judge runs on
// that split run and reviews the split's base, and the judge's own next
// attempt keeps the split's base and start SHA, never the first run's.
func TestE2EJudgeSplitAfterFallBack(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	e, _ := newFallbackE2E(t, "team-a", "team-a2")
	env := e.env
	env.Engine.ScanImage = testScanImage
	head, base, start := sha('a'), sha('9'), sha('8')
	servePublish(env, 1, head)
	env.Enqueue(1, head)
	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	wantFellBack(t, env, first, store.ClassQuota)

	saveReviewers(t, env, [][]string{{"team-b"}, {"team-c"}}, []string{"team-j", "team-j2"})
	mr := openMR(1, head)
	mr.DiffRefs.BaseSHA, mr.DiffRefs.StartSHA = base, start
	env.GitLab.SetMR(mr)
	dispatch(t, env)
	as, err := env.Store.Attempts(env.ctx, first)
	if err != nil || len(as) != 2 {
		t.Fatalf("attempts = %d, %v; want the first run and its split attempt", len(as), err)
	}
	split := as[1].ID
	dispatch(t, env)
	var ms []store.ReviewMember
	for n := 1; n <= 3; n++ {
		ms = append(ms, memberOf(t, env, first, n))
	}
	body := withBase(t, clean(t, head), base)
	releaseReviewers(t, e, split, ms, body, body, body)

	dispatch(t, env)
	if args := judgeArgs(t, env, split); !hasArg(args, "--base="+base) {
		t.Fatalf("the judge does not review the split's base: %v", args)
	}
	e.postJudge(t, split, withRoleError(t, body, "quota", roleJudge))
	publish(t, env)
	wantFellBack(t, env, split, store.ClassQuota)

	dispatch(t, env)
	next := e.nextJudge(t, first, split)
	judgeJob(t, env, next)
	r, want := env.Run(next), env.Run(split)
	if r.BaseSHA != base || r.StartSHA != start || !maps.Equal(r.SkillSources, want.SkillSources) ||
		r.ModelEntryID != want.ModelEntryID {
		t.Errorf("the judge's next attempt = base %q start %q entry %q; want the split run's", r.BaseSHA, r.StartSHA, r.ModelEntryID)
	}
	if !hasArg(judgeArgs(t, env, next), "--base="+base) {
		t.Errorf("the judge's next Job does not review the split's base")
	}
	wantJobKey(t, env, next, "key-team-j2")
	e.postJudge(t, next, body)
	publish(t, env)
	wantPublished(t, env, next)
}

// The judge does not check limits (Ruling L7): with every judge entry at its
// model and team limit, the judge Job holds the first entry's key, with no
// --no-judge, and the run never waits for a limit.
func TestE2EJudgeIgnoresLimits(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, nil, nil)
	env := e.env
	env.SetNow(novemberMid)
	team := ownerTeam(t, env)
	gj := globalRef(t, env, "gpt-gj", "key-gj")
	tj := entryNamed(t, env, "team-j")
	saveExtraReviewers(t, env, [][]store.EntryRef{{entryNamed(t, env, "team-b")}, {entryNamed(t, env, "team-c")}},
		[]store.EntryRef{gj, tj})
	seedEntryUsage(t, env, gj, 10)
	seedEntryUsage(t, env, tj, 10)
	id, _ := releasedReview(t, e, sha('a'))
	setLimit(t, env, gj, 0, 10)
	setLimit(t, env, gj, team, 10)
	setLimit(t, env, tj, team, 10)

	dispatch(t, env)
	if r := env.Run(id); r.State != store.Running || r.SkipReason == store.WaitLimit {
		t.Fatalf("run = %s/%q; want the judge Job started\n%s", r.State, r.SkipReason, env.Logs())
	}
	wantJobKey(t, env, id, "key-gj")
	if hasArg(judgeArgs(t, env, id), "--no-judge") {
		t.Errorf("the judge runs --no-judge at a limit")
	}
	if j := judgeOf(t, env, id); j.NoJudge || j.Entry.ID != gj.ID {
		t.Errorf("judge = %+v; want the first entry", j)
	}
}

// legacyPanelRun queues a review of head on merge request 1, makes it a
// legacy panel's (StartPanel, as a main build split it), sets the members
// by setup, and runs RecoverLegacyPanels, as at start. It returns the run.
func legacyPanelRun(t *testing.T, env *testEnv, head string, setup func(ms []store.PanelMember)) string {
	t.Helper()
	servePublish(env, 1, head)
	id := env.Enqueue(1, head)
	now := env.Now()
	if _, err := env.Store.ClaimQueued(env.ctx, 10, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if started, err := env.Store.StartPanel(env.ctx, id, store.PanelSplit{BaseSHA: "base", StartSHA: "start"}, now); err != nil || !started {
		t.Fatalf("StartPanel = %v, %v", started, err)
	}
	ms, err := env.Store.PanelMembers(env.ctx, id)
	if err != nil || len(ms) != 2 {
		t.Fatalf("PanelMembers = %d, %v", len(ms), err)
	}
	setup(ms)
	env.Engine.RecoverLegacyPanels(env.ctx)
	if r := env.Run(id); r.State != store.Queued {
		t.Fatalf("legacy run = %s; want queued again\n%s", r.State, env.Logs())
	}
	return id
}

// Ruling P40: a legacy panel run that RecoverLegacyPanels queued again runs
// as one plain review, whether both members were superseded or one was
// done: one review Job on chain A, no Job P, no judge Job, no split, and its
// comment has no panel or multi review text. Only that full review's result
// is published.
func TestE2ELegacyPanelRunIsSingleReview(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, env *testEnv, ms []store.PanelMember)
	}{
		{"both superseded", func(*testing.T, *testEnv, []store.PanelMember) {}},
		{"one done, one superseded", func(t *testing.T, env *testEnv, ms []store.PanelMember) {
			execSQL(t, env, fmt.Sprintf(`UPDATE panel_members SET state = 'done', result = '%s'::jsonb,
				model_source = 'global', model_entry_id = 'config', model_entry_name = 'legacy-a', provider = 'openai', model = 'gpt-legacy'
				WHERE id = '%s'`, clean(t, sha('a')), ms[0].ID))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			multiReviewFloor(t, "v0.5.0")
			e := newE2E(t)
			env := e.env
			env.Engine.ScanImage = testScanImage
			head := sha('a')
			id := legacyPanelRun(t, env, head, func(ms []store.PanelMember) { tc.setup(t, env, ms) })

			dispatch(t, env)
			wantJobKey(t, env, id, testLLMKey)
			if n := len(env.Jobs()); n != 1 {
				t.Errorf("%d Jobs; want the one review Job", n)
			}
			job := env.Job(id)
			args := job.Spec.Template.Spec.Containers[0].Args
			for _, bad := range []string{"--panel", "--multi-review", "--no-judge", "--no-skill-verify"} {
				if hasArg(args, bad) {
					t.Errorf("the review's args %v hold %q", args, bad)
				}
			}
			for _, k := range []string{kube.LabelPanelRole, kube.LabelReviewer, kube.LabelReviewMember} {
				if v, ok := job.Labels[k]; ok {
					t.Errorf("the review Job has the label %s=%q", k, v)
				}
			}
			if ms := reviewMembers(t, env, id); len(ms) != 0 {
				t.Errorf("review members = %+v; want none (not split)", ms)
			}
			if _, found, err := env.Store.JudgeOf(env.ctx, id); err != nil || found {
				t.Errorf("JudgeOf = %v, %v; want none", found, err)
			}
			if r := env.Run(id); r.State != store.Running || r.BaseSHA != sha('b') {
				t.Errorf("run = %s base %q; want running a full review of the merge request's base", r.State, r.BaseSHA)
			}

			e.postResult(t, id, string(env.Secret(id).Data["result-token"]), clean(t, head))
			publish(t, env)
			wantPublished(t, env, id)
			body := createdComment(t, env, 1)
			for _, bad := range []string{"Panel", "panel", "multi review", "Multi review", "reviewer 2", "judge"} {
				if strings.Contains(body, bad) {
					t.Errorf("the comment holds %q:\n%s", bad, body)
				}
			}
		})
	}
}
