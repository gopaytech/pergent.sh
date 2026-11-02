package inputs

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidName reports whether name is a legal skill name:
// ^[A-Za-z0-9][A-Za-z0-9._-]*$.
func ValidName(name string) bool { return validName.MatchString(name) }

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

// SkillScope returns the `paths` and `exclude` globs of a skill's frontmatter:
// the files a review gives the skill. Both are nil when the skill has none.
func SkillScope(content []byte) (paths, exclude []string, err error) {
	fm, err := parseFrontmatter(content)
	if err != nil {
		return nil, nil, err
	}
	return fm.Paths, fm.Exclude, nil
}

type frontmatter struct {
	Name       string   `yaml:"name"`
	Paths      []string `yaml:"paths"`
	Exclude    []string `yaml:"exclude"`
	CrossBatch bool     `yaml:"cross_batch"`
}

// parseFrontmatter reads a leading `---\n…\n---` block. Content without one
// (including an unclosed block) has no frontmatter. `name`, `paths`, `exclude`
// and `cross_batch` are read and typed (a string list, a string list, a
// boolean); other fields are ignored.
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
			if err := decodeFrontmatter([]byte(block.String()), &fm); err != nil {
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

// decodeFrontmatter decodes block into fm. The typed fields are checked on the
// loose form first so the error names the field: yaml.v3 would accept the
// word yes for a bool and says nothing about which field was wrong.
func decodeFrontmatter(block []byte, fm *frontmatter) error {
	var loose map[string]any
	if err := yaml.Unmarshal(block, &loose); err != nil {
		return err
	}
	for _, key := range []string{"paths", "exclude"} {
		v, ok := loose[key]
		if !ok || v == nil {
			continue
		}
		list, isList := v.([]any)
		if !isList {
			return fmt.Errorf("%s must be a list of strings", key)
		}
		for _, item := range list {
			if _, isString := item.(string); !isString {
				return fmt.Errorf("%s must be a list of strings", key)
			}
		}
	}
	if v, ok := loose["cross_batch"]; ok && v != nil {
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("cross_batch must be true or false")
		}
	}
	return yaml.Unmarshal(block, fm)
}
