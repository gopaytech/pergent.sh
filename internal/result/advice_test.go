package result

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// StepLimitAdvice is pergent's own text, in internal/runner/outcome.go of the
// pergent repository: if either changes, the other must.
func TestStepAdvicePhrase(t *testing.T) {
	if StepLimitAdvice != "(raise --max-turns to allow more steps)" {
		t.Errorf("StepLimitAdvice = %q, want pergent's phrase", StepLimitAdvice)
	}
}

// adviceResult is a completed result with advice in a failure message, its
// cause message, a note and a not-reviewed cause, and the phrase in a
// finding's explanation, where it is the model's text. Each place that gets
// edited also holds a field Result does not declare.
func adviceResult(advice string) []byte {
	return []byte(`{
  "schema_version": 1, "status": "completed", "pergent_version": "0.1.4",
  "base": "b", "head": "h", "tokens": {"input": 7},
  "skills": [{
    "name": "security", "verdict": "failed", "cost": 1,
    "findings": [{"id": "a1b2c3", "fingerprint": "a1b2c3d4e5f6", "file": "a.go", "severity": "suggestion",
      "title": "t", "explanation": "the log says ` + StepLimitAdvice + `"}],
    "failures": [{"files": ["a.go"], "attempts": 2,
      "message": "Reached the 40-step limit ` + advice + `",
      "cause": {"kind": "step_limit", "message": "Reached the 40-step limit ` + advice + `", "extra": true}}],
    "notes": ["unrelated note", "Verification failed: Reached the 40-step limit ` + advice + `."],
    "files": {"reviewed": [], "skipped": [{"path": "go.sum", "rule": "lockfile"}],
      "not_reviewed": [{"path": "a.go", "cause": "Reached the 40-step limit ` + advice + `", "bytes": 12}]}
  }],
  "comment": {"markdown": "### pergent", "findings_block": "<!-- pergent:findings [] -->"}
}`)
}

func TestReplaceStepAdvice(t *testing.T) {
	data := adviceResult(StepLimitAdvice)

	out, changed := ReplaceStepAdvice(data, AdviceDashboard)
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if _, err := Parse(out); err != nil {
		t.Fatalf("Parse of the output: %v", err)
	}
	// The four texts hold the advice and every other field is as it was,
	// the finding's explanation (the model's text) and the fields Result
	// does not declare included.
	if got, want := canonical(t, out), canonical(t, adviceResult(AdviceDashboard)); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}

	again, changed := ReplaceStepAdvice(out, AdviceDashboard)
	if changed || !bytes.Equal(again, out) {
		t.Errorf("second pass: changed = %v, want false and the same bytes", changed)
	}
}

func TestReplaceStepAdviceConfig(t *testing.T) {
	out, changed := ReplaceStepAdvice(adviceResult(StepLimitAdvice), AdviceConfig)
	if !changed || !strings.Contains(string(out), AdviceConfig) {
		t.Errorf("changed = %v, output lacks the config advice: %s", changed, out)
	}
}

func TestReplaceStepAdviceUnchanged(t *testing.T) {
	cases := map[string][]byte{
		"invalid json":  []byte(`{"skills": [`),
		"not an object": []byte(`[1, 2]`),
		"error result": []byte(`{"schema_version": 1, "status": "error", "error": {"code": "internal", "message": "x ` +
			StepLimitAdvice + `"}, "comment": {"markdown": "failed ` + StepLimitAdvice + `"}}`),
		"no phrase": []byte(strings.ReplaceAll(string(adviceResult(StepLimitAdvice)), StepLimitAdvice, "(nothing to see)")),
		"phrase only in a finding": []byte(`{"schema_version": 1, "status": "completed", "skills": [
			{"name": "s", "findings": [{"explanation": "` + StepLimitAdvice + `"}]}]}`),
		"skills not a list": []byte(`{"skills": {"notes": ["` + StepLimitAdvice + `"]}}`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			out, changed := ReplaceStepAdvice(data, AdviceDashboard)
			if changed || !bytes.Equal(out, data) {
				t.Errorf("changed = %v, output %s; want the input as it is", changed, out)
			}
		})
	}
}

// A skill with wrong-typed fields is left alone, the others are still edited.
func TestReplaceStepAdviceSkipsOddSkills(t *testing.T) {
	data := []byte(`{"skills": [{"notes": "` + StepLimitAdvice + `", "failures": 3}, {"notes": ["x ` + StepLimitAdvice + `"]}]}`)
	out, changed := ReplaceStepAdvice(data, AdviceDashboard)
	if !changed || !strings.Contains(string(out), `"notes":"`+StepLimitAdvice+`"`) || !strings.Contains(string(out), "x "+AdviceDashboard) {
		t.Errorf("changed = %v, output %s", changed, out)
	}
}

// canonical is data re-encoded through a generic value, so equal JSON
// compares equal whatever the layout of its fields.
func canonical(t *testing.T, data []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
