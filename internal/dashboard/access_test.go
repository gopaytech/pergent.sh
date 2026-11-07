package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// repoNames lists a session's repositories as instance/project.
func repoNames(e *env, c *http.Cookie) []string {
	e.t.Helper()
	var got []string
	for _, r := range getJSON[[]RepositoryView](e, c, "/api/repositories") {
		got = append(got, r.Instance+"/"+r.Project)
	}
	return got
}

// wantWaiting checks rec is the 403 of an account on no team.
func wantWaiting(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != `{"code":"waiting","error":"an admin needs to add you to a team"}` {
		t.Errorf("%s = %d %s; want 403 waiting", what, rec.Code, rec.Body)
	}
}

// An admin is an account whose role is admin, with no team: it sees every
// repository of every instance as admin, and every admin page.
func TestAdminSeesEverything(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	e.ssoAdmin(aliceEmail)
	c := e.aliceSession()

	code, me := e.me(c)
	if code != 200 || !me.Admin || me.Role != store.RoleAdmin || me.Kind != store.AccountSSO || me.Email != aliceEmail ||
		me.Username != aliceEmail || me.SignIn != store.SignInOIDC || me.Waiting || me.MustChangePassword {
		t.Fatalf("/api/me = %d %+v; want an SSO admin", code, me)
	}
	if got := repoNames(e, c); !slices.Equal(got, []string{"main/acme/widgets", "main/acme/payments", "gh/acme/widgets"}) {
		t.Errorf("repositories = %v; want every one of both instances", got)
	}
	_, se := e.session(c)
	if !se.Admin || !se.All || se.Role("gh", "acme/widgets") != RoleAdmin || !se.CanSee("main", "acme/anything") {
		t.Errorf("session = %+v", se)
	}
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/gh/acme/widgets"); v.Role != "admin" {
		t.Errorf("role on gh/acme/widgets = %q, want admin", v.Role)
	}
	for _, target := range []string{"/api/users", "/api/teams", "/api/audit", "/api/admin/settings", "/api/admin/instances",
		"/api/integrations?instance=main"} {
		e.wantStatus(c, target, http.StatusOK)
	}
	ov := getJSON[OverviewView](e, c, "/api/overview")
	var kinds []string
	for _, a := range ov.Attention {
		kinds = append(kinds, a.Instance+"/"+a.Project+"/"+a.Kind)
	}
	for _, want := range []string{"main//token_expiry", "main/acme/payments/out_of_memory", "gh/acme/widgets/approver_unreachable"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("attention = %v; want %s", kinds, want)
		}
	}
}

// A user sees the repositories their teams cover and nothing else: not the
// reviews, runs, logs or settings of any other, whatever the instance.
func TestUserSeesOnlyTeamRepos(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	payments := e.addRun(runFix{project: "acme/payments", number: 5, head: head('p'), result: blockingResult(head('p')),
		outcome: OutcomeBlocking, title: "Payments secret change"})
	if err := e.st.PutRunLog(ctx, payments, "payments log"); err != nil {
		t.Fatal(err)
	}
	e.team("widgets", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleViewer}},
		store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	c := e.aliceSession()

	if got := repoNames(e, c); !slices.Equal(got, []string{"main/acme/widgets"}) {
		t.Errorf("repositories = %v; want acme/widgets on main only", got)
	}
	if _, me := e.me(c); me.Admin || me.Waiting || me.Role != store.RoleUser {
		t.Errorf("/api/me = %+v; want a user that is not waiting", me)
	}
	for _, target := range []string{
		"/api/repositories/main/acme/payments", "/api/repositories/main/acme/payments/inputs",
		"/api/reviews/main/acme/payments/5", "/api/runs/" + payments, "/api/runs/" + payments + "/log",
		"/api/repositories/gh/acme/widgets",
	} {
		e.wantStatus(c, target, http.StatusNotFound)
	}
	for _, target := range []string{"/api/reviews", "/api/reviews?project=acme/payments", "/api/overview", "/api/overview?days=90"} {
		rec := e.wantStatus(c, target, http.StatusOK)
		if body := strings.ToLower(rec.Body.String()); strings.Contains(body, "payments") || strings.Contains(body, payments) ||
			strings.Contains(body, "github change") {
			t.Errorf("GET %s shows what the team does not cover: %s", target, rec.Body)
		}
	}
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/payments/settings", `{"approve":false}`), "PUT payments settings", http.StatusNotFound)
	_, se := e.session(c)
	if se.CanSee("main", "acme/payments") || se.Role("main", "acme/payments") != RoleNone || se.CanSee("gh", "acme/widgets") {
		t.Errorf("session sees what its team does not cover: %+v", se)
	}
	if !se.CanSee("main", "ACME/Widgets") || se.Role("main", "acme/widgets") != RoleViewer {
		t.Errorf("session = %+v; want a viewer on acme/widgets, ignoring case", se)
	}
	// What a user may not do: an admin page.
	for _, target := range []string{"/api/users", "/api/teams", "/api/audit", "/api/admin/settings", "/api/integrations?instance=main"} {
		e.wantStatus(c, target, http.StatusForbidden)
	}
}

