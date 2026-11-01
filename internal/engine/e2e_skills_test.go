package engine

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/skillfetch"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The skill source of the skills end-to-end test: a project on the fake
// GitLab that holds one skill, lint, in its skills folder.
const (
	skillSourceID   = int64(21)
	skillSourcePath = "acme/skill-library"
	skillFilePath   = "skills/lint.md"

	lintV1 = "---\nname: lint\n---\nLint v1: check the lint rules.\n"
	lintV2 = "---\nname: lint\n---\nLint v2: check the lint rules and the naming.\n"
)

// skillSource is the fixture repository: the commit its branch points at,
// and the text of lint at each commit.
type skillSource struct {
	mu     sync.Mutex
	head   string
	files  map[string]string // commit -> text of skills/lint.md
	served []string          // commits whose tree or file was read
}

// moveTo points the branch at commit.
func (s *skillSource) moveTo(commit string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.head = commit
}

// serve makes the fake GitLab answer for the fixture: the project, the
// commit of main, the skills folder and the file, read with the review bot.
func (s *skillSource) serve(gl *fakeGitLab) {
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	// seen records the commit a read was made at.
	seen := func(commit string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.served = append(s.served, commit)
	}
	text := func(commit string) (string, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		t, ok := s.files[commit]
		return t, ok
	}
	base := "/projects/" + url.PathEscape(skillSourcePath)
	gl.Handle(http.MethodGet, base, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != testReviewToken {
			reply(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
			return
		}
		reply(w, http.StatusOK, map[string]any{"id": skillSourceID, "path_with_namespace": skillSourcePath})
	})
	id := "/projects/21/repository"
	gl.Handle(http.MethodGet, id+"/commits/main", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		head := s.head
		s.mu.Unlock()
		reply(w, http.StatusOK, map[string]any{"id": head})
	})
	gl.Handle(http.MethodGet, id+"/tree", func(w http.ResponseWriter, r *http.Request) {
		commit := r.URL.Query().Get("ref")
		seen(commit)
		if _, ok := text(commit); !ok || r.URL.Query().Get("path") != "skills" {
			reply(w, http.StatusNotFound, map[string]string{"message": "404 Tree Not Found"})
			return
		}
		reply(w, http.StatusOK, []map[string]string{{"name": "lint.md", "path": skillFilePath, "type": "blob"}})
	})
	gl.Handle(http.MethodGet, id+"/files/"+url.PathEscape(skillFilePath)+"/raw", func(w http.ResponseWriter, r *http.Request) {
		commit := r.URL.Query().Get("ref")
		seen(commit)
		t, ok := text(commit)
		if !ok {
			reply(w, http.StatusNotFound, map[string]string{"message": "404 File Not Found"})
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(t))
	})
}

