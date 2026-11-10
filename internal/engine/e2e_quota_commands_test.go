package engine

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// quotaCommands is the commands harness with a monthly quota of 10 tokens on
// the repository and used tokens (none when 0) spent this month by another change. Two
// writers can use commands: dev (a Developer) and mo (a Maintainer).
func quotaCommands(t *testing.T, used int64) *cmdE2E {
	t.Helper()
	e := newCommandsE2E(t)
	env := e.env
	gl := env.GitLab
	q := int64(10)
	env.Engine.Config.Repositories[0].MonthlyTokenQuota = &q
	gl.Reply(http.MethodGet, membersPath(userDeveloper), http.StatusOK, member(30))
	gl.Reply(http.MethodGet, membersPath(userMaintainer), http.StatusOK, member(40))
	for _, id := range []int64{userDeveloper, userMaintainer} {
		gl.Reply(http.MethodGet, userPath(id), http.StatusOK, map[string]any{"id": id, "bot": false})
	}
	gl.Reply(http.MethodPost, notesPath(1), http.StatusCreated, `{"id": 900}`)
	if used > 0 {
		seedQuotaUsage(t, env, used)
	}
	return e
}

// seedQuotaUsage records a review of another change that reported tokens
// tokens at the engine's clock. It dispatches every queued run, so call it
// before queueing the change's own.
func seedQuotaUsage(t *testing.T, env *testEnv, tokens int64) {
	t.Helper()
	seedQuotaUsageAt(t, env, env.Now(), tokens)
}

// seedQuotaUsageAt is seedQuotaUsage with the result coming in at at.
func seedQuotaUsageAt(t *testing.T, env *testEnv, at time.Time, tokens int64) {
	t.Helper()
	id := env.Enqueue(9, sha('9'))
	// The runs' created_at is the database's clock, whatever the engine's.
	if _, err := env.Store.ClaimQueued(env.ctx, 100, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.MarkDispatched(env.ctx, id, kube.JobName(id), sha('b'), sha('5'), hashToken("seed-token"), at.Add(time.Hour), at); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"schema_version":1,"status":"completed","skills":[]}`)
	if out, err := env.Store.StoreResult(env.ctx, id, "seed-token", body, at); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	zero := int64(0)
	err := env.Store.RecordUsage(env.ctx, id, []store.SkillUsage{{
		Skill: "security", Verdict: "no_blocking_issues", In: tokens, CacheRead: &zero, CacheWrite: &zero, Reasoning: &zero,
	}})
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
}

// wantOutcome checks the log has the command answered with outcome.
func wantOutcome(t *testing.T, env *testEnv, outcome string) {
	t.Helper()
	if logs := env.Logs(); !strings.Contains(logs, "command answered") || !strings.Contains(logs, outcome) {
		t.Errorf("no command answered with %q in the log:\n%s", outcome, logs)
	}
}

// publishedHead is a review of head on merge request 1, published.
func publishedHead(t *testing.T, env *testEnv, head string) string {
	t.Helper()
	servePublish(env, 1, head)
	return env.Publish(1, head, resultFor(t, head, skillWithFinding(t, "no_blocking_issues", "suggestion")))
}

// A writer's review command on a used-up quota gets a 👎 and the note, and
// queues nothing: the review of the head, its status and its approval stay.
func TestReviewCommandRefusedOnUsedUpQuota(t *testing.T) {
	e := quotaCommands(t, 10)
	env := e.env
	head := sha('a')
	run := publishedHead(t, env, head)

	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	e.wantReplies(t, quotaNote)
	wantOutcome(t, env, "refused: the monthly token quota is used up")
	if r := env.Run(run); r.State != store.Published {
		t.Errorf("the published review = %s, want it left published", r.State)
	}
	runs, err := env.Store.MRRunDigests(env.ctx, env.MRKey(1), store.Visibility{All: true})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs of the change = %+v, %v; want only the published one", runs, err)
	}
	if n, err := env.Store.CountReruns(env.ctx, ptr(env.MRKey(1)), "", 0, env.Now().Add(-24*time.Hour)); err != nil || n != 0 {
		t.Errorf("CountReruns = %d, %v; want a refusal to use no re-run", n, err)
	}
	e.wantStatuses(t, head)
}

// The note is posted once a month on a change: another writer refused later
// gets the 👎 only.
func TestSecondQuotaRefusalHasNoReply(t *testing.T) {
	e := quotaCommands(t, 10)
	env := e.env
	publishedHead(t, env, sha('a'))

	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	e.wantReplies(t, quotaNote)

	env.SetNow(env.Now().Add(3 * time.Minute))
	c2 := e.say(t, "mo", userMaintainer, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c2, eyes, down)
	e.wantReplies(t, quotaNote)
}

// The quota is looked at after trust: a writer's start of an untrusted
// author's change, refused for the quota, leaves it waiting for trust.
func TestWriterStartRefusedForQuotaKeepsTrustWait(t *testing.T) {
	e, head := trustedCommands(t, userReporter, "rep")
	env := e.env
	q := int64(10)
	env.Engine.Config.Repositories[0].MonthlyTokenQuota = &q
	seedQuotaUsage(t, env, 10)
	id := waitingRun(t, env, head)

	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	e.wantReplies(t, quotaNote)
	wantOutcome(t, env, "refused: the monthly token quota is used up")
	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitUntrusted {
		t.Errorf("run = %s/%q, want waiting/%s", r.State, r.SkipReason, store.WaitUntrusted)
	}
}

// With room under the quota the command queues the re-run as always, marked
// as checked so the dispatcher doesn't look at the quota again.
func TestReviewCommandWithRoomRuns(t *testing.T) {
	e := quotaCommands(t, 9)
	env := e.env
	publishedHead(t, env, sha('a'))

	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, up)
	e.wantReplies(t)
	runs, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(1<<40))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Trigger != store.TriggerRerun || !runs[0].QuotaChecked {
		t.Fatalf("queued = %+v, want one checked rerun", runs)
	}
}

// A run a quota held last month is waiting still on the 1st; a writer's
// review command then reviews its head.
func TestReviewCommandAfterTheFirstReviewsHeldHead(t *testing.T) {
	e := quotaCommands(t, 0)
	env := e.env
	oct := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	// October's tokens used the quota up; the push was held.
	seedQuotaUsageAt(t, env, oct, 10)
	env.SetNow(oct)
	head := sha('a')
	servePublish(env, 1, head)
	id := env.Enqueue(1, head)
	if ok, err := env.Store.HoldForQuota(env.ctx, id); err != nil || !ok {
		t.Fatalf("HoldForQuota = %v, %v", ok, err)
	}
	if got, err := env.Store.WaitingChanges(env.ctx, testInstance, testProjectPath); err != nil || len(got) != 1 {
		t.Fatalf("WaitingChanges = %+v, %v; want the held change", got, err)
	}

	env.SetNow(time.Date(2026, time.November, 1, 0, 0, 1, 0, time.UTC))
	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, up)
	e.wantReplies(t)
	if got, err := env.Store.WaitingChanges(env.ctx, testInstance, testProjectPath); err != nil || len(got) != 0 {
		t.Errorf("WaitingChanges = %+v, %v; want none after the command", got, err)
	}
	queued, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(1<<40))
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].HeadSHA != head || !queued[0].QuotaChecked {
		t.Fatalf("queued = %+v, want one checked review of the held head", queued)
	}
	if r := env.Run(id); r.State != store.Superseded {
		t.Errorf("the held run = %s, want superseded", r.State)
	}
}
