package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/result"
)

const cleanImpactNote = "Impact: checked gopaytech/pergent.sh, nothing found"

// impactSkill is the skill as pergent reports it: it lists no file, whatever
// it looked at.
func impactSkill(verdict string, notes ...string) result.Skill {
	return result.Skill{Name: result.ImpactSkill, Verdict: verdict, Notes: notes}
}

// A change that only the impact check covered was not reviewed: the other
// skills skipped every file, and impact must not make up for that.
func TestImpactDoesntCountAsReviewed(t *testing.T) {
	skipped := result.Skill{Name: "security", Verdict: "no_blocking_issues",
		Files: result.Files{Skipped: []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}}}
	for name, impact := range map[string]result.Skill{
		"listing no file":        impactSkill("no_blocking_issues", cleanImpactNote),
		"listing a reviewed one": {Name: result.ImpactSkill, Verdict: "no_blocking_issues", Files: result.Files{Reviewed: []string{"go.mod"}}},
	} {
		t.Run(name, func(t *testing.T) {
			d := Decide(Outcome{Result: completed(skipped, impact)}, config.FailureStatusFailed)
			if d.Approve || d.Kind != KindNotApproved || d.Description != descNothingToReview {
				t.Fatalf("decision = %+v; want nothing reviewed, not approved", d)
			}
		})
	}
}

// With a file reviewed by a skill, a clean impact check is still approved.
func TestCleanImpactApproves(t *testing.T) {
	r := completed(skill("security", "no_blocking_issues", "a.go"), impactSkill("no_blocking_issues", cleanImpactNote))
	if d := Decide(Outcome{Result: r}, config.FailureStatusFailed); !d.Approve || d.Kind != KindApprove {
		t.Fatalf("decision = %+v; want an approval", d)
	}
}

func TestFailedImpactWithholdsApproval(t *testing.T) {
	failed := impactSkill("failed")
	failed.Failures = []result.Failure{{Message: result.ImpactNotRunMessage}}
	r := completed(skill("security", "no_blocking_issues", "a.go"), failed)

	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Approve || d.Kind != KindFailed || d.Status != StatusFailed || d.Description != "Review failed for 1 of 2 skills" {
		t.Fatalf("decision = %+v; want a failed review that is not approved", d)
	}
	if !strings.Contains(d.Comment, "linked repositories") {
		t.Errorf("the comment doesn't say which repositories weren't covered:\n%s", d.Comment)
	}
	// An impact verdict this code doesn't know is a failure too.
	if d := Decide(Outcome{Result: completed(skill("security", "no_blocking_issues", "a.go"), impactSkill("maybe"))}, config.FailureStatusFailed); d.Approve {
		t.Errorf("an unknown impact verdict approved: %+v", d)
	}
}

func TestBlockingImpactBlocks(t *testing.T) {
	b := withFinding(impactSkill("has_blocking_issues"), "blocking", "abc123def456", "calls it with the old signature")
	r := completed(skill("security", "no_blocking_issues", "a.go"), b)
	if d := Decide(Outcome{Result: r}, config.FailureStatusFailed); d.Kind != KindBlocking || d.Approve {
		t.Fatalf("decision = %+v; want blocking", d)
	}
}

// The clean run's note reaches the comment through the skill's notes.
func TestCleanImpactSaysSo(t *testing.T) {
	r := completed(skill("security", "no_blocking_issues", "a.go"), impactSkill("no_blocking_issues", cleanImpactNote))
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if !strings.Contains(d.Comment, cleanImpactNote) {
		t.Fatalf("the comment lacks %q:\n%s", cleanImpactNote, d.Comment)
	}
	if n := strings.Count(d.Comment, "Impact: checked"); n != 1 {
		t.Errorf("the note appears %d times, want once:\n%s", n, d.Comment)
	}
}

// A review that did not run the check says nothing about it.
func TestNoImpactRunSaysNothing(t *testing.T) {
	r := completed(skill("security", "no_blocking_issues", "a.go"))
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if strings.Contains(strings.ToLower(d.Comment), "impact") {
		t.Errorf("the comment of a review with no impact check mentions it:\n%s", d.Comment)
	}
}

// A quiet review keeps the suggestions of the impact check in the comment,
// like any skill's.
func TestQuietKeepsImpactSuggestionsInTheComment(t *testing.T) {
	s := withFinding(impactSkill("no_blocking_issues"), "suggestion", "abc123def456", "a caller might break")
	r := completed(skill("security", "no_blocking_issues", "a.go"), s)
	var title string
	var f result.Finding
	_ = json.Unmarshal(s.Findings[0], &f)
	title = f.Title

	d := Decide(Outcome{Result: r, Quiet: true}, config.FailureStatusFailed)
	if !strings.Contains(d.Comment, title) || !strings.Contains(d.Comment, "a caller might break") {
		t.Errorf("a quiet comment lacks the impact suggestion:\n%s", d.Comment)
	}
}
