package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	buttonProject = "acme/widgets"
	blockingFP    = "a1b2c3d4e5f6"
	suggestionFP  = "b2c3d4e5f6a1"
	// bobID opened the change; alice (user 9 on GitLab) did not.
	bobID = 12
)

func buttonURL(action string) string {
	return "/api/reviews/main/" + buttonProject + "/7/" + action
}

// buttonSkill is a skill with one blocking finding (fingerprint blockingFP)
// and one suggestion (suggestionFP).
func buttonSkill(name string) map[string]any {
	return skillJSON(name, "has_blocking_issues",
		map[string]any{"id": "a1b2c3", "fingerprint": blockingFP, "file": "api/handler.go", "line": 42,
			"severity": "blocking", "title": "Request body is used before validation", "explanation": "x"},
		map[string]any{"id": "b2c3d4", "fingerprint": suggestionFP, "file": "api/handler.go", "line": 50,
			"severity": "suggestion", "title": "Name it better", "explanation": "y"})
}

// buttonReview stores a published review of acme/widgets!7 at head, opened
// by bob (user 12), and has the platform report that change as open at head.
func (e *env) buttonReview(result string) string {
	e.t.Helper()
	h := head('1')
	e.src["main"].set(func(f *fakeSource) {
		f.change = forge.Change{State: forge.StateOpen, HeadSHA: h, TargetBranch: "main", Author: "bob", AuthorID: bobID}
	})
	// (An operator finds a project by its path in the projects pergent.sh knows.)
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID(buttonProject), Path: buttonProject}); err != nil {
		e.t.Fatal(err)
	}
	return e.addRun(runFix{project: buttonProject, number: 7, head: h, result: result, user: "bob",
		outcome: OutcomeBlocking, title: "Add the handler"})
}

func (e *env) buttonBlockingReview() string {
	return e.buttonReview(resultJSON(head('1'), buttonSkill("code-review"), skillJSON("style", "no_blocking_issues")))
}

// signInRole signs alice in on main with role on every repository there.
func (e *env) signInRole(role string) *http.Cookie {
	e.t.Helper()
	e.gl.setProject(buttonProject, gitlabProject("private", 20, 0))
	e.aliceOn("main", role)
	return e.signIn("gitlab", "main")
}

