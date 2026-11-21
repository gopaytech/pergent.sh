package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent/rule"
)

const rulesBase = `public_url: https://pergent.example.com
gitlab:
  - name: main
    url: https://gitlab.example.com
    review_token_env: GITLAB_REVIEW_TOKEN
    approve_token_env: GITLAB_APPROVE_TOKEN
    webhook_secret_env: GITLAB_WEBHOOK_SECRET
llm:
  provider: anthropic
  model: claude-sonnet-4
jobs:
  namespace: pergent
`

func TestConfigRuleSetsLoad(t *testing.T) {
	cfg, err := Load(writeConfig(t, rulesBase+`rule_sets:
  - name: go-api
    content: |
      rules:
        - path: "internal/api/**/*.go"
          rule: Check the handlers.
repositories:
  - gitlab: main
    project: acme/widgets
    rule_sets: [go-api]
    built_in_rules: false
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.RuleSets) != 1 || cfg.RuleSets[0].Name != "go-api" {
		t.Fatalf("RuleSets = %+v", cfg.RuleSets)
	}
	want := []rule.Entry{{Path: "internal/api/**/*.go", Rule: "Check the handlers.", Source: "dashboard:go-api"}}
	if !slices.Equal(cfg.RuleSets[0].Entries, want) {
		t.Errorf("Entries = %+v, want %+v", cfg.RuleSets[0].Entries, want)
	}
	repo := cfg.Repositories[0]
	if !slices.Equal(repo.RuleSets, []string{"go-api"}) || repo.BuiltInRules == nil || *repo.BuiltInRules {
		t.Errorf("repository rule settings = %v %v", repo.RuleSets, repo.BuiltInRules)
	}
	if set, ok := cfg.RuleSet("go-api"); !ok || set.Name != "go-api" {
		t.Errorf("RuleSet(go-api) = %+v, %v", set, ok)
	}
	if _, ok := cfg.RuleSet("other"); ok {
		t.Error("RuleSet(other) found")
	}
}

func TestConfigRuleSetBad(t *testing.T) {
	for _, tt := range []struct{ body, want string }{
		{"rule_sets:\n  - name: -bad\n    content: \"rules: []\"\n", `rule_sets[0]: name "-bad" must match`},
		{"rule_sets:\n  - name: a\n    content: \"rules: []\"\n  - name: a\n    content: \"rules: []\"\n", `rule_sets[1]: duplicate rule set "a"`},
		{"rule_sets:\n  - name: a\n    content: |\n      rules:\n        - path: api/\n          rule: x\n", `rule_sets[0] (a): rule set "a": entry 1: path: pattern "api/" can never match`},
		{"repositories:\n  - gitlab: main\n    project: acme/w\n    rule_sets: []\n", `repositories[0] (acme/w): rule_sets must be a list of rule-set names, or left out for the default list`},
		{"repositories:\n  - gitlab: main\n    project: acme/w\n    rule_sets: [\"-x\"]\n", `repositories[0] (acme/w): rule_sets name "-x" must match`},
	} {
		_, err := Load(writeConfig(t, rulesBase+tt.body))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Load(%q) = %v, want %q", tt.body, err, tt.want)
		}
	}
}
