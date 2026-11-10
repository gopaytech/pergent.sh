package instances

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const testDBEnv = "PERGENT_SH_TEST_DATABASE_URL"

var ctx = context.Background()

// newStore opens a migrated store in a fresh schema, dropped when the test
// ends. It skips when no test database is set.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	dbURL := os.Getenv(testDBEnv)
	if dbURL == "" {
		t.Skipf("%s is not set", testDBEnv)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenInSchema(ctx, dbURL, "test_instances_"+hex.EncodeToString(b))
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	t.Cleanup(func() {
		_ = st.DropSchema(context.Background())
		st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// newBox makes a Box on a key made of fill.
func newBox(t *testing.T, fill byte) *secretbox.Box {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = fill
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// addModel adds an entry to the store's list; the first one is the default.
func addModel(t *testing.T, st *store.Store, provider, model, baseURL string, sealedKey []byte, at time.Time) store.LLMModel {
	t.Helper()
	m, err := st.CreateLLMModel(ctx, store.LLMModel{Provider: provider, Model: model, BaseURL: baseURL, APIKeySealed: sealedKey}, at)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func testConfig() *config.Config {
	return &config.Config{
		Dashboard: config.Dashboard{URL: "https://pergent.example.com"},
		GitLab: []config.Instance{
			{Name: "zeta", URL: "https://gitlab.example.com", ReviewTokenEnv: "T_GL_REVIEW", ApproveTokenEnv: "T_GL_APPROVE",
				WebhookSecretEnv: "T_GL_HOOK", OAuthClientIDEnv: "T_GL_OAUTH_ID", OAuthClientSecretEnv: "T_GL_OAUTH_SECRET"},
			{Name: "alpha", URL: "https://gitlab2.example.com", ReviewTokenEnv: "T_GL_REVIEW", ApproveTokenEnv: "T_GL_APPROVE",
				WebhookSecretEnv: "T_GL_HOOK"},
		},
		GitHub: []config.GitHubInstance{
			{Name: "gh", APIURL: "https://api.github.com", ReviewApp: config.App{AppID: 1, PrivateKeyEnv: "T_GH_REVIEW_KEY"},
				ApproveApp: &config.App{AppID: 2, PrivateKeyEnv: "T_GH_APPROVE_KEY"}, WebhookSecretEnv: "T_GH_HOOK"},
		},
	}
}

func addRow(t *testing.T, st *store.Store, box *secretbox.Box, row store.InstanceRow, secrets map[string]string) {
	t.Helper()
	sealed := map[string][]byte{}
	for field, value := range secrets {
		sealed[field] = box.Seal([]byte(value))
	}
	if err := st.CreateInstance(ctx, row, sealed, time.Now()); err != nil {
		t.Fatalf("CreateInstance(%s): %v", row.Name, err)
	}
}

// The config's instances come first, then the browser's, each by name; the
// instance carries what its source knows.
func TestRegistryMergesConfigFirst(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	t.Setenv("T_GL_OAUTH_ID", "gl-client-id")
	addRow(t, st, box, store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, URL: "https://lab.example.com", OAuthClientID: "lab-client"}, nil)
	addRow(t, st, box, store.InstanceRow{Name: "hub", Platform: config.PlatformGitHub, APIURL: "https://api.github.com",
		ReviewAppID: 11, ApproveAppID: 12}, nil)
	addRow(t, st, box, store.InstanceRow{Name: "hub2", Platform: config.PlatformGitHub, APIURL: "https://ghe.example.com/api/v3",
		ReviewAppID: 21}, nil)

	reg := New(testConfig(), st, box)
	if err := reg.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	var names []string
	for _, inst := range reg.All() {
		names = append(names, inst.Name)
	}
	if want := []string{"alpha", "gh", "zeta", "hub", "hub2", "lab"}; !slices.Equal(names, want) {
		t.Errorf("All = %v, want %v", names, want)
	}

	zeta, ok := reg.Get("zeta")
	if !ok || zeta.Source != SourceConfig || zeta.Platform != config.PlatformGitLab || zeta.GitLab == nil ||
		zeta.GitLab.URL != "https://gitlab.example.com" || zeta.OAuthClientID != "gl-client-id" || zeta.Removing {
		t.Errorf("zeta = %+v", zeta)
	}
	lab, ok := reg.Get("lab")
	if !ok || lab.Source != SourceDashboard || lab.Platform != config.PlatformGitLab || lab.GitLab == nil ||
		lab.GitLab.URL != "https://lab.example.com" || lab.GitLab.ReviewTokenEnv != "" || lab.OAuthClientID != "lab-client" {
		t.Errorf("lab = %+v", lab)
	}
	hub, _ := reg.Get("hub")
	if hub.Platform != config.PlatformGitHub || hub.GitHub == nil || hub.GitHub.ReviewApp.AppID != 11 ||
		hub.GitHub.ApproveApp == nil || hub.GitHub.ApproveApp.AppID != 12 || hub.GitHub.APIURL != "https://api.github.com" {
		t.Errorf("hub = %+v", hub)
	}
	hub2, _ := reg.Get("hub2")
	if hub2.GitHub == nil || hub2.GitHub.ApproveApp != nil {
		t.Errorf("hub2 approves with an App, want a token: %+v", hub2.GitHub)
	}
	if p, ok := reg.Platform("hub2"); !ok || p != config.PlatformGitHub {
		t.Errorf("Platform(hub2) = %q, %v", p, ok)
	}
	if p, ok := reg.Platform("alpha"); !ok || p != config.PlatformGitLab {
		t.Errorf("Platform(alpha) = %q, %v", p, ok)
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("Get(nope) found an instance")
	}
	if _, ok := reg.Platform("nope"); ok {
		t.Error("Platform(nope) found an instance")
	}

	// Put and Remove follow a store write.
	reg.Put(Instance{Name: "new", Platform: config.PlatformGitLab, Source: SourceDashboard, GitLab: &config.Instance{Name: "new"}})
	if _, ok := reg.Get("new"); !ok {
		t.Error("Put instance not found")
	}
	reg.Remove("new")
	reg.Remove("never-there")
	if _, ok := reg.Get("new"); ok {
		t.Error("removed instance still found")
	}
	reg.Put(Instance{Name: "lab", Platform: config.PlatformGitLab, Source: SourceDashboard, Removing: true, GitLab: &config.Instance{Name: "lab"}})
	if got, _ := reg.Get("lab"); !got.Removing {
		t.Error("Put didn't replace lab")
	}
}

// A config instance and a browser instance of one name stop startup, naming
// the instance and both places.
func TestRegistryClash(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	addRow(t, st, box, store.InstanceRow{Name: "gh", Platform: config.PlatformGitLab, URL: "https://x.example.com"}, nil)
	err := New(testConfig(), st, box).Load(ctx)
	if err == nil {
		t.Fatal("Load accepted a clash")
	}
	if want := `instance "gh" is in both the config file and the dashboard`; err.Error() != want {
		t.Errorf("Load = %q, want %q", err, want)
	}
}

// Without a store only the config's instances are there.
func TestRegistryConfigOnly(t *testing.T) {
	reg := New(testConfig(), nil, nil)
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(reg.All()); got != 3 {
		t.Errorf("All = %d instances, want 3", got)
	}
}

// A config instance reads its secrets from the environment, a browser one
// opens them from the store.
func TestSecretsFromEnvAndStore(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	t.Setenv("T_GL_REVIEW", "glpat-review")
	t.Setenv("T_GL_APPROVE", "glpat-approve")
	t.Setenv("T_GL_HOOK", "hook")
	t.Setenv("T_GL_OAUTH_ID", "oauth-id")
	t.Setenv("T_GL_OAUTH_SECRET", "oauth-secret")
	t.Setenv("T_GH_REVIEW_KEY", "review-key")
	t.Setenv("T_GH_APPROVE_KEY", "approve-key")
	t.Setenv("T_GH_HOOK", "gh-hook")
	addRow(t, st, box, store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, URL: "https://lab.example.com", OAuthClientID: "c"},
		map[string]string{FieldReviewToken: "r", FieldApproveToken: "a", FieldWebhookSecret: "w", FieldOAuthClientSecret: "o"})
	addRow(t, st, box, store.InstanceRow{Name: "hub", Platform: config.PlatformGitHub, APIURL: "https://api.github.com", ReviewAppID: 1, ApproveAppID: 2},
		map[string]string{FieldReviewAppKey: "rk", FieldApproveAppKey: "ak", FieldWebhookSecret: "hw"})
	reg := New(testConfig(), st, box)
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]Secrets{
		"zeta": {ReviewToken: "glpat-review", ApproveToken: "glpat-approve", WebhookSecret: "hook", OAuthClientSecret: "oauth-secret"},
		"gh":   {ReviewAppKey: "review-key", ApproveAppKey: "approve-key", WebhookSecret: "gh-hook"},
		"lab":  {ReviewToken: "r", ApproveToken: "a", WebhookSecret: "w", OAuthClientSecret: "o"},
		"hub":  {ReviewAppKey: "rk", ApproveAppKey: "ak", WebhookSecret: "hw"},
	} {
		got, unopened, err := reg.Secrets(ctx, name)
		if err != nil || len(unopened) != 0 || got != want {
			t.Errorf("Secrets(%s) = %+v, %v, %v; want %+v", name, got, unopened, err, want)
		}
	}
	if got := (Secrets{ReviewToken: "r", WebhookSecret: "w"}).Fields(); len(got) != 2 || got[FieldReviewToken] != "r" || got[FieldWebhookSecret] != "w" {
		t.Errorf("Fields = %v", got)
	}

	// A token approver instance reads its approve token, not an App key.
	cfg := testConfig()
	cfg.GitHub[0].ApproveApp, cfg.GitHub[0].ApproveTokenEnv = nil, "T_GH_APPROVE_TOKEN"
	t.Setenv("T_GH_APPROVE_TOKEN", "pat")
	got, _, err := New(cfg, nil, nil).Secrets(ctx, "gh")
	if err != nil || got.ApproveToken != "pat" || got.ApproveAppKey != "" {
		t.Errorf("Secrets(gh, token approver) = %+v, %v", got, err)
	}

	// An unset variable is an error naming it; a missing instance too.
	t.Setenv("T_GL_HOOK", "")
	if _, _, err := reg.Secrets(ctx, "zeta"); err == nil || !strings.Contains(err.Error(), "T_GL_HOOK") {
		t.Errorf("Secrets with T_GL_HOOK unset = %v", err)
	}
	if _, _, err := reg.Secrets(ctx, "nope"); err == nil {
		t.Error("Secrets of an unknown instance succeeded")
	}
}

// OAuth env vars are read only when the dashboard is on.
func TestSecretsOAuthOnlyWithDashboard(t *testing.T) {
	cfg := testConfig()
	t.Setenv("T_GL_REVIEW", "r")
	t.Setenv("T_GL_APPROVE", "a")
	t.Setenv("T_GL_HOOK", "h")
	// T_GL_OAUTH_* unset.
	if _, _, err := New(cfg, nil, nil).Secrets(ctx, "zeta"); err == nil || !strings.Contains(err.Error(), "T_GL_OAUTH_ID") {
		t.Errorf("Secrets with the OAuth client unset = %v, want it named", err)
	}
	cfg.Dashboard.URL = ""
	if _, _, err := New(cfg, nil, nil).Secrets(ctx, "zeta"); err != nil {
		t.Errorf("Secrets with the dashboard off = %v", err)
	}
}

// A value sealed with another key can't be opened: it is reported by field
// ("not set"), not as an error, and the others still come back.
func TestSecretsUnopenable(t *testing.T) {
	st := newStore(t)
	other := newBox(t, 9)
	box := newBox(t, 1)
	sealed := map[string][]byte{
		FieldReviewToken:   box.Seal([]byte("good")),
		FieldApproveToken:  other.Seal([]byte("lost")),
		FieldWebhookSecret: []byte("junk"),
	}
	if err := st.CreateInstance(ctx, store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, URL: "https://lab.example.com"}, sealed, time.Now()); err != nil {
		t.Fatal(err)
	}
	reg := New(&config.Config{}, st, box)
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	got, unopened, err := reg.Secrets(ctx, "lab")
	if err != nil {
		t.Fatalf("Secrets = %v, want no error", err)
	}
	if got.ReviewToken != "good" || got.ApproveToken != "" || got.WebhookSecret != "" {
		t.Errorf("Secrets = %+v", got)
	}
	slices.Sort(unopened)
	if want := []string{FieldApproveToken, FieldWebhookSecret}; !slices.Equal(unopened, want) {
		t.Errorf("unopened = %v, want %v", unopened, want)
	}
}

// The config's llm: block wins and its key is read from the env var it names;
// without the block the LLM is the store's row, its key opened with the box.
func TestLLMFromConfigOrStore(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	cfg := testConfig()

	r := New(cfg, st, box)
	if _, _, _, ok := r.LLM(); ok {
		t.Fatal("LLM() is ok with neither a config block nor a stored row")
	}
	if got := r.LLMStatus(); got.Source != SourceDashboard || got.KeySet || got.LLM.Provider != "" {
		t.Errorf("LLMStatus with nothing set = %+v", got)
	}

	// A row without a key is not enough.
	first := addModel(t, st, "anthropic", "claude-sonnet-4", "", nil, time.Now())
	if err := r.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := r.LLM(); ok {
		t.Error("LLM() is ok for a row without a key")
	}
	if got := r.LLMStatus(); got.LLM.Model != "claude-sonnet-4" || got.KeySet {
		t.Errorf("LLMStatus for a row without a key = %+v", got)
	}

	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	if found, err := st.UpdateLLMModel(ctx, first.ID, "https://llm.example.com/v1", box.Seal([]byte("key-one")), at); err != nil || !found {
		t.Fatalf("UpdateLLMModel = %v, %v", found, err)
	}
	if err := r.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}
	llm, key, src, ok := r.LLM()
	if !ok || src != SourceDashboard || key != "key-one" ||
		llm != (config.LLM{Provider: "anthropic", Model: "claude-sonnet-4", BaseURL: "https://llm.example.com/v1"}) {
		t.Fatalf("LLM() = %+v, %q, %s, %v", llm, key, src, ok)
	}
	if got := r.LLMStatus(); !got.KeySet || !got.UpdatedAt.Equal(at) || got.Source != SourceDashboard {
		t.Errorf("LLMStatus = %+v", got)
	}

	// A change is picked up by LoadLLM; every key seen is remembered.
	second := addModel(t, st, "openai", "gpt", "", box.Seal([]byte("key-two")), at)
	if found, err := st.SetDefaultLLMModel(ctx, second.ID, at); err != nil || !found {
		t.Fatalf("SetDefaultLLMModel = %v, %v", found, err)
	}
	if _, key, _, _ := r.LLM(); key != "key-one" {
		t.Errorf("key before LoadLLM = %q, want the loaded one", key)
	}
	if err := r.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}
	if llm, key, _, _ := r.LLM(); key != "key-two" || llm.Provider != "openai" {
		t.Errorf("after LoadLLM = %+v, %q", llm, key)
	}
	if got := r.LLMKeys(); !slices.Equal(got, []string{"key-one", "key-two"}) {
		t.Errorf("LLMKeys = %v, want both keys", got)
	}

	// The config's block wins over the row, and its key is the env var's.
	cfg.LLM = config.LLM{Provider: "anthropic", Model: "from-config", APIKeyEnv: "T_LLM_KEY"}
	t.Setenv("T_LLM_KEY", "key-env")
	llm, key, src, ok = r.LLM()
	if !ok || src != SourceConfig || key != "key-env" || llm.Model != "from-config" {
		t.Fatalf("LLM() with a config block = %+v, %q, %s, %v", llm, key, src, ok)
	}
	if got := r.LLMStatus(); got.Source != SourceConfig || !got.KeySet || !got.UpdatedAt.IsZero() {
		t.Errorf("LLMStatus with a config block = %+v", got)
	}
	if got := r.LLMKeys(); !slices.Equal(got, []string{"key-env", "key-one", "key-two"}) {
		t.Errorf("LLMKeys = %v", got)
	}
	t.Setenv("T_LLM_KEY", "")
	if _, _, _, ok := r.LLM(); ok {
		t.Error("LLM() is ok with the config's key variable empty")
	}
	if got := r.LLMStatus(); got.KeySet {
		t.Errorf("LLMStatus says the key is set with the variable empty: %+v", got)
	}
}

