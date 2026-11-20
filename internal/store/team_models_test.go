package store

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

func newTeam(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	team, err := s.CreateTeam(ctx, name, t0)
	if err != nil {
		t.Fatalf("CreateTeam(%s): %v", name, err)
	}
	return team.ID
}

func newTeamModel(t *testing.T, s *Store, teamID int64, name string, key []byte) TeamModel {
	t.Helper()
	m, err := s.CreateTeamModel(ctx, TeamModel{TeamID: teamID, Name: name, Provider: "openai", Model: "gpt-5", APIKeySealed: key}, t0)
	if err != nil {
		t.Fatalf("CreateTeamModel(%s): %v", name, err)
	}
	return m
}

func teamModelByID(t *testing.T, s *Store, teamID int64, id string) (TeamModel, bool) {
	t.Helper()
	ms, err := s.TeamModels(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.ID == id {
			return m, true
		}
	}
	return TeamModel{}, false
}

func TestTeamModelCRUD(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	m, err := s.CreateTeamModel(ctx, TeamModel{ID: "ignored", TeamID: team, Name: "Team key", Provider: "anthropic",
		Model: "sonnet", BaseURL: "https://llm.example.com", APIKeySealed: []byte("k1")}, t0)
	if err != nil {
		t.Fatalf("CreateTeamModel: %v", err)
	}
	if m.ID == "" || m.ID == "ignored" || m.TeamID != team || m.Name != "Team key" || m.Provider != "anthropic" ||
		m.Model != "sonnet" || m.BaseURL != "https://llm.example.com" || !bytes.Equal(m.APIKeySealed, []byte("k1")) ||
		!m.CreatedAt.Equal(t0) || !m.UpdatedAt.Equal(t0) {
		t.Errorf("created = %+v", m)
	}
	second := newTeamModel(t, s, team, "Another", nil)

	// Update with no key keeps it; a name, base URL and key change.
	if found, err := s.UpdateTeamModel(ctx, team, m.ID, strp("Renamed"), nil, nil, t0.Add(time.Hour)); err != nil || !found {
		t.Fatalf("UpdateTeamModel name = %v, %v", found, err)
	}
	got, _ := teamModelByID(t, s, team, m.ID)
	if got.Name != "Renamed" || got.BaseURL != "https://llm.example.com" || !bytes.Equal(got.APIKeySealed, []byte("k1")) ||
		!got.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("after a rename = %+v", got)
	}
	if found, err := s.UpdateTeamModel(ctx, team, m.ID, nil, strp(""), []byte("k2"), t0); err != nil || !found {
		t.Fatalf("UpdateTeamModel key = %v, %v", found, err)
	}
	got, _ = teamModelByID(t, s, team, m.ID)
	if got.BaseURL != "" || !bytes.Equal(got.APIKeySealed, []byte("k2")) || got.Name != "Renamed" {
		t.Errorf("after a key change = %+v", got)
	}
	if _, err := s.UpdateTeamModel(ctx, team, second.ID, strp("renamed"), nil, nil, t0); !errors.Is(err, ErrNameTaken) {
		t.Errorf("rename to a taken name = %v; want ErrNameTaken", err)
	}

	// Another team can't see or change it.
	other := newTeam(t, s, "Growth")
	if found, err := s.UpdateTeamModel(ctx, other, m.ID, strp("x"), nil, nil, t0); err != nil || found {
		t.Errorf("UpdateTeamModel from another team = %v, %v; want not found", found, err)
	}
	if ms, err := s.TeamModels(ctx, other); err != nil || len(ms) != 0 {
		t.Errorf("TeamModels(other) = %+v, %v; want none", ms, err)
	}
	if _, err := s.DeleteTeamModel(ctx, other, m.ID); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("DeleteTeamModel from another team = %v; want ErrModelNotFound", err)
	}

	ms, err := s.TeamModels(ctx, team)
	if err != nil || len(ms) != 2 || ms[0].ID != m.ID || ms[1].ID != second.ID {
		t.Errorf("TeamModels = %+v, %v; want both, oldest first", ms, err)
	}
	newTeamModel(t, s, other, "Growth key", nil)
	if all, err := s.AllTeamModels(ctx); err != nil || len(all) != 3 {
		t.Errorf("AllTeamModels = %+v, %v; want 3", all, err)
	}
	if _, err := s.DeleteTeamModel(ctx, team, m.ID); err != nil {
		t.Fatalf("DeleteTeamModel: %v", err)
	}
	if _, found := teamModelByID(t, s, team, m.ID); found {
		t.Error("the entry is still there")
	}
	if _, err := s.DeleteTeamModel(ctx, team, m.ID); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("DeleteTeamModel twice = %v; want ErrModelNotFound", err)
	}
}

