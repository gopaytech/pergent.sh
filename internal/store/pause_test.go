package store

import (
	"errors"
	"slices"
	"testing"
)

// A paused row is not "switched on": the safe reads skip it, the all-rows
// reads return it with Enabled false, and its settings stay.
func TestPausedDashboardRepo(t *testing.T) {
	s := newStore(t)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/widgets", Approve: ptrTo(false), CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/gadgets", CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := s.DashboardRepo(ctx, "main", "acme/widgets"); !r.Enabled {
		t.Fatalf("a new row = %+v; want enabled", r)
	}

	found, changed, err := s.SetDashboardRepoEnabled(ctx, "main", "ACME/Widgets", false)
	if err != nil || !found || !changed {
		t.Fatalf("pause = %v, %v, %v; want found and changed", found, changed, err)
	}
	found, changed, err = s.SetDashboardRepoEnabled(ctx, "main", "acme/widgets", false)
	if err != nil || !found || changed {
		t.Fatalf("pause again = %v, %v, %v; want found, not changed", found, changed, err)
	}

	on, err := s.DashboardRepos(ctx)
	if err != nil || len(on) != 1 || on[0].Project != "acme/gadgets" || !on[0].Enabled {
		t.Fatalf("DashboardRepos = %+v, %v; want acme/gadgets alone", on, err)
	}
	if _, found, err := s.DashboardRepo(ctx, "main", "acme/widgets"); err != nil || found {
		t.Fatalf("DashboardRepo of a paused row = %v, %v; want not found", found, err)
	}
	all, err := s.AllDashboardRepos(ctx)
	if err != nil || len(all) != 2 || all[0].Project != "acme/gadgets" || !all[0].Enabled ||
		all[1].Project != "acme/widgets" || all[1].Enabled {
		t.Fatalf("AllDashboardRepos = %+v, %v; want gadgets on, widgets paused", all, err)
	}
	r, found, err := s.AnyDashboardRepo(ctx, "main", "acme/WIDGETS")
	if err != nil || !found || r.Enabled || r.Approve == nil || *r.Approve {
		t.Fatalf("AnyDashboardRepo = %+v, %v, %v; want paused with approve off", r, found, err)
	}

	// Settings edits work on a paused row and leave it paused.
	if found, err := s.UpdateDashboardRepo(ctx, "main", "acme/widgets", func(r *DashboardRepo) { r.QuietReviews = ptrTo(true) }); err != nil || !found {
		t.Fatalf("UpdateDashboardRepo of a paused row = %v, %v", found, err)
	}
	if r, _, _ := s.AnyDashboardRepo(ctx, "main", "acme/widgets"); r.Enabled || r.QuietReviews == nil || !*r.QuietReviews {
		t.Fatalf("after the edit = %+v; want paused with quiet reviews on", r)
	}
	// Neither insert nor upsert switches it on.
	if created, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/widgets", CreatedBy: "bob"}); err != nil || created {
		t.Fatalf("insert over a paused row = %v, %v; want not created", created, err)
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/widgets", Approve: ptrTo(false), QuietReviews: ptrTo(true)}); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := s.AnyDashboardRepo(ctx, "main", "acme/widgets"); r.Enabled {
		t.Fatalf("after insert and upsert = %+v; want still paused", r)
	}

	found, changed, err = s.SetDashboardRepoEnabled(ctx, "main", "acme/widgets", true)
	if err != nil || !found || !changed {
		t.Fatalf("resume = %v, %v, %v", found, changed, err)
	}
	r, found, err = s.DashboardRepo(ctx, "main", "acme/widgets")
	if err != nil || !found || !r.Enabled || r.Approve == nil || *r.Approve || r.QuietReviews == nil || !*r.QuietReviews {
		t.Fatalf("after resume = %+v, %v, %v; want on with its settings", r, found, err)
	}
	found, changed, err = s.SetDashboardRepoEnabled(ctx, "main", "acme/missing", false)
	if err != nil || found || changed {
		t.Fatalf("pause of a missing row = %v, %v, %v; want nothing", found, changed, err)
	}
}

