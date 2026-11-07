package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// These tests need no database: the rule is applied to a platform read through
// one function. The publish tests that need one are in publish_impact_test.go.

func TestCheckImpactTargets(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name    string
		edit    func(w *planWorld)
		targets []string
		ok      bool
	}{
		{"private links public", nil, []string{"acme/web"}, true},
		{"private links private", nil, []string{"acme/api"}, true},
		{"private links internal", nil, []string{"acme/docs"}, true},
		{"every target is checked", nil, []string{"acme/web", "acme/api"}, true},
		{"a target went private under a public repository", func(w *planWorld) { w.vis[planSource] = forge.VisPublic }, []string{"acme/api"}, false},
		{"one of several is refused", func(w *planWorld) { w.vis[planSource] = forge.VisPublic }, []string{"acme/web", "acme/api"}, false},
		{"internal links private", func(w *planWorld) { w.vis[planSource] = forge.VisInternal }, []string{"acme/api"}, false},
		{"the target can't be read", func(w *planWorld) { w.visErr["acme/api"] = boom }, []string{"acme/api"}, false},
		{"the source can't be read", func(w *planWorld) { w.visErr[planSource] = boom }, []string{"acme/web"}, false},
		{"the platform can't tell", func(w *planWorld) { w.visErr["acme/web"] = forge.ErrUnsupported }, []string{"acme/web"}, false},
		{"an unknown visibility", func(w *planWorld) { w.vis["acme/web"] = "secret" }, []string{"acme/web"}, false},
		{"a target that is not owner/name", nil, []string{"acme/../api"}, false},
		{"the repository itself", nil, []string{planSource}, false},
		{"no target", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPlanWorld()
			if tc.edit != nil {
				tc.edit(w)
			}
			err := checkImpactTargets(context.Background(), planSource, tc.targets, w.Visibility)
			if (err == nil) != tc.ok {
				t.Errorf("err = %v; want ok = %t", err, tc.ok)
			}
		})
	}
}

// The direction matters: a public repository may not read a private one, and
// the reverse is allowed.
func TestCheckImpactTargetsDirection(t *testing.T) {
	w := newPlanWorld()
	w.vis[planSource] = forge.VisPublic
	w.vis["acme/api"] = forge.VisPrivate
	if checkImpactTargets(context.Background(), planSource, []string{"acme/api"}, w.Visibility) == nil {
		t.Error("a public repository was allowed a private target")
	}
	w.vis[planSource] = forge.VisPrivate
	w.vis["acme/api"] = forge.VisPublic
	if err := checkImpactTargets(context.Background(), planSource, []string{"acme/api"}, w.Visibility); err != nil {
		t.Errorf("a private repository was refused a public target: %v", err)
	}
}

func TestImpactTargets(t *testing.T) {
	known := []store.Link{{Target: "acme/api"}, {Target: "Acme/Api"}, {Target: "acme/web"}}
	got, err := impactTargets(known, []string{"ACME/web"})
	if err != nil || len(got) != 2 || got[0] != "acme/api" || got[1] != "acme/web" {
		t.Errorf("targets = %v, %v; want the two distinct ones", got, err)
	}
	if _, err := impactTargets(nil, nil); err == nil {
		t.Error("no known link was a pass: a kept check can't be proven")
	}
	if _, err := impactTargets(known, []string{"evil/secret"}); err == nil {
		t.Error("a finding naming a repository that is not linked was a pass")
	}
}

func TestImpactRepos(t *testing.T) {
	f := func(repo string) json.RawMessage {
		b, _ := json.Marshal(result.Finding{Fingerprint: "aaaaaa000001", File: "a.go", Severity: "suggestion", Linked: &result.Linked{Repo: repo, File: "x.go", Line: 1}})
		return b
	}
	plain, _ := json.Marshal(result.Finding{Fingerprint: "bbbbbb000001", File: "a.go", Severity: "suggestion"})
	other, _ := json.Marshal(result.Finding{Fingerprint: "cccccc000001", File: "a.go", Severity: "suggestion", Linked: &result.Linked{Repo: "not/impact"}})
	data, _ := json.Marshal(result.Result{SchemaVersion: 1, Status: "completed", Comment: result.Comment{Markdown: "m"}, Skills: []result.Skill{
		{Name: "security", Verdict: "no_blocking_issues", Findings: []json.RawMessage{other}},
		{Name: result.ImpactSkill, Verdict: "no_blocking_issues", Findings: []json.RawMessage{f("acme/api"), plain, f("acme/web")}},
	}})
	got, err := impactRepos(data)
	if err != nil || len(got) != 2 || got[0] != "acme/api" || got[1] != "acme/web" {
		t.Errorf("repos = %v, %v; want the impact skill's two", got, err)
	}
}
