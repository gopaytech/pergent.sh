package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var inspectNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func jobWith(name string, conds ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "reviews"},
		Status:     batchv1.JobStatus{Conditions: conds},
	}
}

// jobCreated is a Job created at created.
func jobCreated(name string, created time.Time, conds ...batchv1.JobCondition) *batchv1.Job {
	j := jobWith(name, conds...)
	j.CreationTimestamp = metav1.NewTime(created)
	return j
}

func cond(t batchv1.JobConditionType, reason string, at time.Time) batchv1.JobCondition {
	return batchv1.JobCondition{Type: t, Status: corev1.ConditionTrue, Reason: reason, LastTransitionTime: metav1.NewTime(at)}
}

func podFor(job string, created time.Time, init, main []corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job + "-abcde", Namespace: "reviews",
			Labels:            map[string]string{"job-name": job},
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{InitContainerStatuses: init, ContainerStatuses: main},
	}
}

// unscheduled is a pod the scheduler could not place. since is when
// PodScheduled turned False (zero leaves the time out, as a cluster may).
func unscheduled(job string, created, since time.Time, message string) *corev1.Pod {
	p := podFor(job, created, nil, nil)
	p.Status.Phase = corev1.PodPending
	c := corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: message}
	if !since.IsZero() {
		c.LastTransitionTime = metav1.NewTime(since)
	}
	p.Status.Conditions = []corev1.PodCondition{c}
	return p
}

func terminated(name string, code int32, reason string, start, end time.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, Reason: reason, StartedAt: metav1.NewTime(start), FinishedAt: metav1.NewTime(end),
	}}}
}

func waiting(name, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
}

func running(name string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
}

// jobEvent is an event about a Job.
func jobEvent(job, reason, message string, at time.Time) *corev1.Event {
	return objectEvent("Job", job, reason, message, at)
}

func objectEvent(kind, name, reason, message string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: kind + "-" + name + "-" + reason + "-" + at.Format("150405"), Namespace: "reviews"},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name, Namespace: "reviews"},
		Type:           corev1.EventTypeWarning, Reason: reason, Message: message,
		LastTimestamp: metav1.NewTime(at),
	}
}

