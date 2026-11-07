package dashboard

import (
	"context"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// accessOf is what an account may see, read from its role and its teams.
type accessOf struct {
	// admin: the account's role is admin; it sees and does everything, and
	// the other fields are empty.
	admin bool
	// waiting: not an admin, and on no team.
	waiting bool
	// visible holds each repository the teams cover that is switched on,
	// with the platform ID of the project stored at its path: 0 for one
	// reconcile has not stored yet, which is visible all the same.
	visible map[RepoKey]int64
	// roles holds the highest role the teams give on each of them.
	roles map[RepoKey]Role
}

// load reads what account may see now. An admin sees everything. Any other
// account sees the switched-on repositories its teams cover (coverage), each
// with the role its member role gives, the highest where several teams cover
// it. The team is the only limit: nothing asks GitLab or GitHub.
func (a *Auth) load(ctx context.Context, account store.Account) (accessOf, error) {
	if account.Role == store.RoleAdmin {
		return accessOf{admin: true}, nil
	}
	teams, err := a.store.AccountTeams(ctx, account.ID)
	if err != nil {
		return accessOf{}, err
	}
	mine := teamsOf(teams, account.ID)
	if len(mine) == 0 {
		return accessOf{waiting: true}, nil
	}
	repos, err := a.repos.List(ctx)
	if err != nil {
		return accessOf{}, err
	}
	out := accessOf{roles: cover(mine, repos)}
	// The ID of the project at each covered path, from the projects reconcile
	// stored (in project order: of two at one path, the highest ID wins, as
	// ProjectByPath has it).
	out.visible = make(map[RepoKey]int64, len(out.roles))
	for k := range out.roles {
		out.visible[k] = 0
	}
	if len(out.visible) == 0 {
		return out, nil
	}
	projects, err := a.store.Projects(ctx)
	if err != nil {
		return accessOf{}, err
	}
	for _, p := range projects {
		if k := repoKey(p.Instance, p.Path); p.ProjectID > 0 {
			if _, ok := out.visible[k]; ok {
				out.visible[k] = p.ProjectID
			}
		}
	}
	return out, nil
}

// dismissalName is the name a dismissal made by se shows under on a pull
// request or merge request (and in the dashboard): the part of an SSO
// account's email before the "@", so an address is never posted to a
// platform; a local admin keeps the form it has. The audit log keeps the full
// email (actorLabel).
func dismissalName(se Session) string {
	if se.Kind == store.AccountSSO {
		if name, _, ok := strings.Cut(se.Email, "@"); ok && name != "" {
			return name
		}
	}
	return actorLabel(se.Kind, se.Username)
}
