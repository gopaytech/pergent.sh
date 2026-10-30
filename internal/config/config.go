// Package config loads and validates the pergent.sh YAML config file.
//
// The file holds no secrets: tokens and keys are named by environment
// variable (*_env fields) and read with Instance.Secrets.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	defaultListen      = ":8080"
	defaultImage       = "ghcr.io/gopaytech/pergent:v0.1.0"
	defaultMaxRunning  = 5
	defaultTimeout     = 10 * time.Minute
	defaultConcurrency = 4
	defaultAPIKeyEnv   = "OPENCODE_API_KEY"
)

// FailureStatus is the commit status used when a review fails.
type FailureStatus string

const (
	FailureStatusFailed  FailureStatus = "failed"
	FailureStatusNeutral FailureStatus = "neutral"
)

const defaultGitHubAPIURL = "https://api.github.com"

// Platform names, as returned by Config.Platform.
const (
	PlatformGitLab = "gitlab"
	PlatformGitHub = "github"
)

// Config is the whole config file after defaults.
type Config struct {
	Listen       string           `yaml:"listen"`
	PublicURL    string           `yaml:"public_url"`
	CallbackURL  string           `yaml:"callback_url"`
	SkillsDir    string           `yaml:"skills_dir"`
	GitLab       []Instance       `yaml:"gitlab"`
	GitHub       []GitHubInstance `yaml:"github"`
	Repositories []Repository     `yaml:"repositories"`
	LLM          LLM              `yaml:"llm"`
	Jobs         Jobs             `yaml:"jobs"`
}

// Instance is one GitLab server and the env vars holding its secrets.
type Instance struct {
	Name             string `yaml:"name"`
	URL              string `yaml:"url"`
	ReviewTokenEnv   string `yaml:"review_token_env"`
	ApproveTokenEnv  string `yaml:"approve_token_env"`
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
}

// GitHubInstance is one pair of GitHub Apps (and the webhook that feeds the
// review App) and the env vars holding their secrets. One instance covers
// every organisation the Apps are installed on.
//
// The review App comments, writes the check run and clones; it holds the
// webhook. Approvals come from exactly one of ApproveApp (a second App, the
// default) or ApproveTokenEnv (a bot user's fine-grained token, the fallback
// when App approvals do not count toward required reviews).
type GitHubInstance struct {
	Name             string `yaml:"name"`
	APIURL           string `yaml:"api_url"` // default https://api.github.com
	ReviewApp        App    `yaml:"review_app"`
	ApproveApp       *App   `yaml:"approve_app"`
	ApproveTokenEnv  string `yaml:"approve_token_env"`
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
}

// App is a GitHub App: its numeric ID and the env var holding its PEM
// private key.
type App struct {
	AppID         int64  `yaml:"app_id"`
	PrivateKeyEnv string `yaml:"private_key_env"`
}

// Repository is a project that is reviewed. Listing it switches it on. It
// names exactly one of GitLab (an instance under gitlab:) and GitHub (an
// instance under github:); Project is a GitLab path or a GitHub owner/name.
type Repository struct {
	GitLab        string        `yaml:"gitlab"`
	GitHub        string        `yaml:"github"`
	Project       string        `yaml:"project"`
	Skills        []string      `yaml:"skills"`
	FailureStatus FailureStatus `yaml:"failure_status"`
}

// LLM is the model every review uses.
type LLM struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
}

// Jobs shapes the Kubernetes Jobs that run pergent.
type Jobs struct {
	Namespace   string
	Image       string
	MaxRunning  int
	Timeout     time.Duration
	Concurrency int
	Resources   corev1.ResourceRequirements
}

// fileConfig mirrors Config for decoding: Jobs needs string durations and
// quantities, and pointers tell an omitted number from an explicit 0.
type fileConfig struct {
	Listen       string           `yaml:"listen"`
	PublicURL    string           `yaml:"public_url"`
	CallbackURL  string           `yaml:"callback_url"`
	SkillsDir    string           `yaml:"skills_dir"`
	GitLab       []Instance       `yaml:"gitlab"`
	GitHub       []GitHubInstance `yaml:"github"`
	Repositories []Repository     `yaml:"repositories"`
	LLM          LLM              `yaml:"llm"`
	Jobs         fileJobs         `yaml:"jobs"`
}

