// Package chain picks the entry a review attempt runs on, from the chain in
// effect for its repository. Resolve reads nothing and writes nothing: Load
// reads the chain, the entries, the marks and what the review already tried,
// and the caller acts on the Decision.
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
	ReasonSameModel     = "same_model"     // the model Avoid names: a panel's member A runs on it
	ReasonModelLimit    = "model_limit"    // a global entry whose model limit is used up this month
	ReasonTeamLimit     = "team_limit"     // the owner team's limit on the entry is used up this month
)

// SameModel is a provider and a model. Two entries with the same ones are one
// model, whatever their keys, and the case does not count.
type SameModel struct{ Provider, Model string }

// is reports whether m runs on this provider and model. The zero SameModel
// is no model.
func (s SameModel) is(m instances.Model) bool {
	return s != SameModel{} && strings.EqualFold(s.Provider, m.LLM.Provider) && strings.EqualFold(s.Model, m.LLM.Model)
}

// Input is everything Resolve reads. Entries holds every entry known now,
// with its key ("" when it has none or it can't be opened); a ref missing
// from it is a removed entry. OwnerTeam is the team that owns the repository,
// 0 for none. Tried is the entries earlier attempts of this review ran.
// SavedByAdmin is whether an admin saved the chain (store.Chain's). Avoid
// is a model no entry may run on (a panel's member B avoids member A's); the
// zero value avoids none. ModelLimitUsedUp and TeamLimitUsedUp are the
// entries whose model limit, or whose limit for OwnerTeam, is used up this
// month; LoadSet reads them.
type Input struct {
	Scope               string // store.ScopeGlobal, ScopeTeam or ScopeRepo
	Refs                []store.EntryRef
	SavedByAdmin        bool
	Entries             map[store.EntryRef]instances.Model
	OwnerTeam           int64
	Marks               map[store.EntryRef]store.Mark
	Tried               map[store.EntryRef]bool
	InstanceQuotaUsedUp bool
	ModelLimitUsedUp    map[store.EntryRef]bool
	TeamLimitUsedUp     map[store.EntryRef]bool
	Avoid               SameModel
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
// is empty: wait as with no model), Hold (only quotas and limits stopped the
// entries it has not tried: hold on them; Limited is set when a model or team
// limit is one of them) and Exhausted (fail) is set. Length is the chain's.
type Decision struct {
	Entry     instances.Model
	Ref       store.EntryRef
	Position  int
	Length    int
	FirstSkip string
	Skips     []Skip
	Wait      bool
	Hold      bool
	Limited   bool
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
	d.Hold, d.Limited = holds(d.Skips)
	d.Exhausted = !d.Hold
	return d
}

// holds reports whether skips hold the review rather than fail it: a quota or
// a limit, which frees up, stopped at least one entry, and every other entry
// is one this review already tried. A mark, a bad key or any other reason
// fails it. limited is whether a model or team limit is among them.
func holds(skips []Skip) (hold, limited bool) {
	for _, s := range skips {
		switch s.Reason {
		case ReasonModelLimit, ReasonTeamLimit:
			hold, limited = true, true
		case ReasonInstanceQuota:
			hold = true
		case ReasonTried:
		default:
			return false, false
		}
	}
	return hold, hold && limited
}

// skipReason is why ref is passed over, "" when it runs. A live mark comes
// before tried, so the entry an earlier attempt fell back from reads as its
// class (the footer says why); the instance quota and the limits come last, so a hold
// waits only for entries that could run once it frees up. A limit stops a
// 1-entry chain too: marks come from provider errors, limits are a budget.
func skipReason(in Input, ref store.EntryRef, m instances.Model, known bool) string {
	switch {
	case !known:
		return ReasonRemoved
	case ref.Source == store.SourceGlobal && !m.TeamsMayUse && teamSaved(in):
		return ReasonNotAllowed
	case ref.Source == store.SourceTeam && (in.OwnerTeam == 0 || m.TeamID != in.OwnerTeam):
		return ReasonNotOwner
	case in.Avoid.is(m):
		return ReasonSameModel
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
	case ref.Source == store.SourceGlobal && in.ModelLimitUsedUp[ref]:
		return ReasonModelLimit
	case in.TeamLimitUsedUp[ref]:
		return ReasonTeamLimit
	}
	return ""
}

// EntryReason is why the entry at ref would be skipped in the chain of in
// (Resolve's rules), "" when it would run: the state a page shows for it.
func EntryReason(in Input, ref store.EntryRef) string {
	m, known := in.Entries[ref]
	return skipReason(in, ref, m, known)
}

// PastAdminLimits drops what an admin's start goes past: the instance quota,
// the model limits and the team limits on global entries. A team's own limit
// on its team entry stays: that is the team's money. The maps of the Input it
// was copied from are not changed.
func (in *Input) PastAdminLimits() {
	in.InstanceQuotaUsedUp, in.ModelLimitUsedUp = false, nil
	var own map[store.EntryRef]bool
	for ref := range in.TeamLimitUsedUp {
		if ref.Source == store.SourceTeam {
			if own == nil {
				own = map[store.EntryRef]bool{}
			}
			own[ref] = true
		}
	}
	in.TeamLimitUsedUp = own
}

// IgnoreLimits drops every model and team limit: Job P's judge ends a review
// that has started, as a running review finishes after a quota runs out.
func (in *Input) IgnoreLimits() { in.ModelLimitUsedUp, in.TeamLimitUsedUp = nil, nil }

// teamSaved reports whether the chain is held to what teams may use: a team
// chain, or the chain of a repository a team owns that a team's maintainer
// saved. The global chain, a repository chain with no owner (only an admin
// saves either) and a repository chain an admin saved may name any global
// entry (owner ruling 2026-11-20).
func teamSaved(in Input) bool {
	return in.Scope == store.ScopeTeam || (in.Scope == store.ScopeRepo && in.OwnerTeam != 0 && !in.SavedByAdmin)
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
	ReasonSameModel:     "the model the other review runs on",
	ReasonModelLimit:    "model limit used up",
	ReasonTeamLimit:     "team limit used up",
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
