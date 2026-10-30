package engine

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/github"
)

const (
	wantSingleApproval = "One approval is enough to merge here, so pergent's approval alone can unlock it. Add an approval rule that needs a human."
	wantAppUnreachable = "The pergent approve App isn't installed on this repository, so nothing is approved."
	wantBotUnreachable = "The pergent approve bot can't reach this repository, so nothing is approved."
)

// attention returns the recorded setup problems as "instance|project|kind" ->
// message.
func attention(t *testing.T, env *testEnv) map[string]string {
	t.Helper()
	rows, err := env.Store.Attention(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Instance+"|"+r.Project+"|"+r.Kind] = r.Message
	}
	return out
}

// serveGitLabApprovals routes a GitLab project with a default branch and one
// approval rule requiring required approvals.
func serveGitLabApprovals(env *testEnv, required int) {
	serveProject(env, testProjectID, testProjectPath)
	env.GitLab.Reply(http.MethodGet, "/projects/acme%2Fwidgets", http.StatusOK, map[string]any{
		"id": testProjectID, "path_with_namespace": testProjectPath, "default_branch": "main",
		"http_url_to_repo": "https://gitlab.example.com/acme/widgets.git",
	})
	env.GitLab.Reply(http.MethodGet, "/projects/7/approval_rules", http.StatusOK, []any{map[string]any{
		"rule_type": "any_approver", "approvals_required": required,
		"applies_to_all_protected_branches": false, "protected_branches": []any{}}})
}

// Reconcile records single_approval where it warns, and removes it once two
// approvals are needed. A read that fails leaves the item as it was.
func TestAttentionSingleApprovalGitLab(t *testing.T) {
	env := newTestEnv(t)
	key := testInstance + "|" + testProjectPath + "|single_approval"

	serveGitLabApprovals(env, 1)
	reconcile(t, env)
	if got := attention(t, env); got[key] != wantSingleApproval || len(got) != 1 {
		t.Fatalf("attention = %v; want only single_approval", got)
	}

	env.GitLab.Reply(http.MethodGet, "/projects/7/approval_rules", http.StatusForbidden, `{"message":"403 Forbidden"}`)
	reconcile(t, env)
	if got := attention(t, env); got[key] != wantSingleApproval {
		t.Fatalf("attention after a failed read = %v; want it unchanged", got)
	}

	serveGitLabApprovals(env, 2)
	reconcile(t, env)
	if got := attention(t, env); len(got) != 0 {
		t.Fatalf("attention after two approvals are needed = %v; want none", got)
	}
}

func TestAttentionSingleApprovalGitHub(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	serveProject(env, testProjectID, testProjectPath)
	gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, map[string]any{
		"id": ghRepoID, "full_name": ghRepoPath, "clone_url": ghCloneURL, "default_branch": "main"})
	rules := func(n int) {
		gh.Reply(http.MethodGet, ghRepoAPI+"/rules/branches/main", http.StatusOK, []any{
			map[string]any{"type": "pull_request", "parameters": map[string]any{"required_approving_review_count": n}}})
		gh.Reply(http.MethodGet, ghRepoAPI+"/branches/main/protection/required_pull_request_reviews", http.StatusForbidden,
			map[string]any{"message": "Resource not accessible by integration"})
	}
	key := ghInstance + "|" + ghRepoPath + "|single_approval"

	rules(1)
	reconcile(t, env)
	if got := attention(t, env); got[key] != wantSingleApproval {
		t.Fatalf("attention = %v; want single_approval", got)
	}
	rules(2)
	reconcile(t, env)
	if got := attention(t, env); len(got) != 0 {
		t.Fatalf("attention = %v; want none", got)
	}
}

// An approve App that isn't installed is recorded, and removed once it is.
func TestAttentionApproverUnreachableApp(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, false)
	serveProject(env, testProjectID, testProjectPath)
	gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, map[string]any{"id": ghRepoID, "full_name": ghRepoPath, "clone_url": ghCloneURL})
	key := ghInstance + "|" + ghRepoPath + "|approver_unreachable"

	reconcile(t, env)
	if got := attention(t, env); got[key] != wantAppUnreachable || len(got) != 1 {
		t.Fatalf("attention = %v; want only approver_unreachable (App)", got)
	}

	gh.Install(ghApproveApp, ghRepoPath, ghApproveInstallation)
	reconcile(t, env)
	if got := attention(t, env); len(got) != 0 {
		t.Fatalf("attention after installing the App = %v; want none", got)
	}
}

