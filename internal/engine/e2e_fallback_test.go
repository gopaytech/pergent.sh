package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/quota"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// newFallbackE2E is an e2e where a team owns acme/widgets and its chain is
// one entry per name, each with the key "key-" + name.
func newFallbackE2E(t *testing.T, names ...string) (*e2e, []store.EntryRef) {
	t.Helper()
	e := newE2E(t)
	team := teamOwning(t, e.env, "Payments")
	var refs []store.EntryRef
	for _, n := range names {
		refs = append(refs, addTeamModel(t, e.env, team, n, "gpt-"+n, "key-"+n))
	}
	saveChain(t, e.env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: refs})
	return e, refs
}

// attempt dispatches, checks one new Job started on key, posts body as its
// result and publishes. It returns the Job's run.
func (e *e2e) attempt(t *testing.T, key string, body []byte, known ...string) string {
	t.Helper()
	dispatch(t, e.env)
	id := e.newJob(t, known...)
	wantJobKey(t, e.env, id, key)
	if code := e.postResult(t, id, string(e.env.Secret(id).Data["result-token"]), body); code != http.StatusOK {
		t.Fatalf("result of %s: status %d", id, code)
	}
	publish(t, e.env)
	return id
}

// withTokens is body with every skill reporting in and out tokens.
func withTokens(t *testing.T, body []byte, in, out int) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	for _, s := range m["skills"].([]any) {
		s.(map[string]any)["tokens"] = map[string]any{"input": in, "output": out}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func clean(t *testing.T, head string) []byte {
	return resultFor(t, head, reviewed("no_blocking_issues", "a.go"))
}

// wantFellBack checks run id was dropped for class.
func wantFellBack(t *testing.T, env *testEnv, id, class string) {
	t.Helper()
	if r := env.Run(id); r.State != store.Superseded || r.Outcome != store.OutcomeFellBack || r.OutcomeReason != class {
		t.Fatalf("run %s = %s %q %q; want superseded fell_back %s", id, r.State, r.Outcome, r.OutcomeReason, class)
	}
}

// Entry 1 out of quota: the run falls back, the next attempt runs entry 2,
// and its comment says it was reviewed with a backup.
func TestE2EFallbackQuota(t *testing.T) {
	e, _ := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	head := sha('a')
	servePublish(env, 1, head)
	env.Enqueue(1, head)

	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	wantFellBack(t, env, first, store.ClassQuota)
	second := e.attempt(t, "key-team-b", clean(t, head), first)

	wantPublished(t, env, second)
	r := env.Run(second)
	if r.AttemptOf == nil || *r.AttemptOf != first || r.ChainPosition != 2 || r.FallbackClass != store.ClassQuota {
		t.Errorf("attempt = of %v pos %d class %q", r.AttemptOf, r.ChainPosition, r.FallbackClass)
	}
	body := createdComment(t, env, 1)
	if !strings.Contains(body, "Reviewed with team-b (backup model, the main model was out of quota)") {
		t.Errorf("comment lacks the backup line:\n%s", body)
	}
	got := statusesOf(t, env, head)
	if len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "success") {
		t.Errorf("statuses = %v, want success last", got)
	}
}

// A rejected key falls back the same way, and its mark has no end.
func TestE2EFallbackAuth(t *testing.T) {
	e, refs := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	head := sha('a')
	servePublish(env, 1, head)
	env.Enqueue(1, head)

	first := e.attempt(t, "key-team-a", failedOn(t, head, "auth"))
	wantFellBack(t, env, first, store.ClassAuth)
	if m, ok := markOf(t, env, refs[0]); !ok || m.Class != store.ClassAuth || m.Until != nil {
		t.Fatalf("mark = %+v, %v", m, ok)
	}
	second := e.attempt(t, "key-team-b", clean(t, head), first)
	wantPublished(t, env, second)
	if body := createdComment(t, env, 1); !strings.Contains(body, "the main model's key was rejected") {
		t.Errorf("comment lacks the auth reason:\n%s", body)
	}
}

// A plain 429 is no fallback: the review fails as today, and no second Job
// starts.
func TestE2ENoFallbackPlain429(t *testing.T) {
	e, refs := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	head := sha('a')
	servePublish(env, 1, head)
	env.Enqueue(1, head)

	first := e.attempt(t, "key-team-a", failedOn(t, head, "rate_limit"))
	wantPublished(t, env, first)
	dispatch(t, env)
	if n := len(env.Jobs()); n != 1 {
		t.Errorf("%d Jobs, want the first alone", n)
	}
	if _, ok := markOf(t, env, refs[0]); ok {
		t.Error("a rate limit marked the entry")
	}
	if got := statusesOf(t, env, head); len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "failed") {
		t.Errorf("statuses = %v, want failed last", got)
	}
}

