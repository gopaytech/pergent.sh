package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// availablePerPage is the most repositories one page of the picker
	// holds; maxAvailablePage the highest page asked for.
	availablePerPage = 50
	maxAvailablePage = 10000
	// listLimit is the most repositories read from a review bot or App that
	// can't search and page on the platform (GitHub), for the picker.
	listLimit = 5000
	// AvailableBudget bounds the whole GET /api/repositories/available, so
	// that even a truncated answer is written well before the server's 60 s
	// WriteTimeout and the 60 s proxy timeout in front of it.
	AvailableBudget = 45 * time.Second
	// listBudget bounds the bot's listing for the picker, within
	// AvailableBudget; each request of it has platformTimeout. What was read
	// when it runs out is used.
	listBudget = 20 * time.Second
	// maxSearch is the longest picker search.
	maxSearch = 100
	// catalogTTL is how long the review side's list is kept.
	catalogTTL = 5 * time.Minute
	// maxWriteBody bounds a write's body.
	maxWriteBody = 64 << 10

	minJobTimeout = time.Minute
	maxJobTimeout = 2 * time.Hour

	msgBadPage        = "page must be a whole number from 1 to 10000"
	msgListFailed     = "could not list the repositories on the platform, please try again"
	msgConfigManaged  = "this repository is managed by the config file; change it there"
	msgAlreadyOn      = "this repository is switched on already"
	msgWriteFailed    = "could not save the change, please try again"
	msgUserLookup     = "could not look the user up on the platform, please try again"
	msgBadProjectBody = `the body must be {"project": "<path>"}`
	msgBadSettings    = "the body must be a JSON object of settings: approve, reviewDrafts, inlineFindings, commandsAuthorAndMaintainersOnly, incrementalReviews, learnFromDismissals, quietReviews, commentLayout, linkedReposEnabled, linkedRepos, failureStatus, skills, jobTimeoutSeconds, maxTurns, jobMemory, rerunLimitPerMR, rerunLimitPerRepo, trustedAuthorsOnly, trustedAuthors, model, monthlyTokenQuota"
	msgNameInstance   = `name the instance: {"instance": "<name>", "project": "<path>"}`
	msgNeedsAdminCaps = "only admins can change its re-run caps"
	msgNeedsRole      = "only maintainers of this repository and admins can change its settings"
	msgNeedsRoleScan  = "only maintainers of this repository and admins can scan it"
)

var (
	minJobMemory = resource.MustParse("256Mi")
	maxJobMemory = resource.MustParse("32Gi")
)

// writeRoutes adds the write API to mux: every route behind a session
// (Auth.Require checks X-CSRF-Token); switching on and off and the picker
// are admin routes (adminRoute: 404 for anyone else), a repository's settings
// are for its maintainers and admins (403 otherwise).
func (d *Dashboard) writeRoutes(mux *http.ServeMux) {
	d.adminRoute(mux, "GET /api/repositories/available", d.available)
	d.adminRoute(mux, "GET /api/admin/linkable", d.linkable)
	d.adminRoute(mux, "POST /api/repositories", d.switchOn)
	d.adminRoute(mux, "DELETE /api/repositories/{instance}/{rest...}", d.switchOff)
	mux.Handle("PUT /api/repositories/{instance}/{rest...}", d.Auth.Require(http.HandlerFunc(d.repositoryPut)))
	mux.Handle("POST /api/repositories/{instance}/{rest...}", d.Auth.Require(http.HandlerFunc(d.repositoryPost)))
	// The instance alone is 404, not a redirect to the {rest...} routes.
	none := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { notFound(w) })
	for _, method := range []string{"DELETE", "PUT", "POST"} {
		mux.Handle(method+" /api/repositories/{instance}", d.Auth.Require(none))
	}
}

// --- GET /api/repositories/available ---

func (d *Dashboard) available(w http.ResponseWriter, r *http.Request) {
	instance := r.URL.Query().Get("instance")
	if instance == "" {
		writeError(w, http.StatusBadRequest, msgNameInstance)
		return
	}
	d.listAvailable(w, r, instance)
}

// errOutOfBudget is the cause of a picker listing cut short by its budget
// or the request's (AvailableBudget), not by the request going away.
var errOutOfBudget = errors.New("the listing ran out of time")