// A stored key that can't be opened (another secret key) counts as not set.
func TestLLMKeyUnopenable(t *testing.T) {
	st := newStore(t)
	other := newBox(t, 2)
	addModel(t, st, "anthropic", "m", "", other.Seal([]byte("plain-llm-value-9")), time.Now())
	r := New(testConfig(), st, newBox(t, 1))
	var logs strings.Builder
	r.SetLog(slog.New(slog.NewTextHandler(&logs, nil)))
	if err := r.LoadLLM(ctx); err != nil {
		t.Fatalf("LoadLLM = %v, want the unopenable key reported as not set", err)
	}
	// The log names the field, at Warn, and never the key.
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "field=api_key") || strings.Contains(out, "plain-llm-value-9") {
		t.Errorf("log = %q; want a Warn naming the api_key field", out)
	}
	if _, _, _, ok := r.LLM(); ok {
		t.Error("LLM() is ok with a key sealed with another secret key")
	}
	if got := r.LLMStatus(); got.KeySet || got.LLM.Model != "m" {
		t.Errorf("LLMStatus = %+v", got)
	}
	if len(r.LLMKeys()) != 0 {
		t.Errorf("LLMKeys = %v, want none", r.LLMKeys())
	}
}

// Marking an instance removing and changing its client ID are done in place,
// so a change that races a removal can never put Removing back to false.
func TestRegistryChangesInPlace(t *testing.T) {
	r := New(testConfig(), nil, nil)
	r.Put(Instance{Name: "lab", Platform: config.PlatformGitLab, Source: SourceDashboard, GitLab: &config.Instance{Name: "lab"}, OAuthClientID: "c1"})

	inst, ok := r.SetOAuthClientID("lab", "c2")
	if !ok || inst.OAuthClientID != "c2" || inst.Removing {
		t.Fatalf("SetOAuthClientID = %+v, %v", inst, ok)
	}
	inst, ok = r.SetRemoving("lab")
	if !ok || !inst.Removing || inst.OAuthClientID != "c2" {
		t.Fatalf("SetRemoving = %+v, %v", inst, ok)
	}
	inst, ok = r.SetOAuthClientID("lab", "c3")
	if !ok || !inst.Removing || inst.OAuthClientID != "c3" {
		t.Fatalf("SetOAuthClientID after SetRemoving = %+v, %v; want still removing", inst, ok)
	}
	if got, _ := r.Get("lab"); !got.Removing {
		t.Error("the registry lost Removing")
	}
	for _, name := range []string{"nope", "zeta"} { // unknown, and the config's
		if _, ok := r.SetRemoving(name); ok {
			t.Errorf("SetRemoving(%q) = true", name)
		}
		if _, ok := r.SetOAuthClientID(name, "x"); ok {
			t.Errorf("SetOAuthClientID(%q) = true", name)
		}
	}
}

