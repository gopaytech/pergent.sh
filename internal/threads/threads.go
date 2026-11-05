// Package threads decides what to do with the inline review threads of a
// change: from the findings of a review, the diff of the change and the
// threads already posted, it plans which findings to post on the diff, which
// threads to resolve, reopen or rebind to a reworded finding, and which
// findings stay in the summary comment, with a note saying why. It is pure: no
// I/O. Carrying the plan out is the publisher's job.
package threads

import (
	"sort"

	"github.com/gopaytech/pergent.sh/internal/diffmap"
)

// State is what pergent.sh itself recorded about a thread.
type State string

const (
	StateOpen      State = "open"
	StateFixed     State = "fixed"     // resolved by pergent.sh: the finding is gone
	StateGone      State = "gone"      // resolved by pergent.sh: the file left the change
	StateDismissed State = "dismissed" // resolved by pergent.sh: the finding was dismissed
)

// Cap is the most pergent-owned threads that may be open on a change at once.
const Cap = 20

// reach is how far, in lines, a reworded finding may sit from the stored
// range of a thread whose lines cannot be mapped to the head.
const reach = 5

const severityBlocking = "blocking"

// Notes say why a finding is in the summary comment and not on the diff.
// NoteFailed and NoteTooLong are set by the engine, the rest by Decide.
const (
	NoteOffDiff  = "Not on the diff: its lines didn't change here, so it can't be a comment on them."
	NoteOverCap  = "Over the limit of 20 open inline threads; it moves to the diff when one is resolved."
	NoteFailed   = "Posting it on the diff failed, so it is listed here."
	NoteKeptRule = "Kept here: this branch requires threads resolved before merge."
	NoteTooLong  = "Too long for an inline comment, so it is listed here."
)

// Replies posted on a thread when it changes state. ReplyFixed, ReplyBack and
// ReplyDismissed are functions because they carry a value.
const (
	ReplyGone     = "No longer in this change."
	ReplyRestored = "The dismissal was withdrawn, so this finding is open again."
)

// ReplyFixed is the reply on a thread whose finding is gone at head.
func ReplyFixed(head string) string { return "Fixed in `" + ShortSHA(head) + "`." }

// ReplyLeftOut is the reply on a thread whose finding the review no longer
// reports, but whose skill says it left findings out because of past
// dismissals: the code may not have changed, so the thread stays open.
func ReplyLeftOut(head string) string {
	return "No longer reported in `" + ShortSHA(head) + "`; see the note on past dismissals in the comment."
}

// ReplyBack is the reply on a thread whose finding came back at head.
func ReplyBack(head string) string { return "Back in `" + ShortSHA(head) + "`." }

// ReplyDismissed is the reply on a thread whose finding was dismissed. By and
// Reason are used as given; the caller makes them safe to post.
func ReplyDismissed(d Dismissal) string {
	if d.Reason == "" {
		return "Dismissed by " + d.By + "."
	}
	return "Dismissed by " + d.By + ": " + d.Reason
}

// ShortSHA is the first 7 characters of a commit SHA.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Finding is a finding of the review, as far as thread placement cares.
type Finding struct {
	Fingerprint, ID, Skill, File string
	Severity                     string // "blocking" or "suggestion"
	Line, EndLine                int
	HasSuggestion                bool
}

// Thread is an inline thread pergent.sh owns on the change, stored or adopted.
type Thread struct {
	Fingerprint, Skill, File string
	Line, EndLine            int
	Commit                   string
	State                    State
	Resolved                 bool // as the platform shows it now
	// LeftOutNoted: the thread was told, with ReplyLeftOut, that its finding
	// was left out of a review, and has not been told since that it is back.
	LeftOutNoted bool
}

// Dismissal says who dismissed a finding and why.
type Dismissal struct{ By, Reason string }

// Coverage is what one skill did in this review.
type Coverage struct {
	Failed   bool
	Reviewed map[string]bool // files the skill reviewed in this review
	// LeftOut: the skill's notes say it left a finding out because of a past
	// dismissal. It says which skill, not which finding.
	LeftOut bool
}

