// Package web serves the dashboard's single-page app, built by `make web`
// into dist/ and embedded in the binary.
package web

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// dist is the Vite build, written by `make web`. Only dist/.keep is tracked,
// so the binary builds without Node; then fallback is served instead.
//
//go:embed all:dist
var dist embed.FS

// fallback is the page served while dist/ holds no build.
//
//go:embed fallback.html
var fallback []byte

// csp lets the page load only its own scripts, styles and images (and data:
// images); Vite emits no inline script or style.
const csp = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'"

// noncePlaceholder is where index.html takes its style nonce
// (<meta name="csp-nonce" nonce="__CSP_NONCE__">). The page hands it to the
// Radix dialogs and menus, which add one <style> tag to stop the page
// scrolling under them. Scripts get no nonce: script-src stays 'self'.
var noncePlaceholder = []byte("__CSP_NONCE__")

// pageCSP is csp for an HTML page that carries a style nonce.
func pageCSP(nonce string) string {
	return strings.Replace(csp, "style-src 'self'", "style-src 'self' 'nonce-"+nonce+"'", 1)
}

// newNonce is 128 random bits, base64: a new one for every page served.
func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return base64.StdEncoding.EncodeToString(b)
}

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
			if name == "index.html" {
				servePage(w, r, data)
			} else {
				serve(w, r, name, data)
			}
			return
		}
		// A missing hashed asset is an error, not the shell: the page would
		// otherwise parse HTML as script.
		if strings.HasPrefix(p, "/assets/") {
			http.NotFound(w, r)
			return
		}
		// Not a file: a root-level name that looks like one (/favicon.ico)
		// is a 404, not HTML. Deeper paths are client routes and may end in
		// a repository name with a dot (acme/widgets.io).
		if strings.Count(p, "/") == 1 && path.Ext(p) != "" {
			http.NotFound(w, r)
			return
		}
		data, ok := readFile(fsys, "index.html")
		if !ok {
			data = fallback
		}
		servePage(w, r, data)
	})
}

// readFile returns the contents of a regular file in fsys.
func readFile(fsys fs.FS, name string) ([]byte, bool) {
	// Dotfiles (dist/.keep) are never served.
	if !fs.ValidPath(name) || strings.HasPrefix(path.Base(name), ".") {
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

// servePage serves the HTML shell with a fresh style nonce in it and in its
// CSP. A page with no placeholder (the fallback) keeps the plain CSP.
func servePage(w http.ResponseWriter, r *http.Request, data []byte) {
	if bytes.Contains(data, noncePlaceholder) {
		nonce := newNonce()
		data = bytes.ReplaceAll(data, noncePlaceholder, []byte(nonce))
		w.Header().Set("Content-Security-Policy", pageCSP(nonce))
	}
	serve(w, r, "index.html", data)
}

func serve(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	if strings.HasPrefix(name, "assets/") {
		// Vite names these by content hash, so they never change.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else if w.Header().Get("Cache-Control") == "" {
		// A wrapper that set a session cookie on this response has already
		// said "private, no-store": keep it.
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
