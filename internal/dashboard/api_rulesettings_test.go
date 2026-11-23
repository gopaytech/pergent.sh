package dashboard

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func TestRepoRuleSetsSetting(t *testing.T) {
	t.Parallel()
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

// Built-in rules are chosen per repository: a repository that chose none
// shows an empty list, a PUT stores the names once each in builtin.yaml
// order, [] and null and a reset go back to none.
func TestRepoBuiltInRulesSetting(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.sessionFor(e.ssoAdmin("boss@example.com"))
	builtIn := func() SettingView {
		t.Helper()
		return getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets").Settings["builtInRules"]
	}
	if s := builtIn(); !reflect.DeepEqual(s.Value, []any{}) || s.Overridden {
		t.Errorf("unset view = %+v, want [] and not overridden", s)
	}
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"builtInRules":["go","yaml","go"]}`), "choose", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); !slices.Equal(r.BuiltInRules, []string{"yaml", "go"}) {
		t.Errorf("stored = %v, want [yaml go]", r.BuiltInRules)
	}
	if s := builtIn(); !reflect.DeepEqual(s.Value, []any{"yaml", "go"}) || !s.Overridden {
		t.Errorf("view = %+v", s)
	}
	for _, body := range []string{`{"builtInRules":[]}`, `{"builtInRules":null}`} {
		wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"builtInRules":["go"]}`), "choose", http.StatusOK)
		wantCode(t, e.send(c, "PUT", gadgetsSettings, body), body, http.StatusOK)
		if r, _ := e.storedRepo("main", "acme/gadgets"); r.BuiltInRules != nil {
			t.Errorf("stored after %s = %v, want none", body, r.BuiltInRules)
		}
	}
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"builtInRules":["go"]}`), "choose", http.StatusOK)
	wantCode(t, e.send(c, "POST", gadgetsSettings+"/reset", ``), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.BuiltInRules != nil {
		t.Errorf("after reset: %v", r.BuiltInRules)
	}
	if s := builtIn(); !reflect.DeepEqual(s.Value, []any{}) || s.Overridden {
		t.Errorf("view after reset = %+v, want []", s)
	}
}

// A name that is no built-in rule is refused by name, and so is the switch
// the setting was before; nothing is stored.
func TestRepoBuiltInRulesSettingRefused(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.sessionFor(e.ssoAdmin("boss@example.com"))
	for body, want := range map[string]string{
		`{"builtInRules":["go","nope"]}`: `builtInRules: "nope" is not a built-in rule; the Rules page's Built-in tab lists them`,
		`{"builtInRules":true}`:          msgBadBuiltInRules,
		`{"builtInRules":"go"}`:          msgBadBuiltInRules,
	} {
		rec := e.send(c, "PUT", gadgetsSettings, body)
		wantCode(t, rec, body, http.StatusBadRequest)
		if got := errorOf(t, rec); got != want {
			t.Errorf("%s: error = %q, want %q", body, got, want)
		}
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.BuiltInRules != nil {
		t.Errorf("stored = %v, want none", r.BuiltInRules)
	}
}

// The repository page says when jobs.image can't load only the chosen
// built-in rules, and the run page why a run got none.
func TestBuiltInRulesOffNotes(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Jobs.Image = "registry.example.com/pergent:v0.11.0" })
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.sessionFor(e.ssoAdmin("boss@example.com"))
	old := config.BuiltInRulesListMinVersion
	t.Cleanup(func() { config.BuiltInRulesListMinVersion = old })
	config.BuiltInRulesListMinVersion = ""
	v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets")
	if want := "Reviews get no built-in rules for now: no review image can load only the chosen built-in rules yet."; v.BuiltInRulesOff != want {
		t.Errorf("builtInRulesOff with no floor = %q, want %q", v.BuiltInRulesOff, want)
	}
	config.BuiltInRulesListMinVersion = "v0.11.0"
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets"); v.BuiltInRulesOff != "" {
		t.Errorf("builtInRulesOff with an image that loads them = %q", v.BuiltInRulesOff)
	}
	config.BuiltInRulesListMinVersion = "v0.12.0"
	// It names the tag, never the registry: anyone who sees the repository
	// reads it.
	const older = `Reviews get no built-in rules for now: image tag "v0.11.0" is older than v0.12.0, the first pergent that loads only the chosen built-in rules.`
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets"); v.BuiltInRulesOff != older {
		t.Errorf("builtInRulesOff with an older image = %q, want %q", v.BuiltInRulesOff, older)
	}
}

