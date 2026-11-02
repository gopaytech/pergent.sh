package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/skillfetch"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// --- installing and updating library skills from a git source (D13-D19) ---
//
//   - POST /api/admin/skill-fetches: {instance, repo} or {url}, with ref,
//     and path, purpose ("install" or "update") and fromRef when wanted:
//     starts a read (202 {id}). A platform admin reads repositories of its
//     own instance that its user can see there; an operator any (D19).
//   - GET /api/admin/skill-fetches/{id}: the read (SkillFetchView).
//   - POST /api/admin/skill-fetches/{id}/install: {names, addToDefaults}.
//   - POST /api/admin/skill-fetches/{id}/update: {names}, for a check for
//     updates.
//
// A read is the requester's alone: 404 for anyone else. Installs and
// updates check each skill again (the skill checks and the held secrets,
// failing closed), are audited by the skill's name, version, ref and commit
// and never its text or a private repository's path, and drop the kept
// inputs views.

const (
	msgBadFetchBody   = `the body must be {"instance": ..., "repo": ...} or {"url": ...}, with "ref", and "path", "purpose" and "fromRef" when wanted`
	msgBadNamesBody   = `the body must be {"names": [the skills to install], "addToDefaults": true or false}`
	msgBadUpdateBody  = `the body must be {"names": [the skills to update]}`
	msgCeiling        = "you can install only from repositories of your own instance that you can see"
	msgCeilingFailed  = "could not check your access to the repository on the platform, please try again"
	msgNotConnected   = "the instance is not connected to pergent.sh"
	msgNoFetcher      = "installing skills is not set up"
	msgFetchRunning   = "the source is still being read"
	msgFetchFailed    = "reading the source failed; read it again"
	msgNotUpdateCheck = "this read is not a check for updates"
	msgNotInRead      = "not found in this read"
	msgNotInstalled   = "not installed from this source and ref"
	msgRemovedAtSrc   = "removed at the source"
	msgRenamedAtSrc   = "renamed at the source"
	msgSkillGone      = "the skill is no longer in the library"
)

// The states of an installed skill in a check for updates.
const (
	updateChanged = "changed"
	updateSame    = "same"
)

func (d *Dashboard) skillFetchRoutes(mux *http.ServeMux) {
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, d.Auth.Require(requireAdmin(h))) }
	admin("POST /api/admin/skill-fetches", d.startSkillFetch)
	admin("GET /api/admin/skill-fetches/{id}", d.skillFetch)
	admin("POST /api/admin/skill-fetches/{id}/install", d.installSkills)
	admin("POST /api/admin/skill-fetches/{id}/update", d.updateSkills)
}

