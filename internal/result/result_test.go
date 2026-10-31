package result

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent/render"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParse(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		r, err := Parse(fixture(t, "completed.json"))
		if err != nil {
			t.Fatal(err)
		}
		if r.SchemaVersion != 1 || r.Status != "completed" || r.PergentVersion != "0.1.0" {
			t.Errorf("envelope: %+v", r)
		}
		if r.Base != "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b" || r.Head != "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b" {
			t.Errorf("base/head: %q %q", r.Base, r.Head)
		}
		if len(r.Skills) != 2 {
			t.Fatalf("skills: %d", len(r.Skills))
		}
		s := r.Skills[0]
		if s.Name != "code-review" || s.Verdict != "has_blocking_issues" || len(s.Findings) != 2 {
			t.Errorf("skill 0: %+v", s)
		}
		if len(s.Files.Reviewed) != 2 || len(s.Files.Skipped) != 1 || s.Files.Skipped[0] != (Skipped{Path: "go.sum", Rule: "lockfile"}) {
			t.Errorf("files: %+v", s.Files)
		}
		s1 := r.Skills[1]
		if s1.Verdict != "failed" || len(s1.Files.NotReviewed) != 1 {
			t.Errorf("skill 1: %+v", s1)
		}
		if !strings.HasPrefix(r.Comment.Markdown, "### code-review") || !strings.HasPrefix(r.Comment.FindingsBlock, "<!-- pergent:findings") {
			t.Errorf("comment: %+v", r.Comment)
		}
	})
	t.Run("error", func(t *testing.T) {
		r, err := Parse(fixture(t, "error.json"))
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "error" || r.Error == nil || r.Error.Code != "commit_missing" || r.Error.Message == "" {
			t.Errorf("error result: %+v", r)
		}
		if !strings.HasPrefix(r.Comment.Markdown, "<!-- pergent -->") {
			t.Errorf("comment: %q", r.Comment.Markdown)
		}
	})
	t.Run("unknown fields are accepted", func(t *testing.T) {
		in := `{"schema_version":1,"status":"error","future":{"a":1},"comment":{"markdown":"m","extra":2}}`
		if _, err := Parse([]byte(in)); err != nil {
			t.Fatal(err)
		}
	})
	rejects := map[string]string{
		"schema version 2":     `{"schema_version":2,"status":"error","comment":{"markdown":"m"}}`,
		"missing version":      `{"status":"error","comment":{"markdown":"m"}}`,
		"missing markdown":     `{"schema_version":1,"status":"error","comment":{}}`,
		"no comment":           `{"schema_version":1,"status":"error"}`,
		"unknown status":       `{"schema_version":1,"status":"partial","comment":{"markdown":"m"}}`,
		"empty status":         `{"schema_version":1,"comment":{"markdown":"m"}}`,
		"completed no skills":  `{"schema_version":1,"status":"completed","comment":{"markdown":"m"}}`,
		"completed empty list": `{"schema_version":1,"status":"completed","skills":[],"comment":{"markdown":"m"}}`,
		"not json":             `nope`,
		"empty":                ``,
	}
	for name, in := range rejects {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestTrimKeepsNamesAndFindingsOnly(t *testing.T) {
	out, err := Trim(fixture(t, "completed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("top-level keys: %v", got)
	}
	if string(got["schema_version"]) != "1" || string(got["status"]) != `"completed"` {
		t.Errorf("envelope: %s", out)
	}
	var skills []map[string]json.RawMessage
	if err := json.Unmarshal(got["skills"], &skills); err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 {
		t.Fatalf("skills: %d", len(skills))
	}
	for _, s := range skills {
		if len(s) != 2 {
			t.Errorf("skill keys: %v", s)
		}
	}
	var findings []map[string]any
	if err := json.Unmarshal(skills[0]["findings"], &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 || findings[0]["fingerprint"] != "a1b2c3d4e5f6" || findings[0]["suggested_fix"] == nil {
		t.Errorf("findings must be kept verbatim: %v", findings)
	}
	// a skill without findings still has a findings array, never null
	if string(skills[1]["findings"]) != "[]" {
		t.Errorf("empty findings: %s", skills[1]["findings"])
	}
}

func TestTrimRejectsInvalid(t *testing.T) {
	if _, err := Trim([]byte("nope")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseRejectsUndecodableFinding(t *testing.T) {
	data := `{"schema_version":1,"status":"completed","skills":[{"name":"a","verdict":"has_blocking_issues",
		"findings":["not an object"],"files":{}}],"comment":{"markdown":"x"}}`
	if _, err := Parse([]byte(data)); err == nil || !strings.Contains(err.Error(), `skill "a"`) {
		t.Fatalf("Parse = %v, want an error naming the skill", err)
	}
}

func TestToRender(t *testing.T) {
	data := `{"schema_version":1,"status":"completed","skills":[
	  {"name":"a","verdict":"failed",
	   "findings":[
	     {"id":"aaaaaa","fingerprint":"aaaaaa111111","file":"x.go","line":4,"severity":"blocking","title":"T","explanation":"E","suggested_fix":"F"},
	     {"fingerprint":"bbbbbb222222","file":"y.go","severity":"suggestion","title":"S","explanation":"E2"}],
	   "files":{"reviewed":["x.go"],"skipped":[{"path":"go.sum","rule":"lockfile"}],"not_reviewed":[{"path":"y.go","cause":"timeout"}]},
	   "failures":[{"files":["y.go"],"message":"timed out","cause":{"kind":"timeout","message":"m"}}],
	   "notes":["n1"],"summaries":[{"batch":1,"text":"sum"}],"batches":2,"batches_finished":1,
	   "checks":[{"batch":1,"dropped":3,"findings":1,"checks":[{"section":"s","name":"c","status":"fail","detail":"d"}]}]}],
	  "comment":{"markdown":"x"}}`
	r, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	got := ToRender(r)
	want := []render.Skill{{
		Name: "a", Verdict: "failed",
		Findings: []render.Finding{
			{ID: "aaaaaa", Fingerprint: "aaaaaa111111", File: "x.go", Line: 4, Severity: render.Blocking, Title: "T", Explanation: "E", SuggestedFix: "F"},
			{ID: "bbbbbb", Fingerprint: "bbbbbb222222", File: "y.go", Severity: render.Suggestion, Title: "S", Explanation: "E2"},
		},
		Failures: []render.Failure{{Files: []string{"y.go"}, Message: "timed out"}},
		Skipped:  []render.Skipped{{Path: "go.sum", Rule: "lockfile"}},
		Reviewed: []string{"x.go"}, Notes: []string{"n1"},
		Batches: 2, BatchesFinished: 1,
		Summaries: []render.Summary{{Batch: 1, Text: "sum"}},
		Checks: []render.BatchChecks{{Batch: 1, Dropped: 3, Findings: 1,
			Checks: []render.Check{{Section: "s", Name: "c", Status: "fail", Detail: "d"}}}},
	}}
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Errorf("ToRender =\n%s\nwant\n%s", gj, wj)
	}
	if files := ChangedFiles(r); strings.Join(files, ",") != "go.sum,x.go,y.go" {
		t.Errorf("ChangedFiles = %v, want go.sum, x.go, y.go", files)
	}
}

func TestFailedSkills(t *testing.T) {
	skill := func(name, verdict string, notReviewed int) Skill {
		s := Skill{Name: name, Verdict: verdict}
		for i := 0; i < notReviewed; i++ {
			s.Files.NotReviewed = append(s.Files.NotReviewed, json.RawMessage(`{"path":"a.go"}`))
		}
		return s
	}
	for _, tc := range []struct {
		name      string
		r         *Result
		wantNames []string
		wantWhole bool
	}{
		{"error result", &Result{Status: "error", Skills: []Skill{skill("a", "failed", 0)}}, nil, true},
		{"no skills", &Result{Status: "completed"}, nil, true},
		{"nothing failed", &Result{Status: "completed", Skills: []Skill{
			skill("a", "no_blocking_issues", 0), skill("b", "has_blocking_issues", 0)}}, nil, false},
		{"failed verdict", &Result{Status: "completed", Skills: []Skill{
			skill("a", "no_blocking_issues", 0), skill("b", "failed", 0)}}, []string{"b"}, false},
		{"clean verdict with files not reviewed", &Result{Status: "completed", Skills: []Skill{
			skill("a", "no_blocking_issues", 1), skill("b", "no_blocking_issues", 0)}}, []string{"a"}, false},
		{"blocking with files not reviewed is not failed", &Result{Status: "completed", Skills: []Skill{
			skill("a", "has_blocking_issues", 1)}}, nil, false},
		{"unknown verdict counts as failed, as the policy does", &Result{Status: "completed", Skills: []Skill{
			skill("a", "maybe", 0)}}, []string{"a"}, false},
		{"order follows the result", &Result{Status: "completed", Skills: []Skill{
			skill("z", "failed", 0), skill("a", "failed", 0)}}, []string{"z", "a"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names, whole := FailedSkills(tc.r, nil)
			if whole != tc.wantWhole || !slices.Equal(names, tc.wantNames) {
				t.Errorf("FailedSkills = %v, %v; want %v, %v", names, whole, tc.wantNames, tc.wantWhole)
			}
		})
	}
}

// mergeInput is a completed result of base b and head h with the skills, each
// finding given as "skill:fingerprint:severity".
func mergeInput(t *testing.T, head string, skills map[string][]string, order ...string) []byte {
	t.Helper()
	var sk []Skill
	for _, name := range order {
		s := Skill{Name: name, Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{name + ".go"}}}
		for _, fp := range skills[name] {
			b, _ := json.Marshal(Finding{ID: fp[:6], Fingerprint: fp, File: name + ".go", Severity: "suggestion", Title: "t " + fp})
			s.Findings = append(s.Findings, b)
		}
		sk = append(sk, s)
	}
	b, err := json.Marshal(Result{SchemaVersion: 1, Status: "completed", PergentVersion: "0.1.0", Base: "b", Head: head,
		Skills: sk, Comment: Comment{Markdown: "subset comment", FindingsBlock: "<!-- pergent:findings [] -->"}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMerge(t *testing.T) {
	base := mergeInput(t, "h", map[string][]string{"sec": {"aaaaaa000001"}, "style": {"bbbbbb000001"}}, "sec", "style")
	partial := mergeInput(t, "h", map[string][]string{"style": {"cccccc000001"}}, "style")

	merged, err := Merge(partial, base, []string{"style"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(merged)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Merged || r.MergeNote != "" {
		t.Errorf("merged flag = %v, note = %q", r.Merged, r.MergeNote)
	}
	if len(r.Skills) != 2 || r.Skills[0].Name != "sec" || r.Skills[1].Name != "style" {
		t.Fatalf("skills = %+v, want sec kept then style re-run", r.Skills)
	}
	fp := func(s Skill) string {
		var f Finding
		_ = json.Unmarshal(s.Findings[0], &f)
		return f.Fingerprint
	}
	if fp(r.Skills[0]) != "aaaaaa000001" || fp(r.Skills[1]) != "cccccc000001" {
		t.Errorf("findings: kept %s, new %s", fp(r.Skills[0]), fp(r.Skills[1]))
	}
	if r.Head != "h" || r.Comment.Markdown != "subset comment" || r.PergentVersion != "0.1.0" {
		t.Errorf("envelope is not the new result's: %+v", r)
	}
}

// The kept result's fields that Result doesn't declare survive the merge.
func TestMergeKeepsUndeclaredFields(t *testing.T) {
	base := mergeInput(t, "h", map[string][]string{"sec": nil, "style": nil}, "sec", "style")
	base = []byte(strings.Replace(string(base), `"name":"sec"`, `"name":"sec","tokens":{"input":7}`, 1))
	partial := mergeInput(t, "h", map[string][]string{"style": nil}, "style")
	merged, err := Merge(partial, base, []string{"style"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), `"tokens":{"input":7}`) {
		t.Errorf("kept skill lost its tokens: %s", merged)
	}
}

func TestMergeAddsASkillTheBaseLacks(t *testing.T) {
	base := mergeInput(t, "h", map[string][]string{"sec": nil}, "sec")
	partial := mergeInput(t, "h", map[string][]string{"style": nil}, "style")
	merged, err := Merge(partial, base, []string{"style"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := Parse(merged)
	if len(r.Skills) != 2 || r.Skills[0].Name != "sec" || r.Skills[1].Name != "style" {
		t.Errorf("skills = %+v", r.Skills)
	}
}

// A skill that was asked for and has no result is failed, not dropped: a
// dropped skill would count as reviewed and clean.
func TestMergeFailsSubsetSkillsTheNewResultLacks(t *testing.T) {
	base := mergeInput(t, "h", map[string][]string{"sec": nil, "gone": nil}, "sec", "gone")
	partial := mergeInput(t, "h", map[string][]string{"sec": nil}, "sec")
	merged, err := Merge(partial, base, []string{"sec", "gone", "new"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := Parse(merged)
	if len(r.Skills) != 3 || r.Skills[0].Name != "sec" || r.Skills[0].Verdict != "no_blocking_issues" {
		t.Fatalf("skills = %+v", r.Skills)
	}
	for _, i := range []int{1, 2} {
		s := r.Skills[i]
		if s.Verdict != "failed" || len(s.Failures) != 1 || s.Failures[0].Message != failedMessage {
			t.Errorf("skill %d = %+v, want failed with the reason", i, s)
		}
	}
	if r.Skills[1].Name != "gone" || r.Skills[2].Name != "new" {
		t.Errorf("order = %s, %s", r.Skills[1].Name, r.Skills[2].Name)
	}
	if names, _ := FailedSkills(r, nil); !slices.Equal(names, []string{"gone", "new"}) {
		t.Errorf("FailedSkills = %v", names)
	}
}

func TestMergeRefusesWhatCannotBeKept(t *testing.T) {
	good := mergeInput(t, "h", map[string][]string{"sec": nil, "style": nil}, "sec", "style")
	partial := mergeInput(t, "h", map[string][]string{"style": nil}, "style")
	otherHead := mergeInput(t, "other", map[string][]string{"sec": nil, "style": nil}, "sec", "style")
	errorResult := []byte(`{"schema_version":1,"status":"error","base":"b","head":"h","comment":{"markdown":"x"}}`)

	for name, base := range map[string][]byte{
		"not json":        []byte(`nope`),
		"error result":    errorResult,
		"another commit":  otherHead,
		"empty":           nil,
		"unknown version": []byte(`{"schema_version":9}`),
	} {
		if _, err := Merge(partial, base, []string{"style"}); err == nil {
			t.Errorf("%s: merged", name)
		}
	}
	if _, err := Merge(errorResult, good, []string{"style"}); err == nil {
		t.Error("an error result was merged")
	}
	if _, err := Merge(partial, good, nil); err == nil {
		t.Error("merged with no subset")
	}
}

func TestMergedIDsAreReassigned(t *testing.T) {
	// The kept and the new finding share their first 6 characters.
	base := mergeInput(t, "h", map[string][]string{"sec": {"abcdef111111"}, "style": nil}, "sec", "style")
	partial := mergeInput(t, "h", map[string][]string{"style": {"abcdef222222"}}, "style")
	merged, err := Merge(partial, base, []string{"style"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := Parse(merged)
	var ids []string
	for _, s := range r.Skills {
		for _, raw := range s.Findings {
			var f Finding
			_ = json.Unmarshal(raw, &f)
			ids = append(ids, f.ID)
		}
	}
	if !slices.Equal(ids, []string{"abcdef11", "abcdef22"}) {
		t.Errorf("ids = %v, want both widened to 8 characters", ids)
	}
}

func TestNoteUnmerged(t *testing.T) {
	partial := mergeInput(t, "h", map[string][]string{"style": nil}, "style")
	out, err := NoteUnmerged(partial, NoteBaseUnreadable)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if r.MergeNote != NoteBaseUnreadable || r.Merged || len(r.Skills) != 1 {
		t.Errorf("result = %+v", r)
	}
}

func TestFailedSkillsAfterDismissals(t *testing.T) {
	const fp = "abc123def456"
	f, _ := json.Marshal(Finding{ID: "abc123", Fingerprint: fp, File: "a.go", Severity: "blocking", Title: "t"})
	blocking := Skill{Name: "a", Verdict: "has_blocking_issues", Findings: []json.RawMessage{f},
		Files: Files{NotReviewed: []json.RawMessage{json.RawMessage(`{"path":"b.go"}`)}}}
	r := &Result{Status: "completed", Skills: []Skill{blocking, {Name: "c", Verdict: "has_blocking_issues", Findings: []json.RawMessage{f}}}}

	if names, _ := FailedSkills(r, nil); len(names) != 0 {
		t.Errorf("without dismissals = %v, want none", names)
	}
	// The blocking finding is dismissed: the policy then decides a clean
	// verdict with files not reviewed, which is a failure.
	names, whole := FailedSkills(r, []render.Dismissal{{Fingerprint: fp, Severity: "blocking"}})
	if whole || !slices.Equal(names, []string{"a"}) {
		t.Errorf("with the dismissal = %v, %v; want [a]", names, whole)
	}
	// A suggestion dismissal doesn't cover a blocking finding.
	if names, _ := FailedSkills(r, []render.Dismissal{{Fingerprint: fp, Severity: "suggestion"}}); len(names) != 0 {
		t.Errorf("with a suggestion dismissal = %v, want none", names)
	}
}