// inspectCluster is a fake cluster holding job (when not nil), pods and events,
// plus a pod of another job that must be ignored.
func inspectCluster(t *testing.T, job *batchv1.Job, pods []*corev1.Pod, events ...*corev1.Event) *fake.Clientset {
	t.Helper()
	cs := fake.NewSimpleClientset()
	ctx := context.Background()
	if job != nil {
		if _, err := cs.BatchV1().Jobs("reviews").Create(ctx, job, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range pods {
		if _, err := cs.CoreV1().Pods("reviews").Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range events {
		if _, err := cs.CoreV1().Events("reviews").Create(ctx, e, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// a pod of another job must be ignored
	start := inspectNow.Add(-10 * time.Minute)
	other := podFor("other", inspectNow.Add(-time.Hour), []corev1.ContainerStatus{terminated("checkout", 1, "Error", start, start.Add(37*time.Second))}, nil)
	if _, err := cs.CoreV1().Pods("reviews").Create(ctx, other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestInspectReasons(t *testing.T) {
	failedAt := inspectNow.Add(-time.Minute)
	cloneStart := inspectNow.Add(-10 * time.Minute)
	cloneEnd := cloneStart.Add(37 * time.Second)
	okInit := terminated("checkout", 0, "Completed", cloneStart, cloneEnd)

	cases := []struct {
		name         string
		job          *batchv1.Job
		pods         []*corev1.Pod
		want         Observation
		wantFinished bool
	}{
		{
			name: "job not found",
			job:  nil,
			want: Observation{},
		},
		{
			name: "running",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-time.Minute), []corev1.ContainerStatus{okInit}, nil)},
			want: Observation{Exists: true, Clone: 37 * time.Second, PodName: "j-abcde", Checkout: 2},
		},
		{
			name:         "complete",
			job:          jobWith("j", cond(batchv1.JobComplete, "", failedAt)),
			pods:         []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("pergent", 0, "Completed", cloneEnd, failedAt)})},
			want:         Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second, PodName: "j-abcde", Checkout: 2},
			wantFinished: true,
		},
		{
			name: "init container exit 128",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{terminated("checkout", 128, "Error", cloneStart, cloneEnd)}, []corev1.ContainerStatus{waiting("pergent", "PodInitializing")})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second, PodName: "j-abcde",
				Failure: Failure{Step: StepClone, Reason: "the clone failed (checkout exited with code 128)"}},
		},
		{
			name: "deadline exceeded while the checkout was killed",
			job:  jobWith("j", cond(batchv1.JobFailed, "DeadlineExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{terminated("checkout", 137, "Error", cloneStart, cloneEnd)}, nil)},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second, PodName: "j-abcde",
				Failure: Failure{Step: StepClone, Reason: "the clone didn't finish before the deadline (DeadlineExceeded)", Deadline: true}},
		},
		{
			name: "deadline exceeded while the checkout was terminated",
			job:  jobWith("j", cond(batchv1.JobFailed, "DeadlineExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{terminated("checkout", 143, "Error", cloneStart, cloneEnd)}, nil)},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second, PodName: "j-abcde",
				Failure: Failure{Step: StepClone, Reason: "the clone didn't finish before the deadline (DeadlineExceeded)", Deadline: true}},
		},
		{
			name: "deadline exceeded after the clone",
			job:  jobWith("j", cond(batchv1.JobFailed, "DeadlineExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("pergent", 143, "Error", cloneEnd, failedAt)})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second, PodName: "j-abcde", Checkout: 2,
				Failure: Failure{Step: StepReview, Reason: "the review didn't finish before the deadline (DeadlineExceeded)", Deadline: true}},
		},
		{
			name: "deadline exceeded with no pods left",
			job:  jobWith("j", cond(batchv1.JobFailed, "DeadlineExceeded", failedAt)),
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt,
				Failure: Failure{Step: StepUnknown, Reason: "the review job hit its deadline (DeadlineExceeded)", Deadline: true}},
		},
		{
			name: "oom killed main container",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("pergent", 137, "OOMKilled", cloneEnd, failedAt)})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second, PodName: "j-abcde", Checkout: 2,
				Failure: Failure{Step: StepReview, Reason: "the review job ran out of memory (OOMKilled)"}},
		},
		{
			name: "oom killed checkout container",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{terminated("checkout", 137, "OOMKilled", cloneStart, cloneEnd)}, nil)},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second, PodName: "j-abcde",
				Failure: Failure{Step: StepClone, Reason: "the clone ran out of memory (OOMKilled)"}},
		},
		{
			name: "oom beats exit 4",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart,
				[]corev1.ContainerStatus{okInit},
				[]corev1.ContainerStatus{terminated("pergent", 4, "OOMKilled", cloneEnd, failedAt)})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second, PodName: "j-abcde", Checkout: 2,
				Failure: Failure{Step: StepReview, Reason: "the review job ran out of memory (OOMKilled)"}},
		},
		{
			name: "pergent exit 4",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("pergent", 4, "Error", cloneEnd, failedAt)})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second, PodName: "j-abcde", Checkout: 2,
				Failure: Failure{Step: StepDelivery, Reason: "the review job couldn't deliver its result (exit code 4)"}},
		},
		{
			name: "other failure",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("pergent", 1, "Error", cloneEnd, failedAt)})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second, PodName: "j-abcde", Checkout: 2,
				Failure: Failure{Step: StepUnknown, Reason: "the review job failed (BackoffLimitExceeded)"}},
		},
		{
			name: "failure with no reason",
			job:  jobWith("j", cond(batchv1.JobFailed, "", failedAt)),
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt,
				Failure: Failure{Step: StepUnknown, Reason: "the review job failed (unknown reason)"}},
		},
		{
			name: "stuck: ErrImagePull for over 5 minutes",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-6*time.Minute), []corev1.ContainerStatus{waiting("checkout", "ErrImagePull")}, nil)},
			want: Observation{Exists: true, PodName: "j-abcde", Stuck: Failure{Step: StepStarting, Reason: "the review couldn't start (ErrImagePull)"}},
		},
		{
			name: "stuck: ImagePullBackOff on the main container",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-6*time.Minute), []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{waiting("pergent", "ImagePullBackOff")})},
			want: Observation{Exists: true, PodName: "j-abcde", Checkout: 2, Clone: 37 * time.Second,
				Stuck: Failure{Step: StepStarting, Reason: "the review couldn't start (ImagePullBackOff)"}},
		},
		{
			name: "stuck: CreateContainerConfigError",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-6*time.Minute), nil, []corev1.ContainerStatus{waiting("pergent", "CreateContainerConfigError")})},
			want: Observation{Exists: true, PodName: "j-abcde", Stuck: Failure{Step: StepStarting, Reason: "the review couldn't start (CreateContainerConfigError)"}},
		},
		{
			name: "stuck: InvalidImageName",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-6*time.Minute), []corev1.ContainerStatus{waiting("checkout", "InvalidImageName")}, nil)},
			want: Observation{Exists: true, PodName: "j-abcde", Stuck: Failure{Step: StepStarting, Reason: "the review couldn't start (InvalidImageName)"}},
		},
		{
			name: "not stuck yet: ErrImagePull under 5 minutes",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-4*time.Minute), []corev1.ContainerStatus{waiting("checkout", "ErrImagePull")}, nil)},
			want: Observation{Exists: true, PodName: "j-abcde"},
		},
		{
			name: "not stuck: ContainerCreating for a long time",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-20*time.Minute), []corev1.ContainerStatus{waiting("checkout", "ContainerCreating")}, nil)},
			want: Observation{Exists: true, PodName: "j-abcde"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := inspectCluster(t, c.job, c.pods)
			l := &Launcher{Client: cs, Namespace: "reviews"}
			got, err := l.Inspect(context.Background(), "j", inspectNow)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// TestInspectTenMinuteRules pins the two start rules: a pod that stays
// unscheduled, and a Job that makes no pod. Each is stuck only for strictly
// more than 10 minutes, and a pod that is scheduled is never stuck by them.
func TestInspectTenMinuteRules(t *testing.T) {
	const noNode = "0/3 nodes are available: 3 Insufficient memory."
	const quota = "exceeded quota: compute-resources"
	over := 11 * time.Minute
	justUnder := 10*time.Minute - time.Second

	cases := []struct {
		name       string
		job        *batchv1.Job
		pods       []*corev1.Pod
		events     []*corev1.Event
		forbidden  bool
		wantStep   string
		wantReason string
		wantDetail string
		wantStuck  bool // else the Job is not failed by these rules
		wantFailed bool // a finished Job reports Failure, not Stuck
	}{
		{
			name: "unscheduled for 11 minutes", job: jobCreated("j", inspectNow.Add(-time.Hour)),
			pods:     []*corev1.Pod{unscheduled("j", inspectNow.Add(-time.Hour), inspectNow.Add(-over), noNode)},
			wantStep: StepStarting, wantReason: "the review couldn't start: no node could run it", wantDetail: noNode, wantStuck: true,
		},
		{
			name: "unscheduled for 9m59s", job: jobCreated("j", inspectNow.Add(-time.Hour)),
			pods: []*corev1.Pod{unscheduled("j", inspectNow.Add(-time.Hour), inspectNow.Add(-justUnder), noNode)},
		},
		{
			name: "unscheduled for exactly 10 minutes", job: jobCreated("j", inspectNow.Add(-time.Hour)),
			pods: []*corev1.Pod{unscheduled("j", inspectNow.Add(-time.Hour), inspectNow.Add(-10*time.Minute), noNode)},
		},
		{
			// The clock is the condition's: a pod made long ago that turned
			// unschedulable a minute ago has not waited 10 minutes.
			name: "old pod, condition changed 1 minute ago", job: jobCreated("j", inspectNow.Add(-time.Hour)),
			pods: []*corev1.Pod{unscheduled("j", inspectNow.Add(-time.Hour), inspectNow.Add(-time.Minute), noNode)},
		},
		{
			name: "no condition time, pod created 11 minutes ago", job: jobCreated("j", inspectNow.Add(-time.Hour)),
			pods:     []*corev1.Pod{unscheduled("j", inspectNow.Add(-over), time.Time{}, noNode)},
			wantStep: StepStarting, wantReason: "the review couldn't start: no node could run it", wantDetail: noNode, wantStuck: true,
		},
		{
			name: "no condition time, pod created 9m59s ago", job: jobCreated("j", inspectNow.Add(-time.Hour)),
			pods: []*corev1.Pod{unscheduled("j", inspectNow.Add(-justUnder), time.Time{}, noNode)},
		},
		{
			name: "scheduled pod, containers creating at 11 minutes", job: jobCreated("j", inspectNow.Add(-time.Hour)),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-over), []corev1.ContainerStatus{waiting("checkout", "ContainerCreating")}, nil)},
		},
		{
			name: "scheduled pod, image pulling at 11 minutes", job: jobCreated("j", inspectNow.Add(-time.Hour)),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-over), []corev1.ContainerStatus{running("checkout")}, []corev1.ContainerStatus{waiting("pergent", "PodInitializing")})},
		},
		{
			name: "unscheduled but the Job already failed", job: jobCreated("j", inspectNow.Add(-time.Hour), cond(batchv1.JobFailed, "DeadlineExceeded", inspectNow.Add(-time.Minute))),
			pods:       []*corev1.Pod{unscheduled("j", inspectNow.Add(-time.Hour), inspectNow.Add(-over), noNode)},
			wantFailed: true,
		},
		{
			name: "no pod for 11 minutes, FailedCreate event", job: jobCreated("j", inspectNow.Add(-over)),
			events:   []*corev1.Event{jobEvent("j", "FailedCreate", quota, inspectNow.Add(-5*time.Minute))},
			wantStep: StepStarting, wantReason: "the review couldn't start: no pod could be created", wantDetail: quota, wantStuck: true,
		},
		{
			name: "no pod for 11 minutes, newest FailedCreate wins", job: jobCreated("j", inspectNow.Add(-over)),
			events: []*corev1.Event{
				jobEvent("j", "FailedCreate", "older message", inspectNow.Add(-8*time.Minute)),
				jobEvent("j", "FailedCreate", quota, inspectNow.Add(-2*time.Minute)),
				jobEvent("j", "SuccessfulCreate", "created pod", inspectNow.Add(-time.Minute)),
				objectEvent("Job", "other", "FailedCreate", "another job's message", inspectNow.Add(-30*time.Second)),
				objectEvent("Pod", "j", "FailedCreate", "a pod's message", inspectNow.Add(-20*time.Second)),
			},
			wantStep: StepStarting, wantReason: "the review couldn't start: no pod could be created", wantDetail: quota, wantStuck: true,
		},
		{
			name: "no pod for 11 minutes, no FailedCreate event", job: jobCreated("j", inspectNow.Add(-over)),
			events:   []*corev1.Event{jobEvent("j", "SuccessfulCreate", "created pod", inspectNow.Add(-time.Minute))},
			wantStep: StepStarting, wantReason: "the review couldn't start: no pod could be created", wantDetail: "", wantStuck: true,
		},
		{
			name: "no pod for 9m59s", job: jobCreated("j", inspectNow.Add(-justUnder)),
			events: []*corev1.Event{jobEvent("j", "FailedCreate", quota, inspectNow.Add(-time.Minute))},
		},
		{
			name: "no pod for exactly 10 minutes", job: jobCreated("j", inspectNow.Add(-10*time.Minute)),
			events: []*corev1.Event{jobEvent("j", "FailedCreate", quota, inspectNow.Add(-time.Minute))},
		},
		{
			name: "no pod for 11 minutes, events forbidden", job: jobCreated("j", inspectNow.Add(-over)),
			forbidden: true,
			wantStep:  StepStarting, wantReason: "the review couldn't start: no pod could be created", wantDetail: "", wantStuck: true,
		},
		{
			name: "no pod, creation time unknown", job: jobWith("j"),
		},
		{
			name: "finished Job with no pod", job: jobCreated("j", inspectNow.Add(-time.Hour), cond(batchv1.JobComplete, "", inspectNow.Add(-time.Minute))),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := inspectCluster(t, c.job, c.pods, c.events...)
			if c.forbidden {
				cs.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("no list on events"))
				})
			}
			got, err := (&Launcher{Client: cs, Namespace: "reviews"}).Inspect(context.Background(), "j", inspectNow)
			if err != nil {
				t.Fatal(err)
			}
			want := Failure{}
			if c.wantStuck {
				want = Failure{Step: c.wantStep, Reason: c.wantReason, Detail: c.wantDetail}
			}
			if got.Stuck != want {
				t.Errorf("Stuck = %+v; want %+v", got.Stuck, want)
			}
			if (got.Failure != Failure{}) != c.wantFailed {
				t.Errorf("Failure = %+v; want failed = %v", got.Failure, c.wantFailed)
			}
			if c.wantFailed && got.Failure.Detail != "" {
				t.Errorf("a failed Job's Failure has Detail %q", got.Failure.Detail)
			}
		})
	}
}