// A name is unique in its team, ignoring case; 2 teams may use the same one.
func TestTeamModelNameUniquePerTeam(t *testing.T) {
	s := newStore(t)
	a, b := newTeam(t, s, "Payments"), newTeam(t, s, "Growth")
	newTeamModel(t, s, a, "Main", nil)
	if _, err := s.CreateTeamModel(ctx, TeamModel{TeamID: a, Name: "MAIN", Provider: "openai", Model: "gpt-5"}, t0); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a taken name = %v; want ErrNameTaken", err)
	}
	newTeamModel(t, s, b, "Main", nil)
}

func markOf(t *testing.T, s *Store, source, id string) (string, bool) {
	t.Helper()
	var class string
	err := s.pool.QueryRow(ctx, `SELECT class FROM model_entry_marks WHERE source = $1 AND model_id = $2`, source, id).Scan(&class)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return class, true
}

func setMark(t *testing.T, s *Store, source, id, class string) {
	t.Helper()
	var until any = t0.Add(time.Hour)
	if class == "auth" {
		until = nil
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO model_entry_marks (source, model_id, class, until, set_at) VALUES ($1, $2, $3, $4, $5)`,
		source, id, class, until, t0); err != nil {
		t.Fatal(err)
	}
}

// Editing a key clears that entry's auth mark, and only that: a quota mark
// stays, and so does the mark of an entry whose key did not change.
func TestKeyEditClearsAuthMark(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g1 := newModel(t, s, "openai", "gpt-5", "", []byte("k1"))
	g2 := newModel(t, s, "anthropic", "sonnet", "", []byte("k2"))
	t1 := newTeamModel(t, s, team, "Rejected", []byte("k3"))
	t2 := newTeamModel(t, s, team, "Out of quota", []byte("k4"))
	setMark(t, s, "global", g1.ID, "auth")
	setMark(t, s, "global", g2.ID, "auth")
	setMark(t, s, "team", t1.ID, "auth")
	setMark(t, s, "team", t2.ID, "quota")

	if _, err := s.UpdateLLMModel(ctx, g2.ID, "https://llm.example.com", nil, t0); err != nil {
		t.Fatal(err)
	}
	if _, marked := markOf(t, s, "global", g2.ID); !marked {
		t.Error("a base URL edit cleared the auth mark")
	}
	if _, err := s.UpdateLLMModel(ctx, g1.ID, "", []byte("new"), t0); err != nil {
		t.Fatal(err)
	}
	if _, marked := markOf(t, s, "global", g1.ID); marked {
		t.Error("a global key edit left the auth mark")
	}
	if _, err := s.UpdateTeamModel(ctx, team, t1.ID, nil, nil, []byte("new"), t0); err != nil {
		t.Fatal(err)
	}
	if _, marked := markOf(t, s, "team", t1.ID); marked {
		t.Error("a team key edit left the auth mark")
	}
	if _, err := s.UpdateTeamModel(ctx, team, t2.ID, nil, nil, []byte("new"), t0); err != nil {
		t.Fatal(err)
	}
	if class, marked := markOf(t, s, "team", t2.ID); !marked || class != "quota" {
		t.Errorf("quota mark after a key edit = %q, %v; want it kept", class, marked)
	}
}

func insertChain(t *testing.T, s *Store, scope string, teamID *int64, instance, project string, entries ...EntryRef) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO model_chains (scope, team_id, instance, project, updated_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, scope, teamID, instance, project, t0).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		if _, err := s.pool.Exec(ctx, `INSERT INTO model_chain_entries (chain_id, position, source, model_id) VALUES ($1, $2, $3, $4)`,
			id, i+1, e.Source, e.ID); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func chainEntries(t *testing.T, s *Store, chainID int64) []EntryRef {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT position, source, model_id FROM model_chain_entries WHERE chain_id = $1 ORDER BY position`, chainID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []EntryRef
	for rows.Next() {
		var pos int
		var e EntryRef
		if err := rows.Scan(&pos, &e.Source, &e.ID); err != nil {
			t.Fatal(err)
		}
		if pos != len(out)+1 {
			t.Errorf("chain %d has position %d at place %d", chainID, pos, len(out)+1)
		}
		out = append(out, e)
	}
	return out
}

// Removing a team entry takes it out of every chain that names it, the rest
// moved up, and returns those chains as they are now. Its mark goes too.
func TestDeleteTeamModelLeavesChains(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	gone := newTeamModel(t, s, team, "Gone", nil)
	kept := newTeamModel(t, s, team, "Kept", nil)
	G, Gone, Kept := EntryRef{SourceGlobal, g.ID}, EntryRef{SourceTeam, gone.ID}, EntryRef{SourceTeam, kept.ID}
	teamChain := insertChain(t, s, "team", &team, "", "", Gone, G, Kept)
	if _, err := s.pool.Exec(ctx, `INSERT INTO dashboard_repos (instance, project, chain_mode) VALUES ('main', 'acme/api', 'custom')`); err != nil {
		t.Fatal(err)
	}
	repoChain := insertChain(t, s, "repo", nil, "main", "acme/api", Gone, G)
	untouched := insertChain(t, s, "repo", nil, "main", "acme/web", Kept)
	setMark(t, s, "team", gone.ID, "auth")

	changed, err := s.DeleteTeamModel(ctx, team, gone.ID)
	if err != nil {
		t.Fatalf("DeleteTeamModel: %v", err)
	}
	ids := map[int64][]EntryRef{}
	for _, c := range changed {
		ids[c.ID] = c.Entries
	}
	if len(changed) != 2 || !slices.Equal(ids[teamChain], []EntryRef{G, Kept}) || !slices.Equal(ids[repoChain], []EntryRef{G}) {
		t.Errorf("changed chains = %+v; want the team and the repository chain, without the entry", changed)
	}
	if got := chainEntries(t, s, teamChain); !slices.Equal(got, []EntryRef{G, Kept}) {
		t.Errorf("team chain = %+v", got)
	}
	if got := chainEntries(t, s, repoChain); !slices.Equal(got, []EntryRef{G}) {
		t.Errorf("repository chain = %+v", got)
	}
	if got := chainEntries(t, s, untouched); !slices.Equal(got, []EntryRef{Kept}) {
		t.Errorf("a chain that did not name it = %+v", got)
	}
	// The repository chain now starts with a global entry: model_id follows.
	repo, _, err := s.AnyDashboardRepo(ctx, "main", "acme/api")
	if err != nil || repo.ModelID == nil || *repo.ModelID != g.ID {
		t.Errorf("model_id = %v, %v; want %s", repo.ModelID, err, g.ID)
	}
	if _, marked := markOf(t, s, "team", gone.ID); marked {
		t.Error("the mark of the removed entry is still there")
	}
}

// Deleting a team deletes its entries, its chain and their marks, and takes
// its entries out of the repository chains that named them.
func TestDeleteTeamCascades(t *testing.T) {
	s := newStore(t)
	team, other := newTeam(t, s, "Payments"), newTeam(t, s, "Growth")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	m := newTeamModel(t, s, team, "Team key", nil)
	keep := newTeamModel(t, s, other, "Other key", nil)
	insertChain(t, s, "team", &team, "", "", EntryRef{SourceTeam, m.ID})
	otherChain := insertChain(t, s, "team", &other, "", "", EntryRef{SourceTeam, keep.ID})
	repoChain := insertChain(t, s, "repo", nil, "main", "acme/api", EntryRef{SourceTeam, m.ID}, EntryRef{SourceGlobal, g.ID})
	setMark(t, s, "team", m.ID, "quota")
	setMark(t, s, "team", keep.ID, "quota")

	if ok, err := s.DeleteTeam(ctx, team, nil); err != nil || !ok {
		t.Fatalf("DeleteTeam = %v, %v", ok, err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM team_models WHERE team_id = $1`, team).Scan(&n); err != nil || n != 0 {
		t.Errorf("team models left = %d, %v", n, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM model_chains WHERE team_id = $1`, team).Scan(&n); err != nil || n != 0 {
		t.Errorf("team chains left = %d, %v", n, err)
	}
	if _, marked := markOf(t, s, "team", m.ID); marked {
		t.Error("the mark of a deleted team's entry is still there")
	}
	if _, marked := markOf(t, s, "team", keep.ID); !marked {
		t.Error("another team's mark went")
	}
	if got := chainEntries(t, s, otherChain); !slices.Equal(got, []EntryRef{{SourceTeam, keep.ID}}) {
		t.Errorf("another team's chain = %+v", got)
	}
	if got := chainEntries(t, s, repoChain); !slices.Equal(got, []EntryRef{{SourceGlobal, g.ID}}) {
		t.Errorf("repository chain = %+v; want the team entry gone", got)
	}
}
