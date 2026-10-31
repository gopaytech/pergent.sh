package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/result"
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
		Instances:      env.Engine.Instances,
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

// resultWithMarkdown is resultFor with the comment's markdown replaced.
func resultWithMarkdown(t *testing.T, head, markdown string, skills ...result.Skill) []byte {
	t.Helper()
	var r result.Result
	if err := json.Unmarshal(resultFor(t, head, skills...), &r); err != nil {
		t.Fatal(err)
	}
	r.Comment.Markdown = markdown
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Commit statuses are per commit, not per target branch. A head reviewed
// against main (blocking), retargeted to develop (clean, approved) and back
// to main must be reviewed against main again: the earlier main review is
// no longer what the comment, the status and the approval show.
func TestE2ERetargetBackToEarlierTargetReviewsAgain(t *testing.T) {
	e := newE2E(t)
	env := e.env
	head := sha('a')
	mrAt := func(target string) gitlab.MergeRequest {
		mr := openMR(1, head)
		mr.TargetBranch = target
		return mr
	}
	retarget := func(from, to string) {
		t.Helper()
		env.GitLab.SetMR(mrAt(to))
		e.postHook(t, map[string]any{"iid": 1, "action": "update", "target_branch": to,
			"last_commit": map[string]any{"id": head}},
			map[string]any{"target_branch": map[string]any{"previous": from, "current": to}})
	}
	// review dispatches the one queued run, posts its result and publishes it.
	review := func(target, verdict string, known ...string) string {
		t.Helper()
		dispatch(t, env)
		id := e.newJob(t, known...)
		if r := env.Run(id); r.HeadSHA != head || r.TargetBranch != target || r.State != store.Running {
			t.Fatalf("run = %+v, want %s against %s running", r, head, target)
		}
		token := string(env.Secret(id).Data["result-token"])
		body := resultWithMarkdown(t, head, "review against "+target, reviewed(verdict, "a.go"))
		if code := e.postResult(t, id, token, body); code != http.StatusOK {
			t.Fatalf("result: status %d", code)
		}
		publish(t, env)
		wantPublished(t, env, id)
		return id
	}
	lastStatus := func() map[string]any {
		t.Helper()
		reqs := env.GitLab.Requests(http.MethodPost, statusPath(head))
		if len(reqs) == 0 {
			t.Fatalf("no commit status on %s", head)
		}
		return reqs[len(reqs)-1].JSON(t)
	}
	lastComment := func() string {
		t.Helper()
		var bodies []string
		for _, r := range env.GitLab.Requests("", "") {
			if (r.Method == http.MethodPost && r.Path == notesPath(1)) || (r.Method == http.MethodPut && r.Path == notePath(1, createdNote)) {
				body, _ := r.JSON(t)["body"].(string)
				bodies = append(bodies, body)
			}
		}
		if len(bodies) == 0 {
			t.Fatalf("no comment posted")
		}
		return bodies[len(bodies)-1]
	}
	env.GitLab.Reply(http.MethodGet, notesPath(1), http.StatusOK, `[]`)
	env.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusCreated, map[string]any{"id": createdNote})
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, map[string]any{"id": createdNote})
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	env.GitLab.Reply(http.MethodPost, approvePath(1), http.StatusCreated, `{}`)
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	// 1. H against main: blocking, status failed, no approval.
	env.GitLab.SetMR(mrAt("main"))
	e.postHook(t, map[string]any{"iid": 1, "action": "open", "target_branch": "main",
		"last_commit": map[string]any{"id": head}}, nil)
	mainRun := review("main", "has_blocking_issues")
	if st := lastStatus(); st["state"] != string(gitlab.StatusFailed) {
		t.Fatalf("status after the main review = %v, want failed", st)
	}

	// 2. Retargeted to develop: clean, status success, approved.
	retarget("main", "develop")
	cleanup(t, env)
	devRun := review("develop", "no_blocking_issues", mainRun)
	if st := lastStatus(); st["state"] != string(gitlab.StatusSuccess) {
		t.Fatalf("status after the develop review = %v, want success", st)
	}
	if a := approvals(t, env); len(a) != 1 || a[0].ApprovedSHA != head || a[0].ApprovedTarget != "develop" {
		t.Fatalf("approvals = %v, want %s against develop", a, head)
	}

	// 3. Back to main: a new run of H against main is queued, and the
	// approval of the develop review is withdrawn.
	env.GitLab.Reset()
	retarget("develop", "main")
	r := liveRun(t, env, 1, head, "main")
	if r.ID == mainRun || r.State != store.Queued {
		t.Fatalf("after retargeting back: run %s in state %s; want a new queued run, not the published %s", r.ID, r.State, mainRun)
	}
	cleanup(t, env)
	if reqs := env.GitLab.Requests(http.MethodPost, unapprovePath(1)); len(reqs) != 1 || reqs[0].Token != testApproveToken {
		t.Fatalf("got %d unapprovals, want 1 by the approve bot", len(reqs))
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Fatalf("approvals = %v, want none", a)
	}

	// 4. Its blocking result publishes: H's status is failed again, the
	// comment shows the main review, and nothing is approved.
	if got := review("main", "has_blocking_issues", mainRun, devRun); got != r.ID {
		t.Errorf("reviewed run %s, want %s", got, r.ID)
	}
	if st := lastStatus(); st["state"] != string(gitlab.StatusFailed) || st["description"] != "Blocking issues found" {
		t.Errorf("status after retargeting back = %v, want failed", st)
	}
	if body := lastComment(); !strings.Contains(body, "review against main") {
		t.Errorf("comment = %q, want the main review", body)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 0 {
		t.Errorf("approved %d times after retargeting back, want never", n)
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %v, want none", a)
	}
}
