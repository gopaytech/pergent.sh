package dashboard

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func TestRepositoryChoices(t *testing.T) {
	t.Parallel()
	eff := func(instance, project string, paused bool) settings.Effective {
		return settings.Effective{Instance: instance, Project: project, Paused: paused}
	}
	key := func(c RepositoryChoiceView) string { return c.Instance + "|" + c.Project }
	keys := func(v RepositoryChoicesView) []string {
		out := []string{}
		for _, c := range v.Items {
			out = append(out, key(c))
		}
		return out
	}
	other := &TeamRef{ID: 2, Name: "Widgets"}
	many := make([]settings.Effective, 0, 51)
	for i := range 51 {
		many = append(many, eff("main", fmt.Sprintf("acme/r%02d", i), false))
	}

	cases := []struct {
		name      string
		on        []settings.Effective
		instances []string
		teams     []store.Team
		search    string
		page      int
		want      []string
		hasMore   bool
		check     func(t *testing.T, v RepositoryChoicesView)
	}{
		{name: "empty search lists instances first, then repositories sorted, paused included",
			on:        []settings.Effective{eff("main", "Acme/b", false), eff("lab", "z/x", true), eff("main", "acme/A", false)},
			instances: []string{"main", "lab"}, page: 1,
			want: []string{"lab|", "main|", "lab|z/x", "main|acme/A", "main|Acme/b"},
			check: func(t *testing.T, v RepositoryChoicesView) {
				if !v.Items[2].Paused || v.Items[3].Paused {
					t.Errorf("paused = %+v", v.Items)
				}
				if v.Page != 1 {
					t.Errorf("page = %d", v.Page)
				}
			}},
		{name: "search keeps matching projects and instance names, ignoring case",
			on:        []settings.Effective{eff("main", "acme/Payments", false), eff("main", "acme/widgets", false), eff("pay-lab", "x/y", false)},
			instances: []string{"main", "pay-lab"}, search: "PAY", page: 1,
			want: []string{"pay-lab|", "main|acme/Payments"}},
		{name: "entries on the team are left out, in any case",
			on:        []settings.Effective{eff("main", "acme/widgets", false), eff("main", "acme/other", false), eff("lab", "a/b", false)},
			instances: []string{"main", "lab"}, page: 1,
			teams: []store.Team{{ID: 1, Repos: []store.TeamRepo{{Instance: "main", Project: "Acme/Widgets"}, {Instance: "lab"}}}},
			want:  []string{"main|", "lab|a/b", "main|acme/other"}},
		{name: "owner team at the same level",
			on:        []settings.Effective{eff("main", "acme/owned", false), eff("main", "acme/inst-only", false), eff("main", "acme/plain", false)},
			instances: []string{"main"}, page: 1,
			teams: []store.Team{
				{ID: 1, Name: "Mine"},
				{ID: 2, Name: "Widgets", Repos: []store.TeamRepo{{Instance: "main", Project: "Acme/Owned", Owner: true}, {Instance: "main", Owner: true}, {Instance: "main", Project: "acme/plain"}}},
			},
			want: []string{"main|", "main|acme/inst-only", "main|acme/owned", "main|acme/plain"},
			check: func(t *testing.T, v RepositoryChoicesView) {
				got := map[string]*TeamRef{}
				for _, c := range v.Items {
					got[key(c)] = c.OwnedBy
				}
				if !reflect.DeepEqual(got["main|acme/owned"], other) {
					t.Errorf("owned = %v", got["main|acme/owned"])
				}
				if got["main|acme/inst-only"] != nil || got["main|acme/plain"] != nil {
					t.Errorf("inst-only/plain = %v, %v", got["main|acme/inst-only"], got["main|acme/plain"])
				}
				if !reflect.DeepEqual(got["main|"], other) {
					t.Errorf("instance owner = %v", got["main|"])
				}
			}},
		{name: "page 1 of 51 matches", on: many, page: 1, hasMore: true,
			check: func(t *testing.T, v RepositoryChoicesView) {
				if len(v.Items) != repositoryChoicesPage {
					t.Errorf("items = %d", len(v.Items))
				}
			}},
		{name: "page 2 of 51 matches, no instances", on: many, instances: []string{"main"}, page: 2,
			want: []string{"main|acme/r50"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := repositoryChoices(tc.on, tc.instances, tc.teams, 1, tc.search, tc.page)
			if tc.want != nil {
				if got := keys(v); !reflect.DeepEqual(got, tc.want) {
					t.Errorf("items = %v; want %v", got, tc.want)
				}
			}
			if v.HasMore != tc.hasMore {
				t.Errorf("hasMore = %v; want %v", v.HasMore, tc.hasMore)
			}
			if v.Items == nil {
				t.Error("items is nil")
			}
			if tc.check != nil {
				tc.check(t, v)
			}
		})
	}
	// Page 1 of the 51 matches, with an instance entry, still has 50
	// repositories after it; the entry is on page 1 only.
	p1 := repositoryChoices(many, []string{"main"}, nil, 1, "", 1)
	if len(p1.Items) != repositoryChoicesPage+1 || p1.Items[0].Project != "" || !p1.HasMore {
		t.Errorf("page 1 = %d items, hasMore %v", len(p1.Items), p1.HasMore)
	}
}

