package callback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/security"
)

// fakeSources serves one running scan's archive with one token. A body that
// is not set is a platform that gives no archive.
type fakeSources struct {
	token    string
	finished bool
	busy     bool
	body     io.Reader
	closed   bool
	tooLarge []string
}

func (f *fakeSources) OpenSecuritySource(_ context.Context, _, token string) (io.ReadCloser, security.Outcome, error) {
	switch {
	case f.finished:
		return nil, security.Conflict, nil
	case f.busy:
		return nil, security.Busy, nil
	case token != f.token:
		return nil, security.Unauthorized, nil
	case f.body == nil:
		return nil, 0, fmt.Errorf("%w: %w", security.ErrNoSource, forge.ErrSourceNotFound)
	}
	return &closeRecorder{Reader: f.body, closed: &f.closed}, security.Stored, nil
}

func (f *fakeSources) SecuritySourceTooLarge(_ context.Context, _, token string) error {
	f.tooLarge = append(f.tooLarge, token)
	return nil
}

type closeRecorder struct {
	io.Reader
	closed *bool
}

func (c *closeRecorder) Close() error { *c.closed = true; return nil }

// deadlineRecorder is a ResponseRecorder that keeps the deadlines
// http.ResponseController sets on it.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	read, write time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error  { d.read = t; return nil }
func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error { d.write = t; return nil }

// getSource serves one source request; aborted is whether the handler cut
// the response off (http.ErrAbortHandler), as the server does for a stream
// that must not look complete.
func getSource(t *testing.T, h http.Handler, method, token string) (rec *deadlineRecorder, aborted bool) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/security-scans/{id}/source", h)
	req := httptest.NewRequest(method, "/security-scans/s1/source", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec = &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	defer func() {
		if p := recover(); p != nil {
			if p != http.ErrAbortHandler {
				panic(p)
			}
			aborted = true
		}
	}()
	mux.ServeHTTP(rec, req)
	return rec, false
}

// The source route: another method is 405, no or a wrong token 401, a scan
// that is not running 409, a download while one runs or past the scan's
// tries 409, a platform that gives no archive 502, and else
// the archive as it is, application/gzip, past the server's 60 s timeouts.
// The log holds neither the token nor a byte of the archive.
func TestSecuritySourceHandler(t *testing.T) {
	t.Parallel()
	const token = "scan-token-1234567890"
	const archive = "\x1f\x8bsecret-archive-bytes"
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	cases := []struct {
		name, method, token string
		f                   *fakeSources
		want                int
	}{
		{"POST", http.MethodPost, token, &fakeSources{token: token}, http.StatusMethodNotAllowed},
		{"no bearer", http.MethodGet, "", &fakeSources{token: token}, http.StatusUnauthorized},
		{"wrong bearer", http.MethodGet, "wrong", &fakeSources{token: token, body: strings.NewReader(archive)},
			http.StatusUnauthorized},
		{"finished scan", http.MethodGet, token, &fakeSources{token: token, finished: true}, http.StatusConflict},
		{"busy", http.MethodGet, token, &fakeSources{token: token, busy: true, body: strings.NewReader(archive)},
			http.StatusConflict},
		{"no archive", http.MethodGet, token, &fakeSources{token: token}, http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &SecuritySourceHandler{Sources: c.f, Limit: 1 << 20, Log: log}
			rec, aborted := getSource(t, h, c.method, c.token)
			if rec.Code != c.want || aborted {
				t.Errorf("%s = %d, aborted %v; want %d", c.name, rec.Code, aborted, c.want)
			}
			if strings.Contains(rec.Body.String(), "secret-archive") {
				t.Errorf("a refused request got the archive")
			}
		})
	}
	if rec, _ := getSource(t, &SecuritySourceHandler{Sources: &fakeSources{token: token}, Limit: 1 << 20, Log: log},
		http.MethodPost, token); rec.Header().Get("Allow") != http.MethodGet {
		t.Errorf("Allow = %q; want GET", rec.Header().Get("Allow"))
	}

	t.Run("served", func(t *testing.T) {
		f := &fakeSources{token: token, body: strings.NewReader(archive)}
		h := &SecuritySourceHandler{Sources: f, Limit: 1 << 20, Log: log}
		before := time.Now()
		rec, aborted := getSource(t, h, http.MethodGet, token)
		if rec.Code != http.StatusOK || aborted || rec.Body.String() != archive {
			t.Fatalf("GET = %d, aborted %v, body %q; want 200 and the archive", rec.Code, aborted, rec.Body.String())
		}
		if ct, ce := rec.Header().Get("Content-Type"), rec.Header().Get("Content-Encoding"); ct != "application/gzip" || ce != "" {
			t.Errorf("Content-Type %q, Content-Encoding %q; want application/gzip and none", ct, ce)
		}
		for name, d := range map[string]time.Time{"write": rec.write, "read": rec.read} {
			if d.Before(before.Add(29*time.Minute)) || d.After(time.Now().Add(31*time.Minute)) {
				t.Errorf("%s deadline = %v; want 30 minutes from now", name, d)
			}
		}
		if !f.closed || len(f.tooLarge) != 0 {
			t.Errorf("closed %v, too large %d; want closed, not too large", f.closed, len(f.tooLarge))
		}
	})
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "secret-archive") {
		t.Errorf("the log holds the token or the archive: %s", logs.String())
	}
}

// errAfter gives its bytes, then fails, as a platform that cuts the archive
// short.
type errAfter struct{ r io.Reader }

func (e *errAfter) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset")
	}
	return n, err
}

// The source stops exactly at the limit: Limit+1 bytes send at most Limit,
// fail the scan once and cut the response off, so pergent never takes it for
// a whole archive; Limit bytes are a full answer. An archive the platform
// cuts short is cut off too, and is no size failure.
func TestSecuritySourceLimit(t *testing.T) {
	t.Parallel()
	const token, limit = "scan-token-1234567890", 10
	log := slog.New(slog.DiscardHandler)
	cases := []struct {
		name         string
		body         io.Reader
		wantAborted  bool
		wantTooLarge int
	}{
		{"one byte over", strings.NewReader(strings.Repeat("a", limit+1)), true, 1},
		{"far over", strings.NewReader(strings.Repeat("a", 10*limit)), true, 1},
		{"exactly the limit", strings.NewReader(strings.Repeat("a", limit)), false, 0},
		{"under the limit", strings.NewReader("abc"), false, 0},
		{"cut short at the limit", &errAfter{strings.NewReader(strings.Repeat("a", limit))}, true, 0},
		{"cut short", &errAfter{strings.NewReader("abc")}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeSources{token: token, body: c.body}
			rec, aborted := getSource(t, &SecuritySourceHandler{Sources: f, Limit: limit, Log: log}, http.MethodGet, token)
			if rec.Body.Len() > limit {
				t.Errorf("sent %d bytes; want at most %d", rec.Body.Len(), limit)
			}
			if aborted != c.wantAborted || len(f.tooLarge) != c.wantTooLarge {
				t.Errorf("aborted %v, too large %d; want %v, %d", aborted, len(f.tooLarge), c.wantAborted, c.wantTooLarge)
			}
			if c.wantTooLarge > 0 && f.tooLarge[0] != token {
				t.Errorf("SecuritySourceTooLarge got another token")
			}
			if !f.closed {
				t.Errorf("the archive was not closed")
			}
		})
	}
}
