package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// fakeCatalog is the review bot's or App's list of repositories.
type fakeCatalog struct {
	mu     sync.Mutex
	repos  []string
	fail   error
	calls  int // ReachableRepos and CanReach calls
	checks int // CanReach calls
	slow   time.Duration

	// info is what LinkInfo says, by lower-case path; infoErr what it fails
	// with; infoCalls how many times it was asked, by lower-case path.
	info      map[string]linkInfo
	infoErr   map[string]error
	infoCalls map[string]int
}

func (f *fakeCatalog) set(repos ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = repos
}

func (f *fakeCatalog) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

func (f *fakeCatalog) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCatalog) checkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

func (f *fakeCatalog) ReachableRepos(ctx context.Context, search string, limit int) ([]string, bool, error) {
	f.mu.Lock()
	f.calls++
	repos, fail, slow := slices.Clone(f.repos), f.fail, f.slow
	f.mu.Unlock()
	if fail != nil {
		return nil, false, fail
	}
	var out []string
	for _, r := range repos {
		if !strings.Contains(strings.ToLower(r), strings.ToLower(search)) {
			continue
		}
		if len(out) == limit {
			return out, true, nil
		}
		if slow > 0 && len(out) > 0 {
			select {
			case <-time.After(slow):
			case <-ctx.Done():
				return out, false, ctx.Err()
			}
		}
		out = append(out, r)
	}
	return out, false, nil
}

func (f *fakeCatalog) CanReach(_ context.Context, project string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.checks++
	if f.fail != nil {
		return false, f.fail
	}
	return slices.ContainsFunc(f.repos, func(r string) bool { return strings.EqualFold(r, project) }), nil
}

// send makes a write as the session c, with its CSRF token, and records the
// response.
func (e *env) send(c *http.Cookie, method, target, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	code, me := e.me(c)
	if code != http.StatusOK {
		e.t.Fatalf("/api/me = %d", code)
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.AddCookie(c)
	req.Header.Set(CSRFHeader, me.CSRFToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	e.mu.Lock()
	e.responses = append(e.responses, rec)
	e.mu.Unlock()
	return rec
}

func (e *env) dashboardRepo(instance, project string, edit func(*store.DashboardRepo)) {
	e.t.Helper()
	r := store.DashboardRepo{Instance: instance, Project: project, CreatedBy: "carol"}
	if edit != nil {
		edit(&r)
	}
	if err := e.st.UpsertDashboardRepo(ctx, r); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) storedRepo(instance, project string) (store.DashboardRepo, bool) {
	e.t.Helper()
	r, found, err := e.st.AnyDashboardRepo(ctx, instance, project)
	if err != nil {
		e.t.Fatal(err)
	}
	return r, found
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, what string, code int) {
	t.Helper()
	if rec.Code != code {
		t.Errorf("%s = %d %s; want %d", what, rec.Code, rec.Body, code)
	}
}

// writes are every write endpoint for project on instance.
func writes(instance, project string) []struct{ method, target, body string } {
	base := "/api/repositories/" + instance + "/" + project
	return []struct{ method, target, body string }{
		{"POST", "/api/repositories", `{"instance":"` + instance + `","project":"` + project + `"}`},
		{"DELETE", base, ""},
		{"PUT", base + "/settings", `{"approve":false}`},
		{"POST", base + "/settings/reset", ""},
	}
}

// Review Focus 2: a viewer calling a write endpoint directly, with a valid
// session and CSRF token, gets 404 on the switch-on, switch-off and picker
// (an admin's routes) and 403 on a repository's settings, and nothing changes.
func TestWritesRequireAdmin(t *testing.T) {
	t.Parallel()
	for _, instance := range []string{"main", "gh"} {
		t.Run(instance, func(t *testing.T) {
			e := newEnv(t, nil)
			e.dashboardRepo(instance, "acme/gadgets", func(r *store.DashboardRepo) { r.Approve = ptr(true) })
			e.cat[instance].set("acme/gadgets", "acme/tools")
			c := e.userOn(instance, false, "acme/gadgets")
			if _, se := e.session(c); se.Admin || se.Role(instance, "acme/gadgets") != RoleViewer {
				t.Fatalf("session = %+v; want a viewer who sees acme/gadgets", se)
			}

			wantCode(t, e.get(c, "/api/repositories/available?instance="+instance), "GET available", http.StatusNotFound)
			for _, w := range append(writes(instance, "acme/gadgets"),
				struct{ method, target, body string }{"POST", "/api/repositories", `{"instance":"` + instance + `","project":"acme/tools"}`}) {
				want := http.StatusForbidden // a repository's own settings: its maintainers
				if w.target == "/api/repositories" || w.method == "DELETE" {
					want = http.StatusNotFound // an admin's route
				}
				wantCode(t, e.send(c, w.method, w.target, w.body), w.method+" "+w.target, want)
			}

			r, found := e.storedRepo(instance, "acme/gadgets")
			if !found || r.Approve == nil || !*r.Approve {
				t.Errorf("acme/gadgets changed: %+v, %v", r, found)
			}
			if _, found := e.storedRepo(instance, "acme/tools"); found {
				t.Errorf("acme/tools was switched on")
			}
			if n := e.reconciles.Load(); n != 0 {
				t.Errorf("ReconcileSoon called %d times", n)
			}
			if n := e.cat[instance].callCount(); n != 0 {
				t.Errorf("the bot's repositories were listed %d times", n)
			}
		})
	}
}

// Switching a repository on is for admins: a maintainer of every repository,
// and a viewer, get 403 on the picker and on the switch-on, and the review
// bot is never asked. An SSO admin, who has no platform account, switches one
// on; the bot's reach is the only check.
func TestAddRepositoryNeedsAdmin(t *testing.T) {
	t.Parallel()
	for _, role := range []string{store.RoleMaintainer, store.RoleViewer} {
		t.Run(role, func(t *testing.T) {
			e := newEnv(t, nil)
			e.cat["main"].set("acme/tools")
			c := e.signInRole(role)
			// A viewer maintains no team: 404. A maintainer passes that gate
			// and meets the instance's switch, off here: 403.
			want := http.StatusNotFound
			if role == store.RoleMaintainer {
				want = http.StatusForbidden
			}
			avail := e.get(c, "/api/repositories/available?instance=main")
			post := e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/tools"}`)
			wantCode(t, avail, "GET available", want)
			wantCode(t, post, "POST", want)
			if want == http.StatusForbidden {
				for _, rec := range []*httptest.ResponseRecorder{avail, post} {
					if !strings.Contains(rec.Body.String(), msgTeamsMayNotOnboard) {
						t.Errorf("body = %s, want %q", rec.Body, msgTeamsMayNotOnboard)
					}
				}
			}
			if _, found := e.storedRepo("main", "acme/tools"); found {
				t.Error("someone who is not an admin switched a repository on")
			}
			if n := e.cat["main"].callCount(); n != 0 {
				t.Errorf("the bot was asked %d times for someone who is not an admin", n)
			}
			if n := e.reconciles.Load(); n != 0 {
				t.Errorf("ReconcileSoon called %d times", n)
			}
		})
	}

	e := newEnv(t, nil)
	e.cat["main"].set("acme/tools")
	boss := e.sessionFor(e.ssoAdmin("boss@example.com"))
	wantCode(t, e.send(boss, "POST", "/api/repositories", `{"instance":"main","project":"acme/tools"}`), "an SSO admin switching on", http.StatusCreated)
	if r, found := e.storedRepo("main", "acme/tools"); !found || r.CreatedBy != "boss@example.com" {
		t.Errorf("stored repository = %+v, %v; want switched on by boss@example.com", r, found)
	}
	// Not what the bot doesn't reach.
	wantCode(t, e.send(boss, "POST", "/api/repositories", `{"instance":"main","project":"acme/hidden"}`), "a repository the bot can't reach", http.StatusNotFound)
}

// A switch-on and a picker listing name their instance: none is guessed, and an
// instance that isn't there is 404. Nothing asks the bot first.
func TestAddRepositoryNeedsInstance(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.cat["main"].set("acme/tools")
	c := e.userOnMain(true)
	rec := e.send(c, "POST", "/api/repositories", `{"project":"acme/tools"}`)
	wantCode(t, rec, "POST without an instance", http.StatusBadRequest)
	if got := errorOf(t, rec); got != msgNameInstance {
		t.Errorf("error = %q; want %q", got, msgNameInstance)
	}
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"instance":"nope","project":"acme/tools"}`), "POST on an unknown instance", http.StatusNotFound)
	wantCode(t, e.get(c, "/api/repositories/available"), "GET available without an instance", http.StatusBadRequest)
	wantCode(t, e.get(c, "/api/repositories/available?instance=nope"), "GET available on an unknown instance", http.StatusNotFound)
	if _, found := e.storedRepo("main", "acme/tools"); found {
		t.Error("a switch-on without a valid instance stored a repository")
	}
	if n := e.cat["main"].callCount(); n != 0 {
		t.Errorf("the bot was asked %d times before the instance was known", n)
	}
	// With the instance it works.
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/tools"}`), "POST with the instance", http.StatusCreated)
}

// Review Focus 3: a user calling a write endpoint for a repository their
// teams don't cover gets 404, the same answer as for one that doesn't exist.
func TestUserCannotWriteUncoveredRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/hidden", func(r *store.DashboardRepo) { r.Approve = ptr(true) })
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.dashboardRepo("gh", "acme/other", nil)
	// A maintainer of acme/gadgets alone.
	e.team("gadgets", []store.TeamMember{{AccountID: e.alice(), Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "main", Project: "acme/gadgets"})
	c := e.aliceSession()
	if _, se := e.session(c); se.Admin || se.CanSee("main", "acme/hidden") || se.Role("main", "acme/gadgets") != RoleMaintainer {
		t.Fatalf("session = %+v; want a maintainer who can't see acme/hidden", se)
	}
	unknown := e.send(c, "PUT", "/api/repositories/main/acme/nothing-here/settings", `{"approve":false}`)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("PUT of an unknown repository = %d %s", unknown.Code, unknown.Body)
	}
	for _, w := range [][3]string{
		{"PUT", "/api/repositories/main/acme/hidden/settings", `{"approve":false}`},
		{"POST", "/api/repositories/main/acme/hidden/settings/reset", ""},
		{"PUT", "/api/repositories/gh/acme/other/settings", `{"approve":false}`},
		{"POST", "/api/repositories/gh/acme/other/settings/reset", ""},
	} {
		rec := e.send(c, w[0], w[1], w[2])
		wantCode(t, rec, w[0]+" "+w[1], http.StatusNotFound)
		if rec.Body.String() != unknown.Body.String() {
			t.Errorf("%s %s body = %s, want the unknown repository's %s", w[0], w[1], rec.Body, unknown.Body)
		}
	}
	// Switching on and off is for admins (and, once teams may onboard, owner
	// maintainers): with the switch off, 403.
	for _, w := range []struct{ method, target, body string }{
		{"DELETE", "/api/repositories/main/acme/gadgets", ""}, {"DELETE", "/api/repositories/main/acme/hidden", ""},
	} {
		// alice maintains a team, and the switch is off: 403 on both.
		rec := e.send(c, w.method, w.target, w.body)
		wantCode(t, rec, w.method+" "+w.target, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), msgTeamsMayNotOnboard) {
			t.Errorf("%s %s body = %s, want %q", w.method, w.target, rec.Body, msgTeamsMayNotOnboard)
		}
	}
	if r, found := e.storedRepo("main", "acme/hidden"); !found || r.Approve == nil || !*r.Approve {
		t.Errorf("acme/hidden changed: %+v, %v", r, found)
	}
	if _, found := e.storedRepo("gh", "acme/other"); !found {
		t.Errorf("acme/other on gh was switched off")
	}
	if n := e.reconciles.Load(); n != 0 {
		t.Errorf("ReconcileSoon called %d times", n)
	}
}

