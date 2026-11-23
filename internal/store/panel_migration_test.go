package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The migrations of the legacy review panel (0048 and 0049) stay: the
// previous release must run on the migrated database, and their tables
// are dropped only by a later release. These tests keep them honest.

// 0049 only adds: nothing of 0048 changes, and runs_live_idx is untouched.
func TestPanelMembersMigrationOnlyAdds(t *testing.T) {
	b, err := os.ReadFile("migrations/0049_panel_members.sql")
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
		`(?i)\bUPDATE\b`, `(?i)\bALTER\b`} {
		if regexp.MustCompile(bad).MatchString(text) {
			t.Errorf("migration 0049 matches %s; it may only add", bad)
		}
	}

	s := newStoreAt(t, 48)
	before, beforeIdx := schemaColumns(t, s), schemaIndexes(t, s)
	if _, ok := beforeIdx["runs_live_idx"]; !ok {
		t.Fatal("runs_live_idx is missing at 0048")
	}
	applyUpTo(t, s, 49)
	after, afterIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0049 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	for name, def := range beforeIdx {
		if got, ok := afterIdx[name]; !ok || got != def {
			t.Errorf("index %s after 0049 = %q (present %v); want %q", name, got, ok, def)
		}
	}
	for _, col := range []string{"panel_members.run_id", "panel_members.state", "panel_member_logs.member_id",
		"panel_member_diagnostics.member_id", "panel_judges.run_id", "run_usage_models.member_id"} {
		if _, ok := after[col]; !ok {
			t.Errorf("column %s is missing after 0049", col)
		}
	}
}

func insertPanelRun(t *testing.T, s *Store, mr int, sha string) string {
	t.Helper()
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO runs (instance, project_id, mr_iid, head_sha)
		VALUES ('main', 1, $1, $2) RETURNING id`, mr, sha).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertMember(t *testing.T, s *Store, run, role, state string) (string, error) {
	t.Helper()
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO panel_members (run_id, role, state) VALUES ($1, $2, $3) RETURNING id`,
		run, role, state).Scan(&id)
	return id, err
}

// The previous release's inserts still work after 0049, and a run's delete
// takes its members, judges and usage rows with it.
func TestPanelMembersMigrationPreviousRelease(t *testing.T) {
	s := newStoreAt(t, 48)
	applyUpTo(t, s, 49)
	run := insertPanelRun(t, s, 1, "abc")
	if _, err := s.pool.Exec(ctx, `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid,
		trigger, incremental, model, verdict, reported_at, tokens_in, tokens_out, findings, blocking)
		VALUES ($1, 'review', 'main', 1, 'acme/api', 1, 'push', false, 'm', 'pass', $2, 10, 5, 0, 0)`, run, t0); err != nil {
		t.Fatalf("old run_usage insert: %v", err)
	}
	a, err := insertMember(t, s, run, "a", "reported")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO panel_member_logs (member_id, text) VALUES ($1, 'log')`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO panel_member_diagnostics (member_id, clone_log, clone_log_masked, pod_state)
		VALUES ($1, 'c', 'c', 'p')`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO panel_judges (run_id, role, provider, model, model_source, model_entry_id,
		model_entry_name, chain_length) VALUES ($1, 'a', 'anthropic', 'sonnet', 'global', '1', 'n', 1)`, run); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO run_usage_models (run_id, member_id, part, role, skill, provider, model, reported_at, tokens_in, tokens_out)
		VALUES ($1, $2, $3, 'a', 'review', 'anthropic', 'sonnet', $4, 1, 1)`
	if _, err := s.pool.Exec(ctx, ins, run, a, "member", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "merge", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"panel_members", "panel_member_logs", "panel_member_diagnostics", "panel_judges", "run_usage_models", "run_usage"} {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows after its run was deleted; want 0", table, n)
		}
	}
}

func TestPanelMembersOneLivePerRole(t *testing.T) {
	s := newStoreAt(t, 49)
	run := insertPanelRun(t, s, 1, "abc")
	if _, err := insertMember(t, s, run, "a", "queued"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"queued", "running", "reported"} {
		if _, err := insertMember(t, s, run, "a", state); sqlState(err) != "23505" {
			t.Errorf("second live %s row for (run, a): %v; want 23505", state, err)
		}
	}
	if _, err := insertMember(t, s, run, "b", "queued"); err != nil {
		t.Errorf("queued row of the other role: %v", err)
	}
	if _, err := insertMember(t, s, run, "a", "fell_back"); err != nil {
		t.Errorf("fell_back row beside a queued row: %v", err)
	}
	if _, err := insertMember(t, s, run, "a", "failed"); err != nil {
		t.Errorf("failed row beside a queued row: %v", err)
	}
	if _, err := insertMember(t, s, run, "a", "bogus"); sqlState(err) != "23514" {
		t.Errorf("bad state: %v; want 23514", err)
	}
	if _, err := insertMember(t, s, run, "c", "queued"); sqlState(err) != "23514" {
		t.Errorf("bad role: %v; want 23514", err)
	}

	other := insertPanelRun(t, s, 2, "def")
	if _, err := insertMember(t, s, other, "a", "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertMember(t, s, other, "b", "off"); sqlState(err) != "23505" {
		t.Errorf("second off row: %v; want 23505", err)
	}
}

func TestRunUsageModelsKeys(t *testing.T) {
	s := newStoreAt(t, 49)
	run := insertPanelRun(t, s, 1, "abc")
	m, err := insertMember(t, s, run, "a", "queued")
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO run_usage_models (run_id, member_id, part, role, skill, provider, model, reported_at, tokens_in, tokens_out)
		VALUES ($1, $2, $3, $4, $5, 'anthropic', 'sonnet', $6, 1, 1)`
	if _, err := s.pool.Exec(ctx, ins, run, nil, "member", "a", "review", t0); sqlState(err) != "23514" {
		t.Errorf("member row with no member_id: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "merge", "a", "review", t0); sqlState(err) != "23514" {
		t.Errorf("merge row with a member_id: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "member", "a", "review", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "member", "a", "review", t0); sqlState(err) != "23505" {
		t.Errorf("second member row per member and skill: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "member", "a", "other", t0); err != nil {
		t.Errorf("member row of another skill: %v", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "merge", "a", "review", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "merge", "a", "review", t0); sqlState(err) != "23505" {
		t.Errorf("second merge row per run, skill and role: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "merge", "b", "review", t0); err != nil {
		t.Errorf("merge row of the other role: %v", err)
	}
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
