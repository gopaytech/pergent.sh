package store

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

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
