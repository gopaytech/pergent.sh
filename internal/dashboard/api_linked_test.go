package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/links"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// linkInfo is what the fake platform says about a repository.
type linkInfo struct {
	vis  forge.Visibility
	inst int64
}

func (f *fakeCatalog) setInfo(repo string, vis forge.Visibility, inst int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.info == nil {
		f.info = map[string]linkInfo{}
	}
	f.info[strings.ToLower(repo)] = linkInfo{vis, inst}
}

func (f *fakeCatalog) failInfo(repo string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.infoErr == nil {
		f.infoErr = map[string]error{}
	}
	f.infoErr[strings.ToLower(repo)] = err
}

// okInfo undoes failInfo.
func (f *fakeCatalog) okInfo(repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.infoErr, strings.ToLower(repo))
}

func (f *fakeCatalog) infoCount(repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.infoCalls[strings.ToLower(repo)]
}

// LinkInfo makes the fake a RepoLinker. A repository it was told nothing about
// is one the review App can't read.
func (f *fakeCatalog) LinkInfo(ctx context.Context, repo string) (forge.Visibility, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.ToLower(repo)
	if f.infoCalls == nil {
		f.infoCalls = map[string]int{}
	}
	f.infoCalls[key]++
	if err := f.infoErr[key]; err != nil {
		return "", 0, err
	}
	i, ok := f.info[key]
	if !ok {
		return "", 0, forge.ErrSourceNotFound
	}
	return i.vis, i.inst, nil
}

var _ RepoLinker = (*fakeCatalog)(nil)

// linkWorld is a GitHub instance's platform: gadgets (private, installation 1)
// is the repository that links; the others are what it may link.
func linkWorld() *fakeCatalog {
	c := &fakeCatalog{}
	c.set("acme/gadgets", "acme/api", "acme/lib", "acme/secret-lib", "acme/other-app")
	c.setInfo("acme/gadgets", forge.VisPrivate, 1)
	c.setInfo("acme/api", forge.VisPublic, 1)
	c.setInfo("acme/lib", forge.VisInternal, 1)
	c.setInfo("acme/secret-lib", forge.VisPrivate, 1)
	c.setInfo("acme/other-app", forge.VisPublic, 2)
	return c
}

// linkDash is a dashboard that knows only the instance "gh"'s platform: enough
// for the rules of a save and the judging of a list, which read nothing else.
func linkDash(cat *fakeCatalog) *Dashboard {
	catalogs := syncmap.New[RepoCatalog](nil)
	catalogs.Set("gh", cat)
	return &Dashboard{catalogs: catalogs, Log: slog.New(slog.DiscardHandler), now: time.Now}
}

func ghRepo(project string, ls ...store.Link) settings.Effective {
	return settings.Effective{Instance: "gh", Project: project, Platform: "github", LinkedRepos: ls}
}

func link(target string, watched ...string) store.Link {
	return store.Link{Target: target, Watched: watched, CanBlock: true}
}

// --- no database ---

func TestLinkedReposSettingShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []store.Link // nil with ok false
		ok   bool
	}{
		{"null clears", `null`, nil, true},
		{"empty clears", `[]`, []store.Link{}, true},
		{"one link, canBlock left out is on", `[{"target":"acme/api","watched":["render/**"]}]`,
			[]store.Link{{Target: "acme/api", Watched: []string{"render/**"}, CanBlock: true}}, true},
		{"canBlock off, no watched paths", `[{"target":" acme/api ","canBlock":false}]`,
			[]store.Link{{Target: "acme/api", CanBlock: false}}, true},
		{"watched are trimmed", `[{"target":"acme/api","watched":[" a/** "]}]`,
			[]store.Link{{Target: "acme/api", Watched: []string{"a/**"}, CanBlock: true}}, true},
		{"not a list", `{"target":"acme/api"}`, nil, false},
		{"a string", `"acme/api"`, nil, false},
		{"an unknown field", `[{"target":"acme/api","note":"x"}]`, nil, false},
		{"canBlock is not a boolean", `[{"target":"acme/api","canBlock":"yes"}]`, nil, false},
		{"watched is not a list", `[{"target":"acme/api","watched":"a/**"}]`, nil, false},
		{"four links", `[{"target":"a/1"},{"target":"a/2"},{"target":"a/3"},{"target":"a/4"}]`, nil, false},
		{"too many watched paths", `[{"target":"acme/api","watched":[` + strings.TrimSuffix(strings.Repeat(`"a",`, maxWatched+1), ",") + `]}]`, nil, false},
		{"a very long watched path", `[{"target":"acme/api","watched":["` + strings.Repeat("a", maxWatchedLen+1) + `"]}]`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			null := tc.body == "null"
			edit, msg := linkedReposSetting(json.RawMessage(tc.body), null)
			if !tc.ok {
				if msg == "" || edit != nil {
					t.Fatalf("accepted %s", tc.body)
				}
				return
			}
			if msg != "" {
				t.Fatalf("refused %s: %s", tc.body, msg)
			}
			row := store.DashboardRepo{LinkedRepos: []store.Link{{Target: "old/one"}}}
			edit(&row)
			if len(row.LinkedRepos) != len(tc.want) {
				t.Fatalf("links = %+v; want %+v", row.LinkedRepos, tc.want)
			}
			for i, l := range row.LinkedRepos {
				w := tc.want[i]
				if l.Target != w.Target || l.CanBlock != w.CanBlock || !slices.Equal(l.Watched, w.Watched) {
					t.Errorf("link %d = %+v; want %+v", i, l, w)
				}
			}
		})
	}
}

