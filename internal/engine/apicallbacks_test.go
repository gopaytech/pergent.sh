package engine

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// testCallbackSecret is the callback secret newAPIE2E seals for its client.
const testCallbackSecret = "pgs_callback-secret-example"

var deliveryID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// callbackRequest is one POST a receiver got.
type callbackRequest struct {
	method string
	header http.Header
	body   []byte
}

// receiver is a client's callback URL. answer writes the response to the
// n-th request (from 0); nil answers 200.
type receiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	got    []callbackRequest
	answer func(n int, w http.ResponseWriter, r *http.Request)
}

func newReceiver(t *testing.T, answer func(n int, w http.ResponseWriter, r *http.Request)) *receiver {
	t.Helper()
	rc := &receiver{answer: answer}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		n := len(rc.got)
		rc.got = append(rc.got, callbackRequest{method: r.Method, header: r.Header.Clone(), body: body})
		rc.mu.Unlock()
		if rc.answer == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		rc.answer(n, w, r)
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) requests() []callbackRequest {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]callbackRequest(nil), rc.got...)
}

// answerWith answers every request with code.
func answerWith(code int) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// newCallbackE2E is an apiE2E whose client's callback URL is url.
func newCallbackE2E(t *testing.T, url string) *apiE2E {
	t.Helper()
	a := newAPIE2E(t)
	a.setCallbackURL(t, url)
	return a
}

func (a *apiE2E) setCallbackURL(t *testing.T, url string) {
	t.Helper()
	if _, _, err := a.env.Store.UpdateAPIClient(a.env.ctx, "gateway", func(c *store.APIClient) { c.CallbackURL = url }); err != nil {
		t.Fatalf("UpdateAPIClient: %v", err)
	}
}

// supersede asks for two reviews of one subject and returns the first,
// superseded by the second, which has a callback due.
func (a *apiE2E) supersede(t *testing.T) string {
	t.Helper()
	first := a.ask(t, "route:1", twoFiles())
	a.ask(t, "route:1", twoFiles())
	return first
}

// rawReview is the body of GET /v1/reviews/{id}, as sent.
func (a *apiE2E) rawReview(t *testing.T, id string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, a.api.URL+"/v1/reviews/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+a.key)
	resp, err := a.api.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/reviews/%s = %d, %v", id, resp.StatusCode, err)
	}
	return body
}

// catchUp moves the engine's clock just past the database's, which stamps a
// callback's created_at and first try.
func catchUp(env *testEnv) {
	env.SetNow(time.Now().UTC().Add(2 * time.Second))
}

// advance moves the engine's clock by d.
func advance(env *testEnv, d time.Duration) {
	env.SetNow(env.Now().Add(d))
}

func deliver(t *testing.T, env *testEnv) {
	t.Helper()
	if err := env.Engine.DeliverCallbacksOnce(env.ctx); err != nil {
		t.Fatalf("DeliverCallbacksOnce: %v", err)
	}
}

// callbackOf is the state and attempts of run id's callback.
func callbackOf(t *testing.T, env *testEnv, id string) (string, []store.CallbackAttempt) {
	t.Helper()
	state, attempts, err := env.Store.APICallbackAttempts(env.ctx, id)
	if err != nil {
		t.Fatalf("APICallbackAttempts(%s): %v", id, err)
	}
	return state, attempts
}

// signature splits an X-Pergent-Signature into its time and v1 values.
func signature(t *testing.T, header string) (int64, []string) {
	t.Helper()
	var (
		at  int64
		v1s []string
	)
	for i, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(part, "=")
		switch {
		case !ok:
			t.Fatalf("signature %q: part %q has no =", header, part)
		case i == 0 && k == "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("signature %q: t: %v", header, err)
			}
			at = n
		case i > 0 && k == "v1":
			v1s = append(v1s, v)
		default:
			t.Fatalf("signature %q: unexpected part %q", header, part)
		}
	}
	return at, v1s
}

