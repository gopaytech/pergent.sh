package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/diffmap"
	"github.com/gopaytech/pergent.sh/internal/fates"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// fateBatch is how many fate jobs one claim returns.
	fateBatch = 10
	// fateMaxAttempts is how many failed attempts stop a fate job (the
	// store's own limit; it stops offering the job then).
	fateMaxAttempts = 5
	// fateBackoff is the wait after a job's first failure; each further
	// failure doubles it. Five attempts span 75 minutes, which outlasts a
	// platform's hourly rate limit window.
	fateBackoff = 5 * time.Minute
	// fateNoPlatformWait is how long a job waits for an instance that has no
	// platform right now.
	fateNoPlatformWait = time.Minute
	// fateSweepWindow is how far back the hourly sweep looks for a decided
	// review, and fateSweepLimit how many changes it looks at (the store
	// returns one per repository).
	fateSweepWindow = 7 * 24 * time.Hour
	fateSweepLimit  = 50
	// fateRetention is how long a change's fates are kept.
	fateRetention = 365 * 24 * time.Hour
)

// fateBudget bounds a pass of workFates and a sweep: they stop starting work
// after it, and every platform call has it as its deadline, so a hung
// platform can't hold up the loop they share with dispatch. A variable so
// tests can shorten it.
var fateBudget = 30 * time.Second

// fateMinLeft is the least budget a pass needs left to claim or start a job: a
// job begun with less would lose an attempt to the deadline through no fault
// of its own. A variable so tests can shorten it.
var fateMinLeft = 5 * time.Second

// errChangeOpen is what fateEnd returns for a change that is open again.
var errChangeOpen = errors.New("the change is open")

// errNoPlatform is what fateJob returns for an instance with no platform right
// now: no fault of the job's.
var errNoPlatform = errors.New("the instance has no platform right now")

// workFates works out the fates of the changes whose webhook said they merged
// or closed, for at most fateBudget: the pass claims no job after that, and a
// job under way ends with its platform calls cut off. One job's trouble is
// logged and its retry arranged; nothing here touches a review.
func (e *Engine) workFates(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	bctx, cancel := context.WithTimeout(ctx, fateBudget)
	defer cancel()
	type jobKey struct {
		mr          store.MRKey
		requestedAt time.Time
	}
	// A job whose outcome couldn't be recorded is claimed again at once: a
	// pass works on each request once.
	seen := map[jobKey]bool{}
	for fateTimeLeft(bctx) {
		jobs, err := e.Store.ClaimFateJobs(ctx, e.now(), fateBatch)
		if err != nil {
			if ctx.Err() == nil {
				e.log().Error("couldn't list the changes whose fates are to be worked out", "error", e.redact(err.Error()))
			}
			return
		}
		worked := 0
		for _, job := range jobs {
			k := jobKey{job.MR, job.RequestedAt}
			if !fateTimeLeft(bctx) || seen[k] {
				continue
			}
			seen[k] = true
			worked++
			e.workFate(ctx, bctx, job)
		}
		if len(jobs) < fateBatch || worked == 0 {
			return
		}
	}
}

// workFate works one job out and, when it can't be now, arranges the retry.
// ctx is the engine's, bctx the pass's budget. A panic is a failed attempt: one
// bad change can't take the loop, shared with the reviews, down.
func (e *Engine) workFate(ctx, bctx context.Context, job store.FateJob) {
	log := e.log().With("instance", job.MR.Instance, "project_id", job.MR.ProjectID, "mr", job.MR.IID,
		"project", job.ProjectPath, "attempt", job.Attempts+1)
	defer func() {
		if p := recover(); p != nil {
			log.Error("working out the fates of a change panicked", "panic", e.redact(fmt.Sprint(p)), "stack", e.redact(string(debug.Stack())))
			e.fateFailed(ctx, job, fmt.Errorf("panic: %v", p), log)
		}
	}()
	err := e.fateJob(ctx, bctx, job, log)
	switch {
	case err == nil, ctx.Err() != nil:
		// Done, or the engine is stopping: the job stays as it is.
	case errors.Is(err, errChangeOpen):
		if err := e.Store.DropFateJob(ctx, job.MR, job.RequestedAt); err != nil {
			e.log().Error("couldn't drop the fate job of a reopened change", "error", e.redact(err.Error()))
			return
		}
		log.Info("the change is open again; its fates wait for its next merge or close")
	case errors.Is(err, errNoPlatform):
		// Waits a minute, like a command of an instance not connected, and
		// keeps its attempts.
		if err := e.Store.FateJobFailed(ctx, job.MR, job.RequestedAt, e.now().Add(fateNoPlatformWait), job.Attempts); err != nil {
			e.log().Error("couldn't hold back a fate job", "error", e.redact(err.Error()))
			return
		}
		log.Debug("the fates of a change wait: the instance has no platform right now")
	default:
		e.fateFailed(ctx, job, err, log)
	}
}

