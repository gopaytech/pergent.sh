package policy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/result"
)

const findingsBlock = "<!-- pergent:findings eyJ9 -->"

func skill(name, verdict string, reviewed ...string) result.Skill {
	return result.Skill{Name: name, Verdict: verdict, Files: result.Files{Reviewed: reviewed}}
}

// jobComment is the comment the fixture result carries from the Job. The
// comment of a completed review is rendered from the result, so it must never
// show up in a decision.
const jobComment = "JOB-COMMENT-MUST-NOT-BE-USED"

func completed(skills ...result.Skill) *result.Result {
	return &result.Result{
		SchemaVersion: 1, Status: "completed", Skills: skills,
		Comment: result.Comment{Markdown: Marker + "\n### review\n\n" + jobComment + "\n\n" + findingsBlock, FindingsBlock: findingsBlock},
	}
}

// blockOf is the hidden findings block of r as render writes it, with its
// newline.
func blockOf(r *result.Result) string { return render.FindingsBlock(result.ToRender(r)) }

// checkNote asserts the shape of a comment of r that carries note: the marker
// is first and appears once, the note appears once, right before the hidden
// findings block, and the block is the last line.
func checkNote(t *testing.T, comment string, r *result.Result, note string) {
	t.Helper()
	if !strings.HasPrefix(comment, Marker+"\n") || strings.Count(comment, Marker) != 1 {
		t.Errorf("the marker must be first, once: %q", comment)
	}
	if strings.Contains(comment, jobComment) {
		t.Errorf("the Job's comment was used: %q", comment)
	}
	if got := strings.Count(comment, note); got != 1 {
		t.Errorf("note appears %d times, want once:\n%s", got, comment)
	}
	block := strings.TrimRight(blockOf(r), "\n")
	if block == "" || strings.Count(comment, "<!-- pergent:findings ") != 1 {
		t.Fatalf("want exactly one findings block:\n%s", comment)
	}
	if !strings.Contains(comment, note+"\n\n"+block) {
		t.Errorf("the note must sit right before the findings block:\n%s", comment)
	}
	if !strings.HasSuffix(strings.TrimRight(comment, "\n"), block) {
		t.Errorf("the findings block must be the last line:\n%s", comment)
	}
}

// noteOf is the text of a comment from the start of its "Not approved" note
// to the findings block (or its end).
func noteOf(t *testing.T, comment string) string {
	t.Helper()
	i := strings.Index(comment, "---\n**Not approved:**")
	if i < 0 {
		t.Fatalf("no note in %q", comment)
	}
	note := comment[i:]
	if j := strings.Index(note, "<!-- pergent:findings "); j >= 0 {
		note = note[:j]
	}
	return strings.TrimRight(note, "\n")
}

// decodeBlock is the JSON the last findings block of comment carries.
func decodeBlock(t *testing.T, comment string) string {
	t.Helper()
	const start = "<!-- pergent:findings "
	i := strings.LastIndex(comment, start)
	if i < 0 {
		t.Fatalf("no findings block in %q", comment)
	}
	enc := strings.TrimSuffix(strings.TrimSpace(comment[i+len(start):]), " -->")
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDecide(t *testing.T) {
	errResult := &result.Result{
		SchemaVersion: 1, Status: "error",
		Error:   &result.Error{Code: "commit_missing", Message: "boom"},
		Comment: result.Comment{Markdown: Marker + "\n⚠️ **pergent couldn't review this MR:** boom"},
	}
	cases := []struct {
		name        string
		outcome     Outcome
		failure     config.FailureStatus
		wantStatus  Status
		wantDesc    string
		wantApprove bool
		wantComment string // exact; "" skips
		wantPrefix  string // "" skips
	}{
		{
			name:       "no result with a reason",
			outcome:    Outcome{FailureReason: "the job timed out"},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusFailed, wantDesc: "pergent couldn't review this MR",
			wantComment: Marker + "\n⚠️ **pergent couldn't review this MR:** the job timed out.",
		},
		{
			name:       "failure reason wins over a clean result",
			outcome:    Outcome{FailureReason: "x", Result: completed(skill("a", "no_blocking_issues", "f.go"))},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusFailed, wantDesc: "pergent couldn't review this MR",
		},
		{
			name:       "error result",
			outcome:    Outcome{Result: errResult},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusFailed, wantDesc: "pergent couldn't review this MR",
			wantComment: errResult.Comment.Markdown,
		},
		{
			name:       "nil result and no reason never passes",
			outcome:    Outcome{},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusFailed, wantDesc: "pergent couldn't review this MR",
		},
		{
			name:       "blocking issues",
			outcome:    Outcome{Result: completed(skill("a", "has_blocking_issues", "f.go"), skill("b", "no_blocking_issues", "f.go"))},
			failure:    config.FailureStatusNeutral,
			wantStatus: StatusFailed, wantDesc: "Blocking issues found",
		},
		{
			name:       "mixed blocking and failed",
			outcome:    Outcome{Result: completed(skill("a", "failed"), skill("b", "has_blocking_issues", "f.go"))},
			failure:    config.FailureStatusNeutral,
			wantStatus: StatusFailed, wantDesc: "Blocking issues found",
		},
		{
			name:       "one failed skill of two",
			outcome:    Outcome{Result: completed(skill("a", "failed"), skill("b", "no_blocking_issues", "f.go"))},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusFailed, wantDesc: "Review failed for 1 of 2 skills",
		},
		{
			name:       "unknown verdict is a failure",
			outcome:    Outcome{Result: completed(skill("a", "weird", "f.go"))},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusFailed, wantDesc: "Review failed for 1 of 1 skills",
		},
		{
			name:       "empty verdict is a failure",
			outcome:    Outcome{Result: completed(skill("a", "", "f.go"))},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusFailed, wantDesc: "Review failed for 1 of 1 skills",
		},
		{
			name:       "neutral setting on a failed skill",
			outcome:    Outcome{Result: completed(skill("a", "failed"))},
			failure:    config.FailureStatusNeutral,
			wantStatus: StatusNeutral, wantDesc: "Review failed for 1 of 1 skills",
		},
		{
			name:       "neutral setting on an error result",
			outcome:    Outcome{Result: errResult},
			failure:    config.FailureStatusNeutral,
			wantStatus: StatusNeutral, wantDesc: "pergent couldn't review this MR",
		},
		{
			name:       "neutral setting on no result",
			outcome:    Outcome{FailureReason: "gone"},
			failure:    config.FailureStatusNeutral,
			wantStatus: StatusNeutral, wantDesc: "pergent couldn't review this MR",
		},
		{
			name:       "unset failure setting means failed",
			outcome:    Outcome{FailureReason: "gone"},
			failure:    "",
			wantStatus: StatusFailed, wantDesc: "pergent couldn't review this MR",
		},
		{
			name:       "clean review approves",
			outcome:    Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go"), skill("b", "no_blocking_issues"))},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusSuccess, wantDesc: "No blocking issues", wantApprove: true,
			wantPrefix: Marker + "\n### ✅ No blocking issues",
		},
		{
			name: "vendored skip next to a reviewed file is not approved",
			outcome: Outcome{Result: completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
				Reviewed: []string{"f.go"}, Skipped: []result.Skipped{{Path: "vendor/x.go", Rule: "vendored"}}}})},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusSuccess, wantDesc: "No blocking issues; not approved: some changed files weren't reviewed",
		},
		{
			name: "only out-of-scope and excluded skips still approve",
			outcome: Outcome{Result: completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
				Reviewed: []string{"f.go"}, Skipped: []result.Skipped{{Path: "docs/x.md", Rule: "out of scope"}, {Path: "y.go", Rule: "excluded by skill"}, {Path: "z", Rule: "no content changes"}}}})},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusSuccess, wantDesc: "No blocking issues", wantApprove: true,
		},
		{
			name: "blocking wins over a default skip",
			outcome: Outcome{Result: completed(result.Skill{Name: "a", Verdict: "has_blocking_issues", Files: result.Files{
				Reviewed: []string{"f.go"}, Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}})},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusFailed, wantDesc: "Blocking issues found",
		},
		{
			name:       "completed result without skills is not a pass",
			outcome:    Outcome{Result: completed()},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusFailed, wantDesc: "pergent couldn't review this MR",
		},
		{
			name:       "unknown result status is not a pass",
			outcome:    Outcome{Result: &result.Result{Status: "weird", Comment: result.Comment{Markdown: "m"}, Skills: []result.Skill{skill("a", "no_blocking_issues", "f.go")}}},
			failure:    config.FailureStatusFailed,
			wantStatus: StatusFailed, wantDesc: "pergent couldn't review this MR",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.outcome, tc.failure)
			if d.Status != tc.wantStatus || d.Description != tc.wantDesc || d.Approve != tc.wantApprove {
				t.Errorf("got status=%q desc=%q approve=%v, want %q %q %v", d.Status, d.Description, d.Approve, tc.wantStatus, tc.wantDesc, tc.wantApprove)
			}
			if !strings.Contains(d.Comment, Marker) {
				t.Errorf("comment lacks the marker: %q", d.Comment)
			}
			if tc.wantComment != "" && d.Comment != tc.wantComment {
				t.Errorf("comment = %q, want %q", d.Comment, tc.wantComment)
			}
			if tc.wantPrefix != "" && !strings.HasPrefix(d.Comment, tc.wantPrefix) {
				t.Errorf("comment = %q, want it to start with %q", d.Comment, tc.wantPrefix)
			}
			if tc.outcome.Result != nil && tc.outcome.Result.Status == "completed" && strings.Contains(d.Comment, jobComment) {
				t.Errorf("the Job's comment was used: %q", d.Comment)
			}
			if d.Approve && d.Status != StatusSuccess {
				t.Error("approved without success")
			}
		})
	}
}