// A repository the config lists is read-only here: every write is 409. So
// is switching on one that is on already.
func TestConfigManagedIs409(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.cat["main"].set("acme/widgets", "acme/gadgets")
	c := e.userOnMain(true)

	for _, w := range writes("main", "acme/widgets") {
		rec := e.send(c, w.method, w.target, w.body)
		wantCode(t, rec, w.method+" "+w.target, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s %s body = %s", w.method, w.target, rec.Body)
		}
	}
	rec := e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"ACME/Gadgets"}`)
	wantCode(t, rec, "POST of a repository already on", http.StatusConflict)
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/widgets/settings", `{"maxTurns":120}`), "PUT maxTurns", http.StatusConflict)

	if _, found := e.storedRepo("main", "acme/widgets"); found {
		t.Errorf("a dashboard row was made for the config repository")
	}
	if n := e.reconciles.Load(); n != 0 {
		t.Errorf("ReconcileSoon called %d times", n)
	}
}

// Settings are validated like config; null resets a field; unknown fields
// and malformed bodies are refused. A refused change changes nothing.
func TestSettingsValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(cfg *config.Config) {
		cfg.Jobs.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	})
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.skill("style", "x")
	c := e.userOnMain(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"

	for _, tc := range []struct {
		body  string
		ok    bool
		check func(store.DashboardRepo) bool
	}{
		{`{"approve":false}`, true, func(r store.DashboardRepo) bool { return r.Approve != nil && !*r.Approve }},
		{`{"approve":"no"}`, false, nil},
		{`{"approve":null}`, true, func(r store.DashboardRepo) bool { return r.Approve == nil }},
		{`{"reviewDrafts":false}`, true, func(r store.DashboardRepo) bool { return r.ReviewDrafts != nil && !*r.ReviewDrafts }},
		{`{"reviewDrafts":0}`, false, nil},
		{`{"inlineFindings":false}`, true, func(r store.DashboardRepo) bool { return r.InlineFindings != nil && !*r.InlineFindings }},
		{`{"inlineFindings":"no"}`, false, nil},
		{`{"inlineFindings":null}`, true, func(r store.DashboardRepo) bool { return r.InlineFindings == nil }},
		{`{"commandsAuthorAndMaintainersOnly":false}`, true, func(r store.DashboardRepo) bool {
			return r.CommandsAuthorAndMaintainersOnly != nil && !*r.CommandsAuthorAndMaintainersOnly
		}},
		{`{"commandsAuthorAndMaintainersOnly":"no"}`, false, nil},
		{`{"commandsAuthorAndMaintainersOnly":null}`, true, func(r store.DashboardRepo) bool { return r.CommandsAuthorAndMaintainersOnly == nil }},
		{`{"incrementalReviews":false}`, true, func(r store.DashboardRepo) bool { return r.IncrementalReviews != nil && !*r.IncrementalReviews }},
		{`{"incrementalReviews":"no"}`, false, nil},
		{`{"incrementalReviews":null}`, true, func(r store.DashboardRepo) bool { return r.IncrementalReviews == nil }},
		{`{"learnFromDismissals":false}`, true, func(r store.DashboardRepo) bool { return r.LearnFromDismissals != nil && !*r.LearnFromDismissals }},
		{`{"learnFromDismissals":"no"}`, false, nil},
		{`{"learnFromDismissals":null}`, true, func(r store.DashboardRepo) bool { return r.LearnFromDismissals == nil }},
		{`{"quietReviews":true}`, true, func(r store.DashboardRepo) bool { return r.QuietReviews != nil && *r.QuietReviews }},
		// false is the override a repository needs when the admin default is on.
		{`{"quietReviews":false}`, true, func(r store.DashboardRepo) bool { return r.QuietReviews != nil && !*r.QuietReviews }},
		{`{"quietReviews":"no"}`, false, nil},
		{`{"quietReviews":null}`, true, func(r store.DashboardRepo) bool { return r.QuietReviews == nil }},
		{`{"checkDeleted":true}`, true, func(r store.DashboardRepo) bool { return r.CheckDeleted != nil && *r.CheckDeleted }},
		{`{"checkDeleted":false}`, true, func(r store.DashboardRepo) bool { return r.CheckDeleted != nil && !*r.CheckDeleted }},
		{`{"checkDeleted":"no"}`, false, nil},
		{`{"checkDeleted":null}`, true, func(r store.DashboardRepo) bool { return r.CheckDeleted == nil }},
		{`{"commentLayout":["findings:open","files:collapsed"]}`, true, func(r store.DashboardRepo) bool {
			return slices.Equal(r.CommentLayout, []string{"findings:open", "files:collapsed"})
		}},
		{`{"commentLayout":["summary:open"]}`, false, nil},
		{`{"commentLayout":[]}`, false, nil},
		{`{"commentLayout":"findings:open"}`, false, nil},
		{`{"commentLayout":null}`, true, func(r store.DashboardRepo) bool { return r.CommentLayout == nil }},
		{`{"failureStatus":"neutral"}`, true, func(r store.DashboardRepo) bool { return r.FailureStatus != nil && *r.FailureStatus == "neutral" }},
		{`{"failureStatus":"success"}`, false, nil},
		{`{"failureStatus":"failed"}`, true, func(r store.DashboardRepo) bool { return r.FailureStatus != nil && *r.FailureStatus == "failed" }},
		{`{"blockingStatus":"neutral"}`, true, func(r store.DashboardRepo) bool { return r.BlockingStatus != nil && *r.BlockingStatus == "neutral" }},
		{`{"blockingStatus":"success"}`, false, nil},
		{`{"blockingStatus":true}`, false, nil},
		{`{"blockingStatus":"failed"}`, true, func(r store.DashboardRepo) bool { return r.BlockingStatus != nil && *r.BlockingStatus == "failed" }},
		{`{"blockingStatus":null}`, true, func(r store.DashboardRepo) bool { return r.BlockingStatus == nil }},
		{`{"blockingStatus":"neutral"}`, true, func(r store.DashboardRepo) bool { return r.BlockingStatus != nil && *r.BlockingStatus == "neutral" }},
		{`{"skills":["style"]}`, true, func(r store.DashboardRepo) bool { return slices.Equal(r.Skills, []string{"style"}) }},
		{`{"skills":["style","nope"]}`, false, nil},
		{`{"skills":[]}`, false, nil},
		{`{"skills":"style"}`, false, nil},
		{`{"skills":["style",null]}`, false, nil},
		{`{"skills":null}`, true, func(r store.DashboardRepo) bool { return r.Skills == nil }},
		{`{"jobTimeoutSeconds":59}`, false, nil},
		{`{"jobTimeoutSeconds":7201}`, false, nil},
		{`{"jobTimeoutSeconds":90.5}`, false, nil},
		{`{"jobTimeoutSeconds":"600"}`, false, nil},
		{`{"jobTimeoutSeconds":60}`, true, func(r store.DashboardRepo) bool { return r.JobTimeoutSeconds != nil && *r.JobTimeoutSeconds == 60 }},
		{`{"jobTimeoutSeconds":7200}`, true, func(r store.DashboardRepo) bool { return r.JobTimeoutSeconds != nil && *r.JobTimeoutSeconds == 7200 }},
		{`{"maxTurns":4}`, false, nil},
		{`{"maxTurns":201}`, false, nil},
		{`{"maxTurns":40.5}`, false, nil},
		{`{"maxTurns":"40"}`, false, nil},
		{`{"maxTurns":5}`, true, func(r store.DashboardRepo) bool { return r.MaxTurns != nil && *r.MaxTurns == 5 }},
		{`{"maxTurns":120}`, true, func(r store.DashboardRepo) bool { return r.MaxTurns != nil && *r.MaxTurns == 120 }},
		{`{"maxTurns":200}`, true, func(r store.DashboardRepo) bool { return r.MaxTurns != nil && *r.MaxTurns == 200 }},
		{`{"maxTurns":null}`, true, func(r store.DashboardRepo) bool { return r.MaxTurns == nil }},
		{`{"jobMemory":"lots"}`, false, nil},
		{`{"jobMemory":"512Mi"}`, false, nil}, // below the 1Gi request
		{`{"jobMemory":"33Gi"}`, false, nil},
		{`{"jobMemory":"-2Gi"}`, false, nil},
		{`{"jobMemory":2048}`, false, nil},
		{`{"jobMemory":"32Gi"}`, true, func(r store.DashboardRepo) bool { return r.JobMemory != nil && *r.JobMemory == "32Gi" }},
		{`{"jobMemory":"1Gi"}`, true, func(r store.DashboardRepo) bool { return r.JobMemory != nil && *r.JobMemory == "1Gi" }},
		{`{"jobMemory":"1.5Gi"}`, true, func(r store.DashboardRepo) bool { return r.JobMemory != nil && *r.JobMemory == "1536Mi" }},
		{`{"rerunLimitPerMR":0}`, false, nil},
		{`{"rerunLimitPerMR":1001}`, false, nil},
		{`{"rerunLimitPerMR":2.5}`, false, nil},
		{`{"rerunLimitPerMR":"10"}`, false, nil},
		{`{"rerunLimitPerRepo":-1}`, false, nil},
		{`{"rerunLimitPerRepo":1001}`, false, nil},
		{`{"rerunLimitPerMR":1}`, true, func(r store.DashboardRepo) bool { return r.RerunLimitPerMR != nil && *r.RerunLimitPerMR == 1 }},
		{`{"rerunLimitPerMR":1000}`, true, func(r store.DashboardRepo) bool { return r.RerunLimitPerMR != nil && *r.RerunLimitPerMR == 1000 }},
		{`{"rerunLimitPerRepo":250}`, true, func(r store.DashboardRepo) bool { return r.RerunLimitPerRepo != nil && *r.RerunLimitPerRepo == 250 }},
		{`{"rerunLimitPerRepo":null}`, true, func(r store.DashboardRepo) bool { return r.RerunLimitPerRepo == nil }},
		{`{"jobTimeout":600}`, false, nil},
		{`{"approve":true,"extra":1}`, false, nil},
		{`{"approve":true`, false, nil},
		{`[]`, false, nil},
		{`null`, false, nil},
		{``, false, nil},
		{`{"approve":true} {}`, false, nil},
	} {
		before, _ := e.storedRepo("main", "acme/gadgets")
		rec := e.send(c, "PUT", target, tc.body)
		after, _ := e.storedRepo("main", "acme/gadgets")
		if !tc.ok {
			wantCode(t, rec, "PUT "+tc.body, http.StatusBadRequest)
			if !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("PUT %s changed the row", tc.body)
			}
			continue
		}
		wantCode(t, rec, "PUT "+tc.body, http.StatusOK)
		if !tc.check(after) {
			t.Errorf("PUT %s: row = %+v", tc.body, after)
		}
	}

	// A partial update keeps the other fields; the answer is the detail.
	rec := e.send(c, "PUT", target, `{"approve":false,"inlineFindings":false,"jobTimeoutSeconds":900,"maxTurns":120,"rerunLimitPerRepo":75}`)
	wantCode(t, rec, "PUT two fields", http.StatusOK)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Repo.Project != "acme/gadgets" || detail.Repo.Source != "dashboard" ||
		detail.Settings["approve"].Value != false || !detail.Settings["approve"].Overridden ||
		detail.Settings["inlineFindings"].Value != false || !detail.Settings["inlineFindings"].Overridden ||
		detail.Settings["jobTimeoutSeconds"].Value != float64(900) ||
		detail.Settings["maxTurns"].Value != float64(120) || !detail.Settings["maxTurns"].Overridden ||
		detail.Settings["jobMemory"].Value != "1536Mi" || !detail.Settings["jobMemory"].Overridden ||
		detail.Settings["skills"].Overridden || detail.Settings["skills"].Value != nil {
		t.Errorf("detail = %+v", detail)
	}
	// The caps: the one set is overridden, the other (last set to 1000 above) too.
	if s := detail.Settings["rerunLimitPerRepo"]; s.Value != float64(75) || !s.Overridden {
		t.Errorf("rerunLimitPerRepo = %+v, want 75 overridden", s)
	}
	if s := detail.Settings["rerunLimitPerMR"]; s.Value != float64(1000) || !s.Overridden {
		t.Errorf("rerunLimitPerMR = %+v, want 1000 overridden", s)
	}

	// Reset clears every override.
	rec = e.send(c, "POST", target+"/reset", "")
	wantCode(t, rec, "reset", http.StatusOK)
	r, found := e.storedRepo("main", "acme/gadgets")
	if !found || r.Approve != nil || r.ReviewDrafts != nil || r.FailureStatus != nil || r.BlockingStatus != nil || r.Skills != nil ||
		r.InlineFindings != nil || r.CommandsAuthorAndMaintainersOnly != nil || r.IncrementalReviews != nil || r.LearnFromDismissals != nil || r.QuietReviews != nil || r.CheckDeleted != nil || r.CommentLayout != nil || r.LinkedReposEnabled != nil || r.LinkedRepos != nil || r.JobTimeoutSeconds != nil || r.MaxTurns != nil || r.JobMemory != nil || r.RerunLimitPerMR != nil || r.RerunLimitPerRepo != nil ||
		r.CreatedBy != "carol" {
		t.Errorf("after reset row = %+v, %v", r, found)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	for k, s := range detail.Settings {
		if s.Overridden {
			t.Errorf("after reset %s is overridden", k)
		}
	}
	if s := detail.Settings["maxTurns"]; s.Value != float64(40) {
		t.Errorf("after reset maxTurns = %v, want the default 40", s.Value)
	}
	if s := detail.Settings["rerunLimitPerMR"]; s.Value != float64(10) {
		t.Errorf("after reset rerunLimitPerMR = %v, want the default 10", s.Value)
	}
	if s := detail.Settings["rerunLimitPerRepo"]; s.Value != float64(50) {
		t.Errorf("after reset rerunLimitPerRepo = %v, want the default 50", s.Value)
	}
	if n := e.reconciles.Load(); n != 0 {
		t.Errorf("settings changes called ReconcileSoon %d times", n)
	}
}

// Without jobs.resources.requests, the memory limit's floor is 256Mi.
func TestSettingsMemoryFloor(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"
	wantCode(t, e.send(c, "PUT", target, `{"jobMemory":"255Mi"}`), "255Mi", http.StatusBadRequest)
	wantCode(t, e.send(c, "PUT", target, `{"jobMemory":"256Mi"}`), "256Mi", http.StatusOK)
}

// Only admins change the re-run caps: a maintainer's PUT naming one (even to
// null) is 403 and changes nothing, their other settings still work, and
// their reset leaves the caps as stored. An admin sets and resets them.
func TestCapsNeedAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) {
		mr, repo := 7, 70
		r.RerunLimitPerMR, r.RerunLimitPerRepo = &mr, &repo
	})
	target := "/api/repositories/main/acme/gadgets/settings"
	e.aliceOn("main", store.RoleMaintainer)
	maintainer := e.aliceSession()

	for _, body := range []string{`{"rerunLimitPerMR":99}`, `{"rerunLimitPerRepo":null}`, `{"approve":false,"rerunLimitPerMR":5}`} {
		before, _ := e.storedRepo("main", "acme/gadgets")
		rec := e.send(maintainer, "PUT", target, body)
		wantCode(t, rec, "maintainer PUT "+body, http.StatusForbidden)
		if want := "only admins can change its re-run caps"; errorOf(t, rec) != want {
			t.Errorf("maintainer PUT %s: error = %q", body, errorOf(t, rec))
		}
		if after, _ := e.storedRepo("main", "acme/gadgets"); !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("maintainer PUT %s changed the row", body)
		}
	}

	// The step limit is a maintainer's setting, and their reset clears it.
	wantCode(t, e.send(maintainer, "PUT", target, `{"approve":false,"maxTurns":80}`), "maintainer PUT approve and maxTurns", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.MaxTurns == nil || *r.MaxTurns != 80 {
		t.Errorf("after a maintainer's PUT maxTurns = %v, want 80", r.MaxTurns)
	}
	wantCode(t, e.send(maintainer, "POST", target+"/reset", ""), "maintainer reset", http.StatusOK)
	r, _ := e.storedRepo("main", "acme/gadgets")
	if r.MaxTurns != nil {
		t.Errorf("after a maintainer's reset maxTurns = %d, want cleared", *r.MaxTurns)
	}
	if r.Approve != nil || r.RerunLimitPerMR == nil || *r.RerunLimitPerMR != 7 || r.RerunLimitPerRepo == nil || *r.RerunLimitPerRepo != 70 {
		t.Errorf("after a maintainer's reset row = %+v; want approve cleared, caps 7 and 70 kept", r)
	}

	admin := e.userOnMain(true) // the same alice, made an admin
	wantCode(t, e.send(admin, "PUT", target, `{"rerunLimitPerMR":15,"rerunLimitPerRepo":null}`), "admin PUT caps", http.StatusOK)
	r, _ = e.storedRepo("main", "acme/gadgets")
	if r.RerunLimitPerMR == nil || *r.RerunLimitPerMR != 15 || r.RerunLimitPerRepo != nil {
		t.Errorf("after an admin's PUT row = %+v", r)
	}
	wantCode(t, e.send(admin, "POST", target+"/reset", ""), "admin reset", http.StatusOK)
	if r, _ = e.storedRepo("main", "acme/gadgets"); r.RerunLimitPerMR != nil || r.RerunLimitPerRepo != nil {
		t.Errorf("after an admin's reset row = %+v; want the caps cleared", r)
	}
}

// Only admins change the two status settings: a maintainer's PUT naming
// either (even to null, even beside a setting they may change) is 403 and
// changes nothing, and their reset leaves both. An admin sets them, the audit
// detail names each with its new value, and an admin's reset clears them.
func TestStatusSettingsNeedAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) {
		neutral := "neutral"
		r.FailureStatus, r.BlockingStatus = &neutral, &neutral
	})
	target := "/api/repositories/main/acme/gadgets/settings"
	e.aliceOn("main", store.RoleMaintainer)
	maintainer := e.aliceSession()

	for _, body := range []string{`{"failureStatus":"failed"}`, `{"blockingStatus":"failed"}`, `{"blockingStatus":null}`, `{"failureStatus":null}`,
		`{"approve":false,"blockingStatus":"failed"}`} {
		before, _ := e.storedRepo("main", "acme/gadgets")
		rec := e.send(maintainer, "PUT", target, body)
		wantCode(t, rec, "maintainer PUT "+body, http.StatusForbidden)
		if want := "only an admin changes the status settings"; errorOf(t, rec) != want {
			t.Errorf("maintainer PUT %s: error = %q", body, errorOf(t, rec))
		}
		if after, _ := e.storedRepo("main", "acme/gadgets"); !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("maintainer PUT %s changed the row", body)
		}
	}
	// The maintainer still sees both values.
	v := getJSON[RepositoryDetailView](e, maintainer, "/api/repositories/main/acme/gadgets")
	if s := v.Settings["blockingStatus"]; s.Value != "neutral" || !s.Overridden {
		t.Errorf("maintainer's blockingStatus = %+v", s)
	}
	wantCode(t, e.send(maintainer, "PUT", target, `{"approve":false}`), "maintainer PUT approve", http.StatusOK)
	wantCode(t, e.send(maintainer, "POST", target+"/reset", ""), "maintainer reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.Approve != nil || r.FailureStatus == nil || *r.FailureStatus != "neutral" ||
		r.BlockingStatus == nil || *r.BlockingStatus != "neutral" {
		t.Errorf("after a maintainer's reset row = %+v; want approve cleared, both statuses kept", r)
	}

	admin := e.userOnMain(true) // the same alice, made an admin
	wantCode(t, e.send(admin, "PUT", target, `{"blockingStatus":"failed","failureStatus":null}`), "admin PUT statuses", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.BlockingStatus == nil || *r.BlockingStatus != "failed" || r.FailureStatus != nil {
		t.Errorf("after an admin's PUT row = %+v", r)
	}
	var detail string
	for _, a := range e.auditEntries() {
		if a.Action == "settings_updated" {
			detail = a.Detail
			break
		}
	}
	if want := "blockingStatus=failed, failureStatus=default"; detail != want {
		t.Errorf("audit detail = %q, want %q", detail, want)
	}
	wantCode(t, e.send(admin, "PUT", target, `{"blockingStatus":"neutral"}`), "admin PUT neutral", http.StatusOK)
	wantCode(t, e.send(admin, "POST", target+"/reset", ""), "admin reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.BlockingStatus != nil || r.FailureStatus != nil {
		t.Errorf("after an admin's reset row = %+v; want both statuses cleared", r)
	}
}

// The caps' range error names the setting and the range.
func TestSettingsCapMessages(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"
	for key, body := range map[string]string{"rerunLimitPerMR": `{"rerunLimitPerMR":0}`, "rerunLimitPerRepo": `{"rerunLimitPerRepo":1001}`} {
		rec := e.send(c, "PUT", target, body)
		wantCode(t, rec, body, http.StatusBadRequest)
		if want := key + " must be a whole number from 1 to 1000, or null"; errorOf(t, rec) != want {
			t.Errorf("PUT %s: error = %q, want %q", body, errorOf(t, rec), want)
		}
	}
}

// Switching a repository on makes its dashboard record, asks the engine to
// reconcile, and the admin sees it straight away; switching it off deletes
// the record and asks again.
func TestSwitchOnTriggersReconcile(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets")
	c := e.userOnMain(true)

	rec := e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"ACME/Gadgets"}`)
	wantCode(t, rec, "POST", http.StatusCreated)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	// The project is spelled as the admin gave it: the bot's list is matched
	// ignoring case, and nothing asks the platform for its spelling.
	if detail.Repo.Project != "ACME/Gadgets" || detail.Repo.Source != "dashboard" || !detail.Repo.On ||
		detail.Repo.Instance != "main" || detail.Settings["approve"].Overridden || detail.Role != "admin" {
		t.Errorf("detail = %+v", detail)
	}
	r, found := e.storedRepo("main", "ACME/Gadgets")
	if !found || r.Project != "ACME/Gadgets" || r.CreatedBy != aliceEmail || r.Approve != nil || r.Skills != nil {
		t.Errorf("row = %+v, %v", r, found)
	}
	if n := e.reconciles.Load(); n != 1 {
		t.Errorf("ReconcileSoon called %d times, want 1", n)
	}
	e.wantStatus(c, "/api/repositories/main/acme/gadgets", http.StatusOK)

	rec = e.send(c, "DELETE", "/api/repositories/main/acme/gadgets", "")
	wantCode(t, rec, "DELETE", http.StatusOK)
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Repo.On || detail.Repo.Project != "ACME/Gadgets" {
		t.Errorf("detail after DELETE = %+v", detail)
	}
	if _, found := e.storedRepo("main", "acme/gadgets"); found {
		t.Errorf("the row is still there")
	}
	if n := e.reconciles.Load(); n != 2 {
		t.Errorf("ReconcileSoon called %d times, want 2", n)
	}
	// Each names the repository switched, as the dashboard spells it.
	e.mu.Lock()
	defer e.mu.Unlock()
	if want := []string{"main/ACME/Gadgets", "main/ACME/Gadgets"}; !slices.Equal(e.reconciled, want) {
		t.Errorf("ReconcileSoon got %v, want %v", e.reconciled, want)
	}
}

