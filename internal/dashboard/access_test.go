package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func mustBox(t *testing.T, key string) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// issueToken makes the fake hand out a token and returns it.
func issueToken(o *oauthServer) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.issue()
	return o.access
}

func TestVisibilityRules(t *testing.T) {
	gl := newFakeGitLab(t)
	glTok := issueToken(&gl.oauthServer)
	glp := &gitlabProvider{baseURL: gl.srv.URL, hc: http.DefaultClient}
	gh := newFakeGitHub(t)
	ghTok := issueToken(&gh.oauthServer)
	ghp := &githubProvider{apiURL: gh.srv.URL + "/api/v3", hc: http.DefaultClient}

	featurePrivate := gitlabProject("public", 0, 0)
	featurePrivate["repository_access_level"] = "private"
	mrsDisabled := gitlabProject("public", 0, 0)
	mrsDisabled["merge_requests_access_level"] = "disabled"
	noFeatureFields := gitlabProject("public", 0, 0)
	delete(noFeatureFields, "repository_access_level")
	delete(noFeatureFields, "merge_requests_access_level")
	sharedReporter := gitlabProject("private", 0, 0)
	sharedReporter["shared_access"] = 20
	sharedGuest := gitlabProject("internal", 10, 0)
	sharedGuest["shared_access"] = 10

	tests := []struct {
		name    string
		gitlab  map[string]any // nil: a GitHub case, or a 404 when github is nil too
		github  map[string]any
		missing bool
		want    bool
	}{
		{name: "gitlab guest on a private project", gitlab: gitlabProject("private", 10, 0), want: false},
		{name: "gitlab guest on an internal project", gitlab: gitlabProject("internal", 10, 0), want: false},
		{name: "gitlab non-member of an internal project", gitlab: gitlabProject("internal", 0, 0), want: false},
		{name: "gitlab reporter", gitlab: gitlabProject("private", 20, 0), want: true},
		{name: "gitlab developer", gitlab: gitlabProject("private", 30, 0), want: true},
		{name: "gitlab reporter through the group", gitlab: gitlabProject("private", 10, 20), want: true},
		{name: "gitlab guest in the group", gitlab: gitlabProject("private", 0, 10), want: false},
		{name: "gitlab reporter through a share", gitlab: sharedReporter, want: true},
		{name: "gitlab guest through a share", gitlab: sharedGuest, want: false},
		{name: "gitlab public project", gitlab: gitlabProject("public", 0, 0), want: true},
		{name: "gitlab public project, code for members only", gitlab: featurePrivate, want: false},
		{name: "gitlab public project, merge requests off", gitlab: mrsDisabled, want: false},
		{name: "gitlab public project, feature levels not reported", gitlab: noFeatureFields, want: false},
		{name: "gitlab project not found", missing: true, want: false},
		{name: "github private without pull", github: githubRepo(true, false), want: false},
		{name: "github private with pull", github: githubRepo(true, true), want: true},
		{name: "github public", github: githubRepo(false, false), want: true},
		{name: "github private without a permissions object", github: map[string]any{"private": true}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got bool
			var id int64
			var err error
			if tt.github != nil {
				gh.setRepo("acme/widgets", tt.github)
				id, got, err = ghp.canSee(ctx, ghTok, "acme/widgets")
			} else {
				gl.set(func(*oauthServer) { delete(gl.projects, "acme/widgets") })
				if !tt.missing {
					gl.setProject("acme/widgets", tt.gitlab)
				}
				id, got, err = glp.canSee(ctx, glTok, "acme/widgets")
			}
			if err != nil || got != tt.want {
				t.Fatalf("canSee = %v, %v; want %v", got, err, tt.want)
			}
			// A visible project comes with its platform ID.
			if got && id != testID("acme/widgets") {
				t.Fatalf("canSee id = %d, want %d", id, testID("acme/widgets"))
			}
		})
	}

	// GitHub: a repository not found is not visible; a malformed name is never asked.
	if _, got, err := ghp.canSee(ctx, ghTok, "acme/none"); err != nil || got {
		t.Errorf("github not found: %v, %v", got, err)
	}
	if _, got, _ := ghp.canSee(ctx, ghTok, "acme"); got {
		t.Error("github: a name without owner is visible")
	}
	// A token the platform refuses is an error, not "not visible".
	if _, _, err := glp.canSee(ctx, "revoked", "acme/widgets"); err == nil {
		t.Error("gitlab: a refused token gave no error")
	}
	if _, _, err := ghp.canSee(ctx, "revoked", "acme/widgets"); err == nil {
		t.Error("github: a refused token gave no error")
	}
}

