package dashboard

import (
	"context"
	"errors"
	"maps"
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
	// maxTeamName is the longest team name, in characters.
	maxTeamName = 100

	msgBadUsername    = "the username must be 1 to 64 letters, digits, \".\", \"_\" or \"-\""
	msgUsernameTaken  = "this username is taken"
	msgBusy           = "the server is busy, please try again"
	msgBadTeamBody    = `the body must be {"name": ...}`
	msgBadTeamUpdate  = `the body must be {"name": ..., "members": [{"accountId": ..., "role": "viewer" or "maintainer"}], "repos": [{"instance": ..., "project": ...}]}`
	msgBadTeamName    = "the team needs a name of at most 100 characters"
	msgBadMemberRole  = `a member's role must be "viewer" or "maintainer"`
	msgTeamNameTaken  = "a team with this name exists already"
	msgMembers        = "members must be accounts that sign in with SSO"
	msgRepoNotOn      = "every repository must be switched on, or name a whole instance with project \"\""
	msgBadBefore      = "before must be the ID of an audit entry"
	msgDeleteSelf     = "you can't remove your own account"
	msgOwnRole        = "you can't change your own role"
	msgBadAccountRole = `the body must be {"role": "admin" or "user"}`
	msgLocalRole      = "a local admin is always an admin"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// orgRoutes adds the users, teams and audit log API to mux: every route
// behind Auth.Require (which checks X-CSRF-Token on writes), for admins
// only.
func (d *Dashboard) orgRoutes(mux *http.ServeMux) {
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, d.Auth.Require(requireAdmin(h))) }
	admin("GET /api/users", d.users)
	admin("DELETE /api/users/{id}", d.deleteUser)
	admin("PUT /api/admin/users/{id}/role", d.setUserRole)
	admin("POST /api/admin/accounts", d.createPasswordAccount)
	admin("GET /api/teams", d.teams)
	admin("POST /api/teams", d.createTeam)
	admin("PUT /api/teams/{id}", d.updateTeam)
	admin("DELETE /api/teams/{id}", d.deleteTeam)
	admin("GET /api/audit", d.auditLog)
}

// audit records what the session's user did; a failure to is logged, not
// returned.
func (d *Dashboard) audit(ctx context.Context, se Session, action, instance, project, detail string) {
	id := se.AccountID
	if err := d.Store.Audit(context.WithoutCancel(ctx), store.AuditEntry{At: d.now(), ActorID: &id,
		Actor:  actorLabel(se.Kind, se.Username),
		Action: action, Instance: instance, Project: project, Detail: detail}); err != nil {
		d.Log.Error("dashboard: recording an audit entry failed", "action", action, "error", err.Error())
	}
}

// actorLabel names who acted in the audit log: "root (password)" for a local
// admin, the full email for an SSO account. (A password reset from the command
// line is "cli"; older entries of platform accounts read "alice (gitlab
// main)" and stay as they are.)
func actorLabel(kind, username string) string {
	if kind == store.AccountPassword {
		return username + " (password)"
	}
	return username
}

// pathID is the request's {id}; ok is false when it isn't one.
func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// --- users ---

