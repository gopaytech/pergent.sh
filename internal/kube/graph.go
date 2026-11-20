package kube

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A graph build Job builds the code graph of one repository's default branch
// (docs/concepts/code-graph.md). It is a scan Job's shape with one more step:
// a "clone" init container that holds the one read-only git token and clones
// the commit (scan.sh), a "graphify" init container that holds no secret at
// all and runs graphify and pergent's converter over the clone, and a "post"
// container, pergent.sh's own image, that posts the graph with the one-time
// build token. So the graph tool never shares a container with a token, and
// the clone never sees the build token. No LLM key is in the Job. The pod
// carries the managed-by label, so the review-Job NetworkPolicy applies to it.

const (
	labelGraph = "pergent.sh/graph-build"

	keyGraphToken = "graph-token"

	containerGraphify  = "graphify"
	containerGraphPost = "post"

	// GraphJobTimeout is the Job's activeDeadlineSeconds: the clone, the
	// build and the post together.
	GraphJobTimeout = 15 * time.Minute

	// graphWorkSize holds the clone, graphOutSize graphify's output and
	// cache and the converted graph.
	graphWorkSize = "4Gi"
	graphOutSize  = "2Gi"
)

// graphifyScript runs in the graphify container. It reads the clone
// (mounted read-only) and writes only /out: graphify's output, the graph,
// or one of the fixed messages in /out/error. It always exits 0, so the post
// container reports what happened. A clone that failed is left to the post
// container, which reports the clone's own message.
const graphifyScript = `set -u
if [ -e /work/error ] || [ ! -s /work/commit ]; then exit 0; fi
mkdir -p /out/home
export HOME=/out/home PYTHONDONTWRITEBYTECODE=1
commit="$(cat /work/commit)"
if ! graphify extract /work/repo --code-only --out /out/graphify >&2; then
  printf '%s\n' "graphify couldn't read the repository" > /out/error
  exit 0
fi
if ! pergent graph convert --provider graphify --in /out/graphify --out /out/graph.json --commit "$commit" >&2; then
  printf '%s\n' "pergent couldn't convert graphify's output" > /out/error
fi
exit 0
`

// GraphJobParams is everything one graph build Job is built from. None of it
// is secret: RepoURL never carries credentials, and the tokens are
// LaunchGraph's own arguments.
type GraphJobParams struct {
	Name, Namespace, BuildID             string
	GitImage, GraphImage, Image          string
	RepoURL, GitUsername, Branch, Commit string
	ResultURL                            string
}

// GraphJobName is "pergent-graph-" plus the first 12 hex characters of the
// build ID.
func GraphJobName(buildID string) string {
	id := strings.ReplaceAll(buildID, "-", "")
	if len(id) > 12 {
		id = id[:12]
	}
	return "pergent-graph-" + id
}

func graphLabels(buildID string) map[string]string {
	return map[string]string{labelManagedBy: managedBy, labelGraph: buildID}
}

func graphSecretName(p GraphJobParams) string { return p.Name + "-secrets" }

// graphResources bound each container: 250m CPU and 256Mi requested, 2Gi of
// memory at most (graphify holds the whole graph in memory).
func graphResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}
}

