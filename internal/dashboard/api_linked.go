package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/links"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// maxLinkable is the most repositories GET /api/admin/linkable lists.
	maxLinkable = 200
	// linkProbes is how many repositories the list reads at once.
	linkProbes = 8
	// linkBudget bounds the reads of the whole list: a repository not read in
	// time comes back unchecked, and the save checks it.
	linkBudget = 12 * time.Second
	// linkCheckBudget bounds the checks of a save.
	linkCheckBudget = 30 * time.Second
	// linkInfoTTL is how long what the list read about a repository is kept.
	linkInfoTTL = catalogTTL
	// maxWatched is the most watched paths of one link, and maxWatchedLen the
	// longest.
	maxWatched    = 50
	maxWatchedLen = 200

	msgNeedsAdminLinks = "only admins can change the linked repositories"
	msgBadLinks        = `linkedRepos must be a list of at most 3 links like {"target": "owner/name", "watched": ["glob", ...], "canBlock": true}, or null for none`
	msgLinksGitLab     = "linked repositories work on GitHub repositories only"
	msgLinksNoApp      = "this instance can't check linked repositories right now, so nothing was saved"
	msgLinksRead       = "could not check %s with GitHub, so nothing was saved; please try again"
	msgLinkableRepo    = `name the instance and the repository: ?instance=<name>&repo=<owner/name>`

	reasonUnchecked = "not checked yet; it is checked when you save"
	reasonItself    = "this is the repository itself"
	reasonLinked    = "already linked"
	reasonFull      = "this repository has the most links it can have"
	reasonNoApp     = "the pergent review App can't read it"
	reasonInstall   = "it is in another installation of the review App, and a review's token covers one installation"
)

// RepoLinker reads what the rules for a link need about a repository of a
// GitHub instance, by its path ("owner/name"): who may read it, and the
// installation of the review App that covers it. githubforge.Forge is one; it
// answers forge.ErrSourceNotFound for a repository the App can't read, and
// anything else for a read that failed. The dashboard finds it as the instance's
// RepoCatalog; one that isn't also a RepoLinker can't save links.
type RepoLinker interface {
	LinkInfo(ctx context.Context, repo string) (forge.Visibility, int64, error)
}

// LinkView is one link of a repository, as the API shows and takes it.
type LinkView struct {
	Target string `json:"target"`
	// Watched are globs of changed files; none means every change.
	Watched []string `json:"watched"`
	// CanBlock is whether a finding on this link may block; off makes every
	// finding of it a suggestion.
	CanBlock bool `json:"canBlock"`
}

// linksView is links as the API shows them: never null.
func linksView(ls []store.Link) []LinkView {
	out := make([]LinkView, len(ls))
	for i, l := range ls {
		out[i] = LinkView{Target: l.Target, Watched: nonNil(slices.Clone(l.Watched)), CanBlock: l.CanBlock}
	}
	return out
}

// isLinkedKey is whether key is one of the two settings only an admin changes.
func isLinkedKey(key string) bool { return key == "linkedReposEnabled" || key == "linkedRepos" }

