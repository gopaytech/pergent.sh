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
	// reviewBatch is how many reviews are read from the store at a time
	// while a page is filled, and maxReviewBatches how many batches one
	// page reads at most: a filter that matches little returns a shorter
	// page with a cursor rather than scanning everything.
	reviewBatch      = 100
	maxReviewBatches = 10
	// recentReviews is how many reviews the overview lists.
	recentReviews = 10

	defaultDays     = 7
	maxOverviewDays = 90
	maxReviewDays   = 365
	day             = 24 * time.Hour

	// inputsTTL is how long a repository's resolved skills and
	// instructions are kept.
	inputsTTL = 5 * time.Minute

	attentionTokenExpiry = "token_expiry"

	msgLoadFailed   = "could not load the data, please try again"
	msgNotFound     = "not found"
	msgInputsFailed = "could not read the repository's skills and instructions from the platform, please try again"
)

var outcomes = map[string]bool{
	OutcomeRunning: true, OutcomeApproved: true, OutcomeBlocking: true, OutcomeDraft: true,
	OutcomeNoBlocking: true, OutcomeFailed: true, OutcomeSuperseded: true,
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
	get("/api/integrations", adminOnly(d.integrations))
	get("/api/admin/settings", adminOnly(d.adminSettings))
	// Anything else is 404, also the instance alone (which the mux would
	// otherwise redirect to the {rest...} routes).
	none := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { notFound(w) })
	get("/api/reviews/{instance}", none)
	get("/api/repositories/{instance}", none)
	mux.Handle("/api/", d.Auth.Require(none))
}

