package store

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecordUsage(t *testing.T) {
	s := newStore(t)
	id := reported(t, s, mr1, "head1", `{"schema_version":1,"status":"completed","skills":[]}`)
	rows := []SkillUsage{{Skill: "security", Verdict: "failed", In: 10, Out: 2, Findings: 1, Blocking: 1}}
	if err := s.RecordUsage(ctx, id, rows); err != nil {
		t.Fatal(err)
	}
	// A second call keeps the first rows: the background fill and the
	// callback can meet.
	if err := s.RecordUsage(ctx, id, []SkillUsage{{Skill: "security", In: 99}}); err != nil {
		t.Fatal(err)
	}
	var in int64
	var recorded bool
	if err := s.pool.QueryRow(ctx, `SELECT u.tokens_in, r.usage_recorded FROM run_usage u JOIN runs r ON r.id = u.run_id WHERE u.run_id = $1::uuid`, id).Scan(&in, &recorded); err != nil {
		t.Fatal(err)
	}
	if in != 10 || !recorded {
		t.Errorf("tokens_in = %d, recorded = %v; want 10, true", in, recorded)
	}
}

// Counts the Job didn't report are stored as NULL, and the row takes the
// run's own repository, merge request and trigger.
func TestRecordUsageKeepsUnknownCountsNull(t *testing.T) {
	s := newStore(t)
	id := reported(t, s, mr1, "head1", `{"schema_version":1,"status":"completed","skills":[]}`)
	zero := int64(0)
	rows := []SkillUsage{
		{Skill: "old", In: 1, Out: 1},
		{Skill: "new", In: 1, Out: 1, CacheRead: &zero, CacheWrite: &zero, Reasoning: &zero},
	}
	if err := s.RecordUsage(ctx, id, rows); err != nil {
		t.Fatal(err)
	}
	for skill, wantNull := range map[string]bool{"old": true, "new": false} {
		var cacheRead, reasoning *int64
		var project string
		var iid int64
		var trigger string
		if err := s.pool.QueryRow(ctx, `SELECT cache_read, reasoning, project_path, mr_iid, trigger FROM run_usage WHERE run_id = $1::uuid AND skill = $2`,
			id, skill).Scan(&cacheRead, &reasoning, &project, &iid, &trigger); err != nil {
			t.Fatal(err)
		}
		if (cacheRead == nil) != wantNull || (reasoning == nil) != wantNull {
			t.Errorf("%s: cache_read nil = %v, reasoning nil = %v; want nil = %v", skill, cacheRead == nil, reasoning == nil, wantNull)
		}
		if project != "acme/widgets" || iid != mr1.IID || trigger != TriggerPush {
			t.Errorf("%s: project %q, iid %d, trigger %q", skill, project, iid, trigger)
		}
	}
}

