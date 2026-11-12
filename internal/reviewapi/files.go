package reviewapi

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // git object names are SHA-1; this is not a security use
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Committer is the identity of both commits, author and committer alike. The
// Job's `git fast-import` stream and the SHAs computed here must agree byte for
// byte, so the time is fixed at 0 +0000.
const Committer = "pergent <pergent@pergent.sh> 0 +0000"

// maxPathBytes is the longest path a caller may send.
const maxPathBytes = 1024

// maxSegmentBytes is the longest name in a path: the Job checks the head
// commit out to disk, and a file name over 255 bytes fails on ext4.
const maxSegmentBytes = 255

// File is one file of a sent change: its content before (Old) and after (New).
// A file with only New is added, with only Old is deleted, with both is
// changed (or unchanged, which the review then skips).
type File struct {
	Path string  `json:"path"`
	Old  *string `json:"old"`
	New  *string `json:"new"`
}

// FileError is a rule broken by one file of the list.
type FileError struct {
	Index   int
	Message string
}

func (e *FileError) Error() string {
	return fmt.Sprintf("files[%d]: %s", e.Index, e.Message)
}

// CheckFiles applies the rules for sent files. The error is a *FileError for a
// broken file, or a plain error for the list as a whole.
func CheckFiles(files []File) error {
	if len(files) == 0 {
		return errors.New("files must not be empty")
	}
	seen := make(map[string]bool, len(files))
	for i, f := range files {
		if msg := checkPath(f.Path); msg != "" {
			return &FileError{i, msg}
		}
		if seen[f.Path] {
			return &FileError{i, "the path appears twice"}
		}
		seen[f.Path] = true
		if f.Old == nil && f.New == nil {
			return &FileError{i, "a file needs old, new or both"}
		}
		if f.Old != nil && !utf8.ValidString(*f.Old) {
			return &FileError{i, "old is not UTF-8 text"}
		}
		if f.New != nil && !utf8.ValidString(*f.New) {
			return &FileError{i, "new is not UTF-8 text"}
		}
	}
	// A path that is a directory of another one in the same side cannot be
	// stored in one tree.
	for _, side := range []func(File) bool{
		func(f File) bool { return f.Old != nil },
		func(f File) bool { return f.New != nil },
	} {
		dirs := map[string]bool{}
		for _, f := range files {
			if !side(f) {
				continue
			}
			for j := 0; j < len(f.Path); j++ {
				if f.Path[j] == '/' {
					dirs[f.Path[:j]] = true
				}
			}
		}
		for i, f := range files {
			if side(f) && dirs[f.Path] {
				return &FileError{i, "the path is also a directory in the same side"}
			}
		}
	}
	return nil
}

// checkPath returns the broken rule of a path, or "".
func checkPath(p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return "the path must be relative"
	}
	if len(p) > maxPathBytes || !utf8.ValidString(p) {
		return "the path holds a character git can't store"
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return "the path holds a character git can't store"
		}
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == "." || s == ".." {
			return "the path must not hold .. or ."
		}
	}
	for _, s := range segs {
		if s == "" || len(s) > maxSegmentBytes || isGitName(s) {
			return "the path holds a character git can't store"
		}
	}
	return ""
}

// isGitName reports whether a segment is, or on some file system would become,
// the name .git: git refuses these in a path (its NTFS and HFS protections),
// and the Job's git may run with either off, so both are applied here. The
// segment is read without HFS-ignorable code points and in lower case. It
// counts if it starts with .git or the NTFS short name git~1 and the rest,
// once its leading spaces and dots are gone, is empty or starts with a colon
// (the stream form .git::$INDEX_ALLOCATION).
func isGitName(seg string) bool {
	seg = strings.ToLower(strings.Map(func(r rune) rune {
		switch {
		case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e,
			r >= 0x206a && r <= 0x206f, r == 0xfeff:
			return -1
		}
		return r
	}, seg))
	for _, name := range []string{".git", "git~1"} {
		if rest, ok := strings.CutPrefix(seg, name); ok {
			rest = strings.TrimLeft(rest, " .")
			if rest == "" || rest[0] == ':' {
				return true
			}
		}
	}
	return false
}

// Import is what the Job feeds to `git fast-import` and the commits that
// results from it, named before the Job runs.
type Import struct {
	Stream []byte
	Base   string // SHA-1 of refs/heads/base
	Head   string // SHA-1 of refs/heads/head
	Bytes  int    // total size of all old and new content
}

