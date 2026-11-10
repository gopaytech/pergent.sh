// Package settings resolves a repository's effective settings: those of the
// config file when it lists the repository, otherwise those stored in the
// dashboard, with every setting left unset taking its default.
//
// Config wins: a repository listed in the config is never read from the
// dashboard, whatever the dashboard holds for it. Projects are matched
// ignoring case, on both platforms.
package settings

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Source tells where a repository is set up.
type Source string

const (
	SourceConfig    Source = "config"
	SourceDashboard Source = "dashboard"
)

// Setting names, the keys of Effective.Overridden.
const (
	KeyApprove        = "approve"
	KeyReviewDrafts   = "review_drafts"
	KeyInlineFindings = "inline_findings"
	KeyFailureStatus  = "failure_status"
	KeySkills         = "skills"
	KeyRepoSkills     = "repo_skills"
	KeyJobTimeout     = "job_timeout"
	KeyMaxTurns       = "max_turns"
	KeyJobMemory      = "job_memory"
	KeyModel          = "model"

	KeyRerunLimitPerMR   = "rerun_limit_per_mr"
	KeyRerunLimitPerRepo = "rerun_limit_per_repo"

	KeyCommandsAuthorAndMaintainersOnly = "commands_author_and_maintainers_only"
	KeyIncrementalReviews               = "incremental_reviews"
	KeyLearnFromDismissals              = "learn_from_dismissals"
	KeyQuietReviews                     = "quiet_reviews"
	KeyCommentLayout                    = "comment_layout"
	KeyLinkedReposEnabled               = "linked_repos_enabled"
	KeyTrustedAuthorsOnly               = "trusted_authors_only"
	KeyTrustedAuthors                   = "trusted_authors"
	KeyMonthlyTokenQuota                = "monthly_token_quota"
)

// The re-run caps when nothing sets them: per merge request and per
// repository, each per UTC day.
const (
	DefaultRerunLimitPerMR   = 10
	DefaultRerunLimitPerRepo = 50
)

// Effective is a repository switched on, with every setting resolved.
type Effective struct {
	// Platform is config.PlatformGitLab or config.PlatformGitHub.
	Platform string
	// Project is spelled as the config or the dashboard has it.
	Instance, Project string
	Source            Source
	Approve           bool
	ReviewDrafts      bool
	InlineFindings    bool // findings are posted as threads on their lines, not only in the review comment
	// CommandsAuthorAndMaintainersOnly is whether only a change's author and
	// the repository's maintainers and owners can use pergent commands on it.
	CommandsAuthorAndMaintainersOnly bool
	// IncrementalReviews is whether a push reviews only the files changed
	// since the last review.
	IncrementalReviews bool
	// LearnFromDismissals is whether later reviews are told what people with
	// write access dismissed, and why.
	LearnFromDismissals bool
	// QuietReviews is whether a review says less. The default is false.
	QuietReviews bool
	// CommentLayout is the shape of the review comment, "block:state" entries
	// in order; nil means the default layout.
	CommentLayout []string
	// LinkedReposEnabled is whether the impact check across linked
	// repositories is on. The default is false: while it is off the links
	// are kept and ignored.
	LinkedReposEnabled bool
	// LinkedRepos are the repository's own links, whether or not the check is
	// on; there is no default list. Nothing here has checked them against
	// the rules: that is links.Validate, at review time.
	LinkedRepos   []store.Link
	FailureStatus config.FailureStatus
	// Skills are the library skills reviewed; nil means the default list.
	Skills []string
	// RepoSkills are the detected repository skills turned on; nil is none.
	RepoSkills []string
	JobTimeout time.Duration
	// MaxTurns is the step limit of each opencode run, from
	// config.MinMaxTurns to config.MaxMaxTurns.
	MaxTurns int
	// JobMemory is the Job's memory limit; zero when neither the
	// repository nor jobs.resources.limits sets one.
	JobMemory resource.Quantity
	// ModelID is the entry of the list of models the repository runs on;
	// empty means it follows the default. Whether the entry still exists
	// and has a key is the caller's to check at review time.
	ModelID string
	// RerunLimitPerMR and RerunLimitPerRepo cap the re-runs asked for (by
	// comment or button) per merge request and per repository and UTC day.
	RerunLimitPerMR, RerunLimitPerRepo int
	// TrustedAuthorsOnly is the resolved trusted-authors mode: the
	// repository's config.TrustOn or config.TrustOff when it sets one, else
	// its instance's (config.TrustFollowVisibility when the instance is
	// unknown). Whether a follow_visibility repository is public is read at
	// review time.
	TrustedAuthorsOnly string
	// InstanceTrustedAuthorsOnly is the instance's mode, which the
	// repository follows unless it sets its own.
	InstanceTrustedAuthorsOnly string
	// TrustedAuthors is the repository's allowlist, matched by user ID.
	TrustedAuthors []store.TrustedAuthor
	// MonthlyTokenQuota is the repository's own token budget per UTC
	// calendar month, from 1 to config.MaxMonthlyTokenQuota; nil means it has
	// none (an unusable stored value counts as none).
	MonthlyTokenQuota *int64
	// InstanceMonthlyTokenQuota is the instance's budget; nil when it has none
	// or is not registered.
	InstanceMonthlyTokenQuota *int64
	// Overridden tells, for each setting key, whether the repository sets
	// it rather than taking the default. Every key is present.
	Overridden map[string]bool
}

