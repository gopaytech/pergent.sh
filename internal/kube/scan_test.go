package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent.sh/internal/readiness"
)

const testScanID = "7a2c9e41-5b3d-4f60-8e17-3c9d0a4b6f28"

const testScanInput = `{"version":1,"library":[{"name":"go","paths":["**/*.go"],"assigned":true}]}`

func testScanParams() ScanJobParams {
	return ScanJobParams{
		Name:        ScanJobName(testScanID),
		Namespace:   "reviews",
		ScanID:      testScanID,
		GitImage:    "ghcr.io/gopaytech/pergent:v0.1.2",
		Image:       "ghcr.io/gopaytech/pergent.sh:v0.4.0",
		RepoURL:     "https://git.example.com/acme/widgets.git",
		GitUsername: "pergent-bot",
		Branch:      "main",
		Commit:      "0123456789abcdef0123456789abcdef01234567",
		ResultURL:   "http://pergent-sh.pergent-sh.svc:8080/readiness-scans/" + testScanID + "/result",
		Input:       testScanInput,
	}
}

func TestScanJobName(t *testing.T) {
	if got := ScanJobName(testScanID); got != "pergent-scan-7a2c9e415b3d" {
		t.Errorf("ScanJobName = %q", got)
	}
}

// The clone gets the read-only git token and nothing of the scan's; the scan
// container gets the scan token and the input file and nothing of git's. The
// input is a file in the scan container only, never an environment string, and
// no environment names an LLM key or a write token.
func TestBuildScanJobSecrets(t *testing.T) {
	p := testScanParams()
	job := BuildScanJob(p)
	spec := job.Spec.Template.Spec

	if len(spec.InitContainers) != 1 || len(spec.Containers) != 1 {
		t.Fatalf("containers = %d init, %d main", len(spec.InitContainers), len(spec.Containers))
	}
	clone := containerByName(t, spec.InitContainers, "clone")
	scan := containerByName(t, spec.Containers, "scan")
	if clone.Image != p.GitImage || scan.Image != p.Image {
		t.Errorf("images = %s, %s", clone.Image, scan.Image)
	}
	secretName := p.Name + "-secrets"

	// The clone: GIT_TOKEN from git-token, no other secret.
	var cloneSecrets []string
	cloneEnv := map[string]string{}
	for _, e := range clone.Env {
		cloneEnv[e.Name] = e.Value
		if e.ValueFrom != nil {
			ref := e.ValueFrom.SecretKeyRef
			if ref == nil || ref.Name != secretName {
				t.Fatalf("clone env %s = %+v", e.Name, e.ValueFrom)
			}
			cloneSecrets = append(cloneSecrets, e.Name+"="+ref.Key)
		}
	}
	if strings.Join(cloneSecrets, ",") != "GIT_TOKEN=git-token" {
		t.Errorf("clone secret env = %v; want only GIT_TOKEN from git-token", cloneSecrets)
	}
	for name, want := range map[string]string{
		"REPO_URL": p.RepoURL, "SCAN_BRANCH": "main", "SCAN_COMMIT": p.Commit, "GIT_USERNAME": "pergent-bot",
	} {
		if cloneEnv[name] != want {
			t.Errorf("clone env %s = %q; want %q", name, cloneEnv[name], want)
		}
	}
	if len(clone.Command) != 3 || clone.Command[0] != "sh" || clone.Command[1] != "-ec" || clone.Command[2] != scanScript {
		t.Errorf("clone command = %q", clone.Command)
	}

	// The scan container: the scan token from scan-token, no other secret.
	if len(scan.Env) != 1 || scan.Env[0].Name != "PERGENT_SH_SCAN_TOKEN" || scan.Env[0].ValueFrom == nil ||
		scan.Env[0].ValueFrom.SecretKeyRef == nil || scan.Env[0].ValueFrom.SecretKeyRef.Name != secretName ||
		scan.Env[0].ValueFrom.SecretKeyRef.Key != "scan-token" {
		t.Errorf("scan env = %+v; want only the scan token from the Job's Secret", scan.Env)
	}
	if len(scan.Command) != 1 || scan.Command[0] != "/pergent-sh" {
		t.Errorf("scan command = %q", scan.Command)
	}
	wantArgs := "scan-repo --dir=/work/repo --commit-file=/work/commit --error-file=/work/error " +
		"--input-file=/in/scan-input --result-url=" + p.ResultURL
	if got := strings.Join(scan.Args, " "); got != wantArgs {
		t.Errorf("scan args = %q; want %q", got, wantArgs)
	}
	if len(clone.EnvFrom) != 0 || len(scan.EnvFrom) != 0 {
		t.Error("no container may take a whole Secret as env")
	}

	// No environment of either names an LLM key; no token but the two above.
	for _, c := range []corev1.Container{clone, scan} {
		for _, e := range c.Env {
			up := strings.ToUpper(e.Name)
			if strings.Contains(up, "OPENCODE") || strings.Contains(up, "LLM") || strings.Contains(up, "API_KEY") {
				t.Errorf("%s env %s names an LLM key", c.Name, e.Name)
			}
			if strings.Contains(e.Value, testScanInput) {
				t.Errorf("%s env %s holds the scan input", c.Name, e.Name)
			}
		}
	}

	// The input: a Secret volume of one item, mounted read-only in the scan
	// container only.
	var inputVol *corev1.Volume
	for i, v := range spec.Volumes {
		if v.Secret != nil {
			if inputVol != nil {
				t.Fatal("more than one Secret volume")
			}
			inputVol = &spec.Volumes[i]
		}
		if v.Projected != nil || v.ConfigMap != nil {
			t.Errorf("volume %s is a projection or config map", v.Name)
		}
	}
	if inputVol == nil {
		t.Fatal("no Secret volume holds the scan input")
	}
	if inputVol.Secret.SecretName != secretName || len(inputVol.Secret.Items) != 1 ||
		inputVol.Secret.Items[0].Key != "scan-input" || inputVol.Secret.Items[0].Path != "scan-input" {
		t.Errorf("input volume = %+v; want only the scan-input key", inputVol.Secret)
	}
	if m := inputVol.Secret.DefaultMode; m == nil || *m != 0o444 {
		t.Errorf("input volume mode = %v; want 0444 for the non-root user", m)
	}
	var mounts []string
	for _, c := range []corev1.Container{clone, scan} {
		for _, m := range c.VolumeMounts {
			if m.Name == inputVol.Name {
				if !m.ReadOnly || m.MountPath != "/in" {
					t.Errorf("%s mounts the input as %+v", c.Name, m)
				}
				mounts = append(mounts, c.Name)
			}
		}
	}
	if strings.Join(mounts, ",") != "scan" {
		t.Errorf("the input is mounted in %v; want only the scan container", mounts)
	}
	// The clone writes /work; the scan container only reads it.
	for _, m := range scan.VolumeMounts {
		if m.MountPath == "/work" && !m.ReadOnly {
			t.Error("the scan container must mount /work read-only")
		}
	}
	for _, m := range clone.VolumeMounts {
		if m.MountPath == "/work" && m.ReadOnly {
			t.Error("the clone must write /work")
		}
	}

	// The credential is never in a command or argument, nor is the URL.
	for _, c := range []corev1.Container{clone, scan} {
		for _, s := range append(append([]string{}, c.Command...), c.Args...) {
			if strings.Contains(s, "git-token") || strings.Contains(s, testScanInput) {
				t.Errorf("%s command or args hold a secret: %q", c.Name, s)
			}
		}
	}
	if strings.Contains(p.RepoURL, "@") {
		t.Fatal("test params: RepoURL carries credentials")
	}
}

