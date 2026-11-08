package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// pagedCatalog is a fakeCatalog whose platform pages on its own side, as
// GitLab does: it keeps the order it was given and counts each page asked for.
type pagedCatalog struct {
	*fakeCatalog
	pmu   sync.Mutex
	asked []pageAsk
}

type pageAsk struct {
	search        string
	page, perPage int
}

func (p *pagedCatalog) ReachablePage(_ context.Context, search string, page, perPage int) ([]string, bool, error) {
	p.pmu.Lock()
	p.asked = append(p.asked, pageAsk{search, page, perPage})
	p.pmu.Unlock()
	p.mu.Lock()
	repos, fail := slices.Clone(p.repos), p.fail
	p.mu.Unlock()
	if fail != nil {
		return nil, false, fail
	}
	var match []string
	for _, r := range repos {
		if strings.Contains(strings.ToLower(r), strings.ToLower(search)) {
			match = append(match, r)
		}
	}
	from := min((page-1)*perPage, len(match))
	to := min(from+perPage, len(match))
	return match[from:to], to < len(match), nil
}

func (p *pagedCatalog) pageCalls() []pageAsk {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return slices.Clone(p.asked)
}

// ReachableName makes the fake spell a path as GitLab does: the case it holds.
func (p *pagedCatalog) ReachableName(_ context.Context, project string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return "", false, p.fail
	}
	for _, r := range p.repos {
		if strings.EqualFold(r, project) {
			return r, true, nil
		}
	}
	return "", false, nil
}

var (
	_ PagedRepoCatalog    = (*pagedCatalog)(nil)
	_ SpellingRepoCatalog = (*pagedCatalog)(nil)
)

// A dashboard with no store: the picker needs only the config (acme/widgets
// and acme/payments are on, on "main" and "gl"), the registry and a catalog
// per instance. gl's catalog pages on the platform, main's and gh's don't.
type pickerEnv struct {
	d     *Dashboard
	clock *clock
	main  *fakeCatalog
	gh    *fakeCatalog
	gl    *pagedCatalog
}

func newPickerEnv(t *testing.T) *pickerEnv {
	t.Helper()
	cfg := &config.Config{
		GitLab: []config.Instance{
			{Name: "main", URL: "https://gitlab.example.com"},
			{Name: "gl", URL: "https://gitlab2.example.com"},
		},
		GitHub: []config.GitHubInstance{{Name: "gh", APIURL: "https://api.github.com"}},
		Repositories: []config.Repository{
			{GitLab: "main", Project: "acme/widgets"},
			{GitLab: "gl", Project: "acme/widgets"},
			{GitHub: "gh", Project: "acme/widgets"},
		},
		Dashboard: config.Dashboard{URL: dashboardURL},
	}
	e := &pickerEnv{clock: &clock{t: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)},
		main: &fakeCatalog{}, gh: &fakeCatalog{}, gl: &pagedCatalog{fakeCatalog: &fakeCatalog{}}}
	cats := syncmap.New[RepoCatalog](map[string]RepoCatalog{"main": e.main, "gh": e.gh, "gl": e.gl})
	d, err := New(Options{Config: cfg, Instances: instances.New(cfg, nil, nil), Catalogs: cats, SecretKey: testKey(t), Now: e.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	e.d = d
	return e
}

type pickerBody struct {
	Items     []AvailableRepo `json:"items"`
	Page      int             `json:"page"`
	HasMore   bool            `json:"hasMore"`
	Truncated bool            `json:"truncated"`
}

// get calls the picker's handler for the instance and returns the answer.
func (e *pickerEnv) get(t *testing.T, instance, query string) (int, pickerBody, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	e.d.listAvailable(rec, httptest.NewRequest(http.MethodGet, "/api/repositories/available?"+query, nil), instance)
	var body pickerBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("answer %q: %v", rec.Body, err)
		}
	}
	return rec.Code, body, rec.Body.String()
}

