package store

import (
	"bytes"
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
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice-renamed", true, []string{"acme/widgets", "Acme/Widgets", "acme/payments"}, checked); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Session(ctx, in.IDHash)
	if !got.LastSeenAt.Equal(t0.Add(time.Minute)) || !bytes.Equal(got.TokenSealed, []byte("resealed")) ||
		!got.IsAdmin || got.AccessCheckedAt == nil || !got.AccessCheckedAt.Equal(checked) || got.Username != "alice-renamed" {
		t.Fatalf("updated session = %+v", got)
	}
	projects, err := s.SessionProjects(ctx, in.IDHash)
	if err != nil || !slices.Equal(projects, []string{"acme/payments", "acme/widgets"}) {
		t.Fatalf("SessionProjects = %v, %v (a case duplicate is one project)", projects, err)
	}

	// A new check replaces the list.
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", false, []string{"acme/payments"}, checked.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if projects, _ := s.SessionProjects(ctx, in.IDHash); !slices.Equal(projects, []string{"acme/payments"}) {
		t.Fatalf("SessionProjects after a new check = %v", projects)
	}

	// Clearing denies everything but keeps the time of the last good check.
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", true, []string{"acme/payments"}, checked); err != nil {
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
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", false, []string{"acme/widgets"}, checked); err != nil {
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
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", true, []string{"acme/widgets"}, checked); err != nil {
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
	if err := s.SetSessionAccess(ctx, in.IDHash, "alice", true, []string{"acme/widgets"}, t0); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"acme/gadgets", "ACME/Gadgets", "acme/widgets"} {
		if err := s.AddSessionProject(ctx, in.IDHash, p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SessionProjects(ctx, in.IDHash)
	if err != nil || !slices.Equal(got, []string{"acme/gadgets", "acme/widgets"}) {
		t.Fatalf("projects = %v, %v", got, err)
	}
	// A session that is gone stays gone.
	if err := s.AddSessionProject(ctx, bytes.Repeat([]byte{9}, 32), "acme/gadgets"); err != nil {
		t.Fatal(err)
	}
}
