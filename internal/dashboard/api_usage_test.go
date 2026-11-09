package dashboard

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// usageRun stores a run of f's project and records skills as what it used. The
// run's usage is dated as the run was reported.
func (e *env) usageRun(f runFix, skills ...store.SkillUsage) string {
	e.t.Helper()
	id := e.addRun(f)
	if err := e.st.RecordUsage(ctx, id, skills); err != nil {
		e.t.Fatalf("RecordUsage: %v", err)
	}
	return id
}

// usageSkill is a skill's usage with every count reported.
func usageSkill(skill, verdict string, in, out int64) store.SkillUsage {
	zero := int64(0)
	return store.SkillUsage{Skill: skill, Verdict: verdict, In: in, Out: out, CacheRead: &zero, CacheWrite: &zero, Reasoning: &zero}
}

// usageFixture stores usage on three repositories: acme/widgets (on, two
// reviews, 1,160 tokens), acme/old (in no config, one review, 200 tokens) and
// acme/payments (on, nothing).
func (e *env) usageFixture() {
	e.t.Helper()
	now := e.clock.now()
	e.usageRun(runFix{project: "acme/widgets", number: 1, head: head('1'), at: now.Add(-48 * time.Hour), title: "Add login",
		webURL: "https://gitlab.example.com/acme/widgets/-/merge_requests/1", model: "m1", provider: "anthropic"},
		usageSkill("security", "no_blocking_issues", 800, 100), usageSkill("style", "no_blocking_issues", 200, 60))
	e.usageRun(runFix{project: "acme/widgets", number: 2, head: head('2'), at: now.Add(-2 * time.Hour), title: "Refactor",
		model: "m2", incremental: true},
		usageSkill("security", "failed", 0, 0))
	e.usageRun(runFix{project: "acme/old", projectID: 50, number: 3, head: head('3'), at: now.Add(-3 * time.Hour), model: "m1"},
		usageSkill("security", "no_blocking_issues", 150, 50))
}

func TestUsageOverviewForAdmins(t *testing.T) {
	e := newEnv(t, nil)
	e.usageFixture()
	got := getJSON[UsageOverviewView](e, e.operator(), "/api/admin/usage")

	if got.Days != 30 {
		t.Errorf("days = %d, want the default 30", got.Days)
	}
	if got.Reviews != 3 || got.Tokens.In != 1150 || got.Tokens.Out != 210 || got.Tokens.Unknown != 0 {
		t.Errorf("totals = %+v over %d reviews; want 1150 in, 210 out, 3 reviews", got.Tokens, got.Reviews)
	}
	if len(got.Repositories) != 2 {
		t.Fatalf("repositories = %+v; want the two with usage", got.Repositories)
	}
	first, second := got.Repositories[0], got.Repositories[1]
	if first.Instance != "main" || first.Project != "acme/widgets" || !first.SwitchedOn || first.Reviews != 2 || first.Tokens.In != 1000 || first.Tokens.Out != 160 {
		t.Errorf("first = %+v; want acme/widgets, switched on, busiest", first)
	}
	if second.Project != "acme/old" || second.SwitchedOn || second.Reviews != 1 {
		t.Errorf("second = %+v; want acme/old, not switched on", second)
	}
	if len(got.Daily) != 2 || got.Daily[0].Day >= got.Daily[1].Day {
		t.Errorf("daily = %+v; want two days, oldest first", got.Daily)
	}
}

// With nothing used, every list is an empty array.
func TestUsageOverviewIsEmptyWithNoUsage(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.get(e.operator(), "/api/admin/usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/usage = %d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{`"daily":[]`, `"repositories":[]`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body %s lacks %s", rec.Body, want)
		}
	}
}

// A repository's switch is read by instance and the path ignoring case.
func TestUsageOverviewSwitchedOnIgnoresCase(t *testing.T) {
	e := newEnv(t, nil)
	e.usageRun(runFix{project: "Acme/Widgets", projectID: 77, number: 1, head: head('1')}, usageSkill("security", "no_blocking_issues", 1, 1))
	got := getJSON[UsageOverviewView](e, e.operator(), "/api/admin/usage")
	if len(got.Repositories) != 1 || !got.Repositories[0].SwitchedOn {
		t.Errorf("repositories = %+v; want Acme/Widgets switched on as acme/widgets is", got.Repositories)
	}
}

