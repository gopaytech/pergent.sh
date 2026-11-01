package inputs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/forge"
)

// RepoFolders are the folders of a repository that hold skills, in the order
// they win when two of them have a skill of the same name (D23). The first
// always runs; the others are detected and run only when turned on.
var RepoFolders = []string{".pergent/skills", ".agents/skills", ".claude/skills", ".github/skills"}

// Found is one skill entry of a folder. When Problem is set the entry is not
// usable and Name is the fallback name (the file or folder name); otherwise
// Name is the skill's name.
type Found struct {
	Name, Path, Problem string
	Content             []byte
}

// ScanFolder reads the skills of dir at ref: top-level "*.md" files and
// "<name>/SKILL.md" folders. Entries starting with "." and other files are
// skipped. A problem with one entry (no SKILL.md, a bad name or frontmatter,
// too large, a duplicate name) is reported in its Found; only a failure of src
// is an error. The result is ordered by path.
func ScanFolder(ctx context.Context, src Source, projectID int64, dir, ref string) ([]Found, error) {
	entries, err := src.ListTree(ctx, projectID, dir, ref)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	var out []Found
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
		if errors.Is(err, ErrFileTooLarge) {
			out = append(out, Found{Name: fallback, Path: file, Problem: "skill is over the limit of 128 KiB"})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", file, err)
		}
		if !found {
			problem := "file not found"
			if e.Type == "tree" {
				problem = "a skill folder needs a SKILL.md file"
			}
			out = append(out, Found{Name: fallback, Path: file, Problem: problem})
			continue
		}

		f := Found{Name: fallback, Path: file, Content: content}
		if name, problem := inspect(fallback, content); problem != "" {
			f.Problem = problem
		} else if first, dup := origin[name]; dup {
			f.Name = name
			f.Problem = fmt.Sprintf("duplicate skill name %q, also in %s", name, first)
		} else {
			f.Name = name
			origin[name] = file
		}
		out = append(out, f)
	}
	return out, nil
}

// inspect names a skill file and checks it: the size first, so a huge file is
// never parsed. problem is empty when the skill is usable.
func inspect(fallback string, content []byte) (name, problem string) {
	if len(content) > MaxSkillBytes {
		return fallback, CheckSkill(fallback, content).Error()
	}
	name, err := SkillName(content, fallback)
	if err != nil {
		return fallback, err.Error()
	}
	if err := CheckSkill(name, content); err != nil {
		return fallback, err.Error()
	}
	return name, ""
}

// Detected is a skill found in one of the repository folders that run only
// when turned on. Name is the fallback name when Problem is set.
type Detected struct{ Name, Folder, Path, Problem string }

// Detect lists the skills of RepoFolders[1:] at ref, folder by folder in
// precedence order, for the repository's settings page.
func Detect(ctx context.Context, src Source, projectID int64, ref string) ([]Detected, error) {
	var out []Detected
	for _, folder := range RepoFolders[1:] {
		found, err := ScanFolder(ctx, src, projectID, folder, ref)
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			out = append(out, Detected{Name: f.Name, Folder: folder, Path: f.Path, Problem: f.Problem})
		}
	}
	return out, nil
}

// DirSource is a Source over a local tree, such as a clone of an untrusted
// repository. It never follows a symlink and never leaves Root: an absolute
// path, a ".." segment or a path through a symlink is simply not found, and
// symlinks are left out of listings. The ref is ignored.
type DirSource struct{ Root string }

var _ Source = DirSource{}

// ErrFileTooLarge is returned by DirSource.GetFile for a file over
// ConfigMapLimit: nothing a review reads can be larger, and a clone is
// untrusted, so the file is never read into memory.
var ErrFileTooLarge = errors.New("file is too large")

// GetFile returns a regular file below Root. A missing file, or one reached
// through a symlink, is nil, false, nil.
func (d DirSource) GetFile(_ context.Context, _ int64, p, _ string) ([]byte, bool, error) {
	root, rel, ok, err := d.open(p)
	if err != nil || !ok {
		return nil, false, err
	}
	defer root.Close()

	info, ok, err := lstatNoLinks(root, rel)
	if err != nil || !ok || !info.Mode().IsRegular() {
		return nil, false, err
	}
	if info.Size() > ConfigMapLimit {
		return nil, false, ErrFileTooLarge
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, false, nil
	}
	defer f.Close()
	// The size above can be stale (a file that grows), so read through a limit.
	content, err := io.ReadAll(io.LimitReader(f, ConfigMapLimit+1))
	if err != nil {
		return nil, false, err
	}
	if len(content) > ConfigMapLimit {
		return nil, false, ErrFileTooLarge
	}
	return content, true, nil
}

// ListTree lists one directory below Root, regular files as "blob" and
// directories as "tree". A missing directory, or one reached through a
// symlink, is nil, nil.
func (d DirSource) ListTree(_ context.Context, _ int64, dir, _ string) ([]forge.TreeEntry, error) {
	root, rel, ok, err := d.open(dir)
	if err != nil || !ok {
		return nil, err
	}
	defer root.Close()

	info, ok, err := lstatNoLinks(root, rel)
	if err != nil || !ok || !info.IsDir() {
		return nil, err
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}

	var out []forge.TreeEntry
	for _, de := range des {
		e := forge.TreeEntry{Name: de.Name(), Path: path.Join(rel, de.Name())}
		if rel == "." {
			e.Path = de.Name()
		}
		switch t := de.Type(); {
		case t.IsDir():
			e.Type = "tree"
		case t.IsRegular():
			e.Type = "blob"
		default: // symlinks, devices, sockets
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// open cleans p and opens Root. ok is false when p cannot name something below
// Root or Root does not exist.
func (d DirSource) open(p string) (root *os.Root, rel string, ok bool, err error) {
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return nil, "", false, nil
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return nil, "", false, nil
		}
	}
	rel = path.Clean(p)
	root, err = os.OpenRoot(d.Root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	return root, rel, true, nil
}

// lstatNoLinks stats rel, which is not found when it or any directory above it
// is a symlink, is missing, or is a path os.Root refuses.
func lstatNoLinks(root *os.Root, rel string) (fs.FileInfo, bool, error) {
	if rel == "." {
		info, err := root.Lstat(".")
		return info, err == nil, nil
	}
	segments := strings.Split(rel, "/")
	for i := range segments {
		info, err := root.Lstat(strings.Join(segments[:i+1], "/"))
		if err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return nil, false, nil
		}
		if i == len(segments)-1 {
			return info, true, nil
		}
		if !info.IsDir() {
			return nil, false, nil
		}
	}
	return nil, false, nil
}
