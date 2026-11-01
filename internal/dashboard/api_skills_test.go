package dashboard

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
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
	if strings.Contains(raw, "Alpha text.") || strings.Contains(raw, "acme/private-skills") {
		t.Errorf("the list holds a skill's text or its repository: %s", raw)
	}

	op := getJSON[[]LibrarySkillView](e, e.operator(), "/api/skills")
	if s := op[0].Source; s == nil || s.Repo != "acme/private-skills" || s.Hidden {
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
