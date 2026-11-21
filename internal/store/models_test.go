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

func newModel(t *testing.T, s *Store, provider, model, baseURL string, key []byte) LLMModel {
	t.Helper()
	m, err := s.CreateLLMModel(ctx, LLMModel{Provider: provider, Model: model, BaseURL: baseURL, APIKeySealed: key}, t0)
	if err != nil {
		t.Fatalf("CreateLLMModel(%s/%s): %v", provider, model, err)
	}
	return m
}

// settings reads the llm_settings row the previous release reads.
func settings(t *testing.T, s *Store) (LLMRow, bool) {
	t.Helper()
	r, found, err := s.LLM(ctx)
	if err != nil {
		t.Fatalf("LLM: %v", err)
	}
	return r, found
}

func defaultsOf(models []LLMModel) []string {
	var ids []string
	for _, m := range models {
		if m.Default {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

func strp(s string) *string { return &s }

// The migration only adds, so the previous release keeps working on the
// migrated database. This runs without a database.
func TestModelsMigrationOnlyAdds(t *testing.T) {
	b, err := os.ReadFile("migrations/0033_llm_models.sql")
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
		if re := regexp.MustCompile(bad); re.MatchString(sql.String()) {
			t.Errorf("migration 0033 matches %s; it may only add", bad)
		}
	}
	// A column added NOT NULL needs a default, or the previous release's
	// inserts fail.
	for _, stmt := range strings.Split(sql.String(), ";") {
		if strings.Contains(strings.ToUpper(stmt), "ADD COLUMN") && strings.Contains(strings.ToUpper(stmt), "NOT NULL") &&
			!strings.Contains(strings.ToUpper(stmt), "DEFAULT") {
			t.Errorf("%q adds a NOT NULL column with no default", strings.TrimSpace(stmt))
		}
	}
}

// modelsAt33 reads the list with the columns 0033 made, for a database
// migrated that far: LLMModels reads columns later migrations add.
func modelsAt33(t *testing.T, s *Store) ([]LLMModel, error) {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT id, provider, model, base_url, api_key_sealed, is_default, created_at, updated_at
		FROM llm_models ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LLMModel, error) {
		var m LLMModel
		err := row.Scan(&m.ID, &m.Provider, &m.Model, &m.BaseURL, &m.APIKeySealed, &m.Default, &m.CreatedAt, &m.UpdatedAt)
		return m, err
	})
}

// The row of llm_settings becomes the first entry and the default, with the
// same sealed key; no row is an empty list.
func TestMigrationCopiesLLMSettings(t *testing.T) {
	s := newStoreAt(t, 32)
	if _, err := s.pool.Exec(ctx, `INSERT INTO llm_settings (id, provider, model, base_url, api_key_sealed, updated_at)
		VALUES (1, 'anthropic', 'claude-sonnet-4', 'https://llm.example.com', '\x0102030405', $1)`, t0); err != nil {
		t.Fatal(err)
	}
	applyUpTo(t, s, 33)

	models, err := modelsAt33(t, s)
	if err != nil || len(models) != 1 {
		t.Fatalf("models = %+v, %v; want one entry", models, err)
	}
	m := models[0]
	if !m.Default || m.Provider != "anthropic" || m.Model != "claude-sonnet-4" || m.BaseURL != "https://llm.example.com" ||
		!bytes.Equal(m.APIKeySealed, []byte{1, 2, 3, 4, 5}) || !m.CreatedAt.Equal(t0) || !m.UpdatedAt.Equal(t0) || m.ID == "" {
		t.Errorf("entry = %+v", m)
	}
	// llm_settings is left as it was.
	if r, found := settings(t, s); !found || r.Provider != "anthropic" || !bytes.Equal(r.APIKeySealed, []byte{1, 2, 3, 4, 5}) {
		t.Errorf("llm_settings = %+v, %v; want it unchanged", r, found)
	}
}

func TestMigrationWithoutLLMSettingsIsEmpty(t *testing.T) {
	s := newStoreAt(t, 33)
	if models, err := modelsAt33(t, s); err != nil || len(models) != 0 {
		t.Errorf("models = %+v, %v; want none", models, err)
	}
}

