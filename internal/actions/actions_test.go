package actions

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func TestMostSevere(t *testing.T) {
	fs := []result.Finding{
		{ID: "aaaaaa", Fingerprint: "aaaaaa111111", Severity: "suggestion", Title: "soft"},
		{ID: "bbbbbb", Fingerprint: "bbbbbb111111", Severity: "suggestion"},
		{ID: "aaaaaa", Fingerprint: "aaaaaa111111", Severity: "blocking", Title: "hard"},
	}
	if f, ok := MostSevere(fs, "aaaaaa111111"); !ok || f.Severity != "blocking" || f.Title != "hard" {
		t.Errorf("MostSevere(a) = %+v, %v; want the blocking report", f, ok)
	}
	if f, ok := MostSevere(fs, "bbbbbb111111"); !ok || f.Severity != "suggestion" {
		t.Errorf("MostSevere(b) = %+v, %v", f, ok)
	}
	if _, ok := MostSevere(fs, "cccccc111111"); ok {
		t.Error("MostSevere found a fingerprint nobody reported")
	}
}

func TestIsAuthor(t *testing.T) {
	for _, c := range []struct {
		person, author int64
		want           bool
	}{{9, 9, true}, {9, 7, false}, {0, 7, true}, {9, 0, true}, {0, 0, true}} {
		if got := IsAuthor(c.person, c.author); got != c.want {
			t.Errorf("IsAuthor(%d, %d) = %v; want %v", c.person, c.author, got, c.want)
		}
	}
}

func TestTermAndCapMessage(t *testing.T) {
	if Term("github") != "PR" || Term("gitlab") != "MR" {
		t.Errorf("Term = %q, %q", Term("github"), Term("gitlab"))
	}
	if got := CapMessage("MR"); got != "re-run limit reached for this MR; it resets at 00:00 UTC" {
		t.Errorf("CapMessage = %q", got)
	}
}

func TestRenderDismissals(t *testing.T) {
	got := RenderDismissals([]store.Dismissal{{Fingerprint: "f", Severity: "blocking", Actor: "a (x)", Reason: "r"}})
	if len(got) != 1 || got[0].Fingerprint != "f" || got[0].Severity != "blocking" || got[0].By != "a (x)" || got[0].Reason != "r" {
		t.Errorf("RenderDismissals = %+v", got)
	}
}

func TestCleanReason(t *testing.T) {
	for in, want := range map[string]string{
		"the input is a constant": "the input is a constant",
		"  padded  ":              "padded",
		"fine ![x](https://evil.example/p.png?a=b) ok": "fine  ok",
		"![](https://evil.example/p.png)":              "",
		"a ![one](u) b ![two][ref] c":                  "a  b  c",
		"see [a link](https://example.com)":            "see [a link](https://example.com)",
		"x <img src=\"https://evil.example/p.png\"> y": "x  y",
		"wow!": "wow!",
	} {
		if got := CleanReason(in); got != want {
			t.Errorf("CleanReason(%q) = %q; want %q", in, got, want)
		}
	}
}

// A reason stored before images were stripped is rendered without them.
func TestRenderDismissalsStripsImages(t *testing.T) {
	got := RenderDismissals([]store.Dismissal{{Fingerprint: "f", Severity: "blocking", Actor: "a", Reason: "ok ![p](https://evil.example/p.png)"}})
	if len(got) != 1 || got[0].Reason != "ok" {
		t.Errorf("RenderDismissals = %+v", got)
	}
}

// LatestFindings fills in the skill of each finding; the stored finding has
// none, and the skill never goes back into JSON.
func TestLatestFindingsCarriesSkill(t *testing.T) {
	raw := func(fp, severity string) json.RawMessage {
		return json.RawMessage(`{"id":"` + fp[:6] + `","fingerprint":"` + fp + `","file":"a.go","severity":"` + severity + `","title":"t"}`)
	}
	skills := []result.Skill{
		{Name: "style", Findings: []json.RawMessage{raw("aaaaaa111111", "suggestion")}},
		{Name: "security", Findings: []json.RawMessage{raw("aaaaaa111111", "blocking"), raw("bbbbbb111111", "suggestion"),
			json.RawMessage(`{"fingerprint":""}`), json.RawMessage(`7`)}},
	}
	got := findingsOf(skills)
	if len(got) != 3 || got[0].Skill != "style" || got[1].Skill != "security" || got[2].Skill != "security" {
		t.Fatalf("findingsOf = %+v; want three, each with its skill", got)
	}
	// The skill survives picking the most severe report of a fingerprint.
	if f, ok := MostSevere(got, "aaaaaa111111"); !ok || f.Severity != "blocking" || f.Skill != "security" {
		t.Errorf("MostSevere = %+v, %v; want the blocking report with its skill", f, ok)
	}
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "style") || strings.Contains(strings.ToLower(string(b)), "skill") {
		t.Errorf("finding JSON = %s; the skill must not be stored in it", b)
	}
}

