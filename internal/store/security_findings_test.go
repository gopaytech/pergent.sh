package store

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// secCommit is the commit scanRequest stores on a scan.
const secCommit = "0123456789abcdef0123456789abcdef01234567"

func secFinding(fp string) SecurityFindingInput {
	return SecurityFindingInput{Fingerprint: fp, RuleID: "injection", CWE: "CWE-89", Tags: []string{"sql"},
		Severity: "high", Title: "SQL injection in " + fp, Description: "The query is built from input.",
		Exploit: "Send a quote.", FixHint: "Use a bound value.", Confidence: "high", File: "api/" + fp + ".go",
		StartLine: 10}
}

// secResult is a usable result of status with skill commit skill and a
// finding per fingerprint.
func secResult(status, skill string, fps ...string) SecurityResult {
	res := SecurityResult{Status: status, Commit: secCommit, UsageSource: "stats", SkillName: "security-review",
		SkillCommit: skill, State: map[string]string{"plan.md": status}, SARIF: []byte(`{"version": "2.1.0", "runs": []}`)}
	if status != SecurityComplete {
		res.Reason = "step limit reached"
	}
	for _, fp := range fps {
		res.Findings = append(res.Findings, secFinding(fp))
	}
	return res
}

// secRepo runs the scans of one repository one after another, each a
// minute after the last.
type secRepo struct {
	s                 *Store
	instance, project string
	n                 int
}

func newSecRepo(s *Store, project string) *secRepo {
	return &secRepo{s: s, instance: "main", project: project}
}

func (r *secRepo) now() time.Time { return t0.Add(time.Duration(r.n) * time.Minute) }

// start queues a scan and makes it run with the token "tok-<n>".
func (r *secRepo) start(t *testing.T) SecurityScan {
	t.Helper()
	r.n++
	q := scanRequest(r.instance, r.project)
	sc := queueSecurity(t, r.s, q, r.now())
	if _, err := r.s.pool.Exec(ctx, `UPDATE security_scans SET state = 'running', started_at = $2, token_hash = $3
		WHERE id = $1::uuid`, sc.ID, r.now(), r.token()); err != nil {
		t.Fatal(err)
	}
	return sc
}

func (r *secRepo) token() []byte { return hash(fmt.Sprintf("tok-%d", r.n)) }

