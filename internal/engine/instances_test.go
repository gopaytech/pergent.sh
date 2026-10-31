package engine

import (
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
	env.SetNow(env.Now().Add(unconnectedDelay + time.Second))
	dispatch(t, env)
	if r := env.Run(id); r.State != store.Queued {
		t.Fatalf("run after the second pass = %s, want queued", r.State)
	}
	if warns() != 1 {
		t.Errorf("warnings = %d within a minute, want 1", warns())
	}

	env.SetNow(env.Now().Add(unconnectedDelay + time.Second))
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
