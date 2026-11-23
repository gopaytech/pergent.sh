package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func watch(t *testing.T, env *testEnv) {
	t.Helper()
	if err := env.Engine.WatchOnce(env.ctx); err != nil {
		t.Fatalf("WatchOnce: %v", err)
	}
}

// putJob stores run id's Job with the given conditions, and a pod of it
// created at podCreated with the given container statuses.
func putJob(t *testing.T, env *testEnv, id string, podCreated time.Time, init, main []corev1.ContainerStatus, conds ...batchv1.JobCondition) {
	t.Helper()
	name := kube.JobName(id)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Status:     batchv1.JobStatus{Conditions: conds},
	}
	if _, err := env.Kube.BatchV1().Jobs(testNamespace).Create(env.ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-abcde", Namespace: testNamespace,
			Labels:            map[string]string{"job-name": name},
			CreationTimestamp: metav1.NewTime(podCreated),
		},
		Status: corev1.PodStatus{InitContainerStatuses: init, ContainerStatuses: main},
	}
	if _, err := env.Kube.CoreV1().Pods(testNamespace).Create(env.ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func jobCond(typ batchv1.JobConditionType, reason string, at time.Time) batchv1.JobCondition {
	return batchv1.JobCondition{Type: typ, Status: corev1.ConditionTrue, Reason: reason, LastTransitionTime: metav1.NewTime(at)}
}

func terminatedStatus(name string, code int32, reason string, start, end time.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, Reason: reason, StartedAt: metav1.NewTime(start), FinishedAt: metav1.NewTime(end),
	}}}
}

func waitingStatus(name, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
}

// wantFailure checks run id was reported with reason.
func wantFailure(t *testing.T, env *testEnv, id, reason string) {
	t.Helper()
	r := env.Run(id)
	if r.State != store.Reported || r.FailureReason != reason {
		t.Errorf("run = %s with failure %q; want reported with %q", r.State, r.FailureReason, reason)
	}
}

func wantRunning(t *testing.T, env *testEnv, id string) {
	t.Helper()
	if r := env.Run(id); r.State != store.Running || r.FailureReason != "" {
		t.Errorf("run = %s with failure %q; want still running", r.State, r.FailureReason)
	}
}

// woken reports whether the engine has a wake-up pending, and consumes it.
func woken(env *testEnv) bool {
	select {
	case <-env.Engine.wakeCh():
		return true
	default:
		return false
	}
}

func TestWatchReportsOOM(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	now := env.Now()
	putJob(t, env, id, now.Add(-5*time.Minute), nil,
		[]corev1.ContainerStatus{terminatedStatus("pergent", 137, "OOMKilled", now.Add(-4*time.Minute), now.Add(-2*time.Minute))},
		jobCond(batchv1.JobFailed, "BackoffLimitExceeded", now.Add(-2*time.Minute)))

	watch(t, env)

	wantFailure(t, env, id, "the review job ran out of memory (OOMKilled)")
	if !woken(env) {
		t.Errorf("the publisher was not woken after the report")
	}
}

func TestWatchWaitsGraceBeforeReporting(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	finished := env.Now()
	putJob(t, env, id, finished.Add(-5*time.Minute), nil, nil,
		jobCond(batchv1.JobFailed, "DeadlineExceeded", finished))

	env.SetNow(finished.Add(30 * time.Second))
	watch(t, env)
	wantRunning(t, env, id)
	if woken(env) {
		t.Errorf("woken although nothing was reported")
	}

	env.SetNow(finished.Add(61 * time.Second))
	watch(t, env)
	wantFailure(t, env, id, "the review job hit its deadline (DeadlineExceeded)")
}

func TestWatchCompleteWithoutResult(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	now := env.Now()
	putJob(t, env, id, now.Add(-5*time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-2*time.Minute)))

	watch(t, env)

	wantFailure(t, env, id, "the review job finished without sending a result")
}

func TestWatchStuckImagePull(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	putJob(t, env, id, env.Now().Add(-6*time.Minute),
		[]corev1.ContainerStatus{waitingStatus("checkout", "ImagePullBackOff")}, nil)

	watch(t, env)

	if env.Job(id) != nil {
		t.Errorf("the stuck Job was not deleted")
	}
	wantFailure(t, env, id, "the review couldn't start (ImagePullBackOff)")
	if !woken(env) {
		t.Errorf("the publisher was not woken after the report")
	}
}

func TestWatchStuckDeleteFailsRetriesNextPass(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	putJob(t, env, id, env.Now().Add(-6*time.Minute),
		[]corev1.ContainerStatus{waitingStatus("checkout", "ErrImagePull")}, nil)
	fail := true
	env.Kube.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})

	watch(t, env)
	wantRunning(t, env, id)

	fail = false
	watch(t, env)
	if env.Job(id) != nil {
		t.Errorf("the stuck Job was not deleted")
	}
	wantFailure(t, env, id, "the review couldn't start (ErrImagePull)")
}

