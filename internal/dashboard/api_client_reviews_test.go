package dashboard

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// apiReviewRows are the IDs of the rows of a client's list, in order.
func apiReviewRows(v APIReviewListView) []string {
	out := make([]string, 0, len(v.Items))
	for _, it := range v.Items {
		out = append(out, it.ID)
	}
	return out
}

// askAPIReview queues a review of subject under tenant for the client and
// returns its ID.
func (e *env) askAPIReview(client, tenant, subject string) string {
	e.t.Helper()
	id, created, err := e.st.EnqueueAPIReview(ctx, store.NewAPIReview{ClientID: mustClientID(e.t, e, client), Tenant: tenant,
		Subject: subject, Title: "Route change", KeyName: "ci", BaseSHA: "base-" + subject, HeadSHA: "head-" + subject,
		InputsSealed: []byte("sealed"), FileCount: 1, FileBytes: 10})
	if err != nil || !created {
		e.t.Fatalf("EnqueueAPIReview(%s/%s) = %v, %v", tenant, subject, created, err)
	}
	return id
}

// The list takes a tenant, a subject, a status and a cursor, newest first;
// another client's reviews are never in it.
func TestClientReviewsFilter(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	e.addAPIClient(c, goodClient(nil))
	e.addAPIClient(c, goodClient(func(m map[string]any) { m["name"] = "other" }))
	base := "/api/admin/api-clients/fastgateway/reviews"

	done := e.askAPIReview("fastgateway", "acme", "s1")
	e.exec(`UPDATE runs SET state = 'published', result = '{"status":"completed"}'::jsonb WHERE id = $1::uuid`, done)
	e.exec(`UPDATE api_reviews SET response = '{"id": "x", "verdict": "no_blocking_issues"}'::jsonb, finished_at = $2
		WHERE run_id = $1::uuid`, done, time.Date(2026, 10, 1, 10, 5, 0, 0, time.UTC))
	broke := e.askAPIReview("fastgateway", "acme", "s2")
	e.exec(`UPDATE runs SET state = 'published', failure_reason = 'the review failed', failure_step = 'review'
		WHERE id = $1::uuid`, broke)
	queued := e.askAPIReview("fastgateway", "beta", "s1")
	e.askAPIReview("other", "acme", "s1")
	for i, id := range []string{done, broke, queued} {
		e.exec(`UPDATE api_reviews SET created_at = $2 WHERE run_id = $1::uuid`, id,
			time.Date(2026, 10, 1, 10, i, 0, 0, time.UTC))
	}

	for name, tc := range map[string]struct {
		query string
		want  []string
	}{
		"all, newest first": {"", []string{queued, broke, done}},
		"tenant":            {"?tenant=acme", []string{broke, done}},
		"subject":           {"?subject=s1", []string{queued, done}},
		"status":            {"?status=failed", []string{broke}},
		"queued":            {"?status=queued", []string{queued}},
		"all three":         {"?tenant=acme&subject=s2&status=failed", []string{broke}},
		"no match":          {"?tenant=nobody", []string{}},
	} {
		v := getJSON[APIReviewListView](e, c, base+tc.query)
		if got := apiReviewRows(v); !slices.Equal(got, tc.want) || v.NextCursor != "" {
			t.Errorf("%s: got %v (cursor %q), want %v", name, got, v.NextCursor, tc.want)
		}
	}

	// A row: the stored verdict, a failed review's verdict, and none for a queued one.
	rows := map[string]APIReviewRowView{}
	for _, it := range getJSON[APIReviewListView](e, c, base).Items {
		rows[it.ID] = it
	}
	if r := rows[done]; r.Tenant != "acme" || r.Subject != "s1" || r.Title != "Route change" || r.Status != "completed" ||
		r.Verdict != "no_blocking_issues" || r.FinishedAt == nil || !r.FinishedAt.Equal(time.Date(2026, 10, 1, 10, 5, 0, 0, time.UTC)) {
		t.Errorf("completed row = %+v", r)
	}
	if r := rows[broke]; r.Status != "failed" || r.Verdict != "failed" || r.FinishedAt != nil {
		t.Errorf("failed row = %+v", r)
	}
	if r := rows[queued]; r.Status != "queued" || r.Verdict != "" || r.FinishedAt != nil {
		t.Errorf("queued row = %+v", r)
	}

	rec := e.get(c, base+"?status=published")
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != "status must be one of queued, running, completed, failed, superseded" {
		t.Errorf("a bad status = %d %s", rec.Code, rec.Body)
	}
	e.wantStatus(c, base+"?cursor=not-a-cursor", http.StatusBadRequest)
	e.wantStatus(c, "/api/admin/api-clients/nobody/reviews", http.StatusNotFound)
	e.wantStatus(e.aliceSession(), base, http.StatusNotFound)
}

