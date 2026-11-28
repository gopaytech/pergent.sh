package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/security"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Security scans in the engine: a person's Start reads the default branch and
// its head and queues the scan; the loop claims up to security.max_running,
// picks the chain's entry and the token budget, and starts the Job; the sweep
// ends scans whose Job failed, went or ran past the deadline, and cancelled
// scans that never posted.

const testSecurityImage = "ghcr.io/gopaytech/pergent-security:v0.0.0-test"

// secOn turns security scans on for env (the chart gave the scan Jobs a way
// out, one runs at a time) and serves acme/widgets' default branch main at
// sha('c').
func secOn(env *testEnv) {
	env.Engine.SecurityEgress = true
	env.Engine.SecurityImage = testSecurityImage
	env.Engine.Config.Security.MaxRunning = 1
	serveScanRepo(env, testProjectID, testProjectPath, sha('c'))
}

// newSecurityEnv is a test environment with security scans on.
func newSecurityEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	secOn(env)
	return env
}

// secQueue queues a scan of project (projectID) at sha('c'), with a 1 hour
// timeout, at the engine's clock plus after.
func secQueue(t *testing.T, env *testEnv, project string, projectID int64, after time.Duration) store.SecurityScan {
	t.Helper()
	sc, err := env.Store.QueueSecurityScan(env.ctx, store.SecurityScanRequest{
		Instance: testInstance, Project: project, ProjectID: projectID, Branch: "main", Commit: sha('c'),
		StartedBy: "maya", TimeoutSeconds: 3600,
	}, env.Now().Add(after))
	if err != nil {
		t.Fatalf("QueueSecurityScan(%s): %v", project, err)
	}
	return sc
}

func secScan(t *testing.T, env *testEnv, id string) store.SecurityScan {
	t.Helper()
	sc, err := env.Store.SecurityScan(env.ctx, id)
	if err != nil {
		t.Fatalf("SecurityScan(%s): %v", id, err)
	}
	return sc
}

