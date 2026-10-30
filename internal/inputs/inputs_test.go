package inputs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
)

// fakeSource serves files from a map of path → content; every ref is accepted
// except that the last ref seen is recorded.
type fakeSource struct {
	files   map[string]string
	refs    map[string]bool
	listErr error
}

func (f *fakeSource) GetFile(_ context.Context, _ int64, p, ref string) ([]byte, bool, error) {
	if f.refs == nil {
		f.refs = map[string]bool{}
	}
	f.refs[ref] = true
	c, ok := f.files[p]
	if !ok {
		return nil, false, nil
	}
	return []byte(c), true, nil
}

func (f *fakeSource) ListTree(_ context.Context, _ int64, dir, ref string) ([]gitlab.TreeEntry, error) {
	if f.refs == nil {
		f.refs = map[string]bool{}
	}
	f.refs[ref] = true
	if f.listErr != nil {
		return nil, f.listErr
	}
	seen := map[string]gitlab.TreeEntry{}
	for p := range f.files {
		if !strings.HasPrefix(p, dir+"/") {
			continue
		}
		rest := strings.TrimPrefix(p, dir+"/")
		first, _, nested := strings.Cut(rest, "/")
		e := gitlab.TreeEntry{Name: first, Path: dir + "/" + first, Type: "blob"}
		if nested {
			e.Type = "tree"
		}
		seen[first] = e
	}
	var out []gitlab.TreeEntry
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func lib() Library {
	return Library{
		"code-review": []byte("# code review"),
		"security":    []byte("# security"),
	}
}

func names(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLoadLibrary(t *testing.T) {
	l, err := LoadLibrary("testdata/library")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(l); !equal(got, []string{"code-review", "security-review"}) {
		t.Fatalf("names = %v", got)
	}
	if !strings.Contains(string(l["security-review"]), "Look for injection") {
		t.Errorf("content = %q", l["security-review"])
	}
}

func TestLoadLibraryRules(t *testing.T) {
	write := func(dir, name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("folder without SKILL.md", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "foo/readme.txt", "x")
		_, err := LoadLibrary(dir)
		if err == nil || !strings.Contains(err.Error(), "foo") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dot entries and other files skipped", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, ".git/config", "x")
		write(dir, ".hidden.md", "x")
		write(dir, "notes.txt", "x")
		write(dir, "a.md", "# a")
		l, err := LoadLibrary(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(l); !equal(got, []string{"a"}) {
			t.Fatalf("names = %v", got)
		}
	})
	t.Run("symlinks followed", func(t *testing.T) {
		dir := t.TempDir()
		real := t.TempDir()
		write(real, "b.md", "# b")
		write(real, "dirskill/SKILL.md", "# c")
		if err := os.Symlink(filepath.Join(real, "b.md"), filepath.Join(dir, "b.md")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(real, "dirskill"), filepath.Join(dir, "dirskill")); err != nil {
			t.Fatal(err)
		}
		l, err := LoadLibrary(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(l); !equal(got, []string{"b", "dirskill"}) {
			t.Fatalf("names = %v", got)
		}
	})
	t.Run("duplicate names", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "a.md", "# a")
		write(dir, "b/SKILL.md", "---\nname: a\n---\n# b")
		_, err := LoadLibrary(dir)
		if err == nil || !strings.Contains(err.Error(), `"a"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad name", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "a.md", "---\nname: ../evil\n---\n# a")
		if _, err := LoadLibrary(dir); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestSkillNameRejectsSlash(t *testing.T) {
	for _, n := range []string{"a/b", "../x", "", ".hidden", "-x", "a b"} {
		content := []byte("---\nname: \"" + n + "\"\n---\nbody")
		if n == "" {
			content = []byte("body")
		}
		if _, err := SkillName(content, n); err == nil {
			t.Errorf("SkillName(%q) accepted", n)
		}
	}
	if got, err := SkillName([]byte("---\nname: ok.name_1-x\n---\nbody"), "fallback"); err != nil || got != "ok.name_1-x" {
		t.Errorf("got %q, %v", got, err)
	}
	if got, err := SkillName([]byte("no frontmatter"), "fallback"); err != nil || got != "fallback" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := SkillName([]byte("---\nname: [unclosed\n---\nbody"), "fallback"); err == nil {
		t.Error("bad yaml accepted")
	}
}

func TestResolveAllLibraryWhenListEmpty(t *testing.T) {
	in, err := Resolve(context.Background(), &fakeSource{}, lib(), config.Repository{}, 7, "abc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(in.Skills); !equal(got, []string{"code-review", "security"}) {
		t.Fatalf("skills = %v", got)
	}
	if len(in.Overridden) != 0 {
		t.Errorf("overridden = %v", in.Overridden)
	}
}

func TestResolveSelectedLibrary(t *testing.T) {
	in, err := Resolve(context.Background(), &fakeSource{}, lib(), config.Repository{Skills: []string{"security"}}, 7, "abc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(in.Skills); !equal(got, []string{"security"}) {
		t.Fatalf("skills = %v", got)
	}
}

func TestUnknownLibrarySkill(t *testing.T) {
	_, err := Resolve(context.Background(), &fakeSource{}, lib(), config.Repository{Skills: []string{"nope"}}, 7, "abc", nil)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestRepoSkillOverridesLibrary(t *testing.T) {
	src := &fakeSource{files: map[string]string{
		".pergent/skills/code-review.md": "# repo review",
		".pergent/skills/extra/SKILL.md": "---\nname: extra-skill\n---\n# extra",
		".pergent/skills/.hidden.md":     "# hidden",
		".pergent/skills/notes.txt":      "ignored",
	}}
	in, err := Resolve(context.Background(), src, lib(), config.Repository{}, 7, "sha1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(in.Overridden, []string{"code-review"}) {
		t.Errorf("overridden = %v", in.Overridden)
	}
	if string(in.Skills["code-review"]) != "# repo review" {
		t.Errorf("content = %q", in.Skills["code-review"])
	}
	if got := names(in.Skills); !equal(got, []string{"code-review", "extra-skill", "security"}) {
		t.Errorf("skills = %v", got)
	}
	if len(src.refs) != 1 || !src.refs["sha1"] {
		t.Errorf("refs = %v", src.refs)
	}
}

func TestRepoSkillAddedWhenLibrarySelectionIsNarrower(t *testing.T) {
	src := &fakeSource{files: map[string]string{".pergent/skills/security.md": "# repo security"}}
	in, err := Resolve(context.Background(), src, lib(), config.Repository{Skills: []string{"code-review"}}, 7, "sha1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// security was not selected, so the repo's one is an addition, not an override.
	if len(in.Overridden) != 0 {
		t.Errorf("overridden = %v", in.Overridden)
	}
	if got := names(in.Skills); !equal(got, []string{"code-review", "security"}) {
		t.Errorf("skills = %v", got)
	}
}

func TestRepoSkillFolderWithoutSkillMD(t *testing.T) {
	src := &fakeSource{files: map[string]string{".pergent/skills/foo/other.md": "# x"}}
	_, err := Resolve(context.Background(), src, lib(), config.Repository{}, 7, "sha1", nil)
	if err == nil || !strings.Contains(err.Error(), ".pergent/skills/foo") {
		t.Fatalf("err = %v", err)
	}
}

func TestRepoSkillBadName(t *testing.T) {
	src := &fakeSource{files: map[string]string{".pergent/skills/x.md": "---\nname: a/b\n---\n# x"}}
	_, err := Resolve(context.Background(), src, lib(), config.Repository{}, 7, "sha1", nil)
	if err == nil || !strings.Contains(err.Error(), ".pergent/skills/x.md") {
		t.Fatalf("err = %v", err)
	}
}

func TestRepoSkillDuplicate(t *testing.T) {
	src := &fakeSource{files: map[string]string{
		".pergent/skills/a.md":       "# a",
		".pergent/skills/b/SKILL.md": "---\nname: a\n---\n# b",
	}}
	_, err := Resolve(context.Background(), src, lib(), config.Repository{}, 7, "sha1", nil)
	if err == nil || !strings.Contains(err.Error(), ".pergent/skills/a.md") || !strings.Contains(err.Error(), ".pergent/skills/b") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstructionsCombined(t *testing.T) {
	src := &fakeSource{files: map[string]string{
		"AGENTS.md":                "agents text",
		"CLAUDE.md":                "claude text",
		".pergent/instructions.md": "team text",
	}}
	in, err := Resolve(context.Background(), src, lib(), config.Repository{}, 7, "sha1", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Project instructions (AGENTS.md)\n\nagents text\n\n# Team review instructions (.pergent/instructions.md)\n\nteam text\n"
	if string(in.Instructions) != want {
		t.Errorf("instructions = %q", in.Instructions)
	}
}

func TestInstructionsClaudeFallback(t *testing.T) {
	src := &fakeSource{files: map[string]string{"CLAUDE.md": "claude text"}}
	in, err := Resolve(context.Background(), src, lib(), config.Repository{}, 7, "sha1", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Project instructions (CLAUDE.md)\n\nclaude text\n"
	if string(in.Instructions) != want {
		t.Errorf("instructions = %q", in.Instructions)
	}
}

func TestTeamInstructionsOnly(t *testing.T) {
	src := &fakeSource{files: map[string]string{".pergent/instructions.md": "team text"}}
	in, err := Resolve(context.Background(), src, lib(), config.Repository{}, 7, "sha1", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "\n# Team review instructions (.pergent/instructions.md)\n\nteam text\n"
	if string(in.Instructions) != want {
		t.Errorf("instructions = %q", in.Instructions)
	}
}

func TestNoInstructionsIsNil(t *testing.T) {
	in, err := Resolve(context.Background(), &fakeSource{}, lib(), config.Repository{}, 7, "sha1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Instructions != nil {
		t.Errorf("instructions = %q", in.Instructions)
	}
	if in.Previous != nil {
		t.Errorf("previous = %q", in.Previous)
	}
}

func TestResolveStoresPrevious(t *testing.T) {
	in, err := Resolve(context.Background(), &fakeSource{}, lib(), config.Repository{}, 7, "sha1", []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(in.Previous) != `{"a":1}` {
		t.Errorf("previous = %q", in.Previous)
	}
}

func TestResolveSourceError(t *testing.T) {
	src := &fakeSource{listErr: errors.New("boom")}
	if _, err := Resolve(context.Background(), src, lib(), config.Repository{}, 7, "sha1", nil); err == nil {
		t.Fatal("want error")
	}
}

func TestSize(t *testing.T) {
	in := &Inputs{
		Skills:       map[string][]byte{"a": []byte("123"), "b": []byte("45")},
		Instructions: []byte("678"),
		Previous:     []byte("90"),
	}
	if in.Size() != 10 {
		t.Errorf("size = %d", in.Size())
	}
}

func TestFitDropsPreviousFirst(t *testing.T) {
	in := &Inputs{Skills: map[string][]byte{"a": make([]byte, 60)}, Previous: make([]byte, 60)}
	if err := in.Fit(100); err != nil {
		t.Fatal(err)
	}
	if in.Previous != nil {
		t.Error("previous kept")
	}
	if len(in.Skills["a"]) != 60 {
		t.Error("skills changed")
	}

	in = &Inputs{Skills: map[string][]byte{"a": make([]byte, 40)}, Previous: make([]byte, 60)}
	if err := in.Fit(100); err != nil || in.Previous == nil {
		t.Errorf("fit within limit dropped previous: %v", err)
	}
}

func TestFitTooLarge(t *testing.T) {
	in := &Inputs{Skills: map[string][]byte{"a": make([]byte, 150)}, Previous: make([]byte, 10)}
	err := in.Fit(100)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

// Sources says where each resolved skill came from: a repository skill
// replacing a library one of the same name is the repository's.
func TestResolveSources(t *testing.T) {
	src := &fakeSource{files: map[string]string{
		".pergent/skills/code-review.md": "# repo review",
		".pergent/skills/security.md":    "# repo security",
		".pergent/skills/extra.md":       "# extra",
	}}
	in, err := Resolve(context.Background(), src, lib(), config.Repository{Skills: []string{"code-review"}}, 7, "sha1", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"code-review": SourceRepo, "security": SourceRepo, "extra": SourceRepo}
	if fmt.Sprint(in.Sources) != fmt.Sprint(want) {
		t.Errorf("sources = %v, want %v", in.Sources, want)
	}
	in, err = Resolve(context.Background(), &fakeSource{files: map[string]string{}}, lib(), config.Repository{}, 7, "sha1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(in.Sources) != fmt.Sprint(map[string]string{"code-review": SourceLibrary, "security": SourceLibrary}) {
		t.Errorf("library sources = %v", in.Sources)
	}
}