func (e *env) auditOf(action string) []store.AuditEntry {
	e.t.Helper()
	all, err := e.st.AuditLog(ctx, 0, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.AuditEntry
	for _, a := range all {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

type runRow struct {
	ID, State, Trigger string
	Subset             []string
	Base               *string
	Redecide           bool
}

// runRows are the runs of acme/widgets!7, oldest first.
func (e *env) runRows() []runRow {
	e.t.Helper()
	rows, err := e.db.Query(ctx, `SELECT id::text, state, trigger, coalesce(skill_subset, '{}'), base_run_id::text, redecide
		FROM runs WHERE instance = 'main' AND mr_iid = 7 ORDER BY created_at, id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []runRow
	for rows.Next() {
		var r runRow
		if err := rows.Scan(&r.ID, &r.State, &r.Trigger, &r.Subset, &r.Base, &r.Redecide); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *env) dismissalsOf() []store.Dismissal {
	e.t.Helper()
	mr := store.MRKey{Instance: "main", ProjectID: testID(buttonProject), IID: 7}
	ds, err := e.st.Dismissals(ctx, mr)
	if err != nil {
		e.t.Fatal(err)
	}
	return ds
}

func statusOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	return v.Status
}

func TestRerunButton(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)

	rec := e.send(c, "POST", buttonURL("rerun"), `{"failed":false}`)
	wantCode(t, rec, "rerun", http.StatusAccepted)
	if got := statusOf(t, rec); got != "queued" {
		t.Errorf("status = %q; want queued", got)
	}
	rows := e.runRows()
	if len(rows) != 2 || rows[1].State != "queued" || rows[1].Trigger != store.TriggerRerun || len(rows[1].Subset) != 0 {
		t.Fatalf("runs = %+v; want the published run and a queued re-run of all skills", rows)
	}
	if n := e.wakes.Load(); n != 1 {
		t.Errorf("Wake called %d times; want 1", n)
	}
	a := e.auditOf("review_rerun")
	if len(a) != 1 || a[0].Actor != "alice (gitlab main)" || a[0].Instance != "main" || a[0].Project != buttonProject ||
		!strings.Contains(a[0].Detail, "mr=7") || !strings.Contains(a[0].Detail, "head="+head('1')) {
		t.Errorf("audit = %+v", a)
	}

	// The same head again, while the first re-run waits: not queued twice.
	rec = e.send(c, "POST", buttonURL("rerun"), `{}`)
	wantCode(t, rec, "second rerun", http.StatusAccepted)
	if got := statusOf(t, rec); got != "already queued" {
		t.Errorf("second status = %q; want already queued", got)
	}
	if n := len(e.runRows()); n != 2 {
		t.Errorf("%d runs; want 2", n)
	}
	if n := len(e.auditOf("review_rerun")); n != 1 {
		t.Errorf("%d audit entries; want 1", n)
	}
	if n := e.wakes.Load(); n != 1 {
		t.Errorf("Wake called %d times; want 1", n)
	}
}

func TestRerunButtonCap(t *testing.T) {
	// A config repository takes its cap from the config file.
	e := newEnv(t, func(cfg *config.Config) { cfg.Repositories[0].RerunLimitPerMR = ptr(1) })
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)

	wantCode(t, e.send(c, "POST", buttonURL("rerun"), `{}`), "first rerun", http.StatusAccepted)
	// Whatever became of that run, it counts.
	e.exec(`UPDATE runs SET state = 'superseded' WHERE trigger = 'rerun'`)
	rec := e.send(c, "POST", buttonURL("rerun"), `{}`)
	wantCode(t, rec, "rerun at the cap", http.StatusTooManyRequests)
	if got := errorOf(t, rec); got != "re-run limit reached for this MR; it resets at 00:00 UTC" {
		t.Errorf("error = %q", got)
	}
	if n := len(e.runRows()); n != 2 {
		t.Errorf("%d runs; want 2 (nothing queued at the cap)", n)
	}
	if n := e.wakes.Load(); n != 1 {
		t.Errorf("Wake called %d times; want 1", n)
	}

	// The repository's cap counts the re-runs of every change.
	e2 := newEnv(t, func(cfg *config.Config) { cfg.Repositories[0].RerunLimitPerRepo = ptr(1) })
	e2.buttonBlockingReview()
	c2 := e2.signInRole(store.RoleMaintainer)
	wantCode(t, e2.send(c2, "POST", buttonURL("rerun"), `{}`), "first rerun", http.StatusAccepted)
	e2.exec(`UPDATE runs SET state = 'superseded' WHERE trigger = 'rerun'`)
	rec = e2.send(c2, "POST", buttonURL("rerun"), `{}`)
	wantCode(t, rec, "rerun at the repository cap", http.StatusTooManyRequests)
}

func TestRerunFailedButton(t *testing.T) {
	t.Run("only the failed skills", func(t *testing.T) {
		e := newEnv(t, nil)
		prior := e.buttonReview(resultJSON(head('1'), skillJSON("code-review", "no_blocking_issues"), skillJSON("style", "failed")))
		c := e.signInRole(store.RoleMaintainer)
		rec := e.send(c, "POST", buttonURL("rerun"), `{"failed":true}`)
		wantCode(t, rec, "rerun failed", http.StatusAccepted)
		if got := statusOf(t, rec); got != "queued" {
			t.Errorf("status = %q; want queued", got)
		}
		rows := e.runRows()
		if len(rows) != 2 || rows[1].Trigger != store.TriggerRerunFailed || !slices.Equal(rows[1].Subset, []string{"style"}) ||
			rows[1].Base == nil || *rows[1].Base != prior {
			t.Fatalf("runs = %+v; want a re-run of style that keeps the published run's results", rows)
		}
		a := e.auditOf("review_rerun_failed")
		if len(a) != 1 || !strings.Contains(a[0].Detail, "skills=style") {
			t.Errorf("audit = %+v", a)
		}
	})
	t.Run("nothing failed", func(t *testing.T) {
		e := newEnv(t, nil)
		e.buttonReview(cleanResult(head('1')))
		c := e.signInRole(store.RoleMaintainer)
		rec := e.send(c, "POST", buttonURL("rerun"), `{"failed":true}`)
		wantCode(t, rec, "rerun failed", http.StatusAccepted)
		if got := statusOf(t, rec); got != "nothing failed" {
			t.Errorf("status = %q; want nothing failed", got)
		}
		if n := len(e.runRows()); n != 1 || e.wakes.Load() != 0 || len(e.auditOf("review_rerun_failed")) != 0 {
			t.Errorf("runs %d, wakes %d, audit %d; want nothing done", n, e.wakes.Load(), len(e.auditOf("review_rerun_failed")))
		}
	})
	t.Run("dismissed blocking findings are not failures", func(t *testing.T) {
		e := newEnv(t, nil)
		e.buttonBlockingReview()
		if _, err := e.st.Dismiss(ctx, store.Dismissal{MR: store.MRKey{Instance: "main", ProjectID: testID(buttonProject), IID: 7},
			Fingerprint: blockingFP, Severity: "blocking", Reason: "ok", Actor: "x"}); err != nil {
			t.Fatal(err)
		}
		c := e.signInRole(store.RoleMaintainer)
		rec := e.send(c, "POST", buttonURL("rerun"), `{"failed":true}`)
		wantCode(t, rec, "rerun failed", http.StatusAccepted)
		if got := statusOf(t, rec); got != "nothing failed" {
			t.Errorf("status = %q; want nothing failed", got)
		}
	})
	t.Run("no per-skill results", func(t *testing.T) {
		e := newEnv(t, nil)
		errResult := `{"schema_version":1,"status":"error","pergent_version":"0.1.0","base":"` + baseSHA + `","head":"` + head('1') +
			`","skills":[],"error":{"code":"internal","message":"boom"},"comment":{"markdown":"<!-- pergent -->\nfailed","findings_block":""}}`
		e.buttonReview(errResult)
		c := e.signInRole(store.RoleMaintainer)
		rec := e.send(c, "POST", buttonURL("rerun"), `{"failed":true}`)
		wantCode(t, rec, "rerun failed", http.StatusAccepted)
		if got := statusOf(t, rec); got != "whole review" {
			t.Errorf("status = %q; want whole review", got)
		}
		rows := e.runRows()
		if len(rows) != 2 || rows[1].Trigger != store.TriggerRerun || len(rows[1].Subset) != 0 || rows[1].Base != nil {
			t.Errorf("runs = %+v; want a full re-run", rows)
		}
		if a := e.auditOf("review_rerun_failed"); len(a) != 1 {
			t.Errorf("audit = %+v", a)
		}
	})
	t.Run("no review of the current head", func(t *testing.T) {
		e := newEnv(t, nil)
		e.buttonBlockingReview()
		e.src["main"].set(func(f *fakeSource) { f.change.HeadSHA = head('2') })
		c := e.signInRole(store.RoleMaintainer)
		rec := e.send(c, "POST", buttonURL("rerun"), `{"failed":true}`)
		if got := statusOf(t, rec); rec.Code != http.StatusAccepted || got != "whole review" {
			t.Errorf("rerun failed = %d %s; want 202 whole review", rec.Code, rec.Body)
		}
		if rows := e.runRows(); len(rows) != 2 {
			t.Errorf("runs = %+v", rows)
		}
	})
}

func TestRerunButtonRefusals(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)
	set := func(fn func(*fakeSource)) { e.src["main"].set(fn) }

	set(func(f *fakeSource) { f.change.State = forge.StateMerged })
	wantCode(t, e.send(c, "POST", buttonURL("rerun"), `{}`), "merged change", http.StatusConflict)
	// acme/widgets reviews drafts by default.
	set(func(f *fakeSource) { f.change.State, f.change.Draft = forge.StateOpen, true })
	wantCode(t, e.send(c, "POST", buttonURL("rerun"), `{}`), "draft", http.StatusAccepted)

	set(func(f *fakeSource) { f.changeErr = errors.New("gitlab: status 500") })
	wantCode(t, e.send(c, "POST", buttonURL("rerun"), `{}`), "platform error", http.StatusBadGateway)
	wantCode(t, e.send(c, "POST", "/api/reviews/main/acme/widgets/8/rerun", `{}`), "unknown change", http.StatusNotFound)
	wantCode(t, e.send(c, "POST", buttonURL("rerun"), `{"nope":1}`), "unknown field", http.StatusBadRequest)
	wantCode(t, e.send(c, "POST", buttonURL("rerun"), `not json`), "bad body", http.StatusBadRequest)
	wantCode(t, e.send(c, "POST", buttonURL("explode"), `{}`), "unknown action", http.StatusNotFound)
	wantCode(t, e.send(c, "POST", "/api/reviews/main/acme/widgets/x/rerun", `{}`), "bad number", http.StatusNotFound)
	wantCode(t, e.send(c, "POST", "/api/reviews/main", `{}`), "no repository", http.StatusNotFound)
}

func TestRerunButtonRefusesADraftWhereDraftsAreNotReviewed(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) { cfg.Repositories[0].ReviewDrafts = ptr(false) })
	e.buttonBlockingReview()
	e.src["main"].set(func(f *fakeSource) { f.change.Draft = true })
	c := e.signInRole(store.RoleMaintainer)
	rec := e.send(c, "POST", buttonURL("rerun"), `{}`)
	wantCode(t, rec, "draft", http.StatusConflict)
	if n := len(e.runRows()); n != 1 {
		t.Errorf("%d runs; want 1", n)
	}
}