type fileJobs struct {
	Namespace   string        `yaml:"namespace"`
	Image       string        `yaml:"image"`
	MaxRunning  *int          `yaml:"max_running"`
	Timeout     *duration     `yaml:"timeout"`
	Concurrency *int          `yaml:"concurrency"`
	Resources   fileResources `yaml:"resources"`
}

type fileResources struct {
	Requests map[string]quantity `yaml:"requests"`
	Limits   map[string]quantity `yaml:"limits"`
}

// duration decodes "10m" style strings.
type duration time.Duration

func (d *duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q: %w", value.Line, s, err)
	}
	*d = duration(parsed)
	return nil
}

// quantity decodes Kubernetes quantities such as "500m" or "1Gi".
type quantity struct{ resource.Quantity }

func (q *quantity) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := resource.ParseQuantity(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid quantity %q: %w", value.Line, s, err)
	}
	q.Quantity = parsed
	return nil
}

func toResourceList(m map[string]quantity) corev1.ResourceList {
	if len(m) == 0 {
		return nil
	}
	list := make(corev1.ResourceList, len(m))
	for name, q := range m {
		list[corev1.ResourceName(name)] = q.Quantity
	}
	return list
}

func defaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
}

// Load reads path, applies defaults and validates. Unknown keys are errors.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var raw fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg := &Config{
		Listen:       raw.Listen,
		PublicURL:    raw.PublicURL,
		CallbackURL:  raw.CallbackURL,
		SkillsDir:    raw.SkillsDir,
		GitLab:       raw.GitLab,
		GitHub:       raw.GitHub,
		Repositories: raw.Repositories,
		LLM:          raw.LLM,
		Jobs: Jobs{
			Namespace:   raw.Jobs.Namespace,
			Image:       raw.Jobs.Image,
			MaxRunning:  defaultMaxRunning,
			Timeout:     defaultTimeout,
			Concurrency: defaultConcurrency,
			Resources: corev1.ResourceRequirements{
				Requests: toResourceList(raw.Jobs.Resources.Requests),
				Limits:   toResourceList(raw.Jobs.Resources.Limits),
			},
		},
	}
	if raw.Jobs.MaxRunning != nil {
		cfg.Jobs.MaxRunning = *raw.Jobs.MaxRunning
	}
	if raw.Jobs.Concurrency != nil {
		cfg.Jobs.Concurrency = *raw.Jobs.Concurrency
	}
	if raw.Jobs.Timeout != nil {
		cfg.Jobs.Timeout = time.Duration(*raw.Jobs.Timeout)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.CallbackURL == "" {
		c.CallbackURL = c.PublicURL
	}
	if c.LLM.APIKeyEnv == "" {
		c.LLM.APIKeyEnv = defaultAPIKeyEnv
	}
	if c.Jobs.Image == "" {
		c.Jobs.Image = defaultImage
	}
	if len(c.Jobs.Resources.Requests) == 0 && len(c.Jobs.Resources.Limits) == 0 {
		c.Jobs.Resources = defaultResources()
	}
	for i := range c.GitHub {
		if c.GitHub[i].APIURL == "" {
			c.GitHub[i].APIURL = defaultGitHubAPIURL
		}
	}
	for i := range c.Repositories {
		if c.Repositories[i].FailureStatus == "" {
			c.Repositories[i].FailureStatus = FailureStatusFailed
		}
	}
}

