package store

import (
	"testing"
	"time"
)

// A queued push run and an automatic re-run (the publisher's, which checked
// nothing) are both held for the quota, and the dispatcher never claims them.
func TestHoldForQuotaHoldsPushAndAutoRerun(t *testing.T) {
	s := newStore(t)
	push := enqueue(t, s, mr1, sha('a'))
	rerun, created, err := s.EnqueueRerun(ctx, mr2, "acme/widgets", sha('b'), "main", TriggerRerun, nil, nil, true, false)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}
	for _, id := range []string{push, rerun} {
		ok, err := s.HoldForQuota(ctx, id)
		if err != nil || !ok {
			t.Fatalf("HoldForQuota(%s) = %v, %v; want true", id, ok, err)
		}
		if r := mustRun(t, s, id); r.State != Waiting || r.SkipReason != WaitQuota {
			t.Errorf("run %s = %s/%q, want waiting/%q", id, r.State, r.SkipReason, WaitQuota)
		}
	}
	claimed, err := s.ClaimQueued(ctx, 10, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Errorf("claimed %d runs, want none", len(claimed))
	}
}

// A run someone asked for (quota checked) or one already dispatched is not held.
func TestHoldForQuotaLeavesCheckedRuns(t *testing.T) {
	s := newStore(t)
	dispatchedID := dispatched(t, s, mr2, sha('b'), "tok-b")
	if ok, err := s.HoldForQuota(ctx, dispatchedID); err != nil || ok {
		t.Errorf("HoldForQuota of a dispatched run = %v, %v; want false", ok, err)
	}
	if r := mustRun(t, s, dispatchedID); r.State != Running {
		t.Errorf("dispatched run = %s, want running", r.State)
	}

	checked, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", sha('a'), "main", TriggerRerun, nil, nil, true, true)
	if err != nil || !created {
		t.Fatalf("EnqueueRerun = %v, %v", created, err)
	}
	if ok, err := s.HoldForQuota(ctx, checked); err != nil || ok {
		t.Errorf("HoldForQuota of a checked run = %v, %v; want false", ok, err)
	}
	if r := mustRun(t, s, checked); r.State != Queued || !r.QuotaChecked {
		t.Errorf("checked run = %s, checked %v; want queued, true", r.State, r.QuotaChecked)
	}
}

// A start that came before the hold makes the run a start, and the hold that
// follows leaves it alone.
func TestHoldForQuotaLeavesAStartAlone(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, sha('a'))
	if got, started, err := s.StartRun(ctx, mr1, "acme/widgets", sha('a'), "main"); err != nil || !started || got != id {
		t.Fatalf("StartRun = %s, %v, %v; want %s, true", got, started, err, id)
	}
	if ok, err := s.HoldForQuota(ctx, id); err != nil || ok {
		t.Errorf("HoldForQuota of a start = %v, %v; want false", ok, err)
	}
	if r := mustRun(t, s, id); r.State != Queued || r.Trigger != TriggerStart || !r.QuotaChecked {
		t.Errorf("run = %s/%s checked %v, want queued/start/true", r.State, r.Trigger, r.QuotaChecked)
	}
}

// Starting a run the quota held queues it as a start, past the quota.
func TestStartRunMarksQuotaChecked(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, sha('a'))
	if ok, err := s.HoldForQuota(ctx, id); err != nil || !ok {
		t.Fatalf("HoldForQuota = %v, %v", ok, err)
	}
	if got, started, err := s.StartRun(ctx, mr1, "acme/widgets", sha('a'), "main"); err != nil || !started || got != id {
		t.Fatalf("StartRun = %s, %v, %v; want %s, true", got, started, err, id)
	}
	r := mustRun(t, s, id)
	if r.State != Queued || r.Trigger != TriggerStart || !r.QuotaChecked || r.SkipReason != "" {
		t.Errorf("run = %s/%s checked %v reason %q; want queued/start/true/empty", r.State, r.Trigger, r.QuotaChecked, r.SkipReason)
	}

	// A start with nothing to turn into inserts a checked run.
	other := MRKey{Instance: mr1.Instance, ProjectID: mr1.ProjectID, IID: 99}
	fresh, created, err := s.StartRun(ctx, other, "acme/widgets", sha('b'), "main")
	if err != nil || !created {
		t.Fatalf("StartRun of a new head = %v, %v", created, err)
	}
	if r := mustRun(t, s, fresh); !r.QuotaChecked {
		t.Error("a new start run was not marked quota checked")
	}
}

