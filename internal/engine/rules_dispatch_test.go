package engine

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	repoRulesYAML = "rules:\n  - path: \"*.go\"\n    rule: Repo rule.\n"
	setAYAML      = "rules:\n  - path: \"*.py\"\n    rule: Set a.\n"
	setBYAML      = "rules:\n  - path: \"*.ts\"\n    rule: Set b.\n"
)

// jobRules is the rule list in run id's Job inputs, nil when it has none.
func jobRules(t *testing.T, env *testEnv, id string) []rule.Entry {
	t.Helper()
	cm := env.ConfigMap(id)
	if cm == nil {
		r := env.Run(id)
		t.Fatalf("no job inputs; run state %s, reason %q\n%s", r.State, r.FailureReason, env.Logs())
	}
	data, ok := cm.Data["rules.json"]
	if !ok {
		return nil
	}
	list, err := rule.DecodeList([]byte(data))
	if err != nil {
		t.Fatalf("rules.json: %v", err)
	}
	return list
}

func addRuleSet(t *testing.T, env *testEnv, name, content string) {
	t.Helper()
	if err := env.Store.CreateRuleSet(env.ctx, name, content, env.Now()); err != nil {
		t.Fatal(err)
	}
}

// jobVar is the value of the pergent container's environment variable name.
func jobVar(t *testing.T, env *testEnv, id, name string) (string, bool) {
	t.Helper()
	if env.Job(id) == nil {
		t.Fatalf("no job for run %s\n%s", id, env.Logs())
	}
	v, ok := jobEnv(env, id)[name]
	return v, ok
}

func TestDispatchRulesOrder(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.Engine.Config.Repositories[0].RuleSets = []string{"b", "a"}
	addRuleSet(t, env, "a", setAYAML)
	addRuleSet(t, env, "b", setBYAML)
	env.GitLab.Reply(http.MethodGet, filePath(rule.RepoFile), http.StatusOK, repoRulesYAML)
	id, _ := queue(env)

	dispatch(t, env)

	var labels []string
	for _, e := range jobRules(t, env, id) {
		labels = append(labels, e.Label())
	}
	if want := []string{"repo:*.go", "dashboard:b:*.ts", "dashboard:a:*.py"}; !slices.Equal(labels, want) {
		t.Errorf("rules = %v, want %v", labels, want)
	}
	if v, ok := jobVar(t, env, id, "PERGENT_RULES"); !ok || v != "/in/rules.json" {
		t.Errorf("PERGENT_RULES = %q, %v", v, ok)
	}
}

// The rules come from the target side, as the repository skills do: a change
// cannot edit its own rules.
func TestDispatchRulesFromTargetRef(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.GitLab.Reply(http.MethodGet, filePath(rule.RepoFile), http.StatusOK, repoRulesYAML)
	id, head := queue(env)

	dispatch(t, env)

	reqs := env.GitLab.Requests(http.MethodGet, filePath(rule.RepoFile))
	if len(reqs) == 0 {
		t.Fatalf("the rules file was never read\n%s", env.Logs())
	}
	for _, r := range reqs {
		if got := r.Query.Get("ref"); got != sha('5') || got == head {
			t.Errorf("rules read at ref %q, want %s (the start sha), never the head", got, sha('5'))
		}
	}
	if list := jobRules(t, env, id); len(list) != 1 {
		t.Errorf("rules = %+v", list)
	}
}

func TestDispatchBrokenRulesFileFails(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.GitLab.Reply(http.MethodGet, filePath(rule.RepoFile), http.StatusOK, "rules:\n  - path: api/\n    rule: x\n")
	id, _ := queue(env)

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Reported || !strings.HasPrefix(r.FailureReason, ".pergent/rules.yaml: entry 1: path:") {
		t.Fatalf("run = %s %q, want reported with the file's problem", r.State, r.FailureReason)
	}
	if len(env.Jobs()) != 0 {
		t.Error("a job was launched")
	}
	if a := attentionItem(t, env, attRulesInvalid); a == nil || !strings.HasPrefix(a.Message, ".pergent/rules.yaml: entry 1:") {
		t.Errorf("attention rules_invalid = %+v", a)
	}
}

