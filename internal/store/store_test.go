package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"
)

const testURLEnv = "PERGENT_SH_TEST_DATABASE_URL"

// newStore opens a migrated store bound to a fresh random schema and drops
// the schema when the test ends. It skips when no test database is set.
func newStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv(testURLEnv)
	if url == "" {
		t.Skipf("%s is not set", testURLEnv)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
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
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

var (
	ctx  = context.Background()
	mr1  = MRKey{Instance: "https://gitlab.example.com", ProjectID: 7, IID: 3}
	mr2  = MRKey{Instance: "https://gitlab.example.com", ProjectID: 7, IID: 4}
	t0   = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	hash = func(token string) []byte { h := sha256.Sum256([]byte(token)); return h[:] }
)

func enqueue(t *testing.T, s *Store, mr MRKey, head string) string {
	t.Helper()
	id, _, err := s.EnqueueRun(ctx, mr, "acme/widgets", head, "main")
	if err != nil {
		t.Fatalf("EnqueueRun(%s): %v", head, err)
	}
	return id
}

func getRun(t *testing.T, s *Store, id string) Run {
	t.Helper()
	r, err := s.getRun(ctx, id)
	if err != nil {
		t.Fatalf("getRun(%s): %v", id, err)
	}
	return r
}

// dispatched enqueues a run, claims it and records a dispatch with the given
// token, valid for an hour after t0.
func dispatched(t *testing.T, s *Store, mr MRKey, head, token string) string {
	t.Helper()
	id := enqueue(t, s, mr, head)
	claimed, err := s.ClaimQueued(ctx, 10, t0)
	if err != nil || len(claimed) == 0 {
		t.Fatalf("ClaimQueued: %v %v", claimed, err)
	}
	if err := s.MarkDispatched(ctx, id, "job-"+head, "base-"+head, "start-"+head, hash(token), t0.Add(time.Hour), t0); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	return id
}

// reported gives a run in state reported holding the given result body.
func reported(t *testing.T, s *Store, mr MRKey, head, body string) string {
	t.Helper()
	id := dispatched(t, s, mr, head, "tok-"+head)
	out, err := s.StoreResult(ctx, id, "tok-"+head, []byte(body), t0.Add(time.Minute))
	if err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	return id
}

func TestEnqueueSupersedesOlderHead(t *testing.T) {
	s := newStore(t)
	a := enqueue(t, s, mr1, "aaa")
	other := enqueue(t, s, mr2, "ooo")
	b, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "bbb", "main")
	if err != nil || !created {
		t.Fatalf("EnqueueRun B = %v, %v", created, err)
	}
	if got := getRun(t, s, a).State; got != Superseded {
		t.Errorf("A state = %s, want superseded", got)
	}
	if got := getRun(t, s, b).State; got != Queued {
		t.Errorf("B state = %s, want queued", got)
	}
	if got := getRun(t, s, other).State; got != Queued {
		t.Errorf("other MR state = %s, want queued", got)
	}
	rb := getRun(t, s, b)
	if rb.MR != mr1 || rb.ProjectPath != "acme/widgets" || rb.HeadSHA != "bbb" || rb.TargetBranch != "main" {
		t.Errorf("unexpected run %+v", rb)
	}
}

func TestEnqueueSupersedesRunningRun(t *testing.T) {
	s := newStore(t)
	a := dispatched(t, s, mr1, "aaa", "tok")
	enqueue(t, s, mr1, "bbb")
	if got := getRun(t, s, a).State; got != Superseded {
		t.Errorf("running A state = %s, want superseded", got)
	}
	// A late result for the superseded run is discarded and the token is spent.
	out, err := s.StoreResult(ctx, a, "tok", []byte(`{"status":"completed"}`), t0.Add(time.Minute))
	if err != nil || out != ResultDiscarded {
		t.Fatalf("StoreResult = %v, %v; want discarded", out, err)
	}
	out, _ = s.StoreResult(ctx, a, "tok", []byte(`{"status":"completed"}`), t0.Add(time.Minute))
	if out != ResultDiscarded {
		t.Errorf("second late result = %v, want discarded", out)
	}
	if r := getRun(t, s, a); r.Result != nil || r.State != Superseded {
		t.Errorf("discarded result was kept: %+v", r)
	}
}

func TestEnqueueSameHeadIsIdempotent(t *testing.T) {
	s := newStore(t)
	id1, created1, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "main")
	if err != nil || !created1 {
		t.Fatalf("first = %v, %v", created1, err)
	}
	id2, created2, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "main")
	if err != nil || created2 {
		t.Fatalf("second = %v, %v", created2, err)
	}
	if id1 != id2 {
		t.Errorf("ids differ: %s vs %s", id1, id2)
	}
}

func TestEnqueueSameHeadAfterSupersededCreatesNewRun(t *testing.T) {
	s := newStore(t)
	a := enqueue(t, s, mr1, "aaa")
	enqueue(t, s, mr1, "bbb")
	a2, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "main")
	if err != nil || !created || a2 == a {
		t.Fatalf("re-enqueue A = %s %v %v", a2, created, err)
	}
}

func TestSupersedeMR(t *testing.T) {
	s := newStore(t)
	a := enqueue(t, s, mr1, "aaa")
	enqueue(t, s, mr2, "ooo")
	n, err := s.SupersedeMR(ctx, mr1)
	if err != nil || n != 1 {
		t.Fatalf("SupersedeMR = %d, %v", n, err)
	}
	if got := getRun(t, s, a).State; got != Superseded {
		t.Errorf("state = %s", got)
	}
	if n, _ := s.SupersedeMR(ctx, mr1); n != 0 {
		t.Errorf("second SupersedeMR = %d, want 0", n)
	}
}

