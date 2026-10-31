package dashboard

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/password"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// auditPageSize is the most entries GET /api/audit returns at once.
	auditPageSize = 100
	// auditMaxChunks bounds the reading for one GET /api/audit: chunks of
	// auditPageSize entries, read past those the session can't see.
	auditMaxChunks = 50
	// maxTeamName is the longest team name, in characters.
	maxTeamName = 100

	msgBadUsername    = "the username must be 1 to 64 letters, digits, \".\", \"_\" or \"-\""
	msgUsernameTaken  = "this username is taken"
	msgPasswordOnly   = "only password admins can remove password accounts"
	msgBusy           = "the server is busy, please try again"
	msgBadTeamBody    = `the body must be {"name": ..., "role": ...}`
	msgBadTeamUpdate  = `the body must be {"name": ..., "role": ..., "members": [account IDs], "repos": [{"instance": ..., "project": ...}]}`
	msgBadTeamName    = "the team needs a name of at most 100 characters"
	msgBadRole        = `role must be "admin", "maintainer" or "viewer"`
	msgTeamNameTaken  = "a team with this name exists already"
	msgMembers        = "members must be accounts that sign in with GitLab or GitHub"
	msgAdminTeamRepos = "an admin team has no repository list: repos must be []"
	msgRepoNotOn      = "every repository must be switched on, or name a whole instance with project \"\""
	msgOtherInstance  = "you can add repositories of your own instance only"
	msgBadBefore      = "before must be the ID of an audit entry"
	msgHiddenCoverage = "this team covers repositories you can't see; a password admin can change its role or remove it"
	msgOtherAccount   = "you can only remove accounts of your own instance"
	msgOtherMembers   = "you can only change members of your own instance"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// orgRoutes adds the users, teams and audit log API to mux: every route
// behind Auth.Require (which checks X-CSRF-Token on writes), for admins
// only, and adding a password admin for password admins only.
func (d *Dashboard) orgRoutes(mux *http.ServeMux) {
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, d.Auth.Require(requireAdmin(h))) }
	admin("GET /api/users", d.users)
	admin("DELETE /api/users/{id}", d.deleteUser)
	mux.Handle("POST /api/admin/accounts", d.Auth.Require(requireOperator(d.createPasswordAccount)))
	admin("GET /api/teams", d.teams)
	admin("POST /api/teams", d.createTeam)
	admin("PUT /api/teams/{id}", d.updateTeam)
	admin("DELETE /api/teams/{id}", d.deleteTeam)
	admin("GET /api/audit", d.auditLog)
}

// audit records what the session's user did; a failure to is logged, not
// returned: the change is made already.
func (d *Dashboard) audit(ctx context.Context, se Session, action, instance, project, detail string) {
	id := se.AccountID
	if err := d.Store.Audit(context.WithoutCancel(ctx), store.AuditEntry{At: d.now(), ActorID: &id, Actor: se.Username,
		Action: action, Instance: instance, Project: project, Detail: detail}); err != nil {
		d.Log.Error("dashboard: recording an audit entry failed", "action", action, "error", err.Error())
	}
}

// pathID is the request's {id}; ok is false when it isn't one.
func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// --- users ---

// seesAccount reports whether the session can see an account: an operator
// every one; a platform admin the platform accounts of its own instance.
func seesAccount(se Session, a store.Account) bool {
	return se.Operator || (a.Kind == store.AccountPlatform && a.Instance == se.Instance)
}