// An admin is a member of an admin team, by account: admin rights on one
// instance's account give nothing to the same person's account on another,
// and leaving the team ends them at the next request.
func TestAdminByTeam(t *testing.T) {
	t.Run("gitlab admin team member", func(t *testing.T) {
		e := newEnv(t, nil)
		e.aliceOn("main", store.RoleAdmin)
		if _, me := e.me(e.signIn("gitlab", "main")); !me.Admin || me.Operator || me.Waiting {
			t.Fatalf("an admin team's member = %+v; want an admin, no operator", me)
		}
	})
	t.Run("github admin team member", func(t *testing.T) {
		e := newEnv(t, nil)
		e.aliceOn("gh", store.RoleAdmin)
		if _, me := e.me(e.signIn("github", "gh")); !me.Admin {
			t.Fatal("an admin team's member is not admin")
		}
	})
	t.Run("maintainer team member", func(t *testing.T) {
		e := newEnv(t, nil)
		e.aliceOn("main", store.RoleMaintainer)
		if _, me := e.me(e.signIn("gitlab", "main")); me.Admin || me.Waiting {
			t.Fatalf("a maintainer team's member = %+v; want no admin, not waiting", me)
		}
	})
	t.Run("admin rights are per account", func(t *testing.T) {
		e := newEnv(t, nil)
		e.aliceOn("gh", store.RoleAdmin)
		e.aliceOn("main", store.RoleViewer)
		if _, me := e.me(e.signIn("gitlab", "main")); me.Admin {
			t.Fatal("admin on main through the gh account's team")
		}
	})
	t.Run("membership lost", func(t *testing.T) {
		e := newEnv(t, nil)
		team := e.aliceOn("main", store.RoleAdmin)
		e.aliceOn("main", store.RoleViewer)
		c := e.signIn("gitlab", "main")
		e.editTeam(team, func(tm *store.Team) { tm.Members = nil })
		if _, me := e.me(c); me.Admin {
			t.Fatal("still admin after leaving the admin team")
		}
	})
}

// Review Focus 1: a user whose access drops from Developer to Guest stops
// seeing the repository within 5 minutes.
func TestAccessRefreshDropsRepository(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	e.gl.setProject("acme/payments", gitlabProject("private", 20, 0))
	e.aliceOn("main", store.RoleViewer)
	c := e.signIn("gitlab", "main")

	_, se := e.session(c)
	if !se.CanSee("main", "acme/widgets") || !se.CanSee("main", "acme/payments") || se.CanSee("main", "acme/other") {
		t.Fatalf("visible after sign-in = %v", se.Visible)
	}

	e.gl.setProject("acme/widgets", gitlabProject("private", 10, 0)) // now Guest
	e.clock.advance(4 * time.Minute)
	if _, se := e.session(c); !se.CanSee("main", "acme/widgets") {
		t.Fatal("dropped before the 5-minute refresh: the cache is not used")
	}
	e.clock.advance(time.Minute + time.Second)
	_, se = e.session(c)
	if se.CanSee("main", "acme/widgets") || !se.CanSee("main", "acme/payments") {
		t.Fatalf("visible 5 min after the drop = %v", se.Visible)
	}
	projects, err := e.st.SessionProjects(ctx, hashOf(t, c))
	if err != nil || !slices.Equal(projects, []store.VisibleProject{{Instance: "main", Path: "acme/payments", ID: testID("acme/payments")}}) {
		t.Fatalf("stored user_access = %v, %v", projects, err)
	}

	// Only the session's instance is checked: gh's acme/widgets is never
	// in a GitLab session.
	if len(se.Visible) != 1 {
		t.Fatalf("visible = %v", se.Visible)
	}
}