func (d *Dashboard) users(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
			of[m.AccountID] = append(of[m.AccountID], TeamRef{ID: t.ID, Name: t.Name})
		}
	}
	out := make([]UserView, 0, len(accounts))
	for _, a := range accounts {
		refs := of[a.ID]
		if refs == nil {
			refs = []TeamRef{}
		}
		sso := a.Kind == store.AccountSSO
		out = append(out, UserView{
			ID: a.ID, Kind: a.Kind, Username: a.Username, Email: a.Email, Role: a.Role,
			Invited: sso && a.LastSignInAt == nil, LastSignInAt: a.LastSignInAt,
			Waiting: sso && a.Role != store.RoleAdmin && a.LastSignInAt != nil && len(refs) == 0, Teams: refs,
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
	// The old platform accounts are hidden: as good as not there.
	if !found || acc.Kind == store.AccountPlatform {
		notFound(w)
		return
	}
	if acc.ID == se.AccountID {
		writeError(w, http.StatusConflict, msgDeleteSelf)
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

// setUserRole makes an SSO account an admin or a user: JSON {"role"}. An admin
// can't change their own role, so there is always an admin left to change
// another's. A local admin is always an admin. The account's next request
// reads the new role.
func (d *Dashboard) setUserRole(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	id, ok := pathID(r)
	if !ok {
		notFound(w)
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if err := decodeStrict(r, &in); err != nil || (in.Role != store.RoleAdmin && in.Role != store.RoleUser) {
		writeError(w, http.StatusBadRequest, msgBadAccountRole)
		return
	}
	acc, found, err := d.Store.Account(ctx, id)
	if err != nil {
		d.failed(w, r, "loading an account", err)
		return
	}
	if !found || acc.Kind == store.AccountPlatform {
		notFound(w)
		return
	}
	switch {
	case acc.ID == se.AccountID:
		writeError(w, http.StatusConflict, msgOwnRole)
		return
	case acc.Kind != store.AccountSSO:
		writeError(w, http.StatusConflict, msgLocalRole)
		return
	}
	if err := d.Store.SetAccountRole(ctx, id, in.Role); err != nil {
		d.Log.Error("dashboard: changing an account's role failed", "account", id, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if acc.Role != in.Role {
		d.Log.Info("dashboard: account role changed", "user", acc.Username, "role", in.Role, "by", se.Username)
		d.audit(ctx, se, "account_role_changed", "", "", acc.Username+": "+acc.Role+"->"+in.Role)
	}
	w.WriteHeader(http.StatusNoContent)
}

// createPasswordAccount makes a local admin with a generated password, which
// must be changed at its first sign-in. The password is in the answer only:
// never logged or audited.
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

// teamView is t as an admin sees it; accounts are by ID.
func teamView(t store.Team, accounts map[int64]store.Account) TeamView {
	v := TeamView{ID: t.ID, Name: t.Name, Members: []TeamMemberView{}, Repos: []TeamRepoView{}}
	for _, m := range t.Members {
		a := accounts[m.AccountID]
		v.Members = append(v.Members, TeamMemberView{AccountID: m.AccountID, Username: a.Username, Email: a.Email, Role: m.Role})
	}
	for _, tr := range t.Repos {
		v.Repos = append(v.Repos, TeamRepoView{Instance: tr.Instance, Project: tr.Project})
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
	ctx := r.Context()
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
		out = append(out, teamView(t, accounts))
	}
	writeJSON(w, http.StatusOK, out)
}

// teamName checks a team's name (trimmed); msg is the 400's message when it
// is wrong.
func teamName(name string) (string, string) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxTeamName {
		return "", msgBadTeamName
	}
	return name, ""
}

func (d *Dashboard) createTeam(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadTeamBody)
		return
	}
	name, msg := teamName(in.Name)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	t, err := d.Store.CreateTeam(ctx, name, d.now())
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, msgTeamNameTaken)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: creating a team failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: team created", "team", t.Name, "by", se.Username)
	d.audit(ctx, se, "team_created", "", "", t.Name)
	writeJSON(w, http.StatusCreated, teamView(t, nil))
}

// memberInput is a team member in a team change: an account and its role on
// the team.
type memberInput struct {
	AccountID int64  `json:"accountId"`
	Role      string `json:"role"`
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
		Members []memberInput  `json:"members"`
		Repos   []TeamRepoView `json:"repos"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadTeamUpdate)
		return
	}
	name, msg := teamName(in.Name)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	accounts, err := d.accountsByID(ctx)
	if err != nil {
		d.failed(w, r, "listing accounts", err)
		return
	}
	members := make([]store.TeamMember, len(in.Members))
	for i, m := range in.Members {
		if m.Role != store.RoleViewer && m.Role != store.RoleMaintainer {
			writeError(w, http.StatusBadRequest, msgBadMemberRole)
			return
		}
		if accounts[m.AccountID].Kind != store.AccountSSO {
			writeError(w, http.StatusBadRequest, msgMembers)
			return
		}
		members[i] = store.TeamMember{AccountID: m.AccountID, Role: m.Role}
	}
	repos, msg, err := d.teamRepos(ctx, in.Repos)
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
		t.Name, t.Members, t.Repos = name, members, repos
		after = *t
		return nil
	})
	switch {
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, msgTeamNameTaken)
		return
	case errors.Is(err, store.ErrUnknownAccount):
		// Removed since the members were checked.
		writeError(w, http.StatusBadRequest, msgMembers)
		return
	case err != nil:
		d.Log.Error("dashboard: changing a team failed", "team", id, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	case !found:
		notFound(w)
		return
	}
	if parts := teamChanges(before, after, func(id int64) string {
		if a, ok := accounts[id]; ok {
			return a.Username
		}
		return "#" + strconv.FormatInt(id, 10)
	}); len(parts) > 0 {
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
	writeJSON(w, http.StatusOK, teamView(t, accounts))
}

// teamRepos checks the repositories of a team change and returns those to
// store, spelled as the resolver spells them. Each must be switched on, or a
// whole instance that exists (msg otherwise).
func (d *Dashboard) teamRepos(ctx context.Context, in []TeamRepoView) ([]store.TeamRepo, string, error) {
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
		repo := store.TeamRepo{Instance: tr.Instance, Project: tr.Project}
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
// name, members (+ added, - removed, name:role for a changed role; by username
// as userName gives it) and repos, in that order. Nothing in it is a secret.
func teamChanges(before, after store.Team, userName func(int64) string) []string {
	var parts []string
	if before.Name != after.Name {
		parts = append(parts, "name")
	}
	was, now := roleByAccount(before.Members), roleByAccount(after.Members)
	var names []string
	for _, id := range slices.Sorted(maps.Keys(now)) {
		switch old, had := was[id]; {
		case !had:
			names = append(names, "+"+userName(id)+":"+now[id])
		case old != now[id]:
			names = append(names, userName(id)+":"+old+"->"+now[id])
		}
	}
	for _, id := range slices.Sorted(maps.Keys(was)) {
		if _, still := now[id]; !still {
			names = append(names, "-"+userName(id))
		}
	}
	if len(names) > 0 {
		parts = append(parts, "members "+strings.Join(names, " "))
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

// roleByAccount is the role of each member by account ID; an account named
// twice has the higher role, as the store keeps it.
func roleByAccount(members []store.TeamMember) map[int64]string {
	out := make(map[int64]string, len(members))
	for _, m := range members {
		role := m.Role
		if role == "" {
			role = store.RoleViewer
		}
		if role == store.RoleMaintainer || out[m.AccountID] == "" {
			out[m.AccountID] = role
		}
	}
	return out
}

func (d *Dashboard) deleteTeam(w http.ResponseWriter, r *http.Request) {
	ctx, se := r.Context(), session(r)
	id, ok := pathID(r)
	if !ok {
		notFound(w)
		return
	}
	var name string
	deleted, err := d.Store.DeleteTeam(ctx, id, func(t store.Team) error {
		name = t.Name
		return nil
	})
	if err != nil {
		d.Log.Error("dashboard: removing a team failed", "team", id, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !deleted {
		notFound(w)
		return
	}
	d.Log.Info("dashboard: team removed", "team", name, "by", se.Username)
	d.audit(ctx, se, "team_deleted", "", "", name)
	w.WriteHeader(http.StatusNoContent)
}

// --- the audit log ---

// auditLog answers up to auditPageSize entries, newest first, older than
// ?before= when given, and the before of the page after (0 when the log ends
// here).
func (d *Dashboard) auditLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var before int64
	if q := r.URL.Query().Get("before"); q != "" {
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, msgBadBefore)
			return
		}
		before = n
	}
	entries, err := d.Store.AuditLog(ctx, before, auditPageSize)
	if err != nil {
		d.failed(w, r, "reading the audit log", err)
		return
	}
	page := AuditPageView{Entries: make([]AuditView, 0, len(entries))}
	for _, e := range entries {
		page.Entries = append(page.Entries, AuditView{ID: e.ID, At: e.At.UTC(), Actor: e.Actor, Action: e.Action,
			Instance: e.Instance, Project: e.Project, Detail: e.Detail})
	}
	if len(entries) == auditPageSize {
		page.Next = entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, page)
}
