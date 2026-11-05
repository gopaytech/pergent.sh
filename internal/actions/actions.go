// Package actions holds the rules that comment commands (the engine) and the
// dashboard's buttons share: which skills "re-run failed" runs again, the
// re-run caps, which finding a dismissal means and who counts as the author
// of a change. Each rule lives here once, so a command and a button can't
// disagree.
package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Term is what the platform calls a change: "PR" on GitHub, else "MR".
func Term(platform string) string {
	if platform == config.PlatformGitHub {
		return "PR"
	}
	return "MR"
}

// CapMessage is what is said when a re-run cap is reached; term is Term's.
func CapMessage(term string) string {
	return fmt.Sprintf("re-run limit reached for this %s; it resets at 00:00 UTC", term)
}

// CapReached tells whether the re-run caps of the change mr or of its
// repository are used up for the UTC day that now falls in. Command and
// button re-runs count alike.
func CapReached(ctx context.Context, st *store.Store, now time.Time, mr store.MRKey, repo settings.Effective) (bool, error) {
	now = now.UTC()
	since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	n, err := st.CountReruns(ctx, &mr, "", 0, since)
	if err != nil {
		return false, fmt.Errorf("count the re-runs of the change: %w", err)
	}
	if n >= repo.RerunLimitPerMR {
		return true, nil
	}
	n, err = st.CountReruns(ctx, nil, mr.Instance, mr.ProjectID, since)
	if err != nil {
		return false, fmt.Errorf("count the re-runs of the repository: %w", err)
	}
	return n >= repo.RerunLimitPerRepo, nil
}

// RerunStatus is how a request to re-run a review ended.
type RerunStatus int

const (
	// RerunQueued: a run was queued.
	RerunQueued RerunStatus = iota
	// RerunAlreadyQueued: the head is queued or running already; nothing
	// was changed.
	RerunAlreadyQueued
	// RerunNothingFailed: "failed only" found no skill to run again.
	RerunNothingFailed
	// RerunCapped: a re-run cap is reached; nothing was queued.
	RerunCapped
)

// RerunRequest asks for the review of the change mr at Head and TargetBranch
// to be run again. ProjectPath is the repository's path as the platform
// spells it. Repo holds the caps and Now is the clock they count by.
type RerunRequest struct {
	MR                 store.MRKey
	ProjectPath        string
	Head, TargetBranch string
	FailedOnly         bool
	Repo               settings.Effective
	Now                time.Time
}

// RerunResult is what Rerun did. Action and Detail are what the audit log
// records when Status is RerunQueued. Whole is set when "failed only" had no
// skill to single out and the whole review was queued.
type RerunResult struct {
	Status         RerunStatus
	Whole          bool
	Action, Detail string
}

// Rerun is the one sequence a comment command and a button follow: for "failed
// only", the skills to run again (nothing failed ends it; no way to single
// them out re-runs the whole review), then the caps, then queueing the run.
// The caller checks permissions, drafts and the change's state first, and
// wakes the engine and audits when the result is RerunQueued.
func Rerun(ctx context.Context, st *store.Store, req RerunRequest) (RerunResult, error) {
	res := RerunResult{Action: "review_rerun", Detail: "skills=all"}
	trigger := store.TriggerRerun
	var (
		skills []string
		base   *string
	)
	if req.FailedOnly {
		res.Action = "review_rerun_failed"
		var err error
		if skills, base, res.Whole, err = FailedSkills(ctx, st, req.MR, req.Head, req.TargetBranch); err != nil {
			return RerunResult{}, err
		}
		switch {
		case res.Whole:
		case len(skills) == 0:
			return RerunResult{Status: RerunNothingFailed}, nil
		default:
			trigger, res.Detail = store.TriggerRerunFailed, "skills="+strings.Join(skills, ",")
		}
	}

	capped, err := CapReached(ctx, st, req.Now, req.MR, req.Repo)
	if err != nil {
		return RerunResult{}, err
	}
	if capped {
		return RerunResult{Status: RerunCapped}, nil
	}
	_, created, err := st.EnqueueRerun(ctx, req.MR, req.ProjectPath, req.Head, req.TargetBranch, trigger, skills, base, !req.FailedOnly)
	if err != nil {
		return RerunResult{}, fmt.Errorf("queue the re-run: %w", err)
	}
	if !created {
		return RerunResult{Status: RerunAlreadyQueued}, nil
	}
	res.Status = RerunQueued
	return res, nil
}

