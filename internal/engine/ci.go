package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/ci"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// ciRecheckFor is how long after it was reported a published review is
	// checked again by the CI sweep, while its snapshot has a chosen job
	// that didn't pass: a lost CI event then only delays the gate.
	ciRecheckFor = 24 * time.Hour
	// ciRecheckEvery is how many sweeps apart the published reviews are
	// checked again (every 10 minutes): the runs waiting for CI are checked
	// on every sweep, and there are far more published reviews.
	ciRecheckEvery = 5
	// ciNoPipelineGrace is how long after a run was queued a head with none
	// of the chosen jobs still waits: GitLab creates a push's pipeline after
	// the merge request's hook, so the first look often finds nothing.
	ciNoPipelineGrace = 3 * time.Minute
)

// ciSweepBudget bounds one CI sweep: it starts no check after it, and every
// platform call has it as its deadline, so a slow platform can't hold up the
// loop it shares with dispatch. A variable so tests can shorten it.
var ciSweepBudget = 30 * time.Second

// ciDroppedMsg is logged when the inputs have no room for ci-results.md.
const ciDroppedMsg = "reviewing without the CI job results: the inputs are too large for them"

// ciStore is what the CI wait needs from the store.
type ciStore interface {
	HoldForCI(ctx context.Context, id string, deadline time.Time) (bool, error)
	ReleaseCI(ctx context.Context, id string) (bool, error)
	MarkCIChecked(ctx context.Context, id string) error
	SetRunCI(ctx context.Context, id string, jobs []store.CIJobResult) error
	Republish(ctx context.Context, mr store.MRKey, headSHA, targetBranch string) (bool, error)
}

var _ ciStore = (*store.Store)(nil)

// ciStore is the store the dispatcher's CI calls go to: Store, unless a test
// set cis.
func (d *dispatcher) ciStore() ciStore {
	if d.cis != nil {
		return d.cis
	}
	return d.e.Store
}

// waitingText is the pending status of a run waiting for CI, for example
// "Waiting for CI: test, cycode-scan". It names the jobs only.
func waitingText(jobs []store.CIJob) string {
	names := make([]string, len(jobs))
	for i, j := range jobs {
		names[i] = j.Name
	}
	return "Waiting for CI: " + strings.Join(names, ", ")
}

// ciTarget is the head of run's change, as a CI read names it.
func ciTarget(run store.Run, _ Platform) forge.CITarget {
	return forge.CITarget{ProjectID: run.MR.ProjectID, Project: run.ProjectPath, Change: run.MR.IID, Head: run.HeadSHA}
}

// ciReader is the platform's CI reader; false when it can't read CI.
func ciReader(plat Platform) (forge.CIReader, bool) {
	if plat.Forge == nil {
		return nil, false
	}
	r, ok := plat.Forge.(forge.CIReader)
	return r, ok
}

// ciDone says whether the wait for snap's jobs is over at now: snap.Done(),
// except that within ciNoPipelineGrace of when run was queued a head with
// none of the jobs at all (every one missing) still waits for its pipeline.
func ciDone(snap ci.Snapshot, run store.Run, now time.Time) bool {
	if !snap.Done() {
		return false
	}
	if !now.Before(run.CreatedAt.Add(ciNoPipelineGrace)) {
		return true
	}
	for _, j := range snap.Jobs {
		if j.Result != ci.Missing {
			return true
		}
	}
	return len(snap.Jobs) == 0
}

// ciApplies says whether run waits for and reads CI jobs: the repository
// chose some, and it is a merge request's review. An API review has no
// pipeline.
func ciApplies(run store.Run, repo settings.Effective) bool {
	return len(repo.CIJobs.Jobs) > 0 && run.Kind != store.KindAPI
}

