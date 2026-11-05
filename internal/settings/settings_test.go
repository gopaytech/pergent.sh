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
	"github.com/gopaytech/pergent.sh/internal/instances"
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
			Timeout:  10 * time.Minute,
			MaxTurns: 40,
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

var allKeys = []string{KeyApprove, KeyReviewDrafts, KeyInlineFindings, KeyFailureStatus, KeySkills, KeyJobTimeout, KeyMaxTurns, KeyJobMemory,
	KeyRerunLimitPerMR, KeyRerunLimitPerRepo, KeyRepoSkills, KeyCommandsAuthorAndMaintainersOnly, KeyIncrementalReviews, KeyLearnFromDismissals}

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
	if !skip(KeyInlineFindings) && !eff.InlineFindings {
		t.Errorf("InlineFindings = false, want the default true")
	}
	if !skip(KeyCommandsAuthorAndMaintainersOnly) && !eff.CommandsAuthorAndMaintainersOnly {
		t.Errorf("CommandsAuthorAndMaintainersOnly = false, want the default true")
	}
	if !skip(KeyIncrementalReviews) && !eff.IncrementalReviews || !eff.LearnFromDismissals {
		t.Errorf("IncrementalReviews = false, want the default true")
	}
	if !skip(KeyLearnFromDismissals) && !eff.LearnFromDismissals {
		t.Errorf("LearnFromDismissals = false, want the default true")
	}
	if !skip(KeyFailureStatus) && eff.FailureStatus != config.FailureStatusFailed {
		t.Errorf("FailureStatus = %q, want the default failed", eff.FailureStatus)
	}
	if !skip(KeySkills) && eff.Skills != nil {
		t.Errorf("Skills = %v, want nil (the default list)", eff.Skills)
	}
	if !skip(KeyRepoSkills) && eff.RepoSkills != nil {
		t.Errorf("RepoSkills = %v, want nil (none turned on)", eff.RepoSkills)
	}
	if !skip(KeyJobTimeout) && eff.JobTimeout != 10*time.Minute {
		t.Errorf("JobTimeout = %v, want jobs.timeout 10m", eff.JobTimeout)
	}
	if !skip(KeyMaxTurns) && eff.MaxTurns != 40 {
		t.Errorf("MaxTurns = %d, want jobs.max_turns 40", eff.MaxTurns)
	}
	if !skip(KeyJobMemory) && eff.JobMemory.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Errorf("JobMemory = %s, want jobs.resources.limits.memory 4Gi", eff.JobMemory.String())
	}
	if !skip(KeyRerunLimitPerMR) && eff.RerunLimitPerMR != 10 {
		t.Errorf("RerunLimitPerMR = %d, want the default 10", eff.RerunLimitPerMR)
	}
	if !skip(KeyRerunLimitPerRepo) && eff.RerunLimitPerRepo != 50 {
		t.Errorf("RerunLimitPerRepo = %d, want the default 50", eff.RerunLimitPerRepo)
	}
}

func TestResolverConfigKeys(t *testing.T) {
	cfg := testConfig()
	cfg.Repositories = append(cfg.Repositories, config.Repository{
		GitHub: "gh", Project: "Acme/Payments",
		Approve: ptr(false), ReviewDrafts: ptr(false), InlineFindings: ptr(false),
		FailureStatus:   config.FailureStatusNeutral,
		Skills:          []string{"security"},
		RepoSkills:      []string{"team"},
		JobTimeout:      ptr(20 * time.Minute),
		MaxTurns:        ptr(120),
		JobMemory:       ptr(resource.MustParse("6Gi")),
		RerunLimitPerMR: ptr(20), RerunLimitPerRepo: ptr(100),
		CommandsAuthorAndMaintainersOnly: ptr(false), IncrementalReviews: ptr(false), LearnFromDismissals: ptr(false),
	})
	r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)} // no store: config only

	eff := get(t, r, "gh", "acme/payments")
	if eff.Platform != config.PlatformGitHub || eff.Instance != "gh" || eff.Project != "Acme/Payments" || eff.Source != SourceConfig {
		t.Errorf("identity = %s %s %s %s", eff.Platform, eff.Instance, eff.Project, eff.Source)
	}
	if eff.Approve || eff.ReviewDrafts || eff.InlineFindings || eff.CommandsAuthorAndMaintainersOnly || eff.IncrementalReviews || eff.LearnFromDismissals || eff.FailureStatus != config.FailureStatusNeutral ||
		!slices.Equal(eff.Skills, []string{"security"}) || !slices.Equal(eff.RepoSkills, []string{"team"}) ||
		eff.JobTimeout != 20*time.Minute || eff.MaxTurns != 120 || eff.JobMemory.Cmp(resource.MustParse("6Gi")) != 0 || eff.RerunLimitPerMR != 20 || eff.RerunLimitPerRepo != 100 {
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
	eff.Skills[0], eff.RepoSkills[0] = "changed", "changed"
	if cfg.Repositories[1].Skills[0] != "security" || cfg.Repositories[1].RepoSkills[0] != "team" {
		t.Errorf("Effective.Skills or RepoSkills aliases the config")
	}
}

