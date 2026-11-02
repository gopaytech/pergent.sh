package readiness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent/scope"
)

const (
	// binarySniff is how much of a file is searched for a NUL byte, as git
	// does before it calls a file binary (and a review then skips it).
	binarySniff = 8000
	// headerRead caps what is read to find the generated-file comment in the
	// first scope.GeneratedHeaderLines lines.
	headerRead = 64 << 10
	// codeOwnersRead caps what is read of CODEOWNERS; GitHub ignores a larger
	// file.
	codeOwnersRead = 3 << 20
)

// Noise names (scope.Match).
var (
	generatedNames = []string{"*.pb.go", "*_generated.*", "*.min.js"}
	buildOutput    = []string{"**/dist/**", "**/build/**"}
)

var readmeNames = []string{"README", "README.md", "README.rst", "README.txt", "README.adoc"}

// ScanTree scans the checkout at root the way a review would read it. It never
// follows a symlink (one is neither read nor counted) and skips .git. Per file,
// a NUL byte in the first 8000 bytes is the "binary" rule, which no skill
// overrides; else the path rule (scope.Attributes.PathRule, from the root
// .gitattributes); else "generated" when the generated-file comment is in its
// first scope.GeneratedHeaderLines lines. A file is included when an effective
// skill (Effective, with the library skills Assigned) reviews it, or, with no
// effective skill, when it has no rule. ScanTree fills everything but Version,
// Commit and Error; an error means the files could not be scanned.
func ScanTree(ctx context.Context, root string, in JobInput) (JobResult, error) {
	var r JobResult
	dir, err := os.OpenRoot(root)
	if err != nil {
		return JobResult{}, err
	}
	defer dir.Close()
	src := inputs.DirSource{Root: root}

	gitattributes, _, err := src.GetFile(ctx, 0, ".gitattributes", "")
	if err != nil {
		return JobResult{}, fmt.Errorf("reading .gitattributes: %w", err)
	}
	attrs := scope.ParseAttributes(gitattributes)

	scopes, err := skillFits(ctx, src, in, &r)
	if err != nil {
		return JobResult{}, err
	}
	reviewers := effectiveIndexes(r.Skills, in)

	w := walker{
		dir: dir, attrs: attrs, r: &r, scopes: scopes, reviewers: reviewers,
		buf: make([]byte, headerRead), top: map[string]fs.FileInfo{}, decided: make([]bool, len(scopes)),
		langFiles: map[string]int{}, langCovered: map[string]bool{},
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Name() == ".git" && p != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() { // directories go on; symlinks and devices are skipped
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return w.file(filepath.ToSlash(rel), info)
	})
	if err != nil {
		return JobResult{}, err
	}

	sortNoise(r.Noise)
	if len(r.Noise) > MaxNoisePaths {
		r.Noise = r.Noise[:MaxNoisePaths]
	}
	for name, n := range w.langFiles {
		if n*10 > r.SourceFiles {
			r.Languages = append(r.Languages, Language{Name: name, Files: n, Uncovered: !w.langCovered[name]})
		}
	}
	sort.Slice(r.Languages, func(i, j int) bool {
		a, b := r.Languages[i], r.Languages[j]
		return a.Files > b.Files || (a.Files == b.Files && a.Name < b.Name)
	})

	if err := w.context(); err != nil {
		return JobResult{}, err
	}
	if err := w.instructionsBytes(ctx, src); err != nil {
		return JobResult{}, err
	}
	return r, nil
}

// skillScope is the globs of r.Skills[i]; ok is false for a skill with a
// Problem, which reviews nothing.
type skillScope struct {
	paths, exclude []string
	ok             bool
}

// skillFits adds a fit for every library skill and every skill of the
// repository folders to r.Skills, and returns their globs in the same order.
func skillFits(ctx context.Context, src inputs.DirSource, in JobInput, r *JobResult) ([]skillScope, error) {
	var scopes []skillScope
	for _, s := range in.Library {
		r.Skills = append(r.Skills, SkillFit{Name: s.Name, Folder: inputs.SourceLibrary, HasPaths: len(s.Paths) > 0})
		scopes = append(scopes, skillScope{paths: s.Paths, exclude: s.Exclude, ok: true})
	}
	for _, folder := range inputs.RepoFolders {
		found, err := inputs.ScanFolder(ctx, src, 0, folder, "")
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			fit := SkillFit{Name: f.Name, Folder: folder, Problem: f.Problem, Bytes: len(f.Content)}
			var sc skillScope
			if fit.Problem == "" {
				paths, exclude, err := inputs.SkillScope(f.Content)
				if err != nil {
					fit.Problem = err.Error()
				} else {
					fit.HasPaths = len(paths) > 0
					sc = skillScope{paths: paths, exclude: exclude, ok: true}
				}
			}
			r.Skills = append(r.Skills, fit)
			scopes = append(scopes, sc)
		}
	}
	return scopes, nil
}

