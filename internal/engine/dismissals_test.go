package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
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
		"### ✅ No blocking issues (1 dismissed)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	wantLayout(t, body)
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
	body := createdComment(t, env, 1)
	wantLayout(t, body)
	for _, want := range []string{"### ❌ 1 blocking issue", "SQL built from user input", "dismiss abc123"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q, the blocking finding still stands:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Dismissed") {
		t.Errorf("comment = %q, want nothing dismissed", body)
	}
	wantNoApproval(t, env, 1)
}

// A dismissal that covers no finding of the review, or no dismissal at all,
// leaves the comment as the review renders: findings in place, nothing
// dismissed, the draft note before the findings block.
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

			// The review as rendered, the draft note right before the findings
			// block, and nothing dismissed.
			got := createdComment(t, env, 1)
			wantLayout(t, got)
			wantNoteBeforeBlock(t, got, draftNote)
			for _, want := range []string{"### ✅ No blocking issues\n", "SQL built from user input", "1 suggestion"} {
				if !strings.Contains(got, want) {
					t.Errorf("comment lacks %q:\n%s", want, got)
				}
			}
			if strings.Contains(got, "Dismissed") {
				t.Errorf("comment shows a dismissal:\n%s", got)
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

// pending returns the recorded approvals marked for withdrawal, backoff
// aside.
func pending(t *testing.T, env *testEnv) []store.Approval {
	t.Helper()
	got, err := env.Store.PendingWithdrawals(env.ctx, env.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A withdrawal that fails doesn't fail the publish: the comment and status
// are out, the approval stays recorded as owed, and the approval sweep takes
// it off once the approve bot can.
func TestWithdrawalFailureIsRetriedBySweep(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
	servePublish(env, 1, head)
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
		t.Fatal(err)
	}
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)

	publish(t, env)

	wantPublished(t, env, id)
	wantStatus(t, env, head, gitlab.StatusFailed, "Blocking issues found")
	createdComment(t, env, 1)
	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
		t.Fatalf("got %d unapprovals, want 1 attempt", n)
	}
	if got := recordedApproval(t, env, 1); got != head {
		t.Fatalf("recorded approval = %q, want it kept until withdrawn", got)
	}
	if p := pending(t, env); len(p) != 1 || p[0].ApprovedSHA != head {
		t.Fatalf("pending withdrawals = %v, want the head's", p)
	}

	// Inside the backoff the sweep leaves it alone.
	sweep(t, env)
	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
		t.Errorf("got %d unapprovals inside the backoff, want still 1", n)
	}

	// Past it, a failing approve bot keeps the debt ...
	env.SetNow(env.Now().Add(time.Minute))
	sweep(t, env)
	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 2 {
		t.Fatalf("got %d unapprovals after the backoff, want 2", n)
	}
	if p := pending(t, env); len(p) != 1 {
		t.Fatalf("pending withdrawals = %v, want the debt kept", p)
	}

	// ... and a working one pays it.
	env.SetNow(env.Now().Add(time.Hour))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	sweep(t, env)
	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 3 {
		t.Errorf("got %d unapprovals, want 3", n)
	}
	if got := recordedApproval(t, env, 1); got != "" {
		t.Errorf("recorded approval = %q, want it cleared", got)
	}
	if p := pending(t, env); len(p) != 0 {
		t.Errorf("pending withdrawals = %v, want none", p)
	}
}