// TestInspectListsJobEventsOnlyForTheNoPodRule: the events are read by the
// Job's name and kind, and not at all unless the Job has no pod for 10 minutes.
func TestInspectListsJobEventsOnlyForTheNoPodRule(t *testing.T) {
	list := func(t *testing.T, job *batchv1.Job, pods []*corev1.Pod) (fields []string) {
		cs := inspectCluster(t, job, pods)
		cs.PrependReactor("list", "events", func(a k8stesting.Action) (bool, runtime.Object, error) {
			fields = append(fields, a.(k8stesting.ListAction).GetListRestrictions().Fields.String())
			return false, nil, nil
		})
		if _, err := (&Launcher{Client: cs, Namespace: "reviews"}).Inspect(context.Background(), "j", inspectNow); err != nil {
			t.Fatal(err)
		}
		return fields
	}

	got := list(t, jobCreated("j", inspectNow.Add(-11*time.Minute)), nil)
	if len(got) != 1 || got[0] != "involvedObject.kind=Job,involvedObject.name=j" {
		t.Errorf("field selectors = %q; want the Job's kind and name", got)
	}
	if got := list(t, jobCreated("j", inspectNow.Add(-time.Minute)), nil); len(got) != 0 {
		t.Errorf("events listed for a Job under 10 minutes old: %q", got)
	}
	pod := podFor("j", inspectNow.Add(-time.Hour), nil, nil)
	if got := list(t, jobCreated("j", inspectNow.Add(-time.Hour)), []*corev1.Pod{pod}); len(got) != 0 {
		t.Errorf("events listed for a Job with a pod: %q", got)
	}
}