// Usage is for admins: anyone else gets the 404 of a path that is not there.
func TestUsageIsForAdmins(t *testing.T) {
	e := newEnv(t, nil)
	e.usageFixture()
	viewer := e.signInRole(store.RoleViewer)

	unknown := e.get(viewer, "/api/no-such-route")
	unknownRepo := e.get(viewer, "/api/repositories/main/acme/nothing/usage")
	for target, want := range map[string]string{
		"/api/admin/usage":                          unknown.Body.String(),
		"/api/repositories/main/acme/widgets/usage": unknownRepo.Body.String(),
	} {
		rec := e.get(viewer, target)
		if rec.Code != http.StatusNotFound || rec.Body.String() != want {
			t.Errorf("GET %s as a viewer = %d %s; want 404 %s", target, rec.Code, rec.Body, want)
		}
	}
}

func TestUsageBadDays(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	for _, target := range []string{"/api/admin/usage", "/api/repositories/main/acme/widgets/usage"} {
		for _, q := range []string{"?days=0", "?days=91", "?days=x", "?days="} {
			rec := e.get(c, target+q)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "days must be a number from 1 to 90") {
				t.Errorf("GET %s%s = %d %s; want 400", target, q, rec.Code, rec.Body)
			}
		}
		for _, q := range []string{"", "?days=1", "?days=90"} {
			wantCode(t, e.get(c, target+q), "GET "+target+q, http.StatusOK)
		}
	}
}

// A shorter period leaves out what is older.
func TestUsageOverviewDays(t *testing.T) {
	e := newEnv(t, nil)
	e.usageFixture()
	got := getJSON[UsageOverviewView](e, e.operator(), "/api/admin/usage?days=1")
	if got.Days != 1 || got.Reviews != 2 || got.Tokens.In != 150 {
		t.Errorf("days=1: days %d, reviews %d, tokens %+v; want the two reviews of the last day", got.Days, got.Reviews, got.Tokens)
	}
}

func TestUsageOfARepository(t *testing.T) {
	e := newEnv(t, nil)
	e.usageFixture()
	now := e.clock.now()
	// Three small reviews and one far above their median: an outlier.
	for i, n := range []int64{10, 10, 10, 1000} {
		e.usageRun(runFix{project: "acme/payments", number: int64(20 + i), head: head(byte('a' + i)), at: now.Add(-time.Hour), title: "Payments " + string(rune('A'+i)),
			webURL: "javascript:alert(1)", model: "m1"},
			usageSkill("security", "no_blocking_issues", n, 0))
	}
	// A review with no result: counted as wasted, its tokens unknown.
	e.addRun(runFix{project: "acme/payments", number: 30, head: head('9'), failure: "the Job ran out of memory", outcome: OutcomeFailed, at: now.Add(-time.Hour)})

	c := e.operator()
	got := getJSON[UsageRepositoryView](e, c, "/api/repositories/main/acme/widgets/usage")
	if got.Days != 30 || len(got.Daily) != 2 {
		t.Errorf("days %d, daily %+v; want 30 and two days", got.Days, got.Daily)
	}
	if len(got.Skills) != 2 || got.Skills[0].Name != "security" || got.Skills[0].Tokens.In != 800 || got.Skills[0].Runs != 2 || got.Skills[0].Failed != 1 {
		t.Errorf("skills = %+v; want security first with 2 runs, 1 failed", got.Skills)
	}
	if len(got.Models) != 2 || got.Models[0].Name != "m1" || got.Models[0].Runs != 1 || got.Models[1].Name != "m2" {
		t.Errorf("models = %+v; want m1 then m2", got.Models)
	}
	if got.Models[0].Provider != "anthropic" || got.Models[1].Provider != "" || got.Skills[0].Provider != "" {
		t.Errorf("providers = %q, %q, skill %q; want anthropic, none for a run without one, none for a skill",
			got.Models[0].Provider, got.Models[1].Provider, got.Skills[0].Provider)
	}
	if len(got.MergeRequests) != 2 || got.MergeRequests[0].IID != 1 || got.MergeRequests[0].Title != "Add login" ||
		got.MergeRequests[0].WebURL == "" || got.MergeRequests[0].Reviews != 1 {
		t.Errorf("mergeRequests = %+v; want !1 first, with its title and address", got.MergeRequests)
	}
	if m := got.MergeRequests[0].Models; len(m) != 1 || m[0] != (UsageModelView{Provider: "anthropic", Model: "m1"}) {
		t.Errorf("!1 models = %+v; want anthropic's m1", m)
	}
	if m := got.MergeRequests[1].Models; len(m) != 1 || m[0] != (UsageModelView{Model: "m2"}) {
		t.Errorf("!2 models = %+v; want m2 with no provider", m)
	}
	if got.Wasted.Failed.In != 0 || got.Wasted.RerunCount != 0 || got.Wasted.NoResult != 0 {
		t.Errorf("wasted = %+v; want no waste on acme/widgets", got.Wasted)
	}
	if got.FullMedian == nil || *got.FullMedian != 1160 || got.IncrementalMedian == nil || *got.IncrementalMedian != 0 {
		t.Errorf("medians = %v, %v; want 1160 for the full review and 0 for the incremental one", got.FullMedian, got.IncrementalMedian)
	}
	if got.Outliers == nil || len(got.Outliers) != 0 {
		t.Errorf("outliers = %+v; want an empty list", got.Outliers)
	}

	pay := getJSON[UsageRepositoryView](e, c, "/api/repositories/main/acme/payments/usage")
	if pay.FullMedian == nil || *pay.FullMedian != 10 || pay.IncrementalMedian != nil {
		t.Errorf("payments medians = %v, %v; want a full one of 10", pay.FullMedian, pay.IncrementalMedian)
	}
	if len(pay.Outliers) != 1 || pay.Outliers[0].Total != 1000 || pay.Outliers[0].IID != 23 || pay.Outliers[0].Incremental || pay.Outliers[0].RunID == "" {
		t.Fatalf("payments outliers = %+v; want the review of 1000 tokens on !23", pay.Outliers)
	}
	if _, err := time.Parse(time.RFC3339, pay.Outliers[0].At); err != nil {
		t.Errorf("outlier at = %q: %v", pay.Outliers[0].At, err)
	}
	if pay.Wasted.NoResult != 1 {
		t.Errorf("payments wasted = %+v; want 1 review with no result", pay.Wasted)
	}
	for _, mr := range pay.MergeRequests {
		if mr.WebURL != "" {
			t.Errorf("merge request %+v: an address that is not http(s) must be empty", mr)
		}
	}
}

