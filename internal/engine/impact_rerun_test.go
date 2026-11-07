package engine

import (
	"context"
	"slices"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func inputsOf(names ...string) *inputs.Inputs {
	in := &inputs.Inputs{Skills: map[string][]byte{}}
	for _, n := range names {
		in.Skills[n] = []byte(n)
	}
	return in
}

// resultOf is a completed result of the named skills.
func resultOf(names ...string) *result.Result {
	r := &result.Result{Status: "completed"}
	for _, n := range names {
		r.Skills = append(r.Skills, result.Skill{Name: n, Verdict: "no_blocking_issues"})
	}
	return r
}

// The impact check is not one of the repository's skills: an earlier result
// that has it is of the same skills, and does not read as "skills changed",
// which would turn a re-run of some skills into a full review.
func TestSkillsChangedIgnoresImpact(t *testing.T) {
	in := inputsOf("security", "style")
	for _, tc := range []struct {
		name string
		r    *result.Result
		want string
	}{
		{"with impact", resultOf("security", "impact", "style"), ""},
		{"without impact", resultOf("security", "style"), ""},
		{"a skill added", resultOf("security", "impact"), "the skills changed since the earlier result"},
		{"a skill gone", resultOf("security", "style", "other", "impact"), "the skills changed since the earlier result"},
		{"impact in place of a skill", resultOf("security", "impact"), "the skills changed since the earlier result"},
		{"impact twice", resultOf("security", "style", "impact", "impact"), "the earlier result has a skill more than once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := skillsChanged(tc.r, in); got != tc.want {
				t.Errorf("skillsChanged = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestSplitImpact(t *testing.T) {
	rest, impact := splitImpact([]string{"security", "impact", "style"})
	if !impact || !slices.Equal(rest, []string{"security", "style"}) {
		t.Errorf("splitImpact = %v, %t", rest, impact)
	}
	rest, impact = splitImpact([]string{"security"})
	if impact || !slices.Equal(rest, []string{"security"}) {
		t.Errorf("splitImpact = %v, %t", rest, impact)
	}
	if rest, impact = splitImpact([]string{"impact"}); !impact || len(rest) != 0 {
		t.Errorf("splitImpact = %v, %t; want impact alone", rest, impact)
	}
}

// pergent takes at least one skill, so a re-run of the check alone runs it
// with one: the first by name, which is then re-run and merged as well.
func TestImpactAloneNeedsACarrier(t *testing.T) {
	in := inputsOf("style", "security")
	c, ok := carrierSkill(in)
	if !ok || c != "security" {
		t.Fatalf("carrier = %q, %t; want the first by name", c, ok)
	}
	if _, ok := carrierSkill(inputsOf()); ok {
		t.Error("a carrier from no skills")
	}
	only := in.Only([]string{c})
	if got := recordedSubset(only, []string{c}, true); !slices.Equal(got, []string{"security", "impact"}) {
		t.Errorf("subset = %v; want the carrier, then impact", got)
	}
}

func TestRecordedSubset(t *testing.T) {
	in := inputsOf("security", "style")
	only := in.Only([]string{"style", "gone"})
	if got := recordedSubset(only, []string{"style", "gone"}, false); !slices.Equal(got, []string{"style"}) {
		t.Errorf("subset = %v; want the skills that exist", got)
	}
	if got := recordedSubset(only, []string{"style", "gone"}, true); !slices.Equal(got, []string{"style", "impact"}) {
		t.Errorf("subset = %v; want impact last", got)
	}
}

// A re-run of some skills that doesn't re-run the check takes no links: its
// result would carry a skill nothing asked for, and checkSkills would refuse
// it. One that re-runs the check goes over every link.
func TestRerunOfOtherSkillsTakesNoLinks(t *testing.T) {
	on := settings.Effective{LinkedReposEnabled: true, LinkedRepos: []store.Link{{Target: "acme/api", Watched: []string{"render/**"}}}}
	plat := Platform{Kind: config.PlatformGitHub}
	d := &dispatcher{partial: true}
	got := d.linkedRepos(context.Background(), plat, on, store.Project{}, forge.Change{}, store.Run{}, false, &inputs.Inputs{})
	if len(got.Entries) != 0 || got.Matched != nil {
		t.Errorf("plan = %+v; want none for a re-run of other skills", got)
	}
}