// secJobs returns the security scan Jobs in the namespace.
func secJobs(t *testing.T, env *testEnv) []batchv1.Job {
	t.Helper()
	l, err := env.Kube.BatchV1().Jobs(testNamespace).List(env.ctx, metav1.ListOptions{LabelSelector: kube.LabelSecurityScan})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// secJob returns the Job of scan id; nil when there is none.
func secJob(t *testing.T, env *testEnv, id string) *batchv1.Job {
	t.Helper()
	j, err := env.Kube.BatchV1().Jobs(testNamespace).Get(env.ctx, kube.SecurityJobName(id), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return j
}

func secSecret(t *testing.T, env *testEnv, id string) *corev1.Secret {
	t.Helper()
	s, err := env.Kube.CoreV1().Secrets(testNamespace).Get(env.ctx, kube.SecurityJobName(id)+"-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Secret of scan %s: %v", id, err)
	}
	return s
}

// secEnvOf is the value of the env var name in the Job's container; ok is
// false when it is not set.
func secEnvOf(j *batchv1.Job, name string) (string, bool) {
	for _, v := range j.Spec.Template.Spec.Containers[0].Env {
		if v.Name == name {
			return v.Value, true
		}
	}
	return "", false
}

// secFinish ends a running scan with status, as its Job's post would.
func secFinish(t *testing.T, env *testEnv, sc store.SecurityScan, res store.SecurityResult) {
	t.Helper()
	res.Commit = sc.Commit
	if ok, err := env.Store.FinishSecurityScan(env.ctx, sc.ID, nil, res, env.Now()); err != nil || !ok {
		t.Fatalf("FinishSecurityScan(%s) = %v, %v", sc.ID, ok, err)
	}
}

func secLaunch(env *testEnv) { env.Engine.launchSecurityScans(env.ctx) }
func secSweep(env *testEnv)  { env.Engine.sweepSecurityScans(env.ctx) }

func secMaintainer(project string) security.Start {
	return security.Start{Instance: testInstance, Project: project, Actor: "maya"}
}

func secAdmin(project string) security.Start {
	return security.Start{Instance: testInstance, Project: project, Actor: "root", Admin: true}
}

// With no way out for the scan Jobs, no scan starts and nothing is asked of
// the platform.
func TestSecurityStartOffWithoutEgress(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	serveScanRepo(env, testProjectID, testProjectPath, sha('c'))
	if _, err := env.Engine.RequestSecurityScan(env.ctx, secAdmin(testProjectPath)); !errors.Is(err, security.ErrOff) {
		t.Fatalf("RequestSecurityScan = %v; want ErrOff", err)
	}
	if got := env.Engine.StartOff(); got != "Start scan is off: an admin must set securityJobEgress.cidrs, or networkPolicy.enabled: false, in the Helm values first" {
		t.Errorf("StartOff = %q", got)
	}
	if active, err := env.Store.ActiveSecurityScan(env.ctx, testInstance, testProjectPath); err != nil || active != nil {
		t.Errorf("active scan = %+v, %v; want none", active, err)
	}
	if n := len(env.GitLab.Requests(http.MethodGet, projectPath())); n != 0 {
		t.Errorf("%d project reads; want none", n)
	}
	env.Engine.SecurityEgress = true
	if got := env.Engine.StartOff(); got != "" {
		t.Errorf("StartOff with egress = %q; want empty", got)
	}
}

// Start stores the default branch, its head, the repository's ID and its
// settings; the launch asks the platform nothing and runs on that commit.
func TestSecurityStartReadsCommit(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	steps, timeout := 50, 1200
	if err := env.Store.PutSecurityScanSettings(env.ctx, testInstance, testProjectPath,
		store.SecurityScanSettings{MaxSteps: &steps, TimeoutSeconds: &timeout}, env.Now()); err != nil {
		t.Fatal(err)
	}
	sc, err := env.Engine.RequestSecurityScan(env.ctx, secMaintainer(testProjectPath))
	if err != nil {
		t.Fatalf("RequestSecurityScan: %v\n%s", err, env.Logs())
	}
	if sc.State != store.SecurityQueued || sc.Branch != "main" || sc.Commit != sha('c') || sc.ProjectID != testProjectID ||
		sc.StartedBy != "maya" || sc.PastQuota || sc.TimeoutSeconds != 1200 || sc.MaxSteps == nil || *sc.MaxSteps != 50 ||
		sc.SubagentMaxSteps != nil {
		t.Fatalf("queued scan = %+v", sc)
	}

	// The platform moves on: the scan still runs on the commit read at Start.
	env.GitLab.Reply(http.MethodGet, projectPath()+"/repository/commits/main", http.StatusOK, map[string]any{"id": sha('d')})
	reads := len(env.GitLab.Requests(http.MethodGet, projectPath())) +
		len(env.GitLab.Requests(http.MethodGet, projectPath()+"/repository/commits/main"))
	secLaunch(env)
	j := secJob(t, env, sc.ID)
	if j == nil {
		t.Fatalf("no Job\n%s", env.Logs())
	}
	if after := len(env.GitLab.Requests(http.MethodGet, projectPath())) +
		len(env.GitLab.Requests(http.MethodGet, projectPath()+"/repository/commits/main")); after != reads {
		t.Errorf("the launch read the platform %d times", after-reads)
	}
	args := j.Spec.Template.Spec.Containers[0].Args
	for _, want := range []string{"--head=" + sha('c'), "--max-turns=50", "--timeout=20m0s"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v; want %s", args, want)
		}
	}
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--max-subagent-turns") }) {
		t.Errorf("args %v; want the skill's own sub-agent steps", args)
	}
	if d := *j.Spec.ActiveDeadlineSeconds; d != int64((20*time.Minute+securityDeadlineGrace)/time.Second) {
		t.Errorf("Job deadline = %d s", d)
	}
}

