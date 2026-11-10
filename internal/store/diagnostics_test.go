package store

import (
	"testing"
	"time"
)

func TestReportFailureStoresStepAndDetail(t *testing.T) {
	s := newStore(t)
	id := dispatched(t, s, mr1, "aaaa", "tok")
	reason := "the clone failed (checkout exited with code 128)"
	if ok, err := s.ReportFailure(ctx, id, Failure{Reason: reason, Step: "clone"}, t0); err != nil || !ok {
		t.Fatalf("ReportFailure = %v, %v", ok, err)
	}
	r := getRun(t, s, id)
	if r.FailureStep != "clone" || r.FailureReason != reason || r.FailureDetail != "" {
		t.Errorf("run = step %q reason %q detail %q", r.FailureStep, r.FailureReason, r.FailureDetail)
	}

	// An empty reason still becomes the default; the detail is stored.
	other := dispatched(t, s, MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 4}, "bbbb", "tok2")
	if ok, err := s.ReportFailure(ctx, other, Failure{Step: "review", Detail: "pod pending"}, t0); err != nil || !ok {
		t.Fatalf("ReportFailure = %v, %v", ok, err)
	}
	r = getRun(t, s, other)
	if r.FailureReason != "review failed" || r.FailureStep != "review" || r.FailureDetail != "pod pending" {
		t.Errorf("run = step %q reason %q detail %q", r.FailureStep, r.FailureReason, r.FailureDetail)
	}
}

func TestSetRunPod(t *testing.T) {
	s := newStore(t)
	id := dispatched(t, s, mr1, "aaaa", "tok")
	if err := s.SetRunPod(ctx, id, "pergent-review-abc-xyz"); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, id); r.PodName != "pergent-review-abc-xyz" {
		t.Errorf("PodName = %q", r.PodName)
	}
}

func TestAdvanceCheckoutSeenOnlyRaises(t *testing.T) {
	s := newStore(t)
	id := dispatched(t, s, mr1, "aaaa", "tok")
	if r := getRun(t, s, id); r.CheckoutSeen != 0 {
		t.Fatalf("CheckoutSeen = %d, want 0", r.CheckoutSeen)
	}
	for _, c := range []struct{ set, want int }{{2, 2}, {1, 2}, {2, 2}} {
		if err := s.AdvanceCheckoutSeen(ctx, id, c.set); err != nil {
			t.Fatal(err)
		}
		if r := getRun(t, s, id); r.CheckoutSeen != c.want {
			t.Errorf("after %d: CheckoutSeen = %d, want %d", c.set, r.CheckoutSeen, c.want)
		}
	}
}

func TestRunDiagnosticsRoundTrip(t *testing.T) {
	s := newStore(t)
	id := dispatched(t, s, mr1, "aaaa", "tok")
	if _, found, err := s.RunDiagnostics(ctx, id); err != nil || found {
		t.Fatalf("RunDiagnostics before a put = %v, %v", found, err)
	}
	if _, found, err := s.RunDiagnostics(ctx, "not-a-uuid"); err != nil || found {
		t.Fatalf("RunDiagnostics of a malformed id = %v, %v", found, err)
	}
	want := Diagnostics{CloneLog: "cloning https://x", CloneLogMasked: "cloning <masked>", PodState: "Pending"}
	if err := s.PutRunDiagnostics(ctx, id, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.RunDiagnostics(ctx, id)
	if err != nil || !found || got != want {
		t.Fatalf("RunDiagnostics = %+v, %v, %v; want %+v", got, found, err, want)
	}

	// A second put replaces, and text is made fit for a text column.
	second := Diagnostics{CloneLog: "a\x00b \xff", CloneLogMasked: "", PodState: "Running"}
	if err := s.PutRunDiagnostics(ctx, id, second); err != nil {
		t.Fatal(err)
	}
	got, found, err = s.RunDiagnostics(ctx, id)
	if err != nil || !found {
		t.Fatalf("RunDiagnostics = %v, %v", found, err)
	}
	if got.CloneLog != "ab �" || got.CloneLogMasked != "" || got.PodState != "Running" {
		t.Errorf("after the second put = %+v", got)
	}
	if err := s.PutRunDiagnostics(ctx, "not-a-uuid", want); err == nil {
		t.Errorf("PutRunDiagnostics of a malformed id succeeded")
	}
}

func TestPruneRunLogsPrunesDiagnostics(t *testing.T) {
	s := newStore(t)
	oldID := enqueue(t, s, mr1, "aaaa")
	newID := enqueue(t, s, MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 4}, "bbbb")
	for _, id := range []string{oldID, newID} {
		if err := s.PutRunDiagnostics(ctx, id, Diagnostics{CloneLog: "log " + id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE run_diagnostics SET created_at = now() - interval '31 days' WHERE run_id = $1::uuid`, oldID); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneRunLogs(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("PruneRunLogs = %d, %v; want 1", n, err)
	}
	if _, found, _ := s.RunDiagnostics(ctx, oldID); found {
		t.Errorf("the 31-day-old diagnostics are still there")
	}
	if _, found, _ := s.RunDiagnostics(ctx, newID); !found {
		t.Errorf("the fresh diagnostics were pruned")
	}

	// A log and diagnostics pruned together are both counted.
	if err := s.PutRunLog(ctx, oldID, "log"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunDiagnostics(ctx, oldID, Diagnostics{}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"run_logs", "run_diagnostics"} {
		if _, err := s.pool.Exec(ctx, `UPDATE `+table+` SET created_at = now() - interval '31 days' WHERE run_id = $1::uuid`, oldID); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.PruneRunLogs(ctx, time.Now().Add(-30*24*time.Hour)); err != nil || n != 2 {
		t.Errorf("PruneRunLogs = %d, %v; want 2", n, err)
	}
}

func TestCountFailedRunsAtStep(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	fail := func(iid int64, project, step string, at time.Time) {
		t.Helper()
		id := dispatched(t, s, MRKey{Instance: "main", ProjectID: 7, IID: iid}, "aaa", "tok")
		if _, err := s.pool.Exec(ctx, `UPDATE runs SET project_path = $2 WHERE id = $1::uuid`, id, project); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.ReportFailure(ctx, id, Failure{Reason: "x", Step: step}, at); err != nil || !ok {
			t.Fatalf("ReportFailure = %v, %v", ok, err)
		}
	}
	fail(1, "acme/widgets", "clone", now.Add(-time.Hour))
	fail(2, "Acme/Widgets", "clone", now.Add(-24*time.Hour))
	fail(3, "acme/widgets", "clone", now.Add(-8*24*time.Hour)) // too old
	fail(4, "acme/widgets", "review", now.Add(-time.Hour))
	fail(5, "acme/payments", "clone", now.Add(-time.Hour))

	n, err := s.CountFailedRunsAtStep(ctx, "main", "acme/widgets", "clone", now.Add(-7*24*time.Hour))
	if err != nil || n != 2 {
		t.Errorf("CountFailedRunsAtStep = %d, %v; want 2", n, err)
	}
	if n, err := s.CountFailedRunsAtStep(ctx, "other", "acme/widgets", "clone", now.Add(-7*24*time.Hour)); err != nil || n != 0 {
		t.Errorf("another instance = %d, %v; want 0", n, err)
	}
}
