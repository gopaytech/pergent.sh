package quota

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
	schema := "test_quota_" + hex.EncodeToString(b)
	st, err := store.OpenInSchema(ctx, url, schema)
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	// The schema is dropped through a handle of its own, so a test may close st.
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

func i64(n int64) *int64 { return &n }

func TestMonthStartIsUTC(t *testing.T) {
	jakarta := time.FixedZone("WIB", 7*60*60)
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for name, in := range map[string]time.Time{
		"06:30 at UTC+7 is still November in UTC": time.Date(2026, 12, 1, 6, 30, 0, 0, jakarta),
		"mid month": time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
	} {
		if got := MonthStart(in); !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("%s: MonthStart = %v, want %v", name, got, want)
		}
	}
}

func TestUsedUp(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    Usage
		want bool
	}{
		{"no limits", Usage{Repository: 1 << 40, Instance: 1 << 40}, false},
		{"repository at its limit", Usage{Repository: 100, RepositoryLimit: i64(100)}, true},
		{"repository below its limit", Usage{Repository: 99, RepositoryLimit: i64(100)}, false},
		{"instance at its limit, no repository limit", Usage{Instance: 500, InstanceLimit: i64(500)}, true},
		{"repository has room, instance used up", Usage{Repository: 1, RepositoryLimit: i64(100), Instance: 500, InstanceLimit: i64(500)}, true},
		{"both have room", Usage{Repository: 1, RepositoryLimit: i64(100), Instance: 1, InstanceLimit: i64(500)}, false},
	} {
		if got := tc.u.UsedUp(); got != tc.want {
			t.Errorf("%s: UsedUp = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// With no limit, Check asks the database nothing; Read always does.
func TestCheckSkipsQueriesWithoutLimits(t *testing.T) {
	st := newStore(t)
	st.Close() // any query on it fails now
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	if used, err := Check(ctx, st, now, "main", 1, settings.Effective{}); err != nil || used {
		t.Errorf("Check with no limits = %v, %v; want false, nil", used, err)
	}
	// A repository limit with an unknown project (0) is not asked either.
	if used, err := Check(ctx, st, now, "main", 0, settings.Effective{MonthlyTokenQuota: i64(10)}); err != nil || used {
		t.Errorf("Check with a repository limit and project 0 = %v, %v; want false, nil", used, err)
	}
	if _, err := Check(ctx, st, now, "main", 1, settings.Effective{MonthlyTokenQuota: i64(10)}); err == nil {
		t.Error("Check with a limit on a closed store succeeded, want an error")
	}
	if _, err := Read(ctx, st, now, "main", 1, settings.Effective{}); err == nil {
		t.Error("Read on a closed store succeeded, want an error")
	}
}

// seedUsage stores a reported run of the given project and merge request, as
// the Job's result came in at at, using in+out tokens.
func seedUsage(t *testing.T, st *store.Store, instance string, projectID, iid int64, at time.Time, in, out int64) {
	t.Helper()
	mr := store.MRKey{Instance: instance, ProjectID: projectID, IID: iid}
	id, _, err := st.EnqueueRun(ctx, mr, fmt.Sprintf("acme/p%d", projectID), fmt.Sprintf("head%d", iid), "main")
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
	if err := st.RecordUsage(ctx, id, []store.SkillUsage{{Skill: "a", Verdict: "no_blocking_issues", In: in, Out: out, CacheRead: &zero, CacheWrite: &zero, Reasoning: &zero}}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
}

func TestCheckCountsThisMonth(t *testing.T) {
	st := newStore(t)
	at := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, time.UTC) }
	seedUsage(t, st, "main", 1, 1, at(11, 1, 0), 100, 50)
	seedUsage(t, st, "main", 1, 2, at(10, 31, 23), 7, 3) // last month: not counted
	seedUsage(t, st, "main", 2, 1, at(11, 5, 0), 1000, 0)
	seedUsage(t, st, "other", 1, 1, at(11, 2, 0), 5000, 5000) // another instance
	now := at(11, 15, 12)

	got, err := Read(ctx, st, now, "main", 1, settings.Effective{MonthlyTokenQuota: i64(150), InstanceMonthlyTokenQuota: i64(2000)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Repository != 150 || got.Instance != 1150 || *got.RepositoryLimit != 150 || *got.InstanceLimit != 2000 {
		t.Errorf("Read = %+v, want repository 150/150, instance 1150/2000", got)
	}
	if none, err := Read(ctx, st, now, "main", 0, settings.Effective{}); err != nil || none.Repository != 0 || none.Instance != 1150 {
		t.Errorf("Read with project 0 = %+v, %v; want repository 0, instance 1150", none, err)
	}

	for _, tc := range []struct {
		name string
		repo settings.Effective
		want bool
	}{
		{"repository limit equal to usage", settings.Effective{MonthlyTokenQuota: i64(150)}, true},
		{"repository limit above usage", settings.Effective{MonthlyTokenQuota: i64(151)}, false},
		{"instance limit equal to usage", settings.Effective{InstanceMonthlyTokenQuota: i64(1150)}, true},
		{"instance limit above usage", settings.Effective{InstanceMonthlyTokenQuota: i64(1151)}, false},
		{"repository has room, instance used up", settings.Effective{MonthlyTokenQuota: i64(151), InstanceMonthlyTokenQuota: i64(1150)}, true},
	} {
		if used, err := Check(ctx, st, now, "main", 1, tc.repo); err != nil || used != tc.want {
			t.Errorf("%s: Check = %v, %v; want %v", tc.name, used, err, tc.want)
		}
	}
}
