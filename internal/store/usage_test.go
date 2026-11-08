package store

import "testing"

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
