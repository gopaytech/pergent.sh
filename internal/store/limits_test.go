package store

import (
	"errors"
	"os"
	"regexp"
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