// signs reports whether v1 is the HMAC of "<t>.<body>" with secret, as a
// caller checks it.
func signs(secret string, at int64, body []byte, v1 string) bool {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(at, 10) + "." + string(body)))
	want := hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(want), []byte(v1))
}

// A published review is posted once, signed, with the body GET returns.
func TestCallbackDelivered(t *testing.T) {
	rc := newReceiver(t, nil)
	a := newCallbackE2E(t, rc.srv.URL+"/hook")
	env := a.env
	im, err := reviewapi.BuildImport(twoFiles())
	if err != nil {
		t.Fatal(err)
	}
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	token := string(env.Secret(id).Data["result-token"])
	if code := a.postResult(t, id, token, apiResultFor(t, im.Base, im.Head, blockingSkill(t))); code != http.StatusOK {
		t.Fatalf("result: status %d", code)
	}
	publish(t, env)
	wantPublished(t, env, id)
	if got := rc.requests(); len(got) != 0 {
		t.Fatalf("posted before the callback loop ran: %d requests", len(got))
	}

	catchUp(env)
	deliver(t, env)

	got := rc.requests()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(got))
	}
	req := got[0]
	if want := a.rawReview(t, id); req.method != http.MethodPost || string(req.body) != string(want) {
		t.Errorf("%s with body\n%s\nwant POST with the GET body\n%s", req.method, req.body, want)
	}
	if ct := req.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if d := req.header.Get("X-Pergent-Delivery"); !deliveryID.MatchString(d) {
		t.Errorf("X-Pergent-Delivery = %q, want a UUID", d)
	}
	at, v1s := signature(t, req.header.Get("X-Pergent-Signature"))
	if at != env.Now().Unix() || len(v1s) != 1 || !signs(testCallbackSecret, at, req.body, v1s[0]) {
		t.Errorf("signature t=%d %v doesn't verify with the client's secret at %d", at, v1s, env.Now().Unix())
	}
	state, attempts := callbackOf(t, env, id)
	if state != "delivered" || len(attempts) != 1 || attempts[0].StatusCode != http.StatusOK || attempts[0].Error != "" {
		t.Errorf("callback = %s, %+v; want delivered on one 200", state, attempts)
	}

	advance(env, time.Hour)
	deliver(t, env)
	if n := len(rc.requests()); n != 1 {
		t.Errorf("a delivered callback was sent again: %d requests", n)
	}
	if logs := env.Logs(); strings.Contains(logs, testCallbackSecret) {
		t.Errorf("the callback secret reached the log:\n%s", logs)
	}
}