func (e *pickerEnv) page(t *testing.T, instance, query string) pickerBody {
	t.Helper()
	code, body, raw := e.get(t, instance, query)
	if code != http.StatusOK {
		t.Fatalf("GET %s %s = %d: %s", instance, query, code, raw)
	}
	return body
}

func pickNames(n int, format string) []string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprintf(format, i))
	}
	return out
}

// On a platform that pages (GitLab) each page is one request to it, with the
// page and the size of 50, and its order is kept. A project switched on is
// left out of its page, which then holds fewer.
func TestAvailablePagesOnThePlatform(t *testing.T) {
	e := newPickerEnv(t)
	all := pickNames(120, "acme/repo-%03d")
	all[3] = "acme/widgets" // on already
	e.gl.set(all...)

	one := e.page(t, "gl", "")
	if one.Page != 1 || !one.HasMore || one.Truncated || len(one.Items) != 49 {
		t.Fatalf("page 1 = page %d, %d items, hasMore %v, truncated %v; want page 1, 49 (one is on), more",
			one.Page, len(one.Items), one.HasMore, one.Truncated)
	}
	if one.Items[0].Project != "acme/repo-000" || one.Items[3].Project != "acme/repo-004" {
		t.Errorf("items = %v; want the platform's order, the one on left out", projectsOf(one.Items[:5]))
	}
	for _, it := range one.Items {
		if it.Instance != "gl" || it.Platform != "gitlab" {
			t.Fatalf("item = %+v", it)
		}
	}

	three := e.page(t, "gl", "page=3")
	if three.Page != 3 || three.HasMore || len(three.Items) != 20 || three.Items[0].Project != "acme/repo-100" {
		t.Errorf("page 3 = page %d, %d items from %v, hasMore %v; want the last 20", three.Page, len(three.Items),
			projectsOf(three.Items[:1]), three.HasMore)
	}
	if past := e.page(t, "gl", "page=9"); len(past.Items) != 0 || past.HasMore || past.Page != 9 {
		t.Errorf("page 9 = %+v; want none and no more", past)
	}

	want := []pageAsk{{"", 1, 50}, {"", 3, 50}, {"", 9, 50}}
	if got := e.gl.pageCalls(); !slices.Equal(got, want) {
		t.Errorf("the platform was asked %v, want %v: one request for each page, never the whole list", got, want)
	}
	if n := e.gl.callCount(); n != 0 {
		t.Errorf("the whole list was read %d times, want 0", n)
	}
}

// The search goes to the platform, and a new search is its own list.
func TestAvailableSearchGoesToThePlatform(t *testing.T) {
	e := newPickerEnv(t)
	e.gl.set("acme/alpha", "acme/beta", "acme/alpha-two")
	got := e.page(t, "gl", "search=%20ALPHA%20&page=1")
	if !slices.Equal(projectsOf(got.Items), []string{"acme/alpha", "acme/alpha-two"}) || got.HasMore {
		t.Errorf("search = %v hasMore %v", projectsOf(got.Items), got.HasMore)
	}
	if asked := e.gl.pageCalls(); len(asked) != 1 || asked[0] != (pageAsk{"ALPHA", 1, 50}) {
		t.Errorf("asked %v; want the trimmed search with page 1", asked)
	}
}

// A page is kept 5 minutes by instance, search and page.
func TestAvailablePageCache(t *testing.T) {
	e := newPickerEnv(t)
	e.gl.set(pickNames(120, "acme/repo-%03d")...)

	e.page(t, "gl", "page=2")
	e.page(t, "gl", "page=2")
	if n := len(e.gl.pageCalls()); n != 1 {
		t.Fatalf("page 2 twice asked the platform %d times, want 1", n)
	}
	e.page(t, "gl", "page=3")
	e.page(t, "gl", "page=2&search=repo")
	e.page(t, "gl", "page=2&search=REPO")
	if n := len(e.gl.pageCalls()); n != 3 {
		t.Errorf("another page and another search made %d requests in all, want 3 (search ignores case)", n)
	}
	e.clock.advance(5*time.Minute + time.Second)
	e.page(t, "gl", "page=2")
	if n := len(e.gl.pageCalls()); n != 4 {
		t.Errorf("after 5 minutes %d requests, want 4", n)
	}
}

