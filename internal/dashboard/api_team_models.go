package dashboard

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	msgTeamWrite        = "only maintainers of this team change its models and chain"
	msgBadTeamModelBody = `the body must be {"name": ..., "provider": ..., "model": ..., "baseURL": ..., "apiKey": ...}`
	msgBadTeamModelEdit = `the body must be {"name": ..., "baseURL": ..., "apiKey": ...}, each optional: an entry's provider and model never change`
)

// teamRoutes adds the user area's team API to mux: session routes, each
// handler checking what the session may do on the team (teamFor).
func (d *Dashboard) teamRoutes(mux *http.ServeMux) {
	route := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, d.Auth.Require(h)) }
	route("GET /api/me/teams", d.myTeams)
	route("GET /api/teams/{id}/models", d.teamModels)
	route("POST /api/teams/{id}/models", d.createTeamModel)
	route("PUT /api/teams/{id}/models/{model}", d.updateTeamModel)
	route("DELETE /api/teams/{id}/models/{model}", d.deleteTeamModel)
	route("DELETE /api/teams/{id}/models/{model}/mark", d.clearTeamMark)
	route("PUT /api/teams/{id}/models/{model}/limit", d.putTeamModelLimit)
	route("DELETE /api/teams/{id}/models/{model}/limit", d.deleteTeamModelLimit)
	route("GET /api/teams/{id}/limits", d.teamLimits)
	route("GET /api/teams/{id}/chain", d.getTeamChain)
	route("PUT /api/teams/{id}/chain", d.putTeamChain)
	route("GET /api/teams/{id}/panel-chain", d.getTeamPanelChain)
	route("PUT /api/teams/{id}/panel-chain", d.putTeamPanelChain)
}

// teamAccess is what se may do on team id: RoleAdmin for an admin, the
// member's role for a member, RoleNone otherwise; t is the team when the
// role is not RoleNone. maintainsOther reports whether se maintains some
// team: a write by a non-member who does is 403 (a maintainer of another
// team), anyone else's 404. Neither depends on whether team id exists, so a
// non-member learns nothing of it.
func (d *Dashboard) teamAccess(ctx context.Context, se Session, id int64) (t store.Team, role Role, maintainsOther bool, err error) {
	if se.Admin {
		t, found, err := d.Store.Team(ctx, id)
		if err != nil || !found {
			return store.Team{}, RoleNone, false, err
		}
		return t, RoleAdmin, false, nil
	}
	teams, err := d.Store.AccountTeams(ctx, se.AccountID)
	if err != nil {
		return store.Team{}, RoleNone, false, err
	}
	for _, team := range teams {
		r := roleByAccount(team.Members)[se.AccountID]
		if team.ID == id {
			t, role = team, roleOf(r)
		} else if r == store.RoleMaintainer {
			maintainsOther = true
		}
	}
	return t, role, maintainsOther, nil
}

// teamFor answers for the request's team {id}: 404 unless the session sees
// it, and for a write (need RoleMaintainer) 403 for a viewer of the team or
// a maintainer of another one. ok is false when it answered.
func (d *Dashboard) teamFor(w http.ResponseWriter, r *http.Request, need Role) (store.Team, Role, bool) {
	id, ok := pathID(r)
	if !ok {
		notFound(w)
		return store.Team{}, RoleNone, false
	}
	t, role, other, err := d.teamAccess(r.Context(), session(r), id)
	switch {
	case err != nil:
		d.failed(w, r, "reading the team", err)
	case role >= need:
		return t, role, true
	case need >= RoleMaintainer && (role == RoleViewer || other):
		writeError(w, http.StatusForbidden, msgTeamWrite)
	default:
		notFound(w)
	}
	return store.Team{}, RoleNone, false
}

// teamClears lets the session clear the marks of team's own entries: the
// caller has checked it maintains the team. An admin clears any.
func teamClears(admin bool, team int64, in chain.Input) func(store.EntryRef) bool {
	return func(ref store.EntryRef) bool {
		if admin {
			return true
		}
		m, ok := in.Entries[ref]
		return ref.Source == store.SourceTeam && ok && m.TeamID == team
	}
}

// --- GET /api/me/teams ---

