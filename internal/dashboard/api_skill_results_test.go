package dashboard

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

func resultsURL(project, query string) string {
	return "/api/repositories/main/" + project + "/skill-results" + query
}

// decidedFates stores rows as the fates of change number of project, decided
// at at: merged, opened by a person.
func (e *env) decidedFates(project string, number int64, merged bool, at time.Time, rows ...store.FindingFate) {
	e.t.Helper()
	e.decidedFatesIn("main", testID(project), project, number, merged, at, rows...)
}

// decidedFatesIn is decidedFates for the project with platform ID projectID
// on instance, stored under the path project.
func (e *env) decidedFatesIn(instance string, projectID int64, project string, number int64, merged bool, at time.Time, rows ...store.FindingFate) {
	e.t.Helper()
	mr := store.MRKey{Instance: instance, ProjectID: projectID, IID: number}
	if err := e.st.RequestFates(ctx, mr, project, merged, at); err != nil {
		e.t.Fatal(err)
	}
	if stored, err := e.st.ReplaceFates(ctx, mr, at, project, rows, merged, false, at); err != nil || !stored {
		e.t.Fatalf("ReplaceFates(%v) = %v, %v; want it stored", mr, stored, err)
	}
}

// fates is count findings of skill at severity that all came to fate.
func fates(skill, hash, severity, fate string, count int) []store.FindingFate {
	var out []store.FindingFate
	for i := range count {
		out = append(out, store.FindingFate{Fingerprint: strings.Join([]string{skill, severity, fate, string(rune('a' + i))}, "-"),
			Skill: skill, SkillHash: hash, Severity: severity, Fate: fate})
	}
	return out
}

// The tab lists one row a skill and severity, with Addressed worked out.
func TestSkillResultsTab(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	at := e.clock.now().Add(-24 * time.Hour)
	e.decidedFates(buttonProject, 1, true, at, append(fates("code-review", "h1", "blocking", "fixed", 3),
		append(fates("code-review", "h1", "blocking", "open", 1), fates("code-review", "h1", "blocking", "dismissed", 1)...)...)...)
	c := e.signInRole(store.RoleViewer)

	rows := getJSON[[]SkillResultView](e, c, resultsURL(buttonProject, ""))
	want := SkillResultView{Skill: "code-review", Severity: "blocking", Raised: 5, Fixed: 3, Dismissed: 1, Open: 1}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("rows = %+v; want [%+v]", rows, want)
	}
	if rows[0].Addressed != nil || rows[0].EditedAt != nil {
		t.Errorf("addressed = %v, editedAt = %v; want null under 10 raised and no edit", rows[0].Addressed, rows[0].EditedAt)
	}
}

// Ten findings are enough for a percentage; the window is 30 days unless the
// request says 90.
func TestSkillResultsAddressedFromTenFindings(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	e.decidedFates(buttonProject, 1, true, e.clock.now().Add(-48*time.Hour), append(fates("style", "h1", "suggestion", "fixed", 4),
		fates("style", "h1", "suggestion", "open", 6)...)...)
	e.decidedFates(buttonProject, 2, true, e.clock.now().Add(-60*24*time.Hour), fates("old", "h1", "blocking", "fixed", 10)...)
	c := e.signInRole(store.RoleViewer)

	rows := getJSON[[]SkillResultView](e, c, resultsURL(buttonProject, ""))
	if len(rows) != 1 || rows[0].Skill != "style" || rows[0].Addressed == nil || *rows[0].Addressed != 40 {
		t.Fatalf("rows = %+v; want style at 40%% and nothing from 60 days ago", rows)
	}
	rows = getJSON[[]SkillResultView](e, c, resultsURL(buttonProject, "?days=90"))
	if len(rows) != 2 {
		t.Fatalf("90 days: rows = %+v; want 2", rows)
	}
}

func TestSkillResultsSortsLowestFirst(t *testing.T) {
	e := newEnv(t, nil)
	e.buttonBlockingReview()
	at := e.clock.now().Add(-time.Hour)
	var rows []store.FindingFate
	rows = append(rows, fates("good", "h", "blocking", "fixed", 9)...)
	rows = append(rows, fates("good", "h", "blocking", "open", 1)...)
	rows = append(rows, fates("poor", "h", "blocking", "fixed", 1)...)
	rows = append(rows, fates("poor", "h", "blocking", "open", 9)...)
	rows = append(rows, fates("new", "h", "blocking", "fixed", 2)...)
	e.decidedFates(buttonProject, 1, true, at, rows...)
	c := e.signInRole(store.RoleViewer)

	var got []string
	for _, r := range getJSON[[]SkillResultView](e, c, resultsURL(buttonProject, "")) {
		got = append(got, r.Skill)
	}
	if want := []string{"poor", "good", "new"}; !slices.Equal(got, want) {
		t.Errorf("order = %v; want %v", got, want)
	}
}

func TestSkillResultsBadDays(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInRole(store.RoleViewer)
	for _, q := range []string{"?days=7", "?days=0", "?days=x", "?days=", "?days=365"} {
		wantCode(t, e.get(c, resultsURL(buttonProject, q)), "skill results"+q, http.StatusBadRequest)
	}
	for _, q := range []string{"", "?days=30", "?days=90"} {
		wantCode(t, e.get(c, resultsURL(buttonProject, q)), "skill results"+q, http.StatusOK)
	}
}