// effectiveIndexes is the indexes into fits of the skills a review would run
// with the input's assignment, in Effective's order, leaving out any with a
// Problem.
func effectiveIndexes(fits []SkillFit, in JobInput) []int {
	var assigned []string
	for _, s := range in.Library {
		if s.Assigned {
			assigned = append(assigned, s.Name)
		}
	}
	var out []int
	taken := map[int]bool{}
	for _, e := range Effective(fits, assigned, in.RepoSkills) {
		if e.Problem != "" {
			continue
		}
		for i, f := range fits {
			if !taken[i] && f.Problem == "" && f.Folder == e.Folder && f.Name == e.Name {
				out = append(out, i)
				taken[i] = true
				break
			}
		}
	}
	return out
}

type walker struct {
	dir       *os.Root
	attrs     scope.Attributes
	r         *JobResult
	scopes    []skillScope
	reviewers []int // indexes into r.Skills of the effective skills
	buf       []byte
	// top holds the files at the root and one directory down, for Context.
	top         map[string]fs.FileInfo
	langFiles   map[string]int
	langCovered map[string]bool
	decided     []bool   // per file: r.Skills[i] reviews it
	reviewedBy  []string // per file: the effective skills that review it
}

func (w *walker) file(rel string, info fs.FileInfo) error {
	w.r.Files++
	if strings.Count(rel, "/") <= 1 {
		w.top[rel] = info
	}

	rule, optIn, err := w.rule(rel, info)
	if err != nil {
		return err
	}
	if rule == "binary" {
		return nil
	}

	for i, sc := range w.scopes {
		w.decided[i] = sc.ok && scope.Decide(sc.paths, sc.exclude, rel, rule, optIn) == ""
		if w.decided[i] {
			w.r.Skills[i].Files++
		}
	}
	w.reviewedBy = w.reviewedBy[:0]
	for _, i := range w.reviewers {
		if w.decided[i] {
			w.reviewedBy = append(w.reviewedBy, w.r.Skills[i].Name)
		}
	}

	if lang := LanguageOf(rel); lang != "" && rule == "" {
		w.r.SourceFiles++
		w.langFiles[lang]++
		if len(w.reviewedBy) > 0 {
			w.langCovered[lang] = true
		}
	}

	included := rule == ""
	if len(w.reviewers) > 0 {
		included = len(w.reviewedBy) > 0
	}
	if !included {
		return nil
	}
	why := noiseWhy(rel, info.Size())
	if why == "" {
		return nil
	}
	w.r.NoiseTotal++
	if len(rel) > MaxPathBytes {
		return nil
	}
	n := NoiseFile{Path: rel, Why: why, Size: info.Size()}
	if len(w.reviewedBy) > 0 {
		n.Skills = append([]string(nil), w.reviewedBy...)
	}
	w.r.Noise = append(w.r.Noise, n)
	if len(w.r.Noise) > 4*MaxNoisePaths { // keep memory flat on a huge tree
		sortNoise(w.r.Noise)
		w.r.Noise = w.r.Noise[:MaxNoisePaths]
	}
	return nil
}

// rule is the default exclusion of one file and whether a skill's paths may
// override it, in the order of a review's diff classifier.
func (w *walker) rule(rel string, info fs.FileInfo) (string, bool, error) {
	f, err := w.open(rel, info)
	if err != nil || f == nil {
		return "binary", false, err // a file that changed under the scan is not read
	}
	defer f.Close()

	n, err := io.ReadFull(f, w.buf[:binarySniff])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false, err
	}
	if bytes.IndexByte(w.buf[:n], 0) >= 0 {
		return "binary", false, nil
	}
	if rule := w.attrs.PathRule(rel); rule != "" {
		return rule, true, nil
	}
	if n == binarySniff && bytes.Count(w.buf[:n], []byte("\n")) < scope.GeneratedHeaderLines {
		m, err := io.ReadFull(f, w.buf[n:])
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return "", false, err
		}
		n += m
	}
	if scope.HasGeneratedHeader(string(w.buf[:n])) {
		return scope.RuleGenerated, true, nil
	}
	return "", false, nil
}