func (d *Dashboard) users(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	accounts, err := d.Store.Accounts(ctx)
	if err != nil {
		d.failed(w, r, "listing accounts", err)
		return
	}
	teams, err := d.Store.Teams(ctx)
	if err != nil {
		d.failed(w, r, "listing teams", err)
		return
	}
	of := map[int64][]TeamRef{}
	for _, t := range teams {
		for _, m := range t.Members {
			of[m] = append(of[m], TeamRef{ID: t.ID, Name: t.Name})
		}
	}
	out := make([]UserView, 0, len(accounts))
	for _, a := range accounts {
		if !seesAccount(se, a) {
			continue
		}
		refs := of[a.ID]
		if refs == nil {
			refs = []TeamRef{}
		}
		out = append(out, UserView{
			ID: a.ID, Kind: a.Kind, Username: a.Username, Platform: a.Platform, Instance: a.Instance,
			LastSignInAt: a.LastSignInAt, Waiting: a.Kind == store.AccountPlatform && len(refs) == 0, Teams: refs,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Dashboard) deleteUser(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	id, ok := pathID(r)
	if !ok {
		notFound(w)
		return
	}
	acc, found, err := d.Store.Account(ctx, id)
	if err != nil {
		d.failed(w, r, "loading an account", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	if acc.Kind == store.AccountPassword && !se.Operator {
		writeError(w, http.StatusForbidden, msgPasswordOnly)
		return
	}
	if !seesAccount(se, acc) {
		writeError(w, http.StatusForbidden, msgOtherAccount)
		return
	}
	deleted, err := d.Store.DeleteAccount(ctx, id)
	if errors.Is(err, store.ErrLastPasswordAccount) {
		writeError(w, http.StatusConflict, store.ErrLastPasswordAccount.Error())
		return
	}
	if err != nil {
		d.Log.Error("dashboard: removing an account failed", "account", id, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !deleted {
		notFound(w)
		return
	}
	d.Log.Info("dashboard: account removed", "user", acc.Username, "kind", acc.Kind, "by", se.Username)
	d.audit(ctx, se, "account_deleted", "", "", acc.Username)
	w.WriteHeader(http.StatusNoContent)
}

// createPasswordAccount makes a password admin with a generated password,
// which must be changed at its first sign-in. The password is in the
// answer only: never logged or audited.
func (d *Dashboard) createPasswordAccount(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var in struct {
		Username string `json:"username"`
	}
	if err := decodeStrict(r, &in); err != nil || !usernamePattern.MatchString(in.Username) {
		writeError(w, http.StatusBadRequest, msgBadUsername)
		return
	}
	pw := password.Generate()
	release, ok := d.Auth.hashSlot(ctx)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, msgBusy)
		return
	}
	hash, err := d.Auth.hash(pw)
	release()
	if err != nil {
		d.Log.Error("dashboard: hashing a new password failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	acc, err := d.Store.CreatePasswordAccount(ctx, in.Username, hash, true, d.now())
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, msgUsernameTaken)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: creating a password account failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: password account created", "user", acc.Username, "by", se.Username)
	d.audit(ctx, se, "account_created", "", "", acc.Username)
	writeJSON(w, http.StatusCreated, NewAccountView{Username: acc.Username, Password: pw})
}

// --- teams ---

// seesTeamRepo reports whether the session can see a team's repository: an
// operator every one; a platform admin a whole-instance entry of its own
// instance and the repositories it can see.
func seesTeamRepo(se Session, tr store.TeamRepo) bool {
	if se.All {
		return true
	}
	if tr.Project == "" {
		return tr.Instance == se.Instance
	}
	return se.CanSee(tr.Instance, tr.Project)
}

// teamView is t as the session sees it; accounts are by ID.
func teamView(se Session, t store.Team, accounts map[int64]store.Account) TeamView {
	v := TeamView{ID: t.ID, Name: t.Name, Role: t.Role, Members: []TeamMemberView{}, Repos: []TeamRepoView{}}
	for _, id := range t.Members {
		a := accounts[id]
		if !seesAccount(se, a) {
			v.HiddenMembers++
			continue
		}
		v.Members = append(v.Members, TeamMemberView{ID: id, Username: a.Username, Platform: a.Platform, Instance: a.Instance})
	}
	for _, tr := range t.Repos {
		if seesTeamRepo(se, tr) {
			v.Repos = append(v.Repos, TeamRepoView{Instance: tr.Instance, Project: tr.Project})
		} else {
			v.HiddenRepos++
		}
	}
	return v
}

func (d *Dashboard) accountsByID(ctx context.Context) (map[int64]store.Account, error) {
	accounts, err := d.Store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]store.Account, len(accounts))
	for _, a := range accounts {
		out[a.ID] = a
	}
	return out, nil
}

func (d *Dashboard) teams(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	teams, err := d.Store.Teams(ctx)
	if err != nil {
		d.failed(w, r, "listing teams", err)
		return
	}
	accounts, err := d.accountsByID(ctx)
	if err != nil {
		d.failed(w, r, "listing accounts", err)
		return
	}
	out := make([]TeamView, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamView(se, t, accounts))
	}
	writeJSON(w, http.StatusOK, out)
}

// teamNameAndRole checks a team's name (trimmed) and role; msg is the 400's
// message when one is wrong.
func teamNameAndRole(name, role string) (string, string) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxTeamName {
		return "", msgBadTeamName
	}
	if roleOf(role) == RoleNone {
		return "", msgBadRole
	}
	return name, ""
}

func (d *Dashboard) createTeam(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var in struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadTeamBody)
		return
	}
	name, msg := teamNameAndRole(in.Name, in.Role)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	t, err := d.Store.CreateTeam(ctx, name, in.Role, d.now())
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, msgTeamNameTaken)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: creating a team failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: team created", "team", t.Name, "role", t.Role, "by", se.Username)
	d.audit(ctx, se, "team_created", "", "", t.Name+" ("+t.Role+")")
	writeJSON(w, http.StatusCreated, teamView(se, t, nil))
}

