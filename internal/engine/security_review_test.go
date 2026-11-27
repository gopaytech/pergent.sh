package engine

import (
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
			{Fingerprint: "fp-dismissed", Severity: "low", Title: "Weak hash", File: "d.go", StartLine: 5, RuleID: "hash"},
		}})
	all, _, err := env.Store.SecurityFindings(env.ctx, testInstance, testProjectPath, store.SecurityFindingFilter{})
	if err != nil || len(all) != 3 {
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
	// the review Job's PERGENT_SECURITY_FINDINGS and the file in its ConfigMap.
	review := func(t *testing.T, iid int64, c byte) (string, string, string) {
		t.Helper()
		head := sha(c)
		id := env.Enqueue(iid, head)
		env.GitLab.SetMR(openMR(iid, head))
		env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
		serveChangedSince(env, "b.go", "d.go")
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

	t.Run("switch off", func(t *testing.T) {
		got, file, _ := review(t, 3, '1')
		if got != "" || file != "" {
			t.Errorf("env = %q, file = %q; want none with the switch off", got, file)
		}
	})
	setSwitch(t, true)
	t.Run("on lists the open findings in changed files only", func(t *testing.T) {
		got, file, _ := review(t, 4, '2')
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
	t.Run("a store error does not stop the review", func(t *testing.T) {
		execSQL(t, env, `ALTER TABLE security_findings RENAME TO security_findings_gone`)
		got, file, logs := review(t, 5, '3')
		if got != "" || file != "" {
			t.Errorf("env = %q, file = %q; want none after a read error", got, file)
		}
		if !strings.Contains(logs, "reviewing without the open security findings") {
			t.Errorf("no warning in the log:\n%s", logs)
		}
	})
}
