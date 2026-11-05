package dashboard

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

func pastURL(project string) string { return "/api/repositories/main/" + project + "/past-dismissals" }

func stopURL(project string) string { return pastURL(project) + "/stop" }

// pastDismissal stores a dismissal of fingerprint on change number of
// acme/widgets that teaches, made at.
func (e *env) pastDismissal(number int64, fingerprint, severity, reason, actor string, at time.Time) {
	e.t.Helper()
	e.pastDismissalIn(testID(buttonProject), number, fingerprint, severity, reason, actor, at)
}

// pastDismissalIn is pastDismissal in the project with platform ID projectID.
func (e *env) pastDismissalIn(projectID, number int64, fingerprint, severity, reason, actor string, at time.Time) {
	e.t.Helper()
	mr := store.MRKey{Instance: "main", ProjectID: projectID, IID: number}
	if _, err := e.st.Dismiss(ctx, store.Dismissal{MR: mr, Fingerprint: fingerprint, Severity: severity, Reason: reason,
		Actor: actor, CreatedAt: at, Skill: "code-review", File: "api/handler.go", Title: "Title of " + fingerprint,
		Teaches: true, DismisserPlatformID: aliceID["main"]}); err != nil {
		e.t.Fatal(err)
	}
}

// teaching lists the fingerprints the repository's list shows, in order.
func teaching(t *testing.T, e *env, c *http.Cookie) []PastDismissalView {
	t.Helper()
	return decode[[]PastDismissalView](t, e.get(c, pastURL(buttonProject)), "past dismissals", http.StatusOK)
}

// One row a finding, a blocking one over a suggestion and then the newest,
// nothing older than 90 days, all of them whatever their number, and the
// own-change rule left out: alice sees a blocking dismissal of her own.
func TestPastDismissalsList(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	now := e.clock.now()
	// "dup" is dismissed on three changes: a suggestion, a newer suggestion
	// and an older blocking one, which wins.
	e.pastDismissal(1, "dup", "suggestion", "old reason", "bob (gitlab main)", now.Add(-48*time.Hour))
	e.pastDismissal(2, "dup", "suggestion", "newer reason", "bob (gitlab main)", now.Add(-24*time.Hour))
	e.pastDismissal(3, "dup", "blocking", "the blocking one", "carol (gitlab main)", now.Add(-72*time.Hour))
	e.pastDismissal(4, "own", "blocking", "my own", "alice (gitlab main)", now.Add(-time.Hour))
	e.pastDismissal(5, "stale", "suggestion", "too old", "bob (gitlab main)", now.Add(-91*24*time.Hour))
	e.pastDismissal(6, "noreason", "suggestion", "![x](https://example.com/p.png)", "bob (gitlab main)", now.Add(-time.Hour))
	for i := range 25 {
		e.pastDismissal(int64(10+i), fmt.Sprintf("many%02d", i), "suggestion", "reason", "bob (gitlab main)", now.Add(-time.Duration(i+1)*time.Minute))
	}

	rows := teaching(t, e, e.signInRole(store.RoleMaintainer))
	var fps []string
	for _, r := range rows {
		fps = append(fps, r.Fingerprint)
	}
	if len(rows) != 27 {
		t.Fatalf("%d rows %v; want 27: dup, own and the 25 more than a review is told", len(rows), fps)
	}
	if slices.Contains(fps, "stale") || slices.Contains(fps, "noreason") {
		t.Errorf("rows %v; want nothing older than 90 days and nothing without a reason", fps)
	}
	if n := len(slices.Compact(slices.Sorted(slices.Values(fps)))); n != len(fps) {
		t.Errorf("rows %v; want one a fingerprint", fps)
	}
	// Blocking first, then the newest.
	if fps[0] != "own" || fps[1] != "dup" {
		t.Errorf("order %v; want the blocking rows first, the newest of them first", fps[:3])
	}
	dup := rows[1]
	want := PastDismissalView{Skill: "code-review", Severity: "blocking", File: "api/handler.go", Title: "Title of dup",
		Reason: "the blocking one", By: "carol (gitlab main)", At: now.Add(-72 * time.Hour).UTC(), Number: 3, Fingerprint: "dup"}
	if d := dup.At.Sub(want.At); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("at = %v; want %v", dup.At, want.At)
	}
	dup.At = want.At
	if dup != want {
		t.Errorf("dup = %+v; want %+v", dup, want)
	}
}