func TestDecideNothingReviewed(t *testing.T) {
	r := completed(
		result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}, {Path: "vendor/x.go", Rule: "vendored"}}}},
		result.Skill{Name: "b", Verdict: "no_blocking_issues", Files: result.Files{Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}},
	)
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Status != StatusSuccess || d.Approve || d.Description != "Nothing to review: every changed file is skipped" {
		t.Fatalf("got %+v", d)
	}
	want := "---\n**Not approved:** every changed file was skipped by pergent's default rules, so nothing was reviewed. A human needs to look.\n\n" +
		"- `go.sum` (lockfile)\n- `vendor/x.go` (vendored)"
	if got := noteOf(t, d.Comment); got != want {
		t.Errorf("note:\n%q\nwant:\n%q", got, want)
	}
	checkNote(t, d.Comment, r, want)
}

func TestDecideNothingReviewedNoteAppendedWithoutBlock(t *testing.T) {
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}})
	d := Decide(withoutBlock(t, Outcome{Result: r}), config.FailureStatusFailed)
	want := "---\n**Not approved:** every changed file was skipped by pergent's default rules, so nothing was reviewed. A human needs to look.\n\n- `go.sum` (lockfile)"
	if !strings.HasSuffix(d.Comment, "\n\n"+want+"\n") {
		t.Errorf("the note must be appended at the end:\n%s", d.Comment)
	}
	if strings.Contains(d.Comment, "<!-- pergent:findings ") {
		t.Errorf("the block should have been dropped to fit the limit:\n%s", d.Comment)
	}
	if d.Approve {
		t.Error("approved")
	}
}

// withoutBlock is o with a size limit just under the full comment, so the
// renderer drops the hidden findings block to fit: the first step of its
// shrinking.
func withoutBlock(t *testing.T, o Outcome) Outcome {
	t.Helper()
	full := render.Comment(result.ToRender(o.Result), render.Options{ChangedFiles: result.ChangedFiles(o.Result), Commit: o.Result.Head})
	o.MaxComment = len(full) - 1
	if got := render.Comment(result.ToRender(o.Result), render.Options{MaxLen: o.MaxComment, ChangedFiles: result.ChangedFiles(o.Result), Commit: o.Result.Head}); strings.Contains(got, "<!-- pergent:findings ") {
		t.Fatalf("a limit of %d bytes should drop the findings block:\n%s", o.MaxComment, got)
	}
	return o
}

func TestDecideNothingReviewedCapsAtTwenty(t *testing.T) {
	var skipped []result.Skipped
	for i := 0; i < 25; i++ {
		skipped = append(skipped, result.Skipped{Path: fmt.Sprintf("lock/%02d.lock", i), Rule: "lockfile"})
	}
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: skipped}})
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	// The note is what is capped; the comment's own Files list is render's.
	note := noteOf(t, d.Comment)
	if got := strings.Count(note, "(lockfile)"); got != 20 {
		t.Errorf("listed %d, want 20", got)
	}
	if !strings.Contains(note, "- …and 5 more") {
		t.Errorf("missing overflow line: %q", note)
	}
	if !strings.Contains(note, "lock/19.lock") || strings.Contains(note, "lock/20.lock") {
		t.Error("expected the first 20 sorted paths")
	}
	checkNote(t, d.Comment, r, note)
}

func TestDecideNothingReviewedNoSkippedAtAll(t *testing.T) {
	// A completed review with nothing reviewed and nothing skipped (an empty diff)
	// is still not approved.
	d := Decide(Outcome{Result: completed(skill("a", "no_blocking_issues"))}, config.FailureStatusFailed)
	if d.Approve || d.Status != StatusSuccess {
		t.Fatalf("got %+v", d)
	}
}

func TestDecideAnyReviewedFileMeansApprove(t *testing.T) {
	// One skill reviewed a file, the other reviewed none: still reviewed.
	r := completed(skill("a", "no_blocking_issues"), skill("b", "no_blocking_issues", "f.go"))
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if !d.Approve || d.Description != "No blocking issues" {
		t.Fatalf("got %+v", d)
	}
}

func TestMarkerPrepended(t *testing.T) {
	// A completed review is rendered from the result: the marker comes first
	// and once, whatever the Job's comment held.
	r := completed(skill("a", "has_blocking_issues", "f.go"))
	for _, job := range []string{"### no marker here", "x\n" + Marker + "\ny", ""} {
		r.Comment.Markdown = job
		d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
		if !strings.HasPrefix(d.Comment, Marker+"\n") || strings.Count(d.Comment, Marker) != 1 {
			t.Errorf("job comment %q: comment %q", job, d.Comment)
		}
	}

	// An error result keeps the Job's comment: the marker is added when it
	// lacks one and the comment is left alone when it has one.
	e := &result.Result{Status: "error", Comment: result.Comment{Markdown: "### no marker here"}}
	if d := Decide(Outcome{Result: e}, config.FailureStatusFailed); d.Comment != Marker+"\n### no marker here" {
		t.Errorf("comment: %q", d.Comment)
	}
	e.Comment.Markdown = "x\n" + Marker + "\ny"
	if d := Decide(Outcome{Result: e}, config.FailureStatusFailed); d.Comment != e.Comment.Markdown {
		t.Errorf("comment: %q", d.Comment)
	}
}