// TestInspectEventsErrorIsAnError: only Forbidden means "no detail"; any other
// failure to read the events is an error, so the watcher looks again.
func TestInspectEventsErrorIsAnError(t *testing.T) {
	cs := inspectCluster(t, jobCreated("j", inspectNow.Add(-11*time.Minute)), nil)
	cs.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection reset")
	})
	if _, err := (&Launcher{Client: cs, Namespace: "reviews"}).Inspect(context.Background(), "j", inspectNow); err == nil {
		t.Error("Inspect returned no error when the events could not be read")
	}
}

func TestInspectReportsPodAndCheckout(t *testing.T) {
	cloneStart := inspectNow.Add(-10 * time.Minute)
	cases := []struct {
		name         string
		init         []corev1.ContainerStatus
		wantCheckout int
	}{
		{"no state yet", nil, 0},
		{"waiting", []corev1.ContainerStatus{waiting("checkout", "PodInitializing")}, 0},
		{"running", []corev1.ContainerStatus{running("checkout")}, 1},
		{"terminated with 0", []corev1.ContainerStatus{terminated("checkout", 0, "Completed", cloneStart, cloneStart.Add(time.Second))}, 2},
		{"terminated with 128", []corev1.ContainerStatus{terminated("checkout", 128, "Error", cloneStart, cloneStart.Add(time.Second))}, 0},
		{"another init container running", []corev1.ContainerStatus{running("other")}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := podFor("j", inspectNow.Add(-time.Hour), nil, nil)
			old.Name = "j-old"
			newest := podFor("j", inspectNow.Add(-time.Minute), c.init, nil)
			newest.Name = "j-new"
			// created newest first: the order of the list must not matter
			cs := inspectCluster(t, jobWith("j"), []*corev1.Pod{newest, old})
			got, err := (&Launcher{Client: cs, Namespace: "reviews"}).Inspect(context.Background(), "j", inspectNow)
			if err != nil {
				t.Fatal(err)
			}
			if got.PodName != "j-new" || got.Checkout != c.wantCheckout {
				t.Errorf("PodName = %q, Checkout = %d; want j-new, %d", got.PodName, got.Checkout, c.wantCheckout)
			}
		})
	}

	// A Job with no pod has neither.
	cs := inspectCluster(t, jobWith("j"), nil)
	got, err := (&Launcher{Client: cs, Namespace: "reviews"}).Inspect(context.Background(), "j", inspectNow)
	if err != nil {
		t.Fatal(err)
	}
	if got.PodName != "" || got.Checkout != 0 {
		t.Errorf("PodName = %q, Checkout = %d; want none", got.PodName, got.Checkout)
	}
}

