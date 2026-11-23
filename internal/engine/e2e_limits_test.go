package engine

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// limitSeeds numbers the changes seedEntryUsage reviews, so each seed is a
// change of its own.
var limitSeeds atomic.Int64

// seedEntryUsage records a review of another change of acme/widgets that ran
// on ref and reported tokens tokens at the engine's clock. Like seedUsageOf it
// claims every queued run, so call it before queueing the change's own.
func seedEntryUsage(t *testing.T, env *testEnv, ref store.EntryRef, tokens int64) {
	t.Helper()
	id := env.Enqueue(100+limitSeeds.Add(1), sha('9'))
	if _, err := env.Store.ClaimQueued(env.ctx, 100, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.SetRunEntry(env.ctx, id, store.RunEntry{Source: ref.Source, ID: ref.ID, Name: ref.ID, Position: 1, Length: 1}); err != nil {
		t.Fatal(err)
	}
	seedUsageOf(t, env, id, env.Now(), tokens)
}

// setLimit stores a limit of tokens on ref for team (0: the model limit).
func setLimit(t *testing.T, env *testEnv, ref store.EntryRef, team, tokens int64) {
	t.Helper()
	if err := env.Store.SetModelLimit(env.ctx, store.ModelLimit{Ref: ref, TeamID: team, MonthlyTokens: tokens,
		UpdatedAt: env.Now(), UpdatedBy: "test"}); err != nil {
		t.Fatalf("SetModelLimit: %v", err)
	}
}

// configRef is the config file's entry, the global chain of a test env.
func configRef() store.EntryRef {
	return store.EntryRef{Source: store.SourceGlobal, ID: store.ConfigModelID}
}

// configName is the config file's entry's name.
func configName(t *testing.T, env *testEnv) string {
	t.Helper()
	for _, m := range env.Engine.Instances.GlobalModels() {
		if m.ID == store.ConfigModelID {
			return m.Name
		}
	}
	t.Fatal("no config entry")
	return ""
}

// A global entry at its model limit and a team entry at its team's limit are
// skipped: the review runs on the next entry.
func TestLimitsRunNextEntry(t *testing.T) {
	env := newTestEnv(t)
	env.SetNow(novemberMid)
	team := teamOwning(t, env, "Payments")
	g := globalRef(t, env, "gpt-g", "key-g")
	a := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	b := addTeamModel(t, env, team, "team-b", "gpt-b", "key-team-b")
	saveChain(t, env, store.Chain{Scope: store.ScopeRepo, Instance: testInstance, Project: testProjectPath,
		Entries: []store.EntryRef{g, a, b}, SavedByAdmin: true})
	seedEntryUsage(t, env, g, 10)
	seedEntryUsage(t, env, a, 10)
	setLimit(t, env, g, 0, 10)
	setLimit(t, env, a, team, 10)

	id := dispatchMR(t, env, 3, sha('a'))
	wantJobKey(t, env, id, "key-team-b")
	if r := env.Run(id); r.ChainPosition != 3 || r.FallbackClass != chain.ReasonModelLimit {
		t.Errorf("run entry = position %d, first skip %q; want 3 and %q", r.ChainPosition, r.FallbackClass, chain.ReasonModelLimit)
	}
}

// A fallback whose entry fallen back from still has its mark fails, naming the
// limit of the entry left.
func TestFallbackFailsOnMarkAndLimit(t *testing.T) {
	e, refs := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	env.SetNow(novemberMid)
	team := ownerTeam(t, env)
	seedEntryUsage(t, env, refs[1], 10)
	setLimit(t, env, refs[1], team, 10)
	head := sha('a')
	servePublish(env, 1, head)
	root := env.Enqueue(1, head)

	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	wantFellBack(t, env, first, store.ClassQuota)
	dispatch(t, env)
	publish(t, env)

	as, err := env.Store.Attempts(env.ctx, root)
	if err != nil || len(as) != 2 {
		t.Fatalf("attempts = %d, %v; want 2", len(as), err)
	}
	want := "all models exhausted: team-a (out of quota), team-b (team limit used up)"
	if last := as[1]; last.FailureReason != want || last.State != store.Published {
		t.Errorf("last attempt = %s %q; want published %q", last.State, last.FailureReason, want)
	}
	if n := len(env.Jobs()); n != 1 {
		t.Errorf("%d Jobs, want 1", n)
	}
}

// An API review has no owner team: a team's used-up limit does not stop it.
func TestAPIReviewIgnoresTeamLimits(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	env.SetNow(novemberMid)
	team := teamOwning(t, env, "Payments")
	seedEntryUsage(t, env, configRef(), 10)
	setLimit(t, env, configRef(), team, 10)
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no Job; run = %+v\n%s", env.Run(id), env.Logs())
	}
}

