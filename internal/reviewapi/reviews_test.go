package reviewapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const notFoundBody = `{"code":"not_found","message":"review not found"}` + "\n"

// post sends body to POST /v1/reviews with the key and, in headers, name/value
// pairs.
func post(mux *http.ServeMux, key, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/reviews", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// goodBody is a request with one changed and one new file.
const goodBody = `{"subject":"route:1","tenant":"acme","title":"Route change","description":"Rate limit the route.",` +
	`"files":[{"path":"a.go","old":"package a\n","new":"package a\n\nvar x = 1\n"},{"path":"b.go","new":"package b\n"}]}`

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) ErrorBody {
	t.Helper()
	var e ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q is not an error: %v", rec.Body.String(), err)
	}
	return e
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status %d, want %d (body %s)", rec.Code, status, rec.Body)
		return
	}
	if e := decodeError(t, rec); e.Code != code || e.Message != message {
		t.Errorf("error = %+v, want %s: %s", e, code, message)
	}
}

func TestCreateReviewQueues(t *testing.T) {
	h, fs, key, mux := newHandler(t)
	woke := 0
	h.Wake = func() { woke++ }

	// Unknown fields are ignored.
	body := strings.Replace(goodBody, `"tenant"`, `"extra":{"a":[1]},"tenant"`, 1)
	rec := post(mux, key, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202 (body %s)", rec.Code, rec.Body)
	}
	if len(fs.enqueued) != 1 || woke != 1 {
		t.Fatalf("%d enqueued and %d wakes, want 1 and 1", len(fs.enqueued), woke)
	}
	n := fs.enqueued[0]

	files := []File{
		{Path: "a.go", Old: str("package a\n"), New: str("package a\n\nvar x = 1\n")},
		{Path: "b.go", New: str("package b\n")},
	}
	im, err := BuildImport(files)
	if err != nil {
		t.Fatal(err)
	}
	if n.BaseSHA != im.Base || n.HeadSHA != im.Head {
		t.Errorf("base, head = %s, %s; want %s, %s", n.BaseSHA, n.HeadSHA, im.Base, im.Head)
	}
	if n.ClientID != 3 || n.KeyName != "ci" || n.Subject != "route:1" || n.Tenant != "acme" || n.Title != "Route change" {
		t.Errorf("enqueued %+v", n)
	}
	if n.FileCount != 2 || n.FileBytes != im.Bytes {
		t.Errorf("file count, bytes = %d, %d; want 2, %d", n.FileCount, n.FileBytes, im.Bytes)
	}
	if sum := sha256.Sum256([]byte(body)); !bytes.Equal(n.RequestHash, sum[:]) {
		t.Errorf("request hash is not the SHA-256 of the raw body")
	}
	plain, err := h.Box.Open(n.InputsSealed)
	if err != nil {
		t.Fatalf("the sealed inputs do not open: %v", err)
	}
	var sealed Sealed
	if err := json.Unmarshal(plain, &sealed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sealed.Import, im.Stream) || sealed.Description != "Rate limit the route." {
		t.Errorf("sealed stream or description differ from what was sent")
	}
	if bytes.Contains(n.InputsSealed, []byte("Rate limit")) || bytes.Contains(n.InputsSealed, []byte("package a")) {
		t.Error("the inputs are stored in the clear")
	}

	var got Review
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued || got.Subject != "route:1" || got.Tenant != "acme" || got.Title != "Route change" {
		t.Errorf("body = %+v", got)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/reviews/"+got.ID || got.ID == "" {
		t.Errorf("Location %q, id %q", loc, got.ID)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the answer is cacheable")
	}
	if strings.Contains(rec.Body.String(), "Rate limit") {
		t.Error("the description is in the answer")
	}
}

func TestCreateReviewLimitsAreInclusive(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	body, _ := json.Marshal(map[string]any{
		"subject": strings.Repeat("s", 200), "tenant": strings.Repeat("t", 100), "title": strings.Repeat("é", 200),
		"description": strings.Repeat("d", 32*1024),
		"files":       []File{{Path: "a.go", New: str("x")}},
	})
	if rec := post(mux, key, string(body)); rec.Code != http.StatusAccepted {
		t.Errorf("status %d, want 202 (body %s)", rec.Code, rec.Body)
	}
	if len(fs.enqueued) != 1 {
		t.Errorf("%d enqueued, want 1", len(fs.enqueued))
	}
}

