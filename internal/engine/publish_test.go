package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

const (
	testMarkdown = "### security\n\nNo blocking issues.\n\n<!-- pergent:findings [] -->"
	createdNote  = int64(555)
)

func publish(t *testing.T, env *testEnv) {
	t.Helper()
	if err := env.Engine.PublishOnce(env.ctx); err != nil {
		t.Fatalf("PublishOnce: %v", err)
	}
}

func cleanup(t *testing.T, env *testEnv) {
	t.Helper()
	if err := env.Engine.CleanupOnce(env.ctx); err != nil {
		t.Fatalf("CleanupOnce: %v", err)
	}
}

// reviewed is a skill with verdict that reviewed files.
func reviewed(verdict string, files ...string) result.Skill {
	return result.Skill{Name: "security", Verdict: verdict, Files: result.Files{Reviewed: files}}
}

// resultFor is a completed result of base sha('b') and head with skills.
func resultFor(t *testing.T, head string, skills ...result.Skill) []byte {
	t.Helper()
	b, err := json.Marshal(result.Result{
		SchemaVersion: result.SchemaVersion, Status: "completed", PergentVersion: "0.1.0",
		Base: sha('b'), Head: head, Skills: skills,
		Comment: result.Comment{Markdown: testMarkdown, FindingsBlock: "<!-- pergent:findings [] -->"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// servePublish routes everything a publish of merge request iid at head
// needs: no existing note, note creation, the commit status, the MR (opened
// at head) and approval.
func servePublish(env *testEnv, iid int64, head string) {
	env.GitLab.Reply(http.MethodGet, notesPath(iid), http.StatusOK, `[]`)
	env.GitLab.Reply(http.MethodPost, notesPath(iid), http.StatusCreated, map[string]any{"id": createdNote})
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	env.GitLab.SetMR(openMR(iid, head))
	env.GitLab.Reply(http.MethodPost, approvePath(iid), http.StatusCreated, `{}`)
}

// onlyStatus returns the one commit status set on head.
func onlyStatus(t *testing.T, env *testEnv, head string) map[string]any {
	t.Helper()
	reqs := env.GitLab.Requests(http.MethodPost, statusPath(head))
	if len(reqs) != 1 {
		t.Fatalf("got %d commit statuses on %s, want 1", len(reqs), head)
	}
	if reqs[0].Token != testReviewToken {
		t.Errorf("commit status set with token %q, want the review bot's", reqs[0].Token)
	}
	return reqs[0].JSON(t)
}

func wantStatus(t *testing.T, env *testEnv, head string, state gitlab.StatusState, description string) {
	t.Helper()
	got := onlyStatus(t, env, head)
	if got["state"] != string(state) || got["description"] != description || got["name"] != "pergent" {
		t.Errorf("commit status = %v, want state %q, description %q, name pergent", got, state, description)
	}
}

// createdComment returns the body of the one note created on iid.
func createdComment(t *testing.T, env *testEnv, iid int64) string {
	t.Helper()
	reqs := env.GitLab.Requests(http.MethodPost, notesPath(iid))
	if len(reqs) != 1 {
		t.Fatalf("got %d created notes, want 1", len(reqs))
	}
	if reqs[0].Token != testReviewToken {
		t.Errorf("note created with token %q, want the review bot's", reqs[0].Token)
	}
	body, _ := reqs[0].JSON(t)["body"].(string)
	if !strings.Contains(body, policy.Marker) {
		t.Errorf("comment lacks the marker: %q", body)
	}
	return body
}

// wantLayout checks a posted review comment is one rendered from a result:
// the marker first, exactly one findings block and it last, and the re-run
// command in the footer. It is the part of a comment every review has,
// whatever its findings.
func wantLayout(t *testing.T, body string) {
	t.Helper()
	const block = "<!-- pergent:findings "
	if !strings.HasPrefix(body, policy.Marker+"\n### ") {
		t.Errorf("comment does not start with the marker and a headline:\n%s", body)
	}
	i := strings.LastIndex(body, block)
	if i < 0 || strings.Count(body, block) != 1 {
		t.Errorf("comment has %d findings blocks, want 1:\n%s", strings.Count(body, block), body)
		return
	}
	if tail := strings.TrimRight(body[i:], "\n"); strings.Contains(tail, "\n") || !strings.HasSuffix(tail, " -->") {
		t.Errorf("findings block is not last:\n%s", body)
	}
	if !strings.Contains(body, "Run again: `@pergent-review review`") {
		t.Errorf("comment lacks the re-run command:\n%s", body)
	}
	if strings.Contains(body, testMarkdown) {
		t.Errorf("comment is the Job's own markdown, want one rendered from the result:\n%s", body)
	}
}

// draftNote is what a draft's comment says, as a block of its own.
const draftNote = "---\n**Not approved:** this is a draft. It will be approved when it's marked ready.\n\n"

// wantNoteBeforeBlock checks note is the last thing before the findings
// block of body.
func wantNoteBeforeBlock(t *testing.T, body, note string) {
	t.Helper()
	i := strings.LastIndex(body, "<!-- pergent:findings ")
	if i < len(note) || body[i-len(note):i] != note {
		t.Errorf("comment lacks %q right before the findings block:\n%s", note, body)
	}
}

func wantPublished(t *testing.T, env *testEnv, id string) {
	t.Helper()
	if r := env.Run(id); r.State != store.Published {
		t.Errorf("run state = %s (publish error %q), want published", r.State, r.PublishError)
	}
}

func wantNoteID(t *testing.T, env *testEnv, iid, want int64) {
	t.Helper()
	got, err := env.Store.NoteID(env.ctx, env.MRKey(iid))
	if err != nil || got != want {
		t.Errorf("NoteID = %d, %v; want %d", got, err, want)
	}
}

// recordedApproval returns the approval recorded for merge request iid, or
// "".
func recordedApproval(t *testing.T, env *testEnv, iid int64) string {
	t.Helper()
	all, err := env.Store.Approvals(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.MR == env.MRKey(iid) {
			return a.ApprovedSHA
		}
	}
	return ""
}

func wantNoApproval(t *testing.T, env *testEnv, iid int64) {
	t.Helper()
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(iid))); n != 0 {
		t.Errorf("approve called %d times, want never", n)
	}
	if got := recordedApproval(t, env, iid); got != "" {
		t.Errorf("approval recorded for %s, want none", got)
	}
}

func TestPublishApproves(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "api/handler.go")))
	servePublish(env, 1, head)

	publish(t, env)

	body := createdComment(t, env, 1)
	wantLayout(t, body)
	for _, want := range []string{"### ✅ No blocking issues\n", "`api/handler.go`"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	wantNoteID(t, env, 1, createdNote)
	wantStatus(t, env, head, gitlab.StatusSuccess, "No blocking issues")
	approvals := env.GitLab.Requests(http.MethodPost, approvePath(1))
	if len(approvals) != 1 {
		t.Fatalf("approve called %d times, want 1", len(approvals))
	}
	if approvals[0].Token != testApproveToken {
		t.Errorf("approved with token %q, want the approve bot's", approvals[0].Token)
	}
	if got := approvals[0].JSON(t)["sha"]; got != head {
		t.Errorf("approved sha = %v, want %s", got, head)
	}
	wantPublished(t, env, id)
	if got := recordedApproval(t, env, 1); got != head {
		t.Errorf("recorded approval = %q, want %s", got, head)
	}
}

func TestPublishUpdatesExistingNote(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
	servePublish(env, 1, head)
	if err := env.Store.SetNoteID(env.ctx, env.MRKey(1), 777); err != nil {
		t.Fatal(err)
	}
	env.GitLab.Reply(http.MethodPut, notePath(1, 777), http.StatusOK, `{"id":777}`)

	publish(t, env)

	puts := env.GitLab.Requests(http.MethodPut, notePath(1, 777))
	if len(puts) != 1 || puts[0].Token != testReviewToken {
		t.Fatalf("got %d updates of note 777, want 1 by the review bot", len(puts))
	}
	body, _ := puts[0].JSON(t)["body"].(string)
	wantLayout(t, body)
	for _, want := range []string{"### ❌ 1 blocking issue", "SQL built from user input", "`db/query.go`"} {
		if !strings.Contains(body, want) {
			t.Errorf("updated comment lacks %q:\n%s", want, body)
		}
	}
	if n := len(env.GitLab.Requests("", notesPath(1))); n != 0 {
		t.Errorf("listed or created notes %d times, want 0", n)
	}
	wantNoteID(t, env, 1, 777)
	wantPublished(t, env, id)
}

