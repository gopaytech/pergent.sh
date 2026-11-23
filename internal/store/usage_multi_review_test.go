package store

import (
	"strconv"
	"testing"
	"time"
)

// judgeEntry is the entry the test judge runs on: team 4's "judge-team".
var judgeEntry = RunEntry{Source: SourceTeam, ID: "j1", Name: "judge-team", TeamID: 4, Position: 1, Length: 2}

// multiReviewWithEntries gives a multi review of head on mr reviewers 1 to
// n, each on its own entry: odd numbers global "main-<k>", even numbers team
// 4's "team-<k>", provider "p<k>" and model "m<k>". It returns the run and
// the members.
func multiReviewWithEntries(t *testing.T, s *Store, mr MRKey, head string, n int) (string, []ReviewMember) {
	t.Helper()
	id := multiRun(t, s, mr, head, multiSplit(n, nil))
	ms := make([]ReviewMember, n)
	for k := 1; k <= n; k++ {
		e := RunEntry{Source: SourceGlobal, ID: "e" + strconv.Itoa(k), Name: "main-" + strconv.Itoa(k), Position: 1, Length: 2}
		if k%2 == 0 {
			e = RunEntry{Source: SourceTeam, ID: "e" + strconv.Itoa(k), Name: "team-" + strconv.Itoa(k), TeamID: 4, Position: 1, Length: 2}
		}
		m := reviewerOf(t, s, id, k)
		if err := s.SetReviewMemberEntry(ctx, m.ID, e, "p"+strconv.Itoa(k), "m"+strconv.Itoa(k)); err != nil {
			t.Fatal(err)
		}
		ms[k-1] = reviewerOf(t, s, id, k)
	}
	return id, ms
}

// judgeRun takes the multi review id, whose reviewers are final, to its
// judge run: released, claimed, the judge set to j, dispatched and reported.
func judgeRun(t *testing.T, s *Store, id string, j JudgeRow) {
	t.Helper()
	if released, _, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || !released {
		t.Fatalf("ReleaseMultiReview = %v, %v", released, err)
	}
	reportJudge(t, s, id, j)
}

// reportJudge claims the queued judge run id, sets its judge to j, and
// takes it through dispatch to a stored result.
func reportJudge(t *testing.T, s *Store, id string, j JudgeRow) {
	t.Helper()
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetJudge(ctx, id, j); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatched(ctx, id, "job-j-"+id, "base-sha", "start-sha", hash("j-"+id), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, "j-"+id, []byte(`{}`), t0.Add(2*time.Minute)); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
}

// byModelRow is one run_usage_by_model row.
type byModelRow struct {
	part                                       string
	reviewer                                   int
	member, skill, provider, model, entry, src string
	team                                       int64
	in, out                                    int64
}

