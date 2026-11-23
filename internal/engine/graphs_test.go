package engine

import (
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gopaytech/pergent/graph"

	"github.com/gopaytech/pergent.sh/internal/codegraph"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const testGraphImage = "ghcr.io/gopaytech/pergent.sh-graph:v0.0.0-test"

// graphEnv is a test environment with graph storage on and acme/widgets a
// dashboard repository whose Code graph switch is on (unless off).
func graphEnv(t *testing.T, on bool) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	env.Engine.ScanImage = testScanImage
	env.Engine.GraphImage = testGraphImage
	env.Engine.GraphDir = t.TempDir()
	env.Engine.Config.Repositories = nil
	dashboardRepo(t, env, store.DashboardRepo{Instance: testInstance, Project: testProjectPath, CreatedBy: "alice"})
	if on {
		if _, _, err := env.Store.SetDashboardRepoGraph(env.ctx, testInstance, testProjectPath, true); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

// testGraphFile is a small valid graph of commit.
func testGraphFile(t *testing.T, commit string) []byte {
	t.Helper()
	f := graph.File{
		Format: graph.Format, Provider: "graphify", ProviderVersion: "0.9.112", Commit: commit,
		BuiltAt: "2026-11-20T10:00:00Z",
		Nodes: []graph.Node{
			{ID: "n1", Kind: graph.KindFunction, Name: "Dispatch", File: "internal/engine/dispatch.go", Line: 10},
			{ID: "n2", Kind: graph.KindFunction, Name: "Run", File: "internal/engine/engine.go", Line: 5},
		},
		Edges: []graph.Edge{{From: "n2", To: "n1", Kind: graph.EdgeCalls}},
	}
	b, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// startedBuild queues, claims and starts a build of commit of branch for
// acme/widgets, as the loop would, and returns its id and token.
func startedBuild(t *testing.T, env *testEnv, branch, commit string) (id, token string) {
	t.Helper()
	if _, err := env.Store.TakeGraphCheck(env.ctx, testInstance, testProjectPath, env.Now().Add(-time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok, err := env.Store.QueueGraphBuild(env.ctx, testInstance, testProjectPath, branch, commit, env.Now()); err != nil || !ok {
		t.Fatalf("QueueGraphBuild = %v, %v", ok, err)
	}
	g, ok, err := env.Store.ClaimGraphBuild(env.ctx, 1, env.Now().Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("ClaimGraphBuild = %v, %v", ok, err)
	}
	token, hash, err := mintToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Store.StartGraphJob(env.ctx, g.BuildID, kube.GraphJobName(g.BuildID), hash, env.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return g.BuildID, token
}

// storeTestGraph stores a graph of commit of branch through CompleteGraph.
func storeTestGraph(t *testing.T, env *testEnv, branch, commit string) {
	t.Helper()
	id, token := startedBuild(t, env, branch, commit)
	if out, err := env.Engine.CompleteGraph(env.ctx, id, token, testGraphFile(t, commit)); err != nil || out != codegraph.Stored {
		t.Fatalf("CompleteGraph = %v, %v", out, err)
	}
}

func repoGraph(t *testing.T, env *testEnv) (store.RepoGraph, bool) {
	t.Helper()
	g, ok, err := env.Store.RepoGraph(env.ctx, testInstance, testProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	return g, ok
}

func hasGraph(job *batchv1.Job) bool {
	spec := job.Spec.Template.Spec
	for _, c := range spec.InitContainers {
		if c.Name == "graph" {
			return true
		}
	}
	for _, a := range spec.Containers[0].Args {
		if strings.HasPrefix(a, "--graph") {
			return true
		}
	}
	for _, e := range spec.Containers[0].Env {
		if e.Name == "PERGENT_GRAPH" {
			return true
		}
	}
	for _, v := range spec.Volumes {
		if v.Name == "graph" {
			return true
		}
	}
	return false
}

// With the switch off nothing on the review path changes: the Job has no
// graph, no row is read or made, and no head check is asked for, even with
// graph storage on.
func TestDispatchGraphOff(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, false)
	id, _ := queue(env)
	dispatch(t, env)
	job := env.Job(id)
	if job == nil {
		t.Fatalf("no job; run = %+v\n%s", env.Run(id), env.Logs())
	}
	if hasGraph(job) {
		t.Errorf("a review with the switch off got the graph")
	}
	if _, ok := repoGraph(t, env); ok {
		t.Errorf("a repo_graphs row was made")
	}
	if n := len(env.Engine.graphWantCh()); n != 0 {
		t.Errorf("%d head checks asked for", n)
	}
}

// With the switch on, a review asks for a head check, and a change into the
// branch the stored graph was built from gets the graph init container and
// PERGENT_GRAPH; a change into another branch does not.
func TestDispatchGraphOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, branch string
		stored, want bool
	}{
		{"graph of the target branch", "main", true, true},
		{"graph of another branch", "release", true, false},
		{"no graph yet", "main", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := graphEnv(t, true)
			if tc.stored {
				storeTestGraph(t, env, tc.branch, sha('d'))
			}
			id, _ := queue(env) // targets main
			dispatch(t, env)
			job := env.Job(id)
			if job == nil {
				t.Fatalf("no job; run = %+v\n%s", env.Run(id), env.Logs())
			}
			if got := hasGraph(job); got != tc.want {
				t.Fatalf("graph in the Job = %v; want %v", got, tc.want)
			}
			if tc.want {
				g := job.Spec.Template.Spec.InitContainers[1]
				wantURL := "--url=" + strings.TrimRight(env.Engine.Config.CallbackURL, "/") + "/runs/" + id + "/graph"
				if g.Image != testScanImage || !slices.Contains(g.Args, wantURL) {
					t.Errorf("graph container = %s %v; want %s", g.Image, g.Args, wantURL)
				}
			}
			select {
			case k := <-env.Engine.graphWantCh():
				if k.Instance != testInstance || k.Project != testProjectPath {
					t.Errorf("head check asked for %+v", k)
				}
			default:
				t.Error("no head check asked for")
			}
		})
	}
}

// Without graph storage in the install, a repository with the switch on gets
// no graph and asks for nothing.
func TestDispatchGraphStorageOff(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, true)
	env.Engine.GraphDir = ""
	id, _ := queue(env)
	dispatch(t, env)
	if job := env.Job(id); job == nil || hasGraph(job) {
		t.Fatalf("job = %v; want one with no graph", job != nil)
	}
	if n := len(env.Engine.graphWantCh()); n != 0 {
		t.Errorf("%d head checks asked for", n)
	}
}

// A good graph is stored encoded again, in a file named by a hash, and the
// row says what it holds.
func TestCompleteGraphStores(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, true)
	id, token := startedBuild(t, env, "main", sha('d'))
	if out, err := env.Engine.CompleteGraph(env.ctx, id, "wrong", testGraphFile(t, sha('d'))); err != nil || out != codegraph.Unauthorized {
		t.Fatalf("wrong token = %v, %v", out, err)
	}
	// Extra spaces: what is stored is pergent.sh's own encoding.
	spaced := []byte(" " + string(testGraphFile(t, sha('d'))) + "  ")
	if out, err := env.Engine.CompleteGraph(env.ctx, id, token, spaced); err != nil || out != codegraph.Stored {
		t.Fatalf("CompleteGraph = %v, %v", out, err)
	}
	path := env.Engine.graphPath(testInstance, testProjectPath)
	if strings.Contains(path, "acme") || !strings.HasPrefix(path, env.Engine.GraphDir) {
		t.Errorf("path = %s; want a hash under the graphs dir", path)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != string(testGraphFile(t, sha('d'))) {
		t.Fatalf("file = %q, %v", b, err)
	}
	g, _ := repoGraph(t, env)
	if g.Commit != sha('d') || g.Branch != "main" || g.SizeBytes != int64(len(b)) || g.Nodes != 2 || g.Edges != 1 ||
		g.BuildState != store.GraphBuildNone || g.Error != "" {
		t.Errorf("row = %+v", g)
	}
	// The token is spent with the build.
	if out, _ := env.Engine.CompleteGraph(env.ctx, id, token, testGraphFile(t, sha('d'))); out != codegraph.Unauthorized {
		t.Errorf("a second post = %v; want unauthorized", out)
	}
}

// A graph of another commit, a body that is not a graph, and an error are
// each a failed build in pergent.sh's words; the stored graph and its file
// stay.
func TestCompleteGraphFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body func(t *testing.T) []byte
		want string
	}{
		{"another commit", func(t *testing.T) []byte { return testGraphFile(t, sha('e')) }, codegraph.MsgWrongHead},
		{"not a graph", func(*testing.T) []byte { return []byte(`{"format":"pergent-graph/v1","nodes":"x"}`) }, codegraph.MsgUnusable},
		{"free text in a name", func(t *testing.T) []byte {
			return []byte(strings.Replace(string(testGraphFile(t, sha('f'))), `"Dispatch"`, `"password = hunter2"`, 1))
		}, codegraph.MsgUnusable},
		{"known error", func(*testing.T) []byte { return []byte(`{"error":"` + codegraph.MsgGraphify + `"}`) }, codegraph.MsgGraphify},
		{"unknown error", func(*testing.T) []byte { return []byte(`{"error":"token ghs_abc in the log"}`) }, codegraph.MsgFailed},
		{"error with extra fields", func(*testing.T) []byte { return []byte(`{"error":"x","more":1}`) }, codegraph.MsgUnusable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := graphEnv(t, true)
			storeTestGraph(t, env, "main", sha('d'))
			before, err := os.ReadFile(env.Engine.graphPath(testInstance, testProjectPath))
			if err != nil {
				t.Fatal(err)
			}
			id, token := startedBuild(t, env, "main", sha('f'))
			if out, err := env.Engine.CompleteGraph(env.ctx, id, token, tc.body(t)); err != nil || out != codegraph.Stored {
				t.Fatalf("CompleteGraph = %v, %v", out, err)
			}
			g, _ := repoGraph(t, env)
			if g.Error != tc.want || g.Commit != sha('d') || g.BuildState != store.GraphBuildNone {
				t.Errorf("row = %+v; want error %q and the old graph", g, tc.want)
			}
			if after, _ := os.ReadFile(env.Engine.graphPath(testInstance, testProjectPath)); string(after) != string(before) {
				t.Errorf("the stored file changed")
			}
		})
	}
}

