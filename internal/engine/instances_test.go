package engine

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// An engine built without a registry knows the config's instances.
func TestEngineWithoutRegistry(t *testing.T) {
	cl := gitlab.New("https://gitlab.example.com", "t", nil)
	e := &Engine{
		Config:    testConfig("https://gitlab.example.com"),
		Platforms: syncmap.New(map[string]Platform{testInstance: {Kind: config.PlatformGitLab, GitLab: cl}}),
	}
	if got := e.gitLab(testInstance); got != cl {
		t.Errorf("gitLab(%q) = %v, want the review client", testInstance, got)
	}
	if e.gitLab("other") != nil {
		t.Error("gitLab(other) is set, want nil")
	}
}

// registerLab adds the instance "lab" of the dashboard to the registry, with a
// repository switched on there, and queues a run of it. The engine has no
// platform for it: pergent.sh couldn't connect to it at startup.
func registerLab(t *testing.T, env *testEnv) string {
	t.Helper()
	env.Engine.Instances.Put(instances.Instance{Name: "lab", Platform: config.PlatformGitLab, Source: instances.SourceDashboard,
		GitLab: &config.Instance{Name: "lab", URL: "https://gitlab.lab.example.com"}})
	dashboardRepo(t, env, store.DashboardRepo{Instance: "lab", Project: "acme/gadgets", CreatedBy: "alice"})
	id, _, err := env.Store.EnqueueRun(env.ctx, store.MRKey{Instance: "lab", ProjectID: 4, IID: 1}, "acme/gadgets", sha('a'), "main")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A queued run of a registered instance pergent.sh isn't connected to waits:
// it is neither superseded nor failed, nothing is launched or posted, and it
// goes on when the instance is connected. The wait is logged at Warn at most
// once a minute per instance.
func TestDispatchWaitsForAnUnconnectedInstance(t *testing.T) {
	env := newTestEnv(t)
	id := registerLab(t, env)

	dispatch(t, env)
	if r := env.Run(id); r.State != store.Queued || r.Attempts != 0 {
		t.Fatalf("run = state %s attempts %d; want queued, no attempt", r.State, r.Attempts)
	}
	if len(env.Jobs()) != 0 {
		t.Errorf("a job was launched")
	}
	warns := func() int { return strings.Count(env.Logs(), "isn't connected to their instance") }
	if warns() != 1 {
		t.Fatalf("warnings = %d, want 1:\n%s", warns(), env.Logs())
	}

	// Looked at again 30 s later, still within the minute: no new warning.
	env.SetNow(env.Now().Add(waitDelay + time.Second))
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Queued {
		t.Fatalf("run after the second pass = %s, want queued", r.State)
	}
	if warns() != 1 {
		t.Errorf("warnings = %d within a minute, want 1", warns())
	}

	env.SetNow(env.Now().Add(waitDelay + time.Second))
	dispatch(t, env)
	if warns() != 2 {
		t.Errorf("warnings = %d after a minute, want 2", warns())
	}
	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("GitLab got %d requests for an unconnected instance", n)
	}
}

// Only an instance that is gone, or going, drops its queued runs.
func TestDispatchSupersedesRunsOfAnUnregisteredOrRemovingInstance(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(env *testEnv)
	}{
		{"removing", func(env *testEnv) {
			env.Engine.Instances.Put(instances.Instance{Name: "lab", Platform: config.PlatformGitLab, Source: instances.SourceDashboard,
				Removing: true, GitLab: &config.Instance{Name: "lab"}})
		}},
		{"unregistered", func(env *testEnv) { env.Engine.Instances.Remove("lab") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			id := registerLab(t, env)
			tc.edit(env)
			dispatch(t, env)
			if r := env.Run(id); r.State != store.Superseded {
				t.Errorf("run = %s, want superseded", r.State)
			}
		})
	}
}

// With no LLM set, dispatch claims nothing: the run stays queued, no Job or
// status appears, and the wait is logged at Warn once a minute. Once an
// operator saves an LLM, the next pass dispatches with that model and key.
func TestDispatchWaitsWithoutLLM(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Config.LLM = config.LLM{}
	head := sha('a')
	id := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)

	const wait = "no LLM is set: an operator sets it on Admin → Settings"
	dispatch(t, env)
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Queued || r.Attempts != 0 {
		t.Fatalf("run = state %s attempts %d; want queued, no attempt", r.State, r.Attempts)
	}
	if n := len(env.Jobs()); n != 0 {
		t.Errorf("%d jobs launched without an LLM", n)
	}
	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("GitLab got %d requests without an LLM", n)
	}
	if n := strings.Count(env.Logs(), wait); n != 1 {
		t.Errorf("the wait was logged %d times in a minute, want once:\n%s", n, env.Logs())
	}
	env.SetNow(env.Now().Add(61 * time.Second))
	dispatch(t, env)
	if n := strings.Count(env.Logs(), wait); n != 2 {
		t.Errorf("the wait was logged %d times after a minute, want twice", n)
	}

	if err := env.Store.PutLLM(env.ctx, "openai", "gpt-example", "https://llm.example.com/v1", env.Box.Seal([]byte("llm-key-new")), env.Now()); err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.Instances.LoadLLM(env.ctx); err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job after the LLM was set:\n%s", env.Logs())
	}
	if got := string(env.Secret(id).Data["llm-api-key"]); got != "llm-key-new" {
		t.Errorf("the Job's key = %q, want the new one", got)
	}
	vars := map[string]string{}
	for _, v := range env.Job(id).Spec.Template.Spec.Containers[0].Env {
		vars[v.Name] = v.Value
	}
	if vars["OPENCODE_PROVIDER"] != "openai" || vars["OPENCODE_MODEL"] != "gpt-example" || vars["OPENCODE_BASE_URL"] != "https://llm.example.com/v1" {
		t.Errorf("the Job's LLM env = %v", vars)
	}
	if r := env.Run(id); r.Model == nil || *r.Model != "gpt-example" {
		t.Errorf("run model = %v, want gpt-example", r.Model)
	}
	if strings.Contains(env.Logs(), "llm-key-new") {
		t.Errorf("the key reached the log:\n%s", env.Logs())
	}
}