func TestDispatchMissingRuleSetFails(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.Engine.Config.Repositories[0].RuleSets = []string{"gone"}
	id, _ := queue(env)

	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Reported || r.FailureReason != `rule set "gone" is not in the rule library` {
		t.Fatalf("run = %s %q", r.State, r.FailureReason)
	}
	if a := attentionItem(t, env, attRulesInvalid); a == nil {
		t.Error("no rules_invalid attention item")
	}
}

func TestDispatchConfigSetWins(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.Engine.Config.RuleSets = []config.ConfigRuleSet{{Name: "a", Content: setBYAML}}
	env.Engine.Config.Repositories[0].RuleSets = []string{"a"}
	addRuleSet(t, env, "a", setAYAML)
	id, _ := queue(env)

	dispatch(t, env)

	if list := jobRules(t, env, id); len(list) != 1 || list[0].Path != "*.ts" {
		t.Errorf("rules = %+v, want the config file's set", list)
	}
}

func TestDispatchNoBuiltInRules(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.Engine.Config.Repositories[0].BuiltInRules = nil
	id, _ := queue(env)

	dispatch(t, env)

	if v, ok := jobVar(t, env, id, "PERGENT_NO_BUILT_IN_RULES"); !ok || v != "true" {
		t.Errorf("PERGENT_NO_BUILT_IN_RULES = %q, %v", v, ok)
	}
	if _, ok := jobVar(t, env, id, "PERGENT_RULES"); ok {
		t.Error("PERGENT_RULES set with an empty list")
	}
}

// builtInListFloor sets the first pergent that loads only the chosen
// built-in rules for the test. It writes a package variable that dispatch
// reads, so a test that calls it must not call t.Parallel: the sequential
// tests all end before a parallel test starts.
func builtInListFloor(t *testing.T, v string) {
	t.Helper()
	old := config.BuiltInRulesListMinVersion
	config.BuiltInRulesListMinVersion = v
	t.Cleanup(func() { config.BuiltInRulesListMinVersion = old })
}

// Built-in rules are chosen per repository. None chosen runs with
// PERGENT_NO_BUILT_IN_RULES. Chosen ones reach the Job as
// PERGENT_BUILT_IN_RULES, in builtin.yaml order, only when jobs.image loads
// only those; any other image would load every built-in rule, so it gets
// PERGENT_NO_BUILT_IN_RULES and the run records why. The rules hash takes in
// the chosen list either way, so changing it gives one full review.
func TestDispatchBuiltInRulesChosen(t *testing.T) {
	for _, c := range []struct {
		name   string
		chosen []string
		floor  string // BuiltInRulesListMinVersion; testImage is v0.5.2
		list   string // PERGENT_BUILT_IN_RULES, "" for none
		note   string // a substring of the run's note, "" for none
		hashed []string
	}{
		{"none chosen", nil, "v0.5.0", "", "", nil},
		{"no image loads a list yet", []string{"yaml", "go"}, "", "", "no review image can load only the enabled built-in rules yet", []string{"yaml", "go"}},
		{"the image is too old", []string{"go"}, "v0.6.0", "", `image tag "v0.5.2" is older than v0.6.0`, []string{"go"}},
		{"the image loads the list", []string{"go", "yaml"}, "v0.5.2", "yaml,go", "", []string{"yaml", "go"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newTestEnv(t)
			builtInListFloor(t, c.floor)
			env.Engine.Config.Repositories[0].BuiltInRules = c.chosen
			id, _ := queue(env)

			dispatch(t, env)

			list, hasList := jobVar(t, env, id, "PERGENT_BUILT_IN_RULES")
			no, hasNo := jobVar(t, env, id, "PERGENT_NO_BUILT_IN_RULES")
			if c.list != "" {
				if !hasList || list != c.list || hasNo {
					t.Errorf("PERGENT_BUILT_IN_RULES = %q (%v), NO = %q (%v); want %q and no NO", list, hasList, no, hasNo, c.list)
				}
			} else if hasList || !hasNo || no != "true" {
				t.Errorf("PERGENT_BUILT_IN_RULES = %q (%v), NO = %q (%v); want NO only", list, hasList, no, hasNo)
			}
			r := env.Run(id)
			if c.note == "" && r.BuiltInRulesNote != "" || !strings.Contains(r.BuiltInRulesNote, c.note) {
				t.Errorf("note = %q, want %q", r.BuiltInRulesNote, c.note)
			}
			if r.RulesHash == nil || *r.RulesHash != inputs.RulesHash(nil, c.hashed, testImage) {
				t.Errorf("rules_hash = %q, want the hash with built-in %v", deref(r.RulesHash), c.hashed)
			}
		})
	}
}