// skillFetch reads the fixture's skills folder as the admin does in the
// dashboard: it starts the read, waits for it and returns the finished read.
func (e *dashE2E) skillFetch(t *testing.T, admin dashUser, purpose string) dashboard.SkillFetchView {
	t.Helper()
	status, body := e.write(t, admin, http.MethodPost, "/api/admin/skill-fetches", map[string]string{
		"instance": testInstance, "repo": skillSourcePath, "ref": "main", "path": "skills", "purpose": purpose,
	})
	var started dashboard.SkillFetchStartedView
	if status != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.ID == "" {
		t.Fatalf("starting a read = %d: %s", status, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var view dashboard.SkillFetchView
		e.getJSON(t, admin, "/api/admin/skill-fetches/"+started.ID, &view)
		if view.Status == store.FetchDone {
			return view
		}
		if view.Status != store.FetchRunning || time.Now().After(deadline) {
			t.Fatalf("the read ended %q (%q); want done", view.Status, view.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fetchResults posts names to the read's install or update route, wants 200
// and returns the per-skill answers.
func (e *dashE2E) fetchResults(t *testing.T, admin dashUser, id, action string, body map[string]any) []dashboard.SkillFetchResultView {
	t.Helper()
	status, data := e.write(t, admin, http.MethodPost, "/api/admin/skill-fetches/"+id+"/"+action, body)
	var out []dashboard.SkillFetchResultView
	if status != http.StatusOK || json.Unmarshal(data, &out) != nil {
		t.Fatalf("%s = %d: %s", action, status, data)
	}
	return out
}

// pushHead sends the webhook for merge request 1 at head (oldrev the head
// before it, "" for an open), with the platform serving it as open.
func (e *dashE2E) pushHead(t *testing.T, head, oldrev string) {
	t.Helper()
	action := "update"
	if oldrev == "" {
		action = "open"
	}
	e.env.GitLab.SetMR(openMR(1, head))
	e.env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	e.hook(t, action, 1, head, oldrev)
}

// lintResult is a clean review by the lint skill.
func lintResult(t *testing.T, head string) []byte {
	t.Helper()
	return resultFor(t, head, result.Skill{Name: "lint", Verdict: "no_blocking_issues", Files: result.Files{Reviewed: []string{"a.go"}}})
}

// An admin installs a skill from a repository on a connected instance, a
// review runs with it and records its version, the source moves on and a
// check for updates brings the new text in as version 2, which the next
// review uses and the run page shows; with no skills at all a push is
// skipped and sets no status.
func TestEndToEndSkills(t *testing.T) {
	const c1, c2 = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	src := &skillSource{head: c1, files: map[string]string{c1: lintV1, c2: lintV2}}
	e := newDashboardE2E(t, func(o *dashboard.Options) {
		// What main passes: the review bot's Forge reads the repositories of
		// a connected instance.
		o.Fetcher = &skillfetch.Fetcher{
			Store: o.Store,
			Readers: func(instance string) (forge.SourceReader, bool) {
				p, ok := o.Sources.Get(instance)
				if !ok {
					return nil, false
				}
				r, ok := p.(forge.SourceReader)
				return r, ok
			},
			Secrets:      o.Instances.HeldSecrets,
			CallbackBase: o.Config.CallbackURL, Now: o.Now, Log: o.Log,
		}
	})
	env := e.env
	src.serve(env.GitLab)

	// The operator signs in with the first password admin. The library has
	// only the test environment's security skill, which is on the default
	// list: empty the list, so what a review runs is what this test installs.
	t.Setenv(adminPasswordEnv, bootstrapPassword)
	env.Engine.Config.Dashboard.AdminPasswordEnv = adminPasswordEnv
	if err := dashboard.Bootstrap(env.ctx, env.Store, env.Engine.Config, env.Now(), env.Engine.Log); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	admin := e.passwordSignIn(t, "admin", bootstrapPassword)
	if status, body := e.write(t, admin, http.MethodPost, "/auth/password/change",
		map[string]string{"current": bootstrapPassword, "new": changedPassword}); status != http.StatusNoContent {
		t.Fatalf("password change = %d: %s", status, body)
	}
	if status, body := e.write(t, admin, http.MethodPut, "/api/admin/defaults", map[string]any{"skills": []string{}}); status != http.StatusOK {
		t.Fatalf("emptying the default list = %d: %s", status, body)
	}

	// 1. Install lint from the repository at C1, onto the default list.
	read := e.skillFetch(t, admin, store.FetchInstall)
	if read.Commit != c1 || len(read.Skills) != 1 || read.Skills[0].Name != "lint" || read.Skills[0].Path != skillFilePath {
		t.Fatalf("the read = %+v, want lint at %s from %s", read, skillFilePath, c1)
	}
	if got := e.fetchResults(t, admin, read.ID, "install", map[string]any{"names": []string{"lint"}, "addToDefaults": true}); len(got) != 1 || !got[0].OK {
		t.Fatalf("install = %+v, want lint installed", got)
	}
	var lint dashboard.SkillDetailView
	e.getJSON(t, admin, "/api/skills/lint", &lint)
	if lint.Version != 1 || lint.Content != lintV1 || !lint.InDefaults || lint.Source == nil || lint.Source.Commit != c1 {
		t.Fatalf("lint after the install = %+v, want v1 at %s on the default list", lint, c1)
	}

	// 2. A review of the first head: its Job holds lint's text and the run
	// records version 1.
	head1, head2, head3 := sha('a'), sha('c'), sha('d')
	e.pushHead(t, head1, "")
	run1 := liveRun(t, env, 1, head1, "main").ID
	dispatch(t, env)
	if got := jobInputs(t, env, run1); !strings.Contains(got, "Lint v1:") || strings.Contains(got, securityText) {
		t.Fatalf("the first Job's inputs = %q, want lint v1 and nothing of security", got)
	}
	if got := env.Run(run1).SkillVersions; !maps.Equal(got, map[string]int{"lint": 1}) {
		t.Fatalf("run 1 versions = %v, want lint at 1", got)
	}
	if code := e.postResult(t, run1, string(env.Secret(run1).Data["result-token"]), lintResult(t, head1)); code != http.StatusOK {
		t.Fatalf("the first result: status %d", code)
	}
	servePublish(env, 1, head1)
	publish(t, env)
	wantPublished(t, env, run1)

	// 3. The source moves to C2. A check for updates sees the change and
	// Update brings it in as version 2.
	src.moveTo(c2)
	check := e.skillFetch(t, admin, store.FetchUpdate)
	if check.Commit != c2 || len(check.Updates) != 1 || check.Updates[0].Name != "lint" || check.Updates[0].State != "changed" ||
		check.Updates[0].Version != 1 || check.Updates[0].Fetched != lintV2 {
		t.Fatalf("the check for updates = %+v, want lint changed at v1 with the C2 text", check)
	}
	if got := e.fetchResults(t, admin, check.ID, "update", map[string]any{"names": []string{"lint"}}); len(got) != 1 || !got[0].OK {
		t.Fatalf("update = %+v, want lint updated", got)
	}
	e.getJSON(t, admin, "/api/skills/lint", &lint)
	if lint.Version != 2 || lint.Content != lintV2 || lint.Source == nil || lint.Source.Commit != c2 || len(lint.Versions) != 2 {
		t.Fatalf("lint after the update = %+v, want v2 at %s with two versions", lint, c2)
	}

	// 4. The next review uses version 2; the run page shows it.
	env.GitLab.Reset()
	e.pushHead(t, head2, head1)
	cleanup(t, env)
	run2 := liveRun(t, env, 1, head2, "main").ID
	dispatch(t, env)
	if got := jobInputs(t, env, run2); !strings.Contains(got, "Lint v2:") || strings.Contains(got, "Lint v1:") {
		t.Fatalf("the second Job's inputs = %q, want lint v2 only", got)
	}
	if got := env.Run(run2).SkillVersions; !maps.Equal(got, map[string]int{"lint": 2}) {
		t.Fatalf("run 2 versions = %v, want lint at 2", got)
	}
	if code := e.postResult(t, run2, string(env.Secret(run2).Data["result-token"]), lintResult(t, head2)); code != http.StatusOK {
		t.Fatalf("the second result: status %d", code)
	}
	servePublish(env, 1, head2)
	publish(t, env)
	wantPublished(t, env, run2)
	for id, version := range map[string]int{run1: 1, run2: 2} {
		var page dashboard.RunDetailView
		e.getJSON(t, admin, "/api/runs/"+id, &page)
		if len(page.Skills) != 1 || page.Skills[0].Name != "lint" || page.Skills[0].Version != version ||
			page.Skills[0].Source == nil || *page.Skills[0].Source != "library" {
			t.Errorf("run page of %s: skills = %+v, want lint from the library at version %d", id, page.Skills, version)
		}
	}

	// 5. With the default list emptied and no repository skills, a push is
	// skipped: no Job, no comment and no status, and the dashboard says why.
	if status, body := e.write(t, admin, http.MethodPut, "/api/admin/defaults", map[string]any{"skills": []string{}}); status != http.StatusOK {
		t.Fatalf("emptying the default list = %d: %s", status, body)
	}
	env.GitLab.Reset()
	e.pushHead(t, head3, head2)
	run3 := liveRun(t, env, 1, head3, "main").ID
	jobs := len(env.Jobs())
	dispatch(t, env)
	if r := env.Run(run3); r.State != store.Skipped || r.SkipReason != "this repository has no skills" {
		t.Fatalf("run 3 = %s (%q), want skipped: this repository has no skills\n%s", r.State, r.SkipReason, env.Logs())
	}
	if n := len(env.Jobs()); n != jobs {
		t.Errorf("a skipped run started a Job: %d Jobs, want %d", n, jobs)
	}
	publish(t, env)
	if n := len(env.GitLab.Requests(http.MethodPost, statusPath(head3))); n != 0 {
		t.Errorf("a skipped run set %d commit statuses on its head, want none", n)
	}
	for _, r := range gitlabWrites(env) {
		if r.Method == http.MethodPost && (r.Path == notesPath(1) || r.Path == statusPath(head3)) {
			t.Errorf("a skipped run wrote %s %s", r.Method, r.Path)
		}
	}
	if a := attentionItem(t, env, "no_skills"); a == nil || a.Message != "this repository has no skills" {
		t.Errorf("needs attention = %+v, want the no-skills item", a)
	}
	var page dashboard.RunDetailView
	e.getJSON(t, admin, "/api/runs/"+run3, &page)
	if page.Run.Outcome != dashboard.OutcomeSkipped || page.Status != "" || page.Approved ||
		page.Run.Reason != "Not reviewed: this repository has no skills." {
		t.Errorf("run 3 page = outcome %q, status %q, approved %v, reason %q; want skipped with no status, not approved",
			page.Run.Outcome, page.Status, page.Approved, page.Run.Reason)
	}
}