// What the previous release (0032) reads and writes still works on the
// migrated database, and its rows come out with no model and no provider.
func TestMigrationKeepsPreviousReleaseModels(t *testing.T) {
	s := newStoreAt(t, 32)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO dashboard_repos (instance, project, created_by) VALUES ('lab', 'acme/web', 'alice')`)
	exec(`INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, target_branch)
		VALUES ('lab', 7, 3, 'acme/web', 'aaa', 'main')`)
	usage := `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid, trigger, incremental, model,
			verdict, reported_at, tokens_in, tokens_out, findings, blocking)
		SELECT id, $1::text, 'lab', 7, 'acme/web', 3, 'push', false, 'claude', 'no_blocking_issues', now(), 1, 2, 0, 0 FROM runs`
	exec(usage, "style")

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	repo, found, err := s.DashboardRepo(ctx, "lab", "acme/web")
	if err != nil || !found || repo.ModelID != nil {
		t.Fatalf("repository after the migration = %+v, %v, %v; want no model", repo, found, err)
	}
	claimed, err := s.ClaimQueued(ctx, 10, t0)
	if err != nil || len(claimed) != 1 || claimed[0].Provider != nil {
		t.Fatalf("ClaimQueued = %+v, %v; want the run with no provider", claimed, err)
	}
	var provider string
	if err := s.pool.QueryRow(ctx, `SELECT provider FROM run_usage`).Scan(&provider); err != nil || provider != "" {
		t.Errorf("run_usage.provider = %q, %v; want empty", provider, err)
	}
	// The previous release's insert names no provider.
	exec(usage, "security")
	// And it writes llm_settings through PutLLM, which the models don't need.
	if err := s.PutLLM(ctx, "openai", "gpt", "", []byte{9}, t0); err != nil {
		t.Errorf("PutLLM: %v", err)
	}
}

func TestCreateLLMModelFirstIsDefault(t *testing.T) {
	s := newStore(t)
	first, err := s.CreateLLMModel(ctx, LLMModel{ID: "ignored", Default: false, Provider: "anthropic", Model: "sonnet",
		APIKeySealed: []byte("k1")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Default || first.ID == "" || first.ID == "ignored" || first.Provider != "anthropic" || first.Model != "sonnet" ||
		first.BaseURL != "" || !bytes.Equal(first.APIKeySealed, []byte("k1")) || !first.CreatedAt.Equal(t0) || !first.UpdatedAt.Equal(t0) {
		t.Errorf("first entry = %+v; want the default, with a generated ID", first)
	}
	second, err := s.CreateLLMModel(ctx, LLMModel{Provider: "openai", Model: "gpt", Default: true}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.Default || second.APIKeySealed != nil || !second.CreatedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("second entry = %+v; want not the default, with no key", second)
	}

	models, err := s.LLMModels(ctx)
	if err != nil || len(models) != 2 || models[0].ID != first.ID || models[1].ID != second.ID {
		t.Fatalf("LLMModels = %+v, %v; want oldest first", models, err)
	}
	if got := defaultsOf(models); !slices.Equal(got, []string{first.ID}) {
		t.Errorf("defaults = %v; want just the first", got)
	}
}

// The same provider, model and base URL is another entry now: names tell
// them apart (0045 dropped the rule, and the store no longer checks it).
func TestCreateLLMModelSameModelTwice(t *testing.T) {
	s := newStore(t)
	newModel(t, s, "anthropic", "sonnet", "", nil)
	if _, err := s.CreateLLMModel(ctx, LLMModel{Name: "second key", Provider: "anthropic", Model: "sonnet"}, t0); err != nil {
		t.Fatalf("the same model again = %v; want a second entry", err)
	}
	if models, _ := s.LLMModels(ctx); len(models) != 2 || len(defaultsOf(models)) != 1 {
		t.Errorf("list = %+v; want two entries and one default", models)
	}
}

func TestUpdateLLMModelKeepsKeyOnNil(t *testing.T) {
	s := newStore(t)
	newModel(t, s, "anthropic", "sonnet", "", []byte("k1"))
	m := newModel(t, s, "openai", "gpt", "", []byte("k2"))

	found, err := s.UpdateLLMModel(ctx, m.ID, "https://llm.example.com", nil, t0.Add(time.Hour))
	if err != nil || !found {
		t.Fatalf("UpdateLLMModel with no key = %v, %v", found, err)
	}
	got := modelByID(t, s, m.ID)
	if got.BaseURL != "https://llm.example.com" || !bytes.Equal(got.APIKeySealed, []byte("k2")) || !got.UpdatedAt.Equal(t0.Add(time.Hour)) ||
		!got.CreatedAt.Equal(t0) || got.Provider != "openai" || got.Model != "gpt" {
		t.Errorf("after an update with no key = %+v; want the key kept", got)
	}

	if found, err := s.UpdateLLMModel(ctx, m.ID, "", []byte("k3"), t0.Add(2*time.Hour)); err != nil || !found {
		t.Fatalf("UpdateLLMModel with a key = %v, %v", found, err)
	}
	got = modelByID(t, s, m.ID)
	if got.BaseURL != "" || !bytes.Equal(got.APIKeySealed, []byte("k3")) {
		t.Errorf("after an update with a key = %+v; want the new key and an empty base URL", got)
	}

	if found, err := s.UpdateLLMModel(ctx, "no-such-id", "", nil, t0); err != nil || found {
		t.Errorf("UpdateLLMModel of a missing entry = %v, %v; want not found", found, err)
	}
}

// A base URL that makes an entry the same as another is allowed.
func TestUpdateLLMModelBaseURLSameAsAnother(t *testing.T) {
	s := newStore(t)
	newModel(t, s, "anthropic", "sonnet", "https://a.example.com", nil)
	m := newModel(t, s, "anthropic", "sonnet", "https://b.example.com", nil)
	if found, err := s.UpdateLLMModel(ctx, m.ID, "https://a.example.com", nil, t0); err != nil || !found {
		t.Errorf("UpdateLLMModel to another entry's base URL = %v, %v; want it changed", found, err)
	}
	if got := modelByID(t, s, m.ID); got.BaseURL != "https://a.example.com" {
		t.Errorf("base URL = %q", got.BaseURL)
	}
}

func modelByID(t *testing.T, s *Store, id string) LLMModel {
	t.Helper()
	models, err := s.LLMModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("model %s is not in the list", id)
	return LLMModel{}
}

func TestSetDefaultLLMModel(t *testing.T) {
	s := newStore(t)
	a := newModel(t, s, "anthropic", "sonnet", "", []byte("ka"))
	b := newModel(t, s, "openai", "gpt", "https://llm.example.com", []byte("kb"))

	if found, err := s.SetDefaultLLMModel(ctx, b.ID, t0.Add(time.Hour)); err != nil || !found {
		t.Fatalf("SetDefaultLLMModel = %v, %v", found, err)
	}
	models, _ := s.LLMModels(ctx)
	if got := defaultsOf(models); !slices.Equal(got, []string{b.ID}) {
		t.Errorf("defaults = %v; want just b", got)
	}
	// Setting the default again is no change.
	if found, err := s.SetDefaultLLMModel(ctx, b.ID, t0.Add(2*time.Hour)); err != nil || !found {
		t.Fatalf("SetDefaultLLMModel again = %v, %v", found, err)
	}
	// A missing entry leaves the default where it is.
	if found, err := s.SetDefaultLLMModel(ctx, "no-such-id", t0); err != nil || found {
		t.Errorf("SetDefaultLLMModel of a missing entry = %v, %v; want not found", found, err)
	}
	models, _ = s.LLMModels(ctx)
	if got := defaultsOf(models); !slices.Equal(got, []string{b.ID}) {
		t.Errorf("defaults after a missing id = %v; want b still", got)
	}
	if found, err := s.SetDefaultLLMModel(ctx, a.ID, t0); err != nil || !found {
		t.Fatal(found, err)
	}
	models, _ = s.LLMModels(ctx)
	if got := defaultsOf(models); !slices.Equal(got, []string{a.ID}) {
		t.Errorf("defaults after going back = %v; want a", got)
	}
}

// llm_settings, which the previous release reads, always matches the default
// entry: a rollback runs on the current default.
func TestSetDefaultWritesLLMSettings(t *testing.T) {
	s := newStore(t)
	if _, found := settings(t, s); found {
		t.Fatal("llm_settings has a row before any entry")
	}
	// The first entry is the default, so it is written.
	a := newModel(t, s, "anthropic", "sonnet", "", []byte("ka"))
	if r, found := settings(t, s); !found || r.Provider != "anthropic" || r.Model != "sonnet" || r.BaseURL != "" ||
		!bytes.Equal(r.APIKeySealed, []byte("ka")) || !r.UpdatedAt.Equal(t0) {
		t.Fatalf("llm_settings after the first entry = %+v, %v", r, found)
	}
	// A second, not the default, leaves it alone.
	b, err := s.CreateLLMModel(ctx, LLMModel{Provider: "openai", Model: "gpt", BaseURL: "https://llm.example.com"}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := settings(t, s); r.Provider != "anthropic" {
		t.Errorf("llm_settings after a non-default entry = %+v; want it unchanged", r)
	}

	if found, err := s.SetDefaultLLMModel(ctx, b.ID, t0.Add(2*time.Hour)); err != nil || !found {
		t.Fatal(found, err)
	}
	// b has no key: the row must not keep a's key under b's provider.
	r, _ := settings(t, s)
	if r.Provider != "openai" || r.Model != "gpt" || r.BaseURL != "https://llm.example.com" || r.APIKeySealed != nil ||
		!r.UpdatedAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("llm_settings after setting b the default = %+v; want b, with no key", r)
	}
	if found, err := s.SetDefaultLLMModel(ctx, a.ID, t0.Add(3*time.Hour)); err != nil || !found {
		t.Fatal(found, err)
	}
	if r, _ := settings(t, s); r.Provider != "anthropic" || !bytes.Equal(r.APIKeySealed, []byte("ka")) {
		t.Errorf("llm_settings after going back to a = %+v; want a and its key", r)
	}
	// A missing entry writes nothing.
	if found, _ := s.SetDefaultLLMModel(ctx, "no-such-id", t0.Add(4*time.Hour)); found {
		t.Fatal("a missing entry was found")
	}
	if r, _ := settings(t, s); r.Provider != "anthropic" || !r.UpdatedAt.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("llm_settings after a missing id = %+v; want it unchanged", r)
	}
}

func TestUpdateDefaultWritesLLMSettings(t *testing.T) {
	s := newStore(t)
	a := newModel(t, s, "anthropic", "sonnet", "", []byte("ka"))
	b := newModel(t, s, "openai", "gpt", "", []byte("kb"))

	// Updating the default carries the new base URL and key over.
	if found, err := s.UpdateLLMModel(ctx, a.ID, "https://proxy.example.com", []byte("ka2"), t0.Add(time.Hour)); err != nil || !found {
		t.Fatal(found, err)
	}
	r, _ := settings(t, s)
	if r.Provider != "anthropic" || r.BaseURL != "https://proxy.example.com" || !bytes.Equal(r.APIKeySealed, []byte("ka2")) ||
		!r.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("llm_settings after updating the default = %+v", r)
	}
	// With no key, the stored key is what goes over.
	if found, err := s.UpdateLLMModel(ctx, a.ID, "", nil, t0.Add(2*time.Hour)); err != nil || !found {
		t.Fatal(found, err)
	}
	if r, _ := settings(t, s); r.BaseURL != "" || !bytes.Equal(r.APIKeySealed, []byte("ka2")) {
		t.Errorf("llm_settings after an update with no key = %+v; want the kept key", r)
	}
	// Updating another entry leaves it alone.
	if found, err := s.UpdateLLMModel(ctx, b.ID, "https://other.example.com", []byte("kb2"), t0.Add(3*time.Hour)); err != nil || !found {
		t.Fatal(found, err)
	}
	if r, _ := settings(t, s); r.Provider != "anthropic" || !bytes.Equal(r.APIKeySealed, []byte("ka2")) || !r.UpdatedAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("llm_settings after updating a non-default = %+v; want it unchanged", r)
	}
}

// An entry repositories and chains name is removed all the same: they keep
// naming it, so the resolver skips it as removed (spec: "skip, and the
// dashboard warns").
func TestDeleteLLMModelInUse(t *testing.T) {
	s := newStore(t)
	newModel(t, s, "anthropic", "sonnet", "", nil)
	b := newModel(t, s, "openai", "gpt", "", nil)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", ModelID: &b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLLMModel(ctx, b.ID, false); err != nil {
		t.Fatalf("DeleteLLMModel of a model a repository names: %v", err)
	}
	if err := s.DeleteLLMModel(ctx, b.ID, false); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("DeleteLLMModel again = %v; want ErrModelNotFound", err)
	}
	if repo, _, _ := s.DashboardRepo(ctx, "lab", "acme/web"); repo.ModelID == nil || *repo.ModelID != b.ID {
		t.Errorf("the repository's model = %v; want it kept", repo.ModelID)
	}
	if c, found := mustChain(t, s, ScopeRepo, 0, "lab", "acme/web"); !found || !slices.Equal(c.Entries, []EntryRef{{SourceGlobal, b.ID}}) {
		t.Errorf("the repository's chain = %+v, %v; want it kept", c, found)
	}
}

func TestDeleteDefaultRefused(t *testing.T) {
	s := newStore(t)
	a := newModel(t, s, "anthropic", "sonnet", "", []byte("ka"))
	b := newModel(t, s, "openai", "gpt", "", nil)
	if err := s.DeleteLLMModel(ctx, a.ID, false); !errors.Is(err, ErrModelIsDefault) {
		t.Fatalf("DeleteLLMModel of the default = %v; want ErrModelIsDefault", err)
	}
	// Even when a repository names it: the default is the answer to give.
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", ModelID: &a.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLLMModel(ctx, a.ID, false); !errors.Is(err, ErrModelIsDefault) {
		t.Fatalf("DeleteLLMModel of a default a repository names = %v; want ErrModelIsDefault", err)
	}
	if len(mustModels(t, s)) != 2 {
		t.Fatal("the default was deleted")
	}
	// Moving the default off makes it an ordinary entry, which goes.
	if found, err := s.SetDefaultLLMModel(ctx, b.ID, t0); err != nil || !found {
		t.Fatal(found, err)
	}
	if err := s.DeleteLLMModel(ctx, a.ID, false); err != nil {
		t.Errorf("DeleteLLMModel of the old default = %v; want it removed", err)
	}
	// llm_settings still follows the default, and the refusals wrote nothing.
	if r, _ := settings(t, s); r.Provider != "openai" {
		t.Errorf("llm_settings = %+v; want the new default", r)
	}
}

// While the config file's llm: block is the default, the flag on a dashboard
// entry is dormant: it may be deleted, and the oldest entry left takes the
// flag, in llm_settings too.
func TestDeleteShadowedDefault(t *testing.T) {
	s := newStore(t)
	// Added an hour apart, so "oldest" is not a tie.
	add := func(provider, model string, key string, hours time.Duration) LLMModel {
		m, err := s.CreateLLMModel(ctx, LLMModel{Provider: provider, Model: model, APIKeySealed: []byte(key)}, t0.Add(hours*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a, b, c := add("anthropic", "sonnet", "ka", 0), add("openai", "gpt", "kb", 1), add("bedrock", "haiku", "kc", 2)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", ModelID: &a.ID}); err != nil {
		t.Fatal(err)
	}

	// Not shadowed, it is still the default and is refused.
	if err := s.DeleteLLMModel(ctx, a.ID, false); !errors.Is(err, ErrModelIsDefault) {
		t.Fatalf("DeleteLLMModel of the default = %v; want ErrModelIsDefault", err)
	}
	if err := s.DeleteLLMModel(ctx, a.ID, true); err != nil {
		t.Fatalf("DeleteLLMModel of a shadowed default: %v", err)
	}
	// The oldest entry left is the default, here and in llm_settings.
	if got := defaultsOf(mustModels(t, s)); !slices.Equal(got, []string{b.ID}) {
		t.Errorf("defaults = %v; want b, the oldest left", got)
	}
	if r, found := settings(t, s); !found || r.Provider != "openai" || string(r.APIKeySealed) != "kb" {
		t.Errorf("llm_settings = %+v, %v; want b written back", r, found)
	}
	// A shadowed entry that is not the flagged one goes without moving it.
	if err := s.DeleteLLMModel(ctx, c.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := defaultsOf(mustModels(t, s)); !slices.Equal(got, []string{b.ID}) {
		t.Errorf("defaults = %v; want b still", got)
	}
	// The last one leaves no default.
	if err := s.DeleteLLMModel(ctx, b.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := mustModels(t, s); len(got) != 0 {
		t.Errorf("models = %+v; want none", got)
	}
}

func mustModels(t *testing.T, s *Store) []LLMModel {
	t.Helper()
	models, err := s.LLMModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return models
}

func TestDeleteLLMModelMissing(t *testing.T) {
	s := newStore(t)
	if err := s.DeleteLLMModel(ctx, "no-such-id", false); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("DeleteLLMModel of a missing entry = %v; want ErrModelNotFound", err)
	}
	// The config entry has no row to delete.
	if err := s.DeleteLLMModel(ctx, ConfigModelID, false); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("DeleteLLMModel(config) = %v; want ErrModelNotFound", err)
	}
}

// A repository's model round-trips through every way to store a repository.
func TestDashboardRepoModelID(t *testing.T) {
	s := newStore(t)
	a := newModel(t, s, "anthropic", "sonnet", "", nil)
	b := newModel(t, s, "openai", "gpt", "", nil)
	get := func(project string) *string {
		t.Helper()
		r, found, err := s.DashboardRepo(ctx, "lab", project)
		if err != nil || !found {
			t.Fatalf("DashboardRepo(%s) = %v, %v", project, found, err)
		}
		return r.ModelID
	}
	same := func(got *string, want string) bool { return got != nil && *got == want }

	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/up", ModelID: &a.ID}); err != nil {
		t.Fatal(err)
	}
	if got := get("acme/up"); !same(got, a.ID) {
		t.Errorf("after Upsert: %v; want a", got)
	}
	created, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/ins", ModelID: &b.ID})
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if got := get("acme/ins"); !same(got, b.ID) {
		t.Errorf("after Insert: %v; want b", got)
	}
	// Update keeps what apply leaves alone, and sets and clears it.
	if found, err := s.UpdateDashboardRepo(ctx, "lab", "acme/ins", func(r *DashboardRepo) { r.Approve = new(bool) }); err != nil || !found {
		t.Fatal(found, err)
	}
	if got := get("acme/ins"); !same(got, b.ID) {
		t.Errorf("after an unrelated Update: %v; want b kept", got)
	}
	if _, err := s.UpdateDashboardRepo(ctx, "lab", "acme/ins", func(r *DashboardRepo) { r.ModelID = &a.ID }); err != nil {
		t.Fatal(err)
	}
	if got := get("acme/ins"); !same(got, a.ID) {
		t.Errorf("after Update to a: %v", got)
	}
	if _, err := s.UpdateDashboardRepo(ctx, "lab", "acme/ins", func(r *DashboardRepo) { r.ModelID = nil }); err != nil {
		t.Fatal(err)
	}
	if got := get("acme/ins"); got != nil {
		t.Errorf("after Update to nil: %v; want follows the default", *got)
	}
	// Upsert replaces it, NULL included.
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/up"}); err != nil {
		t.Fatal(err)
	}
	if got := get("acme/up"); got != nil {
		t.Errorf("after an Upsert with no model: %v; want nil", *got)
	}
	// The config entry has no row and is accepted.
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/cfg", ModelID: strp(ConfigModelID)}); err != nil {
		t.Fatalf("Upsert naming the config entry: %v", err)
	}
	if got := get("acme/cfg"); !same(got, ConfigModelID) {
		t.Errorf("after naming config: %v", got)
	}
}

// A repository naming a model that isn't in the list is not saved, by any of
// the three ways.
func TestSavingARepoNeedsItsModel(t *testing.T) {
	s := newStore(t)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gone", ""} {
		if _, err := s.UpdateDashboardRepo(ctx, "lab", "acme/web", func(r *DashboardRepo) { r.ModelID = &id }); !errors.Is(err, ErrModelNotFound) {
			t.Errorf("UpdateDashboardRepo naming %q = %v; want ErrModelNotFound", id, err)
		}
		if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/new", ModelID: &id}); !errors.Is(err, ErrModelNotFound) {
			t.Errorf("UpsertDashboardRepo naming %q = %v; want ErrModelNotFound", id, err)
		}
		if _, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/new", ModelID: &id}); !errors.Is(err, ErrModelNotFound) {
			t.Errorf("InsertDashboardRepo naming %q = %v; want ErrModelNotFound", id, err)
		}
	}
	if repo, _, _ := s.DashboardRepo(ctx, "lab", "acme/web"); repo.ModelID != nil {
		t.Errorf("model after the refused updates = %q; want nil", *repo.ModelID)
	}
	if _, found, _ := s.DashboardRepo(ctx, "lab", "acme/new"); found {
		t.Error("a refused save created a row")
	}
}

// Saving a repository holds its model's row FOR KEY SHARE until the
// transaction ends, which blocks a delete (FOR UPDATE) and not another save.
func TestSavingARepoLocksItsModel(t *testing.T) {
	s := newStore(t)
	newModel(t, s, "anthropic", "sonnet", "", nil)
	m := newModel(t, s, "openai", "gpt", "", nil)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := lockModel(ctx, tx, &m.ID); err != nil {
		t.Fatal(err)
	}
	if err := lockModel(ctx, tx, strp("gone")); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("lockModel of a missing model = %v; want ErrModelNotFound", err)
	}
	// The failed lookup is a plain SELECT: the transaction is still usable.
	if err := lockModel(ctx, tx, &m.ID); err != nil {
		t.Fatalf("lockModel again: %v", err)
	}

	forUpdate := `SELECT id FROM llm_models WHERE id = $1 FOR UPDATE NOWAIT`
	var id string
	if err := s.pool.QueryRow(ctx, forUpdate, m.ID).Scan(&id); err == nil {
		t.Error("the model row could be locked FOR UPDATE while a save held it")
	}
	keyShare := `SELECT id FROM llm_models WHERE id = $1 FOR KEY SHARE NOWAIT`
	if err := s.pool.QueryRow(ctx, keyShare, m.ID).Scan(&id); err != nil {
		t.Errorf("a second save could not share the lock: %v", err)
	}
	// No model and the config entry take no lock.
	if err := lockModel(ctx, tx, nil); err != nil {
		t.Errorf("lockModel(nil) = %v", err)
	}
	if err := lockModel(ctx, tx, strp(ConfigModelID)); err != nil {
		t.Errorf("lockModel(config) = %v", err)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, forUpdate, m.ID).Scan(&id); err != nil {
		t.Errorf("the lock outlived the transaction: %v", err)
	}
}

// A delete and a save naming the model race: exactly one wins, and a
// repository never ends up pointing at nothing.
// Two entries created together on an empty list: one default, no error.
func TestCreateLLMModelRaceHasOneDefault(t *testing.T) {
	s := newStore(t)
	nils, errs := race(
		func() error { _, err := s.CreateLLMModel(ctx, LLMModel{Provider: "a", Model: "x"}, t0); return err },
		func() error { _, err := s.CreateLLMModel(ctx, LLMModel{Provider: "b", Model: "y"}, t0); return err })
	if nils != 2 {
		t.Fatalf("creates = %v, %v; want both to succeed", errs[0], errs[1])
	}
	models := mustModels(t, s)
	if len(models) != 2 || len(defaultsOf(models)) != 1 {
		t.Fatalf("list = %+v; want two entries and one default", models)
	}
	def := models[0]
	if !def.Default {
		def = models[1]
	}
	if r, _ := settings(t, s); r.Provider != def.Provider {
		t.Errorf("llm_settings = %+v; want the default %s", r, def.Provider)
	}
}

func TestSetRunModel(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "aaa")
	if r := getRun(t, s, id); r.Provider != nil || r.Model != nil {
		t.Fatalf("a new run = provider %v, model %v; want neither", r.Provider, r.Model)
	}
	if err := s.SetRunModel(ctx, id, "anthropic", "sonnet"); err != nil {
		t.Fatalf("SetRunModel of a queued run: %v", err)
	}
	r := getRun(t, s, id)
	if r.Provider == nil || *r.Provider != "anthropic" || r.Model == nil || *r.Model != "sonnet" {
		t.Errorf("queued run = provider %v, model %v", r.Provider, r.Model)
	}

	claimed, err := s.ClaimQueued(ctx, 10, t0)
	if err != nil || len(claimed) != 1 || claimed[0].Provider == nil || *claimed[0].Provider != "anthropic" {
		t.Fatalf("ClaimQueued = %+v, %v; want the run with its provider", claimed, err)
	}
	if err := s.SetRunModel(ctx, id, "openai", "gpt"); err != nil {
		t.Fatalf("SetRunModel of a running run: %v", err)
	}
	if r := getRun(t, s, id); *r.Provider != "openai" || *r.Model != "gpt" {
		t.Errorf("running run = provider %v, model %v; want the latest", *r.Provider, *r.Model)
	}

	tok := "tok-model"
	if err := s.MarkDispatched(ctx, id, "job-"+id, "base", "start", hash(tok), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, tok, []byte(`{"status":"completed"}`), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := s.SetRunModel(ctx, id, "bedrock", "haiku"); err == nil {
		t.Error("SetRunModel of a reported run succeeded")
	}
	if r := getRun(t, s, id); *r.Provider != "openai" {
		t.Errorf("the reported run changed: provider %v", *r.Provider)
	}
	if err := s.SetRunModel(ctx, "bad", "a", "b"); err == nil {
		t.Error("a malformed id was accepted")
	}
}

// Entry names are unique among global entries, ignoring case.
func TestLLMModelNameUnique(t *testing.T) {
	s := newStore(t)
	a := newModel(t, s, "openai", "gpt-5", "", []byte("k1"))
	b := newModel(t, s, "openai", "gpt-5", "https://llm.example.com", []byte("k2"))
	if found, err := s.SetLLMModelMeta(ctx, a.ID, strp("Main key"), nil, t0); err != nil || !found {
		t.Fatalf("SetLLMModelMeta = %v, %v", found, err)
	}
	if _, err := s.SetLLMModelMeta(ctx, b.ID, strp("main KEY"), nil, t0); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a name taken ignoring case = %v; want ErrNameTaken", err)
	}
	// An unnamed entry is read as provider/model, so that name is taken too.
	if _, err := s.SetLLMModelMeta(ctx, a.ID, strp("OpenAI/GPT-5"), nil, t0); !errors.Is(err, ErrNameTaken) {
		t.Errorf("the name an unnamed entry reads as = %v; want ErrNameTaken", err)
	}
	// Its own name again, and teams may use alone, are fine.
	yes := true
	if found, err := s.SetLLMModelMeta(ctx, a.ID, strp("main key"), &yes, t0.Add(time.Hour)); err != nil || !found {
		t.Fatalf("SetLLMModelMeta of its own name = %v, %v", found, err)
	}
	got := modelByID(t, s, a.ID)
	if got.Name != "main key" || !got.TeamsMayUse || !got.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("entry = %+v; want the new name and teams may use", got)
	}
	if found, err := s.SetLLMModelMeta(ctx, "no-such-id", strp("x"), nil, t0); err != nil || found {
		t.Errorf("SetLLMModelMeta of a missing entry = %v, %v; want not found", found, err)
	}
	// A name given at create counts as well.
	if _, err := s.CreateLLMModel(ctx, LLMModel{Name: "MAIN KEY", Provider: "anthropic", Model: "sonnet"}, t0); !errors.Is(err, ErrNameTaken) {
		t.Errorf("CreateLLMModel with a taken name = %v; want ErrNameTaken", err)
	}
	c, err := s.CreateLLMModel(ctx, LLMModel{Name: "Backup", Provider: "anthropic", Model: "sonnet"}, t0)
	if err != nil || c.Name != "Backup" || c.TeamsMayUse {
		t.Errorf("CreateLLMModel with a name = %+v, %v", c, err)
	}
}

// A row the previous release wrote has no name and reads as provider/model.
func TestLLMModelNameNullReadsProviderModel(t *testing.T) {
	s := newStore(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO llm_models (id, provider, model, base_url, is_default, created_at, updated_at)
		VALUES ('old', 'anthropic', 'sonnet', '', true, $1, $1)`, t0); err != nil {
		t.Fatal(err)
	}
	if got := modelByID(t, s, "old"); got.Name != "anthropic/sonnet" || got.TeamsMayUse {
		t.Errorf("entry with no name = %+v; want anthropic/sonnet", got)
	}
	// So does one created without a name.
	m := newModel(t, s, "openai", "gpt-5", "", nil)
	if m.Name != "openai/gpt-5" {
		t.Errorf("created with no name = %+v; want openai/gpt-5", m)
	}
}

// A repository whose model was removed (the removal is no longer refused)
// still saves its other settings: only a new model must be in the list.
func TestSaveRepoAfterItsModelWasRemoved(t *testing.T) {
	s := newStore(t)
	newModel(t, s, "anthropic", "sonnet", "", nil)
	b := newModel(t, s, "openai", "gpt", "", nil)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", ModelID: &b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLLMModel(ctx, b.ID, false); err != nil {
		t.Fatal(err)
	}
	off := false
	if found, err := s.UpdateDashboardRepo(ctx, "lab", "acme/web", func(r *DashboardRepo) { r.Approve = &off }); err != nil || !found {
		t.Fatalf("UpdateDashboardRepo of another setting = %v, %v; want saved", found, err)
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", ModelID: &b.ID}); err != nil {
		t.Fatalf("UpsertDashboardRepo keeping the removed model = %v; want saved", err)
	}
	other := "no-such-model"
	if _, err := s.UpdateDashboardRepo(ctx, "lab", "acme/web", func(r *DashboardRepo) { r.ModelID = &other }); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("a new model not in the list = %v; want ErrModelNotFound", err)
	}
}
