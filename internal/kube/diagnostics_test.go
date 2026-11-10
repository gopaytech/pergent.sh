package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func TestMaskLinked(t *testing.T) {
	clones := []LinkedClone{
		{Dir: "linked-1", CloneURL: "https://h/g/other.git"},
		{Dir: "linked-2", CloneURL: "https://h/g/other-two.git"},
	}
	cases := []struct{ name, in, want string }{
		{"with .git", "fatal: Authentication failed for 'https://h/g/other.git/'", "fatal: Authentication failed for 'linked-1/'"},
		{"without .git", "fatal: Authentication failed for https://h/g/other", "fatal: Authentication failed for linked-1"},
		{"both forms, one line", "https://h/g/other.git and https://h/g/other", "linked-1 and linked-1"},
		{"the longer name is not cut", "could not read https://h/g/other-two.git", "could not read linked-2"},
		{"longer without .git", "could not read https://h/g/other-two", "could not read linked-2"},
		{"credentials in the URL", "fatal: unable to access 'https://oauth2:s3cret@h/g/other.git/'", "fatal: unable to access 'linked-1/'"},
		{"an unlinked repository stays", "fetching https://h/g/main.git", "fetching https://h/g/main.git"},
		{"a longer name that is not linked stays", "https://h/g/other-three.git", "https://h/g/other-three.git"},
		{"no URL", "cloned linked-1", "cloned linked-1"},
	}
	for _, c := range cases {
		if got := MaskLinked(c.in, clones); got != c.want {
			t.Errorf("%s: MaskLinked(%q) = %q; want %q", c.name, c.in, got, c.want)
		}
	}
	// the clone listed first must not win over the longer one
	reversed := []LinkedClone{clones[1], clones[0]}
	if got := MaskLinked("https://h/g/other-two.git https://h/g/other.git", reversed); got != "linked-2 linked-1" {
		t.Errorf("reversed list: %q", got)
	}
	if got := MaskLinked("https://h/g/other.git", nil); got != "https://h/g/other.git" {
		t.Errorf("no clones changed the text: %q", got)
	}
	if got := MaskLinked("", clones); got != "" {
		t.Errorf("empty text: %q", got)
	}
}

