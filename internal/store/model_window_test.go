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

// Migration 0056 only adds: every column and index of 0055 is still there,
// unchanged, beside the 4 new nullable columns.
func TestModelWindowMigrationOnlyAdds(t *testing.T) {
	b, err := os.ReadFile("migrations/0056_model_limits_per_entry.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sql.WriteString(line + "\n")
		}
	}
	for _, bad := range []string{`(?i)\bDROP\b`, `(?i)\bRENAME\b`, `(?i)\bTRUNCATE\b`, `(?i)\bDELETE\b`, `(?i)\bUPDATE\b`,
		`(?i)\bALTER\s+COLUMN\b`, `(?i)\bSET\s+NOT\s+NULL\b`} {
		if regexp.MustCompile(bad).MatchString(sql.String()) {
			t.Errorf("migration 0056 matches %s; it may only add", bad)
		}
	}
	for _, stmt := range strings.Split(sql.String(), ";") {
		up := strings.ToUpper(stmt)
		if strings.Contains(up, "ADD COLUMN") && strings.Contains(up, "NOT NULL") && !strings.Contains(up, "DEFAULT") {
			t.Errorf("%q adds a NOT NULL column with no default", strings.TrimSpace(stmt))
		}
	}

	s := newStoreAt(t, 55)
	before, beforeIdx := schemaColumns(t, s), schemaIndexes(t, s)
	applyUpTo(t, s, 56)
	after, afterIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0056 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	for name, def := range beforeIdx {
		if got, ok := afterIdx[name]; !ok || got != def {
			t.Errorf("index %s after 0056 = %q (present %v); want %q", name, got, ok, def)
		}
	}
	for _, col := range []string{"llm_models.context_window", "llm_models.max_output",
		"team_models.context_window", "team_models.max_output"} {
		if got := after[col]; got != (columnInfo{Type: "integer", Nullable: "YES"}) {
			t.Errorf("column %s = %+v; want a nullable integer", col, got)
		}
	}
}

