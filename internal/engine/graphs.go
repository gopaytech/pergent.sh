package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gopaytech/pergent/graph"

	"github.com/gopaytech/pergent.sh/internal/codegraph"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The code graph (docs/concepts/code-graph.md). It is off unless the install
// has graph storage (GraphsOn) and a repository's Code graph switch is on;
// with the switch off nothing here runs for that repository, and dispatch
// builds the Job of before. A review of a repository with the switch on asks
// for a head check (WantGraph), which never blocks it. The graph loop
// (RunGraphs), in its own goroutine, reads the default branch head at most
// every graphCheckGap, queues a build when the stored graph is of another
// commit, and runs one build Job at a time. The Job posts the graph
// (CompleteGraph), which is checked, encoded again and kept on the graphs
// volume, newest only. A review of a change into that branch downloads it
// (RunGraph) into its Job.

const (
	// graphCheckGap is the least time between two reads of a repository's
	// default branch head.
	graphCheckGap = 10 * time.Minute
	// graphCap is how many build Jobs run at once in the install.
	graphCap = 1
	// graphPodBudget bounds what the pod does before the Job: reading the
	// clone target and creating the Job.
	graphPodBudget = time.Minute
	// graphGrace is how much longer than the Job's deadline its post is
	// waited for.
	graphGrace = time.Minute
	// graphWantQueue is how many head checks can wait; one more is dropped,
	// and the next review asks again.
	graphWantQueue = 256
)

var (
	// graphPassInterval is how often queued builds are looked for without a
	// wake-up, graphSweepInterval how often expired builds are failed.
	graphPassInterval  = 10 * time.Second
	graphSweepInterval = time.Minute
)

// GraphsOn reports whether this install can build and keep code graphs: the
// chart set the graphs volume and the graph image, and pergent.sh's own image
// is known (the build posts with it, and a review downloads with it).
func (e *Engine) GraphsOn() bool {
	return e.GraphDir != "" && e.GraphImage != "" && e.ScanImage != "" && e.Launcher != nil
}

// graphPath is the file of instance's repository project: the SHA-256 of the
// instance and the lower-cased path, so no name from a platform is a path.
func (e *Engine) graphPath(instance, project string) string {
	h := sha256.Sum256([]byte(instance + "\x00" + strings.ToLower(project)))
	return filepath.Join(e.GraphDir, hex.EncodeToString(h[:])+".json")
}

func (e *Engine) graphWantCh() chan RepoKey {
	e.graphOnce.Do(func() { e.graphWant = make(chan RepoKey, graphWantQueue) })
	return e.graphWant
}

// WantGraph asks the graph loop for a head check of instance's repository
// project. It never blocks: with the queue full the ask is dropped.
func (e *Engine) WantGraph(instance, project string) {
	if !e.GraphsOn() {
		return
	}
	select {
	case e.graphWantCh() <- RepoKey{Instance: instance, Project: project}:
	default:
	}
}

// RunGraphs runs the graph loop until ctx is done. Without GraphsOn it
// returns at once.
func (e *Engine) RunGraphs(ctx context.Context) {
	if !e.GraphsOn() {
		return
	}
	e.sweepGraphs(ctx)
	e.launchGraphs(ctx)
	pass := time.NewTicker(graphPassInterval)
	defer pass.Stop()
	sweep := time.NewTicker(graphSweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case k := <-e.graphWantCh():
			e.checkGraphHead(ctx, k.Instance, k.Project)
			e.launchGraphs(ctx)
		case <-pass.C:
			e.launchGraphs(ctx)
		case <-sweep.C:
			e.sweepGraphs(ctx)
			e.launchGraphs(ctx)
		}
	}
}

// graphOn reports whether instance's repository project has its Code graph
// switch on, paused or not (a paused repository keeps its graph), and whether
// it is paused. A repository that is gone, or comes from the config file, is
// off.
func (e *Engine) graphOn(ctx context.Context, instance, project string) (on, paused bool, err error) {
	eff, ok, err := e.settings().GetAny(ctx, instance, project)
	if err != nil {
		return false, false, err
	}
	return ok && eff.GraphContext, ok && eff.Paused, nil
}