// Each refusal of Start: paused, a default branch the platform can't give, a
// scan already queued, a used-up repository quota (an admin goes past it), and
// a team's own limit (an admin doesn't).
func TestSecurityStartRefusals(t *testing.T) {
	t.Parallel()
	env := quotaPushEnv(t, 1000, 0)
	secOn(env)
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", CreatedBy: "alice"})
	pause(t, env, "acme/gadgets", false)
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/broken", CreatedBy: "alice"})
	env.SeedProject(func(p *store.Project) { p.ProjectID, p.Path = 8, "acme/broken" })

	// cancel ends the queued scan sc, so the next case may queue.
	cancel := func(t *testing.T, sc store.SecurityScan) {
		t.Helper()
		if _, ok, err := env.Store.RequestSecurityCancel(env.ctx, sc.ID, env.Now()); err != nil || !ok {
			t.Fatalf("cancel = %v, %v", ok, err)
		}
	}

	t.Run("paused", func(t *testing.T) {
		if _, err := env.Engine.RequestSecurityScan(env.ctx, secAdmin("acme/gadgets")); !errors.Is(err, security.ErrPaused) {
			t.Fatalf("RequestSecurityScan = %v; want ErrPaused", err)
		}
	})
	t.Run("default branch unread", func(t *testing.T) {
		_, err := env.Engine.RequestSecurityScan(env.ctx, secMaintainer("acme/broken"))
		var refused *security.RefusedError
		if !errors.As(err, &refused) || refused.Reason != errDefaultBranch.Error() {
			t.Fatalf("RequestSecurityScan = %v; want refused with %q", err, errDefaultBranch)
		}
		if active, err := env.Store.ActiveSecurityScan(env.ctx, testInstance, "acme/broken"); err != nil || active != nil {
			t.Errorf("active scan = %+v, %v; want none", active, err)
		}
	})
	t.Run("busy", func(t *testing.T) {
		first, err := env.Engine.RequestSecurityScan(env.ctx, secMaintainer(testProjectPath))
		if err != nil {
			t.Fatalf("first start: %v", err)
		}
		again, err := env.Engine.RequestSecurityScan(env.ctx, secMaintainer(testProjectPath))
		if !errors.Is(err, security.ErrBusy) || again.ID != first.ID {
			t.Fatalf("second start = %s, %v; want ErrBusy with %s", again.ID, err, first.ID)
		}
		cancel(t, first)
	})
	t.Run("quota used up, maintainer", func(t *testing.T) {
		seedQuotaUsage(t, env, 1000)
		if _, err := env.Engine.RequestSecurityScan(env.ctx, secMaintainer(testProjectPath)); !errors.Is(err, security.ErrQuota) {
			t.Fatalf("RequestSecurityScan = %v; want ErrQuota", err)
		}
		if active, err := env.Store.ActiveSecurityScan(env.ctx, testInstance, testProjectPath); err != nil || active != nil {
			t.Errorf("active scan = %+v, %v; want none", active, err)
		}
	})
	t.Run("quota used up, admin", func(t *testing.T) {
		sc, err := env.Engine.RequestSecurityScan(env.ctx, secAdmin(testProjectPath))
		if err != nil || sc.State != store.SecurityQueued || !sc.PastQuota || sc.StartedBy != "root" {
			t.Fatalf("RequestSecurityScan = %+v, %v; want queued past the quota", sc, err)
		}
		cancel(t, sc)
	})
	t.Run("team's own limit, admin", func(t *testing.T) {
		team := teamOwning(t, env, "Payments")
		a := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
		saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{a}})
		seedEntryUsage(t, env, a, 10)
		setLimit(t, env, a, team, 10)
		_, err := env.Engine.RequestSecurityScan(env.ctx, secAdmin(testProjectPath))
		var tl *security.TeamLimitError
		if !errors.As(err, &tl) || tl.Team != "Payments" {
			t.Fatalf("RequestSecurityScan = %v; want the team limit of Payments", err)
		}
	})
}

// With security.max_running 1, one of 2 queued scans runs; the other starts
// once the first has ended.
func TestSecurityLaunchCap(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: "acme/gadgets", CreatedBy: "alice"})
	env.SeedProject(func(p *store.Project) { p.ProjectID, p.Path = 8, "acme/gadgets" })
	first := secQueue(t, env, testProjectPath, testProjectID, 0)
	second := secQueue(t, env, "acme/gadgets", 8, time.Second)

	secLaunch(env)
	secLaunch(env)
	if jobs := secJobs(t, env); len(jobs) != 1 || secJob(t, env, first.ID) == nil {
		t.Fatalf("%d Jobs after 2 passes; want the first scan's alone\n%s", len(jobs), env.Logs())
	}
	if sc := secScan(t, env, second.ID); sc.State != store.SecurityQueued {
		t.Fatalf("second scan = %s; want queued", sc.State)
	}
	secFinish(t, env, secScan(t, env, first.ID), store.SecurityResult{Status: store.SecurityComplete})
	secLaunch(env)
	if secJob(t, env, second.ID) == nil {
		t.Fatalf("no Job for the second scan\n%s", env.Logs())
	}
}

