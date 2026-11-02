package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/skillfetch"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// A readiness scan is queued as a row (on switch-on, once after the upgrade
// for a repository never scanned, or on Scan now) and run by RunScans, in its
// own goroutine, never the review loop's: the pod part reads the settings
// with the review bot or App and stores them at once, then a scan Job clones
// the default branch and posts what it found (CompleteScan). Scan Jobs have
// their own cap (scanCap) and don't count toward jobs.max_running, so a
// review never waits for a scan. A done scan raises or clears the 3 Needs
// attention items; a failed one changes none.

// Limits of the scans.
const (
	// scanCap is how many scans run at once.
	scanCap = 2
	// scanGap is the least time between two scans of a repository asked for
	// with Scan now.
	scanGap = 5 * time.Minute
	// scanJobTimeout is the scan Job's time (its activeDeadlineSeconds), and
	// scanGrace how much longer its result is waited for.
	scanJobTimeout = 5 * time.Minute
	scanGrace      = time.Minute
	// scanPodBudget bounds the pod part: the settings reads.
	scanPodBudget = time.Minute
	// scanLaunchTimeout bounds creating the Job.
	scanLaunchTimeout = 30 * time.Second
)

// The caps on what a scan Job posts, beyond readiness.MaxNoisePaths and
// MaxPathBytes. More skills than maxResultSkills (the library's 200 and as
// many repository skills) is a result pergent.sh can't use; the other lists
// keep their first entries.
const (
	maxResultSkills    = 2 * inputs.MaxLibrarySkills
	maxResultLanguages = 10
	maxNoiseSkills     = 20
	maxNameBytes       = 255
)

// Loop intervals of RunScans. They are variables so tests can shorten them.
var (
	// scanPassInterval is how often queued scans are looked for without a
	// wake-up.
	scanPassInterval = 10 * time.Second
	// scanSweepInterval is how often running scans are checked on.
	scanSweepInterval = time.Minute
)

// Why a scan failed, as the dashboard shows it after "couldn't scan files: ".
// The pod's stop reasons are scanSettings'; the Job's failures are
// kube.ScanFailure's.
const (
	msgScanNoImage   = "readiness scans need PERGENT_SH_IMAGE (set by the chart)"
	msgScanStart     = "couldn't start the scan Job; see the pergent.sh log"
	msgScanUnusable  = "the scan Job sent a result pergent.sh couldn't use"
	msgScanBodyLarge = "the scan Job sent a result over 1 MiB"
	msgScanSettings  = "couldn't read the repository's settings; see the pergent.sh log"
	msgScanSecrets   = "couldn't check the scan result for secrets pergent.sh holds; scan again"
	// msgScanLost: the pod part never got to the Job (pergent.sh stopped).
	msgScanLost     = "the scan stopped before its Job started; scan again"
	msgScanTimedOut = "the scan took longer than 5 minutes"
	msgScanNoResult = "the scan Job ended without a result"
	// msgScanFailedWithheld stands for a Job's error text that is empty or
	// holds a secret, and msgSkillProblemWithheld for such a skill problem.
	msgScanFailedWithheld   = "the scan Job couldn't scan the files"
	msgSkillProblemWithheld = "the skill can't be used"
)

var (
	// gitHubToken is the shape of a GitHub installation token, which a
	// scan Job's clone holds and the store does not.
	gitHubToken = regexp.MustCompile(`ghs_[A-Za-z0-9]{20,}`)
)

// RequestScan queues a scan of instance's repository project for actor (Scan
// now) and wakes the scan loop. A repository with a queued or running scan
// gets readiness.ErrScanBusy, and one scanned less than 5 minutes ago a
// *readiness.ScanTooSoonError; both come with that scan. A repository that
// is not switched on gets readiness.ErrNotSwitchedOn, and every repository
// readiness.ErrScansOff when scans are off.
func (e *Engine) RequestScan(ctx context.Context, instance, project, actor string) (store.ReadinessScan, error) {
	if !e.Scans {
		return store.ReadinessScan{}, readiness.ErrScansOff
	}
	if _, ok, err := e.settings().Get(ctx, instance, project); err != nil {
		return store.ReadinessScan{}, err
	} else if !ok {
		return store.ReadinessScan{}, readiness.ErrNotSwitchedOn
	}
	now := e.now()
	sc, out, err := e.Store.QueueReadinessScan(ctx, instance, project, actor, now, scanGap)
	switch {
	case err != nil:
		return store.ReadinessScan{}, err
	case out == store.ScanBusy:
		return sc, readiness.ErrScanBusy
	case out == store.ScanTooSoon:
		return sc, &readiness.ScanTooSoonError{Wait: max(scanGap-now.Sub(sc.QueuedAt), time.Second)}
	}
	e.log().Info("readiness scan: queued", "scan", sc.ID, "instance", instance, "project", project, "by", actor)
	e.wakeScans()
	return sc, nil
}

