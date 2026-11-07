package engine

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/result"
)

// The impact skill lists no file it reviewed. A thread of it resolves when the
// check ran and finished, and is left alone when it failed or did not run.
func TestImpactCoverage(t *testing.T) {
	impact := func(verdict string) result.Skill {
		return result.Skill{Name: result.ImpactSkill, Verdict: verdict}
	}

	for _, tc := range []struct {
		name       string
		skills     []result.Skill
		covered    bool
		wantFailed bool
	}{
		{"clean", []result.Skill{reviewed("no_blocking_issues", "a.go"), impact("no_blocking_issues")}, true, false},
		{"blocking", []result.Skill{reviewed("no_blocking_issues", "a.go"), impact("has_blocking_issues")}, true, false},
		{"failed", []result.Skill{reviewed("no_blocking_issues", "a.go"), impact("failed")}, true, true},
		{"unknown verdict", []result.Skill{reviewed("no_blocking_issues", "a.go"), impact("later")}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := buildFindings(&result.Result{Skills: tc.skills}, nil, redactSecret)
			got := fs.skills[result.ImpactSkill]
			if got.AllFiles != tc.covered || got.Failed != tc.wantFailed {
				t.Errorf("coverage = %+v; want AllFiles %t, Failed %t", got, tc.covered, tc.wantFailed)
			}
			if fs.skills["security"].AllFiles {
				t.Error("another skill covers every file")
			}
		})
	}

	// A run without the check has no coverage for it at all.
	fs := buildFindings(&result.Result{Skills: []result.Skill{reviewed("no_blocking_issues", "a.go")}}, nil, redactSecret)
	if _, ok := fs.skills[result.ImpactSkill]; ok {
		t.Error("a review without the check has coverage for it")
	}
}

// The impact check is not a skill with content, so it is neither carried nor
// logged as a skill that changed.
func TestCarriedFilesLeavesImpactAlone(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
	impact := lastSkill(result.ImpactSkill, []string{"a.go"})
	last := &result.Result{Skills: []result.Skill{lastSkill("kept", []string{"a.go"}), impact}}
	got := carriedFiles(last, map[string]string{"kept": "h1"}, map[string]string{"kept": "h1"}, []string{"other.go"}, nil, log)
	if len(got["kept"]) != 1 {
		t.Errorf("carried = %v; want kept carried", got)
	}
	if _, ok := got[result.ImpactSkill]; ok {
		t.Errorf("carried = %v; the impact check carries no file", got)
	}
	if strings.Contains(buf.String(), "impact") {
		t.Errorf("log = %q; want nothing about impact", buf.String())
	}
}