// An API review at the model limit fails, naming the entry: it is never held.
func TestAPIReviewAtModelLimitFails(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	env.SetNow(novemberMid)
	seedEntryUsage(t, env, configRef(), 10)
	setLimit(t, env, configRef(), 0, 10)
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	if env.Job(id) != nil {
		t.Fatal("a review at the model limit got a Job")
	}
	publish(t, env)
	a.wantFailed(t, id, reviewapi.StepStarting, "all models exhausted: "+configName(t, env)+" (model limit used up)")
}

// When the usage can't be read, the dispatch is tried again; nothing starts
// unchecked.
func TestLimitReadErrorRetries(t *testing.T) {
	env := quotaPushEnv(t, 0, 0)
	setLimit(t, env, configRef(), 0, 100)
	execSQL(t, env, "DROP TABLE run_usage_models")
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Fatalf("%d jobs were created", len(jobs))
	}
	if r := env.Run(id); r.State != store.Queued || r.Attempts != 1 {
		t.Fatalf("run = %s attempts %d, want queued 1; logs:\n%s", r.State, r.Attempts, env.Logs())
	}
	if logs := env.Logs(); !strings.Contains(logs, "read the model limits") {
		t.Errorf("the retry isn't about the limits; logs:\n%s", logs)
	}
}

// wantLimitHeld checks run id waits because a limit took part, with no Job.
func wantLimitHeld(t *testing.T, env *testEnv, id string) {
	t.Helper()
	r := env.Run(id)
	if r.State != store.Waiting || r.SkipReason != store.WaitLimit {
		t.Fatalf("run = %s/%q, want waiting/%s; logs:\n%s", r.State, r.SkipReason, store.WaitLimit, env.Logs())
	}
	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Fatalf("%d jobs were created", len(jobs))
	}
}

// A push whose only entry is at its model limit is held as for a used-up
// quota: the same pending status and note, naming no limit. A 1-entry chain
// ignores marks, not limits.
func TestModelLimitHoldsPush(t *testing.T) {
	env := quotaPushEnv(t, 0, 0)
	seedEntryUsage(t, env, configRef(), 100)
	setLimit(t, env, configRef(), 0, 100)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)

	wantLimitHeld(t, env, id)
	if got := statusesOf(t, env, head); len(got) != 1 || got[0] != "pending: "+quotaDescription {
		t.Errorf("statuses = %v", got)
	}
	if ns := notes(env, 3); len(ns) != 1 || !strings.Contains(ns[0], quotaNote) {
		t.Fatalf("notes = %v", ns)
	}
}

// A fallback whose entries left are stopped by limits alone is held once the
// mark of the entry it fell back from has ended.
func TestFallbackHeldOnLimit(t *testing.T) {
	e, refs := newFallbackE2E(t, "team-a", "team-b")
	env := e.env
	env.SetNow(novemberMid)
	team := ownerTeam(t, env)
	seedEntryUsage(t, env, refs[1], 10)
	setLimit(t, env, refs[1], team, 10)
	head := sha('a')
	servePublish(env, 1, head)
	root := env.Enqueue(1, head)

	first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
	wantFellBack(t, env, first, store.ClassQuota)
	env.SetNow(novemberMid.Add(2 * time.Hour)) // team-a's quota mark (1h) has ended: it reads "tried"
	dispatch(t, env)

	as, err := env.Store.Attempts(env.ctx, root)
	if err != nil || len(as) != 2 {
		t.Fatalf("attempts = %d, %v; want 2", len(as), err)
	}
	if last := as[1]; last.State != store.Waiting || last.SkipReason != store.WaitLimit {
		t.Errorf("last attempt = %s/%q; want waiting/%s", last.State, last.SkipReason, store.WaitLimit)
	}
	if n := len(env.Jobs()); n != 1 {
		t.Errorf("%d Jobs, want 1", n)
	}
}

