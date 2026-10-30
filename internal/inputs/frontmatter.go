package inputs

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// SkillName returns the skill's name: the frontmatter `name` when set, else
// fallback. The result must match ^[A-Za-z0-9][A-Za-z0-9._-]*$ so it is safe
// as a file name and in a ConfigMap key.
func SkillName(content []byte, fallback string) (string, error) {
	fm, err := parseFrontmatter(content)
	if err != nil {
		return "", err
	}
	name := fm.Name
	if name == "" {
		name = fallback
	}
	if !validName.MatchString(name) {
		return "", fmt.Errorf("invalid skill name %q: use letters, digits, '.', '_' and '-', starting with a letter or digit", name)
	}
	return name, nil
}

type frontmatter struct {
	Name string `yaml:"name"`
}

// parseFrontmatter reads a leading `---\n…\n---` block. Content without one
// (including an unclosed block) has no frontmatter. Only `name` is read.
func parseFrontmatter(content []byte) (frontmatter, error) {
	var fm frontmatter
	s := string(content)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return fm, nil
	}
	s = s[strings.Index(s, "\n")+1:]

	var block strings.Builder
	for len(s) > 0 {
		line, rest, _ := strings.Cut(s, "\n")
		if strings.TrimRight(line, "\r") == "---" {
			if err := yaml.Unmarshal([]byte(block.String()), &fm); err != nil {
				return frontmatter{}, fmt.Errorf("parsing frontmatter: %w", err)
			}
			return fm, nil
		}
		block.WriteString(line)
		block.WriteString("\n")
		s = rest
	}
	return frontmatter{}, nil
}
