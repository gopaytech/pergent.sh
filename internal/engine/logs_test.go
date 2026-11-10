package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

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

// containerLogs serves each container's log from a map (a container without
// one has no log to give) and counts every read.
type containerLogs struct {
	mu    sync.Mutex
	text  map[string]string
	reads int
}

func serveContainerLogs(env *testEnv, text map[string]string) *containerLogs {
	c := &containerLogs{text: text}
	env.Engine.Launcher.LogStream = func(_ context.Context, _, container string, _ int64) (io.ReadCloser, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.reads++
		s, ok := c.text[container]
		if !ok {
			return nil, errors.New("container " + container + " is not available")
		}
		return io.NopCloser(strings.NewReader(s)), nil
	}
	return c
}

func (c *containerLogs) Reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// setCheckoutEnv gives run id's Job a checkout container with the repository's
// URL and the linked repositories it clones, as dispatch would.
func setCheckoutEnv(t *testing.T, env *testEnv, id, repoURL, linked string) {
	t.Helper()
	job := env.Job(id)
	job.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "checkout", Env: []corev1.EnvVar{
		{Name: "REPO_URL", Value: repoURL}, {Name: "LINKED_CLONES", Value: linked},
	}}}
	if _, err := env.Kube.BatchV1().Jobs(testNamespace).Update(env.ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// reportLinked is env.Report for a run with a linked repository (links are
// recorded while a run is running).
func reportLinked(t *testing.T, env *testEnv, iid int64, head string) string {
	t.Helper()
	id := env.Dispatched(iid, head)
	if err := env.Store.SetRunLinks(env.ctx, id, []store.Link{{Target: "acme/api", CanBlock: true}}); err != nil {
		t.Fatal(err)
	}
	if out, err := env.Store.StoreResult(env.ctx, id, "seed-token", []byte(`{"status":"completed"}`), env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	return id
}

var jobSecrets = kube.Secrets{GitToken: "git-token-1", LLMKey: "llm-key-1", ResultToken: "result-token-1"}

// A Job that completed keeps no diagnostics, unless a linked repository's
// clone failed: then its clone log is kept, and no pod state. [CI]
func TestDiagnosticsOnlyForFailures(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	const cloneLog = "cloned linked-2\ncould not clone linked-1\n"
	plain := env.Report(1, sha('1'), []byte(`{"status":"completed"}`))
	linked := reportLinked(t, env, 2, sha('2'))
	fine := reportLinked(t, env, 3, sha('3'))
	for _, id := range []string{plain, linked, fine} {
		putJob(t, env, id, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-time.Second)))
		putJobSecret(t, env, id, jobSecrets)
		setCheckoutEnv(t, env, id, "https://gitlab.example.com/acme/widgets.git",
			"linked-1 https://gitlab.example.com/acme/api.git\nlinked-2 https://gitlab.example.com/acme/lib.git")
	}
	env.Engine.Launcher.LogStream = func(_ context.Context, pod, container string, _ int64) (io.ReadCloser, error) {
		text := map[string]string{"pergent": "review done\n", "checkout": cloneLog}[container]
		if container == "checkout" && strings.HasPrefix(pod, kube.JobName(fine)) {
			text = "cloned linked-1\ncloned linked-2\n"
		}
		return io.NopCloser(strings.NewReader(text)), nil
	}

	watch(t, env)

	if _, found := storedDiagnostics(t, env, plain); found {
		t.Errorf("a completed Job without linked repositories kept diagnostics")
	}
	if _, found := storedDiagnostics(t, env, fine); found {
		t.Errorf("a completed Job whose linked clones all worked kept diagnostics")
	}
	d, found := storedDiagnostics(t, env, linked)
	if !found || d.CloneLog != cloneLog || d.CloneLogMasked != cloneLog || d.PodState != "" {
		t.Errorf("diagnostics = %+v, %v; want both clone logs and no pod state", d, found)
	}
}

