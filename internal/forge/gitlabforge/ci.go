package gitlabforge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/gopaytech/pergent.sh/internal/ci"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/store"
)

var _ forge.CIReader = (*Forge)(nil)

// countingPipelines are the pipelines whose jobs count for t.Head: those on
// the head's SHA, and the merged-results pipelines of the change whose commit
// has the head as a parent. Never head_pipeline alone.
func (f *Forge) countingPipelines(ctx context.Context, t forge.CITarget) ([]gitlab.Pipeline, error) {
	list, err := f.c.PipelinesForSHA(ctx, t.ProjectID, t.Head)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	for _, p := range list {
		seen[p.ID] = true
	}
	mr, err := f.c.MRPipelines(ctx, t.ProjectID, t.Change)
	if err != nil {
		return nil, err
	}
	merge := fmt.Sprintf("refs/merge-requests/%d/merge", t.Change)
	for _, p := range mr {
		if p.Ref != merge || seen[p.ID] {
			continue
		}
		parents, err := f.commitParents(ctx, t.ProjectID, p.SHA)
		if errors.Is(err, gitlab.ErrNotFound) {
			// An old merged-results commit can be pruned: it can't be shown
			// to have the head as a parent, so it doesn't count.
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, id := range parents {
			if id == t.Head {
				seen[p.ID] = true
				list = append(list, p)
				break
			}
		}
	}
	return list, nil
}

// parentsMemoSize bounds the commits whose parents are remembered.
const parentsMemoSize = 1024

type parentsKey struct {
	project int64
	sha     string
}

// commitParents is CommitParents, remembered: a commit's parents never
// change, and every read of a head asks again for each merged-results
// pipeline's commit. A failed lookup is not remembered.
func (f *Forge) commitParents(ctx context.Context, project int64, sha string) ([]string, error) {
	f.parentsMu.Lock()
	ps, ok := f.parents[parentsKey{project, sha}]
	f.parentsMu.Unlock()
	if ok {
		return ps, nil
	}
	ps, err := f.c.CommitParents(ctx, project, sha)
	if err != nil {
		return nil, err
	}
	f.rememberParents(project, sha, ps)
	return ps, nil
}

// rememberParents adds one commit to the memo; a full memo first drops one
// entry it holds.
func (f *Forge) rememberParents(project int64, sha string, ps []string) {
	f.parentsMu.Lock()
	defer f.parentsMu.Unlock()
	if f.parents == nil {
		f.parents = map[parentsKey][]string{}
	}
	if len(f.parents) >= parentsMemoSize {
		for k := range f.parents {
			delete(f.parents, k)
			break
		}
	}
	f.parents[parentsKey{project, sha}] = ps
}

// ReadCI reads the chosen jobs on the head. A job nobody ran is Missing. Per
// name the job with the highest id (the newest attempt) wins.
func (f *Forge) ReadCI(ctx context.Context, t forge.CITarget, jobs []store.CIJob, withText bool) (ci.Snapshot, error) {
	snap := ci.Snapshot{Head: t.Head, Finished: true}
	pipelines, err := f.countingPipelines(ctx, t)
	if err != nil {
		return ci.Snapshot{}, mapErr(err)
	}
	newest := map[string]gitlab.CIJob{}
	for _, p := range pipelines {
		list, err := f.c.PipelineJobs(ctx, t.ProjectID, p.ID)
		if err != nil {
			return ci.Snapshot{}, mapErr(err)
		}
		for _, j := range list {
			// Finished is read from the jobs and bridges, not the pipeline's
			// status: pergent's own pending commit status can keep it running.
			if _, ended := jobStatus(j); !ended {
				snap.Finished = false
			}
			if cur, ok := newest[j.Name]; !ok || j.ID > cur.ID {
				newest[j.Name] = j
			}
		}
	}
	for _, want := range jobs {
		job := ci.Job{Name: want.Name, Read: want.Read, Blocks: want.BlocksApproval}
		j, ok := newest[want.Name]
		if !ok {
			job.Result = ci.Missing
			snap.Jobs = append(snap.Jobs, job)
			continue
		}
		job.Result, job.Ended = jobStatus(j)
		if j.Bridge && want.Read == "report" {
			// A trigger job has no artifacts.
			job.Result, job.Ended, job.Reason = ci.Unreadable, true, "error"
		} else if withText && !j.Bridge {
			if err := f.readText(ctx, t, want, j, &job); err != nil {
				return ci.Snapshot{}, err
			}
		}
		snap.Jobs = append(snap.Jobs, job)
	}
	return snap, nil
}

// jobStatus maps a job's status. A trigger job without strategy: depend is
// success as soon as its child pipeline is created, so a bridge's result is
// its child pipeline's status. A bridge with no child yet is running, unless
// the bridge itself failed, was cancelled or was skipped.
func jobStatus(j gitlab.CIJob) (ci.Result, bool) {
	if !j.Bridge {
		return ci.FromGitLab(j.Status)
	}
	if j.Downstream != nil && j.Downstream.Status != "" {
		return ci.FromGitLab(j.Downstream.Status)
	}
	switch r, ended := ci.FromGitLab(j.Status); r {
	case ci.Failed, ci.Cancelled, ci.Skipped:
		return r, ended
	}
	return ci.Running, false
}

// reportReadLimit is how much of a report artifact is read before Cap.
const reportReadLimit = 1 << 20

// readText fills Log (a failed, cancelled or running job) or Report (an ended
// job that passed or failed). A read that fails makes the job Unreadable; only a ended context
// is an error.
func (f *Forge) readText(ctx context.Context, t forge.CITarget, want store.CIJob, j gitlab.CIJob, job *ci.Job) error {
	var rc io.ReadCloser
	var err error
	switch {
	case want.Read == "log" && (job.Result == ci.Failed || job.Result == ci.Cancelled || job.Result == ci.Running):
		rc, err = f.c.JobTrace(ctx, t.ProjectID, j.ID)
	case want.Read == "report" && want.Report != nil && (job.Result == ci.Passed || job.Result == ci.Failed):
		rc, err = f.c.JobArtifact(ctx, t.ProjectID, j.ID, want.Report.Path)
	default:
		return nil
	}
	var text string
	var cut bool
	if err == nil {
		defer rc.Close()
		if want.Read == "log" {
			text, cut, err = ci.Tail(rc)
		} else {
			var b []byte
			// One byte past the read limit says the report was cut there,
			// whatever Cap then keeps of the rest.
			b, err = io.ReadAll(io.LimitReader(rc, reportReadLimit+1))
			if err == nil {
				over := len(b) > reportReadLimit
				if over {
					b = b[:reportReadLimit]
				}
				text, cut = ci.Cap(string(b))
				cut = cut || over
			}
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		job.Result, job.Ended, job.Reason, job.TextFailed = ci.Unreadable, true, "error", true
		if errors.Is(err, gitlab.ErrForbidden) {
			job.Reason = "no permission"
		}
		return nil
	}
	if want.Read == "log" {
		job.Log = text
	} else {
		job.Report = text
	}
	job.Cut = cut
	return nil
}

// RecentCINames lists the job names of the project's last 10 pipelines.
func (f *Forge) RecentCINames(ctx context.Context, t forge.CITarget) ([]string, error) {
	ps, err := f.c.RecentPipelines(ctx, t.ProjectID, 10)
	if err != nil {
		return nil, mapErr(err)
	}
	set := map[string]bool{}
	for _, p := range ps {
		jobs, err := f.c.PipelineJobs(ctx, t.ProjectID, p.ID)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, j := range jobs {
			set[j.Name] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}
