package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// quotaEnv is the env with acme/widgets on main limited to 10 tokens a month,
// and a review of another change of it that used them all.
func quotaEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, func(cfg *config.Config) { cfg.Repositories[0].MonthlyTokenQuota = ptr(int64(10)) })
	e.usageRun(runFix{project: buttonProject, number: 99, head: head('9')}, usageSkill("code-review", "no_blocking_issues", 6, 4))
	return e
}

// A maintainer's re-run on a used-up quota is refused with the reason; nothing
// is queued and the engine isn't woken.
func TestMaintainerRerunRefusedOnUsedUpQuota(t *testing.T) {
	e := quotaEnv(t)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)

	rec := e.send(c, "POST", buttonURL("rerun"), `{"failed":false}`)
	wantCode(t, rec, "rerun", http.StatusConflict)
	if got := errorOf(t, rec); !strings.Contains(got, "the monthly token quota is used up") {
		t.Errorf("error = %q; want the quota reason", got)
	}
	if rows := e.runRows(); len(rows) != 1 || rows[0].State != "published" {
		t.Errorf("runs = %+v; want only the published review", rows)
	}
	if n := e.wakes.Load(); n != 0 {
		t.Errorf("Wake called %d times; want 0", n)
	}
	if a := e.auditOf("review_rerun"); len(a) != 0 {
		t.Errorf("audit = %+v; want none", a)
	}
}

// An admin's re-run goes past the quota. Its tokens count like any other.
func TestAdminRerunPastQuota(t *testing.T) {
	e := quotaEnv(t)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleAdmin)

	rec := e.send(c, "POST", buttonURL("rerun"), `{"failed":false}`)
	wantCode(t, rec, "rerun", http.StatusAccepted)
	if got := statusOf(t, rec); got != "queued" {
		t.Errorf("status = %q; want queued", got)
	}
	rows := e.runRows()
	if len(rows) != 2 || rows[1].State != "queued" || rows[1].Trigger != store.TriggerRerun {
		t.Fatalf("runs = %+v; want the published run and a queued re-run", rows)
	}
	if a := e.auditOf("review_rerun"); len(a) != 1 {
		t.Errorf("audit = %+v; want one review_rerun", a)
	}
}

// A maintainer's start of a change a quota holds is refused; the change stays
// waiting.
func TestMaintainerStartRefusedOnUsedUpQuota(t *testing.T) {
	e := quotaEnv(t)
	id := e.waitingChange(head('1'), store.WaitQuota)
	c := e.signInRole(store.RoleMaintainer)

	rec := e.send(c, "POST", buttonURL("start"), `{"head":"`+head('1')+`"}`)
	wantCode(t, rec, "start", http.StatusConflict)
	if got := errorOf(t, rec); !strings.Contains(got, "the monthly token quota is used up") {
		t.Errorf("error = %q; want the quota reason", got)
	}
	rows := e.runRows()
	if len(rows) != 1 || rows[0].ID != id || rows[0].State != "waiting" {
		t.Errorf("runs = %+v; want the run still waiting", rows)
	}
	if a := e.auditOf("review_start"); len(a) != 0 {
		t.Errorf("audit = %+v; want none", a)
	}
}

// An admin's start of a quota-held change goes ahead.
func TestAdminStartPastQuota(t *testing.T) {
	e := quotaEnv(t)
	id := e.waitingChange(head('1'), store.WaitQuota)
	c := e.signInRole(store.RoleAdmin)

	rec := e.send(c, "POST", buttonURL("start"), `{"head":"`+head('1')+`"}`)
	if rec.Code != http.StatusAccepted || statusOf(t, rec) != "queued" {
		t.Fatalf("start = %d %s; want 202 queued", rec.Code, rec.Body)
	}
	rows := e.runRows()
	if len(rows) != 1 || rows[0].ID != id || rows[0].State != "queued" || rows[0].Trigger != store.TriggerStart {
		t.Errorf("runs = %+v; want the waiting run queued as a start", rows)
	}
	if a := e.auditOf("review_start"); len(a) != 1 {
		t.Errorf("audit = %+v; want one review_start", a)
	}
}

// A viewer starts nothing, quota or not.
func TestViewerCannotStart(t *testing.T) {
	e := quotaEnv(t)
	e.waitingChange(head('1'), store.WaitQuota)
	c := e.signInRole(store.RoleViewer)
	wantCode(t, e.send(c, "POST", buttonURL("start"), `{"head":"`+head('1')+`"}`), "viewer", http.StatusForbidden)
}