// RunScans runs the scans until ctx is done: it queues the scans after the
// upgrade and asks for a recheck of the config-file repositories, then claims and launches queued scans (on a wake-up and every
// 10 s) and checks on running ones every minute. The rechecks
// RecheckReadiness asks for run in a goroutine of their own, so a slow pod
// part never holds up "Use these skills". RunScans runs in its own
// goroutine; the review loop never waits on it. It returns once both have
// stopped; without Scans it returns at once.
func (e *Engine) RunScans(ctx context.Context) {
	if !e.Scans {
		return
	}
	rechecks := make(chan struct{})
	go func() {
		defer close(rechecks)
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.recheckCh():
				e.runRechecks(ctx)
			}
		}
	}()
	defer func() { <-rechecks }()

	e.upgradeScans(ctx)
	e.recheckConfigRepos(ctx)
	e.sweepScans(ctx)
	e.launchScans(ctx)

	pass := time.NewTicker(scanPassInterval)
	defer pass.Stop()
	sweep := time.NewTicker(scanSweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.scanWakeCh():
			e.launchScans(ctx)
		case <-pass.C:
			e.launchScans(ctx)
		case <-sweep.C:
			// A sweep after the upgrade that failed is tried again here.
			e.upgradeScans(ctx)
			e.sweepScans(ctx)
			e.launchScans(ctx)
		}
	}
}

// wakeScans asks the scan loop for an early pass. It never blocks.
func (e *Engine) wakeScans() {
	select {
	case e.scanWakeCh() <- struct{}{}:
	default:
	}
}

func (e *Engine) scanWakeCh() chan struct{} {
	e.scanWakeOnce.Do(func() { e.scanWake = make(chan struct{}, 1) })
	return e.scanWake
}

// upgradeScans queues a scan, by store.StartedByUpgrade, of every repository
// switched on that has never been scanned, oldest first. It runs once; one
// that failed is tried again by the next call. A repository whose instance
// is being removed, or that is switched off while its projects row is still
// there, is left out.
func (e *Engine) upgradeScans(ctx context.Context) {
	if !e.Scans {
		return
	}
	e.upgradeMu.Lock()
	defer e.upgradeMu.Unlock()
	if e.upgradeDone || ctx.Err() != nil {
		return
	}
	log := e.log()
	refs, err := e.Store.UnscannedProjects(ctx)
	if err != nil {
		log.Error("readiness scan: couldn't list the repositories never scanned", "error", e.redact(err.Error()))
		return
	}
	on, err := e.switchedOn(ctx)
	if err != nil {
		log.Error("readiness scan: couldn't list the repositories switched on", "error", e.redact(err.Error()))
		return
	}
	// Claims go by queued_at, then a random id: each scan is queued a little
	// after the one before, so the order holds.
	now, queued := e.now(), 0
	for _, r := range refs {
		if !on[keyOf(r.Instance, r.Project)] {
			continue
		}
		_, out, err := e.Store.QueueReadinessScan(ctx, r.Instance, r.Project, store.StartedByUpgrade,
			now.Add(time.Duration(queued)*time.Millisecond), 0)
		if err != nil {
			log.Error("readiness scan: couldn't queue a repository never scanned", "instance", r.Instance,
				"project", r.Project, "error", e.redact(err.Error()))
			return
		}
		if out == store.ScanQueuedNow {
			queued++
		}
	}
	e.upgradeDone = true
	if queued > 0 {
		log.Info("readiness scan: queued the repositories never scanned", "scans", queued)
		e.wakeScans()
	}
}

// switchedOn is the repositories switched on now, leaving out those of an
// instance being removed.
func (e *Engine) switchedOn(ctx context.Context) (map[repoKey]bool, error) {
	repos, err := e.settings().List(ctx)
	if err != nil {
		return nil, err
	}
	on := make(map[repoKey]bool, len(repos))
	for _, r := range repos {
		if !e.removing(r.Instance) {
			on[keyOf(r.Instance, r.Project)] = true
		}
	}
	return on, nil
}