// Every rule of a save, with the platform's word for it. A refusal names the
// target and the reason, and a platform that can't tell is never a pass.
func TestCheckLinksRules(t *testing.T) {
	gadgets := ghRepo("acme/gadgets")
	for _, tc := range []struct {
		name   string
		repo   settings.Effective
		links  []store.Link
		edit   func(c *fakeCatalog)
		status int // 0: allowed
		want   string
	}{
		{name: "no links need no reads", repo: gadgets},
		{name: "a public target of a private repository", repo: gadgets, links: []store.Link{link("acme/api", "render/**")}},
		{name: "an internal and a private target of a private repository", repo: gadgets,
			links: []store.Link{link("acme/lib"), link("acme/secret-lib")}},
		{name: "case is not told apart", repo: gadgets, links: []store.Link{link("ACME/Api")}},
		{name: "GitLab", repo: settings.Effective{Instance: "gh", Project: "acme/gadgets", Platform: "gitlab"}, links: []store.Link{link("acme/api")},
			status: http.StatusBadRequest, want: "GitHub"},
		{name: "itself", repo: gadgets, links: []store.Link{link("acme/gadgets")},
			status: http.StatusBadRequest, want: "can't link itself"},
		{name: "twice", repo: gadgets, links: []store.Link{link("acme/api"), link("acme/API")},
			status: http.StatusBadRequest, want: "twice"},
		{name: "four", repo: gadgets, links: []store.Link{link("a/1"), link("a/2"), link("a/3"), link("a/4")},
			status: http.StatusBadRequest, want: "at most 3"},
		{name: "not owner/name", repo: gadgets, links: []store.Link{link("https://evil.example/x/y")},
			status: http.StatusBadRequest, want: "owner/name"},
		{name: "a bad glob", repo: gadgets, links: []store.Link{link("acme/api", "render/")},
			status: http.StatusBadRequest, want: "render/"},
		{name: "a private target of a public repository", repo: ghRepo("acme/gadgets"), links: []store.Link{link("acme/secret-lib")},
			edit:   func(c *fakeCatalog) { c.setInfo("acme/gadgets", forge.VisPublic, 1) },
			status: http.StatusBadRequest, want: "acme/secret-lib: a public repository can't link a private one"},
		{name: "an internal target of a public repository", repo: ghRepo("acme/gadgets"), links: []store.Link{link("acme/lib")},
			edit:   func(c *fakeCatalog) { c.setInfo("acme/gadgets", forge.VisPublic, 1) },
			status: http.StatusBadRequest, want: "can't link an internal one"},
		{name: "a private target of an internal repository", repo: ghRepo("acme/gadgets"), links: []store.Link{link("acme/secret-lib")},
			edit:   func(c *fakeCatalog) { c.setInfo("acme/gadgets", forge.VisInternal, 1) },
			status: http.StatusBadRequest, want: "can't link a private one"},
		{name: "the App can't read the target", repo: gadgets, links: []store.Link{link("acme/ghost")},
			status: http.StatusBadRequest, want: "acme/ghost: the pergent review App can't read it"},
		{name: "another installation", repo: gadgets, links: []store.Link{link("acme/other-app")},
			status: http.StatusBadRequest, want: "another installation"},
		{name: "the target can't be read", repo: gadgets, links: []store.Link{link("acme/api")},
			edit:   func(c *fakeCatalog) { c.failInfo("acme/api", errors.New("GitHub said 500")) },
			status: http.StatusBadGateway, want: "could not check acme/api"},
		{name: "the repository can't be read", repo: gadgets, links: []store.Link{link("acme/api")},
			edit:   func(c *fakeCatalog) { c.failInfo("acme/gadgets", errors.New("GitHub said 500")) },
			status: http.StatusBadGateway, want: "could not check this repository"},
		{name: "the repository is not readable by the App", repo: gadgets, links: []store.Link{link("acme/api")},
			edit:   func(c *fakeCatalog) { c.mu.Lock(); delete(c.info, "acme/gadgets"); c.mu.Unlock() },
			status: http.StatusBadGateway, want: "could not check this repository"},
		{name: "an unknown visibility is not allowed", repo: gadgets, links: []store.Link{link("acme/api")},
			edit:   func(c *fakeCatalog) { c.setInfo("acme/api", "secret", 1) },
			status: http.StatusBadRequest, want: "not public, internal or private"},
		{name: "an unknown visibility of the repository is not allowed", repo: gadgets, links: []store.Link{link("acme/api")},
			edit:   func(c *fakeCatalog) { c.setInfo("acme/gadgets", "", 1) },
			status: http.StatusBadRequest, want: "not public, internal or private"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := linkWorld()
			if tc.edit != nil {
				tc.edit(cat)
			}
			p := linkDash(cat).checkLinks(context.Background(), tc.repo, tc.links)
			if tc.status == 0 {
				if p != nil {
					t.Fatalf("refused: %+v", p)
				}
				return
			}
			if p == nil {
				t.Fatalf("allowed; want %d %q", tc.status, tc.want)
			}
			if p.status != tc.status || !strings.Contains(p.msg, tc.want) {
				t.Errorf("= %d %q; want %d naming %q", p.status, p.msg, tc.status, tc.want)
			}
		})
	}
}

