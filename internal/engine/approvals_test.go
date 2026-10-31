package engine

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func sweep(t *testing.T, env *testEnv) {
	t.Helper()
	if err := env.Engine.SweepApprovals(env.ctx); err != nil {
		t.Fatalf("SweepApprovals: %v", err)
	}
}

// approved publishes a clean review of head on merge request iid and records
// the approve bot's approval of it against main.
func approved(t *testing.T, env *testEnv, iid int64, head string) {
	t.Helper()
	env.Publish(iid, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	if err := env.Store.SetApprovedSHA(env.ctx, env.MRKey(iid), head, "main"); err != nil {
		t.Fatal(err)
	}
}

func approvals(t *testing.T, env *testEnv) []store.Approval {
	t.Helper()
	a, err := env.Store.Approvals(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// gitlabWrites are the requests that change something on GitLab.
func gitlabWrites(env *testEnv) []gitlabRequest {
	var out []gitlabRequest
	for _, r := range env.GitLab.Requests("", "") {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// liveRun returns the live run of head against target on iid, or fails.
func liveRun(t *testing.T, env *testEnv, iid int64, head, target string) store.Run {
	t.Helper()
	id, created, err := env.Store.EnqueueRun(env.ctx, env.MRKey(iid), testProjectPath, head, target)
	if err != nil || created {
		t.Fatalf("no live run of %s against %s (created=%v, %v)", head, target, created, err)
	}
	return env.Run(id)
}

// GitLab doesn't retry a failed webhook delivery: a push pergent.sh never
// heard of must still withdraw the bot's approval and get a review.
func TestSweepMissedPush(t *testing.T) {
	env := newTestEnv(t)
	approved(t, env, 1, sha('a'))
	env.GitLab.SetMR(openMR(1, sha('c')))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	sweep(t, env)

	reqs := env.GitLab.Requests(http.MethodPost, unapprovePath(1))
	if len(reqs) != 1 || reqs[0].Token != testApproveToken {
		t.Fatalf("got %d unapprovals, want 1 by the approve bot", len(reqs))
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %v, want none", a)
	}
	if r := liveRun(t, env, 1, sha('c'), "main"); r.State != store.Queued || r.ProjectPath != testProjectPath {
		t.Errorf("run of the missed head = %+v, want queued", r)
	}
	select {
	case <-env.Engine.wakeCh():
	default:
		t.Errorf("engine was not woken for the new run")
	}
}

// A retarget pergent.sh never heard of withdraws the approval too, and the
// head is reviewed against the new target.
func TestSweepMissedRetarget(t *testing.T) {
	env := newTestEnv(t)
	approved(t, env, 1, sha('a'))
	mr := openMR(1, sha('a'))
	mr.TargetBranch = "develop"
	env.GitLab.SetMR(mr)
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	sweep(t, env)

	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
		t.Fatalf("got %d unapprovals, want 1", n)
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %v, want none", a)
	}
	liveRun(t, env, 1, sha('a'), "develop")
}

func TestSweepMergedMRClearsWithoutWriting(t *testing.T) {
	env := newTestEnv(t)
	approved(t, env, 1, sha('a'))
	mr := openMR(1, sha('c'))
	mr.State = "merged"
	env.GitLab.SetMR(mr)

	sweep(t, env)

	if w := gitlabWrites(env); len(w) != 0 {
		t.Errorf("wrote to GitLab for a merged MR: %+v", w)
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %v, want cleared", a)
	}
	if _, created, _ := env.Store.EnqueueRun(env.ctx, env.MRKey(1), testProjectPath, sha('c'), "main"); !created {
		t.Errorf("a run was queued for a merged MR")
	}
}

// A closed MR can be reopened, so its approval is withdrawn before the record
// goes.
func TestSweepClosedMRUnapproves(t *testing.T) {
	env := newTestEnv(t)
	approved(t, env, 1, sha('a'))
	mr := openMR(1, sha('a'))
	mr.State = "closed"
	env.GitLab.SetMR(mr)
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	sweep(t, env)

	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
		t.Errorf("got %d unapprovals, want 1", n)
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %v, want cleared", a)
	}
}

func TestSweepCurrentApprovalKept(t *testing.T) {
	env := newTestEnv(t)
	approved(t, env, 1, sha('a'))
	env.GitLab.SetMR(openMR(1, sha('a')))

	sweep(t, env)

	if w := gitlabWrites(env); len(w) != 0 {
		t.Errorf("wrote to GitLab for a current approval: %+v", w)
	}
	if a := approvals(t, env); len(a) != 1 || a[0].ApprovedSHA != sha('a') {
		t.Errorf("approvals = %v, want the approval of %s kept", a, sha('a'))
	}
}

// A repository switched off still loses its stale approval, but gets no run.
func TestSweepUnconfiguredRepositoryNoRun(t *testing.T) {
	env := newTestEnv(t)
	approved(t, env, 1, sha('a'))
	env.GitLab.SetMR(openMR(1, sha('c')))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	env.Engine.Config.Repositories = nil

	sweep(t, env)

	if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
		t.Errorf("got %d unapprovals, want 1", n)
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %v, want cleared", a)
	}
	if _, created, _ := env.Store.EnqueueRun(env.ctx, env.MRKey(1), testProjectPath, sha('c'), "main"); !created {
		t.Errorf("a run was queued for a repository no longer configured")
	}
}

// One merge request's trouble is logged and skipped; the others are swept,
// and the failed one is tried again next time.
func TestSweepErrorsAreSkipped(t *testing.T) {
	env := newTestEnv(t)
	approved(t, env, 1, sha('a'))
	approved(t, env, 2, sha('a'))
	approved(t, env, 3, sha('a'))
	env.GitLab.Reply(http.MethodGet, mrPath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	env.GitLab.SetMR(openMR(2, sha('c')))
	env.GitLab.Reply(http.MethodPost, unapprovePath(2), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	env.GitLab.SetMR(openMR(3, sha('c')))
	env.GitLab.Reply(http.MethodPost, unapprovePath(3), http.StatusCreated, `{}`)

	sweep(t, env)

	a := approvals(t, env)
	if len(a) != 2 || a[0].MR != env.MRKey(1) || a[1].MR != env.MRKey(2) {
		t.Errorf("approvals = %v, want MR 1 and 2 kept for the next sweep", a)
	}
	if _, created, _ := env.Store.EnqueueRun(env.ctx, env.MRKey(2), testProjectPath, sha('c'), "main"); !created {
		t.Errorf("a run was queued though the approval couldn't be withdrawn")
	}
	liveRun(t, env, 3, sha('c'), "main")
	if logs := env.Logs(); !strings.Contains(logs, "level=WARN") {
		t.Errorf("failures not logged:\n%s", logs)
	}
	if strings.Contains(env.Logs(), testApproveToken) || strings.Contains(env.Logs(), testReviewToken) {
		t.Error("a token reached the log")
	}
}

// Without bots for the instance, the approval is left alone, quietly.
func TestSweepUnknownInstanceQuiet(t *testing.T) {
	env := newTestEnv(t)
	approved(t, env, 1, sha('a'))
	env.Engine.Platforms = map[string]Platform{}

	sweep(t, env)

	if n := len(env.GitLab.Requests("", "")); n != 0 {
		t.Errorf("made %d GitLab requests without bots", n)
	}
	if logs := env.Logs(); strings.Contains(logs, "level=WARN") || strings.Contains(logs, "level=ERROR") {
		t.Errorf("logged above debug:\n%s", logs)
	}
}

// A retarget the webhook did report gets its own run, and the approval of
// the old target is withdrawn by the cleanup pass.
func TestRetargetWithdrawsApproval(t *testing.T) {
	env := newTestEnv(t)
	approved(t, env, 1, sha('a'))
	if _, created, err := env.Store.EnqueueRun(env.ctx, env.MRKey(1), testProjectPath, sha('a'), "develop"); err != nil || !created {
		t.Fatalf("retarget run: %v, %v", created, err)
	}
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)

	cleanup(t, env)

	reqs := env.GitLab.Requests(http.MethodPost, unapprovePath(1))
	if len(reqs) != 1 || reqs[0].Token != testApproveToken {
		t.Fatalf("got %d unapprovals, want 1 by the approve bot", len(reqs))
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %v, want none", a)
	}
}

// The approval records the target the run was reviewed against.
func TestPublishRecordsApprovedTarget(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)

	publish(t, env)

	if a := approvals(t, env); len(a) != 1 || a[0].ApprovedSHA != head || a[0].ApprovedTarget != "main" {
		t.Errorf("approvals = %v, want %s against main", a, head)
	}
}

const (
	approvalOffNote        = "**Not approved:** approval is turned off for this repository."
	approvalOffDescription = "No blocking issues; not approved: approval is turned off"
)

// setApprove switches the dashboard's "approve clean reviews" for project on
// instance; nil leaves it at its default (on).
func setApprove(t *testing.T, env *testEnv, instance, project string, approve *bool) {
	t.Helper()
	if err := env.Store.UpsertDashboardRepo(env.ctx, store.DashboardRepo{
		Instance: instance, Project: project, Approve: approve,
	}); err != nil {
		t.Fatal(err)
	}
}

// Switching approval off while the approve bot's approval of the current head
// stands: the next cleanup pass withdraws it (GitLab unapproves), the record
// is cleared, and the next clean review is published without an approval.
func TestApprovalOffWithdrawsApproval(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name   string
		turnOn func(env *testEnv)
		// turnOff switches approval off for acme/widgets.
		turnOff func(env *testEnv)
	}{
		{"dashboard",
			func(env *testEnv) {
				env.Engine.Config.Repositories = nil
				setApprove(t, env, testInstance, testProjectPath, nil)
			},
			func(env *testEnv) { setApprove(t, env, testInstance, testProjectPath, &off) }},
		{"config",
			func(env *testEnv) {},
			func(env *testEnv) { env.Engine.Config.Repositories[0].Approve = &off }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tc.turnOn(env)
			head := sha('a')
			env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
			servePublish(env, 1, head)
			publish(t, env)
			if got := recordedApproval(t, env, 1); got != head {
				t.Fatalf("recorded approval = %q, want %s", got, head)
			}

			tc.turnOff(env)
			env.GitLab.Reset()
			env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
			cleanup(t, env)

			reqs := env.GitLab.Requests(http.MethodPost, unapprovePath(1))
			if len(reqs) != 1 || reqs[0].Token != testApproveToken {
				t.Fatalf("got %d unapprovals, want 1 by the approve bot", len(reqs))
			}
			if got := recordedApproval(t, env, 1); got != "" {
				t.Fatalf("recorded approval = %q, want cleared", got)
			}
			if !strings.Contains(env.Logs(), "approval withdrawn: approval is turned off for the repository") {
				t.Errorf("the withdrawal isn't logged:\n%s", env.Logs())
			}
			// Nothing more to withdraw on the next pass.
			cleanup(t, env)
			if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); n != 1 {
				t.Errorf("got %d unapprovals after the second pass, want still 1", n)
			}

			head = sha('c')
			id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
			servePublish(env, 1, head)
			publish(t, env)

			wantPublished(t, env, id)
			wantNoApproval(t, env, 1)
			wantStatus(t, env, head, gitlab.StatusSuccess, approvalOffDescription)
			if body := createdComment(t, env, 1); !strings.Contains(body, "---\n"+approvalOffNote+"\n") {
				t.Errorf("comment lacks the approval-off note: %q", body)
			}
		})
	}
}

