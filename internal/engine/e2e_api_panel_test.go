package engine

import (
	"bytes"
	"net/http"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// newAPIPanelE2E is newAPIE2E on an image that has the panel, with a global
// chain B of one entry per name (model "gpt-" + name, key "key-" + name).
// Chain A is the global chain: the default model, on testLLMKey. It returns
// chain B's refs.
func newAPIPanelE2E(t *testing.T, names ...string) (*apiE2E, []store.EntryRef) {
	t.Helper()
	panelFloor(t, "v0.5.0")
	a := newAPIE2E(t)
	var refs []store.EntryRef
	for _, n := range names {
		refs = append(refs, globalRef(t, a.env, "gpt-"+n, "key-"+n))
	}
	saveChainB(t, a.env, store.Chain{Scope: store.ScopeGlobal, Entries: refs})
	return a, refs
}

// splitAPI asks for a review of twoFiles on subject and dispatches once: the
// review is split. It returns the review and its import.
func (a *apiE2E) splitAPI(t *testing.T, subject string) (string, reviewapi.Import) {
	t.Helper()
	im, err := reviewapi.BuildImport(twoFiles())
	if err != nil {
		t.Fatal(err)
	}
	id := a.ask(t, subject, twoFiles())
	dispatch(t, a.env)
	if r := a.env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitPanel || r.DispatchedAt.IsZero() {
		t.Fatalf("review = %s/%q; want a panel\n%s", r.State, r.SkipReason, a.env.Logs())
	}
	if n := len(a.env.Jobs()); n != 0 {
		t.Fatalf("%d Jobs after the split; want none", n)
	}
	return id, im
}

// wantFiles checks whether review id still has its stored files.
func wantFiles(t *testing.T, env *testEnv, id string, want bool, when string) {
	t.Helper()
	if _, found, err := env.Store.APIReviewInputs(env.ctx, id); err != nil || found != want {
		t.Errorf("%s: stored files found %v (%v); want %v", when, found, err, want)
	}
}

// wantFilesSecret checks the Job of run or member id has the files Secret
// with the request's stream.
func wantFilesSecret(t *testing.T, env *testEnv, id string, im reviewapi.Import) {
	t.Helper()
	files, err := env.Kube.CoreV1().Secrets(testNamespace).Get(env.ctx, kube.JobName(id)+"-files", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the Job of %s has no files Secret: %v", id, err)
	}
	if !bytes.Equal(files.Data["import"], im.Stream) {
		t.Errorf("the files Secret of %s holds %q, want the request's stream", id, files.Data["import"])
	}
}

// An API review with a global chain B runs as a panel: the split, then a
// member Job per role, each with the files Secret and its own key, then Job
// P. The stored files stay through the members and the clean-up pass, and
// go once Job P is created. The review reads running while the members run,
// and the v1 result is P's.
func TestE2EAPIPanel(t *testing.T) {
	a, _ := newAPIPanelE2E(t, "b")
	env := a.env
	id, im := a.splitAPI(t, "route:1")
	wantFiles(t, env, id, true, "after the split")
	if got := a.review(t, id)["status"]; got != reviewapi.StatusRunning {
		t.Errorf("status after the split = %v, want running", got)
	}
	if r := env.Run(id); r.BaseSHA != im.Base || r.StartSHA != "" || r.Model == nil || *r.Model != "claude-sonnet-4" ||
		r.SkillSources["security"] != "library" {
		t.Errorf("run = base %s start %q model %v sources %v; want the request's base and A's inputs", r.BaseSHA, r.StartSHA, r.Model, r.SkillSources)
	}

	dispatch(t, env)
	ma, mb := memberOfRole(t, env, id, "a"), memberOfRole(t, env, id, "b")
	wantMemberKey(t, env, ma, testLLMKey)
	wantMemberKey(t, env, mb, "key-b")
	for _, m := range []store.PanelMember{ma, mb} {
		wantFilesSecret(t, env, m.ID, im)
		job := env.Job(m.ID)
		if job.Labels[kube.LabelAPIClient] != "gateway" || job.Labels[kube.LabelPanelRole] != m.Role {
			t.Errorf("member %s's labels = %v; want the client and the role", m.Role, job.Labels)
		}
		args := job.Spec.Template.Spec.Containers[0].Args
		if !hasArg(args, "--no-skill-verify") ||
			!hasArg(args, "--result-url="+testCallbackURL+"/runs/"+id+"/members/"+m.ID+"/result") {
			t.Errorf("member %s's args = %v; want the member's route and no skill verify", m.Role, args)
		}
	}
	if mb.Entry.Source != store.SourceGlobal || mb.Model != "gpt-b" {
		t.Errorf("member B = %+v; want the global chain B's entry", mb)
	}
	wantFiles(t, env, id, true, "after both member Jobs started")
	if err := env.Engine.dropSpentAPIInputs(env.ctx); err != nil {
		t.Fatal(err)
	}
	wantFiles(t, env, id, true, "after the clean-up pass while the members run")
	if got := a.review(t, id)["status"]; got != reviewapi.StatusRunning {
		t.Errorf("status while the members run = %v, want running", got)
	}

	clean := apiResultFor(t, im.Base, im.Head, reviewed("no_blocking_issues", "routes/api.yaml", "routes/new.yaml"))
	for _, m := range []store.PanelMember{ma, mb} {
		if code := a.postMember(t, id, m, clean); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d\n%s", m.Role, code, env.Logs())
		}
	}
	publish(t, env)
	if r := env.Run(id); r.State != store.Queued {
		t.Fatalf("review = %s; want released for Job P\n%s", r.State, env.Logs())
	}
	if err := env.Engine.dropSpentAPIInputs(env.ctx); err != nil {
		t.Fatal(err)
	}
	wantFiles(t, env, id, true, "after the release")

	dispatch(t, env)
	job := env.Job(id)
	if job == nil || job.Labels[kube.LabelPanelRole] != kube.PanelRoleMerge {
		t.Fatalf("no Job P\n%s", env.Logs())
	}
	if args := job.Spec.Template.Spec.Containers[0].Args; !hasArg(args, "--panel") || !hasArg(args, "--timeout=10m0s") {
		t.Errorf("P's args = %v; want --panel and its own timeout", args)
	}
	wantFilesSecret(t, env, id, im)
	sec := env.Secret(id)
	if string(sec.Data["llm-api-key"]) != testLLMKey || string(sec.Data["llm-api-key-b"]) != "key-b" {
		t.Errorf("P's keys = %q, %q; want A's and B's", sec.Data["llm-api-key"], sec.Data["llm-api-key-b"])
	}
	if js := panelJudges(t, env, id); len(js) != 2 || js["b"].ID != mb.Entry.ID {
		t.Errorf("judges = %+v; want both roles", js)
	}
	wantFiles(t, env, id, false, "after Job P was created")
	if r := env.Run(id); r.State != store.Running || r.JobName != kube.JobName(id) {
		t.Errorf("review = %s job %q; want running with Job P", r.State, r.JobName)
	}

	if code := a.postResult(t, id, string(sec.Data["result-token"]), apiResultFor(t, im.Base, im.Head, blockingSkill(t))); code != http.StatusOK {
		t.Fatalf("P's result: status %d\n%s", code, env.Logs())
	}
	publish(t, env)
	wantPublished(t, env, id)
	if body := a.stored(t, id); body.Status != reviewapi.StatusCompleted || body.Verdict != reviewapi.VerdictHasBlockingIssues {
		t.Errorf("stored = %q, %q; want P's verdict, has_blocking_issues", body.Status, body.Verdict)
	}
	noGitLab(t, env)
}

