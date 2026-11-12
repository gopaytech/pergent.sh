package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	msgAPIStatus       = "status must be one of queued, running, completed, failed, superseded"
	msgBadAPIDismissal = `the body must be {"reason": ...}`
)

// apiStatuses are the statuses a review has, and the ones the list takes.
var apiStatuses = []string{reviewapi.StatusQueued, reviewapi.StatusRunning, reviewapi.StatusCompleted,
	reviewapi.StatusFailed, reviewapi.StatusSuperseded}

func (d *Dashboard) apiClientReviewRoutes(mux *http.ServeMux) {
	d.adminRoute(mux, "GET /api/admin/api-clients/{name}/reviews", d.listClientReviews)
	d.adminRoute(mux, "GET /api/admin/api-reviews/{id}", d.getAPIReview)
	d.adminRoute(mux, "GET /api/admin/api-reviews/{id}/log", d.apiReviewLog)
	d.adminRoute(mux, "GET /api/admin/api-reviews/{id}/diagnostics", d.apiReviewDiagnostics)
	d.adminRoute(mux, "POST /api/admin/api-reviews/{id}/findings/{finding}/dismiss", d.dismissAPIFinding)
	d.adminRoute(mux, "DELETE /api/admin/api-reviews/{id}/findings/{finding}/dismiss", d.restoreAPIFinding)
}

// reviewBody is the v1 body of r: the one stored when it was published, else
// the one the API answers with before that (reviewapi.Pending).
func reviewBody(r store.APIReview) (json.RawMessage, error) {
	if len(r.Response) > 0 {
		return r.Response, nil
	}
	return json.Marshal(reviewapi.Pending(r))
}

// --- GET /api/admin/api-clients/{name}/reviews ---

func (d *Dashboard) listClientReviews(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" && !slices.Contains(apiStatuses, status) {
		writeError(w, http.StatusBadRequest, msgAPIStatus)
		return
	}
	// One more than a page tells whether there is a next one.
	f := store.APIReviewFilter{Tenant: q.Get("tenant"), Subject: q.Get("subject"), Status: status, Limit: reviewPageSize + 1}
	if c := q.Get("cursor"); c != "" {
		cur, ok := decodeCursor(c)
		if !ok {
			writeError(w, http.StatusBadRequest, "malformed cursor")
			return
		}
		f.After = &store.APIReviewCursor{CreatedAt: cur.At, ID: cur.ID}
	}
	client, ok := d.namedAPIClient(w, r)
	if !ok {
		return
	}
	f.ClientID = client.ID
	rows, err := d.Store.APIReviews(r.Context(), f)
	if err != nil {
		d.failed(w, r, "listing a client's reviews", err)
		return
	}
	view := APIReviewListView{Items: make([]APIReviewRowView, 0, len(rows))}
	if len(rows) > reviewPageSize {
		rows = rows[:reviewPageSize]
		last := rows[len(rows)-1]
		view.NextCursor = encodeCursorAt(last.CreatedAt, last.Run.ID)
	}
	for _, row := range rows {
		view.Items = append(view.Items, apiReviewRow(row))
	}
	writeJSON(w, http.StatusOK, view)
}

func apiReviewRow(r store.APIReview) APIReviewRowView {
	v := APIReviewRowView{ID: r.Run.ID, Tenant: r.Tenant, Subject: r.Subject, Title: r.Title,
		Status: reviewapi.StatusOf(r), CreatedAt: r.CreatedAt.UTC()}
	if !r.FinishedAt.IsZero() {
		t := r.FinishedAt.UTC()
		v.FinishedAt = &t
	}
	// The verdict is the stored body's; a review with none yet has the
	// pending body's (failed, or none).
	var body struct {
		Verdict string `json:"verdict"`
	}
	if len(r.Response) > 0 {
		_ = json.Unmarshal(r.Response, &body)
	} else {
		body.Verdict = reviewapi.Pending(r).Verdict
	}
	v.Verdict = body.Verdict
	return v
}

// --- GET /api/admin/api-reviews/{id} ---

