package kube

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Launcher creates, deletes and inspects review Jobs in one namespace.
type Launcher struct {
	Client    kubernetes.Interface
	Namespace string
}

// Launch creates the Job suspended, then its Secret and ConfigMap owned by it,
// then unsuspends it, so the pod never starts without its inputs. If anything
// fails after the Job exists, the Job (and with it the Secret and ConfigMap) is
// deleted and the original error returned.
func (l *Launcher) Launch(ctx context.Context, p JobParams, s Secrets) error {
	p.Namespace = l.Namespace

	job := BuildJob(p)
	job.Spec.Suspend = ptr(true)
	created, err := l.Client.BatchV1().Jobs(l.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create job %s: %w", p.Name, err)
	}

	if err := l.attachAndStart(ctx, p, s, created); err != nil {
		// The caller's context may be what failed; clean up regardless.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = l.Delete(cctx, p.Name)
		return err
	}
	return nil
}

func (l *Launcher) attachAndStart(ctx context.Context, p JobParams, s Secrets, job *batchv1.Job) error {
	if _, err := l.Client.CoreV1().Secrets(l.Namespace).Create(ctx, BuildSecret(p, job, s), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create secret for job %s: %w", p.Name, err)
	}
	if _, err := l.Client.CoreV1().ConfigMaps(l.Namespace).Create(ctx, BuildConfigMap(p, job), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create inputs for job %s: %w", p.Name, err)
	}
	patch := []byte(`{"spec":{"suspend":false}}`)
	if _, err := l.Client.BatchV1().Jobs(l.Namespace).Patch(ctx, p.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("unsuspend job %s: %w", p.Name, err)
	}
	return nil
}

// Delete removes the Job with background propagation, so its pods, Secret and
// ConfigMap go with it. A Job that is already gone is not an error.
func (l *Launcher) Delete(ctx context.Context, name string) error {
	policy := metav1.DeletePropagationBackground
	err := l.Client.BatchV1().Jobs(l.Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete job %s: %w", name, err)
	}
	return nil
}