// checkGraphHead reads the default branch head of a repository with the
// switch on, at most every graphCheckGap, and queues a build when the stored
// graph is of another commit or there is none. A failure is logged; the next
// check after the gap tries again.
func (e *Engine) checkGraphHead(ctx context.Context, instance, project string) {
	log := e.log().With("instance", instance, "project", project)
	if on, paused, err := e.graphOn(ctx, instance, project); err != nil || !on || paused {
		if err != nil {
			log.Info("code graph: couldn't read the repository's settings", "error", e.redact(err.Error()))
		}
		return
	}
	due, err := e.Store.TakeGraphCheck(ctx, instance, project, e.now(), graphCheckGap)
	if err != nil || !due {
		if err != nil {
			log.Info("code graph: couldn't record the head check", "error", e.redact(err.Error()))
		}
		return
	}
	t, err := e.graphTarget(ctx, instance, project, false)
	if err != nil {
		log.Info("code graph: couldn't read the default branch", "reason", err.Error())
		return
	}
	g, _, err := e.Store.RepoGraph(ctx, instance, project)
	if err != nil {
		log.Info("code graph: couldn't read the stored graph", "error", e.redact(err.Error()))
		return
	}
	if g.Commit == t.Commit && g.Branch == t.Branch {
		return
	}
	queued, err := e.Store.QueueGraphBuild(ctx, instance, project, t.Branch, t.Commit, e.now())
	if err != nil {
		log.Info("code graph: couldn't queue the build", "error", e.redact(err.Error()))
		return
	}
	if queued {
		log.Info("code graph: build queued", "branch", t.Branch, "commit", t.Commit)
	}
}

// graphTarget is the default branch of instance's repository project, its
// head, and where the Job clones it. With token it also holds the read-only
// clone token (dispatch's: the GitLab deploy token, or a GitHub token for
// this one repository), which goes to the clone container only. The error is
// one of readiness's reasons; what the platform answered is logged.
func (e *Engine) graphTarget(ctx context.Context, instance, project string, token bool) (scanTarget, error) {
	log := e.log().With("instance", instance, "project", project)
	p, found, err := e.Store.ProjectByPath(ctx, instance, project)
	switch {
	case err != nil:
		log.Info("code graph: couldn't load the project", "error", e.redact(err.Error()))
		return scanTarget{}, errDefaultBranch
	case !found:
		return scanTarget{}, errNotSetUp
	}
	plat, ok := e.Platforms.Get(instance)
	switch {
	case ok && plat.isGitHub() && plat.GitHub != nil && plat.GitHub.App != nil:
		return e.gitHubGraphTarget(ctx, plat.GitHub.App, p, token)
	case ok && !plat.isGitHub() && plat.GitLab != nil:
		return e.gitLabGraphTarget(ctx, plat.GitLab, p, token)
	}
	return scanTarget{}, errDefaultBranch
}

func (e *Engine) gitLabGraphTarget(ctx context.Context, gl *gitlab.Client, p store.Project, token bool) (scanTarget, error) {
	log := e.log().With("instance", p.Instance, "project", p.Path, "project_id", p.ProjectID)
	t := scanTarget{RepoURL: p.HTTPURLToRepo, GitUser: p.DeployTokenUsername}
	gp, err := gl.GetProject(ctx, strconv.FormatInt(p.ProjectID, 10))
	if err != nil {
		log.Info("code graph: couldn't read the project", "error", e.redact(err.Error()))
		return t, errDefaultBranch
	}
	if gp.DefaultBranch == "" {
		return t, errNoDefaultBranch
	}
	commit, err := gl.CommitSHA(ctx, p.ProjectID, gp.DefaultBranch)
	if errors.Is(err, gitlab.ErrNotFound) {
		return t, errNoDefaultBranch
	}
	if err != nil {
		log.Info("code graph: couldn't read the default branch", "error", e.redact(err.Error()))
		return t, errDefaultBranch
	}
	t.Branch, t.Commit = gp.DefaultBranch, commit
	if !token {
		return t, nil
	}
	if len(p.DeployTokenSealed) == 0 {
		return t, errNoCloneToken
	}
	tok, err := e.Box.Open(p.DeployTokenSealed)
	if err != nil {
		log.Warn("code graph: the stored deploy token can't be decrypted", "error", e.redact(err.Error()))
		return t, errNoCloneToken
	}
	t.GitToken = tok
	return t, nil
}

