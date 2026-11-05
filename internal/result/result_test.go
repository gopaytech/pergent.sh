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

// A "review failed" re-run over an incremental review keeps what the review
// covered, or its comment would read like a full review.
func TestMergeKeepsTheBasesIncremental(t *testing.T) {
	base := mergeInput(t, "h", map[string][]string{"sec": nil, "style": nil}, "sec", "style")
	var top map[string]any
	if err := json.Unmarshal(base, &top); err != nil {
		t.Fatal(err)
	}
	top["incremental"] = map[string]any{"reviewed": 2, "carried": 28, "from": fromSHA}
	base, _ = json.Marshal(top)
	partial := mergeInput(t, "h", map[string][]string{"style": nil}, "style")

	merged, err := Merge(partial, base, []string{"style"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := Parse(merged)
	if want := (&Incremental{Reviewed: 2, Carried: 28, From: fromSHA}); r.Incremental == nil || *r.Incremental != *want {
		t.Errorf("incremental = %+v, want %+v", r.Incremental, want)
	}

	// Every skill re-run over all its files: nothing is carried over any
	// more, so the merge says nothing of it.
	all := mergeInput(t, "h", map[string][]string{"sec": nil, "style": nil}, "sec", "style")
	merged, err = Merge(all, base, []string{"sec", "style"})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := Parse(merged); r.Incremental != nil {
		t.Errorf("incremental = %+v, want none when every skill was re-run", r.Incremental)
	}

	// A base that was a full review leaves the merge a full review.
	full := mergeInput(t, "h", map[string][]string{"sec": nil, "style": nil}, "sec", "style")
	merged, err = Merge(partial, full, []string{"style"})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := Parse(merged); r.Incremental != nil {
		t.Errorf("incremental = %+v, want none", r.Incremental)
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

const fromSHA = "abc1234def5678abc1234def5678abc1234def56"

func incFinding(file, fp, severity string) json.RawMessage {
	b, _ := json.Marshal(Finding{ID: fp[:6], Fingerprint: fp, File: file, Severity: severity, Title: "t " + fp})
	return b
}

// incResult is a completed result of head h with the skills.
func incResult(t *testing.T, head string, skills ...Skill) []byte {
	t.Helper()
	b, err := json.Marshal(Result{SchemaVersion: 1, Status: "completed", PergentVersion: "0.5.0", Base: "b", Head: head,
		Skills: skills, Comment: Comment{Markdown: "partial comment", FindingsBlock: "<!-- pergent:findings [] -->"}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mergeIncremental(t *testing.T, partial, base []byte) *Result {
	t.Helper()
	out, err := MergeIncremental(partial, base, fromSHA)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func skillFindings(t *testing.T, s Skill) []Finding {
	t.Helper()
	var out []Finding
	for _, raw := range s.Findings {
		var f Finding
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func TestMergeIncrementalFindings(t *testing.T) {
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues",
		Files: Files{Reviewed: []string{"kept.go", "other.go", "changed.go"}},
		Findings: []json.RawMessage{
			incFinding("kept.go", "abcdef111111", "suggestion"),
			incFinding("other.go", "bbbbbb000001", "suggestion"),
			incFinding("changed.go", "cccccc000001", "suggestion"),
		}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues", Batches: 1,
		Files:    Files{Reviewed: []string{"changed.go"}, Carried: []string{"kept.go"}},
		Findings: []json.RawMessage{incFinding("changed.go", "abcdef222222", "suggestion")}})

	r := mergeIncremental(t, partial, base)
	fs := skillFindings(t, r.Skills[0])
	if len(fs) != 2 || fs[0].Fingerprint != "abcdef222222" || fs[1].Fingerprint != "abcdef111111" {
		t.Fatalf("findings = %+v, want the new one, then the one on the carried file", fs)
	}
	// The two share their first 6 characters, so both IDs are widened.
	if fs[0].ID != "abcdef22" || fs[1].ID != "abcdef11" {
		t.Errorf("ids = %s, %s, want both widened to 8 characters", fs[0].ID, fs[1].ID)
	}
	if !r.Merged || r.Comment.Markdown != "partial comment" || r.Head != "h" {
		t.Errorf("envelope is not the partial's: %+v", r)
	}
}

// The note that a finding was left out of a review because of a past dismissal
// survives a merge while the file it was left out of is carried: it may be the
// only trace of a left-out blocker. The other notes are the new run's.
func TestMergeIncrementalKeepsLeftOutNotes(t *testing.T) {
	leftOut := LeftOutNotePrefix + " Token compared with == (Token compared with == in api/auth.go)"
	again := LeftOutNotePrefix + " Another (Another in x.go)"
	base := incResult(t, fromSHA,
		Skill{Name: "carries", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go", "n.go"}},
			Notes: []string{leftOut, again, "Verification dropped 1 of 1 blocking findings"}},
		Skill{Name: "reruns", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go", "n.go"}},
			Notes: []string{leftOut}})
	partial := incResult(t, "h",
		Skill{Name: "carries", Verdict: "no_blocking_issues", Batches: 1,
			Files: Files{Reviewed: []string{"n.go"}, Carried: []string{"k.go"}}, Notes: []string{"fresh note", again}},
		Skill{Name: "reruns", Verdict: "no_blocking_issues", Batches: 1, Files: Files{Reviewed: []string{"k.go", "n.go"}}})

	r := mergeIncremental(t, partial, base)
	if want := []string{"fresh note", again, leftOut}; !slices.Equal(r.Skills[0].Notes, want) {
		t.Errorf("notes of the skill that carries = %q, want the new run's, then the left-out note it lacked: %q", r.Skills[0].Notes, want)
	}
	if len(r.Skills[1].Notes) != 0 {
		t.Errorf("notes of the skill that reviewed everything = %q, want none", r.Skills[1].Notes)
	}
}

// The kept notes are the last review's newest, at most maxKeptLeftOutNotes of
// them, and the new run's notes always stay on top of that. Other notes are
// the new run's.
func TestKeepLeftOutNotesIsCapped(t *testing.T) {
	lo := func(s string) string { return LeftOutNotePrefix + " " + s }

	var earlier []string
	for _, e := range []string{"n1 (a)", "n2 (b)", "n3 (c)", "n4 (d)", "n5 (e)", "n6 (f)", "n7 (g)"} { // newest first
		earlier = append(earlier, lo(e))
	}
	earlier = append(earlier[:2], append([]string{"Verification dropped 1 of 1 blocking findings"}, earlier[2:]...)...)
	fresh := []string{lo("f1 (x)"), "a plain note"}

	got, kept := keepLeftOutNotes(fresh, earlier)
	want := append(slices.Clone(fresh), lo("n1 (a)"), lo("n2 (b)"), lo("n3 (c)"), lo("n4 (d)"), lo("n5 (e)"))
	if !kept || !slices.Equal(got, want) {
		t.Errorf("notes = %q, kept = %t; want the fresh notes and the newest five: %q", got, kept, want)
	}

	// An older note never evicts a newer one, even one that covers it.
	var many []string
	for _, e := range []string{"a", "b", "c", "d", "e", "f", "a; g"} {
		many = append(many, lo(e))
	}
	got, _ = keepLeftOutNotes(nil, many)
	if want := many[:5]; !slices.Equal(got, want) {
		t.Errorf("notes = %q, want the newest five: %q", got, want)
	}
	// Notes that are not left-out notes are not kept.
	if got, kept = keepLeftOutNotes(nil, []string{"Verification dropped 1"}); kept || len(got) != 0 {
		t.Errorf("notes = %q, kept = %t; want none", got, kept)
	}
}

// A note is dropped when the notes before it already say everything it does,
// entry by entry. Whole entries are compared: text that another entry merely
// contains is another finding.
func TestKeepLeftOutNotesCoverage(t *testing.T) {
	lo := func(s string) string { return LeftOutNotePrefix + " " + s }
	tests := []struct {
		name          string
		fresh, before []string
		want          []string
	}{
		{"covered by a longer note kept before it",
			nil, []string{lo("A (x); B (y)"), lo("A (x)"), lo("C (z)")}, []string{lo("A (x); B (y)"), lo("C (z)")}},
		{"a shorter note kept before a longer one that covers it",
			nil, []string{lo("A (x)"), lo("A (x); B (y)")}, []string{lo("A (x)"), lo("A (x); B (y)")}},
		{"covered by a new note",
			[]string{lo("A (x); B (y)")}, []string{lo("A (x)")}, []string{lo("A (x); B (y)")}},
		{"a title another title contains",
			nil, []string{lo("Missing nil check on response (r)"), lo("Missing nil check")},
			[]string{lo("Missing nil check on response (r)"), lo("Missing nil check")}},
		{"a title with the same ending",
			nil, []string{lo("Not A (x)"), lo("A (x)")}, []string{lo("Not A (x)"), lo("A (x)")}},
		{"a title that looks like the prefix",
			nil, []string{lo(LeftOutNotePrefix + " A (x)"), lo("A (x)")}, []string{lo(LeftOutNotePrefix + " A (x)"), lo("A (x)")}},
		{"the end of a long note does not count",
			nil, []string{lo("A (x); and 3 more"), lo("A (x)"), lo("B (y)")}, []string{lo("A (x); and 3 more"), lo("B (y)")}},
		{"the same note twice",
			nil, []string{lo("A (x)"), lo("A (x)")}, []string{lo("A (x)")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := keepLeftOutNotes(tc.fresh, tc.before)
			if !slices.Equal(got, tc.want) {
				t.Errorf("notes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMergeIncrementalFiles(t *testing.T) {
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues",
		Files: Files{Reviewed: []string{"kept.go", "old.go"}, Skipped: []Skipped{{Path: "old.lock", Rule: "lockfile"}},
			NotReviewed: []json.RawMessage{json.RawMessage(`{"path":"old.go"}`)}},
		Failures: []Failure{{Files: []string{"old.go"}, Message: "old failure"}}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "failed", Batches: 1,
		Files: Files{Reviewed: []string{"new.go"}, Carried: []string{"kept.go"}, Skipped: []Skipped{{Path: "new.lock", Rule: "lockfile"}},
			NotReviewed: []json.RawMessage{json.RawMessage(`{"path":"big.go"}`)}},
		Failures: []Failure{{Files: []string{"big.go"}, Message: "new failure"}}})

	s := mergeIncremental(t, partial, base).Skills[0]
	if !slices.Equal(s.Files.Reviewed, []string{"new.go", "kept.go"}) {
		t.Errorf("reviewed = %v, want the new files, then the carried", s.Files.Reviewed)
	}
	if !slices.Equal(s.Files.Carried, []string{"kept.go"}) {
		t.Errorf("carried = %v", s.Files.Carried)
	}
	if len(s.Files.Skipped) != 1 || s.Files.Skipped[0].Path != "new.lock" {
		t.Errorf("skipped = %+v, want the new run's", s.Files.Skipped)
	}
	if len(s.Files.NotReviewed) != 1 || !strings.Contains(string(s.Files.NotReviewed[0]), "big.go") {
		t.Errorf("not_reviewed = %s, want the new run's", s.Files.NotReviewed)
	}
	if len(s.Failures) != 1 || s.Failures[0].Message != "new failure" {
		t.Errorf("failures = %+v, want the new run's", s.Failures)
	}
}

func TestMergeIncrementalVerdict(t *testing.T) {
	base := incResult(t, fromSHA,
		Skill{Name: "failing", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go"}}},
		Skill{Name: "carries", Verdict: "has_blocking_issues", Files: Files{Reviewed: []string{"k.go"}},
			Findings: []json.RawMessage{incFinding("k.go", "aaaaaa000001", "blocking")}},
		Skill{Name: "clean", Verdict: "has_blocking_issues", Files: Files{Reviewed: []string{"k.go", "n.go"}},
			Findings: []json.RawMessage{
				incFinding("k.go", "bbbbbb000001", "suggestion"),
				incFinding("n.go", "cccccc000001", "blocking"),
			}})
	partial := incResult(t, "h",
		Skill{Name: "failing", Verdict: "failed", Batches: 1, Files: Files{Reviewed: []string{"n.go"}, Carried: []string{"k.go"}}},
		Skill{Name: "carries", Verdict: "no_blocking_issues", Batches: 1, Files: Files{Reviewed: []string{"n.go"}, Carried: []string{"k.go"}}},
		// The blocking finding was on n.go, which is reviewed again and found clean.
		Skill{Name: "clean", Verdict: "no_blocking_issues", Batches: 1, Files: Files{Reviewed: []string{"n.go"}, Carried: []string{"k.go"}}})

	r := mergeIncremental(t, partial, base)
	for i, want := range []string{"failed", "has_blocking_issues", "no_blocking_issues"} {
		if r.Skills[i].Verdict != want {
			t.Errorf("%s: verdict = %s, want %s", r.Skills[i].Name, r.Skills[i].Verdict, want)
		}
	}
}

func TestMergeIncrementalChecks(t *testing.T) {
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go"}},
		Checks: []BatchChecks{
			{Batch: 1, Findings: 2, Checks: []Check{{Section: "A", Name: "x", Status: "pass"}, {Section: "A", Name: "y", Status: "pass"}}},
			{Batch: 2, Checks: []Check{{Section: "B", Name: "z", Status: "pass"}}},
			{Batch: 3, Checks: []Check{{Section: "C", Name: "w", Status: "pass"}}},
		}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues", Batches: 1, Files: Files{Reviewed: []string{"n.go"}, Carried: []string{"k.go"}},
		Checks: []BatchChecks{
			{Batch: 1, Dropped: 1, Checks: []Check{{Section: "A", Name: "x", Status: "fail", Detail: "now"}, {Section: "C", Name: "w", Status: "pass"}}},
		}})

	got := mergeIncremental(t, partial, base).Skills[0].Checks
	if len(got) != 3 {
		t.Fatalf("checks = %+v, want 3 batches (the emptied one is gone)", got)
	}
	for i, b := range got {
		if b.Batch != i+1 {
			t.Errorf("batch %d is numbered %d", i, b.Batch)
		}
	}
	if len(got[0].Checks) != 1 || got[0].Checks[0].Name != "y" || got[0].Findings != 2 {
		t.Errorf("first batch = %+v, want the base's batch without the replaced check", got[0])
	}
	if len(got[1].Checks) != 1 || got[1].Checks[0].Name != "z" {
		t.Errorf("second batch = %+v, want the base's untouched batch", got[1])
	}
	if len(got[2].Checks) != 2 || got[2].Checks[0].Status != "fail" || got[2].Checks[0].Detail != "now" || got[2].Dropped != 1 {
		t.Errorf("third batch = %+v, want the new run's", got[2])
	}
}

// A skill that started no run has nothing of its own to say, so it keeps what
// the last review said; one that ran takes the new run's.
func TestMergeIncrementalSkillDidNotRun(t *testing.T) {
	full := func(name string) Skill {
		return Skill{Name: name, Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go"}},
			Confidence: 4, ConfidenceReason: "base reason", Notes: []string{"base note"},
			Summaries: []Summary{{Batch: 1, Text: "base summary"}}, Batches: 3, BatchesFinished: 3,
			Checks: []BatchChecks{{Batch: 1, Checks: []Check{{Section: "A", Name: "x", Status: "pass"}}}}}
	}
	base := incResult(t, fromSHA, full("idle"), full("ran"))
	partial := incResult(t, "h",
		Skill{Name: "idle", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{}, Carried: []string{"k.go"}}},
		Skill{Name: "ran", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"n.go"}, Carried: []string{"k.go"}},
			Confidence: 2, ConfidenceReason: "new reason", Notes: []string{"new note"},
			Summaries: []Summary{{Batch: 1, Text: "new summary"}}, Batches: 1, BatchesFinished: 1,
			Checks: []BatchChecks{{Batch: 1, Checks: []Check{{Section: "B", Name: "y", Status: "pass"}}}}})

	r := mergeIncremental(t, partial, base)
	idle := r.Skills[0]
	if idle.Confidence != 4 || idle.ConfidenceReason != "base reason" || !slices.Equal(idle.Notes, []string{"base note"}) ||
		len(idle.Summaries) != 1 || idle.Summaries[0].Text != "base summary" || idle.Batches != 3 || idle.BatchesFinished != 3 ||
		len(idle.Checks) != 1 || idle.Checks[0].Checks[0].Name != "x" {
		t.Errorf("idle skill = %+v, want the base's", idle)
	}
	ran := r.Skills[1]
	if ran.Confidence != 2 || ran.ConfidenceReason != "new reason" || !slices.Equal(ran.Notes, []string{"new note"}) ||
		len(ran.Summaries) != 1 || ran.Summaries[0].Text != "new summary" || ran.Batches != 1 || ran.BatchesFinished != 1 {
		t.Errorf("skill that ran = %+v, want the new run's", ran)
	}
	if len(ran.Checks) != 2 {
		t.Errorf("checks of the skill that ran = %+v, want the base's and the new run's", ran.Checks)
	}
}

// A carried blocking finding is one that was dismissed. It keeps its
// fingerprint, so the dismissal still applies and the verdict stays clean.
func TestMergeIncrementalDismissedBlockerKept(t *testing.T) {
	const fp = "dddddd000001"
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "has_blocking_issues", Files: Files{Reviewed: []string{"k.go"}},
		Findings: []json.RawMessage{incFinding("k.go", fp, "blocking")}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{}, Carried: []string{"k.go"}}})

	r := mergeIncremental(t, partial, base)
	fs := skillFindings(t, r.Skills[0])
	if len(fs) != 1 || fs[0].Fingerprint != fp || fs[0].Severity != "blocking" {
		t.Fatalf("findings = %+v", fs)
	}
	if r.Skills[0].Verdict != "has_blocking_issues" {
		t.Errorf("verdict = %s, want has_blocking_issues before the dismissal", r.Skills[0].Verdict)
	}
	got := render.ApplyDismissals(ToRender(r), []render.Dismissal{{Fingerprint: fp, Severity: "blocking"}})
	if got[0].Verdict != "no_blocking_issues" {
		t.Errorf("verdict after the dismissal = %s, want no_blocking_issues", got[0].Verdict)
	}
}

// The skills are the partial's: a skill the base lacks is the partial's as it
// is, and one only the base has is gone.
func TestMergeIncrementalSkillsArePartials(t *testing.T) {
	base := incResult(t, fromSHA,
		Skill{Name: "gone", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go"}}},
		Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go"}}})
	partial := incResult(t, "h",
		Skill{Name: "fresh", Verdict: "has_blocking_issues", Batches: 1, Files: Files{Reviewed: []string{"k.go", "n.go"}},
			Findings: []json.RawMessage{incFinding("n.go", "eeeeee000001", "blocking")}},
		Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{}, Carried: []string{"k.go"}}})
	partial = []byte(strings.Replace(string(partial), `"name":"fresh"`, `"name":"fresh","tokens":{"input":7}`, 1))

	out, err := MergeIncremental(partial, base, fromSHA)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := Parse(out)
	if len(r.Skills) != 2 || r.Skills[0].Name != "fresh" || r.Skills[1].Name != "sec" {
		t.Fatalf("skills = %+v, want the partial's, in its order", r.Skills)
	}
	if r.Skills[0].Verdict != "has_blocking_issues" || len(r.Skills[0].Findings) != 1 || !strings.Contains(string(out), `"tokens":{"input":7}`) {
		t.Errorf("new skill is not the partial's as it is: %s", out)
	}
}

func TestMergeIncrementalCounts(t *testing.T) {
	base := incResult(t, fromSHA,
		Skill{Name: "a", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"x.go", "y.go", "z.go", "w.go"}}},
		Skill{Name: "b", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"x.go", "y.go", "z.go", "w.go"}}})
	partial := incResult(t, "h",
		Skill{Name: "a", Verdict: "no_blocking_issues", Batches: 1, Files: Files{Reviewed: []string{"x.go", "y.go"}, Carried: []string{"z.go", "w.go"}}},
		Skill{Name: "b", Verdict: "no_blocking_issues", Batches: 1, Files: Files{Reviewed: []string{"z.go"}, Carried: []string{"x.go", "y.go", "w.go"}}})

	r := mergeIncremental(t, partial, base)
	want := &Incremental{Reviewed: 3, Carried: 1, From: fromSHA}
	if r.Incremental == nil || *r.Incremental != *want {
		t.Errorf("incremental = %+v, want %+v", r.Incremental, want)
	}
	if !r.Merged {
		t.Error("the merged result is not marked merged")
	}
}

