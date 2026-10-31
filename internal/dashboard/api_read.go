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

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
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

	attentionTokenExpiry = "token_expiry"

	msgLoadFailed   = "could not load the data, please try again"
	msgNotFound     = "not found"
	msgInputsFailed = "could not read the repository's skills and instructions from the platform, please try again"
	msgNotSynced    = "pergent.sh hasn't set this repository up yet; try again in a minute"
)

// errNotSynced is a repository reconcile hasn't stored yet.
var errNotSynced = errors.New("the repository isn't stored yet (reconcile hasn't reached it)")

var outcomes = map[string]bool{
	OutcomeRunning: true, OutcomeApproved: true, OutcomeBlocking: true, OutcomeDraft: true,
	OutcomeNoBlocking: true, OutcomeFailed: true, OutcomeSuperseded: true, OutcomeUnknown: true,
}

// readRoutes adds the read API to mux, every route behind Auth.Require.
func (d *Dashboard) readRoutes(mux *http.ServeMux) {
	get := func(pattern string, h http.HandlerFunc) { mux.Handle("GET "+pattern, d.Auth.Require(h)) }
	get("/api/overview", d.overview)
	get("/api/reviews", d.reviews)
	get("/api/reviews/{instance}/{rest...}", d.review)
	get("/api/runs/{id}", d.run)
	get("/api/runs/{id}/log", d.runLog)
	get("/api/repositories", d.repositories)
	get("/api/repositories/{instance}/{rest...}", d.repository)
	get("/api/skills", d.skills)
	get("/api/integrations", requireAdmin(d.integrations))
	get("/api/admin/settings", requireAdmin(d.adminSettings))
	// Anything else is 404, also the instance alone (which the mux would
	// otherwise redirect to the {rest...} routes).
	none := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { notFound(w) })
	get("/api/reviews/{instance}", none)
	get("/api/repositories/{instance}", none)
	mux.Handle("/api/", d.Auth.Require(none))
}

// requireAdmin serves h to admins (operators and admin-team members), 403
// to others.
func requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if se, _ := SessionFrom(r.Context()); !se.Admin {
			writeError(w, http.StatusForbidden, "only admins can see this")
			return
		}
		h(w, r)
	}
}

