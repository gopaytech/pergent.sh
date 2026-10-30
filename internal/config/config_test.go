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

func loadGitHub(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load("testdata/github.yaml")
	if err != nil {
		t.Fatalf("Load github.yaml: %v", err)
	}
	return cfg
}

func TestLoadGitHubMixed(t *testing.T) {
	cfg := loadGitHub(t)

	gh, ok := cfg.GitHubInstance("gh")
	if !ok {
		t.Fatal("GitHubInstance(gh) not found")
	}
	if gh.APIURL != "https://api.github.com" {
		t.Errorf("APIURL default = %q", gh.APIURL)
	}
	if gh.ReviewApp.AppID != 1001 || gh.ReviewApp.PrivateKeyEnv != "GITHUB_REVIEW_APP_KEY" {
		t.Errorf("ReviewApp = %+v", gh.ReviewApp)
	}
	if gh.ApproveApp == nil || gh.ApproveApp.AppID != 1002 || gh.ApproveApp.PrivateKeyEnv != "GITHUB_APPROVE_APP_KEY" {
		t.Errorf("ApproveApp = %+v", gh.ApproveApp)
	}
	if gh.ApproveTokenEnv != "" || gh.WebhookSecretEnv != "GITHUB_WEBHOOK_SECRET" {
		t.Errorf("gh = %+v", gh)
	}

	tok, ok := cfg.GitHubInstance("gh-token")
	if !ok || tok.ApproveApp != nil || tok.ApproveTokenEnv != "GITHUB_APPROVE_TOKEN" ||
		tok.APIURL != "https://github.example.com/api/v3" {
		t.Errorf("gh-token = %+v, %v", tok, ok)
	}
	if _, ok := cfg.GitHubInstance("main"); ok {
		t.Error("GitHubInstance(main) found; main is a GitLab instance")
	}
	if _, ok := cfg.Instance("gh"); ok {
		t.Error("Instance(gh) found; Instance is GitLab-only")
	}

	for name, want := range map[string]string{"main": "gitlab", "gh": "github", "gh-token": "github"} {
		if got, ok := cfg.Platform(name); !ok || got != want {
			t.Errorf("Platform(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	if got, ok := cfg.Platform("nope"); ok || got != "" {
		t.Errorf("Platform(nope) = %q, %v", got, ok)
	}

	repo := cfg.Repositories[1]
	if repo.GitHub != "gh" || repo.GitLab != "" || repo.Project != "acme/widgets" ||
		repo.FailureStatus != FailureStatusNeutral {
		t.Errorf("github repository = %+v", repo)
	}
	if cfg.Repositories[0].FailureStatus != FailureStatusFailed {
		t.Errorf("gitlab repository default failure_status = %q", cfg.Repositories[0].FailureStatus)
	}
	// Repository stays GitLab-only: the same project path on GitHub is not it.
	if r, ok := cfg.Repository("gh", "acme/widgets"); ok {
		t.Errorf("Repository(gh, ...) = %+v, want not found", r)
	}
}

func TestValidateRejectsGitHub(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{"name used by both platforms", func(c *Config) { c.GitHub[0].Name = "main" }, "both gitlab and github"},
		{"duplicate github instance", func(c *Config) { c.GitHub[1].Name = "gh" }, "duplicate github instance"},
		{"bad github instance name", func(c *Config) { c.GitHub[0].Name = "GH_1" }, "github[0]: instance name"},
		{"repository with both platforms", func(c *Config) { c.Repositories[1].GitLab = "main" }, "exactly one of gitlab and github"},
		{"repository with neither platform", func(c *Config) { c.Repositories[1].GitHub = "" }, "exactly one of gitlab and github"},
		{"unknown github instance", func(c *Config) { c.Repositories[1].GitHub = "nope" }, "unknown github instance"},
		{"github repository names a gitlab instance", func(c *Config) { c.Repositories[1].GitHub = "main" }, "unknown github instance"},
		{"gitlab repository names a github instance", func(c *Config) { c.Repositories[0].GitLab = "gh" }, "unknown gitlab instance"},
		{"project without owner", func(c *Config) { c.Repositories[1].Project = "widgets" }, "owner/name"},
		{"project with subgroup", func(c *Config) { c.Repositories[1].Project = "acme/team/widgets" }, "owner/name"},
		{"project with empty owner", func(c *Config) { c.Repositories[1].Project = "/widgets" }, "owner/name"},
		{"project with empty name", func(c *Config) { c.Repositories[1].Project = "acme/" }, "owner/name"},
		{"duplicate github repository", func(c *Config) { c.Repositories = append(c.Repositories, c.Repositories[1]) }, "duplicate repository"},
		{"duplicate github repository, other case", func(c *Config) {
			r := c.Repositories[1]
			r.Project = "Acme/Widgets"
			c.Repositories = append(c.Repositories, r)
		}, "duplicate repository"},
		{"duplicate gitlab repository, other case", func(c *Config) {
			r := c.Repositories[0]
			r.Project = "Acme/Widgets"
			c.Repositories = append(c.Repositories, r)
		}, "duplicate repository"},
		{"bad failure_status on github repository", func(c *Config) { c.Repositories[1].FailureStatus = "maybe" }, "failure_status"},
		{"http api_url", func(c *Config) { c.GitHub[0].APIURL = "http://api.github.com" }, "github[0].api_url"},
		{"api_url with trailing slash", func(c *Config) { c.GitHub[0].APIURL = "https://api.github.com/" }, "trailing slash"},
		{"api_url with userinfo", func(c *Config) { c.GitHub[0].APIURL = "https://user:pw@api.github.com" }, "credentials"},
		{"api_url not a url", func(c *Config) { c.GitHub[0].APIURL = "api.github.com" }, "api_url"},
		{"review app_id zero", func(c *Config) { c.GitHub[0].ReviewApp.AppID = 0 }, "review_app.app_id"},
		{"review app_id negative", func(c *Config) { c.GitHub[0].ReviewApp.AppID = -3 }, "review_app.app_id"},
		{"review private_key_env empty", func(c *Config) { c.GitHub[0].ReviewApp.PrivateKeyEnv = "" }, "review_app.private_key_env"},
		{"webhook_secret_env empty", func(c *Config) { c.GitHub[0].WebhookSecretEnv = "" }, "webhook_secret_env"},
		{"approve app_id zero", func(c *Config) { c.GitHub[0].ApproveApp.AppID = 0 }, "approve_app.app_id"},
		{"approve private_key_env empty", func(c *Config) { c.GitHub[0].ApproveApp.PrivateKeyEnv = "" }, "approve_app.private_key_env"},
		{"both approvers", func(c *Config) { c.GitHub[0].ApproveTokenEnv = "SOME_TOKEN" }, "exactly one of approve_app and approve_token_env"},
		{"no approver", func(c *Config) { c.GitHub[0].ApproveApp = nil }, "exactly one of approve_app and approve_token_env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadGitHub(t)
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

func TestLoadGitHubRejectsBadYAML(t *testing.T) {
	base, err := os.ReadFile("testdata/github.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, from, to, wantErr string
	}{
		{"unknown key in review_app", "      app_id: 1001\n", "      app_id: 1001\n      app_idd: 1\n", "app_idd"},
		{"unknown key in github instance", "    webhook_secret_env: GITHUB_WEBHOOK_SECRET\n", "    webhook_secret_env: GITHUB_WEBHOOK_SECRET\n    secret: x\n", "secret"},
		{"non-numeric app_id", "app_id: 1001", "app_id: abc", "app_id"},
		{"unknown key in repository", "    project: acme/widgets\n    skills", "    project: acme/widgets\n    nope: 1\n    skills", "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Replace(string(base), tt.from, tt.to, 1)
			if body == string(base) {
				t.Fatal("test replacement did not apply")
			}
			_, err := Load(writeConfig(t, body))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestGitHubSecretsWithApproveApp(t *testing.T) {
	inst := GitHubInstance{
		Name:             "gh",
		ReviewApp:        App{AppID: 1, PrivateKeyEnv: "PERGENT_TEST_GH_REVIEW_KEY"},
		ApproveApp:       &App{AppID: 2, PrivateKeyEnv: "PERGENT_TEST_GH_APPROVE_KEY"},
		WebhookSecretEnv: "PERGENT_TEST_GH_WEBHOOK",
	}
	t.Setenv("PERGENT_TEST_GH_REVIEW_KEY", "review-pem")
	t.Setenv("PERGENT_TEST_GH_APPROVE_KEY", "approve-pem")
	t.Setenv("PERGENT_TEST_GH_WEBHOOK", "hook")
	review, approve, webhook, err := inst.Secrets()
	if err != nil || review != "review-pem" || approve != "approve-pem" || webhook != "hook" {
		t.Fatalf("Secrets() = %q %q %q %v", review, approve, webhook, err)
	}
}

func TestGitHubSecretsWithApproveToken(t *testing.T) {
	inst := GitHubInstance{
		Name:             "gh",
		ReviewApp:        App{AppID: 1, PrivateKeyEnv: "PERGENT_TEST_GH_REVIEW_KEY"},
		ApproveTokenEnv:  "PERGENT_TEST_GH_APPROVE_TOKEN",
		WebhookSecretEnv: "PERGENT_TEST_GH_WEBHOOK",
	}
	t.Setenv("PERGENT_TEST_GH_REVIEW_KEY", "review-pem")
	t.Setenv("PERGENT_TEST_GH_APPROVE_TOKEN", "tok")
	t.Setenv("PERGENT_TEST_GH_WEBHOOK", "hook")
	review, approve, webhook, err := inst.Secrets()
	if err != nil || review != "review-pem" || approve != "tok" || webhook != "hook" {
		t.Fatalf("Secrets() = %q %q %q %v", review, approve, webhook, err)
	}
}

func TestGitHubSecretsMissingEnv(t *testing.T) {
	const secretValue = "super-secret-value"
	full := func() GitHubInstance {
		return GitHubInstance{
			Name:             "gh",
			ReviewApp:        App{AppID: 1, PrivateKeyEnv: "PERGENT_TEST_GH_REVIEW_KEY"},
			ApproveApp:       &App{AppID: 2, PrivateKeyEnv: "PERGENT_TEST_GH_APPROVE_KEY"},
			WebhookSecretEnv: "PERGENT_TEST_GH_WEBHOOK",
		}
	}
	tests := []struct {
		name    string
		inst    func() GitHubInstance
		unset   string
		wantVar string
	}{
		{"review key", full, "PERGENT_TEST_GH_REVIEW_KEY", "PERGENT_TEST_GH_REVIEW_KEY"},
		{"approve key", full, "PERGENT_TEST_GH_APPROVE_KEY", "PERGENT_TEST_GH_APPROVE_KEY"},
		{"webhook", full, "PERGENT_TEST_GH_WEBHOOK", "PERGENT_TEST_GH_WEBHOOK"},
		{"approve token", func() GitHubInstance {
			i := full()
			i.ApproveApp = nil
			i.ApproveTokenEnv = "PERGENT_TEST_GH_APPROVE_TOKEN"
			return i
		}, "PERGENT_TEST_GH_APPROVE_TOKEN", "PERGENT_TEST_GH_APPROVE_TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, v := range []string{
				"PERGENT_TEST_GH_REVIEW_KEY", "PERGENT_TEST_GH_APPROVE_KEY",
				"PERGENT_TEST_GH_WEBHOOK", "PERGENT_TEST_GH_APPROVE_TOKEN",
			} {
				t.Setenv(v, secretValue)
			}
			t.Setenv(tt.unset, "")
			_, _, _, err := tt.inst().Secrets()
			if err == nil {
				t.Fatal("Secrets() = nil error, want error")
			}
			if !strings.Contains(err.Error(), tt.wantVar) {
				t.Errorf("error %q does not name %s", err, tt.wantVar)
			}
			if strings.Contains(err.Error(), secretValue) {
				t.Errorf("error %q leaks a secret value", err)
			}
		})
	}

	t.Run("no approver configured", func(t *testing.T) {
		inst := full()
		inst.ApproveApp = nil
		t.Setenv("PERGENT_TEST_GH_REVIEW_KEY", "k")
		t.Setenv("PERGENT_TEST_GH_WEBHOOK", "w")
		if _, _, _, err := inst.Secrets(); err == nil {
			t.Fatal("Secrets() = nil error, want error")
		}
	})
}

// GitHubRepository finds a repository listed under a github instance, with
// GitHub's case-insensitive owner/name; never a GitLab one.
func TestGitHubRepository(t *testing.T) {
	cfg := loadGitHub(t)
	for _, p := range []string{"acme/widgets", "Acme/Widgets"} {
		if r, ok := cfg.GitHubRepository("gh", p); !ok || r.GitHub != "gh" || r.Project != "acme/widgets" {
			t.Errorf("GitHubRepository(gh, %s) = %+v, %v", p, r, ok)
		}
	}
	if r, ok := cfg.GitHubRepository("main", "acme/widgets"); ok {
		t.Errorf("GitHubRepository(main, ...) = %+v; main is a GitLab instance", r)
	}
	if _, ok := cfg.GitHubRepository("gh", "acme/other"); ok {
		t.Error("GitHubRepository(acme/other) found")
	}
	if got := cfg.Repositories[1].Instance(); got != "gh" {
		t.Errorf("Instance() of the github repository = %q", got)
	}
	if got := cfg.Repositories[0].Instance(); got != "main" {
		t.Errorf("Instance() of the gitlab repository = %q", got)
	}
}