func TestMergeIncrementalRefuses(t *testing.T) {
	skill := Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go"}}}
	good := incResult(t, fromSHA, skill)
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{}, Carried: []string{"k.go"}}})
	errorResult := []byte(`{"schema_version":1,"status":"error","base":"b","head":"h","comment":{"markdown":"x"}}`)

	if _, err := MergeIncremental(partial, good, fromSHA); err != nil {
		t.Fatalf("a good merge failed: %v", err)
	}
	for name, base := range map[string][]byte{
		"not json":      []byte(`nope`),
		"error result":  errorResult,
		"another head":  incResult(t, "elsewhere", skill),
		"empty":         nil,
		"wrong version": []byte(`{"schema_version":9}`),
	} {
		if _, err := MergeIncremental(partial, base, fromSHA); err == nil {
			t.Errorf("%s: merged", name)
		}
	}
	if _, err := MergeIncremental(errorResult, good, fromSHA); err == nil {
		t.Error("an error result was merged")
	}

	// Only a file the last review reviewed can be carried.
	carrying := func(skillName string, carried ...string) []byte {
		return incResult(t, "h", Skill{Name: skillName, Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{}, Carried: carried}})
	}
	for name, p := range map[string][]byte{
		"carries a file the base did not review":     carrying("sec", "never.go"),
		"carries a file with a skill the base lacks": carrying("new", "k.go"),
	} {
		if _, err := MergeIncremental(p, good, fromSHA); err == nil {
			t.Errorf("%s: merged", name)
		}
	}
	// The base listed the file as skipped, which is not reviewed.
	skipped := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues",
		Files: Files{Reviewed: []string{}, Skipped: []Skipped{{Path: "k.go", Rule: "lockfile"}}}})
	if _, err := MergeIncremental(carrying("sec", "k.go"), skipped, fromSHA); err == nil {
		t.Error("carries a skipped file: merged")
	}
	if _, err := MergeIncremental([]byte(`nope`), good, fromSHA); err == nil {
		t.Error("a malformed partial was merged")
	}
}

