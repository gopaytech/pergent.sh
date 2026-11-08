package dashboard

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// UserByName answers from the users a test set.
func (f *fakeSource) UserByName(_ context.Context, _, username string) (forge.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.userErr != nil {
		return forge.User{}, f.userErr
	}
	u, ok := f.users[strings.ToLower(username)]
	if !ok {
		return forge.User{}, forge.ErrNotFound
	}
	return u, nil
}

// A waiting run says what it waits for.
func TestJudgeWaiting(t *testing.T) {
	for reason, want := range map[string]string{
		store.WaitUntrusted:   "Waiting for someone with write access to start the review.",
		store.WaitCheckFailed: "Couldn't check the author's access. Someone with write access can start the review.",
	} {
		got := judge(store.RunDigest{Outcome: OutcomeWaiting, State: store.Waiting, SkipReason: reason})
		if got.outcome != OutcomeWaiting || got.reason != want || got.status != "" {
			t.Errorf("%s: judge = %+v, want waiting / %q", reason, got, want)
		}
	}
}

const gadgetsSettings = "/api/repositories/main/acme/gadgets/settings"

// A name is resolved to its ID when it is saved; an entry with an ID is kept
// as sent; an unknown name saves nothing.
func TestTrustedAuthorsSave(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.src["main"].set(func(f *fakeSource) { f.users = map[string]forge.User{"alice": {ID: 5, Username: "Alice"}} })
	c := e.sessionFor(e.ssoAdmin("boss@example.com"))

	rec := e.send(c, "PUT", gadgetsSettings, `{"trustedAuthors":[{"username":"alice"},{"id":9,"username":"old-name"}]}`)
	wantCode(t, rec, "save", http.StatusOK)
	want := []store.TrustedAuthor{{ID: 5, Username: "Alice"}, {ID: 9, Username: "old-name"}}
	if r, _ := e.storedRepo("main", "acme/gadgets"); !slices.Equal(r.TrustedAuthors, want) {
		t.Errorf("stored = %+v, want %+v", r.TrustedAuthors, want)
	}

	rec = e.send(c, "PUT", gadgetsSettings, `{"trustedAuthors":[{"username":"ghost"}]}`)
	wantCode(t, rec, "unknown name", http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "ghost") {
		t.Errorf("400 body %s doesn't name ghost", rec.Body)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); !slices.Equal(r.TrustedAuthors, want) {
		t.Errorf("an unknown name changed the list: %+v", r.TrustedAuthors)
	}

	e.src["main"].set(func(f *fakeSource) { f.userErr = errors.New("gitlab: status 500") })
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"trustedAuthors":[{"username":"alice"}]}`), "platform error", http.StatusBadGateway)
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"trustedAuthors":[{"id":-1,"username":"x"}]}`), "bad id", http.StatusBadRequest)
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"trustedAuthors":[{"username":""}]}`), "empty name", http.StatusBadRequest)

	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"trustedAuthors":null}`), "clear", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.TrustedAuthors != nil {
		t.Errorf("cleared list = %+v", r.TrustedAuthors)
	}
}

// The repository's mode is on, off or null (the instance's), and the
// settings show the resolved mode.
func TestTrustedAuthorsOnlySetting(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.sessionFor(e.ssoAdmin("boss@example.com"))
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"trustedAuthorsOnly":"maybe"}`), "bad mode", http.StatusBadRequest)
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"trustedAuthorsOnly":"follow_visibility"}`), "repository can't follow", http.StatusBadRequest)

	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"trustedAuthorsOnly":"off"}`), "off", http.StatusOK)
	v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets")
	if s := v.Settings["trustedAuthorsOnly"]; s.Value != "off" || !s.Overridden {
		t.Errorf("setting = %+v, want off, overridden", s)
	}
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"trustedAuthorsOnly":null}`), "reset", http.StatusOK)
	v = getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/gadgets")
	if s := v.Settings["trustedAuthorsOnly"]; s.Value != "follow_visibility" || s.Overridden {
		t.Errorf("setting = %+v, want follow_visibility, not overridden", s)
	}
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/widgets/settings", `{"trustedAuthorsOnly":"off"}`), "config repository", http.StatusConflict)
}

// An admin sets a dashboard instance's mode; a config instance is read-only.
func TestInstanceTrustMode(t *testing.T) {
	e := newEnv(t, nil)
	if err := e.st.CreateInstance(ctx, store.InstanceRow{Name: "lab", Platform: "gitlab", URL: "https://lab.example.com"}, nil, e.clock.now()); err != nil {
		t.Fatal(err)
	}
	if err := e.reg.Load(ctx); err != nil {
		t.Fatal(err)
	}
	boss := e.sessionFor(e.ssoAdmin("boss@example.com"))
	wantCode(t, e.send(boss, "PUT", "/api/admin/instances/lab/trusted-authors", `{"mode":"off"}`), "set off", http.StatusNoContent)
	var lab InstanceAdminView
	for _, v := range getJSON[[]InstanceAdminView](e, boss, "/api/admin/instances") {
		if v.Name == "lab" {
			lab = v
		}
	}
	if lab.TrustedAuthorsOnly != "off" {
		t.Errorf("lab = %q, want off", lab.TrustedAuthorsOnly)
	}
	if inst, _ := e.reg.Get("lab"); inst.TrustedAuthorsOnly != "off" {
		t.Errorf("registry = %q, want off", inst.TrustedAuthorsOnly)
	}
	if a := e.auditOf("instance_updated"); len(a) != 1 {
		t.Errorf("audit = %+v", a)
	}
	wantCode(t, e.send(boss, "PUT", "/api/admin/instances/lab/trusted-authors", `{"mode":"maybe"}`), "bad mode", http.StatusBadRequest)
	wantCode(t, e.send(boss, "PUT", "/api/admin/instances/main/trusted-authors", `{"mode":"off"}`), "config instance", http.StatusConflict)
	wantCode(t, e.send(boss, "PUT", "/api/admin/instances/nope/trusted-authors", `{"mode":"off"}`), "unknown", http.StatusNotFound)
	wantCode(t, e.send(e.aliceSession(), "PUT", "/api/admin/instances/lab/trusted-authors", `{"mode":"on"}`), "not an admin", http.StatusNotFound)
}