// A page holds 50 reviews, and the cursor continues after the last one.
func TestClientReviewsCursor(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	e.addAPIClient(c, goodClient(nil))
	base := "/api/admin/api-clients/fastgateway/reviews"

	var ids []string // oldest first
	for i := range 55 {
		ids = append(ids, e.askAPIReview("fastgateway", "acme", "subject-"+string(rune('a'+i/26))+string(rune('a'+i%26))))
		e.exec(`UPDATE api_reviews SET created_at = $2 WHERE run_id = $1::uuid`, ids[i],
			time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Minute))
	}
	newest := make([]string, len(ids))
	for i, id := range ids {
		newest[len(ids)-1-i] = id
	}

	first := getJSON[APIReviewListView](e, c, base)
	if got := apiReviewRows(first); !slices.Equal(got, newest[:50]) || first.NextCursor == "" {
		t.Fatalf("first page: %d rows, cursor %q; want the 50 newest and a cursor", len(got), first.NextCursor)
	}
	second := getJSON[APIReviewListView](e, c, base+"?cursor="+url.QueryEscape(first.NextCursor))
	if got := apiReviewRows(second); !slices.Equal(got, newest[50:]) || second.NextCursor != "" {
		t.Errorf("second page: %v (cursor %q), want the 5 oldest and no cursor", got, second.NextCursor)
	}
}

