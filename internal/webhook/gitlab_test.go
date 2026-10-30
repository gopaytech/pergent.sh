package webhook

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	testSecret = "example-webhook-secret"
	openHead   = "1111111111111111111111111111111111111111"
	pushHead   = "2222222222222222222222222222222222222222"
)

type enqueueCall struct {
	mr                        store.MRKey
	projectPath, head, branch string
}

type fakeQueue struct {
	enqueues   []enqueueCall
	supersedes []store.MRKey
	created    bool
	err        error
}

func (q *fakeQueue) EnqueueRun(_ context.Context, mr store.MRKey, projectPath, headSHA, targetBranch string) (string, bool, error) {
	q.enqueues = append(q.enqueues, enqueueCall{mr, projectPath, headSHA, targetBranch})
	if q.err != nil {
		return "", false, q.err
	}
	return "run-1", q.created, nil
}

func (q *fakeQueue) SupersedeMR(_ context.Context, mr store.MRKey) (int, error) {
	q.supersedes = append(q.supersedes, mr)
	if q.err != nil {
		return 0, q.err
	}
	return 1, nil
}

type rig struct {
	q     *fakeQueue
	wakes int
	mux   *http.ServeMux
	logs  *bytes.Buffer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{q: &fakeQueue{created: true}, logs: &bytes.Buffer{}}
	h := &Handler{
		Config: &config.Config{
			GitLab:       []config.Instance{{Name: "main", URL: "https://gitlab.example.com"}},
			Repositories: []config.Repository{{GitLab: "main", Project: "Acme/Widgets"}},
		},
		Secrets: map[string]string{"main": testSecret},
		Queue:   r.q,
		Wake:    func() { r.wakes++ },
		Log:     slog.New(slog.NewTextHandler(r.logs, nil)),
	}
	r.mux = http.NewServeMux()
	r.mux.Handle("/webhooks/gitlab/{instance}", h)
	return r
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type opt func(*http.Request)

func withToken(v string) opt { return func(r *http.Request) { r.Header.Set("X-Gitlab-Token", v) } }
func withEvent(v string) opt { return func(r *http.Request) { r.Header.Set("X-Gitlab-Event", v) } }
func noHeader(k string) opt  { return func(r *http.Request) { r.Header.Del(k) } }

func (r *rig) post(path string, body []byte, opts ...opt) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", testSecret)
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func (r *rig) hook(body []byte, opts ...opt) *httptest.ResponseRecorder {
	return r.post("/webhooks/gitlab/main", body, opts...)
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}

func TestOpenEnqueues(t *testing.T) {
	r := newRig(t)
	wantStatus(t, r.hook(fixture(t, "mr_open.json")), 200)
	if len(r.q.enqueues) != 1 {
		t.Fatalf("enqueues = %d, want 1", len(r.q.enqueues))
	}
	want := enqueueCall{store.MRKey{Instance: "main", ProjectID: 42, IID: 7}, "Acme/Widgets", openHead, "main"}
	if got := r.q.enqueues[0]; got != want {
		t.Fatalf("enqueue = %+v, want %+v", got, want)
	}
	if len(r.q.supersedes) != 0 {
		t.Fatal("open must not supersede")
	}
}

func TestReopenEnqueues(t *testing.T) {
	r := newRig(t)
	body := bytes.Replace(fixture(t, "mr_open.json"), []byte(`"open"`), []byte(`"reopen"`), 1)
	wantStatus(t, r.hook(body), 200)
	if len(r.q.enqueues) != 1 {
		t.Fatalf("enqueues = %d, want 1", len(r.q.enqueues))
	}
}

func TestPushEnqueues(t *testing.T) {
	r := newRig(t)
	wantStatus(t, r.hook(fixture(t, "mr_push.json")), 200)
	if len(r.q.enqueues) != 1 || r.q.enqueues[0].head != pushHead {
		t.Fatalf("enqueues = %+v, want one for %s", r.q.enqueues, pushHead)
	}
}

func TestTitleEditIgnored(t *testing.T) {
	r := newRig(t)
	wantStatus(t, r.hook(fixture(t, "mr_title_edit.json")), 200)
	if len(r.q.enqueues)+len(r.q.supersedes) != 0 || r.wakes != 0 {
		t.Fatalf("title edit touched the queue: %+v %+v wakes=%d", r.q.enqueues, r.q.supersedes, r.wakes)
	}
}

// A title edit reports its changes too; only a target branch change counts.
func TestTitleEditWithChangesIgnored(t *testing.T) {
	r := newRig(t)
	body := bytes.Replace(fixture(t, "mr_retarget.json"), []byte(`"target_branch": {"previous": "main", "current": "develop"}`),
		[]byte(`"title": {"previous": "a", "current": "b"}`), 1)
	wantStatus(t, r.hook(body), 200)
	if len(r.q.enqueues)+len(r.q.supersedes) != 0 || r.wakes != 0 {
		t.Fatalf("title edit touched the queue: %+v %+v wakes=%d", r.q.enqueues, r.q.supersedes, r.wakes)
	}
}

// Retargeting a merge request sends "update" without oldrev: the head is
// reviewed again against the new target branch.
func TestRetargetEnqueues(t *testing.T) {
	r := newRig(t)
	wantStatus(t, r.hook(fixture(t, "mr_retarget.json")), 200)
	want := enqueueCall{store.MRKey{Instance: "main", ProjectID: 42, IID: 7}, "Acme/Widgets", openHead, "develop"}
	if len(r.q.enqueues) != 1 || r.q.enqueues[0] != want {
		t.Fatalf("enqueues = %+v, want %+v", r.q.enqueues, want)
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d, want 1", r.wakes)
	}
}

func TestCloseSupersedes(t *testing.T) {
	r := newRig(t)
	wantStatus(t, r.hook(fixture(t, "mr_close.json")), 200)
	want := store.MRKey{Instance: "main", ProjectID: 42, IID: 7}
	if len(r.q.supersedes) != 1 || r.q.supersedes[0] != want {
		t.Fatalf("supersedes = %+v, want [%+v]", r.q.supersedes, want)
	}
	if len(r.q.enqueues) != 0 {
		t.Fatal("close must not enqueue")
	}
}

func TestMergeSupersedes(t *testing.T) {
	r := newRig(t)
	wantStatus(t, r.hook(fixture(t, "mr_merge.json")), 200)
	if len(r.q.supersedes) != 1 {
		t.Fatalf("supersedes = %d, want 1", len(r.q.supersedes))
	}
}

func TestBadTokenIs401(t *testing.T) {
	for name, o := range map[string]opt{
		"wrong":   withToken("nope"),
		"empty":   withToken(""),
		"missing": noHeader("X-Gitlab-Token"),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			wantStatus(t, r.hook(fixture(t, "mr_open.json"), o), 401)
			if len(r.q.enqueues)+len(r.q.supersedes) != 0 {
				t.Fatal("queue was called")
			}
		})
	}
}

