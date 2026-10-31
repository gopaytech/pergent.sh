package store

import (
	"errors"
	"slices"
	"testing"
)

func TestCreateTeam(t *testing.T) {
	s := newStore(t)
	tm, err := s.CreateTeam(ctx, "Platform", RoleMaintainer, t0)
	if err != nil {
		t.Fatal(err)
	}
	if tm.ID == 0 || tm.Name != "Platform" || tm.Role != RoleMaintainer || !tm.CreatedAt.Equal(t0) || len(tm.Members) != 0 || len(tm.Repos) != 0 {
		t.Fatalf("team = %+v", tm)
	}
	if _, err := s.CreateTeam(ctx, "platform", RoleViewer, t0); !errors.Is(err, ErrExists) {
		t.Fatalf("same name in another case = %v; want ErrExists", err)
	}
	if _, err := s.CreateTeam(ctx, "ops", "owner", t0); err == nil {
		t.Error("CreateTeam with an unknown role = nil error")
	}
}

func TestUpdateTeamReplaces(t *testing.T) {
	s := newStore(t)
	a := mustPlatform(t, s, "main", 1, "alice")
	b := mustPlatform(t, s, "main", 2, "bob")
	tm, _ := s.CreateTeam(ctx, "devs", RoleViewer, t0)

	ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error {
		t.Name, t.Role = "developers", RoleMaintainer
		t.Members = []int64{b.ID, a.ID, a.ID}
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
	if got.Name != "developers" || got.Role != RoleMaintainer || !slices.Equal(got.Members, []int64{a.ID, b.ID}) ||
		!slices.Equal(got.Repos, []TeamRepo{{Instance: "main"}, {Instance: "main", Project: "acme/widgets"}}) {
		t.Fatalf("team = %+v (members and repos deduplicated and sorted; an empty project is the whole instance)", got)
	}
	var project string
	if err := s.pool.QueryRow(ctx, `SELECT project FROM team_repos WHERE team_id = $1 AND instance = 'main' ORDER BY project LIMIT 1`, tm.ID).Scan(&project); err != nil || project != "" {
		t.Fatalf("stored project = %q, %v; want ''", project, err)
	}

	// A second update replaces what the first set.
	if ok, err := s.UpdateTeam(ctx, tm.ID, func(t *Team) error {
		t.Members = []int64{b.ID}
		t.Repos = []TeamRepo{{Instance: "other", Project: "acme/tools"}}
		return nil
	}); err != nil || !ok {
		t.Fatalf("second UpdateTeam = %v, %v", ok, err)
	}
	got, _, _ = s.Team(ctx, tm.ID)
	if !slices.Equal(got.Members, []int64{b.ID}) || !slices.Equal(got.Repos, []TeamRepo{{Instance: "other", Project: "acme/tools"}}) {
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

	// A taken name is ErrExists; a bad role is an error.
	other, _ := s.CreateTeam(ctx, "ops", RoleViewer, t0)
	if _, err := s.UpdateTeam(ctx, other.ID, func(t *Team) error { t.Name = "DEVELOPERS"; return nil }); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto a taken name = %v; want ErrExists", err)
	}
	if _, err := s.UpdateTeam(ctx, other.ID, func(t *Team) error { t.Role = "owner"; return nil }); err == nil {
		t.Error("UpdateTeam with an unknown role = nil error")
	}
}

func TestTeamsListAndDelete(t *testing.T) {
	s := newStore(t)
	a := mustPlatform(t, s, "main", 1, "alice")
	t1, _ := s.CreateTeam(ctx, "zeta", RoleViewer, t0)
	t2, _ := s.CreateTeam(ctx, "Alpha", RoleAdmin, t0)
	if _, err := s.UpdateTeam(ctx, t1.ID, func(t *Team) error {
		t.Members = []int64{a.ID}
		t.Repos = []TeamRepo{{Instance: "main", Project: "acme/widgets"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Teams(ctx)
	if err != nil || len(all) != 2 || all[0].ID != t2.ID || all[1].ID != t1.ID {
		t.Fatalf("Teams = %+v, %v; want Alpha then zeta", all, err)
	}
	if !slices.Equal(all[1].Members, []int64{a.ID}) || len(all[1].Repos) != 1 {
		t.Fatalf("zeta = %+v", all[1])
	}
	if _, found, _ := s.Team(ctx, 9999); found {
		t.Error("Team of an unknown ID found")
	}
	if ok, err := s.DeleteTeam(ctx, t1.ID); err != nil || !ok {
		t.Fatalf("DeleteTeam = %v, %v", ok, err)
	}
	if ok, err := s.DeleteTeam(ctx, t1.ID); err != nil || ok {
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
	a := mustPlatform(t, s, "main", 1, "alice")
	b := mustPlatform(t, s, "main", 2, "bob")
	t1, _ := s.CreateTeam(ctx, "one", RoleViewer, t0)
	t2, _ := s.CreateTeam(ctx, "two", RoleMaintainer, t0)
	t3, _ := s.CreateTeam(ctx, "three", RoleAdmin, t0)
	set := func(id int64, members ...int64) {
		t.Helper()
		if _, err := s.UpdateTeam(ctx, id, func(t *Team) error {
			t.Members = members
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
	if !slices.Equal(got[1].Members, []int64{a.ID, b.ID}) || len(got[1].Repos) != 1 {
		t.Fatalf("team two = %+v (members and repos are loaded)", got[1])
	}
	if got, err := s.AccountTeams(ctx, 9999); err != nil || len(got) != 0 {
		t.Fatalf("AccountTeams of nobody = %+v, %v", got, err)
	}
}
