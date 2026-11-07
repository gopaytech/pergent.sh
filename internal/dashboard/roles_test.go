package dashboard

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
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

// alice's account ID in the coverage tests.
const coverAccount = 7

// member is a team alice is on with role, covering repos.
func member(role string, repos ...store.TeamRepo) store.Team {
	return store.Team{Members: []store.TeamMember{{AccountID: 99, Role: store.RoleMaintainer}, {AccountID: coverAccount, Role: role}}, Repos: repos}
}

func wantRoles(t *testing.T, teams []store.Team, want map[RepoKey]Role) {
	t.Helper()
	got := cover(teamsOf(teams, coverAccount), switchedOn)
	if !maps.Equal(got, want) {
		t.Errorf("roles = %v, want %v", got, want)
	}
}

// A team repository with no project covers every repository switched on for
// that instance; a named one covers that one, whatever its case, and only
// while it is switched on.
func TestCoverageWholeInstance(t *testing.T) {
	wantRoles(t, []store.Team{member(store.RoleViewer, store.TeamRepo{Instance: "main"})}, map[RepoKey]Role{
		{"main", "acme/widgets"}: RoleViewer, {"main", "acme/payments"}: RoleViewer,
	})
	wantRoles(t, []store.Team{member(store.RoleMaintainer,
		store.TeamRepo{Instance: "main", Project: "ACME/payments"}, store.TeamRepo{Instance: "main", Project: "acme/switched-off"})},
		map[RepoKey]Role{{"main", "acme/payments"}: RoleMaintainer})
	// A team without repositories covers nothing.
	wantRoles(t, []store.Team{member(store.RoleViewer)}, map[RepoKey]Role{})
}

// The role is the account's own on the team: another member's role, or a team
// the account is not on, gives nothing.
func TestCoverageUsesTheAccountsOwnRole(t *testing.T) {
	// member() makes account 99 a maintainer and alice a viewer.
	wantRoles(t, []store.Team{member(store.RoleViewer, store.TeamRepo{Instance: "main", Project: "acme/widgets"})},
		map[RepoKey]Role{{"main", "acme/widgets"}: RoleViewer})
	notOn := store.Team{Members: []store.TeamMember{{AccountID: 99, Role: store.RoleMaintainer}},
		Repos: []store.TeamRepo{{Instance: "main"}}}
	wantRoles(t, []store.Team{notOn}, map[RepoKey]Role{})
}

// Of the teams covering a repository, the highest role wins.
func TestCoverageHighestRoleWins(t *testing.T) {
	teams := []store.Team{
		member(store.RoleViewer, store.TeamRepo{Instance: "main"}),
		member(store.RoleMaintainer, store.TeamRepo{Instance: "main", Project: "acme/widgets"}),
		member(store.RoleViewer, store.TeamRepo{Instance: "main", Project: "acme/widgets"}),
	}
	wantRoles(t, teams, map[RepoKey]Role{
		{"main", "acme/widgets"}: RoleMaintainer, {"main", "acme/payments"}: RoleViewer,
	})
	// The order of the teams does not matter.
	wantRoles(t, []store.Team{teams[2], teams[1], teams[0]}, map[RepoKey]Role{
		{"main", "acme/widgets"}: RoleMaintainer, {"main", "acme/payments"}: RoleViewer,
	})
}

// A team may cover repositories on several instances; the account has them all.
func TestCoverageSpansInstances(t *testing.T) {
	wantRoles(t, []store.Team{member(store.RoleMaintainer,
		store.TeamRepo{Instance: "gh"}, store.TeamRepo{Instance: "main", Project: "acme/widgets"})},
		map[RepoKey]Role{{"gh", "acme/widgets"}: RoleMaintainer, {"main", "acme/widgets"}: RoleMaintainer})
	wantRoles(t, []store.Team{member(store.RoleViewer, store.TeamRepo{Instance: "other"})}, map[RepoKey]Role{})
}

// A member role pergent.sh doesn't know (the database refuses one, but a
// team is read, not trusted) is as no membership: it covers nothing and
// doesn't count for waiting. "admin" is the account's role, never a team's.
func TestUnknownMemberRoleIsAbsent(t *testing.T) {
	for _, role := range []string{"owner", "admin", ""} {
		odd := member(role, store.TeamRepo{Instance: "main"})
		wantRoles(t, []store.Team{odd}, map[RepoKey]Role{})
		if mine := teamsOf([]store.Team{odd}, coverAccount); len(mine) != 0 {
			t.Errorf("role %q: teamsOf = %+v; want none (waiting)", role, mine)
		}
	}
	if mine := teamsOf([]store.Team{member(store.RoleViewer)}, coverAccount); len(mine) != 1 || mine[0].role != RoleViewer {
		t.Errorf("teamsOf = %+v; want one, as viewer", mine)
	}
}

