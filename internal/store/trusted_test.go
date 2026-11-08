package store

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// sha is a head made of one repeated hex digit.
func sha(c byte) string { return strings.Repeat(string(c), 40) }

// mustRun reads run id back, failing the test when it is gone.
func mustRun(t *testing.T, s *Store, id string) Run {
	t.Helper()
	r, ok, err := s.Run(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Run(%s): ok=%v err=%v", id, ok, err)
	}
	return r
}

// A held run is never handed to the dispatcher.
func TestHoldRunIsNeverClaimed(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, sha('a'))
	if ok, err := s.HoldRun(ctx, id, WaitUntrusted); err != nil || !ok {
		t.Fatalf("HoldRun = %v, %v", ok, err)
	}
	claimed, err := s.ClaimQueued(ctx, 10, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %d runs, want none", len(claimed))
	}
	r := mustRun(t, s, id)
	if r.State != Waiting || r.SkipReason != WaitUntrusted {
		t.Errorf("run = %s/%q, want waiting/%q", r.State, r.SkipReason, WaitUntrusted)
	}
	// Holding it again changes nothing.
	if ok, err := s.HoldRun(ctx, id, WaitCheckFailed); err != nil || ok {
		t.Errorf("second HoldRun = %v, %v; want false", ok, err)
	}
}

// Starting a waiting run queues that same run as a start.
func TestStartRunTurnsWaitingIntoQueuedStart(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, sha('a'))
	if _, err := s.HoldRun(ctx, id, WaitUntrusted); err != nil {
		t.Fatal(err)
	}
	got, started, err := s.StartRun(ctx, mr1, "acme/widgets", sha('a'), "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != id || !started {
		t.Errorf("StartRun = %s, %v; want %s, true", got, started, id)
	}
	r := mustRun(t, s, id)
	if r.State != Queued || r.Trigger != TriggerStart {
		t.Errorf("run = %s/%s, want queued/start", r.State, r.Trigger)
	}
}

