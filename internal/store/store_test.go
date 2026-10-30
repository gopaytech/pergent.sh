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
	if err := s.SetApprovedSHA(ctx, mr1, "aaa"); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.NoteID(ctx, mr1); id != 556 {
		t.Errorf("NoteID after SetApprovedSHA = %d", id)
	}
}

func TestStaleApprovals(t *testing.T) {
	s := newStore(t)
	enqueue(t, s, mr1, "A")
	if err := s.SetApprovedSHA(ctx, mr1, "A"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.StaleApprovals(ctx); err != nil || len(got) != 0 {
		t.Fatalf("approved head: %v, %v", got, err)
	}
	enqueue(t, s, mr1, "B")
	got, err := s.StaleApprovals(ctx)
	if err != nil || len(got) != 1 || got[0] != (StaleApproval{MR: mr1, ApprovedSHA: "A"}) {
		t.Fatalf("after new head: %v, %v", got, err)
	}
	if err := s.SetApprovedSHA(ctx, mr1, "B"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StaleApprovals(ctx); len(got) != 0 {
		t.Errorf("approved B: %v", got)
	}
	if err := s.SetApprovedSHA(ctx, mr1, "A"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetApprovedSHA(ctx, mr1, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.StaleApprovals(ctx); len(got) != 0 {
		t.Errorf("cleared approval: %v", got)
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
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != 1 {
		t.Errorf("schema_migrations rows = %d, %v; want 1", n, err)
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