// A repository that is not switched on has no page, whatever it used; one
// with no usage has empty lists.
func TestUsageOfARepositoryWithNone(t *testing.T) {
	e := newEnv(t, nil)
	e.usageFixture()
	c := e.operator()
	wantCode(t, e.get(c, "/api/repositories/main/acme/old/usage"), "a repository that is not switched on", http.StatusNotFound)

	rec := e.get(c, "/api/repositories/gh/acme/widgets/usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET usage of gh/acme/widgets = %d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{`"daily":[]`, `"skills":[]`, `"models":[]`, `"mergeRequests":[]`, `"outliers":[]`, `"fullMedian":null`, `"incrementalMedian":null`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body %s lacks %s", rec.Body, want)
		}
	}
}

// A cell a spreadsheet would read as a formula is written as text.
func TestCSVCell(t *testing.T) {
	for in, want := range map[string]string{"": "", "acme/widgets": "acme/widgets", "=cmd": "'=cmd", "+1": "'+1", "-x": "'-x", "@a": "'@a",
		"\tx": "'\tx", "\rx": "'\rx", "a=b": "a=b"} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", in, got, want)
		}
	}
}

const csvHeader = "reported_at,instance,repository,merge_request,trigger,incremental,model,skill,verdict," +
	"tokens_in,tokens_out,cache_read,cache_write,reasoning,findings,blocking,provider"

func csvRows(t *testing.T, rec *httptest.ResponseRecorder) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("body %q is not CSV: %v", rec.Body, err)
	}
	return rows
}

