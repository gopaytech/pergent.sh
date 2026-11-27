package callback

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gopaytech/pergent.sh/internal/security"
)

const (
	// sourceTimeout bounds one source download: the platform's archive of a
	// big repository takes longer than the server's 60 s timeouts.
	sourceTimeout = 30 * time.Minute
	// sourceFailTimeout bounds failing a scan whose source is too large.
	sourceFailTimeout = 30 * time.Second
)

// SecuritySources is the part of the engine the security scan source route
// needs.
type SecuritySources interface {
	OpenSecuritySource(ctx context.Context, id, token string) (io.ReadCloser, security.Outcome, error)
	SecuritySourceTooLarge(ctx context.Context, id, token string) error
}

// SecuritySourceHandler serves GET /security-scans/{id}/source: the
// platform's .tar.gz of the scan's commit, which the scan Job downloads with
// its result token (not spent). Another method is 405, a missing or wrong
// token 401, a scan that is not running 409, and a platform that gives no
// archive 502. Else the archive is copied as it is (200, application/gzip),
// at most Limit bytes: at one byte more the response is cut off and the scan
// fails, its Job deleted. A response the platform cuts short is cut off too,
// so the Job never takes part of an archive for all of it. Nothing is
// stored, and neither the token nor a byte of the archive is logged.
type SecuritySourceHandler struct {
	Sources SecuritySources
	// Limit is security.max_source_bytes.
	Limit int64
	Log   *slog.Logger
}

func (h *SecuritySourceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	log := logOr(h.Log).With("scan", id)

	// The server's WriteTimeout would cut the stream at 60 s, and its
	// ReadTimeout would cancel the request's context: move both.
	deadline := time.Now().Add(sourceTimeout)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(deadline)
	_ = rc.SetReadDeadline(deadline)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()

	body, outcome, err := h.Sources.OpenSecuritySource(ctx, id, token)
	switch {
	case errors.Is(err, security.ErrNoSource):
		// The engine logged the platform's error.
		http.Error(w, "the platform did not give the source", http.StatusBadGateway)
		return
	case err != nil:
		log.Error("security scan: opening the source failed", "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	case outcome == security.Conflict:
		http.Error(w, "the scan is not running", http.StatusConflict)
		return
	case outcome != security.Stored:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	n, err := io.Copy(w, io.LimitReader(body, h.Limit))
	if err != nil {
		log.Info("security scan: the source download stopped", "bytes", n)
		panic(http.ErrAbortHandler)
	}
	if n < h.Limit {
		log.Info("security scan: source served", "bytes", n)
		return
	}
	// At the limit: one byte more tells an archive of exactly the limit from
	// a bigger one.
	var one [1]byte
	k, err := io.ReadFull(body, one[:])
	switch {
	case k == 0 && errors.Is(err, io.EOF):
		log.Info("security scan: source served", "bytes", n)
		return
	case k == 0:
		log.Info("security scan: the source download stopped", "bytes", n)
		panic(http.ErrAbortHandler)
	}
	log.Warn("security scan: the source is over the size limit", "limit", h.Limit)
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(r.Context()), sourceFailTimeout)
	err = h.Sources.SecuritySourceTooLarge(fctx, id, token)
	fcancel()
	if err != nil {
		log.Error("security scan: recording a source over the size limit failed", "error", err.Error())
	}
	panic(http.ErrAbortHandler)
}
