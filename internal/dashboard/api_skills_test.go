package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// installedSkill adds a skill installed from a repository of the instance
// "main", as version 1.
func (e *env) installedSkill(name, content string) {
	e.t.Helper()
	src := &store.SkillSource{Kind: store.SourceKindInstance, Instance: "main", Repo: "acme/private-skills",
		Ref: "main", Path: "skills/" + name + ".md", Commit: "abc123"}
	if _, err := e.st.CreateLibrarySkill(ctx, store.NewSkill{Name: name, Content: content, Actor: "installer",
		Source: src, At: e.clock.now()}); err != nil {
		e.t.Fatal(err)
	}
}

func TestSkillsListFromStore(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("beta", "Beta text.")
	e.installedSkill("alpha", "Alpha text.")
	if err := e.st.PutRepoDefaults(ctx, store.RepoDefaults{Skills: []string{"beta"}}); err != nil {
		t.Fatal(err)
	}

	admin := e.signInGitLab(true, "acme/widgets")
	got := getJSON[[]LibrarySkillView](e, admin, "/api/skills")
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Fatalf("skills = %+v; want alpha, beta by name", got)
	}
	alpha, beta := got[0], got[1]
	if alpha.Version != 1 || alpha.Size != len("Alpha text.") || alpha.InDefaults || alpha.UpdatedBy != "installer" ||
		alpha.UpdatedAt.IsZero() {
		t.Errorf("alpha = %+v", alpha)
	}
	if !beta.InDefaults || beta.UpdatedBy != "seed" || beta.Source != nil {
		t.Errorf("beta = %+v; want in the defaults, written by seed, no source", beta)
	}
	// The path of a source repository is for operators only for now: a
	// platform admin sees that it is a private repository of the instance.
	if s := alpha.Source; s == nil || s.Kind != "instance" || s.Instance != "main" || s.Repo != "" || !s.Hidden ||
		s.Commit != "abc123" || s.Ref != "main" {
		t.Errorf("alpha source for an admin = %+v", alpha.Source)
	}
	raw := e.get(admin, "/api/skills").Body.String()
	if strings.Contains(raw, "Alpha text.") || strings.Contains(raw, "acme/private-skills") || strings.Contains(raw, "skills/alpha.md") {
		t.Errorf("the list holds a skill's text or its repository: %s", raw)
	}

	op := getJSON[[]LibrarySkillView](e, e.operator(), "/api/skills")
	if s := op[0].Source; s == nil || s.Repo != "acme/private-skills" || s.Path != "skills/alpha.md" || s.Hidden {
		t.Errorf("alpha source for an operator = %+v", op[0].Source)
	}

	// A viewer reads the library but not who wrote it.
	e2 := newEnv(t, nil)
	e2.skill("beta", "Beta text.")
	viewer := e2.signInGitLab(false, "acme/widgets")
	vs := getJSON[[]LibrarySkillView](e2, viewer, "/api/skills")
	if len(vs) != 1 || vs[0].UpdatedBy != "" || vs[0].Name != "beta" {
		t.Errorf("viewer's skills = %+v", vs)
	}
	if strings.Contains(e2.get(viewer, "/api/skills").Body.String(), "seed") {
		t.Error("a viewer's list names who wrote a skill")
	}
	e.wantStatus(nil, "/api/skills", http.StatusUnauthorized)
}

