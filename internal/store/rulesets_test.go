package store

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func newRuleSet(t *testing.T, s *Store, name, content string) {
	t.Helper()
	if err := s.CreateRuleSet(ctx, name, content, t0); err != nil {
		t.Fatalf("CreateRuleSet(%s): %v", name, err)
	}
}

const ruleYAML = "rules:\n  - path: \"*.go\"\n    rule: check\n"

func TestRuleSetCRUD(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	newRuleSet(t, s, "go", ruleYAML)
	newRuleSet(t, s, "api", "rules: []\n")

	got, err := s.RuleSet(ctx, "go")
	if err != nil || got.Name != "go" || got.Content != ruleYAML || got.Version != 1 || !got.CreatedAt.Equal(t0) || !got.UpdatedAt.Equal(t0) {
		t.Fatalf("RuleSet = %+v, %v", got, err)
	}
	v, err := s.UpdateRuleSet(ctx, "go", "rules: []\n", 1, t0.Add(time.Hour))
	if err != nil || v != 2 {
		t.Fatalf("UpdateRuleSet = %d, %v; want 2", v, err)
	}
	if _, err := s.UpdateRuleSet(ctx, "go", "x", 1, t0); !errors.Is(err, ErrRuleSetVersion) {
		t.Errorf("a stale update = %v, want ErrRuleSetVersion", err)
	}
	if _, err := s.UpdateRuleSet(ctx, "gone", "x", 1, t0); !errors.Is(err, ErrRuleSetNotFound) {
		t.Errorf("updating a missing set = %v, want ErrRuleSetNotFound", err)
	}
	got, _ = s.RuleSet(ctx, "go")
	if got.Version != 2 || got.Content != "rules: []\n" || !got.UpdatedAt.Equal(t0.Add(time.Hour)) || !got.CreatedAt.Equal(t0) {
		t.Errorf("after update = %+v", got)
	}
	list, err := s.RuleSets(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "api" || list[1].Name != "go" {
		t.Errorf("RuleSets = %+v, %v; want api then go", list, err)
	}
	if _, err := s.RuleSet(ctx, "gone"); !errors.Is(err, ErrRuleSetNotFound) {
		t.Errorf("RuleSet(gone) = %v", err)
	}
}

func TestRuleSetCreateTaken(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	newRuleSet(t, s, "go", ruleYAML)
	if err := s.CreateRuleSet(ctx, "go", ruleYAML, t0); !errors.Is(err, ErrRuleSetExists) {
		t.Errorf("a taken name = %v, want ErrRuleSetExists", err)
	}
}

func TestRuleSetLimit(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for i := range MaxRuleSets {
		newRuleSet(t, s, fmt.Sprintf("set-%03d", i), ruleYAML)
	}
	if err := s.CreateRuleSet(ctx, "one-too-many", ruleYAML, t0); !errors.Is(err, ErrRuleSetsFull) {
		t.Errorf("the 201st = %v, want ErrRuleSetsFull", err)
	}
	if err := s.DeleteRuleSet(ctx, "set-000", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRuleSet(ctx, "one-too-many", ruleYAML, t0); err != nil {
		t.Errorf("create after a delete: %v", err)
	}
}

func TestRuleSetDeleteInUse(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		newRuleSet(t, s, n, ruleYAML)
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", RuleSets: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRepoDefaults(ctx, RepoDefaults{RuleSets: []string{"b", "c"}}); err != nil {
		t.Fatal(err)
	}
	cfg := []RepoRef{{Instance: "lab", Project: "acme/api"}}

	var inUse *RuleSetInUseError
	err := s.DeleteRuleSet(ctx, "a", cfg)
	if !errors.As(err, &inUse) || inUse.InDefaults || !slices.Equal(inUse.Repos, []RepoRef{{"lab", "acme/web"}, {"lab", "acme/api"}}) {
		t.Errorf("delete a = %v (%+v)", err, inUse)
	}
	err = s.DeleteRuleSet(ctx, "c", nil)
	if !errors.As(err, &inUse) || !inUse.InDefaults || len(inUse.Repos) != 0 {
		t.Errorf("delete c = %v (%+v)", err, inUse)
	}
	if err := s.DeleteRuleSet(ctx, "d", nil); err != nil {
		t.Errorf("delete d (free) = %v", err)
	}
	if _, err := s.RuleSet(ctx, "d"); !errors.Is(err, ErrRuleSetNotFound) {
		t.Errorf("d still there: %v", err)
	}
	if err := s.DeleteRuleSet(ctx, "gone", nil); !errors.Is(err, ErrRuleSetNotFound) {
		t.Errorf("delete gone = %v", err)
	}
}

func TestResolveRuleSets(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	newRuleSet(t, s, "a", ruleYAML)
	newRuleSet(t, s, "b", "rules: []\n")
	got, err := s.ResolveRuleSets(ctx, []string{"b", "gone", "a", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sets) != 2 || got.Sets["a"].Content != ruleYAML || !slices.Equal(got.Missing, []string{"gone"}) {
		t.Errorf("ResolveRuleSets = %+v", got)
	}
	if err := s.PutRepoDefaults(ctx, RepoDefaults{RuleSets: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.ResolveRuleSets(ctx, nil)
	if err != nil || len(got.Sets) != 1 || got.Sets["b"].Name != "b" || !slices.Equal(got.Names, []string{"b"}) {
		t.Errorf("the default list = %+v, %v", got, err)
	}
	got, err = s.ResolveRuleSets(ctx, []string{"b", "a"})
	if err != nil || !slices.Equal(got.Names, []string{"b", "a"}) {
		t.Errorf("Names = %v, want the order given", got.Names)
	}
}

// A save of a list that names a set and a delete of the set can't both
// succeed.
func TestRuleSetDeleteRacesSave(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for i := range 20 {
		name := fmt.Sprintf("s%d", i)
		newRuleSet(t, s, name, ruleYAML)
		var wg sync.WaitGroup
		var saveErr, delErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			saveErr = s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", RuleSets: []string{name}})
		}()
		go func() { defer wg.Done(); delErr = s.DeleteRuleSet(ctx, name, nil) }()
		wg.Wait()
		if saveErr == nil && delErr == nil {
			t.Fatalf("round %d: both the save and the delete of %s succeeded", i, name)
		}
	}
}

func TestDashboardRepoRuleSetsRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	newRuleSet(t, s, "a", ruleYAML)
	newRuleSet(t, s, "b", ruleYAML)
	for _, tt := range []struct{ sets, builtIn []string }{
		{nil, nil}, {[]string{"b", "a"}, []string{"yaml", "go"}}, {[]string{"a"}, []string{}}, {[]string{"a"}, []string{"go"}},
	} {
		if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", RuleSets: tt.sets, BuiltInRules: tt.builtIn}); err != nil {
			t.Fatal(err)
		}
		got, _, err := s.AnyDashboardRepo(ctx, "lab", "acme/web")
		// An empty list is stored as NULL and read back as nil: none.
		if err != nil || !slices.Equal(got.RuleSets, tt.sets) || !slices.Equal(got.BuiltInRules, tt.builtIn) || (len(tt.builtIn) == 0 && got.BuiltInRules != nil) {
			t.Errorf("stored %v %v, read %v %v (%v)", tt.sets, tt.builtIn, got.RuleSets, got.BuiltInRules, err)
		}
	}
	found, err := s.UpdateDashboardRepo(ctx, "lab", "acme/web", func(r *DashboardRepo) { r.RuleSets, r.BuiltInRules = []string{"b"}, []string{"sql"} })
	if err != nil || !found {
		t.Fatalf("UpdateDashboardRepo = %v, %v", found, err)
	}
	got, _, _ := s.AnyDashboardRepo(ctx, "lab", "acme/web")
	if !slices.Equal(got.RuleSets, []string{"b"}) || !slices.Equal(got.BuiltInRules, []string{"sql"}) {
		t.Errorf("after update: %v %v", got.RuleSets, got.BuiltInRules)
	}
	created, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/api", RuleSets: []string{"a"}, BuiltInRules: []string{"go"}})
	if err != nil || !created {
		t.Fatalf("InsertDashboardRepo = %v, %v", created, err)
	}
	got, _, _ = s.AnyDashboardRepo(ctx, "lab", "acme/api")
	if !slices.Equal(got.RuleSets, []string{"a"}) || !slices.Equal(got.BuiltInRules, []string{"go"}) {
		t.Errorf("inserted: %v %v", got.RuleSets, got.BuiltInRules)
	}
	// The old switch is left as it was: the previous release reads it after
	// a rollback.
	var old *bool
	if err := s.pool.QueryRow(ctx, `SELECT built_in_rules FROM dashboard_repos WHERE project = 'acme/api'`).Scan(&old); err != nil || old != nil {
		t.Errorf("built_in_rules = %v, %v; want NULL, untouched", old, err)
	}
}

func TestRepoDefaultsRuleSets(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	newRuleSet(t, s, "a", ruleYAML)
	if err := s.PutRepoDefaults(ctx, RepoDefaults{RuleSets: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	d, err := s.RepoDefaults(ctx)
	if err != nil || !slices.Equal(d.RuleSets, []string{"a"}) {
		t.Errorf("RepoDefaults = %v, %v", d.RuleSets, err)
	}
	if err := s.PutRepoDefaults(ctx, RepoDefaults{}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.RepoDefaults(ctx); d.RuleSets != nil {
		t.Errorf("cleared defaults = %v", d.RuleSets)
	}
}

// The old default switch is neither read nor written: a value the previous
// release saved stays for it after a rollback.
func TestRepoDefaultsLeaveOldBuiltInSwitch(t *testing.T) {
	s := newStore(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO repo_defaults (id, built_in_rules, updated_at) VALUES (1, true, now())`); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRepoDefaults(ctx, RepoDefaults{}); err != nil {
		t.Fatal(err)
	}
	var on *bool
	if err := s.pool.QueryRow(ctx, `SELECT built_in_rules FROM repo_defaults WHERE id = 1`).Scan(&on); err != nil || on == nil || !*on {
		t.Errorf("built_in_rules = %v, %v; want true, untouched", on, err)
	}
}

func TestSaveRuleSetsMissing(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", RuleSets: []string{"gone"}})
	var missing *MissingRuleSetError
	if !errors.As(err, &missing) || missing.Name != "gone" || !errors.Is(err, ErrRuleSetNotFound) {
		t.Errorf("UpsertDashboardRepo = %v, want a MissingRuleSetError", err)
	}
	if _, found, _ := s.AnyDashboardRepo(ctx, "lab", "acme/web"); found {
		t.Error("the row was written")
	}
	if err := s.PutRepoDefaults(ctx, RepoDefaults{RuleSets: []string{"gone"}}); !errors.Is(err, ErrRuleSetNotFound) {
		t.Errorf("PutRepoDefaults = %v", err)
	}
}

func TestSetRunRulesHash(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	id := enqueue(t, s, mr1, "aaa")
	if r := getRun(t, s, id); r.RulesHash != nil {
		t.Fatalf("a new run has rules hash %v", *r.RulesHash)
	}
	if err := s.SetRunRulesHash(ctx, id, "h1", "left out"); err != nil {
		t.Fatalf("SetRunRulesHash: %v", err)
	}
	if r := getRun(t, s, id); r.RulesHash == nil || *r.RulesHash != "h1" || r.BuiltInRulesNote != "left out" {
		t.Errorf("RulesHash = %v, note %q; want h1, left out", r.RulesHash, r.BuiltInRulesNote)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunRulesHash(ctx, id, "h2", ""); err != nil {
		t.Errorf("SetRunRulesHash of a running run: %v", err)
	}
	var note *string
	if err := s.pool.QueryRow(ctx, `SELECT built_in_rules_note FROM runs WHERE id = $1::uuid`, id).Scan(&note); err != nil || note != nil {
		t.Errorf("built_in_rules_note = %v, %v; want NULL for no note", note, err)
	}
	if _, err := s.SkipRun(ctx, id, "x", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunRulesHash(ctx, id, "h3", ""); err == nil {
		t.Error("SetRunRulesHash of a finished run passed")
	}
}
