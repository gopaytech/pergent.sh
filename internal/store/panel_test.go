package store

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// 0049 only adds: nothing of 0048 changes, and runs_live_idx is untouched.
func TestPanelMembersMigrationOnlyAdds(t *testing.T) {
	b, err := os.ReadFile("migrations/0049_panel_members.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sql.WriteString(line + "\n")
		}
	}
	// A foreign key's ON DELETE is not a delete.
	text := regexp.MustCompile(`(?i)ON\s+DELETE\s+CASCADE`).ReplaceAllString(sql.String(), "")
	for _, bad := range []string{`(?i)\bDROP\b`, `(?i)\bRENAME\b`, `(?i)\bTRUNCATE\b`, `(?i)\bDELETE\b`,
		`(?i)\bUPDATE\b`, `(?i)\bALTER\b`} {
		if regexp.MustCompile(bad).MatchString(text) {
			t.Errorf("migration 0049 matches %s; it may only add", bad)
		}
	}

	s := newStoreAt(t, 48)
	before, beforeIdx := schemaColumns(t, s), schemaIndexes(t, s)
	if _, ok := beforeIdx["runs_live_idx"]; !ok {
		t.Fatal("runs_live_idx is missing at 0048")
	}
	applyUpTo(t, s, 49)
	after, afterIdx := schemaColumns(t, s), schemaIndexes(t, s)
	for col, c := range before {
		if got, ok := after[col]; !ok || got != c {
			t.Errorf("column %s after 0049 = %+v (present %v); want %+v", col, got, ok, c)
		}
	}
	for name, def := range beforeIdx {
		if got, ok := afterIdx[name]; !ok || got != def {
			t.Errorf("index %s after 0049 = %q (present %v); want %q", name, got, ok, def)
		}
	}
	for _, col := range []string{"panel_members.run_id", "panel_members.state", "panel_member_logs.member_id",
		"panel_member_diagnostics.member_id", "panel_judges.run_id", "run_usage_models.member_id"} {
		if _, ok := after[col]; !ok {
			t.Errorf("column %s is missing after 0049", col)
		}
	}
}

func insertPanelRun(t *testing.T, s *Store, mr int, sha string) string {
	t.Helper()
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO runs (instance, project_id, mr_iid, head_sha)
		VALUES ('main', 1, $1, $2) RETURNING id`, mr, sha).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertMember(t *testing.T, s *Store, run, role, state string) (string, error) {
	t.Helper()
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO panel_members (run_id, role, state) VALUES ($1, $2, $3) RETURNING id`,
		run, role, state).Scan(&id)
	return id, err
}

// The previous release's inserts still work after 0049, and a run's delete
// takes its members, judges and usage rows with it.
func TestPanelMembersMigrationPreviousRelease(t *testing.T) {
	s := newStoreAt(t, 48)
	applyUpTo(t, s, 49)
	run := insertPanelRun(t, s, 1, "abc")
	if _, err := s.pool.Exec(ctx, `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid,
		trigger, incremental, model, verdict, reported_at, tokens_in, tokens_out, findings, blocking)
		VALUES ($1, 'review', 'main', 1, 'acme/api', 1, 'push', false, 'm', 'pass', $2, 10, 5, 0, 0)`, run, t0); err != nil {
		t.Fatalf("old run_usage insert: %v", err)
	}
	a, err := insertMember(t, s, run, "a", "reported")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO panel_member_logs (member_id, text) VALUES ($1, 'log')`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO panel_member_diagnostics (member_id, clone_log, clone_log_masked, pod_state)
		VALUES ($1, 'c', 'c', 'p')`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO panel_judges (run_id, role, provider, model, model_source, model_entry_id,
		model_entry_name, chain_length) VALUES ($1, 'a', 'anthropic', 'sonnet', 'global', '1', 'n', 1)`, run); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO run_usage_models (run_id, member_id, part, role, skill, provider, model, reported_at, tokens_in, tokens_out)
		VALUES ($1, $2, $3, 'a', 'review', 'anthropic', 'sonnet', $4, 1, 1)`
	if _, err := s.pool.Exec(ctx, ins, run, a, "member", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "merge", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"panel_members", "panel_member_logs", "panel_member_diagnostics", "panel_judges", "run_usage_models", "run_usage"} {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows after its run was deleted; want 0", table, n)
		}
	}
}

func TestPanelMembersOneLivePerRole(t *testing.T) {
	s := newStoreAt(t, 49)
	run := insertPanelRun(t, s, 1, "abc")
	if _, err := insertMember(t, s, run, "a", "queued"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"queued", "running", "reported"} {
		if _, err := insertMember(t, s, run, "a", state); sqlState(err) != "23505" {
			t.Errorf("second live %s row for (run, a): %v; want 23505", state, err)
		}
	}
	if _, err := insertMember(t, s, run, "b", "queued"); err != nil {
		t.Errorf("queued row of the other role: %v", err)
	}
	if _, err := insertMember(t, s, run, "a", "fell_back"); err != nil {
		t.Errorf("fell_back row beside a queued row: %v", err)
	}
	if _, err := insertMember(t, s, run, "a", "failed"); err != nil {
		t.Errorf("failed row beside a queued row: %v", err)
	}
	if _, err := insertMember(t, s, run, "a", "bogus"); sqlState(err) != "23514" {
		t.Errorf("bad state: %v; want 23514", err)
	}
	if _, err := insertMember(t, s, run, "c", "queued"); sqlState(err) != "23514" {
		t.Errorf("bad role: %v; want 23514", err)
	}

	other := insertPanelRun(t, s, 2, "def")
	if _, err := insertMember(t, s, other, "a", "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertMember(t, s, other, "b", "off"); sqlState(err) != "23505" {
		t.Errorf("second off row: %v; want 23505", err)
	}
}