// fateFailed records a failed attempt on the job: it is held back with the
// backoff, and at fateMaxAttempts given up on, with a warning.
func (e *Engine) fateFailed(ctx context.Context, job store.FateJob, err error, log *slog.Logger) {
	failed := job.Attempts + 1
	delay := fateRetryDelay(failed)
	if err := e.Store.FateJobFailed(ctx, job.MR, job.RequestedAt, e.now().Add(delay), failed); err != nil {
		e.log().Error("couldn't record a failed attempt at working out fates", "error", e.redact(err.Error()))
		return
	}
	if failed >= fateMaxAttempts {
		log.Warn("giving up on the fates of a change: they couldn't be worked out", "attempts", failed, "error", e.redact(err.Error()))
		return
	}
	log.Warn("couldn't work out the fates of a change; will try again", "after", delay, "error", e.redact(err.Error()))
}

// fateTimeLeft reports whether the pass has at least fateMinLeft of its budget
// left, so a job can be started.
func fateTimeLeft(bctx context.Context) bool {
	if bctx.Err() != nil {
		return false
	}
	deadline, ok := bctx.Deadline()
	return !ok || time.Until(deadline) >= fateMinLeft
}

// fateJob works out and stores the fates of one change. It returns
// errChangeOpen for a change found open again, and any other error for a
// failure that is worth another try.
func (e *Engine) fateJob(ctx, bctx context.Context, job store.FateJob, log *slog.Logger) error {
	runs, err := e.Store.DecidedRuns(ctx, job.MR)
	if err != nil {
		return fmt.Errorf("load the change's reviews: %w", err)
	}
	if len(runs) == 0 {
		// Never reviewed: no findings, so nothing to ask the platform.
		return e.storeFates(ctx, job, nil, job.Merged, false, log)
	}
	plat, ok := e.Platforms.Get(job.MR.Instance)
	if !ok || plat.Forge == nil {
		return errNoPlatform
	}

	change, getErr := plat.Forge.GetChange(bctx, job.MR.ProjectID, job.MR.IID)
	merged, err := fateEnd(job, change, getErr)
	if err != nil {
		return err
	}
	// A change the platform no longer knows has no author to look up.
	known := getErr == nil

	ds, err := e.Store.Dismissals(ctx, job.MR)
	if err != nil {
		return fmt.Errorf("load dismissals: %w", err)
	}
	in := fatesInput(runs, ds)
	in.Compare = fateCompare(bctx, plat.Forge, job.MR.ProjectID)
	decided, err := fates.Decide(in)
	if err != nil {
		return fmt.Errorf("work out the fates: %w", err)
	}

	byBot := false
	if known {
		var untold error
		if byBot, untold, err = fateAuthorIsBot(bctx, plat.Forge, change); err != nil {
			return err
		} else if untold != nil {
			log.Info("couldn't tell whether a bot opened the change; counting it as a person's", "error", e.redact(untold.Error()))
		}
	}
	return e.storeFates(ctx, job, fateRows(decided), merged, byBot, log)
}

// storeFates stores rows as the fates of the job's change. The date on them is
// when the job was requested: GitLab's hook carries no merge time.
func (e *Engine) storeFates(ctx context.Context, job store.FateJob, rows []store.FindingFate, merged, byBot bool, log *slog.Logger) error {
	stored, err := e.Store.ReplaceFates(ctx, job.MR, job.RequestedAt, job.ProjectPath, rows, merged, byBot, job.RequestedAt)
	if err != nil {
		return fmt.Errorf("store the fates: %w", err)
	}
	if !stored {
		log.Info("the fate job changed meanwhile; its fates are not stored")
		return nil
	}
	log.Info("worked out the fates of a change", "findings", len(rows), "merged", merged, "by_bot", byBot)
	return nil
}

// fateStateEnded tells whether a change in state is over, and whether it
// merged. A change being merged (locked) is not over yet.
func fateStateEnded(state forge.ChangeState) (merged, ended bool) {
	switch state {
	case forge.StateMerged:
		return true, true
	case forge.StateClosed:
		return false, true
	}
	return false, false
}