// A failed callback is tried again after the backoff, under the same
// delivery ID.
func TestCallbackRetriesSameDelivery(t *testing.T) {
	rc := newReceiver(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	id := a.supersede(t)
	catchUp(env)

	deliver(t, env)
	if state, attempts := callbackOf(t, env, id); state != "pending" || len(attempts) != 1 || attempts[0].StatusCode != 500 {
		t.Fatalf("after a 500: %s, %+v; want pending, one attempt with 500", state, attempts)
	}
	// Not before the backoff.
	advance(env, 9*time.Second)
	deliver(t, env)
	if n := len(rc.requests()); n != 1 {
		t.Fatalf("tried again within 10 s: %d requests", n)
	}
	advance(env, time.Second)
	deliver(t, env)

	got := rc.requests()
	if len(got) != 2 {
		t.Fatalf("receiver got %d requests, want 2", len(got))
	}
	if a, b := got[0].header.Get("X-Pergent-Delivery"), got[1].header.Get("X-Pergent-Delivery"); a == "" || a != b {
		t.Errorf("delivery IDs %q and %q, want one", a, b)
	}
	if string(got[0].body) != string(got[1].body) {
		t.Errorf("the retry's body changed:\n%s\n%s", got[0].body, got[1].body)
	}
	state, attempts := callbackOf(t, env, id)
	if state != "delivered" || len(attempts) != 2 {
		t.Fatalf("callback = %s, %d attempts; want delivered after 2", state, len(attempts))
	}
	if gap := attempts[1].At.Sub(attempts[0].At); gap < 10*time.Second {
		t.Errorf("second attempt %v after the first, want at least 10 s", gap)
	}
	t0, _ := signature(t, got[0].header.Get("X-Pergent-Signature"))
	t1, _ := signature(t, got[1].header.Get("X-Pergent-Signature"))
	if t1-t0 < 10 {
		t.Errorf("signature times %d and %d: the retry is signed anew", t0, t1)
	}
}

// A redirect is a failed attempt: it is never followed, whatever the client.
func TestCallbackRedirectIsFailure(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(other.Close)
	rc := newReceiver(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusMovedPermanently)
	})
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	id := a.supersede(t)
	catchUp(env)

	deliver(t, env)
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the redirect was followed: %d requests elsewhere", n)
	}
	state, attempts := callbackOf(t, env, id)
	if state != "pending" || len(attempts) != 1 || attempts[0].StatusCode != http.StatusMovedPermanently {
		t.Fatalf("callback = %s, %+v; want pending with a 301", state, attempts)
	}

	// A client that follows redirects is made not to.
	env.Engine.CallbackClient = &http.Client{Timeout: 5 * time.Second}
	advance(env, 10*time.Second)
	deliver(t, env)
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the redirect was followed with the engine's own client: %d requests elsewhere", n)
	}
	if _, attempts := callbackOf(t, env, id); len(attempts) != 2 || attempts[1].StatusCode != http.StatusMovedPermanently {
		t.Errorf("attempts = %+v; want a second 301", attempts)
	}
}

// No answer within the client's timeout is a failed attempt.
func TestCallbackTimeout(t *testing.T) {
	rc := newReceiver(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	env.Engine.CallbackClient = &http.Client{Timeout: 200 * time.Millisecond}
	id := a.supersede(t)
	catchUp(env)

	deliver(t, env)

	state, attempts := callbackOf(t, env, id)
	if state != "pending" || len(attempts) != 1 {
		t.Fatalf("callback = %s, %+v; want pending, one attempt", state, attempts)
	}
	if at := attempts[0]; at.StatusCode != 0 || !strings.Contains(strings.ToLower(at.Error), "timeout") || at.Duration <= 0 {
		t.Errorf("attempt = %+v; want no status and a timeout error", at)
	}
}

// Once 24 hours passed since the review ended, a failed attempt gives up.
func TestCallbackGivesUpAfter24h(t *testing.T) {
	rc := newReceiver(t, answerWith(http.StatusServiceUnavailable))
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	id := a.supersede(t)
	catchUp(env)

	deliver(t, env)
	if state, _ := callbackOf(t, env, id); state != "pending" {
		t.Fatalf("after the first failure: %s, want pending", state)
	}
	advance(env, 23*time.Hour)
	deliver(t, env)
	if state, _ := callbackOf(t, env, id); state != "pending" {
		t.Fatalf("after 23 hours: %s, want pending", state)
	}
	advance(env, time.Hour+time.Minute)
	deliver(t, env)
	state, attempts := callbackOf(t, env, id)
	if state != "gave_up" || len(attempts) != 3 {
		t.Fatalf("after 24 hours: %s, %d attempts; want gave_up after 3", state, len(attempts))
	}
	advance(env, time.Hour)
	deliver(t, env)
	if n := len(rc.requests()); n != 3 {
		t.Errorf("sent %d times, want 3: one that gave up is not tried again", n)
	}
}

// During a rotation's overlap a callback carries both signatures, the current
// one first; after it, only the current one.
func TestCallbackRotationSendsBoth(t *testing.T) {
	rc := newReceiver(t, nil)
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	const newSecret = "pgs_new-secret-example"
	until := time.Now().UTC().Add(24 * time.Hour)
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) {
		c.PreviousSecretSealed, c.PreviousSecretUntil = c.CallbackSecretSealed, until
		c.CallbackSecretSealed = env.Box.Seal([]byte(newSecret))
	}); err != nil {
		t.Fatal(err)
	}
	a.supersede(t)
	catchUp(env)

	deliver(t, env)
	got := rc.requests()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(got))
	}
	at, v1s := signature(t, got[0].header.Get("X-Pergent-Signature"))
	if len(v1s) != 2 || !signs(newSecret, at, got[0].body, v1s[0]) || !signs(testCallbackSecret, at, got[0].body, v1s[1]) {
		t.Errorf("signature %q: want the new secret's v1 then the old one's", got[0].header.Get("X-Pergent-Signature"))
	}

	// The second review is superseded in turn, after the overlap.
	a.ask(t, "route:1", twoFiles())
	env.SetNow(until.Add(time.Second))
	deliver(t, env)
	got = rc.requests()
	if len(got) != 2 {
		t.Fatalf("receiver got %d requests, want 2", len(got))
	}
	at, v1s = signature(t, got[1].header.Get("X-Pergent-Signature"))
	if len(v1s) != 1 || !signs(newSecret, at, got[1].body, v1s[0]) {
		t.Errorf("signature after the overlap %q: want the new secret's v1 alone", got[1].header.Get("X-Pergent-Signature"))
	}
	if logs := env.Logs(); strings.Contains(logs, newSecret) || strings.Contains(logs, testCallbackSecret) {
		t.Errorf("a callback secret reached the log:\n%s", logs)
	}
}