// The admin defaults have no built-in rules: the view has no key for them,
// and a PUT that still sends the old switch is refused with what to do.
func TestDefaultsHaveNoBuiltInRules(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.userOnMain(true, "acme/widgets")
	rec := e.send(admin, "GET", "/api/admin/defaults", "")
	wantCode(t, rec, "GET", http.StatusOK)
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["builtInRules"]; ok {
		t.Errorf("defaults have builtInRules: %v", raw)
	}
	if _, ok := raw["config"].(map[string]any)["builtInRules"]; ok {
		t.Error("the config defaults have builtInRules")
	}
	if _, ok := raw["pinned"].(map[string]any)["builtInRules"]; ok {
		t.Error("pinned has builtInRules")
	}
	rec = e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"builtInRules": true})
	wantCode(t, rec, "PUT builtInRules", http.StatusBadRequest)
	if got := errorOf(t, rec); got != msgNoBuiltInDefault {
		t.Errorf("error = %q, want %q", got, msgNoBuiltInDefault)
	}
}

func TestDefaultsRuleSets(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	admin := e.userOnMain(true, "acme/widgets")
	if err := e.st.CreateRuleSet(ctx, "a", goRuleSet, e.d.now()); err != nil {
		t.Fatal(err)
	}
	v := decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"ruleSets": []string{"a"}}), "PUT", http.StatusOK)
	if !slices.Equal(v.RuleSets, []string{"a"}) {
		t.Errorf("defaults = %+v", v)
	}
	// A PUT without ruleSets keeps the list.
	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"approve": true}), "PUT without", http.StatusOK)
	if !slices.Equal(v.RuleSets, []string{"a"}) {
		t.Errorf("after a PUT without them: %v", v.RuleSets)
	}
	rec := e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"ruleSets": []string{"gone"}})
	wantCode(t, rec, "PUT gone", http.StatusBadRequest)
	if got := errorOf(t, rec); got != `rule set "gone" is not in the rule library` {
		t.Errorf("error = %q", got)
	}
}

func TestInputsViewRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(cfg *config.Config) {
		cfg.Repositories[0].RuleSets = []string{"a"}
		cfg.Repositories[0].Skills = []string{"style"}
		cfg.Repositories[0].BuiltInRules = []string{"yaml", "go"}
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
	// The entries are the repository's and the sets'; the built-in rules
	// it chose follow them, by name.
	if !slices.Equal(got, []string{"repo|*.go", "dashboard:a|*.ts"}) || !slices.Equal(v.Rules.BuiltIn, []string{"yaml", "go"}) || v.Rules.Problem != "" {
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
	t.Parallel()
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
	// Why a run's chosen built-in rules were left out, as dispatch recorded it.
	if v.BuiltInRulesNote != "" {
		t.Errorf("builtInRulesNote = %q, want none", v.BuiltInRulesNote)
	}
	const note = "The chosen built-in rules were left out: no review image can load only the chosen built-in rules yet."
	e.exec(`UPDATE runs SET built_in_rules_note = $2 WHERE id = $1::uuid`, older, note)
	if v := getJSON[RunDetailView](e, c, "/api/runs/"+older); v.BuiltInRulesNote != note {
		t.Errorf("builtInRulesNote = %q, want %q", v.BuiltInRulesNote, note)
	}
}