func TestDismissButtonBlockingNeedsMaintainer(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	v := e.signInRole(store.RoleViewer)
	rec := e.send(v, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"nope"}`)
	wantCode(t, rec, "viewer dismiss", http.StatusForbidden)
	if len(e.dismissalsOf()) != 0 {
		t.Fatal("a viewer dismissed a finding")
	}

	// A maintainer: a blocking finding needs a reason.
	e2 := newEnv(t, nil)
	runID := e2.buttonBlockingReview()
	c := e2.signInRole(store.RoleMaintainer)
	for _, reason := range []string{"", "   "} {
		rec = e2.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"`+reason+`"}`)
		wantCode(t, rec, "dismiss without a reason", http.StatusBadRequest)
		if !strings.Contains(errorOf(t, rec), "reason") {
			t.Errorf("error = %q; want it to ask for a reason", errorOf(t, rec))
		}
	}
	if len(e2.dismissalsOf()) != 0 {
		t.Fatal("a blocking finding was dismissed without a reason")
	}
	wantCode(t, e2.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"ffffffffffff","reason":"x"}`),
		"unknown finding", http.StatusNotFound)

	rec = e2.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"the input is a constant"}`)
	wantCode(t, rec, "dismiss", http.StatusOK)
	ds := e2.dismissalsOf()
	if len(ds) != 1 || ds[0].Fingerprint != blockingFP || ds[0].Severity != "blocking" ||
		ds[0].Reason != "the input is a constant" || ds[0].Actor != "alice (gitlab main)" || ds[0].ActorID == nil {
		t.Fatalf("dismissals = %+v", ds)
	}
	// The review is decided again, not run again.
	rows := e2.runRows()
	if len(rows) != 1 || rows[0].ID != runID || rows[0].State != "reported" {
		t.Errorf("runs = %+v; want the published run republished", rows)
	}
	if n := e2.wakes.Load(); n != 1 {
		t.Errorf("Wake called %d times; want 1", n)
	}
	a := e2.auditOf("finding_dismissed")
	if len(a) != 1 || a[0].Actor != "alice (gitlab main)" || a[0].Project != buttonProject ||
		!strings.Contains(a[0].Detail, "mr=7") || !strings.Contains(a[0].Detail, "head="+head('1')) ||
		!strings.Contains(a[0].Detail, "fingerprint="+blockingFP) || !strings.Contains(a[0].Detail, "the input is a constant") {
		t.Errorf("audit = %+v", a)
	}

	// Pressing it again changes nothing.
	rec = e2.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"again"}`)
	wantCode(t, rec, "dismiss again", http.StatusOK)
	if got := statusOf(t, rec); got != "already dismissed" {
		t.Errorf("status = %q; want already dismissed", got)
	}
	if len(e2.auditOf("finding_dismissed")) != 1 || e2.dismissalsOf()[0].Reason != "the input is a constant" {
		t.Error("a second dismissal changed something")
	}
}

// A reason is cut to nothing but text: images are stripped before it is
// stored, and it can be 500 characters at most.
func TestDismissButtonReason(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)

	long := strings.Repeat("é", 501)
	rec := e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"`+long+`"}`)
	wantCode(t, rec, "a reason of 501 characters", http.StatusBadRequest)
	if got := errorOf(t, rec); got != "the reason is too long" {
		t.Errorf("error = %q", got)
	}
	if len(e.dismissalsOf()) != 0 {
		t.Fatal("a too long reason was stored")
	}

	// Only an image: nothing is left, and a blocking finding needs a reason.
	rec = e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"![x](https://evil.example/p.png)"}`)
	wantCode(t, rec, "an image as the reason", http.StatusBadRequest)

	// 500 characters, with an image after them that is stripped.
	ok := strings.Repeat("é", 500)
	wantCode(t, e.send(c, "POST", buttonURL("dismiss"),
		`{"fingerprint":"`+blockingFP+`","reason":"`+ok+` ![x](https://evil.example/p.png)"}`), "500 characters", http.StatusOK)
	if ds := e.dismissalsOf(); len(ds) != 1 || ds[0].Reason != ok {
		t.Fatalf("dismissals = %+v; want the reason without its image", ds)
	}
}

func TestDismissButtonSuggestion(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)
	// A suggestion needs no reason.
	wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+suggestionFP+`"}`), "dismiss suggestion", http.StatusOK)
	ds := e.dismissalsOf()
	if len(ds) != 1 || ds[0].Severity != "suggestion" || ds[0].Reason != "" {
		t.Fatalf("dismissals = %+v", ds)
	}
}