// MaxReasonRunes is the longest reason of a dismissal, in characters. A
// comment's reason is cut to it, the dashboard refuses a longer one.
const MaxReasonRunes = 500

// reasonImage matches what could pull an image into the review comment: a
// markdown image (inline or by reference) or an HTML img tag.
var reasonImage = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)|!\[[^\]]*\]\[[^\]]*\]|(?i:<img\b[^>]*>?)`)

// CleanReason is a dismissal's reason as it is stored and shown: without
// images (the review comment would load them for everyone who reads it, a
// way to track readers) and trimmed.
func CleanReason(reason string) string {
	return strings.TrimSpace(reasonImage.ReplaceAllString(reason, ""))
}

// What a review is told of past dismissals, per skill, and how much of each
// text. A title or file name may have been written from repository content.
const (
	maxPastDismissals = 20
	maxPastTitleRunes = 200
)

// PastDismissalDays is how far back a dismissal still teaches a review.
const PastDismissalDays = 90

// PastDismissal is one entry of what people rejected, as a review is given
// it. It names nobody: who rejected a finding is not the model's business.
type PastDismissal struct {
	File     string `json:"file"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Reason   string `json:"reason"`
}

// DedupeDismissals keeps one row per fingerprint, a blocking one over a
// suggestion, then the newest. The result has blocking rows first, then the
// newest first.
func DedupeDismissals(rows []store.Dismissal) []store.Dismissal {
	best := map[string]store.Dismissal{}
	var order []string
	for _, r := range rows {
		cur, seen := best[r.Fingerprint]
		if !seen {
			order = append(order, r.Fingerprint)
		}
		if !seen || outranks(r, cur) {
			best[r.Fingerprint] = r
		}
	}
	out := make([]store.Dismissal, 0, len(order))
	for _, fp := range order {
		out = append(out, best[fp])
	}
	sort.SliceStable(out, func(i, j int) bool { return outranks(out[i], out[j]) })
	return out
}

// TeachingRows is the rows that teach, one per fingerprint: those whose reason
// is empty once cleaned (an old reason that was only an image) are dropped
// first, so one of them can't hide another row of the same finding that has a
// reason, and then DedupeDismissals picks the row. A review is told these and
// the Past dismissals list shows them.
func TeachingRows(rows []store.Dismissal) []store.Dismissal {
	kept := make([]store.Dismissal, 0, len(rows))
	for _, r := range rows {
		if CleanReason(r.Reason) != "" {
			kept = append(kept, r)
		}
	}
	return DedupeDismissals(kept)
}

// outranks tells whether a goes before b: blocking over suggestion, then newer.
func outranks(a, b store.Dismissal) bool {
	if (a.Severity == "blocking") != (b.Severity == "blocking") {
		return a.Severity == "blocking"
	}
	return a.CreatedAt.After(b.CreatedAt)
}

// PastDismissals is what a review of a change by changeAuthorID (0 when
// unknown) is told, for each of skills that has any: at most 20 entries a
// skill, blocking first. A blocking row by the change's author is left out,
// and so is every one with a recorded dismisser when the author is unknown,
// so nobody clears their own blocker by rejecting it on another change first.
// A row from before dismissers were recorded (ID 0) can't be matched and
// stays. The text goes through redact and is cut.
func PastDismissals(rows []store.Dismissal, skills []string, changeAuthorID int64, redact func(string) string) map[string][]PastDismissal {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	bySkill := map[string][]store.Dismissal{}
	for _, name := range skills {
		bySkill[name] = nil
	}
	for _, r := range rows {
		if _, ok := bySkill[r.Skill]; !ok {
			continue
		}
		if r.Severity == "blocking" && r.DismisserPlatformID != 0 && (changeAuthorID == 0 || r.DismisserPlatformID == changeAuthorID) {
			continue
		}
		bySkill[r.Skill] = append(bySkill[r.Skill], r)
	}
	out := map[string][]PastDismissal{}
	for name, rs := range bySkill {
		rs = TeachingRows(rs)
		if len(rs) > maxPastDismissals {
			rs = rs[:maxPastDismissals]
		}
		for _, r := range rs {
			file, title, reason := TeachingText(r, redact)
			out[name] = append(out[name], PastDismissal{File: file, Title: title, Severity: r.Severity, Reason: reason})
		}
	}
	return out
}

