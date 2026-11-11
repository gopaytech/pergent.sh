package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// apiE2E is an e2e with the review API served beside it, and one client,
// "gateway", reviewing with the security skill, with a key.
type apiE2E struct {
	*e2e
	api    *httptest.Server
	key    string
	client store.APIClient
}

func newAPIE2E(t *testing.T) *apiE2E {
	t.Helper()
	e := newE2E(t)
	env := e.env
	c, err := env.Store.CreateAPIClient(env.ctx, store.APIClient{Name: "gateway", Skills: []string{"security"},
		CallbackSecretSealed: env.Box.Seal([]byte("pgs_callback-secret-example"))})
	if err != nil {
		t.Fatalf("CreateAPIClient: %v", err)
	}
	key, prefix, hash, err := reviewapi.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store.CreateAPIKey(env.ctx, store.APIKey{ClientID: c.ID, Name: "ci", Prefix: prefix, Hash: hash}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	h := &reviewapi.Handler{Store: env.Store, Box: env.Box, Models: env.Engine.Instances, Now: env.Now,
		Wake: env.Engine.Wake, Log: env.Engine.Log}
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &apiE2E{e2e: e, api: srv, key: key, client: c}
}

// twoFiles is a change of one file and a new one.
func twoFiles() []reviewapi.File {
	old, changed, added := "limit: 10\n", "limit: 5\n", "limit: 1\n"
	return []reviewapi.File{
		{Path: "routes/api.yaml", Old: &old, New: &changed},
		{Path: "routes/new.yaml", New: &added},
	}
}

// ask asks for a review of files on subject and returns its ID.
func (a *apiE2E) ask(t *testing.T, subject string, files []reviewapi.File) string {
	t.Helper()
	body, err := json.Marshal(reviewapi.Request{Subject: subject, Tenant: "acme", Title: "Rate limit the route",
		Description: "Lower the limit.", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, a.api.URL+"/v1/reviews", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.key)
	resp, err := a.api.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusAccepted || got.ID == "" {
		t.Fatalf("POST /v1/reviews = %d, id %q, %v", resp.StatusCode, got.ID, err)
	}
	return got.ID
}

// review reads review id through GET /v1/reviews/{id}.
func (a *apiE2E) review(t *testing.T, id string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, a.api.URL+"/v1/reviews/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+a.key)
	resp, err := a.api.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/reviews/%s = %d, %v", id, resp.StatusCode, err)
	}
	return got
}

// stored is the response stored for review id, decoded; it fails the test
// when there is none.
func (a *apiE2E) stored(t *testing.T, id string) reviewapi.Review {
	t.Helper()
	rv, found, err := a.env.Store.APIReviewByID(a.env.ctx, id)
	if err != nil || !found {
		t.Fatalf("APIReviewByID(%s) = %v, %v", id, found, err)
	}
	if rv.Response == nil {
		t.Fatalf("review %s has no stored response (state %s)", id, rv.Run.State)
	}
	var body reviewapi.Review
	if err := json.Unmarshal(rv.Response, &body); err != nil {
		t.Fatalf("stored response %q: %v", rv.Response, err)
	}
	return body
}

// wantFailed checks review id is published as failed at step, for reason,
// in its stored response and its run.
func (a *apiE2E) wantFailed(t *testing.T, id, step, reason string) {
	t.Helper()
	wantPublished(t, a.env, id)
	body := a.stored(t, id)
	if body.Status != reviewapi.StatusFailed || body.Verdict != reviewapi.VerdictFailed || body.FinishedAt == nil {
		t.Errorf("stored status, verdict, finished = %q, %q, %v; want failed, failed and a time", body.Status, body.Verdict, body.FinishedAt)
	}
	if body.Failure == nil || body.Failure.Step != step || body.Failure.Reason != reason {
		t.Errorf("stored failure = %+v; want step %q, reason %q", body.Failure, step, reason)
	}
	if len(body.Skills) != 0 {
		t.Errorf("a failed review has skills: %+v", body.Skills)
	}
	if r := a.env.Run(id); r.Outcome != store.OutcomeFailed {
		t.Errorf("outcome = %q, want failed", r.Outcome)
	}
}

// noGitLab checks the fake GitLab heard nothing: an API review has no forge.
func noGitLab(t *testing.T, env *testEnv) {
	t.Helper()
	if reqs := env.GitLab.Requests("", ""); len(reqs) != 0 {
		t.Errorf("an API review made %d GitLab requests, the first %s %s", len(reqs), reqs[0].Method, reqs[0].Path)
	}
}

// apiResultFor is a completed result of base and head with skills, each of
// which used 1200 tokens in and 300 out.
func apiResultFor(t *testing.T, base, head string, skills ...result.Skill) []byte {
	t.Helper()
	b, err := json.Marshal(result.Result{
		SchemaVersion: result.SchemaVersion, Status: "completed", PergentVersion: "0.1.0",
		Base: base, Head: head, Skills: skills,
		Comment: result.Comment{Markdown: testMarkdown, FindingsBlock: "<!-- pergent:findings [] -->"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// result.Skill has no tokens: pergent's own field, read by usage.
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, s := range m["skills"].([]any) {
		s.(map[string]any)["tokens"] = map[string]any{"input": 1200, "output": 300}
	}
	if b, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	return b
}

// blockingSkill is the security skill, which reviewed both files and found
// one blocking problem.
func blockingSkill(t *testing.T) result.Skill {
	t.Helper()
	s := reviewed("has_blocking_issues", "routes/api.yaml", "routes/new.yaml")
	f, err := json.Marshal(result.Finding{
		ID: "abc123", Fingerprint: sqlFingerprint, File: "routes/api.yaml", Line: 1, Severity: "blocking",
		Title: "The limit is too low", Explanation: "Callers will be refused.",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Findings = []json.RawMessage{f}
	return s
}

func secretExists(t *testing.T, env *testEnv, name string) bool {
	t.Helper()
	_, err := env.Kube.CoreV1().Secrets(testNamespace).Get(env.ctx, name, metav1.GetOptions{})
	return err == nil
}

// An API review goes through the queue, the Job, the result handler and the
// publisher, with no forge call, and reads back completed.
func TestAPIReviewEndToEnd(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	im, err := reviewapi.BuildImport(twoFiles())
	if err != nil {
		t.Fatal(err)
	}
	id := a.ask(t, "route:1", twoFiles())

	dispatch(t, env)

	job := env.Job(id)
	if job == nil {
		t.Fatalf("no Job for the review; jobs: %v", env.Jobs())
	}
	if got := job.Labels[kube.LabelAPIClient]; got != "gateway" {
		t.Errorf("Job label %s = %q, want gateway", kube.LabelAPIClient, got)
	}
	files, err := env.Kube.CoreV1().Secrets(testNamespace).Get(env.ctx, kube.JobName(id)+"-files", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the Job has no files Secret: %v", err)
	}
	if !bytes.Equal(files.Data["import"], im.Stream) {
		t.Errorf("the files Secret holds %q, want the request's stream", files.Data["import"])
	}
	sec := env.Secret(id)
	if len(sec.Data["git-token"]) != 0 || string(sec.Data["llm-api-key"]) != testLLMKey {
		t.Errorf("Job Secret = git-token %q, llm-api-key %q; want no clone token and the LLM key", sec.Data["git-token"], sec.Data["llm-api-key"])
	}
	if got := env.ConfigMap(id).Data["change-description.md"]; got != "Rate limit the route\n\nLower the limit." {
		t.Errorf("change description = %q", got)
	}
	run := env.Run(id)
	if run.State != store.Running || run.JobName != kube.JobName(id) || run.DispatchedAt.IsZero() {
		t.Errorf("run = %s, job %q, dispatched %v; want running with its Job", run.State, run.JobName, run.DispatchedAt)
	}
	if run.BaseSHA != im.Base || run.HeadSHA != im.Head || run.StartSHA != "" {
		t.Errorf("run base, head, start = %s, %s, %q; want the request's commits", run.BaseSHA, run.HeadSHA, run.StartSHA)
	}
	if run.Model == nil || *run.Model != "claude-sonnet-4" || run.SkillSources["security"] != "library" {
		t.Errorf("run model %v, sources %v; want the default model and the library skill", run.Model, run.SkillSources)
	}
	if _, found, err := env.Store.APIReviewInputs(env.ctx, id); err != nil || found {
		t.Errorf("sealed inputs kept after the Job started (found %v, %v)", found, err)
	}

	token := string(sec.Data["result-token"])
	if code := a.postResult(t, id, token, apiResultFor(t, im.Base, im.Head, blockingSkill(t))); code != http.StatusOK {
		t.Fatalf("result: status %d", code)
	}
	publish(t, env)

	wantPublished(t, env, id)
	if r := env.Run(id); r.Outcome != store.OutcomeBlocking {
		t.Errorf("outcome = %q, want blocking", r.Outcome)
	}
	got := a.review(t, id)
	if got["status"] != reviewapi.StatusCompleted || got["verdict"] != reviewapi.VerdictHasBlockingIssues || got["failure"] != nil {
		t.Errorf("review = %v; want completed, has_blocking_issues, no failure", got)
	}
	if got["finished_at"] == nil || got["subject"] != "route:1" || got["tenant"] != "acme" {
		t.Errorf("review = %v; want finished, of route:1 for acme", got)
	}
	u, _ := got["usage"].(map[string]any)
	if u["tokens_in"] != float64(1200) || u["tokens_out"] != float64(300) {
		t.Errorf("usage = %v; want the result's 1200 in and 300 out", got["usage"])
	}
	skills, _ := got["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("skills = %v, want the one skill", got["skills"])
	}
	if fs, _ := skills[0].(map[string]any)["findings"].([]any); len(fs) != 1 {
		t.Errorf("findings = %v, want the blocking one", fs)
	}
	noGitLab(t, env)
}

// API and merge request reviews share jobs.max_running, first in first out.
func TestAPIReviewSharesMaxRunning(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	env.Engine.Config.Jobs.MaxRunning = 1
	head := sha('a')
	mr := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	api := a.ask(t, "route:1", twoFiles())

	dispatch(t, env)

	if env.Job(mr) == nil {
		t.Errorf("the merge request's run, queued first, has no Job")
	}
	if env.Job(api) != nil {
		t.Errorf("the API review got a Job past jobs.max_running")
	}
	if r := env.Run(api); r.State != store.Queued {
		t.Errorf("API review = %s, want queued", r.State)
	}
}

// A client whose skills were emptied after the request fails at the start,
// with the text the 409 has, and no Job.
func TestAPIReviewNoSkillsFails(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	id := a.ask(t, "route:1", twoFiles())
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) { c.Skills = nil }); err != nil {
		t.Fatal(err)
	}

	dispatch(t, env)
	if env.Job(id) != nil {
		t.Fatalf("a client with no skills got a Job")
	}
	publish(t, env)

	a.wantFailed(t, id, reviewapi.StepStarting, reasonClientNoSkills)
	if got := a.review(t, id); got["status"] != reviewapi.StatusFailed {
		t.Errorf("GET status = %v, want failed", got["status"])
	}
	noGitLab(t, env)
}

// A client whose model is gone fails, and never falls back to the default.
func TestAPIReviewModelGoneFails(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	id := a.ask(t, "route:1", twoFiles())
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) { c.ModelID = "gone-model" }); err != nil {
		t.Fatal(err)
	}

	dispatch(t, env)
	if env.Job(id) != nil {
		t.Fatalf("a client whose model is gone got a Job")
	}
	publish(t, env)

	a.wantFailed(t, id, reviewapi.StepStarting, reasonClientModel)
}

// A result for another head is refused by the result handler, and the review
// is failed at delivery.
func TestAPIReviewWrongCommitFails(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	im, err := reviewapi.BuildImport(twoFiles())
	if err != nil {
		t.Fatal(err)
	}
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	token := string(env.Secret(id).Data["result-token"])

	if code := a.postResult(t, id, token, apiResultFor(t, im.Base, sha('e'), blockingSkill(t))); code != http.StatusBadRequest {
		t.Errorf("result of another head: status %d, want 400", code)
	}
	publish(t, env)

	wantPublished(t, env, id)
	body := a.stored(t, id)
	if body.Status != reviewapi.StatusFailed || body.Verdict != reviewapi.VerdictFailed || len(body.Skills) != 0 {
		t.Errorf("stored = %+v; want failed with no skills", body)
	}
	if body.Failure == nil || body.Failure.Step != reviewapi.StepDelivery ||
		!strings.HasPrefix(body.Failure.Reason, "pergent sent a result pergent.sh couldn't use") {
		t.Errorf("failure = %+v; want the unusable result at delivery", body.Failure)
	}
}

// A stored result the publisher can't read is published as failed at
// delivery, with a fixed reason, and never as a pass.
func TestAPIReviewUnusableResultFails(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	token := string(env.Secret(id).Data["result-token"])
	// Past the result handler, which would have refused it.
	if out, err := env.Store.StoreResult(env.ctx, id, token, []byte(`{"schema_version":99,"status":"completed"}`), env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}

	publish(t, env)

	a.wantFailed(t, id, reviewapi.StepDelivery, reasonAPIUnusable)
	if r := env.Run(id); r.Result != nil || r.FailureReason != reasonAPIUnusable {
		t.Errorf("run result %q, failure %q; want the result replaced by the failure", r.Result, r.FailureReason)
	}
	rv, _, err := env.Store.APIReviewByID(env.ctx, id)
	if err != nil || reviewapi.StatusOf(rv) != reviewapi.StatusFailed {
		t.Errorf("StatusOf the stored run = %q (%v), want failed", reviewapi.StatusOf(rv), err)
	}
}

// The files' Secret is deleted once the checkout is done, not before, and the
// watcher writes no repository's rows for an API review.
func TestWatchDeletesFilesAfterCheckout(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	name, files := kube.JobName(id), kube.JobName(id)+"-files"
	now := env.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-abcde", Namespace: testNamespace,
			Labels: map[string]string{"job-name": name}, CreationTimestamp: metav1.NewTime(now.Add(-time.Minute))},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{runningStatus("checkout")}},
	}
	if _, err := env.Kube.CoreV1().Pods(testNamespace).Create(env.ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	watch(t, env)
	if !secretExists(t, env, files) {
		t.Fatalf("the files Secret was deleted while the checkout ran")
	}

	pod, err := env.Kube.CoreV1().Pods(testNamespace).Get(env.ctx, name+"-abcde", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{terminatedStatus("checkout", 0, "Completed", now.Add(-time.Minute), now)}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{runningStatus("pergent")}
	if _, err := env.Kube.CoreV1().Pods(testNamespace).Update(env.ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	watch(t, env)

	if secretExists(t, env, files) {
		t.Errorf("the files Secret is still there after the checkout")
	}
	if env.Job(id) == nil || !secretExists(t, env, name+"-secrets") {
		t.Errorf("the Job or its own Secret went with the files")
	}
	wantRunning(t, env, id)
	if r := env.Run(id); r.CheckoutSeen != 2 {
		t.Errorf("checkout seen %d, want 2", r.CheckoutSeen)
	}
	wantNoRepoRows(t, env)
}

// A failed API review Job is reported with its files deleted, and no
// repository gets a "Needs attention" item for it.
func TestWatchReportsAPIReviewWithoutAttention(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	ids := []string{a.ask(t, "route:1", twoFiles()), a.ask(t, "route:2", twoFiles())}
	dispatch(t, env)
	serveLog(env, "fatal: the commits could not be built\n")
	now := env.Now()
	for _, id := range ids {
		name := kube.JobName(id)
		job := env.Job(id)
		if job == nil {
			t.Fatalf("no Job for %s", id)
		}
		job.Status.Conditions = []batchv1.JobCondition{jobCond(batchv1.JobFailed, "BackoffLimitExceeded", now.Add(-2*time.Minute))}
		if _, err := env.Kube.BatchV1().Jobs(testNamespace).UpdateStatus(env.ctx, job, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-abcde", Namespace: testNamespace,
				Labels: map[string]string{"job-name": name}, CreationTimestamp: metav1.NewTime(now.Add(-5 * time.Minute))},
			Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
				terminatedStatus("checkout", 1, "Error", now.Add(-4*time.Minute), now.Add(-3*time.Minute))}},
		}
		if _, err := env.Kube.CoreV1().Pods(testNamespace).Create(env.ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	watch(t, env)

	for _, id := range ids {
		if r := env.Run(id); r.State != store.Reported || r.FailureStep != kube.StepClone {
			t.Errorf("run %s = %s at step %q (%s); want reported at the clone", id, r.State, r.FailureStep, r.FailureReason)
		}
		if secretExists(t, env, kube.JobName(id)+"-files") {
			t.Errorf("the files Secret of %s is still there after the report", id)
		}
	}
	wantNoRepoRows(t, env)
}

// wantNoRepoRows checks nothing was recorded for a repository: no project row
// and no "Needs attention" item.
func wantNoRepoRows(t *testing.T, env *testEnv) {
	t.Helper()
	if _, found, err := env.Store.Project(env.ctx, "", 0); err != nil || found {
		t.Errorf("a project row for an API review: found %v, %v", found, err)
	}
	att, err := env.Store.Attention(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(att) != 0 {
		t.Errorf("attention = %+v, want none", att)
	}
}

// A review superseded by a newer request of its subject has its Job deleted
// by the cleanup, which sets no status.
func TestSupersededAPIReviewCleanedUp(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	first := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	if env.Job(first) == nil {
		t.Fatalf("the first review has no Job")
	}
	second := a.ask(t, "route:1", twoFiles())
	if r := env.Run(first); r.State != store.Superseded {
		t.Fatalf("first review = %s, want superseded", r.State)
	}

	cleanup(t, env)

	if env.Job(first) != nil {
		t.Errorf("the superseded review's Job is still there")
	}
	if r := env.Run(first); !r.CleanedUp {
		t.Errorf("the superseded review is not marked cleaned up")
	}
	if r := env.Run(second); r.State != store.Queued {
		t.Errorf("the newer review = %s, want queued", r.State)
	}
	noGitLab(t, env)
}

// Each pass drops the sealed files no Job will need: those of a review
// superseded before its Job. A queued review keeps its own.
func TestPassDropsSpentAPIInputs(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	first := a.ask(t, "route:1", twoFiles())
	second := a.ask(t, "route:1", twoFiles())

	if err := env.Engine.dropSpentAPIInputs(env.ctx); err != nil {
		t.Fatalf("dropSpentAPIInputs: %v", err)
	}

	if _, found, err := env.Store.APIReviewInputs(env.ctx, first); err != nil || found {
		t.Errorf("the superseded review keeps its files (found %v, %v)", found, err)
	}
	if _, found, err := env.Store.APIReviewInputs(env.ctx, second); err != nil || !found {
		t.Errorf("the queued review lost its files (found %v, %v)", found, err)
	}
	if !strings.Contains(env.Logs(), "reviews=1") {
		t.Errorf("the drop is not logged:\n%s", env.Logs())
	}
}
