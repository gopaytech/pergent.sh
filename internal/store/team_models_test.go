package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// columnInfo is a column's type and nullability, as information_schema says.
type columnInfo struct{ Type, Nullable string }

func schemaColumns(t *testing.T, s *Store) map[string]columnInfo {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT table_name, column_name, data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name <> 'schema_migrations'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]columnInfo{}
	for rows.Next() {
		var table, col string
		var c columnInfo
		if err := rows.Scan(&table, &col, &c.Type, &c.Nullable); err != nil {
			t.Fatal(err)
		}
		out[table+"."+col] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// schemaIndexes is every index of the schema, by name, with its definition.
func schemaIndexes(t *testing.T, s *Store) map[string]string {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		out[name] = def
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Migration 0045 only adds, so the previous release keeps working on the
// migrated database. Its one drop is the unique rule on (provider, model,
// base_url), which only relaxes. The file check runs without a database.
func TestTeamModelsMigrationOnlyAdds(t *testing.T) {
	b, err := os.ReadFile("migrations/0045_team_models.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sql.WriteString(line + "\n")
		}
	}
	// The relaxed rule and a foreign key's ON DELETE are not a drop or a delete.
	text := regexp.MustCompile(`(?i)DROP\s+CONSTRAINT\s+llm_models_provider_model_base_url_key|ON\s+DELETE\s+CASCADE`).ReplaceAllString(sql.String(), "")
	for _, bad := range []string{`(?i)\bDROP\b`, `(?i)\bRENAME\b`, `(?i)\bTRUNCATE\b`, `(?i)\bDELETE\b`,
		`(?i)\bALTER\s+COLUMN\b`, `(?i)\bSET\s+NOT\s+NULL\b`} {
		if re := regexp.MustCompile(bad); re.MatchString(text) {
			t.Errorf("migration 0045 matches %s; it may only add", bad)
		}
	}
	for _, stmt := range strings.Split(text, ";") {
		up := strings.ToUpper(stmt)
		if strings.Contains(up, "ADD COLUMN") && strings.Contains(up, "NOT NULL") && !strings.Contains(up, "DEFAULT") {
			t.Errorf("%q adds a NOT NULL column with no default", strings.TrimSpace(stmt))
		}
	}

	s := newStoreAt(t, 44)
	before, beforeIdx := schemaColumns(t, s), schemaIndexes(t, s)
	// The previous release's own migration (0044, the code graph) is in what
	// must survive.
	for _, col := range []string{"dashboard_repos.graph_context", "repo_graphs.build_token_hash"} {
		if _, ok := before[col]; !ok {
			t.Fatalf("column %s is missing at 0044", col)
		}
	}
	applyUpTo(t, s, 45)
	after, afterIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0045 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	for name, def := range beforeIdx {
		if name == "llm_models_provider_model_base_url_key" {
			continue // the one rule 0045 relaxes
		}
		if got, ok := afterIdx[name]; !ok || got != def {
			t.Errorf("index %s after 0045 = %q (present %v); want %q", name, got, ok, def)
		}
	}
	if _, ok := afterIdx["llm_models_provider_model_base_url_key"]; ok {
		t.Error("the unique rule on (provider, model, base_url) is still there")
	}
	// llm_models.name stays nullable: the previous release inserts no name.
	if c := after["llm_models.name"]; c.Nullable != "YES" {
		t.Errorf("llm_models.name = %+v; want nullable", c)
	}
}

// Upgrade fills the names, gives a repository with a model its own 1-entry
// chain, and stores no global chain.
func TestTeamModelsMigrationBackfill(t *testing.T) {
	s := newStoreAt(t, 44)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO llm_models (id, provider, model, base_url, is_default, created_at, updated_at) VALUES
		('m1', 'openai', 'gpt-5', '', true, $1, $1),
		('m2', 'openai', 'gpt-5', 'https://llm.example.com', false, $1::timestamptz + interval '1 minute', $1),
		('m3', 'anthropic', 'sonnet', '', false, $1::timestamptz + interval '2 minutes', $1)`, t0)
	exec(`INSERT INTO dashboard_repos (instance, project, model_id) VALUES ('main', 'Acme/API', 'm2'), ('main', 'acme/web', NULL)`)
	applyUpTo(t, s, 45)

	names := map[string]string{}
	rows, err := s.pool.Query(ctx, `SELECT id, name FROM llm_models`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		names[id] = name
	}
	rows.Close()
	if names["m1"] != "openai/gpt-5" || names["m2"] != "openai/gpt-5 (2)" || names["m3"] != "anthropic/sonnet" {
		t.Errorf("names = %v", names)
	}

	mode := func(project string) string {
		t.Helper()
		var m string
		if err := s.pool.QueryRow(ctx, `SELECT chain_mode FROM dashboard_repos WHERE project = $1`, project).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if got := mode("Acme/API"); got != "custom" {
		t.Errorf("chain_mode with a model = %q; want custom", got)
	}
	if got := mode("acme/web"); got != "inherit" {
		t.Errorf("chain_mode without a model = %q; want inherit", got)
	}
	type entry struct {
		Scope, Instance, Project, Source, ModelID string
		Position                                  int
	}
	rows, err = s.pool.Query(ctx, `SELECT c.scope, c.instance, c.project, e.position, e.source, e.model_id
		FROM model_chains c LEFT JOIN model_chain_entries e ON e.chain_id = c.id ORDER BY c.id, e.position`)
	if err != nil {
		t.Fatal(err)
	}
	var got []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.Scope, &e.Instance, &e.Project, &e.Position, &e.Source, &e.ModelID); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	rows.Close()
	want := []entry{{Scope: "repo", Instance: "main", Project: "Acme/API", Position: 1, Source: "global", ModelID: "m2"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("chains = %+v; want %+v (and no global chain)", got, want)
	}
}

// What the previous release (0044) writes still works on the migrated
// database: an entry with no name, even a copy of another, and a team
// repository with no owner.
func TestTeamModelsMigrationPreviousReleaseInsert(t *testing.T) {
	s := newStoreAt(t, 45)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	insert := `INSERT INTO llm_models (id, provider, model, base_url, api_key_sealed, is_default, created_at, updated_at)
		VALUES (gen_random_uuid()::text, 'openai', 'gpt-5', '', NULL, false, $1, $1)`
	exec(insert, t0)
	exec(insert, t0)
	exec(`INSERT INTO teams (name, role, created_at) VALUES ('Payments', 'viewer', $1)`, t0)
	exec(`INSERT INTO team_repos (team_id, instance, project) SELECT id, 'main', 'acme/api' FROM teams`)
	exec(`INSERT INTO dashboard_repos (instance, project, model_id) VALUES ('main', 'acme/api', NULL)`)
	exec(`INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, target_branch)
		VALUES ('main', 7, 3, 'acme/api', 'aaa', 'main')`)
	exec(`INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid, trigger, incremental, model,
			verdict, reported_at, tokens_in, tokens_out, findings, blocking)
		SELECT id, 'style', 'main', 7, 'acme/api', 3, 'push', false, 'gpt-5', 'no_blocking_issues', now(), 1, 2, 0, 0 FROM runs`)
	var source string
	if err := s.pool.QueryRow(ctx, `SELECT source FROM run_usage`).Scan(&source); err != nil || source != "global" {
		t.Errorf("run_usage.source = %q, %v; want global", source, err)
	}
}

// One owner team per (instance, project), ignoring case; the instance entry
// is its own level.
func TestOneOwnerPerLevel(t *testing.T) {
	s := newStoreAt(t, 45)
	var a, b int64
	for name, id := range map[string]*int64{"Payments": &a, "Growth": &b} {
		if err := s.pool.QueryRow(ctx, `INSERT INTO teams (name, role, created_at) VALUES ($1, 'viewer', $2) RETURNING id`,
			name, t0).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	owner := func(team int64, project string) error {
		_, err := s.pool.Exec(ctx, `INSERT INTO team_repos (team_id, instance, project, owner) VALUES ($1, 'main', $2, true)`,
			team, project)
		return err
	}
	if err := owner(a, "acme/api"); err != nil {
		t.Fatalf("owner of the repository: %v", err)
	}
	if err := owner(a, ""); err != nil {
		t.Fatalf("owner of the instance: %v", err)
	}
	if err := owner(b, "ACME/api"); !isUniqueViolation(err) {
		t.Errorf("a second owner = %v; want a unique violation", err)
	}
	// Not an owner: any number of teams may cover it.
	if _, err := s.pool.Exec(ctx, `INSERT INTO team_repos (team_id, instance, project) VALUES ($1, 'main', 'acme/api')`, b); err != nil {
		t.Errorf("a team that does not own it: %v", err)
	}
}
