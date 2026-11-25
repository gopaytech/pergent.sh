package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
	e.cat["gh"].set("acme/mine", "acme/theirs", "acme/stale")
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
	t.Run("a stale owner of its own on an off repository", func(t *testing.T) {
		// C's tick on an off repository of B's instance: B may not take it.
		e.team("C", nil, store.TeamRepo{Instance: "gh", Project: "acme/stale", Owner: true})
		rec := e.send(bc, "POST", "/api/repositories", onboardBody("gh", "acme/stale", b))
		if rec.Code != http.StatusForbidden || errorOf(t, rec) != "owned by C" {
			t.Errorf("POST = %d %s; want 403 %q", rec.Code, rec.Body, "owned by C")
		}
		if _, found := e.storedRepo("gh", "acme/stale"); found {
			t.Error("acme/stale was switched on")
		}
		if got := e.storedTeam(b).Repos; !slices.Equal(got, []store.TeamRepo{{Instance: "gh", Owner: true}}) {
			t.Errorf("B's repos = %+v; want the instance entry alone", got)
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

// ownTeams is the two-team setup of the owner-maintainer tests: A owns
// main/acme/gadgets, B covers it, ann maintains A and ben maintains B.
type ownTeams struct {
	a, b     int64
	ann, ben *http.Cookie
}

func ownSetup(e *env) ownTeams {
	ann, ben := e.sso("ann@example.com"), e.sso("ben@example.com")
	e.dashboardRepo("main", "acme/gadgets", nil)
	return ownTeams{
		a: e.team("A", []store.TeamMember{{AccountID: ann, Role: store.RoleMaintainer}},
			store.TeamRepo{Instance: "main", Project: "acme/gadgets", Owner: true}),
		b: e.team("B", []store.TeamMember{{AccountID: ben, Role: store.RoleMaintainer}},
			store.TeamRepo{Instance: "main", Project: "acme/gadgets"}),
		ann: e.sessionFor(ann), ben: e.sessionFor(ben),
	}
}

const gadgets = "/api/admin/repositories/main/acme/gadgets"

// A maintainer of the owner team pauses and resumes with the switch on.
func TestOwnerMaintainerPausesAndResumes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	o := ownSetup(e)
	rec := e.send(o.ann, "PUT", gadgets, enabledBody(false))
	if detail := decode[RepositoryDetailView](t, rec, "pause", http.StatusOK); detail.Repo.On {
		t.Error("the repository is still on after a pause")
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.Enabled {
		t.Error("the repository is not paused")
	}
	e.send(o.ann, "PUT", gadgets, enabledBody(true))
	if r, _ := e.storedRepo("main", "acme/gadgets"); !r.Enabled {
		t.Error("the repository is not resumed")
	}
	if got := e.auditActionsOf("acme/gadgets"); !slices.Equal(got, []string{"repository_paused", "repository_resumed"}) {
		t.Errorf("audit = %v; want the pause and the resume", got)
	}
}

// A maintainer of a team that only covers the repository gets 403 on pause
// and remove, and nothing changes.
func TestCoveringMaintainerCannotPause(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	o := ownSetup(e)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"pause":  e.send(o.ben, "PUT", gadgets, enabledBody(false)),
		"remove": e.send(o.ben, "DELETE", gadgets, ""),
	} {
		if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgNotOwnerMaintainer {
			t.Errorf("%s = %d %s; want 403 %q", name, rec.Code, rec.Body, msgNotOwnerMaintainer)
		}
	}
	if r, found := e.storedRepo("main", "acme/gadgets"); !found || !r.Enabled {
		t.Errorf("the repository = %+v, %v; want it still on", r, found)
	}
}

// With the switch off the owner's maintainer still pauses, but resume and
// remove are 403 and the repository and its owner stay.
func TestSwitchOffLimits(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	o := ownSetup(e)
	wantCode(t, e.send(o.ann, "PUT", gadgets, enabledBody(false)), "pause", http.StatusOK)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"resume": e.send(o.ann, "PUT", gadgets, enabledBody(true)),
		"remove": e.send(o.ann, "DELETE", gadgets, ""),
	} {
		if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgTeamsMayNotOnboard {
			t.Errorf("%s = %d %s; want 403 %q", name, rec.Code, rec.Body, msgTeamsMayNotOnboard)
		}
	}
	if r, found := e.storedRepo("main", "acme/gadgets"); !found || r.Enabled {
		t.Errorf("the repository = %+v, %v; want it kept and paused", r, found)
	}
	if owner, found, err := e.st.OwnerTeam(ctx, "main", "acme/gadgets"); err != nil || !found || owner != o.a {
		t.Errorf("owner = %d, %v, %v; want A", owner, found, err)
	}
}

// A remove by the owner's maintainer deletes every team entry for the
// repository; then any maintainer may onboard it again.
func TestOwnerMaintainerRemoves(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	o := ownSetup(e)
	e.cat["main"].set("acme/gadgets")
	wantCode(t, e.send(o.ann, "DELETE", gadgets, ""), "remove", http.StatusOK)
	if _, found := e.storedRepo("main", "acme/gadgets"); found {
		t.Error("the repository is still stored")
	}
	for name, id := range map[string]int64{"A": o.a, "B": o.b} {
		if got := e.storedTeam(id).Repos; len(got) != 0 {
			t.Errorf("%s's repos = %+v; want none", name, got)
		}
	}
	rec := e.send(o.ben, "POST", "/api/repositories", onboardBody("main", "acme/gadgets", o.b))
	wantCode(t, rec, "onboard by B", http.StatusCreated)
}

// A maintainer of the instance's owner team pauses a repository of it.
func TestInstanceOwnerMaintainerPauses(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	bob := e.sso("bob@example.com")
	e.team("B", []store.TeamMember{{AccountID: bob, Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "gh", Owner: true})
	e.dashboardRepo("gh", "acme/mine", nil)
	wantCode(t, e.send(e.sessionFor(bob), "PUT", "/api/admin/repositories/gh/acme/mine", enabledBody(false)), "pause", http.StatusOK)
}

// A maintainer's own stale owner tick on an off repository of an instance
// another team owns: the chosen team's tick settles it, so the onboard is
// 201 and the tick stays (it was 403 "owned by" the instance's owner).
func TestMaintainerOnboardOwnTickBeatsInstanceOwner(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	a := e.team("A", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "gh", Project: "acme/back", Owner: true})
	e.team("X", nil, store.TeamRepo{Instance: "gh", Owner: true})
	e.cat["gh"].set("acme/back")
	rec := e.send(e.aliceSession(), "POST", "/api/repositories", onboardBody("gh", "acme/back", a))
	wantCode(t, rec, "onboard", http.StatusCreated)
	if _, found := e.storedRepo("gh", "acme/back"); !found {
		t.Error("acme/back was not switched on")
	}
	if got := e.storedTeam(a).Repos; !slices.Equal(got, []store.TeamRepo{{Instance: "gh", Project: "acme/back", Owner: true}}) {
		t.Errorf("A's repos = %+v; want its own tick kept", got)
	}
}

func claimBody(team int64) string { return fmt.Sprintf(`{"team":%d}`, team) }

const (
	claimPath = "/api/repositories/main/acme/gadgets/claim"
	relPath   = "/api/repositories/main/acme/gadgets/release"
)

// claimSetup is a repository switched on with no owner and alice maintaining
// A (not covering it yet).
func claimSetup(e *env) (a int64) {
	e.dashboardRepo("main", "acme/gadgets", nil)
	return e.team("A", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleMaintainer}})
}

