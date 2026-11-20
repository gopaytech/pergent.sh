// Package chain picks the entry a review attempt runs on, from the chain in
// effect for its repository. It reads nothing and writes nothing: the caller
// loads the chain, the entries, the marks and what the review already tried,
// and acts on the Decision.
package chain

import (
	"fmt"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Why an entry is skipped, besides a live mark, whose reason is its class
// (store.ClassQuota, ClassBilling, ClassAuth).
const (
	ReasonTried         = "tried"          // this review already ran it
	ReasonRemoved       = "removed"        // no such entry any more
	ReasonNotAllowed    = "not_allowed"    // a global entry teams may no longer use
	ReasonNotOwner      = "not_owner"      // a team entry of a team that doesn't own the repository
	ReasonKeyUnusable   = "key_unusable"   // no key, or it can't be opened
	ReasonInstanceQuota = "instance_quota" // a global entry, and the instance's token quota is used up
)

// Input is everything Resolve reads. Entries holds every entry known now,
// with its key ("" when it has none or it can't be opened); a ref missing
// from it is a removed entry. OwnerTeam is the team that owns the repository,
// 0 for none. Tried is the entries earlier attempts of this review ran.
type Input struct {
	Scope               string // store.ScopeGlobal, ScopeTeam or ScopeRepo
	Refs                []store.EntryRef
	Entries             map[store.EntryRef]instances.Model
	OwnerTeam           int64
	Marks               map[store.EntryRef]store.Mark
	Tried               map[store.EntryRef]bool
	InstanceQuotaUsedUp bool
	Now                 time.Time
}

// Skip is an entry passed over, and why. Name is "" for a removed entry.
type Skip struct {
	Ref    store.EntryRef
	Name   string
	Reason string
}

// Decision is what the attempt does. With an entry to run, Entry and Ref are
// it, Position its place in the chain (from 1) and FirstSkip why position 1
// was passed over ("" when it runs). Otherwise exactly one of Wait (the chain
// is empty: wait as with no model), Hold (only the instance quota stopped
// entries: hold on it) and Exhausted (fail) is set. Length is the chain's.
type Decision struct {
	Entry     instances.Model
	Ref       store.EntryRef
	Position  int
	Length    int
	FirstSkip string
	Skips     []Skip
	Wait      bool
	Hold      bool
	Exhausted bool
}

// Resolve walks the chain from position 1 and takes the first entry not
// skipped. A 1-entry chain ignores marks: it never falls back, so it runs
// and fails as it did before chains, and another chain's mark on its entry
// doesn't stop it.
func Resolve(in Input) Decision {
	d := Decision{Length: len(in.Refs)}
	if len(in.Refs) == 0 {
		d.Wait = true
		return d
	}
	for i, ref := range in.Refs {
		m, known := in.Entries[ref]
		reason := skipReason(in, ref, m, known)
		if reason == "" {
			d.Entry, d.Ref, d.Position = m, ref, i+1
			return d
		}
		if i == 0 {
			d.FirstSkip = reason
		}
		name := ""
		if known {
			name = m.Name
		}
		d.Skips = append(d.Skips, Skip{Ref: ref, Name: name, Reason: reason})
	}
	d.Hold = true
	for _, s := range d.Skips {
		if s.Reason != ReasonInstanceQuota {
			d.Hold = false
			break
		}
	}
	d.Exhausted = !d.Hold
	return d
}

// skipReason is why ref is passed over, "" when it runs. A live mark comes
// before tried, so the entry an earlier attempt fell back from reads as its
// class (the footer says why); the instance quota comes last, so a hold
// waits only for entries that could run once it frees up.
func skipReason(in Input, ref store.EntryRef, m instances.Model, known bool) string {
	switch {
	case !known:
		return ReasonRemoved
	case ref.Source == store.SourceGlobal && !m.TeamsMayUse && teamSaved(in):
		return ReasonNotAllowed
	case ref.Source == store.SourceTeam && (in.OwnerTeam == 0 || m.TeamID != in.OwnerTeam):
		return ReasonNotOwner
	}
	if mk, ok := in.Marks[ref]; ok && len(in.Refs) > 1 && live(mk, in.Now) {
		return mk.Class
	}
	switch {
	case in.Tried[ref]:
		return ReasonTried
	case m.Key == "":
		return ReasonKeyUnusable
	case ref.Source == store.SourceGlobal && in.InstanceQuotaUsedUp:
		return ReasonInstanceQuota
	}
	return ""
}

// teamSaved reports whether a team's maintainers may have saved the chain:
// a team chain, or the chain of a repository a team owns. The global chain,
// and a repository chain with no owner, only an admin saves.
func teamSaved(in Input) bool {
	return in.Scope == store.ScopeTeam || (in.Scope == store.ScopeRepo && in.OwnerTeam != 0)
}

// live reports whether a mark still applies at now: an auth mark until it is
// cleared, the others until their until.
func live(m store.Mark, now time.Time) bool {
	if m.Class == store.ClassAuth || m.Until == nil {
		return true
	}
	return now.Before(*m.Until)
}

// reasonWords is how a reason reads in a failure text and on a page.
var reasonWords = map[string]string{
	store.ClassQuota:    "out of quota",
	store.ClassBilling:  "out of credit",
	store.ClassAuth:     "key rejected",
	ReasonTried:         "already tried",
	ReasonRemoved:       "removed",
	ReasonNotAllowed:    "teams may not use it",
	ReasonNotOwner:      "its team doesn't own this repository",
	ReasonKeyUnusable:   "no usable key",
	ReasonInstanceQuota: "instance token quota used up",
}

// ReasonWords is reason as words.
func ReasonWords(reason string) string {
	if w, ok := reasonWords[reason]; ok {
		return w
	}
	return reason
}

// FailureText is the failure reason of an exhausted decision: every entry
// and why it was skipped. Entry names can hold no markdown, so it is safe in
// the MR comment; it never holds a key.
func FailureText(d Decision) string {
	parts := make([]string, 0, len(d.Skips))
	for _, s := range d.Skips {
		name := s.Name
		if name == "" {
			name = "a removed model"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", name, ReasonWords(s.Reason)))
	}
	return "all models exhausted: " + strings.Join(parts, ", ")
}
