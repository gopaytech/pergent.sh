package engine

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/quota"
	"github.com/gopaytech/pergent.sh/internal/security"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// A security scan is started by a person (RequestSecurityScan): the default
// branch and its newest commit are read then, so the scan runs on that
// commit. RunSecurityScans, in its own goroutine, claims queued scans while
// fewer than security.max_running run, picks the entry of the scan chain and
// the token budget, and starts a Job that downloads the source from
// pergent.sh and posts its result (Task 9). A scan never falls back to
// another entry: a failed scan is started again by a person. The sweep ends
// the scans whose Job failed, went or ran too long, and the cancelled ones
// that never posted. A scan that ends with no result records no tokens.

const (
	// securityCancelGrace is how long a scan whose Job got SIGTERM (a cancel,
	// or the Job's deadline) is given to post what it has, partial.
	securityCancelGrace = 2 * time.Minute
	// securityDeadlineGrace is the Job's time beyond the scan's timeout: the
	// source download before the scan and the post after it.
	securityDeadlineGrace = 10 * time.Minute
	// securityLaunchTimeout bounds creating the Job.
	securityLaunchTimeout = 30 * time.Second
)

// Loop intervals of RunSecurityScans. They are variables so tests can
// shorten them.
var (
	// securityPassInterval is how often queued scans are looked for without
	// a wake-up.
	securityPassInterval = 10 * time.Second
	// securitySweepInterval is how often running scans are checked on.
	securitySweepInterval = time.Minute
)

// Why a security scan failed, as the dashboard shows it. The Job's failures
// are kube.SecurityFailure's.
const (
	msgSecurityNoModel  = "no model of the chain can be used"
	msgSecurityNoImage  = "security scans need a scan image (jobs.security_image)"
	msgSecurityStart    = "couldn't start the scan Job; see the pergent.sh log"
	msgSecurityRead     = "couldn't read the repository's scan settings; see the pergent.sh log"
	msgSecurityLost     = "the scan stopped before its Job started; start it again"
	msgSecurityTimedOut = "the scan took longer than its timeout"
	// reasonSecurityCancelled is the reason of a cancelled scan.
	reasonSecurityCancelled = "cancelled"
)

// StartOff is why Start scan is off for every repository, "" when it is on.
func (e *Engine) StartOff() string {
	if !e.SecurityEgress {
		return security.ErrOff.Error()
	}
	return ""
}

// RequestSecurityScan queues a scan of a repository for s.Actor and wakes the
// scan loop. It refuses, in this order: security.ErrOff while the scan Jobs
// have no way out; security.ErrNotOn and security.ErrPaused; security.ErrBusy,
// with the repository's queued or running scan; the quota rules of a review's
// start (securityQuota); and a *security.RefusedError when the platform does
// not give the default branch or its newest commit. The scan is queued with
// that branch and commit, the repository's ID and its scan settings.
func (e *Engine) RequestSecurityScan(ctx context.Context, s security.Start) (store.SecurityScan, error) {
	if !e.SecurityEgress {
		return store.SecurityScan{}, security.ErrOff
	}
	repo, ok, err := e.settings().GetAny(ctx, s.Instance, s.Project)
	switch {
	case err != nil:
		return store.SecurityScan{}, err
	case !ok:
		return store.SecurityScan{}, security.ErrNotOn
	case repo.Paused:
		return store.SecurityScan{}, security.ErrPaused
	}
	active, err := e.Store.ActiveSecurityScan(ctx, repo.Instance, repo.Project)
	if err != nil {
		return store.SecurityScan{}, err
	}
	if active != nil {
		return *active, security.ErrBusy
	}
	// The repository's real ID: the quotas are counted by it.
	p, found, err := e.Store.ProjectByPath(ctx, repo.Instance, repo.Project)
	if err != nil {
		return store.SecurityScan{}, err
	}
	if !found {
		return store.SecurityScan{}, &security.RefusedError{Reason: errNotSetUp.Error()}
	}
	now := e.now()
	pastQuota, err := e.securityQuota(ctx, now, repo, p.ProjectID, s.Admin)
	if err != nil {
		return store.SecurityScan{}, err
	}
	t, err := e.graphTarget(ctx, repo.Instance, repo.Project, false)
	if err != nil {
		return store.SecurityScan{}, &security.RefusedError{Reason: err.Error()}
	}
	set, err := e.Store.SecurityScanSettingsOf(ctx, repo.Instance, repo.Project)
	if err != nil {
		return store.SecurityScan{}, err
	}
	timeout := int(security.DefaultTimeout / time.Second)
	if set.TimeoutSeconds != nil {
		timeout = *set.TimeoutSeconds
	}
	sc, err := e.Store.QueueSecurityScan(ctx, store.SecurityScanRequest{
		Instance: repo.Instance, Project: repo.Project, ProjectID: p.ProjectID, Branch: t.Branch, Commit: t.Commit,
		StartedBy: s.Actor, PastQuota: pastQuota, MaxSteps: set.MaxSteps, SubagentMaxSteps: set.SubagentMaxSteps,
		TimeoutSeconds: timeout,
	}, now)
	if errors.Is(err, store.ErrSecurityScanBusy) {
		return sc, security.ErrBusy
	}
	if err != nil {
		return store.SecurityScan{}, err
	}
	e.log().Info("security scan: queued", "scan", sc.ID, "number", sc.Number, "instance", sc.Instance,
		"project", sc.Project, "commit", sc.Commit, "by", s.Actor, "past_quota", pastQuota)
	e.wakeSecurity()
	return sc, nil
}

