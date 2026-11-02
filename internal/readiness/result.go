// Package readiness is the repository readiness scan: what the scan Job sends
// and receives, the scan of a checkout it runs, and the checklist pergent.sh
// builds from the results. It is pure: no store, platform or cluster.
package readiness

import (
	"errors"
	"fmt"
	"time"

	"github.com/gopaytech/pergent.sh/internal/inputs"
)

// Limits of the scan. A library skill's folder is inputs.SourceLibrary.
const (
	ResultVersion = 1

	MaxResultBytes = 1 << 20   // the result the Job posts
	MaxInputBytes  = 256 << 10 // the JobInput the Job receives

	MaxNoisePaths = 50  // noise files listed, largest first
	MaxPathBytes  = 512 // a longer path is counted, never listed

	NoiseSize = 512 << 10 // a file over this is noise
)

// ProblemNotScanned is the Problem of an effective library skill the scan did
// not see, such as one added to the library after the scan. Its counts are
// unknown, never zero.
const ProblemNotScanned = "not scanned yet"

// ScopedSkill is a library skill as the Job receives it: its globs, never its
// text. Assigned says it is in the repository's list (or the default list) at
// scan time.
type ScopedSkill struct {
	Name     string   `json:"name"`
	Paths    []string `json:"paths,omitempty"`
	Exclude  []string `json:"exclude,omitempty"`
	Assigned bool     `json:"assigned,omitempty"`
}

// JobInput is what the scan Job is told: every library skill, and the
// repository's turned-on folder skills (repoSkills).
type JobInput struct {
	Version    int           `json:"version"`
	Library    []ScopedSkill `json:"library,omitempty"`
	RepoSkills []string      `json:"repoSkills,omitempty"`
}

