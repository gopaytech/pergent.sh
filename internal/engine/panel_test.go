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

	"github.com/gopaytech/pergent/rule"

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
