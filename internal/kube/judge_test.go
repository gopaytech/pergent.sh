package kube

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const testReviewMemberID = "5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9"

var judgeSecrets = Secrets{GitToken: "git-token-value", LLMKey: "judge-key-value", ResultToken: "result-token-value"}

// judgeParams is the golden review's params made a judge Job.
func judgeParams() JobParams {
	p := goldenParams()
	p.Timeout = 25 * time.Minute // a review's timeout; the judge has its own
	p.Reviewer = ReviewerJudge
	p.Judge = &JudgeParams{
		BaseURL:      "http://pergent-sh:8080/runs/" + p.RunID + "/reviewers",
		Reviewers:    []int{1, 2, 3},
		Image:        "ghcr.io/gopaytech/pergent.sh:v0.5.0",
		MRDismissals: []byte(`[{"fingerprint":"fp1","severity":"high"}]`),
	}
	return p
}

// A reviewer Job is the review Job plus --no-skill-verify and the 2 labels.
func TestBuildReviewerJob(t *testing.T) {
	p := goldenParams()
	p.SkipSkillVerify = true
	p.ReviewMemberID, p.Reviewer = testReviewMemberID, "3"
	job := BuildJob(p)
	wantLabels := map[string]string{LabelReviewMember: testReviewMemberID, LabelReviewer: "3", "pergent.sh/run": p.RunID}
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
	for _, a := range args {
		if strings.HasPrefix(a, "--mr-dismissals") || strings.HasPrefix(a, "--multi-review") {
			t.Errorf("a reviewer Job passes %s", a)
		}
	}
	if _, ok := BuildConfigMap(p, job).Data["mr-dismissals.json"]; ok {
		t.Error("a reviewer Job has mr-dismissals.json")
	}
	// Take the 3 additions away: what is left is the golden review Job.
	want, err := os.ReadFile("testdata/review-job.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	got := withoutReviewMarks(t, reviewGolden(t, p, goldenSecrets))
	if got != withoutReviewMarks(t, string(want)) {
		t.Errorf("a reviewer Job differs from the review Job by more than the flag and labels:\n%s", got)
	}
}

// withoutReviewMarks drops the 2 reviewer labels, then the panel marks
// (--no-skill-verify), from a golden's JSON, and prints it again.
func withoutReviewMarks(t *testing.T, text string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if l, ok := x["labels"].(map[string]any); ok {
				delete(l, LabelReviewMember)
				delete(l, LabelReviewer)
			}
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return withoutPanelMarks(t, string(b))
}

func TestBuildJudgeJob(t *testing.T) {
	p := judgeParams()
	p.SkipSkillVerify = true // never passed on to the judge
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
		"--multi-review",
		"--reviewer=/panel/reviewer-1.json",
		"--reviewer=/panel/reviewer-2.json",
		"--reviewer=/panel/reviewer-3.json",
		"--mr-dismissals=/in/mr-dismissals.json",
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
	if got, want := *job.Spec.ActiveDeadlineSeconds, int64(Deadline(JudgeTimeout+JudgeFetchTimeout, p.CloneAllowance)/time.Second); got != want {
		t.Errorf("activeDeadlineSeconds = %d, want %d", got, want)
	}
	if got := *job.Spec.ActiveDeadlineSeconds; got != 16*60 {
		t.Errorf("activeDeadlineSeconds = %d, want 960", got)
	}
	if job.Labels[LabelReviewer] != ReviewerJudge || job.Spec.Template.Labels[LabelReviewer] != ReviewerJudge {
		t.Errorf("reviewer label = %q", job.Labels[LabelReviewer])
	}
	if _, ok := job.Labels[LabelReviewMember]; ok {
		t.Error("the judge Job carries a member label with no member id")
	}

	env := map[string]string{}
	for _, e := range pergent.Env {
		if _, dup := env[e.Name]; dup {
			t.Errorf("%s is set twice", e.Name)
		}
		env[e.Name] = e.Value
		if strings.HasPrefix(e.Name, "PERGENT_PANEL_B_") {
			t.Errorf("the judge Job sets %s", e.Name)
		}
	}
	for _, name := range []string{"PERGENT_CHANGE_DESCRIPTION", "PERGENT_LINKED_REPOS", "PERGENT_CI_RESULTS", "PERGENT_GRAPH"} {
		if _, ok := env[name]; ok {
			t.Errorf("the judge Job sets %s", name)
		}
	}
	for _, a := range pergent.Args {
		for _, refused := range []string{"--previous", "--carried", "--ci-results", "--change-description", "--linked-repos", "--graph", "--no-skill-verify", "--panel"} {
			if strings.HasPrefix(a, refused) {
				t.Errorf("the judge Job passes %s", a)
			}
		}
	}
	if len(spec.InitContainers) != 2 || spec.InitContainers[0].Name != "checkout" || spec.InitContainers[1].Name != "reviewers" {
		t.Fatalf("init containers = %v", names(spec.InitContainers))
	}
	if _, ok := envValue(spec.InitContainers[0].Env, "LINKED_CLONES"); ok {
		t.Error("the judge Job clones linked repositories it never reads")
	}
	in := spec.InitContainers[1]
	wantIn := []string{"get-reviewers", "--base-url=" + p.Judge.BaseURL, "--reviewers=1,2,3", "--dir=/panel"}
	if in.Image != p.Judge.Image || strings.Join(in.Command, " ") != "/pergent-sh" || !slices.Equal(in.Args, wantIn) {
		t.Errorf("reviewers = %s %v %q", in.Image, in.Command, in.Args)
	}
	if len(in.VolumeMounts) != 1 || in.VolumeMounts[0] != (corev1.VolumeMount{Name: "panel", MountPath: "/panel"}) {
		t.Errorf("reviewers mounts = %+v", in.VolumeMounts)
	}
	sc := in.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("reviewers security context = %+v", sc)
	}
	var vols []string
	for _, v := range spec.Volumes {
		vols = append(vols, v.Name)
		if v.Name == "panel" && (v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.String() != "64Mi") {
			t.Errorf("panel volume = %+v; want a 64Mi emptyDir", v)
		}
	}
	if !slices.Equal(vols, []string{"work", "inputs", "panel"}) {
		t.Errorf("volumes = %v", vols)
	}
	var mounted bool
	for _, m := range pergent.VolumeMounts {
		if m.Name == "panel" {
			mounted = m.MountPath == "/panel" && m.ReadOnly
		}
	}
	if !mounted {
		t.Errorf("pergent mounts = %+v; want /panel read-only", pergent.VolumeMounts)
	}

	// The caller's params are left as they were.
	if p.Inputs.Previous == nil || p.Inputs.CIResults == nil || len(p.LinkedClones) != 1 || p.Timeout != 25*time.Minute || !p.SkipSkillVerify {
		t.Error("BuildJob changed the caller's params")
	}
	// pergent's --timeout is the judge's, whatever the review's was.
	if _, ok := envValue(pergent.Env, "PERGENT_PANEL_B_MODEL"); ok {
		t.Error("model B is set")
	}
}