// securityQuota applies a review start's quota rules (actions.refusal and
// adminRefusal) to a scan start: the repository's quota counts every token;
// then the scan chain is resolved with the instance quota and the limits. A
// person who is not an admin is refused with security.ErrQuota when the
// repository's quota is used up, when only quotas and limits stop every
// entry, or when the instance's quota is used up and no entry is left. An
// admin goes past them (pastQuota true) and the admin limits, but not a
// team's own limit: a hold left after that is a *security.TeamLimitError.
func (e *Engine) securityQuota(ctx context.Context, now time.Time, repo settings.Effective, projectID int64, admin bool) (pastQuota bool, err error) {
	repoUsed, instanceUsed, err := quota.Check(ctx, e.Store, now, repo.Instance, projectID, repo)
	if err != nil {
		return false, err
	}
	in, err := chain.LoadScan(ctx, e.Store, e.registry(), now, repo)
	if err != nil {
		return false, err
	}
	in.InstanceQuotaUsedUp = instanceUsed
	dec := chain.Resolve(in)
	if !repoUsed && !dec.Hold && !(instanceUsed && !dec.Runs()) {
		return false, nil
	}
	if !admin {
		return false, security.ErrQuota
	}
	in.PastAdminLimits()
	if !chain.Resolve(in).Hold {
		return true, nil
	}
	t, found, err := e.Store.Team(ctx, in.OwnerTeam)
	if err != nil {
		return false, err
	}
	if !found {
		// Deleted in a race: no team to name.
		return false, security.ErrQuota
	}
	return false, &security.TeamLimitError{Team: t.Name}
}

// CancelSecurityScan asks scan id of a repository to stop. A queued scan ends
// at once as partial, cancelled. A running one records when the cancel was
// asked for; its log is taken and its Job deleted, so pergent gets SIGTERM
// and posts what it has (partial); the sweep ends it when no post comes. A
// scan that has ended comes back with security.ErrEnded; one that is not of
// the repository is store.ErrSecurityScanNotFound. The dashboard checks the
// person is a maintainer or an admin.
func (e *Engine) CancelSecurityScan(ctx context.Context, instance, project, id string) (store.SecurityScan, error) {
	sc, err := e.Store.SecurityScan(ctx, id)
	if err != nil {
		return store.SecurityScan{}, err
	}
	if sc.Instance != instance || !strings.EqualFold(sc.Project, project) {
		return store.SecurityScan{}, store.ErrSecurityScanNotFound
	}
	sc, ok, err := e.Store.RequestSecurityCancel(ctx, id, e.now())
	if err != nil {
		return store.SecurityScan{}, err
	}
	if !ok {
		return sc, security.ErrEnded
	}
	e.log().Info("security scan: cancel asked for", "scan", sc.ID, "instance", sc.Instance, "project", sc.Project,
		"state", sc.State)
	if sc.State == store.SecurityRunning {
		e.stopSecurityJob(ctx, sc)
	}
	return sc, nil
}

