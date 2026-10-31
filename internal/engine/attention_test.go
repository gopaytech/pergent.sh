package engine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/store"
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
	plat, _ := env.Engine.Platforms.Get(ghInstance)
	approver := githubforge.NewTokenApprover(github.NewTokenClient(gh.srv.URL, "github_pat_example", gh.srv.Client()), ghApproveBot, GitHubRepos(env.Store, ghInstance), nil)
	plat.Approver, plat.GitHub.Approve = approver, approver
	env.Engine.Platforms.Set(ghInstance, plat)
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

// eventually waits for cond to hold.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The token-expiry recorder stores the instance-wide item with the date, and
// removes it when the expiry moves beyond 14 days.
func TestTokenExpiryAttention(t *testing.T) {
	env := newTestEnv(t)
	note := TokenExpiryRecorder(env.Store, ghInstance, nil)
	key := ghInstance + "||token_expiry"

	if err := note(time.Date(2026, 11, 5, 23, 0, 0, 0, time.UTC), true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the item", func() bool { return attention(t, env)[key] == "The approve bot's token expires on 2026-11-05." })
	if got := attention(t, env); len(got) != 1 {
		t.Fatalf("attention = %v", got)
	}
	if err := note(time.Date(2026, 12, 30, 0, 0, 0, 0, time.UTC), false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the item to go", func() bool { return len(attention(t, env)) == 0 })
}

// fakeAttentionStore records the writes it is given; the first block calls
// wait for release, the first fail calls fail.
type fakeAttentionStore struct {
	mu      sync.Mutex
	ops     []string
	started chan struct{}
	release chan struct{}
	fail    int
	blocked bool
}

func (f *fakeAttentionStore) write(op string) error {
	f.mu.Lock()
	first := !f.blocked && f.release != nil
	f.blocked = true
	f.mu.Unlock()
	if first {
		close(f.started)
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errors.New("store down")
	}
	f.ops = append(f.ops, op)
	return nil
}

func (f *fakeAttentionStore) SetAttention(_ context.Context, _, _, _, message string) error {
	return f.write("set " + message)
}

func (f *fakeAttentionStore) ClearAttention(_ context.Context, _, _, _ string) error {
	return f.write("clear")
}

func (f *fakeAttentionStore) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// A slow store doesn't hold up the approver's call, and updates that pile up
// behind a write are coalesced into the latest, in order.
func TestTokenExpiryRecorderDoesNotBlock(t *testing.T) {
	st := &fakeAttentionStore{started: make(chan struct{}), release: make(chan struct{})}
	note := TokenExpiryRecorder(st, ghInstance, nil)
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }

	done := make(chan struct{})
	go func() {
		defer close(done)
		note(day(1), true)
		<-st.started // the first write is stuck in the store
		note(day(2), true)
		note(day(3), true)
		note(day(30), false)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the callback waited for the store")
	}
	close(st.release)
	eventually(t, "the last state", func() bool { return len(st.Ops()) == 2 })
	if got := st.Ops(); got[0] != "set The approve bot's token expires on 2026-11-01." || got[1] != "clear" {
		t.Errorf("writes = %v; want the first, then only the last state", got)
	}
}

// A write that fails is tried again, not forgotten.
func TestTokenExpiryRecorderRetries(t *testing.T) {
	old := tokenExpiryRetry
	tokenExpiryRetry = 5 * time.Millisecond
	t.Cleanup(func() { tokenExpiryRetry = old })
	st := &fakeAttentionStore{fail: 2}
	note := TokenExpiryRecorder(st, ghInstance, nil)

	note(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), true)

	eventually(t, "the retried write", func() bool { return len(st.Ops()) == 1 })
}

// Switching a repository off removes its items, and only its own.
func TestAttentionClearedWhenRepoSwitchedOff(t *testing.T) {
	env := newTestEnv(t)
	serveProject(env, testProjectID, testProjectPath)
	env.SeedProject(func(p *store.Project) {
		p.ProjectID, p.Path, p.HookID, p.DeployTokenID = 8, "acme/old", 21, 22
	})
	env.GitLab.Reply(http.MethodDelete, "/projects/8/hooks/21", http.StatusNoContent, nil)
	env.GitLab.Reply(http.MethodDelete, "/projects/8/deploy_tokens/22", http.StatusNoContent, nil)
	for _, a := range [][3]string{
		{"acme/old", "single_approval", "x"}, {"Acme/Old", "out_of_memory", "x"},
		{testProjectPath, "single_approval", "x"}, {"", "token_expiry", "x"},
	} {
		if err := env.Store.SetAttention(env.ctx, testInstance, a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}

	reconcile(t, env)

	got := attention(t, env)
	if len(got) != 2 || got[testInstance+"|"+testProjectPath+"|single_approval"] == "" || got[testInstance+"||token_expiry"] == "" {
		t.Fatalf("attention = %v; want only the configured repository's and the instance's", got)
	}
}

// The same for a GitHub repository switched off.
func TestAttentionClearedWhenGitHubRepoSwitchedOff(t *testing.T) {
	env := newTestEnv(t)
	addGitHub(t, env, true)
	serveProject(env, testProjectID, testProjectPath)
	cfg := env.Engine.Config
	cfg.Repositories = cfg.Repositories[:1] // drop the GitHub repository
	if err := env.Store.SetAttention(env.ctx, ghInstance, ghRepoPath, "approver_unreachable", "x"); err != nil {
		t.Fatal(err)
	}

	reconcile(t, env)

	if got := attention(t, env); len(got) != 0 {
		t.Fatalf("attention = %v; want none", got)
	}
}

// With the approve side unable to reach the repository the approval check
// can't run, so its item goes.
func TestAttentionUnreachableClearsSingleApproval(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, false)
	serveProject(env, testProjectID, testProjectPath)
	gh.Reply(http.MethodGet, ghRepoAPI, http.StatusOK, map[string]any{"id": ghRepoID, "full_name": ghRepoPath, "clone_url": ghCloneURL})
	if err := env.Store.SetAttention(env.ctx, ghInstance, ghRepoPath, "single_approval", "x"); err != nil {
		t.Fatal(err)
	}

	reconcile(t, env)

	got := attention(t, env)
	if len(got) != 1 || got[ghInstance+"|"+ghRepoPath+"|approver_unreachable"] == "" {
		t.Fatalf("attention = %v; want only approver_unreachable", got)
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
