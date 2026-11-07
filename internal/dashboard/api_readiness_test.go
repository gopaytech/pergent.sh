package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// fakeReadiness is the engine's side of the Readiness tab: RequestScan queues
// in the store as the engine does (5 minutes apart, one active), and both
// calls are recorded.
type fakeReadiness struct {
	st  *store.Store
	now func() time.Time

	mu       sync.Mutex
	asked    []string // "instance/project by actor"
	rechecks []string // "instance/project"
	// fail, when set, is RequestScan's error.
	fail error
}

func (f *fakeReadiness) RequestScan(ctx context.Context, instance, project, actor string) (store.ReadinessScan, error) {
	f.mu.Lock()
	f.asked = append(f.asked, instance+"/"+project+" by "+actor)
	fail := f.fail
	f.mu.Unlock()
	if fail != nil {
		return store.ReadinessScan{}, fail
	}
	now := f.now()
	sc, out, err := f.st.QueueReadinessScan(ctx, instance, project, actor, now, 5*time.Minute)
	switch {
	case err != nil:
		return store.ReadinessScan{}, err
	case out == store.ScanBusy:
		return sc, readiness.ErrScanBusy
	case out == store.ScanTooSoon:
		return sc, &readiness.ScanTooSoonError{Wait: 5*time.Minute - now.Sub(sc.QueuedAt)}
	}
	return sc, nil
}

func (f *fakeReadiness) RecheckReadiness(instance, project string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rechecks = append(f.rechecks, instance+"/"+project)
}

func (f *fakeReadiness) RecheckAllReadiness() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rechecks = append(f.rechecks, "*")
}

func (f *fakeReadiness) askedFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

func (f *fakeReadiness) clearRechecks() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rechecks = nil
}

func (f *fakeReadiness) recheckedFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rechecks)
}

// withReadiness gives the env's dashboard a fake engine for the readiness
// scans.
func (e *env) withReadiness() *fakeReadiness {
	f := &fakeReadiness{st: e.st, now: e.clock.now}
	e.d.readiness = f
	return f
}