// fateEnd says how the change of a job ended from what GetChange answered:
// the platform decides, not the webhook, except for a change it no longer
// knows (ErrNotFound), where the job's say stands. A change open again is
// errChangeOpen; one still being merged, or any other error, is a failure
// to try again.
func fateEnd(job store.FateJob, change forge.Change, err error) (merged bool, _ error) {
	switch {
	case errors.Is(err, forge.ErrNotFound):
		return job.Merged, nil
	case err != nil:
		return false, fmt.Errorf("get the change: %w", err)
	case change.State == forge.StateOpen:
		return false, errChangeOpen
	}
	merged, ended := fateStateEnded(change.State)
	if !ended {
		return false, fmt.Errorf("the change is %q, not over yet", change.State)
	}
	return merged, nil
}

// fateRetryDelay is the wait after a job has failed failed times: fateBackoff
// after the first, doubling.
func fateRetryDelay(failed int) time.Duration {
	return fateBackoff << max(failed-1, 0)
}

// fatesInput turns a change's decided runs (oldest first) and dismissals into
// what fates.Decide takes, without the Compare. A run with no result, an
// unreadable one or one of an error is no review: it says nothing about any
// finding. A finding with no fingerprint is dropped, as everywhere else.
func fatesInput(runs []store.Run, ds []store.Dismissal) fates.Input {
	var in fates.Input
	before := map[string][]string{} // skill to its left-out notes in its last review that did not fail
	for _, run := range runs {
		if run.Result == nil {
			continue
		}
		r, err := result.Parse(run.Result)
		if err != nil || r.Status != "completed" {
			continue
		}
		review := fates.Review{Head: run.HeadSHA}
		for _, s := range r.Skills {
			skill := fates.Skill{
				Name:     s.Name,
				Hash:     run.SkillHashes[s.Name],
				Failed:   s.Verdict == "failed",
				Reviewed: map[string]bool{},
			}
			for _, p := range s.Files.Reviewed {
				skill.Reviewed[p] = true
			}
			for _, p := range s.Files.Carried {
				skill.Reviewed[p] = true
			}
			var notes []string
			for _, n := range s.Notes {
				if strings.HasPrefix(n, result.LeftOutNotePrefix) {
					notes = append(notes, n)
					skill.LeftOut = skill.LeftOut || !result.LeftOutCoveredBy(n, before[s.Name])
				}
			}
			if !skill.Failed {
				before[s.Name] = notes
			}
			for _, raw := range s.Findings {
				var f result.Finding
				if json.Unmarshal(raw, &f) != nil || f.Fingerprint == "" {
					continue
				}
				skill.Findings = append(skill.Findings, fates.Finding{
					Fingerprint: f.Fingerprint, File: f.File, Line: f.Line, EndLine: f.EndLine, Severity: f.Severity,
				})
			}
			review.Skills = append(review.Skills, skill)
		}
		in.Reviews = append(in.Reviews, review)
	}
	for _, d := range ds {
		in.Dismissals = append(in.Dismissals, fates.Dismissal{Fingerprint: d.Fingerprint, Severity: d.Severity})
	}
	return in
}

// fateAuthorIsBot tells whether a bot opened the change: GitHub says so in the
// change itself, GitLab is asked (GitHub's IsBot is always false). When it
// can't be told, untold says why and the change counts as a person's, except
// when the budget ran out: merged fates are final, so that is err and the job
// is tried again. The budget is checked, not the error's cause, as the clients
// flatten it.
func fateAuthorIsBot(bctx context.Context, f forge.Forge, change forge.Change) (bot bool, untold, err error) {
	if change.AuthorIsBot {
		return true, nil, nil
	}
	bot, askErr := f.IsBot(bctx, change.AuthorID)
	switch {
	case askErr == nil:
		return bot, nil, nil
	case bctx.Err() != nil:
		return false, nil, fmt.Errorf("tell whether a bot opened the change: %w", askErr)
	}
	return false, askErr, nil
}

// fateCompare is fates.Compare over the platform's compare. A comparison the
// platform can't give for good (a commit gone after a force-push, not an
// ancestor, a diff that may be partial or too large) is fates.ErrGone, and so
// is any platform that can't compare at all. The platforms report the last
// three in their own packages, not forge's. A GitHub 422 may be a head that is
// not there yet, so it stays a failure to try again.
func fateCompare(ctx context.Context, f forge.Forge, projectID int64) fates.Compare {
	inline, ok := f.(forge.Inline)
	return func(from, to string) ([]diffmap.File, error) {
		if !ok {
			return nil, fates.ErrGone
		}
		files, err := inline.CompareDiff(ctx, projectID, from, to)
		switch {
		case errors.Is(err, forge.ErrNotFound), errors.Is(err, github.ErrNotAncestor),
			errors.Is(err, github.ErrPartialDiff), errors.Is(err, gitlab.ErrPartialDiff),
			errors.Is(err, github.ErrTooLarge), errors.Is(err, gitlab.ErrTooLarge):
			return nil, fmt.Errorf("%w: %w", fates.ErrGone, err)
		case err != nil:
			return nil, err
		}
		out := make([]diffmap.File, 0, len(files))
		for _, d := range files {
			out = append(out, diffmap.Parse(d.Path, d.OldPath, d.Patch, d.Deleted))
		}
		return out, nil
	}
}