// The Job runs the security image on the chosen entry's model, with the
// smallest room left as its budget, the commit read at Start and the prior
// file: the last complete scan's state and the dismissed findings.
func TestSecurityLaunchJob(t *testing.T) {
	t.Parallel()
	env := quotaPushEnv(t, 1000, 0)
	secOn(env)
	seedEntryUsage(t, env, configRef(), 300) // the repository has 700 left
	setLimit(t, env, configRef(), 0, 400)    // the model has 100 left

	last := secQueue(t, env, testProjectPath, testProjectID, 0)
	if _, ok, err := env.Store.ClaimSecurityScan(env.ctx, 1, env.Now(), env.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	secFinish(t, env, last, store.SecurityResult{Status: store.SecurityComplete, SkillName: "security-review", SkillCommit: "v1",
		State: map[string]string{"plan.md": "the plan"},
		Findings: []store.SecurityFindingInput{{Fingerprint: "fp-1", Severity: "high", Title: "SQL injection", File: "db.go",
			StartLine: 3, RuleID: "injection"}}})
	fs, _, err := env.Store.SecurityFindings(env.ctx, testInstance, testProjectPath, store.SecurityFindingFilter{})
	if err != nil || len(fs) != 1 {
		t.Fatalf("findings = %d, %v", len(fs), err)
	}
	if ok, err := env.Store.DismissSecurityFinding(env.ctx, testInstance, testProjectPath, fs[0].ID, "false_positive",
		"test data", "maya", env.Now()); err != nil || !ok {
		t.Fatalf("dismiss = %v, %v", ok, err)
	}

	sc := secQueue(t, env, testProjectPath, testProjectID, time.Minute)
	launchedAt := env.Now()
	secLaunch(env)
	j := secJob(t, env, sc.ID)
	if j == nil {
		t.Fatalf("no Job\n%s", env.Logs())
	}
	c := j.Spec.Template.Spec.Containers[0]
	if c.Image != testSecurityImage || j.Labels[kube.LabelSecurityScan] != sc.ID {
		t.Errorf("Job image %q, label %q", c.Image, j.Labels[kube.LabelSecurityScan])
	}
	if v, _ := secEnvOf(j, "OPENCODE_MODEL"); v != "claude-sonnet-4" {
		t.Errorf("OPENCODE_MODEL = %q", v)
	}
	if v, ok := secEnvOf(j, "PERGENT_TOKEN_BUDGET"); !ok || v != "100" {
		t.Errorf("PERGENT_TOKEN_BUDGET = %q, %v; want 100", v, ok)
	}
	for _, want := range []string{"--head=" + sha('c'), "--prior=/in/prior.json", "--timeout=1h0m0s",
		"--source-url=" + testCallbackURL + "/security-scans/" + sc.ID + "/source",
		"--result-url=" + testCallbackURL + "/security-scans/" + sc.ID + "/result"} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("args %v; want %s", c.Args, want)
		}
	}
	secret := secSecret(t, env, sc.ID)
	if got := string(secret.Data["llm-api-key"]); got != testLLMKey {
		t.Errorf("the Job's key = %q", got)
	}
	var prior struct {
		State     map[string]string `json:"state"`
		Dismissed []struct {
			Fingerprint, Reason, Comment, File string
		} `json:"dismissed"`
	}
	if err := json.Unmarshal(secret.Data["prior"], &prior); err != nil {
		t.Fatalf("prior file: %v", err)
	}
	if prior.State["plan.md"] != "the plan" || len(prior.Dismissed) != 1 || prior.Dismissed[0].Fingerprint != "fp-1" ||
		prior.Dismissed[0].Reason != "false_positive" || prior.Dismissed[0].Comment != "test data" {
		t.Errorf("prior file = %+v", prior)
	}

	got := secScan(t, env, sc.ID)
	if got.State != store.SecurityRunning || got.JobName != kube.SecurityJobName(sc.ID) || got.Model != "claude-sonnet-4" ||
		got.TokenBudget == nil || *got.TokenBudget != 100 {
		t.Errorf("scan = %+v", got)
	}
	if want := launchedAt.Add(time.Hour + securityDeadlineGrace + securityCancelGrace); got.Deadline == nil || !got.Deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v", got.Deadline, want)
	}
	// The carrier run names the entry, so the model and team limits count
	// the scan's tokens.
	if r := env.Run(sc.RunID); r.ModelSource != store.SourceGlobal || r.ModelEntryID != store.ConfigModelID {
		t.Errorf("carrier run entry = %s/%s", r.ModelSource, r.ModelEntryID)
	}
}

