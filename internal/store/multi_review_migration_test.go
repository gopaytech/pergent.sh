package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 0050 only adds: nothing of 0049 changes, and the panel tables and
// runs_live_idx are untouched.
func TestMultiReviewMigrationOnlyAdds(t *testing.T) {
	b, err := os.ReadFile("migrations/0050_multi_review.sql")
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
		`(?i)\bUPDATE\b`, `(?i)\bALTER\b`, `(?i)\bINSERT\b`} {
		if regexp.MustCompile(bad).MatchString(text) {
			t.Errorf("migration 0050 matches %s; it may only add", bad)
		}
	}

	s := newStoreAt(t, 49)
	before, beforeIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for _, name := range []string{"runs_live_idx", "panel_members_live_idx", "panel_members_off_idx", "panel_chains_key"} {
		if _, ok := beforeIdx[name]; !ok {
			t.Fatalf("index %s is missing at 0049", name)
		}
	}
	for _, col := range []string{"panel_members.role", "panel_members.state", "panel_chains.saved_by_admin",
		"panel_chain_entries.model_id", "run_usage_models.member_id"} {
		if _, ok := before[col]; !ok {
			t.Fatalf("column %s is missing at 0049", col)
		}
	}
	applyUpTo(t, s, 50)
	after, afterIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0050 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	for name, def := range beforeIdx {
		if got, ok := afterIdx[name]; !ok || got != def {
			t.Errorf("index %s after 0050 = %q (present %v); want %q", name, got, ok, def)
		}
	}
	for _, col := range []string{"reviewer_chains.slot", "reviewer_chain_entries.model_id", "review_members.reviewer",
		"review_members.chain", "review_member_logs.member_id", "review_member_diagnostics.member_id",
		"review_judge_chains.chain", "review_judges.reviewers", "run_usage_by_model.part"} {
		if _, ok := after[col]; !ok {
			t.Errorf("column %s is missing after 0050", col)
		}
	}
}

const frozenChain = `{"scope": "repo", "saved_by_admin": false, "entries": [{"source": "global", "id": "1", "name": "n", "provider": "anthropic", "model": "sonnet"}]}`

