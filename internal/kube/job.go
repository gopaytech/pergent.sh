// Package kube builds, launches and inspects the Kubernetes Jobs that run
// pergent against a merge request.
//
// A Job is the trust boundary: its checkout is untrusted code and an LLM agent
// reads it. The init container gets only a read-only git token; the pergent
// container gets only the LLM key and a one-time result token. No token that
// can comment, approve or write is ever put into a Job or its Secret.
package kube

import (
	_ "embed"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
)

//go:embed checkout.sh
var checkoutScript string

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelRun       = "pergent.sh/run"
	managedBy      = "pergent-sh"

	keyGitToken    = "git-token"
	keyLLMKey      = "llm-api-key"
	keyResultToken = "result-token"

	containerCheckout = "checkout"
	containerPergent  = "pergent"

	minCloneAllowance = 2 * time.Minute
	deliverAllowance  = 2 * time.Minute
)

// Byte limits for the comment pergent renders (--max-comment-size). GitLab
// takes a note of up to 1,000,000 characters; GitHub refuses a comment over
// 65,536, and pergent.sh may append a note of up to 4000 bytes after
// pergent's part (policy's skipped-files note), so GitHub's is lower.
const (
	CommentLimitGitLab = 990000
	CommentLimitGitHub = 60000
)

// JobParams is everything one review Job is built from. None of it is secret.
type JobParams struct {
	Name, Namespace, RunID, Image string
	Timeout, CloneAllowance       time.Duration
	Concurrency                   int
	RepoURL                       string // never carries credentials
	ChangeRef                     string // the platform ref the change head can be fetched from
	TargetBranch, BaseSHA         string
	HeadSHA, GitUsername          string
	ResultURL                     string
	// MaxCommentSize is the platform's comment limit (CommentLimitGitLab or
	// CommentLimitGitHub); 0 is the smaller, GitHub's.
	MaxCommentSize int
	LLM            config.LLM
	Resources      corev1.ResourceRequirements
	Inputs         *inputs.Inputs
}

// Secrets are the per-Job credentials, stored in one Secret owned by the Job.
type Secrets struct{ GitToken, LLMKey, ResultToken string }

// JobName is "pergent-review-" plus the first 12 hex characters of the run ID.
func JobName(runID string) string {
	id := strings.ReplaceAll(runID, "-", "")
	if len(id) > 12 {
		id = id[:12]
	}
	return "pergent-review-" + id
}

// Deadline is the Job's activeDeadlineSeconds: pergent's own budget, 2 minutes
// to finish and deliver, and the allowance for the clone.
func Deadline(timeout, cloneAllowance time.Duration) time.Duration {
	return timeout + deliverAllowance + cloneAllowance
}

// CloneAllowance is max(2 minutes, 2 x the last measured checkout time).
func CloneAllowance(lastCloneSeconds int) time.Duration {
	d := 2 * time.Duration(lastCloneSeconds) * time.Second
	if d < minCloneAllowance {
		return minCloneAllowance
	}
	return d
}

func labels(runID string) map[string]string {
	return map[string]string{labelManagedBy: managedBy, labelRun: runID}
}

func secretsName(p JobParams) string { return p.Name + "-secrets" }
func inputsName(p JobParams) string  { return p.Name + "-inputs" }

func ptr[T any](v T) *T { return &v }

func skillNames(in *inputs.Inputs) []string {
	if in == nil {
		return nil
	}
	names := make([]string, 0, len(in.Skills))
	for n := range in.Skills {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key},
	}}
}

