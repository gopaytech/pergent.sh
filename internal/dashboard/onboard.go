package dashboard

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/settings"
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

// requireOwnerMaintainer answers 403 msgNotOwnerMaintainer, and returns
// false, unless the session maintains repo's owner team. A team that only
// covers the repository is refused. Callers check se.Admin first.
func (d *Dashboard) requireOwnerMaintainer(w http.ResponseWriter, r *http.Request, repo settings.Effective) bool {
	_, ok, err := d.ownerMaintainer(r.Context(), session(r), repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading the owner team", err)
		return false
	}
	if !ok {
		writeError(w, http.StatusForbidden, msgNotOwnerMaintainer)
	}
	return ok
}

// gateDenial is the outcome of the maintainer gate without a response: 0 to
// go on, 404 when the instance is unknown or being removed or the session
// maintains no team, 403 when the switch is off. anySwitch leaves the switch
// out (a maintainer pauses and releases whatever it says). The handlers and
// the button flags both ask it, so they cannot disagree. Callers check
// se.Admin first.
func (d *Dashboard) gateDenial(ctx context.Context, se Session, instance string, anySwitch bool) (int, error) {
	if !d.knownInstance(instance) {
		return http.StatusNotFound, nil
	}
	teams, err := d.maintainedTeams(ctx, se)
	if err != nil {
		return 0, err
	}
	if len(teams) == 0 {
		return http.StatusNotFound, nil
	}
	if !anySwitch && !d.teamsMayOnboard(instance) {
		return http.StatusForbidden, nil
	}
	return 0, nil
}

// maintainerGate lets a non-admin go on only when it maintains some team and
// the instance lets teams onboard: 404 when it maintains none or the
// instance is unknown, 403 when the switch is off. It answers when it
// returns false. Callers check se.Admin first.
func (d *Dashboard) maintainerGate(w http.ResponseWriter, r *http.Request, instance string) bool {
	return d.answerGate(w, r, instance, false)
}

// maintainerGateAnySwitch is maintainerGate without the switch check: a
// maintainer pauses and releases whatever the switch says.
func (d *Dashboard) maintainerGateAnySwitch(w http.ResponseWriter, r *http.Request, instance string) bool {
	return d.answerGate(w, r, instance, true)
}

func (d *Dashboard) answerGate(w http.ResponseWriter, r *http.Request, instance string, anySwitch bool) bool {
	status, err := d.gateDenial(r.Context(), session(r), instance, anySwitch)
	switch {
	case err != nil:
		d.failed(w, r, "reading the teams", err)
	case status == http.StatusNotFound:
		notFound(w)
	case status == http.StatusForbidden:
		writeError(w, http.StatusForbidden, msgTeamsMayNotOnboard)
	}
	return err == nil && status == 0
}

// repoActions are the buttons repo's page shows se: each is true exactly when
// the matching route would succeed now. They are the handlers' own checks
// (gateDenial, ownerMaintainer, the switch and the state), so a flag and a
// route cannot disagree. A repository of the config file has none.
func (d *Dashboard) repoActions(ctx context.Context, se Session, repo settings.Effective) (RepoActionsView, error) {
	var a RepoActionsView
	if repo.Source == settings.SourceConfig {
		return a, nil
	}
	if !se.Admin {
		denied, err := d.gateDenial(ctx, se, repo.Instance, true)
		if err != nil || denied != 0 {
			return a, err
		}
	}
	owner, ok, err := d.ownerMaintainer(ctx, se, repo.Instance, repo.Project)
	if err != nil {
		return a, err
	}
	if !se.Admin && !ok {
		return a, nil
	}
	if se.Admin {
		a.Pause = !repo.Paused
		a.Resume = repo.Paused && d.knownInstance(repo.Instance)
		a.Remove = true
	} else {
		may := d.teamsMayOnboard(repo.Instance)
		a.Pause = !repo.Paused
		a.Resume = repo.Paused && may
		a.Remove = may
	}
	if owner != 0 {
		t, found, err := d.Store.Team(ctx, owner)
		if err != nil {
			return a, err
		}
		i := t.OwnEntry(repo.Instance, repo.Project)
		a.Release = found && i >= 0 && t.Repos[i].Owner
	}
	return a, nil
}

