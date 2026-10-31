package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/forge/gitlabforge"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

// opConnector is the binary's connector for the tests: it connects an
// instance of the dashboard to the test env's fake GitLab, which stands for
// every instance, in the maps the engine, the webhooks and the dashboard
// read.
type opConnector struct {
	env      *testEnv
	sources  *syncmap.Map[dashboard.RepoSource]
	catalogs *syncmap.Map[dashboard.RepoCatalog]
	auth     *dashboard.Auth

	mu          sync.Mutex
	disconnects []string
}

func (c *opConnector) Check(context.Context, instances.Instance, instances.Secrets) error { return nil }

func (c *opConnector) Connect(_ context.Context, inst instances.Instance, s instances.Secrets) error {
	gl := c.env.GitLab
	plat := Platform{
		Kind:     config.PlatformGitLab,
		Forge:    gitlabforge.NewForge(gl.Client(s.ReviewToken), gitlab.User{ID: 100, Username: "pergent-review"}),
		Approver: gitlabforge.NewApprover(gl.Client(s.ApproveToken), gitlab.User{ID: testApproveUserID, Username: "pergent-approve"}),
		GitLab:   gl.Client(s.ReviewToken),
	}
	c.env.Engine.Platforms.Set(inst.Name, plat)
	c.env.Engine.WebhookSecrets.Set(inst.Name, s.WebhookSecret)
	c.sources.Set(inst.Name, plat.Forge.(dashboard.RepoSource))
	c.catalogs.Set(inst.Name, plat.Forge.(dashboard.RepoCatalog))
	c.auth.SetProvider(inst, s)
	return nil
}

func (c *opConnector) Disconnect(name string) {
	c.mu.Lock()
	c.disconnects = append(c.disconnects, name)
	c.mu.Unlock()
	c.env.Engine.Platforms.Delete(name)
	c.env.Engine.WebhookSecrets.Delete(name)
	c.sources.Delete(name)
	c.catalogs.Delete(name)
	c.auth.RemoveProvider(name)
}

// newOperatorE2E is the dashboard e2e with a connector, and the engine told
// to forget the instances the connector lets go.
func newOperatorE2E(t *testing.T) (*dashE2E, *opConnector) {
	t.Helper()
	conn := &opConnector{}
	e := newDashboardE2E(t, func(o *dashboard.Options) {
		conn.sources, conn.catalogs, o.Connector = o.Sources, o.Catalogs, conn
	})
	conn.env, conn.auth = e.env, e.dash.Auth
	e.env.Engine.OnInstanceRemoved = conn.Disconnect
	return e, conn
}

// randomSession stores a session of account and returns its cookie.
func randomSession(t *testing.T, env *testEnv, st store.Session) dashUser {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	st.IDHash, st.CSRF, st.LastSeenAt, st.CreatedAt = hash[:], "csrf-token-example", env.Now(), env.Now()
	if err := env.Store.CreateSession(env.ctx, st); err != nil {
		t.Fatal(err)
	}
	return dashUser{session: &http.Cookie{Name: dashboard.SessionCookieName, Value: base64.RawURLEncoding.EncodeToString(raw)},
		csrf: st.CSRF}
}

// operator stores a password account and returns its session.
func (e *dashE2E) operator(t *testing.T) dashUser {
	t.Helper()
	acc, err := e.env.Store.CreatePasswordAccount(e.env.ctx, "ops", "stored-hash-example", false, e.env.Now())
	if err != nil {
		t.Fatal(err)
	}
	u := randomSession(t, e.env, store.Session{AccountID: acc.ID, Username: "ops"})
	u.isAdmin = true
	return u
}

const (
	labName    = "lab"
	labProject = gadgetsPath
)

// addLab adds the instance "lab" through the API, on the env's fake GitLab.
func (e *dashE2E) addLab(t *testing.T, ops dashUser) {
	t.Helper()
	status, body := e.write(t, ops, http.MethodPost, "/api/admin/instances", map[string]any{
		"name": labName, "platform": "gitlab", "url": e.env.GitLab.URL(),
		"secrets": map[string]string{"review_token": testReviewToken, "approve_token": testApproveToken},
	})
	if status != http.StatusCreated {
		t.Fatalf("adding lab = %d: %s", status, body)
	}
}