// The masked clone log names a linked repository by its directory; the main
// repository's URL stays. Both are redacted of the Job's token first, so a
// URL that carried it is still known as the main repository's. [CI]
func TestDiagnosticsMasked(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	const token = "glpat-clone-token-123"
	cloneLog := "fatal: unable to access 'https://oauth2:" + token + "@gitlab.example.com/acme/api.git/': The requested URL returned error: 403\n" +
		"could not clone linked-1\n" +
		"the checkout of https://oauth2:" + token + "@gitlab.example.com/acme/widgets.git worked\n"
	serveContainerLogs(env, map[string]string{"pergent": "review done\n", "checkout": cloneLog})
	id := reportLinked(t, env, 1, sha('1'))
	putJob(t, env, id, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-time.Second)))
	putJobSecret(t, env, id, kube.Secrets{GitToken: token, LLMKey: "llm-key-1", ResultToken: "result-token-1"})
	setCheckoutEnv(t, env, id, "https://gitlab.example.com/acme/widgets.git", "linked-1 https://gitlab.example.com/acme/api.git")

	watch(t, env)

	d, found := storedDiagnostics(t, env, id)
	if !found {
		t.Fatal("no diagnostics stored")
	}
	wantLog := strings.ReplaceAll(cloneLog, token, "[REDACTED]")
	if d.CloneLog != wantLog {
		t.Errorf("clone log = %q, want %q", d.CloneLog, wantLog)
	}
	wantMasked := "fatal: unable to access 'linked-1/': The requested URL returned error: 403\n" +
		"could not clone linked-1\n" +
		"the checkout of https://oauth2:[REDACTED]@gitlab.example.com/acme/widgets.git worked\n"
	if d.CloneLogMasked != wantMasked {
		t.Errorf("masked clone log = %q, want %q", d.CloneLogMasked, wantMasked)
	}
	for _, text := range []string{d.CloneLog, d.CloneLogMasked} {
		if strings.Contains(text, token) {
			t.Errorf("a stored clone log has the token: %q", text)
		}
	}
	if strings.Contains(d.CloneLogMasked, "acme/api") {
		t.Errorf("the masked clone log names the linked repository: %q", d.CloneLogMasked)
	}
}

// When the Job can't say which repositories it clones, the masked clone log
// is left empty rather than unmasked. [CI]
func TestDiagnosticsMaskFailsClosed(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	serveContainerLogs(env, map[string]string{"pergent": "x\n", "checkout": "https://gitlab.example.com/acme/api.git: denied\ncould not clone linked-1\n"})
	id := reportLinked(t, env, 1, sha('1'))
	putJob(t, env, id, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-time.Second)))
	putJobSecret(t, env, id, jobSecrets)
	gets := 0
	env.Kube.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		// The first read is Inspect's; the ones after it are the masking's.
		if gets++; gets > 1 {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})

	watch(t, env)

	d, found := storedDiagnostics(t, env, id)
	if !found || d.CloneLog == "" || d.CloneLogMasked != "" {
		t.Errorf("diagnostics = %+v, %v; want the clone log and an empty masked one", d, found)
	}
}

