package inputs

import (
	"strings"
	"testing"
)

func TestCheckSkill(t *testing.T) {
	big := "---\nname: big\n---\n" + strings.Repeat("a", MaxSkillBytes)
	exact := "# " + strings.Repeat("a", MaxSkillBytes-2)
	tests := []struct {
		label, name, content string
		want                 string // substring of the error; empty means ok
	}{
		{"plain", "lint", "# lint", ""},
		{"no frontmatter at all", "lint", "", ""},
		{"exactly the limit", "lint", exact, ""},
		{"frontmatter with lists", "lint", "---\nname: lint\npaths: [\"a/**\"]\nexclude:\n  - b\ncross_batch: true\nother: [1, 2]\n---\nx", ""},
		{"bad name", "../evil", "# x", "invalid skill name"},
		{"empty name", "", "# x", "invalid skill name"},
		{"over the limit", "big", big, "128 KiB"},
		{"bad yaml", "lint", "---\nname: [unclosed\n---\nx", "frontmatter"},
		{"paths is a string", "lint", "---\npaths: \"x\"\n---\nx", "frontmatter"},
		{"exclude is a string", "lint", "---\nexclude: x\n---\nx", "frontmatter"},
		{"paths holds a map", "lint", "---\npaths:\n  - {a: b}\n---\nx", "frontmatter"},
		{"cross_batch is a string", "lint", "---\ncross_batch: \"yes\"\n---\nx", "cross_batch"},
		{"cross_batch is a bare word", "lint", "---\ncross_batch: yes\n---\nx", "cross_batch"},
		{"cross_batch is a number", "lint", "---\ncross_batch: 1\n---\nx", "cross_batch"},
		{"name mismatch", "lint", "---\nname: other\n---\nx", "does not match"},
	}
	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			err := CheckSkill(tc.name, []byte(tc.content))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("err = %v, want none", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestContainsSecret(t *testing.T) {
	content := []byte("use token abc123 here")
	tests := []struct {
		label   string
		secrets []string
		want    bool
	}{
		{"present", []string{"zzz", "abc123"}, true},
		{"absent", []string{"zzz"}, false},
		{"none", nil, false},
		{"empty secrets are skipped", []string{"", ""}, false},
		{"empty beside absent", []string{"", "zzz"}, false},
	}
	for _, tc := range tests {
		if got := ContainsSecret(content, tc.secrets); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.label, got, tc.want)
		}
	}
}
