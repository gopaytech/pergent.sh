package actions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const testDBEnv = "PERGENT_SH_TEST_DATABASE_URL"

var ctx = context.Background()

// newStore opens a migrated store in a fresh schema, dropped when the test
// ends. It skips when no test database is set.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv(testDBEnv)
	if url == "" {
		t.Skipf("%s is not set", testDBEnv)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "test_actions_" + hex.EncodeToString(b)
	st, err := store.OpenInSchema(ctx, url, schema)
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		drop, err := store.OpenInSchema(context.Background(), url, schema)
		if err != nil {
			t.Errorf("OpenInSchema: %v", err)
			return
		}
		defer drop.Close()
		if err := drop.DropSchema(context.Background()); err != nil {
			t.Errorf("DropSchema: %v", err)
		}
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// seedUsage stores a reported run of merge request iid of project 1, as the
// Job's result came in at at, using tokens in tokens.
func seedUsage(t *testing.T, st *store.Store, iid int64, at time.Time, tokens int64) {
	t.Helper()
	mr := store.MRKey{Instance: "main", ProjectID: 1, IID: iid}
	id, _, err := st.EnqueueRun(ctx, mr, "acme/web", fmt.Sprintf("seed%d", iid), "main")
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	if got, err := st.ClaimQueued(ctx, 10, at); err != nil || len(got) == 0 {
		t.Fatalf("ClaimQueued = %v, %v", got, err)
	}
	token := "tok-" + id
	sum := sha256.Sum256([]byte(token))
	if err := st.MarkDispatched(ctx, id, "job-"+id, "base", "start", sum[:], at.Add(time.Hour), at); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	body := []byte(`{"schema_version":1,"status":"completed","skills":[]}`)
	if out, err := st.StoreResult(ctx, id, token, body, at); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	zero := int64(0)
	if err := st.RecordUsage(ctx, id, []store.SkillUsage{{Skill: "a", Verdict: "no_blocking_issues", In: tokens, CacheRead: &zero, CacheWrite: &zero, Reasoning: &zero}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
}

var quotaNow = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

// quotaRepo is a repository with a quota of 10 tokens a month and roomy caps.
func quotaRepo() settings.Effective {
	q := int64(10)
	return settings.Effective{MonthlyTokenQuota: &q, RerunLimitPerMR: 10, RerunLimitPerRepo: 50}
}

var quotaMR = store.MRKey{Instance: "main", ProjectID: 1, IID: 1}

func rerunRequest(repo settings.Effective) RerunRequest {
	return RerunRequest{MR: quotaMR, ProjectPath: "acme/web", Head: "head1", TargetBranch: "main", Repo: repo, Now: quotaNow}
}

func startRequest(repo settings.Effective) StartRequest {
	return StartRequest{MR: quotaMR, ProjectPath: "acme/web", Head: "head1", TargetBranch: "main", Repo: repo, Now: quotaNow}
}

// runsOf counts the runs of quotaMR.
func runsOf(t *testing.T, st *store.Store) int {
	t.Helper()
	runs, err := st.MRRunDigests(ctx, quotaMR, store.Visibility{All: true})
	if err != nil {
		t.Fatalf("MRRunDigests: %v", err)
	}
	return len(runs)
}

func reruns(t *testing.T, st *store.Store) int {
	t.Helper()
	n, err := st.CountReruns(ctx, &quotaMR, "", 0, quotaNow.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("CountReruns: %v", err)
	}
	return n
}

func TestRerunRefusedOnUsedUpQuota(t *testing.T) {
	st := newStore(t)
	seedUsage(t, st, 1000, quotaNow.Add(-time.Hour), 10)

	res, err := Rerun(ctx, st, rerunRequest(quotaRepo()))
	if err != nil || res.Status != RerunQuotaUsedUp {
		t.Fatalf("Rerun = %+v, %v; want RerunQuotaUsedUp", res, err)
	}
	if n := runsOf(t, st); n != 0 {
		t.Errorf("a refused re-run left %d runs of the change, want 0", n)
	}
	if n := reruns(t, st); n != 0 {
		t.Errorf("CountReruns = %d after a refusal, want 0", n)
	}
}

func TestRerunPastQuota(t *testing.T) {
	st := newStore(t)
	seedUsage(t, st, 1000, quotaNow.Add(-time.Hour), 10)

	req := rerunRequest(quotaRepo())
	req.PastQuota = true
	res, err := Rerun(ctx, st, req)
	if err != nil || res.Status != RerunQueued {
		t.Fatalf("Rerun = %+v, %v; want RerunQueued", res, err)
	}
	assertQueuedChecked(t, st)
}

func TestRerunWithRoomQueuesChecked(t *testing.T) {
	st := newStore(t)
	seedUsage(t, st, 1000, quotaNow.Add(-time.Hour), 9)

	res, err := Rerun(ctx, st, rerunRequest(quotaRepo()))
	if err != nil || res.Status != RerunQueued {
		t.Fatalf("Rerun = %+v, %v; want RerunQueued", res, err)
	}
	assertQueuedChecked(t, st)
}

// A quota-checked run is the one the dispatcher leaves alone.
func assertQueuedChecked(t *testing.T, st *store.Store) {
	t.Helper()
	runs, err := st.MRRunDigests(ctx, quotaMR, store.Visibility{All: true})
	if err != nil || len(runs) != 1 {
		t.Fatalf("MRRunDigests = %v, %v; want one run", runs, err)
	}
	run, ok, err := st.Run(ctx, runs[0].ID)
	if err != nil || !ok {
		t.Fatalf("Run = %v, %v", ok, err)
	}
	if run.State != store.Queued || !run.QuotaChecked {
		t.Errorf("run = %s, quota checked %v; want queued, checked", run.State, run.QuotaChecked)
	}
}

// A repository with a quota but room under it, and an instance quota used up
// instead, are both read.
func TestRerunRefusedOnUsedUpInstanceQuota(t *testing.T) {
	st := newStore(t)
	seedUsage(t, st, 1000, quotaNow.Add(-time.Hour), 10)

	q := int64(10)
	repo := settings.Effective{InstanceMonthlyTokenQuota: &q, RerunLimitPerMR: 10, RerunLimitPerRepo: 50}
	if res, err := Rerun(ctx, st, rerunRequest(repo)); err != nil || res.Status != RerunQuotaUsedUp {
		t.Fatalf("Rerun = %+v, %v; want RerunQuotaUsedUp", res, err)
	}
}

func TestStartRefusedOnUsedUpQuota(t *testing.T) {
	st := newStore(t)
	seedUsage(t, st, 1000, quotaNow.Add(-time.Hour), 10)
	id, _, err := st.EnqueueRun(ctx, quotaMR, "acme/web", "head1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.HoldForQuota(ctx, id); err != nil || !ok {
		t.Fatalf("HoldForQuota = %v, %v", ok, err)
	}

	res, err := Start(ctx, st, startRequest(quotaRepo()))
	if err != nil || res.Status != RerunQuotaUsedUp {
		t.Fatalf("Start = %+v, %v; want RerunQuotaUsedUp", res, err)
	}
	run, _, _ := st.Run(ctx, id)
	if run.State != store.Waiting || run.SkipReason != store.WaitQuota {
		t.Errorf("held run = %s/%q after a refused start, want waiting/%s", run.State, run.SkipReason, store.WaitQuota)
	}
}

func TestStartPastQuota(t *testing.T) {
	st := newStore(t)
	seedUsage(t, st, 1000, quotaNow.Add(-time.Hour), 10)
	id, _, err := st.EnqueueRun(ctx, quotaMR, "acme/web", "head1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.HoldForQuota(ctx, id); err != nil || !ok {
		t.Fatalf("HoldForQuota = %v, %v", ok, err)
	}

	req := startRequest(quotaRepo())
	req.PastQuota = true
	res, err := Start(ctx, st, req)
	if err != nil || res.Status != RerunQueued {
		t.Fatalf("Start = %+v, %v; want RerunQueued", res, err)
	}
	run, _, _ := st.Run(ctx, id)
	if run.State != store.Queued || run.Trigger != store.TriggerStart || !run.QuotaChecked {
		t.Errorf("run = %s/%s checked %v; want queued/start, checked", run.State, run.Trigger, run.QuotaChecked)
	}
}