func TestErrorComment(t *testing.T) {
	want := Marker + "\n⚠️ **pergent couldn't review this MR:** reason."
	if got := ErrorComment("reason"); got != want {
		t.Errorf("got %q", got)
	}
}

func TestNoteGoesBeforeRealFindingsBlockShape(t *testing.T) {
	// The findings block render writes ends with a newline, and so does the
	// comment.
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}})
	block := blockOf(r)
	if !strings.HasSuffix(block, "\n") {
		t.Fatalf("block = %q", block)
	}
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Approve {
		t.Fatal("approved")
	}
	if !strings.HasSuffix(d.Comment, "\n\n"+block) {
		t.Fatalf("block must be last: %q", d.Comment)
	}
	if strings.Index(d.Comment, "**Not approved:**") > strings.Index(d.Comment, "<!-- pergent:findings") {
		t.Errorf("note must precede the block: %q", d.Comment)
	}
	if strings.Count(d.Comment, "<!-- pergent:findings") != 1 {
		t.Errorf("block duplicated: %q", d.Comment)
	}
}

func TestCleanVerdictWithNotReviewedFilesFailsClosed(t *testing.T) {
	s := skill("a", "no_blocking_issues", "f.go")
	s.Files.NotReviewed = []json.RawMessage{json.RawMessage(`{"path":"g.go","cause":"timeout"}`)}
	for _, tc := range []struct {
		f    config.FailureStatus
		want Status
	}{{config.FailureStatusFailed, StatusFailed}, {config.FailureStatusNeutral, StatusNeutral}} {
		d := Decide(Outcome{Result: completed(s, skill("b", "no_blocking_issues", "f.go"))}, tc.f)
		if d.Approve || d.Status != tc.want || d.Description != "Review failed for 1 of 2 skills" {
			t.Errorf("%s: got %+v", tc.f, d)
		}
	}
}

func TestNothingReviewedListsDistinctPathsWithJoinedRules(t *testing.T) {
	r := completed(
		result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}, {Path: "a.go", Rule: "generated"}}}},
		result.Skill{Name: "b", Verdict: "no_blocking_issues", Files: result.Files{Skipped: []result.Skipped{{Path: "go.sum", Rule: "out of scope"}, {Path: "go.sum", Rule: "lockfile"}}}},
	)
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	note := noteOf(t, d.Comment)
	if strings.Count(note, "`go.sum`") != 1 || !strings.Contains(note, "- `go.sum` (lockfile, out of scope)") {
		t.Errorf("note: %q", note)
	}
	if strings.Index(note, "`a.go`") > strings.Index(note, "`go.sum`") {
		t.Errorf("not sorted: %q", note)
	}
}

func TestNothingReviewedCapCountsPaths(t *testing.T) {
	// 22 paths, each skipped by two skills: the cap counts paths, not pairs.
	var a, b []result.Skipped
	for i := 0; i < 22; i++ {
		p := fmt.Sprintf("p/%02d", i)
		a = append(a, result.Skipped{Path: p, Rule: "lockfile"})
		b = append(b, result.Skipped{Path: p, Rule: "vendored"})
	}
	r := completed(
		result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: a}},
		result.Skill{Name: "b", Verdict: "no_blocking_issues", Files: result.Files{Skipped: b}},
	)
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	note := noteOf(t, d.Comment)
	if got := strings.Count(note, "(lockfile, vendored)"); got != 20 {
		t.Errorf("listed %d, want 20", got)
	}
	if !strings.Contains(note, "- …and 2 more") {
		t.Errorf("note: %q", note)
	}
}

const zwsp = "​"

func TestNeutralizeQuickActions(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"line of its own", "x\n/approve\ny", "x\n" + zwsp + "/approve\ny"},
		{"first line", "/unapprove", zwsp + "/unapprove"},
		// GitLab's quick action extractor deletes every "\r" before it
		// matches a "/" at the start of a line, so a carriage return in front
		// of the slash doesn't hide it.
		{"carriage return before the slash", "x\n\r/approve", "x\n" + zwsp + "/approve"},
		{"carriage returns on the first line", "\r\r/close", zwsp + "/close"},
		{"CRLF", "x\r\n/approve", "x\n" + zwsp + "/approve"},
		{"CRLF throughout", "a\r\n/label ~x\r\n", "a\n" + zwsp + "/label ~x\n"},
		{"column 0 inside a fence", "```\n/merge\n```", "```\n" + zwsp + "/merge\n```"},
		// GitLab runs only a "/" at column 0: indented lines are left as they
		// are, so code in a suggested fix stays byte-identical.
		{"indented code kept", "```go\n    // keep\n```", "```go\n    // keep\n```"},
		{"indented slash kept", "  /merge", "  /merge"},
		{"tab indented slash kept", "\t/close", "\t/close"},
		{"slash mid-line", "use a/b and /approve", "use a/b and /approve"},
		{"marker and block untouched", Marker + "\nbody\n" + findingsBlock + "\n", Marker + "\nbody\n" + findingsBlock + "\n"},
		{"no slash", "a\r\nb", "a\nb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeutralizeQuickActions(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Every comment Decide returns has its quick actions neutralised: model text
// an MR can steer must never make the review bot run /approve, /close...
func TestDecideNeutralizesQuickActions(t *testing.T) {
	// The hostile text sits in the fields the comment shows: a finding's
	// explanation, fix, title and suggestion, a check's detail, a note, a
	// summary and a failure.
	steered := "explanation\n/approve\n\r/merge\n```\n/close\n    // keep\n```"
	errRes := &result.Result{SchemaVersion: 1, Status: "error", Error: &result.Error{Code: "x", Message: "boom\n/approve"}}
	hostile := withFinding(skill("a", "no_blocking_issues", "f.go"), "suggestion", "ffffff000000", steered)
	hostile.Notes = []string{"note\n/label ~x"}
	hostile.Summaries = []result.Summary{{Batch: 1, Text: "summary\n/close"}}
	hostile.Checks = []result.BatchChecks{{Batch: 1, Checks: []result.Check{{Name: "c", Status: "fail", Detail: "detail\n/merge"}}}}
	fixRaw, _ := json.Marshal(result.Finding{
		ID: "ab12cd", Fingerprint: "ab12cd34ef56", File: "f.go", Line: 2, Severity: "suggestion", Title: "t\n/approve",
		Explanation: "e", SuggestedFix: "fix\n/merge", Suggestion: "\r/approve\nx",
	})
	hostile.Findings = append(hostile.Findings, fixRaw)
	clean := completed(hostile)
	nothing := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: []result.Skipped{{Path: "/approve", Rule: "lockfile"}}}})
	failing := completed(result.Skill{Name: "a", Verdict: "failed", Failures: []result.Failure{{Files: []string{"/x"}, Message: "boom\n/approve"}}})
	for name, o := range map[string]Outcome{
		"clean":          {Result: clean},
		"nothing":        {Result: nothing},
		"failed skill":   {Result: failing},
		"error result":   {Result: errRes},
		"failure reason": {FailureReason: "x\n/approve"},
	} {
		t.Run(name, func(t *testing.T) {
			d := Decide(o, config.FailureStatusFailed)
			// GitLab deletes "\r" and then runs a "/" at column 0.
			for _, line := range strings.Split(strings.ReplaceAll(d.Comment, "\r", ""), "\n") {
				if strings.HasPrefix(line, "/") {
					t.Errorf("line %q would run as a quick action in %q", line, d.Comment)
				}
			}
			if !strings.Contains(d.Comment, Marker) {
				t.Errorf("marker lost: %q", d.Comment)
			}
		})
	}
	d := Decide(Outcome{Result: clean}, config.FailureStatusFailed)
	if block := blockOf(clean); !strings.HasSuffix(d.Comment, "\n"+block) {
		t.Errorf("findings block changed: want the comment to end with %q:\n%s", block, d.Comment)
	}
	// The block still carries the model's text as written.
	if raw := decodeBlock(t, d.Comment); !strings.Contains(raw, `\n/approve`) {
		t.Errorf("the findings block lost the model's text: %s", raw)
	}
	if !strings.Contains(d.Comment, "\n    // keep\n") {
		t.Errorf("indented code in a fence changed: %q", d.Comment)
	}
	if !strings.Contains(d.Comment, "\n\u200b/approve") {
		t.Errorf("a column-0 slash was not defused: %q", d.Comment)
	}
}

