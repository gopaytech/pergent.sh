package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// fakeReader is an instance's reader of repositories by path: each
// repository's refs and, by commit, its files.
type fakeReader struct {
	mu    sync.Mutex
	repos map[string]*fakeRepo // by lower-case path
}

type fakeRepo struct {
	refs  map[string]string
	files map[string]map[string]string
}

var _ forge.SourceReader = (*fakeReader)(nil)

// skillRepo sets repository project of instance to have, at ref, commit
// with files.
func (e *env) skillRepo(instance, project, ref, commit string, files map[string]string) {
	e.t.Helper()
	r := e.readers[instance]
	r.mu.Lock()
	defer r.mu.Unlock()
	rp, ok := r.repos[strings.ToLower(project)]
	if !ok {
		rp = &fakeRepo{refs: map[string]string{}, files: map[string]map[string]string{}}
		r.repos[strings.ToLower(project)] = rp
	}
	rp.refs[ref] = commit
	rp.files[commit] = files
}

func (r *fakeReader) at(project, commit string) (map[string]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rp, ok := r.repos[strings.ToLower(project)]
	if !ok {
		return nil, false
	}
	files, ok := rp.files[commit]
	return files, ok
}

func (r *fakeReader) ResolveRef(_ context.Context, project, ref string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rp, ok := r.repos[strings.ToLower(project)]
	if !ok {
		return "", forge.ErrSourceNotFound
	}
	commit, ok := rp.refs[ref]
	if !ok {
		return "", forge.ErrSourceNotFound
	}
	return commit, nil
}

