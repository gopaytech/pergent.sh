package callback

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	runID = "11111111-1111-1111-1111-111111111111"
	base  = "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"
	head  = "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b"
)

var now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func resultJSON(b, h string) string {
	return `{"schema_version":1,"status":"error","base":"` + b + `","head":"` + h + `",` +
		`"error":{"code":"internal","message":"x"},"comment":{"markdown":"hi"}}`
}

// fake is a Results with one run, one good token and a scripted outcome.
type fake struct {
	outcome  store.ResultOutcome
	storeErr error

	token    string // the token that StoreResult accepts
	known    bool
	run      store.Run
	runErr   error
	replaced []string
	reported []string
	stored   int
	replErr  error
}

func (f *fake) Run(_ context.Context, id string) (store.Run, bool, error) {
	if f.runErr != nil {
		return store.Run{}, false, f.runErr
	}
	return f.run, f.known, nil
}

func (f *fake) StoreResult(_ context.Context, id, token string, _ []byte, _ time.Time) (store.ResultOutcome, error) {
	if f.storeErr != nil {
		return 0, f.storeErr
	}
	if !f.known || token != f.token {
		return store.ResultUnauthorized, nil
	}
	if f.outcome == store.ResultStored {
		f.stored++
	}
	return f.outcome, nil
}

func (f *fake) ReportFailure(_ context.Context, id, reason string, _ time.Time) (bool, error) {
	f.reported = append(f.reported, reason)
	return true, nil
}

func (f *fake) ReplaceWithFailure(_ context.Context, id, reason string) error {
	f.replaced = append(f.replaced, reason)
	return f.replErr
}

func newFake() *fake {
	return &fake{known: true, token: "good", run: store.Run{ID: runID, BaseSHA: base, HeadSHA: head}}
}

type env struct {
	f     *fake
	woken int
	h     http.Handler
}

func newEnv(f *fake) *env {
	e := &env{f: f}
	h := &Handler{Results: f, Now: func() time.Time { return now }, Wake: func() { e.woken++ }}
	mux := http.NewServeMux()
	mux.Handle("POST /runs/{id}/result", h)
	mux.Handle("/runs/{id}/result", h)
	e.h = mux
	return e
}

func (e *env) post(id, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/runs/"+id+"/result", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestValidResultStored(t *testing.T) {
	e := newEnv(newFake())
	rec := e.post(runID, "Bearer good", resultJSON(base, head))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if e.woken != 1 || e.f.stored != 1 || len(e.f.replaced) != 0 {
		t.Errorf("woken=%d stored=%d replaced=%v", e.woken, e.f.stored, e.f.replaced)
	}
}

func TestMissingBearer401(t *testing.T) {
	for _, auth := range []string{"", "Basic good", "good", "Bearer "} {
		e := newEnv(newFake())
		if rec := e.post(runID, auth, resultJSON(base, head)); rec.Code != 401 {
			t.Errorf("auth %q: status = %d", auth, rec.Code)
		}
		if e.f.stored != 0 || e.woken != 0 {
			t.Errorf("auth %q: stored=%d woken=%d", auth, e.f.stored, e.woken)
		}
	}
}

func TestWrongToken401(t *testing.T) {
	e := newEnv(newFake())
	if rec := e.post(runID, "Bearer bad", resultJSON(base, head)); rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.f.reported)+len(e.f.replaced) != 0 || e.woken != 0 {
		t.Errorf("reported=%v replaced=%v woken=%d", e.f.reported, e.f.replaced, e.woken)
	}
}

func TestDuplicate409(t *testing.T) {
	f := newFake()
	f.outcome = store.ResultConflict
	e := newEnv(f)
	if rec := e.post(runID, "Bearer good", resultJSON(base, head)); rec.Code != 409 {
		t.Fatalf("status = %d", rec.Code)
	}
	if e.woken != 0 || len(f.replaced) != 0 {
		t.Errorf("woken=%d replaced=%v", e.woken, f.replaced)
	}
}

func TestSuperseded200(t *testing.T) {
	f := newFake()
	f.outcome = store.ResultDiscarded
	e := newEnv(f)
	if rec := e.post(runID, "Bearer good", resultJSON(base, head)); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if e.woken != 0 || len(f.replaced) != 0 {
		t.Errorf("woken=%d replaced=%v", e.woken, f.replaced)
	}
}

