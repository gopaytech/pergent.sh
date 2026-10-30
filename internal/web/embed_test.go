package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte(`<!doctype html><div id="root"></div>`)},
		"assets/index-abc.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc.css": {Data: []byte("body{}")},
		"favicon.svg":          {Data: []byte("<svg/>")},
	}
}

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// Unknown paths get index.html (the router in the browser takes over); paths
// owned by the server's other routes do not.
func TestSPAFallback(t *testing.T) {
	h := newHandler(testFS())
	for _, p := range []string{"/", "/repos", "/repos/acme/widgets/reviews/12", "/index.html", "/missing.txt"} {
		rec := get(h, http.MethodGet, p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
			t.Errorf("GET %s = %d %q, want index.html", p, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s content type = %q", p, ct)
		}
	}
	for _, p := range []string{"/api/", "/api/me", "/auth/login", "/auth/callback", "/webhooks/gitlab/main", "/runs/1/result", "/healthz", "/healthz/x"} {
		if rec := get(h, http.MethodGet, p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	// A missing hashed asset is a 404, not the HTML shell.
	if rec := get(h, http.MethodGet, "/assets/gone-123.js"); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset = %d, want 404", rec.Code)
	}
	// Real files are served as themselves.
	if rec := get(h, http.MethodGet, "/favicon.svg"); rec.Body.String() != "<svg/>" {
		t.Errorf("favicon = %q", rec.Body.String())
	}
	// Only reads.
	if rec := get(h, http.MethodPost, "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", rec.Code)
	}
	if rec := get(h, http.MethodHead, "/repos"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD /repos = %d with %d body bytes", rec.Code, rec.Body.Len())
	}
}

func TestAssetCacheHeaders(t *testing.T) {
	h := newHandler(testFS())
	const immutable = "public, max-age=31536000, immutable"
	for _, p := range []string{"/assets/index-abc.js", "/assets/index-abc.css"} {
		if got := get(h, http.MethodGet, p).Header().Get("Cache-Control"); got != immutable {
			t.Errorf("%s Cache-Control = %q, want %q", p, got, immutable)
		}
	}
	for _, p := range []string{"/", "/index.html", "/repos/x"} {
		if got := get(h, http.MethodGet, p).Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", p, got)
		}
	}
	if ct := get(h, http.MethodGet, "/assets/index-abc.js").Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("js content type = %q", ct)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHandler(testFS())
	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	}
	for _, p := range []string{"/", "/assets/index-abc.js", "/favicon.svg", "/repos/x"} {
		rec := get(h, http.MethodGet, p)
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("GET %s %s = %q, want %q", p, k, got, v)
			}
		}
	}
}

// The binary builds without Node: the committed fallback page is embedded.
func TestEmbeddedHandlerServesAPage(t *testing.T) {
	rec := get(Handler(), http.MethodGet, "/anything")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("embedded GET = %d %q", rec.Code, rec.Body.String())
	}
}