// BuildGraphJob builds the (not yet suspended) graph build Job; LaunchGraph
// suspends it.
func BuildGraphJob(p GraphJobParams) *batchv1.Job {
	secret := graphSecretName(p)
	clone := corev1.Container{
		Name:    containerClone,
		Image:   p.GitImage,
		Command: []string{"sh", "-ec", scanScript},
		Env: []corev1.EnvVar{
			{Name: "REPO_URL", Value: p.RepoURL},
			{Name: "SCAN_BRANCH", Value: p.Branch},
			{Name: "SCAN_COMMIT", Value: p.Commit},
			{Name: "GIT_USERNAME", Value: p.GitUsername},
			secretEnv("GIT_TOKEN", secret, keyGitToken),
		},
		VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: "/work"}},
		Resources:       graphResources(),
		SecurityContext: fetchLockedDown(),
	}
	graphify := corev1.Container{
		Name:    containerGraphify,
		Image:   p.GraphImage,
		Command: []string{"sh", "-c", graphifyScript},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "work", MountPath: "/work", ReadOnly: true},
			{Name: "out", MountPath: "/out"},
		},
		Resources:       graphResources(),
		SecurityContext: fetchLockedDown(),
	}
	post := corev1.Container{
		Name:    containerGraphPost,
		Image:   p.Image,
		Command: []string{"/pergent-sh"},
		Args: []string{
			"post-graph",
			"--work=/work",
			"--out=/out",
			"--result-url=" + p.ResultURL,
		},
		Env: []corev1.EnvVar{secretEnv("PERGENT_SH_GRAPH_TOKEN", secret, keyGraphToken)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "work", MountPath: "/work", ReadOnly: true},
			{Name: "out", MountPath: "/out", ReadOnly: true},
		},
		Resources:       graphResources(),
		SecurityContext: fetchLockedDown(),
	}
	emptyDir := func(size string) corev1.VolumeSource {
		return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse(size))}}
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: graphLabels(p.BuildID)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(GraphJobTimeout / time.Second)),
			TTLSecondsAfterFinished: ptr(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: graphLabels(p.BuildID)},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: ptr(int64(10)),
					AutomountServiceAccountToken:  ptr(false),
					EnableServiceLinks:            ptr(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true),
						RunAsUser:    ptr(int64(fetchUser)),
						RunAsGroup:   ptr(int64(fetchUser)),
					},
					Volumes: []corev1.Volume{
						{Name: "work", VolumeSource: emptyDir(graphWorkSize)},
						{Name: "out", VolumeSource: emptyDir(graphOutSize)},
					},
					InitContainers: []corev1.Container{clone, graphify},
					Containers:     []corev1.Container{post},
				},
			},
		},
	}
}

// BuildGraphSecret holds the git token and the build token, owned by the Job.
func BuildGraphSecret(p GraphJobParams, owner *batchv1.Job, gitToken, buildToken string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: graphSecretName(p), Namespace: p.Namespace, Labels: graphLabels(p.BuildID),
			OwnerReferences: ownerRef(owner),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			keyGitToken:   []byte(gitToken),
			keyGraphToken: []byte(buildToken),
		},
	}
}

// LaunchGraph refuses a repository URL that carries credentials, then creates
// the graph build Job suspended, its Secret owned by it, and unsuspends it
// (launchWithSecret).
func (l *Launcher) LaunchGraph(ctx context.Context, p GraphJobParams, gitToken, buildToken string) error {
	if u, err := url.Parse(p.RepoURL); err != nil || u.User != nil {
		return errors.New("the repository URL for the graph build must be a URL with no credentials in it")
	}
	p.Namespace = l.Namespace
	return l.launchWithSecret(ctx, BuildGraphJob(p), func(owner *batchv1.Job) *corev1.Secret {
		return BuildGraphSecret(p, owner, gitToken, buildToken)
	})
}

// The review Job's side: the graph init container downloads the run's graph
// into its own volume, outside /work, and pergent reads it with --graph.
const (
	containerGraph = "graph"
	graphMount     = "/graph"
	// GraphFile is where the review Job's pergent container finds the graph.
	GraphFile = graphMount + "/graph.json"
	// reviewGraphSize bounds the graph volume: pergent reads at most 64 MiB.
	reviewGraphSize = "128Mi"
)

// graphContainer is the review Job's graph init container: pergent.sh's own
// image, the result token alone, the graph volume alone. It always exits 0.
func graphContainer(p JobParams) corev1.Container {
	sc := fetchLockedDown()
	sc.RunAsNonRoot = ptr(true)
	sc.RunAsUser = ptr(int64(fetchUser))
	sc.RunAsGroup = ptr(int64(fetchUser))
	return corev1.Container{
		Name:    containerGraph,
		Image:   p.GraphImage,
		Command: []string{"/pergent-sh"},
		Args:    []string{"get-graph", "--url=" + p.GraphURL, "--out=" + GraphFile},
		Env:     []corev1.EnvVar{secretEnv("PERGENT_RESULT_TOKEN", secretsName(p), keyResultToken)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "graph", MountPath: graphMount},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		SecurityContext: sc,
	}
}

// graphVolume is the review Job's graph volume.
func graphVolume() corev1.Volume {
	return corev1.Volume{Name: "graph", VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse(reviewGraphSize))},
	}}
}
