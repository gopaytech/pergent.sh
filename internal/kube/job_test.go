package kube

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
)

const (
	testBase = "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"
	testHead = "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b"
)

func testParams() JobParams {
	runID := "7f3a9c12-0b4d-4e5f-8a6b-1c2d3e4f5a6b"
	return JobParams{
		Name:           JobName(runID),
		Namespace:      "reviews",
		RunID:          runID,
		Image:          "ghcr.io/gopaytech/pergent:v0.1.0",
		Timeout:        10 * time.Minute,
		CloneAllowance: 2 * time.Minute,
		Concurrency:    4,
		RepoURL:        "https://gitlab.example.com/acme/widgets.git",
		MRIID:          42,
		TargetBranch:   "main",
		BaseSHA:        testBase,
		HeadSHA:        testHead,
		GitUsername:    "gitlab+deploy-token-7",
		ResultURL:      "https://pergent.example.com/runs/" + runID + "/result",
		LLM: config.LLM{
			Provider: "anthropic",
			Model:    "claude-sonnet-4",
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		},
		Inputs: &inputs.Inputs{
			Skills: map[string][]byte{
				"security":    []byte("# security\n"),
				"code-review": []byte("# code review\n"),
			},
			Instructions: []byte("be kind\n"),
			Previous:     []byte(`{"skills":{}}`),
		},
	}
}

func containerByName(t *testing.T, cs []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %q", name)
	return corev1.Container{}
}

func TestJobName(t *testing.T) {
	got := JobName("7f3a9c12-0b4d-4e5f-8a6b-1c2d3e4f5a6b")
	if got != "pergent-review-7f3a9c120b4d" {
		t.Errorf("JobName = %q", got)
	}
}

func TestDeadline(t *testing.T) {
	if got := Deadline(10*time.Minute, 2*time.Minute); got != 14*time.Minute {
		t.Errorf("Deadline = %v, want 14m", got)
	}
}

func TestCloneAllowance(t *testing.T) {
	for _, c := range []struct {
		secs int
		want time.Duration
	}{{0, 2 * time.Minute}, {30, 2 * time.Minute}, {60, 2 * time.Minute}, {90, 3 * time.Minute}, {300, 10 * time.Minute}} {
		if got := CloneAllowance(c.secs); got != c.want {
			t.Errorf("CloneAllowance(%d) = %v, want %v", c.secs, got, c.want)
		}
	}
}