func TestSkillDetailActorOnlyForAdmins(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("style", "First text.")
	if _, err := e.st.UpdateLibrarySkill(ctx, "style", "Second text.", "editor", 1, e.clock.now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	admin := e.signInGitLab(true, "acme/widgets")

	d := getJSON[SkillDetailView](e, admin, "/api/skills/style")
	if d.Name != "style" || d.Version != 2 || d.Content != "Second text." || d.UpdatedBy != "editor" || len(d.Versions) != 2 {
		t.Fatalf("detail = %+v", d)
	}
	if d.Versions[0].Version != 2 || d.Versions[0].Actor != "editor" || d.Versions[1].Version != 1 || d.Versions[1].Actor != "seed" {
		t.Errorf("versions = %+v; want newest first, with actors", d.Versions)
	}
	v := getJSON[SkillVersionDetailView](e, admin, "/api/skills/style/versions/1")
	if v.Version != 1 || v.Content != "First text." || v.Actor != "seed" || v.At.IsZero() {
		t.Errorf("version 1 = %+v", v)
	}

	// A viewer: the same, without the actors.
	e2 := newEnv(t, nil)
	e2.skill("style", "First text.")
	viewer := e2.signInGitLab(false, "acme/widgets")
	raw := e2.get(viewer, "/api/skills/style").Body.String()
	if strings.Contains(raw, "seed") || strings.Contains(raw, "actor") && strings.Contains(raw, `"actor":"`) {
		t.Errorf("a viewer's detail names an actor: %s", raw)
	}
	vd := getJSON[SkillDetailView](e2, viewer, "/api/skills/style")
	if vd.Content != "First text." || vd.UpdatedBy != "" || len(vd.Versions) != 1 || vd.Versions[0].Actor != "" {
		t.Errorf("viewer's detail = %+v", vd)
	}
	raw = e2.get(viewer, "/api/skills/style/versions/1").Body.String()
	if strings.Contains(raw, "seed") {
		t.Errorf("a viewer's version names an actor: %s", raw)
	}

	for _, target := range []string{"/api/skills/nope", "/api/skills/style/versions/9", "/api/skills/style/versions/x",
		"/api/skills/style/versions/0", "/api/skills/.hidden", "/api/skills/a%20b"} {
		e.wantStatus(admin, target, http.StatusNotFound)
	}
}

// A repository's skills over the review's budget are refused with the total.
func TestRepoSettingsSkillsBudget400(t *testing.T) {
	e := newEnv(t, nil)
	big := strings.Repeat("x", 600*1024)
	e.skill("a", big)
	e.skill("b", big)
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.signInGitLab(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"

	before, _ := e.storedRepo("main", "acme/gadgets")
	rec := e.send(c, "PUT", target, `{"skills":["a","b"]}`)
	wantCode(t, rec, "PUT two big skills", http.StatusBadRequest)
	if want := "the library skills come to 1200 KiB; a review takes at most 900 KiB"; errorOf(t, rec) != want {
		t.Errorf("error = %q; want %q", errorOf(t, rec), want)
	}
	if after, _ := e.storedRepo("main", "acme/gadgets"); !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Error("a refused PUT changed the row")
	}
	wantCode(t, e.send(c, "PUT", target, `{"skills":["a"]}`), "PUT one big skill", http.StatusOK)
}

func TestRepoSettingsUnknownSkill400(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("a", "x")
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.signInGitLab(true, "acme/gadgets")
	target := "/api/repositories/main/acme/gadgets/settings"

	rec := e.send(c, "PUT", target, `{"skills":["a","gone"]}`)
	wantCode(t, rec, "PUT an unknown skill", http.StatusBadRequest)
	if want := `skill "gone" is not in the skill library`; errorOf(t, rec) != want {
		t.Errorf("error = %q; want %q", errorOf(t, rec), want)
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.Skills != nil {
		t.Errorf("a refused PUT stored %v", r.Skills)
	}
	// Not a skill name at all: the same message.
	rec = e.send(c, "PUT", target, `{"skills":["../x"]}`)
	wantCode(t, rec, "PUT a bad name", http.StatusBadRequest)
	if want := `skill "../x" is not in the skill library`; errorOf(t, rec) != want {
		t.Errorf("error = %q; want %q", errorOf(t, rec), want)
	}
}

// A skill that goes between the check and the save is named, not a 500: the
// store's error is mapped on every save path.
func TestSkillNotFoundMapped(t *testing.T) {
	err := &store.MissingSkillError{Name: "racy"}
	if msg, ok := skillMissing(err); !ok || msg != `skill "racy" is not in the skill library` {
		t.Errorf("skillMissing = %q, %v", msg, ok)
	}
	if _, ok := skillMissing(store.ErrSkillNotFound); !ok {
		t.Error("the bare ErrSkillNotFound is not mapped")
	}
	if _, ok := skillMissing(http.ErrAbortHandler); ok {
		t.Error("another error is mapped")
	}
}

func TestRepoSkillsSettingRoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.gl.setProject("acme/gadgets", gitlabProject("private", 20, 0))
	target := "/api/repositories/main/acme/gadgets/settings"

	// A viewer can't.
	e.aliceOn("main", store.RoleViewer)
	viewer := e.signIn("gitlab", "main")
	wantCode(t, e.send(viewer, "PUT", target, `{"repoSkills":["a"]}`), "viewer PUT", http.StatusForbidden)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.RepoSkills != nil {
		t.Errorf("a viewer's PUT stored %v", r.RepoSkills)
	}

	e2 := newEnv(t, nil)
	e2.dashboardRepo("main", "acme/gadgets", nil)
	e2.gl.setProject("acme/gadgets", gitlabProject("private", 20, 0))
	e2.aliceOn("main", store.RoleMaintainer)
	maintainer := e2.signIn("gitlab", "main")

	rec := e2.send(maintainer, "PUT", target, `{"repoSkills":["a","b","a"]}`)
	d := decode[RepositoryDetailView](t, rec, "maintainer PUT", http.StatusOK)
	if s := d.Settings["repoSkills"]; !s.Overridden || !slices.Equal(anyStrings(s.Value), []string{"a", "b"}) {
		t.Errorf("repoSkills setting = %+v; want a, b overridden", s)
	}
	if r, _ := e2.storedRepo("main", "acme/gadgets"); !slices.Equal(r.RepoSkills, []string{"a", "b"}) {
		t.Errorf("stored = %v", r.RepoSkills)
	}
	// The names need not be in the library: they are detected in the repository.
	// An empty list and null both turn them all off.
	for _, body := range []string{`{"repoSkills":[]}`, `{"repoSkills":null}`} {
		wantCode(t, e2.send(maintainer, "PUT", target, `{"repoSkills":["a"]}`), "set", http.StatusOK)
		d = decode[RepositoryDetailView](t, e2.send(maintainer, "PUT", target, body), "PUT "+body, http.StatusOK)
		if s := d.Settings["repoSkills"]; s.Overridden || len(anyStrings(s.Value)) != 0 {
			t.Errorf("after %s repoSkills = %+v", body, s)
		}
		if r, _ := e2.storedRepo("main", "acme/gadgets"); r.RepoSkills != nil {
			t.Errorf("after %s stored = %v", body, r.RepoSkills)
		}
	}
	for _, body := range []string{`{"repoSkills":"a"}`, `{"repoSkills":[1]}`, `{"repoSkills":["../x"]}`, `{"repoSkills":[""]}`} {
		wantCode(t, e2.send(maintainer, "PUT", target, body), "PUT "+body, http.StatusBadRequest)
	}
	// A reset turns them off too.
	wantCode(t, e2.send(maintainer, "PUT", target, `{"repoSkills":["a"]}`), "set", http.StatusOK)
	wantCode(t, e2.send(maintainer, "POST", target+"/reset", ""), "reset", http.StatusOK)
	if r, _ := e2.storedRepo("main", "acme/gadgets"); r.RepoSkills != nil {
		t.Errorf("after a reset stored = %v", r.RepoSkills)
	}
	var detail []string
	for _, a := range e2.auditEntries() {
		if a.Action == "settings_updated" {
			detail = append(detail, a.Detail)
		}
	}
	if len(detail) == 0 || detail[len(detail)-1] != "repoSkills" {
		t.Errorf("settings_updated details = %q; want repoSkills named", detail)
	}
}

func anyStrings(v any) []string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			out = append(out, x.(string))
		}
	case []string:
		out = l
	}
	return out
}

func TestDefaultsSkillsRoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("a", "x")
	e.skill("b", "y")
	admin := e.signInGitLab(true, "acme/widgets")

	v := decode[DefaultsView](t, e.send(admin, "PUT", "/api/admin/defaults", `{"skills":["b","a","b"]}`), "PUT skills", http.StatusOK)
	if !slices.Equal(v.Skills, []string{"b", "a"}) {
		t.Errorf("defaults skills = %v; want b, a", v.Skills)
	}
	if got := getJSON[DefaultsView](e, admin, "/api/admin/defaults"); !slices.Equal(got.Skills, []string{"b", "a"}) {
		t.Errorf("GET = %v", got.Skills)
	}
	if s := getJSON[AdminSettingsView](e, admin, "/api/admin/settings"); !slices.Equal(s.Defaults.Skills, []string{"b", "a"}) {
		t.Errorf("admin settings = %v", s.Defaults.Skills)
	}

	// A save that leaves skills out keeps the list.
	v = decode[DefaultsView](t, e.send(admin, "PUT", "/api/admin/defaults", `{"approve":false}`), "PUT approve", http.StatusOK)
	if v.Approve || !slices.Equal(v.Skills, []string{"b", "a"}) {
		t.Errorf("after a save without skills: %+v; want the list kept", v)
	}
	// null and [] clear it.
	v = decode[DefaultsView](t, e.send(admin, "PUT", "/api/admin/defaults", `{"skills":null}`), "PUT null", http.StatusOK)
	if len(v.Skills) != 0 || v.Skills == nil {
		t.Errorf("after null: %#v; want an empty list", v.Skills)
	}
	wantCode(t, e.send(admin, "PUT", "/api/admin/defaults", `{"skills":["a"]}`), "set", http.StatusOK)
	v = decode[DefaultsView](t, e.send(admin, "PUT", "/api/admin/defaults", `{"skills":[]}`), "PUT []", http.StatusOK)
	if len(v.Skills) != 0 {
		t.Errorf("after []: %v", v.Skills)
	}

	// The bad ones.
	rec := e.send(admin, "PUT", "/api/admin/defaults", `{"skills":["a","gone"]}`)
	wantCode(t, rec, "PUT unknown", http.StatusBadRequest)
	if want := `skill "gone" is not in the skill library`; errorOf(t, rec) != want {
		t.Errorf("error = %q; want %q", errorOf(t, rec), want)
	}
	wantCode(t, e.send(admin, "PUT", "/api/admin/defaults", `{"skills":"a"}`), "PUT a string", http.StatusBadRequest)
	big := strings.Repeat("x", 600*1024)
	e.skill("big1", big)
	e.skill("big2", big)
	rec = e.send(admin, "PUT", "/api/admin/defaults", `{"skills":["big1","big2"]}`)
	wantCode(t, rec, "PUT over budget", http.StatusBadRequest)
	if want := "the library skills come to 1200 KiB; a review takes at most 900 KiB"; errorOf(t, rec) != want {
		t.Errorf("error = %q; want %q", errorOf(t, rec), want)
	}

	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "defaults_updated" {
			details = append(details, a.Detail)
		}
	}
	slices.Reverse(details)
	if want := []string{"skills", "approve", "approve, skills", "skills", "skills"}; !slices.Equal(details, want) {
		t.Errorf("defaults_updated details = %q; want %q", details, want)
	}

	// Only admins.
	viewer := newEnv(t, nil)
	vc := viewer.signInGitLab(false, "acme/widgets")
	wantCode(t, viewer.send(vc, "PUT", "/api/admin/defaults", `{"skills":[]}`), "viewer PUT", http.StatusForbidden)
}