// A merged or gone change has nothing to withdraw: the record is cleared
// without a call, at publish time and by the sweep. A closed one could be
// reopened with the approval in place, so it is withdrawn as an open one is.
func TestWithdrawalOfMergedOrGoneClearsRecord(t *testing.T) {
	for _, state := range []string{"merged", "gone"} {
		setState := func(env *testEnv, head string) {
			if state == "gone" {
				env.GitLab.Reply(http.MethodGet, mrPath(1), http.StatusNotFound, `{"message":"404 Not found"}`)
				return
			}
			mr := openMR(1, head)
			mr.State = state
			env.GitLab.SetMR(mr)
		}
		t.Run("publish "+state, func(t *testing.T) {
			env := newTestEnv(t)
			head := sha('a')
			id := env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
			servePublish(env, 1, head)
			setState(env, head)
			if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
				t.Fatal(err)
			}
			env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

			publish(t, env)

			wantPublished(t, env, id)
			if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 0 {
				t.Errorf("unapprove called %d times for a %s change", n, state)
			}
			if got := recordedApproval(t, env, 1); got != "" {
				t.Errorf("recorded approval = %q, want it cleared", got)
			}
		})
		t.Run("sweep "+state, func(t *testing.T) {
			env := newTestEnv(t)
			head := sha('a')
			env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
			servePublish(env, 1, head)
			if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
				t.Fatal(err)
			}
			env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)
			publish(t, env)
			if p := pending(t, env); len(p) != 1 {
				t.Fatalf("pending withdrawals = %v, want one", p)
			}

			setState(env, head)
			env.SetNow(env.Now().Add(time.Hour))
			env.GitLab.Reset()
			sweep(t, env)

			if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 0 {
				t.Errorf("unapprove called %d times for a %s change", n, state)
			}
			if got := recordedApproval(t, env, 1); got != "" {
				t.Errorf("recorded approval = %q, want it cleared", got)
			}
			if p := pending(t, env); len(p) != 0 {
				t.Errorf("pending withdrawals = %v, want none", p)
			}
		})
	}
}

func TestWithdrawalOfClosedChangeStillUnapproves(t *testing.T) {
	closed := func(env *testEnv, head string) {
		mr := openMR(1, head)
		mr.State = "closed"
		env.GitLab.SetMR(mr)
	}
	t.Run("publish", func(t *testing.T) {
		env := newTestEnv(t)
		head := sha('a')
		id := env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
		servePublish(env, 1, head)
		closed(env, head)
		if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
			t.Fatal(err)
		}
		env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

		publish(t, env)

		wantPublished(t, env, id)
		if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
			t.Errorf("got %d unapprovals, want 1", n)
		}
		if got := recordedApproval(t, env, 1); got != "" {
			t.Errorf("recorded approval = %q, want it cleared", got)
		}
	})
	t.Run("sweep retries until it works", func(t *testing.T) {
		env := newTestEnv(t)
		head := sha('a')
		env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
		servePublish(env, 1, head)
		closed(env, head)
		if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
			t.Fatal(err)
		}
		env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)
		publish(t, env)
		if got := recordedApproval(t, env, 1); got != head {
			t.Fatalf("recorded approval = %q, want it kept after the failed withdrawal", got)
		}
		if p := pending(t, env); len(p) != 1 {
			t.Fatalf("pending withdrawals = %v, want one", p)
		}

		env.SetNow(env.Now().Add(time.Hour))
		env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
		env.GitLab.Reset()
		sweep(t, env)

		if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
			t.Errorf("got %d unapprovals, want 1", n)
		}
		if got := recordedApproval(t, env, 1); got != "" {
			t.Errorf("recorded approval = %q, want it cleared", got)
		}
		if p := pending(t, env); len(p) != 0 {
			t.Errorf("pending withdrawals = %v, want none", p)
		}
	})
}

// The change turned into a draft between the draft check and the approval:
// nothing is approved, and an approval of this head from before is withdrawn.
func TestHeldDraftWithdrawsExistingApproval(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	env.GitLab.Handle(http.MethodGet, mrPath(1), func(w http.ResponseWriter, _ *http.Request) {
		mr := openMR(1, head)
		mr.Draft = reads.Add(1) > 1 // ready for the draft check, a draft by the approval
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mr)
	})
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	publish(t, env)

	wantPublished(t, env, id)
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 0 {
		t.Errorf("approved %d times a change that is a draft", n)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
		t.Errorf("got %d unapprovals, want 1", n)
	}
	if got := recordedApproval(t, env, 1); got != "" {
		t.Errorf("recorded approval = %q, want it cleared", got)
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