func TestResolverConfigWins(t *testing.T) {
	st := newStore(t)
	upsert(t, st, store.DashboardRepo{Instance: "main", Project: "ACME/widgets", Approve: ptr(false), CreatedBy: "alice"})
	cfg := testConfig()
	r := &Resolver{Config: cfg, Store: st, Instances: instances.New(cfg, st, nil)}

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
	if _, err := st.CreateLibrarySkill(ctx, store.NewSkill{Name: "security", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	upsert(t, st, store.DashboardRepo{Instance: "main", Project: "acme/payments", Approve: ptr(false), CreatedBy: "alice"})
	upsert(t, st, store.DashboardRepo{
		Instance: "gh", Project: "Acme/Gadgets",
		Approve: ptr(true), ReviewDrafts: ptr(false), InlineFindings: ptr(false), FailureStatus: ptr("neutral"),
		Skills: []string{"security"}, RepoSkills: []string{"team"}, JobTimeoutSeconds: ptr(1200), MaxTurns: ptr(120), JobMemory: ptr("6Gi"),
		RerunLimitPerMR: ptr(20), RerunLimitPerRepo: ptr(100), CommandsAuthorAndMaintainersOnly: ptr(false), IncrementalReviews: ptr(false), LearnFromDismissals: ptr(false),
	})
	// A row of an instance no longer configured is not a repository.
	upsert(t, st, store.DashboardRepo{Instance: "gone", Project: "acme/old"})
	cfg := testConfig()
	r := &Resolver{Config: cfg, Store: st, Instances: instances.New(cfg, st, nil)}

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
	if !full.Approve || full.ReviewDrafts || full.InlineFindings || full.CommandsAuthorAndMaintainersOnly || full.IncrementalReviews || full.LearnFromDismissals || full.FailureStatus != config.FailureStatusNeutral ||
		!slices.Equal(full.Skills, []string{"security"}) || !slices.Equal(full.RepoSkills, []string{"team"}) ||
		full.JobTimeout != 20*time.Minute || full.MaxTurns != 120 || full.JobMemory.Cmp(resource.MustParse("6Gi")) != 0 || full.RerunLimitPerMR != 20 || full.RerunLimitPerRepo != 100 {
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
	r := &Resolver{Config: cfg, Store: st, Instances: instances.New(cfg, st, nil)}
	for _, memory := range []string{"lots", "0", "512Mi"} {
		upsert(t, st, store.DashboardRepo{Instance: "main", Project: "acme/payments", JobMemory: ptr(memory)})
		eff := get(t, r, "main", "acme/payments")
		wantDefaults(t, eff)
		wantOverridden(t, eff)
	}
}

func TestDefaults(t *testing.T) {
	cfg := &config.Config{Jobs: config.Jobs{Timeout: 7 * time.Minute}}
	got := (&Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}).Defaults(config.PlatformGitLab, "main")
	if !got.Approve || !got.ReviewDrafts || !got.InlineFindings || !got.CommandsAuthorAndMaintainersOnly || !got.IncrementalReviews || !got.LearnFromDismissals || got.FailureStatus != config.FailureStatusFailed ||
		got.JobTimeout != 7*time.Minute || !got.JobMemory.IsZero() || got.Skills != nil || got.Instance != "main" {
		t.Errorf("Defaults = %+v", got)
	}
	for k, v := range got.Overridden {
		if v {
			t.Errorf("%s is overridden", k)
		}
	}
}

// A resolver built without a registry knows the config's instances, as the
// dashboard does: it never panics on a nil one.
func TestResolverWithoutRegistry(t *testing.T) {
	cfg := testConfig()
	r := &Resolver{Config: cfg}
	got, err := r.List(ctx)
	if err != nil || len(got) != 1 || got[0].Project != "acme/widgets" || got[0].Platform != config.PlatformGitLab {
		t.Fatalf("List = %+v, %v; want acme/widgets on gitlab", got, err)
	}
	if eff, ok, err := r.Get(ctx, "main", "ACME/Widgets"); err != nil || !ok || eff.Instance != "main" {
		t.Errorf("Get = %+v, %v, %v", eff, ok, err)
	}
}

// The defaults set in the dashboard apply to the repositories switched on in
// the dashboard, under what a repository sets itself, and to no repository of
// the config.
func TestBrowserDefaultsApplyToDashboardReposOnly(t *testing.T) {
	st := newStore(t)
	cfg := testConfig()
	cfg.Jobs.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	r := &Resolver{Config: cfg, Store: st, Instances: instances.New(cfg, st, nil)}
	if err := st.PutRepoDefaults(ctx, store.RepoDefaults{
		Approve: ptr(false), ReviewDrafts: ptr(false), InlineFindings: ptr(false), FailureStatus: ptr("neutral"),
		JobTimeoutSeconds: ptr(1200), JobMemory: ptr("2Gi"),
		RerunLimitPerMR: ptr(30), RerunLimitPerRepo: ptr(200), CommandsAuthorAndMaintainersOnly: ptr(false), IncrementalReviews: ptr(false), LearnFromDismissals: ptr(false),
	}); err != nil {
		t.Fatal(err)
	}
	upsert(t, st, store.DashboardRepo{Instance: "main", Project: "acme/payments", CreatedBy: "alice"})
	upsert(t, st, store.DashboardRepo{Instance: "main", Project: "acme/gadgets", CreatedBy: "alice",
		Approve: ptr(true), InlineFindings: ptr(true), JobTimeoutSeconds: ptr(600), RerunLimitPerMR: ptr(5), CommandsAuthorAndMaintainersOnly: ptr(true), IncrementalReviews: ptr(true), LearnFromDismissals: ptr(true)})

	// A dashboard repository takes the defaults; none counts as overridden.
	eff := get(t, r, "main", "acme/payments")
	if eff.Approve || eff.ReviewDrafts || eff.InlineFindings || eff.CommandsAuthorAndMaintainersOnly || eff.IncrementalReviews || eff.LearnFromDismissals || eff.FailureStatus != config.FailureStatusNeutral ||
		eff.JobTimeout != 20*time.Minute || eff.JobMemory.Cmp(resource.MustParse("2Gi")) != 0 ||
		eff.RerunLimitPerMR != 30 || eff.RerunLimitPerRepo != 200 {
		t.Errorf("dashboard repository = %+v", eff)
	}
	wantOverridden(t, eff)

	// What it sets itself wins.
	eff = get(t, r, "main", "acme/gadgets")
	if !eff.Approve || eff.JobTimeout != 10*time.Minute || eff.ReviewDrafts || !eff.InlineFindings || !eff.CommandsAuthorAndMaintainersOnly || !eff.IncrementalReviews || !eff.LearnFromDismissals {
		t.Errorf("dashboard repository with its own settings = %+v", eff)
	}
	if eff.RerunLimitPerMR != 5 || eff.RerunLimitPerRepo != 200 {
		t.Errorf("dashboard repository caps = %d, %d; want 5 (its own), 200 (the default)", eff.RerunLimitPerMR, eff.RerunLimitPerRepo)
	}
	wantOverridden(t, eff, KeyApprove, KeyInlineFindings, KeyJobTimeout, KeyRerunLimitPerMR, KeyCommandsAuthorAndMaintainersOnly, KeyIncrementalReviews, KeyLearnFromDismissals)

	// A config repository keeps the config's defaults.
	eff = get(t, r, "main", "acme/widgets")
	wantDefaults(t, eff)
	wantOverridden(t, eff)

	// List agrees, and so does the view of the defaults.
	all, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if e.Source == SourceConfig && !e.Approve {
			t.Errorf("List: config repository %s has the browser defaults", e.Project)
		}
	}
	def, err := r.BrowserDefaults(ctx, config.PlatformGitLab, "main")
	if err != nil || def.Approve || def.JobTimeout != 20*time.Minute || def.Source != SourceDashboard {
		t.Errorf("BrowserDefaults = %+v, %v", def, err)
	}
	if got := r.Defaults(config.PlatformGitLab, "main"); !got.Approve || got.JobTimeout != 10*time.Minute {
		t.Errorf("Defaults (the config's) = %+v", got)
	}
}

// A stored default that can't be used (a memory limit below the Job's
// request) is ignored: the built-in one applies.
func TestBrowserDefaultsBadValuesFallBack(t *testing.T) {
	st := newStore(t)
	cfg := testConfig()
	cfg.Jobs.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	r := &Resolver{Config: cfg, Store: st, Instances: instances.New(cfg, st, nil)}
	if err := st.PutRepoDefaults(ctx, store.RepoDefaults{JobMemory: ptr("512Mi")}); err != nil {
		t.Fatal(err)
	}
	upsert(t, st, store.DashboardRepo{Instance: "main", Project: "acme/payments", CreatedBy: "alice"})
	wantDefaults(t, get(t, r, "main", "acme/payments"))
}

// A stored cap that isn't positive (the database refuses it, but a bad
// value never stops reviews) is ignored, in a repository and in the defaults.
func TestResolverIgnoresUnusableCaps(t *testing.T) {
	cfg := testConfig()
	r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}
	eff := r.defaultsFor(SourceDashboard, config.PlatformGitLab, "main", "acme/x",
		store.RepoDefaults{RerunLimitPerMR: ptr(0), RerunLimitPerRepo: ptr(-1)})
	wantDefaults(t, eff)
	eff, ok := r.fromDashboard(store.DashboardRepo{Instance: "main", Project: "acme/x", RerunLimitPerMR: ptr(0), RerunLimitPerRepo: ptr(-4)},
		store.RepoDefaults{})
	if !ok {
		t.Fatal("fromDashboard: instance not found")
	}
	wantDefaults(t, eff)
	wantOverridden(t, eff)
}