// A dashboard with no way to read the platform refuses a save with links; it
// never allows what it can't check.
func TestCheckLinksWithoutALinkerRefuses(t *testing.T) {
	d := &Dashboard{catalogs: syncmap.New[RepoCatalog](nil), Log: slog.New(slog.DiscardHandler), now: time.Now}
	p := d.checkLinks(context.Background(), ghRepo("acme/gadgets"), []store.Link{link("acme/api")})
	if p == nil || p.status != http.StatusBadGateway {
		t.Errorf("checkLinks = %+v; want a 502", p)
	}
	if p := d.checkLinks(context.Background(), ghRepo("acme/gadgets"), nil); p != nil {
		t.Errorf("clearing the links needs no reads, got %+v", p)
	}
}

// The reads of a save are never the list's: the cache is for choosing.
func TestCheckLinksDoesNotUseTheListsCache(t *testing.T) {
	cat := linkWorld()
	d := linkDash(cat)
	items := []LinkableRepo{{Repo: "acme/api"}}
	d.judgeLinkable(context.Background(), cat, ghRepo("acme/gadgets"), items, []int{0})
	if !items[0].Linkable {
		t.Fatalf("acme/api = %+v; want linkable (private links public)", items[0])
	}
	// Changed since the list read them: the save must see it.
	cat.setInfo("acme/gadgets", forge.VisPublic, 1)
	cat.setInfo("acme/api", forge.VisPrivate, 1)
	if p := d.checkLinks(context.Background(), ghRepo("acme/gadgets"), []store.Link{link("acme/api")}); p == nil {
		t.Error("a save used a cached visibility")
	}
}

func TestJudgeLinkable(t *testing.T) {
	cat := linkWorld()
	cat.failInfo("acme/secret-lib", errors.New("GitHub said 500"))
	d := linkDash(cat)
	names := []string{"acme/api", "acme/lib", "acme/secret-lib", "acme/other-app", "acme/ghost"}
	items := make([]LinkableRepo, len(names))
	var idx []int
	for i, n := range names {
		items[i] = LinkableRepo{Repo: n}
		idx = append(idx, i)
	}
	d.judgeLinkable(context.Background(), cat, ghRepo("acme/gadgets"), items, idx)

	want := map[string]LinkableRepo{
		"acme/api":        {Repo: "acme/api", Visibility: "public", Checked: true, Linkable: true},
		"acme/lib":        {Repo: "acme/lib", Visibility: "internal", Checked: true, Linkable: true},
		"acme/secret-lib": {Repo: "acme/secret-lib", Checked: false, Linkable: true, Reason: reasonUnchecked},
		"acme/other-app":  {Repo: "acme/other-app", Visibility: "public", Checked: true, Linkable: false, Reason: reasonInstall},
		"acme/ghost":      {Repo: "acme/ghost", Checked: true, Linkable: false, Reason: reasonNoApp},
	}
	for _, got := range items {
		if got != want[got.Repo] {
			t.Errorf("%s = %+v; want %+v", got.Repo, got, want[got.Repo])
		}
	}
}

// A public repository may link only public ones, and the list says so in
// plain words.
func TestJudgeLinkableFollowsTheRepositorysVisibility(t *testing.T) {
	cat := linkWorld()
	cat.setInfo("acme/gadgets", forge.VisPublic, 1)
	d := linkDash(cat)
	items := []LinkableRepo{{Repo: "acme/api"}, {Repo: "acme/lib"}, {Repo: "acme/secret-lib"}}
	d.judgeLinkable(context.Background(), cat, ghRepo("acme/gadgets"), items, []int{0, 1, 2})
	if !items[0].Linkable || items[0].Reason != "" {
		t.Errorf("public = %+v", items[0])
	}
	for _, it := range items[1:] {
		if it.Linkable || !it.Checked || !strings.Contains(it.Reason, "a public repository can't link") {
			t.Errorf("%s = %+v; want refused with the reason", it.Repo, it)
		}
	}
}

// The repository itself can't be read: every one is unchecked, none refused
// and none vouched for.
func TestJudgeLinkableWithoutTheSource(t *testing.T) {
	cat := linkWorld()
	cat.failInfo("acme/gadgets", errors.New("GitHub said 500"))
	items := []LinkableRepo{{Repo: "acme/api"}, {Repo: "acme/lib"}}
	linkDash(cat).judgeLinkable(context.Background(), cat, ghRepo("acme/gadgets"), items, []int{0, 1})
	for _, it := range items {
		if it.Checked || !it.Linkable || it.Reason != reasonUnchecked || it.Visibility != "" {
			t.Errorf("%s = %+v; want unchecked", it.Repo, it)
		}
	}
	if n := cat.infoCount("acme/api") + cat.infoCount("acme/lib"); n != 0 {
		t.Errorf("%d reads of targets after the repository failed", n)
	}
}

