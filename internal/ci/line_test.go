package ci

import (
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

func TestLine(t *testing.T) {
	got := Line([]store.CIJobResult{{Name: "test", Result: "passed"}, {Name: "cycode-scan", Result: "failed", Blocks: true}})
	if want := "CI: test passed · cycode-scan failed (blocks approval)"; got != want {
		t.Errorf("got %q", got)
	}
	got = Line([]store.CIJobResult{{Name: "api", Result: "unreadable", Reason: "no permission"}})
	if want := "CI: api unreadable (no permission)"; got != want {
		t.Errorf("got %q", got)
	}
	if got := Line(nil); got != "" {
		t.Errorf("got %q", got)
	}
	if got := Line([]store.CIJobResult{{Name: "t", Result: "passed", Blocks: true}}); got != "CI: t passed" {
		t.Errorf("got %q", got)
	}
}

func TestBlocking(t *testing.T) {
	in := []store.CIJobResult{
		{Name: "a", Result: "passed", Blocks: true},
		{Name: "b", Result: "failed", Blocks: true},
		{Name: "c", Result: "failed"},
		{Name: "d", Result: "missing", Blocks: true},
	}
	got := Blocking(in)
	if len(got) != 2 || got[0].Name != "b" || got[1].Name != "d" {
		t.Fatalf("got %+v", got)
	}
}
