package store

import (
	"slices"
	"testing"
	"time"
)

var mr3 = MRKey{Instance: "https://gitlab.example.com", ProjectID: 7, IID: 5}

func fate(fp, skill, hash, severity, what string) FindingFate {
	return FindingFate{Fingerprint: fp, Skill: skill, SkillHash: hash, Severity: severity, Fate: what}
}

// storeFates gives mr a job requested at at, as if its webhook came then, and
// stores rows for it.
func storeFates(t *testing.T, s *Store, mr MRKey, path string, rows []FindingFate, merged, byBot bool, at time.Time) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `INSERT INTO fate_jobs (instance, project_id, mr_iid, project_path, merged, requested_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET
			requested_at = EXCLUDED.requested_at, attempts = 0, not_before = NULL, finished_at = NULL`,
		mr.Instance, mr.ProjectID, mr.IID, path, merged, at); err != nil {
		t.Fatal(err)
	}
	stored, err := s.ReplaceFates(ctx, mr, at, path, rows, merged, byBot, at)
	if err != nil || !stored {
		t.Fatalf("ReplaceFates(%v) = %v, %v; want it stored", mr, stored, err)
	}
}

// fatesOf returns the fingerprints stored as the fates of mr, in order.
func fatesOf(t *testing.T, s *Store, mr MRKey) []string {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT fingerprint FROM finding_fates
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 ORDER BY fingerprint`, mr.Instance, mr.ProjectID, mr.IID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			t.Fatal(err)
		}
		out = append(out, fp)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// jobOf returns the fate job of mr as stored, and whether it is finished.
func jobOf(t *testing.T, s *Store, mr MRKey) (job FateJob, finished, found bool) {
	t.Helper()
	var finishedAt *time.Time
	err := s.pool.QueryRow(ctx, `SELECT project_path, merged, requested_at, attempts, finished_at FROM fate_jobs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3`, mr.Instance, mr.ProjectID, mr.IID).
		Scan(&job.ProjectPath, &job.Merged, &job.RequestedAt, &job.Attempts, &finishedAt)
	if err != nil {
		return FateJob{}, false, false
	}
	job.MR = mr
	return job, finishedAt != nil, true
}

func TestReplaceFatesReplaces(t *testing.T) {
	s := newStore(t)
	first := []FindingFate{fate("a", "sec", "h1", "blocking", "fixed"), fate("b", "sec", "h1", "suggestion", "open")}
	storeFates(t, s, mr1, "acme/widgets", first, false, false, t0)
	storeFates(t, s, mr2, "acme/widgets", first, false, false, t0)
	if got := fatesOf(t, s, mr1); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("fates = %v; want a, b", got)
	}
	job, finished, found := jobOf(t, s, mr1)
	if !found || !finished || job.Merged || job.ProjectPath != "acme/widgets" {
		t.Errorf("job after a close = %+v finished=%v found=%v; want finished, closed", job, finished, found)
	}

	// The change is merged later: its rows are replaced, the other change's are not.
	storeFates(t, s, mr1, "acme/widgets", []FindingFate{fate("c", "sec", "h2", "blocking", "fixed")}, true, true, t0.Add(time.Hour))
	if got := fatesOf(t, s, mr1); !slices.Equal(got, []string{"c"}) {
		t.Errorf("fates after the merge = %v; want c", got)
	}
	if got := fatesOf(t, s, mr2); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("another change's fates = %v; want a, b", got)
	}
	var merged, byBot bool
	var decided time.Time
	if err := s.pool.QueryRow(ctx, `SELECT merged, by_bot, decided_at FROM finding_fates
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3`, mr1.Instance, mr1.ProjectID, mr1.IID).Scan(&merged, &byBot, &decided); err != nil {
		t.Fatal(err)
	}
	if !merged || !byBot || !decided.Equal(t0.Add(time.Hour)) {
		t.Errorf("row = merged %v, by_bot %v, decided %s; want true, true, %s", merged, byBot, decided, t0.Add(time.Hour))
	}
	if job, finished, _ := jobOf(t, s, mr1); !job.Merged || !finished {
		t.Errorf("job after the merge = %+v finished=%v; want merged, finished", job, finished)
	}

	// No fates is a change with nothing to count: its rows are gone, its job finished.
	storeFates(t, s, mr1, "acme/widgets", nil, true, false, t0.Add(2*time.Hour))
	if got := fatesOf(t, s, mr1); len(got) != 0 {
		t.Errorf("fates = %v; want none", got)
	}

	// A bad row stores nothing, and the rows already there stay.
	if err := s.RequestFates(ctx, mr2, "acme/widgets", false, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	bad := []FindingFate{fate("x", "sec", "", "blocking", "fixed"), fate("y", "sec", "", "blocking", "bogus")}
	if stored, err := s.ReplaceFates(ctx, mr2, t0.Add(time.Hour), "acme/widgets", bad, true, false, t0); err == nil || stored {
		t.Errorf("ReplaceFates with an unknown fate = %v, %v; want an error", stored, err)
	}
	if got := fatesOf(t, s, mr2); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("fates after a failed replace = %v; want a, b", got)
	}
	if _, finished, _ := jobOf(t, s, mr2); finished {
		t.Error("a failed replace finished the job")
	}
}

// One fingerprint can be a finding of two skills, and both are stored.
func TestReplaceFatesKeepsOneFingerprintOfTwoSkills(t *testing.T) {
	s := newStore(t)
	rows := []FindingFate{fate("x", "sec", "", "blocking", "fixed"), fate("x", "style", "", "suggestion", "open")}
	storeFates(t, s, mr1, "acme/widgets", rows, true, false, t0)
	if got := fatesOf(t, s, mr1); !slices.Equal(got, []string{"x", "x"}) {
		t.Errorf("fates = %v; want x for both skills", got)
	}
}

// A pass works on the job it claimed: a webhook that asked again meanwhile,
// a job already finished or one dropped is left to its own turn.
func TestReplaceFatesOnlyActsOnTheClaimedJob(t *testing.T) {
	s := newStore(t)
	if err := s.RequestFates(ctx, mr1, "acme/widgets", false, t0); err != nil {
		t.Fatal(err)
	}
	old := []FindingFate{fate("old", "sec", "", "blocking", "fixed")}
	storeFates(t, s, mr2, "acme/widgets", old, false, false, t0)
	// The webhook asks again (a merge) while a pass for the first request runs.
	if err := s.RequestFates(ctx, mr1, "acme/widgets", true, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rows := []FindingFate{fate("a", "sec", "", "blocking", "fixed")}
	if stored, err := s.ReplaceFates(ctx, mr1, t0, "acme/widgets", rows, false, false, t0.Add(2*time.Minute)); err != nil || stored {
		t.Fatalf("ReplaceFates for the old request = %v, %v; want nothing stored", stored, err)
	}
	if got := fatesOf(t, s, mr1); len(got) != 0 {
		t.Errorf("fates = %v; want none", got)
	}
	if job, finished, _ := jobOf(t, s, mr1); finished || !job.Merged {
		t.Errorf("job = %+v finished=%v; want the newer request, still to do", job, finished)
	}
	// The pass for the new request stores, and a second try for it finds it finished.
	if stored, err := s.ReplaceFates(ctx, mr1, t0.Add(time.Minute), "acme/widgets", rows, true, false, t0.Add(2*time.Minute)); err != nil || !stored {
		t.Fatalf("ReplaceFates for the new request = %v, %v; want it stored", stored, err)
	}
	if stored, err := s.ReplaceFates(ctx, mr1, t0.Add(time.Minute), "acme/widgets", nil, true, false, t0.Add(3*time.Minute)); err != nil || stored {
		t.Errorf("ReplaceFates for a finished job = %v, %v; want nothing stored", stored, err)
	}
	if got := fatesOf(t, s, mr1); !slices.Equal(got, []string{"a"}) {
		t.Errorf("fates = %v; want a kept", got)
	}
	// A change with no job at all stores nothing.
	if stored, err := s.ReplaceFates(ctx, mr3, t0, "acme/widgets", rows, true, false, t0); err != nil || stored {
		t.Errorf("ReplaceFates with no job = %v, %v; want nothing stored", stored, err)
	}
	if got := fatesOf(t, s, mr3); len(got) != 0 {
		t.Errorf("fates with no job = %v; want none", got)
	}
}

// The job's path and merged are the ones of the pass that finished it.
func TestReplaceFatesUpdatesTheJob(t *testing.T) {
	s := newStore(t)
	if err := s.RequestFates(ctx, mr1, "acme/old-name", false, t0); err != nil {
		t.Fatal(err)
	}
	stored, err := s.ReplaceFates(ctx, mr1, t0, "acme/new-name", nil, true, false, t0.Add(time.Minute))
	if err != nil || !stored {
		t.Fatalf("ReplaceFates = %v, %v", stored, err)
	}
	job, finished, _ := jobOf(t, s, mr1)
	if !finished || job.ProjectPath != "acme/new-name" || !job.Merged || !job.RequestedAt.Equal(t0) {
		t.Errorf("job = %+v finished=%v; want finished, acme/new-name, merged, still requested at t0", job, finished)
	}
}

func TestRequestFatesAfterAMergeDoesNothing(t *testing.T) {
	s := newStore(t)
	if err := s.RequestFates(ctx, mr1, "acme/widgets", true, t0); err != nil {
		t.Fatal(err)
	}
	storeFates(t, s, mr1, "acme/widgets", []FindingFate{fate("a", "sec", "", "blocking", "fixed")}, true, false, t0)
	// A redelivered merge, or a close that arrives after it.
	for _, merged := range []bool{true, false} {
		if err := s.RequestFates(ctx, mr1, "acme/other", merged, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		job, finished, _ := jobOf(t, s, mr1)
		if !finished || !job.Merged || job.ProjectPath != "acme/widgets" || !job.RequestedAt.Equal(t0) {
			t.Errorf("job after RequestFates(merged=%v) = %+v finished=%v; want it left as it was", merged, job, finished)
		}
	}
	if got, err := s.ClaimFateJobs(ctx, t0.Add(2*time.Hour), 10); err != nil || len(got) != 0 {
		t.Errorf("ClaimFateJobs = %+v, %v; want none", got, err)
	}
}

// A change can't un-merge: a close that arrives late, while the merge is
// still to be worked out, leaves the merged job as it is.
func TestRequestFatesAfterAPendingMergeDoesNothing(t *testing.T) {
	s := newStore(t)
	if err := s.RequestFates(ctx, mr1, "acme/widgets", true, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.FateJobFailed(ctx, mr1, t0, t0.Add(time.Hour), 2); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestFates(ctx, mr1, "acme/other", false, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	job, finished, _ := jobOf(t, s, mr1)
	if finished || !job.Merged || job.ProjectPath != "acme/widgets" || job.Attempts != 2 || !job.RequestedAt.Equal(t0) {
		t.Errorf("job = %+v finished=%v; want the merge as it was", job, finished)
	}
	// A merge asked again does start it over.
	if err := s.RequestFates(ctx, mr1, "acme/other", true, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if job, _, _ := jobOf(t, s, mr1); job.Attempts != 0 || !job.RequestedAt.Equal(t0.Add(2*time.Minute)) || job.ProjectPath != "acme/other" {
		t.Errorf("job after a merge asked again = %+v; want it started over", job)
	}
}

func TestRequestFatesAfterACloseResets(t *testing.T) {
	s := newStore(t)
	if err := s.RequestFates(ctx, mr1, "acme/widgets", false, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.FateJobFailed(ctx, mr1, t0, t0.Add(time.Hour), 3); err != nil {
		t.Fatal(err)
	}
	// It finished, was reopened and merged: the merge starts the work again.
	storeFates(t, s, mr1, "acme/widgets", nil, false, false, t0)
	if err := s.RequestFates(ctx, mr1, "acme/renamed", true, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	job, finished, _ := jobOf(t, s, mr1)
	if finished || !job.Merged || job.Attempts != 0 || job.ProjectPath != "acme/renamed" || !job.RequestedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("job = %+v finished=%v; want merged, unfinished, no attempts, renamed, requested at the merge", job, finished)
	}
	got, err := s.ClaimFateJobs(ctx, t0.Add(time.Minute), 10)
	if err != nil || len(got) != 1 || got[0].MR != mr1 || !got[0].Merged {
		t.Errorf("ClaimFateJobs = %+v, %v; want the job at once", got, err)
	}

	// A close redelivered while the job waits asks again: it starts over.
	if err := s.RequestFates(ctx, mr2, "acme/widgets", false, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.FateJobFailed(ctx, mr2, t0, t0.Add(time.Hour), 2); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestFates(ctx, mr2, "acme/widgets", false, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if job, _, _ := jobOf(t, s, mr2); job.Attempts != 0 {
		t.Errorf("attempts after a new request = %d; want 0", job.Attempts)
	}
}

func TestClaimFateJobsSkipsStoppedAndFutureOnes(t *testing.T) {
	s := newStore(t)
	mrs := []MRKey{mr1, mr2, mr3, {Instance: mr1.Instance, ProjectID: 7, IID: 6}, {Instance: mr1.Instance, ProjectID: 7, IID: 7}}
	for i, mr := range mrs {
		// Requested in reverse, to see them come back oldest request first.
		if err := s.RequestFates(ctx, mr, "acme/widgets", i%2 == 0, t0.Add(time.Duration(-i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// mr1 waits for later, mr2 failed for good, mr3 is finished.
	if err := s.FateJobFailed(ctx, mr1, t0, t0.Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.FateJobFailed(ctx, mr2, t0.Add(-time.Minute), t0.Add(-time.Hour), 5); err != nil {
		t.Fatal(err)
	}
	storeFates(t, s, mr3, "acme/widgets", nil, true, false, t0)

	got, err := s.ClaimFateJobs(ctx, t0, 10)
	if err != nil || len(got) != 2 || got[0].MR != mrs[4] || got[1].MR != mrs[3] {
		t.Fatalf("ClaimFateJobs = %+v, %v; want IID 7 then 6", got, err)
	}
	if got[0].ProjectPath != "acme/widgets" || !got[0].Merged || got[0].Attempts != 0 || !got[0].RequestedAt.Equal(t0.Add(-4*time.Minute)) {
		t.Errorf("job = %+v; want what RequestFates stored", got[0])
	}
	// The limit counts, and nothing was marked by claiming.
	if got, _ := s.ClaimFateJobs(ctx, t0, 1); len(got) != 1 || got[0].MR != mrs[4] {
		t.Errorf("ClaimFateJobs limit 1 = %+v; want IID 7", got)
	}
	// Once its time comes, a job that failed is offered, with its attempts.
	got, err = s.ClaimFateJobs(ctx, t0.Add(time.Hour), 10)
	if err != nil || len(got) != 3 || got[2].MR != mr1 || got[2].Attempts != 1 {
		t.Errorf("ClaimFateJobs an hour later = %+v, %v; want IID 7, 6 and mr1 with 1 attempt", got, err)
	}
	if got, err := s.ClaimFateJobs(ctx, t0, 0); err != nil || len(got) != 0 {
		t.Errorf("ClaimFateJobs limit 0 = %+v, %v", got, err)
	}

	// Dropping a job takes it away; a finished one stays.
	if err := s.DropFateJob(ctx, mrs[3], t0); err != nil { // asked again since
		t.Fatal(err)
	}
	if _, _, found := jobOf(t, s, mrs[3]); !found {
		t.Error("a job requested again was dropped")
	}
	if err := s.DropFateJob(ctx, mrs[4], t0.Add(-4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.DropFateJob(ctx, mr3, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, found := jobOf(t, s, mrs[4]); found {
		t.Error("the dropped job remains")
	}
	if _, finished, found := jobOf(t, s, mr3); !found || !finished {
		t.Error("a finished job was dropped")
	}
}

// A failure is recorded on the job a pass claimed, as long as it is still
// to do: a finished job, or one requested again, is left as it is.
func TestFateJobFailedLeavesOtherJobsAlone(t *testing.T) {
	s := newStore(t)
	if err := s.RequestFates(ctx, mr1, "acme/widgets", true, t0); err != nil {
		t.Fatal(err)
	}
	storeFates(t, s, mr2, "acme/widgets", nil, true, false, t0)

	if err := s.FateJobFailed(ctx, mr2, t0, t0.Add(time.Hour), 4); err != nil {
		t.Fatal(err)
	}
	if job, finished, _ := jobOf(t, s, mr2); !finished || job.Attempts != 0 {
		t.Errorf("finished job = %+v finished=%v; want it untouched", job, finished)
	}
	if err := s.FateJobFailed(ctx, mr1, t0.Add(-time.Minute), t0.Add(time.Hour), 4); err != nil {
		t.Fatal(err)
	}
	if job, _, _ := jobOf(t, s, mr1); job.Attempts != 0 {
		t.Errorf("job requested again = %+v; want it untouched", job)
	}
	if err := s.FateJobFailed(ctx, mr1, t0, t0.Add(time.Hour), 4); err != nil {
		t.Fatal(err)
	}
	if job, _, _ := jobOf(t, s, mr1); job.Attempts != 4 {
		t.Errorf("job = %+v; want 4 attempts", job)
	}
}

// decidedRun gives mr a published run of head whose review was decided, made
// at created.
func decidedRun(t *testing.T, s *Store, mr MRKey, head string, created time.Time) string {
	t.Helper()
	id := published(t, s, mr, head, "main")
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET outcome = 'no_blocking', created_at = $2 WHERE id = $1::uuid`, id, created); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDecidedRunsIncludesSupersededRuns(t *testing.T) {
	s := newStore(t)
	a := decidedRun(t, s, mr1, "aaa", t0)
	b := decidedRun(t, s, mr1, "bbb", t0.Add(time.Hour))
	other := decidedRun(t, s, mr2, "ooo", t0)
	// A push back to A is a new run, and supersedes the published one; its outcome stays.
	a2, created, err := s.EnqueueRun(ctx, mr1, "acme/widgets", "aaa", "main")
	if err != nil || !created || a2 == a {
		t.Fatalf("EnqueueRun = %s, %v, %v; want a new run", a2, created, err)
	}
	if got := getRun(t, s, a); got.State != Superseded || got.Outcome != "no_blocking" {
		t.Fatalf("run a = %s, outcome %q; want superseded, with its outcome", got.State, got.Outcome)
	}

	// The new run is not decided yet.
	got, err := s.DecidedRuns(ctx, mr1)
	if err != nil || len(got) != 2 || got[0].ID != a || got[1].ID != b {
		t.Fatalf("DecidedRuns = %+v, %v; want a then b", got, err)
	}
	if got[0].HeadSHA != "aaa" || got[0].MR != mr1 || got[0].State != Superseded {
		t.Errorf("run = %+v; want it whole", got[0])
	}
	if got, _ := s.DecidedRuns(ctx, mr2); len(got) != 1 || got[0].ID != other {
		t.Errorf("DecidedRuns(mr2) = %+v; want its own run only", got)
	}
}

