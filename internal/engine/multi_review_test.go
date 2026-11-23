package engine

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

var updateDispatchJob = flag.Bool("update-dispatch-job", false, "rewrite testdata/dispatch-job.golden.json (only when a review's dispatch changes on purpose)")

// goldenEnv is a dashboard repository with every optional input of a
// review's dispatch on: a base URL and a work size, a published review to
// carry files from, a past dismissal, the change's description, CI results,
// quiet, a layout, check deleted, path rules, the code graph and its own
// timeout, step limit and memory. It returns the environment and the queued
// run of head sha('2') on merge request 3. setup, when not nil, runs before
// the run is queued.
func goldenEnv(t *testing.T, setup func(env *testEnv)) (*testEnv, string) {
	t.Helper()
	env := newTestEnv(t)
	env.Engine.Config.LLM.BaseURL = "https://llm.example.com/v1"
	env.Engine.Config.Jobs.WorkSize = resource.MustParse("20Gi")
	env.Engine.ScanImage = testScanImage
	env.Engine.GraphImage = testGraphImage
	env.Engine.GraphDir = t.TempDir()
	env.Engine.Config.Repositories = nil
	timeout, turns, memory := 900, 50, "6Gi"
	addRuleSet(t, env, "a", setAYAML)
	dashboardRepo(t, env, store.DashboardRepo{
		Instance: testInstance, Project: testProjectPath, CreatedBy: "alice",
		JobTimeoutSeconds: &timeout, MaxTurns: &turns, JobMemory: &memory,
		QuietReviews: boolPtr(true), CheckDeleted: boolPtr(true),
		CommentLayout: []string{"checks:open", "findings:collapsed"},
		RuleSets:      []string{"a"},
		CIJobs: &store.CIJobs{WaitSeconds: 1800, Jobs: []store.CIJob{
			{Name: "test", Read: "log", BlocksApproval: true},
		}},
	})
	if _, _, err := env.Store.SetDashboardRepoGraph(env.ctx, testInstance, testProjectPath, true); err != nil {
		t.Fatal(err)
	}
	storeTestGraph(t, env, "main", sha('d'))
	env.GitLab.Reply(http.MethodGet, filePath(rule.RepoFile), http.StatusOK, repoRulesYAML)

	headA, headB := sha('1'), sha('2')
	serveCI(env, headA, "success", "success")
	publishedReview(t, env, headA, "0.5.0", nil)
	teach(t, env, 1, "security", "aaaaaa111111", "blocking", "Not a secret: a public request ID.", 99)
	if setup != nil {
		setup(env)
	}

	id := env.Enqueue(3, headB)
	mr := authoredBy(openMR(3, headB), 7)
	mr.Title, mr.Description = "Add login", "It drops the old cookie on purpose."
	mr.WebURL = "https://gitlab.example.com/acme/widgets/-/merge_requests/3"
	env.GitLab.SetMR(mr)
	env.GitLab.Reply(http.MethodPost, statusPath(headB), http.StatusCreated, `{}`)
	serveCI(env, headB, "success", "success")
	serveChangedSince(env, "b.go", "d.go")
	env.GitLab.Reset()
	return env, id
}