// A repository the user can't see is not found, and one with nothing decided
// has an empty list.
func TestSkillResultsNoAccess(t *testing.T) {
	e := newEnv(t, nil)
	if err := e.st.UpsertProject(ctx, store.Project{Instance: "main", ProjectID: testID("acme/payments"), Path: "acme/payments"}); err != nil {
		t.Fatal(err)
	}
	c := e.signInGitLab(false, "acme/widgets")
	e.wantStatus(c, resultsURL("acme/payments", ""), http.StatusNotFound)
	wantCode(t, e.get(nil, resultsURL("acme/widgets", "")), "signed out", http.StatusUnauthorized)
	rec := e.get(c, resultsURL("acme/widgets", ""))
	wantCode(t, rec, "nothing decided", http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %s; want []", got)
	}
}

// A path that was reused by another project shows the project the session
// sees, and the same project ID on another instance is another project.
func TestSkillResultsProjectID(t *testing.T) {
	const oldID = 999
	setup := func() *env {
		e := newEnv(t, nil)
		e.buttonBlockingReview()
		at := e.clock.now().Add(-time.Hour)
		// The newest run under the path is of the old project.
		e.addRun(runFix{project: buttonProject, projectID: oldID, number: 3, head: head('2'),
			result: resultJSON(head('2'), buttonSkill("code-review")), at: e.clock.now().Add(-time.Minute)})
		e.decidedFatesIn("main", oldID, buttonProject, 3, true, at, fates("old-project", "h", "blocking", "fixed", 2)...)
		e.decidedFates(buttonProject, 1, true, at, fates("this-project", "h", "blocking", "fixed", 1)...)
		e.decidedFatesIn("gh", testID(buttonProject), buttonProject, 1, true, at, fates("other-instance", "h", "blocking", "fixed", 4)...)
		return e
	}
	skills := func(rows []SkillResultView) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Skill)
		}
		return out
	}

	e := setup()
	c := e.signInRole(store.RoleViewer)
	if got := skills(getJSON[[]SkillResultView](e, c, resultsURL(buttonProject, ""))); !slices.Equal(got, []string{"this-project"}) {
		t.Errorf("skills = %v; want the session's project alone", got)
	}
	// An operator has no visible IDs: the project the path is stored under.
	e2 := setup()
	if got := skills(getJSON[[]SkillResultView](e2, e2.signInOperator(), resultsURL(buttonProject, ""))); !slices.Equal(got, []string{"this-project"}) {
		t.Errorf("operator skills = %v; want the stored project's", got)
	}
}

// The rule for Addressed and the order of the rows do not need a database.
func TestSkillResultViews(t *testing.T) {
	edited := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	rows := []store.SkillResult{
		{Skill: "b", Severity: "suggestion", Raised: 10, Fixed: 5},
		{Skill: "b", Severity: "blocking", Raised: 10, Fixed: 5},
		{Skill: "a", Severity: "suggestion", Raised: 9, Fixed: 9},
		{Skill: "c", Severity: "blocking", Raised: 3, Fixed: 1, Dismissed: 2},
		{Skill: "d", Severity: "blocking", Raised: 3, Fixed: 3, Dismissed: 1, EditedAt: &edited},
		{Skill: "e", Severity: "blocking", Raised: 0, Dismissed: 4},
		{Skill: "f", Severity: "blocking", Raised: 12, Fixed: 1, Open: 11},
		{Skill: "g", Severity: "blocking", Raised: 14, Fixed: 5},
		{Skill: "h", Severity: "blocking", Raised: 200, Fixed: 1},
		{Skill: "i", Severity: "blocking", Raised: 3, Fixed: 2},
		{Skill: "j", Severity: "blocking", Raised: 11, Fixed: 5},
		{Skill: "k", Severity: "blocking", Raised: 10, Fixed: 10},
	}
	got := skillResultViews(rows)
	type row struct {
		skill, severity string
		addressed       *int
	}
	var order []row
	for _, v := range got {
		order = append(order, row{v.Skill, v.Severity, v.Addressed})
	}
	want := []row{
		{"h", "blocking", ptr(1)},  // 0.5% rounds to 1
		{"f", "blocking", ptr(8)},  // 8.33
		{"g", "blocking", ptr(36)}, // 35.71 rounds up
		{"j", "blocking", ptr(45)}, // 45.45 rounds down
		{"b", "blocking", ptr(50)}, // equal: blocking before suggestion
		{"b", "suggestion", ptr(50)},
		{"k", "blocking", ptr(100)},
		{"a", "suggestion", nil}, // under 10 raised: no percentage, nulls last, then by skill
		{"c", "blocking", nil},
		{"d", "blocking", nil},
		{"e", "blocking", nil},
		{"i", "blocking", nil},
	}
	if len(order) != len(want) {
		t.Fatalf("%d rows; want %d: %+v", len(order), len(want), order)
	}
	for i := range want {
		g, w := order[i], want[i]
		if g.skill != w.skill || g.severity != w.severity || (g.addressed == nil) != (w.addressed == nil) ||
			(g.addressed != nil && *g.addressed != *w.addressed) {
			t.Errorf("row %d = %v/%s %v; want %v/%s %v", i, g.skill, g.severity, deref(g.addressed), w.skill, w.severity, deref(w.addressed))
		}
	}
	for _, v := range got {
		if v.Skill == "d" && (v.EditedAt == nil || !v.EditedAt.Equal(edited) || v.Dismissed != 1 || v.Fixed != 3) {
			t.Errorf("d = %+v; want its edit time and counts carried over", v)
		}
		if v.Skill != "d" && v.EditedAt != nil {
			t.Errorf("%s has editedAt %v", v.Skill, v.EditedAt)
		}
	}
	if out := skillResultViews(nil); out == nil || len(out) != 0 {
		t.Errorf("no rows = %#v; want an empty list, not null", out)
	}
}

func deref(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}
