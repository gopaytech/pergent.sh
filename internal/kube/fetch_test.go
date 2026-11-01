package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

const testFetchID = "5e1d7c3a-9b2f-4c8e-a6d4-0f1e2d3c4b5a"

func testFetchParams() FetchJobParams {
	return FetchJobParams{
		Name:      FetchJobName(testFetchID),
		Namespace: "reviews",
		FetchID:   testFetchID,
		GitImage:  "ghcr.io/gopaytech/pergent:v0.1.1",
		Image:     "ghcr.io/gopaytech/pergent.sh:v0.3.0",
		URL:       "https://example.com/acme/skills.git",
		Ref:       "release/v1",
		Path:      "skills",
		ResultURL: "http://pergent-sh.pergent-sh.svc:8080/skill-fetches/" + testFetchID + "/result",
		Timeout:   2 * time.Minute,
	}
}

func TestFetchJobName(t *testing.T) {
	if got := FetchJobName(testFetchID); got != "pergent-fetch-5e1d7c3a9b2f" {
		t.Errorf("FetchJobName = %q", got)
	}
}

// The clone runs with no token of any kind and the URL only in its
// environment; the sender gets only the fetch token, from the Job's Secret.
// Neither has a service account token, both are locked down, and the pod
// carries the label the review-Job NetworkPolicy selects.
func TestBuildFetchJob(t *testing.T) {
	p := testFetchParams()
	job := BuildFetchJob(p)

	if job.Name != p.Name || job.Namespace != "reviews" {
		t.Errorf("job = %s/%s", job.Namespace, job.Name)
	}
	for what, l := range map[string]map[string]string{"job": job.Labels, "pod": job.Spec.Template.Labels} {
		if l[labelManagedBy] != managedBy {
			t.Errorf("%s labels = %v; want %s=%s", what, l, labelManagedBy, managedBy)
		}
	}
	spec := job.Spec.Template.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the pod must not mount a service account token")
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Error("the pod must not get service links")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 120 {
		t.Errorf("activeDeadlineSeconds = %v; want 120", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v; want 0", job.Spec.BackoffLimit)
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", spec.RestartPolicy)
	}
	if sc := spec.SecurityContext; sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("the pod must run as non-root")
	}
	for _, v := range spec.Volumes {
		if v.Secret != nil || v.Projected != nil || v.ConfigMap != nil {
			t.Errorf("volume %s mounts a secret, projection or config map", v.Name)
		}
	}

	if len(spec.InitContainers) != 1 || len(spec.Containers) != 1 {
		t.Fatalf("containers = %d init, %d main", len(spec.InitContainers), len(spec.Containers))
	}
	clone := containerByName(t, spec.InitContainers, "clone")
	send := containerByName(t, spec.Containers, "send")

	if clone.Image != p.GitImage || send.Image != p.Image {
		t.Errorf("images = %s, %s", clone.Image, send.Image)
	}
	if len(clone.Command) != 3 || clone.Command[0] != "sh" || clone.Command[1] != "-ec" || clone.Command[2] != fetchScript {
		t.Errorf("clone command = %q", clone.Command)
	}
	env := map[string]string{}
	for _, e := range clone.Env {
		if e.ValueFrom != nil {
			t.Errorf("clone env %s comes from %v; the clone gets no secret", e.Name, e.ValueFrom)
		}
		if strings.Contains(e.Name, "TOKEN") || strings.Contains(e.Name, "PASSWORD") {
			t.Errorf("clone env %s looks like a credential", e.Name)
		}
		env[e.Name] = e.Value
	}
	if env["FETCH_URL"] != p.URL || env["FETCH_REF"] != p.Ref {
		t.Errorf("clone env = %v", env)
	}
	if len(clone.EnvFrom) != 0 || len(send.EnvFrom) != 0 {
		t.Error("no container may take a whole Secret or ConfigMap as env")
	}

	// The URL is in the clone's environment only: never in a command, an
	// argument or the sender's environment.
	for _, c := range []corev1.Container{clone, send} {
		for _, s := range append(append([]string{}, c.Command...), c.Args...) {
			if strings.Contains(s, p.URL) || strings.Contains(s, "example.com") {
				t.Errorf("%s command or args hold the URL: %q", c.Name, s)
			}
		}
	}
	for _, e := range send.Env {
		if strings.Contains(e.Value, "example.com") {
			t.Errorf("send env %s holds the URL", e.Name)
		}
	}

	if len(send.Command) != 1 || send.Command[0] != "/pergent-sh" {
		t.Errorf("send command = %q", send.Command)
	}
	wantArgs := []string{"fetch-skills", "--dir=/work/repo", "--path=skills", "--commit-file=/work/commit",
		"--error-file=/work/error", "--result-url=" + p.ResultURL}
	if strings.Join(send.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("send args = %q; want %q", send.Args, wantArgs)
	}
	if len(send.Env) != 1 || send.Env[0].Name != "PERGENT_SH_FETCH_TOKEN" || send.Env[0].ValueFrom == nil ||
		send.Env[0].ValueFrom.SecretKeyRef == nil || send.Env[0].ValueFrom.SecretKeyRef.Name != p.Name+"-secrets" ||
		send.Env[0].ValueFrom.SecretKeyRef.Key != keyFetchToken {
		t.Errorf("send env = %+v; want only the fetch token from the Job's Secret", send.Env)
	}

	for _, c := range []corev1.Container{clone, send} {
		sc := c.SecurityContext
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
			sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s is not locked down: %+v", c.Name, sc)
		}
		if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s must have a read-only root filesystem", c.Name)
		}
		if c.Resources.Limits.Memory().IsZero() {
			t.Errorf("%s has no memory limit", c.Name)
		}
	}
}