func (e *Engine) removing(instance string) bool {
	inst, ok := e.registry().Get(instance)
	return ok && inst.Removing
}

// queueSwitchOnScan queues a scan, by store.StartedBySwitchOn, of a
// repository a reconcile pass has just set up, when it has no scan at all.
// It is best effort: a failure is logged and the next pass tries again.
func (e *Engine) queueSwitchOnScan(ctx context.Context, instance, project string) {
	if !e.Scans || e.removing(instance) {
		return
	}
	log := e.log().With("instance", instance, "project", project)
	latest, active, err := e.Store.ReadinessScans(ctx, instance, project)
	if err != nil {
		log.Info("readiness scan: couldn't read the repository's scans", "error", e.redact(err.Error()))
		return
	}
	if latest != nil || active != nil {
		return
	}
	sc, out, err := e.Store.QueueReadinessScan(ctx, instance, project, store.StartedBySwitchOn, e.now(), 0)
	if err != nil {
		log.Info("readiness scan: couldn't queue the switch-on scan", "error", e.redact(err.Error()))
		return
	}
	if out == store.ScanQueuedNow {
		log.Info("readiness scan: queued on switch-on", "scan", sc.ID)
		e.wakeScans()
	}
}

// dropScans removes every scan of a repository switched off, and its running
// Job, best effort. A result its Job posts later finds no scan.
func (e *Engine) dropScans(ctx context.Context, instance, project string) {
	log := e.log().With("instance", instance, "project", project)
	if _, active, err := e.Store.ReadinessScans(ctx, instance, project); err == nil && active != nil &&
		active.JobName != "" && e.Launcher != nil {
		if err := e.Launcher.Delete(ctx, active.JobName); err != nil {
			log.Info("readiness scan: couldn't delete the Job of a repository switched off", "job", active.JobName,
				"error", e.redact(err.Error()))
		}
	}
	if err := e.Store.DeleteReadinessScans(ctx, instance, project); err != nil {
		log.Info("couldn't delete the readiness scans of a repository that was switched off",
			"error", e.redact(err.Error()))
	}
}

// launchScans claims queued scans and starts them, one after another, while
// fewer than scanCap run.
func (e *Engine) launchScans(ctx context.Context) {
	for ctx.Err() == nil {
		now := e.now()
		sc, ok, err := e.Store.ClaimReadinessScan(ctx, scanCap, now, now.Add(scanPodBudget+scanGrace))
		if err != nil {
			if ctx.Err() == nil {
				e.log().Error("readiness scan: claiming a scan failed", "error", e.redact(err.Error()))
			}
			return
		}
		if !ok {
			return
		}
		e.startScan(ctx, sc)
	}
}

