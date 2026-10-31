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
	r, found, err := e.st.DashboardRepo(ctx, instance, project)
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
		{"POST", "/api/repositories", `{"project":"` + project + `"}`},
		{"DELETE", base, ""},
		{"PUT", base + "/settings", `{"approve":false}`},
		{"POST", base + "/settings/reset", ""},
	}
}

// Review Focus 2: a viewer calling a write endpoint directly, with a valid
// session and CSRF token, gets 403 and nothing changes.
func TestWritesRequireAdmin(t *testing.T) {
	for _, platform := range []string{"gitlab", "github"} {
		t.Run(platform, func(t *testing.T) {
			e := newEnv(t, nil)
			instance := map[string]string{"gitlab": "main", "github": "gh"}[platform]
			e.dashboardRepo(instance, "acme/gadgets", func(r *store.DashboardRepo) { r.Approve = ptr(true) })
			e.cat[instance].set("acme/gadgets", "acme/tools")
			var c *http.Cookie
			if platform == "gitlab" {
				e.gl.setProject("acme/tools", gitlabProject("private", 40, 0))
				c = e.signInGitLab(false, "acme/gadgets")
			} else {
				e.gh.setRepo("acme/tools", githubRepo(true, true))
				c = e.signInGitHub(false, "acme/gadgets")
			}
			if _, se := e.session(c); se.Admin || se.Role(instance, "acme/gadgets") != RoleViewer {
				t.Fatalf("session = %+v; want a viewer who sees acme/gadgets", se)
			}

			wantCode(t, e.get(c, "/api/repositories/available"), "GET available", http.StatusForbidden)
			for _, w := range append(writes(instance, "acme/gadgets"),
				struct{ method, target, body string }{"POST", "/api/repositories", `{"project":"acme/tools"}`}) {
				wantCode(t, e.send(c, w.method, w.target, w.body), w.method+" "+w.target, http.StatusForbidden)
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

// Review Focus 3: an admin calling a write endpoint for a repository they
// can't see gets 404, the same answer as for one that doesn't exist.
func TestAdminCannotWriteInvisibleRepo(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/hidden", func(r *store.DashboardRepo) { r.Approve = ptr(true) })
	e.dashboardRepo("gh", "acme/other", nil)
	e.cat["main"].set("acme/hidden", "acme/gadgets")
	// alice can't see acme/hidden on GitLab: no project answer for it.
	c := e.signInGitLab(true, "acme/gadgets")
	if _, se := e.session(c); !se.Admin || se.CanSee("main", "acme/hidden") {
		t.Fatalf("session = %+v; want an admin who can't see acme/hidden", se)
	}
	unknown := e.send(c, "DELETE", "/api/repositories/main/acme/nothing-here", "")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("DELETE of an unknown repository = %d %s", unknown.Code, unknown.Body)
	}
	for _, w := range append(writes("main", "acme/hidden"), writes("gh", "acme/other")[1:]...) {
		rec := e.send(c, w.method, w.target, w.body)
		wantCode(t, rec, w.method+" "+w.target, http.StatusNotFound)
		if rec.Body.String() != unknown.Body.String() {
			t.Errorf("%s %s body = %s, want the unknown repository's %s", w.method, w.target, rec.Body, unknown.Body)
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
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.cat["main"].set("acme/widgets", "acme/gadgets")
	c := e.signInGitLab(true, "acme/widgets", "acme/gadgets")

	for _, w := range writes("main", "acme/widgets") {
		rec := e.send(c, w.method, w.target, w.body)
		wantCode(t, rec, w.method+" "+w.target, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s %s body = %s", w.method, w.target, rec.Body)
		}
	}
	rec := e.send(c, "POST", "/api/repositories", `{"project":"ACME/Gadgets"}`)
	wantCode(t, rec, "POST of a repository already on", http.StatusConflict)

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
	e := newEnv(t, func(cfg *config.Config) {
		cfg.Jobs.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	})
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.signInGitLab(true, "acme/gadgets")
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
		{`{"failureStatus":"neutral"}`, true, func(r store.DashboardRepo) bool { return r.FailureStatus != nil && *r.FailureStatus == "neutral" }},
		{`{"failureStatus":"success"}`, false, nil},
		{`{"failureStatus":"failed"}`, true, func(r store.DashboardRepo) bool { return r.FailureStatus != nil && *r.FailureStatus == "failed" }},
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
		{`{"jobMemory":"lots"}`, false, nil},
		{`{"jobMemory":"512Mi"}`, false, nil}, // below the 1Gi request
		{`{"jobMemory":"33Gi"}`, false, nil},
		{`{"jobMemory":"-2Gi"}`, false, nil},
		{`{"jobMemory":2048}`, false, nil},
		{`{"jobMemory":"32Gi"}`, true, func(r store.DashboardRepo) bool { return r.JobMemory != nil && *r.JobMemory == "32Gi" }},
		{`{"jobMemory":"1Gi"}`, true, func(r store.DashboardRepo) bool { return r.JobMemory != nil && *r.JobMemory == "1Gi" }},
		{`{"jobMemory":"1.5Gi"}`, true, func(r store.DashboardRepo) bool { return r.JobMemory != nil && *r.JobMemory == "1536Mi" }},
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
	rec := e.send(c, "PUT", target, `{"approve":false,"jobTimeoutSeconds":900}`)
	wantCode(t, rec, "PUT two fields", http.StatusOK)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Repo.Project != "acme/gadgets" || detail.Repo.Source != "dashboard" ||
		detail.Settings["approve"].Value != false || !detail.Settings["approve"].Overridden ||
		detail.Settings["jobTimeoutSeconds"].Value != float64(900) ||
		detail.Settings["jobMemory"].Value != "1536Mi" || !detail.Settings["jobMemory"].Overridden ||
		detail.Settings["skills"].Overridden || detail.Settings["skills"].Value != nil {
		t.Errorf("detail = %+v", detail)
	}

	// Reset clears every override.
	rec = e.send(c, "POST", target+"/reset", "")
	wantCode(t, rec, "reset", http.StatusOK)
	r, found := e.storedRepo("main", "acme/gadgets")
	if !found || r.Approve != nil || r.ReviewDrafts != nil || r.FailureStatus != nil || r.Skills != nil ||
		r.JobTimeoutSeconds != nil || r.JobMemory != nil || r.CreatedBy != "carol" {
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
	if n := e.reconciles.Load(); n != 0 {
		t.Errorf("settings changes called ReconcileSoon %d times", n)
	}
}

// Without jobs.resources.requests, the memory limit's floor is 256Mi.
func TestSettingsMemoryFloor(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.signInGitLab(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"
	wantCode(t, e.send(c, "PUT", target, `{"jobMemory":"255Mi"}`), "255Mi", http.StatusBadRequest)
	wantCode(t, e.send(c, "PUT", target, `{"jobMemory":"256Mi"}`), "256Mi", http.StatusOK)
}

// Switching a repository on makes its dashboard record, asks the engine to
// reconcile, and the admin sees it straight away; switching it off deletes
// the record and asks again.
func TestSwitchOnTriggersReconcile(t *testing.T) {
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets")
	c := e.signInGitLab(true, "acme/gadgets")

	rec := e.send(c, "POST", "/api/repositories", `{"project":"ACME/Gadgets"}`)
	wantCode(t, rec, "POST", http.StatusCreated)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Repo.Project != "acme/gadgets" || detail.Repo.Source != "dashboard" || !detail.Repo.On ||
		detail.Repo.Instance != "main" || detail.Settings["approve"].Overridden {
		t.Errorf("detail = %+v", detail)
	}
	r, found := e.storedRepo("main", "acme/gadgets")
	if !found || r.Project != "acme/gadgets" || r.CreatedBy != "alice" || r.Approve != nil || r.Skills != nil {
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
	if detail.Repo.On || detail.Repo.Project != "acme/gadgets" {
		t.Errorf("detail after DELETE = %+v", detail)
	}
	if _, found := e.storedRepo("main", "acme/gadgets"); found {
		t.Errorf("the row is still there")
	}
	if n := e.reconciles.Load(); n != 2 {
		t.Errorf("ReconcileSoon called %d times, want 2", n)
	}
	// Each names the repository switched, as the platform spells it.
	e.mu.Lock()
	defer e.mu.Unlock()
	if want := []string{"main/acme/gadgets", "main/acme/gadgets"}; !slices.Equal(e.reconciled, want) {
		t.Errorf("ReconcileSoon got %v, want %v", e.reconciled, want)
	}
}

// The client can't switch on a repository that isn't in the available set:
// the server checks again, with the admin's token and the bot's list.
func TestSwitchOnOnlyAvailable(t *testing.T) {
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets", "acme/bot-only", "acme/guest")
	e.gl.setProject("acme/user-only", gitlabProject("private", 30, 0))
	e.gl.setProject("acme/guest", gitlabProject("private", 10, 0))
	c := e.signInGitLab(true, "acme/gadgets")

	unknown := e.send(c, "POST", "/api/repositories", `{"project":"acme/nothing-here"}`)
	wantCode(t, unknown, "POST unknown", http.StatusNotFound)
	for _, p := range []string{"acme/bot-only", "acme/user-only", "acme/guest"} {
		rec := e.send(c, "POST", "/api/repositories", `{"project":"`+p+`"}`)
		wantCode(t, rec, "POST "+p, http.StatusNotFound)
		if rec.Body.String() != unknown.Body.String() {
			t.Errorf("POST %s body = %s", p, rec.Body)
		}
		if _, found := e.storedRepo("main", p); found {
			t.Errorf("%s was switched on", p)
		}
	}
	for _, body := range []string{`{}`, `{"project":""}`, `{"project":1}`, `{"project":"acme/gadgets","x":1}`, `nope`} {
		wantCode(t, e.send(c, "POST", "/api/repositories", body), "POST "+body, http.StatusBadRequest)
	}
	if n := e.reconciles.Load(); n != 0 {
		t.Errorf("ReconcileSoon called %d times", n)
	}

	// The platforms can't be asked: 502, nothing switched on.
	e.gl.set(func(o *oauthServer) { o.failAccess = true })
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"project":"acme/gadgets"}`), "POST while GitLab fails", http.StatusBadGateway)
	e.gl.set(func(o *oauthServer) { o.failAccess = false })
	e.cat["main"].setFail(errors.New("bot down"))
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"project":"acme/gadgets"}`), "POST while the bot fails", http.StatusBadGateway)
	if _, found := e.storedRepo("main", "acme/gadgets"); found {
		t.Errorf("acme/gadgets was switched on")
	}
}

// A GitLab project reached only through a share (a group the project or
// its group is shared with, as Reporter or above) is in the picker, can be
// switched on, and is then seen: its permissions don't show the share, so
// the project listing is asked. A share below Reporter is not enough.
func TestSwitchOnSharedProject(t *testing.T) {
	e := newEnv(t, nil)
	e.cat["main"].set("acme/shared", "acme/shared-guest")
	shared := gitlabProject("private", 0, 0)
	shared["shared_access"] = 30
	e.gl.setProject("acme/shared", shared)
	guest := gitlabProject("private", 0, 0)
	guest["shared_access"] = 10
	e.gl.setProject("acme/shared-guest", guest)
	c := e.signInGitLab(true)

	got := getJSON[availableBody](e, c, "/api/repositories/available")
	if want := []string{"acme/shared"}; !slices.Equal(projectsOf(got.Items), want) {
		t.Errorf("available = %v, want %v", projectsOf(got.Items), want)
	}
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"project":"acme/shared-guest"}`), "POST a Guest share", http.StatusNotFound)
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"project":"acme/shared"}`), "POST a Developer share", http.StatusCreated)

	// The next access check keeps it.
	e.clock.advance(AccessTTL + time.Second)
	if _, se := e.session(c); !se.CanSee("main", "acme/shared") || se.CanSee("main", "acme/shared-guest") {
		t.Errorf("visible after the next check = %v", se.Visible)
	}
}

// On GitHub the admin needs pull access and the review App an
// installation on the repository.
func TestSwitchOnOnlyAvailableGitHub(t *testing.T) {
	e := newEnv(t, nil)
	e.gh.setRepo("acme/no-pull", githubRepo(true, false))
	e.gh.setRepo("acme/user-only", githubRepo(true, true))
	e.gh.setRepo("Acme/Gadgets", githubRepo(true, true))
	e.cat["gh"].set("acme/no-pull", "acme/gadgets", "acme/bot-only")
	c := e.signInGitHub(true)
	for _, p := range []string{"acme/no-pull", "acme/user-only", "acme/bot-only"} {
		wantCode(t, e.send(c, "POST", "/api/repositories", `{"project":"`+p+`"}`), "POST "+p, http.StatusNotFound)
	}
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"project":"acme/gadgets"}`), "POST acme/gadgets", http.StatusCreated)
	if r, found := e.storedRepo("gh", "acme/gadgets"); !found || r.Project != "Acme/Gadgets" {
		t.Errorf("row = %+v, %v; want GitHub's spelling", r, found)
	}
}

// Switching on doesn't depend on the picker's lists: a repository beyond
// what the lists read can be added.
func TestSwitchOnBeyondListingCap(t *testing.T) {
	e := newEnv(t, nil)
	e.d.listLimit = 2
	var all []string
	for i := range 5 {
		p := fmt.Sprintf("acme/repo-%d", i)
		all = append(all, p)
		e.gl.setProject(p, gitlabProject("private", 20, 0))
	}
	e.cat["main"].set(all...)
	c := e.signInGitLab(true)
	got := getJSON[availableBody](e, c, "/api/repositories/available")
	if len(got.Items) != 2 || !got.Truncated {
		t.Fatalf("available = %v truncated %v; want the first 2, truncated", projectsOf(got.Items), got.Truncated)
	}
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"project":"acme/repo-4"}`), "POST beyond the cap", http.StatusCreated)
}