// withinBudget runs a listing under d.listBudget and whatever is left of
// ctx's budget. When it stops early after something was read (a budget ran
// out, or a page failed) while the request is still there, what was read is
// the answer, truncated, and the failure is logged; an error with nothing
// read is an error.
func (d *Dashboard) withinBudget(ctx context.Context, side string, list func(ctx context.Context) ([]string, bool, error)) ([]string, bool, error) {
	bctx, cancel := context.WithTimeoutCause(ctx, d.listBudget, errOutOfBudget)
	defer cancel()
	repos, truncated, err := list(bctx)
	if err != nil {
		if len(repos) > 0 && (bctx.Err() == nil || errors.Is(context.Cause(bctx), errOutOfBudget)) {
			d.Log.Warn("dashboard: a picker listing stopped early; showing what was read",
				"side", side, "read", len(repos), "error", err.Error())
			return repos, true, nil
		}
		return nil, false, err
	}
	return repos, truncated, nil
}

// catalogCache keeps each instance's review-side lists, by search (and by
// page, on a platform that pages), for catalogTTL, and the listings in
// progress, so requests share one.
type catalogCache struct {
	mu      sync.Mutex
	entries map[string]catalogEntry
	flights map[string]*catalogFlight
}

// catalogEntry is what one listing read. hasMore is a page's: the platform
// has a later one. truncated is a whole list's: it was cut short.
type catalogEntry struct {
	repos     []string
	hasMore   bool
	truncated bool
	at        time.Time
}

type catalogFlight struct {
	done chan struct{}
	res  catalogEntry
	err  error
}

// cached returns the listing kept under key if it is younger than 5 minutes,
// the one being made, or a new one from fetch, made within the request's
// budget but not cut short by the request that started it going away
// (others may wait for it). Failures aren't kept.
func (d *Dashboard) cached(ctx context.Context, key string, fetch func(ctx context.Context) (catalogEntry, error)) (catalogEntry, error) {
	c := d.catalog
	c.mu.Lock()
	now := d.now()
	if e, ok := c.entries[key]; ok && now.Sub(e.at) < catalogTTL && !now.Before(e.at) {
		c.mu.Unlock()
		return e, nil
	}
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.res, f.err
		case <-ctx.Done():
			return catalogEntry{}, ctx.Err()
		}
	}
	f := &catalogFlight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()

	f.res, f.err = fetch(context.WithoutCancel(ctx))

	c.mu.Lock()
	delete(c.flights, key)
	if f.err == nil {
		now := d.now()
		for k, e := range c.entries {
			if now.Sub(e.at) >= catalogTTL {
				delete(c.entries, k)
			}
		}
		f.res.at = now
		c.entries[key] = f.res
	}
	c.mu.Unlock()
	close(f.done)
	return f.res, f.err
}

// catalogRepos returns the repositories holding search that the review bot
// or App of instance reaches, read whole (up to d.listLimit) within the
// listing budget; see cached.
func (d *Dashboard) catalogRepos(ctx context.Context, instance, search string) ([]string, bool, error) {
	cat, ok := d.catalogs.Get(instance)
	if !ok || cat == nil {
		return nil, false, errors.New("no repository list for the instance")
	}
	res, err := d.cached(ctx, "list\x00"+instance+"\x00"+strings.ToLower(search), func(ctx context.Context) (catalogEntry, error) {
		repos, truncated, err := d.withinBudget(ctx, "review", func(ctx context.Context) ([]string, bool, error) {
			return cat.ReachableRepos(ctx, search, d.listLimit)
		})
		return catalogEntry{repos: repos, truncated: truncated}, err
	})
	return res.repos, res.truncated, err
}

// catalogPage returns the page (1-based) of availablePerPage repositories
// holding search that the review bot of instance reaches, asked of the
// platform alone, and whether it has a later page; see cached. An error
// leaves nothing read to show: a page is all or nothing.
func (d *Dashboard) catalogPage(ctx context.Context, cat PagedRepoCatalog, instance, search string, page int) ([]string, bool, error) {
	key := "page\x00" + instance + "\x00" + strings.ToLower(search) + "\x00" + strconv.Itoa(page)
	res, err := d.cached(ctx, key, func(ctx context.Context) (catalogEntry, error) {
		ctx, cancel := context.WithTimeoutCause(ctx, d.listBudget, errOutOfBudget)
		defer cancel()
		repos, hasMore, err := cat.ReachablePage(ctx, search, page, availablePerPage)
		return catalogEntry{repos: repos, hasMore: hasMore}, err
	})
	return res.repos, res.hasMore, err
}