func (d *Dashboard) startSkillFetch(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var in struct {
		Instance string `json:"instance"`
		Repo     string `json:"repo"`
		URL      string `json:"url"`
		Ref      string `json:"ref"`
		Path     string `json:"path"`
		Purpose  string `json:"purpose"`
		FromRef  string `json:"fromRef"`
	}
	if decodeStrict(r, &in) != nil {
		writeError(w, http.StatusBadRequest, msgBadFetchBody)
		return
	}
	if d.fetcher == nil {
		writeError(w, http.StatusServiceUnavailable, msgNoFetcher)
		return
	}
	if !se.Operator && in.URL == "" && in.Instance == "" {
		in.Instance = se.Instance
	}
	req, err := skillfetch.Normalize(skillfetch.Request{Instance: in.Instance, Repo: in.Repo, URL: in.URL, Ref: in.Ref,
		FromRef: in.FromRef, Path: in.Path, Purpose: in.Purpose, Actor: skillActor(se), ActorID: se.AccountID})
	var bad skillfetch.RequestError
	if err != nil {
		msg := msgBadFetchBody
		if errors.As(err, &bad) {
			msg = bad.Error()
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if req.URL == "" {
		ok, err := d.withinCeiling(ctx, se, req.Instance, req.Repo)
		if err != nil {
			d.Log.Warn("dashboard: checking a user's access to a skill source failed", "instance", req.Instance, "error", err.Error())
			writeError(w, http.StatusBadGateway, msgCeilingFailed)
			return
		}
		if !ok {
			writeError(w, http.StatusForbidden, msgCeiling)
			return
		}
	}
	id, err := d.fetcher.Start(ctx, req)
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Error())
		return
	case errors.Is(err, skillfetch.ErrNotConnected):
		writeError(w, http.StatusBadGateway, msgNotConnected)
		return
	case err != nil:
		d.Log.Error("dashboard: starting a skill fetch failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: skill fetch started", "fetch", id, "instance", req.Instance, "ref", req.Ref,
		"purpose", req.Purpose, "by", se.Username)
	writeJSON(w, http.StatusAccepted, SkillFetchStartedView{ID: id})
}

// withinCeiling reports whether se may read repo of instance (D19): an
// operator anything the bot or App reaches, a platform admin its own
// instance's repositories that its user can see there, asked of the
// platform now with the user's token.
func (d *Dashboard) withinCeiling(ctx context.Context, se Session, instance, repo string) (bool, error) {
	if se.Operator {
		return true, nil
	}
	if se.Platform == "" || instance != se.Instance {
		return false, nil
	}
	return d.userCanSee(ctx, se, repo)
}

// userCanSee asks the platform, with the session user's token, whether the
// user can read repo on the session's instance.
func (d *Dashboard) userCanSee(ctx context.Context, se Session, repo string) (bool, error) {
	p, token, err := d.Auth.userToken(ctx, se)
	if err != nil {
		return false, err
	}
	_, ok, err := p.canSee(ctx, token, repo)
	if err != nil && token != "" {
		// The error goes to the log: it must not hold the token.
		err = errors.New(strings.ReplaceAll(err.Error(), token, "[REDACTED]"))
	}
	return ok, err
}

// ownFetch reads the fetch the path names, for its requester only, while it
// is kept; anything else is answered 404 (or 500) and ok is false.
func (d *Dashboard) ownFetch(w http.ResponseWriter, r *http.Request) (store.SkillFetch, bool) {
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		notFound(w)
		return store.SkillFetch{}, false
	}
	f, err := d.Store.SkillFetch(r.Context(), id)
	if errors.Is(err, store.ErrSkillFetchNotFound) {
		notFound(w)
		return store.SkillFetch{}, false
	}
	if err != nil {
		d.failed(w, r, "reading a skill fetch", err)
		return store.SkillFetch{}, false
	}
	if se := session(r); f.RequestedByID != se.AccountID || !f.ExpiresAt.After(d.now()) {
		notFound(w)
		return store.SkillFetch{}, false
	}
	return f, true
}

// fetchReady answers 409 for a fetch not done and reports whether it was.
func fetchReady(w http.ResponseWriter, f store.SkillFetch) bool {
	switch f.Status {
	case store.FetchDone:
		return true
	case store.FetchRunning:
		writeError(w, http.StatusConflict, msgFetchRunning)
	default:
		writeError(w, http.StatusConflict, msgFetchFailed)
	}
	return false
}

