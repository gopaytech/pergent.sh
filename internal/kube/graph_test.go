package kube

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testBuildID = "3b1f6d20-8c4e-4a57-9e02-6f1d7c8a9b30"

func testGraphParams() GraphJobParams {
	return GraphJobParams{
		Name:        GraphJobName(testBuildID),
		Namespace:   "reviews",
		BuildID:     testBuildID,
		GitImage:    "ghcr.io/gopaytech/pergent:v0.1.2",
		GraphImage:  "ghcr.io/gopaytech/pergent.sh-graph:v0.4.0",
		Image:       "ghcr.io/gopaytech/pergent.sh:v0.4.0",
		RepoURL:     "https://git.example.com/acme/widgets.git",
		GitUsername: "pergent-bot",
		Branch:      "main",
		Commit:      "0123456789abcdef0123456789abcdef01234567",
		ResultURL:   "http://pergent-sh.pergent-sh.svc:8080/graphs/" + testBuildID + "/result",
	}
}

func TestGraphJobName(t *testing.T) {
	if got := GraphJobName(testBuildID); got != "pergent-graph-3b1f6d208c4e" {
		t.Errorf("GraphJobName = %q", got)
	}
}

// secretKeys lists NAME=key for each environment variable of c that comes
// from a Secret.
func secretKeys(c corev1.Container) []string {
	var out []string
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			out = append(out, e.Name+"="+e.ValueFrom.SecretKeyRef.Key)
		}
	}
	return out
}

