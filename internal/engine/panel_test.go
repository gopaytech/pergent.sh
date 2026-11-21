package engine

import (
	"encoding/json"
	"flag"
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
		QuietReviews: boolPtr(true), CheckDeleted: boolPtr(true), BuiltInRules: boolPtr(false),
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
}

// panelFloor sets the first pergent with a review panel for the test.
func panelFloor(t *testing.T, v string) {
	t.Helper()
	config.PanelMinVersion = v
	t.Cleanup(func() { config.PanelMinVersion = "" })
}

// saveChainB stores chain B c.
func saveChainB(t *testing.T, env *testEnv, c store.Chain) {
	t.Helper()
	if err := env.Store.SaveChainOf(env.ctx, store.ChainB, c, env.Now()); err != nil {
		t.Fatalf("SaveChainOf(ChainB): %v", err)
	}
}

// globalChainB stores a global chain B of one new global entry, gpt-b with
// key key-b, and returns its ref.
func globalChainB(t *testing.T, env *testEnv) store.EntryRef {
	t.Helper()
	b := globalRef(t, env, "gpt-b", "key-b")
	saveChainB(t, env, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{b}})
	return b
}

// members is the panel_members rows of run id's review.
func members(t *testing.T, env *testEnv, id string) []store.PanelMember {
	t.Helper()
	ms, err := env.Store.PanelMembers(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// wantOnlyA checks run id runs today's review Job, on A's key, with one off
// row of reason.
func wantOnlyA(t *testing.T, env *testEnv, id, reason string) {
	t.Helper()
	wantJobKey(t, env, id, testLLMKey)
	if r := env.Run(id); r.State != store.Running || r.JobName != kube.JobName(id) {
		t.Fatalf("run = %s job %q; want running with its Job", r.State, r.JobName)
	}
	ms := members(t, env, id)
	if len(ms) != 1 || ms[0].State != store.MemberOff || ms[0].FailureReason != reason {
		t.Fatalf("members = %+v; want one off row %q", ms, reason)
	}
	if strings.Contains(env.Logs(), "key-b") || strings.Contains(env.Logs(), testLLMKey) {
		t.Errorf("a key reached the log:\n%s", env.Logs())
	}
}

// With no chain B in effect, even with an image that has the panel, the
// dispatch is today's byte for byte, and no panel row is written.
func TestDispatchNoChainBUnchanged(t *testing.T) {
	panelFloor(t, "v0.5.0")
	env, id := goldenEnv(t, nil)
	dispatch(t, env)
	checkDispatchGolden(t, env, id)
	if ms := members(t, env, id); len(ms) != 0 {
		t.Errorf("members = %+v; want none", ms)
	}
	if r := env.Run(id); r.State != store.Running || r.JobName != kube.JobName(id) {
		t.Errorf("run = %s job %q; want running with its Job", r.State, r.JobName)
	}
}

// A repository that turned chain B off is reviewed as today, whatever its
// team has: the same dispatch byte for byte, and no row.
func TestDispatchRepoOffNoPanel(t *testing.T) {
	panelFloor(t, "v0.5.0")
	env, id := goldenEnv(t, func(env *testEnv) {
		team := teamOwning(t, env, "Payments")
		b := addTeamModel(t, env, team, "team-b", "gpt-b", "key-b")
		saveChainB(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{b}})
		if err := env.Store.SetRepoPanelOff(env.ctx, testInstance, testProjectPath, env.Now()); err != nil {
			t.Fatal(err)
		}
	})
	dispatch(t, env)
	checkDispatchGolden(t, env, id)
	if ms := members(t, env, id); len(ms) != 0 {
		t.Errorf("members = %+v; want none", ms)
	}
}

// A review with chain B, on an image that has the panel, is split: the run
// waits for its members with dispatched_at set and member A's entry kept;
// both members are queued; no Job, ConfigMap, Secret or status yet.
func TestDispatchSplitsPanel(t *testing.T) {
	panelFloor(t, "v0.5.0")
	env := newTestEnv(t)
	globalChainB(t, env)
	id := dispatchMR(t, env, 3, sha('a'))

	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitPanel {
		t.Fatalf("run = %s/%q; want waiting/%s\n%s", r.State, r.SkipReason, store.WaitPanel, env.Logs())
	}
	if !r.DispatchedAt.Equal(env.Now()) || r.JobName != "" {
		t.Errorf("dispatched at %s, job %q; want now and no Job", r.DispatchedAt, r.JobName)
	}
	if r.ModelEntryID != "config" || r.Provider == nil || *r.Provider != "anthropic" {
		t.Errorf("run entry = %q provider %v; want A's", r.ModelEntryID, r.Provider)
	}
	ms := members(t, env, id)
	if len(ms) != 2 || ms[0].State != store.MemberQueued || ms[1].State != store.MemberQueued ||
		ms[0].Role == ms[1].Role || ms[0].RunID != id {
		t.Fatalf("members = %+v; want 2 queued, a and b", ms)
	}
	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Errorf("%d Jobs were created", len(jobs))
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
	if strings.Contains(env.Logs(), "key-b") || strings.Contains(env.Logs(), testLLMKey) {
		t.Errorf("a key reached the log:\n%s", env.Logs())
	}
}

// An image that does not have the panel (the floor unset, a tag below it,
// or a tag that is not a version) runs A only, with the note.
func TestDispatchPanelImageTooOld(t *testing.T) {
	for _, tc := range []struct{ name, floor, image string }{
		{"no floor", "", testImage},
		{"floor above the tag", "v0.6.0", testImage},
		{"tag not a version", "v0.5.0", "ghcr.io/gopaytech/pergent:main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			panelFloor(t, tc.floor)
			env := newTestEnv(t)
			env.Engine.Config.Jobs.Image = tc.image
			globalChainB(t, env)
			id := dispatchMR(t, env, 3, sha('a'))
			wantOnlyA(t, env, id, "the review image does not support it")
		})
	}
}

