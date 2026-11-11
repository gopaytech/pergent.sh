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
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
)

//go:embed checkout.sh
var checkoutScript string

// apiCheckoutScript is the checkout of an API review: no clone, the commits are
// built from the files the caller sent.
//
//go:embed apicheckout.sh
var apiCheckoutScript string

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelRun       = "pergent.sh/run"
	managedBy      = "pergent-sh"

	// LabelInstance to LabelHead name the change a review Job is for (labels);
	// AnnotationProject and AnnotationMRURL name it in full (annotations).
	LabelInstance     = "pergent.sh/instance"
	LabelProjectID    = "pergent.sh/project-id"
	LabelMR           = "pergent.sh/mr"
	LabelHead         = "pergent.sh/head"
	AnnotationProject = "pergent.sh/project"
	AnnotationMRURL   = "pergent.sh/mr-url"

	// LabelAPIClient names the API client an API review is for.
	LabelAPIClient = "pergent.sh/api-client"

	keyGitToken    = "git-token"
	keyLLMKey      = "llm-api-key"
	keyResultToken = "result-token"
	keyImport      = "import"

	containerCheckout = "checkout"
	containerPergent  = "pergent"

	// FilesSecretMaxBytes bounds the stream of an API review. A Secret holds
	// 1 MiB, and the Secret's own metadata takes some of it.
	FilesSecretMaxBytes = 1_000_000

	minCloneAllowance = 2 * time.Minute
	deliverAllowance  = 2 * time.Minute

	// LinkedCloneTimeout and LinkedCloneMaxKiB bound the clone of one linked
	// repository; checkout.sh holds the same two numbers (a test ties them).
	LinkedCloneTimeout = 120 * time.Second
	LinkedCloneMaxKiB  = 2 * 1024 * 1024
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
	Concurrency, MaxTurns         int
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
	// WorkSize limits the work volume, which holds the checkout: a change's
	// author decides how much it takes. Zero leaves it unlimited.
	WorkSize resource.Quantity
	Inputs   *inputs.Inputs
	// LinkedClones are the linked repositories the checkout container clones
	// after the checkout. Nil is none, and the Job is then what it was before
	// linked repositories existed.
	LinkedClones []LinkedClone
	// The change this Job reviews, so a Job found in the cluster can be traced
	// to it. Labels carry Instance, ProjectID, IID and HeadSHA; annotations carry
	// ProjectPath and MRURL, which a label value cannot hold. A zero value is left out.
	Instance, ProjectPath, MRURL string
	ProjectID, IID               int64
	// Import is the `git fast-import` stream of an API review, which has no
	// clone: its checkout container builds the base and head commits from it.
	// Non-nil makes the Job an API review's. APIClient names the client.
	Import    []byte
	APIClient string
}

// LinkedClone is one linked repository to clone: Dir is its directory name
// under /work ("linked-1": never derived from the repository's name) and
// CloneURL a plain https URL without credentials.
type LinkedClone struct{ Dir, CloneURL string }

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

// CloneAllowance is max(2 minutes, 2 x the last measured checkout time), plus
// the time limit of each of targets linked repositories to clone.
func CloneAllowance(lastCloneSeconds, targets int) time.Duration {
	d := 2 * time.Duration(lastCloneSeconds) * time.Second
	if d < minCloneAllowance {
		d = minCloneAllowance
	}
	return d + time.Duration(targets)*LinkedCloneTimeout
}

// workSizeFor is the work volume's limit with n linked repositories: each may
// take LinkedCloneMaxKiB beside the checkout. An unlimited volume stays
// unlimited, and no clone leaves the limit as it was.
func workSizeFor(size resource.Quantity, n int) resource.Quantity {
	if size.IsZero() || n == 0 {
		return size
	}
	out := size.DeepCopy()
	out.Add(*resource.NewQuantity(int64(n)*LinkedCloneMaxKiB*1024, resource.BinarySI))
	return out
}