// With no quota and no limit, the Job gets no budget; with no earlier scan,
// no prior file.
func TestSecurityLaunchNoBudgetWithoutLimits(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	sc := secQueue(t, env, testProjectPath, testProjectID, 0)
	secLaunch(env)
	j := secJob(t, env, sc.ID)
	if j == nil {
		t.Fatalf("no Job\n%s", env.Logs())
	}
	if v, ok := secEnvOf(j, "PERGENT_TOKEN_BUDGET"); ok {
		t.Errorf("PERGENT_TOKEN_BUDGET = %q; want none", v)
	}
	if slices.ContainsFunc(j.Spec.Template.Spec.Containers[0].Args, func(a string) bool { return strings.HasPrefix(a, "--prior") }) {
		t.Errorf("args %v; want no prior file", j.Spec.Template.Spec.Containers[0].Args)
	}
	if got := secScan(t, env, sc.ID); got.TokenBudget != nil {
		t.Errorf("token budget = %d; want none", *got.TokenBudget)
	}
}

// An entry with a live quota mark is passed over at launch; a failed scan is
// never tried again on another entry.
func TestSecurityNoFallback(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	_, first, _ := twoTeamEntries(t, env)
	until := env.Now().Add(time.Hour)
	setMark(t, env, first, store.ClassQuota, &until)
	sc := secQueue(t, env, testProjectPath, testProjectID, 0)
	secLaunch(env)
	if secJob(t, env, sc.ID) == nil {
		t.Fatalf("no Job\n%s", env.Logs())
	}
	if got := string(secSecret(t, env, sc.ID).Data["llm-api-key"]); got != "key-team-b" {
		t.Fatalf("the Job's key = %q; want entry 2's", got)
	}
	if got := secScan(t, env, sc.ID); got.EntryName != "team-b" {
		t.Errorf("entry = %q; want team-b", got.EntryName)
	}

	secFinish(t, env, secScan(t, env, sc.ID), store.SecurityResult{Status: store.SecurityFailed, Reason: "the provider's quota is used up"})
	secLaunch(env)
	secSweep(env)
	if n := len(secJobs(t, env)); n != 1 {
		t.Errorf("%d Jobs; want the first alone", n)
	}
	scans, err := env.Store.SecurityScans(env.ctx, testInstance, testProjectPath, 10)
	if err != nil || len(scans) != 1 || scans[0].State != store.SecurityFailed {
		t.Errorf("scans = %+v, %v; want the failed one alone", scans, err)
	}
}