func TestPublishFindsExistingNote(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("has_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodGet, notesPath(1), http.StatusOK, []map[string]any{
		{"id": 880, "body": policy.Marker + "\nsomeone else", "author": map[string]any{"id": 5}},
		{"id": 888, "body": policy.Marker + "\nold review", "author": map[string]any{"id": 100}},
	})
	env.GitLab.Reply(http.MethodPut, notePath(1, 888), http.StatusOK, `{"id":888}`)

	publish(t, env)

	if n := len(env.GitLab.Requests(http.MethodPut, notePath(1, 888))); n != 1 {
		t.Errorf("got %d updates of the review bot's note, want 1", n)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(1))) + len(env.GitLab.Requests(http.MethodPut, notePath(1, 880))); n != 0 {
		t.Errorf("created a note or touched another author's: %d requests", n)
	}
	wantNoteID(t, env, 1, 888)
	wantPublished(t, env, id)
}

func TestPublishRecreatesDeletedNote(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("has_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	if err := env.Store.SetNoteID(env.ctx, env.MRKey(1), 777); err != nil {
		t.Fatal(err)
	}
	// PUT note 777 is not routed: the fake answers 404, as for a deleted note.

	publish(t, env)

	if n := len(env.GitLab.Requests(http.MethodPut, notePath(1, 777))); n != 1 {
		t.Errorf("tried to update the stored note %d times, want 1", n)
	}
	createdComment(t, env, 1)
	wantNoteID(t, env, 1, createdNote)
	wantPublished(t, env, id)
}

func TestPublishHeadMovedNoApprove(t *testing.T) {
	cases := map[string]struct {
		setup func(env *testEnv)
		// recorded is the approval left recorded. When GitLab refuses the
		// approval itself, the write-ahead record stays: unapproving the
		// head later is harmless, and clearing it could lose the record of
		// an older approval it replaced.
		recorded string
	}{
		"MR has a new head": {setup: func(env *testEnv) { env.GitLab.SetMR(openMR(1, sha('c'))) }},
		"approve says the head moved": {
			setup: func(env *testEnv) {
				env.GitLab.Reply(http.MethodPost, approvePath(1), http.StatusConflict, `{"message":"SHA does not match HEAD of source branch"}`)
			},
			recorded: sha('a'),
		},
		"MR was retargeted": {setup: func(env *testEnv) {
			mr := openMR(1, sha('a'))
			mr.TargetBranch = "develop"
			env.GitLab.SetMR(mr)
		}},
		"MR is merged": {setup: func(env *testEnv) {
			mr := openMR(1, sha('a'))
			mr.State = "merged"
			env.GitLab.SetMR(mr)
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			head := sha('a')
			id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
			servePublish(env, 1, head)
			tc.setup(env)

			publish(t, env)

			wantStatus(t, env, head, gitlab.StatusSuccess, "No blocking issues")
			wantPublished(t, env, id)
			if n := len(env.GitLab.Requests(http.MethodGet, approvalsPath(1))); n != 0 {
				t.Errorf("listed approvals %d times, want 0", n)
			}
			if got := recordedApproval(t, env, 1); got != tc.recorded {
				t.Errorf("recorded approval = %q, want %q", got, tc.recorded)
			}
		})
	}
}

// gitlabApprovals is a fake GitLab approval state for merge request 1: once
// a user approved, approving again gets 401, as GitLab answers.
type gitlabApprovals struct {
	mu sync.Mutex
	by []int64
	// onApprove, when set, runs inside the approve request before the reply.
	onApprove func()
}

func (a *gitlabApprovals) serve(env *testEnv, approver int64) {
	env.GitLab.Handle(http.MethodPost, approvePath(1), func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		already := slices.Contains(a.by, approver)
		if !already {
			a.by = append(a.by, approver)
		}
		hook := a.onApprove
		a.mu.Unlock()
		if hook != nil {
			hook()
		}
		w.Header().Set("Content-Type", "application/json")
		if already {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"401 Unauthorized"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	})
	env.GitLab.Handle(http.MethodGet, approvalsPath(1), func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		by := []map[string]any{}
		for _, id := range a.by {
			by = append(by, map[string]any{"user": map[string]any{"id": id}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"approved_by": by})
	})
}

// A shutdown right after the approval reached GitLab: the approval was
// recorded before it was made, so a stale approval can always be removed.
func TestPublishRecordsApprovalBeforeApproving(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	approvals := &gitlabApprovals{onApprove: cancel}
	approvals.serve(env, testApproveUserID)

	if err := env.Engine.PublishOnce(ctx); err != nil {
		t.Fatalf("PublishOnce: %v", err)
	}

	if r := env.Run(id); r.State != store.Reported || r.Attempts != 0 {
		t.Errorf("run state %s, attempts %d; want reported, 0 (a shutdown is not a failed attempt)", r.State, r.Attempts)
	}
	if got := recordedApproval(t, env, 1); got != head {
		t.Errorf("recorded approval = %q, want %s although the publish was cut short", got, head)
	}
}

// The retry after an approval that landed but wasn't followed by the rest of
// the publish: GitLab refuses the second approval, the bot is listed as an
// approver, and the run is published without a second comment.
func TestPublishRetryAfterApprovalLanded(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, `{}`)
	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	approvals := &gitlabApprovals{onApprove: cancel}
	approvals.serve(env, testApproveUserID)
	if err := env.Engine.PublishOnce(ctx); err != nil {
		t.Fatalf("PublishOnce: %v", err)
	}
	approvals.mu.Lock()
	approvals.onApprove = nil
	approvals.mu.Unlock()

	publish(t, env)

	wantPublished(t, env, id)
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(1))); n != 1 {
		t.Errorf("created %d notes, want 1 (the retry updates it)", n)
	}
	if n := len(env.GitLab.Requests(http.MethodPut, notePath(1, createdNote))); n != 1 {
		t.Errorf("updated the note %d times on retry, want 1", n)
	}
	if got := recordedApproval(t, env, 1); got != head {
		t.Errorf("recorded approval = %q, want %s", got, head)
	}
}

func TestPublishAlreadyApprovedByBot(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	approvals := &gitlabApprovals{by: []int64{31, testApproveUserID}}
	approvals.serve(env, testApproveUserID)

	publish(t, env)

	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 1 {
		t.Errorf("approve called %d times, want 1", n)
	}
	wantPublished(t, env, id)
	if got := recordedApproval(t, env, 1); got != head {
		t.Errorf("recorded approval = %q, want %s", got, head)
	}
}

func TestPublishApproveRefusedNotPublished(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodPost, approvePath(1), http.StatusUnauthorized, `{"message":"401 Unauthorized"}`)
	env.GitLab.Reply(http.MethodGet, approvalsPath(1), http.StatusOK, map[string]any{
		"approved_by": []map[string]any{{"user": map[string]any{"id": 31}}},
	})

	publish(t, env)

	r := env.Run(id)
	if r.State != store.Reported || r.Attempts != 1 || !strings.Contains(r.PublishError, "401") {
		t.Errorf("run state %s, attempts %d, error %q; want reported, 1, the 401", r.State, r.Attempts, r.PublishError)
	}
	// The approval was recorded ahead; unapproving later is harmless.
	if got := recordedApproval(t, env, 1); got != head {
		t.Errorf("recorded approval = %q, want %s (recorded before approving)", got, head)
	}
}

func TestPublishBlockingNoApprove(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("has_blocking_issues", "a.go")))
	servePublish(env, 1, head)

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusFailed, "Blocking issues found")
	wantPublished(t, env, id)
	wantNoApproval(t, env, 1)
}

func TestPublishNothingReviewedNoApprove(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	s := reviewed("no_blocking_issues")
	s.Files.Skipped = []result.Skipped{{Path: "go.sum", Rule: "lockfile"}}
	id := env.Report(1, head, resultFor(t, head, s))
	servePublish(env, 1, head)

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusSuccess, "Nothing to review: every changed file is skipped")
	if body := createdComment(t, env, 1); !strings.Contains(body, "`go.sum` (lockfile)") {
		t.Errorf("comment doesn't list the skipped file: %q", body)
	}
	wantPublished(t, env, id)
	wantNoApproval(t, env, 1)
}

func TestPublishNeutralMapsToSkipped(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Config.Repositories[0].FailureStatus = config.FailureStatusNeutral
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("failed")))
	servePublish(env, 1, head)

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusSkipped, "Review failed for 1 of 1 skills")
	wantPublished(t, env, id)
	wantNoApproval(t, env, 1)
}