// requireOperator serves h to operators (password admins), 403 to others.
func requireOperator(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if se, _ := SessionFrom(r.Context()); !se.Operator {
			writeError(w, http.StatusForbidden, "only password admins can do this")
			return
		}
		h(w, r)
	}
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
// operator, else only those of its visible repositories, by instance and
// lower-cased path, with their IDs.
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
// on instance: its visible one, or for an operator the stored one; ok is
// false when there is none.
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
	var out []settings.Effective
	for _, e := range all {
		if se.CanSee(e.Instance, e.Project) {
			out = append(out, e)
		}
	}
	return out, nil
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
// only's when its Path isn't ""), and, when only is zero, the instance-wide
// ones the session may see: those of its own instance for an admin, every
// one for an operator.
func (d *Dashboard) attention(ctx context.Context, se Session, active map[RepoKey]settings.Effective, only RepoKey) ([]AttentionView, error) {
	items, err := d.Store.Attention(ctx)
	if err != nil {
		return nil, err
	}
	out := []AttentionView{}
	for _, a := range items {
		k := repoKey(a.Instance, a.Project)
		switch {
		case a.Project == "":
			if only.Path != "" || !(se.Operator || (se.Admin && a.Instance == se.Instance)) {
				continue
			}
		case only.Path != "" && k != only:
			continue
		default:
			if _, ok := active[k]; !ok {
				continue
			}
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
	repos, err := d.activeRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	active := byProject(repos)
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
	if view.Attention, err = d.attention(ctx, se, active, RepoKey{}); err != nil {
		d.failed(w, r, "listing setup problems", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// --- GET /api/reviews ---

// cursorJSON is a review cursor's content; the cursor is it, in JSON,
// base64url-encoded.
type cursorJSON struct {
	At time.Time `json:"t"`
	ID string    `json:"id"`
}

func encodeCursor(r store.RunDigest) string {
	b, _ := json.Marshal(cursorJSON{At: r.UpdatedAt, ID: r.ID})
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
// path, or for an operator the stored one.
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
	writeJSON(w, http.StatusOK, ReviewDetailView{Header: header, Runs: summaries})
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
// unknown id.
func (d *Dashboard) visibleRun(ctx context.Context, se Session, id string) (store.Run, bool, error) {
	run, found, err := d.Store.Run(ctx, id)
	if err != nil || !found {
		return store.Run{}, false, err
	}
	if !se.CanSeeRun(run.MR.Instance, run.ProjectPath, run.MR.ProjectID) {
		return store.Run{}, false, nil
	}
	return run, true, nil
}

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
	idx := slices.IndexFunc(digests, func(x store.RunDigest) bool { return x.ID == run.ID })
	if idx < 0 {
		notFound(w)
		return
	}
	j := judge(digests[idx])
	var prev *store.RunDigest
	if idx > 0 {
		prev = &digests[idx-1]
	}
	view := RunDetailView{
		Run: runSummary(digests[idx], prev, j), Instance: run.MR.Instance, Project: run.ProjectPath, Number: run.MR.IID,
		Status: j.status, Approved: run.Approved, ApprovalReason: j.reason, Model: run.Model,
		Files:  FilesView{Reviewed: []string{}, Skipped: []SkippedFile{}, NotReviewed: []NotReviewedFile{}},
		Skills: []SkillView{},
	}
	if run.Approved {
		view.ApprovalReason = ""
	}
	// The skills of a result the publisher could use: one that parses and
	// is for the run's own base and head.
	if res, err := result.Parse(run.Result); err == nil && res.Base == run.BaseSHA && res.Head == run.HeadSHA {
		view.PergentVersion = res.PergentVersion
		fillSkills(&view, res, run.Result, run.SkillSources)
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

type findingJSON struct {
	ID           string `json:"id"`
	Severity     string `json:"severity"`
	Title        string `json:"title"`
	File         string `json:"file"`
	Line         int64  `json:"line"`
	Explanation  string `json:"explanation"`
	SuggestedFix string `json:"suggested_fix"`
}

type notReviewedJSON struct {
	Path  string `json:"path"`
	Cause string `json:"cause"`
}

// fillSkills adds the skills, findings, checks, files and tokens of a
// parsed result (raw is the same result, for the fields result.Result
// doesn't declare), each skill with its recorded source. A part that
// doesn't decode is left out, not guessed.
func fillSkills(view *RunDetailView, res *result.Result, raw []byte, sources map[string]string) {
	var extra struct {
		Skills []json.RawMessage `json:"skills"`
	}
	_ = json.Unmarshal(raw, &extra)

	reviewed, skipped, notReviewed := map[string]bool{}, map[SkippedFile]bool{}, map[string]bool{}
	for i, s := range res.Skills {
		sv := SkillView{Name: s.Name, Verdict: s.Verdict, Findings: []FindingView{}, Checks: []CheckView{}}
		if src, ok := sources[s.Name]; ok {
			sv.Source = &src
		}
		for _, rawFinding := range s.Findings {
			var f findingJSON
			if json.Unmarshal(rawFinding, &f) != nil {
				f = findingJSON{Title: "(a finding pergent.sh couldn't read)"}
			}
			sv.Findings = append(sv.Findings, FindingView{ID: f.ID, Severity: f.Severity, Title: f.Title, File: f.File,
				Line: f.Line, Explanation: f.Explanation, SuggestedFix: f.SuggestedFix})
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

func (d *Dashboard) runLog(w http.ResponseWriter, r *http.Request) {
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
	text, found, err := d.Store.RunLog(ctx, run.ID)
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

// --- GET /api/repositories and /api/repositories/{instance}/{project...} ---

func (d *Dashboard) repositories(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	repos, err := d.activeRepos(ctx, se)
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

// repositoryViews describes repos with their last 7 days and health.
func (d *Dashboard) repositoryViews(ctx context.Context, se Session, repos []settings.Effective) ([]RepositoryView, error) {
	active := byProject(repos)
	counts, err := d.Store.RunStats(ctx, visibleOf(se, repos), d.now().Add(-defaultDays*day))
	if err != nil {
		return nil, err
	}
	items, err := d.attention(ctx, se, active, RepoKey{})
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
		if troubled[k] {
			health = healthNeedsAttention
		}
		out = append(out, RepositoryView{Instance: e.Instance, Project: e.Project, Platform: e.Platform,
			Source: string(e.Source), On: true, Health: health,
			Last7: Last7View{Reviews: c.Reviews, Approved: c.Approved, Failed: c.Failed}})
	}
	return out, nil
}

// repository serves a repository's detail, or with a trailing /inputs its
// skills and instructions. A repository whose own path is the one asked
// for wins over the /inputs of another.
func (d *Dashboard) repository(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	instance, rest := r.PathValue("instance"), r.PathValue("rest")
	repos, err := d.activeRepos(ctx, se)
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
			// 0 when an operator's repository isn't stored yet: not synced.
			id, _, err := d.projectID(ctx, se, repo.Instance, repo.Project)
			if err != nil {
				d.failed(w, r, "loading a project", err)
				return
			}
			d.repositoryInputs(w, r, repo, id)
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

// detailView describes repo, switched on, with its settings, setup problems
// and the session's role on it.
func (d *Dashboard) detailView(ctx context.Context, se Session, repo settings.Effective) (RepositoryDetailView, error) {
	views, err := d.repositoryViews(ctx, se, []settings.Effective{repo})
	if err != nil {
		return RepositoryDetailView{}, err
	}
	items, err := d.attention(ctx, se, byProject([]settings.Effective{repo}), repoKey(repo.Instance, repo.Project))
	if err != nil {
		return RepositoryDetailView{}, err
	}
	return RepositoryDetailView{Repo: views[0], Settings: settingViews(repo), Attention: items,
		Role: se.Role(repo.Instance, repo.Project).String()}, nil
}

// settingViews is repo's settings as RepositoryDetailView shows them.
func settingViews(repo settings.Effective) map[string]SettingView {
	ov := repo.Overridden
	return map[string]SettingView{
		"approve":           {repo.Approve, ov[settings.KeyApprove]},
		"reviewDrafts":      {repo.ReviewDrafts, ov[settings.KeyReviewDrafts]},
		"failureStatus":     {string(repo.FailureStatus), ov[settings.KeyFailureStatus]},
		"skills":            {repo.Skills, ov[settings.KeySkills]},
		"jobTimeoutSeconds": {int64(repo.JobTimeout / time.Second), ov[settings.KeyJobTimeout]},
		"jobMemory":         {quantityText(repo.JobMemory), ov[settings.KeyJobMemory]},
	}
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
	c.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), platformTimeout)
	f.view, f.err = d.resolveInputs(rctx, repo, id)
	cancel()
	if f.err != nil {
		d.Log.Warn("dashboard: reading skills and instructions failed", "instance", repo.Instance,
			"project", repo.Project, "error", f.err.Error())
	}

	c.mu.Lock()
	delete(c.flights, key)
	if f.err == nil {
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
	// A skill removed from the library since is left out, as in a review.
	skills, _ := repo.LibrarySkills(d.library)
	in, err := inputs.Resolve(ctx, src, d.library, config.Repository{Skills: skills}, p.ProjectID, branch, nil)
	if err != nil {
		return InputsView{}, err
	}
	names := make([]string, 0, len(in.Skills))
	for name := range in.Skills {
		names = append(names, name)
	}
	sort.Strings(names)
	v := InputsView{Branch: branch, Skills: []InputSkill{}, Instructions: string(in.Instructions)}
	for _, name := range names {
		v.Skills = append(v.Skills, InputSkill{Name: name, Source: in.Sources[name], Content: string(in.Skills[name])})
	}
	return v, nil
}

// --- GET /api/skills ---

func (d *Dashboard) skills(w http.ResponseWriter, _ *http.Request) {
	names := make([]string, 0, len(d.library))
	for name := range d.library {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]LibrarySkill, 0, len(names))
	for _, name := range names {
		out = append(out, LibrarySkill{Name: name, Content: string(d.library[name])})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- admin: GET /api/integrations and /api/admin/settings ---

var datePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

func (d *Dashboard) integrations(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	instance, platform := se.Instance, se.Platform
	if se.Operator {
		// An operator belongs to no instance: it names the one it wants.
		instance = r.URL.Query().Get("instance")
		inst, ok := d.registry.Get(instance)
		if !ok {
			notFound(w)
			return
		}
		platform = inst.Platform
	}
	repos, err := d.activeRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	stored, err := d.Store.Projects(ctx)
	if err != nil {
		d.failed(w, r, "listing projects", err)
		return
	}
	byPath := map[string]store.Project{}
	for _, p := range stored {
		if p.Instance == instance {
			byPath[lower(p.Path)] = p
		}
	}
	view := IntegrationView{Instance: instance, Platform: platform, Connected: d.connected(instance),
		Approver: approverKindToken, Repos: []IntegrationRepo{}}
	if inst, ok := d.registry.Get(instance); ok && inst.GitHub != nil && inst.GitHub.ApproveApp != nil {
		view.Approver = approverKindApp
	}
	items, err := d.Store.Attention(ctx)
	if err != nil {
		d.failed(w, r, "listing setup problems", err)
		return
	}
	for _, a := range items {
		if a.Instance != instance || a.Project != "" || a.Kind != attentionTokenExpiry {
			continue
		}
		if t, err := time.Parse(time.DateOnly, datePattern.FindString(a.Message)); err == nil {
			view.TokenExpiry = &t
		}
	}
	for _, e := range repos {
		if e.Instance != instance {
			continue
		}
		p, found := byPath[lower(e.Project)]
		installed := found && p.InstallationID != 0
		if e.Platform == config.PlatformGitLab {
			installed = found && p.HookID != 0 && p.DeployTokenID != 0
		}
		view.Repos = append(view.Repos, IntegrationRepo{Project: e.Project, Installed: installed,
			ApproverReachable: found && p.ApproveAvailable})
	}
	writeJSON(w, http.StatusOK, view)
}

// connected reports whether pergent.sh is connected to the instance: its
// review side serves the dashboard. Without that wiring (no Sources) every
// instance counts as connected.
func (d *Dashboard) connected(instance string) bool {
	if d.sources == nil {
		return true
	}
	_, ok := d.sources.Get(instance)
	return ok
}

func (d *Dashboard) adminSettings(w http.ResponseWriter, r *http.Request) {
	se := session(r)
	def, err := d.defaultsView(r.Context())
	if err != nil {
		d.failed(w, r, "reading the defaults", err)
		return
	}
	llm := d.registry.LLMStatus().LLM
	writeJSON(w, http.StatusOK, AdminSettingsView{Instance: se.Instance, Defaults: def,
		LLM: LLMView{Provider: llm.Provider, Model: llm.Model}})
}