// A dismissal from the dashboard teaches, and keeps who dismissed it by their
// platform user ID: the session's, and none for an account that has no
// platform user here.
func TestDashboardDismissTeaches(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)
	wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"constant"}`), "dismiss blocking", http.StatusOK)
	wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+suggestionFP+`"}`), "dismiss suggestion", http.StatusOK)
	ds := e.dismissalsOf()
	if len(ds) != 2 {
		t.Fatalf("dismissals = %+v", ds)
	}
	for _, d := range ds {
		if !d.Teaches || d.DismisserPlatformID != aliceID["main"] || d.Skill != "code-review" || d.File != "api/handler.go" {
			t.Errorf("dismissal = %+v; want it to teach, from alice (%d) with the finding's details", d, aliceID["main"])
		}
	}
	if byFP := map[string]string{ds[0].Fingerprint: ds[0].Title, ds[1].Fingerprint: ds[1].Title}; byFP[blockingFP] != "Request body is used before validation" {
		t.Errorf("titles = %v", byFP)
	}

	// A password admin has no platform user.
	e2 := newEnv(t, nil)
	e2.buttonBlockingReview()
	wantCode(t, e2.send(e2.signInOperator(), "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"constant"}`),
		"operator dismiss", http.StatusOK)
	if ds := e2.dismissalsOf(); len(ds) != 1 || !ds[0].Teaches || ds[0].DismisserPlatformID != 0 {
		t.Errorf("dismissals = %+v; want it to teach, with no dismisser ID", ds)
	}
}

func TestDismisserPlatformID(t *testing.T) {
	user := Session{Platform: "gitlab", Instance: "main", UserID: 9}
	for _, c := range []struct {
		name     string
		se       Session
		instance string
		want     int64
	}{
		{"the change's instance", user, "main", 9},
		{"another instance", user, "gh", 0},
		{"a password account", Session{Operator: true}, "main", 0},
		{"no user ID", Session{Instance: "main"}, "main", 0},
	} {
		if got := dismisserPlatformID(c.se, c.instance); got != c.want {
			t.Errorf("%s: dismisserPlatformID = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestButtonAuthorCannotDismissBlocking(t *testing.T) {
	// alice (user 9) opened the change.
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	e.src["main"].set(func(f *fakeSource) { f.change.Author, f.change.AuthorID = "alice", aliceID["main"] })
	c := e.signInRole(store.RoleAdmin)

	rec := e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"mine"}`)
	wantCode(t, rec, "author dismisses blocking", http.StatusForbidden)
	if len(e.dismissalsOf()) != 0 || len(e.auditOf("finding_dismissed")) != 0 {
		t.Error("the author dismissed a blocking finding of their own change")
	}
	// A suggestion is anyone's who may re-run, the author included.
	wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+suggestionFP+`"}`), "author dismisses suggestion", http.StatusOK)

	t.Run("fails closed", func(t *testing.T) {
		for name, fn := range map[string]func(*fakeSource){
			"platform error": func(f *fakeSource) { f.changeErr = errors.New("gitlab: status 500") },
			"unknown author": func(f *fakeSource) { f.change.AuthorID = 0 },
		} {
			e := newEnv(t, nil)
			e.buttonBlockingReview()
			e.src["main"].set(fn)
			c := e.signInRole(store.RoleMaintainer)
			rec := e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"x"}`)
			if rec.Code < 400 || len(e.dismissalsOf()) != 0 {
				t.Errorf("%s: dismiss = %d %s, %d dismissals; want it refused", name, rec.Code, rec.Body, len(e.dismissalsOf()))
			}
			// A suggestion needs no author check.
			wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+suggestionFP+`"}`),
				name+": suggestion", http.StatusOK)
		}
	})

	t.Run("someone else's change", func(t *testing.T) {
		e := newEnv(t, nil)
		e.buttonBlockingReview() // opened by bob
		c := e.signInRole(store.RoleMaintainer)
		wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"x"}`),
			"dismiss", http.StatusOK)
	})

	t.Run("a password admin is never the author", func(t *testing.T) {
		e := newEnv(t, nil)
		e.buttonBlockingReview()
		c := e.signInOperator()
		wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"x"}`),
			"operator dismiss", http.StatusOK)
		ds := e.dismissalsOf()
		if len(ds) != 1 || ds[0].Actor != "root (password)" {
			t.Errorf("dismissals = %+v", ds)
		}
		// The change's state is read, its author is not asked about.
		if n := e.src["main"].changeCalls; n != 1 {
			t.Errorf("the platform was asked %d times, want 1", n)
		}
	})

	t.Run("a password admin is refused when the change can't be read", func(t *testing.T) {
		e := newEnv(t, nil)
		e.buttonBlockingReview()
		e.src["main"].set(func(f *fakeSource) { f.changeErr = errors.New("down") })
		c := e.signInOperator()
		wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"x"}`),
			"operator dismiss", http.StatusBadGateway)
		if len(e.dismissalsOf()) != 0 {
			t.Error("a finding was dismissed without knowing the change's state")
		}
	})
}

func TestRestoreButton(t *testing.T) {
	mr := store.MRKey{Instance: "main", ProjectID: testID(buttonProject), IID: 7}
	// A review approved with its blocking finding dismissed.
	dismissedReview := func() (*env, string) {
		e := newEnv(t, nil)
		runID := e.buttonBlockingReview()
		if _, err := e.st.Dismiss(ctx, store.Dismissal{MR: mr, Fingerprint: blockingFP, Severity: "blocking",
			Reason: "constant", Actor: "bob (gitlab main)"}); err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE runs SET approved = true WHERE id = $1::uuid`, runID)
		// alice opened the change: restoring only makes the review stricter.
		e.src["main"].set(func(f *fakeSource) { f.change.Author, f.change.AuthorID = "alice", aliceID["main"] })
		return e, runID
	}

	e, _ := dismissedReview()
	v := e.signInRole(store.RoleViewer)
	wantCode(t, e.send(v, "POST", buttonURL("restore"), `{"fingerprint":"`+blockingFP+`"}`), "viewer restore", http.StatusForbidden)
	if len(e.dismissalsOf()) != 1 {
		t.Fatal("a viewer restored a finding")
	}

	e2, runID := dismissedReview()
	c := e2.signInRole(store.RoleMaintainer)
	rec := e2.send(c, "POST", buttonURL("restore"), `{"fingerprint":"`+blockingFP+`"}`)
	wantCode(t, rec, "restore", http.StatusOK)
	if got := statusOf(t, rec); got != "restored" {
		t.Errorf("status = %q; want restored", got)
	}
	if len(e2.dismissalsOf()) != 0 {
		t.Error("the dismissal is still there")
	}
	// The review is decided again (the engine then withdraws the approval),
	// and no run is made.
	rows := e2.runRows()
	if len(rows) != 1 || rows[0].ID != runID || rows[0].State != "reported" {
		t.Errorf("runs = %+v; want the published run republished", rows)
	}
	if n := e2.wakes.Load(); n != 1 {
		t.Errorf("Wake called %d times; want 1", n)
	}
	a := e2.auditOf("finding_restored")
	if len(a) != 1 || a[0].Actor != "alice (gitlab main)" || !strings.Contains(a[0].Detail, "fingerprint="+blockingFP) ||
		!strings.Contains(a[0].Detail, "mr=7") || !strings.Contains(a[0].Detail, "head="+head('1')) {
		t.Errorf("audit = %+v", a)
	}

	// Restoring what is not dismissed changes nothing and says so (the review
	// is decided again all the same, see TestRestoreRetryRedecides).
	rec = e2.send(c, "POST", buttonURL("restore"), `{"fingerprint":"`+blockingFP+`"}`)
	wantCode(t, rec, "restore again", http.StatusOK)
	if got := statusOf(t, rec); got != "not dismissed" {
		t.Errorf("status = %q; want not dismissed", got)
	}
	if len(e2.auditOf("finding_restored")) != 1 {
		t.Error("restoring nothing was audited")
	}
}

