package store

import (
	"errors"
	"testing"
	"time"
)

func mustPassword(t *testing.T, s *Store, username string) Account {
	t.Helper()
	a, err := s.CreatePasswordAccount(ctx, username, "hash-"+username, false, t0)
	if err != nil {
		t.Fatalf("CreatePasswordAccount(%s): %v", username, err)
	}
	return a
}

func mustPlatform(t *testing.T, s *Store, instance string, userID int64, username string) Account {
	t.Helper()
	a, err := s.UpsertPlatformAccount(ctx, "gitlab", instance, userID, username, t0)
	if err != nil {
		t.Fatalf("UpsertPlatformAccount(%s): %v", username, err)
	}
	return a
}

func TestPasswordAccountCaseInsensitive(t *testing.T) {
	s := newStore(t)
	a, err := s.CreatePasswordAccount(ctx, "Admin", "argon-hash", true, t0)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || a.Kind != AccountPassword || a.Username != "Admin" || a.PasswordHash != "argon-hash" ||
		!a.MustChangePassword || !a.CreatedAt.Equal(t0) || a.LastSignInAt != nil || a.LockedUntil != nil {
		t.Fatalf("account = %+v", a)
	}
	if _, err := s.CreatePasswordAccount(ctx, "admin", "other", false, t0); !errors.Is(err, ErrExists) {
		t.Fatalf("second CreatePasswordAccount = %v; want ErrExists", err)
	}
	got, found, err := s.PasswordAccount(ctx, "ADMIN")
	if err != nil || !found || got.ID != a.ID || got.Username != "Admin" {
		t.Fatalf("PasswordAccount(ADMIN) = %+v, %v, %v", got, found, err)
	}
	if _, found, err := s.PasswordAccount(ctx, "nobody"); found || err != nil {
		t.Fatalf("PasswordAccount(nobody) = %v, %v", found, err)
	}
	// A platform account of the same name is not a password account.
	mustPlatform(t, s, "main", 5, "carol")
	if _, found, _ := s.PasswordAccount(ctx, "carol"); found {
		t.Fatal("PasswordAccount found a platform account")
	}
	if n, err := s.CountPasswordAccounts(ctx); err != nil || n != 1 {
		t.Fatalf("CountPasswordAccounts = %d, %v; want 1", n, err)
	}
}

func TestPasswordAccountNeedsHash(t *testing.T) {
	s := newStore(t)
	_, err := s.CreatePasswordAccount(ctx, "admin", "", false, t0)
	if err == nil || errors.Is(err, ErrExists) {
		t.Fatalf("CreatePasswordAccount with an empty hash = %v; want an error other than ErrExists", err)
	}
	if n, _ := s.CountPasswordAccounts(ctx); n != 0 {
		t.Fatalf("accounts = %d; want 0", n)
	}
}

func TestDeleteLastPasswordAccount(t *testing.T) {
	s := newStore(t)
	first := mustPassword(t, s, "admin")
	if ok, err := s.DeleteAccount(ctx, first.ID); !errors.Is(err, ErrLastPasswordAccount) || ok {
		t.Fatalf("DeleteAccount of the last = %v, %v; want ErrLastPasswordAccount", ok, err)
	}
	if _, found, _ := s.Account(ctx, first.ID); !found {
		t.Fatal("the last password account was deleted")
	}
	second := mustPassword(t, s, "ops")
	if ok, err := s.DeleteAccount(ctx, first.ID); err != nil || !ok {
		t.Fatalf("DeleteAccount with two = %v, %v", ok, err)
	}
	if ok, err := s.DeleteAccount(ctx, first.ID); err != nil || ok {
		t.Fatalf("DeleteAccount again = %v, %v; want false", ok, err)
	}
	if ok, err := s.DeleteAccount(ctx, second.ID); !errors.Is(err, ErrLastPasswordAccount) || ok {
		t.Fatalf("DeleteAccount of the new last = %v, %v", ok, err)
	}
	// A platform account is deleted whatever the password count.
	p := mustPlatform(t, s, "main", 5, "carol")
	if ok, err := s.DeleteAccount(ctx, p.ID); err != nil || !ok {
		t.Fatalf("DeleteAccount of a platform account = %v, %v", ok, err)
	}
}