// /api/me tells an admin how many users wait for a team, the ones the Users
// page marks waiting: SSO accounts with role user that signed in and are on no
// team. A local admin, an SSO admin, an invitation that never signed in, an old
// platform account and a user on a team don't count, and nobody else is told.
func TestMeCountsUsersWaitingForAnAdmin(t *testing.T) {
	e := newEnv(t, nil)
	local := e.operator()
	boss := e.sessionFor(e.ssoAdmin("boss@example.com"))
	e.platformAccount("main", 9, "old")
	if _, err := e.st.AddSSOAccount(ctx, "invited@example.com", e.clock.now()); err != nil {
		t.Fatal(err)
	}
	e.sso("zed@example.com")
	alice := e.aliceSession()
	if _, me := e.me(local); me.WaitingUsers != 2 {
		t.Fatalf("waitingUsers for a local admin = %d; want 2 (alice and zed)", me.WaitingUsers)
	}
	if _, me := e.me(boss); me.WaitingUsers != 2 {
		t.Errorf("waitingUsers for an SSO admin = %d; want 2", me.WaitingUsers)
	}
	// It is what the Users page marks.
	marked := 0
	for _, u := range getJSON[[]UserView](e, local, "/api/users") {
		if u.Waiting {
			marked++
		}
	}
	if marked != 2 {
		t.Errorf("the Users page marks %d waiting; want the same 2", marked)
	}
	if _, me := e.me(alice); me.WaitingUsers != 0 || !me.Waiting {
		t.Errorf("/api/me for a waiting user = %+v; want waitingUsers 0", me)
	}

	e.team("devs", viewerMembers(e.alice()))
	if _, me := e.me(local); me.WaitingUsers != 1 {
		t.Errorf("waitingUsers with alice on a team = %d; want 1", me.WaitingUsers)
	}
	e.sso("invited@example.com") // the invitation signs in
	if _, me := e.me(local); me.WaitingUsers != 2 {
		t.Errorf("waitingUsers after the invitation signed in = %d; want 2", me.WaitingUsers)
	}
	e.team("more", viewerMembers(e.sso("zed@example.com"), e.sso("invited@example.com")))
	if _, me := e.me(local); me.WaitingUsers != 0 {
		t.Errorf("waitingUsers with everyone placed = %d; want 0", me.WaitingUsers)
	}
	// A team member's own answer is 0, whatever the others do.
	if _, me := e.me(alice); me.WaitingUsers != 0 || me.Waiting {
		t.Errorf("/api/me for a member = %+v", me)
	}
}

