package github

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Tarball follows GitHub's redirect to the download host without the token,
// gives the bytes as that host sends them (never un-gzipped), and the
// client's timeout does not cut a long download (only ctx bounds it).
func TestGitHubTarball(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	archive := []byte("\x1f\x8b\x08\x00-gzip-bytes")
	var gotAuth, gotEncoding = "unset", ""
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotEncoding = r.Header.Get("Authorization"), r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/x-gzip")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(archive)
	}))
	defer download.Close()
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, download.URL+"/acme-widgets-0123?token=signed", http.StatusFound)
	})
	// A timeout that has passed already: a request it bounded would fail.
	c.hc.Timeout = time.Nanosecond
	rc, err := c.Tarball(ctx, "acme", "widgets", sha)
	if err != nil {
		t.Fatalf("Tarball: %v", err)
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(b, archive) {
		t.Fatalf("body = %q, %v; want the bytes as sent", b, err)
	}
	first := rec.get(0)
	if first.Path != "/repos/acme/widgets/tarball/"+sha || first.Header.Get("Authorization") == "" {
		t.Errorf("first request = %s with Authorization %q", first.Path, first.Header.Get("Authorization"))
	}
	if gotAuth != "" {
		t.Errorf("the download host got Authorization %q", gotAuth)
	}
	if gotEncoding != "identity" || first.Header.Get("Accept-Encoding") != "identity" {
		t.Errorf("Accept-Encoding = %q, %q; want identity", first.Header.Get("Accept-Encoding"), gotEncoding)
	}
}