// scanRow stores a scan of project on main in state, queued at queued; a
// done or failed scan finished then too. It returns the scan's ID.
func (e *env) scanRow(project string, state store.ScanState, queued time.Time, pod, job, errText string) string {
	e.t.Helper()
	var finished *time.Time
	if state == store.ScanDone || state == store.ScanFailed {
		finished = &queued
	}
	var podArg, jobArg any
	if pod != "" {
		podArg = pod
	}
	if job != "" {
		jobArg = job
	}
	var id string
	err := e.db.QueryRow(ctx, `INSERT INTO readiness_scans (id, instance, project, branch, commit, state, started_by,
			queued_at, finished_at, error, pod_result, job_result)
		VALUES (gen_random_uuid(), 'main', $1, 'main', $2, $3, 'switch-on', $4, $5, $6, $7::jsonb, $8::jsonb)
		RETURNING id::text`,
		project, scanCommit, string(state), queued, finished, errText, podArg, jobArg).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// scanCommit is the commit every stored scan was at.
const scanCommit = "c0ffee1234567890c0ffee1234567890c0ffee12"

const (
	podAllPassed = `{"version":1,"approvals":{"state":"passed","count":2},"protected":{"state":"passed"},
		"requiredChecks":{"state":"unknown","reason":"the review App can't see this branch's required checks"}}`
	jobGoRepo = `{"version":1,"commit":"` + scanCommit + `","files":3,"sourceFiles":3,
		"context":{"agentFile":"AGENTS.md","readme":"README.md"},
		"skills":[{"name":"go","folder":"library","hasPaths":true,"files":3}]}`
)

// signInOn is alice's session with role on every repository of main (the
// projects named are the ones the test is about).
func (e *env) signInOn(role string, _ ...string) *http.Cookie {
	e.t.Helper()
	e.aliceOn("main", role)
	return e.aliceSession()
}

func readinessURL(project string) string { return "/api/repositories/main/" + project + "/readiness" }

func scanURL(project string) string { return readinessURL(project) + "/scan" }

// item finds the checklist item key.
func item(t *testing.T, c *readiness.Checklist, key string) readiness.Item {
	t.Helper()
	for _, s := range c.Sections {
		for _, it := range s.Items {
			if it.Key == key {
				return it
			}
		}
	}
	t.Fatalf("no checklist item %q", key)
	return readiness.Item{}
}

// A repository never scanned has no latest scan and no checklist; Scan now
// is offered to maintainers and admins only.
func TestReadinessNotScanned(t *testing.T) {
	for _, tc := range []struct {
		role    string
		canScan bool
	}{{store.RoleViewer, false}, {store.RoleMaintainer, true}, {store.RoleAdmin, true}} {
		t.Run(tc.role, func(t *testing.T) {
			e := newEnv(t, nil)
			e.withReadiness()
			e.dashboardRepo("main", "acme/gadgets", nil)
			c := e.signInOn(tc.role, "acme/gadgets", "acme/widgets")

			rec := e.get(c, readinessURL("acme/gadgets"))
			v := decode[ReadinessView](t, rec, "GET readiness", http.StatusOK)
			if v.Latest != nil || v.Active != nil || v.Checklist != nil {
				t.Errorf("view = %s; want no scan and no checklist", rec.Body)
			}
			if v.CanScan != tc.canScan || v.ConfigRepo {
				t.Errorf("canScan, configRepo = %v, %v; want %v, false", v.CanScan, v.ConfigRepo, tc.canScan)
			}
			for _, key := range []string{`"latest":null`, `"active":null`, `"checklist":null`, `"canScan"`,
				`"configRepo"`, `"followsDefaults"`, `"applyBase":[]`} {
				if !strings.Contains(rec.Body.String(), key) {
					t.Errorf("body %s lacks %s", rec.Body, key)
				}
			}

			cfg := decode[ReadinessView](t, e.get(c, readinessURL("acme/widgets")), "GET config readiness", http.StatusOK)
			if !cfg.ConfigRepo || cfg.CanScan != tc.canScan {
				t.Errorf("config repository: configRepo, canScan = %v, %v; want true, %v", cfg.ConfigRepo, cfg.CanScan, tc.canScan)
			}
		})
	}
}

// A finished scan gives the 4 sections, from the stored results and the
// skills now; the live single_approval item makes approvals a warning.
// "Couldn't check" stays unknown, never passed.
func TestReadinessChecklist(t *testing.T) {
	e := newEnv(t, nil)
	e.withReadiness()
	e.skill("go", "Review Go.")
	if err := e.st.AddDefaultSkills(ctx, []string{"go"}); err != nil {
		t.Fatal(err)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	queued := e.clock.now().Add(-2 * time.Hour)
	id := e.scanRow("acme/gadgets", store.ScanDone, queued, podAllPassed, jobGoRepo, "")
	c := e.signInOn(store.RoleViewer, "acme/gadgets")

	rec := e.get(c, readinessURL("acme/gadgets"))
	v := decode[ReadinessView](t, rec, "GET readiness", http.StatusOK)
	if v.Latest == nil || v.Latest.ID != id || v.Latest.State != "done" || v.Latest.Commit != scanCommit ||
		v.Latest.StartedBy != "switch-on" || v.Latest.Branch != "main" || !v.Latest.QueuedAt.Equal(queued) ||
		v.Latest.FinishedAt == nil || !v.Latest.FinishedAt.Equal(queued) {
		t.Errorf("latest = %+v", v.Latest)
	}
	if v.Active != nil {
		t.Errorf("active = %+v; want none", v.Active)
	}
	if v.Checklist == nil {
		t.Fatalf("checklist = nil; body %s", rec.Body)
	}
	var keys []string
	for _, s := range v.Checklist.Sections {
		keys = append(keys, s.Key)
	}
	if !slices.Equal(keys, []string{"context", "process", "skills", "size"}) {
		t.Errorf("sections = %v", keys)
	}
	for _, key := range []string{`"sections"`, `"items"`, `"key":"approvals"`, `"state":"passed"`, `"reason"`,
		`"startedBy":"switch-on"`, `"queuedAt"`, `"finishedAt"`} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Errorf("body lacks %s: %s", key, rec.Body)
		}
	}
	if got := item(t, v.Checklist, "approvals"); got.State != readiness.Passed {
		t.Errorf("approvals = %+v; want passed", got)
	}
	if got := item(t, v.Checklist, "required_checks"); got.State != readiness.Unknown ||
		got.Reason != "the review App can't see this branch's required checks" {
		t.Errorf("required_checks = %+v; want unknown with its reason", got)
	}
	if got := item(t, v.Checklist, "skill_match"); got.State != readiness.Passed {
		t.Errorf("skill_match = %+v; want passed (the default list's go covers the files)", got)
	}

	if err := e.st.SetAttention(ctx, "main", "acme/gadgets", "single_approval", "one approval"); err != nil {
		t.Fatal(err)
	}
	v = decode[ReadinessView](t, e.get(c, readinessURL("acme/gadgets")), "GET readiness", http.StatusOK)
	if got := item(t, v.Checklist, "approvals"); got.State != readiness.Warning {
		t.Errorf("approvals with single_approval set = %+v; want warning", got)
	}
}