// LLM() reads the model and the key it returns together: a LoadLLM running
// at the same time can't pair the old model with the new key.
func TestLLMReadsModelAndKeyTogether(t *testing.T) {
	r := New(testConfig(), nil, nil)
	set := func(n string) {
		r.llmMu.Lock()
		r.llm = []llmEntry{{Model: Model{ID: "id", LLM: config.LLM{Provider: "p", Model: "model-" + n}, Key: "key-" + n, Default: true, Source: SourceDashboard}}}
		r.llmMu.Unlock()
	}
	set("a")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200000; i++ {
			set([]string{"a", "b"}[i%2])
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		llm, key, _, ok := r.LLM()
		if ok && strings.TrimPrefix(llm.Model, "model-") != strings.TrimPrefix(key, "key-") {
			t.Fatalf("LLM() paired %q with %q", llm.Model, key)
		}
	}
}

// With an llm: block in the config and a nil store the list is that block,
// read-only and the default.
func TestModelsFromConfigIsReadOnlyDefault(t *testing.T) {
	cfg := testConfig()
	cfg.LLM = config.LLM{Provider: "anthropic", Model: "from-config", APIKeyEnv: "T_LLM_KEY"}
	t.Setenv("T_LLM_KEY", "key-env")
	r := New(cfg, nil, nil)

	models := r.Models()
	if len(models) != 1 {
		t.Fatalf("Models() = %+v, want one entry", models)
	}
	if m := models[0]; m.ID != ConfigModelID || m.Source != SourceConfig || !m.Default || m.Key != "key-env" || m.LLM.Model != "from-config" {
		t.Errorf("Models()[0] = %+v", m)
	}
	if m, ok := r.DefaultModel(); !ok || m.ID != ConfigModelID {
		t.Errorf("DefaultModel() = %+v, %v", m, ok)
	}
	if m, ok := r.ModelByID(ConfigModelID); !ok || m.Key != "key-env" {
		t.Errorf("ModelByID(config) = %+v, %v", m, ok)
	}

	// The entry stays in the list without a key, but is not usable.
	t.Setenv("T_LLM_KEY", "")
	if got := r.Models(); len(got) != 1 || got[0].Key != "" {
		t.Errorf("Models() with the key variable empty = %+v", got)
	}
	if _, ok := r.DefaultModel(); ok {
		t.Error("DefaultModel() is ok with the config's key variable empty")
	}
	if _, ok := r.ModelByID(ConfigModelID); ok {
		t.Error("ModelByID(config) is ok with the config's key variable empty")
	}
}