// gateCI checks the chosen CI jobs of the run's head once, status only, after
// the trust and quota gates. When every job has ended (or the head's CI is
// finished and the jobs not found are missing), or the run's deadline (when
// it was queued plus the wait) has passed, it is marked as checked and goes
// on: held is false. Within ciNoPipelineGrace of being queued, a head with
// none of the jobs yet is not over (ciDone). Otherwise it waits for CI (store.WaitCIPending) until
// the deadline, with the pending status waitingText and no Job; the CI sweep,
// a CI event or Start review queues it again. A read that fails holds it too:
// the deadline still releases it. A platform that can't read CI holds
// nothing, and at dispatch every job is unreadable. held is also true when
// the run went back to the queue; err is a store error only.
func (d *dispatcher) gateCI(ctx context.Context, plat Platform, repo settings.Effective) (held bool, err error) {
	e, run, st := d.e, d.run, d.ciStore()
	jobs := repo.CIJobs.Jobs
	deadline := run.CreatedAt.Add(repo.CIJobs.Wait())
	wait := false
	reader, canRead := ciReader(plat)
	switch {
	case !canRead:
		d.log.Info("not waiting for CI: pergent.sh can't read this platform's CI")
	case !e.now().Before(deadline):
		// Started long after it was queued: the wait is over already.
	default:
		snap, rerr := reader.ReadCI(ctx, ciTarget(run, plat), jobs, false)
		switch {
		case rerr != nil && ctx.Err() != nil:
			return true, d.postpone(ctx, e.now())
		case rerr != nil:
			d.log.Warn("the CI jobs can't be read; the run waits for them until its deadline",
				"deadline", deadline, "error", d.redact(rerr.Error()))
			wait = true
		default:
			wait = !ciDone(snap, run, e.now())
		}
	}
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if !wait {
		if err := st.MarkCIChecked(sctx, run.ID); err != nil {
			// Unmarked, a later attempt of the run checks CI again, and
			// waits no longer than the deadline.
			d.log.Warn("couldn't record that CI was checked", "error", d.redact(err.Error()))
		}
		return false, nil
	}
	ok, err := st.HoldForCI(sctx, run.ID, deadline)
	if err != nil {
		return true, d.retry(ctx, fmt.Errorf("hold the run for CI: %w", err))
	}
	if !ok {
		// Superseded meanwhile: back to the queue, which a superseded run
		// doesn't go to.
		return true, d.postpone(ctx, e.now())
	}
	d.log.Info("run waits for its CI jobs", "reason", store.WaitCIPending, "deadline", deadline)
	d.announceHold(sctx, plat, waitingText(jobs), nil)
	return true, nil
}

// addCIResults reads the chosen jobs of the run's head once more, with the
// log or report each one asks for, and gives the review ci-results.md
// (ci.Build, then the run's own secrets and the LLM keys removed). The snapshot (no text) is
// recorded first, for the gate at publish: a lost write would decide the
// review without its CI jobs. A read that fails, or a platform that can't
// read CI, makes every job unreadable (error), and the file is still written.
// The error is a store error (store.ErrRunGone for a run superseded
// meanwhile) or the read's when ctx ended.
func (d *dispatcher) addCIResults(ctx context.Context, plat Platform, repo settings.Effective, in *inputs.Inputs) error {
	jobs := repo.CIJobs.Jobs
	var snap ci.Snapshot
	reader, canRead := ciReader(plat)
	var rerr error
	if canRead {
		snap, rerr = reader.ReadCI(ctx, ciTarget(d.run, plat), jobs, true)
		if rerr != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("read the CI jobs: %w", rerr)
			}
			d.log.Warn("the CI jobs can't be read; the review is told they are unreadable", "error", d.redact(rerr.Error()))
		}
	}
	if !canRead || rerr != nil {
		snap = unreadableCI(d.run.HeadSHA, jobs)
	}
	if err := d.ciStore().SetRunCI(ctx, d.run.ID, snap.Results()); err != nil {
		return err
	}
	// ci.Build removes the known secret shapes. Every LLM key the registry
	// knows goes next, the longest first, then the run's own secrets.
	in.CIResults = []byte(d.redact(d.e.redact(string(ci.Build(snap)))))
	return nil
}

// unreadableCI is the snapshot of jobs on head when CI can't be read.
func unreadableCI(head string, jobs []store.CIJob) ci.Snapshot {
	s := ci.Snapshot{Head: head, Jobs: make([]ci.Job, 0, len(jobs))}
	for _, j := range jobs {
		s.Jobs = append(s.Jobs, ci.Job{
			Name: j.Name, Read: j.Read, Blocks: j.BlocksApproval,
			Result: ci.Unreadable, Ended: true, Reason: "error",
		})
	}
	return s
}