// A build that ends after the switch went off writes nothing.
func TestCompleteGraphAfterSwitchOff(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, true)
	id, token := startedBuild(t, env, "main", sha('d'))
	if _, _, err := env.Store.SetDashboardRepoGraph(env.ctx, testInstance, testProjectPath, false); err != nil {
		t.Fatal(err)
	}
	if out, err := env.Engine.CompleteGraph(env.ctx, id, token, testGraphFile(t, sha('d'))); err != nil || out != codegraph.Stored {
		t.Fatalf("CompleteGraph = %v, %v", out, err)
	}
	if _, err := os.Stat(env.Engine.graphPath(testInstance, testProjectPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file was written: %v", err)
	}
	if _, ok := repoGraph(t, env); ok {
		t.Errorf("the row is still there")
	}
}

// DropGraph deletes the row, the running build's Job and the file.
func TestDropGraph(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, true)
	storeTestGraph(t, env, "main", sha('d'))
	id, _ := startedBuild(t, env, "main", sha('e'))
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: kube.GraphJobName(id), Namespace: testNamespace}}
	if _, err := env.Kube.BatchV1().Jobs(testNamespace).Create(env.ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	env.Engine.DropGraph(env.ctx, testInstance, "ACME/Widgets")
	if _, ok := repoGraph(t, env); ok {
		t.Error("the row is still there")
	}
	if _, err := os.Stat(env.Engine.graphPath(testInstance, testProjectPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file is still there: %v", err)
	}
	if _, err := env.Kube.BatchV1().Jobs(testNamespace).Get(env.ctx, job.Name, metav1.GetOptions{}); err == nil {
		t.Error("the build Job is still there")
	}
}

// resultToken is the result token in the Secret of run id's Job.
func resultToken(t *testing.T, env *testEnv, id string) string {
	t.Helper()
	s, err := env.Kube.CoreV1().Secrets(testNamespace).Get(env.ctx, kube.JobName(id)+"-secrets", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return string(s.Data["result-token"])
}

// A running review gets its own repository's graph with its result token,
// which stays usable for the result. A wrong token, the switch off, or no
// graph is codegraph.ErrNoGraph.
func TestRunGraph(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, true)
	storeTestGraph(t, env, "main", sha('d'))
	id, _ := queue(env)
	dispatch(t, env)
	token := resultToken(t, env, id)

	if _, err := env.Engine.RunGraph(env.ctx, id, "wrong"); !errors.Is(err, codegraph.ErrNoGraph) {
		t.Errorf("wrong token = %v", err)
	}
	f, err := env.Engine.RunGraph(env.ctx, id, token)
	if err != nil {
		t.Fatalf("RunGraph = %v", err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != string(testGraphFile(t, sha('d'))) {
		t.Errorf("served %q", b)
	}
	if r := env.Run(id); r.State != store.Running {
		t.Errorf("run = %s; the download changed it", r.State)
	}

	if _, _, err := env.Store.SetDashboardRepoGraph(env.ctx, testInstance, testProjectPath, false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Engine.RunGraph(env.ctx, id, token); !errors.Is(err, codegraph.ErrNoGraph) {
		t.Errorf("switch off = %v", err)
	}
}

// The head check reads the default branch at most every 10 minutes, queues a
// build of a new head, and the loop starts its Job, which clones that commit
// and runs graphify with no secret.
func TestGraphHeadCheckAndLaunch(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, true)
	serveScanRepo(env, testProjectID, testProjectPath, sha('c'))
	env.Engine.checkGraphHead(env.ctx, testInstance, testProjectPath)
	g, ok := repoGraph(t, env)
	if !ok || g.BuildState != store.GraphBuildQueued || g.BuildCommit != sha('c') || g.BuildBranch != "main" {
		t.Fatalf("after the check = %+v, %v", g, ok)
	}
	reads := len(env.GitLab.Requests(http.MethodGet, "/projects/7/repository/commits/main"))
	env.Engine.checkGraphHead(env.ctx, testInstance, testProjectPath)
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/repository/commits/main")); n != reads {
		t.Errorf("a second check within 10 minutes read the head again")
	}

	env.Engine.launchGraphs(env.ctx)
	l, err := env.Kube.BatchV1().Jobs(testNamespace).List(env.ctx, metav1.ListOptions{LabelSelector: "pergent.sh/graph-build"})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("graph Jobs = %d, %v\n%s", len(l.Items), err, env.Logs())
	}
	spec := l.Items[0].Spec.Template.Spec
	clone := spec.InitContainers[0]
	var commit string
	for _, e := range clone.Env {
		if e.Name == "SCAN_COMMIT" {
			commit = e.Value
		}
	}
	if commit != sha('c') || spec.InitContainers[1].Image != testGraphImage || spec.Containers[0].Image != testScanImage {
		t.Errorf("Job = commit %s, images %s, %s", commit, spec.InitContainers[1].Image, spec.Containers[0].Image)
	}
	if g, _ := repoGraph(t, env); g.BuildState != store.GraphBuildRunning || g.BuildJob != l.Items[0].Name {
		t.Errorf("row = %+v", g)
	}
}

// A head check of a repository with the switch off makes no row and reads
// nothing.
func TestGraphHeadCheckOff(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, false)
	serveScanRepo(env, testProjectID, testProjectPath, sha('c'))
	env.Engine.checkGraphHead(env.ctx, testInstance, testProjectPath)
	if _, ok := repoGraph(t, env); ok {
		t.Error("a row was made")
	}
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/repository/commits/main")); n != 0 {
		t.Errorf("the head was read %d times", n)
	}
}

