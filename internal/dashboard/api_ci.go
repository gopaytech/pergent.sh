package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/ci"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// ciNameHeads is how many head SHAs of a repository's runs the names
	// handler reads GitHub check run names from.
	ciNameHeads = 10
	// ciNamesBudget bounds the platform reads of the names handler.
	ciNamesBudget = 15 * time.Second

	msgBadCIJobs   = `ciJobs must be {"wait": "30m", "jobs": [{"name": "test", "read": "status", "blocksApproval": true}]}, or null for none`
	msgCINamesRead = "could not read the recent CI job names from the platform, please try again"
)

// CIJobsView is the ciJobs setting as the API shows and takes it: how long a
// run waits for the jobs ("30m": 5m to 2h) and the jobs.
type CIJobsView struct {
	Wait string      `json:"wait"`
	Jobs []CIJobView `json:"jobs"`
}

// CIJobView is one job of CIJobsView. Report is set only for read "report".
type CIJobView struct {
	Name           string        `json:"name"`
	Read           string        `json:"read"`
	Report         *CIReportView `json:"report,omitempty"`
	BlocksApproval bool          `json:"blocksApproval"`
}

// CIReportView is where a job's report file is: Artifact is the workflow
// artifact's name (GitHub only), Path the file inside the job's artifacts.
type CIReportView struct {
	Artifact string `json:"artifact,omitempty"`
	Path     string `json:"path"`
}

// CIJobResultView is one chosen job of a run, as it was when the run was
// decided: a snapshot with no CI text.
type CIJobResultView struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	Read   string `json:"read"`
	Size   int    `json:"size"`
	Cut    bool   `json:"cut"`
	Blocks bool   `json:"blocks"`
}

// CIJobNamesView is GET .../ci-job-names: job or check names seen in the
// repository's recent pipelines or runs, sorted.
type CIJobNamesView struct {
	Names []string `json:"names"`
}

// waitText is a wait as the API shows it: "30m", "2h", "1h30m".
func waitText(d time.Duration) string {
	m := int64(d / time.Minute)
	switch {
	case m%60 == 0 && m > 0:
		return strconv.FormatInt(m/60, 10) + "h"
	case m > 60:
		return strconv.FormatInt(m/60, 10) + "h" + strconv.FormatInt(m%60, 10) + "m"
	}
	return strconv.FormatInt(m, 10) + "m"
}

// ciJobsView is a repository's CI jobs as the API shows them: never null,
// and the default wait when none are set.
func ciJobsView(c store.CIJobs) CIJobsView {
	wait := c.Wait()
	if wait == 0 {
		wait = ci.DefaultWait
	}
	jobs := make([]CIJobView, len(c.Jobs))
	for i, j := range c.Jobs {
		jobs[i] = CIJobView{Name: j.Name, Read: j.Read, BlocksApproval: j.BlocksApproval}
		if j.Report != nil {
			jobs[i].Report = &CIReportView{Artifact: j.Report.Artifact, Path: j.Report.Path}
		}
	}
	return CIJobsView{Wait: waitText(wait), Jobs: jobs}
}

// ciResultViews is a run's CI snapshot as the API shows it: null for a run
// that has none.
func ciResultViews(rs []store.CIJobResult) []CIJobResultView {
	if len(rs) == 0 {
		return nil
	}
	out := make([]CIJobResultView, len(rs))
	for i, r := range rs {
		out[i] = CIJobResultView{Name: r.Name, Result: r.Result, Reason: r.Reason, Read: r.Read, Size: r.Size, Cut: r.Cut, Blocks: r.Blocks}
	}
	return out
}

// ciJobsSetting takes null or {"wait", "jobs"} (strict: an unknown field is
// refused). No jobs, like null, clears the setting. Only the shape and the
// wait are read here; the rules (ci.Validate, which needs the platform) are
// checkCIJobs.
func ciJobsSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string) {
	var v *store.CIJobs
	if !null {
		var body struct {
			Wait string `json:"wait"`
			Jobs []struct {
				Name   string `json:"name"`
				Read   string `json:"read"`
				Report *struct {
					Artifact string `json:"artifact"`
					Path     string `json:"path"`
				} `json:"report"`
				BlocksApproval bool `json:"blocksApproval"`
			} `json:"jobs"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil {
			return nil, msgBadCIJobs
		}
		wait := ci.DefaultWait
		if body.Wait != "" {
			var err error
			if wait, err = time.ParseDuration(body.Wait); err != nil {
				return nil, "ciJobs.wait must be a duration such as 30m, from 5m to 2h"
			}
		}
		if len(body.Jobs) > 0 {
			v = &store.CIJobs{WaitSeconds: int64(wait / time.Second)}
			for _, j := range body.Jobs {
				job := store.CIJob{Name: strings.TrimSpace(j.Name), Read: j.Read, BlocksApproval: j.BlocksApproval}
				if j.Report != nil {
					job.Report = &store.CIReport{Artifact: strings.TrimSpace(j.Report.Artifact), Path: strings.TrimSpace(j.Report.Path)}
				}
				v.Jobs = append(v.Jobs, job)
			}
		}
	}
	return func(row *store.DashboardRepo) { row.CIJobs = v }, ""
}

// checkCIJobs runs ci.Validate on what a save would store; "" when it is
// fine or nothing is set.
func checkCIJobs(repo settings.Effective, c *store.CIJobs) string {
	if c == nil {
		return ""
	}
	if err := ci.Validate(*c, repo.Platform == config.PlatformGitHub); err != nil {
		return err.Error()
	}
	return ""
}

// ciJobNames serves GET .../ci-job-names, for maintainers and admins: the job
// (GitLab) or check run (GitHub) names the platform shows in the repository's
// recent pipelines or runs, to suggest in the CI jobs setting. An instance
// whose platform can't list them, or a repository not stored yet, has none.
// A failed read is 502 with a fixed message.
func (d *Dashboard) ciJobNames(w http.ResponseWriter, r *http.Request, repo settings.Effective) {
	ctx, se := r.Context(), session(r)
	if se.Role(repo.Instance, repo.Project) < RoleMaintainer {
		writeError(w, http.StatusForbidden, msgNeedsRole)
		return
	}
	names := []string{}
	reply := func() { writeJSON(w, http.StatusOK, CIJobNamesView{Names: names}) }
	src, ok := d.sources.Get(repo.Instance)
	reader, can := any(src).(forge.CIReader)
	if !ok || src == nil || !can {
		reply()
		return
	}
	id, found, err := d.projectID(ctx, se, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "loading a project", err)
		return
	}
	if !found {
		reply()
		return
	}
	t := forge.CITarget{ProjectID: id, Project: repo.Project}
	if repo.Platform == config.PlatformGitHub {
		if t.RecentHeads, err = d.Store.RecentRunHeads(ctx, repo.Instance, id, ciNameHeads); err != nil {
			d.failed(w, r, "listing the heads of a repository's runs", err)
			return
		}
	}
	bctx, cancel := context.WithTimeout(ctx, ciNamesBudget)
	defer cancel()
	got, err := reader.RecentCINames(bctx, t)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			d.Log.Warn("dashboard: listing recent CI job names failed", "instance", repo.Instance, "project", repo.Project,
				"error", d.registry.Redact(err.Error()))
		}
		writeError(w, http.StatusBadGateway, msgCINamesRead)
		return
	}
	if len(got) > 0 {
		names = got
	}
	reply()
}
