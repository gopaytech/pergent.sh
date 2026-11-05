package dashboard

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/actions"
	"github.com/gopaytech/pergent.sh/internal/settings"
)

const msgBadStop = `the body must be {"fingerprint": ...}`

// --- GET /api/repositories/{instance}/{project...}/past-dismissals ---

// repositoryPastDismissals serves repo's Past dismissals tab: what teaches its
// reviews now, one row a finding, by the rules a review follows except the
// limit of 20 and the own-change rule. Maintainers and admins only.
func (d *Dashboard) repositoryPastDismissals(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	if se.Role(repo.Instance, repo.Project) < RoleMaintainer {
		writeError(w, http.StatusForbidden, msgNeedsRoleReview)
		return
	}
	ctx := r.Context()
	// A repository the platform hasn't told us the ID of has no dismissal.
	id, found, err := d.projectID(ctx, se, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading past dismissals", err)
		return
	}
	out := []PastDismissalView{}
	if found {
		since := d.now().Add(-actions.PastDismissalDays * 24 * time.Hour)
		rows, err := d.Store.TeachingDismissals(ctx, repo.Instance, id, since)
		if err != nil {
			d.failed(w, r, "reading past dismissals", err)
			return
		}
		for _, row := range actions.TeachingRows(rows) {
			file, title, reason := actions.TeachingText(row, d.registry.Redact)
			out = append(out, PastDismissalView{
				Skill: row.Skill, Severity: row.Severity, File: file, Title: title, Reason: reason,
				By: row.Actor, At: row.CreatedAt.UTC(), Number: row.MR.IID, Fingerprint: row.Fingerprint,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// --- POST /api/repositories/{instance}/{project...}/past-dismissals/stop ---

// stopUsing is Stop using: every dismissal of a finding in the repository
// stops teaching reviews, so the next one with the same fingerprint doesn't
// take its place. The dismissals stay on their changes, no review is decided
// again and the platform isn't asked. It works on config repositories too.
// Only a stop that changed something is audited.
func (d *Dashboard) stopUsing(w http.ResponseWriter, r *http.Request) {
	repo, ok := d.targetAny(w, r, "/past-dismissals/stop", RoleMaintainer)
	if !ok {
		return
	}
	var body struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := decodeStrict(r, &body); err != nil || strings.TrimSpace(body.Fingerprint) == "" {
		writeError(w, http.StatusBadRequest, msgBadStop)
		return
	}
	fingerprint := strings.TrimSpace(body.Fingerprint)
	ctx, se := context.WithoutCancel(r.Context()), session(r)
	status := "not teaching"
	id, found, err := d.projectID(ctx, se, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "stopping a past dismissal", err)
		return
	}
	if found {
		stopped, err := d.Store.StopTeaching(ctx, repo.Instance, id, fingerprint)
		if err != nil {
			d.failed(w, r, "stopping a past dismissal", err)
			return
		}
		if stopped {
			status = "stopped"
			d.audit(ctx, se, "dismissal_stopped", repo.Instance, repo.Project, "fingerprint="+fingerprint)
		}
	}
	accepted(w, http.StatusOK, status)
}