// A removed instance: its queued runs are superseded, its users are signed
// out, the engine's reconcile takes the webhook of its repository off the
// platform and then finishes the removal, so the registry has no trace of it,
// its secrets are gone and its webhooks are unknown.
func TestRemoveInstanceWithQueuedRuns(t *testing.T) {
	e, conn := newOperatorE2E(t)
	env := e.env
	gl := env.GitLab
	ops := e.operator(t)
	e.addLab(t, ops)

	// A repository switched on at lab is set up by the engine.
	if status, body := e.write(t, ops, http.MethodPost, "/api/repositories", map[string]string{"instance": labName, "project": labProject}); status != http.StatusCreated {
		t.Fatalf("switch on at lab = %d: %s", status, body)
	}
	if err := env.Engine.ReconcileRepos(env.ctx, env.Engine.takeSoonKeys()); err != nil {
		t.Fatalf("ReconcileRepos: %v", err)
	}
	if p, ok, _ := env.Store.Project(env.ctx, labName, gadgetsID); !ok || p.HookID != newHookID {
		t.Fatalf("lab's project = %+v, %v; want set up with its webhook", p, ok)
	}

	// A queued run, a user signed in through lab, and the webhook that works.
	runID, _, err := env.Store.EnqueueRun(env.ctx, store.MRKey{Instance: labName, ProjectID: gadgetsID, IID: 1}, labProject, sha('a'), "main")
	if err != nil {
		t.Fatal(err)
	}
	acc, err := env.Store.UpsertPlatformAccount(env.ctx, config.PlatformGitLab, labName, 77, "carol", env.Now())
	if err != nil {
		t.Fatal(err)
	}
	carol := randomSession(t, env, store.Session{AccountID: acc.ID, Platform: config.PlatformGitLab, Instance: labName,
		UserID: 77, Username: "carol", TokenSealed: []byte("sealed")})
	hookStatus := func() int {
		status, _, _, _ := e.do(t, http.MethodPost, "/webhooks/gitlab/"+labName, json.RawMessage(`{}`), nil,
			map[string]string{"X-Gitlab-Token": "wrong", "X-Gitlab-Event": "Merge Request Hook"})
		return status
	}
	if status := hookStatus(); status != http.StatusUnauthorized {
		t.Fatalf("webhook to lab before the removal = %d, want 401 (known, wrong secret)", status)
	}

	gl.Reply(http.MethodDelete, gadgetsAPI("/hooks/"+strconv.Itoa(int(newHookID))), http.StatusNoContent, nil)
	gl.Reply(http.MethodDelete, gadgetsAPI("/deploy_tokens/"+strconv.Itoa(int(newDeployTokenID))), http.StatusNoContent, nil)
	if status, body := e.write(t, ops, http.MethodDelete, "/api/admin/instances/"+labName, nil); status != http.StatusAccepted {
		t.Fatalf("DELETE lab = %d: %s", status, body)
	}

	if r := env.Run(runID); r.State != store.Superseded {
		t.Errorf("the queued run = %s, want superseded", r.State)
	}
	if status, _, _, _ := e.do(t, http.MethodGet, "/api/me", nil, []*http.Cookie{carol.session}, nil); status != http.StatusUnauthorized {
		t.Errorf("carol's next request = %d, want 401", status)
	}
	if accounts, _ := env.Store.Accounts(env.ctx); len(accounts) != 1 || accounts[0].Username != "ops" {
		t.Errorf("accounts = %+v, want ops only", accounts)
	}
	// Superseded, so dispatch has nothing to do for it.
	dispatch(t, env)
	if len(env.Jobs()) != 0 {
		t.Errorf("a job was launched for a removed instance")
	}

	// Not finished yet: lab is still registered, removing.
	if inst, ok := env.Engine.Instances.Get(labName); !ok || !inst.Removing {
		t.Fatalf("lab = %+v, %v; want registered and removing until the engine is done", inst, ok)
	}
	if len(conn.disconnects) != 0 {
		t.Fatalf("disconnected before the engine was done: %v", conn.disconnects)
	}

	// The engine's reconcile takes the webhook off and finishes.
	keys := env.Engine.takeSoonKeys()
	if len(keys) != 1 || keys[0].Instance != labName || !strings.EqualFold(keys[0].Project, labProject) {
		t.Fatalf("reconcile asked for %+v, want lab's repository", keys)
	}
	if err := env.Engine.ReconcileRepos(env.ctx, keys); err != nil {
		t.Fatalf("ReconcileRepos: %v", err)
	}
	if n := len(gl.Requests(http.MethodDelete, gadgetsAPI("/hooks/"+strconv.Itoa(int(newHookID))))); n != 1 {
		t.Errorf("webhook deletions = %d, want 1", n)
	}
	if n := len(gl.Requests(http.MethodDelete, gadgetsAPI("/deploy_tokens/"+strconv.Itoa(int(newDeployTokenID))))); n != 1 {
		t.Errorf("deploy token revocations = %d, want 1", n)
	}
	if _, ok, _ := env.Store.Project(env.ctx, labName, gadgetsID); ok {
		t.Error("lab's project row is still stored")
	}
	if _, ok := env.Engine.Instances.Get(labName); ok {
		t.Error("the registry still has lab")
	}
	if _, found, _ := env.Store.InstanceRow(env.ctx, labName); found {
		t.Error("the instance row is still stored")
	}
	if sec, _ := env.Store.InstanceSecrets(env.ctx, labName); len(sec) != 0 {
		t.Errorf("secrets left: %d", len(sec))
	}
	if len(conn.disconnects) != 1 || conn.disconnects[0] != labName {
		t.Errorf("disconnects = %v, want lab", conn.disconnects)
	}
	if status := hookStatus(); status != http.StatusNotFound {
		t.Errorf("webhook to lab after the removal = %d, want 404", status)
	}
	// The config instance is untouched.
	if _, ok := env.Engine.Instances.Get(testInstance); !ok {
		t.Error("main is gone")
	}
	if _, ok := env.Engine.Platforms.Get(testInstance); !ok {
		t.Error("main was disconnected")
	}
}

