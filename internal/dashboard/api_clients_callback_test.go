package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// hookRequest is what a callback receiver got.
type hookRequest struct {
	method, uri string
	header      http.Header
	body        []byte
}

// hook is a callback receiver that answers with status and keeps what it got.
type hook struct {
	srv    *httptest.Server
	status atomic.Int32
	mu     sync.Mutex
	got    []hookRequest
}

func newHook(t *testing.T, status int) *hook {
	t.Helper()
	h := &hook{}
	h.status.Store(int32(status))
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.got = append(h.got, hookRequest{r.Method, r.URL.RequestURI(), r.Header.Clone(), b})
		h.mu.Unlock()
		w.WriteHeader(int(h.status.Load()))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hook) requests() []hookRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookRequest(nil), h.got...)
}

// verifies reports whether header's v1 values are the HMACs of "<t>.<body>"
// with secrets, in order, and returns t.
func verifies(t *testing.T, header string, body []byte, secrets ...string) int64 {
	t.Helper()
	parts := strings.Split(header, ",")
	if len(parts) != 1+len(secrets) || !strings.HasPrefix(parts[0], "t=") {
		t.Fatalf("signature %q: want t= and %d v1 values", header, len(secrets))
	}
	at, err := strconv.ParseInt(strings.TrimPrefix(parts[0], "t="), 10, 64)
	if err != nil {
		t.Fatalf("signature %q: %v", header, err)
	}
	for i, s := range secrets {
		m := hmac.New(sha256.New, []byte(s))
		m.Write([]byte(strconv.FormatInt(at, 10) + "." + string(body)))
		if want := "v1=" + hex.EncodeToString(m.Sum(nil)); parts[1+i] != want {
			t.Errorf("signature %q: part %d = %q, want %q", header, i+1, parts[1+i], want)
		}
	}
	return at
}

var deliveryUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// The test callback reaches the URL as a real one does: the same method,
// headers and signature, with a new delivery ID each time. Its body says it is
// a test and that nothing was reviewed, so it is never taken for a pass.
func TestTestCallbackSigned(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	rc := newHook(t, http.StatusNoContent)
	created := e.addAPIClient(c, goodClient(func(m map[string]any) { m["callbackUrl"] = rc.srv.URL + "/hooks/pergent?token=sekret" }))
	url := apiClientsURL + "/fastgateway/test-callback"

	before := time.Now()
	got := decode[APICallbackTestView](t, e.send(c, "POST", url, ""), "POST test-callback", http.StatusOK)
	if got.StatusCode != http.StatusNoContent || got.Error != "" || got.DurationMs < 0 {
		t.Errorf("answer = %+v, want a 204 and no error", got)
	}
	reqs := rc.requests()
	if len(reqs) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPost || r.uri != "/hooks/pergent?token=sekret" {
		t.Errorf("got %s %s", r.method, r.uri)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !deliveryUUID.MatchString(r.header.Get("X-Pergent-Delivery")) {
		t.Errorf("X-Pergent-Delivery = %q, want a UUID", r.header.Get("X-Pergent-Delivery"))
	}
	at := verifies(t, r.header.Get("X-Pergent-Signature"), r.body, created.CallbackSecret)
	if at < before.Unix() || at > time.Now().Unix() {
		t.Errorf("signed at %d, want the current time", at)
	}
	var review reviewapi.Review
	if err := json.Unmarshal(r.body, &review); err != nil {
		t.Fatalf("body %s: %v", r.body, err)
	}
	if review.ID != "00000000-0000-0000-0000-000000000000" || review.Subject != "pergent.sh test callback" ||
		review.Status != "completed" || review.Verdict != "nothing_reviewed" || !review.Test ||
		review.Skills != nil || review.Failure != nil {
		t.Errorf("body = %s, want the test review with the verdict nothing_reviewed", r.body)
	}
	if want, err := reviewapi.Encode(review); err != nil || string(want) != string(r.body) {
		t.Errorf("body is not encoded as a review is:\n%s\n%s", r.body, want)
	}

	// A second one has a delivery ID of its own.
	decode[APICallbackTestView](t, e.send(c, "POST", url, ""), "POST test-callback again", http.StatusOK)
	reqs = rc.requests()
	if len(reqs) != 2 || reqs[0].header.Get("X-Pergent-Delivery") == reqs[1].header.Get("X-Pergent-Delivery") {
		t.Errorf("deliveries = %v, want two different IDs", reqs)
	}

	// It is not a review's callback: nothing is stored for it.
	var n int
	if err := e.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM api_callbacks) + (SELECT count(*) FROM api_callback_attempts)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("callback rows = %d, %v; want none", n, err)
	}
	if got := auditDetails(e, "api_callback_tested"); len(got) != 2 || got[0] != "fastgateway" {
		t.Errorf("api_callback_tested = %v", got)
	}
	for _, a := range e.auditEntries() {
		if strings.Contains(a.Detail, "pgs_") || strings.Contains(a.Detail, "sekret") {
			t.Errorf("audit %s holds a secret: %q", a.Action, a.Detail)
		}
	}
	if strings.Contains(e.logs.String(), created.CallbackSecret) || strings.Contains(e.logs.String(), "sekret") {
		t.Error("the secret or the URL's token reached the log")
	}
}

