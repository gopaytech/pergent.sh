package kube

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/security"
)

// A security scan Job runs `pergent scan` over one commit. It has one
// container and no init container: pergent downloads the source from
// pergent.sh itself, with the one-time result token, so no Job holds a git
// token. The LLM key and the result token come from the Job's Secret, as for a
// review. The pod carries the managed-by label and LabelSecurityScan. The
// chart's security policy selects on both, and its review-Job policy leaves
// out every pod with LabelSecurityScan, so only the security policy applies.

const (
	// LabelSecurityScan names the scan a security Job is for. The chart's
	// NetworkPolicy for scans selects on it.
	LabelSecurityScan = "pergent.sh/security-scan"

	keySecurityPrior = "prior"

	// securityGrace is the pod's grace period: pergent needs up to about 35
	// seconds after SIGTERM to post a partial result.
	securityGrace = 60

	// securityPriorMount and securityPriorFile are where the prior scan's file is mounted.
	securityPriorMount = "/in"
	securityPriorFile  = securityPriorMount + "/prior.json"

	msgSecurityOOM      = "the scan ran out of memory"
	msgSecurityDeadline = "the scan took longer than its timeout"
	msgSecurityNoResult = "the scan Job ended without a result"
)

// SecurityJobParams is everything one security scan Job is built from. Prior
// is the JSON of the prior scan's file, and goes in the Job's Secret.
type SecurityJobParams struct {
	Name, Namespace, ScanID, Image string
	Commit, SourceURL, ResultURL   string
	LLM                            config.LLM
	Timeout, Deadline              time.Duration
	MaxSteps, SubagentMaxSteps     *int
	TokenBudget                    *int64
	Proxy                          string
	Prior                          []byte
	Resources                      corev1.ResourceRequirements
	WorkSize                       resource.Quantity
}

var fullSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// SecurityJobName is "pergent-security-" plus the first 12 hex characters of
// the scan ID.
func SecurityJobName(scanID string) string {
	id := strings.ReplaceAll(scanID, "-", "")
	if len(id) > 12 {
		id = id[:12]
	}
	return "pergent-security-" + id
}

func securityLabels(scanID string) map[string]string {
	return map[string]string{labelManagedBy: managedBy, LabelSecurityScan: scanID}
}

func securitySecretName(p SecurityJobParams) string { return p.Name + "-secrets" }

// securityLockedDown is lockedDown, run as the non-root user (the same uid as
// the fetch Jobs): the image's skill folder belongs to root, and the agent must
// not be able to change it.
func securityLockedDown() *corev1.SecurityContext {
	sc := lockedDown()
	sc.RunAsNonRoot = ptr(true)
	sc.RunAsUser = ptr(int64(fetchUser))
	sc.RunAsGroup = ptr(int64(fetchUser))
	return sc
}

// callbackHost is the host of the result URL. The source and the result both
// go to pergent.sh, so it is never sent through the proxy.
func callbackHost(resultURL string) string {
	if u, err := url.Parse(resultURL); err == nil {
		return u.Hostname()
	}
	return ""
}

// BuildSecurityJob builds the (not yet suspended) scan Job; LaunchSecurity
// suspends it.
func BuildSecurityJob(p SecurityJobParams) *batchv1.Job {
	secret := securitySecretName(p)
	args := []string{
		"scan",
		"--repo-path=/work/repo",
		"--head=" + p.Commit,
		"--source-url=" + p.SourceURL,
		"--result-url=" + p.ResultURL,
		"--timeout=" + p.Timeout.String(),
	}
	if len(p.Prior) > 0 {
		args = append(args, "--prior="+securityPriorFile)
	}
	if p.MaxSteps != nil {
		args = append(args, "--max-turns="+strconv.Itoa(*p.MaxSteps))
	}
	if p.SubagentMaxSteps != nil {
		args = append(args, "--max-subagent-turns="+strconv.Itoa(*p.SubagentMaxSteps))
	}

	env := []corev1.EnvVar{
		{Name: "OPENCODE_PROVIDER", Value: p.LLM.Provider},
		{Name: "OPENCODE_MODEL", Value: p.LLM.Model},
	}
	if p.LLM.BaseURL != "" {
		env = append(env, corev1.EnvVar{Name: "OPENCODE_BASE_URL", Value: p.LLM.BaseURL})
	}
	env = append(env,
		secretEnv("OPENCODE_API_KEY", secret, keyLLMKey),
		secretEnv("PERGENT_RESULT_TOKEN", secret, keyResultToken),
		corev1.EnvVar{Name: "PERGENT_SCAN_SOURCE_URL", Value: p.SourceURL},
		// The user has no home folder in the image: give it a place to write.
		corev1.EnvVar{Name: "HOME", Value: "/work"},
		// pergent's work folders go to the temporary folder: put it on a
		// volume that has a size limit.
		corev1.EnvVar{Name: "TMPDIR", Value: "/work"},
	)
	if p.TokenBudget != nil {
		env = append(env, corev1.EnvVar{Name: "PERGENT_TOKEN_BUDGET", Value: strconv.FormatInt(*p.TokenBudget, 10)})
	}
	if p.Proxy != "" {
		host := callbackHost(p.ResultURL)
		env = append(env,
			corev1.EnvVar{Name: "HTTP_PROXY", Value: p.Proxy},
			corev1.EnvVar{Name: "HTTPS_PROXY", Value: p.Proxy},
			corev1.EnvVar{Name: "http_proxy", Value: p.Proxy},
			corev1.EnvVar{Name: "https_proxy", Value: p.Proxy},
			corev1.EnvVar{Name: "NO_PROXY", Value: host},
			corev1.EnvVar{Name: "no_proxy", Value: host},
			corev1.EnvVar{Name: "PERGENT_SCAN_KEEP_PROXY", Value: "true"},
		)
	}

	mounts := []corev1.VolumeMount{
		{Name: "work", MountPath: "/work"},
		// pergent refuses a folder that is not empty: this one is its own.
		{Name: "repo", MountPath: "/work/repo"},
	}
	volumes := []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: workVolume(p.WorkSize)}},
		{Name: "repo", VolumeSource: corev1.VolumeSource{EmptyDir: workVolume(p.WorkSize)}},
	}
	if len(p.Prior) > 0 {
		mounts = append(mounts, corev1.VolumeMount{Name: "prior", MountPath: securityPriorMount, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "prior", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  secret,
			Items:       []corev1.KeyToPath{{Key: keySecurityPrior, Path: "prior.json"}},
			DefaultMode: ptr(int32(0o444)),
		}}})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: securityLabels(p.ScanID)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(p.Deadline / time.Second)),
			TTLSecondsAfterFinished: ptr(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: securityLabels(p.ScanID)},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: ptr(int64(securityGrace)),
					AutomountServiceAccountToken:  ptr(false),
					EnableServiceLinks:            ptr(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true),
						RunAsUser:    ptr(int64(fetchUser)),
						RunAsGroup:   ptr(int64(fetchUser)),
					},
					Volumes: volumes,
					Containers: []corev1.Container{{
						Name:            containerPergent,
						Image:           p.Image,
						Command:         []string{"pergent"},
						Args:            args,
						Env:             env,
						VolumeMounts:    mounts,
						Resources:       p.Resources,
						SecurityContext: securityLockedDown(),
					}},
				},
			},
		},
	}
}