func TestRunUsageModelsKeys(t *testing.T) {
	s := newStoreAt(t, 49)
	run := insertPanelRun(t, s, 1, "abc")
	m, err := insertMember(t, s, run, "a", "queued")
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO run_usage_models (run_id, member_id, part, role, skill, provider, model, reported_at, tokens_in, tokens_out)
		VALUES ($1, $2, $3, $4, $5, 'anthropic', 'sonnet', $6, 1, 1)`
	if _, err := s.pool.Exec(ctx, ins, run, nil, "member", "a", "review", t0); sqlState(err) != "23514" {
		t.Errorf("member row with no member_id: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "merge", "a", "review", t0); sqlState(err) != "23514" {
		t.Errorf("merge row with a member_id: %v; want 23514", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "member", "a", "review", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "member", "a", "review", t0); sqlState(err) != "23505" {
		t.Errorf("second member row per member and skill: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, m, "member", "a", "other", t0); err != nil {
		t.Errorf("member row of another skill: %v", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "merge", "a", "review", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "merge", "a", "review", t0); sqlState(err) != "23505" {
		t.Errorf("second merge row per run, skill and role: %v; want 23505", err)
	}
	if _, err := s.pool.Exec(ctx, ins, run, nil, "merge", "b", "review", t0); err != nil {
		t.Errorf("merge row of the other role: %v", err)
	}
}

// panelRun gives a claimed run of head on mr made a panel review: it waits
// for its 2 queued members.
func panelRun(t *testing.T, s *Store, mr MRKey, head string) string {
	t.Helper()
	id := enqueue(t, s, mr, head)
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	started, err := s.StartPanel(ctx, id, t0)
	if err != nil || !started {
		t.Fatalf("StartPanel = %v, %v", started, err)
	}
	return id
}

// memberOf returns the newest member of role of the review whose first run is root.
func memberOf(t *testing.T, s *Store, root, role string) PanelMember {
	t.Helper()
	ms, err := s.PanelMembers(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	var out PanelMember
	for _, m := range ms {
		if m.Role == role && m.State != MemberOff {
			out = m
		}
	}
	if out.ID == "" {
		t.Fatalf("no member %s of %s in %+v", role, root, ms)
	}
	return out
}

// dispatchMember claims the queued members and records a Job for member id
// with the token, valid for an hour after t0.
func dispatchMember(t *testing.T, s *Store, id, token string) {
	t.Helper()
	if _, err := s.ClaimQueuedMembers(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkMemberDispatched(ctx, id, "job-"+token, hash(token), t0.Add(time.Hour), t0); err != nil {
		t.Fatalf("MarkMemberDispatched: %v", err)
	}
}

// doneMember takes member id through dispatch to a stored result and settles it.
func doneMember(t *testing.T, s *Store, id, body string) {
	t.Helper()
	dispatchMember(t, s, id, "tok-"+id)
	if out, err := s.StoreMemberResult(ctx, id, "tok-"+id, []byte(body), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreMemberResult = %v, %v", out, err)
	}
	if st, err := s.SettleMember(ctx, id); err != nil || st != MemberDone {
		t.Fatalf("SettleMember = %q, %v; want done", st, err)
	}
}

// failedMember takes member id through dispatch to a failure and settles it.
func failedMember(t *testing.T, s *Store, id, reason string) {
	t.Helper()
	dispatchMember(t, s, id, "tok-"+id)
	if ok, err := s.ReportMemberFailure(ctx, id, Failure{Reason: reason, Step: "review"}, t0.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("ReportMemberFailure = %v, %v", ok, err)
	}
	if st, err := s.SettleMember(ctx, id); err != nil || st != MemberFailed {
		t.Fatalf("SettleMember = %q, %v; want failed", st, err)
	}
}

func memberStates(t *testing.T, s *Store, root string) map[string][]MemberState {
	t.Helper()
	ms, err := s.PanelMembers(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]MemberState{}
	for _, m := range ms {
		out[m.Role] = append(out[m.Role], m.State)
	}
	return out
}

func TestStartPanelWaitsAndQueuesMembers(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	r := getRun(t, s, id)
	if r.State != Waiting || r.SkipReason != WaitPanel || !r.DispatchedAt.Equal(t0) || r.JobName != "" {
		t.Errorf("run = %s %q dispatched %v job %q; want waiting for its members, dispatched at t0", r.State, r.SkipReason, r.DispatchedAt, r.JobName)
	}
	ms, err := s.PanelMembers(ctx, id)
	if err != nil || len(ms) != 2 {
		t.Fatalf("PanelMembers = %+v, %v; want 2", ms, err)
	}
	roles := map[string]bool{}
	for _, m := range ms {
		roles[m.Role] = true
		if m.RunID != id || m.State != MemberQueued || m.AttemptOf != nil || m.JobName != "" {
			t.Errorf("member = %+v; want queued, of the run, a first attempt", m)
		}
	}
	if !roles["a"] || !roles["b"] {
		t.Errorf("roles = %v; want a and b", roles)
	}
	// A second start finds the run waiting, not claimed.
	if started, err := s.StartPanel(ctx, id, t0); err != nil || started {
		t.Errorf("second StartPanel = %v, %v; want false", started, err)
	}
	// The previous release never claims a waiting run.
	if got, err := s.ClaimQueued(ctx, 10, t0.Add(time.Hour)); err != nil || len(got) != 0 {
		t.Errorf("ClaimQueued = %v, %v; want nothing", runIDs(got), err)
	}
}

func TestStartPanelSupersededRun(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, mr1, "a2")
	if started, err := s.StartPanel(ctx, id, t0); err != nil || started {
		t.Errorf("StartPanel of a superseded run = %v, %v; want false", started, err)
	}
	if ms, err := s.PanelMembers(ctx, id); err != nil || len(ms) != 0 {
		t.Errorf("PanelMembers = %+v, %v; want none", ms, err)
	}
	// A run with a Job is not split either.
	d := dispatched(t, s, mr2, "b1", "tok")
	if started, err := s.StartPanel(ctx, d, t0); err != nil || started {
		t.Errorf("StartPanel of a dispatched run = %v, %v; want false", started, err)
	}
	if started, err := s.StartPanel(ctx, "not-a-uuid", t0); err != nil || started {
		t.Errorf("StartPanel of a bad id = %v, %v; want false", started, err)
	}
}

func TestClaimQueuedMembersSkipsUnconnected(t *testing.T) {
	s := newStore(t)
	other := MRKey{Instance: "https://other.example.com", ProjectID: 1, IID: 1}
	id := panelRun(t, s, other, "a1")
	got, err := s.ClaimQueuedMembers(ctx, 10, t0, other.Instance)
	if err != nil || len(got) != 0 {
		t.Fatalf("ClaimQueuedMembers skipping the instance = %+v, %v; want none", got, err)
	}
	// Not due yet.
	a := memberOf(t, s, id, "a")
	if err := s.PostponeMember(ctx, a.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err = s.ClaimQueuedMembers(ctx, 10, t0)
	if err != nil || len(got) != 1 || got[0].Role != "b" || got[0].State != MemberRunning {
		t.Fatalf("ClaimQueuedMembers = %+v, %v; want b only, running", got, err)
	}
	got, err = s.ClaimQueuedMembers(ctx, 10, t0.Add(time.Hour))
	if err != nil || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("ClaimQueuedMembers later = %+v, %v; want a", got, err)
	}
	if got, err := s.ClaimQueuedMembers(ctx, 10, t0.Add(time.Hour)); err != nil || len(got) != 0 {
		t.Errorf("third claim = %+v, %v; want none", got, err)
	}
}

// A member whose run no longer waits is never claimed, and 2 dispatchers
// never claim the same member.
func TestClaimQueuedMembersOnce(t *testing.T) {
	s := newStore(t)
	gone := panelRun(t, s, mr1, "a1")
	enqueue(t, s, mr1, "a2") // supersedes the waiting run
	for i := 0; i < 5; i++ {
		panelRun(t, s, MRKey{Instance: mr2.Instance, ProjectID: 9, IID: int64(i + 1)}, "h")
	}
	var wg sync.WaitGroup
	got := make([][]PanelMember, 4)
	errs := make([]error, 4)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = s.ClaimQueuedMembers(ctx, 4, t0)
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := range got {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		for _, m := range got[i] {
			if seen[m.ID] {
				t.Errorf("member %s claimed twice", m.ID)
			}
			if m.RunID == gone {
				t.Errorf("member of a superseded run claimed")
			}
			seen[m.ID] = true
		}
	}
	if len(seen) != 10 {
		t.Errorf("claimed %d members; want 10", len(seen))
	}
}

// A claim takes at most limit members, even with more due: one panel's 2
// queued members and limit 1 give one.
func TestClaimQueuedMembersLimit(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	got, err := s.ClaimQueuedMembers(ctx, 1, t0)
	if err != nil || len(got) != 1 {
		t.Fatalf("ClaimQueuedMembers(1) = %d members, %v; want 1", len(got), err)
	}
	if st := memberStates(t, s, id); len(st["a"])+len(st["b"]) != 2 ||
		(st["a"][0] == MemberRunning) == (st["b"][0] == MemberRunning) {
		t.Errorf("states = %v; want one running and one queued", st)
	}
	if n, err := s.CountRunning(ctx); err != nil || n != 1 {
		t.Errorf("CountRunning = %d, %v; want 1", n, err)
	}
	got, err = s.ClaimQueuedMembers(ctx, 1, t0)
	if err != nil || len(got) != 1 {
		t.Fatalf("second ClaimQueuedMembers(1) = %d members, %v; want the other one", len(got), err)
	}
}

func TestCountRunningCountsMembers(t *testing.T) {
	s := newStore(t)
	dispatched(t, s, mr2, "b1", "tok")
	id := panelRun(t, s, mr1, "a1")
	if n, err := s.CountRunning(ctx); err != nil || n != 1 {
		t.Fatalf("CountRunning = %d, %v; want 1 (the waiting run is not running)", n, err)
	}
	dispatchMember(t, s, memberOf(t, s, id, "a").ID, "tok-a")
	if n, err := s.CountRunning(ctx); err != nil || n != 3 {
		t.Errorf("CountRunning = %d, %v; want 3 (a run and 2 members)", n, err)
	}
}

func TestMarkMemberDispatchedFirst(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	if _, err := s.MarkMemberDispatched(ctx, a.ID, "job-a", hash("ta"), t0.Add(time.Hour), t0); !errors.Is(err, ErrNotRunning) {
		t.Errorf("MarkMemberDispatched of a queued member = %v; want ErrNotRunning", err)
	}
	if _, err := s.ClaimQueuedMembers(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	first, err := s.MarkMemberDispatched(ctx, a.ID, "job-a", hash("ta"), t0.Add(time.Hour), t0.Add(time.Second))
	if err != nil || !first {
		t.Errorf("first MarkMemberDispatched = %v, %v; want true", first, err)
	}
	first, err = s.MarkMemberDispatched(ctx, b.ID, "job-b", hash("tb"), t0.Add(time.Hour), t0.Add(time.Second))
	if err != nil || first {
		t.Errorf("second MarkMemberDispatched = %v, %v; want false", first, err)
	}
	got := memberOf(t, s, id, "a")
	if got.JobName != "job-a" || !got.TokenExpiresAt.Equal(t0.Add(time.Hour)) || !got.DispatchedAt.Equal(t0.Add(time.Second)) {
		t.Errorf("member a = %+v; want its Job recorded", got)
	}
	var stored []byte
	if err := s.pool.QueryRow(ctx, `SELECT token_hash FROM panel_members WHERE id = $1::uuid`, a.ID).Scan(&stored); err != nil ||
		!bytes.Equal(stored, hash("ta")) {
		t.Errorf("token_hash = %x, %v; want the hash only", stored, err)
	}
	// Its run gone: refused.
	other := panelRun(t, s, mr2, "b1")
	oa := memberOf(t, s, other, "a")
	if _, err := s.ClaimQueuedMembers(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, mr2, "b2")
	if _, err := s.MarkMemberDispatched(ctx, oa.ID, "job", hash("t"), t0.Add(time.Hour), t0); !errors.Is(err, ErrNotRunning) {
		t.Errorf("MarkMemberDispatched with the run gone = %v; want ErrNotRunning", err)
	}
	if _, err := s.MarkMemberDispatched(ctx, "bad", "job", hash("t"), t0.Add(time.Hour), t0); !errors.Is(err, ErrNotRunning) {
		t.Errorf("MarkMemberDispatched of a bad id = %v; want ErrNotRunning", err)
	}
}

func TestMemberResultToken(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	dispatchMember(t, s, a.ID, "tok-a")
	if _, err := s.MarkMemberDispatched(ctx, b.ID, "job-b", hash("tok-b"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, id, token string
		at              time.Time
	}{
		{"wrong token", a.ID, "nope", t0},
		{"other member's token", a.ID, "tok-b", t0},
		{"no token", a.ID, "", t0},
		{"expired", a.ID, "tok-a", t0.Add(2 * time.Hour)},
		{"unknown member", "00000000-0000-0000-0000-000000000000", "tok-a", t0},
		{"bad id", "x", "tok-a", t0},
	} {
		if out, err := s.StoreMemberResult(ctx, c.id, c.token, []byte(`{}`), c.at); err != nil || out != ResultUnauthorized {
			t.Errorf("%s: StoreMemberResult = %v, %v; want unauthorized", c.name, out, err)
		}
	}
	if out, err := s.StoreMemberResult(ctx, a.ID, "tok-a", []byte(`not json`), t0); !errors.Is(err, ErrInvalidResult) {
		t.Errorf("bad body = %v, %v; want ErrInvalidResult", out, err)
	}
	if out, err := s.StoreMemberResult(ctx, a.ID, "tok-a", []byte(`{"x":1}`), t0.Add(time.Minute)); err != nil || out != ResultStored {
		t.Fatalf("StoreMemberResult = %v, %v; want stored", out, err)
	}
	got := memberOf(t, s, id, "a")
	if got.State != MemberReported || string(got.Result) != `{"x": 1}` || !got.ReportedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("member = %s %s %v; want reported with the result", got.State, got.Result, got.ReportedAt)
	}
	if out, err := s.StoreMemberResult(ctx, a.ID, "tok-a", []byte(`{}`), t0.Add(2*time.Minute)); err != nil || out != ResultConflict {
		t.Errorf("second post = %v, %v; want conflict", out, err)
	}
	// A failed member takes no result either.
	if ok, err := s.ReportMemberFailure(ctx, b.ID, Failure{Reason: "boom"}, t0); err != nil || !ok {
		t.Fatalf("ReportMemberFailure = %v, %v", ok, err)
	}
	if out, err := s.StoreMemberResult(ctx, b.ID, "tok-b", []byte(`{}`), t0); err != nil || out != ResultConflict {
		t.Errorf("post after a failure = %v, %v; want conflict", out, err)
	}
	if ok, err := s.ReportMemberFailure(ctx, a.ID, Failure{Reason: "late"}, t0); err != nil || ok {
		t.Errorf("ReportMemberFailure on a reported member = %v, %v; want false", ok, err)
	}
	// The run is untouched by its members.
	if r := getRun(t, s, id); r.State != Waiting || r.Result != nil || r.FailureReason != "" {
		t.Errorf("run = %s %s %q; want still waiting", r.State, r.Result, r.FailureReason)
	}
}

func TestMemberResultDiscardedAfterSupersede(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	dispatchMember(t, s, a.ID, "tok-a")
	if _, err := s.MarkMemberDispatched(ctx, b.ID, "job-b", hash("tok-b"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, mr1, "a2")
	// The sweep has not run yet: the member is still running, its run gone.
	if out, err := s.StoreMemberResult(ctx, a.ID, "tok-a", []byte(`{}`), t0); err != nil || out != ResultDiscarded {
		t.Errorf("StoreMemberResult with the run gone = %v, %v; want discarded", out, err)
	}
	if n, err := s.SupersedeOrphanMembers(ctx); err != nil || n != 2 {
		t.Fatalf("SupersedeOrphanMembers = %d, %v; want 2", n, err)
	}
	if out, err := s.StoreMemberResult(ctx, b.ID, "tok-b", []byte(`{}`), t0); err != nil || out != ResultDiscarded {
		t.Errorf("StoreMemberResult of a superseded member = %v, %v; want discarded", out, err)
	}
	if got := memberOf(t, s, id, "a"); got.Result != nil || got.State != MemberSuperseded {
		t.Errorf("member a = %s %s; want superseded, no result", got.State, got.Result)
	}
}

func TestEnqueueMemberAttemptPointsAtFirst(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	b := memberOf(t, s, id, "b")
	// Only a reported member falls back.
	if _, created, err := s.EnqueueMemberAttempt(ctx, b, quotaMark("m1")); err != nil || created {
		t.Errorf("EnqueueMemberAttempt of a queued member = %v, %v; want nothing", created, err)
	}
	if marks, _ := s.Marks(ctx); len(marks) != 0 {
		t.Errorf("marks = %+v; want none", marks)
	}
	dispatchMember(t, s, b.ID, "tok-b")
	if err := s.SetMemberEntry(ctx, b.ID, RunEntry{Source: SourceGlobal, ID: "m1", Name: "Main", Position: 1, Length: 3}, "anthropic", "sonnet"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReportMemberFailure(ctx, b.ID, Failure{Reason: "quota"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b = memberOf(t, s, id, "b")
	if b.Entry.ID != "m1" || b.Entry.Name != "Main" || b.Entry.Length != 3 || b.Provider != "anthropic" || b.Model != "sonnet" {
		t.Errorf("member b = %+v; want its entry", b)
	}
	if err := s.SetMemberEntry(ctx, b.ID, RunEntry{Source: SourceGlobal, ID: "m9"}, "x", "y"); err == nil {
		t.Error("SetMemberEntry on a reported member succeeded")
	}
	next, created, err := s.EnqueueMemberAttempt(ctx, b, quotaMark("m1"))
	if err != nil || !created || next == "" || next == b.ID {
		t.Fatalf("EnqueueMemberAttempt = %q, %v, %v", next, created, err)
	}
	if marks, _ := s.Marks(ctx); len(marks) != 1 || marks[0].Ref.ID != "m1" {
		t.Errorf("marks = %+v; want m1", marks)
	}
	n := memberOf(t, s, id, "b")
	if n.ID != next || n.State != MemberQueued || n.AttemptOf == nil || *n.AttemptOf != b.ID || n.RunID != id || n.Entry.ID != "" {
		t.Errorf("next attempt = %+v; want queued, pointing at the first", n)
	}
	if got := memberStates(t, s, id); !slices.Equal(got["b"], []MemberState{MemberFellBack, MemberQueued}) ||
		!slices.Equal(got["a"], []MemberState{MemberRunning}) {
		t.Errorf("states = %v; want b fell back and queued again, a still running", got)
	}
	// Twice: nothing more.
	if _, created, err := s.EnqueueMemberAttempt(ctx, b, quotaMark("m1")); err != nil || created {
		t.Errorf("second EnqueueMemberAttempt = %v, %v; want nothing", created, err)
	}
	// The third attempt still points at the first.
	dispatchMember(t, s, next, "tok-n")
	if ok, err := s.ReportMemberFailure(ctx, next, Failure{Reason: "quota"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	third, created, err := s.EnqueueMemberAttempt(ctx, memberOf(t, s, id, "b"), quotaMark("m2"))
	if err != nil || !created {
		t.Fatal(third, created, err)
	}
	if n := memberOf(t, s, id, "b"); n.ID != third || n.AttemptOf == nil || *n.AttemptOf != b.ID {
		t.Errorf("third attempt = %+v; want attempt_of the first", n)
	}
	// With the run gone, nothing is queued.
	a := memberOf(t, s, id, "a")
	dispatchMember(t, s, a.ID, "tok-a")
	if ok, err := s.ReportMemberFailure(ctx, a.ID, Failure{Reason: "quota"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	enqueue(t, s, mr1, "a2")
	if _, created, err := s.EnqueueMemberAttempt(ctx, memberOf(t, s, id, "a"), quotaMark("m3")); err != nil || created {
		t.Errorf("EnqueueMemberAttempt with the run gone = %v, %v; want nothing", created, err)
	}
}

func TestReleasePanelOneDone(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	doneMember(t, s, memberOf(t, s, id, "a").ID, `{}`)
	failedMember(t, s, memberOf(t, s, id, "b").ID, "B broke")
	setRun(t, s, id, `not_before = $2`, t0.Add(time.Hour))
	released, failed, err := s.ReleasePanel(ctx, id, t0.Add(time.Minute))
	if err != nil || !released || failed {
		t.Fatalf("ReleasePanel = %v, %v, %v; want released", released, failed, err)
	}
	r := getRun(t, s, id)
	if r.State != Queued || r.SkipReason != "" || !r.NotBefore.IsZero() || r.FailureReason != "" {
		t.Errorf("run = %s %q not before %v; want queued again, due now", r.State, r.SkipReason, r.NotBefore)
	}
	if released, failed, err := s.ReleasePanel(ctx, id, t0); err != nil || released || failed {
		t.Errorf("second ReleasePanel = %v, %v, %v; want nothing", released, failed, err)
	}
	if got, err := s.ClaimQueued(ctx, 10, t0); err != nil || len(got) != 1 || got[0].ID != id {
		t.Errorf("ClaimQueued = %v, %v; want the released run", runIDs(got), err)
	}
}

func TestReleasePanelBothFailed(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	failedMember(t, s, memberOf(t, s, id, "a").ID, "A broke")
	failedMember(t, s, memberOf(t, s, id, "b").ID, strings.Repeat("x", 3000))
	released, failed, err := s.ReleasePanel(ctx, id, t0.Add(time.Minute))
	if err != nil || released || !failed {
		t.Fatalf("ReleasePanel = %v, %v, %v; want failed", released, failed, err)
	}
	r := getRun(t, s, id)
	want := "both review panel members failed: A: A broke; B: " + strings.Repeat("x", maxChangeDetail)
	if r.State != Reported || r.FailureReason != want || !r.ReportedAt.Equal(t0.Add(time.Minute)) || r.Result != nil {
		t.Errorf("run = %s %q reported %v; want reported with both reasons", r.State, r.FailureReason, r.ReportedAt)
	}
	if due, err := s.RunsToPublish(ctx, t0.Add(time.Minute), 10); err != nil || len(due) != 1 || due[0].ID != id {
		t.Errorf("RunsToPublish = %v, %v; want the failed run", runIDs(due), err)
	}
}

func TestReleasePanelWaitsForLive(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	check := func(when string) {
		t.Helper()
		if released, failed, err := s.ReleasePanel(ctx, id, t0); err != nil || released || failed {
			t.Errorf("%s: ReleasePanel = %v, %v, %v; want nothing", when, released, failed, err)
		}
		if r := getRun(t, s, id); r.State != Waiting || r.SkipReason != WaitPanel {
			t.Errorf("%s: run = %s %q; want still waiting", when, r.State, r.SkipReason)
		}
	}
	check("both queued")
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	doneMember(t, s, a.ID, `{}`)
	check("b queued")
	dispatchMember(t, s, b.ID, "tok-b")
	check("b running")
	if ok, err := s.ReportMemberFailure(ctx, b.ID, Failure{Reason: "quota"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	check("b reported, not settled")
	next, created, err := s.EnqueueMemberAttempt(ctx, memberOf(t, s, id, "b"), quotaMark("m1"))
	if err != nil || !created {
		t.Fatal(next, created, err)
	}
	check("b fell back, its next attempt queued")
	doneMember(t, s, next, `{}`)
	if released, _, err := s.ReleasePanel(ctx, id, t0); err != nil || !released {
		t.Errorf("ReleasePanel with both done = %v, %v; want released", released, err)
	}
	// MembersToSettle lists reported members of a waiting run only.
	if ms, err := s.MembersToSettle(ctx); err != nil || len(ms) != 0 {
		t.Errorf("MembersToSettle = %+v, %v; want none", ms, err)
	}
}

func TestMembersToSettle(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	dispatchMember(t, s, a.ID, "tok-a")
	if _, err := s.MarkMemberDispatched(ctx, b.ID, "job-b", hash("tok-b"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreMemberResult(ctx, a.ID, "tok-a", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	if st, err := s.SettleMember(ctx, b.ID); err != nil || st != "" {
		t.Errorf("SettleMember of a running member = %q, %v; want nothing", st, err)
	}
	ms, err := s.MembersToSettle(ctx)
	if err != nil || len(ms) != 1 || ms[0].ID != a.ID {
		t.Fatalf("MembersToSettle = %+v, %v; want a", ms, err)
	}
	if err := s.ReplaceMemberWithFailure(ctx, a.ID, Failure{Reason: "bad result", Step: "check"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceMemberWithFailure(ctx, a.ID, Failure{Reason: "again"}); !errors.Is(err, ErrNotReported) {
		t.Errorf("second ReplaceMemberWithFailure = %v; want ErrNotReported", err)
	}
	if st, err := s.SettleMember(ctx, a.ID); err != nil || st != MemberFailed {
		t.Errorf("SettleMember = %q, %v; want failed", st, err)
	}
	if got := memberOf(t, s, id, "a"); got.Result != nil || got.FailureReason != "bad result" || got.FailureStep != "check" {
		t.Errorf("member a = %+v; want the failure", got)
	}
}

func TestSupersedePanelSupersedesMembers(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	doneMember(t, s, a.ID, `{}`)
	dispatchMember(t, s, b.ID, "tok-b")
	if err := s.SupersedePanel(ctx, id, SupersededDraft); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, id); r.State != Superseded || r.OutcomeReason != SupersededDraft {
		t.Errorf("run = %s %q; want superseded as a draft", r.State, r.OutcomeReason)
	}
	if got := memberStates(t, s, id); !slices.Equal(got["a"], []MemberState{MemberDone}) || !slices.Equal(got["b"], []MemberState{MemberSuperseded}) {
		t.Errorf("states = %v; want a done, b superseded", got)
	}
	clean, err := s.MembersToCleanUp(ctx)
	if err != nil || len(clean) != 1 || clean[0].ID != b.ID || clean[0].JobName != "job-tok-b" {
		t.Fatalf("MembersToCleanUp = %+v, %v; want b", clean, err)
	}
	if err := s.MarkMemberCleanedUp(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if clean, err := s.MembersToCleanUp(ctx); err != nil || len(clean) != 0 {
		t.Errorf("MembersToCleanUp after = %+v, %v; want none", clean, err)
	}
	if err := s.MarkMemberCleanedUp(ctx, "bad"); err == nil {
		t.Error("MarkMemberCleanedUp of a bad id succeeded")
	}
	// A run that does not wait for a panel is left alone.
	other := enqueue(t, s, mr2, "b1")
	if err := s.SupersedePanel(ctx, other, ""); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, other); r.State != Queued {
		t.Errorf("queued run = %s; want left alone", r.State)
	}
	if err := s.SupersedePanel(ctx, "bad", ""); err != nil {
		t.Errorf("SupersedePanel of a bad id = %v; want nil", err)
	}
}

func TestPanelRun(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	if r, found, err := s.PanelRun(ctx, id); err != nil || !found || r.ID != id || r.State != Waiting {
		t.Fatalf("PanelRun = %s %s %v, %v; want the waiting run", r.ID, r.State, found, err)
	}
	// A later attempt that waits is found through the review's first run.
	later := enqueue(t, s, mr2, "b1")
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'superseded' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.PanelRun(ctx, id); err != nil || found {
		t.Fatalf("PanelRun of a superseded panel = %v, %v; want none", found, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET attempt_of = $2::uuid, state = 'waiting', skip_reason = 'panel_members'
		WHERE id = $1::uuid`, later, id); err != nil {
		t.Fatal(err)
	}
	if r, found, err := s.PanelRun(ctx, id); err != nil || !found || r.ID != later {
		t.Errorf("PanelRun = %s %v, %v; want the later attempt", r.ID, found, err)
	}
	// A run that waits for something else is not a panel's.
	held := heldRun(t, s, mr1, "a3", WaitUntrusted)
	if _, found, err := s.PanelRun(ctx, held); err != nil || found {
		t.Errorf("PanelRun of a held run = %v, %v; want none", found, err)
	}
	if _, found, err := s.PanelRun(ctx, "bad"); err != nil || found {
		t.Errorf("PanelRun of a bad id = %v, %v; want none", found, err)
	}
}