// A failed refresh keeps the last list for one more period, then fails
// closed: no repository and no role, admin team or not, until a check
// succeeds.
func TestRefreshFailureFailsClosedAfterOnePeriod(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	e.aliceOn("main", store.RoleAdmin)
	c := e.signIn("gitlab", "main")

	e.gl.set(func(o *oauthServer) { o.failAPI = true })
	e.clock.advance(6 * time.Minute)
	if _, se := e.session(c); !se.CanSee("main", "acme/widgets") || !se.Admin {
		t.Fatalf("within one more period: %+v", se)
	}
	e.clock.advance(4*time.Minute + time.Second) // 10 min + 1 s after the last good check
	code, se := e.session(c)
	if code != 200 || len(se.Visible) != 0 || len(se.Roles) != 0 || se.Admin || se.Waiting ||
		se.Role("main", "acme/widgets") != RoleNone {
		t.Fatalf("after two periods without a check: %d %+v", code, se)
	}
	if _, me := e.me(c); me.Username != "alice" || me.Admin || me.WaitingUsers != 0 {
		t.Fatalf("/api/me after failing closed = %+v; want alice, no admin", me)
	}
	for _, target := range []string{"/api/integrations", "/api/admin/settings"} {
		e.wantStatus(c, target, http.StatusForbidden)
	}
	stored, _ := e.storedSession(c)
	projects, _ := e.st.SessionProjects(ctx, hashOf(t, c))
	if stored.AccessScope != "" || len(projects) != 0 {
		t.Fatalf("stored access not cleared: scope %q, projects %v", stored.AccessScope, projects)
	}

	e.gl.set(func(o *oauthServer) { o.failAPI = false })
	e.clock.advance(time.Minute + time.Second)
	if _, se := e.session(c); !se.CanSee("main", "acme/widgets") || !se.Admin {
		t.Fatalf("after the platform is back: %+v", se)
	}
}

// GitHub user tokens of an App expire; pergent.sh refreshes them, and a
// refused refresh fails closed like any failed check.
func TestExpiredTokenRefreshFailsClosed(t *testing.T) {
	e := newEnv(t, nil)
	e.gh.set(func(o *oauthServer) { o.expiresIn = 120 })
	e.gh.setRepo("acme/widgets", githubRepo(true, true))
	e.aliceOn("gh", store.RoleViewer)
	c := e.signIn("github", "gh")
	first := e.gh.tokens()

	// Refreshed: the new token is used, the old one stops working.
	e.clock.advance(5*time.Minute + time.Second)
	if _, se := e.session(c); !se.CanSee("gh", "acme/widgets") {
		t.Fatalf("after a token refresh: %+v", se)
	}
	if now := e.gh.tokens(); len(now) != len(first)+2 {
		t.Fatalf("tokens issued = %v, want one refresh", now)
	}
	stored, _ := e.storedSession(c)
	box := mustBox(t, e.key)
	plain, err := box.Open(stored.TokenSealed)
	if err != nil || !containsAll(string(plain), e.gh.tokens()[2:]) {
		t.Fatal("the refreshed token was not stored")
	}

	// The refresh token is refused: the old list for one more period, then
	// nothing.
	e.gh.set(func(o *oauthServer) { o.failRefresh = true })
	e.clock.advance(6 * time.Minute)
	if _, se := e.session(c); !se.CanSee("gh", "acme/widgets") {
		t.Fatalf("within one more period: %+v", se)
	}
	e.clock.advance(4*time.Minute + time.Second)
	if _, se := e.session(c); len(se.Visible) != 0 || len(se.Roles) != 0 || se.Admin {
		t.Fatalf("after a refused refresh and two periods: %+v", se)
	}
}

// Concurrent requests with a stale list check access once.
func TestConcurrentRefreshChecksOnce(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	e.aliceOn("main", store.RoleViewer)
	c := e.signIn("gitlab", "main")
	calls := e.gl.calls()
	checks := e.gl.projectChecks()
	e.clock.advance(5*time.Minute + time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, se := e.session(c); code != 200 || !se.CanSee("main", "acme/widgets") {
				t.Errorf("concurrent request: %d %+v", code, se)
			}
		}()
	}
	wg.Wait()
	// One check: one look at each of main's two repositories.
	if n := e.gl.projectChecks() - checks; n != 2 {
		t.Errorf("%d project lookups for 8 concurrent requests, want 2", n)
	}
	// The GitLab token (2 h) is not refreshed; the stored check time moved.
	if e.gl.calls() != calls {
		t.Errorf("token endpoint called %d times", e.gl.calls()-calls)
	}
	if se, _ := e.storedSession(c); !se.AccessCheckedAt.Equal(e.clock.now()) {
		t.Errorf("access_checked_at = %v", se.AccessCheckedAt)
	}
}

