package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	goRuleSet = "rules:\n  - path: \"*.go\"\n    rule: Check Go.\n"
	tsRuleSet = "rules:\n  - path: \"*.ts\"\n    rule: Check TS.\n"
)

// The editor's notes need no database: each entry that may hide a built-in
// rule gets one.
func TestRuleSetNotes(t *testing.T) {
	d := &Dashboard{builtInRules: func() []rule.Entry {
		return []rule.Entry{{Path: "*.go", Rule: "built-in go", Source: rule.SourceBuiltIn, Name: "go"}}
	}}
	got := d.ruleSetNotes("s", "rules:\n  - path: \"*.ts\"\n    rule: x\n  - path: \"**/*.go\"\n    rule: y\n")
	want := []string{"entry 2: This entry hides the built-in rule go for matching files."}
	if !slices.Equal(got, want) {
		t.Errorf("notes = %q, want %q", got, want)
	}
	if got := (&Dashboard{builtInRules: rule.Builtin}).ruleSetNotes("s", goRuleSet); len(got) != 0 {
		t.Errorf("with the shipped (empty) built-in list, notes = %q", got)
	}
	if got := d.ruleSetNotes("s", "not: [yaml"); got == nil || len(got) != 0 {
		t.Errorf("text that doesn't load: notes = %#v, want an empty list", got)
	}
}

func TestRuleSetCreateChecks(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.userOnMain(true, "acme/widgets")
	for _, tt := range []struct{ name, content, want string }{
		{"bad", "rules: [", "rule set \"bad\": "},
		{"unknown", "rules:\n  - path: a.go\n    paths: x\n", `rule set "unknown": entry 1: unknown field "paths"`},
		{"dir", "rules:\n  - path: api/\n    rule: x\n", `rule set "dir": entry 1: path: pattern "api/" can never match`},
		{"long", "rules:\n  - path: a.go\n    rule: " + strings.Repeat("x", rule.MaxRuleChars+1) + "\n", "rule is 16001 characters"},
		{"-bad", goRuleSet, msgBadRuleSetName},
		{"empty", "  ", msgRuleSetNoText},
	} {
		rec := e.sendJSON(admin, "POST", "/api/admin/rule-sets", map[string]any{"name": tt.name, "content": tt.content})
		wantCode(t, rec, "POST "+tt.name, http.StatusBadRequest)
		if got := errorOf(t, rec); !strings.Contains(got, tt.want) {
			t.Errorf("%s: error = %q, want %q", tt.name, got, tt.want)
		}
	}
	got := decode[RuleSetWrittenView](t, e.sendJSON(admin, "POST", "/api/admin/rule-sets",
		map[string]any{"name": "go", "content": goRuleSet}), "POST go", http.StatusCreated)
	if got.Version != 1 || got.Notes == nil {
		t.Errorf("written = %+v", got)
	}
	rec := e.sendJSON(admin, "POST", "/api/admin/rule-sets", map[string]any{"name": "go", "content": goRuleSet})
	wantCode(t, rec, "POST go again", http.StatusConflict)
}

func TestRuleSetSecretRefused(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.userOnMain(true, "acme/widgets")
	const secret = "glpat-very-secret-token"
	e.d.heldSecrets = func(context.Context) ([]string, error) { return []string{secret}, nil }
	decode[RuleSetWrittenView](t, e.sendJSON(admin, "POST", "/api/admin/rule-sets",
		map[string]any{"name": "go", "content": goRuleSet}), "POST go", http.StatusCreated)
	leak := "rules:\n  - path: \"*.go\"\n    rule: Use " + secret + ".\n"
	for _, w := range []struct {
		method, target string
		body           any
	}{
		{"POST", "/api/admin/rule-sets", map[string]any{"name": "leak", "content": leak}},
		{"PUT", "/api/admin/rule-sets/go", map[string]any{"content": leak, "version": 1}},
	} {
		rec := e.sendJSON(admin, w.method, w.target, w.body)
		wantCode(t, rec, w.method, http.StatusBadRequest)
		if got := errorOf(t, rec); got != msgRuleSetSecret || strings.Contains(rec.Body.String(), secret) {
			t.Errorf("%s: answer %q", w.method, rec.Body.String())
		}
	}
	e.d.heldSecrets = func(context.Context) ([]string, error) { return nil, errors.New("down") }
	rec := e.sendJSON(admin, "POST", "/api/admin/rule-sets", map[string]any{"name": "other", "content": goRuleSet})
	wantCode(t, rec, "POST with secrets down", http.StatusInternalServerError)
	if _, err := e.st.RuleSet(ctx, "other"); !errors.Is(err, store.ErrRuleSetNotFound) {
		t.Error("a set was saved with the secrets unknown")
	}
}