// open opens the regular file the walk saw at rel. It is nil when the file
// opened is not that one (it was replaced, say by a symlink).
func (w *walker) open(rel string, info fs.FileInfo) (*os.File, error) {
	f, err := w.dir.Open(filepath.FromSlash(rel))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !os.SameFile(st, info) {
		f.Close()
		return nil, err
	}
	return f, nil
}

func noiseWhy(rel string, size int64) string {
	switch {
	case scope.MatchAny(generatedNames, rel):
		return WhyGeneratedName
	case scope.MatchAny(buildOutput, rel):
		return WhyBuildOutput
	case size > NoiseSize:
		return WhyLarge
	}
	return ""
}

func sortNoise(ns []NoiseFile) {
	sort.Slice(ns, func(i, j int) bool {
		return ns[i].Size > ns[j].Size || (ns[i].Size == ns[j].Size && ns[i].Path < ns[j].Path)
	})
}

// context fills r.Context from the files at the top of the tree.
func (w *walker) context() error {
	c := &w.r.Context
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if info, ok := w.top[name]; ok {
			c.AgentFile = name
			empty, err := w.blank(name, info)
			if err != nil {
				return err
			}
			c.AgentFileEmpty = empty
			break
		}
	}

	names := make([]string, 0, len(w.top))
	for rel := range w.top {
		names = append(names, rel)
	}
	sort.Strings(names)
	inDir := func(dir, base string) string {
		for _, rel := range names {
			if path.Dir(rel) == dir && strings.EqualFold(path.Base(rel), base) {
				return rel
			}
		}
		return ""
	}
	for _, base := range readmeNames {
		if c.Readme = inDir(".", base); c.Readme != "" {
			break
		}
	}
	for _, dir := range []string{".", ".github", "docs"} {
		if c.Contributing = inDir(dir, "CONTRIBUTING.md"); c.Contributing != "" {
			break
		}
	}

	for _, p := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS", ".gitlab/CODEOWNERS"} {
		info, ok := w.top[p]
		if !ok {
			continue
		}
		c.CodeOwners = p
		rules, err := w.codeOwnerRules(p, info)
		if err != nil {
			return err
		}
		c.CodeOwnerRules = rules
		break
	}

	_, c.Instructions = w.top[".pergent/instructions.md"]
	return nil
}

// blank reports whether a file holds only whitespace. It stops at the first
// other byte, so a huge file costs one read.
func (w *walker) blank(rel string, info fs.FileInfo) (bool, error) {
	f, err := w.open(rel, info)
	if err != nil || f == nil {
		return false, err
	}
	defer f.Close()
	for {
		n, err := f.Read(w.buf)
		if len(bytes.TrimSpace(w.buf[:n])) > 0 {
			return false, nil
		}
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// codeOwnerRules counts the lines of a CODEOWNERS file that are rules: not
// blank, not a comment, not a GitLab section header ("[Section]" or
// "^[Optional section]").
func (w *walker) codeOwnerRules(rel string, info fs.FileInfo) (int, error) {
	f, err := w.open(rel, info)
	if err != nil || f == nil {
		return 0, err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, codeOwnersRead))
	if err != nil {
		return 0, err
	}
	rules := 0
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "^[") {
			continue
		}
		rules++
	}
	return rules, nil
}

// instructionsBytes sets r.InstructionsBytes to the size of
// inputs.LoadInstructions over the checkout. A file too large for it to read
// still counts, by its size: the review would get it all and go over budget.
func (w *walker) instructionsBytes(ctx context.Context, src inputs.DirSource) error {
	b, err := inputs.LoadInstructions(ctx, src, 0, "")
	if err == nil {
		w.r.InstructionsBytes = len(b)
		return nil
	}
	if !errors.Is(err, inputs.ErrFileTooLarge) {
		return err
	}
	// The headings are LoadInstructions'; TestInstructionsBytes pins the two
	// ways of counting to agree.
	n := 0
	if c := w.r.Context; c.AgentFile != "" {
		n += len("# Project instructions ("+c.AgentFile+")\n\n") + int(w.top[c.AgentFile].Size()) + 1
	}
	if info, ok := w.top[".pergent/instructions.md"]; ok {
		n += len("\n# Team review instructions (.pergent/instructions.md)\n\n") + int(info.Size()) + 1
	}
	w.r.InstructionsBytes = n
	return nil
}
