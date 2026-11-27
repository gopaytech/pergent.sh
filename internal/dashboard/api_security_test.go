package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/security"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// fakeSecurity is the engine's side of the Security tab: Start queues in the
// store as the engine does, unless fail is set; Cancel asks the store; every
// Start is recorded.
type fakeSecurity struct {
	st  *store.Store
	now func() time.Time

	mu     sync.Mutex
	starts []security.Start
	// fail, when set, is RequestSecurityScan's error; with security.ErrBusy
	// the repository's active scan comes with it.
	fail error
	off  string
}

func (f *fakeSecurity) RequestSecurityScan(ctx context.Context, s security.Start) (store.SecurityScan, error) {
	f.mu.Lock()
	f.starts = append(f.starts, s)
	fail := f.fail
	f.mu.Unlock()
	if errors.Is(fail, security.ErrBusy) {
		active, err := f.st.ActiveSecurityScan(ctx, s.Instance, s.Project)
		if err != nil || active == nil {
			return store.SecurityScan{}, errors.New("no active scan to be busy with")
		}
		return *active, fail
	}
	if fail != nil {
		return store.SecurityScan{}, fail
	}
	sc, err := f.st.QueueSecurityScan(ctx, store.SecurityScanRequest{Instance: s.Instance, Project: s.Project,
		ProjectID: testID(s.Project), Branch: "main", Commit: securityCommit, StartedBy: s.Actor, PastQuota: s.Admin,
		TimeoutSeconds: 3600}, f.now())
	if errors.Is(err, store.ErrSecurityScanBusy) {
		return sc, security.ErrBusy
	}
	return sc, err
}

func (f *fakeSecurity) CancelSecurityScan(ctx context.Context, instance, project, id string) (store.SecurityScan, error) {
	sc, err := f.st.SecurityScan(ctx, id)
	if err != nil {
		return store.SecurityScan{}, err
	}
	if sc.Instance != instance || !strings.EqualFold(sc.Project, project) {
		return store.SecurityScan{}, store.ErrSecurityScanNotFound
	}
	sc, ok, err := f.st.RequestSecurityCancel(ctx, id, f.now())
	if err == nil && !ok {
		return sc, security.ErrEnded
	}
	return sc, err
}

func (f *fakeSecurity) StartOff() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.off
}

func (f *fakeSecurity) set(fail error, off string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail, f.off = fail, off
}

func (f *fakeSecurity) started() []security.Start {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.starts)
}

// withSecurity gives the env's dashboard a fake engine for security scans.
func (e *env) withSecurity() *fakeSecurity {
	f := &fakeSecurity{st: e.st, now: e.clock.now}
	e.d.securityScans = f
	return f
}

// securityCommit is the commit of every stored scan.
const securityCommit = "0123456789abcdef0123456789abcdef01234567"

// securityResult is a complete result, with a SARIF, that finds a finding
// per fingerprint.
func securityResult(fps ...string) store.SecurityResult {
	res := store.SecurityResult{Status: store.SecurityComplete, UsageSource: "stats", SkillName: "security-audit",
		SkillCommit: "abc123", SARIF: []byte(`{"version": "2.1.0", "runs": []}`)}
	for _, fp := range fps {
		res.Findings = append(res.Findings, store.SecurityFindingInput{Fingerprint: fp, RuleID: "injection", CWE: "CWE-89",
			Tags: []string{"CWE-89", "sql"}, Severity: store.SeverityHigh, Title: "SQL built from input " + fp,
			Description: "The query is built from input.", Exploit: "Send a quote.", FixHint: "Use a bound value.",
			Confidence: "high", File: "db/" + fp + ".go", StartLine: 12})
	}
	return res
}