func TestBuildScanJobLockedDown(t *testing.T) {
	p := testScanParams()
	job := BuildScanJob(p)
	if job.Name != p.Name || job.Namespace != "reviews" {
		t.Errorf("job = %s/%s", job.Namespace, job.Name)
	}
	for what, l := range map[string]map[string]string{"job": job.Labels, "pod": job.Spec.Template.Labels} {
		if l[labelManagedBy] != managedBy {
			t.Errorf("%s labels = %v; want %s=%s", what, l, labelManagedBy, managedBy)
		}
		if l["pergent.sh/readiness-scan"] != testScanID {
			t.Errorf("%s labels = %v; want the scan ID", what, l)
		}
	}
	spec := job.Spec.Template.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the pod must not mount a service account token")
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Error("the pod must not get service links")
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", spec.RestartPolicy)
	}
	sc := spec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != 65532 ||
		sc.RunAsGroup == nil || *sc.RunAsGroup != 65532 {
		t.Errorf("pod security context = %+v; want non-root 65532", sc)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 300 {
		t.Errorf("activeDeadlineSeconds = %v; want 300", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v; want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("ttlSecondsAfterFinished = %v; want 3600", job.Spec.TTLSecondsAfterFinished)
	}

	var work *corev1.Volume
	for i, v := range spec.Volumes {
		if v.Name == "work" {
			work = &spec.Volumes[i]
		}
	}
	if work == nil || work.EmptyDir == nil || work.EmptyDir.SizeLimit == nil || work.EmptyDir.SizeLimit.String() != "2Gi" {
		t.Errorf("work volume = %+v; want an emptyDir of 2Gi", work)
	}
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		sc := c.SecurityContext
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
			sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s is not locked down: %+v", c.Name, sc)
		}
		if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s must have a read-only root filesystem", c.Name)
		}
		if got := c.Resources.Requests.Cpu().String(); got != "100m" {
			t.Errorf("%s cpu request = %s; want 100m", c.Name, got)
		}
		if got := c.Resources.Requests.Memory().String(); got != "128Mi" {
			t.Errorf("%s memory request = %s; want 128Mi", c.Name, got)
		}
		if got := c.Resources.Limits.Memory().String(); got != "512Mi" {
			t.Errorf("%s memory limit = %s; want 512Mi", c.Name, got)
		}
	}
}