// A run asked for under a used-up instance quota goes ahead on A's global
// entry, but B, on global entries only, does not start.
func TestDispatchPanelQuotaStopsB(t *testing.T) {
	panelFloor(t, "v0.5.0")
	env := quotaPushEnv(t, 0, 0)
	instQuota := int64(500)
	env.Engine.Config.GitLab[0].MonthlyTokenQuota = &instQuota
	seedQuotaUsage(t, env, 500)
	globalChainB(t, env)
	head := sha('a')
	servePush(env, head)
	id, _, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(3), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	wantOnlyA(t, env, id, "the token quota stopped model B")
}

// A run asked for under a used-up repository quota goes ahead, but B, which
// would spend as much again, does not start, on any entry.
func TestDispatchPanelRepoQuotaStopsB(t *testing.T) {
	panelFloor(t, "v0.5.0")
	env := quotaPushEnv(t, 100, 100)
	team := teamOwning(t, env, "Payments")
	b := addTeamModel(t, env, team, "team-b", "gpt-b", "key-b")
	saveChainB(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{b}})
	head := sha('a')
	servePush(env, head)
	id, _, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(3), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	wantOnlyA(t, env, id, "the token quota stopped model B")
}

// Chain B's only entry runs on the model A runs on (another key, another
// case): A only. With a different entry after it, B takes that one.
func TestDispatchPanelSameModel(t *testing.T) {
	t.Run("only entry", func(t *testing.T) {
		panelFloor(t, "v0.5.0")
		env := newTestEnv(t)
		same := store.EntryRef{Source: store.SourceGlobal, ID: addModel(t, env, "Anthropic", "Claude-Sonnet-4", "", "key-b")}
		saveChainB(t, env, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{same}})
		id := dispatchMR(t, env, 3, sha('a'))
		wantOnlyA(t, env, id, "model B is the model A runs on")
	})
	t.Run("another entry left", func(t *testing.T) {
		panelFloor(t, "v0.5.0")
		env := newTestEnv(t)
		same := store.EntryRef{Source: store.SourceGlobal, ID: addModel(t, env, "anthropic", "claude-sonnet-4", "", "key-same")}
		other := globalRef(t, env, "gpt-b", "key-b")
		saveChainB(t, env, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{same, other}})
		id := dispatchMR(t, env, 3, sha('a'))
		if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitPanel {
			t.Fatalf("run = %s/%q; want a panel\n%s", r.State, r.SkipReason, env.Logs())
		}
	})
}

