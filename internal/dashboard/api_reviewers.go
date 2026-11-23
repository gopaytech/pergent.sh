package dashboard

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// A repository's reviewers 2 to 5 and its judge (the Reviewers card).
// Reviewer 1 is the model chain in effect (GET .../chain). The rights are
// chain A's on the repository: admins and maintainers of the owner team
// write, anyone who sees the repository reads.

const (
	msgBadReviewersBody      = `the body must be {"reviewers": [[{"source": "global" or "team", "id": ...}, ...], ...], "judge": [{"source": ..., "id": ...}, ...]}`
	msgTooManyReviewers      = "at most 5 reviewers"
	msgEmptyReviewer         = "a reviewer needs an entry; remove it instead"
	msgNoJudge               = "2 or more reviewers need a judge"
	msgJudgeWithoutReviewers = "a judge needs 2 or more reviewers"
	msgNoReviewerImage       = "no review image supports multi review yet"
	msgWarnNoJudge           = "No judge: findings are merged without checking"
)

// decodeReviewers reads PUT .../reviewers' body: extra are reviewers 2.. in
// order, judge may be left out or null for none. msg is the 400's message
// when it is wrong.
func decodeReviewers(r *http.Request) (extra [][]store.EntryRef, judge []store.EntryRef, msg string) {
	var in struct {
		Reviewers [][]entryInput `json:"reviewers"`
		Judge     []entryInput   `json:"judge"`
	}
	if err := decodeStrict(r, &in); err != nil || in.Reviewers == nil {
		return nil, nil, msgBadReviewersBody
	}
	extra = make([][]store.EntryRef, 0, len(in.Reviewers))
	for _, rv := range in.Reviewers {
		if rv == nil {
			return nil, nil, msgBadReviewersBody
		}
		refs, msg := refsOf(rv, msgBadReviewersBody)
		if msg != "" {
			return nil, nil, msg
		}
		extra = append(extra, refs)
	}
	if judge, msg = refsOf(in.Judge, msgBadReviewersBody); msg != "" {
		return nil, nil, msg
	}
	return extra, judge, ""
}

// reviewersShape reports, as a 400's message, a save with too many
// reviewers, an empty one, a judge missing or with no reviewer to judge, or
// a chain too long.
func reviewersShape(extra [][]store.EntryRef, judge []store.EntryRef) string {
	switch {
	case len(extra) > store.MaxReviewers-1:
		return msgTooManyReviewers
	case slices.ContainsFunc(extra, func(e []store.EntryRef) bool { return len(e) == 0 }):
		return msgEmptyReviewer
	case len(extra) > 0 && len(judge) == 0:
		return msgNoJudge
	case len(extra) == 0 && len(judge) > 0:
		return msgJudgeWithoutReviewers
	case len(judge) > store.MaxChain || slices.ContainsFunc(extra, func(e []store.EntryRef) bool { return len(e) > store.MaxChain }):
		return msgChainTooLong
	}
	return ""
}

// firstModels is the model each reviewer starts with, reviewer 1 (one, the
// chain in effect) first, as provider and model in lower case; "" for an
// empty chain or a removed first entry, which is compared with nothing.
func firstModels(one chain.Input, extra [][]store.EntryRef) []string {
	first := func(refs []store.EntryRef) string {
		if len(refs) == 0 {
			return ""
		}
		m, ok := one.Entries[refs[0]]
		if !ok {
			return ""
		}
		return strings.ToLower(m.LLM.Provider) + "\x00" + strings.ToLower(m.LLM.Model)
	}
	out := []string{first(one.Refs)}
	for _, e := range extra {
		out = append(out, first(e))
	}
	return out
}

// sameModel is reviewer k, who starts with the model of reviewer j (j < k).
type sameModel struct{ k, j int }

// sameFirstModels are the reviewers that start with the model of an earlier
// one, each with the first such earlier reviewer for which counts(k, j) is
// true. firsts is firstModels'; k and j count from 1.
func sameFirstModels(firsts []string, counts func(k, j int) bool) []sameModel {
	var out []sameModel
	for k := 1; k < len(firsts); k++ {
		if firsts[k] == "" {
			continue
		}
		for j := range k {
			if firsts[j] == firsts[k] && counts(k+1, j+1) {
				out = append(out, sameModel{k: k + 1, j: j + 1})
				break
			}
		}
	}
	return out
}

// anyPair counts every pair: the warnings name each one.
func anyPair(int, int) bool { return true }

// reviewersAudit is the audit detail of each slot whose entries the save
// changed, slot by slot: a reviewer moved down by a remove changes the slot
// it leaves and the slot it takes. A slot that is gone reads "(none)".
func reviewersAudit(before store.RepoReviewers, extra [][]store.EntryRef, judge []store.EntryRef, known map[store.EntryRef]instances.Model) []string {
	var out []string
	for i := range max(len(before.Extra), len(extra)) {
		var was, now []store.EntryRef
		if i < len(before.Extra) {
			was = before.Extra[i].Entries
		}
		if i < len(extra) {
			now = extra[i]
		}
		if !slices.Equal(was, now) {
			out = append(out, "reviewer="+strconv.Itoa(i+2)+" entries="+entryNames(now, known))
		}
	}
	if !slices.Equal(before.Judge.Entries, judge) {
		out = append(out, "judge entries="+entryNames(judge, known))
	}
	return out
}

