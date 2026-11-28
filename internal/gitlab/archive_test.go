package gitlab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// Archive opens the .tar.gz of one commit as GitLab sends it: the bytes are
// never un-gzipped (a reply marked Content-Encoding: gzip included), and the
// client's timeout does not cut a long download (only ctx bounds it).
func TestGitLabArchive(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	archive := "\x1f\x8b\x08\x00-gzip-bytes"
	var acceptEncoding string
	c, reqs := serve(t, func(w http.ResponseWriter, r *http.Request) {
		acceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, archive)
	})
	// A timeout that has passed already: a request it bounded would fail.
	c.hc.Timeout = time.Nanosecond
	rc, err := c.Archive(context.Background(), 7, sha)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(b) != archive {
		t.Fatalf("body = %q, %v; want the bytes as sent", b, err)
	}
	expect(t, (*reqs)[0], "GET", "/api/v4/projects/7/repository/archive.tar.gz", "sha="+sha)
	if acceptEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q; want identity", acceptEncoding)
	}

	c, _ = serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusNotFound) })
	if _, err := c.Archive(context.Background(), 7, sha); !errors.Is(err, ErrNotFound) {
		t.Errorf("404 = %v; want ErrNotFound", err)
	}
}
