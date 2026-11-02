package kube

import (
	"context"
	_ "embed"
	"errors"
	"net/url"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/readiness"
)

// A scan Job reads one repository's default branch for the readiness scan.
// It is a fetch Job's shape: a "clone" init container that holds the one
// read-only git token and clones the commit, and a "scan" container that runs
// pergent-sh scan-repo over the clone (untrusted: no symlink is followed) and
// posts what it found with the one-time scan token. The scan container never
// sees the git token. The clone never sees the scan token or the input. The
// input (the library skills' globs, which can be larger than one environment
// string may be) is a file of the Job's Secret, mounted in the scan container
// only. No LLM key and no token that can write is in the Job. The pod carries
// the managed-by label, so the review-Job NetworkPolicy applies to it.

//go:embed scan.sh
var scanScript string

const (
	labelScan = "pergent.sh/readiness-scan"

	keyScanToken = "scan-token"
	keyScanInput = "scan-input"

	containerScan = "scan"

	// scanDeadline is the Job's activeDeadlineSeconds: 5 minutes for the
	// clone and the scan together.
	scanDeadline = 300

	// scanInputMount is where the scan container finds its input file.
	scanInputMount = "/in"
	scanInputFile  = scanInputMount + "/" + keyScanInput

	msgScanTooLargeRepo = "the repository is too large to clone (the scan has 2 GiB of disk)"
	msgScanOOM          = "the scan ran out of memory"
	msgScanDeadline     = "the scan took longer than 5 minutes"
	msgScanNoResult     = "the scan Job ended without a result"
)

// ErrScanInputTooLarge is LaunchScan's refusal of an input over
// readiness.MaxInputBytes.
var ErrScanInputTooLarge = errors.New("the skill library's paths are too large to send to the scan")

// ScanJobParams is everything one scan Job is built from. Input is the JSON of
// a readiness.JobInput, and goes in the Job's Secret, not its spec. None of the
// rest is secret: RepoURL never carries credentials, the git token is
// LaunchScan's own argument.
type ScanJobParams struct {
	Name, Namespace, ScanID, GitImage, Image string
	RepoURL, GitUsername, Branch, Commit     string
	ResultURL, Input                         string
}

// ScanJobName is "pergent-scan-" plus the first 12 hex characters of the scan
// ID.
func ScanJobName(scanID string) string {
	id := strings.ReplaceAll(scanID, "-", "")
	if len(id) > 12 {
		id = id[:12]
	}
	return "pergent-scan-" + id
}

func scanLabels(scanID string) map[string]string {
	return map[string]string{labelManagedBy: managedBy, labelScan: scanID}
}

func scanSecretName(p ScanJobParams) string { return p.Name + "-secrets" }

// BuildScanJob builds the (not yet suspended) scan Job; LaunchScan suspends
// it.
func BuildScanJob(p ScanJobParams) *batchv1.Job {
	secret := scanSecretName(p)
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
		Resources:       fetchResources(),
		SecurityContext: fetchLockedDown(),
	}
	scan := corev1.Container{
		Name:    containerScan,
		Image:   p.Image,
		Command: []string{"/pergent-sh"},
		Args: []string{
			"scan-repo",
			"--dir=/work/repo",
			"--commit-file=/work/commit",
			"--error-file=/work/error",
			"--input-file=" + scanInputFile,
			"--result-url=" + p.ResultURL,
		},
		Env: []corev1.EnvVar{secretEnv("PERGENT_SH_SCAN_TOKEN", secret, keyScanToken)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "work", MountPath: "/work", ReadOnly: true},
			{Name: "scan-input", MountPath: scanInputMount, ReadOnly: true},
		},
		Resources:       fetchResources(),
		SecurityContext: fetchLockedDown(),
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: scanLabels(p.ScanID)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(scanDeadline)),
			TTLSecondsAfterFinished: ptr(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: scanLabels(p.ScanID)},
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
						{Name: "work", VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse("2Gi"))},
						}},
						// Only the input key, readable by the non-root user.
						{Name: "scan-input", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName:  secret,
							Items:       []corev1.KeyToPath{{Key: keyScanInput, Path: keyScanInput}},
							DefaultMode: ptr(int32(0o444)),
						}}},
					},
					InitContainers: []corev1.Container{clone},
					Containers:     []corev1.Container{scan},
				},
			},
		},
	}
}

// BuildScanSecret holds the git token, the scan token and the scan input,
// owned by the Job. LaunchScan has already refused an input over
// readiness.MaxInputBytes.
func BuildScanSecret(p ScanJobParams, owner *batchv1.Job, gitToken, scanToken string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: scanSecretName(p), Namespace: p.Namespace, Labels: scanLabels(p.ScanID),
			OwnerReferences: ownerRef(owner),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			keyGitToken:  []byte(gitToken),
			keyScanToken: []byte(scanToken),
			keyScanInput: []byte(p.Input),
		},
	}
}

// LaunchScan refuses an input over readiness.MaxInputBytes
// (ErrScanInputTooLarge) and a repository URL that carries credentials, then
// creates the scan Job suspended, its Secret owned by it, and unsuspends it
// (launchWithSecret). If anything fails after the Job exists, the Job (and
// with it the Secret) is deleted and the error returned.
func (l *Launcher) LaunchScan(ctx context.Context, p ScanJobParams, gitToken, scanToken string) error {
	if len(p.Input) > readiness.MaxInputBytes {
		return ErrScanInputTooLarge
	}
	// The token reaches git through the auth header only. The URL is not
	// echoed: it is what holds the credential.
	if u, err := url.Parse(p.RepoURL); err != nil || u.User != nil {
		return errors.New("the repository URL for the scan must be a URL with no credentials in it")
	}
	p.Namespace = l.Namespace
	return l.launchWithSecret(ctx, BuildScanJob(p), func(owner *batchv1.Job) *corev1.Secret {
		return BuildScanSecret(p, owner, gitToken, scanToken)
	})
}

// ScanFailure says why the scan Job ended without posting a result, as text
// for the dashboard: "" while it runs and after it completed (a Complete Job
// that posted nothing is for the caller to judge). A pod evicted for its
// 2 GiB disk or a message about ephemeral storage is a repository too large to
// clone; then an OOMKilled container, then the Job's deadline; any other
// failure, and a Job that is gone, is the Job ending without a result. It reads
// the Job as Inspect does.
func (l *Launcher) ScanFailure(ctx context.Context, name string) (string, error) {
	obs, facts, err := l.inspect(ctx, name, time.Now())
	switch {
	case err != nil:
		return "", err
	case !obs.Exists:
		return msgScanNoResult, nil
	case !obs.Finished || obs.Failure == "":
		return "", nil
	case facts.storage:
		return msgScanTooLargeRepo, nil
	case facts.oom:
		return msgScanOOM, nil
	case facts.failReason == "DeadlineExceeded":
		return msgScanDeadline, nil
	}
	return msgScanNoResult, nil
}