// Resolver resolves repositories from Config and, when Store is not nil, the
// dashboard's repositories in Store. Instances tells the platform of the
// instance a repository is on, whichever source the instance is from; nil
// means the config's instances alone.
type Resolver struct {
	Config    *config.Config
	Store     *store.Store
	Instances *instances.Registry
}

// registry is Instances, or the config's instances alone when it is nil.
func (r *Resolver) registry() *instances.Registry {
	if r.Instances != nil {
		return r.Instances
	}
	return instances.New(r.Config, nil, nil)
}

// List returns every repository switched on: the config's first, in config
// order, then the dashboard's that the config doesn't list, ordered by
// instance and project. A dashboard row of an instance that isn't
// registered is left out.
func (r *Resolver) List(ctx context.Context) ([]Effective, error) {
	out := make([]Effective, 0, len(r.Config.Repositories))
	for _, repo := range r.Config.Repositories {
		eff, ok := r.fromConfig(repo)
		if ok {
			out = append(out, eff)
		}
	}
	if r.Store == nil {
		return out, nil
	}
	rows, err := r.Store.DashboardRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("list dashboard repositories: %w", err)
	}
	stored, err := r.Store.RepoDefaults(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the defaults: %w", err)
	}
	for _, row := range rows {
		if r.InConfig(row.Instance, row.Project) {
			continue
		}
		if eff, ok := r.fromDashboard(row, stored); ok {
			out = append(out, eff)
		}
	}
	return out, nil
}

// Get returns project on instance, matched ignoring case: the config's
// repository if the config lists it, otherwise the dashboard's. ok is false
// when it is switched on in neither.
func (r *Resolver) Get(ctx context.Context, instance, project string) (Effective, bool, error) {
	if repo, ok := r.configRepository(instance, project); ok {
		eff, ok := r.fromConfig(repo)
		return eff, ok, nil
	}
	if r.Store == nil {
		return Effective{}, false, nil
	}
	row, found, err := r.Store.DashboardRepo(ctx, instance, project)
	if err != nil {
		return Effective{}, false, fmt.Errorf("load dashboard repository %s on %s: %w", project, instance, err)
	}
	if !found {
		return Effective{}, false, nil
	}
	stored, err := r.Store.RepoDefaults(ctx)
	if err != nil {
		return Effective{}, false, fmt.Errorf("read the defaults: %w", err)
	}
	eff, ok := r.fromDashboard(row, stored)
	return eff, ok, nil
}

// InConfig reports whether the config lists project (ignoring case) on
// instance. Such a repository is managed by config, not the dashboard.
func (r *Resolver) InConfig(instance, project string) bool {
	_, ok := r.configRepository(instance, project)
	return ok
}

func (r *Resolver) configRepository(instance, project string) (config.Repository, bool) {
	for _, repo := range r.Config.Repositories {
		if repo.Instance() == instance && strings.EqualFold(repo.Project, project) {
			return repo, true
		}
	}
	return config.Repository{}, false
}