func TestIncrementalNote(t *testing.T) {
	for _, tc := range []struct {
		in   *Incremental
		want string
	}{
		{nil, ""},
		{&Incremental{Reviewed: 2, Carried: 28, From: "abc1234def"}, "Re-reviewed 2 files · 28 carried over from `abc1234`"},
		{&Incremental{Reviewed: 1, Carried: 1, From: "abc1234def"}, "Re-reviewed 1 file · 1 carried over from `abc1234`"},
	} {
		if got := IncrementalNote(tc.in); got != tc.want {
			t.Errorf("IncrementalNote(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSupportsCarried(t *testing.T) {
	for version, want := range map[string]bool{
		"0.5.0": true, "v0.5.0": true, "0.10.0": true, "1.0.0": true,
		"0.5.0-rc1": false, "v1.2.3-beta.1": false, "0.5.0+build": false, "0.4.9": false, "dev": false, "": false, "0.5": false, "0.5.x": false, "1.2.3.4": false, "0.-5.0": false,
	} {
		if got := (&Result{PergentVersion: version}).SupportsCarried(); got != want {
			t.Errorf("SupportsCarried(%q) = %v, want %v", version, got, want)
		}
	}
}

// A model that reads a carried file can report its finding again. The merge
// keeps one of them, or the result would grow with every push.
func TestMergeIncrementalSkipsRepeatedFinding(t *testing.T) {
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go"}},
		Findings: []json.RawMessage{
			incFinding("k.go", "aaaaaa000001", "suggestion"),
			incFinding("k.go", "bbbbbb000001", "suggestion"),
		}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues", Batches: 1,
		Files:    Files{Reviewed: []string{"n.go"}, Carried: []string{"k.go"}},
		Findings: []json.RawMessage{incFinding("k.go", "aaaaaa000001", "suggestion")}})

	fs := skillFindings(t, mergeIncremental(t, partial, base).Skills[0])
	if len(fs) != 2 || fs[0].Fingerprint != "aaaaaa000001" || fs[1].Fingerprint != "bbbbbb000001" {
		t.Errorf("findings = %+v, want the repeated one once, then the other carried one", fs)
	}
}

// A skill with no carried files reviewed all of its files, so the last
// review's checks are not kept.
func TestMergeIncrementalNoCarriedTakesNewChecksOnly(t *testing.T) {
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go"}},
		Checks: []BatchChecks{{Batch: 1, Checks: []Check{{Section: "A", Name: "x", Status: "pass"}}}}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues", Batches: 1, Files: Files{Reviewed: []string{"k.go"}},
		Checks: []BatchChecks{{Batch: 1, Checks: []Check{{Section: "B", Name: "y", Status: "pass"}}}}})

	got := mergeIncremental(t, partial, base).Skills[0].Checks
	if len(got) != 1 || len(got[0].Checks) != 1 || got[0].Checks[0].Name != "y" {
		t.Errorf("checks = %+v, want the new run's only", got)
	}
}