func (r *fakeReader) ListTreeAt(_ context.Context, project, dir, commit string) ([]forge.TreeEntry, error) {
	files, ok := r.at(project, commit)
	if !ok {
		return nil, forge.ErrSourceNotFound
	}
	seen := map[string]bool{}
	var out []forge.TreeEntry
	for p := range files {
		rest, found := strings.CutPrefix(p, dir+"/")
		if dir == "" {
			rest, found = p, true
		}
		if !found {
			continue
		}
		name, _, nested := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}
		seen[name] = true
		e := forge.TreeEntry{Name: name, Path: path.Join(dir, name), Type: "blob"}
		if nested {
			e.Type = "tree"
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (r *fakeReader) GetFileAt(_ context.Context, project, p, commit string) ([]byte, bool, error) {
	files, ok := r.at(project, commit)
	if !ok {
		return nil, false, forge.ErrSourceNotFound
	}
	c, ok := files[p]
	if !ok {
		return nil, false, nil
	}
	return []byte(c), true, nil
}

const (
	commitV1  = "1111111111111111111111111111111111111111"
	commitV11 = "2222222222222222222222222222222222222222"
)

// startFetch starts a read as c and wants 202; it returns the read's ID.
func (e *env) startFetch(c *http.Cookie, body map[string]any) string {
	e.t.Helper()
	rec := e.sendJSON(c, "POST", "/api/admin/skill-fetches", body)
	started := decode[SkillFetchStartedView](e.t, rec, "start a fetch", http.StatusAccepted)
	if started.ID == "" {
		e.t.Fatalf("start a fetch = %s; want an id", rec.Body)
	}
	return started.ID
}

// fetchResult waits for the read id to end and returns it, as c sees it.
func (e *env) fetchResult(c *http.Cookie, id string) SkillFetchView {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v := getJSON[SkillFetchView](e, c, "/api/admin/skill-fetches/"+id)
		if v.Status != store.FetchRunning {
			return v
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("fetch %s still running", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fetchDone starts a read and waits for it, wanting it done.
func (e *env) fetchDone(c *http.Cookie, body map[string]any) (string, SkillFetchView) {
	e.t.Helper()
	id := e.startFetch(c, body)
	v := e.fetchResult(c, id)
	if v.Status != store.FetchDone {
		e.t.Fatalf("fetch = %+v; want done", v)
	}
	return id, v
}

func (e *env) skillAction(c *http.Cookie, id, action string, body map[string]any) []SkillFetchResultView {
	e.t.Helper()
	rec := e.sendJSON(c, "POST", "/api/admin/skill-fetches/"+id+"/"+action, body)
	return decode[[]SkillFetchResultView](e.t, rec, action, http.StatusOK)
}

func wantResults(t *testing.T, got []SkillFetchResultView, want ...SkillFetchResultView) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("results = %+v\nwant      %+v", got, want)
	}
}

// Any admin reads any repository the instance's review bot or App reaches, of
// any instance, or a public URL: nothing asks what the admin could read on the
// platform. A read names its instance. Someone who is no admin reads nothing.
func TestAnyAdminReadsAnySource(t *testing.T) {
	e := newEnv(t, nil)
	files := map[string]string{"style.md": "Be kind."}
	e.skillRepo("main", "acme/skills", "v1.0", commitV1, files)
	e.skillRepo("gh", "acme/skills", "v1.0", commitV1, files)
	admin := e.userOnMain(true)
	body := map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0"}

	id := e.startFetch(admin, body)
	if v := e.fetchResult(admin, id); v.Status != store.FetchDone || len(v.Skills) != 1 || v.Skills[0].Name != "style" {
		t.Errorf("fetch = %+v", v)
	}
	e.startFetch(admin, map[string]any{"instance": "gh", "repo": "acme/skills", "ref": "v1.0"})
	// The instance is not guessed.
	rec := e.sendJSON(admin, "POST", "/api/admin/skill-fetches", map[string]any{"repo": "acme/skills", "ref": "v1.0"})
	wantCode(t, rec, "no instance", http.StatusBadRequest)

	op := e.operator()
	e.startFetch(op, map[string]any{"instance": "gh", "repo": "acme/skills", "ref": "v1.0"})
	rec = e.sendJSON(op, "POST", "/api/admin/skill-fetches", map[string]any{"instance": "nope", "repo": "acme/skills", "ref": "v1.0"})
	wantCode(t, rec, "an instance not connected", http.StatusBadGateway)
	rec = e.sendJSON(op, "POST", "/api/admin/skill-fetches", map[string]any{"instance": "main", "repo": "acme/skills", "ref": "a b"})
	wantCode(t, rec, "a bad ref", http.StatusBadRequest)
	rec = e.sendJSON(op, "POST", "/api/admin/skill-fetches", map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1", "token": "x"})
	wantCode(t, rec, "an unknown field", http.StatusBadRequest)
	rec = e.sendJSON(op, "POST", "/api/admin/skill-fetches", map[string]any{"url": "http://example.com/skills.git", "ref": "v1"})
	wantCode(t, rec, "an http URL", http.StatusBadRequest)

	// Viewers and maintainers can't read sources.
	e2 := newEnv(t, nil)
	viewer := e2.userOnMain(false, "acme/skills")
	rec = e2.sendJSON(viewer, "POST", "/api/admin/skill-fetches", body)
	wantCode(t, rec, "a viewer", http.StatusNotFound)
}

// Reading a source, installing from it and updating from it are for admins:
// a maintainer and a viewer get 404 on each, even holding the ID of an admin's
// read, and nothing is installed.
func TestInstallSkillNeedsAdmin(t *testing.T) {
	e := newEnv(t, nil)
	e.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"style.md": "Be kind."})
	admin := e.userOnMain(true)
	id, _ := e.fetchDone(admin, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0"})

	e2 := newEnv(t, nil)
	e2.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"style.md": "Be kind."})
	maintainer := e2.signInRole(store.RoleMaintainer)
	e3 := newEnv(t, nil)
	e3.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"style.md": "Be kind."})
	viewer := e3.signInRole(store.RoleViewer)

	for name, who := range map[string]struct {
		e *env
		c *http.Cookie
	}{"maintainer": {e2, maintainer}, "viewer": {e3, viewer}} {
		rec := who.e.sendJSON(who.c, "POST", "/api/admin/skill-fetches", map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0"})
		wantCode(t, rec, name+" starting a read", http.StatusNotFound)
		for _, w := range []struct{ method, target string }{
			{"GET", "/api/admin/skill-fetches/" + id},
			{"POST", "/api/admin/skill-fetches/" + id + "/install"},
			{"POST", "/api/admin/skill-fetches/" + id + "/update"},
		} {
			rec := who.e.sendJSON(who.c, w.method, w.target, map[string]any{"names": []string{"style"}})
			wantCode(t, rec, name+" "+w.method+" "+w.target, http.StatusNotFound)
		}
		if _, found := who.e.storedSkill("style"); found {
			t.Errorf("a %s installed a skill", name)
		}
	}

	// The admin's own read is not harmed, and installs.
	e.skillAction(admin, id, "install", map[string]any{"names": []string{"style"}})
	if _, found := e.storedSkill("style"); !found {
		t.Error("the admin's install did not store the skill")
	}
}