// dispatchGolden is what the dispatch of run id wrote, as JSON: the Job, its
// ConfigMap and its Secret, the run's row and the platform writes. The run's
// ID, the earlier review's, the Job's name and the result token are fixed
// text, and times are offsets from the engine's clock.
func dispatchGolden(t *testing.T, env *testEnv, id string) string {
	t.Helper()
	job := env.Job(id)
	if job == nil {
		t.Fatalf("no job; run = %+v\n%s", env.Run(id), env.Logs())
	}
	cm, sec := env.ConfigMap(id), env.Secret(id)
	token := string(sec.Data["result-token"])
	if token == "" {
		t.Fatal("the Secret has no result token")
	}
	sec.Data["result-token"] = []byte("RESULT-TOKEN")

	run := env.Run(id)
	at := func(x time.Time) string {
		if x.IsZero() {
			return ""
		}
		return x.Sub(env.Now()).String()
	}
	times := map[string]string{
		"dispatched_at":    at(run.DispatchedAt),
		"token_expires_at": at(run.TokenExpiresAt),
		"reported_at":      at(run.ReportedAt),
	}
	run.CreatedAt, run.NotBefore, run.DispatchedAt, run.TokenExpiresAt, run.ReportedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}

	var writes []map[string]any
	for _, r := range env.GitLab.Requests("", "") {
		if r.Method == http.MethodGet {
			continue
		}
		w := map[string]any{"method": r.Method, "path": r.Path}
		if len(r.Body) > 0 {
			var body any
			if err := json.Unmarshal(r.Body, &body); err != nil {
				w["body"] = string(r.Body)
			} else {
				w["body"] = body
			}
		}
		writes = append(writes, w)
	}

	b, err := json.MarshalIndent(map[string]any{
		"job":       job,
		"configMap": cm,
		"secret":    sec,
		"run":       run,
		"runTimes":  times,
		"writes":    writes,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out := string(b) + "\n"
	if strings.Contains(out, token) {
		t.Fatal("the result token is still in the golden text")
	}
	out = strings.ReplaceAll(out, kube.JobName(id), "JOB-NAME")
	out = strings.ReplaceAll(out, id, "RUN-ID")
	if run.BaseRunID != nil {
		out = strings.ReplaceAll(out, *run.BaseRunID, "BASE-RUN-ID")
	}
	return out
}

// checkDispatchGolden compares what the dispatch of id wrote with the bytes
// pinned before review panels existed.
func checkDispatchGolden(t *testing.T, env *testEnv, id string) {
	t.Helper()
	got := dispatchGolden(t, env, id)
	const path = "testdata/dispatch-job.golden.json"
	if *updateDispatchJob {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("a review's dispatch changed:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// A review with every optional input is dispatched byte for byte as it was
// before review panels existed: its Job, ConfigMap and Secret, its run's row
// and what it posts.
func TestDispatchJobUnchanged(t *testing.T) {
	env, id := goldenEnv(t, nil)
	dispatch(t, env)
	checkDispatchGolden(t, env, id)
	if ms := reviewMembers(t, env, id); len(ms) != 0 {
		t.Errorf("review members = %+v; want none", ms)
	}
}

// startRunOld does what the StartRun of a release before review panels does
// to a waiting run: it queues it, keeping its dispatched_at.
func startRunOld(t *testing.T, env *testEnv, id string) {
	t.Helper()
	execSQL(t, env, fmt.Sprintf(`UPDATE runs SET state = 'queued', trigger = 'start', skip_reason = '',
		not_before = NULL WHERE id = '%s'`, id))
}

// multiReviewFloor sets the first pergent with multi review for the test.
func multiReviewFloor(t *testing.T, v string) {
	t.Helper()
	old := config.PanelMinVersion
	config.PanelMinVersion = v
	t.Cleanup(func() { config.PanelMinVersion = old })
}

// saveExtraReviewers stores reviewers 2.. of acme/widgets, one chain per
// item of extra, and the judge chain. A multi review needs pergent.sh's own
// image, so it is set when the test left it empty.
func saveExtraReviewers(t *testing.T, env *testEnv, extra [][]store.EntryRef, judge []store.EntryRef) {
	t.Helper()
	if env.Engine.ScanImage == "" {
		env.Engine.ScanImage = testScanImage
	}
	if _, err := env.Store.SaveReviewers(env.ctx, store.ReviewersSave{Instance: testInstance, Project: testProjectPath,
		Extra: extra, Judge: judge, SavedByAdmin: true}, env.Now()); err != nil {
		t.Fatalf("SaveReviewers: %v", err)
	}
}

// reviewMembers is the review_members rows of run id's review.
func reviewMembers(t *testing.T, env *testEnv, id string) []store.ReviewMember {
	t.Helper()
	ms, err := env.Store.ReviewMembers(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// wantSplit checks run id waits for its reviewers with no Job, and returns
// its members.
func wantSplit(t *testing.T, env *testEnv, id string, n int) []store.ReviewMember {
	t.Helper()
	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitReviewers {
		t.Fatalf("run = %s/%q; want waiting/%s\n%s", r.State, r.SkipReason, store.WaitReviewers, env.Logs())
	}
	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Errorf("%d Jobs were created", len(jobs))
	}
	ms := reviewMembers(t, env, id)
	if len(ms) != n {
		t.Fatalf("members = %+v; want %d", ms, n)
	}
	for i, m := range ms {
		if m.Reviewer != i+1 || m.RunID != id {
			t.Errorf("member %d = reviewer %d of %s; want reviewer %d of %s", i, m.Reviewer, m.RunID, i+1, id)
		}
	}
	return ms
}

// wantSingle checks run id runs today's review Job, on chain A's key, with
// one off row of reason.
func wantSingle(t *testing.T, env *testEnv, id, reason string) {
	t.Helper()
	wantJobKey(t, env, id, testLLMKey)
	if r := env.Run(id); r.State != store.Running || r.JobName != kube.JobName(id) {
		t.Fatalf("run = %s job %q; want running with its Job", r.State, r.JobName)
	}
	ms := reviewMembers(t, env, id)
	if len(ms) != 1 || ms[0].State != store.MemberOff || ms[0].FailureReason != reason {
		t.Fatalf("members = %+v; want one off row %q", ms, reason)
	}
	if strings.Contains(env.Logs(), "key-b") || strings.Contains(env.Logs(), testLLMKey) {
		t.Errorf("a key reached the log:\n%s", env.Logs())
	}
}

// wantSkipped checks member m could not start, for reason.
func wantSkipped(t *testing.T, m store.ReviewMember, reason string) {
	t.Helper()
	if m.State != store.MemberSkipped || m.FailureReason != reason {
		t.Errorf("reviewer %d = %s %q; want skipped %q", m.Reviewer, m.State, m.FailureReason, reason)
	}
}

// Why an extra reviewer can't start: the quota when only quotas, limits and
// models in use stopped its entries (and at least one quota or limit), the
// models in use when only they did, else no usable entry. A tried entry
// neither stops nor names the reason.
func TestSkipWords(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reasons []string
		want    string
	}{
		{"no entry", nil, skipNone},
		{"same model only", []string{chain.ReasonSameModel, chain.ReasonSameModel}, skipSame},
		{"instance quota only", []string{chain.ReasonInstanceQuota}, skipQuota},
		{"model limit only", []string{chain.ReasonModelLimit}, skipQuota},
		{"team limit only", []string{chain.ReasonTeamLimit}, skipQuota},
		{"same model and a limit", []string{chain.ReasonSameModel, chain.ReasonModelLimit}, skipQuota},
		{"a limit and same model", []string{chain.ReasonTeamLimit, chain.ReasonSameModel}, skipQuota},
		{"quota and same model", []string{chain.ReasonInstanceQuota, chain.ReasonSameModel}, skipQuota},
		{"quota and a tried entry", []string{chain.ReasonTried, chain.ReasonInstanceQuota}, skipQuota},
		{"same model and a tried entry", []string{chain.ReasonTried, chain.ReasonSameModel}, skipSame},
		{"tried only", []string{chain.ReasonTried}, skipNone},
		{"removed", []string{chain.ReasonRemoved}, skipNone},
		{"same model and a removed entry", []string{chain.ReasonSameModel, chain.ReasonRemoved}, skipNone},
		{"quota and a mark", []string{chain.ReasonInstanceQuota, store.ClassQuota}, skipNone},
		{"a limit and a mark", []string{chain.ReasonTeamLimit, store.ClassQuota}, skipNone},
		{"no key", []string{chain.ReasonKeyUnusable}, skipNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dec chain.Decision
			for _, r := range tc.reasons {
				dec.Skips = append(dec.Skips, chain.Skip{Reason: r})
			}
			if got := skipWords(dec); got != tc.want {
				t.Errorf("skipWords(%v) = %q, want %q", tc.reasons, got, tc.want)
			}
		})
	}
}

// sameModels names the model of each decision that runs, in order.
func TestSameModels(t *testing.T) {
	run := func(provider, model string) chain.Decision {
		d := chain.Decision{Position: 1, Length: 1}
		d.Entry.LLM.Provider, d.Entry.LLM.Model = provider, model
		return d
	}
	got := sameModels([]chain.Decision{run("anthropic", "a"), {Wait: true}, {Exhausted: true}, run("openai", "b")})
	want := []chain.SameModel{{Provider: "anthropic", Model: "a"}, {Provider: "openai", Model: "b"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("sameModels = %+v, want %+v", got, want)
	}
}

// A review with 2 extra reviewers, on an image that has multi review, is
// split: the run waits for its 3 members with dispatched_at set and chain
// A's entry kept, each member queued with its frozen chain, and the judge
// chain stored; no Job, ConfigMap, Secret or status yet.
func TestDispatchSplitsMultiReview(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := newTestEnv(t)
	b := globalRef(t, env, "gpt-b", "key-b")
	c := globalRef(t, env, "gpt-c", "key-c")
	j := globalRef(t, env, "gpt-j", "key-j")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}, {c}}, []store.EntryRef{j})
	id := dispatchMR(t, env, 3, sha('a'))

	ms := wantSplit(t, env, id, 3)
	r := env.Run(id)
	if !r.DispatchedAt.Equal(env.Now()) || r.JobName != "" {
		t.Errorf("dispatched at %s, job %q; want now and no Job", r.DispatchedAt, r.JobName)
	}
	if r.ModelEntryID != store.ConfigModelID || r.Provider == nil || *r.Provider != "anthropic" {
		t.Errorf("run entry = %q provider %v; want chain A's", r.ModelEntryID, r.Provider)
	}
	for i, want := range []struct {
		ref   store.EntryRef
		model string
	}{{configRef(), "claude-sonnet-4"}, {b, "gpt-b"}, {c, "gpt-c"}} {
		m := ms[i]
		if m.State != store.MemberQueued {
			t.Errorf("reviewer %d = %s; want queued", m.Reviewer, m.State)
		}
		if len(m.Chain.Entries) != 1 || m.Chain.Entries[0].ID != want.ref.ID || m.Chain.Entries[0].Model != want.model {
			t.Errorf("reviewer %d chain = %+v; want %s (%s)", m.Reviewer, m.Chain, want.ref.ID, want.model)
		}
	}
	if ms[1].Chain.Scope != store.ScopeRepo || !ms[1].Chain.SavedByAdmin {
		t.Errorf("reviewer 2 chain = %+v; want the repository's, saved by an admin", ms[1].Chain)
	}
	judge, found, err := env.Store.JudgeChainOf(env.ctx, id)
	if err != nil || !found || len(judge.Entries) != 1 || judge.Entries[0].ID != j.ID || judge.Entries[0].Model != "gpt-j" {
		t.Errorf("judge chain = %+v, %v, %v; want gpt-j", judge, found, err)
	}
	if l, err := env.Kube.CoreV1().Secrets(testNamespace).List(env.ctx, metav1.ListOptions{}); err != nil || len(l.Items) != 0 {
		t.Errorf("secrets = %v, %v; want none", l, err)
	}
	if l, err := env.Kube.CoreV1().ConfigMaps(testNamespace).List(env.ctx, metav1.ListOptions{}); err != nil || len(l.Items) != 0 {
		t.Errorf("config maps = %v, %v; want none", l, err)
	}
	if st := env.GitLab.Requests(http.MethodPost, statusPath(sha('a'))); len(st) != 0 {
		t.Errorf("%d statuses were set", len(st))
	}
	for _, key := range []string{"key-b", "key-c", "key-j", testLLMKey} {
		if strings.Contains(env.Logs(), key) {
			t.Errorf("key %q reached the log:\n%s", key, env.Logs())
		}
	}
	for _, m := range ms {
		if b, _ := json.Marshal(m.Chain); strings.Contains(string(b), "key-") {
			t.Errorf("a key is in reviewer %d's frozen chain: %s", m.Reviewer, b)
		}
	}
}

// Reviewer 3's only entry runs on reviewer 2's model (another key): it is
// skipped with that reason, and reviewer 2 runs. A later entry on another
// model would have let it start.
func TestDispatchSplitSkipsSameModel(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := newTestEnv(t)
	b := globalRef(t, env, "gpt-b", "key-b")
	same := globalRef(t, env, "gpt-b", "key-b2")
	j := globalRef(t, env, "gpt-j", "key-j")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}, {same}}, []store.EntryRef{j})
	id := dispatchMR(t, env, 3, sha('a'))

	ms := wantSplit(t, env, id, 3)
	if ms[1].State != store.MemberQueued {
		t.Errorf("reviewer 2 = %s; want queued", ms[1].State)
	}
	wantSkipped(t, ms[2], "no different model left")
}

