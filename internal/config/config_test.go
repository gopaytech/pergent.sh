package config

import (
	"os"
	"path/filepath"
	"strconv"
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
	if cfg.Jobs.Image != "ghcr.io/gopaytech/pergent:v0.5.2" {
		t.Errorf("Jobs.Image = %q", cfg.Jobs.Image)
	}
	if cfg.Jobs.MaxRunning != 5 {
		t.Errorf("Jobs.MaxRunning = %d, want 5", cfg.Jobs.MaxRunning)
	}
	if cfg.Jobs.Timeout != 30*time.Minute {
		t.Errorf("Jobs.Timeout = %v, want 30m", cfg.Jobs.Timeout)
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
		{"empty skills list", func(c *Config) { c.Repositories[0].Skills = []string{} }, "skills"},
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
		{"app and token approvers", func(c *Config) { c.GitHub[0].ApproveTokenEnv = "SOME_TOKEN" }, "exactly one of approve_app, approve_token_env and approve_with_review_app"},
		{"app and review app approvers", func(c *Config) { c.GitHub[0].ApproveWithReviewApp = true }, "exactly one of approve_app, approve_token_env and approve_with_review_app"},
		{"token and review app approvers", func(c *Config) {
			c.GitHub[1].ApproveWithReviewApp = true
		}, "exactly one of approve_app, approve_token_env and approve_with_review_app"},
		{"no approver", func(c *Config) { c.GitHub[0].ApproveApp = nil }, "exactly one of approve_app, approve_token_env and approve_with_review_app"},
		{"approve app is the review app", func(c *Config) { c.GitHub[0].ApproveApp.AppID = c.GitHub[0].ReviewApp.AppID }, "approve_app.app_id must differ from review_app.app_id"},
		{"approve app is the review app, hint", func(c *Config) { c.GitHub[0].ApproveApp.AppID = c.GitHub[0].ReviewApp.AppID }, "; to approve with the review App, set approve_with_review_app: true"},
		{"approve app shares the review key env", func(c *Config) {
			c.GitHub[0].ApproveApp.PrivateKeyEnv = c.GitHub[0].ReviewApp.PrivateKeyEnv
		}, "approve_app.private_key_env must differ from review_app.private_key_env"},
		{"approve app shares the review key env, hint", func(c *Config) {
			c.GitHub[0].ApproveApp.PrivateKeyEnv = c.GitHub[0].ReviewApp.PrivateKeyEnv
		}, "; to approve with the review App, set approve_with_review_app: true"},
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

// One App is enough: with approve_with_review_app the review App's key is
// also the approver's, and no second env var is read.
func TestGitHubSecretsWithReviewAppApprover(t *testing.T) {
	inst := GitHubInstance{
		Name:                 "gh",
		ReviewApp:            App{AppID: 1, PrivateKeyEnv: "PERGENT_TEST_GH_REVIEW_KEY"},
		ApproveWithReviewApp: true,
		WebhookSecretEnv:     "PERGENT_TEST_GH_WEBHOOK",
	}
	t.Setenv("PERGENT_TEST_GH_REVIEW_KEY", "review-pem")
	t.Setenv("PERGENT_TEST_GH_WEBHOOK", "hook")
	review, approve, webhook, err := inst.Secrets()
	if err != nil || review != "review-pem" || approve != "review-pem" || webhook != "hook" {
		t.Fatalf("Secrets() = %q %q %q %v", review, approve, webhook, err)
	}
	both := inst
	both.ApproveTokenEnv = "PERGENT_TEST_GH_APPROVE_TOKEN"
	if _, _, _, err := both.Secrets(); err == nil {
		t.Error("Secrets() with a token and the review App approving = nil error, want error")
	}
}

func TestGitHubApprover(t *testing.T) {
	tests := []struct {
		name string
		inst GitHubInstance
		want string
	}{
		{"approve App", GitHubInstance{ApproveApp: &App{AppID: 2}}, ApproverApp},
		{"review App", GitHubInstance{ApproveWithReviewApp: true}, ApproverReviewApp},
		{"token", GitHubInstance{ApproveTokenEnv: "T"}, ApproverToken},
		{"nothing set counts as a token", GitHubInstance{}, ApproverToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.inst.Approver(); got != tt.want {
				t.Errorf("Approver() = %q, want %q", got, tt.want)
			}
		})
	}
	if ApproverApp != "app" || ApproverReviewApp != "review_app" || ApproverToken != "token" {
		t.Errorf("approver constants changed: %q %q %q", ApproverApp, ApproverReviewApp, ApproverToken)
	}
}

// approve_with_review_app: true in the file is the review App approving.
func TestLoadGitHubApproveWithReviewApp(t *testing.T) {
	base, err := os.ReadFile("testdata/github.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(base), "    approve_app:\n      app_id: 1002\n      private_key_env: GITHUB_APPROVE_APP_KEY\n", "    approve_with_review_app: true\n", 1)
	if body == string(base) {
		t.Fatal("test replacement did not apply")
	}
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := cfg.GitHub[0]; !got.ApproveWithReviewApp || got.ApproveApp != nil || got.Approver() != ApproverReviewApp {
		t.Errorf("github[0] = %+v", got)
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

func TestLoadRepositorySettings(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(base), "    project: acme/widgets\n", `    project: acme/widgets
    approve: false
    review_drafts: false
    inline_findings: false
    commands_author_and_maintainers_only: false
    incremental_reviews: false
    learn_from_dismissals: false
    job_timeout: 20m
    job_memory: 6Gi
    rerun_limit_per_mr: 20
    rerun_limit_per_repo: 100
`, 1)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	repo := cfg.Repositories[0]
	if repo.Approve == nil || *repo.Approve {
		t.Errorf("Approve = %v, want false", repo.Approve)
	}
	if repo.ReviewDrafts == nil || *repo.ReviewDrafts {
		t.Errorf("ReviewDrafts = %v, want false", repo.ReviewDrafts)
	}
	if repo.InlineFindings == nil || *repo.InlineFindings {
		t.Errorf("InlineFindings = %v, want false", repo.InlineFindings)
	}
	if repo.CommandsAuthorAndMaintainersOnly == nil || *repo.CommandsAuthorAndMaintainersOnly {
		t.Errorf("CommandsAuthorAndMaintainersOnly = %v, want false", repo.CommandsAuthorAndMaintainersOnly)
	}
	if repo.IncrementalReviews == nil || *repo.IncrementalReviews {
		t.Errorf("IncrementalReviews = %v, want false", repo.IncrementalReviews)
	}
	if repo.LearnFromDismissals == nil || *repo.LearnFromDismissals {
		t.Errorf("LearnFromDismissals = %v, want false", repo.LearnFromDismissals)
	}
	if repo.JobTimeout == nil || *repo.JobTimeout != 20*time.Minute {
		t.Errorf("JobTimeout = %v, want 20m", repo.JobTimeout)
	}
	if repo.JobMemory == nil || repo.JobMemory.Cmp(resource.MustParse("6Gi")) != 0 {
		t.Errorf("JobMemory = %v, want 6Gi", repo.JobMemory)
	}
	if repo.RerunLimitPerMR == nil || *repo.RerunLimitPerMR != 20 {
		t.Errorf("RerunLimitPerMR = %v, want 20", repo.RerunLimitPerMR)
	}
	if repo.RerunLimitPerRepo == nil || *repo.RerunLimitPerRepo != 100 {
		t.Errorf("RerunLimitPerRepo = %v, want 100", repo.RerunLimitPerRepo)
	}

	// Omitted, they stay unset: the resolver applies the defaults.
	plain := loadValid(t).Repositories[0]
	if plain.Approve != nil || plain.ReviewDrafts != nil || plain.InlineFindings != nil || plain.CommandsAuthorAndMaintainersOnly != nil || plain.IncrementalReviews != nil || plain.LearnFromDismissals != nil || plain.JobTimeout != nil || plain.JobMemory != nil ||
		plain.RerunLimitPerMR != nil || plain.RerunLimitPerRepo != nil {
		t.Errorf("omitted repository settings are set: %+v", plain)
	}
}

func TestLoadRejectsBadRepositorySettings(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, extra, wantErr string
	}{
		{"bad job_timeout", "    job_timeout: soon\n", "soon"},
		{"zero job_timeout", "    job_timeout: 0s\n", "job_timeout"},
		{"negative job_timeout", "    job_timeout: -1m\n", "job_timeout"},
		{"bad job_memory", "    job_memory: lots\n", "lots"},
		{"zero job_memory", "    job_memory: \"0\"\n", "job_memory"},
		// The default request is 1Gi: a lower limit makes an invalid Job.
		{"job_memory below the request", "    job_memory: 512Mi\n", "job_memory"},
		{"zero rerun_limit_per_mr", "    rerun_limit_per_mr: 0\n", "rerun_limit_per_mr must be from 1 to 1000, got 0"},
		{"negative rerun_limit_per_mr", "    rerun_limit_per_mr: -3\n", "rerun_limit_per_mr must be from 1 to 1000, got -3"},
		{"huge rerun_limit_per_mr", "    rerun_limit_per_mr: 1001\n", "rerun_limit_per_mr must be from 1 to 1000, got 1001"},
		{"zero rerun_limit_per_repo", "    rerun_limit_per_repo: 0\n", "rerun_limit_per_repo must be from 1 to 1000, got 0"},
		{"huge rerun_limit_per_repo", "    rerun_limit_per_repo: 5000\n", "rerun_limit_per_repo must be from 1 to 1000, got 5000"},
		{"rerun_limit_per_mr not a number", "    rerun_limit_per_mr: many\n", "rerun_limit_per_mr"},
		{"approve not a bool", "    approve: sometimes\n", "approve"},
		{"unknown repository key", "    aprove: false\n", "aprove"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Replace(string(base), "    project: acme/widgets\n", "    project: acme/widgets\n"+tt.extra, 1)
			_, err := Load(writeConfig(t, body))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// maxTurnsYAML sets jobs.max_turns and the first repository's max_turns in
// valid.yaml; an empty value leaves the key out.
func maxTurnsYAML(t *testing.T, jobs, repo string) string {
	t.Helper()
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body := string(base)
	if jobs != "" {
		body = strings.Replace(body, "  namespace: pergent\n", "  namespace: pergent\n  max_turns: "+jobs+"\n", 1)
	}
	if repo != "" {
		body = strings.Replace(body, "    project: acme/widgets\n", "    project: acme/widgets\n    max_turns: "+repo+"\n", 1)
	}
	return body
}

func TestMaxTurnsDefault(t *testing.T) {
	cfg := loadValid(t)
	if cfg.Jobs.MaxTurns != 40 || DefaultMaxTurns != 40 {
		t.Errorf("Jobs.MaxTurns = %d, DefaultMaxTurns = %d, want 40", cfg.Jobs.MaxTurns, DefaultMaxTurns)
	}
	for _, repo := range cfg.Repositories {
		if repo.MaxTurns != nil {
			t.Errorf("repository %s: MaxTurns = %d, want unset", repo.Project, *repo.MaxTurns)
		}
	}
}

func TestMaxTurnsSet(t *testing.T) {
	cfg, err := Load(writeConfig(t, maxTurnsYAML(t, "60", "120")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Jobs.MaxTurns != 60 {
		t.Errorf("Jobs.MaxTurns = %d, want 60", cfg.Jobs.MaxTurns)
	}
	if got := cfg.Repositories[0].MaxTurns; got == nil || *got != 120 {
		t.Errorf("Repositories[0].MaxTurns = %v, want 120", got)
	}
}

func TestMaxTurnsOutOfRange(t *testing.T) {
	tests := []struct {
		name, jobs, repo, wantErr string
	}{
		{"jobs below", "4", "", "jobs.max_turns must be from 5 to 200, got 4"},
		{"jobs above", "201", "", "jobs.max_turns must be from 5 to 200, got 201"},
		{"jobs zero", "0", "", "jobs.max_turns must be from 5 to 200, got 0"},
		{"repository below", "", "4", "repositories[0] (acme/widgets): max_turns must be from 5 to 200, got 4"},
		{"repository zero", "", "0", "repositories[0] (acme/widgets): max_turns must be from 5 to 200, got 0"},
		{"repository above", "", "201", "repositories[0] (acme/widgets): max_turns must be from 5 to 200, got 201"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, maxTurnsYAML(t, tt.jobs, tt.repo)))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}

	for _, n := range []int{MinMaxTurns, MaxMaxTurns} {
		cfg, err := Load(writeConfig(t, maxTurnsYAML(t, strconv.Itoa(n), strconv.Itoa(n))))
		if err != nil {
			t.Errorf("max_turns %d: %v", n, err)
			continue
		}
		if cfg.Jobs.MaxTurns != n || cfg.Repositories[0].MaxTurns == nil || *cfg.Repositories[0].MaxTurns != n {
			t.Errorf("max_turns %d: Jobs.MaxTurns = %d, repository = %v", n, cfg.Jobs.MaxTurns, cfg.Repositories[0].MaxTurns)
		}
	}
}

// dashboardYAML adds a dashboard to github.yaml, and OAuth env names to
// main and gh.
func dashboardYAML(t *testing.T) string {
	t.Helper()
	base, err := os.ReadFile("testdata/github.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(base), "    webhook_secret_env: GITLAB_WEBHOOK_SECRET\n",
		"    webhook_secret_env: GITLAB_WEBHOOK_SECRET\n    oauth_client_id_env: GITLAB_OAUTH_ID\n    oauth_client_secret_env: GITLAB_OAUTH_SECRET\n", 1)
	body = strings.Replace(body, "    webhook_secret_env: GITHUB_WEBHOOK_SECRET\n",
		"    webhook_secret_env: GITHUB_WEBHOOK_SECRET\n    oauth_client_id_env: GITHUB_OAUTH_ID\n    oauth_client_secret_env: GITHUB_OAUTH_SECRET\n", 1)
	body += `
dashboard:
  url: https://pergent.example.com
`
	return body
}

func TestLoadDashboard(t *testing.T) {
	cfg, err := Load(writeConfig(t, dashboardYAML(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.DashboardEnabled() || cfg.Dashboard.URL != "https://pergent.example.com" {
		t.Errorf("Dashboard = %+v", cfg.Dashboard)
	}
	if inst, _ := cfg.Instance("main"); inst.OAuthClientIDEnv != "GITLAB_OAUTH_ID" || inst.OAuthClientSecretEnv != "GITLAB_OAUTH_SECRET" {
		t.Errorf("gitlab OAuth envs = %+v", inst)
	}
	if gh, _ := cfg.GitHubInstance("gh"); gh.OAuthClientIDEnv != "GITHUB_OAUTH_ID" || gh.OAuthClientSecretEnv != "GITHUB_OAUTH_SECRET" {
		t.Errorf("github OAuth envs = %+v", gh)
	}

	// A bare host is the only form: the dashboard, its API and the OAuth
	// callbacks are served from / of that host.
	for _, ok := range []string{"https://pergent.example.com"} {
		cfg.Dashboard.URL = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("dashboard.url %s: %v", ok, err)
		}
	}

	// Without a dashboard section the dashboard is off.
	if loadGitHub(t).DashboardEnabled() {
		t.Error("DashboardEnabled() without dashboard.url")
	}
}

func TestValidateRejectsDashboard(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{"http url", func(c *Config) { c.Dashboard.URL = "http://pergent.example.com" }, "dashboard.url"},
		{"url with trailing slash", func(c *Config) { c.Dashboard.URL = "https://pergent.example.com/" }, "trailing slash"},
		{"url not a url", func(c *Config) { c.Dashboard.URL = "pergent.example.com" }, "dashboard.url"},
		{"url with a path", func(c *Config) { c.Dashboard.URL = "https://pergent.example.com/dashboard" }, "dashboard.url"},
		{"url with a deeper path", func(c *Config) { c.Dashboard.URL = "https://pergent.example.com/a/b" }, "must have no path"},
		{"url with a query", func(c *Config) { c.Dashboard.URL = "https://pergent.example.com?x=1" }, "must have no path"},
		{"url with a fragment", func(c *Config) { c.Dashboard.URL = "https://pergent.example.com#x" }, "must have no path"},
		{"url with a port", func(c *Config) { c.Dashboard.URL = "https://pergent.example.com:8443" }, "must have no port"},
		{"url with the https port", func(c *Config) { c.Dashboard.URL = "https://pergent.example.com:443" }, "must have no port"},
		{"gitlab oauth id without secret", func(c *Config) { c.GitLab[0].OAuthClientSecretEnv = "" }, "oauth_client_secret_env"},
		{"gitlab oauth secret without id", func(c *Config) { c.GitLab[0].OAuthClientIDEnv = "" }, "oauth_client_id_env"},
		{"github oauth id without secret", func(c *Config) { c.GitHub[0].OAuthClientSecretEnv = "" }, "oauth_client_secret_env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, dashboardYAML(t)))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.mutate(cfg)
			err = cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// dashboard.admins is gone: admins are teams in the dashboard. A config that
// still has it is refused, whatever it holds and with the dashboard on or
// off, pointing at teams.
func TestDashboardAdminsRefused(t *testing.T) {
	const want = `dashboard.admins was removed: admins are now teams in the dashboard (see the README, "Teams")`
	for name, body := range map[string]string{
		"a group":       dashboardYAML(t) + "  admins:\n    - instance: main\n      group: acme/platform\n",
		"an empty list": dashboardYAML(t) + "  admins: []\n",
		"no value":      dashboardYAML(t) + "  admins:\n",
		"dashboard off": minimalYAML + "dashboard:\n  admins:\n    - instance: main\n      group: acme/platform\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, body))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Load() = %v, want an error containing %q", err, want)
			}
		})
	}
	if _, err := Load(writeConfig(t, dashboardYAML(t))); err != nil {
		t.Fatalf("Load without admins: %v", err)
	}
}

func TestLoadRejectsUnknownDashboardKey(t *testing.T) {
	body := dashboardYAML(t) + "  next: /admin\n"
	if _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "next") {
		t.Fatalf("Load() = %v, want an unknown key error", err)
	}
}

func TestOAuthClient(t *testing.T) {
	const secretValue = "oauth-secret-value"
	gl := Instance{Name: "main", OAuthClientIDEnv: "PERGENT_TEST_OAUTH_ID", OAuthClientSecretEnv: "PERGENT_TEST_OAUTH_SECRET"}
	gh := GitHubInstance{Name: "gh", OAuthClientIDEnv: "PERGENT_TEST_OAUTH_ID", OAuthClientSecretEnv: "PERGENT_TEST_OAUTH_SECRET"}
	t.Setenv("PERGENT_TEST_OAUTH_ID", "client-id")
	t.Setenv("PERGENT_TEST_OAUTH_SECRET", secretValue)
	for name, read := range map[string]func() (string, string, error){"gitlab": gl.OAuthClient, "github": gh.OAuthClient} {
		id, secret, err := read()
		if err != nil || id != "client-id" || secret != secretValue {
			t.Errorf("%s OAuthClient() = %q %q %v", name, id, secret, err)
		}
	}

	t.Setenv("PERGENT_TEST_OAUTH_ID", "")
	for name, read := range map[string]func() (string, string, error){"gitlab": gl.OAuthClient, "github": gh.OAuthClient} {
		_, _, err := read()
		if err == nil || !strings.Contains(err.Error(), "PERGENT_TEST_OAUTH_ID") || strings.Contains(err.Error(), secretValue) {
			t.Errorf("%s OAuthClient() with the id unset = %v", name, err)
		}
	}
	if (Instance{Name: "main"}).HasOAuth() || !gl.HasOAuth() || (GitHubInstance{}).HasOAuth() || !gh.HasOAuth() {
		t.Error("HasOAuth is wrong")
	}
}

// minimalYAML is a config with no instances, repositories or llm block.
const minimalYAML = `public_url: https://pergent.example.com
jobs:
  namespace: pergent
`

func TestDashboardNeedsNoOAuthInstance(t *testing.T) {
	// Password admins can sign in, so no instance has to carry OAuth.
	body := minimalYAML + "dashboard:\n  url: https://pergent.example.com\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.DashboardEnabled() {
		t.Error("DashboardEnabled() = false")
	}

	// Instances without OAuth are fine too.
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeConfig(t, string(base)+"\ndashboard:\n  url: https://pergent.example.com\n")); err != nil {
		t.Fatalf("Load with an instance without OAuth: %v", err)
	}
}

func TestLLMOptionalWithDashboard(t *testing.T) {
	body := minimalYAML + "dashboard:\n  url: https://pergent.example.com\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HasLLM() {
		t.Error("HasLLM() = true without an llm block")
	}
	if cfg.LLM.APIKeyEnv != "" {
		t.Errorf("LLM.APIKeyEnv = %q, want no default without a provider", cfg.LLM.APIKeyEnv)
	}

	if !loadValid(t).HasLLM() {
		t.Error("HasLLM() = false with an llm block")
	}

	// A half-written llm block is still checked with the dashboard on.
	half := body + "llm:\n  provider: anthropic\n"
	if _, err := Load(writeConfig(t, half)); err == nil || !strings.Contains(err.Error(), "llm.model is required") {
		t.Fatalf("Load() = %v, want llm.model is required", err)
	}
}

// Any llm field counts as an llm block: with the dashboard on, a block
// without its provider is refused rather than ignored (the dashboard's LLM
// would otherwise replace what the file says without a word).
func TestLLMAnyFieldIsABlock(t *testing.T) {
	body := minimalYAML + "dashboard:\n  url: https://pergent.example.com\n"
	for _, tc := range []struct{ llm, want string }{
		{"llm:\n  model: claude-sonnet-4\n", "llm.provider is required"},
		{"llm:\n  base_url: https://llm.example.com/v1\n", "llm.provider is required"},
		{"llm:\n  api_key_env: MY_KEY\n", "llm.provider is required"},
		{"llm:\n  provider: anthropic\n  base_url: https://llm.example.com/v1\n", "llm.model is required"},
	} {
		if _, err := Load(writeConfig(t, body+tc.llm)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Load(%q) = %v; want %s", tc.llm, err, tc.want)
		}
	}
	// A complete block, with the key variable left to its default, is one.
	cfg, err := Load(writeConfig(t, body+"llm:\n  provider: anthropic\n  model: claude-sonnet-4\n"))
	if err != nil || !cfg.HasLLM() || cfg.LLM.APIKeyEnv == "" {
		t.Errorf("Load of a complete block = %+v, %v", cfg, err)
	}
	// An empty one (llm: {}) is none.
	cfg, err = Load(writeConfig(t, body+"llm: {}\n"))
	if err != nil || cfg.HasLLM() {
		t.Errorf("Load with llm: {} = %v, HasLLM %v; want no block", err, cfg != nil && cfg.HasLLM())
	}
}

func TestLLMRequiredWithoutDashboard(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(base), "llm:\n  provider: anthropic\n  model: claude-sonnet-4\n", "", 1)
	if body == string(base) {
		t.Fatal("llm block not found in valid.yaml")
	}
	if _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "llm.provider is required") {
		t.Fatalf("Load() = %v, want llm.provider is required", err)
	}
}

