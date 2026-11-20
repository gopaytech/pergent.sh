package store

import (
	"crypto/sha256"
	"testing"
	"time"
)

// The Code graph switch is off on a new row, ignores the case of the path,
// survives settings writes, and says when nothing changed.
func TestDashboardRepoGraphSwitch(t *testing.T) {
	s := newStore(t)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/widgets", CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := s.DashboardRepo(ctx, "main", "acme/widgets"); r.GraphContext {
		t.Fatalf("a new row = %+v; want the graph off", r)
	}
	found, changed, err := s.SetDashboardRepoGraph(ctx, "main", "ACME/Widgets", true)
	if err != nil || !found || !changed {
		t.Fatalf("on = %v, %v, %v; want found and changed", found, changed, err)
	}
	if found, changed, err = s.SetDashboardRepoGraph(ctx, "main", "acme/widgets", true); err != nil || !found || changed {
		t.Fatalf("on again = %v, %v, %v; want found, not changed", found, changed, err)
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/widgets", QuietReviews: ptrTo(true)}); err != nil {
		t.Fatal(err)
	}
	if found, err := s.UpdateDashboardRepo(ctx, "main", "acme/widgets", func(r *DashboardRepo) { r.CheckDeleted = ptrTo(true) }); err != nil || !found {
		t.Fatalf("update = %v, %v", found, err)
	}
	if r, _, _ := s.DashboardRepo(ctx, "main", "acme/widgets"); !r.GraphContext {
		t.Fatalf("after settings writes = %+v; want the graph still on", r)
	}
	// A paused row keeps it and can change it.
	if _, _, err := s.SetDashboardRepoEnabled(ctx, "main", "acme/widgets", false); err != nil {
		t.Fatal(err)
	}
	if found, changed, err = s.SetDashboardRepoGraph(ctx, "main", "acme/widgets", false); err != nil || !found || !changed {
		t.Fatalf("off on a paused row = %v, %v, %v; want found and changed", found, changed, err)
	}
	if r, _, _ := s.AnyDashboardRepo(ctx, "main", "acme/widgets"); r.GraphContext {
		t.Fatalf("after off = %+v; want off", r)
	}
	if found, _, err := s.SetDashboardRepoGraph(ctx, "main", "acme/none", true); err != nil || found {
		t.Fatalf("no such row = %v, %v; want not found", found, err)
	}
}

// A head check is taken once per gap, and makes the row.
func TestTakeGraphCheck(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	gap := 10 * time.Minute
	if ok, err := s.TakeGraphCheck(ctx, "main", "acme/widgets", now, gap); err != nil || !ok {
		t.Fatalf("first check = %v, %v; want taken", ok, err)
	}
	if ok, err := s.TakeGraphCheck(ctx, "main", "ACME/widgets", now.Add(9*time.Minute), gap); err != nil || ok {
		t.Fatalf("check within the gap = %v, %v; want not taken", ok, err)
	}
	if ok, err := s.TakeGraphCheck(ctx, "main", "acme/widgets", now.Add(10*time.Minute), gap); err != nil || !ok {
		t.Fatalf("check after the gap = %v, %v; want taken", ok, err)
	}
	g, found, err := s.RepoGraph(ctx, "main", "Acme/Widgets")
	if err != nil || !found || g.Project != "acme/widgets" || !g.CheckedAt.Equal(now.Add(10*time.Minute)) || g.Commit != "" ||
		g.BuildState != GraphBuildNone {
		t.Fatalf("row = %+v, %v, %v", g, found, err)
	}
	// Another repository has its own gap.
	if ok, err := s.TakeGraphCheck(ctx, "main", "acme/gadgets", now.Add(11*time.Minute), gap); err != nil || !ok {
		t.Fatalf("another repository = %v, %v; want taken", ok, err)
	}
}

