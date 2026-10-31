package dashboard

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// switchedOn is the repositories switched on in the coverage tests: two on
// main (one spelled with capitals) and one on gh.
var switchedOn = []settings.Effective{
	{Instance: "main", Project: "acme/widgets"},
	{Instance: "main", Project: "Acme/Payments"},
	{Instance: "gh", Project: "acme/widgets"},
}

func wantRoles(t *testing.T, got coverage, want map[RepoKey]Role) {
	t.Helper()
	if !maps.Equal(got.roles, want) {
		t.Errorf("roles = %v, want %v", got.roles, want)
	}
}

// An admin team has no repository list: it covers every repository
// switched on for the session's instance, as admin.
func TestCoverageAdminTeam(t *testing.T) {
	teams := []store.Team{{Name: "ops", Role: store.RoleAdmin}}
	got := cover(teams, switchedOn, "main")
	if !got.admin {
		t.Error("an admin team's member is no admin")
	}
	wantRoles(t, got, map[RepoKey]Role{
		{"main", "acme/widgets"}: RoleAdmin, {"main", "acme/payments"}: RoleAdmin,
	})
	if !slices.Equal(got.projects, []string{"Acme/Payments", "acme/widgets"}) {
		t.Errorf("projects = %v, want main's, as the resolver spells them", got.projects)
	}
	// Repositories listed on an admin team change nothing.
	teams[0].Repos = []store.TeamRepo{{Instance: "main", Project: "acme/widgets"}}
	wantRoles(t, cover(teams, switchedOn, "main"), map[RepoKey]Role{
		{"main", "acme/widgets"}: RoleAdmin, {"main", "acme/payments"}: RoleAdmin,
	})
	if got := cover([]store.Team{{Role: store.RoleViewer}}, switchedOn, "main"); got.admin {
		t.Error("a viewer team made an admin")
	}
}

// A team repository with no project covers every repository switched on
// for that instance; a named one covers that one, whatever its case, and
// only while it is switched on.
func TestCoverageWholeInstance(t *testing.T) {
	whole := []store.Team{{Role: store.RoleViewer, Repos: []store.TeamRepo{{Instance: "main"}}}}
	wantRoles(t, cover(whole, switchedOn, "main"), map[RepoKey]Role{
		{"main", "acme/widgets"}: RoleViewer, {"main", "acme/payments"}: RoleViewer,
	})
	named := []store.Team{{Role: store.RoleMaintainer, Repos: []store.TeamRepo{
		{Instance: "main", Project: "ACME/payments"}, {Instance: "main", Project: "acme/switched-off"},
	}}}
	got := cover(named, switchedOn, "main")
	wantRoles(t, got, map[RepoKey]Role{{"main", "acme/payments"}: RoleMaintainer})
	if !slices.Equal(got.projects, []string{"Acme/Payments"}) {
		t.Errorf("projects = %v", got.projects)
	}
	if got.admin {
		t.Error("a maintainer team made an admin")
	}
}

// Of the teams covering a repository, the highest role wins.
func TestCoverageHighestRoleWins(t *testing.T) {
	teams := []store.Team{
		{Role: store.RoleViewer, Repos: []store.TeamRepo{{Instance: "main"}}},
		{Role: store.RoleMaintainer, Repos: []store.TeamRepo{{Instance: "main", Project: "acme/widgets"}}},
		{Role: store.RoleViewer, Repos: []store.TeamRepo{{Instance: "main", Project: "acme/widgets"}}},
	}
	wantRoles(t, cover(teams, switchedOn, "main"), map[RepoKey]Role{
		{"main", "acme/widgets"}: RoleMaintainer, {"main", "acme/payments"}: RoleViewer,
	})
	teams = append(teams, store.Team{Role: store.RoleAdmin})
	wantRoles(t, cover(teams, switchedOn, "main"), map[RepoKey]Role{
		{"main", "acme/widgets"}: RoleAdmin, {"main", "acme/payments"}: RoleAdmin,
	})
}

// A team may cover repositories on several instances; a member sees only
// those of the instance they signed in with.
func TestCoverageIgnoresOtherInstances(t *testing.T) {
	teams := []store.Team{{Role: store.RoleMaintainer, Repos: []store.TeamRepo{
		{Instance: "gh"}, {Instance: "gh", Project: "acme/widgets"}, {Instance: "main", Project: "acme/widgets"},
	}}}
	got := cover(teams, switchedOn, "main")
	wantRoles(t, got, map[RepoKey]Role{{"main", "acme/widgets"}: RoleMaintainer})
	wantRoles(t, cover(teams, switchedOn, "gh"), map[RepoKey]Role{{"gh", "acme/widgets"}: RoleMaintainer})
	if other := cover(teams, switchedOn, "other"); len(other.roles) != 0 || other.scope != cover(nil, nil, "main").scope {
		t.Errorf("coverage on an instance the team doesn't name = %+v", other)
	}
}