func TestTooLarge413(t *testing.T) {
	e := newEnv(newFake())
	big := strings.Repeat("a", 11<<20+1)
	if rec := e.post(runID, "Bearer good", big); rec.Code != 413 {
		t.Fatalf("status = %d", rec.Code)
	}
	if e.f.stored != 0 {
		t.Errorf("stored a too-large body")
	}
}

func TestHeadMismatchFailsRun(t *testing.T) {
	e := newEnv(newFake())
	other := strings.Repeat("a", 40)
	rec := e.post(runID, "Bearer good", resultJSON(base, other))
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.f.replaced) != 1 || !strings.Contains(e.f.replaced[0], "different commit") ||
		!strings.HasPrefix(e.f.replaced[0], "pergent sent a result pergent.sh couldn't use: ") {
		t.Errorf("replaced = %v", e.f.replaced)
	}
	if e.woken != 1 {
		t.Errorf("woken = %d", e.woken)
	}
}

func TestBaseMismatchFailsRun(t *testing.T) {
	e := newEnv(newFake())
	rec := e.post(runID, "Bearer good", resultJSON(strings.Repeat("b", 40), head))
	if rec.Code != 400 || len(e.f.replaced) != 1 || !strings.Contains(e.f.replaced[0], "different commit") {
		t.Errorf("status = %d replaced = %v", rec.Code, e.f.replaced)
	}
}

func TestUnparseableResultFailsRun(t *testing.T) {
	e := newEnv(newFake())
	rec := e.post(runID, "Bearer good", `{"schema_version":99}`)
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.f.replaced) != 1 || !strings.Contains(e.f.replaced[0], "schema_version") || e.woken != 1 {
		t.Errorf("replaced = %v woken = %d", e.f.replaced, e.woken)
	}
}

func TestInvalidJSONWithGoodTokenFailsRun(t *testing.T) {
	f := newFake()
	f.storeErr = store.ErrInvalidResult
	e := newEnv(f)
	rec := e.post(runID, "Bearer good", `not json`)
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(f.reported) != 1 || !strings.HasPrefix(f.reported[0], "pergent sent a result pergent.sh couldn't use: ") || e.woken != 1 {
		t.Errorf("reported = %v woken = %d", f.reported, e.woken)
	}
}

func TestGarbageWithBadTokenIs401(t *testing.T) {
	e := newEnv(newFake())
	if rec := e.post(runID, "Bearer bad", `not json at all`); rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.f.reported)+len(e.f.replaced) != 0 || e.woken != 0 {
		t.Errorf("reported=%v replaced=%v woken=%d", e.f.reported, e.f.replaced, e.woken)
	}
}

func TestUnknownRun401(t *testing.T) {
	f := newFake()
	f.known = false
	e := newEnv(f)
	if rec := e.post(runID, "Bearer good", resultJSON(base, head)); rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec := e.post("not-a-uuid", "Bearer good", resultJSON(base, head)); rec.Code != 401 {
		t.Fatalf("malformed id: status = %d", rec.Code)
	}
}

func TestNonPost405(t *testing.T) {
	e := newEnv(newFake())
	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/result", nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestStorageErrorIs500AndLeavesRetry(t *testing.T) {
	f := newFake()
	f.storeErr = errors.New("db down")
	e := newEnv(f)
	if rec := e.post(runID, "Bearer good", resultJSON(base, head)); rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	if e.woken != 0 {
		t.Errorf("woken on error")
	}
}

func TestReplaceErrorIs500(t *testing.T) {
	f := newFake()
	f.replErr = errors.New("db down")
	e := newEnv(f)
	if rec := e.post(runID, "Bearer good", resultJSON(base, strings.Repeat("a", 40))); rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestTokenNeverInResponse(t *testing.T) {
	e := newEnv(newFake())
	rec := e.post(runID, "Bearer secret-token-value", resultJSON(base, head))
	b, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(b), "secret-token-value") {
		t.Errorf("token echoed: %s", b)
	}
}
