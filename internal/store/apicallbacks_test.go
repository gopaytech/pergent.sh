package store

import (
	"bytes"
	"testing"
	"time"
)

const testCallbackURL = "https://hooks.example.com/pergent"

// newCallbackClient is a client with a callback URL.
func newCallbackClient(t *testing.T, s *Store, name string) APIClient {
	t.Helper()
	newAPIClient(t, s, name)
	c, _, err := s.UpdateAPIClient(ctx, name, func(c *APIClient) { c.CallbackURL = testCallbackURL })
	if err != nil {
		t.Fatalf("UpdateAPIClient(%s): %v", name, err)
	}
	return c
}

// callbackState is the state of run id's callback; it fails the test on an
// error.
func callbackState(t *testing.T, s *Store, id string) (string, []CallbackAttempt) {
	t.Helper()
	state, attempts, err := s.APICallbackAttempts(ctx, id)
	if err != nil {
		t.Fatalf("APICallbackAttempts(%s): %v", id, err)
	}
	return state, attempts
}

// dueFor returns the callbacks due at now, by run ID.
func dueFor(t *testing.T, s *Store, now time.Time) map[string]APICallback {
	t.Helper()
	due, err := s.DueAPICallbacks(ctx, now, 100)
	if err != nil {
		t.Fatalf("DueAPICallbacks: %v", err)
	}
	out := map[string]APICallback{}
	for _, cb := range due {
		out[cb.RunID] = cb
	}
	return out
}

// A review superseded by a request of its subject, and a finished one, each
// get a callback, only when their client has a callback URL.
func TestAPICallbackRowsMade(t *testing.T) {
	s := newStore(t)
	c := newCallbackClient(t, s, "gateway")
	none := newAPIClient(t, s, "quiet")

	superseded := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	live := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	quietOld := enqueueAPI(t, s, apiRequest(none.ID, "acme", "route:1"))
	enqueueAPI(t, s, apiRequest(none.ID, "acme", "route:1"))

	if st, _ := callbackState(t, s, superseded); st != "pending" {
		t.Errorf("superseded review's callback = %q, want pending", st)
	}
	if st, _ := callbackState(t, s, live); st != "none" {
		t.Errorf("queued review's callback = %q, want none", st)
	}
	if st, _ := callbackState(t, s, quietOld); st != "none" {
		t.Errorf("a client with no callback URL got a callback: %q", st)
	}

	setRun(t, s, live, `state = 'reported', job_name = 'job-1', result = '{"status":"completed"}'::jsonb`)
	if err := s.FinishAPIReview(ctx, live, []byte(`{"status":"completed"}`), RunDecision{Outcome: OutcomeNoBlocking}); err != nil {
		t.Fatalf("FinishAPIReview: %v", err)
	}
	if st, _ := callbackState(t, s, live); st != "pending" {
		t.Errorf("finished review's callback = %q, want pending", st)
	}

	// A publish that gave up queues its own; one that exists is kept as it is.
	failed := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:2"))
	setRun(t, s, failed, `state = 'publish_failed'`)
	for range 2 {
		if err := s.EnqueueAPICallback(ctx, failed); err != nil {
			t.Fatalf("EnqueueAPICallback: %v", err)
		}
	}
	due := dueFor(t, s, time.Now().Add(time.Minute))
	if len(due) != 3 {
		t.Errorf("due = %d callbacks, want 3 (superseded, finished, gave up)", len(due))
	}
	cb := due[failed]
	if cb.ClientID != c.ID || cb.ClientName != "gateway" || cb.URL != testCallbackURL || cb.Attempts != 0 || cb.CreatedAt.IsZero() {
		t.Errorf("callback = %+v", cb)
	}
	if !bytes.Equal(cb.SecretSealed, []byte("sealed-gateway")) || cb.PreviousSealed != nil || !cb.PreviousUntil.IsZero() {
		t.Errorf("secrets = %q, %q, %v", cb.SecretSealed, cb.PreviousSealed, cb.PreviousUntil)
	}
	if err := s.EnqueueAPICallback(ctx, superseded); err != nil {
		t.Fatalf("EnqueueAPICallback of a review that has one: %v", err)
	}
	if got := dueFor(t, s, time.Now().Add(time.Minute))[superseded]; got.ID != due[superseded].ID {
		t.Errorf("a second enqueue made callback %s, want %s kept", got.ID, due[superseded].ID)
	}
	// Not an API review, an unknown or a malformed run: nothing, no error.
	for _, id := range []string{enqueue(t, s, mr1, "aaa"), "00000000-0000-0000-0000-000000000000", "x"} {
		if err := s.EnqueueAPICallback(ctx, id); err != nil {
			t.Errorf("EnqueueAPICallback(%s): %v", id, err)
		}
	}
	if n := len(dueFor(t, s, time.Now().Add(time.Minute))); n != 3 {
		t.Errorf("due = %d callbacks after the no-ops, want 3", n)
	}
	if st, _ := callbackState(t, s, "00000000-0000-0000-0000-000000000000"); st != "none" {
		t.Errorf("unknown run's callback = %q, want none", st)
	}
}

