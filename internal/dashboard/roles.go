package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
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
	// RoleMaintainer also changes its settings.
	RoleMaintainer
	// RoleAdmin also switches it on and off.
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

// roleOf is the Role a team's role gives.
func roleOf(teamRole string) Role {
	switch teamRole {
	case store.RoleAdmin:
		return RoleAdmin
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

// coverage is what a platform account's teams give it on the instance it
// signed in with, before the platform's ceiling: the platform check (see
// checkAccess) looks only at these repositories, and the session sees the
// ones it also passes.
type coverage struct {
	// admin is set for a member of an admin team.
	admin bool
	// roles holds each switched-on repository covered, with the highest
	// role of the teams covering it.
	roles map[RepoKey]Role
	// projects is the covered repositories as the resolver spells them, in
	// key order: what the platform check asks about.
	projects []string
	// scope identifies the set of covered repositories (see scopeOf): a
	// session whose stored access was checked for another scope is checked
	// again.
	scope string
}

// cover is the coverage teams give on instance, over the repositories
// switched on (repos, of every instance):
//   - an admin team covers every one of instance, as admin, whatever
//     repositories it lists;
//   - a team repository with no project covers every one of its instance,
//     and one with a project that one, ignoring case;
//   - repositories of other instances are left out;
//   - of the teams covering a repository, the highest role wins;
//   - a team with a role pergent.sh doesn't know gives nothing.
func cover(teams []store.Team, repos []settings.Effective, instance string) coverage {
	on := map[RepoKey]string{}
	for _, e := range repos {
		if e.Instance == instance {
			on[repoKey(e.Instance, e.Project)] = e.Project
		}
	}
	c := coverage{roles: map[RepoKey]Role{}}
	give := func(k RepoKey, r Role) {
		if _, ok := on[k]; ok && r > c.roles[k] {
			c.roles[k] = r
		}
	}
	for _, t := range knownTeams(teams) {
		role := roleOf(t.Role)
		if role == RoleAdmin {
			c.admin = true
			for k := range on {
				give(k, RoleAdmin)
			}
			continue
		}
		for _, tr := range t.Repos {
			switch {
			case tr.Instance != instance:
			case tr.Project == "":
				for k := range on {
					give(k, role)
				}
			default:
				give(repoKey(tr.Instance, tr.Project), role)
			}
		}
	}
	keys := make([]RepoKey, 0, len(c.roles))
	for k := range c.roles {
		keys = append(keys, k)
	}
	sortKeys(keys)
	for _, k := range keys {
		c.projects = append(c.projects, on[k])
	}
	c.scope = scopeOf(keys)
	return c
}

// scopeOf is the hex SHA-256 of keys, sorted, each as instance, NUL,
// lower-cased path and a newline: the same set gives the same scope in any
// order.
func scopeOf(keys []RepoKey) string {
	sorted := make([]RepoKey, len(keys))
	for i, k := range keys {
		sorted[i] = RepoKey{k.Instance, lower(k.Path)}
	}
	sortKeys(sorted)
	h := sha256.New()
	for _, k := range sorted {
		h.Write([]byte(k.Instance + "\x00" + k.Path + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortKeys(keys []RepoKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Instance != keys[j].Instance {
			return keys[i].Instance < keys[j].Instance
		}
		return keys[i].Path < keys[j].Path
	})
}

// knownTeams is teams without those whose role isn't one pergent.sh knows:
// such a team is as none, for waiting as for coverage.
func knownTeams(teams []store.Team) []store.Team {
	out := make([]store.Team, 0, len(teams))
	for _, t := range teams {
		if roleOf(t.Role) != RoleNone {
			out = append(out, t)
		}
	}
	return out
}