// Room under the quota: a maintainer's re-run queues, marked as checked.
func TestMaintainerRerunWithRoomQueuesChecked(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) { cfg.Repositories[0].MonthlyTokenQuota = ptr(int64(11)) })
	e.usageRun(runFix{project: buttonProject, number: 99, head: head('9')}, usageSkill("code-review", "no_blocking_issues", 6, 4))
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)

	wantCode(t, e.send(c, "POST", buttonURL("rerun"), `{}`), "rerun", http.StatusAccepted)
	var checked bool
	if err := e.db.QueryRow(ctx, `SELECT quota_checked FROM runs WHERE instance = 'main' AND mr_iid = 7 AND trigger = 'rerun'`).Scan(&checked); err != nil || !checked {
		t.Errorf("quota_checked = %v, %v; want true", checked, err)
	}
}

// A repository's quota is a whole number from 1 to 10^15, or null. Anything
// else, a fraction, a string, a number too big for a float to hold exactly,
// is 400 and changes nothing.
func TestQuotaSettingBounds(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.sessionFor(e.ssoAdmin("boss@example.com"))

	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"monthlyTokenQuota":5000000}`), "set", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.MonthlyTokenQuota == nil || *r.MonthlyTokenQuota != 5000000 {
		t.Fatalf("stored = %v; want 5000000", r.MonthlyTokenQuota)
	}
	for _, body := range []string{
		`{"monthlyTokenQuota":0}`, `{"monthlyTokenQuota":-5}`, `{"monthlyTokenQuota":1.5}`, `{"monthlyTokenQuota":"5M"}`,
		`{"monthlyTokenQuota":"5"}`, `{"monthlyTokenQuota":1e3}`, `{"monthlyTokenQuota":true}`, `{"monthlyTokenQuota":[]}`,
		`{"monthlyTokenQuota":9007199254740993}`, `{"monthlyTokenQuota":1000000000000001}`,
		`{"monthlyTokenQuota":99999999999999999999999}`,
	} {
		rec := e.send(c, "PUT", gadgetsSettings, body)
		wantCode(t, rec, body, http.StatusBadRequest)
		if got := errorOf(t, rec); got != msgBadQuota {
			t.Errorf("%s: error = %q; want %q", body, got, msgBadQuota)
		}
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.MonthlyTokenQuota == nil || *r.MonthlyTokenQuota != 5000000 {
		t.Errorf("after the refused values: stored = %v; want 5000000 kept", r.MonthlyTokenQuota)
	}
	for _, body := range []string{`{"monthlyTokenQuota":1}`, `{"monthlyTokenQuota":1000000000000000}`} {
		wantCode(t, e.send(c, "PUT", gadgetsSettings, body), body, http.StatusOK)
	}

	v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets")
	if s := v.Settings["monthlyTokenQuota"]; s.Value != float64(config.MaxMonthlyTokenQuota) || !s.Overridden {
		t.Errorf("setting = %+v; want the largest quota, overridden", s)
	}

	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"monthlyTokenQuota":null}`), "clear", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.MonthlyTokenQuota != nil {
		t.Errorf("cleared quota = %d; want none", *r.MonthlyTokenQuota)
	}
	v = getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets")
	if s := v.Settings["monthlyTokenQuota"]; s.Value != nil || s.Overridden {
		t.Errorf("setting = %+v; want null, not overridden", s)
	}
	if a := e.auditOf("settings_updated"); len(a) == 0 || a[len(a)-1].Detail != "monthlyTokenQuota" {
		t.Errorf("audit = %+v; want the key named", a)
	}
}

// A quota is an admin's to say and to see: a maintainer can't change it, the
// settings and the page leave it out for them.
func TestQuotaSettingIsForAdmins(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) { r.MonthlyTokenQuota = ptr(int64(42)) })
	c := e.signInRole(store.RoleMaintainer)

	for _, body := range []string{`{"monthlyTokenQuota":5}`, `{"monthlyTokenQuota":null}`, `{"approve":true,"monthlyTokenQuota":5}`} {
		rec := e.send(c, "PUT", gadgetsSettings, body)
		wantCode(t, rec, body, http.StatusForbidden)
		if got := errorOf(t, rec); got != msgNeedsAdminQuota {
			t.Errorf("%s: error = %q; want %q", body, got, msgNeedsAdminQuota)
		}
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.MonthlyTokenQuota == nil || *r.MonthlyTokenQuota != 42 || r.Approve != nil {
		t.Errorf("stored = %+v; want nothing changed", r)
	}

	rec := e.get(c, "/api/repositories/main/acme/gadgets")
	wantCode(t, rec, "maintainer GET", http.StatusOK)
	var v RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if _, ok := v.Settings["monthlyTokenQuota"]; ok {
		t.Errorf("a maintainer's settings hold monthlyTokenQuota: %+v", v.Settings)
	}
	if v.Quota != nil || !strings.Contains(rec.Body.String(), `"quota":null`) {
		t.Errorf("a maintainer's quota = %+v in %s; want null", v.Quota, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "42") {
		t.Errorf("a maintainer's page holds the quota: %s", rec.Body)
	}
}

// A maintainer's reset leaves the quota; an admin's clears it.
func TestQuotaResetByAdminOnly(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) { r.MonthlyTokenQuota = ptr(int64(42)) })
	maintainer := e.signInRole(store.RoleMaintainer)
	wantCode(t, e.send(maintainer, "POST", gadgetsSettings+"/reset", ""), "maintainer reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.MonthlyTokenQuota == nil || *r.MonthlyTokenQuota != 42 {
		t.Fatalf("after a maintainer's reset: %v; want 42 kept", r.MonthlyTokenQuota)
	}

	admin := e.sessionFor(e.ssoAdmin("boss@example.com"))
	wantCode(t, e.send(admin, "POST", gadgetsSettings+"/reset", ""), "admin reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.MonthlyTokenQuota != nil {
		t.Errorf("after an admin's reset: %d; want none", *r.MonthlyTokenQuota)
	}
}

