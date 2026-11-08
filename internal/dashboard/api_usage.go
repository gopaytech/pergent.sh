package dashboard

import (
	"net/http"
	"time"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	defaultUsageDays = 30
	maxUsageDays     = 90
	// usageMRLimit is how many merge requests a repository's usage lists.
	usageMRLimit = 50

	msgBadUsageDays = "days must be a number from 1 to 90"
)

// usageTokens is t as the views carry it.
func usageTokens(t store.TokenSum) UsageTokensView {
	return UsageTokensView{In: t.In, Out: t.Out, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite,
		Reasoning: t.Reasoning, Unknown: t.Unknown}
}

// addTokens adds b to a.
func addTokens(a *store.TokenSum, b store.TokenSum) {
	a.In += b.In
	a.Out += b.Out
	a.CacheRead += b.CacheRead
	a.CacheWrite += b.CacheWrite
	a.Reasoning += b.Reasoning
	a.Unknown += b.Unknown
}

func usageDays(days []store.DayUsage) []UsageDayView {
	out := make([]UsageDayView, 0, len(days))
	for _, d := range days {
		out = append(out, UsageDayView{Day: d.Day.UTC().Format(time.DateOnly), Tokens: usageTokens(d.Tokens)})
	}
	return out
}

func usageNamed(rows []store.NamedUsage) []UsageNamedView {
	out := make([]UsageNamedView, 0, len(rows))
	for _, r := range rows {
		out = append(out, UsageNamedView{Name: r.Name, Tokens: usageTokens(r.Tokens), Runs: r.Runs, Failed: r.Failed})
	}
	return out
}

// --- GET /api/admin/usage ---

// usageOverview serves what every repository used in the last days: the
// totals, each day, and the repositories busiest first, those switched off
// since and those of a removed instance too.
func (d *Dashboard) usageOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days, ok := parseDays(r, defaultUsageDays, maxUsageDays)
	if !ok {
		writeError(w, http.StatusBadRequest, msgBadUsageDays)
		return
	}
	since := d.now().Add(-time.Duration(days) * day)
	repos, err := d.Store.UsageByRepository(ctx, since)
	if err != nil {
		d.failed(w, r, "reading usage", err)
		return
	}
	daily, err := d.Store.UsageDaily(ctx, since, "", 0)
	if err != nil {
		d.failed(w, r, "reading usage", err)
		return
	}
	on, err := d.Repos.List(ctx)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	switchedOn := byProject(on)

	out := UsageOverviewView{Days: days, Daily: usageDays(daily), Repositories: make([]UsageRepositoryRowView, 0, len(repos))}
	var total store.TokenSum
	for _, u := range repos {
		_, isOn := switchedOn[repoKey(u.Instance, u.ProjectPath)]
		out.Repositories = append(out.Repositories, UsageRepositoryRowView{Instance: u.Instance, Project: u.ProjectPath,
			SwitchedOn: isOn, Tokens: usageTokens(u.Tokens), Reviews: u.Reviews})
		addTokens(&total, u.Tokens)
		// A run belongs to one repository, so the counts add up.
		out.Reviews += u.Reviews
	}
	out.Tokens = usageTokens(total)
	writeJSON(w, http.StatusOK, out)
}

// --- GET /api/repositories/{instance}/{project...}/usage ---

// repositoryUsage serves repo's usage in the last days. Only admins may read
// it: anyone else gets the 404 of a repository that is not there.
func (d *Dashboard) repositoryUsage(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	if !se.Admin {
		notFound(w)
		return
	}
	days, ok := parseDays(r, defaultUsageDays, maxUsageDays)
	if !ok {
		writeError(w, http.StatusBadRequest, msgBadUsageDays)
		return
	}
	ctx := r.Context()
	out := UsageRepositoryView{Days: days, Daily: []UsageDayView{}, Skills: []UsageNamedView{}, Models: []UsageNamedView{},
		MergeRequests: []UsageMRView{}, Outliers: []UsageOutlierView{}}
	// A repository the platform hasn't told us the ID of has no usage.
	id, found, err := d.projectID(ctx, se, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading usage", err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, out)
		return
	}
	since := d.now().Add(-time.Duration(days) * day)
	daily, err := d.Store.UsageDaily(ctx, since, repo.Instance, id)
	if err != nil {
		d.failed(w, r, "reading usage", err)
		return
	}
	skills, err := d.Store.UsageBySkill(ctx, repo.Instance, id, since)
	if err != nil {
		d.failed(w, r, "reading usage", err)
		return
	}
	models, err := d.Store.UsageByModel(ctx, repo.Instance, id, since)
	if err != nil {
		d.failed(w, r, "reading usage", err)
		return
	}
	mrs, err := d.Store.UsageByMR(ctx, repo.Instance, id, since, usageMRLimit)
	if err != nil {
		d.failed(w, r, "reading usage", err)
		return
	}
	wasted, err := d.Store.UsageWasted(ctx, repo.Instance, id, since)
	if err != nil {
		d.failed(w, r, "reading usage", err)
		return
	}
	cost, err := d.Store.UsageCostPerReview(ctx, repo.Instance, id, since)
	if err != nil {
		d.failed(w, r, "reading usage", err)
		return
	}

	out.Daily, out.Skills, out.Models = usageDays(daily), usageNamed(skills), usageNamed(models)
	for _, m := range mrs {
		out.MergeRequests = append(out.MergeRequests, UsageMRView{IID: m.IID, Title: m.Title, WebURL: safeURL(m.WebURL),
			Tokens: usageTokens(m.Tokens), Reviews: m.Reviews})
	}
	out.Wasted = UsageWastedView{Failed: usageTokens(wasted.Failed), Reruns: usageTokens(wasted.Reruns),
		RerunCount: wasted.RerunCount, NoResult: wasted.NoResult, Superseded: wasted.Superseded}
	out.FullMedian, out.IncrementalMedian = cost.FullMedian, cost.IncrementalMedian
	for _, o := range cost.Outliers {
		out.Outliers = append(out.Outliers, UsageOutlierView{RunID: o.RunID, IID: o.IID, Incremental: o.Incremental,
			Total: o.Total, At: o.At.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, out)
}