func (d *Dashboard) getAPIReview(w http.ResponseWriter, r *http.Request) {
	rv, found, err := d.Store.APIReviewByID(r.Context(), r.PathValue("id"))
	if err != nil {
		d.failed(w, r, "loading an API review", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	body, err := reviewBody(rv)
	if err != nil {
		d.failed(w, r, "making an API review's body", err)
		return
	}
	v := APIReviewDetailView{ID: rv.Run.ID, Client: rv.ClientName, Tenant: rv.Tenant, Subject: rv.Subject, Title: rv.Title,
		KeyName: rv.KeyName, Status: reviewapi.StatusOf(rv), Model: rv.Run.Model, CreatedAt: rv.CreatedAt.UTC(), Review: body}
	if !rv.FinishedAt.IsZero() {
		t := rv.FinishedAt.UTC()
		v.FinishedAt = &t
	}
	if f := reviewapi.FailureOf(rv); f != nil {
		v.Failure = &APIReviewFailureView{Step: f.Step, Reason: f.Reason, Detail: rv.Run.FailureDetail}
	}
	state, attempts, err := d.Store.APICallbackAttempts(r.Context(), rv.Run.ID)
	if err != nil {
		d.failed(w, r, "loading an API review's callback", err)
		return
	}
	v.Callback = APICallbackView{State: state, Attempts: make([]APICallbackAttemptView, 0, len(attempts))}
	for _, a := range attempts {
		v.Callback.Attempts = append(v.Callback.Attempts, APICallbackAttemptView{At: a.At.UTC(), StatusCode: a.StatusCode,
			Error: a.Error, DurationMs: a.Duration.Milliseconds()})
	}
	events, err := d.Store.APIReviewEvents(r.Context(), rv.Run.ID)
	if err != nil {
		d.failed(w, r, "loading an API review's history", err)
		return
	}
	v.History = make([]APIReviewEventView, 0, len(events))
	for _, e := range events {
		v.History = append(v.History, APIReviewEventView{At: e.At.UTC(), Kind: e.Kind, Fingerprint: e.Fingerprint,
			Title: e.Title, Actor: e.Actor, Reason: e.Reason})
	}
	dismissals, err := d.Store.APIDismissals(r.Context(), rv.ClientID, rv.Tenant, rv.Subject)
	if err != nil {
		d.failed(w, r, "loading an API review's dismissals", err)
		return
	}
	v.Dismissals = make([]APIDismissalView, 0, len(dismissals))
	for _, x := range dismissals {
		v.Dismissals = append(v.Dismissals, APIDismissalView{Fingerprint: x.Fingerprint, Severity: x.Severity,
			Reason: x.Reason, Actor: x.Actor, CreatedAt: x.CreatedAt.UTC()})
	}
	writeJSON(w, http.StatusOK, v)
}

// --- POST and DELETE /api/admin/api-reviews/{id}/findings/{finding}/dismiss ---

// refuseDismissal answers a dismissal or a restore that was refused, and
// returns false for any other error. The review is not changed by either, and
// no callback goes out.
func refuseDismissal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, reviewapi.ErrBadReason), errors.Is(err, reviewapi.ErrReasonControl):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, reviewapi.ErrNotFinished):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, reviewapi.ErrNoSuchFinding):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		return false
	}
	return true
}

// dismissAPIFinding sets a finding of an API review aside for the review's
// subject, as the admin who calls. It is not audited: the review's history
// holds it.
func (d *Dashboard) dismissAPIFinding(w http.ResponseWriter, r *http.Request) {
	rv, found, err := d.Store.APIReviewByID(r.Context(), r.PathValue("id"))
	if err != nil {
		d.failed(w, r, "loading an API review", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	var req reviewapi.DismissRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgBadAPIDismissal)
		return
	}
	created, err := reviewapi.DismissFinding(r.Context(), d.Store, rv, r.PathValue("finding"), req.Reason, session(r).Username)
	if err != nil {
		if !refuseDismissal(w, err) {
			d.failed(w, r, "dismissing an API review's finding", err)
		}
		return
	}
	status := reviewapi.DismissedStatus
	if !created {
		status = reviewapi.AlreadyDismissedStatus
	}
	writeJSON(w, http.StatusOK, reviewapi.DismissResult{Status: status})
}

// restoreAPIFinding takes back a dismissal made on the review's subject.
func (d *Dashboard) restoreAPIFinding(w http.ResponseWriter, r *http.Request) {
	rv, found, err := d.Store.APIReviewByID(r.Context(), r.PathValue("id"))
	if err != nil {
		d.failed(w, r, "loading an API review", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	restored, err := reviewapi.RestoreFinding(r.Context(), d.Store, rv, r.PathValue("finding"), session(r).Username)
	if err != nil {
		if !refuseDismissal(w, err) {
			d.failed(w, r, "restoring an API review's finding", err)
		}
		return
	}
	status := reviewapi.RestoredStatus
	if !restored {
		status = reviewapi.NotDismissedStatus
	}
	writeJSON(w, http.StatusOK, reviewapi.DismissResult{Status: status})
}

// --- GET /api/admin/api-reviews/{id}/log and /diagnostics ---

// apiRunID returns the ID of the API review the path names; it answers 404 or
// 500 itself and returns false. A merge request's run is not found.
func (d *Dashboard) apiRunID(w http.ResponseWriter, r *http.Request) (string, bool) {
	run, found, err := d.Store.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		d.failed(w, r, "loading a run", err)
		return "", false
	}
	if !found || run.Kind != store.KindAPI {
		notFound(w)
		return "", false
	}
	return run.ID, true
}

func (d *Dashboard) apiReviewLog(w http.ResponseWriter, r *http.Request) {
	id, ok := d.apiRunID(w, r)
	if !ok {
		return
	}
	text, found, err := d.Store.RunLog(r.Context(), id)
	if err != nil {
		d.failed(w, r, "loading a run's log", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, LogView{Text: text})
}

func (d *Dashboard) apiReviewDiagnostics(w http.ResponseWriter, r *http.Request) {
	id, ok := d.apiRunID(w, r)
	if !ok {
		return
	}
	diag, found, err := d.Store.RunDiagnostics(r.Context(), id)
	if err != nil {
		d.failed(w, r, "loading a run's diagnostics", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	// Only admins reach this route: the clone log as it was and the pod's state.
	writeJSON(w, http.StatusOK, DiagnosticsView{CloneLog: diag.CloneLog, PodState: &diag.PodState})
}
