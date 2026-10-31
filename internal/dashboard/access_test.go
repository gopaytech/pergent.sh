package dashboard

import (
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

func TestAdminByGroupAndTeam(t *testing.T) {
	t.Run("gitlab group member", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gl.setMember("acme/platform", true)
		if _, me := e.me(e.signIn("gitlab", "main")); !me.IsAdmin {
			t.Fatal("a member of acme/platform is not admin")
		}
	})
	t.Run("gitlab not a member", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gl.setMember("acme/other", true)
		if _, me := e.me(e.signIn("gitlab", "main")); me.IsAdmin {
			t.Fatal("a non-member is admin")
		}
	})
	t.Run("github active team member", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gh.setTeam("acme/platform", "active")
		if _, me := e.me(e.signIn("github", "gh")); !me.IsAdmin {
			t.Fatal("an active member of acme/platform is not admin")
		}
	})
	t.Run("github pending invitation", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gh.setTeam("acme/platform", "pending")
		if _, me := e.me(e.signIn("github", "gh")); me.IsAdmin {
			t.Fatal("a pending invitation made an admin")
		}
	})
	t.Run("admin rights are per instance", func(t *testing.T) {
		// acme/platform is the admin group of another GitLab instance; on
		// main the admins are acme/admins. A member of acme/platform on
		// main is no admin there: the same group path on another
		// instance is another group.
		e := newEnv(t, func(c *config.Config) {
			c.GitLab = append(c.GitLab, config.Instance{Name: "other", URL: "https://gitlab.example.com"})
			c.Dashboard.Admins = []config.DashboardAdmin{
				{Instance: "other", Group: "acme/platform"},
				{Instance: "main", Group: "acme/admins"},
			}
		})
		e.gl.setMember("acme/platform", true)
		if _, me := e.me(e.signIn("gitlab", "main")); me.IsAdmin {
			t.Fatal("admin on main through another instance's group")
		}
		e.gl.setMember("acme/admins", true)
		e.clock.advance(AccessTTL + time.Second)
		if _, me := e.me(e.signIn("gitlab", "main")); !me.IsAdmin {
			t.Fatal("a member of main's own admin group is not admin")
		}
	})
	t.Run("gitlab member through a parent or shared group only", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gl.set(func(*oauthServer) { e.gl.inherited["acme/platform"] = true })
		if _, me := e.me(e.signIn("gitlab", "main")); me.IsAdmin {
			t.Fatal("a member of a parent or shared group only is admin")
		}
	})
	t.Run("gitlab minimal access", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gl.setMember("acme/platform", true)
		e.gl.set(func(*oauthServer) { e.gl.member = map[string]any{"access_level": 5} })
		if _, me := e.me(e.signIn("gitlab", "main")); me.IsAdmin {
			t.Fatal("a member with Minimal Access is admin")
		}
	})
	t.Run("gitlab member awaiting approval", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gl.setMember("acme/platform", true)
		e.gl.set(func(*oauthServer) { e.gl.member = map[string]any{"membership_state": "awaiting"} })
		if _, me := e.me(e.signIn("gitlab", "main")); me.IsAdmin {
			t.Fatal("a member awaiting approval is admin")
		}
	})
	t.Run("membership lost", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gl.setMember("acme/platform", true)
		c := e.signIn("gitlab", "main")
		e.gl.setMember("acme/platform", false)
		e.clock.advance(5*time.Minute + time.Second)
		if _, me := e.me(c); me.IsAdmin {
			t.Fatal("still admin 5 minutes after leaving the group")
		}
	})
}

