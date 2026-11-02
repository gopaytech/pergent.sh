package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

const scanInst = "main"

// queueScan queues a scan with no gap and fails the test when it is not queued.
func queueScan(t *testing.T, s *Store, project string, at time.Time) ReadinessScan {
	t.Helper()
	sc, out, err := s.QueueReadinessScan(ctx, scanInst, project, StartedBySwitchOn, at, 0)
	if err != nil || out != ScanQueuedNow {
		t.Fatalf("QueueReadinessScan(%s) = %+v, %v, %v; want queued", project, sc, out, err)
	}
	return sc
}

// runningScan queues and claims a scan, and gives it a Job with the token.
func runningScan(t *testing.T, s *Store, project string, at time.Time, token string) ReadinessScan {
	t.Helper()
	queueScan(t, s, project, at)
	sc, ok, err := s.ClaimReadinessScan(ctx, 10, at, at.Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("ClaimReadinessScan = %v, %v", ok, err)
	}
	if err := s.StartReadinessJob(ctx, sc.ID, "pergent-scan-abc", hash(token), at.Add(time.Hour)); err != nil {
		t.Fatalf("StartReadinessJob: %v", err)
	}
	return sc
}

// Ten requests for one repository at once: exactly one queues, the others
// get the scan that did.
func TestQueueScanRace(t *testing.T) {
	s := newStore(t)
	const n = 10
	type res struct {
		sc  ReadinessScan
		out QueueOutcome
		err error
	}
	results := make([]res, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sc, out, err := s.QueueReadinessScan(ctx, scanInst, "acme/web", "alice", t0, 0)
			results[i] = res{sc, out, err}
		}()
	}
	close(start)
	wg.Wait()

	var queued, busy int
	var winner string
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("QueueReadinessScan: %v", r.err)
		}
		switch r.out {
		case ScanQueuedNow:
			queued++
			winner = r.sc.ID
		case ScanBusy:
			busy++
		default:
			t.Fatalf("outcome %d", r.out)
		}
	}
	if queued != 1 || busy != n-1 || winner == "" {
		t.Fatalf("queued %d, busy %d; want 1 and %d", queued, busy, n-1)
	}
	for _, r := range results {
		if r.out == ScanBusy && (r.sc.ID != winner || r.sc.State != ScanQueued) {
			t.Fatalf("busy scan = %+v; want the queued scan %s", r.sc, winner)
		}
	}
}

func TestQueueScanGap(t *testing.T) {
	s := newStore(t)
	first := queueScan(t, s, "acme/web", t0)
	if first.State != ScanQueued || first.StartedBy != StartedBySwitchOn || !first.QueuedAt.Equal(t0) ||
		first.Instance != scanInst || first.Project != "acme/web" {
		t.Fatalf("queued scan = %+v", first)
	}

	// While it is active, a request gets it back, gap or not.
	got, out, err := s.QueueReadinessScan(ctx, scanInst, "acme/web", "alice", t0.Add(time.Hour), 5*time.Minute)
	if err != nil || out != ScanBusy || got.ID != first.ID {
		t.Fatalf("while active = %+v, %v, %v; want busy with %s", got, out, err, first.ID)
	}

	claimed, ok, err := s.ClaimReadinessScan(ctx, 2, t0, t0.Add(time.Hour))
	if err != nil || !ok || claimed.ID != first.ID {
		t.Fatalf("claim = %+v, %v, %v", claimed, ok, err)
	}
	if done, err := s.FinishReadinessScan(ctx, first.ID, nil, nil, "", t0.Add(time.Minute)); err != nil || !done {
		t.Fatalf("finish = %v, %v", done, err)
	}

	// Four minutes after the latest was queued, with a 5 minute gap.
	got, out, err = s.QueueReadinessScan(ctx, scanInst, "acme/web", "alice", t0.Add(4*time.Minute), 5*time.Minute)
	if err != nil || out != ScanTooSoon || got.ID != first.ID {
		t.Fatalf("t0+4m = %+v, %v, %v; want too soon with %s", got, out, err, first.ID)
	}
	// Whatever started it counts, and the repository's case does not matter.
	got, out, err = s.QueueReadinessScan(ctx, scanInst, "ACME/Web", "alice", t0.Add(4*time.Minute), 5*time.Minute)
	if err != nil || out != ScanTooSoon || got.ID != first.ID {
		t.Fatalf("other case = %+v, %v, %v; want too soon", got, out, err)
	}
	// Another repository is not held back.
	if _, out, err = s.QueueReadinessScan(ctx, scanInst, "acme/api", "alice", t0.Add(4*time.Minute), 5*time.Minute); err != nil || out != ScanQueuedNow {
		t.Fatalf("other repo = %v, %v; want queued", out, err)
	}
	// Exactly the gap later it is allowed.
	got, out, err = s.QueueReadinessScan(ctx, scanInst, "acme/web", "alice", t0.Add(5*time.Minute), 5*time.Minute)
	if err != nil || out != ScanQueuedNow || got.ID == first.ID || got.StartedBy != "alice" {
		t.Fatalf("t0+5m = %+v, %v, %v; want queued", got, out, err)
	}
}

