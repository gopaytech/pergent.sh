package dashboard

import (
	"net/http"
	"strings"
	"testing"

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
