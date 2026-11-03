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
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gopaytech/pergent/render"

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
	reasonApproveUnavailable = "the pergent approve App isn't installed on this repository or lacks Pull requests: write"
	reasonReviewAppNoWrite   = "the pergent review App lacks Pull requests: write on this repository"
	reasonApproveBotNoReach  = "the pergent approve bot can't reach this repository"
	reasonApprovalOff        = "approval is turned off"

	noteNothingReviewed    = "**Not approved:** every changed file was skipped by pergent's default rules, so nothing was reviewed. A human needs to look."
	notePartlySkipped      = "**Not approved:** some changed files were skipped by pergent's default rules, so they weren't reviewed. A human needs to look."
	noteDraft              = "---\n**Not approved:** this is a draft. It will be approved when it's marked ready."
	noteApproveUnavailable = "---\n**Not approved:** the pergent approve App isn't installed on this repository or lacks Pull requests: write."
	noteReviewAppNoWrite   = "---\n**Not approved:** the pergent review App lacks Pull requests: write on this repository."
	noteApproveBotNoReach  = "---\n**Not approved:** the pergent approve bot can't reach this repository."
	noteApprovalOff        = "---\n**Not approved:** approval is turned off for this repository."
	noteMerge              = "---\n**Note:** "

	// The skipped-files note lists at most maxListedSkipped paths, each cut
	// to maxListedPath characters with at most maxListedRules rules of
	// maxListedRule characters, in at most maxNoteBytes: a GitHub comment
	// can't be over 65,536 characters, and pergent's part of it is held to
	// 60,000 bytes (kube.CommentLimitGitHub).
	maxListedSkipped = 20
	maxListedPath    = 120
	maxListedRules   = 3
	maxListedRule    = 40
	maxNoteBytes     = 4000
)

// reviewedSkipRules are the skip rules that don't block approval: "out of
// scope" and "excluded by skill" follow the skill's own scope, and "no
// content changes" leaves nothing to review (a rename or a mode change). Any
// other rule blocks it: pergent's default exclusions ("deleted", "binary",
// "vendored", "lockfile", "generated") are decided by the change itself, and a
// rule pergent.sh doesn't know (one added to pergent later, say) is a file
// nobody reviewed.
var reviewedSkipRules = map[string]bool{
	"out of scope": true, "excluded by skill": true, "no content changes": true,
}

// blocksApproval reports whether a file skipped under rule keeps a review
// from being approved.
func blocksApproval(rule string) bool { return !reviewedSkipRules[rule] }

// Outcome is what a run produced. FailureReason is set when no usable result
// arrived (the Job died, the result did not parse, the run was cancelled...).
//
// ApprovalOff, Draft and ApproveUnavailable only matter to a review that
// would otherwise be approved: it still gets its success status, but no
// approval. ApprovalOff is the repository's "approve clean reviews" turned
// off; Draft is the change being a draft (it is approved once marked ready);
// ApproveUnavailable is the approve side unable to reach the repository (the
// approve App isn't installed on it). ApprovalOff wins over the other two (no
// approval comes while it stays off), and Draft over ApproveUnavailable.
// ApproveBot says the approve side is a bot user's token (GitHub's
// approve_token_env) rather than an App, and ApproveReviewApp that it is the
// review App itself (approve_with_review_app); they only change how an
// unavailable approve side is worded.
//
// Dismissals are the findings of the merge request that people set aside.
// Applied before the verdicts are counted, a skill whose blocking findings
// are all dismissed counts as clean. The comment of a completed review is
// rendered from the result (render.Comment, at most MaxComment bytes when
// positive, passed through Redact when set), never the one pergent wrote: it
// needs the dismissals, the commands and the links, and a merged result's
// comment (see result.Merge) covers the re-run skills only. An error
// result's comment is used as it is.
//
// Mention is the bot's name as a comment mentions it, without the "@"; when
// set, each finding carries its dismiss command and the footer the re-run
// command. FileURL links a finding's location to the reviewed commit; nil
// leaves it plain text.
type Outcome struct {
	Result             *result.Result
	FailureReason      string
	Draft              bool
	ApproveUnavailable bool
	ApproveBot         bool
	ApproveReviewApp   bool
	ApprovalOff        bool

	Dismissals []render.Dismissal
	MaxComment int
	Redact     func(string) string
	Mention    string
	FileURL    func(path string, line, endLine int) string
}

