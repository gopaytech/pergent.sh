package reviewapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/store"
)

type fakeEvent struct {
	Run, Kind, Fingerprint, Title, Actor string
}

func dismissalSlot(clientID int64, tenant, subject, fingerprint string) string {
	return fmt.Sprintf("%d/%s/%s/%s", clientID, tenant, subject, fingerprint)
}

// DismissAPIFinding is the real one's contract: the first dismissal of a
// fingerprint is stored with an event, a second changes nothing.
func (f *fakeStore) DismissAPIFinding(_ context.Context, d store.APIDismissal, runID string) (bool, error) {
	if f.dismissErr != nil {
		return false, f.dismissErr
	}
	slot := dismissalSlot(d.ClientID, d.Tenant, d.Subject, d.Fingerprint)
	if _, ok := f.dismissals[slot]; ok {
		return false, nil
	}
	if f.dismissals == nil {
		f.dismissals = map[string]store.APIDismissal{}
	}
	f.dismissals[slot] = d
	f.events = append(f.events, fakeEvent{runID, store.APIEventDismissed, d.Fingerprint, d.Title, d.Actor})
	return true, nil
}

func (f *fakeStore) UndismissAPIFinding(_ context.Context, clientID int64, tenant, subject, fingerprint, runID, actor string) (bool, error) {
	if f.dismissErr != nil {
		return false, f.dismissErr
	}
	slot := dismissalSlot(clientID, tenant, subject, fingerprint)
	d, ok := f.dismissals[slot]
	if !ok {
		return false, nil
	}
	delete(f.dismissals, slot)
	f.events = append(f.events, fakeEvent{runID, store.APIEventUndismissed, fingerprint, d.Title, actor})
	return true, nil
}

const (
	completedID = "00000000-0000-4000-8000-0000000000c1"
	findingID   = "f1"
	suggestID   = "f2"
)

// completedReview stores a published review of the key's client (3) whose
// result holds two findings: f1, blocking, and f2, a suggestion.
func completedReview(t *testing.T, fs *fakeStore, id string, clientID int64) store.APIReview {
	t.Helper()
	body, err := json.Marshal(Review{ID: id, Subject: "route:1", Tenant: "acme", Status: StatusCompleted,
		Verdict: VerdictHasBlockingIssues,
		Skills: []Skill{{Name: "security", Verdict: VerdictHasBlockingIssues, Findings: []Finding{
			{ID: findingID, Fingerprint: "fp-sql", File: "a.go", Line: 3, Severity: "blocking", Title: "SQL built from input", Explanation: "e"},
			{ID: suggestID, Fingerprint: "fp-name", File: "b.go", Severity: "suggestion", Title: "Rename x", Explanation: "e"},
		}}}})
	if err != nil {
		t.Fatal(err)
	}
	rv := store.APIReview{
		Run:      store.Run{ID: id, State: store.Published, Result: []byte(`{"status":"completed"}`)},
		ClientID: clientID, Tenant: "acme", Subject: "route:1", Title: "Route change", KeyName: "ci",
		Response: append(body, '\n'), CreatedAt: testNow, FinishedAt: testNow,
	}
	if fs.reviews == nil {
		fs.reviews = map[string]store.APIReview{}
	}
	fs.reviews[id] = rv
	return rv
}

func dismissCall(mux *http.ServeMux, method, key, id, finding, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/v1/reviews/"+id+"/findings/"+finding+"/dismiss", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var got DismissResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Status != want {
		t.Fatalf("body %q, want status %q", rec.Body, want)
	}
}