// Only the admin who started a read sees it and acts on it.
func TestFetchOnlyRequesterSees(t *testing.T) {
	e := newEnv(t, nil)
	e.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"style.md": "Be kind."})
	admin := e.userOnMain(true)
	id, _ := e.fetchDone(admin, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0"})

	op := e.operator()
	e.wantStatus(op, "/api/admin/skill-fetches/"+id, http.StatusNotFound)
	for _, action := range []string{"install", "update"} {
		rec := e.sendJSON(op, "POST", "/api/admin/skill-fetches/"+id+"/"+action, map[string]any{"names": []string{"style"}})
		wantCode(t, rec, "another admin's "+action, http.StatusNotFound)
	}
	if _, found := e.storedSkill("style"); found {
		t.Error("another admin installed from the read")
	}
	for _, bad := range []string{"nope", "00000000-0000-0000-0000-000000000000"} {
		e.wantStatus(admin, "/api/admin/skill-fetches/"+bad, http.StatusNotFound)
	}
	// An expired read is gone.
	e.clock.advance(2 * time.Hour)
	e.wantStatus(admin, "/api/admin/skill-fetches/"+id, http.StatusNotFound)
}

// An install records each skill's source, ref, path and commit, adds it to
// the default list when asked (within the budget), and is audited without
// the text or the private repository's path.
func TestInstallRecordsSourceAndDefaults(t *testing.T) {
	e := newEnv(t, nil)
	e.skillRepo("main", "acme/private-skills", "v1.0", commitV1, map[string]string{
		"skills/style.md":        "---\nname: style\n---\nBe kind.\n",
		"skills/review/SKILL.md": "Review carefully.\n",
		"other/not-read.md":      "elsewhere",
	})
	op := e.operator()
	id, v := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/private-skills", "ref": "v1.0", "path": "skills"})
	if v.Commit != commitV1 || v.Purpose != "install" || v.Path != "skills" || len(v.Updates) != 0 {
		t.Errorf("fetch = %+v", v)
	}
	want := []FetchedSkillView{
		{Name: "review", Path: "skills/review/SKILL.md", Size: len("Review carefully.\n")},
		{Name: "style", Path: "skills/style.md", Size: len("---\nname: style\n---\nBe kind.\n")},
	}
	if !slices.Equal(v.Skills, want) {
		t.Errorf("skills = %+v\nwant     %+v", v.Skills, want)
	}

	got := e.skillAction(op, id, "install", map[string]any{"names": []string{"style", "review", "nope"}, "addToDefaults": true})
	wantResults(t, got, SkillFetchResultView{Name: "style", OK: true}, SkillFetchResultView{Name: "review", OK: true},
		SkillFetchResultView{Name: "nope", Error: "not found in this read"})
	sk, found := e.storedSkill("style")
	wantSrc := store.SkillSource{Kind: store.SourceKindInstance, Instance: "main", Repo: "acme/private-skills", Ref: "v1.0",
		Path: "skills/style.md", Commit: commitV1}
	if !found || sk.Version != 1 || sk.Content != "---\nname: style\n---\nBe kind.\n" || sk.Source == nil || *sk.Source != wantSrc ||
		sk.UpdatedBy != "ops (password)" {
		t.Errorf("style = %+v (source %+v)", sk, sk.Source)
	}
	if got := e.defaultSkills(); !slices.Equal(got, []string{"style", "review"}) {
		t.Errorf("default skills = %v", got)
	}
	installed := e.auditOf("skill_installed") // newest first
	if len(installed) != 2 || !strings.HasPrefix(installed[1].Detail, "style: installed from ") ||
		!strings.HasPrefix(installed[0].Detail, "review: installed from ") {
		t.Fatalf("skill_installed audit = %+v", installed)
	}
	for _, a := range installed {
		if strings.Contains(a.Detail, "Be kind") || strings.Contains(a.Detail, "acme/private-skills") ||
			!strings.Contains(a.Detail, "v1.0") || !strings.Contains(a.Detail, commitV1[:7]) {
			t.Errorf("audit detail %q: want the ref and commit, no text and no private path", a.Detail)
		}
	}
	if len(e.auditOf("defaults_updated")) != 1 {
		t.Errorf("defaults_updated audit = %+v", e.auditOf("defaults_updated"))
	}
	// The read now shows both names as taken.
	for _, s := range getJSON[SkillFetchView](e, op, "/api/admin/skill-fetches/"+id).Skills {
		if !s.Taken {
			t.Errorf("%s is not taken after its install", s.Name)
		}
	}

	// Over the default list's budget the name is refused whole.
	e2 := newEnv(t, nil)
	var defaults []string
	for i := range 7 {
		name := "big" + string(rune('a'+i))
		e2.skill(name, strings.Repeat("x", 125<<10))
		defaults = append(defaults, name)
	}
	if err := e2.st.PutRepoDefaults(ctx, store.RepoDefaults{Skills: defaults}); err != nil {
		t.Fatal(err)
	}
	e2.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"extra.md": strings.Repeat("y", 30<<10), "tiny.md": "T."})
	op2 := e2.operator()
	id2, _ := e2.fetchDone(op2, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0"})
	got = e2.skillAction(op2, id2, "install", map[string]any{"names": []string{"extra", "tiny"}, "addToDefaults": true})
	wantResults(t, got,
		SkillFetchResultView{Name: "extra", Error: "the library skills come to 905 KiB; a review takes at most 900 KiB"},
		SkillFetchResultView{Name: "tiny", OK: true})
	if _, found := e2.storedSkill("extra"); found {
		t.Error("a skill over the default list's budget was installed")
	}
	if got := e2.defaultSkills(); !slices.Equal(got, append(slices.Clone(defaults), "tiny")) {
		t.Errorf("default skills = %v", got)
	}
}