// Out of time: what was not read is unchecked, and no read is made after.
func TestJudgeLinkableOutOfTime(t *testing.T) {
	cat := linkWorld()
	d := linkDash(cat)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	items := []LinkableRepo{{Repo: "acme/api"}, {Repo: "acme/lib"}}
	d.judgeLinkable(ctx, cat, ghRepo("acme/gadgets"), items, []int{0, 1})
	for _, it := range items {
		if it.Checked || it.Reason != reasonUnchecked {
			t.Errorf("%s = %+v; want unchecked", it.Repo, it)
		}
	}
}

// Each repository is read once for the list, and not again for the next ten
// minutes' worth of opens within the cache's time.
func TestJudgeLinkableReadsOnce(t *testing.T) {
	cat := linkWorld()
	d := linkDash(cat)
	for range 3 {
		items := []LinkableRepo{{Repo: "acme/api"}, {Repo: "acme/lib"}}
		d.judgeLinkable(context.Background(), cat, ghRepo("acme/gadgets"), items, []int{0, 1})
	}
	for _, r := range []string{"acme/gadgets", "acme/api", "acme/lib"} {
		if n := cat.infoCount(r); n != 1 {
			t.Errorf("%s read %d times for three lists; want 1", r, n)
		}
	}
	// After the time is up it is read again.
	d.now = func() time.Time { return time.Now().Add(linkInfoTTL + time.Second) }
	items := []LinkableRepo{{Repo: "acme/api"}}
	d.judgeLinkable(context.Background(), cat, ghRepo("acme/gadgets"), items, []int{0})
	if n := cat.infoCount("acme/api"); n != 2 {
		t.Errorf("acme/api read %d times after the cache expired; want 2", n)
	}
}

// A failed read is not kept: the next list asks again.
func TestJudgeLinkableDoesNotKeepFailures(t *testing.T) {
	cat := linkWorld()
	cat.failInfo("acme/api", errors.New("boom"))
	d := linkDash(cat)
	items := []LinkableRepo{{Repo: "acme/api"}}
	d.judgeLinkable(context.Background(), cat, ghRepo("acme/gadgets"), items, []int{0})
	cat.okInfo("acme/api")
	items = []LinkableRepo{{Repo: "acme/api"}}
	d.judgeLinkable(context.Background(), cat, ghRepo("acme/gadgets"), items, []int{0})
	if !items[0].Checked || !items[0].Linkable {
		t.Errorf("acme/api = %+v after the platform came back; want checked and linkable", items[0])
	}
}

func TestLinksViewIsNeverNull(t *testing.T) {
	b, _ := json.Marshal(linksView(nil))
	if string(b) != "[]" {
		t.Errorf("no links = %s; want []", b)
	}
	b, _ = json.Marshal(linksView([]store.Link{{Target: "acme/api", CanBlock: true}}))
	if string(b) != `[{"target":"acme/api","watched":[],"canBlock":true}]` {
		t.Errorf("a link with no watched paths = %s", b)
	}
	// The cap is the one the rules use.
	if links.Max != 3 {
		t.Errorf("links.Max = %d; the API text says 3", links.Max)
	}
}

func TestSettingViewsLinks(t *testing.T) {
	v := settingViews(settings.Effective{})
	if got := v["linkedReposEnabled"]; got != (SettingView{false, false}) {
		t.Errorf("linkedReposEnabled = %+v; want false, not overridden", got)
	}
	repo := settings.Effective{LinkedReposEnabled: true, Overridden: map[string]bool{settings.KeyLinkedReposEnabled: true},
		LinkedRepos: []store.Link{{Target: "acme/api", Watched: []string{"a/**"}, CanBlock: false}}}
	v = settingViews(repo)
	if got := v["linkedReposEnabled"]; got != (SettingView{true, true}) {
		t.Errorf("linkedReposEnabled = %+v; want true, overridden", got)
	}
	got := v["linkedRepos"]
	b, _ := json.Marshal(got)
	if string(b) != `{"value":[{"target":"acme/api","watched":["a/**"],"canBlock":false}],"overridden":true}` {
		t.Errorf("linkedRepos = %s", b)
	}
}

// --- [CI] these need the test database, like every test that builds an env ---

// ghLinkEnv is an env whose GitHub instance "gh" has the dashboard repository
// acme/gadgets and the linkWorld platform.
func ghLinkEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, nil)
	e.dashboardRepo("gh", "acme/gadgets", nil)
	w := linkWorld()
	e.cat["gh"].mu.Lock()
	e.cat["gh"].repos, e.cat["gh"].info = w.repos, w.info
	e.cat["gh"].mu.Unlock()
	return e
}

const ghSettings = "/api/repositories/gh/acme/gadgets/settings"

func (e *env) storedLinks() ([]store.Link, *bool) {
	e.t.Helper()
	r, found := e.storedRepo("gh", "acme/gadgets")
	if !found {
		e.t.Fatal("the repository is not stored")
	}
	return r.LinkedRepos, r.LinkedReposEnabled
}

