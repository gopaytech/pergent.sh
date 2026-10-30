package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/server"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// e2e drives pergent.sh's HTTP side (server.Routes, which main serves)
// against the engine of a testEnv.
type e2e struct {
	env *testEnv
	srv *httptest.Server
}

func newE2E(t *testing.T) *e2e {
	env := newTestEnv(t)
	mux := server.Routes(server.Deps{
		Config:         env.Engine.Config,
		WebhookSecrets: env.Engine.WebhookSecrets,
		Store:          env.Store,
		Wake:           env.Engine.Wake,
		Now:            env.Now,
		Log:            env.Engine.Log,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &e2e{env: env, srv: srv}
}

// hook posts a merge request webhook for MR iid at head; oldrev is the
// previous head for a push ("" for an open).
func (e *e2e) hook(t *testing.T, action string, iid int64, head, oldrev string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"object_kind": "merge_request",
		"project":     map[string]any{"id": testProjectID, "path_with_namespace": testProjectPath},
		"object_attributes": map[string]any{
			"iid": iid, "action": action, "oldrev": oldrev, "target_branch": "main",
			"last_commit": map[string]any{"id": head},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/webhooks/gitlab/"+testInstance, bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", testWebhookSecret)
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook %s %s: status %d", action, head, resp.StatusCode)
	}
}

// postResult posts body as run id's result with token and returns the status.
func (e *e2e) postResult(t *testing.T, id, token string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/runs/"+id+"/result", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// newJob returns the run id of the one Job that isn't in known, read from
// the Job's run label.
func (e *e2e) newJob(t *testing.T, known ...string) string {
	t.Helper()
	var ids []string
	for _, j := range e.env.Jobs() {
		if id := j.Labels["pergent.sh/run"]; !slices.Contains(known, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("got %d new Jobs, want 1", len(ids))
	}
	return ids[0]
}

func TestEndToEnd(t *testing.T) {
	e := newE2E(t)
	env := e.env
	headA, headB, headC := sha('a'), sha('c'), sha('d')
	for _, h := range []string{headA, headB, headC} {
		env.GitLab.Reply(http.MethodPost, statusPath(h), http.StatusCreated, `{}`)
	}

	// 1. The MR is opened at A: a Job for A.
	env.GitLab.SetMR(openMR(1, headA))
	e.hook(t, "open", 1, headA, "")
	dispatch(t, env)
	runA := e.newJob(t)
	if r := env.Run(runA); r.HeadSHA != headA || r.State != store.Running {
		t.Fatalf("run A = %+v", r)
	}
	tokenA := string(env.Secret(runA).Data["result-token"])

	// 2. A push of B: Job A is deleted and A's status canceled.
	env.GitLab.SetMR(openMR(1, headB))
	e.hook(t, "update", 1, headB, headA)
	cleanup(t, env)
	if env.Job(runA) != nil {
		t.Errorf("Job A still exists after the push of B")
	}
	statusesA := env.GitLab.Requests(http.MethodPost, statusPath(headA))
	if len(statusesA) == 0 || statusesA[len(statusesA)-1].JSON(t)["state"] != string(gitlab.StatusCanceled) {
		t.Errorf("A's last commit status is not canceled: %d statuses", len(statusesA))
	}
	if got := env.Run(runA).State; got != store.Superseded {
		t.Errorf("run A = %s, want superseded", got)
	}

	// 3. A Job for B.
	dispatch(t, env)
	runB := e.newJob(t, runA)
	if r := env.Run(runB); r.HeadSHA != headB || r.State != store.Running {
		t.Fatalf("run B = %+v", r)
	}
	tokenB := string(env.Secret(runB).Data["result-token"])

	// 4. A's late clean result is accepted and discarded; nothing is posted.
	env.GitLab.Reset()
	if code := e.postResult(t, runA, tokenA, resultFor(t, headA, reviewed("no_blocking_issues", "a.go"))); code != http.StatusOK {
		t.Errorf("A's result: status %d, want 200", code)
	}
	if r := env.Run(runA); r.State != store.Superseded || r.Result != nil {
		t.Errorf("run A = %s with result %v; want superseded without one", r.State, r.Result != nil)
	}
	publish(t, env)
	cleanup(t, env)
	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("A's late result made %d GitLab requests, want none", n)
	}

	// 5. B's clean result is stored.
	if code := e.postResult(t, runB, tokenB, resultFor(t, headB, reviewed("no_blocking_issues", "a.go"))); code != http.StatusOK {
		t.Fatalf("B's result: status %d, want 200", code)
	}

	// 6. One comment, success on B, an approval of B.
	servePublish(env, 1, headB)
	publish(t, env)
	createdComment(t, env, 1)
	if st := onlyStatus(t, env, headB); st["state"] != string(gitlab.StatusSuccess) {
		t.Errorf("B's commit status = %v, want success", st)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, statusPath(headA))); n != 0 {
		t.Errorf("set %d statuses on A after its result, want none", n)
	}
	approvals := env.GitLab.Requests(http.MethodPost, approvePath(1))
	if len(approvals) != 1 || approvals[0].Token != testApproveToken || approvals[0].JSON(t)["sha"] != headB {
		t.Fatalf("approvals = %d, want one of B by the approve bot", len(approvals))
	}
	wantPublished(t, env, runB)

	// 7. A push of C removes the approval of B.
	env.GitLab.SetMR(openMR(1, headC))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	e.hook(t, "update", 1, headC, headB)
	cleanup(t, env)
	unapprovals := env.GitLab.Requests(http.MethodPost, unapprovePath(1))
	if len(unapprovals) != 1 || unapprovals[0].Token != testApproveToken {
		t.Errorf("unapprovals = %d, want one by the approve bot", len(unapprovals))
	}
}

// postHook posts a merge request webhook with the given object attributes
// and changes for MR 1 of acme/widgets.
func (e *e2e) postHook(t *testing.T, attrs, changes map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"object_kind":       "merge_request",
		"project":           map[string]any{"id": testProjectID, "path_with_namespace": testProjectPath},
		"object_attributes": attrs,
		"changes":           changes,
	})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/webhooks/gitlab/"+testInstance, bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", testWebhookSecret)
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: status %d", resp.StatusCode)
	}
}

// Retargeting an approved merge request reviews its head again against the
// new target and withdraws the approval; a title edit does neither.
func TestE2ERetargetReviewsAgainAndUnapproves(t *testing.T) {
	e := newE2E(t)
	env := e.env
	head := sha('a')
	approved(t, env, 1, head)
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	e.postHook(t, map[string]any{"iid": 1, "action": "update", "target_branch": "main",
		"last_commit": map[string]any{"id": head}},
		map[string]any{"title": map[string]any{"previous": "a", "current": "b"}})
	select {
	case <-env.Engine.wakeCh():
		t.Fatalf("a title edit queued a run")
	default:
	}
	cleanup(t, env)
	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 0 {
		t.Fatalf("a title edit withdrew the approval")
	}

	e.postHook(t, map[string]any{"iid": 1, "action": "update", "target_branch": "develop",
		"last_commit": map[string]any{"id": head}},
		map[string]any{"target_branch": map[string]any{"previous": "main", "current": "develop"}})
	r := liveRun(t, env, 1, head, "develop")
	if r.State != store.Queued {
		t.Errorf("retarget run = %+v, want queued", r)
	}
	cleanup(t, env)
	reqs := env.GitLab.Requests(http.MethodPost, unapprovePath(1))
	if len(reqs) != 1 || reqs[0].Token != testApproveToken {
		t.Fatalf("got %d unapprovals, want 1 by the approve bot", len(reqs))
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %v, want none", a)
	}
}