func TestInputsViewDetectedAndFinal(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) {
		cfg.Repositories[0].Skills = []string{"a", "gone"}
		cfg.Repositories[0].RepoSkills = []string{"claude-one", "nope"}
	})
	e.skill("a", "Library a.")
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/widgets"), Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	e.src["main"].set(func(f *fakeSource) {
		f.files[".pergent/skills/team.md"] = "Team rules."
		f.files[".claude/skills/claude-one.md"] = "Claude one."
		f.files[".claude/skills/claude-two.md"] = "Claude two."
		f.files[".github/skills/broken.md"] = "---\npaths: 3\n---\nBroken."
	})
	c := e.signInGitLab(false, "acme/widgets", "acme/payments")

	v := getJSON[InputsView](e, c, "/api/repositories/main/acme/widgets/inputs")
	var got []string
	for _, s := range v.Skills {
		got = append(got, strings.Join([]string{s.Name, s.Source, s.Folder, strconv.Itoa(s.Version), s.Problem}, "|"))
	}
	want := []string{
		"a|library||1|",
		"claude-one|.claude/skills|.claude/skills|0|",
		`gone|||0|skill "gone" is not in the skill library`,
		`nope|||0|skill "nope" is turned on but not found in the repository`,
		"team|.pergent/skills|.pergent/skills|0|",
	}
	if !slices.Equal(got, want) {
		t.Errorf("skills = %q\nwant %q", got, want)
	}
	var det []string
	for _, d := range v.Detected {
		det = append(det, strings.Join([]string{d.Name, d.Folder, boolText(d.On), d.Problem}, "|"))
	}
	if len(det) != 3 || det[0] != "claude-one|.claude/skills|on|" || det[1] != "claude-two|.claude/skills|off|" ||
		!strings.HasPrefix(det[2], "broken|.github/skills|off|") || strings.HasSuffix(det[2], "|") {
		t.Errorf("detected = %q", det)
	}

	// A repository that names no skills reviews the default list.
	e.skill("def", "Default text.")
	if err := e.st.PutRepoDefaults(ctx, store.RepoDefaults{Skills: []string{"def"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/payments"), Path: "acme/payments"}); err != nil {
		t.Fatal(err)
	}
	pv := getJSON[InputsView](e, c, "/api/repositories/main/acme/payments/inputs")
	// (The fake repository's folders are the same for both projects.)
	if len(pv.Skills) != 2 || pv.Skills[0].Name != "def" || pv.Skills[0].Version != 1 || pv.Skills[1].Name != "team" {
		t.Errorf("payments skills = %+v; want the default list and the repository's own", pv.Skills)
	}
}

func boolText(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// A change of the defaults, or of a repository's skills, shows at once in
// the view, though the view is kept 5 minutes.
func TestInputsCacheClearedOnDefaultsChange(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("a", "Text a.")
	e.skill("b", "Text b.")
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/gadgets"), Path: "acme/gadgets"}); err != nil {
		t.Fatal(err)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	if err := e.st.PutRepoDefaults(ctx, store.RepoDefaults{Skills: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	admin := e.signInGitLab(true, "acme/gadgets")
	names := func() string {
		v := getJSON[InputsView](e, admin, "/api/repositories/main/acme/gadgets/inputs")
		var out []string
		for _, s := range v.Skills {
			out = append(out, s.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(); got != "a" {
		t.Fatalf("skills = %s; want a", got)
	}
	wantCode(t, e.send(admin, "PUT", "/api/admin/defaults", `{"skills":["b"]}`), "PUT defaults", http.StatusOK)
	if got := names(); got != "b" {
		t.Errorf("after the defaults changed, skills = %s; want b", got)
	}
	wantCode(t, e.send(admin, "PUT", "/api/repositories/main/acme/gadgets/settings", `{"skills":["a","b"]}`), "PUT settings", http.StatusOK)
	if got := names(); got != "a,b" {
		t.Errorf("after the repository's skills changed, skills = %s; want a,b", got)
	}
}

// A read that began before a change isn't kept, and a request after the
// change doesn't share it.
func TestInputsCacheInvalidateDropsInFlight(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("a", "Text a.")
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/widgets"), Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	src := e.src["main"]
	gate, entered := make(chan struct{}), make(chan struct{}, 10)
	src.set(func(f *fakeSource) { f.gate, f.entered = gate, entered })
	c := e.signInGitLab(false, "acme/widgets")

	done := make(chan int)
	go func() { done <- e.get(c, "/api/repositories/main/acme/widgets/inputs").Code }()
	<-entered
	e.d.inputs.invalidate()
	close(gate)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	e.d.inputs.mu.Lock()
	kept := len(e.d.inputs.entries)
	e.d.inputs.mu.Unlock()
	if kept != 0 {
		t.Errorf("%d views kept from a read that began before the change", kept)
	}
	calls := src.callCount()
	getJSON[InputsView](e, c, "/api/repositories/main/acme/widgets/inputs")
	if src.callCount() == calls {
		t.Error("a request after the change used the earlier read")
	}
}

// A repository skill that is turned on but malformed is shown with its
// problem, and the Detected list stays, so a maintainer can still see the
// switch and turn it off. The engine fails the run on it; so would a review.
func TestInputsViewTurnedOnMalformedSkill(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) {
		cfg.Repositories[0].Skills = []string{"a"}
		cfg.Repositories[0].RepoSkills = []string{"claude-bad", "claude-ok"}
	})
	e.skill("a", "Library a.")
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/widgets"), Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	e.src["main"].set(func(f *fakeSource) {
		f.files[".claude/skills/claude-bad.md"] = "---\npaths: 3\n---\nBroken."
		f.files[".claude/skills/claude-ok.md"] = "Fine."
	})
	c := e.signInGitLab(false, "acme/widgets")
	target := "/api/repositories/main/acme/widgets/inputs"

	v := getJSON[InputsView](e, c, target)
	var got []string
	for _, s := range v.Skills {
		got = append(got, s.Name+"|"+s.Source+"|"+s.Content+"|"+s.Problem)
	}
	if len(got) != 3 || got[0] != "a|library|Library a.|" || got[2] != "claude-ok|.claude/skills|Fine.|" ||
		!strings.HasPrefix(got[1], "claude-bad|||.claude/skills/claude-bad.md: ") {
		t.Errorf("skills = %q", got)
	}
	var det []string
	for _, d := range v.Detected {
		det = append(det, d.Name+"|"+boolText(d.On)+"|"+strconv.FormatBool(d.Problem != ""))
	}
	if !slices.Equal(det, []string{"claude-bad|on|true", "claude-ok|on|false"}) {
		t.Errorf("detected = %q; want both listed and on", det)
	}

	// A broken ".pergent/skills" file runs always and is no switch: the view
	// fails as the review does.
	e.src["main"].set(func(f *fakeSource) { f.files[".pergent/skills/broken.md"] = "---\npaths: 3\n---\nBroken." })
	e.clock.advance(6 * time.Minute)
	e.wantStatus(c, target, http.StatusBadGateway)
}

// Clearing a list never meets the budget: a nil list chooses no skills, and
// the store would read it as "the default list" if it were asked. A default
// list that grew over the budget by edits can be cleared, and so can a
// repository's own list, back to the default list.
func TestClearingSkillsIgnoresBudget(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("a", "x")
	e.skill("b", "y")
	e.dashboardRepo("main", "acme/gadgets", nil)
	admin := e.signInGitLab(true, "acme/gadgets")
	overBudget := func() {
		t.Helper()
		if err := e.st.PutRepoDefaults(ctx, store.RepoDefaults{Skills: []string{"a", "b"}}); err != nil {
			t.Fatal(err)
		}
		// Edits grew the two skills past the budget since the list was saved.
		e.exec(`UPDATE library_skills SET content = repeat('x', 600 * 1024)`)
	}

	for _, body := range []string{`{"skills":null}`, `{"skills":[]}`} {
		overBudget()
		// Saving the same over-budget list is still refused.
		wantCode(t, e.send(admin, "PUT", "/api/admin/defaults", `{"skills":["a","b"]}`), "PUT the over-budget list", http.StatusBadRequest)
		wantCode(t, e.send(admin, "PUT", "/api/admin/defaults", body), "PUT "+body, http.StatusOK)
		if d, _ := e.st.RepoDefaults(ctx); len(d.Skills) != 0 {
			t.Errorf("after %s the defaults hold %v", body, d.Skills)
		}
	}

	// A repository's null skills (the default list) is not budget-checked
	// either: it picks nothing, and the default list was checked when it was
	// saved. A maintainer is never locked out of going back to the defaults.
	overBudget()
	target := "/api/repositories/main/acme/gadgets/settings"
	wantCode(t, e.send(admin, "PUT", target, `{"skills":["a","b"]}`), "PUT the over-budget repository list", http.StatusBadRequest)
	e.exec(`UPDATE library_skills SET content = 'x'`)
	wantCode(t, e.send(admin, "PUT", target, `{"skills":["a"]}`), "set", http.StatusOK)
	overBudget()
	wantCode(t, e.send(admin, "PUT", target, `{"skills":null}`), "PUT null", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.Skills != nil {
		t.Errorf("repository skills = %v after null", r.Skills)
	}
}

// --- the library writes ---

const (
	msgSecretInSkill = "the skill contains a secret pergent.sh holds"
	msgStaleSkill    = "this skill changed since you opened it"
	msgInstalledEdit = "an installed skill can't be edited; copy it"
)

type skillWritten struct {
	Version int `json:"version"`
}

// storedSkill is the skill as the store holds it; found is false when it is
// not in the library.
func (e *env) storedSkill(name string) (sk store.LibrarySkill, found bool) {
	e.t.Helper()
	sk, _, err := e.st.LibrarySkill(ctx, name)
	if errors.Is(err, store.ErrSkillNotFound) {
		return store.LibrarySkill{}, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return sk, true
}

func (e *env) libraryCount() int {
	e.t.Helper()
	list, err := e.st.LibrarySkills(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(list)
}

func (e *env) defaultSkills() []string {
	e.t.Helper()
	d, err := e.st.RepoDefaults(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return d.Skills
}

func TestSkillWritesNeedAdmin(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("style", "Style text.")
	viewer := e.signInRole(store.RoleViewer)
	e2 := newEnv(t, nil)
	e2.skill("style", "Style text.")
	maintainer := e2.signInRole(store.RoleMaintainer)

	for _, tc := range []struct {
		who *env
		c   *http.Cookie
	}{{e, viewer}, {e2, maintainer}} {
		for _, w := range []struct{ method, target, body string }{
			{"POST", "/api/admin/skills", `{"name":"new","content":"Text."}`},
			{"PUT", "/api/admin/skills/style", `{"content":"Changed.","version":1}`},
			{"POST", "/api/admin/skills/style/copy", `{"name":"copy"}`},
			{"DELETE", "/api/admin/skills/style", ``},
		} {
			wantCode(t, tc.who.send(tc.c, w.method, w.target, w.body), w.method+" "+w.target, http.StatusForbidden)
		}
		if n := tc.who.libraryCount(); n != 1 {
			t.Errorf("library holds %d skills after refused writes; want 1", n)
		}
		if sk, _ := tc.who.storedSkill("style"); sk.Version != 1 {
			t.Errorf("style is at version %d after refused writes", sk.Version)
		}
	}
	// No session at all.
	req := httptest.NewRequest("POST", "/api/admin/skills", strings.NewReader(`{"name":"new","content":"Text."}`))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	wantCode(t, rec, "POST without a session", http.StatusUnauthorized)
}

// The same authenticated request is refused without the CSRF token and
// goes through with it.
func TestSkillWritesNeedCSRF(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("style", "Style text.")
	e.skill("gone", "Gone text.")
	admin := e.signInGitLab(true, "acme/widgets")
	for _, w := range []struct {
		method, target, body string
		ok                   int
	}{
		{"POST", "/api/admin/skills", `{"name":"new","content":"Text."}`, http.StatusCreated},
		{"DELETE", "/api/admin/skills/gone", ``, http.StatusNoContent},
		{"POST", "/api/admin/skills/style/copy", `{"name":"copy"}`, http.StatusCreated},
		{"PUT", "/api/admin/skills/style", `{"content":"Changed.","version":1}`, http.StatusOK},
	} {
		req := httptest.NewRequest(w.method, w.target, strings.NewReader(w.body))
		req.AddCookie(admin)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		wantCode(t, rec, w.method+" "+w.target+" without CSRF", http.StatusForbidden)
		if n := e.libraryCount(); n != 2 {
			t.Fatalf("library holds %d skills after %s %s without a CSRF token; want 2", n, w.method, w.target)
		}
		if sk, _ := e.storedSkill("style"); sk.Version != 1 || sk.Content != "Style text." {
			t.Fatalf("style = %+v after %s %s without a CSRF token", sk, w.method, w.target)
		}
		wantCode(t, e.send(admin, w.method, w.target, w.body), w.method+" "+w.target+" with CSRF", w.ok)
		// Put the library back for the next write.
		switch w.method + " " + w.target {
		case "POST /api/admin/skills":
			wantCode(t, e.send(admin, "DELETE", "/api/admin/skills/new", ""), "undo", http.StatusNoContent)
		case "DELETE /api/admin/skills/gone":
			e.skill("gone", "Gone text.")
		case "POST /api/admin/skills/style/copy":
			wantCode(t, e.send(admin, "DELETE", "/api/admin/skills/copy", ""), "undo", http.StatusNoContent)
		}
	}
}

// The secret check fails closed: when the held secrets can't be read, no
// create, edit or copy is saved, and the answer says nothing of why.
func TestHeldSecretsErrorRefusesSave(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("style", "Style text.")
	admin := e.signInGitLab(true, "acme/widgets")
	e.d.heldSecrets = func(context.Context) ([]string, error) {
		return nil, errors.New("held secrets: instance \"lab\": the store is down")
	}
	gen := e.d.inputs.generation()
	for _, w := range []struct {
		what, method, target string
		body                 any
	}{
		{"create", "POST", "/api/admin/skills", map[string]any{"name": "new", "content": "Text.", "addToDefaults": true}},
		{"update", "PUT", "/api/admin/skills/style", map[string]any{"content": "Changed.", "version": 1}},
		{"copy", "POST", "/api/admin/skills/style/copy", map[string]any{"name": "copy"}},
	} {
		rec := e.sendJSON(admin, w.method, w.target, w.body)
		wantCode(t, rec, w.what, http.StatusInternalServerError)
		if got := errorOf(t, rec); got != msgWriteFailed {
			t.Errorf("%s: error = %q; want the generic %q", w.what, got, msgWriteFailed)
		}
		if strings.Contains(rec.Body.String(), "lab") || strings.Contains(rec.Body.String(), "store") {
			t.Errorf("%s: the answer names why: %s", w.what, rec.Body)
		}
	}
	if n := e.libraryCount(); n != 1 {
		t.Errorf("library holds %d skills; want 1, nothing saved", n)
	}
	if sk, _ := e.storedSkill("style"); sk.Version != 1 || sk.Content != "Style text." {
		t.Errorf("style = %+v; want unchanged", sk)
	}
	if d := e.defaultSkills(); len(d) != 0 {
		t.Errorf("defaults = %v; want unchanged", d)
	}
	for _, a := range e.auditEntries() {
		if strings.HasPrefix(a.Action, "skill_") || a.Action == "defaults_updated" {
			t.Errorf("a refused write was audited: %+v", a)
		}
	}
	if e.d.inputs.generation() != gen {
		t.Error("a refused write invalidated the inputs views")
	}
	// A delete holds no text to check and still works.
	wantCode(t, e.send(admin, "DELETE", "/api/admin/skills/style", ""), "delete", http.StatusNoContent)
}

func TestCreateSkill(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("existing", "Existing text.")
	if err := e.st.PutRepoDefaults(ctx, store.RepoDefaults{Skills: []string{"existing"}}); err != nil {
		t.Fatal(err)
	}
	admin := e.signInGitLab(true, "acme/widgets")

	got := decode[skillWritten](t, e.sendJSON(admin, "POST", "/api/admin/skills",
		map[string]any{"name": "plain", "content": "Plain text."}), "POST plain", http.StatusCreated)
	if got.Version != 1 {
		t.Errorf("version = %d; want 1", got.Version)
	}
	sk, found := e.storedSkill("plain")
	if !found || sk.Content != "Plain text." || sk.Source != nil || sk.UpdatedBy != "alice (gitlab main)" {
		t.Errorf("plain = %+v, %v; want written in the dashboard by alice", sk, found)
	}
	if d := e.defaultSkills(); !slices.Equal(d, []string{"existing"}) {
		t.Errorf("defaults = %v after a create without addToDefaults; want unchanged", d)
	}

	decode[skillWritten](t, e.sendJSON(admin, "POST", "/api/admin/skills",
		map[string]any{"name": "wanted", "content": "Wanted text.", "addToDefaults": true}), "POST wanted", http.StatusCreated)
	if d := e.defaultSkills(); !slices.Equal(d, []string{"existing", "wanted"}) {
		t.Errorf("defaults = %v; want existing, wanted", d)
	}
	if got := getJSON[[]LibrarySkillView](e, admin, "/api/skills"); !got[slices.IndexFunc(got, func(v LibrarySkillView) bool { return v.Name == "wanted" })].InDefaults {
		t.Error("wanted is not marked as a default")
	}

	// A taken name, whatever the origin of the one that holds it.
	e.installedSkill("taken", "Taken text.")
	for _, name := range []string{"plain", "taken"} {
		rec := e.sendJSON(admin, "POST", "/api/admin/skills", map[string]any{"name": name, "content": "Other."})
		wantCode(t, rec, "POST "+name, http.StatusConflict)
		if errorOf(t, rec) != "name already in the library" {
			t.Errorf("error = %q", errorOf(t, rec))
		}
	}
	if sk, _ := e.storedSkill("taken"); sk.Content != "Taken text." {
		t.Error("a refused create changed the installed skill")
	}

	// A skill as big as one may be, and a body that needs a lot of escaping.
	big := strings.Repeat("x\n", inputs.MaxSkillBytes/2)
	decode[skillWritten](t, e.sendJSON(admin, "POST", "/api/admin/skills",
		map[string]any{"name": "big", "content": big}), "POST the biggest skill", http.StatusCreated)

	// Bad bodies.
	for _, body := range []string{``, `{}`, `[]`, `{"name":"x"}`, `{"content":"y"}`, `{"name":"x","content":"y","extra":1}`,
		`{"name":"x","content":5}`, `{"name":"x","content":"y"} {}`} {
		wantCode(t, e.send(admin, "POST", "/api/admin/skills", body), "POST "+body, http.StatusBadRequest)
	}
	// A skill with no text tells a review nothing.
	for _, content := range []string{"", " \n\t "} {
		rec := e.sendJSON(admin, "POST", "/api/admin/skills", map[string]any{"name": "blank", "content": content})
		wantCode(t, rec, "POST blank text", http.StatusBadRequest)
		if errorOf(t, rec) != "the skill has no text" {
			t.Errorf("error = %q", errorOf(t, rec))
		}
	}
	if _, found := e.storedSkill("blank"); found {
		t.Error("a blank skill was created")
	}
}

func TestCreateChecks(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")
	for _, tc := range []struct{ what, name, content string }{
		{"a name with a slash", "a/b", "Text."},
		{"a name starting with a dot", ".hidden", "Text."},
		{"an empty name", "", "Text."},
		{"a skill over 128 KiB", "huge", strings.Repeat("x", inputs.MaxSkillBytes+1)},
		{"frontmatter that does not parse", "broken", "---\nname: [unclosed\n---\nText."},
		{"paths that is not a list", "paths", "---\npaths: src\n---\nText."},
		{"cross_batch that is not a boolean", "cross", "---\ncross_batch: maybe\n---\nText."},
		{"a frontmatter name that differs", "style", "---\nname: other\n---\nText."},
	} {
		want := inputs.CheckSkill(tc.name, []byte(tc.content))
		if want == nil {
			t.Fatalf("%s: CheckSkill accepts it; the test is wrong", tc.what)
		}
		rec := e.sendJSON(admin, "POST", "/api/admin/skills", map[string]any{"name": tc.name, "content": tc.content})
		wantCode(t, rec, "POST "+tc.what, http.StatusBadRequest)
		if got := errorOf(t, rec); got != want.Error() {
			t.Errorf("%s: error = %q; want %q", tc.what, got, want)
		}
	}
	if n := e.libraryCount(); n != 0 {
		t.Errorf("library holds %d skills after refused creates", n)
	}

	// The library holds at most 200.
	for i := 0; i < inputs.MaxLibrarySkills; i++ {
		e.skill(fmt.Sprintf("s%03d", i), "Text.")
	}
	rec := e.sendJSON(admin, "POST", "/api/admin/skills", map[string]any{"name": "one-more", "content": "Text."})
	wantCode(t, rec, "POST into a full library", http.StatusBadRequest)
	if want := "the library holds 200 skills, the most it can"; errorOf(t, rec) != want {
		t.Errorf("error = %q; want %q", errorOf(t, rec), want)
	}
}

// Adding a skill to the default list on create keeps to the budget like any
// save of the list: the create is refused whole, nothing is written, and the
// same skill goes in without the box ticked.
func TestCreateAddToDefaultsBudget(t *testing.T) {
	e := newEnv(t, nil)
	big := strings.Repeat("x", inputs.MaxSkillBytes)
	var names []string
	for i := 0; i < 7; i++ { // 7 x 128 KiB = 896 KiB, under 900
		name := fmt.Sprintf("big%d", i)
		e.skill(name, big)
		names = append(names, name)
	}
	if err := e.st.PutRepoDefaults(ctx, store.RepoDefaults{Skills: names}); err != nil {
		t.Fatal(err)
	}
	admin := e.signInGitLab(true, "acme/widgets")
	gen := e.d.inputs.generation()

	rec := e.sendJSON(admin, "POST", "/api/admin/skills", map[string]any{"name": "one-more", "content": big, "addToDefaults": true})
	wantCode(t, rec, "POST over the budget", http.StatusBadRequest)
	if want := "the library skills come to 1024 KiB; a review takes at most 900 KiB"; errorOf(t, rec) != want {
		t.Errorf("error = %q; want %q", errorOf(t, rec), want)
	}
	if _, found := e.storedSkill("one-more"); found {
		t.Error("the skill was created though the defaults would go over the budget")
	}
	if d := e.defaultSkills(); !slices.Equal(d, names) {
		t.Errorf("defaults = %v; want unchanged", d)
	}
	if e.d.inputs.generation() != gen {
		t.Error("a refused create invalidated the inputs views")
	}
	wantCode(t, e.sendJSON(admin, "POST", "/api/admin/skills", map[string]any{"name": "one-more", "content": big}),
		"POST without addToDefaults", http.StatusCreated)
	if d := e.defaultSkills(); !slices.Equal(d, names) {
		t.Errorf("defaults = %v after a create that left the box unticked", d)
	}
}

func TestSaveRefusesHeldSecret(t *testing.T) {
	e := newEnv(t, nil)
	op := e.operator()
	e.addLab(op)
	e.sendJSON(op, "POST", "/api/admin/instances", hubBody(nil))
	wantCode(t, e.sendJSON(op, "PUT", "/api/admin/llm", map[string]any{"provider": "anthropic", "model": "claude-sonnet-4", "apiKey": llmKeyOne}),
		"PUT llm", http.StatusOK)
	e.skill("style", "Style text.")
	e.installedSkill("remote", "Remote text.")
	admin := e.signInGitLab(true, "acme/widgets")

	for _, secret := range []string{labReviewToken, labApproveToken, labOAuthSecret, hubReviewKey, hubApproveKey, hubWebhook, llmKeyOne} {
		content := "Review with this: " + secret + " and nothing else."
		for _, w := range []struct {
			what, method, target string
			body                 any
		}{
			{"create", "POST", "/api/admin/skills", map[string]any{"name": "leak", "content": content}},
			{"create with a frontmatter name", "POST", "/api/admin/skills", map[string]any{"name": "leak", "content": "---\nname: leak\n---\n" + content}},
			{"update", "PUT", "/api/admin/skills/style", map[string]any{"content": content, "version": 1}},
		} {
			rec := e.sendJSON(admin, w.method, w.target, w.body)
			wantCode(t, rec, w.what, http.StatusBadRequest)
			if errorOf(t, rec) != msgSecretInSkill {
				t.Errorf("%s: error = %q; want %q", w.what, errorOf(t, rec), msgSecretInSkill)
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("%s: the response holds the secret", w.what)
			}
		}
	}
	// A copy of a skill that holds a secret now is refused too: a secret set
	// after the skill was saved is held all the same.
	if _, err := e.st.UpdateLibrarySkill(ctx, "style", "Now with "+labReviewToken+".", "seed", 1, e.clock.now()); err != nil {
		t.Fatal(err)
	}
	rec := e.sendJSON(admin, "POST", "/api/admin/skills/style/copy", map[string]any{"name": "style-copy"})
	wantCode(t, rec, "copy", http.StatusBadRequest)
	if errorOf(t, rec) != msgSecretInSkill {
		t.Errorf("copy: error = %q", errorOf(t, rec))
	}

	if _, found := e.storedSkill("leak"); found {
		t.Error("a skill with a secret was created")
	}
	if _, found := e.storedSkill("style-copy"); found {
		t.Error("a copy with a secret was created")
	}
	for _, secret := range []string{labReviewToken, labApproveToken, labOAuthSecret, hubReviewKey, hubApproveKey, hubWebhook, llmKeyOne, "hub-review-key-SECRET-444"} {
		if strings.Contains(e.logs.String(), secret) {
			t.Errorf("the log holds %q", secret)
		}
		for _, a := range e.auditEntries() {
			if strings.Contains(a.Detail+a.Project+a.Instance+a.Action, secret) {
				t.Errorf("an audit entry holds %q: %+v", secret, a)
			}
		}
	}
	for _, a := range e.auditEntries() {
		if strings.HasPrefix(a.Action, "skill_") {
			t.Errorf("a refused write was audited: %+v", a)
		}
	}
}

func TestUpdateSkill(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("style", "First.")
	admin := e.signInGitLab(true, "acme/widgets")

	got := decode[skillWritten](t, e.sendJSON(admin, "PUT", "/api/admin/skills/style",
		map[string]any{"content": "Second.", "version": 1}), "PUT", http.StatusOK)
	if got.Version != 2 {
		t.Errorf("version = %d; want 2", got.Version)
	}
	sk, _ := e.storedSkill("style")
	if sk.Content != "Second." || sk.Version != 2 || sk.UpdatedBy != "alice (gitlab main)" {
		t.Errorf("style = %+v", sk)
	}

	for _, tc := range []struct {
		what, target, body string
		code               int
	}{
		{"a check failure", "/api/admin/skills/style", `{"content":"---\nname: other\n---\n","version":2}`, http.StatusBadRequest},
		{"no version", "/api/admin/skills/style", `{"content":"Third."}`, http.StatusBadRequest},
		{"version 0", "/api/admin/skills/style", `{"content":"Third.","version":0}`, http.StatusBadRequest},
		{"no content", "/api/admin/skills/style", `{"version":2}`, http.StatusBadRequest},
		{"an unknown field", "/api/admin/skills/style", `{"content":"Third.","version":2,"name":"x"}`, http.StatusBadRequest},
		{"an unknown skill", "/api/admin/skills/nope", `{"content":"Third.","version":1}`, http.StatusNotFound},
		{"an invalid name", "/api/admin/skills/.hidden", `{"content":"Third.","version":1}`, http.StatusNotFound},
	} {
		wantCode(t, e.send(admin, "PUT", tc.target, tc.body), "PUT with "+tc.what, tc.code)
	}
	if sk, _ := e.storedSkill("style"); sk.Version != 2 || sk.Content != "Second." {
		t.Errorf("style = %+v after refused edits", sk)
	}
}

func TestUpdateStale409(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("style", "First.")
	admin := e.signInGitLab(true, "acme/widgets")
	if _, err := e.st.UpdateLibrarySkill(ctx, "style", "Second, by someone else.", "bob", 1, e.clock.now()); err != nil {
		t.Fatal(err)
	}
	gen := e.d.inputs.generation()
	rec := e.sendJSON(admin, "PUT", "/api/admin/skills/style", map[string]any{"content": "Mine.", "version": 1})
	wantCode(t, rec, "PUT a stale version", http.StatusConflict)
	if errorOf(t, rec) != msgStaleSkill {
		t.Errorf("error = %q; want %q", errorOf(t, rec), msgStaleSkill)
	}
	if sk, _ := e.storedSkill("style"); sk.Content != "Second, by someone else." || sk.Version != 2 {
		t.Errorf("style = %+v; want the other admin's text kept", sk)
	}
	if e.d.inputs.generation() != gen {
		t.Error("a refused edit invalidated the inputs views")
	}
}

func TestUpdateInstalled409(t *testing.T) {
	e := newEnv(t, nil)
	e.installedSkill("remote", "Remote text.")
	admin := e.signInGitLab(true, "acme/widgets")
	rec := e.sendJSON(admin, "PUT", "/api/admin/skills/remote", map[string]any{"content": "Mine.", "version": 1})
	wantCode(t, rec, "PUT an installed skill", http.StatusConflict)
	if errorOf(t, rec) != msgInstalledEdit {
		t.Errorf("error = %q; want %q", errorOf(t, rec), msgInstalledEdit)
	}
	if sk, _ := e.storedSkill("remote"); sk.Content != "Remote text." || sk.Version != 1 {
		t.Errorf("remote = %+v", sk)
	}
}

func TestCopySetsFrontmatterName(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("base", "---\nname: base\npaths:\n  - src/**\ncross_batch: true\n---\nBody of base.\n")
	e.skill("bare", "No frontmatter at all.")
	e.skill("anonymous", "---\npaths:\n  - docs/**\n---\nNo name here.\n")
	e.installedSkill("remote", "---\nname: remote\n---\nRemote body.\n")
	admin := e.signInGitLab(true, "acme/widgets")

	copyOf := func(from, to string) *httptest.ResponseRecorder {
		return e.sendJSON(admin, "POST", "/api/admin/skills/"+from+"/copy", map[string]any{"name": to})
	}
	got := decode[skillWritten](t, copyOf("base", "base-2"), "copy base", http.StatusCreated)
	if got.Version != 1 {
		t.Errorf("version = %d; want 1", got.Version)
	}
	sk, found := e.storedSkill("base-2")
	want := "---\nname: \"base-2\"\npaths:\n  - src/**\ncross_batch: true\n---\nBody of base.\n"
	if !found || sk.Content != want || sk.Source != nil || sk.UpdatedBy != "alice (gitlab main)" {
		t.Errorf("base-2 = %+v, %v; want %q written in the dashboard", sk, found, want)
	}
	if err := inputs.CheckSkill("base-2", []byte(sk.Content)); err != nil {
		t.Errorf("the copy fails the skill checks: %v", err)
	}
	if orig, _ := e.storedSkill("base"); orig.Version != 1 || !strings.Contains(orig.Content, "name: base\n") {
		t.Errorf("the original changed: %+v", orig)
	}

	// A name that looks like a number or a boolean stays a string.
	decode[skillWritten](t, copyOf("base", "1.5"), "copy to 1.5", http.StatusCreated)
	sk, _ = e.storedSkill("1.5")
	if err := inputs.CheckSkill("1.5", []byte(sk.Content)); err != nil {
		t.Errorf("copy to 1.5 fails the checks: %v", err)
	}

	// Without a frontmatter name nothing is added; an installed skill copies
	// into an editable one.
	decode[skillWritten](t, copyOf("bare", "bare-2"), "copy bare", http.StatusCreated)
	if sk, _ := e.storedSkill("bare-2"); sk.Content != "No frontmatter at all." {
		t.Errorf("bare-2 = %q", sk.Content)
	}
	decode[skillWritten](t, copyOf("anonymous", "anonymous-2"), "copy anonymous", http.StatusCreated)
	if sk, _ := e.storedSkill("anonymous-2"); sk.Content != "---\npaths:\n  - docs/**\n---\nNo name here.\n" {
		t.Errorf("anonymous-2 = %q", sk.Content)
	}
	decode[skillWritten](t, copyOf("remote", "local"), "copy remote", http.StatusCreated)
	sk, _ = e.storedSkill("local")
	if sk.Source != nil || !strings.Contains(sk.Content, `name: "local"`) {
		t.Errorf("local = %+v; want an editable skill named local", sk)
	}
	decode[skillWritten](t, e.sendJSON(admin, "PUT", "/api/admin/skills/local", map[string]any{"content": "Edited.", "version": 1}),
		"edit the copy", http.StatusOK)

	for _, tc := range []struct {
		what, from, to string
		code           int
	}{
		{"a taken name", "base", "bare", http.StatusConflict},
		{"its own name", "base", "base", http.StatusConflict},
		{"an invalid name", "base", "a/b", http.StatusBadRequest},
		{"an empty name", "base", "", http.StatusBadRequest},
		{"an unknown skill", "nope", "fresh", http.StatusNotFound},
	} {
		wantCode(t, copyOf(tc.from, tc.to), "copy with "+tc.what, tc.code)
	}
	if rec := copyOf("base", "bare"); errorOf(t, rec) != "name already in the library" {
		t.Errorf("error = %q", errorOf(t, rec))
	}
	wantCode(t, e.send(admin, "POST", "/api/admin/skills/base/copy", `{"name":"x","extra":1}`), "copy with an unknown field", http.StatusBadRequest)
}

func TestDeleteSkill(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("style", "Style text.")
	admin := e.signInGitLab(true, "acme/widgets")

	wantCode(t, e.send(admin, "DELETE", "/api/admin/skills/style", ""), "DELETE", http.StatusNoContent)
	if _, found := e.storedSkill("style"); found {
		t.Error("style is still in the library")
	}
	// The history outlives the skill.
	if _, err := e.st.LibrarySkillVersion(ctx, "style", 1); err != nil {
		t.Errorf("version 1 of a deleted skill: %v", err)
	}
	wantCode(t, e.send(admin, "DELETE", "/api/admin/skills/style", ""), "DELETE again", http.StatusNotFound)
	wantCode(t, e.send(admin, "DELETE", "/api/admin/skills/.hidden", ""), "DELETE an invalid name", http.StatusNotFound)
}

func TestDeleteInUse409HidesUnseenRepos(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) {
		cfg.Repositories[0].Skills = []string{"x"} // main acme/widgets: visible
		cfg.Repositories[1].Skills = []string{"x"} // main acme/payments: not visible
	})
	e.skill("x", "Text x.")
	e.skill("y", "Text y.")
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) { r.Skills = []string{"x"} }) // visible
	e.dashboardRepo("main", "acme/hidden", func(r *store.DashboardRepo) { r.Skills = []string{"x"} })  // not visible
	e.dashboardRepo("gh", "acme/other", func(r *store.DashboardRepo) { r.Skills = []string{"y"} })     // names another skill
	if err := e.st.PutRepoDefaults(ctx, store.RepoDefaults{Skills: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	admin := e.signInGitLab(true, "acme/widgets", "acme/gadgets")
	gen := e.d.inputs.generation()

	type inUse struct {
		Error        string `json:"error"`
		Repositories []struct {
			Instance string `json:"instance"`
			Project  string `json:"project"`
		} `json:"repositories"`
		Hidden     int  `json:"hidden"`
		InDefaults bool `json:"inDefaults"`
	}
	got := decode[inUse](t, e.send(admin, "DELETE", "/api/admin/skills/x", ""), "DELETE a skill in use", http.StatusConflict)
	var seen []string
	for _, r := range got.Repositories {
		seen = append(seen, r.Instance+"/"+r.Project)
	}
	if !slices.Equal(seen, []string{"main/acme/gadgets", "main/acme/widgets"}) || got.Hidden != 2 || !got.InDefaults || got.Error == "" {
		t.Errorf("409 = %+v; want gadgets and widgets listed, 2 hidden, in the defaults", got)
	}
	raw := e.send(admin, "DELETE", "/api/admin/skills/x", "").Body.String()
	if strings.Contains(raw, "acme/hidden") || strings.Contains(raw, "acme/payments") {
		t.Errorf("the 409 names a repository the caller can't see: %s", raw)
	}
	if _, found := e.storedSkill("x"); !found {
		t.Error("a skill in use was deleted")
	}
	if e.d.inputs.generation() != gen {
		t.Error("a refused delete invalidated the inputs views")
	}

	// An operator sees them all.
	op := decode[inUse](t, e.send(e.operator(), "DELETE", "/api/admin/skills/x", ""), "operator DELETE", http.StatusConflict)
	if len(op.Repositories) != 4 || op.Hidden != 0 || !op.InDefaults {
		t.Errorf("operator's 409 = %+v; want 4 repositories, none hidden", op)
	}

	// Only the default list names it: an empty list and a count of 0.
	e2 := newEnv(t, nil)
	e2.skill("z", "Text z.")
	if err := e2.st.PutRepoDefaults(ctx, store.RepoDefaults{Skills: []string{"z"}}); err != nil {
		t.Fatal(err)
	}
	only := decode[inUse](t, e2.send(e2.signInGitLab(true, "acme/widgets"), "DELETE", "/api/admin/skills/z", ""), "DELETE", http.StatusConflict)
	if only.Repositories == nil || len(only.Repositories) != 0 || only.Hidden != 0 || !only.InDefaults {
		t.Errorf("409 = %+v; want [] repositories, none hidden, in the defaults", only)
	}
}

// A config repository on GitHub counts as a user of the skill too.
func TestDeleteInUseByGitHubConfigRepo(t *testing.T) {
	e := newEnv(t, func(cfg *config.Config) { cfg.Repositories[2].Skills = []string{"x"} })
	e.skill("x", "Text x.")
	admin := e.signInGitHub(true, "acme/widgets")
	var got struct {
		Repositories []struct{ Instance, Project string } `json:"repositories"`
	}
	rec := e.send(admin, "DELETE", "/api/admin/skills/x", "")
	wantCode(t, rec, "DELETE", http.StatusConflict)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Repositories) != 1 ||
		got.Repositories[0].Instance != "gh" || got.Repositories[0].Project != "acme/widgets" {
		t.Errorf("409 = %s (%v)", rec.Body, err)
	}
}

func TestSkillAuditHasNoContent(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")
	const marker = "DISTINCTIVE-SKILL-TEXT-MARKER"

	decode[skillWritten](t, e.sendJSON(admin, "POST", "/api/admin/skills",
		map[string]any{"name": "style", "content": marker + " one", "addToDefaults": true}), "create", http.StatusCreated)
	decode[skillWritten](t, e.sendJSON(admin, "PUT", "/api/admin/skills/style",
		map[string]any{"content": marker + " two", "version": 1}), "update", http.StatusOK)
	decode[skillWritten](t, e.sendJSON(admin, "POST", "/api/admin/skills/style/copy", map[string]any{"name": "style-2"}), "copy", http.StatusCreated)
	wantCode(t, e.send(admin, "DELETE", "/api/admin/skills/style-2", ""), "delete", http.StatusNoContent)

	detail := func(action string) string {
		entries := e.auditOf(action)
		if len(entries) != 1 {
			t.Fatalf("%s: %d audit entries; want 1", action, len(entries))
		}
		a := entries[0]
		if a.Actor != "alice (gitlab main)" || a.Instance != "" || a.Project != "" {
			t.Errorf("%s: entry = %+v; want alice, no instance or project (a platform admin sees it)", action, a)
		}
		return a.Detail
	}
	for action, want := range map[string]string{
		"skill_created": "style: v1", "skill_updated": "style: v1 -> v2", "skill_copied": "style-2: from style",
		"skill_deleted": "style-2", "defaults_updated": "skills",
	} {
		if got := detail(action); got != want {
			t.Errorf("%s detail = %q; want %q", action, got, want)
		}
	}
	for _, a := range e.auditEntries() {
		if strings.Contains(a.Detail+a.Action+a.Project+a.Instance, marker) {
			t.Errorf("an audit entry holds a skill's text: %+v", a)
		}
	}
	if strings.Contains(e.logs.String(), marker) {
		t.Error("the log holds a skill's text")
	}
	// A platform admin reads these entries.
	if got := getJSON[AuditPageView](e, admin, "/api/audit"); len(got.Entries) < 5 {
		t.Errorf("audit page = %+v", got)
	}
}

// Every write of the library, and the default list change a create makes,
// drops the kept inputs views: they list skills and their state.
func TestSkillWritesInvalidateInputs(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")
	check := func(what string, do func() *httptest.ResponseRecorder, code int, atLeast uint64) {
		t.Helper()
		before := e.d.inputs.generation()
		wantCode(t, do(), what, code)
		if got := e.d.inputs.generation() - before; got < atLeast {
			t.Errorf("%s: the inputs views were dropped %d times; want at least %d", what, got, atLeast)
		}
	}
	check("create", func() *httptest.ResponseRecorder {
		return e.sendJSON(admin, "POST", "/api/admin/skills", map[string]any{"name": "a", "content": "Text."})
	}, http.StatusCreated, 1)
	check("create into the defaults", func() *httptest.ResponseRecorder {
		return e.sendJSON(admin, "POST", "/api/admin/skills", map[string]any{"name": "b", "content": "Text.", "addToDefaults": true})
	}, http.StatusCreated, 2) // the new skill, then the default list
	check("update", func() *httptest.ResponseRecorder {
		return e.sendJSON(admin, "PUT", "/api/admin/skills/a", map[string]any{"content": "Text two.", "version": 1})
	}, http.StatusOK, 1)
	check("copy", func() *httptest.ResponseRecorder {
		return e.sendJSON(admin, "POST", "/api/admin/skills/a/copy", map[string]any{"name": "c"})
	}, http.StatusCreated, 1)
	check("delete", func() *httptest.ResponseRecorder { return e.send(admin, "DELETE", "/api/admin/skills/c", "") }, http.StatusNoContent, 1)
}

// A view kept before a write is not served after it.
func TestSkillWriteDropsKeptInputsView(t *testing.T) {
	e := newEnv(t, nil)
	e.skill("a", "Text a.")
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/widgets"), Path: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	c := e.signInGitLab(true, "acme/widgets")
	if err := e.st.PutRepoDefaults(ctx, store.RepoDefaults{Skills: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	names := func() string {
		v := getJSON[InputsView](e, c, "/api/repositories/main/acme/widgets/inputs")
		var out []string
		for _, s := range v.Skills {
			out = append(out, s.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(); got != "a" {
		t.Fatalf("skills = %s; want a", got)
	}
	decode[skillWritten](t, e.sendJSON(c, "POST", "/api/admin/skills", map[string]any{"name": "b", "content": "Text b.", "addToDefaults": true}),
		"create", http.StatusCreated)
	if got := names(); got != "a,b" {
		t.Errorf("after a create into the defaults, skills = %s; want a,b", got)
	}
}