// The scan script's isolation: a token only in the auth header (skipped when
// empty), no hooks, no config, no prompt, no symlinks, no push, one ref.
func TestScanScript(t *testing.T) {
	for _, want := range []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"HOME=/work",
		"core.hooksPath /dev/null",
		"core.symlinks false",
		`git remote add origin -- "$REPO_URL"`,
		"pergent-push-disabled:",
		"--depth=1",
		`"$SCAN_COMMIT"`,
		`refs/heads/$SCAN_BRANCH`,
		"the default branch moved during the scan; scan again",
		"could not fetch the default branch",
		"/work/commit",
		"/work/error",
		"http.extraHeader",
		"GIT_USERNAME:-",
	} {
		if !strings.Contains(scanScript, want) {
			t.Errorf("scan.sh lacks %q", want)
		}
	}
	if strings.Contains(scanScript, "eval") {
		t.Error("scan.sh must not eval anything")
	}
}

// The Secret holds the git token, the scan token and the scan input, owned by
// the Job.
func TestBuildScanSecret(t *testing.T) {
	p := testScanParams()
	owner := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: p.Name, UID: "uid-1"}}
	sec := BuildScanSecret(p, owner, "git-tok", "scan-tok")
	if sec.Name != p.Name+"-secrets" || sec.Namespace != "reviews" || sec.Labels[labelManagedBy] != managedBy {
		t.Errorf("secret meta = %+v", sec.ObjectMeta)
	}
	if len(sec.Data) != 3 || string(sec.Data["git-token"]) != "git-tok" || string(sec.Data["scan-token"]) != "scan-tok" ||
		string(sec.Data["scan-input"]) != testScanInput {
		t.Errorf("secret data keys = %d; want git-token, scan-token and scan-input", len(sec.Data))
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != "uid-1" {
		t.Errorf("owner = %+v", sec.OwnerReferences)
	}
}

// LaunchScan creates the Job suspended, then its Secret, then starts it.
func TestLaunchScan(t *testing.T) {
	l, cs := fakeLauncher()
	p := testScanParams()
	p.Namespace = "ignored"
	if err := l.LaunchScan(context.Background(), p, "git-tok", "scan-tok"); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "patch" {
			order = append(order, a.GetVerb()+" "+a.GetResource().Resource+" "+a.GetNamespace())
		}
		if c, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "jobs" {
			if j := c.GetObject().(*batchv1.Job); j.Spec.Suspend == nil || !*j.Spec.Suspend {
				t.Error("the Job must be created suspended")
			}
		}
	}
	if got := strings.Join(order, ", "); got != "create jobs reviews, create secrets reviews, patch jobs reviews" {
		t.Errorf("actions = %s", got)
	}
}

// A Secret that can't be created takes the Job with it.
func TestLaunchScanCleansUp(t *testing.T) {
	l, cs := fakeLauncher()
	cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota")
	})
	p := testScanParams()
	err := l.LaunchScan(context.Background(), p, "git-tok", "scan-tok")
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("LaunchScan = %v; want the Secret's error", err)
	}
	if _, err := cs.BatchV1().Jobs("reviews").Get(context.Background(), p.Name, metav1.GetOptions{}); err == nil {
		t.Error("the Job is left after its Secret failed")
	}
}