// A failed scan still shows the settings the pod read; the file checks say
// why they couldn't be read. A queued scan is the active one.
func TestReadinessFailedScan(t *testing.T) {
	e := newEnv(t, nil)
	e.withReadiness()
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.scanRow("acme/gadgets", store.ScanFailed, e.clock.now().Add(-time.Hour), podAllPassed, "",
		"the repository is too large to clone (the scan has 2 GiB of disk)")
	active := e.scanRow("acme/gadgets", store.ScanQueued, e.clock.now(), "", "", "")
	c := e.signInOn(store.RoleViewer, "acme/gadgets")

	v := decode[ReadinessView](t, e.get(c, readinessURL("acme/gadgets")), "GET readiness", http.StatusOK)
	if v.Active == nil || v.Active.ID != active || v.Active.State != "queued" || v.Active.FinishedAt != nil {
		t.Errorf("active = %+v", v.Active)
	}
	if v.Checklist == nil {
		t.Fatal("checklist = nil after a failed scan")
	}
	if got := item(t, v.Checklist, "protected"); got.State != readiness.Passed {
		t.Errorf("protected = %+v; want what the pod read", got)
	}
	want := "couldn't scan files: the repository is too large to clone (the scan has 2 GiB of disk)"
	if got := item(t, v.Checklist, "readme"); got.State != readiness.Unknown || got.Reason != want {
		t.Errorf("readme = %+v; want unknown %q", got, want)
	}
}