func TestModelByIDMissing(t *testing.T) {
	r := New(testConfig(), nil, nil) // no llm: block, no store
	for _, id := range []string{"", "nope", ConfigModelID} {
		if m, ok := r.ModelByID(id); ok {
			t.Errorf("ModelByID(%q) = %+v, true; want not ok", id, m)
		}
	}
	if len(r.Models()) != 0 {
		t.Errorf("Models() = %+v, want none", r.Models())
	}
	if _, ok := r.DefaultModel(); ok {
		t.Error("DefaultModel() is ok with nothing set")
	}
}

// The list holds the config's entry first, then the store's; the store's
// default is the default only while the config has no llm: block.
func TestModelsListsConfigThenStore(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	a := addModel(t, st, "anthropic", "a", "", box.Seal([]byte("key-a")), time.Now())
	b := addModel(t, st, "openai", "b", "", nil, time.Now().Add(time.Second))
	cfg := testConfig()
	r := New(cfg, st, box)
	if err := r.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}

	got := r.Models()
	if len(got) != 2 || got[0].ID != a.ID || !got[0].Default || got[1].ID != b.ID || got[1].Default {
		t.Fatalf("Models() = %+v", got)
	}
	if m, ok := r.DefaultModel(); !ok || m.ID != a.ID || m.Key != "key-a" || m.Source != SourceDashboard {
		t.Errorf("DefaultModel() = %+v, %v", m, ok)
	}
	if _, ok := r.ModelByID(b.ID); ok {
		t.Error("ModelByID is ok for an entry without a key")
	}

	cfg.LLM = config.LLM{Provider: "anthropic", Model: "from-config", APIKeyEnv: "T_LLM_KEY"}
	t.Setenv("T_LLM_KEY", "key-env")
	got = r.Models()
	if len(got) != 3 || got[0].ID != ConfigModelID || !got[0].Default || got[1].Default || got[2].Default {
		t.Fatalf("Models() with a config block = %+v", got)
	}
	if m, ok := r.DefaultModel(); !ok || m.ID != ConfigModelID {
		t.Errorf("DefaultModel() with a config block = %+v, %v", m, ok)
	}
	if m, ok := r.ModelByID(a.ID); !ok || m.Key != "key-a" || m.Default {
		t.Errorf("ModelByID(a) with a config block = %+v, %v", m, ok)
	}
}

