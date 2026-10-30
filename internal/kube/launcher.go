package kube

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Launcher creates, deletes and inspects review Jobs in one namespace.
type Launcher struct {
	Client    kubernetes.Interface
	Namespace string
	// LogStream opens a container's log; nil reads it through the pods/log
	// API of Client. Tests set it, as the fake clientset can't stream.
	LogStream func(ctx context.Context, pod, container string, tailLines int64) (io.ReadCloser, error)
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

// logTailLines bounds what is asked of the API server for one log.
const logTailLines = 20000

// Logs returns the last keepBytes bytes of the pergent container's log in the
// newest pod of the Job, cut on a UTF-8 boundary. It fails when the Job has
// no pod or the container has no log to give.
func (l *Launcher) Logs(ctx context.Context, jobName string, keepBytes int) (string, error) {
	pods, err := l.Client.CoreV1().Pods(l.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil {
		return "", fmt.Errorf("list pods of job %s: %w", jobName, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("job %s has no pod", jobName)
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.Before(&pods.Items[j].CreationTimestamp)
	})
	pod := pods.Items[len(pods.Items)-1].Name

	stream := l.LogStream
	if stream == nil {
		stream = l.clientLogStream
	}
	body, err := stream(ctx, pod, containerPergent, logTailLines)
	if err != nil {
		return "", fmt.Errorf("read the log of pod %s: %w", pod, err)
	}
	defer body.Close()
	tail, err := readTail(body, keepBytes)
	if err != nil {
		return "", fmt.Errorf("read the log of pod %s: %w", pod, err)
	}
	return tail, nil
}

func (l *Launcher) clientLogStream(ctx context.Context, pod, container string, tailLines int64) (io.ReadCloser, error) {
	return l.Client.CoreV1().Pods(l.Namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, TailLines: &tailLines,
	}).Stream(ctx)
}

// readTail reads r to its end and returns its last keep bytes, without the
// partial character a cut may leave at the start. It never holds much more
// than 2*keep bytes.
func readTail(r io.Reader, keep int) (string, error) {
	if keep <= 0 {
		return "", nil
	}
	var buf []byte
	chunk := make([]byte, 32*1024)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) > 2*keep {
			buf = append(buf[:0], buf[len(buf)-keep:]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if len(buf) > keep {
		buf = buf[len(buf)-keep:]
		for len(buf) > 0 && !utf8.RuneStart(buf[0]) {
			buf = buf[1:]
		}
	}
	return string(buf), nil
}

// JobSecrets reads the credentials of the Job's own Secret. They are the
// values to hide from anything its pod logged.
func (l *Launcher) JobSecrets(ctx context.Context, jobName string) (Secrets, error) {
	sec, err := l.Client.CoreV1().Secrets(l.Namespace).Get(ctx, secretsName(JobParams{Name: jobName}), metav1.GetOptions{})
	if err != nil {
		return Secrets{}, fmt.Errorf("get the secret of job %s: %w", jobName, err)
	}
	return Secrets{
		GitToken:    string(sec.Data[keyGitToken]),
		LLMKey:      string(sec.Data[keyLLMKey]),
		ResultToken: string(sec.Data[keyResultToken]),
	}, nil
}