// An admin's page shows this month's tokens against both quotas. A quota that
// isn't set is a null limit.
func TestDetailShowsQuotaToAdmins(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) { cfg.Repositories[0].MonthlyTokenQuota = ptr(int64(100)) })
	e.usageRun(runFix{project: "acme/widgets", number: 1, head: head('1')}, usageSkill("code-review", "no_blocking_issues", 50, 30))
	e.usageRun(runFix{project: "acme/other", number: 2, head: head('2')}, usageSkill("code-review", "no_blocking_issues", 25, 15))
	// Last month's tokens don't count.
	e.usageRun(runFix{project: "acme/widgets", number: 3, head: head('3'), at: e.clock.now().Add(-40 * 24 * time.Hour)},
		usageSkill("code-review", "no_blocking_issues", 5000, 5000))
	c := e.sessionFor(e.ssoAdmin("boss@example.com"))

	v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/widgets")
	if v.Quota == nil {
		t.Fatal("quota = null; want the usage")
	}
	want := QuotaView{Repository: QuotaUseView{Used: 80, Limit: ptr(int64(100))}, Instance: QuotaUseView{Used: 120}}
	got, _ := json.Marshal(v.Quota)
	wantJSON, _ := json.Marshal(want)
	if string(got) != string(wantJSON) {
		t.Errorf("quota = %s; want %s", got, wantJSON)
	}
	if !strings.Contains(string(got), `"instance":{"used":120,"limit":null}`) {
		t.Errorf("quota = %s; want the instance's limit null", got)
	}

}

// A page whose usage can't be read still opens, with a null quota.
func TestDetailQuotaReadFailureIsNull(t *testing.T) {
	e := newEnv(t, nil)
	c := e.sessionFor(e.ssoAdmin("boss@example.com"))
	e.exec(`ALTER TABLE run_usage RENAME TO run_usage_gone`)
	rec := e.get(c, "/api/repositories/main/acme/widgets")
	wantCode(t, rec, "GET with the usage unreadable", http.StatusOK)
	var v RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Quota != nil || !strings.Contains(rec.Body.String(), `"quota":null`) {
		t.Errorf("quota = %+v in %s; want null", v.Quota, rec.Body)
	}
}

// labInstance adds the dashboard instance "lab" and returns an admin's
// session.
func (e *env) labInstance() *http.Cookie {
	e.t.Helper()
	if err := e.st.CreateInstance(ctx, store.InstanceRow{Name: "lab", Platform: "gitlab", URL: "https://lab.example.com"}, nil, e.clock.now()); err != nil {
		e.t.Fatal(err)
	}
	if err := e.reg.Load(ctx); err != nil {
		e.t.Fatal(err)
	}
	return e.sessionFor(e.ssoAdmin("boss@example.com"))
}

func (e *env) instanceView(c *http.Cookie, name string) InstanceAdminView {
	e.t.Helper()
	for _, v := range getJSON[[]InstanceAdminView](e, c, "/api/admin/instances") {
		if v.Name == name {
			return v
		}
	}
	e.t.Fatalf("no instance %q in the list", name)
	return InstanceAdminView{}
}