func TestDecidePartlySkippedNotApproved(t *testing.T) {
	for _, rule := range []string{"deleted", "binary", "vendored", "lockfile", "generated"} {
		t.Run(rule, func(t *testing.T) {
			r := completed(
				result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
					Reviewed: []string{"f.go"},
					Skipped:  []result.Skipped{{Path: "x/skipped", Rule: rule}, {Path: "docs/a.md", Rule: "out of scope"}},
				}},
				result.Skill{Name: "b", Verdict: "no_blocking_issues", Files: result.Files{
					Reviewed: []string{"f.go"},
					Skipped:  []result.Skipped{{Path: "x/skipped", Rule: "out of scope"}},
				}},
			)
			d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
			if d.Approve || d.Status != StatusSuccess || d.Description != "No blocking issues; not approved: some changed files weren't reviewed" {
				t.Fatalf("got %+v", d)
			}
			want := "---\n**Not approved:** some changed files were skipped by pergent's default rules, so they weren't reviewed. A human needs to look.\n\n" +
				"- `x/skipped` (" + strings.Join(sorted(rule, "out of scope"), ", ") + ")"
			if got := noteOf(t, d.Comment); got != want {
				t.Errorf("note:\n%q\nwant:\n%q", got, want)
			}
			checkNote(t, d.Comment, r, want)
		})
	}
}

func TestDecidePartlySkippedCapsAtTwenty(t *testing.T) {
	var skipped []result.Skipped
	for i := 0; i < 23; i++ {
		skipped = append(skipped, result.Skipped{Path: fmt.Sprintf("vendor/%02d.go", i), Rule: "vendored"})
	}
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Reviewed: []string{"f.go"}, Skipped: skipped}})
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Approve {
		t.Fatal("approved")
	}
	note := noteOf(t, d.Comment)
	if got := strings.Count(note, "(vendored)"); got != 20 {
		t.Errorf("listed %d, want 20", got)
	}
	if !strings.HasSuffix(note, "- …and 3 more") {
		t.Errorf("note: %q", note)
	}
	checkNote(t, d.Comment, r, note)
}

func sorted(s ...string) []string {
	slices.Sort(s)
	return s
}

const (
	wantNoteDraft              = "---\n**Not approved:** this is a draft. It will be approved when it's marked ready."
	wantNoteApproveUnavailable = "---\n**Not approved:** the pergent approve App isn't installed on this repository or lacks Pull requests: write."
)

// A draft is reviewed and gets its status, but is approved only once it is
// marked ready.
func TestDecideDraftNotApproved(t *testing.T) {
	r := completed(skill("a", "no_blocking_issues", "f.go"))
	d := Decide(Outcome{Result: r, Draft: true}, config.FailureStatusFailed)
	if d.Approve || d.Status != StatusSuccess || d.Description != "No blocking issues; not approved: draft" {
		t.Fatalf("got %+v", d)
	}
	checkNote(t, d.Comment, r, wantNoteDraft)
}

// The approve App missing from a repository: reviewed, success, no approval.
func TestDecideApproveUnavailableNotApproved(t *testing.T) {
	r := completed(skill("a", "no_blocking_issues", "f.go"))
	d := Decide(Outcome{Result: r, ApproveUnavailable: true}, config.FailureStatusFailed)
	if d.Approve || d.Status != StatusSuccess ||
		d.Description != "No blocking issues; not approved: the pergent approve App isn't installed on this repository or lacks Pull requests: write" {
		t.Fatalf("got %+v", d)
	}
	checkNote(t, d.Comment, r, wantNoteApproveUnavailable)
}

// A draft on a repository without the approve App: the draft is the reason.
func TestDecideDraftWinsOverApproveUnavailable(t *testing.T) {
	d := Decide(Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go")), Draft: true, ApproveUnavailable: true}, config.FailureStatusFailed)
	if d.Approve || d.Description != "No blocking issues; not approved: draft" ||
		!strings.Contains(d.Comment, wantNoteDraft) || strings.Contains(d.Comment, "approve App") {
		t.Fatalf("got %+v", d)
	}
}

// Draft and approve-unavailable only change a decision that would approve:
// every other row is decided as before.
func TestDecideDraftKeepsOtherRows(t *testing.T) {
	partly := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
		Reviewed: []string{"f.go"}, Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}})
	nothing := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
		Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}})
	cases := map[string]Outcome{
		"blocking":        {Result: completed(skill("a", "has_blocking_issues", "f.go"))},
		"failed skill":    {Result: completed(skill("a", "failed"))},
		"failure reason":  {FailureReason: "the job timed out"},
		"error result":    {Result: &result.Result{Status: "error", Comment: result.Comment{Markdown: Marker + "\nboom"}}},
		"partly skipped":  {Result: partly},
		"nothing to do":   {Result: nothing},
		"no result":       {},
		"unknown verdict": {Result: completed(skill("a", "weird", "f.go"))},
	}
	for name, o := range cases {
		for _, flags := range []struct{ draft, unavailable bool }{{true, false}, {false, true}, {true, true}} {
			t.Run(name, func(t *testing.T) {
				base := Decide(o, config.FailureStatusFailed)
				flagged := o
				flagged.Draft, flagged.ApproveUnavailable = flags.draft, flags.unavailable
				got := Decide(flagged, config.FailureStatusFailed)
				if got != base {
					t.Errorf("draft=%v unavailable=%v changed the decision:\n%+v\nwant\n%+v", flags.draft, flags.unavailable, got, base)
				}
			})
		}
	}
}

// A draft with blocking findings is failed, as any review with them.
func TestDecideDraftWithBlockingFindingsFails(t *testing.T) {
	d := Decide(Outcome{Result: completed(skill("a", "has_blocking_issues", "f.go")), Draft: true}, config.FailureStatusNeutral)
	if d.Approve || d.Status != StatusFailed || d.Description != "Blocking issues found" || strings.Contains(d.Comment, "draft") {
		t.Fatalf("got %+v", d)
	}
}

