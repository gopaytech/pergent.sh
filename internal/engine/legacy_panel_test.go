package engine

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// legacyMember is one panel_members row a main build left.
type legacyMember struct{ ID, Role, State, JobName string }

// startLegacyPanel makes the claimed run id a legacy panel review, as a main
// build split it: the run waits (WaitPanel), dispatched at now, and members
// a and b are queued. The review panel's code is gone, so this is plain SQL.
// It returns the members, a first.
func startLegacyPanel(t *testing.T, env *testEnv, id string, now time.Time) []legacyMember {
	t.Helper()
	conn := legacyConn(t, env)
	defer conn.Close(env.ctx)
	tag, err := conn.Exec(env.ctx, `UPDATE runs SET state = 'waiting', skip_reason = $2, dispatched_at = $3,
		base_sha = 'base', start_sha = 'start' WHERE id = $1::uuid AND state = 'running'`, id, store.WaitPanel, now)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("make run %s wait for a legacy panel: %v, %d rows", id, err, tag.RowsAffected())
	}
	if _, err := conn.Exec(env.ctx, `INSERT INTO panel_members (run_id, role) VALUES ($1::uuid, 'a'), ($1::uuid, 'b')`, id); err != nil {
		t.Fatal(err)
	}
	return legacyMembers(t, env, id)
}

// legacyMembers is every panel member of the review whose first run is
// root, by role.
func legacyMembers(t *testing.T, env *testEnv, root string) []legacyMember {
	t.Helper()
	conn := legacyConn(t, env)
	defer conn.Close(env.ctx)
	rows, err := conn.Query(env.ctx, `SELECT id::text, role, state, job_name FROM panel_members
		WHERE run_id = $1::uuid ORDER BY role, created_at`, root)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []legacyMember
	for rows.Next() {
		var m legacyMember
		if err := rows.Scan(&m.ID, &m.Role, &m.State, &m.JobName); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// legacyConn opens a connection in the test's schema.
func legacyConn(t *testing.T, env *testEnv) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv(testDBEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.schema
	conn, err := pgx.ConnectConfig(env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// A review panel a main build left in flight: one warning, its member Jobs
// deleted, its run queued again with dispatched_at kept. A multi review is
// left alone. One start logs one warning.
func TestRecoverLegacyPanelsWarnsOnce(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	id := env.Enqueue(3, sha('a'))
	if _, err := env.Store.ClaimQueued(env.ctx, 10, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ms := startLegacyPanel(t, env, id, now)
	execSQL(t, env, `UPDATE panel_members SET state = 'running', job_name = 'legacy-job-a', dispatched_at = now()
		WHERE id = '`+ms[0].ID+`'`)
	if _, err := env.Kube.BatchV1().Jobs(testNamespace).Create(env.ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-job-a", Namespace: testNamespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	multi := env.Enqueue(4, sha('b'))
	if _, err := env.Store.ClaimQueued(env.ctx, 10, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	split := store.MultiReviewSplit{BaseSHA: "base", StartSHA: "start",
		Reviewers: []store.SplitReviewer{{Number: 1}, {Number: 2}}}
	if started, err := env.Store.StartMultiReview(env.ctx, multi, split, now); err != nil || !started {
		t.Fatalf("StartMultiReview = %v, %v", started, err)
	}
	before := env.Run(id)

	env.Engine.RecoverLegacyPanels(env.ctx)

	if r := env.Run(id); r.State != store.Queued || r.SkipReason != "" || !r.DispatchedAt.Equal(before.DispatchedAt) {
		t.Errorf("legacy run = %s %q dispatched %v; want queued, dispatched_at kept %v", r.State, r.SkipReason, r.DispatchedAt, before.DispatchedAt)
	}
	if r := env.Run(multi); r.State != store.Waiting || r.SkipReason != store.WaitReviewers {
		t.Errorf("multi review run = %s %q; want untouched", r.State, r.SkipReason)
	}
	if jobs := env.Jobs(); len(jobs) != 0 {
		t.Errorf("Jobs = %d left; want the legacy member Job deleted", len(jobs))
	}
	const warning = "review panel data from a main build is not used"
	if n := strings.Count(env.Logs(), warning); n != 1 {
		t.Errorf("warning logged %d times; want 1\n%s", n, env.Logs())
	}
}

// A legacy panel member still queued is never dispatched: the dispatcher
// starts no Job for it, and its run keeps waiting for the recovery at the
// next start (RecoverLegacyPanels). Another run is dispatched as before.
func TestDispatchIgnoresLegacyPanelMembers(t *testing.T) {
	env := newTestEnv(t)
	now := time.Now()
	id := env.Enqueue(3, sha('a'))
	if _, err := env.Store.ClaimQueued(env.ctx, 10, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	startLegacyPanel(t, env, id, now)
	env.GitLab.SetMR(openMR(4, sha('b')))
	env.GitLab.Reply(http.MethodPost, statusPath(sha('b')), http.StatusCreated, `{}`)
	other := env.Enqueue(4, sha('b'))

	dispatch(t, env)

	for _, m := range legacyMembers(t, env, id) {
		if m.State != string(store.MemberQueued) || m.JobName != "" {
			t.Errorf("legacy member %s = %s job %q; want queued with no Job", m.Role, m.State, m.JobName)
		}
	}
	if r := env.Run(id); r.State != store.Waiting || r.SkipReason != store.WaitPanel {
		t.Errorf("legacy run = %s %q; want still waiting", r.State, r.SkipReason)
	}
	if r := env.Run(other); r.State != store.Running || r.JobName == "" {
		t.Errorf("other run = %s job %q; want dispatched\n%s", r.State, r.JobName, env.Logs())
	}
	if n := len(env.Jobs()); n != 1 {
		t.Errorf("%d Jobs; want the other run's only", n)
	}
}
