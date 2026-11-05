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

// pastDismissals is the JSON of what people rejected, for the skills in, and
// how many entries each skill has; nil when there is none. A failure only
// logs: the review runs without it.
func (d *dispatcher) pastDismissals(ctx context.Context, repo settings.Effective, in *inputs.Inputs, authorID int64) ([]byte, map[string]int) {
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
	skills := make([]string, 0, len(in.Skills))
	for name := range in.Skills {
		skills = append(skills, name)
	}
	slices.Sort(skills)
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
