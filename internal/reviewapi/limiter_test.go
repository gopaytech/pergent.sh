package reviewapi

import (
	"net/http"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	var l Limiter
	at := testNow
	for i := range 3 {
		if ok, _ := l.Allow(7, 3, at); !ok {
			t.Fatalf("request %d of 3 refused", i+1)
		}
	}
	ok, wait := l.Allow(7, 3, at)
	if ok {
		t.Fatal("the 4th request in the hour was allowed")
	}
	if wait < 19*time.Minute || wait > 21*time.Minute {
		t.Errorf("retry after %v, want about 20 minutes", wait)
	}
	// A refusal spends nothing.
	if ok, again := l.Allow(7, 3, at); ok || again != wait {
		t.Errorf("a second refusal = %v, %v; want refused after %v", ok, again, wait)
	}

	// Keys are apart: another key has its own hour.
	if ok, _ := l.Allow(8, 3, at); !ok {
		t.Error("another key was refused")
	}

	at = at.Add(20 * time.Minute)
	if ok, _ := l.Allow(7, 3, at); !ok {
		t.Fatal("20 minutes later the next request was refused")
	}
	if ok, _ := l.Allow(7, 3, at); ok {
		t.Error("20 minutes refill one request, not two")
	}

	// An hour of quiet fills the bucket and no more.
	at = at.Add(5 * time.Hour)
	for i := range 3 {
		if ok, _ := l.Allow(7, 3, at); !ok {
			t.Fatalf("after a quiet hour, request %d of 3 refused", i+1)
		}
	}
	if ok, _ := l.Allow(7, 3, at); ok {
		t.Error("a quiet hour filled more than the hour's requests")
	}
}

// A limit an admin lowers takes effect on the next request.
func TestLimiterLimitLowered(t *testing.T) {
	var l Limiter
	if ok, _ := l.Allow(7, 600, testNow); !ok {
		t.Fatal("the first request was refused")
	}
	if ok, _ := l.Allow(7, 1, testNow); ok {
		t.Error("a key at 1 an hour made its second request at once")
	}
}

func TestRateLimit429(t *testing.T) {
	h, fs, key, mux := newHandler(t)
	prefix, _ := ParseKey(key)
	k := fs.keys[prefix]
	k.client.Limits.RequestsPerHour = 2
	fs.keys[prefix] = k
	h.Limiter = &Limiter{}

	// Every authenticated route counts, whatever it answers.
	if rec := call(mux, "GET", "/v1/client", "Bearer "+key); rec.Code != http.StatusOK {
		t.Fatalf("first call: status %d", rec.Code)
	}
	if rec := call(mux, "GET", "/v1/nope", "Bearer "+key); rec.Code != http.StatusNotFound {
		t.Fatalf("second call: status %d", rec.Code)
	}
	// A refused key never reaches the limit.
	if rec := call(mux, "GET", "/v1/client", "Bearer nonsense"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a bad key: status %d", rec.Code)
	}

	touched := len(fs.touched)
	rec := call(mux, "GET", "/v1/client", "Bearer "+key)
	wantError(t, rec, http.StatusTooManyRequests, CodeRateLimited, "too many requests for this key")
	// 2 an hour: the next one is due in 30 minutes.
	if got := rec.Header().Get("Retry-After"); got != "1800" {
		t.Errorf("Retry-After %q, want 1800", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	if len(fs.touched) != touched {
		t.Errorf("a refused call was recorded as the key's use")
	}
	if rec := post(mux, key, goodBody); rec.Code != http.StatusTooManyRequests || len(fs.enqueued) != 0 {
		t.Errorf("POST over the limit: status %d, %d enqueued; want 429 and none", rec.Code, len(fs.enqueued))
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                       "1",
		time.Millisecond:        "1",
		time.Second:             "1",
		time.Second + 1:         "2",
		20 * time.Minute:        "1200",
		20*time.Minute - 1:      "1200",
		-5 * time.Second:        "1",
		90*time.Second + 999999: "91",
	} {
		if got := retryAfter(d); got != want {
			t.Errorf("retryAfter(%v) = %q, want %q", d, got, want)
		}
	}
}
