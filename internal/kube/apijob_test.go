package kube

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// apiParams is a Job for an API review: no clone, so no repository URL, ref,
// token or change labels, and the stream the checkout step builds commits from.
func apiParams() JobParams {
	p := testParams()
	p.RepoURL, p.ChangeRef, p.TargetBranch, p.GitUsername = "", "", "", ""
	p.Instance, p.ProjectPath, p.MRURL, p.ProjectID, p.IID = "", "", "", 0, 0
	p.Import = []byte("blob\nmark :1\ndata 2\nhi\n")
	p.APIClient = "gateway-ci"
	return p
}

func envNames(c corev1.Container) []string {
	var names []string
	for _, e := range c.Env {
		names = append(names, e.Name)
	}
	return names
}

func TestAPIJobShape(t *testing.T) {
	p := apiParams()
	job := BuildJob(p)
	pod := job.Spec.Template.Spec

	checkout := containerByName(t, pod.InitContainers, "checkout")
	if len(checkout.Command) != 3 || checkout.Command[2] != apiCheckoutScript || checkout.Command[2] == checkoutScript {
		t.Errorf("the checkout command is not apicheckout.sh: %q", checkout.Command)
	}
	if !strings.Contains(checkout.Command[2], "git fast-import") {
		t.Errorf("the checkout script does not run git fast-import:\n%s", checkout.Command[2])
	}
	got := strings.Join(envNames(checkout), ",")
	if got != "BASE_SHA,HEAD_SHA" {
		t.Errorf("checkout env = %s; want BASE_SHA,HEAD_SHA only (no GIT_TOKEN, REPO_URL or MR_REF)", got)
	}
	for _, e := range checkout.Env {
		if e.ValueFrom != nil {
			t.Errorf("checkout env %s reads a Secret", e.Name)
		}
		if e.Name == "BASE_SHA" && e.Value != testBase || e.Name == "HEAD_SHA" && e.Value != testHead {
			t.Errorf("checkout env %s = %q", e.Name, e.Value)
		}
	}

	// Only the checkout container mounts the files.
	var mount *corev1.VolumeMount
	for i, m := range checkout.VolumeMounts {
		if m.Name == "files" {
			mount = &checkout.VolumeMounts[i]
		}
	}
	if mount == nil || mount.MountPath != "/files" || !mount.ReadOnly {
		t.Errorf("checkout files mount = %+v; want read-only at /files", mount)
	}
	pergent := containerByName(t, pod.Containers, "pergent")
	for _, m := range pergent.VolumeMounts {
		if m.Name == "files" || strings.HasPrefix(m.MountPath, "/files") {
			t.Errorf("the pergent container mounts the files: %+v", m)
		}
	}

	var vol *corev1.Volume
	for i, v := range pod.Volumes {
		if v.Name == "files" {
			vol = &pod.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil {
		t.Fatalf("no Secret volume named files in %+v", pod.Volumes)
	}
	s := vol.Secret
	if s.SecretName != p.Name+"-files" || len(s.Items) != 1 || s.Items[0].Key != "import" || s.Items[0].Path != "import" {
		t.Errorf("files volume = %+v; want Secret %s-files with item import", s, p.Name)
	}
	// Readable by any user the image runs as; only the checkout container mounts it.
	if s.DefaultMode == nil || *s.DefaultMode != 0o444 {
		t.Errorf("files volume mode = %v; want 0444", s.DefaultMode)
	}
	if pod.SecurityContext != nil {
		t.Errorf("pod security context = %+v; want none", pod.SecurityContext)
	}

	// The pergent container still gets the Job's own Secret and the same args.
	if !strings.Contains(strings.Join(pergent.Args, " "), "--base="+testBase) {
		t.Errorf("pergent args = %v", pergent.Args)
	}

	for what, labels := range map[string]map[string]string{"job": job.Labels, "pod": job.Spec.Template.Labels} {
		if labels[LabelAPIClient] != "gateway-ci" || labels["pergent.sh/run"] != p.RunID {
			t.Errorf("%s labels = %v", what, labels)
		}
		for _, k := range []string{LabelInstance, LabelProjectID, LabelMR} {
			if v, ok := labels[k]; ok {
				t.Errorf("%s has label %s = %q", what, k, v)
			}
		}
	}
	if job.Annotations != nil {
		t.Errorf("annotations = %v; want none", job.Annotations)
	}
}

// An odd client name never stops a Job: the label is left out.
func TestAPIJobLabelLeavesOutInvalidName(t *testing.T) {
	p := apiParams()
	p.APIClient = "Bad Name/x"
	if v, ok := BuildJob(p).Labels[LabelAPIClient]; ok {
		t.Errorf("label %s = %q; want it left out", LabelAPIClient, v)
	}
	p.APIClient = ""
	if _, ok := BuildJob(p).Labels[LabelAPIClient]; ok {
		t.Error("an empty client name got a label")
	}
}

// A merge request Job has no files volume and no pod security context.
func TestMRJobHasNoFilesVolume(t *testing.T) {
	job := BuildJob(testParams())
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "files" {
			t.Errorf("a merge request Job has the files volume: %+v", v)
		}
	}
	if job.Spec.Template.Spec.SecurityContext != nil {
		t.Errorf("a merge request Job got a pod security context: %+v", job.Spec.Template.Spec.SecurityContext)
	}
}

func TestBuildFilesSecret(t *testing.T) {
	p := apiParams()
	job := BuildJob(p)
	job.UID = "job-uid-123"
	sec := BuildFilesSecret(p, job)
	if sec.Name != p.Name+"-files" || sec.Namespace != p.Namespace || sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("secret = %s/%s %s", sec.Namespace, sec.Name, sec.Type)
	}
	if len(sec.Data) != 1 || !bytes.Equal(sec.Data["import"], p.Import) {
		t.Errorf("secret data keys = %d; want one, import, holding the stream", len(sec.Data))
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != job.Name || sec.OwnerReferences[0].UID != "job-uid-123" {
		t.Errorf("owner references = %+v; want the Job", sec.OwnerReferences)
	}
	if sec.Labels[LabelAPIClient] != "gateway-ci" {
		t.Errorf("labels = %v", sec.Labels)
	}
}

func TestLaunchAPIJobCreatesFilesSecret(t *testing.T) {
	l, cs := fakeLauncher()
	p := apiParams()
	ctx := context.Background()
	if err := l.Launch(ctx, p, Secrets{LLMKey: "l", ResultToken: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.BatchV1().Jobs("reviews").Get(ctx, p.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("job: %v", err)
	}
	for _, name := range []string{p.Name + "-secrets", p.Name + "-files"} {
		sec, err := cs.CoreV1().Secrets("reviews").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("secret %s: %v", name, err)
		}
		if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Job" || sec.OwnerReferences[0].UID != "job-uid-123" {
			t.Errorf("%s owner references = %+v; want the Job", name, sec.OwnerReferences)
		}
	}
	if _, err := cs.CoreV1().ConfigMaps("reviews").Get(ctx, p.Name+"-inputs", metav1.GetOptions{}); err != nil {
		t.Fatalf("inputs: %v", err)
	}
	// The files are in no other object.
	secrets, _ := cs.CoreV1().Secrets("reviews").List(ctx, metav1.ListOptions{})
	for _, sec := range secrets.Items {
		if sec.Name == p.Name+"-files" {
			continue
		}
		for k, v := range sec.Data {
			if bytes.Contains(v, p.Import) {
				t.Errorf("secret %s key %s holds the files", sec.Name, k)
			}
		}
	}

	if err := l.DeleteFiles(ctx, p.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Secrets("reviews").Get(ctx, p.Name+"-files", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("after DeleteFiles, get = %v; want NotFound", err)
	}
	if _, err := cs.CoreV1().Secrets("reviews").Get(ctx, p.Name+"-secrets", metav1.GetOptions{}); err != nil {
		t.Errorf("DeleteFiles removed the Job's own secret: %v", err)
	}
	if err := l.DeleteFiles(ctx, p.Name); err != nil {
		t.Errorf("a second DeleteFiles = %v; want nil", err)
	}
}

// A merge request Job makes no files Secret.
func TestLaunchMRJobCreatesNoFilesSecret(t *testing.T) {
	l, cs := fakeLauncher()
	p := testParams()
	if err := l.Launch(context.Background(), p, Secrets{GitToken: "g", LLMKey: "l", ResultToken: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Secrets("reviews").Get(context.Background(), p.Name+"-files", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("get files secret = %v; want NotFound", err)
	}
}

func TestLaunchRefusesLargeImport(t *testing.T) {
	l, cs := fakeLauncher()
	p := apiParams()
	p.Import = bytes.Repeat([]byte("a"), FilesSecretMaxBytes+1)
	err := l.Launch(context.Background(), p, Secrets{LLMKey: "l", ResultToken: "r"})
	if err == nil {
		t.Fatal("Launch accepted a stream over the limit")
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("Launch created %s before refusing", a.GetResource().Resource)
		}
	}

	// The limit itself is allowed.
	p.Import = bytes.Repeat([]byte("a"), FilesSecretMaxBytes)
	if err := l.Launch(context.Background(), p, Secrets{LLMKey: "l", ResultToken: "r"}); err != nil {
		t.Errorf("a stream of exactly %d bytes: %v", FilesSecretMaxBytes, err)
	}
}
