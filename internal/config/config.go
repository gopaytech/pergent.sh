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

// Config is the whole config file after defaults.
type Config struct {
	Listen       string       `yaml:"listen"`
	PublicURL    string       `yaml:"public_url"`
	CallbackURL  string       `yaml:"callback_url"`
	SkillsDir    string       `yaml:"skills_dir"`
	GitLab       []Instance   `yaml:"gitlab"`
	Repositories []Repository `yaml:"repositories"`
	LLM          LLM          `yaml:"llm"`
	Jobs         Jobs         `yaml:"jobs"`
}

// Instance is one GitLab server and the env vars holding its secrets.
type Instance struct {
	Name             string `yaml:"name"`
	URL              string `yaml:"url"`
	ReviewTokenEnv   string `yaml:"review_token_env"`
	ApproveTokenEnv  string `yaml:"approve_token_env"`
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
}

// Repository is a project that is reviewed. Listing it switches it on.
type Repository struct {
	GitLab        string        `yaml:"gitlab"`
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
	Listen       string       `yaml:"listen"`
	PublicURL    string       `yaml:"public_url"`
	CallbackURL  string       `yaml:"callback_url"`
	SkillsDir    string       `yaml:"skills_dir"`
	GitLab       []Instance   `yaml:"gitlab"`
	Repositories []Repository `yaml:"repositories"`
	LLM          LLM          `yaml:"llm"`
	Jobs         fileJobs     `yaml:"jobs"`
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

	instances := make(map[string]bool, len(c.GitLab))
	for i, inst := range c.GitLab {
		if !instanceNameRE.MatchString(inst.Name) {
			add("gitlab[%d]: instance name %q must match %s", i, inst.Name, instanceNameRE)
		}
		if instances[inst.Name] {
			add("duplicate gitlab instance %q", inst.Name)
		}
		instances[inst.Name] = true
		if err := checkURL(fmt.Sprintf("gitlab[%d].url", i), inst.URL, false); err != nil {
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

	seen := make(map[[2]string]bool, len(c.Repositories))
	for i, repo := range c.Repositories {
		if !instances[repo.GitLab] {
			add("repositories[%d]: unknown gitlab instance %q", i, repo.GitLab)
		}
		if repo.Project == "" {
			add("repositories[%d]: project is required", i)
		}
		key := [2]string{repo.GitLab, repo.Project}
		if seen[key] {
			add("duplicate repository %s on gitlab instance %q", repo.Project, repo.GitLab)
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

// Instance returns the GitLab instance with the given name.
func (c *Config) Instance(name string) (Instance, bool) {
	for _, inst := range c.GitLab {
		if inst.Name == name {
			return inst, true
		}
	}
	return Instance{}, false
}

// Repository returns the repository configured for project on instance.
func (c *Config) Repository(instance, project string) (Repository, bool) {
	for _, repo := range c.Repositories {
		if repo.GitLab == instance && repo.Project == project {
			return repo, true
		}
	}
	return Repository{}, false
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