// Every entry's key, and a key replaced since start, is remembered and
// hidden by Redact.
func TestLLMKeysCoverEveryEntry(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	a := addModel(t, st, "anthropic", "a", "", box.Seal([]byte("key-a-secret")), time.Now())
	addModel(t, st, "openai", "b", "", box.Seal([]byte("key-b-secret")), time.Now())
	r := New(testConfig(), st, box)
	if err := r.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}
	if found, err := st.UpdateLLMModel(ctx, a.ID, "", box.Seal([]byte("key-a-newer")), time.Now()); err != nil || !found {
		t.Fatalf("UpdateLLMModel = %v, %v", found, err)
	}
	if err := r.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}

	want := []string{"key-a-newer", "key-a-secret", "key-b-secret"}
	if got := r.LLMKeys(); !slices.Equal(got, want) {
		t.Fatalf("LLMKeys = %v, want %v", got, want)
	}
	text := "x key-a-secret y key-a-newer z key-b-secret"
	if got := r.Redact(text); got != "x *** y *** z ***" {
		t.Errorf("Redact = %q", got)
	}
	held, err := r.HeldSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range want {
		if !slices.Contains(held, k) {
			t.Errorf("HeldSecrets lacks %q", k)
		}
	}
}

// A key that is the start of another is replaced after it, so the tail of the
// longer one doesn't stay in the text. This needs no database.
func TestRedactLongestKeyFirst(t *testing.T) {
	r := New(testConfig(), nil, nil)
	r.noteKey("sk-abc")
	r.noteKey("sk-abcdef-tail")
	if got := r.Redact("a sk-abcdef-tail b sk-abc c"); got != "a *** b *** c" {
		t.Errorf("Redact = %q, want both keys replaced whole", got)
	}
}

// An entry whose key is sealed under another secret key warns and is not
// usable, while the entries before and after it still are.
func TestUnopenableKeyIsNotUsable(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	good := addModel(t, st, "anthropic", "good", "", box.Seal([]byte("key-good")), time.Now())
	bad := addModel(t, st, "openai", "bad", "", newBox(t, 2).Seal([]byte("key-bad-secret")), time.Now().Add(time.Second))
	r := New(testConfig(), st, box)
	var logs strings.Builder
	r.SetLog(slog.New(slog.NewTextHandler(&logs, nil)))
	if err := r.LoadLLM(ctx); err != nil {
		t.Fatalf("LoadLLM = %v, want the unopenable key reported as not set", err)
	}
	if _, ok := r.ModelByID(bad.ID); ok {
		t.Error("ModelByID is ok for a key sealed with another secret key")
	}
	if m, ok := r.ModelByID(good.ID); !ok || m.Key != "key-good" {
		t.Errorf("ModelByID(good) = %+v, %v", m, ok)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "field=api_key") || strings.Contains(out, "key-bad-secret") {
		t.Errorf("log = %q; want a Warn naming the api_key field", out)
	}
	if got := r.LLMKeys(); !slices.Equal(got, []string{"key-good"}) {
		t.Errorf("LLMKeys = %v, want only the one that opened", got)
	}
}

