package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The readiness scan in the engine: queued on switch-on, after the upgrade
// and on request; claimed 2 at a time, the pod part stored first, then the
// Job; its result read as untrusted text and turned into Needs attention
// items; rechecked with no new Job when the skills change.

const testScanImage = "ghcr.io/gopaytech/pergent.sh:v0.0.0-test"

// rubySkill has paths that match no file of the scans below; goSkill's match.
const (
	rubySkill = "---\nname: ruby\npaths: [\"**/*.rb\"]\n---\nReview Ruby.\n"
	goSkill   = "---\nname: go\npaths: [\"**/*.go\"]\n---\nReview Go.\n"
)

// newScanEnv is a test environment with scans on (the dashboard is) and
// acme/widgets (project 7) serving its settings at sha('c').
func newScanEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	env.Engine.Scans = true
	env.Engine.ScanImage = testScanImage
	serveGitLabSettings(env)
	return env
}

// serveScanRepo stores project id at path, with a deploy token, and serves
// its default branch main at commit; its other settings are not served, so
// they read as couldn't-check.
func serveScanRepo(env *testEnv, id int64, path, commit string) {
	env.t.Helper()
	env.SeedProject(func(p *store.Project) {
		p.ProjectID, p.Path, p.HTTPURLToRepo = id, path, "https://gitlab.example.com/"+path+".git"
	})
	pid := "/projects/" + strconv.FormatInt(id, 10)
	env.GitLab.Reply(http.MethodGet, pid, http.StatusOK, map[string]any{
		"id": id, "path_with_namespace": path, "default_branch": "main",
	})
	env.GitLab.Reply(http.MethodGet, pid+"/repository/commits/main", http.StatusOK, map[string]any{"id": commit})
}

func queueScan(t *testing.T, env *testEnv, instance, project, by string) store.ReadinessScan {
	t.Helper()
	sc, out, err := env.Store.QueueReadinessScan(env.ctx, instance, project, by, env.Now(), 0)
	if err != nil || out != store.ScanQueuedNow {
		t.Fatalf("QueueReadinessScan(%s) = %v, %v", project, out, err)
	}
	return sc
}

func scanOf(t *testing.T, env *testEnv, id string) store.ReadinessScan {
	t.Helper()
	sc, err := env.Store.ReadinessScan(env.ctx, id)
	if err != nil {
		t.Fatalf("ReadinessScan(%s): %v", id, err)
	}
	return sc
}

