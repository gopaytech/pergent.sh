package kube

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var inspectNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func jobWith(name string, conds ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "reviews"},
		Status:     batchv1.JobStatus{Conditions: conds},
	}
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

func terminated(name string, code int32, reason string, start, end time.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, Reason: reason, StartedAt: metav1.NewTime(start), FinishedAt: metav1.NewTime(end),
	}}}
}

func waiting(name, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
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
		wantFailure  string
		wantStuck    string
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
			want: Observation{Exists: true, Clone: 37 * time.Second},
		},
		{
			name:         "complete",
			job:          jobWith("j", cond(batchv1.JobComplete, "", failedAt)),
			pods:         []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("pergent", 0, "Completed", cloneEnd, failedAt)})},
			want:         Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second},
			wantFinished: true,
		},
		{
			name: "init container exit 128",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{terminated("checkout", 128, "Error", cloneStart, cloneEnd)}, []corev1.ContainerStatus{waiting("pergent", "PodInitializing")})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second,
				Failure: "couldn't fetch the merge request's commits (checkout exited with code 128)"},
		},
		{
			name: "oom killed main container",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("pergent", 137, "OOMKilled", cloneEnd, failedAt)})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second,
				Failure: "the review job ran out of memory (OOMKilled)"},
		},
		{
			name: "oom beats exit 4",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart,
				[]corev1.ContainerStatus{okInit},
				[]corev1.ContainerStatus{terminated("pergent", 4, "OOMKilled", cloneEnd, failedAt)})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second,
				Failure: "the review job ran out of memory (OOMKilled)"},
		},
		{
			name: "pergent exit 4",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("pergent", 4, "Error", cloneEnd, failedAt)})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second,
				Failure: "the review job couldn't deliver its result (exit code 4)"},
		},
		{
			name: "deadline exceeded",
			job:  jobWith("j", cond(batchv1.JobFailed, "DeadlineExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("pergent", 143, "Error", cloneEnd, failedAt)})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second,
				Failure: "the review job hit its deadline (DeadlineExceeded)"},
		},
		{
			name: "deadline exceeded with no pods left",
			job:  jobWith("j", cond(batchv1.JobFailed, "DeadlineExceeded", failedAt)),
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt,
				Failure: "the review job hit its deadline (DeadlineExceeded)"},
		},
		{
			name: "other failure",
			job:  jobWith("j", cond(batchv1.JobFailed, "BackoffLimitExceeded", failedAt)),
			pods: []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("pergent", 1, "Error", cloneEnd, failedAt)})},
			want: Observation{Exists: true, Finished: true, FinishedAt: failedAt, Clone: 37 * time.Second,
				Failure: "the review job failed (BackoffLimitExceeded)"},
		},
		{
			name: "stuck: ErrImagePull for over 5 minutes",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-6*time.Minute), []corev1.ContainerStatus{waiting("checkout", "ErrImagePull")}, nil)},
			want: Observation{Exists: true, Stuck: "couldn't start the review job (ErrImagePull)"},
		},
		{
			name: "stuck: ImagePullBackOff on the main container",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-6*time.Minute), []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{waiting("pergent", "ImagePullBackOff")})},
			want: Observation{Exists: true, Stuck: "couldn't start the review job (ImagePullBackOff)", Clone: 37 * time.Second},
		},
		{
			name: "stuck: CreateContainerConfigError",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-6*time.Minute), nil, []corev1.ContainerStatus{waiting("pergent", "CreateContainerConfigError")})},
			want: Observation{Exists: true, Stuck: "couldn't start the review job (CreateContainerConfigError)"},
		},
		{
			name: "stuck: InvalidImageName",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-6*time.Minute), []corev1.ContainerStatus{waiting("checkout", "InvalidImageName")}, nil)},
			want: Observation{Exists: true, Stuck: "couldn't start the review job (InvalidImageName)"},
		},
		{
			name: "not stuck yet: ErrImagePull under 5 minutes",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-4*time.Minute), []corev1.ContainerStatus{waiting("checkout", "ErrImagePull")}, nil)},
			want: Observation{Exists: true},
		},
		{
			name: "not stuck: ContainerCreating for a long time",
			job:  jobWith("j"),
			pods: []*corev1.Pod{podFor("j", inspectNow.Add(-20*time.Minute), []corev1.ContainerStatus{waiting("checkout", "ContainerCreating")}, nil)},
			want: Observation{Exists: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			name := "j"
			if c.job != nil {
				if _, err := cs.BatchV1().Jobs("reviews").Create(context.Background(), c.job, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			for _, p := range c.pods {
				if _, err := cs.CoreV1().Pods("reviews").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			// a pod of another job must be ignored
			other := podFor("other", inspectNow.Add(-time.Hour), []corev1.ContainerStatus{terminated("checkout", 1, "Error", cloneStart, cloneEnd)}, nil)
			if _, err := cs.CoreV1().Pods("reviews").Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			l := &Launcher{Client: cs, Namespace: "reviews"}
			got, err := l.Inspect(context.Background(), name, inspectNow)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
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
	if got.Stuck != "" || got.Failure != "the review job hit its deadline (DeadlineExceeded)" {
		t.Errorf("got %+v", got)
	}
}
