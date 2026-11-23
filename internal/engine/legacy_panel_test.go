package engine

import (
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent.sh/internal/store"
)

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
	if started, err := env.Store.StartPanel(env.ctx, id, store.PanelSplit{BaseSHA: "base", StartSHA: "start"}, now); err != nil || !started {
		t.Fatalf("StartPanel = %v, %v", started, err)
	}
	ms, err := env.Store.ClaimQueuedMembers(env.ctx, 10, now.Add(time.Hour))
	if err != nil || len(ms) != 2 {
		t.Fatalf("ClaimQueuedMembers = %d, %v", len(ms), err)
	}
	if _, err := env.Store.MarkMemberDispatched(env.ctx, ms[0].ID, "legacy-job-a", []byte("hash"), now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
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
