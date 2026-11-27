package store

import (
	"testing"
	"time"
)

// scanUsageEnv is a store with one scan of acme/p7 that used 1,000,000
// tokens on a global entry, in a repository a team owns, and a review of the
// same repository beside it, so no check is empty by accident.
type scanUsageEnv struct {
	s        *Store
	scan     SecurityScan
	team     int64
	reviewID string
	at       time.Time
}

func newScanUsageEnv(t *testing.T) scanUsageEnv {
	t.Helper()
	s := newStore(t)
	e := scanUsageEnv{s: s, at: usageAt(6, 10, 0)}
	e.team = newTeam(t, s, "Payments")
	ownRepo(t, s, e.team, mr1.Instance, "acme/p7")
	e.reviewID = seedUsage(t, s, usageMR(7, 1), usageAt(5, 10, 0), TriggerPush, false, "m",
		skillRow("security", "no_blocking_issues", 100, 50))
	e.scan = e.queueFinished(t, "acme/p7", 7, SourceGlobal, "m1", 0)
	return e
}

// queueFinished queues a scan of project on the test instance, gives its
// carrier run the entry (a team entry has the team's ID), and ends it at e.at.
func (e scanUsageEnv) queueFinished(t *testing.T, project string, projectID int64, source, entry string, team int64) SecurityScan {
	t.Helper()
	q := scanRequest(mr1.Instance, project)
	q.ProjectID = projectID
	sc := queueSecurity(t, e.s, q, usageAt(6, 9, 0))
	if _, err := e.s.pool.Exec(ctx, `UPDATE runs SET model_source = $2, model_entry_id = $3, model_entry_name = $3,
			model_team_id = NULLIF($4::bigint, 0), chain_position = 1, chain_length = 1 WHERE id = $1::uuid`,
		sc.RunID, source, entry, team); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.pool.Exec(ctx, `UPDATE security_scans SET state = 'complete', finished_at = $2::timestamptz WHERE id = $1::uuid`,
		sc.ID, e.at); err != nil {
		t.Fatal(err)
	}
	return sc
}

func millionRow() ScanModelUsage {
	zero := int64(0)
	return ScanModelUsage{Provider: "p", Model: "m", Usage: SkillUsage{Skill: "security-scan", In: 600000, Out: 400000,
		CacheRead: &zero, CacheWrite: &zero, Reasoning: &zero}}
}

func (e scanUsageEnv) record(t *testing.T, sc SecurityScan, rows ...ScanModelUsage) {
	t.Helper()
	if err := e.s.RecordScanUsage(ctx, sc.ID, rows); err != nil {
		t.Fatalf("RecordScanUsage: %v", err)
	}
}

// A scan's tokens count in the repository's, the instance's and the entry's
// quotas and limits, with the scan's real instance and project.
func TestScanUsageCountsInQuotas(t *testing.T) {
	t.Parallel()
	e := newScanUsageEnv(t)
	e.record(t, e.scan, millionRow())
	since := usageSince
	// A scan on a team's own key, in another repository.
	other := e.queueFinished(t, "acme/p8", 8, SourceTeam, "t9", e.team)
	e.record(t, other, ScanModelUsage{Provider: "p", Model: "m", Usage: SkillUsage{Skill: "security-scan", In: 7}})

	t.Run("repository", func(t *testing.T) {
		if n, err := e.s.TokensSince(ctx, mr1.Instance, 7, since); err != nil || n != 1000150 {
			t.Errorf("TokensSince(7) = %d, %v; want 1000150 (the scan and the review)", n, err)
		}
	})
	t.Run("instance, global keys only", func(t *testing.T) {
		if n, err := e.s.InstanceGlobalTokensSince(ctx, mr1.Instance, since); err != nil || n != 1000150 {
			t.Errorf("InstanceGlobalTokensSince = %d, %v; want 1000150 (the team key's scan is not counted)", n, err)
		}
	})
	t.Run("entry and owner team", func(t *testing.T) {
		got, err := e.s.EntryTokensSince(ctx, since)
		if err != nil {
			t.Fatal(err)
		}
		var global, team int64
		for _, g := range got {
			switch {
			case g.Ref == (EntryRef{Source: SourceGlobal, ID: "m1"}) && g.OwnerTeam == e.team:
				global += g.Tokens
			case g.Ref == (EntryRef{Source: SourceTeam, ID: "t9"}):
				team += g.Tokens
			}
		}
		if global != 1000000 || team != 7 {
			t.Errorf("EntryTokensSince = %+v; want 1000000 on the global entry for the owner team and 7 on the team entry", got)
		}
	})
	t.Run("the scan row", func(t *testing.T) {
		var instance, path, trigger, verdict, source string
		var id int64
		var owner *int64
		var at time.Time
		if err := e.s.pool.QueryRow(ctx, `SELECT instance, project_id, project_path, trigger, verdict, source, owner_team_id, reported_at
			FROM run_usage WHERE run_id = $1::uuid`, e.scan.RunID).Scan(&instance, &id, &path, &trigger, &verdict, &source, &owner, &at); err != nil {
			t.Fatal(err)
		}
		if instance != mr1.Instance || id != 7 || path != "acme/p7" || trigger != "security_scan" || verdict != "security" ||
			source != SourceGlobal || owner == nil || *owner != e.team || !at.Equal(e.at) {
			t.Errorf("row = %q, %d, %q, %q, %q, %q, %v, %v", instance, id, path, trigger, verdict, source, owner, at)
		}
	})
}