// The clone script holds no credential and runs no hook: a public URL only,
// one ref, depth 1.
func TestFetchScriptHasNoCredentials(t *testing.T) {
	for _, bad := range []string{"GIT_TOKEN", "extraHeader", "Authorization", "credential.helper=", "PERGENT_SH_FETCH_TOKEN"} {
		if strings.Contains(fetchScript, bad) {
			t.Errorf("fetch.sh holds %q", bad)
		}
	}
	for _, want := range []string{
		"core.hooksPath /dev/null",
		"--depth=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		`"$FETCH_URL"`,
		`"$FETCH_REF"`,
		"/work/commit",
		"/work/error",
		"could not fetch %s at %s; the URL, branch or tag may be wrong, or outside jobEgress.cidrs",
	} {
		if !strings.Contains(fetchScript, want) {
			t.Errorf("fetch.sh lacks %q", want)
		}
	}
	// The URL is only ever expanded in double quotes, never run as a command.
	if strings.Contains(fetchScript, "eval") || strings.Contains(fetchScript, " $FETCH_URL") {
		t.Error("fetch.sh must not eval the URL or expand it unquoted")
	}
}

// The Secret holds the fetch token and nothing else, owned by the Job.
func TestBuildFetchSecret(t *testing.T) {
	p := testFetchParams()
	owner := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: p.Name, UID: "uid-1"}}
	sec := BuildFetchSecret(p, owner, "tok-123")
	if sec.Name != p.Name+"-secrets" || sec.Namespace != "reviews" || sec.Labels[labelManagedBy] != managedBy {
		t.Errorf("secret meta = %+v", sec.ObjectMeta)
	}
	if len(sec.Data) != 1 || string(sec.Data[keyFetchToken]) != "tok-123" {
		t.Errorf("secret data keys = %d; want only the fetch token", len(sec.Data))
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != "uid-1" {
		t.Errorf("owner = %+v", sec.OwnerReferences)
	}
}

// LaunchFetch creates the Job suspended, then its Secret, then starts it; a
// Secret that can't be created takes the Job with it.
func TestLaunchFetch(t *testing.T) {
	l, cs := fakeLauncher()
	p := testFetchParams()
	p.Namespace = "ignored"
	if err := l.LaunchFetch(context.Background(), p, "tok"); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "patch" {
			order = append(order, a.GetVerb()+" "+a.GetResource().Resource+" "+a.GetNamespace())
		}
		if c, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "jobs" {
			if j := c.GetObject().(*batchv1.Job); j.Spec.Suspend == nil || !*j.Spec.Suspend {
				t.Error("the Job must be created suspended")
			}
		}
	}
	if got := strings.Join(order, ", "); got != "create jobs reviews, create secrets reviews, patch jobs reviews" {
		t.Errorf("actions = %s", got)
	}

	l, cs = fakeLauncher()
	cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota")
	})
	if err := l.LaunchFetch(context.Background(), testFetchParams(), "tok"); err == nil {
		t.Fatal("LaunchFetch = nil; want the Secret's error")
	}
	if _, err := cs.BatchV1().Jobs("reviews").Get(context.Background(), p.Name, metav1.GetOptions{}); err == nil {
		t.Error("the Job is left after its Secret failed")
	}
}
