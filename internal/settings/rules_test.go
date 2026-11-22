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
	if unset.RuleSets != nil || unset.BuiltInRules || unset.Overridden[KeyRuleSets] || unset.Overridden[KeyBuiltInRules] {
		t.Errorf("unset: %v %v %v", unset.RuleSets, unset.BuiltInRules, unset.Overridden)
	}
	set := get(t, r, "gh", "acme/rules")
	if !slices.Equal(set.RuleSets, []string{"b", "a"}) || set.BuiltInRules || !set.Overridden[KeyRuleSets] || !set.Overridden[KeyBuiltInRules] {
		t.Errorf("set: %v %v %v", set.RuleSets, set.BuiltInRules, set.Overridden)
	}
}

// Built-in rules are opt-in: a config-file repository that leaves
// built_in_rules out gets none, and one that sets it gets what it set.
func TestBuiltInRulesConfigOptIn(t *testing.T) {
	for _, c := range []struct {
		name       string
		set        *bool
		want       bool
		overridden bool
	}{
		{"unset is off", nil, false, false},
		{"on is on", ptr(true), true, true},
		{"off is off", ptr(false), false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Repositories[0].BuiltInRules = c.set
			r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}
			eff := get(t, r, "main", "acme/widgets")
			if eff.BuiltInRules != c.want || eff.Overridden[KeyBuiltInRules] != c.overridden {
				t.Errorf("BuiltInRules = %v (overridden %v), want %v (%v)", eff.BuiltInRules, eff.Overridden[KeyBuiltInRules], c.want, c.overridden)
			}
		})
	}
}

func TestEffectiveRuleSetsDashboard(t *testing.T) {
	cfg := testConfig()
	r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}
	for _, c := range []struct {
		name     string
		row      store.DashboardRepo
		defaults store.RepoDefaults
		sets     []string
		builtIn  bool
		// setsOver and builtInOver are whether the repository overrides
		// its rule sets and its Built-in rules switch.
		setsOver, builtInOver bool
	}{
		// Built-in rules are opt-in: NULL on the repository and in the
		// defaults is off.
		{"nothing set", store.DashboardRepo{}, store.RepoDefaults{}, nil, false, false, false},
		{"default switch off", store.DashboardRepo{}, store.RepoDefaults{BuiltInRules: ptr(false)}, nil, false, false, false},
		{"default switch on", store.DashboardRepo{}, store.RepoDefaults{BuiltInRules: ptr(true)}, nil, true, false, false},
		{"repository on", store.DashboardRepo{BuiltInRules: ptr(true)}, store.RepoDefaults{}, nil, true, false, true},
		{"repository off over a default on", store.DashboardRepo{BuiltInRules: ptr(false)},
			store.RepoDefaults{BuiltInRules: ptr(true)}, nil, false, false, true},
		{"repository wins", store.DashboardRepo{RuleSets: []string{"x"}, BuiltInRules: ptr(true)},
			store.RepoDefaults{BuiltInRules: ptr(false), RuleSets: []string{"d"}}, []string{"x"}, true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := c.row
			row.Instance, row.Project = "main", "acme/widgets"
			eff, ok := r.fromDashboard(row, c.defaults)
			if !ok {
				t.Fatal("fromDashboard: not ok")
			}
			if !slices.Equal(eff.RuleSets, c.sets) || eff.BuiltInRules != c.builtIn ||
				eff.Overridden[KeyRuleSets] != c.setsOver || eff.Overridden[KeyBuiltInRules] != c.builtInOver {
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
	if d.BuiltInRules {
		t.Error("built-in rules are on by default; they are opt-in")
	}
}