// A clean review published after its repository was switched off is not
// approved: it is posted like a review with approval turned off.
func TestPublishAfterSwitchOffIsNotApproved(t *testing.T) {
	env := newTestEnv(t)
	head := sha('a')
	id := env.Report(1, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublish(env, 1, head)
	env.Engine.Config.Repositories = nil

	publish(t, env)

	wantPublished(t, env, id)
	wantNoApproval(t, env, 1)
	wantStatus(t, env, head, gitlab.StatusSuccess, approvalOffDescription)
	if body := createdComment(t, env, 1); !strings.Contains(body, "---\n"+approvalOffNote+"\n") {
		t.Errorf("comment lacks the approval-off note: %q", body)
	}
	if r := env.Run(id); r.Outcome != store.OutcomeNoBlocking || r.Approved {
		t.Errorf("run = outcome %q approved %v; want no_blocking, not approved", r.Outcome, r.Approved)
	}
}

// The same on GitHub: the approve App's approval is dismissed.
func TestApprovalOffWithdrawsApprovalGitHub(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	// acme/gadgets is the dashboard's, not the config's.
	env.Engine.Config.Repositories = env.Engine.Config.Repositories[:1]
	setApprove(t, env, ghInstance, ghRepoPath, nil)
	head := sha('a')
	ghReport(t, env, 3, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublishGitHub(gh, 3, head)
	publish(t, env)
	if a := approvals(t, env); len(a) != 1 || a[0].MR != ghKey(3) || a[0].ApprovedSHA != head {
		t.Fatalf("approvals = %+v, want %s's", a, head)
	}

	off := false
	setApprove(t, env, ghInstance, ghRepoPath, &off)
	gh.Reset()
	gh.Reply(http.MethodGet, ghPRPath(3)+"/reviews", http.StatusOK, []map[string]any{
		{"id": 41, "user": map[string]any{"id": ghApproveBot}, "state": "APPROVED", "commit_id": head},
	})
	gh.Reply(http.MethodPut, ghPRPath(3)+"/reviews/41/dismissals", http.StatusOK, map[string]any{"id": 41})
	cleanup(t, env)

	d := gh.Requests(http.MethodPut, ghPRPath(3)+"/reviews/41/dismissals")
	if len(d) != 1 || d[0].Token != ghApproveToken {
		t.Fatalf("dismissals = %+v; want one by the approve App", d)
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Fatalf("approvals = %+v, want none", a)
	}

	gh.Reset()
	head = sha('c')
	id := ghReport(t, env, 3, head, resultFor(t, head, reviewed("no_blocking_issues", "a.go")))
	servePublishGitHub(gh, 3, head)
	publish(t, env)

	wantPublished(t, env, id)
	if r := gh.Requests(http.MethodPost, ghPRPath(3)+"/reviews"); len(r) != 0 {
		t.Errorf("reviews = %+v; want no approval", r)
	}
	if a := approvals(t, env); len(a) != 0 {
		t.Errorf("approvals = %+v, want none", a)
	}
	run := onlyCheckRun(t, gh)
	if out, _ := run["output"].(map[string]any); run["conclusion"] != "success" || out["title"] != approvalOffDescription {
		t.Errorf("check run = %v", run)
	}
	comments := gh.Requests(http.MethodPost, ghRepoAPI+"/issues/3/comments")
	if len(comments) != 1 {
		t.Fatalf("%d comments posted", len(comments))
	}
	if body, _ := comments[0].JSON(t)["body"].(string); !strings.Contains(body, approvalOffNote) {
		t.Errorf("comment lacks the note: %q", body)
	}
}

// Approval off for a repository leaves the approvals of others alone, and an
// approval the bot can't withdraw waits out the unapprove backoff.
func TestApprovalOffOnlyItsRepositoryWithBackoff(t *testing.T) {
	env := newTestEnv(t)
	gh := addGitHub(t, env, true)
	ghApproved(t, env, 3, sha('a'))
	approved(t, env, 1, sha('a'))
	off := false
	env.Engine.Config.Repositories[0].Approve = &off
	env.GitLab.SetMR(openMR(1, sha('a')))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	attempts := func() int { return len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))) }

	cleanup(t, env)
	if n := attempts(); n != 1 {
		t.Fatalf("first pass: %d unapprovals, want 1", n)
	}
	cleanup(t, env)
	env.SetNow(env.Now().Add(29 * time.Second))
	cleanup(t, env)
	if n := attempts(); n != 1 {
		t.Fatalf("inside the backoff: %d unapprovals, want still 1", n)
	}
	env.SetNow(env.Now().Add(time.Second))
	env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
	cleanup(t, env)
	if n := attempts(); n != 2 {
		t.Fatalf("after the backoff: %d unapprovals, want 2", n)
	}
	if w := gh.Writes(); len(w) != 0 {
		t.Errorf("GitHub writes for a repository with approval on: %+v", w)
	}
	if a := approvals(t, env); len(a) != 1 || a[0].MR != ghKey(3) {
		t.Errorf("approvals = %+v, want only the GitHub one kept", a)
	}
}