// The clone holds the git token alone, graphify holds no secret at all and
// mounts no Secret, and the post container holds the build token alone. Each
// runs locked down, as a non-root user, and the Job runs once.
func TestBuildGraphJob(t *testing.T) {
	p := testGraphParams()
	job := BuildGraphJob(p)
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 2 || len(spec.Containers) != 1 {
		t.Fatalf("containers = %d init, %d main", len(spec.InitContainers), len(spec.Containers))
	}
	if spec.InitContainers[0].Name != "clone" || spec.InitContainers[1].Name != "graphify" {
		t.Fatalf("init order = %s, %s; want clone, graphify", spec.InitContainers[0].Name, spec.InitContainers[1].Name)
	}
	clone, graphify, post := spec.InitContainers[0], spec.InitContainers[1], spec.Containers[0]
	if clone.Image != p.GitImage || graphify.Image != p.GraphImage || post.Image != p.Image || post.Name != "post" {
		t.Errorf("images = %s, %s, %s (%s)", clone.Image, graphify.Image, post.Image, post.Name)
	}
	if got := secretKeys(clone); strings.Join(got, ",") != "GIT_TOKEN=git-token" {
		t.Errorf("clone secrets = %v", got)
	}
	if got := secretKeys(graphify); len(got) != 0 {
		t.Errorf("graphify secrets = %v; want none", got)
	}
	for _, e := range graphify.Env {
		if e.ValueFrom != nil {
			t.Errorf("graphify env %s comes from %+v", e.Name, e.ValueFrom)
		}
	}
	if got := secretKeys(post); strings.Join(got, ",") != "PERGENT_SH_GRAPH_TOKEN=graph-token" {
		t.Errorf("post secrets = %v", got)
	}
	for _, v := range spec.Volumes {
		if v.Secret != nil || v.ConfigMap != nil || v.Projected != nil {
			t.Errorf("volume %s is a Secret or ConfigMap; the Job needs none", v.Name)
		}
		if v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil {
			t.Errorf("volume %s = %+v; want a limited emptyDir", v.Name, v.VolumeSource)
		}
	}
	for _, c := range []corev1.Container{clone, graphify, post} {
		sc := c.SecurityContext
		if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
			sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.Capabilities == nil ||
			!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Errorf("%s security context = %+v", c.Name, sc)
		}
		if c.Resources.Limits.Memory().IsZero() {
			t.Errorf("%s has no memory limit", c.Name)
		}
	}
	// graphify reads the clone read-only and writes only its own volume.
	for _, m := range graphify.VolumeMounts {
		if m.Name == "work" && !m.ReadOnly {
			t.Errorf("graphify mounts the clone writable")
		}
	}
	for _, m := range post.VolumeMounts {
		if !m.ReadOnly {
			t.Errorf("post mounts %s writable", m.Name)
		}
	}
	if !slices.Contains(post.Args, "--result-url="+p.ResultURL) {
		t.Errorf("post args = %v", post.Args)
	}
	if !strings.Contains(strings.Join(graphify.Command, " "), "graphify extract") ||
		!strings.Contains(strings.Join(graphify.Command, " "), "pergent graph convert") {
		t.Errorf("graphify command = %v", graphify.Command)
	}
	ps := spec.SecurityContext
	if ps == nil || ps.RunAsNonRoot == nil || !*ps.RunAsNonRoot || ps.RunAsUser == nil || *ps.RunAsUser == 0 {
		t.Errorf("pod security context = %+v", ps)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the service account token is mounted")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v; want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64(GraphJobTimeout.Seconds()) {
		t.Errorf("deadline = %v", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.Template.Labels[labelManagedBy] != managedBy {
		t.Errorf("pod labels = %v; the NetworkPolicy needs the managed-by label", job.Spec.Template.Labels)
	}
}

// LaunchGraph refuses a URL with credentials and creates the Job with its
// Secret, which holds the two tokens and nothing else.
func TestLaunchGraph(t *testing.T) {
	cs := fake.NewSimpleClientset()
	l := &Launcher{Client: cs, Namespace: "reviews"}
	bad := testGraphParams()
	bad.RepoURL = "https://user:pass@git.example.com/acme/widgets.git"
	if err := l.LaunchGraph(context.Background(), bad, "git", "build"); err == nil {
		t.Fatal("launched with credentials in the URL")
	}
	p := testGraphParams()
	if err := l.LaunchGraph(context.Background(), p, "git-secret", "build-secret"); err != nil {
		t.Fatal(err)
	}
	sec, err := cs.CoreV1().Secrets("reviews").Get(context.Background(), p.Name+"-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sec.Data) != 2 || string(sec.Data["git-token"]) != "git-secret" || string(sec.Data["graph-token"]) != "build-secret" {
		t.Errorf("secret data = %v", sec.Data)
	}
	job, err := cs.BatchV1().Jobs("reviews").Get(context.Background(), p.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.Suspend == nil || *job.Spec.Suspend {
		// The fake clientset applies the merge patch.
		b, _ := json.Marshal(job.Spec)
		t.Errorf("job still suspended: %s", b)
	}
}

// A review Job with a graph URL gets one more init container, which holds the
// result token alone and writes only the graph volume; the pergent container
// reads that volume read-only and gets --graph. The repository volume is not
// the graph's.
func TestBuildJobWithGraph(t *testing.T) {
	p := testParams()
	p.GraphURL = "http://pergent-sh:8080/runs/" + p.RunID + "/graph"
	p.GraphImage = "ghcr.io/gopaytech/pergent.sh:v0.4.0"
	job := BuildJob(p)
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 2 || spec.InitContainers[0].Name != "checkout" || spec.InitContainers[1].Name != "graph" {
		t.Fatalf("init containers = %+v", spec.InitContainers)
	}
	g := spec.InitContainers[1]
	if g.Image != p.GraphImage || strings.Join(g.Command, " ") != "/pergent-sh" ||
		!slices.Equal(g.Args, []string{"get-graph", "--url=" + p.GraphURL, "--out=/graph/graph.json"}) {
		t.Errorf("graph container = %s %v %v", g.Image, g.Command, g.Args)
	}
	if got := secretKeys(g); strings.Join(got, ",") != "PERGENT_RESULT_TOKEN=result-token" {
		t.Errorf("graph container secrets = %v", got)
	}
	if len(g.VolumeMounts) != 1 || g.VolumeMounts[0].Name != "graph" || g.VolumeMounts[0].MountPath != "/graph" || g.VolumeMounts[0].ReadOnly {
		t.Errorf("graph container mounts = %+v", g.VolumeMounts)
	}
	sc := g.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("graph container security context = %+v", sc)
	}
	pergent := containerByName(t, spec.Containers, "pergent")
	if !slices.Contains(pergent.Args, "--graph=/graph/graph.json") {
		t.Errorf("pergent args = %v", pergent.Args)
	}
	var mounted bool
	for _, m := range pergent.VolumeMounts {
		if m.Name == "graph" {
			mounted = m.MountPath == "/graph" && m.ReadOnly
		}
	}
	if !mounted {
		t.Errorf("pergent mounts = %+v; want /graph read-only", pergent.VolumeMounts)
	}
	var vol bool
	for _, v := range spec.Volumes {
		if v.Name == "graph" {
			vol = v.EmptyDir != nil && v.EmptyDir.SizeLimit != nil
		}
	}
	if !vol {
		t.Errorf("volumes = %+v; want a limited emptyDir graph", spec.Volumes)
	}

	// Without the URL the Job has none of it, even with the image set.
	p.GraphURL = ""
	spec = BuildJob(p).Spec.Template.Spec
	if len(spec.InitContainers) != 1 || len(spec.Volumes) != 2 {
		t.Errorf("no graph: %d init containers, %d volumes", len(spec.InitContainers), len(spec.Volumes))
	}
	for _, a := range containerByName(t, spec.Containers, "pergent").Args {
		if strings.HasPrefix(a, "--graph") {
			t.Errorf("no graph, but pergent got %s", a)
		}
	}
}