// Kind is what a decision is, in short.
type Kind string

const (
	// KindFailed is no usable review: no result, an error result, an
	// unknown status or a skill that failed.
	KindFailed Kind = "failed"
	// KindBlocking is a review with blocking findings.
	KindBlocking Kind = "blocking"
	// KindNotApproved is a clean review that isn't approved: files were
	// skipped, approval is turned off or the approve side can't reach the
	// repository.
	KindNotApproved Kind = "not_approved"
	// KindDraft is a clean review of a draft, approved once it is ready.
	KindDraft Kind = "draft"
	// KindApprove is a clean review to approve.
	KindApprove Kind = "approve"
)

// Decision is what to do with an outcome.
type Decision struct {
	Comment     string
	Status      Status
	Description string
	Approve     bool
	Kind        Kind
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
		return Decision{Comment: withMarker(ErrorComment(o.FailureReason)), Status: failed, Description: descCouldNotReview, Kind: KindFailed}
	}
	r := o.Result
	if r == nil {
		return Decision{Comment: ErrorComment("no result was received"), Status: failed, Description: descCouldNotReview, Kind: KindFailed}
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
		return Decision{Comment: withMarker(comment), Status: failed, Description: descCouldNotReview, Kind: KindFailed}
	case "completed":
	default:
		return Decision{Comment: ErrorComment(fmt.Sprintf("the result has an unknown status %q", r.Status)), Status: failed, Description: descCouldNotReview, Kind: KindFailed}
	}
	if len(r.Skills) == 0 {
		return Decision{Comment: ErrorComment("the result has no skills"), Status: failed, Description: descCouldNotReview, Kind: KindFailed}
	}

	skills := r.Skills
	rs, dismissed := applyDismissals(o, r)
	if dismissed {
		skills = withVerdicts(r.Skills, rs)
	} else {
		rs = result.ToRender(r)
	}
	redact := o.Redact
	if redact == nil {
		redact = func(s string) string { return s }
	}
	// The text is redacted before it is rendered, so the encoded findings
	// block and a cut comment can't hide a credential from the pass over the
	// finished comment.
	rs = redactSkills(rs, redact)
	comment := withMarker(redact(render.Comment(rs, renderOptions(o, r, redactAll(result.ChangedFiles(r), redact)))))
	block := render.FindingsBlock(rs)
	if r.MergeNote != "" {
		comment = withNote(comment, block, noteMerge+r.MergeNote+", so this review covers only the skills that were re-run and is not approved.")
	}
	var blocking, failedSkills, reviewed int
	for _, s := range skills {
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
		return Decision{Comment: comment, Status: StatusFailed, Description: descBlocking, Kind: KindBlocking}
	case failedSkills > 0:
		return Decision{
			Comment:     comment,
			Status:      failed,
			Description: fmt.Sprintf("Review failed for %d of %d skills", failedSkills, len(skills)),
			Kind:        KindFailed,
		}
	case reviewed == 0:
		return Decision{
			Comment:     withNote(comment, block, skippedNote(noteNothingReviewed, skills, false)),
			Status:      StatusSuccess,
			Description: descNothingToReview,
			Kind:        KindNotApproved,
		}
	case blockingSkip(skills):
		return Decision{
			Comment:     withNote(comment, block, skippedNote(notePartlySkipped, skills, true)),
			Status:      StatusSuccess,
			Description: descPartlySkipped,
			Kind:        KindNotApproved,
		}
	}
	switch {
	case r.MergeNote != "":
		// A re-run kept alone: the skills it did not review are unknown, so
		// the change is not approved on it. The comment carries the note.
		return Decision{
			Comment:     comment,
			Status:      StatusSuccess,
			Description: descNotApproved + r.MergeNote,
			Kind:        KindNotApproved,
		}
	case o.ApprovalOff:
		return notApproved(comment, block, reasonApprovalOff, noteApprovalOff)
	case o.Draft:
		d := notApproved(comment, block, reasonDraft, noteDraft)
		d.Kind = KindDraft
		return d
	case o.ApproveUnavailable && o.ApproveReviewApp:
		return notApproved(comment, block, reasonReviewAppNoWrite, noteReviewAppNoWrite)
	case o.ApproveUnavailable && o.ApproveBot:
		return notApproved(comment, block, reasonApproveBotNoReach, noteApproveBotNoReach)
	case o.ApproveUnavailable:
		return notApproved(comment, block, reasonApproveUnavailable, noteApproveUnavailable)
	}
	return Decision{Comment: comment, Status: StatusSuccess, Description: descClean, Approve: true, Kind: KindApprove}
}

