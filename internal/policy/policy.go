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
	descPartlySkipped   = "No blocking issues; not approved: some changed files weren't reviewed"
	descNotApproved     = "No blocking issues; not approved: "

	reasonDraft              = "draft"
	reasonApproveUnavailable = "the pergent approve App isn't installed on this repository"
	reasonApproveBotNoReach  = "the pergent approve bot can't reach this repository"

	noteNothingReviewed    = "**Not approved:** every changed file was skipped by pergent's default rules, so nothing was reviewed. A human needs to look."
	notePartlySkipped      = "**Not approved:** some changed files were skipped by pergent's default rules, so they weren't reviewed. A human needs to look."
	noteDraft              = "---\n**Not approved:** this is a draft. It will be approved when it's marked ready."
	noteApproveUnavailable = "---\n**Not approved:** the pergent approve App isn't installed on this repository."
	noteApproveBotNoReach  = "---\n**Not approved:** the pergent approve bot can't reach this repository."

	maxListedSkipped = 20
)

// defaultSkipRules are the skip rules a change decides on its own (pergent's
// default exclusions). A file skipped under one of them was never reviewed,
// so a review that has one is not approved. "out of scope", "excluded by
// skill" and "no content changes" follow the skill's own scope or leave
// nothing to review, and don't block approval.
var defaultSkipRules = map[string]bool{
	"deleted": true, "binary": true, "vendored": true, "lockfile": true, "generated": true,
}

// Outcome is what a run produced. FailureReason is set when no usable result
// arrived (the Job died, the result did not parse, the run was cancelled...).
//
// Draft and ApproveUnavailable only matter to a review that would otherwise
// be approved: it still gets its success status, but no approval. Draft is
// the change being a draft (it is approved once marked ready);
// ApproveUnavailable is the approve side unable to reach the repository (the
// approve App isn't installed on it). Draft wins when both are set.
// ApproveBot says the approve side is a bot user's token (GitHub's
// approve_token_env) rather than an App, which only changes how an
// unavailable approve side is worded.
type Outcome struct {
	Result             *result.Result
	FailureReason      string
	Draft              bool
	ApproveUnavailable bool
	ApproveBot         bool
}

// Decision is what to do with an outcome.
type Decision struct {
	Comment     string
	Status      Status
	Description string
	Approve     bool
}

// Decide applies the approval policy. failure is the repository's
// failure_status; it picks the status of every "could not review" row. The
// comment it returns has its quick actions neutralised (NeutralizeQuickActions),
// whatever the path that built it.
func Decide(o Outcome, failure config.FailureStatus) Decision {
	d := decide(o, failure)
	d.Comment = NeutralizeQuickActions(d.Comment)
	return d
}

// NeutralizeQuickActions stops GitLab from running quick actions in a
// comment. GitLab runs a line such as "/approve" or "/close" in a note
// created through the API as a command of the note's author, and the comment
// carries model text a merge request can steer.
//
// It mirrors GitLab's extractor, which deletes every "\r" from the note and
// then runs each line that starts with "/" at column 0. So every "\r" is
// removed first (otherwise "x\n\r/approve" would still run), then every line
// that starts with "/" gets a zero-width space (U+200B) before the slash,
// inside code fences too, so it renders the same and runs nothing. Indented
// lines never run and are left byte-identical (code in a suggested fix stays
// as written). The marker and the findings block start with "<" and are left
// alone.
func NeutralizeQuickActions(comment string) string {
	comment = strings.ReplaceAll(comment, "\r", "")
	if !strings.Contains(comment, "/") {
		return comment
	}
	lines := strings.SplitAfter(comment, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "/") {
			lines[i] = "\u200b" + line
		}
	}
	return strings.Join(lines, "")
}

func decide(o Outcome, failure config.FailureStatus) Decision {
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
			Comment:     withNote(comment, r.Comment.FindingsBlock, skippedNote(noteNothingReviewed, r.Skills, false)),
			Status:      StatusSuccess,
			Description: descNothingToReview,
		}
	case defaultSkipped(r.Skills):
		return Decision{
			Comment:     withNote(comment, r.Comment.FindingsBlock, skippedNote(notePartlySkipped, r.Skills, true)),
			Status:      StatusSuccess,
			Description: descPartlySkipped,
		}
	}
	switch {
	case o.Draft:
		return notApproved(comment, r.Comment.FindingsBlock, reasonDraft, noteDraft)
	case o.ApproveUnavailable && o.ApproveBot:
		return notApproved(comment, r.Comment.FindingsBlock, reasonApproveBotNoReach, noteApproveBotNoReach)
	case o.ApproveUnavailable:
		return notApproved(comment, r.Comment.FindingsBlock, reasonApproveUnavailable, noteApproveUnavailable)
	}
	return Decision{Comment: comment, Status: StatusSuccess, Description: descClean, Approve: true}
}

// notApproved is a clean review that isn't approved for reason: success, the
// note before the findings block.
func notApproved(comment, block, reason, note string) Decision {
	return Decision{
		Comment:     withNote(comment, block, note),
		Status:      StatusSuccess,
		Description: descNotApproved + reason,
	}
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

// defaultSkipped reports whether any skill skipped a file under a default rule.
func defaultSkipped(skills []result.Skill) bool {
	for _, s := range skills {
		for _, f := range s.Files.Skipped {
			if defaultSkipRules[f.Rule] {
				return true
			}
		}
	}
	return false
}

// skippedNote is title followed by the distinct skipped paths across skills,
// one line per path, at most 20. A path skipped under different rules by
// different skills shows all of its rules, sorted. With onlyDefault, only the
// paths that some skill skipped under a default rule are listed.
func skippedNote(title string, skills []result.Skill, onlyDefault bool) string {
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
	for p, rs := range rules {
		if onlyDefault && !anyDefault(rs) {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	b.WriteString("---\n" + title + "\n")
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

func anyDefault(rules map[string]bool) bool {
	for r := range rules {
		if defaultSkipRules[r] {
			return true
		}
	}
	return false
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
