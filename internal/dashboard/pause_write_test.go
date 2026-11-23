package dashboard

import (
	"net/http"
	"slices"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// auditActionsOf is the actions of the audit log about project, oldest first.
func (e *env) auditActionsOf(project string) []string {
	e.t.Helper()
	var out []string
	for _, a := range e.auditEntries() {
		if a.Project == project {
			out = append(out, a.Action)
		}
	}
	slices.Reverse(out)
	return out
}

func enabledBody(on bool) string {
	if on {
		return `{"enabled": true}`
	}
	return `{"enabled": false}`
}

// The switch pauses a repository and resumes it, its settings kept, each
// with its own audit entry and a reconcile of that repository.
func TestPauseAndResume(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) { r.Approve = ptr(false) })
	c := e.userOnMain(true)

	rec := e.send(c, "PUT", "/api/admin/repositories/main/acme/gadgets", enabledBody(false))
	detail := decode[RepositoryDetailView](t, rec, "pause", http.StatusOK)
	if detail.Repo.On || !detail.Repo.Paused || detail.Settings["approve"].Value != false || !detail.Settings["approve"].Overridden {
		t.Errorf("paused detail = %+v %+v", detail.Repo, detail.Settings["approve"])
	}
	if r, found := e.storedRepo("main", "acme/gadgets"); !found || r.Enabled {
		t.Errorf("row after the pause = %+v, %v; want paused", r, found)
	}
	if n := e.reconciles.Load(); n != 1 {
		t.Errorf("the pause asked for %d reconciles, want 1", n)
	}

	rec = e.send(c, "PUT", "/api/admin/repositories/main/acme/gadgets", enabledBody(true))
	detail = decode[RepositoryDetailView](t, rec, "resume", http.StatusOK)
	if !detail.Repo.On || detail.Repo.Paused || detail.Settings["approve"].Value != false {
		t.Errorf("resumed detail = %+v %+v", detail.Repo, detail.Settings["approve"])
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); !r.Enabled || r.Approve == nil || *r.Approve {
		t.Errorf("row after the resume = %+v; want on with approve off", r)
	}
	if got := e.auditActionsOf("acme/gadgets"); !slices.Equal(got, []string{"repository_paused", "repository_resumed"}) {
		t.Errorf("audit = %v", got)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if want := []string{"main/acme/gadgets", "main/acme/gadgets"}; !slices.Equal(e.reconciled, want) {
		t.Errorf("reconciled %v, want %v", e.reconciled, want)
	}
}

// The path is matched ignoring case; the reconcile names the stored spelling.
func TestPauseResumeIgnoresCase(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true)
	wantCode(t, e.send(c, "PUT", "/api/admin/repositories/main/ACME/Gadgets", enabledBody(false)), "pause", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.Enabled || r.Project != "acme/gadgets" {
		t.Errorf("row = %+v; want paused, spelled as stored", r)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !slices.Equal(e.reconciled, []string{"main/acme/gadgets"}) {
		t.Errorf("reconciled %v", e.reconciled)
	}
}

// The same state again changes nothing: no audit entry, no reconcile.
func TestPauseTwiceIsQuiet(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true)
	wantCode(t, e.send(c, "PUT", "/api/admin/repositories/main/acme/gadgets", enabledBody(true)), "resume a running one", http.StatusOK)
	wantCode(t, e.send(c, "PUT", "/api/admin/repositories/main/acme/gadgets", enabledBody(false)), "pause", http.StatusOK)
	wantCode(t, e.send(c, "PUT", "/api/admin/repositories/main/acme/gadgets", enabledBody(false)), "pause again", http.StatusOK)
	if got := e.auditActionsOf("acme/gadgets"); !slices.Equal(got, []string{"repository_paused"}) {
		t.Errorf("audit = %v; want one pause", got)
	}
	if n := e.reconciles.Load(); n != 1 {
		t.Errorf("%d reconciles, want 1", n)
	}
}

