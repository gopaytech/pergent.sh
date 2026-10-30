// Package inputs resolves what a review gets: library skills, repository
// skills and team instructions read from the target branch at a fixed SHA, and
// the previous result. They end up in a Job's ConfigMap.
package inputs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
)

// ConfigMapLimit is the most the inputs may weigh. Kubernetes caps a ConfigMap
// at 1 MiB; the rest is headroom for keys and metadata.
const ConfigMapLimit = 900 * 1024

// ErrTooLarge is returned by Fit when the inputs do not fit even without the
// previous result.
var ErrTooLarge = errors.New("review inputs are too large")

const (
	repoSkillsDir    = ".pergent/skills"
	repoInstructions = ".pergent/instructions.md"
)

// Source reads files of a project at a ref. Every forge.Forge is one.
type Source interface {
	GetFile(ctx context.Context, projectID int64, path, ref string) ([]byte, bool, error)
	ListTree(ctx context.Context, projectID int64, path, ref string) ([]forge.TreeEntry, error)
}

// Library is the set of skills mounted into pergent.sh, name → content.
type Library map[string][]byte

// LoadLibrary reads a skills directory with the layout pergent's own loader
// accepts: top-level "*.md" files and "<name>/SKILL.md" folders. A folder
// without a SKILL.md is an error, entries starting with "." are skipped and
// symlinks (a ConfigMap volume makes every entry one) are followed.
func LoadLibrary(dir string) (Library, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading skills directory: %w", err)
	}

	lib := Library{}
	origin := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}

		isDir := entry.IsDir()
		if entry.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
				isDir = info.IsDir()
			}
		}

		var file, fallback string
		switch {
		case isDir:
			file = filepath.Join(dir, name, "SKILL.md")
			if info, err := os.Stat(file); err != nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s: a skill folder needs a SKILL.md file", filepath.Join(dir, name))
			}
			fallback = name
		case filepath.Ext(name) == ".md":
			file = filepath.Join(dir, name)
			fallback = strings.TrimSuffix(name, ".md")
		default:
			continue
		}

		content, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("reading skill file: %w", err)
		}
		skillName, err := SkillName(content, fallback)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if prev, dup := origin[skillName]; dup {
			return nil, fmt.Errorf("duplicate skill name %q in %s and %s", skillName, prev, file)
		}
		origin[skillName] = file
		lib[skillName] = content
	}
	return lib, nil
}

// Where a resolved skill comes from (Inputs.Sources).
const (
	SourceLibrary = "library"
	SourceRepo    = "repo"
)

// Inputs is everything a review gets besides the diff.
type Inputs struct {
	Skills       map[string][]byte // name → content
	Instructions []byte            // nil when none
	Previous     []byte            // nil when none
	Overridden   []string          // library skills replaced by repo skills, sorted
	// Sources maps each skill's name to SourceLibrary or SourceRepo (the
	// repository's own, also when it replaces a library skill).
	Sources map[string]string
}

// Resolve gathers the inputs for one review. Library skills are picked by
// repo.Skills (all of them when empty); repository skills and instructions are
// read from the project at ref, so a merge request cannot change its own
// rules. A repository skill replaces a library skill of the same name.
// previous is stored as given.
func Resolve(ctx context.Context, src Source, lib Library, repo config.Repository, projectID int64, ref string, previous []byte) (*Inputs, error) {
	in := &Inputs{Skills: map[string][]byte{}, Previous: previous, Sources: map[string]string{}}

	if len(repo.Skills) == 0 {
		for name, content := range lib {
			in.Skills[name] = content
		}
	} else {
		for _, name := range repo.Skills {
			content, ok := lib[name]
			if !ok {
				return nil, fmt.Errorf("skill %q is not in the skill library", name)
			}
			in.Skills[name] = content
		}
	}
	for name := range in.Skills {
		in.Sources[name] = SourceLibrary
	}

	repoSkills, err := loadRepoSkills(ctx, src, projectID, ref)
	if err != nil {
		return nil, err
	}
	for name, content := range repoSkills {
		if _, ok := in.Skills[name]; ok {
			if _, inLib := lib[name]; inLib {
				in.Overridden = append(in.Overridden, name)
			}
		}
		in.Skills[name] = content
		in.Sources[name] = SourceRepo
	}
	sort.Strings(in.Overridden)

	in.Instructions, err = loadInstructions(ctx, src, projectID, ref)
	if err != nil {
		return nil, err
	}
	return in, nil
}

func loadRepoSkills(ctx context.Context, src Source, projectID int64, ref string) (map[string][]byte, error) {
	entries, err := src.ListTree(ctx, projectID, repoSkillsDir, ref)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", repoSkillsDir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	skills := map[string][]byte{}
	origin := map[string]string{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name, ".") {
			continue
		}

		var file, fallback string
		switch {
		case e.Type == "tree":
			file = path.Join(e.Path, "SKILL.md")
			fallback = e.Name
		case e.Type == "blob" && path.Ext(e.Name) == ".md":
			file = e.Path
			fallback = strings.TrimSuffix(e.Name, ".md")
		default:
			continue
		}

		content, found, err := src.GetFile(ctx, projectID, file, ref)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", file, err)
		}
		if !found {
			if e.Type == "tree" {
				return nil, fmt.Errorf("%s: a skill folder needs a SKILL.md file", e.Path)
			}
			return nil, fmt.Errorf("%s: file not found", file)
		}

		name, err := SkillName(content, fallback)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if prev, dup := origin[name]; dup {
			return nil, fmt.Errorf("duplicate skill name %q in %s and %s", name, prev, file)
		}
		origin[name] = file
		skills[name] = content
	}
	return skills, nil
}

func loadInstructions(ctx context.Context, src Source, projectID int64, ref string) ([]byte, error) {
	var out []byte

	projectFile := "AGENTS.md"
	content, found, err := src.GetFile(ctx, projectID, projectFile, ref)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", projectFile, err)
	}
	if !found {
		projectFile = "CLAUDE.md"
		content, found, err = src.GetFile(ctx, projectID, projectFile, ref)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", projectFile, err)
		}
	}
	if found {
		out = append(out, "# Project instructions ("+projectFile+")\n\n"...)
		out = append(out, content...)
		out = append(out, '\n')
	}

	content, found, err = src.GetFile(ctx, projectID, repoInstructions, ref)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", repoInstructions, err)
	}
	if found {
		out = append(out, "\n# Team review instructions ("+repoInstructions+")\n\n"...)
		out = append(out, content...)
		out = append(out, '\n')
	}
	return out, nil
}

// Size is the total number of bytes the inputs put into a ConfigMap.
func (in *Inputs) Size() int {
	n := len(in.Instructions) + len(in.Previous)
	for _, c := range in.Skills {
		n += len(c)
	}
	return n
}

// Fit makes the inputs fit limit bytes: over the limit, the previous result is
// dropped first; if they are still over, it returns ErrTooLarge.
func (in *Inputs) Fit(limit int) error {
	if in.Size() <= limit {
		return nil
	}
	in.Previous = nil
	if size := in.Size(); size > limit {
		return fmt.Errorf("%w: skills and instructions are %d bytes, the limit is %d", ErrTooLarge, size, limit)
	}
	return nil
}
