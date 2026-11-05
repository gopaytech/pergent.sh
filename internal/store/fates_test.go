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
	if err := s.ReplaceFates(ctx, mr1, "acme/widgets", first, false, false, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFates(ctx, mr2, "acme/widgets", first, true, false, t0); err != nil {
		t.Fatal(err)
	}
	if got := fatesOf(t, s, mr1); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("fates = %v; want a, b", got)
	}
	job, finished, found := jobOf(t, s, mr1)
	if !found || !finished || job.Merged || job.ProjectPath != "acme/widgets" {
		t.Errorf("job after a close = %+v finished=%v found=%v; want finished, closed", job, finished, found)
	}

	// The change is merged later: its rows are replaced, the other change's are not.
	if err := s.ReplaceFates(ctx, mr1, "acme/widgets", []FindingFate{fate("c", "sec", "h2", "blocking", "fixed")}, true, true, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
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
	if err := s.ReplaceFates(ctx, mr1, "acme/widgets", nil, true, false, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := fatesOf(t, s, mr1); len(got) != 0 {
		t.Errorf("fates = %v; want none", got)
	}

	// A bad row stores nothing, and the rows already there stay.
	err := s.ReplaceFates(ctx, mr2, "acme/widgets", []FindingFate{fate("x", "sec", "", "blocking", "fixed"), fate("y", "sec", "", "blocking", "bogus")}, true, false, t0)
	if err == nil {
		t.Error("an unknown fate was stored")
	}
	if got := fatesOf(t, s, mr2); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("fates after a failed replace = %v; want a, b", got)
	}
}

