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

// RenderDismissals is stored dismissals in the form the comment renderer and
// the result's policy take.
func RenderDismissals(stored []store.Dismissal) []render.Dismissal {
	out := make([]render.Dismissal, len(stored))
	for i, d := range stored {
		out[i] = render.Dismissal{Fingerprint: d.Fingerprint, Severity: d.Severity, By: d.Actor, Reason: d.Reason}
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
	for _, s := range r.Skills {
		for _, raw := range s.Findings {
			var x result.Finding
			if json.Unmarshal(raw, &x) == nil && x.Fingerprint != "" {
				findings = append(findings, x)
			}
		}
	}
	return findings, run, true, nil
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