// HeldSecrets lists every secret of every instance, the config's and the
// dashboard's, GitLab and GitHub, App and token approvers, and every LLM key.
func TestHeldSecretsCoversEveryField(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	t.Setenv("T_GL_REVIEW", "cfg-gl-review-token")
	t.Setenv("T_GL_APPROVE", "cfg-gl-approve-token")
	t.Setenv("T_GL_HOOK", "cfg-gl-hook-secret")
	t.Setenv("T_GL_OAUTH_ID", "oauth-id")
	t.Setenv("T_GL_OAUTH_SECRET", "cfg-gl-oauth-secret")
	t.Setenv("T_GH_REVIEW_KEY", "cfg-gh-review-key")
	t.Setenv("T_GH_APPROVE_KEY", "cfg-gh-approve-key")
	t.Setenv("T_GH_HOOK", "cfg-gh-hook-secret")
	addRow(t, st, box, store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, URL: "https://lab.example.com", OAuthClientID: "c"},
		map[string]string{FieldReviewToken: "lab-review-token", FieldApproveToken: "lab-approve-token",
			FieldWebhookSecret: "lab-hook-secret", FieldOAuthClientSecret: "lab-oauth-secret"})
	addRow(t, st, box, store.InstanceRow{Name: "hub", Platform: config.PlatformGitHub, APIURL: "https://api.github.com", ReviewAppID: 1, ApproveAppID: 2},
		map[string]string{FieldReviewAppKey: "hub-review-key", FieldApproveAppKey: "hub-approve-key", FieldWebhookSecret: "hub-hook-secret"})
	addRow(t, st, box, store.InstanceRow{Name: "hub2", Platform: config.PlatformGitHub, APIURL: "https://ghe.example.com/api/v3", ReviewAppID: 3},
		map[string]string{FieldReviewAppKey: "hub2-review-key", FieldApproveToken: "hub2-approve-pat", FieldWebhookSecret: "hub2-hook-secret"})
	llmModel := addModel(t, st, "anthropic", "claude-sonnet-4", "", box.Seal([]byte("llm-key-one")), time.Now())

	reg := New(testConfig(), st, box)
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reg.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}
	// The alpha instance reads the same env vars as zeta, minus the OAuth
	// client, so it adds no value of its own.
	got, err := reg.HeldSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"cfg-gl-review-token", "cfg-gl-approve-token", "cfg-gl-hook-secret", "cfg-gl-oauth-secret",
		"cfg-gh-review-key", "cfg-gh-approve-key", "cfg-gh-hook-secret",
		"lab-review-token", "lab-approve-token", "lab-hook-secret", "lab-oauth-secret",
		"hub-review-key", "hub-approve-key", "hub-hook-secret",
		"hub2-review-key", "hub2-approve-pat", "hub2-hook-secret",
		"llm-key-one",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("HeldSecrets lacks %q; got %v", want, got)
		}
	}
	if slices.Contains(got, "") {
		t.Error("HeldSecrets holds an empty value, which would match every skill")
	}

	// A key the LLM held earlier still counts.
	if found, err := st.UpdateLLMModel(ctx, llmModel.ID, "", box.Seal([]byte("llm-key-two")), time.Now()); err != nil || !found {
		t.Fatalf("UpdateLLMModel = %v, %v", found, err)
	}
	if err := reg.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := reg.HeldSecrets(ctx); err != nil || !slices.Contains(got, "llm-key-one") || !slices.Contains(got, "llm-key-two") {
		t.Errorf("HeldSecrets after the key changed = %v, %v; want both keys", got, err)
	}
}

// A config instance's env var that is unset holds nothing and hides none of
// the others.
func TestHeldSecretsConfigEnvOneByOne(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	t.Setenv("T_GL_REVIEW", "cfg-gl-review-token") // the other variables are unset
	addRow(t, st, box, store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, URL: "https://lab.example.com"},
		map[string]string{FieldReviewToken: "lab-review-token"})
	reg := New(testConfig(), st, box)
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := reg.HeldSecrets(ctx)
	if err != nil || !slices.Contains(got, "lab-review-token") || !slices.Contains(got, "cfg-gl-review-token") {
		t.Errorf("HeldSecrets = %v, %v; want lab's token and the one env var that is set", got, err)
	}
}

// The check fails closed: a dashboard instance's secrets that can't be read
// or opened are an error, never a shorter list.
func TestHeldSecretsFailClosed(t *testing.T) {
	t.Run("a secret that can't be opened", func(t *testing.T) {
		st := newStore(t)
		addRow(t, st, newBox(t, 1), store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, URL: "https://lab.example.com"},
			map[string]string{FieldReviewToken: "lab-review-token"})
		reg := New(testConfig(), st, newBox(t, 2)) // another key
		if err := reg.Load(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := reg.HeldSecrets(ctx)
		if err == nil {
			t.Fatalf("HeldSecrets = %v, nil; want an error", got)
		}
		if strings.Contains(err.Error(), "lab-review-token") {
			t.Errorf("the error holds a secret: %v", err)
		}
	})
	t.Run("the store can't be read", func(t *testing.T) {
		st := newStore(t)
		box := newBox(t, 1)
		addRow(t, st, box, store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, URL: "https://lab.example.com"},
			map[string]string{FieldReviewToken: "lab-review-token"})
		reg := New(testConfig(), st, box)
		if err := reg.Load(ctx); err != nil {
			t.Fatal(err)
		}
		st.Close()
		if got, err := reg.HeldSecrets(ctx); err == nil {
			t.Errorf("HeldSecrets = %v, nil with the store closed; want an error", got)
		}
	})
}

