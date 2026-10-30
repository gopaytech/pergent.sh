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

func newSession(hashByte byte, created time.Time) Session {
	return Session{
		IDHash: bytes.Repeat([]byte{hashByte}, 32), Platform: "gitlab", Instance: "main",
		UserID: 9, Username: "alice", TokenSealed: []byte("sealed"), CSRF: "csrf-token",
		LastSeenAt: created, CreatedAt: created,
	}
}

func TestSessionsCRUD(t *testing.T) {
	s := newStore(t)
	in := newSession(1, t0)
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
		got.Username != "alice" || got.IsAdmin || !bytes.Equal(got.TokenSealed, []byte("sealed")) || got.CSRF != "csrf-token" ||
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
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice-renamed", true, vis(7, "acme/widgets", "Acme/Widgets", "acme/payments"), checked); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Session(ctx, in.IDHash)
	if !got.LastSeenAt.Equal(t0.Add(time.Minute)) || !bytes.Equal(got.TokenSealed, []byte("resealed")) ||
		!got.IsAdmin || got.AccessCheckedAt == nil || !got.AccessCheckedAt.Equal(checked) || got.Username != "alice-renamed" {
		t.Fatalf("updated session = %+v", got)
	}
	projects, err := s.SessionProjects(ctx, in.IDHash)
	if err != nil || !slices.Equal(projects, []VisibleProject{{"acme/payments", 7}, {"acme/widgets", 7}}) {
		t.Fatalf("SessionProjects = %v, %v (a case duplicate is one project)", projects, err)
	}

	// A new check replaces the list.
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", false, vis(8, "acme/payments"), checked.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if projects, _ := s.SessionProjects(ctx, in.IDHash); !slices.Equal(projects, []VisibleProject{{"acme/payments", 8}}) {
		t.Fatalf("SessionProjects after a new check = %v", projects)
	}

	// Clearing denies everything but keeps the time of the last good check.
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", true, vis(8, "acme/payments"), checked); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearSessionAccess(ctx, in.IDHash); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Session(ctx, in.IDHash)
	if projects, _ := s.SessionProjects(ctx, in.IDHash); len(projects) != 0 || got.IsAdmin || !got.AccessCheckedAt.Equal(checked) {
		t.Fatalf("after ClearSessionAccess: projects %v, session %+v", projects, got)
	}

	// Deleting removes the session and its projects.
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", false, vis(7, "acme/widgets"), checked); err != nil {
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
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", true, vis(7, "acme/widgets"), checked); err != nil {
		t.Fatalf("SetSessionAccess on a deleted session: %v", err)
	}
	if _, found, _ := s.Session(ctx, in.IDHash); found {
		t.Fatal("SetSessionAccess brought a deleted session back")
	}
}

func TestPruneSessions(t *testing.T) {
	s := newStore(t)
	now := t0.Add(30 * 24 * time.Hour)
	old := newSession(1, now.Add(-8*24*time.Hour)) // created 8 days ago, seen just now
	old.LastSeenAt = now.Add(-time.Minute)
	idle := newSession(2, now.Add(-time.Hour*10)) // seen 10 h ago
	fresh := newSession(3, now.Add(-time.Hour))
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
	in := newSession(1, t0)
	if err := s.CreateSession(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", true, vis(7, "acme/widgets"), t0); err != nil {
		t.Fatal(err)
	}
	for _, p := range []VisibleProject{{"acme/gadgets", 11}, {"ACME/Gadgets", 12}, {"acme/widgets", 13}} {
		if err := s.AddSessionProject(ctx, in.IDHash, p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SessionProjects(ctx, in.IDHash)
	if err != nil || !slices.Equal(got, []VisibleProject{{"acme/gadgets", 11}, {"acme/widgets", 7}}) {
		t.Fatalf("projects = %v, %v", got, err)
	}
	// A session that is gone stays gone.
	if err := s.AddSessionProject(ctx, bytes.Repeat([]byte{9}, 32), VisibleProject{"acme/gadgets", 11}); err != nil {
		t.Fatal(err)
	}
	// A project needs its ID.
	if err := s.AddSessionProject(ctx, in.IDHash, VisibleProject{Path: "acme/tools"}); err == nil {
		t.Error("AddSessionProject without an ID = nil error")
	}
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", true, []VisibleProject{{Path: "acme/tools"}}, t0); err == nil {
		t.Error("SetSessionAccess without an ID = nil error")
	}
}

// vis is paths, each visible with project ID id.
func vis(id int64, paths ...string) []VisibleProject {
	out := make([]VisibleProject, 0, len(paths))
	for _, p := range paths {
		out = append(out, VisibleProject{Path: p, ID: id})
	}
	return out
}

// Sessions checked before visible repositories had IDs keep no list: each
// is checked again on its next request (access_checked_at is cleared) and
// is no admin until then, so nothing matches a project by path alone.
func TestMigrationDropsAccessWithoutIDs(t *testing.T) {
	s := newStoreAt(t, 7)
	in := newSession(1, t0)
	if err := s.CreateSession(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET is_admin = true, access_checked_at = $2 WHERE id_hash = $1`,
		in.IDHash, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO user_access (session_hash, instance, project) VALUES ($1, 'main', 'acme/widgets')`,
		in.IDHash); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got, found, err := s.Session(ctx, in.IDHash)
	if err != nil || !found || got.AccessCheckedAt != nil || got.IsAdmin {
		t.Fatalf("session after the migration = %+v, %v, %v; want unchecked, no admin", got, found, err)
	}
	if projects, err := s.SessionProjects(ctx, in.IDHash); err != nil || len(projects) != 0 {
		t.Fatalf("projects after the migration = %v, %v; want none", projects, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO user_access (session_hash, instance, project) VALUES ($1, 'main', 'acme/widgets')`,
		in.IDHash); err == nil {
		t.Error("a visible project without an ID was stored")
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
	return s
}
