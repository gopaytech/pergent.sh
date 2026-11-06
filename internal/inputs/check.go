package inputs

import (
	"bytes"
	"fmt"

	"github.com/gopaytech/pergent/scope"
)

// Limits on the skill library (D6).
const (
	MaxSkillBytes    = 128 << 10 // one skill's text
	MaxLibrarySkills = 200
)

// CheckSkill reports why content cannot be the skill called name: the name
// does not match ^[A-Za-z0-9][A-Za-z0-9._-]*$, the text is over MaxSkillBytes,
// the frontmatter does not parse or has a field of the wrong type (`paths` and
// `exclude` are string lists, `cross_batch` a boolean), or its `name` differs
// from name, or a `paths` or `exclude` pattern is one pergent refuses at load
// (scope.CheckPattern: bad glob syntax, or one that can never match, such as
// "api/"). Other frontmatter fields are ignored.
func CheckSkill(name string, content []byte) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid skill name %q: use letters, digits, '.', '_' and '-', starting with a letter or digit", name)
	}
	if len(content) > MaxSkillBytes {
		return fmt.Errorf("skill is %d bytes, over the limit of %d (128 KiB)", len(content), MaxSkillBytes)
	}
	fm, err := parseFrontmatter(content)
	if err != nil {
		return err
	}
	if fm.Name != "" && fm.Name != name {
		return fmt.Errorf("frontmatter name %q does not match the skill name %q", fm.Name, name)
	}
	for _, field := range []struct {
		name     string
		patterns []string
	}{{"paths", fm.Paths}, {"exclude", fm.Exclude}} {
		for _, p := range field.patterns {
			// pergent fails every review that uses a skill with such a
			// pattern (skills_invalid), so it is never saved.
			if err := scope.CheckPattern(p); err != nil {
				return fmt.Errorf("%s: %w", field.name, err)
			}
		}
	}
	return nil
}

// ContainsSecret reports whether content holds any of secrets verbatim. Empty
// secrets are skipped: they would match everything.
func ContainsSecret(content []byte, secrets []string) bool {
	for _, s := range secrets {
		if s != "" && bytes.Contains(content, []byte(s)) {
			return true
		}
	}
	return false
}