// renderOptions is how the comment of r is rendered: the size limit, the
// changed files, the reviewed commit and the pergent version that reviewed
// it, the links and, with a mention, the dismiss and re-run commands.
func renderOptions(o Outcome, r *result.Result, changed []string) render.Options {
	opts := render.Options{MaxLen: o.MaxComment, ChangedFiles: changed, Commit: r.Head, FileURL: o.FileURL}
	if r.PergentVersion != "" {
		opts.Version = "v" + strings.TrimPrefix(r.PergentVersion, "v")
	}
	if o.Mention != "" {
		opts.DismissCommand = "@" + o.Mention + " dismiss"
		opts.Footer = "Run again: `@" + o.Mention + " review`"
	}
	return opts
}

// applyDismissals applies the outcome's dismissals to the result's skills. It
// returns the skills with the findings set aside moved to Dismissed, and
// whether any finding was.
func applyDismissals(o Outcome, r *result.Result) ([]render.Skill, bool) {
	if len(o.Dismissals) == 0 {
		return nil, false
	}
	rs := render.ApplyDismissals(result.ToRender(r), o.Dismissals)
	for _, s := range rs {
		if len(s.Dismissed) > 0 {
			return rs, true
		}
	}
	return nil, false
}

// redactSkills is skills with redact applied to every text in them that came
// from the review: findings (dismissed ones included), failures, notes,
// summaries, checks, the confidence reason and file paths. skills is not
// modified.
func redactSkills(skills []render.Skill, redact func(string) string) []render.Skill {
	finding := func(f render.Finding) render.Finding {
		f.File, f.Title = redact(f.File), redact(f.Title)
		f.Explanation, f.SuggestedFix = redact(f.Explanation), redact(f.SuggestedFix)
		f.Suggestion = redact(f.Suggestion)
		return f
	}
	out := make([]render.Skill, len(skills))
	for i, s := range skills {
		s.Findings = nil
		for _, f := range skills[i].Findings {
			s.Findings = append(s.Findings, finding(f))
		}
		s.Dismissed = nil
		for _, d := range skills[i].Dismissed {
			s.Dismissed = append(s.Dismissed, render.DismissedFinding{Finding: finding(d.Finding), By: redact(d.By), Reason: redact(d.Reason)})
		}
		s.Failures = nil
		for _, f := range skills[i].Failures {
			s.Failures = append(s.Failures, render.Failure{Files: redactAll(f.Files, redact), Message: redact(f.Message), Lead: redact(f.Lead)})
		}
		s.Skipped = nil
		for _, f := range skills[i].Skipped {
			s.Skipped = append(s.Skipped, render.Skipped{Path: redact(f.Path), Rule: redact(f.Rule)})
		}
		s.Summaries = nil
		for _, sum := range skills[i].Summaries {
			s.Summaries = append(s.Summaries, render.Summary{Batch: sum.Batch, Text: redact(sum.Text)})
		}
		s.Checks = nil
		for _, bc := range skills[i].Checks {
			rb := bc
			rb.Checks = nil
			for _, c := range bc.Checks {
				rb.Checks = append(rb.Checks, render.Check{Section: redact(c.Section), Name: redact(c.Name), Status: c.Status, Detail: redact(c.Detail)})
			}
			s.Checks = append(s.Checks, rb)
		}
		s.Reviewed = redactAll(skills[i].Reviewed, redact)
		s.Notes = redactAll(skills[i].Notes, redact)
		s.ConfidenceReason = redact(skills[i].ConfidenceReason)
		out[i] = s
	}
	return out
}

