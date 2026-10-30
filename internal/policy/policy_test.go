package policy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/result"
)

const findingsBlock = "<!-- pergent:findings eyJ9 -->"

func skill(name, verdict string, reviewed ...string) result.Skill {
	return result.Skill{Name: name, Verdict: verdict, Files: result.Files{Reviewed: reviewed}}
}

func completed(skills ...result.Skill) *result.Result {
	return &result.Result{
		SchemaVersion: 1, Status: "completed", Skills: skills,
		Comment: result.Comment{Markdown: Marker + "\n### review\n\nbody\n\n" + findingsBlock, FindingsBlock: findingsBlock},
	}
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
			wantComment: Marker + "\n### review\n\nbody\n\n" + findingsBlock,
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
	want := Marker + "\n### review\n\nbody\n\n" +
		"---\n**Not approved:** every changed file was skipped by pergent's default rules, so nothing was reviewed. A human needs to look.\n\n" +
		"- `go.sum` (lockfile)\n- `vendor/x.go` (vendored)\n\n" +
		findingsBlock
	if d.Comment != want {
		t.Errorf("comment:\n%q\nwant:\n%q", d.Comment, want)
	}
}

func TestDecideNothingReviewedNoteAppendedWithoutBlock(t *testing.T) {
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}})
	r.Comment = result.Comment{Markdown: Marker + "\nbody"}
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	want := Marker + "\nbody\n\n---\n**Not approved:** every changed file was skipped by pergent's default rules, so nothing was reviewed. A human needs to look.\n\n- `go.sum` (lockfile)\n"
	if d.Comment != want {
		t.Errorf("comment:\n%q\nwant:\n%q", d.Comment, want)
	}
	if d.Approve {
		t.Error("approved")
	}
}

func TestDecideNothingReviewedCapsAtTwenty(t *testing.T) {
	var skipped []result.Skipped
	for i := 0; i < 25; i++ {
		skipped = append(skipped, result.Skipped{Path: fmt.Sprintf("lock/%02d.lock", i), Rule: "lockfile"})
	}
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: skipped}})
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if got := strings.Count(d.Comment, "(lockfile)"); got != 20 {
		t.Errorf("listed %d, want 20", got)
	}
	if !strings.Contains(d.Comment, "- …and 5 more") {
		t.Errorf("missing overflow line: %q", d.Comment)
	}
	if !strings.Contains(d.Comment, "lock/19.lock") || strings.Contains(d.Comment, "lock/20.lock") {
		t.Error("expected the first 20 sorted paths")
	}
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
	r := completed(skill("a", "has_blocking_issues", "f.go"))
	r.Comment.Markdown = "### no marker here"
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Comment != Marker+"\n### no marker here" {
		t.Errorf("comment: %q", d.Comment)
	}
	// already present: left alone
	r.Comment.Markdown = "x\n" + Marker + "\ny"
	if d := Decide(Outcome{Result: r}, config.FailureStatusFailed); d.Comment != r.Comment.Markdown {
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
	// pergent's findings_block ends with a newline, and so does the markdown.
	block := "<!-- pergent:findings eyJ9 -->\n"
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}})
	r.Comment = result.Comment{Markdown: Marker + "\n### review\n\nbody\n\n" + block, FindingsBlock: block}
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
	if strings.Count(d.Comment, "`go.sum`") != 1 || !strings.Contains(d.Comment, "- `go.sum` (lockfile, out of scope)") {
		t.Errorf("comment: %q", d.Comment)
	}
	if strings.Index(d.Comment, "`a.go`") > strings.Index(d.Comment, "`go.sum`") {
		t.Errorf("not sorted: %q", d.Comment)
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
	if got := strings.Count(d.Comment, "(lockfile, vendored)"); got != 20 {
		t.Errorf("listed %d, want 20", got)
	}
	if !strings.Contains(d.Comment, "- …and 2 more") {
		t.Errorf("comment: %q", d.Comment)
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
	steered := Marker + "\n### review\n\nexplanation\n/approve\n\r/merge\n```\n/close\n    // keep\n```\n\n" + findingsBlock
	errRes := &result.Result{SchemaVersion: 1, Status: "error", Error: &result.Error{Code: "x", Message: "boom\n/approve"}}
	clean := completed(skill("a", "no_blocking_issues", "f.go"))
	clean.Comment.Markdown = steered
	nothing := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Skipped: []result.Skipped{{Path: "/approve", Rule: "lockfile"}}}})
	nothing.Comment.Markdown = steered
	for name, o := range map[string]Outcome{
		"clean":          {Result: clean},
		"nothing":        {Result: nothing},
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
	if !strings.HasSuffix(d.Comment, "\n"+findingsBlock) {
		t.Errorf("findings block changed: %q", d.Comment)
	}
	if !strings.Contains(d.Comment, "\n    // keep\n") {
		t.Errorf("indented code in a fence changed: %q", d.Comment)
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
			want := Marker + "\n### review\n\nbody\n\n" +
				"---\n**Not approved:** some changed files were skipped by pergent's default rules, so they weren't reviewed. A human needs to look.\n\n" +
				"- `x/skipped` (" + strings.Join(sorted(rule, "out of scope"), ", ") + ")\n\n" +
				findingsBlock
			if d.Comment != want {
				t.Errorf("comment:\n%q\nwant:\n%q", d.Comment, want)
			}
		})
	}
}