// A platform that can't page (GitHub) is read whole once, kept, and served 50
// at a time from that list; switched-on repositories are left out before the
// list is cut into pages, so every page but the last holds 50.
func TestAvailableServesAWholeListInPages(t *testing.T) {
	e := newPickerEnv(t)
	all := pickNames(120, "acme/repo-%03d")
	e.gh.set(append([]string{"acme/widgets", "Acme/Zeta"}, all...)...)

	one := e.page(t, "gh", "")
	if one.Page != 1 || len(one.Items) != 50 || !one.HasMore || one.Truncated {
		t.Fatalf("page 1 = page %d, %d items, hasMore %v, truncated %v", one.Page, len(one.Items), one.HasMore, one.Truncated)
	}
	if one.Items[0].Project != "Acme/Zeta" || one.Items[1].Project != "acme/repo-000" {
		t.Errorf("items start %v; want the sorted list without acme/widgets", projectsOf(one.Items[:2]))
	}
	for _, it := range one.Items {
		if it.Instance != "gh" || it.Platform != "github" {
			t.Fatalf("item = %+v", it)
		}
	}
	two := e.page(t, "gh", "page=2")
	three := e.page(t, "gh", "page=3")
	if len(two.Items) != 50 || !two.HasMore || len(three.Items) != 21 || three.HasMore {
		t.Errorf("pages 2 and 3 hold %d and %d items, hasMore %v and %v; want 50, 21, true, false",
			len(two.Items), len(three.Items), two.HasMore, three.HasMore)
	}
	seen := map[string]bool{}
	for _, b := range []pickerBody{one, two, three} {
		for _, it := range b.Items {
			if seen[it.Project] {
				t.Fatalf("%s is on two pages", it.Project)
			}
			seen[it.Project] = true
		}
	}
	if len(seen) != 121 {
		t.Errorf("the pages hold %d repositories, want 121 (122 less the one that is on)", len(seen))
	}
	if past := e.page(t, "gh", "page=4"); len(past.Items) != 0 || past.HasMore {
		t.Errorf("page 4 = %+v; want none", past)
	}
	if n := e.gh.callCount(); n != 1 {
		t.Errorf("the list was read %d times for 4 pages, want 1", n)
	}
}

// The whole list's own limits still show as truncated, on every page.
func TestAvailableWholeListTruncated(t *testing.T) {
	e := newPickerEnv(t)
	e.d.listLimit = 60
	e.gh.set(pickNames(100, "acme/repo-%03d")...)
	one, two := e.page(t, "gh", ""), e.page(t, "gh", "page=2")
	if len(one.Items) != 50 || !one.HasMore || !one.Truncated || len(two.Items) != 10 || two.HasMore || !two.Truncated {
		t.Errorf("page 1 %d items hasMore %v truncated %v; page 2 %d items hasMore %v truncated %v; want 50 true true; 10 false true",
			len(one.Items), one.HasMore, one.Truncated, len(two.Items), two.HasMore, two.Truncated)
	}
}

func TestAvailableRejectsABadPage(t *testing.T) {
	e := newPickerEnv(t)
	e.gl.set("acme/a")
	for _, q := range []string{"page=0", "page=-1", "page=abc", "page=1.5", "page=10001", "page=99999999999999999999"} {
		if code, _, raw := e.get(t, "gl", q); code != http.StatusBadRequest || !strings.Contains(raw, "page must be") {
			t.Errorf("?%s = %d %s; want 400 about the page", q, code, raw)
		}
	}
	if n := len(e.gl.pageCalls()); n != 0 {
		t.Errorf("a bad page reached the platform %d times", n)
	}
	if got := e.page(t, "gl", "page=10000"); got.Page != 10000 {
		t.Errorf("page 10000 = %+v", got)
	}
}