// myTeams lists the session's teams, with its role on each: every team, as
// "admin", for an admin.
func (d *Dashboard) myTeams(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var teams []store.Team
	var err error
	if se.Admin {
		teams, err = d.Store.Teams(ctx)
	} else {
		teams, err = d.Store.AccountTeams(ctx, se.AccountID)
	}
	if err != nil {
		d.failed(w, r, "listing teams", err)
		return
	}
	out := make([]MyTeamView, 0, len(teams))
	for _, t := range teams {
		role := store.RoleAdmin
		if !se.Admin {
			role = roleByAccount(t.Members)[se.AccountID]
		}
		v := MyTeamView{ID: t.ID, Name: t.Name, Role: role, Repos: []TeamRepoView{}}
		for _, tr := range t.Repos {
			v.Repos = append(v.Repos, TeamRepoView{Instance: tr.Instance, Project: tr.Project, Owner: tr.Owner})
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// --- the team's own entries ---

// teamModelView is a team entry. Its base URL is shown to those who may
// change it (edit) only; its key is never shown.
func (d *Dashboard) teamModelView(m store.TeamModel, l modelLists, edit bool) TeamModelView {
	v := TeamModelView{ID: m.ID, Name: m.Name, Provider: m.Provider, Model: m.Model}
	if edit {
		v.BaseURL = m.BaseURL
	}
	if len(m.APIKeySealed) > 0 {
		if _, err := d.Auth.box.Open(m.APIKeySealed); err == nil {
			at := m.UpdatedAt
			v.Key = SecretView{Set: true, UpdatedAt: &at}
		}
	}
	ref := store.EntryRef{Source: store.SourceTeam, ID: m.ID}
	v.UsedIn = nonNilStrings(l.usedIn[ref])
	mk, ok := l.marks[ref]
	v.Mark = d.markView(mk, ok)
	return v
}

// teamModelLists is modelLists without the repositories, which team entries
// don't have.
func (d *Dashboard) teamModelLists(ctx context.Context) (modelLists, error) {
	teams, err := d.teamNames(ctx)
	if err != nil {
		return modelLists{}, err
	}
	var l modelLists
	if l.usedIn, err = d.usedIn(ctx, teams); err != nil {
		return modelLists{}, err
	}
	if l.marks, err = d.marksByRef(ctx); err != nil {
		return modelLists{}, err
	}
	return l, nil
}

// storedTeamModel finds entry id of team.
func (d *Dashboard) storedTeamModel(ctx context.Context, team int64, id string) (store.TeamModel, bool, error) {
	rows, err := d.Store.TeamModels(ctx, team)
	if err != nil {
		return store.TeamModel{}, false, err
	}
	for _, m := range rows {
		if m.ID == id {
			return m, true, nil
		}
	}
	return store.TeamModel{}, false, nil
}

// --- GET /api/teams/{id}/models ---

func (d *Dashboard) teamModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, role, ok := d.teamFor(w, r, RoleViewer)
	if !ok {
		return
	}
	rows, err := d.Store.TeamModels(ctx, t.ID)
	if err != nil {
		d.failed(w, r, "reading the team's models", err)
		return
	}
	l, err := d.teamModelLists(ctx)
	if err != nil {
		d.failed(w, r, "reading the team's models", err)
		return
	}
	edit := role >= RoleMaintainer
	v := TeamModelsView{Team: TeamRef{ID: t.ID, Name: t.Name}, Role: role.String(), CanEdit: edit, Models: []TeamModelView{}}
	for _, m := range rows {
		v.Models = append(v.Models, d.teamModelView(m, l, edit))
	}
	writeJSON(w, http.StatusOK, v)
}

// --- POST /api/teams/{id}/models ---

func (d *Dashboard) createTeamModel(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	t, _, ok := d.teamFor(w, r, RoleMaintainer)
	if !ok {
		return
	}
	var in struct {
		Name     *string `json:"name"`
		Provider string  `json:"provider"`
		Model    string  `json:"model"`
		BaseURL  string  `json:"baseURL"`
		APIKey   *string `json:"apiKey"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadTeamModelBody)
		return
	}
	if in.Name == nil {
		writeError(w, http.StatusBadRequest, msgBadModelName)
		return
	}
	name, ok := entryName(*in.Name)
	if !ok {
		writeError(w, http.StatusBadRequest, msgBadModelName)
		return
	}
	in.Provider, in.Model, in.BaseURL = strings.TrimSpace(in.Provider), strings.TrimSpace(in.Model), strings.TrimSpace(in.BaseURL)
	if in.Provider == "" || in.Model == "" || len(in.Provider) > maxLLMField || len(in.Model) > maxLLMField {
		writeError(w, http.StatusBadRequest, msgLLMRequired)
		return
	}
	if !checkBaseURL(in.BaseURL) {
		writeError(w, http.StatusBadRequest, msgBadBaseURL)
		return
	}
	if in.APIKey == nil {
		writeError(w, http.StatusBadRequest, msgKeyRequired)
		return
	}
	if strings.TrimSpace(*in.APIKey) == "" || len(*in.APIKey) > maxSecretBytes {
		writeError(w, http.StatusBadRequest, msgBadKey)
		return
	}
	m, err := d.Store.CreateTeamModel(ctx, store.TeamModel{TeamID: t.ID, Name: name, Provider: in.Provider, Model: in.Model,
		BaseURL: in.BaseURL, APIKeySealed: d.Auth.box.Seal([]byte(*in.APIKey))}, d.now())
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, msgNameTaken)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: adding a team model failed", "team", t.ID, "error", d.registry.Redact(redactKey(err.Error(), *in.APIKey)))
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	// The registry holds team keys too (for the Jobs and for redaction).
	d.reloadModels(ctx, *in.APIKey)
	detail := t.Name + ": " + modelDetail(m.Name, m.Provider, m.Model)
	d.Log.Info("dashboard: team model added", "team", t.ID, "model_id", m.ID, "by", se.Username)
	d.audit(ctx, se, "team_model_added", "", "", detail)
	writeJSON(w, http.StatusCreated, d.teamModelView(m, modelLists{}, true))
}

// --- PUT /api/teams/{id}/models/{model} ---

func (d *Dashboard) updateTeamModel(w http.ResponseWriter, r *http.Request) {
	ctx, se, id := r.Context(), session(r), r.PathValue("model")
	t, _, ok := d.teamFor(w, r, RoleMaintainer)
	if !ok {
		return
	}
	var in struct {
		Name    *string `json:"name"`
		BaseURL *string `json:"baseURL"`
		APIKey  *string `json:"apiKey"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadTeamModelEdit)
		return
	}
	var name, baseURL *string
	if in.Name != nil {
		n, ok := entryName(*in.Name)
		if !ok {
			writeError(w, http.StatusBadRequest, msgBadModelName)
			return
		}
		name = &n
	}
	if in.BaseURL != nil {
		u := strings.TrimSpace(*in.BaseURL)
		if !checkBaseURL(u) {
			writeError(w, http.StatusBadRequest, msgBadBaseURL)
			return
		}
		baseURL = &u
	}
	var sealed []byte
	hidden := ""
	if in.APIKey != nil {
		if strings.TrimSpace(*in.APIKey) == "" || len(*in.APIKey) > maxSecretBytes {
			writeError(w, http.StatusBadRequest, msgBadKey)
			return
		}
		sealed, hidden = d.Auth.box.Seal([]byte(*in.APIKey)), *in.APIKey
	}
	before, found, err := d.storedTeamModel(ctx, t.ID, id)
	if err != nil {
		d.failed(w, r, "reading the team's models", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	found, err = d.Store.UpdateTeamModel(ctx, t.ID, id, name, baseURL, sealed, d.now())
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, msgNameTaken)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: changing a team model failed", "team", t.ID, "model_id", id, "error", d.registry.Redact(redactKey(err.Error(), hidden)))
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !found {
		notFound(w) // removed meanwhile
		return
	}
	d.reloadModels(ctx, hidden)

	after := before
	var changed []string
	if name != nil && *name != before.Name {
		after.Name = *name
		changed = append(changed, "name")
	}
	if baseURL != nil && *baseURL != before.BaseURL {
		after.BaseURL = *baseURL
		changed = append(changed, "base_url")
	}
	if sealed != nil {
		after.APIKeySealed, after.UpdatedAt = sealed, d.now()
		changed = append(changed, "api_key")
	}
	if len(changed) > 0 {
		detail := t.Name + ": " + modelDetail(after.Name, after.Provider, after.Model) + " (" + strings.Join(changed, ", ") + ")"
		d.Log.Info("dashboard: team model updated", "team", t.ID, "model_id", id, "fields", changed, "by", se.Username)
		d.audit(ctx, se, "team_model_updated", "", "", detail)
	}
	l, err := d.teamModelLists(ctx)
	if err != nil {
		d.failed(w, r, "reading the team's models", err)
		return
	}
	writeJSON(w, http.StatusOK, d.teamModelView(after, l, true))
}

// --- DELETE /api/teams/{id}/models/{model} ---

// deleteTeamModel removes a team entry and takes it out of every chain that
// names it, in one transaction (the web asks first, listing usedIn); the
// audit entry names those chains.
func (d *Dashboard) deleteTeamModel(w http.ResponseWriter, r *http.Request) {
	ctx, se, id := r.Context(), session(r), r.PathValue("model")
	t, _, ok := d.teamFor(w, r, RoleMaintainer)
	if !ok {
		return
	}
	before, found, err := d.storedTeamModel(ctx, t.ID, id)
	if err != nil {
		d.failed(w, r, "reading the team's models", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	changed, changedR, err := d.Store.DeleteTeamModel(ctx, t.ID, id)
	if errors.Is(err, store.ErrModelNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: removing a team model failed", "team", t.ID, "model_id", id, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.reloadModels(ctx, "")
	detail := t.Name + ": " + modelDetail(before.Name, before.Provider, before.Model)
	if len(changed) > 0 || len(changedR) > 0 {
		teams, err := d.teamNames(ctx)
		if err != nil {
			d.Log.Error("dashboard: listing teams failed", "error", err.Error())
		}
		// A repository's reviewer chains share its label: each label once.
		labels := func(chains []store.Chain) string {
			out := make([]string, 0, len(chains))
			for _, c := range chains {
				if l := chainLabel(c, teams); !slices.Contains(out, l) {
					out = append(out, l)
				}
			}
			return strings.Join(out, ", ")
		}
		if len(changed) > 0 {
			detail += ", removed from " + labels(changed)
		}
		// Only when a reviewer chain named it, so the line is as before
		// otherwise.
		if len(changedR) > 0 {
			detail += ", removed from reviewer chains: " + labels(changedR)
		}
	}
	args := []any{"team", t.ID, "model_id", id, "chains", len(changed)}
	// Only when a reviewer chain changed, so the line is as before otherwise.
	if len(changedR) > 0 {
		args = append(args, "reviewer_chains", len(changedR))
	}
	d.Log.Info("dashboard: team model removed", append(args, "by", se.Username)...)
	d.audit(ctx, se, "team_model_removed", "", "", detail)
	w.WriteHeader(http.StatusNoContent)
}

// --- DELETE /api/teams/{id}/models/{model}/mark ---

// clearTeamMark clears the mark of one of the team's entries; a global
// entry's is the admins' (DELETE /api/admin/models/{id}/mark), so an ID that
// is not the team's is 404.
func (d *Dashboard) clearTeamMark(w http.ResponseWriter, r *http.Request) {
	ctx, se, id := r.Context(), session(r), r.PathValue("model")
	t, _, ok := d.teamFor(w, r, RoleMaintainer)
	if !ok {
		return
	}
	m, found, err := d.storedTeamModel(ctx, t.ID, id)
	if err != nil {
		d.failed(w, r, "reading the team's models", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	d.clearMark(w, r, se, store.EntryRef{Source: store.SourceTeam, ID: id}, t.Name+": "+m.Name, "", "")
}

// --- the team chain ---

// teamChoices are the entries a team chain may hold: the team's own and the
// global entries teams may use (never the config file's).
func (d *Dashboard) teamChoices(team int64) []instances.Model {
	out := d.registry.TeamModels(team)
	for _, m := range d.registry.GlobalModels() {
		if m.TeamsMayUse && m.Source != instances.SourceConfig {
			out = append(out, m)
		}
	}
	if out == nil {
		out = []instances.Model{}
	}
	return out
}

// teamShown is team t's chain of set, or, while it has none (or an empty
// one), the global chain of set its repositories use.
func (d *Dashboard) teamShown(ctx context.Context, t store.Team, set store.ChainSet) (shownChain, error) {
	c, found, err := d.Store.ChainOf(ctx, set, store.ScopeTeam, t.ID, "", "")
	if err != nil {
		return shownChain{}, err
	}
	if !found || len(c.Entries) == 0 {
		g, err := d.globalShown(ctx, set)
		g.scope = store.ScopeTeam
		return g, err
	}
	in, err := d.chainEntries(ctx)
	if err != nil {
		return shownChain{}, err
	}
	// The team's own repositories are what its chain runs for.
	in.Scope, in.Refs, in.OwnerTeam, in.SavedByAdmin = store.ScopeTeam, c.Entries, t.ID, c.SavedByAdmin
	return shownChain{scope: store.ScopeTeam, source: store.ScopeTeam, team: t.Name, in: in}, nil
}

func (d *Dashboard) writeTeamChain(w http.ResponseWriter, r *http.Request, se Session, t store.Team, role Role, set store.ChainSet) {
	ctx := r.Context()
	c, err := d.teamShown(ctx, t, set)
	if err != nil {
		d.failed(w, r, "reading the team's chain", err)
		return
	}
	teams, err := d.teamNames(ctx)
	if err != nil {
		d.failed(w, r, "listing teams", err)
		return
	}
	var choices []instances.Model
	clears := func(store.EntryRef) bool { return false }
	if role >= RoleMaintainer {
		choices, clears = d.teamChoices(t.ID), teamClears(se.Admin, t.ID, c.in)
	}
	writeJSON(w, http.StatusOK, d.chainView(c, choices, teams, clears))
}

// --- GET /api/teams/{id}/chain and .../panel-chain ---

func (d *Dashboard) getTeamChain(w http.ResponseWriter, r *http.Request) {
	d.getTeamChainOf(w, r, store.ChainA)
}

func (d *Dashboard) getTeamPanelChain(w http.ResponseWriter, r *http.Request) {
	d.getTeamChainOf(w, r, store.ChainB)
}

func (d *Dashboard) getTeamChainOf(w http.ResponseWriter, r *http.Request, set store.ChainSet) {
	t, role, ok := d.teamFor(w, r, RoleViewer)
	if !ok {
		return
	}
	d.writeTeamChain(w, r, session(r), t, role, set)
}

// --- PUT /api/teams/{id}/chain and .../panel-chain ---

func (d *Dashboard) putTeamChain(w http.ResponseWriter, r *http.Request) {
	d.putTeamChainOf(w, r, store.ChainA)
}

func (d *Dashboard) putTeamPanelChain(w http.ResponseWriter, r *http.Request) {
	d.putTeamChainOf(w, r, store.ChainB)
}

// putTeamChainOf saves the team's chain of set: its own entries and the
// global entries teams may use, at most 5; another team's entry, or a global
// one teams may not use, is 400, for an admin too (the resolver would skip
// it). No entries is no chain: the team's repositories use the global one.
func (d *Dashboard) putTeamChainOf(w http.ResponseWriter, r *http.Request, set store.ChainSet) {
	ctx, se := r.Context(), session(r)
	t, role, ok := d.teamFor(w, r, RoleMaintainer)
	if !ok {
		return
	}
	refs, msg := decodeChain(r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	in, err := d.chainEntries(ctx)
	if err != nil {
		d.failed(w, r, "reading the models", err)
		return
	}
	if msg := checkEntries(refs, d.teamChoices(t.ID), in.Entries); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if !d.saveChain(ctx, w, set, store.Chain{Scope: store.ScopeTeam, TeamID: t.ID, Entries: refs, SavedByAdmin: se.Admin}) {
		return
	}
	detail := "team " + t.Name + ": " + entryNames(refs, in.Entries)
	if set == store.ChainB {
		detail = "scope=team team=" + t.Name + " entries=" + entryNames(refs, in.Entries)
	}
	d.Log.Info(chainLog(set), "chain", "team", "team", t.ID, "by", se.Username)
	d.audit(ctx, se, chainAudit(set), "", "", detail)
	d.writeTeamChain(w, r, se, t, role, set)
}
