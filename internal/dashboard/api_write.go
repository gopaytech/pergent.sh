package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// maxAvailable is the most repositories the picker lists.
	maxAvailable = 500
	// listLimit is the most repositories read from each side (the admin's
	// and the review bot's or App's) for the picker.
	listLimit = 5000
	// listBudget bounds each side's listing for the picker; each request
	// of it has platformTimeout. What was read when it runs out is used.
	listBudget = 60 * time.Second
	// maxSearch is the longest picker search.
	maxSearch = 100
	// catalogTTL is how long the review side's list is kept.
	catalogTTL = 5 * time.Minute
	// maxWriteBody bounds a write's body.
	maxWriteBody = 64 << 10

	minJobTimeout = time.Minute
	maxJobTimeout = 2 * time.Hour

	msgListFailed     = "could not list the repositories on the platform, please try again"
	msgConfigManaged  = "this repository is managed by the config file; change it there"
	msgAlreadyOn      = "this repository is switched on already"
	msgWriteFailed    = "could not save the change, please try again"
	msgBadProjectBody = `the body must be {"project": "<path>"}`
	msgBadSettings    = "the body must be a JSON object of settings: approve, reviewDrafts, failureStatus, skills, jobTimeoutSeconds, jobMemory"
)

var (
	minJobMemory = resource.MustParse("256Mi")
	maxJobMemory = resource.MustParse("32Gi")
)

// writeRoutes adds the write API to mux: every route behind Auth.Require
// (which checks X-CSRF-Token) and for admins only (403 otherwise).
func (d *Dashboard) writeRoutes(mux *http.ServeMux) {
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, d.Auth.Require(adminOnly(h))) }
	admin("GET /api/repositories/available", d.available)
	admin("POST /api/repositories", d.switchOn)
	admin("DELETE /api/repositories/{instance}/{rest...}", d.switchOff)
	admin("PUT /api/repositories/{instance}/{rest...}", d.putSettings)
	admin("POST /api/repositories/{instance}/{rest...}", d.resetSettings)
	// The instance alone is 404, not a redirect to the {rest...} routes.
	none := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { notFound(w) })
	for _, method := range []string{"DELETE", "PUT", "POST"} {
		mux.Handle(method+" /api/repositories/{instance}", d.Auth.Require(none))
	}
}

// --- GET /api/repositories/available ---

