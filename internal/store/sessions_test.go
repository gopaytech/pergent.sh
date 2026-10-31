package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path"
	"slices"
	"testing"
	"time"
)

// newSession is a platform session of account alice (platform user 9 on
// instance main), which it creates when it isn't there yet.
func newSession(t *testing.T, s *Store, hashByte byte, created time.Time) Session {
	t.Helper()
	a, err := s.UpsertPlatformAccount(ctx, "gitlab", "main", 9, "alice", t0)
	if err != nil {
		t.Fatal(err)
	}
	return Session{
		IDHash: bytes.Repeat([]byte{hashByte}, 32), AccountID: a.ID, Platform: "gitlab", Instance: "main",
		UserID: 9, Username: "alice", TokenSealed: []byte("sealed"), CSRF: "csrf-token",
		LastSeenAt: created, CreatedAt: created,
	}
}

func TestSessionsCRUD(t *testing.T) {
	s := newStore(t)
	in := newSession(t, s, 1, t0)
	if err := s.CreateSession(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Session(ctx, bytes.Repeat([]byte{2}, 32)); found || err != nil {
		t.Fatalf("Session of an unknown hash = %v, %v", found, err)
	}
	got, found, err := s.Session(ctx, in.IDHash)
	if err != nil || !found {
		t.Fatalf("Session = %v, %v", found, err)
	}
	if !bytes.Equal(got.IDHash, in.IDHash) || got.Platform != "gitlab" || got.Instance != "main" || got.UserID != 9 ||
		got.Username != "alice" || got.AccessScope != "" || got.AccountID != in.AccountID || !bytes.Equal(got.TokenSealed, []byte("sealed")) || got.CSRF != "csrf-token" ||
		got.AccessCheckedAt != nil || !got.LastSeenAt.Equal(t0) || !got.CreatedAt.Equal(t0) {
		t.Fatalf("session = %+v", got)
	}

	if err := s.TouchSession(ctx, in.IDHash, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionToken(ctx, in.IDHash, []byte("resealed")); err != nil {
		t.Fatal(err)
	}
	checked := t0.Add(2 * time.Minute)
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice-renamed", "read", vis(7, "acme/widgets", "Acme/Widgets", "acme/payments"), checked); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Session(ctx, in.IDHash)
	if !got.LastSeenAt.Equal(t0.Add(time.Minute)) || !bytes.Equal(got.TokenSealed, []byte("resealed")) ||
		got.AccessScope != "read" || got.AccessCheckedAt == nil || !got.AccessCheckedAt.Equal(checked) || got.Username != "alice-renamed" {
		t.Fatalf("updated session = %+v", got)
	}
	projects, err := s.SessionProjects(ctx, in.IDHash)
	if err != nil || !slices.Equal(projects, []VisibleProject{{"main", "acme/payments", 7}, {"main", "acme/widgets", 7}}) {
		t.Fatalf("SessionProjects = %v, %v (a case duplicate is one project)", projects, err)
	}

	// A new check replaces the list.
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", "read", vis(8, "acme/payments"), checked.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if projects, _ := s.SessionProjects(ctx, in.IDHash); !slices.Equal(projects, []VisibleProject{{"main", "acme/payments", 8}}) {
		t.Fatalf("SessionProjects after a new check = %v", projects)
	}

	// Clearing denies everything but keeps the time of the last good check.
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", "read", vis(8, "acme/payments"), checked); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearSessionAccess(ctx, in.IDHash); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Session(ctx, in.IDHash)
	if projects, _ := s.SessionProjects(ctx, in.IDHash); len(projects) != 0 || got.AccessScope != "" || !got.AccessCheckedAt.Equal(checked) {
		t.Fatalf("after ClearSessionAccess: projects %v, session %+v", projects, got)
	}

	// Deleting removes the session and its projects.
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", "read", vis(7, "acme/widgets"), checked); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, in.IDHash); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.Session(ctx, in.IDHash); found {
		t.Fatal("session still there after DeleteSession")
	}
	if projects, _ := s.SessionProjects(ctx, in.IDHash); len(projects) != 0 {
		t.Fatalf("projects left after DeleteSession: %v", projects)
	}
	// Access set on a session that's gone is not stored.
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", "read", vis(7, "acme/widgets"), checked); err != nil {
		t.Fatalf("SetSessionAccess on a deleted session: %v", err)
	}
	if _, found, _ := s.Session(ctx, in.IDHash); found {
		t.Fatal("SetSessionAccess brought a deleted session back")
	}
}