func TestInstanceRequiredWithoutDashboard(t *testing.T) {
	body := minimalYAML + "llm:\n  provider: anthropic\n  model: claude-sonnet-4\n"
	_, err := Load(writeConfig(t, body))
	want := "at least one gitlab or github instance is required (or turn on the dashboard to add them there)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Load() = %v, want %q", err, want)
	}
}

func TestAdminPasswordEnv(t *testing.T) {
	body := minimalYAML + "dashboard:\n  url: https://pergent.example.com\n  admin_password_env: PERGENT_ADMIN_PASSWORD\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Dashboard.AdminPasswordEnv != "PERGENT_ADMIN_PASSWORD" {
		t.Errorf("AdminPasswordEnv = %q", cfg.Dashboard.AdminPasswordEnv)
	}

	for _, bad := range []string{"pergent-admin", "lower_case", "1ST", "HAS SPACE", "A.B"} {
		body := minimalYAML + "dashboard:\n  url: https://pergent.example.com\n  admin_password_env: " + strconv.Quote(bad) + "\n"
		if _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "dashboard.admin_password_env") {
			t.Errorf("admin_password_env %q: Load() = %v, want an error naming dashboard.admin_password_env", bad, err)
		}
	}
}

// repo_skills names the detected repository skills to turn on; each must be a
// valid skill name.
func TestRepoSkillsConfig(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	with := func(list string) string {
		body := strings.Replace(string(base), "    project: acme/widgets\n", "    project: acme/widgets\n    repo_skills: "+list+"\n", 1)
		if body == string(base) {
			t.Fatal("test replacement did not apply")
		}
		return body
	}

	cfg, err := Load(writeConfig(t, with("[a, team-rules]")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Repositories[0].RepoSkills; len(got) != 2 || got[0] != "a" || got[1] != "team-rules" {
		t.Errorf("RepoSkills = %v, want [a team-rules]", got)
	}
	if got := loadValid(t).Repositories[0].RepoSkills; got != nil {
		t.Errorf("RepoSkills = %v, want nil when not set", got)
	}

	for _, bad := range []string{"[../x]", `["a b"]`, `[""]`, "[.hidden]"} {
		_, err := Load(writeConfig(t, with(bad)))
		if err == nil || !strings.Contains(err.Error(), "repo_skills") {
			t.Errorf("Load with repo_skills %s = %v, want an error naming repo_skills", bad, err)
		}
	}
}

// skills_dir is gone: the library lives in the dashboard. A config that still
// has it is refused, whatever it holds, pointing at the README.
func TestSkillsDirRefused(t *testing.T) {
	const want = `skills_dir was removed: skills live in the dashboard (see the README, "Skills and instructions")`
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"a path": "/skills", "no value": "", "an empty string": `""`} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, string(base)+"skills_dir: "+value+"\n"))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Load() = %v, want an error containing %q", err, want)
			}
		})
	}
	// Without it, and with no dashboard, a config loads: nothing reads a directory.
	cfg, err := Load(writeConfig(t, string(base)))
	if err != nil {
		t.Fatalf("Load without skills_dir: %v", err)
	}
	if cfg.DashboardEnabled() {
		t.Error("the dashboard is on in a config that does not set dashboard.url")
	}
}