// sweepCI checks again (checkCIRun) every run waiting for CI and, on every
// ciRecheckEvery-th sweep (the first included), the newest published review
// of each change reported in the last ciRecheckFor whose snapshot has a job
// that didn't pass: a lost CI event only delays a run or its gate. The
// waiting runs go first, and the sweep stops at ciSweepBudget; what is left
// waits for the next one. One run's trouble is logged and left for the next
// sweep.
func (e *Engine) sweepCI(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	recheck := e.ciSweepRechecks()
	runs, err := e.Store.CIPendingRuns(ctx)
	if err != nil && ctx.Err() == nil {
		e.log().Error("CI sweep: couldn't list the runs waiting for CI", "error", e.redact(err.Error()))
	}
	if recheck {
		published, err := e.Store.CIRecheckRuns(ctx, e.now().Add(-ciRecheckFor))
		if err != nil && ctx.Err() == nil {
			e.log().Error("CI sweep: couldn't list the published reviews to check again", "error", e.redact(err.Error()))
		}
		for _, r := range published {
			if ciSweepWorth(r.CIJobs) {
				runs = append(runs, r)
			}
		}
	}
	e.sweepCIRuns(ctx, runs, e.checkCIRun)
}

// ciSweepRechecks counts a sweep and says whether it checks the published
// reviews too. Only the engine loop calls it.
func (e *Engine) ciSweepRechecks() bool {
	n := e.ciSweeps
	e.ciSweeps++
	return n%ciRecheckEvery == 0
}

// sweepCIRuns runs check on each of runs, in order, for at most
// ciSweepBudget. A shutdown (ctx) or the end of the budget stops it; neither
// is an error to log.
func (e *Engine) sweepCIRuns(ctx context.Context, runs []store.Run, check func(context.Context, store.Run) error) {
	bctx, cancel := context.WithTimeout(ctx, ciSweepBudget)
	defer cancel()
	for i, run := range runs {
		if ctx.Err() != nil {
			return
		}
		if bctx.Err() != nil {
			e.log().Info("CI sweep: out of time; the next sweep checks the rest", "left", len(runs)-i)
			return
		}
		err := check(bctx, run)
		if err == nil || ctx.Err() != nil {
			continue
		}
		if bctx.Err() != nil {
			e.log().Info("CI sweep: out of time while checking a run; the next sweep tries again", "run", run.ID)
			continue
		}
		e.log().Error("CI sweep: couldn't check a run's CI jobs; the next sweep tries again",
			"run", run.ID, "instance", run.MR.Instance, "project", run.ProjectPath, "mr", run.MR.IID,
			"error", e.redact(err.Error()))
	}
}

// checkCIRun looks at run's CI jobs again, status only. A run waiting for CI
// is queued (and the dispatcher woken) once every chosen job has ended, at
// its deadline, or when its repository chooses no jobs any more or is gone.
// A published review is decided again, with no new review, when a chosen
// job's result changed. Any other run is left alone. Call it from the engine
// loop only: a re-decide puts the review back to be published, then writes
// the new snapshot, and that order is safe only because publish runs on the
// same goroutine, so it can't read the run in between.
func (e *Engine) checkCIRun(ctx context.Context, run store.Run) error {
	repo, ok, err := e.settings().Get(ctx, run.MR.Instance, run.ProjectPath)
	if err != nil {
		return fmt.Errorf("load repository settings: %w", err)
	}
	var jobs []store.CIJob
	if ok {
		jobs = repo.CIJobs.Jobs
	}
	plat, _ := e.Platforms.Get(run.MR.Instance)
	reader, canRead := ciReader(plat)
	if plat.Forge != nil && !canRead {
		// Connected, and its CI can't be read: there is nothing to wait for.
		jobs = nil
	}
	return e.checkCI(ctx, e.Store, reader, plat, run, jobs)
}

// checkCI is checkCIRun once the repository's chosen jobs and the platform's
// reader (nil: not connected) are known.
func (e *Engine) checkCI(ctx context.Context, st ciStore, reader forge.CIReader, plat Platform, run store.Run, jobs []store.CIJob) error {
	switch {
	case run.State == store.Waiting && run.SkipReason == store.WaitCIPending:
		return e.releaseCI(ctx, st, reader, plat, run, jobs)
	case run.State == store.Published && run.Kind == store.KindMR:
		return e.redecideCI(ctx, st, reader, plat, run, jobs)
	}
	return nil
}

