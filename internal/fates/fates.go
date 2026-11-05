// Package fates works out what became of each finding raised on a change:
// still open when it merged or closed, dismissed, fixed, or gone for no
// telling reason. It walks the change's reviews oldest first and follows each
// finding, by fingerprint and, when a finding is reworded, by its lines. It is
// pure: no I/O, only the standard library and diffmap. The comparisons it
// needs between two heads come from the caller.
package fates

import (
	"errors"
	"sort"

	"github.com/gopaytech/pergent.sh/internal/diffmap"
)

// What became of a finding.
const (
	Dismissed = "dismissed"
	Open      = "open"
	Fixed     = "fixed"
	WentAway  = "went_away"
)

const (
	// Reach is how many lines from a finding's lines a change still counts as
	// touching it, and how far a reworded finding may sit from the lines of
	// one whose old lines cannot be mapped.
	Reach = 3
	// MaxCompares is the most comparisons made for one change; a finding
	// that would need another went away.
	MaxCompares = 20
)

const severitySuggestion = "suggestion"

// Finding is a finding of one review. Anything but a suggestion is blocking.
// A Line of 0 is a finding with no line; an EndLine of 0 is one line.
type Finding struct {
	Fingerprint, File string
	Line, EndLine     int
	Severity          string
}

// Skill is one skill's part of a review.
type Skill struct {
	Name, Hash string
	Failed     bool            // its verdict is "failed"
	Reviewed   map[string]bool // files it reviewed, carried ones too
	LeftOut    bool            // it has a "Left out because of past dismissals:" note
	Findings   []Finding
}

// Review is a decided review at Head.
type Review struct {
	Head   string
	Skills []Skill
}

// Dismissal is a dismissal on the change.
type Dismissal struct{ Fingerprint, Severity string }

// Compare gives the diff between two heads, by the file's path on the new side.
// ErrGone means it can never be had; any other error is worth trying again.
type Compare func(from, to string) ([]diffmap.File, error)

// ErrGone is what a Compare returns when the comparison can never be had.
var ErrGone = errors.New("the comparison can't be had")

// Input is a change: its reviews oldest first, its dismissals, and Compare.
type Input struct {
	Reviews    []Review
	Dismissals []Dismissal
	Compare    Compare
}

// Fate is what became of one finding of one skill. Fingerprint is the newest
// the finding had, Severity the highest.
type Fate struct {
	Fingerprint, Skill, SkillHash, Severity, Fate string
}

// track follows one finding through the reviews.
type track struct {
	skill, hash, file string
	fp                string          // the newest fingerprint
	fps               map[string]bool // every fingerprint it had
	line, end         int
	head              string // head of the last review that reported it
	severity          string
	live              bool   // reported as of the last review that says anything
	fate              string // when not live: Fixed or WentAway
}

// Decide works out the fate of every finding of the reviews. An error other
// than ErrGone from Compare is returned, with no fates.
func Decide(in Input) ([]Fate, error) {
	c := &comparer{fn: in.Compare, cache: map[[2]string]*result{}}
	var tracks []*track
	for _, r := range in.Reviews {
		for _, s := range r.Skills {
			w := &walk{c: c, r: r, s: s, reported: group(s.Findings), owner: map[string]*track{}, matched: map[*track]bool{}}
			var err error
			if tracks, err = w.run(tracks); err != nil {
				return nil, err
			}
		}
	}

	dismissed := map[string][]string{}
	for _, d := range in.Dismissals {
		dismissed[d.Fingerprint] = append(dismissed[d.Fingerprint], severity(d.Severity))
	}
	var out []Fate
	for _, t := range tracks {
		fate := Open
		switch {
		case covered(t, dismissed):
			fate = Dismissed
		case !t.live:
			fate = t.fate
		}
		out = append(out, Fate{Fingerprint: t.fp, Skill: t.skill, SkillHash: t.hash, Severity: t.severity, Fate: fate})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Fingerprint != out[j].Fingerprint {
			return out[i].Fingerprint < out[j].Fingerprint
		}
		return out[i].Skill < out[j].Skill
	})
	return out, nil
}

// covered reports whether a dismissal covers the finding: the same
// fingerprint, and a blocking dismissal or a finding that only ever was a
// suggestion. Any fingerprint the finding had counts, so rewording it does
// not lose a dismissal.
func covered(t *track, dismissed map[string][]string) bool {
	for fp := range t.fps {
		for _, sev := range dismissed[fp] {
			if sev == "blocking" || t.severity == severitySuggestion {
				return true
			}
		}
	}
	return false
}

// severity is "suggestion" or "blocking": anything else is blocking.
func severity(s string) string {
	if s == severitySuggestion {
		return s
	}
	return "blocking"
}

// reported is a fingerprint as one skill's review reports it: the first (lowest)
// line it was raised on, and the highest severity.
type reported struct {
	fp, file  string
	line, end int
	severity  string
}

