package reviewapi

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The values of the v1 result's enums. They are fixed: a new one makes a new
// /v2/, so a caller that gates on the verdict never meets one it doesn't know.
const (
	StatusQueued     = "queued"
	StatusRunning    = "running"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
	StatusSuperseded = "superseded"

	VerdictFailed            = "failed"
	VerdictNothingReviewed   = "nothing_reviewed"
	VerdictHasBlockingIssues = "has_blocking_issues"
	VerdictNoBlockingIssues  = "no_blocking_issues"

	StepStarting = "starting"
	StepReview   = "review"
	StepDelivery = "delivery"
	StepUnknown  = "unknown"

	RuleDeleted          = "deleted"
	RuleBinary           = "binary"
	RuleNoContentChanges = "no_content_changes"
	RuleLockfile         = "lockfile"
	RuleVendored         = "vendored"
	RuleGenerated        = "generated"
	RuleOutOfScope       = "out_of_scope"
	RuleExcludedBySkill  = "excluded_by_skill"
	RuleOther            = "other"
)

// Review is the v1 result of a review: what GET /v1/reviews/{id} returns and
// the callback posts. Verdict, Skills and Usage are set once the review has a
// result; Failure when it failed. A review that reviewed nothing or failed is
// never a pass: only the verdict no_blocking_issues is.
type Review struct {
	ID         string     `json:"id"`
	Subject    string     `json:"subject"`
	Tenant     string     `json:"tenant"`
	Title      string     `json:"title"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Verdict    string     `json:"verdict,omitempty"`
	Skills     []Skill    `json:"skills,omitempty"`
	Failure    *Failure   `json:"failure,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
	Test       bool       `json:"test,omitempty"`
}

// Skill is the outcome of one skill.
type Skill struct {
	Name     string    `json:"name"`
	Verdict  string    `json:"verdict"`
	Findings []Finding `json:"findings"`
	Files    Files     `json:"files"`
}

// Files records what a skill looked at. Its verdict covers Reviewed only.
type Files struct {
	Reviewed    []string      `json:"reviewed"`
	Skipped     []Skipped     `json:"skipped"`
	NotReviewed []NotReviewed `json:"not_reviewed"`
}

// Skipped is a file left out of a review, and the v1 rule that left it out.
type Skipped struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// NotReviewed is a file that should have been reviewed and was not, with
// pergent's reason. Its skill is failed.
type NotReviewed struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Finding is one finding. Dismissed is set when an earlier dismissal on the
// same subject covers it; the finding stays in the list.
type Finding struct {
	ID          string     `json:"id"`
	Fingerprint string     `json:"fingerprint"`
	File        string     `json:"file"`
	Line        int        `json:"line,omitempty"`
	EndLine     int        `json:"end_line,omitempty"`
	Severity    string     `json:"severity"`
	Title       string     `json:"title"`
	Explanation string     `json:"explanation"`
	Fix         string     `json:"fix,omitempty"`
	Suggestion  string     `json:"suggestion,omitempty"`
	Dismissed   *Dismissed `json:"dismissed,omitempty"`
}

// Dismissed is why, and when, a finding was dismissed.
type Dismissed struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Failure is where a review failed (a step) and why. Reason is safe for the
// caller to read.
type Failure struct {
	Step   string `json:"step"`
	Reason string `json:"reason"`
}

// Usage is what the review's skills used.
type Usage struct {
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
}

// DismissalRef is a dismissal on the review's subject. Severity "blocking"
// covers both severities of finding, "suggestion" only suggestions, as
// render.ApplyDismissals has it. Earlier refs win.
type DismissalRef struct {
	Fingerprint, Severity, Reason string
	At                            time.Time
}

