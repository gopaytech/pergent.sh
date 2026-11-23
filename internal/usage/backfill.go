package usage

import (
	"context"
	"log/slog"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// Runs is the part of the store Backfill needs.
type Runs interface {
	Recorder
	RunsWithoutUsage(ctx context.Context, limit int) ([]store.Run, error)
	IsJudgeRun(ctx context.Context, id string) (bool, error)
}

// backfillPage is how many results are read at once: each can be 10 MB, and
// the pod has 512Mi, so a page stays well under that.
const backfillPage = 5

// Backfill records the usage of every run whose result was stored before
// usage was recorded, page by page, until none is left or ctx ends. A result
// it can't read is marked with no usage. A multi review's judge run is
// recorded with RecordJudgeUsage (Usage.Record), so its tokens are added to
// the rows its reviewers made, with its judge rows. It returns how many runs
// it marked.
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
			judge, err := st.IsJudgeRun(ctx, run.ID)
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("couldn't read whether a run is a multi review's judge run", "run", run.ID, "error", err.Error())
				}
				return done
			}
			// An unreadable result is marked all the same, so it isn't read
			// again on every start.
			u, _ := Rows(run.Result, run, judge)
			if err := u.Record(ctx, st, run.ID); err != nil {
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

// Members is the part of the store BackfillReviewMembers needs.
type Members interface {
	ReviewMembersWithoutUsage(ctx context.Context, limit int) ([]store.ReviewMember, error)
	Run(ctx context.Context, id string) (store.Run, bool, error)
	RecordReviewMemberUsage(ctx context.Context, id string, skills []store.SkillUsage) error
}

// BackfillReviewMembers records the usage of every multi review reviewer whose
// result is stored and whose usage write was lost (a restart), page by
// page, as Backfill does for runs. The skills counted are those its review's
// first run ran (FromResult); a result it can't read, or one whose run is
// gone, is marked with no usage. It returns how many members it marked.
func BackfillReviewMembers(ctx context.Context, st Members, log *slog.Logger) int {
	done := 0
	for ctx.Err() == nil {
		ms, err := st.ReviewMembersWithoutUsage(ctx, backfillPage)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("couldn't read the reviewers whose usage isn't recorded yet", "error", err.Error())
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
					log.Warn("couldn't read a reviewer's run", "member", m.ID, "error", err.Error())
				}
				return done
			}
			var skills []store.SkillUsage
			if found {
				if skills, err = FromResult(m.Result, run); err != nil {
					skills = nil
				}
			}
			if err := st.RecordReviewMemberUsage(ctx, m.ID, skills); err != nil {
				if ctx.Err() == nil {
					log.Warn("couldn't record a reviewer's usage", "member", m.ID, "error", err.Error())
				}
				return done
			}
			done++
		}
	}
	return done
}