// releaseCI queues a run waiting for CI when its wait is over. A run with no
// deadline recorded is due: nothing would ever release it.
func (e *Engine) releaseCI(ctx context.Context, st ciStore, reader forge.CIReader, plat Platform, run store.Run, jobs []store.CIJob) error {
	due := run.CIDeadline == nil || !e.now().Before(*run.CIDeadline)
	if len(jobs) > 0 && !due {
		if reader == nil {
			// Not connected: the deadline releases it.
			return nil
		}
		snap, err := reader.ReadCI(ctx, ciTarget(run, plat), jobs, false)
		if err != nil {
			return fmt.Errorf("read the CI jobs: %w", err)
		}
		if !ciDone(snap, run, e.now()) {
			return nil
		}
	}
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	ok, err := st.ReleaseCI(sctx, run.ID)
	if err != nil {
		return fmt.Errorf("queue the run waiting for CI: %w", err)
	}
	if ok {
		e.log().Info("run no longer waits for CI", "run", run.ID, "instance", run.MR.Instance,
			"project", run.ProjectPath, "mr", run.MR.IID, "deadline_passed", due)
		e.Wake()
	}
	return nil
}

// redecideCI decides a published review again when a status-only read of the
// jobs it was decided with gives another result for one of them, while the
// repository still chooses CI jobs. The review is put back to be published first (Republish, of this
// run's head and target only); only then is the new snapshot written, so a
// failure leaves the old one, and the next sweep sees the change again. That
// order relies on publish running on the engine loop (see checkCIRun).
func (e *Engine) redecideCI(ctx context.Context, st ciStore, reader forge.CIReader, plat Platform, run store.Run, jobs []store.CIJob) error {
	if len(jobs) == 0 || reader == nil || len(run.CIJobs) == 0 {
		return nil
	}
	// The jobs the review was decided with, not the repository's later
	// choice: the review keeps what it was dispatched with.
	asked := make([]store.CIJob, len(run.CIJobs))
	for i, j := range run.CIJobs {
		asked[i] = store.CIJob{Name: j.Name, Read: j.Read, BlocksApproval: j.Blocks}
	}
	snap, err := reader.ReadCI(ctx, ciTarget(run, plat), asked, false)
	if err != nil {
		return fmt.Errorf("read the CI jobs: %w", err)
	}
	next, changed := mergeCI(run.CIJobs, snap.Results())
	if !changed {
		return nil
	}
	log := e.log().With("run", run.ID, "instance", run.MR.Instance, "project", run.ProjectPath, "mr", run.MR.IID)
	// The pair is written whatever is left of the sweep's budget: a
	// republish with no new snapshot would only be decided the same again.
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	ok, err := st.Republish(sctx, run.MR, run.HeadSHA, run.TargetBranch)
	if err != nil {
		return fmt.Errorf("put the review back to be decided again: %w", err)
	}
	if !ok {
		log.Debug("a CI job's result changed, but the review isn't the change's newest any more, or of its head")
		return nil
	}
	if err := st.SetRunCI(sctx, run.ID, next); err != nil {
		if errors.Is(err, store.ErrRunGone) {
			log.Debug("a CI job's result changed, but the run was superseded meanwhile")
			return nil
		}
		return fmt.Errorf("record the CI jobs' new results: %w", err)
	}
	log.Info("a CI job's result changed; the review is decided again")
	e.Wake()
	return nil
}

// ciSweepWorth says whether the sweep checks a published review again: some
// job didn't pass and its text read didn't fail. A job whose text read failed
// is kept as it is (mergeCI), so a review with no other job that didn't pass
// can only change on a job that passed, which a CI event reports.
func ciSweepWorth(stored []store.CIJobResult) bool {
	for _, j := range stored {
		if j.Result != string(ci.Passed) && !j.TextFailed {
			return true
		}
	}
	return false
}