// With nothing waiting on the head, a start is a new run.
func TestStartRunInsertsWhenNothingWaits(t *testing.T) {
	s := newStore(t)
	old := enqueue(t, s, mr1, sha('a'))
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'published' WHERE id = $1::uuid`, old); err != nil {
		t.Fatal(err)
	}
	id, created, err := s.StartRun(ctx, mr1, "acme/widgets", sha('a'), "main")
	if err != nil || !created || id == old {
		t.Fatalf("StartRun = %s, %v, %v; want a new run", id, created, err)
	}
	if r := mustRun(t, s, id); r.State != Queued || r.Trigger != TriggerStart {
		t.Errorf("run = %s/%s, want queued/start", r.State, r.Trigger)
	}
	if r := mustRun(t, s, old); r.State != Superseded {
		t.Errorf("old run = %s, want superseded", r.State)
	}
	// A second start finds the live one.
	again, created, err := s.StartRun(ctx, mr1, "acme/widgets", sha('a'), "main")
	if err != nil || created || again != id {
		t.Errorf("second StartRun = %s, %v, %v; want %s, false", again, created, err, id)
	}
}

// A waiting run goes away like a queued one: on a new head, and on a close.
func TestPushSupersedesWaitingRun(t *testing.T) {
	s := newStore(t)
	a := enqueue(t, s, mr1, sha('a'))
	if _, err := s.HoldRun(ctx, a, WaitUntrusted); err != nil {
		t.Fatal(err)
	}
	b := enqueue(t, s, mr1, sha('b'))
	if r := mustRun(t, s, a); r.State != Superseded {
		t.Errorf("run of the old head = %s, want superseded", r.State)
	}
	if _, err := s.HoldRun(ctx, b, WaitUntrusted); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SupersedeMR(ctx, mr1); err != nil || n != 1 {
		t.Errorf("SupersedeMR = %d, %v; want 1", n, err)
	}
	if r := mustRun(t, s, b); r.State != Superseded {
		t.Errorf("waiting run after a close = %s, want superseded", r.State)
	}
}

// A start counts against the change's cap only.
func TestStartDoesNotCountAgainstRepoCap(t *testing.T) {
	s := newStore(t)
	for _, h := range []byte{'a', 'b'} {
		if _, _, err := s.StartRun(ctx, mr1, "acme/widgets", sha(h), "main"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.CountReruns(ctx, &mr1, "", 0, t0.Add(-time.Hour*24*365)); err != nil || n != 2 {
		t.Errorf("per change = %d, %v; want 2", n, err)
	}
	if n, err := s.CountReruns(ctx, nil, mr1.Instance, mr1.ProjectID, t0.Add(-time.Hour*24*365)); err != nil || n != 0 {
		t.Errorf("per repository = %d, %v; want 0", n, err)
	}
}

// The note of an untrusted change is claimed once.
func TestClaimUntrustedNoteOnce(t *testing.T) {
	s := newStore(t)
	for i, want := range []bool{true, false} {
		if got, err := s.ClaimUntrustedNote(ctx, mr1, t0); err != nil || got != want {
			t.Fatalf("claim %d = %v, %v; want %v", i, got, err, want)
		}
	}
	if err := s.ReleaseUntrustedNote(ctx, mr1); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ClaimUntrustedNote(ctx, mr1, t0); err != nil || !got {
		t.Errorf("claim after release = %v, %v; want true", got, err)
	}
	// The pergent comment's id on the same row is untouched.
	if err := s.SetNoteID(ctx, mr1, 42); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ClaimUntrustedNote(ctx, mr1, t0); err != nil || got {
		t.Errorf("claim after SetNoteID = %v, %v; want false", got, err)
	}
	if id, _ := s.NoteID(ctx, mr1); id != 42 {
		t.Errorf("NoteID = %d, want 42", id)
	}
}

// The repository's mode and list, and the instance's mode, are stored.
func TestTrustedAuthorsRoundTrip(t *testing.T) {
	s := newStore(t)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/widgets", CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	on := "on"
	authors := []TrustedAuthor{{ID: 5, Username: "alice"}, {ID: 9, Username: "bob"}}
	if found, err := s.UpdateDashboardRepo(ctx, "main", "acme/widgets", func(r *DashboardRepo) {
		r.TrustedAuthorsOnly, r.TrustedAuthors = &on, authors
	}); err != nil || !found {
		t.Fatalf("UpdateDashboardRepo = %v, %v", found, err)
	}
	r, _, err := s.DashboardRepo(ctx, "main", "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if r.TrustedAuthorsOnly == nil || *r.TrustedAuthorsOnly != "on" || !slices.Equal(r.TrustedAuthors, authors) {
		t.Errorf("repo = %v %+v", r.TrustedAuthorsOnly, r.TrustedAuthors)
	}
	bad := "maybe"
	if _, err := s.UpdateDashboardRepo(ctx, "main", "acme/widgets", func(r *DashboardRepo) { r.TrustedAuthorsOnly = &bad }); err == nil {
		t.Error("mode maybe was stored")
	}

	if err := s.CreateInstance(ctx, InstanceRow{Name: "lab", Platform: "gitlab"}, nil, t0); err != nil {
		t.Fatal(err)
	}
	row, _, err := s.InstanceRow(ctx, "lab")
	if err != nil || row.TrustedAuthorsOnly != "follow_visibility" {
		t.Fatalf("new instance mode = %q, %v; want follow_visibility", row.TrustedAuthorsOnly, err)
	}
	if found, err := s.SetInstanceTrustMode(ctx, "lab", "off"); err != nil || !found {
		t.Fatalf("SetInstanceTrustMode = %v, %v", found, err)
	}
	if row, _, _ := s.InstanceRow(ctx, "lab"); row.TrustedAuthorsOnly != "off" {
		t.Errorf("mode = %q, want off", row.TrustedAuthorsOnly)
	}
	if _, err := s.SetInstanceTrustMode(ctx, "lab", "bogus"); err == nil {
		t.Error("mode bogus was stored")
	}
	if found, err := s.SetInstanceTrustMode(ctx, "nope", "on"); err != nil || found {
		t.Errorf("unknown instance = %v, %v; want not found", found, err)
	}
}

// A waiting run is listed among the change's runs.
func TestWaitingRunDigest(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, sha('a'))
	if _, err := s.HoldRun(ctx, id, WaitUntrusted); err != nil {
		t.Fatal(err)
	}
	got, err := s.MRRunDigests(ctx, mr1, Visibility{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != Waiting {
		t.Fatalf("digests = %+v, want one waiting run", got)
	}
}

// The previous release, on the migrated database, never claims a waiting run.
func TestMigrationKeepsPreviousReleaseTrusted(t *testing.T) {
	s := newStoreAt(t, 30)
	id := enqueue(t, s, mr1, sha('a'))
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HoldRun(ctx, id, WaitUntrusted); err != nil {
		t.Fatal(err)
	}
	// 0.11.0's claim, as it is in that release.
	rows, err := s.pool.Query(ctx, `UPDATE runs SET state = 'running', claimed_at = $2
		WHERE id IN (SELECT id FROM runs WHERE state = 'queued' AND (not_before IS NULL OR not_before <= $2)
			ORDER BY created_at, id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING id`, 10, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if n != 0 {
		t.Errorf("the previous release claimed %d runs, want none", n)
	}
}

// A start that comes before the dispatcher held the push run (queued, or
// claimed and still being checked) makes that run a start, and the hold
// that follows doesn't take it back.
func TestStartBeforeHold(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, sha('a'))
	got, started, err := s.StartRun(ctx, mr1, "acme/widgets", sha('a'), "main")
	if err != nil || !started || got != id {
		t.Fatalf("StartRun = %s, %v, %v; want %s, true", got, started, err, id)
	}
	if r := mustRun(t, s, id); r.State != Queued || r.Trigger != TriggerStart {
		t.Errorf("run = %s/%s, want queued/start", r.State, r.Trigger)
	}
	if ok, err := s.HoldRun(ctx, id, WaitUntrusted); err != nil || ok {
		t.Errorf("HoldRun of a start = %v, %v; want false", ok, err)
	}

	// Claimed, not dispatched: the same.
	id2 := enqueue(t, s, mr1, sha('b'))
	if _, err := s.ClaimQueued(ctx, 10, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, started, err := s.StartRun(ctx, mr1, "acme/widgets", sha('b'), "main"); err != nil || !started {
		t.Fatalf("StartRun of a claimed run = %v, %v", started, err)
	}
	if r := mustRun(t, s, id2); r.Trigger != TriggerStart {
		t.Errorf("claimed run trigger = %s, want start", r.Trigger)
	}
	if ok, _ := s.HoldRun(ctx, id2, WaitUntrusted); ok {
		t.Error("a claimed start was held")
	}
}
