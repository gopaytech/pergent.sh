package store

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Migration 0051 only adds: every column and index of 0050 is still there,
// unchanged, beside the new table and columns.
func TestModelLimitsMigrationOnlyAdds(t *testing.T) {
	b, err := os.ReadFile("migrations/0051_model_limits.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sql.WriteString(line + "\n")
		}
	}
	// A foreign key's ON DELETE is not a delete.
	text := regexp.MustCompile(`(?i)ON\s+DELETE\s+CASCADE`).ReplaceAllString(sql.String(), "")
	for _, bad := range []string{`(?i)\bDROP\b`, `(?i)\bRENAME\b`, `(?i)\bTRUNCATE\b`, `(?i)\bDELETE\b`, `(?i)\bUPDATE\b`,
		`(?i)\bALTER\s+COLUMN\b`, `(?i)\bSET\s+NOT\s+NULL\b`} {
		if regexp.MustCompile(bad).MatchString(text) {
			t.Errorf("migration 0051 matches %s; it may only add", bad)
		}
	}
	for _, stmt := range strings.Split(text, ";") {
		up := strings.ToUpper(stmt)
		if strings.Contains(up, "ADD COLUMN") && strings.Contains(up, "NOT NULL") && !strings.Contains(up, "DEFAULT") {
			t.Errorf("%q adds a NOT NULL column with no default", strings.TrimSpace(stmt))
		}
	}

	s := newStoreAt(t, 50)
	before, beforeIdx := schemaColumns(t, s), schemaIndexes(t, s)
	applyUpTo(t, s, 51)
	after, afterIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0051 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	for name, def := range beforeIdx {
		if got, ok := afterIdx[name]; !ok || got != def {
			t.Errorf("index %s after 0051 = %q (present %v); want %q", name, got, ok, def)
		}
	}
	for _, col := range []string{"model_limits.source", "model_limits.entry_id", "model_limits.team_id", "model_limits.monthly_tokens",
		"model_limits.updated_at", "model_limits.updated_by", "run_usage.owner_team_id", "run_usage_models.owner_team_id"} {
		if _, ok := after[col]; !ok {
			t.Errorf("column %s is missing after 0051", col)
		}
	}
	for _, col := range []string{"run_usage.owner_team_id", "run_usage_models.owner_team_id"} {
		if after[col].Nullable != "YES" {
			t.Errorf("%s = %+v; want nullable: the previous release writes none", col, after[col])
		}
	}
}

// One limit per entry and team, the model limit (no team) included; a team
// entry has no model limit; the number is in the quotas' range.
func TestModelLimitsKey(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	ins := func(source string, teamID any, n int64) error {
		_, err := s.pool.Exec(ctx, `INSERT INTO model_limits (source, entry_id, team_id, monthly_tokens, updated_at, updated_by)
			VALUES ($1, 'e1', $2, $3, now(), 'test')`, source, teamID, n)
		return err
	}
	if err := ins("global", nil, 10); err != nil {
		t.Fatalf("model limit: %v", err)
	}
	if err := ins("global", team, 10); err != nil {
		t.Fatalf("team limit: %v", err)
	}
	wantCode := func(what string, err error, code string) {
		t.Helper()
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != code {
			t.Errorf("%s = %v; want SQLSTATE %s", what, err, code)
		}
	}
	wantCode("a second model limit", ins("global", nil, 20), "23505")
	wantCode("a second team limit", ins("global", team, 20), "23505")
	wantCode("a team entry's model limit", ins("team", nil, 10), "23514")
	wantCode("0 tokens", ins("team", team, 0), "23514")
	wantCode("over the maximum", ins("team", team, 1_000_000_000_000_001), "23514")
}

