package policy

import (
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/result"
)

// updateNoCI rewrites testdata/no-ci-decisions.json. The file was written by
// this test run on the code before CI jobs existed, so it pins that a
// repository with no CI jobs is decided byte for byte as before.
var updateNoCI = flag.Bool("update-no-ci", false, "rewrite testdata/no-ci-decisions.json")

const noCIGolden = "testdata/no-ci-decisions.json"

// noCICase is one Outcome with no CI and the failure_status it is decided under.
type noCICase struct {
	o       Outcome
	failure config.FailureStatus
}

// noCICases builds only on Outcome fields older than CI jobs.
func noCICases() map[string]noCICase {
	sk := func(verdict string, reviewed ...string) result.Skill {
		return result.Skill{Name: "security", Verdict: verdict, Files: result.Files{Reviewed: reviewed}}
	}
	res := func(skills ...result.Skill) *result.Result {
		return &result.Result{SchemaVersion: 1, Status: "completed", Base: "b", Head: "h", Skills: skills}
	}
	blocking := sk("has_blocking_issues", "db/query.go")
	raw, _ := json.Marshal(result.Finding{
		ID: "abcdef", Fingerprint: "abcdef0123456789", File: "db/query.go", Line: 3, Severity: "blocking",
		Title: "Unchecked input", Explanation: "The id reaches the query unchecked.",
	})
	blocking.Findings = append(blocking.Findings, raw)
	errRes := &result.Result{SchemaVersion: 1, Status: "error", Error: &result.Error{Code: "x", Message: "boom"},
		Comment: result.Comment{Markdown: Marker + "\n⚠️ **pergent couldn't review this MR:** boom"}}
	return map[string]noCICase{
		"clean":                 {Outcome{Result: res(sk("no_blocking_issues", "a.go"))}, config.FailureStatusFailed},
		"blocking":              {Outcome{Result: res(blocking)}, config.FailureStatusFailed},
		"failed skill":          {Outcome{Result: res(sk("failed", "a.go"))}, config.FailureStatusFailed},
		"failed skill, neutral": {Outcome{Result: res(sk("failed", "a.go"))}, config.FailureStatusNeutral},
		"draft":                 {Outcome{Result: res(sk("no_blocking_issues", "a.go")), Draft: true}, config.FailureStatusFailed},
		"approval off":          {Outcome{Result: res(sk("no_blocking_issues", "a.go")), ApprovalOff: true}, config.FailureStatusFailed},
		"failure reason":        {Outcome{FailureReason: "the review Job ended without a result"}, config.FailureStatusFailed},
		"failure, neutral":      {Outcome{FailureReason: "the review Job ended without a result"}, config.FailureStatusNeutral},
		"no result":             {Outcome{}, config.FailureStatusFailed},
		"error result":          {Outcome{Result: errRes}, config.FailureStatusFailed},
		"nothing reviewed": {Outcome{Result: res(result.Skill{Name: "security", Verdict: "no_blocking_issues",
			Files: result.Files{Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}})}, config.FailureStatusFailed},
	}
}

// TestDecideNoCIByteIdentical decides every noCICases case and compares the
// whole Decision with the one the code before CI jobs gave.
func TestDecideNoCIByteIdentical(t *testing.T) {
	got := map[string]Decision{}
	for name, c := range noCICases() {
		got[name] = Decide(c.o, c.failure, config.BlockingStatusFailed)
	}
	if *updateNoCI {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(noCIGolden, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(noCIGolden)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]Decision
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("golden has %d cases, the test %d", len(want), len(got))
	}
	for name, w := range want {
		if g := got[name]; g != w {
			t.Errorf("%s:\n got  %+v\n want %+v", name, g, w)
		}
	}
}