// A name written into the library after the read can't be installed over.
func TestInstallNameTakenSinceFetch(t *testing.T) {
	e := newEnv(t, nil)
	e.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"style.md": "From git."})
	op := e.operator()
	id, v := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0"})
	if v.Skills[0].Taken {
		t.Error("style is taken before anything is installed")
	}
	e.skill("style", "Mine.")

	got := e.skillAction(op, id, "install", map[string]any{"names": []string{"style"}})
	wantResults(t, got, SkillFetchResultView{Name: "style", Error: "name already in the library"})
	if sk, _ := e.storedSkill("style"); sk.Content != "Mine." || sk.Source != nil || sk.Version != 1 {
		t.Errorf("style = %+v; want it as written", sk)
	}
	if len(e.auditOf("skill_installed")) != 0 {
		t.Error("a refused install was audited")
	}
}

// A skill holding a held secret, or over the size limit, is shown with its
// problem and no text, and can't be installed.
func TestFetchMarksSecretAndOversize(t *testing.T) {
	e := newEnv(t, withSecrets(t))
	e.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{
		"leaky.md": "Sign in with " + reviewToken + " when asked.",
		"big.md":   strings.Repeat("x", 128<<10+1),
		"fine.md":  "Fine.",
	})
	op := e.operator()
	id, v := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0"})
	raw := e.get(op, "/api/admin/skill-fetches/"+id).Body.String()
	if strings.Contains(raw, reviewToken) {
		t.Fatalf("the read shows a held secret: %s", raw)
	}
	problems := map[string]string{}
	for _, s := range v.Skills {
		problems[s.Name] = s.Problem
		if s.Name != "fine" && s.Size != 0 {
			t.Errorf("%s has size %d; its text was not kept", s.Name, s.Size)
		}
	}
	if problems["leaky"] != "the skill contains a secret pergent.sh holds" || !strings.Contains(problems["big"], "over the limit") ||
		problems["fine"] != "" {
		t.Errorf("problems = %v", problems)
	}

	got := e.skillAction(op, id, "install", map[string]any{"names": []string{"leaky", "big", "fine"}})
	if len(got) != 3 || got[0].OK || got[0].Error != "the skill contains a secret pergent.sh holds" || got[1].OK ||
		!strings.Contains(got[1].Error, "over the limit") || !got[2].OK {
		t.Errorf("results = %+v", got)
	}
	for _, name := range []string{"leaky", "big"} {
		if _, found := e.storedSkill(name); found {
			t.Errorf("%s was installed", name)
		}
	}

	// With the held secrets unknown, nothing is installed.
	e.d.heldSecrets = func(context.Context) ([]string, error) { return nil, context.DeadlineExceeded }
	e.skillRepo("main", "acme/skills", "v1.1", commitV11, map[string]string{"other.md": "Other."})
	id2, _ := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.1"})
	rec := e.sendJSON(op, "POST", "/api/admin/skill-fetches/"+id2+"/install", map[string]any{"names": []string{"other"}})
	wantCode(t, rec, "install with the secrets unknown", http.StatusInternalServerError)
	if _, found := e.storedSkill("other"); found {
		t.Error("a skill was installed with the held secrets unknown")
	}
}

