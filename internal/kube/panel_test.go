package kube

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/config"
)

const testMemberID = "0c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"

// panelParams is the golden review's params made a review panel's merge.
func panelParams() JobParams {
	p := goldenParams()
	p.Timeout = 25 * time.Minute // a review's timeout; Job P has its own
	p.PanelRole = PanelRoleMerge
	p.Panel = &PanelParams{
		AURL:    "http://pergent-sh:8080/runs/" + p.RunID + "/panel/a",
		BURL:    "http://pergent-sh:8080/runs/" + p.RunID + "/panel/b",
		B:       config.LLM{Provider: "openai", Model: "gpt-5", BaseURL: "https://b.example.com/v1"},
		Image:   "ghcr.io/gopaytech/pergent.sh:v0.5.0",
		HasKeyB: true,
	}
	return p
}

var panelSecrets = Secrets{GitToken: "git-token-value", LLMKey: "llm-key-a-value", LLMKeyB: "llm-key-b-value", ResultToken: "result-token-value"}

// A member Job is the review Job plus --no-skill-verify and the 2 panel
// labels, and nothing else.
func TestBuildMemberJob(t *testing.T) {
	for _, role := range []string{PanelRoleA, PanelRoleB} {
		t.Run(role, func(t *testing.T) {
			p := goldenParams()
			p.SkipSkillVerify = true
			p.PanelMemberID, p.PanelRole = testMemberID, role
			job := BuildJob(p)
			wantLabels := map[string]string{LabelPanelMember: testMemberID, LabelPanelRole: role, "pergent.sh/run": p.RunID}
			for what, l := range map[string]map[string]string{
				"job":        job.Labels,
				"pod":        job.Spec.Template.Labels,
				"secret":     BuildSecret(p, job, goldenSecrets).Labels,
				"config map": BuildConfigMap(p, job).Labels,
			} {
				for k, v := range wantLabels {
					if l[k] != v {
						t.Errorf("%s label %s = %q, want %q", what, k, l[k], v)
					}
				}
			}
			args := containerByName(t, job.Spec.Template.Spec.Containers, "pergent").Args
			if n := countOf(args, "--no-skill-verify"); n != 1 {
				t.Errorf("--no-skill-verify %d times in %v", n, args)
			}
			// A member's key is its own entry's: a key B given by mistake is not stored.
			s := goldenSecrets
			s.LLMKeyB = "stray-key"
			if strings.Contains(reviewGolden(t, p, s), "stray") {
				t.Error("a member Job's Secret holds a second key")
			}

			// Take the 3 additions away: what is left is the golden review Job.
			want, err := os.ReadFile("testdata/review-job.golden.json")
			if err != nil {
				t.Fatal(err)
			}
			got := withoutPanelMarks(t, reviewGolden(t, p, goldenSecrets))
			if got != withoutPanelMarks(t, string(want)) {
				t.Errorf("a member Job differs from the review Job by more than the flag and labels:\n%s", got)
			}
			if !strings.Contains(string(want), `"pergent.sh/run"`) || strings.Contains(string(want), "panel") {
				t.Error("the golden is not a review Job's")
			}
		})
	}
}