func TestPublishFailureReasonComment(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Enqueue(1, head)
	reason := "the review Job ended without a result"
	if ok, err := env.Store.ReportFailure(env.ctx, id, reason, env.Now()); err != nil || !ok {
		t.Fatalf("ReportFailure = %v, %v", ok, err)
	}
	servePublish(env, 1, head)

	publish(t, env)

	if body := createdComment(t, env, 1); body != policy.ErrorComment(reason) {
		t.Errorf("comment = %q, want %q", body, policy.ErrorComment(reason))
	}
	wantStatus(t, env, head, gitlab.StatusFailed, "pergent couldn't review this MR")
	wantPublished(t, env, id)
	wantNoApproval(t, env, 1)
}

func TestPublishUnusableResult(t *testing.T) {
	cases := map[string]struct {
		result func(t *testing.T, head string) []byte
		want   string
	}{
		"not a result": {
			result: func(*testing.T, string) []byte { return []byte(`{"status":"completed"}`) },
			want:   "pergent sent a result pergent.sh couldn't use: ",
		},
		"another head": {
			result: func(t *testing.T, _ string) []byte {
				return resultFor(t, sha('9'), reviewed("no_blocking_issues", "a.go"))
			},
			want: "different commit",
		},
		"another base": {
			result: func(t *testing.T, head string) []byte {
				var r map[string]any
				if err := json.Unmarshal(resultFor(t, head, reviewed("no_blocking_issues", "a.go")), &r); err != nil {
					t.Fatal(err)
				}
				r["base"] = sha('8')
				b, _ := json.Marshal(r)
				return b
			},
			want: "different commit",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			head := sha('a')
			id := env.Report(1, head, tc.result(t, head))
			servePublish(env, 1, head)

			publish(t, env)

			body := createdComment(t, env, 1)
			if !strings.Contains(body, "pergent sent a result pergent.sh couldn't use: ") || !strings.Contains(body, tc.want) {
				t.Errorf("comment = %q, want the unusable-result reason with %q", body, tc.want)
			}
			wantStatus(t, env, head, gitlab.StatusFailed, "pergent couldn't review this MR")
			wantPublished(t, env, id)
			if n := len(env.GitLab.Requests(http.MethodGet, mrPath(1))); n != 0 {
				t.Errorf("looked up the MR %d times for an unusable result, want 0", n)
			}
			wantNoApproval(t, env, 1)
		})
	}
}

func TestPublishRepositoryGoneUsesFailed(t *testing.T) {
	env := newTestEnv(t)
	env.Engine.Config.Repositories[0].FailureStatus = config.FailureStatusNeutral
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("failed")))
	servePublish(env, 1, head)
	env.Engine.Config.Repositories = nil

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusFailed, "Review failed for 1 of 1 skills")
	wantPublished(t, env, id)
}

// stepLimited is a failed skill that ended on pergent's step limit, with the
// phrase pergent adds in its failure and in a note.
func stepLimited(name string) result.Skill {
	s := result.Skill{Name: name, Verdict: "failed", Files: result.Files{Reviewed: []string{name + ".go"}}}
	s.Failures = []result.Failure{{Files: []string{name + ".go"}, Message: "Reached the 40-step limit " + result.StepLimitAdvice}}
	s.Notes = []string{"Verification failed: Reached the 40-step limit " + result.StepLimitAdvice + "."}
	return s
}

// wantAdvice checks text holds advice in place of pergent's phrase.
func wantAdvice(t *testing.T, what, text, advice string) {
	t.Helper()
	if !strings.Contains(text, advice) || strings.Contains(text, result.StepLimitAdvice) {
		t.Errorf("%s does not hold %q in place of pergent's phrase:\n%s", what, advice, text)
	}
}

// The step-limit advice in a published result names where the limit is set
// for the repository: its settings in the dashboard, or pergent.sh's config.
// A repository switched off since the run was queued can only be switched on
// again in the dashboard.
func TestPublishReplacesStepLimitAdvice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, env *testEnv)
		advice string
	}{
		{"config repository", func(*testing.T, *testEnv) {}, result.AdviceConfig},
		{"dashboard repository", func(t *testing.T, env *testEnv) {
			env.Engine.Config.Repositories = nil
			dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: testProjectPath, CreatedBy: "alice"})
		}, result.AdviceDashboard},
		{"repository switched off", func(_ *testing.T, env *testEnv) {
			env.Engine.Config.Repositories = nil
		}, result.AdviceDashboard},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			head := sha('a')
			id := env.Report(1, head, resultFor(t, head, stepLimited("security")))
			servePublish(env, 1, head)
			tc.setup(t, env)

			publish(t, env)

			wantPublished(t, env, id)
			wantAdvice(t, "the comment", createdComment(t, env, 1), tc.advice)
			wantAdvice(t, "the stored result", string(env.Run(id).Result), tc.advice)
			if _, err := result.Parse(env.Run(id).Result); err != nil {
				t.Errorf("stored result does not parse: %v", err)
			}
		})
	}
}

// A re-run of some skills is published with the advice replaced in the skills
// kept from before too: their results may predate the upgrade.
func TestPublishReplacesStepLimitAdviceInKeptSkills(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	base := env.Publish(1, head, resultFor(t, head, stepLimited("security"), stepLimited("style")))
	id := rerunOf(t, env, head, store.TriggerRerunFailed, []string{"style"}, base)

	reportRerun(t, env, id, head, resultFor(t, head, stepLimited("style")))

	body := createdComment(t, env, 1)
	wantAdvice(t, "the comment", body, result.AdviceConfig)
	stored := env.Run(id).Result
	wantAdvice(t, "the stored result", string(stored), result.AdviceConfig)
	r, err := result.Parse(stored)
	if err != nil || !r.Merged || len(r.Skills) != 2 {
		t.Fatalf("stored result = %+v, %v; want security and style, merged", r, err)
	}
	for _, s := range r.Skills {
		if len(s.Notes) != 1 || !strings.Contains(s.Notes[0], result.AdviceConfig) {
			t.Errorf("notes of %s = %q, want the config advice", s.Name, s.Notes)
		}
	}
	// The earlier run's own result is not rewritten.
	if got := env.Run(base).Result; !strings.Contains(string(got), result.StepLimitAdvice) {
		t.Errorf("the base run's result was changed: %s", got)
	}
}

// A result without the phrase is stored and published as it came.
func TestPublishLeavesResultWithoutStepLimitAdvice(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	res := resultFor(t, head, reviewed("no_blocking_issues", "api/handler.go"))
	id := env.Report(1, head, res)
	servePublish(env, 1, head)

	publish(t, env)

	wantPublished(t, env, id)
	if got := env.Run(id).Result; !json.Valid(got) || canonicalJSON(t, got) != canonicalJSON(t, res) {
		t.Errorf("stored result = %s, want %s", got, res)
	}
}

func canonicalJSON(t *testing.T, data []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestPublishRetriesOnGitLabError(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	// The error body echoes the token, as a misbehaving proxy might.
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusForbidden, `{"message":"forbidden for `+testReviewToken+`"}`)

	publish(t, env)

	r := env.Run(id)
	if r.State != store.Reported || r.Attempts != 1 {
		t.Fatalf("run state %s, attempts %d; want reported, 1", r.State, r.Attempts)
	}
	if !r.NotBefore.Equal(env.Now().Add(10 * time.Second)) {
		t.Errorf("not_before = %v, want now + 10s", r.NotBefore)
	}
	if r.PublishError == "" || !strings.Contains(r.PublishError, "403") {
		t.Errorf("publish error = %q, want the GitLab error", r.PublishError)
	}
	for _, secret := range []string{testReviewToken, testApproveToken, testLLMKey} {
		if strings.Contains(r.PublishError, secret) || strings.Contains(env.Logs(), secret) {
			t.Errorf("a token reached the publish error or the log: %q", r.PublishError)
		}
	}
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 0 {
		t.Errorf("approved %d times after a failed publish", n)
	}

	// Not due yet: nothing happens.
	env.GitLab.Reset()
	env.SetNow(env.Now().Add(9 * time.Second))
	publish(t, env)
	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("made %d GitLab requests before the retry was due", n)
	}

	// Due and GitLab is back: the comment is updated in place, not duplicated.
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, `{}`)
	env.SetNow(env.Now().Add(time.Second))
	publish(t, env)
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(1))); n != 0 {
		t.Errorf("created %d more notes on retry, want an update", n)
	}
	wantStatus(t, env, head, gitlab.StatusSuccess, "No blocking issues")
	wantPublished(t, env, id)
	if got := recordedApproval(t, env, 1); got != head {
		t.Errorf("recorded approval = %q, want %s", got, head)
	}
}

func TestPublishBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		0: 10 * time.Second, 1: 20 * time.Second, 5: 320 * time.Second,
		6: 10 * time.Minute, 9: 10 * time.Minute, 64: 10 * time.Minute, -1: 10 * time.Second,
	} {
		if got := publishBackoff(attempts); got != want {
			t.Errorf("publishBackoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

func TestPublishOneFailureDoesNotStopOthers(t *testing.T) {
	env := newTestEnv(t)
	bad := env.Report(1, sha('a'), resultFor(t, sha('a'), reviewed("has_blocking_issues", "a.go")))
	good := env.Report(2, sha('c'), resultFor(t, sha('c'), reviewed("has_blocking_issues", "a.go")))
	servePublish(env, 2, sha('c'))
	// MR 1 has no routes: every request for it gets 404.

	publish(t, env)

	if r := env.Run(bad); r.State != store.Reported || r.Attempts != 1 {
		t.Errorf("failing run: state %s, attempts %d; want reported, 1", r.State, r.Attempts)
	}
	wantPublished(t, env, good)
}

// A policy status maps onto a forge status; anything pergent.sh doesn't know
// is failed, never success. (gitlabforge.TestState maps those onto GitLab's.)
func TestStatusState(t *testing.T) {
	for in, want := range map[policy.Status]forge.StatusState{
		policy.StatusSuccess: forge.StatusSuccess,
		policy.StatusFailed:  forge.StatusFailed,
		policy.StatusNeutral: forge.StatusNeutral,
		"":                   forge.StatusFailed,
		"pending":            forge.StatusFailed,
	} {
		if got := StatusState(in); got != want {
			t.Errorf("StatusState(%q) = %q, want %q", in, got, want)
		}
	}
}

// createJob puts a Job named name in the fake cluster.
func createJob(t *testing.T, env *testEnv, name string) {
	t.Helper()
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	if _, err := env.Kube.BatchV1().Jobs(testNamespace).Create(env.ctx, j, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupDeletesJobAndCancelsStatus(t *testing.T) {
	env := newTestEnv(t)
	old := sha('a')
	id := env.Dispatched(1, old)
	createJob(t, env, kube.JobName(id))
	env.Enqueue(1, sha('c')) // supersedes the running run
	env.GitLab.Reply(http.MethodPost, statusPath(old), http.StatusCreated, `{}`)

	cleanup(t, env)

	if env.Job(id) != nil {
		t.Errorf("the superseded run's Job still exists")
	}
	wantStatus(t, env, old, gitlab.StatusCanceled, "Superseded by a newer push")
	if !env.Run(id).CleanedUp {
		t.Errorf("run not marked cleaned up")
	}

	env.GitLab.Reset()
	cleanup(t, env)
	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("second cleanup made %d GitLab requests, want 0", n)
	}
}

// A full review of a head replaces its incremental run. The commit status is
// the new run's now, so the cleanup leaves it alone, and still deletes the Job
// and marks the run cleaned up.
func TestCleanUpKeepsStatusOfANewerRunOfTheHead(t *testing.T) {
	env := newTestEnv(t)
	base := env.Publish(1, sha('c'), resultFor(t, sha('c'), reviewed("no_blocking_issues", "a.go")))
	head := sha('a')
	id := env.Dispatched(1, head)
	createJob(t, env, kube.JobName(id))
	if err := env.Store.SetIncremental(env.ctx, id, &base); err != nil {
		t.Fatal(err)
	}
	full, created, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(1), testProjectPath, head, "main", store.TriggerRerun, nil, nil, true)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v; want a new run", created, err)
	}
	if env.Run(id).State != store.Superseded || env.Run(full).State != store.Queued {
		t.Fatalf("incremental run is %s, full review %s; want superseded and queued", env.Run(id).State, env.Run(full).State)
	}
	env.GitLab.Reset()

	cleanup(t, env)

	if env.Job(id) != nil {
		t.Errorf("the replaced run's Job still exists")
	}
	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("cleanup made %d GitLab requests, want no status set", n)
	}
	if !env.Run(id).CleanedUp {
		t.Errorf("run not marked cleaned up")
	}
}

// A superseded run's log is kept, redacted, before its Job is deleted; a
// log that can't be read doesn't stop the cleanup.
func TestCleanupKeepsSupersededRunLog(t *testing.T) {
	for _, readable := range []bool{true, false} {
		env := newTestEnv(t)
		const clone = "glpat-clone-token-superseded"
		if readable {
			serveLog(env, "cloning with "+clone+"\nreviewing\n")
		} else {
			env.Engine.Launcher.LogStream = func(context.Context, string, string, int64) (io.ReadCloser, error) {
				return nil, errors.New("container pergent is not available")
			}
		}
		old := sha('a')
		id := env.Dispatched(1, old)
		putJob(t, env, id, env.Now().Add(-time.Minute), nil, nil)
		putJobSecret(t, env, id, kube.Secrets{GitToken: clone, LLMKey: "llm-key-1", ResultToken: "result-token-1"})
		env.Enqueue(1, sha('c')) // supersedes the running run
		env.GitLab.Reply(http.MethodPost, statusPath(old), http.StatusCreated, `{}`)

		cleanup(t, env)

		text, found := storedLog(t, env, id)
		if readable && (!found || text != "cloning with [REDACTED]\nreviewing\n") {
			t.Errorf("stored log = %q, %v; want it redacted", text, found)
		}
		if !readable && found {
			t.Errorf("a log was stored though the read failed")
		}
		if env.Job(id) != nil || !env.Run(id).CleanedUp {
			t.Errorf("readable %v: Job left (%v) or run not cleaned up", readable, env.Job(id) != nil)
		}
	}
}

func TestCleanupNeverDispatchedRun(t *testing.T) {
	env := newTestEnv(t)
	id := env.Enqueue(1, sha('a'))
	env.Enqueue(1, sha('c'))
	deletes := 0
	env.Kube.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		deletes++
		return false, nil, nil
	})

	cleanup(t, env)

	if deletes != 0 {
		t.Errorf("deleted %d Jobs for a run that never had one", deletes)
	}
	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("made %d GitLab requests for a run that was never dispatched", n)
	}
	if !env.Run(id).CleanedUp {
		t.Errorf("run not marked cleaned up")
	}
}

func TestCleanupFailureLeavesRun(t *testing.T) {
	env := newTestEnv(t)
	old := sha('a')
	id := env.Dispatched(1, old)
	env.Enqueue(1, sha('c'))
	env.GitLab.Reply(http.MethodPost, statusPath(old), http.StatusForbidden, `{"message":"403 Forbidden"}`)

	cleanup(t, env)
	if env.Run(id).CleanedUp {
		t.Fatalf("run marked cleaned up although its status couldn't be set")
	}

	env.GitLab.Reply(http.MethodPost, statusPath(old), http.StatusCreated, `{}`)
	cleanup(t, env)
	if !env.Run(id).CleanedUp {
		t.Errorf("run not cleaned up on the next pass")
	}
}

func TestStaleApprovalUnapproved(t *testing.T) {
	env := newTestEnv(t)
	env.Publish(1, sha('a'), resultFor(t, sha('a'), reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), sha('a'), "main"); err != nil {
		t.Fatal(err)
	}
	env.Enqueue(1, sha('c'))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	cleanup(t, env)

	reqs := env.GitLab.Requests(http.MethodPost, unapprovePath(1))
	if len(reqs) != 1 || reqs[0].Token != testApproveToken {
		t.Fatalf("got %d unapprovals, want 1 by the approve bot", len(reqs))
	}
	stale, err := env.Store.StaleApprovals(env.ctx, env.Now())
	if err != nil || len(stale) != 0 {
		t.Errorf("StaleApprovals = %v, %v; want none", stale, err)
	}
}

// An approval made after the stale list was read is not cleared by it.
func TestStaleApprovalClearIsConditional(t *testing.T) {
	env := newTestEnv(t)
	env.Publish(1, sha('a'), resultFor(t, sha('a'), reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), sha('a'), "main"); err != nil {
		t.Fatal(err)
	}
	env.Enqueue(1, sha('c'))
	env.GitLab.Handle(http.MethodPost, unapprovePath(1), func(w http.ResponseWriter, _ *http.Request) {
		// The publisher records an approval of the new head meanwhile.
		if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), sha('c'), "main"); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
	})

	cleanup(t, env)

	if got := recordedApproval(t, env, 1); got != sha('c') {
		t.Errorf("recorded approval = %q, want the new head %s kept", got, sha('c'))
	}
}