// Every LLM key the engine has held is hidden from text bound for a log or a
// stored reason, the one in use and those it replaced.
func TestRedactsEveryLLMKeySeen(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Config.LLM = config.LLM{}
	save := func(key string) {
		t.Helper()
		if err := env.Store.PutLLM(env.ctx, "openai", "m", "", env.Box.Seal([]byte(key)), env.Now()); err != nil {
			t.Fatal(err)
		}
		if err := env.Engine.Instances.LoadLLM(env.ctx); err != nil {
			t.Fatal(err)
		}
	}
	save("llm-key-first")
	save("llm-key-second")
	got := env.Engine.redact("a llm-key-first b llm-key-second c")
	if got != "a *** b *** c" {
		t.Errorf("redact = %q, want both keys hidden", got)
	}
}

// removingLab stores the instance "lab" as an operator removing it: its row
// and a secret in the store, Removing in the registry, and a project of it.
func removingLab(t *testing.T, env *testEnv) {
	t.Helper()
	if err := env.Store.CreateInstance(env.ctx, store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, URL: "https://gitlab.lab.example.com"},
		map[string][]byte{"review_token": env.Box.Seal([]byte("lab-review-token"))}, env.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := env.Store.MarkInstanceRemoving(env.ctx, "lab"); err != nil || !ok {
		t.Fatalf("MarkInstanceRemoving = %v, %v", ok, err)
	}
	if err := env.Engine.Instances.Load(env.ctx); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.UpsertProject(env.ctx, store.Project{Instance: "lab", ProjectID: 4, Path: "acme/gadgets"}); err != nil {
		t.Fatal(err)
	}
}

// An instance being removed is deleted once its projects are gone from the
// store, not before; then its secrets go, the registry forgets it and the
// hook is told. Other instances are left alone.
func TestFinishRemovals(t *testing.T) {
	env := newTestEnv(t)
	removingLab(t, env)
	var gone []string
	env.Engine.OnInstanceRemoved = func(name string) { gone = append(gone, name) }

	env.Engine.finishRemovals(env.ctx)
	if _, ok := env.Engine.Instances.Get("lab"); !ok || len(gone) != 0 {
		t.Fatalf("lab was removed with a project left (hook calls %v)", gone)
	}

	if err := env.Store.DeleteProject(env.ctx, "lab", 4); err != nil {
		t.Fatal(err)
	}
	// An account made by a sign-in that raced the removal goes with it.
	if _, err := env.Store.UpsertPlatformAccount(env.ctx, config.PlatformGitLab, "lab", 9, "alice", env.Now()); err != nil {
		t.Fatal(err)
	}
	env.Engine.finishRemovals(env.ctx)

	if _, ok := env.Engine.Instances.Get("lab"); ok {
		t.Error("the registry still has lab")
	}
	if _, found, err := env.Store.InstanceRow(env.ctx, "lab"); err != nil || found {
		t.Errorf("InstanceRow(lab) = %v, %v; want gone", found, err)
	}
	if secrets, err := env.Store.InstanceSecrets(env.ctx, "lab"); err != nil || len(secrets) != 0 {
		t.Errorf("InstanceSecrets(lab) = %v, %v; want none", secrets, err)
	}
	if accounts, err := env.Store.Accounts(env.ctx); err != nil || len(accounts) != 0 {
		t.Errorf("accounts = %v, %v; want the straggler gone", accounts, err)
	}
	if len(gone) != 1 || gone[0] != "lab" {
		t.Errorf("OnInstanceRemoved calls = %v, want [lab]", gone)
	}
	if _, ok := env.Engine.Instances.Get(testInstance); !ok {
		t.Error("the config instance is gone")
	}
	env.Engine.finishRemovals(env.ctx) // nothing left to do
	if len(gone) != 1 {
		t.Errorf("the hook ran again: %v", gone)
	}
}

// A reconcile takes the webhook and deploy token of a removed instance's
// project off its platform, then finishes the removal. One pergent.sh isn't
// connected to can't be reached: its project rows are forgotten with a
// warning, so the removal doesn't wait for ever.
func TestReconcileFinishesRemoval(t *testing.T) {
	env := newTestEnv(t)
	serveProject(env, testProjectID, testProjectPath)
	removingLab(t, env)
	env.Engine.Platforms.Delete("lab")
	var gone []string
	env.Engine.OnInstanceRemoved = func(name string) { gone = append(gone, name) }

	if err := env.Engine.Reconcile(env.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := env.Engine.Instances.Get("lab"); ok {
		t.Errorf("lab is still registered:\n%s", env.Logs())
	}
	if projects, _ := env.Store.Projects(env.ctx); len(projects) != 1 || projects[0].Instance != testInstance {
		t.Errorf("projects = %+v, want the main one only", projects)
	}
	if !strings.Contains(env.Logs(), "forgot a project of an instance being removed") || len(gone) != 1 {
		t.Errorf("logs / hook = %v\n%s", gone, env.Logs())
	}
}