// The scan shows in the token pages: the skill split, the days and the CSV rows.
func TestScanUsageShowsInTokenPages(t *testing.T) {
	t.Parallel()
	e := newScanUsageEnv(t)
	e.record(t, e.scan, millionRow())

	t.Run("by skill", func(t *testing.T) {
		got, err := e.s.UsageBySkill(ctx, mr1.Instance, 7, usageSince)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, u := range got {
			if u.Name == "security-scan" {
				found = u.Tokens.Total() == 1000000
			}
		}
		if !found {
			t.Errorf("UsageBySkill = %+v; want a security-scan row of 1000000", got)
		}
	})
	t.Run("by day", func(t *testing.T) {
		got, err := e.s.UsageDaily(ctx, usageSince, mr1.Instance, 7)
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, d := range got {
			total += d.Tokens.Total()
		}
		if total != 1000150 {
			t.Errorf("UsageDaily total = %d; want 1000150", total)
		}
	})
	t.Run("rows", func(t *testing.T) {
		var scan *UsageRow
		err := e.s.UsageRows(ctx, usageSince, mr1.Instance, 7, 100, func(u UsageRow) error {
			if u.Skill == "security-scan" {
				scan = &u
			}
			return nil
		})
		if err != nil || scan == nil || scan.In+scan.Out != 1000000 || scan.MR != 0 || scan.Trigger != "security_scan" || scan.Project != "acme/p7" {
			t.Errorf("UsageRows scan row = %+v, %v", scan, err)
		}
	})
}