// TeachingText is the file, title and reason of a dismissal as a review is told
// them: redacted, the reason cleaned, and each cut (file and title to 200
// characters, the reason to MaxReasonRunes). The Past dismissals list shows the
// same text.
func TeachingText(r store.Dismissal, redact func(string) string) (file, title, reason string) {
	return cutRunes(redact(r.File), maxPastTitleRunes), cutRunes(redact(r.Title), maxPastTitleRunes),
		cutRunes(redact(CleanReason(r.Reason)), MaxReasonRunes)
}

// cutRunes is s cut to n characters.
func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// RenderDismissals is stored dismissals in the form the comment renderer and
// the result's policy take.
func RenderDismissals(stored []store.Dismissal) []render.Dismissal {
	out := make([]render.Dismissal, len(stored))
	for i, d := range stored {
		out[i] = render.Dismissal{Fingerprint: d.Fingerprint, Severity: d.Severity, By: d.Actor, Reason: CleanReason(d.Reason)}
	}
	return out
}

// FailedSkills names the skills of the newest review of the change that a
// "review failed" re-runs, and that review's run, which the re-run keeps the
// other skills' results of. The review counts only when it is for head and
// targetBranch. whole is set when no skill can be singled out (no review of
// this head and target, an error result, or one whose results were not all
// kept), and the whole review is re-run.
func FailedSkills(ctx context.Context, st *store.Store, mr store.MRKey, head, targetBranch string) (skills []string, base *string, whole bool, err error) {
	run, found, err := st.LatestDecidedRun(ctx, mr)
	if err != nil {
		return nil, nil, false, fmt.Errorf("load the latest review: %w", err)
	}
	if !found || run.HeadSHA != head || run.TargetBranch != targetBranch || run.Result == nil {
		return nil, nil, true, nil
	}
	r, perr := result.Parse(run.Result)
	if perr != nil || r.MergeNote != "" {
		// An unreadable result, or a partial one whose other skills were not
		// kept: nothing to build on.
		return nil, nil, true, nil
	}
	stored, err := st.Dismissals(ctx, mr)
	if err != nil {
		return nil, nil, false, fmt.Errorf("load dismissals: %w", err)
	}
	skills, whole = result.FailedSkills(r, RenderDismissals(stored))
	if whole {
		return nil, nil, true, nil
	}
	id := run.ID
	return skills, &id, false, nil
}

// LatestFindings returns every finding with a fingerprint in the newest
// decided review of the change, and the run that review is. ok is false when
// there is no readable review.
func LatestFindings(ctx context.Context, st *store.Store, mr store.MRKey) (findings []result.Finding, run store.Run, ok bool, err error) {
	run, found, err := st.LatestDecidedRun(ctx, mr)
	if err != nil {
		return nil, run, false, fmt.Errorf("load the latest review: %w", err)
	}
	if !found || run.Result == nil {
		return nil, run, false, nil
	}
	r, perr := result.Parse(run.Result)
	if perr != nil {
		return nil, run, false, nil
	}
	return findingsOf(r.Skills), run, true, nil
}

// findingsOf returns every finding with a fingerprint in skills, each
// carrying the name of the skill that reported it.
func findingsOf(skills []result.Skill) (findings []result.Finding) {
	for _, s := range skills {
		for _, raw := range s.Findings {
			var x result.Finding
			if json.Unmarshal(raw, &x) == nil && x.Fingerprint != "" {
				x.Skill = s.Name
				findings = append(findings, x)
			}
		}
	}
	return findings
}

// MostSevere returns the finding with fingerprint, as severe as the most
// severe report of it (two skills may report one finding differently). A
// severity other than "suggestion" is blocking.
func MostSevere(findings []result.Finding, fingerprint string) (f result.Finding, found bool) {
	for _, x := range findings {
		if x.Fingerprint != fingerprint {
			continue
		}
		if !found || (f.Severity == "suggestion" && x.Severity != "suggestion") {
			f, found = x, true
		}
	}
	return f, found
}

// IsAuthor tells whether the person with user ID personID is the author of a
// change, by user ID. When either ID is unknown (0) they can't be told apart
// and the person is taken for the author.
func IsAuthor(personID, authorID int64) bool {
	return personID == 0 || authorID == 0 || personID == authorID
}