// Input is everything Decide looks at.
type Input struct {
	Head      string                  // the reviewed head SHA
	Findings  []Finding               // the review's findings that are not dismissed
	Dismissed map[string]Dismissal    // by fingerprint
	Skills    map[string]Coverage     // by skill name, every skill of the review
	Diff      map[string]diffmap.File // the change's files at Head, by path
	Threads   []Thread                // pergent-owned threads of the change
	// Map gives a thread's lines at Head; ok is false when they can't be mapped.
	// A nil Map maps nothing.
	Map          func(t Thread) (line, endLine int, ok bool)
	OnlyBlocking bool // the target branch requires threads resolved before merge
}

// Post is a finding to post as a new inline thread.
type Post struct {
	Finding       Finding
	Line, EndLine int  // the anchor; EndLine == Line for one line
	Suggestion    bool // post the suggestion block
}

// Resolve is a thread to resolve.
type Resolve struct {
	Thread Thread
	State  State // StateFixed, StateGone or StateDismissed
	Reply  string
}

// Note is an open thread to reply on, without resolving it.
type Note struct {
	Thread Thread
	Reply  string
}

// Reopen is a resolved thread to open again.
type Reopen struct {
	Thread Thread
	Reply  string
}

// Rebind moves a thread to a reworded finding: the thread keeps its place and
// history, and from now on is the thread of Fingerprint at Line..EndLine.
type Rebind struct {
	Thread        Thread // as stored, with its old fingerprint
	Fingerprint   string
	Line, EndLine int
}

// Plan is what Decide decides. Its slices are in a fixed order: Post blocking
// first, then by file, then line, then fingerprint; Resolve, Reopen and Rebind
// by the thread's file, then line, then fingerprint. Note holds the threads told
// that their finding was left out; they stay open. Back holds the open threads
// that were told so and whose finding is reported again.
type Plan struct {
	Post    []Post
	Resolve []Resolve
	Note    []Note
	Back    []Note
	Reopen  []Reopen
	Rebind  []Rebind
	// Inline holds the fingerprints shown on the diff: kept, reopened,
	// rebound or about to be posted.
	Inline map[string]bool
	// Notes maps a fingerprint to why its finding is in the summary comment.
	Notes map[string]string
}

