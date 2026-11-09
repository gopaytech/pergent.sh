package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/config"
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
			got := carriedFiles(last, tc.lastHashes, tc.hashes, tc.changed, tc.ds, discardLog)
			if !maps.EqualFunc(got, tc.want, slices.Equal) {
				t.Errorf("carriedFiles = %v, want %v", got, tc.want)
			}
		})
	}
}

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// Every skill that carries nothing is logged with its own reason, also when
// another skill carries.
func TestCarriedFilesLogsWhyASkillCarriesNothing(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
	last := &result.Result{Skills: []result.Skill{
		lastSkill("kept", []string{"a.go"}),
		lastSkill("edited", []string{"a.go"}),
		lastSkill("stray", []string{"a.go"}, lastFinding(t, "fp1", "elsewhere.go", "suggestion")),
		lastSkill("touched", []string{"a.go"}),
	}}
	lastHashes := map[string]string{"kept": "h1", "edited": "h1", "stray": "h1", "touched": "h1"}
	hashes := map[string]string{"kept": "h1", "edited": "h2", "stray": "h1", "touched": "h1", "added": "h3"}

	got := carriedFiles(last, lastHashes, hashes, []string{"a.go"}, nil, log)

	if !maps.EqualFunc(got, map[string][]string{}, slices.Equal) {
		t.Fatalf("carriedFiles = %v, want nothing (a.go changed)", got)
	}
	want := []string{
		`level=INFO msg="nothing carried for a skill" skill=kept reason="nothing unchanged"`,
		`level=INFO msg="nothing carried for a skill" skill=edited reason=changed`,
		`level=INFO msg="nothing carried for a skill" skill=stray reason="finding outside reviewed files"`,
		`level=INFO msg="nothing carried for a skill" skill=touched reason="nothing unchanged"`,
		`level=INFO msg="nothing carried for a skill" skill=added reason=changed`,
	}
	if have := strings.Split(strings.TrimSpace(buf.String()), "\n"); !slices.Equal(have, want) {
		t.Errorf("log =\n%s\nwant\n%s", strings.Join(have, "\n"), strings.Join(want, "\n"))
	}

	// A skill that carries is not logged.
	buf.Reset()
	got = carriedFiles(last, lastHashes, hashes, []string{"other.go"}, nil, log)
	if len(got["kept"]) != 1 || strings.Contains(buf.String(), "skill=kept") {
		t.Errorf("carried = %v, log = %q; want kept carried and not logged", got, buf.String())
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
		d:        &dispatcher{e: &Engine{Config: &config.Config{}}, run: store.Run{HeadSHA: sha('2'), TargetBranch: "main"}},
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
		{"the Jobs image is pinned to an older pergent", func(t *testing.T, f *planFixture) {
			f.d.e.Config.Jobs.Image = "ghcr.io/gopaytech/pergent:v0.4.0"
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

// A review is a full one when the model it will run on is not the one the last
// review ran on. What an older run never recorded, and a run with nothing
// resolved, say nothing: they are no change.
func TestModelChanged(t *testing.T) {
	p, m, other := "anthropic", "claude-a", "claude-b"
	for name, tc := range map[string]struct {
		last            store.Run
		provider, model string
		want            bool
	}{
		"same":                {store.Run{Provider: &p, Model: &m}, p, m, false},
		"other model":         {store.Run{Provider: &p, Model: &m}, p, other, true},
		"other provider":      {store.Run{Provider: &p, Model: &m}, "litellm", m, true},
		"upgrade, same model": {store.Run{Model: &m}, "litellm", m, false},
		"upgrade, other":      {store.Run{Model: &m}, p, other, true},
		"never recorded":      {store.Run{}, p, m, false},
		"nothing resolved":    {store.Run{Provider: &p, Model: &m}, "", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := modelChanged(tc.last, tc.provider, tc.model); got != tc.want {
				t.Errorf("modelChanged = %v, want %v", got, tc.want)
			}
		})
	}
}

// A push whose model differs from the last review's is reviewed in full. The
// cases that carry files need the store (past dismissals), and no change is
// what TestModelChanged pins.
func TestPlanCarriedFullWhenModelChanged(t *testing.T) {
	p, m := "anthropic", "claude-a"
	for _, tc := range []struct {
		name            string
		provider, model string
	}{
		{"other model", p, "claude-b"},
		{"other provider", "litellm", m},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanFixture(t)
			f.last.Provider, f.last.Model = &p, &m
			f.d.provider, f.d.model = tc.provider, tc.model

			carried, reason := f.plan(t)

			if len(carried) != 0 || reason != fullModelChanged {
				t.Errorf("planCarried = %v, %q; want a full review: %q", carried, reason, fullModelChanged)
			}
		})
	}
}

// The tag of the Jobs image, when it names a pergent version.
func TestImageTag(t *testing.T) {
	for image, want := range map[string]string{
		"ghcr.io/gopaytech/pergent:v0.5.0":               "v0.5.0",
		"ghcr.io/gopaytech/pergent:0.4.0":                "0.4.0",
		"ghcr.io/gopaytech/pergent:v0.4.0@sha256:abc123": "v0.4.0",
		"host:5000/pergent:v0.4.0":                       "v0.4.0",
		"ghcr.io/gopaytech/pergent@sha256:abc123":        "",
		"host:5000/pergent":                              "",
		"host:5000/pergent@sha256:abc123":                "",
		"pergent":                                        "",
		"ghcr.io/gopaytech/pergent:latest":               "latest",
		"ghcr.io/gopaytech/pergent:main-4f2a":            "main-4f2a",
		"":                                               "",
	} {
		if got := imageTag(image); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", image, got, want)
		}
	}
}

// Only a tag that is exactly a version below 0.5.0 gives a full review; a
// digest, another tag or a newer version leaves the gate to the last result.
func TestImagePinnedBeforeCarried(t *testing.T) {
	for image, want := range map[string]bool{
		"ghcr.io/gopaytech/pergent:v0.4.0":               true,
		"ghcr.io/gopaytech/pergent:0.4.9":                true,
		"ghcr.io/gopaytech/pergent:v0.4.0@sha256:abc123": true,
		"ghcr.io/gopaytech/pergent:v0.5.0":               false,
		"ghcr.io/gopaytech/pergent:v1.0.0":               false,
		"ghcr.io/gopaytech/pergent:v0.5.0-rc1":           false,
		"ghcr.io/gopaytech/pergent:latest":               false,
		"ghcr.io/gopaytech/pergent@sha256:abc123":        false,
		"host:5000/pergent":                              false,
		"host:5000/pergent:v0.4.0":                       true,
	} {
		if got := imagePinnedBeforeCarried(image); got != want {
			t.Errorf("imagePinnedBeforeCarried(%q) = %v, want %v", image, got, want)
		}
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