// An App that may not read the admin team (403) makes nobody an admin on
// that instance, but signs users in; the problem is a warning and an
// instance-wide "Needs attention" item, gone once the team can be read.
func TestGitHubAdminTeamForbidden(t *testing.T) {
	e := newEnv(t, nil)
	e.gh.setRepo("acme/widgets", githubRepo(true, true))
	e.gh.setTeam("acme/platform", "active")
	e.gh.set(func(*oauthServer) { e.gh.teamStatus = http.StatusForbidden })
	c := e.signIn("github", "gh")
	if _, se := e.session(c); se.IsAdmin || !se.CanSee("acme/widgets") {
		t.Fatalf("session with an unreadable admin team = %+v; want acme/widgets, no admin", se)
	}
	logs := e.logs.String()
	if !containsAll(logs, []string{"level=WARN", "admin team", "instance=gh", "team=acme/platform"}) {
		t.Fatalf("no warning names the instance and team:\n%s", logs)
	}
	for _, tok := range e.gh.tokens() {
		if strings.Contains(logs, tok) {
			t.Fatal("the warning holds the user's token")
		}
	}
	items, err := e.st.Attention(ctx)
	if err != nil || len(items) != 1 || items[0].Instance != "gh" || items[0].Project != "" ||
		items[0].Kind != attentionAdminTeam || !strings.Contains(items[0].Message, "acme/platform") {
		t.Fatalf("attention = %+v, %v; want gh's instance-wide %s item", items, err, attentionAdminTeam)
	}

	e.gh.set(func(*oauthServer) { e.gh.teamStatus = 0 })
	e.clock.advance(AccessTTL + time.Second)
	if _, se := e.session(c); !se.IsAdmin {
		t.Fatal("not admin once the team can be read")
	}
	if items, err := e.st.Attention(ctx); err != nil || len(items) != 0 {
		t.Fatalf("attention after a good check = %+v, %v; want none", items, err)
	}
}

// Any other failure of the admin check (here 500s until the call's time is
// up) fails the check: no sign-in, nothing stored.
func TestGitHubAdminTeamErrorFailsClosed(t *testing.T) {
	e := newEnv(t, nil)
	e.d.Auth.providers["gh"].(*githubProvider).timeout = 1500 * time.Millisecond
	e.gh.setRepo("acme/widgets", githubRepo(true, true))
	e.gh.set(func(*oauthServer) { e.gh.teamStatus = http.StatusInternalServerError })
	loc, sc := e.startLogin("github", "gh")
	rec := e.callback("github", "gh", goodCode, loc.Query().Get("state"), sc)
	if rec.Code == http.StatusSeeOther || cookie(rec, SessionCookieName) != nil {
		t.Fatalf("callback = %d with a session; want a failed sign-in", rec.Code)
	}
	if items, err := e.st.Attention(ctx); err != nil || len(items) != 0 {
		t.Fatalf("attention = %+v, %v; want none", items, err)
	}
}

// Review Focus 1: a user whose access drops from Developer to Guest stops
// seeing the repository within 5 minutes.
func TestAccessRefreshDropsRepository(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	e.gl.setProject("acme/payments", gitlabProject("private", 20, 0))
	c := e.signIn("gitlab", "main")

	_, se := e.session(c)
	if !se.CanSee("acme/widgets") || !se.CanSee("acme/payments") || se.CanSee("acme/other") {
		t.Fatalf("visible after sign-in = %v", se.Visible)
	}

	e.gl.setProject("acme/widgets", gitlabProject("private", 10, 0)) // now Guest
	e.clock.advance(4 * time.Minute)
	if _, se := e.session(c); !se.CanSee("acme/widgets") {
		t.Fatal("dropped before the 5-minute refresh: the cache is not used")
	}
	e.clock.advance(time.Minute + time.Second)
	_, se = e.session(c)
	if se.CanSee("acme/widgets") || !se.CanSee("acme/payments") {
		t.Fatalf("visible 5 min after the drop = %v", se.Visible)
	}
	projects, err := e.st.SessionProjects(ctx, hashOf(t, c))
	if err != nil || !slices.Equal(projects, []store.VisibleProject{{Path: "acme/payments", ID: testID("acme/payments")}}) {
		t.Fatalf("stored user_access = %v, %v", projects, err)
	}

	// Only the session's instance is checked: gh's acme/widgets is never
	// in a GitLab session.
	if len(se.Visible) != 1 {
		t.Fatalf("visible = %v", se.Visible)
	}
}