func TestRecordFailedSignInLocks(t *testing.T) {
	s := newStore(t)
	a := mustPassword(t, s, "admin")
	now := t0.Add(time.Hour)
	for i := 1; i <= 4; i++ {
		locked, err := s.RecordFailedSignIn(ctx, a.ID, now, 5, 15*time.Minute)
		if err != nil || locked {
			t.Fatalf("failure %d: locked %v, %v", i, locked, err)
		}
	}
	got, _, _ := s.Account(ctx, a.ID)
	if got.FailedAttempts != 4 || got.LockedUntil != nil {
		t.Fatalf("after 4 failures = %+v", got)
	}
	locked, err := s.RecordFailedSignIn(ctx, a.ID, now, 5, 15*time.Minute)
	if err != nil || !locked {
		t.Fatalf("5th failure: locked %v, %v", locked, err)
	}
	got, _, _ = s.Account(ctx, a.ID)
	if got.FailedAttempts != 5 || got.LockedUntil == nil || !got.LockedUntil.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("after 5 failures = %+v", got)
	}
	// A failure during the lock does not push it out.
	if locked, err := s.RecordFailedSignIn(ctx, a.ID, now.Add(time.Minute), 5, 15*time.Minute); err != nil || !locked {
		t.Fatalf("failure while locked: %v, %v", locked, err)
	}
	if got, _, _ = s.Account(ctx, a.ID); !got.LockedUntil.Equal(now.Add(15 * time.Minute)) {
		t.Fatalf("lock moved to %v", got.LockedUntil)
	}

	if err := s.RecordSignIn(ctx, a.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Account(ctx, a.ID)
	if got.FailedAttempts != 0 || got.LockedUntil != nil || got.LastSignInAt == nil || !got.LastSignInAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("after RecordSignIn = %+v", got)
	}
}

// Failures from before a lock that has ended don't count towards the next.
func TestRecordFailedSignInRestartsAfterLock(t *testing.T) {
	s := newStore(t)
	a := mustPassword(t, s, "admin")
	for i := 0; i < 5; i++ {
		s.RecordFailedSignIn(ctx, a.ID, t0, 5, time.Minute) //nolint:errcheck // checked by the next call
	}
	locked, err := s.RecordFailedSignIn(ctx, a.ID, t0.Add(2*time.Minute), 5, time.Minute)
	if err != nil || locked {
		t.Fatalf("first failure after the lock ended: locked %v, %v", locked, err)
	}
	if got, _, _ := s.Account(ctx, a.ID); got.FailedAttempts != 1 || got.LockedUntil != nil {
		t.Fatalf("account = %+v; want 1 failure and no lock", got)
	}
}

func TestSetPasswordClearsFailures(t *testing.T) {
	s := newStore(t)
	a := mustPassword(t, s, "admin")
	for i := 0; i < 5; i++ {
		s.RecordFailedSignIn(ctx, a.ID, t0, 5, time.Hour) //nolint:errcheck // checked below
	}
	if err := s.SetPassword(ctx, a.ID, "new-hash", true); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Account(ctx, a.ID)
	if got.PasswordHash != "new-hash" || !got.MustChangePassword || got.FailedAttempts != 0 || got.LockedUntil != nil {
		t.Fatalf("account = %+v", got)
	}
	p := mustPlatform(t, s, "main", 5, "carol")
	if err := s.SetPassword(ctx, p.ID, "x", false); err == nil {
		t.Error("SetPassword on a platform account = nil error")
	}
}

