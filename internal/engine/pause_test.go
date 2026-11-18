package engine

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// pause pauses the dashboard repository project on the test instance.
func pause(t *testing.T, env *testEnv, project string, enabled bool) {
	t.Helper()
	if found, _, err := env.Store.SetDashboardRepoEnabled(env.ctx, testInstance, project, enabled); err != nil || !found {
		t.Fatalf("SetDashboardRepoEnabled(%s, %v) = %v, %v", project, enabled, found, err)
	}
}

// A paused dashboard repository loses its webhook and deploy token, but its
// project row stays with its dismissals and commands; a later reconcile asks
// the platform nothing. Resume sets the same row up again and queues a scan.
func TestReconcilePausedDashboardRepo(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Scans = true
	serveProject(env, testProjectID, testProjectPath)
	serveProject(env, 9, "acme/gadgets")
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", CreatedBy: "alice"})
	reconcile(t, env)
	if p, ok := project(t, env, 9); !ok || p.HookID != newHookID || p.DeployTokenID != newDeployTokenID {
		t.Fatalf("acme/gadgets = %+v, %v; want set up", p, ok)
	}
	mr := store.MRKey{Instance: testInstance, ProjectID: 9, IID: 1}
	if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{MR: mr, Fingerprint: "fp", Severity: "blocking", Reason: "fine", Actor: "maya"}); err != nil {
		t.Fatal(err)
	}
	cmd := store.Command{MR: mr, ProjectPath: "acme/gadgets", Platform: "gitlab", Kind: "review", CommentID: 5, Author: "maya", AuthorID: 3}
	if _, err := env.Store.InsertCommand(env.ctx, cmd); err != nil {
		t.Fatal(err)
	}
	// Drop the switch-on scan, so the resume's is the one seen.
	env.Engine.dropScans(env.ctx, testInstance, "acme/gadgets")

	pause(t, env, "acme/gadgets", false)
	env.GitLab.Reply(http.MethodDelete, "/projects/9/hooks/31", http.StatusNoContent, nil)
	env.GitLab.Reply(http.MethodDelete, "/projects/9/deploy_tokens/42", http.StatusNoContent, nil)
	reconcile(t, env)
	if len(env.GitLab.Requests(http.MethodDelete, "/projects/9/hooks/31")) != 1 ||
		len(env.GitLab.Requests(http.MethodDelete, "/projects/9/deploy_tokens/42")) != 1 {
		t.Errorf("webhook and deploy token were not removed")
	}
	p, ok := project(t, env, 9)
	if !ok || p.HookID != 0 || p.DeployTokenID != 0 || len(p.DeployTokenSealed) != 0 || p.DeployTokenUsername != "" ||
		!p.DeployTokenExpiresAt.IsZero() || p.Path != "acme/gadgets" {
		t.Fatalf("paused project = %+v, %v; want kept with no hook and no token", p, ok)
	}
	if ds, err := env.Store.Dismissals(env.ctx, mr); err != nil || len(ds) != 1 {
		t.Errorf("dismissals after the pause = %+v, %v; want the one kept", ds, err)
	}
	// The same comment again is a redelivery while its row is kept.
	if stored, err := env.Store.InsertCommand(env.ctx, cmd); err != nil || stored {
		t.Errorf("the command again after the pause = %v, %v; want the kept row to refuse it", stored, err)
	}
	if latest, active, err := env.Store.ReadinessScans(env.ctx, testInstance, "acme/gadgets"); err != nil || latest != nil || active != nil {
		t.Errorf("scans while paused = %v, %v, %v; want none", latest, active, err)
	}

	// The hourly reconcile of a paused repository asks nothing more.
	env.GitLab.Reset()
	serveProject(env, testProjectID, testProjectPath)
	serveProject(env, 9, "acme/gadgets")
	reconcile(t, env)
	for _, r := range env.GitLab.Requests("", "") {
		if strings.HasPrefix(r.Path, "/projects/9") || strings.Contains(r.Path, "acme%2Fgadgets") {
			t.Errorf("a reconcile of the paused repository asked %s %s", r.Method, r.Path)
		}
	}

	// Resume: the same row is set up again, its dismissal stays, a scan is queued.
	pause(t, env, "acme/gadgets", true)
	reconcile(t, env)
	p, ok = project(t, env, 9)
	if !ok || p.HookID != newHookID || p.DeployTokenID != newDeployTokenID || len(p.DeployTokenSealed) == 0 {
		t.Fatalf("resumed project = %+v, %v; want a new hook and token", p, ok)
	}
	if ds, err := env.Store.Dismissals(env.ctx, mr); err != nil || len(ds) != 1 {
		t.Errorf("dismissals after the resume = %+v, %v; want the one kept", ds, err)
	}
	if _, active, err := env.Store.ReadinessScans(env.ctx, testInstance, "acme/gadgets"); err != nil || active == nil ||
		active.StartedBy != store.StartedBySwitchOn {
		t.Errorf("scan after the resume = %+v, %v; want one queued on switch-on", active, err)
	}
}