// A skill whose files all left the change started no run and carries nothing:
// the last review says nothing about files that are gone, so none of its
// checks, confidence or summaries are kept.
func TestMergeIncrementalNothingLeftTakesNothingOfTheBase(t *testing.T) {
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"gone.go"}},
		Confidence: 4, ConfidenceReason: "base reason", Notes: []string{"base note"},
		Summaries: []Summary{{Batch: 1, Text: "base summary"}}, Batches: 1, BatchesFinished: 1,
		Checks: []BatchChecks{{Batch: 1, Checks: []Check{{Section: "A", Name: "x", Status: "pass"}}}}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{}}})

	s := mergeIncremental(t, partial, base).Skills[0]
	if len(s.Checks) != 0 || s.Confidence != 0 || s.ConfidenceReason != "" || len(s.Summaries) != 0 || len(s.Notes) != 0 || s.Batches != 0 {
		t.Errorf("skill = %+v, want none of the last review's checks, confidence, summaries or notes", s)
	}
}

// Findings without a fingerprint are not told apart by it: one the new run
// reports must not hide the carried ones.
func TestMergeIncrementalEmptyFingerprintIsNotRepeated(t *testing.T) {
	bare := func(file string) json.RawMessage {
		b, _ := json.Marshal(Finding{File: file, Severity: "suggestion", Title: "t"})
		return b
	}
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go"}},
		Findings: []json.RawMessage{bare("k.go")}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues", Batches: 1,
		Files: Files{Reviewed: []string{"n.go"}, Carried: []string{"k.go"}}, Findings: []json.RawMessage{bare("n.go")}})

	if fs := skillFindings(t, mergeIncremental(t, partial, base).Skills[0]); len(fs) != 2 {
		t.Errorf("findings = %+v, want the new one and the carried one", fs)
	}
}