// Review Focus 3: a user who can't see the repository gets the generic 404
// for the view and for Scan now, with nothing of the repository in it, and
// nothing is queued.
func TestReadinessHiddenRepo404(t *testing.T) {
	e := newEnv(t, nil)
	f := e.withReadiness()
	e.dashboardRepo("main", "acme/hidden", nil)
	e.scanRow("acme/hidden", store.ScanDone, e.clock.now().Add(-time.Hour), podAllPassed, jobGoRepo, "")
	// alice is an admin who can't see acme/hidden on GitLab.
	c := e.userOnMain(true, "acme/gadgets")
	if _, se := e.session(c); !se.Admin || se.CanSee("main", "acme/hidden") {
		t.Fatalf("session = %+v; want an admin who can't see acme/hidden", se)
	}
	unknown := e.get(c, readinessURL("acme/nothing-here"))
	wantCode(t, unknown, "GET readiness of an unknown repository", http.StatusNotFound)

	for _, rec := range []*httptest.ResponseRecorder{
		e.get(c, readinessURL("acme/hidden")),
		e.get(c, readinessURL("ACME/Hidden")),
		e.send(c, "POST", scanURL("acme/hidden"), ""),
		e.send(c, "POST", scanURL("acme/nothing-here"), ""),
	} {
		wantCode(t, rec, "readiness of acme/hidden", http.StatusNotFound)
		if rec.Body.String() != unknown.Body.String() {
			t.Errorf("body = %s; want the unknown repository's %s", rec.Body, unknown.Body)
		}
		for _, leak := range []string{"hidden", scanCommit[:7], "AGENTS.md", "approvals"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("body %s holds %q", rec.Body, leak)
			}
		}
	}
	if got := f.askedFor(); len(got) != 0 {
		t.Errorf("scans asked for: %v", got)
	}
	if _, active, err := e.st.ReadinessScans(ctx, "main", "acme/hidden"); err != nil || active != nil {
		t.Errorf("active scan = %+v, %v; want none", active, err)
	}
}

// Scan now is for maintainers and admins, config repositories included.
func TestScanNowRoles(t *testing.T) {
	t.Run("viewer", func(t *testing.T) {
		e := newEnv(t, nil)
		f := e.withReadiness()
		e.dashboardRepo("main", "acme/gadgets", nil)
		c := e.signInOn(store.RoleViewer, "acme/gadgets", "acme/widgets")
		for _, project := range []string{"acme/gadgets", "acme/widgets"} {
			rec := e.send(c, "POST", scanURL(project), "")
			body := decode[map[string]string](t, rec, "POST scan as a viewer", http.StatusForbidden)
			if body["error"] != msgNeedsRoleScan {
				t.Errorf("error = %q; want %q", body["error"], msgNeedsRoleScan)
			}
		}
		if got := f.askedFor(); len(got) != 0 {
			t.Errorf("scans asked for: %v", got)
		}
	})
	t.Run("maintainer", func(t *testing.T) {
		e := newEnv(t, nil)
		f := e.withReadiness()
		e.dashboardRepo("main", "acme/gadgets", nil)
		c := e.signInOn(store.RoleMaintainer, "acme/gadgets", "acme/widgets")
		for _, project := range []string{"acme/gadgets", "acme/widgets"} {
			rec := e.send(c, "POST", scanURL(project), "")
			body := decode[struct{ Scan ScanView }](t, rec, "POST scan of "+project, http.StatusAccepted)
			if body.Scan.ID == "" || body.Scan.State != "queued" || body.Scan.StartedBy != "alice@example.com" ||
				body.Scan.FinishedAt != nil {
				t.Errorf("scan = %+v", body.Scan)
			}
			if !strings.Contains(rec.Body.String(), `"scan":{"id":`) {
				t.Errorf("body = %s; want {\"scan\": {...}}", rec.Body)
			}
		}
		want := []string{"main/acme/gadgets by alice@example.com", "main/acme/widgets by alice@example.com"}
		if got := f.askedFor(); !slices.Equal(got, want) {
			t.Errorf("scans asked for = %v; want %v", got, want)
		}
	})
}

// A repository switched off under the request, or scans being off, is the
// generic 404; so is a dashboard with no engine for the scans.
func TestScanNowNotOn(t *testing.T) {
	for _, fail := range []error{readiness.ErrNotSwitchedOn, readiness.ErrScansOff, nil} {
		name := "no engine"
		if fail != nil {
			name = fail.Error()
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			if fail != nil {
				e.withReadiness().fail = fail
			}
			e.dashboardRepo("main", "acme/gadgets", nil)
			c := e.signInOn(store.RoleMaintainer, "acme/gadgets")
			unknown := e.send(c, "POST", scanURL("acme/nothing-here"), "")
			rec := e.send(c, "POST", scanURL("acme/gadgets"), "")
			wantCode(t, rec, "POST scan", http.StatusNotFound)
			if rec.Body.String() != unknown.Body.String() {
				t.Errorf("body = %s; want %s", rec.Body, unknown.Body)
			}
			if got := e.auditOf("readiness_scan_requested"); len(got) != 0 {
				t.Errorf("audit = %+v; want none", got)
			}
		})
	}
}

