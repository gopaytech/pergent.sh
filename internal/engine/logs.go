package engine

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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

// captureLog stores the redacted tail of the log of run's finished Job, once
// per run: a failure is logged and not tried again, and never touches the
// run. The values to hide are the ones in the Job's own Secret (its clone,
// model and result tokens) and the engine's model key; with the Secret
// unreadable nothing says what to hide, so nothing is stored.
func (e *Engine) captureLog(ctx context.Context, run store.Run, log *slog.Logger) {
	e.captureLogWithin(ctx, run, log, logTimeout)
}

// captureLogWithin is captureLog bounded by timeout.
func (e *Engine) captureLogWithin(ctx context.Context, run store.Run, log *slog.Logger, timeout time.Duration) {
	if !e.firstLogTry(run.ID) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	secrets, err := e.Launcher.JobSecrets(ctx, run.JobName)
	if err != nil {
		log.Info("not keeping the review job's log: its secrets can't be read", "error", e.redact(err.Error()))
		return
	}
	hide := []string{secrets.GitToken, secrets.LLMKey, secrets.ResultToken, e.LLMKey}
	// More than is kept is read and hidden in first, so a secret across the
	// point where the kept tail starts is hidden whole, not left in part.
	read := logKeepBytes + logMargin(hide)
	text, err := e.Launcher.Logs(ctx, run.JobName, read)
	if err != nil {
		log.Info("couldn't read the review job's log", "error", e.redact(err.Error()))
		return
	}
	// A read that may have been cut starts within a line, perhaps within a
	// secret that no longer matches; hiding the secrets after it can leave
	// less than is kept, so that line is dropped here, not by keepTail.
	if len(text) > read-utf8.UTFMax {
		text = afterFirstLine(text)
	}
	text = keepTail(store.CleanText(redactAll(text, hide...)), logKeepBytes)
	if err := e.Store.PutRunLog(ctx, run.ID, text); err != nil {
		log.Info("couldn't store the review job's log", "error", e.redact(err.Error()))
	}
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
			e.captureLog(ctx, run, log)
		}
	}
	return nil
}

// pruneRunLogs deletes the logs older than the retention.
func (e *Engine) pruneRunLogs(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	n, err := e.Store.PruneRunLogs(ctx, e.now().Add(-logRetention))
	if err != nil {
		if ctx.Err() == nil {
			e.log().Error("couldn't prune old review logs", "error", e.redact(err.Error()))
		}
		return
	}
	if n > 0 {
		e.log().Info("pruned old review logs", "logs", n)
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