func TestPastDismissalsNeedsMaintainer(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	e.pastDismissal(1, "fp1", "suggestion", "reason", "bob (gitlab main)", e.clock.now().Add(-time.Hour))
	v := e.signInRole(store.RoleViewer)
	wantCode(t, e.get(v, pastURL(buttonProject)), "viewer list", http.StatusForbidden)
	wantCode(t, e.send(v, "POST", stopURL(buttonProject), `{"fingerprint":"fp1"}`), "viewer stop", http.StatusForbidden)
	if len(e.auditOf("dismissal_stopped")) != 0 {
		t.Error("a viewer's stop was audited")
	}
	if !e.stillTeaches("fp1") {
		t.Error("a viewer stopped a dismissal")
	}

	for _, role := range []string{store.RoleMaintainer, store.RoleAdmin} {
		e2 := newEnv(t, nil)
		e2.buttonBlockingReview()
		c := e2.signInRole(role)
		wantCode(t, e2.get(c, pastURL(buttonProject)), role+" list", http.StatusOK)
		wantCode(t, e2.send(c, "POST", stopURL(buttonProject), `{"fingerprint":"fp1"}`), role+" stop", http.StatusOK)
	}
}

// stillTeaches tells whether the dismissal of fingerprint on change 1 teaches.
func (e *env) stillTeaches(fingerprint string) bool {
	e.t.Helper()
	for _, d := range e.dismissalsOnChange(1) {
		if d.Fingerprint == fingerprint {
			return d.Teaches
		}
	}
	e.t.Fatalf("no dismissal of %s on change 1", fingerprint)
	return false
}

func (e *env) dismissalsOnChange(number int64) []store.Dismissal {
	e.t.Helper()
	ds, err := e.st.Dismissals(ctx, store.MRKey{Instance: "main", ProjectID: testID(buttonProject), IID: number})
	if err != nil {
		e.t.Fatal(err)
	}
	return ds
}

// The list and Stop using work on a repository the config file sets up, which
// the settings can't change from here.
func TestPastDismissalsConfigRepo(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	e.pastDismissal(1, "fp1", "suggestion", "reason", "bob (gitlab main)", e.clock.now().Add(-time.Hour))
	c := e.signInRole(store.RoleMaintainer)
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/"+buttonProject+"/settings", `{"approve":false}`),
		"settings of a config repository", http.StatusConflict)
	if rows := teaching(t, e, c); len(rows) != 1 {
		t.Fatalf("rows = %+v; want 1", rows)
	}
	wantCode(t, e.send(c, "POST", stopURL(buttonProject), `{"fingerprint":"fp1"}`), "stop", http.StatusOK)
	if rows := teaching(t, e, c); len(rows) != 0 {
		t.Errorf("rows = %+v; want none after stopping", rows)
	}
}

// A repository with nothing dismissed has nothing that teaches, and there is
// nothing to stop.
func TestPastDismissalsNoRuns(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInRole(store.RoleMaintainer)
	rec := e.get(c, pastURL(buttonProject))
	wantCode(t, rec, "list", http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %s; want []", got)
	}
	rec = e.send(c, "POST", stopURL(buttonProject), `{"fingerprint":"fp1"}`)
	wantCode(t, rec, "stop", http.StatusOK)
	if got := statusOf(t, rec); got != "not teaching" {
		t.Errorf("status = %q; want not teaching", got)
	}
	if len(e.auditOf("dismissal_stopped")) != 0 {
		t.Error("stopping nothing was audited")
	}
}

// Stop using stops every dismissal of the finding in the repository and
// nothing else: the dismissals stay on their changes, no review is decided
// again and the platform isn't asked.
func TestStopUsing(t *testing.T) {
	e := newEnv(t, nil)
	runID := e.buttonBlockingReview()
	now := e.clock.now()
	// The finding is dismissed on this change and on another.
	e.pastDismissal(7, "fp1", "blocking", "constant", "bob (gitlab main)", now.Add(-time.Hour))
	e.pastDismissal(8, "fp1", "suggestion", "later", "bob (gitlab main)", now.Add(-time.Minute))
	e.pastDismissal(7, "fp2", "suggestion", "other", "bob (gitlab main)", now.Add(-time.Hour))
	c := e.signInRole(store.RoleMaintainer)

	rec := e.send(c, "POST", stopURL(buttonProject), `{"fingerprint":"fp1"}`)
	wantCode(t, rec, "stop", http.StatusOK)
	if got := statusOf(t, rec); got != "stopped" {
		t.Errorf("status = %q; want stopped", got)
	}
	rows := teaching(t, e, c)
	if len(rows) != 1 || rows[0].Fingerprint != "fp2" {
		t.Fatalf("rows = %+v; want only fp2: stopping fp1 must not bring up its other dismissal", rows)
	}
	// The dismissals stay where they were made.
	on7 := e.dismissalsOnChange(7)
	if len(on7) != 2 || len(e.dismissalsOnChange(8)) != 1 {
		t.Fatalf("dismissals on 7: %+v; want both kept, and 8's too", on7)
	}
	for _, d := range on7 {
		if d.Fingerprint == "fp1" && (d.Teaches || d.Severity != "blocking" || d.Reason != "constant") {
			t.Errorf("fp1 on 7 = %+v; want it kept as it was, not teaching", d)
		}
		if d.Fingerprint == "fp2" && !d.Teaches {
			t.Error("fp2 stopped teaching")
		}
	}
	a := e.auditOf("dismissal_stopped")
	if len(a) != 1 || a[0].Actor != "alice (gitlab main)" || a[0].Project != buttonProject || !strings.Contains(a[0].Detail, "fingerprint=fp1") {
		t.Errorf("audit = %+v", a)
	}
	// No review decided again, no platform call.
	runs := e.runRows()
	if len(runs) != 1 || runs[0].ID != runID || runs[0].State != string(store.Published) || runs[0].Redecide {
		t.Errorf("runs = %+v; want the run untouched", runs)
	}
	if n := e.wakes.Load(); n != 0 {
		t.Errorf("Wake called %d times; want 0", n)
	}
	if n := e.src["main"].changeCalls; n != 0 {
		t.Errorf("the platform was asked %d times", n)
	}
}