func TestPutLinkedRepos(t *testing.T) {
	e := ghLinkEnv(t)
	admin := e.userOnGH(true)

	// A valid list saves, and the answer shows it.
	rec := e.send(admin, "PUT", ghSettings,
		`{"linkedReposEnabled":true,"linkedRepos":[{"target":"acme/api","watched":["render/**","scope/**"],"canBlock":false},{"target":"acme/lib"}]}`)
	wantCode(t, rec, "PUT valid links", http.StatusOK)
	var detail RepositoryDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if s := detail.Settings["linkedReposEnabled"]; s.Value != true || !s.Overridden {
		t.Errorf("linkedReposEnabled = %+v", s)
	}
	b, _ := json.Marshal(detail.Settings["linkedRepos"])
	const want = `{"value":[{"target":"acme/api","watched":["render/**","scope/**"],"canBlock":false},{"target":"acme/lib","watched":[],"canBlock":true}],"overridden":true}`
	if string(b) != want {
		t.Errorf("linkedRepos = %s\nwant %s", b, want)
	}
	stored, enabled := e.storedLinks()
	if enabled == nil || !*enabled || len(stored) != 2 || stored[0].CanBlock || !stored[1].CanBlock {
		t.Errorf("stored = %+v, %v", stored, enabled)
	}
	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "settings_updated" {
			details = append(details, a.Detail)
		}
	}
	if !slices.Equal(details, []string{"linkedRepos, linkedReposEnabled"}) {
		t.Errorf("settings_updated details = %q; names the keys only", details)
	}

	// Each refusal is 400 and changes nothing.
	for _, tc := range []struct {
		name, body, want string
		edit             func()
	}{
		{"itself", `{"linkedRepos":[{"target":"acme/gadgets"}]}`, "can't link itself", nil},
		{"twice", `{"linkedRepos":[{"target":"acme/api"},{"target":"ACME/api"}]}`, "twice", nil},
		{"four", `{"linkedRepos":[{"target":"a/1"},{"target":"a/2"},{"target":"a/3"},{"target":"a/4"}]}`, "at most 3", nil},
		{"a URL", `{"linkedRepos":[{"target":"https://example.com/a/b"}]}`, "", nil},
		{"a bad glob", `{"linkedRepos":[{"target":"acme/api","watched":["render/"]}]}`, "render/", nil},
		{"the App can't read it", `{"linkedRepos":[{"target":"acme/ghost"}]}`, "acme/ghost: the pergent review App can't read it", nil},
		{"another installation", `{"linkedRepos":[{"target":"acme/other-app"}]}`, "another installation", nil},
		{"a string", `{"linkedRepos":"acme/api"}`, msgBadLinks, nil},
		{"an unknown field", `{"linkedRepos":[{"target":"acme/api","x":1}]}`, msgBadLinks, nil},
		{"a private target on a public repository", `{"linkedRepos":[{"target":"acme/secret-lib"}]}`, "acme/secret-lib: a public repository can't link a private one",
			func() { e.cat["gh"].setInfo("acme/gadgets", forge.VisPublic, 1) }},
	} {
		if tc.edit != nil {
			tc.edit()
		}
		before, _ := e.storedRepo("gh", "acme/gadgets")
		rec := e.send(admin, "PUT", ghSettings, tc.body)
		wantCode(t, rec, tc.name, http.StatusBadRequest)
		if !strings.Contains(errorOf(t, rec), tc.want) {
			t.Errorf("%s: error %q; want it to hold %q", tc.name, errorOf(t, rec), tc.want)
		}
		after, _ := e.storedRepo("gh", "acme/gadgets")
		if !after.UpdatedAt.Equal(before.UpdatedAt) || len(after.LinkedRepos) != 2 {
			t.Errorf("%s changed the row: %+v", tc.name, after)
		}
	}
	e.cat["gh"].setInfo("acme/gadgets", forge.VisPrivate, 1)

	// GitHub can't tell: 502, nothing saved, and no word from the platform.
	e.cat["gh"].failInfo("acme/api", errors.New("GitHub token ghs_SECRET said no"))
	rec = e.send(admin, "PUT", ghSettings, `{"linkedRepos":[{"target":"acme/api"}]}`)
	wantCode(t, rec, "a platform error", http.StatusBadGateway)
	if strings.Contains(rec.Body.String(), "ghs_SECRET") {
		t.Errorf("the platform's error text reached the answer: %s", rec.Body)
	}
	if stored, _ := e.storedLinks(); len(stored) != 2 {
		t.Errorf("a failed check changed the links: %+v", stored)
	}
	e.cat["gh"].okInfo("acme/api")

	// A GitLab repository's links are refused.
	e.dashboardRepo("main", "acme/gadgets", nil)
	lab := e.userOnMain(true)
	rec = e.send(lab, "PUT", "/api/repositories/main/acme/gadgets/settings", `{"linkedRepos":[{"target":"acme/api"}]}`)
	wantCode(t, rec, "links on GitLab", http.StatusBadRequest)
	if !strings.Contains(errorOf(t, rec), "GitHub") {
		t.Errorf("error %q", errorOf(t, rec))
	}

	// Null and [] clear the links, and the switch is kept.
	for _, body := range []string{`{"linkedRepos":null}`, `{"linkedRepos":[]}`} {
		e.send(admin, "PUT", ghSettings, `{"linkedRepos":[{"target":"acme/api"}]}`)
		wantCode(t, e.send(admin, "PUT", ghSettings, body), body, http.StatusOK)
		if stored, enabled := e.storedLinks(); len(stored) != 0 || enabled == nil || !*enabled {
			t.Errorf("after %s: %+v, %v", body, stored, enabled)
		}
	}

	// Clearing needs no reads, even when the platform is down.
	e.send(admin, "PUT", ghSettings, `{"linkedRepos":[{"target":"acme/api"}]}`)
	e.cat["gh"].failInfo("acme/gadgets", errors.New("down"))
	wantCode(t, e.send(admin, "PUT", ghSettings, `{"linkedRepos":null}`), "clearing while the platform is down", http.StatusOK)

	// The setting alone, without links, needs no reads either.
	wantCode(t, e.send(admin, "PUT", ghSettings, `{"linkedReposEnabled":false}`), "switch off", http.StatusOK)
	// And a PUT that leaves the links out keeps them.
	e.cat["gh"].okInfo("acme/gadgets")
	wantCode(t, e.send(admin, "PUT", ghSettings, `{"linkedRepos":[{"target":"acme/api"}]}`), "set again", http.StatusOK)
	wantCode(t, e.send(admin, "PUT", ghSettings, `{"approve":false}`), "another setting", http.StatusOK)
	if stored, _ := e.storedLinks(); len(stored) != 1 {
		t.Errorf("a PUT of another setting changed the links: %+v", stored)
	}
}