func TestNoConfiguredSecretNeverAuthenticates(t *testing.T) {
	r := newRig(t)
	h := &Handler{Config: &config.Config{GitLab: []config.Instance{{Name: "main"}}}, Secrets: map[string]string{}, Queue: r.q}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/main", strings.NewReader("{}"))
	req.SetPathValue("instance", "main")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	wantStatus(t, rec, 401)
}

func TestUnknownInstanceIs404(t *testing.T) {
	r := newRig(t)
	wantStatus(t, r.post("/webhooks/gitlab/other", fixture(t, "mr_open.json")), 404)
}

func TestNonPostIs405(t *testing.T) {
	r := newRig(t)
	req := httptest.NewRequest(http.MethodGet, "/webhooks/gitlab/main", nil)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	wantStatus(t, rec, 405)
}

func TestUnknownProjectIgnored(t *testing.T) {
	r := newRig(t)
	body := bytes.Replace(fixture(t, "mr_open.json"), []byte("acme/widgets"), []byte("acme/other"), 1)
	wantStatus(t, r.hook(body), 200)
	if len(r.q.enqueues)+len(r.q.supersedes) != 0 {
		t.Fatal("unknown project touched the queue")
	}
}

func TestProjectMatchIsCaseInsensitive(t *testing.T) {
	r := newRig(t)
	body := bytes.Replace(fixture(t, "mr_open.json"), []byte("acme/widgets"), []byte("ACME/WIDGETS"), 1)
	wantStatus(t, r.hook(body), 200)
	if len(r.q.enqueues) != 1 || r.q.enqueues[0].projectPath != "Acme/Widgets" {
		t.Fatalf("enqueues = %+v, want config spelling", r.q.enqueues)
	}
}

func TestOtherEventIgnored(t *testing.T) {
	r := newRig(t)
	wantStatus(t, r.hook(fixture(t, "mr_open.json"), withEvent("Push Hook")), 200)
	wantStatus(t, r.hook(fixture(t, "mr_open.json"), noHeader("X-Gitlab-Event")), 200)
	if len(r.q.enqueues)+len(r.q.supersedes) != 0 {
		t.Fatal("other event touched the queue")
	}
}

func TestStoreErrorIs500(t *testing.T) {
	for _, fx := range []string{"mr_open.json", "mr_close.json"} {
		r := newRig(t)
		r.q.err = errors.New("db down")
		wantStatus(t, r.hook(fixture(t, fx)), 500)
		if r.wakes != 0 {
			t.Fatal("woke on error")
		}
	}
}

func TestWakeOnlyWhenCreated(t *testing.T) {
	r := newRig(t)
	wantStatus(t, r.hook(fixture(t, "mr_open.json")), 200)
	if r.wakes != 1 {
		t.Fatalf("wakes = %d, want 1", r.wakes)
	}
	r.q.created = false
	wantStatus(t, r.hook(fixture(t, "mr_open.json")), 200)
	if r.wakes != 1 {
		t.Fatalf("wakes = %d after duplicate, want still 1", r.wakes)
	}
	wantStatus(t, r.hook(fixture(t, "mr_close.json")), 200)
	if r.wakes != 1 {
		t.Fatalf("wakes = %d after close, want still 1", r.wakes)
	}
}