// Decide plans the inline threads of a review. See the package comment.
func Decide(in Input) Plan {
	p := Plan{Inline: map[string]bool{}, Notes: map[string]string{}}

	findings := dedupe(in.Findings)
	byFP := map[string]Finding{}
	for _, f := range findings {
		byFP[f.Fingerprint] = f
	}
	threads := append([]Thread(nil), in.Threads...)
	sort.SliceStable(threads, func(i, j int) bool { return lessThread(threads[i], threads[j]) })

	keptRule := map[string]bool{}

	// Exact matches, and the findings that now have a thread.
	hasThread := map[string]bool{}
	var unmatched []Thread           // in priority order for reworded matching
	matched := map[string][]Thread{} // by finding fingerprint
	for _, t := range threads {
		if _, ok := byFP[t.Fingerprint]; ok {
			hasThread[t.Fingerprint] = true
			matched[t.Fingerprint] = append(matched[t.Fingerprint], t)
		} else {
			unmatched = append(unmatched, t)
		}
	}

	// Live threads claim a reworded finding before resolved ones, so an old
	// fixed thread never takes the finding of a thread that is still open.
	sort.SliceStable(unmatched, func(i, j int) bool { return claimTier(unmatched[i]) < claimTier(unmatched[j]) })

	var rest []Thread // unmatched threads that are not rebound
	for _, t := range unmatched {
		if _, dismissed := in.Dismissed[t.Fingerprint]; dismissed || t.State == StateDismissed {
			// A dismissed thread is no reworded candidate: reopening it would
			// say a dismissal was withdrawn that never was.
			rest = append(rest, t)
			continue
		}
		f, ok := reworded(in, t, findings, hasThread)
		if !ok {
			rest = append(rest, t)
			continue
		}
		hasThread[f.Fingerprint] = true
		matched[f.Fingerprint] = append(matched[f.Fingerprint], t)
		p.Rebind = append(p.Rebind, Rebind{Thread: t, Fingerprint: f.Fingerprint, Line: f.Line, EndLine: endOf(f.Line, f.EndLine)})
	}

	// Threads with a finding.
	reopened := 0
	for fp, ts := range matched {
		for _, t := range ts {
			switch t.State {
			case StateFixed, StateGone, StateDismissed:
				if in.OnlyBlocking && byFP[fp].Severity != severityBlocking {
					keptRule[fp] = true // a suggestion stays in the summary
					continue
				}
				reply := ReplyBack(in.Head)
				if t.State == StateDismissed {
					reply = ReplyRestored
				}
				p.Reopen = append(p.Reopen, Reopen{Thread: t, Reply: reply})
				p.Inline[fp] = true
				reopened++
			default: // open, or a state we don't know: kept, even when a person resolved it
				p.Inline[fp] = true
				if t.LeftOutNoted && !t.Resolved {
					// Its last message says the finding is no longer reported.
					p.Back = append(p.Back, Note{Thread: t, Reply: ReplyBack(in.Head)})
				}
			}
		}
	}
	for fp := range keptRule {
		if !p.Inline[fp] {
			p.Notes[fp] = NoteKeptRule
		}
	}

	// Threads without one.
	for _, t := range rest {
		if d, ok := in.Dismissed[t.Fingerprint]; ok {
			if t.State != StateDismissed {
				p.Resolve = append(p.Resolve, Resolve{Thread: t, State: StateDismissed, Reply: ReplyDismissed(d)})
			}
			continue
		}
		if t.State != StateOpen || t.Resolved {
			continue // resolved by a person: not ours to resolve or reopen
		}
		d, inDiff := in.Diff[t.File]
		switch cov, known := in.Skills[t.Skill]; {
		case !inDiff || d.Deleted:
			p.Resolve = append(p.Resolve, Resolve{Thread: t, State: StateGone, Reply: ReplyGone})
		case known && !cov.Failed && cov.Reviewed[t.File] && cov.LeftOut:
			// The finding may be missing because the skill left it out, not
			// because the code changed: the thread stays open, and is told once.
			if !t.LeftOutNoted {
				p.Note = append(p.Note, Note{Thread: t, Reply: ReplyLeftOut(in.Head)})
			}
		case known && !cov.Failed && cov.Reviewed[t.File]:
			p.Resolve = append(p.Resolve, Resolve{Thread: t, State: StateFixed, Reply: ReplyFixed(in.Head)})
		}
	}

	// New findings.
	var cands []Post
	for _, f := range findings {
		if hasThread[f.Fingerprint] {
			continue
		}
		post, note := place(in, f)
		switch {
		case note != "":
			p.Notes[f.Fingerprint] = note
		case in.OnlyBlocking && f.Severity != severityBlocking:
			p.Notes[f.Fingerprint] = NoteKeptRule
		default:
			cands = append(cands, post)
		}
	}

	// The cap counts every pergent-owned thread open at once, matched or not.
	open := reopened
	for _, t := range in.Threads {
		if countsOpen(t) {
			open++
		}
	}
	for _, r := range p.Resolve {
		if countsOpen(r.Thread) {
			open--
		}
	}
	room := Cap - open
	if room < 0 {
		room = 0
	}
	sort.SliceStable(cands, func(i, j int) bool { return lessPost(cands[i], cands[j]) })
	for i, c := range cands {
		if i < room {
			p.Post = append(p.Post, c)
			p.Inline[c.Finding.Fingerprint] = true
		} else {
			p.Notes[c.Finding.Fingerprint] = NoteOverCap
		}
	}

	sort.SliceStable(p.Resolve, func(i, j int) bool { return lessThread(p.Resolve[i].Thread, p.Resolve[j].Thread) })
	sort.SliceStable(p.Note, func(i, j int) bool { return lessThread(p.Note[i].Thread, p.Note[j].Thread) })
	sort.SliceStable(p.Back, func(i, j int) bool { return lessThread(p.Back[i].Thread, p.Back[j].Thread) })
	sort.SliceStable(p.Reopen, func(i, j int) bool { return lessThread(p.Reopen[i].Thread, p.Reopen[j].Thread) })
	sort.SliceStable(p.Rebind, func(i, j int) bool { return lessThread(p.Rebind[i].Thread, p.Rebind[j].Thread) })
	return p
}