// A run reported by its callback whose Job then failed keeps its diagnostics
// once: the next pass doesn't ask the cluster again. [CI]
func TestDiagnosticsOnceFromReportedRun(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	logs := serveContainerLogs(env, map[string]string{"pergent": "untrusted instructions\n", "checkout": "checked out\n"})
	id := env.Report(1, sha('1'), []byte(`{"status":"error","error":{"code":"untrusted_instructions","message":"untrusted instructions"}}`))
	putJob(t, env, id, now.Add(-time.Minute),
		[]corev1.ContainerStatus{terminatedStatus("checkout", 0, "Completed", now.Add(-time.Minute), now.Add(-50*time.Second))},
		[]corev1.ContainerStatus{terminatedStatus("pergent", 3, "Error", now.Add(-50*time.Second), now.Add(-time.Second))},
		jobCond(batchv1.JobFailed, "BackoffLimitExceeded", now.Add(-time.Second)))
	putJobSecret(t, env, id, jobSecrets)

	watch(t, env)

	d, found := storedDiagnostics(t, env, id)
	if !found || d.CloneLog != "checked out\n" || !strings.Contains(d.PodState, "pergent: terminated, reason Error, exit code 3") {
		t.Errorf("diagnostics = %+v, %v; want the clone log and the pod state", d, found)
	}
	reads, actions := logs.Reads(), len(env.Kube.Actions())
	watch(t, env)
	if logs.Reads() != reads || len(env.Kube.Actions()) != actions {
		t.Errorf("the second pass asked the cluster again: %d log reads (was %d), %d actions (was %d)",
			logs.Reads(), reads, len(env.Kube.Actions()), actions)
	}
	if r := env.Run(id); r.State != store.Reported || r.FailureReason != "" {
		t.Errorf("run = %s, %q; the capture must not touch it", r.State, r.FailureReason)
	}
}

// A cluster that refuses to list events gives a pod state without them and
// one warning per run, however often the watcher looks; the review fails as
// it would have. [CI]
func TestEventsForbiddenWarnsOnce(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	serveContainerLogs(env, map[string]string{})
	env.Kube.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("no access"))
	})
	var ids []string
	for iid := int64(1); iid <= 2; iid++ {
		id := env.Dispatched(iid, sha('a'))
		putJob(t, env, id, now.Add(-5*time.Minute), nil,
			[]corev1.ContainerStatus{terminatedStatus("pergent", 1, "Error", now.Add(-4*time.Minute), now.Add(-2*time.Minute))},
			jobCond(batchv1.JobFailed, "BackoffLimitExceeded", now.Add(-2*time.Minute)))
		putJobSecret(t, env, id, jobSecrets)
		ids = append(ids, id)
	}

	for i := 0; i < 3; i++ {
		watch(t, env)
	}

	for _, id := range ids {
		wantFailure(t, env, id, "the review job failed (BackoffLimitExceeded)")
		warnings := 0
		for _, line := range strings.Split(env.Logs(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "events") && strings.Contains(line, "run="+id) {
				warnings++
			}
		}
		if warnings != 1 {
			t.Errorf("run %s: %d warnings about events; want 1\n%s", id, warnings, env.Logs())
		}
		if d, found := storedDiagnostics(t, env, id); !found || !strings.Contains(d.PodState, "pergent: terminated") || strings.Contains(d.PodState, "Events") {
			t.Errorf("diagnostics = %+v, %v; want the pod state without events", d, found)
		}
	}
}

// Only checkout.sh's own line says a linked clone failed, not git's text
// that happens to hold the words.
func TestLinkedCloneFailed(t *testing.T) {
	for text, want := range map[string]bool{
		"cloned linked-1\ncould not clone linked-2\n": true,
		"could not clone linked-1":                    true,
		"cloned linked-1\n":                           false,
		"fatal: could not clone linked-1 here\n":      false,
		"":                                            false,
	} {
		if got := linkedCloneFailed(text); got != want {
			t.Errorf("linkedCloneFailed(%q) = %v, want %v", text, got, want)
		}
	}
}

