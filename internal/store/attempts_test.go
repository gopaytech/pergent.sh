package store

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func quotaMark(id string) Mark {
	until := t0.Add(time.Hour)
	return Mark{Ref: EntryRef{SourceGlobal, id}, Class: ClassQuota, Until: &until, SetAt: t0}
}

// reportAttempt takes a queued attempt through dispatch to reported, with a
// result.
func reportAttempt(t *testing.T, s *Store, id, token string) {
	t.Helper()
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatched(ctx, id, "job-"+token, "base", "start", hash(token), t0.Add(time.Hour), t0); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	if out, err := s.StoreResult(ctx, id, token, []byte(`{}`), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
}

// An attempt closes the run it falls back from and queues the next one with
// what the first attempt was asked for, pointing at the first run.
func TestEnqueueAttemptCopiesAndCloses(t *testing.T) {
	s := newStore(t)
	base := reported(t, s, mr2, "b1", `{}`)
	from := dispatched(t, s, mr1, "a1", "tok-a1")
	setRun(t, s, from, `trigger = 'rerun', skill_subset = ARRAY['security'], base_run_id = $2::uuid, quota_checked = true,
		ci_checked = true`, base)
	if err := s.SetRunEntry(ctx, from, RunEntry{Source: SourceGlobal, ID: "m1", Name: "Main", Position: 1, Length: 3}); err != nil {
		t.Fatalf("SetRunEntry: %v", err)
	}
	if err := s.SetRunStatusRef(ctx, from, 42); err != nil {
		t.Fatalf("SetRunStatusRef: %v", err)
	}
	if out, err := s.StoreResult(ctx, from, "tok-a1", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	// An entry is recorded only while the run is being dispatched.
	if err := s.SetRunEntry(ctx, from, RunEntry{Source: SourceGlobal, ID: "m9"}); err == nil {
		t.Error("SetRunEntry on a reported run succeeded")
	}
	run := getRun(t, s, from)
	if run.ModelSource != SourceGlobal || run.ModelEntryID != "m1" || run.ModelEntryName != "Main" || run.ChainPosition != 1 ||
		run.ChainLength != 3 || run.ModelTeamID != nil || run.StatusRef != 42 || run.AttemptOf != nil {
		t.Fatalf("run = %+v; want its entry recorded", run)
	}

	next, created, err := s.EnqueueAttempt(ctx, run, quotaMark("m1"))
	if err != nil || !created || next == "" || next == from {
		t.Fatalf("EnqueueAttempt = %q, %v, %v", next, created, err)
	}
	closed := getRun(t, s, from)
	if closed.State != Superseded || closed.Outcome != OutcomeFellBack || closed.OutcomeReason != ClassQuota || closed.CleanedUp {
		t.Errorf("closed run = %s %q %q cleaned %v; want superseded, fell_back, quota, not cleaned up",
			closed.State, closed.Outcome, closed.OutcomeReason, closed.CleanedUp)
	}
	n := getRun(t, s, next)
	if n.State != Queued || n.Trigger != TriggerRerun || !slices.Equal(n.SkillSubset, []string{"security"}) ||
		n.BaseRunID == nil || *n.BaseRunID != base || !n.QuotaChecked || !n.CIChecked || n.StatusRef != 42 ||
		n.AttemptOf == nil || *n.AttemptOf != from || n.Kind != KindMR || n.HeadSHA != "a1" || n.TargetBranch != "main" ||
		n.MR != mr1 || n.ModelEntryID != "" || n.ChainPosition != 0 {
		t.Errorf("next attempt = %+v", n)
	}
	if marks, err := s.Marks(ctx); err != nil || len(marks) != 1 || marks[0].Ref != (EntryRef{SourceGlobal, "m1"}) || marks[0].Class != ClassQuota {
		t.Errorf("marks = %+v, %v; want the entry marked", marks, err)
	}

	// An attempt of an attempt still points at the first run.
	reportAttempt(t, s, next, "tok-next")
	third, created, err := s.EnqueueAttempt(ctx, getRun(t, s, next), quotaMark("m2"))
	if err != nil || !created {
		t.Fatalf("second EnqueueAttempt = %v, %v", created, err)
	}
	if r := getRun(t, s, third); r.AttemptOf == nil || *r.AttemptOf != from {
		t.Errorf("third attempt_of = %v; want %s", r.AttemptOf, from)
	}
}

// A result that arrives twice makes one attempt and one mark.
func TestFallbackIsOnce(t *testing.T) {
	s := newStore(t)
	from := reported(t, s, mr1, "a1", `{}`)
	run := getRun(t, s, from)
	if _, created, err := s.EnqueueAttempt(ctx, run, quotaMark("m1")); err != nil || !created {
		t.Fatalf("EnqueueAttempt = %v, %v", created, err)
	}
	later := quotaMark("m1")
	until := t0.Add(5 * time.Hour)
	later.Until = &until
	id, created, err := s.EnqueueAttempt(ctx, run, later)
	if err != nil || created || id != "" {
		t.Errorf("second EnqueueAttempt = %q, %v, %v; want nothing", id, created, err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE attempt_of = $1::uuid`, from).Scan(&n); err != nil || n != 1 {
		t.Errorf("attempts = %d, %v; want 1", n, err)
	}
	if marks, _ := s.Marks(ctx); len(marks) != 1 || !marks[0].Until.Equal(t0.Add(time.Hour)) {
		t.Errorf("marks = %+v; want the first mark alone", marks)
	}
}

// A push that superseded the run first wins: no attempt and no mark.
func TestEnqueueAttemptAfterPush(t *testing.T) {
	s := newStore(t)
	from := reported(t, s, mr1, "a1", `{}`)
	run := getRun(t, s, from)
	enqueue(t, s, mr1, "a2")
	id, created, err := s.EnqueueAttempt(ctx, run, quotaMark("m1"))
	if err != nil || created || id != "" {
		t.Errorf("EnqueueAttempt after a push = %q, %v, %v; want nothing", id, created, err)
	}
	if r := getRun(t, s, from); r.Outcome != "" {
		t.Errorf("superseded run outcome = %q; want it left alone", r.Outcome)
	}
	if marks, _ := s.Marks(ctx); len(marks) != 0 {
		t.Errorf("marks = %+v; want none", marks)
	}
}

func TestAttemptsOrder(t *testing.T) {
	s := newStore(t)
	first := reported(t, s, mr1, "a1", `{}`)
	second, _, err := s.EnqueueAttempt(ctx, getRun(t, s, first), quotaMark("m1"))
	if err != nil {
		t.Fatal(err)
	}
	reportAttempt(t, s, second, "tok-2")
	third, _, err := s.EnqueueAttempt(ctx, getRun(t, s, second), quotaMark("m2"))
	if err != nil {
		t.Fatal(err)
	}
	other := reported(t, s, mr2, "b1", `{}`)
	got, err := s.Attempts(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if ids := runIDs(got); !slices.Equal(ids, []string{first, second, third}) {
		t.Errorf("Attempts = %v; want %v (not %s)", ids, []string{first, second, third}, other)
	}
}

// Attempts are not re-runs anyone asked for, so they don't count against the
// caps.
func TestCountRerunsSkipsAttempts(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "a1", "main", TriggerRerun, nil, nil, false, false); err != nil {
		t.Fatal(err)
	}
	reportAttempt(t, s, getRunOfHead(t, s, "a1"), "tok-a1")
	if _, created, err := s.EnqueueAttempt(ctx, getRun(t, s, getRunOfHead(t, s, "a1")), quotaMark("m1")); err != nil || !created {
		t.Fatalf("EnqueueAttempt = %v, %v", created, err)
	}
	if n, err := s.CountReruns(ctx, &mr1, "", 0, t0.Add(-24*time.Hour)); err != nil || n != 1 {
		t.Errorf("CountReruns(mr) = %d, %v; want 1", n, err)
	}
	if n, err := s.CountReruns(ctx, nil, mr1.Instance, mr1.ProjectID, t0.Add(-24*time.Hour)); err != nil || n != 1 {
		t.Errorf("CountReruns(project) = %d, %v; want 1", n, err)
	}
}

// getRunOfHead is the live run of mr1 for head.
func getRunOfHead(t *testing.T, s *Store, head string) string {
	t.Helper()
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT id::text FROM runs WHERE instance = $1 AND project_id = $2 AND mr_iid = $3
		AND head_sha = $4 AND state <> 'superseded'`, mr1.Instance, mr1.ProjectID, mr1.IID, head).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// One review is one row in the lists, however many attempts it took; the
// dropped attempt reads as fell_back.
func TestListsOneRowPerReview(t *testing.T) {
	s := newStore(t)
	from := reported(t, s, mr1, "a1", `{}`)
	next, _, err := s.EnqueueAttempt(ctx, getRun(t, s, from), quotaMark("m1"))
	if err != nil {
		t.Fatal(err)
	}
	v := seen(mr1.Instance, 7, "acme/widgets")
	got, err := s.ReviewDigests(ctx, ReviewFilter{Visible: v, Limit: 10})
	if err != nil || len(got) != 1 || got[0].ID != next {
		t.Errorf("ReviewDigests = %+v, %v; want the next attempt alone", got, err)
	}
	mrRuns, err := s.MRRunDigests(ctx, mr1, v)
	if err != nil || len(mrRuns) != 1 || mrRuns[0].ID != next {
		t.Errorf("MRRunDigests = %+v, %v; want the next attempt alone", mrRuns, err)
	}
	stats, err := s.RunStats(ctx, v, t0.Add(-24*time.Hour*365))
	if err != nil || stats[PathKey{Instance: mr1.Instance, Path: "acme/widgets"}].Reviews != 1 {
		t.Errorf("RunStats = %+v, %v; want 1 review", stats, err)
	}
	var outcome string
	if err := s.pool.QueryRow(ctx, `SELECT `+outcomeSQL+` FROM runs r WHERE r.id = $1::uuid`, from).Scan(&outcome); err != nil || outcome != OutcomeFellBack {
		t.Errorf("outcome of the dropped attempt = %q, %v; want fell_back", outcome, err)
	}
}

// The usage of a run on a team's key says so, with the team.
func TestRecordUsageSource(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	onTeam := dispatched(t, s, mr1, "a1", "tok-a1")
	if err := s.SetRunEntry(ctx, onTeam, RunEntry{Source: SourceTeam, ID: "t1", Name: "Team key", TeamID: team, Position: 1, Length: 1}); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, onTeam, "tok-a1", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	onGlobal := reported(t, s, mr2, "b1", `{}`)
	for _, id := range []string{onTeam, onGlobal} {
		if err := s.RecordUsage(ctx, id, []SkillUsage{{Skill: "security", In: 1, Out: 1}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	source := func(id string) (string, *int64) {
		t.Helper()
		var src string
		var teamID *int64
		if err := s.pool.QueryRow(ctx, `SELECT source, team_id FROM run_usage WHERE run_id = $1::uuid`, id).Scan(&src, &teamID); err != nil {
			t.Fatal(err)
		}
		return src, teamID
	}
	if src, id := source(onTeam); src != SourceTeam || id == nil || *id != team {
		t.Errorf("team run usage = %s, %v; want team %d", src, id, team)
	}
	if src, id := source(onGlobal); src != SourceGlobal || id != nil {
		t.Errorf("global run usage = %s, %v; want global and no team", src, id)
	}
}

// The instance quota counts only what global keys spent; TokensSince, which
// the repository quota reads, counts every token.
func TestInstanceQuotaCountsGlobalOnly(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	seedUsage(t, s, MRKey{Instance: "main", ProjectID: 1, IID: 1}, at, TriggerPush, false, "m", skillRow("a", "v", 100, 50))
	paid := seedUsage(t, s, MRKey{Instance: "main", ProjectID: 1, IID: 2}, at, TriggerPush, false, "m", skillRow("a", "v", 1000, 0))
	if _, err := s.pool.Exec(ctx, `UPDATE run_usage SET source = 'team', team_id = $2 WHERE run_id = $1::uuid`, paid, team); err != nil {
		t.Fatal(err)
	}
	seedUsage(t, s, MRKey{Instance: "other", ProjectID: 1, IID: 1}, at, TriggerPush, false, "m", skillRow("a", "v", 7, 0))
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if got, err := s.InstanceGlobalTokensSince(ctx, "main", since); err != nil || got != 150 {
		t.Errorf("InstanceGlobalTokensSince = %d, %v; want 150", got, err)
	}
	if got, err := s.TokensSince(ctx, "main", 1, since); err != nil || got != 1150 {
		t.Errorf("TokensSince(repository) = %d, %v; want 1150", got, err)
	}
	if got, err := s.InstanceGlobalTokensSince(ctx, "main", at.Add(time.Hour)); err != nil || got != 0 {
		t.Errorf("InstanceGlobalTokensSince after = %d, %v; want 0", got, err)
	}
}

// A dropped attempt is counted as a fallback, not as a review, a review with
// no result or a skill run; its tokens still count. The exhausted gauge has
// the live marks, by name.
func TestMetricsLeaveOutFellBack(t *testing.T) {
	s := newStore(t)
	main, err := s.CreateLLMModel(ctx, LLMModel{Name: "Main", Provider: "openai", Model: "gpt-5"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	dropped := seedUsage(t, s, usageMR(7, 1), usageAt(5, 10, 0), TriggerPush, false, "gpt-5", skillRow("security", "failed", 30, 0))
	// seedUsage gives a reported run; the dispatcher would have set this.
	setRun(t, s, dropped, `model_source = 'global', model_entry_id = $2, model_entry_name = 'Main', chain_position = 1, chain_length = 2`, main.ID)
	until := time.Now().Add(time.Hour)
	next, _, err := s.EnqueueAttempt(ctx, getRun(t, s, dropped), Mark{Ref: EntryRef{SourceGlobal, main.ID}, Class: ClassQuota, Until: &until, SetAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunEntry(ctx, next, RunEntry{Source: SourceTeam, ID: "t1", Name: "Backup", TeamID: 1, Position: 2, Length: 2}); err != nil {
		t.Fatal(err)
	}
	seedUsage(t, s, usageMR(7, 2), usageAt(5, 11, 0), TriggerPush, false, "gpt-5", skillRow("security", "no_blocking_issues", 10, 1))
	// A dropped attempt that left no result is not a review with no result.
	noResult := dispatched(t, s, usageMR(7, 3), "nores", "tok-nores")
	setRun(t, s, noResult, `state = 'superseded', failure_reason = 'quota', outcome = 'fell_back', outcome_reason = 'quota'`)
	// An expired mark is not exhausted; an auth mark is, with no until.
	gone := time.Now().Add(-time.Hour)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertMark(ctx, tx, Mark{Ref: EntryRef{SourceTeam, "expired"}, Class: ClassBilling, Until: &gone, SetAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := upsertMark(ctx, tx, Mark{Ref: EntryRef{SourceTeam, "rejected"}, Class: ClassAuth, SetAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := s.MetricsTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reviews, skillRuns, tokens int64
	for _, r := range got.Reviews {
		reviews += r.Reviews
	}
	for _, r := range got.SkillRuns {
		skillRuns += r.Runs
	}
	for _, r := range got.Tokens {
		tokens += r.Tokens.In
	}
	if reviews != 1 || skillRuns != 1 || tokens != 40 || len(got.NoResult) != 0 {
		t.Errorf("reviews %d, skill runs %d, tokens in %d, no result %+v; want 1, 1, 40 (the dropped attempt's tokens kept), none",
			reviews, skillRuns, tokens, got.NoResult)
	}
	if want := []FallbackTotal{{From: "Main", To: "Backup", Class: ClassQuota, Source: SourceGlobal, Count: 1},
		{From: "", To: "none", Class: ClassQuota, Source: "", Count: 1}}; !sameFallbacks(got.Fallbacks, want) {
		t.Errorf("fallbacks = %+v; want %+v", got.Fallbacks, want)
	}
	if want := []ExhaustedEntry{{ID: main.ID, Name: "Main", Source: SourceGlobal, Class: ClassQuota},
		{ID: "rejected", Name: "rejected", Source: SourceTeam, Class: ClassAuth}}; !slices.Equal(got.Exhausted, want) {
		t.Errorf("exhausted = %+v; want %+v", got.Exhausted, want)
	}
}

func sameFallbacks(got, want []FallbackTotal) bool {
	key := func(f FallbackTotal) string { return f.From + "|" + f.To + "|" + f.Class + "|" + f.Source }
	cmp := func(a, b FallbackTotal) int { return strings.Compare(key(a), key(b)) }
	got, want = slices.Clone(got), slices.Clone(want)
	slices.SortFunc(got, cmp)
	slices.SortFunc(want, cmp)
	return slices.Equal(got, want)
}

// A dropped attempt is left out of the review list, but RunDigest reads it on
// its own, as fell_back, for the run page the attempts link to.
func TestRunDigestOfDroppedAttempt(t *testing.T) {
	s := newStore(t)
	from := reported(t, s, mr1, "a1", `{}`)
	if _, created, err := s.EnqueueAttempt(ctx, getRun(t, s, from), quotaMark("m1")); err != nil || !created {
		t.Fatalf("EnqueueAttempt = %v, %v", created, err)
	}
	list, err := s.MRRunDigests(ctx, mr1, Visibility{All: true})
	if err != nil || slices.ContainsFunc(list, func(d RunDigest) bool { return d.ID == from }) {
		t.Fatalf("MRRunDigests = %+v, %v; want the dropped attempt left out", list, err)
	}
	d, found, err := s.RunDigest(ctx, from)
	if err != nil || !found || d.ID != from || d.Outcome != OutcomeFellBack {
		t.Errorf("RunDigest = %+v, %v, %v; want the dropped attempt, fell_back", d, found, err)
	}
	if _, found, err := s.RunDigest(ctx, "not-a-uuid"); err != nil || found {
		t.Errorf("RunDigest of a bad id = %v, %v", found, err)
	}
}

// Each digest counts the attempts of the review it is one of: the review
// list's row of a review that fell back, the review page's row of it and the
// dropped attempt read on its own all say 2, and a review that never fell
// back says 1.
func TestDigestsCountAttempts(t *testing.T) {
	s := newStore(t)
	from := reported(t, s, mr1, "a1", `{}`)
	next, _, err := s.EnqueueAttempt(ctx, getRun(t, s, from), quotaMark("m1"))
	if err != nil {
		t.Fatal(err)
	}
	plain := reported(t, s, mr2, "b1", `{}`)
	all := Visibility{All: true}
	list, err := s.ReviewDigests(ctx, ReviewFilter{Visible: all, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, d := range list {
		got[d.ID] = d.Attempts
	}
	if len(got) != 2 || got[next] != 2 || got[plain] != 1 {
		t.Errorf("ReviewDigests attempts = %v; want %s: 2, %s: 1", got, next, plain)
	}
	if mrRuns, err := s.MRRunDigests(ctx, mr1, all); err != nil || len(mrRuns) != 1 || mrRuns[0].Attempts != 2 {
		t.Errorf("MRRunDigests = %+v, %v; want one run of 2 attempts", mrRuns, err)
	}
	if d, found, err := s.RunDigest(ctx, from); err != nil || !found || d.Attempts != 2 {
		t.Errorf("RunDigest(dropped) = %+v, %v, %v; want 2 attempts", d, found, err)
	}
}

// UsageBySource splits one repository's tokens (input plus output) by whose
// key paid, in the period only.
func TestUsageBySource(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	mr := MRKey{Instance: "main", ProjectID: 1, IID: 1}
	seedUsage(t, s, mr, at, TriggerPush, false, "m", skillRow("a", "v", 100, 50))
	paid := seedUsage(t, s, MRKey{Instance: "main", ProjectID: 1, IID: 2}, at, TriggerPush, false, "m", skillRow("a", "v", 1000, 7))
	if _, err := s.pool.Exec(ctx, `UPDATE run_usage SET source = 'team', team_id = $2 WHERE run_id = $1::uuid`, paid, team); err != nil {
		t.Fatal(err)
	}
	// Another repository, and this one before the period: not counted.
	seedUsage(t, s, MRKey{Instance: "main", ProjectID: 2, IID: 1}, at, TriggerPush, false, "m", skillRow("a", "v", 9, 0))
	seedUsage(t, s, MRKey{Instance: "main", ProjectID: 1, IID: 3}, at.Add(-48*time.Hour), TriggerPush, false, "m", skillRow("a", "v", 5, 0))
	since := at.Add(-time.Hour)
	if global, teamTokens, err := s.UsageBySource(ctx, "main", 1, since); err != nil || global != 150 || teamTokens != 1007 {
		t.Errorf("UsageBySource = %d, %d, %v; want 150 and 1007", global, teamTokens, err)
	}
	if global, teamTokens, err := s.UsageBySource(ctx, "main", 3, since); err != nil || global != 0 || teamTokens != 0 {
		t.Errorf("UsageBySource(no usage) = %d, %d, %v; want 0 and 0", global, teamTokens, err)
	}
}

// Each usage row says whose key paid for it.
func TestUsageRowsSource(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	seedUsage(t, s, MRKey{Instance: "main", ProjectID: 1, IID: 1}, at, TriggerPush, false, "m", skillRow("a", "v", 1, 0))
	paid := seedUsage(t, s, MRKey{Instance: "main", ProjectID: 1, IID: 2}, at.Add(time.Minute), TriggerPush, false, "m", skillRow("a", "v", 1, 0))
	if _, err := s.pool.Exec(ctx, `UPDATE run_usage SET source = 'team', team_id = $2 WHERE run_id = $1::uuid`, paid, team); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := s.UsageRows(ctx, at.Add(-time.Hour), "", 0, 10, func(u UsageRow) error {
		got = append(got, u.Source)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{SourceGlobal, SourceTeam}) {
		t.Errorf("sources = %v; want global, then team", got)
	}
}

// A judge retry and an attempt copy the same columns: 2 runs set up the
// same way, one retried and one fallen back, give next attempts that are
// equal on every column but those of the row itself.
func TestJudgeRetryCopiesAsAttempt(t *testing.T) {
	s := newStore(t)
	base := reported(t, s, mr3, "c1", `{}`)
	// Both sources first: a later dispatch would claim a queued attempt.
	froms := map[MRKey]string{}
	for _, mr := range []MRKey{mr1, mr2} {
		tok := "tok-" + strconv.FormatInt(mr.IID, 10)
		from := dispatched(t, s, mr, "a1", tok)
		setRun(t, s, from, `trigger = 'rerun', skill_subset = ARRAY['security'], base_run_id = $2::uuid, quota_checked = true,
			ci_checked = true, project_path = 'acme/copied', status_ref = 42`, base)
		if out, err := s.StoreResult(ctx, from, tok, []byte(`{}`), t0); err != nil || out != ResultStored {
			t.Fatalf("StoreResult = %v, %v", out, err)
		}
		froms[mr] = from
	}
	attemptID, created, err := s.EnqueueAttempt(ctx, getRun(t, s, froms[mr1]), quotaMark("m1"))
	if err != nil || !created {
		t.Fatalf("EnqueueAttempt = %v, %v", created, err)
	}
	retryID, created, err := s.EnqueueJudgeRetry(ctx, getRun(t, s, froms[mr2]))
	if err != nil || !created {
		t.Fatalf("EnqueueJudgeRetry = %v, %v", created, err)
	}
	for _, from := range froms {
		if r := getRun(t, s, from); r.State != Superseded || r.Outcome != OutcomeFellBack {
			t.Errorf("run %s = %s %q; want superseded, fell_back", from, r.State, r.Outcome)
		}
	}
	// Every column but the row's own: its id, merge request, times and first run.
	row := func(id string) map[string]any {
		t.Helper()
		var b []byte
		if err := s.pool.QueryRow(ctx, `SELECT (to_jsonb(r) - 'id' - 'mr_iid' - 'created_at' - 'updated_at' - 'attempt_of')::text
			FROM runs r WHERE id = $1::uuid`, id).Scan(&b); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	attempt, retry := row(attemptID), row(retryID)
	if !reflect.DeepEqual(attempt, retry) {
		for k, v := range attempt {
			if !reflect.DeepEqual(v, retry[k]) {
				t.Errorf("column %s: attempt %v, judge retry %v", k, v, retry[k])
			}
		}
	}
	if attempt["project_path"] != "acme/copied" || attempt["status_ref"] != float64(42) || attempt["trigger"] != "rerun" {
		t.Errorf("attempt = %v; want the copied columns", attempt)
	}
}