// byModelRowsOf returns the run_usage_by_model rows of run id: reviewers by
// number and attempt, then the judge's, by skill. team is 0 for none.
func byModelRowsOf(t *testing.T, s *Store, id string) []byModelRow {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT u.part, COALESCE(u.reviewer, 0), COALESCE(u.member_id::text, ''), u.skill,
			u.provider, u.model, u.entry_name, u.source, COALESCE(u.team_id, 0), u.tokens_in, u.tokens_out
		FROM run_usage_by_model u LEFT JOIN review_members m ON m.id = u.member_id
		WHERE u.run_id = $1::uuid ORDER BY u.part = 'judge', u.reviewer, m.created_at, u.skill`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []byModelRow
	for rows.Next() {
		var r byModelRow
		if err := rows.Scan(&r.part, &r.reviewer, &r.member, &r.skill, &r.provider, &r.model, &r.entry, &r.src,
			&r.team, &r.in, &r.out); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func runRecorded(t *testing.T, s *Store, id string) bool {
	t.Helper()
	var recorded bool
	if err := s.pool.QueryRow(ctx, `SELECT usage_recorded FROM runs WHERE id = $1::uuid`, id).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	return recorded
}

func reviewMemberRecorded(t *testing.T, s *Store, id string) bool {
	t.Helper()
	var recorded bool
	if err := s.pool.QueryRow(ctx, `SELECT usage_recorded FROM review_members WHERE id = $1::uuid`, id).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// A reviewer's usage: one reviewer row per skill with the member's entry,
// provider, model, source, team and number; its tokens are added to the
// run_usage row of the split run, which has no verdict yet. A second call
// adds nothing. Unknown, bad and off members are no error and write nothing.
func TestRecordReviewMemberUsage(t *testing.T) {
	s := newStore(t)
	id, ms := multiReviewWithEntries(t, s, mr1, "a1", 2)
	doneReviewer(t, s, ms[0].ID, `{"status":"completed"}`)
	failedReviewer(t, s, ms[1].ID, "boom")
	three := int64(3)
	if err := s.RecordReviewMemberUsage(ctx, ms[0].ID, []SkillUsage{
		{Skill: "style", In: 7, Out: 1},
		{Skill: "security", Verdict: "has_blocking_issues", In: 100, Out: 10, CacheRead: &three, Findings: 2, Blocking: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordReviewMemberUsage(ctx, ms[0].ID, []SkillUsage{{Skill: "security", In: 999, Out: 999}}); err != nil {
		t.Fatalf("second RecordReviewMemberUsage: %v", err)
	}
	// A reviewer that failed records the tokens it reported all the same.
	if err := s.RecordReviewMemberUsage(ctx, ms[1].ID, []SkillUsage{{Skill: "security", In: 50, Out: 5}}); err != nil {
		t.Fatal(err)
	}
	u, ok := usageOf(t, s, id, "security")
	if !ok || u.in != 150 || u.out != 15 || u.verdict != "" || u.findings != 0 || u.blocks != 0 {
		t.Errorf("run_usage = %+v, %v; want reviewer 1 + 2's tokens, no verdict or counts yet", u, ok)
	}
	if u.cache == nil || *u.cache != 3 {
		t.Errorf("cache_read = %v; want reviewer 1's 3", u.cache)
	}
	want := []byModelRow{
		{part: "reviewer", reviewer: 1, member: ms[0].ID, skill: "security", provider: "p1", model: "m1", entry: "main-1", src: "global", in: 100, out: 10},
		{part: "reviewer", reviewer: 1, member: ms[0].ID, skill: "style", provider: "p1", model: "m1", entry: "main-1", src: "global", in: 7, out: 1},
		{part: "reviewer", reviewer: 2, member: ms[1].ID, skill: "security", provider: "p2", model: "m2", entry: "team-2", src: "team", team: 4, in: 50, out: 5},
	}
	got := byModelRowsOf(t, s, id)
	if len(got) != len(want) {
		t.Fatalf("model rows = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, m := range ms {
		if !reviewMemberRecorded(t, s, m.ID) {
			t.Errorf("reviewer %d usage_recorded = false; want true", m.Reviewer)
		}
	}
	// No legacy panel table is written.
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM run_usage_models WHERE run_id = $1::uuid`, id).Scan(&n); err != nil || n != 0 {
		t.Errorf("run_usage_models = %d rows, %v; want none", n, err)
	}
	if err := s.RecordReviewMemberUsage(ctx, "00000000-0000-0000-0000-000000000000", []SkillUsage{{Skill: "security", In: 1}}); err != nil {
		t.Errorf("RecordReviewMemberUsage of an unknown member: %v", err)
	}
	if err := s.RecordReviewMemberUsage(ctx, "bad", nil); err != nil {
		t.Errorf("RecordReviewMemberUsage of a bad id: %v", err)
	}
	off := enqueue(t, s, mr2, "o1")
	if err := s.RecordMultiReviewOff(ctx, off, "the review image does not support it"); err != nil {
		t.Fatal(err)
	}
	offs, err := s.ReviewMembers(ctx, off)
	if err != nil || len(offs) != 1 || offs[0].State != MemberOff {
		t.Fatalf("ReviewMembers(off) = %+v, %v; want the off row", offs, err)
	}
	if err := s.RecordReviewMemberUsage(ctx, offs[0].ID, []SkillUsage{{Skill: "security", In: 1}}); err != nil {
		t.Errorf("RecordReviewMemberUsage of an off row: %v", err)
	}
	if _, ok := usageOf(t, s, off, "security"); ok {
		t.Error("an off row wrote usage")
	}
}

