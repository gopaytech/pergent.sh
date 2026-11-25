package dashboard

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// ownerMaintainer: ok only for a maintainer of the team that owns the
// repository, not of a team that merely covers it.
func TestOwnerMaintainer(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	owner := e.sso("owner@example.com")
	cover := e.sso("cover@example.com")
	inst := e.sso("instance@example.com")
	e.team("owners", []store.TeamMember{{AccountID: owner, Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "main", Project: "acme/widgets", Owner: true})
	e.team("covers", []store.TeamMember{{AccountID: cover, Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	e.team("instance owners", []store.TeamMember{{AccountID: inst, Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "gh", Owner: true})

	for _, c := range []struct {
		name              string
		account           int64
		instance, project string
		ok                bool
	}{
		{"maintainer of the owner team", owner, "main", "acme/widgets", true},
		{"maintainer of a covering team", cover, "main", "acme/widgets", false},
		{"maintainer of an instance-level owner", inst, "gh", "acme/widgets", true},
		{"repository with no owner", owner, "main", "acme/payments", false},
	} {
		got, ok, err := e.d.ownerMaintainer(ctx, Session{AccountID: c.account}, c.instance, c.project)
		if err != nil || ok != c.ok {
			t.Errorf("%s: ownerMaintainer = %d, %v, %v; want ok %v", c.name, got, ok, err, c.ok)
		}
	}
}

// onboardBody is the body of an onboard by team.
func onboardBody(instance, project string, team int64) string {
	return fmt.Sprintf(`{"instance":%q,"project":%q,"ownerTeam":%d}`, instance, project, team)
}

// teamsOnboard switches the "teams may onboard" key on for main and gh.
func teamsOnboard(cfg *config.Config) {
	cfg.GitLab[0].TeamsMayOnboard = true
	cfg.GitHub[0].TeamsMayOnboard = true
}

// A maintainer onboards a repository for a team they maintain, in the rules'
// order, and nothing is written before every check passes. One env: alice
// maintains A and only views V, bob maintains B, which owns all of gh.
func TestMaintainerOnboard(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	alice, bob := e.alice(), e.sso("bob@example.com")
	a := e.team("A", []store.TeamMember{{AccountID: alice, Role: store.RoleMaintainer}})
	v := e.team("V", []store.TeamMember{{AccountID: alice, Role: store.RoleViewer}})
	b := e.team("B", []store.TeamMember{{AccountID: bob, Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "gh", Owner: true})
	e.cat["main"].set("acme/new", "acme/dash", "acme/paused", "acme/plain", "acme/widgets")
	e.cat["gh"].set("acme/mine", "acme/theirs")
	e.dashboardRepo("main", "acme/dash", nil)
	e.pausedRepo("main", "acme/paused", nil)
	ac, bc := e.aliceSession(), e.sessionFor(bob)

	t.Run("a maintainer of the owner team", func(t *testing.T) {
		rec := e.send(ac, "POST", "/api/repositories", onboardBody("main", "acme/new", a))
		detail := decode[RepositoryDetailView](t, rec, "POST", http.StatusCreated)
		if detail.OwnerTeam == nil || detail.OwnerTeam.ID != a || detail.OwnerTeam.Name != "A" {
			t.Errorf("ownerTeam = %+v; want A", detail.OwnerTeam)
		}
		if got := e.storedTeam(a).Repos; !slices.Equal(got, []store.TeamRepo{{Instance: "main", Project: "acme/new", Owner: true}}) {
			t.Errorf("A's repos = %+v; want acme/new with the owner tick", got)
		}
		if got := e.auditActionsOf("acme/new"); !slices.Equal(got, []string{"repository_on", "team_updated", "owner_set"}) {
			t.Errorf("audit = %v; want repository_on and the owner change", got)
		}
	})
	t.Run("needs a team they maintain", func(t *testing.T) {
		rec := e.send(ac, "POST", "/api/repositories", `{"instance":"main","project":"acme/plain"}`)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgNoTeam {
			t.Errorf("no ownerTeam = %d %s; want 400 %q", rec.Code, rec.Body, msgNoTeam)
		}
		for name, id := range map[string]int64{"a viewer of it": v, "a team they are not on": b, "no such team": 9999} {
			rec := e.send(ac, "POST", "/api/repositories", onboardBody("main", "acme/plain", id))
			if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgNotOwnerMaintainer {
				t.Errorf("%s = %d %s; want 403 %q", name, rec.Code, rec.Body, msgNotOwnerMaintainer)
			}
		}
		if _, found := e.storedRepo("main", "acme/plain"); found {
			t.Error("acme/plain was switched on")
		}
	})
	t.Run("on or paused is for Claim", func(t *testing.T) {
		for _, p := range []string{"acme/dash", "acme/paused"} {
			rec := e.send(ac, "POST", "/api/repositories", onboardBody("main", p, a))
			if rec.Code != http.StatusConflict || errorOf(t, rec) != msgUseClaim {
				t.Errorf("%s = %d %s; want 409 %q", p, rec.Code, rec.Body, msgUseClaim)
			}
		}
		if r, _ := e.storedRepo("main", "acme/paused"); r.Enabled {
			t.Error("the paused repository was resumed")
		}
		if got := e.auditActionsOf("acme/paused"); len(got) != 0 {
			t.Errorf("audit = %v; want nothing", got)
		}
	})
	t.Run("a config repository", func(t *testing.T) {
		rec := e.send(ac, "POST", "/api/repositories", onboardBody("main", "acme/widgets", a))
		if rec.Code != http.StatusConflict || errorOf(t, rec) != msgConfigManaged {
			t.Errorf("POST = %d %s; want 409 %q", rec.Code, rec.Body, msgConfigManaged)
		}
	})
	t.Run("an instance owned by another team", func(t *testing.T) {
		rec := e.send(ac, "POST", "/api/repositories", onboardBody("gh", "acme/theirs", a))
		if rec.Code != http.StatusForbidden || errorOf(t, rec) != "owned by B" {
			t.Errorf("POST = %d %s; want 403 %q", rec.Code, rec.Body, "owned by B")
		}
		if _, found := e.storedRepo("gh", "acme/theirs"); found {
			t.Error("acme/theirs was switched on")
		}
	})
	t.Run("the instance's owner inherits", func(t *testing.T) {
		rec := e.send(bc, "POST", "/api/repositories", onboardBody("gh", "acme/mine", b))
		detail := decode[RepositoryDetailView](t, rec, "POST", http.StatusCreated)
		if detail.OwnerTeam == nil || detail.OwnerTeam.ID != b {
			t.Errorf("ownerTeam = %+v; want B", detail.OwnerTeam)
		}
		if got := e.storedTeam(b).Repos; !slices.Equal(got, []store.TeamRepo{{Instance: "gh", Owner: true}}) {
			t.Errorf("B's repos = %+v; want the instance entry alone, no tick of its own", got)
		}
		if got := e.auditActionsOf("acme/mine"); !slices.Equal(got, []string{"repository_on"}) {
			t.Errorf("audit = %v; want repository_on alone", got)
		}
	})
	t.Run("a viewer", func(t *testing.T) {
		viewer := e.sessionFor(e.sso("viewer@example.com"))
		e.team("W", viewerMembers(e.sso("viewer@example.com")))
		rec := e.send(viewer, "POST", "/api/repositories", onboardBody("main", "acme/plain", a))
		wantCode(t, rec, "a viewer onboarding", http.StatusNotFound)
	})
}

// With the switch off a maintainer is 403 and nothing is stored.
func TestMaintainerOnboardSwitchOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	a := e.team("A", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleMaintainer}})
	e.cat["main"].set("acme/new")
	rec := e.send(e.aliceSession(), "POST", "/api/repositories", onboardBody("main", "acme/new", a))
	if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgTeamsMayNotOnboard {
		t.Errorf("POST = %d %s; want 403 %q", rec.Code, rec.Body, msgTeamsMayNotOnboard)
	}
	if _, found := e.storedRepo("main", "acme/new"); found {
		t.Error("a repository was stored with the switch off")
	}
}