// securityScanDone stores a scan of project on main that ran for a minute
// and ended with res, and returns it as stored. No other scan may be queued.
func (e *env) securityScanDone(project string, res store.SecurityResult) store.SecurityScan {
	e.t.Helper()
	sc, err := e.st.QueueSecurityScan(ctx, store.SecurityScanRequest{Instance: "main", Project: project,
		ProjectID: testID(project), Branch: "main", Commit: securityCommit, StartedBy: "carol", TimeoutSeconds: 3600},
		e.clock.now())
	if err != nil {
		e.t.Fatal(err)
	}
	claimed, ok, err := e.st.ClaimSecurityScan(ctx, 5, e.clock.now(), e.clock.now().Add(time.Hour))
	if err != nil || !ok || claimed.ID != sc.ID {
		e.t.Fatalf("ClaimSecurityScan = %+v, %v, %v; want scan %s", claimed, ok, err, sc.ID)
	}
	e.clock.advance(time.Minute)
	res.Commit = securityCommit
	if ok, err := e.st.FinishSecurityScan(ctx, sc.ID, nil, res, e.clock.now()); err != nil || !ok {
		e.t.Fatalf("FinishSecurityScan = %v, %v", ok, err)
	}
	e.clock.advance(time.Minute)
	got, err := e.st.SecurityScan(ctx, sc.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// findingID is the ID of project's finding fp, as a path part.
func (e *env) findingID(project, fp string) string {
	e.t.Helper()
	rows, _, err := e.st.SecurityFindings(ctx, "main", project, store.SecurityFindingFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, f := range rows {
		if f.Fingerprint == fp {
			return strconv.FormatInt(f.ID, 10)
		}
	}
	e.t.Fatalf("no finding %s in %s", fp, project)
	return ""
}

// findingState is the state of project's finding id.
func (e *env) findingState(project, id string) string {
	e.t.Helper()
	n, _ := strconv.ParseInt(id, 10, 64)
	f, found, err := e.st.SecurityFinding(ctx, "main", project, n)
	if err != nil || !found {
		e.t.Fatalf("SecurityFinding(%s, %s) = %v, %v", project, id, found, err)
	}
	return f.State
}

// securityAudits are the audit entries of the security routes, oldest
// first, as "action project detail".
func (e *env) securityAudits() []string {
	e.t.Helper()
	var out []string
	for _, a := range e.auditEntries() {
		if strings.HasPrefix(a.Action, "security_") || a.Action == "settings_updated" || a.Action == "chain_changed" {
			out = append(out, a.Action+" "+a.Project+" "+a.Detail)
		}
	}
	slices.Reverse(out)
	return out
}

// maintainerOn is the session of a new account that maintains every
// repository of main.
func (e *env) maintainerOn(email string) *http.Cookie {
	e.t.Helper()
	id := e.sso(email)
	e.team("maintainers "+email, []store.TeamMember{{AccountID: id, Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "main"})
	return e.sessionFor(id)
}

func securityURL(project, rest string) string {
	return "/api/repositories/main/" + project + "/security" + rest
}

// The path's shapes, the project before them, and what is not one.
func TestParseSecurityPath(t *testing.T) {
	t.Parallel()
	const scan = "0b5c3f4e-1d2a-4b6c-8e9f-0a1b2c3d4e5f"
	for _, tc := range []struct {
		rest, project string
		want          securityPath
	}{
		{"acme/widgets/security", "acme/widgets", securityPath{}},
		{"acme/widgets/security/findings", "acme/widgets", securityPath{Kind: "findings"}},
		{"acme/widgets/security/findings/12", "acme/widgets", securityPath{Kind: "findings", ID: "12"}},
		{"acme/widgets/security/findings/12/dismiss", "acme/widgets", securityPath{Kind: "findings", ID: "12", Action: "dismiss"}},
		{"acme/widgets/security/findings/12/restore", "acme/widgets", securityPath{Kind: "findings", ID: "12", Action: "restore"}},
		{"acme/widgets/security/scans", "acme/widgets", securityPath{Kind: "scans"}},
		{"g/sub/w/security/scans/" + scan, "g/sub/w", securityPath{Kind: "scans", ID: scan}},
		{"acme/widgets/security/scans/" + scan + "/sarif", "acme/widgets", securityPath{Kind: "scans", ID: scan, Action: "sarif"}},
		{"acme/widgets/security/scans/" + scan + "/cancel", "acme/widgets", securityPath{Kind: "scans", ID: scan, Action: "cancel"}},
		{"acme/widgets/security/settings", "acme/widgets", securityPath{Kind: "settings"}},
		{"acme/widgets/security/chain", "acme/widgets", securityPath{Kind: "chain"}},
		{"acme/widgets/security/chain/reset", "acme/widgets", securityPath{Kind: "chain", Action: "reset"}},
		// A repository named security, and its own tab.
		{"acme/security", "acme", securityPath{}},
		{"acme/security/security", "acme/security", securityPath{}},
	} {
		project, p, ok := parseSecurityPath(tc.rest)
		if !ok || project != tc.project || p != tc.want {
			t.Errorf("parseSecurityPath(%q) = %q, %+v, %v; want %q, %+v", tc.rest, project, p, ok, tc.project, tc.want)
		}
	}
	for _, rest := range []string{
		"security", "/security", "acme/widgets", "acme/widgets/chain", "acme/widgets/security/findings/0",
		"acme/widgets/security/findings/-1", "acme/widgets/security/findings/x", "acme/widgets/security/scans/12",
		"acme/widgets/security/scans/" + scan + "/dismiss", "acme/widgets/security/findings/12/sarif",
		"acme/widgets/security/settings/reset", "acme/widgets/security/chain/x", "acme/widgets/security/findings/12/x",
		"acme/widgets/security/other",
	} {
		if project, p, ok := parseSecurityPath(rest); ok {
			t.Errorf("parseSecurityPath(%q) = %q, %+v; want no route", rest, project, p)
		}
	}
}

// Which method each shape takes.
func TestSecurityHandles(t *testing.T) {
	t.Parallel()
	const scan = "0b5c3f4e-1d2a-4b6c-8e9f-0a1b2c3d4e5f"
	for _, tc := range []struct {
		rest string
		want []string
	}{
		{"", []string{"GET"}},
		{"/findings", []string{"GET"}},
		{"/findings/3", []string{"GET"}},
		{"/findings/3/dismiss", []string{"POST"}},
		{"/findings/3/restore", []string{"POST"}},
		{"/scans", []string{"GET", "POST"}},
		{"/scans/" + scan, []string{"GET"}},
		{"/scans/" + scan + "/sarif", []string{"GET"}},
		{"/scans/" + scan + "/cancel", []string{"POST"}},
		{"/settings", []string{"GET", "PUT"}},
		{"/chain", []string{"GET", "PUT"}},
		{"/chain/reset", []string{"POST"}},
	} {
		_, p, ok := parseSecurityPath("acme/w/security" + tc.rest)
		if !ok {
			t.Fatalf("%q is no route", tc.rest)
		}
		var got []string
		for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
			if securityHandles(m, p) {
				got = append(got, m)
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s takes %v; want %v", tc.rest, got, tc.want)
		}
	}
}

func TestSarifName(t *testing.T) {
	t.Parallel()
	for project, want := range map[string]string{
		"acme/widgets":        "widgets-scan-7.sarif",
		"g/sub/my_repo.v2":    "my_repo.v2-scan-7.sarif",
		`acme/we"ird; name=x`: "weirdnamex-scan-7.sarif",
		"acme/..":             "repository-scan-7.sarif",
		"acme/ünïcode-only-ü": "ncode-only--scan-7.sarif",
	} {
		if got := sarifName(project, 7); got != want {
			t.Errorf("sarifName(%q) = %q; want %q", project, got, want)
		}
	}
}

// A code link is an http or https address with no user in it, or none.
func TestCodeLink(t *testing.T) {
	t.Parallel()
	for u, want := range map[string]string{
		"https://gitlab.example.com/acme/w/-/blob/abc/a.go#L3":         "https://gitlab.example.com/acme/w/-/blob/abc/a.go#L3",
		"http://git.example.com/acme/w/blob/abc/a.go":                  "http://git.example.com/acme/w/blob/abc/a.go",
		"https://user:token@gitlab.example.com/acme/w/-/blob/abc/a.go": "",
		"javascript:alert(1)//a.go":                                    "",
		"ssh://git@gitlab.example.com/acme/w/-/blob/abc/a.go":          "",
		"/acme/w/-/blob/abc/a.go":                                      "",
		"":                                                             "",
	} {
		if got := codeLink(u); got != want {
			t.Errorf("codeLink(%q) = %q; want %q", u, got, want)
		}
	}
}

// Viewers read every route and get 403 on every write, with nothing
// changed; a maintainer's writes pass; a finding or a scan of another
// repository is 404 (Review Focus 3); with no engine every route is 404.
func TestSecurityRoutesRoles(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.withSecurity()
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.dashboardRepo("main", "acme/tools", nil)
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/gadgets"), Path: "acme/gadgets",
		HTTPURLToRepo: "https://gitlab.example.com/acme/gadgets.git"}); err != nil {
		t.Fatal(err)
	}
	sc := e.securityScanDone("acme/gadgets", securityResult("fp-gadgets-1"))
	other := e.securityScanDone("acme/tools", securityResult("fp-tools-1"))
	fid, oid := e.findingID("acme/gadgets", "fp-gadgets-1"), e.findingID("acme/tools", "fp-tools-1")
	viewer := e.signInOn(store.RoleViewer)
	maintainer := e.maintainerOn("bob@example.com")
	url := func(rest string) string { return securityURL("acme/gadgets", rest) }

	t.Run("a viewer reads", func(t *testing.T) {
		for _, rest := range []string{"", "/findings", "/findings/" + fid, "/scans", "/scans/" + sc.ID,
			"/scans/" + sc.ID + "/sarif", "/settings", "/chain"} {
			wantCode(t, e.get(viewer, url(rest)), "GET "+rest, http.StatusOK)
		}
		v := getJSON[SecurityView](e, viewer, url(""))
		if v.CanStart || v.CanManage || v.Last == nil || v.Last.ID != sc.ID || v.Counts.High != 1 {
			t.Errorf("viewer's tab = %+v", v)
		}
		f := getJSON[SecurityFindingView](e, viewer, url("/findings/"+fid))
		if f.CanManage || f.Finding.FirstScan != 1 || f.Finding.CWE != "CWE-89" || f.Finding.RuleID != "injection" ||
			f.CodeURL != "https://gitlab.example.com/acme/gadgets/-/blob/"+securityCommit+"/db/fp-gadgets-1.go#L12" {
			t.Errorf("viewer's finding = %+v", f)
		}
	})
	writes := []struct{ method, rest, body string }{
		{"POST", "/findings/" + fid + "/dismiss", `{"reason": "false_positive", "comment": ""}`},
		{"POST", "/findings/" + fid + "/restore", `{}`},
		{"PUT", "/settings", `{"maxSteps": 200}`},
		{"POST", "/scans", `{}`},
		{"POST", "/scans/" + sc.ID + "/cancel", `{}`},
		{"PUT", "/chain", `{"entries": []}`},
		{"POST", "/chain/reset", `{}`},
	}
	t.Run("a viewer writes nothing", func(t *testing.T) {
		for _, w := range writes {
			wantCode(t, e.send(viewer, w.method, url(w.rest), w.body), w.method+" "+w.rest, http.StatusForbidden)
		}
		if got := e.securityAudits(); len(got) != 0 {
			t.Errorf("audit = %q; want nothing", got)
		}
		if active, err := e.st.ActiveSecurityScan(ctx, "main", "acme/gadgets"); err != nil || active != nil {
			t.Errorf("active scan = %+v, %v; want none", active, err)
		}
		if s := e.findingState("acme/gadgets", fid); s != store.FindingOpen {
			t.Errorf("finding = %s; want open", s)
		}
	})
	t.Run("a maintainer writes", func(t *testing.T) {
		wantCode(t, e.send(maintainer, "POST", url("/findings/"+fid+"/dismiss"), writes[0].body), "dismiss", http.StatusOK)
		wantCode(t, e.send(maintainer, "POST", url("/findings/"+fid+"/restore"), `{}`), "restore", http.StatusOK)
		wantCode(t, e.send(maintainer, "PUT", url("/settings"), `{"maxSteps": 200}`), "PUT settings", http.StatusOK)
		started := decode[struct {
			Scan SecurityScanView `json:"scan"`
		}](t, e.send(maintainer, "POST", url("/scans"), `{}`), "start", http.StatusAccepted)
		wantCode(t, e.send(maintainer, "POST", url("/scans/"+started.Scan.ID+"/cancel"), `{}`), "cancel", http.StatusOK)
		// The scan that ended earlier can't be cancelled; it comes back.
		rec := e.send(maintainer, "POST", url("/scans/"+sc.ID+"/cancel"), `{}`)
		ended := decode[securityConflict](t, rec, "cancel an ended scan", http.StatusConflict)
		if ended.Error != security.ErrEnded.Error() || ended.Scan == nil || ended.Scan.ID != sc.ID {
			t.Errorf("cancel an ended scan = %+v", ended)
		}
	})
	t.Run("another repository's finding and scan", func(t *testing.T) {
		for _, rest := range []string{"/findings/" + oid, "/scans/" + other.ID, "/scans/" + other.ID + "/sarif"} {
			wantCode(t, e.get(viewer, url(rest)), "GET "+rest, http.StatusNotFound)
		}
		for _, w := range []struct{ rest, body string }{
			{"/findings/" + oid + "/dismiss", writes[0].body}, {"/findings/" + oid + "/restore", `{}`},
			{"/scans/" + other.ID + "/cancel", `{}`},
		} {
			wantCode(t, e.send(maintainer, "POST", url(w.rest), w.body), "POST "+w.rest, http.StatusNotFound)
		}
		if s := e.findingState("acme/tools", oid); s != store.FindingOpen {
			t.Errorf("acme/tools' finding = %s; want open", s)
		}
	})
	t.Run("no engine", func(t *testing.T) {
		e.d.securityScans = nil
		wantCode(t, e.get(viewer, url("")), "GET with no engine", http.StatusNotFound)
		wantCode(t, e.send(maintainer, "POST", url("/scans"), `{}`), "POST with no engine", http.StatusNotFound)
	})
}

// Start scan: 202 and one audit entry naming the scan and the model its chain
// gives; each refusal is 409 with its text and no entry; busy comes with the
// active scan; an admin's request says so. The tab says why Start is off.
func TestSecurityStartAnswers(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	f := e.withSecurity()
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.dashboardRepo("main", "acme/tools", nil)
	e.pausedRepo("main", "acme/paused", nil)
	c := e.signInOn(store.RoleMaintainer)
	admin := e.sessionFor(e.ssoAdmin("root@example.com"))
	m := e.addModel("openai", "gpt-scan", "", llmKeyOne)
	chainView := decode[ChainView](t, e.sendJSON(admin, "PUT", securityURL("acme/gadgets", "/chain"),
		entryBody("global/"+m.ID)), "PUT scan chain", http.StatusOK)
	if len(chainView.Entries) != 1 || chainView.Entries[0].Name == "" {
		t.Fatalf("scan chain = %+v", chainView)
	}
	model := chainView.Entries[0].Name
	start := func(c *http.Cookie, project string) *httptest.ResponseRecorder {
		return e.send(c, "POST", securityURL(project, "/scans"), `{}`)
	}

	for name, tc := range map[string]struct {
		fail error
		want string
	}{
		"off":        {security.ErrOff, security.ErrOff.Error()},
		"not on":     {security.ErrNotOn, security.ErrNotOn.Error()},
		"quota":      {security.ErrQuota, security.ErrQuota.Error()},
		"team limit": {&security.TeamLimitError{Team: "Payments"}, "every model of the chain is at the team's own limit: a maintainer of Payments can raise it on the team page"},
		"refused":    {&security.RefusedError{Reason: "the platform did not give the default branch"}, "the platform did not give the default branch"},
	} {
		f.set(tc.fail, "")
		rec := start(c, "acme/gadgets")
		if rec.Code != http.StatusConflict || errorOf(t, rec) != tc.want {
			t.Errorf("%s = %d %s; want 409 %q", name, rec.Code, rec.Body, tc.want)
		}
	}
	f.set(nil, "")
	n := len(f.started())
	rec := start(c, "acme/paused")
	if rec.Code != http.StatusConflict || errorOf(t, rec) != "the repository is paused" || len(f.started()) != n {
		t.Errorf("paused = %d %s, %d starts; want 409 and no start", rec.Code, rec.Body, len(f.started())-n)
	}
	if got := e.securityAudits(); len(got) != 1 {
		t.Errorf("audit after the refusals = %q; want only the chain save", got)
	}

	tab := getJSON[SecurityView](e, c, securityURL("acme/gadgets", ""))
	if !tab.CanStart || tab.StartOff != "" || tab.Start.Model != model || !tab.Start.StepsFromSkill ||
		tab.Start.MaxSteps != 600 || tab.Start.SubagentMaxSteps != 100 || tab.Start.TimeoutSeconds != 10800 {
		t.Errorf("tab before a scan = %+v", tab)
	}
	got := decode[struct {
		Scan SecurityScanView `json:"scan"`
	}](t, start(c, "acme/gadgets"), "start", http.StatusAccepted)
	if got.Scan.State != store.SecurityQueued || got.Scan.Number != 1 || got.Scan.StartedBy != aliceEmail {
		t.Errorf("started scan = %+v", got.Scan)
	}
	f.set(security.ErrBusy, "")
	busy := decode[securityConflict](t, start(c, "acme/gadgets"), "busy", http.StatusConflict)
	if busy.Error != security.ErrBusy.Error() || busy.Scan == nil || busy.Scan.ID != got.Scan.ID {
		t.Errorf("busy = %+v; want the active scan", busy)
	}
	if audits := e.securityAudits(); !slices.Equal(audits[1:], []string{"security_scan_started acme/gadgets scan=1 model=" + model}) {
		t.Errorf("audit = %q", audits)
	}

	t.Run("the tab says why Start is off", func(t *testing.T) {
		tab := getJSON[SecurityView](e, c, securityURL("acme/gadgets", ""))
		if tab.CanStart || tab.Running == nil || tab.Running.ID != got.Scan.ID || !tab.CanManage {
			t.Errorf("tab with a scan queued = %+v", tab)
		}
		if p := getJSON[SecurityView](e, c, securityURL("acme/paused", "")); p.CanStart || p.StartOff != "the repository is paused" {
			t.Errorf("paused tab = %+v", p)
		}
		f.set(nil, security.ErrOff.Error())
		if o := getJSON[SecurityView](e, c, securityURL("acme/tools", "")); o.CanStart || o.StartOff != security.ErrOff.Error() {
			t.Errorf("tab with scans off = %+v", o)
		}
		f.set(nil, "")
	})
	t.Run("an admin's start says so", func(t *testing.T) {
		decode[struct{}](t, start(admin, "acme/tools"), "admin start", http.StatusAccepted)
		starts := f.started()
		last := starts[len(starts)-1]
		if !last.Admin || last.Project != "acme/tools" || last.Actor != "root@example.com" {
			t.Errorf("admin's start = %+v", last)
		}
		if starts[0].Admin {
			t.Errorf("the maintainer's start = %+v; want not an admin", starts[0])
		}
	})
}

// Dismiss needs a known reason and a comment of at most 1000 characters;
// each change is audited once; doing it again changes nothing; a fixed
// finding is 409.
func TestSecurityDismissRestore(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.withSecurity()
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.securityScanDone("acme/gadgets", securityResult("fp-one-0123456789", "fp-two"))
	// The second complete scan, of the same skill, fixes fp-two.
	e.securityScanDone("acme/gadgets", securityResult("fp-one-0123456789"))
	id, fixed := e.findingID("acme/gadgets", "fp-one-0123456789"), e.findingID("acme/gadgets", "fp-two")
	c := e.signInOn(store.RoleMaintainer)
	dismiss := func(id, body string) *httptest.ResponseRecorder {
		return e.send(c, "POST", securityURL("acme/gadgets", "/findings/"+id+"/dismiss"), body)
	}
	restore := func(id string) *httptest.ResponseRecorder {
		return e.send(c, "POST", securityURL("acme/gadgets", "/findings/"+id+"/restore"), `{}`)
	}

	for name, tc := range map[string]struct{ body, want string }{
		"unknown reason": {`{"reason": "boring", "comment": ""}`, msgBadSecurityReason},
		"no reason":      {`{"comment": "x"}`, msgBadSecurityReason},
		"long comment":   {`{"reason": "mitigated", "comment": "` + strings.Repeat("é", 1001) + `"}`, msgSecurityCommentLong},
		"not JSON":       {`reason=mitigated`, msgBadSecurityDismiss},
	} {
		rec := dismiss(id, tc.body)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != tc.want {
			t.Errorf("%s = %d %s; want 400 %q", name, rec.Code, rec.Body, tc.want)
		}
	}
	v := decode[SecurityFindingView](t, dismiss(id, `{"reason": "used_in_tests", "comment": "`+strings.Repeat("é", 1000)+`"}`),
		"dismiss", http.StatusOK)
	if v.Finding.State != store.FindingDismissed || v.Dismissal == nil || v.Dismissal.Reason != "used_in_tests" ||
		v.Dismissal.By != aliceEmail || len(v.History) != 2 || v.History[1].Kind != store.FindingEventDismissed ||
		v.History[0].Scan != 1 || !v.CanManage {
		t.Errorf("dismissed = %+v", v)
	}
	decode[SecurityFindingView](t, dismiss(id, `{"reason": "mitigated", "comment": ""}`), "dismiss again", http.StatusOK)
	v = decode[SecurityFindingView](t, restore(id), "restore", http.StatusOK)
	if v.Finding.State != store.FindingOpen || v.Dismissal != nil || len(v.History) != 3 {
		t.Errorf("restored = %+v", v)
	}
	decode[SecurityFindingView](t, restore(id), "restore again", http.StatusOK)
	for what, rec := range map[string]*httptest.ResponseRecorder{
		"dismiss a fixed one": dismiss(fixed, `{"reason": "mitigated", "comment": ""}`), "restore a fixed one": restore(fixed),
	} {
		if rec.Code != http.StatusConflict {
			t.Errorf("%s = %d %s; want 409", what, rec.Code, rec.Body)
		}
	}
	if got := e.securityAudits(); !slices.Equal(got, []string{
		"security_finding_dismissed acme/gadgets fingerprint=fp-one-01234 reason=used_in_tests",
		"security_finding_restored acme/gadgets fingerprint=fp-one-01234",
	}) {
		t.Errorf("audit = %q", got)
	}
}

// Each setting out of its range is 400 and saves nothing; null is the
// default; each change is one settings_updated entry, and no change is none.
func TestSecuritySettingsRanges(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.withSecurity()
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.signInOn(store.RoleMaintainer)
	url := securityURL("acme/gadgets", "/settings")

	for _, body := range []string{
		`{"maxSteps": 4}`, `{"maxSteps": 1001}`, `{"subagentMaxSteps": 4}`, `{"subagentMaxSteps": 301}`,
		`{"timeoutSeconds": 599}`, `{"timeoutSeconds": 21601}`, `{"timeoutSeconds": 9223372036854775807}`,
		`{"maxSteps": "ten"}`, `{"tokenBudget": 5}`, `[]`,
	} {
		if rec := e.send(c, "PUT", url, body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s; want 400", body, rec.Code, rec.Body)
		}
	}
	v := getJSON[SecuritySettingsView](e, c, url)
	if v.MaxSteps != nil || v.SubagentMaxSteps != nil || v.TimeoutSeconds != nil || v.FindingsInReviews || !v.CanEdit ||
		v.Defaults != (SecurityDefaultsView{MaxSteps: 600, SubagentMaxSteps: 100, TimeoutSeconds: 10800}) {
		t.Errorf("settings after the refusals = %+v", v)
	}
	v = decode[SecuritySettingsView](t, e.send(c, "PUT", url,
		`{"maxSteps": 5, "subagentMaxSteps": 300, "timeoutSeconds": 600, "findingsInReviews": true}`), "PUT", http.StatusOK)
	if v.MaxSteps == nil || *v.MaxSteps != 5 || *v.SubagentMaxSteps != 300 || *v.TimeoutSeconds != 600 || !v.FindingsInReviews {
		t.Errorf("saved = %+v", v)
	}
	decode[SecuritySettingsView](t, e.send(c, "PUT", url,
		`{"maxSteps": 5, "subagentMaxSteps": 300, "timeoutSeconds": 600, "findingsInReviews": true}`), "PUT the same", http.StatusOK)
	v = decode[SecuritySettingsView](t, e.send(c, "PUT", url,
		`{"maxSteps": null, "subagentMaxSteps": 300, "timeoutSeconds": 21600, "findingsInReviews": true}`), "PUT null", http.StatusOK)
	if v.MaxSteps != nil || *v.TimeoutSeconds != 21600 {
		t.Errorf("null = %+v; want the default", v)
	}
	if got := e.securityAudits(); !slices.Equal(got, []string{
		"settings_updated acme/gadgets security.maxSteps=5, security.subagentMaxSteps=300, security.timeoutSeconds=600, security.findingsInReviews=true",
		"settings_updated acme/gadgets security.maxSteps=default, security.timeoutSeconds=21600",
	}) {
		t.Errorf("audit = %q", got)
	}
}

// The scan chain follows the repository chain's write rule: a maintainer of
// the owner team saves it; a maintainer of another team that covers the
// repository is 403 with the repository chain's text; a reset inherits the
// model chain again. Each save and reset is audited.
func TestSecurityChainRule(t *testing.T) {
	t.Parallel()
	te := newTeamsEnv(t)
	te.withSecurity()
	shared, _ := te.sharedAndPrivate()
	own := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	url := securityURL("acme/payments", "/chain")

	v := getJSON[ChainView](te.env, te.viewer, url)
	if v.Scope != store.ScopeRepo || v.Source != sourceRepositoryChain || !v.ReadOnly {
		t.Errorf("inherited scan chain = %+v", v)
	}
	rec := te.sendJSON(te.otherMaintainer, "PUT", url, entryBody("global/"+shared.ID))
	if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgRepoChainWrite {
		t.Errorf("another team's maintainer = %d %s; want 403 %q", rec.Code, rec.Body, msgRepoChainWrite)
	}
	rec = te.sendJSON(te.maintainer, "PUT", url, entryBody())
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgEmptyScanChain {
		t.Errorf("empty = %d %s", rec.Code, rec.Body)
	}
	v = decode[ChainView](t, te.sendJSON(te.maintainer, "PUT", url, entryBody("team/"+own.ID, "global/"+shared.ID)), "PUT", http.StatusOK)
	if v.Source != "repository" || v.ReadOnly || v.SavedByAdmin || !slices.Equal(chainRefs(v), []string{"team/" + own.ID, "global/" + shared.ID}) {
		t.Errorf("saved = %+v", v)
	}
	// The scan chain is its own: the repository's model chain is untouched.
	if c, found, err := te.st.Chain(ctx, store.ScopeRepo, 0, "main", "acme/payments"); err != nil || found {
		t.Errorf("model chain = %+v, %v, %v; want none", c, found, err)
	}
	wantCode(t, te.sendJSON(te.otherMaintainer, "POST", url+"/reset", map[string]any{}), "another team's reset", http.StatusForbidden)
	v = decode[ChainView](t, te.sendJSON(te.maintainer, "POST", url+"/reset", map[string]any{}), "reset", http.StatusOK)
	if v.Source != sourceRepositoryChain {
		t.Errorf("after reset = %+v", v)
	}
	if got := auditDetails(te.env, "chain_changed"); !slices.Equal(got, []string{"security scan: own, openai/gpt-shared", "security scan: inherit"}) {
		t.Errorf("chain_changed = %q", got)
	}
}

// The SARIF is a file to save, named by the repository and the scan, and
// 404 once retention dropped it, when the scan says it has none.
func TestSecuritySARIFDownload(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.withSecurity()
	e.dashboardRepo("main", "acme/gadgets", nil)
	sc := e.securityScanDone("acme/gadgets", securityResult("fp1"))
	c := e.signInOn(store.RoleViewer)
	url := securityURL("acme/gadgets", "/scans/"+sc.ID+"/sarif")

	rec := e.get(c, url)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/sarif+json" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="gadgets-scan-1.sarif"` {
		t.Errorf("SARIF = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Content-Disposition"))
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc["version"] != "2.1.0" {
		t.Errorf("SARIF body %s: %v", rec.Body, err)
	}
	if d := getJSON[SecurityScanDetailView](e, c, securityURL("acme/gadgets", "/scans/"+sc.ID)); !d.Scan.HasSARIF ||
		len(d.New) != 1 || len(d.Fixed) != 0 {
		t.Errorf("scan = %+v", d)
	}
	if _, err := e.db.Exec(ctx, `UPDATE security_scans SET sarif = NULL WHERE id = $1::uuid`, sc.ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.get(c, url), "SARIF once dropped", http.StatusNotFound)
	if d := getJSON[SecurityScanDetailView](e, c, securityURL("acme/gadgets", "/scans/"+sc.ID)); d.Scan.HasSARIF {
		t.Errorf("scan once dropped = %+v; want no SARIF", d.Scan)
	}
}

// Review Focus 1: a repository whose path ends in /security keeps its detail
// and its settings, and has a Security tab of its own.
func TestSecurityPathPrefersRepository(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.withSecurity()
	e.dashboardRepo("main", "acme/security", nil)
	c := e.signInOn(store.RoleMaintainer)

	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/security"); v.Repo.Project != "acme/security" {
		t.Errorf("detail = %+v", v)
	}
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/security/settings", `{"approve": false}`), "PUT settings", http.StatusOK)
	if r, found := e.storedRepo("main", "acme/security"); !found || r.Approve == nil || *r.Approve {
		t.Errorf("stored = %+v, %v; want approve off", r, found)
	}
	if v := getJSON[SecurityView](e, c, "/api/repositories/main/acme/security/security"); !v.CanStart {
		t.Errorf("its own tab = %+v", v)
	}
}