// Remove deletes the row and the team links to it, in any case, and leaves
// whole-instance links and links to the same path on another instance.
func TestRemoveDashboardRepo(t *testing.T) {
	s := newStore(t)
	for _, p := range []string{"Acme/Widgets", "acme/gadgets"} {
		if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: p}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.SetDashboardRepoEnabled(ctx, "main", "acme/widgets", false); err != nil {
		t.Fatal(err)
	}
	tm, err := s.CreateTeam(ctx, "devs", t0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error {
		t.Repos = []TeamRepo{{Instance: "main", Project: "acme/widgets"}, {Instance: "main"},
			{Instance: "gh", Project: "acme/widgets"}, {Instance: "main", Project: "acme/gadgets"}}
		return nil
	}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	teamRepos := func() []TeamRepo {
		t.Helper()
		got, found, err := s.Team(ctx, tm.ID)
		if err != nil || !found {
			t.Fatal(found, err)
		}
		return got.Repos
	}

	// A paused row.
	if found, err := s.RemoveDashboardRepo(ctx, "main", "ACME/WIDGETS"); err != nil || !found {
		t.Fatalf("remove = %v, %v; want found", found, err)
	}
	if _, found, _ := s.AnyDashboardRepo(ctx, "main", "acme/widgets"); found {
		t.Error("the paused row is still there")
	}
	want := []TeamRepo{{Instance: "gh", Project: "acme/widgets"}, {Instance: "main"}, {Instance: "main", Project: "acme/gadgets"}}
	if got := teamRepos(); !slices.Equal(got, want) {
		t.Errorf("team repos = %+v; want %+v", got, want)
	}
	if found, err := s.RemoveDashboardRepo(ctx, "main", "acme/widgets"); err != nil || found {
		t.Errorf("remove again = %v, %v; want not found", found, err)
	}

	// A running row.
	if found, err := s.RemoveDashboardRepo(ctx, "main", "acme/gadgets"); err != nil || !found {
		t.Fatalf("remove of a running row = %v, %v", found, err)
	}
	if _, found, _ := s.AnyDashboardRepo(ctx, "main", "acme/gadgets"); found {
		t.Error("the running row is still there")
	}
	want = []TeamRepo{{Instance: "gh", Project: "acme/widgets"}, {Instance: "main"}}
	if got := teamRepos(); !slices.Equal(got, want) {
		t.Errorf("team repos = %+v; want %+v", got, want)
	}
}

// The enabled column is NOT NULL and defaults to true.
func TestDashboardReposEnabledColumn(t *testing.T) {
	s := newStore(t)
	var nullable, def string
	if err := s.pool.QueryRow(ctx, `SELECT is_nullable, column_default FROM information_schema.columns
		WHERE table_name = 'dashboard_repos' AND column_name = 'enabled'`).Scan(&nullable, &def); err != nil {
		t.Fatalf("dashboard_repos.enabled: %v", err)
	}
	if nullable != "NO" || def != "true" {
		t.Errorf("enabled is_nullable = %q, default = %q; want NO, true", nullable, def)
	}
}

// A paused repository's project row is not scanned; resumed, it is.
func TestUnscannedProjectsSkipsPaused(t *testing.T) {
	s := newStore(t)
	for _, p := range []Project{{Instance: "main", ProjectID: 1, Path: "acme/paused"}, {Instance: "cfg", ProjectID: 2, Path: "acme/config"}} {
		if err := s.UpsertProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "Acme/Paused"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetDashboardRepoEnabled(ctx, "main", "acme/paused", false); err != nil {
		t.Fatal(err)
	}
	got, err := s.UnscannedProjects(ctx)
	if err != nil || !slices.Equal(got, []ProjectRef{{Instance: "cfg", Project: "acme/config"}}) {
		t.Fatalf("unscanned while paused = %v, %v; want the config project alone", got, err)
	}
	if _, _, err := s.SetDashboardRepoEnabled(ctx, "main", "acme/paused", true); err != nil {
		t.Fatal(err)
	}
	got, err = s.UnscannedProjects(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("unscanned after resume = %v, %v; want both", got, err)
	}
}

// A paused repository still counts as using its skills, so a resume never
// meets a missing one.
func TestDeleteInUseByPausedRepo(t *testing.T) {
	s := newStore(t)
	newSkill(t, s, "a", "x")
	newModel(t, s, "anthropic", "sonnet", "", nil) // the first is the default
	m := newModel(t, s, "openai", "gpt", "", nil)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", Skills: []string{"a"}, ModelID: &m.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetDashboardRepoEnabled(ctx, "lab", "acme/web", false); err != nil {
		t.Fatal(err)
	}
	var skillInUse *SkillInUseError
	if err := s.DeleteLibrarySkill(ctx, "a", nil); !errors.As(err, &skillInUse) ||
		!slices.Equal(skillInUse.Repos, []RepoRef{{Instance: "lab", Project: "acme/web"}}) {
		t.Errorf("delete of a skill a paused repository names = %#v; want in use by it", err)
	}
	// A model goes even when a repository names it (its chain skips it as
	// removed), paused or not.
	if err := s.DeleteLLMModel(ctx, m.ID, false); err != nil {
		t.Errorf("delete of a model a paused repository names = %v; want it removed", err)
	}
}
