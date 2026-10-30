package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func fakeLauncher() (*Launcher, *fake.Clientset) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		j := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		j.UID = types.UID("job-uid-123")
		return false, nil, nil
	})
	return &Launcher{Client: cs, Namespace: "reviews"}, cs
}

func TestLaunchOrder(t *testing.T) {
	l, cs := fakeLauncher()
	p := testParams()
	s := Secrets{GitToken: "g", LLMKey: "l", ResultToken: "r"}

	if err := l.Launch(context.Background(), p, s); err != nil {
		t.Fatal(err)
	}

	// Action order: job created suspended, then secret + configmap, then the patch.
	var order []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "patch" {
			order = append(order, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	want := []string{"create jobs", "create secrets", "create configmaps", "patch jobs"}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	for _, a := range cs.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "jobs" {
			j := c.GetObject().(*batchv1.Job)
			if j.Spec.Suspend == nil || !*j.Spec.Suspend {
				t.Errorf("job must be created suspended")
			}
		}
		if pa, ok := a.(k8stesting.PatchAction); ok {
			if string(pa.GetPatch()) != `{"spec":{"suspend":false}}` || pa.GetPatchType() != types.MergePatchType {
				t.Errorf("patch = %s (%s)", pa.GetPatch(), pa.GetPatchType())
			}
		}
	}

	ctx := context.Background()
	job, err := cs.BatchV1().Jobs("reviews").Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Suspend == nil || *job.Spec.Suspend {
		t.Errorf("job must be unsuspended after Launch, suspend=%v", job.Spec.Suspend)
	}
	sec, err := cs.CoreV1().Secrets("reviews").Get(ctx, p.Name+"-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm, err := cs.CoreV1().ConfigMaps("reviews").Get(ctx, p.Name+"-inputs", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for kind, refs := range map[string][]metav1.OwnerReference{"secret": sec.OwnerReferences, "configmap": cm.OwnerReferences} {
		if len(refs) != 1 || refs[0].UID != "job-uid-123" || refs[0].Kind != "Job" || refs[0].Name != p.Name || refs[0].APIVersion != "batch/v1" {
			t.Errorf("%s ownerRefs = %+v", kind, refs)
		}
	}
}

func TestLaunchCleansUpOnSecretError(t *testing.T) {
	l, cs := fakeLauncher()
	boom := errors.New("secret create denied")
	cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	p := testParams()

	err := l.Launch(context.Background(), p, Secrets{GitToken: "g", LLMKey: "l", ResultToken: "r"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the original error", err)
	}
	if _, err := cs.BatchV1().Jobs("reviews").Get(context.Background(), p.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("job must be deleted after failure, get err = %v", err)
	}
	var deleted bool
	for _, a := range cs.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok && d.GetResource().Resource == "jobs" {
			deleted = true
			if o := d.GetDeleteOptions().PropagationPolicy; o == nil || *o != metav1.DeletePropagationBackground {
				t.Errorf("propagation = %v", o)
			}
		}
	}
	if !deleted {
		t.Errorf("no delete issued")
	}
}

func TestLaunchCleansUpOnPatchError(t *testing.T) {
	l, cs := fakeLauncher()
	boom := errors.New("patch denied")
	cs.PrependReactor("patch", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	p := testParams()
	if err := l.Launch(context.Background(), p, Secrets{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := cs.BatchV1().Jobs("reviews").Get(context.Background(), p.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("job must be deleted, err = %v", err)
	}
}

func TestLaunchJobCreateError(t *testing.T) {
	l, cs := fakeLauncher()
	boom := errors.New("quota")
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	if err := l.Launch(context.Background(), testParams(), Secrets{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "delete" {
			t.Errorf("nothing to delete when the Job was never created")
		}
	}
}

func TestDeleteNotFoundIsNil(t *testing.T) {
	l, _ := fakeLauncher()
	if err := l.Delete(context.Background(), "nope"); err != nil {
		t.Errorf("Delete of a missing job = %v", err)
	}
}

func TestDeleteUsesBackgroundPropagation(t *testing.T) {
	l, cs := fakeLauncher()
	p := testParams()
	if err := l.Launch(context.Background(), p, Secrets{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(context.Background(), p.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.BatchV1().Jobs("reviews").Get(context.Background(), p.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("job still there: %v", err)
	}
	_ = corev1.Pod{}
}

// logPod stores a pod of job name created at created.
func logPod(t *testing.T, cs *fake.Clientset, job, pod string, created time.Time) {
	t.Helper()
	_, err := cs.CoreV1().Pods("reviews").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: pod, Namespace: "reviews", Labels: map[string]string{"job-name": job}, CreationTimestamp: metav1.NewTime(created),
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLaunchLogsTail(t *testing.T) {
	l, cs := fakeLauncher()
	now := time.Now()
	logPod(t, cs, "job-1", "job-1-old", now.Add(-time.Hour))
	logPod(t, cs, "job-1", "job-1-new", now)

	// 300 000 bytes, each one telling where it is.
	var big strings.Builder
	for i := 0; big.Len() < 300000; i++ {
		fmt.Fprintf(&big, "line %06d\n", i)
	}
	full := big.String()[:300000]

	var gotPod, gotContainer string
	var gotTail int64
	l.LogStream = func(_ context.Context, pod, container string, tail int64) (io.ReadCloser, error) {
		gotPod, gotContainer, gotTail = pod, container, tail
		return io.NopCloser(strings.NewReader(full)), nil
	}
	got, err := l.Logs(context.Background(), "job-1", 204800)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 204800 || got != full[len(full)-204800:] {
		t.Errorf("Logs returned %d bytes; want the last 204800 of the log", len(got))
	}
	if gotPod != "job-1-new" || gotContainer != "pergent" || gotTail != 20000 {
		t.Errorf("read pod %q container %q tail %d; want the newest pod's pergent container, 20000 lines", gotPod, gotContainer, gotTail)
	}

	// A small log comes back whole.
	l.LogStream = func(context.Context, string, string, int64) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("short\n")), nil
	}
	if got, err := l.Logs(context.Background(), "job-1", 204800); err != nil || got != "short\n" {
		t.Errorf("Logs = %q, %v; want the whole log", got, err)
	}
}

func TestLaunchLogsCutOnRuneBoundary(t *testing.T) {
	l, cs := fakeLauncher()
	logPod(t, cs, "job-1", "job-1-a", time.Now())
	// "é" is two bytes: a cut after 5 bytes lands inside the second one.
	l.LogStream = func(context.Context, string, string, int64) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("aéééé")), nil // 9 bytes
	}
	got, err := l.Logs(context.Background(), "job-1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got != "éé" || !utf8.ValidString(got) {
		t.Errorf("Logs = %q; want %q, cut on a character boundary", got, "éé")
	}
}

func TestLaunchLogsErrors(t *testing.T) {
	l, cs := fakeLauncher()
	if _, err := l.Logs(context.Background(), "job-1", 100); err == nil {
		t.Errorf("Logs of a Job without a pod succeeded")
	}
	logPod(t, cs, "job-1", "job-1-a", time.Now())
	l.LogStream = func(context.Context, string, string, int64) (io.ReadCloser, error) {
		return nil, errors.New("container is waiting to start")
	}
	if _, err := l.Logs(context.Background(), "job-1", 100); err == nil {
		t.Errorf("Logs succeeded though the stream failed")
	}
}

// Without an injected stream, the log is read through the pods/log API.
func TestLaunchLogsViaClient(t *testing.T) {
	l, cs := fakeLauncher()
	logPod(t, cs, "job-1", "job-1-a", time.Now())
	got, err := l.Logs(context.Background(), "job-1", 204800)
	if err != nil || got != "fake logs" {
		t.Errorf("Logs = %q, %v; want the fake clientset's log", got, err)
	}
}

func TestLaunchJobSecrets(t *testing.T) {
	l, _ := fakeLauncher()
	p := testParams()
	if err := l.Launch(context.Background(), p, Secrets{GitToken: "g1", LLMKey: "l1", ResultToken: "r1"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.JobSecrets(context.Background(), p.Name)
	if err != nil || got != (Secrets{GitToken: "g1", LLMKey: "l1", ResultToken: "r1"}) {
		t.Errorf("JobSecrets = %+v, %v", got, err)
	}
	if _, err := l.JobSecrets(context.Background(), "nope"); err == nil {
		t.Errorf("JobSecrets of a missing Job succeeded")
	}
}