// max_turns: the config's jobs.max_turns unless the repository sets it, and
// for a dashboard repository the stored default in between.
func TestResolverMaxTurns(t *testing.T) {
	cfg := testConfig()
	cfg.Repositories = append(cfg.Repositories, config.Repository{GitHub: "gh", Project: "acme/set", MaxTurns: ptr(120)})
	r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}

	// A config repository: jobs.max_turns, or its own.
	plain := get(t, r, "main", "acme/widgets")
	if plain.MaxTurns != 40 || plain.Overridden[KeyMaxTurns] {
		t.Errorf("config repository without max_turns = %d, overridden %v; want 40, false", plain.MaxTurns, plain.Overridden[KeyMaxTurns])
	}
	set := get(t, r, "gh", "acme/set")
	if set.MaxTurns != 120 || !set.Overridden[KeyMaxTurns] {
		t.Errorf("config repository with max_turns 120 = %d, overridden %v; want 120, true", set.MaxTurns, set.Overridden[KeyMaxTurns])
	}

	// A dashboard repository: the row, over the stored default, over jobs.max_turns.
	row := store.DashboardRepo{Instance: "main", Project: "acme/x"}
	eff, ok := r.fromDashboard(row, store.RepoDefaults{})
	if !ok || eff.MaxTurns != 40 || eff.Overridden[KeyMaxTurns] {
		t.Errorf("no row value, no default = %d, overridden %v; want 40, false", eff.MaxTurns, eff.Overridden[KeyMaxTurns])
	}
	eff, _ = r.fromDashboard(row, store.RepoDefaults{MaxTurns: ptr(80)})
	if eff.MaxTurns != 80 || eff.Overridden[KeyMaxTurns] {
		t.Errorf("stored default 80 = %d, overridden %v; want 80, false", eff.MaxTurns, eff.Overridden[KeyMaxTurns])
	}
	row.MaxTurns = ptr(120)
	eff, _ = r.fromDashboard(row, store.RepoDefaults{MaxTurns: ptr(80)})
	if eff.MaxTurns != 120 || !eff.Overridden[KeyMaxTurns] {
		t.Errorf("row 120 over default 80 = %d, overridden %v; want 120, true", eff.MaxTurns, eff.Overridden[KeyMaxTurns])
	}
	for _, n := range []int{5, 200} {
		row.MaxTurns = ptr(n)
		if eff, _ = r.fromDashboard(row, store.RepoDefaults{}); eff.MaxTurns != n || !eff.Overridden[KeyMaxTurns] {
			t.Errorf("row %d = %d, overridden %v; want it used", n, eff.MaxTurns, eff.Overridden[KeyMaxTurns])
		}
	}
	if def := r.defaultsFor(SourceDashboard, config.PlatformGitLab, "main", "", store.RepoDefaults{MaxTurns: ptr(200)}); def.MaxTurns != 200 {
		t.Errorf("stored default 200 = %d; want 200", def.MaxTurns)
	}
}

