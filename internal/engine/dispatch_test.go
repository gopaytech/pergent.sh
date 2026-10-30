package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func dispatch(t *testing.T, env *testEnv) {
	t.Helper()
	if err := env.Engine.DispatchOnce(env.ctx); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
}

func hasArg(args []string, want string) bool { return slices.Contains(args, want) }

func TestDispatchLaunchesJob(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)

	dispatch(t, env)

	job := env.Job(id)
	if job == nil {
		t.Fatalf("no job %s; jobs: %v", kube.JobName(id), env.Jobs())
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	for _, want := range []string{
		"--base=" + sha('b'),
		"--head=" + head,
		"--result-url=" + testCallbackURL + "/runs/" + id + "/result",
		"--timeout=10m0s",
		"--concurrency=4",
		// GitLab takes notes up to 1,000,000 characters.
		"--max-comment-size=990000",
	} {
		if !hasArg(args, want) {
			t.Errorf("job args %v lack %q", args, want)
		}
	}
	if job.Spec.Suspend != nil && *job.Spec.Suspend {
		t.Errorf("job is still suspended")
	}
	if got := *job.Spec.ActiveDeadlineSeconds; got != int64((10*time.Minute+2*time.Minute+2*time.Minute)/time.Second) {
		t.Errorf("activeDeadlineSeconds = %d", got)
	}
	env1 := map[string]string{}
	for _, v := range job.Spec.Template.Spec.InitContainers[0].Env {
		env1[v.Name] = v.Value
	}
	if env1["REPO_URL"] != testRepoURL || env1["GIT_USERNAME"] != testDeployUser || env1["MR_REF"] != "refs/merge-requests/3/head" || env1["TARGET_BRANCH"] != "main" {
		t.Errorf("checkout env = %v", env1)
	}

	// The Secret holds the deploy token, the LLM key and a fresh result
	// token, and never a bot token.
	sec := env.Secret(id)
	if got := string(sec.Data["git-token"]); got != testDeployToken {
		t.Errorf("git-token = %q", got)
	}
	if got := string(sec.Data["llm-api-key"]); got != testLLMKey {
		t.Errorf("llm-api-key = %q", got)
	}
	resultToken := string(sec.Data["result-token"])
	decodeToken(t, resultToken)
	for k, v := range sec.Data {
		if bytes.Contains(v, []byte(testReviewToken)) || bytes.Contains(v, []byte(testApproveToken)) {
			t.Errorf("secret key %s holds a bot token", k)
		}
	}

	// The inputs were read at the MR's start SHA.
	cm := env.ConfigMap(id)
	if !strings.Contains(cm.Data["skill-0.md"], "Look for security problems.") {
		t.Errorf("configmap data = %v", cm.Data)
	}
	trees := env.GitLab.Requests(http.MethodGet, treePath())
	if len(trees) == 0 || trees[0].Query.Get("ref") != sha('5') {
		t.Errorf("tree requests = %+v, want ref %s", trees, sha('5'))
	}

	run := env.Run(id)
	if run.State != store.Running || run.JobName != kube.JobName(id) {
		t.Errorf("run = %+v", run)
	}
	if run.BaseSHA != sha('b') || run.StartSHA != sha('5') {
		t.Errorf("run base/start = %s/%s", run.BaseSHA, run.StartSHA)
	}
	wantExp := env.Now().Add(kube.Deadline(10*time.Minute, kube.CloneAllowance(0)) + time.Minute)
	if !run.TokenExpiresAt.Equal(wantExp) {
		t.Errorf("token expires %s, want %s", run.TokenExpiresAt, wantExp)
	}
	if !run.DispatchedAt.Equal(env.Now()) {
		t.Errorf("dispatched at %s", run.DispatchedAt)
	}

	// The running status went to the head with the review bot's token.
	statuses := env.GitLab.Requests(http.MethodPost, statusPath(head))
	if len(statuses) != 1 {
		t.Fatalf("status requests = %d", len(statuses))
	}
	st := statuses[0].JSON(t)
	if st["state"] != "running" || st["description"] != "pergent review running" || st["name"] != "pergent" {
		t.Errorf("status body = %v", st)
	}
	if statuses[0].Token != testReviewToken {
		t.Errorf("status set with token %q", statuses[0].Token)
	}

	// The stored hash is the token's: the Job's token is accepted once.
	out, err := env.Store.StoreResult(env.ctx, id, resultToken, []byte(`{"status":"completed"}`), env.Now().Add(time.Minute))
	if err != nil || out != store.ResultStored {
		t.Errorf("StoreResult with the Job's token = %v, %v", out, err)
	}
	if strings.Contains(env.Logs(), resultToken) || strings.Contains(env.Logs(), testDeployToken) {
		t.Errorf("a token reached the log:\n%s", env.Logs())
	}
}