// A page that can't be read is a 502 that leaks nothing, and isn't kept.
func TestAvailablePageFails(t *testing.T) {
	e := newPickerEnv(t)
	e.gl.set("acme/a")
	e.gl.setFail(errors.New("gitlab: status 500: SECRET"))
	code, _, raw := e.get(t, "gl", "page=2")
	if code != http.StatusBadGateway || strings.Contains(raw, "SECRET") || strings.Contains(raw, "500") {
		t.Errorf("GET = %d %s; want 502 with no upstream detail", code, raw)
	}
	e.gl.setFail(nil)
	if got := e.page(t, "gl", "page=1"); len(got.Items) != 1 {
		t.Errorf("after the failure: %+v; want the list again (a failure isn't kept)", got)
	}
}

// GitLab pages by ID, so a page comes in no order of path; the picker sorts
// within the page and never across pages.
func TestAvailableSortsWithinAPage(t *testing.T) {
	e := newPickerEnv(t)
	e.gl.set("acme/zeta", "acme/alpha", "Acme/Mid", "acme/beta")
	got := e.page(t, "gl", "")
	if want := []string{"Acme/Mid", "acme/alpha", "acme/beta", "acme/zeta"}; !slices.Equal(projectsOf(got.Items), want) {
		t.Errorf("items = %v, want %v", projectsOf(got.Items), want)
	}
	e.gl.set(append([]string{"acme/zzz"}, pickNames(60, "acme/a-%02d")...)...)
	e.clock.advance(catalogTTL)
	one, two := e.page(t, "gl", ""), e.page(t, "gl", "page=2")
	if !one.HasMore || len(one.Items) != 50 || one.Items[0].Project != "acme/a-00" || one.Items[49].Project != "acme/zzz" {
		t.Errorf("page 1 = %d items from %s to %s, hasMore %v; want 50, a-00 to zzz, more", len(one.Items),
			one.Items[0].Project, one.Items[len(one.Items)-1].Project, one.HasMore)
	}
	if two.Items[0].Project != "acme/a-49" {
		t.Errorf("page 2 starts at %s; want the platform's next project, not a re-sorted list", two.Items[0].Project)
	}
}

// Switching on uses GitLab's spelling of the path, whatever case the client
// sent; a platform that has no spelling to give (GitHub, and the fake of
// "main") keeps the name as sent, trimmed.
func TestSwitchOnUsesThePlatformsSpelling(t *testing.T) {
	e := newPickerEnv(t)
	e.gl.set("acme/gadgets", "acme/widgets-old", "acme/Widgets-New")
	e.main.set("acme/gadgets")
	ctx := context.Background()

	for _, sent := range []string{"Acme/Gadgets", " ACME/GADGETS ", "acme/gadgets"} {
		name, ok, err := e.d.switchOnAvailable(ctx, "gl", sent)
		if err != nil || !ok || name != "acme/gadgets" {
			t.Errorf("gl %q = %q, %v, %v; want acme/gadgets", sent, name, ok, err)
		}
	}
	if name, ok, err := e.d.switchOnAvailable(ctx, "gl", "acme/widgets-new"); err != nil || !ok || name != "acme/Widgets-New" {
		t.Errorf("gl widgets-new = %q, %v, %v; want GitLab's spelling acme/Widgets-New", name, ok, err)
	}
	if _, ok, err := e.d.switchOnAvailable(ctx, "gl", "acme/widgets"); err != nil || ok {
		t.Errorf("gl acme/widgets = %v, %v; want not reached (only longer names are listed)", ok, err)
	}
	if name, ok, err := e.d.switchOnAvailable(ctx, "main", " Acme/Gadgets "); err != nil || !ok || name != "Acme/Gadgets" {
		t.Errorf("main = %q, %v, %v; want the name as sent, trimmed", name, ok, err)
	}
	e.gl.setFail(errors.New("down"))
	if _, ok, err := e.d.switchOnAvailable(ctx, "gl", "acme/gadgets"); err == nil || ok {
		t.Errorf("gl while down = %v, %v; want an error", ok, err)
	}
}