func TestButtonsWorkOnConfigRepos(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)
	// acme/widgets is in the config file: its settings can't be changed here...
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/"+buttonProject+"/settings", `{"approve":false}`),
		"settings of a config repository", http.StatusConflict)
	// ...but its reviews can be acted on.
	wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"x"}`), "dismiss", http.StatusOK)
	wantCode(t, e.send(c, "POST", buttonURL("restore"), `{"fingerprint":"`+blockingFP+`"}`), "restore", http.StatusOK)
	wantCode(t, e.send(c, "POST", buttonURL("rerun"), `{}`), "rerun", http.StatusAccepted)

	// And a repository switched on in the dashboard behaves the same.
	e2 := newEnv(t, nil)
	e2.dashboardRepo("main", buttonProject+"-2", nil)
	e2.gl.setProject(buttonProject+"-2", gitlabProject("private", 20, 0))
	e2.addRun(runFix{project: buttonProject + "-2", number: 7, head: head('1'),
		result: resultJSON(head('1'), buttonSkill("code-review")), outcome: OutcomeBlocking})
	e2.src["main"].set(func(f *fakeSource) {
		f.change = forge.Change{State: forge.StateOpen, HeadSHA: head('1'), TargetBranch: "main", AuthorID: bobID}
	})
	e2.aliceOn("main", store.RoleMaintainer)
	c2 := e2.signIn("gitlab", "main")
	base := "/api/reviews/main/" + buttonProject + "-2/7/"
	wantCode(t, e2.send(c2, "POST", base+"dismiss", `{"fingerprint":"`+suggestionFP+`"}`), "dismiss", http.StatusOK)
	wantCode(t, e2.send(c2, "POST", base+"rerun", `{}`), "rerun", http.StatusAccepted)
}

func TestViewerCannotPressButtons(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	v := e.signInRole(store.RoleViewer)
	for _, w := range []struct{ action, body string }{
		{"rerun", `{}`}, {"rerun", `{"failed":true}`},
		{"dismiss", `{"fingerprint":"` + suggestionFP + `"}`},
		{"restore", `{"fingerprint":"` + suggestionFP + `"}`},
	} {
		wantCode(t, e.send(v, "POST", buttonURL(w.action), w.body), w.action, http.StatusForbidden)
	}
	if n := len(e.runRows()); n != 1 || len(e.dismissalsOf()) != 0 || e.wakes.Load() != 0 {
		t.Errorf("runs %d, dismissals %d, wakes %d; want nothing done", n, len(e.dismissalsOf()), e.wakes.Load())
	}
	for _, a := range []string{"review_rerun", "review_rerun_failed", "finding_dismissed", "finding_restored"} {
		if len(e.auditOf(a)) != 0 {
			t.Errorf("%s was audited", a)
		}
	}
	if n := e.src["main"].changeCalls; n != 0 {
		t.Errorf("the platform was asked %d times", n)
	}

	// A repository the session can't see is not found, whatever its role elsewhere.
	wantCode(t, e.send(v, "POST", "/api/reviews/main/acme/payments/7/rerun", `{}`), "invisible repository", http.StatusNotFound)
}

func TestReviewViewCarriesRoleAndDismissals(t *testing.T) {
	roles := map[string]struct {
		role string
		can  CanView
	}{
		store.RoleViewer:     {"viewer", CanView{}},
		store.RoleMaintainer: {"maintainer", CanView{Rerun: true, DismissBlocking: true, DismissSuggestion: true, Restore: true}},
		store.RoleAdmin:      {"admin", CanView{Rerun: true, DismissBlocking: true, DismissSuggestion: true, Restore: true}},
	}
	for role, want := range roles {
		t.Run(role, func(t *testing.T) {
			e := newEnv(t, nil)
			e.buttonBlockingReview()
			c := e.signInRole(role)
			v := getJSON[ReviewDetailView](e, c, "/api/reviews/main/"+buttonProject+"/7")
			if v.Role != want.role || v.Can != want.can {
				t.Errorf("role %q can %+v; want %q %+v", v.Role, v.Can, want.role, want.can)
			}
		})
	}

	t.Run("the author may not dismiss blocking findings", func(t *testing.T) {
		e := newEnv(t, nil)
		e.addRun(runFix{project: buttonProject, number: 7, head: head('1'), user: "alice", result: cleanResult(head('1')),
			outcome: OutcomeApproved})
		c := e.signInRole(store.RoleMaintainer)
		v := getJSON[ReviewDetailView](e, c, "/api/reviews/main/"+buttonProject+"/7")
		if !v.Can.Rerun || v.Can.DismissBlocking || !v.Can.DismissSuggestion || !v.Can.Restore {
			t.Errorf("can = %+v; want everything but dismissing blocking findings", v.Can)
		}
	})

	t.Run("a password admin", func(t *testing.T) {
		e := newEnv(t, nil)
		e.buttonBlockingReview()
		v := getJSON[ReviewDetailView](e, e.signInOperator(), "/api/reviews/main/"+buttonProject+"/7")
		if v.Role != "admin" || !v.Can.DismissBlocking {
			t.Errorf("role %q can %+v", v.Role, v.Can)
		}
	})

	t.Run("dismissals", func(t *testing.T) {
		e := newEnv(t, nil)
		runID := e.buttonBlockingReview()
		c := e.signInRole(store.RoleMaintainer)
		run := getJSON[RunDetailView](e, c, "/api/runs/"+runID)
		sk := run.Skills[0]
		if sk.Verdict != "has_blocking_issues" || len(sk.Findings) != 2 || len(sk.Dismissed) != 0 {
			t.Fatalf("skill before = %+v", sk)
		}
		if sk.Findings[0].Fingerprint != blockingFP || sk.Findings[0].Dismissed != nil || sk.Findings[1].Fingerprint != suggestionFP {
			t.Errorf("findings = %+v; want fingerprints and no dismissal", sk.Findings)
		}

		wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"constant"}`), "dismiss", 200)
		run = getJSON[RunDetailView](e, c, "/api/runs/"+runID)
		sk = run.Skills[0]
		// Only the suggestion is left, so nothing blocks any more.
		if sk.Verdict != "no_blocking_issues" || len(sk.Findings) != 1 || sk.Findings[0].Fingerprint != suggestionFP {
			t.Errorf("skill after = %+v", sk)
		}
		if len(sk.Dismissed) != 1 {
			t.Fatalf("dismissed = %+v", sk.Dismissed)
		}
		d := sk.Dismissed[0]
		if d.Fingerprint != blockingFP || d.ID != "a1b2c3" || d.Dismissed == nil || d.Dismissed.By != "alice (gitlab main)" ||
			d.Dismissed.Reason != "constant" || d.Dismissed.At.IsZero() {
			t.Errorf("dismissed finding = %+v", d)
		}
		if other := run.Skills[1]; other.Verdict != "no_blocking_issues" || len(other.Dismissed) != 0 {
			t.Errorf("the other skill = %+v", other)
		}

		// A dismissed suggestion does not cover the same finding at blocking.
		e2 := newEnv(t, nil)
		runID = e2.buttonBlockingReview()
		c2 := e2.signInRole(store.RoleMaintainer)
		wantCode(t, e2.send(c2, "POST", buttonURL("dismiss"), `{"fingerprint":"`+suggestionFP+`"}`), "dismiss", 200)
		run = getJSON[RunDetailView](e2, c2, "/api/runs/"+runID)
		if sk := run.Skills[0]; sk.Verdict != "has_blocking_issues" || len(sk.Findings) != 1 || len(sk.Dismissed) != 1 ||
			sk.Dismissed[0].Fingerprint != suggestionFP {
			t.Errorf("skill = %+v", sk)
		}
	})
}

