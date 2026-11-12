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
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gopaytech/pergent/render"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gopaytech/pergent.sh/internal/links"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	defaultListen      = ":8080"
	defaultImage       = "ghcr.io/gopaytech/pergent:v0.9.0"
	defaultMaxRunning  = 5
	defaultWorkSize    = "20Gi"
	defaultTimeout     = 30 * time.Minute
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
	Listen        string           `yaml:"listen"`
	MetricsListen string           `yaml:"metrics_listen"` // empty means off; serves only GET /metrics
	PublicURL     string           `yaml:"public_url"`
	CallbackURL   string           `yaml:"callback_url"`
	GitLab        []Instance       `yaml:"gitlab"`
	GitHub        []GitHubInstance `yaml:"github"`
	Repositories  []Repository     `yaml:"repositories"`
	LLM           LLM              `yaml:"llm"`
	Jobs          Jobs             `yaml:"jobs"`
	Dashboard     Dashboard        `yaml:"dashboard"`
}

// Dashboard is the web dashboard. It is enabled when URL is set.
type Dashboard struct {
	// URL is the https address the dashboard is served at; the OIDC
	// callback is under it.
	URL string `yaml:"url"`
	// AdminPasswordEnv names the env var holding the password of the first
	// admin account, created when the dashboard starts with no accounts.
	AdminPasswordEnv string `yaml:"admin_password_env"`
	// Admins is kept only to refuse it: admins are teams in the dashboard
	// now, not platform groups named here.
	Admins yaml.Node `yaml:"admins"`
	// SignIn sets how people other than the local admins sign in: OIDC, or a
	// validated JWT from a proxy, behind an email allowlist. When its mode is
	// set it wins over what an admin saved in the dashboard.
	SignIn SignIn `yaml:"sign_in"`
}

// DashboardEnabled reports whether the dashboard is on: dashboard.url is set.
func (c *Config) DashboardEnabled() bool { return c.Dashboard.URL != "" }

// HasLLM reports whether the file has an llm: block: any of its fields set
// (then the block must be complete). With the dashboard on the LLM can be
// added there instead.
func (c *Config) HasLLM() bool { return c.LLM != (LLM{}) }

// Instance is one GitLab server and the env vars holding its secrets.
// Approvals come from exactly one of ApproveTokenEnv (a second user, the
// default way) and ApproveWithReviewBot (the review bot's own user).
type Instance struct {
	Name                 string `yaml:"name"`
	URL                  string `yaml:"url"`
	ReviewTokenEnv       string `yaml:"review_token_env"`
	ApproveTokenEnv      string `yaml:"approve_token_env"`
	ApproveWithReviewBot bool   `yaml:"approve_with_review_bot"`
	WebhookSecretEnv     string `yaml:"webhook_secret_env"`
	// OAuthClientIDEnv and OAuthClientSecretEnv name the env vars holding
	// the OAuth application used to sign in to the dashboard. Both or
	// neither are set; neither means no sign-in with this instance.
	OAuthClientIDEnv     string `yaml:"oauth_client_id_env"`
	OAuthClientSecretEnv string `yaml:"oauth_client_secret_env"`
	// TrustedAuthorsOnly is the default of the instance's repositories:
	// TrustFollowVisibility (also when empty), TrustOn or TrustOff.
	TrustedAuthorsOnly string `yaml:"trusted_authors_only"`
	// MonthlyTokenQuota is the instance's token budget per UTC calendar
	// month, from 1 to MaxMonthlyTokenQuota (nil: none).
	MonthlyTokenQuota *int64 `yaml:"monthly_token_quota"`
}

// MaxMonthlyTokenQuota is the largest monthly_token_quota. It keeps the sums
// the quota is compared with far from overflowing, and below 2^53, where a
// number sent to the browser stays exact.
const MaxMonthlyTokenQuota int64 = 1_000_000_000_000_000

// The values of trusted_authors_only. A repository takes TrustOn or TrustOff;
// an instance also TrustFollowVisibility: on for a public repository, off
// otherwise.
const (
	TrustFollowVisibility = "follow_visibility"
	TrustOn               = "on"
	TrustOff              = "off"
)