func TestRequestFatesAfterAMergeDoesNothing(t *testing.T) {
	s := newStore(t)
	if err := s.RequestFates(ctx, mr1, "acme/widgets", true, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFates(ctx, mr1, "acme/widgets", []FindingFate{fate("a", "sec", "", "blocking", "fixed")}, true, false, t0); err != nil {
		t.Fatal(err)
	}
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

func TestRequestFatesAfterACloseResets(t *testing.T) {
	s := newStore(t)
	if err := s.RequestFates(ctx, mr1, "acme/widgets", false, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.FateJobFailed(ctx, mr1, t0.Add(time.Hour), 3); err != nil {
		t.Fatal(err)
	}
	// It finished, was reopened and merged: the merge starts the work again.
	if err := s.ReplaceFates(ctx, mr1, "acme/widgets", nil, false, false, t0); err != nil {
		t.Fatal(err)
	}
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

	// A close redelivered while the job waits changes nothing but the request.
	if err := s.RequestFates(ctx, mr2, "acme/widgets", false, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.FateJobFailed(ctx, mr2, t0.Add(time.Hour), 2); err != nil {
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
	if err := s.FateJobFailed(ctx, mr1, t0.Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.FateJobFailed(ctx, mr2, t0.Add(-time.Hour), 5); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFates(ctx, mr3, "acme/widgets", nil, true, false, t0); err != nil {
		t.Fatal(err)
	}

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
	if err := s.DropFateJob(ctx, mrs[4]); err != nil {
		t.Fatal(err)
	}
	if err := s.DropFateJob(ctx, mr3); err != nil {
		t.Fatal(err)
	}
	if _, _, found := jobOf(t, s, mrs[4]); found {
		t.Error("the dropped job remains")
	}
	if _, finished, found := jobOf(t, s, mr3); !found || !finished {
		t.Error("a finished job was dropped")
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
	decidedRun(t, s, mr1, "aaa", now.Add(-2*time.Hour)) // its path is replaced by the newest run's
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET project_path = 'acme/old' WHERE mr_iid = $1`, mr1.IID); err != nil {
		t.Fatal(err)
	}
	decidedRun(t, s, mr1, "bbb", now.Add(-time.Hour))
	decidedRun(t, s, mr2, "ccc", now.Add(-48*time.Hour)) // too old
	decidedRun(t, s, mr3, "ddd", now.Add(-time.Hour))    // has a job
	if err := s.RequestFates(ctx, mr3, "acme/widgets", true, now); err != nil {
		t.Fatal(err)
	}
	last := MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 9}
	enqueue(t, s, last, "eee") // never decided

	got, err := s.ChangesWithoutFates(ctx, since, 10)
	if err != nil || len(got) != 1 || got[0].MR != mr1 || got[0].ProjectPath != "acme/widgets" {
		t.Fatalf("ChangesWithoutFates = %+v, %v; want mr1 once, with the newest run's path", got, err)
	}

	// Once it has a job it is not offered again; the limit counts.
	other := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 1}
	decidedRun(t, s, other, "fff", now.Add(-30*time.Minute))
	if got, _ := s.ChangesWithoutFates(ctx, since, 1); len(got) != 1 || got[0].MR != other {
		t.Errorf("ChangesWithoutFates limit 1 = %+v; want the newest, %v", got, other)
	}
	if err := s.RequestFates(ctx, mr1, "acme/widgets", false, now); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ChangesWithoutFates(ctx, since, 10)
	if len(got) != 1 || got[0].MR != other {
		t.Errorf("ChangesWithoutFates after a job = %+v; want only %v", got, other)
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
		if err := s.ReplaceFates(ctx, c.mr, "acme/widgets", c.rows, c.merged, c.byBot, at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSkillResultsCountsMergedPeople(t *testing.T) {
	s := newStore(t)
	other := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 1}
	if err := s.ReplaceFates(ctx, other, "acme/other", []FindingFate{fate("a", "sec", "h1", "blocking", "fixed")}, true, false, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFates(ctx, mr1, "acme/widgets", []FindingFate{
		fate("a", "sec", "h1", "blocking", "fixed"),
		fate("b", "sec", "h1", "blocking", "dismissed"),
		fate("c", "sec", "h1", "blocking", "open"),
		fate("d", "sec", "h1", "blocking", "went_away"),
		fate("e", "sec", "h1", "blocking", "fixed"),
		fate("f", "sec", "h1", "suggestion", "fixed"),
		fate("g", "style", "h9", "suggestion", "open"),
	}, true, false, t0); err != nil {
		t.Fatal(err)
	}
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
	// mr1: sec blocking a fixed, b dismissed, e fixed; mr2 adds the dismissed a.
	want := []SkillResult{
		{Skill: "sec", Severity: "blocking", Raised: 4, Fixed: 2, Dismissed: 2},
		{Skill: "sec", Severity: "suggestion", Raised: 1, Open: 1},
		{Skill: "style", Severity: "suggestion", Raised: 1, WentAway: 1},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("SkillResults = %+v, %v; want %+v", got, err, want)
	}
}

func TestSkillResultsLeavesOutBots(t *testing.T) {
	s := newStore(t)
	// Merged by a bot, and closed by a bot with a dismissed finding: none counts.
	if err := s.ReplaceFates(ctx, mr3, "acme/widgets", []FindingFate{fate("a", "sec", "h1", "blocking", "fixed")}, true, true, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFates(ctx, mr2, "acme/widgets", []FindingFate{fate("a", "sec", "h1", "blocking", "dismissed")}, false, true, t0); err != nil {
		t.Fatal(err)
	}
	got, err := s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	if err != nil || len(got) != 0 {
		t.Errorf("SkillResults = %+v, %v; want none", got, err)
	}
	seedResults(t, s, t0)
	got, _ = s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	if len(got) != 3 || got[0].Raised != 4 {
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
		if err := s.ReplaceFates(ctx, c.mr, "acme/widgets", rows(c.hash), true, false, c.at); err != nil {
			t.Fatal(err)
		}
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
	if err := s.ReplaceFates(ctx, extra, "acme/widgets", rows(""), true, false, t0.Add(4*day)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0)
	if got[0].EditedAt == nil || !got[0].EditedAt.Equal(first) {
		t.Errorf("EditedAt with an empty hash newest = %v; want %s", got[0].EditedAt, first)
	}
	got, _ = s.SkillResults(ctx, mr1.Instance, mr1.ProjectID, t0.Add(day))
	if got[0].Raised != 3 || got[0].EditedAt != nil {
		t.Errorf("a window with one hash and an empty one = %+v; want no EditedAt", got[0])
	}
}

func TestPruneFates(t *testing.T) {
	s := newStore(t)
	old, recent := t0, t0.Add(400*24*time.Hour)
	if err := s.ReplaceFates(ctx, mr1, "acme/widgets", []FindingFate{fate("a", "sec", "", "blocking", "fixed"), fate("b", "sec", "", "blocking", "open")}, true, false, old); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFates(ctx, mr2, "acme/widgets", []FindingFate{fate("a", "sec", "", "blocking", "fixed")}, true, false, recent); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneFates(ctx, t0.Add(365*24*time.Hour))
	if err != nil || n != 3 { // two rows and the old change's job
		t.Fatalf("PruneFates = %d, %v; want 3", n, err)
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
	if err := s.CreateInstance(ctx, InstanceRow{Name: mr1.Instance, Platform: "gitlab"}, map[string][]byte{"review_token": []byte("x")}, t0); err != nil {
		t.Fatal(err)
	}
	for _, mr := range []MRKey{mr1, otherProject, otherInstance} {
		if err := s.ReplaceFates(ctx, mr, "acme/widgets", []FindingFate{fate("a", "sec", "", "blocking", "fixed")}, true, false, t0); err != nil {
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
	if count("finding_fates") != 2 || count("fate_jobs") != 2 {
		t.Errorf("after DeleteProject: %d fates, %d jobs; want 2, 2", count("finding_fates"), count("fate_jobs"))
	}
	if err := s.DeleteInstanceData(ctx, mr1.Instance); err != nil {
		t.Fatal(err)
	}
	if count("finding_fates") != 1 || count("fate_jobs") != 1 {
		t.Errorf("after DeleteInstanceData: %d fates, %d jobs; want 1, 1", count("finding_fates"), count("fate_jobs"))
	}
	if got := fatesOf(t, s, otherInstance); len(got) != 1 {
		t.Errorf("another instance's fates = %v; want kept", got)
	}
}
