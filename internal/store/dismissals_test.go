package store

import (
	"testing"
	"time"
)

func TestDismissSeverityRaise(t *testing.T) {
	s := newStore(t)
	actor := int64(5)
	first := Dismissal{MR: mr1, Fingerprint: "fp1", Severity: "suggestion", Reason: "fine", Actor: "alice (gitlab main)", ActorID: &actor}

	created, err := s.Dismiss(ctx, first)
	if err != nil || !created {
		t.Fatalf("first Dismiss = %v, %v", created, err)
	}
	// The same severity again changes nothing.
	again := first
	again.Reason, again.Actor = "other", "bob"
	if created, err := s.Dismiss(ctx, again); err != nil || created {
		t.Fatalf("same severity: Dismiss = %v, %v; want false", created, err)
	}
	got, err := s.Dismissals(ctx, mr1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Dismissals = %+v, %v", got, err)
	}
	d := got[0]
	if d.MR != mr1 || d.Fingerprint != "fp1" || d.Severity != "suggestion" || d.Reason != "fine" ||
		d.Actor != "alice (gitlab main)" || d.ActorID == nil || *d.ActorID != 5 || d.CreatedAt.IsZero() {
		t.Errorf("dismissal = %+v", d)
	}

	// A higher severity raises it, and the new reason and actor replace the old.
	blocking := Dismissal{MR: mr1, Fingerprint: "fp1", Severity: "blocking", Reason: "false positive", Actor: "carol (gitlab main)"}
	if created, err := s.Dismiss(ctx, blocking); err != nil || !created {
		t.Fatalf("raise: Dismiss = %v, %v; want true", created, err)
	}
	got, _ = s.Dismissals(ctx, mr1)
	if len(got) != 1 || got[0].Severity != "blocking" || got[0].Reason != "false positive" ||
		got[0].Actor != "carol (gitlab main)" || got[0].ActorID != nil {
		t.Errorf("after raise = %+v", got)
	}

	// A lower severity never lowers it.
	if created, err := s.Dismiss(ctx, first); err != nil || created {
		t.Fatalf("lower: Dismiss = %v, %v; want false", created, err)
	}
	got, _ = s.Dismissals(ctx, mr1)
	if len(got) != 1 || got[0].Severity != "blocking" {
		t.Errorf("after lowering attempt = %+v", got)
	}

	// An unknown severity is refused.
	if _, err := s.Dismiss(ctx, Dismissal{MR: mr1, Fingerprint: "fp2", Severity: "info"}); err == nil {
		t.Error("Dismiss accepted an unknown severity")
	}
}

func TestDismissalsAreScopedToTheMR(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, d := range []Dismissal{
		{MR: mr1, Fingerprint: "b", Severity: "blocking", Reason: "r", CreatedAt: at.Add(time.Minute)},
		{MR: mr1, Fingerprint: "a", Severity: "suggestion", CreatedAt: at},
		{MR: mr2, Fingerprint: "a", Severity: "blocking", Reason: "r"},
	} {
		if created, err := s.Dismiss(ctx, d); err != nil || !created {
			t.Fatalf("Dismiss(%+v) = %v, %v", d, created, err)
		}
	}
	got, err := s.Dismissals(ctx, mr1)
	if err != nil || len(got) != 2 || got[0].Fingerprint != "a" || got[1].Fingerprint != "b" {
		t.Fatalf("Dismissals(mr1) = %+v, %v; want a, b oldest first", got, err)
	}
	if !got[0].CreatedAt.Equal(at) {
		t.Errorf("CreatedAt = %s, want the one given (%s)", got[0].CreatedAt, at)
	}
	if got, err := s.Dismissals(ctx, MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 99}); err != nil || len(got) != 0 {
		t.Errorf("Dismissals of another MR = %+v, %v", got, err)
	}
}

func TestRestore(t *testing.T) {
	s := newStore(t)
	if ok, err := s.Restore(ctx, mr1, "fp1"); err != nil || ok {
		t.Fatalf("Restore of nothing = %v, %v; want false", ok, err)
	}
	for _, mr := range []MRKey{mr1, mr2} {
		if _, err := s.Dismiss(ctx, Dismissal{MR: mr, Fingerprint: "fp1", Severity: "blocking", Reason: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.Restore(ctx, mr1, "fp1"); err != nil || !ok {
		t.Fatalf("Restore = %v, %v; want true", ok, err)
	}
	if got, _ := s.Dismissals(ctx, mr1); len(got) != 0 {
		t.Errorf("dismissal still stored: %+v", got)
	}
	if got, _ := s.Dismissals(ctx, mr2); len(got) != 1 {
		t.Errorf("the other MR's dismissal was removed: %+v", got)
	}
	if ok, _ := s.Restore(ctx, mr1, "fp1"); ok {
		t.Error("second Restore = true")
	}
	// A restored finding can be dismissed again, at any severity.
	if created, err := s.Dismiss(ctx, Dismissal{MR: mr1, Fingerprint: "fp1", Severity: "suggestion"}); err != nil || !created {
		t.Errorf("Dismiss after Restore = %v, %v", created, err)
	}
}