// A stored step limit outside 5 to 200 (the database refuses it, but a
// value written by hand is possible) is ignored, in a repository and in the
// defaults: a bad setting never stops reviews.
func TestResolverIgnoresUnusableMaxTurns(t *testing.T) {
	cfg := testConfig()
	r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}
	for _, n := range []int{4, 201, 0, -1} {
		eff := r.defaultsFor(SourceDashboard, config.PlatformGitLab, "main", "acme/x", store.RepoDefaults{MaxTurns: ptr(n)})
		wantDefaults(t, eff)
		eff, ok := r.fromDashboard(store.DashboardRepo{Instance: "main", Project: "acme/x", MaxTurns: ptr(n)}, store.RepoDefaults{})
		if !ok {
			t.Fatal("fromDashboard: instance not found")
		}
		wantDefaults(t, eff)
		wantOverridden(t, eff)

		// A bad row value falls back to a usable stored default, not past it.
		eff, _ = r.fromDashboard(store.DashboardRepo{Instance: "main", Project: "acme/x", MaxTurns: ptr(n)}, store.RepoDefaults{MaxTurns: ptr(80)})
		if eff.MaxTurns != 80 || eff.Overridden[KeyMaxTurns] {
			t.Errorf("bad row %d over default 80 = %d, overridden %v; want 80, false", n, eff.MaxTurns, eff.Overridden[KeyMaxTurns])
		}
	}
}

// A config repository ignores the step limit stored as the dashboard's default.
func TestResolverConfigIgnoresStoredMaxTurns(t *testing.T) {
	st := newStore(t)
	cfg := testConfig()
	r := &Resolver{Config: cfg, Store: st, Instances: instances.New(cfg, st, nil)}
	if err := st.PutRepoDefaults(ctx, store.RepoDefaults{MaxTurns: ptr(80)}); err != nil {
		t.Fatal(err)
	}
	upsert(t, st, store.DashboardRepo{Instance: "main", Project: "acme/payments", CreatedBy: "alice"})

	eff := get(t, r, "main", "acme/widgets")
	if eff.MaxTurns != 40 || eff.Overridden[KeyMaxTurns] {
		t.Errorf("config repository = %d, overridden %v; want jobs.max_turns 40, false", eff.MaxTurns, eff.Overridden[KeyMaxTurns])
	}
	if eff := get(t, r, "main", "acme/payments"); eff.MaxTurns != 80 || eff.Overridden[KeyMaxTurns] {
		t.Errorf("dashboard repository = %d, overridden %v; want the stored 80, false", eff.MaxTurns, eff.Overridden[KeyMaxTurns])
	}
}
