// Package web serves the dashboard's single-page app, built by `make web`
// into dist/ and embedded in the binary.
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// dist is the Vite build. A committed placeholder index.html lets the binary
// build without Node; `make web` replaces it with the real app.
//
//go:embed all:dist
var dist embed.FS

// csp lets the page load only its own scripts, styles and images (and data:
// images); Vite emits no inline script or style.
const csp = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'"

// reserved are the paths other routes of the server own; an unknown path
// under them is a 404, never the HTML shell.
var reserved = []string{"/api", "/auth", "/webhooks", "/runs", "/healthz"}

// Handler serves the embedded app: files as themselves, and index.html for any
// other path, so the browser's router can handle it.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // "dist" is embedded above; cannot happen
	}
	return newHandler(sub)
}

func newHandler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean("/" + r.URL.Path)
		for _, prefix := range reserved {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				http.NotFound(w, r)
				return
			}
		}

		name := strings.TrimPrefix(p, "/")
		if name == "" {
			name = "index.html"
		}
		if data, ok := readFile(fsys, name); ok {
			serve(w, r, name, data)
			return
		}
		// A missing hashed asset is an error, not the shell: the page would
		// otherwise parse HTML as script.
		if strings.HasPrefix(p, "/assets/") {
			http.NotFound(w, r)
			return
		}
		data, ok := readFile(fsys, "index.html")
		if !ok {
			http.NotFound(w, r)
			return
		}
		serve(w, r, "index.html", data)
	})
}

// readFile returns the contents of a regular file in fsys.
func readFile(fsys fs.FS, name string) ([]byte, bool) {
	if !fs.ValidPath(name) {
		return nil, false
	}
	info, err := fs.Stat(fsys, name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, false
	}
	return data, true
}

func serve(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	if strings.HasPrefix(name, "assets/") {
		// Vite names these by content hash, so they never change.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