// GitHubInstance is a GitHub App (and the webhook that feeds it), usually
// with a second one, and the env vars holding their secrets. One instance
// covers every organisation the Apps are installed on.
//
// The review App comments, writes the check run and clones; it holds the
// webhook. Approvals come from exactly one of ApproveWithReviewApp (the
// review App itself, so one App is enough; it then needs Pull requests:
// write), ApproveApp (a second App, so the identity that writes the review
// never approves it) or ApproveTokenEnv (a bot user's fine-grained token,
// the fallback when App approvals do not count toward required reviews).
type GitHubInstance struct {
	Name                 string `yaml:"name"`
	APIURL               string `yaml:"api_url"` // default https://api.github.com
	ReviewApp            App    `yaml:"review_app"`
	ApproveApp           *App   `yaml:"approve_app"`
	ApproveTokenEnv      string `yaml:"approve_token_env"`
	ApproveWithReviewApp bool   `yaml:"approve_with_review_app"`
	WebhookSecretEnv     string `yaml:"webhook_secret_env"`
	// OAuthClientIDEnv and OAuthClientSecretEnv name the env vars holding
	// the review App's client ID and secret, used to sign in to the
	// dashboard. Both or neither are set.
	OAuthClientIDEnv     string `yaml:"oauth_client_id_env"`
	OAuthClientSecretEnv string `yaml:"oauth_client_secret_env"`
	// TrustedAuthorsOnly is as for Instance.
	TrustedAuthorsOnly string `yaml:"trusted_authors_only"`
	// MonthlyTokenQuota is as for Instance.
	MonthlyTokenQuota *int64 `yaml:"monthly_token_quota"`
}

// The three ways a GitHub instance approves, as Approver reports them.
const (
	ApproverApp       = "app"        // a second App
	ApproverReviewApp = "review_app" // the review App itself
	ApproverToken     = "token"      // a bot user's token
)

// Approver tells which of the three approves for g. An instance with no
// approver set (which Validate refuses) counts as a token.
func (g GitHubInstance) Approver() string {
	switch {
	case g.ApproveWithReviewApp:
		return ApproverReviewApp
	case g.ApproveApp != nil:
		return ApproverApp
	}
	return ApproverToken
}

// App is a GitHub App: its numeric ID and the env var holding its PEM
// private key.
type App struct {
	AppID         int64  `yaml:"app_id"`
	PrivateKeyEnv string `yaml:"private_key_env"`
}

// The bounds of a repository's re-run caps.
const (
	MinRerunLimit = 1
	MaxRerunLimit = 1000
)

// The bounds of the step limit (pergent's --max-turns), and its default.
const (
	MinMaxTurns     = 5
	MaxMaxTurns     = 200
	DefaultMaxTurns = 40
)

// Repository is a project that is reviewed. Listing it switches it on. It
// names exactly one of GitLab (an instance under gitlab:) and GitHub (an
// instance under github:); Project is a GitLab path or a GitHub owner/name.
//
// The per-repository settings left nil take their defaults where they are
// resolved (settings.Resolver): approval, draft reviews and inline findings on, the Job
// timeout of jobs.timeout, the step limit of jobs.max_turns and the memory
// limit of jobs.resources.limits.
type Repository struct {
	GitLab  string
	GitHub  string
	Project string
	// Skills names library skills (nil: the default list). RepoSkills names
	// the detected repository skills to turn on (nil: none).
	Skills, RepoSkills []string
	FailureStatus      FailureStatus
	// Approve is whether a clean review is approved (default true).
	Approve *bool
	// ReviewDrafts is whether a draft is reviewed (default true).
	ReviewDrafts *bool
	// InlineFindings is whether findings are posted as threads on their
	// lines (default true).
	InlineFindings *bool
	// CommandsAuthorAndMaintainersOnly is whether only a change's author and
	// the repository's maintainers and owners can use pergent commands on it
	// (default true).
	CommandsAuthorAndMaintainersOnly *bool
	// IncrementalReviews is whether a push reviews only the files changed
	// since the last review (default true).
	IncrementalReviews *bool
	// LearnFromDismissals is whether later reviews are told what people with
	// write access dismissed, and why (default true).
	LearnFromDismissals *bool
	// QuietReviews is whether a review says less: the model is told to leave
	// out nitpicks and the comment is shorter (default false).
	QuietReviews *bool
	// CheckDeleted is whether a review also reads the lines a change deletes
	// and checks what still uses deleted files and renamed files' old paths
	// (default false).
	CheckDeleted *bool
	// CommentLayout is the shape of the review comment, "block:state" entries
	// in order, checked by render.ParseLayout (nil: the default layout).
	CommentLayout []string
	// LinkedReposEnabled is whether the impact check across linked
	// repositories is on (default false). While it is off the links are kept
	// and ignored.
	LinkedReposEnabled *bool
	// LinkedRepos are the repository's links (GitHub only, at most 3), checked
	// by links.Validate. nil and an empty list both mean no links.
	LinkedRepos []store.Link
	// JobTimeout replaces jobs.timeout for this repository.
	JobTimeout *time.Duration
	// JobMemory replaces the memory limit of jobs.resources.limits.
	JobMemory *resource.Quantity
	// MaxTurns replaces jobs.max_turns for this repository (5 to 200).
	MaxTurns *int
	// RerunLimitPerMR and RerunLimitPerRepo cap the re-runs asked for per
	// merge request and per repository and day (1 to 1000).
	RerunLimitPerMR, RerunLimitPerRepo *int
	// TrustedAuthorsOnly is TrustOn or TrustOff (nil: the instance's).
	TrustedAuthorsOnly *string
	// TrustedAuthors is the repository's allowlist, matched by ID.
	TrustedAuthors []store.TrustedAuthor
	// MonthlyTokenQuota is the repository's token budget per UTC calendar
	// month, from 1 to MaxMonthlyTokenQuota (nil: none of its own).
	MonthlyTokenQuota *int64
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
	// WorkSize limits a review Job's work volume, which holds the checkout.
	WorkSize resource.Quantity
	// MaxTurns is pergent's step limit per opencode run (--max-turns), from
	// 5 to 200.
	MaxTurns int
}

