package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// putJobSecret stores the Secret of run id's Job, as dispatch would.
func putJobSecret(t *testing.T, env *testEnv, id string, s kube.Secrets) {
	t.Helper()
	p := kube.JobParams{Name: kube.JobName(id), Namespace: testNamespace, RunID: id}
	if _, err := env.Kube.CoreV1().Secrets(testNamespace).Create(env.ctx, kube.BuildSecret(p, &batchv1.Job{}, s), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// serveLog makes every pod log read return text, and counts the reads.
func serveLog(env *testEnv, text string) *atomic.Int32 {
	var reads atomic.Int32
	env.Engine.Launcher.LogStream = func(context.Context, string, string, int64) (io.ReadCloser, error) {
		reads.Add(1)
		return io.NopCloser(strings.NewReader(text)), nil
	}
	return &reads
}

func storedLog(t *testing.T, env *testEnv, id string) (string, bool) {
	t.Helper()
	text, found, err := env.Store.RunLog(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return text, found
}

func TestWatchStoresLogOnce(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	reads := serveLog(env, "review started\nreview done\n")

	// A running run whose Job finished: logged at once, even inside the grace
	// period before it is reported.
	running := env.Dispatched(1, sha('1'))
	putJob(t, env, running, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-time.Second)))
	putJobSecret(t, env, running, kube.Secrets{GitToken: "git-token-1", LLMKey: "llm-key-1", ResultToken: "result-token-1"})
	// A run whose result arrived: its Job finishes after the run left "running".
	reported := env.Report(2, sha('2'), []byte(`{"status":"completed"}`))
	putJob(t, env, reported, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-time.Second)))
	putJobSecret(t, env, reported, kube.Secrets{GitToken: "git-token-1", LLMKey: "llm-key-1", ResultToken: "result-token-1"})
	// A reported run whose Job is still running: not yet.
	busy := env.Report(3, sha('3'), []byte(`{"status":"completed"}`))
	putJob(t, env, busy, now.Add(-time.Minute), nil, nil)
	putJobSecret(t, env, busy, kube.Secrets{GitToken: "git-token-1", LLMKey: "llm-key-1", ResultToken: "result-token-1"})

	for i := 0; i < 3; i++ {
		watch(t, env)
	}
	for _, id := range []string{running, reported} {
		if text, found := storedLog(t, env, id); !found || text != "review started\nreview done\n" {
			t.Errorf("log of %s = %q, %v; want it stored", id, text, found)
		}
	}
	if _, found := storedLog(t, env, busy); found {
		t.Errorf("the log of a Job that has not finished was stored")
	}
	if n := reads.Load(); n != 2 {
		t.Errorf("pod logs read %d times over 3 passes; want 2 (once per finished run)", n)
	}
	wantRunning(t, env, running)
}

func TestWatchLogFailureIsHarmless(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	var reads atomic.Int32
	env.Engine.Launcher.LogStream = func(context.Context, string, string, int64) (io.ReadCloser, error) {
		reads.Add(1)
		return nil, errors.New("container pergent is not available")
	}
	id := env.Report(1, sha('1'), []byte(`{"status":"completed"}`))
	putJob(t, env, id, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-time.Second)))
	putJobSecret(t, env, id, kube.Secrets{GitToken: "git-token-1", LLMKey: "llm-key-1", ResultToken: "result-token-1"})

	watch(t, env)
	watch(t, env)
	if _, found := storedLog(t, env, id); found {
		t.Errorf("a log was stored though the read failed")
	}
	if r := env.Run(id); r.State != store.Reported || r.FailureReason != "" {
		t.Errorf("run = %s, %q; the log failure must not touch it", r.State, r.FailureReason)
	}
	if !strings.Contains(env.Logs(), "level=INFO") || !strings.Contains(env.Logs(), "pergent is not available") {
		t.Errorf("the failure was not logged at Info:\n%s", env.Logs())
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("the read was tried %d times; want once per run", n)
	}
}

// A Job already deleted can't give its log, and is not asked again.
func TestWatchNoLogWithoutJob(t *testing.T) {
	env := newTestEnv(t)
	reads := serveLog(env, "x")
	id := env.Report(1, sha('1'), []byte(`{"status":"completed"}`))
	watch(t, env)
	watch(t, env)
	if _, found := storedLog(t, env, id); found || reads.Load() != 0 {
		t.Errorf("log stored (%v) or read (%d) for a run without a Job", found, reads.Load())
	}
}