// Reviewer 3's chain starts with chain A's model: it skips it and starts on
// its next entry.
func TestDispatchSplitAvoidsReviewer1Model(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := newTestEnv(t)
	b := globalRef(t, env, "gpt-b", "key-b")
	sameA := store.EntryRef{Source: store.SourceGlobal, ID: addModel(t, env, "Anthropic", "Claude-Sonnet-4", "", "key-a2")}
	j := globalRef(t, env, "gpt-j", "key-j")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}, {sameA}}, []store.EntryRef{j})
	id := dispatchMR(t, env, 3, sha('a'))
	ms := wantSplit(t, env, id, 3)
	wantSkipped(t, ms[2], "no different model left")

	env2 := newTestEnv(t)
	b2 := globalRef(t, env2, "gpt-b", "key-b")
	sameA2 := store.EntryRef{Source: store.SourceGlobal, ID: addModel(t, env2, "anthropic", "claude-sonnet-4", "", "key-a2")}
	c2 := globalRef(t, env2, "gpt-c", "key-c")
	saveExtraReviewers(t, env2, [][]store.EntryRef{{b2}, {sameA2, c2}}, []store.EntryRef{c2})
	id2 := dispatchMR(t, env2, 3, sha('a'))
	if ms := wantSplit(t, env2, id2, 3); ms[2].State != store.MemberQueued {
		t.Errorf("reviewer 3 = %s %q; want queued on its next entry", ms[2].State, ms[2].FailureReason)
	}
}