func TestCreateReviewRules(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	one := `"files":[{"path":"a.go","new":"x"}]`
	cases := []struct {
		name, body string
		headers    []string
		message    string
	}{
		{"not JSON", `{`, nil, "the body is not valid JSON"},
		{"empty body", ``, nil, "the body is not valid JSON"},
		{"trailing data", `{"subject":"s",` + one + `} x`, nil, "the body is not valid JSON"},
		{"an array", `[]`, nil, "the body must be a JSON object"},
		{"wrong type", `{"subject":5,` + one + `}`, nil, "subject has the wrong type"},
		{"no subject", `{` + one + `}`, nil, "subject must be 1 to 200 characters"},
		{"subject too long", `{"subject":"` + strings.Repeat("s", 201) + `",` + one + `}`, nil, "subject must be 1 to 200 characters"},
		{"subject control", `{"subject":"a\u0007b",` + one + `}`, nil, "subject must not hold a control character"},
		{"subject newline", `{"subject":"a\nb",` + one + `}`, nil, "subject must not hold a control character"},
		{"tenant too long", `{"subject":"s","tenant":"` + strings.Repeat("t", 101) + `",` + one + `}`, nil, "tenant must be at most 100 characters"},
		{"tenant control", `{"subject":"s","tenant":"a\u007fb",` + one + `}`, nil, "tenant must not hold a control character"},
		{"title too long", `{"subject":"s","title":"` + strings.Repeat("t", 201) + `",` + one + `}`, nil, "title must be at most 200 characters"},
		{"title control", `{"subject":"s","title":"a\tb",` + one + `}`, nil, "title must not hold a control character"},
		{"description too long", `{"subject":"s","description":"` + strings.Repeat("d", 32*1024+1) + `",` + one + `}`, nil, "description must be at most 32768 bytes"},
		{"no files", `{"subject":"s"}`, nil, "files must not be empty"},
		{"empty files", `{"subject":"s","files":[]}`, nil, "files must not be empty"},
		{"a bad file", `{"subject":"s","files":[{"path":"a.go","new":"x"},{"path":"/b.go","new":"x"}]}`, nil, "files[1]: the path must be relative"},
		{"a file with no content", `{"subject":"s","files":[{"path":"a.go"}]}`, nil, "files[0]: a file needs old, new or both"},
		{"key too long", `{"subject":"s",` + one + `}`, []string{"Idempotency-Key", strings.Repeat("k", 201)}, "Idempotency-Key must be 1 to 200 printable ASCII characters"},
		{"key not ASCII", `{"subject":"s",` + one + `}`, []string{"Idempotency-Key", "café"}, "Idempotency-Key must be 1 to 200 printable ASCII characters"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantError(t, post(mux, key, c.body, c.headers...), http.StatusUnprocessableEntity, CodeInvalid, c.message)
		})
	}
	if len(fs.enqueued) != 0 {
		t.Errorf("%d reviews enqueued by refused requests", len(fs.enqueued))
	}
}

func TestCreateReviewEmptyIdempotencyKeyHeader(t *testing.T) {
	_, _, key, mux := newHandler(t)
	req := httptest.NewRequest("POST", "/v1/reviews", strings.NewReader(goodBody))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	wantError(t, rec, http.StatusUnprocessableEntity, CodeInvalid, "Idempotency-Key must be 1 to 200 printable ASCII characters")
}

func TestCreateReviewTooLarge(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	prefix, _ := ParseKey(key)
	setLimits := func(f func(*store.APILimits)) {
		k := fs.keys[prefix]
		f(&k.client.Limits)
		fs.keys[prefix] = k
	}

	// The limit counts the body: exactly that many bytes pass, one more does not.
	const head = `{"subject":"s","files":[{"path":"a.go","new":"x"}],"pad":"`
	pad := func(n int) string { return head + strings.Repeat("p", n-len(head)-len(`"}`)) + `"}` }
	const limit = 2048
	setLimits(func(l *store.APILimits) { l.MaxRequestBytes = limit })
	exact := pad(limit)
	if len(exact) != limit {
		t.Fatalf("test body is %d bytes, want %d", len(exact), limit)
	}
	if rec := post(mux, key, exact); rec.Code != http.StatusAccepted {
		t.Errorf("a body of exactly the limit: status %d, want 202 (%s)", rec.Code, rec.Body)
	}
	wantError(t, post(mux, key, exact+" "), http.StatusRequestEntityTooLarge, CodeTooLarge, "the request is over 2048 bytes")

	// A client's limit never lifts the hard one.
	setLimits(func(l *store.APILimits) { l.MaxRequestBytes = 10 << 20 })
	big := `{"subject":"s","files":[{"path":"a.go","new":"` + strings.Repeat("x", store.HardMaxRequestBytes) + `"}]}`
	wantError(t, post(mux, key, big), http.StatusRequestEntityTooLarge, CodeTooLarge, "the request is over 819200 bytes")

	// More files than the client may send.
	setLimits(func(l *store.APILimits) { l.MaxRequestBytes = store.HardMaxRequestBytes; l.MaxFiles = 2 })
	three := `{"subject":"s","files":[{"path":"a","new":"x"},{"path":"b","new":"x"},{"path":"c","new":"x"}]}`
	wantError(t, post(mux, key, three), http.StatusRequestEntityTooLarge, CodeTooLarge, "the request holds more than 2 files")
	two := `{"subject":"s","files":[{"path":"a","new":"x"},{"path":"b","new":"x"}]}`
	if rec := post(mux, key, two); rec.Code != http.StatusAccepted {
		t.Errorf("exactly max_files files: status %d, want 202 (%s)", rec.Code, rec.Body)
	}

	if len(fs.enqueued) != 2 {
		t.Errorf("%d enqueued, want 2 (the two that passed)", len(fs.enqueued))
	}
}