// A verdict that is neither clean one stays what the new run said, even with
// a blocking finding carried over.
func TestMergeIncrementalUnknownVerdictStays(t *testing.T) {
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "has_blocking_issues", Files: Files{Reviewed: []string{"k.go"}},
		Findings: []json.RawMessage{incFinding("k.go", "aaaaaa000001", "blocking")}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "mystery", Batches: 1, Files: Files{Reviewed: []string{"n.go"}, Carried: []string{"k.go"}}})

	if v := mergeIncremental(t, partial, base).Skills[0].Verdict; v != "mystery" {
		t.Errorf("verdict = %q, want mystery", v)
	}
}

// Fields of a skill that Result does not declare survive when the skill is in
// both results.
func TestMergeIncrementalKeepsUndeclaredFields(t *testing.T) {
	skill := Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"k.go"}}}
	base := incResult(t, fromSHA, skill)
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues", Batches: 1, Files: Files{Reviewed: []string{"n.go"}, Carried: []string{"k.go"}}})
	partial = []byte(strings.Replace(string(partial), `"name":"sec"`, `"name":"sec","tokens":{"input":7}`, 1))

	out, err := MergeIncremental(partial, base, fromSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"tokens":{"input":7}`) {
		t.Errorf("merged skill lost its tokens: %s", out)
	}
}