// A run asked for under a used-up instance quota goes ahead on chain A's
// global entry, but reviewer 2, on global entries only, is skipped for the
// quota; reviewer 3, on a team entry, starts.
func TestDispatchSplitInstanceQuota(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := quotaPushEnv(t, 0, 0)
	instQuota := int64(500)
	env.Engine.Config.GitLab[0].MonthlyTokenQuota = &instQuota
	seedQuotaUsage(t, env, 500)
	team := teamOwning(t, env, "Payments")
	b := globalRef(t, env, "gpt-b", "key-b")
	c := addTeamModel(t, env, team, "team-c", "gpt-c", "key-c")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}, {c}}, []store.EntryRef{c})
	head := sha('a')
	servePush(env, head)
	id, _, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(3), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)

	ms := wantSplit(t, env, id, 3)
	wantSkipped(t, ms[1], "the token quota")
	if ms[2].State != store.MemberQueued {
		t.Errorf("reviewer 3 = %s %q; want queued", ms[2].State, ms[2].FailureReason)
	}
}

// An extra reviewer never goes past a limit: reviewer 2's only entry at its
// model limit is skipped for the quota while reviewer 3 starts; with no
// other extra reviewer the review runs as one, for the quota.
func TestDispatchSplitSkipsAtModelLimit(t *testing.T) {
	t.Run("another reviewer starts", func(t *testing.T) {
		multiReviewFloor(t, "v0.5.0")
		env := quotaPushEnv(t, 0, 0)
		b := globalRef(t, env, "gpt-b", "key-b")
		c := globalRef(t, env, "gpt-c", "key-c")
		seedEntryUsage(t, env, b, 10)
		setLimit(t, env, b, 0, 10)
		saveExtraReviewers(t, env, [][]store.EntryRef{{b}, {c}}, []store.EntryRef{c})
		head := sha('a')
		servePush(env, head)
		id := env.Enqueue(3, head)
		dispatch(t, env)
		ms := wantSplit(t, env, id, 3)
		wantSkipped(t, ms[1], "the token quota")
		if ms[2].State != store.MemberQueued {
			t.Errorf("reviewer 3 = %s %q; want queued", ms[2].State, ms[2].FailureReason)
		}
	})
	t.Run("none starts", func(t *testing.T) {
		multiReviewFloor(t, "v0.5.0")
		env := quotaPushEnv(t, 0, 0)
		b := globalRef(t, env, "gpt-b", "key-b")
		seedEntryUsage(t, env, b, 10)
		setLimit(t, env, b, 0, 10)
		saveExtraReviewers(t, env, [][]store.EntryRef{{b}}, []store.EntryRef{b})
		head := sha('a')
		servePush(env, head)
		id := env.Enqueue(3, head)
		dispatch(t, env)
		wantSingle(t, env, id, "the token quota is used up")
	})
}