// The client can't switch on a repository the review bot or App doesn't
// reach: the server checks again, against the bot, whatever the picker listed.
func TestSwitchOnOnlyAvailable(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets", "acme/bot-only")
	c := e.userOnMain(true)

	unknown := e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/nothing-here"}`)
	wantCode(t, unknown, "POST unknown", http.StatusNotFound)
	for _, body := range []string{`{"instance":"main"}`, `{"instance":"main","project":""}`, `{"instance":"main","project":1}`,
		`{"instance":"main","project":"acme/gadgets","x":1}`, `{"project":"acme/gadgets"}`, `nope`} {
		wantCode(t, e.send(c, "POST", "/api/repositories", body), "POST "+body, http.StatusBadRequest)
	}
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"instance":"nope","project":"acme/gadgets"}`), "POST an unknown instance", http.StatusNotFound)
	if _, found := e.storedRepo("main", "acme/nothing-here"); found {
		t.Error("an unreachable repository was switched on")
	}
	if n := e.reconciles.Load(); n != 0 {
		t.Errorf("ReconcileSoon called %d times", n)
	}

	// The bot can't be asked: 502, nothing switched on.
	e.cat["main"].setFail(errors.New("bot down"))
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/gadgets"}`), "POST while the bot fails", http.StatusBadGateway)
	e.cat["main"].setFail(nil)
	if _, found := e.storedRepo("main", "acme/gadgets"); found {
		t.Errorf("acme/gadgets was switched on")
	}
	// A repository the bot reaches is enough, on either platform.
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/bot-only"}`), "POST a repository only the bot reaches", http.StatusCreated)
	e.cat["gh"].set("acme/gadgets")
	rec := e.send(c, "POST", "/api/repositories", `{"instance":"gh","project":"acme/gadgets"}`)
	wantCode(t, rec, "POST on GitHub", http.StatusCreated)
	if r, found := e.storedRepo("gh", "acme/gadgets"); !found || r.Project != "acme/gadgets" {
		t.Errorf("row = %+v, %v", r, found)
	}
}