func TestClaimQueuedRespectsNotBeforeAndLimit(t *testing.T) {
	s := newStore(t)
	ids := make([]string, 0, 4)
	for i, head := range []string{"h1", "h2", "h3", "h4"} {
		ids = append(ids, enqueue(t, s, MRKey{mr1.Instance, 7, int64(100 + i)}, head))
	}
	// h2 is delayed until after the claim time.
	if _, err := s.ClaimQueued(ctx, 1, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Requeue(ctx, ids[0], t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, ids[0]); r.State != Queued || r.Attempts != 1 || r.NotBefore.IsZero() {
		t.Fatalf("after Requeue: %+v", r)
	}
	got, err := s.ClaimQueued(ctx, 2, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != ids[1] || got[1].ID != ids[2] {
		t.Fatalf("claimed %v, want ids[1], ids[2] oldest first", runIDs(got))
	}
	for _, r := range got {
		if r.State != Running {
			t.Errorf("claimed run %s state = %s", r.ID, r.State)
		}
	}
	got, _ = s.ClaimQueued(ctx, 10, t0)
	if len(got) != 1 || got[0].ID != ids[3] {
		t.Fatalf("third claim = %v, want only ids[3]", runIDs(got))
	}
	got, _ = s.ClaimQueued(ctx, 10, t0)
	if len(got) != 0 {
		t.Fatalf("fourth claim = %v, want none (delayed)", runIDs(got))
	}
	got, _ = s.ClaimQueued(ctx, 10, t0.Add(2*time.Hour))
	if len(got) != 1 || got[0].ID != ids[0] {
		t.Fatalf("claim after delay = %v, want ids[0]", runIDs(got))
	}
	n, err := s.CountRunning(ctx)
	if err != nil || n != 4 {
		t.Errorf("CountRunning = %d, %v; want 4", n, err)
	}
	running, err := s.RunningRuns(ctx)
	if err != nil || len(running) != 4 {
		t.Errorf("RunningRuns = %d, %v; want 4", len(running), err)
	}
}

func runIDs(rs []Run) []string {
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestMarkDispatchedRecordsFields(t *testing.T) {
	s := newStore(t)
	id := dispatched(t, s, mr1, "aaa", "tok")
	r := getRun(t, s, id)
	if r.JobName != "job-aaa" || r.BaseSHA != "base-aaa" || r.StartSHA != "start-aaa" {
		t.Errorf("fields not recorded: %+v", r)
	}
	if !r.TokenExpiresAt.Equal(t0.Add(time.Hour)) || !r.DispatchedAt.Equal(t0) {
		t.Errorf("times: expires=%v dispatched=%v", r.TokenExpiresAt, r.DispatchedAt)
	}
	if r.State != Running {
		t.Errorf("state = %s", r.State)
	}
}

func TestStoreResultOutcomes(t *testing.T) {
	s := newStore(t)
	body := []byte(`{"status":"completed","verdict":"no_blocking_issues"}`)

	t.Run("stored then conflict", func(t *testing.T) {
		id := dispatched(t, s, mr1, "aaa", "tok")
		out, err := s.StoreResult(ctx, id, "tok", body, t0.Add(time.Minute))
		if err != nil || out != ResultStored {
			t.Fatalf("first = %v, %v", out, err)
		}
		r := getRun(t, s, id)
		if r.State != Reported || !bytes.Contains(r.Result, []byte("no_blocking_issues")) || !r.ReportedAt.Equal(t0.Add(time.Minute)) {
			t.Errorf("after store: %+v", r)
		}
		// A retried POST with the right token is a conflict, a wrong token is not.
		out, err = s.StoreResult(ctx, id, "tok", body, t0.Add(time.Minute))
		if err != nil || out != ResultConflict {
			t.Errorf("replay = %v, %v; want conflict", out, err)
		}
		out, err = s.StoreResult(ctx, id, "other", body, t0.Add(time.Minute))
		if err != nil || out != ResultUnauthorized {
			t.Errorf("replay with wrong token = %v, %v; want unauthorized", out, err)
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		id := dispatched(t, s, mr2, "bbb", "tok")
		out, err := s.StoreResult(ctx, id, "not-tok", body, t0.Add(time.Minute))
		if err != nil || out != ResultUnauthorized {
			t.Errorf("= %v, %v; want unauthorized", out, err)
		}
		if r := getRun(t, s, id); r.State != Running || r.Result != nil {
			t.Errorf("run changed: %+v", r)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		id := dispatched(t, s, MRKey{mr1.Instance, 8, 1}, "ccc", "tok")
		out, err := s.StoreResult(ctx, id, "tok", body, t0.Add(time.Hour+time.Second))
		if err != nil || out != ResultUnauthorized {
			t.Errorf("= %v, %v; want unauthorized", out, err)
		}
	})

	t.Run("unknown and malformed ids", func(t *testing.T) {
		for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid", ""} {
			out, err := s.StoreResult(ctx, id, "tok", body, t0)
			if err != nil || out != ResultUnauthorized {
				t.Errorf("id %q = %v, %v; want unauthorized", id, out, err)
			}
		}
	})

	t.Run("run not yet dispatched has no token", func(t *testing.T) {
		id := enqueue(t, s, MRKey{mr1.Instance, 8, 2}, "ddd")
		out, err := s.StoreResult(ctx, id, "", body, t0)
		if err != nil || out != ResultUnauthorized {
			t.Errorf("= %v, %v; want unauthorized", out, err)
		}
	})

	t.Run("superseded is discarded", func(t *testing.T) {
		mr := MRKey{mr1.Instance, 8, 3}
		id := dispatched(t, s, mr, "eee", "tok")
		enqueue(t, s, mr, "fff")
		out, err := s.StoreResult(ctx, id, "tok", body, t0.Add(time.Minute))
		if err != nil || out != ResultDiscarded {
			t.Errorf("= %v, %v; want discarded", out, err)
		}
		out, err = s.StoreResult(ctx, id, "wrong", body, t0.Add(time.Minute))
		if err != nil || out != ResultUnauthorized {
			t.Errorf("wrong token on superseded run = %v, %v; want unauthorized", out, err)
		}
	})

	t.Run("after ReportFailure is a conflict", func(t *testing.T) {
		id := dispatched(t, s, MRKey{mr1.Instance, 8, 4}, "ggg", "tok")
		ok, err := s.ReportFailure(ctx, id, "job failed: OOMKilled", t0.Add(time.Minute))
		if err != nil || !ok {
			t.Fatalf("ReportFailure = %v, %v", ok, err)
		}
		out, err := s.StoreResult(ctx, id, "tok", body, t0.Add(2*time.Minute))
		if err != nil || out != ResultConflict {
			t.Errorf("= %v, %v; want conflict", out, err)
		}
		if r := getRun(t, s, id); r.Result != nil || r.FailureReason != "job failed: OOMKilled" {
			t.Errorf("run = %+v", r)
		}
	})

	t.Run("invalid JSON is ErrInvalidResult, not a stored result", func(t *testing.T) {
		id := dispatched(t, s, MRKey{mr1.Instance, 8, 5}, "hhh", "tok")
		for _, bad := range []string{"not json", "", `{"status":`} {
			_, err := s.StoreResult(ctx, id, "tok", []byte(bad), t0.Add(time.Minute))
			if !errors.Is(err, ErrInvalidResult) {
				t.Errorf("body %q: err = %v, want ErrInvalidResult", bad, err)
			}
		}
		if r := getRun(t, s, id); r.State != Running || r.Result != nil {
			t.Errorf("run changed: %+v", r)
		}
		// Authentication comes first: a wrong token never learns the body was bad.
		out, err := s.StoreResult(ctx, id, "wrong", []byte("not json"), t0.Add(time.Minute))
		if err != nil || out != ResultUnauthorized {
			t.Errorf("wrong token, bad body = %v, %v; want unauthorized", out, err)
		}
		// The run is still usable with a valid body.
		out, err = s.StoreResult(ctx, id, "tok", body, t0.Add(time.Minute))
		if err != nil || out != ResultStored {
			t.Errorf("valid body afterwards = %v, %v", out, err)
		}
	})
}

func TestStoreResultKeepsHashForSingleUse(t *testing.T) {
	s := newStore(t)
	id := dispatched(t, s, mr1, "aaa", "super-secret-token")
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT token_hash FROM runs WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, hash("super-secret-token")) {
		t.Errorf("token_hash is not the SHA-256 of the token")
	}
	if _, err := s.StoreResult(ctx, id, "super-secret-token", []byte(`{"status":"completed"}`), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The hash stays; single use is enforced by the run's state.
	if err := s.pool.QueryRow(ctx, `SELECT token_hash FROM runs WHERE id = $1`, id).Scan(&raw); err != nil || !bytes.Equal(raw, hash("super-secret-token")) {
		t.Errorf("token hash after result: %x, %v", raw, err)
	}
	out, err := s.StoreResult(ctx, id, "super-secret-token", []byte(`{"status":"completed"}`), t0.Add(time.Minute))
	if err != nil || out != ResultConflict {
		t.Errorf("replay = %v, %v; want conflict", out, err)
	}
}

func TestReportFailureOnlyMovesUnreportedRuns(t *testing.T) {
	s := newStore(t)
	queued := enqueue(t, s, mr1, "aaa")
	ok, err := s.ReportFailure(ctx, queued, "no job", t0)
	if err != nil || !ok {
		t.Fatalf("queued run: %v %v", ok, err)
	}
	if r := getRun(t, s, queued); r.State != Reported || r.FailureReason != "no job" || !r.ReportedAt.Equal(t0) {
		t.Errorf("run = %+v", r)
	}
	if ok, _ := s.ReportFailure(ctx, queued, "again", t0); ok {
		t.Errorf("second ReportFailure returned true")
	}
	done := reported(t, s, mr2, "bbb", `{"status":"completed"}`)
	if ok, _ := s.ReportFailure(ctx, done, "late", t0); ok {
		t.Errorf("ReportFailure moved a run that has a result")
	}
	if ok, _ := s.ReportFailure(ctx, "00000000-0000-0000-0000-000000000000", "x", t0); ok {
		t.Errorf("ReportFailure on unknown id returned true")
	}
}

func TestRunsToPublishAndMarkPublished(t *testing.T) {
	s := newStore(t)
	a := reported(t, s, MRKey{mr1.Instance, 9, 1}, "a", `{"status":"completed"}`)
	b := reported(t, s, MRKey{mr1.Instance, 9, 2}, "b", `{"status":"completed"}`)
	c := reported(t, s, MRKey{mr1.Instance, 9, 3}, "c", `{"status":"completed"}`)
	enqueue(t, s, MRKey{mr1.Instance, 9, 4}, "queued-not-reported")

	now := t0.Add(time.Hour)
	if err := s.PublishRetry(ctx, b, "boom", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunsToPublish(ctx, now, 10)
	if err != nil || len(got) != 2 || got[0].ID != a || got[1].ID != c {
		t.Fatalf("RunsToPublish = %v, %v; want a, c", runIDs(got), err)
	}
	if got, _ := s.RunsToPublish(ctx, now, 1); len(got) != 1 || got[0].ID != a {
		t.Errorf("limit 1 = %v", runIDs(got))
	}
	if err := s.MarkPublished(ctx, a); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, a); r.State != Published {
		t.Errorf("state = %s", r.State)
	}
	got, _ = s.RunsToPublish(ctx, now.Add(2*time.Hour), 10)
	if len(got) != 2 || got[0].ID != b || got[1].ID != c {
		t.Errorf("after delay = %v, want b, c", runIDs(got))
	}
}

func TestRunWithFailureReasonIsPublishable(t *testing.T) {
	s := newStore(t)
	id := dispatched(t, s, mr1, "aaa", "tok")
	if ok, err := s.ReportFailure(ctx, id, "boom", t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	got, err := s.RunsToPublish(ctx, t0, 10)
	if err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("RunsToPublish = %v, %v", runIDs(got), err)
	}
}

func TestPublishRetryGivesUpAtTen(t *testing.T) {
	s := newStore(t)
	id := reported(t, s, mr1, "aaa", `{"status":"completed"}`)
	for i := 1; i <= 10; i++ {
		if err := s.PublishRetry(ctx, id, "gitlab 502", t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		r := getRun(t, s, id)
		want := Reported
		if i == 10 {
			want = PublishFailed
		}
		if r.State != want || r.Attempts != i {
			t.Fatalf("after %d retries: state=%s attempts=%d, want %s/%d", i, r.State, r.Attempts, want, i)
		}
		if r.PublishError != "gitlab 502" {
			t.Errorf("PublishError = %q", r.PublishError)
		}
	}
	if got, _ := s.RunsToPublish(ctx, t0.Add(24*time.Hour), 10); len(got) != 0 {
		t.Errorf("publish_failed run is still offered: %v", runIDs(got))
	}
}

func TestPublishRetryCountsFromReport(t *testing.T) {
	// Dispatch retries must not eat into the publish budget.
	s := newStore(t)
	id := enqueue(t, s, mr1, "aaa")
	for i := 0; i < 3; i++ {
		if _, err := s.ClaimQueued(ctx, 1, t0.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.Requeue(ctx, id, t0.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ClaimQueued(ctx, 1, t0.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReportFailure(ctx, id, "x", t0.Add(10*time.Hour)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if r := getRun(t, s, id); r.Attempts != 0 {
		t.Errorf("Attempts after report = %d, want 0", r.Attempts)
	}
}

func TestRunsToCleanUp(t *testing.T) {
	s := newStore(t)
	a := dispatched(t, s, mr1, "aaa", "tok")
	enqueue(t, s, mr1, "bbb")
	got, err := s.RunsToCleanUp(ctx)
	if err != nil || len(got) != 1 || got[0].ID != a || got[0].JobName != "job-aaa" {
		t.Fatalf("RunsToCleanUp = %+v, %v", got, err)
	}
	if err := s.MarkCleanedUp(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RunsToCleanUp(ctx); len(got) != 0 {
		t.Errorf("still listed after MarkCleanedUp: %v", runIDs(got))
	}
	if !getRun(t, s, a).CleanedUp {
		t.Errorf("CleanedUp not set")
	}
}

func TestPreviousResultSkipsErrorsAndCurrent(t *testing.T) {
	s := newStore(t)
	if _, ok, err := s.PreviousResult(ctx, mr1, ""); err != nil || ok {
		t.Fatalf("empty MR = %v, %v", ok, err)
	}
	published := func(mr MRKey, head, body string) string {
		id := reported(t, s, mr, head, body)
		if err := s.MarkPublished(ctx, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	published(mr1, "a", `{"status":"completed","verdict":"first"}`)
	published(mr1, "b", `{"status":"error","error":{"code":"internal"}}`)
	published(mr1, "c", `{"status":"completed","verdict":"second"}`)
	current := published(mr1, "d", `{"status":"completed","verdict":"current"}`)
	reported(t, s, mr1, "e", `{"status":"completed","verdict":"unpublished"}`)
	published(mr2, "z", `{"status":"completed","verdict":"other-mr"}`)

	got, ok, err := s.PreviousResult(ctx, mr1, current)
	if err != nil || !ok {
		t.Fatalf("= %v, %v", ok, err)
	}
	if !bytes.Contains(got, []byte("second")) {
		t.Errorf("got %s, want the second run's result", got)
	}
	// Without excluding the current run, the newest published completed one wins.
	got, ok, _ = s.PreviousResult(ctx, mr1, "")
	if !ok || !bytes.Contains(got, []byte("current")) {
		t.Errorf("no exclusion: %s %v", got, ok)
	}
}

func TestNoteIDAndApproval(t *testing.T) {
	s := newStore(t)
	if id, err := s.NoteID(ctx, mr1); err != nil || id != 0 {
		t.Fatalf("NoteID empty = %d, %v", id, err)
	}
	if err := s.SetNoteID(ctx, mr1, 555); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNoteID(ctx, mr1, 556); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.NoteID(ctx, mr1); id != 556 {
		t.Errorf("NoteID = %d, want 556", id)
	}
	if id, _ := s.NoteID(ctx, mr2); id != 0 {
		t.Errorf("other MR NoteID = %d", id)
	}
	// Setting an approval must not lose the note id.
	if err := s.SetApprovedSHA(ctx, mr1, "aaa", "main"); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.NoteID(ctx, mr1); id != 556 {
		t.Errorf("NoteID after SetApprovedSHA = %d", id)
	}
}

func TestStaleApprovals(t *testing.T) {
	s := newStore(t)
	enqueue(t, s, mr1, "A")
	if err := s.SetApprovedSHA(ctx, mr1, "A", "main"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.StaleApprovals(ctx); err != nil || len(got) != 0 {
		t.Fatalf("approved head: %v, %v", got, err)
	}
	enqueue(t, s, mr1, "B")
	got, err := s.StaleApprovals(ctx)
	if err != nil || len(got) != 1 || got[0] != (Approval{MR: mr1, ApprovedSHA: "A", ApprovedTarget: "main"}) {
		t.Fatalf("after new head: %v, %v", got, err)
	}
	if err := s.SetApprovedSHA(ctx, mr1, "B", "main"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StaleApprovals(ctx); len(got) != 0 {
		t.Errorf("approved B: %v", got)
	}
	if err := s.SetApprovedSHA(ctx, mr1, "A", "main"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetApprovedSHA(ctx, mr1, "", "main"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StaleApprovals(ctx); len(got) != 0 {
		t.Errorf("cleared approval: %v", got)
	}
}

func TestClearApprovedSHA(t *testing.T) {
	s := newStore(t)
	enqueue(t, s, mr1, "A")
	if err := s.SetApprovedSHA(ctx, mr1, "A", "main"); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, mr1, "B")
	// Another commit was approved meanwhile: the clear leaves it alone.
	if err := s.ClearApprovedSHA(ctx, mr1, "X", "main"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StaleApprovals(ctx); len(got) != 1 || got[0].ApprovedSHA != "A" {
		t.Fatalf("after a clear of another commit: %v, want A still recorded", got)
	}
	if err := s.ClearApprovedSHA(ctx, mr1, "A", "main"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StaleApprovals(ctx); len(got) != 0 {
		t.Errorf("after clearing A: %v, want none", got)
	}
	// A merge request with no row, and an empty old value, are no-ops.
	if err := s.ClearApprovedSHA(ctx, mr2, "A", "main"); err != nil {
		t.Errorf("clear on unknown MR: %v", err)
	}
	if err := s.ClearApprovedSHA(ctx, mr1, "", "main"); err != nil {
		t.Errorf("clear of empty: %v", err)
	}
	if id, _ := s.NoteID(ctx, mr2); id != 0 {
		t.Errorf("clear created a row for mr2 with note %d", id)
	}
}

func TestProjects(t *testing.T) {
	s := newStore(t)
	inst := "https://gitlab.example.com"
	if _, ok, err := s.Project(ctx, inst, 7); err != nil || ok {
		t.Fatalf("empty = %v, %v", ok, err)
	}
	p := Project{
		Instance: inst, ProjectID: 7, Path: "acme/widgets", HTTPURLToRepo: "https://gitlab.example.com/acme/widgets.git",
		HookID: 11, DeployTokenID: 12, DeployTokenUsername: "gitlab+deploy-token-12",
		DeployTokenSealed: []byte{1, 2, 3}, DeployTokenExpiresAt: t0.Add(30 * 24 * time.Hour), CloneSeconds: 5,
	}
	if err := s.UpsertProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Project(ctx, inst, 7)
	if err != nil || !ok {
		t.Fatalf("Project = %v, %v", ok, err)
	}
	if got.Path != p.Path || got.HookID != 11 || got.DeployTokenID != 12 || !bytes.Equal(got.DeployTokenSealed, p.DeployTokenSealed) ||
		!got.DeployTokenExpiresAt.Equal(p.DeployTokenExpiresAt) || got.CloneSeconds != 5 || got.HTTPURLToRepo != p.HTTPURLToRepo || got.DeployTokenUsername != p.DeployTokenUsername {
		t.Errorf("got %+v", got)
	}

	if err := s.SetCloneSeconds(ctx, inst, 7, 42); err != nil {
		t.Fatal(err)
	}
	// Upsert updates the rest but keeps the measured clone time.
	p.Path = "acme/gadgets"
	p.HookID = 99
	p.CloneSeconds = 0
	if err := s.UpsertProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Project(ctx, inst, 7)
	if got.Path != "acme/gadgets" || got.HookID != 99 || got.CloneSeconds != 42 {
		t.Errorf("after upsert: %+v", got)
	}

	if err := s.UpsertProject(ctx, Project{Instance: inst, ProjectID: 8, Path: "acme/other"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Projects(ctx)
	if err != nil || len(all) != 2 || all[0].ProjectID != 7 || all[1].ProjectID != 8 {
		t.Fatalf("Projects = %+v, %v", all, err)
	}
	if !all[1].DeployTokenExpiresAt.IsZero() || all[1].DeployTokenSealed != nil {
		t.Errorf("empty project has token fields: %+v", all[1])
	}
	if err := s.DeleteProject(ctx, inst, 7); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Project(ctx, inst, 7); ok {
		t.Errorf("project still present")
	}
	if err := s.DeleteProject(ctx, inst, 7); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestMigrateTwice(t *testing.T) {
	s := newStore(t) // already migrated once
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(entries) {
		t.Errorf("schema_migrations rows = %d, %v; want one per migration (%d)", n, err, len(entries))
	}
}

func TestMigrateConcurrent(t *testing.T) {
	url := os.Getenv(testURLEnv)
	if url == "" {
		t.Skipf("%s is not set", testURLEnv)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	s, err := OpenInSchema(ctx, url, "test_"+hex.EncodeToString(b))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.DropSchema(ctx)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { errs <- s.Migrate(ctx) }()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Migrate: %v", err)
		}
	}
}

func TestEnqueueSupersedesReportedRun(t *testing.T) {
	s := newStore(t)
	a := reported(t, s, mr1, "A", `{"status":"completed"}`)
	other := reported(t, s, mr2, "O", `{"status":"completed"}`)
	b := enqueue(t, s, mr1, "B")
	if got := getRun(t, s, a).State; got != Superseded {
		t.Errorf("reported A state = %s, want superseded", got)
	}
	if got := getRun(t, s, b).State; got != Queued {
		t.Errorf("B state = %s", got)
	}
	if got := getRun(t, s, other).State; got != Reported {
		t.Errorf("other MR's reported run = %s, want reported", got)
	}
	if got, _ := s.RunsToPublish(ctx, t0.Add(time.Hour), 10); len(got) != 1 || got[0].ID != other {
		t.Errorf("RunsToPublish = %v, want only the other MR's run", runIDs(got))
	}
	if got, _ := s.RunsToCleanUp(ctx); len(got) != 1 || got[0].ID != a {
		t.Errorf("RunsToCleanUp = %v, want A", runIDs(got))
	}
	// A superseded run's result is never kept, even when it already has one.
	if err := s.MarkPublished(ctx, a); err == nil {
		t.Errorf("MarkPublished on a superseded run succeeded")
	}
	out, err := s.StoreResult(ctx, a, "tok-A", []byte(`{"status":"completed"}`), t0.Add(time.Minute))
	if err != nil || out != ResultDiscarded {
		t.Errorf("StoreResult on superseded reported run = %v, %v; want discarded", out, err)
	}
}

func TestSupersedeMRSupersedesReportedRun(t *testing.T) {
	s := newStore(t)
	a := reported(t, s, mr1, "A", `{"status":"completed"}`)
	other := reported(t, s, mr2, "O", `{"status":"completed"}`)
	n, err := s.SupersedeMR(ctx, mr1)
	if err != nil || n != 1 {
		t.Fatalf("SupersedeMR = %d, %v", n, err)
	}
	if got := getRun(t, s, a).State; got != Superseded {
		t.Errorf("state = %s", got)
	}
	if got := getRun(t, s, other).State; got != Reported {
		t.Errorf("other MR = %s", got)
	}
}

func TestSupersedeMRWaitsForEnqueueLock(t *testing.T) {
	s := newStore(t)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockMR(ctx, tx, mr1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.SupersedeMR(ctx, mr1); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("SupersedeMR finished while the MR lock was held (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SupersedeMR did not finish after the lock was released")
	}
}

func TestMarkDispatchedRequiresRunning(t *testing.T) {
	s := newStore(t)
	mark := func(id string) error {
		return s.MarkDispatched(ctx, id, "job", "b", "s", hash("tok"), t0.Add(time.Hour), t0)
	}
	queued := enqueue(t, s, mr1, "A")
	if err := mark(queued); !errors.Is(err, ErrNotRunning) {
		t.Errorf("queued run: %v, want ErrNotRunning", err)
	}
	if r := getRun(t, s, queued); r.JobName != "" || r.State != Queued {
		t.Errorf("queued run changed: %+v", r)
	}
	// Claimed, then superseded before the Job was recorded.
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, mr1, "B")
	if err := mark(queued); !errors.Is(err, ErrNotRunning) {
		t.Errorf("superseded run: %v, want ErrNotRunning", err)
	}
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if err := mark(id); !errors.Is(err, ErrNotRunning) {
			t.Errorf("id %q: %v, want ErrNotRunning", id, err)
		}
	}
	// The happy path still works on a running run.
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	running, _ := s.RunningRuns(ctx)
	if len(running) != 1 {
		t.Fatalf("running = %d", len(running))
	}
	if err := mark(running[0].ID); err != nil {
		t.Errorf("running run: %v", err)
	}
}

func TestRunLookup(t *testing.T) {
	s := newStore(t)
	id := dispatched(t, s, mr1, "aaa", "tok")
	r, ok, err := s.Run(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Run = %v, %v", ok, err)
	}
	if r.ID != id || r.HeadSHA != "aaa" || r.BaseSHA != "base-aaa" {
		t.Errorf("run = %+v", r)
	}
	if _, ok, err := s.Run(ctx, "00000000-0000-0000-0000-000000000000"); ok || err != nil {
		t.Errorf("unknown id = %v, %v", ok, err)
	}
	if _, ok, err := s.Run(ctx, "not-a-uuid"); ok || err != nil {
		t.Errorf("malformed id = %v, %v", ok, err)
	}
}

func TestReplaceWithFailure(t *testing.T) {
	s := newStore(t)
	id := reported(t, s, mr1, "aaa", `{"status":"completed"}`)
	if err := s.ReplaceWithFailure(ctx, id, "unusable"); err != nil {
		t.Fatalf("ReplaceWithFailure: %v", err)
	}
	r := getRun(t, s, id)
	if r.State != Reported || r.FailureReason != "unusable" || r.Result != nil {
		t.Errorf("run = state %s reason %q result %q", r.State, r.FailureReason, r.Result)
	}

	// A run with a failure but no result, a queued run, and an unknown or
	// malformed id are not replaced.
	if err := s.ReplaceWithFailure(ctx, id, "again"); !errors.Is(err, ErrNotReported) {
		t.Errorf("run without result: %v", err)
	}
	queued := enqueue(t, s, mr2, "bbb")
	if err := s.ReplaceWithFailure(ctx, queued, "x"); !errors.Is(err, ErrNotReported) {
		t.Errorf("queued run: %v", err)
	}
	if r := getRun(t, s, queued); r.State != Queued || r.FailureReason != "" {
		t.Errorf("queued run changed: %+v", r)
	}
	if err := s.ReplaceWithFailure(ctx, "00000000-0000-0000-0000-000000000000", "x"); !errors.Is(err, ErrNotReported) {
		t.Errorf("unknown id: %v", err)
	}
	if err := s.ReplaceWithFailure(ctx, "nope", "x"); !errors.Is(err, ErrNotReported) {
		t.Errorf("malformed id: %v", err)
	}
}

func TestSupersede(t *testing.T) {
	s := newStore(t)
	queued := enqueue(t, s, mr1, "aaa")
	running := dispatched(t, s, mr2, "bbb", "tok")
	done := reported(t, s, MRKey{mr1.Instance, 7, 9}, "ccc", `{"status":"completed"}`)

	for _, id := range []string{queued, running} {
		if err := s.Supersede(ctx, id); err != nil {
			t.Fatalf("Supersede(%s): %v", id, err)
		}
		if got := getRun(t, s, id).State; got != Superseded {
			t.Errorf("run %s state = %s, want superseded", id, got)
		}
	}
	// Already superseded, reported, unknown and malformed ids are left alone.
	for _, id := range []string{queued, done, "00000000-0000-0000-0000-000000000000", "nope"} {
		if err := s.Supersede(ctx, id); err != nil {
			t.Errorf("Supersede(%s): %v", id, err)
		}
	}
	if got := getRun(t, s, done).State; got != Reported {
		t.Errorf("reported run state = %s, want reported", got)
	}
	// The superseded running run's late result is discarded.
	if out, err := s.StoreResult(ctx, running, "tok", []byte(`{"status":"completed"}`), t0.Add(time.Minute)); err != nil || out != ResultDiscarded {
		t.Errorf("StoreResult = %v, %v; want discarded", out, err)
	}
}

func TestPostpone(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "aaa")
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Requeue(ctx, id, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Postpone(ctx, id, t0.Add(time.Minute)); err != nil {
		t.Fatalf("Postpone: %v", err)
	}
	r := getRun(t, s, id)
	if r.State != Queued || r.Attempts != 1 || !r.NotBefore.Equal(t0.Add(time.Minute)) {
		t.Errorf("after Postpone: state %s attempts %d not_before %s; want queued, attempts unchanged at 1", r.State, r.Attempts, r.NotBefore)
	}
	if got, _ := s.ClaimQueued(ctx, 10, t0); len(got) != 0 {
		t.Errorf("postponed run was claimed before not_before")
	}

	// A run that is no longer queued or running is left alone, without error.
	done := reported(t, s, mr2, "bbb", `{"status":"completed"}`)
	sup := enqueue(t, s, MRKey{mr1.Instance, 7, 9}, "ccc")
	if err := s.Supersede(ctx, sup); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{done, sup, "00000000-0000-0000-0000-000000000000", "nope"} {
		if err := s.Postpone(ctx, id, t0); err != nil {
			t.Errorf("Postpone(%s): %v", id, err)
		}
	}
	if got := getRun(t, s, done).State; got != Reported {
		t.Errorf("reported run state = %s", got)
	}
	if got := getRun(t, s, sup).State; got != Superseded {
		t.Errorf("superseded run state = %s", got)
	}
}

// A run claimed but never dispatched (pergent.sh stopped between the claim and
// MarkDispatched) goes back to the queue once its claim is old enough, without
// counting an attempt. Dispatched runs and recent claims are left alone.
func TestRecoverUnlaunched(t *testing.T) {
	s := newStore(t)
	stranded := enqueue(t, s, mr1, "aaaa")
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	// One failed attempt before it got stranded: attempts must stay 1.
	if err := s.Requeue(ctx, stranded, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	launched := dispatched(t, s, mr2, "bbbb", "tok")
	recent := enqueue(t, s, MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 5}, "cccc")
	if _, err := s.ClaimQueued(ctx, 10, t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	n, err := s.RecoverUnlaunched(ctx, t0.Add(5*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("RecoverUnlaunched = %d, %v; want 1", n, err)
	}
	r := getRun(t, s, stranded)
	if r.State != Queued || r.Attempts != 1 {
		t.Errorf("stranded run = %s with %d attempts, want queued with 1", r.State, r.Attempts)
	}
	if r.NotBefore.IsZero() || r.NotBefore.After(time.Now().Add(time.Minute)) {
		t.Errorf("not_before = %s, want about now", r.NotBefore)
	}
	if got := getRun(t, s, launched).State; got != Running {
		t.Errorf("dispatched run = %s, want running", got)
	}
	if got := getRun(t, s, recent).State; got != Running {
		t.Errorf("recently claimed run = %s, want running", got)
	}

	// It is claimable again.
	claimed, err := s.ClaimQueued(ctx, 10, time.Now().Add(time.Minute))
	if err != nil || len(claimed) != 1 || claimed[0].ID != stranded {
		t.Errorf("ClaimQueued after recovery = %v, %v; want the stranded run", runIDs(claimed), err)
	}

	if n, err := s.RecoverUnlaunched(ctx, t0); err != nil || n != 0 {
		t.Errorf("second RecoverUnlaunched = %d, %v; want 0", n, err)
	}
}

func TestEnqueueRetargetIsANewRun(t *testing.T) {
	s := newStore(t)
	a := enqueue(t, s, mr1, "aaa")
	b, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "develop")
	if err != nil || !created || b == a {
		t.Fatalf("retarget = %s %v %v; want a new run", b, created, err)
	}
	if got := getRun(t, s, a).State; got != Superseded {
		t.Errorf("run for the old target = %s, want superseded", got)
	}
	if r := getRun(t, s, b); r.HeadSHA != "aaa" || r.TargetBranch != "develop" || r.State != Queued {
		t.Errorf("new run = %+v", r)
	}
	// The same head and target again is the same run.
	if c, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "develop"); err != nil || created || c != b {
		t.Errorf("same head and target = %s %v %v; want %s", c, created, err, b)
	}
}

func TestEnqueueRetargetKeepsPublishedRunsOfOtherTarget(t *testing.T) {
	s := newStore(t)
	a := reported(t, s, mr1, "aaa", `{"status":"completed"}`)
	if err := s.MarkPublished(ctx, a); err != nil {
		t.Fatal(err)
	}
	b, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "develop")
	if err != nil || !created || b == a {
		t.Fatalf("retarget after publish = %s %v %v", b, created, err)
	}
	if got := getRun(t, s, a).State; got != Published {
		t.Errorf("published run = %s, want published (history)", got)
	}
}

// published runs head against target on mr through dispatch and a stored
// result to published, and returns its id.
func published(t *testing.T, s *Store, mr MRKey, head, target string) string {
	t.Helper()
	id, created, err := s.EnqueueRun(ctx, mr, "acme/widgets", head, target)
	if err != nil || !created {
		t.Fatalf("EnqueueRun(%s, %s) = %v, %v; want a new run", head, target, created, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	tok := "tok-" + head + "-" + target
	if err := s.MarkDispatched(ctx, id, "job-"+id, "base", "start", hash(tok), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, tok, []byte(`{"status":"completed"}`), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := s.MarkPublished(ctx, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Retargeting back to a branch the head was already reviewed against is a
// fresh run: the comment, the status and the approval show the newest
// review, and commit statuses are per commit, not per target. The old
// published run is superseded as history: its Job finished and its status
// was posted, so there is nothing to clean up or cancel.
func TestEnqueueRetargetBackIsANewRun(t *testing.T) {
	s := newStore(t)
	mainRun := published(t, s, mr1, "aaa", "main")
	devRun := published(t, s, mr1, "aaa", "develop")

	back, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "main")
	if err != nil || !created || back == mainRun {
		t.Fatalf("retarget back to main = %s %v %v; want a new run", back, created, err)
	}
	if r := getRun(t, s, back); r.HeadSHA != "aaa" || r.TargetBranch != "main" || r.State != Queued {
		t.Errorf("new run = %+v", r)
	}
	if r := getRun(t, s, mainRun); r.State != Superseded || !r.CleanedUp {
		t.Errorf("old main run = %s cleaned_up=%v; want superseded and cleaned up", r.State, r.CleanedUp)
	}
	if got := getRun(t, s, devRun).State; got != Published {
		t.Errorf("develop run = %s, want published (history)", got)
	}
	if got, err := s.RunsToCleanUp(ctx); err != nil || len(got) != 0 {
		t.Errorf("RunsToCleanUp = %v, %v; want nothing to cancel", runIDs(got), err)
	}
	// The same head and target again is the same run.
	if again, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "main"); err != nil || created || again != back {
		t.Errorf("same head and target = %s %v %v; want %s", again, created, err, back)
	}
}

// A head pushed again after another head (A, B, then A) is reviewed again
// for the same reason.
func TestEnqueueHeadBackIsANewRun(t *testing.T) {
	s := newStore(t)
	a := published(t, s, mr1, "aaa", "main")
	published(t, s, mr1, "bbb", "main")
	a2, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "main")
	if err != nil || !created || a2 == a {
		t.Fatalf("push back to A = %s %v %v; want a new run", a2, created, err)
	}
}

// A repeated webhook for the newest run's head and target, published or
// not, is not a new review.
func TestEnqueueNewestPublishedRunIsKept(t *testing.T) {
	s := newStore(t)
	a := published(t, s, mr1, "aaa", "main")
	again, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "main")
	if err != nil || created || again != a {
		t.Fatalf("repeat = %s %v %v; want %s", again, created, err, a)
	}
	if got := getRun(t, s, a).State; got != Published {
		t.Errorf("run = %s, want published", got)
	}
}

// A run for the newest target that hasn't published yet is newer than the
// old run of the target the merge request moves back to: moving back while
// it is live still gives a fresh run.
func TestEnqueueRetargetBackWhileOtherTargetLive(t *testing.T) {
	s := newStore(t)
	mainRun := published(t, s, mr1, "aaa", "main")
	dev, _, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "develop")
	if err != nil {
		t.Fatal(err)
	}
	back, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "main")
	if err != nil || !created || back == mainRun {
		t.Fatalf("retarget back = %s %v %v; want a new run", back, created, err)
	}
	if got := getRun(t, s, dev).State; got != Superseded {
		t.Errorf("develop run = %s, want superseded", got)
	}
}

func TestStaleApprovalsAfterRetarget(t *testing.T) {
	s := newStore(t)
	enqueue(t, s, mr1, "A")
	if err := s.SetApprovedSHA(ctx, mr1, "A", "main"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "A", "develop"); err != nil {
		t.Fatal(err)
	}
	got, err := s.StaleApprovals(ctx)
	if err != nil || len(got) != 1 || got[0] != (Approval{MR: mr1, ApprovedSHA: "A", ApprovedTarget: "main"}) {
		t.Fatalf("after retarget: %v, %v; want the approval for main stale", got, err)
	}
	// Clearing needs both the commit and the target to match.
	if err := s.ClearApprovedSHA(ctx, mr1, "A", "develop"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StaleApprovals(ctx); len(got) != 1 {
		t.Fatalf("a clear for another target removed the approval: %v", got)
	}
	if err := s.ClearApprovedSHA(ctx, mr1, "A", "main"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StaleApprovals(ctx); len(got) != 0 {
		t.Errorf("after clear: %v", got)
	}
	// An approval recorded without a target (before targets were recorded)
	// goes stale only on a new head.
	if err := s.SetApprovedSHA(ctx, mr1, "A", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StaleApprovals(ctx); len(got) != 0 {
		t.Errorf("targetless approval of the head: %v", got)
	}
}

func TestApprovals(t *testing.T) {
	s := newStore(t)
	if got, err := s.Approvals(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	if err := s.SetNoteID(ctx, mr2, 9); err != nil { // a row without an approval
		t.Fatal(err)
	}
	if err := s.SetApprovedSHA(ctx, mr1, "A", "main"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Approvals(ctx)
	if err != nil || len(got) != 1 || got[0] != (Approval{MR: mr1, ApprovedSHA: "A", ApprovedTarget: "main"}) {
		t.Fatalf("Approvals = %v, %v", got, err)
	}
	if err := s.SetApprovedSHA(ctx, mr1, "", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Approvals(ctx); len(got) != 0 {
		t.Errorf("after clear: %v", got)
	}
}

// Migration 0003 records the target of approvals made before it, from the
// newest run of the approved commit.
func TestMigration0003BackfillsApprovedTarget(t *testing.T) {
	s := newStore(t)
	enqueue(t, s, mr1, "A")
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET target_branch = 'release'`); err != nil {
		t.Fatal(err)
	}
	// Roll the schema back to before 0003, with an approval of A.
	if _, err := s.pool.Exec(ctx, `
		DROP INDEX runs_live_idx;
		CREATE UNIQUE INDEX runs_live_head_idx ON runs (instance, project_id, mr_iid, head_sha) WHERE state <> 'superseded';
		ALTER TABLE merge_requests DROP COLUMN approved_target;
		DELETE FROM schema_migrations WHERE version = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO merge_requests (instance, project_id, mr_iid, approved_sha) VALUES ($1, $2, $3, 'A')`,
		mr1.Instance, mr1.ProjectID, mr1.IID); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Approvals(ctx)
	if err != nil || len(got) != 1 || got[0].ApprovedTarget != "release" {
		t.Errorf("Approvals = %v, %v; want target release", got, err)
	}
}

// Republish puts the newest run back to reported when it is published at the
// given head, so the publisher decides again (a draft marked ready).
func TestRepublish(t *testing.T) {
	s := newStore(t)
	id := published(t, s, mr1, "A", "main")
	if err := s.PublishRetry(ctx, id, "", t0); err == nil {
		t.Fatal("PublishRetry on a published run should fail")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET attempts = 3, publish_error = 'old', not_before = $2 WHERE id = $1::uuid`, id, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	ok, err := s.Republish(ctx, mr1, "A")
	if err != nil || !ok {
		t.Fatalf("Republish = %v, %v; want true", ok, err)
	}
	r := getRun(t, s, id)
	if r.State != Reported || r.Attempts != 0 || r.PublishError != "" || !r.NotBefore.IsZero() {
		t.Errorf("run = state %s, attempts %d, error %q, not_before %v; want reported, 0, '', unset", r.State, r.Attempts, r.PublishError, r.NotBefore)
	}
	due, err := s.RunsToPublish(ctx, t0, 10)
	if err != nil || len(due) != 1 || due[0].ID != id {
		t.Errorf("RunsToPublish = %v, %v; want the republished run", runIDs(due), err)
	}

	// Already reported: nothing to do.
	if ok, err := s.Republish(ctx, mr1, "A"); err != nil || ok {
		t.Errorf("second Republish = %v, %v; want false", ok, err)
	}
}

func TestRepublishOnlyTheNewestPublishedHead(t *testing.T) {
	s := newStore(t)
	old := published(t, s, mr1, "A", "main")
	newer := reported(t, s, mr1, "B", `{"status":"completed"}`)

	for name, tc := range map[string]struct {
		mr   MRKey
		head string
	}{
		"older head":            {mr1, "A"},
		"newest not published":  {mr1, "B"},
		"unknown head":          {mr1, "C"},
		"another merge request": {mr2, "A"},
	} {
		t.Run(name, func(t *testing.T) {
			if ok, err := s.Republish(ctx, tc.mr, tc.head); err != nil || ok {
				t.Errorf("Republish = %v, %v; want false", ok, err)
			}
		})
	}
	if r := getRun(t, s, old); r.State != Published {
		t.Errorf("old run state = %s, want published", r.State)
	}
	if r := getRun(t, s, newer); r.State != Reported || r.Attempts != 0 {
		t.Errorf("newest run = %s, %d", r.State, r.Attempts)
	}
}

// A published run of the head that isn't the newest run (another target was
// queued since) is not republished.
func TestRepublishNotNewest(t *testing.T) {
	s := newStore(t)
	id := published(t, s, mr1, "A", "main")
	if _, _, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "A", "develop"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Republish(ctx, mr1, "A"); err != nil || ok {
		t.Errorf("Republish = %v, %v; want false", ok, err)
	}
	if r := getRun(t, s, id); r.State != Published {
		t.Errorf("run state = %s, want published", r.State)
	}
}

// Migration 0004 adds the GitHub fields; a row stored before it gets
// installation 0 and an approve side that is available.
func TestProjectGitHubFields(t *testing.T) {
	s := newStore(t)
	inst := "https://gitlab.example.com"
	if _, err := s.pool.Exec(ctx, `INSERT INTO projects (instance, project_id, path) VALUES ($1, 5, 'acme/old')`, inst); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Project(ctx, inst, 5)
	if err != nil || !ok || got.InstallationID != 0 || !got.ApproveAvailable {
		t.Fatalf("defaults = %+v, %v, %v; want installation 0, approve available", got, ok, err)
	}

	// A GitLab row (no installation) keeps true whatever the caller passes:
	// reconcile builds GitLab rows without the field.
	if err := s.UpsertProject(ctx, Project{Instance: inst, ProjectID: 5, Path: "acme/old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertProject(ctx, Project{Instance: inst, ProjectID: 6, Path: "acme/new"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{5, 6} {
		if got, _, _ := s.Project(ctx, inst, id); !got.ApproveAvailable {
			t.Errorf("GitLab project %d: approve available = false, want true", id)
		}
	}

	// A GitHub row stores both fields as given, on insert and on update.
	gh := Project{Instance: "github", ProjectID: 900, Path: "acme/widgets", InstallationID: 12345, ApproveAvailable: false}
	if err := s.UpsertProject(ctx, gh); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Project(ctx, "github", 900)
	if got.InstallationID != 12345 || got.ApproveAvailable {
		t.Errorf("GitHub insert = %+v; want installation 12345, approve unavailable", got)
	}
	gh.InstallationID, gh.ApproveAvailable = 777, true
	if err := s.UpsertProject(ctx, gh); err != nil {
		t.Fatal(err)
	}
	all, err := s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range all {
		if p.Instance == "github" {
			found = true
			if p.InstallationID != 777 || !p.ApproveAvailable {
				t.Errorf("GitHub update = %+v; want installation 777, approve available", p)
			}
		}
	}
	if !found {
		t.Error("Projects lacks the GitHub row")
	}
}