// startScan runs a claimed scan's pod part and starts its Job: the settings
// are read and stored first, so they show even when the Job can't run; then
// the Job's name, token hash and deadline, then the Job (a fast Job never
// posts before its token is known). Any stop fails the scan with its reason.
// A scan deleted meanwhile (switched off) is left alone. With no scan image
// it can never get to its Job, so it fails before any platform call.
func (e *Engine) startScan(ctx context.Context, sc store.ReadinessScan) {
	log := e.log().With("scan", sc.ID, "instance", sc.Instance, "project", sc.Project)
	if e.ScanImage == "" || e.Launcher == nil {
		e.failScan(ctx, sc.ID, msgScanNoImage)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, scanPodBudget)
	target, stop := e.scanSettings(pctx, sc.Instance, sc.Project)
	cancel()
	defer clear(target.GitToken)

	pod, err := json.Marshal(target.Pod)
	if err == nil {
		err = e.Store.SetReadinessPod(ctx, sc.ID, target.Branch, target.Commit, pod)
	}
	switch {
	case errors.Is(err, store.ErrReadinessScanNotFound):
		log.Info("readiness scan: the scan was gone before its settings were stored")
		return
	case err != nil:
		log.Error("readiness scan: storing the settings failed", "error", e.redact(err.Error()))
		e.failScan(ctx, sc.ID, msgScanSettings)
		return
	case stop != nil:
		log.Info("readiness scan: stopped before the Job", "reason", stop.Error())
		e.failScan(ctx, sc.ID, stop.Error())
		return
	}

	input, err := e.scanInput(ctx, sc.Instance, sc.Project)
	switch {
	case errors.Is(err, readiness.ErrNotSwitchedOn):
		e.failScan(ctx, sc.ID, errNotSetUp.Error())
		return
	case err != nil:
		log.Error("readiness scan: reading the skills failed", "error", e.redact(err.Error()))
		e.failScan(ctx, sc.ID, msgScanSettings)
		return
	case len(input) > readiness.MaxInputBytes:
		e.failScan(ctx, sc.ID, kube.ErrScanInputTooLarge.Error())
		return
	}

	token, hash, err := mintToken()
	if err != nil {
		log.Error("readiness scan: minting the token failed", "error", err.Error())
		e.failScan(ctx, sc.ID, msgScanStart)
		return
	}
	name := kube.ScanJobName(sc.ID)
	err = e.Store.StartReadinessJob(ctx, sc.ID, name, hash, e.now().Add(scanJobTimeout+scanGrace))
	switch {
	case errors.Is(err, store.ErrReadinessScanNotFound):
		log.Info("readiness scan: the scan was gone before its Job started")
		return
	case err != nil:
		log.Error("readiness scan: recording the Job failed", "error", e.redact(err.Error()))
		e.failScan(ctx, sc.ID, msgScanStart)
		return
	}
	p := kube.ScanJobParams{
		Name: name, ScanID: sc.ID, GitImage: e.Config.Jobs.Image, Image: e.ScanImage,
		RepoURL: target.RepoURL, GitUsername: target.GitUser, Branch: target.Branch, Commit: target.Commit,
		ResultURL: strings.TrimRight(e.Config.CallbackURL, "/") + "/readiness-scans/" + sc.ID + "/result",
		Input:     string(input),
	}
	lctx, cancel := context.WithTimeout(ctx, scanLaunchTimeout)
	err = e.Launcher.LaunchScan(lctx, p, string(target.GitToken), token)
	cancel()
	if err != nil {
		log.Error("readiness scan: starting the Job failed", "job", name, "error", e.redact(err.Error()))
		e.failScan(ctx, sc.ID, msgScanStart)
		return
	}
	log.Info("readiness scan: Job started", "job", name, "branch", target.Branch, "commit", target.Commit)
}

// scanInput is the JSON of the Job's readiness.JobInput: every library skill's
// globs (never its text), Assigned when the repository's list (or the default
// list, for a repository that names none) has it, and the repository skills
// turned on. A library skill whose globs can't be read is left out, so the
// tab says it was not scanned rather than guess.
func (e *Engine) scanInput(ctx context.Context, instance, project string) ([]byte, error) {
	eff, ok, err := e.settings().Get(ctx, instance, project)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, readiness.ErrNotSwitchedOn
	}
	assigned, err := e.assignedSkills(ctx, eff.Skills)
	if err != nil {
		return nil, err
	}
	list, err := e.Store.LibrarySkills(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list)) // not nil: nil reads the default list
	for _, s := range list {
		names = append(names, s.Name)
	}
	resolved, err := e.Store.ResolveLibrary(ctx, names)
	if err != nil {
		return nil, err
	}
	in := readiness.JobInput{Version: readiness.ResultVersion, RepoSkills: eff.RepoSkills}
	for _, name := range names {
		sk, ok := resolved.Skills[name]
		if !ok {
			continue // deleted since it was listed
		}
		paths, exclude, err := inputs.SkillScope([]byte(sk.Content))
		if err != nil {
			e.log().Warn("readiness scan: a library skill's paths can't be read; it is not scanned",
				"skill", name, "error", err.Error())
			continue
		}
		in.Library = append(in.Library, readiness.ScopedSkill{Name: name, Paths: paths, Exclude: exclude,
			Assigned: slices.Contains(assigned, name)})
	}
	return json.Marshal(in)
}

// assignedSkills is a repository's library skills: skills, or the default
// list when it is nil.
func (e *Engine) assignedSkills(ctx context.Context, skills []string) ([]string, error) {
	if skills != nil {
		return skills, nil
	}
	d, err := e.Store.RepoDefaults(ctx)
	if err != nil {
		return nil, err
	}
	return d.Skills, nil
}

