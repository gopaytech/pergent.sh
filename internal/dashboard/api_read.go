package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/actions"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// reviewPageSize is the most reviews GET /api/reviews returns at once.
	reviewPageSize = 50
	// recentReviews is how many reviews the overview lists, from the last
	// recentDays days.
	recentReviews = 10
	recentDays    = 90

	defaultDays       = 7
	maxOverviewDays   = 90
	defaultReviewDays = 90
	maxReviewDays     = 365
	day               = 24 * time.Hour

	// inputsTTL is how long a repository's resolved skills and
	// instructions are kept.
	inputsTTL = 5 * time.Minute

	msgLoadFailed   = "could not load the data, please try again"
	msgNotFound     = "not found"
	msgInputsFailed = "could not read the repository's skills and instructions from the platform, please try again"
	msgNotSynced    = "pergent.sh hasn't set this repository up yet; try again in a minute"
)

// errNotSynced is a repository reconcile hasn't stored yet.
var errNotSynced = errors.New("the repository isn't stored yet (reconcile hasn't reached it)")

var outcomes = map[string]bool{
	OutcomeRunning: true, OutcomeApproved: true, OutcomeBlocking: true, OutcomeCI: true, OutcomeDraft: true,
	OutcomeNoBlocking: true, OutcomeFailed: true, OutcomeSuperseded: true, OutcomeSkipped: true, OutcomeUnknown: true, OutcomeWaiting: true,
}

// readRoutes adds the read API to mux, every route behind a session; the
// Settings page's data is an admin route (adminRoute).
func (d *Dashboard) readRoutes(mux *http.ServeMux) {
	get := func(pattern string, h http.HandlerFunc) { mux.Handle("GET "+pattern, d.Auth.Require(h)) }
	get("/api/overview", d.overview)
	get("/api/reviews", d.reviews)
	get("/api/reviews/{instance}/{rest...}", d.review)
	get("/api/runs/{id}", d.run)
	get("/api/runs/{id}/log", d.runLog)
	get("/api/runs/{id}/diagnostics", d.runDiagnostics)
	// A multi review reviewer attempt's: the same handlers, so the same gates.
	get("/api/runs/{id}/members/{member}/log", d.runLog)
	get("/api/runs/{id}/members/{member}/diagnostics", d.runDiagnostics)
	get("/api/repositories", d.repositories)
	get("/api/repositories/{instance}/{rest...}", d.repository)
	get("/api/skills", d.skills)
	get("/api/skills/{name}", d.skill)
	get("/api/skills/{name}/versions/{v}", d.skillVersion)
	d.adminRoute(mux, "GET /api/admin/settings", d.adminSettings)
	// Anything else is 404, also the instance alone (which the mux would
	// otherwise redirect to the {rest...} routes).
	none := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { notFound(w) })
	get("/api/reviews/{instance}", none)
	get("/api/repositories/{instance}", none)
	mux.Handle("/api/", d.Auth.Require(none))
}

// failed logs err and answers 500 with a message that says nothing of it.
func (d *Dashboard) failed(w http.ResponseWriter, r *http.Request, what string, err error) {
	d.Log.Error("dashboard: "+what+" failed", "path", r.URL.Path, "error", err.Error())
	writeError(w, http.StatusInternalServerError, msgLoadFailed)
}

func notFound(w http.ResponseWriter) { writeError(w, http.StatusNotFound, msgNotFound) }

func session(r *http.Request) Session {
	se, _ := SessionFrom(r.Context())
	return se
}

// visibility is what the session may read of the runs: every run for an
// admin, else only those of its visible repositories, by instance and
// lower-cased path, with their IDs. A repository reconcile has not stored yet
// (ID 0) has no runs.
func visibility(se Session) store.Visibility {
	if se.All {
		return store.Visibility{All: true}
	}
	out := make([]store.VisibleProject, 0, len(se.Visible))
	for k, id := range se.Visible {
		if id > 0 {
			out = append(out, store.VisibleProject{Instance: k.Instance, Path: k.Path, ID: id})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Instance != out[j].Instance {
			return out[i].Instance < out[j].Instance
		}
		return out[i].Path < out[j].Path
	})
	return store.Visibility{Projects: out}
}

// visibleOf is what the session may read of the runs of repos.
func visibleOf(se Session, repos []settings.Effective) store.Visibility {
	if se.All {
		return store.Visibility{All: true}
	}
	out := make([]store.VisibleProject, 0, len(repos))
	for _, e := range repos {
		k := repoKey(e.Instance, e.Project)
		if id := se.Visible[k]; id > 0 {
			out = append(out, store.VisibleProject{Instance: k.Instance, Path: k.Path, ID: id})
		}
	}
	return store.Visibility{Projects: out}
}

// projectID is the platform ID of the project the session sees at project
// on instance: its visible one, or for an admin the stored one; ok is false
// when there is none (a repository reconcile has not stored yet).
func (d *Dashboard) projectID(ctx context.Context, se Session, instance, project string) (int64, bool, error) {
	if !se.All {
		id := se.Visible[repoKey(instance, project)]
		return id, id > 0, nil
	}
	p, found, err := d.Store.ProjectByPath(ctx, instance, project)
	if err != nil || !found {
		return 0, false, err
	}
	return p.ProjectID, true, nil
}

// activeRepos returns the repositories switched on that the session's user
// can see, on every instance, in the resolver's order.
func (d *Dashboard) activeRepos(ctx context.Context, se Session) ([]settings.Effective, error) {
	all, err := d.Repos.List(ctx)
	if err != nil {
		return nil, err
	}
	return seenBy(se, all), nil
}

// visibleRepos is activeRepos with the paused repositories too: the list, a
// repository's page and its writes show and change a paused one. Nothing
// that starts a Job may go by it alone (refusePaused).
func (d *Dashboard) visibleRepos(ctx context.Context, se Session) ([]settings.Effective, error) {
	all, err := d.Repos.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	return seenBy(se, all), nil
}

// seenBy is the repositories of all the session's user can see.
func seenBy(se Session, all []settings.Effective) []settings.Effective {
	var out []settings.Effective
	for _, e := range all {
		if se.CanSee(e.Instance, e.Project) {
			out = append(out, e)
		}
	}
	return out
}

func byProject(repos []settings.Effective) map[RepoKey]settings.Effective {
	m := make(map[RepoKey]settings.Effective, len(repos))
	for _, e := range repos {
		m[repoKey(e.Instance, e.Project)] = e
	}
	return m
}

func (d *Dashboard) summary(r store.RunDigest, j judged) ReviewSummary {
	platform, _ := d.registry.Platform(r.MR.Instance)
	return ReviewSummary{
		Instance: r.MR.Instance, Project: r.ProjectPath, Platform: platform, Number: r.MR.IID,
		Title: r.Title, Outcome: j.outcome, Reason: j.reason, UpdatedAt: r.UpdatedAt.UTC(), WebURL: safeURL(r.WebURL),
		Attempts: r.Attempts,
	}
}

// safeURL is u when it is an absolute http or https address, else "": the
// page links to it.
func safeURL(u string) string {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
		return ""
	}
	return u
}

