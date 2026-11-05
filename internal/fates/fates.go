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
	// A track's newest fingerprint is looked for first.
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
		if t := w.owner[r.fp]; t != nil && !w.matched[t] {
			w.update(t, r)
		}
	}

	for _, t := range mine {
		if w.matched[t] || !t.live {
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
	if t.severity != "blocking" {
		t.severity = r.severity
	}
	t.live = true
	t.fate = ""
	w.matched[t] = true
	w.owner[r.fp] = t
}

// vanished decides what a live track that the review does not report becomes:
// nothing, if the review says nothing about it; the same finding reworded; or
// a track that has stopped being reported.
func (w *walk) vanished(t *track) error {
	if w.s.Failed {
		return nil
	}
	// A file renamed since the finding was reported is reviewed under its new name.
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
		if _, path, _ := find(files, t.file); !w.s.Reviewed[path] {
			return nil
		}
	}

	if t.head != w.r.Head && t.line > 0 {
		if cands := w.candidates(); len(cands) > 0 {
			files, ok, err := w.c.get(t.head, w.r.Head)
			if err != nil {
				return err
			}
			if r := w.reworded(t, cands, files, ok); r != nil {
				w.update(t, r)
				return nil
			}
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

// candidates are the findings of the review that no track has had, with a line.
func (w *walk) candidates() []*reported {
	var out []*reported
	for _, r := range w.reported {
		if w.owner[r.fp] == nil && r.line > 0 {
			out = append(out, r)
		}
	}
	return out
}

// reworded finds the finding that t was reworded into: same file (following a
// rename), the closest to its lines. Where the diff maps the old lines, the
// ranges must overlap; where it cannot, the finding must lie within Reach of
// the old lines. cands is in line order, so a tie goes to the first.
func (w *walk) reworded(t *track, cands []*reported, files []diffmap.File, ok bool) *reported {
	lo, hi, limit, file := t.line, t.end, Reach, t.file
	if ok {
		d, path, found := find(files, t.file)
		file = path
		if !found {
			limit = 0 // the file is untouched: the lines are where they were
		} else if l, ok1 := d.MapOld(lo); ok1 {
			if h, ok2 := d.MapOld(hi); ok2 {
				lo, hi, limit = l, h, 0
			}
		}
	}
	var best *reported
	bestGap := 0
	for _, r := range cands {
		if r.file != file {
			continue
		}
		g := gap(lo, hi, r.line, r.end)
		if g > limit {
			continue
		}
		if best == nil || g < bestGap {
			best, bestGap = r, g
		}
	}
	return best
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