// A newer request of the subject supersedes a review whose members run: the
// clean-up deletes the member Jobs, and the clean-up pass drops the files.
func TestE2EAPIPanelSupersededDropsFiles(t *testing.T) {
	a, _ := newAPIPanelE2E(t, "b")
	env := a.env
	id, _ := a.splitAPI(t, "route:1")
	dispatch(t, env)
	ma, mb := memberOfRole(t, env, id, "a"), memberOfRole(t, env, id, "b")
	if env.Job(ma.ID) == nil || env.Job(mb.ID) == nil {
		t.Fatalf("the members have no Jobs\n%s", env.Logs())
	}

	second := a.ask(t, "route:1", twoFiles())
	if r := env.Run(id); r.State != store.Superseded {
		t.Fatalf("review = %s; want superseded by the newer request", r.State)
	}
	cleanup(t, env)
	for _, m := range []store.PanelMember{ma, mb} {
		if env.Job(m.ID) != nil {
			t.Errorf("member %s's Job is still there", m.Role)
		}
		if got := memberOfRole(t, env, id, m.Role); got.State != store.MemberSuperseded {
			t.Errorf("member %s = %s; want superseded", m.Role, got.State)
		}
	}
	if err := env.Engine.dropSpentAPIInputs(env.ctx); err != nil {
		t.Fatal(err)
	}
	wantFiles(t, env, id, false, "after the clean-up pass")
	wantFiles(t, env, second, true, "the newer review")
	noGitLab(t, env)
}

// The client is removed while its review's members run: the removal names
// both member Jobs, and the files go with the review.
func TestE2EAPIPanelClientRemoved(t *testing.T) {
	a, _ := newAPIPanelE2E(t, "b")
	env := a.env
	id, _ := a.splitAPI(t, "route:1")
	dispatch(t, env)
	ma, mb := memberOfRole(t, env, id, "a"), memberOfRole(t, env, id, "b")

	jobs, found, err := env.Store.DeleteAPIClient(env.ctx, "gateway")
	if err != nil || !found {
		t.Fatalf("DeleteAPIClient = %v, %v", found, err)
	}
	slices.Sort(jobs)
	want := []string{kube.JobName(ma.ID), kube.JobName(mb.ID)}
	slices.Sort(want)
	if !slices.Equal(jobs, want) {
		t.Errorf("jobs to delete = %v; want the 2 members' %v", jobs, want)
	}
	if err := env.Engine.dropSpentAPIInputs(env.ctx); err != nil {
		t.Fatal(err)
	}
	wantFiles(t, env, id, false, "after the client was removed")
}