func (d *Dashboard) available(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > maxSearch {
		writeError(w, http.StatusBadRequest, "search must be at most 100 characters")
		return
	}
	on, err := d.onProjects(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	reach, truncated, err := d.reachable(ctx, se, search)
	if err != nil {
		d.Log.Warn("dashboard: listing the repositories that can be switched on failed",
			"instance", se.Instance, "user", se.Username, "error", err.Error())
		writeError(w, http.StatusBadGateway, msgListFailed)
		return
	}
	keys := make([]string, 0, len(reach))
	for k := range reach {
		if !on[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	view := AvailableView{Items: []AvailableRepo{}, Truncated: truncated}
	for _, k := range keys {
		if len(view.Items) == maxAvailable {
			view.Truncated = true
			break
		}
		view.Items = append(view.Items, AvailableRepo{Instance: se.Instance, Project: reach[k], Platform: se.Platform})
	}
	writeJSON(w, http.StatusOK, view)
}

// onProjects is the lower-cased paths of the repositories switched on for
// the session's instance, in the config or the dashboard, seen or not.
func (d *Dashboard) onProjects(ctx context.Context, se Session) (map[string]bool, error) {
	all, err := d.Repos.List(ctx)
	if err != nil {
		return nil, err
	}
	on := map[string]bool{}
	for _, e := range all {
		if e.Instance == se.Instance {
			on[lower(e.Project)] = true
		}
	}
	return on, nil
}

// reachable returns the repositories of the session's instance holding
// search that both its user (a member with read access, asked with their
// token now) and the review bot or App (asked at most every 5 minutes)
// reach, by lower-cased path, each spelled as the user's list has it.
// truncated is true when a list was too long, or too slow, to read whole.
func (d *Dashboard) reachable(ctx context.Context, se Session, search string) (map[string]string, bool, error) {
	type listed struct {
		repos     []string
		truncated bool
		err       error
	}
	review := make(chan listed, 1)
	go func() {
		repos, truncated, err := d.catalogRepos(ctx, se.Instance, search)
		review <- listed{repos, truncated, err}
	}()
	mine, mineTruncated, err := d.memberRepos(ctx, se, search)
	bot := <-review
	if err != nil {
		return nil, false, fmt.Errorf("listing the user's repositories: %w", err)
	}
	if bot.err != nil {
		return nil, false, fmt.Errorf("listing the review bot's or App's repositories: %w", bot.err)
	}
	reached := make(map[string]bool, len(bot.repos))
	for _, p := range bot.repos {
		reached[lower(p)] = true
	}
	out := map[string]string{}
	for _, p := range mine {
		if k := lower(p); reached[k] && out[k] == "" {
			out[k] = p
		}
	}
	return out, mineTruncated || bot.truncated, nil
}

// withinBudget runs a listing under d.listBudget. When the budget runs out
// after something was read, what was read is the answer, truncated; an
// error before that, or with nothing read, is an error.
func (d *Dashboard) withinBudget(ctx context.Context, list func(ctx context.Context) ([]string, bool, error)) ([]string, bool, error) {
	bctx, cancel := context.WithTimeout(ctx, d.listBudget)
	defer cancel()
	repos, truncated, err := list(bctx)
	if err != nil {
		if bctx.Err() != nil && ctx.Err() == nil && len(repos) > 0 {
			return repos, true, nil
		}
		return nil, false, err
	}
	return repos, truncated, nil
}

// memberRepos lists the session user's repositories holding search with
// their token.
func (d *Dashboard) memberRepos(ctx context.Context, se Session, search string) ([]string, bool, error) {
	p, token, err := d.Auth.userToken(ctx, se)
	if err != nil {
		return nil, false, err
	}
	return d.withinBudget(ctx, func(ctx context.Context) ([]string, bool, error) {
		return p.memberRepos(ctx, token, search, d.listLimit)
	})
}

// catalogCache keeps each instance's review-side lists, by search, for
// catalogTTL, and the listings in progress, so requests share one.
type catalogCache struct {
	mu      sync.Mutex
	entries map[string]catalogEntry
	flights map[string]*catalogFlight
}

type catalogEntry struct {
	repos     []string
	truncated bool
	at        time.Time
}

type catalogFlight struct {
	done      chan struct{}
	repos     []string
	truncated bool
	err       error
}

// catalogRepos returns the repositories holding search that the review bot
// or App of instance reaches: a list younger than 5 minutes, the one being
// made, or a new one, made within the listing budget and not cut short by
// the request that started it going away (others may wait for it).
// Failures aren't kept.
func (d *Dashboard) catalogRepos(ctx context.Context, instance, search string) ([]string, bool, error) {
	cat, ok := d.catalogs[instance]
	if !ok || cat == nil {
		return nil, false, errors.New("no repository list for the instance")
	}
	c, key := d.catalog, instance+"\x00"+strings.ToLower(search)
	c.mu.Lock()
	now := d.now()
	if e, ok := c.entries[key]; ok && now.Sub(e.at) < catalogTTL && !now.Before(e.at) {
		c.mu.Unlock()
		return e.repos, e.truncated, nil
	}
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.repos, f.truncated, f.err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	f := &catalogFlight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()

	f.repos, f.truncated, f.err = d.withinBudget(context.WithoutCancel(ctx), func(ctx context.Context) ([]string, bool, error) {
		return cat.ReachableRepos(ctx, search, d.listLimit)
	})

	c.mu.Lock()
	delete(c.flights, key)
	if f.err == nil {
		now := d.now()
		for k, e := range c.entries {
			if now.Sub(e.at) >= catalogTTL {
				delete(c.entries, k)
			}
		}
		c.entries[key] = catalogEntry{repos: f.repos, truncated: f.truncated, at: now}
	}
	c.mu.Unlock()
	close(f.done)
	return f.repos, f.truncated, f.err
}

// projectAvailable checks project on its own, whatever the lists hold: the
// session's user must be a member with read access (asked with their
// token) and the review bot or App must reach it. It returns the path as
// the platform spells it.
func (d *Dashboard) projectAvailable(ctx context.Context, se Session, project string) (string, bool, error) {
	cat, ok := d.catalogs[se.Instance]
	if !ok || cat == nil {
		return "", false, errors.New("no repository list for the instance")
	}
	p, token, err := d.Auth.userToken(ctx, se)
	if err != nil {
		return "", false, err
	}
	name, ok, err := p.memberRepo(ctx, token, project)
	if err != nil || !ok {
		return "", false, err
	}
	reached, err := cat.CanReach(ctx, name)
	if err != nil || !reached {
		return "", false, err
	}
	return name, true, nil
}

// --- POST /api/repositories ---

func (d *Dashboard) switchOn(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var body struct {
		Project string `json:"project"`
	}
	if err := decodeStrict(r, &body); err != nil || strings.TrimSpace(body.Project) == "" || len(body.Project) > 1024 {
		writeError(w, http.StatusBadRequest, msgBadProjectBody)
		return
	}

	// The client's word isn't taken: the admin and the review side must
	// reach the repository now. This comes before anything about whether
	// it is on, so a repository the admin can't reach is 404 the same way,
	// after the same calls, on or off.
	name, ok, err := d.projectAvailable(ctx, se, body.Project)
	if err != nil {
		d.Log.Warn("dashboard: checking a repository to switch on failed",
			"instance", se.Instance, "user", se.Username, "error", err.Error())
		writeError(w, http.StatusBadGateway, msgListFailed)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	if d.Repos.InConfig(se.Instance, name) {
		writeError(w, http.StatusConflict, msgConfigManaged)
		return
	}
	created, err := d.Store.InsertDashboardRepo(ctx, store.DashboardRepo{
		Instance: se.Instance, Project: name, CreatedBy: se.Username})
	if err != nil {
		d.Log.Error("dashboard: switching a repository on failed", "instance", se.Instance, "project", name,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !created {
		writeError(w, http.StatusConflict, msgAlreadyOn)
		return
	}
	d.Log.Info("dashboard: repository switched on", "instance", se.Instance, "project", name, "by", se.Username)
	d.reconcileSoon()
	// The admin checked they can see it just now: they see it at once.
	if err := d.Store.AddSessionProject(ctx, se.idHash, name); err != nil {
		d.Log.Warn("dashboard: adding a switched-on repository to the session failed", "error", err.Error())
	}
	repo, ok, err := d.Repos.Get(ctx, se.Instance, name)
	if err == nil && !ok {
		err = errors.New("the repository switched on isn't there")
	}
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	view, err := d.detailView(ctx, se, repo)
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// decodeStrict reads one JSON value of at most maxWriteBody bytes into v;
// unknown fields and anything after the value are errors.
func decodeStrict(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBody+1))
	if err != nil {
		return err
	}
	if len(data) > maxWriteBody {
		return errors.New("body too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("more than one JSON value")
	}
	return nil
}

// --- the writes to one repository ---

// target returns the repository the request's {instance}/{rest...} names
// once suffix is cut from rest: switched on, seen by the session (404
// otherwise, as for one that doesn't exist) and not managed by config (409).
func (d *Dashboard) target(w http.ResponseWriter, r *http.Request, suffix string) (settings.Effective, bool) {
	ctx, se := r.Context(), session(r)
	project, ok := strings.CutSuffix(r.PathValue("rest"), suffix)
	if r.PathValue("instance") != se.Instance || !ok || project == "" {
		notFound(w)
		return settings.Effective{}, false
	}
	repos, err := d.activeRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return settings.Effective{}, false
	}
	for _, repo := range repos {
		if !strings.EqualFold(repo.Project, project) {
			continue
		}
		if repo.Source == settings.SourceConfig {
			writeError(w, http.StatusConflict, msgConfigManaged)
			return settings.Effective{}, false
		}
		return repo, true
	}
	notFound(w)
	return settings.Effective{}, false
}

// --- DELETE /api/repositories/{instance}/{project...} ---

func (d *Dashboard) switchOff(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	repo, ok := d.target(w, r, "")
	if !ok {
		return
	}
	if err := d.Store.DeleteDashboardRepo(ctx, repo.Instance, repo.Project); err != nil {
		d.Log.Error("dashboard: switching a repository off failed", "instance", repo.Instance, "project", repo.Project,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: repository switched off", "instance", repo.Instance, "project", repo.Project, "by", se.Username)
	d.reconcileSoon()
	view, err := d.detailView(ctx, se, repo)
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	view.Repo.On = false
	view.Settings = settingViews(d.Repos.Defaults(repo.Platform, repo.Instance))
	view.Attention = []AttentionView{}
	writeJSON(w, http.StatusOK, view)
}

// --- PUT .../settings and POST .../settings/reset ---

func (d *Dashboard) putSettings(w http.ResponseWriter, r *http.Request) {
	repo, ok := d.target(w, r, "/settings")
	if !ok {
		return
	}
	edit, msg := d.parseSettings(r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	d.updateSettings(w, r, repo, edit, "settings changed")
}

func (d *Dashboard) resetSettings(w http.ResponseWriter, r *http.Request) {
	repo, ok := d.target(w, r, "/settings/reset")
	if !ok {
		return
	}
	d.updateSettings(w, r, repo, func(row *store.DashboardRepo) {
		row.Approve, row.ReviewDrafts, row.FailureStatus = nil, nil, nil
		row.Skills, row.JobTimeoutSeconds, row.JobMemory = nil, nil, nil
	}, "settings reset")
}

// updateSettings applies edit to repo's stored settings and answers with
// its detail. A repository switched off meanwhile is 404 and stays off.
func (d *Dashboard) updateSettings(w http.ResponseWriter, r *http.Request, repo settings.Effective, edit func(*store.DashboardRepo), what string) {
	ctx, se := r.Context(), session(r)
	found, err := d.Store.UpdateDashboardRepo(ctx, repo.Instance, repo.Project, edit)
	if err != nil {
		d.Log.Error("dashboard: saving repository settings failed", "instance", repo.Instance, "project", repo.Project,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !found {
		notFound(w)
		return
	}
	d.Log.Info("dashboard: repository "+what, "instance", repo.Instance, "project", repo.Project, "by", se.Username)
	now, ok, err := d.Repos.Get(ctx, repo.Instance, repo.Project)
	if err == nil && !ok {
		err = errors.New("the repository isn't switched on any more")
	}
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	view, err := d.detailView(ctx, se, now)
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// parseSettings reads a settings change: a JSON object of some of the
// settings, each a valid value or null (back to the default). It returns
// the edit, or the message of a 400.
func (d *Dashboard) parseSettings(r *http.Request) (func(*store.DashboardRepo), string) {
	var fields map[string]json.RawMessage
	if err := decodeStrict(r, &fields); err != nil || fields == nil {
		return nil, msgBadSettings
	}
	var edits []func(*store.DashboardRepo)
	for key, raw := range fields {
		null := string(bytes.TrimSpace(raw)) == "null"
		var edit func(*store.DashboardRepo)
		var msg string
		switch key {
		case "approve", "reviewDrafts":
			edit, msg = boolSetting(key, raw, null)
		case "failureStatus":
			edit, msg = failureStatusSetting(raw, null)
		case "skills":
			edit, msg = d.skillsSetting(raw, null)
		case "jobTimeoutSeconds":
			edit, msg = timeoutSetting(raw, null)
		case "jobMemory":
			edit, msg = d.memorySetting(raw, null)
		default:
			msg = msgBadSettings
		}
		if msg != "" {
			return nil, msg
		}
		edits = append(edits, edit)
	}
	return func(row *store.DashboardRepo) {
		for _, edit := range edits {
			edit(row)
		}
	}, ""
}

func boolSetting(key string, raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v *bool
	if !null {
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return nil, key + " must be true, false or null"
		}
		v = &b
	}
	if key == "approve" {
		return func(row *store.DashboardRepo) { row.Approve = v }, ""
	}
	return func(row *store.DashboardRepo) { row.ReviewDrafts = v }, ""
}

func failureStatusSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v *string
	if !null {
		var s string
		if json.Unmarshal(raw, &s) != nil ||
			(s != string(config.FailureStatusFailed) && s != string(config.FailureStatusNeutral)) {
			return nil, `failureStatus must be "failed", "neutral" or null`
		}
		v = &s
	}
	return func(row *store.DashboardRepo) { row.FailureStatus = v }, ""
}

func (d *Dashboard) skillsSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	const msg = "skills must be a non-empty list of skills from the library, or null for all of them"
	var v []string
	if !null {
		var names []string
		if json.Unmarshal(raw, &names) != nil || len(names) == 0 {
			return nil, msg
		}
		seen := map[string]bool{}
		for _, n := range names {
			if _, ok := d.library[n]; !ok {
				return nil, msg
			}
			if !seen[n] {
				seen[n] = true
				v = append(v, n)
			}
		}
	}
	return func(row *store.DashboardRepo) { row.Skills = v }, ""
}

func timeoutSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v *int
	if !null {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < int64(minJobTimeout/time.Second) || n > int64(maxJobTimeout/time.Second) {
			return nil, "jobTimeoutSeconds must be a whole number of seconds from 60 to 7200, or null"
		}
		secs := int(n)
		v = &secs
	}
	return func(row *store.DashboardRepo) { row.JobTimeoutSeconds = v }, ""
}

// memorySetting takes a Kubernetes quantity from 256Mi (or the Job's memory
// request, when higher: a limit below it makes a Job Kubernetes refuses) to
// 32Gi, stored in its canonical form.
func (d *Dashboard) memorySetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v *string
	if !null {
		floor := minJobMemory.DeepCopy()
		if req, ok := d.cfg.Jobs.Resources.Requests[corev1.ResourceMemory]; ok && req.Cmp(floor) > 0 {
			floor = req.DeepCopy()
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, memoryMessage(floor)
		}
		q, err := resource.ParseQuantity(s)
		if err != nil || q.Cmp(floor) < 0 || q.Cmp(maxJobMemory) > 0 {
			return nil, memoryMessage(floor)
		}
		text := q.String()
		v = &text
	}
	return func(row *store.DashboardRepo) { row.JobMemory = v }, ""
}

func memoryMessage(floor resource.Quantity) string {
	return fmt.Sprintf(`jobMemory must be a size from %s to %s such as "6Gi", or null`, floor.String(), maxJobMemory.String())
}