func TestRunLogRedacted(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	const clone, result, llm = "glpat-clone-token-123", "result-token-abc", "sk-engine-llm-key"
	env.Engine.LLMKey = llm
	serveLog(env, "cloning with "+clone+"\nAuthorization: Bearer "+result+"\nkey="+testLLMKey+" and "+llm+"\nclean line\n")

	id := env.Report(1, sha('1'), []byte(`{"status":"completed"}`))
	putJob(t, env, id, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-time.Second)))
	// The Job's own Secret is where this run's clone and result tokens are.
	putJobSecret(t, env, id, kube.Secrets{GitToken: clone, LLMKey: testLLMKey, ResultToken: result})

	watch(t, env)
	text, found := storedLog(t, env, id)
	if !found {
		t.Fatal("no log stored")
	}
	for _, secret := range []string{clone, result, testLLMKey, llm} {
		if strings.Contains(text, secret) {
			t.Errorf("stored log still has %q:\n%s", secret, text)
		}
	}
	if strings.Count(text, "[REDACTED]") != 4 || !strings.Contains(text, "clean line") {
		t.Errorf("stored log = %q; want four [REDACTED] and the clean line kept", text)
	}
}

// A secret across the point where the kept tail starts is hidden whole: no
// end of it is kept. The tail starts at a whole line.
func TestRunLogSecretAcrossCut(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	const clone = "glpat-clone-token-START-0123456789-ENDSUFFIX"
	// The kept tail starts 20 bytes before the token's end.
	tail := clone[len(clone)-20:] + "\n" + strings.Repeat("x", 99) + "\n"
	rest := strings.Repeat("y", logKeepBytes-len(tail)-1) + "\n"
	serveLog(env, strings.Repeat("early line\n", 1000)+"cloning with "+clone[:len(clone)-20]+tail+rest)

	id := env.Report(1, sha('1'), []byte(`{"status":"completed"}`))
	putJob(t, env, id, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-time.Second)))
	putJobSecret(t, env, id, kube.Secrets{GitToken: clone, LLMKey: testLLMKey, ResultToken: "result-token-abc"})

	watch(t, env)
	text, found := storedLog(t, env, id)
	if !found {
		t.Fatal("no log stored")
	}
	if strings.Contains(text, "ENDSUFFIX") || strings.Contains(text, "DACTED") {
		t.Errorf("the stored log keeps part of the token or its mark: %q", text[:min(len(text), 80)])
	}
	if len(text) > logKeepBytes || !strings.HasPrefix(text, strings.Repeat("x", 99)+"\n") {
		t.Errorf("stored log is %d bytes starting %q; want at most %d from the first whole line", len(text), text[:min(len(text), 40)], logKeepBytes)
	}
}

// A tail without a line break after the cut is kept as cut, on a character
// boundary.
func TestRunLogCutWithoutLineBreak(t *testing.T) {
	text := strings.Repeat("é", logKeepBytes) // 2 bytes each
	got := keepTail(text, logKeepBytes)
	if len(got) > logKeepBytes || len(got) < logKeepBytes-1 || !utf8.ValidString(got) {
		t.Errorf("keepTail gave %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if short := "a\nb"; keepTail(short, logKeepBytes) != short {
		t.Errorf("a short log is changed")
	}
}

// Without the Job's Secret nothing says which tokens to hide, so the log is
// not stored.
func TestRunLogNotStoredWithoutSecret(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	serveLog(env, "cloning with a-secret\n")
	id := env.Report(1, sha('1'), []byte(`{"status":"completed"}`))
	putJob(t, env, id, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-time.Second)))

	watch(t, env)
	if _, found := storedLog(t, env, id); found {
		t.Errorf("a log was stored without knowing the run's secrets")
	}
}

func TestPruneRunLogsOnce(t *testing.T) {
	env := newTestEnv(t)
	id := env.Report(1, sha('1'), []byte(`{}`))
	if err := env.Store.PutRunLog(env.ctx, id, "log"); err != nil {
		t.Fatal(err)
	}
	// 29 days on, the log stays; 31 days on, it goes.
	env.SetNow(time.Now().Add(29 * 24 * time.Hour))
	env.Engine.pruneRunLogs(env.ctx)
	if _, found := storedLog(t, env, id); !found {
		t.Errorf("a 29-day-old log was pruned")
	}
	env.SetNow(time.Now().Add(31 * 24 * time.Hour))
	env.Engine.pruneRunLogs(env.ctx)
	if _, found := storedLog(t, env, id); found {
		t.Errorf("a 31-day-old log was kept")
	}
}
