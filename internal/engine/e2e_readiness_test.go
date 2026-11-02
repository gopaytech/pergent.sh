package engine

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// serveGadgetsSettings serves what the pod part of a scan of acme/gadgets
// reads with the review bot: the project and its default branch at commit,
// 2 approvals, main protected for Maintainers only, and pipelines that must
// succeed.
func serveGadgetsSettings(env *testEnv, commit string) {
	gl := env.GitLab
	gl.Reply(http.MethodGet, gadgetsAPI(""), http.StatusOK, map[string]any{
		"id": gadgetsID, "path_with_namespace": gadgetsPath, "default_branch": "main",
		"http_url_to_repo": "https://gitlab.example.com/" + gadgetsPath + ".git", "only_allow_merge_if_pipeline_succeeds": true,
	})
	gl.Reply(http.MethodGet, gadgetsAPI("/repository/commits/main"), http.StatusOK, map[string]any{"id": commit})
	gl.Reply(http.MethodGet, gadgetsAPI("/approval_rules"), http.StatusOK, []map[string]any{
		{"rule_type": "regular", "approvals_required": 2},
	})
	gl.Reply(http.MethodGet, gadgetsAPI("/protected_branches"), http.StatusOK, []map[string]any{
		{"name": "main", "push_access_levels": []map[string]any{{"access_level": 40}}},
	})
}

// checklistItem is the item key of the checklist.
func checklistItem(t *testing.T, c *readiness.Checklist, key string) readiness.Item {
	t.Helper()
	for _, s := range c.Sections {
		for _, it := range s.Items {
			if it.Key == key {
				return it
			}
		}
	}
	t.Fatalf("the checklist has no item %q: %+v", key, c)
	return readiness.Item{}
}

// postScanResult posts body as the result of scan id with token and returns
// the status.
func (e *dashE2E) postScanResult(t *testing.T, id, token string, body []byte) int {
	t.Helper()
	status, _, _, _ := e.do(t, http.MethodPost, "/readiness-scans/"+id+"/result", json.RawMessage(body), nil,
		map[string]string{"Authorization": "Bearer " + token})
	return status
}

