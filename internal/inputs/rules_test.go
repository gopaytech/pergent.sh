package inputs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent/rule"
)

func TestCheckSkillRulesField(t *testing.T) {
	for _, v := range []string{"true", "false", "False"} {
		if err := CheckSkill("s", []byte("---\nrules: "+v+"\n---\nReview.")); err != nil {
			t.Errorf("rules: %s: %v", v, err)
		}
	}
	for _, v := range []string{"yes", `"false"`, "1", "[a]"} {
		err := CheckSkill("s", []byte("---\nrules: "+v+"\n---\nReview."))
		if err == nil || !strings.Contains(err.Error(), "rules must be true or false") {
			t.Errorf("rules: %s: %v, want an error", v, err)
		}
	}
}

func TestSkillRules(t *testing.T) {
	for content, want := range map[string]bool{
		"Review.":                         true,
		"---\nname: s\n---\nReview.":      true,
		"---\nrules: true\n---\nReview.":  true,
		"---\nrules: false\n---\nReview.": false,
		"---\nrules: FALSE\n---\nReview.": false,
	} {
		got, err := SkillRules([]byte(content))
		if err != nil || got != want {
			t.Errorf("SkillRules(%q) = %v, %v; want %v", content, got, err, want)
		}
	}
	if _, err := SkillRules([]byte("---\nrules: yes\n---\n")); err == nil {
		t.Error("rules: yes was read")
	}
}

const goRules = "rules:\n  - path: \"*.go\"\n    rule: check\n"

func TestLoadRulesMissing(t *testing.T) {
	got, err := LoadRules(context.Background(), &fakeSource{files: map[string]string{}}, 1, "abc")
	if err != nil || got != nil {
		t.Errorf("LoadRules = %v, %v; want nothing", got, err)
	}
}

func TestLoadRulesReadsRef(t *testing.T) {
	src := &fakeSource{files: map[string]string{rule.RepoFile: goRules}}
	got, err := LoadRules(context.Background(), src, 1, "target-sha")
	if err != nil || len(got) != 1 || got[0] != (rule.Entry{Path: "*.go", Rule: "check", Source: rule.SourceRepo}) {
		t.Errorf("LoadRules = %+v, %v", got, err)
	}
	if !src.refs["target-sha"] || len(src.refs) != 1 {
		t.Errorf("read at %v, want only target-sha", src.refs)
	}
}

func TestLoadRulesMalformed(t *testing.T) {
	src := &fakeSource{files: map[string]string{rule.RepoFile: "rules:\n  - path: a.go\n"}}
	_, err := LoadRules(context.Background(), src, 1, "abc")
	var pe *ProblemError
	if !errors.As(err, &pe) || pe.Path != rule.RepoFile || err.Error() != ".pergent/rules.yaml: entry 1: needs a rule or off: true" {
		t.Errorf("LoadRules = %v, want a ProblemError naming the entry", err)
	}
}

func TestLoadRulesSourceError(t *testing.T) {
	src := &errSource{err: errors.New("boom")}
	_, err := LoadRules(context.Background(), src, 1, "abc")
	var pe *ProblemError
	if err == nil || errors.As(err, &pe) {
		t.Errorf("LoadRules = %v, want the source's error, not a problem", err)
	}
}

type errSource struct {
	fakeSource
	err error
}

func (e *errSource) GetFile(context.Context, int64, string, string) ([]byte, bool, error) {
	return nil, false, e.err
}

func TestRuleListOrder(t *testing.T) {
	repo := []rule.Entry{{Path: "*.go", Rule: "r", Source: rule.SourceRepo}}
	got, err := RuleList(repo, []NamedRuleSet{
		{Name: "b", Content: "rules:\n  - path: \"*.py\"\n    rule: b1\n  - path: \"*.ts\"\n    rule: b2\n"},
		{Name: "a", Content: "rules:\n  - path: \"*.md\"\n    off: true\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, e := range got {
		labels = append(labels, e.Label())
	}
	want := []string{"repo:*.go", "dashboard:b:*.py", "dashboard:b:*.ts", "dashboard:a:*.md"}
	if !slices.Equal(labels, want) {
		t.Errorf("labels = %v, want %v", labels, want)
	}
}

func TestRuleListBadStoredSet(t *testing.T) {
	_, err := RuleList(nil, []NamedRuleSet{{Name: "old", Content: "rules:\n  - path: api/\n    rule: x\n"}})
	var pe *ProblemError
	if !errors.As(err, &pe) || pe.Name != "old" || !strings.HasPrefix(err.Error(), `rule set "old": entry 1: path:`) {
		t.Errorf("RuleList = %v, want a ProblemError naming the set", err)
	}
}

func TestRulesHash(t *testing.T) {
	list := []rule.Entry{{Path: "*.go", Rule: "r", Source: rule.SourceRepo}}
	h := RulesHash(list, true)
	if len(h) != 64 || RulesHash(list, true) != h {
		t.Fatalf("RulesHash = %q, not stable", h)
	}
	if RulesHash(list, false) == h {
		t.Error("the switch does not change the hash")
	}
	if RulesHash([]rule.Entry{{Path: "*.go", Rule: "other", Source: rule.SourceRepo}}, true) == h {
		t.Error("the rule does not change the hash")
	}
	if RulesHash(nil, true) == RulesHash(nil, false) {
		t.Error("an empty list hashes the same with built-ins on and off")
	}
}

func TestSizeCountsRules(t *testing.T) {
	in := &Inputs{Skills: map[string][]byte{"s": []byte("abc")}, Rules: []byte("12345")}
	if in.Size() != 8 {
		t.Errorf("Size = %d, want 8", in.Size())
	}
}

func TestFitNeverDropsRules(t *testing.T) {
	in := &Inputs{Skills: map[string][]byte{"s": []byte("x")}, Rules: []byte(strings.Repeat("r", 100)),
		CIResults: []byte(strings.Repeat("c", 50)), Previous: []byte("p"), Carried: []byte("c")}
	if err := in.Fit(105); err != nil {
		t.Fatalf("Fit = %v", err)
	}
	if in.CIResults != nil || len(in.Rules) != 100 {
		t.Errorf("after Fit: CI %d bytes, rules %d bytes; want CI dropped, rules kept", len(in.CIResults), len(in.Rules))
	}
	err := in.Fit(50)
	if !errors.Is(err, ErrTooLarge) || len(in.Rules) != 100 || !strings.Contains(err.Error(), "skills, instructions and rules are") {
		t.Errorf("Fit(50) = %v, rules %d bytes; want ErrTooLarge and the rules kept", err, len(in.Rules))
	}
}
