package dashboard

import (
	"net/http"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// withoutConfigWidgets drops main's acme/widgets from the config, so the
// review buttons' repository can be a dashboard one, and paused.
func withoutConfigWidgets(cfg *config.Config) {
	var keep []config.Repository
	for _, r := range cfg.Repositories {
		if !(r.Instance() == "main" && r.Project == buttonProject) {
			keep = append(keep, r)
		}
	}
	cfg.Repositories = keep
}

// Scan now on a paused repository is 409 "the repository is paused", and
// nothing is asked of the engine. The tab offers no Scan now.
func TestScanNowPausedRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	f := e.withReadiness()
	e.pausedRepo("main", "acme/gadgets", nil)
	c := e.signInOn(store.RoleMaintainer, "acme/gadgets")
	rec := e.send(c, "POST", scanURL("acme/gadgets"), "")
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgPaused {
		t.Errorf("POST scan = %d %s; want 409 %q", rec.Code, rec.Body, msgPaused)
	}
	f.mu.Lock()
	asked := len(f.asked)
	f.mu.Unlock()
	if asked != 0 {
		t.Errorf("the engine was asked for %d scans", asked)
	}
	if got := e.auditOf("readiness_scan_requested"); len(got) != 0 {
		t.Errorf("audit = %+v; want none", got)
	}
	if v := getJSON[ReadinessView](e, c, "/api/repositories/main/acme/gadgets/readiness"); v.CanScan {
		t.Error("the Readiness tab offers Scan now on a paused repository")
	}
}

// The review buttons on a paused repository are 409 "the repository is
// paused": no run is queued, the platform is not asked, the engine is not
// woken. The review page offers none of them.
func TestReviewButtonsPausedRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutConfigWidgets)
	e.pausedRepo("main", buttonProject, nil)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)
	runs := len(e.runRows())
	for action, body := range map[string]string{
		"rerun":   `{"failed":false}`,
		"start":   `{"head":"` + head('1') + `"}`,
		"dismiss": `{"fingerprint":"` + blockingFP + `","reason":"fine"}`,
		"restore": `{"fingerprint":"` + blockingFP + `"}`,
	} {
		rec := e.send(c, "POST", buttonURL(action), body)
		if rec.Code != http.StatusConflict || errorOf(t, rec) != msgPaused {
			t.Errorf("%s = %d %s; want 409 %q", action, rec.Code, rec.Body, msgPaused)
		}
	}
	if n := len(e.runRows()); n != runs {
		t.Errorf("%d runs; want %d", n, runs)
	}
	var calls int
	e.src["main"].set(func(f *fakeSource) { calls = f.changeCalls + f.calls })
	if calls != 0 {
		t.Errorf("the platform was asked %d times", calls)
	}
	if n := e.wakes.Load(); n != 0 {
		t.Errorf("the engine was woken %d times", n)
	}
	if ds := e.dismissalsOf(); len(ds) != 0 {
		t.Errorf("dismissals = %+v; want none", ds)
	}
	view := getJSON[ReviewDetailView](e, c, "/api/reviews/main/"+buttonProject+"/7")
	if view.Can != (CanView{}) {
		t.Errorf("can = %+v on a paused repository; want nothing", view.Can)
	}
}