// Only an admin changes the setting and the links: a maintainer's PUT naming
// either (null too, and with other keys in the body) is 403 and changes
// nothing, their other settings still work, and their reset leaves both. An
// admin's reset clears them.
func TestLinkedReposNeedAdmin(t *testing.T) {
	e := ghLinkEnv(t)
	e.dashboardRepo("gh", "acme/gadgets", func(r *store.DashboardRepo) {
		on := true
		r.LinkedReposEnabled = &on
		r.LinkedRepos = []store.Link{{Target: "acme/api", Watched: []string{"render/**"}, CanBlock: true}}
	})
	e.aliceOn("gh", store.RoleMaintainer)
	maintainer := e.aliceSession()

	for _, body := range []string{
		`{"linkedReposEnabled":false}`, `{"linkedReposEnabled":null}`,
		`{"linkedRepos":[{"target":"acme/lib"}]}`, `{"linkedRepos":null}`, `{"linkedRepos":[]}`,
		`{"approve":false,"linkedRepos":null}`,
	} {
		before, _ := e.storedRepo("gh", "acme/gadgets")
		rec := e.send(maintainer, "PUT", ghSettings, body)
		wantCode(t, rec, "maintainer PUT "+body, http.StatusForbidden)
		if got := errorOf(t, rec); got != msgNeedsAdminLinks {
			t.Errorf("%s: error %q; want %q", body, got, msgNeedsAdminLinks)
		}
		after, _ := e.storedRepo("gh", "acme/gadgets")
		if !after.UpdatedAt.Equal(before.UpdatedAt) || after.Approve != nil {
			t.Errorf("%s changed the row: %+v", body, after)
		}
	}
	// No platform read was made for a refused write.
	if n := e.cat["gh"].infoCount("acme/lib"); n != 0 {
		t.Errorf("%d reads for a write that was refused", n)
	}
	wantCode(t, e.send(maintainer, "PUT", ghSettings, `{"approve":false}`), "maintainer, another setting", http.StatusOK)

	// A maintainer sees the setting but not the links.
	detail := getJSON[RepositoryDetailView](e, maintainer, "/api/repositories/gh/acme/gadgets")
	if s := detail.Settings["linkedReposEnabled"]; s.Value != true {
		t.Errorf("linkedReposEnabled = %+v", s)
	}
	if b, _ := json.Marshal(detail.Settings["linkedRepos"]); string(b) != `{"value":[],"overridden":false}` {
		t.Errorf("a maintainer sees linkedRepos = %s", b)
	}
	if strings.Contains(e.get(maintainer, "/api/repositories/gh/acme/gadgets").Body.String(), "acme/api") {
		t.Error("the maintainer's view names a linked repository")
	}

	// Their reset leaves both.
	wantCode(t, e.send(maintainer, "POST", ghSettings+"/reset", ""), "maintainer reset", http.StatusOK)
	if stored, enabled := e.storedLinks(); len(stored) != 1 || enabled == nil || !*enabled {
		t.Errorf("a maintainer's reset left %+v, %v", stored, enabled)
	}

	// A viewer is refused like any other settings write.
	viewer := e.sessionFor(e.sso("viewer@example.com"))
	e.team("gh viewers", []store.TeamMember{{AccountID: e.accountOf(viewer), Role: store.RoleViewer}}, store.TeamRepo{Instance: "gh"})
	wantCode(t, e.send(viewer, "PUT", ghSettings, `{"linkedReposEnabled":true}`), "viewer", http.StatusForbidden)

	// An admin sees the links, and their reset clears both.
	admin := e.sessionFor(e.ssoAdmin("admin@example.com"))
	detail = getJSON[RepositoryDetailView](e, admin, "/api/repositories/gh/acme/gadgets")
	if b, _ := json.Marshal(detail.Settings["linkedRepos"]); !strings.Contains(string(b), "acme/api") {
		t.Errorf("an admin sees linkedRepos = %s", b)
	}
	wantCode(t, e.send(admin, "POST", ghSettings+"/reset", ""), "admin reset", http.StatusOK)
	if stored, enabled := e.storedLinks(); len(stored) != 0 || enabled != nil {
		t.Errorf("an admin's reset left %+v, %v", stored, enabled)
	}
}

