package result

import (
	"slices"
	"testing"

	"github.com/gopaytech/pergent/render"
)

func TestParseFileRules(t *testing.T) {
	r, err := Parse([]byte(`{"schema_version":1,"status":"completed","rules":{"built_in":true,"entries":3},
		"skills":[{"name":"s","verdict":"no_blocking_issues","findings":[],"files":{"reviewed":["a.go"],"skipped":[],"not_reviewed":[],
		"rules":[{"path":"a.go","rule":"repo:*.go"}]},"failures":[],"notes":[]}],"comment":{"markdown":"x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Rules == nil || !r.Rules.BuiltIn || r.Rules.Entries != 3 {
		t.Errorf("Rules = %+v", r.Rules)
	}
	if got := r.Skills[0].Files.Rules; !slices.Equal(got, []FileRule{{Path: "a.go", Rule: "repo:*.go"}}) {
		t.Errorf("files.rules = %+v", got)
	}
	old, err := Parse([]byte(`{"schema_version":1,"status":"completed","skills":[{"name":"s","verdict":"no_blocking_issues","findings":[],
		"files":{"reviewed":["a.go"],"skipped":[],"not_reviewed":[]},"failures":[],"notes":[]}],"comment":{"markdown":"x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if old.Rules != nil || old.Skills[0].Files.Rules != nil {
		t.Errorf("an older result has rules: %+v %+v", old.Rules, old.Skills[0].Files.Rules)
	}
}

func TestToRenderFileRules(t *testing.T) {
	r := &Result{Skills: []Skill{{Name: "s", Verdict: "no_blocking_issues",
		Files: Files{Reviewed: []string{"a.go"}, Rules: []FileRule{{Path: "a.go", Rule: "repo:*.go"}}}}}}
	got := ToRender(r)
	if !slices.Equal(got[0].FileRules, []render.FileRule{{Path: "a.go", Rule: "repo:*.go"}}) {
		t.Errorf("FileRules = %+v", got[0].FileRules)
	}
}

// A carried file keeps the rule the last review checked it with; a file the
// new run reviewed takes the new rule.
func TestMergeIncrementalKeepsCarriedRules(t *testing.T) {
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues",
		Files: Files{Reviewed: []string{"kept.go", "again.go"}, Rules: []FileRule{{Path: "kept.go", Rule: "repo:*.go"}, {Path: "again.go", Rule: "repo:old"}}}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues", Batches: 1,
		Files: Files{Reviewed: []string{"again.go"}, Carried: []string{"kept.go"}, Rules: []FileRule{{Path: "again.go", Rule: "repo:new"}}}})
	s := mergeIncremental(t, partial, base).Skills[0]
	want := []FileRule{{Path: "again.go", Rule: "repo:new"}, {Path: "kept.go", Rule: "repo:*.go"}}
	if !slices.Equal(s.Files.Rules, want) {
		t.Errorf("files.rules = %+v, want %+v", s.Files.Rules, want)
	}
}