// dedupe drops findings that share a fingerprint, keeping the first in the
// order blocking first, then file, line. The result is in file order.
func dedupe(in []Finding) []Finding {
	all := append([]Finding(nil), in...)
	sort.SliceStable(all, func(i, j int) bool {
		return lessPost(Post{Finding: all[i]}, Post{Finding: all[j]})
	})
	seen := map[string]bool{}
	out := all[:0]
	for _, f := range all {
		if seen[f.Fingerprint] {
			continue
		}
		seen[f.Fingerprint] = true
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return lessFinding(out[i], out[j]) })
	return out
}

// countsOpen reports whether a thread is open for the cap: the platform shows
// it unresolved and pergent.sh has not resolved it. A state we don't know
// counts as open.
func countsOpen(t Thread) bool {
	if t.Resolved {
		return false
	}
	switch t.State {
	case StateFixed, StateGone, StateDismissed:
		return false
	}
	return true
}

// claimTier orders threads for reworded matching: open threads the platform
// shows unresolved first, then open threads a person resolved, then fixed and
// gone ones. A state we don't know counts as open.
func claimTier(t Thread) int {
	switch {
	case t.State == StateFixed || t.State == StateGone:
		return 2
	case t.Resolved:
		return 1
	}
	return 0
}

// reworded finds the finding that a thread without an exact match now stands
// for: same skill and file, no thread yet, and the closest to the thread. With
// Map the ranges must overlap at Head; when the thread can't be mapped the
// finding must lie within reach of the stored range. findings is in file order.
func reworded(in Input, t Thread, findings []Finding, taken map[string]bool) (Finding, bool) {
	lo, hi, mapped := 0, 0, false
	if in.Map != nil {
		if l, e, ok := in.Map(t); ok {
			lo, hi, mapped = l, endOf(l, e), true
		}
	}
	if !mapped {
		lo, hi = t.Line, endOf(t.Line, t.EndLine)
	}
	limit := reach
	if mapped {
		limit = 0
	}
	var best Finding
	bestDist := -1
	for _, f := range findings {
		if taken[f.Fingerprint] || f.Skill != t.Skill || f.File != t.File || f.Line <= 0 {
			continue
		}
		d := gap(lo, hi, f.Line, endOf(f.Line, f.EndLine))
		if d > limit {
			continue
		}
		if bestDist < 0 || d < bestDist {
			best, bestDist = f, d
		}
	}
	return best, bestDist >= 0
}

// place decides where a new finding goes on the diff. It returns a note,
// and no post, when the finding can't be a comment on the diff.
func place(in Input, f Finding) (Post, string) {
	d, ok := in.Diff[f.File]
	if !ok || d.Deleted || f.Line <= 0 {
		return Post{}, NoteOffDiff
	}
	h, ok := d.HunkOf(f.Line)
	if !ok {
		return Post{}, NoteOffDiff
	}
	post := Post{Finding: f, Line: f.Line, EndLine: f.Line}
	if f.EndLine <= f.Line {
		post.Suggestion = f.HasSuggestion
		return post, ""
	}
	eh, ok := d.HunkOf(f.EndLine)
	if !ok {
		return Post{}, NoteOffDiff
	}
	if eh == h {
		post.EndLine = f.EndLine
		post.Suggestion = f.HasSuggestion
	}
	return post, ""
}

// endOf is the last line of a range that starts at line; an unset or smaller
// end means a single line.
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

func lessFinding(a, b Finding) bool {
	if a.File != b.File {
		return a.File < b.File
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	if a.EndLine != b.EndLine {
		return a.EndLine < b.EndLine
	}
	return a.Fingerprint < b.Fingerprint
}

func lessThread(a, b Thread) bool {
	if a.File != b.File {
		return a.File < b.File
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Fingerprint < b.Fingerprint
}

func lessPost(a, b Post) bool {
	ab, bb := a.Finding.Severity == severityBlocking, b.Finding.Severity == severityBlocking
	if ab != bb {
		return ab
	}
	return lessFinding(a.Finding, b.Finding)
}
