package reviewapi

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func str(s string) *string { return &s }

func TestCheckFiles(t *testing.T) {
	long := strings.Repeat("a", maxPathBytes+1)
	seg255 := strings.Repeat("a", maxSegmentBytes)
	const noStore = "files[0]: the path holds a character git can't store"
	cases := []struct {
		name  string
		files []File
		// want is "" for a good list, else the whole error text.
		want string
	}{
		{"new only", []File{{Path: "a/b.yaml", New: str("x")}}, ""},
		{"old only", []File{{Path: "x", Old: str("x")}}, ""},
		{"both", []File{{Path: "x", Old: str("a"), New: str("b")}}, ""},
		{"empty content", []File{{Path: "x", New: str("")}}, ""},
		{"spaces and accents", []File{{Path: "my dir/café menu.txt", New: str("x")}}, ""},
		{"a dot in a name", []File{{Path: ".github/.gitignore", New: str("x")}, {Path: "a..b/c.d", New: str("x")}}, ""},
		{"longest path", []File{{Path: strings.Join([]string{seg255, seg255, seg255, seg255}, "/"), New: str("x")}}, ""},
		{"longest name", []File{{Path: seg255, New: str("x")}}, ""},
		{".gitmodules", []File{{Path: ".gitmodules", New: str("x")}}, ""},
		{"look-alikes of .git", []File{
			{Path: "a ", New: str("x")}, {Path: "b.", New: str("x")}, {Path: "x:y", New: str("x")},
			{Path: "aux", New: str("x")}, {Path: "git~2/x", New: str("x")}, {Path: " .git", New: str("x")},
			{Path: ".gi", New: str("x")}, {Path: "git~", New: str("x")}, {Path: "git~10", New: str("x")},
		}, ""},
		{"no files", nil, "files must not be empty"},
		{"empty list", []File{}, "files must not be empty"},
		{"empty path", []File{{Path: "", New: str("x")}}, "files[0]: the path must be relative"},
		{"absolute", []File{{Path: "/etc/passwd", New: str("x")}}, "files[0]: the path must be relative"},
		{"dotdot", []File{{Path: "a/../b", New: str("x")}}, "files[0]: the path must not hold .. or ."},
		{"leading dotdot", []File{{Path: "../b", New: str("x")}}, "files[0]: the path must not hold .. or ."},
		{"dot segment", []File{{Path: "a/./b", New: str("x")}}, "files[0]: the path must not hold .. or ."},
		{"only a dot", []File{{Path: ".", New: str("x")}}, "files[0]: the path must not hold .. or ."},
		{"twice", []File{{Path: "a", New: str("x")}, {Path: "b", New: str("x")}, {Path: "a", Old: str("x")}}, "files[2]: the path appears twice"},
		{"too long", []File{{Path: long, New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{"not UTF-8 path", []File{{Path: "a\xffb", New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{"newline in path", []File{{Path: "a\nb", New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{"NUL in path", []File{{Path: "a\x00b", New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{"DEL in path", []File{{Path: "a\x7fb", New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{"tab in path", []File{{Path: "a\tb", New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{"backslash", []File{{Path: `a\b`, New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{"trailing slash", []File{{Path: "a/", New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{"empty segment", []File{{Path: "a//b", New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{".git file", []File{{Path: ".git", New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{".git dir", []File{{Path: "a/.git/config", New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{".GIT any case", []File{{Path: ".GiT/config", New: str("x")}}, "files[0]: the path holds a character git can't store"},
		{"name over 255 bytes", []File{{Path: seg255 + "a", New: str("x")}}, noStore},
		{"name over 255 bytes in a directory", []File{{Path: "d/" + seg255 + "a/f", New: str("x")}}, noStore},
		{".git trailing space", []File{{Path: ".git ", New: str("x")}}, noStore},
		{".git trailing dot", []File{{Path: ".git.", New: str("x")}}, noStore},
		{".git dot in a directory", []File{{Path: ".git./x", New: str("x")}}, noStore},
		{".git dots and spaces", []File{{Path: "a/.git . ./x", New: str("x")}}, noStore},
		{"git~1", []File{{Path: "git~1/x", New: str("x")}}, noStore},
		{"GIT~1", []File{{Path: "GIT~1", New: str("x")}}, noStore},
		{"git~1 trailing dot", []File{{Path: "a/Git~1./x", New: str("x")}}, noStore},
		{".git with a zero-width non-joiner", []File{{Path: ".g\u200cit/x", New: str("x")}}, noStore},
		{".git ending in a zero-width non-joiner", []File{{Path: ".gi\u200ct", New: str("x")}}, noStore},
		{".git with other ignorable points", []File{{Path: ".g\u202ei\u206ft\ufeff", New: str("x")}}, noStore},
		{".git NTFS stream", []File{{Path: ".git::$INDEX_ALLOCATION", New: str("x")}}, noStore},
		{".git stream in a directory", []File{{Path: "a/.GIT:x/y", New: str("x")}}, noStore},
		{"old not UTF-8", []File{{Path: "a", Old: str("\xff")}}, "files[0]: old is not UTF-8 text"},
		{"new not UTF-8", []File{{Path: "ok", New: str("x")}, {Path: "a", New: str("\xc3")}}, "files[1]: new is not UTF-8 text"},
		{"neither", []File{{Path: "a"}}, "files[0]: a file needs old, new or both"},
		{"dir in the new side", []File{{Path: "a", New: str("x")}, {Path: "a/b", New: str("x")}}, "files[0]: the path is also a directory in the same side"},
		{"dir in the old side", []File{{Path: "a/b", Old: str("x")}, {Path: "a", Old: str("x")}}, "files[1]: the path is also a directory in the same side"},
		{"deep dir", []File{{Path: "a/b/c", New: str("x")}, {Path: "a/b", New: str("x")}}, "files[1]: the path is also a directory in the same side"},
		// A file turning into a directory is a normal change: the sides differ.
		{"file becomes dir", []File{{Path: "a", Old: str("x")}, {Path: "a/b", New: str("x")}}, ""},
		{"siblings that share a prefix", []File{{Path: "a", New: str("x")}, {Path: "a.b", New: str("x")}, {Path: "ab/c", New: str("x")}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckFiles(c.files)
			if c.want == "" {
				if err != nil {
					t.Fatalf("CheckFiles = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != c.want {
				t.Fatalf("CheckFiles = %v, want %q", err, c.want)
			}
		})
	}
}

func TestCheckFilesErrorKinds(t *testing.T) {
	var fe *FileError
	if err := CheckFiles(nil); errors.As(err, &fe) {
		t.Fatalf("an empty list is a plain error, got %#v", fe)
	}
	err := CheckFiles([]File{{Path: "a"}, {Path: "b", New: str("x")}})
	if !errors.As(err, &fe) || fe.Index != 0 {
		t.Fatalf("want a *FileError at index 0, got %v", err)
	}
}

func TestBuildImportRejects(t *testing.T) {
	if _, err := BuildImport([]File{{Path: "../x", New: str("x")}}); err == nil {
		t.Fatal("BuildImport accepted a bad path")
	}
}

func TestBuildImportShas(t *testing.T) {
	im, err := BuildImport([]File{{Path: "a.txt", New: str("hi\n")}})
	if err != nil {
		t.Fatal(err)
	}
	// Written down once they agreed with `git fast-import` (TestImportMatchesGit).
	const wantBase = "417e1d43d5c40ec8abd9d8517ad83311264b30f6"
	const wantHead = "1feefb2d55467af154e5689ff1ea57f9b2d8dd0b"
	if im.Base != wantBase {
		t.Errorf("Base = %s, want %s", im.Base, wantBase)
	}
	if im.Head != wantHead {
		t.Errorf("Head = %s, want %s", im.Head, wantHead)
	}
	if im.Bytes != 3 {
		t.Errorf("Bytes = %d, want 3", im.Bytes)
	}
}

func TestBuildImportStream(t *testing.T) {
	im, err := BuildImport([]File{
		{Path: "b", Old: str("old"), New: str("new")},
		{Path: `q"x é`, New: str("n\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The path order is byte order: `b` (0x62) sorts before `q`.
	want := "blob\nmark :1\ndata 3\nold\n" +
		"blob\nmark :2\ndata 3\nnew\n" +
		"blob\nmark :3\ndata 2\nn\n\n" +
		"commit refs/heads/base\ncommitter pergent <pergent@pergent.sh> 0 +0000\ndata 5\nbase\n" +
		"M 100644 :1 \"b\"\n" +
		"commit refs/heads/head\ncommitter pergent <pergent@pergent.sh> 0 +0000\ndata 5\nhead\nfrom refs/heads/base\ndeleteall\n" +
		"M 100644 :2 \"b\"\n" +
		"M 100644 :3 \"q\\\"x é\"\n"
	if string(im.Stream) != want {
		t.Fatalf("stream:\n%q\nwant:\n%q", im.Stream, want)
	}
	if im.Bytes != 3+3+2 {
		t.Errorf("Bytes = %d, want 8", im.Bytes)
	}
}

// TestImportMatchesGit is the proof that the SHAs computed here are the ones
// the Job's `git fast-import` makes. If it fails, every API review fails.
func TestImportMatchesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	cases := []struct {
		name  string
		files []File
	}{
		{"one new file", []File{{Path: "a.txt", New: str("hi\n")}}},
		{"nested directories", []File{
			{Path: "a/b/c.yaml", New: str("c: 1\n")},
			{Path: "a/b/d.yaml", New: str("d: 1\n")},
			{Path: "a/e.txt", New: str("e\n")},
			{Path: "f/g/h/i.txt", New: str("i\n")},
			{Path: "z", New: str("z\n")},
		}},
		{"a, a.b and a/c order", []File{
			{Path: "a", Old: str("file a\n")},
			{Path: "a.b", Old: str("file a.b\n"), New: str("file a.b\n")},
			{Path: "a/c", New: str("file a/c\n")},
			{Path: "a0", New: str("file a0\n")},
		}},
		{"a and a.b and a/c in one side", []File{
			{Path: "a.b", New: str("1\n")},
			{Path: "a/c", New: str("2\n")},
			{Path: "a-b", New: str("3\n")},
			{Path: "a0", New: str("4\n")},
		}},
		{"deleted file", []File{{Path: "gone.txt", Old: str("bye\n")}, {Path: "stay.txt", Old: str("s\n"), New: str("t\n")}}},
		{"only deletions", []File{{Path: "gone.txt", Old: str("bye\n")}}},
		{"unchanged file", []File{{Path: "same.txt", Old: str("same\n"), New: str("same\n")}}},
		{"changed file", []File{{Path: "x/y.go", Old: str("package a\n"), New: str("package b\n")}}},
		{"empty content", []File{{Path: "empty", New: str("")}, {Path: "old-empty", Old: str("")}}},
		{"no trailing newline", []File{{Path: "n", New: str("no newline")}}},
		{"CRLF", []File{{Path: "win.txt", Old: str("a\r\nb\r\n"), New: str("a\r\nc\r\n")}}},
		{"attributes", []File{
			{Path: ".gitattributes", New: str("* text eol=crlf\n*.yaml filter=lfs diff=lfs merge=lfs -text\n")},
			{Path: "values.yaml", New: str("a: 1\r\nb: 2\n")},
			{Path: "main.go", New: str("package main\r\n")},
		}},
		{"quote and UTF-8 in path", []File{
			{Path: `say "hi".txt`, New: str("q\n")},
			{Path: "café/naïve ✓.md", New: str("é\n")},
			{Path: "日本語 dir/ファイル", Old: str("jp\n")},
			{Path: "a b/c d", New: str("sp\n")},
		}},
		{"text that looks like a stream", []File{
			{Path: "tricky", New: str("data 5\nhello\ncommit refs/heads/evil\n")},
		}},
		{"look-alikes of .git", []File{
			{Path: ".gitmodules", New: str("m\n")},
			{Path: "a ", New: str("1\n")},
			{Path: "a.", New: str("2\n")},
			{Path: "x:y", New: str("3\n")},
			{Path: "aux", New: str("4\n")},
			{Path: "git~2/x", New: str("5\n")},
			{Path: " .git", New: str("6\n")},
		}},
		{"longest name", []File{{Path: strings.Repeat("a", maxSegmentBytes), New: str("n\n")}}},
		{"empty base", []File{{Path: "added", New: str("a\n")}, {Path: "dir/also", New: str("b\n")}}},
		{"empty head", []File{{Path: "a", Old: str("a\n")}, {Path: "d/b", Old: str("b\n")}}},
		{"file becomes directory", []File{{Path: "a", Old: str("was a file\n")}, {Path: "a/b", New: str("now a dir\n")}}},
		{"long path", []File{{Path: strings.Repeat("d/", 200) + "f", New: str("deep\n")}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			im, err := BuildImport(c.files)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			git := func(stdin []byte, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(),
					"HOME="+dir,
					"XDG_CONFIG_HOME="+dir,
					"GIT_CONFIG_NOSYSTEM=1",
					"GIT_CONFIG_GLOBAL=/dev/null",
				)
				if stdin != nil {
					cmd.Stdin = bytes.NewReader(stdin)
				}
				var out, errb bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &errb
				if err := cmd.Run(); err != nil {
					t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errb.String())
				}
				return out.String()
			}
			git(nil, "init", "--quiet")
			git(im.Stream, "fast-import", "--quiet")
			got := strings.Fields(git(nil, "rev-parse", "refs/heads/base", "refs/heads/head"))
			if len(got) != 2 || got[0] != im.Base || got[1] != im.Head {
				t.Fatalf("git made %v, BuildImport computed base %s head %s", got, im.Base, im.Head)
			}
			git(nil, "fsck", "--strict", "--no-dangling")
		})
	}
}