// Map turns pergent's agent result into the v1 skills and the review's
// verdict, and sums usage. A finding's severity is suggestion or, for
// anything else pergent sends, blocking. A skill's verdict is pergent's, with
// these changes: a skill pergent doesn't call no_blocking_issues or
// has_blocking_issues, or that lists a file it could not review, is failed;
// one that is not failed and reviewed no file is nothing_reviewed; one whose
// blocking findings are all dismissed is no_blocking_issues; one pergent calls
// no_blocking_issues with a blocking finding left undismissed is
// has_blocking_issues. The review's verdict is the first that applies: failed
// (any skill failed, or the result is no completed one), nothing_reviewed (no
// skill reviewed a file), has_blocking_issues, no_blocking_issues. redact
// (nil: none) cleans every text the model or the checkout wrote.
func Map(res *result.Result, usage []store.SkillUsage, dismissals []DismissalRef, redact func(string) string) (verdict string, skills []Skill, u Usage) {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	for _, s := range usage {
		u.TokensIn += s.In
		u.TokensOut += s.Out
	}
	if res == nil || res.Status != "completed" {
		return VerdictFailed, nil, u
	}

	rds := make([]render.Dismissal, len(dismissals))
	for i, d := range dismissals {
		// By carries the index, so the reason and time of the dismissal that
		// won are found again.
		rds[i] = render.Dismissal{Fingerprint: d.Fingerprint, Severity: d.Severity, By: strconv.Itoa(i)}
	}
	in := result.ToRender(res)
	applied := render.ApplyDismissals(in, rds)

	skills = make([]Skill, len(in))
	for i, rs := range in {
		skills[i] = mapSkill(res.Skills[i], rs, applied[i], dismissals, redact)
	}
	return overall(skills), skills, u
}

func mapSkill(raw result.Skill, in, applied render.Skill, dismissals []DismissalRef, redact func(string) string) Skill {
	s := Skill{
		Name:     raw.Name,
		Findings: []Finding{},
		Files:    Files{Reviewed: []string{}, Skipped: []Skipped{}, NotReviewed: []NotReviewed{}},
	}
	// Both lists keep the order of in.Findings, so one pass pairs them.
	next := 0
	for _, f := range in.Findings {
		out := Finding{
			ID: f.ID, Fingerprint: f.Fingerprint, File: redact(f.File), Line: f.Line, EndLine: f.EndLine,
			Severity: severity(f.Severity), Title: redact(f.Title), Explanation: redact(f.Explanation),
			Fix: redact(f.SuggestedFix), Suggestion: redact(f.Suggestion),
		}
		if next < len(applied.Dismissed) && applied.Dismissed[next].Finding == f {
			if i, err := strconv.Atoi(applied.Dismissed[next].By); err == nil && i >= 0 && i < len(dismissals) {
				out.Dismissed = &Dismissed{Reason: redact(dismissals[i].Reason), At: dismissals[i].At}
			}
			next++
		}
		s.Findings = append(s.Findings, out)
	}
	for _, p := range raw.Files.Reviewed {
		s.Files.Reviewed = append(s.Files.Reviewed, redact(p))
	}
	for _, k := range raw.Files.Skipped {
		s.Files.Skipped = append(s.Files.Skipped, Skipped{Path: redact(k.Path), Rule: skipRule(k.Rule)})
	}
	for _, msg := range raw.Files.NotReviewed {
		var n struct{ Path, Cause string }
		if json.Unmarshal(msg, &n) != nil {
			n.Cause = "pergent didn't say why"
		}
		s.Files.NotReviewed = append(s.Files.NotReviewed, NotReviewed{Path: redact(n.Path), Reason: redact(n.Cause)})
	}

	switch applied.Verdict {
	case VerdictHasBlockingIssues, VerdictNoBlockingIssues:
		s.Verdict = applied.Verdict
	default:
		s.Verdict = VerdictFailed
	}
	if s.Verdict == VerdictNoBlockingIssues && hasBlocking(applied.Findings) {
		s.Verdict = VerdictHasBlockingIssues
	}
	switch {
	case len(raw.Files.NotReviewed) > 0 || len(raw.Failures) > 0:
		s.Verdict = VerdictFailed
	case s.Verdict != VerdictFailed && len(raw.Files.Reviewed) == 0:
		s.Verdict = VerdictNothingReviewed
	}
	return s
}

// severity is the v1 severity of a finding: suggestion, or blocking for
// anything else, so a caller never meets a third value.
func severity(s render.Severity) string {
	if s == render.Suggestion {
		return string(render.Suggestion)
	}
	return string(render.Blocking)
}