// waitingChange stores acme/widgets!7 at head as waiting, by bob.
func (e *env) waitingChange(h, reason string) string {
	e.t.Helper()
	e.src["main"].set(func(f *fakeSource) {
		f.change = forge.Change{State: forge.StateOpen, HeadSHA: h, TargetBranch: "main", Author: "bob", AuthorID: bobID}
	})
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID(buttonProject), Path: buttonProject}); err != nil {
		e.t.Fatal(err)
	}
	id, _, err := e.st.EnqueueRun(ctx, store.MRKey{Instance: "main", ProjectID: testID(buttonProject), IID: 7}, buttonProject, h, "main")
	if err != nil {
		e.t.Fatal(err)
	}
	if ok, err := e.st.HoldRun(ctx, id, reason); err != nil || !ok {
		e.t.Fatalf("HoldRun = %v, %v", ok, err)
	}
	if _, err := e.db.Exec(ctx, `UPDATE runs SET title = 'Add the handler', author = 'bob', web_url = 'https://gitlab.example.com/acme/widgets/-/merge_requests/7' WHERE id = $1::uuid`, id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// The repository page lists the changes waiting for a start.
func TestWaitingList(t *testing.T) {
	e := newEnv(t, nil)
	e.waitingChange(head('1'), store.WaitCheckFailed)
	c := e.signInRole(store.RoleViewer)
	v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/widgets")
	if len(v.Waiting) != 1 {
		t.Fatalf("waiting = %+v, want one", v.Waiting)
	}
	w := v.Waiting[0]
	if w.Number != 7 || w.Title != "Add the handler" || w.Author != "bob" || w.Reason != store.WaitCheckFailed || w.WebURL == "" || w.Head != head('1') {
		t.Errorf("waiting = %+v", w)
	}

	// Once it is started it isn't waiting.
	if _, _, err := e.st.StartRun(ctx, store.MRKey{Instance: "main", ProjectID: testID(buttonProject), IID: 7}, buttonProject, head('1'), "main"); err != nil {
		t.Fatal(err)
	}
	if v := getJSON[RepositoryDetailView](e, c, "/api/repositories/main/acme/widgets"); len(v.Waiting) != 0 {
		t.Errorf("waiting after a start = %+v", v.Waiting)
	}
}

// Start review: a maintainer starts a waiting change; a viewer can't; a
// change that isn't waiting is 409.
func TestStartButton(t *testing.T) {
	e := newEnv(t, nil)
	id := e.waitingChange(head('1'), store.WaitUntrusted)
	viewer := e.signInRole(store.RoleViewer)
	body := `{"head":"` + head('1') + `"}`
	wantCode(t, e.send(viewer, "POST", buttonURL("start"), body), "viewer", http.StatusForbidden)

	e.aliceOn("main", store.RoleMaintainer)
	c := e.aliceSession()
	wantCode(t, e.send(c, "POST", buttonURL("start"), `{}`), "no head", http.StatusBadRequest)
	// The change has a newer push than the page showed: nothing starts.
	wantCode(t, e.send(c, "POST", buttonURL("start"), `{"head":"`+head('2')+`"}`), "head moved", http.StatusConflict)
	if rows := e.runRows(); len(rows) != 1 || rows[0].State != "waiting" {
		t.Fatalf("after a moved head: runs = %+v, want still waiting", rows)
	}
	rec := e.send(c, "POST", buttonURL("start"), body)
	if rec.Code != http.StatusAccepted || statusOf(t, rec) != "queued" {
		t.Fatalf("start = %d %s; want 202 queued", rec.Code, rec.Body)
	}
	rows := e.runRows()
	if len(rows) != 1 || rows[0].ID != id || rows[0].State != "queued" || rows[0].Trigger != store.TriggerStart {
		t.Errorf("runs = %+v; want the waiting run queued as a start", rows)
	}
	if a := e.auditOf("review_start"); len(a) != 1 {
		t.Errorf("audit = %+v", a)
	}
	if e.wakes.Load() == 0 {
		t.Error("the engine wasn't woken")
	}
	wantCode(t, e.send(c, "POST", buttonURL("start"), body), "not waiting", http.StatusConflict)
}

// Who is trusted is an admin's to say: a maintainer can't change it, and a
// maintainer's reset leaves it as it is.
func TestTrustSettingsAreAdmins(t *testing.T) {
	e := newEnv(t, nil)
	off := "off"
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) {
		r.TrustedAuthorsOnly, r.TrustedAuthors = &off, []store.TrustedAuthor{{ID: 9, Username: "bob"}}
	})
	c := e.signInRole(store.RoleMaintainer)
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"trustedAuthorsOnly":"on"}`), "maintainer mode", http.StatusForbidden)
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"trustedAuthors":null}`), "maintainer list", http.StatusForbidden)
	wantCode(t, e.send(c, "POST", gadgetsSettings+"/reset", ""), "maintainer reset", http.StatusOK)
	r, _ := e.storedRepo("main", "acme/gadgets")
	if r.TrustedAuthorsOnly == nil || *r.TrustedAuthorsOnly != "off" || len(r.TrustedAuthors) != 1 {
		t.Errorf("after a maintainer's reset: %v %+v; want them kept", r.TrustedAuthorsOnly, r.TrustedAuthors)
	}
}