func TestBuildJobShape(t *testing.T) {
	p := testParams()
	j := BuildJob(p)

	if j.Name != p.Name || j.Namespace != "reviews" {
		t.Errorf("name/namespace = %s/%s", j.Namespace, j.Name)
	}
	wantLabels := map[string]string{"app.kubernetes.io/managed-by": "pergent-sh", "pergent.sh/run": p.RunID}
	for k, v := range wantLabels {
		if j.Labels[k] != v {
			t.Errorf("job label %s = %q", k, j.Labels[k])
		}
		if j.Spec.Template.Labels[k] != v {
			t.Errorf("pod label %s = %q", k, j.Spec.Template.Labels[k])
		}
	}
	s := j.Spec
	if s.BackoffLimit == nil || *s.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v", s.BackoffLimit)
	}
	if s.ActiveDeadlineSeconds == nil || *s.ActiveDeadlineSeconds != 600+120+120 {
		t.Errorf("activeDeadlineSeconds = %v, want 840", s.ActiveDeadlineSeconds)
	}
	if s.TTLSecondsAfterFinished == nil || *s.TTLSecondsAfterFinished != 3600 {
		t.Errorf("ttlSecondsAfterFinished = %v", s.TTLSecondsAfterFinished)
	}
	if s.Suspend != nil && *s.Suspend {
		t.Errorf("BuildJob must not suspend; Launch does")
	}
	pod := s.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q", pod.RestartPolicy)
	}
	if pod.TerminationGracePeriodSeconds == nil || *pod.TerminationGracePeriodSeconds != 30 {
		t.Errorf("terminationGracePeriodSeconds = %v", pod.TerminationGracePeriodSeconds)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken must be false")
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
		t.Errorf("enableServiceLinks must be false")
	}
	if len(pod.InitContainers) != 1 || len(pod.Containers) != 1 {
		t.Fatalf("containers: %d init, %d main", len(pod.InitContainers), len(pod.Containers))
	}
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		if c.Image != p.Image {
			t.Errorf("%s image = %q", c.Name, c.Image)
		}
		sc := c.SecurityContext
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s: allowPrivilegeEscalation must be false", c.Name)
		}
		if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s: capabilities must drop ALL", c.Name)
		}
	}

	// volumes
	vols := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		vols[v.Name] = v
	}
	if vols["work"].EmptyDir == nil {
		t.Errorf("work must be an emptyDir")
	}
	cm := vols["inputs"].ConfigMap
	if cm == nil || cm.Name != p.Name+"-inputs" || len(cm.Items) == 0 {
		t.Errorf("inputs volume = %+v", vols["inputs"])
	}

	// init container
	init := containerByName(t, pod.InitContainers, "checkout")
	if len(init.Command) != 3 || init.Command[0] != "sh" || init.Command[1] != "-ec" || init.Command[2] != checkoutScript {
		t.Errorf("checkout command = %q", init.Command)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range init.Env {
		env[e.Name] = e
	}
	for k, v := range map[string]string{
		"REPO_URL": p.RepoURL, "MR_IID": "42", "TARGET_BRANCH": "main",
		"BASE_SHA": testBase, "HEAD_SHA": testHead, "GIT_USERNAME": "gitlab+deploy-token-7",
	} {
		if env[k].Value != v {
			t.Errorf("init env %s = %q, want %q", k, env[k].Value, v)
		}
	}
	if len(init.VolumeMounts) != 1 || init.VolumeMounts[0].Name != "work" || init.VolumeMounts[0].MountPath != "/work" {
		t.Errorf("init mounts = %+v", init.VolumeMounts)
	}

	// main container
	main := containerByName(t, pod.Containers, "pergent")
	if len(main.Command) != 1 || main.Command[0] != "pergent" {
		t.Errorf("command = %q", main.Command)
	}
	wantArgs := []string{
		"agent", "--repo-path=/work/repo", "--base=" + testBase, "--head=" + testHead,
		"--skills-dir=/in/skills", "--instructions=/in/instructions.md", "--previous=/in/previous.json",
		"--scrub-instructions", "--timeout=10m0s", "--concurrency=4", "--max-comment-size=990000",
		"--result-url=" + p.ResultURL,
	}
	if strings.Join(main.Args, "\n") != strings.Join(wantArgs, "\n") {
		t.Errorf("args =\n%q\nwant\n%q", main.Args, wantArgs)
	}
	menv := map[string]corev1.EnvVar{}
	for _, e := range main.Env {
		menv[e.Name] = e
	}
	if menv["OPENCODE_PROVIDER"].Value != "anthropic" || menv["OPENCODE_MODEL"].Value != "claude-sonnet-4" {
		t.Errorf("provider/model env = %+v / %+v", menv["OPENCODE_PROVIDER"], menv["OPENCODE_MODEL"])
	}
	if _, ok := menv["OPENCODE_BASE_URL"]; ok {
		t.Errorf("OPENCODE_BASE_URL must be absent when unset")
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range main.VolumeMounts {
		mounts[m.Name] = m
	}
	if mounts["work"].MountPath != "/work" || mounts["inputs"].MountPath != "/in" || !mounts["inputs"].ReadOnly {
		t.Errorf("main mounts = %+v", main.VolumeMounts)
	}
	if main.Resources.Requests.Memory().String() != "1Gi" {
		t.Errorf("resources not applied: %+v", main.Resources)
	}

	p.LLM.BaseURL = "https://llm.example.com/v1"
	main = containerByName(t, BuildJob(p).Spec.Template.Spec.Containers, "pergent")
	for _, e := range main.Env {
		if e.Name == "OPENCODE_BASE_URL" && e.Value == "https://llm.example.com/v1" {
			return
		}
	}
	t.Errorf("OPENCODE_BASE_URL missing when set")
}

func secretRefs(c corev1.Container) (keys []string, names map[string]bool) {
	names = map[string]bool{}
	for _, e := range c.Env {
		if r := e.ValueFrom; r != nil && r.SecretKeyRef != nil {
			keys = append(keys, r.SecretKeyRef.Key)
			names[r.SecretKeyRef.Name] = true
		}
	}
	return
}

func TestSecretsSplitPerContainer(t *testing.T) {
	p := testParams()
	j := BuildJob(p)
	pod := j.Spec.Template.Spec

	init := containerByName(t, pod.InitContainers, "checkout")
	keys, names := secretRefs(init)
	if strings.Join(keys, ",") != "git-token" || !names[p.Name+"-secrets"] || len(names) != 1 {
		t.Errorf("init secret refs = %v %v", keys, names)
	}
	main := containerByName(t, pod.Containers, "pergent")
	keys, names = secretRefs(main)
	got := map[string]bool{}
	for _, k := range keys {
		got[k] = true
	}
	if len(keys) != 2 || !got["llm-api-key"] || !got["result-token"] || !names[p.Name+"-secrets"] || len(names) != 1 {
		t.Errorf("main secret refs = %v %v", keys, names)
	}
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		if len(c.EnvFrom) != 0 {
			t.Errorf("%s uses envFrom", c.Name)
		}
	}
	for _, v := range pod.Volumes {
		if v.Secret != nil {
			t.Errorf("secret volume %q mounted", v.Name)
		}
	}

	// No literal token anywhere in the Job or ConfigMap.
	s := Secrets{GitToken: "glpat-GITSECRET-1a2b3c", LLMKey: "sk-LLMSECRET-4d5e6f", ResultToken: "RESULTSECRET-7a8b9c"}
	owner := j.DeepCopy()
	owner.UID = "uid-1"
	for name, obj := range map[string]any{"job": j, "configmap": BuildConfigMap(p, owner)} {
		b, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{s.GitToken, s.LLMKey, s.ResultToken} {
			if strings.Contains(string(b), tok) {
				t.Errorf("%s contains token %q", name, tok)
			}
		}
	}
	// ... and they are in the Secret.
	sec := BuildSecret(p, owner, s)
	if sec.Name != p.Name+"-secrets" || sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("secret = %s %s", sec.Name, sec.Type)
	}
	if string(sec.Data["git-token"]) != s.GitToken || string(sec.Data["llm-api-key"]) != s.LLMKey || string(sec.Data["result-token"]) != s.ResultToken || len(sec.Data) != 3 {
		t.Errorf("secret data = %v", sec.Data)
	}
	for _, o := range []struct {
		kind string
		refs []metaRef
	}{{"secret", refsOf(sec.OwnerReferences)}, {"configmap", refsOf(BuildConfigMap(p, owner).OwnerReferences)}} {
		if len(o.refs) != 1 || o.refs[0] != (metaRef{"batch/v1", "Job", p.Name, "uid-1", false}) {
			t.Errorf("%s ownerRefs = %+v", o.kind, o.refs)
		}
	}
}