// linkedReposSetting takes null or a list of links, at most links.Max, each
// {"target", "watched", "canBlock"}: canBlock left out is true, as a new link's
// switch is. Null and [] both clear the links. Only the shape is checked here;
// the rules (including those that need the platform) are checkLinks, which
// knows the repository.
func linkedReposSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v []store.Link
	if !null {
		var body []struct {
			Target   string   `json:"target"`
			Watched  []string `json:"watched"`
			CanBlock *bool    `json:"canBlock"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || len(body) > links.Max {
			return nil, msgBadLinks
		}
		for _, b := range body {
			if len(b.Watched) > maxWatched {
				return nil, fmt.Sprintf("a link watches at most %d paths", maxWatched)
			}
			l := store.Link{Target: strings.TrimSpace(b.Target), CanBlock: b.CanBlock == nil || *b.CanBlock}
			for _, p := range b.Watched {
				p = strings.TrimSpace(p)
				if len(p) > maxWatchedLen {
					return nil, fmt.Sprintf("a watched path is at most %d characters", maxWatchedLen)
				}
				l.Watched = append(l.Watched, p)
			}
			v = append(v, l)
		}
	}
	return func(row *store.DashboardRepo) { row.LinkedRepos = v }, ""
}

// linkProblem is why a save of links is refused: a 400 about the links, a 502
// when the platform can't tell.
type linkProblem struct {
	status int
	msg    string
}

// linkerOf is the instance's reader of link information.
func (d *Dashboard) linkerOf(instance string) (RepoLinker, bool) {
	cat, ok := d.catalogs.Get(instance)
	if !ok || cat == nil {
		return nil, false
	}
	l, ok := cat.(RepoLinker)
	return l, ok
}

// checkLinks is the rules for the links of repo, run before they are saved,
// on what the platform says now: GitHub only, links.Validate (at most 3, no
// itself, no twice, plain owner/name, good globs), then for each target its
// visibility against the repository's (links.Check) and the same installation
// of the review App. Anything the platform can't tell refuses the save: an
// unknown is never allowed. No links need no checks.
func (d *Dashboard) checkLinks(ctx context.Context, repo settings.Effective, ls []store.Link) *linkProblem {
	if len(ls) == 0 {
		return nil
	}
	if repo.Platform != config.PlatformGitHub {
		return &linkProblem{http.StatusBadRequest, msgLinksGitLab}
	}
	if err := links.Validate(repo.Project, ls); err != nil {
		return &linkProblem{http.StatusBadRequest, err.Error()}
	}
	linker, ok := d.linkerOf(repo.Instance)
	if !ok {
		return &linkProblem{http.StatusBadGateway, msgLinksNoApp}
	}
	ctx, cancel := context.WithTimeout(ctx, linkCheckBudget)
	defer cancel()
	srcVis, srcInst, err := linker.LinkInfo(ctx, repo.Project)
	if err != nil {
		d.Log.Warn("dashboard: reading a repository to check its links failed",
			"instance", repo.Instance, "project", repo.Project, "error", err.Error())
		return &linkProblem{http.StatusBadGateway, fmt.Sprintf(msgLinksRead, "this repository")}
	}
	for _, l := range ls {
		tgtVis, tgtInst, err := linker.LinkInfo(ctx, l.Target)
		switch {
		case errors.Is(err, forge.ErrSourceNotFound):
			return &linkProblem{http.StatusBadRequest, l.Target + ": " + reasonNoApp + " (install it there, or check the name)"}
		case err != nil:
			d.Log.Warn("dashboard: reading a linked repository to check it failed",
				"instance", repo.Instance, "project", repo.Project, "error", err.Error())
			return &linkProblem{http.StatusBadGateway, fmt.Sprintf(msgLinksRead, l.Target)}
		}
		if err := links.Check(srcVis, tgtVis); err != nil {
			return &linkProblem{http.StatusBadRequest, l.Target + ": " + err.Error()}
		}
		if tgtInst != srcInst {
			return &linkProblem{http.StatusBadRequest, l.Target + ": " + reasonInstall}
		}
	}
	return nil
}

// --- GET /api/admin/linkable ---

// LinkableView is GET /api/admin/linkable?instance=&repo=: repositories the
// instance's review App reaches, each with whether `repo` may link it.
type LinkableView struct {
	Items []LinkableRepo `json:"items"`
	// Truncated is true when there were more repositories than are listed.
	Truncated bool `json:"truncated"`
}

// LinkableRepo is one repository of LinkableView.
type LinkableRepo struct {
	Repo string `json:"repo"`
	// Visibility is "public", "internal" or "private" when it was read, else "".
	Visibility string `json:"visibility"`
	// Checked is whether the rules were applied to it. An unchecked one (the
	// reads ran out of time, or one failed) is Linkable true with a reason that
	// says it is checked when the links are saved: the save is what enforces
	// the rules, this list only helps to choose.
	Checked bool `json:"checked"`
	// Linkable is whether it can be linked, Reason why not, or "" when it can.
	Linkable bool   `json:"linkable"`
	Reason   string `json:"reason"`
}

// linkInfoCache keeps what the list read about a repository for linkInfoTTL, so
// opening the picker twice reads once. Failures aren't kept. The save never
// uses it.
type linkInfoCache struct {
	mu      sync.Mutex
	entries map[string]linkInfoEntry
}

type linkInfoEntry struct {
	vis  forge.Visibility
	inst int64
	at   time.Time
}

func (c *linkInfoCache) get(key string, now time.Time) (linkInfoEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.Sub(e.at) >= linkInfoTTL || now.Before(e.at) {
		return linkInfoEntry{}, false
	}
	return e, true
}

func (c *linkInfoCache) put(key string, e linkInfoEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]linkInfoEntry{}
	}
	for k, old := range c.entries {
		if e.at.Sub(old.at) >= linkInfoTTL {
			delete(c.entries, k)
		}
	}
	c.entries[key] = e
}

// linkable lists what `repo` of `instance` may link. The repositories are those
// the review App reaches (the picker's list, kept 5 minutes, at most 200 shown
// in order of name, `search` narrowing it). The rules that need no read are
// applied first: itself, already linked, and the most links. For the others the
// visibility and installation are read once each, 8 at a time within 12 seconds
// (and kept 5 minutes); one not read in time or whose read failed is unchecked,
// and the save checks it again.
func (d *Dashboard) linkable(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	q := r.URL.Query()
	instance, project, search := q.Get("instance"), strings.TrimSpace(q.Get("repo")), q.Get("search")
	if instance == "" || project == "" || len(project) > 1024 {
		writeError(w, http.StatusBadRequest, msgLinkableRepo)
		return
	}
	if len(search) > maxSearch {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("search is at most %d characters", maxSearch))
		return
	}
	if !d.knownInstance(instance) {
		notFound(w)
		return
	}
	repos, err := d.activeRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	i := slices.IndexFunc(repos, func(e settings.Effective) bool {
		return e.Instance == instance && strings.EqualFold(e.Project, project)
	})
	if i < 0 {
		notFound(w)
		return
	}
	repo := repos[i]
	if repo.Platform != config.PlatformGitHub {
		writeError(w, http.StatusBadRequest, msgLinksGitLab)
		return
	}
	linker, ok := d.linkerOf(instance)
	if !ok {
		writeError(w, http.StatusBadGateway, msgLinksNoApp)
		return
	}
	listed, truncated, err := d.catalogRepos(ctx, instance, search)
	if err != nil {
		d.Log.Warn("dashboard: listing repositories to link failed", "instance", instance, "user", se.Username, "error", err.Error())
		writeError(w, http.StatusBadGateway, msgListFailed)
		return
	}
	listed = slices.Clone(listed) // the list is the cache's
	sort.Slice(listed, func(i, j int) bool { return strings.ToLower(listed[i]) < strings.ToLower(listed[j]) })
	if len(listed) > maxLinkable {
		listed, truncated = listed[:maxLinkable], true
	}

	linked := map[string]bool{}
	for _, l := range repo.LinkedRepos {
		linked[strings.ToLower(l.Target)] = true
	}
	items := make([]LinkableRepo, len(listed))
	var toRead []int
	for i, name := range listed {
		items[i] = LinkableRepo{Repo: name}
		switch lower := strings.ToLower(name); {
		case lower == strings.ToLower(repo.Project):
			items[i].Reason = reasonItself
		case linked[lower]:
			items[i].Reason = reasonLinked
		case len(repo.LinkedRepos) >= links.Max:
			items[i].Reason = reasonFull
		default:
			toRead = append(toRead, i)
		}
	}
	if len(toRead) > 0 {
		d.judgeLinkable(ctx, linker, repo, items, toRead)
	}
	writeJSON(w, http.StatusOK, LinkableView{Items: items, Truncated: truncated})
}

// judgeLinkable fills in items[i] for each i of toRead: the visibility and
// installation of the repository (the source's first; if that can't be read
// every one stays unchecked), then links.Check and the installation.
func (d *Dashboard) judgeLinkable(ctx context.Context, linker RepoLinker, repo settings.Effective, items []LinkableRepo, toRead []int) {
	bctx, cancel := context.WithTimeout(ctx, linkBudget)
	defer cancel()
	unchecked := func(i int) {
		items[i].Checked, items[i].Linkable, items[i].Reason = false, true, reasonUnchecked
	}
	read := func(name string) (linkInfoEntry, error) {
		key := repo.Instance + "\x00" + strings.ToLower(name)
		if e, ok := d.linkInfo.get(key, d.now()); ok {
			return e, nil
		}
		vis, inst, err := linker.LinkInfo(bctx, name)
		if err != nil {
			return linkInfoEntry{}, err
		}
		e := linkInfoEntry{vis: vis, inst: inst, at: d.now()}
		d.linkInfo.put(key, e)
		return e, nil
	}
	src, err := read(repo.Project)
	if err != nil {
		d.Log.Warn("dashboard: reading a repository to list what it may link failed",
			"instance", repo.Instance, "project", repo.Project, "error", err.Error())
		for _, i := range toRead {
			unchecked(i)
		}
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, linkProbes)
	for _, i := range toRead {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if bctx.Err() != nil {
				unchecked(i)
				return
			}
			tgt, err := read(items[i].Repo)
			switch {
			case errors.Is(err, forge.ErrSourceNotFound):
				items[i].Checked, items[i].Reason = true, reasonNoApp
			case err != nil:
				unchecked(i)
			default:
				items[i].Checked, items[i].Visibility = true, string(tgt.vis)
				switch cerr := links.Check(src.vis, tgt.vis); {
				case cerr != nil:
					items[i].Reason = cerr.Error()
				case tgt.inst != src.inst:
					items[i].Reason = reasonInstall
				default:
					items[i].Linkable = true
				}
			}
		}()
	}
	wg.Wait()
}
