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

// pastDismissalDays is how far back a dismissal still teaches a review.
const pastDismissalDays = 90

// pastDismissals is the JSON of what people rejected, for the skills in; nil
// when there is none. A failure only logs: the review runs without it.
func (d *dispatcher) pastDismissals(ctx context.Context, repo settings.Effective, in *inputs.Inputs, authorID int64) []byte {
	if !repo.LearnFromDismissals {
		return nil
	}
	run := d.run
	since := d.e.now().Add(-pastDismissalDays * 24 * time.Hour)
	rows, err := d.e.Store.TeachingDismissals(ctx, run.MR.Instance, run.MR.ProjectID, since)
	if err != nil {
		d.log.Warn("reviewing without past dismissals: they can't be read", "error", err)
		return nil
	}
	skills := make([]string, 0, len(in.Skills))
	for name := range in.Skills {
		skills = append(skills, name)
	}
	slices.Sort(skills)
	past := actions.PastDismissals(rows, skills, authorID, d.e.redact)
	if len(past) == 0 {
		return nil
	}
	b, err := json.Marshal(past)
	if err != nil {
		d.log.Warn("reviewing without past dismissals: they can't be encoded", "error", err)
		return nil
	}
	counts := map[string]int{}
	for name, entries := range past {
		counts[name] = len(entries)
	}
	d.log.Info("past dismissals", "entries", counts)
	return b
}
