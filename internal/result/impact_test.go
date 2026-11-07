package result

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// impactFinding is a finding of the impact skill, in the shape pergent writes
// it: the changed code is File and Line, the caller is Linked.
func impactFinding(fp, severity string) json.RawMessage {
	b, _ := json.Marshal(Finding{
		ID: fp[:6], Fingerprint: fp, File: "render/comment.go", Line: 12, Severity: severity, Title: "t " + fp,
		Linked: &Linked{Repo: "acme/api", File: "client.go", Line: 40},
	})
	return b
}

func impactOf(t *testing.T, r *Result) Skill {
	t.Helper()
	for _, s := range r.Skills {
		if s.Name == ImpactSkill {
			return s
		}
	}
	t.Fatalf("no impact skill in %+v", r.Skills)
	return Skill{}
}

func TestFindingLinkedDecodes(t *testing.T) {
	data := incResult(t, "h", Skill{Name: ImpactSkill, Verdict: "has_blocking_issues",
		Findings: []json.RawMessage{impactFinding("aaaaaa000001", "blocking")}})
	r, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	fs := skillFindings(t, r.Skills[0])
	if len(fs) != 1 || fs[0].Linked == nil || *fs[0].Linked != (Linked{Repo: "acme/api", File: "client.go", Line: 40}) {
		t.Errorf("findings = %+v; want the caller decoded", fs)
	}

	// Another finding carries none, and the field is left out of its JSON.
	plain, _ := json.Marshal(Finding{Fingerprint: "bbbbbb000001", File: "a.go", Severity: "suggestion"})
	if strings.Contains(string(plain), "linked") {
		t.Errorf("a finding with no caller is written as %s", plain)
	}
}

// The linked caller is not part of the comment: the explanation carries it.
func TestLinkedIsNotDrawn(t *testing.T) {
	data := incResult(t, "h", Skill{Name: ImpactSkill, Verdict: "has_blocking_issues",
		Findings: []json.RawMessage{impactFinding("aaaaaa000001", "blocking")}})
	r, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ToRender(r) {
		for _, f := range s.Findings {
			if strings.Contains(f.Title+f.Explanation+f.File, "client.go") {
				t.Errorf("rendered finding %+v shows the linked caller", f)
			}
		}
	}
}

func TestMergeKeepsOpenImpact(t *testing.T) {
	regular := func(file string) Skill {
		return Skill{Name: "sec", Verdict: "no_blocking_issues", Batches: 1, Files: Files{Reviewed: []string{file}}}
	}
	for _, tc := range []struct {
		name    string
		earlier Skill
		keep    bool
	}{
		{"a blocking finding", Skill{Name: ImpactSkill, Verdict: "has_blocking_issues",
			Findings: []json.RawMessage{impactFinding("aaaaaa000001", "blocking")}}, true},
		{"a failed check", Skill{Name: ImpactSkill, Verdict: "failed",
			Failures: []Failure{{Message: "Not checked: it did not finish"}}}, true},
		{"a verdict this code does not know", Skill{Name: ImpactSkill, Verdict: "maybe"}, true},
		{"a blocking finding under a clean verdict", Skill{Name: ImpactSkill, Verdict: "no_blocking_issues",
			Findings: []json.RawMessage{impactFinding("aaaaaa000001", "blocking")}}, true},
		{"a clean check", Skill{Name: ImpactSkill, Verdict: "no_blocking_issues",
			Notes: []string{"Impact: checked acme/api, nothing found"}}, false},
		{"suggestions only", Skill{Name: ImpactSkill, Verdict: "no_blocking_issues",
			Findings: []json.RawMessage{impactFinding("aaaaaa000001", "suggestion")}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := incResult(t, fromSHA, regular("a.go"), tc.earlier)
			partial := incResult(t, "h", regular("b.go"))

			r := mergeIncremental(t, partial, base)
			if !tc.keep {
				if len(r.Skills) != 1 || r.Skills[0].Name != "sec" {
					t.Fatalf("skills = %+v; want the new run's alone", r.Skills)
				}
				return
			}
			if len(r.Skills) != 2 || r.Skills[0].Name != "sec" || r.Skills[1].Name != ImpactSkill {
				t.Fatalf("skills = %+v; want the new run's, then the earlier impact", r.Skills)
			}
			got := impactOf(t, r)
			if got.Verdict != tc.earlier.Verdict || len(got.Findings) != len(tc.earlier.Findings) || len(got.Failures) != len(tc.earlier.Failures) {
				t.Errorf("impact = %+v; want the earlier one as it was", got)
			}
		})
	}
}

// The new run's own impact wins; the earlier one is not added next to it.
func TestMergeTakesTheNewImpact(t *testing.T) {
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues"},
		Skill{Name: ImpactSkill, Verdict: "failed", Failures: []Failure{{Message: "old"}}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues"},
		Skill{Name: ImpactSkill, Verdict: "no_blocking_issues", Notes: []string{"Impact: checked acme/api, nothing found"}})

	r := mergeIncremental(t, partial, base)
	if len(r.Skills) != 2 {
		t.Fatalf("skills = %+v", r.Skills)
	}
	if got := impactOf(t, r); got.Verdict != "no_blocking_issues" || len(got.Failures) != 0 {
		t.Errorf("impact = %+v; want the new run's clean one", got)
	}
}