// fileConfig mirrors Config for decoding: Jobs needs string durations and
// quantities, and pointers tell an omitted number from an explicit 0.
type fileConfig struct {
	Listen        string           `yaml:"listen"`
	MetricsListen string           `yaml:"metrics_listen"`
	PublicURL     string           `yaml:"public_url"`
	CallbackURL   string           `yaml:"callback_url"`
	GitLab        []Instance       `yaml:"gitlab"`
	GitHub        []GitHubInstance `yaml:"github"`
	Repositories  []fileRepository `yaml:"repositories"`
	LLM           LLM              `yaml:"llm"`
	Jobs          fileJobs         `yaml:"jobs"`
	Dashboard     Dashboard        `yaml:"dashboard"`

	// SkillsDir is kept only to refuse it: the skill library lives in the
	// dashboard now, not in a directory.
	SkillsDir yaml.Node `yaml:"skills_dir"`
}

type fileRepository struct {
	GitLab         string        `yaml:"gitlab"`
	GitHub         string        `yaml:"github"`
	Project        string        `yaml:"project"`
	Skills         []string      `yaml:"skills"`
	RepoSkills     []string      `yaml:"repo_skills"`
	FailureStatus  FailureStatus `yaml:"failure_status"`
	Approve        *bool         `yaml:"approve"`
	ReviewDrafts   *bool         `yaml:"review_drafts"`
	InlineFindings *bool         `yaml:"inline_findings"`
	JobTimeout     *duration     `yaml:"job_timeout"`
	JobMemory      *quantity     `yaml:"job_memory"`
	MaxTurns       *int          `yaml:"max_turns"`

	RerunLimitPerMR   *int `yaml:"rerun_limit_per_mr"`
	RerunLimitPerRepo *int `yaml:"rerun_limit_per_repo"`

	CommandsAuthorAndMaintainersOnly *bool `yaml:"commands_author_and_maintainers_only"`
	IncrementalReviews               *bool `yaml:"incremental_reviews"`
	LearnFromDismissals              *bool `yaml:"learn_from_dismissals"`
	QuietReviews                     *bool `yaml:"quiet_reviews"`
	CheckDeleted                     *bool `yaml:"check_deleted"`

	CommentLayout LayoutEntries `yaml:"comment_layout"`

	LinkedReposEnabled *bool      `yaml:"linked_repos_enabled"`
	LinkedRepos        []fileLink `yaml:"linked_repos"`

	TrustedAuthorsOnly *string               `yaml:"trusted_authors_only"`
	TrustedAuthors     []store.TrustedAuthor `yaml:"trusted_authors"`

	MonthlyTokenQuota *int64 `yaml:"monthly_token_quota"`
}

// fileLink is one entry of linked_repos. can_block is true when left out.
type fileLink struct {
	Target   string   `yaml:"target"`
	Watched  []string `yaml:"watched"`
	CanBlock *bool    `yaml:"can_block"`
}