// A superseded review's callback says so, with the body GET returns.
func TestSupersededReviewGetsCallback(t *testing.T) {
	rc := newReceiver(t, nil)
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	id := a.supersede(t)
	catchUp(env)

	deliver(t, env)

	got := rc.requests()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1: the newer, queued review has none", len(got))
	}
	if want := a.rawReview(t, id); string(got[0].body) != string(want) {
		t.Errorf("body\n%s\nwant the GET body\n%s", got[0].body, want)
	}
	if !strings.Contains(string(got[0].body), `"status":"superseded"`) || !strings.Contains(string(got[0].body), `"id":"`+id+`"`) {
		t.Errorf("body %s; want review %s, superseded", got[0].body, id)
	}
}

// A review whose publish gave up gets its callback, which reads failed.
func TestGaveUpAPIReviewGetsCallback(t *testing.T) {
	rc := newReceiver(t, nil)
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	im, err := reviewapi.BuildImport(twoFiles())
	if err != nil {
		t.Fatal(err)
	}
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	token := string(env.Secret(id).Data["result-token"])
	if code := a.postResult(t, id, token, apiResultFor(t, im.Base, im.Head, blockingSkill(t))); code != http.StatusOK {
		t.Fatalf("result: status %d", code)
	}
	for range 9 {
		if _, err := env.Store.PublishRetry(env.ctx, id, "the store is down", env.Now()); err != nil {
			t.Fatal(err)
		}
	}
	env.Engine.retryPublish(env.ctx, env.Run(id), "the store is down", env.Engine.log())
	if r := env.Run(id); r.State != store.PublishFailed {
		t.Fatalf("run = %s, want publish_failed", r.State)
	}

	catchUp(env)
	deliver(t, env)

	got := rc.requests()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(got))
	}
	if want := a.rawReview(t, id); string(got[0].body) != string(want) || !strings.Contains(string(want), `"status":"failed"`) {
		t.Errorf("body\n%s\nwant the failed GET body\n%s", got[0].body, want)
	}
}