// Claim: the rules in their order. One env, one case per rule.
func TestClaimUnowned(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	a := claimSetup(e)
	rec := e.send(e.aliceSession(), "POST", claimPath, claimBody(a))
	detail := decode[RepositoryDetailView](t, rec, "claim", http.StatusOK)
	if detail.OwnerTeam == nil || detail.OwnerTeam.ID != a {
		t.Errorf("OwnerTeam = %+v; want A", detail.OwnerTeam)
	}
	if got := e.auditActionsOf("acme/gadgets"); !slices.Equal(got, []string{"repository_claimed", "owner_set"}) {
		t.Errorf("audit = %v; want repository_claimed then owner_set", got)
	}
	if owner, found, err := e.st.OwnerTeam(ctx, "main", "acme/gadgets"); err != nil || !found || owner != a {
		t.Errorf("owner = %d, %v, %v; want A", owner, found, err)
	}
}

func TestClaimPausedStaysPaused(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	a := e.team("A", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleMaintainer}})
	e.pausedRepo("main", "acme/gadgets", nil)
	wantCode(t, e.send(e.aliceSession(), "POST", claimPath, claimBody(a)), "claim", http.StatusOK)
	if r, found := e.storedRepo("main", "acme/gadgets"); !found || r.Enabled {
		t.Errorf("the repository = %+v, %v; want it kept and paused", r, found)
	}
	if owner, found, _ := e.st.OwnerTeam(ctx, "main", "acme/gadgets"); !found || owner != a {
		t.Errorf("owner = %d, %v; want A", owner, found)
	}
}

// A taken repository is 409, at its own entry or the instance's.
func TestClaimTaken(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	a := claimSetup(e)
	e.team("B", nil, store.TeamRepo{Instance: "main", Project: "acme/gadgets", Owner: true})
	rec := e.send(e.aliceSession(), "POST", claimPath, claimBody(a))
	if want := "acme/gadgets already has owner B"; rec.Code != http.StatusConflict || errorOf(t, rec) != want {
		t.Errorf("claim = %d %s; want 409 %q", rec.Code, rec.Body, want)
	}
	if got := e.storedTeam(a).Repos; len(got) != 0 {
		t.Errorf("A's repos = %+v; want none", got)
	}
}