// Defaults is what a repository of the config on platform and instance gets
// when it sets nothing: every setting at its default, none overridden, no
// project.
func (r *Resolver) Defaults(platform, instance string) Effective {
	return r.defaults(platform, instance, "", "")
}

// BrowserDefaults is what a repository switched on in the dashboard on
// platform and instance gets when it sets nothing: the built-in and config
// defaults, under those an operator set in the dashboard.
func (r *Resolver) BrowserDefaults(ctx context.Context, platform, instance string) (Effective, error) {
	var stored store.RepoDefaults
	if r.Store != nil {
		var err error
		if stored, err = r.Store.RepoDefaults(ctx); err != nil {
			return Effective{}, fmt.Errorf("read the defaults: %w", err)
		}
	}
	return r.defaultsFor(SourceDashboard, platform, instance, "", stored), nil
}

// defaults is a repository of platform on instance with every setting at
// the config's default and none overridden.
func (r *Resolver) defaults(platform, instance, project string, source Source) Effective {
	return r.defaultsFor(source, platform, instance, project, store.RepoDefaults{})
}

// defaultsFor is defaults for a repository from source: for SourceDashboard,
// the defaults stored in the dashboard replace the built-in and config ones.
// A stored value that can't be used (an unknown failure status, a timeout
// that isn't positive, a step limit outside 5 to 200, a re-run cap that
// isn't positive, a memory limit that doesn't parse, isn't positive or is
// below jobs.resources.requests, which Kubernetes would refuse) is ignored:
// a bad setting never stops reviews.
func (r *Resolver) defaultsFor(source Source, platform, instance, project string, stored store.RepoDefaults) Effective {
	eff := Effective{
		Platform: platform, Instance: instance, Project: project, Source: source,
		Approve:        true,
		ReviewDrafts:   true,
		InlineFindings: true,
		FailureStatus:  config.FailureStatusFailed,

		CommandsAuthorAndMaintainersOnly: true,
		IncrementalReviews:               true,
		LearnFromDismissals:              true,
		JobTimeout:                       r.Config.Jobs.Timeout,
		MaxTurns:                         r.Config.Jobs.MaxTurns,
		JobMemory:                        r.jobsMemory(r.Config.Jobs.Resources.Limits),

		RerunLimitPerMR:            DefaultRerunLimitPerMR,
		RerunLimitPerRepo:          DefaultRerunLimitPerRepo,
		TrustedAuthorsOnly:         r.instanceTrust(instance),
		InstanceTrustedAuthorsOnly: r.instanceTrust(instance),
		InstanceMonthlyTokenQuota:  r.instanceQuota(instance),
		Overridden: map[string]bool{
			KeyTrustedAuthorsOnly: false, KeyTrustedAuthors: false, KeyMonthlyTokenQuota: false,
			KeyApprove: false, KeyReviewDrafts: false, KeyInlineFindings: false, KeyFailureStatus: false,
			KeySkills: false, KeyRepoSkills: false, KeyJobTimeout: false, KeyMaxTurns: false, KeyJobMemory: false, KeyModel: false,
			KeyRerunLimitPerMR: false, KeyRerunLimitPerRepo: false, KeyCommandsAuthorAndMaintainersOnly: false, KeyIncrementalReviews: false, KeyLearnFromDismissals: false, KeyQuietReviews: false, KeyCommentLayout: false, KeyLinkedReposEnabled: false,
		},
	}
	if source != SourceDashboard {
		return eff
	}
	if stored.Approve != nil {
		eff.Approve = *stored.Approve
	}
	if stored.ReviewDrafts != nil {
		eff.ReviewDrafts = *stored.ReviewDrafts
	}
	if stored.InlineFindings != nil {
		eff.InlineFindings = *stored.InlineFindings
	}
	if stored.CommandsAuthorAndMaintainersOnly != nil {
		eff.CommandsAuthorAndMaintainersOnly = *stored.CommandsAuthorAndMaintainersOnly
	}
	if stored.IncrementalReviews != nil {
		eff.IncrementalReviews = *stored.IncrementalReviews
	}
	if stored.LearnFromDismissals != nil {
		eff.LearnFromDismissals = *stored.LearnFromDismissals
	}
	if stored.QuietReviews != nil {
		eff.QuietReviews = *stored.QuietReviews
	}
	if len(stored.CommentLayout) > 0 {
		eff.CommentLayout = slices.Clone(stored.CommentLayout)
	}
	if stored.LinkedReposEnabled != nil {
		eff.LinkedReposEnabled = *stored.LinkedReposEnabled
	}
	if stored.FailureStatus != nil {
		if fs, ok := failureStatus(*stored.FailureStatus); ok {
			eff.FailureStatus = fs
		}
	}
	if stored.JobTimeoutSeconds != nil && *stored.JobTimeoutSeconds > 0 {
		eff.JobTimeout = time.Duration(*stored.JobTimeoutSeconds) * time.Second
	}
	if usableMaxTurns(stored.MaxTurns) {
		eff.MaxTurns = *stored.MaxTurns
	}
	if stored.JobMemory != nil {
		if q, ok := r.usableMemory(*stored.JobMemory); ok {
			eff.JobMemory = q
		}
	}
	if usableCap(stored.RerunLimitPerMR) {
		eff.RerunLimitPerMR = *stored.RerunLimitPerMR
	}
	if usableCap(stored.RerunLimitPerRepo) {
		eff.RerunLimitPerRepo = *stored.RerunLimitPerRepo
	}
	return eff
}