// The draft note goes after the review text and before the findings block,
// and is appended when there is no block.
func TestDecideDraftNoteWithoutBlock(t *testing.T) {
	r := completed(skill("a", "no_blocking_issues", "f.go"))
	d := Decide(withoutBlock(t, Outcome{Result: r, Draft: true}), config.FailureStatusFailed)
	if !strings.HasSuffix(d.Comment, "\n\n"+wantNoteDraft+"\n") || strings.Contains(d.Comment, "<!-- pergent:findings ") {
		t.Errorf("want the note appended at the end of a comment with no block:\n%s", d.Comment)
	}
}

// The approve side is a bot user's token (approve_token_env), not an App:
// the reason and note say the bot can't reach the repository.
func TestDecideApproveBotUnavailableNotApproved(t *testing.T) {
	r := completed(skill("a", "no_blocking_issues", "f.go"))
	d := Decide(Outcome{Result: r, ApproveUnavailable: true, ApproveBot: true}, config.FailureStatusFailed)
	if d.Approve || d.Status != StatusSuccess ||
		d.Description != "No blocking issues; not approved: the pergent approve bot can't reach this repository" {
		t.Fatalf("got %+v", d)
	}
	checkNote(t, d.Comment, r, "---\n**Not approved:** the pergent approve bot can't reach this repository.")
	// ApproveBot alone changes nothing.
	if d := Decide(Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go")), ApproveBot: true}, config.FailureStatusFailed); !d.Approve {
		t.Errorf("ApproveBot without ApproveUnavailable: %+v", d)
	}
}

// The review App approves (approve_with_review_app) and lacks Pull requests:
// write: the reason and note name the permission.
func TestDecideReviewAppApproverUnavailableNotApproved(t *testing.T) {
	r := completed(skill("a", "no_blocking_issues", "f.go"))
	d := Decide(Outcome{Result: r, ApproveUnavailable: true, ApproveReviewApp: true}, config.FailureStatusFailed)
	if d.Approve || d.Status != StatusSuccess ||
		d.Description != "No blocking issues; not approved: the pergent review App lacks Pull requests: write on this repository" {
		t.Fatalf("got %+v", d)
	}
	checkNote(t, d.Comment, r, "---\n**Not approved:** the pergent review App lacks Pull requests: write on this repository.")
	// ApproveReviewApp alone changes nothing.
	if d := Decide(Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go")), ApproveReviewApp: true}, config.FailureStatusFailed); !d.Approve {
		t.Errorf("ApproveReviewApp without ApproveUnavailable: %+v", d)
	}
}

// Any skip rule that isn't known to leave the change reviewed blocks
// approval: a rule pergent adds later (or a result that makes one up) is a
// file nobody reviewed.
func TestDecideUnknownSkipRuleNotApproved(t *testing.T) {
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
		Reviewed: []string{"f.go"},
		Skipped:  []result.Skipped{{Path: "x/new", Rule: "too large"}, {Path: "docs/a.md", Rule: "out of scope"}},
	}})
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Approve || d.Status != StatusSuccess || d.Description != "No blocking issues; not approved: some changed files weren't reviewed" {
		t.Fatalf("got %+v", d)
	}
	note := noteOf(t, d.Comment)
	if !strings.Contains(note, "\n- `x/new` (too large)") {
		t.Errorf("the note doesn't list the file: %q", note)
	}
	if strings.Contains(note, "docs/a.md") {
		t.Errorf("the note lists an out-of-scope file: %q", note)
	}
	checkNote(t, d.Comment, r, note)

	// Nothing reviewed, only an unknown rule: still not approved.
	only := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
		Skipped: []result.Skipped{{Path: "x/new", Rule: "too large"}}}})
	if d := Decide(Outcome{Result: only}, config.FailureStatusFailed); d.Approve {
		t.Fatalf("approved a review of nothing: %+v", d)
	}
}

// Paths come from the merge request: a backtick, newline or other control
// character in one can't break out of its code span or line, and the note
// stays small whatever the paths.
func TestSkippedNoteEscapesPaths(t *testing.T) {
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
		Reviewed: []string{"f.go"},
		Skipped: []result.Skipped{
			{Path: "a`b\n## pwned\rc\x00d", Rule: "lockfile"},
			{Path: "e\u202ef", Rule: "binary\n# x`"},
		},
	}})
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Approve {
		t.Fatal("approved")
	}
	note := noteOf(t, d.Comment)
	for _, want := range []string{
		"\n- `\"a\\x60b\\n## pwned\\rc\\x00d\"` (lockfile)",
		"\n- `\"e\\u202ef\"` (`\"binary\\n# x\\x60\"`)",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%q", want, note)
		}
	}
	// The note is policy's: nothing raw gets into it. The Files list above it
	// is render's, and keeps a path on its own line.
	if strings.Contains(note, "\n## pwned") || strings.Contains(note, "\x00") || strings.Contains(note, "\u202e") {
		t.Errorf("a raw control character reached the note: %q", note)
	}
	if strings.Contains(d.Comment, "\n## pwned") {
		t.Errorf("a path started a heading: %q", d.Comment)
	}
}

func TestSkippedNoteCapsPathLengthAndSize(t *testing.T) {
	long := strings.Repeat("é", 500)
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
		Reviewed: []string{"f.go"},
		Skipped:  []result.Skipped{{Path: long, Rule: "vendored"}},
	}})
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	want := "\n- `" + strings.Repeat("é", maxListedPath) + "…` (vendored)"
	if got := noteOf(t, d.Comment); !strings.Contains(got, want) {
		t.Errorf("long path not cut at %d characters: %q", maxListedPath, got)
	}

	// 20 paths and a rule each at their longest stay under the note's cap.
	var skipped []result.Skipped
	for i := 0; i < 40; i++ {
		skipped = append(skipped,
			result.Skipped{Path: fmt.Sprintf("%02d", i) + strings.Repeat("\x01", 400), Rule: "lockfile"},
			result.Skipped{Path: fmt.Sprintf("%02d", i) + strings.Repeat("\x01", 400), Rule: strings.Repeat("\x02", 400)},
			result.Skipped{Path: fmt.Sprintf("%02d", i) + strings.Repeat("\x01", 400), Rule: strings.Repeat("\x03", 400)},
		)
	}
	big := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Reviewed: []string{"f.go"}, Skipped: skipped}})
	d = Decide(Outcome{Result: big}, config.FailureStatusFailed)
	note := noteOf(t, d.Comment)
	if len(note) > maxNoteBytes {
		t.Errorf("note is %d bytes, want at most %d", len(note), maxNoteBytes)
	}
	if !strings.HasSuffix(note, " more") {
		t.Errorf("note doesn't say how many paths it left out: %q", note)
	}
}

const wantNoteApprovalOff = "---\n**Not approved:** approval is turned off for this repository."