func TestSupersedeOrphanMembers(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	keep := panelRun(t, s, mr2, "b1")
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	doneMember(t, s, a.ID, `{}`)
	dispatchMember(t, s, b.ID, "tok-b")
	if n, err := s.SupersedeOrphanMembers(ctx); err != nil || n != 0 {
		t.Fatalf("SupersedeOrphanMembers with live runs = %d, %v; want 0", n, err)
	}
	enqueue(t, s, mr1, "a2")
	if r := getRun(t, s, id); r.State != Superseded {
		t.Fatalf("waiting run = %s; want superseded by the new head", r.State)
	}
	if n, err := s.SupersedeOrphanMembers(ctx); err != nil || n != 1 {
		t.Fatalf("SupersedeOrphanMembers = %d, %v; want 1", n, err)
	}
	if got := memberStates(t, s, id); !slices.Equal(got["a"], []MemberState{MemberDone}) || !slices.Equal(got["b"], []MemberState{MemberSuperseded}) {
		t.Errorf("states = %v; want a done, b superseded", got)
	}
	if got := memberStates(t, s, keep); !slices.Equal(got["a"], []MemberState{MemberRunning}) && !slices.Equal(got["a"], []MemberState{MemberQueued}) {
		t.Errorf("other review's members = %v; want left alone", got)
	}
	if n, err := s.SupersedeOrphanMembers(ctx); err != nil || n != 0 {
		t.Errorf("second SupersedeOrphanMembers = %d, %v; want 0", n, err)
	}
}