func TestRuleSetUpdateStale(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.userOnMain(true, "acme/widgets")
	decode[RuleSetWrittenView](t, e.sendJSON(admin, "POST", "/api/admin/rule-sets", map[string]any{"name": "go", "content": goRuleSet}), "POST", http.StatusCreated)
	got := decode[RuleSetWrittenView](t, e.sendJSON(admin, "PUT", "/api/admin/rule-sets/go", map[string]any{"content": tsRuleSet, "version": 1}), "PUT", http.StatusOK)
	if got.Version != 2 {
		t.Errorf("version = %d", got.Version)
	}
	wantCode(t, e.sendJSON(admin, "PUT", "/api/admin/rule-sets/go", map[string]any{"content": goRuleSet, "version": 1}), "stale PUT", http.StatusConflict)
}

func TestRuleSetDeleteInUse409(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Repositories[0].RuleSets = []string{"cfg-used"} })
	admin := e.userOnMain(true, "acme/widgets")
	for _, n := range []string{"cfg-used", "free"} {
		decode[RuleSetWrittenView](t, e.sendJSON(admin, "POST", "/api/admin/rule-sets", map[string]any{"name": n, "content": goRuleSet}), "POST "+n, http.StatusCreated)
	}
	rec := e.sendJSON(admin, "DELETE", "/api/admin/rule-sets/cfg-used", nil)
	wantCode(t, rec, "DELETE cfg-used", http.StatusConflict)
	if v := decode[SkillInUseView](t, rec, "409 body", http.StatusConflict); v.Error != msgRuleSetInUse || len(v.Repositories) != 1 {
		t.Errorf("409 = %+v", v)
	}
	wantCode(t, e.sendJSON(admin, "DELETE", "/api/admin/rule-sets/free", nil), "DELETE free", http.StatusNoContent)
}

func TestRuleSetConfigReadOnly(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.RuleSets = []config.ConfigRuleSet{{Name: "team", Content: goRuleSet, Entries: []rule.Entry{{Path: "*.go", Rule: "Check Go.", Source: "dashboard:team"}}}}
	})
	admin := e.userOnMain(true, "acme/widgets")
	list := getJSON[[]RuleSetView](e, admin, "/api/rule-sets")
	if len(list) != 1 || list[0].Source != "config" || list[0].Entries != 1 {
		t.Errorf("list = %+v", list)
	}
	for _, w := range []struct {
		method, target string
		body           any
	}{
		{"POST", "/api/admin/rule-sets", map[string]any{"name": "team", "content": tsRuleSet}},
		{"PUT", "/api/admin/rule-sets/team", map[string]any{"content": tsRuleSet, "version": 1}},
		{"DELETE", "/api/admin/rule-sets/team", nil},
	} {
		wantCode(t, e.sendJSON(admin, w.method, w.target, w.body), w.method, http.StatusConflict)
	}
}

func TestRuleSetLimit400(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.userOnMain(true, "acme/widgets")
	for i := range store.MaxRuleSets {
		if err := e.st.CreateRuleSet(ctx, fmt.Sprintf("s%03d", i), goRuleSet, e.d.now()); err != nil {
			t.Fatal(err)
		}
	}
	rec := e.sendJSON(admin, "POST", "/api/admin/rule-sets", map[string]any{"name": "one-more", "content": goRuleSet})
	wantCode(t, rec, "POST 201st", http.StatusBadRequest)
	if got := errorOf(t, rec); got != "the rule library holds 200 rule sets, the most it can" {
		t.Errorf("error = %q", got)
	}
}

func TestRuleSetAudit(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.userOnMain(true, "acme/widgets")
	e.sendJSON(admin, "POST", "/api/admin/rule-sets", map[string]any{"name": "go", "content": goRuleSet})
	e.sendJSON(admin, "PUT", "/api/admin/rule-sets/go", map[string]any{"content": tsRuleSet, "version": 1})
	e.sendJSON(admin, "DELETE", "/api/admin/rule-sets/go", nil)
	want := map[string]string{"rule_set_created": "go: v1", "rule_set_updated": "go: v1 -> v2", "rule_set_deleted": "go"}
	got := map[string]string{}
	for _, a := range e.auditEntries() {
		if strings.HasPrefix(a.Action, "rule_set_") {
			got[a.Action] = a.Detail
			if strings.Contains(a.Detail, "Check") {
				t.Errorf("audit entry %s holds the text: %q", a.Action, a.Detail)
			}
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("audit %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestRuleSetReadAccess(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.userOnMain(true, "acme/widgets")
	user := e.userOnMain(false, "acme/widgets")
	e.sendJSON(admin, "POST", "/api/admin/rule-sets", map[string]any{"name": "go", "content": goRuleSet})
	if got := getJSON[RuleSetDetailView](e, user, "/api/rule-sets/go"); got.Content != goRuleSet || got.Version != 1 {
		t.Errorf("a user reads %+v", got)
	}
	if got := getJSON[[]BuiltInRuleView](e, user, "/api/built-in-rules"); got == nil || len(got) != len(rule.Builtin()) {
		t.Errorf("built-in rules = %#v", got)
	}
	wantCode(t, e.sendJSON(user, "POST", "/api/admin/rule-sets", map[string]any{"name": "x", "content": goRuleSet}), "user POST", http.StatusNotFound)
	wantCode(t, e.sendJSON(user, "DELETE", "/api/admin/rule-sets/go", nil), "user DELETE", http.StatusNotFound)
}
