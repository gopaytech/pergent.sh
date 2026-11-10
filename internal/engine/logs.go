package engine

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// logKeepBytes is how much of the end of a Job's log is kept.
	logKeepBytes = 200 * 1024
	// minLogMargin is the least read of a log beyond what is kept.
	minLogMargin = 4 * 1024
	// logRetention is how long a stored log is kept.
	logRetention = 30 * 24 * time.Hour
	// logWindow is how long after its result a run's Job is still looked at
	// for its log. Jobs are deleted an hour after they finish.
	logWindow = 3 * time.Hour
	// logTimeout bounds taking one log, so the watcher never waits long.
	logTimeout = 30 * time.Second
	// supersededLogTimeout bounds taking the log of a superseded run before
	// its Job is deleted, in the loop that also dispatches and publishes.
	supersededLogTimeout = 10 * time.Second

	redacted = "[REDACTED]"
)

// captureLog stores the redacted tail of the log of run's Job, once per run:
// a failure is logged and not tried again, and never touches the run. The
// values to hide are the ones in the Job's own Secret (its clone, model and
// result tokens) and the engine's model key; with the Secret unreadable
// nothing says what to hide, so nothing is stored.
//
// obs is what the watcher saw of the Job. A Job that failed or is stuck also
// keeps its diagnostics (see captureDiagnostics), once, and so does one that
// completed with a linked repository whose clone failed. The zero obs keeps
// only the log.
func (e *Engine) captureLog(ctx context.Context, run store.Run, obs kube.Observation, log *slog.Logger) {
	e.captureLogWithin(ctx, run, obs, log, logTimeout)
}

// captureLogWithin is captureLog bounded by timeout.
func (e *Engine) captureLogWithin(ctx context.Context, run store.Run, obs kube.Observation, log *slog.Logger, timeout time.Duration) {
	failed := obs.Failure.Reason != "" || obs.Stuck.Reason != ""
	takeLog := e.firstLogTry(run.ID)
	takeDiag := (failed || obs.Finished && len(run.LinkedRepos) > 0) && e.firstLogTry(diagKey(run.ID))
	if !takeLog && !takeDiag {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	hide, err := e.jobHide(ctx, run.JobName)
	if err != nil {
		log.Info("not keeping the review job's log: its secrets can't be read", "error", e.redact(err.Error()))
		return
	}
	if takeLog {
		e.capturePergentLog(ctx, run, hide, log)
	}
	if takeDiag {
		e.captureDiagnostics(ctx, run, failed, hide, log)
	}
}

// diagKey is the key of run id's diagnostics in logTried, apart from its log.
func diagKey(id string) string { return "diag:" + id }

// jobHide is what to hide in text read from a Job: the values of its own
// Secret and the engine's model keys.
func (e *Engine) jobHide(ctx context.Context, jobName string) ([]string, error) {
	secrets, err := e.Launcher.JobSecrets(ctx, jobName)
	if err != nil {
		return nil, err
	}
	return append([]string{secrets.GitToken, secrets.LLMKey, secrets.ResultToken}, e.registry().LLMKeys()...), nil
}

// capturePergentLog stores the redacted tail of the pergent container's log.
func (e *Engine) capturePergentLog(ctx context.Context, run store.Run, hide []string, log *slog.Logger) {
	text, err := readRedacted(hide, logKeepBytes, func(n int) (string, error) {
		return e.Launcher.Logs(ctx, run.JobName, n)
	})
	if err != nil {
		log.Info("couldn't read the review job's log", "error", e.redact(err.Error()))
		return
	}
	if err := e.Store.PutRunLog(ctx, run.ID, text); err != nil {
		log.Info("couldn't store the review job's log", "error", e.redact(err.Error()))
	}
}

// captureDiagnostics stores what a failed Job left, redacted with hide: the
// checkout container's clone log, the same log with the linked repositories
// masked, and, for a Job that failed or is stuck, its pod state. A Job that
// completed keeps them only when a linked repository's clone failed. A part
// that can't be read is logged and left empty.
func (e *Engine) captureDiagnostics(ctx context.Context, run store.Run, failed bool, hide []string, log *slog.Logger) {
	var d store.Diagnostics
	text, err := readRedacted(hide, kube.CloneLogKeepBytes, func(n int) (string, error) {
		return e.Launcher.CheckoutLog(ctx, run.JobName, n)
	})
	if err != nil {
		log.Info("couldn't read the review job's clone log", "error", e.redact(err.Error()))
	}
	if !failed && !linkedCloneFailed(text) {
		return
	}
	if text != "" {
		d.CloneLog = text
		d.CloneLogMasked = e.maskClones(ctx, run, text, log)
	}
	if failed {
		state, forbidden, err := e.Launcher.PodState(ctx, run.JobName)
		if err != nil {
			log.Info("couldn't read the review job's pod state", "error", e.redact(err.Error()))
		}
		d.PodState = redactAll(state, hide...)
		if forbidden {
			// Once per run: the diagnostics are taken once.
			log.Warn("the review job's pod state has no events: the cluster refused to list them (the Role needs list on events)")
		}
	}
	if d == (store.Diagnostics{}) {
		return
	}
	if err := e.Store.PutRunDiagnostics(ctx, run.ID, d); err != nil {
		log.Info("couldn't store the review job's diagnostics", "error", e.redact(err.Error()))
	}
}

// maskClones is a redacted clone log with the run's linked repositories
// masked (kube.MaskLinked). When the Job can't say which repositories it
// clones, nothing is safe to show, so it is "".
func (e *Engine) maskClones(ctx context.Context, run store.Run, text string, log *slog.Logger) string {
	clones, err := e.Launcher.LinkedClones(ctx, run.JobName)
	if err != nil {
		log.Info("not keeping the masked clone log: the review job's linked repositories can't be read", "error", e.redact(err.Error()))
		return ""
	}
	repoURL, err := e.Launcher.CheckoutRepoURL(ctx, run.JobName)
	if err != nil {
		log.Info("not keeping the masked clone log: the review job's repository can't be read", "error", e.redact(err.Error()))
		return ""
	}
	return kube.MaskLinked(text, clones, repoURL)
}

// linkedCloneFailed reports whether a clone log says a linked repository's
// clone failed (checkout.sh's "could not clone linked-N").
func linkedCloneFailed(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "could not clone ") {
			return true
		}
	}
	return false
}

