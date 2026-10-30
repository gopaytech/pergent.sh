package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func loadValid(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("Load valid.yaml: %v", err)
	}
	return cfg
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	cfg := loadValid(t)

	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want :8080", cfg.Listen)
	}
	if cfg.CallbackURL != cfg.PublicURL {
		t.Errorf("CallbackURL = %q, want PublicURL %q", cfg.CallbackURL, cfg.PublicURL)
	}
	if cfg.Jobs.Image != "ghcr.io/gopaytech/pergent:v0.1.0" {
		t.Errorf("Jobs.Image = %q", cfg.Jobs.Image)
	}
	if cfg.Jobs.MaxRunning != 5 {
		t.Errorf("Jobs.MaxRunning = %d, want 5", cfg.Jobs.MaxRunning)
	}
	if cfg.Jobs.Timeout != 10*time.Minute {
		t.Errorf("Jobs.Timeout = %v, want 10m", cfg.Jobs.Timeout)
	}
	if cfg.Jobs.Concurrency != 4 {
		t.Errorf("Jobs.Concurrency = %d, want 4", cfg.Jobs.Concurrency)
	}
	if got := cfg.Repositories[0].FailureStatus; got != "failed" {
		t.Errorf("FailureStatus = %q, want failed", got)
	}
	if cfg.LLM.APIKeyEnv != "OPENCODE_API_KEY" {
		t.Errorf("LLM.APIKeyEnv = %q", cfg.LLM.APIKeyEnv)
	}

	res := cfg.Jobs.Resources
	for _, c := range []struct {
		list corev1.ResourceList
		name corev1.ResourceName
		want string
	}{
		{res.Requests, corev1.ResourceCPU, "500m"},
		{res.Requests, corev1.ResourceMemory, "1Gi"},
		{res.Limits, corev1.ResourceCPU, "2"},
		{res.Limits, corev1.ResourceMemory, "4Gi"},
	} {
		got := c.list[c.name]
		if got.Cmp(resource.MustParse(c.want)) != 0 {
			t.Errorf("default %s = %s, want %s", c.name, got.String(), c.want)
		}
	}

	if inst, ok := cfg.Instance("main"); !ok || inst.URL != "https://gitlab.example.com" {
		t.Errorf("Instance(main) = %+v, %v", inst, ok)
	}
	if _, ok := cfg.Instance("other"); ok {
		t.Error("Instance(other) found, want not found")
	}
	if repo, ok := cfg.Repository("main", "acme/widgets"); !ok || repo.Project != "acme/widgets" {
		t.Errorf("Repository = %+v, %v", repo, ok)
	}
	if _, ok := cfg.Repository("main", "acme/other"); ok {
		t.Error("Repository(acme/other) found, want not found")
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{"image without tag", func(c *Config) { c.Jobs.Image = "ghcr.io/gopaytech/pergent" }, "pinned"},
		{"image latest", func(c *Config) { c.Jobs.Image = "ghcr.io/gopaytech/pergent:latest" }, "pinned"},
		{"image registry port but no tag", func(c *Config) { c.Jobs.Image = "registry.example.com:5000/pergent" }, "pinned"},
		{"bad failure_status", func(c *Config) { c.Repositories[0].FailureStatus = "maybe" }, "failure_status"},
		{"unknown instance", func(c *Config) { c.Repositories[0].GitLab = "nope" }, "unknown gitlab instance"},
		{"duplicate repository", func(c *Config) { c.Repositories = append(c.Repositories, c.Repositories[0]) }, "duplicate repository"},
		{"duplicate instance", func(c *Config) { c.GitLab = append(c.GitLab, c.GitLab[0]) }, "duplicate gitlab instance"},
		{"empty public_url", func(c *Config) { c.PublicURL = "" }, "public_url"},
		{"http public_url", func(c *Config) { c.PublicURL = "http://pergent.example.com" }, "https"},
		{"trailing slash public_url", func(c *Config) { c.PublicURL = "https://pergent.example.com/" }, "trailing slash"},
		{"http gitlab url", func(c *Config) { c.GitLab[0].URL = "http://gitlab.example.com" }, "https"},
		{"missing namespace", func(c *Config) { c.Jobs.Namespace = "" }, "namespace"},
		{"bad instance name", func(c *Config) { c.GitLab[0].Name = "Main_1" }, "instance name"},
		{"max_running zero", func(c *Config) { c.Jobs.MaxRunning = 0 }, "max_running"},
		{"concurrency zero", func(c *Config) { c.Jobs.Concurrency = 0 }, "concurrency"},
		{"empty skills_dir", func(c *Config) { c.SkillsDir = "" }, "skills_dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadValid(t)
			tt.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestImageDigestAllowed(t *testing.T) {
	cfg := loadValid(t)
	cfg.Jobs.Image = "ghcr.io/gopaytech/pergent@sha256:" + strings.Repeat("a", 64)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with digest image: %v", err)
	}
	cfg.Jobs.Image = "registry.example.com:5000/pergent:v0.1.0"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with registry port and tag: %v", err)
	}
}