func TestButtonsCSRF(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	c := e.signInRole(store.RoleAdmin)
	_, me := e.me(c)

	for _, w := range []struct{ action, body string }{
		{"rerun", `{}`}, {"dismiss", `{"fingerprint":"` + suggestionFP + `"}`}, {"restore", `{"fingerprint":"` + suggestionFP + `"}`},
	} {
		for name, header := range map[string]map[string]string{
			"no token":    nil,
			"wrong token": {CSRFHeader: me.CSRFToken + "x"},
		} {
			req := httptest.NewRequest("POST", buttonURL(w.action), strings.NewReader(w.body))
			req.AddCookie(c)
			for k, v := range header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"csrf"`) {
				t.Errorf("%s with %s = %d %s; want 403 csrf", w.action, name, rec.Code, rec.Body)
			}
		}
		// No session: 401.
		req := httptest.NewRequest("POST", buttonURL(w.action), strings.NewReader(w.body))
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d; want 401", w.action, rec.Code)
		}
	}
	if n := len(e.runRows()); n != 1 || len(e.dismissalsOf()) != 0 || e.wakes.Load() != 0 {
		t.Errorf("runs %d, dismissals %d, wakes %d; want nothing done", n, len(e.dismissalsOf()), e.wakes.Load())
	}
}

// failRepublish makes every republishing of a run (published -> reported) fail
// while the returned switch is armed; disarm it with e.exec.
func (e *env) failRepublish() {
	e.t.Helper()
	e.exec(`CREATE TABLE fail_republish (armed boolean)`)
	e.exec(`INSERT INTO fail_republish VALUES (true)`)
	e.exec(`CREATE FUNCTION fail_republish_fn() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF (SELECT armed FROM fail_republish) THEN RAISE EXCEPTION 'republish refused'; END IF;
			RETURN NEW;
		END $$`)
	e.exec(`CREATE TRIGGER fail_republish_trg BEFORE UPDATE ON runs FOR EACH ROW
		WHEN (OLD.state = 'published' AND NEW.state = 'reported') EXECUTE FUNCTION fail_republish_fn()`)
}

func (e *env) stateOf(id string) string {
	e.t.Helper()
	var s string
	if err := e.db.QueryRow(ctx, `SELECT state FROM runs WHERE id = $1::uuid`, id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// A dismissal whose review could not be decided again is decided again when
// the button is pressed once more, the dismissal being there already.
func TestDismissRetryRedecides(t *testing.T) {
	e := newEnv(t, nil)
	runID := e.buttonBlockingReview()
	c := e.signInRole(store.RoleMaintainer)
	e.failRepublish()

	body := `{"fingerprint":"` + blockingFP + `","reason":"constant"}`
	wantCode(t, e.send(c, "POST", buttonURL("dismiss"), body), "dismiss while the store refuses", http.StatusInternalServerError)
	if len(e.dismissalsOf()) != 1 || e.stateOf(runID) != "published" || e.wakes.Load() != 0 {
		t.Fatalf("dismissals %d, state %s, wakes %d; want the dismissal stored and the review not decided again",
			len(e.dismissalsOf()), e.stateOf(runID), e.wakes.Load())
	}

	e.exec(`UPDATE fail_republish SET armed = false`)
	rec := e.send(c, "POST", buttonURL("dismiss"), body)
	wantCode(t, rec, "dismiss again", http.StatusOK)
	if got := statusOf(t, rec); got != "already dismissed" {
		t.Errorf("status = %q; want already dismissed", got)
	}
	if got := e.stateOf(runID); got != "reported" {
		t.Errorf("run state = %q; want reported (decided again)", got)
	}
	if e.wakes.Load() != 1 || len(e.auditOf("finding_dismissed")) != 1 {
		t.Errorf("wakes %d, audit %d; want 1 and 1", e.wakes.Load(), len(e.auditOf("finding_dismissed")))
	}
}

func TestRestoreRetryRedecides(t *testing.T) {
	e := newEnv(t, nil)
	runID := e.buttonBlockingReview()
	mr := store.MRKey{Instance: "main", ProjectID: testID(buttonProject), IID: 7}
	if _, err := e.st.Dismiss(ctx, store.Dismissal{MR: mr, Fingerprint: blockingFP, Severity: "blocking", Reason: "x", Actor: "bob"}); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE runs SET approved = true WHERE id = $1::uuid`, runID)
	c := e.signInRole(store.RoleMaintainer)
	e.failRepublish()

	body := `{"fingerprint":"` + blockingFP + `"}`
	wantCode(t, e.send(c, "POST", buttonURL("restore"), body), "restore while the store refuses", http.StatusInternalServerError)
	if len(e.dismissalsOf()) != 0 || e.stateOf(runID) != "published" {
		t.Fatalf("dismissals %d, state %s; want the dismissal gone and the review not decided again",
			len(e.dismissalsOf()), e.stateOf(runID))
	}

	e.exec(`UPDATE fail_republish SET armed = false`)
	rec := e.send(c, "POST", buttonURL("restore"), body)
	wantCode(t, rec, "restore again", http.StatusOK)
	if got := statusOf(t, rec); got != "not dismissed" {
		t.Errorf("status = %q; want not dismissed", got)
	}
	if got := e.stateOf(runID); got != "reported" {
		t.Errorf("run state = %q; want reported (decided again, so the approval is withdrawn)", got)
	}
	if e.wakes.Load() != 1 || len(e.auditOf("finding_restored")) != 1 {
		t.Errorf("wakes %d, audit %d; want 1 and 1", e.wakes.Load(), len(e.auditOf("finding_restored")))
	}
}

// A request that ends while the restore is being made (the browser went away)
// still leaves it audited and the review decided again.
func TestRestoreSurvivesAnInterruptedRequest(t *testing.T) {
	e := newEnv(t, nil)
	runID := e.buttonBlockingReview()
	mr := store.MRKey{Instance: "main", ProjectID: testID(buttonProject), IID: 7}
	if _, err := e.st.Dismiss(ctx, store.Dismissal{MR: mr, Fingerprint: blockingFP, Severity: "blocking", Reason: "x", Actor: "bob"}); err != nil {
		t.Fatal(err)
	}
	c := e.signInRole(store.RoleMaintainer)
	_, me := e.me(c)
	e.exec(`CREATE FUNCTION slow_restore() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(1.5); RETURN OLD; END $$`)
	e.exec(`CREATE TRIGGER slow_restore_trg BEFORE DELETE ON dismissals FOR EACH ROW EXECUTE FUNCTION slow_restore()`)

	rctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("POST", buttonURL("restore"), strings.NewReader(`{"fingerprint":"`+blockingFP+`"}`)).WithContext(rctx)
	req.AddCookie(c)
	req.Header.Set(CSRFHeader, me.CSRFToken)
	e.h.ServeHTTP(httptest.NewRecorder(), req)

	if len(e.dismissalsOf()) != 0 {
		t.Fatal("the dismissal is still there")
	}
	if got := e.stateOf(runID); got != "reported" {
		t.Errorf("run state = %q; want reported (decided again)", got)
	}
	if n := len(e.auditOf("finding_restored")); n != 1 {
		t.Errorf("%d audit entries; want 1", n)
	}
	if e.wakes.Load() != 1 {
		t.Errorf("Wake called %d times; want 1", e.wakes.Load())
	}
}

// A restore while the run of a new head is being published (its dismissals
// are already loaded) has that run decided again; the older published review
// is left as it was.
func TestRestoreReachesAReviewBeingPublished(t *testing.T) {
	e := newEnv(t, nil)
	older := e.buttonBlockingReview()
	mr := store.MRKey{Instance: "main", ProjectID: testID(buttonProject), IID: 7}
	if _, err := e.st.Dismiss(ctx, store.Dismissal{MR: mr, Fingerprint: blockingFP, Severity: "blocking", Reason: "x", Actor: "bob"}); err != nil {
		t.Fatal(err)
	}
	newer := e.addRun(runFix{project: buttonProject, number: 7, head: head('2'), user: "bob", state: store.Reported,
		at:     e.clock.now().Add(-30 * time.Minute),
		result: resultJSON(head('2'), buttonSkill("code-review"))})
	c := e.signInRole(store.RoleMaintainer)

	wantCode(t, e.send(c, "POST", buttonURL("restore"), `{"fingerprint":"`+blockingFP+`"}`), "restore", http.StatusOK)
	rows := map[string]runRow{}
	for _, r := range e.runRows() {
		rows[r.ID] = r
	}
	if !rows[newer].Redecide || rows[newer].State != "reported" {
		t.Errorf("newest run = %+v; want reported and flagged to be decided again", rows[newer])
	}
	if rows[older].Redecide || rows[older].State != "published" {
		t.Errorf("older run = %+v; want untouched", rows[older])
	}
}

// The same with a re-run of the head that was published: no run is decided
// yet, and the run being published still has to be decided again.
func TestRestoreReachesASameHeadRerunBeingPublished(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	mr := store.MRKey{Instance: "main", ProjectID: testID(buttonProject), IID: 7}
	if _, err := e.st.Dismiss(ctx, store.Dismissal{MR: mr, Fingerprint: blockingFP, Severity: "blocking", Reason: "x", Actor: "bob"}); err != nil {
		t.Fatal(err)
	}
	rerun, created, err := e.st.EnqueueRerun(ctx, mr, buttonProject, head('1'), "main", store.TriggerRerun, nil, nil, false)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}
	e.exec(`UPDATE runs SET state = 'reported', result = $2::jsonb WHERE id = $1::uuid`, rerun,
		resultJSON(head('1'), buttonSkill("code-review")))
	c := e.signInRole(store.RoleMaintainer)

	wantCode(t, e.send(c, "POST", buttonURL("restore"), `{"fingerprint":"`+blockingFP+`"}`), "restore", http.StatusOK)
	var got runRow
	for _, r := range e.runRows() {
		if r.ID == rerun {
			got = r
		}
	}
	if !got.Redecide || got.State != "reported" {
		t.Errorf("re-run = %+v; want reported and flagged to be decided again", got)
	}
}