// failScan ends a running scan as failed with reason, from the pod side.
func (e *Engine) failScan(ctx context.Context, id, reason string) {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	ok, err := e.Store.FinishReadinessScan(sctx, id, nil, nil, reason, e.now())
	if err != nil {
		e.log().Error("readiness scan: recording the failure failed", "scan", id, "error", e.redact(err.Error()))
		return
	}
	if ok {
		e.log().Info("readiness scan: failed", "scan", id, "reason", reason)
		e.wakeScans()
	}
}

// sweepScans checks on the running scans: one whose Job failed or is gone
// fails with kube.ScanFailure's reason; one past its deadline fails as too
// slow (its Job is deleted), or as ending with no result when its Job
// completed without posting; one whose pod part never got to the Job (a
// restart) fails past its deadline. Lost scans are not retried: Scan now is
// the retry.
func (e *Engine) sweepScans(ctx context.Context) {
	log := e.log()
	scans, err := e.Store.RunningReadinessScans(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("readiness scan: listing the running scans failed", "error", e.redact(err.Error()))
		}
		return
	}
	now := e.now()
	for _, sc := range scans {
		if ctx.Err() != nil {
			return
		}
		late := now.After(sc.Deadline)
		if sc.JobName == "" || e.Launcher == nil {
			if late {
				e.failScan(ctx, sc.ID, msgScanLost)
			}
			continue
		}
		reason, err := e.Launcher.ScanFailure(ctx, sc.JobName)
		if err != nil {
			log.Info("readiness scan: couldn't read the Job", "scan", sc.ID, "job", sc.JobName, "error", e.redact(err.Error()))
			continue
		}
		if reason == "" && late {
			obs, err := e.Launcher.Inspect(ctx, sc.JobName, now)
			if err != nil {
				log.Info("readiness scan: couldn't read the Job", "scan", sc.ID, "job", sc.JobName, "error", e.redact(err.Error()))
				continue
			}
			reason = msgScanTimedOut
			if obs.Finished {
				reason = msgScanNoResult
			}
		}
		if reason == "" {
			continue
		}
		e.failScan(ctx, sc.ID, reason)
		if late {
			if err := e.Launcher.Delete(ctx, sc.JobName); err != nil {
				log.Info("readiness scan: couldn't delete the Job past its deadline", "job", sc.JobName,
					"error", e.redact(err.Error()))
			}
		}
	}
}

// CompleteScan takes the result body a scan Job posted with token, once. The
// callback has already read the body (at most readiness.MaxResultBytes); the
// token is checked here before the body is parsed. A result that is not version 1,
// or not of the scan's commit, fails the scan as unusable; a Job's error
// fails it with that text. Every string is untrusted (skillfetch.JobText, held
// secrets withheld), paths over readiness.MaxPathBytes are dropped and lists
// capped. A done scan raises or clears the Needs attention items. A scan that
// does not exist (deleted when its repository was switched off) or holds
// another token is ScanUnauthorized; one that has ended, ScanConflict.
func (e *Engine) CompleteScan(ctx context.Context, id, token string, body []byte) (readiness.ScanOutcome, error) {
	sc, err := e.Store.ReadinessScan(ctx, id)
	switch {
	case errors.Is(err, store.ErrReadinessScanNotFound):
		return readiness.ScanUnauthorized, nil
	case err != nil:
		return 0, err
	case sc.State != store.ScanRunning:
		return readiness.ScanConflict, nil
	}
	hash := hashToken(token)
	match, err := e.Store.ReadinessTokenIs(ctx, id, hash)
	switch {
	case err != nil:
		return 0, err
	case !match:
		return readiness.ScanUnauthorized, nil
	}

	job, errText := e.scanResult(ctx, sc, body)
	var raw json.RawMessage
	if job != nil {
		if raw, err = json.Marshal(job); err != nil {
			return 0, err
		}
	}
	ok, err := e.Store.FinishReadinessScan(ctx, id, hash, raw, errText, e.now())
	if err != nil {
		return 0, err
	}
	if !ok {
		// Ended, or deleted, since it was read.
		if again, err := e.Store.ReadinessScan(ctx, id); err == nil && again.State != store.ScanRunning {
			return readiness.ScanConflict, nil
		}
		return readiness.ScanUnauthorized, nil
	}
	e.wakeScans()
	e.log().Info("readiness scan: result taken", "scan", id, "instance", sc.Instance, "project", sc.Project,
		"failed", errText != "")
	if job != nil {
		e.readinessItems(ctx, sc.Instance, sc.Project, job)
	}
	return readiness.ScanStored, nil
}