// cancelSecurityScanOf cancels a repository's queued or running scan, best
// effort: a repository switched off keeps its scans and findings, but nothing
// runs for it.
func (e *Engine) cancelSecurityScanOf(ctx context.Context, instance, project string) {
	log := e.log().With("instance", instance, "project", project)
	active, err := e.Store.ActiveSecurityScan(ctx, instance, project)
	if err != nil {
		log.Info("security scan: couldn't read the scan of a repository switched off", "error", e.redact(err.Error()))
		return
	}
	if active == nil {
		return
	}
	sc, ok, err := e.Store.RequestSecurityCancel(ctx, active.ID, e.now())
	if err != nil {
		log.Info("security scan: couldn't cancel the scan of a repository switched off", "scan", active.ID,
			"error", e.redact(err.Error()))
		return
	}
	if ok {
		log.Info("security scan: cancelled, the repository is switched off", "scan", sc.ID)
		if sc.State == store.SecurityRunning {
			e.stopSecurityJob(ctx, sc)
		}
	}
}

// stopSecurityJob takes the log of a running scan's Job, then deletes the
// Job: its pod gets SIGTERM and pergent posts what it has.
func (e *Engine) stopSecurityJob(ctx context.Context, sc store.SecurityScan) {
	if sc.JobName == "" || e.Launcher == nil {
		return
	}
	log := e.log().With("scan", sc.ID, "job", sc.JobName)
	e.captureJobLog(ctx, e.securityJob(sc), kube.Observation{}, log, logTimeout)
	e.deleteSecurityJob(ctx, sc, log)
}

func (e *Engine) deleteSecurityJob(ctx context.Context, sc store.SecurityScan, log *slog.Logger) {
	if sc.JobName == "" || e.Launcher == nil {
		return
	}
	if err := e.Launcher.Delete(ctx, sc.JobName); err != nil {
		log.Info("security scan: couldn't delete the Job", "job", sc.JobName, "error", e.redact(err.Error()))
	}
}

// securityJob is the jobLog of a scan's Job: its log and diagnostics go to
// the scan's hidden run.
func (e *Engine) securityJob(sc store.SecurityScan) jobLog {
	return jobLog{
		key: sc.ID, jobName: sc.JobName,
		putLog: func(ctx context.Context, text string) error { return e.Store.PutRunLog(ctx, sc.RunID, text) },
		putDiag: func(ctx context.Context, d store.Diagnostics) error {
			return e.Store.PutRunDiagnostics(ctx, sc.RunID, d)
		},
	}
}

// RunSecurityScans runs the security scans until ctx is done: it claims and
// launches queued scans on a wake-up and every 10 s, and checks on running
// ones every minute. It runs in its own goroutine; the review loop never
// waits on it. Without the dashboard (Scans) it returns at once.
func (e *Engine) RunSecurityScans(ctx context.Context) {
	if !e.Scans {
		return
	}
	e.sweepSecurityScans(ctx)
	e.launchSecurityScans(ctx)

	pass := time.NewTicker(securityPassInterval)
	defer pass.Stop()
	sweep := time.NewTicker(securitySweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.securityWakeCh():
			e.launchSecurityScans(ctx)
		case <-pass.C:
			e.launchSecurityScans(ctx)
		case <-sweep.C:
			e.sweepSecurityScans(ctx)
			e.launchSecurityScans(ctx)
		}
	}
}

// wakeSecurity asks the security scan loop for an early pass. It never
// blocks.
func (e *Engine) wakeSecurity() {
	select {
	case e.securityWakeCh() <- struct{}{}:
	default:
	}
}

func (e *Engine) securityWakeCh() chan struct{} {
	e.securityWakeOnce.Do(func() { e.securityWake = make(chan struct{}, 1) })
	return e.securityWake
}

// launchSecurityScans claims queued scans and starts them, one after
// another, while fewer than security.max_running run. A claimed scan has
// until its Job exists to get one, or the sweep fails it.
func (e *Engine) launchSecurityScans(ctx context.Context) {
	for ctx.Err() == nil {
		now := e.now()
		sc, ok, err := e.Store.ClaimSecurityScan(ctx, e.Config.Security.MaxRunning, now,
			now.Add(securityLaunchTimeout+time.Minute))
		if err != nil {
			if ctx.Err() == nil {
				e.log().Error("security scan: claiming a scan failed", "error", e.redact(err.Error()))
			}
			return
		}
		if !ok {
			return
		}
		e.startSecurityScan(ctx, sc)
	}
}