// errHiddenCoverage refuses a change that would act on repositories a team
// covers which the session can't see: changing its role (an admin team has
// none) or removing it. Only a session that sees everything can.
var errHiddenCoverage = errors.New(msgHiddenCoverage)

// errOtherMembers refuses adding a member of another instance: a platform
// admin only changes the members of its own.
var errOtherMembers = errors.New(msgOtherMembers)

// coversHidden reports whether t covers a repository the session can't see.
func coversHidden(se Session, t store.Team) bool {
	return slices.ContainsFunc(t.Repos, func(tr store.TeamRepo) bool { return !seesTeamRepo(se, tr) })
}

func (d *Dashboard) updateTeam(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	id, ok := pathID(r)
	if !ok {
		notFound(w)
		return
	}
	var in struct {
		Name    string         `json:"name"`
		Role    string         `json:"role"`
		Members []int64        `json:"members"`
		Repos   []TeamRepoView `json:"repos"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadTeamUpdate)
		return
	}
	name, msg := teamNameAndRole(in.Name, in.Role)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	accounts, err := d.accountsByID(ctx)
	if err != nil {
		d.failed(w, r, "listing accounts", err)
		return
	}
	for _, m := range in.Members {
		if accounts[m].Kind != store.AccountPlatform {
			writeError(w, http.StatusBadRequest, msgMembers)
			return
		}
	}
	if in.Role == store.RoleAdmin && len(in.Repos) > 0 {
		writeError(w, http.StatusBadRequest, msgAdminTeamRepos)
		return
	}
	repos, msg, err := d.teamRepos(ctx, se, in.Repos)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	var before, after store.Team
	found, err := d.Store.UpdateTeam(ctx, id, func(t *store.Team) error {
		before = *t
		before.Members, before.Repos = slices.Clone(t.Members), slices.Clone(t.Repos)
		// A role change gives the repositories the session can't see a
		// role out of its sight: only a session that sees them all may.
		if t.Role != in.Role && coversHidden(se, *t) {
			return errHiddenCoverage
		}
		members, err := teamMembers(se, *t, in.Members, accounts)
		if err != nil {
			return err
		}
		saved := repos
		if in.Role == store.RoleAdmin {
			saved = nil // an admin team has no repository list
		} else {
			// What the session can't see stays as it is.
			for _, tr := range t.Repos {
				if !seesTeamRepo(se, tr) {
					saved = append(saved, tr)
				}
			}
		}
		t.Name, t.Role, t.Members, t.Repos = name, in.Role, members, saved
		after = *t
		return nil
	})
	switch {
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, msgTeamNameTaken)
		return
	case errors.Is(err, errHiddenCoverage):
		writeError(w, http.StatusConflict, msgHiddenCoverage)
		return
	case errors.Is(err, errOtherMembers):
		writeError(w, http.StatusConflict, msgOtherMembers)
		return
	case err != nil:
		d.Log.Error("dashboard: changing a team failed", "team", id, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	case !found:
		notFound(w)
		return
	}
	if parts := teamChanges(before, after); len(parts) > 0 {
		d.Log.Info("dashboard: team changed", "team", after.Name, "changed", strings.Join(parts, ", "), "by", se.Username)
		d.audit(ctx, se, "team_updated", "", "", after.Name+": "+strings.Join(parts, ", "))
	}
	t, found, err := d.Store.Team(ctx, id)
	if err == nil && !found {
		err = errors.New("the team changed isn't there")
	}
	if err != nil {
		d.failed(w, r, "loading a team", err)
		return
	}
	writeJSON(w, http.StatusOK, teamView(se, t, accounts))
}

// teamMembers is the member list to store for a team change that names
// submitted. An operator's is the one submitted. A platform admin changes
// only the members of its own instance: those of other instances stay on
// the team whatever it names (it is never shown them), and naming one that
// isn't on the team already is errOtherMembers.
func teamMembers(se Session, t store.Team, submitted []int64, accounts map[int64]store.Account) ([]int64, error) {
	if se.Operator {
		return submitted, nil
	}
	own := func(id int64) bool { return accounts[id].Instance == se.Instance }
	var out []int64
	for _, id := range t.Members {
		if !own(id) {
			out = append(out, id)
		}
	}
	kept := slices.Clone(out)
	for _, id := range submitted {
		switch {
		case own(id):
			out = append(out, id)
		case !slices.Contains(kept, id):
			return nil, errOtherMembers
		}
	}
	return out, nil
}

// teamRepos checks the repositories of a team change and returns those to
// store, spelled as the resolver spells them. Each must be switched on, or
// a whole instance that exists (msg otherwise). A platform admin names
// repositories of its own instance only; those it can't see are dropped
// here, whether they are switched on or not, and the team's own are kept by
// the caller.
func (d *Dashboard) teamRepos(ctx context.Context, se Session, in []TeamRepoView) ([]store.TeamRepo, string, error) {
	if len(in) == 0 {
		return nil, "", nil
	}
	on, err := d.Repos.List(ctx)
	if err != nil {
		return nil, "", err
	}
	spelled := make(map[RepoKey]string, len(on))
	for _, e := range on {
		spelled[repoKey(e.Instance, e.Project)] = e.Project
	}
	var out []store.TeamRepo
	for _, tr := range in {
		if !se.All && tr.Instance != se.Instance {
			return nil, msgOtherInstance, nil
		}
		repo := store.TeamRepo{Instance: tr.Instance, Project: tr.Project}
		if !seesTeamRepo(se, repo) {
			continue
		}
		if tr.Project == "" {
			if !d.knownInstance(tr.Instance) {
				return nil, msgRepoNotOn, nil
			}
		} else {
			p, ok := spelled[repoKey(tr.Instance, tr.Project)]
			if !ok {
				return nil, msgRepoNotOn, nil
			}
			repo.Project = p
		}
		out = append(out, repo)
	}
	return out, "", nil
}

// knownInstance reports whether instance is one of the config's or one
// added in the browser and not being removed.
func (d *Dashboard) knownInstance(instance string) bool {
	inst, ok := d.registry.Get(instance)
	return ok && !inst.Removing
}

// teamChanges names what differs between a team before and after a change:
// name, role, members and repos, in that order.
func teamChanges(before, after store.Team) []string {
	var parts []string
	if before.Name != after.Name {
		parts = append(parts, "name")
	}
	if before.Role != after.Role {
		parts = append(parts, "role")
	}
	members := func(ids []int64) []int64 {
		out := slices.Clone(ids)
		slices.Sort(out)
		return slices.Compact(out)
	}
	if !slices.Equal(members(before.Members), members(after.Members)) {
		parts = append(parts, "members")
	}
	repos := func(rs []store.TeamRepo) []RepoKey {
		out := make([]RepoKey, 0, len(rs))
		for _, r := range rs {
			out = append(out, repoKey(r.Instance, r.Project))
		}
		sortKeys(out)
		return slices.Compact(out)
	}
	if !slices.Equal(repos(before.Repos), repos(after.Repos)) {
		parts = append(parts, "repos")
	}
	return parts
}

func (d *Dashboard) deleteTeam(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	id, ok := pathID(r)
	if !ok {
		notFound(w)
		return
	}
	t, found, err := d.Store.Team(ctx, id)
	if err != nil {
		d.failed(w, r, "loading a team", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	if coversHidden(se, t) {
		writeError(w, http.StatusConflict, msgHiddenCoverage)
		return
	}
	deleted, err := d.Store.DeleteTeam(ctx, id)
	if err != nil {
		d.Log.Error("dashboard: removing a team failed", "team", id, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !deleted {
		notFound(w)
		return
	}
	d.Log.Info("dashboard: team removed", "team", t.Name, "by", se.Username)
	d.audit(ctx, se, "team_deleted", "", "", t.Name)
	w.WriteHeader(http.StatusNoContent)
}

// --- the audit log ---

// auditLog answers up to auditPageSize entries the session can see, newest
// first, older than ?before= when given, and the before of the page after.
// For a platform admin it reads on past the entries it can't see, at most
// auditMaxChunks chunks: when they don't fill the page, it answers what
// they held, and next goes on after the last entry read.
func (d *Dashboard) auditLog(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var before int64
	if q := r.URL.Query().Get("before"); q != "" {
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, msgBadBefore)
			return
		}
		before = n
	}
	page := AuditPageView{Entries: []AuditView{}}
	for chunk := 0; chunk < auditMaxChunks && len(page.Entries) < auditPageSize; chunk++ {
		entries, err := d.Store.AuditLog(ctx, before, auditPageSize)
		if err != nil {
			d.failed(w, r, "reading the audit log", err)
			return
		}
		read := 0
		for _, e := range entries {
			if len(page.Entries) == auditPageSize {
				break
			}
			if e.Project == "" || se.CanSee(e.Instance, e.Project) {
				page.Entries = append(page.Entries, AuditView{ID: e.ID, At: e.At.UTC(), Actor: e.Actor, Action: e.Action,
					Instance: e.Instance, Project: e.Project, Detail: e.Detail})
			}
			before = e.ID
			read++
		}
		if read == len(entries) && len(entries) < auditPageSize {
			before = 0 // the log ends here
			break
		}
	}
	page.Next = before
	writeJSON(w, http.StatusOK, page)
}