// A review that fell back on reviewer 1's chain before its split: the split
// run (a later attempt) holds the reviewers' tokens and rows, the dropped
// first attempt only its own. A reviewer filled after the release goes to
// the split run too.
func TestRecordReviewMemberUsageOnSplitRun(t *testing.T) {
	s := newStore(t)
	first := reported(t, s, mr1, "a1", `{}`)
	if err := s.RecordUsage(ctx, first, []SkillUsage{{Skill: "security", Verdict: "failed", In: 9}}); err != nil {
		t.Fatal(err)
	}
	split, _, err := s.EnqueueAttempt(ctx, getRun(t, s, first), quotaMark("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if started, err := s.StartMultiReview(ctx, split, multiSplit(2, nil), t0); err != nil || !started {
		t.Fatalf("StartMultiReview = %v, %v", started, err)
	}
	r1, r2 := reviewerOf(t, s, first, 1), reviewerOf(t, s, first, 2)
	doneReviewer(t, s, r1.ID, `{}`)
	doneReviewer(t, s, r2.ID, `{}`)
	if err := s.RecordReviewMemberUsage(ctx, r1.ID, []SkillUsage{{Skill: "security", In: 100}}); err != nil {
		t.Fatal(err)
	}
	if released, _, err := s.ReleaseMultiReview(ctx, first, t0); err != nil || !released {
		t.Fatalf("ReleaseMultiReview = %v, %v", released, err)
	}
	if err := s.RecordReviewMemberUsage(ctx, r2.ID, []SkillUsage{{Skill: "security", In: 50}}); err != nil {
		t.Fatal(err)
	}
	if u, ok := usageOf(t, s, split, "security"); !ok || u.in != 150 {
		t.Errorf("split run usage = %+v, %v; want reviewer 1 + 2", u, ok)
	}
	if u, ok := usageOf(t, s, first, "security"); !ok || u.in != 9 || u.verdict != "failed" {
		t.Errorf("dropped attempt usage = %+v, %v; want its own 9 tokens only", u, ok)
	}
	if rows := byModelRowsOf(t, s, split); len(rows) != 2 {
		t.Errorf("reviewer rows on the split run = %+v; want 2", rows)
	}
	if rows := byModelRowsOf(t, s, first); len(rows) != 0 {
		t.Errorf("reviewer rows on the dropped attempt = %+v; want none", rows)
	}
}

// A reviewer result whose usage write was lost is listed for the fill at
// start, once; an off row, a reviewer with no result, a skipped reviewer and
// a recorded one are not.
func TestReviewMembersWithoutUsage(t *testing.T) {
	s := newStore(t)
	id := multiRun(t, s, mr1, "a1", multiSplit(3, map[int]string{3: "no backup entry left"}))
	r1, r2 := reviewerOf(t, s, id, 1), reviewerOf(t, s, id, 2)
	doneReviewer(t, s, r1.ID, `{"status":"completed","skills":[]}`)
	failedReviewer(t, s, r2.ID, "boom")
	off := enqueue(t, s, mr2, "o1")
	if err := s.RecordMultiReviewOff(ctx, off, "the review image does not support it"); err != nil {
		t.Fatal(err)
	}
	ms, err := s.ReviewMembersWithoutUsage(ctx, 10)
	if err != nil || len(ms) != 1 || ms[0].ID != r1.ID || ms[0].Result == nil || ms[0].RunID != id || ms[0].Reviewer != 1 {
		t.Fatalf("ReviewMembersWithoutUsage = %+v, %v; want reviewer 1 with its result", ms, err)
	}
	if err := s.RecordReviewMemberUsage(ctx, r1.ID, []SkillUsage{{Skill: "security", In: 5, Out: 1}}); err != nil {
		t.Fatal(err)
	}
	if ms, err := s.ReviewMembersWithoutUsage(ctx, 10); err != nil || len(ms) != 0 {
		t.Errorf("ReviewMembersWithoutUsage after the fill = %+v, %v; want none", ms, err)
	}
	if ms, err := s.ReviewMembersWithoutUsage(ctx, 0); err != nil || len(ms) != 0 {
		t.Errorf("ReviewMembersWithoutUsage(0) = %+v, %v; want none", ms, err)
	}
}

// After reviewers 1, 2, 3 and the judge, run_usage of the review holds the
// sum per skill with the judge's verdict and counts; the judge rows carry
// the review_judges entry. A second record of the judge adds nothing, and a
// reviewer filled after the judge adds its tokens and keeps the verdict.
func TestRecordJudgeUsageTotal(t *testing.T) {
	s := newStore(t)
	id, ms := multiReviewWithEntries(t, s, mr1, "a1", 3)
	for _, m := range ms {
		doneReviewer(t, s, m.ID, `{"status":"completed"}`)
	}
	if err := s.RecordReviewMemberUsage(ctx, ms[0].ID, []SkillUsage{{Skill: "security", In: 100, Out: 10}, {Skill: "style", In: 7, Out: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordReviewMemberUsage(ctx, ms[1].ID, []SkillUsage{{Skill: "security", In: 50, Out: 5}}); err != nil {
		t.Fatal(err)
	}
	judgeRun(t, s, id, JudgeRow{Reviewers: []int{1, 2, 3}, Provider: "anthropic", Model: "judge-m", Entry: judgeEntry})
	j := []SkillUsage{
		{Skill: "style", Verdict: "no_blocking_issues", In: 4, Out: 0},
		{Skill: "security", Verdict: "has_blocking_issues", In: 20, Out: 2, Findings: 3, Blocking: 1},
	}
	if err := s.RecordJudgeUsage(ctx, id, j); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordJudgeUsage(ctx, id, j); err != nil {
		t.Fatalf("second RecordJudgeUsage: %v", err)
	}
	// Reviewer 3's write was lost and comes from the fill, after the judge.
	if err := s.RecordReviewMemberUsage(ctx, ms[2].ID, []SkillUsage{{Skill: "security", Verdict: "no_blocking_issues", In: 30, Out: 3, Findings: 9}}); err != nil {
		t.Fatal(err)
	}
	for skill, want := range map[string]usageRow{
		"security": {in: 200, out: 20, verdict: "has_blocking_issues", findings: 3, blocks: 1},
		"style":    {in: 11, out: 1, verdict: "no_blocking_issues"},
	} {
		got, ok := usageOf(t, s, id, skill)
		got.cache = nil
		if !ok || got != want {
			t.Errorf("run_usage %s = %+v, %v; want %+v", skill, got, ok, want)
		}
	}
	var judge []byModelRow
	for _, r := range byModelRowsOf(t, s, id) {
		if r.part == "judge" {
			judge = append(judge, r)
		}
	}
	want := []byModelRow{
		{part: "judge", skill: "security", provider: "anthropic", model: "judge-m", entry: "judge-team", src: "team", team: 4, in: 20, out: 2},
		{part: "judge", skill: "style", provider: "anthropic", model: "judge-m", entry: "judge-team", src: "team", team: 4, in: 4},
	}
	if len(judge) != len(want) || judge[0] != want[0] || judge[1] != want[1] {
		t.Errorf("judge rows = %+v; want %+v", judge, want)
	}
	if !runRecorded(t, s, id) {
		t.Error("run usage_recorded = false; want true")
	}
	// The quota's sums see all 4 Jobs.
	n, err := s.TokensSince(ctx, mr1.Instance, mr1.ProjectID, t0.Add(-time.Hour))
	if err != nil || n != 200+20+11+1 {
		t.Errorf("TokensSince = %d, %v; want every reviewer and the judge", n, err)
	}
	if in, _, found, err := s.RunUsage(ctx, id); err != nil || !found || in != 211 {
		t.Errorf("RunUsage = %d, %v, %v; want 211", in, found, err)
	}
	if err := s.RecordJudgeUsage(ctx, "00000000-0000-0000-0000-000000000000", j); err != nil {
		t.Errorf("RecordJudgeUsage of an unknown run: %v", err)
	}
	if err := s.RecordJudgeUsage(ctx, "bad", j); err != nil {
		t.Errorf("RecordJudgeUsage of a bad id: %v", err)
	}
}

// A judge run with --no-judge adds its tokens, verdict and counts to the
// total and writes no judge row.
func TestRecordJudgeUsageNoJudgeWritesNoModelRow(t *testing.T) {
	s := newStore(t)
	id, ms := multiReviewWithEntries(t, s, mr1, "a1", 2)
	for _, m := range ms {
		doneReviewer(t, s, m.ID, `{}`)
		if err := s.RecordReviewMemberUsage(ctx, m.ID, []SkillUsage{{Skill: "security", In: 10, Out: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	judgeRun(t, s, id, JudgeRow{Reviewers: []int{1, 2}, NoJudge: true, NoJudgeReason: "the judge chain has no entry"})
	if err := s.RecordJudgeUsage(ctx, id, []SkillUsage{{Skill: "security", Verdict: "no_blocking_issues", In: 3, Out: 1, Findings: 1}}); err != nil {
		t.Fatal(err)
	}
	if u, ok := usageOf(t, s, id, "security"); !ok || u.in != 23 || u.out != 3 || u.verdict != "no_blocking_issues" || u.findings != 1 {
		t.Errorf("run_usage = %+v, %v; want both reviewers + the run, with its verdict", u, ok)
	}
	rows := byModelRowsOf(t, s, id)
	if len(rows) != 2 || rows[0].part != "reviewer" || rows[1].part != "reviewer" {
		t.Errorf("model rows = %+v; want the 2 reviewer rows and no judge row", rows)
	}
	if !runRecorded(t, s, id) {
		t.Error("run usage_recorded = false; want true")
	}
}

// ReviewCosts lists reviewers by number and attempt, then the judge runs
// oldest first, each summed over its skills; any run of the review reads
// the same list.
func TestReviewCostsOrder(t *testing.T) {
	s := newStore(t)
	id, ms := multiReviewWithEntries(t, s, mr1, "a1", 3)
	doneReviewer(t, s, ms[0].ID, `{}`)
	doneReviewer(t, s, ms[2].ID, `{}`)
	// Reviewer 2 falls back to its second entry, which ends done.
	reportReviewer(t, s, ms[1].ID, `{}`)
	second, created, err := s.EnqueueReviewMemberAttempt(ctx, reviewerOf(t, s, id, 2), quotaMark("e2"))
	if err != nil || !created {
		t.Fatal(second, created, err)
	}
	if err := s.SetReviewMemberEntry(ctx, second, RunEntry{Source: SourceGlobal, ID: "e2b", Name: "main-2b", Position: 2, Length: 2}, "p2b", "m2b"); err != nil {
		t.Fatal(err)
	}
	doneReviewer(t, s, second, `{}`)
	for i, rec := range []struct {
		id     string
		skills []SkillUsage
	}{
		// Reviewer 3 first: the order is by number, not by write.
		{ms[2].ID, []SkillUsage{{Skill: "security", In: 3, Out: 1}}},
		{second, []SkillUsage{{Skill: "security", In: 22, Out: 2}}},
		{ms[1].ID, []SkillUsage{{Skill: "security", In: 21, Out: 1}}},
		{ms[0].ID, []SkillUsage{{Skill: "security", In: 10, Out: 1}, {Skill: "style", In: 1, Out: 1}}},
	} {
		if err := s.RecordReviewMemberUsage(ctx, rec.id, rec.skills); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	judgeRun(t, s, id, JudgeRow{Reviewers: []int{1, 2, 3}, Provider: "anthropic", Model: "judge-m", Entry: judgeEntry})
	if err := s.RecordJudgeUsage(ctx, id, []SkillUsage{{Skill: "security", In: 5, Out: 1}, {Skill: "style", In: 2}}); err != nil {
		t.Fatal(err)
	}
	// The judge crashed: its next attempt runs with --no-judge.
	retry, created, err := s.EnqueueJudgeRetry(ctx, getRun(t, s, id))
	if err != nil || !created {
		t.Fatal(retry, created, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET created_at = created_at + interval '1 hour' WHERE id = $1::uuid`, retry); err != nil {
		t.Fatal(err)
	}
	reportJudge(t, s, retry, JudgeRow{Reviewers: []int{1, 2, 3}, Provider: "openai", Model: "judge-2",
		Entry: RunEntry{Source: SourceGlobal, ID: "j2", Name: "judge-2", Position: 2, Length: 2}})
	if err := s.RecordJudgeUsage(ctx, retry, []SkillUsage{{Skill: "security", In: 6, Out: 2}}); err != nil {
		t.Fatal(err)
	}
	want := []ReviewCost{
		{Part: "reviewer", Reviewer: 1, RunID: id, MemberID: ms[0].ID, Entry: "main-1", Provider: "p1", Model: "m1", In: 11, Out: 2},
		{Part: "reviewer", Reviewer: 2, RunID: id, MemberID: ms[1].ID, Entry: "team-2", Provider: "p2", Model: "m2", In: 21, Out: 1},
		{Part: "reviewer", Reviewer: 2, RunID: id, MemberID: second, Entry: "main-2b", Provider: "p2b", Model: "m2b", In: 22, Out: 2},
		{Part: "reviewer", Reviewer: 3, RunID: id, MemberID: ms[2].ID, Entry: "main-3", Provider: "p3", Model: "m3", In: 3, Out: 1},
		{Part: "judge", RunID: id, Entry: "judge-team", Provider: "anthropic", Model: "judge-m", In: 7, Out: 1},
		{Part: "judge", RunID: retry, Entry: "judge-2", Provider: "openai", Model: "judge-2", In: 6, Out: 2},
	}
	for _, from := range []string{id, retry} {
		got, err := s.ReviewCosts(ctx, from)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("ReviewCosts(%s) = %+v; want %+v", from, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("ReviewCosts(%s)[%d] = %+v, want %+v", from, i, got[i], want[i])
			}
		}
	}
	// Another review, a single review and a bad id have none.
	other := reported(t, s, mr2, "b1", `{}`)
	if err := s.RecordUsage(ctx, other, []SkillUsage{{Skill: "security", In: 1}}); err != nil {
		t.Fatal(err)
	}
	for _, none := range []string{other, "bad", "00000000-0000-0000-0000-000000000000"} {
		if got, err := s.ReviewCosts(ctx, none); err != nil || len(got) != 0 {
			t.Errorf("ReviewCosts(%s) = %+v, %v; want none", none, got, err)
		}
	}
}

// A judge run reported with no result (the Job timed out): the review counts
// once, as a review with no result, and its reviewers' tokens are wasted.
func TestReviewNoResultIsWasted(t *testing.T) {
	s := newStore(t)
	mr := usageMR(7, 1)
	id, ms := multiReviewWithEntries(t, s, mr, "a1", 2)
	for _, m := range ms {
		doneReviewer(t, s, m.ID, `{}`)
	}
	if err := s.RecordReviewMemberUsage(ctx, ms[0].ID, []SkillUsage{{Skill: "security", In: 100, Out: 10}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordReviewMemberUsage(ctx, ms[1].ID, []SkillUsage{{Skill: "security", In: 50, Out: 5}}); err != nil {
		t.Fatal(err)
	}
	if released, _, err := s.ReleaseMultiReview(ctx, id, t0); err != nil || !released {
		t.Fatalf("ReleaseMultiReview = %v, %v", released, err)
	}
	// The judge ran and left no result.
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'reported', failure_reason = 'the Job timed out',
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
	if w.Failed.In != 150 || w.Failed.Out != 15 || w.NoResult != 1 {
		t.Errorf("wasted = %+v; want the reviewers' tokens as failed and one review with no result", w)
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
	if len(m.Tokens) != 1 || m.Tokens[0].Tokens.In != 150 {
		t.Errorf("tokens = %+v; want the reviewers' 150 kept", m.Tokens)
	}
}

// A single review with no result and an off row is not a multi review: its
// tokens are wasted only by their own failed verdict, as today.
func TestReviewNoResultOffIsSingle(t *testing.T) {
	s := newStore(t)
	mr := usageMR(7, 1)
	id := enqueue(t, s, mr, "a1")
	if err := s.RecordMultiReviewOff(ctx, id, "the review image does not support it"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'reported', failure_reason = 'the Job timed out',
		reported_at = $2, created_at = $2 WHERE id = $1::uuid`, id, usageAt(5, 10, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, id, []SkillUsage{{Skill: "security", Verdict: "no_blocking_issues", In: 40}}); err != nil {
		t.Fatal(err)
	}
	w, err := s.UsageWasted(ctx, mr.Instance, 7, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	if w.Failed.In != 0 {
		t.Errorf("wasted = %+v; want nothing wasted for a single review's passed skill", w)
	}
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
