package engine

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	testHookURL      = "https://pergent.example.com/webhooks/gitlab/main"
	newDeployToken   = "new-deploy-token-example"
	newDeployTokenID = int64(42)
	newDeployUser    = "gitlab+deploy-token-42"
	newHookID        = int64(31)
)

func reconcile(t *testing.T, env *testEnv) {
	t.Helper()
	if err := env.Engine.Reconcile(env.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// serveProject routes what Reconcile needs for project id at path: the
// project lookup, an empty hook list, hook creation and deploy token creation.
func serveProject(env *testEnv, id int64, path string) {
	pid := "/projects/" + strconv.FormatInt(id, 10)
	env.GitLab.Reply(http.MethodGet, "/projects/"+strings.ReplaceAll(path, "/", "%2F"), http.StatusOK, map[string]any{
		"id": id, "path_with_namespace": path, "http_url_to_repo": "https://gitlab.example.com/" + path + ".git",
	})
	env.GitLab.Reply(http.MethodGet, pid+"/hooks", http.StatusOK, `[]`)
	env.GitLab.Reply(http.MethodPost, pid+"/hooks", http.StatusCreated, map[string]any{"id": newHookID, "url": testHookURL})
	env.GitLab.Reply(http.MethodPost, pid+"/deploy_tokens", http.StatusCreated, map[string]any{
		"id": newDeployTokenID, "username": newDeployUser, "token": newDeployToken,
		"expires_at": env.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339),
	})
}

func project(t *testing.T, env *testEnv, id int64) (store.Project, bool) {
	t.Helper()
	p, ok, err := env.Store.Project(env.ctx, testInstance, id)
	if err != nil {
		t.Fatal(err)
	}
	return p, ok
}

func TestReconcileCreatesHookAndToken(t *testing.T) {
	env := newTestEnv(t)
	if err := env.Store.DeleteProject(env.ctx, testInstance, testProjectID); err != nil {
		t.Fatal(err)
	}
	serveProject(env, testProjectID, testProjectPath)

	reconcile(t, env)

	hooks := env.GitLab.Requests(http.MethodPost, "/projects/7/hooks")
	if len(hooks) != 1 {
		t.Fatalf("got %d hook creations, want 1", len(hooks))
	}
	hook := hooks[0].JSON(t)
	if hook["url"] != testHookURL || hook["token"] != testWebhookSecret || hook["merge_requests_events"] != true {
		t.Errorf("hook = %v", hook)
	}
	if hooks[0].Token != testReviewToken {
		t.Errorf("hook created with token %q, want the review bot's", hooks[0].Token)
	}

	tokens := env.GitLab.Requests(http.MethodPost, "/projects/7/deploy_tokens")
	if len(tokens) != 1 {
		t.Fatalf("got %d deploy token creations, want 1", len(tokens))
	}
	dt := tokens[0].JSON(t)
	wantExp := env.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if dt["name"] != "pergent-sh" || dt["expires_at"] != wantExp {
		t.Errorf("deploy token request = %v, want name pergent-sh expiring %s", dt, wantExp)
	}
	if n := len(env.GitLab.Requests(http.MethodDelete, "")); n != 0 {
		t.Errorf("made %d deletes on a first reconcile", n)
	}

	p, ok := project(t, env, testProjectID)
	if !ok {
		t.Fatal("project not stored")
	}
	if p.Path != testProjectPath || p.HTTPURLToRepo != testRepoURL || p.HookID != newHookID ||
		p.DeployTokenID != newDeployTokenID || p.DeployTokenUsername != newDeployUser {
		t.Errorf("project = %+v", p)
	}
	if !p.DeployTokenExpiresAt.Equal(env.Now().Add(30 * 24 * time.Hour)) {
		t.Errorf("deploy token expires %s", p.DeployTokenExpiresAt)
	}
	if bytes.Contains(p.DeployTokenSealed, []byte(newDeployToken)) {
		t.Errorf("the deploy token is stored in the clear")
	}
	plain, err := env.Box.Open(p.DeployTokenSealed)
	if err != nil || string(plain) != newDeployToken {
		t.Errorf("sealed token opens to %q, %v; want %q", plain, err, newDeployToken)
	}
	for _, secret := range []string{newDeployToken, testWebhookSecret, testReviewToken} {
		if strings.Contains(env.Logs(), secret) {
			t.Errorf("a secret reached the log:\n%s", env.Logs())
		}
	}
}

func TestReconcileKeepsFreshToken(t *testing.T) {
	env := newTestEnv(t) // the seeded token expires in 30 days
	serveProject(env, testProjectID, testProjectPath)
	env.GitLab.Reply(http.MethodGet, "/projects/7/hooks", http.StatusOK, []map[string]any{{"id": 11, "url": testHookURL}})
	env.GitLab.Reply(http.MethodPut, "/projects/7/hooks/11", http.StatusOK, map[string]any{"id": 11, "url": testHookURL})

	reconcile(t, env)

	if n := len(env.GitLab.Requests(http.MethodPut, "/projects/7/hooks/11")); n != 1 {
		t.Errorf("got %d hook updates, want 1", n)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, "/projects/7/hooks")); n != 0 {
		t.Errorf("created %d hooks although one exists", n)
	}
	if n := len(env.GitLab.Requests("", "/projects/7/deploy_tokens")); n != 0 {
		t.Errorf("touched the deploy token %d times, want never", n)
	}
	p, _ := project(t, env, testProjectID)
	if p.DeployTokenID != 12 || p.HookID != 11 {
		t.Errorf("project = %+v", p)
	}
}