// readRedacted reads the tail of a log with read(n), hides the secrets in
// it and keeps its last keep bytes.
func readRedacted(hide []string, keep int, read func(n int) (string, error)) (string, error) {
	// More than is kept is read and hidden in first, so a secret across the
	// point where the kept tail starts is hidden whole, not left in part.
	n := keep + logMargin(hide)
	text, err := read(n)
	if err != nil {
		return "", err
	}
	// A read that may have been cut starts within a line, perhaps within a
	// secret that no longer matches; hiding the secrets after it can leave
	// less than is kept, so that line is dropped here, not by keepTail.
	if len(text) > n-utf8.UTFMax {
		text = afterFirstLine(text)
	}
	return keepTail(store.CleanText(redactAll(text, hide...)), keep), nil
}

// redactDetail hides the Job's secrets in a failure's admin-only detail (a
// scheduler or event message). With the Job's Secret unreadable nothing says
// what to hide, so the detail is dropped rather than kept raw.
func (e *Engine) redactDetail(ctx context.Context, run store.Run, detail string, log *slog.Logger) string {
	if detail == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, logTimeout)
	defer cancel()
	hide, err := e.jobHide(ctx, run.JobName)
	if err != nil {
		log.Info("not keeping the failure's detail: the review job's secrets can't be read", "error", e.redact(err.Error()))
		return ""
	}
	return redactAll(detail, hide...)
}

// firstLogTry reports whether run id's log has not been tried yet, and
// records that it is being. Entries older than the log window go, as the
// runs they stand for are no longer looked at.
func (e *Engine) firstLogTry(id string) bool {
	now := e.now()
	e.logMu.Lock()
	defer e.logMu.Unlock()
	if e.logTried == nil {
		e.logTried = make(map[string]time.Time)
	}
	for k, at := range e.logTried {
		if now.Sub(at) > logWindow {
			delete(e.logTried, k)
		}
	}
	if _, done := e.logTried[id]; done {
		return false
	}
	e.logTried[id] = now
	return true
}

func (e *Engine) logTriedAlready(id string) bool {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	_, done := e.logTried[id]
	return done
}

// captureReportedLogs takes the log of every recently reported run whose Job
// has finished and still exists. Such a run is no longer "running", so the
// watch of running runs doesn't see it.
func (e *Engine) captureReportedLogs(ctx context.Context, now time.Time) error {
	runs, err := e.Store.RunsToLog(ctx, now.Add(-logWindow))
	if err != nil {
		return err
	}
	for _, run := range runs {
		if ctx.Err() != nil {
			break
		}
		if e.logTriedAlready(run.ID) {
			continue
		}
		log := e.log().With("run", run.ID, "instance", run.MR.Instance, "project", run.ProjectPath,
			"mr", run.MR.IID, "head", run.HeadSHA, "job", run.JobName)
		obs, err := e.Launcher.Inspect(ctx, run.JobName, now)
		if err != nil {
			log.Info("couldn't inspect the review job for its log", "error", e.redact(err.Error()))
			continue
		}
		switch {
		case !obs.Exists:
			// Gone with its pods: there is no log to take.
			e.firstLogTry(run.ID)
		case obs.Finished:
			e.captureLog(ctx, run, obs, log)
		}
	}
	return nil
}

// pruneRunLogs deletes the logs and diagnostics older than the retention.
func (e *Engine) pruneRunLogs(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	n, err := e.Store.PruneRunLogs(ctx, e.now().Add(-logRetention))
	if err != nil {
		if ctx.Err() == nil {
			e.log().Error("couldn't prune old review logs and diagnostics", "error", e.redact(err.Error()))
		}
		return
	}
	if n > 0 {
		e.log().Info("pruned old review logs and diagnostics", "rows", n)
	}
}

// logMargin is how much more than is kept is read of a log: at least
// minLogMargin, and at least the longest secret.
func logMargin(secrets []string) int {
	n := minLogMargin
	for _, s := range secrets {
		n = max(n, len(s))
	}
	return n
}

// keepTail returns the last keep bytes of text, starting at the first whole
// line in them; with no line break in them, as cut, on a character
// boundary.
func keepTail(text string, keep int) string {
	if len(text) <= keep {
		return text
	}
	tail := text[len(text)-keep:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 {
		return tail[i+1:]
	}
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail
}

// afterFirstLine returns text after its first line break; with none, nothing.
func afterFirstLine(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[i+1:]
	}
	return ""
}

// redactAll replaces every non-empty secret in text with [REDACTED], longest
// first so one that contains another is hidden whole.
func redactAll(text string, secrets ...string) string {
	var list []string
	for _, s := range secrets {
		if s != "" {
			list = append(list, s)
		}
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, s := range list {
		text = strings.ReplaceAll(text, s, redacted)
	}
	return text
}