// The URL and secrets are read when the callbacks are listed, so a change
// since the review ended is what is used.
func TestDueAPICallbacksReadClientNow(t *testing.T) {
	s := newStore(t)
	c := newCallbackClient(t, s, "gateway")
	old := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	until := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	if _, _, err := s.UpdateAPIClient(ctx, "gateway", func(c *APIClient) {
		c.CallbackURL = "https://other.example.com/hook"
		c.PreviousSecretSealed, c.PreviousSecretUntil = c.CallbackSecretSealed, until
		c.CallbackSecretSealed = []byte("sealed-new")
	}); err != nil {
		t.Fatal(err)
	}
	cb := dueFor(t, s, time.Now().Add(time.Minute))[old]
	if cb.URL != "https://other.example.com/hook" || string(cb.SecretSealed) != "sealed-new" ||
		string(cb.PreviousSealed) != "sealed-gateway" || !cb.PreviousUntil.Equal(until) {
		t.Errorf("callback = %+v", cb)
	}
	// Not due before its time.
	if n := len(dueFor(t, s, time.Now().Add(-time.Hour))); n != 0 {
		t.Errorf("due an hour ago = %d, want 0", n)
	}
}

func TestRecordAPICallbackAttempt(t *testing.T) {
	s := newStore(t)
	c := newCallbackClient(t, s, "gateway")
	id := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	cb := dueFor(t, s, time.Now().Add(time.Minute))[id]

	at := time.Now().UTC().Truncate(time.Millisecond)
	next := at.Add(10 * time.Second)
	failed := CallbackAttempt{At: at, StatusCode: 500, Error: "", Duration: 120 * time.Millisecond}
	if err := s.RecordAPICallbackAttempt(ctx, cb.ID, failed, false, next, false); err != nil {
		t.Fatalf("RecordAPICallbackAttempt: %v", err)
	}
	if _, ok := dueFor(t, s, next.Add(-time.Second))[id]; ok {
		t.Error("due before its next try")
	}
	again, ok := dueFor(t, s, next)[id]
	if !ok || again.Attempts != 1 || again.ID != cb.ID {
		t.Fatalf("at its next try: due %v, %+v", ok, again)
	}

	timeout := CallbackAttempt{At: next, Error: "Post \"https://hooks.example.com/pergent\": timeout", Duration: 10 * time.Second}
	if err := s.RecordAPICallbackAttempt(ctx, cb.ID, timeout, false, next.Add(20*time.Second), false); err != nil {
		t.Fatal(err)
	}
	state, attempts := callbackState(t, s, id)
	if state != "pending" || len(attempts) != 2 {
		t.Fatalf("state %q, %d attempts; want pending, 2", state, len(attempts))
	}
	if a := attempts[0]; !a.At.Equal(at) || a.StatusCode != 500 || a.Error != "" || a.Duration != 120*time.Millisecond {
		t.Errorf("first attempt = %+v", a)
	}
	if a := attempts[1]; !a.At.Equal(next) || a.StatusCode != 0 || a.Error != timeout.Error || a.Duration != 10*time.Second {
		t.Errorf("second attempt = %+v", a)
	}

	ok200 := CallbackAttempt{At: next.Add(20 * time.Second), StatusCode: 200, Duration: time.Millisecond}
	if err := s.RecordAPICallbackAttempt(ctx, cb.ID, ok200, true, time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	if state, attempts := callbackState(t, s, id); state != "delivered" || len(attempts) != 3 {
		t.Errorf("state %q, %d attempts; want delivered, 3", state, len(attempts))
	}
	if _, ok := dueFor(t, s, time.Now().Add(48*time.Hour))[id]; ok {
		t.Error("a delivered callback is due again")
	}
	// A finished callback takes no more attempts.
	if err := s.RecordAPICallbackAttempt(ctx, cb.ID, failed, false, next, true); err != nil {
		t.Fatal(err)
	}
	if state, attempts := callbackState(t, s, id); state != "delivered" || len(attempts) != 3 {
		t.Errorf("after a late attempt: state %q, %d attempts; want delivered, 3", state, len(attempts))
	}
}

func TestAPICallbackGivesUp(t *testing.T) {
	s := newStore(t)
	c := newCallbackClient(t, s, "gateway")
	id := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	cb := dueFor(t, s, time.Now().Add(time.Minute))[id]

	a := CallbackAttempt{At: time.Now(), Error: "the client has no callback URL"}
	if err := s.RecordAPICallbackAttempt(ctx, cb.ID, a, false, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if state, attempts := callbackState(t, s, id); state != "gave_up" || len(attempts) != 1 || attempts[0].Error != a.Error {
		t.Errorf("state %q, attempts %+v; want gave_up with the one attempt", state, attempts)
	}
	if _, ok := dueFor(t, s, time.Now().Add(48*time.Hour))[id]; ok {
		t.Error("a callback that gave up is due again")
	}
}

// A removed client takes its callbacks and their attempts with it, and its
// callback_failing item; an attempt recorded after that is no error.
func TestDeleteAPIClientDropsCallbacks(t *testing.T) {
	s := newStore(t)
	c := newCallbackClient(t, s, "gateway")
	id := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	cb := dueFor(t, s, time.Now().Add(time.Minute))[id]
	if err := s.RecordAPICallbackAttempt(ctx, cb.ID, CallbackAttempt{At: time.Now(), StatusCode: 500}, false, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttention(ctx, "", "gateway", "callback_failing", "failing"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttention(ctx, "", "other", "callback_failing", "failing"); err != nil {
		t.Fatal(err)
	}

	if _, found, err := s.DeleteAPIClient(ctx, "gateway"); err != nil || !found {
		t.Fatalf("DeleteAPIClient: found=%v err=%v", found, err)
	}
	if n := len(dueFor(t, s, time.Now().Add(48*time.Hour))); n != 0 {
		t.Errorf("due after the delete = %d, want 0", n)
	}
	if state, attempts := callbackState(t, s, id); state != "none" || len(attempts) != 0 {
		t.Errorf("state %q, %d attempts; want none", state, len(attempts))
	}
	var left int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM api_callback_attempts`).Scan(&left); err != nil || left != 0 {
		t.Errorf("attempts left = %d (%v), want 0", left, err)
	}
	if err := s.RecordAPICallbackAttempt(ctx, cb.ID, CallbackAttempt{At: time.Now(), StatusCode: 200}, true, time.Time{}, false); err != nil {
		t.Errorf("an attempt of a removed callback: %v", err)
	}
	att, err := s.Attention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(att) != 1 || att[0].Project != "other" {
		t.Errorf("attention = %+v; want only the other client's item", att)
	}
}

// Undelivered counts the callbacks of a client made since a time, not
// delivered, with at least one failed attempt.
func TestCountUndeliveredAPICallbacks(t *testing.T) {
	s := newStore(t)
	c := newCallbackClient(t, s, "gateway")
	other := newCallbackClient(t, s, "other")
	var ids []string
	for range 4 {
		ids = append(ids, enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1")))
	}
	for range 2 {
		enqueueAPI(t, s, apiRequest(other.ID, "acme", "route:1"))
	}
	due := dueFor(t, s, time.Now().Add(time.Minute))
	since := time.Now().Add(-time.Hour)
	count := func() int {
		t.Helper()
		n, err := s.CountUndeliveredAPICallbacks(ctx, c.ID, since)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 0 {
		t.Errorf("before any attempt = %d, want 0", n)
	}
	fail := CallbackAttempt{At: time.Now(), StatusCode: 500}
	for _, id := range ids[:3] {
		if err := s.RecordAPICallbackAttempt(ctx, due[id].ID, fail, false, time.Now(), false); err != nil {
			t.Fatal(err)
		}
	}
	for id, cb := range due {
		if cb.ClientID == other.ID {
			if err := s.RecordAPICallbackAttempt(ctx, cb.ID, fail, false, time.Now(), false); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
		}
	}
	if n := count(); n != 3 {
		t.Errorf("after three failed = %d, want 3 (the other client's not counted)", n)
	}
	if err := s.RecordAPICallbackAttempt(ctx, due[ids[0]].ID, CallbackAttempt{At: time.Now(), StatusCode: 204}, true, time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Errorf("after a delivery = %d, want 2", n)
	}
	// One that gave up is still undelivered.
	if err := s.RecordAPICallbackAttempt(ctx, due[ids[1]].ID, fail, false, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Errorf("after a give-up = %d, want 2", n)
	}
	// Made before since: not counted.
	if n, err := s.CountUndeliveredAPICallbacks(ctx, c.ID, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Errorf("since an hour ahead = %d, %v; want 0", n, err)
	}
}
