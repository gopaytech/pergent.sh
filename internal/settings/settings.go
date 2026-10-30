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
	KeyApprove       = "approve"
	KeyReviewDrafts  = "review_drafts"
	KeyFailureStatus = "failure_status"
	KeySkills        = "skills"
	KeyJobTimeout    = "job_timeout"
	KeyJobMemory     = "job_memory"
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
	FailureStatus     config.FailureStatus
	// Skills are the library skills reviewed; nil means all of them.
	Skills     []string
	JobTimeout time.Duration
	// JobMemory is the Job's memory limit; zero when neither the
	// repository nor jobs.resources.limits sets one.
	JobMemory resource.Quantity
	// Overridden tells, for each setting key, whether the repository sets
	// it rather than taking the default. Every key is present.
	Overridden map[string]bool
}

// Resolver resolves repositories from Config and, when Store is not nil, the
// dashboard's repositories in Store.
type Resolver struct {
	Config *config.Config
	Store  *store.Store
}

// List returns every repository switched on: the config's first, in config
// order, then the dashboard's that the config doesn't list, ordered by
// instance and project. A dashboard row of an instance the config doesn't
// have is left out.
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
	for _, row := range rows {
		if r.InConfig(row.Instance, row.Project) {
			continue
		}
		if eff, ok := r.fromDashboard(row); ok {
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
	eff, ok := r.fromDashboard(row)
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

// defaults is a repository of platform on instance with every setting at
// its default and none overridden.
func (r *Resolver) defaults(platform, instance, project string, source Source) Effective {
	return Effective{
		Platform: platform, Instance: instance, Project: project, Source: source,
		Approve:       true,
		ReviewDrafts:  true,
		FailureStatus: config.FailureStatusFailed,
		JobTimeout:    r.Config.Jobs.Timeout,
		JobMemory:     r.jobsMemory(r.Config.Jobs.Resources.Limits),
		Overridden: map[string]bool{
			KeyApprove: false, KeyReviewDrafts: false, KeyFailureStatus: false,
			KeySkills: false, KeyJobTimeout: false, KeyJobMemory: false,
		},
	}
}

// jobsMemory is the memory entry of list (jobs.resources), copied; zero
// when there is none.
func (r *Resolver) jobsMemory(list corev1.ResourceList) resource.Quantity {
	q := list[corev1.ResourceMemory]
	return q.DeepCopy()
}

// fromConfig resolves a config repository; ok is false when its instance
// isn't configured (config.Validate rules that out). The config defaults
// failure_status to failed, so failure_status counts as overridden only when
// it is neutral.
func (r *Resolver) fromConfig(repo config.Repository) (Effective, bool) {
	platform, ok := r.Config.Platform(repo.Instance())
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
	if repo.FailureStatus != "" && repo.FailureStatus != config.FailureStatusFailed {
		eff.FailureStatus, eff.Overridden[KeyFailureStatus] = repo.FailureStatus, true
	}
	if len(repo.Skills) > 0 {
		eff.Skills, eff.Overridden[KeySkills] = slices.Clone(repo.Skills), true
	}
	if repo.JobTimeout != nil {
		eff.JobTimeout, eff.Overridden[KeyJobTimeout] = *repo.JobTimeout, true
	}
	if repo.JobMemory != nil {
		eff.JobMemory, eff.Overridden[KeyJobMemory] = repo.JobMemory.DeepCopy(), true
	}
	return eff, true
}

// fromDashboard resolves a dashboard row; ok is false when its instance
// isn't configured. A stored value that can't be used (a memory limit that
// doesn't parse, isn't positive or is below jobs.resources.requests, which
// Kubernetes would refuse; an unknown failure status) is ignored and the
// default applies: a bad setting never stops reviews.
func (r *Resolver) fromDashboard(row store.DashboardRepo) (Effective, bool) {
	platform, ok := r.Config.Platform(row.Instance)
	if !ok {
		return Effective{}, false
	}
	eff := r.defaults(platform, row.Instance, row.Project, SourceDashboard)
	if row.Approve != nil {
		eff.Approve, eff.Overridden[KeyApprove] = *row.Approve, true
	}
	if row.ReviewDrafts != nil {
		eff.ReviewDrafts, eff.Overridden[KeyReviewDrafts] = *row.ReviewDrafts, true
	}
	if row.FailureStatus != nil {
		switch fs := config.FailureStatus(*row.FailureStatus); fs {
		case config.FailureStatusFailed, config.FailureStatusNeutral:
			eff.FailureStatus, eff.Overridden[KeyFailureStatus] = fs, true
		}
	}
	if len(row.Skills) > 0 {
		eff.Skills, eff.Overridden[KeySkills] = slices.Clone(row.Skills), true
	}
	if row.JobTimeoutSeconds != nil && *row.JobTimeoutSeconds > 0 {
		eff.JobTimeout, eff.Overridden[KeyJobTimeout] = time.Duration(*row.JobTimeoutSeconds)*time.Second, true
	}
	if row.JobMemory != nil {
		request := r.jobsMemory(r.Config.Jobs.Resources.Requests)
		if q, err := resource.ParseQuantity(*row.JobMemory); err == nil && q.Sign() > 0 && q.Cmp(request) >= 0 {
			eff.JobMemory, eff.Overridden[KeyJobMemory] = q, true
		}
	}
	return eff, true
}