// compact decodes JSON, so two bodies compare whatever their spacing.
func compact(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

// The page of a published review holds its stored body, and the page of a
// review that failed at dispatch holds the failure with the admin's detail.
func TestAPIReviewDetail(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	e.addAPIClient(c, goodClient(nil))

	done := e.askAPIReview("fastgateway", "acme", "route:1")
	stored := `{"id":"` + done + `","subject":"route:1","tenant":"acme","title":"Route change","status":"completed",
		"created_at":"2026-10-01T10:00:00Z","finished_at":"2026-10-01T10:05:00Z","verdict":"no_blocking_issues",
		"skills":[{"name":"security","verdict":"no_blocking_issues","findings":[],"files":{"reviewed":["a.go"],"skipped":[],"not_reviewed":[]}}],
		"usage":{"tokens_in":100,"tokens_out":20}}`
	e.exec(`UPDATE runs SET state = 'published', model = 'claude-x', result = '{"status":"completed"}'::jsonb WHERE id = $1::uuid`, done)
	e.exec(`UPDATE api_reviews SET response = $2::jsonb, finished_at = $3 WHERE run_id = $1::uuid`, done, stored,
		time.Date(2026, 10, 1, 10, 5, 0, 0, time.UTC))

	v := getJSON[APIReviewDetailView](e, c, "/api/admin/api-reviews/"+done)
	if v.ID != done || v.Client != "fastgateway" || v.Tenant != "acme" || v.Subject != "route:1" || v.Title != "Route change" ||
		v.KeyName != "ci" || v.Status != "completed" || v.Model == nil || *v.Model != "claude-x" || v.Failure != nil ||
		v.FinishedAt == nil || !v.FinishedAt.Equal(time.Date(2026, 10, 1, 10, 5, 0, 0, time.UTC)) {
		t.Errorf("detail = %+v", v)
	}
	if !reflect.DeepEqual(compact(t, v.Review), compact(t, []byte(stored))) {
		t.Errorf("review = %s, want the stored body %s", v.Review, stored)
	}

	// A dispatch failure: the Job never started. The review key is the body
	// the API builds; only the page's own failure holds the detail.
	broke := e.askAPIReview("fastgateway", "acme", "route:2")
	const detail = `jobs.batch "pergent-review-x" is forbidden: exceeded quota`
	e.exec(`UPDATE runs SET state = 'published', failure_reason = 'the review could not start', failure_step = 'starting',
		failure_detail = $2 WHERE id = $1::uuid`, broke, detail)
	rec := e.get(c, "/api/admin/api-reviews/"+broke)
	v = decode[APIReviewDetailView](t, rec, "GET failed review", http.StatusOK)
	if v.Status != "failed" || v.Model != nil || v.Failure == nil ||
		*v.Failure != (APIReviewFailureView{Step: "starting", Reason: "the review could not start", Detail: detail}) {
		t.Errorf("failed detail = %+v, failure %+v", v, v.Failure)
	}
	var body reviewapi.Review
	if err := json.Unmarshal(v.Review, &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != broke || body.Status != "failed" || body.Verdict != "failed" || body.Failure == nil ||
		*body.Failure != (reviewapi.Failure{Step: "starting", Reason: "the review could not start"}) {
		t.Errorf("review = %s, want the pending failed body", v.Review)
	}
	if strings.Count(rec.Body.String(), "exceeded quota") != 1 {
		t.Errorf("the detail is in the page %d times, want once (failure.detail only): %s",
			strings.Count(rec.Body.String(), "exceeded quota"), rec.Body)
	}

	// A review still waiting has the pending body, with no failure.
	queued := e.askAPIReview("fastgateway", "acme", "route:3")
	v = getJSON[APIReviewDetailView](e, c, "/api/admin/api-reviews/"+queued)
	if v.Status != "queued" || v.Failure != nil || v.FinishedAt != nil {
		t.Errorf("queued detail = %+v", v)
	}
	var pending reviewapi.Review
	if err := json.Unmarshal(v.Review, &pending); err != nil || pending.ID != queued || pending.Status != "queued" ||
		pending.Verdict != "" || pending.Skills != nil {
		t.Errorf("queued review = %s, %v", v.Review, err)
	}

	e.wantStatus(c, "/api/admin/api-reviews/00000000-0000-0000-0000-000000000000", http.StatusNotFound)
	e.wantStatus(c, "/api/admin/api-reviews/not-an-id", http.StatusNotFound)
	e.wantStatus(e.aliceSession(), "/api/admin/api-reviews/"+done, http.StatusNotFound)
}

// The log and the diagnostics of an API review come from the readers the
// merge request pages use; an admin sees the clone log and the pod's state.
func TestAPIReviewLogAndDiagnostics(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	e.addAPIClient(c, goodClient(nil))
	id := e.askAPIReview("fastgateway", "acme", "route:1")
	base := "/api/admin/api-reviews/" + id

	e.wantStatus(c, base+"/log", http.StatusNotFound)
	e.wantStatus(c, base+"/diagnostics", http.StatusNotFound)

	if err := e.st.PutRunLog(ctx, id, "review log line"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.PutRunDiagnostics(ctx, id, store.Diagnostics{CloneLog: "clone log", CloneLogMasked: "masked", PodState: "Failed: OOMKilled"}); err != nil {
		t.Fatal(err)
	}
	if v := getJSON[LogView](e, c, base+"/log"); v.Text != "review log line" {
		t.Errorf("log = %+v", v)
	}
	v := getJSON[DiagnosticsView](e, c, base+"/diagnostics")
	if v.CloneLog != "clone log" || v.PodState == nil || *v.PodState != "Failed: OOMKilled" {
		t.Errorf("diagnostics = %+v", v)
	}
	e.wantStatus(e.aliceSession(), base+"/log", http.StatusNotFound)
	e.wantStatus(e.aliceSession(), base+"/diagnostics", http.StatusNotFound)
}

// A merge request's run is not an API review: none of the routes find it, and
// the API review routes of its ID answer as for an unknown one.
func TestAPIReviewDetailRefusesMRRun(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	id := e.addRun(runFix{project: "acme/widgets", number: 1, head: "aaa", result: `{"status":"completed"}`})
	if err := e.st.PutRunLog(ctx, id, "a merge request's log"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.PutRunDiagnostics(ctx, id, store.Diagnostics{CloneLog: "clone log", PodState: "pod"}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"", "/log", "/diagnostics"} {
		rec := e.wantStatus(c, "/api/admin/api-reviews/"+id+target, http.StatusNotFound)
		if strings.Contains(rec.Body.String(), "a merge request's log") || strings.Contains(rec.Body.String(), "clone log") {
			t.Errorf("GET api-reviews/%s%s leaked the run: %s", id, target, rec.Body)
		}
	}
}

// The list of clients counts this month's reviews (from the 1st, UTC), the
// failed ones and their tokens, and leaves last month's out.
func TestClientListMonthCounts(t *testing.T) {
	e := newEnv(t, nil) // the clock is 2026-10-31
	c := e.signInOperator()
	e.addAPIClient(c, goodClient(nil))
	e.addAPIClient(c, goodClient(func(m map[string]any) { m["name"] = "quiet" }))
	wantCode(t, e.sendJSON(c, "POST", apiClientsURL+"/fastgateway/keys", map[string]any{"name": "ci"}), "POST key", http.StatusCreated)

	usage := func(id string, in, out int64) {
		t.Helper()
		if err := e.st.RecordUsage(ctx, id, []store.SkillUsage{{Skill: "security", Verdict: "no_blocking_issues", In: in, Out: out}}); err != nil {
			t.Fatal(err)
		}
	}
	at := func(id string, t0 time.Time) {
		e.exec(`UPDATE api_reviews SET created_at = $2 WHERE run_id = $1::uuid`, id, t0)
	}
	good := e.askAPIReview("fastgateway", "acme", "s1")
	e.exec(`UPDATE runs SET state = 'published', result = '{"status":"completed"}'::jsonb WHERE id = $1::uuid`, good)
	usage(good, 100, 50)
	at(good, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	bad := e.askAPIReview("fastgateway", "acme", "s2")
	e.exec(`UPDATE runs SET state = 'published', result = '{"status":"failed"}'::jsonb WHERE id = $1::uuid`, bad)
	usage(bad, 25, 5)
	at(bad, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	last := e.askAPIReview("fastgateway", "acme", "s3")
	e.exec(`UPDATE runs SET state = 'published', result = '{"status":"failed"}'::jsonb WHERE id = $1::uuid`, last)
	usage(last, 9000, 999)
	at(last, time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC))

	got := map[string]APIClientRowView{}
	for _, it := range getJSON[APIClientListView](e, c, apiClientsURL).Items {
		got[it.Name] = it
	}
	if want := (APIClientRowView{Name: "fastgateway", Keys: 1, ReviewsThisMonth: 2, FailedThisMonth: 1, TokensThisMonth: 180}); got["fastgateway"] != want {
		t.Errorf("fastgateway = %+v, want %+v", got["fastgateway"], want)
	}
	if want := (APIClientRowView{Name: "quiet"}); got["quiet"] != want {
		t.Errorf("quiet = %+v, want %+v", got["quiet"], want)
	}
}