func TestCreateReviewCannotReview(t *testing.T) {
	h, fs, key, mux := newHandler(t)
	prefix, _ := ParseKey(key)
	setClient := func(f func(*store.APIClient)) {
		k := fs.keys[prefix]
		f(&k.client)
		fs.keys[prefix] = k
	}

	setClient(func(c *store.APIClient) { c.Skills = nil })
	wantError(t, post(mux, key, goodBody), http.StatusConflict, CodeCannotReview,
		"this client has no skills: an admin chooses them in its settings")

	setClient(func(c *store.APIClient) { c.Skills = []string{"security", "gone", "also-gone"} })
	wantError(t, post(mux, key, goodBody), http.StatusConflict, CodeCannotReview,
		`skill "gone" is not in the skill library`)

	setClient(func(c *store.APIClient) { c.Skills = []string{"security"}; c.ModelID = "removed" })
	wantError(t, post(mux, key, goodBody), http.StatusConflict, CodeCannotReview,
		"this client's model is no longer in the model catalog")

	// A client on the default model, with no default left.
	setClient(func(c *store.APIClient) { c.ModelID = "" })
	h.Models = fakeModels{}
	wantError(t, post(mux, key, goodBody), http.StatusConflict, CodeCannotReview,
		"this client's model is no longer in the model catalog")

	// A named model that exists.
	setClient(func(c *store.APIClient) { c.ModelID = "m1" })
	h.Models = fakeModels{byID: map[string]instances.Model{"m1": {ID: "m1"}}}
	if rec := post(mux, key, goodBody); rec.Code != http.StatusAccepted {
		t.Errorf("a client whose model exists: status %d, want 202 (%s)", rec.Code, rec.Body)
	}
	if len(fs.enqueued) != 1 {
		t.Errorf("%d enqueued, want 1: the refused requests make no review", len(fs.enqueued))
	}
}

func TestCreateReviewIdempotent(t *testing.T) {
	h, fs, key, mux := newHandler(t)
	woke := 0
	h.Wake = func() { woke++ }
	prefix, _ := ParseKey(key)

	first := post(mux, key, goodBody, "Idempotency-Key", "retry-1")
	again := post(mux, key, goodBody, "Idempotency-Key", "retry-1")
	if first.Code != http.StatusAccepted || again.Code != http.StatusAccepted {
		t.Fatalf("statuses %d and %d, want 202 and 202", first.Code, again.Code)
	}
	var a, b Review
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &b)
	if a.ID == "" || a.ID != b.ID {
		t.Errorf("ids %q and %q, want one", a.ID, b.ID)
	}
	if loc := again.Header().Get("Location"); loc != "/v1/reviews/"+a.ID {
		t.Errorf("the retry's Location is %q", loc)
	}
	if len(fs.enqueued) != 1 || woke != 1 {
		t.Errorf("%d enqueued and %d wakes, want 1 and 1: a retry makes nothing", len(fs.enqueued), woke)
	}

	// A retry is answered from what was stored, before the body is read as a
	// request: it holds even when the client has no skills now.
	k := fs.keys[prefix]
	k.client.Skills = nil
	fs.keys[prefix] = k
	if rec := post(mux, key, goodBody, "Idempotency-Key", "retry-1"); rec.Code != http.StatusAccepted {
		t.Errorf("a retry after the skills went: status %d, want 202 (%s)", rec.Code, rec.Body)
	}

	// The same key for another request, even one that is not valid JSON.
	for _, other := range []string{strings.Replace(goodBody, "route:1", "route:2", 1), `{`} {
		wantError(t, post(mux, key, other, "Idempotency-Key", "retry-1"), http.StatusUnprocessableEntity,
			CodeIdempotencyKeyReused, "the Idempotency-Key was used before for another request")
	}
	if len(fs.enqueued) != 1 {
		t.Errorf("%d enqueued, want 1", len(fs.enqueued))
	}

	// A retry shows the review as it is now.
	r := fs.reviews[a.ID]
	r.Run.State, r.Run.JobName = store.Running, "job-1"
	fs.reviews[a.ID] = r
	rec := post(mux, key, goodBody, "Idempotency-Key", "retry-1")
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if rec.Code != http.StatusAccepted || b.Status != StatusRunning {
		t.Errorf("a retry of a running review: status %d, review status %q; want 202, running", rec.Code, b.Status)
	}
}