// The scope is a hash of what is covered: the same set in any order, or
// spelled in any case, gives the same scope, and another set another one.
func TestScopeStable(t *testing.T) {
	teams := []store.Team{{Role: store.RoleViewer, Repos: []store.TeamRepo{{Instance: "main"}}}}
	a := cover(teams, switchedOn, "main").scope
	reversed := slices.Clone(switchedOn)
	slices.Reverse(reversed)
	reversed[1].Project = "ACME/PAYMENTS"
	if b := cover(teams, reversed, "main").scope; b != a {
		t.Errorf("scope in another order = %s, want %s", b, a)
	}
	if b := scopeOf([]RepoKey{{"main", "acme/widgets"}, {"main", "acme/payments"}}); b != a {
		t.Errorf("scopeOf = %s, want %s", b, a)
	}
	// A role change covers the same repositories: same scope.
	teams[0].Role = store.RoleMaintainer
	if b := cover(teams, switchedOn, "main").scope; b != a {
		t.Errorf("scope after a role change = %s, want %s", b, a)
	}
	if b := cover(teams, switchedOn[:1], "main").scope; b == a {
		t.Error("covering fewer repositories gives the same scope")
	}
	if len(a) != 64 {
		t.Errorf("scope %q is not a sha256 in hex", a)
	}
}

// A team whose role pergent.sh doesn't know is as no team: it covers
// nothing, makes no admin, and doesn't count for waiting.
func TestUnknownRoleTeamIsAbsent(t *testing.T) {
	odd := store.Team{Name: "odd", Role: "owner", Repos: []store.TeamRepo{{Instance: "main"}}}
	got := cover([]store.Team{odd}, switchedOn, "main")
	if got.admin || len(got.roles) != 0 || len(got.projects) != 0 || got.scope != cover(nil, switchedOn, "main").scope {
		t.Errorf("coverage of an unknown-role team = %+v; want none", got)
	}
	viewers := store.Team{Name: "viewers", Role: store.RoleViewer}
	if known := knownTeams([]store.Team{odd, viewers}); len(known) != 1 || known[0].Name != "viewers" {
		t.Errorf("knownTeams = %+v; want viewers only", known)
	}
	if known := knownTeams([]store.Team{odd}); len(known) != 0 {
		t.Errorf("knownTeams of an unknown-role team = %+v; want none (waiting)", known)
	}
}

// A session's role on a repository is RoleNone where it can't see it, the
// coverage's role where it can, and admin everywhere for an operator.
func TestSessionRole(t *testing.T) {
	widgets := RepoKey{"main", "acme/widgets"}
	se := Session{
		Visible: map[RepoKey]int64{widgets: 7},
		Roles:   map[RepoKey]Role{widgets: RoleMaintainer, {"main", "acme/payments"}: RoleViewer},
	}
	if se.Role("main", "Acme/Widgets") != RoleMaintainer || !se.CanSee("main", "ACME/widgets") || !se.CanSeeRun("main", "acme/widgets", 7) {
		t.Errorf("visible repository: role %v", se.Role("main", "acme/widgets"))
	}
	if se.Role("main", "acme/payments") != RoleNone || se.CanSee("main", "acme/payments") {
		t.Error("a covered repository the platform hides has a role")
	}
	if se.CanSee("gh", "acme/widgets") || se.CanSeeRun("main", "acme/widgets", 8) {
		t.Error("another instance or another project ID is visible")
	}
	op := Session{Operator: true, Admin: true, All: true}
	if op.Role("gh", "acme/anything") != RoleAdmin || !op.CanSee("gh", "acme/anything") || !op.CanSeeRun("gh", "acme/anything", 3) {
		t.Error("an operator doesn't see everything as admin")
	}
	for r, name := range map[Role]string{RoleNone: "", RoleViewer: "viewer", RoleMaintainer: "maintainer", RoleAdmin: "admin"} {
		if r.String() != name {
			t.Errorf("Role(%d).String() = %q, want %q", r, r.String(), name)
		}
	}
}

// requireAdmin lets operators and admin-team members through,
// requireOperator only operators; everyone else gets 403 and h never runs.
func TestRequireAdminAndOperator(t *testing.T) {
	sessions := map[string]Session{
		"operator": {Operator: true, Admin: true, All: true},
		"admin":    {Admin: true},
		"viewer":   {},
	}
	for name, se := range sessions {
		for guard, wrap := range map[string]func(http.HandlerFunc) http.HandlerFunc{
			"requireAdmin": requireAdmin, "requireOperator": requireOperator,
		} {
			ran := false
			h := wrap(func(w http.ResponseWriter, _ *http.Request) { ran = true; w.WriteHeader(http.StatusOK) })
			req := httptest.NewRequest("GET", "/", nil)
			req = req.WithContext(context.WithValue(req.Context(), sessionKey{}, se))
			rec := httptest.NewRecorder()
			h(rec, req)
			want := name == "operator" || (guard == "requireAdmin" && name == "admin")
			if ran != want || (rec.Code == http.StatusOK) != want || (!want && rec.Code != http.StatusForbidden) {
				t.Errorf("%s for %s: ran %v, %d; want ran %v", guard, name, ran, rec.Code, want)
			}
		}
	}
}
