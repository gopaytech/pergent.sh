package kube

import (
	"context"
	_ "embed"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A fetch Job reads the skills of a public git URL an admin typed, so that
// the pergent.sh pod, which holds every secret, never fetches it (D18). Its
// "clone" init container clones one ref with no credential of any kind; its
// "send" container runs pergent-sh fetch-skills, which reads the skills of
// the clone (untrusted: no symlink is followed) and posts them back with the
// one-time fetch token, the only secret in the Job. The pod carries the
// managed-by label, so the review-Job NetworkPolicy applies to it.

//go:embed fetch.sh
var fetchScript string

const (
	labelFetch    = "pergent.sh/skill-fetch"
	keyFetchToken = "fetch-token"

	containerClone = "clone"
	containerSend  = "send"

	// fetchUser is the user both containers run as: the nonroot user of the
	// pergent.sh image.
	fetchUser = 65532
)

// FetchJobParams is everything one fetch Job is built from. None of it is
// secret. GitImage has git (jobs.image); Image is pergent.sh's own
// (PERGENT_SH_IMAGE). URL, Ref and Path were checked by skillfetch.Normalize.
type FetchJobParams struct {
	Name, Namespace, FetchID, GitImage, Image, URL, Ref, Path, ResultURL string
	Timeout                                                              time.Duration
}

// FetchJobName is "pergent-fetch-" plus the first 12 hex characters of the
// fetch ID.
func FetchJobName(fetchID string) string {
	id := strings.ReplaceAll(fetchID, "-", "")
	if len(id) > 12 {
		id = id[:12]
	}
	return "pergent-fetch-" + id
}

func fetchLabels(fetchID string) map[string]string {
	return map[string]string{labelManagedBy: managedBy, labelFetch: fetchID}
}

func fetchSecretName(p FetchJobParams) string { return p.Name + "-secrets" }

// fetchResources bound a shallow clone of one ref and the read of its skills.
func fetchResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
}

// fetchLockedDown is lockedDown with a read-only root filesystem: both
// containers write only to /work.
func fetchLockedDown() *corev1.SecurityContext {
	sc := lockedDown()
	sc.ReadOnlyRootFilesystem = ptr(true)
	return sc
}

// BuildFetchJob builds the (not yet suspended) fetch Job; LaunchFetch
// suspends it. The URL is only in the clone's environment, never in a
// command or argument.
func BuildFetchJob(p FetchJobParams) *batchv1.Job {
	clone := corev1.Container{
		Name:    containerClone,
		Image:   p.GitImage,
		Command: []string{"sh", "-ec", fetchScript},
		Env: []corev1.EnvVar{
			{Name: "FETCH_URL", Value: p.URL},
			{Name: "FETCH_REF", Value: p.Ref},
		},
		VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: "/work"}},
		Resources:       fetchResources(),
		SecurityContext: fetchLockedDown(),
	}
	send := corev1.Container{
		Name:    containerSend,
		Image:   p.Image,
		Command: []string{"/pergent-sh"},
		Args: []string{
			"fetch-skills",
			"--dir=/work/repo",
			"--path=" + p.Path,
			"--commit-file=/work/commit",
			"--error-file=/work/error",
			"--result-url=" + p.ResultURL,
		},
		Env:             []corev1.EnvVar{secretEnv("PERGENT_SH_FETCH_TOKEN", fetchSecretName(p), keyFetchToken)},
		VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: "/work", ReadOnly: true}},
		Resources:       fetchResources(),
		SecurityContext: fetchLockedDown(),
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: fetchLabels(p.FetchID)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(p.Timeout / time.Second)),
			TTLSecondsAfterFinished: ptr(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: fetchLabels(p.FetchID)},
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
					Volumes: []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse("1Gi"))},
					}}},
					InitContainers: []corev1.Container{clone},
					Containers:     []corev1.Container{send},
				},
			},
		},
	}
}

// BuildFetchSecret holds the fetch token, and nothing else, owned by the Job.
func BuildFetchSecret(p FetchJobParams, owner *batchv1.Job, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: fetchSecretName(p), Namespace: p.Namespace, Labels: fetchLabels(p.FetchID),
			OwnerReferences: ownerRef(owner),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{keyFetchToken: []byte(token)},
	}
}

// LaunchFetch creates the fetch Job suspended, then its Secret owned by it,
// then unsuspends it, like Launch. If anything fails after the Job exists,
// the Job (and with it the Secret) is deleted and the error returned.
func (l *Launcher) LaunchFetch(ctx context.Context, p FetchJobParams, token string) error {
	p.Namespace = l.Namespace
	return l.launchWithSecret(ctx, BuildFetchJob(p), func(owner *batchv1.Job) *corev1.Secret {
		return BuildFetchSecret(p, owner, token)
	})
}