// The sweep ends a scan whose Job never started, whose Job failed, or that
// ran past its deadline (its Job deleted); a cancelled scan with no post ends
// partial, cancelled, only after the grace.
func TestSecuritySweep(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	serveLog(env, "scanning\n")
	// launched queues a scan and starts its Job.
	launched := func(t *testing.T) store.SecurityScan {
		t.Helper()
		sc := secQueue(t, env, testProjectPath, testProjectID, 0)
		secLaunch(env)
		got := secScan(t, env, sc.ID)
		if got.State != store.SecurityRunning || secJob(t, env, sc.ID) == nil {
			t.Fatalf("scan = %s with no Job\n%s", got.State, env.Logs())
		}
		return got
	}
	wantEnded := func(t *testing.T, id, state, reason string) {
		t.Helper()
		if sc := secScan(t, env, id); sc.State != state || sc.Reason != reason {
			t.Fatalf("scan = %s %q; want %s %q", sc.State, sc.Reason, state, reason)
		}
	}

	t.Run("no Job past the deadline", func(t *testing.T) {
		sc := secQueue(t, env, testProjectPath, testProjectID, 0)
		if _, ok, err := env.Store.ClaimSecurityScan(env.ctx, 1, env.Now(), env.Now().Add(time.Minute)); err != nil || !ok {
			t.Fatalf("claim = %v, %v", ok, err)
		}
		secSweep(env)
		wantEnded(t, sc.ID, store.SecurityRunning, "")
		env.SetNow(env.Now().Add(2 * time.Minute))
		secSweep(env)
		wantEnded(t, sc.ID, store.SecurityFailed, "the scan stopped before its Job started; start it again")
	})
	t.Run("Job not created yet", func(t *testing.T) {
		sc := secQueue(t, env, testProjectPath, testProjectID, 0)
		claimed, ok, err := env.Store.ClaimSecurityScan(env.ctx, 1, env.Now(), env.Now().Add(time.Minute))
		if err != nil || !ok || claimed.ID != sc.ID {
			t.Fatalf("claim = %v, %v", ok, err)
		}
		// Its name is stored, and the launch has not created the Job yet.
		if err := env.Store.StartSecurityJob(env.ctx, sc.ID, store.SecurityJobStart{JobName: kube.SecurityJobName(sc.ID),
			TokenHash: hashToken("t"), Deadline: env.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		env.SetNow(claimed.StartedAt.Add(securityLaunchTimeout + time.Minute))
		secSweep(env)
		wantEnded(t, sc.ID, store.SecurityRunning, "")
		env.SetNow(env.Now().Add(time.Second))
		secSweep(env)
		wantEnded(t, sc.ID, store.SecurityFailed, "the scan Job ended without a result")
	})
	t.Run("failed Job", func(t *testing.T) {
		sc := launched(t)
		j := secJob(t, env, sc.ID)
		at := env.Now().Add(-5 * time.Minute)
		j.Status.Conditions = []batchv1.JobCondition{jobCond(batchv1.JobFailed, "BackoffLimitExceeded", at)}
		if _, err := env.Kube.BatchV1().Jobs(testNamespace).UpdateStatus(env.ctx, j, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-abcde", Namespace: testNamespace,
				Labels: map[string]string{"job-name": j.Name}, CreationTimestamp: metav1.NewTime(at.Add(-time.Minute))},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				terminatedStatus("pergent", 137, "OOMKilled", at.Add(-time.Minute), at)}},
		}
		if _, err := env.Kube.CoreV1().Pods(testNamespace).Create(env.ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		secSweep(env)
		wantEnded(t, sc.ID, store.SecurityFailed, "the scan ran out of memory")
	})
	t.Run("past the deadline", func(t *testing.T) {
		sc := launched(t)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sc.JobName + "-abcde", Namespace: testNamespace,
			Labels: map[string]string{"job-name": sc.JobName}}}
		if _, err := env.Kube.CoreV1().Pods(testNamespace).Create(env.ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		env.SetNow(sc.Deadline.Add(-time.Second))
		secSweep(env)
		wantEnded(t, sc.ID, store.SecurityRunning, "")
		env.SetNow(sc.Deadline.Add(time.Second))
		secSweep(env)
		wantEnded(t, sc.ID, store.SecurityFailed, "the scan took longer than its timeout")
		if secJob(t, env, sc.ID) != nil {
			t.Errorf("the Job past its deadline was not deleted")
		}
		// Its log was taken before the Job went.
		if text, found, err := env.Store.RunLog(env.ctx, sc.RunID); err != nil || !found || text != "scanning\n" {
			t.Errorf("log of the timed-out scan = %q, %v, %v; want it stored", text, found, err)
		}
	})
	t.Run("cancel with no post", func(t *testing.T) {
		sc := launched(t)
		if _, err := env.Engine.CancelSecurityScan(env.ctx, testInstance, testProjectPath, sc.ID); err != nil {
			t.Fatalf("CancelSecurityScan: %v", err)
		}
		// Its Job is gone, but pergent may still post: the scan waits.
		env.SetNow(env.Now().Add(securityCancelGrace))
		secSweep(env)
		wantEnded(t, sc.ID, store.SecurityRunning, "")
		env.SetNow(env.Now().Add(time.Second))
		secSweep(env)
		wantEnded(t, sc.ID, store.SecurityPartial, "cancelled")
	})
}