// Teams hold accounts, which are platform user IDs, never logins: a
// renamed GitHub user keeps their teams, and the username shown follows the
// rename, read afresh from /user at each access check.
func TestRenamedUserChecksCurrentLogin(t *testing.T) {
	e := newEnv(t, nil)
	e.gh.setRepo("acme/widgets", githubRepo(true, true))
	e.aliceOn("gh", store.RoleAdmin)
	c := e.signIn("github", "gh")
	if _, me := e.me(c); !me.Admin || me.Username != "alice" {
		t.Fatalf("before the rename: %+v", me)
	}

	e.gh.setLogin("bob")
	e.clock.advance(AccessTTL + time.Second)
	_, me := e.me(c)
	if !me.Admin {
		t.Fatal("the rename lost the admin team")
	}
	if me.Username != "bob" {
		t.Errorf("username = %q, want bob", me.Username)
	}
	if se, _ := e.storedSession(c); se.Username != "bob" {
		t.Errorf("stored username = %q, want bob", se.Username)
	}
}

// The username shown follows a GitLab rename too.
func TestRenamedGitLabUserUpdated(t *testing.T) {
	e := newEnv(t, nil)
	e.aliceOn("main", store.RoleViewer)
	c := e.signIn("gitlab", "main")
	e.gl.setUsername("alice-renamed")
	e.clock.advance(AccessTTL + time.Second)
	if _, me := e.me(c); me.Username != "alice-renamed" {
		t.Fatalf("username after the rename = %q", me.Username)
	}
}

// Every call to the platform gets its own 10 s deadline, even with an
// injected HTTP client that has none.
func TestPlatformCallsTimeOut(t *testing.T) {
	hang := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(hang))
	t.Cleanup(srv.Close)
	noTimeout := &http.Client{}
	ps := []provider{
		&gitlabProvider{baseURL: srv.URL, hc: noTimeout, now: time.Now, timeout: 50 * time.Millisecond},
		&githubProvider{webURL: srv.URL, apiURL: srv.URL, hc: noTimeout, now: time.Now, timeout: 50 * time.Millisecond},
	}
	for _, p := range ps {
		calls := map[string]func() error{
			"exchange": func() error { _, err := p.exchange(ctx, "code", "verifier"); return err },
			"refresh":  func() error { _, err := p.refresh(ctx, "refresh"); return err },
			"user":     func() error { _, _, err := p.user(ctx, "token"); return err },
			"canSee":   func() error { _, _, err := p.canSee(ctx, "token", "acme/widgets"); return err },
		}
		for name, call := range calls {
			start := time.Now()
			err := call()
			if err == nil || time.Since(start) > 3*time.Second {
				t.Errorf("%s %s: err %v after %s, want a timeout", p.platform(), name, err, time.Since(start))
			}
		}
	}

	// New gives every provider the 10 s deadline, whatever client it is given.
	t.Setenv("PERGENT_TEST_ID", "id")
	t.Setenv("PERGENT_TEST_SECRET", "secret")
	cfg := &config.Config{
		GitLab: []config.Instance{{Name: "main", URL: "https://gitlab.example.com",
			OAuthClientIDEnv: "PERGENT_TEST_ID", OAuthClientSecretEnv: "PERGENT_TEST_SECRET"}},
		GitHub: []config.GitHubInstance{{Name: "gh", APIURL: "https://api.github.com",
			OAuthClientIDEnv: "PERGENT_TEST_ID", OAuthClientSecretEnv: "PERGENT_TEST_SECRET"}},
		Dashboard: config.Dashboard{URL: dashboardURL},
	}
	d, err := New(Options{Config: cfg, SecretKey: testKey(t), HTTPClient: noTimeout})
	if err != nil {
		t.Fatal(err)
	}
	signInProviders(t, d)
	gl, _ := d.Auth.providers.Get("main")
	if got := gl.(*gitlabProvider).timeout; got != 10*time.Second {
		t.Errorf("gitlab call timeout = %s", got)
	}
	gh, _ := d.Auth.providers.Get("gh")
	if got := gh.(*githubProvider).timeout; got != 10*time.Second {
		t.Errorf("github call timeout = %s", got)
	}
}

