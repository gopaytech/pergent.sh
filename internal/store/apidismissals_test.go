package store

import (
	"testing"
	"time"
)

func apiDismissal(clientID int64, tenant, subject, fingerprint string) APIDismissal {
	return APIDismissal{ClientID: clientID, Tenant: tenant, Subject: subject, Fingerprint: fingerprint,
		Severity: "blocking", Skill: "security", File: "a.go", Title: "SQL built from input",
		Reason: "the input is checked upstream", Actor: "key ci"}
}

// A finding dismissed twice is one row and one event; the second call leaves
// the first one's reason and actor.
func TestDismissAPIFindingTwiceIsNoop(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "gateway")
	run := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))

	d := apiDismissal(c.ID, "acme", "route:1", "fp1")
	created, err := s.DismissAPIFinding(ctx, d, run)
	if err != nil || !created {
		t.Fatalf("first DismissAPIFinding = %v, %v; want created", created, err)
	}
	again := d
	again.Reason, again.Actor = "another reason", "admin"
	created, err = s.DismissAPIFinding(ctx, again, run)
	if err != nil || created {
		t.Fatalf("second DismissAPIFinding = %v, %v; want not created", created, err)
	}

	got, err := s.APIDismissals(ctx, c.ID, "acme", "route:1")
	if err != nil || len(got) != 1 {
		t.Fatalf("APIDismissals = %+v, %v; want one row", got, err)
	}
	if g := got[0]; g.Reason != d.Reason || g.Actor != d.Actor || g.Severity != "blocking" || g.Skill != "security" ||
		g.File != "a.go" || g.Title != d.Title || g.CreatedAt.IsZero() {
		t.Errorf("row = %+v, want the first dismissal's", g)
	}
	events, err := s.APIReviewEvents(ctx, run)
	if err != nil || len(events) != 1 {
		t.Fatalf("APIReviewEvents = %+v, %v; want one event", events, err)
	}
	if e := events[0]; e.Kind != APIEventDismissed || e.Fingerprint != "fp1" || e.Title != d.Title ||
		e.Actor != "key ci" || e.Reason != d.Reason || e.At.IsZero() {
		t.Errorf("event = %+v", e)
	}
}

// A restore removes the row, and records who did it on the review it was done
// on; restoring what is not dismissed changes nothing.
func TestUndismissAPIFinding(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "gateway")
	run := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))

	restored, err := s.UndismissAPIFinding(ctx, c.ID, "acme", "route:1", "fp1", run, "key ci")
	if err != nil || restored {
		t.Fatalf("restoring nothing = %v, %v; want false", restored, err)
	}
	if events, _ := s.APIReviewEvents(ctx, run); len(events) != 0 {
		t.Fatalf("restoring nothing left events: %+v", events)
	}

	if _, err := s.DismissAPIFinding(ctx, apiDismissal(c.ID, "acme", "route:1", "fp1"), run); err != nil {
		t.Fatal(err)
	}
	restored, err = s.UndismissAPIFinding(ctx, c.ID, "acme", "route:1", "fp1", run, "admin")
	if err != nil || !restored {
		t.Fatalf("UndismissAPIFinding = %v, %v; want true", restored, err)
	}
	if got, _ := s.APIDismissals(ctx, c.ID, "acme", "route:1"); len(got) != 0 {
		t.Errorf("the dismissal is still there: %+v", got)
	}
	events, err := s.APIReviewEvents(ctx, run)
	if err != nil || len(events) != 2 {
		t.Fatalf("APIReviewEvents = %+v, %v; want two", events, err)
	}
	if e := events[1]; e.Kind != APIEventUndismissed || e.Fingerprint != "fp1" || e.Title != "SQL built from input" ||
		e.Actor != "admin" || e.Reason != "" {
		t.Errorf("undismissed event = %+v", e)
	}
	if restored, _ := s.UndismissAPIFinding(ctx, c.ID, "acme", "route:1", "fp1", run, "admin"); restored {
		t.Error("a second restore found a dismissal")
	}
	if events, _ := s.APIReviewEvents(ctx, run); len(events) != 2 {
		t.Errorf("a second restore recorded an event: %+v", events)
	}

	// The same finding can be dismissed again after a restore.
	if created, err := s.DismissAPIFinding(ctx, apiDismissal(c.ID, "acme", "route:1", "fp1"), run); err != nil || !created {
		t.Errorf("dismissing again after a restore = %v, %v", created, err)
	}
}

// The list is one client's, one tenant's, one subject's; oldest first.
func TestAPIDismissalsBySubject(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "gateway")
	other := newAPIClient(t, s, "other")
	run := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:1"))
	otherRun := enqueueAPI(t, s, apiRequest(other.ID, "acme", "route:1"))

	older := apiDismissal(c.ID, "acme", "route:1", "fp-b")
	older.CreatedAt = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	newer := apiDismissal(c.ID, "acme", "route:1", "fp-a")
	newer.CreatedAt = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	for _, d := range []APIDismissal{newer, older} {
		if _, err := s.DismissAPIFinding(ctx, d, run); err != nil {
			t.Fatal(err)
		}
	}
	// Left out: another tenant, another subject, another client.
	for _, d := range []APIDismissal{
		apiDismissal(c.ID, "beta", "route:1", "fp-x"),
		apiDismissal(c.ID, "acme", "route:2", "fp-y"),
	} {
		if _, err := s.DismissAPIFinding(ctx, d, run); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DismissAPIFinding(ctx, apiDismissal(other.ID, "acme", "route:1", "fp-z"), otherRun); err != nil {
		t.Fatal(err)
	}

	got, err := s.APIDismissals(ctx, c.ID, "acme", "route:1")
	if err != nil || len(got) != 2 || got[0].Fingerprint != "fp-b" || got[1].Fingerprint != "fp-a" {
		t.Fatalf("APIDismissals = %+v, %v; want fp-b then fp-a", got, err)
	}
	if !got[0].CreatedAt.Equal(older.CreatedAt) {
		t.Errorf("created at %v, want %v", got[0].CreatedAt, older.CreatedAt)
	}
	if got, _ := s.APIDismissals(ctx, c.ID, "acme", "nobody"); len(got) != 0 {
		t.Errorf("an unknown subject has dismissals: %+v", got)
	}
	if events, _ := s.APIReviewEvents(ctx, otherRun); len(events) != 1 || events[0].Fingerprint != "fp-z" {
		t.Errorf("events of the other client's review = %+v", events)
	}
	if events, err := s.APIReviewEvents(ctx, "not-an-id"); err != nil || len(events) != 0 {
		t.Errorf("a malformed ID = %+v, %v; want none", events, err)
	}
}