// row is a teaching dismissal for the tests below; n orders them in time.
func row(skill, fp, severity string, n int, by int64) store.Dismissal {
	return store.Dismissal{
		Fingerprint: fp, Severity: severity, Reason: "why " + fp, Skill: skill, File: "a.go", Title: "title " + fp,
		CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour), Teaches: true,
		DismisserPlatformID: by,
	}
}

func TestDedupeDismissalsBlockingWins(t *testing.T) {
	older := row("security", "aaaaaa111111", "blocking", 1, 5)
	newer := row("security", "aaaaaa111111", "suggestion", 2, 6)
	got := DedupeDismissals([]store.Dismissal{newer, older})
	if len(got) != 1 || got[0].Severity != "blocking" {
		t.Fatalf("DedupeDismissals = %+v; want the older blocking row", got)
	}
}

func TestDedupeDismissalsNewestWins(t *testing.T) {
	older := row("security", "aaaaaa111111", "suggestion", 1, 5)
	newer := row("security", "aaaaaa111111", "suggestion", 2, 6)
	other := row("security", "bbbbbb111111", "blocking", 0, 5)
	got := DedupeDismissals([]store.Dismissal{older, newer, other})
	if len(got) != 2 || got[0].Fingerprint != "bbbbbb111111" || got[1].DismisserPlatformID != 6 {
		t.Fatalf("DedupeDismissals = %+v; want the blocking row, then the newer suggestion", got)
	}
}

// A row whose reason is only an image teaches nothing, so it can't hide the
// older row of the same finding that does.
func TestTeachingRowsDropsEmptyReasonsBeforeDeduping(t *testing.T) {
	older := row("security", "aaaaaa111111", "suggestion", 1, 5)
	newer := row("security", "aaaaaa111111", "blocking", 2, 6)
	newer.Reason = "![x](https://example.com/p.png)"
	other := row("security", "bbbbbb111111", "suggestion", 3, 5)
	got := TeachingRows([]store.Dismissal{newer, older, other})
	if len(got) != 2 || got[0].Fingerprint != "bbbbbb111111" || got[1] != older {
		t.Fatalf("TeachingRows = %+v; want the newest suggestion, then the older row of the finding", got)
	}
}

func TestPastDismissalsPerSkill(t *testing.T) {
	rows := []store.Dismissal{
		row("security", "aaaaaa111111", "blocking", 1, 5),
		row("style", "bbbbbb111111", "suggestion", 2, 5),
		row("perf", "cccccc111111", "suggestion", 3, 5),
	}
	got := PastDismissals(rows, []string{"security", "style", "docs"}, 9, nil)
	if len(got) != 2 || len(got["security"]) != 1 || len(got["style"]) != 1 {
		t.Fatalf("PastDismissals = %+v; want security and style only", got)
	}
	if e := got["security"][0]; e.Title != "title aaaaaa111111" || e.Severity != "blocking" || e.Reason != "why aaaaaa111111" || e.File != "a.go" {
		t.Errorf("security entry = %+v", e)
	}
	if _, ok := got["perf"]; ok {
		t.Error("a skill not in the list got entries")
	}
	if _, ok := got["docs"]; ok {
		t.Error("a skill with no rows got a key")
	}
}

func TestPastDismissalsBlockingFirstCap(t *testing.T) {
	var rows []store.Dismissal
	for i := 0; i < 25; i++ {
		rows = append(rows, row("security", fmt.Sprintf("s%011d", i), "suggestion", 10+i, 5))
	}
	rows = append(rows, row("security", "b00000000001", "blocking", 1, 5), row("security", "b00000000002", "blocking", 0, 5))
	got := PastDismissals(rows, []string{"security"}, 9, nil)["security"]
	if len(got) != 20 {
		t.Fatalf("got %d entries, want 20", len(got))
	}
	if got[0].Severity != "blocking" || got[1].Severity != "blocking" || got[2].Severity != "suggestion" {
		t.Errorf("severities = %s %s %s; want blocking, blocking, suggestion", got[0].Severity, got[1].Severity, got[2].Severity)
	}
	if got[0].Title != "title b00000000001" {
		t.Errorf("first = %q; want the newer blocking row first", got[0].Title)
	}
	if got[2].Title != "title s00000000024" {
		t.Errorf("third = %q; want the newest suggestion", got[2].Title)
	}
}

