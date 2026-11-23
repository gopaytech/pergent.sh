package kube

import (
	"strconv"
	"strings"

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

	// JudgeTimeout is the judge Job's --timeout; JudgeFetchTimeout bounds
	// get-reviewers. M4.2 moves the values here and drops the Panel names.
	JudgeTimeout      = PanelTimeout
	JudgeFetchTimeout = PanelFetchTimeout

	// MRDismissalsFile is where the judge's pergent reads --mr-dismissals.
	MRDismissalsFile = "/in/mr-dismissals.json"

	containerReviewers = "reviewers"
	keyMRDismissals    = "mr-dismissals.json"
)

// JudgeParams is what a judge Job needs beyond a review Job's checkout.
type JudgeParams struct {
	BaseURL   string // <callback>/runs/<run>/reviewers
	Reviewers []int  // every reviewer of the frozen set, in order (D2)
	NoJudge   bool   // --no-judge: no model runs and no key is held
	Image     string // pergent.sh's own image, for get-reviewers
	// MRDismissals is the merge request's dismissals, JSON
	// [{"fingerprint","severity"}] (D11). Empty is written as [].
	MRDismissals []byte
}

// forJudge is p as the judge Job is built from it: the inputs pergent refuses
// with --multi-review are left out (as forPanel does), and the timeout is
// JudgeTimeout. p itself is not changed.
func forJudge(p JobParams) JobParams {
	if p.Judge == nil {
		return p
	}
	p = dropRefusedInputs(p)
	p.Timeout = JudgeTimeout
	return p
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
	return panelMount + "/reviewer-" + strconv.Itoa(n) + ".json"
}

// mrDismissalsData is the file's content: the dismissals, or [] for none.
func mrDismissalsData(jp *JudgeParams) string {
	if len(jp.MRDismissals) == 0 {
		return "[]"
	}
	return string(jp.MRDismissals)
}

// reviewersContainer is the judge Job's reviewers init container:
// pergent.sh's own image, the result token alone, the panel volume alone. It
// always exits 0; a reviewer with no result gets no file.
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
			"--dir=" + panelMount,
		},
		Env: []corev1.EnvVar{secretEnv("PERGENT_RESULT_TOKEN", secretsName(p), keyResultToken)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "panel", MountPath: panelMount},
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