func (f fileRepository) repository() Repository {
	r := Repository{
		GitLab: f.GitLab, GitHub: f.GitHub, Project: f.Project, Skills: f.Skills, RepoSkills: f.RepoSkills,
		FailureStatus: f.FailureStatus, Approve: f.Approve, ReviewDrafts: f.ReviewDrafts,
		InlineFindings:  f.InlineFindings,
		RerunLimitPerMR: f.RerunLimitPerMR, RerunLimitPerRepo: f.RerunLimitPerRepo,
		MaxTurns: f.MaxTurns,

		CommandsAuthorAndMaintainersOnly: f.CommandsAuthorAndMaintainersOnly,
		IncrementalReviews:               f.IncrementalReviews,
		LearnFromDismissals:              f.LearnFromDismissals,
		QuietReviews:                     f.QuietReviews,
		CheckDeleted:                     f.CheckDeleted,
		CommentLayout:                    []string(f.CommentLayout),
		LinkedReposEnabled:               f.LinkedReposEnabled,
		TrustedAuthorsOnly:               f.TrustedAuthorsOnly,
		TrustedAuthors:                   f.TrustedAuthors,
		MonthlyTokenQuota:                f.MonthlyTokenQuota,
	}
	if f.LinkedRepos != nil {
		r.LinkedRepos = make([]store.Link, 0, len(f.LinkedRepos))
		for _, l := range f.LinkedRepos {
			r.LinkedRepos = append(r.LinkedRepos, store.Link{Target: l.Target, Watched: l.Watched, CanBlock: l.CanBlock == nil || *l.CanBlock})
		}
	}
	if f.JobTimeout != nil {
		d := time.Duration(*f.JobTimeout)
		r.JobTimeout = &d
	}
	if f.JobMemory != nil {
		q := f.JobMemory.Quantity
		r.JobMemory = &q
	}
	return r
}