// The admin default is one switch: it is saved with the others (a save of the
// defaults must not lose it), repositories follow it, and a list of links is
// not a default.
func TestDefaultsLinkedReposEnabled(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.userOnMain(true, "acme/widgets")

	v := getJSON[DefaultsView](e, admin, "/api/admin/defaults")
	if v.LinkedReposEnabled || v.Config.LinkedReposEnabled || v.Pinned["linkedReposEnabled"] {
		t.Errorf("initial view = %+v; want off, config off, not pinned", v)
	}
	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"linkedReposEnabled": true}), "PUT", http.StatusOK)
	if !v.LinkedReposEnabled || v.Config.LinkedReposEnabled || !v.Pinned["linkedReposEnabled"] {
		t.Errorf("after PUT true: view = %+v; want on, config off, pinned", v)
	}
	if stored, err := e.st.RepoDefaults(ctx); err != nil || stored.LinkedReposEnabled == nil || !*stored.LinkedReposEnabled {
		t.Errorf("stored defaults = %+v, %v; want true", stored, err)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || !g.LinkedReposEnabled {
		t.Errorf("dashboard repository = %+v, %v; want the check on", g, err)
	}
	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "defaults_updated" {
			details = append(details, a.Detail)
		}
	}
	if !slices.Equal(details, []string{"linkedReposEnabled"}) {
		t.Errorf("defaults_updated details = %q", details)
	}

	rec := e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"linkedReposEnabled": "x"})
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != "linkedReposEnabled must be true, false or null" {
		t.Errorf("PUT x = %d %s", rec.Code, rec.Body)
	}
	// Links are per repository.
	rec = e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"linkedRepos": []any{}})
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgBadDefaults {
		t.Errorf("PUT linkedRepos = %d %s; want 400", rec.Code, rec.Body)
	}
	if !strings.Contains(msgBadDefaults, "linkedReposEnabled") || strings.Contains(msgBadDefaults, "linkedRepos,") {
		t.Errorf("msgBadDefaults = %q", msgBadDefaults)
	}

	// Left out of a body it is the built-in default (off), as the others are.
	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"approve": true}), "PUT", http.StatusOK)
	if v.LinkedReposEnabled || v.Pinned["linkedReposEnabled"] {
		t.Errorf("after a PUT without it: view = %+v; want off, not pinned", v)
	}
}

// The list says whether each repository can be linked and why not, in plain
// words, and reads each repository at most once.
func TestLinkableSaysWhy(t *testing.T) {
	e := ghLinkEnv(t)
	e.dashboardRepo("gh", "acme/gadgets", func(r *store.DashboardRepo) {
		r.LinkedRepos = []store.Link{{Target: "acme/lib", CanBlock: true}}
	})
	e.cat["gh"].set("acme/gadgets", "acme/api", "acme/lib", "acme/secret-lib", "acme/other-app", "acme/ghost")
	admin := e.userOnGH(true)

	v := getJSON[LinkableView](e, admin, "/api/admin/linkable?instance=gh&repo=acme/gadgets")
	got := map[string]LinkableRepo{}
	for _, it := range v.Items {
		got[it.Repo] = it
	}
	if len(v.Items) != 6 || v.Truncated {
		t.Fatalf("items = %+v, truncated %v", v.Items, v.Truncated)
	}
	for repo, want := range map[string]LinkableRepo{
		"acme/api":        {Repo: "acme/api", Visibility: "public", Checked: true, Linkable: true},
		"acme/secret-lib": {Repo: "acme/secret-lib", Visibility: "private", Checked: true, Linkable: true},
		"acme/other-app":  {Repo: "acme/other-app", Visibility: "public", Checked: true, Reason: reasonInstall},
		"acme/ghost":      {Repo: "acme/ghost", Checked: true, Reason: reasonNoApp},
		"acme/lib":        {Repo: "acme/lib", Reason: reasonLinked},
		"acme/gadgets":    {Repo: "acme/gadgets", Reason: reasonItself},
	} {
		if got[repo] != want {
			t.Errorf("%s = %+v; want %+v", repo, got[repo], want)
		}
	}
	// By name, and itself, ones linked already: no read for them.
	names := make([]string, len(v.Items))
	for i, it := range v.Items {
		names[i] = it.Repo
	}
	if !slices.IsSorted(names) {
		t.Errorf("not by name: %v", names)
	}
	if n := e.cat["gh"].infoCount("acme/lib"); n != 0 {
		t.Errorf("a repository already linked was read %d times", n)
	}
	// Opening it again reads nothing more.
	api := e.cat["gh"].infoCount("acme/api")
	getJSON[LinkableView](e, admin, "/api/admin/linkable?instance=gh&repo=acme/gadgets")
	if n := e.cat["gh"].infoCount("acme/api"); n != api {
		t.Errorf("acme/api read %d times after the second open, %d after the first", n, api)
	}

	// A public repository is told what it can't have.
	e2 := ghLinkEnv(t)
	e2.cat["gh"].setInfo("acme/gadgets", forge.VisPublic, 1)
	v = getJSON[LinkableView](e2, e2.userOnGH(true), "/api/admin/linkable?instance=gh&repo=acme/gadgets")
	for _, it := range v.Items {
		if it.Repo == "acme/secret-lib" && (it.Linkable || !strings.Contains(it.Reason, "a public repository can't link a private one")) {
			t.Errorf("acme/secret-lib = %+v", it)
		}
	}
}