// ScanTooLarge fails the scan whose Job posted a body over
// readiness.MaxResultBytes, when token is the scan's.
func (e *Engine) ScanTooLarge(ctx context.Context, id, token string) error {
	ok, err := e.Store.FinishReadinessScan(ctx, id, hashToken(token), nil, msgScanBodyLarge, e.now())
	if ok {
		e.wakeScans()
	}
	return err
}

// scanResult reads a Job's body into the result to store, or the error text
// to fail the scan with.
func (e *Engine) scanResult(ctx context.Context, sc store.ReadinessScan, body []byte) (*readiness.JobResult, string) {
	log := e.log().With("scan", sc.ID)
	var res readiness.JobResult
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		log.Warn("readiness scan: a Job sent a body that is not a result")
		return nil, msgScanUnusable
	}
	if res.Version != readiness.ResultVersion {
		log.Warn("readiness scan: a Job sent a result of another version", "version", res.Version)
		return nil, msgScanUnusable
	}
	secrets, err := e.scanSecrets(ctx, sc)
	if err != nil {
		log.Error("readiness scan: reading the held secrets failed", "error", e.redact(err.Error()))
		return nil, msgScanSecrets
	}
	if res.Error != "" {
		text, ok := untrustedText(res.Error, secrets)
		if !ok {
			text = msgScanFailedWithheld
		}
		return nil, text
	}
	if res.Commit == "" || res.Commit != sc.Commit {
		log.Warn("readiness scan: a Job sent a result of another commit")
		return nil, msgScanUnusable
	}
	clean, ok := cleanJobResult(res, secrets)
	if !ok {
		log.Warn("readiness scan: a Job sent a result with a bad name, folder or too many skills")
		return nil, msgScanUnusable
	}
	return &clean, ""
}

// scanSecrets is what a scan's text is checked against: every secret
// pergent.sh holds, and the repository's deploy token, which the Job's clone
// holds. It fails closed.
func (e *Engine) scanSecrets(ctx context.Context, sc store.ReadinessScan) ([]string, error) {
	secrets, err := e.registry().HeldSecrets(ctx)
	if err != nil {
		return nil, err
	}
	p, found, err := e.Store.ProjectByPath(ctx, sc.Instance, sc.Project)
	if err != nil {
		return nil, err
	}
	if found && len(p.DeployTokenSealed) > 0 && e.Box != nil {
		if tok, err := e.Box.Open(p.DeployTokenSealed); err == nil {
			secrets = append(secrets, string(tok))
		}
	}
	return secrets, nil
}

// untrustedText is s as one line of at most 500 bytes (skillfetch.JobText);
// ok is false when that is empty or s holds a secret.
func untrustedText(s string, secrets []string) (string, bool) {
	t := skillfetch.JobText(s)
	return t, t != "" && !holdsSecret(s, secrets) && !holdsSecret(t, secrets)
}

func holdsSecret(s string, secrets []string) bool {
	return inputs.ContainsSecret([]byte(s), secrets) || gitHubToken.MatchString(s)
}

// exactName reports whether s, a path or name a Job sent, can be shown as it
// is: not empty, at most limit bytes, already one plain line, no secret.
func exactName(s string, limit int, secrets []string) bool {
	return s != "" && len(s) <= limit && skillfetch.JobText(s) == s && !holdsSecret(s, secrets)
}

// skillFolders are the folders a skill fit may name.
var skillFolders = append([]string{inputs.SourceLibrary}, inputs.RepoFolders...)

// noiseWhys are the reasons a noise file may give.
var noiseWhys = []string{readiness.WhyGeneratedName, readiness.WhyBuildOutput, readiness.WhyLarge}