// Cancel of a running scan takes its log, then deletes its Job; a queued scan
// ends at once; a scan that has ended can't be cancelled; a scan of another
// repository is not found.
func TestSecurityCancel(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	serveLog(env, "scan started with "+testLLMKey+"\n")

	t.Run("running", func(t *testing.T) {
		sc := secQueue(t, env, testProjectPath, testProjectID, 0)
		secLaunch(env)
		j := secJob(t, env, sc.ID)
		if j == nil {
			t.Fatalf("no Job\n%s", env.Logs())
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-abcde", Namespace: testNamespace,
			Labels: map[string]string{"job-name": j.Name}}}
		if _, err := env.Kube.CoreV1().Pods(testNamespace).Create(env.ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.Engine.CancelSecurityScan(env.ctx, testInstance, "Acme/Other", sc.ID); !errors.Is(err, store.ErrSecurityScanNotFound) {
			t.Errorf("cancel through another repository = %v; want not found", err)
		}
		got, err := env.Engine.CancelSecurityScan(env.ctx, testInstance, "ACME/widgets", sc.ID)
		if err != nil || got.State != store.SecurityRunning || got.CancelRequestedAt == nil {
			t.Fatalf("CancelSecurityScan = %+v, %v; want running with the cancel asked", got, err)
		}
		if secJob(t, env, sc.ID) != nil {
			t.Errorf("the Job was not deleted")
		}
		text, found, err := env.Store.RunLog(env.ctx, sc.RunID)
		if err != nil || !found || strings.Contains(text, testLLMKey) || !strings.Contains(text, "scan started with") {
			t.Errorf("stored log = %q, %v, %v; want it redacted", text, found, err)
		}
		secFinish(t, env, got, store.SecurityResult{Status: store.SecurityPartial, Reason: "cancelled"})
	})
	t.Run("queued", func(t *testing.T) {
		sc := secQueue(t, env, testProjectPath, testProjectID, 0)
		got, err := env.Engine.CancelSecurityScan(env.ctx, testInstance, testProjectPath, sc.ID)
		if err != nil || got.State != store.SecurityPartial || got.Reason != "cancelled" {
			t.Fatalf("CancelSecurityScan = %+v, %v; want partial, cancelled", got, err)
		}
	})
	t.Run("ended", func(t *testing.T) {
		scans, err := env.Store.SecurityScans(env.ctx, testInstance, testProjectPath, 1)
		if err != nil || len(scans) != 1 {
			t.Fatal(scans, err)
		}
		got, err := env.Engine.CancelSecurityScan(env.ctx, testInstance, testProjectPath, scans[0].ID)
		if !errors.Is(err, security.ErrEnded) || got.ID != scans[0].ID {
			t.Fatalf("CancelSecurityScan = %s, %v; want ErrEnded with the scan", got.ID, err)
		}
	})
}