// An instance on the approve token says "bot", not "App".
func TestAttentionApproverUnreachableToken(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, false)
	cfg := env.Engine.Config
	cfg.GitHub[0].ApproveApp, cfg.GitHub[0].ApproveTokenEnv = nil, "GITHUB_APPROVE_TOKEN"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	plat := env.Engine.Platforms[ghInstance]
	approver := githubforge.NewTokenApprover(github.NewTokenClient(gh.srv.URL, "github_pat_example", gh.srv.Client()), ghApproveBot, GitHubRepos(env.Store, ghInstance), nil)
	plat.Approver, plat.GitHub.Approve = approver, approver
	env.Engine.Platforms[ghInstance] = plat
	serveProject(env, testProjectID, testProjectPath)
	gh.Handle(http.MethodGet, ghRepoAPI, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "github_pat_example") {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": ghRepoID, "full_name": ghRepoPath, "clone_url": ghCloneURL})
	})

	reconcile(t, env)
	key := ghInstance + "|" + ghRepoPath + "|approver_unreachable"
	if got := attention(t, env); got[key] != wantBotUnreachable {
		t.Fatalf("attention = %v; want approver_unreachable (bot)", got)
	}
}

func oomRun(t *testing.T, env *testEnv, iid int64) {
	t.Helper()
	id := env.Dispatched(iid, sha(byte('a'+iid)))
	now := env.Now()
	putJob(t, env, id, now.Add(-5*time.Minute), nil,
		[]corev1.ContainerStatus{terminatedStatus("pergent", 137, "OOMKilled", now.Add(-4*time.Minute), now.Add(-2*time.Minute))},
		jobCond(batchv1.JobFailed, "BackoffLimitExceeded", now.Add(-2*time.Minute)))
}

// The watcher records out_of_memory from the second OOMKilled failure in 7
// days (with the count) and reconcile removes it once the failures age out.
func TestAttentionOutOfMemory(t *testing.T) {
	env := newTestEnv(t)
	key := testInstance + "|" + testProjectPath + "|out_of_memory"

	oomRun(t, env, 1)
	watch(t, env)
	if got := attention(t, env); len(got) != 0 {
		t.Fatalf("attention after one OOM = %v; want none", got)
	}

	oomRun(t, env, 2)
	watch(t, env)
	want := "Reviews ran out of memory 2 times in the last 7 days. Raise the Job memory limit."
	if got := attention(t, env); got[key] != want || len(got) != 1 {
		t.Fatalf("attention after two OOMs = %v; want %q", got, want)
	}

	oomRun(t, env, 3)
	watch(t, env)
	if got := attention(t, env); got[key] != strings.Replace(want, "2 times", "3 times", 1) {
		t.Fatalf("attention after three OOMs = %v", got)
	}

	// A week and a day on, the failures are outside the window.
	serveProject(env, testProjectID, testProjectPath)
	env.SetNow(env.Now().Add(8 * 24 * time.Hour))
	reconcile(t, env)
	if got := attention(t, env); len(got) != 0 {
		t.Fatalf("attention after the failures aged out = %v; want none", got)
	}
}

// Other failures don't count.
func TestAttentionOutOfMemoryIgnoresOtherFailures(t *testing.T) {
	env := newTestEnv(t)
	for iid := int64(1); iid <= 3; iid++ {
		id := env.Dispatched(iid, sha(byte('a'+iid)))
		now := env.Now()
		putJob(t, env, id, now.Add(-5*time.Minute), nil, nil, jobCond(batchv1.JobFailed, "DeadlineExceeded", now.Add(-2*time.Minute)))
	}
	watch(t, env)
	if got := attention(t, env); len(got) != 0 {
		t.Fatalf("attention = %v; want none", got)
	}
}

// The token-expiry recorder stores the instance-wide item with the date, and
// removes it when the expiry moves beyond 14 days.
func TestTokenExpiryAttention(t *testing.T) {
	env := newTestEnv(t)
	note := TokenExpiryRecorder(env.Store, ghInstance, nil)
	key := ghInstance + "||token_expiry"

	note(time.Date(2026, 11, 5, 23, 0, 0, 0, time.UTC), true)
	if got := attention(t, env); got[key] != "The approve bot's token expires on 2026-11-05." || len(got) != 1 {
		t.Fatalf("attention = %v", got)
	}
	note(time.Date(2026, 12, 30, 0, 0, 0, 0, time.UTC), false)
	if got := attention(t, env); len(got) != 0 {
		t.Fatalf("attention after rotating = %v; want none", got)
	}
}

// A store that fails costs a line at info and nothing else.
func TestAttentionStoreErrorIsBestEffort(t *testing.T) {
	env := newTestEnv(t)
	dead, cancel := context.WithCancel(context.Background())
	cancel() // every store call under it fails

	env.Engine.setAttention(dead, testInstance, testProjectPath, "single_approval", "x")
	env.Engine.clearAttention(dead, testInstance, testProjectPath, "single_approval")

	logs := env.Logs()
	if n := strings.Count(logs, "couldn't"); n != 2 {
		t.Errorf("logged %d failures, want 2:\n%s", n, logs)
	}
	if strings.Contains(logs, "level=WARN") || strings.Contains(logs, "level=ERROR") {
		t.Errorf("more than info:\n%s", logs)
	}
}