// cleanJobResult is what is kept of a result a Job posted. ok is false when
// it can't be used: a context file or skill with a name that is not plain or
// holds a secret, a skill of no known folder, or more than maxResultSkills
// skills (dropping one would misstate the size). Otherwise a problem holding
// a secret is withheld, noise paths and languages that are not plain are
// dropped (a noise file is still counted), lists are capped and negative
// numbers are 0.
func cleanJobResult(r readiness.JobResult, secrets []string) (readiness.JobResult, bool) {
	out := readiness.JobResult{
		Version: r.Version, Commit: r.Commit,
		Files: max(r.Files, 0), SourceFiles: max(r.SourceFiles, 0),
		NoiseTotal:        max(r.NoiseTotal, len(r.Noise), 0),
		InstructionsBytes: max(r.InstructionsBytes, 0),
	}

	c := r.Context
	if c.AgentFile != "" && c.AgentFile != "AGENTS.md" && c.AgentFile != "CLAUDE.md" {
		return readiness.JobResult{}, false
	}
	for _, p := range []string{c.Readme, c.Contributing, c.CodeOwners} {
		if p != "" && !exactName(p, readiness.MaxPathBytes, secrets) {
			return readiness.JobResult{}, false
		}
	}
	out.Context = c
	out.Context.CodeOwnerRules = max(c.CodeOwnerRules, 0)

	if len(r.Skills) > maxResultSkills {
		return readiness.JobResult{}, false
	}
	for _, f := range r.Skills {
		if !exactName(f.Name, maxNameBytes, secrets) || !slices.Contains(skillFolders, f.Folder) {
			return readiness.JobResult{}, false
		}
		f.Files, f.Bytes = max(f.Files, 0), max(f.Bytes, 0)
		if f.Problem != "" {
			text, ok := untrustedText(f.Problem, secrets)
			if !ok {
				text = msgSkillProblemWithheld
			}
			f.Problem = text
		}
		out.Skills = append(out.Skills, f)
	}

	for _, l := range r.Languages {
		if len(out.Languages) == maxResultLanguages {
			break
		}
		if exactName(l.Name, maxNameBytes, secrets) {
			l.Files = max(l.Files, 0)
			out.Languages = append(out.Languages, l)
		}
	}

	for _, n := range r.Noise {
		if len(out.Noise) == readiness.MaxNoisePaths {
			break
		}
		if !exactName(n.Path, readiness.MaxPathBytes, secrets) || !slices.Contains(noiseWhys, n.Why) {
			continue
		}
		n.Size = max(n.Size, 0)
		var skills []string
		for _, s := range n.Skills {
			if len(skills) == maxNoiseSkills {
				break
			}
			if exactName(s, maxNameBytes, secrets) {
				skills = append(skills, s)
			}
		}
		n.Skills = skills
		out.Noise = append(out.Noise, n)
	}
	return out, true
}

// readinessItems sets or clears the Needs attention items of a repository
// from its scan's file result and the skills turned on now: no skill
// covering it, no project context, and skills plus instructions over
// 900 KiB, the last only when the size is known (else it is left as it is).
// A repository no longer switched on is left alone; one switched off while
// this ran loses the items again.
func (e *Engine) readinessItems(ctx context.Context, instance, project string, job *readiness.JobResult) {
	log := e.log().With("instance", instance, "project", project)
	eff, ok, err := e.settings().Get(ctx, instance, project)
	if err != nil {
		log.Info("readiness scan: couldn't read the repository's settings", "error", e.redact(err.Error()))
		return
	}
	if !ok {
		return
	}
	library, err := e.assignedSkills(ctx, eff.Skills)
	if err != nil {
		log.Info("readiness scan: couldn't read the default skills", "error", e.redact(err.Error()))
		return
	}
	list, err := e.Store.LibrarySkills(ctx)
	if err != nil {
		log.Info("readiness scan: couldn't read the skill library", "error", e.redact(err.Error()))
		return
	}
	sizes := make(map[string]int, len(list))
	for _, s := range list {
		sizes[s.Name] = s.Size
	}
	f := readiness.ComputeFlags(job, library, eff.RepoSkills, sizes)
	e.toggleAttention(ctx, instance, project, attNoSkillCoverage, f.NoCoverage, noSkillCoverageNote)
	e.toggleAttention(ctx, instance, project, attNoProjectContext, f.NoContext, noProjectContextNote)
	if f.BudgetKnown {
		e.toggleAttention(ctx, instance, project, attInputsOverBudget, f.Budget > inputs.ConfigMapLimit, overBudgetNote(f.Budget))
	}
	// Switching off deletes the scans, then the items: with no scan left
	// now, the items just set went in after that, so they go again.
	if latest, active, err := e.Store.ReadinessScans(ctx, instance, project); err == nil && latest == nil && active == nil {
		for _, kind := range []string{attNoSkillCoverage, attNoProjectContext, attInputsOverBudget} {
			e.clearAttention(ctx, instance, project, kind)
		}
	}
}