// usableMaxTurns is whether a stored step limit is set and from
// config.MinMaxTurns to config.MaxMaxTurns.
func usableMaxTurns(n *int) bool {
	return n != nil && *n >= config.MinMaxTurns && *n <= config.MaxMaxTurns
}

// usableCap is whether a stored re-run cap is set and positive.
func usableCap(n *int) bool { return n != nil && *n > 0 }

// failureStatus is s as a config.FailureStatus; ok is false for an unknown
// one.
func failureStatus(s string) (config.FailureStatus, bool) {
	switch fs := config.FailureStatus(s); fs {
	case config.FailureStatusFailed, config.FailureStatusNeutral:
		return fs, true
	}
	return "", false
}

// usableMemory parses a stored memory limit; ok is false when it doesn't
// parse, isn't positive or is below jobs.resources.requests.
func (r *Resolver) usableMemory(s string) (resource.Quantity, bool) {
	request := r.jobsMemory(r.Config.Jobs.Resources.Requests)
	q, err := resource.ParseQuantity(s)
	if err != nil || q.Sign() <= 0 || q.Cmp(request) < 0 {
		return resource.Quantity{}, false
	}
	return q, true
}

// cloneLinks copies a repository's links, so no caller changes the stored or
// configured ones; an empty list is nil.
func cloneLinks(ls []store.Link) []store.Link {
	if len(ls) == 0 {
		return nil
	}
	out := make([]store.Link, len(ls))
	for i, l := range ls {
		out[i] = l
		out[i].Watched = slices.Clone(l.Watched)
	}
	return out
}

// jobsMemory is the memory entry of list (jobs.resources), copied; zero
// when there is none.
func (r *Resolver) jobsMemory(list corev1.ResourceList) resource.Quantity {
	q := list[corev1.ResourceMemory]
	return q.DeepCopy()
}

// instanceTrust is the trusted-authors mode of the instance called name;
// config.TrustFollowVisibility when it is unknown.
func (r *Resolver) instanceTrust(name string) string {
	if inst, ok := r.registry().Get(name); ok && inst.TrustedAuthorsOnly != "" {
		return inst.TrustedAuthorsOnly
	}
	return config.TrustFollowVisibility
}

// instanceQuota is a copy of the monthly token quota of the instance called
// name; nil when it has none or is unknown.
func (r *Resolver) instanceQuota(name string) *int64 {
	inst, ok := r.registry().Get(name)
	if !ok || inst.MonthlyTokenQuota == nil {
		return nil
	}
	q := *inst.MonthlyTokenQuota
	return &q
}

