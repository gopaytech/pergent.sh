package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/actions"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	msgNeedsRoleReview = "only maintainers of this repository and admins can do this"
	msgBadRerun        = `the body must be {"failed": true or false}`
	msgBadDismiss      = `the body must be {"fingerprint": ..., "reason": ...}`
	msgBadRestore      = `the body must be {"fingerprint": ...}`
	msgNeedsReason     = "dismissing a blocking finding needs a reason"
	msgReasonTooLong   = "the reason is too long"
	msgNoSuchFinding   = "there is no such finding in the latest review"
	msgChangeNotOpen   = "this change is not open"
	msgBadStart        = "the body must be empty or {}"
	msgNotWaiting      = "this change isn't waiting for someone to start its review"
	msgDraftsNotOn     = "drafts aren't reviewed in this repository"
	msgPlatformFailed  = "could not read this change from its platform, please try again"
	msgRedecideFailed  = "the change was saved, but the review could not be decided again yet; the next review will use it"
)

// commandRoutes adds the review's buttons to mux, behind Auth.Require (which
// checks X-CSRF-Token):
//   - POST /api/reviews/{instance}/{project...}/{number}/rerun
//     {"failed": bool}: 202 {"status": "queued" | "already queued" |
//     "nothing failed" | "whole review"}, 429 at a re-run cap;
//   - POST …/{number}/dismiss {"fingerprint", "reason"}: 200 {"status":
//     "dismissed" | "already dismissed"};
//   - POST …/{number}/restore {"fingerprint"}: 200 {"status": "restored" |
//     "not dismissed"}.
//
// They work on every repository switched on, those set up in the config file
// too, for its maintainers and admins.
func (d *Dashboard) commandRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/reviews/{instance}/{rest...}", d.Auth.Require(http.HandlerFunc(d.reviewAction)))
	none := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { notFound(w) })
	mux.Handle("POST /api/reviews/{instance}", d.Auth.Require(none))
}

// buttonsFor is the session's role on a repository and what the buttons
// offer it: a maintainer or an admin gets all of them. (A session has no
// platform identity, so the change's author is not told apart from anyone
// else.)
func buttonsFor(se Session, instance, project string) (string, CanView) {
	role := se.Role(instance, project)
	if role < RoleMaintainer {
		return role.String(), CanView{}
	}
	return role.String(), CanView{Rerun: true, DismissBlocking: true, DismissSuggestion: true, Restore: true}
}

// reviewButton is one button press: who, on which change.
type reviewButton struct {
	repo settings.Effective
	mr   store.MRKey
	// project is the change's repository path as its runs spell it.
	project string
}

