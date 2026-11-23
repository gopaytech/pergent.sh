package engine

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// An API review has no repository: it gets the admin default rule sets and
// no built-in rules, reads no rules file and records its hash.
func TestAPIReviewRules(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	if err := env.Store.CreateRuleSet(env.ctx, "a", setAYAML, env.Now()); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.PutRepoDefaults(env.ctx, store.RepoDefaults{Skills: []string{"security"}, RuleSets: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	id := a.ask(t, "route:1", twoFiles())

	dispatch(t, env)

	list := jobRules(t, env, id)
	if len(list) != 1 || list[0].Source != rule.DashboardSource("a") {
		t.Errorf("rules = %+v, want the default set only", list)
	}
	if v, ok := jobVar(t, env, id, "PERGENT_NO_BUILT_IN_RULES"); !ok || v != "true" {
		t.Errorf("PERGENT_NO_BUILT_IN_RULES = %q, %v", v, ok)
	}
	r := env.Run(id)
	if r.RulesHash == nil || *r.RulesHash != inputs.RulesHash(list, nil, testImage) {
		t.Errorf("rules_hash = %v", r.RulesHash)
	}
	noGitLab(t, env)
}

// Built-in rules are chosen per repository and have no default, so an API
// review gets none, even with an image that loads a chosen list.
func TestAPIReviewBuiltInRulesOffByDefault(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	builtInListFloor(t, "v0.5.0")
	id := a.ask(t, "route:1", twoFiles())

	dispatch(t, env)

	if v, ok := jobVar(t, env, id, "PERGENT_NO_BUILT_IN_RULES"); !ok || v != "true" {
		t.Errorf("PERGENT_NO_BUILT_IN_RULES = %q, %v; want \"true\" with nothing set", v, ok)
	}
	if v, ok := jobVar(t, env, id, "PERGENT_BUILT_IN_RULES"); ok {
		t.Errorf("PERGENT_BUILT_IN_RULES = %q; want none", v)
	}
	if r := env.Run(id); r.BuiltInRulesNote != "" {
		t.Errorf("note = %q; want none with nothing chosen", r.BuiltInRulesNote)
	}
	noGitLab(t, env)
}

func TestAPIReviewMissingDefaultSet(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	if err := env.Store.CreateRuleSet(env.ctx, "gone", setAYAML, env.Now()); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.PutRepoDefaults(env.ctx, store.RepoDefaults{RuleSets: []string{"gone"}}); err != nil {
		t.Fatal(err)
	}
	// Raw SQL: the store refuses to delete a set the defaults name.
	rawExec(t, env, `DELETE FROM rule_sets WHERE name = 'gone'`)
	id := a.ask(t, "route:1", twoFiles())

	dispatch(t, env)
	if env.Job(id) != nil {
		t.Fatal("a Job started without its rules")
	}
	publish(t, env)
	a.wantFailed(t, id, reviewapi.StepStarting, `rule set "gone" is not in the rule library`)
	if items, _ := env.Store.Attention(env.ctx); len(items) != 0 && strings.Contains(items[0].Kind, "rules") {
		t.Errorf("an API review set attention: %+v", items)
	}
}

// rawExec runs sql in the test's schema, for a state the store refuses.
func rawExec(t *testing.T, env *testEnv, sql string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(env.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.schema
	conn, err := pgx.ConnectConfig(env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(env.ctx)
	if _, err := conn.Exec(env.ctx, sql); err != nil {
		t.Fatal(err)
	}
}