// With approval off, an approval on a merge request merged or gone since is
// only forgotten: nothing is written to it. One on a closed merge request,
// which could be reopened, is withdrawn as on an open one.
func TestApprovalOffMergedOnlyClears(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name     string
		state    string // "" for a merge request that is gone
		withdraw bool
	}{
		{"merged", "merged", false},
		{"gone", "", false},
		{"closed", "closed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			approved(t, env, 1, sha('a'))
			env.Engine.Config.Repositories[0].Approve = &off
			if tc.state == "" {
				env.GitLab.Reply(http.MethodGet, mrPath(1), http.StatusNotFound, `{"message":"404 Not found"}`)
			} else {
				mr := openMR(1, sha('a'))
				mr.State = tc.state
				env.GitLab.SetMR(mr)
			}
			env.GitLab.Reply(http.MethodPost, unapprovePath(1), http.StatusCreated, `{}`)
			env.GitLab.Reset()

			cleanup(t, env)

			if n := len(env.GitLab.Requests(http.MethodPost, unapprovePath(1))); (n == 1) != tc.withdraw || n > 1 {
				t.Errorf("got %d unapprovals; want withdrawn %v", n, tc.withdraw)
			}
			if a := approvals(t, env); len(a) != 0 {
				t.Errorf("approvals = %v, want none", a)
			}
		})
	}
}
