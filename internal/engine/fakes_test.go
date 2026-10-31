package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge/gitlabforge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/secretbox"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// Values every engine test shares. None of them is a real secret.
const (
	testDBEnv = "PERGENT_SH_TEST_DATABASE_URL"

	testInstance    = "main"
	testProjectID   = int64(7)
	testProjectPath = "acme/widgets"
	testRepoURL     = "https://gitlab.example.com/acme/widgets.git"
	testNamespace   = "pergent-jobs"
	testImage       = "ghcr.io/gopaytech/pergent:v0.1.0"
	testCallbackURL = "https://pergent.example.com"

	testReviewToken  = "review-bot-token-example"
	testApproveToken = "approve-bot-token-example"
	testDeployUser   = "gitlab+deploy-token-1"
	testDeployToken  = "deploy-token-example"
	testLLMKey       = "llm-key-example"

	testWebhookSecret = "webhook-secret-example"

	// testApproveUserID is the approve bot's GitLab user id.
	testApproveUserID = int64(200)

	// testBoxKey is a fixed 32-byte key, standard base64.
	testBoxKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
)

// sha returns a 40-character hex SHA made of one repeated digit, e.g. sha('a').
func sha(c byte) string { return string(bytes.Repeat([]byte{c}, 40)) }

// ---------------------------------------------------------------------------
// Fake GitLab

// gitlabRequest is one request the fake GitLab received.
type gitlabRequest struct {
	Method, Path string // Path is relative to /api/v4 and still escaped
	Query        url.Values
	Token        string // the PRIVATE-TOKEN header
	Body         []byte
}

// JSON decodes the request body into a map.
func (r gitlabRequest) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("request %s %s body %q: %v", r.Method, r.Path, r.Body, err)
	}
	return m
}