func redactAll(ss []string, redact func(string) string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = redact(s)
	}
	return out
}

// withVerdicts is skills with the verdicts of the dismissed-applied skills rs
// (the same skills, in the same order).
func withVerdicts(skills []result.Skill, rs []render.Skill) []result.Skill {
	out := make([]result.Skill, len(skills))
	copy(out, skills)
	for i := range out {
		out[i].Verdict = rs[i].Verdict
	}
	return out
}

// notApproved is a clean review that isn't approved for reason: success, the
// note before the findings block.
func notApproved(comment, block, reason, note string) Decision {
	return Decision{
		Comment:     withNote(comment, block, note),
		Status:      StatusSuccess,
		Description: descNotApproved + reason,
		Kind:        KindNotApproved,
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

// blockingSkip reports whether any skill skipped a file under a rule that
// blocks approval.
func blockingSkip(skills []result.Skill) bool {
	for _, s := range skills {
		for _, f := range s.Files.Skipped {
			if blocksApproval(f.Rule) {
				return true
			}
		}
	}
	return false
}

// skippedNote is title followed by the distinct skipped paths across skills,
// one line per path: at most 20 lines and maxNoteBytes in all, then "…and N
// more" for the rest. A path skipped under different rules by different
// skills shows its rules sorted (the first three, then "…"). With
// onlyBlocking, only the paths that some skill skipped under a rule that
// blocks approval are listed.
//
// Paths come from the merge request, so each is shown through listedText: a
// backtick, newline or control character in one can't end its code span or
// line, and a long one is cut.
func skippedNote(title string, skills []result.Skill, onlyBlocking bool) string {
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
		if onlyBlocking && !anyBlocking(rs) {
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
		more := fmt.Sprintf("- …and %d more\n", len(paths)-i)
		if i == maxListedSkipped {
			b.WriteString(more)
			break
		}
		rs := make([]string, 0, len(rules[p]))
		for r := range rules[p] {
			rs = append(rs, r)
		}
		sort.Strings(rs)
		shown := make([]string, 0, maxListedRules+1)
		for j, r := range rs {
			if j == maxListedRules {
				shown = append(shown, "…")
				break
			}
			shown = append(shown, ruleText(r))
		}
		line := "- `" + listedText(p, maxListedPath) + "` (" + strings.Join(shown, ", ") + ")\n"
		// Room is kept for the "and N more" line after this one.
		if b.Len()+len(line)+len(fmt.Sprintf("- …and %d more\n", len(paths))) > maxNoteBytes {
			b.WriteString(more)
			break
		}
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// listedText is s as it is shown inside a code span of the note: as it is
// when it is printable text without a backtick, otherwise Go-quoted with each
// backtick written \x60, so nothing in it can end the span or the line. It is
// cut to n characters.
func listedText(s string, n int) string {
	if utf8.ValidString(s) && !strings.ContainsRune(s, '`') && strings.IndexFunc(s, notPrintable) < 0 {
		return cut(s, n)
	}
	return cut(strings.ReplaceAll(strconv.Quote(s), "`", `\x60`), n)
}

// ruleText is a skip rule as the note shows it: plain when it is lower-case
// words like pergent's own rules, otherwise a code span of listedText.
func ruleText(rule string) string {
	plain := rule != "" && strings.IndexFunc(rule, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r == ' ' || r == '-' || r == '_')
	}) < 0
	if plain {
		return cut(rule, maxListedRule)
	}
	return "`" + listedText(rule, maxListedRule) + "`"
}

func notPrintable(r rune) bool { return !unicode.IsPrint(r) }

// cut shortens s to n characters, marking a cut with "…".
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func anyBlocking(rules map[string]bool) bool {
	for r := range rules {
		if blocksApproval(r) {
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