func (e *Engine) gitHubGraphTarget(ctx context.Context, app *github.App, p store.Project, token bool) (scanTarget, error) {
	log := e.log().With("instance", p.Instance, "project", p.Path, "project_id", p.ProjectID)
	t := scanTarget{RepoURL: p.HTTPURLToRepo, GitUser: gitHubCloneUser}
	if p.InstallationID == 0 {
		return t, errNotSetUp
	}
	owner, name, _ := strings.Cut(p.Path, "/")
	c := app.Client(p.InstallationID)
	repo, err := c.Repo(ctx, owner, name)
	if err != nil {
		log.Info("code graph: couldn't read the repository", "error", e.redact(err.Error()))
		return t, errDefaultBranch
	}
	if repo.DefaultBranch == "" {
		return t, errNoDefaultBranch
	}
	branch, err := c.Branch(ctx, owner, name, repo.DefaultBranch)
	if errors.Is(err, github.ErrNotFound) {
		return t, errNoDefaultBranch
	}
	if err == nil && branch.Commit == "" {
		err = errors.New("GitHub returned no commit")
	}
	if err != nil {
		log.Info("code graph: couldn't read the default branch", "error", e.redact(err.Error()))
		return t, errDefaultBranch
	}
	t.Branch, t.Commit = repo.DefaultBranch, branch.Commit
	if !token {
		return t, nil
	}
	tok, _, err := app.CloneToken(ctx, p.InstallationID, p.ProjectID)
	if err != nil {
		log.Warn("code graph: couldn't mint the clone token", "error", e.redact(err.Error()))
		return t, errNoCloneToken
	}
	t.GitToken = []byte(tok)
	return t, nil
}

// launchGraphs claims queued builds and starts them while fewer than
// graphCap run.
func (e *Engine) launchGraphs(ctx context.Context) {
	for ctx.Err() == nil {
		g, ok, err := e.Store.ClaimGraphBuild(ctx, graphCap, e.now().Add(graphPodBudget+graphGrace))
		if err != nil {
			if ctx.Err() == nil {
				e.log().Error("code graph: claiming a build failed", "error", e.redact(err.Error()))
			}
			return
		}
		if !ok {
			return
		}
		e.startGraph(ctx, g)
	}
}

// startGraph starts a claimed build's Job: the switch is checked again (a
// repository switched off meanwhile is dropped, not built), the clone target
// read, then the Job's name, token hash and deadline stored, then the Job
// created (a fast Job never posts before its token is known). Any stop fails
// the build.
func (e *Engine) startGraph(ctx context.Context, g store.RepoGraph) {
	log := e.log().With("build", g.BuildID, "instance", g.Instance, "project", g.Project)
	on, paused, err := e.graphOn(ctx, g.Instance, g.Project)
	switch {
	case err != nil:
		log.Error("code graph: couldn't read the repository's settings", "error", e.redact(err.Error()))
		e.failGraph(ctx, g.BuildID, codegraph.MsgTarget)
		return
	case !on:
		e.DropGraph(ctx, g.Instance, g.Project)
		return
	case paused:
		// Its graph stays; the next head check after the resume builds.
		e.failGraph(ctx, g.BuildID, codegraph.MsgPaused)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, graphPodBudget)
	defer cancel()
	t, err := e.graphTarget(pctx, g.Instance, g.Project, true)
	defer clear(t.GitToken)
	if err != nil {
		log.Info("code graph: stopped before the Job", "reason", err.Error())
		e.failGraph(ctx, g.BuildID, codegraph.MsgTarget)
		return
	}
	token, hash, err := mintToken()
	if err != nil {
		log.Error("code graph: minting the token failed", "error", err.Error())
		e.failGraph(ctx, g.BuildID, codegraph.MsgStart)
		return
	}
	name := kube.GraphJobName(g.BuildID)
	err = e.Store.StartGraphJob(ctx, g.BuildID, name, hash, e.now().Add(kube.GraphJobTimeout+graphGrace))
	switch {
	case errors.Is(err, store.ErrGraphBuildNotFound):
		log.Info("code graph: the build was gone before its Job started")
		return
	case err != nil:
		log.Error("code graph: recording the Job failed", "error", e.redact(err.Error()))
		e.failGraph(ctx, g.BuildID, codegraph.MsgStart)
		return
	}
	p := kube.GraphJobParams{
		Name: name, BuildID: g.BuildID,
		GitImage: e.Config.Jobs.Image, GraphImage: e.GraphImage, Image: e.ScanImage,
		RepoURL: t.RepoURL, GitUsername: t.GitUser, Branch: g.BuildBranch, Commit: g.BuildCommit,
		ResultURL: strings.TrimRight(e.Config.CallbackURL, "/") + "/graphs/" + g.BuildID + "/result",
	}
	if err := e.Launcher.LaunchGraph(pctx, p, string(t.GitToken), token); err != nil {
		log.Error("code graph: starting the Job failed", "job", name, "error", e.redact(err.Error()))
		e.failGraph(ctx, g.BuildID, codegraph.MsgStart)
		return
	}
	log.Info("code graph: Job started", "job", name, "branch", g.BuildBranch, "commit", g.BuildCommit)
}

