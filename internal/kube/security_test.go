package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/security"
)

const testSecurityID = "9b1e7c20-4d3a-4f8b-a5c6-0e2d1f3a4b5c"

func testSecurityParams() SecurityJobParams {
	return SecurityJobParams{
		Name:      SecurityJobName(testSecurityID),
		Namespace: "reviews",
		ScanID:    testSecurityID,
		Image:     "ghcr.io/gopaytech/pergent:v0.1.2",
		Commit:    "0123456789abcdef0123456789abcdef01234567",
		SourceURL: "http://pergent-sh.pergent-sh.svc:8080/security-scans/" + testSecurityID + "/source",
		ResultURL: "http://pergent-sh.pergent-sh.svc:8080/security-scans/" + testSecurityID + "/result",
		LLM:       config.LLM{Provider: "anthropic", Model: "claude-sonnet-4"},
		Timeout:   90 * time.Minute,
		Deadline:  100 * time.Minute,
		WorkSize:  resource.MustParse("4Gi"),
	}
}

func envByName(c corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

func TestSecurityJobName(t *testing.T) {
	if got := SecurityJobName(testSecurityID); got != "pergent-security-9b1e7c204d3a" {
		t.Errorf("SecurityJobName = %q", got)
	}
}

func TestBuildSecurityJob(t *testing.T) {
	ptrInt := func(n int) *int { return &n }
	budget := int64(2_000_000)
	cases := []struct {
		name     string
		mutate   func(*SecurityJobParams)
		wantArgs []string
		budget   bool
		prior    bool
	}{
		{"plain", func(*SecurityJobParams) {}, nil, false, false},
		{"step limits", func(p *SecurityJobParams) { p.MaxSteps, p.SubagentMaxSteps = ptrInt(120), ptrInt(40) },
			[]string{"--max-turns=120", "--max-subagent-turns=40"}, false, false},
		{"main limit only", func(p *SecurityJobParams) { p.MaxSteps = ptrInt(120) }, []string{"--max-turns=120"}, false, false},
		{"budget", func(p *SecurityJobParams) { p.TokenBudget = &budget }, nil, true, false},
		{"prior", func(p *SecurityJobParams) { p.Prior = []byte(`{"version":1}`) }, []string{"--prior=/in/prior.json"}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := testSecurityParams()
			c.mutate(&p)
			j := BuildSecurityJob(p)
			spec := j.Spec.Template.Spec

			if len(spec.InitContainers) != 0 || len(spec.Containers) != 1 || spec.Containers[0].Name != "pergent" {
				t.Fatalf("containers = %d init, %v", len(spec.InitContainers), spec.Containers)
			}
			ct := spec.Containers[0]
			if ct.Image != p.Image {
				t.Errorf("image = %q", ct.Image)
			}
			want := []string{
				"scan", "--repo-path=/work/repo", "--head=" + p.Commit,
				"--source-url=" + p.SourceURL, "--result-url=" + p.ResultURL, "--timeout=1h30m0s",
			}
			want = append(want, c.wantArgs...)
			if strings.Join(ct.Args, " ") != strings.Join(want, " ") {
				t.Errorf("args = %v\nwant %v", ct.Args, want)
			}
			if hasArg(ct.Args, "--prior=/in/prior.json") != c.prior {
				t.Errorf("--prior present = %v, want %v", !c.prior, c.prior)
			}
			env := envByName(ct)
			if _, ok := env["PERGENT_TOKEN_BUDGET"]; ok != c.budget {
				t.Errorf("PERGENT_TOKEN_BUDGET present = %v, want %v", ok, c.budget)
			}
			if c.budget && env["PERGENT_TOKEN_BUDGET"].Value != "2000000" {
				t.Errorf("budget = %q", env["PERGENT_TOKEN_BUDGET"].Value)
			}
			if env["PERGENT_SCAN_SOURCE_URL"].Value != p.SourceURL {
				t.Errorf("source env = %q", env["PERGENT_SCAN_SOURCE_URL"].Value)
			}
			if env["OPENCODE_PROVIDER"].Value != "anthropic" || env["OPENCODE_MODEL"].Value != "claude-sonnet-4" {
				t.Errorf("model env = %v %v", env["OPENCODE_PROVIDER"], env["OPENCODE_MODEL"])
			}
			keys, names := secretRefs(ct)
			if strings.Join(keys, ",") != "llm-api-key,result-token" || !names[p.Name+"-secrets"] || len(names) != 1 {
				t.Errorf("secret refs = %v %v", keys, names)
			}
			for n, e := range env {
				if strings.Contains(strings.ToLower(n), "git") || strings.Contains(strings.ToLower(e.Value), "git_token") {
					t.Errorf("env %s looks like a git credential", n)
				}
			}

			// The repo folder is its own empty volume, at exactly /work/repo.
			var repoMounts []string
			for _, m := range ct.VolumeMounts {
				if m.MountPath == "/work/repo" {
					repoMounts = append(repoMounts, m.Name)
				}
			}
			if len(repoMounts) != 1 || repoMounts[0] == "work" {
				t.Errorf("/work/repo mounts = %v", repoMounts)
			}
			var hasIn bool
			for _, v := range spec.Volumes {
				if v.Secret != nil && len(v.Secret.Items) == 1 && v.Secret.Items[0].Key == "prior" {
					hasIn = true
				}
			}
			if hasIn != c.prior {
				t.Errorf("prior volume = %v, want %v", hasIn, c.prior)
			}

			for _, l := range []map[string]string{j.Labels, j.Spec.Template.Labels} {
				if l["app.kubernetes.io/managed-by"] != "pergent-sh" || l[LabelSecurityScan] != testSecurityID {
					t.Errorf("labels = %v", l)
				}
			}
			if *j.Spec.ActiveDeadlineSeconds != 6000 || *spec.TerminationGracePeriodSeconds != 60 {
				t.Errorf("deadline = %d, grace = %d", *j.Spec.ActiveDeadlineSeconds, *spec.TerminationGracePeriodSeconds)
			}
			if *j.Spec.BackoffLimit != 0 || *j.Spec.TTLSecondsAfterFinished != 3600 ||
				*spec.AutomountServiceAccountToken || *spec.EnableServiceLinks {
				t.Errorf("job shape = %+v", j.Spec)
			}
		})
	}
}

func hasArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

// The scan container never runs as root: the image's skill folder is owned by
// root and is safe from the agent only for another user.
func TestSecurityJobNonRoot(t *testing.T) {
	t.Parallel()
	spec := BuildSecurityJob(testSecurityParams()).Spec.Template.Spec
	ps := spec.SecurityContext
	if ps == nil || ps.RunAsNonRoot == nil || !*ps.RunAsNonRoot || ps.RunAsUser == nil || *ps.RunAsUser == 0 {
		t.Errorf("pod security context = %+v", ps)
	}
	cs := spec.Containers[0].SecurityContext
	if cs == nil || cs.RunAsNonRoot == nil || !*cs.RunAsNonRoot || cs.RunAsUser == nil || *cs.RunAsUser == 0 ||
		cs.AllowPrivilegeEscalation == nil || *cs.AllowPrivilegeEscalation {
		t.Errorf("container security context = %+v", cs)
	}
}

// Both folders are limited, and the temporary folder is one of them.
func TestSecurityJobDisk(t *testing.T) {
	t.Parallel()
	p := testSecurityParams()
	spec := BuildSecurityJob(p).Spec.Template.Spec
	limited := map[string]bool{}
	for _, v := range spec.Volumes {
		if v.EmptyDir != nil && v.EmptyDir.SizeLimit != nil && v.EmptyDir.SizeLimit.Cmp(p.WorkSize) == 0 {
			limited[v.Name] = true
		}
	}
	if !limited["work"] || !limited["repo"] {
		t.Errorf("limited volumes = %v", limited)
	}
	env := envByName(spec.Containers[0])
	if env["TMPDIR"].Value != "/work" || env["HOME"].Value != "/work" {
		t.Errorf("TMPDIR = %q, HOME = %q", env["TMPDIR"].Value, env["HOME"].Value)
	}
}

func TestSecurityJobNoProxy(t *testing.T) {
	t.Parallel()
	p := testSecurityParams()
	for _, c := range BuildSecurityJob(p).Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if strings.HasSuffix(strings.ToLower(e.Name), "proxy") {
				t.Errorf("env %s with no proxy", e.Name)
			}
		}
	}

	p.Proxy = "http://proxy.example.com:3128"
	env := envByName(BuildSecurityJob(p).Spec.Template.Spec.Containers[0])
	for _, n := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if env[n].Value != p.Proxy {
			t.Errorf("%s = %q", n, env[n].Value)
		}
	}
	for _, n := range []string{"NO_PROXY", "no_proxy"} {
		if env[n].Value != "pergent-sh.pergent-sh.svc" {
			t.Errorf("%s = %q", n, env[n].Value)
		}
	}
	if env["PERGENT_SCAN_KEEP_PROXY"].Value != "true" {
		t.Errorf("PERGENT_SCAN_KEEP_PROXY = %q", env["PERGENT_SCAN_KEEP_PROXY"].Value)
	}
}

func TestBuildSecuritySecret(t *testing.T) {
	t.Parallel()
	p := testSecurityParams()
	s := BuildSecuritySecret(p, nil, "llm", "tok")
	if string(s.Data["llm-api-key"]) != "llm" || string(s.Data["result-token"]) != "tok" || len(s.Data) != 2 {
		t.Errorf("data keys = %v", s.Data)
	}
	p.Prior = []byte("{}")
	s = BuildSecuritySecret(p, nil, "llm", "tok")
	if string(s.Data["prior"]) != "{}" || len(s.Data) != 3 {
		t.Errorf("data keys = %v", s.Data)
	}
	if _, ok := s.Data["git-token"]; ok {
		t.Error("the Secret has a git token")
	}
}

func TestLaunchSecurityRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*SecurityJobParams)
	}{
		{"short commit", func(p *SecurityJobParams) { p.Commit = "0123abc" }},
		{"not hex", func(p *SecurityJobParams) { p.Commit = strings.Repeat("z", 40) }},
		{"prior too large", func(p *SecurityJobParams) { p.Prior = make([]byte, security.MaxPriorBytes+1) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			l, cs := fakeLauncher()
			p := testSecurityParams()
			c.mutate(&p)
			if err := l.LaunchSecurity(context.Background(), p, "k", "t"); err == nil {
				t.Fatal("no error")
			}
			if n := len(cs.Actions()); n != 0 {
				t.Errorf("a refusal still did %d calls", n)
			}
		})
	}
	l, _ := fakeLauncher()
	if err := l.LaunchSecurity(context.Background(), testSecurityParams(), "k", "t"); err != nil {
		t.Errorf("a good launch failed: %v", err)
	}
}

func TestLaunchSecurityCleansUp(t *testing.T) {
	t.Parallel()
	l, cs := fakeLauncher()
	cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("secret refused")
	})
	if err := l.LaunchSecurity(context.Background(), testSecurityParams(), "k", "t"); err == nil {
		t.Fatal("no error")
	}
	var deleted bool
	for _, a := range cs.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "jobs" {
			deleted = true
		}
	}
	if !deleted {
		t.Error("the Job was not deleted")
	}
}

func TestSecurityFailure(t *testing.T) {
	t.Parallel()
	failedAt := inspectNow.Add(-time.Minute)
	start := inspectNow.Add(-4 * time.Minute)
	failedJob := func(reason string) *batchv1.Job {
		j := BuildSecurityJob(testSecurityParams())
		j.Name = "j"
		j.Status.Conditions = []batchv1.JobCondition{cond(batchv1.JobFailed, reason, failedAt)}
		return j
	}
	evicted := podFor("j", start, nil, nil)
	evicted.Status.Phase = corev1.PodFailed
	evicted.Status.Reason = "Evicted"
	evicted.Status.Message = `Usage of EmptyDir volume "repo" exceeds the limit "4Gi". `

	// The text uses the limit of the volume the message names.
	twoLimits := func() *batchv1.Job {
		j := failedJob("BackoffLimitExceeded")
		for i := range j.Spec.Template.Spec.Volumes {
			if v := &j.Spec.Template.Spec.Volumes[i]; v.Name == "repo" {
				q := resource.MustParse("2Gi")
				v.EmptyDir.SizeLimit = &q
			}
		}
		return j
	}
	evictedWork := podFor("j", start, nil, nil)
	evictedWork.Status.Phase = corev1.PodFailed
	evictedWork.Status.Reason = "Evicted"
	evictedWork.Status.Message = `Usage of EmptyDir volume "work" exceeds the limit "4Gi". `

	cases := []struct {
		name string
		job  *batchv1.Job
		pods []*corev1.Pod
		want string
	}{
		{"evicted for storage", failedJob("BackoffLimitExceeded"), []*corev1.Pod{evicted}, "the repository is too large for the scan's disk (4Gi)"},
		{"repo volume limit", twoLimits(), []*corev1.Pod{evicted}, "the repository is too large for the scan's disk (2Gi)"},
		{"work volume limit", twoLimits(), []*corev1.Pod{evictedWork}, "the repository is too large for the scan's disk (4Gi)"},
		{"OOMKilled", failedJob("BackoffLimitExceeded"),
			[]*corev1.Pod{podFor("j", start, nil, []corev1.ContainerStatus{terminated("pergent", 137, "OOMKilled", start, failedAt)})}, "the scan ran out of memory"},
		{"deadline", failedJob("DeadlineExceeded"), nil, "the scan took longer than its timeout"},
		{"other failure", failedJob("BackoffLimitExceeded"),
			[]*corev1.Pod{podFor("j", start, nil, []corev1.ContainerStatus{terminated("pergent", 1, "Error", start, failedAt)})}, "the scan Job ended without a result"},
		{"gone", nil, nil, "the scan Job ended without a result"},
		{"running", func() *batchv1.Job { j := BuildSecurityJob(testSecurityParams()); j.Name = "j"; return j }(), nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cs := fake.NewSimpleClientset()
			if c.job != nil {
				if _, err := cs.BatchV1().Jobs("reviews").Create(context.Background(), c.job, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			for _, p := range c.pods {
				if _, err := cs.CoreV1().Pods("reviews").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			l := &Launcher{Client: cs, Namespace: "reviews"}
			got, err := l.SecurityFailure(context.Background(), "j")
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("SecurityFailure = %q; want %q", got, c.want)
			}
		})
	}
}