func TestPastDismissalsOwnChange(t *testing.T) {
	const author = 5
	rows := []store.Dismissal{
		row("security", "aaaaaa111111", "blocking", 1, author),
		row("security", "bbbbbb111111", "suggestion", 2, author),
		row("security", "cccccc111111", "blocking", 3, author),
		row("security", "cccccc111111", "blocking", 1, 6),
	}
	got := PastDismissals(rows, []string{"security"}, author, nil)["security"]
	titles := map[string]bool{}
	for _, e := range got {
		titles[e.Title] = true
	}
	if len(got) != 2 || titles["title aaaaaa111111"] || !titles["title bbbbbb111111"] || !titles["title cccccc111111"] {
		t.Fatalf("entries = %+v; want the suggestion and the other person's blocking row", got)
	}
}

func TestPastDismissalsUnknownAuthor(t *testing.T) {
	rows := []store.Dismissal{
		row("security", "aaaaaa111111", "blocking", 1, 5),
		row("security", "bbbbbb111111", "blocking", 2, 0),
		row("security", "cccccc111111", "suggestion", 3, 5),
	}
	got := PastDismissals(rows, []string{"security"}, 0, nil)["security"]
	if len(got) != 2 || got[0].Title != "title bbbbbb111111" || got[1].Title != "title cccccc111111" {
		t.Fatalf("entries = %+v; want the blocking row without a dismisser and the suggestion", got)
	}
}

func TestPastDismissalsOldRowsCannotMatch(t *testing.T) {
	rows := []store.Dismissal{row("security", "aaaaaa111111", "blocking", 1, 0)}
	for _, author := range []int64{0, 5} {
		if got := PastDismissals(rows, []string{"security"}, author, nil); len(got["security"]) != 1 {
			t.Errorf("author %d: entries = %+v; want the old row to stay", author, got)
		}
	}
}

func TestPastDismissalsEmptyAfterCleaning(t *testing.T) {
	image := row("security", "aaaaaa111111", "blocking", 2, 5)
	image.Reason = "![p](https://evil.example/p.png)"
	text := row("security", "aaaaaa111111", "blocking", 1, 6)
	got := PastDismissals([]store.Dismissal{image, text}, []string{"security"}, 9, nil)["security"]
	if len(got) != 1 || got[0].Reason != "why aaaaaa111111" {
		t.Fatalf("entries = %+v; want the row with a reason to win", got)
	}
	if got := PastDismissals([]store.Dismissal{image}, []string{"security"}, 9, nil); len(got) != 0 {
		t.Errorf("entries = %+v; want none for a reason that is only an image", got)
	}
}

func TestPastDismissalsClean(t *testing.T) {
	d := row("security", "aaaaaa111111", "blocking", 1, 5)
	d.Title = "SECRET " + strings.Repeat("é", 300)
	d.File = strings.Repeat("ü", 300) + "SECRET"
	d.Reason = "SECRET " + strings.Repeat("日", 600)
	redact := func(s string) string { return strings.ReplaceAll(s, "SECRET", "***") }
	e := PastDismissals([]store.Dismissal{d}, []string{"security"}, 9, redact)["security"][0]
	if !strings.HasPrefix(e.Title, "*** ") || strings.Contains(e.Title, "SECRET") || len([]rune(e.Title)) != 200 {
		t.Errorf("title = %q (%d runes); want redacted and 200 runes", e.Title, len([]rune(e.Title)))
	}
	if len([]rune(e.File)) != 200 || strings.Contains(e.File, "SECRET") {
		t.Errorf("file = %q (%d runes); want 200 runes", e.File, len([]rune(e.File)))
	}
	if !strings.HasPrefix(e.Reason, "*** ") || len([]rune(e.Reason)) != 500 {
		t.Errorf("reason = %q (%d runes); want redacted and 500 runes", e.Reason, len([]rune(e.Reason)))
	}
}

// The model, and the provider behind it, learn what was rejected and why, not
// who rejected it.
func TestPastDismissalsNoIdentity(t *testing.T) {
	d := row("security", "aaaaaa111111", "blocking", 1, 424242)
	d.Actor = "maya (gitlab main)"
	id := int64(77)
	d.ActorID = &id
	b, err := json.Marshal(PastDismissals([]store.Dismissal{d}, []string{"security"}, 9, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"maya", "424242", "77", "fingerprint", "actor"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("JSON %s holds %q", b, bad)
		}
	}
	const want = `{"security":[{"file":"a.go","title":"title aaaaaa111111","severity":"blocking","reason":"why aaaaaa111111"}]}`
	if string(b) != want {
		t.Errorf("JSON = %s; want %s", b, want)
	}
}
