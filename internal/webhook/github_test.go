package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const ghSecret = "example-github-webhook-secret"

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func newGitHubRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{q: &fakeQueue{created: true, republished: true}, logs: &bytes.Buffer{}}
	h := &GitHubHandler{
		Config: &config.Config{
			GitHub:       []config.GitHubInstance{{Name: "gh"}},
			Repositories: []config.Repository{{GitHub: "gh", Project: "Acme/Widgets"}},
		},
		Secrets: map[string]string{"gh": ghSecret},
		Queue:   r.q,
		Wake:    func() { r.wakes++ },
		Log:     slog.New(slog.NewTextHandler(r.logs, nil)),
	}
	r.mux = http.NewServeMux()
	r.mux.Handle("/webhooks/github/{instance}", h)
	return r
}

func withSignature(v string) opt {
	return func(r *http.Request) { r.Header.Set("X-Hub-Signature-256", v) }
}

// gh posts a body signed with the right secret as a pull_request event.
func (r *rig) gh(body []byte, opts ...opt) *httptest.ResponseRecorder {
	return r.ghTo("/webhooks/github/gh", body, opts...)
}

func (r *rig) ghTo(path string, body []byte, opts ...opt) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(ghSecret, body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func noQueueCalls(t *testing.T, r *rig) {
	t.Helper()
	if n := len(r.q.enqueues) + len(r.q.supersedes) + len(r.q.republishes); n != 0 || r.wakes != 0 {
		t.Fatalf("queue touched: %+v %+v %+v wakes=%d", r.q.enqueues, r.q.supersedes, r.q.republishes, r.wakes)
	}
}

func TestGitHubOpenedEnqueues(t *testing.T) {
	r := newGitHubRig(t)
	wantStatus(t, r.gh(fixture(t, "github_opened.json")), 200)
	want := enqueueCall{store.MRKey{Instance: "gh", ProjectID: 42, IID: 7}, "Acme/Widgets", openHead, "main"}
	if len(r.q.enqueues) != 1 || r.q.enqueues[0] != want {
		t.Fatalf("enqueues = %+v, want [%+v]", r.q.enqueues, want)
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d, want 1", r.wakes)
	}
	if len(r.q.supersedes)+len(r.q.republishes) != 0 {
		t.Fatal("opened must only enqueue")
	}
}

func TestGitHubReopenedAndSynchronizeEnqueue(t *testing.T) {
	for fx, head := range map[string]string{"github_reopened.json": openHead, "github_synchronize.json": pushHead} {
		r := newGitHubRig(t)
		wantStatus(t, r.gh(fixture(t, fx)), 200)
		if len(r.q.enqueues) != 1 || r.q.enqueues[0].head != head {
			t.Fatalf("%s: enqueues = %+v, want one for %s", fx, r.q.enqueues, head)
		}
	}
}

// Review Focus 1: an edit of the title or body must not start a run.
func TestGitHubEditedWithoutBaseChangeIgnored(t *testing.T) {
	r := newGitHubRig(t)
	wantStatus(t, r.gh(fixture(t, "github_edited_title.json")), 200)
	noQueueCalls(t, r)
	// A bare edited event carries no changes at all.
	body := bytes.Replace(fixture(t, "github_edited_title.json"), []byte(`"changes": {"title": {"from": "old title"}},`), nil, 1)
	wantStatus(t, r.gh(body), 200)
	noQueueCalls(t, r)
}

// Review Focus 1: a retarget carries changes.base and is reviewed again.
func TestGitHubEditedWithBaseChangeEnqueues(t *testing.T) {
	r := newGitHubRig(t)
	wantStatus(t, r.gh(fixture(t, "github_edited_base.json")), 200)
	want := enqueueCall{store.MRKey{Instance: "gh", ProjectID: 42, IID: 7}, "Acme/Widgets", openHead, "main"}
	if len(r.q.enqueues) != 1 || r.q.enqueues[0] != want {
		t.Fatalf("enqueues = %+v, want [%+v]", r.q.enqueues, want)
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d, want 1", r.wakes)
	}
}

func TestGitHubNullBaseChangeIgnored(t *testing.T) {
	r := newGitHubRig(t)
	body := bytes.Replace(fixture(t, "github_edited_base.json"), []byte(`"base": {"ref": {"from": "develop"}, "sha": {"from": "4444444444444444444444444444444444444444"}}`), []byte(`"base": null`), 1)
	wantStatus(t, r.gh(body), 200)
	noQueueCalls(t, r)
}

func TestGitHubClosedSupersedes(t *testing.T) {
	r := newGitHubRig(t)
	wantStatus(t, r.gh(fixture(t, "github_closed.json")), 200)
	want := store.MRKey{Instance: "gh", ProjectID: 42, IID: 7}
	if len(r.q.supersedes) != 1 || r.q.supersedes[0] != want {
		t.Fatalf("supersedes = %+v, want [%+v]", r.q.supersedes, want)
	}
	if len(r.q.enqueues)+len(r.q.republishes) != 0 || r.wakes != 0 {
		t.Fatal("closed must only supersede")
	}
}

func TestGitHubClosedDoesNotNeedHead(t *testing.T) {
	r := newGitHubRig(t)
	body := bytes.Replace(fixture(t, "github_closed.json"), []byte(openHead), nil, 1)
	wantStatus(t, r.gh(body), 200)
	if len(r.q.supersedes) != 1 {
		t.Fatal("closed without a head must still supersede")
	}
}

func TestGitHubReadyForReviewRepublishes(t *testing.T) {
	r := newGitHubRig(t)
	wantStatus(t, r.gh(fixture(t, "github_ready.json")), 200)
	want := republishCall{store.MRKey{Instance: "gh", ProjectID: 42, IID: 7}, openHead}
	if len(r.q.republishes) != 1 || r.q.republishes[0] != want {
		t.Fatalf("republishes = %+v, want [%+v]", r.q.republishes, want)
	}
	if len(r.q.enqueues)+len(r.q.supersedes) != 0 {
		t.Errorf("ready touched the queue: %+v %+v", r.q.enqueues, r.q.supersedes)
	}
	if r.wakes != 1 {
		t.Errorf("wakes = %d, want 1", r.wakes)
	}
}

// Nothing was republished (the head's review isn't published yet): the normal
// flow reads the draft flag when it publishes, so there is nothing to wake.
func TestGitHubReadyNothingToRepublish(t *testing.T) {
	r := newGitHubRig(t)
	r.q.republished = false
	wantStatus(t, r.gh(fixture(t, "github_ready.json")), 200)
	if len(r.q.republishes) != 1 || r.wakes != 0 {
		t.Fatalf("republishes = %+v, wakes = %d; want one call, no wake", r.q.republishes, r.wakes)
	}
}

func TestGitHubReadyBadHeadIs200(t *testing.T) {
	r := newGitHubRig(t)
	body := bytes.Replace(fixture(t, "github_ready.json"), []byte(openHead), []byte("abc"), 1)
	wantStatus(t, r.gh(body), 200)
	noQueueCalls(t, r)
	if !strings.Contains(r.logs.String(), "bad_head_sha") {
		t.Errorf("not logged: %s", r.logs.String())
	}
}

func TestGitHubConvertedToDraftAndOtherActionsIgnored(t *testing.T) {
	for _, action := range []string{"converted_to_draft", "labeled", "assigned", "review_requested", "locked", "auto_merge_enabled", ""} {
		r := newGitHubRig(t)
		body := bytes.Replace(fixture(t, "github_opened.json"), []byte(`"opened"`), []byte(`"`+action+`"`), 1)
		wantStatus(t, r.gh(body), 200)
		noQueueCalls(t, r)
	}
	r := newGitHubRig(t)
	wantStatus(t, r.gh(fixture(t, "github_draft.json")), 200)
	noQueueCalls(t, r)
}

// Review Focus 2: wrong or missing signature is 401 and the queue is untouched.
func TestGitHubBadSignatureIs401(t *testing.T) {
	body := fixture(t, "github_opened.json")
	for name, o := range map[string]opt{
		"wrong secret":      withSignature(sign("some-other-secret", body)),
		"missing":           noHeader("X-Hub-Signature-256"),
		"empty":             withSignature(""),
		"no prefix":         withSignature(strings.TrimPrefix(sign(ghSecret, body), "sha256=")),
		"sha1 prefix":       withSignature("sha1=" + strings.TrimPrefix(sign(ghSecret, body), "sha256=")),
		"not hex":           withSignature("sha256=zzzz"),
		"truncated":         withSignature(sign(ghSecret, body)[:20]),
		"uppercase prefix":  withSignature("SHA256=" + strings.TrimPrefix(sign(ghSecret, body), "sha256=")),
		"signed other body": withSignature(sign(ghSecret, []byte("{}"))),
	} {
		t.Run(name, func(t *testing.T) {
			r := newGitHubRig(t)
			wantStatus(t, r.gh(body, o), 401)
			noQueueCalls(t, r)
		})
	}
}

// A bad signature wins over an event the handler would ignore anyway.
func TestGitHubBadSignatureBeatsEventCheck(t *testing.T) {
	r := newGitHubRig(t)
	wantStatus(t, r.gh(fixture(t, "github_ping.json"), withEvent2("ping"), withSignature("sha256=00")), 401)
}

func withEvent2(v string) opt { return func(r *http.Request) { r.Header.Set("X-GitHub-Event", v) } }

func TestGitHubNoConfiguredSecretNeverAuthenticates(t *testing.T) {
	r := newGitHubRig(t)
	h := &GitHubHandler{
		Config:  &config.Config{GitHub: []config.GitHubInstance{{Name: "gh"}}},
		Secrets: map[string]string{}, Queue: r.q,
	}
	body := []byte("{}")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/gh", bytes.NewReader(body))
	req.SetPathValue("instance", "gh")
	// A signature made with the empty key must not pass for an unset secret.
	req.Header.Set("X-Hub-Signature-256", sign("", body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	wantStatus(t, rec, 401)
}

func TestGitHubUnknownInstanceIs404(t *testing.T) {
	r := newGitHubRig(t)
	wantStatus(t, r.ghTo("/webhooks/github/other", fixture(t, "github_opened.json")), 404)
	// A GitLab instance name is not a GitHub one.
	wantStatus(t, r.ghTo("/webhooks/github/main", fixture(t, "github_opened.json")), 404)
}

func TestGitHubNonPostIs405(t *testing.T) {
	r := newGitHubRig(t)
	req := httptest.NewRequest(http.MethodGet, "/webhooks/github/gh", nil)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	wantStatus(t, rec, 405)
}

func TestGitHubOversizedBodyIs413(t *testing.T) {
	r := newGitHubRig(t)
	big := append([]byte(`{"pad":"`), bytes.Repeat([]byte("a"), 5<<20)...)
	big = append(big, []byte(`"}`)...)
	wantStatus(t, r.gh(big), 413)
	noQueueCalls(t, r)
}

func TestGitHubPingIs200(t *testing.T) {
	r := newGitHubRig(t)
	wantStatus(t, r.gh(fixture(t, "github_ping.json"), withEvent2("ping")), 200)
	noQueueCalls(t, r)
}

func TestGitHubOtherEventIgnored(t *testing.T) {
	for _, ev := range []string{"push", "issue_comment", "pull_request_review", ""} {
		r := newGitHubRig(t)
		o := withEvent2(ev)
		if ev == "" {
			o = noHeader("X-GitHub-Event")
		}
		wantStatus(t, r.gh(fixture(t, "github_opened.json"), o), 200)
		noQueueCalls(t, r)
	}
}

func TestGitHubUnknownRepositoryIgnored(t *testing.T) {
	r := newGitHubRig(t)
	body := bytes.Replace(fixture(t, "github_opened.json"), []byte("acme/widgets"), []byte("acme/other"), 1)
	wantStatus(t, r.gh(body), 200)
	noQueueCalls(t, r)
}

// A repository listed under a different instance is not reviewed here.
func TestGitHubRepositoryOfAnotherInstanceIgnored(t *testing.T) {
	r := newGitHubRig(t)
	h := &GitHubHandler{
		Config: &config.Config{
			GitHub:       []config.GitHubInstance{{Name: "gh"}, {Name: "gh2"}},
			Repositories: []config.Repository{{GitHub: "gh2", Project: "acme/widgets"}},
		},
		Secrets: map[string]string{"gh": ghSecret}, Queue: r.q,
	}
	r.mux = http.NewServeMux()
	r.mux.Handle("/webhooks/github/{instance}", h)
	wantStatus(t, r.gh(fixture(t, "github_opened.json")), 200)
	noQueueCalls(t, r)
}

func TestGitHubRepositoryMatchIsCaseInsensitive(t *testing.T) {
	r := newGitHubRig(t)
	body := bytes.Replace(fixture(t, "github_opened.json"), []byte("acme/widgets"), []byte("ACME/WIDGETS"), 1)
	wantStatus(t, r.gh(body), 200)
	if len(r.q.enqueues) != 1 || r.q.enqueues[0].projectPath != "Acme/Widgets" {
		t.Fatalf("enqueues = %+v, want config spelling", r.q.enqueues)
	}
}

func TestGitHubStoreErrorIs500(t *testing.T) {
	for _, fx := range []string{"github_opened.json", "github_closed.json", "github_ready.json"} {
		r := newGitHubRig(t)
		r.q.err = errors.New("db down")
		wantStatus(t, r.gh(fixture(t, fx)), 500)
		if r.wakes != 0 {
			t.Fatal("woke on error")
		}
	}
}

func TestGitHubWakeOnlyWhenCreated(t *testing.T) {
	r := newGitHubRig(t)
	wantStatus(t, r.gh(fixture(t, "github_opened.json")), 200)
	if r.wakes != 1 {
		t.Fatalf("wakes = %d, want 1", r.wakes)
	}
	r.q.created = false
	wantStatus(t, r.gh(fixture(t, "github_opened.json")), 200)
	wantStatus(t, r.gh(fixture(t, "github_closed.json")), 200)
	if r.wakes != 1 {
		t.Fatalf("wakes = %d, want still 1", r.wakes)
	}
}

func TestGitHubNilWakeIsSafe(t *testing.T) {
	q := &fakeQueue{created: true}
	h := &GitHubHandler{
		Config: &config.Config{
			GitHub:       []config.GitHubInstance{{Name: "gh"}},
			Repositories: []config.Repository{{GitHub: "gh", Project: "acme/widgets"}},
		},
		Secrets: map[string]string{"gh": ghSecret}, Queue: q,
	}
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github/{instance}", h)
	body := fixture(t, "github_opened.json")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/gh", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(ghSecret, body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	wantStatus(t, rec, 200)
}

func TestGitHubMalformedJSONIs200(t *testing.T) {
	r := newGitHubRig(t)
	wantStatus(t, r.gh([]byte("{not json")), 200)
	noQueueCalls(t, r)
	if !strings.Contains(r.logs.String(), "level=WARN") || !strings.Contains(r.logs.String(), "malformed_json") {
		t.Errorf("not logged as a warning: %s", r.logs.String())
	}
}

func TestGitHubMissingIDsIs200(t *testing.T) {
	for name, body := range map[string][]byte{
		"repository id": bytes.Replace(fixture(t, "github_opened.json"), []byte(`"id": 42`), []byte(`"id": 0`), 1),
		"number": bytes.Replace(fixture(t, "github_opened.json"), []byte(`"number": 7,
    "state"`), []byte(`"number": 0,
    "state"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			r := newGitHubRig(t)
			wantStatus(t, r.gh(body), 200)
			noQueueCalls(t, r)
			if !strings.Contains(r.logs.String(), "level=WARN") || !strings.Contains(r.logs.String(), "missing_ids") {
				t.Errorf("not logged as a warning: %s", r.logs.String())
			}
		})
	}
}

func TestGitHubBadHeadSHAIs200(t *testing.T) {
	for name, head := range map[string]string{
		"empty":     "",
		"short":     "abc123",
		"uppercase": strings.Repeat("A", 40),
		"non-hex":   strings.Repeat("g", 40),
		"41 chars":  strings.Repeat("a", 41),
	} {
		t.Run(name, func(t *testing.T) {
			r := newGitHubRig(t)
			body := bytes.Replace(fixture(t, "github_opened.json"), []byte(openHead), []byte(head), 1)
			wantStatus(t, r.gh(body), 200)
			noQueueCalls(t, r)
			if !strings.Contains(r.logs.String(), "level=WARN") || !strings.Contains(r.logs.String(), "bad_head_sha") {
				t.Errorf("not logged as a warning: %s", r.logs.String())
			}
		})
	}
}

func TestGitHubSHA256HeadAccepted(t *testing.T) {
	r := newGitHubRig(t)
	head := strings.Repeat("ab", 32)
	body := bytes.Replace(fixture(t, "github_opened.json"), []byte(openHead), []byte(head), 1)
	wantStatus(t, r.gh(body), 200)
	if len(r.q.enqueues) != 1 || r.q.enqueues[0].head != head {
		t.Fatalf("enqueues = %+v", r.q.enqueues)
	}
}

func TestGitHubNeverLogsSecretOrBody(t *testing.T) {
	r := newGitHubRig(t)
	r.gh(fixture(t, "github_opened.json"))
	r.gh(fixture(t, "github_opened.json"), withSignature("sha256=deadbeefdeadbeef"))
	logs := r.logs.String()
	if logs == "" {
		t.Fatal("expected log output")
	}
	for _, bad := range []string{ghSecret, "deadbeefdeadbeef", "full_name", sign(ghSecret, fixture(t, "github_opened.json"))} {
		if strings.Contains(logs, bad) {
			t.Fatalf("log leaks %q: %s", bad, logs)
		}
	}
}