// A run with no skills is still marked, so the background fill doesn't read
// it again on every start.
func TestRecordUsageNoSkillsMarksTheRun(t *testing.T) {
	s := newStore(t)
	id := reported(t, s, mr1, "head1", `{"schema_version":1,"status":"completed","skills":[]}`)
	if err := s.RecordUsage(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	var rows int
	var recorded bool
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM run_usage WHERE run_id = $1::uuid), usage_recorded FROM runs WHERE id = $1::uuid`, id).Scan(&rows, &recorded); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || !recorded {
		t.Errorf("rows = %d, recorded = %v; want 0, true", rows, recorded)
	}
}

func TestRunsWithoutUsage(t *testing.T) {
	s := newStore(t)
	a := reported(t, s, mr1, "head1", `{"schema_version":1,"status":"completed","skills":[]}`)
	b := reported(t, s, mr2, "head2", `{"schema_version":1,"status":"completed","skills":[]}`)
	enqueue(t, s, MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 5}, "head3") // no result yet: never returned
	if err := s.RecordUsage(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunsWithoutUsage(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != b {
		t.Errorf("RunsWithoutUsage = %d runs, want only run %s", len(got), b)
	}
}

var usageSeq atomic.Int64

// usageAt is a moment in March 2026, which the usage tests read from
// usageSince on.
func usageAt(day, hour, minute int) time.Time {
	return time.Date(2026, 3, day, hour, minute, 0, 0, time.UTC)
}

var usageSince = usageAt(1, 0, 0)

// skillRow is one skill's usage with every count reported (cache and
// reasoning as zero).
func skillRow(skill, verdict string, in, out int64) SkillUsage {
	zero := int64(0)
	return SkillUsage{Skill: skill, Verdict: verdict, In: in, Out: out, CacheRead: &zero, CacheWrite: &zero, Reasoning: &zero}
}

// usageMR is merge request iid of project projectID on the test instance.
func usageMR(projectID, iid int64) MRKey {
	return MRKey{Instance: mr1.Instance, ProjectID: projectID, IID: iid}
}

// seedUsage inserts a reported run for mr, as the Job's result came in at the
// given time with the given trigger, kind and model, and records its skills.
// The project's path is acme/p<id>.
func seedUsage(t *testing.T, s *Store, mr MRKey, at time.Time, trigger string, incremental bool, model string, skills ...SkillUsage) string {
	t.Helper()
	head := fmt.Sprintf("usage%d", usageSeq.Add(1))
	id := reported(t, s, mr, head, `{"schema_version":1,"status":"completed","skills":[]}`)
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET created_at = $2::timestamptz, reported_at = $2::timestamptz, trigger = $3::text,
			incremental = $4::boolean, model = $5::text, project_path = $6::text WHERE id = $1::uuid`,
		id, at, trigger, incremental, model, fmt.Sprintf("acme/p%d", mr.ProjectID)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, id, skills); err != nil {
		t.Fatal(err)
	}
	return id
}

func wantTokens(t *testing.T, what string, got, want TokenSum) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %+v, want %+v", what, got, want)
	}
}

func TestUsageByRepository(t *testing.T) {
	s := newStore(t)
	// Project 7: two reviews, three skill rows, 185 tokens.
	seedUsage(t, s, usageMR(7, 1), usageAt(5, 10, 0), TriggerPush, false, "m",
		skillRow("security", "no_blocking_issues", 100, 50), skillRow("style", "no_blocking_issues", 10, 5))
	seedUsage(t, s, usageMR(7, 2), usageAt(6, 10, 0), TriggerPush, false, "m", skillRow("security", "no_blocking_issues", 20, 0))
	// Project 8: one review of 1000 tokens, so it comes first.
	seedUsage(t, s, usageMR(8, 1), usageAt(5, 10, 0), TriggerPush, false, "m", skillRow("security", "no_blocking_issues", 1000, 0))
	// Before the period: not counted.
	seedUsage(t, s, usageMR(7, 3), usageAt(1, 0, 0).Add(-time.Second), TriggerPush, false, "m", skillRow("security", "no_blocking_issues", 5000, 0))

	got, err := s.UsageByRepository(ctx, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("UsageByRepository = %d repositories, want 2: %+v", len(got), got)
	}
	if got[0].ProjectID != 8 || got[0].ProjectPath != "acme/p8" || got[0].Reviews != 1 {
		t.Errorf("first = %+v, want project 8 with 1 review", got[0])
	}
	wantTokens(t, "project 8", got[0].Tokens, TokenSum{In: 1000})
	if got[1].ProjectID != 7 || got[1].Instance != mr1.Instance || got[1].Reviews != 2 {
		t.Errorf("second = %+v, want project 7 with 2 reviews (once per run, not per skill)", got[1])
	}
	wantTokens(t, "project 7", got[1].Tokens, TokenSum{In: 130, Out: 55})
}