// Every entry out of quota: the review fails with each entry's reason, never
// passes, and approves nothing.
func TestE2EAllExhaustedFails(t *testing.T) {
	e, _ := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	head := sha('a')
	servePublish(env, 1, head)
	root := env.Enqueue(1, head)

	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	second := e.attempt(t, "key-team-b", failedOn(t, head, "quota"), first)
	wantFellBack(t, env, second, store.ClassQuota)
	dispatch(t, env) // the third attempt has no entry left
	publish(t, env)

	as, err := env.Store.Attempts(env.ctx, root)
	if err != nil || len(as) != 3 {
		t.Fatalf("attempts = %d, %v; want 3", len(as), err)
	}
	last := as[2]
	want := "all models exhausted: team-a (out of quota), team-b (out of quota)"
	if last.FailureReason != want || last.State != store.Published {
		t.Errorf("last attempt = %s %q; want published %q", last.State, last.FailureReason, want)
	}
	if n := len(env.Jobs()); n != 2 {
		t.Errorf("%d Jobs, want 2", n)
	}
	for _, s := range statusesOf(t, env, head) {
		if strings.HasPrefix(s, "success") {
			t.Errorf("a status passed: %v", statusesOf(t, env, head))
		}
	}
	if got := statusesOf(t, env, head); !strings.HasPrefix(got[len(got)-1], "failed") {
		t.Errorf("statuses = %v, want failed last", got)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 0 {
		t.Errorf("%d approvals", n)
	}
	if body := createdComment(t, env, 1); !strings.Contains(body, want) {
		t.Errorf("the comment doesn't say why:\n%s", body)
	}
}

// A dropped attempt posts nothing but the shared status: no comment, no
// thread, no approval, even with a blocking finding in its result.
func TestE2EDroppedAttemptPostsOnlyStatus(t *testing.T) {
	e, _ := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	head := sha('a')
	servePublish(env, 1, head)
	env.Enqueue(1, head)
	blocking := withProviderError(t, resultFor(t, head, blockingSkill(t)), "quota")

	e.attempt(t, "key-team-a", blocking)

	for _, path := range []string{notesPath(1), discussionsPath(1), approvePath(1)} {
		if n := len(env.GitLab.Requests(http.MethodPost, path)); n != 0 {
			t.Errorf("POST %s: %d requests from the dropped attempt", path, n)
		}
	}
	want := []string{"running: " + runningDescription, "running: retrying on backup model (2 of 2)"}
	if got := statusesOf(t, env, head); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("statuses = %v, want %v", got, want)
	}
}

// Each entry is tried once: 3 entries out of quota are 3 Jobs, on 3 keys,
// then the review fails.
func TestE2EEachEntryOnce(t *testing.T) {
	e, _ := newFallbackE2E(t, "team-a", "team-b", "team-c")
	env := e.env
	head := sha('a')
	servePublish(env, 1, head)
	root := env.Enqueue(1, head)

	var known []string
	for _, key := range []string{"key-team-a", "key-team-b", "key-team-c"} {
		known = append(known, e.attempt(t, key, failedOn(t, head, "quota"), known...))
	}
	dispatch(t, env)
	if n := len(env.Jobs()); n != 3 {
		t.Errorf("%d Jobs, want 3", n)
	}
	as, err := env.Store.Attempts(env.ctx, root)
	if err != nil || len(as) != 4 || !strings.HasPrefix(as[3].FailureReason, "all models exhausted") {
		t.Fatalf("attempts = %d, %v; want 3 dropped and a failed one", len(as), err)
	}
}

// A dropped attempt's tokens count: in Usage, and in the repository quota the
// next attempt is checked against.
func TestE2EDroppedTokensCount(t *testing.T) {
	e, _ := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	env.SetNow(novemberMid)
	q := int64(1500)
	env.Engine.Config.Repositories[0].MonthlyTokenQuota = &q
	head := sha('a')
	servePublish(env, 1, head)
	env.Enqueue(1, head)

	first := e.attempt(t, "key-team-a", withTokens(t, failedOn(t, head, "quota"), 1200, 300))
	wantFellBack(t, env, first, store.ClassQuota)
	cleanup(t, env) // the dropped attempt's Job goes
	n, err := env.Store.TokensSince(env.ctx, testInstance, testProjectID, quota.MonthStart(env.Now()))
	if err != nil || n != 1500 {
		t.Fatalf("TokensSince = %d, %v; want the dropped attempt's 1500", n, err)
	}
	dispatch(t, env)
	as, err := env.Store.Attempts(env.ctx, first)
	if err != nil || len(as) != 2 {
		t.Fatalf("attempts = %d, %v", len(as), err)
	}
	wantHeld(t, env, as[1].ID)
}

// The chain is read again for each attempt: an entry removed after the first
// is not run, the next one is.
func TestE2EChainEditBetweenAttempts(t *testing.T) {
	e, refs := newFallbackE2E(t, "team-a", "team-b", "team-c")
	env := e.env
	head := sha('a')
	servePublish(env, 1, head)
	env.Enqueue(1, head)

	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	team, ok, err := env.Store.OwnerTeam(env.ctx, testInstance, testProjectPath)
	if err != nil || !ok {
		t.Fatal(err)
	}
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{refs[0], refs[2]}})
	second := e.attempt(t, "key-team-c", clean(t, head), first)
	if r := env.Run(second); r.ChainPosition != 2 || r.ChainLength != 2 || r.ModelEntryID != refs[2].ID {
		t.Errorf("attempt = %q at %d of %d", r.ModelEntryID, r.ChainPosition, r.ChainLength)
	}
}

