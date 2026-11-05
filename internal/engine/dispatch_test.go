package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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
	env.AddSkill("huge", string(bytes.Repeat([]byte("x"), inputs.ConfigMapLimit+1)))
	env.SetDefaultSkills("huge")
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
			if r.State != store.Reported || r.FailureReason != "pergent.sh has no clone token for this repository yet" {
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

				if r := env.Run(id); r.State != store.Superseded || r.OutcomeReason != store.SupersededDraft {
					t.Errorf("run = %s %q, want superseded as a draft", r.State, r.OutcomeReason)
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

// The Job's timeout, step limit and memory limit come from the repository's
// effective settings; only the memory limit of the resources changes.
func TestDispatchUsesEffectiveTimeoutTurnsAndMemory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *testEnv)
	}{
		{"config", func(env *testEnv) {
			timeout, memory, turns := 20*time.Minute, resource.MustParse("6Gi"), 120
			env.Engine.Config.Repositories[0].JobTimeout = &timeout
			env.Engine.Config.Repositories[0].MaxTurns = &turns
			env.Engine.Config.Repositories[0].JobMemory = &memory
		}},
		{"dashboard", func(env *testEnv) {
			env.Engine.Config.Repositories = nil
			seconds, memory, turns := 1200, "6Gi", 120
			if err := env.Store.UpsertDashboardRepo(env.ctx, store.DashboardRepo{
				Instance: testInstance, Project: testProjectPath, JobTimeoutSeconds: &seconds, MaxTurns: &turns, JobMemory: &memory,
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
			if !hasArg(job.Spec.Template.Spec.Containers[0].Args, "--max-turns=120") {
				t.Errorf("job args %v lack --max-turns=120", job.Spec.Template.Spec.Containers[0].Args)
			}
			if got := env.Run(id).MaxTurns; got == nil || *got != 120 {
				t.Errorf("recorded max turns = %v, want 120", got)
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

// reviewedFiles are the files the published reviews of the push tests reviewed.
var reviewedFiles = []string{"a.go", "b.go", "c.go"}

// publishedReview dispatches a push run of head on merge request 3 and
// publishes it with a completed result of the security skill that reviewed
// reviewedFiles, made by pergent version. Dispatching records the skill's
// hash, as every review does. edit changes the result before it is stored.
func publishedReview(t *testing.T, env *testEnv, head, version string, edit func(res map[string]any)) string {
	t.Helper()
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("the last review was not dispatched; run = %+v\n%s", env.Run(id), env.Logs())
	}
	if env.Run(id).SkillHashes["security"] == "" {
		t.Fatalf("no hash of the security skill was recorded: %v", env.Run(id).SkillHashes)
	}
	res := map[string]any{
		"schema_version": 1, "status": "completed", "pergent_version": version, "base": sha('b'), "head": head,
		"skills": []any{map[string]any{
			"name": "security", "verdict": "no_blocking_issues", "findings": []any{},
			"files": map[string]any{"reviewed": reviewedFiles, "skipped": []any{}, "not_reviewed": []any{}},
		}},
		"comment": map[string]any{"markdown": testMarkdown, "findings_block": "<!-- pergent:findings [] -->"},
	}
	if edit != nil {
		edit(res)
	}
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	token := string(env.Secret(id).Data["result-token"])
	if out, err := env.Store.StoreResult(env.ctx, id, token, body, env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := env.Store.MarkPublished(env.ctx, id, store.RunDecision{}); err != nil {
		t.Fatal(err)
	}
	return id
}

func mergeBaseRoute() string { return projectPath() + "/repository/merge_base" }

// serveChangedSince makes every comparison of two commits succeed: the first
// is an ancestor of the second, and changed are the files between them.
func serveChangedSince(env *testEnv, changed ...string) {
	env.GitLab.Handle(http.MethodGet, mergeBaseRoute(), func(w http.ResponseWriter, r *http.Request) {
		refs := r.URL.Query()["refs[]"]
		if len(refs) != 2 {
			http.Error(w, `{"message":"two refs"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": refs[0]})
	})
	var files []fileDiff
	for _, p := range changed {
		files = append(files, fileDiff{OldPath: p, NewPath: p, Diff: "@@ -1 +1 @@\n-a\n+b\n"})
	}
	env.GitLab.ServeCompare(files...)
}

// fullReviews returns the reasons the log gives for full reviews since mark.
func fullReviews(env *testEnv, mark int) []string {
	var out []string
	for _, line := range strings.Split(env.Logs()[mark:], "\n") {
		if !strings.Contains(line, "msg=\"full review\"") {
			continue
		}
		_, reason, _ := strings.Cut(line, " reason=")
		out = append(out, reason)
	}
	return out
}

// assertFull checks the Job of run id was not given carried files, the run is
// not incremental and has no base run.
func assertFull(t *testing.T, env *testEnv, id string) {
	t.Helper()
	job := env.Job(id)
	if job == nil {
		t.Fatalf("no job; run = %+v\n%s", env.Run(id), env.Logs())
	}
	for _, a := range job.Spec.Template.Spec.Containers[0].Args {
		if strings.HasPrefix(a, "--carried") {
			t.Errorf("a full review got %q", a)
		}
	}
	if _, ok := env.ConfigMap(id).Data["carried.json"]; ok {
		t.Errorf("a full review got carried.json")
	}
	if r := env.Run(id); r.Incremental || r.BaseRunID != nil {
		t.Errorf("run incremental = %v, base = %v; want a full review", r.Incremental, r.BaseRunID)
	}
}

// A push after a published review of the change reviews only what changed
// since, or all of it, and says why when it does.
func TestPushDispatch(t *testing.T) {
	headA, headB := sha('1'), sha('2')
	statusOK := func(env *testEnv, head string) {
		env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	}

	t.Run("incremental", func(t *testing.T) {
		env := newTestEnv(t)
		last := publishedReview(t, env, headA, "0.5.0", nil)
		id := env.Enqueue(3, headB)
		env.GitLab.SetMR(openMR(3, headB))
		statusOK(env, headB)
		serveChangedSince(env, "b.go", "d.go")
		mark := len(env.Logs())

		dispatch(t, env)

		job := env.Job(id)
		if job == nil {
			t.Fatalf("no job; run = %+v\n%s", env.Run(id), env.Logs())
		}
		if !hasArg(job.Spec.Template.Spec.Containers[0].Args, "--carried=/in/carried.json") {
			t.Errorf("job args %v lack --carried", job.Spec.Template.Spec.Containers[0].Args)
		}
		if got, want := env.ConfigMap(id).Data["carried.json"], `{"security":["a.go","c.go"]}`; got != want {
			t.Errorf("carried.json = %s, want %s", got, want)
		}
		if r := env.Run(id); !r.Incremental || r.BaseRunID == nil || *r.BaseRunID != last {
			t.Errorf("run incremental = %v, base = %v; want an incremental review on %s", r.Incremental, r.BaseRunID, last)
		}
		if got := fullReviews(env, mark); len(got) != 0 {
			t.Errorf("an incremental review logged a full review: %v", got)
		}
		// The compared commits are the last review's head and this one's.
		reqs := env.GitLab.Requests(http.MethodGet, mergeBaseRoute())
		if len(reqs) != 1 || !slices.Equal(reqs[0].Query["refs[]"], []string{headA, headB}) {
			t.Errorf("merge base requests = %+v", reqs)
		}
	})

	for _, tc := range []struct {
		name string
		// reason is what the log says, or "" when the platform's own error is.
		reason string
		// version is the pergent version of the last review.
		version string
		// noLast leaves the change without a published review.
		noLast bool
		// prepare runs before the last review, change after it.
		prepare func(env *testEnv)
		edit    func(res map[string]any)
		change  func(env *testEnv)
		target  string
		changed []string
	}{
		{name: "setting off", reason: "setting off", change: func(env *testEnv) {
			off := false
			env.Engine.Config.Repositories[0].IncrementalReviews = &off
		}},
		{name: "no earlier review", reason: "no earlier review", noLast: true},
		{name: "target branch changed", reason: "target branch changed", target: "develop"},
		{name: "the platform can't compare", change: func(env *testEnv) {
			env.GitLab.Reply(http.MethodGet, mergeBaseRoute(), http.StatusBadRequest, `{"message":"bad"}`)
		}},
		{name: "older pergent", reason: "older pergent", version: "0.4.0"},
		{name: "jobs image pinned to an older pergent", reason: "older pergent", change: func(env *testEnv) {
			env.Engine.Config.Jobs.Image = "ghcr.io/gopaytech/pergent:v0.4.0"
		}},
		{name: "partial last review", reason: "the last review is partial", edit: func(res map[string]any) {
			res["merge_note"] = "the earlier results could not be kept"
		}},
		{name: "nothing to carry", reason: "nothing to carry", changed: reviewedFiles},
		{name: "skill changed since the last review", reason: "nothing to carry", change: func(env *testEnv) {
			if _, err := env.Store.UpdateLibrarySkill(env.ctx, "security", "---\nname: security\n---\nLook again.\n", "test", 1, env.Now()); err != nil {
				env.t.Fatal(err)
			}
		}},
		{name: "inputs too large", reason: "inputs too large", prepare: func(env *testEnv) {
			// The skill alone fits; the previous result and the carried
			// files on top of it do not.
			const head = "---\nname: security\n---\n"
			big := head + strings.Repeat("x", inputs.ConfigMapLimit-len(head)-5)
			if _, err := env.Store.UpdateLibrarySkill(env.ctx, "security", big, "test", 1, env.Now()); err != nil {
				env.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			if tc.prepare != nil {
				tc.prepare(env)
			}
			if !tc.noLast {
				version := tc.version
				if version == "" {
					version = "0.5.0"
				}
				publishedReview(t, env, headA, version, tc.edit)
			}
			target := "main"
			if tc.target != "" {
				target = tc.target
			}
			id, _, err := env.Store.EnqueueRun(env.ctx, env.MRKey(3), testProjectPath, headB, target)
			if err != nil {
				t.Fatal(err)
			}
			mr := openMR(3, headB)
			mr.TargetBranch = target
			env.GitLab.SetMR(mr)
			statusOK(env, headB)
			changed := tc.changed
			if changed == nil {
				changed = []string{"b.go"}
			}
			serveChangedSince(env, changed...)
			if tc.change != nil {
				tc.change(env)
			}
			mark := len(env.Logs())

			dispatch(t, env)

			assertFull(t, env, id)
			got := fullReviews(env, mark)
			if len(got) != 1 {
				t.Fatalf("logged %d full reviews %v, want 1\n%s", len(got), got, env.Logs()[mark:])
			}
			if tc.reason != "" && got[0] != fmt.Sprintf("%q", tc.reason) {
				t.Errorf("full review reason = %s, want %q", got[0], tc.reason)
			}
		})
	}

	// A re-run on request is a full review whatever the last review, and is
	// not a push, so it logs no reason.
	t.Run("rerun", func(t *testing.T) {
		env := newTestEnv(t)
		publishedReview(t, env, headA, "0.5.0", nil)
		id, created, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(3), testProjectPath, headB, "main", store.TriggerRerun, nil, nil, false)
		if err != nil || !created {
			t.Fatalf("EnqueueRerun = %v, %v", created, err)
		}
		env.GitLab.SetMR(openMR(3, headB))
		statusOK(env, headB)
		serveChangedSince(env, "b.go")
		mark := len(env.Logs())

		dispatch(t, env)

		assertFull(t, env, id)
		if got := fullReviews(env, mark); len(got) != 0 {
			t.Errorf("a re-run logged a full review: %v", got)
		}
		if reqs := env.GitLab.Requests(http.MethodGet, mergeBaseRoute()); len(reqs) != 0 {
			t.Errorf("a re-run compared commits: %+v", reqs)
		}
	})
}

// The last review is the newest published one, not a run that was superseded
// after it: the comparison goes from A even though B was queued in between.
func TestIncrementalComparesFromLastPublished(t *testing.T) {
	env := newTestEnv(t)
	headA, headB, headC := sha('1'), sha('2'), sha('3')
	a := publishedReview(t, env, headA, "0.5.0", nil)
	env.Enqueue(3, headB)
	c := env.Enqueue(3, headC) // supersedes the run of headB
	env.GitLab.SetMR(openMR(3, headC))
	env.GitLab.Reply(http.MethodPost, statusPath(headC), http.StatusCreated, `{}`)
	serveChangedSince(env, "b.go")

	dispatch(t, env)

	reqs := env.GitLab.Requests(http.MethodGet, mergeBaseRoute())
	if len(reqs) != 1 || !slices.Equal(reqs[0].Query["refs[]"], []string{headA, headC}) {
		t.Fatalf("merge base requests = %+v, want one from %s to %s", reqs, headA, headC)
	}
	if r := env.Run(c); !r.Incremental || r.BaseRunID == nil || *r.BaseRunID != a {
		t.Errorf("run incremental = %v, base = %v; want an incremental review on %s", r.Incremental, r.BaseRunID, a)
	}
}

// teach stores a dismissal that teaches reviews, made by the platform user
// by on merge request iid.
func teach(t *testing.T, env *testEnv, iid int64, skill, fp, severity, reason string, by int64) {
	t.Helper()
	if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{
		MR: env.MRKey(iid), Fingerprint: fp, Severity: severity, Reason: reason, Actor: "maya (gitlab main)",
		CreatedAt: env.Now().Add(-time.Hour), Skill: skill, File: "api/auth.go", Title: "Token compared with ==",
		Teaches: true, DismisserPlatformID: by,
	}); err != nil {
		t.Fatal(err)
	}
}

// dismissalsEnv returns the value of PERGENT_DISMISSALS on job, and whether it
// is set.
func dismissalsEnv(job *batchv1.Job) (string, bool) {
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "PERGENT_DISMISSALS" {
			return e.Value, true
		}
	}
	return "", false
}

func authoredBy(mr gitlab.MergeRequest, id int64) gitlab.MergeRequest {
	mr.Author = gitlab.User{ID: id, Username: "dev"}
	return mr
}

// A review is told what people rejected earlier, for its skills: not by the
// change's author, not for a skill it doesn't run, and not who rejected it.
func TestDispatchPassesDismissals(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(authoredBy(openMR(3, head), 7))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	teach(t, env, 1, "security", "aaaaaa111111", "blocking", "Not a secret: a public request ID.", 99)
	teach(t, env, 2, "security", "bbbbbb111111", "blocking", "Mine, on another change.", 7)
	teach(t, env, 2, "style", "cccccc111111", "suggestion", "Not this skill.", 99)

	dispatch(t, env)

	job := env.Job(id)
	if job == nil {
		t.Fatalf("no job; run = %+v\n%s", env.Run(id), env.Logs())
	}
	if v, ok := dismissalsEnv(job); !ok || v != "/in/dismissals.json" {
		t.Errorf("PERGENT_DISMISSALS = %q, %v", v, ok)
	}
	for _, a := range job.Spec.Template.Spec.Containers[0].Args {
		if strings.HasPrefix(a, "--dismissals") {
			t.Errorf("job got the argument %q", a)
		}
	}
	const want = `{"security":[{"file":"api/auth.go","title":"Token compared with ==","severity":"blocking","reason":"Not a secret: a public request ID."}]}`
	if got := env.ConfigMap(id).Data["dismissals.json"]; got != want {
		t.Errorf("dismissals.json = %s\nwant %s", got, want)
	}
}

func TestDispatchSettingOffPassesNone(t *testing.T) {
	env := newTestEnv(t)
	off := false
	env.Engine.Config.Repositories[0].LearnFromDismissals = &off
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(authoredBy(openMR(3, head), 7))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	teach(t, env, 1, "security", "aaaaaa111111", "blocking", "Not a secret.", 99)

	dispatch(t, env)

	job := env.Job(id)
	if job == nil {
		t.Fatalf("no job; run = %+v\n%s", env.Run(id), env.Logs())
	}
	if v, ok := dismissalsEnv(job); ok {
		t.Errorf("PERGENT_DISMISSALS = %q with the setting off", v)
	}
	if _, ok := env.ConfigMap(id).Data["dismissals.json"]; ok {
		t.Error("dismissals.json present with the setting off")
	}
}

// A list that doesn't fit is dropped before the previous result is: the push
// stays incremental.
func TestDispatchTooManyDismissalsStaysIncremental(t *testing.T) {
	env := newTestEnv(t)
	// The skill leaves room for the previous result and the carried files,
	// but not for 20 long reasons.
	const head = "---\nname: security\n---\n"
	big := head + strings.Repeat("x", inputs.ConfigMapLimit-len(head)-15000)
	if _, err := env.Store.UpdateLibrarySkill(env.ctx, "security", big, "test", 1, env.Now()); err != nil {
		t.Fatal(err)
	}
	headA, headB := sha('1'), sha('2')
	publishedReview(t, env, headA, "0.5.0", nil)
	for i := 0; i < 25; i++ {
		teach(t, env, 1, "security", fmt.Sprintf("f%011d", i), "suggestion", strings.Repeat("日", 500), 99)
	}
	id := env.Enqueue(3, headB)
	env.GitLab.SetMR(authoredBy(openMR(3, headB), 7))
	env.GitLab.Reply(http.MethodPost, statusPath(headB), http.StatusCreated, `{}`)
	serveChangedSince(env, "b.go")

	dispatch(t, env)

	job := env.Job(id)
	if job == nil {
		t.Fatalf("no job; run = %+v\n%s", env.Run(id), env.Logs())
	}
	if v, ok := dismissalsEnv(job); ok {
		t.Errorf("PERGENT_DISMISSALS = %q for a list over the limit", v)
	}
	if !strings.Contains(env.Logs(), "reviewing without past dismissals: the inputs are too large for them") {
		t.Errorf("no warning that the dismissals were dropped\n%s", env.Logs())
	}
	if strings.Contains(env.Logs(), "msg=\"past dismissals\"") {
		t.Errorf("the log says the dismissals were passed\n%s", env.Logs())
	}
	if _, ok := env.ConfigMap(id).Data["dismissals.json"]; ok {
		t.Error("dismissals.json present over the limit")
	}
	if !hasArg(job.Spec.Template.Spec.Containers[0].Args, "--carried=/in/carried.json") {
		t.Errorf("job args %v lack --carried", job.Spec.Template.Spec.Containers[0].Args)
	}
	if r := env.Run(id); !r.Incremental {
		t.Errorf("run incremental = %v; want an incremental review", r.Incremental)
	}
}

// A re-run of some skills is given the entries of those skills only.
func TestDispatchRerunPassesDismissalsOfItsSkills(t *testing.T) {
	env, head, base := twoSkills(t, map[string]int{"a": 1, "b": 1})
	teach(t, env, 2, "a", "aaaaaa111111", "suggestion", "Fine for a.", 99)
	teach(t, env, 2, "b", "bbbbbb111111", "suggestion", "Fine for b.", 99)

	r := dispatchRerun(t, env, head, base)

	const want = `{"a":[{"file":"api/auth.go","title":"Token compared with ==","severity":"suggestion","reason":"Fine for a."}]}`
	if got := env.ConfigMap(r.ID).Data["dismissals.json"]; got != want {
		t.Errorf("dismissals.json = %s\nwant %s", got, want)
	}
}