// fakeGitLab is an httptest GitLab that routes on method and escaped path
// (relative to /api/v4) and records every request. An unrouted request gets
// 404, which the client reads as "missing" for files and trees. Use 4xx codes
// other than 404/429 for errors: 5xx and 429 make the client retry with
// real sleeps.
type fakeGitLab struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	routes   map[string]http.HandlerFunc
	requests []gitlabRequest
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{t: t, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the instance's base URL.
func (f *fakeGitLab) URL() string { return f.srv.URL }

// Client returns a client for the fake GitLab with the given token.
func (f *fakeGitLab) Client(token string) *gitlab.Client {
	return gitlab.New(f.srv.URL, token, f.srv.Client())
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4")
	f.mu.Lock()
	f.requests = append(f.requests, gitlabRequest{
		Method: r.Method, Path: p, Query: r.URL.Query(), Token: r.Header.Get("PRIVATE-TOKEN"), Body: body,
	})
	h := f.routes[r.Method+" "+p]
	f.mu.Unlock()
	if h == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"404 Not Found"}`)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	h(w, r)
}

// Handle routes method and path (relative to /api/v4, escaped as the client
// sends it) to h, replacing any earlier route.
func (f *fakeGitLab) Handle(method, path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+path] = h
}

// Reply routes method and path to a fixed response. A string or []byte body is
// sent as is; anything else is encoded as JSON.
func (f *fakeGitLab) Reply(method, path string, status int, body any) {
	var data []byte
	switch b := body.(type) {
	case nil:
	case string:
		data = []byte(b)
	case []byte:
		data = b
	default:
		var err error
		if data, err = json.Marshal(b); err != nil {
			f.t.Fatalf("encode fake response: %v", err)
		}
	}
	f.Handle(method, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(data)
	})
}

// Requests returns the recorded requests for method and path; "" matches any.
func (f *fakeGitLab) Requests(method, path string) []gitlabRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gitlabRequest
	for _, r := range f.requests {
		if (method == "" || r.Method == method) && (path == "" || r.Path == path) {
			out = append(out, r)
		}
	}
	return out
}

// Reset forgets the recorded requests (routes stay).
func (f *fakeGitLab) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

// Paths of the endpoints the engine uses.
func mrPath(iid int64) string {
	return "/projects/" + strconv.FormatInt(testProjectID, 10) + "/merge_requests/" + strconv.FormatInt(iid, 10)
}

func statusPath(sha string) string {
	return "/projects/" + strconv.FormatInt(testProjectID, 10) + "/statuses/" + sha
}

func treePath() string {
	return "/projects/" + strconv.FormatInt(testProjectID, 10) + "/repository/tree"
}

func filePath(file string) string {
	return "/projects/" + strconv.FormatInt(testProjectID, 10) + "/repository/files/" + url.PathEscape(file) + "/raw"
}

func notesPath(iid int64) string { return mrPath(iid) + "/notes" }

func notePath(iid, noteID int64) string {
	return notesPath(iid) + "/" + strconv.FormatInt(noteID, 10)
}

func approvePath(iid int64) string   { return mrPath(iid) + "/approve" }
func unapprovePath(iid int64) string { return mrPath(iid) + "/unapprove" }
func approvalsPath(iid int64) string { return mrPath(iid) + "/approvals" }

// SetMR serves mr at its merge request endpoint.
func (f *fakeGitLab) SetMR(mr gitlab.MergeRequest) {
	f.Reply(http.MethodGet, mrPath(mr.IID), http.StatusOK, mr)
}

// openMR is an opened merge request with all three diff refs.
func openMR(iid int64, head string) gitlab.MergeRequest {
	return gitlab.MergeRequest{
		IID: iid, State: "opened", SHA: head, TargetBranch: "main",
		DiffRefs: gitlab.DiffRefs{BaseSHA: sha('b'), HeadSHA: head, StartSHA: sha('5')},
	}
}

// ---------------------------------------------------------------------------
// Fake Kubernetes

// newFakeKube returns a fake clientset that gives every created Job a UID
// (as the API server would), and a Launcher on it in testNamespace.
func newFakeKube() (*fake.Clientset, *kube.Launcher) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		j := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		j.UID = types.UID("uid-" + j.Name)
		return false, nil, nil
	})
	return cs, &kube.Launcher{Client: cs, Namespace: testNamespace}
}

// ---------------------------------------------------------------------------
// Test environment

// testEnv is an Engine wired to a real store in a fresh schema, a fake GitLab
// and a fake clientset, with a clock the test moves.
type testEnv struct {
	t      *testing.T
	ctx    context.Context
	Store  *store.Store
	GitLab *fakeGitLab
	Kube   *fake.Clientset
	Box    *secretbox.Box
	Engine *Engine

	logs lockedWriter

	clockMu sync.Mutex
	now     time.Time
}

// newTestEnv builds the environment. It skips without a test database.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dbURL := os.Getenv(testDBEnv)
	if dbURL == "" {
		t.Skipf("%s is not set", testDBEnv)
	}
	ctx := context.Background()

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenInSchema(ctx, dbURL, "test_engine_"+hex.EncodeToString(b))
	if err != nil {
		t.Fatalf("OpenInSchema: %v", err)
	}
	t.Cleanup(func() {
		if err := st.DropSchema(context.Background()); err != nil {
			t.Errorf("DropSchema: %v", err)
		}
		st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	box, err := secretbox.New(testBoxKey)
	if err != nil {
		t.Fatal(err)
	}
	gl := newFakeGitLab(t)
	cs, launcher := newFakeKube()
	// The config's llm: block names this variable.
	t.Setenv("OPENCODE_API_KEY", testLLMKey)
	cfg := testConfig(gl.URL())
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config is invalid: %v", err)
	}

	env := &testEnv{
		t: t, ctx: ctx, Store: st, GitLab: gl, Kube: cs, Box: box,
		// Wall time, so the runs' created_at (set by the database) is "now".
		now: time.Now().UTC().Truncate(time.Second),
	}
	env.Engine = &Engine{
		Store:     st,
		Config:    cfg,
		Instances: instances.New(cfg, st, box),
		Platforms: syncmap.New(map[string]Platform{testInstance: {
			Kind:     config.PlatformGitLab,
			Forge:    gitlabforge.NewForge(gl.Client(testReviewToken), gitlab.User{ID: 100, Username: "pergent-review"}),
			Approver: gitlabforge.NewApprover(gl.Client(testApproveToken), gitlab.User{ID: testApproveUserID, Username: "pergent-approve"}),
			GitLab:   gl.Client(testReviewToken),
		}}),
		WebhookSecrets: syncmap.New(map[string]string{testInstance: testWebhookSecret}),
		Launcher:       launcher,
		Library:        inputs.Library{"security": []byte("---\nname: security\n---\nLook for security problems.\n")},
		Box:            box,
		Now:            env.Now,
		Log:            slog.New(slog.NewTextHandler(&env.logs, nil)),
	}
	env.SeedProject(nil)
	return env
}

// testConfig is a valid config with one instance "main" at gitlabURL and one
// repository acme/widgets on it.
func testConfig(gitlabURL string) *config.Config {
	return &config.Config{
		Listen:      ":8080",
		PublicURL:   "https://pergent.example.com",
		CallbackURL: testCallbackURL,
		SkillsDir:   "/skills",
		GitLab: []config.Instance{{
			Name: testInstance, URL: gitlabURL,
			ReviewTokenEnv: "GITLAB_REVIEW_TOKEN", ApproveTokenEnv: "GITLAB_APPROVE_TOKEN", WebhookSecretEnv: "GITLAB_WEBHOOK_SECRET",
		}},
		Repositories: []config.Repository{{
			GitLab: testInstance, Project: testProjectPath, FailureStatus: config.FailureStatusFailed,
		}},
		LLM: config.LLM{Provider: "anthropic", Model: "claude-sonnet-4", APIKeyEnv: "OPENCODE_API_KEY"},
		Jobs: config.Jobs{
			Namespace: testNamespace, Image: testImage, MaxRunning: 5,
			Timeout: 10 * time.Minute, Concurrency: 4,
		},
	}
}

// Now is the engine's clock.
func (e *testEnv) Now() time.Time {
	e.clockMu.Lock()
	defer e.clockMu.Unlock()
	return e.now
}

// SetNow moves the clock.
func (e *testEnv) SetNow(t time.Time) {
	e.clockMu.Lock()
	defer e.clockMu.Unlock()
	e.now = t
}

// SeedProject stores the acme/widgets project with a sealed deploy token.
// edit, when not nil, changes the row before it is stored.
func (e *testEnv) SeedProject(edit func(p *store.Project)) {
	e.t.Helper()
	p := store.Project{
		Instance: testInstance, ProjectID: testProjectID, Path: testProjectPath, HTTPURLToRepo: testRepoURL,
		HookID: 11, DeployTokenID: 12, DeployTokenUsername: testDeployUser,
		DeployTokenSealed:    e.Box.Seal([]byte(testDeployToken)),
		DeployTokenExpiresAt: e.Now().Add(30 * 24 * time.Hour),
	}
	if edit != nil {
		edit(&p)
	}
	if err := e.Store.UpsertProject(e.ctx, p); err != nil {
		e.t.Fatalf("UpsertProject: %v", err)
	}
}

// MRKey is the key of merge request iid of acme/widgets.
func (e *testEnv) MRKey(iid int64) store.MRKey {
	return store.MRKey{Instance: testInstance, ProjectID: testProjectID, IID: iid}
}

// Enqueue queues a run of head on merge request iid and returns its id.
func (e *testEnv) Enqueue(iid int64, head string) string {
	e.t.Helper()
	id, _, err := e.Store.EnqueueRun(e.ctx, e.MRKey(iid), testProjectPath, head, "main")
	if err != nil {
		e.t.Fatalf("EnqueueRun: %v", err)
	}
	return id
}

// Run reads a run back.
func (e *testEnv) Run(id string) store.Run {
	e.t.Helper()
	r, ok, err := e.Store.Run(e.ctx, id)
	if err != nil || !ok {
		e.t.Fatalf("Run(%s) = %v, %v", id, ok, err)
	}
	return r
}

// Job returns the Job of run id, or nil when there is none.
func (e *testEnv) Job(id string) *batchv1.Job {
	e.t.Helper()
	j, err := e.Kube.BatchV1().Jobs(testNamespace).Get(e.ctx, kube.JobName(id), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return j
}

// Jobs returns every Job in the namespace.
func (e *testEnv) Jobs() []batchv1.Job {
	e.t.Helper()
	l, err := e.Kube.BatchV1().Jobs(testNamespace).List(e.ctx, metav1.ListOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

// Secret returns the Secret of run id's Job.
func (e *testEnv) Secret(id string) *corev1.Secret {
	e.t.Helper()
	s, err := e.Kube.CoreV1().Secrets(testNamespace).Get(e.ctx, kube.JobName(id)+"-secrets", metav1.GetOptions{})
	if err != nil {
		e.t.Fatalf("get secret: %v", err)
	}
	return s
}

// ConfigMap returns the inputs ConfigMap of run id's Job.
func (e *testEnv) ConfigMap(id string) *corev1.ConfigMap {
	e.t.Helper()
	c, err := e.Kube.CoreV1().ConfigMaps(testNamespace).Get(e.ctx, kube.JobName(id)+"-inputs", metav1.GetOptions{})
	if err != nil {
		e.t.Fatalf("get configmap: %v", err)
	}
	return c
}

// Dispatched moves run id of merge request iid, head, to running as if the
// dispatcher had started its Job (named kube.JobName(id), base sha('b'),
// start sha('5'), result token "seed-token"), without the engine or a Job.
// It claims every queued run, so queue other runs after it.
func (e *testEnv) Dispatched(iid int64, head string) string {
	e.t.Helper()
	id := e.Enqueue(iid, head)
	if _, err := e.Store.ClaimQueued(e.ctx, 100, e.Now().Add(time.Hour)); err != nil {
		e.t.Fatal(err)
	}
	h := hashToken("seed-token")
	if err := e.Store.MarkDispatched(e.ctx, id, kube.JobName(id), sha('b'), sha('5'), h, e.Now().Add(time.Hour), e.Now()); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// Report moves run id of merge request iid, head, through dispatch to
// reported with result stored, so the publisher picks it up.
func (e *testEnv) Report(iid int64, head string, result []byte) string {
	e.t.Helper()
	id := e.Dispatched(iid, head)
	if out, err := e.Store.StoreResult(e.ctx, id, "seed-token", result, e.Now()); err != nil || out != store.ResultStored {
		e.t.Fatalf("StoreResult = %v, %v", out, err)
	}
	return id
}

// Publish moves run id of merge request iid, head, through dispatch and a
// stored result to published, without the engine, so it can serve as the
// previous review.
func (e *testEnv) Publish(iid int64, head string, result []byte) string {
	e.t.Helper()
	id := e.Report(iid, head, result)
	if err := e.Store.MarkPublished(e.ctx, id, store.RunDecision{}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// Logs returns what the engine has logged so far.
func (e *testEnv) Logs() string {
	e.logs.mu.Lock()
	defer e.logs.mu.Unlock()
	return e.logs.buf.String()
}

// lockedWriter is a log buffer safe for concurrent writes.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// decodeToken checks a result token is 32 random bytes in base64url without
// padding.
func decodeToken(t *testing.T, tok string) {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(b) != 32 {
		t.Errorf("result token %q is not 32 bytes of base64url without padding (%d bytes, %v)", tok, len(b), err)
	}
}
