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
// Built-in rules switch, reads no rules file and records its hash.
func TestAPIReviewRules(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	if err := env.Store.CreateRuleSet(env.ctx, "a", setAYAML, env.Now()); err != nil {
		t.Fatal(err)
	}
	off := false
	if err := env.Store.PutRepoDefaults(env.ctx, store.RepoDefaults{Skills: []string{"security"}, RuleSets: []string{"a"}, BuiltInRules: &off}); err != nil {
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
	if r.RulesHash == nil || *r.RulesHash != inputs.RulesHash(list, false, testImage) {
		t.Errorf("rules_hash = %v", r.RulesHash)
	}
	noGitLab(t, env)
}

// With no admin default, an API review gets no built-in rules: they are
// opt-in.
func TestAPIReviewBuiltInRulesOffByDefault(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	id := a.ask(t, "route:1", twoFiles())

	dispatch(t, env)

	if v, ok := jobVar(t, env, id, "PERGENT_NO_BUILT_IN_RULES"); !ok || v != "true" {
		t.Errorf("PERGENT_NO_BUILT_IN_RULES = %q, %v; want \"true\" with nothing set", v, ok)
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