// An admin's start of a limit-held change goes past the model limit and runs
// on the first global entry with no mark; the team entry at its team's own
// limit stays skipped.
func TestStartGoesPastAdminLimits(t *testing.T) {
	env := quotaPushEnv(t, 0, 0)
	team := teamOwning(t, env, "Payments")
	a := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	g := globalRef(t, env, "gpt-g", "key-g")
	saveChain(t, env, store.Chain{Scope: store.ScopeRepo, Instance: testInstance, Project: testProjectPath,
		Entries: []store.EntryRef{a, g}, SavedByAdmin: true})
	seedEntryUsage(t, env, a, 10)
	seedEntryUsage(t, env, g, 10)
	setLimit(t, env, a, team, 10) // the team's own
	setLimit(t, env, g, 0, 10)    // an admin's
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantLimitHeld(t, env, id)

	if started, ok, err := env.Store.StartRun(env.ctx, env.MRKey(3), testProjectPath, head, "main"); err != nil || !ok || started != id {
		t.Fatalf("StartRun = %s, %v, %v", started, ok, err)
	}
	dispatch(t, env)
	wantJobKey(t, env, id, "key-g")
}

// A start whose chain is only the team's entry at the team's own limit can't
// be held (a start never is) and can't run: it fails, naming the limit.
func TestStartStoppedByTeamOwnLimitFails(t *testing.T) {
	env := quotaPushEnv(t, 0, 0)
	team := teamOwning(t, env, "Payments")
	a := addTeamModel(t, env, team, "team-a", "gpt-a", "key-team-a")
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: []store.EntryRef{a}})
	seedEntryUsage(t, env, a, 10)
	setLimit(t, env, a, team, 10)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantLimitHeld(t, env, id)

	if _, ok, err := env.Store.StartRun(env.ctx, env.MRKey(3), testProjectPath, head, "main"); err != nil || !ok {
		t.Fatalf("StartRun = %v, %v", ok, err)
	}
	dispatch(t, env)
	if r := env.Run(id); r.FailureReason != "all models exhausted: team-a (team limit used up)" {
		t.Fatalf("run = %s %q; want the exhausted failure\n%s", r.State, r.FailureReason, env.Logs())
	}
	if n := len(env.Jobs()); n != 0 {
		t.Errorf("%d Jobs, want 0", n)
	}
}

// Member A passed the hold with its run: a model limit used up since the
// split does not stop it.
func TestMemberAGoesPastAdminLimits(t *testing.T) {
	panelFloor(t, "v0.5.0")
	e := newE2E(t)
	env := e.env
	env.SetNow(novemberMid)
	env.Engine.ScanImage = testScanImage
	teamOwning(t, env, "Payments")
	savePanelChain(t, env, "team-b")
	seedEntryUsage(t, env, configRef(), 10)
	id := splitPanel(t, env, 1, sha('a'))
	setLimit(t, env, configRef(), 0, 10)
	dispatch(t, env)
	wantMemberKey(t, env, memberOfRole(t, env, id, "a"), testLLMKey)
}

// Chain B with no entry left under its limits turns the panel off with the
// quota's reason; the review runs on chain A alone.
func TestDispatchPanelLimitStopsB(t *testing.T) {
	panelFloor(t, "v0.5.0")
	env := quotaPushEnv(t, 0, 0)
	b := globalChainB(t, env)
	seedEntryUsage(t, env, b, 10)
	setLimit(t, env, b, 0, 10)
	head := sha('a')
	servePush(env, head)
	id := env.Enqueue(3, head)
	dispatch(t, env)
	wantOnlyA(t, env, id, "the token quota stopped model B")
}