// One build per repository: a queued build takes a newer commit and keeps its
// place, a running one is left alone; claims respect the cap; only the
// build's token stores it; a failure keeps the stored graph.
func TestGraphBuildLifecycle(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if ok, _ := s.QueueGraphBuild(ctx, "main", "acme/widgets", "main", "aaa", now); ok {
		t.Fatal("queued a build with no row")
	}
	for _, p := range []string{"acme/widgets", "acme/gadgets"} {
		if _, err := s.TakeGraphCheck(ctx, "main", p, now, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.QueueGraphBuild(ctx, "main", "acme/widgets", "main", "aaa", now); err != nil || !ok {
		t.Fatalf("queue = %v, %v", ok, err)
	}
	first, _, _ := s.RepoGraph(ctx, "main", "acme/widgets")
	if ok, _ := s.QueueGraphBuild(ctx, "main", "acme/widgets", "main", "aaa", now.Add(time.Second)); ok {
		t.Fatal("the same commit again changed the queued build")
	}
	if ok, _ := s.QueueGraphBuild(ctx, "main", "acme/widgets", "main", "bbb", now.Add(2*time.Second)); !ok {
		t.Fatal("a newer commit didn't replace the queued one")
	}
	g, _, _ := s.RepoGraph(ctx, "main", "acme/widgets")
	if g.BuildState != GraphBuildQueued || g.BuildCommit != "bbb" || g.BuildID != first.BuildID || !g.QueuedAt.Equal(now) {
		t.Fatalf("queued build = %+v; want bbb with the first id and time", g)
	}
	if ok, _ := s.QueueGraphBuild(ctx, "main", "acme/gadgets", "trunk", "ccc", now.Add(time.Minute)); !ok {
		t.Fatal("queue gadgets")
	}

	// The cap is 1: the oldest is claimed, then nothing while it runs.
	claimed, ok, err := s.ClaimGraphBuild(ctx, 1, now.Add(time.Hour))
	if err != nil || !ok || claimed.Project != "acme/widgets" || claimed.BuildState != GraphBuildRunning || claimed.BuildCommit != "bbb" {
		t.Fatalf("claim = %+v, %v, %v", claimed, ok, err)
	}
	if _, ok, _ := s.ClaimGraphBuild(ctx, 1, now.Add(time.Hour)); ok {
		t.Fatal("claimed a second build over the cap")
	}
	if ok, _ := s.QueueGraphBuild(ctx, "main", "acme/widgets", "main", "ddd", now); ok {
		t.Fatal("a running build took a newer commit")
	}

	hash := sha256.Sum256([]byte("token"))
	if err := s.StartGraphJob(ctx, claimed.BuildID, "pergent-graph-1", hash[:], now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.StartGraphJob(ctx, first.BuildID[:35]+"0", "x", hash[:], now); err != ErrGraphBuildNotFound {
		t.Fatalf("start of another build = %v", err)
	}
	wrong := sha256.Sum256([]byte("other"))
	if _, ok, _ := s.GraphBuild(ctx, claimed.BuildID, wrong[:]); ok {
		t.Fatal("found the build with the wrong token")
	}
	if b, ok, err := s.GraphBuild(ctx, claimed.BuildID, hash[:]); err != nil || !ok || b.BuildJob != "pergent-graph-1" {
		t.Fatalf("GraphBuild = %+v, %v, %v", b, ok, err)
	}
	if ok, _ := s.StoreGraph(ctx, claimed.BuildID, wrong[:], StoredGraph{SizeBytes: 1}, now); ok {
		t.Fatal("stored with the wrong token")
	}
	if ok, err := s.StoreGraph(ctx, claimed.BuildID, hash[:], StoredGraph{SizeBytes: 123, Nodes: 4, Edges: 5}, now.Add(3*time.Minute)); err != nil || !ok {
		t.Fatalf("store = %v, %v", ok, err)
	}
	if ok, _ := s.StoreGraph(ctx, claimed.BuildID, hash[:], StoredGraph{}, now); ok {
		t.Fatal("stored twice")
	}
	g, _, _ = s.RepoGraph(ctx, "main", "acme/widgets")
	if g.Branch != "main" || g.Commit != "bbb" || g.SizeBytes != 123 || g.Nodes != 4 || g.Edges != 5 ||
		!g.BuiltAt.Equal(now.Add(3*time.Minute)) || g.BuildState != GraphBuildNone || g.BuildID != "" || g.BuildJob != "" {
		t.Fatalf("stored = %+v", g)
	}

	// A failed build keeps the stored graph and records the error; the next
	// stored graph clears it.
	if ok, _ := s.QueueGraphBuild(ctx, "main", "acme/widgets", "main", "eee", now.Add(2*time.Minute)); !ok {
		t.Fatal("queue after store")
	}
	if _, ok, _ := s.ClaimGraphBuild(ctx, 2, now.Add(time.Hour)); !ok { // gadgets
		t.Fatal("claim gadgets")
	}
	next, ok, _ := s.ClaimGraphBuild(ctx, 2, now.Add(-time.Minute))
	if !ok || next.Project != "acme/widgets" {
		t.Fatalf("claim widgets = %+v, %v", next, ok)
	}
	expired, err := s.ExpiredGraphBuilds(ctx, now)
	if err != nil || len(expired) != 1 || expired[0].BuildID != next.BuildID {
		t.Fatalf("expired = %+v, %v; want widgets alone", expired, err)
	}
	if ok, err := s.FailGraphBuild(ctx, next.BuildID, "the graph build took too long", now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("fail = %v, %v", ok, err)
	}
	if ok, _ := s.FailGraphBuild(ctx, next.BuildID, "again", now); ok {
		t.Fatal("failed an ended build")
	}
	g, _, _ = s.RepoGraph(ctx, "main", "acme/widgets")
	if g.Commit != "bbb" || g.Error != "the graph build took too long" || g.BuildState != GraphBuildNone || g.FailedAt.IsZero() {
		t.Fatalf("after the failure = %+v", g)
	}

	job, found, err := s.DeleteRepoGraph(ctx, "main", "ACME/gadgets")
	if err != nil || !found || job != "" {
		t.Fatalf("delete gadgets = %q, %v, %v", job, found, err)
	}
	if _, found, _ := s.RepoGraph(ctx, "main", "acme/gadgets"); found {
		t.Fatal("gadgets row still there")
	}
	if _, found, _ := s.DeleteRepoGraph(ctx, "main", "acme/gadgets"); found {
		t.Fatal("deleted twice")
	}
}

// DeleteRepoGraph returns the running build's Job, so it can be stopped.
func TestDeleteRepoGraphReturnsJob(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if _, err := s.TakeGraphCheck(ctx, "main", "acme/widgets", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueGraphBuild(ctx, "main", "acme/widgets", "main", "aaa", now); err != nil {
		t.Fatal(err)
	}
	b, _, _ := s.ClaimGraphBuild(ctx, 1, now.Add(time.Hour))
	hash := sha256.Sum256([]byte("token"))
	if err := s.StartGraphJob(ctx, b.BuildID, "pergent-graph-abc", hash[:], now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	job, found, err := s.DeleteRepoGraph(ctx, "main", "acme/widgets")
	if err != nil || !found || job != "pergent-graph-abc" {
		t.Fatalf("delete = %q, %v, %v", job, found, err)
	}
	if ok, _ := s.StoreGraph(ctx, b.BuildID, hash[:], StoredGraph{}, now); ok {
		t.Fatal("a build of a deleted row stored a graph")
	}
}
