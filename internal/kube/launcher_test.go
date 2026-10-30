package kube

import (
	"context"
	"errors"
	"testing"

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