// Without the Job's Secret nothing says what to hide: no diagnostics. [CI]
func TestDiagnosticsNotStoredWithoutSecret(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	serveContainerLogs(env, map[string]string{"pergent": "x\n", "checkout": "cloning with a-secret\n"})
	id := env.Dispatched(1, sha('1'))
	putJob(t, env, id, now.Add(-time.Minute),
		[]corev1.ContainerStatus{terminatedStatus("checkout", 128, "Error", now.Add(-time.Minute), now.Add(-2*time.Second))}, nil,
		jobCond(batchv1.JobFailed, "BackoffLimitExceeded", now.Add(-time.Second)))

	watch(t, env)
	if _, found := storedDiagnostics(t, env, id); found {
		t.Errorf("diagnostics were stored without knowing the run's secrets")
	}
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
	// The engine's key is another now than the one in the Job's Secret.
	t.Setenv("OPENCODE_API_KEY", llm)
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

// The clone log is cut as the pergent log is: a secret across the point where
// its kept tail starts is hidden whole, in both the full and the masked clone
// log. [CI]
func TestCloneLogSecretAcrossCut(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	const clone = "glpat-clone-token-START-0123456789-ENDSUFFIX"
	// The kept tail starts 20 bytes before the token's end.
	tail := clone[len(clone)-20:] + "\n" + strings.Repeat("x", 99) + "\n"
	rest := strings.Repeat("y", kube.CloneLogKeepBytes-len(tail)-1) + "\n"
	serveContainerLogs(env, map[string]string{
		"pergent":  "x\n",
		"checkout": strings.Repeat("early line\n", 1000) + "cloning with " + clone[:len(clone)-20] + tail + rest,
	})
	id := env.Dispatched(1, sha('1'))
	putJob(t, env, id, now.Add(-time.Minute),
		[]corev1.ContainerStatus{terminatedStatus("checkout", 128, "Error", now.Add(-time.Minute), now.Add(-2*time.Second))}, nil,
		jobCond(batchv1.JobFailed, "BackoffLimitExceeded", now.Add(-time.Second)))
	putJobSecret(t, env, id, kube.Secrets{GitToken: clone, LLMKey: testLLMKey, ResultToken: "result-token-abc"})

	watch(t, env)
	d, found := storedDiagnostics(t, env, id)
	if !found {
		t.Fatal("no diagnostics stored")
	}
	for name, text := range map[string]string{"clone log": d.CloneLog, "masked clone log": d.CloneLogMasked} {
		if strings.Contains(text, "ENDSUFFIX") || strings.Contains(text, "DACTED") {
			t.Errorf("the stored %s keeps part of the token or its mark: %q", name, text[:min(len(text), 80)])
		}
		if len(text) > kube.CloneLogKeepBytes || !strings.HasPrefix(text, strings.Repeat("x", 99)+"\n") {
			t.Errorf("stored %s is %d bytes starting %q; want at most %d from the first whole line",
				name, len(text), text[:min(len(text), 40)], kube.CloneLogKeepBytes)
		}
	}
}

// A secret across the start of what is read is hidden too, even when hiding
// the many secrets after it leaves less than is kept: the read starts within
// a line, and that partial line is never stored.
func TestRunLogSecretAcrossReadStart(t *testing.T) {
	env := newTestEnv(t)
	now := env.Now()
	key := "sk-model-key-" + strings.Repeat("k", 80) + "-KEYSUFFIX"
	read := logKeepBytes + logMargin([]string{key})
	// Many more copies of the key shrink the read by far more than the
	// margin.
	var body strings.Builder
	for body.Len() < read-len("-KEYSUFFIX")-200 {
		body.WriteString("calling the model with " + key + "\n")
	}
	body.WriteString(strings.Repeat("z", read-len("-KEYSUFFIX")-body.Len()-1) + "\n")
	serveLog(env, strings.Repeat("early line\n", 1000)+"using "+key+"\n"+body.String())

	id := env.Report(1, sha('1'), []byte(`{"status":"completed"}`))
	putJob(t, env, id, now.Add(-time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-time.Second)))
	putJobSecret(t, env, id, kube.Secrets{GitToken: "clone-token-abc", LLMKey: key, ResultToken: "result-token-abc"})

	watch(t, env)
	text, found := storedLog(t, env, id)
	if !found {
		t.Fatal("no log stored")
	}
	if strings.Contains(text, "KEYSUFFIX") {
		t.Errorf("the stored log keeps the end of the key: %q", text[:min(len(text), 80)])
	}
	if !strings.HasPrefix(text, "calling the model with [REDACTED]\n") {
		t.Errorf("stored log starts %q; want the first whole line", text[:min(len(text), 40)])
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
