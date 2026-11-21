package store

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// sqlState is the Postgres error code of err, or "".
func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// 0048 only adds: nothing of 0046 changes, and chain A is untouched.
func TestPanelChainsMigrationOnlyAdds(t *testing.T) {
	b, err := os.ReadFile("migrations/0048_panel_chains.sql")
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
	for _, bad := range []string{`(?i)\bDROP\b`, `(?i)\bRENAME\b`, `(?i)\bTRUNCATE\b`, `(?i)\bDELETE\b`,
		`(?i)\bUPDATE\b`, `(?i)\bALTER\s+COLUMN\b`, `(?i)\bSET\s+NOT\s+NULL\b`} {
		if regexp.MustCompile(bad).MatchString(text) {
			t.Errorf("migration 0048 matches %s; it may only add", bad)
		}
	}
	for _, stmt := range strings.Split(text, ";") {
		up := strings.ToUpper(stmt)
		if strings.Contains(up, "ADD COLUMN") && strings.Contains(up, "NOT NULL") && !strings.Contains(up, "DEFAULT") {
			t.Errorf("%q adds a NOT NULL column with no default", strings.TrimSpace(stmt))
		}
	}

	s := newStoreAt(t, 46)
	before, beforeIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for _, col := range []string{"model_chains.saved_by_admin", "model_chain_entries.model_id"} {
		if _, ok := before[col]; !ok {
			t.Fatalf("column %s is missing at 0046", col)
		}
	}
	applyUpTo(t, s, 48)
	after, afterIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0048 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	for name, def := range beforeIdx {
		if got, ok := afterIdx[name]; !ok || got != def {
			t.Errorf("index %s after 0048 = %q (present %v); want %q", name, got, ok, def)
		}
	}
	for _, col := range []string{"panel_chains.saved_by_admin", "panel_chain_entries.model_id", "dashboard_repos.panel_chain_mode"} {
		if _, ok := after[col]; !ok {
			t.Errorf("column %s is missing after 0048", col)
		}
	}
	if c := after["dashboard_repos.panel_chain_mode"]; c.Nullable != "NO" {
		t.Errorf("dashboard_repos.panel_chain_mode = %+v; want NOT NULL", c)
	}
}

// The previous release's inserts (0046 era) still work after 0048.
func TestPanelChainsMigrationPreviousRelease(t *testing.T) {
	s := newStoreAt(t, 46)
	applyUpTo(t, s, 48)
	if _, err := s.pool.Exec(ctx, `INSERT INTO dashboard_repos (instance, project, created_by) VALUES ('main', 'acme/api', 'alice')`); err != nil {
		t.Fatalf("0046-era dashboard_repos insert: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO model_chains (scope, instance, project, updated_at)
		VALUES ('repo', 'main', 'acme/api', $1), ('global', '', '', $1)`, t0); err != nil {
		t.Fatalf("0046-era model_chains insert: %v", err)
	}
	var mode string
	if err := s.pool.QueryRow(ctx, `SELECT panel_chain_mode FROM dashboard_repos WHERE project = 'acme/api'`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "inherit" {
		t.Errorf("panel_chain_mode = %q; want inherit", mode)
	}
}

func TestPanelChainOneRowPerTarget(t *testing.T) {
	s := newStoreAt(t, 48)
	ins := `INSERT INTO panel_chains (scope, instance, project, updated_at) VALUES ($1, $2, $3, $4)`
	if _, err := s.pool.Exec(ctx, ins, "global", "", "", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, "global", "", "", t0); sqlState(err) != "23505" {
		t.Errorf("second global row: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ins, "repo", "main", "acme/api", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, "repo", "main", "ACME/api", t0); sqlState(err) != "23505" {
		t.Errorf("same repo, other case: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ins, "team", "", "", t0); sqlState(err) != "23514" {
		t.Errorf("team row with no team: %v; want 23514", err)
	}
}

func TestPanelChainModeValues(t *testing.T) {
	s := newStoreAt(t, 48)
	for i, mode := range []string{"inherit", "custom", "off"} {
		project := []string{"a/a", "b/b", "c/c"}[i]
		if _, err := s.pool.Exec(ctx, `INSERT INTO dashboard_repos (instance, project, panel_chain_mode) VALUES ('main', $1, $2)`, project, mode); err != nil {
			t.Errorf("mode %q: %v", mode, err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO dashboard_repos (instance, project, panel_chain_mode) VALUES ('main', 'd/d', 'other')`); sqlState(err) != "23514" {
		t.Errorf("mode other: %v; want 23514", err)
	}
}