type fileJobs struct {
	Namespace   string        `yaml:"namespace"`
	Image       string        `yaml:"image"`
	MaxRunning  *int          `yaml:"max_running"`
	Timeout     *duration     `yaml:"timeout"`
	MaxTurns    *int          `yaml:"max_turns"`
	Concurrency *int          `yaml:"concurrency"`
	Resources   fileResources `yaml:"resources"`
	WorkSize    *quantity     `yaml:"work_size"`
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

// LayoutEntries is the comment_layout list: each entry is a one-key map such
// as `- findings: collapsed`, held as "findings:collapsed". A list that is
// left out stays nil; one written out as `[]` is empty, which Validate
// refuses.
type LayoutEntries []string

func (l *LayoutEntries) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: comment_layout must be a list of entries such as `- findings: collapsed`", value.Line)
	}
	out := make(LayoutEntries, 0, len(value.Content))
	for _, n := range value.Content {
		if n.Kind != yaml.MappingNode || len(n.Content) != 2 || n.Content[0].Kind != yaml.ScalarNode || n.Content[1].Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: a comment_layout entry must be one block and its state, such as `- findings: collapsed`", n.Line)
		}
		out = append(out, n.Content[0].Value+":"+n.Content[1].Value)
	}
	*l = out
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
		Listen:        raw.Listen,
		MetricsListen: raw.MetricsListen,
		PublicURL:     raw.PublicURL,
		CallbackURL:   raw.CallbackURL,
		GitLab:        raw.GitLab,
		GitHub:        raw.GitHub,
		Repositories:  make([]Repository, 0, len(raw.Repositories)),
		LLM:           raw.LLM,
		Dashboard:     raw.Dashboard,
		Jobs: Jobs{
			Namespace:   raw.Jobs.Namespace,
			Image:       raw.Jobs.Image,
			MaxRunning:  defaultMaxRunning,
			Timeout:     defaultTimeout,
			MaxTurns:    DefaultMaxTurns,
			Concurrency: defaultConcurrency,
			Resources: corev1.ResourceRequirements{
				Requests: toResourceList(raw.Jobs.Resources.Requests),
				Limits:   toResourceList(raw.Jobs.Resources.Limits),
			},
		},
	}
	for _, repo := range raw.Repositories {
		cfg.Repositories = append(cfg.Repositories, repo.repository())
	}
	if raw.Jobs.WorkSize != nil {
		cfg.Jobs.WorkSize = raw.Jobs.WorkSize.Quantity
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
	if raw.Jobs.MaxTurns != nil {
		cfg.Jobs.MaxTurns = *raw.Jobs.MaxTurns
	}

	cfg.applyDefaults()
	err = cfg.Validate()
	if !raw.SkillsDir.IsZero() {
		err = errors.Join(errors.New(`skills_dir was removed: skills live in the dashboard (see the README, "Skills and instructions")`), err)
	}
	if err != nil {
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
	// No llm block means no LLM yet (the dashboard adds it), not a default one.
	if c.HasLLM() && c.LLM.APIKeyEnv == "" {
		c.LLM.APIKeyEnv = defaultAPIKeyEnv
	}
	if c.Jobs.Image == "" {
		c.Jobs.Image = defaultImage
	}
	if len(c.Jobs.Resources.Requests) == 0 && len(c.Jobs.Resources.Limits) == 0 {
		c.Jobs.Resources = defaultResources()
	}
	if c.Jobs.WorkSize.IsZero() {
		c.Jobs.WorkSize = resource.MustParse(defaultWorkSize)
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
	envNameRE      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	digestRE       = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	// skillNameRE is the rule for a skill's name (inputs holds the same rule).
	skillNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Validate checks the config and returns every problem found, joined.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if err := checkURL("public_url", c.PublicURL, true); err != nil {
		errs = append(errs, err)
	}
	if err := checkMetricsListen(c.MetricsListen, c.Listen); err != nil {
		errs = append(errs, err)
	}
	if c.CallbackURL != "" {
		if err := checkURL("callback_url", c.CallbackURL, false); err != nil {
			errs = append(errs, err)
		}
	}

	if len(c.GitLab)+len(c.GitHub) == 0 && !c.DashboardEnabled() {
		add("at least one gitlab or github instance is required (or turn on the dashboard to add them there)")
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
			{"webhook_secret_env", inst.WebhookSecretEnv},
		} {
			if f.val == "" {
				add("gitlab[%d] (%s): %s is required", i, inst.Name, f.field)
			}
		}
		if (inst.ApproveTokenEnv != "") == inst.ApproveWithReviewBot {
			add("gitlab[%d] (%s): exactly one of approve_token_env and approve_with_review_bot must be set", i, inst.Name)
		}
		if !validInstanceTrust(inst.TrustedAuthorsOnly) {
			add("gitlab[%d] (%s): trusted_authors_only must be follow_visibility, on or off, got %q", i, inst.Name, inst.TrustedAuthorsOnly)
		}
		if q := inst.MonthlyTokenQuota; q != nil && !ValidTokenQuota(*q) {
			add("gitlab[%d] (%s): monthly_token_quota must be from 1 to %d, got %d", i, inst.Name, MaxMonthlyTokenQuota, *q)
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
		if !validInstanceTrust(inst.TrustedAuthorsOnly) {
			add("github[%d] (%s): trusted_authors_only must be follow_visibility, on or off, got %q", i, inst.Name, inst.TrustedAuthorsOnly)
		}
		if q := inst.MonthlyTokenQuota; q != nil && !ValidTokenQuota(*q) {
			add("github[%d] (%s): monthly_token_quota must be from 1 to %d, got %d", i, inst.Name, MaxMonthlyTokenQuota, *q)
		}
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
		// skills is absent or null (the default list) or a non-empty list: an
		// empty list would silently mean the default list too.
		if repo.Skills != nil && len(repo.Skills) == 0 {
			add("repositories[%d] (%s): skills must be a list of skill names, or left out for the default list", i, repo.Project)
		}
		if m := repo.TrustedAuthorsOnly; m != nil && *m != TrustOn && *m != TrustOff {
			add("repositories[%d] (%s): trusted_authors_only must be on or off, got %q", i, repo.Project, *m)
		}
		if q := repo.MonthlyTokenQuota; q != nil && !ValidTokenQuota(*q) {
			add("repositories[%d] (%s): monthly_token_quota must be from 1 to %d, got %d", i, repo.Project, MaxMonthlyTokenQuota, *q)
		}
		trustedIDs := make(map[int64]bool, len(repo.TrustedAuthors))
		for j, a := range repo.TrustedAuthors {
			switch {
			case a.ID <= 0:
				add("repositories[%d] (%s): trusted_authors[%d]: id is required (the user's numeric ID on the platform)", i, repo.Project, j)
			case trustedIDs[a.ID]:
				add("repositories[%d] (%s): trusted_authors[%d]: id %d is listed twice", i, repo.Project, j, a.ID)
			}
			trustedIDs[a.ID] = true
			if strings.TrimSpace(a.Username) == "" {
				add("repositories[%d] (%s): trusted_authors[%d]: username is required", i, repo.Project, j)
			}
		}
		for _, name := range repo.RepoSkills {
			if !skillNameRE.MatchString(name) {
				add("repositories[%d] (%s): repo_skills name %q must match %s", i, repo.Project, name, skillNameRE)
			}
		}
		if repo.FailureStatus != FailureStatusFailed && repo.FailureStatus != FailureStatusNeutral {
			add("repositories[%d] (%s): failure_status %q must be %q or %q",
				i, repo.Project, repo.FailureStatus, FailureStatusFailed, FailureStatusNeutral)
		}
		if repo.JobTimeout != nil && *repo.JobTimeout <= 0 {
			add("repositories[%d] (%s): job_timeout must be positive, got %s", i, repo.Project, *repo.JobTimeout)
		}
		if repo.JobMemory != nil {
			// A limit below the request makes a Job Kubernetes refuses.
			request := c.Jobs.Resources.Requests[corev1.ResourceMemory]
			switch {
			case repo.JobMemory.Sign() <= 0:
				add("repositories[%d] (%s): job_memory must be positive, got %s", i, repo.Project, repo.JobMemory.String())
			case repo.JobMemory.Cmp(request) < 0:
				add("repositories[%d] (%s): job_memory %s is below the memory request %s of jobs.resources.requests",
					i, repo.Project, repo.JobMemory.String(), request.String())
			}
		}
		// comment_layout is absent (the default layout) or a list that
		// render.ParseLayout accepts; an empty list is refused, not read as absent.
		if repo.CommentLayout != nil {
			if _, err := render.ParseLayout(repo.CommentLayout); err != nil {
				add("repositories[%d] (%s): comment_layout: %v", i, repo.Project, err)
			}
		}
		// linked_repos is GitHub only; its links must pass links.Validate.
		// An empty list written out is allowed and means no links.
		switch {
		case len(repo.LinkedRepos) > 0 && platform != PlatformGitHub:
			add("repositories[%d] (%s): linked_repos is supported on GitHub only", i, repo.Project)
		case len(repo.LinkedRepos) > 0:
			if err := links.Validate(repo.Project, repo.LinkedRepos); err != nil {
				add("repositories[%d] (%s): linked_repos: %v", i, repo.Project, err)
			}
		}
		if n := repo.MaxTurns; n != nil && (*n < MinMaxTurns || *n > MaxMaxTurns) {
			add("repositories[%d] (%s): max_turns must be from %d to %d, got %d", i, repo.Project, MinMaxTurns, MaxMaxTurns, *n)
		}
		if n := repo.RerunLimitPerMR; n != nil && (*n < MinRerunLimit || *n > MaxRerunLimit) {
			add("repositories[%d] (%s): rerun_limit_per_mr must be from %d to %d, got %d", i, repo.Project, MinRerunLimit, MaxRerunLimit, *n)
		}
		if n := repo.RerunLimitPerRepo; n != nil && (*n < MinRerunLimit || *n > MaxRerunLimit) {
			add("repositories[%d] (%s): rerun_limit_per_repo must be from %d to %d, got %d", i, repo.Project, MinRerunLimit, MaxRerunLimit, *n)
		}
	}

	errs = append(errs, c.checkDashboard()...)

	// With the dashboard on the LLM can be added there, so an absent llm
	// block is fine; one that is present must be complete.
	if c.HasLLM() || !c.DashboardEnabled() {
		if c.LLM.Provider == "" {
			add("llm.provider is required")
		}
		if c.LLM.Model == "" {
			add("llm.model is required")
		}
		if c.LLM.APIKeyEnv == "" {
			add("llm.api_key_env is required")
		}
	}

	if c.Jobs.Namespace == "" {
		add("jobs.namespace is required")
	}
	if err := checkImagePinned(c.Jobs.Image); err != nil {
		errs = append(errs, err)
	}
	if c.Jobs.WorkSize.Sign() < 0 {
		add("jobs.work_size must not be negative, got %s", c.Jobs.WorkSize.String())
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
	if c.Jobs.MaxTurns < MinMaxTurns || c.Jobs.MaxTurns > MaxMaxTurns {
		add("jobs.max_turns must be from %d to %d, got %d", MinMaxTurns, MaxMaxTurns, c.Jobs.MaxTurns)
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

	set := 0
	for _, on := range []bool{inst.ApproveApp != nil, inst.ApproveTokenEnv != "", inst.ApproveWithReviewApp} {
		if on {
			set++
		}
	}
	switch {
	case set > 1:
		add("%s: exactly one of approve_app, approve_token_env and approve_with_review_app must be set, got %d", prefix, set)
	case set == 0:
		add("%s: exactly one of approve_app, approve_token_env and approve_with_review_app must be set, got none", prefix)
	case inst.ApproveApp != nil:
		checkApp("approve_app", *inst.ApproveApp)
		// The same App, or the same key, would review and approve: say
		// so, and name the way to ask for that.
		const useReviewApp = "; to approve with the review App, set approve_with_review_app: true"
		if inst.ApproveApp.AppID > 0 && inst.ApproveApp.AppID == inst.ReviewApp.AppID {
			add("%s: approve_app.app_id must differ from review_app.app_id, got %d for both%s", prefix, inst.ReviewApp.AppID, useReviewApp)
		}
		if inst.ApproveApp.PrivateKeyEnv != "" && inst.ApproveApp.PrivateKeyEnv == inst.ReviewApp.PrivateKeyEnv {
			add("%s: approve_app.private_key_env must differ from review_app.private_key_env, got %q for both%s", prefix, inst.ReviewApp.PrivateKeyEnv, useReviewApp)
		}
	}

	if inst.WebhookSecretEnv == "" {
		add("%s: webhook_secret_env is required", prefix)
	}
	return errs
}

// checkMetricsListen refuses a metrics_listen that is not host:port or that
// the main server already listens on. Empty is off. A port both addresses
// share is refused when their hosts are the same or either one is every
// address, because the second bind would fail at start-up.
func checkMetricsListen(addr, listen string) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("metrics_listen %q must be host:port or :port: %w", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("metrics_listen %q must have a port from 1 to 65535, got %q", addr, port)
	}
	mainHost, mainPort, err := net.SplitHostPort(listen)
	if err != nil {
		// listen is not checked here; the same text is the most that can be said.
		if addr == listen {
			return fmt.Errorf("metrics_listen %q must differ from listen", addr)
		}
		return nil
	}
	if port == mainPort && (host == mainHost || host == "" || mainHost == "") {
		return fmt.Errorf("metrics_listen %q must differ from listen %q: the metrics port serves nothing else", addr, listen)
	}
	return nil
}

// checkDashboard returns every problem with dashboard:. (The instances'
// OAuth env names are no longer checked: they only warn, see
// DeprecatedWarnings.)
func (c *Config) checkDashboard() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	d := c.Dashboard
	if d.AdminPasswordEnv != "" && !envNameRE.MatchString(d.AdminPasswordEnv) {
		add("dashboard.admin_password_env %q must be an environment variable name (%s)", d.AdminPasswordEnv, envNameRE)
	}
	if !d.Admins.IsZero() {
		add(`dashboard.admins was removed: admins are now teams in the dashboard (see the README, "Teams")`)
	}
	for _, p := range d.SignIn.Problems(true) {
		add("dashboard.sign_in: %s", p)
	}
	if d.SignIn.Set() && d.URL == "" {
		add("dashboard.sign_in needs dashboard.url: the sign-in routes are part of the dashboard")
	}
	if d.URL == "" {
		return errs
	}
	if err := checkURL("dashboard.url", d.URL, true); err != nil {
		errs = append(errs, err)
	} else {
		u, _ := url.Parse(d.URL)
		if u.User != nil {
			add("dashboard.url must not contain credentials")
		}
		// The dashboard, its API, the OAuth callbacks and its __Host-
		// cookies all live at / of the host.
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			add("dashboard.url %q must have no path, query or fragment: the dashboard is served at / of its host", d.URL)
		}
		// The chart's dashboard Ingress serves a host on 443; a URL with a
		// port matches none of its hosts.
		if u.Port() != "" {
			add("dashboard.url %q must have no port: the dashboard Ingress serves its host on the https port", d.URL)
		}
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
// instance. GitLab paths are case-insensitive. GitHub repositories are not
// found here.
func (c *Config) Repository(instance, project string) (Repository, bool) {
	for _, repo := range c.Repositories {
		if repo.GitLab == instance && strings.EqualFold(repo.Project, project) {
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
	if !i.ApproveWithReviewBot {
		if approve, err = read(i.ApproveTokenEnv); err != nil {
			return "", "", "", err
		}
	}
	if webhook, err = read(i.WebhookSecretEnv); err != nil {
		return "", "", "", err
	}
	return review, approve, webhook, nil
}

// HasOAuth reports whether the instance is set up for dashboard sign-in.
func (i Instance) HasOAuth() bool { return i.OAuthClientIDEnv != "" && i.OAuthClientSecretEnv != "" }

// OAuthClient reads the instance's OAuth application ID and secret from the
// environment. An unset or empty variable is an error that names it, never
// a value.
func (i Instance) OAuthClient() (id, secret string, err error) {
	return readOAuthClient("gitlab", i.Name, i.OAuthClientIDEnv, i.OAuthClientSecretEnv)
}

// HasOAuth reports whether the instance is set up for dashboard sign-in.
func (g GitHubInstance) HasOAuth() bool {
	return g.OAuthClientIDEnv != "" && g.OAuthClientSecretEnv != ""
}

// OAuthClient reads the review App's client ID and secret from the
// environment. An unset or empty variable is an error that names it, never
// a value.
func (g GitHubInstance) OAuthClient() (id, secret string, err error) {
	return readOAuthClient("github", g.Name, g.OAuthClientIDEnv, g.OAuthClientSecretEnv)
}

func readOAuthClient(platform, name, idEnv, secretEnv string) (string, string, error) {
	read := func(env string) (string, error) {
		if env == "" {
			return "", fmt.Errorf("%s instance %q: oauth env var name is empty", platform, name)
		}
		v := os.Getenv(env)
		if v == "" {
			return "", fmt.Errorf("%s instance %q: env var %s is not set or empty", platform, name, env)
		}
		return v, nil
	}
	id, err := read(idEnv)
	if err != nil {
		return "", "", err
	}
	secret, err := read(secretEnv)
	if err != nil {
		return "", "", err
	}
	return id, secret, nil
}

// Secrets reads the instance's review App private key, approver secret (the
// approve App's private key, the approve token, or the review key again when
// the review App approves) and webhook secret from the environment. An unset
// or empty variable is an error that names it, never its value.
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
	case (g.ApproveApp != nil && g.ApproveTokenEnv != "") || (g.ApproveWithReviewApp && (g.ApproveApp != nil || g.ApproveTokenEnv != "")):
		return "", "", "", fmt.Errorf("github instance %q: more than one of approve_app, approve_token_env and approve_with_review_app is set", g.Name)
	case g.ApproveWithReviewApp:
		approveEnv = g.ReviewApp.PrivateKeyEnv
	case g.ApproveApp != nil:
		approveEnv = g.ApproveApp.PrivateKeyEnv
	case g.ApproveTokenEnv == "":
		return "", "", "", fmt.Errorf("github instance %q: none of approve_app, approve_token_env and approve_with_review_app is set", g.Name)
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

// SecondsPerStep is how long pergent.sh allows each step of an opencode
// run when it checks that a Job timeout fits the step limit.
const SecondsPerStep = 30

// TimeoutWarnings returns one message per timeout in c shorter than its
// step limit times SecondsPerStep: jobs.timeout against jobs.max_turns,
// and each repository's effective job_timeout against its effective
// max_turns. A repository that overrides neither is covered by the jobs
// message. Empty when every timeout fits.
func TimeoutWarnings(c *Config) []string {
	var out []string
	if need := time.Duration(c.Jobs.MaxTurns) * SecondsPerStep * time.Second; c.Jobs.Timeout < need {
		out = append(out, fmt.Sprintf("jobs.timeout %s is short for jobs.max_turns %d: allow about %s (30s a step)",
			c.Jobs.Timeout, c.Jobs.MaxTurns, need))
	}
	for i, repo := range c.Repositories {
		if repo.JobTimeout == nil && repo.MaxTurns == nil {
			continue
		}
		timeout, turns := c.Jobs.Timeout, c.Jobs.MaxTurns
		if repo.JobTimeout != nil {
			timeout = *repo.JobTimeout
		}
		if repo.MaxTurns != nil {
			turns = *repo.MaxTurns
		}
		if need := time.Duration(turns) * SecondsPerStep * time.Second; timeout < need {
			out = append(out, fmt.Sprintf("repositories[%d] (%s): job_timeout %s is short for max_turns %d: allow about %s (30s a step)",
				i, repo.Project, timeout, turns, need))
		}
	}
	return out
}

// WebhookURL is where platform sends instance's webhooks.
func WebhookURL(publicURL, platform, instance string) string {
	return strings.TrimRight(publicURL, "/") + "/webhooks/" + platform + "/" + instance
}

// ValidTokenQuota reports whether q is a usable monthly token quota: from 1 to
// MaxMonthlyTokenQuota. The config, the settings and the dashboard share it.
func ValidTokenQuota(q int64) bool {
	return q >= 1 && q <= MaxMonthlyTokenQuota
}

// validInstanceTrust reports whether m is an instance's trusted_authors_only:
// empty (follow visibility) or one of the three values.
func validInstanceTrust(m string) bool {
	return m == "" || m == TrustFollowVisibility || m == TrustOn || m == TrustOff
}