// withoutPanelMarks drops the panel labels and --no-skill-verify from a
// golden's JSON, and prints it again.
func withoutPanelMarks(t *testing.T, text string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if k == "labels" {
					if l, ok := e.(map[string]any); ok {
						delete(l, LabelPanelMember)
						delete(l, LabelPanelRole)
					}
				}
				x[k] = walk(e)
			}
		case []any:
			out := x[:0:0]
			for _, e := range x {
				if e != "--no-skill-verify" {
					out = append(out, walk(e))
				}
			}
			return out
		}
		return v
	}
	b, err := json.MarshalIndent(walk(v), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func countOf(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}

func TestBuildPanelJob(t *testing.T) {
	p := panelParams()
	p.SkipSkillVerify = true // refused with --panel: never passed on
	job := BuildJob(p)
	spec := job.Spec.Template.Spec
	pergent := containerByName(t, spec.Containers, "pergent")

	wantArgs := []string{
		"agent",
		"--repo-path=/work/repo",
		"--base=" + testBase,
		"--head=" + testHead,
		"--skills-dir=/in/skills",
		"--instructions=/in/instructions.md",
		"--panel",
		"--panel-a=/panel/a.json",
		"--panel-b=/panel/b.json",
		"--scrub-instructions",
		"--timeout=10m0s",
		"--concurrency=4",
		"--max-turns=40",
		"--max-comment-size=990000",
		"--result-url=" + p.ResultURL,
	}
	if !slices.Equal(pergent.Args, wantArgs) {
		t.Errorf("args:\n got %q\nwant %q", pergent.Args, wantArgs)
	}
	if got, want := *job.Spec.ActiveDeadlineSeconds, int64(Deadline(10*time.Minute+PanelFetchTimeout, p.CloneAllowance)/time.Second); got != want {
		t.Errorf("activeDeadlineSeconds = %d, want %d", got, want)
	}
	// 10m merge + 2m fetch + 2m clone + 2m deliver.
	if got := *job.Spec.ActiveDeadlineSeconds; got != 16*60 {
		t.Errorf("activeDeadlineSeconds = %d, want 960", got)
	}

	env := map[string]string{}
	for _, e := range pergent.Env {
		if _, dup := env[e.Name]; dup {
			t.Errorf("%s is set twice", e.Name)
		}
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"OPENCODE_PROVIDER":        "anthropic",
		"OPENCODE_MODEL":           "claude-sonnet-4",
		"OPENCODE_BASE_URL":        "https://llm.example.com/v1",
		"PERGENT_PANEL_B_PROVIDER": "openai",
		"PERGENT_PANEL_B_MODEL":    "gpt-5",
		"PERGENT_PANEL_B_BASE_URL": "https://b.example.com/v1",
		"PERGENT_DISMISSALS":       "/in/dismissals.json",
		"PERGENT_CHECK_DELETED":    "true",
	} {
		if got, ok := env[name]; !ok || got != want {
			t.Errorf("%s = %q, %v; want %q", name, got, ok, want)
		}
	}
	// The inputs pergent refuses with --panel, as a flag or as an env default.
	for _, name := range []string{"PERGENT_CHANGE_DESCRIPTION", "PERGENT_LINKED_REPOS", "PERGENT_CI_RESULTS", "PERGENT_GRAPH"} {
		if _, ok := env[name]; ok {
			t.Errorf("Job P sets %s", name)
		}
	}
	for _, a := range pergent.Args {
		for _, refused := range []string{"--previous", "--carried", "--ci-results", "--change-description", "--linked-repos", "--graph", "--no-skill-verify"} {
			if strings.HasPrefix(a, refused) {
				t.Errorf("Job P passes %s", a)
			}
		}
	}

	if len(spec.InitContainers) != 2 || spec.InitContainers[0].Name != "checkout" || spec.InitContainers[1].Name != "panel-inputs" {
		t.Fatalf("init containers = %v", names(spec.InitContainers))
	}
	if _, ok := envValue(spec.InitContainers[0].Env, "LINKED_CLONES"); ok {
		t.Error("Job P clones linked repositories it never reads")
	}
	in := spec.InitContainers[1]
	wantIn := []string{"get-panel", "--a-url=" + p.Panel.AURL, "--b-url=" + p.Panel.BURL, "--dir=/panel"}
	if in.Image != p.Panel.Image || strings.Join(in.Command, " ") != "/pergent-sh" || !slices.Equal(in.Args, wantIn) {
		t.Errorf("panel-inputs = %s %v %q", in.Image, in.Command, in.Args)
	}
	if len(in.VolumeMounts) != 1 || in.VolumeMounts[0] != (corev1.VolumeMount{Name: "panel", MountPath: "/panel"}) {
		t.Errorf("panel-inputs mounts = %+v", in.VolumeMounts)
	}
	sc := in.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("panel-inputs security context = %+v", sc)
	}
	var mounted bool
	for _, m := range pergent.VolumeMounts {
		if m.Name == "panel" {
			mounted = m.MountPath == "/panel" && m.ReadOnly
		}
		if m.Name == "graph" {
			t.Error("Job P mounts the graph")
		}
	}
	if !mounted {
		t.Errorf("pergent mounts = %+v; want /panel read-only", pergent.VolumeMounts)
	}
	var vols []string
	for _, v := range spec.Volumes {
		vols = append(vols, v.Name)
		if v.Name == "panel" && (v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil) {
			t.Errorf("panel volume = %+v; want a limited emptyDir", v)
		}
		if v.Name == "work" && v.EmptyDir.SizeLimit.String() != "20Gi" {
			t.Errorf("work volume = %v; want 20Gi with no linked clone", v.EmptyDir.SizeLimit)
		}
	}
	if !slices.Equal(vols, []string{"work", "inputs", "panel"}) {
		t.Errorf("volumes = %v", vols)
	}

	// The ConfigMap and its items: skills, instructions, dismissals only.
	cm := BuildConfigMap(p, job)
	keys := slices.Sorted(maps.Keys(cm.Data))
	if want := []string{"dismissals.json", "instructions.md", "skill-0.md", "skill-1.md"}; !slices.Equal(keys, want) {
		t.Errorf("config map keys = %v, want %v", keys, want)
	}
	for _, v := range spec.Volumes {
		if v.Name != "inputs" {
			continue
		}
		var items []string
		for _, it := range v.ConfigMap.Items {
			items = append(items, it.Key)
			if _, ok := cm.Data[it.Key]; !ok {
				t.Errorf("item %s has no key in the config map", it.Key)
			}
		}
		if len(items) != len(keys) {
			t.Errorf("items = %v, keys = %v", items, keys)
		}
	}

	wantLabels := map[string]string{"pergent.sh/run": p.RunID, LabelPanelRole: "p"}
	for k, v := range wantLabels {
		if job.Labels[k] != v || job.Spec.Template.Labels[k] != v {
			t.Errorf("label %s = %q / %q, want %q", k, job.Labels[k], job.Spec.Template.Labels[k], v)
		}
	}
	if _, ok := job.Labels[LabelPanelMember]; ok {
		t.Error("Job P carries a member label")
	}

	// The caller's params are left as they were.
	if p.Inputs.Previous == nil || p.Inputs.CIResults == nil || len(p.LinkedClones) != 1 || p.Timeout != 25*time.Minute {
		t.Error("BuildJob changed the caller's params")
	}
}