// startSecurityScan starts a claimed scan's Job. The repository is read
// again (gone or paused fails it), then the entry and the budget; the Job's
// name, token hash, deadline and entry are stored (on the hidden run too, so
// the model and team limits count its tokens) before the Job is created, so a
// fast Job never posts before its token is known. Any stop fails the scan
// with its reason. The platform is not asked anything: the commit was read
// at Start.
func (e *Engine) startSecurityScan(ctx context.Context, sc store.SecurityScan) {
	log := e.log().With("scan", sc.ID, "instance", sc.Instance, "project", sc.Project)
	switch {
	case !e.SecurityEgress:
		e.failSecurityScan(ctx, sc, security.ErrOff.Error())
		return
	case e.SecurityImage == "" || e.Launcher == nil:
		e.failSecurityScan(ctx, sc, msgSecurityNoImage)
		return
	}
	// LaunchSecurity does not check these: an out-of-range value never
	// reaches a Job.
	timeout := time.Duration(sc.TimeoutSeconds) * time.Second
	if err := security.CheckLimits(sc.MaxSteps, sc.SubagentMaxSteps, timeout); err != nil {
		e.failSecurityScan(ctx, sc, err.Error())
		return
	}
	repo, ok, err := e.settings().GetAny(ctx, sc.Instance, sc.Project)
	switch {
	case err != nil:
		log.Error("security scan: reading the repository failed", "error", e.redact(err.Error()))
		e.failSecurityScan(ctx, sc, msgSecurityRead)
		return
	case !ok:
		e.failSecurityScan(ctx, sc, security.ErrNotOn.Error())
		return
	case repo.Paused:
		e.failSecurityScan(ctx, sc, security.ErrPaused.Error())
		return
	}
	dec, ownerTeam, stop, err := e.securityEntry(ctx, sc, repo)
	if err == nil && stop == "" {
		var limited bool
		var left int64
		left, limited, err = quota.ScanBudget(ctx, e.Store, e.now(), repo, sc.ProjectID, dec.Ref, ownerTeam, sc.PastQuota)
		switch {
		case err == nil && limited && left <= 0:
			stop = security.ErrQuota.Error()
		case err == nil && limited:
			sc.TokenBudget = &left
		}
	}
	switch {
	case err != nil:
		log.Error("security scan: reading the chain or the quotas failed", "error", e.redact(err.Error()))
		e.failSecurityScan(ctx, sc, msgSecurityRead)
		return
	case stop != "":
		log.Info("security scan: stopped before the Job", "reason", stop)
		e.failSecurityScan(ctx, sc, stop)
		return
	}
	prior, err := e.securityPrior(ctx, sc)
	if err != nil {
		log.Error("security scan: building the prior file failed", "error", e.redact(err.Error()))
		e.failSecurityScan(ctx, sc, msgSecurityRead)
		return
	}

	token, hash, err := mintToken()
	if err != nil {
		log.Error("security scan: minting the token failed", "error", err.Error())
		e.failSecurityScan(ctx, sc, msgSecurityStart)
		return
	}
	name := kube.SecurityJobName(sc.ID)
	// The Job's deadline covers the timeout, the download and the post; the
	// scan waits a little longer, for the post a SIGTERM at that deadline
	// makes.
	jobDeadline := timeout + securityDeadlineGrace
	err = e.Store.StartSecurityJob(ctx, sc.ID, store.SecurityJobStart{
		JobName: name, TokenHash: hash, Deadline: e.now().Add(jobDeadline + securityCancelGrace),
		Entry: runEntry(dec), Provider: dec.Entry.LLM.Provider, Model: dec.Entry.LLM.Model, TokenBudget: sc.TokenBudget,
	})
	switch {
	case errors.Is(err, store.ErrSecurityScanNotFound):
		log.Info("security scan: the scan ended before its Job started")
		return
	case err != nil:
		log.Error("security scan: recording the Job failed", "error", e.redact(err.Error()))
		e.failSecurityScan(ctx, sc, msgSecurityStart)
		return
	}
	base := strings.TrimRight(e.Config.CallbackURL, "/") + "/security-scans/" + sc.ID
	p := kube.SecurityJobParams{
		Name: name, ScanID: sc.ID, Image: e.SecurityImage, Commit: sc.Commit,
		SourceURL: base + "/source", ResultURL: base + "/result",
		LLM: dec.Entry.LLM, Timeout: timeout, Deadline: jobDeadline,
		MaxSteps: sc.MaxSteps, SubagentMaxSteps: sc.SubagentMaxSteps, TokenBudget: sc.TokenBudget,
		Proxy: e.Config.Security.Proxy, Prior: prior,
		Resources: e.Config.Jobs.Resources, WorkSize: e.Config.Jobs.WorkSize,
	}
	lctx, cancel := context.WithTimeout(ctx, securityLaunchTimeout)
	err = e.Launcher.LaunchSecurity(lctx, p, dec.Entry.Key, token)
	cancel()
	if err != nil {
		log.Error("security scan: starting the Job failed", "job", name, "error", e.redact(err.Error()))
		e.failSecurityScan(ctx, sc, msgSecurityStart)
		return
	}
	log.Info("security scan: Job started", "job", name, "commit", sc.Commit, "entry", dec.Entry.Name,
		"budget", sc.TokenBudget != nil)
}