func TestDismissFinding(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	completedReview(t, fs, completedID, 3)

	rec := dismissCall(mux, "POST", key, completedID, findingID, `{"reason":"  checked upstream  "}`)
	wantStatus(t, rec, "dismissed")
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the answer is cacheable")
	}
	d, ok := fs.dismissals[dismissalSlot(3, "acme", "route:1", "fp-sql")]
	if !ok {
		t.Fatalf("nothing stored under the subject: %+v", fs.dismissals)
	}
	if d.ClientID != 3 || d.Tenant != "acme" || d.Subject != "route:1" || d.Fingerprint != "fp-sql" || d.Severity != "blocking" ||
		d.Skill != "security" || d.File != "a.go" || d.Title != "SQL built from input" || d.Reason != "checked upstream" ||
		d.Actor != "key ci" {
		t.Errorf("stored %+v", d)
	}

	wantStatus(t, dismissCall(mux, "POST", key, completedID, findingID, `{"reason":"again"}`), "already_dismissed")
	if len(fs.events) != 1 || fs.events[0] != (fakeEvent{completedID, "dismissed", "fp-sql", "SQL built from input", "key ci"}) {
		t.Errorf("events = %+v", fs.events)
	}

	// A suggestion is stored as one.
	wantStatus(t, dismissCall(mux, "POST", key, completedID, suggestID, `{"reason":"style"}`), "dismissed")
	if got := fs.dismissals[dismissalSlot(3, "acme", "route:1", "fp-name")].Severity; got != "suggestion" {
		t.Errorf("severity %q, want suggestion", got)
	}

	// Restore, then restore again.
	wantStatus(t, dismissCall(mux, "DELETE", key, completedID, findingID, ""), "restored")
	wantStatus(t, dismissCall(mux, "DELETE", key, completedID, findingID, ""), "not_dismissed")
	if _, ok := fs.dismissals[dismissalSlot(3, "acme", "route:1", "fp-sql")]; ok {
		t.Error("the dismissal is still stored")
	}
	if n := len(fs.events); n != 3 || fs.events[2].Kind != "undismissed" || fs.events[2].Actor != "key ci" {
		t.Errorf("events = %+v", fs.events)
	}
}

func TestDismissNeedsReason(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	completedReview(t, fs, completedID, 3)
	const msg = "a dismissal needs a reason of at most 500 characters"

	for name, body := range map[string]string{
		"no body field":  `{}`,
		"empty":          `{"reason":""}`,
		"spaces":         `{"reason":"   "}`,
		"only an image":  `{"reason":"![x](https://example.com/p.png)"}`,
		"501 characters": `{"reason":"` + strings.Repeat("é", 501) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, dismissCall(mux, "POST", key, completedID, findingID, body), http.StatusUnprocessableEntity, CodeInvalid, msg)
		})
	}
	// 500 characters (not bytes) is fine.
	wantStatus(t, dismissCall(mux, "POST", key, completedID, findingID, `{"reason":"`+strings.Repeat("é", 500)+`"}`), "dismissed")

	wantError(t, dismissCall(mux, "POST", key, completedID, suggestID, `{"reason":`), http.StatusUnprocessableEntity, CodeInvalid, notJSONMessage)
	wantError(t, dismissCall(mux, "POST", key, completedID, suggestID, `{"reason":5}`), http.StatusUnprocessableEntity, CodeInvalid, "reason has the wrong type")
	wantError(t, dismissCall(mux, "POST", key, completedID, suggestID, `{"reason":"a\u0000b"}`), http.StatusUnprocessableEntity, CodeInvalid,
		"the reason must not hold a control character")
	wantError(t, dismissCall(mux, "POST", key, completedID, suggestID, "{\"reason\":\"\xff\"}"), http.StatusUnprocessableEntity, CodeInvalid, notUTF8Message)
	wantError(t, dismissCall(mux, "POST", key, completedID, suggestID, `{"reason":"`+strings.Repeat("x", maxDismissBody)+`"}`),
		http.StatusRequestEntityTooLarge, CodeTooLarge, fmt.Sprintf("the request is over %d bytes", maxDismissBody))
	if len(fs.dismissals) != 1 {
		t.Errorf("a refused dismissal was stored: %+v", fs.dismissals)
	}
}

func TestDismissBeforeCompleted(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	const msg = "findings can be dismissed once the review is completed"

	queued := completedReview(t, fs, "00000000-0000-4000-8000-0000000000a1", 3)
	queued.Run.State, queued.Response = store.Queued, nil
	fs.reviews[queued.Run.ID] = queued
	failed := completedReview(t, fs, "00000000-0000-4000-8000-0000000000a2", 3)
	failed.Run.Result = []byte(`{"status":"error"}`)
	fs.reviews[failed.Run.ID] = failed

	for _, id := range []string{queued.Run.ID, failed.Run.ID} {
		wantError(t, dismissCall(mux, "POST", key, id, findingID, `{"reason":"r"}`), http.StatusConflict, CodeNotFinished, msg)
		wantError(t, dismissCall(mux, "DELETE", key, id, findingID, ""), http.StatusConflict, CodeNotFinished, msg)
	}
	if len(fs.dismissals) != 0 || len(fs.events) != 0 {
		t.Errorf("stored %+v, %+v", fs.dismissals, fs.events)
	}
}

func TestDismissUnknownFinding(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	completedReview(t, fs, completedID, 3)
	// The id is the finding's id in this review, not its fingerprint.
	for _, id := range []string{"f9", "fp-sql", "F1"} {
		wantError(t, dismissCall(mux, "POST", key, completedID, id, `{"reason":"r"}`), http.StatusNotFound, CodeNotFound, "finding not found")
	}
	wantError(t, dismissCall(mux, "DELETE", key, completedID, "f9", ""), http.StatusNotFound, CodeNotFound, "finding not found")
}