func (d *Dashboard) skillFetch(w http.ResponseWriter, r *http.Request) {
	f, ok := d.ownFetch(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	view := SkillFetchView{ID: f.ID, Status: f.Status, Commit: f.Commit, Error: f.Error, Purpose: f.Purpose,
		Instance: f.Instance, Repo: f.Repo, URL: f.URL, Ref: f.Ref, FromRef: f.FromRef, Path: f.Path,
		Skills: []FetchedSkillView{}, Updates: []SkillUpdateView{}}
	if f.Status != store.FetchDone {
		writeJSON(w, http.StatusOK, view)
		return
	}
	library, err := d.Store.LibrarySkills(ctx)
	if err != nil {
		d.failed(w, r, "listing the skill library", err)
		return
	}
	taken := map[string]bool{}
	for _, sk := range library {
		taken[sk.Name] = true
	}
	for _, file := range f.Files {
		view.Skills = append(view.Skills, FetchedSkillView{Name: file.Name, Path: file.Path, Size: len(file.Content),
			Problem: file.Problem, Taken: taken[file.Name]})
	}
	if f.Purpose == store.FetchUpdate {
		updates, err := d.Store.InstalledFrom(ctx, installedSource(f))
		if err != nil {
			d.failed(w, r, "reading the installed skills", err)
			return
		}
		for _, u := range matchUpdates(f, updates) {
			view.Updates = append(view.Updates, u.view)
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// installedSource is the source and ref the skills a check for updates
// compares with were installed from: the fetch's, at its from-ref.
func installedSource(f store.SkillFetch) store.SkillSource {
	ref := f.FromRef
	if ref == "" {
		ref = f.Ref
	}
	return store.SkillSource{Kind: f.Kind, Instance: f.Instance, Repo: f.Repo, URL: f.URL, Ref: ref}
}

// fetchedSource is the source a skill read by f from its file at path is
// recorded with.
func fetchedSource(f store.SkillFetch, path string) store.SkillSource {
	return store.SkillSource{Kind: f.Kind, Instance: f.Instance, Repo: f.Repo, URL: f.URL, Ref: f.Ref, Path: path, Commit: f.Commit}
}

// skillUpdate is one installed skill in a check for updates, with the file
// it matched (none when removed at the source).
type skillUpdate struct {
	view      SkillUpdateView
	installed store.LibrarySkill
	file      store.FetchedFile
}

// matchUpdates matches each installed skill whose file the fetch's folder
// holds with the file read at its path (D16): gone is "removed at the
// source"; a file that names another skill is "renamed at the source"; else
// "same" or "changed", with the file's problem if it has one.
func matchUpdates(f store.SkillFetch, installed []store.LibrarySkill) []skillUpdate {
	byPath := map[string]store.FetchedFile{}
	for _, file := range f.Files {
		byPath[file.Path] = file
	}
	var out []skillUpdate
	for _, sk := range installed {
		if sk.Source == nil || !inFolder(sk.Source.Path, f.Path) {
			continue
		}
		u := skillUpdate{installed: sk, view: SkillUpdateView{Name: sk.Name, Path: sk.Source.Path, Version: sk.Version,
			Current: sk.Content}}
		file, found := byPath[sk.Source.Path]
		u.file = file
		switch {
		case !found:
			u.view.State = msgRemovedAtSrc
		case file.Problem == "" && file.Name != sk.Name:
			u.view.State, u.view.Fetched = msgRenamedAtSrc, file.Content
		default:
			u.view.Fetched, u.view.Problem = file.Content, file.Problem
			u.view.State = updateChanged
			if file.Problem == "" && file.Content == sk.Content {
				u.view.State = updateSame
			}
		}
		out = append(out, u)
	}
	return out
}

// inFolder reports whether a scan of folder ("" for the top) reads a skill
// file at p: folder/<name>.md or folder/<name>/SKILL.md.
func inFolder(p, folder string) bool {
	rel := p
	if folder != "" {
		var ok bool
		if rel, ok = strings.CutPrefix(p, folder+"/"); !ok {
			return false
		}
	}
	parts := strings.Split(rel, "/")
	return len(parts) == 1 || (len(parts) == 2 && parts[1] == "SKILL.md")
}

// fetchNames reads a body's names: one at least, no more than a fetch
// holds, each once in the order given. ok is false for a bad body.
func fetchNames(names []string) ([]string, bool) {
	if len(names) == 0 || len(names) > skillfetch.MaxFetchFiles {
		return nil, false
	}
	var out []string
	for _, n := range names {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, true
}

// fileNamed is the fetch's file of the skill called name: one with no
// problem first. found is false when there is none.
func fileNamed(f store.SkillFetch, name string) (file store.FetchedFile, found bool) {
	for _, file := range f.Files {
		if file.Name == name && file.Problem == "" {
			return file, true
		}
	}
	for _, file := range f.Files {
		if file.Name == name {
			return file, true
		}
	}
	return store.FetchedFile{}, false
}

// skillRefusal says why file's text can't be saved as the skill name, ""
// when it can: its problem from the read, then the skill checks and the held
// secrets again (they may have changed since).
func skillRefusal(file store.FetchedFile, name string, held []string) string {
	content := []byte(file.Content)
	switch {
	case file.Problem != "":
		return file.Problem
	case strings.TrimSpace(file.Content) == "":
		return msgSkillNoText
	case inputs.CheckSkill(name, content) != nil:
		return inputs.CheckSkill(name, content).Error()
	case inputs.ContainsSecret(content, held):
		return msgSkillSecret
	}
	return ""
}

// readHeld reads the held secrets for a skill check; on an error it answers
// 500, naming nothing, and ok is false: nothing is saved with them unknown.
func (d *Dashboard) readHeld(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	held, err := d.heldSecrets(r.Context())
	if err != nil {
		d.Log.Error("dashboard: reading the held secrets for a skill check failed", "path", r.URL.Path, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return nil, false
	}
	return held, true
}

// fetchOrigin names where f read, for the audit log: a URL is public, a
// repository of an instance is named by its instance only (its path is for
// those who can see it, D11). Then the ref and the short commit.
func fetchOrigin(f store.SkillFetch) string {
	where := "a repository on " + f.Instance
	if f.Kind == store.SourceKindURL {
		where = f.URL
	}
	return fmt.Sprintf("%s at %s (%s)", where, f.Ref, shortCommit(f.Commit))
}

func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

func (d *Dashboard) installSkills(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	f, ok := d.ownFetch(w, r)
	if !ok {
		return
	}
	var in struct {
		Names         []string `json:"names"`
		AddToDefaults bool     `json:"addToDefaults"`
	}
	if decodeStrict(r, &in) != nil {
		writeError(w, http.StatusBadRequest, msgBadNamesBody)
		return
	}
	names, ok := fetchNames(in.Names)
	if !ok {
		writeError(w, http.StatusBadRequest, msgBadNamesBody)
		return
	}
	if !fetchReady(w, f) {
		return
	}
	held, ok := d.readHeld(w, r)
	if !ok {
		return
	}
	var defaults store.RepoDefaults
	total := 0
	if in.AddToDefaults {
		var err error
		if defaults, err = d.Store.RepoDefaults(ctx); err != nil {
			d.failed(w, r, "reading the defaults", err)
			return
		}
		if len(defaults.Skills) > 0 { // an empty list is nil to LibrarySize: the stored default list
			if total, err = d.Store.LibrarySize(ctx, defaults.Skills); err != nil {
				d.failed(w, r, "checking the skill budget", err)
				return
			}
		}
	}

	out := make([]SkillFetchResultView, 0, len(names))
	var added []string
	installed := 0
	for _, name := range names {
		res := SkillFetchResultView{Name: name}
		file, found := fileNamed(f, name)
		switch {
		case !found:
			res.Error = msgNotInRead
		default:
			res.Error = skillRefusal(file, name, held)
		}
		if res.Error == "" && in.AddToDefaults {
			// Over the budget this name is refused whole, as a written skill
			// would be: it goes in with the box unticked.
			res.Error = budgetMessage(total + len(file.Content))
		}
		if res.Error != "" {
			out = append(out, res)
			continue
		}
		src := fetchedSource(f, file.Path)
		version, err := d.Store.CreateLibrarySkill(ctx, store.NewSkill{Name: name, Content: file.Content,
			Actor: skillActor(se), Source: &src, At: d.now()})
		switch {
		case errors.Is(err, store.ErrSkillExists):
			res.Error = msgSkillExists
		case errors.Is(err, store.ErrLibraryFull):
			res.Error = msgLibraryFull()
		case err != nil:
			d.Log.Error("dashboard: installing a skill failed", "skill", name, "error", err.Error())
			res.Error = msgWriteFailed
		default:
			res.OK = true
			installed++
			d.Log.Info("dashboard: skill installed", "skill", name, "version", version, "fetch", f.ID, "by", se.Username)
			d.auditSkill(r, "skill_installed", name, fmt.Sprintf("installed from %s, v%d", fetchOrigin(f), version))
			if in.AddToDefaults {
				total += len(file.Content)
				added = append(added, name)
			}
		}
		out = append(out, res)
	}
	if installed > 0 {
		d.inputs.invalidate()
	}
	if len(added) > 0 {
		if err := d.Store.AddDefaultSkills(ctx, added); err != nil {
			// The skills are in the library already; they can be added from
			// Settings.
			d.Log.Error("dashboard: adding installed skills to the default skills failed", "skills", added, "error", err.Error())
			writeError(w, http.StatusInternalServerError, msgWriteFailed)
			return
		}
		d.inputs.invalidate()
		d.Log.Info("dashboard: defaults updated", "fields", []string{"skills"}, "by", se.Username)
		d.audit(ctx, se, "defaults_updated", "", "", "skills")
		d.recheck("", "")
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Dashboard) updateSkills(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	f, ok := d.ownFetch(w, r)
	if !ok {
		return
	}
	var in struct {
		Names []string `json:"names"`
	}
	if decodeStrict(r, &in) != nil {
		writeError(w, http.StatusBadRequest, msgBadUpdateBody)
		return
	}
	names, ok := fetchNames(in.Names)
	if !ok {
		writeError(w, http.StatusBadRequest, msgBadUpdateBody)
		return
	}
	if !fetchReady(w, f) {
		return
	}
	if f.Purpose != store.FetchUpdate {
		writeError(w, http.StatusConflict, msgNotUpdateCheck)
		return
	}
	held, ok := d.readHeld(w, r)
	if !ok {
		return
	}
	installed, err := d.Store.InstalledFrom(ctx, installedSource(f))
	if err != nil {
		d.failed(w, r, "reading the installed skills", err)
		return
	}
	byName := map[string]skillUpdate{}
	for _, u := range matchUpdates(f, installed) {
		byName[u.view.Name] = u
	}

	out := make([]SkillFetchResultView, 0, len(names))
	updated := 0
	for _, name := range names {
		res := SkillFetchResultView{Name: name}
		u, found := byName[name]
		switch {
		case !found:
			res.Error = msgNotInstalled
		case u.view.State == msgRemovedAtSrc || u.view.State == msgRenamedAtSrc:
			res.Error = u.view.State
		default:
			res.Error = skillRefusal(u.file, name, held)
		}
		if res.Error != "" {
			out = append(out, res)
			continue
		}
		src := fetchedSource(f, u.file.Path)
		if u.view.State == updateSame && *u.installed.Source == src {
			res.OK = true // nothing to save: same text, ref and commit
			out = append(out, res)
			continue
		}
		version, err := d.Store.UpdateInstalledSkill(ctx, name, u.file.Content, skillActor(se), src, d.now())
		switch {
		case errors.Is(err, store.ErrSkillNotFound):
			res.Error = msgSkillGone
		case err != nil:
			d.Log.Error("dashboard: updating a skill failed", "skill", name, "error", err.Error())
			res.Error = msgWriteFailed
		default:
			res.OK = true
			updated++
			d.Log.Info("dashboard: skill updated from its source", "skill", name, "version", version, "fetch", f.ID, "by", se.Username)
			d.auditSkill(r, "skill_source_updated", name,
				fmt.Sprintf("v%d -> v%d from %s", u.installed.Version, version, fetchOrigin(f)))
		}
		out = append(out, res)
	}
	if updated > 0 {
		d.inputs.invalidate()
	}
	writeJSON(w, http.StatusOK, out)
}

// --- who sees a source repository's path (D11) ---

const (
	// sightBudget bounds the platform lookups of one request that lists
	// skills; a repository not answered in time is shown as hidden.
	sightBudget = 5 * time.Second
	// sightFailTTL is how long a failure to tell is kept.
	sightFailTTL = 30 * time.Second
	// sightWorkers is how many repositories one request asks about at once.
	sightWorkers = 8
)

// sightCache keeps, per session and source repository, whether the
// session's user can see the repository on the platform: an answer for
// AccessTTL, a failure to tell for sightFailTTL (as "no").
type sightCache struct {
	mu      sync.Mutex
	entries map[string]sightEntry
}

type sightEntry struct {
	ok     bool
	failed bool
	at     time.Time
}

func (e sightEntry) ttl() time.Duration {
	if e.failed {
		return sightFailTTL
	}
	return AccessTTL
}

// sourceSight is who sees what for one request: whether the session may see
// each source repository, by sightKey. A repository it lacks is not seen.
type sourceSight map[string]bool

func sightKey(instance, repo string) string { return instance + "\x00" + lower(repo) }

func (s sourceSight) sees(instance, repo string) bool { return s[sightKey(instance, repo)] }

// sightOf decides, for one request, which of the source repositories of
// sources se may see: each distinct repository once, a few at a time, all
// within sightBudget. A repository not answered in time is not seen (D11),
// so a platform that hangs costs the request the budget, not a wait per
// skill.
func (d *Dashboard) sightOf(ctx context.Context, se Session, sources ...*store.SkillSource) sourceSight {
	type pair struct{ instance, repo string }
	var todo []pair
	out := sourceSight{}
	for _, s := range sources {
		if s == nil || s.Kind != store.SourceKindInstance {
			continue
		}
		k := sightKey(s.Instance, s.Repo)
		if _, dup := out[k]; !dup {
			out[k] = false
			todo = append(todo, pair{s.Instance, s.Repo})
		}
	}
	if len(todo) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, d.sightBudget)
	defer cancel()
	var mu sync.Mutex
	answers := make(map[string]bool, len(todo))
	var wg sync.WaitGroup
	slots := make(chan struct{}, sightWorkers)
	for _, p := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			ok := d.seesSourceRepo(ctx, se, p.instance, p.repo)
			mu.Lock()
			answers[sightKey(p.instance, p.repo)] = ok
			mu.Unlock()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	mu.Lock()
	defer mu.Unlock()
	for k, ok := range answers {
		out[k] = ok
	}
	return out
}

// seesSourceRepo reports whether se may see the path of repo, a skill
// source on instance: an operator always; a user of that instance who sees
// it as a repository of its teams, or whom the platform lets read it (asked
// with the user's token, the answer kept AccessTTL per session and
// repository). Anyone else, and a failure to tell, does not; the failure is
// kept sightFailTTL, so a platform that is down is not asked again at once.
func (d *Dashboard) seesSourceRepo(ctx context.Context, se Session, instance, repo string) bool {
	switch {
	case se.Operator:
		return true
	case se.Platform == "" || se.Waiting || se.Instance != instance:
		return false
	case se.CanSee(instance, repo):
		return true
	}
	c, key, now := d.sight, string(se.idHash)+"\x00"+lower(repo), d.now()
	c.mu.Lock()
	e, found := c.entries[key]
	c.mu.Unlock()
	if found && now.Sub(e.at) < e.ttl() && !now.Before(e.at) {
		return e.ok
	}
	ok, err := d.userCanSee(ctx, se, repo)
	if err != nil {
		d.Log.Warn("dashboard: checking whether a user can see a skill source failed", "instance", instance, "error", err.Error())
		if errors.Is(err, context.Canceled) {
			return false // the user went away: nothing to learn about the platform
		}
		ok = false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, old := range c.entries {
		if now.Sub(old.at) >= old.ttl() {
			delete(c.entries, k)
		}
	}
	c.entries[key] = sightEntry{ok: ok, failed: err != nil, at: now}
	return ok
}