// toggleAttention sets kind with message when on, and clears it when not.
func (e *Engine) toggleAttention(ctx context.Context, instance, project, kind string, on bool, message string) {
	if on {
		e.setAttention(ctx, instance, project, kind, message)
		return
	}
	e.clearAttention(ctx, instance, project, kind)
}

// RecheckReadiness asks the scan loop to re-judge a repository's last scan
// against its skills now, with no Job and no platform call: the dashboard
// calls it when a repository's skills are saved. project "" rechecks every
// repository that follows the default list (its skills are null), on
// instance or, with instance "", on every instance: the default list was
// saved. It never blocks; requests are merged until the loop takes them.
func (e *Engine) RecheckReadiness(instance, project string) {
	if !e.Scans {
		return
	}
	e.recheckMu.Lock()
	if project == "" {
		if e.recheckDefaults == nil {
			e.recheckDefaults = map[string]bool{}
		}
		e.recheckDefaults[instance] = true
	} else {
		if e.rechecks == nil {
			e.rechecks = map[repoKey]RepoKey{}
		}
		e.rechecks[keyOf(instance, project)] = RepoKey{Instance: instance, Project: project}
	}
	e.recheckMu.Unlock()
	select {
	case e.recheckCh() <- struct{}{}:
	default:
	}
}

// RecheckAllReadiness asks the scan loop to re-judge every repository's last
// done scan against its skills now, with no Job and no platform call: the
// dashboard calls it when a library skill's text changes (an edit, or an
// update from its source), which changes its size for every repository that
// uses it. It never blocks; requests are merged until the loop takes them.
func (e *Engine) RecheckAllReadiness() {
	if !e.Scans {
		return
	}
	e.recheckMu.Lock()
	e.recheckAll = true
	e.recheckMu.Unlock()
	select {
	case e.recheckCh() <- struct{}{}:
	default:
	}
}

// recheckConfigRepos asks for a recheck of every config-file repository: the
// config's skills change only with a restart, so RunScans calls it once at
// startup. A failure to list them is logged; the items then follow the next
// scan.
func (e *Engine) recheckConfigRepos(ctx context.Context) {
	repos, err := e.settings().List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.log().Info("readiness recheck: couldn't list the repositories", "error", e.redact(err.Error()))
		}
		return
	}
	for _, r := range repos {
		if r.Source == settings.SourceConfig && !e.removing(r.Instance) {
			e.RecheckReadiness(r.Instance, r.Project)
		}
	}
}

func (e *Engine) recheckCh() chan struct{} {
	e.recheckOnce.Do(func() { e.recheckReq = make(chan struct{}, 1) })
	return e.recheckReq
}

// runRechecks re-judges the repositories RecheckReadiness and
// RecheckAllReadiness asked for from
// their newest done scan, which a failed scan after it keeps; a repository
// with no done scan is left as it is.
func (e *Engine) runRechecks(ctx context.Context) {
	e.recheckMu.Lock()
	keys, defaults, all := e.rechecks, e.recheckDefaults, e.recheckAll
	e.rechecks, e.recheckDefaults, e.recheckAll = nil, nil, false
	e.recheckMu.Unlock()
	if keys == nil {
		keys = map[repoKey]RepoKey{}
	}
	if all || len(defaults) > 0 {
		repos, err := e.settings().List(ctx)
		if err != nil {
			e.log().Info("readiness recheck: couldn't list the repositories", "error", e.redact(err.Error()))
		}
		for _, r := range repos {
			if all || (r.Skills == nil && (defaults[""] || defaults[r.Instance])) {
				keys[keyOf(r.Instance, r.Project)] = RepoKey{Instance: r.Instance, Project: r.Project}
			}
		}
	}
	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}
		e.recheck(ctx, k.Instance, k.Project)
	}
}

func (e *Engine) recheck(ctx context.Context, instance, project string) {
	done, err := e.Store.DoneReadinessScan(ctx, instance, project)
	if err != nil {
		e.log().Info("readiness recheck: couldn't read the scans", "instance", instance, "project", project,
			"error", e.redact(err.Error()))
		return
	}
	if done == nil || done.Job == nil {
		return
	}
	var job readiness.JobResult
	if err := json.Unmarshal(done.Job, &job); err != nil {
		e.log().Warn("readiness recheck: the stored result can't be read", "scan", done.ID, "error", err.Error())
		return
	}
	e.readinessItems(ctx, instance, project, &job)
}