var (
	instanceNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)
	digestRE       = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// Validate checks the config and returns every problem found, joined.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if err := checkURL("public_url", c.PublicURL, true); err != nil {
		errs = append(errs, err)
	}
	if c.CallbackURL != "" {
		if err := checkURL("callback_url", c.CallbackURL, false); err != nil {
			errs = append(errs, err)
		}
	}
	if c.SkillsDir == "" {
		add("skills_dir is required")
	}

	// Instance names are unique across gitlab: and github:, so a name alone
	// tells the platform (Config.Platform).
	names := make(map[string]string, len(c.GitLab)+len(c.GitHub))
	claim := func(platform, name string) {
		switch prev, ok := names[name]; {
		case !ok:
			names[name] = platform
		case prev == platform:
			add("duplicate %s instance %q", platform, name)
		default:
			add("instance name %q is used by both gitlab and github", name)
		}
	}
	gitlabNames := make(map[string]bool, len(c.GitLab))
	for i, inst := range c.GitLab {
		if !instanceNameRE.MatchString(inst.Name) {
			add("gitlab[%d]: instance name %q must match %s", i, inst.Name, instanceNameRE)
		}
		claim(PlatformGitLab, inst.Name)
		gitlabNames[inst.Name] = true
		if err := checkURL(fmt.Sprintf("gitlab[%d].url", i), inst.URL, true); err != nil {
			errs = append(errs, err)
		}
		for _, f := range []struct{ field, val string }{
			{"review_token_env", inst.ReviewTokenEnv},
			{"approve_token_env", inst.ApproveTokenEnv},
			{"webhook_secret_env", inst.WebhookSecretEnv},
		} {
			if f.val == "" {
				add("gitlab[%d] (%s): %s is required", i, inst.Name, f.field)
			}
		}
	}

	githubNames := make(map[string]bool, len(c.GitHub))
	for i, inst := range c.GitHub {
		if !instanceNameRE.MatchString(inst.Name) {
			add("github[%d]: instance name %q must match %s", i, inst.Name, instanceNameRE)
		}
		claim(PlatformGitHub, inst.Name)
		githubNames[inst.Name] = true
		errs = append(errs, checkGitHubInstance(i, inst)...)
	}

	seen := make(map[[3]string]bool, len(c.Repositories))
	for i, repo := range c.Repositories {
		platform, instance := PlatformGitLab, repo.GitLab
		switch {
		case repo.GitLab != "" && repo.GitHub != "":
			add("repositories[%d]: exactly one of gitlab and github must be set, got both", i)
		case repo.GitLab == "" && repo.GitHub == "":
			add("repositories[%d]: exactly one of gitlab and github must be set, got neither", i)
		case repo.GitHub != "":
			platform, instance = PlatformGitHub, repo.GitHub
			if !githubNames[instance] {
				add("repositories[%d]: unknown github instance %q", i, instance)
			}
		default:
			if !gitlabNames[instance] {
				add("repositories[%d]: unknown gitlab instance %q", i, instance)
			}
		}
		switch {
		case repo.Project == "":
			add("repositories[%d]: project is required", i)
		case platform == PlatformGitHub && !isOwnerName(repo.Project):
			add("repositories[%d]: github project %q must be owner/name", i, repo.Project)
		}
		// Both platforms match project paths case-insensitively.
		key := [3]string{platform, instance, strings.ToLower(repo.Project)}
		if seen[key] {
			add("duplicate repository %s on %s instance %q", repo.Project, platform, instance)
		}
		seen[key] = true
		if repo.FailureStatus != FailureStatusFailed && repo.FailureStatus != FailureStatusNeutral {
			add("repositories[%d] (%s): failure_status %q must be %q or %q",
				i, repo.Project, repo.FailureStatus, FailureStatusFailed, FailureStatusNeutral)
		}
	}

	if c.LLM.Provider == "" {
		add("llm.provider is required")
	}
	if c.LLM.Model == "" {
		add("llm.model is required")
	}
	if c.LLM.APIKeyEnv == "" {
		add("llm.api_key_env is required")
	}

	if c.Jobs.Namespace == "" {
		add("jobs.namespace is required")
	}
	if err := checkImagePinned(c.Jobs.Image); err != nil {
		errs = append(errs, err)
	}
	if c.Jobs.MaxRunning < 1 {
		add("jobs.max_running must be at least 1, got %d", c.Jobs.MaxRunning)
	}
	if c.Jobs.Concurrency < 1 {
		add("jobs.concurrency must be at least 1, got %d", c.Jobs.Concurrency)
	}
	if c.Jobs.Timeout <= 0 {
		add("jobs.timeout must be positive, got %s", c.Jobs.Timeout)
	}

	return errors.Join(errs...)
}