// An admin's start of a limit-held change splits it: reviewer 1 goes past
// the admin limits as the start does, but an extra reviewer at its limit is
// skipped, and no member holds the review.
func TestDispatchSplitAfterLimitStart(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := quotaPushEnv(t, 0, 0)
	b := globalRef(t, env, "gpt-b", "key-b")
	c := globalRef(t, env, "gpt-c", "key-c")
	seedEntryUsage(t, env, configRef(), 10)
	setLimit(t, env, configRef(), 0, 10)
	seedEntryUsage(t, env, b, 10)
	setLimit(t, env, b, 0, 10)
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}, {c}}, []store.EntryRef{c})
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantLimitHeld(t, env, id)
	if ms := reviewMembers(t, env, id); len(ms) != 0 {
		t.Fatalf("members of a held run = %+v; want none", ms)
	}

	if started, ok, err := env.Store.StartRun(env.ctx, env.MRKey(3), testProjectPath, head, "main"); err != nil || !ok || started != id {
		t.Fatalf("StartRun = %s, %v, %v", started, ok, err)
	}
	dispatch(t, env)
	ms := wantSplit(t, env, id, 3)
	if r := env.Run(id); r.ModelEntryID != store.ConfigModelID {
		t.Errorf("run entry = %q; want chain A's, past its admin limit", r.ModelEntryID)
	}
	if ms[0].State != store.MemberQueued {
		t.Errorf("reviewer 1 = %s; want queued", ms[0].State)
	}
	wantSkipped(t, ms[1], "the token quota")
	if ms[2].State != store.MemberQueued {
		t.Errorf("reviewer 3 = %s %q; want queued", ms[2].State, ms[2].FailureReason)
	}
}