// reviewersView is repo's reviewers 2 to 5 and judge as se sees them, read
// as a split reads them (chain.LoadReviewers). The store keeps a judge row
// exactly while there are reviewers 2 to 5, so the judge is shown with them
// and null without.
func (d *Dashboard) reviewersView(ctx context.Context, se Session, repo settings.Effective) (ReviewersView, error) {
	rs, err := chain.LoadReviewers(ctx, d.Store, d.registry, d.now(), store.Run{}, repo)
	if err != nil {
		return ReviewersView{}, err
	}
	owner := rs.One.OwnerTeam
	var choices []instances.Model
	if se.Role(repo.Instance, repo.Project) >= RoleMaintainer {
		if choices, _, err = d.repoChoices(ctx, se, owner); err != nil {
			return ReviewersView{}, err
		}
	}
	teams, err := d.teamNames(ctx)
	if err != nil {
		return ReviewersView{}, err
	}
	clears := repoClears(se, owner, choices, rs.One)
	show := func(in chain.Input) ChainView {
		return d.chainView(shownChain{scope: store.ScopeRepo, source: "repository", in: in}, choices, teams, clears)
	}
	v := ReviewersView{ReadOnly: choices == nil, Reviewers: []ReviewerChain{}, Warnings: []string{}}
	extra := make([][]store.EntryRef, 0, len(rs.Extra))
	for i, in := range rs.Extra {
		v.Reviewers = append(v.Reviewers, ReviewerChain{Number: i + 2, Chain: show(in)})
		extra = append(extra, in.Refs)
	}
	if len(extra) > 0 {
		j := show(rs.Judge)
		v.Judge = &j
		if len(rs.Judge.Refs) == 0 {
			v.Warnings = append(v.Warnings, msgWarnNoJudge)
		}
	}
	for _, p := range sameFirstModels(firstModels(rs.One, extra), anyPair) {
		v.Warnings = append(v.Warnings, "Reviewer "+strconv.Itoa(p.k)+" starts with the same model as reviewer "+strconv.Itoa(p.j)+", so it runs on its next entry")
	}
	if v.AddRefusal = d.reviewerImageRefusal(); v.AddRefusal == "" && len(extra) >= store.MaxReviewers-1 {
		v.AddRefusal = msgTooManyReviewers
	}
	return v, nil
}

func (d *Dashboard) writeReviewers(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	v, err := d.reviewersView(r.Context(), se, repo)
	if err != nil {
		d.failed(w, r, "reading the reviewers", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// --- GET .../reviewers ---

func (d *Dashboard) repositoryReviewers(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	d.writeReviewers(w, r, se, repo)
}

// --- PUT .../reviewers ---

// putReviewers replaces the repository's reviewers 2 to 5 and its judge in
// one save. The checks, in order: the rights (as PUT .../chain), the body,
// the counts and empty chains, the entries of each changed chain (a chain
// that a remove moved down with the same entries is not changed: it keeps
// who saved it, store.RepoReviewers.Unchanged), no 2 reviewers starting
// with the same model when the save changed one of them (reviewer 1 is
// never changed here; a pair the save left alone only warns), and the
// review image when the save adds a reviewer. It reads reviewer 1 with
// chain.Load and the stored slots with Store.Reviewers, not with
// chain.LoadReviewers: Unchanged needs the stored chains themselves.
func (d *Dashboard) putReviewers(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	repo, choices, ok := d.repoChainTarget(w, r, "/reviewers")
	if !ok {
		return
	}
	extra, judge, msg := decodeReviewers(r)
	if msg == "" {
		msg = reviewersShape(extra, judge)
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	one, err := chain.Load(ctx, d.Store, d.registry, d.now(), store.Run{}, repo)
	if err != nil {
		d.failed(w, r, "reading the model chain", err)
		return
	}
	stored, err := d.Store.Reviewers(ctx, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading the reviewers", err)
		return
	}
	// changed[k-1] is whether the save changed reviewer k.
	changed := make([]bool, 1+len(extra))
	for i, e := range extra {
		if _, same := stored.Unchanged(extra, i); same {
			continue
		}
		changed[i+1] = true
		if msg := checkEntries(e, choices, one.Entries); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	if !stored.JudgeUnchanged(judge) {
		if msg := checkEntries(judge, choices, one.Entries); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	byThisSave := func(k, j int) bool { return changed[k-1] || changed[j-1] }
	if same := sameFirstModels(firstModels(one, extra), byThisSave); len(same) > 0 {
		writeError(w, http.StatusBadRequest, "reviewer "+strconv.Itoa(same[0].k)+" starts with the same model as reviewer "+strconv.Itoa(same[0].j))
		return
	}
	if len(extra) > len(stored.Extra) {
		if msg := d.reviewerImageRefusal(); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	before, err := d.Store.SaveReviewers(ctx, store.ReviewersSave{Instance: repo.Instance, Project: repo.Project,
		Extra: extra, Judge: judge, SavedByAdmin: se.Admin}, d.now())
	switch {
	case errors.Is(err, store.ErrChainTooLong):
		writeError(w, http.StatusBadRequest, msgChainTooLong)
		return
	case errors.Is(err, store.ErrModelNotFound):
		writeError(w, http.StatusBadRequest, msgUnknownEntry)
		return
	case err != nil:
		d.Log.Error("dashboard: saving the reviewers failed", "instance", repo.Instance, "project", repo.Project, "error", d.registry.Redact(err.Error()))
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: reviewers changed", "instance", repo.Instance, "project", repo.Project, "reviewers", 1+len(extra), "by", se.Username)
	for _, detail := range reviewersAudit(before, extra, judge, one.Entries) {
		d.audit(ctx, se, "reviewers_changed", repo.Instance, repo.Project, detail)
	}
	d.writeReviewers(w, r, se, repo)
}