func (e *Engine) failGraph(ctx context.Context, id, reason string) {
	if _, err := e.Store.FailGraphBuild(ctx, id, reason, e.now()); err != nil {
		e.log().Error("code graph: recording a failed build failed", "build", id, "error", e.redact(err.Error()))
	}
}

// sweepGraphs fails the builds past their deadline, and deletes their Jobs.
func (e *Engine) sweepGraphs(ctx context.Context) {
	expired, err := e.Store.ExpiredGraphBuilds(ctx, e.now())
	if err != nil {
		if ctx.Err() == nil {
			e.log().Error("code graph: listing the expired builds failed", "error", e.redact(err.Error()))
		}
		return
	}
	for _, g := range expired {
		if g.BuildJob != "" && e.Launcher != nil {
			if err := e.Launcher.Delete(ctx, g.BuildJob); err != nil {
				e.log().Info("code graph: couldn't delete an expired build's Job", "job", g.BuildJob, "error", e.redact(err.Error()))
			}
		}
		e.log().Info("code graph: build timed out", "build", g.BuildID, "instance", g.Instance, "project", g.Project)
		e.failGraph(ctx, g.BuildID, codegraph.MsgTimedOut)
	}
}

// CompleteGraph takes what a build Job posted with token: a graph, or a
// codegraph.ErrorBody. A token that is not the running build's is
// codegraph.Unauthorized and changes nothing. A graph that doesn't decode as
// pergent-graph/v1, or is of another commit, fails the build; an error is
// stored in pergent.sh's words (codegraph.Shown). A good graph is encoded
// again, written to its file (a temp file, then renamed), then recorded. Both
// are codegraph.Stored: the Job has nothing to try again.
func (e *Engine) CompleteGraph(ctx context.Context, id, token string, body []byte) (codegraph.Outcome, error) {
	hash := hashToken(token)
	g, ok, err := e.Store.GraphBuild(ctx, id, hash)
	if err != nil {
		return 0, err
	}
	if !ok {
		return codegraph.Unauthorized, nil
	}
	log := e.log().With("build", id, "instance", g.Instance, "project", g.Project)
	f, decodeErr := graph.Decode(bytes.NewReader(body))
	if decodeErr != nil {
		var eb codegraph.ErrorBody
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if dec.Decode(&eb) == nil && eb.Error != "" {
			log.Info("code graph: the build failed", "reason", codegraph.Shown(eb.Error))
			e.failGraph(ctx, id, codegraph.Shown(eb.Error))
			return codegraph.Stored, nil
		}
		log.Info("code graph: the build sent a graph that can't be used", "reason", decodeErr.Error())
		e.failGraph(ctx, id, codegraph.MsgUnusable)
		return codegraph.Stored, nil
	}
	if f.Commit != g.BuildCommit {
		e.failGraph(ctx, id, codegraph.MsgWrongHead)
		return codegraph.Stored, nil
	}
	out, err := f.Encode()
	if err != nil {
		log.Info("code graph: the graph can't be encoded", "reason", err.Error())
		e.failGraph(ctx, id, codegraph.MsgUnusable)
		return codegraph.Stored, nil
	}

	e.graphMu.Lock()
	defer e.graphMu.Unlock()
	// Switched off since the build started (or removed): DropGraph has
	// run, or will find nothing to delete; nothing is written. A paused
	// repository keeps its graph, so its build is stored.
	if on, _, err := e.graphOn(ctx, g.Instance, g.Project); err != nil {
		return 0, err
	} else if !on {
		e.dropGraphLocked(ctx, g.Instance, g.Project)
		return codegraph.Stored, nil
	}
	// The file goes first: a row that names a commit its file doesn't hold
	// would never be built again, a file newer than its row is rebuilt by
	// the next head check.
	if err := writeGraphFile(e.graphPath(g.Instance, g.Project), out); err != nil {
		log.Error("code graph: writing the graph failed", "error", e.redact(err.Error()))
		e.failGraph(ctx, id, codegraph.MsgStore)
		return codegraph.Stored, nil
	}
	stored, err := e.Store.StoreGraph(ctx, id, hash, store.StoredGraph{SizeBytes: int64(len(out)), Nodes: len(f.Nodes), Edges: len(f.Edges)}, e.now())
	if err != nil {
		return 0, err
	}
	if stored {
		log.Info("code graph: stored", "commit", f.Commit, "bytes", len(out), "nodes", len(f.Nodes), "edges", len(f.Edges))
	}
	return codegraph.Stored, nil
}