// onboardInstances are the instances se may onboard on from the picker: an
// admin every one not being removed, a maintainer of some team those with the
// switch on, nobody else any. Sorted, never nil.
func (d *Dashboard) onboardInstances(ctx context.Context, se Session) ([]string, error) {
	out := []string{}
	if !se.Admin {
		teams, err := d.maintainedTeams(ctx, se)
		if err != nil || len(teams) == 0 {
			return out, err
		}
	}
	for _, inst := range d.registry.All() {
		if se.Admin && !inst.Removing || !se.Admin && d.teamsMayOnboard(inst.Name) {
			out = append(out, inst.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

const (
	msgBadClaimBody   = `the body must be {"team": <id>}`
	msgBadReleaseBody = "the body must be {}"
	msgNoOwnOwner     = "the repository has no owner of its own"
)

// claimRepo is POST .../claim {"team": id}: the named team becomes the owner
// of a repository that is on or paused and has no owner at either level.
// It does not use target: a maintainer may not see a repository their team
// does not cover yet. A paused repository stays paused.
func (d *Dashboard) claimRepo(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	instance := r.PathValue("instance")
	if !se.Admin && !d.maintainerGate(w, r, instance) {
		return
	}
	project, ok := strings.CutSuffix(r.PathValue("rest"), "/claim")
	if !ok || project == "" {
		notFound(w)
		return
	}
	var body struct {
		Team *int64 `json:"team"`
	}
	if err := decodeStrict(r, &body); err != nil || body.Team == nil {
		writeError(w, http.StatusBadRequest, msgBadClaimBody)
		return
	}
	repo, found, err := d.Repos.GetAny(ctx, instance, project)
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	if repo.Source == settings.SourceConfig && !se.Admin {
		writeError(w, http.StatusConflict, msgConfigManaged)
		return
	}
	if !se.Admin {
		teams, err := d.maintainedTeams(ctx, se)
		if err != nil {
			d.failed(w, r, "reading the teams", err)
			return
		}
		if !slices.ContainsFunc(teams, func(t store.Team) bool { return t.ID == *body.Team }) {
			writeError(w, http.StatusForbidden, msgNotOwnerMaintainer)
			return
		}
	}
	// repo.Project is the spelling the repository is stored under.
	change, err := d.Store.ClaimOwner(ctx, repo.Instance, repo.Project, *body.Team)
	var taken *store.OwnerTakenError
	switch {
	case errors.As(err, &taken):
		writeError(w, http.StatusConflict, taken.Error())
		return
	case errors.Is(err, store.ErrUnknownTeam):
		writeError(w, http.StatusBadRequest, msgNoTeam)
		return
	case err != nil:
		d.Log.Error("dashboard: claiming a repository failed", "instance", repo.Instance, "project", repo.Project,
			"team", *body.Team, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: repository claimed", "instance", repo.Instance, "project", repo.Project,
		"team", change.After.Name, "by", se.Username)
	d.audit(ctx, se, "repository_claimed", repo.Instance, repo.Project, change.After.Name)
	d.auditOwners(ctx, se, change.Before, change.After)
	if !se.Admin {
		// The session was read before the owner tick: the new owner's
		// maintainer sees and runs the repository it just claimed.
		k := repoKey(repo.Instance, repo.Project)
		visible, roles := maps.Clone(se.Visible), maps.Clone(se.Roles)
		if visible == nil {
			visible = map[RepoKey]int64{}
		}
		if roles == nil {
			roles = map[RepoKey]Role{}
		}
		if _, ok := visible[k]; !ok {
			visible[k] = 0
		}
		roles[k] = max(roles[k], RoleMaintainer)
		se.Visible, se.Roles = visible, roles
	}
	d.writeDetail(w, r, se, repo.Instance, repo.Project)
}

// releaseRepo is POST .../release {}: clears the owner tick on the owner
// team's own entry for the repository and keeps the entry. A maintainer of
// the owner team releases whatever the switch says; an instance-level owner
// is released by an admin only. An admin releases the tick of whichever team
// holds it.
func (d *Dashboard) releaseRepo(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	if !se.Admin && !d.maintainerGateAnySwitch(w, r, r.PathValue("instance")) {
		return
	}
	repo, ok := d.target(w, r, "/release", RoleViewer)
	if !ok {
		return
	}
	var body struct{}
	if err := decodeStrict(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, msgBadReleaseBody)
		return
	}
	owner, ok, err := d.ownerMaintainer(ctx, se, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading the owner team", err)
		return
	}
	if !se.Admin && !ok {
		writeError(w, http.StatusForbidden, msgNotOwnerMaintainer)
		return
	}
	if owner == 0 {
		// An admin, and no owner at either level.
		writeError(w, http.StatusConflict, msgNoOwnOwner)
		return
	}
	change, released, err := d.Store.ReleaseOwner(ctx, repo.Instance, repo.Project, owner)
	switch {
	case errors.Is(err, store.ErrUnknownTeam):
		writeError(w, http.StatusConflict, msgNoOwnOwner)
		return
	case err != nil:
		d.Log.Error("dashboard: releasing a repository failed", "instance", repo.Instance, "project", repo.Project,
			"team", owner, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !released {
		// The owner is the instance's: only an admin releases that, by the
		// team page.
		if !se.Admin {
			writeError(w, http.StatusForbidden, msgNotOwnerMaintainer)
		} else {
			writeError(w, http.StatusConflict, msgNoOwnOwner)
		}
		return
	}
	d.Log.Info("dashboard: repository released", "instance", repo.Instance, "project", repo.Project,
		"team", change.After.Name, "by", se.Username)
	d.audit(ctx, se, "repository_released", repo.Instance, repo.Project, change.After.Name)
	d.auditOwners(ctx, se, change.Before, change.After)
	d.writeDetail(w, r, se, repo.Instance, repo.Project)
}

// writeDetail answers 200 with the stored repository's detail view.
func (d *Dashboard) writeDetail(w http.ResponseWriter, r *http.Request, se Session, instance, project string) {
	ctx := r.Context()
	now, ok, err := d.Repos.GetAny(ctx, instance, project)
	if err == nil && !ok {
		err = errors.New("the repository isn't there any more")
	}
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	view, err := d.detailView(ctx, se, now)
	if err != nil {
		d.failed(w, r, "loading a repository", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