// A run decided again (a dismissal) goes back to reported and keeps its outcome.
func TestDecidedRunsIncludesRunsToBeDecidedAgain(t *testing.T) {
	s := newStore(t)
	id := decidedRun(t, s, mr1, "aaa", t0)
	if ok, err := s.Republish(ctx, mr1, "aaa", "main"); err != nil || !ok {
		t.Fatalf("Republish = %v, %v", ok, err)
	}
	got, err := s.DecidedRuns(ctx, mr1)
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].State != Reported {
		t.Fatalf("DecidedRuns = %+v, %v; want the run, reported", got, err)
	}
}

func TestDecidedRunsLeavesOutUndecidedOnes(t *testing.T) {
	s := newStore(t)
	id := decidedRun(t, s, mr1, "aaa", t0)
	// A queued run, and a published one with no outcome (before outcomes were recorded).
	queued := enqueue(t, s, mr1, "bbb")
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET outcome = '' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	got, err := s.DecidedRuns(ctx, mr1)
	if err != nil || len(got) != 0 {
		t.Fatalf("DecidedRuns = %+v, %v; want none (queued %s)", got, err, queued)
	}
}

func TestChangesWithoutFates(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	since := now.Add(-24 * time.Hour)
	decidedRun(t, s, mr1, "aaa", now.Add(-2*time.Hour))
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET project_path = 'acme/old' WHERE mr_iid = $1`, mr1.IID); err != nil {
		t.Fatal(err)
	}
	decidedRun(t, s, mr1, "bbb", now.Add(-time.Hour))    // the newest run gives the path
	decidedRun(t, s, mr2, "ccc", now.Add(-48*time.Hour)) // too old
	decidedRun(t, s, mr3, "ddd", now.Add(-time.Hour))    // has a job
	if err := s.RequestFates(ctx, mr3, "acme/widgets", true, now); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 9}, "eee") // never decided

	got, err := s.ChangesWithoutFates(ctx, since, 10)
	if err != nil || len(got) != 1 || got[0].MR != mr1 || got[0].ProjectPath != "acme/widgets" {
		t.Fatalf("ChangesWithoutFates = %+v, %v; want mr1 once, with the newest run's path", got, err)
	}
	if got, err := s.ChangesWithoutFates(ctx, since, 0); err != nil || len(got) != 0 {
		t.Errorf("ChangesWithoutFates limit 0 = %+v, %v", got, err)
	}

	// Once it has a job it is not offered.
	if err := s.RequestFates(ctx, mr1, "acme/widgets", false, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ChangesWithoutFates(ctx, since, 10); len(got) != 0 {
		t.Errorf("ChangesWithoutFates after a job = %+v; want none", got)
	}
}

// One change per repository is offered, the one least recently checked, the
// unchecked ones oldest first: open changes that were looked at, or are newer,
// don't keep a missed merge from being found.
func TestChangesWithoutFatesRotates(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	since := now.Add(-24 * time.Hour)
	missed := mr1                                                    // merged, the webhook missed; reviewed longest ago
	open := mr2                                                      // still open, reviewed more recently (same repository as missed)
	elsewhere := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 1} // another repository
	decidedRun(t, s, missed, "aaa", now.Add(-3*time.Hour))
	decidedRun(t, s, open, "bbb", now.Add(-time.Hour))
	decidedRun(t, s, elsewhere, "ccc", now.Add(-2*time.Hour))

	// Each repository gets a row, the oldest review of the unchecked first, so
	// a new open change doesn't hide an old missed merge.
	got, err := s.ChangesWithoutFates(ctx, since, 10)
	if err != nil || len(got) != 2 || got[0].MR != missed || got[1].MR != elsewhere {
		t.Fatalf("ChangesWithoutFates = %+v, %v; want the missed merge, then the other repository's", got, err)
	}
	if got, _ := s.ChangesWithoutFates(ctx, since, 1); len(got) != 1 || got[0].MR != missed {
		t.Errorf("ChangesWithoutFates limit 1 = %+v; want the missed merge", got)
	}

	// Once the first was checked (it was still open), the other change of its repository comes.
	if err := s.FateChecked(ctx, missed, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.ChangesWithoutFates(ctx, since, 10)
	if err != nil || len(got) != 2 || got[0].MR != elsewhere || got[1].MR != open {
		t.Fatalf("ChangesWithoutFates after a check = %+v, %v; want the other repository's, then the open change", got, err)
	}

	// All checked: the least recently checked of each repository, the oldest check first.
	if err := s.FateChecked(ctx, elsewhere, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.FateChecked(ctx, open, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ChangesWithoutFates(ctx, since, 10)
	if len(got) != 2 || got[0].MR != missed || got[1].MR != elsewhere {
		t.Errorf("ChangesWithoutFates all checked = %+v; want the change checked first, then the other repository's", got)
	}

	// Checking it again moves it behind.
	if err := s.FateChecked(ctx, missed, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ChangesWithoutFates(ctx, since, 10)
	if len(got) != 2 || got[0].MR != elsewhere || got[1].MR != open {
		t.Errorf("ChangesWithoutFates after checking again = %+v; want the other repository's, then the open change", got)
	}
}

// A change closed and counted, then reopened and merged without the merge
// being recorded, is offered again once it has a review after the close.
func TestChangesWithoutFatesAfterAClose(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	since := now.Add(-24 * time.Hour)
	closedAt := now.Add(-2 * time.Hour)
	decidedRun(t, s, mr1, "aaa", now.Add(-3*time.Hour))
	storeFates(t, s, mr1, "acme/widgets", nil, false, false, closedAt)
	if got, err := s.ChangesWithoutFates(ctx, since, 10); err != nil || len(got) != 0 {
		t.Fatalf("ChangesWithoutFates with only a review before the close = %+v, %v; want none", got, err)
	}

	decidedRun(t, s, mr1, "bbb", now.Add(-time.Hour))
	// Not offered: merged and counted, and a job still to do.
	decidedRun(t, s, mr2, "ccc", now.Add(-3*time.Hour))
	storeFates(t, s, mr2, "acme/widgets", nil, true, false, closedAt)
	decidedRun(t, s, mr2, "ddd", now.Add(-time.Hour))
	decidedRun(t, s, mr3, "eee", now.Add(-3*time.Hour))
	if err := s.RequestFates(ctx, mr3, "acme/widgets", false, closedAt); err != nil {
		t.Fatal(err)
	}
	decidedRun(t, s, mr3, "fff", now.Add(-time.Hour))

	got, err := s.ChangesWithoutFates(ctx, since, 10)
	if err != nil || len(got) != 1 || got[0].MR != mr1 {
		t.Errorf("ChangesWithoutFates = %+v, %v; want mr1 only", got, err)
	}
}

// seedResults stores the fates the SkillResults tests count: mr1 merged by a
// person, mr2 closed by a person, mr3 merged by a bot.
func seedResults(t *testing.T, s *Store, at time.Time) {
	t.Helper()
	for _, c := range []struct {
		mr     MRKey
		rows   []FindingFate
		merged bool
		byBot  bool
	}{
		{mr1, []FindingFate{
			fate("a", "sec", "h1", "blocking", "fixed"),
			fate("b", "sec", "h1", "blocking", "dismissed"),
			fate("c", "sec", "h1", "suggestion", "open"),
			fate("d", "style", "h9", "suggestion", "went_away"),
			fate("e", "sec", "h1", "blocking", "fixed"),
		}, true, false},
		{mr2, []FindingFate{
			fate("a", "sec", "h1", "blocking", "dismissed"),
			fate("f", "sec", "h1", "blocking", "fixed"), // closed, not dismissed: left out
			fate("g", "style", "h9", "suggestion", "open"),
		}, false, false},
		{mr3, []FindingFate{fate("a", "sec", "h1", "blocking", "fixed")}, true, true},
	} {
		storeFates(t, s, c.mr, "acme/widgets", c.rows, c.merged, c.byBot, at)
	}
}

func TestSkillResultsCountsMergedPeople(t *testing.T) {
	s := newStore(t)
	other := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 1}
	storeFates(t, s, other, "acme/other", []FindingFate{fate("a", "sec", "h1", "blocking", "fixed")}, true, false, t0)
	storeFates(t, s, mr1, "acme/widgets", []FindingFate{
		fate("a", "sec", "h1", "blocking", "fixed"),
		fate("b", "sec", "h1", "blocking", "dismissed"),
		fate("c", "sec", "h1", "blocking", "open"),
		fate("d", "sec", "h1", "blocking", "went_away"),
		fate("e", "sec", "h1", "blocking", "fixed"),
		fate("f", "sec", "h1", "suggestion", "fixed"),
		fate("g", "style", "h9", "suggestion", "open"),
	}, true, false, t0)
	got, err := s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0.Add(-time.Hour))
	want := []SkillResult{
		{Skill: "sec", Severity: "blocking", Raised: 5, Fixed: 2, Dismissed: 1, Open: 1, WentAway: 1},
		{Skill: "sec", Severity: "suggestion", Raised: 1, Fixed: 1},
		{Skill: "style", Severity: "suggestion", Raised: 1, Open: 1},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("SkillResults = %+v, %v; want %+v", got, err, want)
	}

	// The window is by when it was decided, and it is the repository's alone.
	if got, _ := s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0.Add(time.Second)); len(got) != 0 {
		t.Errorf("SkillResults after the window = %+v; want none", got)
	}
	if got, _ := s.SkillResults(ctx, mr1.Instance, 8, t0); len(got) != 1 || got[0].Raised != 1 {
		t.Errorf("SkillResults of another repository = %+v; want its own one finding", got)
	}
}

func TestSkillResultsDismissedOnClosedCounts(t *testing.T) {
	s := newStore(t)
	seedResults(t, s, t0)
	got, err := s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	// mr1: sec blocking a fixed, b dismissed, e fixed; mr2 adds the dismissed a,
	// which is dismissed but not raised: only merged changes raise.
	want := []SkillResult{
		{Skill: "sec", Severity: "blocking", Raised: 3, Fixed: 2, Dismissed: 2},
		{Skill: "sec", Severity: "suggestion", Raised: 1, Open: 1},
		{Skill: "style", Severity: "suggestion", Raised: 1, WentAway: 1},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("SkillResults = %+v, %v; want %+v", got, err, want)
	}
}

// Addressed is Fixed over Raised, so what people dismissed on changes they
// closed must not raise anything: 10 fixed on merged changes and 10 dismissed
// on closed ones is 10 raised, not 20.
func TestSkillResultsRaisedIsMergedOnly(t *testing.T) {
	s := newStore(t)
	var fixed, dismissed []FindingFate
	for i := 0; i < 10; i++ {
		fp := string(rune('a' + i))
		fixed = append(fixed, fate(fp, "sec", "h1", "blocking", "fixed"))
		dismissed = append(dismissed, fate(fp, "sec", "h1", "blocking", "dismissed"))
	}
	storeFates(t, s, mr1, "acme/widgets", fixed, true, false, t0)
	storeFates(t, s, mr2, "acme/widgets", dismissed, false, false, t0)
	got, err := s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	want := []SkillResult{{Skill: "sec", Severity: "blocking", Raised: 10, Fixed: 10, Dismissed: 10}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("SkillResults = %+v, %v; want %+v", got, err, want)
	}

	// Only dismissals of a closed change: a row with nothing raised.
	storeFates(t, s, mr1, "acme/widgets", nil, true, false, t0)
	got, _ = s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	want = []SkillResult{{Skill: "sec", Severity: "blocking", Dismissed: 10}}
	if !slices.Equal(got, want) {
		t.Errorf("SkillResults with only closed dismissals = %+v; want %+v", got, want)
	}
}

func TestSkillResultsLeavesOutBots(t *testing.T) {
	s := newStore(t)
	// Merged by a bot, and closed by a bot with a dismissed finding: none counts.
	storeFates(t, s, mr3, "acme/widgets", []FindingFate{fate("a", "sec", "h1", "blocking", "fixed")}, true, true, t0)
	storeFates(t, s, mr2, "acme/widgets", []FindingFate{fate("a", "sec", "h1", "blocking", "dismissed")}, false, true, t0)
	got, err := s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	if err != nil || len(got) != 0 {
		t.Errorf("SkillResults = %+v, %v; want none", got, err)
	}
	seedResults(t, s, t0)
	got, _ = s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	if len(got) != 3 || got[0].Raised != 3 || got[0].Dismissed != 2 {
		t.Errorf("SkillResults with a bot's change = %+v; want it left out", got)
	}
}

func TestSkillResultsEditedAt(t *testing.T) {
	s := newStore(t)
	rows := func(hash string) []FindingFate {
		return []FindingFate{fate("a", "sec", hash, "blocking", "fixed"), fate("b", "sec", hash, "suggestion", "open"), fate("z", "style", "same", "suggestion", "open")}
	}
	day := 24 * time.Hour
	for _, c := range []struct {
		mr   MRKey
		hash string
		at   time.Time
	}{
		{mr1, "old", t0},
		{mr2, "new", t0.Add(2 * day)},
		{mr3, "new", t0.Add(3 * day)},
	} {
		storeFates(t, s, c.mr, "acme/widgets", rows(c.hash), true, false, c.at)
	}
	got, err := s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	if err != nil || len(got) != 3 {
		t.Fatalf("SkillResults = %+v, %v; want 3 rows", got, err)
	}
	first := t0.Add(2 * day)
	for _, r := range got[:2] { // sec, both severities: the newest hash first appears with mr2
		if r.Skill != "sec" || r.EditedAt == nil || !r.EditedAt.Equal(first) {
			t.Errorf("row %s/%s EditedAt = %v; want %s", r.Skill, r.Severity, r.EditedAt, first)
		}
	}
	if got[2].Skill != "style" || got[2].EditedAt != nil {
		t.Errorf("a skill with one hash = %+v; want no EditedAt", got[2])
	}

	// A window holding only the newer hash has nothing to tell.
	got, _ = s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0.Add(day))
	if got[0].EditedAt != nil {
		t.Errorf("EditedAt with one hash in the window = %v; want nil", got[0].EditedAt)
	}

	// A finding with no recorded hash isn't another version, nor the newest one.
	extra := MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 8}
	storeFates(t, s, extra, "acme/widgets", rows(""), true, false, t0.Add(4*day))
	got, _ = s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	if got[0].EditedAt == nil || !got[0].EditedAt.Equal(first) {
		t.Errorf("EditedAt with an empty hash newest = %v; want %s", got[0].EditedAt, first)
	}
	got, _ = s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0.Add(day))
	if got[0].Raised != 3 || got[0].EditedAt != nil {
		t.Errorf("a window with one hash and an empty one = %+v; want no EditedAt", got[0])
	}
}

// The newest hash is the one that appeared last, whatever the dates of its
// findings: a change that took long to merge brings an old hash late.
func TestSkillResultsEditedAtNamesTheNewestHash(t *testing.T) {
	s := newStore(t)
	day := 24 * time.Hour
	one := func(hash string) []FindingFate { return []FindingFate{fate("a", "sec", hash, "blocking", "fixed")} }
	storeFates(t, s, mr1, "acme/widgets", one("old"), true, false, t0.Add(5*day))
	storeFates(t, s, mr2, "acme/widgets", one("old"), true, false, t0)
	storeFates(t, s, mr3, "acme/widgets", one("new"), true, false, t0.Add(2*day))
	got, err := s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	if err != nil || len(got) != 1 || got[0].EditedAt == nil || !got[0].EditedAt.Equal(t0.Add(2*day)) {
		t.Fatalf("SkillResults = %+v, %v; want EditedAt %s, when the new hash first appears", got, err, t0.Add(2*day))
	}
}

func TestPruneFates(t *testing.T) {
	s := newStore(t)
	old, recent := t0, t0.Add(400*24*time.Hour)
	storeFates(t, s, mr1, "acme/widgets", []FindingFate{fate("a", "sec", "", "blocking", "fixed"), fate("b", "sec", "", "blocking", "open")}, true, false, old)
	storeFates(t, s, mr2, "acme/widgets", []FindingFate{fate("a", "sec", "", "blocking", "fixed")}, true, false, recent)
	for mr, at := range map[MRKey]time.Time{mr1: old, mr2: recent} {
		if err := s.FateChecked(ctx, mr, at); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneFates(ctx, t0.Add(365*24*time.Hour))
	if err != nil || n != 4 { // two rows, the old change's job and its check
		t.Fatalf("PruneFates = %d, %v; want 4", n, err)
	}
	var checks int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM fate_checks`).Scan(&checks); err != nil || checks != 1 {
		t.Errorf("checks left = %d, %v; want 1", checks, err)
	}
	if got := fatesOf(t, s, mr1); len(got) != 0 {
		t.Errorf("old fates remain: %v", got)
	}
	if got := fatesOf(t, s, mr2); len(got) != 1 {
		t.Errorf("recent fates = %v; want 1", got)
	}
	if _, _, found := jobOf(t, s, mr1); found {
		t.Error("the old job remains")
	}
	if _, _, found := jobOf(t, s, mr2); !found {
		t.Error("the recent job was pruned")
	}
}