func TestClaimInstanceOwned(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	a := claimSetup(e)
	e.team("B", nil, store.TeamRepo{Instance: "main", Owner: true})
	rec := e.send(e.aliceSession(), "POST", claimPath, claimBody(a))
	if want := "main (whole instance) already has owner B"; rec.Code != http.StatusConflict || errorOf(t, rec) != want {
		t.Errorf("claim = %d %s; want 409 %q", rec.Code, rec.Body, want)
	}
}

// Not switched on (no row, not in config) is 404; a team the maintainer
// does not maintain is 403.
func TestClaimNotOn(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	a := e.team("A", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleMaintainer}})
	other := e.team("O", nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	wantCode(t, e.send(e.aliceSession(), "POST", "/api/repositories/main/acme/none/claim", claimBody(a)), "not on", http.StatusNotFound)
	rec := e.send(e.aliceSession(), "POST", claimPath, claimBody(other))
	if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgNotOwnerMaintainer {
		t.Errorf("a team they don't maintain = %d %s; want 403 %q", rec.Code, rec.Body, msgNotOwnerMaintainer)
	}
}

func TestClaimSwitchOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	a := claimSetup(e)
	rec := e.send(e.aliceSession(), "POST", claimPath, claimBody(a))
	if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgTeamsMayNotOnboard {
		t.Errorf("claim = %d %s; want 403 %q", rec.Code, rec.Body, msgTeamsMayNotOnboard)
	}
	if owner, found, _ := e.st.OwnerTeam(ctx, "main", "acme/gadgets"); found {
		t.Errorf("owner = %d; want none", owner)
	}
}

// A repository of the config file: 409 for a maintainer, an admin may claim.
func TestClaimConfigRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	a := e.team("A", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleMaintainer}})
	const path = "/api/repositories/main/acme/widgets/claim"
	rec := e.send(e.aliceSession(), "POST", path, claimBody(a))
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgConfigManaged {
		t.Errorf("maintainer = %d %s; want 409 %q", rec.Code, rec.Body, msgConfigManaged)
	}
	admin := e.sessionFor(e.ssoAdmin("root@example.com"))
	wantCode(t, e.send(admin, "POST", path, claimBody(a)), "admin", http.StatusOK)
	if owner, found, _ := e.st.OwnerTeam(ctx, "main", "acme/widgets"); !found || owner != a {
		t.Errorf("owner = %d, %v; want A", owner, found)
	}
}

// The owner's maintainer releases with the switch off: the tick goes, the
// entry stays, the repository stays.
func TestReleaseByOwnerMaintainer(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	o := ownSetup(e)
	rec := e.send(o.ann, "POST", relPath, `{}`)
	detail := decode[RepositoryDetailView](t, rec, "release", http.StatusOK)
	if detail.OwnerTeam != nil {
		t.Errorf("OwnerTeam = %+v; want none", detail.OwnerTeam)
	}
	if got := e.storedTeam(o.a).Repos; !slices.Equal(got, []store.TeamRepo{{Instance: "main", Project: "acme/gadgets"}}) {
		t.Errorf("A's repos = %+v; want its entry without the tick", got)
	}
	if got := e.auditActionsOf("acme/gadgets"); !slices.Equal(got, []string{"repository_released", "owner_cleared"}) {
		t.Errorf("audit = %v; want repository_released then owner_cleared", got)
	}
	if _, found := e.storedRepo("main", "acme/gadgets"); !found {
		t.Error("the repository was removed")
	}
	// B only covered it: it is nobody's now, and a covering maintainer may not release.
	if rec := e.send(o.ben, "POST", relPath, `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("B after = %d %s; want 403", rec.Code, rec.Body)
	}
}

// An instance-level owner is not released here: its maintainer is 403, an
// admin is 409 (no owner of its own).
func TestReleaseInstanceOwnerRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t, teamsOnboard)
	bob := e.sso("bob@example.com")
	b := e.team("B", []store.TeamMember{{AccountID: bob, Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "gh", Owner: true})
	e.dashboardRepo("gh", "acme/mine", nil)
	const path = "/api/repositories/gh/acme/mine/release"
	rec := e.send(e.sessionFor(bob), "POST", path, `{}`)
	if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgNotOwnerMaintainer {
		t.Errorf("maintainer = %d %s; want 403 %q", rec.Code, rec.Body, msgNotOwnerMaintainer)
	}
	admin := e.sessionFor(e.ssoAdmin("root@example.com"))
	rec = e.send(admin, "POST", path, `{}`)
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgNoOwnOwner {
		t.Errorf("admin = %d %s; want 409 %q", rec.Code, rec.Body, msgNoOwnOwner)
	}
	if got := e.storedTeam(b).Repos; !slices.Equal(got, []store.TeamRepo{{Instance: "gh", Owner: true}}) {
		t.Errorf("B's repos = %+v; want the instance entry kept", got)
	}
}
