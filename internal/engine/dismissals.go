package engine

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/gopaytech/pergent.sh/internal/actions"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/settings"
)

// dismissalSkills are the skills a review is told past dismissals for: those
// it runs, sorted, and the linked repositories' check when the run has one.
// The check is no skill of the library, so it is not in in.Skills.
func dismissalSkills(in *inputs.Inputs, withImpact bool) []string {
	skills := make([]string, 0, len(in.Skills)+1)
	for name := range in.Skills {
		skills = append(skills, name)
	}
	if withImpact && !slices.Contains(skills, impactSkill) {
		skills = append(skills, impactSkill)
	}
	slices.Sort(skills)
	return skills
}

// mayLink is whether this run may have linked repositories, judged before the
// plan is made (the plan needs the inputs this step helps size): the setting
// is on, there are links, the platform is GitHub and the run is not a re-run
// of other skills. The plan can still match nothing.
func (d *dispatcher) mayLink(plat Platform, repo settings.Effective) bool {
	if !repo.LinkedReposEnabled || len(repo.LinkedRepos) == 0 || !plat.isGitHub() {
		return false
	}
	return !d.partial || d.rerunImpact
}

// pastDismissals is the JSON of what people rejected, for the skills in (and
// the linked repositories' check when withImpact), and how many entries each
// skill has; nil when there is none. A failure only logs: the review runs
// without it.
func (d *dispatcher) pastDismissals(ctx context.Context, repo settings.Effective, in *inputs.Inputs, authorID int64, withImpact bool) ([]byte, map[string]int) {
	if !repo.LearnFromDismissals {
		return nil, nil
	}
	run := d.run
	since := d.e.now().Add(-actions.PastDismissalDays * 24 * time.Hour)
	rows, err := d.e.Store.TeachingDismissals(ctx, run.MR.Instance, run.MR.ProjectID, since)
	if err != nil {
		d.log.Warn("reviewing without past dismissals: they can't be read", "error", err)
		return nil, nil
	}
	skills := dismissalSkills(in, withImpact)
	// Both redactions: the engine's removes the LLM keys, the dispatcher's
	// whatever secrets the run holds by now. That is the clone token on GitLab;
	// on GitHub it is minted later, and the result token is minted later on
	// both, so neither is removed here.
	past := actions.PastDismissals(rows, skills, authorID, func(s string) string { return d.redact(d.e.redact(s)) })
	if len(past) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(past)
	if err != nil {
		d.log.Warn("reviewing without past dismissals: they can't be encoded", "error", err)
		return nil, nil
	}
	counts := map[string]int{}
	for name, entries := range past {
		counts[name] = len(entries)
	}
	return b, counts
}