type metaRef struct {
	APIVersion, Kind, Name, UID string
	Controller                  bool
}

func refsOf(in []metav1.OwnerReference) []metaRef {
	var out []metaRef
	for _, r := range in {
		c := false
		if r.Controller != nil {
			c = *r.Controller
		}
		out = append(out, metaRef{r.APIVersion, r.Kind, r.Name, string(r.UID), c})
	}
	return out
}

func TestCheckoutScriptHasNoTokenInURL(t *testing.T) {
	p := testParams()
	if strings.Contains(p.RepoURL, "@") {
		t.Fatalf("test URL has userinfo")
	}
	init := containerByName(t, BuildJob(p).Spec.Template.Spec.InitContainers, "checkout")
	for _, e := range init.Env {
		if e.Name == "REPO_URL" && (strings.Contains(e.Value, "@") || strings.Contains(e.Value, "token")) {
			t.Errorf("REPO_URL carries credentials: %q", e.Value)
		}
	}
	for _, line := range strings.Split(checkoutScript, "\n") {
		if strings.Contains(line, "git config") && strings.Contains(line, "TOKEN") {
			t.Errorf("script writes the token to git config: %q", line)
		}
		if strings.Contains(line, "remote add") && strings.Contains(line, "TOKEN") {
			t.Errorf("script puts the token in the remote: %q", line)
		}
	}
	for _, must := range []string{
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"git config core.hooksPath /dev/null",
		"git remote set-url --push origin pergent-push-disabled:",
		`git remote add origin "$REPO_URL"`,
		`git checkout -q --detach "$HEAD_SHA"`,
	} {
		if !strings.Contains(checkoutScript, must) {
			t.Errorf("script lacks %q", must)
		}
	}
}