// BuildSecuritySecret holds the LLM key, the result token and the prior scan's
// file, owned by the Job. It never holds a git token.
func BuildSecuritySecret(p SecurityJobParams, owner *batchv1.Job, llmKey, resultToken string) *corev1.Secret {
	data := map[string][]byte{
		keyLLMKey:      []byte(llmKey),
		keyResultToken: []byte(resultToken),
	}
	if len(p.Prior) > 0 {
		data[keySecurityPrior] = p.Prior
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: securitySecretName(p), Namespace: p.Namespace, Labels: securityLabels(p.ScanID),
			OwnerReferences: ownerRef(owner),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// LaunchSecurity refuses a commit that is not a full SHA and a prior file over
// security.MaxPriorBytes, then creates the Job suspended, its Secret owned by
// it, and unsuspends it (launchWithSecret).
func (l *Launcher) LaunchSecurity(ctx context.Context, p SecurityJobParams, llmKey, resultToken string) error {
	if !fullSHA.MatchString(p.Commit) {
		return errors.New("the security scan needs a full commit SHA")
	}
	if len(p.Prior) > security.MaxPriorBytes {
		return errors.New("the prior scan's file is too large to send to the scan")
	}
	p.Namespace = l.Namespace
	return l.launchWithSecret(ctx, BuildSecurityJob(p), func(owner *batchv1.Job) *corev1.Secret {
		return BuildSecuritySecret(p, owner, llmKey, resultToken)
	})
}

// SecurityFailure says why the scan Job ended without posting a result, as
// text for the dashboard: "" while it runs and after it completed. It reads
// the Job as ScanFailure does, with the scan's own texts.
func (l *Launcher) SecurityFailure(ctx context.Context, name string) (string, error) {
	obs, facts, err := l.inspect(ctx, name, time.Now())
	switch {
	case err != nil:
		return "", err
	case !obs.Exists:
		return msgSecurityNoResult, nil
	case !obs.Finished || obs.Failure.Reason == "":
		return "", nil
	case facts.storage:
		msg := "the repository is too large for the scan's disk"
		vol := l.evictedVolume(ctx, name)
		if job, err := l.Client.BatchV1().Jobs(l.Namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
			for _, v := range job.Spec.Template.Spec.Volumes {
				if v.Name == vol && v.EmptyDir != nil && v.EmptyDir.SizeLimit != nil {
					msg += fmt.Sprintf(" (%s)", v.EmptyDir.SizeLimit.String())
				}
			}
		}
		return msg, nil
	case facts.oom:
		return msgSecurityOOM, nil
	case facts.failReason == "DeadlineExceeded":
		return msgSecurityDeadline, nil
	}
	return msgSecurityNoResult, nil
}

var emptyDirVolume = regexp.MustCompile(`EmptyDir volume "([^"]+)"`)

// evictedVolume is the volume a pod's eviction message names ("repo" or
// "work"), or "work" when no message does.
func (l *Launcher) evictedVolume(ctx context.Context, name string) string {
	pods, err := l.Client.CoreV1().Pods(l.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err == nil {
		for _, p := range pods.Items {
			if m := emptyDirVolume.FindStringSubmatch(p.Status.Message); m != nil {
				return m[1]
			}
		}
	}
	return "work"
}
