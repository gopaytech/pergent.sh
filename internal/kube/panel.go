package kube

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gopaytech/pergent.sh/internal/config"
)

// A review panel runs 3 Jobs. Members A and B are review Jobs with
// --no-skill-verify and the panel labels; each holds its own entry's key
// only. The merge, Job P, runs pergent agent --panel: a "panel-inputs" init
// container, pergent.sh's own image with the result token alone, downloads
// both member results into its own volume, outside /work, and pergent merges
// them. Job P's Secret holds model A's key and, when there is one, model B's
// (none with --panel-no-judge).

const (
	// LabelPanelMember and LabelPanelRole mark a panel's Jobs: the member's
	// ID and PanelRoleA or PanelRoleB, or PanelRoleMerge for Job P.
	LabelPanelMember = "pergent.sh/panel-member"
	LabelPanelRole   = "pergent.sh/panel-role"

	PanelRoleA     = "a"
	PanelRoleB     = "b"
	PanelRoleMerge = "p"

	// PanelTimeout is Job P's --timeout. Its deadline adds the clone and
	// deliver allowances (Deadline).
	PanelTimeout = 10 * time.Minute

	keyLLMKeyB = "llm-api-key-b"

	containerPanelInputs = "panel-inputs"
	panelMount           = "/panel"
	// PanelAFile and PanelBFile are where Job P's pergent finds the results.
	PanelAFile = panelMount + "/a.json"
	PanelBFile = panelMount + "/b.json"
	// panelSize bounds the panel volume: 2 results of at most 10 MiB each,
	// and a temp file while one downloads.
	panelSize = "32Mi"
)

// PanelParams is what a merge Job needs beyond a review Job's checkout.
type PanelParams struct {
	AURL, BURL string     // /runs/<id>/panel/a and /b on the callback URL
	B          config.LLM // model B's provider, model and base URL; its key is Secrets.LLMKeyB
	NoJudge    bool       // --panel-no-judge: the quota stopped the cross-check
	Image      string     // pergent.sh's own image, for get-panel
	// HasKeyB names model B's key in the pergent container. Launch sets it
	// from Secrets.LLMKeyB, so the env and the Secret always agree.
	HasKeyB bool
}

// forPanel is p as Job P is built from it: the inputs pergent refuses with
// --panel (previous, carried, description, linked repositories, CI results,
// graph, --no-skill-verify) are left out, so neither a flag nor an env default
// names them, and the timeout is PanelTimeout. p itself is not changed.
func forPanel(p JobParams) JobParams {
	if p.Panel == nil {
		return p
	}
	if p.Inputs != nil {
		in := *p.Inputs
		in.Previous, in.Carried, in.Description, in.Linked, in.CIResults = nil, nil, nil, nil, nil
		p.Inputs = &in
	}
	p.LinkedClones = nil
	p.GraphURL, p.GraphImage = "", ""
	p.SkipSkillVerify = false
	p.Timeout = PanelTimeout
	return p
}

// panelArgs are Job P's own flags.
func panelArgs(pp *PanelParams) []string {
	args := []string{"--panel", "--panel-a=" + PanelAFile, "--panel-b=" + PanelBFile}
	if pp.NoJudge {
		args = append(args, "--panel-no-judge")
	}
	return args
}

// panelEnv is model B, as pergent reads it with --panel. Its key is named
// only when there is one and a judge runs.
func panelEnv(p JobParams) []corev1.EnvVar {
	b := p.Panel.B
	env := []corev1.EnvVar{
		{Name: "PERGENT_PANEL_B_PROVIDER", Value: b.Provider},
		{Name: "PERGENT_PANEL_B_MODEL", Value: b.Model},
	}
	if b.BaseURL != "" {
		env = append(env, corev1.EnvVar{Name: "PERGENT_PANEL_B_BASE_URL", Value: b.BaseURL})
	}
	if p.Panel.HasKeyB && !p.Panel.NoJudge {
		env = append(env, secretEnv("PERGENT_PANEL_B_API_KEY", secretsName(p), keyLLMKeyB))
	}
	return env
}

// panelInputsContainer is Job P's panel-inputs init container: pergent.sh's
// own image, the result token alone, the panel volume alone. It always
// exits 0; a member with no result gets no file.
func panelInputsContainer(p JobParams) corev1.Container {
	sc := fetchLockedDown()
	sc.RunAsNonRoot = ptr(true)
	sc.RunAsUser = ptr(int64(fetchUser))
	sc.RunAsGroup = ptr(int64(fetchUser))
	return corev1.Container{
		Name:    containerPanelInputs,
		Image:   p.Panel.Image,
		Command: []string{"/pergent-sh"},
		Args:    []string{"get-panel", "--a-url=" + p.Panel.AURL, "--b-url=" + p.Panel.BURL, "--dir=" + panelMount},
		Env:     []corev1.EnvVar{secretEnv("PERGENT_RESULT_TOKEN", secretsName(p), keyResultToken)},
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

// panelVolume is Job P's panel volume.
func panelVolume() corev1.Volume {
	return corev1.Volume{Name: "panel", VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse(panelSize))},
	}}
}
