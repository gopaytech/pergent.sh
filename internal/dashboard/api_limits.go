package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/quota"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	msgBadLimit       = "monthlyTokens must be a whole number from 1 to 1000000000000000"
	msgNotTeamModel   = "the model is not one of this team's models"
	msgAdminLimit     = "only admins set a team's limit on a global model"
	msgTeamLimitsRead = "only maintainers of this team see its model limits"
)

// globalEntry is the global entry id, the config file's included (config,
// config-1 to config-4).
func (d *Dashboard) globalEntry(id string) (instances.Model, bool) {
	for _, m := range d.registry.GlobalModels() {
		if m.ID == id {
			return m, true
		}
	}
	return instances.Model{}, false
}

// limitBody reads {"monthlyTokens": n} (quotaValue: a whole number from 1 to
// config.MaxMonthlyTokenQuota). On anything else it has answered 400.
func limitBody(w http.ResponseWriter, r *http.Request) (int64, bool) {
	var in struct {
		MonthlyTokens json.RawMessage `json:"monthlyTokens"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadLimit)
		return 0, false
	}
	n, ok := quotaValue(in.MonthlyTokens)
	if !ok || n == nil {
		writeError(w, http.StatusBadRequest, msgBadLimit)
		return 0, false
	}
	return *n, true
}

// limitDetail is how a limit is audited: the entry's name, the team's name
// ("" for the model limit: all teams) and the number. Never a key.
func limitDetail(entry, team string, n int64) string {
	if team == "" {
		team = "all teams"
	}
	return fmt.Sprintf("%s for %s: monthly_tokens=%d", entry, team, n)
}

// setLimit stores l, answers 204 and audits it.
func (d *Dashboard) setLimit(w http.ResponseWriter, r *http.Request, l store.ModelLimit, entry, team string) {
	ctx, se := r.Context(), session(r)
	l.UpdatedAt, l.UpdatedBy = d.now(), actorLabel(se.Kind, se.Username)
	if err := d.Store.SetModelLimit(ctx, l); err != nil {
		d.Log.Error("dashboard: setting a model limit failed", "source", l.Ref.Source, "model_id", l.Ref.ID, "team", l.TeamID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.audit(ctx, se, "model_limit_set", "", "", limitDetail(entry, team, l.MonthlyTokens))
	w.WriteHeader(http.StatusNoContent)
}

// clearLimit removes the limit of ref for team (0: the model limit): 204
// whether there was one or not; one removed is audited with its number.
func (d *Dashboard) clearLimit(w http.ResponseWriter, r *http.Request, ref store.EntryRef, teamID int64, entry, team string) {
	ctx, se := r.Context(), session(r)
	old, found, err := d.Store.ClearModelLimit(ctx, ref, teamID)
	if err != nil {
		d.Log.Error("dashboard: clearing a model limit failed", "source", ref.Source, "model_id", ref.ID, "team", teamID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if found {
		d.audit(ctx, se, "model_limit_cleared", "", "", limitDetail(entry, team, old))
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- PUT and DELETE /api/admin/models/{id}/limit ---

func (d *Dashboard) putModelLimit(w http.ResponseWriter, r *http.Request) {
	m, ok := d.globalEntry(r.PathValue("id"))
	if !ok {
		notFound(w)
		return
	}
	n, ok := limitBody(w, r)
	if !ok {
		return
	}
	d.setLimit(w, r, store.ModelLimit{Ref: chain.RefOf(m), MonthlyTokens: n}, m.Name, "")
}

func (d *Dashboard) deleteModelLimit(w http.ResponseWriter, r *http.Request) {
	m, ok := d.globalEntry(r.PathValue("id"))
	if !ok {
		notFound(w)
		return
	}
	d.clearLimit(w, r, chain.RefOf(m), 0, m.Name, "")
}

// --- PUT and DELETE /api/admin/models/{id}/team-limits/{teamId} ---

// teamLimitTarget is the global entry {id} and the team {teamId}; ok is false
// when it has answered (404 for either unknown).
func (d *Dashboard) teamLimitTarget(w http.ResponseWriter, r *http.Request) (instances.Model, store.Team, bool) {
	m, ok := d.globalEntry(r.PathValue("id"))
	teamID, err := strconv.ParseInt(r.PathValue("teamId"), 10, 64)
	if !ok || err != nil || teamID <= 0 {
		notFound(w)
		return instances.Model{}, store.Team{}, false
	}
	t, found, err := d.Store.Team(r.Context(), teamID)
	if err != nil {
		d.failed(w, r, "reading the team", err)
		return instances.Model{}, store.Team{}, false
	}
	if !found {
		notFound(w)
		return instances.Model{}, store.Team{}, false
	}
	return m, t, true
}

func (d *Dashboard) putTeamLimit(w http.ResponseWriter, r *http.Request) {
	m, t, ok := d.teamLimitTarget(w, r)
	if !ok {
		return
	}
	n, ok := limitBody(w, r)
	if !ok {
		return
	}
	d.setLimit(w, r, store.ModelLimit{Ref: chain.RefOf(m), TeamID: t.ID, MonthlyTokens: n}, m.Name, t.Name)
}

func (d *Dashboard) deleteTeamLimit(w http.ResponseWriter, r *http.Request) {
	m, t, ok := d.teamLimitTarget(w, r)
	if !ok {
		return
	}
	d.clearLimit(w, r, chain.RefOf(m), t.ID, m.Name, t.Name)
}

// The state of a global entry's model limit as a team page shows it.
const (
	limitNone     = "none"
	limitRoomLeft = "room_left"
	limitUsedUp   = "used_up"
)

// limitOf is the limit of ref for team (0: the model limit) among limits, nil
// for none.
func limitOf(limits []store.ModelLimit, ref store.EntryRef, team int64) *int64 {
	for _, l := range limits {
		if l.Ref == ref && l.TeamID == team {
			n := l.MonthlyTokens
			return &n
		}
	}
	return nil
}

// --- PUT and DELETE /api/teams/{id}/models/{model}/limit ---

// teamModelTarget is the team {id} and its own entry {model}, for a
// maintainer of the team or an admin (teamFor). A global entry is the
// admins' (403 to anyone else); any other ID is not one of the team's
// entries (400). ok is false when it has answered.
func (d *Dashboard) teamModelTarget(w http.ResponseWriter, r *http.Request) (store.Team, instances.Model, bool) {
	t, role, ok := d.teamFor(w, r, RoleMaintainer)
	if !ok {
		return store.Team{}, instances.Model{}, false
	}
	id := r.PathValue("model")
	for _, m := range d.registry.TeamModels(t.ID) {
		if m.ID == id {
			return t, m, true
		}
	}
	if _, global := d.globalEntry(id); global && role < RoleAdmin {
		writeError(w, http.StatusForbidden, msgAdminLimit)
	} else {
		writeError(w, http.StatusBadRequest, msgNotTeamModel)
	}
	return store.Team{}, instances.Model{}, false
}

func (d *Dashboard) putTeamModelLimit(w http.ResponseWriter, r *http.Request) {
	t, m, ok := d.teamModelTarget(w, r)
	if !ok {
		return
	}
	n, ok := limitBody(w, r)
	if !ok {
		return
	}
	d.setLimit(w, r, store.ModelLimit{Ref: chain.RefOf(m), TeamID: t.ID, MonthlyTokens: n}, m.Name, t.Name)
}

func (d *Dashboard) deleteTeamModelLimit(w http.ResponseWriter, r *http.Request) {
	t, m, ok := d.teamModelTarget(w, r)
	if !ok {
		return
	}
	d.clearLimit(w, r, chain.RefOf(m), t.ID, m.Name, t.Name)
}

// --- GET /api/teams/{id}/limits ---

// teamLimits is the team's limits (TeamLimitsView): 404 for a non-member, 403
// for a viewer.
func (d *Dashboard) teamLimits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, role, ok := d.teamFor(w, r, RoleViewer)
	if !ok {
		return
	}
	if role < RoleMaintainer {
		writeError(w, http.StatusForbidden, msgTeamLimitsRead)
		return
	}
	limits, err := d.Store.ModelLimits(ctx)
	if err != nil {
		d.failed(w, r, "reading the model limits", err)
		return
	}
	usage, err := quota.ReadEntryUsage(ctx, d.Store, d.now())
	if err != nil {
		d.failed(w, r, "reading the token usage", err)
		return
	}
	v := TeamLimitsView{Team: TeamRef{ID: t.ID, Name: t.Name}, Models: []TeamModelLimitView{}, Global: []TeamGlobalLimitView{}}
	for _, m := range d.registry.TeamModels(t.ID) {
		ref := chain.RefOf(m)
		v.Models = append(v.Models, TeamModelLimitView{ID: m.ID, Name: m.Name, Limit: limitOf(limits, ref, t.ID), Used: usage.Used(ref, t.ID)})
	}
	for _, m := range d.registry.GlobalModels() {
		ref := chain.RefOf(m)
		team := limitOf(limits, ref, t.ID)
		if team == nil {
			continue
		}
		state := limitNone
		if ml := limitOf(limits, ref, 0); ml != nil {
			state = limitRoomLeft
			if usage.UsedUp(store.ModelLimit{Ref: ref, MonthlyTokens: *ml}) {
				state = limitUsedUp
			}
		}
		v.Global = append(v.Global, TeamGlobalLimitView{ID: m.ID, Name: m.Name, Limit: *team, Used: usage.Used(ref, t.ID), ModelLimit: state})
	}
	writeJSON(w, http.StatusOK, v)
}
