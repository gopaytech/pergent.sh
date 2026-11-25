package store

import (
	"errors"
	"slices"
	"sync"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func setOwner(t *testing.T, s *Store, teamID int64, repos ...TeamRepo) error {
	t.Helper()
	_, err := s.UpdateTeam(ctx, teamID, func(tm *Team) error { tm.Repos = repos; return nil })
	return err
}

// The owner of the repository entry wins over the owner of the instance
// entry.
func TestOwnerMostSpecific(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, _ := s.CreateTeam(ctx, "Platform", t0)
	b, _ := s.CreateTeam(ctx, "Payments", t0)
	if err := setOwner(t, s, a.ID, TeamRepo{Instance: "main", Owner: true}); err != nil {
		t.Fatal(err)
	}
	if err := setOwner(t, s, b.ID, TeamRepo{Instance: "main", Project: "acme/api", Owner: true}, TeamRepo{Instance: "main", Project: "acme/web"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Team(ctx, b.ID)
	if !slices.Equal(got.Repos, []TeamRepo{{Instance: "main", Project: "acme/api", Owner: true}, {Instance: "main", Project: "acme/web"}}) {
		t.Errorf("repos = %+v; want the owner tick read back", got.Repos)
	}
	for _, c := range []struct {
		instance, project string
		want              int64
		ok                bool
	}{
		{"main", "ACME/api", b.ID, true},
		{"main", "acme/web", a.ID, true}, // b covers it but does not own it
		{"main", "acme/other", a.ID, true},
		{"other", "acme/api", 0, false},
	} {
		id, ok, err := s.OwnerTeam(ctx, c.instance, c.project)
		if err != nil || id != c.want || ok != c.ok {
			t.Errorf("OwnerTeam(%s, %s) = %d, %v, %v; want %d, %v", c.instance, c.project, id, ok, err, c.want, c.ok)
		}
	}
	// A save of the team that leaves the tick keeps it.
	if _, err := s.UpdateTeam(ctx, b.ID, func(tm *Team) error { tm.Name = "Payments team"; return nil }); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := s.OwnerTeam(ctx, "main", "acme/api"); id != b.ID {
		t.Errorf("owner after a rename = %d; want %d", id, b.ID)
	}
}

// A second owner at the same level is refused with the owner's name.
func TestOwnerTakenError(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, _ := s.CreateTeam(ctx, "Payments", t0)
	b, _ := s.CreateTeam(ctx, "Growth", t0)
	if err := setOwner(t, s, a.ID, TeamRepo{Instance: "main", Project: "payments/api", Owner: true}, TeamRepo{Instance: "lab", Owner: true}); err != nil {
		t.Fatal(err)
	}
	err := setOwner(t, s, b.ID, TeamRepo{Instance: "main", Project: "payments/api", Owner: true})
	var taken *OwnerTakenError
	if !errors.As(err, &taken) || err.Error() != "payments/api already has owner Payments" {
		t.Errorf("second owner = %v; want payments/api already has owner Payments", err)
	}
	err = setOwner(t, s, b.ID, TeamRepo{Instance: "lab", Owner: true})
	if !errors.As(err, &taken) || err.Error() != "lab (whole instance) already has owner Payments" {
		t.Errorf("second instance owner = %v", err)
	}
	if got, _, _ := s.Team(ctx, b.ID); len(got.Repos) != 0 {
		t.Errorf("refused team repos = %+v; want nothing stored", got.Repos)
	}
	// Covering it without the tick is fine; so is a tick at another level.
	if err := setOwner(t, s, b.ID, TeamRepo{Instance: "main", Project: "payments/api"}, TeamRepo{Instance: "main", Owner: true}); err != nil {
		t.Errorf("cover without owning: %v", err)
	}
}

func TestClaimOwnerSetsTick(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, _ := s.CreateTeam(ctx, "A", t0)
	ch, err := s.ClaimOwner(ctx, "main", "g/r", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok, _ := s.OwnerTeam(ctx, "main", "g/r"); !ok || id != a.ID {
		t.Errorf("OwnerTeam = %d, %v; want %d", id, ok, a.ID)
	}
	got, _, _ := s.Team(ctx, a.ID)
	if !slices.Equal(got.Repos, []TeamRepo{{Instance: "main", Project: "g/r", Owner: true}}) {
		t.Errorf("repos = %+v; want 1 ticked entry", got.Repos)
	}
	if len(ch.Before.Repos) != 0 || !slices.Equal(ch.After.Repos, got.Repos) {
		t.Errorf("change = %+v", ch)
	}
	if _, err := s.ClaimOwner(ctx, "main", "g/r", a.ID+99); !errors.Is(err, ErrUnknownTeam) {
		t.Errorf("unknown team = %v; want ErrUnknownTeam", err)
	}
}

func TestClaimOwnerKeepsExistingEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, _ := s.CreateTeam(ctx, "A", t0)
	if err := setOwner(t, s, a.ID, TeamRepo{Instance: "main", Project: "G/R"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimOwner(ctx, "main", "g/r", a.ID); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Team(ctx, a.ID)
	if !slices.Equal(got.Repos, []TeamRepo{{Instance: "main", Project: "G/R", Owner: true}}) {
		t.Errorf("repos = %+v; want the one entry, ticked", got.Repos)
	}
}

func TestClaimOwnerRefusedRepoOwner(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, _ := s.CreateTeam(ctx, "A", t0)
	b, _ := s.CreateTeam(ctx, "B", t0)
	if err := setOwner(t, s, b.ID, TeamRepo{Instance: "main", Project: "g/r", Owner: true}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ClaimOwner(ctx, "main", "G/R", a.ID)
	var taken *OwnerTakenError
	if !errors.As(err, &taken) || taken.Team != "B" || taken.Project != "g/r" {
		t.Fatalf("err = %v; want *OwnerTakenError for B", err)
	}
	if got, _, _ := s.Team(ctx, a.ID); len(got.Repos) != 0 {
		t.Errorf("repos = %+v; want none written", got.Repos)
	}
}

func TestClaimOwnerRefusesEmptyProject(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, _ := s.CreateTeam(ctx, "A", t0)
	if _, err := s.ClaimOwner(ctx, "main", "", a.ID); err == nil {
		t.Fatal("err = nil; want a refusal")
	}
	if got, _, _ := s.Team(ctx, a.ID); len(got.Repos) != 0 {
		t.Errorf("repos = %+v; want none written", got.Repos)
	}
}

func TestClaimOwnerRefusedInstanceOwner(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, _ := s.CreateTeam(ctx, "A", t0)
	b, _ := s.CreateTeam(ctx, "B", t0)
	if err := setOwner(t, s, b.ID, TeamRepo{Instance: "main", Owner: true}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ClaimOwner(ctx, "main", "g/r", a.ID)
	var taken *OwnerTakenError
	if !errors.As(err, &taken) || taken.Team != "B" || taken.Project != "" {
		t.Fatalf("err = %v; want *OwnerTakenError for B with no project", err)
	}
}

func TestClaimOwnerRace(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, _ := s.CreateTeam(ctx, "A", t0)
	b, _ := s.CreateTeam(ctx, "B", t0)
	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, id := range []int64{a.ID, b.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = s.ClaimOwner(ctx, "main", "g/r", id)
		}()
	}
	close(start)
	wg.Wait()
	nils, takens := 0, 0
	for _, err := range errs {
		var taken *OwnerTakenError
		switch {
		case err == nil:
			nils++
		case errors.As(err, &taken):
			takens++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if nils != 1 || takens != 1 {
		t.Fatalf("errs = %v; want exactly 1 nil and 1 *OwnerTakenError", errs)
	}
}

func TestReleaseOwner(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, _ := s.CreateTeam(ctx, "A", t0)
	if err := setOwner(t, s, a.ID, TeamRepo{Instance: "main", Project: "g/r", Owner: true}); err != nil {
		t.Fatal(err)
	}
	ch, released, err := s.ReleaseOwner(ctx, "main", "G/R", a.ID)
	if err != nil || !released {
		t.Fatalf("ReleaseOwner = %v, %v; want released", released, err)
	}
	if !ch.Before.Repos[0].Owner || ch.After.Repos[0].Owner {
		t.Errorf("change = %+v; want the tick cleared", ch)
	}
	got, _, _ := s.Team(ctx, a.ID)
	if !slices.Equal(got.Repos, []TeamRepo{{Instance: "main", Project: "g/r"}}) {
		t.Errorf("repos = %+v; want the entry kept without a tick", got.Repos)
	}
	if _, ok, _ := s.OwnerTeam(ctx, "main", "g/r"); ok {
		t.Errorf("OwnerTeam has an owner; want none")
	}
	if _, released, err := s.ReleaseOwner(ctx, "main", "g/r", a.ID); err != nil || released {
		t.Errorf("second ReleaseOwner = %v, %v; want not released", released, err)
	}
}

func TestInstanceOwners(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	a, _ := s.CreateTeam(ctx, "A", t0)
	b, _ := s.CreateTeam(ctx, "B", t0)
	if err := setOwner(t, s, a.ID, TeamRepo{Instance: "main", Project: "G/R", Owner: true}, TeamRepo{Instance: "main", Project: "g/x"}); err != nil {
		t.Fatal(err)
	}
	if err := setOwner(t, s, b.ID, TeamRepo{Instance: "main", Owner: true}, TeamRepo{Instance: "other", Project: "o/p", Owner: true}); err != nil {
		t.Fatal(err)
	}
	byPath, inst, err := s.InstanceOwners(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(byPath) != 1 || byPath["g/r"] != "A" || inst != "B" {
		t.Errorf("owners = %v, %q; want {g/r: A}, B", byPath, inst)
	}
}
