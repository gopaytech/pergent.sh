package dashboard

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// repositoryChoicesPage is how many repositories a page of choices holds.
const repositoryChoicesPage = 50

// repositoryChoices is one page of what may be added to team: the instances
// in instanceNames (page 1 only, sorted by name), then the switched-on
// repositories in on, sorted by instance and lower-cased project, 50 to a
// page. search keeps the instance names and projects that contain it, in any
// case. What is already on the team is left out, a repository in any case.
// OwnedBy is the other team that owns the entry at the same level.
func repositoryChoices(on []settings.Effective, instanceNames []string, teams []store.Team, team int64, search string, page int) RepositoryChoicesView {
	search = strings.ToLower(search)
	hit := func(s string) bool { return search == "" || strings.Contains(strings.ToLower(s), search) }

	taken := map[RepoKey]bool{}
	owner := map[RepoKey]*TeamRef{}
	for _, t := range teams {
		for _, tr := range t.Repos {
			k := repoKey(tr.Instance, tr.Project)
			if t.ID == team {
				taken[k] = true
			} else if tr.Owner && owner[k] == nil {
				owner[k] = &TeamRef{ID: t.ID, Name: t.Name}
			}
		}
	}

	v := RepositoryChoicesView{Items: []RepositoryChoiceView{}, Page: page}
	if page == 1 {
		names := slices.Clone(instanceNames)
		sort.Strings(names)
		for _, n := range names {
			k := repoKey(n, "")
			if hit(n) && !taken[k] {
				v.Items = append(v.Items, RepositoryChoiceView{Instance: n, OwnedBy: owner[k]})
			}
		}
	}

	var repos []settings.Effective
	for _, e := range on {
		if hit(e.Project) && !taken[repoKey(e.Instance, e.Project)] {
			repos = append(repos, e)
		}
	}
	sort.SliceStable(repos, func(i, j int) bool {
		if repos[i].Instance != repos[j].Instance {
			return repos[i].Instance < repos[j].Instance
		}
		return lower(repos[i].Project) < lower(repos[j].Project)
	})
	from := min((max(page, 1)-1)*repositoryChoicesPage, len(repos))
	to := min(from+repositoryChoicesPage, len(repos))
	for _, e := range repos[from:to] {
		v.Items = append(v.Items, RepositoryChoiceView{Instance: e.Instance, Project: e.Project,
			Paused: e.Paused, OwnedBy: owner[repoKey(e.Instance, e.Project)]})
	}
	v.HasMore = to < len(repos)
	return v
}

// teamRepositoryChoices is the search behind the team page's Add repository,
// for admins: 404 for a team that is not there, 400 for a bad page or a search
// that is too long.
func (d *Dashboard) teamRepositoryChoices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		notFound(w)
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > maxSearch {
		writeError(w, http.StatusBadRequest, "search must be at most 100 characters")
		return
	}
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxAvailablePage {
			writeError(w, http.StatusBadRequest, msgBadPage)
			return
		}
		page = n
	}
	_, found, err := d.Store.Team(ctx, id)
	if err != nil {
		d.failed(w, r, "reading the team", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	on, err := d.Repos.ListAll(ctx)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	teams, err := d.Store.Teams(ctx)
	if err != nil {
		d.failed(w, r, "listing teams", err)
		return
	}
	var names []string
	for _, inst := range d.registry.All() {
		if !inst.Removing {
			names = append(names, inst.Name)
		}
	}
	writeJSON(w, http.StatusOK, repositoryChoices(on, names, teams, id, search, page))
}