func TestWatchJobDisappeared(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a')) // dispatched at env.Now(), no Job in the cluster
	dispatchedAt := env.Now()

	env.SetNow(dispatchedAt.Add(30 * time.Second))
	watch(t, env)
	wantRunning(t, env, id)

	env.SetNow(dispatchedAt.Add(61 * time.Second))
	watch(t, env)
	wantFailure(t, env, id, "the review job disappeared before sending a result")
}

func TestWatchRecordsCloneSeconds(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	start := env.Now().Add(-3 * time.Minute)
	putJob(t, env, id, start.Add(-time.Second),
		[]corev1.ContainerStatus{terminatedStatus("checkout", 0, "Completed", start, start.Add(42*time.Second))}, nil)

	watch(t, env)

	p, ok, err := env.Store.Project(env.ctx, testInstance, testProjectID)
	if err != nil || !ok || p.CloneSeconds != 42 {
		t.Errorf("project clone seconds = %d (%v, %v), want 42", p.CloneSeconds, ok, err)
	}
	wantRunning(t, env, id)
}

// The checkout container of a run with linked repositories also clones them:
// its time is not the repository's own, and the next review that clones
// nothing else must not get a longer deadline for it. [CI]
func TestWatchDoesNotRecordTheCloneTimeOfALinkedRun(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	if err := env.Store.SetRunLinks(env.ctx, id, []store.Link{{Target: "acme/api", CanBlock: true}}); err != nil {
		t.Fatal(err)
	}
	start := env.Now().Add(-3 * time.Minute)
	putJob(t, env, id, start.Add(-time.Second),
		[]corev1.ContainerStatus{terminatedStatus("checkout", 0, "Completed", start, start.Add(200*time.Second))}, nil)

	watch(t, env)

	p, ok, err := env.Store.Project(env.ctx, testInstance, testProjectID)
	if err != nil || !ok || p.CloneSeconds != 0 {
		t.Errorf("project clone seconds = %d (%v, %v); want none recorded for a run with links", p.CloneSeconds, ok, err)
	}
	wantRunning(t, env, id)
}

// A result that arrives while the watcher looks wins: the run is not reported
// again and nothing fails.
func TestWatchResultWonTheRace(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	now := env.Now()
	putJob(t, env, id, now.Add(-5*time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-2*time.Minute)))
	env.Kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if _, err := env.Store.StoreResult(env.ctx, id, "seed-token", resultFor(t, sha('a')), env.Now()); err != nil {
			t.Error(err)
		}
		return false, nil, nil
	})

	watch(t, env)

	r := env.Run(id)
	if r.State != store.Reported || r.FailureReason != "" || r.Result == nil {
		t.Errorf("run = %s, failure %q, result %v; want the stored result kept", r.State, r.FailureReason, r.Result != nil)
	}
}