// Nothing is kept when the earlier review had no impact skill at all.
func TestMergeWithoutAnEarlierImpact(t *testing.T) {
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues"})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "no_blocking_issues"})
	if r := mergeIncremental(t, partial, base); len(r.Skills) != 1 {
		t.Errorf("skills = %+v; want one", r.Skills)
	}
}

func TestMergeKeepsAnEarlierImpactWithItsIDs(t *testing.T) {
	// Two findings in different skills share a short ID prefix: the kept one
	// takes part in the ID assignment like any other.
	base := incResult(t, fromSHA, Skill{Name: "sec", Verdict: "no_blocking_issues"},
		Skill{Name: ImpactSkill, Verdict: "has_blocking_issues",
			Findings: []json.RawMessage{impactFinding("abcdef111111", "blocking")}})
	partial := incResult(t, "h", Skill{Name: "sec", Verdict: "has_blocking_issues", Batches: 1,
		Findings: []json.RawMessage{incFinding("a.go", "abcdef222222", "blocking")}})

	r := mergeIncremental(t, partial, base)
	a, b := skillFindings(t, r.Skills[0])[0], skillFindings(t, impactOf(t, r))[0]
	if a.ID == b.ID || len(a.ID) != 8 || len(b.ID) != 8 {
		t.Errorf("ids = %q and %q; want both widened and different", a.ID, b.ID)
	}
}

func completedWith(t *testing.T, skills ...Skill) []byte {
	t.Helper()
	return incResult(t, "h", skills...)
}

func TestFailImpact(t *testing.T) {
	data := completedWith(t,
		Skill{Name: "sec", Verdict: "has_blocking_issues", Files: Files{Reviewed: []string{"a.go"}},
			Findings: []json.RawMessage{incFinding("a.go", "bbbbbb000001", "blocking")}},
		Skill{Name: ImpactSkill, Verdict: "has_blocking_issues", Confidence: 4, ConfidenceReason: "saw the caller",
			Findings:  []json.RawMessage{impactFinding("aaaaaa000001", "blocking")},
			Notes:     []string{"Impact: checked acme/secret, nothing found"},
			Summaries: []Summary{{Batch: 1, Text: "found acme/secret/client.go"}},
			Checks:    []BatchChecks{{Batch: 1, Checks: []Check{{Name: "acme/secret"}}}}})

	out, changed, err := FailImpact(data, ImpactWithheldMessage)
	if err != nil || !changed {
		t.Fatalf("FailImpact = %v, %v; want a change", changed, err)
	}
	r, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	got := impactOf(t, r)
	if got.Verdict != "failed" || len(got.Findings) != 0 || len(got.Notes) != 0 || len(got.Summaries) != 0 ||
		len(got.Checks) != 0 || got.Confidence != 0 || got.ConfidenceReason != "" {
		t.Errorf("impact = %+v; want a failed skill with nothing of the check", got)
	}
	if len(got.Failures) != 1 || got.Failures[0].Message != ImpactWithheldMessage {
		t.Errorf("failures = %+v; want the fixed message alone", got.Failures)
	}
	if strings.Contains(string(out), "acme/secret") || strings.Contains(string(out), "client.go") {
		t.Errorf("the result still carries what the check found: %s", out)
	}
	// The other skills are as they were.
	if sec := r.Skills[0]; sec.Name != "sec" || sec.Verdict != "has_blocking_issues" || len(sec.Findings) != 1 || sec.Files.Reviewed[0] != "a.go" {
		t.Errorf("the other skill changed: %+v", sec)
	}
	// Done twice, it changes nothing more.
	again, changed, err := FailImpact(out, ImpactWithheldMessage)
	if err != nil || changed {
		t.Errorf("second FailImpact = %v, %v; want no change", changed, err)
	}
	if string(again) != string(out) {
		t.Errorf("second FailImpact rewrote the result")
	}
}

func TestFailImpactWithoutImpactChangesNothing(t *testing.T) {
	data := completedWith(t, Skill{Name: "sec", Verdict: "no_blocking_issues"})
	out, changed, err := FailImpact(data, ImpactWithheldMessage)
	if err != nil || changed || string(out) != string(data) {
		t.Errorf("FailImpact = %q, %v, %v; want the result untouched", out, changed, err)
	}
}

// Fields pergent.sh does not declare survive, as in every other edit.
func TestFailImpactKeepsUndeclaredFields(t *testing.T) {
	data := []byte(`{"schema_version":1,"status":"completed","base":"b","head":"h","future":{"x":1},
		"skills":[{"name":"impact","verdict":"has_blocking_issues","findings":[],"files":{},"later":7}],
		"comment":{"markdown":"m"}}`)
	out, changed, err := FailImpact(data, ImpactWithheldMessage)
	if err != nil || !changed {
		t.Fatalf("FailImpact = %v, %v", changed, err)
	}
	if !strings.Contains(string(out), `"future"`) {
		t.Errorf("the envelope lost a field: %s", out)
	}
}