// Check for updates at a new tag shows each installed skill's text against
// the tag's, and Update saves it with the new ref and commit.
func TestUpdateNewRefShowsDiff(t *testing.T) {
	e := newEnv(t, nil)
	e.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"skills/style.md": "Old text.", "skills/same.md": "Same."})
	e.skillRepo("main", "acme/skills", "v1.1", commitV11, map[string]string{"skills/style.md": "New text.", "skills/same.md": "Same."})
	op := e.operator()
	id, _ := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0", "path": "skills"})
	e.skillAction(op, id, "install", map[string]any{"names": []string{"style", "same"}})

	upd, v := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.1", "fromRef": "v1.0",
		"path": "skills", "purpose": "update"})
	want := []SkillUpdateView{
		{Name: "same", Path: "skills/same.md", State: "same", Version: 1, Current: "Same.", Fetched: "Same."},
		{Name: "style", Path: "skills/style.md", State: "changed", Version: 1, Current: "Old text.", Fetched: "New text."},
	}
	if v.Purpose != "update" || v.FromRef != "v1.0" || !slices.Equal(v.Updates, want) {
		t.Fatalf("updates = %+v\nwant      %+v", v.Updates, want)
	}
	if sk, _ := e.storedSkill("style"); sk.Content != "Old text." || sk.Version != 1 {
		t.Errorf("style changed before Update: %+v", sk)
	}

	got := e.skillAction(op, upd, "update", map[string]any{"names": []string{"style", "same", "nope"}})
	wantResults(t, got, SkillFetchResultView{Name: "style", OK: true}, SkillFetchResultView{Name: "same", OK: true},
		SkillFetchResultView{Name: "nope", Error: "not installed from this source and ref"})
	for name, content := range map[string]string{"style": "New text.", "same": "Same."} {
		sk, _ := e.storedSkill(name)
		if sk.Version != 2 || sk.Content != content || sk.Source == nil || sk.Source.Ref != "v1.1" || sk.Source.Commit != commitV11 {
			t.Errorf("%s = %+v (source %+v); want v2 at v1.1", name, sk, sk.Source)
		}
	}
	audited := e.auditOf("skill_source_updated") // newest first
	if len(audited) != 2 || !strings.HasPrefix(audited[1].Detail, "style: v1 -> v2") || strings.Contains(audited[1].Detail, "New text") ||
		!strings.Contains(audited[1].Detail, "v1.1 ("+commitV11[:7]+")") {
		t.Errorf("skill_source_updated audit = %+v", audited)
	}

	// An install read can't update.
	rec := e.sendJSON(op, "POST", "/api/admin/skill-fetches/"+id+"/update", map[string]any{"names": []string{"style"}})
	wantCode(t, rec, "update from an install read", http.StatusConflict)
}