func TestRepositoryChoicesRoute(t *testing.T) {
	t.Parallel()
	te := newTeamsEnv(t)
	te.addLab(te.admin)
	base := teamURL(te.payments, "/repository-choices")

	// Payments has main/acme/payments and main/acme/widgets. main/acme/gadgets
	// is paused and owned by Widgets.
	te.pausedRepo("main", "acme/gadgets", nil)
	te.editTeam(te.widgets, func(tm *store.Team) {
		tm.Repos = append(tm.Repos, store.TeamRepo{Instance: "main", Project: "acme/gadgets", Owner: true})
	})
	keysOf := func(v RepositoryChoicesView) []string {
		out := []string{}
		for _, c := range v.Items {
			out = append(out, c.Instance+"|"+c.Project)
		}
		return out
	}

	t.Run("the admin gets the instances and the repositories not on the team", func(t *testing.T) {
		v := decode[RepositoryChoicesView](t, te.get(te.admin, base), "GET choices", 200)
		want := []string{"gh|", "lab|", "main|", "gh|acme/widgets", "main|acme/gadgets"}
		if got := keysOf(v); !reflect.DeepEqual(got, want) || v.Page != 1 || v.HasMore {
			t.Fatalf("items = %v, page %d, hasMore %v; want %v", got, v.Page, v.HasMore, want)
		}
		g := v.Items[4]
		if !g.Paused || g.OwnedBy == nil || g.OwnedBy.ID != te.widgets || g.OwnedBy.Name != "Widgets" {
			t.Errorf("gadgets = %+v, owner %+v", g, g.OwnedBy)
		}
		if w := v.Items[3]; w.Paused || w.OwnedBy != nil {
			t.Errorf("gh widgets = %+v", w)
		}
		if v.Items[2].OwnedBy != nil {
			t.Errorf("main instance owner = %+v", v.Items[2].OwnedBy)
		}
	})
	t.Run("search keeps the matches", func(t *testing.T) {
		v := decode[RepositoryChoicesView](t, te.get(te.admin, base+"?search=GADG"), "GET search", 200)
		if got := keysOf(v); !reflect.DeepEqual(got, []string{"main|acme/gadgets"}) {
			t.Errorf("items = %v", got)
		}
	})
	t.Run("an instance being removed is not offered", func(t *testing.T) {
		te.reg.SetRemoving("lab")
		v := decode[RepositoryChoicesView](t, te.get(te.admin, base), "GET choices", 200)
		for _, c := range v.Items {
			if c.Instance == "lab" {
				t.Errorf("lab is offered: %+v", c)
			}
		}
		if len(v.Items) != 4 {
			t.Errorf("items = %v", keysOf(v))
		}
	})
	for _, p := range []string{"0", "x"} {
		t.Run("bad page "+p, func(t *testing.T) {
			rec := te.get(te.admin, base+"?page="+p)
			wantCode(t, rec, "GET page="+p, 400)
		})
	}
	t.Run("search too long", func(t *testing.T) {
		wantCode(t, te.get(te.admin, base+"?search="+strings.Repeat("a", maxSearch+1)), "GET long search", 400)
	})
	t.Run("unknown team", func(t *testing.T) {
		wantCode(t, te.get(te.admin, teamURL(99999, "/repository-choices")), "GET unknown team", 404)
	})
}