func TestArgsOptionalInputs(t *testing.T) {
	p := testParams()
	p.Inputs = &inputs.Inputs{Skills: map[string][]byte{"a": []byte("x")}}
	main := containerByName(t, BuildJob(p).Spec.Template.Spec.Containers, "pergent")
	for _, a := range main.Args {
		if strings.HasPrefix(a, "--instructions") || strings.HasPrefix(a, "--previous") {
			t.Errorf("unexpected arg %q", a)
		}
	}
	cm := BuildConfigMap(p, nil)
	if _, ok := cm.Data["instructions.md"]; ok {
		t.Errorf("instructions.md present")
	}
	if _, ok := cm.Data["previous.json"]; ok {
		t.Errorf("previous.json present")
	}
	vol := BuildJob(p).Spec.Template.Spec.Volumes
	for _, v := range vol {
		if v.ConfigMap != nil && len(v.ConfigMap.Items) != 1 {
			t.Errorf("items = %+v", v.ConfigMap.Items)
		}
	}
}

func TestConfigMapItems(t *testing.T) {
	p := testParams()
	owner := &batchv1.Job{}
	owner.Name = p.Name
	owner.UID = "u"
	cm := BuildConfigMap(p, owner)
	if cm.Name != p.Name+"-inputs" || cm.Namespace != "reviews" {
		t.Errorf("cm = %s/%s", cm.Namespace, cm.Name)
	}
	// sorted: code-review=0, security=1
	if cm.Data["skill-0.md"] != "# code review\n" || cm.Data["skill-1.md"] != "# security\n" {
		t.Errorf("skills data = %v", cm.Data)
	}
	if cm.Data["instructions.md"] != "be kind\n" || cm.Data["previous.json"] != `{"skills":{}}` || len(cm.Data) != 4 {
		t.Errorf("data = %v", cm.Data)
	}
	var items []corev1.KeyToPath
	for _, v := range BuildJob(p).Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil {
			items = v.ConfigMap.Items
		}
	}
	want := map[string]string{
		"skill-0.md": "skills/code-review.md", "skill-1.md": "skills/security.md",
		"instructions.md": "instructions.md", "previous.json": "previous.json",
	}
	if len(items) != len(want) {
		t.Fatalf("items = %+v", items)
	}
	for _, it := range items {
		if want[it.Key] != it.Path {
			t.Errorf("item %s -> %s, want %s", it.Key, it.Path, want[it.Key])
		}
		if _, ok := cm.Data[it.Key]; !ok {
			t.Errorf("item key %s not in data", it.Key)
		}
	}
}