func TestDeleteProjectRemovesFates(t *testing.T) {
	s := newStore(t)
	otherProject := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 3}
	otherInstance := MRKey{Instance: "lab", ProjectID: 7, IID: 3}
	for _, mr := range []MRKey{mr1, otherProject, otherInstance} {
		storeFates(t, s, mr, "acme/widgets", []FindingFate{fate("a", "sec", "", "blocking", "fixed")}, true, false, t0)
	}
	for _, mr := range []MRKey{mr1, otherProject, otherInstance} {
		if err := s.FateChecked(ctx, mr, t0); err != nil {
			t.Fatal(err)
		}
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := s.DeleteProject(ctx, mr1.Instance, mr1.ProjectID); err != nil {
		t.Fatal(err)
	}
	if count("finding_fates") != 2 || count("fate_jobs") != 2 || count("fate_checks") != 2 {
		t.Errorf("after DeleteProject: %d fates, %d jobs, %d checks; want 2, 2, 2", count("finding_fates"), count("fate_jobs"), count("fate_checks"))
	}
	if err := s.DeleteInstanceData(ctx, mr1.Instance); err != nil {
		t.Fatal(err)
	}
	if count("finding_fates") != 1 || count("fate_jobs") != 1 || count("fate_checks") != 1 {
		t.Errorf("after DeleteInstanceData: %d fates, %d jobs, %d checks; want 1, 1, 1", count("finding_fates"), count("fate_jobs"), count("fate_checks"))
	}
	if got := fatesOf(t, s, otherInstance); len(got) != 1 {
		t.Errorf("another instance's fates = %v; want kept", got)
	}
}