// A build past its deadline fails, and its Job goes.
func TestSweepGraphs(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, true)
	id, _ := startedBuild(t, env, "main", sha('d'))
	env.SetNow(env.Now().Add(2 * time.Hour))
	env.Engine.sweepGraphs(env.ctx)
	g, _ := repoGraph(t, env)
	if g.BuildState != store.GraphBuildNone || g.Error != codegraph.MsgTimedOut {
		t.Errorf("row = %+v; build %s", g, id)
	}
}

// Pausing a repository keeps its graph; removing it deletes the graph.
func TestReconcileGraphPauseAndRemove(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, true)
	serveProject(env, testProjectID, testProjectPath)
	reconcile(t, env)
	storeTestGraph(t, env, "main", sha('d'))
	path := env.Engine.graphPath(testInstance, testProjectPath)

	pause(t, env, testProjectPath, false)
	env.GitLab.Reply(http.MethodDelete, "/projects/7/hooks/31", http.StatusNoContent, nil)
	env.GitLab.Reply(http.MethodDelete, "/projects/7/deploy_tokens/42", http.StatusNoContent, nil)
	reconcile(t, env)
	if _, ok := repoGraph(t, env); !ok {
		t.Fatal("the pause deleted the graph's row")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the pause deleted the graph file: %v", err)
	}

	if found, err := env.Store.RemoveDashboardRepo(env.ctx, testInstance, testProjectPath); err != nil || !found {
		t.Fatalf("RemoveDashboardRepo = %v, %v", found, err)
	}
	reconcile(t, env)
	if _, ok := repoGraph(t, env); ok {
		t.Error("the removal kept the graph's row")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the removal kept the graph file: %v", err)
	}
}

