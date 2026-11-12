package engine

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// SendCallback posts the body as given, signed with every secret at the
// current time, under the delivery ID, and reports the status.
func TestSendCallbackSigns(t *testing.T) {
	var (
		gotBody            []byte
		gotHeader          http.Header
		gotMethod, gotPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader, gotMethod, gotPath = r.Header.Clone(), r.Method, r.URL.RequestURI()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	body := []byte(`{"id":"x"}` + "\n")

	before := time.Now()
	a := SendCallback(t.Context(), nil, srv.URL+"/hook?token=t0k", body, "11111111-1111-4111-8111-111111111111", "pgs_new", "pgs_old")

	if a.StatusCode != http.StatusNoContent || a.Error != "" {
		t.Fatalf("attempt = %+v, want a 204 and no error", a)
	}
	if gotMethod != http.MethodPost || gotPath != "/hook?token=t0k" || string(gotBody) != string(body) {
		t.Errorf("got %s %s with %q", gotMethod, gotPath, gotBody)
	}
	if gotHeader.Get("Content-Type") != "application/json" || gotHeader.Get("X-Pergent-Delivery") != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("headers = %v", gotHeader)
	}
	at, v1s := signature(t, gotHeader.Get("X-Pergent-Signature"))
	if at < before.Unix() || at > time.Now().Unix() {
		t.Errorf("signed at %d, want the current time", at)
	}
	if len(v1s) != 2 || !signs("pgs_new", at, body, v1s[0]) || !signs("pgs_old", at, body, v1s[1]) {
		t.Errorf("signature %q: want the new secret's v1 then the old one's", gotHeader.Get("X-Pergent-Signature"))
	}
	if a.At.Unix() != at {
		t.Errorf("At = %v, want the signing time", a.At)
	}
}

// A redirect is the answer, never followed: with no client and with one that
// follows redirects.
func TestSendCallbackNeverFollowsRedirect(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	for name, hc := range map[string]*http.Client{"nil": nil, "default": {}, "own": {Timeout: time.Second}} {
		a := SendCallback(t.Context(), hc, srv.URL, []byte("{}"), "d", "s")
		if a.StatusCode != http.StatusFound || a.Error != msgCallbackRedirect {
			t.Errorf("%s: attempt = %+v, want a 302 and the redirect error", name, a)
		}
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("a redirect was followed %d times", n)
	}
}

// A transport error is text with the URL's path and query and the secrets
// taken out.
func TestSendCallbackErrorIsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/hooks/t0kenInPath?key=q-secret"
	srv.Close() // nothing listens now: the dial fails

	a := SendCallback(t.Context(), nil, url, []byte("{}"), "d", "pgs_s3cret")

	if a.StatusCode != 0 || a.Error == "" {
		t.Fatalf("attempt = %+v, want no status and an error", a)
	}
	for _, leak := range []string{"t0kenInPath", "q-secret", "pgs_s3cret"} {
		if strings.Contains(a.Error, leak) {
			t.Errorf("error %q holds %q", a.Error, leak)
		}
	}
}