// A skill gone from its path is "removed at the source", one whose file now
// names another skill is "renamed at the source"; neither is updated.
func TestUpdateRemovedAndRenamed(t *testing.T) {
	e := newEnv(t, nil)
	e.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"a.md": "A.", "b.md": "B."})
	e.skillRepo("main", "acme/skills", "v2.0", commitV11, map[string]string{"a.md": "---\nname: alpha\n---\nA.", "c.md": "C."})
	op := e.operator()
	id, _ := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0"})
	e.skillAction(op, id, "install", map[string]any{"names": []string{"a", "b"}})

	upd, v := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v2.0", "fromRef": "v1.0", "purpose": "update"})
	states := map[string]string{}
	for _, u := range v.Updates {
		states[u.Name] = u.State
	}
	if len(v.Updates) != 2 || states["a"] != "renamed at the source" || states["b"] != "removed at the source" {
		t.Errorf("updates = %+v", v.Updates)
	}
	got := e.skillAction(op, upd, "update", map[string]any{"names": []string{"a", "b"}})
	wantResults(t, got, SkillFetchResultView{Name: "a", Error: "renamed at the source"},
		SkillFetchResultView{Name: "b", Error: "removed at the source"})
	for _, name := range []string{"a", "b"} {
		if sk, _ := e.storedSkill(name); sk.Version != 1 || sk.Source.Ref != "v1.0" {
			t.Errorf("%s = %+v; want it unchanged", name, sk)
		}
	}
	// The renamed skill can be installed again under its new name.
	got = e.skillAction(op, upd, "install", map[string]any{"names": []string{"alpha"}})
	wantResults(t, got, SkillFetchResultView{Name: "alpha", OK: true})
}

// The repository an installed skill was read from shows to admins only; a
// user on a team sees that it came from "a private repository on <instance>",
// and a URL source is public. (Every admin may read every repository the bot
// or App reaches, so there is no per-user check.)
func TestPrivateSourceHiddenFromUsers(t *testing.T) {
	e := newEnv(t, nil)
	e.installedSkill("alpha", "Alpha text.")
	src := store.SkillSource{Kind: store.SourceKindURL, URL: "https://example.com/skills.git", Ref: "main", Path: "beta.md", Commit: "c"}
	if _, err := e.st.CreateLibrarySkill(ctx, store.NewSkill{Name: "beta", Content: "Beta.", Source: &src, At: e.clock.now()}); err != nil {
		t.Fatal(err)
	}
	sourceOf := func(c *http.Cookie, name string) SkillSourceView {
		t.Helper()
		for _, s := range getJSON[[]LibrarySkillView](e, c, "/api/skills") {
			if s.Name == name {
				return *s.Source
			}
		}
		t.Fatalf("no skill %s", name)
		return SkillSourceView{}
	}

	viewer := e.userOnMain(false)
	if s := sourceOf(viewer, "alpha"); !s.Hidden || s.Repo != "" || s.Path != "" || s.Instance != "main" {
		t.Errorf("alpha's source for a user = %+v", s)
	}
	if raw := e.get(viewer, "/api/skills/alpha").Body.String(); strings.Contains(raw, "acme/private-skills") {
		t.Errorf("the skill's page shows the private path: %s", raw)
	}
	if d := getJSON[SkillDetailView](e, viewer, "/api/skills/alpha"); d.Source == nil || !d.Source.Hidden || d.Source.Repo != "" {
		t.Errorf("alpha's page source = %+v", d.Source)
	}
	if s := sourceOf(viewer, "beta"); s.URL != "https://example.com/skills.git" || s.Hidden {
		t.Errorf("beta's URL source = %+v; want shown", s)
	}

	// An admin, an SSO one or a local one, sees it.
	for name, c := range map[string]*http.Cookie{"sso admin": e.sessionFor(e.ssoAdmin("boss@example.com")), "local admin": e.operator()} {
		if s := sourceOf(c, "alpha"); s.Hidden || s.Repo != "acme/private-skills" {
			t.Errorf("alpha's source for a %s = %+v", name, s)
		}
		if d := getJSON[SkillDetailView](e, c, "/api/skills/alpha"); d.Source == nil || d.Source.Repo != "acme/private-skills" {
			t.Errorf("alpha's page source for a %s = %+v", name, d.Source)
		}
	}
}