func TestAddFailedImpact(t *testing.T) {
	with := completedWith(t, Skill{Name: "sec", Verdict: "no_blocking_issues"},
		Skill{Name: ImpactSkill, Verdict: "no_blocking_issues"})
	without := completedWith(t, Skill{Name: "sec", Verdict: "no_blocking_issues", Files: Files{Reviewed: []string{"a.go"}}})

	out, added, err := AddFailedImpact(without, ImpactNotRunMessage)
	if err != nil || !added {
		t.Fatalf("AddFailedImpact = %v, %v; want it added", added, err)
	}
	r, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Skills) != 2 || r.Skills[0].Name != "sec" {
		t.Fatalf("skills = %+v; want the review's, then impact", r.Skills)
	}
	got := impactOf(t, r)
	if got.Verdict != "failed" || len(got.Findings) != 0 || len(got.Files.Reviewed) != 0 ||
		len(got.Failures) != 1 || got.Failures[0].Message != ImpactNotRunMessage {
		t.Errorf("impact = %+v", got)
	}
	// A failed skill lists no file: it would be counted among the reviewed.
	if names, whole := FailedSkills(r, nil); whole || !slices.Equal(names, []string{ImpactSkill}) {
		t.Errorf("FailedSkills = %v, %v; want impact", names, whole)
	}

	if out, added, err := AddFailedImpact(with, ImpactNotRunMessage); err != nil || added || string(out) != string(with) {
		t.Errorf("AddFailedImpact on a result with impact = %v, %v", added, err)
	}
	errResult := []byte(`{"schema_version":1,"status":"error","comment":{"markdown":"m"},"error":{"code":"x","message":"y"}}`)
	if out, added, err := AddFailedImpact(errResult, ImpactNotRunMessage); err != nil || added || string(out) != string(errResult) {
		t.Errorf("AddFailedImpact on an error result = %v, %v", added, err)
	}
}

func TestHoldsImpact(t *testing.T) {
	if (&Result{Skills: []Skill{{Name: "sec"}}}).HoldsImpact() {
		t.Error("a result without impact holds it")
	}
	if !(&Result{Skills: []Skill{{Name: "sec"}, {Name: ImpactSkill}}}).HoldsImpact() {
		t.Error("a result with impact does not hold it")
	}
}

// pergent's own comment and findings block may quote what was withheld: the
// stored result keeps neither, only text that Parse accepts.
func TestFailImpactDropsPergentsComment(t *testing.T) {
	data := completedWith(t, Skill{Name: ImpactSkill, Verdict: "has_blocking_issues",
		Findings: []json.RawMessage{impactFinding("aaaaaa000001", "blocking")}})
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	top["comment"] = map[string]any{"markdown": "found a caller in acme/secret/client.go", "findings_block": "<!-- pergent:findings acme/secret -->"}
	data, _ = json.Marshal(top)

	out, changed, err := FailImpact(data, ImpactWithheldMessage)
	if err != nil || !changed {
		t.Fatalf("FailImpact = %v, %v", changed, err)
	}
	if strings.Contains(string(out), "acme/secret") {
		t.Errorf("the stored result still carries the withheld text: %s", out)
	}
	r, err := Parse(out)
	if err != nil || r.Comment.Markdown != ImpactWithheldComment || r.Comment.FindingsBlock != "" {
		t.Errorf("comment = %+v, %v; want the fixed text and no block", r.Comment, err)
	}
	// Nothing changed, nothing rewritten: the comment stays.
	clean := completedWith(t, Skill{Name: "sec", Verdict: "no_blocking_issues"})
	if again, changed, _ := FailImpact(clean, ImpactWithheldMessage); changed || string(again) != string(clean) {
		t.Error("a result with no impact skill was rewritten")
	}
}

func TestIsImpactCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		skill   string
		sources map[string]string
		want    bool
	}{
		"dispatched without it":  {"impact", map[string]string{"sec": "library"}, true},
		"dispatched with it":     {"impact", map[string]string{"sec": "library", "impact": "repo"}, false},
		"nothing recorded":       {"impact", nil, true},
		"another skill":          {"sec", map[string]string{"sec": "library"}, false},
		"another skill, nothing": {"sec", nil, false},
	} {
		if got := IsImpactCheck(tc.skill, tc.sources); got != tc.want {
			t.Errorf("%s: IsImpactCheck = %v; want %v", name, got, tc.want)
		}
	}
	r := &Result{Skills: []Skill{{Name: ImpactSkill}}}
	if !r.IsCheck(ImpactSkill) || r.ForRun(map[string]string{"impact": "library"}).IsCheck(ImpactSkill) {
		t.Error("ForRun did not mark an own skill")
	}
}
