package engine

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// logKeepBytes is how much of the end of a Job's log is kept.
	logKeepBytes = 200 * 1024
	// logRetention is how long a stored log is kept.
	logRetention = 30 * 24 * time.Hour
	// logWindow is how long after its result a run's Job is still looked at
	// for its log. Jobs are deleted an hour after they finish.
	logWindow = 3 * time.Hour
	// logTimeout bounds taking one log, so the watcher never waits long.
	logTimeout = 30 * time.Second

	redacted = "[REDACTED]"
)

// captureLog stores the redacted tail of the log of run's finished Job, once
// per run: a failure is logged and not tried again, and never touches the
// run. The values to hide are the ones in the Job's own Secret (its clone,
// model and result tokens) and the engine's model key; with the Secret
// unreadable nothing says what to hide, so nothing is stored.
func (e *Engine) captureLog(ctx context.Context, run store.Run, log *slog.Logger) {
	if !e.firstLogTry(run.ID) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, logTimeout)
	defer cancel()

	secrets, err := e.Launcher.JobSecrets(ctx, run.JobName)
	if err != nil {
		log.Info("not keeping the review job's log: its secrets can't be read", "error", e.redact(err.Error()))
		return
	}
	text, err := e.Launcher.Logs(ctx, run.JobName, logKeepBytes)
	if err != nil {
		log.Info("couldn't read the review job's log", "error", e.redact(err.Error()))
		return
	}
	text = redactAll(text, secrets.GitToken, secrets.LLMKey, secrets.ResultToken, e.LLMKey)
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
