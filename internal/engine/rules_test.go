package engine

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// ruleSource serves files at one ref and records each ref it is asked for.
type ruleSource struct {
	files map[string]string
	refs  []string
	err   error
}

func (s *ruleSource) GetFile(_ context.Context, _ int64, p, ref string) ([]byte, bool, error) {
	s.refs = append(s.refs, ref)
	if s.err != nil {
		return nil, false, s.err
	}
	c, ok := s.files[p]
	return []byte(c), ok, nil
}

func (s *ruleSource) ListTree(context.Context, int64, string, string) ([]forge.TreeEntry, error) {
	return nil, nil
}

// storedSets is a ruleSetReader over a fixed library and default list.
func storedSets(defaults []string, sets map[string]string) ruleSetReader {
	return func(_ context.Context, names []string) (store.ResolvedRuleSets, error) {
		if names == nil {
			names = defaults
		}
		out := store.ResolvedRuleSets{Names: names, Sets: map[string]store.RuleSet{}}
		for _, n := range names {
			if c, ok := sets[n]; ok {
				out.Sets[n] = store.RuleSet{Name: n, Content: c}
			} else if !slices.Contains(out.Missing, n) {
				out.Missing = append(out.Missing, n)
			}
		}
		return out, nil
	}
}

func labelsOf(list []rule.Entry) []string {
	var out []string
	for _, e := range list {
		out = append(out, e.Label())
	}
	return out
}

const (
	repoRulesFile = "rules:\n  - path: \"*.go\"\n    rule: repo\n"
	setA          = "rules:\n  - path: \"*.py\"\n    rule: a\n"
	setB          = "rules:\n  - path: \"*.ts\"\n    rule: b\n"
)

func TestRuleListOrderAndRef(t *testing.T) {
	src := &ruleSource{files: map[string]string{rule.RepoFile: repoRulesFile}}
	list, problem, err := ruleList(context.Background(), src, 1, "start-sha", []string{"b", "a"}, &config.Config{},
		storedSets(nil, map[string]string{"a": setA, "b": setB}), true)
	if err != nil || problem != "" {
		t.Fatalf("ruleList = %q, %v", problem, err)
	}
	if want := []string{"repo:*.go", "dashboard:b:*.ts", "dashboard:a:*.py"}; !slices.Equal(labelsOf(list), want) {
		t.Errorf("labels = %v, want %v", labelsOf(list), want)
	}
	if !slices.Equal(src.refs, []string{"start-sha"}) {
		t.Errorf("read at %v, want only the target ref", src.refs)
	}
}

func TestRuleListDefaultList(t *testing.T) {
	list, problem, err := ruleList(context.Background(), &ruleSource{}, 1, "ref", nil, &config.Config{},
		storedSets([]string{"a"}, map[string]string{"a": setA}), true)
	if err != nil || problem != "" || !slices.Equal(labelsOf(list), []string{"dashboard:a:*.py"}) {
		t.Errorf("ruleList = %v, %q, %v", labelsOf(list), problem, err)
	}
}

func TestRuleListBrokenRepoFile(t *testing.T) {
	src := &ruleSource{files: map[string]string{rule.RepoFile: "rules:\n  - path: api/\n    rule: x\n"}}
	_, problem, err := ruleList(context.Background(), src, 1, "ref", nil, &config.Config{}, storedSets(nil, nil), true)
	if err != nil || problem == "" || problem[:len(".pergent/rules.yaml: entry 1: path:")] != ".pergent/rules.yaml: entry 1: path:" {
		t.Errorf("ruleList = %q, %v; want the file's problem", problem, err)
	}
}

func TestRuleListMissingSet(t *testing.T) {
	_, problem, err := ruleList(context.Background(), &ruleSource{}, 1, "ref", []string{"gone"}, &config.Config{}, storedSets(nil, nil), true)
	if err != nil || problem != `rule set "gone" is not in the rule library` {
		t.Errorf("ruleList = %q, %v", problem, err)
	}
}

func TestRuleListStoredSetNoLongerLoads(t *testing.T) {
	_, problem, err := ruleList(context.Background(), &ruleSource{}, 1, "ref", []string{"old"}, &config.Config{},
		storedSets(nil, map[string]string{"old": "rules:\n  - path: a.go\n"}), true)
	if err != nil || problem != `rule set "old": entry 1: needs a rule or off: true` {
		t.Errorf("ruleList = %q, %v", problem, err)
	}
}

func TestRuleListConfigSetWins(t *testing.T) {
	cfg := &config.Config{RuleSets: []config.ConfigRuleSet{{Name: "a", Content: setB}}}
	list, problem, err := ruleList(context.Background(), &ruleSource{}, 1, "ref", []string{"a", "only-config"}, cfg,
		storedSets(nil, map[string]string{"a": setA}), true)
	if err == nil && problem == "" {
		t.Fatalf("only-config is in neither, but ruleList passed: %v", labelsOf(list))
	}
	cfg.RuleSets = append(cfg.RuleSets, config.ConfigRuleSet{Name: "only-config", Content: setA})
	list, problem, err = ruleList(context.Background(), &ruleSource{}, 1, "ref", []string{"a", "only-config"}, cfg,
		storedSets(nil, map[string]string{"a": setA}), true)
	if err != nil || problem != "" || !slices.Equal(labelsOf(list), []string{"dashboard:a:*.ts", "dashboard:only-config:*.py"}) {
		t.Errorf("ruleList = %v, %q, %v; want the config's a, then only-config", labelsOf(list), problem, err)
	}
}

func TestRuleListWithoutRepoFile(t *testing.T) {
	src := &ruleSource{files: map[string]string{rule.RepoFile: repoRulesFile}}
	list, problem, err := ruleList(context.Background(), src, 1, "ref", []string{"a"}, &config.Config{},
		storedSets(nil, map[string]string{"a": setA}), false)
	if err != nil || problem != "" || !slices.Equal(labelsOf(list), []string{"dashboard:a:*.py"}) || len(src.refs) != 0 {
		t.Errorf("ruleList = %v, %q, %v, refs %v; want the set only and no read", labelsOf(list), problem, err, src.refs)
	}
}

func TestRuleListSourceErrorIsAnError(t *testing.T) {
	_, problem, err := ruleList(context.Background(), &ruleSource{err: errors.New("boom")}, 1, "ref", nil, &config.Config{}, storedSets(nil, nil), true)
	if err == nil || problem != "" {
		t.Errorf("ruleList = %q, %v; want an error to retry", problem, err)
	}
}
