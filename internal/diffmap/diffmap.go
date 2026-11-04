// Package diffmap reads the hunks of one file's unified diff, as the GitHub
// and GitLab APIs return them per file, and answers the questions inline
// threads need: is a head line part of the change, which base line is an
// unchanged head line, and where does an old line sit after the change. It is
// pure: no I/O, only the standard library.
package diffmap

import (
	"regexp"
	"strconv"
	"strings"
)

// File is one file's diff: the hunks of its unified diff, head side and base side.
type File struct {
	Path, OldPath string
	Deleted       bool
	Hunks         []Hunk
}

// Hunk is one "@@" block of a unified diff.
type Hunk struct {
	OldStart, OldLines, NewStart, NewLines int
	// Lines holds, for each line of the hunk on the new side, its old line
	// (0 for an added line); removed lines are not listed.
	Lines []Pair
}

// Pair is a line of the new side with its line on the old side (0 if added).
type Pair struct{ Old, New int }

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// Parse reads the hunks of a file's patch. The patch is the hunks only, from
// the first "@@" on, as the platforms return it per file; file header lines
// ("diff --git", "---", "+++") before a hunk are skipped. A hunk header
// without numbers is skipped with the lines below it.
func Parse(path, oldPath, patch string, deleted bool) File {
	f := File{Path: path, OldPath: oldPath, Deleted: deleted}
	var cur *Hunk
	var oldNext, newNext, oldLeft, newLeft int

	flush := func() {
		if cur != nil {
			f.Hunks = append(f.Hunks, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "@@") {
			flush()
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			h := Hunk{
				OldStart: atoi(m[1]), OldLines: count(m[2]),
				NewStart: atoi(m[3]), NewLines: count(m[4]),
			}
			cur = &h
			oldNext, newNext = h.OldStart, h.NewStart
			oldLeft, newLeft = h.OldLines, h.NewLines
			continue
		}
		if cur == nil || strings.HasPrefix(line, `\`) {
			continue
		}
		if oldLeft <= 0 && newLeft <= 0 {
			continue // past the counted lines: a trailing header or blank
		}
		switch {
		case strings.HasPrefix(line, "+"):
			cur.Lines = append(cur.Lines, Pair{0, newNext})
			newNext++
			newLeft--
		case strings.HasPrefix(line, "-"):
			oldNext++
			oldLeft--
		default: // context: a space, or an empty line whose space was trimmed
			cur.Lines = append(cur.Lines, Pair{oldNext, newNext})
			oldNext++
			newNext++
			oldLeft--
			newLeft--
		}
	}
	flush()
	return f
}

// HunkOf returns the index of the hunk whose new side holds line; context
// lines count.
func (f File) HunkOf(line int) (int, bool) {
	for i, h := range f.Hunks {
		if h.NewLines > 0 && line >= h.NewStart && line < h.NewStart+h.NewLines {
			return i, true
		}
	}
	return 0, false
}

// OldLine returns the base line of an unchanged head line. It is false for an
// added line and for a line outside the hunks.
func (f File) OldLine(line int) (int, bool) {
	i, ok := f.HunkOf(line)
	if !ok {
		return 0, false
	}
	for _, p := range f.Hunks[i].Lines {
		if p.New == line {
			return p.Old, p.Old > 0
		}
	}
	return 0, false
}

// MapOld returns the new-side line of an old-side line through this diff: a
// line outside the hunks moves by the hunks above it, an unchanged line
// inside a hunk maps to its pair, and a removed line (or any line of a
// deleted file) is false.
func (f File) MapOld(oldLine int) (int, bool) {
	if f.Deleted || oldLine < 1 {
		return 0, false
	}
	shift := 0
	for _, h := range f.Hunks {
		first, last := h.OldStart, h.OldStart+h.OldLines-1
		if h.OldLines == 0 {
			// A pure insertion: OldStart is the line before it.
			first, last = h.OldStart+1, h.OldStart
		}
		switch {
		case oldLine > last:
			shift += h.NewLines - h.OldLines
		case oldLine >= first:
			for _, p := range h.Lines {
				if p.Old == oldLine {
					return p.New, true
				}
			}
			return 0, false
		default:
			return oldLine + shift, true
		}
	}
	return oldLine + shift, true
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// count reads a hunk header's line count; an omitted count means 1.
func count(s string) int {
	if s == "" {
		return 1
	}
	return atoi(s)
}
