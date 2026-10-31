package actions

import (
	"testing"

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