func TestBuildJudgeJobMRDismissals(t *testing.T) {
	itemsOf := func(job interface{ GetName() string }, p JobParams) []string {
		var out []string
		for _, v := range BuildJob(p).Spec.Template.Spec.Volumes {
			if v.Name == "inputs" {
				for _, it := range v.ConfigMap.Items {
					out = append(out, it.Key+"="+it.Path)
				}
			}
		}
		return out
	}
	p := judgeParams()
	job := BuildJob(p)
	cm := BuildConfigMap(p, job)
	if got := cm.Data["mr-dismissals.json"]; got != string(p.Judge.MRDismissals) {
		t.Errorf("mr-dismissals.json = %q", got)
	}
	if !slices.Contains(itemsOf(job, p), "mr-dismissals.json=mr-dismissals.json") {
		t.Errorf("items = %v", itemsOf(job, p))
	}
	inputs := containerByName(t, job.Spec.Template.Spec.Containers, "pergent").VolumeMounts
	var ro bool
	for _, m := range inputs {
		if m.Name == "inputs" {
			ro = m.MountPath == "/in" && m.ReadOnly
		}
	}
	if !ro {
		t.Errorf("inputs mount = %+v", inputs)
	}

	// Empty is written as [].
	e := judgeParams()
	e.Judge.MRDismissals = nil
	if got := BuildConfigMap(e, job).Data["mr-dismissals.json"]; got != "[]" {
		t.Errorf("empty mr-dismissals.json = %q, want []", got)
	}
	if !slices.Contains(itemsOf(job, e), "mr-dismissals.json=mr-dismissals.json") {
		t.Error("an empty judge Job has no mr-dismissals item")
	}

	// --no-judge keeps the file and the flag.
	n := judgeParams()
	n.Judge.NoJudge = true
	args := containerByName(t, BuildJob(n).Spec.Template.Spec.Containers, "pergent").Args
	if !slices.Contains(args, "--mr-dismissals=/in/mr-dismissals.json") || !slices.Contains(args, "--no-judge") {
		t.Errorf("no-judge args = %q", args)
	}
	if _, ok := BuildConfigMap(n, job).Data["mr-dismissals.json"]; !ok {
		t.Error("a --no-judge Job has no mr-dismissals.json")
	}

	// A review Job and a reviewer Job have neither.
	for name, r := range map[string]JobParams{"review": goldenParams(), "reviewer": func() JobParams {
		q := goldenParams()
		q.SkipSkillVerify, q.ReviewMemberID, q.Reviewer = true, testReviewMemberID, "2"
		return q
	}()} {
		if _, ok := BuildConfigMap(r, job).Data["mr-dismissals.json"]; ok {
			t.Errorf("a %s Job has mr-dismissals.json", name)
		}
		for _, it := range itemsOf(job, r) {
			if strings.Contains(it, "mr-dismissals") {
				t.Errorf("a %s Job maps %s", name, it)
			}
		}
		for _, a := range containerByName(t, BuildJob(r).Spec.Template.Spec.Containers, "pergent").Args {
			if strings.HasPrefix(a, "--mr-dismissals") || a == "--no-judge" || a == "--multi-review" {
				t.Errorf("a %s Job passes %s", name, a)
			}
		}
	}
}