// One Job the cluster can't be asked about doesn't stop the others.
func TestWatchInspectErrorSkipsRun(t *testing.T) {
	env := newTestEnv(t)
	broken := env.Dispatched(1, sha('a'))
	gone := env.Dispatched(2, sha('a'))
	env.Kube.PrependReactor("get", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == kube.JobName(broken) {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	env.SetNow(env.Now().Add(2 * time.Minute))

	watch(t, env)

	wantRunning(t, env, broken)
	wantFailure(t, env, gone, "the review job disappeared before sending a result")
	if !strings.Contains(env.Logs(), "apiserver unavailable") {
		t.Errorf("the inspect error was not logged:\n%s", env.Logs())
	}
}

// putBareJob stores run id's Job, created at created, with the given
// conditions and no pod.
func putBareJob(t *testing.T, env *testEnv, id string, created time.Time, conds ...batchv1.JobCondition) {
	t.Helper()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: kube.JobName(id), Namespace: testNamespace, CreationTimestamp: metav1.NewTime(created)},
		Status:     batchv1.JobStatus{Conditions: conds},
	}
	if _, err := env.Kube.BatchV1().Jobs(testNamespace).Create(env.ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// putUnscheduledPod stores a pod of run id's Job that the scheduler has failed
// to place since since, with message.
func putUnscheduledPod(t *testing.T, env *testEnv, id string, since time.Time, message string) {
	t.Helper()
	name := kube.JobName(id)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-abcde", Namespace: testNamespace,
			Labels:            map[string]string{"job-name": name},
			CreationTimestamp: metav1.NewTime(since),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
			Message: message, LastTransitionTime: metav1.NewTime(since),
		}}},
	}
	if _, err := env.Kube.CoreV1().Pods(testNamespace).Create(env.ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// putEvent stores an event about the object kind/name.
func putEvent(t *testing.T, env *testEnv, kind, name, reason, message string, at time.Time) {
	t.Helper()
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name + "." + reason, Namespace: testNamespace},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name, Namespace: testNamespace},
		Type:           corev1.EventTypeWarning, Reason: reason, Message: message, Count: 1,
		LastTimestamp: metav1.NewTime(at),
	}
	if _, err := env.Kube.CoreV1().Events(testNamespace).Create(env.ctx, ev, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func runningStatus(name string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
}

func storedDiagnostics(t *testing.T, env *testEnv, id string) (store.Diagnostics, bool) {
	t.Helper()
	d, found, err := env.Store.RunDiagnostics(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return d, found
}

// A pod no node can run fails at the starting step once it has waited 10
// minutes. The scheduler's message is the admin-only detail, the pod state is
// kept, and both are taken before the Job is deleted. [CI]
func TestWatchStoresStepAndDetail(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	const message = "0/3 nodes are available: 3 Insufficient memory."
	putBareJob(t, env, id, env.Now().Add(-12*time.Minute))
	putUnscheduledPod(t, env, id, env.Now().Add(-11*time.Minute), message)
	putJobSecret(t, env, id, kube.Secrets{GitToken: "git-token-1", LLMKey: "llm-key-1", ResultToken: "result-token-1"})
	// No container started, so neither has a log.
	serveContainerLogs(env, map[string]string{})
	keptBeforeDelete := false
	env.Kube.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		_, keptBeforeDelete, _ = env.Store.RunDiagnostics(env.ctx, id)
		return false, nil, nil
	})

	watch(t, env)

	wantFailure(t, env, id, "the review couldn't start: no node could run it")
	if r := env.Run(id); r.FailureStep != kube.StepStarting || r.FailureDetail != message {
		t.Errorf("step %q, detail %q; want %q, %q", r.FailureStep, r.FailureDetail, kube.StepStarting, message)
	}
	if env.Job(id) != nil {
		t.Errorf("the stuck Job was not deleted")
	}
	d, found := storedDiagnostics(t, env, id)
	if !found || d.CloneLog != "" || !strings.Contains(d.PodState, "PodScheduled=False: Unschedulable: "+message) {
		t.Errorf("diagnostics = %+v, %v; want the pod state with the scheduler's message, no clone log", d, found)
	}
	if !keptBeforeDelete {
		t.Errorf("the diagnostics were not stored before the Job was deleted")
	}
}

// The admin-only detail is redacted like the log; with the Job's Secret
// unreadable it is left out, never stored raw. [CI]
func TestWatchRedactsDetail(t *testing.T) {
	env := newTestEnv(t)
	withSecret := env.Dispatched(1, sha('a'))
	without := env.Dispatched(2, sha('a'))
	for _, id := range []string{withSecret, without} {
		putBareJob(t, env, id, env.Now().Add(-11*time.Minute))
		putEvent(t, env, "Job", kube.JobName(id), "FailedCreate",
			"pods is forbidden: exceeded quota, token git-token-1 in the way", env.Now().Add(-time.Minute))
	}
	putJobSecret(t, env, withSecret, kube.Secrets{GitToken: "git-token-1", LLMKey: "llm-key-1", ResultToken: "result-token-1"})

	watch(t, env)

	wantFailure(t, env, withSecret, "the review couldn't start: no pod could be created")
	if r := env.Run(withSecret); r.FailureStep != kube.StepStarting || r.FailureDetail != "pods is forbidden: exceeded quota, token [REDACTED] in the way" {
		t.Errorf("step %q, detail %q; want the event's message redacted", r.FailureStep, r.FailureDetail)
	}
	wantFailure(t, env, without, "the review couldn't start: no pod could be created")
	if r := env.Run(without); r.FailureStep != kube.StepStarting || r.FailureDetail != "" {
		t.Errorf("step %q, detail %q; want no detail without the Secret", r.FailureStep, r.FailureDetail)
	}
}

// The deadline kills the pod, so the step comes from what earlier passes saw:
// a checkout seen done means the deadline passed during the review. [CI]
func TestWatchRefinesDeadlineStep(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	if err := env.Store.AdvanceCheckoutSeen(env.ctx, id, 2); err != nil {
		t.Fatal(err)
	}
	putBareJob(t, env, id, env.Now().Add(-time.Hour), jobCond(batchv1.JobFailed, "DeadlineExceeded", env.Now().Add(-2*time.Minute)))

	watch(t, env)

	wantFailure(t, env, id, "the review didn't finish before the deadline (DeadlineExceeded)")
	if r := env.Run(id); r.FailureStep != kube.StepReview {
		t.Errorf("step = %q, want %q", r.FailureStep, kube.StepReview)
	}
}