// An admin sets a dashboard instance's quota, and the list shows it with this
// month's tokens. A config instance's is in the config file.
func TestInstanceTokenQuota(t *testing.T) {
	e := newEnv(t, nil)
	boss := e.labInstance()
	e.usageRun(runFix{instance: "lab", project: "acme/lab", number: 1, head: head('1')}, usageSkill("code-review", "no_blocking_issues", 6, 4))
	const url = "/api/admin/instances/lab/token-quota"

	if v := e.instanceView(boss, "lab"); v.MonthlyTokenQuota != nil || v.TokensThisMonth != 10 {
		t.Errorf("before: lab = %v, %d tokens; want no quota, 10 tokens", v.MonthlyTokenQuota, v.TokensThisMonth)
	}
	wantCode(t, e.send(boss, "PUT", url, `{"monthlyTokenQuota":1000}`), "set", http.StatusNoContent)
	v := e.instanceView(boss, "lab")
	if v.MonthlyTokenQuota == nil || *v.MonthlyTokenQuota != 1000 || v.TokensThisMonth != 10 {
		t.Errorf("lab = %v, %d tokens; want quota 1000, 10 tokens", v.MonthlyTokenQuota, v.TokensThisMonth)
	}
	if inst, _ := e.reg.Get("lab"); inst.MonthlyTokenQuota == nil || *inst.MonthlyTokenQuota != 1000 {
		t.Errorf("registry = %v; want 1000", inst.MonthlyTokenQuota)
	}
	if a := e.auditOf("instance_updated"); len(a) != 1 || a[0].Detail != "monthly_token_quota=1000" || a[0].Instance != "lab" {
		t.Errorf("audit = %+v; want monthly_token_quota=1000 on lab", a)
	}

	wantCode(t, e.send(boss, "PUT", url, `{"monthlyTokenQuota":null}`), "clear", http.StatusNoContent)
	if v := e.instanceView(boss, "lab"); v.MonthlyTokenQuota != nil {
		t.Errorf("cleared: lab = %d; want none", *v.MonthlyTokenQuota)
	}
	if a := e.auditOf("instance_updated"); len(a) != 2 || a[0].Detail != "monthly_token_quota=none" || a[1].Detail != "monthly_token_quota=1000" {
		t.Errorf("audit = %+v; want none, then 1000 (newest first)", a)
	}

	for _, body := range []string{`{}`, `{"monthlyTokenQuota":0}`, `{"monthlyTokenQuota":1.5}`, `{"monthlyTokenQuota":"5M"}`,
		`{"monthlyTokenQuota":9007199254740993}`, `{"monthlyTokenQuota":5,"x":1}`, `[]`, ``} {
		rec := e.send(boss, "PUT", url, body)
		wantCode(t, rec, body, http.StatusBadRequest)
		if got := errorOf(t, rec); got != msgBadQuota {
			t.Errorf("%q: error = %q; want %q", body, got, msgBadQuota)
		}
	}
	wantCode(t, e.send(boss, "PUT", "/api/admin/instances/main/token-quota", `{"monthlyTokenQuota":5}`), "config instance", http.StatusConflict)
	wantCode(t, e.send(boss, "PUT", "/api/admin/instances/nope/token-quota", `{"monthlyTokenQuota":5}`), "unknown", http.StatusNotFound)
	wantCode(t, e.send(e.aliceSession(), "PUT", url, `{"monthlyTokenQuota":5}`), "not an admin", http.StatusNotFound)
	if n := len(e.auditOf("instance_updated")); n != 2 {
		t.Errorf("audit entries = %d; want the refused ones to leave none", n)
	}

	if ok, err := e.st.MarkInstanceRemoving(ctx, "lab"); err != nil || !ok {
		t.Fatalf("MarkInstanceRemoving = %v, %v", ok, err)
	}
	if err := e.reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.send(boss, "PUT", url, `{"monthlyTokenQuota":5}`), "removing", http.StatusConflict)
}

// A config instance shows its quota from the config file and its tokens.
func TestConfigInstanceShowsQuota(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) { cfg.GitLab[0].MonthlyTokenQuota = ptr(int64(500)) })
	e.usageRun(runFix{project: "acme/widgets", number: 1, head: head('1')}, usageSkill("code-review", "no_blocking_issues", 6, 4))
	v := e.instanceView(e.sessionFor(e.ssoAdmin("boss@example.com")), "main")
	if v.MonthlyTokenQuota == nil || *v.MonthlyTokenQuota != 500 || v.TokensThisMonth != 10 {
		t.Errorf("main = %v, %d tokens; want quota 500, 10 tokens", v.MonthlyTokenQuota, v.TokensThisMonth)
	}
}

// A waiting run held by the quota says so.
func TestJudgeQuotaWaiting(t *testing.T) {
	got := judge(store.RunDigest{Outcome: OutcomeWaiting, State: store.Waiting, SkipReason: store.WaitQuota})
	if got.outcome != OutcomeWaiting || got.reason != reasonQuotaUsedUp || got.status != "" {
		t.Errorf("judge = %+v; want waiting / %q", got, reasonQuotaUsedUp)
	}
	if reasonQuotaUsedUp != "the monthly token quota is used up; an admin can start the review" {
		t.Errorf("reasonQuotaUsedUp = %q", reasonQuotaUsedUp)
	}
}

func TestIsQuotaKey(t *testing.T) {
	if !isQuotaKey("monthlyTokenQuota") || isQuotaKey("approve") || isQuotaKey("") {
		t.Error("isQuotaKey is true for exactly monthlyTokenQuota")
	}
}