func TestBuildJudgeJobKeys(t *testing.T) {
	p := judgeParams()
	job := BuildJob(p)
	sec := BuildSecret(p, job, judgeSecrets)
	if got := slices.Sorted(maps.Keys(sec.Data)); !slices.Equal(got, []string{"git-token", "llm-api-key", "result-token"}) {
		t.Errorf("secret keys = %v", got)
	}
	if string(sec.Data["llm-api-key"]) != "judge-key-value" {
		t.Error("the judge key is not llm-api-key")
	}
	// A stray key B is never stored.
	s := judgeSecrets
	s.LLMKeyB = "stray-key"
	if _, ok := BuildSecret(p, job, s).Data["llm-api-key-b"]; ok {
		t.Error("a judge Job holds llm-api-key-b")
	}
	refs := keyRefs(allContainers(p))
	want := map[string]map[string]string{
		"checkout":  {"GIT_TOKEN": "git-token"},
		"reviewers": {"PERGENT_RESULT_TOKEN": "result-token"},
		"pergent":   {"OPENCODE_API_KEY": "llm-api-key", "PERGENT_RESULT_TOKEN": "result-token"},
	}
	if !maps.EqualFunc(refs, want, maps.Equal) {
		t.Errorf("secret refs = %v, want %v", refs, want)
	}
	for _, c := range allContainers(p) {
		if c.Name == "reviewers" && len(c.Env) != 1 {
			t.Errorf("reviewers env = %+v; want the result token alone", c.Env)
		}
	}
	noKeyIn(t, job, BuildConfigMap(p, job))
}

func TestBuildJudgeJobNoJudge(t *testing.T) {
	p := judgeParams()
	p.Judge.NoJudge = true
	job := BuildJob(p)
	sec := BuildSecret(p, job, judgeSecrets)
	if got := slices.Sorted(maps.Keys(sec.Data)); !slices.Equal(got, []string{"git-token", "result-token"}) {
		t.Errorf("secret keys = %v", got)
	}
	args := containerByName(t, job.Spec.Template.Spec.Containers, "pergent").Args
	if countOf(args, "--no-judge") != 1 {
		t.Errorf("args = %q", args)
	}
	if i := slices.Index(args, "--no-judge"); i < slices.Index(args, "--mr-dismissals=/in/mr-dismissals.json") {
		t.Errorf("--no-judge is before --mr-dismissals: %q", args)
	}
	for _, c := range allContainers(p) {
		for _, e := range c.Env {
			if e.Name == "OPENCODE_API_KEY" {
				t.Errorf("%s names OPENCODE_API_KEY with --no-judge", c.Name)
			}
		}
	}
}
