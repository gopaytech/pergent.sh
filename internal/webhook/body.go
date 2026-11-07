package webhook

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// maxBufferedBytes is how many bytes of webhook bodies are held at once, over
// both platforms. GitHub's signature covers the whole body, so a body is read
// before its sender is known: without a bound, parallel uploads from anyone
// could take the pod's memory, and the pod serves everything. It counts bytes
// and not requests, so a client that sends slowly holds nothing a delivery
// needs. A real delivery is far smaller than maxBodyBytes.
const maxBufferedBytes = 48 << 20

// bodyReadTimeout is how long a body may take to arrive; the server's own
// limit is longer. GitHub gives up on a delivery after 10 seconds. A variable
// so a test can shorten it.
var bodyReadTimeout = 15 * time.Second

var errBodyBudget = errors.New("webhook: too many bytes buffered")

// byteBudget counts the bytes that may still be buffered.
type byteBudget struct{ left atomic.Int64 }

func newByteBudget(n int64) *byteBudget {
	b := &byteBudget{}
	b.left.Store(n)
	return b
}

func (b *byteBudget) take(n int64) bool {
	if b.left.Add(-n) < 0 {
		b.left.Add(n)
		return false
	}
	return true
}

func (b *byteBudget) give(n int64) { b.left.Add(n) }

var bodyBudget = newByteBudget(maxBufferedBytes)

// budgetReader charges what it reads to bodyBudget.
type budgetReader struct {
	r     io.Reader
	taken int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		if !bodyBudget.take(int64(n)) {
			return 0, errBodyBudget
		}
		b.taken += int64(n)
	}
	return n, err
}

// readBody reads the request's body within maxBodyBytes, bodyReadTimeout and
// the shared budget. ok is false when the request was answered; otherwise the
// caller calls release when it is done with the body.
func readBody(w http.ResponseWriter, r *http.Request, log *slog.Logger) (body []byte, release func(), ok bool) {
	// A writer without deadlines (a test's recorder) just has none.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
	br := &budgetReader{r: http.MaxBytesReader(w, r.Body, maxBodyBytes)}
	body, err := io.ReadAll(br)
	if err == nil {
		return body, func() { bodyBudget.give(br.taken) }, true
	}
	bodyBudget.give(br.taken)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		log.Warn("webhook rejected", "outcome", "body_too_large")
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, errBodyBudget):
		log.Warn("webhook rejected", "outcome", "busy")
		w.Header().Set("Retry-After", "5")
		http.Error(w, "busy", http.StatusServiceUnavailable)
	default:
		log.Warn("webhook rejected", "outcome", "read_error")
		http.Error(w, "cannot read body", http.StatusBadRequest)
	}
	return nil, nil, false
}