// adminOnly serves h to admins of the session's instance, 403 to others.
func adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if se, _ := SessionFrom(r.Context()); !se.IsAdmin {
			writeError(w, http.StatusForbidden, "only admins can see this")
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

// visibleProjects is the session's visible repositories, lower-cased and
// sorted: the only projects any query reads.
func visibleProjects(se Session) []string {
	out := make([]string, 0, len(se.Visible))
	for p, ok := range se.Visible {
		if ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// activeRepos returns the repositories switched on for the session's
// instance and platform that its user can see, in the resolver's order.
func (d *Dashboard) activeRepos(ctx context.Context, se Session) ([]settings.Effective, error) {
	all, err := d.Repos.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []settings.Effective
	for _, e := range all {
		if e.Instance == se.Instance && e.Platform == se.Platform && se.CanSee(e.Project) {
			out = append(out, e)
		}
	}
	return out, nil
}

func byProject(repos []settings.Effective) map[string]settings.Effective {
	m := make(map[string]settings.Effective, len(repos))
	for _, e := range repos {
		m[lower(e.Project)] = e
	}
	return m
}

func lowerAll(repos []settings.Effective) []string {
	out := make([]string, 0, len(repos))
	for _, e := range repos {
		out = append(out, lower(e.Project))
	}
	return out
}

// judgeDigest decides a listed run's outcome under its repository's
// settings.
func (d *Dashboard) judgeDigest(r store.RunDigest, active map[string]settings.Effective) judged {
	repo, ok := active[lower(r.ProjectPath)]
	return judge(digestFacts(r), d.policyFor(r.MR.Instance, repo, ok))
}

func (d *Dashboard) summary(r store.RunDigest, platform string, j judged) ReviewSummary {
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

// attention returns the setup problems of the session's instance for the
// repositories in active (only project's when it isn't ""), and, when
// project is "" and the user is an admin, the instance-wide ones.
func (d *Dashboard) attention(ctx context.Context, se Session, active map[string]settings.Effective, project string) ([]AttentionView, error) {
	items, err := d.Store.Attention(ctx)
	if err != nil {
		return nil, err
	}
	out := []AttentionView{}
	for _, a := range items {
		if a.Instance != se.Instance {
			continue
		}
		switch {
		case a.Project == "":
			if project != "" || !se.IsAdmin {
				continue
			}
		case project != "" && !strings.EqualFold(a.Project, project):
			continue
		default:
			if _, ok := active[lower(a.Project)]; !ok {
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
	platform, _ := d.cfg.Platform(se.Instance)
	repos, err := d.activeRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	active := byProject(repos)
	projects := visibleProjects(se)

	runs, err := d.Store.RunDigestsSince(ctx, se.Instance, projects, d.now().Add(-time.Duration(days)*day))
	if err != nil {
		d.failed(w, r, "counting reviews", err)
		return
	}
	var stats StatsView
	for _, run := range runs {
		stats.Reviews++
		switch d.judgeDigest(run, active).outcome {
		case OutcomeApproved:
			stats.Approved++
		case OutcomeBlocking:
			stats.Blocking++
		case OutcomeFailed:
			stats.Failed++
		}
	}
	recent, err := d.Store.ReviewDigests(ctx, store.ReviewFilter{Instance: se.Instance, Projects: projects, Limit: recentReviews})
	if err != nil {
		d.failed(w, r, "listing recent reviews", err)
		return
	}
	view := OverviewView{Stats: stats, Recent: []ReviewSummary{}}
	for _, run := range recent {
		view.Recent = append(view.Recent, d.summary(run, platform, d.judgeDigest(run, active)))
	}
	if view.Attention, err = d.attention(ctx, se, active, ""); err != nil {
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
	filter := store.ReviewFilter{Instance: se.Instance, Projects: visibleProjects(se), Limit: reviewBatch}
	if p := q.Get("project"); p != "" {
		filter.Projects = nil
		if se.CanSee(p) {
			filter.Projects = []string{lower(p)}
		}
	}
	if q.Has("days") {
		days, ok := parseDays(r, 0, maxReviewDays)
		if !ok {
			writeError(w, http.StatusBadRequest, "days must be a number from 1 to 365")
			return
		}
		filter.Since = d.now().Add(-time.Duration(days) * day)
	}
	if c := q.Get("cursor"); c != "" {
		cur, ok := decodeCursor(c)
		if !ok {
			writeError(w, http.StatusBadRequest, "malformed cursor")
			return
		}
		filter.After = cur
	}
	platform, _ := d.cfg.Platform(se.Instance)
	repos, err := d.activeRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	active := byProject(repos)

	view := ReviewListView{Items: []ReviewSummary{}}
	var last *store.RunDigest
	for batch := 0; batch < maxReviewBatches; batch++ {
		rows, err := d.Store.ReviewDigests(ctx, filter)
		if err != nil {
			d.failed(w, r, "listing reviews", err)
			return
		}
		for i := range rows {
			run := rows[i]
			j := d.judgeDigest(run, active)
			if outcome != "" && j.outcome != outcome {
				last = &rows[i]
				continue
			}
			if len(view.Items) == reviewPageSize {
				// One more matches: the page ends at its last item.
				view.NextCursor = encodeCursor(*last)
				writeJSON(w, http.StatusOK, view)
				return
			}
			view.Items = append(view.Items, d.summary(run, platform, j))
			last = &rows[i]
		}
		if len(rows) < reviewBatch {
			// Nothing more to read.
			writeJSON(w, http.StatusOK, view)
			return
		}
		filter.After = &store.ReviewCursor{At: rows[len(rows)-1].UpdatedAt, ID: rows[len(rows)-1].ID}
	}
	// Read as much as one page may: the next page goes on from here.
	view.NextCursor = encodeCursor(*last)
	writeJSON(w, http.StatusOK, view)
}

// --- GET /api/reviews/{instance}/{project...}/{number} ---

func (d *Dashboard) review(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	instance, rest := r.PathValue("instance"), r.PathValue("rest")
	i := strings.LastIndex(rest, "/")
	if instance != se.Instance || i <= 0 {
		notFound(w)
		return
	}
	project := rest[:i]
	number, err := strconv.ParseInt(rest[i+1:], 10, 64)
	if err != nil || number <= 0 || !se.CanSee(project) {
		notFound(w)
		return
	}
	mr, found, err := d.Store.FindMR(ctx, instance, project, number)
	if err != nil {
		d.failed(w, r, "finding a review", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	runs, err := d.Store.MRRunDigests(ctx, mr)
	if err != nil {
		d.failed(w, r, "listing a review's runs", err)
		return
	}
	if len(runs) == 0 {
		notFound(w)
		return
	}
	repos, err := d.activeRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	active := byProject(repos)
	platform, _ := d.cfg.Platform(instance)

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
	summaries := runSummaries(runs, func(run store.RunDigest) judged { return d.judgeDigest(run, active) })
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

// visibleRun returns the run id when the session can see its repository;
// found is false otherwise, as for an unknown id.
func (d *Dashboard) visibleRun(ctx context.Context, se Session, id string) (store.Run, bool, error) {
	run, found, err := d.Store.Run(ctx, id)
	if err != nil || !found {
		return store.Run{}, false, err
	}
	if run.MR.Instance != se.Instance || !se.CanSee(run.ProjectPath) {
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
	digests, err := d.Store.MRRunDigests(ctx, run.MR)
	if err != nil {
		d.failed(w, r, "listing a review's runs", err)
		return
	}
	idx := slices.IndexFunc(digests, func(x store.RunDigest) bool { return x.ID == run.ID })
	if idx < 0 {
		notFound(w)
		return
	}
	repos, err := d.activeRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	repo, ok := byProject(repos)[lower(run.ProjectPath)]
	pol := d.policyFor(run.MR.Instance, repo, ok)
	facts := digestFacts(digests[idx])
	facts.result = run.Result
	j := judge(facts, pol)
	var prev *store.RunDigest
	if idx > 0 {
		prev = &digests[idx-1]
	}

	view := RunDetailView{
		Run: runSummary(digests[idx], prev, j), Instance: run.MR.Instance, Project: run.ProjectPath, Number: run.MR.IID,
		Status: j.status, Approved: j.outcome == OutcomeApproved, ApprovalReason: j.reason, Model: d.cfg.LLM.Model,
		Files:  FilesView{Reviewed: []string{}, Skipped: []SkippedFile{}, NotReviewed: []NotReviewedFile{}},
		Skills: []SkillView{},
	}
	if o, _ := policyOutcome(facts); o.Result != nil {
		view.PergentVersion = o.Result.PergentVersion
		d.fillSkills(&view, o.Result, run.Result)
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
// doesn't declare). A part that doesn't decode is left out, not guessed.
func (d *Dashboard) fillSkills(view *RunDetailView, res *result.Result, raw []byte) {
	var extra struct {
		Skills []json.RawMessage `json:"skills"`
	}
	_ = json.Unmarshal(raw, &extra)

	reviewed, skipped, notReviewed := map[string]bool{}, map[SkippedFile]bool{}, map[string]bool{}
	for i, s := range res.Skills {
		sv := SkillView{Name: s.Name, Verdict: s.Verdict, Source: sourceKindRepo,
			Findings: []FindingView{}, Checks: []CheckView{}}
		if _, ok := d.library[s.Name]; ok {
			sv.Source = sourceKindLibrary
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
	runs, err := d.Store.RunDigestsSince(ctx, se.Instance, lowerAll(repos), d.now().Add(-defaultDays*day))
	if err != nil {
		return nil, err
	}
	last7 := map[string]*Last7View{}
	for _, e := range repos {
		last7[lower(e.Project)] = &Last7View{}
	}
	for _, run := range runs {
		l := last7[lower(run.ProjectPath)]
		if l == nil {
			continue
		}
		l.Reviews++
		switch d.judgeDigest(run, active).outcome {
		case OutcomeApproved:
			l.Approved++
		case OutcomeFailed:
			l.Failed++
		}
	}
	items, err := d.attention(ctx, se, active, "")
	if err != nil {
		return nil, err
	}
	troubled := map[string]bool{}
	for _, a := range items {
		troubled[lower(a.Project)] = true
	}
	out := make([]RepositoryView, 0, len(repos))
	for _, e := range repos {
		health := healthOK
		if troubled[lower(e.Project)] {
			health = healthNeedsAttention
		}
		out = append(out, RepositoryView{Instance: e.Instance, Project: e.Project, Platform: e.Platform,
			Source: string(e.Source), On: true, Last7: *last7[lower(e.Project)], Health: health})
	}
	return out, nil
}

// repository serves a repository's detail, or with a trailing /inputs its
// skills and instructions. A repository whose own path is the one asked
// for wins over the /inputs of another.
func (d *Dashboard) repository(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	instance, rest := r.PathValue("instance"), r.PathValue("rest")
	if instance != se.Instance {
		notFound(w)
		return
	}
	repos, err := d.activeRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	find := func(project string) (settings.Effective, bool) {
		i := slices.IndexFunc(repos, func(e settings.Effective) bool { return strings.EqualFold(e.Project, project) })
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
			d.repositoryInputs(w, r, repo)
			return
		}
	}
	notFound(w)
}

func (d *Dashboard) repositoryDetail(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	ctx := r.Context()
	views, err := d.repositoryViews(ctx, se, []settings.Effective{repo})
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	items, err := d.attention(ctx, se, byProject([]settings.Effective{repo}), repo.Project)
	if err != nil {
		d.failed(w, r, "listing setup problems", err)
		return
	}
	ov := repo.Overridden
	writeJSON(w, http.StatusOK, RepositoryDetailView{
		Repo: views[0],
		Settings: map[string]SettingView{
			"approve":           {repo.Approve, ov[settings.KeyApprove]},
			"reviewDrafts":      {repo.ReviewDrafts, ov[settings.KeyReviewDrafts]},
			"failureStatus":     {string(repo.FailureStatus), ov[settings.KeyFailureStatus]},
			"skills":            {repo.Skills, ov[settings.KeySkills]},
			"jobTimeoutSeconds": {int64(repo.JobTimeout / time.Second), ov[settings.KeyJobTimeout]},
			"jobMemory":         {quantityText(repo.JobMemory), ov[settings.KeyJobMemory]},
		},
		Attention: items,
	})
}

func quantityText(q resource.Quantity) string {
	if q.IsZero() {
		return ""
	}
	return q.String()
}

// --- GET /api/repositories/{instance}/{project...}/inputs ---

// inputsCache keeps resolved skills and instructions per repository.
type inputsCache struct {
	mu      sync.Mutex
	entries map[string]inputsEntry
}

type inputsEntry struct {
	view InputsView
	at   time.Time
}

func (c *inputsCache) get(key string, now time.Time) (InputsView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.Sub(e.at) >= inputsTTL || now.Before(e.at) {
		return InputsView{}, false
	}
	return e.view, true
}

func (c *inputsCache) put(key string, v InputsView, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if now.Sub(e.at) >= inputsTTL {
			delete(c.entries, k)
		}
	}
	c.entries[key] = inputsEntry{view: v, at: now}
}

func (d *Dashboard) repositoryInputs(w http.ResponseWriter, r *http.Request, repo settings.Effective) {
	key := repo.Instance + "\x00" + lower(repo.Project)
	if v, ok := d.inputs.get(key, d.now()); ok {
		writeJSON(w, http.StatusOK, v)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), platformTimeout)
	defer cancel()
	v, err := d.resolveInputs(ctx, repo)
	if err != nil {
		d.Log.Warn("dashboard: reading skills and instructions failed", "instance", repo.Instance,
			"project", repo.Project, "error", err.Error())
		writeError(w, http.StatusBadGateway, msgInputsFailed)
		return
	}
	d.inputs.put(key, v, d.now())
	writeJSON(w, http.StatusOK, v)
}

// resolveInputs reads repo's skills and instructions at its default branch,
// the way a review resolves them.
func (d *Dashboard) resolveInputs(ctx context.Context, repo settings.Effective) (InputsView, error) {
	src, ok := d.sources[repo.Instance]
	if !ok || src == nil {
		return InputsView{}, errors.New("no reader for the instance")
	}
	p, found, err := d.Store.ProjectByPath(ctx, repo.Instance, repo.Project)
	if err != nil {
		return InputsView{}, err
	}
	if !found {
		return InputsView{}, errors.New("the repository isn't stored yet (reconcile hasn't reached it)")
	}
	branch, err := src.DefaultBranch(ctx, p.ProjectID)
	if err != nil {
		return InputsView{}, err
	}
	if branch == "" {
		return InputsView{}, errors.New("the repository has no default branch")
	}
	in, err := inputs.Resolve(ctx, src, d.library, config.Repository{Skills: repo.Skills}, p.ProjectID, branch, nil)
	if err != nil {
		return InputsView{}, err
	}
	picked := map[string]bool{}
	if len(repo.Skills) == 0 {
		for name := range d.library {
			picked[name] = true
		}
	}
	for _, name := range repo.Skills {
		picked[name] = true
	}
	names := make([]string, 0, len(in.Skills))
	for name := range in.Skills {
		names = append(names, name)
	}
	sort.Strings(names)
	v := InputsView{Branch: branch, Skills: []InputSkill{}, Instructions: string(in.Instructions)}
	for _, name := range names {
		source := sourceKindRepo
		if _, inLib := d.library[name]; inLib && picked[name] && !slices.Contains(in.Overridden, name) {
			source = sourceKindLibrary
		}
		v.Skills = append(v.Skills, InputSkill{Name: name, Source: source, Content: string(in.Skills[name])})
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
		if p.Instance == se.Instance {
			byPath[lower(p.Path)] = p
		}
	}
	view := IntegrationView{Instance: se.Instance, Platform: se.Platform, Connected: true,
		Approver: approverKindToken, Repos: []IntegrationRepo{}}
	if gi, ok := d.cfg.GitHubInstance(se.Instance); ok && gi.ApproveApp != nil {
		view.Approver = approverKindApp
	}
	items, err := d.Store.Attention(ctx)
	if err != nil {
		d.failed(w, r, "listing setup problems", err)
		return
	}
	for _, a := range items {
		if a.Instance != se.Instance || a.Project != "" || a.Kind != attentionTokenExpiry {
			continue
		}
		if t, err := time.Parse(time.DateOnly, datePattern.FindString(a.Message)); err == nil {
			view.TokenExpiry = &t
		}
	}
	for _, e := range repos {
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

func (d *Dashboard) adminSettings(w http.ResponseWriter, r *http.Request) {
	se := session(r)
	def := d.Repos.Defaults(se.Platform, se.Instance)
	view := AdminSettingsView{
		Instance: se.Instance,
		Defaults: DefaultsView{Approve: def.Approve, ReviewDrafts: def.ReviewDrafts, FailureStatus: string(def.FailureStatus),
			JobTimeoutSeconds: int64(def.JobTimeout / time.Second), JobMemory: quantityText(def.JobMemory)},
		LLM:    LLMView{Provider: d.cfg.LLM.Provider, Model: d.cfg.LLM.Model},
		Admins: []AdminView{},
	}
	for _, a := range d.cfg.Dashboard.Admins {
		if a.Instance == se.Instance {
			view.Admins = append(view.Admins, AdminView{Instance: a.Instance, Group: a.Group, Team: a.Team})
		}
	}
	writeJSON(w, http.StatusOK, view)
}