// BuildImport checks the files, builds the fast-import stream for the two
// commits (base from the old contents, head from the new ones) and computes
// both commit SHAs as git will.
func BuildImport(files []File) (Import, error) {
	if err := CheckFiles(files); err != nil {
		return Import{}, err
	}
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var im Import
	var stream bytes.Buffer
	oldMark := make([]int, len(sorted))
	newMark := make([]int, len(sorted))
	mark := 0
	blob := func(content string) int {
		mark++
		fmt.Fprintf(&stream, "blob\nmark :%d\ndata %d\n%s\n", mark, len(content), content)
		im.Bytes += len(content)
		return mark
	}
	for i, f := range sorted {
		if f.Old != nil {
			oldMark[i] = blob(*f.Old)
		}
		if f.New != nil {
			newMark[i] = blob(*f.New)
		}
	}

	fmt.Fprintf(&stream, "commit refs/heads/base\ncommitter %s\ndata 5\nbase\n", Committer)
	for i, f := range sorted {
		if f.Old != nil {
			fmt.Fprintf(&stream, "M 100644 :%d %s\n", oldMark[i], quotePath(f.Path))
		}
	}
	fmt.Fprintf(&stream, "commit refs/heads/head\ncommitter %s\ndata 5\nhead\nfrom refs/heads/base\ndeleteall\n", Committer)
	for i, f := range sorted {
		if f.New != nil {
			fmt.Fprintf(&stream, "M 100644 :%d %s\n", newMark[i], quotePath(f.Path))
		}
	}
	im.Stream = stream.Bytes()

	oldTree, newTree := newDir(), newDir()
	for _, f := range sorted {
		if f.Old != nil {
			oldTree.add(strings.Split(f.Path, "/"), gitHash("blob", []byte(*f.Old)))
		}
		if f.New != nil {
			newTree.add(strings.Split(f.Path, "/"), gitHash("blob", []byte(*f.New)))
		}
	}
	im.Base = commitSHA(oldTree.sha(), "", "base")
	im.Head = commitSHA(newTree.sha(), im.Base, "head")
	return im, nil
}

// quotePath is a path as fast-import reads a quoted one. CheckFiles has
// refused every byte that would need another escape.
func quotePath(p string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
}

// gitHash is the object name of a git object: sha1("<kind> <len>\0" + body).
func gitHash(kind string, body []byte) [20]byte {
	h := sha1.New() //nolint:gosec // see the import
	fmt.Fprintf(h, "%s %d\x00", kind, len(body))
	h.Write(body)
	var sum [20]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func commitSHA(tree [20]byte, parent, msg string) string {
	body := "tree " + hex.EncodeToString(tree[:]) + "\n"
	if parent != "" {
		body += "parent " + parent + "\n"
	}
	body += "author " + Committer + "\ncommitter " + Committer + "\n\n" + msg + "\n"
	sum := gitHash("commit", []byte(body))
	return hex.EncodeToString(sum[:])
}

// dir is a directory of the tree being built.
type dir struct {
	files map[string][20]byte
	dirs  map[string]*dir
}

func newDir() *dir {
	return &dir{files: map[string][20]byte{}, dirs: map[string]*dir{}}
}

func (d *dir) add(segs []string, blob [20]byte) {
	if len(segs) == 1 {
		d.files[segs[0]] = blob
		return
	}
	sub := d.dirs[segs[0]]
	if sub == nil {
		sub = newDir()
		d.dirs[segs[0]] = sub
	}
	sub.add(segs[1:], blob)
}

// sha is the tree object name. Git sorts the entries by name, a directory's
// name compared as name + "/", so a/c comes after a.b.
func (d *dir) sha() [20]byte {
	type entry struct {
		key, mode, name string
		sha             [20]byte
	}
	var entries []entry
	for n, s := range d.files {
		entries = append(entries, entry{n, "100644", n, s})
	}
	for n, sub := range d.dirs {
		entries = append(entries, entry{n + "/", "40000", n, sub.sha()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	var body []byte
	for _, e := range entries {
		body = append(body, e.mode+" "+e.name+"\x00"...)
		body = append(body, e.sha[:]...)
	}
	return gitHash("tree", body)
}
