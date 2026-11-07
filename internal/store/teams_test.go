package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// viewers makes the viewer members for the accounts ids.
func viewers(ids ...int64) []TeamMember {
	out := make([]TeamMember, len(ids))
	for i, id := range ids {
		out[i] = TeamMember{AccountID: id, Role: RoleViewer}
	}
	return out
}

func memberIDs(ms []TeamMember) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.AccountID
	}
	return out
}

func TestCreateTeam(t *testing.T) {
	s := newStore(t)
	tm, err := s.CreateTeam(ctx, "Platform", t0)
	if err != nil {
		t.Fatal(err)
	}
	if tm.ID == 0 || tm.Name != "Platform" || !tm.CreatedAt.Equal(t0) || len(tm.Members) != 0 || len(tm.Repos) != 0 {
		t.Fatalf("team = %+v", tm)
	}
	if _, err := s.CreateTeam(ctx, "platform", t0); !errors.Is(err, ErrExists) {
		t.Fatalf("same name in another case = %v; want ErrExists", err)
	}
	// A team has no role of its own; the unused column (NOT NULL, dropped by a
	// later release) is written "viewer", and an update leaves it be.
	var role string
	if err := s.pool.QueryRow(ctx, `SELECT role FROM teams WHERE id = $1`, tm.ID).Scan(&role); err != nil || role != RoleViewer {
		t.Fatalf("teams.role = %q, %v; want viewer", role, err)
	}
	if _, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error { t.Name = "Platform 2"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT role FROM teams WHERE id = $1`, tm.ID).Scan(&role); err != nil || role != RoleViewer {
		t.Fatalf("teams.role after an update = %q, %v; want viewer", role, err)
	}
}

func TestUpdateTeamReplaces(t *testing.T) {
	s := newStore(t)
	a := mustSSO(t, s, "alice@example.com")
	b := mustSSO(t, s, "bob@example.com")
	tm, _ := s.CreateTeam(ctx, "devs", t0)

	ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error {
		t.Name = "developers"
		t.Members = viewers(b.ID, a.ID, a.ID)
		t.Repos = []TeamRepo{{Instance: "main"}, {Instance: "main", Project: "acme/widgets"}, {Instance: "main", Project: "ACME/Widgets"}}
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("UpdateTeam = %v, %v", ok, err)
	}
	got, found, err := s.Team(ctx, tm.ID)
	if err != nil || !found {
		t.Fatalf("Team = %v, %v", found, err)
	}
	if got.Name != "developers" || !slices.Equal(memberIDs(got.Members), []int64{a.ID, b.ID}) ||
		!slices.Equal(got.Repos, []TeamRepo{{Instance: "main"}, {Instance: "main", Project: "acme/widgets"}}) {
		t.Fatalf("team = %+v (members and repos deduplicated and sorted; an empty project is the whole instance)", got)
	}
	var project string
	if err := s.pool.QueryRow(ctx, `SELECT project FROM team_repos WHERE team_id = $1 AND instance = 'main' ORDER BY project LIMIT 1`, tm.ID).Scan(&project); err != nil || project != "" {
		t.Fatalf("stored project = %q, %v; want ''", project, err)
	}

	// A second update replaces what the first set.
	if ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error {
		t.Members = viewers(b.ID)
		t.Repos = []TeamRepo{{Instance: "other", Project: "acme/tools"}}
		return nil
	}); err != nil || !ok {
		t.Fatalf("second UpdateTeam = %v, %v", ok, err)
	}
	got, _, _ = s.Team(ctx, tm.ID)
	if !slices.Equal(memberIDs(got.Members), []int64{b.ID}) || !slices.Equal(got.Repos, []TeamRepo{{Instance: "other", Project: "acme/tools"}}) {
		t.Fatalf("team after the second update = %+v", got)
	}

	// The callback's error aborts the update.
	boom := errors.New("boom")
	if ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error { t.Name = "changed"; t.Members = nil; return boom }); !errors.Is(err, boom) || ok {
		t.Fatalf("UpdateTeam with a failing callback = %v, %v", ok, err)
	}
	if again, _, _ := s.Team(ctx, tm.ID); again.Name != "developers" || len(again.Members) != 1 {
		t.Fatalf("team after an aborted update = %+v", again)
	}
	if ok, err := s.UpdateTeam(ctx, 9999, func(*Team) error { return nil }); err != nil || ok {
		t.Fatalf("UpdateTeam of an unknown team = %v, %v", ok, err)
	}

	// A taken name is ErrExists.
	other, _ := s.CreateTeam(ctx, "ops", t0)
	if _, err := s.UpdateTeam(ctx, other.ID, func(t *Team) error { t.Name = "DEVELOPERS"; return nil }); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto a taken name = %v; want ErrExists", err)
	}
}

func TestTeamsListAndDelete(t *testing.T) {
	s := newStore(t)
	a := mustSSO(t, s, "alice@example.com")
	t1, _ := s.CreateTeam(ctx, "zeta", t0)
	t2, _ := s.CreateTeam(ctx, "Alpha", t0)
	if _, err := s.UpdateTeam(ctx, t1.ID, func(t *Team) error {
		t.Members = viewers(a.ID)
		t.Repos = []TeamRepo{{Instance: "main", Project: "acme/widgets"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Teams(ctx)
	if err != nil || len(all) != 2 || all[0].ID != t2.ID || all[1].ID != t1.ID {
		t.Fatalf("Teams = %+v, %v; want Alpha then zeta", all, err)
	}
	if !slices.Equal(memberIDs(all[1].Members), []int64{a.ID}) || len(all[1].Repos) != 1 {
		t.Fatalf("zeta = %+v", all[1])
	}
	if _, found, _ := s.Team(ctx, 9999); found {
		t.Error("Team of an unknown ID found")
	}
	if ok, err := s.DeleteTeam(ctx, t1.ID, nil); err != nil || !ok {
		t.Fatalf("DeleteTeam = %v, %v", ok, err)
	}
	if ok, err := s.DeleteTeam(ctx, t1.ID, nil); err != nil || ok {
		t.Fatalf("DeleteTeam again = %v, %v", ok, err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM team_members) + (SELECT count(*) FROM team_repos)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left after DeleteTeam = %d, %v", n, err)
	}
	if got, _, _ := s.Account(ctx, a.ID); got.ID != a.ID {
		t.Error("DeleteTeam deleted a member account")
	}
}

func TestAccountTeams(t *testing.T) {
	s := newStore(t)
	a := mustSSO(t, s, "alice@example.com")
	b := mustSSO(t, s, "bob@example.com")
	t1, _ := s.CreateTeam(ctx, "one", t0)
	t2, _ := s.CreateTeam(ctx, "two", t0)
	t3, _ := s.CreateTeam(ctx, "three", t0)
	set := func(id int64, members ...int64) {
		t.Helper()
		if _, err := s.UpdateTeam(ctx, id, func(t *Team) error {
			t.Members = viewers(members...)
			t.Repos = []TeamRepo{{Instance: "main"}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	set(t1.ID, a.ID)
	set(t2.ID, a.ID, b.ID)
	set(t3.ID, b.ID)

	got, err := s.AccountTeams(ctx, a.ID)
	if err != nil || len(got) != 2 || got[0].ID != t1.ID || got[1].ID != t2.ID {
		t.Fatalf("AccountTeams(alice) = %+v, %v; want one, two", got, err)
	}
	if !slices.Equal(memberIDs(got[1].Members), []int64{a.ID, b.ID}) || len(got[1].Repos) != 1 {
		t.Fatalf("team two = %+v (members and repos are loaded)", got[1])
	}
	if got, err := s.AccountTeams(ctx, 9999); err != nil || len(got) != 0 {
		t.Fatalf("AccountTeams of nobody = %+v, %v", got, err)
	}
}

// A member that isn't an account (any more) is ErrUnknownAccount, and
// nothing of the update is kept.
func TestUpdateTeamUnknownMember(t *testing.T) {
	s := newStore(t)
	a := mustSSO(t, s, "alice@example.com")
	tm, _ := s.CreateTeam(ctx, "devs", t0)
	if _, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error { t.Members = viewers(a.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error { t.Name = "renamed"; t.Members = viewers(a.ID, 99999); return nil })
	if !errors.Is(err, ErrUnknownAccount) || ok {
		t.Fatalf("UpdateTeam with an unknown member = %v, %v; want ErrUnknownAccount", ok, err)
	}
	if got, _, _ := s.Team(ctx, tm.ID); got.Name != "devs" || !slices.Equal(memberIDs(got.Members), []int64{a.ID}) {
		t.Errorf("team after the refused update = %+v", got)
	}
}

// DeleteTeam hands the team to check under a row lock: an error from it
// keeps the team, and a change in flight is seen by it.
func TestDeleteTeamChecked(t *testing.T) {
	s := newStore(t)
	tm, _ := s.CreateTeam(ctx, "devs", t0)

	boom := errors.New("boom")
	if ok, err := s.DeleteTeam(ctx, tm.ID, func(Team) error { return boom }); !errors.Is(err, boom) || ok {
		t.Fatalf("DeleteTeam with a failing check = %v, %v", ok, err)
	}
	if _, found, _ := s.Team(ctx, tm.ID); !found {
		t.Fatal("a refused DeleteTeam removed the team")
	}
	if ok, err := s.DeleteTeam(ctx, 9999, func(Team) error { t.Error("check called for an unknown team"); return nil }); err != nil || ok {
		t.Fatalf("DeleteTeam of an unknown team = %v, %v", ok, err)
	}

	// An update holding the lock: the delete waits, then checks what the
	// update left.
	inUpdate, release := make(chan struct{}), make(chan struct{})
	updated := make(chan error, 1)
	go func() {
		_, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error {
			close(inUpdate)
			<-release
			t.Repos = []TeamRepo{{Instance: "main", Project: "acme/widgets"}}
			return nil
		})
		updated <- err
	}()
	<-inUpdate
	var saw []TeamRepo
	deleted := make(chan error, 1)
	go func() {
		_, err := s.DeleteTeam(ctx, tm.ID, func(t Team) error { saw = t.Repos; return boom })
		deleted <- err
	}()
	select {
	case err := <-deleted:
		t.Fatalf("DeleteTeam finished (%v) while an update held the team", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; !errors.Is(err, boom) {
		t.Fatalf("DeleteTeam = %v", err)
	}
	if !slices.Equal(saw, []TeamRepo{{Instance: "main", Project: "acme/widgets"}}) {
		t.Errorf("check saw %+v; want the repository the update added", saw)
	}
}

// A member has a role on the team: viewer unless it says maintainer. The
// roles come back on Team and on AccountTeams, an account named twice is one
// member with the higher role, and a platform account can't be a member.
func TestTeamMemberRoles(t *testing.T) {
	s := newStore(t)
	a := mustSSO(t, s, "alice@example.com")
	b := mustSSO(t, s, "bob@example.com")
	c := mustSSO(t, s, "carol@example.com")
	tm, err := s.CreateTeam(ctx, "devs", t0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error {
		t.Members = []TeamMember{{a.ID, RoleMaintainer}, {b.ID, ""}, {c.ID, RoleViewer}, {c.ID, RoleMaintainer}, {c.ID, RoleViewer}}
		return nil
	}); err != nil || !ok {
		t.Fatalf("UpdateTeam = %v, %v", ok, err)
	}
	want := []TeamMember{{a.ID, RoleMaintainer}, {b.ID, RoleViewer}, {c.ID, RoleMaintainer}}
	got, _, _ := s.Team(ctx, tm.ID)
	if !slices.Equal(got.Members, want) {
		t.Fatalf("members = %+v; want %+v", got.Members, want)
	}
	teams, err := s.AccountTeams(ctx, b.ID)
	if err != nil || len(teams) != 1 || !slices.Equal(teams[0].Members, want) {
		t.Fatalf("AccountTeams = %+v, %v", teams, err)
	}

	// A role can change; the same account keeps one row.
	if ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error {
		t.Members = []TeamMember{{a.ID, RoleViewer}, {b.ID, RoleMaintainer}}
		return nil
	}); err != nil || !ok {
		t.Fatalf("second UpdateTeam = %v, %v", ok, err)
	}
	got, _, _ = s.Team(ctx, tm.ID)
	if want := []TeamMember{{a.ID, RoleViewer}, {b.ID, RoleMaintainer}}; !slices.Equal(got.Members, want) {
		t.Fatalf("members after the change = %+v; want %+v", got.Members, want)
	}

	// A bad role is an error and keeps the team as it was; so does an
	// account that is not one, or is a platform account.
	platform := mustPlatform(t, s, "main", 1, "pat")
	for name, members := range map[string][]TeamMember{
		"admin role":      {{a.ID, RoleAdmin}},
		"unknown role":    {{a.ID, "owner"}},
		"unknown account": {{a.ID, RoleViewer}, {99999, RoleViewer}},
		"platform":        {{a.ID, RoleViewer}, {platform.ID, RoleViewer}},
	} {
		ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error { t.Name = "renamed"; t.Members = members; return nil })
		if err == nil || ok {
			t.Errorf("%s: UpdateTeam = %v, %v; want an error", name, ok, err)
		}
	}
	if err := func() error {
		_, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error { t.Members = viewers(99999); return nil })
		return err
	}(); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("unknown account = %v; want ErrUnknownAccount", err)
	}
	if again, _, _ := s.Team(ctx, tm.ID); again.Name != "devs" || len(again.Members) != 2 || again.Members[1].Role != RoleMaintainer {
		t.Fatalf("team after the refused updates = %+v", again)
	}

	// The stored role is checked by the database too, and a row written the
	// old way (no role) reads as a viewer.
	if _, err := s.pool.Exec(ctx, `UPDATE team_members SET role = 'admin'`); err == nil {
		t.Error("the database took a team member role of admin")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO team_members (team_id, account_id) VALUES ($1, $2)`, tm.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Team(ctx, tm.ID); got.Members[2] != (TeamMember{c.ID, RoleViewer}) {
		t.Fatalf("a member row with no role = %+v; want viewer", got.Members)
	}
}

// Platform accounts stay on a team in the database (the previous release
// reads them) but are never listed, and a change to the team leaves them.
func TestTeamKeepsPlatformMembershipsUnlisted(t *testing.T) {
	s := newStore(t)
	p := mustPlatform(t, s, "main", 1, "pat")
	a := mustSSO(t, s, "alice@example.com")
	tm, _ := s.CreateTeam(ctx, "devs", t0)
	addPlatformMember(t, s, tm.ID, p.ID)
	if got, _, _ := s.Team(ctx, tm.ID); len(got.Members) != 0 {
		t.Fatalf("members = %+v; want none (the platform member is hidden)", got.Members)
	}
	if ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error { t.Members = viewers(a.ID); return nil }); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error { t.Members = nil; return nil }); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM team_members WHERE team_id = $1 AND account_id = $2`, tm.ID, p.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("platform membership rows = %d, %v; want 1 kept", n, err)
	}
}
