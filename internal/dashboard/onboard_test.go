package dashboard

import (
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// ownerMaintainer: ok only for a maintainer of the team that owns the
// repository, not of a team that merely covers it.
func TestOwnerMaintainer(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	owner := e.sso("owner@example.com")
	cover := e.sso("cover@example.com")
	inst := e.sso("instance@example.com")
	e.team("owners", []store.TeamMember{{AccountID: owner, Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "main", Project: "acme/widgets", Owner: true})
	e.team("covers", []store.TeamMember{{AccountID: cover, Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	e.team("instance owners", []store.TeamMember{{AccountID: inst, Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "gh", Owner: true})

	for _, c := range []struct {
		name              string
		account           int64
		instance, project string
		ok                bool
	}{
		{"maintainer of the owner team", owner, "main", "acme/widgets", true},
		{"maintainer of a covering team", cover, "main", "acme/widgets", false},
		{"maintainer of an instance-level owner", inst, "gh", "acme/widgets", true},
		{"repository with no owner", owner, "main", "acme/payments", false},
	} {
		got, ok, err := e.d.ownerMaintainer(ctx, Session{AccountID: c.account}, c.instance, c.project)
		if err != nil || ok != c.ok {
			t.Errorf("%s: ownerMaintainer = %d, %v, %v; want ok %v", c.name, got, ok, err, c.ok)
		}
	}
}
