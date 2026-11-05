// Package inputs resolves what a review gets: library skills, repository
// skills and team instructions read from the target branch at a fixed SHA, and
// the previous result. They end up in a Job's ConfigMap.
package inputs

import (
	"context"
	"errors"
	"fmt"
	"sort"

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
	// Versions maps each library skill to the version read (ResolveSkills);
	// skills of a repository folder are not in it.
	Versions map[string]int
	// Paths maps each repository skill to the file it was read from, in the
	// repository; library skills are not in it.
	Paths map[string]string
	// Carried is the JSON of the files an incremental review leaves out, per
	// skill ({"<skill>": ["path", ...]}); nil for a full review.
	Carried []byte
	// Dismissals is the JSON of what people rejected in this repository, per
	// skill ({"<skill>": [{"file", "title", "severity", "reason"}]}); nil when
	// there is none. It is not part of any skill's hash.
	Dismissals []byte
}

// LibraryEntry is one skill of the library as ResolveSkills receives it.
type LibraryEntry struct {
	Content []byte
	Version int
}

// NotFoundError says a skill turned on for a repository is in none of the
// repository's skill folders.
type NotFoundError struct{ Name string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("skill %q is turned on but not found in the repository", e.Name)
}

// ProblemError says a skill file ResolveSkills had to use is malformed: one
// of ".pergent/skills" (always used) or a turned-on one of another folder.
// Name is the skill's name, or the file or folder name when it has none.
type ProblemError struct{ Name, Path, Problem string }

func (e *ProblemError) Error() string { return e.Path + ": " + e.Problem }

// ResolveSkills gathers the inputs for one review. lib holds the library
// skills the repository uses (its list, or the default list); repoSkills names
// the detected skills turned on. Repository skills and instructions are read
// from the project at ref, so a merge request cannot change its own rules.
//
// A name is taken from the first of RepoFolders that has it, then the library
// (D23). ".pergent/skills" always runs and a problem there is an error. The
// other folders are read only for the names in repoSkills: a skill that is off
// is ignored even when malformed and replaces nothing, a turned-on one that is
// malformed is an error and one that is in no folder is a *NotFoundError.
// previous is stored as given.
func ResolveSkills(ctx context.Context, src Source, lib map[string]LibraryEntry, repoSkills []string, projectID int64, ref string, previous []byte) (*Inputs, error) {
	in := &Inputs{
		Skills: map[string][]byte{}, Previous: previous,
		Sources: map[string]string{}, Versions: map[string]int{}, Paths: map[string]string{},
	}
	for name, e := range lib {
		in.Skills[name] = e.Content
		in.Sources[name] = SourceLibrary
		in.Versions[name] = e.Version
	}

	take := func(f Found, folder string) {
		in.Skills[f.Name] = f.Content
		in.Sources[f.Name] = folder
		in.Paths[f.Name] = f.Path
		delete(in.Versions, f.Name)
	}

	// found is what the repository folders supply by name so far.
	found := map[string]bool{}

	always, err := ScanFolder(ctx, src, projectID, RepoFolders[0], ref)
	if err != nil {
		return nil, err
	}
	for _, f := range always {
		if f.Problem != "" {
			return nil, &ProblemError{Name: f.Name, Path: f.Path, Problem: f.Problem}
		}
		take(f, RepoFolders[0])
		found[f.Name] = true
	}

	on := map[string]bool{}
	for _, name := range repoSkills {
		on[name] = true
	}
	for _, folder := range RepoFolders[1:] {
		if allFound(on, found) {
			break
		}
		scanned, err := ScanFolder(ctx, src, projectID, folder, ref)
		if err != nil {
			return nil, err
		}
		var supplied []Found
		for _, f := range scanned {
			if !on[f.Name] || found[f.Name] {
				continue
			}
			if f.Problem != "" {
				return nil, &ProblemError{Name: f.Name, Path: f.Path, Problem: f.Problem}
			}
			take(f, folder)
			supplied = append(supplied, f)
		}
		for _, f := range supplied {
			found[f.Name] = true
		}
	}
	for _, name := range repoSkills {
		if !found[name] {
			return nil, &NotFoundError{Name: name}
		}
	}

	for name := range found {
		if _, inLib := lib[name]; inLib {
			in.Overridden = append(in.Overridden, name)
		}
	}
	sort.Strings(in.Overridden)

	in.Instructions, err = LoadInstructions(ctx, src, projectID, ref)
	if err != nil {
		return nil, err
	}
	return in, nil
}

func allFound(on, found map[string]bool) bool {
	for name := range on {
		if !found[name] {
			return false
		}
	}
	return true
}

// Only is the inputs reduced to the skills named in names: a name that is not
// among the skills (after the repository's own were merged in) is left out.
// The instructions and the previous result are shared with in.
func (in *Inputs) Only(names []string) *Inputs {
	out := &Inputs{
		Skills: map[string][]byte{}, Sources: map[string]string{},
		Instructions: in.Instructions, Previous: in.Previous,
	}
	if in.Versions != nil {
		out.Versions = map[string]int{}
	}
	if in.Paths != nil {
		out.Paths = map[string]string{}
	}
	for _, name := range names {
		content, ok := in.Skills[name]
		if !ok {
			continue
		}
		out.Skills[name] = content
		if src, ok := in.Sources[name]; ok {
			out.Sources[name] = src
		}
		if v, ok := in.Versions[name]; ok {
			out.Versions[name] = v
		}
		if p, ok := in.Paths[name]; ok {
			out.Paths[name] = p
		}
	}
	for _, name := range in.Overridden {
		if _, ok := out.Skills[name]; ok {
			out.Overridden = append(out.Overridden, name)
		}
	}
	return out
}

// LoadInstructions reads the instructions a review gets at ref: the root
// AGENTS.md (or CLAUDE.md when there is none) and .pergent/instructions.md,
// each under a heading. It is nil when the project has neither.
func LoadInstructions(ctx context.Context, src Source, projectID int64, ref string) ([]byte, error) {
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
	n := len(in.Instructions) + len(in.Previous) + len(in.Carried) + len(in.Dismissals)
	for _, c := range in.Skills {
		n += len(c)
	}
	return n
}

// Fit makes the inputs fit limit bytes: over the limit, the dismissals are
// dropped first, so a long list never turns an incremental review into a full
// one; then the previous result, together with the carried files (the review
// is then a full one). If they are still over, it returns ErrTooLarge.
func (in *Inputs) Fit(limit int) error {
	if in.Size() <= limit {
		return nil
	}
	in.Dismissals = nil
	if in.Size() <= limit {
		return nil
	}
	in.Previous = nil
	in.Carried = nil
	if size := in.Size(); size > limit {
		return fmt.Errorf("%w: skills and instructions are %d bytes, the limit is %d", ErrTooLarge, size, limit)
	}
	return nil
}
