package dashboard

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// maxEntryName is the longest entry name, in characters.
	maxEntryName = 64

	msgBadModelName     = "name must be 1 to 64 characters: letters, digits, spaces and . _ : / + -, starting with a letter or digit"
	msgNameTaken        = "an entry with this name exists already"
	msgBadChainBody     = `the body must be {"entries": [{"source": "global" or "team", "id": ...}, ...]}`
	msgChainTooLong     = "the chain has more than 5 entries"
	msgEntryTwice       = "an entry is in the chain twice"
	msgUnknownEntry     = "every entry must be an entry of the list"
	msgGlobalOnly       = "the global chain holds global entries only"
	msgConfigFirst      = "the config file's entries stay at the front of the global chain, in their order"
	msgForeignTeamEntry = "a team's entry can be in its own team's chain and in the chains of repositories the team owns, nowhere else"
	msgNotTeamsMayUse   = "teams may not use this global entry: an admin can turn on teams may use"
)

// entryNamePattern is what an entry name may hold. A name reaches the MR
// comment after a fallback, so it can hold no markdown.
var entryNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:/+-]*$`)

// entryName checks an entry's name, trimmed; ok is false when it breaks the
// rule (msgBadModelName).
func entryName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	return name, n >= 1 && n <= maxEntryName && entryNamePattern.MatchString(name)
}

// chainLabel names a chain in a model's usedIn, a warning and the audit log.
func chainLabel(c store.Chain, teams map[int64]string) string {
	switch c.Scope {
	case store.ScopeGlobal:
		return "global chain"
	case store.ScopeTeam:
		return "team " + teams[c.TeamID]
	}
	return c.Instance + "/" + c.Project
}

// teamNames are the teams' names by ID.
func (d *Dashboard) teamNames(ctx context.Context) (map[int64]string, error) {
	teams, err := d.Store.Teams(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(teams))
	for _, t := range teams {
		out[t.ID] = t.Name
	}
	return out, nil
}

// usedIn labels, for each entry, the chains that name it, in chain order.
func (d *Dashboard) usedIn(ctx context.Context, teams map[int64]string) (map[store.EntryRef][]string, error) {
	chains, err := d.Store.Chains(ctx)
	if err != nil {
		return nil, err
	}
	out := map[store.EntryRef][]string{}
	for _, c := range chains {
		label := chainLabel(c, teams)
		for _, ref := range c.Entries {
			if l := out[ref]; len(l) == 0 || l[len(l)-1] != label {
				out[ref] = append(out[ref], label)
			}
		}
	}
	return out, nil
}

// marksByRef is every mark, by entry.
func (d *Dashboard) marksByRef(ctx context.Context) (map[store.EntryRef]store.Mark, error) {
	marks, err := d.Store.Marks(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[store.EntryRef]store.Mark, len(marks))
	for _, m := range marks {
		out[m.Ref] = m
	}
	return out, nil
}

// markView is m as a page shows it; nil for none.
func (d *Dashboard) markView(m store.Mark, ok bool) *MarkView {
	if !ok {
		return nil
	}
	live := m.Class == store.ClassAuth || m.Until == nil || d.now().Before(*m.Until)
	return &MarkView{Class: m.Class, Until: m.Until, SetAt: m.SetAt.UTC(), Live: live}
}

// stateOf is a chain.EntryReason as a ChainEntryView's state.
func stateOf(reason string) string {
	switch reason {
	case "":
		return "ready"
	case store.ClassQuota, store.ClassBilling:
		return "exhausted"
	case store.ClassAuth:
		return "key_rejected"
	}
	return reason
}

// shownChain is a chain to show: its scope and where its entries come from
// (ChainView's), the resolver's view of it (in: the entries known, the
// marks, the owner team and whether an admin saved it) and how many of its
// first entries are the config file's.
type shownChain struct {
	scope, source string
	team          string
	in            chain.Input
	fixed         int
}

// chainEntries reads the entries every chain view needs: every entry known
// now and the marks.
func (d *Dashboard) chainEntries(ctx context.Context) (chain.Input, error) {
	in := chain.Input{Entries: map[store.EntryRef]instances.Model{}, Now: d.now()}
	for _, m := range d.registry.AllModels() {
		in.Entries[chain.RefOf(m)] = m
	}
	marks, err := d.marksByRef(ctx)
	if err != nil {
		return chain.Input{}, err
	}
	in.Marks = marks
	return in, nil
}

// entryView is ref as a chain shows it, at position pos (0 for a choice).
// canClear says whether the session may clear a mark of it.
func entryView(in chain.Input, ref store.EntryRef, pos int, teams map[int64]string, canClear func(store.EntryRef) bool) ChainEntryView {
	v := ChainEntryView{Position: pos, Source: ref.Source, ID: ref.ID, State: stateOf(chain.EntryReason(in, ref))}
	if m, ok := in.Entries[ref]; ok {
		v.Name, v.Provider, v.Model = m.Name, m.LLM.Provider, m.LLM.Model
		if m.TeamID != 0 {
			v.Team = teams[m.TeamID]
		}
		v.Fixed = m.Source == instances.SourceConfig
	}
	if mk, ok := in.Marks[ref]; ok {
		v.Class, v.Until, v.CanClear = mk.Class, mk.Until, canClear(ref)
	}
	return v
}

// chainView is c as the session sees it. choices are the entries it may put
// in the chain; nil makes the view read-only.
func (d *Dashboard) chainView(c shownChain, choices []instances.Model, teams map[int64]string, canClear func(store.EntryRef) bool) ChainView {
	v := ChainView{Scope: c.scope, Source: c.source, Team: c.team, ReadOnly: choices == nil, SavedByAdmin: c.in.SavedByAdmin,
		Entries: make([]ChainEntryView, 0, len(c.in.Refs)), Choices: []ChainEntryView{}}
	for i, ref := range c.in.Refs {
		v.Entries = append(v.Entries, entryView(c.in, ref, i+1, teams, canClear))
	}
	for _, m := range choices {
		v.Choices = append(v.Choices, entryView(c.in, chain.RefOf(m), 0, teams, canClear))
	}
	return v
}

// adminClears lets an admin clear any mark.
func adminClears(store.EntryRef) bool { return true }

// entryInput is an entry of a chain's PUT body.
type entryInput struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}

// decodeChain reads a chain's PUT body; msg is the 400's message when it is
// wrong.
func decodeChain(r *http.Request) ([]store.EntryRef, string) {
	var in struct {
		Entries []entryInput `json:"entries"`
	}
	if err := decodeStrict(r, &in); err != nil || in.Entries == nil {
		return nil, msgBadChainBody
	}
	out := make([]store.EntryRef, 0, len(in.Entries))
	seen := map[store.EntryRef]bool{}
	for _, e := range in.Entries {
		if (e.Source != store.SourceGlobal && e.Source != store.SourceTeam) || e.ID == "" {
			return nil, msgBadChainBody
		}
		ref := store.EntryRef{Source: e.Source, ID: e.ID}
		if seen[ref] {
			return nil, msgEntryTwice
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out, ""
}

// checkEntries reports, as a 400's message, an entry of refs that is not one
// of choices: one no list has, a team's entry the chain may not hold, or a
// global entry teams may not use. known holds every entry there is.
func checkEntries(refs []store.EntryRef, choices []instances.Model, known map[store.EntryRef]instances.Model) string {
	if len(refs) > store.MaxChain {
		return msgChainTooLong
	}
	allowed := make(map[store.EntryRef]bool, len(choices))
	for _, m := range choices {
		allowed[chain.RefOf(m)] = true
	}
	for _, ref := range refs {
		switch _, ok := known[ref]; {
		case allowed[ref]:
		case !ok:
			return msgUnknownEntry
		case ref.Source == store.SourceTeam:
			return msgForeignTeamEntry
		default:
			return msgNotTeamsMayUse
		}
	}
	return ""
}

// entryNames are the names of refs, in order, for an audit entry: a removed
// entry reads "(removed)", never its ID.
func entryNames(refs []store.EntryRef, known map[store.EntryRef]instances.Model) string {
	if len(refs) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		if m, ok := known[ref]; ok {
			names = append(names, m.Name)
		} else {
			names = append(names, "(removed)")
		}
	}
	return strings.Join(names, ", ")
}

// saveChain stores c, or answers the error: 400 for a chain too long or a
// team entry removed meanwhile, 500 otherwise. ok is false when it answered.
func (d *Dashboard) saveChain(ctx context.Context, w http.ResponseWriter, c store.Chain) bool {
	err := d.Store.SaveChain(ctx, c, d.now())
	switch {
	case errors.Is(err, store.ErrChainTooLong):
		writeError(w, http.StatusBadRequest, msgChainTooLong)
	case errors.Is(err, store.ErrModelNotFound):
		writeError(w, http.StatusBadRequest, msgUnknownEntry)
	case err != nil:
		d.Log.Error("dashboard: saving a model chain failed", "scope", c.Scope, "error", d.registry.Redact(err.Error()))
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
	default:
		return true
	}
	return false
}

// --- the global chain (admins) ---

// globalShown is the global chain as it is written (chain.GlobalRefs).
func (d *Dashboard) globalShown(ctx context.Context) (shownChain, error) {
	in, err := d.chainEntries(ctx)
	if err != nil {
		return shownChain{}, err
	}
	refs, fixed, _, err := chain.GlobalRefs(ctx, d.Store, d.registry)
	if err != nil {
		return shownChain{}, err
	}
	in.Scope, in.Refs, in.SavedByAdmin = store.ScopeGlobal, refs, true
	return shownChain{scope: store.ScopeGlobal, source: store.ScopeGlobal, in: in, fixed: fixed}, nil
}

// globalChoices are the entries an admin may put in the global chain: the
// dashboard's (the config file's are always in front).
func (d *Dashboard) globalChoices() []instances.Model {
	out := []instances.Model{}
	for _, m := range d.registry.GlobalModels() {
		if m.Source != instances.SourceConfig {
			out = append(out, m)
		}
	}
	return out
}

func (d *Dashboard) writeGlobalChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := d.globalShown(ctx)
	if err != nil {
		d.failed(w, r, "reading the global chain", err)
		return
	}
	teams, err := d.teamNames(ctx)
	if err != nil {
		d.failed(w, r, "listing teams", err)
		return
	}
	writeJSON(w, http.StatusOK, d.chainView(c, d.globalChoices(), teams, adminClears))
}

// --- GET /api/admin/chain ---

func (d *Dashboard) getGlobalChain(w http.ResponseWriter, r *http.Request) { d.writeGlobalChain(w, r) }

// --- PUT /api/admin/chain ---

// putGlobalChain saves the global chain. The config file's entries stay in
// front: the body may start with them, in their order, or leave them out;
// anywhere else is 400. They count towards the 5 entries.
func (d *Dashboard) putGlobalChain(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	refs, msg := decodeChain(r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	cur, err := d.globalShown(ctx)
	if err != nil {
		d.failed(w, r, "reading the global chain", err)
		return
	}
	fixed := cur.in.Refs[:cur.fixed]
	if len(refs) >= len(fixed) && slices.Equal(refs[:len(fixed)], fixed) {
		refs = refs[len(fixed):]
	}
	for _, ref := range refs {
		if ref.Source == store.SourceTeam {
			writeError(w, http.StatusBadRequest, msgGlobalOnly)
			return
		}
		if m, ok := cur.in.Entries[ref]; ok && m.Source == instances.SourceConfig {
			writeError(w, http.StatusBadRequest, msgConfigFirst)
			return
		}
	}
	if len(fixed)+len(refs) > store.MaxChain {
		writeError(w, http.StatusBadRequest, msgChainTooLong)
		return
	}
	if msg := checkEntries(refs, d.globalChoices(), cur.in.Entries); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if !d.saveChain(ctx, w, store.Chain{Scope: store.ScopeGlobal, Entries: refs, SavedByAdmin: true}) {
		return
	}
	// Position 1 became the default, which the registry holds.
	d.reloadModels(ctx, "")
	detail := "global: " + entryNames(append(append([]store.EntryRef{}, fixed...), refs...), cur.in.Entries)
	d.Log.Info("dashboard: model chain changed", "chain", "global", "by", se.Username)
	d.audit(ctx, se, "chain_changed", "", "", detail)
	d.writeGlobalChain(w, r)
}

// --- DELETE /api/admin/models/{id}/mark ---

// clearGlobalMark clears a global entry's mark: 204 whether it had one or
// not (a change only is audited), 404 for an entry the list doesn't have.
func (d *Dashboard) clearGlobalMark(w http.ResponseWriter, r *http.Request) {
	se, id := session(r), r.PathValue("id")
	var name string
	for _, m := range d.registry.GlobalModels() {
		if m.ID == id {
			name = m.Name
		}
	}
	if name == "" {
		notFound(w)
		return
	}
	d.clearMark(w, r, se, store.EntryRef{Source: store.SourceGlobal, ID: id}, name, "", "")
}

// clearMark drops ref's mark and audits it when there was one.
func (d *Dashboard) clearMark(w http.ResponseWriter, r *http.Request, se Session, ref store.EntryRef, name, instance, project string) {
	ctx := r.Context()
	cleared, err := d.Store.ClearMark(ctx, ref)
	if err != nil {
		d.Log.Error("dashboard: clearing a mark failed", "source", ref.Source, "model_id", ref.ID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if cleared {
		d.Log.Info("dashboard: mark cleared", "source", ref.Source, "model_id", ref.ID, "by", se.Username)
		d.audit(ctx, se, "model_mark_cleared", instance, project, name)
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- a repository's chain ---

const (
	msgRepoChainWrite   = "only admins and maintainers of the repository's owner team change its model chain"
	msgRepoChainNoOwner = "only admins change the model chain of a repository with no owner team"
	msgEmptyRepoChain   = "a repository's chain needs an entry: reset it to inherit its owner team's or the global chain"
)

// repoShown is the chain in effect for repo, as the resolver reads it
// (chain.Load): its own, else its owner team's, else the global one, written
// out in full (chain.GlobalRefs) even with no usable key. owner is its owner
// team, 0 for none.
func (d *Dashboard) repoShown(ctx context.Context, repo settings.Effective) (c shownChain, owner int64, err error) {
	in, err := chain.Load(ctx, d.Store, d.registry, d.now(), store.Run{}, repo)
	if err != nil {
		return shownChain{}, 0, err
	}
	c = shownChain{scope: store.ScopeRepo, in: in}
	switch in.Scope {
	case store.ScopeRepo:
		c.source = "repository"
	case store.ScopeTeam:
		c.source = store.ScopeTeam
	default:
		c.source = store.ScopeGlobal
		if c.in.Refs, c.fixed, _, err = chain.GlobalRefs(ctx, d.Store, d.registry); err != nil {
			return shownChain{}, 0, err
		}
		c.in.SavedByAdmin = true
	}
	if in.OwnerTeam != 0 && in.Scope == store.ScopeTeam {
		teams, err := d.teamNames(ctx)
		if err != nil {
			return shownChain{}, 0, err
		}
		c.team = teams[in.OwnerTeam]
	}
	return c, in.OwnerTeam, nil
}

// maintainsTeam reports whether se is a maintainer of team.
func (d *Dashboard) maintainsTeam(ctx context.Context, se Session, team int64) (bool, error) {
	if team == 0 {
		return false, nil
	}
	_, role, _, err := d.teamAccess(ctx, se, team)
	return role >= RoleMaintainer, err
}

// repoChoices are the entries se may put in repo's chain, nil when it may
// not change it: an admin any global entry and the owner team's entries; a
// maintainer of the owner team that team's entries and the global entries
// teams may use (never the config file's). msg is the 403's message when nil.
func (d *Dashboard) repoChoices(ctx context.Context, se Session, owner int64) (choices []instances.Model, msg string, err error) {
	if se.Admin {
		choices = append([]instances.Model{}, d.registry.GlobalModels()...)
		if owner != 0 {
			choices = append(choices, d.registry.TeamModels(owner)...)
		}
		return choices, "", nil
	}
	if owner == 0 {
		return nil, msgRepoChainNoOwner, nil
	}
	ok, err := d.maintainsTeam(ctx, se, owner)
	if err != nil || !ok {
		return nil, msgRepoChainWrite, err
	}
	return d.teamChoices(owner), "", nil
}

// repoClears lets an admin clear any mark and a maintainer of the owner team
// those of the team's own entries.
func repoClears(se Session, owner int64, choices []instances.Model, in chain.Input) func(store.EntryRef) bool {
	if choices == nil {
		return func(store.EntryRef) bool { return false }
	}
	return teamClears(se.Admin, owner, in)
}

// writeRepoChain answers with repo's chain as se sees it.
func (d *Dashboard) writeRepoChain(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	ctx := r.Context()
	c, owner, err := d.repoShown(ctx, repo)
	if err != nil {
		d.failed(w, r, "reading the model chain", err)
		return
	}
	var choices []instances.Model
	if se.Role(repo.Instance, repo.Project) >= RoleMaintainer {
		if choices, _, err = d.repoChoices(ctx, se, owner); err != nil {
			d.failed(w, r, "reading the owner team", err)
			return
		}
	}
	teams, err := d.teamNames(ctx)
	if err != nil {
		d.failed(w, r, "listing teams", err)
		return
	}
	writeJSON(w, http.StatusOK, d.chainView(c, choices, teams, repoClears(se, owner, choices, c.in)))
}

// --- GET .../chain ---

func (d *Dashboard) repositoryChain(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	d.writeRepoChain(w, r, se, repo)
}

// repoChainTarget is the repository of a chain write, with the entries se
// may put in its chain: 404 when se doesn't see it, 403 for anyone but an
// admin or a maintainer of its owner team. ok is false when it answered.
func (d *Dashboard) repoChainTarget(w http.ResponseWriter, r *http.Request, suffix string) (settings.Effective, []instances.Model, bool) {
	ctx, se := r.Context(), session(r)
	repo, ok := d.targetAny(w, r, suffix, RoleViewer)
	if !ok {
		return settings.Effective{}, nil, false
	}
	owner, _, err := d.Store.OwnerTeam(ctx, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading the owner team", err)
		return settings.Effective{}, nil, false
	}
	choices, msg, err := d.repoChoices(ctx, se, owner)
	if err != nil {
		d.failed(w, r, "reading the owner team", err)
		return settings.Effective{}, nil, false
	}
	if choices == nil {
		writeError(w, http.StatusForbidden, msg)
		return settings.Effective{}, nil, false
	}
	return repo, choices, true
}

// --- PUT .../chain ---

// putRepoChain gives the repository its own chain, which replaces its owner
// team's for it alone. The chain records whether an admin saved it: one an
// admin saved may keep global entries teams may not use (owner ruling
// 2026-11-20); a maintainer's save may hold only what teams may use.
func (d *Dashboard) putRepoChain(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	repo, choices, ok := d.repoChainTarget(w, r, "/chain")
	if !ok {
		return
	}
	refs, msg := decodeChain(r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, msgEmptyRepoChain)
		return
	}
	in, err := d.chainEntries(ctx)
	if err != nil {
		d.failed(w, r, "reading the models", err)
		return
	}
	if msg := checkEntries(refs, choices, in.Entries); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if !d.saveChain(ctx, w, store.Chain{Scope: store.ScopeRepo, Instance: repo.Instance, Project: repo.Project,
		Entries: refs, SavedByAdmin: se.Admin}) {
		return
	}
	d.Log.Info("dashboard: model chain changed", "chain", "repository", "instance", repo.Instance, "project", repo.Project, "by", se.Username)
	d.audit(ctx, se, "chain_changed", repo.Instance, repo.Project, "repository: "+entryNames(refs, in.Entries))
	d.writeRepoChain(w, r, se, repo)
}

// --- POST .../chain/reset ---

// resetRepoChain drops the repository's own chain: it inherits again. Who
// may is who may save it. (DELETE on a repository's path is the admins'
// Remove, so the reset is a POST, as the settings' is.)
func (d *Dashboard) resetRepoChain(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	repo, _, ok := d.repoChainTarget(w, r, "/chain/reset")
	if !ok {
		return
	}
	if err := d.Store.ResetRepoChain(ctx, repo.Instance, repo.Project); err != nil {
		d.Log.Error("dashboard: resetting a model chain failed", "instance", repo.Instance, "project", repo.Project, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: model chain reset", "instance", repo.Instance, "project", repo.Project, "by", se.Username)
	d.audit(ctx, se, "chain_changed", repo.Instance, repo.Project, "repository: inherit")
	d.writeRepoChain(w, r, se, repo)
}

// chainModelLabel is "provider / model" of position 1 of the chain in
// effect for repo; "" when there is none or it was removed.
func (d *Dashboard) chainModelLabel(ctx context.Context, repo settings.Effective) (string, error) {
	c, _, err := d.repoShown(ctx, repo)
	if err != nil || len(c.in.Refs) == 0 {
		return "", err
	}
	if m, ok := c.in.Entries[c.in.Refs[0]]; ok {
		return modelLabel(m.LLM.Provider, m.LLM.Model), nil
	}
	return "", nil
}