// A user signing in for the first time is on no team: /api/me says they
// wait, everything else is 403 "waiting", and nothing asks the review bot or
// the platform about a repository.
func TestUserOnNoTeamIsWaiting(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	c := e.aliceSession()
	code, me := e.me(c)
	if code != 200 || !me.Waiting || me.Admin || me.Kind != store.AccountSSO || me.Username != aliceEmail || me.CSRFToken == "" {
		t.Fatalf("/api/me = %d %+v; want alice waiting", code, me)
	}
	for _, target := range readEndpoints("00000000-0000-0000-0000-000000000000") {
		if target != "/api/me" {
			wantWaiting(t, e.get(c, target), "GET "+target)
		}
	}
	wantWaiting(t, e.get(c, "/test/session"), "GET /test/session")
	wantWaiting(t, e.get(c, "/api/repositories/available?instance=main"), "GET /api/repositories/available")
	wantWaiting(t, e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/widgets"}`), "POST /api/repositories")
	wantWaiting(t, e.send(c, "PUT", "/api/repositories/main/acme/widgets/settings", `{"approve":false}`), "PUT settings")

	e.clock.advance(time.Hour)
	if _, me := e.me(c); !me.Waiting {
		t.Errorf("/api/me later = %+v", me)
	}
	wantWaiting(t, e.get(c, "/api/overview"), "GET /api/overview later")
	if n := e.cat["main"].callCount(); n != 0 {
		t.Errorf("the review bot's repositories were listed %d times", n)
	}
	// A team that exists but covers nothing is not waiting: on a team, seeing
	// nothing.
	e.team("empty", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleViewer}})
	if _, me := e.me(c); me.Waiting {
		t.Errorf("/api/me on an empty team = %+v; want not waiting", me)
	}
	if got := repoNames(e, c); len(got) != 0 {
		t.Errorf("repositories on an empty team = %v", got)
	}
	// Signing out works.
	_, me = e.me(c)
	if rec := e.do("POST", "/auth/logout", []*http.Cookie{c}, map[string]string{CSRFHeader: me.CSRFToken}); rec.Code != http.StatusNoContent {
		t.Errorf("logout = %d %s", rec.Code, rec.Body)
	}
}

// Of the teams covering a repository, the highest role wins: a viewer on one
// and a maintainer on another is a maintainer there, a viewer elsewhere.
func TestHighestRoleWins(t *testing.T) {
	e := newEnv(t, nil)
	alice := e.alice()
	e.team("everyone", []store.TeamMember{{AccountID: alice, Role: store.RoleViewer}}, store.TeamRepo{Instance: "main"})
	e.team("widgets", []store.TeamMember{{AccountID: alice, Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	c := e.aliceSession()

	_, se := e.session(c)
	if se.Role("main", "acme/widgets") != RoleMaintainer || se.Role("main", "acme/payments") != RoleViewer {
		t.Errorf("roles = widgets %v, payments %v; want maintainer, viewer", se.Role("main", "acme/widgets"), se.Role("main", "acme/payments"))
	}
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/widgets"); v.Role != "maintainer" {
		t.Errorf("widgets role = %q, want maintainer", v.Role)
	}
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/payments"); v.Role != "viewer" {
		t.Errorf("payments role = %q, want viewer", v.Role)
	}
	// The team's role for the member counts, not another member's.
	bob := e.sso("bob@example.com")
	e.team("others", []store.TeamMember{{AccountID: bob, Role: store.RoleMaintainer}, {AccountID: alice, Role: store.RoleViewer}},
		store.TeamRepo{Instance: "main", Project: "acme/payments"})
	if _, se := e.session(c); se.Role("main", "acme/payments") != RoleViewer {
		t.Errorf("payments role = %v; bob's role on a team leaked to alice", se.Role("main", "acme/payments"))
	}
}

// A team that covers a whole instance covers a repository switched on later,
// with no change to the team.
func TestInstanceWideTeamCoversNewRepository(t *testing.T) {
	e := newEnv(t, nil)
	e.team("main", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "main"})
	c := e.aliceSession()
	if _, se := e.session(c); se.CanSee("main", "acme/gadgets") {
		t.Fatal("sees a repository that is not switched on")
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	_, se := e.session(c)
	if !se.CanSee("main", "acme/gadgets") || se.Role("main", "acme/gadgets") != RoleMaintainer {
		t.Errorf("after switching acme/gadgets on: %+v", se)
	}
	if got := repoNames(e, c); !slices.Contains(got, "main/acme/gadgets") || slices.Contains(got, "gh/acme/widgets") {
		t.Errorf("repositories = %v; want acme/gadgets, and nothing of gh", got)
	}
	// Switched off, it is gone again.
	if err := e.st.DeleteDashboardRepo(ctx, "main", "acme/gadgets"); err != nil {
		t.Fatal(err)
	}
	if _, se := e.session(c); se.CanSee("main", "acme/gadgets") {
		t.Error("still sees acme/gadgets after it was switched off")
	}
}

// Review Focus 3: a repository switched on and covered by a team is visible
// before reconcile has stored its project. It has no ID yet, so no run is
// visible, but its page opens.
func TestUnreconciledRepositoryIsVisible(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.team("gadgets", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleViewer}},
		store.TeamRepo{Instance: "main", Project: "acme/gadgets"})
	c := e.aliceSession()

	_, se := e.session(c)
	if id, ok := se.Visible[repoKey("main", "acme/gadgets")]; !ok || id != 0 {
		t.Fatalf("visible = %v; want acme/gadgets with ID 0", se.Visible)
	}
	if !se.CanSee("main", "acme/gadgets") || se.CanSeeRun("main", "acme/gadgets", 0) || se.CanSeeRun("main", "acme/gadgets", 7) {
		t.Errorf("CanSee/CanSeeRun on an unreconciled repository: %+v", se)
	}
	if got := repoNames(e, c); !slices.Equal(got, []string{"main/acme/gadgets"}) {
		t.Errorf("repositories = %v; want acme/gadgets", got)
	}
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets"); v.Role != "viewer" {
		t.Errorf("role = %q, want viewer", v.Role)
	}
	if list := getJSON[ReviewListView](e, c, "/api/reviews"); len(list.Items) != 0 {
		t.Errorf("reviews = %+v; want none", list.Items)
	}
	e.wantStatus(c, "/api/repositories/main/acme/gadgets/inputs", http.StatusServiceUnavailable)

	// Once reconcile stores the project, its runs show.
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: 55, Path: "acme/gadgets", HookID: 1}); err != nil {
		t.Fatal(err)
	}
	e.addRun(runFix{project: "acme/gadgets", projectID: 55, number: 3, head: head('g'), result: cleanResult(head('g')),
		outcome: OutcomeApproved, approved: true})
	_, se = e.session(c)
	if se.Visible[repoKey("main", "acme/gadgets")] != 55 || !se.CanSeeRun("main", "acme/gadgets", 55) {
		t.Errorf("after reconcile: %+v", se)
	}
	if list := getJSON[ReviewListView](e, c, "/api/reviews"); len(list.Items) != 1 {
		t.Errorf("reviews = %+v; want the one run", list.Items)
	}
}