// An image that does not have multi review (the floor unset, a tag below
// it, or a tag that is not a version) runs one review, with the reason.
func TestDispatchMultiReviewImageTooOld(t *testing.T) {
	for _, tc := range []struct{ name, floor, image string }{
		{"no floor", "", testImage},
		{"floor above the tag", "v0.6.0", testImage},
		{"tag not a version", "v0.5.0", "ghcr.io/gopaytech/pergent:main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			multiReviewFloor(t, tc.floor)
			env := newTestEnv(t)
			env.Engine.Config.Jobs.Image = tc.image
			b := globalRef(t, env, "gpt-b", "key-b")
			saveExtraReviewers(t, env, [][]store.EntryRef{{b}}, []store.EntryRef{b})
			id := dispatchMR(t, env, 3, sha('a'))
			wantSingle(t, env, id, "the review image does not support it")
		})
	}
}

// Without pergent.sh's own image the judge could not fetch the reviewers'
// results: one review, with the reason, and no member starts.
func TestDispatchMultiReviewNoFetchImage(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := newTestEnv(t)
	b := globalRef(t, env, "gpt-b", "key-b")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}}, []store.EntryRef{b})
	env.Engine.ScanImage = ""
	id := dispatchMR(t, env, 3, sha('a'))
	wantSingle(t, env, id, "pergent.sh's own image is not set")
}