// wantWaiting checks rec is the 403 of a platform account on no team.
func wantWaiting(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != `{"code":"waiting","error":"an admin needs to add you to a team"}` {
		t.Errorf("%s = %d %s; want 403 waiting", what, rec.Code, rec.Body)
	}
}

// A user signing in for the first time is on no team: /api/me says they
// wait, everything else is 403 "waiting", and none of it asks the platform
// about a repository.
func TestWaitingUser(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	c := e.signIn("gitlab", "main")
	code, me := e.me(c)
	if code != 200 || !me.Waiting || me.Admin || me.Operator || me.Kind != "platform" || me.Username != "alice" ||
		me.Instance != "main" || me.CSRFToken == "" || me.WaitingUsers != 0 {
		t.Fatalf("/api/me = %d %+v; want alice waiting", code, me)
	}
	for _, target := range readEndpoints("00000000-0000-0000-0000-000000000000") {
		if target != "/api/me" {
			wantWaiting(t, e.get(c, target), "GET "+target)
		}
	}
	wantWaiting(t, e.get(c, "/test/session"), "GET /test/session")
	wantWaiting(t, e.get(c, "/api/repositories/available"), "GET /api/repositories/available")
	wantWaiting(t, e.send(c, "POST", "/api/repositories", `{"project":"acme/widgets"}`), "POST /api/repositories")
	wantWaiting(t, e.send(c, "PUT", "/api/repositories/main/acme/widgets/settings", `{"approve":false}`), "PUT settings")

	// Past the access check's 5 minutes: still nothing asked.
	api := e.gl.api()
	e.clock.advance(AccessTTL + time.Second)
	if _, me := e.me(c); !me.Waiting {
		t.Errorf("/api/me later = %+v", me)
	}
	wantWaiting(t, e.get(c, "/api/overview"), "GET /api/overview later")
	if n := e.gl.projectChecks(); n != 0 {
		t.Errorf("%d project lookups for a waiting user", n)
	}
	if n := e.gl.api() - api; n != 0 {
		t.Errorf("%d platform calls for a waiting user's requests", n)
	}
	if n := e.cat["main"].callCount(); n != 0 {
		t.Errorf("the review bot's repositories were listed %d times", n)
	}
	// Signing out works.
	if rec := e.do("POST", "/auth/logout", []*http.Cookie{c}, map[string]string{CSRFHeader: me.CSRFToken}); rec.Code != http.StatusNoContent {
		t.Errorf("logout = %d %s", rec.Code, rec.Body)
	}
}

// coversPayments checks what alice's session shows of acme/payments on
// main, which her team covers but GitLab hides from her: nothing, as for a
// repository that doesn't exist.
func coversPayments(t *testing.T, e *env, c *http.Cookie, run string) {
	t.Helper()
	for _, target := range []string{
		"/api/repositories/main/acme/payments", "/api/repositories/main/acme/payments/inputs",
		"/api/reviews/main/acme/payments/5", "/api/runs/" + run, "/api/runs/" + run + "/log",
	} {
		e.wantStatus(c, target, http.StatusNotFound)
	}
	for _, target := range []string{"/api/repositories", "/api/reviews", "/api/reviews?project=acme/payments",
		"/api/overview", "/api/overview?days=90"} {
		rec := e.wantStatus(c, target, http.StatusOK)
		if body := strings.ToLower(rec.Body.String()); strings.Contains(body, "payments") || strings.Contains(body, run) {
			t.Errorf("GET %s shows acme/payments: %s", target, rec.Body)
		}
	}
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/payments/settings", `{"approve":false}`), "PUT payments settings", http.StatusNotFound)
	if repos := getJSON[[]RepositoryView](e, c, "/api/repositories"); len(repos) != 1 || repos[0].Project != "acme/widgets" {
		t.Errorf("repositories = %+v; want acme/widgets only", repos)
	}
	if _, se := e.session(c); se.CanSee("main", "acme/payments") || se.Role("main", "acme/payments") != RoleNone {
		t.Errorf("session sees acme/payments: %+v", se)
	}
}

