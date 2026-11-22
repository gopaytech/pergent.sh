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
		RuleSets: []string{"b", "a"}, BuiltInRules: []string{"go"}})
	r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}

	unset := get(t, r, "main", "acme/widgets")
	if unset.RuleSets != nil || unset.BuiltInRules != nil || unset.Overridden[KeyRuleSets] || unset.Overridden[KeyBuiltInRules] {
		t.Errorf("unset: %v %v %v", unset.RuleSets, unset.BuiltInRules, unset.Overridden)
	}
	set := get(t, r, "gh", "acme/rules")
	if !slices.Equal(set.RuleSets, []string{"b", "a"}) || !slices.Equal(set.BuiltInRules, []string{"go"}) || !set.Overridden[KeyRuleSets] || !set.Overridden[KeyBuiltInRules] {
		t.Errorf("set: %v %v %v", set.RuleSets, set.BuiltInRules, set.Overridden)
	}
}

// Built-in rules are chosen per repository: a config-file repository that
// leaves built_in_rules out, or lists none, gets none; one that lists some
// gets those, in builtin.yaml order.
func TestBuiltInRulesConfig(t *testing.T) {
	for _, c := range []struct {
		name       string
		set        []string
		want       []string
		overridden bool
	}{
		{"unset is none", nil, nil, false},
		{"empty is none", []string{}, nil, false},
		{"a list is that list", []string{"yaml", "go"}, []string{"yaml", "go"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Repositories[0].BuiltInRules = c.set
			r := &Resolver{Config: cfg, Instances: instances.New(cfg, nil, nil)}
			eff := get(t, r, "main", "acme/widgets")
			if !slices.Equal(eff.BuiltInRules, c.want) || eff.Overridden[KeyBuiltInRules] != c.overridden {
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
		builtIn  []string
		// setsOver and builtInOver are whether the repository overrides
		// its rule sets and its built-in rules.
		setsOver, builtInOver bool
	}{
		// Built-in rules have no default: a repository that chose none
		// gets none.
		{"nothing set", store.DashboardRepo{}, store.RepoDefaults{}, nil, nil, false, false},
		{"chosen", store.DashboardRepo{BuiltInRules: []string{"go", "yaml"}}, store.RepoDefaults{}, nil, []string{"yaml", "go"}, false, true},
		// A name the pinned pergent no longer has is dropped: the picker
		// can't show it and pergent would skip it.
		{"a name that is gone", store.DashboardRepo{BuiltInRules: []string{"gone", "go"}}, store.RepoDefaults{}, nil, []string{"go"}, false, true},
		{"only gone names", store.DashboardRepo{BuiltInRules: []string{"gone"}}, store.RepoDefaults{}, nil, nil, false, false},
		{"repository wins", store.DashboardRepo{RuleSets: []string{"x"}, BuiltInRules: []string{"go"}},
			store.RepoDefaults{RuleSets: []string{"d"}}, []string{"x"}, []string{"go"}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := c.row
			row.Instance, row.Project = "main", "acme/widgets"
			eff, ok := r.fromDashboard(row, c.defaults)
			if !ok {
				t.Fatal("fromDashboard: not ok")
			}
			if !slices.Equal(eff.RuleSets, c.sets) || !slices.Equal(eff.BuiltInRules, c.builtIn) || (c.builtIn == nil && eff.BuiltInRules != nil) ||
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
	if d.BuiltInRules != nil {
		t.Errorf("the defaults choose built-in rules %v; there is no default", d.BuiltInRules)
	}
}