// No entry of chain B can be used (no key): A only, with that reason.
func TestDispatchPanelNoUsableB(t *testing.T) {
	panelFloor(t, "v0.5.0")
	env := newTestEnv(t)
	gone := store.EntryRef{Source: store.SourceGlobal, ID: "no-such-entry"}
	saveChainB(t, env, store.Chain{Scope: store.ScopeGlobal, Entries: []store.EntryRef{gone}})
	id := dispatchMR(t, env, 3, sha('a'))
	wantOnlyA(t, env, id, "no entry of chain B can be used")
}

// Chain B that can't be read never fails or holds the review: it runs A
// only, with the reason, and the error is logged.
func TestDispatchPanelChainBUnreadable(t *testing.T) {
	panelFloor(t, "v0.5.0")
	env := newTestEnv(t)
	globalChainB(t, env)
	execSQL(t, env, `ALTER TABLE panel_chains RENAME TO panel_chains_gone`)
	id := dispatchMR(t, env, 3, sha('a'))
	wantOnlyA(t, env, id, "chain B could not be read")
	if !strings.Contains(env.Logs(), "chain B") {
		t.Errorf("the error was not logged:\n%s", env.Logs())
	}
}

// Why B can't run: the quota when only it and A's model stopped the
// entries, A's model when only it did, else no usable entry.
func TestPanelOffReason(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reasons []string
		want    string
	}{
		{"no entry", nil, panelOffNone},
		{"quota only", []string{chain.ReasonInstanceQuota, chain.ReasonInstanceQuota}, panelOffQuota},
		{"same model only", []string{chain.ReasonSameModel}, panelOffSame},
		{"same model and quota", []string{chain.ReasonSameModel, chain.ReasonInstanceQuota}, panelOffQuota},
		{"quota and same model", []string{chain.ReasonInstanceQuota, chain.ReasonSameModel}, panelOffQuota},
		{"same model and a removed entry", []string{chain.ReasonSameModel, chain.ReasonRemoved}, panelOffNone},
		{"quota and a mark", []string{chain.ReasonInstanceQuota, store.ClassQuota}, panelOffNone},
		{"no key", []string{chain.ReasonKeyUnusable}, panelOffNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dec chain.Decision
			for _, r := range tc.reasons {
				dec.Skips = append(dec.Skips, chain.Skip{Reason: r})
			}
			if got := panelOffReason(dec); got != tc.want {
				t.Errorf("panelOffReason(%v) = %q, want %q", tc.reasons, got, tc.want)
			}
		})
	}
}

// A run released once its members are done keeps its dispatched_at: its next
// dispatch never splits it again or writes an off row, and it is not left
// claimed with no Job (it gets today's Job until Job P, S3.8).
func TestDispatchReleasedPanelNotSplitAgain(t *testing.T) {
	panelFloor(t, "v0.5.0")
	env := newTestEnv(t)
	globalChainB(t, env)
	head := sha('a')
	id := dispatchMR(t, env, 3, head)
	dispatch(t, env)
	for _, m := range members(t, env, id) {
		token := string(env.Secret(m.ID).Data["result-token"])
		if out, err := env.Store.StoreMemberResult(env.ctx, m.ID, token, resultFor(t, head, reviewed("no_blocking_issues", "a.go")), env.Now()); err != nil || out != store.ResultStored {
			t.Fatalf("StoreMemberResult = %v, %v", out, err)
		}
		if st, err := env.Store.SettleMember(env.ctx, m.ID); err != nil || st != store.MemberDone {
			t.Fatalf("SettleMember = %q, %v", st, err)
		}
	}
	if released, _, err := env.Store.ReleasePanel(env.ctx, id, env.Now()); err != nil || !released {
		t.Fatalf("ReleasePanel = %v, %v", released, err)
	}

	dispatch(t, env)
	if r := env.Run(id); r.State != store.Running || r.JobName != kube.JobName(id) {
		t.Errorf("run = %s job %q; want running with a Job\n%s", r.State, r.JobName, env.Logs())
	}
	ms := members(t, env, id)
	if len(ms) != 2 || ms[0].State != store.MemberDone || ms[1].State != store.MemberDone {
		t.Errorf("members = %+v; want the 2 done ones and no off row", ms)
	}
}