// Switching on doesn't depend on the picker's lists: a repository beyond
// what the list reads can be added.
func TestSwitchOnBeyondListingCap(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.d.listLimit = 2
	var all []string
	for i := range 5 {
		all = append(all, fmt.Sprintf("acme/repo-%d", i))
	}
	e.cat["main"].set(all...)
	c := e.userOnMain(true)
	got := getJSON[availableBody](e, c, "/api/repositories/available?instance=main")
	if len(got.Items) != 2 || !got.Truncated {
		t.Fatalf("available = %v truncated %v; want the first 2, truncated", projectsOf(got.Items), got.Truncated)
	}
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/repo-4"}`), "POST beyond the cap", http.StatusCreated)
}

// Whether a repository is on is told only for one the bot reaches: one that is
// on but out of the bot's reach gets the same 404, after the same check, as
// one that is off.
func TestSwitchOnChecksBeforeTellingOnOrOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/hidden", nil)
	e.cat["main"].set("acme/reachable")
	c := e.userOnMain(true)

	for _, p := range []string{"acme/hidden", "acme/off"} {
		before := e.cat["main"].checkCount()
		rec := e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"`+p+`"}`)
		wantCode(t, rec, "POST "+p, http.StatusNotFound)
		if rec.Body.String() != `{"error":"not found"}`+"\n" {
			t.Errorf("POST %s body = %q", p, rec.Body)
		}
		if n := e.cat["main"].checkCount() - before; n != 1 {
			t.Errorf("POST %s asked the bot %d times, want 1", p, n)
		}
	}
}

type availableBody struct {
	Items     []AvailableRepo `json:"items"`
	Truncated bool            `json:"truncated"`
}

func projectsOf(items []AvailableRepo) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Project)
	}
	return out
}

// The picker lists what the review bot or App of the instance named reaches,
// with the state of each, for any admin.
func TestAvailableListsWhatTheBotReaches(t *testing.T) {
	t.Parallel()
	t.Run("gitlab", func(t *testing.T) {
		e := newEnv(t, nil)
		e.dashboardRepo("main", "acme/dash", nil)
		e.cat["main"].set("acme/gadgets", "acme/tools", "acme/widgets", "acme/dash", "Acme/Zeta")
		c := e.userOnMain(true)

		got := getJSON[availableBody](e, c, "/api/repositories/available?instance=main")
		if want := []string{"Acme/Zeta", "acme/dash", "acme/gadgets", "acme/tools", "acme/widgets"}; !slices.Equal(projectsOf(got.Items), want) || got.Truncated {
			t.Errorf("available = %v truncated %v, want %v (what is on too)", projectsOf(got.Items), got.Truncated, want)
		}
		for _, it := range got.Items {
			state := stateOff
			if it.Project == "acme/dash" || it.Project == "acme/widgets" {
				state = stateOn
			}
			if it.Instance != "main" || it.Platform != "gitlab" || it.State != state {
				t.Errorf("item = %+v; want state %s", it, state)
			}
		}

		// The bot's list is kept 5 minutes.
		getJSON[availableBody](e, c, "/api/repositories/available?instance=main")
		if n := e.cat["main"].callCount(); n != 1 {
			t.Errorf("bot listed %d times within 5 minutes, want 1", n)
		}
		e.clock.advance(5*time.Minute + time.Second)
		getJSON[availableBody](e, c, "/api/repositories/available?instance=main")
		if n := e.cat["main"].callCount(); n != 2 {
			t.Errorf("bot listed %d times after 5 minutes, want 2", n)
		}
	})

	t.Run("github", func(t *testing.T) {
		e := newEnv(t, nil)
		e.cat["gh"].set("acme/gadgets", "acme/widgets")
		c := e.userOnGH(true)

		got := getJSON[availableBody](e, c, "/api/repositories/available?instance=gh")
		if want := []string{"acme/gadgets", "acme/widgets"}; !slices.Equal(projectsOf(got.Items), want) || got.Truncated {
			t.Errorf("available = %v truncated %v, want %v", projectsOf(got.Items), got.Truncated, want)
		}
		if len(got.Items) == 2 && (got.Items[0].Instance != "gh" || got.Items[0].Platform != "github" ||
			got.Items[0].State != stateOff || got.Items[1].State != stateOn) {
			t.Errorf("items = %+v; want gadgets off and widgets on", got.Items)
		}
		// The instance is the request's: main's list is not gh's.
		if got := getJSON[availableBody](e, c, "/api/repositories/available?instance=main"); len(got.Items) != 0 {
			t.Errorf("main's available = %v; want none (its bot reaches nothing)", projectsOf(got.Items))
		}
	})

	t.Run("50 a page", func(t *testing.T) {
		e := newEnv(t, nil)
		var all []string
		for i := range 600 {
			all = append(all, fmt.Sprintf("acme/repo-%03d", i))
		}
		e.cat["main"].set(all...)
		c := e.userOnMain(true)
		got := getJSON[AvailableView](e, c, "/api/repositories/available?instance=main")
		if len(got.Items) != 50 || got.Page != 1 || !got.HasMore || got.Truncated || got.Items[0].Project != "acme/repo-000" {
			t.Errorf("available has %d items, page %d, hasMore %v, truncated %v", len(got.Items), got.Page, got.HasMore, got.Truncated)
		}
		got = getJSON[AvailableView](e, c, "/api/repositories/available?instance=main&page=12")
		if len(got.Items) != 50 || got.Page != 12 || got.HasMore || got.Items[0].Project != "acme/repo-550" {
			t.Errorf("page 12 has %d items from %v, hasMore %v", len(got.Items), projectsOf(got.Items[:1]), got.HasMore)
		}
		wantCode(t, e.get(c, "/api/repositories/available?instance=main&page=0"), "page 0", http.StatusBadRequest)
	})

	t.Run("the bot fails", func(t *testing.T) {
		e := newEnv(t, nil)
		c := e.userOnMain(true)
		e.cat["main"].setFail(errors.New("gitlab: status 500: SECRET"))
		rec := e.get(c, "/api/repositories/available?instance=main")
		wantCode(t, rec, "available while the bot fails", http.StatusBadGateway)
		if strings.Contains(rec.Body.String(), "500") || strings.Contains(rec.Body.String(), "SECRET") {
			t.Errorf("body leaks upstream detail: %s", rec.Body)
		}
	})

	t.Run("an instance that is not there", func(t *testing.T) {
		e := newEnv(t, nil)
		c := e.userOnMain(true)
		wantCode(t, e.get(c, "/api/repositories/available?instance=nope"), "unknown instance", http.StatusNotFound)
		wantCode(t, e.get(c, "/api/repositories/available"), "no instance", http.StatusBadRequest)
	})
}

// No write answer ever holds a token.
func TestWriteAnswersHoldNoSecret(t *testing.T) {
	e := newEnv(t, withSecrets(t))
	e.cat["main"].set("acme/gadgets")
	c := e.userOnMain(true)
	e.get(c, "/api/repositories/available?instance=main")
	e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/gadgets"}`)
	e.send(c, "PUT", "/api/repositories/main/acme/gadgets/settings", `{"jobMemory":"lots"}`)
	e.send(c, "PUT", "/api/repositories/main/acme/gadgets/settings", `{"approve":false}`)
	e.send(c, "DELETE", "/api/repositories/main/acme/gadgets", "")
	secrets := []string{llmKey, reviewToken, approveToken, webhookSecret}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range e.responses {
		for _, s := range secrets {
			if strings.Contains(rec.Body.String(), s) {
				t.Errorf("a response holds %q: %s", s, rec.Body)
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }

// A write without the CSRF token is refused before anything else, admin or
// not.
func TestWriteNeedsCSRF(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets")
	c := e.userOnMain(true)
	req := httptest.NewRequest("POST", "/api/repositories", strings.NewReader(`{"instance":"main","project":"acme/gadgets"}`))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	wantCode(t, rec, "POST without CSRF", http.StatusForbidden)
	if _, found := e.storedRepo("main", "acme/gadgets"); found || e.reconciles.Load() != 0 {
		t.Errorf("switched on without a CSRF token")
	}
}

// ?search= finds repositories beyond the first ones listed: the bot's list is
// searched.
func TestAvailableSearch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.d.listLimit = 3
	var all []string
	for i := range 10 {
		all = append(all, fmt.Sprintf("acme/repo-%d", i))
	}
	e.cat["main"].set(all...)
	c := e.userOnMain(true)
	got := getJSON[availableBody](e, c, "/api/repositories/available?instance=main&search=REPO-9")
	if !slices.Equal(projectsOf(got.Items), []string{"acme/repo-9"}) || got.Truncated {
		t.Errorf("search = %v truncated %v", projectsOf(got.Items), got.Truncated)
	}
	wantCode(t, e.get(c, "/api/repositories/available?instance=main&search="+strings.Repeat("x", 101)), "long search", http.StatusBadRequest)
}

// The bot's listing has an overall budget (20 s): when it runs out, what was
// read is listed, truncated; with nothing read it is 502.
func TestAvailableListingBudget(t *testing.T) {
	t.Parallel()
	if listBudget != 20*time.Second || platformTimeout != 10*time.Second || AvailableBudget != 45*time.Second || listBudget >= AvailableBudget {
		t.Errorf("listBudget %v, platformTimeout %v, AvailableBudget %v; want 20s, 10s and 45s", listBudget, platformTimeout, AvailableBudget)
	}
	e := newEnv(t, nil)
	var all []string
	for i := range 150 {
		all = append(all, fmt.Sprintf("acme/repo-%03d", i))
	}
	e.cat["main"].set(all...)
	c := e.userOnMain(true)
	e.d.listBudget = 300 * time.Millisecond

	e.cat["main"].mu.Lock()
	e.cat["main"].slow = 200 * time.Millisecond
	e.cat["main"].mu.Unlock()
	got := getJSON[availableBody](e, c, "/api/repositories/available?instance=main")
	if len(got.Items) == 0 || len(got.Items) >= 150 || !got.Truncated {
		t.Errorf("available has %d items, truncated %v; want some, truncated", len(got.Items), got.Truncated)
	}

	// Nothing read in time: 502.
	e.clock.advance(catalogTTL)
	e.cat["main"].setFail(errors.New("down"))
	wantCode(t, e.get(c, "/api/repositories/available?instance=main&search=repo"), "nothing read", http.StatusBadGateway)
}

// The whole picker request has its own budget (45 s), below the server's write
// timeout. The listing runs detached from the request that started it, within
// listBudget, so a request that waits for a listing already under way is the
// one the budget cuts: it answers 502 when the budget is gone, and does not
// wait for the listing, which finishes and is kept for the next request.
func TestAvailableRequestBudget(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.cat["main"].set("acme/a", "acme/b", "acme/c", "acme/d", "acme/e")
	c := e.userOnMain(true)
	e.d.availableBudget = 200 * time.Millisecond
	e.d.listBudget = time.Minute
	e.cat["main"].mu.Lock()
	e.cat["main"].slow = 600 * time.Millisecond // 2.4 s for five repositories
	e.cat["main"].mu.Unlock()

	leader := make(chan *httptest.ResponseRecorder, 1)
	go func() { leader <- e.get(c, "/api/repositories/available?instance=main") }()
	deadline := time.Now().Add(5 * time.Second)
	for e.cat["main"].callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first listing never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	rec := e.get(c, "/api/repositories/available?instance=main")
	wantCode(t, rec, "a request that waits for a listing under way", http.StatusBadGateway)
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("the waiting request took %v; want about the 200ms budget, not the listing's 2.4 s", took)
	}
	if got := errorOf(t, rec); got != msgListFailed {
		t.Errorf("error = %q; want the fixed text", got)
	}

	// The listing was not cut: it finishes, answers its own request and is kept.
	first := <-leader
	wantCode(t, first, "the request that started the listing", http.StatusOK)
	var body availableBody
	if err := json.Unmarshal(first.Body.Bytes(), &body); err != nil || len(body.Items) != 5 {
		t.Errorf("the first request listed %d items (%v); want all 5", len(body.Items), err)
	}
	e.d.availableBudget = AvailableBudget
	if got := getJSON[availableBody](e, c, "/api/repositories/available?instance=main"); len(got.Items) != 5 {
		t.Errorf("the next request listed %d items; want 5", len(got.Items))
	}
	if n := e.cat["main"].callCount(); n != 1 {
		t.Errorf("the bot listed %d times; want once, kept for the next request", n)
	}
}