// insertReviewMember adds a review_members row; chain "" stores NULL.
func insertReviewMember(t *testing.T, s *Store, run string, reviewer int, state, chain string) (string, error) {
	t.Helper()
	var c any
	if chain != "" {
		c = chain
	}
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO review_members (run_id, reviewer, state, chain)
		VALUES ($1, $2, $3, $4::jsonb) RETURNING id`, run, reviewer, state, c).Scan(&id)
	return id, err
}

// v0.18.0's inserts still work after 0050, and a run's delete takes its
// members, judge rows and usage rows with it.
func TestMultiReviewMigrationPreviousRelease(t *testing.T) {
	s := newStoreAt(t, 49)
	applyUpTo(t, s, 50)
	run := insertPanelRun(t, s, 1, "abc")
	if _, err := s.pool.Exec(ctx, `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid,
		trigger, incremental, model, verdict, reported_at, tokens_in, tokens_out, findings, blocking)
		VALUES ($1, 'review', 'main', 1, 'acme/api', 1, 'push', false, 'm', 'pass', $2, 10, 5, 0, 0)`, run, t0); err != nil {
		t.Fatalf("old run_usage insert: %v", err)
	}
	if _, err := insertMember(t, s, run, "a", "queued"); err != nil {
		t.Fatalf("old panel_members insert: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO model_chains (scope, instance, project, updated_at)
		VALUES ('repo', 'main', 'acme/api', $1), ('global', '', '', $1)`, t0); err != nil {
		t.Fatalf("old model_chains insert: %v", err)
	}

	m, err := insertReviewMember(t, s, run, 2, "reported", frozenChain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO review_member_logs (member_id, text) VALUES ($1, 'log')`, m); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO review_member_diagnostics (member_id, clone_log, clone_log_masked, pod_state)
		VALUES ($1, 'c', 'c', 'p')`, m); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO review_judge_chains (run_id, chain) VALUES ($1, $2::jsonb)`, run, frozenChain); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO review_judges (run_id, reviewers, no_judge, provider, model)
		VALUES ($1, '{1,2}', false, 'anthropic', 'sonnet')`, run); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO run_usage_by_model (run_id, member_id, part, reviewer, skill, provider, model, reported_at, tokens_in, tokens_out)
		VALUES ($1, $2, $3, $4, 'review', 'anthropic', 'sonnet', $5, 1, 1)`
	if _, err := s.pool.Exec(ctx, ins, run, m, "reviewer", 2, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "judge", nil, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"review_members", "review_member_logs", "review_member_diagnostics",
		"review_judge_chains", "review_judges", "run_usage_by_model", "panel_members", "run_usage"} {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows after its run was deleted; want 0", table, n)
		}
	}
}

func TestReviewerChainOneRowPerSlot(t *testing.T) {
	s := newStoreAt(t, 50)
	ins := `INSERT INTO reviewer_chains (instance, project, slot, updated_at) VALUES ($1, $2, $3, $4)`
	if _, err := s.pool.Exec(ctx, ins, "main", "acme/api", "r2", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, "main", "ACME/api", "r2", t0); sqlState(err) != "23505" {
		t.Errorf("second r2 of one repository, other case: %v; want 23505", err)
	}
	for _, slot := range []string{"r3", "r4", "r5", "judge"} {
		if _, err := s.pool.Exec(ctx, ins, "main", "acme/api", slot, t0); err != nil {
			t.Errorf("slot %s: %v", slot, err)
		}
	}
	if _, err := s.pool.Exec(ctx, ins, "other", "acme/api", "r2", t0); err != nil {
		t.Errorf("r2 of the same project on another instance: %v", err)
	}
	for _, slot := range []string{"r1", "r6", ""} {
		if _, err := s.pool.Exec(ctx, ins, "main", "acme/web", slot, t0); sqlState(err) != "23514" {
			t.Errorf("slot %q: %v; want 23514", slot, err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO reviewer_chains (scope, instance, project, slot, updated_at)
		VALUES ('team', 'main', 'acme/web', 'r2', $1)`, t0); sqlState(err) != "23514" {
		t.Errorf("scope team: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO reviewer_chains (team_id, instance, project, slot, updated_at)
		VALUES (1, 'main', 'acme/web', 'r2', $1)`, t0); sqlState(err) != "23514" {
		t.Errorf("a team_id: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, "", "acme/web", "r2", t0); sqlState(err) != "23514" {
		t.Errorf("empty instance: %v; want 23514", err)
	}

	var chain int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM reviewer_chains WHERE slot = 'r2' AND instance = 'main'`).Scan(&chain); err != nil {
		t.Fatal(err)
	}
	ent := `INSERT INTO reviewer_chain_entries (chain_id, position, source, model_id) VALUES ($1, $2, $3, '1')`
	if _, err := s.pool.Exec(ctx, ent, chain, 1, "global"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ent, chain, 1, "team"); sqlState(err) != "23505" {
		t.Errorf("second entry at position 1: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ent, chain, 6, "global"); sqlState(err) != "23514" {
		t.Errorf("position 6: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ent, chain, 2, "other"); sqlState(err) != "23514" {
		t.Errorf("source other: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM reviewer_chains WHERE id = $1`, chain); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM reviewer_chain_entries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d entries left after their chain was deleted; want 0", n)
	}
}

func TestReviewMembersOneLivePerReviewer(t *testing.T) {
	s := newStoreAt(t, 50)
	run := insertPanelRun(t, s, 1, "abc")
	if _, err := insertReviewMember(t, s, run, 2, "queued", frozenChain); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"queued", "running", "reported"} {
		if _, err := insertReviewMember(t, s, run, 2, state, frozenChain); sqlState(err) != "23505" {
			t.Errorf("second live %s row for (run, 2): %v; want 23505", state, err)
		}
	}
	for _, state := range []string{"fell_back", "failed", "done", "superseded", "skipped"} {
		if _, err := insertReviewMember(t, s, run, 2, state, frozenChain); err != nil {
			t.Errorf("%s row beside a queued row of one reviewer: %v", state, err)
		}
	}
	for r := 1; r <= 5; r++ {
		if r == 2 {
			continue
		}
		if _, err := insertReviewMember(t, s, run, r, "queued", frozenChain); err != nil {
			t.Errorf("queued row of reviewer %d: %v", r, err)
		}
	}
	for _, r := range []int{0, 6} {
		if _, err := insertReviewMember(t, s, run, r, "failed", frozenChain); sqlState(err) != "23514" {
			t.Errorf("reviewer %d: %v; want 23514", r, err)
		}
	}
	if _, err := insertReviewMember(t, s, run, 3, "bogus", frozenChain); sqlState(err) != "23514" {
		t.Errorf("bad state: %v; want 23514", err)
	}
	if _, err := insertReviewMember(t, s, run, 3, "failed", ""); sqlState(err) != "23514" {
		t.Errorf("failed row without a chain: %v; want 23514", err)
	}

	// A failed attempt may keep the result it delivered (Rulings P12, P20).
	var kept string
	if err := s.pool.QueryRow(ctx, `INSERT INTO review_members (run_id, reviewer, state, chain, result, failure_reason)
		VALUES ($1, 4, 'failed', $2::jsonb, '{"skills": []}', 'provider refused') RETURNING id`, run, frozenChain).Scan(&kept); err != nil {
		t.Errorf("failed row with a result: %v", err)
	}

	other := insertPanelRun(t, s, 2, "def")
	if _, err := insertReviewMember(t, s, other, 1, "off", frozenChain); sqlState(err) != "23514" {
		t.Errorf("off row with a chain: %v; want 23514", err)
	}
	if _, err := insertReviewMember(t, s, other, 1, "queued", ""); sqlState(err) != "23514" {
		t.Errorf("queued row without a chain: %v; want 23514", err)
	}
	if _, err := insertReviewMember(t, s, other, 1, "off", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := insertReviewMember(t, s, other, 2, "off", ""); sqlState(err) != "23505" {
		t.Errorf("second off row: %v; want 23505", err)
	}

	first, err := insertReviewMember(t, s, other, 3, "fell_back", frozenChain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO review_members (run_id, reviewer, state, chain, attempt_of)
		VALUES ($1, 3, 'queued', $2::jsonb, $3)`, other, frozenChain, first); err != nil {
		t.Errorf("later attempt pointing at the first: %v", err)
	}
}

func TestRunUsageByModelKeys(t *testing.T) {
	s := newStoreAt(t, 50)
	run := insertPanelRun(t, s, 1, "abc")
	m, err := insertReviewMember(t, s, run, 2, "queued", frozenChain)
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO run_usage_by_model (run_id, member_id, part, reviewer, skill, provider, model, reported_at, tokens_in, tokens_out)
		VALUES ($1, $2, $3, $4, $5, 'anthropic', 'sonnet', $6, 1, 1)`
	if _, err := s.pool.Exec(ctx, ins, run, nil, "reviewer", 2, "review", t0); sqlState(err) != "23514" {
		t.Errorf("reviewer row with no member_id: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "reviewer", nil, "review", t0); sqlState(err) != "23514" {
		t.Errorf("reviewer row with no reviewer: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "judge", nil, "review", t0); sqlState(err) != "23514" {
		t.Errorf("judge row with a member_id: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "judge", 2, "review", t0); sqlState(err) != "23514" {
		t.Errorf("judge row with a reviewer: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "reviewer", 6, "review", t0); sqlState(err) != "23514" {
		t.Errorf("reviewer 6: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "merge", nil, "review", t0); sqlState(err) != "23514" {
		t.Errorf("part merge: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "reviewer", 2, "review", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "reviewer", 2, "review", t0); sqlState(err) != "23505" {
		t.Errorf("second reviewer row per member and skill: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "reviewer", 2, "other", t0); err != nil {
		t.Errorf("reviewer row of another skill: %v", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "judge", nil, "review", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "judge", nil, "review", t0); sqlState(err) != "23505" {
		t.Errorf("second judge row per run and skill: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "judge", nil, "other", t0); err != nil {
		t.Errorf("judge row of another skill: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO run_usage_by_model (run_id, part, skill, provider, model, reported_at,
		tokens_in, tokens_out) VALUES ($1, 'judge', 'x', 'p', 'm', $2, -1, 1)`, run, t0); sqlState(err) != "23514" {
		t.Errorf("negative tokens_in: %v; want 23514", err)
	}
}