// Review Focus 3: a team removed, or a member taken off it, ends access at
// the next request, with no wait.
func TestTeamRemovalEndsAccess(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	alice, bob := e.alice(), e.sso("bob@example.com")
	members := []store.TeamMember{{AccountID: alice, Role: store.RoleViewer}, {AccountID: bob, Role: store.RoleViewer}}
	shared := e.team("shared", members, store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	c := e.aliceSession()
	if got := repoNames(e, c); !slices.Equal(got, []string{"main/acme/widgets"}) {
		t.Fatalf("repositories = %v", got)
	}

	// Taken off the team: on no team, waiting.
	e.editTeam(shared, func(tm *store.Team) { tm.Members = viewerMembers(bob) })
	wantWaiting(t, e.get(c, "/api/repositories"), "GET /api/repositories after leaving the team")
	e.editTeam(shared, func(tm *store.Team) { tm.Members = members })
	if got := repoNames(e, c); !slices.Equal(got, []string{"main/acme/widgets"}) {
		t.Fatalf("repositories after rejoining = %v", got)
	}

	// The team stops covering the repository: still a member, sees nothing.
	e.editTeam(shared, func(tm *store.Team) { tm.Repos = nil })
	if got := repoNames(e, c); len(got) != 0 {
		t.Errorf("repositories after the team dropped acme/widgets = %v", got)
	}
	e.wantStatus(c, "/api/repositories/main/acme/widgets", http.StatusNotFound)
	e.editTeam(shared, func(tm *store.Team) { tm.Repos = []store.TeamRepo{{Instance: "main", Project: "acme/widgets"}} })

	// The team deleted: waiting again.
	if _, err := e.st.DeleteTeam(ctx, shared, nil); err != nil {
		t.Fatal(err)
	}
	wantWaiting(t, e.get(c, "/api/overview"), "GET /api/overview after the team was deleted")
	wantWaiting(t, e.get(c, "/api/runs/"+overviewRun(e)), "GET a run after the team was deleted")
}

// overviewRun is the ID of any stored run.
func overviewRun(e *env) string {
	e.t.Helper()
	var id string
	if err := e.db.QueryRow(ctx, `SELECT id::text FROM runs ORDER BY created_at LIMIT 1`).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// A session whose account is of the old platform kind (a pod of the previous
// release can still make one after the upgrade, as a password session by the
// column's default) gets nothing: the next request ends it, whatever team the
// account was on.
func TestPlatformSessionGetsNothing(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	old, err := e.st.UpsertPlatformAccount(ctx, config.PlatformGitLab, "main", 9, "alice", e.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	// A membership row from before the upgrade (the store refuses new ones).
	team := e.team("old", nil, store.TeamRepo{Instance: "main"})
	e.exec(`INSERT INTO team_members (team_id, account_id, role) VALUES ($1, $2, 'viewer')`, team, old.ID)
	// platformSession stores a session of the old account, as a pod of the
	// previous release makes it, and returns its cookie.
	platformSession := func(csrf string) *http.Cookie {
		t.Helper()
		id := randomString(sessionIDBytes)
		err := e.st.CreateSession(ctx, store.Session{IDHash: hashID(id), AccountID: old.ID, Platform: config.PlatformGitLab,
			Instance: "main", UserID: 9, Username: "alice", CSRF: csrf, LastSeenAt: e.clock.now(), CreatedAt: e.clock.now()})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Cookie{Name: SessionCookieName, Value: id}
	}
	c := platformSession("csrf-token-example")
	// A second session for the write at the end: the first request ends the
	// first one, and a write sent with its cookie would be refused for having
	// no session, which proves nothing about the account.
	second := platformSession("csrf-token-second")
	if se, found := e.storedSession(c); !found || se.SignIn != store.SignInPassword {
		t.Fatalf("stored session = %+v, %v; want one that reads as a password session", se, found)
	}

	for _, target := range []string{"/api/me", "/api/repositories", "/api/overview", "/api/users", "/test/session"} {
		if rec := e.get(c, target); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d %s; want 401", target, rec.Code, rec.Body)
		}
	}
	if _, found := e.storedSession(c); found {
		t.Error("the session of a platform account was kept")
	}
	// A write with a live session of the same account, and the right CSRF
	// token, is refused because of the account, and the session ends.
	if _, found := e.storedSession(second); !found {
		t.Fatal("the second session is gone before it was used")
	}
	req := httptest.NewRequest("POST", "/api/teams", strings.NewReader(`{"name":"x"}`))
	req.AddCookie(second)
	req.Header.Set(CSRFHeader, "csrf-token-second")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/teams = %d %s; want 401", rec.Code, rec.Body)
	}
	if _, found := e.storedSession(second); found {
		t.Error("the write did not end the second session")
	}
	var teams int
	if err := e.db.QueryRow(ctx, `SELECT count(*) FROM teams WHERE name = 'x'`).Scan(&teams); err != nil || teams != 0 {
		t.Errorf("teams named x = %d, %v; want none", teams, err)
	}
	// And the account does not show on the Users page.
	admin := e.ssoAdmin("root@example.com")
	users := getJSON[[]UserView](e, e.sessionFor(admin), "/api/users")
	for _, u := range users {
		if u.Kind == store.AccountPlatform {
			t.Errorf("users lists a platform account: %+v", u)
		}
	}
}

// A failure to read the account's teams answers 500 with a fixed text, ends
// nothing and gives no access: it is never read as "waiting" or as access.
func TestAccessLoadFailureAnswers500(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	e.team("devs", viewerMembers(e.alice()), store.TeamRepo{Instance: "main"})
	c := e.aliceSession()
	wantCode(t, e.get(c, "/api/repositories"), "GET /api/repositories before", http.StatusOK)

	e.exec(`ALTER TABLE team_members RENAME TO team_members_gone`)
	for _, target := range []string{"/api/me", "/api/repositories", "/api/overview", "/test/session"} {
		rec := e.get(c, target)
		wantCode(t, rec, "GET "+target+" while the teams can't be read", http.StatusInternalServerError)
		if got := errorOf(t, rec); got != "could not load the session" {
			t.Errorf("GET %s error = %q; want the fixed text", target, got)
		}
		if strings.Contains(rec.Body.String(), "team_members") {
			t.Errorf("GET %s leaks the failure: %s", target, rec.Body)
		}
	}
	if _, found := e.storedSession(c); !found {
		t.Error("a load failure ended the session")
	}

	e.exec(`ALTER TABLE team_members_gone RENAME TO team_members`)
	wantCode(t, e.get(c, "/api/repositories"), "GET /api/repositories after", http.StatusOK)
}

// GitHub and GitLab sign-in is gone: the routes are not there.
func TestPlatformSignInRoutesAreGone(t *testing.T) {
	e := newEnv(t, nil)
	for _, target := range []string{
		"/auth/github/x/login", "/auth/github/gh/login", "/auth/gitlab/main/login",
		"/auth/github/gh/callback?code=c&state=s", "/auth/gitlab/main/callback?code=c&state=s",
	} {
		if rec := e.get(nil, target); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d %s; want 404", target, rec.Code, rec.Body)
		}
	}
	// The sign-in page's question has no instance to offer.
	rec := e.get(nil, "/api/instances")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"password":true`) || !strings.Contains(rec.Body.String(), `"instances":[]`) {
		t.Errorf("GET /api/instances = %d %s; want the password form and no instance", rec.Code, rec.Body)
	}
}

// A maintainer changes the settings of the repositories their teams cover,
// and can't switch them off; a viewer changes nothing.
func TestMaintainerCanEditSettings(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	team := e.team("gadgets", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "main", Project: "acme/gadgets"})
	c := e.aliceSession()
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets"); v.Role != "maintainer" {
		t.Errorf("role = %q, want maintainer", v.Role)
	}

	rec := e.send(c, "PUT", "/api/repositories/main/acme/gadgets/settings", `{"approve":false}`)
	wantCode(t, rec, "maintainer PUT settings", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.Approve == nil || *r.Approve {
		t.Errorf("approve = %v, want false", r.Approve)
	}
	wantCode(t, e.send(c, "POST", "/api/repositories/main/acme/gadgets/settings/reset", ""), "maintainer reset", http.StatusOK)
	wantCode(t, e.send(c, "DELETE", "/api/repositories/main/acme/gadgets", ""), "maintainer switch off", http.StatusForbidden)
	wantCode(t, e.get(c, "/api/repositories/available?instance=main"), "maintainer picker", http.StatusForbidden)
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/gadgets/settings", `{"rerunLimitPerMR":5}`), "maintainer re-run caps", http.StatusForbidden)
	if _, found := e.storedRepo("main", "acme/gadgets"); !found {
		t.Fatal("the maintainer switched acme/gadgets off")
	}

	e.editTeam(team, func(tm *store.Team) { tm.Members = []store.TeamMember{{AccountID: e.alice(), Role: store.RoleViewer}} })
	rec = e.send(c, "PUT", "/api/repositories/main/acme/gadgets/settings", `{"approve":false}`)
	wantCode(t, rec, "viewer PUT settings", http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), msgNeedsRole) {
		t.Errorf("viewer PUT body = %s", rec.Body)
	}
	wantCode(t, e.send(c, "POST", "/api/repositories/main/acme/gadgets/settings/reset", ""), "viewer reset", http.StatusForbidden)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.Approve != nil {
		t.Errorf("the viewer changed approve to %v", *r.Approve)
	}
	if n := e.reconciles.Load(); n != 0 {
		t.Errorf("ReconcileSoon called %d times", n)
	}
}

// An admin may do everything on any repository: switch one off, change the
// re-run caps.
func TestAdminMayChangeAnyRepository(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.ssoAdmin(aliceEmail)
	c := e.aliceSession()
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/gadgets/settings", `{"rerunLimitPerMR":5}`), "admin caps", http.StatusOK)
	wantCode(t, e.send(c, "DELETE", "/api/repositories/main/acme/gadgets", ""), "admin switch off", http.StatusOK)
	if _, found := e.storedRepo("main", "acme/gadgets"); found {
		t.Error("acme/gadgets is still switched on")
	}
}

// A local admin is an admin like any other: it reads the repositories,
// reviews, runs and setup problems of every instance, and never asks a
// platform.
func TestLocalAdminSeesEverything(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e)
	ghRun := e.addRun(runFix{instance: "gh", project: "acme/widgets", projectID: 900, number: 8, head: head('8'),
		result: cleanResult(head('8')), outcome: OutcomeApproved, approved: true})
	if err := e.st.PutRunLog(ctx, ghRun, "gh log"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertGitHubProject(ctx, store.Project{Instance: "gh", ProjectID: 900, Path: "acme/widgets", InstallationID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: 8, Path: "acme/payments", HookID: 1}); err != nil {
		t.Fatal(err)
	}
	e.addPasswordAccount("ops", "ops-password-123", false)
	c := e.signInPassword("ops", "ops-password-123")
	code, me := e.me(c)
	if code != 200 || !me.Admin || me.Waiting || me.Kind != "password" || me.Role != store.RoleAdmin || me.SignIn != store.SignInPassword ||
		me.Email != "" || me.Username != "ops" {
		t.Fatalf("/api/me = %d %+v", code, me)
	}

	if got := repoNames(e, c); !slices.Equal(got, []string{"main/acme/widgets", "main/acme/payments", "gh/acme/widgets"}) {
		t.Errorf("repositories = %v; want every one of both instances", got)
	}
	list := getJSON[ReviewListView](e, c, "/api/reviews")
	var reviews []string
	for _, r := range list.Items {
		reviews = append(reviews, fmt.Sprintf("%s/%s/%d", r.Instance, r.Project, r.Number))
	}
	for _, want := range []string{"gh/acme/widgets/7", "gh/acme/widgets/8", "main/acme/payments/5", "main/acme/widgets/1"} {
		if !slices.Contains(reviews, want) {
			t.Errorf("reviews = %v; want %s among them", reviews, want)
		}
	}
	if l := getJSON[ReviewListView](e, c, "/api/reviews?project=acme/widgets&instance=gh"); len(l.Items) != 2 {
		t.Errorf("gh's acme/widgets reviews = %+v; want 7 and 8", l.Items)
	}
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/gh/acme/widgets"); v.Role != "admin" || v.Repo.Last7.Reviews != 2 {
		t.Errorf("gh/acme/widgets = %+v", v)
	}
	if v := getJSON[ReviewDetailView](e, c, "/api/reviews/main/acme/payments/5"); len(v.Runs) != 1 {
		t.Errorf("payments review = %+v", v)
	}
	if v := getJSON[LogView](e, c, "/api/runs/"+ghRun+"/log"); v.Text != "gh log" {
		t.Errorf("gh run log = %+v", v)
	}
	ov := getJSON[OverviewView](e, c, "/api/overview")
	var kinds []string
	for _, a := range ov.Attention {
		kinds = append(kinds, a.Instance+"/"+a.Project+"/"+a.Kind)
	}
	for _, want := range []string{"main//token_expiry", "main/acme/payments/out_of_memory", "gh/acme/widgets/approver_unreachable"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("attention = %v; want %s", kinds, want)
		}
	}
	if ov.Stats.Reviews < 7 {
		t.Errorf("stats = %+v; want both instances' runs", ov.Stats)
	}
}

// The picker lists what the review bot or App reaches on the instance a
// request names, for any admin; switching on names the instance too.
func TestAdminPicksRepositories(t *testing.T) {
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets", "acme/widgets")
	e.ssoAdmin(aliceEmail)
	c := e.aliceSession()
	if rec := e.get(c, "/api/repositories/available"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "name the instance") {
		t.Errorf("GET available without an instance = %d %s; want 400 asking for it", rec.Code, rec.Body)
	}
	wantCode(t, e.get(c, "/api/repositories/available?instance=nope"), "GET available for an unknown instance", http.StatusNotFound)
	v := getJSON[AvailableView](e, c, "/api/repositories/available?instance=main")
	if len(v.Items) != 1 || v.Items[0].Project != "acme/gadgets" || v.Items[0].Instance != "main" || v.Items[0].Platform != "gitlab" {
		t.Errorf("available = %+v; want acme/gadgets on main (acme/widgets is on already)", v)
	}
	if rec := e.send(c, "POST", "/api/repositories", `{"project":"acme/gadgets"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "name the instance") {
		t.Errorf("POST /api/repositories without an instance = %d %s; want 400 asking for it", rec.Code, rec.Body)
	}
	rec := e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/gadgets"}`)
	wantCode(t, rec, "POST /api/repositories", http.StatusCreated)
	if _, found := e.storedRepo("main", "acme/gadgets"); !found {
		t.Error("acme/gadgets was not switched on")
	}
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/unknown"}`), "POST an unreachable repository", http.StatusNotFound)
	if n := e.reconciles.Load(); n != 1 {
		t.Errorf("ReconcileSoon called %d times, want 1", n)
	}
}
