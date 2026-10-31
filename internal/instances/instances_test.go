package instances

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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
	if err := st.PutLLM(ctx, "anthropic", "claude-sonnet-4", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
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
	if err := st.PutLLM(ctx, "anthropic", "claude-sonnet-4", "https://llm.example.com/v1", box.Seal([]byte("key-one")), at); err != nil {
		t.Fatal(err)
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
	if err := st.PutLLM(ctx, "openai", "gpt", "", box.Seal([]byte("key-two")), at); err != nil {
		t.Fatal(err)
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
	if err := st.PutLLM(ctx, "anthropic", "m", "", other.Seal([]byte("key")), time.Now()); err != nil {
		t.Fatal(err)
	}
	r := New(testConfig(), st, newBox(t, 1))
	if err := r.LoadLLM(ctx); err != nil {
		t.Fatalf("LoadLLM = %v, want the unopenable key reported as not set", err)
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