// --- POST /api/repositories ---

func (d *Dashboard) switchOn(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var body struct {
		Project  string `json:"project"`
		Instance string `json:"instance"`
	}
	if err := decodeStrict(r, &body); err != nil || strings.TrimSpace(body.Project) == "" || len(body.Project) > 1024 {
		writeError(w, http.StatusBadRequest, msgBadProjectBody)
		return
	}
	instance := body.Instance
	if instance == "" {
		writeError(w, http.StatusBadRequest, msgNameInstance)
		return
	}
	if !d.knownInstance(instance) {
		notFound(w)
		return
	}

	// The client's word isn't taken: the review bot or App must reach the
	// repository now. This comes before anything about whether it is on, so a
	// repository it can't reach is 404 the same way, after the same calls,
	// on or off.
	name, ok, err := d.switchOnAvailable(ctx, instance, body.Project)
	if err != nil {
		d.Log.Warn("dashboard: checking a repository to switch on failed",
			"instance", instance, "user", se.Username, "error", err.Error())
		writeError(w, http.StatusBadGateway, msgListFailed)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	if d.Repos.InConfig(instance, name) {
		writeError(w, http.StatusConflict, msgConfigManaged)
		return
	}
	created, err := d.Store.InsertDashboardRepo(ctx, store.DashboardRepo{
		Instance: instance, Project: name, CreatedBy: se.Username})
	if err != nil {
		d.Log.Error("dashboard: switching a repository on failed", "instance", instance, "project", name,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !created {
		writeError(w, http.StatusConflict, msgAlreadyOn)
		return
	}
	d.Log.Info("dashboard: repository switched on", "instance", instance, "project", name, "by", se.Username)
	d.audit(ctx, se, "repository_on", instance, name, "")
	d.reconcileSoon(instance, name)
	repo, ok, err := d.Repos.Get(ctx, instance, name)
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

// switchOnAvailable checks that the review bot or App of instance reaches
// project, and returns the name to switch it on under: as the platform
// spells it when it says (GitLab, whatever case the client sent), otherwise as
// given (trimmed). An admin has no platform account to ask, so the bot's access
// is the only check.
func (d *Dashboard) switchOnAvailable(ctx context.Context, instance, project string) (string, bool, error) {
	project = strings.TrimSpace(project)
	cat, ok := d.catalogs.Get(instance)
	if !ok || cat == nil {
		return "", false, errors.New("no repository list for the instance")
	}
	if sp, ok := cat.(SpellingRepoCatalog); ok {
		name, reached, err := sp.ReachableName(ctx, project)
		if err != nil || !reached {
			return "", false, err
		}
		return name, true, nil
	}
	reached, err := cat.CanReach(ctx, project)
	if err != nil || !reached {
		return "", false, err
	}
	return project, true, nil
}

// decodeStrict reads one JSON value of at most maxWriteBody bytes into v;
// unknown fields and anything after the value are errors.
func decodeStrict(r *http.Request, v any) error { return decodeStrictLimit(r, v, maxWriteBody) }

// decodeStrictLimit is decodeStrict for a body of at most limit bytes.
func decodeStrictLimit(r *http.Request, v any, limit int64) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
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
// otherwise, as for one that doesn't exist), on which the session's role is
// need or above (403) and not managed by config (409).
func (d *Dashboard) target(w http.ResponseWriter, r *http.Request, suffix string, need Role) (settings.Effective, bool) {
	return d.targetRepo(w, r, suffix, need, false, msgNeedsRole)
}

// targetAny is target without the 409 for a repository managed by config: a
// review's buttons work on every repository switched on, wherever it is set
// up.
func (d *Dashboard) targetAny(w http.ResponseWriter, r *http.Request, suffix string, need Role) (settings.Effective, bool) {
	return d.targetRepo(w, r, suffix, need, true, msgNeedsRoleReview)
}

// targetRepo is target and targetAny: denied is the 403's message.
func (d *Dashboard) targetRepo(w http.ResponseWriter, r *http.Request, suffix string, need Role, allowConfig bool, denied string) (settings.Effective, bool) {
	ctx, se := r.Context(), session(r)
	instance := r.PathValue("instance")
	project, ok := strings.CutSuffix(r.PathValue("rest"), suffix)
	if !ok || project == "" {
		notFound(w)
		return settings.Effective{}, false
	}
	repos, err := d.activeRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return settings.Effective{}, false
	}
	for _, repo := range repos {
		if repo.Instance != instance || !strings.EqualFold(repo.Project, project) {
			continue
		}
		if se.Role(repo.Instance, repo.Project) < need {
			writeError(w, http.StatusForbidden, denied)
			return settings.Effective{}, false
		}
		if repo.Source == settings.SourceConfig && !allowConfig {
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
	repo, ok := d.target(w, r, "", RoleAdmin)
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
	d.audit(ctx, se, "repository_off", repo.Instance, repo.Project, "")
	d.reconcileSoon(repo.Instance, repo.Project)
	view, err := d.detailView(ctx, se, repo)
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	view.Repo.On = false
	def, err := d.Repos.BrowserDefaults(ctx, repo.Platform, repo.Instance)
	if err != nil {
		d.failed(w, r, "reading the defaults", err)
		return
	}
	view.Settings = settingViews(def)
	view.Model = d.effectiveModel("")
	view.Attention = []AttentionView{}
	writeJSON(w, http.StatusOK, view)
}

// --- POST /api/repositories/{instance}/{project...}/... ---

// repositoryPost serves the POSTs to one repository: .../readiness/scan
// (scanNow), .../past-dismissals/stop (stopUsing) and .../settings/reset; any
// other is 404.
func (d *Dashboard) repositoryPost(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.PathValue("rest"), "/readiness/scan") {
		d.scanNow(w, r)
		return
	}
	if strings.HasSuffix(r.PathValue("rest"), "/past-dismissals/stop") {
		d.stopUsing(w, r)
		return
	}
	d.resetSettings(w, r)
}

// --- PUT .../settings, PUT .../teams and POST .../settings/reset ---

// repositoryPut serves the PUTs to one repository: .../teams (putRepositoryTeams,
// admins only) and .../settings (putSettings, which answers any other path 404).
func (d *Dashboard) repositoryPut(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.PathValue("rest"), "/teams") {
		d.putRepositoryTeams(w, r)
		return
	}
	d.putSettings(w, r)
}

func (d *Dashboard) putSettings(w http.ResponseWriter, r *http.Request) {
	repo, ok := d.target(w, r, "/settings", RoleMaintainer)
	if !ok {
		return
	}
	edit, keys, msg := d.parseSettings(r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// The caps are the admins' to change, even for a repository whose
	// maintainer may change the rest.
	if slices.ContainsFunc(keys, isRerunLimitKey) && session(r).Role(repo.Instance, repo.Project) < RoleAdmin {
		writeError(w, http.StatusForbidden, msgNeedsAdminCaps)
		return
	}
	// So are the linked repositories: a link lets a review read another
	// repository and quote it here.
	if slices.ContainsFunc(keys, isLinkedKey) && session(r).Role(repo.Instance, repo.Project) < RoleAdmin {
		writeError(w, http.StatusForbidden, msgNeedsAdminLinks)
		return
	}
	// So is who is trusted: turning it off, or a name on the list, lets a
	// stranger's change be reviewed, and approved, without anyone asking.
	if slices.ContainsFunc(keys, isTrustKey) && session(r).Role(repo.Instance, repo.Project) < RoleAdmin {
		writeError(w, http.StatusForbidden, msgNeedsAdminTrust)
		return
	}
	// So is the token quota: it stops the repository's reviews.
	if slices.ContainsFunc(keys, isQuotaKey) && session(r).Role(repo.Instance, repo.Project) < RoleAdmin {
		writeError(w, http.StatusForbidden, msgNeedsAdminQuota)
		return
	}
	// So is the model: it decides what the repository's reviews cost.
	if slices.ContainsFunc(keys, isModelKey) && session(r).Role(repo.Instance, repo.Project) < RoleAdmin {
		writeError(w, http.StatusForbidden, msgNeedsAdminModel)
		return
	}
	// The entry the config's llm: block supplies is not stored, so only the
	// registry knows whether it is there.
	if slices.Contains(keys, "model") {
		var probe store.DashboardRepo
		edit(&probe)
		if probe.ModelID != nil && *probe.ModelID == store.ConfigModelID && !d.hasConfigModel() {
			writeError(w, http.StatusBadRequest, msgUnknownModel)
			return
		}
	}
	// The rules for links that need the platform run before the save; a link
	// that fails them is never stored.
	if slices.Contains(keys, "linkedRepos") {
		var probe store.DashboardRepo
		edit(&probe)
		if p := d.checkLinks(r.Context(), repo, probe.LinkedRepos); p != nil {
			writeError(w, p.status, p.msg)
			return
		}
	}
	// The library skills the list names must fit a review (D6). The check
	// reads the library now; a skill that goes before the save is named by
	// the save.
	if slices.Contains(keys, "skills") {
		var probe store.DashboardRepo
		edit(&probe)
		msg, err := d.checkSkillBudget(r, probe.Skills)
		if err != nil {
			d.failed(w, r, "checking the skill budget", err)
			return
		}
		if msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	// The allowlist's new names are looked up on the platform before the
	// save, and stored by user ID; a name it doesn't know saves nothing.
	if slices.Contains(keys, "trustedAuthors") {
		var probe store.DashboardRepo
		edit(&probe)
		resolved, p := d.resolveTrusted(r.Context(), repo, probe.TrustedAuthors)
		if p != nil {
			writeError(w, p.status, p.msg)
			return
		}
		inner := edit
		edit = func(row *store.DashboardRepo) {
			inner(row)
			row.TrustedAuthors = resolved
		}
	}
	// The audit detail names the model and the quota a change gives, not only
	// the key.
	details := slices.Clone(keys)
	if i := slices.Index(details, "model"); i >= 0 {
		var probe store.DashboardRepo
		edit(&probe)
		details[i] = d.modelSettingDetail(probe.ModelID)
	}
	if i := slices.Index(details, "monthlyTokenQuota"); i >= 0 {
		var probe store.DashboardRepo
		edit(&probe)
		details[i] = "monthlyTokenQuota=" + quotaText(probe.MonthlyTokenQuota)
	}
	d.updateSettings(w, r, repo, edit, "settings changed", "settings_updated", strings.Join(details, ", "))
}

// isTrustKey reports whether key is one of the trusted-authors settings,
// which only an admin of the repository changes.
func isTrustKey(key string) bool { return key == "trustedAuthorsOnly" || key == "trustedAuthors" }

// maxTrustedAuthors is the most users a repository's allowlist holds.
const maxTrustedAuthors = 100

// trustModeSetting reads trustedAuthorsOnly: "on", "off" or null (the
// instance's).
func trustModeSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v *string
	if !null {
		var m string
		if json.Unmarshal(raw, &m) != nil || (m != config.TrustOn && m != config.TrustOff) {
			return nil, `trustedAuthorsOnly must be "on", "off" or null`
		}
		v = &m
	}
	return func(row *store.DashboardRepo) { row.TrustedAuthorsOnly = v }, ""
}

// modelSetting reads model: the ID of an entry of the list of models, or null
// (follow the default). Whether the entry is there is the save's to say
// (store.ErrModelNotFound), except for the config's, which putSettings checks.
func modelSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v *string
	if !null {
		var id string
		if json.Unmarshal(raw, &id) != nil || id == "" || len(id) > maxLLMField {
			return nil, msgUnknownModel
		}
		v = &id
	}
	return func(row *store.DashboardRepo) { row.ModelID = v }, ""
}

// trustedAuthorsSetting reads trustedAuthors: a list of {"id"?, "username"}
// (an entry without an id is looked up on save) or null (nobody).
func trustedAuthorsSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	const bad = `trustedAuthors must be a list of {"id"?: number, "username": string}, at most 100, or null`
	var as []store.TrustedAuthor
	if !null {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if dec.Decode(&as) != nil || len(as) > maxTrustedAuthors {
			return nil, bad
		}
		for i := range as {
			as[i].Username = strings.TrimSpace(as[i].Username)
			if as[i].ID < 0 || as[i].Username == "" || len(as[i].Username) > 255 {
				return nil, bad
			}
		}
	}
	if len(as) == 0 {
		as = nil
	}
	return func(row *store.DashboardRepo) { row.TrustedAuthors = as }, ""
}

// resolveTrusted looks up, on repo's platform, each entry of as without an ID,
// or with one that isn't the repository's stored entry, and drops later
// entries of an ID already listed. A name the platform doesn't
// know is a 400 naming it; a platform that can't answer is a 502.
func (d *Dashboard) resolveTrusted(ctx context.Context, repo settings.Effective, as []store.TrustedAuthor) ([]store.TrustedAuthor, *linkProblem) {
	if len(as) == 0 {
		return nil, nil
	}
	src, ok := d.sources.Get(repo.Instance)
	if !ok {
		return nil, &linkProblem{http.StatusBadGateway, msgUserLookup}
	}
	ctx, cancel := context.WithTimeout(ctx, platformTimeout)
	defer cancel()
	out := make([]store.TrustedAuthor, 0, len(as))
	seen := map[int64]bool{}
	for _, a := range as {
		// An entry with an ID is kept only as it is stored (one the page got
		// back); any other is looked up by its name, so a name is never
		// stored beside someone else's ID.
		if a.ID != 0 && !slices.Contains(repo.TrustedAuthors, a) {
			a.ID = 0
		}
		if a.ID == 0 {
			u, err := src.UserByName(ctx, repo.Project, a.Username)
			if errors.Is(err, forge.ErrNotFound) {
				return nil, &linkProblem{http.StatusBadRequest, "trustedAuthors: " + strconv.Quote(a.Username) + " is not a user on " + repo.Instance}
			}
			if err != nil {
				d.Log.Warn("dashboard: looking a user up failed", "instance", repo.Instance, "project", repo.Project,
					"error", d.registry.Redact(err.Error()))
				return nil, &linkProblem{http.StatusBadGateway, msgUserLookup}
			}
			a = store.TrustedAuthor{ID: u.ID, Username: u.Username}
		}
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		out = append(out, a)
	}
	return out, nil
}

func (d *Dashboard) resetSettings(w http.ResponseWriter, r *http.Request) {
	repo, ok := d.target(w, r, "/settings/reset", RoleMaintainer)
	if !ok {
		return
	}
	// A maintainer's reset leaves the caps, the links, who is trusted, the
	// model and the token quota, which only an admin changes.
	admin := session(r).Role(repo.Instance, repo.Project) >= RoleAdmin
	detail := ""
	if admin && repo.ModelID != "" {
		// The model goes back to the default: say which one it was.
		detail = "model (default, was " + d.effectiveModelOrID(repo.ModelID) + ")"
	}
	d.updateSettings(w, r, repo, func(row *store.DashboardRepo) {
		row.Approve, row.ReviewDrafts, row.InlineFindings, row.FailureStatus = nil, nil, nil, nil
		row.CommandsAuthorAndMaintainersOnly, row.IncrementalReviews, row.LearnFromDismissals, row.QuietReviews = nil, nil, nil, nil
		row.CommentLayout = nil
		row.Skills, row.RepoSkills, row.JobTimeoutSeconds, row.MaxTurns, row.JobMemory = nil, nil, nil, nil, nil
		if admin {
			row.RerunLimitPerMR, row.RerunLimitPerRepo = nil, nil
			row.LinkedReposEnabled, row.LinkedRepos = nil, nil
			row.TrustedAuthorsOnly, row.TrustedAuthors = nil, nil
			row.ModelID = nil
			row.MonthlyTokenQuota = nil
		}
	}, "settings reset", "settings_reset", detail)
}

// updateSettings applies edit to repo's stored settings, records action
// with detail in the audit log and answers with its detail. A repository
// switched off meanwhile is 404 and stays off. A change of its skills or
// repository skills has its last readiness scan judged again.
func (d *Dashboard) updateSettings(w http.ResponseWriter, r *http.Request, repo settings.Effective, edit func(*store.DashboardRepo),
	what, action, detail string) {
	ctx, se := r.Context(), session(r)
	var skillsChanged bool
	found, err := d.Store.UpdateDashboardRepo(ctx, repo.Instance, repo.Project, func(row *store.DashboardRepo) {
		skills, repoSkills := slices.Clone(row.Skills), slices.Clone(row.RepoSkills)
		edit(row)
		skillsChanged = !slices.Equal(skills, row.Skills) || !slices.Equal(repoSkills, row.RepoSkills)
	})
	if msg, ok := skillMissing(err); ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if errors.Is(err, store.ErrModelNotFound) {
		writeError(w, http.StatusBadRequest, msgUnknownModel)
		return
	}
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
	d.audit(ctx, se, action, repo.Instance, repo.Project, detail)
	d.inputs.invalidate()
	if skillsChanged {
		d.recheck(repo.Instance, repo.Project)
	}
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
// the edit and the keys it changes, sorted, or the message of a 400.
func (d *Dashboard) parseSettings(r *http.Request) (func(*store.DashboardRepo), []string, string) {
	var fields map[string]json.RawMessage
	if err := decodeStrict(r, &fields); err != nil || fields == nil {
		return nil, nil, msgBadSettings
	}
	keys := make([]string, 0, len(fields))
	var edits []func(*store.DashboardRepo)
	for key, raw := range fields {
		null := string(bytes.TrimSpace(raw)) == "null"
		var edit func(*store.DashboardRepo)
		var msg string
		switch key {
		case "approve", "reviewDrafts", "inlineFindings", "commandsAuthorAndMaintainersOnly", "incrementalReviews", "learnFromDismissals", "quietReviews", "linkedReposEnabled":
			edit, msg = boolSetting(key, raw, null)
		case "commentLayout":
			edit, msg = commentLayoutSetting(raw, null, msgBadSettings)
		case "linkedRepos":
			edit, msg = linkedReposSetting(raw, null)
		case "failureStatus":
			edit, msg = failureStatusSetting(raw, null)
		case "skills":
			edit, msg = skillsSetting(raw, null)
		case "repoSkills":
			edit, msg = repoSkillsSetting(raw, null)
		case "jobTimeoutSeconds":
			edit, msg = timeoutSetting(raw, null)
		case "maxTurns":
			edit, msg = maxTurnsSetting(raw, null)
		case "jobMemory":
			edit, msg = d.memorySetting(raw, null)
		case "rerunLimitPerMR", "rerunLimitPerRepo":
			edit, msg = rerunLimitSetting(key, raw, null)
		case "trustedAuthorsOnly":
			edit, msg = trustModeSetting(raw, null)
		case "trustedAuthors":
			edit, msg = trustedAuthorsSetting(raw, null)
		case "model":
			edit, msg = modelSetting(raw, null)
		case "monthlyTokenQuota":
			edit, msg = quotaSetting(raw)
		default:
			msg = msgBadSettings
		}
		if msg != "" {
			return nil, nil, msg
		}
		edits = append(edits, edit)
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return func(row *store.DashboardRepo) {
		for _, edit := range edits {
			edit(row)
		}
	}, keys, ""
}

// boolSettings are the settings that are true, false or null (the default),
// each by its key in the API, with the way it sets the repository field.
var boolSettings = map[string]func(row *store.DashboardRepo, v *bool){
	"approve":                          func(row *store.DashboardRepo, v *bool) { row.Approve = v },
	"reviewDrafts":                     func(row *store.DashboardRepo, v *bool) { row.ReviewDrafts = v },
	"inlineFindings":                   func(row *store.DashboardRepo, v *bool) { row.InlineFindings = v },
	"commandsAuthorAndMaintainersOnly": func(row *store.DashboardRepo, v *bool) { row.CommandsAuthorAndMaintainersOnly = v },
	"incrementalReviews":               func(row *store.DashboardRepo, v *bool) { row.IncrementalReviews = v },
	"learnFromDismissals":              func(row *store.DashboardRepo, v *bool) { row.LearnFromDismissals = v },
	"quietReviews":                     func(row *store.DashboardRepo, v *bool) { row.QuietReviews = v },
	"linkedReposEnabled":               func(row *store.DashboardRepo, v *bool) { row.LinkedReposEnabled = v },
}

// boolSetting reads the setting key, one of boolSettings, as true, false or
// null. A key that is not one of them is refused, not applied.
func boolSetting(key string, raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v *bool
	if !null {
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return nil, key + " must be true, false or null"
		}
		v = &b
	}
	set, ok := boolSettings[key]
	if !ok {
		return nil, key + " is not a setting"
	}
	return func(row *store.DashboardRepo) { set(row, v) }, ""
}

// commentLayoutSetting takes null (the default: for a repository the admin
// default, else the built-in layout; for the defaults, the built-in layout)
// or a list that render.ParseLayout accepts: block:state texts, each block at most once,
// findings among them. Anything else, an empty list included, is refused with
// bad, the message of the whole body, which names the key. The list is kept
// in the order given.
func commentLayoutSetting(raw json.RawMessage, null bool, bad string) (func(*store.DashboardRepo), string) {
	var v []string
	if !null {
		var entries []string
		if json.Unmarshal(raw, &entries) != nil {
			return nil, bad
		}
		blocks, err := render.ParseLayout(entries)
		if err != nil {
			return nil, bad
		}
		v = make([]string, len(blocks))
		for i, b := range blocks {
			v[i] = b.String()
		}
	}
	return func(row *store.DashboardRepo) { row.CommentLayout = v }, ""
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

// skillsSetting takes null (the default list) or a non-empty list of
// library skill names. A name the library lacks is refused by the save,
// which locks the skills it names (see updateSettings).
func skillsSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v []string
	if !null {
		var names []string
		if json.Unmarshal(raw, &names) != nil || len(names) == 0 {
			return nil, "skills must be a non-empty list of skills from the library, or null for the default list"
		}
		var msg string
		if v, msg = skillNames(names); msg != "" {
			return nil, msg
		}
	}
	return func(row *store.DashboardRepo) { row.Skills = v }, ""
}

// skillNames drops repeats from names, keeping the order, and refuses a name
// that can't be a skill's with the message for a skill the library lacks.
func skillNames(names []string) ([]string, string) {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		if !inputs.ValidName(n) {
			return nil, msgSkillMissing(n)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, ""
}

// repoSkillsSetting takes the names of the detected repository skills to
// turn on: a list, which may be empty, or null. Either of those is none.
func repoSkillsSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	const msg = "repoSkills must be a list of skill names, or null"
	var v []string
	if !null {
		var names []string
		if json.Unmarshal(raw, &names) != nil {
			return nil, msg
		}
		for _, n := range names {
			if !inputs.ValidName(n) {
				return nil, msg
			}
		}
		v, _ = skillNames(names)
	}
	return func(row *store.DashboardRepo) { row.RepoSkills = v }, ""
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

// maxTurnsSetting takes a whole number from config.MinMaxTurns to
// config.MaxMaxTurns for the step limit of each opencode run.
func maxTurnsSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v *int
	if !null {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < config.MinMaxTurns || n > config.MaxMaxTurns {
			return nil, "maxTurns must be a whole number from 5 to 200, or null"
		}
		steps := int(n)
		v = &steps
	}
	return func(row *store.DashboardRepo) { row.MaxTurns = v }, ""
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

// rerunLimitSetting takes a whole number from config.MinRerunLimit to
// config.MaxRerunLimit for one of the re-run caps.
func rerunLimitSetting(key string, raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v *int
	if !null {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < config.MinRerunLimit || n > config.MaxRerunLimit {
			return nil, fmt.Sprintf("%s must be a whole number from %d to %d, or null", key, config.MinRerunLimit, config.MaxRerunLimit)
		}
		limit := int(n)
		v = &limit
	}
	switch key {
	case "rerunLimitPerMR":
		return func(row *store.DashboardRepo) { row.RerunLimitPerMR = v }, ""
	case "rerunLimitPerRepo":
		return func(row *store.DashboardRepo) { row.RerunLimitPerRepo = v }, ""
	}
	return nil, key + " is not a re-run cap" // unreachable: the callers switch on the two keys
}

// isRerunLimitKey is whether key is one of the re-run caps.
func isRerunLimitKey(key string) bool { return key == "rerunLimitPerMR" || key == "rerunLimitPerRepo" }

func memoryMessage(floor resource.Quantity) string {
	return fmt.Sprintf(`jobMemory must be a size from %s to %s such as "6Gi", or null`, floor.String(), maxJobMemory.String())
}