// mergeCI is the snapshot to store after a status-only read of a review's
// jobs, and whether a job's result or reason changed. Each stored entry takes
// the read's result and reason, and keeps what the review was given (what was
// read, its size and cut) and whether it blocks. An entry whose text read
// failed (TextFailed) is kept as it is: a status-only read can't say the text
// could be read now. A stored job the read lacks is kept as it is.
func mergeCI(stored, read []store.CIJobResult) ([]store.CIJobResult, bool) {
	byName := make(map[string]store.CIJobResult, len(read))
	for _, r := range read {
		byName[r.Name] = r
	}
	out := make([]store.CIJobResult, len(stored))
	changed := false
	for i, s := range stored {
		out[i] = s
		r, ok := byName[s.Name]
		if !ok || s.TextFailed {
			continue
		}
		if r.Result != s.Result || r.Reason != s.Reason {
			out[i].Result, out[i].Reason = r.Result, r.Reason
			changed = true
		}
	}
	return out, changed
}

// ciEventQueue is how many CI events wait for the loop; a burst past it is
// dropped, and the sweep finds what was lost.
const ciEventQueue = 256

// ciEvent is a CI change on a head of a project, as a webhook reported it; a
// merged-results pipeline has no head of the run's, so it names the merge
// request (iid) instead.
type ciEvent struct {
	instance, project, head string
	iid                     int64
}

// ciKey is what two events must share to count as one.
type ciKey struct {
	instance, project, head string
	iid                     int64
}

func (ev ciEvent) key() ciKey {
	return ciKey{ev.instance, strings.ToLower(ev.project), ev.head, ev.iid}
}

// ciCh is the CI event channel, created on first use.
func (e *Engine) ciCh() chan ciEvent {
	e.ciOnce.Do(func() { e.ciEvents = make(chan ciEvent, ciEventQueue) })
	return e.ciEvents
}

// CIChanged tells the engine that a CI job or pipeline of head changed on
// project of instance. It never blocks: checking a run is the loop's work
// (checkCIRun is safe on that goroutine only), so the event is queued, and
// one that finds the queue full is dropped and logged. An event for a head
// that is already queued is merged into it.
func (e *Engine) CIChanged(instance, project, head string) {
	e.queueCI(ciEvent{instance: instance, project: project, head: head})
}

// CIChangedMR is CIChanged for a merged-results pipeline, which has the merge
// commit's sha: it names merge request iid instead.
func (e *Engine) CIChangedMR(instance, project string, iid int64) {
	e.queueCI(ciEvent{instance: instance, project: project, iid: iid})
}

func (e *Engine) queueCI(ev ciEvent) {
	k := ev.key()
	e.ciMu.Lock()
	if e.ciPending[k] {
		e.ciMu.Unlock()
		return
	}
	if e.ciPending == nil {
		e.ciPending = map[ciKey]bool{}
	}
	e.ciPending[k] = true
	e.ciMu.Unlock()
	select {
	case e.ciCh() <- ev:
	default:
		e.ciMu.Lock()
		delete(e.ciPending, k)
		e.ciMu.Unlock()
		e.log().Warn("CI event dropped: the queue is full; a waiting run is released by the sweep, a published review may stay as it is",
			"instance", ev.instance, "project", ev.project)
	}
}

// takeCIEvent forgets ev's key, before the check: an event that arrives while
// the check runs queues again.
func (e *Engine) takeCIEvent(ev ciEvent) {
	e.ciMu.Lock()
	delete(e.ciPending, ev.key())
	e.ciMu.Unlock()
}

// checkCIEvent checks again the runs of ev's head (store.CIRunsForHead) or
// merge request (CIRunsForMR), with the sweep's budget, so a burst of events
// can't hold the loop.
func (e *Engine) checkCIEvent(ctx context.Context, ev ciEvent) {
	e.takeCIEvent(ev)
	if ctx.Err() != nil {
		return
	}
	var runs []store.Run
	var err error
	if ev.iid != 0 {
		runs, err = e.Store.CIRunsForMR(ctx, ev.instance, ev.project, ev.iid)
	} else {
		runs, err = e.Store.CIRunsForHead(ctx, ev.instance, ev.project, ev.head)
	}
	if err != nil {
		if ctx.Err() == nil {
			e.log().Error("CI event: couldn't list the runs of the head; the next sweep tries again",
				"instance", ev.instance, "project", ev.project, "error", e.redact(err.Error()))
		}
		return
	}
	e.sweepCIRuns(ctx, runs, e.checkCIRun)
}