func TestNilWakeIsSafe(t *testing.T) {
	r := newRig(t)
	h := &Handler{Config: &config.Config{
		GitLab:       []config.Instance{{Name: "main"}},
		Repositories: []config.Repository{{GitLab: "main", Project: "acme/widgets"}},
	}, Secrets: map[string]string{"main": testSecret}, Queue: r.q}
	mux := http.NewServeMux()
	mux.Handle("/webhooks/gitlab/{instance}", h)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/main", bytes.NewReader(fixture(t, "mr_open.json")))
	req.Header.Set("X-Gitlab-Token", testSecret)
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	wantStatus(t, rec, 200)
}

func TestOversizedBodyIs413(t *testing.T) {
	r := newRig(t)
	big := append([]byte(`{"pad":"`), bytes.Repeat([]byte("a"), 5<<20)...)
	big = append(big, []byte(`"}`)...)
	wantStatus(t, r.hook(big), 413)
	if len(r.q.enqueues) != 0 {
		t.Fatal("queue was called")
	}
}

// An authenticated hook GitLab can't fix by sending again is answered 200,
// with a warning: GitLab counts 4xx answers toward disabling the hook.
func TestMalformedJSONIs200(t *testing.T) {
	r := newRig(t)
	wantStatus(t, r.hook([]byte("{not json")), 200)
	if len(r.q.enqueues)+len(r.q.supersedes) != 0 {
		t.Fatal("queue was called")
	}
	if !strings.Contains(r.logs.String(), "level=WARN") || !strings.Contains(r.logs.String(), "malformed_json") {
		t.Errorf("not logged as a warning: %s", r.logs.String())
	}
}

func TestMissingIDsIs200(t *testing.T) {
	for name, body := range map[string][]byte{
		"project id": bytes.Replace(fixture(t, "mr_open.json"), []byte(`"id": 42`), []byte(`"id": 0`), 1),
		"iid":        bytes.Replace(fixture(t, "mr_open.json"), []byte(`"iid": 7`), []byte(`"iid": 0`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			wantStatus(t, r.hook(body), 200)
			if len(r.q.enqueues)+len(r.q.supersedes) != 0 {
				t.Fatal("queue was called")
			}
			if !strings.Contains(r.logs.String(), "level=WARN") || !strings.Contains(r.logs.String(), "missing_ids") {
				t.Errorf("not logged as a warning: %s", r.logs.String())
			}
		})
	}
}

func TestBadHeadSHAIs200(t *testing.T) {
	for name, head := range map[string]string{
		"empty":     "",
		"short":     "abc123",
		"uppercase": strings.Repeat("A", 40),
		"non-hex":   strings.Repeat("g", 40),
		"41 chars":  strings.Repeat("a", 41),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			body := bytes.Replace(fixture(t, "mr_open.json"), []byte(openHead), []byte(head), 1)
			wantStatus(t, r.hook(body), 200)
			if len(r.q.enqueues) != 0 {
				t.Fatal("garbage head was enqueued")
			}
			if !strings.Contains(r.logs.String(), "level=WARN") || !strings.Contains(r.logs.String(), "bad_head_sha") {
				t.Errorf("not logged as a warning: %s", r.logs.String())
			}
		})
	}
}

func TestSHA256HeadAccepted(t *testing.T) {
	r := newRig(t)
	head := strings.Repeat("ab", 32)
	body := bytes.Replace(fixture(t, "mr_open.json"), []byte(openHead), []byte(head), 1)
	wantStatus(t, r.hook(body), 200)
	if len(r.q.enqueues) != 1 || r.q.enqueues[0].head != head {
		t.Fatalf("enqueues = %+v", r.q.enqueues)
	}
}

func TestUnhandledActionIgnored(t *testing.T) {
	for _, action := range []string{"approved", "unapproved", "approval", "unapproval"} {
		r := newRig(t)
		body := bytes.Replace(fixture(t, "mr_open.json"), []byte(`"open"`), []byte(`"`+action+`"`), 1)
		wantStatus(t, r.hook(body), 200)
		if len(r.q.enqueues)+len(r.q.supersedes) != 0 {
			t.Fatalf("%s touched the queue", action)
		}
	}
}

func TestCloseDoesNotNeedHead(t *testing.T) {
	r := newRig(t)
	body := bytes.Replace(fixture(t, "mr_close.json"), []byte(openHead), []byte(""), 1)
	wantStatus(t, r.hook(body), 200)
	if len(r.q.supersedes) != 1 {
		t.Fatal("close without a head must still supersede")
	}
}

func TestNeverLogsTokenOrBody(t *testing.T) {
	r := newRig(t)
	r.hook(fixture(t, "mr_open.json"))
	r.hook(fixture(t, "mr_open.json"), withToken("wrong-token-value"))
	logs := r.logs.String()
	if logs == "" {
		t.Fatal("expected log output")
	}
	for _, bad := range []string{testSecret, "wrong-token-value", "last_commit"} {
		if strings.Contains(logs, bad) {
			t.Fatalf("log leaks %q: %s", bad, logs)
		}
	}
}