// checkLinkedClones refuses a clone that is not under linked-1..3, or whose
// URL is not a plain https one: a credential in a URL would be written to the
// clone's .git/config, which the model can read.
func checkLinkedClones(cs []LinkedClone) error {
	for _, c := range cs {
		if len(c.Dir) != len("linked-1") || !strings.HasPrefix(c.Dir, "linked-") || c.Dir[7] < '1' || c.Dir[7] > '3' {
			return fmt.Errorf("linked clone directory %q is not linked-1 to linked-3", c.Dir)
		}
		u, err := url.Parse(c.CloneURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(c.CloneURL, " \t\r\n") {
			return fmt.Errorf("linked clone %s: the URL is not a plain https URL", c.Dir)
		}
	}
	return nil
}

// labels are the run and, when set, the change it reviews. A value Kubernetes
// would refuse as a label is left out, so one odd name never stops a Job.
func labels(p JobParams) map[string]string {
	l := map[string]string{labelManagedBy: managedBy, labelRun: p.RunID}
	set := func(key, value string) {
		if value != "" && len(validation.IsValidLabelValue(value)) == 0 {
			l[key] = value
		}
	}
	set(LabelInstance, p.Instance)
	if p.ProjectID != 0 {
		set(LabelProjectID, strconv.FormatInt(p.ProjectID, 10))
	}
	if p.IID != 0 {
		set(LabelMR, strconv.FormatInt(p.IID, 10))
	}
	set(LabelHead, p.HeadSHA)
	set(LabelAPIClient, p.APIClient)
	return l
}

// annotations hold what a label value cannot: the project's full path and the
// change's URL. Nil when neither is set.
func annotations(p JobParams) map[string]string {
	var a map[string]string
	set := func(key, value string) {
		if value == "" {
			return
		}
		if a == nil {
			a = map[string]string{}
		}
		a[key] = value
	}
	set(AnnotationProject, p.ProjectPath)
	set(AnnotationMRURL, p.MRURL)
	return a
}

func secretsName(p JobParams) string { return p.Name + "-secrets" }
func inputsName(p JobParams) string  { return p.Name + "-inputs" }
func filesName(p JobParams) string   { return p.Name + "-files" }

func ptr[T any](v T) *T { return &v }

// workVolume is the work volume, limited to size unless that is zero.
func workVolume(size resource.Quantity) *corev1.EmptyDirVolumeSource {
	if size.IsZero() {
		return &corev1.EmptyDirVolumeSource{}
	}
	return &corev1.EmptyDirVolumeSource{SizeLimit: &size}
}

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
	hasCarried := p.Inputs != nil && len(p.Inputs.Carried) > 0
	hasDismissals := p.Inputs != nil && len(p.Inputs.Dismissals) > 0
	hasDescription := p.Inputs != nil && len(p.Inputs.Description) > 0
	hasLinked := p.Inputs != nil && len(p.Inputs.Linked) > 0

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
	if hasCarried {
		args = append(args, "--carried=/in/carried.json")
	}
	args = append(args,
		"--scrub-instructions",
		"--timeout="+p.Timeout.String(),
		"--concurrency="+strconv.Itoa(p.Concurrency),
		"--max-turns="+strconv.Itoa(p.MaxTurns),
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
	if hasDismissals {
		// An environment variable, not an argument: an older image ignores a
		// variable it doesn't know, but exits 2 on an unknown flag.
		mainEnv = append(mainEnv, corev1.EnvVar{Name: "PERGENT_DISMISSALS", Value: "/in/dismissals.json"})
	}
	if hasDescription {
		// An environment variable for the same reason.
		mainEnv = append(mainEnv, corev1.EnvVar{Name: "PERGENT_CHANGE_DESCRIPTION", Value: "/in/change-description.md"})
	}
	if hasLinked {
		// An environment variable for the same reason.
		mainEnv = append(mainEnv, corev1.EnvVar{Name: "PERGENT_LINKED_REPOS", Value: "/in/linked-repos.json"})
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
	if p.Import != nil {
		// No clone and no token: the commits are built from the mounted stream.
		checkout.Command = []string{"sh", "-ec", apiCheckoutScript}
		checkout.Env = []corev1.EnvVar{
			{Name: "BASE_SHA", Value: p.BaseSHA},
			{Name: "HEAD_SHA", Value: p.HeadSHA},
		}
		checkout.VolumeMounts = append(checkout.VolumeMounts, corev1.VolumeMount{Name: "files", MountPath: "/files", ReadOnly: true})
	}
	if len(p.LinkedClones) > 0 {
		lines := make([]string, len(p.LinkedClones))
		for i, c := range p.LinkedClones {
			lines[i] = c.Dir + " " + c.CloneURL
		}
		checkout.Env = append(checkout.Env, corev1.EnvVar{Name: "LINKED_CLONES", Value: strings.Join(lines, "\n")})
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
	if hasCarried {
		items = append(items, corev1.KeyToPath{Key: "carried.json", Path: "carried.json"})
	}
	if hasDismissals {
		items = append(items, corev1.KeyToPath{Key: "dismissals.json", Path: "dismissals.json"})
	}
	if hasDescription {
		items = append(items, corev1.KeyToPath{Key: "change-description.md", Path: "change-description.md"})
	}

	if hasLinked {
		items = append(items, corev1.KeyToPath{Key: "linked-repos.json", Path: "linked-repos.json"})
	}

	volumes := []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: workVolume(workSizeFor(p.WorkSize, len(p.LinkedClones)))}},
		{Name: "inputs", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: inputsName(p)},
			Items:                items,
		}}},
	}
	var podSecurity *corev1.PodSecurityContext
	if p.Import != nil {
		// Only the checkout container mounts it. The file is root's and 0400,
		// so the image's non-root user reads it through the group.
		volumes = append(volumes, corev1.Volume{Name: "files", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  filesName(p),
			Items:       []corev1.KeyToPath{{Key: keyImport, Path: keyImport}},
			DefaultMode: ptr(int32(0o400)),
		}}})
		podSecurity = &corev1.PodSecurityContext{FSGroup: ptr(int64(fetchUser))}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: labels(p), Annotations: annotations(p)},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(Deadline(p.Timeout, p.CloneAllowance) / time.Second)),
			TTLSecondsAfterFinished: ptr(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(p), Annotations: annotations(p)},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: ptr(int64(30)),
					AutomountServiceAccountToken:  ptr(false),
					EnableServiceLinks:            ptr(false),
					SecurityContext:               podSecurity,
					Volumes:                       volumes,
					InitContainers:                []corev1.Container{checkout},
					Containers:                    []corev1.Container{pergent},
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