// holdLibrary locks the skill library against writes, calls start (which
// begins a write in another goroutine), waits for that write to be stuck
// behind the lock, runs change meanwhile and then lets the write go on.
func (e *env) holdLibrary(start, change func()) {
	e.t.Helper()
	tx, err := e.db.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // released at the end of the test's hold
	if _, err := tx.Exec(ctx, `LOCK TABLE library_skills IN ACCESS EXCLUSIVE MODE`); err != nil {
		e.t.Fatal(err)
	}
	start()
	// The write is stuck once Postgres shows a statement waiting on a lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := e.db.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%library_skills%'`).Scan(&waiting); err != nil {
			e.t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			e.t.Fatal("the library write never waited for the lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	change()
	if err := tx.Rollback(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// Adding a new or installed skill to the default list changes that list and
// nothing else: a change to the other defaults made while the skill is being
// saved (approve here) stays.
func TestAddToDefaultsKeepsOtherDefaultChanges(t *testing.T) {
	check := func(t *testing.T, e *env, send func() *httptest.ResponseRecorder) {
		t.Helper()
		done := make(chan *httptest.ResponseRecorder, 1)
		e.holdLibrary(func() { go func() { done <- send() }() }, func() {
			yes := true
			if err := e.st.PutRepoDefaults(ctx, store.RepoDefaults{Approve: &yes}); err != nil {
				t.Error(err)
			}
		})
		if rec := <-done; rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("save = %d %s", rec.Code, rec.Body)
		}
		d, err := e.st.RepoDefaults(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d.Approve == nil || !*d.Approve {
			t.Errorf("approve = %v after the save; want the change made meanwhile kept", d.Approve)
		}
		if !slices.Equal(d.Skills, []string{"raced"}) {
			t.Errorf("default skills = %v; want [raced]", d.Skills)
		}
	}

	t.Run("create", func(t *testing.T) {
		e := newEnv(t, nil)
		admin := e.userOnMain(true, "acme/widgets")
		check(t, e, func() *httptest.ResponseRecorder {
			return e.sendJSON(admin, "POST", "/api/admin/skills", map[string]any{"name": "raced", "content": "Raced.", "addToDefaults": true})
		})
	})
	t.Run("install", func(t *testing.T) {
		e := newEnv(t, nil)
		e.skillRepo("main", "acme/skills", "v1.0", commitV1, map[string]string{"raced.md": "Raced."})
		op := e.operator()
		id, _ := e.fetchDone(op, map[string]any{"instance": "main", "repo": "acme/skills", "ref": "v1.0"})
		check(t, e, func() *httptest.ResponseRecorder {
			return e.sendJSON(op, "POST", "/api/admin/skill-fetches/"+id+"/install",
				map[string]any{"names": []string{"raced"}, "addToDefaults": true})
		})
	})
}