// Dismissals are decided again for the newest review only, so an older run
// shows what was published for it.
func TestOlderRunsShowWhatWasPublished(t *testing.T) {
	e := newEnv(t, nil)
	older := e.buttonBlockingReview()
	newer := e.addRun(runFix{project: buttonProject, number: 7, head: head('2'), user: "bob", outcome: OutcomeBlocking,
		at:     e.clock.now().Add(-30 * time.Minute),
		result: resultJSON(head('2'), buttonSkill("code-review"), skillJSON("style", "no_blocking_issues"))})
	c := e.signInRole(store.RoleMaintainer)
	wantCode(t, e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"constant"}`), "dismiss", 200)

	latest := getJSON[RunDetailView](e, c, "/api/runs/"+newer)
	if sk := latest.Skills[0]; sk.Verdict != "no_blocking_issues" || len(sk.Dismissed) != 1 || len(sk.Findings) != 1 {
		t.Errorf("newest run skill = %+v; want the dismissal applied", sk)
	}
	old := getJSON[RunDetailView](e, c, "/api/runs/"+older)
	if sk := old.Skills[0]; sk.Verdict != "has_blocking_issues" || len(sk.Dismissed) != 0 || len(sk.Findings) != 2 {
		t.Errorf("older run skill = %+v; want what was published", sk)
	}
}

func TestDismissBlockingOnAMergedChange(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	e.src["main"].set(func(f *fakeSource) { f.change.State = forge.StateMerged })
	c := e.signInRole(store.RoleMaintainer)
	rec := e.send(c, "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"x"}`)
	wantCode(t, rec, "dismiss on a merged change", http.StatusConflict)
	if got := errorOf(t, rec); got != "this change is merged" {
		t.Errorf("error = %q", got)
	}
	if len(e.dismissalsOf()) != 0 {
		t.Error("a finding of a merged change was dismissed")
	}
}

