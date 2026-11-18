package engine

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// An admin pauses a repository and resumes it, through the real routes and
// engine. While paused nothing reaches GitLab for it: a webhook starts no
// review, a comment command is ignored, a run queued before the pause is
// dropped, and a settings change asks GitLab nothing. A team member still
// sees it and its reviews. Resume sets it up again on the same project, with
// its settings and its dismissal, and queues a readiness scan.
func TestEndToEndPauseResume(t *testing.T) {
	// Comment commands are on, as in the binary, so an ignored one shows.
	e := newCommandsE2E(t).dashE2E
	env := e.env
	env.Engine.Scans = true
	gl := env.GitLab

	t.Setenv(adminPasswordEnv, bootstrapPassword)
	env.Engine.Config.Dashboard.AdminPasswordEnv = adminPasswordEnv
	if err := dashboard.Bootstrap(env.ctx, env.Store, env.Engine.Config, env.Now(), env.Engine.Log); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	admin := e.passwordSignIn(t, "admin", bootstrapPassword)
	if status, body := e.write(t, admin, http.MethodPost, "/auth/password/change",
		map[string]string{"current": bootstrapPassword, "new": changedPassword}); status != http.StatusNoContent {
		t.Fatalf("password change = %d: %s", status, body)
	}
	repoPath := "/api/repositories/" + testInstance + "/" + gadgetsPath
	switchPath := "/api/admin/repositories/" + testInstance + "/" + gadgetsPath
	reconcileSoon := func() {
		t.Helper()
		if err := env.Engine.ReconcileRepos(env.ctx, env.Engine.takeSoonKeys()); err != nil {
			t.Fatalf("ReconcileRepos: %v", err)
		}
	}

	// 1. acme/gadgets is switched on, set up, given settings, reviewed once,
	// and a finding of that review is dismissed. bob's team covers it.
	if status, body := e.write(t, admin, http.MethodPost, "/api/repositories", map[string]string{"instance": testInstance, "project": gadgetsPath}); status != http.StatusCreated {
		t.Fatalf("switch on = %d: %s", status, body)
	}
	reconcileSoon()
	if n := len(gl.Requests(http.MethodPost, gadgetsAPI("/hooks"))); n != 1 {
		t.Fatalf("%d hook creations, want 1", n)
	}
	if status, body := e.write(t, admin, http.MethodPut, repoPath+"/settings", map[string]any{"approve": false, "quietReviews": true}); status != http.StatusOK {
		t.Fatalf("settings = %d: %s", status, body)
	}
	seedPublished(t, env, gadgetsID, gadgetsPath, 1, sha('1'))
	mr1 := store.MRKey{Instance: testInstance, ProjectID: gadgetsID, IID: 1}
	if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{MR: mr1, Fingerprint: "fp-1", Severity: "blocking", Reason: "fine", Actor: "maya"}); err != nil {
		t.Fatal(err)
	}
	bob := e.signIn(t, "bob@example.com")
	team := e.newTeam(t, admin, "gadgets")
	e.setTeam(t, admin, team, "gadgets", []teamMember{{e.accountID(t, admin, "bob@example.com"), store.RoleViewer}},
		[]dashboard.TeamRepoView{{Instance: testInstance, Project: gadgetsPath}})
	// A review queued before the pause (merge request 2).
	queued, _, err := env.Store.EnqueueRun(env.ctx, store.MRKey{Instance: testInstance, ProjectID: gadgetsID, IID: 2}, gadgetsPath, sha('2'), "main")
	if err != nil {
		t.Fatal(err)
	}

	// 2. The pause: the reconcile deletes the hook and the deploy token, and
	// the scans, but keeps the project.
	gl.Reply(http.MethodDelete, gadgetsAPI("/hooks/31"), http.StatusNoContent, nil)
	gl.Reply(http.MethodDelete, gadgetsAPI("/deploy_tokens/42"), http.StatusNoContent, nil)
	var detail dashboard.RepositoryDetailView
	status, body := e.write(t, admin, http.MethodPut, switchPath, map[string]bool{"enabled": false})
	if status != http.StatusOK {
		t.Fatalf("pause = %d: %s", status, body)
	}
	if err := json.Unmarshal(body, &detail); err != nil || detail.Repo.On || !detail.Repo.Paused {
		t.Fatalf("pause answer = %+v, %v", detail.Repo, err)
	}
	reconcileSoon()
	if len(gl.Requests(http.MethodDelete, gadgetsAPI("/hooks/31"))) != 1 || len(gl.Requests(http.MethodDelete, gadgetsAPI("/deploy_tokens/42"))) != 1 {
		t.Errorf("the pause did not delete the hook and the deploy token")
	}
	if p, ok := project(t, env, gadgetsID); !ok || p.HookID != 0 || p.DeployTokenID != 0 {
		t.Fatalf("paused project = %+v, %v; want kept with no hook and no token", p, ok)
	}
	if latest, active, err := env.Store.ReadinessScans(env.ctx, testInstance, gadgetsPath); err != nil || latest != nil || active != nil {
		t.Errorf("scans while paused = %v, %v, %v; want none", latest, active, err)
	}

	// bob still sees the repository and its review.
	var seen dashboard.RepositoryDetailView
	e.getJSON(t, bob, repoPath, &seen)
	if !seen.Repo.Paused {
		t.Errorf("bob's view = %+v; want paused", seen.Repo)
	}
	e.expectStatus(t, bob, http.MethodGet, "/api/reviews/"+testInstance+"/"+gadgetsPath+"/1", http.StatusOK)
	var list dashboard.ReviewListView
	e.getJSON(t, bob, "/api/reviews?project="+gadgetsPath, &list)
	if len(list.Items) == 0 {
		t.Errorf("bob sees no reviews of the paused repository")
	}

	// 3. A merge request webhook starts no review; a command is ignored.
	gl.Reset()
	hook, _ := json.Marshal(map[string]any{
		"object_kind": "merge_request",
		"project":     map[string]any{"id": gadgetsID, "path_with_namespace": gadgetsPath},
		"object_attributes": map[string]any{
			"iid": 3, "action": "open", "target_branch": "main", "last_commit": map[string]any{"id": sha('3')},
		},
	})
	if status, body, _, _ := e.do(t, http.MethodPost, "/webhooks/gitlab/"+testInstance, json.RawMessage(hook), nil, map[string]string{
		"X-Gitlab-Token": testWebhookSecret, "X-Gitlab-Event": "Merge Request Hook",
	}); status != http.StatusOK {
		t.Fatalf("merge request webhook = %d: %s", status, body)
	}
	note, _ := json.Marshal(map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"id": 77, "username": "maya"},
		"project":     map[string]any{"id": gadgetsID, "path_with_namespace": gadgetsPath},
		"object_attributes": map[string]any{
			"id": 9001, "note": "@pergent-review review", "noteable_type": "MergeRequest", "type": nil, "action": "create", "system": false,
		},
		"merge_request": map[string]any{"iid": 1},
	})
	if status, body, _, _ := e.do(t, http.MethodPost, "/webhooks/gitlab/"+testInstance, json.RawMessage(note), nil, map[string]string{
		"X-Gitlab-Token": testWebhookSecret, "X-Gitlab-Event": "Note Hook",
	}); status != http.StatusOK {
		t.Fatalf("note webhook = %d: %s", status, body)
	}
	// The comment was not stored: storing it now is no redelivery.
	if stored, err := env.Store.InsertCommand(env.ctx, store.Command{MR: mr1, ProjectPath: gadgetsPath, Platform: "gitlab",
		Kind: "review", CommentID: 9001, Author: "maya", AuthorID: 77}); err != nil || !stored {
		t.Errorf("the paused repository's command was stored (%v, %v)", stored, err)
	}

	// 4. The engine's pass: the run queued before the pause is dropped, and
	// no Job starts for the paused repository.
	dispatch(t, env)
	for _, j := range env.Jobs() {
		if id := j.Labels["pergent.sh/run"]; env.Run(id).MR.ProjectID == gadgetsID {
			t.Errorf("a Job started for the paused repository: run %s", id)
		}
	}
	if r := env.Run(queued); r.State == store.Queued || r.State == store.Running {
		t.Errorf("the run queued before the pause is %s; want it dropped", r.State)
	}
	if n := runsOf(t, env, mr1); n != 1 {
		t.Errorf("%d runs of merge request 1; want the seeded one alone", n)
	}
	if n := runsOf(t, env, store.MRKey{Instance: testInstance, ProjectID: gadgetsID, IID: 3}); n != 0 {
		t.Errorf("%d runs of merge request 3 while paused; want none", n)
	}

	// 5. A settings change while paused asks GitLab nothing.
	gl.Reset()
	if status, body := e.write(t, admin, http.MethodPut, repoPath+"/settings", map[string]any{"reviewDrafts": false}); status != http.StatusOK {
		t.Fatalf("settings while paused = %d: %s", status, body)
	}
	if reqs := gl.Requests("", ""); len(reqs) != 0 {
		t.Errorf("a settings change while paused made %d GitLab requests", len(reqs))
	}

	// 6. Resume: the same project is set up again, with its settings and its
	// dismissal, and a scan is queued.
	serveDashboardGitLab(t, env)
	if status, body := e.write(t, admin, http.MethodPut, switchPath, map[string]bool{"enabled": true}); status != http.StatusOK {
		t.Fatalf("resume = %d: %s", status, body)
	}
	reconcileSoon()
	if p, ok := project(t, env, gadgetsID); !ok || p.HookID != newHookID || p.DeployTokenID != newDeployTokenID {
		t.Fatalf("resumed project = %+v, %v; want set up again", p, ok)
	}
	e.getJSON(t, admin, repoPath, &detail)
	if !detail.Repo.On || detail.Settings["approve"].Value != false || detail.Settings["quietReviews"].Value != true ||
		detail.Settings["reviewDrafts"].Value != false {
		t.Errorf("resumed = %+v; want on with approve off, quiet on and drafts off", detail)
	}
	if ds, err := env.Store.Dismissals(env.ctx, mr1); err != nil || len(ds) != 1 || ds[0].Fingerprint != "fp-1" {
		t.Errorf("dismissals after the resume = %+v, %v; want the one from before", ds, err)
	}
	if _, active, err := env.Store.ReadinessScans(env.ctx, testInstance, gadgetsPath); err != nil || active == nil || active.StartedBy != store.StartedBySwitchOn {
		t.Errorf("scan after the resume = %+v, %v; want a switch-on scan", active, err)
	}

	// 7. A merge request webhook now queues a review.
	if status, body, _, _ := e.do(t, http.MethodPost, "/webhooks/gitlab/"+testInstance, json.RawMessage(hook), nil, map[string]string{
		"X-Gitlab-Token": testWebhookSecret, "X-Gitlab-Event": "Merge Request Hook",
	}); status != http.StatusOK {
		t.Fatalf("merge request webhook after the resume = %d: %s", status, body)
	}
	if n := runsOf(t, env, store.MRKey{Instance: testInstance, ProjectID: gadgetsID, IID: 3}); n != 1 {
		t.Errorf("%d runs of merge request 3 after the resume; want 1", n)
	}
}

// runsOf counts the runs of mr.
func runsOf(t *testing.T, env *testEnv, mr store.MRKey) int {
	t.Helper()
	runs, err := env.Store.MRRunDigests(env.ctx, mr, store.Visibility{All: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(runs)
}
