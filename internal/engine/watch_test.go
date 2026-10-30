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
	wantFailure(t, env, id, "couldn't start the review job (ImagePullBackOff)")
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
	wantFailure(t, env, id, "couldn't start the review job (ErrImagePull)")
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
