package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent/rule"
)

// BuiltInRuleNames is each built-in's name once, in builtin.yaml order: the
// order rule.Builtin lists them in, by first entry.
func TestBuiltInRuleNames(t *testing.T) {
	var want []string
	for _, e := range rule.Builtin() {
		if !slices.Contains(want, e.Name) {
			want = append(want, e.Name)
		}
	}
	got := BuiltInRuleNames()
	if !slices.Equal(got, want) {
		t.Fatalf("BuiltInRuleNames() = %v, want %v", got, want)
	}
	if len(got) < 2 || !slices.Contains(got, "go") || !slices.Contains(got, "yaml") {
		t.Fatalf("BuiltInRuleNames() = %v; want go and yaml among them", got)
	}
	got[0] = "changed"
	if BuiltInRuleNames()[0] == "changed" {
		t.Error("BuiltInRuleNames gives out its own slice")
	}
}

func TestOrderBuiltInRules(t *testing.T) {
	names := BuiltInRuleNames()
	first, second := names[0], names[1]
	known, unknown := OrderBuiltInRules([]string{"nope", second, first, second, "nope", "other"})
	if !slices.Equal(known, []string{first, second}) {
		t.Errorf("known = %v, want %v in builtin.yaml order", known, []string{first, second})
	}
	if !slices.Equal(unknown, []string{"nope", "other"}) {
		t.Errorf("unknown = %v, want [nope other] once each", unknown)
	}
	if known, unknown := OrderBuiltInRules(nil); known != nil || unknown != nil {
		t.Errorf("OrderBuiltInRules(nil) = %v, %v; want nil, nil", known, unknown)
	}
}

func setBuiltInListMin(t *testing.T, v string) {
	t.Helper()
	old := BuiltInRulesListMinVersion
	BuiltInRulesListMinVersion = v
	t.Cleanup(func() { BuiltInRulesListMinVersion = old })
}

func TestBuiltInRulesListImage(t *testing.T) {
	const none = "no review image can load only the enabled built-in rules yet"
	for _, tc := range []struct {
		min, image string
		ok         bool
		reason     string // a substring; "" means no refusal
	}{
		{"", "ghcr.io/x/pergent:v0.12.0", false, none},
		{"", "ghcr.io/x/pergent:v99.0.0", false, none},
		{"v0.12.0", "ghcr.io/x/pergent:v0.12.0", true, ""},
		{"v0.12.0", "ghcr.io/x/pergent:v0.13.1", true, ""},
		{"v0.12.0", "ghcr.io/x/pergent:v0.11.0", false, `image tag "v0.11.0" is older than v0.12.0`},
		{"v0.12.0", "ghcr.io/x/pergent:main", false, "not a version"},
		{"v0.12.0", "ghcr.io/x/pergent@sha256:abc123", false, "no tag"},
		{"v0.12.0", "host:5000/pergent", false, "no tag"},
	} {
		setBuiltInListMin(t, tc.min)
		ok, reason := BuiltInRulesListImage(tc.image)
		if ok != tc.ok || (tc.ok && reason != "") || (!tc.ok && !strings.Contains(reason, tc.reason)) {
			t.Errorf("BuiltInRulesListImage(%q) with floor %q = %v, %q; want %v, %q", tc.image, tc.min, ok, reason, tc.ok, tc.reason)
		}
		// The reason is shown on a run's page to anyone who reads the run:
		// it names the tag, never the registry.
		if strings.Contains(reason, "ghcr.io") || strings.Contains(reason, "host:5000") {
			t.Errorf("BuiltInRulesListImage(%q) reason %q names the image", tc.image, reason)
		}
	}
}

func TestConfigBuiltInRulesList(t *testing.T) {
	names := BuiltInRuleNames()
	first, second := names[0], names[1]
	cfg, err := Load(writeConfig(t, rulesBase+`repositories:
  - gitlab: main
    project: acme/widgets
    built_in_rules: [`+second+`, `+first+`, `+second+`]
  - gitlab: main
    project: acme/empty
    built_in_rules: []
  - gitlab: main
    project: acme/unset
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Repositories[0].BuiltInRules; !slices.Equal(got, []string{first, second}) {
		t.Errorf("built_in_rules = %v, want %v (builtin.yaml order, once each)", got, []string{first, second})
	}
	for _, r := range cfg.Repositories[1:] {
		if len(r.BuiltInRules) != 0 {
			t.Errorf("%s built_in_rules = %v, want none", r.Project, r.BuiltInRules)
		}
	}
}

// The switch of v0.18.0 and before still loads, so an old config starts:
// true is every built-in rule, in builtin.yaml order, and false is none. A
// bool is what yaml.v3 decodes into a bool, as the old *bool field did, so
// yes and off count too.
func TestConfigBuiltInRulesOldSwitch(t *testing.T) {
	const repo = "repositories:\n  - gitlab: main\n    project: acme/w\n    built_in_rules: "
	for _, tt := range []struct {
		value string
		want  []string
	}{
		{"true", BuiltInRuleNames()},
		{"True", BuiltInRuleNames()},
		{"yes", BuiltInRuleNames()},
		{"false", nil},
		{"off", nil},
		{"null", nil},
	} {
		cfg, err := Load(writeConfig(t, rulesBase+repo+tt.value+"\n"))
		if err != nil {
			t.Errorf("built_in_rules: %s: Load = %v, want it to start", tt.value, err)
			continue
		}
		if got := cfg.Repositories[0].BuiltInRules; !slices.Equal(got, tt.want) {
			t.Errorf("built_in_rules: %s = %v, want %v", tt.value, got, tt.want)
		}
	}
	if n := len(BuiltInRuleNames()); n != 67 {
		t.Errorf("true gives %d built-in rules, want the 67 of the pinned pergent", n)
	}
}

func TestConfigBuiltInRulesBad(t *testing.T) {
	const repo = "repositories:\n  - gitlab: main\n    project: acme/w\n    built_in_rules: "
	for _, tt := range []struct{ value, want string }{
		{"go", "built_in_rules must be a list of built-in rule names, such as [go, yaml]"},
		{`"true"`, "built_in_rules must be a list of built-in rule names, such as [go, yaml]"},
		{"[go, nope]", `repositories[0] (acme/w): built_in_rules: "nope" is not a built-in rule`},
		{"[{a: b}]", "built_in_rules must be a list of built-in rule names, such as [go, yaml]"},
	} {
		_, err := Load(writeConfig(t, rulesBase+repo+tt.value+"\n"))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("built_in_rules: %s: Load = %v, want %q", tt.value, err, tt.want)
		}
	}
}