// Approval turned off for the repository: a clean review is reviewed,
// commented and gets its success status, but is never approved.
func TestDecideApprovalOff(t *testing.T) {
	r := completed(skill("a", "no_blocking_issues", "f.go"))
	d := Decide(Outcome{Result: r, ApprovalOff: true}, config.FailureStatusFailed)
	if d.Approve || d.Status != StatusSuccess ||
		d.Description != "No blocking issues; not approved: approval is turned off" {
		t.Fatalf("got %+v", d)
	}
	checkNote(t, d.Comment, r, wantNoteApprovalOff)
}

// Approval off is the reason over a draft or an unreachable approve side:
// neither would be approved later while it stays off.
func TestDecideApprovalOffWinsOverDraftAndUnavailable(t *testing.T) {
	for _, o := range []Outcome{
		{Draft: true}, {ApproveUnavailable: true}, {ApproveUnavailable: true, ApproveBot: true}, {ApproveUnavailable: true, ApproveReviewApp: true},
		{Draft: true, ApproveUnavailable: true},
	} {
		o.Result, o.ApprovalOff = completed(skill("a", "no_blocking_issues", "f.go")), true
		d := Decide(o, config.FailureStatusFailed)
		if d.Approve || d.Description != "No blocking issues; not approved: approval is turned off" ||
			!strings.Contains(d.Comment, wantNoteApprovalOff) || strings.Count(d.Comment, "**Not approved:**") != 1 {
			t.Errorf("%+v: got %+v", o, d)
		}
	}
}

// Approval off only changes a decision that would approve: every other row
// keeps its own outcome and note.
func TestDecideApprovalOffKeepsOtherRows(t *testing.T) {
	partly := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
		Reviewed: []string{"f.go"}, Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}})
	nothing := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{
		Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}})
	cases := map[string]Outcome{
		"blocking":        {Result: completed(skill("a", "has_blocking_issues", "f.go"))},
		"failed skill":    {Result: completed(skill("a", "failed"))},
		"failure reason":  {FailureReason: "the job timed out"},
		"error result":    {Result: &result.Result{Status: "error", Comment: result.Comment{Markdown: Marker + "\nboom"}}},
		"partly skipped":  {Result: partly},
		"nothing to do":   {Result: nothing},
		"no result":       {},
		"unknown verdict": {Result: completed(skill("a", "weird", "f.go"))},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			for _, fs := range []config.FailureStatus{config.FailureStatusFailed, config.FailureStatusNeutral} {
				base := Decide(o, fs)
				off := o
				off.ApprovalOff = true
				if got := Decide(off, fs); got != base {
					t.Errorf("approval off changed the decision:\n%+v\nwant\n%+v", got, base)
				}
			}
		})
	}
}

// Kind says what a decision is, for recording on the run.
func TestDecideKind(t *testing.T) {
	skipped := skill("security", "no_blocking_issues")
	skipped.Files.Skipped = []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}
	for name, tc := range map[string]struct {
		o    Outcome
		want Kind
	}{
		"failure":          {Outcome{FailureReason: "boom"}, KindFailed},
		"no result":        {Outcome{}, KindFailed},
		"error result":     {Outcome{Result: &result.Result{SchemaVersion: 1, Status: "error"}}, KindFailed},
		"failed skill":     {Outcome{Result: completed(skill("a", "failed", "x.go"))}, KindFailed},
		"blocking":         {Outcome{Result: completed(skill("a", "has_blocking_issues", "x.go"))}, KindBlocking},
		"nothing reviewed": {Outcome{Result: completed(skipped)}, KindNotApproved},
		"approval off":     {Outcome{Result: completed(skill("a", "no_blocking_issues", "x.go")), ApprovalOff: true}, KindNotApproved},
		"unavailable":      {Outcome{Result: completed(skill("a", "no_blocking_issues", "x.go")), ApproveUnavailable: true}, KindNotApproved},
		"draft":            {Outcome{Result: completed(skill("a", "no_blocking_issues", "x.go")), Draft: true}, KindDraft},
		"clean":            {Outcome{Result: completed(skill("a", "no_blocking_issues", "x.go"))}, KindApprove},
	} {
		if got := Decide(tc.o, config.FailureStatusFailed).Kind; got != tc.want {
			t.Errorf("%s: Kind = %q, want %q", name, got, tc.want)
		}
	}
}

func withFinding(s result.Skill, severity, fingerprint, explanation string) result.Skill {
	b, _ := json.Marshal(result.Finding{
		ID: fingerprint[:6], Fingerprint: fingerprint, File: "db/query.go", Line: 3, Severity: severity,
		Title: "Unchecked input", Explanation: explanation,
	})
	s.Findings = append(s.Findings, b)
	return s
}

func TestDecideDismissals(t *testing.T) {
	const fp = "abc123def456"
	blocking := withFinding(skill("security", "has_blocking_issues", "db/query.go"), "blocking", fp, "plain")
	dismissal := render.Dismissal{Fingerprint: fp, Severity: "blocking", By: "maya", Reason: "ok"}

	t.Run("all blocking dismissed approves", func(t *testing.T) {
		d := Decide(Outcome{Result: completed(blocking), Dismissals: []render.Dismissal{dismissal}}, config.FailureStatusFailed)
		if d.Kind != KindApprove || !d.Approve || d.Status != StatusSuccess {
			t.Fatalf("decision = %+v, want an approval", d)
		}
		if !strings.HasPrefix(d.Comment, Marker+"\n### ✅ No blocking issues (1 dismissed)") || !strings.Contains(d.Comment, "Dismissed (1)") ||
			!strings.Contains(d.Comment, "dismissed by maya: ok") || strings.Contains(d.Comment, "#### Findings") {
			t.Errorf("comment = %q, want the dismissed finding in its own section and no open finding", d.Comment)
		}
	})
	t.Run("another blocking finding still blocks", func(t *testing.T) {
		two := withFinding(blocking, "blocking", "ffffff000000", "plain")
		d := Decide(Outcome{Result: completed(two), Dismissals: []render.Dismissal{dismissal}}, config.FailureStatusFailed)
		if d.Kind != KindBlocking || d.Approve || !strings.Contains(d.Comment, "1 blocking issue") {
			t.Errorf("decision = %+v", d)
		}
	})
	t.Run("a dismissal covering nothing keeps the comment", func(t *testing.T) {
		other := render.Dismissal{Fingerprint: "ffffff000000", Severity: "blocking"}
		r := completed(blocking)
		d := Decide(Outcome{Result: r, Dismissals: []render.Dismissal{other}}, config.FailureStatusFailed)
		if want := Decide(Outcome{Result: r}, config.FailureStatusFailed); d != want || d.Kind != KindBlocking ||
			strings.Contains(d.Comment, "Dismissed (") || !strings.Contains(d.Comment, "#### Findings (1)") {
			t.Errorf("decision = %+v, want the comment without dismissals and blocking", d)
		}
	})
	t.Run("notes go before the new findings block", func(t *testing.T) {
		d := Decide(Outcome{Result: completed(blocking), Dismissals: []render.Dismissal{dismissal}, Draft: true}, config.FailureStatusFailed)
		i := strings.Index(d.Comment, noteDraft)
		j := strings.Index(d.Comment, "<!-- pergent:findings ")
		if d.Kind != KindDraft || i < 0 || j < i || !strings.HasSuffix(d.Comment, "-->\n") {
			t.Errorf("comment = %q, want the draft note before the findings block, which stays last", d.Comment)
		}
	})
	t.Run("redacted and neutralized", func(t *testing.T) {
		// The suggestion stays in the comment; the model text in it is untrusted.
		s := withFinding(blocking, "suggestion", "ffffff000000", "key sk-secret-123\n/approve")
		o := Outcome{
			Result: completed(s), Dismissals: []render.Dismissal{dismissal},
			Redact: func(t string) string { return strings.ReplaceAll(t, "sk-secret-123", "***") },
		}
		d := Decide(o, config.FailureStatusFailed)
		if strings.Contains(d.Comment, "sk-secret-123") {
			t.Errorf("comment leaks the secret: %q", d.Comment)
		}
		if strings.Contains(d.Comment, "\n/approve") || !strings.Contains(d.Comment, "\n\u200b/approve") {
			t.Errorf("comment runs a quick action: %q", d.Comment)
		}
	})
	t.Run("size limit", func(t *testing.T) {
		long := strings.Repeat("word ", 4000)
		s := withFinding(skill("security", "no_blocking_issues", "db/query.go"), "suggestion", "ffffff000000", long)
		s = withFinding(s, "suggestion", fp, "x")
		d := Decide(Outcome{
			Result: completed(s), Dismissals: []render.Dismissal{{Fingerprint: fp, Severity: "suggestion"}}, MaxComment: 5000,
		}, config.FailureStatusFailed)
		if len(d.Comment) > 5000 {
			t.Errorf("comment is %d bytes, want at most 5000", len(d.Comment))
		}
	})
	t.Run("failed skill stays failed", func(t *testing.T) {
		f := blocking
		f.Verdict = "failed"
		d := Decide(Outcome{Result: completed(f), Dismissals: []render.Dismissal{dismissal}}, config.FailureStatusFailed)
		if d.Kind != KindFailed || d.Approve {
			t.Errorf("decision = %+v, want failed", d)
		}
	})
}