// A repository's skills is absent (the default list) or a non-empty list: an
// empty list is refused, as it would silently mean the default list.
func TestRepositorySkillsMustNotBeEmpty(t *testing.T) {
	base, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	with := func(skills string) string {
		body := strings.Replace(string(base), "    project: acme/widgets\n", "    project: acme/widgets\n    skills:"+skills+"\n", 1)
		if body == string(base) {
			t.Fatal("test replacement did not apply")
		}
		return body
	}
	for _, empty := range []string{" []", " [ ]"} {
		_, err := Load(writeConfig(t, with(empty)))
		if err == nil || !strings.Contains(err.Error(), "skills") || !strings.Contains(err.Error(), "acme/widgets") {
			t.Errorf("Load with skills:%s = %v, want an error naming skills and the repository", empty, err)
		}
	}
	// Null and absent both mean the default list.
	for name, body := range map[string]string{"null": with(""), "absent": string(base)} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load with skills %s: %v", name, err)
		}
		if cfg.Repositories[0].Skills != nil {
			t.Errorf("skills %s = %v, want nil", name, cfg.Repositories[0].Skills)
		}
	}
	cfg, err := Load(writeConfig(t, with(" [security]")))
	if err != nil || len(cfg.Repositories[0].Skills) != 1 {
		t.Errorf("Load with one skill = %v, %v", cfg, err)
	}
}

