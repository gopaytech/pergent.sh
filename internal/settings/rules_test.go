package settings

import (
	"slices"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func TestEffectiveRuleSetsConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Repositories = append(cfg.Repositories, config.Repository{GitHub: "gh", Project: "Acme/Rules",
		RuleSets: []string{"b", "a"}, BuiltInRules: ptr(false)})
	r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}

	unset := get(t, r, "main", "acme/widgets")
	if unset.RuleSets != nil || !unset.BuiltInRules || unset.Overridden[KeyRuleSets] || unset.Overridden[KeyBuiltInRules] {
		t.Errorf("unset: %v %v %v", unset.RuleSets, unset.BuiltInRules, unset.Overridden)
	}
	set := get(t, r, "gh", "acme/rules")
	if !slices.Equal(set.RuleSets, []string{"b", "a"}) || set.BuiltInRules || !set.Overridden[KeyRuleSets] || !set.Overridden[KeyBuiltInRules] {
		t.Errorf("set: %v %v %v", set.RuleSets, set.BuiltInRules, set.Overridden)
	}
}

func TestEffectiveRuleSetsDashboard(t *testing.T) {
	cfg := testConfig()
	r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}
	for _, c := range []struct {
		name       string
		row        store.DashboardRepo
		defaults   store.RepoDefaults
		sets       []string
		builtIn    bool
		overridden bool
	}{
		{"nothing set", store.DashboardRepo{}, store.RepoDefaults{}, nil, true, false},
		{"default switch off", store.DashboardRepo{}, store.RepoDefaults{BuiltInRules: ptr(false)}, nil, false, false},
		{"repository wins", store.DashboardRepo{RuleSets: []string{"x"}, BuiltInRules: ptr(true)},
			store.RepoDefaults{BuiltInRules: ptr(false), RuleSets: []string{"d"}}, []string{"x"}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := c.row
			row.Instance, row.Project = "main", "acme/widgets"
			eff, ok := r.fromDashboard(row, c.defaults)
			if !ok {
				t.Fatal("fromDashboard: not ok")
			}
			if !slices.Equal(eff.RuleSets, c.sets) || eff.BuiltInRules != c.builtIn ||
				eff.Overridden[KeyRuleSets] != c.overridden || eff.Overridden[KeyBuiltInRules] != c.overridden {
				t.Errorf("got %v %v %v", eff.RuleSets, eff.BuiltInRules, eff.Overridden)
			}
		})
	}
}

func TestDefaultsHaveRuleKeys(t *testing.T) {
	cfg := testConfig()
	r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}
	d := r.Defaults(config.PlatformGitLab, "main")
	if _, ok := d.Overridden[KeyRuleSets]; !ok {
		t.Error("Overridden lacks rule_sets")
	}
	if _, ok := d.Overridden[KeyBuiltInRules]; !ok {
		t.Error("Overridden lacks built_in_rules")
	}
	if !d.BuiltInRules {
		t.Error("built-in rules are off by default")
	}
}
