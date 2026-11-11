package dashboard

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	defaultUsageDays = 30
	maxUsageDays     = 90
	// usageMRLimit is how many merge requests a repository's usage lists.
	usageMRLimit = 50
	// usageTenantLimit is how many tenants a client's usage lists: a caller
	// makes as many as it likes.
	usageTenantLimit = 100

	msgBadUsageDays = "days must be a number from 1 to 90"
)

// usageTokens is t as the views carry it.
func usageTokens(t store.TokenSum) UsageTokensView {
	return UsageTokensView{In: t.In, Out: t.Out, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite,
		Reasoning: t.Reasoning, Unknown: t.Unknown}
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
		out = append(out, UsageNamedView{Provider: r.Provider, Name: r.Name, Tokens: usageTokens(r.Tokens), Runs: r.Runs, Failed: r.Failed})
	}
	return out
}

// --- GET /api/admin/usage ---

// usageOverview serves what every repository used in the last days: the
// totals, each day, the repositories busiest first, those switched off since
// and those of a removed instance too, and the API clients busiest first. The
// totals are the repositories plus the clients, which is what the daily chart
// adds up to.
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
	clients, err := d.Store.UsageByAPIClient(ctx, since)
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

	out := UsageOverviewView{Days: days, Daily: usageDays(daily), Repositories: make([]UsageRepositoryRowView, 0, len(repos)),
		Clients: make([]UsageClientView, 0, len(clients))}
	var total store.TokenSum
	for _, u := range repos {
		_, isOn := switchedOn[repoKey(u.Instance, u.ProjectPath)]
		out.Repositories = append(out.Repositories, UsageRepositoryRowView{Instance: u.Instance, Project: u.ProjectPath,
			SwitchedOn: isOn, Tokens: usageTokens(u.Tokens), Reviews: u.Reviews})
		total.Add(u.Tokens)
		// A run belongs to one repository, so the counts add up.
		out.Reviews += u.Reviews
	}
	for _, c := range clients {
		v := UsageClientView{Name: c.Name, Tokens: usageTokens(c.Tokens), Reviews: c.Reviews,
			Tenants: make([]UsageTenantView, 0, min(len(c.Tenants), usageTenantLimit))}
		for i, t := range c.Tenants {
			if i == usageTenantLimit {
				v.MoreTenants = len(c.Tenants) - usageTenantLimit
				break
			}
			v.Tenants = append(v.Tenants, UsageTenantView{Tenant: t.Tenant, Tokens: usageTokens(t.Tokens), Reviews: t.Reviews})
		}
		out.Clients = append(out.Clients, v)
		total.Add(c.Tokens)
		out.Reviews += c.Reviews
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
		models := make([]UsageModelView, 0, len(m.Models))
		for _, u := range m.Models {
			models = append(models, UsageModelView{Provider: u.Provider, Model: u.Model})
		}
		out.MergeRequests = append(out.MergeRequests, UsageMRView{IID: m.IID, Title: m.Title, WebURL: safeURL(m.WebURL),
			Tokens: usageTokens(m.Tokens), Reviews: m.Reviews, Models: models})
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

// --- GET /api/admin/usage.csv ---

// maxUsageCSVRows is the most rows an export holds. A longer one ends there,
// without a mark: 90 days of a busy installation is far below it.
const maxUsageCSVRows = 200000

const msgBadUsageRepository = "instance and project go together: give both or neither"

// usageCSVHeader is the first row of an export.
var usageCSVHeader = []string{"reported_at", "instance", "repository", "merge_request", "trigger", "incremental", "model",
	"skill", "verdict", "tokens_in", "tokens_out", "cache_read", "cache_write", "reasoning", "findings", "blocking", "provider"}

// csvCell keeps a spreadsheet from reading a cell as a formula.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// optionalCount is a count the Job may not have reported: "" when it didn't.
func optionalCount(n *int64) string {
	if n == nil {
		return ""
	}
	return strconv.FormatInt(*n, 10)
}

// usageCSV exports what each skill of each run used in the last days, one row
// a run and skill, oldest first, for every repository or for the one given by
// instance and project; the provider is the last column, so the others keep
// their places. A row holds no title and no text a person wrote, and
// no dismissal: those belong to a change. The rows go out as the database
// sends them.
func (d *Dashboard) usageCSV(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	days, ok := parseDays(r, defaultUsageDays, maxUsageDays)
	if !ok {
		writeError(w, http.StatusBadRequest, msgBadUsageDays)
		return
	}
	instance, project := r.URL.Query().Get("instance"), r.URL.Query().Get("project")
	if (instance == "") != (project == "") {
		writeError(w, http.StatusBadRequest, msgBadUsageRepository)
		return
	}
	var projectID int64
	// A repository the platform hasn't told us the ID of has no usage: a
	// header alone.
	noUsage := false
	if instance != "" {
		var found bool
		var err error
		if projectID, found, err = d.projectID(ctx, se, instance, project); err == nil && !found {
			// Switching a repository off deletes its project but not its
			// usage, so the usage names the repository too.
			projectID, found, err = d.Store.UsageProjectID(ctx, instance, project)
		}
		if err != nil {
			d.failed(w, r, "exporting usage", err)
			return
		}
		noUsage = !found
	}

	out := csv.NewWriter(w)
	started := false
	start := func() error {
		if started {
			return nil
		}
		started = true
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="pergent-usage-%dd.csv"`, days))
		return out.Write(usageCSVHeader)
	}
	if !noUsage {
		err := d.Store.UsageRows(ctx, d.now().Add(-time.Duration(days)*day), instance, projectID, maxUsageCSVRows, func(u store.UsageRow) error {
			if err := start(); err != nil {
				return err
			}
			return out.Write([]string{
				u.ReportedAt.UTC().Format(time.RFC3339), csvCell(u.Instance), csvCell(u.Project),
				// A merge request's number is a number, not text.
				strconv.FormatInt(u.MR, 10), csvCell(u.Trigger), strconv.FormatBool(u.Incremental), csvCell(u.Model),
				csvCell(u.Skill), csvCell(u.Verdict), strconv.FormatInt(u.In, 10), strconv.FormatInt(u.Out, 10),
				optionalCount(u.CacheRead), optionalCount(u.CacheWrite), optionalCount(u.Reasoning),
				strconv.Itoa(u.Findings), strconv.Itoa(u.Blocking), csvCell(u.Provider)})
		})
		if err != nil {
			if ctx.Err() != nil {
				return // the download was cancelled
			}
			if !started {
				d.failed(w, r, "exporting usage", err)
				return
			}
			// The file is begun, and whether any of it has left depends on
			// the writer's buffer, so the status can't be changed to an
			// error. End the response without its last chunk, so that the
			// download fails either way instead of passing for a whole file.
			d.Log.Error("dashboard: exporting usage failed", "path", r.URL.Path, "error", err.Error())
			panic(http.ErrAbortHandler)
		}
	}
	if err := start(); err == nil {
		out.Flush()
	}
}