// A re-run after a fallback is a new review: it starts at position 1 (here
// the mark is cleared, so entry 1 runs).
func TestE2ERerunStartsAtOne(t *testing.T) {
	e, refs := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	head := sha('a')
	servePublish(env, 1, head)
	env.Enqueue(1, head)
	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	second := e.attempt(t, "key-team-b", clean(t, head), first)
	wantPublished(t, env, second)

	if cleared, err := env.Store.ClearMark(env.ctx, refs[0]); err != nil || !cleared {
		t.Fatalf("ClearMark = %v, %v", cleared, err)
	}
	rerun, created, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(1), testProjectPath, head, "main", store.TriggerRerun, nil, nil, true, true)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}
	dispatch(t, env)
	wantJobKey(t, env, rerun, "key-team-a")
	if r := env.Run(rerun); r.AttemptOf != nil || r.ChainPosition != 1 || r.FallbackClass != "" {
		t.Errorf("re-run = of %v at %d, class %q", r.AttemptOf, r.ChainPosition, r.FallbackClass)
	}
}

// A push during a fallback: the new head supersedes the queued attempt,
// nothing more is posted for the old head, and the new head's review starts
// at position 1.
func TestFallbackThenPush(t *testing.T) {
	e, refs := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	headA, headB := sha('a'), sha('c')
	servePublish(env, 1, headA)
	env.Enqueue(1, headA)
	first := e.attempt(t, "key-team-a", failedOn(t, headA, "quota"))
	cleanup(t, env)
	as, err := env.Store.Attempts(env.ctx, first)
	if err != nil || len(as) != 2 {
		t.Fatalf("attempts = %d, %v", len(as), err)
	}
	queued := as[1].ID
	before := len(statusesOf(t, env, headA))

	if cleared, err := env.Store.ClearMark(env.ctx, refs[0]); err != nil || !cleared {
		t.Fatalf("ClearMark = %v, %v", cleared, err)
	}
	servePublish(env, 1, headB)
	pushed := env.Enqueue(1, headB)
	dispatch(t, env)
	cleanup(t, env)

	if r := env.Run(queued); r.State != store.Superseded || r.Outcome == store.OutcomeFellBack {
		t.Errorf("queued attempt = %s %q, want superseded by the push", r.State, r.Outcome)
	}
	if env.Job(queued) != nil {
		t.Error("the superseded attempt got a Job")
	}
	wantJobKey(t, env, pushed, "key-team-a")
	if r := env.Run(pushed); r.AttemptOf != nil || r.ChainPosition != 1 {
		t.Errorf("new head's run = of %v at %d", r.AttemptOf, r.ChainPosition)
	}
	if n := len(statusesOf(t, env, headA)); n != before {
		t.Errorf("%d statuses on the old head after the push, want none more", n-before)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(1))); n != 0 {
		t.Errorf("%d comments", n)
	}
}

// On GitHub the attempts share one Check Run: one create, then updates only.
func TestE2EGitHubOneCheckRun(t *testing.T) {
	e := newGitHubE2E(t)
	env, gh := e.env, e.gh
	env.Engine.Config.LLM = config.LLM{}
	first := globalRef(t, env, "gpt-first", "key-first")
	second := globalRef(t, env, "gpt-second", "key-second")
	saveChain(t, env, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{first, second}})
	head := sha('a')
	servePublishGitHub(gh, 1, head)
	gh.Reply(http.MethodPatch, ghRepoAPI+"/check-runs/1", http.StatusOK, map[string]any{"id": 1})
	e.signedHook(t, "opened", 1, head, nil)

	a := e.attempt(t, "key-first", failedOn(t, head, "quota"))
	wantFellBack(t, env, a, store.ClassQuota)
	b := e.attempt(t, "key-second", clean(t, head), a)
	wantPublished(t, env, b)

	if n := len(gh.Requests(http.MethodPost, ghRepoAPI+"/check-runs")); n != 1 {
		t.Errorf("%d Check Runs created, want 1", n)
	}
	var titles []string
	patches := gh.Requests(http.MethodPatch, ghRepoAPI+"/check-runs/1")
	for _, p := range patches {
		var m struct {
			Conclusion string `json:"conclusion"`
			Output     struct {
				Title string `json:"title"`
			} `json:"output"`
		}
		if err := json.Unmarshal(p.Body, &m); err != nil {
			t.Fatal(err)
		}
		titles = append(titles, m.Output.Title+"/"+m.Conclusion)
	}
	if len(titles) != 3 || titles[0] != "retrying on backup model (2 of 2)/" || !strings.HasSuffix(titles[2], "/success") {
		t.Errorf("updates = %v; want the retry, the second attempt running, then success", titles)
	}
	if r := env.Run(b); r.StatusRef != 1 {
		t.Errorf("the attempt's Check Run = %d, want the first's", r.StatusRef)
	}
}