// Scan now while a scan is queued is 409 with that scan; within 5 minutes
// of the latest one it is 429 with the wait, rounded up to whole minutes in
// the message.
func TestScanNowTwice(t *testing.T) {
	e := newEnv(t, nil)
	e.withReadiness()
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.signInOn(store.RoleMaintainer, "acme/gadgets")

	first := decode[struct{ Scan ScanView }](t, e.send(c, "POST", scanURL("acme/gadgets"), ""), "first scan", http.StatusAccepted)
	busy := decode[struct {
		Error string
		Scan  ScanView
	}](t, e.send(c, "POST", scanURL("acme/gadgets"), ""), "second scan", http.StatusConflict)
	if busy.Error != "a scan is already queued or running" || busy.Scan.ID != first.Scan.ID {
		t.Errorf("409 = %+v; want the queued scan %s", busy, first.Scan.ID)
	}

	e.exec(`UPDATE readiness_scans SET state = 'done', finished_at = queued_at WHERE id = $1`, first.Scan.ID)
	e.clock.advance(time.Minute)
	for _, tc := range []struct {
		after   time.Duration
		seconds int
		message string
	}{
		{0, 240, "this repository was scanned less than 5 minutes ago; try again in 4 minutes"},
		{30 * time.Second, 210, "this repository was scanned less than 5 minutes ago; try again in 4 minutes"},
		{3 * time.Minute, 30, "this repository was scanned less than 5 minutes ago; try again in 1 minute"},
	} {
		e.clock.advance(tc.after)
		rec := e.send(c, "POST", scanURL("acme/gadgets"), "")
		soon := decode[struct {
			Error       string
			WaitSeconds int
		}](t, rec, "scan too soon", http.StatusTooManyRequests)
		if soon.Error != tc.message || soon.WaitSeconds != tc.seconds {
			t.Errorf("429 = %+v; want %q, %d", soon, tc.message, tc.seconds)
		}
		if got := rec.Header().Get("Retry-After"); got != strconv.Itoa(tc.seconds) {
			t.Errorf("Retry-After = %q; want %d", got, tc.seconds)
		}
		if !strings.Contains(rec.Body.String(), `"waitSeconds"`) {
			t.Errorf("body = %s", rec.Body)
		}
	}
}

// Only an accepted Scan now is audited.
func TestScanNowAudited(t *testing.T) {
	e := newEnv(t, nil)
	e.withReadiness()
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.signInOn(store.RoleMaintainer, "acme/gadgets")

	wantCode(t, e.send(c, "POST", scanURL("acme/gadgets"), ""), "first scan", http.StatusAccepted)
	got := e.auditOf("readiness_scan_requested")
	if len(got) != 1 || got[0].Instance != "main" || got[0].Project != "acme/gadgets" || got[0].Actor != "alice@example.com" {
		t.Fatalf("audit = %+v; want one entry for alice on main acme/gadgets", got)
	}
	wantCode(t, e.send(c, "POST", scanURL("acme/gadgets"), ""), "busy scan", http.StatusConflict)
	e.exec(`UPDATE readiness_scans SET state = 'done', finished_at = queued_at`)
	wantCode(t, e.send(c, "POST", scanURL("acme/gadgets"), ""), "scan too soon", http.StatusTooManyRequests)
	if got := e.auditOf("readiness_scan_requested"); len(got) != 1 {
		t.Errorf("audit = %+v; want only the accepted scan's", got)
	}
}