// Remove after a pause: the kept row and its dismissals go, with no platform
// call (the hook and token are gone already).
func TestReconcileRemovedAfterPause(t *testing.T) {
	env := newTestEnv(t)
	serveProject(env, testProjectID, testProjectPath)
	serveProject(env, 9, "acme/gadgets")
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", CreatedBy: "alice"})
	reconcile(t, env)
	mr := store.MRKey{Instance: testInstance, ProjectID: 9, IID: 1}
	if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{MR: mr, Fingerprint: "fp", Severity: "blocking", Reason: "fine", Actor: "maya"}); err != nil {
		t.Fatal(err)
	}
	pause(t, env, "acme/gadgets", false)
	env.GitLab.Reply(http.MethodDelete, "/projects/9/hooks/31", http.StatusNoContent, nil)
	env.GitLab.Reply(http.MethodDelete, "/projects/9/deploy_tokens/42", http.StatusNoContent, nil)
	reconcile(t, env)

	if found, err := env.Store.RemoveDashboardRepo(env.ctx, testInstance, "acme/gadgets"); err != nil || !found {
		t.Fatal(found, err)
	}
	env.GitLab.Reset()
	serveProject(env, testProjectID, testProjectPath)
	reconcile(t, env)
	if _, ok := project(t, env, 9); ok {
		t.Error("the removed repository's project row is still there")
	}
	if ds, err := env.Store.Dismissals(env.ctx, mr); err != nil || len(ds) != 0 {
		t.Errorf("dismissals after Remove = %+v, %v; want none", ds, err)
	}
	if n := len(env.GitLab.Requests(http.MethodDelete, "")); n != 0 {
		t.Errorf("%d GitLab deletes for a repository whose hook and token were gone", n)
	}
}

// A paused GitHub repository keeps its row as it is.
func TestReconcilePausedGitHubRepoKeepsRow(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	env.Engine.Config.Repositories = env.Engine.Config.Repositories[:1]
	serveProject(env, testProjectID, testProjectPath)
	dashboardRepo(t, env, store.DashboardRepo{Instance: ghInstance, Project: ghRepoPath, CreatedBy: "alice"})
	if _, _, err := env.Store.SetDashboardRepoEnabled(env.ctx, ghInstance, ghRepoPath, false); err != nil {
		t.Fatal(err)
	}
	before, ok := ghProject(t, env)
	if !ok {
		t.Fatal("no GitHub project to start with")
	}

	reconcile(t, env)

	after, ok := ghProject(t, env)
	if !ok || after.InstallationID != before.InstallationID || after.HTTPURLToRepo != before.HTTPURLToRepo {
		t.Fatalf("paused GitHub project = %+v, %v; want %+v kept", after, ok, before)
	}
	if n := len(gh.Requests("", "")); n != 0 {
		t.Errorf("%d GitHub requests for a paused repository", n)
	}
}

// The config takes over a paused dashboard row: the row is deleted.
func TestReconcileDropsPausedDashboardRowOfConfigRepo(t *testing.T) {
	env := newTestEnv(t)
	serveProject(env, testProjectID, testProjectPath)
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "ACME/Widgets", CreatedBy: "alice"})
	pause(t, env, "ACME/Widgets", false)

	reconcile(t, env)

	if _, found, err := env.Store.AnyDashboardRepo(env.ctx, testInstance, testProjectPath); err != nil || found {
		t.Errorf("the paused dashboard row of the config repository is still there (%v)", err)
	}
	if !strings.Contains(env.Logs(), "dashboard record") {
		t.Errorf("the takeover was not logged:\n%s", env.Logs())
	}
	if _, ok := project(t, env, testProjectID); !ok {
		t.Errorf("the config repository's project was removed")
	}
}

// Scan now on a paused repository is "not switched on".
func TestRequestScanPausedRepo(t *testing.T) {
	env := newScanEnv(t)
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", CreatedBy: "alice"})
	pause(t, env, "acme/gadgets", false)
	if _, err := env.Engine.RequestScan(env.ctx, testInstance, "acme/gadgets", "maya"); err != readiness.ErrNotSwitchedOn {
		t.Fatalf("RequestScan of a paused repository = %v; want ErrNotSwitchedOn", err)
	}
	if latest, active, err := env.Store.ReadinessScans(env.ctx, testInstance, "acme/gadgets"); err != nil || latest != nil || active != nil {
		t.Errorf("scans = %v, %v, %v; want none queued", latest, active, err)
	}
}
