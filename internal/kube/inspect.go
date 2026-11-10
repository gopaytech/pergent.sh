package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// stuckAfter is how long a pod may sit on an unrecoverable start error before
// the Job counts as stuck.
const stuckAfter = 5 * time.Minute

// The start rules. A pod the scheduler can't place, and a Job that makes no
// pod, count as stuck once they have waited for more than this. A pod that is
// scheduled and still pulling or creating is never stuck by them.
const (
	unscheduledAfter = 10 * time.Minute
	noPodAfter       = 10 * time.Minute
)

// The steps a review Job fails at.
const (
	StepStarting = "starting"
	StepClone    = "clone"
	StepReview   = "review"
	StepDelivery = "delivery"
	StepUnknown  = "unknown"
)

// Failure is why a Job failed or can't start, and at which step. Reason is
// safe for everyone to read; Detail (a scheduler or event message) is for
// admins only. The zero Failure is none.
type Failure struct {
	Step, Reason, Detail string
	// Deadline is set when the Job's deadline passed, so the engine can refine
	// Step from what earlier looks at the Job saw.
	Deadline bool
}

// Observation is what the cluster says about one review Job.
type Observation struct {
	Exists, Finished bool
	FinishedAt       time.Time
	// Failure says why a finished Job failed (zero for a Complete one).
	Failure Failure
	// Stuck says why an unfinished Job can't start (zero when it can).
	Stuck Failure
	// Clone is how long the checkout init container ran, when it has finished.
	Clone time.Duration
	// PodName is the newest pod of the Job ("" when it has none).
	PodName string
	// Checkout is what was seen of the checkout container: 0 nothing, 1
	// running, 2 finished with exit 0.
	Checkout int
}

// DeadlineFailure is the failure of a Job whose deadline passed, by how far its
// checkout container got (Observation.Checkout): not started or unseen, running,
// or done.
func DeadlineFailure(checkout int) Failure {
	switch checkout {
	case 1:
		return Failure{Step: StepClone, Reason: "the clone didn't finish before the deadline (DeadlineExceeded)", Deadline: true}
	case 2:
		return Failure{Step: StepReview, Reason: "the review didn't finish before the deadline (DeadlineExceeded)", Deadline: true}
	}
	return Failure{Step: StepUnknown, Reason: "the review job hit its deadline (DeadlineExceeded)", Deadline: true}
}

var stuckReasons = map[string]bool{
	"ErrImagePull":               true,
	"ImagePullBackOff":           true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
}

// jobFacts is what Inspect read of a Job that Observation has no field for,
// for the Jobs whose failure text is not a review's (ScanFailure).
type jobFacts struct {
	// failReason is the reason of the Job's Failed condition.
	failReason string
	// oom: a container of a pod was OOMKilled.
	oom bool
	// storage: a pod was evicted, or a message names ephemeral storage.
	storage bool
}

// storageMessage reports whether a pod or Job message says the pod used more
// ephemeral storage than its limit.
func storageMessage(s string) bool {
	return strings.Contains(s, "ephemeral-storage") || strings.Contains(s, "ephemeral local storage")
}

// Inspect reads the Job and its pods. A missing Job is Exists=false, not an
// error. now is the reference for the stuck rules.
func (l *Launcher) Inspect(ctx context.Context, name string, now time.Time) (Observation, error) {
	obs, _, err := l.inspect(ctx, name, now)
	return obs, err
}

