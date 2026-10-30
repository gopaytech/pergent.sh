// Package policy decides what pergent.sh does with a review outcome: the
// comment to post, the commit status to set and whether the approve bot
// approves. It is a pure function of the outcome and the failure setting.
//
// The rule that matters: a failed, partial or missing review is never
// "success" and never approved. Anything unrecognised counts as a failure.
package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/result"
)

// Marker identifies pergent's one comment on an MR, so it is updated in place.
const Marker = "<!-- pergent -->"

// Status is the commit status state.
type Status string

const (
	StatusSuccess Status = "success"
	StatusFailed  Status = "failed"
	StatusNeutral Status = "neutral"
)

const (
	descCouldNotReview  = "pergent couldn't review this MR"
	descBlocking        = "Blocking issues found"
	descNothingToReview = "Nothing to review: every changed file is skipped"
	descClean           = "No blocking issues"

	maxListedSkipped = 20
)

// Outcome is what a run produced. FailureReason is set when no usable result
// arrived (the Job died, the result did not parse, the run was cancelled...).
type Outcome struct {
	Result        *result.Result
	FailureReason string
}

// Decision is what to do with an outcome.
type Decision struct {
	Comment     string
	Status      Status
	Description string
	Approve     bool
}

// Decide applies the approval policy. failure is the repository's
// failure_status; it picks the status of every "could not review" row.
func Decide(o Outcome, failure config.FailureStatus) Decision {
	failed := failureStatus(failure)

	if o.FailureReason != "" {
		return Decision{Comment: withMarker(ErrorComment(o.FailureReason)), Status: failed, Description: descCouldNotReview}
	}
	r := o.Result
	if r == nil {
		return Decision{Comment: ErrorComment("no result was received"), Status: failed, Description: descCouldNotReview}
	}
	switch r.Status {
	case "error":
		comment := r.Comment.Markdown
		if comment == "" {
			reason := "the review failed"
			if r.Error != nil && r.Error.Message != "" {
				reason = r.Error.Message
			}
			comment = ErrorComment(reason)
		}
		return Decision{Comment: withMarker(comment), Status: failed, Description: descCouldNotReview}
	case "completed":
	default:
		return Decision{Comment: ErrorComment(fmt.Sprintf("the result has an unknown status %q", r.Status)), Status: failed, Description: descCouldNotReview}
	}
	if len(r.Skills) == 0 {
		return Decision{Comment: ErrorComment("the result has no skills"), Status: failed, Description: descCouldNotReview}
	}

	comment := withMarker(r.Comment.Markdown)
	var blocking, failedSkills, reviewed int
	for _, s := range r.Skills {
		switch s.Verdict {
		case "has_blocking_issues":
			blocking++
		case "no_blocking_issues":
			// A clean verdict that also lists files it could not review is a
			// contradiction; fail closed.
			if len(s.Files.NotReviewed) > 0 {
				failedSkills++
			}
		default: // "failed" and anything unknown
			failedSkills++
		}
		reviewed += len(s.Files.Reviewed)
	}

	switch {
	case blocking > 0:
		return Decision{Comment: comment, Status: StatusFailed, Description: descBlocking}
	case failedSkills > 0:
		return Decision{
			Comment:     comment,
			Status:      failed,
			Description: fmt.Sprintf("Review failed for %d of %d skills", failedSkills, len(r.Skills)),
		}
	case reviewed == 0:
		return Decision{
			Comment:     withNote(comment, r.Comment.FindingsBlock, nothingReviewedNote(r.Skills)),
			Status:      StatusSuccess,
			Description: descNothingToReview,
		}
	}
	return Decision{Comment: comment, Status: StatusSuccess, Description: descClean, Approve: true}
}

// ErrorComment is the comment for a review that could not run.
func ErrorComment(reason string) string {
	return Marker + "\n⚠️ **pergent couldn't review this MR:** " + reason + "."
}

func failureStatus(f config.FailureStatus) Status {
	if f == config.FailureStatusNeutral {
		return StatusNeutral
	}
	return StatusFailed
}

func withMarker(comment string) string {
	if strings.Contains(comment, Marker) {
		return comment
	}
	return Marker + "\n" + comment
}

// nothingReviewedNote lists the distinct skipped paths across skills, one line
// per path. A path skipped under different rules by different skills shows all
// of its rules, sorted.
func nothingReviewedNote(skills []result.Skill) string {
	rules := map[string]map[string]bool{}
	for _, s := range skills {
		for _, f := range s.Files.Skipped {
			if rules[f.Path] == nil {
				rules[f.Path] = map[string]bool{}
			}
			rules[f.Path][f.Rule] = true
		}
	}
	paths := make([]string, 0, len(rules))
	for p := range rules {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	b.WriteString("---\n**Not approved:** every changed file was skipped by pergent's default rules, so nothing was reviewed. A human needs to look.\n")
	if len(paths) > 0 {
		b.WriteString("\n")
	}
	for i, p := range paths {
		if i == maxListedSkipped {
			fmt.Fprintf(&b, "- …and %d more\n", len(paths)-maxListedSkipped)
			break
		}
		rs := make([]string, 0, len(rules[p]))
		for r := range rules[p] {
			rs = append(rs, r)
		}
		sort.Strings(rs)
		fmt.Fprintf(&b, "- `%s` (%s)\n", p, strings.Join(rs, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// withNote puts note before the hidden findings block when the comment ends
// with it (ignoring trailing newlines on both), and appends it otherwise. The
// block stays the last line, with the comment's own trailing newlines.
func withNote(comment, block, note string) string {
	trimmed := strings.TrimRight(comment, "\n")
	tail := comment[len(trimmed):]
	tb := strings.TrimRight(block, "\n")
	if tb != "" && strings.HasSuffix(trimmed, tb) {
		before := strings.TrimRight(strings.TrimSuffix(trimmed, tb), "\n")
		return before + "\n\n" + note + "\n\n" + tb + tail
	}
	return trimmed + "\n\n" + note + "\n"
}