func TestUpsertPlatformAccount(t *testing.T) {
	s := newStore(t)
	a, err := s.UpsertPlatformAccount(ctx, "gitlab", "main", 42, "alice", t0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != AccountPlatform || a.Platform != "gitlab" || a.Instance != "main" || a.PlatformUserID != 42 ||
		a.Username != "alice" || a.PasswordHash != "" || a.LastSignInAt == nil || !a.LastSignInAt.Equal(t0) || !a.CreatedAt.Equal(t0) {
		t.Fatalf("account = %+v", a)
	}
	later := t0.Add(time.Hour)
	b, err := s.UpsertPlatformAccount(ctx, "gitlab", "main", 42, "alice-renamed", later)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID || b.Username != "alice-renamed" || !b.LastSignInAt.Equal(later) || !b.CreatedAt.Equal(t0) {
		t.Fatalf("second upsert = %+v; want the same row renamed", b)
	}
	all, _ := s.Accounts(ctx)
	if len(all) != 1 {
		t.Fatalf("Accounts = %d rows; want 1", len(all))
	}
	// The same user ID on another instance is another account.
	c, err := s.UpsertPlatformAccount(ctx, "gitlab", "other", 42, "alice", t0)
	if err != nil || c.ID == a.ID {
		t.Fatalf("other instance: %+v, %v", c, err)
	}
}

func TestAccountsWaitingFirst(t *testing.T) {
	s := newStore(t)
	admin := mustPassword(t, s, "admin")
	member := mustPlatform(t, s, "main", 1, "alice")
	waiting := mustPlatform(t, s, "main", 2, "zed")
	team, err := s.CreateTeam(ctx, "devs", RoleViewer, t0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UpdateTeam(ctx, team.ID, func(tm *Team) error { tm.Members = []int64{member.ID}; return nil }); err != nil || !ok {
		t.Fatalf("UpdateTeam = %v, %v", ok, err)
	}
	all, err := s.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, a := range all {
		ids = append(ids, a.ID)
	}
	// zed has no team, so it comes first; then alice and admin by username.
	want := []int64{waiting.ID, admin.ID, member.ID}
	if len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("order = %v; want %v", ids, want)
	}
}

func TestDeleteAccountCascades(t *testing.T) {
	s := newStore(t)
	mustPassword(t, s, "admin")
	a := mustPlatform(t, s, "main", 1, "alice")
	se := newSession(t, s, 1, t0)
	se.AccountID = a.ID
	if err := s.CreateSession(ctx, se); err != nil {
		t.Fatal(err)
	}
	team, _ := s.CreateTeam(ctx, "devs", RoleViewer, t0)
	if _, err := s.UpdateTeam(ctx, team.ID, func(tm *Team) error { tm.Members = []int64{a.ID}; return nil }); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteAccount(ctx, a.ID); err != nil || !ok {
		t.Fatalf("DeleteAccount = %v, %v", ok, err)
	}
	if _, found, _ := s.Session(ctx, se.IDHash); found {
		t.Error("the account's session is still there")
	}
	got, _, _ := s.Team(ctx, team.ID)
	if len(got.Members) != 0 {
		t.Errorf("members after the delete = %v", got.Members)
	}
}

func TestDeleteAccountSessions(t *testing.T) {
	s := newStore(t)
	a := mustPlatform(t, s, "main", 1, "alice")
	b := mustPlatform(t, s, "main", 2, "bob")
	var all []Session
	for i, acct := range []int64{a.ID, a.ID, b.ID} {
		se := newSession(t, s, byte(i+1), t0)
		se.AccountID = acct
		if err := s.CreateSession(ctx, se); err != nil {
			t.Fatal(err)
		}
		all = append(all, se)
	}
	keep := all[0].IDHash
	if err := s.DeleteAccountSessions(ctx, a.ID, keep); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false, true} {
		if _, found, _ := s.Session(ctx, all[i].IDHash); found != want {
			t.Errorf("session %d found = %v; want %v", i, found, want)
		}
	}
	if err := s.DeleteAccountSessions(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.Session(ctx, keep); found {
		t.Error("nil keep left a session")
	}
	if _, found, _ := s.Session(ctx, all[2].IDHash); !found {
		t.Error("another account's session was deleted")
	}
}