// The merge request's title, web address, source branch and author are
// stored on the run for the dashboard, also when the run can't start.
func TestDispatchStoresChangeDetails(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	mr := openMR(3, head)
	mr.Title, mr.SourceBranch = "Add login", "feature/login"
	mr.WebURL = "https://gitlab.example.com/acme/widgets/-/merge_requests/3"
	mr.Author = gitlab.User{ID: 5, Username: "alice"}
	env.GitLab.SetMR(mr)
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)

	dispatch(t, env)

	run := env.Run(id)
	if run.Title != "Add login" || run.WebURL != mr.WebURL || run.SourceBranch != "feature/login" || run.Author != "alice" {
		t.Errorf("run details = %q %q %q %q", run.Title, run.WebURL, run.SourceBranch, run.Author)
	}
}

func TestDispatchUsesPreviousResult(t *testing.T) {
	env := newTestEnv(t)
	prev := []byte(`{"schema_version":1,"status":"completed","skills":[{"name":"security","verdict":"has_blocking_issues","findings":[{"id":"f1","title":"x"}],"files":{"reviewed":["a.go"]}}]}`)
	env.Publish(3, sha('1'), prev)
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)

	dispatch(t, env)

	got := env.ConfigMap(id).Data["previous.json"]
	var trimmed struct {
		Status string `json:"status"`
		Skills []struct {
			Name     string            `json:"name"`
			Findings []json.RawMessage `json:"findings"`
			Files    any               `json:"files"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(got), &trimmed); err != nil {
		t.Fatalf("previous.json %q: %v", got, err)
	}
	if trimmed.Status != "completed" || len(trimmed.Skills) != 1 || len(trimmed.Skills[0].Findings) != 1 || trimmed.Skills[0].Files != nil {
		t.Errorf("previous.json is not the trimmed previous result: %s", got)
	}
	if !hasArg(env.Job(id).Spec.Template.Spec.Containers[0].Args, "--previous=/in/previous.json") {
		t.Errorf("job lacks --previous")
	}
}

func TestDispatchRespectsMaxRunning(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Config.Jobs.MaxRunning = 2
	var ids []string
	for iid := int64(1); iid <= 3; iid++ {
		head := sha(byte('0' + iid))
		ids = append(ids, env.Enqueue(iid, head))
		env.GitLab.SetMR(openMR(iid, head))
		env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	}

	dispatch(t, env)
	if n := len(env.Jobs()); n != 2 {
		t.Fatalf("jobs after first dispatch = %d, want 2", n)
	}
	if r := env.Run(ids[2]); r.State != store.Queued {
		t.Errorf("third run state = %s, want queued", r.State)
	}

	dispatch(t, env)
	if n := len(env.Jobs()); n != 2 {
		t.Errorf("jobs after second dispatch = %d, want still 2", n)
	}
	if r := env.Run(ids[2]); r.State != store.Queued {
		t.Errorf("third run state = %s, want queued", r.State)
	}
}

func TestDispatchSupersedesMovedHead(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		env := newTestEnv(t)
		head := sha('a')
		id := env.Enqueue(3, head)
		mr := openMR(3, head)
		mr.State = "merged"
		env.GitLab.SetMR(mr)

		dispatch(t, env)

		if r := env.Run(id); r.State != store.Superseded {
			t.Errorf("state = %s, want superseded", r.State)
		}
		if len(env.Jobs()) != 0 {
			t.Errorf("a job was launched for a merged MR")
		}
		// No new run was queued for the merged MR.
		if _, created, _ := env.Store.EnqueueRun(env.ctx, env.MRKey(3), testProjectPath, head, "main"); !created {
			t.Errorf("a live run is left for the merged MR")
		}
	})

	t.Run("newer head", func(t *testing.T) {
		env := newTestEnv(t)
		old, newer := sha('a'), sha('c')
		id := env.Enqueue(3, old)
		mr := openMR(3, newer)
		mr.TargetBranch = "release"
		env.GitLab.SetMR(mr)

		dispatch(t, env)

		if r := env.Run(id); r.State != store.Superseded {
			t.Errorf("old run state = %s, want superseded", r.State)
		}
		if len(env.Jobs()) != 0 {
			t.Errorf("a job was launched for an old head")
		}
		newID, created, err := env.Store.EnqueueRun(env.ctx, env.MRKey(3), testProjectPath, newer, "release")
		if err != nil || created {
			t.Fatalf("no run was queued for the newer head (created=%v, %v)", created, err)
		}
		if r := env.Run(newID); r.State != store.Queued || r.TargetBranch != "release" || r.ProjectPath != testProjectPath {
			t.Errorf("new run = %+v", r)
		}
		select {
		case <-env.Engine.wakeCh():
		default:
			t.Errorf("engine was not woken for the new run")
		}

		// The next pass dispatches the newer head.
		env.GitLab.Reply(http.MethodPost, statusPath(newer), http.StatusCreated, `{}`)
		dispatch(t, env)
		if env.Job(newID) == nil {
			t.Errorf("newer head was not dispatched")
		}
	})
}

// A merge request retargeted after the run was queued (its webhook not seen
// yet) is reviewed against its current target, as a run of its own.
func TestDispatchSupersedesRetargetedRun(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	mr := openMR(3, head)
	mr.TargetBranch = "develop"
	env.GitLab.SetMR(mr)

	dispatch(t, env)

	if r := env.Run(id); r.State != store.Superseded {
		t.Errorf("run for the old target = %s, want superseded", r.State)
	}
	if len(env.Jobs()) != 0 {
		t.Errorf("a job was launched against the old target")
	}
	newID, created, err := env.Store.EnqueueRun(env.ctx, env.MRKey(3), testProjectPath, head, "develop")
	if err != nil || created {
		t.Fatalf("no run was queued for the new target (created=%v, %v)", created, err)
	}
	if r := env.Run(newID); r.State != store.Queued {
		t.Errorf("new run = %+v", r)
	}
}

func TestDispatchRequeuesWithoutBaseSHA(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	mr := openMR(3, head)
	mr.DiffRefs = gitlab.DiffRefs{}
	env.GitLab.SetMR(mr)

	dispatch(t, env)

	// Waiting for GitLab is not a failed attempt.
	r := env.Run(id)
	if r.State != store.Queued || r.Attempts != 0 || !r.NotBefore.Equal(env.Now().Add(10*time.Second)) {
		t.Fatalf("run = state %s attempts %d not_before %s, want queued 0 now+10s", r.State, r.Attempts, r.NotBefore)
	}
	if len(env.Jobs()) != 0 {
		t.Fatalf("a job was launched without diff refs")
	}

	// Not due yet: nothing happens.
	env.GitLab.Reset()
	dispatch(t, env)
	if n := len(env.GitLab.Requests(http.MethodGet, mrPath(3))); n != 0 {
		t.Errorf("run was retried before not_before (%d MR fetches)", n)
	}

	// Due, and GitLab has the diff refs now.
	env.SetNow(env.Now().Add(10 * time.Second))
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Errorf("run was not dispatched once the diff refs were there")
	}
}

func TestDispatchDiffNotReadyGivesUpAfterTenMinutes(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	mr := openMR(3, head)
	mr.DiffRefs = gitlab.DiffRefs{}
	env.GitLab.SetMR(mr)
	created := env.Run(id).CreatedAt

	// At exactly ten minutes it still waits.
	env.SetNow(created.Add(10 * time.Minute))
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Queued || r.Attempts != 0 {
		t.Fatalf("at 10m: state %s attempts %d, want queued 0", r.State, r.Attempts)
	}

	env.SetNow(created.Add(10*time.Minute + 11*time.Second))
	dispatch(t, env)
	r := env.Run(id)
	if r.State != store.Reported {
		t.Fatalf("past 10m: state = %s, want reported", r.State)
	}
	if want := "GitLab hasn't computed this merge request's diff yet"; r.FailureReason != want {
		t.Errorf("failure reason = %q, want %q", r.FailureReason, want)
	}
	if len(env.Jobs()) != 0 {
		t.Errorf("a job was launched")
	}
}

func TestDispatchDiffWaitsKeepRetryBudget(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	mr := openMR(3, head)
	mr.DiffRefs = gitlab.DiffRefs{}
	env.GitLab.SetMR(mr)

	for i := 0; i < 6; i++ {
		dispatch(t, env)
		r := env.Run(id)
		if r.State != store.Queued || r.Attempts != 0 {
			t.Fatalf("wait %d: state %s attempts %d", i, r.State, r.Attempts)
		}
		env.SetNow(r.NotBefore)
	}

	// Now GitLab fails: the run still gets all five attempts, backoff from 10 s.
	env.GitLab.Reply(http.MethodGet, mrPath(3), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	for attempt := 0; attempt < 4; attempt++ {
		dispatch(t, env)
		r := env.Run(id)
		wantNB := env.Now().Add(10 * time.Second << attempt)
		if r.State != store.Queued || r.Attempts != attempt+1 || !r.NotBefore.Equal(wantNB) {
			t.Fatalf("attempt %d: state %s attempts %d not_before %s, want queued %d %s",
				attempt, r.State, r.Attempts, r.NotBefore, attempt+1, wantNB)
		}
		env.SetNow(r.NotBefore)
	}
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Reported || !strings.HasPrefix(r.FailureReason, "pergent.sh couldn't start the review: ") {
		t.Errorf("after 5 attempts: state %s reason %q", r.State, r.FailureReason)
	}
}

func TestDispatchCancelledPostponesWithoutAttempt(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	// The service shuts down while GitLab is answering.
	env.GitLab.Handle(http.MethodGet, mrPath(3), func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(http.StatusForbidden)
	})

	if err := env.Engine.DispatchOnce(ctx); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}

	r := env.Run(id)
	if r.State != store.Queued || r.Attempts != 0 || r.FailureReason != "" || !r.NotBefore.Equal(env.Now()) {
		t.Errorf("run = state %s attempts %d reason %q not_before %s, want queued 0 \"\" now",
			r.State, r.Attempts, r.FailureReason, r.NotBefore)
	}
	if len(env.Jobs()) != 0 {
		t.Errorf("a job was launched")
	}
}

func TestDispatchRepoSkillErrorFailsRun(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodGet, treePath(), http.StatusOK, []gitlab.TreeEntry{
		{Name: "broken.md", Path: ".pergent/skills/broken.md", Type: "blob"},
	})
	env.GitLab.Reply(http.MethodGet, filePath(".pergent/skills/broken.md"), http.StatusOK, "---\nname: [unclosed\n---\nbody\n")

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Reported {
		t.Fatalf("state = %s, want reported", r.State)
	}
	if !strings.Contains(r.FailureReason, ".pergent/skills/broken.md") {
		t.Errorf("failure reason %q does not name the file", r.FailureReason)
	}
	if len(env.Jobs()) != 0 {
		t.Errorf("a job was launched")
	}
}

func TestDispatchTooLargeFailsRun(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Library = inputs.Library{"huge": bytes.Repeat([]byte("x"), inputs.ConfigMapLimit+1)}
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Reported {
		t.Fatalf("state = %s, want reported", r.State)
	}
	if want := "the skills and instructions for this repository are larger than 900 KiB"; r.FailureReason != want {
		t.Errorf("failure reason = %q, want %q", r.FailureReason, want)
	}
	if len(env.Jobs()) != 0 {
		t.Errorf("a job was launched")
	}
}

func TestDispatchMissingDeployTokenFailsRun(t *testing.T) {
	for name, edit := range map[string]func(p *store.Project){
		"no token":   func(p *store.Project) { p.DeployTokenSealed = nil },
		"no project": nil,
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			if edit != nil {
				env.SeedProject(edit)
			} else if err := env.Store.DeleteProject(env.ctx, testInstance, testProjectID); err != nil {
				t.Fatal(err)
			}
			head := sha('a')
			id := env.Enqueue(3, head)
			env.GitLab.SetMR(openMR(3, head))

			dispatch(t, env)

			r := env.Run(id)
			if r.State != store.Reported || r.FailureReason != "pergent.sh has no clone token for this project yet" {
				t.Errorf("run = state %s reason %q", r.State, r.FailureReason)
			}
			if len(env.Jobs()) != 0 {
				t.Errorf("a job was launched")
			}
		})
	}
}

func TestDispatchGitLabErrorRequeuesThenFails(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.Reply(http.MethodGet, mrPath(3), http.StatusForbidden, `{"message":"403 Forbidden"}`)

	for attempt := 0; attempt < 4; attempt++ {
		dispatch(t, env)
		r := env.Run(id)
		wantNB := env.Now().Add(10 * time.Second << attempt)
		if r.State != store.Queued || r.Attempts != attempt+1 || !r.NotBefore.Equal(wantNB) {
			t.Fatalf("attempt %d: state %s attempts %d not_before %s, want queued %d %s",
				attempt, r.State, r.Attempts, r.NotBefore, attempt+1, wantNB)
		}
		env.SetNow(r.NotBefore)
	}

	dispatch(t, env)
	r := env.Run(id)
	if r.State != store.Reported {
		t.Fatalf("after 5 attempts state = %s, want reported", r.State)
	}
	const prefix = "pergent.sh couldn't start the review: "
	if !strings.HasPrefix(r.FailureReason, prefix) || !strings.Contains(r.FailureReason, "403") {
		t.Errorf("failure reason = %q", r.FailureReason)
	}
	if strings.Contains(r.FailureReason, testReviewToken) {
		t.Errorf("failure reason holds the bot token")
	}
	if len(env.Jobs()) != 0 {
		t.Errorf("a job was launched")
	}
}

func TestDispatchKubernetesErrorRequeues(t *testing.T) {
	env := newTestEnv(t)
	env.Kube.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Queued || r.Attempts != 1 || !r.NotBefore.Equal(env.Now().Add(10*time.Second)) {
		t.Errorf("run = state %s attempts %d not_before %s", r.State, r.Attempts, r.NotBefore)
	}
	if len(env.Jobs()) != 0 {
		t.Errorf("the half-launched job was left behind")
	}
	if n := len(env.GitLab.Requests(http.MethodPost, statusPath(head))); n != 0 {
		t.Errorf("a running status was set for a run that did not start")
	}
}

func TestDispatchSupersededDuringLaunchDeletesJob(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	// A newer push lands while the Job is being started.
	env.Kube.PrependReactor("patch", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if err := env.Store.Supersede(env.ctx, id); err != nil {
			t.Errorf("Supersede: %v", err)
		}
		return false, nil, nil
	})

	dispatch(t, env)

	if env.Job(id) != nil {
		t.Errorf("the superseded run's job was not deleted")
	}
	if r := env.Run(id); r.State != store.Superseded {
		t.Errorf("state = %s, want superseded", r.State)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, statusPath(head))); n != 0 {
		t.Errorf("a running status was set for a superseded run")
	}
}

func TestDispatchStatusErrorKeepsRunDispatched(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusForbidden, `{"message":"403 Forbidden"}`)

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Running || r.JobName == "" || r.Attempts != 0 {
		t.Errorf("run = state %s job %q attempts %d", r.State, r.JobName, r.Attempts)
	}
	if env.Job(id) == nil {
		t.Errorf("job was removed")
	}
}

func TestDispatchOneFailureDoesNotStopOthers(t *testing.T) {
	env := newTestEnv(t)
	bad := env.Enqueue(3, sha('a'))
	good := env.Enqueue(4, sha('c'))
	env.GitLab.Reply(http.MethodGet, mrPath(3), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	env.GitLab.SetMR(openMR(4, sha('c')))
	env.GitLab.Reply(http.MethodPost, statusPath(sha('c')), http.StatusCreated, `{}`)

	dispatch(t, env)

	if r := env.Run(bad); r.State != store.Queued || r.Attempts != 1 {
		t.Errorf("failing run = state %s attempts %d", r.State, r.Attempts)
	}
	if env.Job(good) == nil {
		t.Errorf("the healthy run was not dispatched")
	}
}

func TestWakeDoesNotBlock(t *testing.T) {
	e := &Engine{}
	e.Wake()
	e.Wake()
	select {
	case <-e.wakeCh():
	default:
		t.Fatal("no wake-up pending")
	}
	select {
	case <-e.wakeCh():
		t.Fatal("two wake-ups pending, want one")
	default:
	}
}

func TestDispatchInputsGitLabErrorRequeues(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodGet, treePath(), http.StatusForbidden, `{"message":"403 Forbidden"}`)

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Queued || r.Attempts != 1 || r.FailureReason != "" {
		t.Errorf("run = state %s attempts %d reason %q, want requeued", r.State, r.Attempts, r.FailureReason)
	}
	if len(env.Jobs()) != 0 {
		t.Errorf("a job was launched")
	}
}

func TestDispatchUnconfiguredRepositoryIsSuperseded(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Config.Repositories = nil
	id := env.Enqueue(3, sha('a'))

	dispatch(t, env)

	if r := env.Run(id); r.State != store.Superseded {
		t.Errorf("state = %s, want superseded", r.State)
	}
	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("GitLab got %d requests for a switched-off repository", n)
	}
}

// A draft found at dispatch, while the repository doesn't review drafts (the
// webhook raced a setting change or a conversion to draft), is superseded:
// no Job, no status, no comment, and no run is queued for a newer head.
func TestDispatchDraftIsSupersededWhenDraftsAreOff(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name  string
		setup func(env *testEnv)
	}{
		{"config", func(env *testEnv) { env.Engine.Config.Repositories[0].ReviewDrafts = &off }},
		{"dashboard", func(env *testEnv) {
			env.Engine.Config.Repositories = nil
			if err := env.Store.UpsertDashboardRepo(env.ctx, store.DashboardRepo{
				Instance: testInstance, Project: testProjectPath, ReviewDrafts: &off, CreatedBy: "alice"}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, newerHead := range []bool{false, true} {
			name := tc.name
			if newerHead {
				name += "/newer head"
			}
			t.Run(name, func(t *testing.T) {
				env := newTestEnv(t)
				tc.setup(env)
				head := sha('a')
				id := env.Enqueue(3, head)
				current := head
				if newerHead {
					current = sha('c')
				}
				mr := openMR(3, current)
				mr.Draft = true
				env.GitLab.SetMR(mr)

				dispatch(t, env)

				if r := env.Run(id); r.State != store.Superseded {
					t.Errorf("state = %s, want superseded", r.State)
				}
				if len(env.Jobs()) != 0 {
					t.Errorf("a job was launched for a draft")
				}
				for _, req := range env.GitLab.Requests("", "") {
					if req.Method != http.MethodGet {
						t.Errorf("a draft got %s %s", req.Method, req.Path)
					}
				}
				if _, created, err := env.Store.EnqueueRun(env.ctx, env.MRKey(3), testProjectPath, current, "main"); err != nil || !created {
					t.Errorf("a live run is left for the draft (created=%v, %v)", created, err)
				}
			})
		}
	}
}

// With drafts on (the default) a draft is reviewed like any other merge request.
func TestDispatchDraftIsReviewedWhenDraftsAreOn(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	mr := openMR(3, head)
	mr.Draft = true
	env.GitLab.SetMR(mr)
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)

	dispatch(t, env)

	if env.Job(id) == nil {
		t.Fatalf("no job for a draft; run = %+v", env.Run(id))
	}
}

// The Job's timeout and memory limit come from the repository's effective
// settings; only the memory limit of the resources changes.
func TestDispatchUsesEffectiveTimeoutAndMemory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *testEnv)
	}{
		{"config", func(env *testEnv) {
			timeout, memory := 20*time.Minute, resource.MustParse("6Gi")
			env.Engine.Config.Repositories[0].JobTimeout = &timeout
			env.Engine.Config.Repositories[0].JobMemory = &memory
		}},
		{"dashboard", func(env *testEnv) {
			env.Engine.Config.Repositories = nil
			seconds, memory := 1200, "6Gi"
			if err := env.Store.UpsertDashboardRepo(env.ctx, store.DashboardRepo{
				Instance: testInstance, Project: testProjectPath, JobTimeoutSeconds: &seconds, JobMemory: &memory,
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.Engine.Config.Jobs.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
			}
			tc.setup(env)
			head := sha('a')
			id := env.Enqueue(3, head)
			env.GitLab.SetMR(openMR(3, head))
			env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)

			dispatch(t, env)

			job := env.Job(id)
			if job == nil {
				t.Fatalf("no job; run = %+v", env.Run(id))
			}
			if args := job.Spec.Template.Spec.Containers[0].Args; !hasArg(args, "--timeout=20m0s") {
				t.Errorf("job args %v lack --timeout=20m0s", args)
			}
			if got, want := *job.Spec.ActiveDeadlineSeconds, int64(kube.Deadline(20*time.Minute, kube.CloneAllowance(0))/time.Second); got != want {
				t.Errorf("activeDeadlineSeconds = %d, want %d", got, want)
			}
			wantExp := env.Now().Add(kube.Deadline(20*time.Minute, kube.CloneAllowance(0)) + time.Minute)
			if got := env.Run(id).TokenExpiresAt; !got.Equal(wantExp) {
				t.Errorf("token expires %s, want %s", got, wantExp)
			}
			res := job.Spec.Template.Spec.Containers[0].Resources
			for _, c := range []struct {
				list corev1.ResourceList
				name corev1.ResourceName
				want string
			}{
				{res.Limits, corev1.ResourceMemory, "6Gi"},
				{res.Limits, corev1.ResourceCPU, "2"},
				{res.Requests, corev1.ResourceMemory, "1Gi"},
				{res.Requests, corev1.ResourceCPU, "500m"},
			} {
				if got := c.list[c.name]; got.Cmp(resource.MustParse(c.want)) != 0 {
					t.Errorf("%s = %s, want %s", c.name, got.String(), c.want)
				}
			}
			// The config's own resources are untouched.
			if got := env.Engine.Config.Jobs.Resources.Limits[corev1.ResourceMemory]; got.Cmp(resource.MustParse("4Gi")) != 0 {
				t.Errorf("config memory limit changed to %s", got.String())
			}
		})
	}
}
