package githubforge

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/ci"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/store"
)

var _ forge.CIReader = (*Forge)(nil)

// actionsSlug is the app slug of GitHub Actions' check runs.
const actionsSlug = "github-actions"

// maxAnnotations is how many annotations of another app's check run go in
// its log.
const maxAnnotations = 50

// own reports whether a check run comes from one of pergent.sh's own Apps.
func (f *Forge) own(appID int64) bool {
	return appID == f.app.ID() || f.ownApps[appID]
}

// order is the spec's table order: the first result of a name's runs that
// isn't passed is the job's result.
var order = []ci.Result{ci.Running, ci.Failed, ci.Cancelled, ci.Skipped}

// ReadCI reads the chosen jobs on the head. Every check run with a chosen
// name must pass; no run is Missing. Finished: every check suite with check
// runs has completed (a suite with none is ignored).
func (f *Forge) ReadCI(ctx context.Context, t forge.CITarget, jobs []store.CIJob, withText bool) (ci.Snapshot, error) {
	c, r, err := f.client(ctx, t.ProjectID)
	if err != nil {
		return ci.Snapshot{}, err
	}
	snap := ci.Snapshot{Head: t.Head, Finished: true}
	suites, err := c.CheckSuites(ctx, r.Owner, r.Name, t.Head)
	if err != nil {
		return ci.Snapshot{}, mapErr(err)
	}
	for _, s := range suites {
		// pergent.sh's own check run (queued while a run waits) never counts.
		if !f.own(s.AppID) && s.RunCount > 0 && s.Status != "completed" {
			snap.Finished = false
		}
	}
	for _, want := range jobs {
		job := ci.Job{Name: want.Name, Read: want.Read, Blocks: want.BlocksApproval}
		all, err := c.CheckRuns(ctx, r.Owner, r.Name, t.Head, want.Name)
		if err != nil {
			return ci.Snapshot{}, mapErr(err)
		}
		var mine []github.CheckRun
		for _, cr := range all {
			if !f.own(cr.AppID) {
				mine = append(mine, cr)
			}
		}
		if len(mine) == 0 {
			job.Result = ci.Missing
			snap.Jobs = append(snap.Jobs, job)
			continue
		}
		chosen := mine[0]
		job.Result, job.Ended = ci.Passed, true
		results := make([]ci.Result, len(mine))
		for i, cr := range mine {
			results[i], _ = ci.FromGitHub(cr.Status, cr.Conclusion)
		}
	pick:
		for _, res := range order {
			for i, got := range results {
				if got == res {
					chosen, job.Result = mine[i], res
					job.Ended = res != ci.Running
					break pick
				}
			}
		}
		actions := chosen.AppSlug == actionsSlug
		if want.Read == "report" && !actions {
			// Another app's check run has no artifacts.
			job.Result, job.Ended, job.Reason = ci.Unreadable, true, "error"
		} else if withText {
			if err := f.readText(ctx, c, r, want, chosen, actions, &job); err != nil {
				return ci.Snapshot{}, err
			}
		}
		snap.Jobs = append(snap.Jobs, job)
	}
	return snap, nil
}

// readText fills Log (a failed, cancelled or running job) or Report (a job
// that passed or failed). A read that fails makes the job Unreadable; only an
// ended context is an error.
func (f *Forge) readText(ctx context.Context, c *github.Client, r Repo, want store.CIJob, cr github.CheckRun, actions bool, job *ci.Job) error {
	var text string
	var cut bool
	var err error
	switch {
	case want.Read == "log" && (job.Result == ci.Failed || job.Result == ci.Cancelled || job.Result == ci.Running):
		if actions {
			var rc io.ReadCloser
			if rc, err = c.JobLogs(ctx, r.Owner, r.Name, cr.ID); err == nil {
				defer rc.Close()
				text, cut, err = ci.Tail(rc)
			}
		} else {
			var notes []string
			if notes, err = c.CheckRunAnnotations(ctx, r.Owner, r.Name, cr.ID, maxAnnotations); err == nil {
				parts := []string{cr.Title, cr.Summary, cr.Text}
				parts = append(parts, notes...)
				var kept []string
				for _, p := range parts {
					if strings.TrimSpace(p) != "" {
						kept = append(kept, p)
					}
				}
				text, cut, err = ci.Tail(strings.NewReader(strings.Join(kept, "\n")))
			}
		}
	case want.Read == "report" && want.Report != nil && (job.Result == ci.Passed || job.Result == ci.Failed):
		var runID int64
		if runID, err = c.ActionsJobRunID(ctx, r.Owner, r.Name, cr.ID); err == nil {
			var body string
			if body, err = c.RunArtifactFile(ctx, r.Owner, r.Name, runID, want.Report.Artifact, want.Report.Path); err == nil {
				text, cut = ci.Cap(body)
			}
		}
	default:
		return nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		job.Result, job.Ended, job.Reason, job.TextFailed = ci.Unreadable, true, "error", true
		var apiErr *github.APIError
		if errors.Is(err, github.ErrForbidden) || errors.As(err, &apiErr) && apiErr.Status == 401 {
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

// RecentCINames lists the check run names on the target's recent heads, less
// pergent's own.
func (f *Forge) RecentCINames(ctx context.Context, t forge.CITarget) ([]string, error) {
	c, r, err := f.client(ctx, t.ProjectID)
	if err != nil {
		return nil, err
	}
	names, err := c.RecentCheckRunNames(ctx, r.Owner, r.Name, t.RecentHeads)
	if err != nil {
		return nil, mapErr(err)
	}
	out := names[:0]
	for _, n := range names {
		if n != checkName {
			out = append(out, n)
		}
	}
	return out, nil
}
