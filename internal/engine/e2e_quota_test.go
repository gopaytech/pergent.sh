package engine

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// novemberMid is a clock in the middle of a month, so no test straddles the 1st.
var novemberMid = time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)

// quotaPushEnv is a GitLab environment whose repository has a monthly quota of
// quota tokens (none when 0) and used tokens (none when 0) spent this month by
// another change. Merge request 3 accepts statuses and notes.
func quotaPushEnv(t *testing.T, quota, used int64) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	env.SetNow(novemberMid)
	if quota > 0 {
		env.Engine.Config.Repositories[0].MonthlyTokenQuota = &quota
	}
	env.GitLab.Reply(http.MethodPost, notesPath(3), http.StatusCreated, `{"id": 900}`)
	if used > 0 {
		seedQuotaUsage(t, env, used)
	}
	return env
}

// servePush serves merge request 3 at head and accepts its status.
func servePush(env *testEnv, head string) {
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
}

// wantHeld checks run id waits for the quota with no Job.
func wantHeld(t *testing.T, env *testEnv, id string) {
	t.Helper()
	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitQuota {
		t.Fatalf("run = %s/%q, want waiting/%s; logs:\n%s", r.State, r.SkipReason, store.WaitQuota, env.Logs())
	}
	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Fatalf("%d jobs were created", len(jobs))
	}
}

// A repository with no quota is reviewed as before, and nothing is read.
func TestNoQuotaReviewsAsToday(t *testing.T) {
	env := quotaPushEnv(t, 0, 0)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
	if n := len(notes(env, 3)); n != 0 {
		t.Errorf("%d notes, want none", n)
	}
}

// A push on a used-up quota waits: no Job, a pending status and one note. A
// second push waits too, with its own status and no second note.
func TestUsedUpRepoQuotaHoldsPush(t *testing.T) {
	env := quotaPushEnv(t, 100, 100)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)

	wantHeld(t, env, id)
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "pending: "+quotaDescription {
		t.Errorf("statuses = %v", got)
	}
	if ns := notes(env, 3); len(ns) != 1 || !strings.Contains(ns[0], quotaNote) {
		t.Fatalf("notes = %v", ns)
	}

	head2 := sha('c')
	servePush(env, head2)
	id2 := env.Enqueue(3, head2)
	dispatch(t, env)
	wantHeld(t, env, id2)
	if r := env.Run(id); r.State != store.Superseded {
		t.Errorf("first head = %s, want superseded", r.State)
	}
	if got := statusesOf(t, env, head2); len(got) != 1 || got[0] != "pending: "+quotaDescription {
		t.Errorf("statuses of the second head = %v", got)
	}
	if n := len(notes(env, 3)); n != 1 {
		t.Errorf("%d notes, want 1", n)
	}
}

// The instance's quota holds a change whose repository has room.
func TestUsedUpInstanceQuotaHolds(t *testing.T) {
	env := quotaPushEnv(t, 1000, 0)
	instQuota := int64(500)
	env.Engine.Config.GitLab[0].MonthlyTokenQuota = &instQuota
	seedQuotaUsage(t, env, 500)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantHeld(t, env, id)
}

// One token under the quota is reviewed.
func TestRoomLeftReviews(t *testing.T) {
	env := quotaPushEnv(t, 100, 99)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
}

// pergent.sh's own full re-run (queued with quotaChecked false, as publish
// does) is an automatic run: held.
func TestOwnFullRerunHeld(t *testing.T) {
	env := quotaPushEnv(t, 100, 100)
	head := sha('a')
	servePush(env, head)
	id, _, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(3), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	wantHeld(t, env, id)
}

// A run asked for by hand was checked when it was asked for: it goes ahead
// even though the quota is used up now (an overshoot by design).
func TestCheckedRunNotHeld(t *testing.T) {
	env := quotaPushEnv(t, 100, 100)
	head := sha('a')
	servePush(env, head)
	id, _, err := env.Store.EnqueueRerun(env.ctx, env.MRKey(3), testProjectPath, head, "main", store.TriggerRerun, nil, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no job; run = %+v; logs:\n%s", env.Run(id), env.Logs())
	}
	if n := len(notes(env, 3)); n != 0 {
		t.Errorf("%d notes, want none", n)
	}
}