func TestStaleApprovalFailureAndUnknownInstance(t *testing.T) {
	env := newTestEnv(t)
	env.Publish(1, sha('a'), resultFor(t, sha('a'), reviewed("no_blocking_issues", "a.go")))
	env.Publish(2, sha('a'), resultFor(t, sha('a'), reviewed("no_blocking_issues", "a.go")))
	for _, iid := range []int64{1, 2} {
		if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(iid), sha('a'), "main"); err != nil {
			t.Fatal(err)
		}
		env.Enqueue(iid, sha('c'))
	}
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	env.GitLab.Reply(http.MethodPost, unapprovePath(2), http.StatusCreated, `{}`)

	cleanup(t, env)

	// MR 1's is kept, and listed again once its backoff is over.
	env.SetNow(env.Now().Add(30 * time.Second))
	stale, err := env.Store.StaleApprovals(env.ctx, env.Now())
	if err != nil || len(stale) != 1 || stale[0].MR != env.MRKey(1) {
		t.Fatalf("StaleApprovals = %v, %v; want only MR 1's, kept for a later pass", stale, err)
	}

	// Without bots for the instance, the approval is left alone, quietly.
	env.Engine.Platforms = syncmap.New(map[string]Platform{})
	env.GitLab.Reset()
	before := len(env.Logs())
	cleanup(t, env)
	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("made %d GitLab requests without bots for the instance", n)
	}
	if logs := env.Logs()[before:]; strings.Contains(logs, "level=WARN") || strings.Contains(logs, "level=ERROR") {
		t.Errorf("an unknown instance's stale approval is logged above debug every pass:\n%s", logs)
	}
	if stale, _ := env.Store.StaleApprovals(env.ctx, env.Now()); len(stale) != 1 {
		t.Errorf("StaleApprovals = %v, want MR 1's still", stale)
	}
}

// A stale approval the approve bot can't remove is not tried on every pass:
// the next attempt waits 30 s × 2^(failures before), at most 5 minutes. The
// record is kept throughout, and a removal that works clears it.
func TestStaleApprovalUnapproveBackoff(t *testing.T) {
	env := newTestEnv(t)
	env.Publish(1, sha('a'), resultFor(t, sha('a'), reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), sha('a'), "main"); err != nil {
		t.Fatal(err)
	}
	env.Enqueue(1, sha('c'))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	attempts := func() int { return len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))) }
	start := env.Now()

	cleanup(t, env)
	if n := attempts(); n != 1 {
		t.Fatalf("first pass: %d unapprovals, want 1", n)
	}
	// The next pass, at once (a Wake) or on the 2 s tick, leaves it alone.
	cleanup(t, env)
	env.SetNow(start.Add(2 * time.Second))
	cleanup(t, env)
	if n := attempts(); n != 1 {
		t.Fatalf("passes inside the backoff: %d unapprovals, want still 1", n)
	}
	if !strings.Contains(env.Logs(), "retry_at") {
		t.Errorf("the failure's log line doesn't say when it is tried again:\n%s", env.Logs())
	}

	// Each failure doubles the wait, up to 5 minutes.
	at := start
	for i, wait := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute} {
		env.SetNow(at.Add(wait - time.Second))
		cleanup(t, env)
		if n := attempts(); n != i+1 {
			t.Fatalf("a second before backoff %d (%s) is over: %d unapprovals, want %d", i+1, wait, n, i+1)
		}
		at = at.Add(wait)
		env.SetNow(at)
		cleanup(t, env)
		if n := attempts(); n != i+2 {
			t.Fatalf("once backoff %d (%s) is over: %d unapprovals, want %d", i+1, wait, n, i+2)
		}
	}
	if got := recordedApproval(t, env, 1); got != sha('a') {
		t.Fatalf("recorded approval = %q after the failures, want %s kept", got, sha('a'))
	}

	// The 6th failure waits the cap too; then the removal works and the
	// record is cleared.
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	env.SetNow(at.Add(5*time.Minute - time.Second))
	cleanup(t, env)
	if n := attempts(); n != 6 {
		t.Fatalf("inside the capped backoff: %d unapprovals, want 6", n)
	}
	env.SetNow(at.Add(5 * time.Minute))
	cleanup(t, env)
	if n := attempts(); n != 7 {
		t.Fatalf("after the capped backoff: %d unapprovals, want 7", n)
	}
	if stale, err := env.Store.StaleApprovals(env.ctx, env.Now().Add(time.Hour)); err != nil || len(stale) != 0 {
		t.Errorf("StaleApprovals = %v, %v; want none once removed", stale, err)
	}
}

// A draft is reviewed and gets its comment and status but no approval. Marked
// ready, its published review is decided again: the comment loses the draft
// note, and the head is approved.
func TestPublishDraftThenReady(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	draft := openMR(1, head)
	draft.Draft = true
	env.GitLab.SetMR(draft)

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusSuccess, "No blocking issues; not approved: draft")
	body := createdComment(t, env, 1)
	wantLayout(t, body)
	wantNoteBeforeBlock(t, body, draftNote)
	wantPublished(t, env, id)
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 0 {
		t.Fatalf("approved a draft %d times", n)
	}

	// Marked ready: the webhook republishes the head's run.
	env.GitLab.SetMR(openMR(1, head))
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, `{}`)
	env.GitLab.Reset()
	if ok, err := env.Store.Republish(env.ctx, env.MRKey(1), head, "main"); err != nil || !ok {
		t.Fatalf("Republish = %v, %v", ok, err)
	}

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusSuccess, "No blocking issues")
	puts := env.GitLab.Requests(http.MethodPut, notePath(1, createdNote))
	if len(puts) != 1 {
		t.Fatalf("got %d note updates, want 1", len(puts))
	}
	ready, _ := puts[0].JSON(t)["body"].(string)
	wantLayout(t, ready)
	// The same review, without the draft note and nothing else changed.
	if want := strings.Replace(body, draftNote, "", 1); ready != want {
		t.Errorf("updated comment = %q, want the draft's comment without its note %q", ready, want)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(1))); n != 0 {
		t.Errorf("created %d notes, want the existing one updated", n)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 1 {
		t.Errorf("approve called %d times, want 1", n)
	}
	wantPublished(t, env, id)
	if got := recordedApproval(t, env, 1); got != head {
		t.Errorf("recorded approval = %q, want %s", got, head)
	}
}

// A draft with blocking findings is failed as before, and the MR is never
// read for the draft flag.
func TestPublishDraftBlockingFails(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("has_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	draft := openMR(1, head)
	draft.Draft = true
	env.GitLab.SetMR(draft)

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusFailed, "Blocking issues found")
	if body := createdComment(t, env, 1); strings.Contains(body, "draft") {
		t.Errorf("comment has the draft note: %q", body)
	}
	wantPublished(t, env, id)
	wantNoApproval(t, env, 1)
}

// The MR can't be read for the draft flag: nothing is posted, and the run is
// retried, as for any GitLab error.
func TestPublishDraftFlagUnreadableRetries(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodGet, mrPath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)

	publish(t, env)

	if r := env.Run(id); r.State != store.Reported || r.Attempts != 1 {
		t.Errorf("run state %s, attempts %d; want reported, 1", r.State, r.Attempts)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(1))) + len(env.GitLab.Requests(http.MethodPost, statusPath(head))); n != 0 {
		t.Errorf("posted %d comments or statuses without knowing whether the MR is a draft", n)
	}
	wantNoApproval(t, env, 1)
}

// A platform whose approve side can be missing (GitHub, where the approve App
// may not be installed): a project stored without it is reviewed and gets
// its status, but isn't approved.
func TestPublishApproveUnavailable(t *testing.T) {
	env := newTestEnv(t)
	plat, _ := env.Engine.Platforms.Get(testInstance)
	plat.Kind, plat.GitLab = config.PlatformGitHub, nil // not a GitLab instance
	env.Engine.Platforms.Set(testInstance, plat)
	env.SeedProject(func(p *store.Project) { p.InstallationID, p.ApproveAvailable = 99, false })
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusSuccess, "No blocking issues; not approved: the pergent approve App isn't installed on this repository or lacks Pull requests: write")
	if body := createdComment(t, env, 1); !strings.Contains(body, "**Not approved:** the pergent approve App isn't installed on this repository or lacks Pull requests: write.") {
		t.Errorf("comment lacks the note: %q", body)
	}
	wantPublished(t, env, id)
	wantNoApproval(t, env, 1)
}

