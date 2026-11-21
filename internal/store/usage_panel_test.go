package store

import (
	"testing"
	"time"
)

// panelMembersWithEntries gives a panel review of head on mr1 its 2 members
// on their own entries (A global "main", B team 4's "team-b") and returns
// the run and the members.
func panelMembersWithEntries(t *testing.T, s *Store, head string) (string, PanelMember, PanelMember) {
	t.Helper()
	id := panelRun(t, s, mr1, head)
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	if err := s.SetMemberEntry(ctx, a.ID, RunEntry{Source: SourceGlobal, ID: "m1", Name: "main", Length: 1}, "openai", "gpt-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMemberEntry(ctx, b.ID, RunEntry{Source: SourceTeam, ID: "t1", Name: "team-b", TeamID: 4, Length: 1}, "anthropic", "sonnet-b"); err != nil {
		t.Fatal(err)
	}
	return id, memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
}

// usageRow is one run_usage row of run id and skill.
type usageRow struct {
	in, out          int64
	cache            *int64
	verdict          string
	findings, blocks int
}

func usageOf(t *testing.T, s *Store, id, skill string) (usageRow, bool) {
	t.Helper()
	var u usageRow
	err := s.pool.QueryRow(ctx, `SELECT tokens_in, tokens_out, cache_read, verdict, findings, blocking FROM run_usage
		WHERE run_id = $1::uuid AND skill = $2`, id, skill).Scan(&u.in, &u.out, &u.cache, &u.verdict, &u.findings, &u.blocks)
	if err != nil {
		return usageRow{}, false
	}
	return u, true
}

// modelRow is one run_usage_models row.
type modelRow struct {
	part, role, skill, provider, model, entry, source string
	team                                              *int64
	in, out                                           int64
}

