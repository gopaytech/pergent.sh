package reviewapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// The expected values were computed with openssl:
// printf '%s.%s' 1700000000 '{}' | openssl dgst -sha256 -hmac pgs_x
func TestSign(t *testing.T) {
	at := time.Unix(1700000000, 0)
	const (
		cur = "209391bb8c46dd6f45407256a2fa898b73eb7f9ad511a1b206a745eb936f3c6e"
		old = "8b994264ee669aa73db70b947bd81993f682df303695c7c38107a40f2743e426"
	)
	if got, want := Sign([]byte("{}"), at, "pgs_x"), "t=1700000000,v1="+cur; got != want {
		t.Errorf("Sign = %q, want %q", got, want)
	}
	// During a rotation: one v1= per secret, the current one first.
	if got, want := Sign([]byte("{}"), at, "pgs_x", "pgs_old"), "t=1700000000,v1="+cur+",v1="+old; got != want {
		t.Errorf("Sign with two secrets = %q, want %q", got, want)
	}
	// The time is in seconds, whatever the zone or the fraction.
	if got, want := Sign([]byte("{}"), at.Add(900*time.Millisecond).In(time.FixedZone("x", 3600)), "pgs_x"), "t=1700000000,v1="+cur; got != want {
		t.Errorf("Sign of a time with a fraction = %q, want %q", got, want)
	}
}

// Body is what GET writes, byte for byte, so a callback and GET never differ:
// the stored response as stored, or the pending body.
func TestBodyIsWhatGetWrites(t *testing.T) {
	for name, rv := range map[string]store.APIReview{
		"superseded": apiReview(store.Superseded, "", ""),
		"gave up":    apiReview(store.PublishFailed, "job-1", `{"status":"completed"}`),
		"published":  {Run: store.Run{ID: "r1", State: store.Published}, Response: []byte(`{"id": "r1", "status": "completed"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := Body(rv)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			writeReview(rec, http.StatusOK, rv)
			if rec.Body.String() != string(body) {
				t.Errorf("Body = %q, GET writes %q", body, rec.Body.String())
			}
			if rv.Response != nil && string(body) != string(rv.Response) {
				t.Errorf("Body = %q, want the stored response %q", body, rv.Response)
			}
		})
	}
	if b, _ := Body(apiReview(store.Superseded, "", "")); !strings.Contains(string(b), `"status":"superseded"`) {
		t.Errorf("superseded body = %s", b)
	}
}