// A graph as graphify 0.9.112 and pergent's converter really write it (of
// internal/kube, at a made-up commit) is taken and stored.
func TestCompleteGraphConverterOutput(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("testdata/graph-converted.json")
	if err != nil {
		t.Fatal(err)
	}
	env := graphEnv(t, true)
	id, token := startedBuild(t, env, "main", "0123456789abcdef0123456789abcdef01234567")
	if out, err := env.Engine.CompleteGraph(env.ctx, id, token, b); err != nil || out != codegraph.Stored {
		t.Fatalf("CompleteGraph = %v, %v", out, err)
	}
	g, _ := repoGraph(t, env)
	if g.Error != "" || g.Nodes != 225 || g.Edges != 541 {
		t.Errorf("row = %+v", g)
	}
}

// A paused repository keeps its graph: a build that ends while it is paused
// still stores, a queued one is not started (and nothing is deleted), and no
// head check reads the platform.
func TestGraphWhilePaused(t *testing.T) {
	t.Parallel()
	env := graphEnv(t, true)
	storeTestGraph(t, env, "main", sha('d'))
	id, token := startedBuild(t, env, "main", sha('e'))
	pause(t, env, testProjectPath, false)
	if out, err := env.Engine.CompleteGraph(env.ctx, id, token, testGraphFile(t, sha('e'))); err != nil || out != codegraph.Stored {
		t.Fatalf("CompleteGraph = %v, %v", out, err)
	}
	g, ok := repoGraph(t, env)
	if !ok || g.Commit != sha('e') {
		t.Fatalf("after a paused build = %+v, %v; want the new graph stored", g, ok)
	}
	if ok, err := env.Store.QueueGraphBuild(env.ctx, testInstance, testProjectPath, "main", sha('f'), env.Now()); err != nil || !ok {
		t.Fatalf("QueueGraphBuild = %v, %v", ok, err)
	}
	env.Engine.launchGraphs(env.ctx)
	g, ok = repoGraph(t, env)
	if !ok || g.Commit != sha('e') || g.BuildState != store.GraphBuildNone {
		t.Errorf("after a launch while paused = %+v, %v; want the graph kept, the build dropped", g, ok)
	}
	if _, err := os.Stat(env.Engine.graphPath(testInstance, testProjectPath)); err != nil {
		t.Errorf("the file is gone: %v", err)
	}
	serveScanRepo(env, testProjectID, testProjectPath, sha('c'))
	env.Engine.checkGraphHead(env.ctx, testInstance, testProjectPath)
	if n := len(env.GitLab.Requests(http.MethodGet, "/projects/7/repository/commits/main")); n != 0 {
		t.Errorf("a paused repository's head was read %d times", n)
	}
}