func TestUsageCSV(t *testing.T) {
	e := newEnv(t, nil)
	e.usageFixture()
	// A skill whose pergent reported no cache counts, and a name that a
	// spreadsheet would read as a formula.
	e.usageRun(runFix{project: "acme/widgets", number: 4, head: head('4'), at: e.clock.now().Add(-time.Hour), title: "=HYPERLINK(\"x\")", model: "=m", provider: "=p"},
		store.SkillUsage{Skill: "+skill", Verdict: "has_blocking_issues", In: 7, Out: 3, Findings: 2, Blocking: 1})
	rec := e.get(e.operator(), "/api/admin/usage.csv?days=30")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET usage.csv = %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="pergent-usage-30d.csv"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	rows := csvRows(t, rec)
	if got := strings.Join(rows[0], ","); got != csvHeader {
		t.Errorf("header = %s, want %s", got, csvHeader)
	}
	// Three runs and four skills in the fixture, and the new one.
	if len(rows) != 1+5 {
		t.Fatalf("%d rows, want the header and 5: %v", len(rows), rows)
	}
	for _, r := range rows[1:] {
		if len(r) != 17 {
			t.Errorf("row %v has %d cells, want 17", r, len(r))
		}
	}
	// Oldest first: the first review of acme/widgets, two days ago.
	first := rows[1]
	if first[1] != "main" || first[2] != "acme/widgets" || first[3] != "1" || first[4] != "push" || first[5] != "false" ||
		first[6] != "m1" || first[7] != "security" || first[9] != "800" || first[10] != "100" || first[11] != "0" {
		t.Errorf("first row = %v", first)
	}
	if _, err := time.Parse(time.RFC3339, first[0]); err != nil || !strings.HasSuffix(first[0], "Z") {
		t.Errorf("reported_at = %q, want RFC 3339 in UTC", first[0])
	}
	last := rows[5]
	if last[16] != "'=p" {
		t.Errorf("last provider = %q, want the formula as text", last[16])
	}
	if first[16] != "anthropic" {
		t.Errorf("first provider = %q, want anthropic", first[16])
	}
	if last[6] != "'=m" || last[7] != "'+skill" || last[11] != "" || last[12] != "" || last[13] != "" || last[14] != "2" || last[15] != "1" {
		t.Errorf("last row = %v; want the formulas as text and empty cache and reasoning counts", last)
	}
	if strings.Contains(rec.Body.String(), "HYPERLINK") || strings.Contains(rec.Body.String(), "Add login") {
		t.Errorf("the body holds a title: %s", rec.Body)
	}
}

// The provider is the last column, so a script reading the others by position
// keeps working.
func TestUsageCSVHasProviderLast(t *testing.T) {
	old := []string{"reported_at", "instance", "repository", "merge_request", "trigger", "incremental", "model",
		"skill", "verdict", "tokens_in", "tokens_out", "cache_read", "cache_write", "reasoning", "findings", "blocking"}
	if len(usageCSVHeader) != len(old)+1 || usageCSVHeader[len(old)] != "provider" ||
		strings.Join(usageCSVHeader[:len(old)], ",") != strings.Join(old, ",") {
		t.Errorf("header = %v; want %v and provider last", usageCSVHeader, old)
	}
}

func TestUsageCSVOfARepository(t *testing.T) {
	e := newEnv(t, nil)
	e.usageFixture()
	c := e.operator()

	rows := csvRows(t, e.get(c, "/api/admin/usage.csv?instance=main&project=acme/widgets"))
	if len(rows) != 1+3 {
		t.Errorf("acme/widgets: %d rows, want the header and 3: %v", len(rows), rows)
	}
	for _, r := range rows[1:] {
		if r[2] != "acme/widgets" {
			t.Errorf("row %v is of another repository", r)
		}
	}
	// A repository without usage, or not stored, is a header alone.
	if rows := csvRows(t, e.get(c, "/api/admin/usage.csv?instance=gh&project=acme/widgets")); len(rows) != 1 {
		t.Errorf("gh/acme/widgets: %v, want the header alone", rows)
	}
	// The period leaves out what is older.
	if rows := csvRows(t, e.get(c, "/api/admin/usage.csv?days=1")); len(rows) != 1+2 {
		t.Errorf("days=1: %v, want the header and 2 rows", rows)
	}
}

// A repository switched off loses its project row but keeps its usage, and
// its export is still its rows.
func TestUsageCSVOfASwitchedOffRepository(t *testing.T) {
	e := newEnv(t, nil)
	e.usageFixture()
	e.exec(`DELETE FROM projects WHERE instance = 'main' AND lower(path) = 'acme/old'`)

	rows := csvRows(t, e.get(e.operator(), "/api/admin/usage.csv?instance=main&project=acme/old"))
	if len(rows) != 1+1 || rows[1][2] != "acme/old" || rows[1][9] != "150" {
		t.Errorf("acme/old: %v, want the header and its one row", rows)
	}
}

func TestUsageCSVBadQuery(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	for _, q := range []string{"?days=0", "?days=91", "?days=x", "?instance=main", "?project=acme/widgets", "?instance=&project=acme/widgets"} {
		wantCode(t, e.get(c, "/api/admin/usage.csv"+q), "usage.csv"+q, http.StatusBadRequest)
	}
}

func TestUsageCSVIsForAdmins(t *testing.T) {
	e := newEnv(t, nil)
	e.usageFixture()
	viewer := e.signInRole(store.RoleViewer)
	unknown := e.get(viewer, "/api/no-such-route")
	rec := e.get(viewer, "/api/admin/usage.csv")
	if rec.Code != http.StatusNotFound || rec.Body.String() != unknown.Body.String() {
		t.Errorf("usage.csv as a viewer = %d %s; want 404 %s", rec.Code, rec.Body, unknown.Body)
	}
}