// fromConfig resolves a config repository; ok is false when its instance
// isn't registered (config.Validate rules that out). The config defaults
// failure_status to failed, so failure_status counts as overridden only when
// it is neutral.
func (r *Resolver) fromConfig(repo config.Repository) (Effective, bool) {
	platform, ok := r.registry().Platform(repo.Instance())
	if !ok {
		return Effective{}, false
	}
	eff := r.defaults(platform, repo.Instance(), repo.Project, SourceConfig)
	if repo.Approve != nil {
		eff.Approve, eff.Overridden[KeyApprove] = *repo.Approve, true
	}
	if repo.ReviewDrafts != nil {
		eff.ReviewDrafts, eff.Overridden[KeyReviewDrafts] = *repo.ReviewDrafts, true
	}
	if repo.InlineFindings != nil {
		eff.InlineFindings, eff.Overridden[KeyInlineFindings] = *repo.InlineFindings, true
	}
	if repo.CommandsAuthorAndMaintainersOnly != nil {
		eff.CommandsAuthorAndMaintainersOnly, eff.Overridden[KeyCommandsAuthorAndMaintainersOnly] = *repo.CommandsAuthorAndMaintainersOnly, true
	}
	if repo.IncrementalReviews != nil {
		eff.IncrementalReviews, eff.Overridden[KeyIncrementalReviews] = *repo.IncrementalReviews, true
	}
	if repo.LearnFromDismissals != nil {
		eff.LearnFromDismissals, eff.Overridden[KeyLearnFromDismissals] = *repo.LearnFromDismissals, true
	}
	if repo.QuietReviews != nil {
		eff.QuietReviews, eff.Overridden[KeyQuietReviews] = *repo.QuietReviews, true
	}
	if len(repo.CommentLayout) > 0 {
		eff.CommentLayout, eff.Overridden[KeyCommentLayout] = slices.Clone(repo.CommentLayout), true
	}
	if repo.LinkedReposEnabled != nil {
		eff.LinkedReposEnabled, eff.Overridden[KeyLinkedReposEnabled] = *repo.LinkedReposEnabled, true
	}
	eff.LinkedRepos = cloneLinks(repo.LinkedRepos)
	if repo.TrustedAuthorsOnly != nil {
		eff.TrustedAuthorsOnly, eff.Overridden[KeyTrustedAuthorsOnly] = *repo.TrustedAuthorsOnly, true
	}
	if len(repo.TrustedAuthors) > 0 {
		eff.TrustedAuthors, eff.Overridden[KeyTrustedAuthors] = slices.Clone(repo.TrustedAuthors), true
	}
	if q := repo.MonthlyTokenQuota; q != nil {
		v := *q
		eff.MonthlyTokenQuota, eff.Overridden[KeyMonthlyTokenQuota] = &v, true
	}
	if repo.FailureStatus != "" && repo.FailureStatus != config.FailureStatusFailed {
		eff.FailureStatus, eff.Overridden[KeyFailureStatus] = repo.FailureStatus, true
	}
	if len(repo.Skills) > 0 {
		eff.Skills, eff.Overridden[KeySkills] = slices.Clone(repo.Skills), true
	}
	if len(repo.RepoSkills) > 0 {
		eff.RepoSkills, eff.Overridden[KeyRepoSkills] = slices.Clone(repo.RepoSkills), true
	}
	if repo.JobTimeout != nil {
		eff.JobTimeout, eff.Overridden[KeyJobTimeout] = *repo.JobTimeout, true
	}
	if repo.MaxTurns != nil {
		eff.MaxTurns, eff.Overridden[KeyMaxTurns] = *repo.MaxTurns, true
	}
	if repo.JobMemory != nil {
		eff.JobMemory, eff.Overridden[KeyJobMemory] = repo.JobMemory.DeepCopy(), true
	}
	if repo.RerunLimitPerMR != nil {
		eff.RerunLimitPerMR, eff.Overridden[KeyRerunLimitPerMR] = *repo.RerunLimitPerMR, true
	}
	if repo.RerunLimitPerRepo != nil {
		eff.RerunLimitPerRepo, eff.Overridden[KeyRerunLimitPerRepo] = *repo.RerunLimitPerRepo, true
	}
	return eff, true
}