func TestDecidePartlySkippedCapsAtTwenty(t *testing.T) {
	var skipped []result.Skipped
	for i := 0; i < 23; i++ {
		skipped = append(skipped, result.Skipped{Path: fmt.Sprintf("vendor/%02d.go", i), Rule: "vendored"})
	}
	r := completed(result.Skill{Name: "a", Verdict: "no_blocking_issues", Files: result.Files{Reviewed: []string{"f.go"}, Skipped: skipped}})
	r.Comment = result.Comment{Markdown: Marker + "\nbody"}
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Approve {
		t.Fatal("approved")
	}
	if got := strings.Count(d.Comment, "(vendored)"); got != 20 {
		t.Errorf("listed %d, want 20", got)
	}
	if !strings.HasSuffix(d.Comment, "- …and 3 more\n") {
		t.Errorf("comment: %q", d.Comment)
	}
}

func sorted(s ...string) []string {
	slices.Sort(s)
	return s
}

const (
	wantNoteDraft              = "---\n**Not approved:** this is a draft. It will be approved when it's marked ready."
	wantNoteApproveUnavailable = "---\n**Not approved:** the pergent approve App isn't installed on this repository."
)

// A draft is reviewed and gets its status, but is approved only once it is
// marked ready.
func TestDecideDraftNotApproved(t *testing.T) {
	d := Decide(Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go")), Draft: true}, config.FailureStatusFailed)
	if d.Approve || d.Status != StatusSuccess || d.Description != "No blocking issues; not approved: draft" {
		t.Fatalf("got %+v", d)
	}
	want := Marker + "\n### review\n\nbody\n\n" + wantNoteDraft + "\n\n" + findingsBlock
	if d.Comment != want {
		t.Errorf("comment:\n%q\nwant:\n%q", d.Comment, want)
	}
}

// The approve App missing from a repository: reviewed, success, no approval.
func TestDecideApproveUnavailableNotApproved(t *testing.T) {
	d := Decide(Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go")), ApproveUnavailable: true}, config.FailureStatusFailed)
	if d.Approve || d.Status != StatusSuccess ||
		d.Description != "No blocking issues; not approved: the pergent approve App isn't installed on this repository" {
		t.Fatalf("got %+v", d)
	}
	want := Marker + "\n### review\n\nbody\n\n" + wantNoteApproveUnavailable + "\n\n" + findingsBlock
	if d.Comment != want {
		t.Errorf("comment:\n%q\nwant:\n%q", d.Comment, want)
	}
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
	r.Comment = result.Comment{Markdown: Marker + "\nbody"}
	d := Decide(Outcome{Result: r, Draft: true}, config.FailureStatusFailed)
	if want := Marker + "\nbody\n\n" + wantNoteDraft + "\n"; d.Comment != want {
		t.Errorf("comment:\n%q\nwant:\n%q", d.Comment, want)
	}
}

// The approve side is a bot user's token (approve_token_env), not an App:
// the reason and note say the bot can't reach the repository.
func TestDecideApproveBotUnavailableNotApproved(t *testing.T) {
	d := Decide(Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go")), ApproveUnavailable: true, ApproveBot: true}, config.FailureStatusFailed)
	if d.Approve || d.Status != StatusSuccess ||
		d.Description != "No blocking issues; not approved: the pergent approve bot can't reach this repository" {
		t.Fatalf("got %+v", d)
	}
	want := Marker + "\n### review\n\nbody\n\n---\n**Not approved:** the pergent approve bot can't reach this repository.\n\n" + findingsBlock
	if d.Comment != want {
		t.Errorf("comment:\n%q\nwant:\n%q", d.Comment, want)
	}
	// ApproveBot alone changes nothing.
	if d := Decide(Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go")), ApproveBot: true}, config.FailureStatusFailed); !d.Approve {
		t.Errorf("ApproveBot without ApproveUnavailable: %+v", d)
	}
}
