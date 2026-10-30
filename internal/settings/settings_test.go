package settings

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const testDBEnv = "PERGENT_SH_TEST_DATABASE_URL"

var ctx = context.Background()

// newStore opens a migrated store in a fresh schema, dropped when the test
// ends. It skips when no test database is set.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv(testDBEnv)
	if url == "" {
		t.Skipf("%s is not set", testDBEnv)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenInSchema(ctx, url, "test_settings_"+hex.EncodeToString(b))
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	t.Cleanup(func() {
		if err := st.DropSchema(context.Background()); err != nil {
			t.Errorf("DropSchema: %v", err)
		}
		st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// testConfig has GitLab instance "main", GitHub instance "gh" and the
// repository acme/widgets on main.
func testConfig() *config.Config {
	return &config.Config{
		GitLab:       []config.Instance{{Name: "main", URL: "https://gitlab.example.com"}},
		GitHub:       []config.GitHubInstance{{Name: "gh"}},
		Repositories: []config.Repository{{GitLab: "main", Project: "acme/widgets", FailureStatus: config.FailureStatusFailed}},
		Jobs: config.Jobs{
			Timeout: 10 * time.Minute,
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func upsert(t *testing.T, st *store.Store, r store.DashboardRepo) {
	t.Helper()
	if err := st.UpsertDashboardRepo(ctx, r); err != nil {
		t.Fatalf("UpsertDashboardRepo(%s): %v", r.Project, err)
	}
}

func get(t *testing.T, r *Resolver, instance, project string) Effective {
	t.Helper()
	eff, ok, err := r.Get(ctx, instance, project)
	if err != nil || !ok {
		t.Fatalf("Get(%s, %s) = %v, %v", instance, project, ok, err)
	}
	return eff
}

var allKeys = []string{KeyApprove, KeyReviewDrafts, KeyFailureStatus, KeySkills, KeyJobTimeout, KeyJobMemory}

// wantOverridden checks exactly the given keys are overridden.
func wantOverridden(t *testing.T, eff Effective, keys ...string) {
	t.Helper()
	for _, k := range allKeys {
		if eff.Overridden[k] != slices.Contains(keys, k) {
			t.Errorf("Overridden[%s] = %v, want %v", k, eff.Overridden[k], slices.Contains(keys, k))
		}
	}
}

// wantDefaults checks every setting has testConfig's default value.
func wantDefaults(t *testing.T, eff Effective, except ...string) {
	t.Helper()
	skip := func(k string) bool { return slices.Contains(except, k) }
	if !skip(KeyApprove) && !eff.Approve {
		t.Errorf("Approve = false, want the default true")
	}
	if !skip(KeyReviewDrafts) && !eff.ReviewDrafts {
		t.Errorf("ReviewDrafts = false, want the default true")
	}
	if !skip(KeyFailureStatus) && eff.FailureStatus != config.FailureStatusFailed {
		t.Errorf("FailureStatus = %q, want the default failed", eff.FailureStatus)
	}
	if !skip(KeySkills) && eff.Skills != nil {
		t.Errorf("Skills = %v, want nil (all library skills)", eff.Skills)
	}
	if !skip(KeyJobTimeout) && eff.JobTimeout != 10*time.Minute {
		t.Errorf("JobTimeout = %v, want jobs.timeout 10m", eff.JobTimeout)
	}
	if !skip(KeyJobMemory) && eff.JobMemory.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Errorf("JobMemory = %s, want jobs.resources.limits.memory 4Gi", eff.JobMemory.String())
	}
}

func TestResolverConfigKeys(t *testing.T) {
	cfg := testConfig()
	cfg.Repositories = append(cfg.Repositories, config.Repository{
		GitHub: "gh", Project: "Acme/Payments",
		Approve: ptr(false), ReviewDrafts: ptr(false),
		FailureStatus: config.FailureStatusNeutral,
		Skills:        []string{"security"},
		JobTimeout:    ptr(20 * time.Minute),
		JobMemory:     ptr(resource.MustParse("6Gi")),
	})
	r := &Resolver{Config: cfg} // no store: config only

	eff := get(t, r, "gh", "acme/payments")
	if eff.Platform != config.PlatformGitHub || eff.Instance != "gh" || eff.Project != "Acme/Payments" || eff.Source != SourceConfig {
		t.Errorf("identity = %s %s %s %s", eff.Platform, eff.Instance, eff.Project, eff.Source)
	}
	if eff.Approve || eff.ReviewDrafts || eff.FailureStatus != config.FailureStatusNeutral ||
		!slices.Equal(eff.Skills, []string{"security"}) || eff.JobTimeout != 20*time.Minute ||
		eff.JobMemory.Cmp(resource.MustParse("6Gi")) != 0 {
		t.Errorf("config keys not reflected: %+v", eff)
	}
	wantOverridden(t, eff, allKeys...)

	plain := get(t, r, "main", "ACME/widgets")
	if plain.Platform != config.PlatformGitLab || plain.Project != "acme/widgets" || plain.Source != SourceConfig {
		t.Errorf("plain identity = %+v", plain)
	}
	wantDefaults(t, plain)
	wantOverridden(t, plain)

	if _, ok, err := r.Get(ctx, "main", "acme/other"); ok || err != nil {
		t.Errorf("Get(acme/other) = %v, %v; want not found", ok, err)
	}
	if _, ok, _ := r.Get(ctx, "gh", "acme/widgets"); ok {
		t.Errorf("acme/widgets found on the wrong instance")
	}

	list, err := r.List(ctx)
	if err != nil || len(list) != 2 || list[0].Project != "acme/widgets" || list[1].Project != "Acme/Payments" {
		t.Fatalf("List = %+v, %v", list, err)
	}

	// The resolver hands out copies: changing one doesn't change config.
	eff.Skills[0] = "changed"
	if cfg.Repositories[1].Skills[0] != "security" {
		t.Errorf("Effective.Skills aliases the config")
	}
}

func TestResolverConfigWins(t *testing.T) {
	st := newStore(t)
	upsert(t, st, store.DashboardRepo{Instance: "main", Project: "ACME/widgets", Approve: ptr(false), CreatedBy: "alice"})
	r := &Resolver{Config: testConfig(), Store: st}

	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Source != SourceConfig || list[0].Project != "acme/widgets" || !list[0].Approve {
		t.Fatalf("List = %+v, want only the config repository", list)
	}
	if eff := get(t, r, "main", "Acme/Widgets"); eff.Source != SourceConfig || !eff.Approve {
		t.Errorf("Get = %+v, want the config repository", eff)
	}
	if !r.InConfig("main", "ACME/WIDGETS") || r.InConfig("main", "acme/payments") || r.InConfig("gh", "acme/widgets") {
		t.Errorf("InConfig is wrong")
	}
}

func TestResolverDefaultsAndOverrides(t *testing.T) {
	st := newStore(t)
	upsert(t, st, store.DashboardRepo{Instance: "main", Project: "acme/payments", Approve: ptr(false), CreatedBy: "alice"})
	upsert(t, st, store.DashboardRepo{
		Instance: "gh", Project: "Acme/Gadgets",
		Approve: ptr(true), ReviewDrafts: ptr(false), FailureStatus: ptr("neutral"),
		Skills: []string{"security"}, JobTimeoutSeconds: ptr(1200), JobMemory: ptr("6Gi"),
	})
	// A row of an instance no longer configured is not a repository.
	upsert(t, st, store.DashboardRepo{Instance: "gone", Project: "acme/old"})
	r := &Resolver{Config: testConfig(), Store: st}

	eff := get(t, r, "main", "ACME/Payments")
	if eff.Source != SourceDashboard || eff.Platform != config.PlatformGitLab || eff.Instance != "main" || eff.Project != "acme/payments" {
		t.Errorf("identity = %+v", eff)
	}
	if eff.Approve {
		t.Errorf("Approve = true, want false")
	}
	wantDefaults(t, eff, KeyApprove)
	wantOverridden(t, eff, KeyApprove)

	full := get(t, r, "gh", "acme/gadgets")
	if full.Platform != config.PlatformGitHub || full.Project != "Acme/Gadgets" {
		t.Errorf("identity = %+v", full)
	}
	if !full.Approve || full.ReviewDrafts || full.FailureStatus != config.FailureStatusNeutral ||
		!slices.Equal(full.Skills, []string{"security"}) || full.JobTimeout != 20*time.Minute ||
		full.JobMemory.Cmp(resource.MustParse("6Gi")) != 0 {
		t.Errorf("overrides not applied: %+v", full)
	}
	wantOverridden(t, full, allKeys...)

	if _, ok, err := r.Get(ctx, "gone", "acme/old"); ok || err != nil {
		t.Errorf("Get(gone) = %v, %v; want not found", ok, err)
	}

	list, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range list {
		got = append(got, e.Instance+":"+e.Project+":"+string(e.Source))
	}
	want := []string{"main:acme/widgets:config", "gh:Acme/Gadgets:dashboard", "main:acme/payments:dashboard"}
	if !slices.Equal(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

func TestResolverBadDashboardMemoryFallsBack(t *testing.T) {
	st := newStore(t)
	cfg := testConfig()
	cfg.Jobs.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	r := &Resolver{Config: cfg, Store: st}
	for _, memory := range []string{"lots", "0", "512Mi"} {
		upsert(t, st, store.DashboardRepo{Instance: "main", Project: "acme/payments", JobMemory: ptr(memory)})
		eff := get(t, r, "main", "acme/payments")
		wantDefaults(t, eff)
		wantOverridden(t, eff)
	}
}