// A session's role on a repository is RoleNone where it can't see it, the
// teams' role where it can, and admin everywhere for an admin.
func TestSessionRole(t *testing.T) {
	widgets := RepoKey{"main", "acme/widgets"}
	se := Session{
		Visible: map[RepoKey]int64{widgets: 7, {"main", "acme/gadgets"}: 0},
		Roles:   map[RepoKey]Role{widgets: RoleMaintainer, {"main", "acme/gadgets"}: RoleViewer},
	}
	if se.Role("main", "Acme/Widgets") != RoleMaintainer || !se.CanSee("main", "ACME/widgets") || !se.CanSeeRun("main", "acme/widgets", 7) {
		t.Errorf("visible repository: role %v", se.Role("main", "acme/widgets"))
	}
	if se.Role("main", "acme/payments") != RoleNone || se.CanSee("main", "acme/payments") {
		t.Error("a repository the teams don't cover has a role")
	}
	if se.CanSee("gh", "acme/widgets") || se.CanSeeRun("main", "acme/widgets", 8) {
		t.Error("another instance or another project ID is visible")
	}
	// Covered and switched on, but not stored by reconcile yet (ID 0): the
	// repository is seen, no run of it is.
	if !se.CanSee("main", "acme/gadgets") || se.Role("main", "acme/gadgets") != RoleViewer ||
		se.CanSeeRun("main", "acme/gadgets", 0) || se.CanSeeRun("main", "acme/gadgets", 5) {
		t.Error("an unreconciled repository is not seen, or shows a run")
	}
	op := Session{Admin: true, All: true}
	if op.Role("gh", "acme/anything") != RoleAdmin || !op.CanSee("gh", "acme/anything") || !op.CanSeeRun("gh", "acme/anything", 3) {
		t.Error("an admin doesn't see everything as admin")
	}
	for r, name := range map[Role]string{RoleNone: "", RoleViewer: "viewer", RoleMaintainer: "maintainer", RoleAdmin: "admin"} {
		if r.String() != name {
			t.Errorf("Role(%d).String() = %q, want %q", r, r.String(), name)
		}
	}
}

// requireAdmin lets admins through; everyone else gets 403 and h never runs.
func TestRequireAdmin(t *testing.T) {
	sessions := map[string]Session{
		"admin":   {Admin: true, All: true},
		"user":    {},
		"waiting": {Waiting: true},
	}
	for name, se := range sessions {
		ran := false
		h := requireAdmin(func(w http.ResponseWriter, _ *http.Request) { ran = true; w.WriteHeader(http.StatusOK) })
		req := httptest.NewRequest("GET", "/", nil)
		req = req.WithContext(context.WithValue(req.Context(), sessionKey{}, se))
		rec := httptest.NewRecorder()
		h(rec, req)
		want := name == "admin"
		if ran != want || (rec.Code == http.StatusOK) != want || (!want && rec.Code != http.StatusForbidden) {
			t.Errorf("requireAdmin for %s: ran %v, %d; want ran %v", name, ran, rec.Code, want)
		}
	}
}

// dismissalName never gives an address away: the part of an SSO account's
// email before the "@"; a local admin keeps the form it has.
func TestDismissalName(t *testing.T) {
	for name, tc := range map[string]struct {
		se   Session
		want string
	}{
		"sso":         {Session{Kind: store.AccountSSO, Username: "alice@example.com", Email: "alice@example.com"}, "alice"},
		"sso, plus":   {Session{Kind: store.AccountSSO, Username: "a.b+c@example.com", Email: "a.b+c@example.com"}, "a.b+c"},
		"no email":    {Session{Kind: store.AccountSSO, Username: "x"}, "x"},
		"local admin": {Session{Kind: store.AccountPassword, Username: "root"}, "root (password)"},
	} {
		if got := dismissalName(tc.se); got != tc.want {
			t.Errorf("%s: dismissalName = %q, want %q", name, got, tc.want)
		}
	}
}
