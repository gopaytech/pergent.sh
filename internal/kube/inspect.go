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

// Observation is what the cluster says about one review Job.
type Observation struct {
	Exists, Finished bool
	FinishedAt       time.Time
	// Failure says why a finished Job failed ("" for a Complete one).
	Failure string
	// Stuck says why an unfinished Job can't start ("" when it can).
	Stuck string
	// Clone is how long the checkout init container ran, when it has finished.
	Clone time.Duration
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
// error. now is the reference for the stuck rule.
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
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.Before(&pods.Items[j].CreationTimestamp)
	})

	obs := Observation{Exists: true}
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
	var oom, exit4 bool
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Reason == "Evicted" || storageMessage(pod.Status.Message) {
			facts.storage = true
		}
		for _, cs := range pod.Status.InitContainerStatuses {
			t := cs.State.Terminated
			if cs.Name == containerCheckout && t != nil {
				obs.Clone = t.FinishedAt.Sub(t.StartedAt.Time)
				if t.ExitCode != 0 && initExit == nil {
					code := t.ExitCode
					initExit = &code
				}
			}
			if t != nil && t.Reason == "OOMKilled" {
				oom = true
			}
			if t != nil && storageMessage(t.Message) {
				facts.storage = true
			}
			if !obs.Finished && obs.Stuck == "" && now.Sub(pod.CreationTimestamp.Time) > stuckAfter {
				obs.Stuck = stuckReason(cs)
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
			if !obs.Finished && obs.Stuck == "" && now.Sub(pod.CreationTimestamp.Time) > stuckAfter {
				obs.Stuck = stuckReason(cs)
			}
		}
	}

	if failed != nil {
		switch {
		case initExit != nil:
			obs.Failure = fmt.Sprintf("couldn't fetch the merge request's commits (checkout exited with code %d)", *initExit)
		case oom:
			obs.Failure = "the review job ran out of memory (OOMKilled)"
		case exit4:
			obs.Failure = "the review job couldn't deliver its result (exit code 4)"
		case failed.Reason == "DeadlineExceeded":
			obs.Failure = "the review job hit its deadline (DeadlineExceeded)"
		default:
			reason := failed.Reason
			if reason == "" {
				reason = "unknown reason"
			}
			obs.Failure = fmt.Sprintf("the review job failed (%s)", reason)
		}
	}
	facts.oom = oom
	return obs, facts, nil
}

func stuckReason(cs corev1.ContainerStatus) string {
	if w := cs.State.Waiting; w != nil && stuckReasons[w.Reason] {
		return fmt.Sprintf("couldn't start the review job (%s)", w.Reason)
	}
	return ""
}
