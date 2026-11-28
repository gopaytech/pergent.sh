package kube

import (
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// A multi review runs one Job per reviewer and one judge Job. A reviewer's
// Job is a review Job with --no-skill-verify and 2 labels; it holds only its
// own entry's key. The judge Job runs pergent agent --multi-review: a
// "reviewers" init container, pergent.sh's own image with the result token
// alone, downloads the reviewers' results into its own volume, outside /work,
// and pergent merges them. The judge Job's Secret holds the judge's key and
// none with --no-judge.

const (
	// LabelReviewMember and LabelReviewer mark a multi review's Jobs: the
	// member's ID, and the reviewer's number ("1" to "5") or ReviewerJudge.
	LabelReviewMember = "pergent.sh/review-member"
	LabelReviewer     = "pergent.sh/reviewer"
	ReviewerJudge     = "judge"

	// JudgeTimeout is the judge Job's --timeout. Its deadline adds the clone
	// and deliver allowances (Deadline).
	JudgeTimeout = 10 * time.Minute
	// JudgeFetchTimeout bounds get-reviewers in the reviewers container,
	// every result and every try. The judge Job's deadline adds it to
	// JudgeTimeout.
	JudgeFetchTimeout = 2 * time.Minute

	// MRDismissalsFile is where the judge's pergent reads --mr-dismissals.
	MRDismissalsFile = "/in/mr-dismissals.json"

	containerReviewers = "reviewers"
	keyMRDismissals    = "mr-dismissals.json"

	// keyLegacyKeyB is model B's key in the Secret of a legacy panel's merge
	// Job, which a main build made. No Job stores it now. JobSecrets still
	// reads it, so a log of such a Job never shows it. Remove it in the next
	// release.
	keyLegacyKeyB = "llm-api-key-b"

	// The judge Job's reviewers volume. Its name and path are the ones the
	// review panel used, so the judge Job's spec does not change.
	reviewersVolumeName = "panel"
	reviewersMount      = "/panel"
	// reviewersSize bounds the reviewers volume: 5 results of at most 10 MiB
	// each, and a temp file while one downloads.
	reviewersSize = "64Mi"
)

// JudgeParams is what a judge Job needs beyond a review Job's checkout.
type JudgeParams struct {
	BaseURL string // <callback>/runs/<run>/reviewers
	// Reviewers is every reviewer of the frozen set, in order (D2). It must be
	// 1..k: pergent numbers a reviewer by its flag position, not by file name.
	Reviewers []int
	NoJudge   bool   // --no-judge: no model runs and no key is held
	Image     string // pergent.sh's own image, for get-reviewers
	// MRDismissals is the merge request's dismissals, JSON
	// [{"fingerprint","severity"}] (D11). Empty is written as [].
	MRDismissals []byte
}

// forJudge is p as the judge Job is built from it: the inputs pergent refuses
// with --multi-review are left out (dropRefusedInputs), and the timeout is
// JudgeTimeout. p itself is not changed.
func forJudge(p JobParams) JobParams {
	if p.Judge == nil {
		return p
	}
	p = dropRefusedInputs(p)
	p.Timeout = JudgeTimeout
	return p
}

// dropRefusedInputs is p without the inputs pergent refuses with
// --multi-review: previous, carried, description, linked repositories, CI
// results, security findings, graph and --no-skill-verify. p itself is not changed.
func dropRefusedInputs(p JobParams) JobParams {
	if p.Inputs != nil {
		in := *p.Inputs
		in.Previous, in.Carried, in.Description, in.Linked, in.CIResults, in.SecurityFindings = nil, nil, nil, nil, nil, nil
		p.Inputs = &in
	}
	p.LinkedClones = nil
	p.GraphURL, p.GraphImage = "", ""
	p.SkipSkillVerify = false
	return p
}

// deadline is the Job's activeDeadlineSeconds. The judge Job's counts the
// result fetch too.
func deadline(p JobParams) time.Duration {
	if p.Judge != nil {
		return Deadline(p.Timeout+JudgeFetchTimeout, p.CloneAllowance)
	}
	return Deadline(p.Timeout, p.CloneAllowance)
}

// judgeArgs are the judge Job's own flags.
func judgeArgs(jp *JudgeParams) []string {
	args := []string{"--multi-review"}
	for _, n := range jp.Reviewers {
		args = append(args, "--reviewer="+reviewerFile(n))
	}
	args = append(args, "--mr-dismissals="+MRDismissalsFile)
	if jp.NoJudge {
		args = append(args, "--no-judge")
	}
	return args
}

// reviewerFile is where reviewer n's result is in the judge Job.
func reviewerFile(n int) string {
	return reviewersMount + "/reviewer-" + strconv.Itoa(n) + ".json"
}

// mrDismissalsData is the file's content: the dismissals, or [] for none.
func mrDismissalsData(jp *JudgeParams) string {
	if len(jp.MRDismissals) == 0 {
		return "[]"
	}
	return string(jp.MRDismissals)
}

// reviewersContainer is the judge Job's reviewers init container:
// pergent.sh's own image, the result token alone, the reviewers volume alone.
// A reviewer with no result (the route's marked 404) gets no file. On any
// other failure it exits 1, so the judge does not run on part of the
// results.
func reviewersContainer(p JobParams) corev1.Container {
	nums := make([]string, len(p.Judge.Reviewers))
	for i, n := range p.Judge.Reviewers {
		nums[i] = strconv.Itoa(n)
	}
	sc := fetchLockedDown()
	sc.RunAsNonRoot = ptr(true)
	sc.RunAsUser = ptr(int64(fetchUser))
	sc.RunAsGroup = ptr(int64(fetchUser))
	return corev1.Container{
		Name:    containerReviewers,
		Image:   p.Judge.Image,
		Command: []string{"/pergent-sh"},
		Args: []string{
			"get-reviewers",
			"--base-url=" + p.Judge.BaseURL,
			"--reviewers=" + strings.Join(nums, ","),
			"--dir=" + reviewersMount,
		},
		Env: []corev1.EnvVar{secretEnv("PERGENT_RESULT_TOKEN", secretsName(p), keyResultToken)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: reviewersVolumeName, MountPath: reviewersMount},
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

// reviewersVolume is the judge Job's reviewers volume.
func reviewersVolume() corev1.Volume {
	return corev1.Volume{Name: reviewersVolumeName, VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse(reviewersSize))},
	}}
}