// The text of the review is redacted before it is rendered: the findings
// block carries it base64-encoded, where a pass over the finished comment
// can't see a credential.
func TestDecideDismissalsRedactBeforeRendering(t *testing.T) {
	const fp, key = "abc123def456", "sk-secret-123"
	redact := func(t string) string { return strings.ReplaceAll(t, key, "***") }
	s := withFinding(skill("security", "has_blocking_issues", "db/query.go"), "blocking", fp, "plain")
	s = withFinding(s, "suggestion", "ffffff000000", "uses "+key)
	s.Failures = []result.Failure{{Files: []string{"a.go"}, Message: "failed with " + key}}
	s.Notes = []string{"note " + key}
	s.Summaries = []result.Summary{{Batch: 1, Text: "summary " + key}}
	s.Checks = []result.BatchChecks{{Batch: 1, Checks: []result.Check{{Name: "c", Status: "pass", Detail: key}}}}
	dismissal := render.Dismissal{Fingerprint: fp, Severity: "blocking", By: "maya " + key, Reason: "reason " + key}

	for name, max := range map[string]int{"no limit": 0, "small limit": 600} {
		t.Run(name, func(t *testing.T) {
			d := Decide(Outcome{Result: completed(s), Dismissals: []render.Dismissal{dismissal}, MaxComment: max, Redact: redact}, config.FailureStatusFailed)
			if strings.Contains(d.Comment, key) {
				t.Errorf("comment leaks the key: %q", d.Comment)
			}
			if max > 0 && len(d.Comment) > max {
				t.Errorf("comment is %d bytes, want at most %d", len(d.Comment), max)
			}
			i := strings.Index(d.Comment, "<!-- pergent:findings ")
			if i < 0 {
				if max == 0 {
					t.Fatalf("no findings block in %q", d.Comment)
				}
				return
			}
			enc := strings.TrimSuffix(strings.TrimSpace(d.Comment[i+len("<!-- pergent:findings "):]), " -->")
			raw, err := base64.StdEncoding.DecodeString(enc)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), key) || !strings.Contains(string(raw), "uses ***") {
				t.Errorf("findings block = %s, want the key redacted", raw)
			}
		})
	}
}

// A skill with files it could not review is a failure whatever is dismissed.
func TestDecideDismissalsKeepNotReviewedFailed(t *testing.T) {
	const fp = "abc123def456"
	s := withFinding(skill("security", "has_blocking_issues", "db/query.go"), "blocking", fp, "plain")
	s.Files.NotReviewed = []json.RawMessage{json.RawMessage(`{"path":"g.go","cause":"timeout"}`)}
	d := Decide(Outcome{Result: completed(s), Dismissals: []render.Dismissal{{Fingerprint: fp, Severity: "blocking"}}}, config.FailureStatusFailed)
	if d.Kind != KindFailed || d.Approve || d.Status != StatusFailed {
		t.Errorf("decision = %+v, want failed and not approved", d)
	}
}

// A merged result carries the kept skills of an earlier run, which the stored
// comment (the re-run skills' only) doesn't show: the comment is rendered from
// the result, so it covers every skill.
func TestDecideMergedResultIsRenderedAgain(t *testing.T) {
	kept := withFinding(skill("security", "has_blocking_issues", "db/query.go"), "blocking", "abc123def456", "plain")
	rerun := skill("style", "no_blocking_issues", "a.go")
	r := completed(kept, rerun)
	r.Merged = true
	r.Comment.Markdown = "### style\n\nNo blocking issues.\n"

	d := Decide(Outcome{Result: r, Redact: func(s string) string { return strings.ReplaceAll(s, "plain", "***") }}, config.FailureStatusFailed)

	if d.Kind != KindBlocking {
		t.Errorf("kind = %q, want blocking", d.Kind)
	}
	for _, want := range []string{"| `security` | ❌ Has blocking issues", "| `style` | ✅ No blocking issues", "Unchecked input", "`db/query.go`", "`a.go`"} {
		if !strings.Contains(d.Comment, want) {
			t.Errorf("comment lacks %q:\n%s", want, d.Comment)
		}
	}
	if strings.Contains(d.Comment, "plain") {
		t.Errorf("the merged comment was not redacted:\n%s", d.Comment)
	}
	if strings.Contains(d.Comment, "No blocking issues.\n") {
		t.Errorf("the stored comment was used:\n%s", d.Comment)
	}

	// Merged or not, the comment is the result's own, never the stored one.
	r.Merged = false
	plain := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if !strings.Contains(plain.Comment, "| `security` |") || !strings.Contains(plain.Comment, "| `style` |") || strings.Contains(plain.Comment, "### style") {
		t.Errorf("an unmerged result's comment is rendered from the result too: %q", plain.Comment)
	}
}

// A re-run whose earlier results could not be merged covers only the skills
// that were re-run: it is never approved, whatever else allows it.
func TestDecideMergeNote(t *testing.T) {
	r := completed(skill("style", "no_blocking_issues", "a.go"))
	r.MergeNote = result.NoteBaseUnreadable
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	i, j := strings.Index(d.Comment, result.NoteBaseUnreadable), strings.Index(d.Comment, "<!-- pergent:findings ")
	if i < 0 || j < i {
		t.Errorf("comment = %q, want the note before the findings block", d.Comment)
	}
	if d.Approve || d.Kind != KindNotApproved || d.Status != StatusSuccess ||
		!strings.HasPrefix(d.Description, descNotApproved) || !strings.Contains(d.Description, result.NoteBaseUnreadable) {
		t.Errorf("decision = %+v, want not approved because of the note", d)
	}

	blocking := completed(withFinding(skill("style", "has_blocking_issues", "a.go"), "blocking", "abc123def456", "x"))
	blocking.MergeNote = result.NoteBaseUnreadable
	if d := Decide(Outcome{Result: blocking}, config.FailureStatusFailed); d.Kind != KindBlocking {
		t.Errorf("blocking findings still block: %+v", d)
	}
	failed := completed(skill("style", "failed", "a.go"))
	failed.MergeNote = result.NoteBaseUnreadable
	if d := Decide(Outcome{Result: failed}, config.FailureStatusFailed); d.Kind != KindFailed {
		t.Errorf("a failed skill still fails: %+v", d)
	}
}