// POST .../settings/reset still resets; any other POST suffix is 404.
func TestResetStillWorks(t *testing.T) {
	e := newEnv(t, nil)
	e.withReadiness()
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) { r.Approve = ptr(true) })
	c := e.signInOn(store.RoleMaintainer, "acme/gadgets")

	wantCode(t, e.send(c, "POST", "/api/repositories/main/acme/gadgets/settings/reset", ""), "reset", http.StatusOK)
	if r, found := e.storedRepo("main", "acme/gadgets"); !found || r.Approve != nil {
		t.Errorf("after reset = %+v, %v; want approve back to the default", r, found)
	}
	if got := e.auditOf("settings_reset"); len(got) != 1 {
		t.Errorf("settings_reset audit = %+v", got)
	}
	for _, target := range []string{"/api/repositories/main/acme/gadgets", "/api/repositories/main/acme/gadgets/readiness",
		"/api/repositories/main/acme/gadgets/other"} {
		wantCode(t, e.send(c, "POST", target, ""), "POST "+target, http.StatusNotFound)
	}
}

// Saving a repository's skills or repository skills rechecks it; saving
// anything else doesn't. Saving the default list rechecks every repository
// that follows it.
func TestRecheckOnSettingsSave(t *testing.T) {
	e := newEnv(t, nil)
	f := e.withReadiness()
	e.skill("go", "Review Go.")
	e.skill("ts", "Review TypeScript.")
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.signInOn(store.RoleMaintainer, "acme/gadgets")
	settingsURL := "/api/repositories/main/acme/gadgets/settings"

	wantCode(t, e.send(c, "PUT", settingsURL, `{"skills":["go"]}`), "PUT skills", http.StatusOK)
	wantCode(t, e.send(c, "PUT", settingsURL, `{"approve":false}`), "PUT approve", http.StatusOK)
	wantCode(t, e.send(c, "PUT", settingsURL, `{"skills":["go"]}`), "PUT the same skills", http.StatusOK)
	wantCode(t, e.send(c, "PUT", settingsURL, `{"repoSkills":["style"]}`), "PUT repoSkills", http.StatusOK)
	wantCode(t, e.send(c, "POST", settingsURL+"/reset", ""), "reset", http.StatusOK)
	want := []string{"main/acme/gadgets", "main/acme/gadgets", "main/acme/gadgets"}
	if got := f.recheckedFor(); !slices.Equal(got, want) {
		t.Errorf("rechecks = %v; want %v (skills, repoSkills, reset)", got, want)
	}

	op := e.operator()
	f.clearRechecks()
	wantCode(t, e.sendJSON(op, "PUT", "/api/admin/defaults", map[string]any{"approve": false}), "PUT defaults approve", http.StatusOK)
	if got := f.recheckedFor(); len(got) != 0 {
		t.Errorf("rechecks after a defaults save without skills = %v", got)
	}
	wantCode(t, e.sendJSON(op, "PUT", "/api/admin/defaults", map[string]any{"skills": []string{"ts"}}), "PUT defaults skills", http.StatusOK)
	if got := f.recheckedFor(); !slices.Equal(got, []string{"/"}) {
		t.Errorf("rechecks after a defaults save = %v; want [/]", got)
	}

	f.clearRechecks()
	wantCode(t, e.sendJSON(op, "POST", "/api/admin/skills", map[string]any{"name": "py", "content": "Review Python."}),
		"create without addToDefaults", http.StatusCreated)
	if got := f.recheckedFor(); len(got) != 0 {
		t.Errorf("rechecks after a create = %v", got)
	}
	wantCode(t, e.sendJSON(op, "POST", "/api/admin/skills", map[string]any{"name": "rb", "content": "Review Ruby.", "addToDefaults": true}),
		"create with addToDefaults", http.StatusCreated)
	if got := f.recheckedFor(); !slices.Equal(got, []string{"/"}) {
		t.Errorf("rechecks after a create added to the defaults = %v; want [/]", got)
	}

	f.clearRechecks()
	e.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"skills/style.md": "Be kind.\n"})
	id, _ := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0", "path": "skills"})
	e.skillAction(op, id, "install", map[string]any{"names": []string{"style"}, "addToDefaults": true})
	if got := f.recheckedFor(); !slices.Equal(got, []string{"/"}) {
		t.Errorf("rechecks after an install added to the defaults = %v; want [/]", got)
	}
}