func TestReconcileRotatesExpiringToken(t *testing.T) {
	env := newTestEnv(t)
	env.SeedProject(func(p *store.Project) { p.DeployTokenExpiresAt = env.Now().Add(6 * 24 * time.Hour) })
	serveProject(env, testProjectID, testProjectPath)
	var storedAtDelete store.Project
	env.GitLab.Handle(http.MethodDelete, "/projects/7/deploy_tokens/12", func(w http.ResponseWriter, _ *http.Request) {
		storedAtDelete, _ = project(t, env, testProjectID)
		w.WriteHeader(http.StatusNoContent)
	})

	reconcile(t, env)

	if n := len(env.GitLab.Requests(http.MethodPost, "/projects/7/deploy_tokens")); n != 1 {
		t.Fatalf("got %d deploy token creations, want 1", n)
	}
	dels := env.GitLab.Requests(http.MethodDelete, "/projects/7/deploy_tokens/12")
	if len(dels) != 1 || dels[0].Token != testReviewToken {
		t.Fatalf("old token deletes = %d, want 1 by the review bot", len(dels))
	}
	if storedAtDelete.DeployTokenID != newDeployTokenID {
		t.Errorf("the old token was deleted before the new one was stored (stored id %d)", storedAtDelete.DeployTokenID)
	}
	p, _ := project(t, env, testProjectID)
	plain, err := env.Box.Open(p.DeployTokenSealed)
	if p.DeployTokenID != newDeployTokenID || err != nil || string(plain) != newDeployToken {
		t.Errorf("project token = %d, %q, %v", p.DeployTokenID, plain, err)
	}
}

// A token that can't be created leaves the old one in place.
func TestReconcileTokenCreationFailureKeepsOldToken(t *testing.T) {
	env := newTestEnv(t)
	env.SeedProject(func(p *store.Project) { p.DeployTokenExpiresAt = env.Now().Add(24 * time.Hour) })
	serveProject(env, testProjectID, testProjectPath)
	env.GitLab.Reply(http.MethodPost, "/projects/7/deploy_tokens", http.StatusForbidden, `{"message":"403 Forbidden"}`)

	if err := env.Engine.Reconcile(env.ctx); err == nil {
		t.Fatal("Reconcile succeeded although the token couldn't be created")
	}
	if n := len(env.GitLab.Requests(http.MethodDelete, "")); n != 0 {
		t.Errorf("deleted %d things although no new token exists", n)
	}
	p, _ := project(t, env, testProjectID)
	if p.DeployTokenID != 12 {
		t.Errorf("project token id = %d, want the old 12 kept", p.DeployTokenID)
	}
}

func TestReconcileRemovesUnconfigured(t *testing.T) {
	env := newTestEnv(t)
	serveProject(env, testProjectID, testProjectPath)
	env.SeedProject(func(p *store.Project) {
		p.ProjectID, p.Path, p.HookID, p.DeployTokenID = 8, "acme/old", 21, 22
	})
	env.GitLab.Reply(http.MethodDelete, "/projects/8/hooks/21", http.StatusNoContent, nil)
	env.GitLab.Reply(http.MethodDelete, "/projects/8/deploy_tokens/22", http.StatusNoContent, nil)

	reconcile(t, env)

	for _, path := range []string{"/projects/8/hooks/21", "/projects/8/deploy_tokens/22"} {
		reqs := env.GitLab.Requests(http.MethodDelete, path)
		if len(reqs) != 1 || reqs[0].Token != testReviewToken {
			t.Errorf("DELETE %s: %d requests, want 1 by the review bot", path, len(reqs))
		}
	}
	if _, ok := project(t, env, 8); ok {
		t.Errorf("the unconfigured project is still stored")
	}
	if _, ok := project(t, env, testProjectID); !ok {
		t.Errorf("the configured project was removed")
	}
}