// fateRows converts decided fates to the rows the store keeps.
func fateRows(decided []fates.Fate) []store.FindingFate {
	rows := make([]store.FindingFate, 0, len(decided))
	for _, f := range decided {
		rows = append(rows, store.FindingFate{
			Fingerprint: f.Fingerprint, Skill: f.Skill, SkillHash: f.SkillHash, Severity: f.Severity, Fate: f.Fate,
		})
	}
	return rows
}

// sweepFates finds the changes whose merge or close the webhook missed (it is
// down during a rollout, or a delivery failed) and asks for their fates to be
// worked out. The store offers at most one change per repository, least
// recently looked at, so the sweep reads at most one change per repository: one
// platform call on GitLab, two on GitHub for an open pull request (the pull
// request and a compare for its merge base). Each change is marked as looked
// at, so the next sweep reads another. It only asks: workFates, which holds the
// platform's rate limit per pass, does the rest.
func (e *Engine) sweepFates(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	bctx, cancel := context.WithTimeout(ctx, fateBudget)
	defer cancel()
	changes, err := e.Store.ChangesWithoutFates(ctx, e.now().Add(-fateSweepWindow), fateSweepLimit)
	if err != nil {
		if ctx.Err() == nil {
			e.log().Error("couldn't list the changes the fate sweep checks", "error", e.redact(err.Error()))
		}
		return
	}
	for _, c := range changes {
		// A read begun with little time left would fail and mark the change
		// as looked at without having looked.
		if !fateTimeLeft(bctx) {
			return
		}
		log := e.log().With("instance", c.MR.Instance, "project_id", c.MR.ProjectID, "mr", c.MR.IID, "project", c.ProjectPath)
		path, ended, merged := e.sweepFate(ctx, bctx, c, log)
		if ended {
			if err := e.Store.RequestFates(ctx, c.MR, path, merged, e.now()); err != nil {
				log.Error("couldn't ask for the fates of a change the sweep found ended", "error", e.redact(err.Error()))
				continue
			}
			log.Info("the fate sweep found a change that ended without a webhook", "merged", merged)
		}
		if err := e.Store.FateChecked(ctx, c.MR, e.now()); err != nil && ctx.Err() == nil {
			log.Error("couldn't record that the fate sweep looked at a change", "error", e.redact(err.Error()))
		}
	}
}

// sweepFate reads one change the sweep looks at and reports whether it ended,
// whether it merged, and the repository's path now. The path is the project's,
// as the approval sweep reads it: the run's may be from before a rename, and
// a repository with no project row is not switched on. Neither a repository
// switched off nor an instance with no platform right now is read; a change the
// platform can't be asked about is logged and counts as not ended.
func (e *Engine) sweepFate(ctx, bctx context.Context, c store.FateJob, log *slog.Logger) (path string, ended, merged bool) {
	project, found, err := e.Store.Project(ctx, c.MR.Instance, c.MR.ProjectID)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("couldn't load the project for the fate sweep", "error", e.redact(err.Error()))
		}
		return "", false, false
	}
	if !found {
		return "", false, false
	}
	if _, on, err := e.settings().Get(ctx, c.MR.Instance, project.Path); err != nil || !on {
		if err != nil && ctx.Err() == nil {
			log.Warn("couldn't check whether the repository is switched on", "error", e.redact(err.Error()))
		}
		return "", false, false
	}
	plat, ok := e.Platforms.Get(c.MR.Instance)
	if !ok || plat.Forge == nil {
		return "", false, false
	}
	change, err := plat.Forge.GetChange(bctx, c.MR.ProjectID, c.MR.IID)
	if err != nil {
		if !errors.Is(err, forge.ErrNotFound) && ctx.Err() == nil {
			log.Warn("the fate sweep couldn't read a change; will look again later", "error", e.redact(err.Error()))
		}
		return "", false, false
	}
	merged, ended = fateStateEnded(change.State)
	return project.Path, ended, merged
}

// pruneFates deletes the fates, jobs and checks older than the retention.
func (e *Engine) pruneFates(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	n, err := e.Store.PruneFates(ctx, e.now().Add(-fateRetention))
	if err != nil {
		if ctx.Err() == nil {
			e.log().Error("couldn't prune old fates", "error", e.redact(err.Error()))
		}
		return
	}
	if n > 0 {
		e.log().Info("pruned old fates", "rows", n)
	}
}
