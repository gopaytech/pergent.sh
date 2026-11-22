package dashboard

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func TestRepoRuleSetsSetting(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.sessionFor(e.ssoAdmin("boss@example.com"))
	for _, n := range []string{"a", "b"} {
		if err := e.st.CreateRuleSet(ctx, n, goRuleSet, e.d.now()); err != nil {
			t.Fatal(err)
		}
	}
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"ruleSets":["b","a","b"]}`), "set", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); !slices.Equal(r.RuleSets, []string{"b", "a"}) {
		t.Errorf("stored = %v, want b, a once each", r.RuleSets)
	}
	for body, want := range map[string]string{
		`{"ruleSets":["gone"]}`: `rule set "gone" is not in the rule library`,
		`{"ruleSets":[]}`:       "ruleSets must be a non-empty list of rule sets from the library, or null for the default list",
		`{"ruleSets":"a"}`:      "ruleSets must be a non-empty list of rule sets from the library, or null for the default list",
	} {
		rec := e.send(c, "PUT", gadgetsSettings, body)
		wantCode(t, rec, body, http.StatusBadRequest)
		if got := errorOf(t, rec); got != want {
			t.Errorf("%s: error = %q, want %q", body, got, want)
		}
	}
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"ruleSets":null}`), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.RuleSets != nil {
		t.Errorf("after null: %v", r.RuleSets)
	}
}

// Built-in rules are opt-in: a repository that sets nothing shows them off,
// turning them on stores true, and a reset goes back to off.
func TestRepoBuiltInRulesSetting(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.sessionFor(e.ssoAdmin("boss@example.com"))
	if s := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets").Settings["builtInRules"]; s.Value != false || s.Overridden {
		t.Errorf("unset view = %+v, want off and not overridden", s)
	}
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"builtInRules":true}`), "on", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.BuiltInRules == nil || !*r.BuiltInRules {
		t.Errorf("stored = %v", r.BuiltInRules)
	}
	v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets")
	if s := v.Settings["builtInRules"]; s.Value != true || !s.Overridden {
		t.Errorf("view = %+v", s)
	}
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"builtInRules":false}`), "off", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.BuiltInRules == nil || *r.BuiltInRules {
		t.Errorf("stored after off = %v", r.BuiltInRules)
	}
	wantCode(t, e.send(c, "POST", gadgetsSettings+"/reset", ``), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.BuiltInRules != nil {
		t.Errorf("after reset: %v", r.BuiltInRules)
	}
	if s := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets").Settings["builtInRules"]; s.Value != false || s.Overridden {
		t.Errorf("view after reset = %+v, want off", s)
	}
}

func TestDefaultsRuleSets(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.userOnMain(true, "acme/widgets")
	if err := e.st.CreateRuleSet(ctx, "a", goRuleSet, e.d.now()); err != nil {
		t.Fatal(err)
	}
	v := decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"ruleSets": []string{"a"}, "builtInRules": true}), "PUT", http.StatusOK)
	if !slices.Equal(v.RuleSets, []string{"a"}) || !v.BuiltInRules || !v.Pinned["builtInRules"] || v.Config.BuiltInRules {
		t.Errorf("defaults = %+v", v)
	}
	// A PUT without ruleSets keeps the list; one without builtInRules goes
	// back to the default, off.
	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"approve": true}), "PUT without", http.StatusOK)
	if !slices.Equal(v.RuleSets, []string{"a"}) || v.BuiltInRules || v.Pinned["builtInRules"] {
		t.Errorf("after a PUT without them: %v %v", v.RuleSets, v.BuiltInRules)
	}
	rec := e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"ruleSets": []string{"gone"}})
	wantCode(t, rec, "PUT gone", http.StatusBadRequest)
	if got := errorOf(t, rec); got != `rule set "gone" is not in the rule library` {
		t.Errorf("error = %q", got)
	}
}

func TestInputsViewRules(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) {
		cfg.Repositories[0].RuleSets = []string{"a"}
		cfg.Repositories[0].Skills = []string{"style"}
	})
	e.skill("style", "Style text.")
	if err := e.st.CreateRuleSet(ctx, "a", tsRuleSet, e.d.now()); err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/widgets"), Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	e.src["main"].set(func(f *fakeSource) { f.files[".pergent/rules.yaml"] = goRuleSet })
	c := e.userOnMain(false, "acme/widgets")
	v := getJSON[InputsView](e, c, "/api/repositories/main/acme/widgets/inputs")
	var got []string
	for _, r := range v.Rules.Entries {
		got = append(got, r.Source+"|"+r.Path)
	}
	// The repository sets no Built-in rules switch, so they are off: opt-in.
	if !slices.Equal(got, []string{"repo|*.go", "dashboard:a|*.ts"}) || v.Rules.BuiltIn || v.Rules.Problem != "" {
		t.Errorf("rules = %+v", v.Rules)
	}

	e.src["main"].set(func(f *fakeSource) { f.files[".pergent/rules.yaml"] = "rules:\n  - path: a.go\n" })
	e.d.inputs.invalidate()
	v = getJSON[InputsView](e, c, "/api/repositories/main/acme/widgets/inputs")
	if v.Rules.Problem != ".pergent/rules.yaml: entry 1: needs a rule or off: true" || len(v.Rules.Entries) != 0 || len(v.Skills) == 0 {
		t.Errorf("a broken file: rules = %+v, skills %d", v.Rules, len(v.Skills))
	}
}

func TestRunViewFileRules(t *testing.T) {
	e := newEnv(t, nil)
	res := map[string]any{
		"schema_version": 1, "status": "completed", "base": baseSHA, "head": head('a'),
		"rules": map[string]any{"built_in": true, "entries": 1},
		"skills": []any{
			map[string]any{"name": "one", "verdict": "no_blocking_issues", "findings": []any{},
				"files": map[string]any{"reviewed": []string{"a.go"}, "skipped": []any{}, "not_reviewed": []any{},
					"rules": []any{map[string]any{"path": "a.go", "rule": "repo:*.go"}}}},
			map[string]any{"name": "two", "verdict": "no_blocking_issues", "findings": []any{},
				"files": map[string]any{"reviewed": []string{"a.go"}, "skipped": []any{}, "not_reviewed": []any{},
					"rules": []any{map[string]any{"path": "a.go", "rule": "repo:*.go"}}}},
		},
		"comment": map[string]any{"markdown": "x"},
	}
	b, _ := json.Marshal(res)
	id := e.addRun(runFix{project: "acme/widgets", number: 3, head: head('a'), result: string(b), duration: time.Minute, outcome: OutcomeApproved})
	older := e.addRun(runFix{project: "acme/widgets", number: 4, head: head('b'), result: cleanResult(head('b')), outcome: OutcomeApproved})
	c := e.userOnMain(false, "acme/widgets")

	v := getJSON[RunDetailView](e, c, "/api/runs/"+id)
	if !v.RulesUsed || !slices.Equal(v.Files.Rules, []FileRuleView{{Path: "a.go", Rule: "repo:*.go"}}) {
		t.Errorf("rulesUsed %v, files.rules %+v", v.RulesUsed, v.Files.Rules)
	}
	if v := getJSON[RunDetailView](e, c, "/api/runs/"+older); v.RulesUsed || v.Files.Rules == nil || len(v.Files.Rules) != 0 {
		t.Errorf("an older result: rulesUsed %v, files.rules %#v", v.RulesUsed, v.Files.Rules)
	}
}