// The platform is the ceiling: a team covering a repository the platform
// hides from its member shows them nothing of it, whether it was covered at
// sign-in or added to the team later.
func TestTeamCoverageNeverExceedsPlatform(t *testing.T) {
	e := newEnv(t, nil)
	overviewFixture(e) // acme/payments' run of MR 5 is 'Payments secret change'
	payments := e.addRun(runFix{project: "acme/payments", number: 5, head: head('p'), result: blockingResult(head('p')),
		outcome: OutcomeBlocking, title: "Payments secret change"})
	if err := e.st.PutRunLog(ctx, payments, "payments log"); err != nil {
		t.Fatal(err)
	}
	e.gl.setProject("acme/widgets", gitlabProject("private", 20, 0))
	e.gl.setProject("acme/payments", gitlabProject("private", 10, 0)) // Guest: no code, no merge requests
	alice := e.alice("main")
	team := e.team("devs", store.RoleViewer, []int64{alice},
		store.TeamRepo{Instance: "main", Project: "acme/widgets"}, store.TeamRepo{Instance: "main", Project: "acme/payments"})
	c := e.signIn("gitlab", "main")
	coversPayments(t, e, c, payments)

	// Covered later: the team change brings a check of payments, which
	// GitLab still refuses.
	e.editTeam(team, func(tm *store.Team) { tm.Repos = tm.Repos[1:] }) // acme/widgets only
	other := e.team("payments", store.RoleMaintainer, []int64{alice})
	if _, se := e.session(c); se.CanSee("main", "acme/payments") || !se.CanSee("main", "acme/widgets") {
		t.Fatalf("session = %+v", se)
	}
	checks := e.gl.projectChecks()
	e.editTeam(other, func(tm *store.Team) { tm.Repos = []store.TeamRepo{{Instance: "main", Project: "acme/payments"}} })
	coversPayments(t, e, c, payments)
	if e.gl.projectChecks() == checks {
		t.Error("adding acme/payments to a team made no platform check")
	}
}

// Team changes apply at the user's next request, without waiting for the
// 5-minute access check: a member removed sees nothing of the team's
// repositories, and a repository added is checked with the platform and
// shown at once.
func TestTeamChangeAppliesNextRequest(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 20, 0))
	e.gl.setProject("acme/payments", gitlabProject("private", 20, 0))
	alice := e.alice("main")
	widgets := e.team("widgets", store.RoleViewer, []int64{alice}, store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	// A second team, covering only gh: alice is on a team, not waiting.
	other := e.team("github", store.RoleViewer, []int64{alice}, store.TeamRepo{Instance: "gh"})
	c := e.signIn("gitlab", "main")
	if repos := getJSON[[]RepositoryView](e, c, "/api/repositories"); len(repos) != 1 || repos[0].Project != "acme/widgets" {
		t.Fatalf("repositories = %+v", repos)
	}

	e.editTeam(widgets, func(tm *store.Team) { tm.Members = nil })
	if repos := getJSON[[]RepositoryView](e, c, "/api/repositories"); len(repos) != 0 {
		t.Fatalf("repositories after leaving the team = %+v; want none", repos)
	}
	e.wantStatus(c, "/api/repositories/main/acme/widgets", http.StatusNotFound)

	checks := e.gl.projectChecks()
	e.editTeam(other, func(tm *store.Team) {
		tm.Repos = append(tm.Repos, store.TeamRepo{Instance: "main", Project: "acme/payments"})
	})
	repos := getJSON[[]RepositoryView](e, c, "/api/repositories")
	if len(repos) != 1 || repos[0].Project != "acme/payments" {
		t.Fatalf("repositories after adding acme/payments = %+v", repos)
	}
	if e.gl.projectChecks() == checks {
		t.Error("the added repository was shown without asking GitLab")
	}
	if se, _ := e.storedSession(c); !se.AccessCheckedAt.Equal(e.clock.now()) {
		t.Errorf("access checked at %v; want now", se.AccessCheckedAt)
	}
	// A role change needs no new check: the same repositories are covered.
	checks = e.gl.projectChecks()
	e.editTeam(other, func(tm *store.Team) { tm.Role = store.RoleMaintainer })
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/payments"); v.Role != "maintainer" {
		t.Errorf("role after the change = %q", v.Role)
	}
	if n := e.gl.projectChecks() - checks; n != 0 {
		t.Errorf("a role change made %d platform checks", n)
	}
}