// An input over readiness.MaxInputBytes is refused before anything is created,
// with the text the dashboard shows; one at the cap goes.
func TestLaunchScanRefusesLargeInput(t *testing.T) {
	l, cs := fakeLauncher()
	p := testScanParams()
	p.Input = strings.Repeat("x", readiness.MaxInputBytes+1)
	err := l.LaunchScan(context.Background(), p, "git-tok", "scan-tok")
	if err == nil || err.Error() != "the skill library's paths are too large to send to the scan" {
		t.Fatalf("LaunchScan = %v; want the too-large refusal", err)
	}
	if !errors.Is(err, ErrScanInputTooLarge) {
		t.Errorf("err = %v; want ErrScanInputTooLarge", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" {
			t.Errorf("a refused input still did %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}

	p.Input = strings.Repeat("x", readiness.MaxInputBytes)
	if err := l.LaunchScan(context.Background(), p, "git-tok", "scan-tok"); err != nil {
		t.Errorf("an input at the cap: %v", err)
	}
}

// A repository URL with credentials in it is refused: the token reaches git
// only through the auth header.
func TestLaunchScanRefusesCredentialURL(t *testing.T) {
	l, cs := fakeLauncher()
	p := testScanParams()
	p.RepoURL = "https://oauth2:glpat-secret@git.example.com/acme/widgets.git"
	err := l.LaunchScan(context.Background(), p, "git-tok", "scan-tok")
	if err == nil {
		t.Fatal("LaunchScan = nil; want a refusal")
	}
	if strings.Contains(err.Error(), "glpat-secret") {
		t.Errorf("the error repeats the credential: %v", err)
	}
	if len(cs.Actions()) != 0 {
		t.Errorf("a refused URL still did %d calls", len(cs.Actions()))
	}
}

func TestScanFailureReasons(t *testing.T) {
	failedAt := inspectNow.Add(-time.Minute)
	cloneStart := inspectNow.Add(-4 * time.Minute)
	cloneEnd := cloneStart.Add(10 * time.Second)
	okInit := terminated("clone", 0, "Completed", cloneStart, cloneEnd)
	failedJob := func(reason, message string) *batchv1.Job {
		j := jobWith("j", cond(batchv1.JobFailed, reason, failedAt))
		j.Status.Conditions[0].Message = message
		return j
	}
	evicted := func() *corev1.Pod {
		p := podFor("j", cloneStart, nil, nil)
		p.Status.Phase = corev1.PodFailed
		p.Status.Reason = "Evicted"
		p.Status.Message = "Pod ephemeral local storage usage exceeds the total limit of containers 2Gi. "
		return p
	}

	const (
		tooLarge = "the repository is too large to clone (the scan has 2 GiB of disk)"
		oom      = "the scan ran out of memory"
		deadline = "the scan took longer than 5 minutes"
		other    = "the scan Job ended without a result"
	)
	cases := []struct {
		name string
		job  *batchv1.Job
		pods []*corev1.Pod
		want string
	}{
		{"missing Job", nil, nil, other},
		{"running", jobWith("j"), []*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, nil)}, ""},
		{"completed", jobWith("j", cond(batchv1.JobComplete, "", failedAt)),
			[]*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("scan", 0, "Completed", cloneEnd, failedAt)})}, ""},
		{"evicted pod", failedJob("BackoffLimitExceeded", ""), []*corev1.Pod{evicted()}, tooLarge},
		{"ephemeral-storage in the Job message", failedJob("BackoffLimitExceeded", "container clone exceeded ephemeral-storage limit"),
			[]*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{terminated("clone", 1, "Error", cloneStart, cloneEnd)}, nil)}, tooLarge},
		{"ephemeral-storage in a container message", failedJob("BackoffLimitExceeded", ""),
			[]*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{{Name: "clone", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 128, Reason: "Error", Message: "fatal: write error: no space left (ephemeral-storage)",
			}}}}, nil)}, tooLarge},
		{"OOMKilled clone", failedJob("BackoffLimitExceeded", ""),
			[]*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{terminated("clone", 137, "OOMKilled", cloneStart, cloneEnd)}, nil)}, oom},
		{"OOMKilled scan", failedJob("BackoffLimitExceeded", ""),
			[]*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("scan", 137, "OOMKilled", cloneEnd, failedAt)})}, oom},
		{"deadline", failedJob("DeadlineExceeded", ""),
			[]*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("scan", 143, "Error", cloneEnd, failedAt)})}, deadline},
		{"deadline with no pods left", failedJob("DeadlineExceeded", ""), nil, deadline},
		{"evicted beats deadline", failedJob("DeadlineExceeded", ""), []*corev1.Pod{evicted()}, tooLarge},
		{"other failure", failedJob("BackoffLimitExceeded", ""),
			[]*corev1.Pod{podFor("j", cloneStart, []corev1.ContainerStatus{okInit}, []corev1.ContainerStatus{terminated("scan", 1, "Error", cloneEnd, failedAt)})}, other},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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
			got, err := l.ScanFailure(context.Background(), "j")
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("ScanFailure = %q; want %q", got, c.want)
			}
		})
	}
}

// An API error reading the Job is returned, not turned into a reason.
func TestScanFailureAPIError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	l := &Launcher{Client: cs, Namespace: "reviews"}
	if got, err := l.ScanFailure(context.Background(), "j"); err == nil || got != "" {
		t.Errorf("ScanFailure = %q, %v; want an error", got, err)
	}
}