// Another client's review, an unknown ID and a malformed one are one answer,
// whatever the body is.
func TestDismissOtherClientsReview(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	const theirs = "00000000-0000-4000-8000-0000000000b1"
	completedReview(t, fs, theirs, 99)

	for _, body := range []string{`{"reason":"r"}`, `{}`, `not json`, ""} {
		for _, id := range []string{theirs, "00000000-0000-4000-8000-0000000000ff", "not-a-uuid"} {
			rec := dismissCall(mux, "POST", key, id, findingID, body)
			if rec.Code != http.StatusNotFound || rec.Body.String() != notFoundBody {
				t.Errorf("POST %s with %q = %d %q, want the unknown-ID 404", id, body, rec.Code, rec.Body)
			}
		}
	}
	for _, id := range []string{theirs, "not-a-uuid"} {
		rec := dismissCall(mux, "DELETE", key, id, findingID, "")
		if rec.Code != http.StatusNotFound || rec.Body.String() != notFoundBody {
			t.Errorf("DELETE %s = %d %q", id, rec.Code, rec.Body)
		}
	}
	if len(fs.dismissals) != 0 || len(fs.events) != 0 {
		t.Errorf("stored %+v, %+v", fs.dismissals, fs.events)
	}
}

func TestDismissNeedsKey(t *testing.T) {
	_, _, _, mux := newHandler(t)
	for _, m := range []string{"POST", "DELETE"} {
		req := httptest.NewRequest(m, "/v1/reviews/"+completedID+"/findings/f1/dismiss", strings.NewReader(`{"reason":"r"}`))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		wantError(t, rec, http.StatusUnauthorized, CodeUnauthorized, unauthorizedMessage)
	}
}

func TestDismissStoreFailureIs500(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	completedReview(t, fs, completedID, 3)
	fs.dismissErr = fmt.Errorf("boom: secret detail")
	for _, m := range []string{"POST", "DELETE"} {
		rec := dismissCall(mux, m, key, completedID, findingID, `{"reason":"r"}`)
		wantError(t, rec, http.StatusInternalServerError, CodeInternal, "internal error")
		if strings.Contains(rec.Body.String(), "secret") {
			t.Error("the error body leaks the cause")
		}
	}
}

// A dismissal changes nothing about the review it was made on.
func TestDismissChangesNothing(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	completedReview(t, fs, completedID, 3)
	before := call(mux, "GET", "/v1/reviews/"+completedID, "Bearer "+key)
	if before.Code != http.StatusOK {
		t.Fatalf("GET = %d", before.Code)
	}
	wantStatus(t, dismissCall(mux, "POST", key, completedID, findingID, `{"reason":"r"}`), "dismissed")
	wantStatus(t, dismissCall(mux, "DELETE", key, completedID, findingID, ""), "restored")
	wantStatus(t, dismissCall(mux, "POST", key, completedID, findingID, `{"reason":"r"}`), "dismissed")
	after := call(mux, "GET", "/v1/reviews/"+completedID, "Bearer "+key)
	if after.Code != http.StatusOK || after.Body.String() != before.Body.String() {
		t.Errorf("the review changed:\nbefore %s\nafter  %s", before.Body, after.Body)
	}
	if strings.Contains(after.Body.String(), "key ci") || strings.Contains(after.Body.String(), `"dismissed"`) {
		t.Errorf("the body shows the dismissal: %s", after.Body)
	}
}

// A finding with no fingerprint can't be dismissed: every such finding of the
// subject would share one dismissal.
func TestDismissFindingWithoutFingerprint(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	rv := completedReview(t, fs, completedID, 3)
	var body Review
	if err := json.Unmarshal(rv.Response, &body); err != nil {
		t.Fatal(err)
	}
	body.Skills[0].Findings[0].Fingerprint = ""
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rv.Response = b
	fs.reviews[completedID] = rv

	wantError(t, dismissCall(mux, "POST", key, completedID, findingID, `{"reason":"r"}`), http.StatusNotFound, CodeNotFound, "finding not found")
	wantError(t, dismissCall(mux, "DELETE", key, completedID, findingID, ""), http.StatusNotFound, CodeNotFound, "finding not found")
	if len(fs.events) != 0 || len(fs.dismissals) != 0 {
		t.Errorf("a finding with no fingerprint was dismissed: %v, %v", fs.events, fs.dismissals)
	}
}
