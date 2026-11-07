package dashboard

import (
	"sort"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Role is what a user may do on a repository; each includes the ones below
// it.
type Role int

const (
	// RoleNone: the repository isn't the user's to see.
	RoleNone Role = iota
	// RoleViewer reads the repository's reviews, runs and settings.
	RoleViewer
	// RoleMaintainer also changes its settings and presses re-run, dismiss
	// and restore.
	RoleMaintainer
	// RoleAdmin is the account's role: everything, on every repository.
	RoleAdmin
)

// String is the role's name as the API gives it: "admin", "maintainer",
// "viewer", or "" for RoleNone.
func (r Role) String() string {
	switch r {
	case RoleAdmin:
		return store.RoleAdmin
	case RoleMaintainer:
		return store.RoleMaintainer
	case RoleViewer:
		return store.RoleViewer
	}
	return ""
}

// roleOf is the Role a member's role on a team gives: viewer or maintainer.
// Anything else, "admin" included (that is the account's role, not a team
// member's), gives none.
func roleOf(memberRole string) Role {
	switch memberRole {
	case store.RoleMaintainer:
		return RoleMaintainer
	case store.RoleViewer:
		return RoleViewer
	}
	return RoleNone
}

// RepoKey is a repository: its instance and its lower-cased path.
type RepoKey struct{ Instance, Path string }

func repoKey(instance, project string) RepoKey { return RepoKey{instance, lower(project)} }

// teamOf is a team with the role one account has on it: what cover reads.
type teamOf struct {
	store.Team
	role Role
}

// teamsOf are the teams among teams on which the account has a role pergent.sh
// knows. A team the account isn't a member of, or with a role it doesn't know,
// gives nothing.
func teamsOf(teams []store.Team, accountID int64) []teamOf {
	var out []teamOf
	for _, t := range teams {
		for _, m := range t.Members {
			if role := roleOf(m.Role); m.AccountID == accountID && role != RoleNone {
				out = append(out, teamOf{Team: t, role: role})
				break
			}
		}
	}
	return out
}

// cover is the roles the teams give, over the repositories switched on
// (repos, of every instance):
//   - a team repository with no project covers every repository of its
//     instance, switched on now or later, and one with a project covers that
//     one, ignoring case;
//   - a repository not switched on is left out;
//   - of the teams covering a repository, the highest role wins.
func cover(teams []teamOf, repos []settings.Effective) map[RepoKey]Role {
	on := map[RepoKey]bool{}
	for _, e := range repos {
		on[repoKey(e.Instance, e.Project)] = true
	}
	roles := map[RepoKey]Role{}
	give := func(k RepoKey, r Role) {
		if on[k] && r > roles[k] {
			roles[k] = r
		}
	}
	for _, t := range teams {
		for _, tr := range t.Repos {
			if tr.Project != "" {
				give(repoKey(tr.Instance, tr.Project), t.role)
				continue
			}
			for k := range on {
				if k.Instance == tr.Instance {
					give(k, t.role)
				}
			}
		}
	}
	return roles
}

func sortKeys(keys []RepoKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Instance != keys[j].Instance {
			return keys[i].Instance < keys[j].Instance
		}
		return keys[i].Path < keys[j].Path
	})
}