// Without anything seen earlier, the deadline's step is unknown; a checkout
// the deadline killed is the clone, whatever was seen. [CI]
func TestWatchDeadlineStepWithoutProgress(t *testing.T) {
	env := newTestEnv(t)
	unseen := env.Dispatched(1, sha('a'))
	putBareJob(t, env, unseen, env.Now().Add(-time.Hour), jobCond(batchv1.JobFailed, "DeadlineExceeded", env.Now().Add(-2*time.Minute)))
	killed := env.Dispatched(2, sha('a'))
	now := env.Now()
	putJob(t, env, killed, now.Add(-time.Hour),
		[]corev1.ContainerStatus{terminatedStatus("checkout", 137, "Error", now.Add(-time.Hour), now.Add(-2*time.Minute))}, nil,
		jobCond(batchv1.JobFailed, "DeadlineExceeded", now.Add(-2*time.Minute)))

	watch(t, env)

	wantFailure(t, env, unseen, "the review job hit its deadline (DeadlineExceeded)")
	if r := env.Run(unseen); r.FailureStep != kube.StepUnknown {
		t.Errorf("step = %q, want %q", r.FailureStep, kube.StepUnknown)
	}
	wantFailure(t, env, killed, "the clone didn't finish before the deadline (DeadlineExceeded)")
	if r := env.Run(killed); r.FailureStep != kube.StepClone {
		t.Errorf("step = %q, want %q", r.FailureStep, kube.StepClone)
	}
}

// Each pass records the Job's pod and how far its checkout got. [CI]
func TestWatchRecordsPodAndProgress(t *testing.T) {
	env := newTestEnv(t)
	id := env.Dispatched(1, sha('a'))
	now := env.Now()
	putJob(t, env, id, now.Add(-time.Minute), []corev1.ContainerStatus{runningStatus("checkout")}, nil)

	watch(t, env)
	if r := env.Run(id); r.PodName != kube.JobName(id)+"-abcde" || r.CheckoutSeen != 1 {
		t.Errorf("pod %q, checkout seen %d; want the pod and 1", r.PodName, r.CheckoutSeen)
	}

	pod, err := env.Kube.CoreV1().Pods(testNamespace).Get(env.ctx, kube.JobName(id)+"-abcde", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{terminatedStatus("checkout", 0, "Completed", now.Add(-time.Minute), now)}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{runningStatus("pergent")}
	if _, err := env.Kube.CoreV1().Pods(testNamespace).Update(env.ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	watch(t, env)
	if r := env.Run(id); r.CheckoutSeen != 2 {
		t.Errorf("checkout seen %d after the checkout finished; want 2", r.CheckoutSeen)
	}
	wantRunning(t, env, id)
}

// The watcher's own failures name their step. [CI]
func TestWatchFailureSteps(t *testing.T) {
	env := newTestEnv(t)
	gone := env.Dispatched(1, sha('a'))
	quiet := env.Dispatched(2, sha('a'))
	now := env.Now()
	putJob(t, env, quiet, now.Add(-5*time.Minute), nil, nil, jobCond(batchv1.JobComplete, "", now.Add(-2*time.Minute)))
	env.SetNow(now.Add(2 * time.Minute))

	watch(t, env)

	for id, step := range map[string]string{gone: kube.StepUnknown, quiet: kube.StepDelivery} {
		if r := env.Run(id); r.State != store.Reported || r.FailureStep != step {
			t.Errorf("run = %s at step %q (%s); want reported at %q", r.State, r.FailureStep, r.FailureReason, step)
		}
	}
}

// A run claimed but never given a Job (pergent.sh stopped in between) goes
// back to the queue once its claim is five minutes old.
func TestWatchRecoversUnlaunchedRun(t *testing.T) {
	env := newTestEnv(t)
	stranded := env.Enqueue(1, sha('a'))
	if _, err := env.Store.ClaimQueued(env.ctx, 10, env.Now().Add(-6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	recent := env.Enqueue(2, sha('a'))
	if _, err := env.Store.ClaimQueued(env.ctx, 10, env.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	watch(t, env)

	if r := env.Run(stranded); r.State != store.Queued || r.Attempts != 0 {
		t.Errorf("stranded run = %s with %d attempts, want queued with 0", r.State, r.Attempts)
	}
	if !woken(env) {
		t.Errorf("the dispatcher was not woken after the recovery")
	}
	wantRunning(t, env, recent)
}