// An admin switches a repository on. The reconcile queues a switch-on scan,
// which reads the settings in the pod and starts a Job that holds the clone
// token in its clone container only. The Job posts its result with the
// one-time token through the real route; the Readiness tab then has the
// checklist and Needs attention the missing project context. Scan now is 409
// while the scan runs and 429 once it has ended, until 5 minutes have passed.
func TestReadinessScanEndToEnd(t *testing.T) {
	e := newDashboardE2E(t)
	env := e.env
	env.Engine.Scans = true
	env.Engine.ScanImage = testScanImage
	commit := sha('c')
	serveGadgetsSettings(env, commit)

	t.Setenv(adminPasswordEnv, bootstrapPassword)
	env.Engine.Config.Dashboard.AdminPasswordEnv = adminPasswordEnv
	if err := dashboard.Bootstrap(env.ctx, env.Store, env.Engine.Config, env.Now(), env.Engine.Log); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	admin := e.passwordSignIn(t, "admin", bootstrapPassword)
	if status, body := e.write(t, admin, http.MethodPost, "/auth/password/change",
		map[string]string{"current": bootstrapPassword, "new": changedPassword}); status != http.StatusNoContent {
		t.Fatalf("password change = %d: %s", status, body)
	}
	tab := "/api/repositories/" + testInstance + "/" + gadgetsPath + "/readiness"

	// 1. Switching acme/gadgets on and the reconcile that follows queue a
	// scan, started by switch-on. Nothing has run yet.
	if status, body := e.write(t, admin, http.MethodPost, "/api/repositories", map[string]string{"instance": testInstance, "project": gadgetsPath}); status != http.StatusCreated {
		t.Fatalf("switch on = %d: %s", status, body)
	}
	keys := env.Engine.takeSoonKeys()
	if len(keys) != 1 || !strings.EqualFold(keys[0].Project, gadgetsPath) {
		t.Fatalf("reconcile asked for %+v, want acme/gadgets alone", keys)
	}
	if err := env.Engine.ReconcileRepos(env.ctx, keys); err != nil {
		t.Fatalf("ReconcileRepos: %v", err)
	}
	var view dashboard.ReadinessView
	e.getJSON(t, admin, tab, &view)
	if view.Latest != nil || view.Active == nil || view.Active.State != string(store.ScanQueued) ||
		view.Active.StartedBy != store.StartedBySwitchOn || view.Checklist != nil || !view.CanScan {
		t.Fatalf("the tab after the switch-on = %+v, want a queued switch-on scan and no checklist", view)
	}
	if n := len(scanJobs(t, env)); n != 0 {
		t.Fatalf("%d scan Jobs before the scan loop ran, want none", n)
	}
	id := view.Active.ID

	// 2. The loop reads the settings in the pod and starts the Job.
	env.Engine.launchScans(env.ctx)
	jobs := scanJobs(t, env)
	if len(jobs) != 1 || jobs[0].Name != "pergent-scan-"+strings.ReplaceAll(id, "-", "")[:12] {
		t.Fatalf("scan Jobs = %d, want 1 named after the scan %s\n%s", len(jobs), id, env.Logs())
	}
	secret, err := env.Kube.CoreV1().Secrets(testNamespace).Get(env.ctx, jobs[0].Name+"-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the scan Job's Secret: %v", err)
	}
	token := string(secret.Data["scan-token"])
	if token == "" || string(secret.Data["git-token"]) != newDeployToken {
		t.Fatalf("the Secret holds the scan token %q and the clone token of %d bytes, want both", token, len(secret.Data["git-token"]))
	}
	checkScanJob(t, jobs[0], secret, token)
	e.getJSON(t, admin, tab, &view)
	if view.Active == nil || view.Active.State != string(store.ScanRunning) || view.Active.Branch != "main" || view.Active.Commit != commit {
		t.Fatalf("the tab while the Job runs = %+v, want a running scan of main at %s", view.Active, commit)
	}

	// 3. Scan now while it runs is 409 and names the running scan; a result
	// with the wrong token is refused and leaves the scan running.
	status, body := e.write(t, admin, http.MethodPost, tab+"/scan", nil)
	var busy struct {
		Scan *dashboard.ScanView `json:"scan"`
	}
	if status != http.StatusConflict || json.Unmarshal(body, &busy) != nil || busy.Scan == nil || busy.Scan.ID != id {
		t.Fatalf("Scan now while one runs = %d: %s, want 409 with scan %s", status, body, id)
	}
	result, err := json.Marshal(readiness.JobResult{
		Version: readiness.ResultVersion, Commit: commit, Files: 10, SourceFiles: 8,
		Skills:            []readiness.SkillFit{{Name: "security", Folder: inputs.SourceLibrary}},
		InstructionsBytes: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := e.postScanResult(t, id, "not-the-scan-token-example", result); code != http.StatusUnauthorized {
		t.Fatalf("a result with the wrong token = %d, want 401", code)
	}
	if got := scanOf(t, env, id); got.State != store.ScanRunning {
		t.Fatalf("a refused result left the scan %s, want running", got.State)
	}

	// 4. The Job posts its result with its token through the real route; the
	// token is spent on first use.
	if code := e.postScanResult(t, id, token, result); code != http.StatusNoContent {
		t.Fatalf("the result = %d, want 204\n%s", code, env.Logs())
	}
	if code := e.postScanResult(t, id, token, result); code != http.StatusConflict {
		t.Errorf("the result posted twice = %d, want 409", code)
	}

	// 5. The tab has the checklist: no agent file or README, and the 3
	// settings the pod read.
	e.getJSON(t, admin, tab, &view)
	if view.Active != nil || view.Latest == nil || view.Latest.State != string(store.ScanDone) || view.Latest.Commit != commit ||
		view.Latest.StartedBy != store.StartedBySwitchOn || view.Checklist == nil {
		t.Fatalf("the tab after the result = %+v, want a done switch-on scan of %s with a checklist", view, commit)
	}
	for key, want := range map[string]string{
		"agent_file": readiness.Missing, "readme": readiness.Missing, "approvals": readiness.Passed,
		"protected": readiness.Passed, "required_checks": readiness.Passed, "skill_match": readiness.Passed,
		"budget": readiness.Passed,
	} {
		if it := checklistItem(t, view.Checklist, key); it.State != want {
			t.Errorf("checklist %s = %q (%s), want %q", key, it.State, it.Reason, want)
		}
	}

	// 6. Needs attention has no_project_context, and neither of the other 2
	// readiness items (security has no paths, so it covers the repository).
	var overview dashboard.OverviewView
	e.getJSON(t, admin, "/api/overview", &overview)
	kinds := map[string]bool{}
	for _, a := range overview.Attention {
		if a.Instance == testInstance && strings.EqualFold(a.Project, gadgetsPath) {
			kinds[a.Kind] = true
		}
	}
	if !kinds[attNoProjectContext] || kinds[attNoSkillCoverage] || kinds[attInputsOverBudget] {
		t.Errorf("needs attention of acme/gadgets = %v, want no_project_context alone of the readiness items", kinds)
	}

	// 7. Scan now at once is 429 with the wait; after 5 minutes it is
	// accepted, audited, and queued as a scan by the admin.
	status, body, header, _ := e.do(t, http.MethodPost, tab+"/scan", nil, []*http.Cookie{admin.session}, map[string]string{
		dashboard.CSRFHeader: admin.csrf, "Content-Type": "application/json",
	})
	var soon struct {
		Error       string `json:"error"`
		WaitSeconds int    `json:"waitSeconds"`
	}
	if status != http.StatusTooManyRequests || json.Unmarshal(body, &soon) != nil || soon.WaitSeconds < 1 || soon.WaitSeconds > 300 ||
		header.Get("Retry-After") != strconv.Itoa(soon.WaitSeconds) || !strings.Contains(soon.Error, "try again in ") {
		t.Fatalf("Scan now right after a scan = %d: %s (Retry-After %q), want 429 with the wait", status, body, header.Get("Retry-After"))
	}
	env.SetNow(env.Now().Add(5*time.Minute + time.Second))
	status, body = e.write(t, admin, http.MethodPost, tab+"/scan", nil)
	var accepted struct {
		Scan *dashboard.ScanView `json:"scan"`
	}
	if status != http.StatusAccepted || json.Unmarshal(body, &accepted) != nil || accepted.Scan == nil ||
		accepted.Scan.State != string(store.ScanQueued) || accepted.Scan.StartedBy != "admin (password)" {
		t.Fatalf("Scan now after 5 minutes = %d: %s, want 202 with a queued scan by admin (password)", status, body)
	}
	var audit dashboard.AuditPageView
	e.getJSON(t, admin, "/api/audit", &audit)
	var audited int
	for _, a := range audit.Entries {
		if a.Action == "readiness_scan_requested" && a.Instance == testInstance && strings.EqualFold(a.Project, gadgetsPath) {
			audited++
		}
	}
	if audited != 1 {
		t.Errorf("%d readiness_scan_requested audit entries for acme/gadgets, want 1 (the switch-on scan is not audited)", audited)
	}
}

// checkScanJob checks the credentials of a scan Job: the clone token reaches
// the clone container only, the scan token and the skill-glob input the scan
// container only, neither is a literal in the Job, and no LLM key or write
// token is anywhere.
func checkScanJob(t *testing.T, job batchv1.Job, secret *corev1.Secret, scanToken string) {
	t.Helper()
	if job.Labels["app.kubernetes.io/managed-by"] != "pergent-sh" || job.Spec.Template.Labels["app.kubernetes.io/managed-by"] != "pergent-sh" {
		t.Errorf("the scan Job's labels = %v, pod labels = %v; want managed-by pergent-sh on both so the NetworkPolicy applies",
			job.Labels, job.Spec.Template.Labels)
	}
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || len(spec.Containers) != 1 {
		t.Fatalf("the scan Job has %d init and %d containers, want 1 and 1", len(spec.InitContainers), len(spec.Containers))
	}
	clone, scan := spec.InitContainers[0], spec.Containers[0]
	secretKeys := func(c corev1.Container) []string {
		var keys []string
		for _, v := range c.Env {
			if v.Value != "" && v.ValueFrom != nil {
				t.Errorf("env %s of %s has a value and a source", v.Name, c.Name)
			}
			if v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil {
				keys = append(keys, v.ValueFrom.SecretKeyRef.Key)
			}
		}
		return keys
	}
	if got := secretKeys(clone); len(got) != 1 || got[0] != "git-token" {
		t.Errorf("the clone container reads Secret keys %v, want git-token alone", got)
	}
	if got := secretKeys(scan); len(got) != 1 || got[0] != "scan-token" {
		t.Errorf("the scan container reads Secret keys %v, want scan-token alone", got)
	}
	for _, v := range spec.Volumes {
		if v.Secret != nil && (len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "scan-input") {
			t.Errorf("Secret volume %s mounts %+v, want the scan-input key alone", v.Name, v.Secret.Items)
		}
	}
	mounted := func(c corev1.Container, volume string) bool {
		for _, m := range c.VolumeMounts {
			if m.Name == volume {
				return true
			}
		}
		return false
	}
	if mounted(clone, "scan-input") || !mounted(scan, "scan-input") {
		t.Errorf("scan-input is mounted in the clone container: %v, in the scan container: %v; want the scan container only",
			mounted(clone, "scan-input"), mounted(scan, "scan-input"))
	}
	if secret.Data["scan-input"] == nil || strings.Contains(string(secret.Data["scan-input"]), "Look for security problems") {
		t.Errorf("the scan input is %q, want the skills' names and globs, never their text", secret.Data["scan-input"])
	}
	spent, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"the clone token": newDeployToken, "the scan token": scanToken, "the LLM key": testLLMKey} {
		if strings.Contains(string(spent), v) {
			t.Errorf("%s is in the scan Job's spec", name)
		}
		for key, data := range secret.Data {
			if name == "the LLM key" && strings.Contains(string(data), v) {
				t.Errorf("the LLM key is in the Secret key %s", key)
			}
		}
	}
	if scan.Resources.Limits.Memory().String() != "512Mi" || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 300 {
		t.Errorf("the scan Job's memory limit %s and deadline %v, want 512Mi and 300 s",
			scan.Resources.Limits.Memory(), job.Spec.ActiveDeadlineSeconds)
	}
}