// API members do not fall back: B's quota marks B's entry and fails B, with
// no next attempt, and Job P runs with A only.
func TestE2EAPIPanelNoMemberFallback(t *testing.T) {
	a, refs := newAPIPanelE2E(t, "b", "c")
	env := a.env
	id, im := a.splitAPI(t, "route:1")
	dispatch(t, env)
	ma, mb := memberOfRole(t, env, id, "a"), memberOfRole(t, env, id, "b")
	wantMemberKey(t, env, mb, "key-b")

	clean := apiResultFor(t, im.Base, im.Head, reviewed("no_blocking_issues", "routes/api.yaml"))
	quota := withProviderError(t, apiResultFor(t, im.Base, im.Head, result.Skill{Name: "security", Verdict: "failed",
		Failures: []result.Failure{{Files: []string{"routes/api.yaml"}, Message: "the provider refused the request"}}}), "quota")
	if code := a.postMember(t, id, ma, clean); code != http.StatusOK {
		t.Fatalf("member A's result: status %d", code)
	}
	if code := a.postMember(t, id, mb, quota); code != http.StatusOK {
		t.Fatalf("member B's result: status %d\n%s", code, env.Logs())
	}
	publish(t, env)

	if m, ok := markOf(t, env, refs[0]); !ok || m.Class != store.ClassQuota {
		t.Errorf("B's mark = %+v, %v; want quota", m, ok)
	}
	entries, err := env.Store.AuditLog(env.ctx, 0, 10)
	if err != nil || len(entries) == 0 || entries[0].Action != "model_marked" || entries[0].Detail != "model=openai/gpt-b class=quota" {
		t.Errorf("audit = %+v, %v; want the mark", entries, err)
	}
	ms := members(t, env, id)
	if len(ms) != 2 {
		t.Fatalf("members = %+v; want A and B alone (no next attempt)", ms)
	}
	if got := memberOfRole(t, env, id, "b"); got.ID != mb.ID || got.State != store.MemberFailed || got.FailureReason == "" {
		t.Errorf("member B = %s %q; want failed", got.State, got.FailureReason)
	}
	if r := env.Run(id); r.State != store.Queued {
		t.Fatalf("review = %s; want released for Job P with A only\n%s", r.State, env.Logs())
	}

	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no Job P\n%s", env.Logs())
	}
	if got := secretKeys(env, id); slices.Contains(got, "llm-api-key-b") {
		t.Errorf("P's Secret keys = %v; want no key B", got)
	}
	if js := panelJudges(t, env, id); len(js) != 1 || js["a"].ID == "" {
		t.Errorf("judges = %+v; want A's alone", js)
	}
	wantFiles(t, env, id, false, "after Job P was created")
}

// A client with its own model runs member A on it, and member B on the
// global chain B.
func TestE2EAPIClientModelPanel(t *testing.T) {
	a, _ := newAPIPanelE2E(t, "b")
	env := a.env
	own := addModel(t, env, "openai", "gpt-own", "", "key-own")
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) { c.ModelID = own }); err != nil {
		t.Fatal(err)
	}
	id, im := a.splitAPI(t, "route:1")
	if r := env.Run(id); r.Model == nil || *r.Model != "gpt-own" {
		t.Errorf("run model = %v; want the client's", r.Model)
	}

	dispatch(t, env)
	ma, mb := memberOfRole(t, env, id, "a"), memberOfRole(t, env, id, "b")
	wantMemberKey(t, env, ma, "key-own")
	wantMemberKey(t, env, mb, "key-b")
	if ma.Model != "gpt-own" || ma.Entry.ID != own || mb.Model != "gpt-b" {
		t.Errorf("members = A %s %+v, B %s; want A on the client's model and B on chain B", ma.Model, ma.Entry, mb.Model)
	}

	clean := apiResultFor(t, im.Base, im.Head, reviewed("no_blocking_issues", "routes/api.yaml"))
	for _, m := range []store.PanelMember{ma, mb} {
		if code := a.postMember(t, id, m, clean); code != http.StatusOK {
			t.Fatalf("member %s's result: status %d", m.Role, code)
		}
	}
	publish(t, env)
	dispatch(t, env)
	sec := env.Secret(id)
	if string(sec.Data["llm-api-key"]) != "key-own" || string(sec.Data["llm-api-key-b"]) != "key-b" {
		t.Errorf("P's keys = %q, %q; want the client's and B's", sec.Data["llm-api-key"], sec.Data["llm-api-key-b"])
	}
	if js := panelJudges(t, env, id); js["a"].ID != own {
		t.Errorf("judges = %+v; want A on the client's model", js)
	}
}