// A maintainer changes the settings of the repositories their teams cover,
// and can't switch them off; a viewer changes nothing.
func TestMaintainerCanEditSettings(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.gl.setProject("acme/gadgets", gitlabProject("private", 30, 0))
	team := e.team("gadgets", store.RoleMaintainer, []int64{e.alice("main")}, store.TeamRepo{Instance: "main", Project: "acme/gadgets"})
	c := e.signIn("gitlab", "main")
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
	wantCode(t, e.get(c, "/api/repositories/available"), "maintainer picker", http.StatusForbidden)
	if _, found := e.storedRepo("main", "acme/gadgets"); !found {
		t.Fatal("the maintainer switched acme/gadgets off")
	}

	e.editTeam(team, func(tm *store.Team) { tm.Role = store.RoleViewer })
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

// An admin team's member is an admin, not an operator, and sees every
// repository switched on for their instance that the platform shows them,
// as admin; never one it hides.
func TestAdminTeamIsAdmin(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/hidden", nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 20, 0))
	e.gl.setProject("acme/payments", gitlabProject("internal", 30, 0))
	e.aliceOn("main", store.RoleAdmin)
	c := e.signIn("gitlab", "main")
	if _, me := e.me(c); !me.Admin || me.Operator || me.Waiting {
		t.Fatalf("/api/me = %+v; want admin, not operator", me)
	}
	repos := getJSON[[]RepositoryView](e, c, "/api/repositories")
	var got []string
	for _, r := range repos {
		got = append(got, r.Instance+"/"+r.Project)
	}
	if !slices.Equal(got, []string{"main/acme/widgets", "main/acme/payments"}) {
		t.Errorf("repositories = %v; want main's two the platform shows", got)
	}
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/payments"); v.Role != "admin" {
		t.Errorf("role = %q, want admin", v.Role)
	}
	e.wantStatus(c, "/api/repositories/main/acme/hidden", http.StatusNotFound)
	e.wantStatus(c, "/api/repositories/gh/acme/widgets", http.StatusNotFound)
	wantCode(t, e.send(c, "DELETE", "/api/repositories/main/acme/hidden", ""), "DELETE the hidden one", http.StatusNotFound)
	e.wantStatus(c, "/api/integrations", http.StatusOK)
}

// A password session is the operator: it reads the repositories, reviews,
// runs and setup problems of every instance, and never asks a platform.
func TestOperatorSeesEverything(t *testing.T) {
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
	if _, me := e.me(c); !me.Operator || !me.Admin || me.Waiting || me.Kind != "password" {
		t.Fatalf("/api/me = %+v", me)
	}

	repos := getJSON[[]RepositoryView](e, c, "/api/repositories")
	var got []string
	for _, r := range repos {
		got = append(got, r.Instance+"/"+r.Project)
	}
	if !slices.Equal(got, []string{"main/acme/widgets", "main/acme/payments", "gh/acme/widgets"}) {
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
	if n := e.gl.api() + e.gh.api() + e.gl.calls() + e.gh.calls(); n != 0 {
		t.Errorf("%d platform calls for an operator", n)
	}
}

// The picker and switching on ask the platform with the user's token,
// which a password admin hasn't got.
func TestOperatorCannotUsePicker(t *testing.T) {
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets")
	e.addPasswordAccount("ops", "ops-password-123", false)
	c := e.signInPassword("ops", "ops-password-123")
	const want = `{"error":"sign in with GitLab or GitHub to add repositories"}`
	if rec := e.get(c, "/api/repositories/available"); rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("GET available = %d %s; want 400 %s", rec.Code, rec.Body, want)
	}
	if rec := e.send(c, "POST", "/api/repositories", `{"project":"acme/gadgets"}`); rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("POST /api/repositories = %d %s; want 400 %s", rec.Code, rec.Body, want)
	}
	if n := e.cat["main"].callCount(); n != 0 {
		t.Errorf("the review bot's repositories were listed %d times", n)
	}
	if _, found := e.storedRepo("main", "acme/gadgets"); found {
		t.Error("acme/gadgets was switched on")
	}
}