// On GitLab the approve bot is a user that can always approve: the stored
// flag is never read as missing.
func TestPublishGitLabIgnoresApproveAvailable(t *testing.T) {
	env := newTestEnv(t)
	env.SeedProject(func(p *store.Project) { p.InstallationID, p.ApproveAvailable = 99, false })
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)

	publish(t, env)

	wantStatus(t, env, head, gitlab.StatusSuccess, "No blocking issues")
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 1 {
		t.Errorf("approve called %d times, want 1", n)
	}
	wantPublished(t, env, id)
}

// The draft is marked ready while its review is being published: after the
// draft flag was read and before the run is marked published. The ready
// webhook's Republish finds the run reported; the publish still ends, and the
// next pass decides again and approves.
func TestPublishReadyDuringPublish(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	draft := openMR(1, head)
	draft.Draft = true
	env.GitLab.SetMR(draft)
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, `{}`)
	var once sync.Once
	env.GitLab.Handle(http.MethodPost, statusPath(head), func(w http.ResponseWriter, _ *http.Request) {
		// The draft flag was read; the comment is out. Marked ready now.
		once.Do(func() {
			env.GitLab.SetMR(openMR(1, head))
			if ok, err := env.Store.Republish(env.ctx, env.MRKey(1), head, "main"); err != nil || !ok {
				t.Errorf("Republish during the publish = %v, %v; want true", ok, err)
			}
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	})

	publish(t, env)

	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 0 {
		t.Fatalf("approved %d times on the draft decision", n)
	}
	if r := env.Run(id); r.State != store.Reported || r.Attempts != 0 {
		t.Fatalf("run state %s, attempts %d; want reported, 0 for another decision", r.State, r.Attempts)
	}

	publish(t, env)

	wantPublished(t, env, id)
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 1 {
		t.Errorf("approve called %d times, want 1", n)
	}
	puts := env.GitLab.Requests(http.MethodPut, notePath(1, createdNote))
	if len(puts) != 1 {
		t.Fatalf("got %d note updates, want 1", len(puts))
	}
	if body, _ := puts[0].JSON(t)["body"].(string); strings.Contains(body, "draft") {
		t.Errorf("comment still has the draft note: %q", body)
	}
	if got := recordedApproval(t, env, 1); got != head {
		t.Errorf("recorded approval = %q, want %s", got, head)
	}
}

// publishUntilGivenUp runs the publisher 10 times, each when the run's retry
// is due, so the run that never posts becomes publish_failed.
func publishUntilGivenUp(t *testing.T, env *testEnv, id string) {
	t.Helper()
	for i := 0; i < 10; i++ {
		publish(t, env)
		env.SetNow(env.Now().Add(11 * time.Minute))
	}
	if r := env.Run(id); r.State != store.PublishFailed {
		t.Fatalf("run state = %s after 10 failed publishes, want publish_failed", r.State)
	}
}

// A run that can't be posted gives up after its 10th attempt; its head then
// gets one final failed status, so a required check isn't left running, and
// is never approved.
func TestPublishFailedSetsFinalStatusOnce(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusUnprocessableEntity, `{"message":"Note is too long"}`)

	publishUntilGivenUp(t, env, id)
	wantStatus(t, env, head, gitlab.StatusFailed, "pergent couldn't post its review")
	wantNoApproval(t, env, 1)

	// It is never posted again.
	for i := 0; i < 3; i++ {
		publish(t, env)
		env.SetNow(env.Now().Add(time.Hour))
	}
	if n := len(env.GitLab.Requests(http.MethodPost, statusPath(head))); n != 1 {
		t.Errorf("%d commit statuses, want the one final status", n)
	}
	for _, secret := range []string{testReviewToken, testApproveToken, testLLMKey} {
		if strings.Contains(env.Logs(), secret) {
			t.Error("a token reached the log")
		}
	}
}

// The final status is best effort: when it can't be set either, the run stays
// publish_failed and nothing is tried again.
func TestPublishFailedFinalStatusRefused(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusForbidden, `{"message":"403 Forbidden"}`)

	publishUntilGivenUp(t, env, id)
	publish(t, env)
	if n := len(env.GitLab.Requests(http.MethodPost, statusPath(head))); n != 1 {
		t.Errorf("%d commit status attempts, want 1", n)
	}
	if !strings.Contains(env.Logs(), "couldn't set the final status") {
		t.Errorf("the refused final status isn't logged:\n%s", env.Logs())
	}
}

// A same-head re-run supersedes the published run that approved the head.
// When the re-run can never be posted, the approval of that commit must not
// stand: the run that gives up withdraws it, through the owed-withdrawal path
// the approval sweep keeps trying.
func TestPublishGiveUpWithdrawsApprovalOfItsHead(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	// R1 approved the head with its blocking finding dismissed.
	env.Publish(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking")))
	dismiss(t, env, 1, "blocking", "false positive")
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
		t.Fatal(err)
	}
	// R2, a re-run of the same head, supersedes R1 and is reported.
	r2, created, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(1), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}
	if got := env.Report(1, head, resultFor(t, head, skillWithFinding(t, "has_blocking_issues", "blocking"))); got != r2 {
		t.Fatalf("reported run %s, want the re-run %s", got, r2)
	}
	// The dismissal is restored while R2 is in flight: R1 is superseded, so
	// nothing re-decides it, and R2's publish is expected to.
	if ok, err := env.Store.Restore(env.ctx, env.MRKey(1), sqlFingerprint); err != nil || !ok {
		t.Fatalf("Restore = %v, %v", ok, err)
	}
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusUnprocessableEntity, `{"message":"Note is too long"}`)
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)

	publishUntilGivenUp(t, env, r2)

	reqs := env.GitLab.Requests(http.MethodPost, unapprovePath(1))
	if len(reqs) != 1 || reqs[0].Token != testApproveToken {
		t.Fatalf("got %d unapprovals, want 1 attempt by the approve bot on giving up", len(reqs))
	}
	if got := recordedApproval(t, env, 1); got != head {
		t.Fatalf("recorded approval = %q, want it kept as owed until withdrawn", got)
	}
	if p := pending(t, env); len(p) != 1 || p[0].ApprovedSHA != head {
		t.Fatalf("pending withdrawals = %v, want the head's", p)
	}

	// A later sweep with a working approver pays it.
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	env.SetNow(env.Now().Add(time.Hour))
	sweep(t, env)
	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 2 {
		t.Errorf("got %d unapprovals, want 2", n)
	}
	if got := recordedApproval(t, env, 1); got != "" {
		t.Errorf("recorded approval = %q, want it cleared", got)
	}
	if p := pending(t, env); len(p) != 0 {
		t.Errorf("pending withdrawals = %v, want none", p)
	}
}

// With a working approver the approval goes at the moment of giving up.
func TestPublishGiveUpWithdrawsApprovalNow(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusUnprocessableEntity, `{"message":"Note is too long"}`)
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
		t.Fatal(err)
	}

	publishUntilGivenUp(t, env, id)

	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
		t.Errorf("got %d unapprovals, want 1", n)
	}
	if got := recordedApproval(t, env, 1); got != "" {
		t.Errorf("recorded approval = %q, want it cleared", got)
	}
}

// A run that gives up is not the approved head's: its approval is not its to
// withdraw.
func TestPublishGiveUpKeepsApprovalOfAnotherHead(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusUnprocessableEntity, `{"message":"Note is too long"}`)
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), sha('z'), "main"); err != nil {
		t.Fatal(err)
	}

	publishUntilGivenUp(t, env, id)

	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 0 {
		t.Errorf("unapprove called %d times, want never", n)
	}
	if got := recordedApproval(t, env, 1); got != sha('z') {
		t.Errorf("recorded approval = %q, want the other head's kept", got)
	}
	if p := pending(t, env); len(p) != 0 {
		t.Errorf("pending withdrawals = %v, want none", p)
	}
}

// A merged change has nothing to withdraw: at give-up the record is only
// cleared.
func TestPublishGiveUpMergedClearsRecord(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusUnprocessableEntity, `{"message":"Note is too long"}`)
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	mr := openMR(1, head)
	mr.State = "merged"
	env.GitLab.SetMR(mr)
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
		t.Fatal(err)
	}

	publishUntilGivenUp(t, env, id)

	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 0 {
		t.Errorf("unapprove called %d times for a merged change", n)
	}
	if got := recordedApproval(t, env, 1); got != "" {
		t.Errorf("recorded approval = %q, want it cleared", got)
	}
}