// A removed client's callbacks are gone with it: nothing is sent, and nothing
// is logged as an error, also for one listed just before the removal.
func TestRemovedClientNoCallback(t *testing.T) {
	rc := newReceiver(t, nil)
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	a.supersede(t)
	catchUp(env)
	due, err := env.Store.DueAPICallbacks(env.ctx, env.Now(), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("DueAPICallbacks = %d, %v; want 1", len(due), err)
	}
	if _, found, err := env.Store.DeleteAPIClient(env.ctx, "gateway"); err != nil || !found {
		t.Fatalf("DeleteAPIClient: found=%v err=%v", found, err)
	}
	before := len(env.Logs())

	deliver(t, env)
	tried, err := env.Engine.sendCallback(env.ctx, env.Engine.callbackClient(), due[0])
	if err != nil || tried {
		t.Errorf("sendCallback of a removed client's callback = %v, %v; want nothing tried, no error", tried, err)
	}

	if n := len(rc.requests()); n != 0 {
		t.Errorf("receiver got %d requests, want none", n)
	}
	if logs := env.Logs()[before:]; strings.Contains(logs, "level=ERROR") || strings.Contains(logs, "level=WARN") {
		t.Errorf("a removed client's callback logged a problem:\n%s", logs)
	}
}

// A client whose callback URL was emptied gives up at once, with a fixed
// error, and nothing is sent.
func TestCallbackNoURLGivesUp(t *testing.T) {
	rc := newReceiver(t, nil)
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	id := a.supersede(t)
	a.setCallbackURL(t, "")
	catchUp(env)

	deliver(t, env)

	state, attempts := callbackOf(t, env, id)
	if state != "gave_up" || len(attempts) != 1 || attempts[0].Error != "the client has no callback URL" {
		t.Errorf("callback = %s, %+v; want gave_up with the fixed error", state, attempts)
	}
	if n := len(rc.requests()); n != 0 {
		t.Errorf("receiver got %d requests, want none", n)
	}
}

