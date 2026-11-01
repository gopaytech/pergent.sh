package inputs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanFolder(t *testing.T) {
	src := &fakeSource{files: map[string]string{
		".agents/skills/flat.md":          "# flat",
		".agents/skills/folder/SKILL.md":  "---\nname: renamed\n---\n# folder",
		".agents/skills/folder/extra.txt": "ignored",
		".agents/skills/nofile/other.md":  "# no SKILL.md",
		".agents/skills/bad.md":           "---\nname: [unclosed\n---\nx",
		".agents/skills/.hidden.md":       "# hidden",
		".agents/skills/notes.txt":        "ignored",
	}}
	got, err := ScanFolder(context.Background(), src, 7, ".agents/skills", "sha1")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Found{}
	for _, f := range got {
		by[f.Name] = f
	}
	if len(got) != 4 {
		t.Fatalf("found %d entries: %+v", len(got), got)
	}
	if f := by["flat"]; f.Problem != "" || string(f.Content) != "# flat" || f.Path != ".agents/skills/flat.md" {
		t.Errorf("flat = %+v", f)
	}
	if f := by["renamed"]; f.Problem != "" || f.Path != ".agents/skills/folder/SKILL.md" {
		t.Errorf("folder = %+v", f)
	}
	if f := by["nofile"]; !strings.Contains(f.Problem, "SKILL.md") {
		t.Errorf("nofile = %+v", f)
	}
	if f := by["bad"]; f.Problem == "" {
		t.Errorf("bad = %+v", f)
	}
}

func TestScanFolderMissingDirAndErrors(t *testing.T) {
	got, err := ScanFolder(context.Background(), &fakeSource{}, 7, ".agents/skills", "sha1")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
	_, err = ScanFolder(context.Background(), &fakeSource{listErr: errors.New("boom")}, 7, ".agents/skills", "sha1")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestScanFolderDuplicateName(t *testing.T) {
	src := &fakeSource{files: map[string]string{
		".agents/skills/a.md":       "# a",
		".agents/skills/b/SKILL.md": "---\nname: a\n---\n# b",
	}}
	got, err := ScanFolder(context.Background(), src, 7, ".agents/skills", "sha1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Problem != "" || !strings.Contains(got[1].Problem, ".agents/skills/a.md") {
		t.Fatalf("got %+v", got)
	}
}

func TestDirSourceStaysInRoot(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "dir", "SKILL.md"), []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	mk := func(p, c string) {
		t.Helper()
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(".claude/skills/ok.md", "# ok")
	mk("real/in.md", "# in")
	// a symlink to a file outside, one to a directory outside, and one that
	// stays inside the root: none is followed.
	links := map[string]string{
		".claude/skills/leak.md":  secret,
		".claude/skills/leakdir":  filepath.Join(outside, "dir"),
		".claude/skills/inside":   filepath.Join(root, "real"),
		".claude/skills/inner.md": filepath.Join(root, "real", "in.md"),
	}
	for l, target := range links {
		if err := os.Symlink(target, filepath.Join(root, l)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "up")); err != nil {
		t.Fatal(err)
	}

	src := DirSource{Root: root}
	ctx := context.Background()

	if c, ok, err := src.GetFile(ctx, 0, ".claude/skills/ok.md", ""); err != nil || !ok || string(c) != "# ok" {
		t.Fatalf("ok.md = %q %v %v", c, ok, err)
	}
	for _, p := range []string{
		".claude/skills/leak.md", "../" + filepath.Base(outside) + "/secret.md", "../../etc/passwd",
		".claude/skills/../../" + filepath.Base(outside) + "/secret.md", "up/secret.md", "up/dir/SKILL.md",
		".claude/skills/leakdir/SKILL.md", ".claude/skills/inside/in.md", ".claude/skills/inner.md",
		filepath.Join(outside, "secret.md"), "/etc/passwd", ".claude/skills",
	} {
		c, ok, err := src.GetFile(ctx, 0, p, "")
		if err != nil || ok || c != nil {
			t.Errorf("GetFile(%q) = %q, %v, %v; want not found", p, c, ok, err)
		}
	}

	for _, d := range []string{"up", "../" + filepath.Base(outside), ".claude/skills/leakdir", ".claude/skills/inside", "nowhere", ".claude/skills/ok.md"} {
		es, err := src.ListTree(ctx, 0, d, "")
		if err != nil || len(es) != 0 {
			t.Errorf("ListTree(%q) = %v, %v; want empty", d, es, err)
		}
	}

	es, err := src.ListTree(ctx, 0, ".claude/skills", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].Name != "ok.md" || es[0].Type != "blob" || es[0].Path != ".claude/skills/ok.md" {
		t.Fatalf("entries = %+v, want only ok.md", es)
	}
	top, err := src.ListTree(ctx, 0, "real", "")
	if err != nil || len(top) != 1 || top[0].Type != "blob" {
		t.Fatalf("real = %+v, %v", top, err)
	}
	rootEntries, err := src.ListTree(ctx, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rootEntries {
		if e.Name == "up" {
			t.Errorf("root listing follows symlink: %+v", e)
		}
	}
}

func TestDirSourceMissingRoot(t *testing.T) {
	src := DirSource{Root: filepath.Join(t.TempDir(), "gone")}
	if c, ok, err := src.GetFile(context.Background(), 0, "a.md", ""); err != nil || ok || c != nil {
		t.Errorf("GetFile = %q %v %v", c, ok, err)
	}
	if es, err := src.ListTree(context.Background(), 0, "", ""); err != nil || len(es) != 0 {
		t.Errorf("ListTree = %v %v", es, err)
	}
}

func TestDetect(t *testing.T) {
	src := &fakeSource{files: map[string]string{
		".pergent/skills/mine.md":   "# not detected: always runs",
		".agents/skills/a.md":       "# a",
		".claude/skills/b/SKILL.md": "# b",
		".claude/skills/broken.md":  "---\nname: [x\n---\n",
		".github/skills/c.md":       "# c",
		".github/skills/c-folder/x": "ignored",
	}}
	got, err := Detect(context.Background(), src, 7, "sha1")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, d := range got {
		keys = append(keys, d.Folder+"|"+d.Name+"|"+d.Path+"|"+boolStr(d.Problem != ""))
	}
	want := []string{
		".agents/skills|a|.agents/skills/a.md|ok",
		".claude/skills|b|.claude/skills/b/SKILL.md|ok",
		".claude/skills|broken|.claude/skills/broken.md|problem",
		".github/skills|c-folder|.github/skills/c-folder/SKILL.md|problem",
		".github/skills|c|.github/skills/c.md|ok",
	}
	if strings.Join(keys, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(keys, "\n"), strings.Join(want, "\n"))
	}
}

func boolStr(b bool) string {
	if b {
		return "problem"
	}
	return "ok"
}
