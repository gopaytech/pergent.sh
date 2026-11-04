package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// lastFinding is a finding of a last review, raw as the result stores it.
func lastFinding(t *testing.T, fp, file, severity string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(result.Finding{Fingerprint: fp, File: file, Severity: severity, Title: "t", Explanation: "e"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// lastSkill is a skill of a last review that reviewed files and reported
// findings.
func lastSkill(name string, reviewed []string, findings ...json.RawMessage) result.Skill {
	return result.Skill{
		Name: name, Verdict: "no_blocking_issues", Findings: findings,
		Files: result.Files{Reviewed: reviewed},
	}
}

func TestCarriedFiles(t *testing.T) {
	same := map[string]string{"security": "h1"}
	for _, tc := range []struct {
		name       string
		skills     func(t *testing.T) []result.Skill
		lastHashes map[string]string
		hashes     map[string]string
		changed    []string
		ds         []render.Dismissal
		want       map[string][]string
	}{
		{
			name: "a reviewed, unchanged file is carried and a changed one is not",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go", "b.go", "c.go"})}
			},
			lastHashes: same, hashes: same, changed: []string{"b.go"},
			want: map[string][]string{"security": {"a.go", "c.go"}},
		},
		{
			name: "the paths are sorted and each is listed once",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"z.go", "a.go", "z.go", "m.go"})}
			},
			lastHashes: same, hashes: same,
			want: map[string][]string{"security": {"a.go", "m.go", "z.go"}},
		},
		{
			name: "a file that was not reviewed is not carried",
			skills: func(t *testing.T) []result.Skill {
				s := lastSkill("security", []string{"a.go"})
				s.Files.NotReviewed = []json.RawMessage{json.RawMessage(`{"path":"b.go","reason":"failed"}`)}
				return []result.Skill{s}
			},
			lastHashes: same, hashes: same,
			want: map[string][]string{"security": {"a.go"}},
		},
		{
			name: "a file with an open blocking finding is not carried",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go", "b.go"}, lastFinding(t, "fp1", "b.go", "blocking"))}
			},
			lastHashes: same, hashes: same,
			want: map[string][]string{"security": {"a.go"}},
		},
		{
			name: "a file with only a suggestion is carried",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go", "b.go"}, lastFinding(t, "fp1", "b.go", "suggestion"))}
			},
			lastHashes: same, hashes: same,
			want: map[string][]string{"security": {"a.go", "b.go"}},
		},
		{
			name: "a file whose blocking finding was dismissed is carried",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go", "b.go"}, lastFinding(t, "fp1", "b.go", "blocking"))}
			},
			lastHashes: same, hashes: same,
			ds:   []render.Dismissal{{Fingerprint: "fp1", Severity: "blocking", By: "alice"}},
			want: map[string][]string{"security": {"a.go", "b.go"}},
		},
		{
			name: "a dismissal of another finding does not free a blocker's file",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go", "b.go"},
					lastFinding(t, "fp1", "b.go", "blocking"), lastFinding(t, "fp2", "a.go", "blocking"))}
			},
			lastHashes: same, hashes: same,
			ds:   []render.Dismissal{{Fingerprint: "fp2", Severity: "blocking"}},
			want: map[string][]string{"security": {"a.go"}},
		},
		{
			name: "a skill whose hash differs has no entry",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go"})}
			},
			lastHashes: same, hashes: map[string]string{"security": "h2"},
			want: nil,
		},
		{
			name: "a skill with no hash in the last run has no entry",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go"})}
			},
			lastHashes: nil, hashes: same,
			want: nil,
		},
		{
			name: "a skill with no hash now has no entry",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go"})}
			},
			lastHashes: same, hashes: nil,
			want: nil,
		},
		{
			name: "an empty hash is not a match",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go"})}
			},
			lastHashes: map[string]string{"security": ""}, hashes: map[string]string{"security": ""},
			want: nil,
		},
		{
			name: "a finding on a file outside files.reviewed leaves the skill with no entry",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go"}, lastFinding(t, "fp1", "elsewhere.go", "suggestion"))}
			},
			lastHashes: same, hashes: same,
			want: nil,
		},
		{
			name: "a dismissed finding outside files.reviewed counts too",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go"}, lastFinding(t, "fp1", "elsewhere.go", "blocking"))}
			},
			lastHashes: same, hashes: same,
			ds:   []render.Dismissal{{Fingerprint: "fp1", Severity: "blocking"}},
			want: nil,
		},
		{
			name: "a skill with nothing to carry has no entry",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go", "b.go"}, lastFinding(t, "fp1", "b.go", "blocking"))}
			},
			lastHashes: same, hashes: same, changed: []string{"a.go"},
			want: nil,
		},
		{
			name: "a skill that reviewed nothing has no entry",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", nil)}
			},
			lastHashes: same, hashes: same,
			want: nil,
		},
		{
			name: "each skill is decided on its own",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{
					lastSkill("security", []string{"a.go", "b.go"}),
					lastSkill("style", []string{"a.go", "b.go"}),
					lastSkill("docs", []string{"a.go"}, lastFinding(t, "fp1", "x.md", "suggestion")),
				}
			},
			lastHashes: map[string]string{"security": "h1", "style": "s1", "docs": "d1"},
			hashes:     map[string]string{"security": "h1", "style": "s2", "docs": "d1"},
			changed:    []string{"b.go"},
			want:       map[string][]string{"security": {"a.go"}},
		},
		{
			name: "a skill the last review has twice has no entry",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("security", []string{"a.go"}), lastSkill("security", []string{"b.go"})}
			},
			lastHashes: same, hashes: same,
			want: nil,
		},
		{
			name: "a skill the last review lacks has no entry",
			skills: func(t *testing.T) []result.Skill {
				return []result.Skill{lastSkill("style", []string{"a.go"})}
			},
			lastHashes: same, hashes: same,
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			last := &result.Result{Skills: tc.skills(t)}
			got := carriedFiles(last, tc.lastHashes, tc.hashes, tc.changed, tc.ds)
			if !maps.EqualFunc(got, tc.want, slices.Equal) {
				t.Errorf("carriedFiles = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSkillHashes(t *testing.T) {
	got := skillHashes(map[string][]byte{"security": []byte("Look for problems.\n"), "empty": nil})
	sum := sha256.Sum256([]byte("Look for problems.\n"))
	empty := sha256.Sum256(nil)
	want := map[string]string{"security": hex.EncodeToString(sum[:]), "empty": hex.EncodeToString(empty[:])}
	if !maps.Equal(got, want) {
		t.Errorf("skillHashes = %v, want %v", got, want)
	}
	if len(skillHashes(nil)) != 0 {
		t.Errorf("skillHashes(nil) = %v", skillHashes(nil))
	}
}

// planFixture is a dispatcher, platform and last review for planCarried, none
// of which reaches the store: every full review decided before the
// dismissals are read can be tested without a database.
type planFixture struct {
	d        *dispatcher
	plat     Platform
	repo     settings.Effective
	last     store.Run
	haveLast bool
	in       *inputs.Inputs
}

// forgeWithInline is a forge that can compare commits.
type forgeWithInline struct {
	forge.Forge
	forge.Inline
}

func newPlanFixture(t *testing.T) *planFixture {
	t.Helper()
	body, err := json.Marshal(result.Result{
		SchemaVersion: result.SchemaVersion, Status: "completed", PergentVersion: "0.5.0",
		Base: sha('b'), Head: sha('1'),
		Skills:  []result.Skill{lastSkill("security", []string{"a.go"})},
		Comment: result.Comment{Markdown: testMarkdown},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &planFixture{
		d:        &dispatcher{run: store.Run{HeadSHA: sha('2'), TargetBranch: "main"}},
		plat:     Platform{Forge: forgeWithInline{Inline: &fakeInline{changed: []string{"a.go"}}}},
		repo:     settings.Effective{IncrementalReviews: true},
		last:     store.Run{HeadSHA: sha('1'), TargetBranch: "main", Result: body},
		haveLast: true,
		in:       &inputs.Inputs{Skills: map[string][]byte{"security": []byte("Look.\n")}},
	}
}

func (f *planFixture) plan(t *testing.T) (map[string][]string, string) {
	t.Helper()
	carried, reason, err := f.d.planCarried(context.Background(), f.plat, f.repo, f.last, f.haveLast, f.in)
	if err != nil {
		t.Fatalf("planCarried: %v", err)
	}
	return carried, reason
}

// A push is reviewed in full, for a reason the log gives, whenever one of the
// conditions of an incremental review does not hold.
func TestPlanCarriedFullReviews(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(t *testing.T, f *planFixture)
		reason string
	}{
		{"setting off", func(t *testing.T, f *planFixture) { f.repo.IncrementalReviews = false }, "setting off"},
		{"no earlier review", func(t *testing.T, f *planFixture) { f.haveLast = false; f.last = store.Run{} }, "no earlier review"},
		{"target branch changed", func(t *testing.T, f *planFixture) { f.last.TargetBranch = "develop" }, "target branch changed"},
		{"same head", func(t *testing.T, f *planFixture) { f.last.HeadSHA = f.d.run.HeadSHA }, "same head"},
		{"the platform can't compare commits", func(t *testing.T, f *planFixture) {
			f.plat = Platform{Forge: struct{ forge.Forge }{}}
		}, "the platform can't compare commits"},
		{"the last result can't be read", func(t *testing.T, f *planFixture) { f.last.Result = []byte(`{`) }, "the last review can't be read"},
		{"older pergent", func(t *testing.T, f *planFixture) {
			f.last.Result = bytes.Replace(f.last.Result, []byte(`"0.5.0"`), []byte(`"0.4.0"`), 1)
		}, "older pergent"},
		{"no version", func(t *testing.T, f *planFixture) {
			f.last.Result = bytes.Replace(f.last.Result, []byte(`"pergent_version":"0.5.0"`), []byte(`"pergent_version":""`), 1)
		}, "older pergent"},
		{"the last review is partial", func(t *testing.T, f *planFixture) {
			f.last.Result = bytes.Replace(f.last.Result, []byte(`"head"`), []byte(`"merge_note":"x","head"`), 1)
		}, "the last review is partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanFixture(t)
			tc.edit(t, f)
			carried, reason := f.plan(t)
			if len(carried) != 0 || reason != tc.reason {
				t.Errorf("planCarried = %v, %q; want a full review: %q", carried, reason, tc.reason)
			}
		})
	}
}

// The platform's error is the reason of the full review, without secrets.
func TestPlanCarriedCompareErrorIsTheReason(t *testing.T) {
	f := newPlanFixture(t)
	f.plat = Platform{Forge: forgeWithInline{Inline: &fakeInline{changedErr: errors.New("gitlab: 500 with token s3cret-example")}}}
	f.d.secrets = []string{"s3cret-example"}

	carried, reason := f.plan(t)

	if len(carried) != 0 || reason != "gitlab: 500 with token ***" {
		t.Errorf("planCarried = %v, %q", carried, reason)
	}
}