// GraphTooLarge fails the build whose Job posted a body over the limit, when
// token is the build's.
func (e *Engine) GraphTooLarge(ctx context.Context, id, token string) error {
	if _, ok, err := e.Store.GraphBuild(ctx, id, hashToken(token)); err != nil || !ok {
		return err
	}
	e.failGraph(ctx, id, codegraph.MsgTooLarge)
	return nil
}

// writeGraphFile writes b to path through a temp file in the same folder,
// so a reader sees the old file or the new one, whole.
func writeGraphFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".graph-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// DropGraph deletes the graph of instance's repository project: its row, its
// running build's Job, and its file. The switch going off and the repository
// being removed call it; pausing does not. It is best effort: a failure is
// logged.
func (e *Engine) DropGraph(ctx context.Context, instance, project string) {
	e.graphMu.Lock()
	defer e.graphMu.Unlock()
	e.dropGraphLocked(ctx, instance, project)
}

func (e *Engine) dropGraphLocked(ctx context.Context, instance, project string) {
	log := e.log().With("instance", instance, "project", project)
	job, found, err := e.Store.DeleteRepoGraph(ctx, instance, project)
	if err != nil {
		log.Info("code graph: couldn't delete the graph's row", "error", e.redact(err.Error()))
		return
	}
	if job != "" && e.Launcher != nil {
		if err := e.Launcher.Delete(ctx, job); err != nil {
			log.Info("code graph: couldn't delete the build Job", "job", job, "error", e.redact(err.Error()))
		}
	}
	if e.GraphDir != "" {
		if err := os.Remove(e.graphPath(instance, project)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Info("code graph: couldn't delete the graph file", "error", err.Error())
		}
	}
	if found {
		log.Info("code graph: deleted")
	}
}

// reviewGraphURL is where run's Job downloads its graph, or "" for none: the
// install keeps graphs, a graph is stored, and the change targets the branch
// it was built from. repo has the switch on (the caller checked). A failed
// read is logged and gives no graph; it never holds the review up.
func (e *Engine) reviewGraphURL(ctx context.Context, runID, instance, project, target string) string {
	if !e.GraphsOn() {
		return ""
	}
	g, ok, err := e.Store.RepoGraph(ctx, instance, project)
	if err != nil {
		e.log().Info("code graph: couldn't read the stored graph; reviewing without it", "run", runID,
			"error", e.redact(err.Error()))
		return ""
	}
	if !ok || g.Commit == "" || g.Branch != target {
		return ""
	}
	return strings.TrimRight(e.Config.CallbackURL, "/") + "/runs/" + runID + "/graph"
}

// RunGraph opens the graph run's Job may download with token: the run is
// running and token is its result token (checked, never spent), the run is a
// review of a repository whose switch is on, and the stored graph is of the
// branch the change targets. Anything else is codegraph.ErrNoGraph. The caller closes
// the file.
func (e *Engine) RunGraph(ctx context.Context, runID, token string) (*os.File, error) {
	if !e.GraphsOn() {
		return nil, codegraph.ErrNoGraph
	}
	r, ok, err := e.Store.GraphRun(ctx, runID, hashToken(token), e.now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, codegraph.ErrNoGraph
	}
	if on, _, err := e.graphOn(ctx, r.Instance, r.Project); err != nil {
		return nil, err
	} else if !on {
		return nil, codegraph.ErrNoGraph
	}
	g, ok, err := e.Store.RepoGraph(ctx, r.Instance, r.Project)
	if err != nil {
		return nil, err
	}
	if !ok || g.Commit == "" || g.Branch != r.TargetBranch {
		return nil, codegraph.ErrNoGraph
	}
	f, err := os.Open(e.graphPath(r.Instance, r.Project))
	if errors.Is(err, os.ErrNotExist) {
		return nil, codegraph.ErrNoGraph
	}
	if err != nil {
		return nil, fmt.Errorf("open the graph: %w", err)
	}
	return f, nil
}