func TestPanelMemberResult(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	dispatchMember(t, s, a.ID, "tok-a")
	if _, err := s.MarkMemberDispatched(ctx, b.ID, "job-b", hash("tok-b"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreMemberResult(ctx, a.ID, "tok-a", []byte(`{"role":"a"}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	if ok, err := s.ReportMemberFailure(ctx, b.ID, Failure{Reason: "b broke"}, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// Reported, not settled: not done yet. The run waits: no token yet.
	if body, found, err := s.PanelMemberResult(ctx, id, "tok-p", "a"); err != nil || found || body != nil {
		t.Errorf("PanelMemberResult before P = %s, %v, %v; want nothing", body, found, err)
	}
	for _, m := range []string{a.ID, b.ID} {
		if _, err := s.SettleMember(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if released, _, err := s.ReleasePanel(ctx, id, t0); err != nil || !released {
		t.Fatal(released, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	// The token check reads the wall clock.
	if err := s.MarkDispatched(ctx, id, "job-p", "base", "start", hash("tok-p"), time.Now().Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	body, found, err := s.PanelMemberResult(ctx, id, "tok-p", "a")
	if err != nil || !found || string(body) != `{"role": "a"}` {
		t.Errorf("PanelMemberResult(a) = %s, %v, %v; want a's result", body, found, err)
	}
	other := dispatched(t, s, mr2, "b1", "tok-o")
	for _, c := range []struct {
		name, run, token, role string
	}{
		{"failed role", id, "tok-p", "b"},
		{"wrong token", id, "tok-a", "a"},
		{"no token", id, "", "a"},
		{"other run's token", id, "tok-o", "a"},
		{"other run", other, "tok-o", "a"},
		{"unknown role", id, "tok-p", "c"},
		{"unknown run", "00000000-0000-0000-0000-000000000000", "tok-p", "a"},
		{"bad id", "x", "tok-p", "a"},
	} {
		if body, found, err := s.PanelMemberResult(ctx, c.run, c.token, c.role); err != nil || found || body != nil {
			t.Errorf("%s: PanelMemberResult = %s, %v, %v; want nothing", c.name, body, found, err)
		}
	}
	if _, found, err := s.PanelMemberResultAt(ctx, id, "tok-p", "a", time.Now().Add(2*time.Hour)); err != nil || found {
		t.Errorf("expired token: found %v, %v; want nothing", found, err)
	}

	// A later attempt of P reads its root's members; the first one's token no
	// longer works.
	if out, err := s.StoreResult(ctx, id, "tok-p", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	next, created, err := s.EnqueueAttempt(ctx, getRun(t, s, id), quotaMark("m1"))
	if err != nil || !created {
		t.Fatal(next, created, err)
	}
	reportAttemptToRunning(t, s, next, "tok-p2")
	if body, found, err := s.PanelMemberResult(ctx, next, "tok-p2", "a"); err != nil || !found || string(body) != `{"role": "a"}` {
		t.Errorf("later attempt: PanelMemberResult = %s, %v, %v; want the root's a", body, found, err)
	}
	if _, found, err := s.PanelMemberResult(ctx, id, "tok-p", "a"); err != nil || found {
		t.Errorf("superseded first attempt: found %v, %v; want nothing", found, err)
	}
}

// reportAttemptToRunning claims the queued attempt id and records its Job.
func reportAttemptToRunning(t *testing.T, s *Store, id, token string) {
	t.Helper()
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatched(ctx, id, "job-"+token, "base", "start", hash(token), time.Now().Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
}

func TestRecordPanelOffOnce(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if err := s.RecordPanelOff(ctx, id, "the review image does not support it"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPanelOff(ctx, id, "the token quota stopped model B"); err != nil {
		t.Fatalf("second RecordPanelOff: %v", err)
	}
	ms, err := s.PanelMembers(ctx, id)
	if err != nil || len(ms) != 1 || ms[0].State != MemberOff || ms[0].FailureReason != "the token quota stopped model B" {
		t.Fatalf("PanelMembers = %+v, %v; want one off row with the last reason", ms, err)
	}
	// Off never moves.
	if got, err := s.ClaimQueuedMembers(ctx, 10, t0); err != nil || len(got) != 0 {
		t.Errorf("ClaimQueuedMembers = %+v, %v; want none", got, err)
	}
	if n, err := s.SupersedeOrphanMembers(ctx); err != nil || n != 0 {
		t.Errorf("SupersedeOrphanMembers = %d, %v; want 0", n, err)
	}
	if err := s.RecordPanelOff(ctx, "00000000-0000-0000-0000-000000000000", "x"); err == nil {
		t.Error("RecordPanelOff of an unknown run succeeded")
	}
	// A later attempt's off row is its review's.
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatched(ctx, id, "job", "base", "start", hash("t"), t0.Add(time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if out, err := s.StoreResult(ctx, id, "t", []byte(`{}`), t0); err != nil || out != ResultStored {
		t.Fatal(out, err)
	}
	next, _, err := s.EnqueueAttempt(ctx, getRun(t, s, id), quotaMark("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPanelOff(ctx, next, "model B is the model A runs on"); err != nil {
		t.Fatal(err)
	}
	if ms, err := s.PanelMembers(ctx, id); err != nil || len(ms) != 1 || ms[0].RunID != id || ms[0].FailureReason != "model B is the model A runs on" {
		t.Errorf("PanelMembers = %+v, %v; want the one off row of the review", ms, err)
	}
	// A panel that starts on a later attempt replaces the off row.
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if started, err := s.StartPanel(ctx, next, t0); err != nil || !started {
		t.Fatalf("StartPanel of the later attempt = %v, %v", started, err)
	}
	if got := memberStates(t, s, id); !slices.Equal(got["a"], []MemberState{MemberQueued}) || !slices.Equal(got["b"], []MemberState{MemberQueued}) {
		t.Errorf("states = %v; want 2 queued members of the review, no off row", got)
	}
	if got, err := s.ClaimQueuedMembers(ctx, 10, t0); err != nil || len(got) != 2 {
		t.Errorf("ClaimQueuedMembers = %+v, %v; want both, their run being the later attempt", got, err)
	}
}

func TestRecoverUnlaunchedMembersRequeues(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	dispatchMember(t, s, a.ID, "tok-a") // claims b too, with no Job
	if rs, err := s.RunningMembers(ctx); err != nil || len(rs) != 2 {
		t.Fatalf("RunningMembers = %+v, %v; want 2", rs, err)
	}
	if n, err := s.RecoverUnlaunchedMembers(ctx, t0); err != nil || n != 0 {
		t.Errorf("RecoverUnlaunchedMembers before the claim = %d, %v; want 0", n, err)
	}
	if n, err := s.RecoverUnlaunchedMembers(ctx, t0.Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("RecoverUnlaunchedMembers = %d, %v; want 1", n, err)
	}
	if got := memberOf(t, s, id, "b"); got.State != MemberQueued || got.Attempts != 0 {
		t.Errorf("member b = %s attempts %d; want queued, no attempt counted", got.State, got.Attempts)
	}
	if _, err := s.ClaimQueuedMembers(ctx, 10, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueMember(ctx, b.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := memberOf(t, s, id, "b"); got.State != MemberQueued || got.Attempts != 1 || !got.NotBefore.Equal(t0.Add(time.Hour)) {
		t.Errorf("member b = %s attempts %d not before %v; want queued, one attempt", got.State, got.Attempts, got.NotBefore)
	}
	if err := s.RequeueMember(ctx, "bad", t0); err == nil {
		t.Error("RequeueMember of a bad id succeeded")
	}
	if err := s.SetMemberPod(ctx, a.ID, "pod-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceMemberCheckoutSeen(ctx, a.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceMemberCheckoutSeen(ctx, a.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := memberOf(t, s, id, "a"); got.PodName != "pod-a" || got.CheckoutSeen != 2 {
		t.Errorf("member a = pod %q seen %d; want pod-a, 2", got.PodName, got.CheckoutSeen)
	}
}

func TestPanelJudgesRoundTrip(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	ea := RunEntry{Source: SourceGlobal, ID: "m1", Name: "Main", Length: 2}
	eb := RunEntry{Source: SourceTeam, ID: "t1", Name: "Team", TeamID: 4, Length: 1}
	if err := s.SetPanelJudge(ctx, id, "a", RunEntry{Source: SourceGlobal, ID: "old"}, "p", "m"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPanelJudge(ctx, id, "a", ea, "anthropic", "sonnet"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPanelJudge(ctx, id, "b", eb, "openai", "gpt"); err != nil {
		t.Fatal(err)
	}
	got, err := s.PanelJudges(ctx, id)
	if err != nil || len(got) != 2 || got["a"] != ea || got["b"] != eb {
		t.Errorf("PanelJudges = %+v, %v; want a %+v, b %+v", got, err, ea, eb)
	}
	if err := s.SetPanelJudge(ctx, id, "c", ea, "x", "y"); err == nil {
		t.Error("SetPanelJudge of role c succeeded")
	}
	if got, err := s.PanelJudges(ctx, "bad"); err != nil || len(got) != 0 {
		t.Errorf("PanelJudges of a bad id = %+v, %v; want none", got, err)
	}
}

func TestPruneRunLogsPrunesMembers(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
	for _, m := range []string{a.ID, b.ID} {
		if err := s.PutMemberLog(ctx, m, "log \x00"+m); err != nil {
			t.Fatal(err)
		}
		if err := s.PutMemberDiagnostics(ctx, m, Diagnostics{CloneLog: "c", CloneLogMasked: "cm", PodState: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	if text, found, err := s.MemberLog(ctx, a.ID); err != nil || !found || text != "log "+a.ID {
		t.Errorf("MemberLog = %q, %v, %v", text, found, err)
	}
	if d, found, err := s.MemberDiagnostics(ctx, a.ID); err != nil || !found || d != (Diagnostics{"c", "cm", "p"}) {
		t.Errorf("MemberDiagnostics = %+v, %v, %v", d, found, err)
	}
	// A member's log is not the run's.
	if _, found, _ := s.RunLog(ctx, id); found {
		t.Error("the run has a log")
	}
	for _, table := range []string{"panel_member_logs", "panel_member_diagnostics"} {
		if _, err := s.pool.Exec(ctx, `UPDATE `+table+` SET created_at = now() - interval '31 days' WHERE member_id = $1::uuid`, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneRunLogs(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("PruneRunLogs = %d, %v; want 2", n, err)
	}
	if _, found, _ := s.MemberLog(ctx, a.ID); found {
		t.Error("a's old log is still there")
	}
	if _, found, _ := s.MemberDiagnostics(ctx, a.ID); found {
		t.Error("a's old diagnostics are still there")
	}
	if _, found, _ := s.MemberLog(ctx, b.ID); !found {
		t.Error("b's fresh log was pruned")
	}
	if _, found, _ := s.MemberDiagnostics(ctx, b.ID); !found {
		t.Error("b's fresh diagnostics were pruned")
	}
	if _, found, err := s.MemberLog(ctx, "bad"); found || err != nil {
		t.Errorf("MemberLog of a bad id = %v, %v", found, err)
	}
}

// 2 dispatches at once: one is first. 2 posts of a result at once: one is stored.
func TestMemberDispatchAndResultRaces(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 5; i++ {
		id := panelRun(t, s, MRKey{Instance: mr1.Instance, ProjectID: 8, IID: int64(i + 1)}, "h")
		a, b := memberOf(t, s, id, "a"), memberOf(t, s, id, "b")
		if _, err := s.ClaimQueuedMembers(ctx, 10, t0); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		firsts := make([]bool, 2)
		errs := make([]error, 2)
		for j, m := range []PanelMember{a, b} {
			wg.Add(1)
			go func(j int, m PanelMember) {
				defer wg.Done()
				firsts[j], errs[j] = s.MarkMemberDispatched(ctx, m.ID, "job", hash("tok-"+m.Role), t0.Add(time.Hour), t0)
			}(j, m)
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || firsts[0] == firsts[1] {
			t.Fatalf("dispatches = %v, %v; want exactly one first", firsts, errs)
		}
		outs := make([]ResultOutcome, 2)
		for j := range outs {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				outs[j], errs[j] = s.StoreMemberResult(ctx, a.ID, "tok-a", []byte(`{}`), t0)
			}(j)
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || !slices.Contains(outs, ResultStored) || !slices.Contains(outs, ResultConflict) {
			t.Fatalf("posts = %v, %v; want one stored, one conflict", outs, errs)
		}
	}
}

// heldRun gives a run of head on mr held for reason: a waiting run that is no
// panel's.
func heldRun(t *testing.T, s *Store, mr MRKey, head, reason string) string {
	t.Helper()
	id := enqueue(t, s, mr, head)
	if ok, err := s.HoldRun(ctx, id, reason); err != nil || !ok {
		t.Fatalf("HoldRun = %v, %v", ok, err)
	}
	return id
}

// apiPanelRun gives client's API review of subject made a panel review. Any
// other queued run is claimed with it.
func apiPanelRun(t *testing.T, s *Store, clientID int64, subject string) string {
	t.Helper()
	id := enqueueAPI(t, s, apiRequest(clientID, "acme", subject))
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if started, err := s.StartPanel(ctx, id, t0); err != nil || !started {
		t.Fatalf("StartPanel = %v, %v", started, err)
	}
	return id
}

// A start finds a panel's waiting run on its way and leaves it as it is; a
// held run is started as before.
func TestStartRunLeavesPanelAlone(t *testing.T) {
	s := newStore(t)
	panel := panelRun(t, s, mr1, "a1")
	held := heldRun(t, s, mr2, "b1", WaitUntrusted)

	id, started, err := s.StartRun(ctx, mr1, "acme/widgets", "a1", "main")
	if err != nil || id != panel || started {
		t.Fatalf("StartRun(panel) = %s, %v, %v; want %s, false", id, started, err, panel)
	}
	if r := getRun(t, s, panel); r.State != Waiting || r.SkipReason != WaitPanel || r.Trigger != TriggerPush {
		t.Errorf("panel run = %s %q %s; want still waiting for its members", r.State, r.SkipReason, r.Trigger)
	}
	if got := memberStates(t, s, panel); !slices.Equal(got["a"], []MemberState{MemberQueued}) ||
		!slices.Equal(got["b"], []MemberState{MemberQueued}) {
		t.Errorf("members = %v; want both still queued", got)
	}

	id, started, err = s.StartRun(ctx, mr2, "acme/widgets", "b1", "main")
	if err != nil || id != held || !started {
		t.Fatalf("StartRun(held) = %s, %v, %v; want %s, true", id, started, err, held)
	}
	if r := getRun(t, s, held); r.State != Queued || r.Trigger != TriggerStart {
		t.Errorf("held run = %s %s; want queued as a start", r.State, r.Trigger)
	}
}

// A re-run while the members run lands on the panel's run; a held run is
// replaced by a new one as before.
func TestRerunWhilePanelRuns(t *testing.T) {
	s := newStore(t)
	panel := panelRun(t, s, mr1, "a1")
	held := heldRun(t, s, mr2, "b1", WaitUntrusted)

	if active, err := s.ReviewActiveForRerun(ctx, mr1, "a1", "main", true); err != nil || !active {
		t.Errorf("ReviewActiveForRerun(panel) = %v, %v; want true", active, err)
	}
	if active, err := s.ReviewActiveForStart(ctx, mr1, "a1", "main"); err != nil || !active {
		t.Errorf("ReviewActiveForStart(panel) = %v, %v; want true", active, err)
	}
	id, created, err := s.EnqueueRerun(ctx, mr1, "acme/widgets", "a1", "main", TriggerRerun, nil, nil, true, false)
	if err != nil || id != panel || created {
		t.Fatalf("EnqueueRerun(panel) = %s, %v, %v; want %s, false", id, created, err, panel)
	}
	if r := getRun(t, s, panel); r.State != Waiting || r.SkipReason != WaitPanel {
		t.Errorf("panel run = %s %q; want still waiting for its members", r.State, r.SkipReason)
	}

	if active, err := s.ReviewActiveForRerun(ctx, mr2, "b1", "main", true); err != nil || active {
		t.Errorf("ReviewActiveForRerun(held) = %v, %v; want false", active, err)
	}
	if active, err := s.ReviewActiveForStart(ctx, mr2, "b1", "main"); err != nil || active {
		t.Errorf("ReviewActiveForStart(held) = %v, %v; want false", active, err)
	}
	id, created, err = s.EnqueueRerun(ctx, mr2, "acme/widgets", "b1", "main", TriggerRerun, nil, nil, true, false)
	if err != nil || id == held || !created {
		t.Fatalf("EnqueueRerun(held) = %s, %v, %v; want a new run", id, created, err)
	}
	if r := getRun(t, s, held); r.State != Superseded {
		t.Errorf("held run = %s; want superseded by the re-run", r.State)
	}
}

// The changes waiting for a start leave out a panel waiting for its members.
func TestWaitingChangesHidesPanel(t *testing.T) {
	s := newStore(t)
	panelRun(t, s, mr1, "a1")
	heldRun(t, s, mr2, "b1", WaitUntrusted)
	got, err := s.WaitingChanges(ctx, mr1.Instance, "acme/widgets")
	if err != nil || len(got) != 1 || got[0].IID != mr2.IID || got[0].Reason != WaitUntrusted {
		t.Errorf("WaitingChanges = %+v, %v; want only !%d", got, err, mr2.IID)
	}
}

// The dashboard reads a panel waiting for its members as running, and a held
// run as waiting.
func TestDashboardOutcomePanelRunning(t *testing.T) {
	s := newStore(t)
	panel := panelRun(t, s, mr1, "a1")
	held := heldRun(t, s, mr2, "b1", WaitUntrusted)
	for id, want := range map[string]string{panel: "running", held: "waiting"} {
		d, found, err := s.RunDigest(ctx, id)
		if err != nil || !found || d.State != Waiting || d.Outcome != want {
			t.Errorf("RunDigest(%s) = %s %q, %v, %v; want waiting, %q", id, d.State, d.Outcome, found, err, want)
		}
	}
	for _, c := range []struct {
		outcome string
		want    []string
	}{{"running", []string{panel}}, {"waiting", []string{held}}} {
		ds, err := s.ReviewDigests(ctx, ReviewFilter{Visible: Visibility{All: true}, Outcome: c.outcome, Limit: 10})
		var ids []string
		for _, d := range ds {
			ids = append(ids, d.ID)
		}
		if err != nil || !slices.Equal(ids, c.want) {
			t.Errorf("ReviewDigests(%s) = %v, %v; want %v", c.outcome, ids, err, c.want)
		}
	}
}

// The v1 status of a panel review in its members is running; a held review is
// in none of the statuses, as before.
func TestAPIStatusPanelRunning(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	panel := apiPanelRun(t, s, c.ID, "route:1")
	held := enqueueAPI(t, s, apiRequest(c.ID, "acme", "route:2"))
	if ok, err := s.HoldRun(ctx, held, WaitQuota); err != nil || !ok {
		t.Fatalf("HoldRun = %v, %v", ok, err)
	}
	for status, want := range map[string][]string{"running": {panel}, "queued": nil, "completed": nil, "failed": nil, "superseded": nil} {
		rs, err := s.APIReviews(ctx, APIReviewFilter{ClientID: c.ID, Status: status})
		var ids []string
		for _, r := range rs {
			ids = append(ids, r.Run.ID)
		}
		if err != nil || !slices.Equal(ids, want) {
			t.Errorf("APIReviews(%s) = %v, %v; want %v", status, ids, err, want)
		}
	}
}

// A client's panel waiting for its members is a review in flight: with
// max_in_flight 1 the client's next review stays queued. A held review is
// not in flight, as before. The members' Jobs hold jobs.max_running slots
// (CountRunning); the waiting run holds none.
func TestClaimQueuedCountsPanelInFlight(t *testing.T) {
	s := newStore(t)
	a := newAPIClient(t, s, "client-a")
	b := newAPIClient(t, s, "client-b")
	setMaxInFlight(t, s, "client-a", 1)
	setMaxInFlight(t, s, "client-b", 1)
	panel := apiPanelRun(t, s, a.ID, "s1")
	held := enqueueAPI(t, s, apiRequest(b.ID, "acme", "s1"))
	if ok, err := s.HoldRun(ctx, held, WaitQuota); err != nil || !ok {
		t.Fatalf("HoldRun = %v, %v", ok, err)
	}
	a2 := enqueueAPI(t, s, apiRequest(a.ID, "acme", "s2"))
	b2 := enqueueAPI(t, s, apiRequest(b.ID, "acme", "s2"))

	got, err := s.ClaimQueued(ctx, 10, time.Now().Add(time.Hour))
	if err != nil || !slices.Equal(runIDs(got), []string{b2}) {
		t.Fatalf("ClaimQueued = %v, %v; want only %s: a's panel is in flight", runIDs(got), err, b2)
	}
	if st := getRun(t, s, a2).State; st != Queued {
		t.Errorf("a's next review = %s, want queued", st)
	}

	// Both members are claimed: each is counted once, the waiting run not.
	dispatchMember(t, s, memberOf(t, s, panel, "a").ID, "tok-a")
	if n, err := s.CountRunning(ctx); err != nil || n != 3 {
		t.Errorf("CountRunning = %d, %v; want 3 (b's claimed review and 2 members)", n, err)
	}
}

// Removing a client stops its panels: the running members' Jobs are named
// and the members go with the runs.
func TestDeleteAPIClientStopsPanel(t *testing.T) {
	s := newStore(t)
	c := newAPIClient(t, s, "fastgateway")
	other := newAPIClient(t, s, "other")
	panel := apiPanelRun(t, s, c.ID, "route:1")
	otherPanel := apiPanelRun(t, s, other.ID, "route:1")
	ma, mb := memberOf(t, s, panel, "a"), memberOf(t, s, panel, "b")
	dispatchMember(t, s, ma.ID, "tok-a")
	dispatchMember(t, s, memberOf(t, s, otherPanel, "a").ID, "tok-other")
	// Member b is claimed, not dispatched: it has no Job yet.

	jobs, found, err := s.DeleteAPIClient(ctx, "fastgateway")
	if err != nil || !found {
		t.Fatalf("DeleteAPIClient = %v, %v, %v", jobs, found, err)
	}
	if !slices.Equal(jobs, []string{"job-tok-a"}) {
		t.Errorf("jobs = %v, want [job-tok-a]", jobs)
	}
	if ms, err := s.PanelMembers(ctx, panel); err != nil || len(ms) != 0 {
		t.Errorf("members after the delete = %+v, %v; want none", ms, err)
	}
	if _, err := s.MarkMemberDispatched(ctx, mb.ID, "job-late", hash("late"), t0.Add(time.Hour), t0); !errors.Is(err, ErrNotRunning) {
		t.Errorf("a late dispatch = %v, want ErrNotRunning", err)
	}
	if ms, err := s.PanelMembers(ctx, otherPanel); err != nil || len(ms) != 2 {
		t.Errorf("another client's members = %+v, %v; want both", ms, err)
	}
}

// A run released for Job P is claimed again with its dispatched_at kept: it is
// not split a second time.
func TestStartPanelOncePerReview(t *testing.T) {
	s := newStore(t)
	id := panelRun(t, s, mr1, "a1")
	doneMember(t, s, memberOf(t, s, id, "a").ID, `{}`)
	failedMember(t, s, memberOf(t, s, id, "b").ID, "B broke")
	if released, _, err := s.ReleasePanel(ctx, id, t0); err != nil || !released {
		t.Fatal(released, err)
	}
	if got, err := s.ClaimQueued(ctx, 10, t0); err != nil || len(got) != 1 {
		t.Fatal(runIDs(got), err)
	}
	if started, err := s.StartPanel(ctx, id, t0.Add(time.Minute)); err != nil || started {
		t.Errorf("StartPanel of a released run = %v, %v; want not started", started, err)
	}
	if r := getRun(t, s, id); r.State != Running || r.SkipReason != "" || !r.DispatchedAt.Equal(t0) {
		t.Errorf("run = %s %q dispatched %v; want still claimed for Job P", r.State, r.SkipReason, r.DispatchedAt)
	}
	if got := memberStates(t, s, id); !slices.Equal(got["a"], []MemberState{MemberDone}) || !slices.Equal(got["b"], []MemberState{MemberFailed}) {
		t.Errorf("states = %v; want the first panel only", got)
	}
}

// A panel started on a later attempt is read and released by that attempt's
// id as well as by the first run's.
func TestReleasePanelByLaterAttempt(t *testing.T) {
	s := newStore(t)
	root := reported(t, s, mr1, "a1", `{}`)
	next, created, err := s.EnqueueAttempt(ctx, getRun(t, s, root), quotaMark("m1"))
	if err != nil || !created {
		t.Fatal(next, created, err)
	}
	if _, err := s.ClaimQueued(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if started, err := s.StartPanel(ctx, next, t0); err != nil || !started {
		t.Fatalf("StartPanel = %v, %v", started, err)
	}
	byRoot, err := s.PanelMembers(ctx, root)
	if err != nil || len(byRoot) != 2 {
		t.Fatalf("PanelMembers(root) = %+v, %v; want 2", byRoot, err)
	}
	byNext, err := s.PanelMembers(ctx, next)
	if err != nil || len(byNext) != 2 || byNext[0].ID != byRoot[0].ID || byNext[1].ID != byRoot[1].ID {
		t.Fatalf("PanelMembers(later attempt) = %+v, %v; want the root's members", byNext, err)
	}
	doneMember(t, s, memberOf(t, s, next, "a").ID, `{}`)
	doneMember(t, s, memberOf(t, s, next, "b").ID, `{}`)
	released, failed, err := s.ReleasePanel(ctx, next, t0)
	if err != nil || !released || failed {
		t.Fatalf("ReleasePanel(later attempt) = %v, %v, %v; want released", released, failed, err)
	}
	if r := getRun(t, s, next); r.State != Queued || r.SkipReason != "" {
		t.Errorf("later attempt = %s %q; want queued again", r.State, r.SkipReason)
	}
	if r := getRun(t, s, root); r.State != Superseded {
		t.Errorf("first run = %s; want left superseded", r.State)
	}
}
