package dashboard

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// pausedRepo stores a dashboard repository, as dashboardRepo does, and
// pauses it.
func (e *env) pausedRepo(instance, project string, edit func(*store.DashboardRepo)) {
	e.t.Helper()
	e.dashboardRepo(instance, project, edit)
	if found, _, err := e.st.SetDashboardRepoEnabled(ctx, instance, project, false); err != nil || !found {
		e.t.Fatalf("pause %s: %v, %v", project, found, err)
	}
}

// A paused repository is listed with health "paused", even with a setup
// problem left from before, and stays out of the overview's Needs attention.
func TestPausedRepoHealthAndOverview(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.pausedRepo("main", "acme/tools", nil)
	for _, p := range []string{"acme/gadgets", "acme/tools"} {
		if err := e.st.SetAttention(ctx, "main", p, "out_of_memory", p+" ran out of memory"); err != nil {
			t.Fatal(err)
		}
	}
	c := e.userOnMain(true)

	byPath := map[string]RepositoryView{}
	for _, r := range getJSON[[]RepositoryView](e, c, "/api/repositories") {
		byPath[r.Instance+"/"+r.Project] = r
	}
	if r := byPath["main/acme/tools"]; r.On || !r.Paused || r.Health != healthPaused || r.Source != "dashboard" {
		t.Errorf("paused repository = %+v; want off, paused, health paused", r)
	}
	if r := byPath["main/acme/gadgets"]; !r.On || r.Paused || r.Health != healthNeedsAttention {
		t.Errorf("running repository = %+v; want on with health attention", r)
	}
	detail := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/tools")
	if detail.Repo.On || !detail.Repo.Paused || detail.Repo.Health != healthPaused {
		t.Errorf("paused detail = %+v", detail.Repo)
	}
}

// Whoever could see a repository while it ran still sees it, and its
// reviews, while it is paused. Others still get 404.
func TestPausedRepoStaysVisible(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.pausedRepo("main", "acme/tools", nil)
	e.addRun(runFix{project: "acme/tools", number: 4, head: head('4'), title: "Tool change"})
	c := e.userOnMain(false, "acme/tools")

	repos := getJSON[[]RepositoryView](e, c, "/api/repositories")
	if len(repos) != 1 || repos[0].Project != "acme/tools" || !repos[0].Paused {
		t.Fatalf("a viewer's repositories = %+v; want acme/tools, paused", repos)
	}
	detail := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/tools")
	if !detail.Repo.Paused || detail.Role != "viewer" {
		t.Errorf("a viewer's detail = %+v, role %s", detail.Repo, detail.Role)
	}
	e.wantStatus(c, "/api/repositories/main/acme/tools/readiness", http.StatusOK)
	e.wantStatus(c, "/api/repositories/main/acme/tools/skill-results", http.StatusOK)
	e.wantStatus(c, "/api/reviews/main/acme/tools/4", http.StatusOK)
	list := getJSON[ReviewListView](e, c, "/api/reviews?project=acme/tools")
	if len(list.Items) != 1 {
		t.Errorf("a viewer's reviews of the paused repository = %+v; want the one", list.Items)
	}

	other := e.sessionFor(e.sso("bob@example.com"))
	e.team("other", []store.TeamMember{{AccountID: e.accountOf(other), Role: store.RoleViewer}}, store.TeamRepo{Instance: "main", Project: "acme/gadgets"})
	e.wantStatus(other, "/api/repositories/main/acme/tools", http.StatusNotFound)
}

// The picker lists a paused repository with the state paused.
func TestPickerShowsPausedRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.cat["main"].set("acme/tools", "acme/new")
	e.pausedRepo("main", "acme/tools", nil)
	got := getJSON[AvailableView](e, e.userOnMain(true), "/api/repositories/available?instance=main")
	if len(got.Items) != 2 || got.Items[0].Project != "acme/new" || got.Items[0].State != stateOff ||
		got.Items[1].Project != "acme/tools" || got.Items[1].State != statePaused {
		t.Errorf("available = %+v; want acme/new (off) and acme/tools (paused)", got.Items)
	}
}

// Settings of a paused repository save, keep it paused, and set nothing up.
func TestSettingsEditOnPausedRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.pausedRepo("main", "acme/tools", nil)
	c := e.userOnMain(true)

	rec := e.send(c, "PUT", "/api/repositories/main/acme/tools/settings", `{"approve": false}`)
	wantCode(t, rec, "PUT settings", http.StatusOK)
	detail := decode[RepositoryDetailView](t, rec, "PUT settings", http.StatusOK)
	if !detail.Repo.Paused || detail.Settings["approve"].Value != false {
		t.Errorf("detail after the save = %+v %+v", detail.Repo, detail.Settings["approve"])
	}
	r, found := e.storedRepo("main", "acme/tools")
	if !found || r.Enabled || r.Approve == nil || *r.Approve {
		t.Errorf("row = %+v, %v; want paused with approve off", r, found)
	}
	wantCode(t, e.send(c, "POST", "/api/repositories/main/acme/tools/settings/reset", ""), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/tools"); r.Enabled || r.Approve != nil {
		t.Errorf("row after reset = %+v; want paused with approve back to the default", r)
	}
	if n := e.reconciles.Load(); n != 0 {
		t.Errorf("a settings edit of a paused repository asked for %d reconciles", n)
	}
	if n := e.wakes.Load(); n != 0 {
		t.Errorf("a settings edit of a paused repository woke the engine %d times", n)
	}
}

// A team may cover a paused repository.
func TestTeamMayCoverPausedRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.pausedRepo("main", "acme/tools", nil)
	c := e.userOnMain(true)
	id := e.team("devs", nil)
	rec := e.send(c, "PUT", "/api/teams/"+strconv.FormatInt(id, 10),
		`{"name":"devs","members":[],"repos":[{"instance":"main","project":"ACME/Tools"}]}`)
	wantCode(t, rec, "PUT team", http.StatusOK)
	if tm := e.storedTeam(id); len(tm.Repos) != 1 || tm.Repos[0].Project != "acme/tools" {
		t.Errorf("team repos = %+v; want acme/tools as stored", tm.Repos)
	}
}

// Usage tells a running, a paused and a removed repository apart.
func TestUsageMarksPausedRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.usageFixture() // acme/widgets (on), acme/old (removed)
	e.pausedRepo("main", "acme/tools", nil)
	e.usageRun(runFix{project: "acme/tools", number: 9, head: head('9'), at: e.clock.now().Add(-time.Hour), model: "m1"},
		usageSkill("security", "no_blocking_issues", 10, 5))
	got := getJSON[UsageOverviewView](e, e.operator(), "/api/admin/usage")
	want := map[string][2]bool{"acme/widgets": {true, false}, "acme/old": {false, false}, "acme/tools": {false, true}}
	if len(got.Repositories) != len(want) {
		t.Fatalf("usage rows = %+v", got.Repositories)
	}
	for _, r := range got.Repositories {
		if w := want[r.Project]; r.SwitchedOn != w[0] || r.Paused != w[1] {
			t.Errorf("%s: switchedOn %v paused %v; want %v %v", r.Project, r.SwitchedOn, r.Paused, w[0], w[1])
		}
	}
}