// group folds the findings of one review by fingerprint, in line order.
func group(fs []Finding) []*reported {
	by := map[string]*reported{}
	var out []*reported
	for _, f := range fs {
		sev, end := severity(f.Severity), endOf(f.Line, f.EndLine)
		r := by[f.Fingerprint]
		if r == nil {
			r = &reported{fp: f.Fingerprint, file: f.File, line: f.Line, end: end, severity: sev}
			by[f.Fingerprint] = r
			out = append(out, r)
			continue
		}
		if sev != severitySuggestion {
			r.severity = sev
		}
		if f.Line > 0 && (r.line <= 0 || f.Line < r.line) {
			r.file, r.line, r.end = f.File, f.Line, end
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].line != out[j].line {
			return out[i].line < out[j].line
		}
		return out[i].fp < out[j].fp
	})
	return out
}

// walk is one skill of one review being read against the tracks.
type walk struct {
	c        *comparer
	r        Review
	s        Skill
	reported []*reported
	owner    map[string]*track // fingerprint to the track of this skill that had it
	matched  map[*track]bool   // tracks that this review accounts for
}

func (w *walk) run(tracks []*track) ([]*track, error) {
	var mine []*track
	for _, t := range tracks {
		if t.skill != w.s.Name {
			continue
		}
		mine = append(mine, t)
		for fp := range t.fps {
			w.owner[fp] = t
		}
	}

	// A finding reported counts as reported, even by a skill that failed or a
	// review of other files: the skip rules are about a finding not reported.
	// A track's newest fingerprint is looked for first; another wording of it
	// reported beside it only raises its severity.
	byFP := map[string]*reported{}
	for _, r := range w.reported {
		byFP[r.fp] = r
	}
	for _, t := range mine {
		if r := byFP[t.fp]; r != nil {
			w.update(t, r)
		}
	}
	for _, r := range w.reported {
		if t := w.owner[r.fp]; t != nil {
			if w.matched[t] {
				t.raise(r.severity)
			} else {
				w.update(t, r)
			}
		}
	}

	// A finding not reported may be reworded, whatever the review says about
	// the file; only then can it be said to have stopped being reported.
	var pending []*track
	for _, t := range mine {
		if !w.matched[t] && t.live {
			pending = append(pending, t)
		}
	}
	if err := w.reword(pending); err != nil {
		return nil, err
	}
	for _, t := range pending {
		if w.matched[t] {
			continue
		}
		if err := w.vanished(t); err != nil {
			return nil, err
		}
	}

	for _, r := range w.reported {
		if w.owner[r.fp] != nil {
			continue
		}
		t := &track{skill: w.s.Name, fps: map[string]bool{}}
		w.update(t, r)
		tracks = append(tracks, t)
	}
	return tracks, nil
}

// update records that the review reported the finding r as t. A track that had
// vanished is live again.
func (w *walk) update(t *track, r *reported) {
	t.fp, t.hash, t.file = r.fp, w.s.Hash, r.file
	t.fps[r.fp] = true
	t.line, t.end = r.line, r.end
	t.head = w.r.Head
	t.raise(r.severity)
	t.live = true
	t.fate = ""
	w.matched[t] = true
	w.owner[r.fp] = t
}

// raise makes the track's severity at least sev.
func (t *track) raise(sev string) {
	if t.severity != "blocking" {
		t.severity = sev
	}
}

// vanished decides what a live track that the review does not report becomes:
// nothing, if the review says nothing about it, or a track that has stopped
// being reported.
func (w *walk) vanished(t *track) error {
	if w.s.Failed {
		return nil
	}
	// A file renamed since the finding was reported is reviewed under its new
	// name. A deleted file is never reviewed, and its findings are gone.
	if !w.s.Reviewed[t.file] {
		if t.head == w.r.Head {
			return nil
		}
		files, ok, err := w.c.get(t.head, w.r.Head)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if d, path, _ := find(files, t.file); !d.Deleted && !w.s.Reviewed[path] {
			return nil
		}
	}

	t.live = false
	t.fate = WentAway
	if t.head == w.r.Head || w.s.LeftOut {
		return nil
	}
	files, ok, err := w.c.get(t.head, w.r.Head)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	d, _, found := find(files, t.file)
	switch {
	case !found:
	case d.Deleted:
		t.fate = Fixed
	case t.line > 0:
		if d.ChangedNear(t.line, t.end, Reach) {
			t.fate = Fixed
		}
	case len(d.Hunks) > 0:
		t.fate = Fixed
	}
	return nil
}

// candidates are the findings of the review that no track has had.
func (w *walk) candidates() []*reported {
	var out []*reported
	for _, r := range w.reported {
		if w.owner[r.fp] == nil {
			out = append(out, r)
		}
	}
	return out
}