// The withdrawal a given-up run owes is recorded with the give-up itself, and
// neither a final status that hangs nor the platform being slow can lose it:
// the approval sweep takes it off once the approve bot can.
func TestPublishGiveUpWithdrawalSurvivesAHangingFinalStatus(t *testing.T) {
	defer func(d time.Duration) { settleTimeout = d }(settleTimeout)
	settleTimeout = 300 * time.Millisecond

	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.GitLab.Reply(http.MethodPost, notesPath(1), http.StatusUnprocessableEntity, `{"message":"Note is too long"}`)
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), head, "main"); err != nil {
		t.Fatal(err)
	}
	// The final status takes longer than the settle timeout.
	env.GitLab.Handle(http.MethodPost, statusPath(head), func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusCreated)
	})

	publishUntilGivenUp(t, env, id)

	if p := pending(t, env); len(p) != 1 || p[0].ApprovedSHA != head {
		t.Fatalf("pending withdrawals = %v, want the head's", p)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
		t.Errorf("got %d unapprovals at give-up, want 1 attempt (the status hang must not starve it)", n)
	}

	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	env.SetNow(env.Now().Add(time.Hour))
	sweep(t, env)
	if got := recordedApproval(t, env, 1); got != "" {
		t.Errorf("recorded approval = %q, want it withdrawn by the sweep", got)
	}
}

// A merge request whose newest run gave up publishing never keeps the bot's
// approval of that commit, even when the debt was not recorded (a crash
// between the give-up and its bookkeeping, or a row from an older version);
// an approval of another head is not touched.
func TestSweepWithdrawsApprovalOfAPublishFailedRun(t *testing.T) {
	for name, tc := range map[string]struct {
		approved string
		want     string
	}{
		"same head":    {sha('a'), ""},
		"another head": {sha('z'), sha('z')},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			head := sha('a')
			id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
			servePublish(env, 1, head)
			env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
			for i := 0; i < 10; i++ {
				if _, err := env.Store.PublishRetry(env.ctx, id, "gitlab 502", env.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if tc.approved != head {
				// The merge request is at the approved head, so the stale
				// approval rule has nothing to say.
				env.GitLab.SetMR(openMR(1, tc.approved))
			}
			// Recorded after the give-up: no debt is marked.
			if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(1), tc.approved, "main"); err != nil {
				t.Fatal(err)
			}

			sweep(t, env)

			if got := recordedApproval(t, env, 1); got != tc.want {
				t.Errorf("recorded approval = %q, want %q", got, tc.want)
			}
			wantUnapprovals := 0
			if tc.want == "" {
				wantUnapprovals = 1
			}
			if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != wantUnapprovals {
				t.Errorf("got %d unapprovals, want %d", n, wantUnapprovals)
			}
		})
	}
}

// reportIncremental reports result for an incremental run of head on merge
// request 1 that builds on the run base, as a dispatch records it, and returns
// its id.
func reportIncremental(t *testing.T, env *testEnv, head, base string, res []byte) string {
	t.Helper()
	id := env.Dispatched(1, head)
	if err := env.Store.SetIncremental(env.ctx, id, &base); err != nil {
		t.Fatal(err)
	}
	if out, err := env.Store.StoreResult(env.ctx, id, "seed-token", res, env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	return id
}

// partialReview is the skill of an incremental review that reviewed reviewedFiles
// again and carried over carried, with findings.
func partialReview(reviewedFiles, carried []string, findings ...json.RawMessage) result.Skill {
	s := reviewed("no_blocking_issues", reviewedFiles...)
	s.Files.Carried = carried
	s.Findings = findings
	if len(reviewedFiles) > 0 {
		s.Batches = 1
	}
	return s
}

// wantRestarted checks the incremental run id of head was not published, and a
// full re-run of the head was queued in its place.
func wantRestarted(t *testing.T, env *testEnv, id, head string) {
	t.Helper()
	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(1))); n != 0 {
		t.Errorf("%d comments posted, want none", n)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, statusPath(head))); n != 0 {
		t.Errorf("%d commit statuses set, want none", n)
	}
	wantNoApproval(t, env, 1)
	if r := env.Run(id); r.State != store.Superseded {
		t.Errorf("run state = %s, want superseded", r.State)
	}
	queued, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("queued %d runs, want 1", len(queued))
	}
	if r := queued[0]; r.HeadSHA != head || r.Trigger != store.TriggerRerun || r.SkillSubset != nil || r.BaseRunID != nil || r.Incremental {
		t.Errorf("run = head %s %q %v %v incremental %v, want a full re-run of the head", r.HeadSHA, r.Trigger, r.SkillSubset, r.BaseRunID, r.Incremental)
	}
}

func TestIncrementalMergedBeforePublish(t *testing.T) {
	const kept, fresh, gone = "aaaaaa000001", "bbbbbb000001", "cccccc000001"
	env := newTestEnv(t)
	headA, headB := sha('a'), sha('c')
	last := reviewed("no_blocking_issues", "a.go", "b.go")
	last.Findings = []json.RawMessage{
		finding(kept, "a.go", 3, 0, "suggestion", ""),
		finding(gone, "b.go", 4, 0, "suggestion", ""), // b.go is reviewed again, and this is not reported
	}
	base := env.Publish(1, headA, resultFor(t, headA, last))
	partial := partialReview([]string{"b.go"}, []string{"a.go"}, finding(fresh, "b.go", 5, 0, "suggestion", ""))
	id := reportIncremental(t, env, headB, base, resultFor(t, headB, partial))
	servePublish(env, 1, headB)

	publish(t, env)

	body := createdComment(t, env, 1)
	wantLayout(t, body)
	for _, want := range []string{"Title of aaa", "Title of bbb"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Title of ccc") {
		t.Errorf("comment has the finding of a file reviewed again:\n%s", body)
	}
	wantStatus(t, env, headB, gitlab.StatusSuccess, "No blocking issues")
	wantPublished(t, env, id)

	r, err := result.Parse(env.Run(id).Result)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Merged || r.Incremental == nil || *r.Incremental != (result.Incremental{Reviewed: 1, Carried: 1, From: headA}) || len(r.Skills) != 1 {
		t.Fatalf("stored result = %+v, want the merged result of an incremental review from %s", r, headA)
	}
	if got := r.Skills[0].Files.Reviewed; !slices.Equal(got, []string{"b.go", "a.go"}) {
		t.Errorf("reviewed = %v, want the new files, then the carried", got)
	}
	// The next run's previous result is the whole review.
	next, found, err := env.Store.LastReview(env.ctx, env.MRKey(1), "none")
	if err != nil || !found || next.ID != id {
		t.Fatalf("LastReview = %s, %v, %v; want %s", next.ID, found, err, id)
	}
	for _, fp := range []string{kept, fresh} {
		if !strings.Contains(string(next.Result), fp) {
			t.Errorf("the previous result lacks %s: %s", fp, next.Result)
		}
	}
	if strings.Contains(string(next.Result), gone) {
		t.Errorf("the previous result has %s, which was not carried: %s", gone, next.Result)
	}
}

// The merge is done once: publishing the run again works on the merged result,
// and the run is not replaced by a full review.
func TestIncrementalMergesOnce(t *testing.T) {
	env := newTestEnv(t)
	headA, headB := sha('a'), sha('c')
	base := env.Publish(1, headA, resultFor(t, headA, reviewed("no_blocking_issues", "a.go", "b.go")))
	id := reportIncremental(t, env, headB, base, resultFor(t, headB, partialReview([]string{"b.go"}, []string{"a.go"})))
	servePublish(env, 1, headB)
	publish(t, env)
	before := string(env.Run(id).Result)

	if ok, err := env.Store.Republish(env.ctx, env.MRKey(1), headB, "main"); err != nil || !ok {
		t.Fatalf("Republish = %v, %v", ok, err)
	}
	env.GitLab.Reset()
	env.GitLab.Reply(http.MethodPut, notePath(1, createdNote), http.StatusOK, `{}`)
	servePublish(env, 1, headB)
	publish(t, env)

	if after := string(env.Run(id).Result); after != before {
		t.Errorf("the merged result changed on the second publish:\n%s\n%s", before, after)
	}
	wantPublished(t, env, id)
	if queued, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(time.Hour)); err != nil || len(queued) != 0 {
		t.Errorf("queued %+v, %v; want no run", queued, err)
	}
}

