package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func scanRequest(instance, project string) SecurityScanRequest {
	return SecurityScanRequest{Instance: instance, Project: project, ProjectID: 7, Branch: "main",
		Commit: "0123456789abcdef0123456789abcdef01234567", StartedBy: "alice", TimeoutSeconds: 3600}
}

func queueSecurity(t *testing.T, s *Store, q SecurityScanRequest, now time.Time) SecurityScan {
	t.Helper()
	sc, err := s.QueueSecurityScan(ctx, q, now)
	if err != nil {
		t.Fatalf("QueueSecurityScan(%s): %v", q.Project, err)
	}
	return sc
}

// A queued scan is number 1 of its repository and owns a hidden run of kind
// scan; a second one on the same repository, whatever the case of its path,
// is busy and gets the first back; another repository counts from 1.
func TestSecurityScanQueue(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	q := scanRequest("main", "Acme/API")
	q.MaxSteps, q.SubagentMaxSteps, q.PastQuota = ptrTo(200), ptrTo(50), true
	first := queueSecurity(t, s, q, t0)

	t.Run("first", func(t *testing.T) {
		if first.Number != 1 || first.State != SecurityQueued || first.Instance != "main" || first.Project != "Acme/API" ||
			first.ProjectID != 7 || first.Commit != q.Commit || first.StartedBy != "alice" || !first.PastQuota ||
			!first.QueuedAt.Equal(t0) || first.TimeoutSeconds != 3600 || first.MaxSteps == nil || *first.MaxSteps != 200 ||
			first.SubagentMaxSteps == nil || *first.SubagentMaxSteps != 50 || first.StartedAt != nil || first.HasSARIF {
			t.Errorf("first scan = %+v", first)
		}
		run := getRun(t, s, first.RunID)
		if run.Kind != KindScan || run.State != Superseded || run.MR.Instance != "" || run.MR.ProjectID != 0 ||
			run.ProjectPath != "" || run.HeadSHA != "" {
			t.Errorf("carrier run = kind %q, state %q, %+v, path %q, head %q; want a hidden superseded scan run",
				run.Kind, run.State, run.MR, run.ProjectPath, run.HeadSHA)
		}
		var recorded bool
		if err := s.pool.QueryRow(ctx, `SELECT usage_recorded FROM runs WHERE id = $1::uuid`, first.RunID).Scan(&recorded); err != nil || !recorded {
			t.Errorf("carrier usage_recorded = %v, %v; want true", recorded, err)
		}
		got, err := s.SecurityScan(ctx, first.ID)
		if err != nil || got.ID != first.ID || got.RunID != first.RunID || got.Number != 1 {
			t.Errorf("SecurityScan = %+v, %v", got, err)
		}
	})

	t.Run("busy", func(t *testing.T) {
		got, err := s.QueueSecurityScan(ctx, scanRequest("main", "acme/api"), t0.Add(time.Minute))
		if !errors.Is(err, ErrSecurityScanBusy) || got.ID != first.ID {
			t.Errorf("second queue = %s, %v; want %s and ErrSecurityScanBusy", got.ID, err, first.ID)
		}
		active, err := s.ActiveSecurityScan(ctx, "main", "ACME/api")
		if err != nil || active == nil || active.ID != first.ID {
			t.Errorf("ActiveSecurityScan = %+v, %v; want %s", active, err, first.ID)
		}
	})

	t.Run("another repository", func(t *testing.T) {
		other := queueSecurity(t, s, scanRequest("main", "acme/web"), t0.Add(2*time.Minute))
		if other.Number != 1 || other.RunID == first.RunID {
			t.Errorf("other repository's scan = number %d, run %s", other.Number, other.RunID)
		}
	})

	t.Run("next number and the list", func(t *testing.T) {
		if _, err := s.pool.Exec(ctx, `UPDATE security_scans SET state = 'complete' WHERE id = $1::uuid`, first.ID); err != nil {
			t.Fatal(err)
		}
		second := queueSecurity(t, s, scanRequest("main", "acme/api"), t0.Add(3*time.Minute))
		if second.Number != 2 {
			t.Errorf("second scan number = %d; want 2", second.Number)
		}
		list, err := s.SecurityScans(ctx, "main", "ACME/API", 10)
		if err != nil || len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
			t.Errorf("SecurityScans = %+v, %v; want the 2 scans, newest first", list, err)
		}
		if list, err := s.SecurityScans(ctx, "main", "acme/api", 1); err != nil || len(list) != 1 || list[0].ID != second.ID {
			t.Errorf("SecurityScans limit 1 = %+v, %v", list, err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		if _, err := s.SecurityScan(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrSecurityScanNotFound) {
			t.Errorf("SecurityScan of no scan = %v; want ErrSecurityScanNotFound", err)
		}
		if _, err := s.SecurityScan(ctx, "not-a-uuid"); !errors.Is(err, ErrSecurityScanNotFound) {
			t.Errorf("SecurityScan of a malformed id = %v; want ErrSecurityScanNotFound", err)
		}
		if active, err := s.ActiveSecurityScan(ctx, "main", "acme/none"); err != nil || active != nil {
			t.Errorf("ActiveSecurityScan of a repository with none = %+v, %v", active, err)
		}
	})
}

// With a limit of 1, one scan runs at a time: the oldest queued is claimed,
// and the next claim waits until it ends.
func TestSecurityScanClaimCap(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	var queued []SecurityScan
	for i, p := range []string{"acme/a", "acme/b", "acme/c"} {
		queued = append(queued, queueSecurity(t, s, scanRequest("main", p), t0.Add(time.Duration(i)*time.Minute)))
	}
	now, deadline := t0.Add(time.Hour), t0.Add(4*time.Hour)

	sc, ok, err := s.ClaimSecurityScan(ctx, 1, now, deadline)
	if err != nil || !ok || sc.ID != queued[0].ID || sc.State != SecurityRunning ||
		sc.StartedAt == nil || !sc.StartedAt.Equal(now) || sc.Deadline == nil || !sc.Deadline.Equal(deadline) {
		t.Fatalf("first claim = %+v, %v, %v; want the oldest, running", sc, ok, err)
	}
	if _, ok, err := s.ClaimSecurityScan(ctx, 1, now, deadline); err != nil || ok {
		t.Errorf("claim while one runs = %v, %v; want none", ok, err)
	}
	if _, ok, err := s.ClaimSecurityScan(ctx, 0, now, deadline); err != nil || ok {
		t.Errorf("claim with limit 0 = %v, %v; want none", ok, err)
	}
	running, err := s.RunningSecurityScans(ctx)
	if err != nil || len(running) != 1 || running[0].ID != queued[0].ID {
		t.Errorf("RunningSecurityScans = %+v, %v", running, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE security_scans SET state = 'complete', finished_at = $2 WHERE id = $1::uuid`,
		queued[0].ID, now); err != nil {
		t.Fatal(err)
	}
	sc, ok, err = s.ClaimSecurityScan(ctx, 1, now, deadline)
	if err != nil || !ok || sc.ID != queued[1].ID {
		t.Errorf("claim after the first ended = %+v, %v, %v; want %s", sc, ok, err, queued[1].ID)
	}
}

// StartSecurityJob stores the Job on the scan and the entry on its hidden
// run, as SetRunEntry writes it; the token check reads the stored hash.
func TestSecurityStartSetsCarrierEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	tm := newTeamModel(t, s, team, "Team key", nil)
	queued := queueSecurity(t, s, scanRequest("main", "acme/api"), t0)
	budget := int64(5000)
	start := SecurityJobStart{JobName: "pergent-security-1", TokenHash: hash("tok"), Deadline: t0.Add(3 * time.Hour),
		Entry:    RunEntry{Source: SourceTeam, ID: tm.ID, Name: "Team key", TeamID: team, Position: 1, Length: 2},
		Provider: "openai", Model: "gpt-5", TokenBudget: &budget}

	if err := s.StartSecurityJob(ctx, queued.ID, start); !errors.Is(err, ErrSecurityScanNotFound) {
		t.Errorf("StartSecurityJob on a queued scan = %v; want ErrSecurityScanNotFound", err)
	}
	if _, ok, err := s.ClaimSecurityScan(ctx, 1, t0, t0.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if err := s.StartSecurityJob(ctx, queued.ID, start); err != nil {
		t.Fatalf("StartSecurityJob: %v", err)
	}
	sc, err := s.SecurityScan(ctx, queued.ID)
	if err != nil || sc.JobName != "pergent-security-1" || sc.Provider != "openai" || sc.Model != "gpt-5" ||
		sc.EntryName != "Team key" || sc.TokenBudget == nil || *sc.TokenBudget != 5000 ||
		sc.Deadline == nil || !sc.Deadline.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("started scan = %+v, %v", sc, err)
	}
	run := getRun(t, s, queued.RunID)
	if run.ModelSource != SourceTeam || run.ModelEntryID != tm.ID || run.ModelEntryName != "Team key" ||
		run.ModelTeamID == nil || *run.ModelTeamID != team || run.ChainPosition != 1 || run.ChainLength != 2 ||
		run.Provider == nil || *run.Provider != "openai" || run.Model == nil || *run.Model != "gpt-5" {
		t.Errorf("carrier run entry = source %q, id %q, name %q, team %v, %d/%d, provider %v, model %v",
			run.ModelSource, run.ModelEntryID, run.ModelEntryName, run.ModelTeamID, run.ChainPosition, run.ChainLength,
			run.Provider, run.Model)
	}
	if run.State != Superseded {
		t.Errorf("carrier state = %q; want it still superseded", run.State)
	}
	for _, c := range []struct {
		name string
		id   string
		hash []byte
		want bool
	}{
		{"its token", queued.ID, hash("tok"), true},
		{"another token", queued.ID, hash("other"), false},
		{"no token", queued.ID, nil, false},
		{"malformed id", "nope", hash("tok"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, err := s.SecurityTokenIs(ctx, c.id, c.hash); err != nil || got != c.want {
				t.Errorf("SecurityTokenIs = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// A scan's hidden run is never a review: it is not listed, never claimed by
// the review loop, and never filled with usage. A merge request's run beside
// it is, so the checks are not empty by accident.
func TestSecurityCarrierInvisible(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mr := enqueue(t, s, mr1, "aaa")
	sc := queueSecurity(t, s, scanRequest(mr1.Instance, "acme/widgets"), t0)

	digests, err := s.ReviewDigests(ctx, ReviewFilter{Visible: Visibility{All: true}, Limit: 50})
	if err != nil || len(digests) != 1 || digests[0].ID != mr {
		t.Errorf("ReviewDigests = %+v, %v; want only the merge request's run %s", digests, err, mr)
	}
	claimed, err := s.ClaimQueued(ctx, 10, time.Now())
	if err != nil || len(claimed) != 1 || claimed[0].ID != mr {
		t.Errorf("ClaimQueued = %+v, %v; want only the merge request's run %s", claimed, err, mr)
	}
	// Even with a result stored, its usage is recorded, so the fill skips it.
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET result = '{}' WHERE id = $1::uuid`, sc.RunID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.RunsWithoutUsage(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range pending {
		if r.ID == sc.RunID {
			t.Errorf("RunsWithoutUsage lists the scan's run %s", sc.RunID)
		}
	}
	stats, err := s.RunStats(ctx, Visibility{All: true}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for k := range stats {
		if k.Instance == "" {
			t.Errorf("RunStats counts the scan's run: %+v", stats)
		}
	}
}

// The previous release runs on the migrated database: its runs still read,
// it still inserts merge request runs and API reviews, and the wider checks
// take a scan's run but refuse a run of no known kind or a scan run with a
// repository.
func TestSecurityScanMigrationPreviousRelease(t *testing.T) {
	s := newStoreAt(t, 54)
	var client int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO api_clients (name, callback_secret_sealed) VALUES ('ci', '\x00'::bytea)
		RETURNING id`).Scan(&client); err != nil {
		t.Fatal(err)
	}
	mrInsert := `INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, target_branch)
		VALUES ('main', 1, $1, 'acme/api', 'abc', 'main') RETURNING id::text`
	apiInsert := `INSERT INTO runs (instance, project_id, mr_iid, head_sha, base_sha, kind, api_client_id, quota_checked)
		VALUES ('', 0, nextval('api_review_numbers'), 'h', 'b', 'api', $1, true) RETURNING id::text`
	var oldMR, oldAPI string
	if err := s.pool.QueryRow(ctx, mrInsert, 1).Scan(&oldMR); err != nil {
		t.Fatalf("old merge request run: %v", err)
	}
	if err := s.pool.QueryRow(ctx, apiInsert, client).Scan(&oldAPI); err != nil {
		t.Fatalf("old API review: %v", err)
	}

	applyUpTo(t, s, 55)

	if r := getRun(t, s, oldMR); r.Kind != KindMR {
		t.Errorf("old merge request run kind = %q", r.Kind)
	}
	if r := getRun(t, s, oldAPI); r.Kind != KindAPI || r.APIClientID == nil || *r.APIClientID != client {
		t.Errorf("old API review = kind %q, client %v", r.Kind, r.APIClientID)
	}
	var id string
	if err := s.pool.QueryRow(ctx, mrInsert, 2).Scan(&id); err != nil {
		t.Errorf("previous release's merge request insert after 0055: %v", err)
	}
	if err := s.pool.QueryRow(ctx, apiInsert, client).Scan(&id); err != nil {
		t.Errorf("previous release's API review insert after 0055: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO runs (instance, project_id, mr_iid, head_sha, kind, state, usage_recorded)
		VALUES ('', 0, nextval('security_scan_numbers'), '', 'scan', 'superseded', true)`); err != nil {
		t.Errorf("a scan's run after 0055: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO runs (instance, project_id, mr_iid, head_sha, kind) VALUES ('', 0, 9001, '', 'other')`,
		`INSERT INTO runs (instance, project_id, mr_iid, head_sha, kind, state) VALUES ('main', 1, 9002, '', 'scan', 'superseded')`,
		`INSERT INTO runs (instance, project_id, mr_iid, head_sha, kind) VALUES ('', 0, 9003, '', 'api')`,
	} {
		if _, err := s.pool.Exec(ctx, q); err == nil {
			t.Errorf("%s was taken; want a check violation", q)
		}
	}
	if _, err := s.QueueSecurityScan(ctx, scanRequest("main", "acme/api"), t0); err != nil {
		t.Errorf("QueueSecurityScan after 0055: %v", err)
	}
}

// No row is the zero value; a put and a read round-trip, with the path in
// any case; a put replaces every setting; a value out of range is refused.
func TestSecurityScanSettings(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	read := func(t *testing.T, project string) SecurityScanSettings {
		t.Helper()
		v, err := s.SecurityScanSettingsOf(ctx, "main", project)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	same := func(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	equal := func(a, b SecurityScanSettings) bool {
		return same(a.MaxSteps, b.MaxSteps) && same(a.SubagentMaxSteps, b.SubagentMaxSteps) &&
			same(a.TimeoutSeconds, b.TimeoutSeconds) && a.FindingsInReviews == b.FindingsInReviews
	}

	t.Run("no row", func(t *testing.T) {
		if v := read(t, "acme/api"); !equal(v, SecurityScanSettings{}) {
			t.Errorf("settings with no row = %+v; want the zero value", v)
		}
	})
	set := SecurityScanSettings{MaxSteps: ptrTo(200), SubagentMaxSteps: ptrTo(50), TimeoutSeconds: ptrTo(7200), FindingsInReviews: true}
	t.Run("round trip", func(t *testing.T) {
		if err := s.PutSecurityScanSettings(ctx, "main", "Acme/API", set, t0); err != nil {
			t.Fatal(err)
		}
		if v := read(t, "acme/api"); !equal(v, set) {
			t.Errorf("settings = %+v; want %+v", v, set)
		}
		if v := read(t, "acme/web"); !equal(v, SecurityScanSettings{}) {
			t.Errorf("another repository's settings = %+v; want the zero value", v)
		}
	})
	t.Run("replace", func(t *testing.T) {
		back := SecurityScanSettings{TimeoutSeconds: ptrTo(600)}
		if err := s.PutSecurityScanSettings(ctx, "main", "acme/api", back, t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if v := read(t, "ACME/api"); !equal(v, back) {
			t.Errorf("settings = %+v; want %+v", v, back)
		}
	})
	t.Run("out of range", func(t *testing.T) {
		for _, v := range []SecurityScanSettings{{MaxSteps: ptrTo(4)}, {SubagentMaxSteps: ptrTo(301)}, {TimeoutSeconds: ptrTo(21601)}} {
			if err := s.PutSecurityScanSettings(ctx, "main", "acme/api", v, t0); err == nil {
				t.Errorf("PutSecurityScanSettings(%+v) was taken", v)
			}
		}
	})
}

// A repository's scan chain is saved and read by the chain code, leaves the
// repository's own model setting alone, loses a removed team entry, and goes
// with a reset, a removed team or a removed repository.
func TestScanChainSave(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	tm := newTeamModel(t, s, team, "Team key", nil)
	G, T := EntryRef{SourceGlobal, g.ID}, EntryRef{SourceTeam, tm.ID}
	addRepos(t, s, "acme/api")
	save := func(t *testing.T, project string, entries ...EntryRef) {
		t.Helper()
		if err := s.SaveChainOf(ctx, ChainScan, Chain{Scope: ScopeRepo, Instance: "main", Project: project,
			Entries: entries, SavedByAdmin: true}, t0); err != nil {
			t.Fatalf("SaveChainOf(ChainScan): %v", err)
		}
	}
	scanChain := func(t *testing.T, project string) (Chain, bool) {
		t.Helper()
		c, found, err := s.ChainOf(ctx, ChainScan, ScopeRepo, 0, "main", project)
		if err != nil {
			t.Fatal(err)
		}
		return c, found
	}

	t.Run("save and read", func(t *testing.T) {
		save(t, "Acme/API", G, T)
		c, found := scanChain(t, "acme/api")
		if !found || !slices.Equal(c.Entries, []EntryRef{G, T}) || !c.SavedByAdmin || c.Scope != ScopeRepo {
			t.Errorf("scan chain = %+v, %v; want [G T], saved by an admin", c, found)
		}
		r := mustRepo(t, s, "acme/api")
		if r.ChainMode != "inherit" || r.ModelID != nil {
			t.Errorf("repository = chain mode %q, model %v; want it untouched", r.ChainMode, r.ModelID)
		}
		if _, found := mustChain(t, s, ScopeRepo, 0, "main", "acme/api"); found {
			t.Error("saving the scan chain wrote a review chain")
		}
	})
	t.Run("a removed team entry leaves it", func(t *testing.T) {
		if _, _, err := s.DeleteTeamModel(ctx, team, tm.ID); err != nil {
			t.Fatal(err)
		}
		if c, _ := scanChain(t, "acme/api"); !slices.Equal(c.Entries, []EntryRef{G}) {
			t.Errorf("scan chain = %+v; want [G]", c.Entries)
		}
	})
	t.Run("a removed team leaves it", func(t *testing.T) {
		other := newTeam(t, s, "Web")
		om := newTeamModel(t, s, other, "Web key", nil)
		save(t, "acme/api", EntryRef{SourceTeam, om.ID}, G)
		if ok, err := s.DeleteTeam(ctx, other, nil); err != nil || !ok {
			t.Fatalf("DeleteTeam = %v, %v", ok, err)
		}
		if c, _ := scanChain(t, "acme/api"); !slices.Equal(c.Entries, []EntryRef{G}) {
			t.Errorf("scan chain = %+v; want [G]", c.Entries)
		}
	})
	t.Run("reset", func(t *testing.T) {
		if err := s.ResetScanChain(ctx, "main", "ACME/api"); err != nil {
			t.Fatal(err)
		}
		if _, found := scanChain(t, "acme/api"); found {
			t.Error("the scan chain is still there after a reset")
		}
	})
	t.Run("a removed repository drops it", func(t *testing.T) {
		save(t, "acme/api", G)
		if found, err := s.RemoveDashboardRepo(ctx, "main", "acme/api"); err != nil || !found {
			t.Fatalf("RemoveDashboardRepo = %v, %v", found, err)
		}
		if _, found := scanChain(t, "acme/api"); found {
			t.Error("the removed repository's scan chain is still there")
		}
	})
}

// A cancel of a running scan records when it was asked for, once; a queued
// scan ends at once, partial and cancelled; a finished one is left as it is.
func TestSecurityCancel(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	running := queueSecurity(t, s, scanRequest("main", "acme/a"), t0)
	if _, ok, err := s.ClaimSecurityScan(ctx, 1, t0, t0.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	queued := queueSecurity(t, s, scanRequest("main", "acme/b"), t0.Add(time.Minute))
	at := t0.Add(10 * time.Minute)

	t.Run("running", func(t *testing.T) {
		sc, ok, err := s.RequestSecurityCancel(ctx, running.ID, at)
		if err != nil || !ok || sc.State != SecurityRunning || sc.CancelRequestedAt == nil || !sc.CancelRequestedAt.Equal(at) {
			t.Errorf("cancel of a running scan = %+v, %v, %v", sc, ok, err)
		}
		sc, ok, err = s.RequestSecurityCancel(ctx, running.ID, at.Add(time.Minute))
		if err != nil || !ok || sc.CancelRequestedAt == nil || !sc.CancelRequestedAt.Equal(at) {
			t.Errorf("second cancel = %+v, %v, %v; want the first time kept", sc, ok, err)
		}
	})
	t.Run("queued", func(t *testing.T) {
		sc, ok, err := s.RequestSecurityCancel(ctx, queued.ID, at)
		if err != nil || !ok || sc.State != SecurityPartial || sc.Reason != "cancelled" ||
			sc.FinishedAt == nil || !sc.FinishedAt.Equal(at) {
			t.Errorf("cancel of a queued scan = %+v, %v, %v; want partial, cancelled", sc, ok, err)
		}
		if active, err := s.ActiveSecurityScan(ctx, "main", "acme/b"); err != nil || active != nil {
			t.Errorf("active scan after the cancel = %+v, %v", active, err)
		}
	})
	t.Run("finished", func(t *testing.T) {
		sc, ok, err := s.RequestSecurityCancel(ctx, queued.ID, at.Add(time.Hour))
		if err != nil || ok || sc.State != SecurityPartial || sc.FinishedAt == nil || !sc.FinishedAt.Equal(at) {
			t.Errorf("cancel of a finished scan = %+v, %v, %v; want it unchanged and false", sc, ok, err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, _, err := s.RequestSecurityCancel(ctx, "00000000-0000-0000-0000-000000000000", at); !errors.Is(err, ErrSecurityScanNotFound) {
			t.Errorf("cancel of no scan = %v; want ErrSecurityScanNotFound", err)
		}
	})
}

// SecurityScansToLog lists the scans that ended since a time with a Job and
// no stored log on their hidden run: not a running scan, not one with a log,
// not one that ended before, and not one cancelled before its Job.
func TestSecurityScansToLog(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	since := t0.Add(time.Hour)
	// ended runs a scan of project with a Job and ends it at end.
	ended := func(project string, end time.Time) SecurityScan {
		t.Helper()
		r := newSecRepo(s, project)
		sc := r.start(t)
		if _, err := s.pool.Exec(ctx, `UPDATE security_scans SET job_name = $2 WHERE id = $1::uuid`,
			sc.ID, "pergent-security-"+project[len("acme/"):]); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.FinishSecurityScan(ctx, sc.ID, r.token(), secResult(SecurityComplete, "v1"), end); err != nil || !ok {
			t.Fatalf("FinishSecurityScan = %v, %v", ok, err)
		}
		got, err := s.SecurityScan(ctx, sc.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := ended("acme/want", since.Add(time.Minute))
	logged := ended("acme/logged", since.Add(time.Minute))
	if err := s.PutRunLog(ctx, logged.RunID, "a log"); err != nil {
		t.Fatal(err)
	}
	ended("acme/old", since.Add(-time.Minute))
	running := newSecRepo(s, "acme/running").start(t)
	if _, err := s.pool.Exec(ctx, `UPDATE security_scans SET job_name = 'pergent-security-running' WHERE id = $1::uuid`,
		running.ID); err != nil {
		t.Fatal(err)
	}
	noJob := queueSecurity(t, s, scanRequest("main", "acme/nojob"), t0)
	if _, ok, err := s.RequestSecurityCancel(ctx, noJob.ID, since.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("cancel = %v, %v", ok, err)
	}

	got, err := s.SecurityScansToLog(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != want.ID || got[0].RunID != want.RunID {
		t.Fatalf("SecurityScansToLog = %+v; want only %s", got, want.ID)
	}
}