// A failed refresh keeps the last list for one more period, then denies
// everything, and access comes back once a check succeeds.
func TestRefreshFailureFailsClosedAfterOnePeriod(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	e.gl.setMember("acme/platform", true)
	c := e.signIn("gitlab", "main")

	e.gl.set(func(o *oauthServer) { o.failAPI = true })
	e.clock.advance(6 * time.Minute)
	if _, se := e.session(c); !se.CanSee("acme/widgets") || !se.IsAdmin {
		t.Fatalf("within one more period: %+v", se)
	}
	e.clock.advance(4*time.Minute + time.Second) // 10 min + 1 s after the last good check
	code, se := e.session(c)
	if code != 200 || len(se.Visible) != 0 || se.IsAdmin {
		t.Fatalf("after two periods without a check: %d %+v", code, se)
	}
	if _, me := e.me(c); me.IsAdmin || me.Username != "alice" {
		t.Fatalf("/api/me after failing closed = %+v; want alice, no admin", me)
	}
	stored, _ := e.storedSession(c)
	projects, _ := e.st.SessionProjects(ctx, hashOf(t, c))
	if stored.IsAdmin || len(projects) != 0 {
		t.Fatalf("stored access not cleared: admin %v, projects %v", stored.IsAdmin, projects)
	}

	e.gl.set(func(o *oauthServer) { o.failAPI = false })
	e.clock.advance(time.Minute + time.Second)
	if _, se := e.session(c); !se.CanSee("acme/widgets") || !se.IsAdmin {
		t.Fatalf("after the platform is back: %+v", se)
	}
}

// GitHub user tokens of an App expire; pergent.sh refreshes them, and a
// refused refresh fails closed like any failed check.
func TestExpiredTokenRefreshFailsClosed(t *testing.T) {
	e := newEnv(t, nil)
	e.gh.set(func(o *oauthServer) { o.expiresIn = 120 })
	e.gh.setRepo("acme/widgets", githubRepo(true, true))
	c := e.signIn("github", "gh")
	first := e.gh.tokens()

	// Refreshed: the new token is used, the old one stops working.
	e.clock.advance(5*time.Minute + time.Second)
	if _, se := e.session(c); !se.CanSee("acme/widgets") {
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
	if _, se := e.session(c); !se.CanSee("acme/widgets") {
		t.Fatalf("within one more period: %+v", se)
	}
	e.clock.advance(4*time.Minute + time.Second)
	if _, se := e.session(c); len(se.Visible) != 0 || se.IsAdmin {
		t.Fatalf("after a refused refresh and two periods: %+v", se)
	}
}

// Concurrent requests with a stale list check access once.
func TestConcurrentRefreshChecksOnce(t *testing.T) {
	e := newEnv(t, nil)
	e.gl.setProject("acme/widgets", gitlabProject("private", 30, 0))
	c := e.signIn("gitlab", "main")
	calls := e.gl.calls()
	checks := e.gl.projectChecks()
	e.clock.advance(5*time.Minute + time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, se := e.session(c); code != 200 || !se.CanSee("acme/widgets") {
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

// A renamed GitHub login is never used again for the team check: each
// access check reads /user afresh. Someone else may take the old login.
func TestRenamedUserChecksCurrentLogin(t *testing.T) {
	e := newEnv(t, nil)
	e.gh.setRepo("acme/widgets", githubRepo(true, true))
	e.gh.setTeam("acme/platform", "active")
	c := e.signIn("github", "gh")
	if _, me := e.me(c); !me.IsAdmin || me.Username != "alice" {
		t.Fatalf("before the rename: %+v", me)
	}

	// alice renames herself bob; someone else takes "alice" and is in
	// the admin team, bob isn't.
	e.gh.setLogin("bob")
	e.gh.setTeamFor("acme/platform", "bob", "")
	e.clock.advance(AccessTTL + time.Second)
	_, me := e.me(c)
	if me.IsAdmin {
		t.Fatal("admin through the old login")
	}
	if me.Username != "bob" {
		t.Errorf("username = %q, want bob", me.Username)
	}
	if se, _ := e.storedSession(c); se.Username != "bob" {
		t.Errorf("stored username = %q, want bob", se.Username)
	}
}

// GitLab checks admin by user ID, but the username shown follows a rename
// too.
func TestRenamedGitLabUserUpdated(t *testing.T) {
	e := newEnv(t, nil)
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
			"isAdmin": func() error {
				_, err := p.isAdmin(ctx, "token", 9, "alice", config.DashboardAdmin{Group: "acme/platform", Team: "acme/platform"})
				return err
			},
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
	if got := d.Auth.providers["main"].(*gitlabProvider).timeout; got != 10*time.Second {
		t.Errorf("gitlab call timeout = %s", got)
	}
	if got := d.Auth.providers["gh"].(*githubProvider).timeout; got != 10*time.Second {
		t.Errorf("github call timeout = %s", got)
	}
}