// builtInRulesFor never gives an empty list to send: pergent reads that as
// every built-in rule.
func TestBuiltInRulesFor(t *testing.T) {
	builtInListFloor(t, "v1.0.0")
	if send, note := builtInRulesFor(nil, "x/pergent:v1.0.0"); send != nil || note != "" {
		t.Errorf("none chosen = %v, %q", send, note)
	}
	if send, note := builtInRulesFor([]string{}, "x/pergent:v1.0.0"); send != nil || note != "" {
		t.Errorf("an empty choice = %v, %q", send, note)
	}
	if send, note := builtInRulesFor([]string{"go"}, "x/pergent:v0.9.0"); send != nil || !strings.HasPrefix(note, "The enabled built-in rules were left out: ") {
		t.Errorf("an old image = %v, %q", send, note)
	}
	chosen := []string{"go"}
	send, note := builtInRulesFor(chosen, "x/pergent:v1.0.0")
	if !slices.Equal(send, chosen) || note != "" {
		t.Errorf("a new image = %v, %q", send, note)
	}
	send[0] = "changed"
	if chosen[0] != "go" {
		t.Error("builtInRulesFor gives out the repository's own slice")
	}
}

func TestDispatchRulesHashStored(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.GitLab.Reply(http.MethodGet, filePath(rule.RepoFile), http.StatusOK, repoRulesYAML)
	id, _ := queue(env)

	dispatch(t, env)

	list, _ := rule.Load([]byte(repoRulesYAML), rule.SourceRepo)
	r := env.Run(id)
	if r.RulesHash == nil || *r.RulesHash != inputs.RulesHash(list, nil, testImage) {
		t.Errorf("rules_hash = %v, want the hash of the list with built-ins off (the default) and the Jobs image", r.RulesHash)
	}
}

// The built-in texts ship inside the Jobs image: a run on another image gets
// another hash, so its next push reviews every skill with rules in full.
func TestDispatchRulesHashHasTheImage(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.Engine.Config.Jobs.Image = "ghcr.io/gopaytech/pergent:v9.9.9"
	env.GitLab.Reply(http.MethodGet, filePath(rule.RepoFile), http.StatusOK, repoRulesYAML)
	id, _ := queue(env)

	dispatch(t, env)

	list, _ := rule.Load([]byte(repoRulesYAML), rule.SourceRepo)
	r := env.Run(id)
	if r.RulesHash == nil || *r.RulesHash != inputs.RulesHash(list, nil, "ghcr.io/gopaytech/pergent:v9.9.9") {
		t.Errorf("rules_hash = %v, want the hash with the configured Jobs image", r.RulesHash)
	}
}

func TestDispatchRulesAttentionCleared(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.Engine.Config.Repositories[0].RuleSets = []string{"later"}
	queue(env)
	dispatch(t, env)
	if attentionItem(t, env, attRulesInvalid) == nil {
		t.Fatal("no rules_invalid attention item")
	}
	addRuleSet(t, env, "later", setAYAML)
	env.Enqueue(3, sha('c'))
	env.GitLab.SetMR(openMR(3, sha('c')))
	env.GitLab.Reply(http.MethodPost, statusPath(sha('c')), http.StatusCreated, `{}`)
	dispatch(t, env)
	if a := attentionItem(t, env, attRulesInvalid); a != nil {
		t.Errorf("rules_invalid still listed after a Job started: %+v", a)
	}
}