// fromDashboard resolves a dashboard row on top of the defaults stored in
// the dashboard; ok is false when its instance isn't registered. A stored
// value that can't be used (see defaultsFor) is ignored and the default
// applies: a bad setting never stops reviews.
func (r *Resolver) fromDashboard(row store.DashboardRepo, stored store.RepoDefaults) (Effective, bool) {
	platform, ok := r.registry().Platform(row.Instance)
	if !ok {
		return Effective{}, false
	}
	eff := r.defaultsFor(SourceDashboard, platform, row.Instance, row.Project, stored)
	if row.Approve != nil {
		eff.Approve, eff.Overridden[KeyApprove] = *row.Approve, true
	}
	if row.ReviewDrafts != nil {
		eff.ReviewDrafts, eff.Overridden[KeyReviewDrafts] = *row.ReviewDrafts, true
	}
	if row.InlineFindings != nil {
		eff.InlineFindings, eff.Overridden[KeyInlineFindings] = *row.InlineFindings, true
	}
	if row.CommandsAuthorAndMaintainersOnly != nil {
		eff.CommandsAuthorAndMaintainersOnly, eff.Overridden[KeyCommandsAuthorAndMaintainersOnly] = *row.CommandsAuthorAndMaintainersOnly, true
	}
	if row.IncrementalReviews != nil {
		eff.IncrementalReviews, eff.Overridden[KeyIncrementalReviews] = *row.IncrementalReviews, true
	}
	if row.LearnFromDismissals != nil {
		eff.LearnFromDismissals, eff.Overridden[KeyLearnFromDismissals] = *row.LearnFromDismissals, true
	}
	if row.QuietReviews != nil {
		eff.QuietReviews, eff.Overridden[KeyQuietReviews] = *row.QuietReviews, true
	}
	if len(row.CommentLayout) > 0 {
		eff.CommentLayout, eff.Overridden[KeyCommentLayout] = slices.Clone(row.CommentLayout), true
	}
	if m := row.TrustedAuthorsOnly; m != nil && (*m == config.TrustOn || *m == config.TrustOff) {
		eff.TrustedAuthorsOnly, eff.Overridden[KeyTrustedAuthorsOnly] = *m, true
	}
	if len(row.TrustedAuthors) > 0 {
		eff.TrustedAuthors, eff.Overridden[KeyTrustedAuthors] = slices.Clone(row.TrustedAuthors), true
	}
	if q := row.MonthlyTokenQuota; q != nil && *q >= 1 && *q <= config.MaxMonthlyTokenQuota {
		v := *q
		eff.MonthlyTokenQuota, eff.Overridden[KeyMonthlyTokenQuota] = &v, true
	}
	if row.LinkedReposEnabled != nil {
		eff.LinkedReposEnabled, eff.Overridden[KeyLinkedReposEnabled] = *row.LinkedReposEnabled, true
	}
	eff.LinkedRepos = cloneLinks(row.LinkedRepos)
	if row.FailureStatus != nil {
		if fs, ok := failureStatus(*row.FailureStatus); ok {
			eff.FailureStatus, eff.Overridden[KeyFailureStatus] = fs, true
		}
	}
	if len(row.Skills) > 0 {
		eff.Skills, eff.Overridden[KeySkills] = slices.Clone(row.Skills), true
	}
	if len(row.RepoSkills) > 0 {
		eff.RepoSkills, eff.Overridden[KeyRepoSkills] = slices.Clone(row.RepoSkills), true
	}
	if row.JobTimeoutSeconds != nil && *row.JobTimeoutSeconds > 0 {
		eff.JobTimeout, eff.Overridden[KeyJobTimeout] = time.Duration(*row.JobTimeoutSeconds)*time.Second, true
	}
	if usableMaxTurns(row.MaxTurns) {
		eff.MaxTurns, eff.Overridden[KeyMaxTurns] = *row.MaxTurns, true
	}
	if row.JobMemory != nil {
		if q, ok := r.usableMemory(*row.JobMemory); ok {
			eff.JobMemory, eff.Overridden[KeyJobMemory] = q, true
		}
	}
	if row.ModelID != nil && *row.ModelID != "" {
		eff.ModelID, eff.Overridden[KeyModel] = *row.ModelID, true
	}
	if usableCap(row.RerunLimitPerMR) {
		eff.RerunLimitPerMR, eff.Overridden[KeyRerunLimitPerMR] = *row.RerunLimitPerMR, true
	}
	if usableCap(row.RerunLimitPerRepo) {
		eff.RerunLimitPerRepo, eff.Overridden[KeyRerunLimitPerRepo] = *row.RerunLimitPerRepo, true
	}
	return eff, true
}