// checkGitHubInstance returns every problem with the i-th github instance
// other than its name.
func checkGitHubInstance(i int, inst GitHubInstance) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	prefix := fmt.Sprintf("github[%d] (%s)", i, inst.Name)

	field := fmt.Sprintf("github[%d].api_url", i)
	if err := checkURL(field, inst.APIURL, true); err != nil {
		errs = append(errs, err)
	} else if u, _ := url.Parse(inst.APIURL); u.User != nil {
		add("%s must not contain credentials", field)
	}

	checkApp := func(name string, app App) {
		if app.AppID <= 0 {
			add("%s: %s.app_id must be positive, got %d", prefix, name, app.AppID)
		}
		if app.PrivateKeyEnv == "" {
			add("%s: %s.private_key_env is required", prefix, name)
		}
	}
	checkApp("review_app", inst.ReviewApp)

	switch {
	case inst.ApproveApp != nil && inst.ApproveTokenEnv != "":
		add("%s: exactly one of approve_app and approve_token_env must be set, got both", prefix)
	case inst.ApproveApp == nil && inst.ApproveTokenEnv == "":
		add("%s: exactly one of approve_app and approve_token_env must be set, got neither", prefix)
	case inst.ApproveApp != nil:
		checkApp("approve_app", *inst.ApproveApp)
		// The same App, or the same key, would review and approve: the
		// two identities would silently be one.
		if inst.ApproveApp.AppID > 0 && inst.ApproveApp.AppID == inst.ReviewApp.AppID {
			add("%s: approve_app.app_id must differ from review_app.app_id, got %d for both", prefix, inst.ReviewApp.AppID)
		}
		if inst.ApproveApp.PrivateKeyEnv != "" && inst.ApproveApp.PrivateKeyEnv == inst.ReviewApp.PrivateKeyEnv {
			add("%s: approve_app.private_key_env must differ from review_app.private_key_env, got %q for both", prefix, inst.ReviewApp.PrivateKeyEnv)
		}
	}

	if inst.WebhookSecretEnv == "" {
		add("%s: webhook_secret_env is required", prefix)
	}
	return errs
}