// Admins see how many platform accounts wait for a team; nobody else does.
func TestMeWaitingUsers(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("ops", "ops-password-123", false)
	op := e.signInPassword("ops", "ops-password-123")
	waiting := e.signIn("gitlab", "main") // alice on main, on no team
	bob, err := e.st.UpsertPlatformAccount(ctx, config.PlatformGitHub, "gh", 70, "bob", e.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, me := e.me(op); me.WaitingUsers != 2 {
		t.Errorf("operator's waitingUsers = %d, want 2", me.WaitingUsers)
	}
	if _, me := e.me(waiting); me.WaitingUsers != 0 {
		t.Errorf("a waiting user's waitingUsers = %d, want 0", me.WaitingUsers)
	}
	viewers := e.team("viewers", store.RoleViewer, []int64{bob.ID}, store.TeamRepo{Instance: "gh"})
	if _, me := e.me(op); me.WaitingUsers != 1 {
		t.Errorf("waitingUsers with bob on a team = %d, want 1", me.WaitingUsers)
	}
	e.editTeam(viewers, func(tm *store.Team) { tm.Members = append(tm.Members, e.accountOf(waiting)) })
	if _, me := e.me(waiting); me.Waiting || me.Admin || me.WaitingUsers != 0 {
		t.Errorf("a viewer's /api/me = %+v; want no count", me)
	}
	e.team("admins", store.RoleAdmin, []int64{e.accountOf(waiting)})
	carol, err := e.st.UpsertPlatformAccount(ctx, config.PlatformGitLab, "main", 30, "carol", e.clock.now())
	if err != nil || carol.ID == 0 {
		t.Fatal(err)
	}
	if _, me := e.me(waiting); !me.Admin || me.WaitingUsers != 1 {
		t.Errorf("a platform admin's /api/me = %+v; want admin and carol waiting", me)
	}
	// A platform admin counts only their own instance's; an operator all.
	if _, err := e.st.UpsertPlatformAccount(ctx, config.PlatformGitHub, "gh", 71, "dan", e.clock.now()); err != nil {
		t.Fatal(err)
	}
	if _, me := e.me(waiting); me.WaitingUsers != 1 {
		t.Errorf("main's admin's waitingUsers with dan waiting on gh = %d, want 1 (carol)", me.WaitingUsers)
	}
	if _, me := e.me(op); me.WaitingUsers != 2 {
		t.Errorf("operator's waitingUsers = %d, want 2 (carol and dan)", me.WaitingUsers)
	}
}

// A repository taken off a team is gone at the next request even when the
// platform can't be asked: the last check's list is used only where the
// teams still cover.
func TestTeamRemovalAppliesWhenPlatformDown(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 20, 0))
	e.gl.setProject("acme/payments", gitlabProject("private", 20, 0))
	team := e.team("devs", store.RoleViewer, []int64{e.alice("main")}, store.TeamRepo{Instance: "main"})
	c := e.signIn("gitlab", "main")
	if _, se := e.session(c); !se.CanSee("main", "acme/payments") {
		t.Fatalf("session = %+v", se)
	}
	e.gl.set(func(o *oauthServer) { o.failAPI = true })
	e.editTeam(team, func(tm *store.Team) { tm.Repos = []store.TeamRepo{{Instance: "main", Project: "acme/widgets"}} })
	_, se := e.session(c)
	if se.CanSee("main", "acme/payments") || !se.CanSee("main", "acme/widgets") {
		t.Errorf("with GitLab down after the change: visible %v; want acme/widgets only", se.Visible)
	}
	if repos := getJSON[[]RepositoryView](e, c, "/api/repositories"); len(repos) != 1 || repos[0].Project != "acme/widgets" {
		t.Errorf("repositories = %+v", repos)
	}
}

// A session whose access was never checked, and whose check fails, has no
// role at all: an admin team gives nothing the platform hasn't vouched for.
func TestNeverCheckedFailsClosed(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	e.aliceOn("main", store.RoleAdmin)
	c := e.signIn("gitlab", "main")
	e.exec(`UPDATE sessions SET access_checked_at = NULL`)
	e.gl.set(func(o *oauthServer) { o.failAPI = true })
	if code, se := e.session(c); code != 200 || se.Admin || len(se.Visible) != 0 || len(se.Roles) != 0 {
		t.Fatalf("never checked, check failing: %d %+v; want no admin, nothing visible", code, se)
	}
	e.wantStatus(c, "/api/admin/settings", http.StatusForbidden)
	e.gl.set(func(o *oauthServer) { o.failAPI = false })
	e.clock.advance(retryAfterFailure + time.Second)
	if _, se := e.session(c); !se.Admin || !se.CanSee("main", "acme/widgets") {
		t.Fatalf("after a good check: %+v", se)
	}
}