func (d *Dashboard) reviewAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	instance, rest := r.PathValue("instance"), r.PathValue("rest")
	// rest is {project...}/{number}/{action}.
	i := strings.LastIndex(rest, "/")
	if i <= 0 {
		notFound(w)
		return
	}
	action, rest := rest[i+1:], rest[:i]
	i = strings.LastIndex(rest, "/")
	if i <= 0 || (action != "rerun" && action != "dismiss" && action != "restore" && action != "start") {
		notFound(w)
		return
	}
	number, err := strconv.ParseInt(rest[i+1:], 10, 64)
	if err != nil || number <= 0 {
		notFound(w)
		return
	}
	repo, ok := d.targetAny(w, r, "/"+rest[i+1:]+"/"+action, RoleMaintainer)
	if !ok {
		return
	}
	// Decode the body first: a bad one is 400 whatever the change is.
	var rerun struct {
		Failed bool `json:"failed"`
	}
	var dismiss struct {
		Fingerprint string `json:"fingerprint"`
		Reason      string `json:"reason"`
	}
	var restore struct {
		Fingerprint string `json:"fingerprint"`
	}
	switch action {
	case "rerun":
		// No body at all is a re-run of everything.
		if err := decodeStrict(r, &rerun); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, msgBadRerun)
			return
		}
	case "start":
		var none struct{}
		if err := decodeStrict(r, &none); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, msgBadStart)
			return
		}
	case "dismiss":
		if err := decodeStrict(r, &dismiss); err != nil || strings.TrimSpace(dismiss.Fingerprint) == "" {
			writeError(w, http.StatusBadRequest, msgBadDismiss)
			return
		}
	case "restore":
		if err := decodeStrict(r, &restore); err != nil || strings.TrimSpace(restore.Fingerprint) == "" {
			writeError(w, http.StatusBadRequest, msgBadRestore)
			return
		}
	}

	// The change must be one pergent.sh has reviewed: the session sees its
	// runs.
	id, ok, err := d.projectID(ctx, session(r), instance, repo.Project)
	if err != nil {
		d.failed(w, r, "loading a project", err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	mr := store.MRKey{Instance: instance, ProjectID: id, IID: number}
	runs, err := d.Store.MRRunDigests(ctx, mr, visibility(session(r)))
	if err != nil {
		d.failed(w, r, "listing a review's runs", err)
		return
	}
	if len(runs) == 0 {
		notFound(w)
		return
	}
	b := reviewButton{repo: repo, mr: mr, project: runs[len(runs)-1].ProjectPath}
	switch action {
	case "rerun":
		d.rerun(w, r, b, rerun.Failed)
	case "start":
		if runs[len(runs)-1].State != store.Waiting {
			writeError(w, http.StatusConflict, msgNotWaiting)
			return
		}
		d.start(w, r, b)
	case "dismiss":
		d.dismiss(w, r, b, strings.TrimSpace(dismiss.Fingerprint), strings.TrimSpace(dismiss.Reason))
	case "restore":
		d.restore(w, r, b, strings.TrimSpace(restore.Fingerprint))
	}
}

func accepted(w http.ResponseWriter, status int, what string) {
	writeJSON(w, status, map[string]string{"status": what})
}

// platformChange reads the change from its platform, within platformTimeout.
// On failure it has answered the request: 404 when the platform has no such
// change, else 502.
func (d *Dashboard) platformChange(w http.ResponseWriter, r *http.Request, b reviewButton) (forge.Change, bool) {
	src, ok := d.sources.Get(b.mr.Instance)
	if !ok {
		writeError(w, http.StatusBadGateway, msgPlatformFailed)
		return forge.Change{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), platformTimeout)
	defer cancel()
	change, err := src.GetChange(ctx, b.mr.ProjectID, b.mr.IID)
	if errors.Is(err, forge.ErrNotFound) {
		notFound(w)
		return forge.Change{}, false
	}
	if err != nil {
		d.Log.Warn("dashboard: reading a change failed", "instance", b.mr.Instance, "project", b.project,
			"mr", b.mr.IID, "error", d.registry.Redact(err.Error()))
		writeError(w, http.StatusBadGateway, msgPlatformFailed)
		return forge.Change{}, false
	}
	return change, true
}

// auditReview records a button press on the change at head. A failure to is
// logged, not returned.
func (d *Dashboard) auditReview(r *http.Request, b reviewButton, action, head, detail string) {
	text := fmt.Sprintf("mr=%d head=%s", b.mr.IID, head)
	if detail != "" {
		text += " " + detail
	}
	d.audit(r.Context(), session(r), action, b.mr.Instance, b.project, text)
}

// rerun is the re-run buttons. Like the comment command, it re-runs against
// the change's head on the platform now.
func (d *Dashboard) rerun(w http.ResponseWriter, r *http.Request, b reviewButton, failedOnly bool) {
	ctx := r.Context()
	change, ok := d.platformChange(w, r, b)
	if !ok {
		return
	}
	switch {
	case change.State != forge.StateOpen:
		writeError(w, http.StatusConflict, msgChangeNotOpen)
		return
	case change.Draft && !b.repo.ReviewDrafts:
		writeError(w, http.StatusConflict, msgDraftsNotOn)
		return
	case change.HeadSHA == "":
		writeError(w, http.StatusBadGateway, msgPlatformFailed)
		return
	}

	res, err := actions.Rerun(ctx, d.Store, actions.RerunRequest{
		MR: b.mr, ProjectPath: b.project, Head: change.HeadSHA, TargetBranch: change.TargetBranch,
		FailedOnly: failedOnly, Repo: b.repo, Now: d.now(),
	})
	if err != nil {
		d.failed(w, r, "queueing a re-run", err)
		return
	}
	switch res.Status {
	case actions.RerunNothingFailed:
		accepted(w, http.StatusAccepted, "nothing failed")
	case actions.RerunCapped:
		writeError(w, http.StatusTooManyRequests, actions.CapMessage(actions.Term(b.repo.Platform)))
	case actions.RerunAlreadyQueued:
		// A head already queued or running is reviewed once.
		accepted(w, http.StatusAccepted, "already queued")
	default:
		d.wake()
		d.auditReview(r, b, res.Action, change.HeadSHA, res.Detail)
		if res.Whole {
			accepted(w, http.StatusAccepted, "whole review")
			return
		}
		accepted(w, http.StatusAccepted, "queued")
	}
}

// start is the Start review button of a change whose review waits because
// the repository doesn't trust its author: like a comment from someone with
// write access, it starts the review of the change's head on the platform
// now, against the change's re-run cap only.
func (d *Dashboard) start(w http.ResponseWriter, r *http.Request, b reviewButton) {
	ctx := r.Context()
	change, ok := d.platformChange(w, r, b)
	if !ok {
		return
	}
	switch {
	case change.State != forge.StateOpen:
		writeError(w, http.StatusConflict, msgChangeNotOpen)
		return
	case change.Draft && !b.repo.ReviewDrafts:
		writeError(w, http.StatusConflict, msgDraftsNotOn)
		return
	case change.HeadSHA == "":
		writeError(w, http.StatusBadGateway, msgPlatformFailed)
		return
	}
	res, err := actions.Start(ctx, d.Store, actions.StartRequest{
		MR: b.mr, ProjectPath: b.project, Head: change.HeadSHA, TargetBranch: change.TargetBranch,
		Repo: b.repo, Now: d.now(),
	})
	if err != nil {
		d.failed(w, r, "starting a review", err)
		return
	}
	switch res.Status {
	case actions.RerunCapped:
		writeError(w, http.StatusTooManyRequests, actions.CapMessage(actions.Term(b.repo.Platform)))
	case actions.RerunAlreadyQueued:
		accepted(w, http.StatusAccepted, "already queued")
	default:
		d.wake()
		d.auditReview(r, b, res.Action, change.HeadSHA, res.Detail)
		accepted(w, http.StatusAccepted, "queued")
	}
}

// dismiss sets a finding of the latest review aside.
func (d *Dashboard) dismiss(w http.ResponseWriter, r *http.Request, b reviewButton, fingerprint, reason string) {
	ctx, se := r.Context(), session(r)
	reason = actions.CleanReason(reason)
	if utf8.RuneCountInString(reason) > actions.MaxReasonRunes {
		writeError(w, http.StatusBadRequest, msgReasonTooLong)
		return
	}
	findings, reviewed, ok, err := actions.LatestFindings(ctx, d.Store, b.mr)
	if err != nil {
		d.failed(w, r, "loading the latest review", err)
		return
	}
	f, found := actions.MostSevere(findings, fingerprint)
	if !ok || !found {
		writeError(w, http.StatusNotFound, msgNoSuchFinding)
		return
	}
	// Anything but a suggestion is dismissed as a blocking finding, which
	// needs a reason and an open change.
	severity := "blocking"
	if f.Severity == "suggestion" {
		severity = "suggestion"
	}
	if severity == "blocking" {
		if reason == "" {
			writeError(w, http.StatusBadRequest, msgNeedsReason)
			return
		}
		if !d.mayDismissBlocking(w, r, b) {
			return
		}
	}

	accountID := se.AccountID
	created, err := d.Store.Dismiss(context.WithoutCancel(ctx), store.Dismissal{
		MR: b.mr, Fingerprint: fingerprint, Severity: severity, Reason: reason,
		Actor: dismissalName(se), ActorID: &accountID, CreatedAt: d.now(),
		// The buttons are for maintainers and admins, so a dismissal teaches.
		// DismisserPlatformID stays 0: an account has no platform identity.
		Skill: f.Skill, File: f.File, Title: f.Title, Teaches: true,
	})
	if err != nil {
		d.Log.Error("dashboard: storing a dismissal failed", "instance", b.mr.Instance, "project", b.project, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	status := "already dismissed"
	if created {
		status = "dismissed"
		detail := "fingerprint=" + fingerprint + " severity=" + severity
		if reason != "" {
			detail += fmt.Sprintf(" reason=%q", d.registry.Redact(reason))
		}
		d.auditReview(r, b, "finding_dismissed", reviewed.HeadSHA, detail)
	}
	// Decided again whether or not the dismissal was new: an earlier press
	// may have stored it and failed to decide the review, and pressing the
	// button again is how that is put right.
	if !d.redecide(r, b.mr) {
		writeError(w, http.StatusInternalServerError, msgRedecideFailed)
		return
	}
	accepted(w, http.StatusOK, status)
}

// mayDismissBlocking tells whether the session may dismiss a blocking
// finding of the change. Nobody may on a merged or closed change, an admin
// included, and when the platform can't say what state it is in the request
// fails closed. Anyone else who reaches the button may, the change's author
// too: an account has no platform identity to compare. When it may not, it
// has answered the request.
func (d *Dashboard) mayDismissBlocking(w http.ResponseWriter, r *http.Request, b reviewButton) bool {
	change, ok := d.platformChange(w, r, b)
	if !ok {
		return false
	}
	switch change.State {
	case forge.StateMerged, forge.StateClosed:
		writeError(w, http.StatusConflict, "this change is "+string(change.State))
		return false
	}
	return true
}

// restore undoes a dismissal.
func (d *Dashboard) restore(w http.ResponseWriter, r *http.Request, b reviewButton, fingerprint string) {
	ctx := r.Context()
	stored, err := d.Store.Dismissals(ctx, b.mr)
	if err != nil {
		d.failed(w, r, "loading dismissals", err)
		return
	}
	severity := ""
	for _, dm := range stored {
		if dm.Fingerprint == fingerprint {
			severity = dm.Severity
		}
	}
	reviewed, _, err := d.Store.LatestDecidedRun(ctx, b.mr)
	if err != nil {
		d.failed(w, r, "loading the latest review", err)
		return
	}
	// Everything from here on is carried through even if the request ends.
	wctx := context.WithoutCancel(ctx)
	restored, err := d.Store.Restore(wctx, b.mr, fingerprint)
	if err != nil {
		d.Log.Error("dashboard: restoring a finding failed", "instance", b.mr.Instance, "project", b.project, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	status := "not dismissed"
	if restored {
		status = "restored"
		d.auditReview(r, b, "finding_restored", reviewed.HeadSHA, "fingerprint="+fingerprint+" severity="+severity)
	}
	// Decided again whether or not anything was restored, as dismiss does.
	if !d.redecide(r, b.mr) {
		writeError(w, http.StatusInternalServerError, msgRedecideFailed)
		return
	}
	accepted(w, http.StatusOK, status)
}

// redecide has the change's newest review decided again with the dismissals
// as they are now, without a new run (the engine publishes it), and wakes the
// engine. The newest run is the one to reach whatever its head: one whose
// first publish is under way has loaded the dismissals already (store.
// RepublishNewest). A newest run still queued or running, or no run at all,
// has nothing to decide: it loads them when it is published. It reports
// whether the store accepted the request.
func (d *Dashboard) redecide(r *http.Request, mr store.MRKey) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), platformTimeout)
	defer cancel()
	if _, err := d.Store.RepublishNewest(ctx, mr); err != nil {
		d.Log.Error("dashboard: deciding a review again failed", "instance", mr.Instance, "mr", mr.IID, "error", err.Error())
		return false
	}
	d.wake()
	return true
}