func TestLinkableLimits(t *testing.T) {
	e := ghLinkEnv(t)
	var names []string
	for i := range 230 {
		n := fmt.Sprintf("acme/r%03d", i)
		names = append(names, n)
		e.cat["gh"].setInfo(n, forge.VisPublic, 1)
	}
	e.cat["gh"].set(append(names, "acme/gadgets")...)
	admin := e.userOnGH(true)

	v := getJSON[LinkableView](e, admin, "/api/admin/linkable?instance=gh&repo=acme/gadgets")
	if len(v.Items) != maxLinkable || !v.Truncated {
		t.Errorf("%d items, truncated %v; want %d and truncated", len(v.Items), v.Truncated, maxLinkable)
	}
	// A search narrows it.
	v = getJSON[LinkableView](e, admin, "/api/admin/linkable?instance=gh&repo=acme/gadgets&search=r00")
	if len(v.Items) != 10 || v.Truncated {
		t.Errorf("search: %d items, truncated %v; want 10", len(v.Items), v.Truncated)
	}

	// With three links already, nothing else is offered, and nothing is read.
	e.dashboardRepo("gh", "acme/gadgets", func(r *store.DashboardRepo) {
		r.LinkedRepos = []store.Link{{Target: "acme/r000"}, {Target: "acme/r001"}, {Target: "acme/r002"}}
	})
	before := e.cat["gh"].infoCount("acme/r100")
	v = getJSON[LinkableView](e, admin, "/api/admin/linkable?instance=gh&repo=acme/gadgets&search=r1")
	for _, it := range v.Items {
		if it.Linkable || it.Reason != reasonFull {
			t.Errorf("%s = %+v; want refused for the most links", it.Repo, it)
		}
	}
	if e.cat["gh"].infoCount("acme/r100") != before {
		t.Error("a repository was read though no link could be added")
	}
}

func TestLinkableBadRequests(t *testing.T) {
	e := ghLinkEnv(t)
	e.dashboardRepo("main", "acme/gadgets", nil)
	admin := e.userOnGH(true)
	for target, status := range map[string]int{
		"/api/admin/linkable":                                                                          http.StatusBadRequest,
		"/api/admin/linkable?instance=gh":                                                              http.StatusBadRequest,
		"/api/admin/linkable?repo=acme/gadgets":                                                        http.StatusBadRequest,
		"/api/admin/linkable?instance=none&repo=acme/gadgets":                                          http.StatusNotFound,
		"/api/admin/linkable?instance=gh&repo=acme/none":                                               http.StatusNotFound,
		"/api/admin/linkable?instance=main&repo=acme/gadgets":                                          http.StatusBadRequest, // GitLab
		"/api/admin/linkable?instance=gh&repo=acme/gadgets&search=" + strings.Repeat("a", maxSearch+1): http.StatusBadRequest,
	} {
		e.wantStatus(admin, target, status)
	}
	e.cat["gh"].setFail(errors.New("down"))
	rec := e.wantStatus(admin, "/api/admin/linkable?instance=gh&repo=acme/gadgets", http.StatusBadGateway)
	if strings.Contains(rec.Body.String(), "down") {
		t.Errorf("the platform's error reached the answer: %s", rec.Body)
	}
}

// Only an admin has the list: it names repositories the App reaches.
func TestLinkableNeedsAdmin(t *testing.T) {
	e := ghLinkEnv(t)
	const target = "/api/admin/linkable?instance=gh&repo=acme/gadgets"
	e.aliceOn("gh", store.RoleMaintainer)
	maintainer := e.aliceSession()
	viewer := e.sessionFor(e.sso("viewer@example.com"))
	e.team("gh viewers", []store.TeamMember{{AccountID: e.accountOf(viewer), Role: store.RoleViewer}}, store.TeamRepo{Instance: "gh"})
	waiting := e.sessionFor(e.sso("waiting@example.com"))

	unknown := e.get(maintainer, "/api/no-such-route")
	for name, c := range map[string]*http.Cookie{"maintainer": maintainer, "viewer": viewer, "no team": waiting, "signed out": nil} {
		rec := e.get(c, target)
		want := http.StatusNotFound
		if c == nil {
			want = http.StatusUnauthorized
		}
		if rec.Code != want {
			t.Errorf("%s: %d %s; want %d", name, rec.Code, rec.Body, want)
		}
		if c != nil && rec.Body.String() != unknown.Body.String() {
			t.Errorf("%s: body %s differs from an unknown path's %s", name, rec.Body, unknown.Body)
		}
		if strings.Contains(rec.Body.String(), "acme/") {
			t.Errorf("%s: the answer names a repository: %s", name, rec.Body)
		}
	}
	if n := e.cat["gh"].infoCount("acme/api"); n != 0 {
		t.Errorf("%d reads for requests that were refused", n)
	}
}