// A scan's tokens are counted, but it is no review, no merge request, no skill
// run, no finding, no wasted run and no cost-per-review sample.
func TestScanUsageIsNoReview(t *testing.T) {
	t.Parallel()
	e := newScanUsageEnv(t)
	e.record(t, e.scan, millionRow())

	t.Run("by repository", func(t *testing.T) {
		got, err := e.s.UsageByRepository(ctx, usageSince)
		if err != nil || len(got) != 1 {
			t.Fatalf("UsageByRepository = %+v, %v", got, err)
		}
		if got[0].Reviews != 1 || got[0].Tokens.Total() != 1000150 || got[0].GlobalTokens != 1000150 {
			t.Errorf("repository = %+v; want 1 review (the merge request's) and 1000150 tokens", got[0])
		}
	})
	t.Run("merge requests", func(t *testing.T) {
		got, err := e.s.UsageByMR(ctx, mr1.Instance, 7, usageSince, 10)
		if err != nil || len(got) != 1 || got[0].IID != 1 || got[0].Tokens.Total() != 150 || got[0].Reviews != 1 {
			t.Errorf("UsageByMR = %+v, %v; want only merge request 1 with 150 tokens", got, err)
		}
		for _, m := range got {
			for _, md := range m.Models {
				if md.Model == "m" && md.Provider == "p" {
					t.Errorf("UsageByMR lists the scan's model: %+v", got)
				}
			}
		}
	})
	t.Run("cost per review", func(t *testing.T) {
		got, err := e.s.UsageCostPerReview(ctx, mr1.Instance, 7, usageSince)
		if err != nil || got.FullMedian == nil || *got.FullMedian != 150 || len(got.Outliers) != 0 {
			t.Errorf("UsageCostPerReview = %+v, %v; want a full median of 150 and no outlier", got, err)
		}
	})
	t.Run("wasted", func(t *testing.T) {
		if _, err := e.s.pool.Exec(ctx, `UPDATE run_usage SET verdict = 'failed' WHERE run_id = $1::uuid`, e.scan.RunID); err != nil {
			t.Fatal(err)
		}
		got, err := e.s.UsageWasted(ctx, mr1.Instance, 7, usageSince)
		if err != nil || got.Failed.Total() != 0 || got.Reruns.Total() != 0 || got.RerunCount != 0 {
			t.Errorf("UsageWasted = %+v, %v; want none", got, err)
		}
	})
	t.Run("team usage", func(t *testing.T) {
		got, err := e.s.TeamUsage(ctx, e.team, usageSince)
		if err != nil || got.Reviews != 1 || got.Tokens.Total() != 1000150 || len(got.Repos) != 1 ||
			got.Repos[0].Reviews != 1 || got.Repos[0].Tokens.Total() != 1000150 {
			t.Errorf("TeamUsage = %+v, %v; want 1 review (team and repository) and 1000150 tokens", got, err)
		}
	})
	t.Run("metrics", func(t *testing.T) {
		got, err := e.s.MetricsTotals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var scanTokens int64
		for _, u := range got.Tokens {
			if u.Skill == "security-scan" {
				scanTokens += u.Tokens.Total()
			}
		}
		if scanTokens != 1000000 {
			t.Errorf("metrics tokens of the scan = %d; want 1000000", scanTokens)
		}
		for _, r := range got.Reviews {
			if r.Trigger == "security_scan" {
				t.Errorf("metrics count the scan as a review: %+v", r)
			}
		}
		for _, r := range got.SkillRuns {
			if r.Skill == "security-scan" || r.Verdict == "security" {
				t.Errorf("metrics count the scan as a skill run: %+v", r)
			}
		}
		for _, f := range got.Findings {
			if f.Skill == "security-scan" {
				t.Errorf("metrics count the scan's findings: %+v", f)
			}
		}
	})
}

// A second call keeps the first rows: the Job's callback can arrive twice.
func TestRecordScanUsageOnce(t *testing.T) {
	t.Parallel()
	e := newScanUsageEnv(t)
	e.record(t, e.scan, millionRow())
	again := millionRow()
	again.Usage.In = 5
	e.record(t, e.scan, again)

	var n, in int64
	if err := e.s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(tokens_in), 0) FROM run_usage WHERE run_id = $1::uuid`,
		e.scan.RunID).Scan(&n, &in); err != nil {
		t.Fatal(err)
	}
	if n != 1 || in != 600000 {
		t.Errorf("rows = %d, tokens_in = %d; want the first row only (1, 600000)", n, in)
	}
}

// Two models are two rows, split by model, and the entry's limit counts both.
func TestScanUsagePerModel(t *testing.T) {
	t.Parallel()
	e := newScanUsageEnv(t)
	second := ScanModelUsage{Provider: "q", Model: "sub", Usage: SkillUsage{Skill: "security-scan@q/sub", In: 300, Out: 200}}
	e.record(t, e.scan, millionRow(), second)

	var n int64
	if err := e.s.pool.QueryRow(ctx, `SELECT count(*) FROM run_usage WHERE run_id = $1::uuid`, e.scan.RunID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d, %v; want 2", n, err)
	}
	t.Run("by model", func(t *testing.T) {
		got, err := e.s.UsageByModel(ctx, mr1.Instance, 7, usageSince)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]int64{}
		for _, u := range got {
			seen[u.Provider+"/"+u.Name] = u.Tokens.Total()
		}
		if seen["p/m"] != 1000000 || seen["q/sub"] != 500 {
			t.Errorf("UsageByModel = %+v; want p/m 1000000 and q/sub 500 (the review has no provider)", got)
		}
	})
	t.Run("entry limit", func(t *testing.T) {
		got, err := e.s.EntryTokensSince(ctx, usageSince)
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, g := range got {
			if g.Ref == (EntryRef{Source: SourceGlobal, ID: "m1"}) {
				total += g.Tokens
			}
		}
		if total != 1000500 {
			t.Errorf("EntryTokensSince = %+v; want 1000500 on the entry (both models)", got)
		}
	})
}