func TestGetReview(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	queued := post(mux, key, goodBody)
	var q Review
	_ = json.Unmarshal(queued.Body.Bytes(), &q)

	get := func(id string) *httptest.ResponseRecorder { return call(mux, "GET", "/v1/reviews/"+id, "Bearer "+key) }

	rec := get(q.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body)
	}
	want, _ := json.Marshal(Pending(fs.reviews[q.ID]))
	if got := strings.TrimSpace(rec.Body.String()); got != string(want) {
		t.Errorf("body %s, want %s", got, want)
	}
	if !strings.Contains(rec.Body.String(), `"status":"queued"`) {
		t.Errorf("body %s is not queued", rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", rec.Header())
	}

	// Once published, the stored response is sent as it is stored.
	stored := `{"id":"` + q.ID + `",  "status":"completed","verdict":"no_blocking_issues","unknown_later_field":[1, 2]}`
	r := fs.reviews[q.ID]
	r.Run.State, r.Response = store.Published, []byte(stored)
	fs.reviews[q.ID] = r
	rec = get(q.ID)
	if rec.Code != http.StatusOK || rec.Body.String() != stored {
		t.Errorf("published: status %d, body %q; want 200 and %q", rec.Code, rec.Body, stored)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type %q", rec.Header().Get("Content-Type"))
	}

	// Without a key it is the 401.
	if rec := call(mux, "GET", "/v1/reviews/"+q.ID, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: status %d, want 401", rec.Code)
	}
}

func TestOtherClientsReviewIsNotFound(t *testing.T) {
	_, fs, key, mux := newHandler(t)
	// Client 3 owns the key. Client 4 has a review.
	const theirs = "00000000-0000-4000-8000-0000000000aa"
	fs.reviews = map[string]store.APIReview{theirs: {
		Run: store.Run{ID: theirs, State: store.Queued}, ClientID: 4, Subject: "secret", CreatedAt: testNow,
	}}

	unknown := call(mux, "GET", "/v1/reviews/00000000-0000-4000-8000-0000000000bb", "Bearer "+key)
	if unknown.Code != http.StatusNotFound || unknown.Body.String() != notFoundBody {
		t.Fatalf("an unknown id: status %d, body %q", unknown.Code, unknown.Body)
	}
	for name, id := range map[string]string{
		"another client's": theirs,
		"malformed":        "not-a-uuid",
		"almost a uuid":    "00000000-0000-4000-8000-0000000000aZ",
		"empty-ish":        "%20",
		"sql":              "x'%20or%20'1'='1",
	} {
		rec := call(mux, "GET", "/v1/reviews/"+id, "Bearer "+key)
		if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
			t.Errorf("%s id: status %d, body %q; want exactly the unknown id's %d, %q", name, rec.Code, rec.Body, unknown.Code, unknown.Body)
		}
		for k := range unknown.Header() {
			if rec.Header().Get(k) != unknown.Header().Get(k) {
				t.Errorf("%s id: header %s is %q, the unknown id's is %q", name, k, rec.Header().Get(k), unknown.Header().Get(k))
			}
		}
	}
	// A retry key of another client's review is not seen either.
	fs.keyed = map[string]string{"4/retry": theirs}
	fs.hashes = map[string][]byte{"4/retry": []byte("h")}
	if rec := post(mux, key, goodBody, "Idempotency-Key", "retry"); rec.Code != http.StatusAccepted {
		t.Errorf("a key another client used: status %d, want 202", rec.Code)
	}
	if len(fs.enqueued) != 1 {
		t.Errorf("%d enqueued, want 1: the other client's key is not this client's", len(fs.enqueued))
	}
}
