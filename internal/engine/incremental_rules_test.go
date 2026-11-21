package engine

import (
	"bytes"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }

func TestRulesChanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		last *string
		now  string
		want bool
	}{
		{"same hash", strPtr("h1"), "h1", false},
		{"new hash", strPtr("h1"), "h2", true},
		{"a run from before rules", nil, "h1", true},
	} {
		if got := rulesChanged(tc.last, tc.now); got != tc.want {
			t.Errorf("%s: rulesChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSkillsWithRules(t *testing.T) {
	got := skillsWithRules(map[string][]byte{
		"plain":  []byte("Review."),
		"off":    []byte("---\nrules: false\n---\nReview."),
		"on":     []byte("---\nrules: true\n---\nReview."),
		"unread": []byte("---\nrules: yes\n---\nReview."),
	})
	want := map[string]bool{"plain": true, "on": true, "unread": true}
	if !maps.Equal(got, want) {
		t.Errorf("skillsWithRules = %v, want %v (a field it can't read counts as on)", got, want)
	}
}

func TestCarryRulesChangedFullForRulesSkills(t *testing.T) {
	carried := map[string][]string{"a": {"x.go"}, "b": {"y.go"}}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	got := dropRulesCarry(carried, map[string]bool{"a": true, "b": true}, log)
	if len(got) != 0 {
		t.Errorf("carried = %v, want nothing", got)
	}
	if !strings.Contains(buf.String(), `reason="rules changed"`) || !strings.Contains(buf.String(), "skill=a") {
		t.Errorf("log = %q, want each skill logged with the reason", buf.String())
	}
}

func TestCarryRulesOffSkillCarriesOverRulesChange(t *testing.T) {
	carried := map[string][]string{"on": {"x.go"}, "off": {"y.go"}}
	got := dropRulesCarry(carried, map[string]bool{"on": true}, discardLog)
	if want := map[string][]string{"off": {"y.go"}}; !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("carried = %v, want %v", got, want)
	}
}

func TestCarrySameRulesHashCarries(t *testing.T) {
	carried := map[string][]string{"a": {"x.go"}}
	if got := dropRulesCarry(carried, nil, discardLog); !maps.EqualFunc(got, carried, slices.Equal) {
		t.Errorf("carried = %v, want it unchanged", got)
	}
}