// The step limit takes a whole number from 5 to 200 or null, and nothing
// else; this needs no database.
func TestMaxTurnsSetting(t *testing.T) {
	const bad = "maxTurns must be a whole number from 5 to 200, or null"
	for _, tc := range []struct {
		body string
		want *int
		msg  string
	}{
		{"5", ptr(5), ""},
		{"40", ptr(40), ""},
		{"200", ptr(200), ""},
		{"null", nil, ""},
		{"4", nil, bad},
		{"201", nil, bad},
		{"0", nil, bad},
		{"-40", nil, bad},
		{"40.5", nil, bad},
		{`"40"`, nil, bad},
		{"true", nil, bad},
		{"[40]", nil, bad},
	} {
		edit, msg := maxTurnsSetting(json.RawMessage(tc.body), tc.body == "null")
		if msg != tc.msg {
			t.Errorf("%s: message = %q, want %q", tc.body, msg, tc.msg)
			continue
		}
		if msg != "" {
			continue
		}
		row := store.DashboardRepo{MaxTurns: ptr(99)}
		edit(&row)
		if !samePtr(row.MaxTurns, tc.want) {
			t.Errorf("%s: MaxTurns = %v, want %v", tc.body, row.MaxTurns, tc.want)
		}
	}
}

// A defaults body with maxTurns stores it and null clears it; the audit
// names it when it changes.
func TestParseDefaultsMaxTurns(t *testing.T) {
	d := &Dashboard{}
	parse := func(body string) (store.RepoDefaults, string) {
		next, _, msg := d.parseDefaults(httptest.NewRequest("PUT", "/api/admin/defaults", strings.NewReader(body)), nil, nil)
		return next, msg
	}
	if next, msg := parse(`{"maxTurns":80}`); msg != "" || next.MaxTurns == nil || *next.MaxTurns != 80 {
		t.Errorf("maxTurns 80: %+v, %q", next, msg)
	}
	if next, msg := parse(`{"maxTurns":null}`); msg != "" || next.MaxTurns != nil {
		t.Errorf("maxTurns null: %+v, %q", next, msg)
	}
	if next, msg := parse(`{"approve":false}`); msg != "" || next.MaxTurns != nil {
		t.Errorf("maxTurns left out: %+v, %q", next, msg)
	}
	if _, msg := parse(`{"maxTurns":201}`); msg != "maxTurns must be a whole number from 5 to 200, or null" {
		t.Errorf("maxTurns 201: %q", msg)
	}
	if _, msg := parse(`{"maxTurn":80}`); !strings.Contains(msg, "maxTurns") {
		t.Errorf("a wrong key: %q; want the message to list maxTurns", msg)
	}

	a, b := store.RepoDefaults{}, store.RepoDefaults{MaxTurns: ptr(80)}
	if got := changedDefaults(a, b); !slices.Equal(got, []string{"maxTurns"}) {
		t.Errorf("changedDefaults = %q, want maxTurns", got)
	}
	if got := changedDefaults(b, b); len(got) != 0 {
		t.Errorf("changedDefaults of equal defaults = %q", got)
	}
}