// The previous release writes usage rows with no owner team: they still go
// in, with none.
func TestModelLimitsMigrationPreviousReleaseInsert(t *testing.T) {
	s := newStore(t)
	id := reported(t, s, mr1, "h1", `{"schema_version":1,"status":"completed","skills":[]}`)
	if _, err := s.pool.Exec(ctx, `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid, trigger,
			incremental, provider, model, verdict, reported_at, tokens_in, tokens_out, findings, blocking, source, team_id)
		VALUES ($1::uuid, 'security', $2, 7, 'acme/widgets', 3, 'push', false, '', 'gpt', 'no_blocking_issues', now(),
			1, 1, 0, 0, 'global', NULL)`, id, mr1.Instance); err != nil {
		t.Fatalf("the previous release's insert: %v", err)
	}
	var owner *int64
	if err := s.pool.QueryRow(ctx, `SELECT owner_team_id FROM run_usage WHERE run_id = $1::uuid`, id).Scan(&owner); err != nil || owner != nil {
		t.Errorf("owner_team_id = %v, %v; want NULL", owner, err)
	}
}

// ownRepo makes team cover (instance, project) alone, with the owner tick.
func ownRepo(t *testing.T, s *Store, team int64, instance, project string) {
	t.Helper()
	if err := setOwner(t, s, team, TeamRepo{Instance: instance, Project: project, Owner: true}); err != nil {
		t.Fatal(err)
	}
}

// ownerOf is the owner_team_id of run id's run_usage rows (nil: none). Every
// row of a run has the same.
func ownerOf(t *testing.T, s *Store, id string) *int64 {
	t.Helper()
	var owner *int64
	if err := s.pool.QueryRow(ctx, `SELECT owner_team_id FROM run_usage WHERE run_id = $1::uuid LIMIT 1`, id).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	return owner
}

func wantOwner(t *testing.T, what string, got *int64, want int64) {
	t.Helper()
	if (want == 0) != (got == nil) || (got != nil && *got != want) {
		t.Errorf("%s: owner_team_id = %v, want %d (0: none)", what, got, want)
	}
}

// A usage row keeps the owner team of its repository when it is written: the
// repository's own owner, else its instance's, else none. A repository that
// moves to another team leaves its earlier rows with the old one.
func TestUsageRecordsOwnerTeam(t *testing.T) {
	s := newStore(t)
	row := skillRow("security", "no_blocking_issues", 1, 1)
	none := seedUsage(t, s, usageMR(7, 1), t0, TriggerPush, false, "gpt", row)

	platform, payments, moved := newTeam(t, s, "Platform"), newTeam(t, s, "Payments"), newTeam(t, s, "Moved")
	ownRepo(t, s, platform, mr1.Instance, "")
	byInstance := seedUsage(t, s, usageMR(7, 2), t0, TriggerPush, false, "gpt", row)
	ownRepo(t, s, payments, mr1.Instance, "ACME/p7") // seedUsage writes project_path acme/p7
	byRepo := seedUsage(t, s, usageMR(7, 3), t0, TriggerPush, false, "gpt", row)
	if err := setOwner(t, s, payments); err != nil {
		t.Fatal(err)
	}
	ownRepo(t, s, moved, mr1.Instance, "acme/p7")
	after := seedUsage(t, s, usageMR(7, 4), t0, TriggerPush, false, "gpt", row)

	wantOwner(t, "no owner", ownerOf(t, s, none), 0)
	wantOwner(t, "the instance's owner", ownerOf(t, s, byInstance), platform)
	wantOwner(t, "the repository's own owner, ignoring case", ownerOf(t, s, byRepo), payments)
	wantOwner(t, "after the move", ownerOf(t, s, after), moved)
	// The earlier row stays with the team it had.
	wantOwner(t, "before the move, read again", ownerOf(t, s, byRepo), payments)
}