func TestStopUsingTwice(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	e.pastDismissal(1, "fp1", "suggestion", "reason", "bob (gitlab main)", e.clock.now().Add(-time.Hour))
	c := e.signInRole(store.RoleMaintainer)
	wantCode(t, e.send(c, "POST", stopURL(buttonProject), `{"fingerprint":"fp1"}`), "stop", http.StatusOK)
	rec := e.send(c, "POST", stopURL(buttonProject), `{"fingerprint":"fp1"}`)
	wantCode(t, rec, "stop again", http.StatusOK)
	if got := statusOf(t, rec); got != "not teaching" {
		t.Errorf("status = %q; want not teaching", got)
	}
	if n := len(e.auditOf("dismissal_stopped")); n != 1 {
		t.Errorf("%d audit entries; want 1", n)
	}
	// A fingerprint nobody dismissed is the same.
	rec = e.send(c, "POST", stopURL(buttonProject), `{"fingerprint":"unknown"}`)
	wantCode(t, rec, "stop unknown", http.StatusOK)
	if got := statusOf(t, rec); got != "not teaching" {
		t.Errorf("status = %q; want not teaching", got)
	}
}

func TestStopUsingBadRequest(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)
	for _, body := range []string{`{"fingerprint":""}`, `{"fingerprint":"  "}`, `{}`, ``, `{"fingerprint":"x","reason":"y"}`, `[]`} {
		wantCode(t, e.send(c, "POST", stopURL(buttonProject), body), "stop "+body, http.StatusBadRequest)
	}
	if len(e.auditOf("dismissal_stopped")) != 0 {
		t.Error("a bad request was audited")
	}
}

// The project is the one the session sees, not whichever its path has runs
// under: a repository deleted and made again on the platform has a new ID, and
// what was dismissed under the old one is another project's.
func TestPastDismissalsProjectID(t *testing.T) {
	const oldID = 999
	setup := func() *env {
		e := newEnv(t, nil)
		e.buttonBlockingReview()
		now := e.clock.now()
		// The newest run under the path is of the old project.
		e.addRun(runFix{project: buttonProject, projectID: oldID, number: 3, head: head('2'),
			result: resultJSON(head('2'), buttonSkill("code-review")), at: now.Add(-time.Minute)})
		e.pastDismissalIn(oldID, 3, "oldfp", "suggestion", "of the old project", "bob (gitlab main)", now.Add(-time.Hour))
		e.pastDismissal(1, "newfp", "suggestion", "of this project", "bob (gitlab main)", now.Add(-time.Hour))
		return e
	}

	e := setup()
	c := e.signInRole(store.RoleMaintainer)
	rows := teaching(t, e, c)
	if len(rows) != 1 || rows[0].Fingerprint != "newfp" {
		t.Fatalf("rows = %+v; want the session's project alone", rows)
	}
	rec := e.send(c, "POST", stopURL(buttonProject), `{"fingerprint":"oldfp"}`)
	wantCode(t, rec, "stop another project's", http.StatusOK)
	if got := statusOf(t, rec); got != "not teaching" {
		t.Errorf("status = %q; want not teaching: that finding isn't this project's", got)
	}
	rec = e.send(c, "POST", stopURL(buttonProject), `{"fingerprint":"newfp"}`)
	if got := statusOf(t, rec); got != "stopped" {
		t.Errorf("status = %q; want stopped", got)
	}

	// An operator has no visible IDs: the project the path is stored under.
	e2 := setup()
	rows = teaching(t, e2, e2.signInOperator())
	if len(rows) != 1 || rows[0].Fingerprint != "newfp" {
		t.Errorf("operator rows = %+v; want the stored project's", rows)
	}
}