// allScans returns every scan row, oldest queued first.
func allScans(t *testing.T, env *testEnv) []store.ReadinessScan {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv(testDBEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.schema
	conn, err := pgx.ConnectConfig(env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(env.ctx)
	rows, err := conn.Query(env.ctx, `SELECT id::text FROM readiness_scans ORDER BY queued_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	out := make([]store.ReadinessScan, 0, len(ids))
	for _, id := range ids {
		out = append(out, scanOf(t, env, id))
	}
	return out
}

// scanJobs returns the scan Jobs in the namespace.
func scanJobs(t *testing.T, env *testEnv) []batchv1.Job {
	t.Helper()
	l, err := env.Kube.BatchV1().Jobs(testNamespace).List(env.ctx, metav1.ListOptions{LabelSelector: "pergent.sh/readiness-scan"})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// scanToken is the one-time token in the Secret of the scan's Job.
func scanToken(t *testing.T, env *testEnv, id string) string {
	t.Helper()
	s, err := env.Kube.CoreV1().Secrets(testNamespace).Get(env.ctx, kube.ScanJobName(id)+"-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("scan Secret of %s: %v", id, err)
	}
	return string(s.Data["scan-token"])
}

func body(t *testing.T, r readiness.JobResult) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// cleanResult is a scan of commit with project context, the go skill's files
// and a small size.
func cleanResult(commit string) readiness.JobResult {
	return readiness.JobResult{
		Version: readiness.ResultVersion, Commit: commit, Files: 10, SourceFiles: 8,
		Context: readiness.Context{AgentFile: "AGENTS.md", Readme: "README.md"},
		Skills: []readiness.SkillFit{
			{Name: "go", Folder: inputs.SourceLibrary, HasPaths: true, Files: 8},
			{Name: "ruby", Folder: inputs.SourceLibrary, HasPaths: true},
			{Name: "security", Folder: inputs.SourceLibrary},
		},
		InstructionsBytes: 100,
	}
}

// gapResult is a scan of commit with no project context, files only the go
// skill matches, and instructions over 900 KiB.
func gapResult(commit string) readiness.JobResult {
	r := cleanResult(commit)
	r.Context = readiness.Context{}
	r.InstructionsBytes = 950 << 10
	return r
}

// scanWith runs one scan of instance's project to the end: queued, launched,
// and r posted with its token.
func scanWith(t *testing.T, env *testEnv, instance, project string, r readiness.JobResult) store.ReadinessScan {
	t.Helper()
	sc := queueScan(t, env, instance, project, "alice")
	env.Engine.launchScans(env.ctx)
	if got := scanOf(t, env, sc.ID); got.JobName == "" {
		t.Fatalf("scan of %s did not launch: %+v", project, got)
	}
	out, err := env.Engine.CompleteScan(env.ctx, sc.ID, scanToken(t, env, sc.ID), body(t, r))
	if err != nil || out != readiness.ScanStored {
		t.Fatalf("CompleteScan = %v, %v", out, err)
	}
	return scanOf(t, env, sc.ID)
}

// items returns the Needs attention items of project, kind to message.
func items(t *testing.T, env *testEnv, project string) map[string]string {
	t.Helper()
	all, err := env.Store.Attention(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, a := range all {
		if strings.EqualFold(a.Project, project) {
			out[a.Kind] = a.Message
		}
	}
	return out
}

// useLibrary makes the library go, ruby and security, with the default list
// names.
func useLibrary(env *testEnv, defaults ...string) {
	env.AddSkill("go", goSkill)
	env.AddSkill("ruby", rubySkill)
	env.SetDefaultSkills(defaults...)
}

// A repository a reconcile pass sets up and that has never been scanned
// gets one scan, by switch-on; the next pass queues none.
func TestSwitchOnQueuesOneScan(t *testing.T) {
	env := newScanEnv(t)
	serveProject(env, testProjectID, testProjectPath)

	reconcile(t, env)
	scans := allScans(t, env)
	if len(scans) != 1 || scans[0].StartedBy != store.StartedBySwitchOn || scans[0].State != store.ScanQueued ||
		!strings.EqualFold(scans[0].Project, testProjectPath) {
		t.Fatalf("scans after the first reconcile = %+v; want one queued by switch-on", scans)
	}
	// Finished, it is still the repository's scan: no other is queued.
	if _, ok, err := env.Store.ClaimReadinessScan(env.ctx, 1, env.Now(), env.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if ok, err := env.Store.FinishReadinessScan(env.ctx, scans[0].ID, nil, nil, "stopped by the test", env.Now()); err != nil || !ok {
		t.Fatalf("finish = %v, %v", ok, err)
	}
	reconcile(t, env)
	if again := allScans(t, env); len(again) != 1 || again[0].ID != scans[0].ID {
		t.Fatalf("scans after the second reconcile = %+v; want the first alone", again)
	}
}

// Without the dashboard, nothing queues a scan: not reconcile, not the
// upgrade sweep, not a request.
func TestNoScansWithoutDashboard(t *testing.T) {
	env := newTestEnv(t)
	serveGitLabSettings(env)
	serveProject(env, testProjectID, testProjectPath)
	reconcile(t, env)

	ctx, cancel := context.WithTimeout(env.ctx, 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { env.Engine.RunScans(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("RunScans kept running with scans off")
	}
	if _, err := env.Engine.RequestScan(env.ctx, testInstance, testProjectPath, "alice"); err == nil {
		t.Error("RequestScan worked with scans off")
	}
	if scans := allScans(t, env); len(scans) != 0 {
		t.Fatalf("scans = %+v; want none", scans)
	}
}

// After the upgrade, every repository switched on that was never scanned is
// queued once by "upgrade", oldest first; a restart queues none.
func TestStartupScansUnscanned(t *testing.T) {
	env := newScanEnv(t)
	// No Job is wanted here: a claimed scan fails at once.
	env.Engine.ScanImage = ""
	serveScanRepo(env, 9, "acme/gadgets", sha('d'))
	serveScanRepo(env, 10, "acme/tools", sha('e'))
	serveScanRepo(env, 11, "acme/scanned", sha('f'))
	serveScanRepo(env, 12, "acme/switched-off", sha('f'))
	// acme/tools was switched on before acme/gadgets.
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", CreatedBy: "alice"})
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/tools", CreatedBy: "alice"})
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/scanned", CreatedBy: "alice"})
	execSQL(t, env, `UPDATE dashboard_repos SET created_at = now() - interval '1 day' WHERE project = 'acme/tools'`)
	execSQL(t, env, `UPDATE dashboard_repos SET created_at = now() - interval '1 hour' WHERE project = 'acme/gadgets'`)
	scanned := queueScan(t, env, testInstance, "acme/scanned", "alice")

	runScansUntil(t, env, func() bool { return len(allScans(t, env)) >= 4 })

	scans := allScans(t, env)
	var upgraded []string
	for _, sc := range scans {
		if sc.ID == scanned.ID {
			continue
		}
		if sc.StartedBy != store.StartedByUpgrade {
			t.Errorf("scan of %s started by %q; want upgrade", sc.Project, sc.StartedBy)
		}
		upgraded = append(upgraded, sc.Project)
	}
	// The config repository first, then the dashboard's oldest first; the
	// switched-off one (a projects row and no dashboard row) not at all.
	want := []string{testProjectPath, "acme/tools", "acme/gadgets"}
	if strings.Join(upgraded, ",") != strings.Join(want, ",") {
		t.Fatalf("upgrade scans in claim order = %v; want %v", upgraded, want)
	}

	// A restart: a new engine on the same store.
	restarted := &Engine{Store: env.Store, Config: env.Engine.Config, Instances: env.Engine.Instances,
		Platforms: env.Engine.Platforms, WebhookSecrets: env.Engine.WebhookSecrets, Launcher: env.Engine.Launcher,
		Box: env.Box, Now: env.Now, Log: env.Engine.Log, Scans: true}
	restarted.upgradeScans(env.ctx)
	if again := allScans(t, env); len(again) != len(scans) {
		t.Fatalf("a restart queued scans: %d, was %d", len(again), len(scans))
	}
}

// At most 2 scan Jobs run at once, whatever reviews are running: a full
// jobs.max_running holds no scan back.
func TestScanCapIsTwo(t *testing.T) {
	env := newScanEnv(t)
	serveScanRepo(env, 9, "acme/gadgets", sha('d'))
	serveScanRepo(env, 10, "acme/tools", sha('e'))
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", CreatedBy: "alice"})
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/tools", CreatedBy: "alice"})
	env.Engine.Config.Jobs.MaxRunning = 1
	env.Dispatched(1, sha('1'))

	queueScan(t, env, testInstance, testProjectPath, "alice")
	queueScan(t, env, testInstance, "acme/gadgets", "alice")
	queueScan(t, env, testInstance, "acme/tools", "alice")
	env.Engine.launchScans(env.ctx)
	if n := len(scanJobs(t, env)); n != 2 {
		t.Fatalf("%d scan Jobs; want 2", n)
	}
	env.Engine.launchScans(env.ctx)
	if n := len(scanJobs(t, env)); n != 2 {
		t.Fatalf("%d scan Jobs after another pass; want still 2", n)
	}
}

// A scan needs no LLM: with none set, its Job starts, and nothing in it
// names an LLM key.
func TestScanNeedsNoLLM(t *testing.T) {
	env := newScanEnv(t)
	t.Setenv("OPENCODE_API_KEY", "")
	env.Engine.Config.LLM = config.LLM{}

	sc := queueScan(t, env, testInstance, testProjectPath, "alice")
	env.Engine.launchScans(env.ctx)
	jobs := scanJobs(t, env)
	if len(jobs) != 1 {
		t.Fatalf("%d scan Jobs; want 1 (scan %+v)", len(jobs), scanOf(t, env, sc.ID))
	}
	spec := jobs[0].Spec.Template.Spec
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		for _, ev := range c.Env {
			if strings.Contains(strings.ToUpper(ev.Name), "LLM") || strings.Contains(ev.Name, "OPENCODE") ||
				strings.Contains(strings.ToUpper(ev.Name), "API_KEY") {
				t.Errorf("container %s has env %s", c.Name, ev.Name)
			}
		}
	}
	secret, err := env.Kube.CoreV1().Secrets(testNamespace).Get(env.ctx, kube.ScanJobName(sc.ID)+"-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range secret.Data {
		if string(v) == testLLMKey {
			t.Errorf("the Secret holds the LLM key under %s", k)
		}
	}
	if string(secret.Data["git-token"]) != testDeployToken {
		t.Errorf("the clone token is not the deploy token")
	}
	var in readiness.JobInput
	if err := json.Unmarshal(secret.Data["scan-input"], &in); err != nil {
		t.Fatalf("scan input: %v", err)
	}
	if in.Version != 1 || len(in.Library) != 1 || in.Library[0].Name != "security" || !in.Library[0].Assigned {
		t.Errorf("scan input = %+v; want the security skill, assigned", in)
	}
}

// The pod part is stored before the Job is created, with the Job's name and
// token (R1), and a Job that can't be created fails the scan with the
// settings kept.
func TestScanPodPartStoredFirst(t *testing.T) {
	env := newScanEnv(t)
	sc := queueScan(t, env, testInstance, testProjectPath, "alice")
	var (
		atCreate      store.ReadinessScan
		tokenAtCreate bool
	)
	creates := 0
	env.Kube.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if creates++; creates > 1 {
			t.Error("a scan Job was created twice")
			return true, nil, errors.New("the API server said no")
		}
		atCreate = scanOf(t, env, sc.ID)
		tokenAtCreate = querySQL(t, env, `SELECT token_hash IS NOT NULL FROM readiness_scans WHERE id = '`+sc.ID+`'`)
		return true, nil, errors.New("the API server said no")
	})
	env.Engine.launchScans(env.ctx)

	if atCreate.Pod == nil || atCreate.Commit != sha('c') || atCreate.JobName != kube.ScanJobName(sc.ID) ||
		atCreate.Deadline.IsZero() || !tokenAtCreate {
		t.Fatalf("scan when the Job was created = %+v, token stored %v; want the pod part, commit, Job name and token stored",
			atCreate, tokenAtCreate)
	}
	got := scanOf(t, env, sc.ID)
	if got.State != store.ScanFailed || got.Error != "couldn't start the scan Job; see the pergent.sh log" {
		t.Fatalf("scan = %s %q; want failed with the start reason", got.State, got.Error)
	}
	var pod readiness.PodResult
	if err := json.Unmarshal(got.Pod, &pod); err != nil || pod.Approvals.State != readiness.Passed ||
		pod.Protected.State != readiness.Passed || pod.RequiredChecks.State != readiness.Passed {
		t.Fatalf("pod = %s (%v); want the settings kept", got.Pod, err)
	}
	if !strings.Contains(string(got.Pod), `"requiredChecks"`) {
		t.Errorf("pod JSON %s does not use lowerCamel names", got.Pod)
	}
}

// A scan Job evicted for its disk fails the scan as too large to clone, at
// the next sweep; the settings stay.
func TestScanEvictedKeepsSettings(t *testing.T) {
	env := newScanEnv(t)
	sc := queueScan(t, env, testInstance, testProjectPath, "alice")
	env.Engine.launchScans(env.ctx)
	name := kube.ScanJobName(sc.ID)
	job, err := env.Kube.BatchV1().Jobs(testNamespace).Get(env.ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	if _, err := env.Kube.BatchV1().Jobs(testNamespace).UpdateStatus(env.ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-x", Namespace: testNamespace, Labels: map[string]string{"job-name": name}},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"},
	}
	if _, err := env.Kube.CoreV1().Pods(testNamespace).Create(env.ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	env.Engine.sweepScans(env.ctx)
	got := scanOf(t, env, sc.ID)
	if got.State != store.ScanFailed || got.Error != "the repository is too large to clone (the scan has 2 GiB of disk)" {
		t.Fatalf("scan = %s %q; want failed as too large", got.State, got.Error)
	}
	if got.Pod == nil || got.Commit != sha('c') {
		t.Errorf("scan = %+v; want the pod part kept", got)
	}
}

// A sweep fails a running scan past its deadline, and one whose pod part
// never got to its Job (a restart).
func TestScanSweepDeadline(t *testing.T) {
	env := newScanEnv(t)
	launched := queueScan(t, env, testInstance, testProjectPath, "alice")
	env.Engine.launchScans(env.ctx)

	serveScanRepo(env, 9, "acme/gadgets", sha('d'))
	stranded := queueScan(t, env, testInstance, "acme/gadgets", "alice")
	if _, ok, err := env.Store.ClaimReadinessScan(env.ctx, 5, env.Now(), env.Now().Add(scanPodBudget+scanGrace)); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}

	env.Engine.sweepScans(env.ctx)
	if got := scanOf(t, env, launched.ID); got.State != store.ScanRunning {
		t.Fatalf("scan within its deadline = %s %q; want running", got.State, got.Error)
	}
	env.SetNow(env.Now().Add(scanJobTimeout + scanGrace + time.Second))
	env.Engine.sweepScans(env.ctx)
	if got := scanOf(t, env, launched.ID); got.State != store.ScanFailed || got.Error != "the scan took longer than 5 minutes" {
		t.Errorf("scan past its deadline = %s %q", got.State, got.Error)
	}
	if n := len(scanJobs(t, env)); n != 0 {
		t.Errorf("%d scan Jobs left after the deadline", n)
	}
	if got := scanOf(t, env, stranded.ID); got.State != store.ScanFailed || got.Error != msgScanLost {
		t.Errorf("stranded scan = %s %q", got.State, got.Error)
	}
}

// A result is taken once, with its token; the wrong token takes nothing; a
// result for another commit fails the scan as unusable.
func TestCompleteScanOneTime(t *testing.T) {
	env := newScanEnv(t)
	useLibrary(env, "go")
	sc := queueScan(t, env, testInstance, testProjectPath, "alice")
	env.Engine.launchScans(env.ctx)
	token := scanToken(t, env, sc.ID)

	if out, err := env.Engine.CompleteScan(env.ctx, sc.ID, "not-the-token", body(t, cleanResult(sha('c')))); err != nil || out != readiness.ScanUnauthorized {
		t.Fatalf("wrong token = %v, %v; want unauthorized", out, err)
	}
	if out, err := env.Engine.CompleteScan(env.ctx, sc.ID, token, body(t, cleanResult(sha('c')))); err != nil || out != readiness.ScanStored {
		t.Fatalf("right token = %v, %v; want stored", out, err)
	}
	got := scanOf(t, env, sc.ID)
	if got.State != store.ScanDone || got.Job == nil {
		t.Fatalf("scan = %s %q, job %s; want done", got.State, got.Error, got.Job)
	}
	if !strings.Contains(string(got.Job), `"sourceFiles"`) {
		t.Errorf("job result %s is not lowerCamel JSON", got.Job)
	}
	if out, err := env.Engine.CompleteScan(env.ctx, sc.ID, token, body(t, cleanResult(sha('c')))); err != nil || out != readiness.ScanConflict {
		t.Fatalf("again = %v, %v; want conflict", out, err)
	}
	if out, err := env.Engine.CompleteScan(env.ctx, "not-a-scan", token, nil); err != nil || out != readiness.ScanUnauthorized {
		t.Fatalf("unknown scan = %v, %v; want unauthorized", out, err)
	}

	env.SetNow(env.Now().Add(time.Minute))
	other := queueScan(t, env, testInstance, testProjectPath, "alice")
	env.Engine.launchScans(env.ctx)
	if out, err := env.Engine.CompleteScan(env.ctx, other.ID, scanToken(t, env, other.ID), body(t, cleanResult(sha('9')))); err != nil || out != readiness.ScanStored {
		t.Fatalf("other commit = %v, %v; want stored (as failed)", out, err)
	}
	if got := scanOf(t, env, other.ID); got.State != store.ScanFailed || got.Error != "the scan Job sent a result pergent.sh couldn't use" {
		t.Fatalf("scan of another commit = %s %q", got.State, got.Error)
	}

	env.SetNow(env.Now().Add(time.Minute))
	big := queueScan(t, env, testInstance, testProjectPath, "alice")
	env.Engine.launchScans(env.ctx)
	if err := env.Engine.ScanTooLarge(env.ctx, big.ID, scanToken(t, env, big.ID)); err != nil {
		t.Fatal(err)
	}
	if got := scanOf(t, env, big.ID); got.State != store.ScanFailed || got.Error != "the scan Job sent a result over 1 MiB" {
		t.Fatalf("scan with a body over 1 MiB = %s %q", got.State, got.Error)
	}
}

// What a Job posts is untrusted: text is one line, held secrets are not
// kept, long paths are dropped (and counted) and lists are capped.
func TestCompleteScanUntrustedText(t *testing.T) {
	t.Setenv("GITLAB_REVIEW_TOKEN", testReviewToken)
	env := newScanEnv(t)
	useLibrary(env, "go")
	r := cleanResult(sha('c'))
	r.Noise = []readiness.NoiseFile{
		{Path: "gen/a.pb.go", Why: readiness.WhyGeneratedName, Size: 10, Skills: []string{"go"}},
		{Path: strings.Repeat("x", readiness.MaxPathBytes+1), Why: readiness.WhyLarge, Size: 1 << 20},
		{Path: "bad\x1bpath.go", Why: readiness.WhyLarge, Size: 1 << 20},
		{Path: "leak-" + testDeployToken + ".go", Why: readiness.WhyLarge, Size: 1 << 20},
		{Path: "odd.go", Why: "something else", Size: 1},
	}
	r.NoiseTotal = 5
	r.Skills = append(r.Skills, readiness.SkillFit{Name: "broken", Folder: inputs.RepoFolders[0], Problem: "bad\nfront matter " + testReviewToken})
	for i := range maxResultLanguages + 5 {
		r.Languages = append(r.Languages, readiness.Language{Name: "Lang" + strconv.Itoa(i), Files: 1})
	}
	r.InstructionsBytes = -5
	sc := queueScan(t, env, testInstance, testProjectPath, "alice")
	env.Engine.launchScans(env.ctx)
	if out, err := env.Engine.CompleteScan(env.ctx, sc.ID, scanToken(t, env, sc.ID), body(t, r)); err != nil || out != readiness.ScanStored {
		t.Fatalf("CompleteScan = %v, %v", out, err)
	}
	got := scanOf(t, env, sc.ID)
	if got.State != store.ScanDone {
		t.Fatalf("scan = %s %q", got.State, got.Error)
	}
	for _, s := range []string{testDeployToken, testReviewToken, "\\u001b", "\\n"} {
		if strings.Contains(string(got.Job), s) {
			t.Errorf("job result holds %q: %s", s, got.Job)
		}
	}
	var stored readiness.JobResult
	if err := json.Unmarshal(got.Job, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Noise) != 1 || stored.Noise[0].Path != "gen/a.pb.go" || stored.NoiseTotal != 5 {
		t.Errorf("noise = %+v, total %d; want the one good path, 5 counted", stored.Noise, stored.NoiseTotal)
	}
	if len(stored.Languages) != maxResultLanguages {
		t.Errorf("%d languages kept; want %d", len(stored.Languages), maxResultLanguages)
	}
	if stored.InstructionsBytes != 0 {
		t.Errorf("instructions bytes = %d; want a negative size dropped", stored.InstructionsBytes)
	}
	for _, f := range stored.Skills {
		if f.Name == "broken" && f.Problem != msgSkillProblemWithheld {
			t.Errorf("problem = %q; want it withheld", f.Problem)
		}
	}

	// The Job's own error text is one line, and a secret in it is not kept.
	env.SetNow(env.Now().Add(time.Minute))
	failed := queueScan(t, env, testInstance, testProjectPath, "alice")
	env.Engine.launchScans(env.ctx)
	res := readiness.JobResult{Version: readiness.ResultVersion, Error: "could not\nfetch " + testDeployToken}
	if out, err := env.Engine.CompleteScan(env.ctx, failed.ID, scanToken(t, env, failed.ID), body(t, res)); err != nil || out != readiness.ScanStored {
		t.Fatalf("CompleteScan = %v, %v", out, err)
	}
	if got := scanOf(t, env, failed.ID); got.State != store.ScanFailed || got.Error != msgScanFailedWithheld {
		t.Errorf("failed scan = %s %q", got.State, got.Error)
	}
	env.SetNow(env.Now().Add(time.Minute))
	plain := queueScan(t, env, testInstance, testProjectPath, "alice")
	env.Engine.launchScans(env.ctx)
	res.Error = "could not fetch\tthe default branch"
	if _, err := env.Engine.CompleteScan(env.ctx, plain.ID, scanToken(t, env, plain.ID), body(t, res)); err != nil {
		t.Fatal(err)
	}
	if got := scanOf(t, env, plain.ID); got.Error != "could not fetch the default branch" {
		t.Errorf("failed scan error = %q", got.Error)
	}
}

// A done scan raises the 3 items it finds and a clean one clears them.
func TestScanAttention(t *testing.T) {
	env := newScanEnv(t)
	useLibrary(env, "ruby")
	scanWith(t, env, testInstance, testProjectPath, gapResult(sha('c')))

	got := items(t, env, testProjectPath)
	budget := (950<<10 + len(rubySkill) + 1023) / 1024
	want := map[string]string{
		attNoSkillCoverage:  noSkillCoverageNote,
		attNoProjectContext: noProjectContextNote,
		attInputsOverBudget: "Skills and instructions come to " + strconv.Itoa(budget) + " KiB, over the 900 KiB a review can take, so every review fails. Use fewer skills or shorten the instructions.",
	}
	for kind, msg := range want {
		if got[kind] != msg {
			t.Errorf("item %s = %q; want %q", kind, got[kind], msg)
		}
	}
	if noSkillCoverageNote != "No skill this repository uses matches its files, so its reviews check nothing useful. The Readiness tab suggests skills that fit." ||
		noProjectContextNote != "The repository has no AGENTS.md, CLAUDE.md or README, so reviews don't know what the project is. Add one." {
		t.Error("the notes changed")
	}

	env.SetDefaultSkills("go")
	env.SetNow(env.Now().Add(scanGap))
	scanWith(t, env, testInstance, testProjectPath, cleanResult(sha('c')))
	got = items(t, env, testProjectPath)
	for kind := range want {
		if _, ok := got[kind]; ok {
			t.Errorf("item %s still set after a clean scan", kind)
		}
	}
}

// An unknown size changes nothing: an earlier inputs_over_budget stays when
// a skill's size can't be added up.
func TestScanBudgetUnknownKeepsItem(t *testing.T) {
	env := newScanEnv(t)
	useLibrary(env, "go")
	scanWith(t, env, testInstance, testProjectPath, gapResult(sha('c')))
	if _, ok := items(t, env, testProjectPath)[attInputsOverBudget]; !ok {
		t.Fatal("no inputs_over_budget item")
	}
	r := cleanResult(sha('c'))
	r.Skills = append(r.Skills, readiness.SkillFit{Name: "broken", Folder: inputs.RepoFolders[0], Problem: "bad front matter"})
	env.SetNow(env.Now().Add(scanGap))
	scanWith(t, env, testInstance, testProjectPath, r)
	if _, ok := items(t, env, testProjectPath)[attInputsOverBudget]; !ok {
		t.Error("an unknown size cleared inputs_over_budget")
	}
}

// Saving skills that match re-judges the last scan with no Job and no
// platform call.
func TestRecheckAfterSkillsChange(t *testing.T) {
	env := newScanEnv(t)
	useLibrary(env, "ruby")
	scanWith(t, env, testInstance, testProjectPath, cleanResult(sha('c')))
	if _, ok := items(t, env, testProjectPath)[attNoSkillCoverage]; !ok {
		t.Fatal("no no_skill_coverage item to clear")
	}
	jobs := len(scanJobs(t, env))
	env.GitLab.Reset()

	env.SetDefaultSkills("go")
	done := make(chan struct{})
	go func() { env.Engine.RecheckReadiness(testInstance, testProjectPath); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RecheckReadiness blocked")
	}
	// The scan loop takes it.
	runScansUntil(t, env, func() bool {
		_, ok := items(t, env, testProjectPath)[attNoSkillCoverage]
		return !ok
	})

	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("a recheck made %d platform calls", n)
	}
	if n := len(scanJobs(t, env)); n != jobs {
		t.Errorf("a recheck started a Job")
	}
	if scans := allScans(t, env); len(scans) != 1 {
		t.Errorf("a recheck queued a scan: %d scans", len(scans))
	}
}

// A recheck of the default list (project "") re-judges only the
// repositories that follow it.
func TestRecheckDefaults(t *testing.T) {
	env := newScanEnv(t)
	useLibrary(env, "ruby")
	serveScanRepo(env, 9, "acme/gadgets", sha('d'))
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", Skills: []string{"ruby"}, CreatedBy: "alice"})
	scanWith(t, env, testInstance, testProjectPath, cleanResult(sha('c')))
	scanWith(t, env, testInstance, "acme/gadgets", cleanResult(sha('d')))
	for _, p := range []string{testProjectPath, "acme/gadgets"} {
		if _, ok := items(t, env, p)[attNoSkillCoverage]; !ok {
			t.Fatalf("%s has no no_skill_coverage item", p)
		}
	}

	// acme/gadgets' own list now matches too, but only the default list is
	// rechecked: its item stays until its own save asks.
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", Skills: []string{"go"}, CreatedBy: "alice"})
	env.SetDefaultSkills("go")
	env.Engine.RecheckReadiness("", "")
	env.Engine.runRechecks(env.ctx)
	if _, ok := items(t, env, testProjectPath)[attNoSkillCoverage]; ok {
		t.Error("the repository on the default list still has no_skill_coverage")
	}
	if _, ok := items(t, env, "acme/gadgets")[attNoSkillCoverage]; !ok {
		t.Error("the repository with its own list was rechecked")
	}
	env.Engine.RecheckReadiness(testInstance, "acme/gadgets")
	env.Engine.runRechecks(env.ctx)
	if _, ok := items(t, env, "acme/gadgets")[attNoSkillCoverage]; ok {
		t.Error("a recheck of acme/gadgets kept no_skill_coverage")
	}
}

// Switching a repository off drops its scans and the 3 items; a result that
// comes late is refused.
func TestSwitchOffDropsScans(t *testing.T) {
	env := newScanEnv(t)
	useLibrary(env, "ruby")
	serveScanRepo(env, 9, "acme/gadgets", sha('d'))
	serveProject(env, 9, "acme/gadgets")
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", CreatedBy: "alice"})
	scanWith(t, env, testInstance, "acme/gadgets", gapResult(sha('d')))
	if n := len(items(t, env, "acme/gadgets")); n != 3 {
		t.Fatalf("%d items; want 3", n)
	}
	env.SetNow(env.Now().Add(scanGap))
	late := queueScan(t, env, testInstance, "acme/gadgets", "alice")
	env.Engine.launchScans(env.ctx)
	token := scanToken(t, env, late.ID)

	if err := env.Store.DeleteDashboardRepo(env.ctx, testInstance, "acme/gadgets"); err != nil {
		t.Fatal(err)
	}
	env.GitLab.Reply(http.MethodDelete, "/projects/9/hooks/11", http.StatusNoContent, nil)
	env.GitLab.Reply(http.MethodDelete, "/projects/9/deploy_tokens/12", http.StatusNoContent, nil)
	if err := env.Engine.ReconcileRepos(env.ctx, []RepoKey{{Instance: testInstance, Project: "acme/gadgets"}}); err != nil {
		t.Fatalf("ReconcileRepos: %v", err)
	}
	latest, active, err := env.Store.ReadinessScans(env.ctx, testInstance, "acme/gadgets")
	if err != nil || latest != nil || active != nil {
		t.Fatalf("scans after switch-off = %v, %v, %v; want none", latest, active, err)
	}
	if n := len(items(t, env, "acme/gadgets")); n != 0 {
		t.Errorf("%d items after switch-off; want none", n)
	}
	if out, err := env.Engine.CompleteScan(env.ctx, late.ID, token, body(t, cleanResult(sha('d')))); err != nil || out != readiness.ScanUnauthorized {
		t.Errorf("late result = %v, %v; want unauthorized", out, err)
	}
	if n := len(items(t, env, "acme/gadgets")); n != 0 {
		t.Errorf("a late result raised %d items", n)
	}
}

// A failed scan changes no item.
func TestFailedScanKeepsItems(t *testing.T) {
	env := newScanEnv(t)
	useLibrary(env, "go")
	scanWith(t, env, testInstance, testProjectPath, gapResult(sha('c')))
	if _, ok := items(t, env, testProjectPath)[attNoProjectContext]; !ok {
		t.Fatal("no no_project_context item")
	}
	env.SetNow(env.Now().Add(scanGap))
	sc := scanWith(t, env, testInstance, testProjectPath, readiness.JobResult{Version: readiness.ResultVersion, Error: "could not fetch the default branch"})
	if sc.State != store.ScanFailed {
		t.Fatalf("scan = %s; want failed", sc.State)
	}
	if _, ok := items(t, env, testProjectPath)[attNoProjectContext]; !ok {
		t.Error("a failed scan cleared no_project_context")
	}
}

// A failed scan after a done one keeps the done scan's file result, so a
// skills save still clears the items it raised.
func TestRecheckAfterFailedScan(t *testing.T) {
	env := newScanEnv(t)
	useLibrary(env, "ruby")
	scanWith(t, env, testInstance, testProjectPath, cleanResult(sha('c')))
	if _, ok := items(t, env, testProjectPath)[attNoSkillCoverage]; !ok {
		t.Fatal("no no_skill_coverage item to clear")
	}
	env.SetNow(env.Now().Add(scanGap))
	failed := scanWith(t, env, testInstance, testProjectPath, readiness.JobResult{Version: readiness.ResultVersion, Error: "could not fetch the default branch"})
	if failed.State != store.ScanFailed {
		t.Fatalf("scan = %s; want failed", failed.State)
	}

	env.SetDefaultSkills("go")
	env.Engine.RecheckReadiness(testInstance, testProjectPath)
	env.Engine.runRechecks(env.ctx)
	if _, ok := items(t, env, testProjectPath)[attNoSkillCoverage]; ok {
		t.Error("a recheck after a failed scan kept no_skill_coverage")
	}
	// The tab still shows the failed scan as the latest.
	if latest, _, err := env.Store.ReadinessScans(env.ctx, testInstance, testProjectPath); err != nil || latest == nil || latest.ID != failed.ID {
		t.Errorf("latest = %+v, %v; want the failed scan", latest, err)
	}
}

// Scan now queues a scan and wakes the loop; a second while it is queued is
// busy, and one within 5 minutes of the last is too soon.
func TestRequestScan(t *testing.T) {
	env := newScanEnv(t)
	sc, err := env.Engine.RequestScan(env.ctx, testInstance, testProjectPath, "alice")
	if err != nil || sc.StartedBy != "alice" || sc.State != store.ScanQueued {
		t.Fatalf("RequestScan = %+v, %v", sc, err)
	}
	again, err := env.Engine.RequestScan(env.ctx, testInstance, "ACME/Widgets", "bob")
	if !errors.Is(err, readiness.ErrScanBusy) || again.ID != sc.ID {
		t.Fatalf("second request = %+v, %v; want busy with the first", again, err)
	}
	env.Engine.launchScans(env.ctx)
	if _, err := env.Engine.CompleteScan(env.ctx, sc.ID, scanToken(t, env, sc.ID), body(t, cleanResult(sha('c')))); err != nil {
		t.Fatal(err)
	}
	env.SetNow(env.Now().Add(2 * time.Minute))
	latest, err := env.Engine.RequestScan(env.ctx, testInstance, testProjectPath, "alice")
	var soon *readiness.ScanTooSoonError
	if !errors.As(err, &soon) || soon.Wait != 3*time.Minute || latest.ID != sc.ID {
		t.Fatalf("request within 5 minutes = %+v, %v; want too soon by 3m", latest, err)
	}
	if _, err := env.Engine.RequestScan(env.ctx, testInstance, "acme/unknown", "alice"); err == nil {
		t.Error("a scan of a repository that is not switched on was queued")
	}
}

// querySQL runs a query of one boolean in the test's schema.
func querySQL(t *testing.T, env *testEnv, sql string) bool {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv(testDBEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.schema
	conn, err := pgx.ConnectConfig(env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(env.ctx)
	var b bool
	if err := conn.QueryRow(env.ctx, sql).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func execSQL(t *testing.T, env *testEnv, sql string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv(testDBEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.schema
	conn, err := pgx.ConnectConfig(env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(env.ctx)
	if _, err := conn.Exec(env.ctx, sql); err != nil {
		t.Fatal(err)
	}
}

// runScansUntil runs RunScans until cond holds (checked every 20 ms, for at
// most 5 s), then stops it.
func runScansUntil(t *testing.T, env *testEnv, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(env.ctx)
	done := make(chan struct{})
	go func() { env.Engine.RunScans(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("RunScans did not get there in 5 s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