// During a rotation's overlap the test callback is signed with both secrets,
// as a real one is; after it, with the new one alone.
func TestTestCallbackSignsWithBothSecretsInOverlap(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	rc := newHook(t, http.StatusOK)
	created := e.addAPIClient(c, goodClient(func(m map[string]any) { m["callbackUrl"] = rc.srv.URL }))
	rotated := decode[APICallbackSecretView](t, e.send(c, "POST", apiClientsURL+"/fastgateway/secret", ""), "rotate", http.StatusOK)
	url := apiClientsURL + "/fastgateway/test-callback"

	decode[APICallbackTestView](t, e.send(c, "POST", url, ""), "POST test-callback", http.StatusOK)
	r := rc.requests()[0]
	verifies(t, r.header.Get("X-Pergent-Signature"), r.body, rotated.CallbackSecret, created.CallbackSecret)

	if _, _, err := e.st.UpdateAPIClient(ctx, "fastgateway", func(c *store.APIClient) {
		c.PreviousSecretUntil = e.clock.now().Add(-time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	decode[APICallbackTestView](t, e.send(c, "POST", url, ""), "POST test-callback after", http.StatusOK)
	r = rc.requests()[1]
	verifies(t, r.header.Get("X-Pergent-Signature"), r.body, rotated.CallbackSecret)
}

// A client with no URL has nothing to send to.
func TestTestCallbackNoURL(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	e.addAPIClient(c, goodClient(func(m map[string]any) { m["callbackUrl"] = "" }))

	rec := e.send(c, "POST", apiClientsURL+"/fastgateway/test-callback", "")
	if rec.Code != http.StatusConflict || errorOf(t, rec) != "this client has no callback URL" {
		t.Errorf("POST test-callback = %d %s, want 409 this client has no callback URL", rec.Code, rec.Body)
	}
	wantCode(t, e.send(c, "POST", apiClientsURL+"/nobody/test-callback", ""), "test callback of an unknown client", http.StatusNotFound)
}

// A URL that answers 5xx is shown as what it answered; one that answers with a
// redirect is a failure that is not followed; one that does not answer gives
// an error with no part of the URL's path or query and no secret in it.
func TestTestCallbackFailuresAreShown(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	rc := newHook(t, http.StatusInternalServerError)
	created := e.addAPIClient(c, goodClient(func(m map[string]any) { m["callbackUrl"] = rc.srv.URL + "/hooks/t0kenInPath?key=q-secret" }))
	url := apiClientsURL + "/fastgateway/test-callback"

	got := decode[APICallbackTestView](t, e.send(c, "POST", url, ""), "POST 500", http.StatusOK)
	if got.StatusCode != http.StatusInternalServerError || got.Error != "" {
		t.Errorf("answer = %+v, want a 500", got)
	}

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	e.put(c, "fastgateway", redirect.URL)
	got = decode[APICallbackTestView](t, e.send(c, "POST", url, ""), "POST redirect", http.StatusOK)
	if got.StatusCode != http.StatusFound || got.Error == "" || elsewhere.Load() != 0 {
		t.Errorf("answer = %+v, followed %d times; want a 302 and an error, not followed", got, elsewhere.Load())
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL + "/hooks/t0kenInPath?key=q-secret"
	dead.Close()
	e.put(c, "fastgateway", deadURL)
	rec := e.send(c, "POST", url, "")
	got = decode[APICallbackTestView](t, rec, "POST dead", http.StatusOK)
	if got.StatusCode != 0 || got.Error == "" {
		t.Errorf("answer = %+v, want no status and an error", got)
	}
	for _, leak := range []string{"t0kenInPath", "q-secret", created.CallbackSecret} {
		if strings.Contains(rec.Body.String(), leak) || strings.Contains(e.logs.String(), leak) {
			t.Errorf("%q reached the answer or the log: %s", leak, rec.Body)
		}
	}
}

// put sets the client's callback URL, keeping the rest.
func (e *env) put(c *http.Cookie, name, callbackURL string) {
	e.t.Helper()
	cl := decode[APIClientView](e.t, e.get(c, apiClientsURL+"/"+name), "GET client", http.StatusOK)
	wantCode(e.t, e.sendJSON(c, "PUT", apiClientsURL+"/"+name, map[string]any{"skills": cl.Skills, "modelId": cl.ModelID,
		"callbackUrl": callbackURL, "limits": goodLimits(nil)}), "PUT client", http.StatusOK)
}

// sameAttempt compares two attempts, the times by instant.
func sameAttempt(a, b APICallbackAttemptView) bool {
	return a.At.Equal(b.At) && a.StatusCode == b.StatusCode && a.Error == b.Error && a.DurationMs == b.DurationMs
}

// The review page lists the callback's state and every attempt, oldest first.
func TestAPIReviewDetailCallbackAttempts(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	e.addAPIClient(c, goodClient(nil))
	id := e.askAPIReview("fastgateway", "acme", "route:1")

	v := getJSON[APIReviewDetailView](e, c, "/api/admin/api-reviews/"+id)
	if v.Callback.State != "none" || v.Callback.Attempts == nil || len(v.Callback.Attempts) != 0 {
		t.Errorf("callback = %+v, want none and an empty list", v.Callback)
	}
	if body := e.get(c, "/api/admin/api-reviews/"+id).Body.String(); !strings.Contains(body, `"attempts":[]`) {
		t.Errorf("body %s: want \"attempts\":[], not null", body)
	}

	if err := e.st.EnqueueAPICallback(ctx, id); err != nil {
		t.Fatal(err)
	}
	due, err := e.st.DueAPICallbacks(ctx, time.Now().Add(time.Hour), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("DueAPICallbacks = %v, %v", due, err)
	}
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	if err := e.st.RecordAPICallbackAttempt(ctx, due[0].ID,
		store.CallbackAttempt{At: at, Error: "dial tcp: connection refused", Duration: 120 * time.Millisecond},
		false, at.Add(10*time.Second), false); err != nil {
		t.Fatal(err)
	}
	v = getJSON[APIReviewDetailView](e, c, "/api/admin/api-reviews/"+id)
	if v.Callback.State != "pending" || len(v.Callback.Attempts) != 1 || !sameAttempt(v.Callback.Attempts[0],
		APICallbackAttemptView{At: at, Error: "dial tcp: connection refused", DurationMs: 120}) {
		t.Errorf("callback = %+v, want pending with the failed attempt", v.Callback)
	}

	later := at.Add(time.Minute)
	if err := e.st.RecordAPICallbackAttempt(ctx, due[0].ID, store.CallbackAttempt{At: later, StatusCode: 200, Duration: 30 * time.Millisecond},
		true, later, false); err != nil {
		t.Fatal(err)
	}
	v = getJSON[APIReviewDetailView](e, c, "/api/admin/api-reviews/"+id)
	if v.Callback.State != "delivered" || len(v.Callback.Attempts) != 2 || !sameAttempt(v.Callback.Attempts[0],
		APICallbackAttemptView{At: at, Error: "dial tcp: connection refused", DurationMs: 120}) || !sameAttempt(v.Callback.Attempts[1],
		APICallbackAttemptView{At: later, StatusCode: 200, DurationMs: 30}) {
		t.Errorf("callback = %+v, want delivered with two attempts, oldest first", v.Callback)
	}
}