// BuildConfigMap holds the skills, instructions, previous review, carried
// files, past dismissals and the change's description. owner may be nil (tests); Launch always passes
// the created Job.
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
		if len(p.Inputs.Carried) > 0 {
			data["carried.json"] = string(p.Inputs.Carried)
		}
		if len(p.Inputs.Dismissals) > 0 {
			data["dismissals.json"] = string(p.Inputs.Dismissals)
		}
		if len(p.Inputs.Description) > 0 {
			data["change-description.md"] = string(p.Inputs.Description)
		}
		if len(p.Inputs.Linked) > 0 {
			data["linked-repos.json"] = string(p.Inputs.Linked)
		}
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: inputsName(p), Namespace: p.Namespace, Labels: labels(p),
			OwnerReferences: ownerRef(owner),
		},
		Data: data,
	}
}

// BuildSecret holds the per-Job credentials, owned by the Job.
func BuildSecret(p JobParams, owner *batchv1.Job, s Secrets) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretsName(p), Namespace: p.Namespace, Labels: labels(p),
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

// BuildFilesSecret holds the stream an API review's checkout container builds
// its commits from, owned by the Job. It is mounted into that container alone.
func BuildFilesSecret(p JobParams, owner *batchv1.Job) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: filesName(p), Namespace: p.Namespace, Labels: labels(p),
			OwnerReferences: ownerRef(owner),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{keyImport: p.Import},
	}
}