func TestLoadRejectsExplicitZeroAndUnknownKeys(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{"max_running 0", "\n  max_running: 0\n", "max_running"},
		{"concurrency 0", "\n  concurrency: 0\n", "concurrency"},
		{"unknown key in jobs", "\n  namespce: typo\n", "namespce"},
		{"bad timeout", "\n  timeout: soon\n", "timeout"},
		{"bad quantity", "\n  resources:\n    requests:\n      cpu: lots\n", "lots"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, string(base)+strings.TrimPrefix(tt.extra, "\n"))
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}

	t.Run("unknown top-level key", func(t *testing.T) {
		_, err := Load(writeConfig(t, string(base)+"surprise: 1\n"))
		if err == nil || !strings.Contains(err.Error(), "surprise") {
			t.Fatalf("Load() = %v, want unknown-key error naming surprise", err)
		}
	})
}

func TestLoadJobsOverrides(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(base), "  namespace: pergent\n", `  namespace: pergent
  timeout: 25m
  max_running: 2
  concurrency: 1
  resources:
    requests:
      cpu: 250m
      memory: 512Mi
    limits:
      memory: 2Gi
`, 1)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Jobs.Timeout != 25*time.Minute || cfg.Jobs.MaxRunning != 2 || cfg.Jobs.Concurrency != 1 {
		t.Errorf("overrides not applied: %+v", cfg.Jobs)
	}
	res := cfg.Jobs.Resources
	if q := res.Requests[corev1.ResourceCPU]; q.Cmp(resource.MustParse("250m")) != 0 {
		t.Errorf("requests cpu = %s", q.String())
	}
	if _, ok := res.Limits[corev1.ResourceCPU]; ok {
		t.Error("limits cpu set, want only the configured memory limit (no default merge)")
	}
	if q := res.Limits[corev1.ResourceMemory]; q.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("limits memory = %s", q.String())
	}
}

func TestSecrets(t *testing.T) {
	inst := Instance{
		Name:             "main",
		ReviewTokenEnv:   "PERGENT_TEST_REVIEW",
		ApproveTokenEnv:  "PERGENT_TEST_APPROVE",
		WebhookSecretEnv: "PERGENT_TEST_WEBHOOK",
	}
	t.Setenv("PERGENT_TEST_REVIEW", "r")
	t.Setenv("PERGENT_TEST_APPROVE", "a")
	t.Setenv("PERGENT_TEST_WEBHOOK", "w")
	review, approve, webhook, err := inst.Secrets()
	if err != nil || review != "r" || approve != "a" || webhook != "w" {
		t.Fatalf("Secrets() = %q %q %q %v", review, approve, webhook, err)
	}
}

func TestSecretsMissingEnv(t *testing.T) {
	t.Setenv("PERGENT_TEST_APPROVE", "a")
	t.Setenv("PERGENT_TEST_WEBHOOK", "w")
	inst := Instance{
		Name:             "main",
		ReviewTokenEnv:   "PERGENT_TEST_REVIEW_UNSET",
		ApproveTokenEnv:  "PERGENT_TEST_APPROVE",
		WebhookSecretEnv: "PERGENT_TEST_WEBHOOK",
	}
	_, _, _, err := inst.Secrets()
	if err == nil {
		t.Fatal("Secrets() = nil error, want error")
	}
	if !strings.Contains(err.Error(), "PERGENT_TEST_REVIEW_UNSET") {
		t.Fatalf("error %q does not name the env var", err)
	}
}