func (l *Launcher) inspect(ctx context.Context, name string, now time.Time) (Observation, jobFacts, error) {
	job, err := l.Client.BatchV1().Jobs(l.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Observation{}, jobFacts{}, nil
	}
	if err != nil {
		return Observation{}, jobFacts{}, fmt.Errorf("get job %s: %w", name, err)
	}
	pods, err := l.Client.CoreV1().Pods(l.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err != nil {
		return Observation{}, jobFacts{}, fmt.Errorf("list pods of job %s: %w", name, err)
	}
	sort.SliceStable(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.Before(&pods.Items[j].CreationTimestamp)
	})

	obs := Observation{Exists: true}
	if n := len(pods.Items); n > 0 {
		obs.PodName = pods.Items[n-1].Name
	}
	var failed *batchv1.JobCondition
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			obs.Finished = true
			obs.FinishedAt = c.LastTransitionTime.Time
		case batchv1.JobFailed:
			obs.Finished = true
			obs.FinishedAt = c.LastTransitionTime.Time
			failed = c
		}
	}
	if obs.Finished && obs.FinishedAt.IsZero() && job.Status.CompletionTime != nil {
		obs.FinishedAt = job.Status.CompletionTime.Time
	}

	var facts jobFacts
	if failed != nil {
		facts.failReason = failed.Reason
		facts.storage = storageMessage(failed.Message)
	}
	var initExit *int32
	// cloneOOM: the checkout container was OOMKilled; oom: any other was.
	var cloneOOM, oom, exit4 bool
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Reason == "Evicted" || storageMessage(pod.Status.Message) {
			facts.storage = true
		}
		if !obs.Finished && obs.Stuck.Reason == "" {
			obs.Stuck = unscheduledFailure(pod, now)
		}
		for _, cs := range pod.Status.InitContainerStatuses {
			t := cs.State.Terminated
			if cs.Name == containerCheckout {
				switch {
				case cs.State.Running != nil:
					obs.Checkout = max(obs.Checkout, 1)
				case t != nil && t.ExitCode == 0:
					obs.Checkout = 2
				}
			}
			if cs.Name == containerCheckout && t != nil {
				obs.Clone = t.FinishedAt.Sub(t.StartedAt.Time)
				if t.ExitCode != 0 && initExit == nil {
					code := t.ExitCode
					initExit = &code
				}
			}
			if t != nil && t.Reason == "OOMKilled" {
				if cs.Name == containerCheckout {
					cloneOOM = true
				} else {
					oom = true
				}
			}
			if t != nil && storageMessage(t.Message) {
				facts.storage = true
			}
			if !obs.Finished && obs.Stuck.Reason == "" && now.Sub(pod.CreationTimestamp.Time) > stuckAfter {
				obs.Stuck = imageFailure(cs)
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			t := cs.State.Terminated
			if t != nil && t.Reason == "OOMKilled" {
				oom = true
			}
			if t != nil && storageMessage(t.Message) {
				facts.storage = true
			}
			if t != nil && cs.Name == containerPergent && t.ExitCode == 4 {
				exit4 = true
			}
			if !obs.Finished && obs.Stuck.Reason == "" && now.Sub(pod.CreationTimestamp.Time) > stuckAfter {
				obs.Stuck = imageFailure(cs)
			}
		}
	}

	if !obs.Finished && len(pods.Items) == 0 && obs.Stuck.Reason == "" &&
		!job.CreationTimestamp.IsZero() && now.Sub(job.CreationTimestamp.Time) > noPodAfter {
		events, _, err := l.ObjectEvents(ctx, "Job", name)
		if err != nil {
			return Observation{}, jobFacts{}, fmt.Errorf("list events of job %s: %w", name, err)
		}
		obs.Stuck = Failure{Step: StepStarting, Reason: "the review couldn't start: no pod could be created", Detail: failedCreate(events)}
	}

	if failed != nil {
		switch {
		case failed.Reason == "DeadlineExceeded" && !cloneOOM && initExit != nil && (*initExit == 137 || *initExit == 143):
			// The deadline killed the checkout: not a failed clone.
			obs.Failure = DeadlineFailure(1)
		case cloneOOM:
			obs.Failure = Failure{Step: StepClone, Reason: "the clone ran out of memory (OOMKilled)"}
		case initExit != nil:
			obs.Failure = Failure{Step: StepClone, Reason: fmt.Sprintf("the clone failed (checkout exited with code %d)", *initExit)}
		case oom:
			obs.Failure = Failure{Step: StepReview, Reason: "the review job ran out of memory (OOMKilled)"}
		case exit4:
			obs.Failure = Failure{Step: StepDelivery, Reason: "the review job couldn't deliver its result (exit code 4)"}
		case failed.Reason == "DeadlineExceeded":
			obs.Failure = DeadlineFailure(obs.Checkout)
		default:
			reason := failed.Reason
			if reason == "" {
				reason = "unknown reason"
			}
			obs.Failure = Failure{Step: StepUnknown, Reason: fmt.Sprintf("the review job failed (%s)", reason)}
		}
	}
	facts.oom = oom || cloneOOM
	return obs, facts, nil
}

// imageFailure is the stuck failure for a container waiting on an image or
// config error that won't clear, or none.
func imageFailure(cs corev1.ContainerStatus) Failure {
	if w := cs.State.Waiting; w != nil && stuckReasons[w.Reason] {
		return Failure{Step: StepStarting, Reason: fmt.Sprintf("the review couldn't start (%s)", w.Reason)}
	}
	return Failure{}
}

// unscheduledFailure is the stuck failure for a pod the scheduler could not
// place for more than unscheduledAfter, or none. The clock is when PodScheduled
// turned False, or the pod's creation when the cluster left that out. The
// scheduler's message is the detail.
func unscheduledFailure(pod *corev1.Pod, now time.Time) Failure {
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse {
			continue
		}
		since := c.LastTransitionTime.Time
		if since.IsZero() {
			since = pod.CreationTimestamp.Time
		}
		if since.IsZero() || now.Sub(since) <= unscheduledAfter {
			return Failure{}
		}
		return Failure{Step: StepStarting, Reason: "the review couldn't start: no node could run it", Detail: c.Message}
	}
	return Failure{}
}

// failedCreate is the message of the newest FailedCreate event of events
// (oldest first), or "".
func failedCreate(events []corev1.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Reason == "FailedCreate" {
			return events[i].Message
		}
	}
	return ""
}

// ObjectEvents lists the events about one object of the namespace (kind "Job"
// or "Pod"), oldest first. A cluster that refuses to list events (the chart's
// Role may lack it) is no events, forbidden=true and no error.
func (l *Launcher) ObjectEvents(ctx context.Context, kind, name string) (events []corev1.Event, forbidden bool, err error) {
	list, err := l.Client.CoreV1().Events(l.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=" + kind + ",involvedObject.name=" + name,
	})
	if apierrors.IsForbidden(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	var out []corev1.Event
	for _, e := range list.Items {
		// the selector does the work; this keeps a server that ignores it honest
		if e.InvolvedObject.Kind == kind && e.InvolvedObject.Name == name {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return eventTime(out[i]).Before(eventTime(out[j])) })
	return out, false, nil
}

// eventTime is when an event last happened.
func eventTime(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}