// Whether a repository is on is told only to an admin who reaches it on
// the platform: one that is on but out of the admin's reach gets the same
// 404, after the same platform check, as one that is off.
func TestSwitchOnChecksBeforeTellingOnOrOff(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/hidden", nil)
	e.cat["main"].set("acme/hidden", "acme/off")
	c := e.signInGitLab(true)

	for _, p := range []string{"acme/hidden", "acme/off"} {
		before := e.gl.projectChecks()
		rec := e.send(c, "POST", "/api/repositories", `{"project":"`+p+`"}`)
		wantCode(t, rec, "POST "+p, http.StatusNotFound)
		if rec.Body.String() != `{"error":"not found"}`+"\n" {
			t.Errorf("POST %s body = %q", p, rec.Body)
		}
		if n := e.gl.projectChecks() - before; n != 1 {
			t.Errorf("POST %s asked GitLab %d times, want 1", p, n)
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

// The picker lists what both the admin (member, Reporter or above) and the
// review bot (Maintainer or above) or App reach, minus what is on already.
func TestAvailableIsIntersection(t *testing.T) {
	t.Run("gitlab", func(t *testing.T) {
		e := newEnv(t, nil)
		e.dashboardRepo("main", "acme/dash", nil)
		for p, level := range map[string]int{"acme/gadgets": 20, "acme/tools": 30, "acme/guest": 10,
			"acme/user-only": 40, "acme/dash": 20, "Acme/Zeta": 50} {
			e.gl.setProject(p, gitlabProject("private", level, 0))
		}
		e.gl.setProject("acme/public", gitlabProject("public", 0, 0))
		e.cat["main"].set("acme/gadgets", "acme/tools", "acme/guest", "acme/public", "acme/widgets",
			"acme/dash", "acme/bot-only", "acme/zeta")
		c := e.signInGitLab(true, "acme/widgets")

		got := getJSON[availableBody](e, c, "/api/repositories/available")
		if want := []string{"acme/gadgets", "acme/tools", "Acme/Zeta"}; !slices.Equal(projectsOf(got.Items), want) || got.Truncated {
			t.Errorf("available = %v truncated %v, want %v", projectsOf(got.Items), got.Truncated, want)
		}
		for _, it := range got.Items {
			if it.Instance != "main" || it.Platform != "gitlab" {
				t.Errorf("item = %+v", it)
			}
		}
		e.gl.mu.Lock()
		listings := slices.Clone(e.gl.listings)
		e.gl.mu.Unlock()
		if len(listings) != 1 || !strings.Contains(listings[0], "min_access_level=20") || !strings.Contains(listings[0], "simple=true") {
			t.Errorf("user listings = %v", listings)
		}

		// The bot's list is kept 5 minutes; the admin's is asked each time.
		getJSON[availableBody](e, c, "/api/repositories/available")
		if n := e.cat["main"].callCount(); n != 1 {
			t.Errorf("bot listed %d times within 5 minutes, want 1", n)
		}
		e.clock.advance(5*time.Minute + time.Second)
		getJSON[availableBody](e, c, "/api/repositories/available")
		if n := e.cat["main"].callCount(); n != 2 {
			t.Errorf("bot listed %d times after 5 minutes, want 2", n)
		}
		e.gl.mu.Lock()
		n := len(e.gl.listings)
		e.gl.mu.Unlock()
		if n != 3 {
			t.Errorf("user listed %d times, want 3", n)
		}
	})

	t.Run("github", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gh.setRepo("acme/gadgets", githubRepo(true, true))
		e.gh.setRepo("acme/user-only", githubRepo(true, true))
		e.gh.setRepo("acme/no-pull", githubRepo(true, false))
		e.cat["gh"].set("acme/gadgets", "acme/widgets", "acme/no-pull", "acme/bot-only")
		c := e.signInGitHub(true, "acme/widgets")

		got := getJSON[availableBody](e, c, "/api/repositories/available")
		if want := []string{"acme/gadgets"}; !slices.Equal(projectsOf(got.Items), want) || got.Truncated {
			t.Errorf("available = %v truncated %v, want %v", projectsOf(got.Items), got.Truncated, want)
		}
		if len(got.Items) == 1 && (got.Items[0].Instance != "gh" || got.Items[0].Platform != "github") {
			t.Errorf("item = %+v", got.Items[0])
		}

		rec := e.send(c, "POST", "/api/repositories", `{"project":"acme/gadgets"}`)
		wantCode(t, rec, "POST on GitHub", http.StatusCreated)
		if _, found := e.storedRepo("gh", "acme/gadgets"); !found {
			t.Errorf("acme/gadgets on gh was not switched on")
		}
	})

	t.Run("capped at 500", func(t *testing.T) {
		e := newEnv(t, nil)
		var all []string
		for i := range 600 {
			p := fmt.Sprintf("acme/repo-%03d", i)
			all = append(all, p)
			e.gl.setProject(p, gitlabProject("private", 20, 0))
		}
		e.cat["main"].set(all...)
		c := e.signInGitLab(true)
		got := getJSON[availableBody](e, c, "/api/repositories/available")
		if len(got.Items) != 500 || !got.Truncated || got.Items[0].Project != "acme/repo-000" {
			t.Errorf("available has %d items, truncated %v", len(got.Items), got.Truncated)
		}
	})

	t.Run("platform failure", func(t *testing.T) {
		e := newEnv(t, nil)
		c := e.signInGitLab(true)
		e.gl.set(func(*oauthServer) { e.gl.listFail = true })
		rec := e.get(c, "/api/repositories/available")
		wantCode(t, rec, "available while GitLab fails", http.StatusBadGateway)
		if strings.Contains(rec.Body.String(), "500") || strings.Contains(rec.Body.String(), "SECRET") {
			t.Errorf("body leaks upstream detail: %s", rec.Body)
		}
	})

	// A page failing after others were read lists what was read,
	// truncated, and logs the failure without the user's token.
	t.Run("later page fails", func(t *testing.T) {
		e := newEnv(t, nil)
		var all []string
		for i := range 150 { // two pages of 100
			p := fmt.Sprintf("acme/repo-%03d", i)
			all = append(all, p)
			e.gl.setProject(p, gitlabProject("private", 20, 0))
		}
		e.cat["main"].set(all...)
		c := e.signInGitLab(true)
		e.gl.set(func(*oauthServer) { e.gl.listFailFrom = 2 })
		got := getJSON[availableBody](e, c, "/api/repositories/available")
		if len(got.Items) != 100 || !got.Truncated {
			t.Errorf("available has %d items, truncated %v; want the first page, truncated", len(got.Items), got.Truncated)
		}
		logs := e.logs.String()
		if !strings.Contains(logs, "listing stopped early") {
			t.Errorf("the failed page isn't logged:\n%s", logs)
		}
		for _, tok := range e.gl.tokens() {
			if strings.Contains(logs, tok) {
				t.Errorf("the log holds a token:\n%s", logs)
			}
		}

		// The first page failing still has nothing to show: 502.
		e.gl.set(func(*oauthServer) { e.gl.listFailFrom = 1 })
		wantCode(t, e.get(c, "/api/repositories/available?search=repo"), "first page fails", http.StatusBadGateway)
	})
}

// No write answer ever holds a token.
func TestWriteAnswersHoldNoSecret(t *testing.T) {
	e := newEnv(t, withSecrets(t))
	e.cat["main"].set("acme/gadgets")
	c := e.signInGitLab(true, "acme/gadgets")
	e.get(c, "/api/repositories/available")
	e.send(c, "POST", "/api/repositories", `{"project":"acme/gadgets"}`)
	e.send(c, "PUT", "/api/repositories/main/acme/gadgets/settings", `{"jobMemory":"lots"}`)
	e.send(c, "PUT", "/api/repositories/main/acme/gadgets/settings", `{"approve":false}`)
	e.send(c, "DELETE", "/api/repositories/main/acme/gadgets", "")
	secrets := append(e.gl.tokens(), llmKey, reviewToken, approveToken, webhookSecret, glClientSecret)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range e.responses {
		for _, s := range secrets {
			if strings.Contains(rec.Body.String(), s) {
				t.Errorf("a response holds %q: %s", s, rec.Body)
			}
		}
	}
	if logs := e.logs.String(); strings.Contains(logs, "gl-access") {
		t.Errorf("logs hold a user token:\n%s", logs)
	}
}

func ptr[T any](v T) *T { return &v }

// The picker uses the admin's token: one about to expire is renewed first,
// even while the access check is still fresh.
func TestAvailableRenewsAnExpiringToken(t *testing.T) {
	e := newEnv(t, nil) // GitLab tokens expire after 2 hours
	e.cat["main"].set("acme/gadgets")
	e.gl.setProject("acme/gadgets", gitlabProject("private", 20, 0))
	c := e.signInGitLab(true)
	e.clock.advance(2*time.Hour - 90*time.Second)
	e.get(c, "/api/me") // access checked again, token not due yet
	if n := e.gl.calls(); n != 1 {
		t.Fatalf("token calls = %d, want 1 (sign-in only)", n)
	}
	e.clock.advance(time.Minute)

	got := getJSON[availableBody](e, c, "/api/repositories/available")
	if !slices.Equal(projectsOf(got.Items), []string{"acme/gadgets"}) {
		t.Errorf("available = %v", projectsOf(got.Items))
	}
	if n := e.gl.calls(); n != 2 {
		t.Errorf("token calls = %d, want 2 (the renewal)", n)
	}
}

// A write without the CSRF token is refused before anything else, admin or
// not.
func TestWriteNeedsCSRF(t *testing.T) {
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets")
	c := e.signInGitLab(true, "acme/gadgets")
	req := httptest.NewRequest("POST", "/api/repositories", strings.NewReader(`{"project":"acme/gadgets"}`))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	wantCode(t, rec, "POST without CSRF", http.StatusForbidden)
	if _, found := e.storedRepo("main", "acme/gadgets"); found || e.reconciles.Load() != 0 {
		t.Errorf("switched on without a CSRF token")
	}
}

// ?search= finds repositories beyond the first ones listed: GitLab
// searches, GitHub's lists are filtered here.
func TestAvailableSearch(t *testing.T) {
	t.Run("gitlab", func(t *testing.T) {
		e := newEnv(t, nil)
		e.d.listLimit = 3
		var all []string
		for i := range 10 {
			p := fmt.Sprintf("acme/repo-%d", i)
			all = append(all, p)
			e.gl.setProject(p, gitlabProject("private", 20, 0))
		}
		e.cat["main"].set(all...)
		c := e.signInGitLab(true)
		got := getJSON[availableBody](e, c, "/api/repositories/available?search=REPO-9")
		if !slices.Equal(projectsOf(got.Items), []string{"acme/repo-9"}) || got.Truncated {
			t.Errorf("search = %v truncated %v", projectsOf(got.Items), got.Truncated)
		}
		e.gl.mu.Lock()
		last := e.gl.listings[len(e.gl.listings)-1]
		e.gl.mu.Unlock()
		if !strings.Contains(last, "search=REPO-9") {
			t.Errorf("GitLab listing query = %q, want the search", last)
		}
		wantCode(t, e.get(c, "/api/repositories/available?search="+strings.Repeat("x", 101)), "long search", http.StatusBadRequest)
	})
	t.Run("github", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gh.setRepo("acme/gadgets", githubRepo(true, true))
		e.gh.setRepo("acme/tools", githubRepo(true, true))
		e.cat["gh"].set("acme/gadgets", "acme/tools")
		c := e.signInGitHub(true)
		got := getJSON[availableBody](e, c, "/api/repositories/available?search=GAD")
		if !slices.Equal(projectsOf(got.Items), []string{"acme/gadgets"}) {
			t.Errorf("search = %v", projectsOf(got.Items))
		}
	})
}

// Each listing has an overall budget (20 s): when it runs out, what was
// read is listed, truncated; with nothing read it is 502.
func TestAvailableListingBudget(t *testing.T) {
	if listBudget != 20*time.Second || platformTimeout != 10*time.Second {
		t.Errorf("listBudget %v, platformTimeout %v; want 20s and 10s", listBudget, platformTimeout)
	}
	e := newEnv(t, nil)
	var all []string
	for i := range 150 { // two pages of 100
		p := fmt.Sprintf("acme/repo-%03d", i)
		all = append(all, p)
		e.gl.setProject(p, gitlabProject("private", 20, 0))
	}
	e.cat["main"].set(all...)
	c := e.signInGitLab(true)
	e.d.listBudget = 300 * time.Millisecond

	e.gl.set(func(*oauthServer) { e.gl.slowFrom, e.gl.slow = 2, 5*time.Second })
	got := getJSON[availableBody](e, c, "/api/repositories/available")
	if len(got.Items) != 100 || !got.Truncated {
		t.Errorf("available has %d items, truncated %v; want the first page, truncated", len(got.Items), got.Truncated)
	}

	// The review side's listing runs out too: what it read is used.
	e.gl.set(func(*oauthServer) { e.gl.slowFrom = 0 })
	e.cat["main"].mu.Lock()
	e.cat["main"].slow = 200 * time.Millisecond
	e.cat["main"].mu.Unlock()
	e.clock.advance(catalogTTL)
	got = getJSON[availableBody](e, c, "/api/repositories/available")
	if len(got.Items) == 0 || len(got.Items) >= 150 || !got.Truncated {
		t.Errorf("available has %d items, truncated %v; want some, truncated", len(got.Items), got.Truncated)
	}

	// Nothing read in time: 502.
	e.gl.set(func(*oauthServer) { e.gl.slowFrom = 1 })
	wantCode(t, e.get(c, "/api/repositories/available?search=repo"), "nothing read", http.StatusBadGateway)
}

// The whole request, renewing the user's token included, has a budget
// (45 s) below the server's write timeout: when it runs out, what was read
// is listed, truncated, and a renewal that outlasts it is not waited for.
func TestAvailableWholeBudget(t *testing.T) {
	if AvailableBudget != 45*time.Second || listBudget >= AvailableBudget {
		t.Errorf("AvailableBudget %v, listBudget %v; want 45s and less", AvailableBudget, listBudget)
	}
	e := newEnv(t, nil)
	// A token about to expire: every use renews it first.
	e.gl.set(func(*oauthServer) { e.gl.expiresIn = 60 })
	var all []string
	for i := range 150 { // two pages of 100
		p := fmt.Sprintf("acme/repo-%03d", i)
		all = append(all, p)
		e.gl.setProject(p, gitlabProject("private", 20, 0))
	}
	e.cat["main"].set(all...)
	c := e.signInGitLab(true)
	e.d.availableBudget = 500 * time.Millisecond
	e.d.listBudget = time.Minute

	// The listing's own budget is longer: the request's cuts it short.
	e.gl.set(func(*oauthServer) { e.gl.slowFrom, e.gl.slow = 2, 5*time.Second })
	start := time.Now()
	got := getJSON[availableBody](e, c, "/api/repositories/available")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("available took %v, want about the 500ms budget", took)
	}
	if len(got.Items) != 100 || !got.Truncated {
		t.Errorf("available has %d items, truncated %v; want the first page, truncated", len(got.Items), got.Truncated)
	}

	// A slow token renewal counts against the same budget.
	e.gl.set(func(*oauthServer) { e.gl.slowFrom, e.gl.tokenSlow = 0, 2*time.Second })
	start = time.Now()
	wantCode(t, e.get(c, "/api/repositories/available?search=repo"), "slow renewal", http.StatusBadGateway)
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("available took %v with a slow renewal, want about the 500ms budget", took)
	}
	// The renewal still finishes and is stored: the next request waits for
	// it and goes on with the renewed token.
	e.gl.set(func(*oauthServer) { e.gl.tokenSlow = 0 })
	e.d.availableBudget = AvailableBudget
	got = getJSON[availableBody](e, c, "/api/repositories/available?search=repo-00")
	if len(got.Items) != 10 || got.Truncated {
		t.Errorf("after the renewal, available has %d items, truncated %v", len(got.Items), got.Truncated)
	}
}