func names(cs []corev1.Container) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// keyRefs are the secret keys each container's env reads, by variable name.
func keyRefs(job []corev1.Container) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, c := range job {
		out[c.Name] = map[string]string{}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				out[c.Name][e.Name] = e.ValueFrom.SecretKeyRef.Key
			}
		}
	}
	return out
}

func allContainers(p JobParams) []corev1.Container {
	spec := BuildJob(p).Spec.Template.Spec
	return append(slices.Clone(spec.InitContainers), spec.Containers...)
}

// Each key is named once, only in the pergent container; panel-inputs holds
// the result token alone; no key is in the Job or the ConfigMap.
func TestBuildPanelJobKeys(t *testing.T) {
	p := panelParams()
	job := BuildJob(p)
	sec := BuildSecret(p, job, panelSecrets)
	want := map[string]string{"git-token": "git-token-value", "llm-api-key": "llm-key-a-value", "llm-api-key-b": "llm-key-b-value", "result-token": "result-token-value"}
	if len(sec.Data) != len(want) {
		t.Errorf("secret keys = %v", slices.Sorted(maps.Keys(sec.Data)))
	}
	for k, v := range want {
		if string(sec.Data[k]) != v {
			t.Errorf("secret %s = %q, want %q", k, sec.Data[k], v)
		}
	}
	refs := keyRefs(allContainers(p))
	wantRefs := map[string]map[string]string{
		"checkout":     {"GIT_TOKEN": "git-token"},
		"panel-inputs": {"PERGENT_RESULT_TOKEN": "result-token"},
		"pergent":      {"OPENCODE_API_KEY": "llm-api-key", "PERGENT_PANEL_B_API_KEY": "llm-api-key-b", "PERGENT_RESULT_TOKEN": "result-token"},
	}
	if !maps.EqualFunc(refs, wantRefs, maps.Equal) {
		t.Errorf("secret refs = %v, want %v", refs, wantRefs)
	}
	if n := len(keyRefs(BuildJob(p).Spec.Template.Spec.InitContainers)["panel-inputs"]); n != 1 {
		t.Errorf("panel-inputs reads %d secrets", n)
	}
	for _, c := range allContainers(p) {
		if c.Name == "panel-inputs" && len(c.Env) != 1 {
			t.Errorf("panel-inputs env = %+v; want the result token alone", c.Env)
		}
	}
	noKeyIn(t, job, BuildConfigMap(p, job))
}

func noKeyIn(t *testing.T, objs ...any) {
	t.Helper()
	for _, o := range objs {
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{panelSecrets.LLMKey, panelSecrets.LLMKeyB, panelSecrets.GitToken, panelSecrets.ResultToken} {
			if strings.Contains(string(b), v) {
				t.Errorf("%T holds %q", o, v)
			}
		}
	}
}