// The previous release inserts entries with the 0055 columns: they still go
// in, with no context window and no max output.
func TestModelWindowMigrationPreviousRelease(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	if _, err := s.pool.Exec(ctx, `INSERT INTO llm_models (id, provider, model, base_url, api_key_sealed, is_default,
			teams_may_use, created_at, updated_at)
		VALUES ('old', 'anthropic', 'sonnet', '', NULL, false, false, now(), now())`); err != nil {
		t.Fatalf("the previous release's global insert: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO team_models (team_id, name, provider, model, base_url, api_key_sealed,
			created_at, updated_at)
		VALUES ($1, 'Old', 'openai', 'gpt', '', NULL, now(), now())`, team); err != nil {
		t.Fatalf("the previous release's team insert: %v", err)
	}
	gl, err := s.LLMModels(ctx)
	if err != nil || len(gl) != 1 || gl[0].ContextWindow != 0 || gl[0].MaxOutput != 0 {
		t.Errorf("LLMModels = %+v, %v; want 0 and 0", gl, err)
	}
	tm, err := s.TeamModels(ctx, team)
	if err != nil || len(tm) != 1 || tm[0].ContextWindow != 0 || tm[0].MaxOutput != 0 {
		t.Errorf("TeamModels = %+v, %v; want 0 and 0", tm, err)
	}
}

func TestModelWindowRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	otherTeam := newTeam(t, s, "Platform")
	at := t0.Add(time.Hour)

	globalOf := func(t *testing.T, id string) LLMModel {
		t.Helper()
		all, err := s.LLMModels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range all {
			if m.ID == id {
				return m
			}
		}
		t.Fatalf("no global entry %s", id)
		return LLMModel{}
	}
	teamOf := func(t *testing.T, id string) TeamModel {
		t.Helper()
		m, ok := teamModelByID(t, s, team, id)
		if !ok {
			t.Fatalf("no team entry %s", id)
		}
		return m
	}
	isNull := func(t *testing.T, table, col, id string) bool {
		t.Helper()
		var null bool
		if err := s.pool.QueryRow(ctx, `SELECT `+col+` IS NULL FROM `+table+` WHERE id = $1`, id).Scan(&null); err != nil {
			t.Fatal(err)
		}
		return null
	}

	t.Run("global create", func(t *testing.T) {
		m, err := s.CreateLLMModel(ctx, LLMModel{Name: "Big", Provider: "anthropic", Model: "sonnet",
			ContextWindow: 1_000_000, MaxOutput: 64_000}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if m.ContextWindow != 1_000_000 || m.MaxOutput != 64_000 {
			t.Errorf("created = %d, %d", m.ContextWindow, m.MaxOutput)
		}
		if got := globalOf(t, m.ID); got.ContextWindow != 1_000_000 || got.MaxOutput != 64_000 {
			t.Errorf("read = %d, %d", got.ContextWindow, got.MaxOutput)
		}
	})

	t.Run("global set and clear", func(t *testing.T) {
		m, err := s.CreateLLMModel(ctx, LLMModel{Name: "Plain", Provider: "openai", Model: "gpt"}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if found, err := s.SetLLMModelWindow(ctx, m.ID, 200_000, 0, at); err != nil || !found {
			t.Fatalf("SetLLMModelWindow = %v, %v", found, err)
		}
		got := globalOf(t, m.ID)
		if got.ContextWindow != 200_000 || got.MaxOutput != 0 || !got.UpdatedAt.Equal(at) {
			t.Errorf("after set = %+v", got)
		}
		if !isNull(t, "llm_models", "max_output", m.ID) {
			t.Error("max_output is not NULL after 0")
		}
		if found, err := s.SetLLMModelWindow(ctx, m.ID, 0, 0, at); err != nil || !found {
			t.Fatalf("SetLLMModelWindow clear = %v, %v", found, err)
		}
		if got := globalOf(t, m.ID); got.ContextWindow != 0 || got.MaxOutput != 0 {
			t.Errorf("after clear = %d, %d", got.ContextWindow, got.MaxOutput)
		}
	})

	t.Run("global missing", func(t *testing.T) {
		if found, err := s.SetLLMModelWindow(ctx, "no-such-id", 8192, 0, at); err != nil || found {
			t.Errorf("SetLLMModelWindow = %v, %v; want false, nil", found, err)
		}
	})

	t.Run("team create, set, clear, missing", func(t *testing.T) {
		m, err := s.CreateTeamModel(ctx, TeamModel{TeamID: team, Name: "Big", Provider: "anthropic", Model: "sonnet",
			ContextWindow: 1_000_000, MaxOutput: 64_000}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if m.ContextWindow != 1_000_000 || m.MaxOutput != 64_000 {
			t.Errorf("created = %d, %d", m.ContextWindow, m.MaxOutput)
		}
		if got := teamOf(t, m.ID); got.ContextWindow != 1_000_000 || got.MaxOutput != 64_000 {
			t.Errorf("read = %d, %d", got.ContextWindow, got.MaxOutput)
		}
		if found, err := s.SetTeamModelWindow(ctx, team, m.ID, 200_000, 0, at); err != nil || !found {
			t.Fatalf("SetTeamModelWindow = %v, %v", found, err)
		}
		got := teamOf(t, m.ID)
		if got.ContextWindow != 200_000 || got.MaxOutput != 0 || !got.UpdatedAt.Equal(at) {
			t.Errorf("after set = %+v", got)
		}
		if !isNull(t, "team_models", "max_output", m.ID) {
			t.Error("max_output is not NULL after 0")
		}
		if found, err := s.SetTeamModelWindow(ctx, team, m.ID, 0, 0, at); err != nil || !found {
			t.Fatalf("SetTeamModelWindow clear = %v, %v", found, err)
		}
		if got := teamOf(t, m.ID); got.ContextWindow != 0 || got.MaxOutput != 0 {
			t.Errorf("after clear = %d, %d", got.ContextWindow, got.MaxOutput)
		}
		if found, err := s.SetTeamModelWindow(ctx, otherTeam, m.ID, 8192, 0, at); err != nil || found {
			t.Errorf("another team's entry = %v, %v; want false, nil", found, err)
		}
		if found, err := s.SetTeamModelWindow(ctx, team, "no-such-id", 8192, 0, at); err != nil || found {
			t.Errorf("missing entry = %v, %v; want false, nil", found, err)
		}
	})

	t.Run("check refuses", func(t *testing.T) {
		g, err := s.CreateLLMModel(ctx, LLMModel{Name: "Checked", Provider: "google", Model: "gemini"}, t0)
		if err != nil {
			t.Fatal(err)
		}
		tm, err := s.CreateTeamModel(ctx, TeamModel{TeamID: team, Name: "Checked", Provider: "google", Model: "gemini"}, t0)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct{ name, sql, id string }{
			{"global window too small", `UPDATE llm_models SET context_window = 4095 WHERE id = $1`, g.ID},
			{"global output over window", `UPDATE llm_models SET context_window = 8192, max_output = 9000 WHERE id = $1`, g.ID},
			{"team window too small", `UPDATE team_models SET context_window = 4095 WHERE id = $1`, tm.ID},
			{"team output over window", `UPDATE team_models SET context_window = 8192, max_output = 9000 WHERE id = $1`, tm.ID},
		} {
			_, err := s.pool.Exec(ctx, c.sql, c.id)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "23514" {
				t.Errorf("%s = %v; want SQLSTATE 23514", c.name, err)
			}
		}
	})
}