// Changing a library skill's text, by an edit or an update from its source,
// rechecks every repository ("*"): any of them may use it. A save that
// changes nothing, or is refused, rechecks none.
func TestRecheckOnSkillContentChange(t *testing.T) {
	e := newEnv(t, nil)
	f := e.withReadiness()
	e.skill("go", "Review Go.")
	op := e.operator()

	wantCode(t, e.sendJSON(op, "PUT", "/api/admin/skills/go", map[string]any{"content": "Review Go well.", "version": 1}),
		"edit", http.StatusOK)
	if got := f.recheckedFor(); !slices.Equal(got, []string{"*"}) {
		t.Errorf("rechecks after an edit = %v; want [*]", got)
	}
	f.clearRechecks()
	wantCode(t, e.sendJSON(op, "PUT", "/api/admin/skills/go", map[string]any{"content": "Stale.", "version": 1}),
		"stale edit", http.StatusConflict)
	if got := f.recheckedFor(); len(got) != 0 {
		t.Errorf("rechecks after a refused edit = %v", got)
	}

	e.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"skills/style.md": "Old text."})
	e.skillRepo("main", "acme/skills", "v1.1", commitV11, map[string]string{"skills/style.md": "New text."})
	id, _ := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0", "path": "skills"})
	e.skillAction(op, id, "install", map[string]any{"names": []string{"style"}})
	f.clearRechecks()
	same, _ := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0", "fromRef": "v1.0",
		"path": "skills", "purpose": "update"})
	e.skillAction(op, same, "update", map[string]any{"names": []string{"style"}})
	if got := f.recheckedFor(); len(got) != 0 {
		t.Errorf("rechecks after an update that saved nothing = %v", got)
	}
	upd, _ := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.1", "fromRef": "v1.0",
		"path": "skills", "purpose": "update"})
	e.skillAction(op, upd, "update", map[string]any{"names": []string{"style"}})
	if got := f.recheckedFor(); !slices.Equal(got, []string{"*"}) {
		t.Errorf("rechecks after an update from the source = %v; want [*]", got)
	}
}

// Use these skills starts from today's list: the default list for a
// repository that follows it, else the repository's own.
func TestApplyBase(t *testing.T) {
	e := newEnv(t, nil)
	e.withReadiness()
	e.skill("go", "Review Go.")
	e.skill("ts", "Review TypeScript.")
	if err := e.st.AddDefaultSkills(ctx, []string{"go", "ts"}); err != nil {
		t.Fatal(err)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.dashboardRepo("main", "acme/tools", func(r *store.DashboardRepo) { r.Skills = []string{"ts"} })
	c := e.signInOn(store.RoleMaintainer, "acme/gadgets", "acme/tools")

	v := decode[ReadinessView](t, e.get(c, readinessURL("acme/gadgets")), "GET gadgets", http.StatusOK)
	if !v.FollowsDefaults || !slices.Equal(v.ApplyBase, []string{"go", "ts"}) {
		t.Errorf("gadgets: followsDefaults, applyBase = %v, %v; want true, [go ts]", v.FollowsDefaults, v.ApplyBase)
	}
	v = decode[ReadinessView](t, e.get(c, readinessURL("acme/tools")), "GET tools", http.StatusOK)
	if v.FollowsDefaults || !slices.Equal(v.ApplyBase, []string{"ts"}) {
		t.Errorf("tools: followsDefaults, applyBase = %v, %v; want false, [ts]", v.FollowsDefaults, v.ApplyBase)
	}
}