// An operator sets the LLM in the dashboard and the engine uses it from its
// next dispatch: the Job's Secret holds the new key, its env the new model.
// Until then nothing is dispatched.
func TestLLMEditable(t *testing.T) {
	e, _ := newOperatorE2E(t)
	env := e.env
	env.Engine.Config.LLM = config.LLM{} // the LLM is the dashboard's
	ops := e.operator(t)

	head := sha('a')
	first := env.Enqueue(3, head)
	env.GitLab.SetMR(openMR(3, head))
	env.GitLab.Reply(http.MethodPost, statusPath(head), http.StatusCreated, `{}`)
	dispatch(t, env)
	if r := env.Run(first); r.State != store.Queued {
		t.Fatalf("run without an LLM = %s, want queued", r.State)
	}

	put := func(model, key string) {
		t.Helper()
		body := map[string]string{"provider": "anthropic", "model": model, "baseURL": "https://llm.example.com/v1"}
		if key != "" {
			body["apiKey"] = key
		}
		if status, out := e.write(t, ops, http.MethodPut, "/api/admin/llm", body); status != http.StatusOK {
			t.Fatalf("PUT /api/admin/llm = %d: %s", status, out)
		}
	}
	jobEnv := func(id string) map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, v := range env.Job(id).Spec.Template.Spec.Containers[0].Env {
			out[v.Name] = v.Value
		}
		return out
	}

	put("claude-model-one", "llm-key-one-example")
	dispatch(t, env)
	if env.Job(first) == nil {
		t.Fatalf("no job after the LLM was set:\n%s", env.Logs())
	}
	if got := string(env.Secret(first).Data["llm-api-key"]); got != "llm-key-one-example" {
		t.Errorf("Job key = %q, want the one set", got)
	}
	if got := jobEnv(first); got["OPENCODE_MODEL"] != "claude-model-one" || got["OPENCODE_PROVIDER"] != "anthropic" ||
		got["OPENCODE_BASE_URL"] != "https://llm.example.com/v1" {
		t.Errorf("Job LLM env = %v", got)
	}

	// Changed: the next run takes the new model and key.
	put("claude-model-two", "llm-key-two-example")
	head2 := sha('c')
	second := env.Enqueue(4, head2)
	env.GitLab.SetMR(openMR(4, head2))
	env.GitLab.Reply(http.MethodPost, statusPath(head2), http.StatusCreated, `{}`)
	dispatch(t, env)
	if env.Job(second) == nil {
		t.Fatalf("no job for the second run:\n%s", env.Logs())
	}
	if got := string(env.Secret(second).Data["llm-api-key"]); got != "llm-key-two-example" {
		t.Errorf("second Job key = %q, want the new one", got)
	}
	if got := jobEnv(second); got["OPENCODE_MODEL"] != "claude-model-two" {
		t.Errorf("second Job model = %q", got["OPENCODE_MODEL"])
	}
	// The keys never reach the engine's log, and both are redacted.
	logs := env.Logs()
	if strings.Contains(logs, "llm-key-one-example") || strings.Contains(logs, "llm-key-two-example") {
		t.Errorf("a key reached the log:\n%s", logs)
	}
	if got := env.Engine.redact("a llm-key-one-example b llm-key-two-example"); got != "a *** b ***" {
		t.Errorf("redact = %q", got)
	}
}