// An incremental review that can't be completed with the last one is not
// published: a full review of the head takes its place.
func TestIncrementalBaseGoneRestartsFull(t *testing.T) {
	headA, headB := sha('a'), sha('c')
	for name, tc := range map[string]struct {
		base    []byte
		partial result.Skill
	}{
		"the base's result does not parse": {
			base:    []byte(`{"status":"completed"}`),
			partial: partialReview([]string{"b.go"}, []string{"a.go"}),
		},
		"the base is for another commit": {
			base:    resultFor(t, sha('9'), reviewed("no_blocking_issues", "a.go", "b.go")),
			partial: partialReview([]string{"b.go"}, []string{"a.go"}),
		},
		"the base did not review a carried file": {
			base:    resultFor(t, headA, reviewed("no_blocking_issues", "b.go")),
			partial: partialReview([]string{"b.go"}, []string{"a.go"}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			base := env.Publish(1, headA, tc.base)
			id := reportIncremental(t, env, headB, base, resultFor(t, headB, tc.partial))
			servePublish(env, 1, headB)

			publish(t, env)

			wantRestarted(t, env, id, headB)
			if !strings.Contains(env.Logs(), "reviewing everything instead") {
				t.Errorf("the restart was not logged:\n%s", env.Logs())
			}
		})
	}

	// The run lost its base run: the foreign key sets base_run_id to NULL
	// when the earlier run is pruned. The store has no way to prune one, so
	// the run is read, and the base taken off it, as the publisher would.
	t.Run("the run has no base run", func(t *testing.T) {
		env := newTestEnv(t)
		base := env.Publish(1, headA, resultFor(t, headA, reviewed("no_blocking_issues", "a.go", "b.go")))
		id := reportIncremental(t, env, headB, base, resultFor(t, headB, partialReview([]string{"b.go"}, []string{"a.go"})))
		servePublish(env, 1, headB)
		run := env.Run(id)
		run.BaseRunID = nil

		_, restarted, err := env.Engine.mergeBase(env.ctx, run, env.Engine.log())

		if err != nil || !restarted {
			t.Fatalf("mergeBase = restarted %v, %v; want restarted", restarted, err)
		}
		wantRestarted(t, env, id, headB)
	})
}

// A partial with every file carried over reviews nothing again: the last
// review's verdict, status and approval stand.
func TestIncrementalNothingToReview(t *testing.T) {
	headA, headB := sha('a'), sha('c')
	for name, tc := range map[string]struct {
		verdict  string
		findings []json.RawMessage
		status   gitlab.StatusState
		desc     string
		approved bool
	}{
		"clean": {
			verdict: "no_blocking_issues", status: gitlab.StatusSuccess, desc: "No blocking issues", approved: true,
		},
		"blocking": {
			verdict:  "has_blocking_issues",
			findings: []json.RawMessage{finding("aaaaaa000001", "a.go", 3, 0, "blocking", "")},
			status:   gitlab.StatusFailed, desc: "Blocking issues found",
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			last := reviewed(tc.verdict, "a.go", "b.go")
			last.Findings = tc.findings
			base := env.Publish(1, headA, resultFor(t, headA, last))
			// The skill started no run: it reports no batches and, whatever
			// it says of the verdict, the carried findings decide it.
			partial := partialReview(nil, []string{"a.go", "b.go"})
			id := reportIncremental(t, env, headB, base, resultFor(t, headB, partial))
			servePublish(env, 1, headB)

			publish(t, env)

			wantStatus(t, env, headB, tc.status, tc.desc)
			wantPublished(t, env, id)
			if tc.approved {
				if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 1 {
					t.Errorf("approve called %d times, want 1", n)
				}
				if got := recordedApproval(t, env, 1); got != headB {
					t.Errorf("recorded approval = %q, want %s", got, headB)
				}
			} else {
				wantNoApproval(t, env, 1)
			}
			body := createdComment(t, env, 1)
			wantLayout(t, body)
			if len(tc.findings) > 0 && !strings.Contains(body, "Title of aaa") {
				t.Errorf("comment lacks the carried finding:\n%s", body)
			}
			r, err := result.Parse(env.Run(id).Result)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Merged || len(r.Skills) != 1 || r.Skills[0].Verdict != tc.verdict ||
				!slices.Equal(r.Skills[0].Files.Reviewed, []string{"a.go", "b.go"}) {
				t.Errorf("stored result = %+v, want the last review's verdict %s over both files", r, tc.verdict)
			}
		})
	}
}

// A finding carried over from the last review is still the same finding: its
// thread gets no "Fixed" reply and stays open.
func TestIncrementalKeepsThreads(t *testing.T) {
	e := newInlineEnv(t)
	headB := sha('c')
	e.review(t, sha('a'), with("has_blocking_issues", finding(fpBlocking, "db/query.go", 11, 0, "blocking", "")))
	last, found, err := e.env.Store.LastReview(e.env.ctx, e.env.MRKey(1), "none")
	if err != nil || !found {
		t.Fatalf("LastReview = %v, %v", found, err)
	}
	if rows := e.rows(t); len(rows) != 1 || rows[fpBlocking].State != "open" {
		t.Fatalf("rows = %+v, want the thread of the blocking finding", rows)
	}

	// The next push changed another file; db/query.go and its finding are
	// carried over.
	servePublish(e.env, 1, headB)
	e.env.GitLab.ServeDiff(1, headB, queryFile)
	partial := partialReview([]string{"other.go"}, []string{"db/query.go"})
	reportIncremental(t, e.env, headB, last.ID, resultFor(t, headB, partial))
	publish(t, e.env)

	if replies := e.disc.Replies(t); len(replies) != 0 {
		t.Errorf("replies = %q, want none for a finding that is still there", replies)
	}
	if e.disc.Resolved("disc1") {
		t.Errorf("the thread of a carried finding was resolved")
	}
	if n := len(e.disc.Posted()); n != 1 {
		t.Errorf("%d discussions started, want only the first review's", n)
	}
	if r := e.rows(t)[fpBlocking]; r.State != "open" {
		t.Errorf("state = %q, want open", r.State)
	}
	wantStatus(t, e.env, headB, gitlab.StatusFailed, "Blocking issues found")
}

// A partial with status error is published as an error, as any error result
// is: nothing is merged and no full review is queued.
func TestIncrementalErrorResultPublishedAsIs(t *testing.T) {
	env := newTestEnv(t)
	headA, headB := sha('a'), sha('c')
	base := env.Publish(1, headA, resultFor(t, headA, reviewed("no_blocking_issues", "a.go", "b.go")))
	broken, err := json.Marshal(result.Result{
		SchemaVersion: result.SchemaVersion, Status: "error", PergentVersion: "0.5.0",
		Base: sha('b'), Head: headB, Skills: []result.Skill{},
		Error: &result.Error{Code: "internal", Message: "the model was unreachable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := reportIncremental(t, env, headB, base, broken)
	servePublish(env, 1, headB)

	publish(t, env)

	if body := createdComment(t, env, 1); !strings.Contains(body, "the model was unreachable") {
		t.Errorf("comment = %q, want the error", body)
	}
	wantStatus(t, env, headB, gitlab.StatusFailed, "pergent couldn't review this MR")
	wantPublished(t, env, id)
	wantNoApproval(t, env, 1)
	if r, err := result.Parse(env.Run(id).Result); err != nil || r.Merged {
		t.Errorf("stored result = %+v, %v; want it as it was sent", r, err)
	}
	if queued, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(time.Hour)); err != nil || len(queued) != 0 {
		t.Errorf("queued %+v, %v; want no run", queued, err)
	}
}

// An incremental run that failed to produce a result is reported as failed,
// as any run is: there is nothing to merge and nothing to restart.
func TestIncrementalFailureReasonPublishedAsIs(t *testing.T) {
	env := newTestEnv(t)
	headA, headB := sha('a'), sha('c')
	base := env.Publish(1, headA, resultFor(t, headA, reviewed("no_blocking_issues", "a.go", "b.go")))
	id := env.Dispatched(1, headB)
	if err := env.Store.SetIncremental(env.ctx, id, &base); err != nil {
		t.Fatal(err)
	}
	reason := "the review Job ended without a result"
	if ok, err := env.Store.ReportFailure(env.ctx, id, reason, env.Now()); err != nil || !ok {
		t.Fatalf("ReportFailure = %v, %v", ok, err)
	}
	servePublish(env, 1, headB)

	publish(t, env)

	if body := createdComment(t, env, 1); body != policy.ErrorComment(reason) {
		t.Errorf("comment = %q, want %q", body, policy.ErrorComment(reason))
	}
	wantStatus(t, env, headB, gitlab.StatusFailed, "pergent couldn't review this MR")
	wantPublished(t, env, id)
	if queued, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(time.Hour)); err != nil || len(queued) != 0 {
		t.Errorf("queued %+v, %v; want no run", queued, err)
	}
}
