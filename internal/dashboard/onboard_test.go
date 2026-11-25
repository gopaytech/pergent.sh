package dashboard

import (
	"encoding/json"
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

// tickOn gives team id the owner tick on a new own entry for project.
func (e *env) tickOn(id int64, instance, project string) {
	e.t.Helper()
	e.editTeam(id, func(t *store.Team) {
		t.Repos = append(t.Repos, store.TeamRepo{Instance: instance, Project: project, Owner: true})
	})
}

// actionsEnv is the setup of the flag test: A owns main/acme/gadgets (and the
// config repository main/acme/widgets), B covers the instance, V only views A
// and bob maintains the team that owns all of gh.
type actionsEnv struct {
	*env
	admin, ann, ben, viewer, bob *http.Cookie
	a                            int64
}

func newActionsEnv(t *testing.T, cfg func(*config.Config)) actionsEnv {
	e := newEnv(t, cfg)
	ann, ben, vic, bob := e.sso("ann@example.com"), e.sso("ben@example.com"), e.sso("vic@example.com"), e.sso("bob@example.com")
	a := e.team("A", []store.TeamMember{{AccountID: ann, Role: store.RoleMaintainer}, {AccountID: vic, Role: store.RoleViewer}})
	e.team("B", []store.TeamMember{{AccountID: ben, Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "main"})
	e.team("X", []store.TeamMember{{AccountID: bob, Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "gh", Owner: true})
	e.tickOn(a, "main", "acme/widgets")
	return actionsEnv{env: e, a: a, admin: e.sessionFor(e.ssoAdmin("root@example.com")),
		ann: e.sessionFor(ann), ben: e.sessionFor(ben), viewer: e.sessionFor(vic), bob: e.sessionFor(bob)}
}

// Each flag is true exactly when its route would succeed: a table over the
// sessions, the repository's state and the switch, on one env per switch.
func TestRepoActionsFlags(t *testing.T) {
	t.Parallel()
	type flags = RepoActionsView
	all := flags{true, false, true, true}
	allPaused := flags{false, true, true, true}
	none := flags{}
	for _, c := range []struct {
		name string
		cfg  func(*config.Config)
		// want per session on a repository that is on, then paused.
		admin, ann, ben, viewer [2]flags
	}{
		{"switch on", teamsOnboard, [2]flags{all, allPaused}, [2]flags{all, allPaused}, [2]flags{none, none}, [2]flags{none, none}},
		{"switch off", nil, [2]flags{all, allPaused},
			[2]flags{{true, false, false, true}, {false, false, false, true}}, [2]flags{none, none}, [2]flags{none, none}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newActionsEnv(t, c.cfg)
			e.tickOn(e.a, "main", "acme/gadgets")
			e.dashboardRepo("main", "acme/gadgets", nil)
			get := func(who *http.Cookie, path string) flags {
				return getJSON[RepositoryDetailView](e.env, who, path).Actions
			}
			const gad = "/api/repositories/main/acme/gadgets"
			for i, state := range []string{"on", "paused"} {
				if i == 1 {
					e.pausedRepo("main", "acme/gadgets", nil)
				}
				for who, row := range map[string]struct {
					cookie *http.Cookie
					want   flags
				}{
					"admin": {e.admin, c.admin[i]}, "A maintainer": {e.ann, c.ann[i]},
					"B maintainer": {e.ben, c.ben[i]}, "A viewer": {e.viewer, c.viewer[i]},
				} {
					if got := get(row.cookie, gad); got != row.want {
						t.Errorf("%s, %s: actions = %+v; want %+v", who, state, got, row.want)
					}
				}
			}
			// The config file's repository has no button for anyone, release included.
			for who, cookie := range map[string]*http.Cookie{"admin": e.admin, "A maintainer": e.ann} {
				if got := get(cookie, "/api/repositories/main/acme/widgets"); got != none {
					t.Errorf("%s on a config repository: actions = %+v; want none", who, got)
				}
			}
			// An instance-level owner: its maintainer pauses (and removes with the switch on), but nobody releases it here.
			e.dashboardRepo("gh", "acme/mine", nil)
			wantBob := flags{Pause: true, Remove: c.cfg != nil}
			if got := get(e.bob, "/api/repositories/gh/acme/mine"); got != wantBob {
				t.Errorf("instance owner's maintainer: actions = %+v; want %+v", got, wantBob)
			}
			if got := get(e.admin, "/api/repositories/gh/acme/mine"); got.Release {
				t.Errorf("admin on an instance-owned repository: release = true; want false")
			}
		})
	}
}

// /api/me names the instances a session may onboard on.
func TestMeOnboardInstances(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(cfg *config.Config) { cfg.GitLab[0].TeamsMayOnboard = true })
	ann, vic := e.sso("ann@example.com"), e.sso("vic@example.com")
	e.team("A", []store.TeamMember{{AccountID: ann, Role: store.RoleMaintainer}, {AccountID: vic, Role: store.RoleViewer}})
	for _, c := range []struct {
		name   string
		cookie *http.Cookie
		want   []string
	}{
		{"maintainer, switch on for main only", e.sessionFor(ann), []string{"main"}},
		{"viewer", e.sessionFor(vic), []string{}},
		{"admin, every instance", e.sessionFor(e.ssoAdmin("root@example.com")), []string{"gh", "main"}},
		{"waiting", e.sessionFor(e.sso("new@example.com")), []string{}},
	} {
		rec := e.get(c.cookie, "/api/me")
		var got struct {
			OnboardInstances []string `json:"onboardInstances"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%s: /api/me = %d %s: %v", c.name, rec.Code, rec.Body, err)
		}
		if got.OnboardInstances == nil || !slices.Equal(got.OnboardInstances, c.want) {
			t.Errorf("%s: onboardInstances = %#v; want %#v", c.name, got.OnboardInstances, c.want)
		}
	}
}

// TestRoleMatrix pins every action, for every kind of session, with the
// switch on and off, to the spec's rules table. It asserts the status code
// only. Each cell has a repository of its own, so no cell sees another's write.
func TestRoleMatrix(t *testing.T) {
	t.Parallel()
	const (
		admin = iota
		owner
		other
		viewer
		none
	)
	type cell struct {
		method, path, body string
	}
	for _, sw := range []struct {
		name string
		cfg  func(*config.Config)
		on   bool
	}{{"switch on", teamsOnboard, true}, {"switch off", nil, false}} {
		t.Run(sw.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, sw.cfg)
			ann, ben, vic, nob := e.sso("ann@example.com"), e.sso("ben@example.com"), e.sso("vic@example.com"), e.sso("nobody@example.com")
			a := e.team("A", []store.TeamMember{{AccountID: ann, Role: store.RoleMaintainer}})
			b := e.team("B", []store.TeamMember{{AccountID: ben, Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "main"})
			v := e.team("V", []store.TeamMember{{AccountID: vic, Role: store.RoleViewer}}, store.TeamRepo{Instance: "main"})
			who := [5]*http.Cookie{admin: e.sessionFor(e.ssoAdmin("root@example.com")), owner: e.sessionFor(ann),
				other: e.sessionFor(ben), viewer: e.sessionFor(vic), none: e.sessionFor(nob)}
			ownTeam := [5]int64{admin: a, owner: a, other: b, viewer: v, none: a}
			n := 0
			next := func() string { n++; return fmt.Sprintf("acme/cell%d", n) }
			// A repository of A, with its tick, on or paused.
			ownedRepo := func(paused bool) string {
				p := next()
				e.tickOn(a, "main", p)
				if paused {
					e.pausedRepo("main", p, nil)
				} else {
					e.dashboardRepo("main", p, nil)
				}
				return p
			}
			for _, c := range []struct {
				action string
				// cell builds the request of session i; want[i] is the status
				// with the switch on, wantOff[i] with it off.
				cell    func(i int) cell
				want    [5]int
				wantOff [5]int
			}{
				{"picker", func(int) cell { return cell{"GET", "/api/repositories/available?instance=main", ""} },
					[5]int{200, 200, 200, 404, 403}, [5]int{200, 403, 403, 404, 403}},
				{"onboard", func(i int) cell {
					p := next()
					e.cat["main"].set(p)
					return cell{"POST", "/api/repositories", onboardBody("main", p, ownTeam[i])}
				}, [5]int{201, 201, 201, 404, 403}, [5]int{201, 403, 403, 404, 403}},
				{"pause", func(int) cell {
					return cell{"PUT", "/api/admin/repositories/main/" + ownedRepo(false), enabledBody(false)}
				}, [5]int{200, 200, 403, 404, 403}, [5]int{200, 200, 403, 404, 403}},
				{"resume", func(int) cell {
					return cell{"PUT", "/api/admin/repositories/main/" + ownedRepo(true), enabledBody(true)}
				}, [5]int{200, 200, 403, 404, 403}, [5]int{200, 403, 403, 404, 403}},
				{"remove", func(int) cell {
					return cell{"DELETE", "/api/repositories/main/" + ownedRepo(false), ""}
				}, [5]int{200, 200, 403, 404, 403}, [5]int{200, 403, 403, 404, 403}},
				{"claim", func(i int) cell {
					p := next()
					e.dashboardRepo("main", p, nil)
					return cell{"POST", "/api/repositories/main/" + p + "/claim", claimBody(ownTeam[i])}
				}, [5]int{200, 200, 200, 404, 403}, [5]int{200, 403, 403, 404, 403}},
				{"release", func(int) cell {
					return cell{"POST", "/api/repositories/main/" + ownedRepo(false) + "/release", `{}`}
				}, [5]int{200, 200, 403, 404, 403}, [5]int{200, 200, 403, 404, 403}},
			} {
				want := c.want
				if !sw.on {
					want = c.wantOff
				}
				for i, name := range []string{"admin", "owner maintainer", "other-team maintainer", "viewer", "no team"} {
					req := c.cell(i)
					if rec := e.send(who[i], req.method, req.path, req.body); rec.Code != want[i] {
						t.Errorf("%s as %s = %d %s; want %d", c.action, name, rec.Code, rec.Body, want[i])
					}
				}
			}

			t.Run("picker lists names and state only", func(t *testing.T) {
				e.cat["main"].set("acme/listed")
				var page struct {
					Items []map[string]any `json:"items"`
				}
				rec := e.get(who[owner], "/api/repositories/available?instance=main&search=listed")
				if !sw.on {
					wantCode(t, rec, "picker", http.StatusForbidden)
					return
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) == 0 {
					t.Fatalf("picker = %d %s: %v", rec.Code, rec.Body, err)
				}
				for _, item := range page.Items {
					keys := make([]string, 0, len(item))
					for k := range item {
						keys = append(keys, k)
					}
					slices.Sort(keys)
					if want := []string{"instance", "ownerTeam", "platform", "project", "state"}; !slices.Equal(keys, want) {
						t.Errorf("picker item keys = %v; want %v", keys, want)
					}
				}
			})
			t.Run("a member who only views the named team cannot claim for it", func(t *testing.T) {
				e.editTeam(v, func(tm *store.Team) {
					tm.Members = append(tm.Members, store.TeamMember{AccountID: ben, Role: store.RoleViewer})
				})
				p := next()
				e.dashboardRepo("main", p, nil)
				rec := e.send(who[other], "POST", "/api/repositories/main/"+p+"/claim", claimBody(v))
				want := http.StatusForbidden
				if rec.Code != want {
					t.Errorf("claim for a team they only view = %d %s; want %d", rec.Code, rec.Body, want)
				}
			})
			t.Run("an admin claim naming an unknown team is 400", func(t *testing.T) {
				p := next()
				e.dashboardRepo("main", p, nil)
				rec := e.send(who[admin], "POST", "/api/repositories/main/"+p+"/claim", claimBody(9999))
				if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgNoTeam {
					t.Errorf("claim = %d %s; want 400 %q", rec.Code, rec.Body, msgNoTeam)
				}
			})
			t.Run("release on a config repository is 409", func(t *testing.T) {
				e.tickOn(a, "main", "acme/widgets")
				for _, i := range []int{admin, owner} {
					rec := e.send(who[i], "POST", "/api/repositories/main/acme/widgets/release", `{}`)
					if rec.Code != http.StatusConflict || errorOf(t, rec) != msgConfigManaged {
						t.Errorf("release = %d %s; want 409 %q", rec.Code, rec.Body, msgConfigManaged)
					}
				}
			})
			t.Run("a maintainer on an instance being removed is 404", func(t *testing.T) {
				e.addLab(e.operator())
				e.reg.SetTeamsMayOnboard("lab", true)
				e.dashboardRepo("lab", "acme/x", nil)
				e.tickOn(a, "lab", "acme/x")
				e.reg.SetRemoving("lab")
				for name, req := range map[string]cell{
					"pause":   {"PUT", "/api/admin/repositories/lab/acme/x", enabledBody(false)},
					"release": {"POST", "/api/repositories/lab/acme/x/release", `{}`},
					"claim":   {"POST", "/api/repositories/lab/acme/x/claim", claimBody(a)},
				} {
					wantCode(t, e.send(who[owner], req.method, req.path, req.body), name, http.StatusNotFound)
				}
			})
		})
	}
}
