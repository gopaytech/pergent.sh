package usage

import (
	"context"
	"log/slog"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// Runs is the part of the store Backfill needs.
type Runs interface {
	RunsWithoutUsage(ctx context.Context, limit int) ([]store.Run, error)
	IsPanelMerge(ctx context.Context, id string) (bool, error)
	RecordUsage(ctx context.Context, id string, skills []store.SkillUsage, models []store.ModelUsage) error
}

// backfillPage is how many results are read at once: each can be 10 MB, and
// the pod has 512Mi, so a page stays well under that.
const backfillPage = 5

// Backfill records the usage of every run whose result was stored before
// usage was recorded, page by page, until none is left or ctx ends. A result
// it can't read is marked with no usage. A review panel's merge (Job P) is
// recorded with its model rows (Rows). It returns how many runs it marked.
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
			merge, err := st.IsPanelMerge(ctx, run.ID)
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("couldn't read whether a run is a review panel's merge", "run", run.ID, "error", err.Error())
				}
				return done
			}
			// An unreadable result is marked all the same, so it isn't read
			// again on every start.
			skills, models, _ := Rows(run.Result, run, merge)
			if err := st.RecordUsage(ctx, run.ID, skills, models); err != nil {
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

// Members is the part of the store BackfillMembers needs.
type Members interface {
	MembersWithoutUsage(ctx context.Context, limit int) ([]store.PanelMember, error)
	Run(ctx context.Context, id string) (store.Run, bool, error)
	RecordMemberUsage(ctx context.Context, id string, skills []store.SkillUsage) error
}

// BackfillMembers records the usage of every review panel member whose
// result is stored and whose usage write was lost (a restart), page by
// page, as Backfill does for runs. The skills counted are those its review's
// first run ran (FromResult); a result it can't read, or one whose run is
// gone, is marked with no usage. It returns how many members it marked.
func BackfillMembers(ctx context.Context, st Members, log *slog.Logger) int {
	done := 0
	for ctx.Err() == nil {
		ms, err := st.MembersWithoutUsage(ctx, backfillPage)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("couldn't read the panel members whose usage isn't recorded yet", "error", err.Error())
			}
			return done
		}
		if len(ms) == 0 {
			return done
		}
		for _, m := range ms {
			run, found, err := st.Run(ctx, m.RunID)
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("couldn't read a panel member's run", "member", m.ID, "error", err.Error())
				}
				return done
			}
			var skills []store.SkillUsage
			if found {
				if skills, err = FromResult(m.Result, run); err != nil {
					skills = nil
				}
			}
			if err := st.RecordMemberUsage(ctx, m.ID, skills); err != nil {
				if ctx.Err() == nil {
					log.Warn("couldn't record a panel member's usage", "member", m.ID, "error", err.Error())
				}
				return done
			}
			done++
		}
	}
	return done
}