func TestLinkedClonesFromJob(t *testing.T) {
	l, _ := fakeLauncher()
	p := linkedParams()
	if err := l.Launch(context.Background(), p, Secrets{GitToken: "g", LLMKey: "l", ResultToken: "r"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.LinkedClones(context.Background(), p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != p.LinkedClones[0] || got[1] != p.LinkedClones[1] {
		t.Errorf("LinkedClones = %v; want %v", got, p.LinkedClones)
	}

	// A Job without links has none; a missing Job is an error.
	q := testParams()
	q.Name = "pergent-review-none"
	if err := l.Launch(context.Background(), q, Secrets{GitToken: "g", LLMKey: "l", ResultToken: "r"}); err != nil {
		t.Fatal(err)
	}
	if got, err := l.LinkedClones(context.Background(), q.Name); err != nil || len(got) != 0 {
		t.Errorf("Job without links: %v, %v; want none", got, err)
	}
	if _, err := l.LinkedClones(context.Background(), "pergent-review-gone"); err == nil {
		t.Error("a missing Job gave no error")
	}
}

func TestCheckoutLogReadsCheckout(t *testing.T) {
	l, cs := fakeLauncher()
	logPod(t, cs, "job-1", "job-1-old", time.Now().Add(-time.Hour))
	logPod(t, cs, "job-1", "job-1-new", time.Now())
	var pod, container string
	l.LogStream = func(_ context.Context, p, c string, _ int64) (io.ReadCloser, error) {
		pod, container = p, c
		return io.NopCloser(strings.NewReader("could not clone linked-1\n")), nil
	}
	got, err := l.CheckoutLog(context.Background(), "job-1", CloneLogKeepBytes)
	if err != nil || got != "could not clone linked-1\n" {
		t.Fatalf("CheckoutLog = %q, %v", got, err)
	}
	if pod != "job-1-new" || container != "checkout" {
		t.Errorf("read pod %q container %q; want the newest pod's checkout container", pod, container)
	}
	// Logs still reads the pergent container.
	if _, err := l.Logs(context.Background(), "job-1", 100); err != nil || container != "pergent" {
		t.Errorf("Logs read container %q, %v; want pergent", container, err)
	}
	if CloneLogKeepBytes != 64*1024 {
		t.Errorf("CloneLogKeepBytes = %d", CloneLogKeepBytes)
	}
}

// stateEvent is an event of the given type with a count, about kind/name.
func stateEvent(kind, name, typ, reason, message string, count int32, at time.Time) *corev1.Event {
	e := objectEvent(kind, name, reason, message, at)
	e.Type, e.Count = typ, count
	return e
}

func TestPodStateText(t *testing.T) {
	pod := unscheduled("j", inspectNow.Add(-time.Hour), inspectNow.Add(-50*time.Minute), "0/3 nodes are available: 3 Insufficient memory.")
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{waiting("checkout", "ContainerCreating")}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "pergent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
	}}

	// 12 events, pod and Job, distinct times; the two oldest must fall off.
	var evs []*corev1.Event
	for i := 0; i < 12; i++ {
		kind, name, typ := "Pod", "j-abcde", corev1.EventTypeWarning
		if i%3 == 0 {
			kind, name = "Job", "j"
		}
		if i%2 == 0 {
			typ = corev1.EventTypeNormal
		}
		evs = append(evs, stateEvent(kind, name, typ, fmt.Sprintf("Reason%02d", i), fmt.Sprintf("message %02d", i), int32(i%4), inspectNow.Add(time.Duration(i-20)*time.Minute)))
	}
	// an event of another pod must stay out
	evs = append(evs, objectEvent("Pod", "j-other", "Other", "another pod", inspectNow))
	cs := inspectCluster(t, jobWith("j"), []*corev1.Pod{pod}, evs...)
	l := &Launcher{Client: cs, Namespace: "reviews"}

	text, forbidden, err := l.PodState(context.Background(), "j")
	if err != nil || forbidden {
		t.Fatalf("err = %v, forbidden = %v", err, forbidden)
	}
	for _, want := range []string{
		"Pod j-abcde: phase Pending",
		"PodScheduled=False: Unschedulable: 0/3 nodes are available: 3 Insufficient memory.",
		"checkout (init): waiting, reason ContainerCreating",
		"pergent: terminated, reason OOMKilled, exit code 137",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Other") {
		t.Errorf("an event of another pod is in the text:\n%s", text)
	}

	var lines []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.Contains(ln, " Reason") {
			lines = append(lines, ln)
		}
	}
	if len(lines) != podStateEvents {
		t.Fatalf("%d event lines, want %d:\n%s", len(lines), podStateEvents, text)
	}
	for k, ln := range lines {
		i := k + 2 // events 0 and 1 are the oldest and dropped
		typ := "Warning"
		if i%2 == 0 {
			typ = "Normal"
		}
		count := i % 4
		if count == 0 {
			count = 1 // an event with no count happened once
		}
		want := fmt.Sprintf("%s %s Reason%02d (x%d): message %02d",
			inspectNow.Add(time.Duration(i-20)*time.Minute).Format(time.RFC3339), typ, i, count, i)
		if ln != want {
			t.Errorf("event line %d = %q; want %q", k, ln, want)
		}
	}
}

func TestPodStateNoPod(t *testing.T) {
	cs := inspectCluster(t, jobWith("j"), nil,
		jobEvent("j", "FailedCreate", "exceeded quota: compute-resources", inspectNow),
		objectEvent("Pod", "j-other", "Other", "x", inspectNow))
	l := &Launcher{Client: cs, Namespace: "reviews"}
	text, forbidden, err := l.PodState(context.Background(), "j")
	if err != nil || forbidden {
		t.Fatalf("err = %v, forbidden = %v", err, forbidden)
	}
	want := inspectNow.Format(time.RFC3339) + " Warning FailedCreate (x1): exceeded quota: compute-resources"
	if !strings.Contains(text, want) || strings.Contains(text, "Pod ") || strings.Contains(text, "Other") {
		t.Errorf("text should be the Job's events only:\n%s", text)
	}
}

func TestPodStateEventsForbidden(t *testing.T) {
	pod := unscheduled("j", inspectNow.Add(-time.Hour), inspectNow.Add(-50*time.Minute), "no node")
	cs := inspectCluster(t, jobWith("j"), []*corev1.Pod{pod})
	cs.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("no list on events"))
	})
	l := &Launcher{Client: cs, Namespace: "reviews"}
	text, forbidden, err := l.PodState(context.Background(), "j")
	if err != nil || !forbidden {
		t.Fatalf("err = %v, forbidden = %v; want nil, true", err, forbidden)
	}
	if !strings.Contains(text, "Pod j-abcde: phase Pending") || !strings.Contains(text, "PodScheduled=False") {
		t.Errorf("pod lines missing:\n%s", text)
	}
}

func TestPodStateEventsError(t *testing.T) {
	pod := unscheduled("j", inspectNow.Add(-time.Hour), inspectNow.Add(-50*time.Minute), "no node")
	cs := inspectCluster(t, jobWith("j"), []*corev1.Pod{pod})
	cs.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	l := &Launcher{Client: cs, Namespace: "reviews"}
	if _, _, err := l.PodState(context.Background(), "j"); err == nil {
		t.Error("a failed event listing was not an error")
	}
}