func TestDefaultTimeoutIs30Minutes(t *testing.T) {
	cfg := loadValid(t)
	if cfg.Jobs.Timeout != 30*time.Minute {
		t.Errorf("Jobs.Timeout = %v, want 30m", cfg.Jobs.Timeout)
	}
}

func TestTimeoutWarnings(t *testing.T) {
	dur := func(d time.Duration) *time.Duration { return &d }
	num := func(n int) *int { return &n }
	tests := []struct {
		name string
		edit func(c *Config)
		want []string
	}{
		{"defaults", func(c *Config) {}, nil},
		{
			"short jobs timeout",
			func(c *Config) { c.Jobs.Timeout = 10 * time.Minute },
			[]string{"jobs.timeout 10m0s is short for jobs.max_turns 40: allow about 20m0s (30s a step)"},
		},
		{
			"repository timeout and steps",
			func(c *Config) {
				c.Repositories[0].JobTimeout = dur(5 * time.Minute)
				c.Repositories[0].MaxTurns = num(20)
			},
			[]string{"repositories[0] (acme/widgets): job_timeout 5m0s is short for max_turns 20: allow about 10m0s (30s a step)"},
		},
		{
			"repository steps only",
			func(c *Config) { c.Repositories[0].MaxTurns = num(100) },
			[]string{"repositories[0] (acme/widgets): job_timeout 30m0s is short for max_turns 100: allow about 50m0s (30s a step)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadValid(t)
			tt.edit(cfg)
			got := TimeoutWarnings(cfg)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("TimeoutWarnings = %q, want %q", got, tt.want)
			}
		})
	}
}