// blockingStatus is "failed", "neutral" or null in a repository's settings
// and in the defaults; both status keys are the admins', and the audit names
// each with its new value. No database here.
func TestParseBlockingStatus(t *testing.T) {
	d := &Dashboard{}
	for _, tc := range []struct {
		body string
		want *string
		msg  string
	}{
		{`{"blockingStatus":"neutral"}`, ptr("neutral"), ""},
		{`{"blockingStatus":"failed"}`, ptr("failed"), ""},
		{`{"blockingStatus":null}`, nil, ""},
		{`{"blockingStatus":"success"}`, nil, `blockingStatus must be "failed", "neutral" or null`},
		{`{"blockingStatus":1}`, nil, `blockingStatus must be "failed", "neutral" or null`},
	} {
		edit, keys, msg := d.parseSettings(httptest.NewRequest("PUT", "/x", strings.NewReader(tc.body)))
		if msg != tc.msg {
			t.Errorf("settings %s: msg = %q, want %q", tc.body, msg, tc.msg)
		}
		if msg == "" {
			row := store.DashboardRepo{BlockingStatus: ptr("x")}
			edit(&row)
			if !samePtr(row.BlockingStatus, tc.want) || !slices.Equal(keys, []string{"blockingStatus"}) {
				t.Errorf("settings %s: BlockingStatus = %v, keys %q", tc.body, row.BlockingStatus, keys)
			}
		}
		next, _, msg := d.parseDefaults(httptest.NewRequest("PUT", "/x", strings.NewReader(tc.body)), nil, nil)
		if msg != tc.msg || msg == "" && !samePtr(next.BlockingStatus, tc.want) {
			t.Errorf("defaults %s: %v, %q", tc.body, next.BlockingStatus, msg)
		}
	}
	for key, want := range map[string]bool{"failureStatus": true, "blockingStatus": true, "approve": false, "rerunLimitPerMR": false} {
		if isStatusKey(key) != want {
			t.Errorf("isStatusKey(%q) = %v", key, !want)
		}
	}
	got := changedDefaults(store.RepoDefaults{}, store.RepoDefaults{BlockingStatus: ptr("neutral"), FailureStatus: ptr("neutral")})
	if !slices.Equal(got, []string{"failureStatus=neutral", "blockingStatus=neutral"}) {
		t.Errorf("changedDefaults = %q", got)
	}
	got = changedDefaults(store.RepoDefaults{BlockingStatus: ptr("neutral")}, store.RepoDefaults{})
	if !slices.Equal(got, []string{"blockingStatus=default"}) {
		t.Errorf("changedDefaults back to the default = %q", got)
	}
	if got := statusDetail("blockingStatus", nil); got != "blockingStatus=default" {
		t.Errorf("statusDetail = %q", got)
	}
}

