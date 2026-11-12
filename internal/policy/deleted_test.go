package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/result"
)

// goAndDocs is the two-skill result of a change that deletes pkg/old.go and
// edits pkg/new.go: go (paths **/*.go) reviews both and lists the deletion in
// files.deleted, docs (paths docs/**) skips both, the deletion as "deleted"
// because pergent keeps a deletion out of a skill's scope under that rule.
func goAndDocs(goVerdict string) *result.Result {
	return completed(
		result.Skill{Name: "go", Verdict: goVerdict, Files: result.Files{
			Reviewed: []string{"pkg/new.go", "pkg/old.go"}, Deleted: []string{"pkg/old.go"}}},
		result.Skill{Name: "docs", Verdict: "no_blocking_issues", Files: result.Files{
			Reviewed: []string{},
			Skipped:  []result.Skipped{{Path: "pkg/new.go", Rule: "out of scope"}, {Path: "pkg/old.go", Rule: "deleted"}}}})
}

// A deletion one skill reviewed is approved even though another skill's scope
// leaves it out, and the comment doesn't call it skipped.
func TestDeletionOneSkillReviewedApproves(t *testing.T) {
	d := Decide(Outcome{Result: goAndDocs("no_blocking_issues")}, config.FailureStatusFailed)
	if d.Kind != KindApprove || !d.Approve || d.Status != StatusSuccess {
		t.Errorf("decision = %+v, want approve", d)
	}
	if strings.Contains(d.Comment, "Not approved") {
		t.Errorf("the comment says not approved:\n%s", d.Comment)
	}
}

// With the switch off (or an older image) no skill lists files.deleted: every
// skill skips the deletion as "deleted" and it still blocks, as before.
func TestDeletionNoSkillReviewedStillBlocks(t *testing.T) {
	r := completed(
		result.Skill{Name: "go", Verdict: "no_blocking_issues", Files: result.Files{
			Reviewed: []string{"pkg/new.go"}, Skipped: []result.Skipped{{Path: "pkg/old.go", Rule: "deleted"}}}},
		result.Skill{Name: "docs", Verdict: "no_blocking_issues", Files: result.Files{
			Reviewed: []string{},
			Skipped:  []result.Skipped{{Path: "pkg/new.go", Rule: "out of scope"}, {Path: "pkg/old.go", Rule: "deleted"}}}})
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Kind != KindNotApproved || d.Approve || d.Description != descPartlySkipped {
		t.Errorf("decision = %+v, want not approved, partly skipped", d)
	}
	if !strings.Contains(d.Comment, "- `pkg/old.go` (deleted)") {
		t.Errorf("the note doesn't list the deletion:\n%s", d.Comment)
	}
}

// Only the deletion a skill reviewed is let through: another deletion no skill
// lists (binary, empty, read-denied, past the cap) still blocks, and the note
// lists it alone.
func TestUnreviewedDeletionBesideAReviewedOneBlocks(t *testing.T) {
	r := goAndDocs("no_blocking_issues")
	for i := range r.Skills {
		r.Skills[i].Files.Skipped = append(r.Skills[i].Files.Skipped, result.Skipped{Path: "pkg/blob.bin", Rule: "deleted"})
	}
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Kind != KindNotApproved || d.Approve {
		t.Fatalf("decision = %+v, want not approved", d)
	}
	_, note, ok := strings.Cut(d.Comment, notePartlySkipped)
	if !ok {
		t.Fatalf("no skipped note:\n%s", d.Comment)
	}
	if !strings.Contains(note, "- `pkg/blob.bin` (deleted)") {
		t.Errorf("the note doesn't list the unreviewed deletion:\n%s", note)
	}
	if strings.Contains(note, "pkg/old.go") {
		t.Errorf("the note lists the reviewed deletion as skipped:\n%s", note)
	}
}

// A failed skill's files.deleted approves nothing: the review fails.
func TestDeletionOfAFailedSkillIsNotApproved(t *testing.T) {
	d := Decide(Outcome{Result: goAndDocs("failed")}, config.FailureStatusFailed)
	if d.Kind != KindFailed || d.Approve || d.Status == StatusSuccess {
		t.Errorf("decision = %+v, want failed", d)
	}
}

// The linked repositories' check reviews no file of this repository, so its
// files.deleted (if it ever had one) lets no deletion through.
func TestCheckSkillDeletionDoesNotApprove(t *testing.T) {
	r := completed(
		result.Skill{Name: "go", Verdict: "no_blocking_issues", Files: result.Files{
			Reviewed: []string{"pkg/new.go"}, Skipped: []result.Skipped{{Path: "pkg/old.go", Rule: "deleted"}}}},
		result.Skill{Name: result.ImpactSkill, Verdict: "no_blocking_issues", Files: result.Files{
			Reviewed: []string{"pkg/old.go"}, Deleted: []string{"pkg/old.go"}}})
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Kind != KindNotApproved || d.Approve {
		t.Errorf("decision = %+v, want not approved", d)
	}
}

// A skill must list the deletion as reviewed too: files.deleted alone, which
// pergent never writes, is not a review.
func TestDeletedWithoutReviewedDoesNotApprove(t *testing.T) {
	r := goAndDocs("no_blocking_issues")
	r.Skills[0].Files.Reviewed = []string{"pkg/new.go"}
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Kind != KindNotApproved || d.Approve {
		t.Errorf("decision = %+v, want not approved", d)
	}
}

// A deletion reviewed in an earlier push and carried keeps its files.deleted
// mark through the merge, so the change is still approved.
func TestCarriedDeletionOneSkillReviewedApproves(t *testing.T) {
	const from = "1111111111111111111111111111111111111111"
	marshal := func(head string, skills ...result.Skill) []byte {
		b, err := json.Marshal(result.Result{SchemaVersion: 1, Status: "completed", PergentVersion: "0.9.0", Base: "b", Head: head,
			Skills: skills, Comment: result.Comment{Markdown: "c", FindingsBlock: "<!-- pergent:findings [] -->"}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	docs := func(paths ...string) result.Skill {
		s := result.Skill{Name: "docs", Verdict: "no_blocking_issues", Findings: []json.RawMessage{}, Files: result.Files{
			Reviewed: []string{}, Skipped: []result.Skipped{{Path: "pkg/old.go", Rule: "deleted"}}}}
		for _, p := range paths {
			s.Files.Skipped = append(s.Files.Skipped, result.Skipped{Path: p, Rule: "out of scope"})
		}
		return s
	}
	base := marshal(from,
		result.Skill{Name: "go", Verdict: "no_blocking_issues", Batches: 1, Findings: []json.RawMessage{}, Files: result.Files{
			Reviewed: []string{"pkg/new.go", "pkg/old.go"}, Deleted: []string{"pkg/old.go"}}},
		docs("pkg/new.go"))
	partial := marshal("2222222222222222222222222222222222222222",
		result.Skill{Name: "go", Verdict: "no_blocking_issues", Batches: 1, Findings: []json.RawMessage{}, Files: result.Files{
			Reviewed: []string{"pkg/y.go"}, Carried: []string{"pkg/new.go", "pkg/old.go"}}},
		docs("pkg/new.go", "pkg/y.go"))
	out, err := result.MergeIncremental(partial, base, from, false)
	if err != nil {
		t.Fatal(err)
	}
	r, err := result.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	d := Decide(Outcome{Result: r}, config.FailureStatusFailed)
	if d.Kind != KindApprove || !d.Approve {
		t.Errorf("decision = %+v, want approve", d)
	}
}