func TestUsageDaily(t *testing.T) {
	s := newStore(t)
	seedUsage(t, s, usageMR(7, 1), usageAt(5, 23, 30), TriggerPush, false, "m", skillRow("security", "no_blocking_issues", 10, 1))
	seedUsage(t, s, usageMR(7, 2), usageAt(6, 0, 30), TriggerPush, false, "m", skillRow("security", "no_blocking_issues", 20, 2))
	seedUsage(t, s, usageMR(7, 3), usageAt(6, 12, 0), TriggerPush, false, "m", skillRow("security", "no_blocking_issues", 30, 3))
	seedUsage(t, s, usageMR(8, 1), usageAt(6, 12, 0), TriggerPush, false, "m", skillRow("security", "no_blocking_issues", 400, 0))

	all, err := s.UsageDaily(ctx, usageSince, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("UsageDaily(all) = %d days, want 2: %+v", len(all), all)
	}
	if !all[0].Day.Equal(usageAt(5, 0, 0)) || !all[1].Day.Equal(usageAt(6, 0, 0)) || all[0].Day.Location() != time.UTC {
		t.Errorf("days = %v, %v; want midnight UTC of the 5th and the 6th", all[0].Day, all[1].Day)
	}
	wantTokens(t, "5th", all[0].Tokens, TokenSum{In: 10, Out: 1})
	wantTokens(t, "6th", all[1].Tokens, TokenSum{In: 450, Out: 5})

	one, err := s.UsageDaily(ctx, usageSince, mr1.Instance, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 2 {
		t.Fatalf("UsageDaily(project 7) = %d days, want 2", len(one))
	}
	wantTokens(t, "6th of project 7", one[1].Tokens, TokenSum{In: 50, Out: 5})

	late, err := s.UsageDaily(ctx, usageAt(6, 0, 0), "", 0)
	if err != nil || len(late) != 1 {
		t.Fatalf("UsageDaily(since the 6th) = %+v, %v; want 1 day", late, err)
	}
}

func TestUsageRepositoryBySkillMRModel(t *testing.T) {
	s := newStore(t)
	// MR 3 is reviewed twice; the later run has the newer title.
	old := seedUsage(t, s, usageMR(7, 3), usageAt(5, 10, 0), TriggerPush, false, "small",
		skillRow("security", "has_blocking_issues", 100, 0), skillRow("style", "failed", 10, 0))
	newer := seedUsage(t, s, usageMR(7, 3), usageAt(6, 10, 0), TriggerPush, false, "big",
		skillRow("security", "no_blocking_issues", 200, 0), skillRow("style", "no_blocking_issues", 20, 0))
	seedUsage(t, s, usageMR(7, 4), usageAt(6, 11, 0), TriggerPush, false, "big", skillRow("security", "no_blocking_issues", 500, 0))
	seedUsage(t, s, usageMR(8, 1), usageAt(6, 11, 0), TriggerPush, false, "other", skillRow("security", "no_blocking_issues", 9999, 0))
	for id, title := range map[string]string{old: "Old title", newer: "New title"} {
		if _, err := s.pool.Exec(ctx, `UPDATE runs SET title = $2::text, web_url = $3::text WHERE id = $1::uuid`, id, title, "https://gitlab.example.com/mr/"+title); err != nil {
			t.Fatal(err)
		}
	}

	skills, err := s.UsageBySkill(ctx, mr1.Instance, 7, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 || skills[0].Name != "security" || skills[1].Name != "style" {
		t.Fatalf("UsageBySkill = %+v, want security then style", skills)
	}
	wantTokens(t, "security", skills[0].Tokens, TokenSum{In: 800})
	if skills[0].Runs != 3 || skills[0].Failed != 0 || skills[1].Runs != 2 || skills[1].Failed != 1 {
		t.Errorf("runs/failed = %d/%d and %d/%d; want 3/0 and 2/1", skills[0].Runs, skills[0].Failed, skills[1].Runs, skills[1].Failed)
	}

	models, err := s.UsageByModel(ctx, mr1.Instance, 7, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Name != "big" || models[1].Name != "small" {
		t.Fatalf("UsageByModel = %+v, want big then small", models)
	}
	wantTokens(t, "big", models[0].Tokens, TokenSum{In: 720})
	if models[0].Runs != 2 || models[0].Failed != 0 || models[1].Runs != 1 {
		t.Errorf("runs = %d (failed %d) and %d; want 2 (0) and 1: a run counts once, not per skill", models[0].Runs, models[0].Failed, models[1].Runs)
	}

	mrs, err := s.UsageByMR(ctx, mr1.Instance, 7, usageSince, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(mrs) != 2 || mrs[0].IID != 4 || mrs[1].IID != 3 {
		t.Fatalf("UsageByMR = %+v, want !4 then !3", mrs)
	}
	wantTokens(t, "!3", mrs[1].Tokens, TokenSum{In: 330})
	if mrs[1].Reviews != 2 || mrs[1].Title != "New title" || mrs[1].WebURL != "https://gitlab.example.com/mr/New title" {
		t.Errorf("!3 = %+v, want 2 reviews and the latest title and address", mrs[1])
	}
	top, err := s.UsageByMR(ctx, mr1.Instance, 7, usageSince, 1)
	if err != nil || len(top) != 1 || top[0].IID != 4 {
		t.Errorf("UsageByMR(limit 1) = %+v, %v; want only !4", top, err)
	}
}

func TestUsageWasted(t *testing.T) {
	s := newStore(t)
	// A failed skill counts, a passing skill of the same run doesn't.
	seedUsage(t, s, usageMR(7, 1), usageAt(5, 10, 0), TriggerPush, false, "m",
		skillRow("security", "failed", 100, 10), skillRow("style", "no_blocking_issues", 50, 5))
	// A re-run is counted apart, and its failed skill is waste as well.
	seedUsage(t, s, usageMR(7, 2), usageAt(5, 11, 0), "rerun", false, "m", skillRow("security", "no_blocking_issues", 7, 3))
	seedUsage(t, s, usageMR(7, 3), usageAt(5, 12, 0), "rerun_failed", false, "m", skillRow("security", "failed", 20, 0))
	// Before the period, and another repository: not counted.
	seedUsage(t, s, usageMR(7, 4), usageAt(1, 0, 0).Add(-time.Second), TriggerPush, false, "m", skillRow("security", "failed", 5000, 0))
	seedUsage(t, s, usageMR(8, 1), usageAt(5, 10, 0), TriggerPush, false, "m", skillRow("security", "failed", 9000, 0))

	setState := func(id, state, failure string) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = $2::text, failure_reason = $3::text, created_at = $4::timestamptz WHERE id = $1::uuid`,
			id, state, failure, usageAt(5, 9, 0)); err != nil {
			t.Fatal(err)
		}
	}
	// A reported run with no result and a reason: no result.
	setState(dispatched(t, s, usageMR(7, 10), "noresult", "tok-noresult"), "reported", "the Job failed")
	// A reported run with no result and no reason is still being told apart
	// from a failure: not counted.
	setState(dispatched(t, s, usageMR(7, 11), "nofailure", "tok-nofailure"), "reported", "")
	// A superseded run that was dispatched and left no result: superseded.
	setState(dispatched(t, s, usageMR(7, 12), "stopped", "tok-stopped"), "superseded", "")
	// A superseded run that kept its result: neither.
	setState(reported(t, s, usageMR(7, 13), "kept", `{"schema_version":1,"status":"completed","skills":[]}`), "superseded", "")
	// A superseded run that never started: neither.
	setState(enqueue(t, s, usageMR(7, 14), "never"), "superseded", "")

	got, err := s.UsageWasted(ctx, mr1.Instance, 7, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	wantTokens(t, "Failed", got.Failed, TokenSum{In: 120, Out: 10})
	wantTokens(t, "Reruns", got.Reruns, TokenSum{In: 27, Out: 3})
	if got.RerunCount != 2 || got.NoResult != 1 || got.Superseded != 1 {
		t.Errorf("RerunCount, NoResult, Superseded = %d, %d, %d; want 2, 1, 1", got.RerunCount, got.NoResult, got.Superseded)
	}
}

func TestUsageMediansAndOutliers(t *testing.T) {
	s := newStore(t)
	full := func(iid int64, rows ...SkillUsage) string {
		return seedUsage(t, s, usageMR(7, iid), usageAt(5, 10, int(iid)), TriggerPush, false, "m", rows...)
	}
	incremental := func(iid int64, tokens int64) {
		seedUsage(t, s, usageMR(7, iid), usageAt(5, 11, int(iid)), TriggerPush, true, "m", skillRow("security", "no_blocking_issues", tokens, 0))
	}
	full(1, skillRow("security", "no_blocking_issues", 100, 0))
	full(2, skillRow("security", "no_blocking_issues", 60, 40))
	full(3, skillRow("security", "no_blocking_issues", 100, 0))
	// Exactly three times the median, over two skills: not an outlier.
	full(4, skillRow("security", "no_blocking_issues", 200, 0), skillRow("style", "no_blocking_issues", 0, 100))
	over := full(5, skillRow("security", "no_blocking_issues", 300, 1))
	incremental(6, 10)
	incremental(7, 20)
	incremental(8, 30)
	// A re-run of the failed skills covers a subset: in neither kind.
	seedUsage(t, s, usageMR(7, 9), usageAt(5, 12, 0), "rerun_failed", true, "m", skillRow("security", "failed", 9999, 0))

	got, err := s.UsageCostPerReview(ctx, mr1.Instance, 7, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	if got.FullMedian == nil || *got.FullMedian != 100 {
		t.Errorf("FullMedian = %v, want 100", got.FullMedian)
	}
	if got.IncrementalMedian == nil || *got.IncrementalMedian != 20 {
		t.Errorf("IncrementalMedian = %v, want 20", got.IncrementalMedian)
	}
	if len(got.Outliers) != 1 {
		t.Fatalf("Outliers = %+v, want only the review of 301 tokens", got.Outliers)
	}
	o := got.Outliers[0]
	if o.RunID != over || o.IID != 5 || o.Incremental || o.Total != 301 || !o.At.Equal(usageAt(5, 10, 5)) {
		t.Errorf("outlier = %+v, want run %s, !5, full, 301 tokens", o, over)
	}

	none, err := s.UsageCostPerReview(ctx, mr1.Instance, 99, usageSince)
	if err != nil {
		t.Fatal(err)
	}
	if none.FullMedian != nil || none.IncrementalMedian != nil || len(none.Outliers) != 0 {
		t.Errorf("without reviews: %+v, want no medians and no outliers", none)
	}
}

// A count the Job didn't report is not zero: the row adds to Unknown and to
// no sum of that count.
func TestUsageCacheUnknown(t *testing.T) {
	s := newStore(t)
	three, four := int64(3), int64(4)
	known := int64(5)
	seedUsage(t, s, usageMR(7, 1), usageAt(5, 10, 0), TriggerPush, false, "m",
		SkillUsage{Skill: "old", Verdict: "no_blocking_issues", In: 1, Out: 1, CacheWrite: &three, Reasoning: &four},
		SkillUsage{Skill: "new", Verdict: "no_blocking_issues", In: 1, Out: 1, CacheRead: &known, CacheWrite: &known, Reasoning: &known})

	got, err := s.UsageByRepository(ctx, usageSince)
	if err != nil || len(got) != 1 {
		t.Fatalf("UsageByRepository = %+v, %v", got, err)
	}
	wantTokens(t, "tokens", got[0].Tokens, TokenSum{In: 2, Out: 2, CacheRead: 5, CacheWrite: 8, Reasoning: 9, Unknown: 1})
}