// Inline findings is stored on its own: true or false, null back to the
// default (on); a reset clears it; anything else is 400 with its message.
func TestInlineFindingsSetting(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"

	rec := e.send(c, "PUT", target, `{"inlineFindings":false}`)
	wantCode(t, rec, "PUT false", http.StatusOK)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if s := detail.Settings["inlineFindings"]; s.Value != false || !s.Overridden {
		t.Errorf("inlineFindings = %+v, want false overridden", s)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.InlineFindings == nil || *r.InlineFindings {
		t.Errorf("stored = %v, want false", r.InlineFindings)
	}
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || g.InlineFindings {
		t.Errorf("effective = %+v, %v; want inline findings off", g, err)
	}

	rec = e.send(c, "PUT", target, `{"inlineFindings":"no"}`)
	wantCode(t, rec, "PUT no", http.StatusBadRequest)
	if got := errorOf(t, rec); got != "inlineFindings must be true, false or null" {
		t.Errorf("error = %q", got)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.InlineFindings == nil || *r.InlineFindings {
		t.Errorf("a refused PUT changed the row: %v", r.InlineFindings)
	}

	rec = e.send(c, "PUT", target, `{"inlineFindings":null}`)
	wantCode(t, rec, "PUT null", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.InlineFindings != nil {
		t.Errorf("stored after null = %v, want NULL", r.InlineFindings)
	}
	if g, _, _ := e.d.Repos.Get(ctx, "main", "acme/gadgets"); !g.InlineFindings {
		t.Errorf("effective after null = %+v; want the default, on", g)
	}

	e.send(c, "PUT", target, `{"inlineFindings":false}`)
	wantCode(t, e.send(c, "POST", target+"/reset", ""), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.InlineFindings != nil {
		t.Errorf("stored after reset = %v, want NULL", r.InlineFindings)
	}
}

// The switch for commands is stored on its own: true or false, null back to
// the default (on); a reset clears it; anything else is 400 with its message.
func TestCommandsAuthorAndMaintainersOnlySetting(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"

	rec := e.send(c, "PUT", target, `{"commandsAuthorAndMaintainersOnly":false}`)
	wantCode(t, rec, "PUT false", http.StatusOK)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if s := detail.Settings["commandsAuthorAndMaintainersOnly"]; s.Value != false || !s.Overridden {
		t.Errorf("commandsAuthorAndMaintainersOnly = %+v, want false overridden", s)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CommandsAuthorAndMaintainersOnly == nil || *r.CommandsAuthorAndMaintainersOnly {
		t.Errorf("stored = %v, want false", r.CommandsAuthorAndMaintainersOnly)
	}
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || g.CommandsAuthorAndMaintainersOnly {
		t.Errorf("effective = %+v, %v; want the switch off", g, err)
	}

	rec = e.send(c, "PUT", target, `{"commandsAuthorAndMaintainersOnly":"x"}`)
	wantCode(t, rec, "PUT x", http.StatusBadRequest)
	if got := errorOf(t, rec); got != "commandsAuthorAndMaintainersOnly must be true, false or null" {
		t.Errorf("error = %q", got)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CommandsAuthorAndMaintainersOnly == nil || *r.CommandsAuthorAndMaintainersOnly {
		t.Errorf("a refused PUT changed the row: %v", r.CommandsAuthorAndMaintainersOnly)
	}

	rec = e.send(c, "PUT", target, `{"commandsAuthorAndMaintainersOnly":null}`)
	wantCode(t, rec, "PUT null", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CommandsAuthorAndMaintainersOnly != nil {
		t.Errorf("stored after null = %v, want NULL", r.CommandsAuthorAndMaintainersOnly)
	}
	if g, _, _ := e.d.Repos.Get(ctx, "main", "acme/gadgets"); !g.CommandsAuthorAndMaintainersOnly {
		t.Errorf("effective after null = %+v; want the default, on", g)
	}

	e.send(c, "PUT", target, `{"commandsAuthorAndMaintainersOnly":false}`)
	wantCode(t, e.send(c, "POST", target+"/reset", ""), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CommandsAuthorAndMaintainersOnly != nil {
		t.Errorf("stored after reset = %v, want NULL", r.CommandsAuthorAndMaintainersOnly)
	}
	if !strings.Contains(msgBadSettings, "commandsAuthorAndMaintainersOnly") {
		t.Errorf("msgBadSettings = %q", msgBadSettings)
	}
}

// Incremental reviews is stored on its own: true or false, null back to the
// default (on); a reset clears it; anything else is 400 with its message.
func TestIncrementalReviewsSetting(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"

	rec := e.send(c, "PUT", target, `{"incrementalReviews":false}`)
	wantCode(t, rec, "PUT false", http.StatusOK)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if s := detail.Settings["incrementalReviews"]; s.Value != false || !s.Overridden {
		t.Errorf("incrementalReviews = %+v, want false overridden", s)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.IncrementalReviews == nil || *r.IncrementalReviews {
		t.Errorf("stored = %v, want false", r.IncrementalReviews)
	}
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || g.IncrementalReviews {
		t.Errorf("effective = %+v, %v; want incremental reviews off", g, err)
	}

	rec = e.send(c, "PUT", target, `{"incrementalReviews":"x"}`)
	wantCode(t, rec, "PUT x", http.StatusBadRequest)
	if got := errorOf(t, rec); got != "incrementalReviews must be true, false or null" {
		t.Errorf("error = %q", got)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.IncrementalReviews == nil || *r.IncrementalReviews {
		t.Errorf("a refused PUT changed the row: %v", r.IncrementalReviews)
	}

	rec = e.send(c, "PUT", target, `{"incrementalReviews":null}`)
	wantCode(t, rec, "PUT null", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.IncrementalReviews != nil {
		t.Errorf("stored after null = %v, want NULL", r.IncrementalReviews)
	}
	if g, _, _ := e.d.Repos.Get(ctx, "main", "acme/gadgets"); !g.IncrementalReviews {
		t.Errorf("effective after null = %+v; want the default, on", g)
	}

	e.send(c, "PUT", target, `{"incrementalReviews":false}`)
	wantCode(t, e.send(c, "POST", target+"/reset", ""), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.IncrementalReviews != nil {
		t.Errorf("stored after reset = %v, want NULL", r.IncrementalReviews)
	}
	if !strings.Contains(msgBadSettings, "incrementalReviews") {
		t.Errorf("msgBadSettings = %q", msgBadSettings)
	}
}

// Learn from dismissals is stored on its own: true or false, null back to the
// default (on); a reset clears it; anything else is 400 with its message.
func TestLearnFromDismissalsSetting(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"

	rec := e.send(c, "PUT", target, `{"learnFromDismissals":false}`)
	wantCode(t, rec, "PUT false", http.StatusOK)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if s := detail.Settings["learnFromDismissals"]; s.Value != false || !s.Overridden {
		t.Errorf("learnFromDismissals = %+v, want false overridden", s)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.LearnFromDismissals == nil || *r.LearnFromDismissals {
		t.Errorf("stored = %v, want false", r.LearnFromDismissals)
	}
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || g.LearnFromDismissals {
		t.Errorf("effective = %+v, %v; want learn from dismissals off", g, err)
	}

	rec = e.send(c, "PUT", target, `{"learnFromDismissals":"x"}`)
	wantCode(t, rec, "PUT x", http.StatusBadRequest)
	if got := errorOf(t, rec); got != "learnFromDismissals must be true, false or null" {
		t.Errorf("error = %q", got)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.LearnFromDismissals == nil || *r.LearnFromDismissals {
		t.Errorf("a refused PUT changed the row: %v", r.LearnFromDismissals)
	}

	rec = e.send(c, "PUT", target, `{"learnFromDismissals":null}`)
	wantCode(t, rec, "PUT null", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.LearnFromDismissals != nil {
		t.Errorf("stored after null = %v, want NULL", r.LearnFromDismissals)
	}
	if g, _, _ := e.d.Repos.Get(ctx, "main", "acme/gadgets"); !g.LearnFromDismissals {
		t.Errorf("effective after null = %+v; want the default, on", g)
	}

	e.send(c, "PUT", target, `{"learnFromDismissals":false}`)
	wantCode(t, e.send(c, "POST", target+"/reset", ""), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.LearnFromDismissals != nil {
		t.Errorf("stored after reset = %v, want NULL", r.LearnFromDismissals)
	}
	if !strings.Contains(msgBadSettings, "learnFromDismissals") {
		t.Errorf("msgBadSettings = %q", msgBadSettings)
	}
}

// Quiet reviews is stored on its own: true or false, null back to the default
// (off); a reset clears it; anything else is 400 with its message. A
// maintainer may change it, the audit entry names it, and a repository the
// config manages refuses it.
func TestQuietReviewsSetting(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"

	rec := e.send(c, "PUT", target, `{"quietReviews":true}`)
	wantCode(t, rec, "PUT true", http.StatusOK)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if s := detail.Settings["quietReviews"]; s.Value != true || !s.Overridden {
		t.Errorf("quietReviews = %+v, want true overridden", s)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.QuietReviews == nil || !*r.QuietReviews {
		t.Errorf("stored = %v, want true", r.QuietReviews)
	}
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || !g.QuietReviews {
		t.Errorf("effective = %+v, %v; want quiet reviews on", g, err)
	}
	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "settings_updated" {
			details = append(details, a.Detail)
		}
	}
	if !slices.Equal(details, []string{"quietReviews"}) {
		t.Errorf("settings_updated details = %q; want quietReviews named", details)
	}

	rec = e.send(c, "PUT", target, `{"quietReviews":"x"}`)
	wantCode(t, rec, "PUT x", http.StatusBadRequest)
	if got := errorOf(t, rec); got != "quietReviews must be true, false or null" {
		t.Errorf("error = %q", got)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.QuietReviews == nil || !*r.QuietReviews {
		t.Errorf("a refused PUT changed the row: %v", r.QuietReviews)
	}

	rec = e.send(c, "PUT", target, `{"quietReviews":null}`)
	wantCode(t, rec, "PUT null", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.QuietReviews != nil {
		t.Errorf("stored after null = %v, want NULL", r.QuietReviews)
	}
	if g, ok, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || !ok || g.QuietReviews {
		t.Errorf("effective after null = %+v, %v, %v; want the default, off", g, ok, err)
	}

	e.send(c, "PUT", target, `{"quietReviews":true}`)
	wantCode(t, e.send(c, "POST", target+"/reset", ""), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.QuietReviews != nil {
		t.Errorf("stored after reset = %v, want NULL", r.QuietReviews)
	}
	if !strings.Contains(msgBadSettings, "quietReviews") {
		t.Errorf("msgBadSettings = %q", msgBadSettings)
	}

	// A maintainer changes it like the other switches.
	m := newEnv(t, nil)
	m.dashboardRepo("main", "acme/gadgets", nil)
	m.aliceOn("main", store.RoleMaintainer)
	maintainer := m.aliceSession()
	wantCode(t, m.send(maintainer, "PUT", target, `{"quietReviews":true}`), "maintainer PUT", http.StatusOK)
	if r, _ := m.storedRepo("main", "acme/gadgets"); r.QuietReviews == nil || !*r.QuietReviews {
		t.Errorf("after a maintainer's PUT stored = %v, want true", r.QuietReviews)
	}

	// A repository the config lists is read-only.
	k := newEnv(t, nil)
	k.cat["main"].set("acme/widgets")
	cfg := k.userOnMain(true, "acme/widgets")
	wantCode(t, k.send(cfg, "PUT", "/api/repositories/main/acme/widgets/settings", `{"quietReviews":true}`), "PUT on a config repository", http.StatusConflict)
	if _, found := k.storedRepo("main", "acme/widgets"); found {
		t.Errorf("a dashboard row was made for the config repository")
	}
}

// The comment layout is stored on its own: a list of block:state entries in
// the order given, null back to the default; a reset clears it; the five
// refusals of render.ParseLayout (an unknown block, an unknown state, a
// repeated block, an empty list, a list without findings) and anything that
// is not a list of texts are 400 with msgBadSettings and leave the row as it
// was. A maintainer may change it, the audit entry names it, and a
// repository the config manages refuses it.
func TestPutCommentLayout(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"
	asText := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}

	// Nothing set: the view carries the built-in layout, not overridden.
	detail := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets")
	wantDefault := `["summary:open","findings:collapsed","dismissed:collapsed","checks:open","files:collapsed"]`
	if s := detail.Settings["commentLayout"]; asText(s.Value) != wantDefault || s.Overridden {
		t.Errorf("commentLayout = %s overridden %v; want the default, not overridden", asText(s.Value), s.Overridden)
	}

	// A valid list saves and reads back in the order given. A block left out
	// stays out.
	want := []string{"checks:collapsed", "findings:open", "summary:open"}
	rec := e.sendJSON(c, "PUT", target, map[string]any{"commentLayout": want})
	wantCode(t, rec, "PUT list", http.StatusOK)
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if s := detail.Settings["commentLayout"]; asText(s.Value) != asText(want) || !s.Overridden {
		t.Errorf("commentLayout = %s overridden %v; want %s overridden", asText(s.Value), s.Overridden, asText(want))
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); !slices.Equal(r.CommentLayout, want) {
		t.Errorf("stored = %q, want %q", r.CommentLayout, want)
	}
	if g, ok, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || !ok || !slices.Equal(g.CommentLayout, want) {
		t.Errorf("effective = %+v, %v, %v; want the layout", g, ok, err)
	}
	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "settings_updated" {
			details = append(details, a.Detail)
		}
	}
	if !slices.Equal(details, []string{"commentLayout"}) {
		t.Errorf("settings_updated details = %q; want commentLayout named", details)
	}

	// Each refusal is 400 with the settings message, and the row stays.
	for _, tc := range []struct{ name, body string }{
		{"unknown block", `{"commentLayout":["findings:open","notes:open"]}`},
		{"unknown state", `{"commentLayout":["findings:hidden"]}`},
		{"repeated block", `{"commentLayout":["findings:open","findings:collapsed"]}`},
		{"empty list", `{"commentLayout":[]}`},
		{"no findings", `{"commentLayout":["summary:open","files:open"]}`},
		{"no state", `{"commentLayout":["findings"]}`},
		{"not a list", `{"commentLayout":"findings:open"}`},
		{"not texts", `{"commentLayout":[{"findings":"open"}]}`},
	} {
		rec := e.send(c, "PUT", target, tc.body)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgBadSettings {
			t.Errorf("%s: PUT = %d %s; want 400 %q", tc.name, rec.Code, rec.Body, msgBadSettings)
		}
		if r, _ := e.storedRepo("main", "acme/gadgets"); !slices.Equal(r.CommentLayout, want) {
			t.Errorf("%s: a refused PUT changed the row: %q", tc.name, r.CommentLayout)
		}
	}
	if !strings.Contains(msgBadSettings, "commentLayout") {
		t.Errorf("msgBadSettings = %q", msgBadSettings)
	}

	// Null resets it; so does the reset button.
	wantCode(t, e.send(c, "PUT", target, `{"commentLayout":null}`), "PUT null", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CommentLayout != nil {
		t.Errorf("stored after null = %q, want NULL", r.CommentLayout)
	}
	if g, ok, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || !ok || g.CommentLayout != nil {
		t.Errorf("effective after null = %+v, %v, %v; want the default", g, ok, err)
	}
	e.sendJSON(c, "PUT", target, map[string]any{"commentLayout": want})
	wantCode(t, e.send(c, "POST", target+"/reset", ""), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CommentLayout != nil {
		t.Errorf("stored after reset = %q, want NULL", r.CommentLayout)
	}

	// A maintainer changes it like the other settings.
	m := newEnv(t, nil)
	m.dashboardRepo("main", "acme/gadgets", nil)
	m.aliceOn("main", store.RoleMaintainer)
	maintainer := m.aliceSession()
	wantCode(t, m.send(maintainer, "PUT", target, `{"commentLayout":["findings:open"]}`), "maintainer PUT", http.StatusOK)
	if r, _ := m.storedRepo("main", "acme/gadgets"); !slices.Equal(r.CommentLayout, []string{"findings:open"}) {
		t.Errorf("after a maintainer's PUT stored = %q", r.CommentLayout)
	}

	// A repository the config lists is read-only.
	k := newEnv(t, nil)
	k.cat["main"].set("acme/widgets")
	cfg := k.userOnMain(true, "acme/widgets")
	wantCode(t, k.send(cfg, "PUT", "/api/repositories/main/acme/widgets/settings", `{"commentLayout":["findings:open"]}`), "PUT on a config repository", http.StatusConflict)
	if _, found := k.storedRepo("main", "acme/widgets"); found {
		t.Errorf("a dashboard row was made for the config repository")
	}
}

// boolSetting takes the two switches and the one for inline findings, and
// names the key in its message.
func TestBoolSettingInlineFindings(t *testing.T) {
	var row store.DashboardRepo
	edit, msg := boolSetting("inlineFindings", json.RawMessage(`false`), false)
	if msg != "" {
		t.Fatalf("msg = %q", msg)
	}
	edit(&row)
	if row.InlineFindings == nil || *row.InlineFindings || row.Approve != nil || row.ReviewDrafts != nil {
		t.Errorf("row = %+v, want only inline findings false", row)
	}
	edit, _ = boolSetting("inlineFindings", json.RawMessage(`null`), true)
	edit(&row)
	if row.InlineFindings != nil {
		t.Errorf("after null: %v", row.InlineFindings)
	}
	if _, msg := boolSetting("inlineFindings", json.RawMessage(`"no"`), false); msg != "inlineFindings must be true, false or null" {
		t.Errorf("msg = %q", msg)
	}
	if !strings.Contains(msgBadSettings, "inlineFindings") {
		t.Errorf("msgBadSettings = %q", msgBadSettings)
	}
}

// A key that is not a bool setting is refused with a message, not a nil
// function that panics when the edit runs.
func TestBoolSettingUnknownKey(t *testing.T) {
	for _, raw := range []string{`true`, `null`} {
		edit, msg := boolSetting("nope", json.RawMessage(raw), raw == "null")
		if edit != nil {
			t.Errorf("%s: edit is not nil", raw)
		}
		if msg != "nope is not a setting" {
			t.Errorf("%s: msg = %q", raw, msg)
		}
	}
}

// Review deleted and renamed files is stored on its own: true or false, null
// back to the default (off); a reset clears it; anything else is 400 with its
// message. A maintainer may change it, a viewer may not, the audit entry names
// it, and a repository the config manages refuses it.
func TestCheckDeletedSetting(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"

	rec := e.send(c, "PUT", target, `{"checkDeleted":true}`)
	wantCode(t, rec, "PUT true", http.StatusOK)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if s := detail.Settings["checkDeleted"]; s.Value != true || !s.Overridden {
		t.Errorf("checkDeleted = %+v, want true overridden", s)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CheckDeleted == nil || !*r.CheckDeleted {
		t.Errorf("stored = %v, want true", r.CheckDeleted)
	}
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || !g.CheckDeleted {
		t.Errorf("effective = %+v, %v; want review deleted files on", g, err)
	}
	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "settings_updated" {
			details = append(details, a.Detail)
		}
	}
	if !slices.Equal(details, []string{"checkDeleted"}) {
		t.Errorf("settings_updated details = %q; want checkDeleted named", details)
	}

	rec = e.send(c, "PUT", target, `{"checkDeleted":"x"}`)
	wantCode(t, rec, "PUT x", http.StatusBadRequest)
	if got := errorOf(t, rec); got != "checkDeleted must be true, false or null" {
		t.Errorf("error = %q", got)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CheckDeleted == nil || !*r.CheckDeleted {
		t.Errorf("a refused PUT changed the row: %v", r.CheckDeleted)
	}

	rec = e.send(c, "PUT", target, `{"checkDeleted":null}`)
	wantCode(t, rec, "PUT null", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CheckDeleted != nil {
		t.Errorf("stored after null = %v, want NULL", r.CheckDeleted)
	}
	if g, ok, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || !ok || g.CheckDeleted {
		t.Errorf("effective after null = %+v, %v, %v; want the default, off", g, ok, err)
	}

	e.send(c, "PUT", target, `{"checkDeleted":true}`)
	wantCode(t, e.send(c, "POST", target+"/reset", ""), "reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.CheckDeleted != nil {
		t.Errorf("stored after reset = %v, want NULL", r.CheckDeleted)
	}
	if !strings.Contains(msgBadSettings, "checkDeleted") {
		t.Errorf("msgBadSettings = %q", msgBadSettings)
	}

	// A maintainer changes it like the other switches.
	m := newEnv(t, nil)
	m.dashboardRepo("main", "acme/gadgets", nil)
	m.aliceOn("main", store.RoleMaintainer)
	maintainer := m.aliceSession()
	wantCode(t, m.send(maintainer, "PUT", target, `{"checkDeleted":true}`), "maintainer PUT", http.StatusOK)
	if r, _ := m.storedRepo("main", "acme/gadgets"); r.CheckDeleted == nil || !*r.CheckDeleted {
		t.Errorf("after a maintainer's PUT stored = %v, want true", r.CheckDeleted)
	}

	// A viewer may not.
	v := newEnv(t, nil)
	v.dashboardRepo("main", "acme/gadgets", nil)
	v.aliceOn("main", store.RoleViewer)
	wantCode(t, v.send(v.aliceSession(), "PUT", target, `{"checkDeleted":true}`), "viewer PUT", http.StatusForbidden)
	if r, _ := v.storedRepo("main", "acme/gadgets"); r.CheckDeleted != nil {
		t.Errorf("a viewer's PUT changed the row: %v", r.CheckDeleted)
	}

	// A repository the config lists is read-only.
	k := newEnv(t, nil)
	k.cat["main"].set("acme/widgets")
	cfg := k.userOnMain(true, "acme/widgets")
	wantCode(t, k.send(cfg, "PUT", "/api/repositories/main/acme/widgets/settings", `{"checkDeleted":true}`), "PUT on a config repository", http.StatusConflict)
	if _, found := k.storedRepo("main", "acme/widgets"); found {
		t.Errorf("a dashboard row was made for the config repository")
	}
}

// POST /api/repositories with ownerTeam switches the repository on and makes
// the team its owner in one step, audited as the team page audits it. An
// unknown team is 400 before the platform is asked; another team's owner
// tick on a paused repository is 409, and the repository stays paused.
func TestSwitchOnOwnerTeam(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets", "acme/tools", "acme/theirs")
	c := e.userOnMain(true)
	devs := e.team("devs", nil)
	ops := e.team("ops", nil)
	body := func(project string, team int64) string {
		return fmt.Sprintf(`{"instance":"main","project":%q,"ownerTeam":%d}`, project, team)
	}

	t.Run("unknown team", func(t *testing.T) {
		for _, id := range []int64{999999, 0} {
			before := e.cat["main"].checkCount()
			rec := e.send(c, "POST", "/api/repositories", body("acme/gadgets", id))
			if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgNoTeam {
				t.Errorf("ownerTeam %d = %d %s; want 400 %q", id, rec.Code, rec.Body, msgNoTeam)
			}
			if n := e.cat["main"].checkCount() - before; n != 0 {
				t.Errorf("ownerTeam %d asked the platform %d times; want 0", id, n)
			}
		}
		if _, found := e.storedRepo("main", "acme/gadgets"); found {
			t.Error("acme/gadgets was switched on for an unknown team")
		}
	})
	t.Run("valid team", func(t *testing.T) {
		rec := e.send(c, "POST", "/api/repositories", body("acme/gadgets", devs))
		detail := decode[RepositoryDetailView](t, rec, "POST with ownerTeam", http.StatusCreated)
		if want := (OwnerTeamView{ID: devs, Name: "devs", Open: true}); detail.OwnerTeam == nil || *detail.OwnerTeam != want {
			t.Errorf("detail.ownerTeam = %+v; want %+v", detail.OwnerTeam, want)
		}
		teams := decode[RepoTeamsView](t, e.get(c, "/api/repositories/main/acme/gadgets/teams"), "GET teams", http.StatusOK)
		if i := slices.IndexFunc(teams.Teams, func(v RepoTeamView) bool { return v.ID == devs }); i < 0 || !teams.Teams[i].Direct {
			t.Errorf("teams = %+v; want devs direct", teams.Teams)
		}
		if got := e.storedTeam(devs).Repos; !slices.Equal(got, []store.TeamRepo{{Instance: "main", Project: "acme/gadgets", Owner: true}}) {
			t.Errorf("devs repos = %+v", got)
		}
		if got := e.auditActionsOf("acme/gadgets"); !slices.Equal(got, []string{"repository_on", "team_updated", "owner_set"}) {
			t.Errorf("audit = %v", got)
		}
		if set := e.auditOf("owner_set"); len(set) != 1 || set[0].Detail != "devs: main/acme/gadgets" {
			t.Errorf("owner_set = %+v; want one, detail %q", set, "devs: main/acme/gadgets")
		}
	})
	t.Run("another owner on a paused repository", func(t *testing.T) {
		e.pausedRepo("main", "acme/theirs", nil)
		e.editTeam(ops, func(tm *store.Team) {
			tm.Repos = append(tm.Repos, store.TeamRepo{Instance: "main", Project: "acme/theirs", Owner: true})
		})
		rec := e.send(c, "POST", "/api/repositories", body("acme/theirs", devs))
		if want := "acme/theirs already has owner ops"; rec.Code != http.StatusConflict || errorOf(t, rec) != want {
			t.Errorf("POST = %d %s; want 409 %q", rec.Code, rec.Body, want)
		}
		if r, _ := e.storedRepo("main", "acme/theirs"); r.Enabled {
			t.Error("the repository was resumed although the owner was refused")
		}
		if got := e.storedTeam(devs).Repos; len(got) != 1 {
			t.Errorf("devs repos = %+v; want acme/gadgets alone", got)
		}
		if got := e.auditActionsOf("acme/theirs"); len(got) != 0 {
			t.Errorf("audit = %v; want nothing", got)
		}
	})
	t.Run("no owner team", func(t *testing.T) {
		rec := e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/tools"}`)
		detail := decode[RepositoryDetailView](t, rec, "POST without ownerTeam", http.StatusCreated)
		if detail.OwnerTeam != nil {
			t.Errorf("detail.ownerTeam = %+v; want null", detail.OwnerTeam)
		}
		if got := e.auditActionsOf("acme/tools"); !slices.Equal(got, []string{"repository_on"}) {
			t.Errorf("audit = %v", got)
		}
	})
}

// A repository's page names its owner team. Open (a link to the team page)
// is for an admin and the team's members; anyone else who sees the
// repository gets the name alone. No owner team is null.
func TestRepositoryOwnerTeamShown(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.dashboardRepo("main", "acme/tools", nil)
	bob := e.sso("bob@example.com")
	devs := e.team("devs", viewerMembers(e.alice()), store.TeamRepo{Instance: "main", Project: "acme/gadgets", Owner: true})
	e.team("readers", viewerMembers(bob), store.TeamRepo{Instance: "main", Project: "acme/gadgets"},
		store.TeamRepo{Instance: "main", Project: "acme/tools"})

	for _, tc := range []struct {
		name    string
		c       *http.Cookie
		project string
		want    *OwnerTeamView
	}{
		{"member of the owner team", e.aliceSession(), "acme/gadgets", &OwnerTeamView{ID: devs, Name: "devs", Open: true}},
		{"member of another team", e.sessionFor(bob), "acme/gadgets", &OwnerTeamView{ID: devs, Name: "devs", Open: false}},
		{"no owner team", e.sessionFor(bob), "acme/tools", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.get(tc.c, "/api/repositories/main/"+tc.project)
			detail := decode[RepositoryDetailView](t, rec, "GET "+tc.project, http.StatusOK)
			switch {
			case tc.want == nil && detail.OwnerTeam != nil:
				t.Errorf("ownerTeam = %+v; want null", detail.OwnerTeam)
			case tc.want == nil && !strings.Contains(rec.Body.String(), `"ownerTeam":null`):
				t.Errorf("body has no ownerTeam null: %s", rec.Body)
			case tc.want != nil && (detail.OwnerTeam == nil || *detail.OwnerTeam != *tc.want):
				t.Errorf("ownerTeam = %+v; want %+v", detail.OwnerTeam, tc.want)
			}
		})
	}
}
