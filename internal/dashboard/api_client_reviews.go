package dashboard

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const msgAPIStatus = "status must be one of queued, running, completed, failed, superseded"

// apiStatuses are the statuses a review has, and the ones the list takes.
var apiStatuses = []string{reviewapi.StatusQueued, reviewapi.StatusRunning, reviewapi.StatusCompleted,
	reviewapi.StatusFailed, reviewapi.StatusSuperseded}

func (d *Dashboard) apiClientReviewRoutes(mux *http.ServeMux) {
	d.adminRoute(mux, "GET /api/admin/api-clients/{name}/reviews", d.listClientReviews)
	d.adminRoute(mux, "GET /api/admin/api-reviews/{id}", d.getAPIReview)
	d.adminRoute(mux, "GET /api/admin/api-reviews/{id}/log", d.apiReviewLog)
	d.adminRoute(mux, "GET /api/admin/api-reviews/{id}/diagnostics", d.apiReviewDiagnostics)
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
	writeJSON(w, http.StatusOK, v)
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