// A run asked for under a used-up repository quota goes ahead, but no extra
// reviewer starts, on any entry: one review, for the quota.
func TestDispatchMultiReviewRepoQuota(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := quotaPushEnv(t, 100, 100)
	team := teamOwning(t, env, "Payments")
	b := addTeamModel(t, env, team, "team-b", "gpt-b", "key-b")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}}, []store.EntryRef{b})
	head := sha('a')
	servePush(env, head)
	id, _, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(3), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	wantSingle(t, env, id, "the token quota is used up")
}

// No extra reviewer can start (no key, or only chain A's model): one review,
// with that reason.
func TestDispatchMultiReviewNoneCanStart(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := newTestEnv(t)
	noKey := globalRef(t, env, "gpt-b", "")
	sameA := store.EntryRef{Source: store.SourceGlobal, ID: addModel(t, env, "anthropic", "claude-sonnet-4", "", "key-a2")}
	saveExtraReviewers(t, env, [][]store.EntryRef{{noKey}, {sameA}}, []store.EntryRef{sameA})
	id := dispatchMR(t, env, 3, sha('a'))
	wantSingle(t, env, id, "no other reviewer can start")
}

// Reviewers that can't be read never fail or hold the review: it runs as
// one, with the reason, and the error is logged.
func TestDispatchMultiReviewUnreadable(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := newTestEnv(t)
	b := globalRef(t, env, "gpt-b", "key-b")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}}, []store.EntryRef{b})
	execSQL(t, env, `ALTER TABLE reviewer_chains RENAME TO reviewer_chains_gone`)
	id := dispatchMR(t, env, 3, sha('a'))
	wantSingle(t, env, id, "the reviewers could not be read")
	if !strings.Contains(env.Logs(), "reviewers could not be read") {
		t.Errorf("the error was not logged:\n%s", env.Logs())
	}
}

// A judge chain whose entries were all removed does not stop the split: the
// judge copy has no entries (the judge runs with --no-judge).
func TestDispatchSplitEmptyJudge(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := newTestEnv(t)
	b := globalRef(t, env, "gpt-b", "key-b")
	j := globalRef(t, env, "gpt-j", "key-j")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}}, []store.EntryRef{j})
	execSQL(t, env, `DELETE FROM reviewer_chain_entries WHERE chain_id IN (SELECT id FROM reviewer_chains WHERE slot = 'judge')`)
	id := dispatchMR(t, env, 3, sha('a'))

	wantSplit(t, env, id, 2)
	judge, found, err := env.Store.JudgeChainOf(env.ctx, id)
	if err != nil || !found || len(judge.Entries) != 0 {
		t.Errorf("judge chain = %+v, %v, %v; want stored with no entries", judge, found, err)
	}
}

// A run split once and queued again with its dispatched_at is never split
// again: no new member and no off row. Its reviewers are live, so it goes
// back to wait for them (WaitForReviewMembers), keeping the split's
// dispatched_at.
func TestDispatchSplitOnce(t *testing.T) {
	multiReviewFloor(t, "v0.5.0")
	env := newTestEnv(t)
	b := globalRef(t, env, "gpt-b", "key-b")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}}, []store.EntryRef{b})
	id := dispatchMR(t, env, 3, sha('a'))
	before := wantSplit(t, env, id, 2)
	split := env.Run(id)

	startRunOld(t, env, id)
	dispatch(t, env)
	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitReviewers || !r.DispatchedAt.Equal(split.DispatchedAt) || r.JobName != "" {
		t.Fatalf("run = %s/%q dispatched %v job %q; want back to wait, dispatched at the split (%v)\n%s",
			r.State, r.SkipReason, r.DispatchedAt, r.JobName, split.DispatchedAt, env.Logs())
	}
	ms := reviewMembers(t, env, id)
	if len(ms) != len(before) {
		t.Fatalf("members = %+v; want the %d of the split", ms, len(before))
	}
	for i, m := range ms {
		if m.ID != before[i].ID || m.State == store.MemberOff {
			t.Errorf("member %d = %+v; want %+v", i, m, before[i])
		}
	}
}