// The quota note is claimed once a calendar month, and a failed post gives
// the claim back.
func TestClaimQuotaNoteOncePerMonth(t *testing.T) {
	s := newStore(t)
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 12, 0, 0, 0, time.UTC) }
	claim := func(want bool, now, monthStart time.Time) {
		t.Helper()
		if got, err := s.ClaimQuotaNote(ctx, mr1, monthStart, now); err != nil || got != want {
			t.Fatalf("ClaimQuotaNote(%s) = %v, %v; want %v", now.Format("2006-01-02"), got, err, want)
		}
	}
	nov, dec := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	claim(true, day(11, 3), nov)
	claim(false, day(11, 20), nov)
	claim(true, day(12, 2), dec)
	claim(false, day(12, 3), dec)
	if err := s.ReleaseQuotaNote(ctx, mr1); err != nil {
		t.Fatal(err)
	}
	claim(true, day(12, 4), dec)
	// The untrusted note is its own claim.
	if got, err := s.ClaimUntrustedNote(ctx, mr1, t0); err != nil || !got {
		t.Errorf("ClaimUntrustedNote = %v, %v; want true", got, err)
	}
}

func TestTokensSince(t *testing.T) {
	s := newStore(t)
	at := func(m time.Month, d, h, mi, sec int) time.Time { return time.Date(2026, m, d, h, mi, sec, 0, time.UTC) }
	main1 := MRKey{Instance: "main", ProjectID: 1, IID: 1}
	main1b := MRKey{Instance: "main", ProjectID: 1, IID: 2}
	main2 := MRKey{Instance: "main", ProjectID: 2, IID: 1}
	other := MRKey{Instance: "other", ProjectID: 1, IID: 1}
	seedUsage(t, s, main1, at(11, 1, 0, 0, 0), TriggerPush, false, "m", skillRow("a", "v", 100, 50))
	seedUsage(t, s, main1b, at(10, 31, 23, 59, 59), TriggerPush, false, "m", skillRow("a", "v", 7, 3))
	seedUsage(t, s, main2, at(11, 5, 0, 0, 0), TriggerPush, false, "m", skillRow("a", "v", 1000, 0))
	seedUsage(t, s, other, at(11, 2, 0, 0, 0), TriggerPush, false, "m", skillRow("a", "v", 5000, 5000))

	nov1 := at(11, 1, 0, 0, 0)
	for _, tc := range []struct {
		instance string
		project  int64
		want     int64
	}{
		{"main", 1, 150},
		{"main", 0, 1150},
		{"other", 0, 10000},
		{"nowhere", 0, 0},
	} {
		if got, err := s.TokensSince(ctx, tc.instance, tc.project, nov1); err != nil || got != tc.want {
			t.Errorf("TokensSince(%q, %d) = %d, %v; want %d", tc.instance, tc.project, got, err, tc.want)
		}
	}
}

// The previous release (0034) keeps working on the migrated database: its
// inserts name none of the new columns, and a quota of zero is refused.
func TestMigrationKeepsPreviousReleaseQuota(t *testing.T) {
	s := newStoreAt(t, 34)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) error {
		_, err := s.pool.Exec(ctx, q, args...)
		return err
	}
	for _, q := range []string{
		`INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, target_branch)
			VALUES ('lab', 7, 3, 'acme/web', 'aaa', 'main')`,
		`INSERT INTO dashboard_repos (instance, project, created_by) VALUES ('lab', 'acme/web', 'alice')`,
		`INSERT INTO instances (name, platform, url, api_url, created_at)
			VALUES ('lab', 'gitlab', 'https://gitlab.example.com', 'https://gitlab.example.com/api/v4', now())`,
		`INSERT INTO merge_requests (instance, project_id, mr_iid) VALUES ('lab', 7, 3)`,
	} {
		if err := exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var checked bool
	if err := s.pool.QueryRow(ctx, `SELECT quota_checked FROM runs`).Scan(&checked); err != nil || checked {
		t.Errorf("quota_checked = %v, %v; want false", checked, err)
	}
	for _, q := range []string{
		`UPDATE dashboard_repos SET monthly_token_quota = 0`,
		`UPDATE instances SET monthly_token_quota = 0`,
	} {
		if err := exec(q); err == nil {
			t.Errorf("%s was accepted, want the CHECK to refuse it", q)
		}
	}
	for _, q := range []string{
		`UPDATE dashboard_repos SET monthly_token_quota = 1000`,
		`UPDATE instances SET monthly_token_quota = 1000`,
	} {
		if err := exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}
