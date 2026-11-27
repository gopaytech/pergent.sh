package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// A review gets the repository's open security findings in the files it
// changes, only when the repository's switch is on; it never fails or waits
// for them, and never changes a finding.
func TestDispatchSecurityFindings(t *testing.T) {
	t.Parallel()
	env := newSecurityEnv(t)
	sc := secQueue(t, env, testProjectPath, testProjectID, 0)
	if _, ok, err := env.Store.ClaimSecurityScan(env.ctx, 1, env.Now(), env.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	secFinish(t, env, sc, store.SecurityResult{Status: store.SecurityComplete, SkillName: "security-review", SkillCommit: "v1",
		Findings: []store.SecurityFindingInput{
			{Fingerprint: "fp-in", Severity: "high", Title: "SQL injection", File: "b.go", StartLine: 3, RuleID: "injection"},
			{Fingerprint: "fp-out", Severity: "high", Title: "Hardcoded key", File: "elsewhere.go", StartLine: 9, RuleID: "secret"},
			{Fingerprint: "fp-old", Severity: "medium", Title: "Open redirect", File: "old.go", StartLine: 7, RuleID: "redirect"},
			{Fingerprint: "fp-dismissed", Severity: "low", Title: "Weak hash", File: "d.go", StartLine: 5, RuleID: "hash"},
		}})
	all, _, err := env.Store.SecurityFindings(env.ctx, testInstance, testProjectPath, store.SecurityFindingFilter{})
	if err != nil || len(all) != 4 {
		t.Fatalf("findings = %d, %v", len(all), err)
	}
	for _, f := range all {
		if f.Fingerprint == "fp-dismissed" {
			if ok, err := env.Store.DismissSecurityFinding(env.ctx, testInstance, testProjectPath, f.ID, "false_positive", "test data", "maya", env.Now()); err != nil || !ok {
				t.Fatalf("dismiss = %v, %v", ok, err)
			}
		}
	}
	before, _, _ := env.Store.SecurityFindings(env.ctx, testInstance, testProjectPath, store.SecurityFindingFilter{})

	// review dispatches merge request iid at a head of its own and returns
	// the review Job's PERGENT_SECURITY_FINDINGS, the file in its ConfigMap
	// and the log. The Job must exist: a problem never holds a review.
	review := func(t *testing.T, iid int64, c byte) (string, string, string) {
		t.Helper()
		head := sha(c)
		id := env.Enqueue(iid, head)
		env.GitLab.SetMR(openMR(iid, head))
		env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
		dispatch(t, env)
		if env.Job(id) == nil {
			t.Fatalf("no Job; the review was not dispatched\n%s", env.Logs())
		}
		return jobEnv(env, id)["PERGENT_SECURITY_FINDINGS"], env.ConfigMap(id).Data["security-findings.md"], env.Logs()
	}
	setSwitch := func(t *testing.T, on bool) {
		t.Helper()
		if err := env.Store.PutSecurityScanSettings(env.ctx, testInstance, testProjectPath, store.SecurityScanSettings{FindingsInReviews: on}, env.Now()); err != nil {
			t.Fatal(err)
		}
	}
	const warning = "reviewing without the open security findings: "
	serveChangedSince(env, "b.go", "d.go")

	// Findings are only read at dispatch, and nothing later (the stored
	// result, the publish) touches them. Review 3 is published before the
	// switch is on, so it is the earlier review of the incremental subtest.
	published := publishedReview(t, env, sha('1'), "0.5.0", nil)
	if env.Run(published).State != store.Published {
		t.Fatalf("run = %+v", env.Run(published))
	}

	t.Run("switch off", func(t *testing.T) {
		got, file, _ := review(t, 4, '4')
		if got != "" || file != "" {
			t.Errorf("env = %q, file = %q; want none with the switch off", got, file)
		}
	})
	setSwitch(t, true)
	t.Run("on lists the open findings in changed files only", func(t *testing.T) {
		got, file, _ := review(t, 5, '5')
		if got != "/in/security-findings.md" {
			t.Errorf("PERGENT_SECURITY_FINDINGS = %q", got)
		}
		if !strings.Contains(file, "SQL injection") || strings.Contains(file, "Hardcoded key") {
			t.Errorf("security-findings.md = %q; want the finding in b.go and none from elsewhere", file)
		}
		if strings.Contains(file, "Weak hash") {
			t.Errorf("security-findings.md = %q; a dismissed finding was sent", file)
		}
	})
	t.Run("an incremental review lists the whole change's files", func(t *testing.T) {
		// Since the earlier review only z.go changed; from the base, old.go
		// and b.go did too.
		env.GitLab.Handle(http.MethodGet, comparePath(), func(w http.ResponseWriter, r *http.Request) {
			files := []fileDiff{{OldPath: "old.go", NewPath: "old.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"}, {OldPath: "b.go", NewPath: "b.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
			if r.URL.Query().Get("from") == sha('1') {
				files = []fileDiff{{OldPath: "z.go", NewPath: "z.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"diffs": files})
		})
		defer serveChangedSince(env, "b.go", "d.go")
		head := sha('2')
		id := env.Enqueue(3, head)
		env.GitLab.SetMR(openMR(3, head))
		env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
		dispatch(t, env)
		if env.Job(id) == nil {
			t.Fatalf("no Job\n%s", env.Logs())
		}
		data := env.ConfigMap(id).Data
		if _, ok := data["carried.json"]; !ok {
			t.Fatalf("the review is not incremental; ConfigMap keys %v", data)
		}
		if f := data["security-findings.md"]; !strings.Contains(f, "Open redirect") || !strings.Contains(f, "SQL injection") {
			t.Errorf("security-findings.md = %q; want the findings of files changed earlier in the change", f)
		}
	})
	t.Run("a finding's state is unchanged", func(t *testing.T) {
		after, _, err := env.Store.SecurityFindings(env.ctx, testInstance, testProjectPath, store.SecurityFindingFilter{})
		if err != nil || len(after) != len(before) {
			t.Fatalf("findings = %d, %v", len(after), err)
		}
		for i := range after {
			if after[i].State != before[i].State || !after[i].UpdatedAt.Equal(before[i].UpdatedAt) {
				t.Errorf("finding %d changed: %+v -> %+v", after[i].ID, before[i], after[i])
			}
		}
	})
	// Each problem leaves the file out with a warning and the review runs.
	// The log is cumulative, so only what this review logged is read, and it
	// must hold this path's own warning.
	problem := func(t *testing.T, iid int64, c byte, why string) {
		t.Helper()
		mark := len(env.Logs())
		got, file, logs := review(t, iid, c)
		logs = logs[mark:]
		if got != "" || file != "" {
			t.Errorf("env = %q, file = %q; want none after a read error", got, file)
		}
		if !strings.Contains(logs, warning+why) {
			t.Errorf("no %q warning in the log:\n%s", warning+why, logs)
		}
	}
	t.Run("a changed-files error does not stop the review", func(t *testing.T) {
		env.GitLab.Reply(http.MethodGet, mergeBaseRoute(), http.StatusInternalServerError, `{"message":"down"}`)
		defer serveChangedSince(env, "b.go", "d.go")
		problem(t, 6, '6', "the changed files can't be read")
	})
	t.Run("a settings error does not stop the review", func(t *testing.T) {
		execSQL(t, env, `ALTER TABLE security_scan_settings RENAME TO security_scan_settings_gone`)
		defer execSQL(t, env, `ALTER TABLE security_scan_settings_gone RENAME TO security_scan_settings`)
		problem(t, 7, '7', "the settings can't be read")
	})
	t.Run("a store error does not stop the review", func(t *testing.T) {
		execSQL(t, env, `ALTER TABLE security_findings RENAME TO security_findings_gone`)
		problem(t, 8, '8', "they can't be read")
	})
}