// SkillFit is how one skill fits the checkout. Folder is inputs.SourceLibrary
// or one of inputs.RepoFolders. Files counts the files a review gives the
// skill (scope.Decide is ""). Bytes is a repository skill's size; library
// skill sizes are known to pergent.sh. A Problem makes the skill unusable: a
// malformed repository skill, or one Effective could not find.
type SkillFit struct {
	Name     string `json:"name"`
	Folder   string `json:"folder,omitempty"`
	Problem  string `json:"problem,omitempty"`
	HasPaths bool   `json:"hasPaths,omitempty"`
	Files    int    `json:"files,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
}

// Covers reports whether a review with this skill checks something: it is
// usable and its paths match a file, or it has no paths and so applies to
// every repository.
func (f SkillFit) Covers() bool { return f.Problem == "" && (f.Files > 0 || !f.HasPaths) }

// Language is a language over 10% of the source files. Uncovered says no
// effective skill reviews any of them.
type Language struct {
	Name      string `json:"name"`
	Files     int    `json:"files"`
	Uncovered bool   `json:"uncovered,omitempty"`
}

// Why a file is noise.
const (
	WhyGeneratedName = "generated or minified name"
	WhyBuildOutput   = "build output"
	WhyLarge         = "larger than 512 KiB"
)

// NoiseFile is a file a review would read that looks like noise. Skills are
// the effective skills that review it.
type NoiseFile struct {
	Path   string   `json:"path"`
	Why    string   `json:"why"`
	Size   int64    `json:"size"`
	Skills []string `json:"skills,omitempty"`
}

// Context is the project context found at the root of the checkout. Paths are
// as found (README and CONTRIBUTING.md in any case); "" means none.
type Context struct {
	AgentFile      string `json:"agentFile,omitempty"` // "AGENTS.md", "CLAUDE.md" or ""
	Readme         string `json:"readme,omitempty"`
	Contributing   string `json:"contributing,omitempty"`
	CodeOwners     string `json:"codeOwners,omitempty"`
	AgentFileEmpty bool   `json:"agentFileEmpty,omitempty"` // only whitespace
	Instructions   bool   `json:"instructions,omitempty"`   // .pergent/instructions.md
	CodeOwnerRules int    `json:"codeOwnerRules,omitempty"`
}

// JobResult is what the scan Job posts. Error is set when it could not scan
// the files; then nothing else but Version and Commit is. InstructionsBytes is
// the size of the instructions a review would get (inputs.LoadInstructions).
type JobResult struct {
	Version           int         `json:"version"`
	Commit            string      `json:"commit,omitempty"`
	Error             string      `json:"error,omitempty"`
	Files             int         `json:"files,omitempty"`
	SourceFiles       int         `json:"sourceFiles,omitempty"`
	NoiseTotal        int         `json:"noiseTotal,omitempty"`
	InstructionsBytes int         `json:"instructionsBytes,omitempty"`
	Context           Context     `json:"context,omitzero"`
	Skills            []SkillFit  `json:"skills,omitempty"`
	Languages         []Language  `json:"languages,omitempty"`
	Noise             []NoiseFile `json:"noise,omitempty"`
}

// ScanOutcome is what became of a posted result (the engine returns it, the
// callback reads it).
type ScanOutcome int

const (
	ScanStored ScanOutcome = iota + 1
	ScanUnauthorized
	ScanConflict
)

// ErrScansOff refuses a scan when scans are off: they need the dashboard.
var ErrScansOff = errors.New("readiness scans need the dashboard")

// ErrNotSwitchedOn refuses a scan of a repository that is not switched on.
var ErrNotSwitchedOn = errors.New("the repository is not switched on")

// ErrScanBusy refuses a scan asked for while the repository has one queued or
// running; the engine returns that scan with it.
var ErrScanBusy = errors.New("a scan is already queued or running")

// ScanTooSoonError refuses a scan asked for within 5 minutes of the
// repository's latest one; the engine returns that scan with it. Wait is how
// long until a scan is accepted.
type ScanTooSoonError struct{ Wait time.Duration }

func (e *ScanTooSoonError) Error() string {
	return fmt.Sprintf("a scan ran less than 5 minutes ago; scan again in %s", e.Wait.Round(time.Second))
}

// Effective is the skills a review would run, in D23 order: every skill of
// .pergent/skills, each repoSkills name from the first other folder that has
// it, then the library names not replaced by those. It mirrors
// inputs.ResolveSkills, so a review that would fail shows as a Problem: a
// repoSkills name in no folder gets a fit with no Folder and the
// inputs.NotFoundError text, and a library name with no fit (added after the
// scan) one with ProblemNotScanned.
func Effective(fits []SkillFit, library, repoSkills []string) []SkillFit {
	var out []SkillFit
	supplied := map[string]bool{}
	for _, f := range fits {
		if f.Folder == inputs.RepoFolders[0] {
			out = append(out, f)
			supplied[f.Name] = true
		}
	}
	for _, name := range repoSkills {
		if supplied[name] {
			continue
		}
		supplied[name] = true
		found := false
		for _, folder := range inputs.RepoFolders[1:] {
			// Every entry of that name: a duplicate in the folder fails a review.
			for _, f := range fits {
				if f.Folder == folder && f.Name == name {
					out = append(out, f)
					found = true
				}
			}
			if found {
				break
			}
		}
		if !found {
			out = append(out, SkillFit{Name: name, Problem: (&inputs.NotFoundError{Name: name}).Error()})
		}
	}
	for _, name := range library {
		if supplied[name] {
			continue
		}
		supplied[name] = true
		f, ok := libraryFit(fits, name)
		if !ok {
			f = SkillFit{Name: name, Folder: inputs.SourceLibrary, Problem: ProblemNotScanned}
		}
		out = append(out, f)
	}
	return out
}

func libraryFit(fits []SkillFit, name string) (SkillFit, bool) {
	for _, f := range fits {
		if f.Folder == inputs.SourceLibrary && f.Name == name {
			return f, true
		}
	}
	return SkillFit{}, false
}