// The secret key that seals the stored secrets and the password in the
// database URL are held too: a skill that holds either is refused. A URL
// without a password, or none at all, adds nothing.
func TestHeldSecretsHoldsSecretKeyAndDatabasePassword(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	reg := New(testConfig(), st, box)
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PERGENT_SH_SECRET_KEY", "test-seal-key-value")
	t.Setenv("DATABASE_URL", "postgres://dbuser:p%40ss-test@localhost:5432/pergent?sslmode=disable")
	got, err := reg.HeldSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"test-seal-key-value", "p@ss-test", "p%40ss-test"} {
		if !slices.Contains(got, want) {
			t.Errorf("HeldSecrets lacks %q; got %v", want, got)
		}
	}
	for _, v := range got {
		if v == "dbuser" || strings.Contains(v, "localhost") {
			t.Errorf("HeldSecrets holds %q; want the password only", v)
		}
	}

	for name, url := range map[string]string{"no password": "postgres://dbuser@localhost:5432/pergent", "unset": "", "not a URL": "://%zz"} {
		t.Setenv("DATABASE_URL", url)
		got, err := reg.HeldSecrets(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if slices.Contains(got, "") || slices.Contains(got, "dbuser") || !slices.Contains(got, "test-seal-key-value") {
			t.Errorf("%s: HeldSecrets = %v", name, got)
		}
	}
}

// A row whose approve App is its review App is the review App approving:
// no second App, no approve secret. 0 stays a token, another ID an App.
// A GitLab row says whether its review bot approves.
func TestFromRowReviewBotApprover(t *testing.T) {
	for _, on := range []bool{false, true} {
		inst := FromRow(store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, URL: "https://gitlab.example.com", ApproveWithReviewBot: on})
		if inst.GitLab == nil || inst.GitLab.ApproveWithReviewBot != on {
			t.Errorf("row with %v: GitLab = %+v", on, inst.GitLab)
		}
	}
}

func TestFromRowApprover(t *testing.T) {
	tests := []struct {
		name         string
		row          store.InstanceRow
		want         string
		wantApproveA bool
	}{
		{"review App approves", store.InstanceRow{ReviewAppID: 7, ApproveAppID: 7}, config.ApproverReviewApp, false},
		{"separate approve App", store.InstanceRow{ReviewAppID: 7, ApproveAppID: 8}, config.ApproverApp, true},
		{"token", store.InstanceRow{ReviewAppID: 7}, config.ApproverToken, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.row.Name, tt.row.Platform, tt.row.APIURL = "hub", config.PlatformGitHub, "https://api.github.com"
			gh := FromRow(tt.row).GitHub
			if got := gh.Approver(); got != tt.want {
				t.Errorf("Approver() = %q, want %q", got, tt.want)
			}
			if (gh.ApproveApp != nil) != tt.wantApproveA {
				t.Errorf("ApproveApp = %+v", gh.ApproveApp)
			}
			if tt.want == config.ApproverReviewApp && !gh.ApproveWithReviewApp {
				t.Error("ApproveWithReviewApp not set")
			}
		})
	}
}

// A review App that approves holds no approve secret, in the config or the
// dashboard, so the approver adds nothing to Secrets or HeldSecrets.
func TestReviewAppApproverHoldsNoApproveSecret(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	t.Setenv("T_GH_REVIEW_KEY", "cfg-gh-review-key")
	t.Setenv("T_GH_HOOK", "cfg-gh-hook-secret")
	t.Setenv("T_GH_APPROVE_KEY", "")
	cfg := testConfig()
	cfg.GitHub[0].ApproveApp, cfg.GitHub[0].ApproveWithReviewApp = nil, true
	addRow(t, st, box, store.InstanceRow{Name: "hub", Platform: config.PlatformGitHub, APIURL: "https://api.github.com", ReviewAppID: 5, ApproveAppID: 5},
		map[string]string{FieldReviewAppKey: "hub-review-key", FieldWebhookSecret: "hub-hook-secret"})
	reg := New(cfg, st, box)
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]Secrets{
		"gh":  {ReviewAppKey: "cfg-gh-review-key", WebhookSecret: "cfg-gh-hook-secret"},
		"hub": {ReviewAppKey: "hub-review-key", WebhookSecret: "hub-hook-secret"},
	} {
		got, unopened, err := reg.Secrets(ctx, name)
		if err != nil || len(unopened) != 0 || got != want {
			t.Errorf("Secrets(%s) = %+v, %v, %v; want %+v", name, got, unopened, err, want)
		}
	}
	held, err := reg.HeldSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(held, "cfg-gh-review-key") || !slices.Contains(held, "hub-review-key") || slices.Contains(held, "") {
		t.Errorf("HeldSecrets = %v", held)
	}
}

// The OIDC client secret is held: the one the config names by env var (no
// database needed), and the one stored sealed from the dashboard.
func TestHeldSecretsHoldsTheSignInClientSecret(t *testing.T) {
	t.Setenv("T_SIGNIN_SECRET", "oidc-client-secret-from-env")
	cfg := &config.Config{Dashboard: config.Dashboard{SignIn: config.SignIn{Mode: config.SignInOIDC, ClientSecretEnv: "T_SIGNIN_SECRET"}}}
	got, err := New(cfg, nil, nil).HeldSecrets(ctx)
	if err != nil || !slices.Contains(got, "oidc-client-secret-from-env") {
		t.Errorf("HeldSecrets = %v, %v; want the config's client secret", got, err)
	}
	t.Setenv("T_SIGNIN_SECRET", "")
	if got, err := New(cfg, nil, nil).HeldSecrets(ctx); err != nil || slices.Contains(got, "") {
		t.Errorf("HeldSecrets with the env var unset = %v, %v; want no empty entry", got, err)
	}
}