// A removal GitLab refuses keeps the row, so it is tried again.
func TestReconcileRemovalFailureKeepsRow(t *testing.T) {
	env := newTestEnv(t)
	serveProject(env, testProjectID, testProjectPath)
	env.SeedProject(func(p *store.Project) {
		p.ProjectID, p.Path, p.HookID, p.DeployTokenID = 8, "acme/old", 21, 22
	})
	env.GitLab.Reply(http.MethodDelete, "/projects/8/hooks/21", http.StatusForbidden, `{"message":"403 Forbidden"}`)
	env.GitLab.Reply(http.MethodDelete, "/projects/8/deploy_tokens/22", http.StatusNoContent, nil)

	if err := env.Engine.Reconcile(env.ctx); err == nil {
		t.Fatal("Reconcile succeeded although the hook couldn't be deleted")
	}
	if _, ok := project(t, env, 8); !ok {
		t.Errorf("the row was removed although its hook is still there")
	}
}

// One repository's failure is logged and joined; the others are reconciled,
// and a configured project that couldn't be looked up is never removed.
func TestReconcileOneRepoFailureDoesNotStopOthers(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Config.Repositories = []config.Repository{
		{GitLab: testInstance, Project: testProjectPath, FailureStatus: config.FailureStatusFailed},
		{GitLab: testInstance, Project: "acme/gadgets", FailureStatus: config.FailureStatusFailed},
	}
	env.GitLab.Reply(http.MethodGet, "/projects/acme%2Fwidgets", http.StatusForbidden, `{"message":"403 Forbidden"}`)
	serveProject(env, 9, "acme/gadgets")

	err := env.Engine.Reconcile(env.ctx)
	if err == nil || !strings.Contains(err.Error(), testProjectPath) {
		t.Fatalf("Reconcile = %v, want an error naming %s", err, testProjectPath)
	}
	if !strings.Contains(env.Logs(), testProjectPath) {
		t.Errorf("the failure was not logged:\n%s", env.Logs())
	}
	if p, ok := project(t, env, 9); !ok || p.HookID != newHookID || p.DeployTokenID != newDeployTokenID {
		t.Errorf("acme/gadgets = %+v, %v; want reconciled", p, ok)
	}
	if _, ok := project(t, env, testProjectID); !ok {
		t.Errorf("acme/widgets was removed although it is configured")
	}
	if n := len(env.GitLab.Requests(http.MethodDelete, "")); n != 0 {
		t.Errorf("made %d deletes", n)
	}
}

// A project deleted and recreated under a configured path is a new project:
// the old row is cleaned up and the new one set up.
func TestReconcileReplacesRecreatedProject(t *testing.T) {
	env := newTestEnv(t) // project 7 at acme/widgets, hook 11, token 12
	serveProject(env, 9, testProjectPath)
	env.GitLab.Reply(http.MethodDelete, "/projects/7/hooks/11", http.StatusNotFound, `{"message":"404 Project Not Found"}`)
	env.GitLab.Reply(http.MethodDelete, "/projects/7/deploy_tokens/12", http.StatusNotFound, `{"message":"404 Project Not Found"}`)

	reconcile(t, env)

	if _, ok := project(t, env, testProjectID); ok {
		t.Errorf("the old project's row is still stored")
	}
	if p, ok := project(t, env, 9); !ok || p.HookID != newHookID || p.DeployTokenID != newDeployTokenID {
		t.Errorf("new project = %+v, %v", p, ok)
	}
}

// A project of an instance removed from the config can't be reached: its row
// is dropped with one warning, not reported as an error every hour.
func TestReconcileForgetsProjectOfRemovedInstance(t *testing.T) {
	env := newTestEnv(t)
	serveProject(env, testProjectID, testProjectPath)
	env.SeedProject(func(p *store.Project) {
		p.Instance, p.ProjectID, p.Path, p.HookID, p.DeployTokenID = "old", 8, "acme/old", 21, 22
	})

	reconcile(t, env)

	if _, ok, err := env.Store.Project(env.ctx, "old", 8); err != nil || ok {
		t.Errorf("the removed instance's project is still stored (%v)", err)
	}
	if n := len(env.GitLab.Requests("", "/projects/8/hooks/21")) + len(env.GitLab.Requests("", "/projects/8/deploy_tokens/22")); n != 0 {
		t.Errorf("made %d requests for a project of a removed instance", n)
	}
	if got := strings.Count(env.Logs(), "level=WARN"); got != 1 {
		t.Errorf("got %d warnings, want 1:\n%s", got, env.Logs())
	}
	if strings.Contains(env.Logs(), "level=ERROR") {
		t.Errorf("logged an error:\n%s", env.Logs())
	}
	if _, ok := project(t, env, testProjectID); !ok {
		t.Errorf("the configured project was removed")
	}
}
