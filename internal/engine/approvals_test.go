package engine

import (
	"net/http"
	"strings"
	"testing"

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