// [CI] The stored client secret is held; one that can't be opened fails the
// check closed, and the error holds no secret.
func TestHeldSecretsHoldsTheStoredSignInClientSecret(t *testing.T) {
	st := newStore(t)
	box := newBox(t, 1)
	if err := st.PutSignInSettings(ctx, store.SignInSettings{Mode: store.SignInModeOIDC, Issuer: "https://idp.example.com",
		ClientID: "c", ClientSecretSealed: box.Seal([]byte("stored-oidc-client-secret"))}); err != nil {
		t.Fatal(err)
	}
	reg := New(testConfig(), st, box)
	if err := reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := reg.HeldSecrets(ctx)
	if err != nil || !slices.Contains(got, "stored-oidc-client-secret") {
		t.Errorf("HeldSecrets = %v, %v; want the stored client secret", got, err)
	}
	other := New(testConfig(), st, newBox(t, 2)) // another key
	if err := other.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := other.HeldSecrets(ctx); err == nil || strings.Contains(err.Error(), "stored-oidc-client-secret") {
		t.Errorf("HeldSecrets with the wrong key = %v, %v; want an error without the secret", got, err)
	}
}

// Every instance says how it trusts authors; unset in the config is
// follow_visibility.
func TestInstanceTrustMode(t *testing.T) {
	if got := FromRow(store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, TrustedAuthorsOnly: "off"}).TrustedAuthorsOnly; got != "off" {
		t.Errorf("dashboard instance = %q, want off", got)
	}
	if got := FromRow(store.InstanceRow{Name: "lab", Platform: config.PlatformGitHub}).TrustedAuthorsOnly; got != config.TrustFollowVisibility {
		t.Errorf("dashboard instance with no value = %q, want follow_visibility", got)
	}
	cfg := &config.Config{
		GitLab: []config.Instance{{Name: "main"}, {Name: "closed", TrustedAuthorsOnly: "on"}},
		GitHub: []config.GitHubInstance{{Name: "gh"}},
	}
	r := New(cfg, nil, nil)
	for name, want := range map[string]string{"main": "follow_visibility", "closed": "on", "gh": "follow_visibility"} {
		if inst, _ := r.Get(name); inst.TrustedAuthorsOnly != want {
			t.Errorf("%s = %q, want %q", name, inst.TrustedAuthorsOnly, want)
		}
	}
}

// Every instance carries its monthly token quota, from the config or the row.
func TestInstanceTokenQuota(t *testing.T) {
	q := int64(5000)
	row := FromRow(store.InstanceRow{Name: "lab", Platform: config.PlatformGitLab, MonthlyTokenQuota: &q})
	if row.MonthlyTokenQuota == nil || *row.MonthlyTokenQuota != 5000 {
		t.Errorf("FromRow quota = %v, want 5000", row.MonthlyTokenQuota)
	}
	if got := FromRow(store.InstanceRow{Name: "lab", Platform: config.PlatformGitHub}).MonthlyTokenQuota; got != nil {
		t.Errorf("FromRow with no quota = %d, want nil", *got)
	}

	gl, gh := int64(100), int64(200)
	cfg := &config.Config{
		GitLab: []config.Instance{{Name: "main", MonthlyTokenQuota: &gl}, {Name: "plain"}},
		GitHub: []config.GitHubInstance{{Name: "gh", MonthlyTokenQuota: &gh}},
	}
	r := New(cfg, nil, nil)
	for name, want := range map[string]int64{"main": 100, "gh": 200} {
		if inst, _ := r.Get(name); inst.MonthlyTokenQuota == nil || *inst.MonthlyTokenQuota != want {
			t.Errorf("%s quota = %v, want %d", name, inst.MonthlyTokenQuota, want)
		}
	}
	if inst, _ := r.Get("plain"); inst.MonthlyTokenQuota != nil {
		t.Errorf("plain quota = %d, want nil", *inst.MonthlyTokenQuota)
	}

	r.Put(row)
	if inst, ok := r.Get("lab"); !ok || inst.MonthlyTokenQuota == nil || *inst.MonthlyTokenQuota != 5000 {
		t.Fatalf("Get(lab) = %+v, %v", inst, ok)
	}
	nine := int64(9)
	if inst, ok := r.SetTokenQuota("lab", &nine); !ok || inst.MonthlyTokenQuota == nil || *inst.MonthlyTokenQuota != 9 {
		t.Errorf("SetTokenQuota = %v, %v; want 9", inst.MonthlyTokenQuota, ok)
	}
	if inst, _ := r.Get("lab"); inst.MonthlyTokenQuota == nil || *inst.MonthlyTokenQuota != 9 {
		t.Errorf("Get after SetTokenQuota = %v, want 9", inst.MonthlyTokenQuota)
	}
	if inst, ok := r.SetTokenQuota("lab", nil); !ok || inst.MonthlyTokenQuota != nil {
		t.Errorf("SetTokenQuota(nil) = %v, %v; want nil", inst.MonthlyTokenQuota, ok)
	}
	if _, ok := r.SetTokenQuota("nope", &nine); ok {
		t.Error("SetTokenQuota of an unknown instance found it")
	}
	// A config instance is not changed through the registry.
	if _, ok := r.SetTokenQuota("main", &nine); ok {
		t.Error("SetTokenQuota changed a config instance")
	}
}