// securityEntry picks the entry a claimed scan runs on, with the owner team
// the budget counts for, or the reason it can't run (stop). A scan an admin
// started past the quotas (PastQuota) is resolved as an admin's start is
// (pastAdmin); any other is stopped by a used-up repository quota and held
// by the instance quota and the limits, which stops it too: a scan never
// waits. A scan never falls back: a failed scan is not tried on the next
// entry.
func (e *Engine) securityEntry(ctx context.Context, sc store.SecurityScan, repo settings.Effective) (dec chain.Decision, ownerTeam int64, stop string, err error) {
	now := e.now()
	instanceUsed := false
	if !sc.PastQuota {
		var repoUsed bool
		repoUsed, instanceUsed, err = quota.Check(ctx, e.Store, now, repo.Instance, sc.ProjectID, repo)
		if err != nil {
			return chain.Decision{}, 0, "", err
		}
		if repoUsed {
			return chain.Decision{}, 0, security.ErrQuota.Error(), nil
		}
	}
	in, err := chain.LoadScan(ctx, e.Store, e.registry(), now, repo)
	if err != nil {
		return chain.Decision{}, 0, "", err
	}
	in.InstanceQuotaUsedUp = instanceUsed
	dec = chain.Resolve(in)
	if dec.Hold {
		if !sc.PastQuota {
			return dec, 0, security.ErrQuota.Error(), nil
		}
		dec = pastAdmin(in)
	}
	if !dec.Runs() {
		return dec, 0, msgSecurityNoModel, nil
	}
	return dec, in.OwnerTeam, "", nil
}

// securityPrior is the prior file of a scan: the last complete scan's state
// and the dismissed findings (security.BuildPrior); nil when there are
// neither.
func (e *Engine) securityPrior(ctx context.Context, sc store.SecurityScan) ([]byte, error) {
	state, _, err := e.Store.LastCompleteSecurityState(ctx, sc.Instance, sc.Project)
	if err != nil {
		return nil, err
	}
	dismissed, err := e.Store.DismissedSecurityFindings(ctx, sc.Instance, sc.Project)
	if err != nil {
		return nil, err
	}
	if len(state) == 0 && len(dismissed) == 0 {
		return nil, nil
	}
	return security.BuildPrior(state, dismissed)
}

// failSecurityScan ends a running scan as failed with reason, from the
// pergent.sh side.
func (e *Engine) failSecurityScan(ctx context.Context, sc store.SecurityScan, reason string) {
	e.endSecurityScan(ctx, sc, store.SecurityFailed, reason)
}

// endSecurityScan ends a running scan with status and reason and no result:
// no finding, no state, and no tokens recorded.
func (e *Engine) endSecurityScan(ctx context.Context, sc store.SecurityScan, status, reason string) {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	ok, err := e.Store.FinishSecurityScan(sctx, sc.ID, nil,
		store.SecurityResult{Status: status, Reason: reason, Commit: sc.Commit}, e.now())
	if err != nil {
		e.log().Error("security scan: recording the end failed", "scan", sc.ID, "error", e.redact(err.Error()))
		return
	}
	if ok {
		e.log().Info("security scan: ended", "scan", sc.ID, "state", status, "reason", reason)
		e.wakeSecurity()
	}
}