func TestDeadlineFailure(t *testing.T) {
	cases := []struct {
		checkout int
		want     Failure
	}{
		{0, Failure{Step: StepUnknown, Reason: "the review job hit its deadline (DeadlineExceeded)", Deadline: true}},
		{1, Failure{Step: StepClone, Reason: "the clone didn't finish before the deadline (DeadlineExceeded)", Deadline: true}},
		{2, Failure{Step: StepReview, Reason: "the review didn't finish before the deadline (DeadlineExceeded)", Deadline: true}},
	}
	for _, c := range cases {
		if got := DeadlineFailure(c.checkout); got != c.want {
			t.Errorf("DeadlineFailure(%d) = %+v; want %+v", c.checkout, got, c.want)
		}
	}
}

// TestObjectEvents: the helper reads one object's events by kind and name,
// oldest first (a timeline), and a refusal to list is no events, not an error.
func TestObjectEvents(t *testing.T) {
	cs := inspectCluster(t, jobWith("j"), nil,
		objectEvent("Pod", "j-abcde", "Scheduled", "assigned", inspectNow.Add(-3*time.Minute)),
		objectEvent("Pod", "j-abcde", "Pulling", "pulling", inspectNow.Add(-2*time.Minute)),
		objectEvent("Pod", "j-other", "Pulling", "another pod", inspectNow.Add(-time.Minute)),
		jobEvent("j-abcde", "FailedCreate", "a Job with the pod's name", inspectNow),
	)
	l := &Launcher{Client: cs, Namespace: "reviews"}
	got, err := l.ObjectEvents(context.Background(), "Pod", "j-abcde")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Reason != "Scheduled" || got[1].Reason != "Pulling" {
		t.Errorf("events = %v; want [Scheduled Pulling]", reasons(got))
	}

	cs.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("no"))
	})
	got, err = l.ObjectEvents(context.Background(), "Pod", "j-abcde")
	if err != nil || len(got) != 0 {
		t.Errorf("forbidden: events = %v, err = %v; want none, nil", reasons(got), err)
	}
}

func reasons(es []corev1.Event) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Reason)
	}
	return out
}

func TestInspectStuckWinsOverUnfinished(t *testing.T) {
	// Stuck is only reported while the Job is unfinished; a finished Job reports Failure only.
	cs := fake.NewSimpleClientset()
	failedAt := inspectNow.Add(-time.Minute)
	cs.BatchV1().Jobs("reviews").Create(context.Background(), jobWith("j", cond(batchv1.JobFailed, "DeadlineExceeded", failedAt)), metav1.CreateOptions{})
	cs.CoreV1().Pods("reviews").Create(context.Background(),
		podFor("j", inspectNow.Add(-time.Hour), []corev1.ContainerStatus{waiting("checkout", "ErrImagePull")}, nil), metav1.CreateOptions{})
	got, err := (&Launcher{Client: cs, Namespace: "reviews"}).Inspect(context.Background(), "j", inspectNow)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stuck != (Failure{}) || got.Failure.Reason != "the review job hit its deadline (DeadlineExceeded)" {
		t.Errorf("got %+v", got)
	}
}