// judgedPanel is a panel review of head on mr1 taken to the end: members A
// (global m1) and B (team t1 of team 4) report their usage, the run is
// released, and Job P, judging with both, reports its own with a row per
// model. It returns the run.
func judgedPanel(t *testing.T, s *Store, head string) string {
	t.Helper()
	id, a, b := panelMembersWithEntries(t, s, head)
	doneMember(t, s, a.ID, `{"status":"completed"}`)
	doneMember(t, s, b.ID, `{"status":"completed"}`)
	if err := s.RecordMemberUsage(ctx, a.ID, []SkillUsage{{Skill: "security", In: 100, Out: 10}, {Skill: "style", In: 7, Out: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMemberUsage(ctx, b.ID, []SkillUsage{{Skill: "security", In: 50, Out: 5}}); err != nil {
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
	token := "p-" + head
	if err := s.MarkDispatched(ctx, id, "job-p-"+head, "base-sha", "start-sha", hash(token), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, token, []byte(`{}`), t0.Add(2*time.Minute)); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	if err := s.RecordUsage(ctx, id, []SkillUsage{{Skill: "security", Verdict: "has_blocking_issues", In: 20, Out: 2}},
		[]ModelUsage{
			{Skill: "security", Role: "a", Provider: "openai", Model: "gpt-a", In: 12, Out: 1},
			{Skill: "security", Role: "b", Provider: "anthropic", Model: "sonnet-b", In: 8, Out: 1},
		}); err != nil {
		t.Fatal(err)
	}
	return id
}

// A panel's rows, members and Job P's, keep the owner team too.
func TestPanelUsageRecordsOwnerTeam(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	ownRepo(t, s, team, mr1.Instance, "acme/widgets")
	id := judgedPanel(t, s, "a1")
	wantOwner(t, "run_usage", ownerOf(t, s, id), team)
	rows, err := s.pool.Query(ctx, `SELECT part, owner_team_id FROM run_usage_models WHERE run_id = $1::uuid`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var part string
		var owner *int64
		if err := rows.Scan(&part, &owner); err != nil {
			t.Fatal(err)
		}
		wantOwner(t, part+" row", owner, team)
		n++
	}
	if n != 5 { // A: 2 skills, B: 1, P: 2 roles
		t.Errorf("%d model rows, want 5", n)
	}
}

// A limit is set, replaced and cleared per entry and team; clearing names the
// number it had; ModelLimits lists every one, by entry and team.
func TestSetAndClearModelLimit(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := EntryRef{Source: SourceGlobal, ID: "m1"}
	tm := EntryRef{Source: SourceTeam, ID: "t1"}
	for _, l := range []ModelLimit{
		{Ref: g, MonthlyTokens: 100, UpdatedAt: t0, UpdatedBy: "root (password)"},
		{Ref: g, TeamID: team, MonthlyTokens: 50, UpdatedAt: t0, UpdatedBy: "root (password)"},
		{Ref: g, MonthlyTokens: 200, UpdatedAt: t0.Add(time.Minute), UpdatedBy: "alice@example.com"}, // replaces the first
		{Ref: tm, TeamID: team, MonthlyTokens: 7, UpdatedAt: t0, UpdatedBy: "alice@example.com"},
	} {
		if err := s.SetModelLimit(ctx, l); err != nil {
			t.Fatalf("SetModelLimit(%+v): %v", l, err)
		}
	}
	got, err := s.ModelLimits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelLimit{
		{Ref: g, MonthlyTokens: 200, UpdatedAt: t0.Add(time.Minute), UpdatedBy: "alice@example.com"},
		{Ref: g, TeamID: team, MonthlyTokens: 50, UpdatedAt: t0, UpdatedBy: "root (password)"},
		{Ref: tm, TeamID: team, MonthlyTokens: 7, UpdatedAt: t0, UpdatedBy: "alice@example.com"},
	}
	if len(got) != len(want) {
		t.Fatalf("ModelLimits = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i].Ref != want[i].Ref || got[i].TeamID != want[i].TeamID || got[i].MonthlyTokens != want[i].MonthlyTokens ||
			!got[i].UpdatedAt.Equal(want[i].UpdatedAt) || got[i].UpdatedBy != want[i].UpdatedBy {
			t.Errorf("limit %d = %+v; want %+v", i, got[i], want[i])
		}
	}
	if old, found, err := s.ClearModelLimit(ctx, g, 0); err != nil || !found || old != 200 {
		t.Errorf("ClearModelLimit(model) = %d, %v, %v; want 200, true", old, found, err)
	}
	if _, found, err := s.ClearModelLimit(ctx, g, 0); err != nil || found {
		t.Errorf("ClearModelLimit again = %v, %v; want not found", found, err)
	}
	if got, _ := s.ModelLimits(ctx); len(got) != 2 {
		t.Errorf("after the clear: %+v; want the 2 team limits", got)
	}
}

// Removing a team removes its limits, on global and team entries.
func TestDeleteTeamDropsItsLimits(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := EntryRef{Source: SourceGlobal, ID: "m1"}
	for _, l := range []ModelLimit{{Ref: g, MonthlyTokens: 100}, {Ref: g, TeamID: team, MonthlyTokens: 50}} {
		l.UpdatedAt, l.UpdatedBy = t0, "test"
		if err := s.SetModelLimit(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.DeleteTeam(ctx, team, nil); err != nil || !ok {
		t.Fatalf("DeleteTeam = %v, %v", ok, err)
	}
	if got, err := s.ModelLimits(ctx); err != nil || len(got) != 1 || got[0].TeamID != 0 {
		t.Errorf("ModelLimits = %+v, %v; want the model limit alone", got, err)
	}
}

// entryUsage records a review of a new change on mr1's project that ran on
// ref and reported in and out tokens at at (at most t0 + 1h, the token's
// expiry). It returns the run.
func entryUsage(t *testing.T, s *Store, ref EntryRef, at time.Time, in, out int64) string {
	t.Helper()
	n := usageSeq.Add(1)
	head := fmt.Sprintf("limit%d", n)
	mr := MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: 1000 + n}
	id := dispatched(t, s, mr, head, "tok-"+head)
	if ref.ID != "" {
		if err := s.SetRunEntry(ctx, id, RunEntry{Source: ref.Source, ID: ref.ID, Name: "n-" + ref.ID, Position: 1, Length: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := s.StoreResult(ctx, id, "tok-"+head, []byte(`{"schema_version":1,"status":"completed","skills":[]}`), at); err != nil || res != ResultStored {
		t.Fatalf("StoreResult = %v, %v", res, err)
	}
	if err := s.RecordUsage(ctx, id, []SkillUsage{skillRow("security", "no_blocking_issues", in, out)}, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

// The sums: per entry and owner team, from the 1st of the month in UTC on; a
// row with no owner team counts under 0; a run that recorded no entry counts
// on none.
func TestEntryTokensSince(t *testing.T) {
	s := newStore(t)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := EntryRef{Source: SourceGlobal, ID: "m1"}
	tm := EntryRef{Source: SourceTeam, ID: "t9"}
	entryUsage(t, s, g, t0, 3, 0) // before the team owned the repository
	team := newTeam(t, s, "Payments")
	ownRepo(t, s, team, mr1.Instance, "acme/widgets")
	entryUsage(t, s, g, t0, 10, 5)
	entryUsage(t, s, tm, t0, 7, 0)
	entryUsage(t, s, g, since, 1, 0)                    // the first second of the month counts
	entryUsage(t, s, g, since.Add(-time.Second), 99, 0) // the last second of the month before does not
	entryUsage(t, s, EntryRef{}, t0, 50, 0)             // no entry

	got, err := s.EntryTokensSince(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	want := []EntryTokens{
		{Ref: g, Tokens: 3},
		{Ref: g, OwnerTeam: team, Tokens: 16},
		{Ref: tm, OwnerTeam: team, Tokens: 7},
	}
	if len(got) != len(want) {
		t.Fatalf("EntryTokensSince = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v; want %+v", i, got[i], want[i])
		}
	}
}

// A panel counts each Job on its own entry: the members on theirs, Job P's
// roles on their judges'. Its run_usage rows, the total of all 3 Jobs, are
// not counted again on the run's entry.
func TestEntryTokensSincePanel(t *testing.T) {
	s := newStore(t)
	id := judgedPanel(t, s, "a1")
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET model_source = 'global', model_entry_id = 'm1' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	got, err := s.EntryTokensSince(ctx, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []EntryTokens{
		{Ref: EntryRef{Source: SourceGlobal, ID: "m1"}, Tokens: 110 + 8 + 13}, // A's 2 skills, P's role a
		{Ref: EntryRef{Source: SourceTeam, ID: "t1"}, Tokens: 55 + 9},         // B, P's role b
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("EntryTokensSince = %+v; want %+v", got, want)
	}
}

// A multi review counts each Job on its own entry (Ruling L8): each
// reviewer on its review_members entry, the judge on its review_judges
// entry, each with the owner team of the run_usage row it was added to.
// The run's run_usage rows, the total of every Job, are not counted again
// on the run's entry (reviewer 1's). A --no-judge run's own tokens count on
// no entry.
func TestEntryTokensSinceMultiReview(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	ownRepo(t, s, team, mr1.Instance, "acme/widgets")
	id, ms := multiReviewWithEntries(t, s, mr1, "a1", 3)
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET model_source = 'global', model_entry_id = 'e1' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		doneReviewer(t, s, m.ID, `{"status":"completed"}`)
	}
	for i, skills := range [][]SkillUsage{
		{{Skill: "security", In: 100, Out: 10}, {Skill: "style", In: 7, Out: 1}},
		{{Skill: "security", In: 50, Out: 5}},
		{{Skill: "security", In: 30, Out: 3}},
	} {
		if err := s.RecordReviewMemberUsage(ctx, ms[i].ID, skills); err != nil {
			t.Fatal(err)
		}
	}
	judgeRun(t, s, id, JudgeRow{Reviewers: []int{1, 2, 3}, Provider: "anthropic", Model: "judge-m", Entry: judgeEntry})
	if err := s.RecordJudgeUsage(ctx, id, []SkillUsage{{Skill: "security", In: 20, Out: 2}, {Skill: "style", In: 4}}); err != nil {
		t.Fatal(err)
	}

	// A second review, on mr2, whose judge ran --no-judge: its own tokens
	// count on no entry.
	other, oms := multiReviewWithEntries(t, s, mr2, "b1", 2)
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET model_source = 'global', model_entry_id = 'e1' WHERE id = $1::uuid`, other); err != nil {
		t.Fatal(err)
	}
	for _, m := range oms {
		doneReviewer(t, s, m.ID, `{"status":"completed"}`)
		if err := s.RecordReviewMemberUsage(ctx, m.ID, []SkillUsage{{Skill: "security", In: 1000, Out: 0}}); err != nil {
			t.Fatal(err)
		}
	}
	judgeRun(t, s, other, JudgeRow{Reviewers: []int{1, 2}, NoJudge: true, NoJudgeReason: "no judge"})
	if err := s.RecordJudgeUsage(ctx, other, []SkillUsage{{Skill: "security", In: 9, Out: 0}}); err != nil {
		t.Fatal(err)
	}

	got, err := s.EntryTokensSince(ctx, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []EntryTokens{
		{Ref: EntryRef{Source: SourceGlobal, ID: "e1"}, OwnerTeam: team, Tokens: 110 + 8 + 1000}, // reviewer 1 of both reviews
		{Ref: EntryRef{Source: SourceGlobal, ID: "e3"}, OwnerTeam: team, Tokens: 33},
		{Ref: EntryRef{Source: SourceTeam, ID: "e2"}, OwnerTeam: team, Tokens: 55 + 1000},
		{Ref: EntryRef{Source: SourceTeam, ID: "j1"}, OwnerTeam: team, Tokens: 22 + 4}, // the judge, not the --no-judge run
	}
	if !slices.Equal(got, want) {
		t.Errorf("EntryTokensSince = %+v\nwant %+v", got, want)
	}
}

// A reviewer with 2 attempts and a judge with a retry: each attempt counts
// once, on its own entry. Reviewer 2's first attempt on team e2, its
// fallback on global e2b; the judge on team j1, its retry on global j2. The
// runs' own entry (reviewer 1's) gets none of them.
func TestEntryTokensSinceMultiReviewAttempts(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	ownRepo(t, s, team, mr1.Instance, "acme/widgets")
	id, ms := multiReviewWithEntries(t, s, mr1, "a1", 3)
	doneReviewer(t, s, ms[0].ID, `{}`)
	doneReviewer(t, s, ms[2].ID, `{}`)
	reportReviewer(t, s, ms[1].ID, `{}`)
	second, created, err := s.EnqueueReviewMemberAttempt(ctx, reviewerOf(t, s, id, 2), quotaMark("e2"))
	if err != nil || !created {
		t.Fatal(second, created, err)
	}
	if err := s.SetReviewMemberEntry(ctx, second, RunEntry{Source: SourceGlobal, ID: "e2b", Name: "main-2b", Position: 2, Length: 2}, "p2b", "m2b"); err != nil {
		t.Fatal(err)
	}
	doneReviewer(t, s, second, `{}`)
	for _, rec := range []struct {
		id     string
		skills []SkillUsage
	}{
		{ms[0].ID, []SkillUsage{{Skill: "security", In: 10, Out: 1}}},
		{ms[1].ID, []SkillUsage{{Skill: "security", In: 21, Out: 1}}},
		{second, []SkillUsage{{Skill: "security", In: 22, Out: 2}}},
		{ms[2].ID, []SkillUsage{{Skill: "security", In: 3, Out: 1}}},
	} {
		if err := s.RecordReviewMemberUsage(ctx, rec.id, rec.skills); err != nil {
			t.Fatal(err)
		}
	}
	judgeRun(t, s, id, JudgeRow{Reviewers: []int{1, 2, 3}, Provider: "anthropic", Model: "judge-m", Entry: judgeEntry})
	if err := s.RecordJudgeUsage(ctx, id, []SkillUsage{{Skill: "security", In: 5, Out: 1}, {Skill: "style", In: 2}}); err != nil {
		t.Fatal(err)
	}
	// The provider refused the judge entry: the next attempt judges on the
	// chain's backup.
	retry, created, err := s.EnqueueJudgeRetry(ctx, getRun(t, s, id))
	if err != nil || !created {
		t.Fatal(retry, created, err)
	}
	reportJudge(t, s, retry, JudgeRow{Reviewers: []int{1, 2, 3}, Provider: "openai", Model: "judge-2",
		Entry: RunEntry{Source: SourceGlobal, ID: "j2", Name: "judge-2", Position: 2, Length: 2}})
	if err := s.RecordJudgeUsage(ctx, retry, []SkillUsage{{Skill: "security", In: 6, Out: 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET model_source = 'global', model_entry_id = 'e1' WHERE id IN ($1::uuid, $2::uuid)`, id, retry); err != nil {
		t.Fatal(err)
	}

	got, err := s.EntryTokensSince(ctx, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []EntryTokens{
		{Ref: EntryRef{Source: SourceGlobal, ID: "e1"}, OwnerTeam: team, Tokens: 11},
		{Ref: EntryRef{Source: SourceGlobal, ID: "e2b"}, OwnerTeam: team, Tokens: 24}, // reviewer 2's fallback
		{Ref: EntryRef{Source: SourceGlobal, ID: "e3"}, OwnerTeam: team, Tokens: 4},
		{Ref: EntryRef{Source: SourceGlobal, ID: "j2"}, OwnerTeam: team, Tokens: 9}, // the judge's retry
		{Ref: EntryRef{Source: SourceTeam, ID: "e2"}, OwnerTeam: team, Tokens: 22},  // reviewer 2's first attempt
		{Ref: EntryRef{Source: SourceTeam, ID: "j1"}, OwnerTeam: team, Tokens: 8},   // the first judge run
	}
	if !slices.Equal(got, want) {
		t.Errorf("EntryTokensSince = %+v\nwant %+v", got, want)
	}
}