// An empty key B: no llm-api-key-b in the Secret and no PERGENT_PANEL_B_API_KEY.
func TestBuildPanelJobOneKey(t *testing.T) {
	p := panelParams()
	p.Panel.HasKeyB = false
	s := panelSecrets
	s.LLMKeyB = ""
	sec := BuildSecret(p, BuildJob(p), s)
	if _, ok := sec.Data["llm-api-key-b"]; ok {
		t.Error("the Secret holds an empty llm-api-key-b")
	}
	if string(sec.Data["llm-api-key"]) != s.LLMKey {
		t.Errorf("llm-api-key = %q", sec.Data["llm-api-key"])
	}
	refs := keyRefs(allContainers(p))["pergent"]
	if _, ok := refs["PERGENT_PANEL_B_API_KEY"]; ok {
		t.Error("PERGENT_PANEL_B_API_KEY is set with no key B")
	}
	if refs["OPENCODE_API_KEY"] != "llm-api-key" {
		t.Errorf("pergent refs = %v", refs)
	}
	// Model B is still named: pergent needs it with --panel.
	if v, _ := envValue(containerByName(t, BuildJob(p).Spec.Template.Spec.Containers, "pergent").Env, "PERGENT_PANEL_B_MODEL"); v != "gpt-5" {
		t.Errorf("PERGENT_PANEL_B_MODEL = %q", v)
	}
}

// --panel-no-judge runs no model: no LLM key in the Secret or the Job.
func TestBuildPanelJobNoJudge(t *testing.T) {
	p := panelParams()
	p.Panel.NoJudge = true
	job := BuildJob(p)
	args := containerByName(t, job.Spec.Template.Spec.Containers, "pergent").Args
	if countOf(args, "--panel-no-judge") != 1 || countOf(args, "--panel") != 1 {
		t.Errorf("args = %q", args)
	}
	// pergent refuses --panel without model B, also with --panel-no-judge.
	env := containerByName(t, job.Spec.Template.Spec.Containers, "pergent").Env
	for name, want := range map[string]string{
		"PERGENT_PANEL_B_PROVIDER": "openai",
		"PERGENT_PANEL_B_MODEL":    "gpt-5",
		"PERGENT_PANEL_B_BASE_URL": "https://b.example.com/v1",
		"OPENCODE_PROVIDER":        "anthropic",
		"OPENCODE_MODEL":           "claude-sonnet-4",
	} {
		if got, ok := envValue(env, name); !ok || got != want {
			t.Errorf("%s = %q, %v; want %q", name, got, ok, want)
		}
	}
	sec := BuildSecret(p, job, panelSecrets)
	if got := slices.Sorted(maps.Keys(sec.Data)); !slices.Equal(got, []string{"git-token", "result-token"}) {
		t.Errorf("secret keys = %v", got)
	}
	for c, refs := range keyRefs(allContainers(p)) {
		for name, key := range refs {
			if strings.HasPrefix(key, "llm-") || name == "OPENCODE_API_KEY" || name == "PERGENT_PANEL_B_API_KEY" {
				t.Errorf("%s reads %s from %s", c, name, key)
			}
		}
	}
}

// Without a panel, a key B never reaches a review Job's Secret.
func TestBuildSecretIgnoresKeyBWithoutPanel(t *testing.T) {
	p := goldenParams()
	s := goldenSecrets
	s.LLMKeyB = "llm-key-b-value"
	if _, ok := BuildSecret(p, nil, s).Data["llm-api-key-b"]; ok {
		t.Error("a review Job's Secret holds llm-api-key-b")
	}
}

// Launch tells BuildJob whether there is a key B from the Secrets it gets,
// so the env and the Secret always agree.
func TestLaunchPanelJobKeyB(t *testing.T) {
	for _, keyB := range []string{"", "llm-key-b-value"} {
		l, cs := fakeLauncher()
		p := panelParams()
		p.Panel.HasKeyB = keyB == "" // the opposite: Launch must correct it
		s := panelSecrets
		s.LLMKeyB = keyB
		if err := l.Launch(context.Background(), p, s); err != nil {
			t.Fatal(err)
		}
		job, err := cs.BatchV1().Jobs(l.Namespace).Get(context.Background(), p.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, hasEnv := keyRefs(job.Spec.Template.Spec.Containers)["pergent"]["PERGENT_PANEL_B_API_KEY"]
		sec, err := cs.CoreV1().Secrets(l.Namespace).Get(context.Background(), p.Name+"-secrets", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, hasKey := sec.Data["llm-api-key-b"]
		if hasEnv != (keyB != "") || hasKey != (keyB != "") {
			t.Errorf("key B %q: env %v, secret key %v", keyB, hasEnv, hasKey)
		}
		if p.Panel.HasKeyB != (keyB == "") {
			t.Error("Launch changed the caller's PanelParams")
		}
	}
}