func TestPruneSessions(t *testing.T) {
	s := newStore(t)
	now := t0.Add(30 * 24 * time.Hour)
	old := newSession(t, s, 1, now.Add(-8*24*time.Hour)) // created 8 days ago, seen just now
	old.LastSeenAt = now.Add(-time.Minute)
	idle := newSession(t, s, 2, now.Add(-time.Hour*10)) // seen 10 h ago
	fresh := newSession(t, s, 3, now.Add(-time.Hour))
	for _, se := range []Session{old, idle, fresh} {
		if err := s.CreateSession(ctx, se); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneSessions(ctx, now.Add(-7*24*time.Hour), now.Add(-8*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("PruneSessions = %d, %v; want 2", n, err)
	}
	if _, found, _ := s.Session(ctx, fresh.IDHash); !found {
		t.Fatal("the fresh session was pruned")
	}
}

// AddSessionProject adds one repository to a session's visible ones, once
// whatever its case, on the session's instance.
func TestAddSessionProject(t *testing.T) {
	s := newStore(t)
	in := newSession(t, s, 1, t0)
	if err := s.CreateSession(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", "read", vis(7, "acme/widgets"), t0); err != nil {
		t.Fatal(err)
	}
	for _, p := range []VisibleProject{{"main", "acme/gadgets", 11}, {"main", "ACME/Gadgets", 12}, {"main", "acme/widgets", 13}} {
		if err := s.AddSessionProject(ctx, in.IDHash, p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SessionProjects(ctx, in.IDHash)
	if err != nil || !slices.Equal(got, []VisibleProject{{"main", "acme/gadgets", 11}, {"main", "acme/widgets", 7}}) {
		t.Fatalf("projects = %v, %v", got, err)
	}
	// A session that is gone stays gone.
	if err := s.AddSessionProject(ctx, bytes.Repeat([]byte{9}, 32), VisibleProject{"main", "acme/gadgets", 11}); err != nil {
		t.Fatal(err)
	}
	// A project needs its ID.
	if err := s.AddSessionProject(ctx, in.IDHash, VisibleProject{Path: "acme/tools"}); err == nil {
		t.Error("AddSessionProject without an ID = nil error")
	}
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", "read", []VisibleProject{{Path: "acme/tools"}}, t0); err == nil {
		t.Error("SetSessionAccess without an ID = nil error")
	}
}

// vis is paths, each visible with project ID id.
func vis(id int64, paths ...string) []VisibleProject {
	out := make([]VisibleProject, 0, len(paths))
	for _, p := range paths {
		out = append(out, VisibleProject{Instance: "main", Path: p, ID: id})
	}
	return out
}

// Sessions checked before visible repositories had IDs keep no list: each
// is checked again on its next request (access_checked_at is cleared) and
// is no admin until then, so nothing matches a project by path alone. The
// next migration then ends every session, as sessions belong to accounts.
func TestMigrationDropsAccessWithoutIDs(t *testing.T) {
	s := newStoreAt(t, 7)
	hash := bytes.Repeat([]byte{1}, 32)
	if _, err := s.pool.Exec(ctx, `INSERT INTO sessions (id_hash, platform, instance, user_id, username, token_sealed, csrf,
			is_admin, access_checked_at)
		VALUES ($1, 'gitlab', 'main', 9, 'alice', 'sealed', 'csrf', true, $2)`, hash, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO user_access (session_hash, instance, project) VALUES ($1, 'main', 'acme/widgets')`,
		hash); err != nil {
		t.Fatal(err)
	}
	applyUpTo(t, s, 8)
	var isAdmin bool
	var checked *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT is_admin, access_checked_at FROM sessions WHERE id_hash = $1`, hash).Scan(&isAdmin, &checked); err != nil ||
		isAdmin || checked != nil {
		t.Fatalf("session after migration 8 = admin %v, checked %v, %v; want unchecked, no admin", isAdmin, checked, err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM user_access`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("user_access rows after migration 8 = %d, %v; want none", n, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO user_access (session_hash, instance, project) VALUES ($1, 'main', 'acme/widgets')`,
		hash); err == nil {
		t.Error("a visible project without an ID was stored")
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, found, err := s.Session(ctx, hash); err != nil || found {
		t.Fatalf("session after migration 9 = %v, %v; want it ended", found, err)
	}
}

// Admins come from the teams on every request: sessions keep no admin flag,
// and no admin group is left to be unreadable.
func TestMigrationDropsSessionAdmin(t *testing.T) {
	s := newStoreAt(t, 10)
	column := func() int {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = 'is_admin'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if column() != 1 {
		t.Fatal("no sessions.is_admin before migration 11")
	}
	for _, kind := range []string{"admin_team_unreadable", "token_expiry"} {
		if err := s.SetAttention(ctx, "gh", "", kind, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if column() != 0 {
		t.Error("sessions.is_admin is still there")
	}
	if items, err := s.Attention(ctx); err != nil || len(items) != 1 || items[0].Kind != "token_expiry" {
		t.Errorf("attention after migration 11 = %+v, %v; want only token_expiry", items, err)
	}
}

// A password session has no platform, instance, user or OAuth token.
func TestPasswordSession(t *testing.T) {
	s := newStore(t)
	a := mustPassword(t, s, "admin")
	in := Session{IDHash: bytes.Repeat([]byte{4}, 32), AccountID: a.ID, Username: "admin", CSRF: "csrf",
		LastSeenAt: t0, CreatedAt: t0}
	if err := s.CreateSession(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.Session(ctx, in.IDHash)
	if err != nil || !found {
		t.Fatalf("Session = %v, %v", found, err)
	}
	if got.AccountID != a.ID || got.Platform != "" || got.Instance != "" || got.UserID != 0 || got.TokenSealed != nil || got.Username != "admin" {
		t.Fatalf("session = %+v", got)
	}
	// Access can be set and cleared; a session with no instance keeps no list.
	if err := s.SetSessionAccess(ctx, in.IDHash, "admin", "", nil, t0); err != nil {
		t.Fatalf("SetSessionAccess: %v", err)
	}
	if err := s.AddSessionProject(ctx, in.IDHash, VisibleProject{Path: "acme/widgets", ID: 3}); err != nil {
		t.Fatalf("AddSessionProject: %v", err)
	}
	if projects, err := s.SessionProjects(ctx, in.IDHash); err != nil || len(projects) != 0 {
		t.Fatalf("SessionProjects = %v, %v; want none", projects, err)
	}
	// A session needs an account.
	orphan := in
	orphan.IDHash, orphan.AccountID = bytes.Repeat([]byte{5}, 32), 9999
	if err := s.CreateSession(ctx, orphan); err == nil {
		t.Error("CreateSession for an unknown account = nil error")
	}
}

// newStoreAt is newStore with only the migrations up to version applied.
func newStoreAt(t *testing.T, version int) *Store {
	t.Helper()
	url := os.Getenv(testURLEnv)
	if url == "" {
		t.Skipf("%s is not set", testURLEnv)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	s, err := OpenInSchema(ctx, url, "test_"+hex.EncodeToString(b))
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	t.Cleanup(func() {
		if err := s.DropSchema(context.Background()); err != nil {
			t.Errorf("DropSchema: %v", err)
		}
		s.Close()
	})
	applyUpTo(t, s, version)
	return s
}

// applyUpTo applies the migrations up to version that have not run yet.
func applyUpTo(t *testing.T, s *Store, version int) {
	t.Helper()
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		v, err := migrationVersion(e.Name())
		if err != nil || v > version {
			continue
		}
		sql, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.applyMigration(ctx, v, e.Name(), string(sql)); err != nil {
			t.Fatalf("migration %s: %v", e.Name(), err)
		}
	}
}