func TestQueueScanGapZeroNeverRefuses(t *testing.T) {
	s := newStore(t)
	first := queueScan(t, s, "acme/web", t0)
	if _, ok, err := s.ClaimReadinessScan(ctx, 2, t0, t0.Add(time.Hour)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if done, err := s.FinishReadinessScan(ctx, first.ID, nil, nil, "", t0); err != nil || !done {
		t.Fatal(done, err)
	}
	// The same instant, and even a clock that went back.
	for _, at := range []time.Time{t0, t0.Add(-time.Hour)} {
		sc, out, err := s.QueueReadinessScan(ctx, scanInst, "acme/web", "alice", at, 0)
		if err != nil || out != ScanQueuedNow {
			t.Fatalf("gap 0 at %v = %+v, %v, %v; want queued", at, sc, out, err)
		}
		if _, ok, err := s.ClaimReadinessScan(ctx, 2, at, at.Add(time.Hour)); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if done, err := s.FinishReadinessScan(ctx, sc.ID, nil, nil, "", at); err != nil || !done {
			t.Fatal(done, err)
		}
	}
}

func TestQueueScanCaseInsensitive(t *testing.T) {
	s := newStore(t)
	first := queueScan(t, s, "Acme/Web", t0)
	got, out, err := s.QueueReadinessScan(ctx, scanInst, "acme/web", "alice", t0, 0)
	if err != nil || out != ScanBusy || got.ID != first.ID || got.Project != "Acme/Web" {
		t.Fatalf("acme/web = %+v, %v, %v; want busy with %s", got, out, err, first.ID)
	}
	// A running scan blocks too, and another instance is a different repository.
	if _, ok, err := s.ClaimReadinessScan(ctx, 2, t0, t0.Add(time.Hour)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	got, out, err = s.QueueReadinessScan(ctx, scanInst, "ACME/WEB", "alice", t0, 0)
	if err != nil || out != ScanBusy || got.ID != first.ID || got.State != ScanRunning {
		t.Fatalf("running = %+v, %v, %v; want busy with the running scan", got, out, err)
	}
	if _, out, err = s.QueueReadinessScan(ctx, "other", "acme/web", "alice", t0, 0); err != nil || out != ScanQueuedNow {
		t.Fatalf("other instance = %v, %v; want queued", out, err)
	}
}

func TestClaimRespectsLimit(t *testing.T) {
	s := newStore(t)
	a := queueScan(t, s, "acme/a", t0)
	b := queueScan(t, s, "acme/b", t0.Add(time.Minute))
	c := queueScan(t, s, "acme/c", t0.Add(2*time.Minute))
	// Queued in another order than created: the order is queued_at.
	if _, ok, err := s.ClaimReadinessScan(ctx, 0, t0, t0.Add(time.Hour)); err != nil || ok {
		t.Fatalf("limit 0 claimed: %v, %v", ok, err)
	}

	now, deadline := t0.Add(10*time.Minute), t0.Add(70*time.Minute)
	for _, want := range []string{a.ID, b.ID} {
		got, ok, err := s.ClaimReadinessScan(ctx, 2, now, deadline)
		if err != nil || !ok || got.ID != want {
			t.Fatalf("claim = %+v, %v, %v; want %s", got, ok, err, want)
		}
		if got.State != ScanRunning || !got.StartedAt.Equal(now) || !got.Deadline.Equal(deadline) {
			t.Fatalf("claimed scan = %+v", got)
		}
	}
	if got, ok, err := s.ClaimReadinessScan(ctx, 2, now, deadline); err != nil || ok {
		t.Fatalf("third claim = %+v, %v, %v; want false", got, ok, err)
	}
	got, err := s.ReadinessScan(ctx, c.ID)
	if err != nil || got.State != ScanQueued {
		t.Fatalf("third = %+v, %v; want still queued", got, err)
	}

	// A finished scan frees a place.
	if done, err := s.FinishReadinessScan(ctx, a.ID, nil, nil, "", now); err != nil || !done {
		t.Fatal(done, err)
	}
	if got, ok, err := s.ClaimReadinessScan(ctx, 2, now, deadline); err != nil || !ok || got.ID != c.ID {
		t.Fatalf("after finish = %+v, %v, %v; want %s", got, ok, err, c.ID)
	}
}

// Claimers at once never run more than the limit.
func TestClaimConcurrentNeverExceedsLimit(t *testing.T) {
	s := newStore(t)
	for i := range 8 {
		queueScan(t, s, "acme/r"+string(rune('a'+i)), t0.Add(time.Duration(i)*time.Second))
	}
	const limit = 3
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, _, err := s.ClaimReadinessScan(ctx, limit, t0, t0.Add(time.Hour)); err != nil {
				t.Errorf("ClaimReadinessScan: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	running, err := s.RunningReadinessScans(ctx)
	if err != nil || len(running) != limit {
		t.Fatalf("running = %d, %v; want %d", len(running), err, limit)
	}
}

func TestFinishSpendsToken(t *testing.T) {
	s := newStore(t)
	sc := runningScan(t, s, "acme/web", t0, "right")

	if ok, err := s.ReadinessTokenIs(ctx, sc.ID, hash("right")); err != nil || !ok {
		t.Fatalf("ReadinessTokenIs(right) = %v, %v", ok, err)
	}
	if ok, err := s.ReadinessTokenIs(ctx, sc.ID, hash("wrong")); err != nil || ok {
		t.Fatalf("ReadinessTokenIs(wrong) = %v, %v", ok, err)
	}
	if ok, err := s.ReadinessTokenIs(ctx, sc.ID, nil); err != nil || ok {
		t.Fatalf("ReadinessTokenIs(nil) = %v, %v", ok, err)
	}

	job := json.RawMessage(`{"files":12}`)
	ok, err := s.FinishReadinessScan(ctx, sc.ID, hash("wrong"), job, "", t0.Add(time.Minute))
	if err != nil || ok {
		t.Fatalf("wrong hash = %v, %v; want false", ok, err)
	}
	got, err := s.ReadinessScan(ctx, sc.ID)
	if err != nil || got.State != ScanRunning || !got.FinishedAt.IsZero() || got.Job != nil {
		t.Fatalf("after wrong hash = %+v, %v; want unchanged", got, err)
	}

	ok, err = s.FinishReadinessScan(ctx, sc.ID, hash("right"), job, "", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("right hash = %v, %v; want true", ok, err)
	}
	got, err = s.ReadinessScan(ctx, sc.ID)
	if err != nil || got.State != ScanDone || !got.FinishedAt.Equal(t0.Add(time.Minute)) || got.Error != "" {
		t.Fatalf("done scan = %+v, %v", got, err)
	}
	var j map[string]int
	if err := json.Unmarshal(got.Job, &j); err != nil || j["files"] != 12 {
		t.Fatalf("job result = %s, %v", got.Job, err)
	}

	// The token is spent.
	if ok, err := s.ReadinessTokenIs(ctx, sc.ID, hash("right")); err != nil || ok {
		t.Fatalf("spent token still valid: %v, %v", ok, err)
	}
	if ok, err = s.FinishReadinessScan(ctx, sc.ID, hash("right"), job, "", t0.Add(2*time.Minute)); err != nil || ok {
		t.Fatalf("second finish = %v, %v; want false", ok, err)
	}
	if ok, err = s.FinishReadinessScan(ctx, sc.ID, nil, nil, "late", t0.Add(2*time.Minute)); err != nil || ok {
		t.Fatalf("finish of a finished scan = %v, %v; want false", ok, err)
	}
}

func TestFinishFailedAndPodSide(t *testing.T) {
	s := newStore(t)
	sc := runningScan(t, s, "acme/web", t0, "tok")
	// A pod-side end (no hash) fails a running scan whatever its token, and
	// spends the token so a late Job callback is refused.
	ok, err := s.FinishReadinessScan(ctx, sc.ID, nil, nil, "the Job could not start", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("pod-side end = %v, %v", ok, err)
	}
	got, err := s.ReadinessScan(ctx, sc.ID)
	if err != nil || got.State != ScanFailed || got.Error != "the Job could not start" || got.Job != nil {
		t.Fatalf("failed scan = %+v, %v", got, err)
	}
	if ok, err := s.ReadinessTokenIs(ctx, sc.ID, hash("tok")); err != nil || ok {
		t.Fatalf("token after a pod-side end = %v, %v; want spent", ok, err)
	}
	// A queued scan is not running: it cannot be finished.
	q := queueScan(t, s, "acme/api", t0)
	if ok, err := s.FinishReadinessScan(ctx, q.ID, nil, nil, "", t0); err != nil || ok {
		t.Fatalf("finish of a queued scan = %v, %v; want false", ok, err)
	}
}

func TestFinishPrunesOlder(t *testing.T) {
	s := newStore(t)
	// Two finished scans of the repository (spelled in 2 cases), one of
	// another repository, which must stay.
	for _, f := range []struct {
		project, state string
		at             time.Time
	}{
		{"acme/web", "done", t0},
		{"Acme/Web", "failed", t0.Add(time.Hour)},
		{"acme/api", "done", t0},
	} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO readiness_scans (id, instance, project, state, started_by, queued_at, finished_at)
			VALUES (gen_random_uuid(), 'main', $1, $2, 'alice', $3, $3)`, f.project, f.state, f.at); err != nil {
			t.Fatal(err)
		}
	}
	third := runningScan(t, s, "acme/web", t0.Add(2*time.Hour), "tok")
	if ok, err := s.FinishReadinessScan(ctx, third.ID, hash("tok"), nil, "", t0.Add(3*time.Hour)); err != nil || !ok {
		t.Fatalf("finish = %v, %v", ok, err)
	}
	var web, api int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE lower(project) = 'acme/web'),
		count(*) FILTER (WHERE project = 'acme/api') FROM readiness_scans`).Scan(&web, &api); err != nil {
		t.Fatal(err)
	}
	if web != 1 || api != 1 {
		t.Fatalf("acme/web rows = %d, acme/api rows = %d; want 1 and 1", web, api)
	}
	if _, err := s.ReadinessScan(ctx, third.ID); err != nil {
		t.Fatalf("the finished scan is gone: %v", err)
	}
}

// An active scan of the repository survives another's finish: only older
// finished scans are pruned.
func TestFinishKeepsActive(t *testing.T) {
	s := newStore(t)
	first := runningScan(t, s, "acme/web", t0, "tok")
	if ok, err := s.FinishReadinessScan(ctx, first.ID, hash("tok"), nil, "", t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	next := queueScan(t, s, "acme/web", t0.Add(time.Hour))
	latest, active, err := s.ReadinessScans(ctx, scanInst, "acme/web")
	if err != nil || latest == nil || latest.ID != first.ID || active == nil || active.ID != next.ID {
		t.Fatalf("latest, active = %+v, %+v, %v", latest, active, err)
	}
}

func TestSetReadinessPodAndJob(t *testing.T) {
	s := newStore(t)
	queueScan(t, s, "acme/web", t0)
	sc, ok, err := s.ClaimReadinessScan(ctx, 2, t0, t0.Add(time.Hour))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	pod := json.RawMessage(`{"protected":true}`)
	if err := s.SetReadinessPod(ctx, sc.ID, "main", "abc123", pod); err != nil {
		t.Fatalf("SetReadinessPod: %v", err)
	}
	deadline := t0.Add(2 * time.Hour)
	if err := s.StartReadinessJob(ctx, sc.ID, "pergent-scan-1234", hash("tok"), deadline); err != nil {
		t.Fatalf("StartReadinessJob: %v", err)
	}
	got, err := s.ReadinessScan(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "main" || got.Commit != "abc123" || got.JobName != "pergent-scan-1234" ||
		!got.Deadline.Equal(deadline) || got.State != ScanRunning {
		t.Fatalf("scan = %+v", got)
	}
	var p map[string]bool
	if err := json.Unmarshal(got.Pod, &p); err != nil || !p["protected"] {
		t.Fatalf("pod result = %s, %v", got.Pod, err)
	}
	// Neither touches a scan that is not running.
	if _, err := s.FinishReadinessScan(ctx, sc.ID, nil, nil, "", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReadinessPod(ctx, sc.ID, "x", "y", pod); !errors.Is(err, ErrReadinessScanNotFound) {
		t.Fatalf("SetReadinessPod on a finished scan = %v; want ErrReadinessScanNotFound", err)
	}
	if err := s.StartReadinessJob(ctx, sc.ID, "x", hash("t"), deadline); !errors.Is(err, ErrReadinessScanNotFound) {
		t.Fatalf("StartReadinessJob on a finished scan = %v; want ErrReadinessScanNotFound", err)
	}
	got, _ = s.ReadinessScan(ctx, sc.ID)
	if got.Branch != "main" || got.JobName != "pergent-scan-1234" {
		t.Fatalf("a finished scan was changed: %+v", got)
	}
}

func TestReadinessScanNotFound(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"not-a-uuid", "6f1c2a3e-0000-4000-8000-000000000000"} {
		if _, err := s.ReadinessScan(ctx, id); !errors.Is(err, ErrReadinessScanNotFound) {
			t.Errorf("ReadinessScan(%q) = %v; want ErrReadinessScanNotFound", id, err)
		}
		if ok, err := s.ReadinessTokenIs(ctx, id, hash("t")); err != nil || ok {
			t.Errorf("ReadinessTokenIs(%q) = %v, %v; want false", id, ok, err)
		}
		if ok, err := s.FinishReadinessScan(ctx, id, nil, nil, "", t0); err != nil || ok {
			t.Errorf("FinishReadinessScan(%q) = %v, %v; want false", id, ok, err)
		}
	}
}

func TestRunningReadinessScans(t *testing.T) {
	s := newStore(t)
	queueScan(t, s, "acme/a", t0)
	queueScan(t, s, "acme/b", t0.Add(time.Minute))
	if got, err := s.RunningReadinessScans(ctx); err != nil || len(got) != 0 {
		t.Fatalf("none running = %v, %v", got, err)
	}
	a, _, _ := s.ClaimReadinessScan(ctx, 5, t0, t0.Add(time.Hour))
	b, _, _ := s.ClaimReadinessScan(ctx, 5, t0, t0.Add(time.Hour))
	got, err := s.RunningReadinessScans(ctx)
	if err != nil || len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		t.Fatalf("running = %+v, %v; want %s then %s", got, err, a.ID, b.ID)
	}
}

func TestReadinessScansLatestAndActive(t *testing.T) {
	s := newStore(t)
	latest, active, err := s.ReadinessScans(ctx, scanInst, "acme/web")
	if err != nil || latest != nil || active != nil {
		t.Fatalf("none = %+v, %+v, %v", latest, active, err)
	}

	first := queueScan(t, s, "Acme/Web", t0)
	latest, active, err = s.ReadinessScans(ctx, scanInst, "acme/web")
	if err != nil || latest != nil || active == nil || active.ID != first.ID {
		t.Fatalf("queued only = %+v, %+v, %v", latest, active, err)
	}

	if _, ok, err := s.ClaimReadinessScan(ctx, 2, t0, t0.Add(time.Hour)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	latest, active, _ = s.ReadinessScans(ctx, scanInst, "ACME/WEB")
	if latest != nil || active == nil || active.State != ScanRunning {
		t.Fatalf("running = %+v, %+v", latest, active)
	}

	if ok, err := s.FinishReadinessScan(ctx, first.ID, nil, nil, "boom", t0.Add(time.Minute)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	latest, active, _ = s.ReadinessScans(ctx, scanInst, "acme/web")
	if latest == nil || latest.ID != first.ID || latest.State != ScanFailed || latest.Error != "boom" || active != nil {
		t.Fatalf("failed = %+v, %+v", latest, active)
	}

	second := queueScan(t, s, "acme/web", t0.Add(time.Hour))
	latest, active, _ = s.ReadinessScans(ctx, scanInst, "acme/web")
	if latest == nil || latest.ID != first.ID || active == nil || active.ID != second.ID {
		t.Fatalf("both = %+v, %+v", latest, active)
	}
	// Another repository and another instance see nothing.
	if l, a, err := s.ReadinessScans(ctx, scanInst, "acme/api"); err != nil || l != nil || a != nil {
		t.Fatalf("other repo = %+v, %+v, %v", l, a, err)
	}
	if l, a, err := s.ReadinessScans(ctx, "other", "acme/web"); err != nil || l != nil || a != nil {
		t.Fatalf("other instance = %+v, %+v, %v", l, a, err)
	}
}

func TestDeleteReadinessScans(t *testing.T) {
	s := newStore(t)
	web := runningScan(t, s, "Acme/Web", t0, "tok")
	if ok, err := s.FinishReadinessScan(ctx, web.ID, hash("tok"), nil, "", t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	queued := queueScan(t, s, "acme/web", t0.Add(time.Hour))
	api := queueScan(t, s, "acme/api", t0)
	if _, _, err := s.QueueReadinessScan(ctx, "other", "acme/web", "x", t0, 0); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteReadinessScans(ctx, scanInst, "ACME/web"); err != nil {
		t.Fatalf("DeleteReadinessScans: %v", err)
	}
	for _, id := range []string{web.ID, queued.ID} {
		if _, err := s.ReadinessScan(ctx, id); !errors.Is(err, ErrReadinessScanNotFound) {
			t.Errorf("scan %s after delete = %v; want not found", id, err)
		}
	}
	if _, err := s.ReadinessScan(ctx, api.ID); err != nil {
		t.Errorf("another repository's scan was deleted: %v", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM readiness_scans WHERE instance = 'other'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("another instance's scans = %d, %v; want 1", n, err)
	}
	if err := s.DeleteReadinessScans(ctx, scanInst, "acme/none"); err != nil {
		t.Errorf("deleting none: %v", err)
	}
}

// A repository is switched on when it has a project row. The order is the
// config-file repositories first (by instance, then project id), then the
// dashboard repositories by when they were switched on.
func TestUnscannedProjects(t *testing.T) {
	s := newStore(t)
	addProject := func(inst string, id int64, path string) {
		t.Helper()
		if err := s.UpsertProject(ctx, Project{Instance: inst, ProjectID: id, Path: path}); err != nil {
			t.Fatal(err)
		}
	}
	dash := func(inst, path string, created time.Time) {
		t.Helper()
		if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: inst, Project: path}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE dashboard_repos SET created_at = $3
			WHERE instance = $1 AND lower(project) = lower($2)`, inst, path, created); err != nil {
			t.Fatal(err)
		}
	}
	// Config-file repositories: project rows with no dashboard row. The
	// second instance sorts after the first, though its project id is lower.
	addProject("b", 1, "acme/cfg-b")
	addProject("a", 5, "acme/cfg-a")
	// Dashboard repositories, project ids in the opposite order to when they
	// were switched on.
	addProject("main", 1, "acme/late")
	dash("main", "acme/late", t0.Add(2*time.Hour))
	addProject("main", 2, "acme/early")
	dash("main", "Acme/Early", t0.Add(time.Hour)) // spelled differently: the join ignores case
	// One already scanned (any state), one switched off (no project row).
	addProject("main", 3, "acme/scanned")
	dash("main", "acme/scanned", t0)
	sc := queueScan(t, s, "Acme/Scanned", t0) // spelled differently again
	if _, ok, err := s.ClaimReadinessScan(ctx, 1, t0, t0.Add(time.Hour)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.FinishReadinessScan(ctx, sc.ID, nil, nil, "x", t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	dash("main", "acme/off", t0)

	got, err := s.UnscannedProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ProjectRef{
		{Instance: "a", Project: "acme/cfg-a"},
		{Instance: "b", Project: "acme/cfg-b"},
		{Instance: "main", Project: "acme/early"},
		{Instance: "main", Project: "acme/late"},
	}
	if len(got) != len(want) {
		t.Fatalf("unscanned = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unscanned = %v; want %v", got, want)
		}
	}

	// A queued scan counts as scanned: the sweep must not queue twice.
	queueScan(t, s, "acme/early", t0)
	got, _ = s.UnscannedProjects(ctx)
	if len(got) != 3 || got[2].Project != "acme/late" {
		t.Fatalf("after queueing early = %v", got)
	}
	// A repository's scan row on another instance does not hide it.
	if _, _, err := s.QueueReadinessScan(ctx, "b", "acme/cfg-a", "x", t0, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UnscannedProjects(ctx)
	if len(got) != 3 || got[0].Project != "acme/cfg-a" {
		t.Fatalf("after another instance's scan = %v", got)
	}
}

func TestReadinessScanBytesRoundTrip(t *testing.T) {
	s := newStore(t)
	sc := runningScan(t, s, "acme/web", t0, "tok")
	got, err := s.ReadinessScan(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pod != nil || got.Job != nil || got.FinishedAt != (time.Time{}) {
		t.Fatalf("fresh scan has results: %+v", got)
	}
	var stored []byte
	if err := s.pool.QueryRow(ctx, `SELECT token_hash FROM readiness_scans WHERE id = $1::uuid`, sc.ID).Scan(&stored); err != nil || !bytes.Equal(stored, hash("tok")) {
		t.Fatalf("stored token hash = %x, %v", stored, err)
	}
}