// hasBlocking reports whether any of fs is not a suggestion.
func hasBlocking(fs []render.Finding) bool {
	for _, f := range fs {
		if f.Severity != render.Suggestion {
			return true
		}
	}
	return false
}

// overall is the review's verdict from its skills' verdicts.
func overall(skills []Skill) string {
	blocking, reviewed := false, false
	for _, s := range skills {
		switch s.Verdict {
		case VerdictFailed:
			return VerdictFailed
		case VerdictNothingReviewed:
			continue
		case VerdictHasBlockingIssues:
			blocking = true
		}
		reviewed = true
	}
	switch {
	case !reviewed:
		return VerdictNothingReviewed
	case blocking:
		return VerdictHasBlockingIssues
	}
	return VerdictNoBlockingIssues
}

// skipRule is the v1 rule of a rule pergent names.
func skipRule(rule string) string {
	switch rule {
	case "deleted":
		return RuleDeleted
	case "binary":
		return RuleBinary
	case "no content changes":
		return RuleNoContentChanges
	case "lockfile":
		return RuleLockfile
	case "vendored":
		return RuleVendored
	case "generated":
		return RuleGenerated
	case "out of scope":
		return RuleOutOfScope
	case "excluded by skill":
		return RuleExcludedBySkill
	}
	return RuleOther
}

// StatusOf is the v1 status of a review, from its run. It agrees with the SQL
// of store's apiStatusSQL for every state that has a status there. A run that
// waits is still queued; a skipped run, or a state this code doesn't know, is
// failed: that never reads as a review that finished well.
func StatusOf(r store.APIReview) string {
	switch r.Run.State {
	case store.Queued, store.Waiting:
		return StatusQueued
	case store.Running:
		if r.Run.JobName == "" {
			return StatusQueued
		}
		return StatusRunning
	case store.Reported:
		return StatusRunning
	case store.Superseded:
		return StatusSuperseded
	case store.Published:
		if resultStatus(r.Run.Result) == "completed" {
			return StatusCompleted
		}
	}
	return StatusFailed
}

// resultStatus is the status of a stored result, "" when it has none.
func resultStatus(data []byte) string {
	var s struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(data, &s) != nil {
		return ""
	}
	return s.Status
}

// FailureOf is why a failed review failed, nil for any other status. The
// reason is the run's own (safe for everyone to read), or a fixed text.
func FailureOf(r store.APIReview) *Failure {
	if StatusOf(r) != StatusFailed {
		return nil
	}
	if r.Run.State != store.Published {
		return &Failure{Step: StepUnknown, Reason: "pergent.sh couldn't finish the review"}
	}
	f := &Failure{Step: stepOf(r.Run.FailureStep), Reason: r.Run.FailureReason}
	if f.Reason == "" {
		f.Reason = "the review failed"
		// An error result has no failure step: its Job ran, and pergent
		// found no review it could do. Its message is not shown.
		if resultStatus(r.Run.Result) == "error" {
			f.Step, f.Reason = StepReview, "pergent couldn't review the change"
		}
	}
	return f
}

// stepOf is the v1 step of a run's failure step: none and clone (the checkout
// that builds the commits) are starting.
func stepOf(step string) string {
	switch step {
	case "", "clone", "starting":
		return StepStarting
	case "review":
		return StepReview
	case "delivery":
		return StepDelivery
	}
	return StepUnknown
}

// Pending is the body of a review that has no stored response: queued,
// running or superseded, and failed with only a run's failure to show. A
// review with a result is built from Map.
func Pending(r store.APIReview) Review {
	b := Review{
		ID: r.Run.ID, Subject: r.Subject, Tenant: r.Tenant, Title: r.Title,
		Status: StatusOf(r), CreatedAt: r.CreatedAt.UTC(),
	}
	if !r.FinishedAt.IsZero() {
		t := r.FinishedAt.UTC()
		b.FinishedAt = &t
	}
	if f := FailureOf(r); f != nil {
		b.Failure, b.Verdict = f, VerdictFailed
	}
	return b
}