// reword joins each pending track to the finding it was reworded into: same
// file (following a rename), no track yet, a line if the track had one, and the
// closest to its lines, a tie going to the earlier track and the lower line.
// A track with no line takes a finding with no line in the same file. On the
// same head the lines are where they were; otherwise a comparison is made, and
// only if some candidate could be the track's match.
func (w *walk) reword(pending []*track) error {
	cands := w.candidates()
	if len(cands) == 0 {
		return nil
	}
	type pair struct{ t, c, gap int }
	var pairs []pair
	for i, t := range pending {
		if !hasKind(cands, t.line > 0) {
			continue
		}
		lo, hi, limit, file := t.line, t.end, 0, t.file
		if t.head != w.r.Head {
			files, ok, err := w.c.get(t.head, w.r.Head)
			if err != nil {
				return err
			}
			lo, hi, limit, file = mapRange(files, ok, t)
		}
		for j, r := range cands {
			if r.file != file || (r.line > 0) != (t.line > 0) {
				continue
			}
			g := 0
			if t.line > 0 {
				g = gap(lo, hi, r.line, r.end)
			}
			if g <= limit {
				pairs = append(pairs, pair{i, j, g})
			}
		}
	}
	sort.SliceStable(pairs, func(a, b int) bool {
		if pairs[a].gap != pairs[b].gap {
			return pairs[a].gap < pairs[b].gap
		}
		if pairs[a].t != pairs[b].t {
			return pairs[a].t < pairs[b].t
		}
		return pairs[a].c < pairs[b].c
	})
	taken := map[int]bool{}
	for _, p := range pairs {
		if w.matched[pending[p.t]] || taken[p.c] {
			continue
		}
		taken[p.c] = true
		w.update(pending[p.t], cands[p.c])
	}
	return nil
}

func hasKind(rs []*reported, withLine bool) bool {
	for _, r := range rs {
		if (r.line > 0) == withLine {
			return true
		}
	}
	return false
}

// mapRange says where the lines of a track are at the newer head of a
// comparison, and how far a finding may sit from them: not at all where the
// diff maps both ends of the lines or doesn't touch the file; Reach where it
// can't, as when the lines were rewritten, the diff has no hunks to trust, or
// the comparison is gone. An end that maps goes where it maps; one that
// doesn't moves with the lines above it. The path is the file's path at the
// newer head.
func mapRange(files []diffmap.File, ok bool, t *track) (lo, hi, limit int, path string) {
	if !ok {
		return t.line, t.end, Reach, t.file
	}
	d, path, found := find(files, t.file)
	if !found {
		return t.line, t.end, 0, path
	}
	if len(d.Hunks) == 0 {
		return t.line, t.end, Reach, path
	}
	limit = 0
	end := func(oldLine int) int {
		if n, ok := d.MapOld(oldLine); ok {
			return n
		}
		limit = Reach
		return oldLine + shiftAt(d, oldLine)
	}
	lo = end(t.line)
	hi = end(t.end)
	return lo, hi, limit, path
}

// shiftAt is how far the diff moves an old line that has no new line of its
// own: the hunks above move it, and so do the lines added or removed above it
// within its own hunk, as the nearest unchanged line above it shows.
func shiftAt(d diffmap.File, oldLine int) int {
	shift := 0
	for _, h := range d.Hunks {
		last := h.OldStart + h.OldLines - 1
		if h.OldLines == 0 {
			last = h.OldStart // a pure insertion: OldStart is the line before it
		}
		if oldLine > last {
			shift += h.NewLines - h.OldLines
			continue
		}
		if h.OldLines > 0 && oldLine >= h.OldStart {
			for _, p := range h.Lines {
				if p.Old > 0 && p.Old < oldLine {
					shift = p.New - p.Old
				}
			}
		}
		break
	}
	return shift
}

// find returns the diff of a file given its path on the old side, and its path
// on the new side. It follows a rename; found is false for a file the
// diff doesn't touch.
func find(files []diffmap.File, path string) (d diffmap.File, newPath string, found bool) {
	for _, f := range files {
		if f.OldPath == path && f.Path != path {
			return f, f.Path, true
		}
	}
	for _, f := range files {
		if f.Path == path && (f.OldPath == "" || f.OldPath == path) {
			return f, path, true
		}
	}
	return diffmap.File{}, path, false
}

func endOf(line, end int) int {
	if end < line {
		return line
	}
	return end
}

// gap is the number of lines between two line ranges, 0 when they overlap.
func gap(aLo, aHi, bLo, bHi int) int {
	switch {
	case bLo > aHi:
		return bLo - aHi
	case aLo > bHi:
		return aLo - bHi
	}
	return 0
}

// comparer calls Compare once per pair of heads and gives up after
// MaxCompares calls.
type comparer struct {
	fn    Compare
	n     int
	cache map[[2]string]*result
}

type result struct {
	files []diffmap.File
	gone  bool
}

// get returns the diff between two heads. ok is false when it can't be had:
// the platform says so for good, or the cap is spent. Any other error is
// returned, and not cached.
func (c *comparer) get(from, to string) (files []diffmap.File, ok bool, err error) {
	key := [2]string{from, to}
	if r := c.cache[key]; r != nil {
		return r.files, !r.gone, nil
	}
	if c.fn == nil || c.n >= MaxCompares {
		c.cache[key] = &result{gone: true}
		return nil, false, nil
	}
	c.n++
	files, err = c.fn(from, to)
	switch {
	case errors.Is(err, ErrGone):
		c.cache[key] = &result{gone: true}
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	c.cache[key] = &result{files: files}
	return files, true, nil
}
