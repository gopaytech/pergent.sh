package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const sqlFingerprint = "abc123def456"

// findingJSON is a stored finding with the fingerprint sqlFingerprint.
func findingJSON(t *testing.T, severity string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(result.Finding{
		ID: "abc123", Fingerprint: sqlFingerprint, File: "db/query.go", Line: 12, Severity: severity,
		Title: "SQL built from user input", Explanation: "The id is concatenated into the query.",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// skillWithFinding is a skill with verdict, one reviewed file and one
// finding of severity.
func skillWithFinding(t *testing.T, verdict, severity string) result.Skill {
	s := reviewed(verdict, "db/query.go")
	s.Findings = []json.RawMessage{findingJSON(t, severity)}
	return s
}

func dismiss(t *testing.T, env *testEnv, iid int64, severity, reason string) {
	t.Helper()
	if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{
		MR: env.MRKey(iid), Fingerprint: sqlFingerprint, Severity: severity, Reason: reason, Actor: "maya (gitlab main)",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDismissalUnblocksApproval(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
	servePublish(env, 1, head)
	dismiss(t, env, 1, "blocking", "false positive, the id is an integer")

	publish(t, env)

	body := createdComment(t, env, 1)
	for _, want := range []string{
		"Dismissed (1)", "SQL built from user input", "`abc123`",
		"dismissed by maya (gitlab main): false positive, the id is an integer",
		"✅ **No blocking issues** (1 dismissed)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "❌") {
		t.Errorf("comment still shows a blocking issue:\n%s", body)
	}
	wantStatus(t, env, head, gitlab.StatusSuccess, "No blocking issues")
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 1 {
		t.Errorf("approve called %d times, want 1", n)
	}
	if got := recordedApproval(t, env, 1); got != head {
		t.Errorf("recorded approval = %q, want %s", got, head)
	}
	wantPublished(t, env, id)
}

func TestSuggestionDismissalDoesNotCoverBlocking(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
	servePublish(env, 1, head)
	dismiss(t, env, 1, "suggestion", "")

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusFailed, "Blocking issues found")
	if body := createdComment(t, env, 1); !strings.Contains(body, testMarkdown) || strings.Contains(body, "Dismissed") {
		t.Errorf("comment = %q, want the stored one, nothing dismissed", body)
	}
	wantNoApproval(t, env, 1)
}

// A dismissal that covers no finding of the review, or no dismissal at all,
// leaves the comment as the result carries it, notes included.
func TestNoDismissalsKeepsOriginalComment(t *testing.T) {
	for name, withUnrelated := range map[string]bool{"none": false, "unrelated": true} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			head := sha('a')
			env.Report(1, head, resultFor(t, head, skillWithFinding(t, "no_blocking_issues", "suggestion")))
			servePublish(env, 1, head)
			draft := openMR(1, head)
			draft.Draft = true
			env.GitLab.SetMR(draft)
			if withUnrelated {
				if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{
					MR: env.MRKey(1), Fingerprint: "ffffffffffff", Severity: "blocking", Reason: "x", Actor: "maya",
				}); err != nil {
					t.Fatal(err)
				}
			}

			publish(t, env)

			const block = "<!-- pergent:findings [] -->"
			want := policy.Marker + "\n### security\n\nNo blocking issues.\n\n" +
				"---\n**Not approved:** this is a draft. It will be approved when it's marked ready.\n\n" + block
			if got := createdComment(t, env, 1); got != want {
				t.Errorf("comment = %q\nwant %q", got, want)
			}
		})
	}
}

// A restored finding blocks again: the head's approval is withdrawn.
func TestRedecideWithdrawsApproval(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
	servePublish(env, 1, head)
	dismiss(t, env, 1, "blocking", "false positive")
	publish(t, env)
	wantPublished(t, env, id)
	if got := recordedApproval(t, env, 1); got != head {
		t.Fatalf("recorded approval = %q, want %s", got, head)
	}

	if ok, err := env.Store.Restore(env.ctx, env.MRKey(1), sqlFingerprint); err != nil || !ok {
		t.Fatalf("Restore = %v, %v", ok, err)
	}
	env.GitLab.Reset()
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, `{}`)
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	if ok, err := env.Store.Republish(env.ctx, env.MRKey(1), head, "main"); err != nil || !ok {
		t.Fatalf("Republish = %v, %v", ok, err)
	}

	publish(t, env)

	reqs := env.GitLab.Requests(http.MethodPost, unapprovePath(1))
	if len(reqs) != 1 || reqs[0].Token != testApproveToken {
		t.Fatalf("got %d unapprovals, want 1 by the approve bot", len(reqs))
	}
	if got := recordedApproval(t, env, 1); got != "" {
		t.Errorf("recorded approval = %q, want it cleared", got)
	}
	wantStatus(t, env, head, gitlab.StatusFailed, "Blocking issues found")
	wantPublished(t, env, id)
}

// A review that was never approved, or approved at another head, has nothing
// to withdraw.
func TestRedecideWithoutApprovalForThisHeadWithdrawsNothing(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
	servePublish(env, 1, head)
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), sha('z'), "main"); err != nil {
		t.Fatal(err)
	}

	publish(t, env)

	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 0 {
		t.Errorf("unapprove called %d times, want never", n)
	}
	if got := recordedApproval(t, env, 1); got != sha('z') {
		t.Errorf("recorded approval = %q, want the other head's kept", got)
	}
}

// A failed withdrawal is not hidden: the publish is retried.
func TestRedecideWithdrawalFailureRetriesPublish(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
	servePublish(env, 1, head)
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
		t.Fatal(err)
	}
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusInternalServerError, `{"message":"boom"}`)

	publish(t, env)

	if r := env.Run(id); r.State != store.Reported || r.Attempts != 1 {
		t.Errorf("run = %s after %d attempts (%q), want reported for a retry", r.State, r.Attempts, r.PublishError)
	}
	if got := recordedApproval(t, env, 1); got != head {
		t.Errorf("recorded approval = %q, want it kept until withdrawn", got)
	}
}

func TestFailedSkillNotUnblockedByDismissal(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	failed := skillWithFinding(t, "failed", "blocking")
	failed.Failures = []result.Failure{{Files: []string{"db/query.go"}, Message: "timed out"}}
	env.Report(1, head, resultFor(t, head, failed))
	servePublish(env, 1, head)
	dismiss(t, env, 1, "blocking", "false positive")

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusFailed, "Review failed for 1 of 1 skills")
	body := createdComment(t, env, 1)
	if !strings.Contains(body, "Dismissed (1)") || !strings.Contains(body, "timed out") {
		t.Errorf("comment = %s\nwant the dismissed finding and the failure", body)
	}
	wantNoApproval(t, env, 1)
}
