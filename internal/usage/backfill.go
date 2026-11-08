package usage

import (
	"context"
	"log/slog"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// Runs is the part of the store Backfill needs.
type Runs interface {
	RunsWithoutUsage(ctx context.Context, limit int) ([]store.Run, error)
	RecordUsage(ctx context.Context, id string, skills []store.SkillUsage) error
}

// backfillPage is how many results are read at once: each can be 10 MB, and
// the pod has 512Mi, so a page stays well under that.
const backfillPage = 5

// Backfill records the usage of every run whose result was stored before
// usage was recorded, page by page, until none is left or ctx ends. A result
// it can't read is marked with no usage. It returns how many runs it marked.
func Backfill(ctx context.Context, st Runs, log *slog.Logger) int {
	done := 0
	for ctx.Err() == nil {
		runs, err := st.RunsWithoutUsage(ctx, backfillPage)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("couldn't read the runs whose usage isn't recorded yet", "error", err.Error())
			}
			return done
		}
		if len(runs) == 0 {
			return done
		}
		for _, run := range runs {
			skills, err := FromResult(run.Result, run)
			if err != nil {
				// Marked all the same, so it isn't read again on every start.
				skills = nil
			}
			if err := st.RecordUsage(ctx, run.ID, skills); err != nil {
				if ctx.Err() == nil {
					log.Warn("couldn't record a run's usage", "run", run.ID, "error", err.Error())
				}
				return done
			}
			done++
		}
	}
	return done
}