// A merged or closed change refuses every blocking dismissal, a password
// admin's included.
func TestOperatorDismissBlockingOnAMergedChange(t *testing.T) {
	for _, state := range []forge.ChangeState{forge.StateMerged, forge.StateClosed} {
		e := newEnv(t, nil)
		e.buttonBlockingReview()
		e.src["main"].set(func(f *fakeSource) { f.change.State = state })
		rec := e.send(e.signInOperator(), "POST", buttonURL("dismiss"), `{"fingerprint":"`+blockingFP+`","reason":"x"}`)
		wantCode(t, rec, "operator dismiss on a "+string(state)+" change", http.StatusConflict)
		if got := errorOf(t, rec); got != "this change is "+string(state) {
			t.Errorf("error = %q", got)
		}
		if len(e.dismissalsOf()) != 0 {
			t.Errorf("an operator dismissed a finding of a %s change", state)
		}
	}
}

// TestRunDetailLatestDecided: latestDecided marks the one run of a change
// that dismiss and restore act on, whatever its place in the history.
func TestRunDetailLatestDecided(t *testing.T) {
	e := newEnv(t, nil)
	// buttonReview's run is an hour old; the others come after it.
	ago := func(minutes int) time.Time { return e.clock.now().Add(-time.Duration(minutes) * time.Minute) }
	res := resultJSON(head('1'), buttonSkill("code-review"))
	decided := e.buttonReview(res)
	c := e.signInRole(store.RoleMaintainer)
	latest := func(id string) bool { return getJSON[RunDetailView](e, c, "/api/runs/"+id).LatestDecided }

	if !latest(decided) {
		t.Error("the only decided run is not marked")
	}
	// A run in progress (a new head's, say) is newer, but not decided.
	running := e.addRun(runFix{project: buttonProject, number: 7, head: head('2'), state: store.Running, at: ago(50)})
	if latest(running) || !latest(decided) {
		t.Errorf("with a run in progress: running %v, decided %v; want false, true", latest(running), latest(decided))
	}
	// A run whose publication failed is not decided either.
	failed := e.addRun(runFix{project: buttonProject, number: 7, head: head('3'), state: store.PublishFailed, result: res,
		at: ago(40)})
	if latest(failed) || !latest(decided) {
		t.Errorf("with a failed publication: failed %v, decided %v; want false, true", latest(failed), latest(decided))
	}
	// Reported with no outcome: not decided yet.
	reported := e.addRun(runFix{project: buttonProject, number: 7, head: head('4'), state: store.Reported, result: res,
		at: ago(30)})
	if latest(reported) || !latest(decided) {
		t.Errorf("with an undecided report: reported %v, decided %v; want false, true", latest(reported), latest(decided))
	}
	// A newer decided run takes over.
	newer := e.addRun(runFix{project: buttonProject, number: 7, head: head('5'), result: res, user: "bob",
		outcome: OutcomeBlocking, at: ago(20)})
	if !latest(newer) || latest(decided) {
		t.Errorf("with a newer decided run: newer %v, older %v; want true, false", latest(newer), latest(decided))
	}
}