// isOwnerName reports whether project is exactly owner/name.
func isOwnerName(project string) bool {
	owner, name, ok := strings.Cut(project, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

// checkURL requires an absolute URL without a trailing slash. When https is
// true the scheme must be https; otherwise http or https is accepted.
func checkURL(field, raw string, httpsOnly bool) error {
	if raw == "" {
		return fmt.Errorf("%s is required", field)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s %q is not a valid URL", field, raw)
	}
	switch {
	case httpsOnly && u.Scheme != "https":
		return fmt.Errorf("%s %q must use https", field, raw)
	case u.Scheme != "https" && u.Scheme != "http":
		return fmt.Errorf("%s %q must use http or https", field, raw)
	}
	if strings.HasSuffix(raw, "/") {
		return fmt.Errorf("%s %q must not have a trailing slash", field, raw)
	}
	return nil
}

// checkImagePinned requires a digest, or a tag other than latest. A colon in
// the registry host ("host:5000/img") is not a tag.
func checkImagePinned(image string) error {
	if image == "" {
		return errors.New("jobs.image is required and must be pinned by tag or digest")
	}
	if digestRE.MatchString(image) {
		return nil
	}
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	i := strings.LastIndex(name, ":")
	if i < 0 || i == len(name)-1 {
		return fmt.Errorf("jobs.image %q must be pinned by tag or digest", image)
	}
	if name[i+1:] == "latest" {
		return fmt.Errorf("jobs.image %q must be pinned by tag or digest, not :latest", image)
	}
	return nil
}

// Instance returns the GitLab instance with the given name. It knows nothing
// of GitHub instances: use GitHubInstance, and Platform to tell which a name is.
func (c *Config) Instance(name string) (Instance, bool) {
	for _, inst := range c.GitLab {
		if inst.Name == name {
			return inst, true
		}
	}
	return Instance{}, false
}

// GitHubInstance returns the GitHub instance with the given name.
func (c *Config) GitHubInstance(name string) (GitHubInstance, bool) {
	for _, inst := range c.GitHub {
		if inst.Name == name {
			return inst, true
		}
	}
	return GitHubInstance{}, false
}

// Platform reports which platform the named instance is on: "gitlab" or
// "github". Names are unique across both lists.
func (c *Config) Platform(instance string) (string, bool) {
	if _, ok := c.Instance(instance); ok {
		return PlatformGitLab, true
	}
	if _, ok := c.GitHubInstance(instance); ok {
		return PlatformGitHub, true
	}
	return "", false
}

// Repository returns the GitLab repository configured for project on
// instance. GitHub repositories are not found here.
func (c *Config) Repository(instance, project string) (Repository, bool) {
	for _, repo := range c.Repositories {
		if repo.GitLab == instance && repo.Project == project {
			return repo, true
		}
	}
	return Repository{}, false
}

// GitHubRepository returns the repository configured for owner/name project
// on GitHub instance instance. GitHub names are case-insensitive.
func (c *Config) GitHubRepository(instance, project string) (Repository, bool) {
	for _, repo := range c.Repositories {
		if repo.GitHub == instance && strings.EqualFold(repo.Project, project) {
			return repo, true
		}
	}
	return Repository{}, false
}

// Instance is the name of the instance the repository is on, whichever
// platform it is.
func (r Repository) Instance() string {
	if r.GitHub != "" {
		return r.GitHub
	}
	return r.GitLab
}

// Secrets reads the instance's review token, approve token and webhook
// secret from the environment. An unset or empty variable is an error that
// names it.
func (i Instance) Secrets() (review, approve, webhook string, err error) {
	read := func(env string) (string, error) {
		if env == "" {
			return "", fmt.Errorf("gitlab instance %q: env var name is empty", i.Name)
		}
		v := os.Getenv(env)
		if v == "" {
			return "", fmt.Errorf("gitlab instance %q: env var %s is not set or empty", i.Name, env)
		}
		return v, nil
	}
	if review, err = read(i.ReviewTokenEnv); err != nil {
		return "", "", "", err
	}
	if approve, err = read(i.ApproveTokenEnv); err != nil {
		return "", "", "", err
	}
	if webhook, err = read(i.WebhookSecretEnv); err != nil {
		return "", "", "", err
	}
	return review, approve, webhook, nil
}

// Secrets reads the instance's review App private key, approver secret (the
// approve App's private key, or the approve token) and webhook secret from
// the environment. An unset or empty variable is an error that names it,
// never its value.
func (g GitHubInstance) Secrets() (reviewKey, approveKeyOrToken, webhook string, err error) {
	read := func(env string) (string, error) {
		if env == "" {
			return "", fmt.Errorf("github instance %q: env var name is empty", g.Name)
		}
		v := os.Getenv(env)
		if v == "" {
			return "", fmt.Errorf("github instance %q: env var %s is not set or empty", g.Name, env)
		}
		return v, nil
	}
	approveEnv := g.ApproveTokenEnv
	switch {
	case g.ApproveApp != nil && g.ApproveTokenEnv != "":
		return "", "", "", fmt.Errorf("github instance %q: both approve_app and approve_token_env are set", g.Name)
	case g.ApproveApp != nil:
		approveEnv = g.ApproveApp.PrivateKeyEnv
	case g.ApproveTokenEnv == "":
		return "", "", "", fmt.Errorf("github instance %q: neither approve_app nor approve_token_env is set", g.Name)
	}
	if reviewKey, err = read(g.ReviewApp.PrivateKeyEnv); err != nil {
		return "", "", "", err
	}
	if approveKeyOrToken, err = read(approveEnv); err != nil {
		return "", "", "", err
	}
	if webhook, err = read(g.WebhookSecretEnv); err != nil {
		return "", "", "", err
	}
	return reviewKey, approveKeyOrToken, webhook, nil
}
