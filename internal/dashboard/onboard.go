package dashboard

import (
	"context"
	"net/http"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// The states of a repository in the picker.
const (
	stateOff    = "off"
	statePaused = "paused"
	stateOn     = "on"
)

const (
	msgTeamsMayNotOnboard = "teams may not onboard repositories on this instance"
	msgUseClaim           = "the repository is already switched on: use Claim"
	msgNotOwnerMaintainer = "only maintainers of the owner team and admins can do this"
)

// teamRoute registers an API route admins and maintainers use: a session
// route (Auth.Require), recorded in teamRoutes so a test can tie it to the
// spec. The handler gates a non-admin itself (maintainerGate, ownerMaintainer):
// a user on no team gets the waiting gate's 403 before it runs.
func (d *Dashboard) teamRoute(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	d.teamRoutes = append(d.teamRoutes, pattern)
	mux.Handle(pattern, d.Auth.Require(h))
}

// maintainedTeams returns the teams on which se is a maintainer. An admin
// maintains none: callers check se.Admin first.
func (d *Dashboard) maintainedTeams(ctx context.Context, se Session) ([]store.Team, error) {
	teams, err := d.Store.AccountTeams(ctx, se.AccountID)
	if err != nil {
		return nil, err
	}
	var out []store.Team
	for _, t := range teams {
		if roleByAccount(t.Members)[se.AccountID] == store.RoleMaintainer {
			out = append(out, t)
		}
	}
	return out, nil
}

// teamsMayOnboard reports the instance's switch: false for an instance that
// is unknown or being removed.
func (d *Dashboard) teamsMayOnboard(instance string) bool {
	inst, ok := d.registry.Get(instance)
	return ok && !inst.Removing && inst.TeamsMayOnboard
}

// ownerMaintainer reports whether project's owner team is one se maintains;
// owner is that team (0 when the repository has none).
func (d *Dashboard) ownerMaintainer(ctx context.Context, se Session, instance, project string) (owner int64, ok bool, err error) {
	owner, found, err := d.Store.OwnerTeam(ctx, instance, project)
	if err != nil || !found {
		return 0, false, err
	}
	ok, err = d.maintainsTeam(ctx, se, owner)
	return owner, ok, err
}

// maintainerGate lets a non-admin go on only when it maintains some team and
// the instance lets teams onboard: 404 when it maintains none or the
// instance is unknown, 403 when the switch is off. It answers when it
// returns false. Callers check se.Admin first.
func (d *Dashboard) maintainerGate(w http.ResponseWriter, r *http.Request, instance string) bool {
	if !d.maintainerGateAnySwitch(w, r, instance) {
		return false
	}
	if !d.teamsMayOnboard(instance) {
		writeError(w, http.StatusForbidden, msgTeamsMayNotOnboard)
		return false
	}
	return true
}

// maintainerGateAnySwitch is maintainerGate without the switch check: a
// maintainer pauses and releases whatever the switch says.
func (d *Dashboard) maintainerGateAnySwitch(w http.ResponseWriter, r *http.Request, instance string) bool {
	if !d.knownInstance(instance) {
		notFound(w)
		return false
	}
	teams, err := d.maintainedTeams(r.Context(), session(r))
	if err != nil {
		d.failed(w, r, "reading the teams", err)
		return false
	}
	if len(teams) == 0 {
		notFound(w)
		return false
	}
	return true
}