// Three undelivered callbacks in 24 hours raise callback_failing, for admins,
// and a delivery that brings them to two clears it.
func TestCallbackFailingAttention(t *testing.T) {
	var succeed atomic.Int32
	rc := newReceiver(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		if succeed.Add(-1) >= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	failing := func() []store.Attention {
		t.Helper()
		att, err := env.Store.Attention(env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []store.Attention
		for _, x := range att {
			if x.Kind == attCallbackFailing {
				out = append(out, x)
			}
		}
		return out
	}
	// Two superseded reviews fail: under the threshold.
	for range 3 {
		a.ask(t, "route:1", twoFiles())
	}
	catchUp(env)
	deliver(t, env)
	if got := failing(); len(got) != 0 {
		t.Fatalf("after two failed callbacks: %+v, want no item", got)
	}

	// A third.
	a.ask(t, "route:1", twoFiles())
	advance(env, 10*time.Second)
	deliver(t, env)
	got := failing()
	const msg = "Callbacks to this client failed for 3 reviews in the last 24 hours. Check its callback URL; the results stay readable with GET /v1/reviews/{id}."
	if len(got) != 1 || got[0].Instance != "" || got[0].Project != "gateway" || got[0].Message != msg {
		t.Fatalf("after three failed callbacks: %+v; want the client's item with %q", got, msg)
	}

	// One delivers: two are left undelivered.
	succeed.Store(1)
	advance(env, 30*time.Second)
	deliver(t, env)
	due, err := env.Store.DueAPICallbacks(env.ctx, env.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("%d callbacks left undelivered, want 2", len(due))
	}
	if got := failing(); len(got) != 0 {
		t.Errorf("after a delivery left two: %+v, want the item cleared", got)
	}
}

// Emptying the URL is not a delivery failure: the callbacks it gives up on
// stop counting, and the item goes in that pass.
func TestCallbackFailingClearedWhenURLEmptied(t *testing.T) {
	rc := newReceiver(t, answerWith(http.StatusBadGateway))
	a := newCallbackE2E(t, rc.srv.URL)
	env := a.env
	for range 4 {
		a.ask(t, "route:1", twoFiles())
	}
	catchUp(env)
	deliver(t, env)
	att, err := env.Store.Attention(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(att) != 1 || att[0].Kind != attCallbackFailing {
		t.Fatalf("after three failed callbacks: %+v, want the client's item", att)
	}

	a.setCallbackURL(t, "")
	advance(env, 10*time.Second)
	deliver(t, env)

	due, err := env.Store.DueAPICallbacks(env.ctx, env.Now().Add(48*time.Hour), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("due after emptying the URL = %d, %v; want all given up", len(due), err)
	}
	if att, err := env.Store.Attention(env.ctx); err != nil || len(att) != 0 {
		t.Errorf("attention after emptying the URL = %+v, %v; want none", att, err)
	}
	if n := len(rc.requests()); n != 3 {
		t.Errorf("receiver got %d requests, want the 3 first attempts only", n)
	}
}

// A failed attempt's stored error has the URL's path and query and the
// secrets replaced, and is short.
func TestCallbackErrorRedacted(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL + "/hook?token=abc"
	closed.Close()
	a := newCallbackE2E(t, url)
	env := a.env
	id := a.supersede(t)
	catchUp(env)

	deliver(t, env)

	state, attempts := callbackOf(t, env, id)
	if state != "pending" || len(attempts) != 1 {
		t.Fatalf("callback = %s, %+v; want pending, one attempt", state, attempts)
	}
	e := attempts[0].Error
	if e == "" || strings.Contains(e, "abc") || strings.Contains(e, "/hook") || !strings.Contains(e, "/***?***") || len(e) > 500 {
		t.Errorf("stored error %q; want the path and query replaced by ***, at most 500 bytes", e)
	}
	if logs := env.Logs(); strings.Contains(logs, "token=abc") || strings.Contains(logs, "/hook") {
		t.Errorf("the URL's path or query reached the log:\n%s", logs)
	}
}

func TestCallbackErrorText(t *testing.T) {
	long := strings.Repeat("é", 400)
	for _, tc := range []struct{ name, text, url, want string }{
		{"query", `Post "https://h.example/x?token=abc&b=2": refused`, "https://h.example/x?token=abc&b=2",
			`Post "https://h.example/***?***": refused`},
		{"query elsewhere", `bad query token=abc&b=2`, "https://h.example/?token=abc&b=2", `bad query ***`},
		{"fragment", `Post "https://h.example/x#frag": refused`, "https://h.example/x#frag",
			`Post "https://h.example/***#***": refused`},
		// A webhook URL often holds its token in the path.
		{"path", `Post "https://hooks.example.com/services/T0/B0/XYZ": EOF`, "https://hooks.example.com/services/T0/B0/XYZ",
			`Post "https://hooks.example.com/***": EOF`},
		{"root", `Post "http://10.0.0.5:8080/": refused`, "http://10.0.0.5:8080/", `Post "http://10.0.0.5:8080/": refused`},
		{"secret", "signed with pgs_s3cret", "https://h.example/x", "signed with ***"},
		{"plain", "refused", "https://h.example/x", "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := callbackError(tc.text, tc.url, []string{"pgs_s3cret", ""}); got != tc.want {
				t.Errorf("callbackError = %q, want %q", got, tc.want)
			}
		})
	}
	got := callbackError(long, "https://h.example/x", nil)
	if len(got) > 500 || !strings.HasPrefix(long, got) || len(got) < 498 {
		t.Errorf("cut to %d bytes (valid prefix %v), want 499 or 500 on a rune boundary", len(got), strings.HasPrefix(long, got))
	}
}

func TestCallbackBackoff(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{{0, 10 * time.Second}, {1, 20 * time.Second}, {2, 40 * time.Second}, {8, 2560 * time.Second}, {9, time.Hour}, {40, time.Hour}, {1000, time.Hour}} {
		if got := callbackBackoff(tc.attempts); got != tc.want {
			t.Errorf("callbackBackoff(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}