// parseDays reads the days parameter: def when absent, else 1 to max.
func parseDays(r *http.Request, def, max int) (int, bool) {
	q := r.URL.Query()
	if !q.Has("days") {
		return def, true
	}
	n, err := strconv.Atoi(q.Get("days"))
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

// attention returns the setup problems of the repositories in active (only
// only's when its Path isn't ""). Instance-wide items, the library's (no
// instance) and an API client's are never a repository's: no view shows them.
func (d *Dashboard) attention(ctx context.Context, active map[RepoKey]settings.Effective, only RepoKey) ([]AttentionView, error) {
	items, err := d.Store.Attention(ctx)
	if err != nil {
		return nil, err
	}
	out := []AttentionView{}
	for _, a := range items {
		// A client's item is stored with the client's name as the project.
		if a.Kind == store.AttentionCallbackFailing || a.Project == "" {
			continue
		}
		k := repoKey(a.Instance, a.Project)
		if only.Path != "" && k != only {
			continue
		}
		if _, ok := active[k]; !ok {
			continue
		}
		out = append(out, AttentionView{Instance: a.Instance, Project: a.Project, Kind: a.Kind,
			Message: a.Message, UpdatedAt: a.UpdatedAt.UTC()})
	}
	return out, nil
}

// --- GET /api/overview ---

func (d *Dashboard) overview(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	days, ok := parseDays(r, defaultDays, maxOverviewDays)
	if !ok {
		writeError(w, http.StatusBadRequest, "days must be a number from 1 to 90")
		return
	}
	vis := visibility(se)
	counts, err := d.Store.RunStats(ctx, vis, d.now().Add(-time.Duration(days)*day))
	if err != nil {
		d.failed(w, r, "counting reviews", err)
		return
	}
	var stats StatsView
	for _, c := range counts {
		stats.Reviews += c.Reviews
		stats.Approved += c.Approved
		stats.Blocking += c.Blocking
		stats.Failed += c.Failed
	}
	recent, err := d.Store.ReviewDigests(ctx, store.ReviewFilter{Visible: vis,
		Since: d.now().Add(-recentDays * day), Limit: recentReviews})
	if err != nil {
		d.failed(w, r, "listing recent reviews", err)
		return
	}
	view := OverviewView{Stats: stats, Recent: []ReviewSummary{}}
	for _, run := range recent {
		view.Recent = append(view.Recent, d.summary(run, judge(run)))
	}
	if se.Admin {
		if view.Setup, err = d.setup(ctx); err != nil {
			d.failed(w, r, "reading the setup checklist", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// setup is the checklist an admin sees on the overview, nil once every item
// is done. The skill item is the library's: a repository's own skills don't
// count, as a fresh install builds the library first. The repository item
// looks at every repository switched on, not only those the admin sees.
func (d *Dashboard) setup(ctx context.Context) (*SetupView, error) {
	library, err := d.Store.LibrarySkills(ctx)
	if err != nil {
		return nil, err
	}
	repos, err := d.Repos.List(ctx)
	if err != nil {
		return nil, err
	}
	v := SetupView{LLM: d.registry.LLMStatus().KeySet, Instance: len(d.registry.All()) > 0,
		Skill: len(library) > 0, Repository: len(repos) > 0}
	if v == (SetupView{true, true, true, true}) {
		return nil, nil
	}
	return &v, nil
}

// --- GET /api/reviews ---

// cursorJSON is a review cursor's content; the cursor is it, in JSON,
// base64url-encoded.
type cursorJSON struct {
	At time.Time `json:"t"`
	ID string    `json:"id"`
}

func encodeCursor(r store.RunDigest) string {
	return encodeCursorAt(r.UpdatedAt, r.ID)
}

// encodeCursorAt is the cursor that ends a page at the row with this time and
// run ID.
func encodeCursorAt(at time.Time, id string) string {
	b, _ := json.Marshal(cursorJSON{At: at, ID: id})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*store.ReviewCursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	var c cursorJSON
	if json.Unmarshal(b, &c) != nil || c.At.IsZero() || !uuidPattern.MatchString(c.ID) {
		return nil, false
	}
	return &store.ReviewCursor{At: c.At, ID: c.ID}, true
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (d *Dashboard) reviews(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	q := r.URL.Query()
	outcome := q.Get("outcome")
	if outcome != "" && !outcomes[outcome] {
		writeError(w, http.StatusBadRequest, "unknown outcome")
		return
	}
	days, ok := parseDays(r, defaultReviewDays, maxReviewDays)
	if !ok {
		writeError(w, http.StatusBadRequest, "days must be a number from 1 to 365")
		return
	}
	// One more than a page tells whether there is a next one.
	filter := store.ReviewFilter{Visible: visibility(se), Outcome: outcome,
		Since: d.now().Add(-time.Duration(days) * day), Limit: reviewPageSize + 1}
	if p := q.Get("project"); p != "" {
		v, err := d.projectVisibility(ctx, se, q.Get("instance"), p)
		if err != nil {
			d.failed(w, r, "listing projects", err)
			return
		}
		filter.Visible = v
	}
	if c := q.Get("cursor"); c != "" {
		cur, ok := decodeCursor(c)
		if !ok {
			writeError(w, http.StatusBadRequest, "malformed cursor")
			return
		}
		filter.After = cur
	}
	rows, err := d.Store.ReviewDigests(ctx, filter)
	if err != nil {
		d.failed(w, r, "listing reviews", err)
		return
	}
	view := ReviewListView{Items: []ReviewSummary{}}
	if len(rows) > reviewPageSize {
		rows = rows[:reviewPageSize]
		view.NextCursor = encodeCursor(rows[len(rows)-1])
	}
	for _, run := range rows {
		view.Items = append(view.Items, d.summary(run, judge(run)))
	}
	writeJSON(w, http.StatusOK, view)
}

// projectVisibility is what the session may read of the runs of project
// (on instance, or on any when instance is ""): the visible one at that
// path, or for an admin the stored one.
func (d *Dashboard) projectVisibility(ctx context.Context, se Session, instance, project string) (store.Visibility, error) {
	var out []store.VisibleProject
	if !se.All {
		for k, id := range se.Visible {
			if k.Path == lower(project) && (instance == "" || k.Instance == instance) && id > 0 {
				out = append(out, store.VisibleProject{Instance: k.Instance, Path: k.Path, ID: id})
			}
		}
		return store.Visibility{Projects: out}, nil
	}
	stored, err := d.Store.Projects(ctx)
	if err != nil {
		return store.Visibility{}, err
	}
	for _, p := range stored {
		if strings.EqualFold(p.Path, project) && (instance == "" || p.Instance == instance) {
			out = append(out, store.VisibleProject{Instance: p.Instance, Path: p.Path, ID: p.ProjectID})
		}
	}
	return store.Visibility{Projects: out}, nil
}

// --- GET /api/reviews/{instance}/{project...}/{number} ---

func (d *Dashboard) review(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	instance, rest := r.PathValue("instance"), r.PathValue("rest")
	i := strings.LastIndex(rest, "/")
	if i <= 0 {
		notFound(w)
		return
	}
	project := rest[:i]
	number, err := strconv.ParseInt(rest[i+1:], 10, 64)
	if err != nil || number <= 0 || !se.CanSee(instance, project) {
		notFound(w)
		return
	}
	// The merge request of the project the user sees at that path, not of
	// one that had the path before.
	id, ok, err := d.projectID(ctx, se, instance, project)
	if err == nil && !ok && se.All {
		// An admin's review list holds the runs of a repository that was
		// removed, whose project row is gone: its runs still name it.
		id, ok, err = d.Store.RunProjectID(ctx, instance, project)
	}
	if err != nil {
		d.failed(w, r, "loading a project", err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	mr := store.MRKey{Instance: instance, ProjectID: id, IID: number}
	// Only the runs of paths the user can see: a project moved somewhere
	// they can't see keeps its later runs hidden.
	runs, err := d.Store.MRRunDigests(ctx, mr, visibility(se))
	if err != nil {
		d.failed(w, r, "listing a review's runs", err)
		return
	}
	if len(runs) == 0 {
		notFound(w)
		return
	}
	platform, _ := d.registry.Platform(instance)

	newest := runs[len(runs)-1]
	header := ReviewHeader{Instance: instance, Project: newest.ProjectPath, Platform: platform, Number: number,
		Title: newest.Title, TargetBranch: newest.TargetBranch, WebURL: safeURL(newest.WebURL)}
	for k := len(runs) - 1; k >= 0; k-- {
		if header.SourceBranch == "" {
			header.SourceBranch = runs[k].SourceBranch
		}
		if header.Author == "" {
			header.Author = runs[k].Author
		}
	}
	summaries := runSummaries(runs, judge)
	slices.Reverse(summaries)
	view := ReviewDetailView{Header: header, Runs: summaries}
	// The buttons are for repositories switched on.
	if _, on, err := d.Repos.Get(ctx, instance, newest.ProjectPath); err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	} else if on {
		view.Role, view.Can = buttonsFor(se, instance, newest.ProjectPath)
	}
	writeJSON(w, http.StatusOK, view)
}

// runSummaries summarises runs (oldest first), each with the trigger read
// from the run before it.
func runSummaries(runs []store.RunDigest, judgeRun func(store.RunDigest) judged) []RunSummary {
	out := make([]RunSummary, 0, len(runs))
	for i, run := range runs {
		var prev *store.RunDigest
		if i > 0 {
			prev = &runs[i-1]
		}
		out = append(out, runSummary(run, prev, judgeRun(run)))
	}
	return out
}

func runSummary(run store.RunDigest, prev *store.RunDigest, j judged) RunSummary {
	trigger := "first"
	switch {
	case prev == nil:
	case prev.HeadSHA != run.HeadSHA:
		trigger = "push"
	case prev.TargetBranch != run.TargetBranch:
		trigger = "retarget"
	default:
		trigger = "rerun"
	}
	s := RunSummary{ID: run.ID, Head: run.HeadSHA, Trigger: trigger, Outcome: j.outcome, Reason: j.reason,
		CreatedAt: run.CreatedAt.UTC()}
	if !run.DispatchedAt.IsZero() && !run.ReportedAt.IsZero() && !run.ReportedAt.Before(run.DispatchedAt) {
		secs := run.ReportedAt.Sub(run.DispatchedAt).Seconds()
		s.DurationSeconds = &secs
	}
	return s
}

// --- GET /api/runs/{id} and /api/runs/{id}/log ---

// visibleRun returns the run id when the session can see its repository,
// its path and its project ID both; found is false otherwise, as for an
// unknown id. An API review has no repository: it is not found here, for an
// admin too.
func (d *Dashboard) visibleRun(ctx context.Context, se Session, id string) (store.Run, bool, error) {
	run, found, err := d.Store.Run(ctx, id)
	if err != nil || !found || run.Kind != store.KindMR {
		return store.Run{}, false, err
	}
	if !se.CanSeeRun(run.MR.Instance, run.ProjectPath, run.MR.ProjectID) {
		return store.Run{}, false, nil
	}
	return run, true, nil
}

// attemptsOf sets view's entry, from what run recorded, and the attempts of
// the review run is one of. A team is named by its name now (a team since
// removed reads "").
func (d *Dashboard) attemptsOf(ctx context.Context, run store.Run, view *RunDetailView) error {
	view.Attempts = []AttemptView{}
	if run.ChainLength > 0 {
		e := RunEntryView{Name: run.ModelEntryName, Position: run.ChainPosition, Length: run.ChainLength, Source: run.ModelSource}
		if run.ModelTeamID != nil {
			teams, err := d.teamNames(ctx)
			if err != nil {
				return err
			}
			e.Team = teams[*run.ModelTeamID]
		}
		view.Entry = &e
	}
	root := run.ID
	if run.AttemptOf != nil {
		root = *run.AttemptOf
	}
	attempts, err := d.Store.Attempts(ctx, root)
	if err != nil || len(attempts) < 2 {
		return err
	}
	for _, a := range attempts {
		v := AttemptView{RunID: a.ID, Name: a.ModelEntryName, Position: a.ChainPosition, Outcome: a.Outcome}
		if a.Outcome == store.OutcomeFellBack {
			v.Reason = a.OutcomeReason
		}
		view.Attempts = append(view.Attempts, v)
	}
	return nil
}

// notMergedNote is the incrementalNote of an incremental run whose result was
// not merged with the last review.
const notMergedNote = "Part of an incremental review that was never merged."

func (d *Dashboard) run(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	run, found, err := d.visibleRun(ctx, se, r.PathValue("id"))
	if err != nil {
		d.failed(w, r, "loading a run", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	digests, err := d.Store.MRRunDigests(ctx, run.MR, visibility(se))
	if err != nil {
		d.failed(w, r, "listing a review's runs", err)
		return
	}
	var digest store.RunDigest
	var prev *store.RunDigest
	if idx := slices.IndexFunc(digests, func(x store.RunDigest) bool { return x.ID == run.ID }); idx >= 0 {
		digest = digests[idx]
		if idx > 0 {
			prev = &digests[idx-1]
		}
	} else {
		// An attempt dropped for the next entry of its chain is no row of
		// the review list, but its review's attempts link to it: it is read
		// on its own, after the review's runs created before it.
		var found bool
		if digest, found, err = d.Store.RunDigest(ctx, run.ID); err != nil {
			d.failed(w, r, "loading a run", err)
			return
		}
		if !found || digest.Outcome != store.OutcomeFellBack {
			notFound(w)
			return
		}
		for i := range digests {
			if digests[i].CreatedAt.Before(digest.CreatedAt) {
				prev = &digests[i]
			}
		}
	}
	j := judge(digest)
	view := RunDetailView{
		Run: runSummary(digest, prev, j), Instance: run.MR.Instance, Project: run.ProjectPath, Number: run.MR.IID,
		Status: j.status, Approved: run.Approved, ApprovalReason: j.reason, Model: run.Model, Provider: run.Provider, MaxTurns: run.MaxTurns, Quiet: run.Quiet,
		CIJobs: ciResultViews(run.CIJobs),
		Files:  FilesView{Reviewed: []string{}, Skipped: []SkippedFile{}, NotReviewed: []NotReviewedFile{}, Rules: []FileRuleView{}},
		Skills: []SkillView{},
	}
	if run.Approved {
		view.ApprovalReason = ""
	}
	view.BuiltInRulesNote = run.BuiltInRulesNote
	// The deadline is shown only while the run waits for CI: it is kept after.
	if run.State == store.Waiting && run.SkipReason == store.WaitCIPending {
		view.CIDeadline = run.CIDeadline
	}
	// The cluster's names and its words about a failure are for admins: a
	// viewer's JSON has no kubernetes key at all.
	if run.JobName != "" && se.Role(run.MR.Instance, run.ProjectPath) >= RoleAdmin {
		view.Kubernetes = &KubernetesView{Namespace: d.cfg.Jobs.Namespace, Job: run.JobName, Pod: run.PodName,
			Detail: run.FailureDetail}
	}
	if err := d.attemptsOf(ctx, run, &view); err != nil {
		d.failed(w, r, "loading a review's attempts", err)
		return
	}
	if view.MultiReview, err = d.multiReviewOf(ctx, run); err != nil {
		d.failed(w, r, "loading a multi review", err)
		return
	}
	// The change's dismissals are applied to its newest decided review only,
	// the one a dismissal or restore decides again; an older run shows what
	// was published for it.
	latest, decided, err := d.Store.LatestDecidedRun(ctx, run.MR)
	if err != nil {
		d.failed(w, r, "loading the latest review", err)
		return
	}
	view.LatestDecided = decided && latest.ID == run.ID
	// The skills of a result the publisher could use: one that parses and
	// is for the run's own base and head.
	if res, err := result.Parse(run.Result); err == nil && res.Base == run.BaseSHA && res.Head == run.HeadSHA {
		var dismissals []store.Dismissal
		if view.LatestDecided {
			dismissals, err = d.Store.Dismissals(ctx, run.MR)
		}
		if err != nil {
			d.failed(w, r, "loading dismissals", err)
			return
		}
		view.PergentVersion = res.PergentVersion
		view.IncrementalNote = result.IncrementalNote(res.Incremental)
		if run.Incremental && res.Incremental == nil && res.Status == "completed" && run.State != store.Reported {
			// A run that reviewed only some files whose result was never
			// merged (a push superseded it, or the pod restarted): what
			// its page lists is a few files, not the whole change. A
			// reported run is still to be merged, and an error result
			// lists no files.
			view.IncrementalNote = notMergedNote
		}
		view.RulesUsed = res.Rules != nil
		fillSkills(&view, res, run.Result, run.SkillSources, run.SkillVersions, dismissals)
		// A re-run of failed skills stores the other skills' results with its
		// own, so the sum of the result counts their tokens a second time.
		// The usage table has only what this run used. Until it is filled in
		// the sum of the result stands, and so it does when the usage can't be
		// read: usage never breaks a review's page.
		if t, found, err := d.Store.RunUsage(ctx, run.ID); err != nil {
			d.Log.Warn("dashboard: reading a run's usage failed", "run", run.ID, "error", err.Error())
		} else if found {
			view.Tokens = TokensView{In: t.In, Out: t.Out}
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// skillExtra is what the view shows of a skill beyond result.Skill.
type skillExtra struct {
	Checks []struct {
		Batch  int `json:"batch"`
		Checks []struct {
			Section string `json:"section"`
			Name    string `json:"name"`
			Status  string `json:"status"`
			Detail  string `json:"detail"`
		} `json:"checks"`
	} `json:"checks"`
	Tokens struct {
		Input  int64 `json:"input"`
		Output int64 `json:"output"`
	} `json:"tokens"`
}

type notReviewedJSON struct {
	Path  string `json:"path"`
	Cause string `json:"cause"`
}

// fillSkills adds the skills, findings, checks, files and tokens of a
// parsed result (raw is the same result, for the fields result.Result
// doesn't declare), each skill with its recorded source and library version.
// A part that doesn't decode is left out, not guessed. Findings that
// dismissals cover are moved to the skill's Dismissed, and its verdict is the one the policy
// decides on with them set aside.
func fillSkills(view *RunDetailView, res *result.Result, raw []byte, sources map[string]string, versions map[string]int, dismissals []store.Dismissal) {
	var extra struct {
		Skills []json.RawMessage `json:"skills"`
	}
	_ = json.Unmarshal(raw, &extra)

	decided := render.ApplyDismissals(result.ToRender(res), actions.RenderDismissals(dismissals))
	at := map[string]time.Time{}
	for _, dm := range dismissals {
		at[dm.Fingerprint] = dm.CreatedAt
	}
	reviewed, skipped, notReviewed, ruled := map[string]bool{}, map[SkippedFile]bool{}, map[string]bool{}, map[string]bool{}
	for i, s := range res.Skills {
		sv := SkillView{Name: s.Name, Verdict: decided[i].Verdict, Findings: []FindingView{}, Dismissed: []FindingView{},
			Checks: []CheckView{}}
		if src, ok := sources[s.Name]; ok {
			sv.Source = &src
		}
		sv.Version = versions[s.Name]
		for _, f := range decided[i].Findings {
			sv.Findings = append(sv.Findings, findingView(f))
		}
		for _, df := range decided[i].Dismissed {
			fv := findingView(df.Finding)
			fv.Dismissed = &DismissedView{By: df.By, Reason: df.Reason, At: at[df.Finding.Fingerprint].UTC()}
			sv.Dismissed = append(sv.Dismissed, fv)
		}
		if i < len(extra.Skills) {
			var x skillExtra
			if json.Unmarshal(extra.Skills[i], &x) == nil {
				for _, batch := range x.Checks {
					for _, c := range batch.Checks {
						sv.Checks = append(sv.Checks, CheckView{Batch: batch.Batch, Section: c.Section, Name: c.Name,
							Status: c.Status, Detail: c.Detail})
					}
				}
				view.Tokens.In += x.Tokens.Input
				view.Tokens.Out += x.Tokens.Output
			}
		}
		for _, p := range s.Files.Reviewed {
			if !reviewed[p] {
				reviewed[p] = true
				view.Files.Reviewed = append(view.Files.Reviewed, p)
			}
		}
		// A file's rule is the same in every skill that had rules on.
		for _, fr := range s.Files.Rules {
			if !ruled[fr.Path] {
				ruled[fr.Path] = true
				view.Files.Rules = append(view.Files.Rules, FileRuleView{Path: fr.Path, Rule: fr.Rule})
			}
		}
		for _, sk := range s.Files.Skipped {
			f := SkippedFile{Path: sk.Path, Rule: sk.Rule}
			if !skipped[f] {
				skipped[f] = true
				view.Files.Skipped = append(view.Files.Skipped, f)
			}
		}
		for _, rawNR := range s.Files.NotReviewed {
			var nr notReviewedJSON
			if json.Unmarshal(rawNR, &nr) != nil || nr.Path == "" || notReviewed[nr.Path] {
				continue
			}
			notReviewed[nr.Path] = true
			view.Files.NotReviewed = append(view.Files.NotReviewed, NotReviewedFile(nr))
		}
		view.Skills = append(view.Skills, sv)
	}
}

// findingView is f as the page shows it.
func findingView(f render.Finding) FindingView {
	return FindingView{ID: f.ID, Fingerprint: f.Fingerprint, Severity: string(f.Severity), Title: f.Title, File: f.File,
		Line: int64(f.Line), Explanation: f.Explanation, SuggestedFix: f.SuggestedFix}
}

// reviewMembers returns the reviewer attempts of run's review, its off row
// left out, oldest first, and the review's runs, oldest first, when run is a
// multi review's: the run that was split or a later attempt of its judge. An
// attempt that fell back before the split (a later attempt was made before
// the first member) has none.
func (d *Dashboard) reviewMembers(ctx context.Context, run store.Run) ([]store.ReviewMember, []store.Run, error) {
	all, err := d.Store.ReviewMembers(ctx, run.ID)
	if err != nil {
		return nil, nil, err
	}
	members := slices.DeleteFunc(all, func(m store.ReviewMember) bool { return m.State == store.MemberOff })
	if len(members) == 0 {
		return nil, nil, nil
	}
	root := run.ID
	if run.AttemptOf != nil {
		root = *run.AttemptOf
	}
	attempts, err := d.Store.Attempts(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	split := members[0].CreatedAt
	if run.CreatedAt.Before(split) {
		for _, a := range attempts {
			if a.CreatedAt.After(run.CreatedAt) && a.CreatedAt.Before(split) {
				return nil, nil, nil
			}
		}
	}
	return members, attempts, nil
}

// judgeRuns is how many of attempts (a review's runs, oldest first) are its
// judge runs: the run that was split (the newest made before split, the
// time of the first member) and every later one.
func judgeRuns(attempts []store.Run, split time.Time) int {
	first := 0
	for i, a := range attempts {
		if a.CreatedAt.Before(split) {
			first = i
		}
	}
	return len(attempts) - first
}

// findingsIn counts the findings of every skill of a reviewer's result; 0
// for one that does not decode.
func findingsIn(raw []byte) int {
	var r struct {
		Skills []struct {
			Findings []json.RawMessage `json:"findings"`
		} `json:"skills"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return 0
	}
	n := 0
	for _, s := range r.Skills {
		n += len(s.Findings)
	}
	return n
}

// multiReviewOf is the multi review of run's page: one row per reviewer (its
// final attempt, why it has no result, its findings, tokens and attempts),
// the judge this run judged with, and the tokens of the whole review (every
// reviewer attempt and every judge run, the same on each attempt's page;
// each run's own tokens stay in its Tokens); or why its multi review did not
// start (its review_members off row); nil for a run that is no multi review.
// Model names are shown as the run's own model is, never a key or a base
// URL. The tokens never break the page: when they can't be read, the page
// shows none.
func (d *Dashboard) multiReviewOf(ctx context.Context, run store.Run) (*RunMultiReviewView, error) {
	members, attempts, err := d.reviewMembers(ctx, run)
	if err != nil {
		return nil, err
	}
	mv := &RunMultiReviewView{Reviewers: []ReviewerView{}}
	if len(members) == 0 {
		reason, found, err := d.Store.MultiReviewOffReason(ctx, run.ID)
		if err != nil || !found {
			return nil, err
		}
		mv.Off = reason
		return mv, nil
	}
	// Members are oldest first, so each reviewer's last is its final
	// attempt, and its result is its newest done or failed attempt's.
	rows := map[int]int{}
	served := map[int]bool{}
	for _, m := range members {
		i, ok := rows[m.Reviewer]
		if !ok {
			i = len(mv.Reviewers)
			rows[m.Reviewer] = i
			mv.Reviewers = append(mv.Reviewers, ReviewerView{Number: m.Reviewer, Attempts: []MemberAttemptView{}})
		}
		rv := &mv.Reviewers[i]
		name := m.EntryName()
		rv.Attempts = append(rv.Attempts, MemberAttemptView{ID: m.ID, Entry: name, Position: m.Entry.Position,
			Provider: m.Provider, Model: m.Model, State: string(m.State), Failure: m.FailureReason, Step: m.FailureStep})
		rv.Entry, rv.Provider, rv.Model, rv.State = name, m.Provider, m.Model, string(m.State)
		if m.Result != nil && (m.State == store.MemberDone || m.State == store.MemberFailed) {
			served[m.Reviewer] = true
			rv.Findings = findingsIn(m.Result)
		}
	}
	for i := range mv.Reviewers {
		rv := &mv.Reviewers[i]
		switch store.MemberState(rv.State) {
		case store.MemberQueued, store.MemberRunning, store.MemberReported:
			continue // no answer yet
		}
		if !served[rv.Number] {
			rv.Skipped = rv.Attempts[len(rv.Attempts)-1].Failure
			if rv.Skipped == "" {
				rv.Skipped = "the reviewer gave no result"
			}
		}
	}
	slices.SortStableFunc(mv.Reviewers, func(a, b ReviewerView) int { return a.Number - b.Number })
	j, judged, err := d.Store.JudgeOf(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	if judged {
		mv.Judge = &JudgeView{Entry: j.Entry.Name, Provider: j.Provider, Model: j.Model, NoJudge: j.NoJudge,
			Reason: j.NoJudgeReason, Attempts: judgeRuns(attempts, members[0].CreatedAt)}
	}
	costs, err := d.Store.ReviewCosts(ctx, run.ID)
	if err != nil {
		d.Log.Warn("dashboard: reading a multi review's tokens per model failed", "run", run.ID, "error", err.Error())
		return mv, nil
	}
	sorted := map[int]int{}
	for i, rv := range mv.Reviewers {
		sorted[rv.Number] = i
	}
	for _, c := range costs {
		mv.Total.In += c.Tokens.In
		mv.Total.Out += c.Tokens.Out
		switch i, ok := sorted[c.Reviewer]; {
		case c.Part == "judge" && mv.Judge != nil:
			mv.Judge.In += c.Tokens.In
			mv.Judge.Out += c.Tokens.Out
		case c.Part == "reviewer" && ok:
			mv.Reviewers[i].In += c.Tokens.In
			mv.Reviewers[i].Out += c.Tokens.Out
		}
	}
	return mv, nil
}

// logOwner is what a log or diagnostics route reads: the run when the
// session can see it, and on a member's route ({member} set) that reviewer
// attempt of the run's multi review. A member of another review, the off
// row, or one the run's page does not list, is not found, as an unknown one.
func (d *Dashboard) logOwner(r *http.Request) (run store.Run, member string, found bool, err error) {
	ctx := r.Context()
	run, found, err = d.visibleRun(ctx, session(r), r.PathValue("id"))
	if err != nil || !found {
		return store.Run{}, "", false, err
	}
	member = r.PathValue("member")
	if member == "" {
		return run, "", true, nil
	}
	members, _, err := d.reviewMembers(ctx, run)
	if err != nil {
		return store.Run{}, "", false, err
	}
	if !slices.ContainsFunc(members, func(m store.ReviewMember) bool { return m.ID == member }) {
		return store.Run{}, "", false, nil
	}
	return run, member, true, nil
}

// runLog answers the log of a run, or of a reviewer attempt of its multi
// review.
func (d *Dashboard) runLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run, member, found, err := d.logOwner(r)
	if err != nil {
		d.failed(w, r, "loading a run", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	var text string
	if member != "" {
		text, found, err = d.Store.ReviewMemberLog(ctx, member)
	} else {
		text, found, err = d.Store.RunLog(ctx, run.ID)
	}
	if err != nil {
		d.failed(w, r, "loading a run's log", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, LogView{Text: text})
}

// runDiagnostics answers what was kept of the Job of a run, or of a
// reviewer attempt of its multi review; below admin the clone log is masked.
func (d *Dashboard) runDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	run, member, found, err := d.logOwner(r)
	if err != nil {
		d.failed(w, r, "loading a run", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	var diag store.Diagnostics
	if member != "" {
		diag, found, err = d.Store.ReviewMemberDiagnostics(ctx, member)
	} else {
		diag, found, err = d.Store.RunDiagnostics(ctx, run.ID)
	}
	if err != nil {
		d.failed(w, r, "loading a run's diagnostics", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	if se.Role(run.MR.Instance, run.ProjectPath) < RoleAdmin {
		// The masked log hides linked repositories; it may be empty.
		writeJSON(w, http.StatusOK, DiagnosticsView{CloneLog: diag.CloneLogMasked})
		return
	}
	writeJSON(w, http.StatusOK, DiagnosticsView{CloneLog: diag.CloneLog, PodState: &diag.PodState})
}

// --- GET /api/repositories and /api/repositories/{instance}/{project...} ---

func (d *Dashboard) repositories(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	repos, err := d.visibleRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	views, err := d.repositoryViews(ctx, se, repos)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// repositoryViews describes repos with their last 7 days and health; a
// paused one's health is "paused", whatever setup problems are left.
func (d *Dashboard) repositoryViews(ctx context.Context, se Session, repos []settings.Effective) ([]RepositoryView, error) {
	active := byProject(repos)
	counts, err := d.Store.RunStats(ctx, visibleOf(se, repos), d.now().Add(-defaultDays*day))
	if err != nil {
		return nil, err
	}
	items, err := d.attention(ctx, active, RepoKey{})
	if err != nil {
		return nil, err
	}
	troubled := map[RepoKey]bool{}
	for _, a := range items {
		troubled[repoKey(a.Instance, a.Project)] = true
	}
	out := make([]RepositoryView, 0, len(repos))
	for _, e := range repos {
		k := repoKey(e.Instance, e.Project)
		c := counts[store.PathKey(k)]
		health := healthOK
		switch {
		case e.Paused:
			health = healthPaused
		case troubled[k]:
			health = healthNeedsAttention
		}
		out = append(out, RepositoryView{Instance: e.Instance, Project: e.Project, Platform: e.Platform,
			Source: string(e.Source), On: !e.Paused, Paused: e.Paused, Health: health,
			Last7: Last7View{Reviews: c.Reviews, Approved: c.Approved, Failed: c.Failed}})
	}
	return out, nil
}

// repository serves a repository's detail, with a trailing /inputs its
// skills and instructions, with a trailing /readiness its Readiness tab, with
// a trailing /past-dismissals its Past dismissals, with a trailing
// /skill-results its Skill results, with a trailing /skill-results/tokens
// (admins only) what each skill used on the changes those count, with a
// trailing /teams (admins only)
// the teams that cover it, or with a trailing /usage (admins only) what it
// used. A repository whose own path is the one asked for wins over the
// /inputs, /readiness, /past-dismissals, /skill-results, /skill-results/tokens, /teams or /usage of
// another.
func (d *Dashboard) repository(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	instance, rest := r.PathValue("instance"), r.PathValue("rest")
	repos, err := d.visibleRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	find := func(project string) (settings.Effective, bool) {
		i := slices.IndexFunc(repos, func(e settings.Effective) bool {
			return e.Instance == instance && strings.EqualFold(e.Project, project)
		})
		if i < 0 {
			return settings.Effective{}, false
		}
		return repos[i], true
	}
	if repo, ok := find(rest); ok {
		d.repositoryDetail(w, r, se, repo)
		return
	}
	if project, ok := strings.CutSuffix(rest, "/inputs"); ok {
		if repo, ok := find(project); ok {
			// 0 when the repository isn't stored yet: not synced.
			id, _, err := d.projectID(ctx, se, repo.Instance, repo.Project)
			if err != nil {
				d.failed(w, r, "loading a project", err)
				return
			}
			d.repositoryInputs(w, r, repo, id)
			return
		}
	}
	if project, ok := strings.CutSuffix(rest, "/readiness"); ok {
		if repo, ok := find(project); ok {
			d.repositoryReadiness(w, r, se, repo)
			return
		}
	}
	if project, ok := strings.CutSuffix(rest, "/past-dismissals"); ok {
		if repo, ok := find(project); ok {
			d.repositoryPastDismissals(w, r, se, repo)
			return
		}
	}
	// Before /skill-results: a path ending in /tokens is never that tab's.
	if project, ok := strings.CutSuffix(rest, "/skill-results/tokens"); ok {
		if repo, ok := find(project); ok {
			d.repositorySkillTokens(w, r, se, repo)
			return
		}
	}
	if project, ok := strings.CutSuffix(rest, "/skill-results"); ok {
		if repo, ok := find(project); ok {
			d.repositorySkillResults(w, r, se, repo)
			return
		}
	}
	if project, ok := strings.CutSuffix(rest, "/teams"); ok {
		if repo, ok := find(project); ok {
			d.repositoryTeams(w, r, se, repo)
			return
		}
	}
	if project, ok := strings.CutSuffix(rest, "/usage"); ok {
		if repo, ok := find(project); ok {
			d.repositoryUsage(w, r, se, repo)
			return
		}
	}
	if project, ok := strings.CutSuffix(rest, "/chain"); ok {
		if repo, ok := find(project); ok {
			d.repositoryChain(w, r, se, repo)
			return
		}
	}
	if project, ok := strings.CutSuffix(rest, "/reviewers"); ok {
		if repo, ok := find(project); ok {
			d.repositoryReviewers(w, r, se, repo)
			return
		}
	}
	if project, ok := strings.CutSuffix(rest, "/ci-job-names"); ok {
		if repo, ok := find(project); ok {
			d.ciJobNames(w, r, repo)
			return
		}
	}
	notFound(w)
}

func (d *Dashboard) repositoryDetail(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	view, err := d.detailView(r.Context(), se, repo)
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// detailView describes repo, switched on or paused, with its settings, setup problems
// and the session's role on it.
func (d *Dashboard) detailView(ctx context.Context, se Session, repo settings.Effective) (RepositoryDetailView, error) {
	views, err := d.repositoryViews(ctx, se, []settings.Effective{repo})
	if err != nil {
		return RepositoryDetailView{}, err
	}
	items, err := d.attention(ctx, byProject([]settings.Effective{repo}), repoKey(repo.Instance, repo.Project))
	if err != nil {
		return RepositoryDetailView{}, err
	}
	role := se.Role(repo.Instance, repo.Project)
	settingsView := settingViews(repo)
	if role < RoleAdmin {
		// The links name other repositories, which someone who may see this
		// one may not be allowed to see. The setting itself is shown.
		settingsView["linkedRepos"] = SettingView{Value: []LinkView{}}
		// The quota, like its usage, is for admins to see.
		delete(settingsView, "monthlyTokenQuota")
	}
	waiting, err := d.Store.WaitingChanges(ctx, repo.Instance, repo.Project)
	if err != nil {
		return RepositoryDetailView{}, err
	}
	wv := make([]WaitingView, 0, len(waiting))
	for _, c := range waiting {
		wv = append(wv, WaitingView{Number: c.IID, Head: c.Head, Title: c.Title, Author: c.Author, WebURL: c.WebURL, Since: c.Since, Reason: c.Reason})
	}
	gv, err := d.graphView(ctx, repo)
	if err != nil {
		return RepositoryDetailView{}, err
	}
	model, multi, err := d.repoChainSummary(ctx, repo)
	if err != nil {
		return RepositoryDetailView{}, err
	}
	owner, err := d.ownerTeamView(ctx, se, repo)
	if err != nil {
		return RepositoryDetailView{}, err
	}
	actions, err := d.repoActions(ctx, se, repo)
	if err != nil {
		return RepositoryDetailView{}, err
	}
	covering, err := d.coveringTeams(ctx, repo, actions.Remove)
	if err != nil {
		return RepositoryDetailView{}, err
	}
	return RepositoryDetailView{Repo: views[0], Settings: settingsView, Attention: items, Role: role.String(), Waiting: wv,
		InstanceTrustedAuthorsOnly: repo.InstanceTrustedAuthorsOnly, Model: model, MultiReview: multi,
		Quota: d.quotaView(ctx, se, repo), Graph: gv, BuiltInRulesOff: builtInRulesOff(d.cfg.Jobs.Image),
		OwnerTeam: owner, Actions: actions, CoveringTeams: covering}, nil
}

// coveringTeams are the names of the teams other than repo's owner team that
// list the repository themselves (their own entry, not the whole instance):
// the teams a Remove takes it from. Only a session that may remove is told,
// the rest get none. Sorted, never nil.
func (d *Dashboard) coveringTeams(ctx context.Context, repo settings.Effective, mayRemove bool) ([]string, error) {
	out := []string{}
	if !mayRemove {
		return out, nil
	}
	owner, _, err := d.Store.OwnerTeam(ctx, repo.Instance, repo.Project)
	if err != nil {
		return nil, err
	}
	teams, err := d.Store.Teams(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		if t.ID != owner && t.OwnEntry(repo.Instance, repo.Project) >= 0 {
			out = append(out, t.Name)
		}
	}
	slices.SortFunc(out, strings.Compare)
	return out, nil
}

// ownerTeamView is repo's owner team (Store.OwnerTeam) as its page shows
// it, nil for none. Open: the session is an admin or a member of the team.
func (d *Dashboard) ownerTeamView(ctx context.Context, se Session, repo settings.Effective) (*OwnerTeamView, error) {
	id, ok, err := d.Store.OwnerTeam(ctx, repo.Instance, repo.Project)
	if err != nil || !ok {
		return nil, err
	}
	t, found, err := d.Store.Team(ctx, id)
	if err != nil || !found {
		return nil, err // removed meanwhile: no owner team
	}
	member := slices.ContainsFunc(t.Members, func(m store.TeamMember) bool { return m.AccountID == se.AccountID })
	return &OwnerTeamView{ID: t.ID, Name: t.Name, Open: se.Admin || member}, nil
}

// builtInRulesOff is why a review with jobs.image image leaves chosen
// built-in rules out, "" when it loads them (config.BuiltInRulesListImage).
func builtInRulesOff(image string) string {
	if ok, reason := config.BuiltInRulesListImage(image); !ok {
		return "Reviews get no built-in rules for now: " + reason + "."
	}
	return ""
}

// graphsOn reports whether the install keeps code graphs.
func (d *Dashboard) graphsOn() bool { return d.graphs != nil && d.graphs.GraphsOn() }

// graphView is repo's Code graph switch and status. With the switch off
// nothing is read: its graph was deleted with it.
func (d *Dashboard) graphView(ctx context.Context, repo settings.Effective) (GraphView, error) {
	v := GraphView{Enabled: repo.GraphContext, Available: d.graphsOn()}
	if !repo.GraphContext {
		return v, nil
	}
	g, ok, err := d.Store.RepoGraph(ctx, repo.Instance, repo.Project)
	if err != nil || !ok {
		return v, err
	}
	v.Branch, v.Commit, v.SizeBytes, v.Nodes, v.Edges = g.Branch, g.Commit, g.SizeBytes, g.Nodes, g.Edges
	v.Building, v.Error = string(g.BuildState), g.Error
	if !g.BuiltAt.IsZero() {
		t := g.BuiltAt
		v.BuiltAt = &t
	}
	if !g.FailedAt.IsZero() {
		t := g.FailedAt
		v.FailedAt = &t
	}
	return v, nil
}

// settingViews is repo's settings as RepositoryDetailView shows them.
func settingViews(repo settings.Effective) map[string]SettingView {
	ov := repo.Overridden
	return map[string]SettingView{
		"approve":                          {repo.Approve, ov[settings.KeyApprove]},
		"reviewDrafts":                     {repo.ReviewDrafts, ov[settings.KeyReviewDrafts]},
		"inlineFindings":                   {repo.InlineFindings, ov[settings.KeyInlineFindings]},
		"commandsAuthorAndMaintainersOnly": {repo.CommandsAuthorAndMaintainersOnly, ov[settings.KeyCommandsAuthorAndMaintainersOnly]},
		"incrementalReviews":               {repo.IncrementalReviews, ov[settings.KeyIncrementalReviews]},
		"learnFromDismissals":              {repo.LearnFromDismissals, ov[settings.KeyLearnFromDismissals]},
		"quietReviews":                     {repo.QuietReviews, ov[settings.KeyQuietReviews]},
		"checkDeleted":                     {repo.CheckDeleted, ov[settings.KeyCheckDeleted]},
		"commentLayout":                    {layoutText(repo.CommentLayout), ov[settings.KeyCommentLayout]},
		"ciJobs":                           {ciJobsView(repo.CIJobs), ov[settings.KeyCIJobs]},
		"linkedReposEnabled":               {repo.LinkedReposEnabled, ov[settings.KeyLinkedReposEnabled]},
		"linkedRepos":                      {linksView(repo.LinkedRepos), len(repo.LinkedRepos) > 0},
		"failureStatus":                    {string(repo.FailureStatus), ov[settings.KeyFailureStatus]},
		"blockingStatus":                   {string(repo.BlockingStatus), ov[settings.KeyBlockingStatus]},
		"skills":                           {repo.Skills, ov[settings.KeySkills]},
		"repoSkills":                       {nonNil(repo.RepoSkills), ov[settings.KeyRepoSkills]},
		"jobTimeoutSeconds":                {int64(repo.JobTimeout / time.Second), ov[settings.KeyJobTimeout]},
		"maxTurns":                         {repo.MaxTurns, ov[settings.KeyMaxTurns]},
		"jobMemory":                        {quantityText(repo.JobMemory), ov[settings.KeyJobMemory]},
		"rerunLimitPerMR":                  {repo.RerunLimitPerMR, ov[settings.KeyRerunLimitPerMR]},
		"rerunLimitPerRepo":                {repo.RerunLimitPerRepo, ov[settings.KeyRerunLimitPerRepo]},
		"trustedAuthorsOnly":               {repo.TrustedAuthorsOnly, ov[settings.KeyTrustedAuthorsOnly]},
		"trustedAuthors":                   {nonNilAuthors(repo.TrustedAuthors), ov[settings.KeyTrustedAuthors]},
		"monthlyTokenQuota":                {repo.MonthlyTokenQuota, ov[settings.KeyMonthlyTokenQuota]},
		"ruleSets":                         {repo.RuleSets, ov[settings.KeyRuleSets]},
		"builtInRules":                     {nonNil(repo.BuiltInRules), ov[settings.KeyBuiltInRules]},
	}
}

// nonNilAuthors is as, or an empty list for none.
func nonNilAuthors(as []store.TrustedAuthor) []store.TrustedAuthor {
	if as == nil {
		return []store.TrustedAuthor{}
	}
	return as
}

// layoutText is a comment layout as the API shows it: its "block:state"
// entries, or the built-in layout (from pergent's render.DefaultLayout) when
// there are none. It is always a new list.
func layoutText(layout []string) []string {
	if len(layout) > 0 {
		return slices.Clone(layout)
	}
	blocks := render.DefaultLayout()
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.String()
	}
	return out
}

// nonNil is list, or an empty list for nil, so a view shows [] and not null.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func quantityText(q resource.Quantity) string {
	if q.IsZero() {
		return ""
	}
	return q.String()
}

// --- GET /api/repositories/{instance}/{project...}/inputs ---

// inputsCache keeps resolved skills and instructions per repository, and
// the resolutions in progress, so requests for the same repository share
// one set of platform calls.
type inputsCache struct {
	mu      sync.Mutex
	entries map[string]inputsEntry
	flights map[string]*inputsFlight
	// gen counts the invalidations: a read that began before one is not kept.
	gen uint64
}

// generation is the number of invalidations so far.
func (c *inputsCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// invalidate drops every kept view and lets the reads in progress finish for
// those already waiting on them without keeping or sharing what they read:
// a change of a skill, of the default list or of a repository's skills has
// been made since they began. Every write of those calls it.
func (c *inputsCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	clear(c.entries)
	clear(c.flights)
}

type inputsEntry struct {
	view InputsView
	at   time.Time
}

type inputsFlight struct {
	done chan struct{}
	view InputsView
	err  error
}

// repositoryInputs serves repo's skills and instructions; id is the
// platform ID of the project the user sees at its path.
func (d *Dashboard) repositoryInputs(w http.ResponseWriter, r *http.Request, repo settings.Effective, id int64) {
	v, err := d.cachedInputs(r.Context(), repo, id)
	switch {
	case errors.Is(err, errNotSynced):
		writeError(w, http.StatusServiceUnavailable, msgNotSynced)
	case err != nil:
		writeError(w, http.StatusBadGateway, msgInputsFailed)
	default:
		v.Rules = d.withBuiltInRules(v.Rules, repo)
		writeJSON(w, http.StatusOK, v)
	}
}

// cachedInputs returns repo's resolved skills and instructions: kept ones
// younger than 5 minutes, those of a resolution in progress, or a new
// resolution, bounded by 10 s and not cut short by the request that
// started it going away (others may wait for it). Failures aren't kept.
func (d *Dashboard) cachedInputs(ctx context.Context, repo settings.Effective, id int64) (InputsView, error) {
	c, key := d.inputs, repo.Instance+"\x00"+lower(repo.Project)+"\x00"+strconv.FormatInt(id, 10)
	c.mu.Lock()
	now := d.now()
	if e, ok := c.entries[key]; ok && now.Sub(e.at) < inputsTTL && !now.Before(e.at) {
		c.mu.Unlock()
		return e.view, nil
	}
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.view, f.err
		case <-ctx.Done():
			return InputsView{}, ctx.Err()
		}
	}
	f := &inputsFlight{done: make(chan struct{})}
	c.flights[key] = f
	gen := c.gen
	c.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), platformTimeout)
	f.view, f.err = d.resolveInputs(rctx, repo, id)
	cancel()
	if f.err != nil {
		d.Log.Warn("dashboard: reading skills and instructions failed", "instance", repo.Instance,
			"project", repo.Project, "error", f.err.Error())
	}

	c.mu.Lock()
	if c.flights[key] == f {
		delete(c.flights, key)
	}
	if f.err == nil && c.gen == gen {
		now := d.now()
		for k, e := range c.entries {
			if now.Sub(e.at) >= inputsTTL {
				delete(c.entries, k)
			}
		}
		c.entries[key] = inputsEntry{view: f.view, at: now}
	}
	c.mu.Unlock()
	close(f.done)
	return f.view, f.err
}

// resolveInputs reads repo's skills and instructions at its default branch,
// the way a review resolves them, from project id: the stored project at
// its path must be that one (one that had the path before, not yet
// replaced by a reconcile, is not synced).
func (d *Dashboard) resolveInputs(ctx context.Context, repo settings.Effective, id int64) (InputsView, error) {
	src, ok := d.sources.Get(repo.Instance)
	if !ok || src == nil {
		return InputsView{}, errors.New("no reader for the instance")
	}
	inst, _ := d.registry.Get(repo.Instance)
	p, found, err := d.Store.ProjectByPath(ctx, repo.Instance, repo.Project)
	if err != nil {
		return InputsView{}, err
	}
	if !found || p.ProjectID != id {
		return InputsView{}, errNotSynced
	}
	branch, err := src.DefaultBranch(ctx, p.ProjectID)
	if err != nil {
		return InputsView{}, err
	}
	if branch == "" {
		return InputsView{}, errors.New("the repository has no default branch")
	}
	// The skills a review would get: the repository's own list or the
	// default list, then its folders, as the engine resolves them. A name
	// that can't be had is shown with its problem, as a review would fail
	// on it, never dropped and never replaced by other skills.
	resolved, err := d.Store.ResolveLibrary(ctx, repo.Skills)
	if err != nil {
		return InputsView{}, err
	}
	lib := make(map[string]inputs.LibraryEntry, len(resolved.Skills))
	for name, sk := range resolved.Skills {
		lib[name] = inputs.LibraryEntry{Content: []byte(sk.Content), Version: sk.Version}
	}
	problems := map[string]string{}
	for _, name := range resolved.Missing {
		problems[name] = msgSkillMissing(name)
	}
	on := slices.Clone(repo.RepoSkills)
	var in *inputs.Inputs
	for {
		in, err = inputs.ResolveSkills(ctx, src, lib, on, p.ProjectID, branch, nil)
		var nf *inputs.NotFoundError
		var pe *inputs.ProblemError
		var name string
		switch {
		case errors.As(err, &nf):
			name = nf.Name
		case errors.As(err, &pe) && slices.Contains(on, pe.Name):
			// A turned-on skill that is malformed. A maintainer must still
			// see its switch to turn it off. (A broken ".pergent/skills"
			// file always runs and has no switch: it stays an error, as it
			// fails a review.)
			name = pe.Name
		default:
			name = ""
		}
		if name == "" {
			break
		}
		// ResolveSkills names one at a time: show it, ask again without it.
		if _, has := problems[name]; !has {
			problems[name] = err.Error()
		}
		on = slices.DeleteFunc(on, func(n string) bool { return n == name })
	}
	if err != nil {
		return InputsView{}, err
	}
	detected, err := inputs.Detect(ctx, src, p.ProjectID, branch)
	if err != nil {
		return InputsView{}, err
	}

	v := InputsView{Branch: branch, Skills: []InputSkill{}, Detected: []DetectedSkillView{}, Instructions: string(in.Instructions)}
	names := make([]string, 0, len(in.Skills)+len(problems))
	for name := range in.Skills {
		names = append(names, name)
	}
	for name := range problems {
		if _, ok := in.Skills[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if problem, bad := problems[name]; bad {
			v.Skills = append(v.Skills, InputSkill{Name: name, Problem: problem})
			continue
		}
		sk := InputSkill{Name: name, Source: in.Sources[name], Version: in.Versions[name], Content: string(in.Skills[name])}
		if sk.Source != inputs.SourceLibrary {
			sk.Folder = sk.Source
			sk.URL = repoSkillURL(inst, repo.Project, branch, in.Paths[name])
		}
		v.Skills = append(v.Skills, sk)
	}
	for _, f := range detected {
		v.Detected = append(v.Detected, DetectedSkillView{Name: f.Name, Folder: f.Folder, Problem: f.Problem,
			On: slices.Contains(repo.RepoSkills, f.Name)})
	}
	if v.Rules, err = d.rulesView(ctx, src, p.ProjectID, branch, repo); err != nil {
		return InputsView{}, err
	}
	return v, nil
}

// repoSkillURL is the address of the file of a repository skill on the
// platform's web site: GitLab <instance url>/<project>/-/blob/<branch>/<path>,
// GitHub <web url>/<owner>/<repo>/blob/<branch>/<path>, each segment escaped.
// It is "" when the file is unknown or the address isn't an http(s) one.
func repoSkillURL(inst instances.Instance, project, branch, file string) string {
	if file == "" {
		return ""
	}
	var base, blob string
	switch {
	case inst.GitLab != nil:
		base, blob = strings.TrimRight(inst.GitLab.URL, "/"), "/-/blob/"
	case inst.GitHub != nil:
		base, blob = githubWebURL(inst.GitHub.APIURL), "/blob/"
	default:
		return ""
	}
	return safeURL(base + "/" + escapePath(project) + blob + escapePath(branch) + "/" + escapePath(file))
}

// githubWebURL is the web address of the GitHub site whose API is at apiURL:
// github.com for api.github.com, the host for GitHub Enterprise Server's
// /api/v3.
func githubWebURL(apiURL string) string {
	apiURL = strings.TrimRight(apiURL, "/")
	if apiURL == "" || apiURL == github.DefaultAPIURL {
		return "https://github.com"
	}
	return strings.TrimSuffix(apiURL, "/api/v3")
}

// escapePath escapes each segment of p, keeping the slashes between them.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// --- admin: GET /api/admin/settings ---

func (d *Dashboard) adminSettings(w http.ResponseWriter, r *http.Request) {
	def, err := d.defaultsView(r.Context())
	if err != nil {
		d.failed(w, r, "reading the defaults", err)
		return
	}
	llm := d.registry.LLMStatus().LLM
	writeJSON(w, http.StatusOK, AdminSettingsView{Defaults: def,
		LLM: LLMView{Provider: llm.Provider, Model: llm.Model}})
}