// run starts a scan, finishes it with res and returns it as stored.
func (r *secRepo) run(t *testing.T, res SecurityResult) SecurityScan {
	t.Helper()
	sc := r.start(t)
	ok, err := r.s.FinishSecurityScan(ctx, sc.ID, r.token(), res, r.now().Add(30*time.Second))
	if err != nil || !ok {
		t.Fatalf("FinishSecurityScan(scan %d) = %v, %v; want true", sc.Number, ok, err)
	}
	got, err := r.s.SecurityScan(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// findings returns the repository's findings by fingerprint.
func (r *secRepo) findings(t *testing.T) map[string]SecurityFinding {
	t.Helper()
	rows, truncated, err := r.s.SecurityFindings(ctx, r.instance, r.project, SecurityFindingFilter{Limit: 100})
	if err != nil || truncated {
		t.Fatalf("SecurityFindings = %v, truncated %v", err, truncated)
	}
	out := map[string]SecurityFinding{}
	for _, f := range rows {
		out[f.Fingerprint] = f
	}
	return out
}

// events returns the kinds of a finding's events, oldest first.
func (r *secRepo) events(t *testing.T, id int64) []string {
	t.Helper()
	evs, err := r.s.SecurityFindingEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func (r *secRepo) dismiss(t *testing.T, id int64) {
	t.Helper()
	if changed, err := r.s.DismissSecurityFinding(ctx, r.instance, r.project, id, "false_positive", "", "alice",
		r.now()); err != nil || !changed {
		t.Fatalf("DismissSecurityFinding = %v, %v", changed, err)
	}
}

// A finding found by a complete scan is open; a complete scan without it
// fixes it; found again it is open; a dismissed one stays dismissed, also
// when a complete scan no longer finds it. A finding found again takes the
// new scan's fields.
func TestSecurityFindingStates(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	r := newSecRepo(s, "acme/api")
	var scans []SecurityScan

	t.Run("scan 1 finds A and B", func(t *testing.T) {
		sc := r.run(t, secResult(SecurityComplete, "aaa", "A", "B"))
		scans = append(scans, sc)
		if sc.State != SecurityComplete || sc.NewCount != 2 || sc.FixedCount != 0 || sc.SkillUpdated {
			t.Errorf("scan 1 = %s, new %d, fixed %d, skill updated %v; want complete, 2, 0, false",
				sc.State, sc.NewCount, sc.FixedCount, sc.SkillUpdated)
		}
		f := r.findings(t)
		for _, fp := range []string{"A", "B"} {
			got := f[fp]
			if got.State != "open" || got.FirstScanID != sc.ID || got.LastScanID != sc.ID || got.FixedScanID != nil ||
				got.Severity != "high" || got.RuleID != "injection" || !slices.Equal(got.Tags, []string{"sql"}) ||
				got.File != "api/"+fp+".go" || got.StartLine != 10 || got.Project != "acme/api" {
				t.Errorf("finding %s = %+v; want open, found by scan 1", fp, got)
			}
			if kinds := r.events(t, got.ID); !slices.Equal(kinds, []string{"found"}) {
				t.Errorf("finding %s events = %v; want [found]", fp, kinds)
			}
		}
	})

	t.Run("scan 2 without B fixes it", func(t *testing.T) {
		sc := r.run(t, secResult(SecurityComplete, "aaa", "A"))
		scans = append(scans, sc)
		if sc.NewCount != 0 || sc.FixedCount != 1 {
			t.Errorf("scan 2 = new %d, fixed %d; want 0, 1", sc.NewCount, sc.FixedCount)
		}
		f := r.findings(t)
		if b := f["B"]; b.State != "fixed" || b.FixedScanID == nil || *b.FixedScanID != sc.ID || b.LastScanID != scans[0].ID {
			t.Errorf("B = %+v; want fixed by scan 2, last seen by scan 1", b)
		}
		if a := f["A"]; a.State != "open" || a.LastScanID != sc.ID || a.FirstScanID != scans[0].ID {
			t.Errorf("A = %+v; want open, last seen by scan 2", a)
		}
		evs, err := s.SecurityFindingEvents(ctx, f["B"].ID)
		if err != nil || len(evs) != 2 || evs[1].Kind != "fixed" || evs[1].ScanID != sc.ID {
			t.Errorf("B events = %+v, %v; want found, then fixed by scan 2", evs, err)
		}
	})

	t.Run("scan 3 finds B again", func(t *testing.T) {
		sc := r.run(t, secResult(SecurityComplete, "aaa", "A", "B"))
		scans = append(scans, sc)
		if sc.NewCount != 1 || sc.FixedCount != 0 {
			t.Errorf("scan 3 = new %d, fixed %d; want 1, 0", sc.NewCount, sc.FixedCount)
		}
		b := r.findings(t)["B"]
		if b.State != "open" || b.FixedScanID != nil || b.FirstScanID != scans[0].ID || b.LastScanID != sc.ID {
			t.Errorf("B = %+v; want open again, first found by scan 1", b)
		}
		if kinds := r.events(t, b.ID); !slices.Equal(kinds, []string{"found", "fixed", "reopened"}) {
			t.Errorf("B events = %v; want [found fixed reopened]", kinds)
		}
	})

	t.Run("a dismissed finding stays dismissed", func(t *testing.T) {
		r.dismiss(t, r.findings(t)["A"].ID)
		res := secResult(SecurityComplete, "aaa", "B")
		res.Findings[0].Severity, res.Findings[0].Title, res.Findings[0].StartLine = "critical", "Worse now", 12
		sc := r.run(t, res)
		if sc.NewCount != 0 || sc.FixedCount != 0 {
			t.Errorf("scan 4 = new %d, fixed %d; want 0, 0", sc.NewCount, sc.FixedCount)
		}
		f := r.findings(t)
		if a := f["A"]; a.State != "dismissed" || a.FixedScanID != nil {
			t.Errorf("A = %+v; want still dismissed", a)
		}
		if b := f["B"]; b.State != "open" || b.Severity != "critical" || b.Title != "Worse now" || b.StartLine != 12 ||
			b.LastScanID != sc.ID {
			t.Errorf("B = %+v; want open with scan 4's fields", b)
		}
		// Found again while dismissed: it stays dismissed, with no event.
		sc = r.run(t, secResult(SecurityComplete, "aaa", "A", "B"))
		a := r.findings(t)["A"]
		if a.State != "dismissed" || a.LastScanID != sc.ID || sc.NewCount != 0 {
			t.Errorf("A found again = %+v, scan new %d; want dismissed, last seen by scan 5, nothing new", a, sc.NewCount)
		}
		if kinds := r.events(t, a.ID); !slices.Equal(kinds, []string{"found", "dismissed"}) {
			t.Errorf("A events = %v; want [found dismissed]", kinds)
		}
	})
}

// A partial or failed scan records what it found but fixes nothing.
func TestSecurityPartialFixesNothing(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for _, status := range []string{SecurityPartial, SecurityFailed} {
		t.Run(status, func(t *testing.T) {
			r := newSecRepo(s, "acme/"+status)
			r.run(t, secResult(SecurityComplete, "aaa", "A"))
			sc := r.run(t, secResult(status, "aaa", "C"))
			if sc.State != status || sc.Reason != "step limit reached" || sc.FixedCount != 0 || sc.NewCount != 1 {
				t.Errorf("scan = %s %q, new %d, fixed %d; want %s, 1 new, 0 fixed", sc.State, sc.Reason, sc.NewCount,
					sc.FixedCount, status)
			}
			f := r.findings(t)
			if f["A"].State != "open" || f["A"].FixedScanID != nil {
				t.Errorf("A = %+v; want still open", f["A"])
			}
			if f["C"].State != "open" || f["C"].FirstScanID != sc.ID {
				t.Errorf("C = %+v; want open, found by the %s scan", f["C"], status)
			}
		})
	}
}

// The first complete scan after the skill changes fixes nothing; the next
// one does. A partial scan, or a failed one with no skill, is no change.
func TestSecuritySkillUpdatedFixesNothing(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	t.Run("new skill commit", func(t *testing.T) {
		r := newSecRepo(s, "acme/api")
		r.run(t, secResult(SecurityComplete, "aaa", "A"))
		sc := r.run(t, secResult(SecurityComplete, "bbb"))
		if !sc.SkillUpdated || sc.FixedCount != 0 || sc.SkillCommit != "bbb" || sc.SkillName != "security-review" {
			t.Errorf("scan 2 = skill updated %v, fixed %d, skill %s@%s; want true, 0, security-review@bbb",
				sc.SkillUpdated, sc.FixedCount, sc.SkillName, sc.SkillCommit)
		}
		if a := r.findings(t)["A"]; a.State != "open" {
			t.Errorf("A after scan 2 = %s; want open", a.State)
		}
		sc = r.run(t, secResult(SecurityComplete, "bbb"))
		if sc.SkillUpdated || sc.FixedCount != 1 {
			t.Errorf("scan 3 = skill updated %v, fixed %d; want false, 1", sc.SkillUpdated, sc.FixedCount)
		}
		if a := r.findings(t)["A"]; a.State != "fixed" {
			t.Errorf("A after scan 3 = %s; want fixed", a.State)
		}
	})

	t.Run("only complete scans count", func(t *testing.T) {
		r := newSecRepo(s, "acme/web")
		r.run(t, secResult(SecurityComplete, "aaa", "A"))
		early := secResult(SecurityFailed, "")
		early.SkillName, early.State, early.SARIF = "", nil, nil
		if sc := r.run(t, early); sc.SkillUpdated || sc.SkillName != "" {
			t.Errorf("failed scan with no skill = skill updated %v, name %q", sc.SkillUpdated, sc.SkillName)
		}
		r.run(t, secResult(SecurityPartial, "bbb", "A"))
		sc := r.run(t, secResult(SecurityComplete, "aaa"))
		if sc.SkillUpdated || sc.FixedCount != 1 {
			t.Errorf("complete scan = skill updated %v, fixed %d; want false, 1", sc.SkillUpdated, sc.FixedCount)
		}
	})
}

// After 1 complete scan and 11 partial ones, the oldest partial scan lost
// its SARIF and state, the newest 10 keep theirs, and the complete one
// keeps both although it is older than the newest 10.
func TestSecurityRetentionKeepsLastComplete(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	r := newSecRepo(s, "acme/api")
	if _, found, err := s.LastCompleteSecurityState(ctx, "main", "acme/api"); err != nil || found {
		t.Errorf("LastCompleteSecurityState with no scan = %v, %v; want none", found, err)
	}
	first := secResult(SecurityComplete, "aaa")
	first.State = map[string]string{"plan.md": "first", "notes/a.md": "seen"}
	complete := r.run(t, first)
	var partials []SecurityScan
	for range 11 {
		partials = append(partials, r.run(t, secResult(SecurityPartial, "aaa")))
	}

	sarif := func(t *testing.T, id string) bool {
		t.Helper()
		b, found, err := s.SecurityScanSARIF(ctx, id)
		if err != nil || found != (len(b) > 0) {
			t.Fatalf("SecurityScanSARIF = %q, %v, %v", b, found, err)
		}
		return found
	}
	for _, c := range []struct {
		name string
		id   string
		want bool
	}{
		{"the oldest partial", partials[0].ID, false},
		{"the second partial", partials[1].ID, true},
		{"the newest partial", partials[10].ID, true},
		{"the last complete", complete.ID, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := sarif(t, c.id); got != c.want {
				t.Errorf("has SARIF = %v; want %v", got, c.want)
			}
			sc, err := s.SecurityScan(ctx, c.id)
			if err != nil || sc.HasSARIF != c.want {
				t.Errorf("HasSARIF = %v, %v; want %v", sc.HasSARIF, err, c.want)
			}
			var hasState bool
			if err := s.pool.QueryRow(ctx, `SELECT state_files IS NOT NULL FROM security_scans WHERE id = $1::uuid`,
				c.id).Scan(&hasState); err != nil || hasState != c.want {
				t.Errorf("has state = %v, %v; want %v", hasState, err, c.want)
			}
		})
	}
	t.Run("the last complete state", func(t *testing.T) {
		state, found, err := s.LastCompleteSecurityState(ctx, "main", "ACME/api")
		if err != nil || !found || len(state) != 2 || state["plan.md"] != "first" || state["notes/a.md"] != "seen" {
			t.Errorf("LastCompleteSecurityState = %v, %v, %v; want the complete scan's state", state, found, err)
		}
	})
	t.Run("raw SARIF", func(t *testing.T) {
		if b, found, err := s.SecurityScanSARIF(ctx, complete.ID); err != nil || !found || len(b) == 0 {
			t.Errorf("SecurityScanSARIF = %q, %v, %v", b, found, err)
		}
		if _, found, err := s.SecurityScanSARIF(ctx, "not-a-uuid"); err != nil || found {
			t.Errorf("SecurityScanSARIF of a malformed id = %v, %v", found, err)
		}
	})
}

// A scan takes one result: a wrong token, a second result, a scan not
// running and a bad status change nothing; a result for another commit
// fails the scan and keeps none of it.
func TestSecurityFinishOnce(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	findingCount := func(t *testing.T, project string) int {
		t.Helper()
		rows, _, err := s.SecurityFindings(ctx, "main", project, SecurityFindingFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}

	t.Run("wrong token, then its token, then again", func(t *testing.T) {
		r := newSecRepo(s, "acme/api")
		sc := r.start(t)
		now := r.now().Add(time.Minute)
		res := secResult(SecurityComplete, "aaa", "A")
		res.CoverageDone, res.CoveragePlanned = ptrTo(3), ptrTo(4)
		if ok, err := s.FinishSecurityScan(ctx, sc.ID, hash("other"), res, now); err != nil || ok {
			t.Errorf("finish with a wrong token = %v, %v; want false", ok, err)
		}
		if got, _ := s.SecurityScan(ctx, sc.ID); got.State != SecurityRunning || findingCount(t, "acme/api") != 0 {
			t.Errorf("after a wrong token: state %s; want still running, no findings", got.State)
		}
		if ok, err := s.SecurityTokenIs(ctx, sc.ID, r.token()); err != nil || !ok {
			t.Errorf("token after a wrong one = %v, %v; want still valid", ok, err)
		}
		if ok, err := s.FinishSecurityScan(ctx, sc.ID, r.token(), res, now); err != nil || !ok {
			t.Fatalf("finish = %v, %v; want true", ok, err)
		}
		got, err := s.SecurityScan(ctx, sc.ID)
		if err != nil || got.State != SecurityComplete || got.FinishedAt == nil || !got.FinishedAt.Equal(now) ||
			got.Commit != secCommit || got.UsageSource != "stats" || got.CoverageDone == nil || *got.CoverageDone != 3 ||
			got.CoveragePlanned == nil || *got.CoveragePlanned != 4 || got.SkillName != "security-review" ||
			got.SkillCommit != "aaa" || !got.HasSARIF || got.NewCount != 1 {
			t.Errorf("finished scan = %+v, %v", got, err)
		}
		if ok, err := s.SecurityTokenIs(ctx, sc.ID, r.token()); err != nil || ok {
			t.Errorf("token after the finish = %v, %v; want spent", ok, err)
		}
		again := secResult(SecurityComplete, "aaa", "A", "B")
		if ok, err := s.FinishSecurityScan(ctx, sc.ID, r.token(), again, now.Add(time.Minute)); err != nil || ok {
			t.Errorf("second finish = %v, %v; want false", ok, err)
		}
		if ok, err := s.FinishSecurityScan(ctx, sc.ID, nil, again, now.Add(time.Minute)); err != nil || ok {
			t.Errorf("second finish with no token check = %v, %v; want false", ok, err)
		}
		if n := findingCount(t, "acme/api"); n != 1 {
			t.Errorf("findings after a second finish = %d; want 1", n)
		}
	})

	t.Run("no token check", func(t *testing.T) {
		r := newSecRepo(s, "acme/sweep")
		sc := r.start(t)
		res := secResult(SecurityFailed, "")
		res.Reason = "the scan Job ended with no result"
		if ok, err := s.FinishSecurityScan(ctx, sc.ID, nil, res, r.now()); err != nil || !ok {
			t.Errorf("finish with no token check = %v, %v; want true", ok, err)
		}
	})

	t.Run("not running", func(t *testing.T) {
		sc := queueSecurity(t, s, scanRequest("main", "acme/queued"), t0)
		if ok, err := s.FinishSecurityScan(ctx, sc.ID, nil, secResult(SecurityComplete, "aaa", "A"), t0); err != nil || ok {
			t.Errorf("finish of a queued scan = %v, %v; want false", ok, err)
		}
		if ok, err := s.FinishSecurityScan(ctx, "not-a-uuid", nil, secResult(SecurityComplete, "aaa"), t0); err != nil || ok {
			t.Errorf("finish of a malformed id = %v, %v; want false", ok, err)
		}
		if n := findingCount(t, "acme/queued"); n != 0 {
			t.Errorf("findings = %d; want 0", n)
		}
	})

	t.Run("bad status", func(t *testing.T) {
		r := newSecRepo(s, "acme/bad")
		sc := r.start(t)
		for _, st := range []string{SecurityRunning, "done", ""} {
			if ok, err := s.FinishSecurityScan(ctx, sc.ID, r.token(), secResult(st, "aaa", "A"), r.now()); err == nil || ok {
				t.Errorf("finish with status %q = %v, %v; want an error", st, ok, err)
			}
		}
		bad := secResult(SecurityComplete, "aaa", "A")
		bad.Findings[0].Severity = "error"
		if ok, err := s.FinishSecurityScan(ctx, sc.ID, r.token(), bad, r.now()); err == nil || ok {
			t.Errorf("finish with a bad severity = %v, %v; want an error", ok, err)
		}
		if got, _ := s.SecurityScan(ctx, sc.ID); got.State != SecurityRunning || findingCount(t, "acme/bad") != 0 {
			t.Errorf("after bad results: state %s; want still running, no findings", got.State)
		}
	})

	t.Run("another commit", func(t *testing.T) {
		r := newSecRepo(s, "acme/commit")
		sc := r.start(t)
		res := secResult(SecurityComplete, "aaa", "A")
		res.Commit = "ffffffffffffffffffffffffffffffffffffffff"
		if ok, err := s.FinishSecurityScan(ctx, sc.ID, r.token(), res, r.now()); err != nil || !ok {
			t.Fatalf("finish for another commit = %v, %v; want true", ok, err)
		}
		got, err := s.SecurityScan(ctx, sc.ID)
		if err != nil || got.State != SecurityFailed ||
			got.Reason != "pergent sent a result pergent.sh couldn't use: the result is for another commit" ||
			got.Commit != secCommit || got.HasSARIF || got.SkillName != "" || got.NewCount != 0 {
			t.Errorf("scan = %+v, %v; want failed as unusable, keeping nothing", got, err)
		}
		if n := findingCount(t, "acme/commit"); n != 0 {
			t.Errorf("findings = %d; want 0", n)
		}
	})
}

// The list puts open findings first, then by severity and file, filters by
// state, severity, rule and a text in the file or title, and says when the
// limit cut it. The counts are of open findings only.
func TestSecurityFindingFilters(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	r := newSecRepo(s, "acme/api")
	res := secResult(SecurityComplete, "aaa")
	res.Findings = []SecurityFindingInput{
		{Fingerprint: "X", RuleID: "injection", Severity: "high", Title: "SQL injection in the user search", File: "api/users.go"},
		{Fingerprint: "Y", RuleID: "path-traversal", Severity: "critical", Title: "Path traversal on upload", File: "web/upload.go"},
		{Fingerprint: "Z", RuleID: "injection", Severity: "low", Title: "Command injection in Backup", File: "api/admin.go",
			NeedsValidation: true},
	}
	r.run(t, res)
	r.dismiss(t, r.findings(t)["Y"].ID)

	for _, c := range []struct {
		name      string
		f         SecurityFindingFilter
		want      []string
		truncated bool
	}{
		{"all", SecurityFindingFilter{}, []string{"X", "Z", "Y"}, false},
		{"open", SecurityFindingFilter{State: "open"}, []string{"X", "Z"}, false},
		{"dismissed", SecurityFindingFilter{State: "dismissed"}, []string{"Y"}, false},
		{"severity", SecurityFindingFilter{Severity: "high"}, []string{"X"}, false},
		{"rule", SecurityFindingFilter{Rule: "injection"}, []string{"X", "Z"}, false},
		{"query on a file part", SecurityFindingFilter{Query: "WEB/"}, []string{"Y"}, false},
		{"query on a title word", SecurityFindingFilter{Query: "backup"}, []string{"Z"}, false},
		{"query is plain text", SecurityFindingFilter{Query: "%"}, nil, false},
		{"limit cuts", SecurityFindingFilter{Limit: 2}, []string{"X", "Z"}, true},
		{"limit fits", SecurityFindingFilter{Limit: 3}, []string{"X", "Z", "Y"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows, truncated, err := s.SecurityFindings(ctx, "main", "Acme/API", c.f)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range rows {
				got = append(got, f.Fingerprint)
			}
			if !slices.Equal(got, c.want) || truncated != c.truncated {
				t.Errorf("SecurityFindings(%+v) = %v, truncated %v; want %v, %v", c.f, got, truncated, c.want, c.truncated)
			}
		})
	}
	t.Run("counts", func(t *testing.T) {
		got, err := s.SecurityCountsOf(ctx, "main", "acme/api")
		if want := (SecurityCounts{High: 1, Low: 1, NeedsValidation: 1}); err != nil || got != want {
			t.Errorf("SecurityCountsOf = %+v, %v; want %+v", got, err, want)
		}
	})
	t.Run("dismissed", func(t *testing.T) {
		rows, err := s.DismissedSecurityFindings(ctx, "main", "acme/api")
		if err != nil || len(rows) != 1 || rows[0].Fingerprint != "Y" || rows[0].DismissReason != "false_positive" {
			t.Errorf("DismissedSecurityFindings = %+v, %v; want Y", rows, err)
		}
	})
	t.Run("open in files", func(t *testing.T) {
		rows, err := s.OpenSecurityFindingsIn(ctx, "main", "acme/api", []string{"api/users.go", "web/upload.go", "none.go"})
		if err != nil || len(rows) != 1 || rows[0].Fingerprint != "X" {
			t.Errorf("OpenSecurityFindingsIn = %+v, %v; want X only", rows, err)
		}
		if rows, err := s.OpenSecurityFindingsIn(ctx, "main", "acme/api", nil); err != nil || len(rows) != 0 {
			t.Errorf("OpenSecurityFindingsIn of no files = %+v, %v; want none", rows, err)
		}
	})
}

// A dismissal needs a known reason, records who and why, and happens once;
// a restore makes the finding open again; a finding of another repository
// is not found through this one.
func TestSecurityDismissRestore(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	api, web := newSecRepo(s, "acme/api"), newSecRepo(s, "acme/web")
	api.run(t, secResult(SecurityComplete, "aaa", "A"))
	web.run(t, secResult(SecurityComplete, "aaa", "B"))
	a, b := api.findings(t)["A"], web.findings(t)["B"]
	now := t0.Add(time.Hour)

	t.Run("dismiss", func(t *testing.T) {
		changed, err := s.DismissSecurityFinding(ctx, "main", "ACME/api", a.ID, "wont_fix", "only in a test tool", "alice", now)
		if err != nil || !changed {
			t.Fatalf("DismissSecurityFinding = %v, %v; want changed", changed, err)
		}
		got, found, err := s.SecurityFinding(ctx, "main", "acme/api", a.ID)
		if err != nil || !found || got.State != "dismissed" || got.DismissReason != "wont_fix" ||
			got.DismissComment != "only in a test tool" || got.DismissedBy != "alice" || got.DismissedAt == nil ||
			!got.DismissedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
			t.Errorf("dismissed finding = %+v, %v, %v", got, found, err)
		}
		evs, err := s.SecurityFindingEvents(ctx, a.ID)
		if err != nil || len(evs) != 2 {
			t.Fatalf("events = %+v, %v; want found and dismissed", evs, err)
		}
		if e := evs[1]; e.Kind != "dismissed" || e.ScanID != "" || e.Actor != "alice" || e.Reason != "wont_fix" ||
			e.Comment != "only in a test tool" || !e.At.Equal(now) {
			t.Errorf("dismiss event = %+v", e)
		}
	})
	t.Run("dismiss again", func(t *testing.T) {
		if changed, err := s.DismissSecurityFinding(ctx, "main", "acme/api", a.ID, "mitigated", "", "bob", now); err != nil || changed {
			t.Errorf("second dismiss = %v, %v; want unchanged", changed, err)
		}
		if kinds := api.events(t, a.ID); len(kinds) != 2 {
			t.Errorf("events = %v; want no new one", kinds)
		}
	})
	t.Run("unknown reason", func(t *testing.T) {
		if changed, err := s.DismissSecurityFinding(ctx, "main", "acme/web", b.ID, "boring", "", "bob", now); err == nil || changed {
			t.Errorf("dismiss with an unknown reason = %v, %v; want an error", changed, err)
		}
	})
	t.Run("restore", func(t *testing.T) {
		later := now.Add(time.Minute)
		changed, err := s.RestoreSecurityFinding(ctx, "main", "acme/api", a.ID, "bob", later)
		if err != nil || !changed {
			t.Fatalf("RestoreSecurityFinding = %v, %v; want changed", changed, err)
		}
		got, _, _ := s.SecurityFinding(ctx, "main", "acme/api", a.ID)
		if got.State != "open" || got.DismissReason != "" || got.DismissComment != "" || got.DismissedBy != "" ||
			got.DismissedAt != nil {
			t.Errorf("restored finding = %+v; want open with no dismissal", got)
		}
		evs, _ := s.SecurityFindingEvents(ctx, a.ID)
		if len(evs) != 3 || evs[2].Kind != "restored" || evs[2].Actor != "bob" || !evs[2].At.Equal(later) {
			t.Errorf("events = %+v; want a restored event by bob", evs)
		}
		if changed, err := s.RestoreSecurityFinding(ctx, "main", "acme/api", a.ID, "bob", later); err != nil || changed {
			t.Errorf("restore of an open finding = %v, %v; want unchanged", changed, err)
		}
	})
	t.Run("another repository", func(t *testing.T) {
		if _, found, err := s.SecurityFinding(ctx, "main", "acme/api", b.ID); err != nil || found {
			t.Errorf("SecurityFinding of another repository's finding = %v, %v; want not found", found, err)
		}
		if changed, err := s.DismissSecurityFinding(ctx, "main", "acme/api", b.ID, "wont_fix", "", "alice", now); err != nil || changed {
			t.Errorf("dismiss through another repository = %v, %v; want unchanged", changed, err)
		}
		if got, _, _ := s.SecurityFinding(ctx, "main", "acme/web", b.ID); got.State != "open" {
			t.Errorf("B = %s; want still open", got.State)
		}
		if changed, err := s.RestoreSecurityFinding(ctx, "other", "acme/web", b.ID, "alice", now); err != nil || changed {
			t.Errorf("restore through another instance = %v, %v; want unchanged", changed, err)
		}
	})
}
