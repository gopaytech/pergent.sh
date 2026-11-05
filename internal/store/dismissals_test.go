package store

import (
	"fmt"
	"slices"
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

func TestDismissStoresDetails(t *testing.T) {
	s := newStore(t)
	in := Dismissal{MR: mr1, Fingerprint: "fp1", Severity: "blocking", Reason: "constant", Actor: "mo (gitlab main)",
		Skill: "security", File: "db/query.go", Title: "SQL built from user input", Teaches: true, DismisserPlatformID: 50}
	if created, err := s.Dismiss(ctx, in); err != nil || !created {
		t.Fatalf("Dismiss = %v, %v", created, err)
	}
	got, err := s.Dismissals(ctx, mr1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Dismissals = %+v, %v", got, err)
	}
	d := got[0]
	if d.Skill != in.Skill || d.File != in.File || d.Title != in.Title || !d.Teaches || d.DismisserPlatformID != 50 {
		t.Errorf("dismissal = %+v; want the details it was stored with", d)
	}

	// The defaults: nothing known, nothing taught.
	if _, err := s.Dismiss(ctx, Dismissal{MR: mr2, Fingerprint: "fp1", Severity: "suggestion"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Dismissals(ctx, mr2)
	if len(got) != 1 || got[0].Skill != "" || got[0].File != "" || got[0].Title != "" || got[0].Teaches || got[0].DismisserPlatformID != 0 {
		t.Errorf("dismissal with no details = %+v", got)
	}
}

// Raising a suggestion to blocking replaces the details too: the person who
// dismissed it as blocking is the one it teaches by.
func TestDismissRaiseReplacesDetails(t *testing.T) {
	s := newStore(t)
	first := Dismissal{MR: mr1, Fingerprint: "fp1", Severity: "suggestion", Reason: "fine", Actor: "dev",
		Skill: "style", File: "a.go", Title: "old", Teaches: false}
	if _, err := s.Dismiss(ctx, first); err != nil {
		t.Fatal(err)
	}
	raised := Dismissal{MR: mr1, Fingerprint: "fp1", Severity: "blocking", Reason: "false positive", Actor: "mo",
		Skill: "security", File: "b.go", Title: "new", Teaches: true, DismisserPlatformID: 50}
	if created, err := s.Dismiss(ctx, raised); err != nil || !created {
		t.Fatalf("raise: Dismiss = %v, %v", created, err)
	}
	got, _ := s.Dismissals(ctx, mr1)
	if len(got) != 1 {
		t.Fatalf("Dismissals = %+v", got)
	}
	d := got[0]
	if d.Skill != "security" || d.File != "b.go" || d.Title != "new" || !d.Teaches || d.DismisserPlatformID != 50 {
		t.Errorf("after raise = %+v; want the details of the blocking dismissal", d)
	}
}

func TestDismissTeachingReplacesNonTeaching(t *testing.T) {
	s := newStore(t)
	author := Dismissal{MR: mr1, Fingerprint: "fp1", Severity: "suggestion", Reason: "not now", Actor: "dev",
		Skill: "style", File: "a.go", Title: "old", DismisserPlatformID: 7}
	maintainer := Dismissal{MR: mr1, Fingerprint: "fp1", Severity: "suggestion", Reason: "false positive", Actor: "mo",
		Skill: "style", File: "a.go", Title: "old", Teaches: true, DismisserPlatformID: 50}
	if _, err := s.Dismiss(ctx, author); err != nil {
		t.Fatal(err)
	}

	noReason := maintainer
	noReason.Reason = ""
	if created, err := s.Dismiss(ctx, noReason); err != nil || created {
		t.Fatalf("no reason: Dismiss = %v, %v; want nothing stored", created, err)
	}
	if created, err := s.Dismiss(ctx, maintainer); err != nil || !created {
		t.Fatalf("teaching: Dismiss = %v, %v; want stored", created, err)
	}
	got, _ := s.Dismissals(ctx, mr1)
	if len(got) != 1 || !got[0].Teaches || got[0].Reason != "false positive" || got[0].Actor != "mo" || got[0].DismisserPlatformID != 50 {
		t.Fatalf("Dismissals = %+v; want the maintainer's", got)
	}

	// One that teaches stays as it is.
	if created, err := s.Dismiss(ctx, author); err != nil || created {
		t.Fatalf("not teaching: Dismiss = %v, %v; want nothing stored", created, err)
	}
	again := maintainer
	again.Reason = "another reason"
	if created, err := s.Dismiss(ctx, again); err != nil || created {
		t.Fatalf("teaching twice: Dismiss = %v, %v; want nothing stored", created, err)
	}
}

func TestDismissTeachingKeepsBlocking(t *testing.T) {
	s := newStore(t)
	blocking := Dismissal{MR: mr1, Fingerprint: "fp1", Severity: "blocking", Reason: "false positive", Actor: "mo",
		Skill: "security", File: "a.go", Title: "t", Teaches: true, DismisserPlatformID: 50}
	if _, err := s.Dismiss(ctx, blocking); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.StopTeaching(ctx, mr1.Instance, mr1.ProjectID, "fp1"); err != nil || !ok {
		t.Fatalf("StopTeaching = %v, %v", ok, err)
	}
	suggestion := blocking
	suggestion.Severity, suggestion.Reason = "suggestion", "still wrong"
	if created, err := s.Dismiss(ctx, suggestion); err != nil || !created {
		t.Fatalf("Dismiss = %v, %v; want stored", created, err)
	}
	got, _ := s.Dismissals(ctx, mr1)
	if len(got) != 1 || got[0].Severity != "blocking" || !got[0].Teaches {
		t.Fatalf("Dismissals = %+v; want it blocking and teaching", got)
	}
}

func TestTeachingDismissals(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	since := at.Add(-time.Hour)
	other := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 3}
	foreign := MRKey{Instance: "https://other.example.com", ProjectID: 7, IID: 3}
	base := Dismissal{Reason: "r", Skill: "security", File: "a.go", Title: "t", Teaches: true, CreatedAt: at}
	for _, d := range []struct {
		fp, severity string
		mr           MRKey
		edit         func(*Dismissal)
	}{
		{"soft-new", "suggestion", mr2, func(d *Dismissal) { d.CreatedAt = at.Add(3 * time.Minute) }},
		{"soft-old", "suggestion", mr1, func(d *Dismissal) {}},
		{"hard-old", "blocking", mr1, func(d *Dismissal) { d.CreatedAt = at.Add(-30 * time.Minute) }},
		{"hard-new", "blocking", mr1, func(d *Dismissal) { d.CreatedAt = at.Add(time.Minute) }},
		{"no-teach", "blocking", mr1, func(d *Dismissal) { d.Teaches = false }},
		{"no-reason", "suggestion", mr1, func(d *Dismissal) { d.Reason = "" }},
		{"no-skill", "suggestion", mr1, func(d *Dismissal) { d.Skill = "" }},
		{"before-since", "blocking", mr1, func(d *Dismissal) { d.CreatedAt = since.Add(-time.Minute) }},
		{"other-repo", "blocking", other, func(d *Dismissal) {}},
		{"other-instance", "blocking", foreign, func(d *Dismissal) {}},
	} {
		x := base
		x.MR, x.Fingerprint, x.Severity = d.mr, d.fp, d.severity
		d.edit(&x)
		if _, err := s.Dismiss(ctx, x); err != nil {
			t.Fatalf("Dismiss(%s): %v", d.fp, err)
		}
	}

	got, err := s.TeachingDismissals(ctx, mr1.Instance, mr1.ProjectID, since)
	if err != nil {
		t.Fatal(err)
	}
	var fps []string
	for _, d := range got {
		fps = append(fps, d.Fingerprint)
	}
	// Blocking first, then newest first.
	want := []string{"hard-new", "hard-old", "soft-new", "soft-old"}
	if !slices.Equal(fps, want) {
		t.Fatalf("TeachingDismissals = %v, want %v", fps, want)
	}
	if got[2].MR != mr2 || got[0].MR != mr1 {
		t.Errorf("MR of the rows = %+v, %+v; want the change each came from", got[0].MR, got[2].MR)
	}
	d := got[0]
	if d.Skill != "security" || d.File != "a.go" || d.Title != "t" || d.Reason != "r" || !d.Teaches || d.Severity != "blocking" {
		t.Errorf("row = %+v", d)
	}
}

func TestStopTeachingEveryChange(t *testing.T) {
	s := newStore(t)
	other := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 3}
	for _, mr := range []MRKey{mr1, mr2, other} {
		if _, err := s.Dismiss(ctx, Dismissal{MR: mr, Fingerprint: "fp1", Severity: "blocking", Reason: "r", Skill: "security", Teaches: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Dismiss(ctx, Dismissal{MR: mr1, Fingerprint: "fp2", Severity: "blocking", Reason: "r", Skill: "security", Teaches: true}); err != nil {
		t.Fatal(err)
	}

	if ok, err := s.StopTeaching(ctx, mr1.Instance, mr1.ProjectID, "nope"); err != nil || ok {
		t.Fatalf("StopTeaching(unknown) = %v, %v; want false", ok, err)
	}
	if ok, err := s.StopTeaching(ctx, mr1.Instance, mr1.ProjectID, "fp1"); err != nil || !ok {
		t.Fatalf("StopTeaching = %v, %v; want true", ok, err)
	}
	for _, mr := range []MRKey{mr1, mr2} {
		got, _ := s.Dismissals(ctx, mr)
		for _, d := range got {
			if d.Fingerprint == "fp1" && (d.Teaches || d.Reason != "r") {
				t.Errorf("%+v: still teaches, or the row changed", d)
			}
			if d.Fingerprint == "fp2" && !d.Teaches {
				t.Errorf("%+v: another finding stopped teaching", d)
			}
		}
		if len(got) == 0 {
			t.Errorf("the dismissals of %+v were removed", mr)
		}
	}
	// Another repository's dismissal of the same fingerprint is its own.
	if got, _ := s.Dismissals(ctx, other); len(got) != 1 || !got[0].Teaches {
		t.Errorf("another repository's dismissal = %+v; want it left alone", got)
	}
	// Nothing is left to stop.
	if ok, _ := s.StopTeaching(ctx, mr1.Instance, mr1.ProjectID, "fp1"); ok {
		t.Error("second StopTeaching = true")
	}
}

// addRunWithResult stores a published run of mr at head with result as its
// stored result.
func addRunWithResult(t *testing.T, s *Store, mr MRKey, head string, at time.Time, result string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, state, result, created_at)
		VALUES ($1, $2, $3, 'acme/widgets', $4, 'published', $5::jsonb, $6)`,
		mr.Instance, mr.ProjectID, mr.IID, head, result, at); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillDismissals(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	since := at.Add(-24 * time.Hour)
	dismissedAt := at.Add(2 * time.Hour)
	for _, d := range []Dismissal{
		{MR: mr1, Fingerprint: "fp1", Severity: "blocking", Reason: "r", CreatedAt: dismissedAt},
		{MR: mr1, Fingerprint: "fp2", Severity: "blocking", Reason: "r", CreatedAt: dismissedAt,
			Skill: "style", File: "kept.go", Title: "kept", Teaches: true},
		// Its change has no run holding the fingerprint.
		{MR: mr2, Fingerprint: "fp1", Severity: "suggestion", CreatedAt: dismissedAt},
		// Before since.
		{MR: MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 5}, Fingerprint: "fp1", Severity: "suggestion", CreatedAt: since.Add(-time.Hour)},
	} {
		if _, err := s.Dismiss(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	older := `{"skills":[{"name":"security","findings":[{"fingerprint":"fp1","file":"old.go","title":"old title"}]}]}`
	newer := `{"skills":[
		{"name":"style","findings":[{"fingerprint":"fp9","file":"z.go","title":"z"}]},
		{"name":"security","findings":[{"fingerprint":"fp1","file":"new.go","title":"new title"},
			{"fingerprint":"fp2","file":"other.go","title":"other"}]}]}`
	addRunWithResult(t, s, mr1, "1111", at, older)
	addRunWithResult(t, s, mr1, "2222", at.Add(time.Hour), newer)
	// The same fingerprint in a run of another change is not this change's.
	addRunWithResult(t, s, MRKey{Instance: mr1.Instance, ProjectID: 7, IID: 5}, "3333", at,
		`{"skills":[{"name":"security","findings":[{"fingerprint":"fp1","file":"x.go","title":"x"}]}]}`)

	n, err := s.BackfillDismissals(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("BackfillDismissals = %d, want 1", n)
	}
	got, _ := s.Dismissals(ctx, mr1)
	byFP := map[string]Dismissal{}
	for _, d := range got {
		byFP[d.Fingerprint] = d
	}
	if d := byFP["fp1"]; d.Skill != "security" || d.File != "new.go" || d.Title != "new title" {
		t.Errorf("fp1 = %+v; want the details from the newest run holding it", d)
	}
	if d := byFP["fp2"]; d.Skill != "style" || d.File != "kept.go" || d.Title != "kept" {
		t.Errorf("fp2 = %+v; want it left as it was", d)
	}
	if got, _ := s.Dismissals(ctx, mr2); len(got) != 1 || got[0].Skill != "" {
		t.Errorf("a change with no such finding = %+v; want it left empty", got)
	}
	// Teaching is the dismisser's access, not something to guess from a result.
	if byFP["fp1"].Teaches {
		t.Error("backfill made a dismissal teach")
	}
	// Run again: nothing is left to fill.
	if n, err := s.BackfillDismissals(ctx, since); err != nil || n != 0 {
		t.Errorf("second BackfillDismissals = %d, %v; want 0", n, err)
	}
}

// Stored results are not trusted to have the shape the schema says: a result
// that does not fit leaves its dismissal as it is, and is no error.
func TestBackfillDismissalsOddResults(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.Dismiss(ctx, Dismissal{MR: mr1, Fingerprint: "fp1", Severity: "blocking", Reason: "r", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	for i, result := range []string{
		`{"skills":"oops"}`,
		`{"skills":{"name":"a"}}`,
		`{"skills":[7,null,"x"]}`,
		`{"skills":[{"name":"a","findings":null}]}`,
		`{"skills":[{"name":"a","findings":"nope"}]}`,
		`{"skills":[{"name":"a","findings":[7,null,"fp1",[1]]}]}`,
		`{"skills":[{"findings":[{"fingerprint":"fp1","file":"a.go"}]}]}`,
		`{"skills":[{"name":"a","findings":[{"fingerprint":["fp1"]}]}]}`,
		`"just a string"`,
		`null`,
		`[]`,
	} {
		addRunWithResult(t, s, mr1, fmt.Sprintf("%04d", i), at.Add(time.Duration(i)*time.Minute), result)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, state, created_at)
		VALUES ($1, $2, $3, 'acme/widgets', 'noresult', 'queued', $4)`, mr1.Instance, mr1.ProjectID, mr1.IID, at); err != nil {
		t.Fatal(err)
	}

	n, err := s.BackfillDismissals(ctx, at.Add(-time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("BackfillDismissals = %d, %v; want 0 and no error", n, err)
	}
	got, _ := s.Dismissals(ctx, mr1)
	if len(got) != 1 || got[0].Skill != "" || got[0].File != "" || got[0].Title != "" {
		t.Errorf("dismissal = %+v; want it empty", got)
	}
}

// Dismissals made before the details were stored: a blocking one came from
// a maintainer, so it teaches; a suggestion one is not known to.
func TestMigrationTeachesOldBlocking(t *testing.T) {
	s := newStoreAt(t, 21)
	for fp, severity := range map[string]string{"hard": "blocking", "soft": "suggestion"} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO dismissals (instance, project_id, mr_iid, fingerprint, severity)
			VALUES ($1, $2, $3, $4, $5)`, mr1.Instance, mr1.ProjectID, mr1.IID, fp, severity); err != nil {
			t.Fatal(err)
		}
	}
	applyUpTo(t, s, 22)
	got, err := s.Dismissals(ctx, mr1)
	if err != nil || len(got) != 2 {
		t.Fatalf("Dismissals = %+v, %v", got, err)
	}
	for _, d := range got {
		if want := d.Fingerprint == "hard"; d.Teaches != want {
			t.Errorf("%s: Teaches = %v, want %v", d.Fingerprint, d.Teaches, want)
		}
		if d.Skill != "" || d.File != "" || d.Title != "" || d.DismisserPlatformID != 0 {
			t.Errorf("%s: %+v; want empty details", d.Fingerprint, d)
		}
	}
}