// An untrusted author's push waits for trust first: the trust note only, and
// the reason is the trust one.
func TestUntrustedWaitsForTrustNotQuota(t *testing.T) {
	head := sha('a')
	env := trustEnv(t, 3, head, 20)
	env.SetNow(novemberMid)
	q := int64(100)
	env.Engine.Config.Repositories[0].MonthlyTokenQuota = &q
	seedQuotaUsage(t, env, 100)
	id := env.Enqueue(3, head)
	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitUntrusted {
		t.Fatalf("run = %s/%q, want waiting/untrusted_author", r.State, r.SkipReason)
	}
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "pending: "+waitingDescription {
		t.Errorf("statuses = %v", got)
	}
	ns := notes(env, 3)
	if len(ns) != 1 || strings.Contains(ns[0], quotaNote) {
		t.Fatalf("notes = %v, want the trust note only", ns)
	}
}

// A quota that can't be read is a failed attempt, not a pass and not a hold:
// no Job, and the run goes back to the queue.
func TestQuotaReadErrorRetries(t *testing.T) {
	env := quotaPushEnv(t, 100, 0)
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
	if _, err := conn.Exec(env.ctx, `DROP TABLE run_usage`); err != nil {
		t.Fatal(err)
	}

	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)

	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Fatalf("%d jobs were created", len(jobs))
	}
	r := env.Run(id)
	if r.State != store.Queued || r.Attempts != 1 {
		t.Fatalf("run = %s attempts %d, want queued 1; logs:\n%s", r.State, r.Attempts, env.Logs())
	}
	if n := len(notes(env, 3)); n != 0 {
		t.Errorf("%d notes, want none", n)
	}
}

// The note is posted once a month: a change held in November gets it again in
// December.
func TestNoteAgainNextMonth(t *testing.T) {
	env := quotaPushEnv(t, 100, 100)
	nov := time.Date(2026, time.November, 20, 9, 0, 0, 0, time.UTC)
	env.SetNow(nov)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantHeld(t, env, id)
	if n := len(notes(env, 3)); n != 1 {
		t.Fatalf("%d notes in November, want 1", n)
	}

	dec := time.Date(2026, time.December, 3, 9, 0, 0, 0, time.UTC)
	env.SetNow(dec)
	seedUsageOf(t, env, env.Enqueue(10, sha('8')), dec, 100)
	head2 := sha('c')
	servePush(env, head2)
	id2 := env.Enqueue(3, head2)
	dispatch(t, env)
	wantHeld(t, env, id2)
	if n := len(notes(env, 3)); n != 2 {
		t.Errorf("%d notes after December's push, want 2", n)
	}
}

// GitHub holds the same way, with a queued check run in place of a status.
func TestGitHubUsedUpQuotaHolds(t *testing.T) {
	head := sha('a')
	env, gh := ghTrustEnv(t, head, "dev", 51, "write")
	env.SetNow(novemberMid)
	q := int64(100)
	env.Engine.Config.Repositories[1].MonthlyTokenQuota = &q
	seedUsageOf(t, env, ghEnqueue(t, env, 9, sha('9')), env.Now(), 100)
	id := ghEnqueue(t, env, 3, head)
	dispatch(t, env)

	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitQuota {
		t.Fatalf("run = %s/%q, want waiting/%s; logs:\n%s", r.State, r.SkipReason, store.WaitQuota, env.Logs())
	}
	if env.Job(id) != nil {
		t.Errorf("a job was created")
	}
	if got := ghCheckRuns(t, gh); len(got) != 1 || got[0] != "queued/"+quotaDescription {
		t.Errorf("check runs = %v", got)
	}
	if n := len(gh.Requests(http.MethodPost, ghRepoAPI+"/issues/3/comments")); n != 1 {
		t.Errorf("%d notes, want 1", n)
	}
}

// A note that can't be posted is tried again on the next held push, and the
// run waits all the same.
func TestQuotaNoteRetriedAfterFailedReply(t *testing.T) {
	env := quotaPushEnv(t, 100, 100)
	env.GitLab.Reply(http.MethodPost, notesPath(3), http.StatusInternalServerError, `{"message":"boom"}`)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantHeld(t, env, id)
	failed := len(notes(env, 3))
	if failed == 0 {
		t.Fatal("the note was not tried")
	}

	env.GitLab.Reply(http.MethodPost, notesPath(3), http.StatusCreated, `{"id": 900}`)
	head2 := sha('c')
	servePush(env, head2)
	id2 := env.Enqueue(3, head2)
	dispatch(t, env)
	wantHeld(t, env, id2)
	if ns := notes(env, 3); len(ns) != failed+1 || !strings.Contains(ns[failed], quotaNote) {
		t.Errorf("notes = %v, want one more try after the %d failed", ns, failed)
	}
}