func lockedDown() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// BuildJob builds the (not yet suspended) review Job; Launch suspends it.
func BuildJob(p JobParams) *batchv1.Job {
	secret := secretsName(p)
	hasInstructions := p.Inputs != nil && len(p.Inputs.Instructions) > 0
	hasPrevious := p.Inputs != nil && len(p.Inputs.Previous) > 0

	args := []string{
		"agent",
		"--repo-path=/work/repo",
		"--base=" + p.BaseSHA,
		"--head=" + p.HeadSHA,
		"--skills-dir=/in/skills",
	}
	if hasInstructions {
		args = append(args, "--instructions=/in/instructions.md")
	}
	if hasPrevious {
		args = append(args, "--previous=/in/previous.json")
	}
	args = append(args,
		"--scrub-instructions",
		"--timeout="+p.Timeout.String(),
		"--concurrency="+strconv.Itoa(p.Concurrency),
		"--max-comment-size="+strconv.Itoa(commentLimit(p.MaxCommentSize)),
		"--result-url="+p.ResultURL,
	)

	mainEnv := []corev1.EnvVar{
		{Name: "OPENCODE_PROVIDER", Value: p.LLM.Provider},
		{Name: "OPENCODE_MODEL", Value: p.LLM.Model},
	}
	if p.LLM.BaseURL != "" {
		mainEnv = append(mainEnv, corev1.EnvVar{Name: "OPENCODE_BASE_URL", Value: p.LLM.BaseURL})
	}
	mainEnv = append(mainEnv,
		secretEnv("OPENCODE_API_KEY", secret, keyLLMKey),
		secretEnv("PERGENT_RESULT_TOKEN", secret, keyResultToken),
	)

	checkout := corev1.Container{
		Name:    containerCheckout,
		Image:   p.Image,
		Command: []string{"sh", "-ec", checkoutScript},
		Env: []corev1.EnvVar{
			{Name: "REPO_URL", Value: p.RepoURL},
			{Name: "MR_REF", Value: p.ChangeRef},
			{Name: "TARGET_BRANCH", Value: p.TargetBranch},
			{Name: "BASE_SHA", Value: p.BaseSHA},
			{Name: "HEAD_SHA", Value: p.HeadSHA},
			{Name: "GIT_USERNAME", Value: p.GitUsername},
			secretEnv("GIT_TOKEN", secret, keyGitToken),
		},
		VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: "/work"}},
		Resources:       p.Resources,
		SecurityContext: lockedDown(),
	}
	pergent := corev1.Container{
		Name:    containerPergent,
		Image:   p.Image,
		Command: []string{"pergent"},
		Args:    args,
		Env:     mainEnv,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "work", MountPath: "/work"},
			{Name: "inputs", MountPath: "/in", ReadOnly: true},
		},
		Resources:       p.Resources,
		SecurityContext: lockedDown(),
	}

	// ConfigMap keys can't contain "/", so items put each skill under skills/.
	var items []corev1.KeyToPath
	for i, n := range skillNames(p.Inputs) {
		items = append(items, corev1.KeyToPath{Key: skillKey(i), Path: "skills/" + n + ".md"})
	}
	if hasInstructions {
		items = append(items, corev1.KeyToPath{Key: "instructions.md", Path: "instructions.md"})
	}
	if hasPrevious {
		items = append(items, corev1.KeyToPath{Key: "previous.json", Path: "previous.json"})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: labels(p.RunID)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(Deadline(p.Timeout, p.CloneAllowance) / time.Second)),
			TTLSecondsAfterFinished: ptr(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(p.RunID)},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: ptr(int64(30)),
					AutomountServiceAccountToken:  ptr(false),
					EnableServiceLinks:            ptr(false),
					Volumes: []corev1.Volume{
						{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "inputs", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: inputsName(p)},
							Items:                items,
						}}},
					},
					InitContainers: []corev1.Container{checkout},
					Containers:     []corev1.Container{pergent},
				},
			},
		},
	}
}

// commentLimit is n, or the smaller limit when n is unset.
func commentLimit(n int) int {
	if n <= 0 {
		return CommentLimitGitHub
	}
	return n
}

func skillKey(i int) string { return "skill-" + strconv.Itoa(i) + ".md" }

func ownerRef(job *batchv1.Job) []metav1.OwnerReference {
	if job == nil {
		return nil
	}
	return []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr(false),
	}}
}

// BuildConfigMap holds the skills, instructions and previous review. owner may
// be nil (tests); Launch always passes the created Job.
func BuildConfigMap(p JobParams, owner *batchv1.Job) *corev1.ConfigMap {
	data := map[string]string{}
	if p.Inputs != nil {
		for i, n := range skillNames(p.Inputs) {
			data[skillKey(i)] = string(p.Inputs.Skills[n])
		}
		if len(p.Inputs.Instructions) > 0 {
			data["instructions.md"] = string(p.Inputs.Instructions)
		}
		if len(p.Inputs.Previous) > 0 {
			data["previous.json"] = string(p.Inputs.Previous)
		}
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: inputsName(p), Namespace: p.Namespace, Labels: labels(p.RunID),
			OwnerReferences: ownerRef(owner),
		},
		Data: data,
	}
}

// BuildSecret holds the per-Job credentials, owned by the Job.
func BuildSecret(p JobParams, owner *batchv1.Job, s Secrets) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretsName(p), Namespace: p.Namespace, Labels: labels(p.RunID),
			OwnerReferences: ownerRef(owner),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			keyGitToken:    []byte(s.GitToken),
			keyLLMKey:      []byte(s.LLMKey),
			keyResultToken: []byte(s.ResultToken),
		},
	}
}