func modelRowsOf(t *testing.T, s *Store, id string) []modelRow {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT part, role, skill, provider, model, entry_name, source, team_id, tokens_in, tokens_out
		FROM run_usage_models WHERE run_id = $1::uuid ORDER BY part DESC, skill, role`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []modelRow
	for rows.Next() {
		var m modelRow
		if err := rows.Scan(&m.part, &m.role, &m.skill, &m.provider, &m.model, &m.entry, &m.source, &m.team, &m.in, &m.out); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// A member's usage: one member row per skill with the member's entry,
// provider and model, its source and team; its tokens are added to the
// review's run_usage row, which has no verdict yet. A second call adds
// nothing, and a member already recorded is not counted again.
func TestRecordMemberUsage(t *testing.T) {
	s := newStore(t)
	id, a, b := panelMembersWithEntries(t, s, "a1")
	doneMember(t, s, a.ID, `{"status":"completed"}`)
	three := int64(3)
	if err := s.RecordMemberUsage(ctx, a.ID, []SkillUsage{{Skill: "security", Verdict: "has_blocking_issues", In: 100, Out: 10, CacheRead: &three, Findings: 2, Blocking: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMemberUsage(ctx, a.ID, []SkillUsage{{Skill: "security", In: 999, Out: 999}}); err != nil {
		t.Fatalf("second RecordMemberUsage: %v", err)
	}
	if err := s.RecordMemberUsage(ctx, b.ID, []SkillUsage{{Skill: "security", In: 50, Out: 5}}); err != nil {
		t.Fatal(err)
	}
	u, ok := usageOf(t, s, id, "security")
	if !ok || u.in != 150 || u.out != 15 || u.verdict != "" || u.findings != 0 || u.blocks != 0 {
		t.Errorf("run_usage = %+v, %v; want A + B's tokens, no verdict or counts yet", u, ok)
	}
	// B did not report its cache counts: A's are kept.
	if u.cache == nil || *u.cache != 3 {
		t.Errorf("cache_read = %v; want A's 3", u.cache)
	}
	team := int64(4)
	want := []modelRow{
		{part: "member", role: "a", skill: "security", provider: "openai", model: "gpt-a", entry: "main", source: "global", in: 100, out: 10},
		{part: "member", role: "b", skill: "security", provider: "anthropic", model: "sonnet-b", entry: "team-b", source: "team", team: &team, in: 50, out: 5},
	}
	got := modelRowsOf(t, s, id)
	if len(got) != len(want) {
		t.Fatalf("model rows = %+v; want %+v", got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if (g.team == nil) != (w.team == nil) || (g.team != nil && *g.team != *w.team) {
			t.Errorf("row %d team = %v, want %v", i, g.team, w.team)
		}
		g.team, w.team = nil, nil
		if g != w {
			t.Errorf("row %d = %+v, want %+v", i, g, w)
		}
	}
	for _, m := range []PanelMember{a, b} {
		var recorded bool
		if err := s.pool.QueryRow(ctx, `SELECT usage_recorded FROM panel_members WHERE id = $1::uuid`, m.ID).Scan(&recorded); err != nil || !recorded {
			t.Errorf("member %s usage_recorded = %v, %v; want true", m.Role, recorded, err)
		}
	}
	// An unknown or off member is no error and writes nothing.
	if err := s.RecordMemberUsage(ctx, "00000000-0000-0000-0000-000000000000", []SkillUsage{{Skill: "security", In: 1}}); err != nil {
		t.Errorf("RecordMemberUsage of an unknown member: %v", err)
	}
	if err := s.RecordMemberUsage(ctx, "bad", nil); err != nil {
		t.Errorf("RecordMemberUsage of a bad id: %v", err)
	}
}

// After A, B and P, run_usage of the review holds A + B + P per skill, with
// P's verdict and counts; P's merge rows carry the judge entries. A second
// record of P adds nothing, and a member recorded after P (the fill at
// start) adds its tokens and keeps P's verdict.
func TestRecordUsagePanelTotal(t *testing.T) {
	s := newStore(t)
	id, a, b := panelMembersWithEntries(t, s, "a1")
	doneMember(t, s, a.ID, `{"status":"completed"}`)
	doneMember(t, s, b.ID, `{"status":"completed"}`)
	if err := s.RecordMemberUsage(ctx, a.ID, []SkillUsage{{Skill: "security", In: 100, Out: 10}, {Skill: "style", In: 7, Out: 1}}); err != nil {
		t.Fatal(err)
	}
	if released, _, err := s.ReleasePanel(ctx, id, t0); err != nil || !released {
		t.Fatalf("ReleasePanel = %v, %v", released, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPanelJudge(ctx, id, "a", RunEntry{Source: SourceGlobal, ID: "m1", Name: "main", Length: 1}, "openai", "gpt-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPanelJudge(ctx, id, "b", RunEntry{Source: SourceTeam, ID: "t1", Name: "team-b", TeamID: 4, Length: 1}, "anthropic", "sonnet-b"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatched(ctx, id, "job-p", "base-sha", "start-sha", hash("p"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, "p", []byte(`{}`), t0.Add(2*time.Minute)); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	p := []SkillUsage{
		{Skill: "security", Verdict: "has_blocking_issues", In: 20, Out: 2, Findings: 3, Blocking: 1},
		{Skill: "style", Verdict: "no_blocking_issues", In: 4, Out: 0},
	}
	models := []ModelUsage{
		{Skill: "security", Role: "a", Provider: "openai", Model: "gpt-a", In: 12, Out: 1},
		{Skill: "security", Role: "b", Provider: "anthropic", Model: "sonnet-b", In: 8, Out: 1},
	}
	if err := s.RecordUsage(ctx, id, p, models); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, id, p, models); err != nil {
		t.Fatalf("second RecordUsage: %v", err)
	}
	// B's write was lost and comes from the fill, after P.
	if err := s.RecordMemberUsage(ctx, b.ID, []SkillUsage{{Skill: "security", Verdict: "no_blocking_issues", In: 50, Out: 5, Findings: 9}}); err != nil {
		t.Fatal(err)
	}
	for skill, want := range map[string]usageRow{
		"security": {in: 170, out: 17, verdict: "has_blocking_issues", findings: 3, blocks: 1},
		"style":    {in: 11, out: 1, verdict: "no_blocking_issues"},
	} {
		got, ok := usageOf(t, s, id, skill)
		got.cache = nil
		if !ok || got != want {
			t.Errorf("run_usage %s = %+v, %v; want %+v", skill, got, ok, want)
		}
	}
	team := int64(4)
	var merge []modelRow
	for _, m := range modelRowsOf(t, s, id) {
		if m.part == "merge" {
			merge = append(merge, m)
		}
	}
	if len(merge) != 2 || merge[0].role != "a" || merge[0].entry != "main" || merge[0].source != "global" || merge[0].team != nil ||
		merge[0].in != 12 || merge[1].role != "b" || merge[1].entry != "team-b" || merge[1].source != "team" ||
		merge[1].team == nil || *merge[1].team != team || merge[1].model != "sonnet-b" || merge[1].in != 8 {
		t.Errorf("merge rows = %+v; want one per role with the judge entries", merge)
	}
	var recorded bool
	if err := s.pool.QueryRow(ctx, `SELECT usage_recorded FROM runs WHERE id = $1::uuid`, id).Scan(&recorded); err != nil || !recorded {
		t.Errorf("run usage_recorded = %v, %v; want true", recorded, err)
	}
	// The quota's sums see all 3 Jobs.
	n, err := s.TokensSince(ctx, mr1.Instance, mr1.ProjectID, t0.Add(-time.Hour))
	if err != nil || n != 170+17+11+1 {
		t.Errorf("TokensSince = %d, %v; want A + B + P", n, err)
	}
}

// A run with no model rows is recorded with today's statement: a row
// already there is kept and its tokens are not added to.
func TestRecordUsageWithoutModelsUnchanged(t *testing.T) {
	s := newStore(t)
	id := reported(t, s, mr1, "head1", `{"schema_version":1,"status":"completed","skills":[]}`)
	if err := s.RecordUsage(ctx, id, []SkillUsage{{Skill: "security", Verdict: "failed", In: 10, Out: 2, Findings: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, id, []SkillUsage{{Skill: "security", Verdict: "no_blocking_issues", In: 99}, {Skill: "style", In: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if u, ok := usageOf(t, s, id, "security"); !ok || u.in != 10 || u.out != 2 || u.verdict != "failed" || u.findings != 1 {
		t.Errorf("run_usage = %+v, %v; want the first row kept", u, ok)
	}
	// Today's statement adds a skill that is not there yet.
	if _, ok := usageOf(t, s, id, "style"); !ok {
		t.Error("a new skill's row was not added")
	}
	if got := modelRowsOf(t, s, id); len(got) != 0 {
		t.Errorf("model rows = %+v; want none", got)
	}
}

// A member result whose usage write was lost (a restart) is listed for the
// fill at start, which records it once; an off row, a member with no result
// and a recorded member are not listed.
func TestMembersWithoutUsageFill(t *testing.T) {
	s := newStore(t)
	id, a, b := panelMembersWithEntries(t, s, "a1")
	doneMember(t, s, a.ID, `{"status":"completed","skills":[]}`)
	failedMember(t, s, b.ID, "boom")
	off := enqueue(t, s, mr2, "o1")
	if err := s.RecordPanelOff(ctx, off, "the review image does not support it"); err != nil {
		t.Fatal(err)
	}
	ms, err := s.MembersWithoutUsage(ctx, 10)
	if err != nil || len(ms) != 1 || ms[0].ID != a.ID || ms[0].Result == nil || ms[0].RunID != id {
		t.Fatalf("MembersWithoutUsage = %+v, %v; want member A with its result", ms, err)
	}
	if err := s.RecordMemberUsage(ctx, a.ID, []SkillUsage{{Skill: "security", In: 5, Out: 1}}); err != nil {
		t.Fatal(err)
	}
	if ms, err := s.MembersWithoutUsage(ctx, 10); err != nil || len(ms) != 0 {
		t.Errorf("MembersWithoutUsage after the fill = %+v, %v; want none", ms, err)
	}
	if u, ok := usageOf(t, s, id, "security"); !ok || u.in != 5 {
		t.Errorf("run_usage = %+v, %v; want the filled tokens", u, ok)
	}
	if ms, err := s.MembersWithoutUsage(ctx, 0); err != nil || len(ms) != 0 {
		t.Errorf("MembersWithoutUsage(0) = %+v, %v; want none", ms, err)
	}
}

// The off row counts for the claim of the run that recorded it. A run
// claimed again (a launch that failed, retried) whose dispatch recorded no
// off row (chain B removed meanwhile) has a stale one: it is not the run's.
// A later attempt that records it again has it.
func TestPanelOffReasonStale(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if _, ok, err := s.PanelOffReason(ctx, id); err != nil || ok {
		t.Fatalf("PanelOffReason before any = %v, %v; want none", ok, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPanelOff(ctx, id, "the review image does not support it"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.PanelOffReason(ctx, id); err != nil || !ok || got != "the review image does not support it" {
		t.Fatalf("PanelOffReason = %q, %v, %v; want the reason", got, ok, err)
	}
	// The launch failed: the run is queued and claimed again, and this
	// dispatch has no chain B, so it records nothing.
	if err := s.Requeue(ctx, id, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.PanelOffReason(ctx, id); err != nil || ok {
		t.Errorf("PanelOffReason after a claim with no off = %q, %v, %v; want none", got, ok, err)
	}
	// The same claim records it again: it counts.
	if err := s.RecordPanelOff(ctx, id, "the token quota stopped model B"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.PanelOffReason(ctx, id); err != nil || !ok || got != "the token quota stopped model B" {
		t.Errorf("PanelOffReason = %q, %v, %v; want the new reason", got, ok, err)
	}
	// Another run of the review has no claim of its own yet: not its.
	if err := s.MarkDispatched(ctx, id, "job", "base", "start", hash("t"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, "t", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	next, _, err := s.EnqueueAttempt(ctx, getRun(t, s, id), quotaMark("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.PanelOffReason(ctx, next); err != nil || ok {
		t.Errorf("PanelOffReason of the next attempt = %v, %v; want none", ok, err)
	}
	if _, ok, err := s.PanelOffReason(ctx, "bad"); err != nil || ok {
		t.Errorf("PanelOffReason of a bad id = %v, %v; want none", ok, err)
	}
}

// The provider and model each role of a Job P run judged with.
func TestPanelJudgeModels(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if err := s.SetPanelJudge(ctx, id, "a", RunEntry{Source: SourceGlobal, ID: "m1", Name: "main"}, "openai", "gpt-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPanelJudge(ctx, id, "b", RunEntry{Source: SourceTeam, ID: "t1", Name: "team", TeamID: 4}, "anthropic", "sonnet-b"); err != nil {
		t.Fatal(err)
	}
	got, err := s.PanelJudgeModels(ctx, id)
	if err != nil || len(got) != 2 || got["a"] != (ModelUsed{"openai", "gpt-a"}) || got["b"] != (ModelUsed{"anthropic", "sonnet-b"}) {
		t.Errorf("PanelJudgeModels = %+v, %v", got, err)
	}
	if got, err := s.PanelJudgeModels(ctx, "bad"); err != nil || len(got) != 0 {
		t.Errorf("PanelJudgeModels of a bad id = %+v, %v; want none", got, err)
	}
}

// A count is added when both sides report it, kept when one side does, and
// stays unknown only when neither does.
func TestRecordUsageCacheKeepsKnownSide(t *testing.T) {
	s := newStore(t)
	id, a, b := panelMembersWithEntries(t, s, "a1")
	two, five := int64(2), int64(5)
	if err := s.RecordMemberUsage(ctx, a.ID, []SkillUsage{{Skill: "both", CacheRead: &two}, {Skill: "one", CacheRead: &two}, {Skill: "none"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMemberUsage(ctx, b.ID, []SkillUsage{{Skill: "both", CacheRead: &five}, {Skill: "one"}, {Skill: "none"}}); err != nil {
		t.Fatal(err)
	}
	for skill, want := range map[string]*int64{"both": ptr(int64(7)), "one": ptr(int64(2)), "none": nil} {
		u, ok := usageOf(t, s, id, skill)
		if !ok || (u.cache == nil) != (want == nil) || (want != nil && *u.cache != *want) {
			t.Errorf("%s cache_read = %v, %v; want %v", skill, u.cache, ok, want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// A review that fell back on chain A before its split: the split run (a
// later attempt, the one published) holds the members' and P's tokens, the
// dropped first attempt only its own, the member rows too. A member filled
// after the release goes to the split run as well.
func TestMemberUsageOnSplitRun(t *testing.T) {
	s := newStore(t)
	first := reported(t, s, mr1, "a1", `{}`)
	if err := s.RecordUsage(ctx, first, []SkillUsage{{Skill: "security", Verdict: "failed", In: 9}}, nil); err != nil {
		t.Fatal(err)
	}
	split, _, err := s.EnqueueAttempt(ctx, getRun(t, s, first), quotaMark("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if started, err := s.StartPanel(ctx, split, testSplit, t0); err != nil || !started {
		t.Fatalf("StartPanel = %v, %v", started, err)
	}
	a, b := memberOf(t, s, first, "a"), memberOf(t, s, first, "b")
	doneMember(t, s, a.ID, `{"status":"completed"}`)
	doneMember(t, s, b.ID, `{"status":"completed"}`)
	// A's write while the run waits.
	if err := s.RecordMemberUsage(ctx, a.ID, []SkillUsage{{Skill: "security", In: 100}}); err != nil {
		t.Fatal(err)
	}
	if released, _, err := s.ReleasePanel(ctx, first, t0); err != nil || !released {
		t.Fatalf("ReleasePanel = %v, %v", released, err)
	}
	// B's from the fill, after the release.
	if err := s.RecordMemberUsage(ctx, b.ID, []SkillUsage{{Skill: "security", In: 50}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatched(ctx, split, "job-p", "base-sha", "start-sha", hash("p"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, split, "p", []byte(`{}`), t0.Add(2*time.Minute)); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	if err := s.RecordUsage(ctx, split, []SkillUsage{{Skill: "security", Verdict: "no_blocking_issues", In: 20}}, []ModelUsage{}); err != nil {
		t.Fatal(err)
	}
	if u, ok := usageOf(t, s, split, "security"); !ok || u.in != 170 || u.verdict != "no_blocking_issues" {
		t.Errorf("split run usage = %+v, %v; want A + B + P with P's verdict", u, ok)
	}
	if u, ok := usageOf(t, s, first, "security"); !ok || u.in != 9 || u.verdict != "failed" {
		t.Errorf("dropped attempt usage = %+v, %v; want its own 9 tokens only", u, ok)
	}
	if in, _, _, err := s.RunUsage(ctx, split); err != nil || in != 170 {
		t.Errorf("RunUsage(split) = %d, %v; want 170", in, err)
	}
	if rows := modelRowsOf(t, s, split); len(rows) != 2 {
		t.Errorf("member rows on the split run = %+v; want A's and B's", rows)
	}
	if rows := modelRowsOf(t, s, first); len(rows) != 0 {
		t.Errorf("member rows on the dropped attempt = %+v; want none", rows)
	}
}

// A panel whose merge left no result (Job P timed out): the review counts
// once, as a review with no result, and its members' tokens are wasted.
func TestUsagePanelNoResult(t *testing.T) {
	s := newStore(t)
	mr := usageMR(7, 1)
	id := enqueue(t, s, mr, "a1")
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if started, err := s.StartPanel(ctx, id, testSplit, t0); err != nil || !started {
		t.Fatalf("StartPanel = %v, %v", started, err)
	}
	a := memberOf(t, s, id, "a")
	doneMember(t, s, a.ID, `{"status":"completed"}`)
	if err := s.RecordMemberUsage(ctx, a.ID, []SkillUsage{{Skill: "security", In: 100, Out: 10}}); err != nil {
		t.Fatal(err)
	}
	// P ran and left no result.
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'reported', skip_reason = '', failure_reason = 'the Job timed out',
		failure_step = 'review', reported_at = $2 WHERE id = $1::uuid`, id, usageAt(5, 10, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE run_usage SET reported_at = $2 WHERE run_id = $1::uuid`, id, usageAt(5, 10, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET created_at = $2 WHERE id = $1::uuid`, id, usageAt(5, 9, 0)); err != nil {
		t.Fatal(err)
	}
	w, err := s.UsageWasted(ctx, mr.Instance, 7, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	if w.Failed.In != 100 || w.Failed.Out != 10 || w.NoResult != 1 {
		t.Errorf("wasted = %+v; want the members' tokens as failed and one review with no result", w)
	}
	m, err := s.MetricsTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Reviews) != 0 || len(m.SkillRuns) != 0 {
		t.Errorf("reviews %+v, skill runs %+v; want none: the review has no result", m.Reviews, m.SkillRuns)
	}
	if len(m.NoResult) != 1 || m.NoResult[0].Reviews != 1 {
		t.Errorf("no result = %+v; want the review once", m.NoResult)
	}
	if len(m.Tokens) != 1 || m.Tokens[0].Tokens.In != 100 {
		t.Errorf("tokens = %+v; want the members' 100 kept", m.Tokens)
	}
}
