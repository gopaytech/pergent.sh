package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	msgScanBusy = "a scan is already queued or running"
	// msgScanResultUnreadable stands for a stored file result that can't be
	// decoded: the checklist says "couldn't scan files: " and this.
	msgScanResultUnreadable = "the stored scan result can't be read; scan again"
	// kindSingleApproval is the hourly check's Needs attention item for a
	// repository where one approval is enough to merge (engine
	// attSingleApproval). It is newer than any scan, so it decides the
	// approvals row.
	kindSingleApproval = "single_approval"
)

// msgScanTooSoon is the 429's message: the wait in whole minutes, rounded up.
func msgScanTooSoon(wait int) string {
	minutes := (wait + 59) / 60
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	return fmt.Sprintf("this repository was scanned less than 5 minutes ago; try again in %d %s", minutes, unit)
}

// --- GET /api/repositories/{instance}/{project...}/readiness ---

// repositoryReadiness serves repo's Readiness tab: its scans and the
// checklist built now from the latest finished one. Nothing here asks the
// platform or the cluster.
func (d *Dashboard) repositoryReadiness(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	ctx := r.Context()
	latest, active, err := d.Store.ReadinessScans(ctx, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading the readiness scans", err)
		return
	}
	library := repo.Skills
	if library == nil {
		def, err := d.Store.RepoDefaults(ctx)
		if err != nil {
			d.failed(w, r, "reading the defaults", err)
			return
		}
		library = def.Skills
	}
	view := ReadinessView{
		Latest: scanView(latest), Active: scanView(active),
		CanScan:         se.Role(repo.Instance, repo.Project) >= RoleMaintainer,
		ConfigRepo:      repo.Source == settings.SourceConfig,
		FollowsDefaults: repo.Skills == nil,
		ApplyBase:       nonNil(slices.Clone(library)),
	}
	if latest != nil {
		c, err := d.readinessChecklist(ctx, se, repo, latest, library)
		if err != nil {
			d.failed(w, r, "building the readiness checklist", err)
			return
		}
		view.Checklist = &c
	}
	writeJSON(w, http.StatusOK, view)
}

// readinessChecklist builds repo's checklist from its latest finished scan
// and library, the library skills it has now: the settings the pod read,
// the files the Job scanned (or why it couldn't), and the sizes of the
// library's skills now.
func (d *Dashboard) readinessChecklist(ctx context.Context, se Session, repo settings.Effective, latest *store.ReadinessScan,
	library []string) (readiness.Checklist, error) {
	in := readiness.ChecklistInput{Library: library, RepoSkills: repo.RepoSkills}
	if latest.Pod != nil {
		var pod readiness.PodResult
		if err := json.Unmarshal(latest.Pod, &pod); err != nil {
			// Without it the settings rows say they couldn't be checked.
			d.Log.Warn("dashboard: a stored readiness pod result can't be read", "scan", latest.ID, "error", err.Error())
		} else {
			in.Pod = &pod
		}
	}
	switch {
	case latest.State == store.ScanFailed:
		in.FileError = latest.Error
	case latest.Job != nil:
		var job readiness.JobResult
		if err := json.Unmarshal(latest.Job, &job); err != nil {
			d.Log.Warn("dashboard: a stored readiness file result can't be read", "scan", latest.ID, "error", err.Error())
			in.FileError = msgScanResultUnreadable
		} else {
			in.Job = &job
		}
	}
	list, err := d.Store.LibrarySkills(ctx)
	if err != nil {
		return readiness.Checklist{}, err
	}
	in.LibrarySizes = make(map[string]int, len(list))
	for _, s := range list {
		in.LibrarySizes[s.Name] = s.Size
	}
	items, err := d.attention(ctx, se, byProject([]settings.Effective{repo}), repoKey(repo.Instance, repo.Project))
	if err != nil {
		return readiness.Checklist{}, err
	}
	in.SingleApproval = slices.ContainsFunc(items, func(a AttentionView) bool { return a.Kind == kindSingleApproval })
	c := readiness.BuildChecklist(in)
	c.Suggested = nonNil(c.Suggested)
	return c, nil
}

// scanView is sc as the API shows it; nil for none.
func scanView(sc *store.ReadinessScan) *ScanView {
	if sc == nil {
		return nil
	}
	v := &ScanView{ID: sc.ID, State: string(sc.State), StartedBy: sc.StartedBy, Branch: sc.Branch, Commit: sc.Commit,
		Error: sc.Error, QueuedAt: sc.QueuedAt.UTC()}
	if !sc.FinishedAt.IsZero() {
		at := sc.FinishedAt.UTC()
		v.FinishedAt = &at
	}
	return v
}

// --- POST /api/repositories/{instance}/{project...}/readiness/scan ---

// scanNow queues a scan of the repository (Scan now) for a maintainer or
// admin of it, config repositories included. A repository the session
// can't see, or one with nothing to scan (switched off meanwhile, scans
// off), is the generic 404 and nothing is queued. Only an accepted scan is
// audited.
func (d *Dashboard) scanNow(w http.ResponseWriter, r *http.Request) {
	repo, ok := d.targetRepo(w, r, "/readiness/scan", RoleMaintainer, true, msgNeedsRoleScan)
	if !ok {
		return
	}
	if d.readiness == nil {
		notFound(w)
		return
	}
	ctx, se := r.Context(), session(r)
	sc, err := d.readiness.RequestScan(ctx, repo.Instance, repo.Project, actorLabel(se.Kind, se.Platform, se.Instance, se.Username))
	var soon *readiness.ScanTooSoonError
	switch {
	case errors.Is(err, readiness.ErrScanBusy):
		writeJSON(w, http.StatusConflict, struct {
			Error string    `json:"error"`
			Scan  *ScanView `json:"scan"`
		}{msgScanBusy, scanView(&sc)})
	case errors.As(err, &soon):
		wait := max(int(math.Ceil(soon.Wait.Seconds())), 1)
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		writeJSON(w, http.StatusTooManyRequests, struct {
			Error       string `json:"error"`
			WaitSeconds int    `json:"waitSeconds"`
		}{msgScanTooSoon(wait), wait})
	case errors.Is(err, readiness.ErrNotSwitchedOn), errors.Is(err, readiness.ErrScansOff):
		notFound(w)
	case err != nil:
		d.failed(w, r, "queueing a readiness scan", err)
	default:
		d.Log.Info("dashboard: readiness scan asked for", "instance", repo.Instance, "project", repo.Project,
			"scan", sc.ID, "by", se.Username)
		d.audit(ctx, se, "readiness_scan_requested", repo.Instance, repo.Project, "")
		writeJSON(w, http.StatusAccepted, struct {
			Scan *ScanView `json:"scan"`
		}{scanView(&sc)})
	}
}

// recheck asks the engine to re-judge the last readiness scan of
// instance's repository project against its skills now; project "" is
// every repository that follows the default list. It never blocks.
func (d *Dashboard) recheck(instance, project string) {
	if d.readiness != nil {
		d.readiness.RecheckReadiness(instance, project)
	}
}

// recheckAll asks the engine to re-judge the last readiness scan of every
// repository: a library skill's text changed, so its size and paths did.
// It never blocks.
func (d *Dashboard) recheckAll() {
	if d.readiness != nil {
		d.readiness.RecheckAllReadiness()
	}
}