// A repository switched off has its running scan cancelled (its Job
// deleted); the scan's rows stay.
func TestSecurityRemoveCancels(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	sc, err := env.Store.QueueSecurityScan(env.ctx, store.SecurityScanRequest{Instance: "gone", Project: "acme/old",
		ProjectID: 99, Branch: "main", Commit: sha('c'), StartedBy: "maya", TimeoutSeconds: 3600}, env.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := env.Store.ClaimSecurityScan(env.ctx, 1, env.Now(), env.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	name := kube.SecurityJobName(sc.ID)
	if err := env.Store.StartSecurityJob(env.ctx, sc.ID, store.SecurityJobStart{JobName: name, TokenHash: hashToken("t"),
		Deadline: env.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Kube.BatchV1().Jobs(testNamespace).Create(env.ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: testNamespace, Labels: map[string]string{kube.LabelSecurityScan: sc.ID}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := env.Engine.removeProject(env.ctx, store.Project{Instance: "gone", ProjectID: 99, Path: "acme/old"}); err != nil {
		t.Fatalf("removeProject: %v", err)
	}
	got := secScan(t, env, sc.ID)
	if got.State != store.SecurityRunning || got.CancelRequestedAt == nil {
		t.Errorf("scan = %s, cancel %v; want running with the cancel asked", got.State, got.CancelRequestedAt)
	}
	if secJob(t, env, sc.ID) != nil {
		t.Errorf("the Job of the removed repository was not deleted")
	}
	if scans, err := env.Store.SecurityScans(env.ctx, "gone", "acme/old", 10); err != nil || len(scans) != 1 {
		t.Errorf("scans = %d, %v; want the row kept", len(scans), err)
	}
}

// A cancel during the launch stops the scan: before the Job is recorded it
// gets no Job and ends partial, cancelled; while the Job is being created the
// cancel finds no Job to delete, so the launch deletes it once it exists.
func TestSecurityCancelDuringLaunch(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)

	t.Run("before the Job is recorded", func(t *testing.T) {
		secQueue(t, env, testProjectPath, testProjectID, 0)
		claimed, ok, err := env.Store.ClaimSecurityScan(env.ctx, 1, env.Now(), env.Now().Add(time.Minute))
		if err != nil || !ok {
			t.Fatalf("claim = %v, %v", ok, err)
		}
		if _, err := env.Engine.CancelSecurityScan(env.ctx, testInstance, testProjectPath, claimed.ID); err != nil {
			t.Fatalf("CancelSecurityScan: %v", err)
		}
		env.Engine.startSecurityScan(env.ctx, claimed)
		if sc := secScan(t, env, claimed.ID); sc.State != store.SecurityPartial || sc.Reason != "cancelled" || sc.JobName != "" {
			t.Errorf("scan = %s %q, Job %q; want partial, cancelled, no Job", sc.State, sc.Reason, sc.JobName)
		}
		if secJob(t, env, claimed.ID) != nil {
			t.Errorf("a cancelled scan got a Job")
		}
	})
	t.Run("while the Job is created", func(t *testing.T) {
		sc := secQueue(t, env, testProjectPath, testProjectID, 0)
		var once sync.Once
		env.Kube.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.(k8stesting.CreateAction).GetObject().(*batchv1.Job).Name == kube.SecurityJobName(sc.ID) {
				once.Do(func() {
					// The dashboard's cancel: the scan has a Job name, but
					// no Job to delete yet.
					if _, ok, err := env.Store.RequestSecurityCancel(env.ctx, sc.ID, env.Now()); err != nil || !ok {
						t.Errorf("cancel = %v, %v", ok, err)
					}
				})
			}
			return false, nil, nil
		})
		secLaunch(env)
		got := secScan(t, env, sc.ID)
		if got.State != store.SecurityRunning || got.CancelRequestedAt == nil || got.JobName == "" {
			t.Fatalf("scan = %+v; want running with the cancel asked\n%s", got, env.Logs())
		}
		if secJob(t, env, sc.ID) != nil {
			t.Errorf("the Job of a scan cancelled during its launch was not deleted")
		}
	})
}

// An admin's start goes past the quota whether or not it had room then: a
// scan that waits while the quota fills still runs.
func TestSecurityAdminStartLaunchesPastQuota(t *testing.T) {
	t.Parallel()
	env := quotaPushEnv(t, 1000, 0)
	secOn(env)
	sc, err := env.Engine.RequestSecurityScan(env.ctx, secAdmin(testProjectPath))
	if err != nil || !sc.PastQuota {
		t.Fatalf("RequestSecurityScan = %+v, %v; want queued past the quota", sc, err)
	}
	seedQuotaUsage(t, env, 1000)
	secLaunch(env)
	if got := secScan(t, env, sc.ID); got.State != store.SecurityRunning || secJob(t, env, sc.ID) == nil {
		t.Fatalf("scan = %s %q; want running with a Job\n%s", got.State, got.Reason, env.Logs())
	}
}