// Job P's judge does not check limits: it ends a review that has started.
func TestPanelJudgeIgnoresLimits(t *testing.T) {
	e := newPanelE2E(t, []string{"team-a"}, []string{"team-b"})
	env := e.env
	env.SetNow(novemberMid)
	head := sha('a')
	id, a, b := releasedPanel(t, e, head, withTokens(t, clean(t, head), 5, 5), withTokens(t, clean(t, head), 5, 5))
	team := ownerTeam(t, env)
	setLimit(t, env, store.EntryRef{Source: a.Entry.Source, ID: a.Entry.ID}, team, 1) // used up by member A's own tokens
	setLimit(t, env, store.EntryRef{Source: b.Entry.Source, ID: b.Entry.ID}, team, 1)

	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no Job P\n%s", env.Logs())
	}
	if js := panelJudges(t, env, id); len(js) != 2 || js["a"].Name != "team-a" || js["b"].Name != "team-b" {
		t.Errorf("judges = %+v; want both, limits or not", js)
	}
}

// A client that pinned a model at its model limit fails, naming the entry: a
// pinned model is a chain of 1, and an API review is never held.
func TestAPIReviewPinnedModelAtLimitFails(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	env.SetNow(novemberMid)
	seedEntryUsage(t, env, configRef(), 10)
	setLimit(t, env, configRef(), 0, 10)
	id := a.ask(t, "route:1", twoFiles())
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) { c.ModelID = store.ConfigModelID }); err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	if env.Job(id) != nil {
		t.Fatal("a pinned model at its model limit got a Job")
	}
	publish(t, env)
	a.wantFailed(t, id, reviewapi.StepStarting, "all models exhausted: "+configName(t, env)+" (model limit used up)")
}

// With no stored global chain and a config entry with no usable key, the global
// chain is empty. A client pinned to a keyed entry that is at its model limit
// still fails: an empty chain must not skip the limits.
func TestAPIReviewPinnedModelAtLimitEmptyGlobalChainFails(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	env.SetNow(novemberMid)
	ref := globalRef(t, env, "pinned-model", "sk-pinned-example")
	t.Setenv("OPENCODE_API_KEY", "") // the config entry has no usable key now
	seedEntryUsage(t, env, ref, 10)
	setLimit(t, env, ref, 0, 10)
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) { c.ModelID = ref.ID }); err != nil {
		t.Fatal(err)
	}
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	if env.Job(id) != nil {
		t.Fatal("a pinned model at its model limit got a Job, with an empty global chain")
	}
	publish(t, env)
	m, ok := env.Engine.Instances.ModelByID(ref.ID)
	if !ok {
		t.Fatal("the pinned model is gone")
	}
	a.wantFailed(t, id, reviewapi.StepStarting, "all models exhausted: "+m.Name+" (model limit used up)")
}

// A pinned review's tokens count toward the model limit of its entry, so the
// run records that entry.
func TestAPIReviewPinnedModelRecordsEntry(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	env.SetNow(novemberMid)
	setLimit(t, env, configRef(), 0, 1000000)
	id := a.ask(t, "route:1", twoFiles())
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) { c.ModelID = store.ConfigModelID }); err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	if env.Job(id) == nil {
		t.Fatalf("no Job for a pinned model under its limit\n%s", env.Logs())
	}
	rv, found, err := env.Store.APIReviewByID(env.ctx, id)
	if err != nil || !found {
		t.Fatalf("APIReviewByID = %v, %v", found, err)
	}
	if rv.Run.ModelEntryID != store.ConfigModelID {
		t.Errorf("run's ModelEntryID = %q; want %q", rv.Run.ModelEntryID, store.ConfigModelID)
	}
}

// A writer's review command is refused when only limits stop every entry:
// 👎 and the quota's note, as for a used-up quota.
func TestReviewCommandRefusedAtModelLimit(t *testing.T) {
	e := quotaCommands(t, 0)
	env := e.env
	publishedHead(t, env, sha('a'))
	seedEntryUsage(t, env, configRef(), 5) // 5 of the repository's 10: room left
	setLimit(t, env, configRef(), 0, 5)

	c := e.say(t, "dev", userDeveloper, "@pergent-review review")
	e.answer(t)
	e.wantReactions(t, c, eyes, down)
	e.wantReplies(t, quotaNote)
	wantOutcome(t, env, "refused: "+whyQuota, 1)
}