// sweepSecurityScans checks on the running scans (sweepSecurityScan), then
// takes the log of the scans that ended lately.
func (e *Engine) sweepSecurityScans(ctx context.Context) {
	scans, err := e.Store.RunningSecurityScans(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.log().Error("security scan: listing the running scans failed", "error", e.redact(err.Error()))
		}
		return
	}
	now := e.now()
	for _, sc := range scans {
		if ctx.Err() != nil {
			return
		}
		e.sweepSecurityScan(ctx, sc, now)
	}
	e.captureSecurityLogs(ctx, now)
}

// sweepSecurityScan checks on one running scan. A cancelled scan that has not
// posted within securityCancelGrace ends partial, cancelled. A scan with no
// Job past its deadline (pergent.sh stopped before the Job) fails. A Job that
// failed or is gone fails the scan with kube.SecurityFailure's reason, once
// the Job has had securityCancelGrace to post after its SIGTERM; a scan past
// its deadline with no post fails as too slow. The Job of a scan ended past
// its deadline, or after a cancel, is deleted. Lost scans are not started
// again: a person starts a new one.
func (e *Engine) sweepSecurityScan(ctx context.Context, sc store.SecurityScan, now time.Time) {
	log := e.log().With("scan", sc.ID, "job", sc.JobName)
	if sc.CancelRequestedAt != nil {
		if now.Sub(*sc.CancelRequestedAt) <= securityCancelGrace {
			return
		}
		e.endSecurityScan(ctx, sc, store.SecurityPartial, reasonSecurityCancelled)
		e.deleteSecurityJob(ctx, sc, log)
		return
	}
	late := sc.Deadline != nil && now.After(*sc.Deadline)
	if sc.JobName == "" || e.Launcher == nil {
		if late {
			e.failSecurityScan(ctx, sc, msgSecurityLost)
		}
		return
	}
	reason, err := e.Launcher.SecurityFailure(ctx, sc.JobName)
	if err != nil {
		log.Info("security scan: couldn't read the Job", "error", e.redact(err.Error()))
		return
	}
	if reason != "" && !late {
		// A Job at its deadline gave pergent SIGTERM: it may still post.
		obs, err := e.Launcher.Inspect(ctx, sc.JobName, now)
		if err != nil {
			log.Info("security scan: couldn't read the Job", "error", e.redact(err.Error()))
			return
		}
		if obs.Exists && obs.Finished && now.Sub(obs.FinishedAt) < securityCancelGrace {
			return
		}
	}
	if reason == "" && late {
		reason = msgSecurityTimedOut
	}
	if reason == "" {
		return
	}
	e.failSecurityScan(ctx, sc, reason)
	if late {
		e.deleteSecurityJob(ctx, sc, log)
	}
}

// captureSecurityLogs takes, once, the log of every scan that ended within
// logWindow and whose Job has finished, into its hidden run's log. A Job that
// is gone has no log to take.
func (e *Engine) captureSecurityLogs(ctx context.Context, now time.Time) {
	if e.Launcher == nil {
		return
	}
	scans, err := e.Store.SecurityScansToLog(ctx, now.Add(-logWindow))
	if err != nil {
		if ctx.Err() == nil {
			e.log().Info("security scan: listing the scans to log failed", "error", e.redact(err.Error()))
		}
		return
	}
	for _, sc := range scans {
		if ctx.Err() != nil {
			return
		}
		if e.logTriedAlready(sc.ID) {
			continue
		}
		log := e.log().With("scan", sc.ID, "job", sc.JobName)
		obs, err := e.Launcher.Inspect(ctx, sc.JobName, now)
		if err != nil {
			log.Info("security scan: couldn't inspect the Job for its log", "error", e.redact(err.Error()))
			continue
		}
		switch {
		case !obs.Exists:
			e.firstLogTry(sc.ID)
		case obs.Finished:
			e.captureJobLog(ctx, e.securityJob(sc), obs, log, logTimeout)
		}
	}
}