// The comment of a completed review shows the commit, the pergent version, the
// dismiss command of each finding and the re-run command when the outcome has
// a mention, and links each finding's location with FileURL.
func TestDecideRendersCommandsAndLinks(t *testing.T) {
	const fp = "abc123def456"
	r := completed(withFinding(skill("security", "has_blocking_issues", "db/query.go"), "blocking", fp, "plain"))
	r.Head, r.PergentVersion = "0123456789abcdef", "0.3.0"
	o := Outcome{
		Result: r, Mention: "pergent-bot",
		FileURL: func(path string, line, endLine int) string {
			return fmt.Sprintf("https://example.com/blob/%s#L%d", path, line)
		},
	}
	d := Decide(o, config.FailureStatusFailed)
	for _, want := range []string{
		"Reply `@pergent-bot dismiss abc123 <reason>`",
		"Run again: `@pergent-bot review`",
		"pergent v0.3.0",
		"Commit `0123456`",
		`<a href="https://example.com/blob/db/query.go#L3">`,
	} {
		if !strings.Contains(d.Comment, want) {
			t.Errorf("comment lacks %q:\n%s", want, d.Comment)
		}
	}
	if strings.Contains(d.Comment, "0123456789abcdef") {
		t.Errorf("the full commit is shown, want the short one:\n%s", d.Comment)
	}
	// With a dismiss command the ID is part of it, not a separate label.
	if strings.Contains(d.Comment, "· id <code>") {
		t.Errorf("the ID is listed next to the command:\n%s", d.Comment)
	}
	if !strings.HasPrefix(d.Comment, Marker+"\n") || !strings.HasSuffix(d.Comment, blockOf(r)) {
		t.Errorf("want the marker first and the findings block last:\n%s", d.Comment)
	}
}

// Without a mention the comment has no command to give: no dismiss line and no
// re-run footer. The version and the link still show.
func TestDecideNoMentionNoCommands(t *testing.T) {
	r := completed(withFinding(skill("security", "has_blocking_issues", "db/query.go"), "blocking", "abc123def456", "plain"))
	r.Head, r.PergentVersion = "0123456789abcdef", "v0.3.0"
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	for _, bad := range []string{"dismiss", "Run again", "@"} {
		if strings.Contains(d.Comment, bad) {
			t.Errorf("comment has %q without a mention:\n%s", bad, d.Comment)
		}
	}
	// The ID stays so a person can still quote it, and the version is shown
	// once, with a single "v".
	for _, want := range []string{"id <code>abc123</code>", "pergent v0.3.0"} {
		if !strings.Contains(d.Comment, want) {
			t.Errorf("comment lacks %q:\n%s", want, d.Comment)
		}
	}
	if strings.Contains(d.Comment, "<a href") {
		t.Errorf("a link without a FileURL:\n%s", d.Comment)
	}
}

// The model's confidence score and a finding's suggested change reach the
// comment and the findings block. (A blocking finding caps the score at 3, so
// the finding here is a suggestion.)
func TestDecideKeepsConfidenceAndSuggestion(t *testing.T) {
	s := skill("security", "no_blocking_issues", "db/query.go")
	s.Confidence, s.ConfidenceReason = 4, "read every changed file"
	raw, err := json.Marshal(result.Finding{
		ID: "abc123", Fingerprint: "abc123def456", File: "db/query.go", Line: 3, EndLine: 5, Severity: "suggestion",
		Title: "Unchecked input", Explanation: "plain", Suggestion: "if x == nil {\n\treturn\n}",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Findings = append(s.Findings, raw)
	r := completed(s)
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)

	for _, want := range []string{
		"### ✅ No blocking issues · Confidence 4/5",
		"> read every changed file",
		"_Suggested change, replacing lines 3–5:_",
		"```\nif x == nil {\n\treturn\n}\n```",
	} {
		if !strings.Contains(d.Comment, want) {
			t.Errorf("comment lacks %q:\n%s", want, d.Comment)
		}
	}
	block := blockOf(r)
	if !strings.HasSuffix(d.Comment, block) {
		t.Errorf("the comment must end with the findings block %q:\n%s", block, d.Comment)
	}
	raws := decodeBlock(t, d.Comment)
	for _, want := range []string{`"confidence":{"security":{"score":4,"reason":"read every changed file"}}`, `"end_line":5`, `"suggestion":"if x == nil {\n\treturn\n}"`} {
		if !strings.Contains(raws, want) {
			t.Errorf("findings block lacks %s: %s", want, raws)
		}
	}
}

// The confidence reason and a suggested change are model text: they are
// redacted before they are rendered, in the comment and in the findings block.
func TestDecideRedactsConfidenceReasonAndSuggestion(t *testing.T) {
	const key = "sk-secret-123"
	redact := func(t string) string { return strings.ReplaceAll(t, key, "***") }
	s := skill("security", "has_blocking_issues", "db/query.go")
	s.Confidence, s.ConfidenceReason = 3, "checked with "+key
	raw, err := json.Marshal(result.Finding{
		ID: "abc123", Fingerprint: "abc123def456", File: "db/query.go", Line: 3, Severity: "blocking",
		Title: "Unchecked input", Explanation: "plain", Suggestion: "token := \"" + key + "\"",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Findings = append(s.Findings, raw)

	for name, dismissals := range map[string][]render.Dismissal{
		"rendered from the result": nil,
		"with a dismissal":         {{Fingerprint: "ffffff000000", Severity: "blocking", By: "maya"}},
	} {
		t.Run(name, func(t *testing.T) {
			d := Decide(Outcome{Result: completed(s), Redact: redact, Dismissals: dismissals}, config.FailureStatusFailed)
			if strings.Contains(d.Comment, key) {
				t.Errorf("comment leaks the key: %s", d.Comment)
			}
			if raw := decodeBlock(t, d.Comment); strings.Contains(raw, key) || !strings.Contains(raw, "checked with ***") || !strings.Contains(raw, `token := \"***\"`) {
				t.Errorf("findings block = %s, want the key redacted", raw)
			}
			for _, want := range []string{"> checked with ***", `token := "***"`} {
				if !strings.Contains(d.Comment, want) {
					t.Errorf("comment lacks %q:\n%s", want, d.Comment)
				}
			}
		})
	}
}

// The Job's comment is never posted for a completed review.
func TestDecideIgnoresJobComment(t *testing.T) {
	d := Decide(Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go"))}, config.FailureStatusFailed)
	if strings.Contains(d.Comment, jobComment) || strings.Contains(d.Comment, findingsBlock) {
		t.Errorf("the Job's comment was used: %s", d.Comment)
	}
}