// Bad bodies are 400; a config repository is 409; one that isn't there, or
// that the user can't see, 404; a resume while the instance is being removed
// is 409.
func TestPauseRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.userOnMain(true)
	for _, body := range []string{`{}`, `{"enabled": "no"}`, `{"enabled": true, "x": 1}`, `{"enabled": null}`, `nope`} {
		rec := e.send(c, "PUT", "/api/admin/repositories/main/acme/gadgets", body)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgBadEnabled {
			t.Errorf("PUT %s = %d %s; want 400 %q", body, rec.Code, rec.Body, msgBadEnabled)
		}
	}
	rec := e.send(c, "PUT", "/api/admin/repositories/main/acme/widgets", enabledBody(false))
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgConfigManaged {
		t.Errorf("pause of a config repository = %d %s", rec.Code, rec.Body)
	}
	wantCode(t, e.send(c, "PUT", "/api/admin/repositories/main/acme/missing", enabledBody(false)), "pause a missing one", http.StatusNotFound)
	wantCode(t, e.send(c, "PUT", "/api/admin/repositories/main", enabledBody(false)), "the instance alone", http.StatusNotFound)

	// An instance being removed: pause is allowed, resume is not.
	e.addLab(e.operator())
	e.dashboardRepo("lab", "acme/lab-repo", nil)
	e.reg.SetRemoving("lab")
	wantCode(t, e.send(c, "PUT", "/api/admin/repositories/lab/acme/lab-repo", enabledBody(false)), "pause while removing", http.StatusOK)
	rec = e.send(c, "PUT", "/api/admin/repositories/lab/acme/lab-repo", enabledBody(true))
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgInstanceRemoving {
		t.Errorf("resume while removing = %d %s; want 409 %q", rec.Code, rec.Body, msgInstanceRemoving)
	}
	if r, _ := e.storedRepo("lab", "acme/lab-repo"); r.Enabled {
		t.Error("the repository was resumed on an instance being removed")
	}
}

// POST /api/repositories on a paused repository resumes it with its
// settings: 200, not 201. Then it is on, so a second POST is 409.
func TestSwitchOnResumesPausedRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets")
	e.pausedRepo("main", "acme/gadgets", func(r *store.DashboardRepo) { r.Approve = ptr(false) })
	c := e.userOnMain(true)

	rec := e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/gadgets"}`)
	detail := decode[RepositoryDetailView](t, rec, "POST on a paused repository", http.StatusOK)
	if !detail.Repo.On || detail.Repo.Paused || detail.Settings["approve"].Value != false {
		t.Errorf("detail = %+v %+v", detail.Repo, detail.Settings["approve"])
	}
	if got := e.auditActionsOf("acme/gadgets"); !slices.Equal(got, []string{"repository_resumed"}) {
		t.Errorf("audit = %v", got)
	}
	if n := e.reconciles.Load(); n != 1 {
		t.Errorf("%d reconciles, want 1", n)
	}
	rec = e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/gadgets"}`)
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgAlreadyOn {
		t.Errorf("POST again = %d %s", rec.Code, rec.Body)
	}

	// One the bot no longer reaches stays paused.
	e.pausedRepo("main", "acme/gone", nil)
	wantCode(t, e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/gone"}`), "POST unreachable", http.StatusNotFound)
	if r, _ := e.storedRepo("main", "acme/gone"); r.Enabled {
		t.Error("an unreachable paused repository was resumed")
	}
}

// DELETE removes a repository, running or paused, with its team links.
func TestRemoveRepo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.pausedRepo("main", "acme/tools", nil)
	e.team("devs", nil, store.TeamRepo{Instance: "main", Project: "acme/gadgets"}, store.TeamRepo{Instance: "main", Project: "acme/tools"},
		store.TeamRepo{Instance: "main"})
	c := e.userOnMain(true)

	for i, p := range []string{"acme/gadgets", "ACME/Tools"} {
		rec := e.send(c, "DELETE", "/api/repositories/main/"+p, "")
		detail := decode[RepositoryDetailView](t, rec, "DELETE "+p, http.StatusOK)
		if detail.Repo.On || detail.Repo.Paused {
			t.Errorf("detail after DELETE %s = %+v", p, detail.Repo)
		}
		if _, found := e.storedRepo("main", p); found {
			t.Errorf("%s is still stored", p)
		}
		if n := e.reconciles.Load(); n != int32(i+1) {
			t.Errorf("after DELETE %s: %d reconciles", p, n)
		}
	}
	teams, err := e.st.Teams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tm := range teams {
		if tm.Name == "devs" && !slices.Equal(tm.Repos, []store.TeamRepo{{Instance: "main"}}) {
			t.Errorf("devs covers %+v; want the whole instance alone", tm.Repos)
		}
	}
	if got := e.auditActionsOf("acme/gadgets"); !slices.Equal(got, []string{"repository_removed"}) {
		t.Errorf("audit of gadgets = %v", got)
	}
	rec := e.send(c, "DELETE", "/api/repositories/main/acme/widgets", "")
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgConfigManaged {
		t.Errorf("DELETE of a config repository = %d %s", rec.Code, rec.Body)
	}
}
