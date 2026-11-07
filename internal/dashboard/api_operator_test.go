package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
	"github.com/gopaytech/pergent.sh/internal/webhook"
)

// Distinctive values for the secrets the tests enter: none may ever come
// back in a response, a log line or the audit log.
const (
	labReviewToken  = "lab-review-token-SECRET-111"
	labApproveToken = "lab-approve-token-SECRET-222"
	labOAuthSecret  = "lab-oauth-client-secret-SECRET-333"
	hubReviewKey    = "-----BEGIN RSA PRIVATE KEY-----\nhub-review-key-SECRET-444\n-----END RSA PRIVATE KEY-----"
	hubApproveKey   = "-----BEGIN RSA PRIVATE KEY-----\nhub-approve-key-SECRET-555\n-----END RSA PRIVATE KEY-----"
	hubWebhook      = "hub-webhook-secret-SECRET-666"
	llmKeyOne       = "llm-api-key-SECRET-777"
	llmKeyTwo       = "llm-api-key-SECRET-888"
)

// fakeConnector stands for the binary's connector: it records what the API
// asks of it and, on Connect, does what the real one does for the maps the
// dashboard and the webhooks read.
type fakeConnector struct {
	mu       sync.Mutex
	auth     *Auth
	sources  *syncmap.Map[RepoSource]
	catalogs *syncmap.Map[RepoCatalog]
	hooks    *syncmap.Map[string] // the webhook secret of each instance connected
	checkErr func(inst instances.Instance, s instances.Secrets) error
	// connectErr fails Connect alone: a platform that went away after
	// its check passed.
	connectErr  func(inst instances.Instance, s instances.Secrets) error
	checks      []connectCall
	connects    []connectCall
	disconnects []string
}

type connectCall struct {
	inst instances.Instance
	s    instances.Secrets
}

func newFakeConnector(sources *syncmap.Map[RepoSource], catalogs *syncmap.Map[RepoCatalog]) *fakeConnector {
	return &fakeConnector{sources: sources, catalogs: catalogs, hooks: syncmap.New[string](nil)}
}

func (c *fakeConnector) Check(_ context.Context, inst instances.Instance, s instances.Secrets) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks = append(c.checks, connectCall{inst, s})
	if c.checkErr != nil {
		return c.checkErr(inst, s)
	}
	return nil
}

func (c *fakeConnector) Connect(_ context.Context, inst instances.Instance, s instances.Secrets) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checkErr != nil {
		if err := c.checkErr(inst, s); err != nil {
			return err
		}
	}
	if c.connectErr != nil {
		if err := c.connectErr(inst, s); err != nil {
			return err
		}
	}
	c.connects = append(c.connects, connectCall{inst, s})
	c.hooks.Set(inst.Name, s.WebhookSecret)
	if _, ok := c.sources.Get(inst.Name); !ok {
		c.sources.Set(inst.Name, newFakeSource())
		c.catalogs.Set(inst.Name, &fakeCatalog{})
	}
	c.auth.SetProvider(inst, s)
	return nil
}

func (c *fakeConnector) Disconnect(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnects = append(c.disconnects, name)
	c.hooks.Delete(name)
	c.sources.Delete(name)
	c.catalogs.Delete(name)
	c.auth.RemoveProvider(name)
}

func (c *fakeConnector) connectCalls() []connectCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.connects)
}

func (c *fakeConnector) checkCalls() []connectCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.checks)
}

// operator signs the password admin "ops" in.
func (e *env) operator() *http.Cookie {
	e.t.Helper()
	e.addPasswordAccount("ops", operatorPassword, false)
	return e.signInPassword("ops", operatorPassword)
}

// json sends body as JSON with the operator's CSRF token.
func (e *env) sendJSON(c *http.Cookie, method, target string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.send(c, method, target, string(data))
}

// labBody is the body that adds the GitLab instance "lab".
func labBody(edit func(m map[string]any)) map[string]any {
	m := map[string]any{
		"name": "lab", "platform": "gitlab", "url": "https://gitlab.lab.example.com", "oauthClientID": "lab-client",
		"secrets": map[string]string{"review_token": labReviewToken, "approve_token": labApproveToken, "oauth_client_secret": labOAuthSecret},
	}
	if edit != nil {
		edit(m)
	}
	return m
}

// hubBody is the body that adds the GitHub instance "hub" with an approve App.
func hubBody(edit func(m map[string]any)) map[string]any {
	m := map[string]any{
		"name": "hub", "platform": "github", "apiURL": "https://github.hub.example.com/api/v3", "reviewAppID": 11, "approveAppID": 12,
		"secrets": map[string]string{"review_app_key": hubReviewKey, "approve_app_key": hubApproveKey, "webhook_secret": hubWebhook},
	}
	if edit != nil {
		edit(m)
	}
	return m
}

func (e *env) addLab(c *http.Cookie) InstanceAdminView {
	e.t.Helper()
	return decode[InstanceAdminView](e.t, e.sendJSON(c, "POST", "/api/admin/instances", labBody(nil)), "POST /api/admin/instances", http.StatusCreated)
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	return v.Error
}

// Adding a GitLab instance checks it, stores its secrets sealed, generates
// its webhook secret, registers and connects it; its webhooks are accepted
// with the generated secret and nothing else; and no secret comes back.
func TestAddGitLabInstance(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()

	view := e.addLab(c)
	if view.Name != "lab" || view.Platform != "gitlab" || view.Source != "dashboard" || view.Removing ||
		view.URL != "https://gitlab.lab.example.com" || view.Approver != "token" || view.OAuthClientID != "lab-client" {
		t.Fatalf("view = %+v", view)
	}
	for _, f := range []string{"review_token", "approve_token", "webhook_secret", "oauth_client_secret"} {
		if sv, ok := view.Secrets[f]; !ok || !sv.Set || sv.UpdatedAt == nil {
			t.Errorf("secrets[%s] = %+v, want set with a time", f, sv)
		}
	}

	// Checked with the secrets sent and the generated webhook secret, then
	// connected with the same.
	checks, connects := e.conn.checkCalls(), e.conn.connectCalls()
	if len(checks) != 1 || len(connects) != 1 {
		t.Fatalf("checks %d, connects %d; want one each", len(checks), len(connects))
	}
	s := connects[0].s
	if s.ReviewToken != labReviewToken || s.ApproveToken != labApproveToken || s.OAuthClientSecret != labOAuthSecret ||
		!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s.WebhookSecret) || checks[0].s != s {
		t.Errorf("connected with %+v; want the secrets sent and a generated 32-byte hex webhook secret", s)
	}
	if connects[0].inst.GitLab == nil || connects[0].inst.GitLab.URL != "https://gitlab.lab.example.com" {
		t.Errorf("connected instance = %+v", connects[0].inst)
	}

	// Registered, stored, with the secrets sealed.
	if inst, ok := e.reg.Get("lab"); !ok || inst.Source != instances.SourceDashboard || inst.OAuthClientID != "lab-client" {
		t.Errorf("registry lab = %+v, %v", inst, ok)
	}
	got, _, err := e.reg.Secrets(ctx, "lab")
	if err != nil || got != s {
		t.Errorf("stored secrets = %+v, %v; want %+v", got, err, s)
	}
	sealed, _ := e.st.InstanceSecrets(ctx, "lab")
	for field, sec := range sealed {
		if strings.Contains(string(sec.Value), labReviewToken) || strings.Contains(string(sec.Value), s.WebhookSecret) {
			t.Errorf("secret %s is stored in the clear", field)
		}
	}

	// The list shows it, after the config's.
	list := decode[[]InstanceAdminView](t, e.get(c, "/api/admin/instances"), "GET /api/admin/instances", http.StatusOK)
	var names []string
	for _, v := range list {
		names = append(names, v.Source+"/"+v.Name)
	}
	if !slices.Equal(names, []string{"config/gh", "config/main", "dashboard/lab"}) {
		t.Errorf("instances = %v", names)
	}
	if sv := list[2].Secrets["webhook_secret"]; !sv.Set {
		t.Errorf("GET shows webhook_secret %+v, want set", sv)
	}
	if v := list[1]; v.Source != "config" || v.URL != e.gl.srv.URL || v.Secrets["review_token"].Set || !v.Secrets["oauth_client_secret"].Set ||
		v.OAuthClientID != glClientID {
		t.Errorf("config instance main = %+v", v)
	}

	// Sign-in through it works as soon as it is added.
	signin := decode[instancesView](t, e.do("GET", "/api/instances", nil, nil), "GET /api/instances", http.StatusOK)
	if !slices.Contains(signin.Instances, InstanceView{Platform: "gitlab", Instance: "lab", Label: "GitLab (lab)"}) {
		t.Errorf("sign-in instances = %+v, want lab", signin.Instances)
	}

	// Its webhooks: refused with a wrong secret, accepted with the
	// generated one, and the pull request is queued.
	if rec := e.sendJSON(c, "POST", "/api/admin/instances", labBody(nil)); rec.Code != http.StatusConflict {
		t.Errorf("adding lab again = %d %s, want 409", rec.Code, rec.Body)
	}
	cat, _ := e.catalogs.Get("lab")
	cat.(*fakeCatalog).set("acme/gadgets")
	if rec := e.sendJSON(c, "POST", "/api/repositories", map[string]string{"instance": "lab", "project": "acme/gadgets"}); rec.Code != http.StatusCreated {
		t.Fatalf("switching a repository on at lab = %d %s", rec.Code, rec.Body)
	}
	q := &hookQueue{}
	h := &webhook.Handler{Config: e.cfg, Instances: e.reg, Secrets: e.conn.hooks, Queue: q, Repos: e.d.Repos}
	post := func(instance, secret string) int {
		body := `{"object_kind":"merge_request","project":{"id":4,"path_with_namespace":"acme/gadgets"},` +
			`"object_attributes":{"iid":3,"action":"open","target_branch":"main","last_commit":{"id":"` + strings.Repeat("a", 40) + `"}}}`
		req := httptest.NewRequest("POST", "/webhooks/gitlab/"+instance, strings.NewReader(body))
		req.SetPathValue("instance", instance)
		req.Header.Set("X-Gitlab-Token", secret)
		req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("lab", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("webhook with a wrong secret = %d, want 401", code)
	}
	if code := post("lab", s.WebhookSecret); code != http.StatusOK || len(q.enqueued) != 1 || q.enqueued[0].Instance != "lab" {
		t.Errorf("webhook with the generated secret = %d, queued %+v; want 200 and a run queued at lab", code, q.enqueued)
	}

	entries := e.auditEntries()
	if i := slices.IndexFunc(entries, func(a store.AuditEntry) bool { return a.Action == "instance_added" }); i < 0 ||
		entries[i].Instance != "lab" || entries[i].Actor != "ops (password)" {
		t.Errorf("audit = %+v, want instance_added by ops at lab", entries)
	}
}

// hookQueue is the queue a webhook handler writes to.
type hookQueue struct{ enqueued []store.MRKey }

func (q *hookQueue) EnqueueRun(_ context.Context, mr store.MRKey, _, _, _ string) (string, bool, error) {
	q.enqueued = append(q.enqueued, mr)
	return "run", true, nil
}
func (q *hookQueue) SupersedeMR(context.Context, store.MRKey) (int, error) { return 0, nil }
func (q *hookQueue) RequestFates(context.Context, store.MRKey, string, bool, time.Time) error {
	return nil
}
func (q *hookQueue) SupersedeDraftMR(context.Context, store.MRKey, string, string) (int, error) {
	return 0, nil
}
func (q *hookQueue) Republish(context.Context, store.MRKey, string, string) (bool, error) {
	return true, nil
}

// An instance's webhook URL comes from public_url, its platform and its name.
func TestInstanceViewWebhookURL(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PublicURL = "https://pergent.example.com" })
	view := e.addLab(e.operator())
	if want := "https://pergent.example.com/webhooks/gitlab/lab"; view.WebhookURL != want {
		t.Errorf("WebhookURL = %q, want %q", view.WebhookURL, want)
	}
}

// A GitHub instance is added with its Apps' keys and the webhook secret the
// operator entered; the approve App shows as "app".
func TestAddGitHubInstance(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	view := decode[InstanceAdminView](t, e.sendJSON(c, "POST", "/api/admin/instances", hubBody(nil)), "POST", http.StatusCreated)
	if view.Platform != "github" || view.APIURL != "https://github.hub.example.com/api/v3" || view.ReviewAppID != 11 ||
		view.ApproveAppID != 12 || view.Approver != "app" || view.URL != "" {
		t.Errorf("view = %+v", view)
	}
	for _, f := range []string{"review_app_key", "approve_app_key", "webhook_secret"} {
		if !view.Secrets[f].Set {
			t.Errorf("secrets[%s] not set", f)
		}
	}
	if sv, ok := view.Secrets["approve_token"]; ok {
		t.Errorf("an instance approving with an App lists approve_token: %+v", sv)
	}
	if got := e.conn.connectCalls()[0].s; got.WebhookSecret != hubWebhook || got.ReviewAppKey != hubReviewKey || got.ApproveAppKey != hubApproveKey {
		t.Errorf("connected with %+v", got)
	}

	// With a bot user's token instead of an approve App.
	tok := hubBody(func(m map[string]any) {
		m["name"], m["approveAppID"] = "hub2", 0
		m["secrets"] = map[string]string{"review_app_key": hubReviewKey, "approve_token": labApproveToken, "webhook_secret": hubWebhook}
	})
	view = decode[InstanceAdminView](t, e.sendJSON(c, "POST", "/api/admin/instances", tok), "POST", http.StatusCreated)
	if view.Approver != "token" || !view.Secrets["approve_token"].Set || view.ApproveAppID != 0 {
		t.Errorf("token approver view = %+v", view)
	}
}

// An approve App ID equal to the review App's is the review App approving:
// "review_app", the review App's ID as approveAppID, and neither approve
// secret taken, checked or connected.
func TestAddGitHubInstanceReviewAppApproves(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	body := hubBody(func(m map[string]any) {
		m["approveAppID"] = 11
		m["secrets"] = map[string]string{"review_app_key": hubReviewKey, "webhook_secret": hubWebhook}
	})
	view := decode[InstanceAdminView](t, e.sendJSON(c, "POST", "/api/admin/instances", body), "POST", http.StatusCreated)
	if view.Approver != "review_app" || view.ReviewAppID != 11 || view.ApproveAppID != 11 {
		t.Errorf("view = %+v", view)
	}
	for _, f := range []string{"review_app_key", "webhook_secret", "oauth_client_secret"} {
		if _, ok := view.Secrets[f]; !ok {
			t.Errorf("secrets lacks %s: %+v", f, view.Secrets)
		}
	}
	for _, f := range []string{"approve_app_key", "approve_token"} {
		if sv, ok := view.Secrets[f]; ok {
			t.Errorf("a review App approver lists %s: %+v", f, sv)
		}
	}
	checks, connects := e.conn.checkCalls(), e.conn.connectCalls()
	if len(checks) != 1 || len(connects) != 1 {
		t.Fatalf("checks %d, connects %d; want one each", len(checks), len(connects))
	}
	want := instances.Secrets{ReviewAppKey: hubReviewKey, WebhookSecret: hubWebhook}
	if checks[0].s != want || connects[0].s != want {
		t.Errorf("checked %+v, connected %+v; want %+v", checks[0].s, connects[0].s, want)
	}
	if gh := checks[0].inst.GitHub; gh == nil || gh.Approver() != config.ApproverReviewApp || gh.ApproveApp != nil {
		t.Errorf("checked instance = %+v", gh)
	}
	rows, _ := e.st.Instances(ctx)
	if len(rows) != 1 || rows[0].ReviewAppID != 11 || rows[0].ApproveAppID != 11 {
		t.Errorf("stored rows = %+v", rows)
	}

	// Updating: the key can change, a second approver's secret can't be
	// added, and the approve App ID can't move off the review App's.
	rec := e.sendJSON(c, "PUT", "/api/admin/instances/hub", map[string]any{"approveAppID": 11, "reviewAppID": 11,
		"secrets": map[string]string{"review_app_key": "rotated-review-key"}})
	upd := decode[InstanceAdminView](t, rec, "PUT", http.StatusOK)
	if upd.Approver != "review_app" || !upd.Secrets["review_app_key"].Set {
		t.Errorf("updated view = %+v", upd)
	}
	if cs := e.conn.connectCalls(); cs[len(cs)-1].s != (instances.Secrets{ReviewAppKey: "rotated-review-key", WebhookSecret: hubWebhook}) {
		t.Errorf("connected after the update with %+v", cs[len(cs)-1].s)
	}
	for _, f := range []string{"approve_app_key", "approve_token"} {
		rec = e.sendJSON(c, "PUT", "/api/admin/instances/hub", map[string]any{"secrets": map[string]string{f: "x"}})
		if rec.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, rec), f+" is not a secret") {
			t.Errorf("PUT %s = %d %s; want 400", f, rec.Code, rec.Body)
		}
	}
	for _, id := range []int64{0, 12} {
		rec = e.sendJSON(c, "PUT", "/api/admin/instances/hub", map[string]any{"approveAppID": id})
		if rec.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, rec), "can't change") {
			t.Errorf("PUT approveAppID %d = %d %s; want 400 immutable", id, rec.Code, rec.Body)
		}
	}
}

// An instance of the config whose review App approves lists "review_app"
// and takes no approve secret.
func TestConfigInstanceReviewAppApprover(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.GitHub[0].ReviewApp = config.App{AppID: 5, PrivateKeyEnv: "PERGENT_TEST_OWN_KEY"}
		c.GitHub[0].ApproveWithReviewApp = true
	})
	c := e.operator()
	list := decode[[]InstanceAdminView](t, e.get(c, "/api/admin/instances"), "GET", http.StatusOK)
	v := list[0]
	if v.Name != "gh" || v.Approver != "review_app" || v.ReviewAppID != 5 || v.ApproveAppID != 5 {
		t.Fatalf("gh = %+v", v)
	}
	for _, f := range []string{"approve_app_key", "approve_token"} {
		if _, ok := v.Secrets[f]; ok {
			t.Errorf("gh lists %s", f)
		}
	}
}

// A failed check is a 400 with its reason, and nothing is stored, registered
// or connected; a secret the reason echoes is hidden.
func TestAddInstanceCheckFails(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.conn.checkErr = func(inst instances.Instance, s instances.Secrets) error {
		return errors.New(`gitlab instance "lab": the review token and the approve token belong to the same user; the approve bot must be a separate user (` + s.ReviewToken + `)`)
	}
	rec := e.sendJSON(c, "POST", "/api/admin/instances", labBody(nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d %s, want 400", rec.Code, rec.Body)
	}
	want := `gitlab instance "lab": the review token and the approve token belong to the same user; the approve bot must be a separate user ([REDACTED])`
	if got := errorOf(t, rec); got != want {
		t.Errorf("error = %q\nwant %q", got, want)
	}
	if rows, _ := e.st.Instances(ctx); len(rows) != 0 {
		t.Errorf("stored instances = %+v, want none", rows)
	}
	if _, ok := e.reg.Get("lab"); ok {
		t.Error("lab is registered")
	}
	if n := len(e.conn.connectCalls()); n != 0 {
		t.Errorf("connected %d times", n)
	}
	if strings.Contains(e.logs.String(), labReviewToken) {
		t.Errorf("the token reached the log:\n%s", e.logs.String())
	}
	if slices.ContainsFunc(e.auditEntries(), func(a store.AuditEntry) bool { return a.Action == "instance_added" }) {
		t.Error("a failed add was audited as added")
	}
}

// What the operator sends is checked before anything reaches the platform.
func TestAddInstanceValidation(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	secrets := func(m map[string]string) func(map[string]any) {
		return func(b map[string]any) { b["secrets"] = m }
	}
	full := map[string]string{"review_token": "r", "approve_token": "a"}
	for _, tc := range []struct {
		name   string
		body   map[string]any
		status int
		want   string // a part of the message
	}{
		{"bad name", labBody(func(m map[string]any) { m["name"] = "Lab_1" }), 400, "lowercase letters"},
		{"empty name", labBody(func(m map[string]any) { m["name"] = "" }), 400, "lowercase letters"},
		{"bad platform", labBody(func(m map[string]any) { m["platform"] = "gitea" }), 400, `"gitlab" or "github"`},
		{"config name", labBody(func(m map[string]any) { m["name"] = "main" }), 409, "exists already"},
		{"config name on the other platform", hubBody(func(m map[string]any) { m["name"] = "main" }), 409, "exists already"},
		{"http url", labBody(func(m map[string]any) { m["url"] = "http://gitlab.lab.example.com" }), 400, "https address"},
		{"trailing slash", labBody(func(m map[string]any) { m["url"] = "https://gitlab.lab.example.com/" }), 400, "trailing slash"},
		{"url with credentials", labBody(func(m map[string]any) { m["url"] = "https://u:p@gitlab.lab.example.com" }), 400, "https address"},
		{"gitlab with an app", labBody(func(m map[string]any) { m["reviewAppID"] = 3 }), 400, "takes a url"},
		{"github with a url", hubBody(func(m map[string]any) { m["url"] = "https://x.example.com" }), 400, "not url"},
		{"github negative approve app", hubBody(func(m map[string]any) { m["approveAppID"] = -1 }), 400, "the reviewAppID (the review App approves)"},
		{"review app approver with an approve App key", hubBody(func(m map[string]any) {
			m["approveAppID"] = 11
			m["secrets"] = map[string]string{"review_app_key": "k", "approve_app_key": "k2", "webhook_secret": "w"}
		}), 400, "approve_app_key is not a secret"},
		{"review app approver with an approve token", hubBody(func(m map[string]any) {
			m["approveAppID"] = 11
			m["secrets"] = map[string]string{"review_app_key": "k", "approve_token": "t", "webhook_secret": "w"}
		}), 400, "approve_token is not a secret"},
		{"review app approver without its key", hubBody(func(m map[string]any) {
			m["approveAppID"] = 11
			m["secrets"] = map[string]string{"webhook_secret": "w"}
		}), 400, "review_app_key is required"},
		{"token instance without its token", hubBody(func(m map[string]any) {
			m["approveAppID"] = 0
			m["secrets"] = map[string]string{"review_app_key": "k", "webhook_secret": "w"}
		}), 400, "approve_token is required"},
		{"github without an app", hubBody(func(m map[string]any) { m["reviewAppID"] = 0 }), 400, "reviewAppID must be positive"},
		{"gitlab webhook secret", labBody(func(m map[string]any) {
			m["secrets"] = map[string]string{"review_token": "r", "approve_token": "a", "webhook_secret": "mine", "oauth_client_secret": "o"}
		}), 400, "generated"},
		{"empty secret", labBody(secrets(map[string]string{"review_token": "  ", "approve_token": "a", "oauth_client_secret": "o"})), 400, "review_token must not be empty"},
		{"missing secret", labBody(secrets(map[string]string{"review_token": "r", "oauth_client_secret": "o"})), 400, "approve_token is required"},
		{"unknown secret", labBody(secrets(map[string]string{"review_token": "r", "approve_token": "a", "oauth_client_secret": "o", "api_key": "k"})), 400, "api_key is not a secret"},
		{"app key on a token instance", hubBody(func(m map[string]any) {
			m["approveAppID"] = 0
			m["secrets"] = map[string]string{"review_app_key": "k", "approve_app_key": "k2", "webhook_secret": "w"}
		}), 400, "approve_app_key is not a secret"},
		{"github without a webhook secret", hubBody(func(m map[string]any) {
			m["secrets"] = map[string]string{"review_app_key": "k", "approve_app_key": "k2"}
		}), 400, "webhook_secret is required"},
		{"client id without a secret", labBody(secrets(full)), 400, "go together"},
		{"client secret without an id", labBody(func(m map[string]any) {
			m["oauthClientID"] = ""
		}), 400, "go together"},
		{"client id with a space", labBody(func(m map[string]any) { m["oauthClientID"] = "a b" }), 400, "without spaces"},
		{"unknown field", labBody(func(m map[string]any) { m["token"] = "x" }), 400, "the body must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.sendJSON(c, "POST", "/api/admin/instances", tc.body)
			if rec.Code != tc.status || !strings.Contains(errorOf(t, rec), tc.want) {
				t.Errorf("POST = %d %s; want %d with %q", rec.Code, rec.Body, tc.status, tc.want)
			}
		})
	}
	if n := len(e.conn.checkCalls()); n != 0 {
		t.Errorf("%d checks reached the platform for bodies that were refused", n)
	}
	if rows, _ := e.st.Instances(ctx); len(rows) != 0 {
		t.Errorf("stored instances = %+v, want none", rows)
	}
}

// Secrets and the OAuth client ID can change, checked as a whole with the
// secrets already stored; the address and App IDs can't.
func TestUpdateInstance(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	before := e.addLab(c)
	hook := e.conn.connectCalls()[0].s.WebhookSecret

	rec := e.sendJSON(c, "PUT", "/api/admin/instances/lab", map[string]any{
		"oauthClientID": "lab-client-2", "secrets": map[string]string{"review_token": "new-review-token-SECRET-999"},
	})
	view := decode[InstanceAdminView](t, rec, "PUT", http.StatusOK)
	if view.OAuthClientID != "lab-client-2" || !view.Secrets["review_token"].Set {
		t.Errorf("view = %+v", view)
	}
	if view.Secrets["review_token"].UpdatedAt.Before(*before.Secrets["review_token"].UpdatedAt) {
		t.Errorf("updatedAt went back")
	}
	if !view.Secrets["approve_token"].UpdatedAt.Equal(*before.Secrets["approve_token"].UpdatedAt) {
		t.Errorf("a secret that wasn't sent changed its updatedAt")
	}
	// Checked and connected with the new token and the stored rest.
	want := instances.Secrets{ReviewToken: "new-review-token-SECRET-999", ApproveToken: labApproveToken, WebhookSecret: hook, OAuthClientSecret: labOAuthSecret}
	checks, connects := e.conn.checkCalls(), e.conn.connectCalls()
	if len(checks) != 2 || checks[1].s != want || checks[1].inst.OAuthClientID != "lab-client-2" {
		t.Errorf("check = %+v; want the merged secrets", checks)
	}
	if len(connects) != 2 || connects[1].s != want {
		t.Errorf("connect = %+v; want the merged secrets", connects)
	}
	if got, _, _ := e.reg.Secrets(ctx, "lab"); got != want {
		t.Errorf("stored secrets = %+v", got)
	}
	if inst, _ := e.reg.Get("lab"); inst.OAuthClientID != "lab-client-2" {
		t.Errorf("registry client ID = %q", inst.OAuthClientID)
	}
	entries := e.auditEntries()
	if i := slices.IndexFunc(entries, func(a store.AuditEntry) bool { return a.Action == "instance_updated" }); i < 0 ||
		entries[i].Detail != "oauth_client_id, review_token" || entries[i].Instance != "lab" {
		t.Errorf("audit = %+v; want instance_updated naming the fields", entries)
	}

	// A check that fails keeps everything as it was.
	e.conn.checkErr = func(instances.Instance, instances.Secrets) error { return errors.New("the approve token doesn't work") }
	rec = e.sendJSON(c, "PUT", "/api/admin/instances/lab", map[string]any{"secrets": map[string]string{"approve_token": "bad"}})
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != "the approve token doesn't work" {
		t.Errorf("PUT with a failing check = %d %s", rec.Code, rec.Body)
	}
	if got, _, _ := e.reg.Secrets(ctx, "lab"); got != want {
		t.Errorf("secrets after a failed check = %+v", got)
	}
	e.conn.checkErr = nil

	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"nothing", map[string]any{}, "nothing to change"},
		{"webhook secret", map[string]any{"secrets": map[string]string{"webhook_secret": "x"}}, "generated"},
		{"empty secret", map[string]any{"secrets": map[string]string{"review_token": ""}}, "must not be empty"},
	} {
		rec := e.sendJSON(c, "PUT", "/api/admin/instances/lab", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, rec), tc.want) {
			t.Errorf("%s: PUT = %d %s; want 400 with %q", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
	// Clearing the client ID alone takes sign-in away.
	decode[InstanceAdminView](t, e.sendJSON(c, "PUT", "/api/admin/instances/lab", map[string]any{"oauthClientID": ""}), "PUT", http.StatusOK)
	signin := decode[instancesView](t, e.do("GET", "/api/instances", nil, nil), "GET /api/instances", http.StatusOK)
	if slices.ContainsFunc(signin.Instances, func(v InstanceView) bool { return v.Instance == "lab" }) {
		t.Errorf("sign-in is still offered through lab: %+v", signin.Instances)
	}
	if rec := e.sendJSON(c, "PUT", "/api/admin/instances/nope", map[string]any{"oauthClientID": "x"}); rec.Code != http.StatusNotFound {
		t.Errorf("PUT of an unknown instance = %d, want 404", rec.Code)
	}
}

func TestUpdateInstanceImmutable(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.addLab(c)
	e.sendJSON(c, "POST", "/api/admin/instances", hubBody(nil))
	const want = "an instance's address and App IDs can't change: remove it and add it again"
	for _, tc := range []struct {
		instance string
		body     map[string]any
	}{
		{"lab", map[string]any{"url": "https://elsewhere.example.com"}},
		{"hub", map[string]any{"apiURL": "https://elsewhere.example.com/api/v3"}},
		{"hub", map[string]any{"reviewAppID": 99}},
		{"hub", map[string]any{"approveAppID": 0}},
	} {
		rec := e.sendJSON(c, "PUT", "/api/admin/instances/"+tc.instance, tc.body)
		if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != `{"error":"`+want+`"}` {
			t.Errorf("PUT %s %v = %d %s; want 400 %s", tc.instance, tc.body, rec.Code, rec.Body, want)
		}
	}
	// Naming the same address is not a change.
	rec := e.sendJSON(c, "PUT", "/api/admin/instances/lab", map[string]any{"url": "https://gitlab.lab.example.com", "oauthClientID": "same-id-again"})
	if rec.Code != http.StatusOK {
		t.Errorf("PUT with the current url = %d %s, want 200", rec.Code, rec.Body)
	}
	if rows, _ := e.st.Instances(ctx); len(rows) != 2 || rows[0].URL != "https://github.hub.example.com/api/v3" && rows[1].URL != "https://gitlab.lab.example.com" {
		t.Errorf("stored rows = %+v", rows)
	}
}

// The config's instances can be listed but not changed or removed.
func TestConfigInstanceReadOnly(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	for _, name := range []string{"main", "gh"} {
		for _, req := range []struct{ method, body string }{
			{"PUT", `{"oauthClientID":"x"}`}, {"DELETE", ""},
		} {
			rec := e.send(c, req.method, "/api/admin/instances/"+name, req.body)
			if rec.Code != http.StatusConflict || !strings.Contains(errorOf(t, rec), "config file") {
				t.Errorf("%s %s = %d %s, want 409 naming the config file", req.method, name, rec.Code, rec.Body)
			}
		}
	}
	if n := len(e.conn.checkCalls()) + len(e.conn.disconnects); n != 0 {
		t.Errorf("the connector was used %d times for config instances", n)
	}
	list := decode[[]InstanceAdminView](t, e.get(c, "/api/admin/instances"), "GET", http.StatusOK)
	if len(list) != 2 || list[0].Name != "gh" || list[0].Source != "config" || list[1].Platform != "gitlab" {
		t.Errorf("instances = %+v", list)
	}
}

// Platform admins, and everyone else, can't touch the instances; admins can
// read the LLM and the defaults, only operators write the LLM.
func TestPlatformAdminCannotTouchInstances(t *testing.T) {
	e := newEnv(t, nil)
	op := e.operator()
	e.addLab(op)
	admin := e.signInGitLab(true, "acme/widgets")

	for _, tc := range []struct{ method, target, body string }{
		{"GET", "/api/admin/instances", ""},
		{"POST", "/api/admin/instances", `{}`},
		{"PUT", "/api/admin/instances/lab", `{}`},
		{"DELETE", "/api/admin/instances/lab", ""},
		{"GET", "/api/admin/instances/lab/repositories", ""},
		{"PUT", "/api/admin/llm", `{}`},
	} {
		var rec *httptest.ResponseRecorder
		if tc.method == "GET" {
			rec = e.get(admin, tc.target)
		} else {
			rec = e.send(admin, tc.method, tc.target, tc.body)
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a platform admin = %d %s, want 403", tc.method, tc.target, rec.Code, rec.Body)
		}
	}
	if _, found, _ := e.st.InstanceRow(ctx, "lab"); !found {
		t.Error("lab was removed by a platform admin")
	}
	for _, target := range []string{"/api/admin/llm", "/api/admin/defaults"} {
		if rec := e.get(admin, target); rec.Code != http.StatusOK {
			t.Errorf("GET %s as a platform admin = %d, want 200", target, rec.Code)
		}
	}
	// alice on gh is on no team: a waiting account is no admin.
	waiting := e.signIn("github", "gh")
	for _, target := range []string{"/api/admin/instances", "/api/admin/llm", "/api/admin/defaults"} {
		if rec := e.get(waiting, target); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as a non-admin = %d, want 403", target, rec.Code)
		}
	}
	if rec := e.get(nil, "/api/admin/instances"); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET without a session = %d, want 401", rec.Code)
	}
	// A write needs the CSRF token.
	req := httptest.NewRequest("DELETE", "/api/admin/instances/lab", nil)
	req.AddCookie(op)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("DELETE without a CSRF token = %d, want 403", rec.Code)
	}
}

// labSession makes a platform account on lab with a stored session and
// returns its cookie.
func (e *env) labSession(userID int64, name string) *http.Cookie {
	e.t.Helper()
	acc, err := e.st.UpsertPlatformAccount(ctx, config.PlatformGitLab, "lab", userID, name, e.clock.now())
	if err != nil {
		e.t.Fatal(err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		e.t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	if err := e.st.CreateSession(ctx, store.Session{IDHash: hash[:], AccountID: acc.ID, Platform: config.PlatformGitLab,
		Instance: "lab", UserID: userID, Username: name, TokenSealed: []byte("sealed"), CSRF: "csrf",
		LastSeenAt: e.clock.now(), CreatedAt: e.clock.now()}); err != nil {
		e.t.Fatal(err)
	}
	return &http.Cookie{Name: SessionCookieName, Value: base64.RawURLEncoding.EncodeToString(raw)}
}

// Removing an instance supersedes its runs, deletes its accounts, sessions
// and repositories, stops sign-in through it and asks the engine to finish;
// it is idempotent, and the instance stays registered, removing, until the
// engine deletes it.
func TestRemoveInstance(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.addLab(c)
	cat, _ := e.catalogs.Get("lab")
	cat.(*fakeCatalog).set("acme/gadgets", "acme/tools")
	for _, p := range []string{"acme/gadgets", "acme/tools"} {
		if rec := e.sendJSON(c, "POST", "/api/repositories", map[string]string{"instance": "lab", "project": p}); rec.Code != http.StatusCreated {
			t.Fatalf("switch on %s = %d %s", p, rec.Code, rec.Body)
		}
	}
	e.dashboardRepo("main", "acme/other", nil)
	runID, _, err := e.st.EnqueueRun(ctx, store.MRKey{Instance: "lab", ProjectID: 4, IID: 1}, "acme/gadgets", strings.Repeat("a", 40), "main")
	if err != nil {
		t.Fatal(err)
	}
	otherRun, _, _ := e.st.EnqueueRun(ctx, store.MRKey{Instance: "main", ProjectID: 4, IID: 1}, "acme/widgets", strings.Repeat("b", 40), "main")
	user := e.labSession(31, "carol")
	if code, _ := e.me(user); code != http.StatusOK {
		t.Fatalf("carol's session before the removal = %d", code)
	}
	team := e.team("lab devs", store.RoleViewer, []int64{e.accountOf(user)}, store.TeamRepo{Instance: "lab"})
	e.reconciled = nil

	rec := e.send(c, "DELETE", "/api/admin/instances/lab", "")
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != `{"status":"removing"}` {
		t.Fatalf("DELETE = %d %s, want 202 removing", rec.Code, rec.Body)
	}

	if r, _, _ := e.st.Run(ctx, runID); r.State != store.Superseded {
		t.Errorf("the queued run of lab = %s, want superseded", r.State)
	}
	if r, _, _ := e.st.Run(ctx, otherRun); r.State != store.Queued {
		t.Errorf("a run of another instance = %s, want queued", r.State)
	}
	if code, _ := e.me(user); code != http.StatusUnauthorized {
		t.Errorf("carol's next request = %d, want 401", code)
	}
	if repos, _ := e.st.DashboardRepos(ctx); len(repos) != 1 || repos[0].Instance != "main" {
		t.Errorf("dashboard repositories = %+v, want main's only", repos)
	}
	if tm := e.storedTeam(team); len(tm.Repos) != 0 || len(tm.Members) != 0 {
		t.Errorf("team after the removal = %+v, want no lab repositories or members", tm)
	}
	e.mu.Lock()
	reconciled := slices.Clone(e.reconciled)
	e.mu.Unlock()
	slices.Sort(reconciled)
	if !slices.Equal(reconciled, []string{"lab/acme/gadgets", "lab/acme/tools"}) {
		t.Errorf("reconcile asked for %v, want both repositories of lab", reconciled)
	}

	// Still there, removing, until the engine is done; nobody signs in.
	inst, ok := e.reg.Get("lab")
	if !ok || !inst.Removing {
		t.Errorf("registry lab = %+v, %v; want removing", inst, ok)
	}
	if row, found, _ := e.st.InstanceRow(ctx, "lab"); !found || !row.Removing {
		t.Errorf("stored lab = %+v, %v; want removing", row, found)
	}
	if sec, _ := e.st.InstanceSecrets(ctx, "lab"); len(sec) == 0 {
		t.Error("secrets were deleted before the engine was done")
	}
	if code := e.do("GET", "/auth/gitlab/lab/login", nil, nil).Code; code != http.StatusNotFound {
		t.Errorf("login through lab = %d, want 404", code)
	}
	signin := decode[instancesView](t, e.do("GET", "/api/instances", nil, nil), "GET /api/instances", http.StatusOK)
	if slices.ContainsFunc(signin.Instances, func(v InstanceView) bool { return v.Instance == "lab" }) {
		t.Errorf("sign-in instances = %+v, want no lab", signin.Instances)
	}
	list := decode[[]InstanceAdminView](t, e.get(c, "/api/admin/instances"), "GET", http.StatusOK)
	if len(list) != 3 || !list[2].Removing {
		t.Errorf("instances = %+v, want lab removing", list)
	}
	for _, tc := range []struct{ method, target, body string }{
		{"PUT", "/api/admin/instances/lab", `{"oauthClientID":"x"}`},
	} {
		if rec := e.send(c, tc.method, tc.target, tc.body); rec.Code != http.StatusConflict {
			t.Errorf("%s %s of a removing instance = %d, want 409", tc.method, tc.target, rec.Code)
		}
	}
	if rec := e.sendJSON(c, "POST", "/api/repositories", map[string]string{"instance": "lab", "project": "acme/gadgets"}); rec.Code != http.StatusNotFound {
		t.Errorf("switching a repository on at a removing instance = %d, want 404", rec.Code)
	}
	if rec := e.sendJSON(c, "POST", "/api/admin/instances", labBody(nil)); rec.Code != http.StatusConflict {
		t.Errorf("adding lab again while it is removing = %d, want 409", rec.Code)
	}
	if i := slices.IndexFunc(e.auditEntries(), func(a store.AuditEntry) bool { return a.Action == "instance_removed" }); i < 0 {
		t.Error("no instance_removed audit entry")
	}

	// Again is fine, and an unknown instance is 404.
	if rec := e.send(c, "DELETE", "/api/admin/instances/lab", ""); rec.Code != http.StatusAccepted {
		t.Errorf("second DELETE = %d, want 202", rec.Code)
	}
	if rec := e.send(c, "DELETE", "/api/admin/instances/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE of an unknown instance = %d, want 404", rec.Code)
	}
}

// An instance with no repository still gets a pass of the engine, which is
// what finishes the removal.
func TestRemoveInstanceWithoutRepositoriesAsksForAPass(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.addLab(c)
	e.reconciled = nil
	if rec := e.send(c, "DELETE", "/api/admin/instances/lab", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !slices.Equal(e.reconciled, []string{"lab/"}) {
		t.Errorf("reconcile asked for %v, want a pass naming lab", e.reconciled)
	}
}

// Nobody signs in through an instance being removed, even with a sign-in
// that began before.
func TestRemovingInstanceRefusesSignIn(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.addLab(c)
	loc, sc := e.startLogin("gitlab", "lab")
	if loc.Query().Get("state") == "" {
		t.Fatal("no state")
	}
	e.send(c, "DELETE", "/api/admin/instances/lab", "")
	rec := e.callback("gitlab", "lab", goodCode, loc.Query().Get("state"), sc)
	if rec.Code != http.StatusNotFound {
		t.Errorf("callback of a sign-in begun before the removal = %d, want 404", rec.Code)
	}
	if accounts, _ := e.st.Accounts(ctx); slices.ContainsFunc(accounts, func(a store.Account) bool { return a.Instance == "lab" }) {
		t.Error("an account was made on a removing instance")
	}
	// Even when the provider is still installed (a connect racing the removal).
	inst, _ := e.reg.Get("lab")
	e.d.Auth.SetProvider(inst, instances.Secrets{OAuthClientSecret: "x"})
	if code := e.do("GET", "/auth/gitlab/lab/login", nil, nil).Code; code != http.StatusNotFound {
		t.Errorf("login with a provider left behind = %d, want 404", code)
	}
}

// The LLM is edited by operators when the config has none: the key is
// needed the first time only, and is never sent back.
func TestLLMEditable(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	v := getJSON[LLMAdminView](e, c, "/api/admin/llm")
	if v.Source != "dashboard" || !v.Editable || v.Key.Set || v.Provider != "" {
		t.Errorf("empty LLM view = %+v", v)
	}
	put := func(body map[string]any) *httptest.ResponseRecorder {
		return e.sendJSON(c, "PUT", "/api/admin/llm", body)
	}

	if rec := put(map[string]any{"provider": "anthropic", "model": "claude-sonnet-4"}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(errorOf(t, rec), "apiKey is required") {
		t.Errorf("first PUT without a key = %d %s, want 400", rec.Code, rec.Body)
	}
	rec := put(map[string]any{"provider": "anthropic", "model": "claude-sonnet-4", "baseURL": "https://llm.example.com/v1", "apiKey": llmKeyOne})
	v = decode[LLMAdminView](t, rec, "PUT /api/admin/llm", http.StatusOK)
	if v.Source != "dashboard" || v.Provider != "anthropic" || v.Model != "claude-sonnet-4" || v.BaseURL != "https://llm.example.com/v1" ||
		!v.Key.Set || v.Key.UpdatedAt == nil || !v.Editable {
		t.Errorf("view after PUT = %+v", v)
	}
	llm, key, src, ok := e.reg.LLM()
	if !ok || key != llmKeyOne || llm.Model != "claude-sonnet-4" || src != instances.SourceDashboard {
		t.Errorf("registry LLM = %+v, %q, %s, %v", llm, key, src, ok)
	}

	// Without a key the stored one stays; a new key replaces it.
	v = decode[LLMAdminView](t, put(map[string]any{"provider": "anthropic", "model": "claude-opus-4", "baseURL": ""}), "PUT", http.StatusOK)
	if v.Model != "claude-opus-4" || v.BaseURL != "" || !v.Key.Set {
		t.Errorf("view after PUT without a key = %+v", v)
	}
	if _, key, _, _ := e.reg.LLM(); key != llmKeyOne {
		t.Errorf("the key changed to %q", key)
	}
	decode[LLMAdminView](t, put(map[string]any{"provider": "anthropic", "model": "claude-opus-4", "apiKey": llmKeyTwo}), "PUT", http.StatusOK)
	if _, key, _, _ := e.reg.LLM(); key != llmKeyTwo {
		t.Errorf("the key = %q, want the new one", key)
	}
	if got := e.reg.LLMKeys(); !slices.Contains(got, llmKeyOne) || !slices.Contains(got, llmKeyTwo) {
		t.Errorf("LLMKeys = %v, want both for redaction", got)
	}

	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "llm_updated" {
			details = append(details, a.Detail)
		}
	}
	slices.Reverse(details)
	if !slices.Equal(details, []string{"provider, model, base_url, api_key", "model, base_url", "api_key"}) {
		t.Errorf("llm_updated details = %q", details)
	}

	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"no provider", map[string]any{"provider": " ", "model": "m"}, "provider and model"},
		{"no model", map[string]any{"provider": "p"}, "provider and model"},
		{"relative base url", map[string]any{"provider": "p", "model": "m", "baseURL": "llm.example.com"}, "baseURL"},
		{"ftp base url", map[string]any{"provider": "p", "model": "m", "baseURL": "ftp://llm.example.com"}, "baseURL"},
		{"base url with credentials", map[string]any{"provider": "p", "model": "m", "baseURL": "https://u:p@llm.example.com"}, "baseURL"},
		{"empty key", map[string]any{"provider": "p", "model": "m", "apiKey": " "}, "apiKey must not be empty"},
		{"unknown field", map[string]any{"provider": "p", "model": "m", "key": "k"}, "the body must be"},
	} {
		if rec := put(tc.body); rec.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, rec), tc.want) {
			t.Errorf("%s: PUT = %d %s; want 400 with %q", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
	if llm, _, _, _ := e.reg.LLM(); llm.Provider != "anthropic" {
		t.Errorf("a refused PUT changed the LLM: %+v", llm)
	}
}

// An llm: block in the config wins: the dashboard shows it read-only.
func TestLLMConfigWins(t *testing.T) {
	t.Setenv("PERGENT_TEST_LLM_KEY", llmKeyOne)
	e := newEnv(t, func(cfg *config.Config) {
		cfg.LLM = config.LLM{Provider: "anthropic", Model: "from-config", APIKeyEnv: "PERGENT_TEST_LLM_KEY"}
	})
	c := e.operator()
	v := getJSON[LLMAdminView](e, c, "/api/admin/llm")
	if v.Source != "config" || v.Editable || v.Model != "from-config" || !v.Key.Set || v.Key.UpdatedAt != nil {
		t.Errorf("view = %+v", v)
	}
	rec := e.sendJSON(c, "PUT", "/api/admin/llm", map[string]any{"provider": "p", "model": "m", "apiKey": "k"})
	if rec.Code != http.StatusConflict || !strings.Contains(errorOf(t, rec), "config file") {
		t.Errorf("PUT = %d %s, want 409", rec.Code, rec.Body)
	}
	if rows, found, _ := e.st.LLM(ctx); found {
		t.Errorf("the LLM was stored despite the config's: %+v", rows)
	}
	// A platform admin sees it too, not editable either.
	admin := e.signInGitLab(true, "acme/widgets")
	if v := getJSON[LLMAdminView](e, admin, "/api/admin/llm"); v.Editable || v.Model != "from-config" {
		t.Errorf("platform admin's view = %+v", v)
	}
	// The older settings view agrees.
	if s := getJSON[AdminSettingsView](e, admin, "/api/admin/settings"); s.LLM.Model != "from-config" {
		t.Errorf("settings LLM = %+v", s.LLM)
	}
}

// sameDefaults compares two defaults views, an empty skill list being the
// same as none.
func sameDefaults(a, b DefaultsView) bool {
	if len(a.Skills) == 0 {
		a.Skills = nil
	}
	if len(b.Skills) == 0 {
		b.Skills = nil
	}
	// What the config gives, what is pinned and the comment layout have their
	// own tests.
	a.Config, a.Pinned, b.Config, b.Pinned = DefaultValues{}, nil, DefaultValues{}, nil
	a.CommentLayout, b.CommentLayout = nil, nil
	return reflect.DeepEqual(a, b)
}

// The view says what the config gives and which defaults were saved here, so
// the form can show the first and keep the rest following it.
func TestDefaultsViewConfigAndPinned(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Jobs.Timeout = 30 * time.Minute })
	admin := e.signInGitLab(true, "acme/widgets")

	// Nothing saved: the effective values are the config's, nothing is pinned.
	v := getJSON[DefaultsView](e, admin, "/api/admin/defaults")
	if v.JobTimeoutSeconds != 1800 || v.Config.JobTimeoutSeconds != 1800 || v.Pinned["jobTimeoutSeconds"] {
		t.Errorf("nothing saved: view = %+v", v)
	}

	// Saved: the value saved wins, the config's value is still shown.
	if err := e.st.PutRepoDefaults(ctx, store.RepoDefaults{JobTimeoutSeconds: ptr(600)}); err != nil {
		t.Fatal(err)
	}
	e.d.inputs.invalidate()
	v = getJSON[DefaultsView](e, admin, "/api/admin/defaults")
	if v.JobTimeoutSeconds != 600 || v.Config.JobTimeoutSeconds != 1800 || !v.Pinned["jobTimeoutSeconds"] || v.Pinned["maxTurns"] {
		t.Errorf("saved: view = %+v; want 600 pinned, config 1800, maxTurns not pinned", v)
	}
	if v.Config.MaxTurns != 40 || v.Config.FailureStatus != "failed" || !v.Config.Approve || v.Config.RerunLimitPerMR != 10 || v.Config.JobMemory != "" {
		t.Errorf("config values = %+v", v.Config)
	}
	for _, k := range []string{"approve", "reviewDrafts", "inlineFindings", "commandsAuthorAndMaintainersOnly", "incrementalReviews", "learnFromDismissals", "quietReviews", "commentLayout", "failureStatus", "jobTimeoutSeconds", "maxTurns", "jobMemory", "rerunLimitPerMR", "rerunLimitPerRepo"} {
		if _, ok := v.Pinned[k]; !ok {
			t.Errorf("pinned has no %q: %v", k, v.Pinned)
		}
	}

	// A PUT that sends null puts the field back on the config.
	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"jobTimeoutSeconds": nil}), "PUT", http.StatusOK)
	if v.JobTimeoutSeconds != 1800 || v.Pinned["jobTimeoutSeconds"] {
		t.Errorf("after null: view = %+v", v)
	}
}

// The default step limit is stored with the other defaults, and null clears
// it back to the built-in one.
func TestDefaultsMaxTurns(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")

	v := decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"maxTurns": 80}), "PUT", http.StatusOK)
	if v.MaxTurns != 80 {
		t.Errorf("maxTurns after PUT 80 = %d", v.MaxTurns)
	}
	if stored, err := e.st.RepoDefaults(ctx); err != nil || stored.MaxTurns == nil || *stored.MaxTurns != 80 {
		t.Errorf("stored defaults = %+v, %v; want maxTurns 80", stored, err)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || g.MaxTurns != 80 {
		t.Errorf("dashboard repository = %+v, %v; want 80", g, err)
	}

	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"maxTurns": nil}), "PUT null", http.StatusOK)
	if v.MaxTurns != 40 {
		t.Errorf("maxTurns after PUT null = %d, want the built-in 40", v.MaxTurns)
	}
	if stored, err := e.st.RepoDefaults(ctx); err != nil || stored.MaxTurns != nil {
		t.Errorf("stored defaults = %+v, %v; want maxTurns cleared", stored, err)
	}
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || g.MaxTurns != 40 {
		t.Errorf("dashboard repository = %+v, %v; want 40", g, err)
	}
}

// The defaults are shown and changed by admins, with the bounds and the
// messages of a repository's settings; they apply to the repositories
// switched on in the dashboard, not to the config's.
func TestDefaults(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")
	v := getJSON[DefaultsView](e, admin, "/api/admin/defaults")
	if want := (DefaultsView{Approve: true, ReviewDrafts: true, InlineFindings: true, CommandsAuthorAndMaintainersOnly: true, IncrementalReviews: true, LearnFromDismissals: true, FailureStatus: "failed", JobTimeoutSeconds: 600, MaxTurns: 40, RerunLimitPerMR: 10, RerunLimitPerRepo: 50}); !sameDefaults(v, want) {
		t.Errorf("defaults = %+v, want %+v", v, want)
	}

	rec := e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{
		"approve": false, "failureStatus": "neutral", "jobTimeoutSeconds": 900, "maxTurns": 80, "jobMemory": "2Gi",
		"rerunLimitPerMR": 20, "rerunLimitPerRepo": 200})
	v = decode[DefaultsView](t, rec, "PUT /api/admin/defaults", http.StatusOK)
	if want := (DefaultsView{Approve: false, ReviewDrafts: true, InlineFindings: true, CommandsAuthorAndMaintainersOnly: true, IncrementalReviews: true, LearnFromDismissals: true, FailureStatus: "neutral", JobTimeoutSeconds: 900, MaxTurns: 80, JobMemory: "2Gi",
		RerunLimitPerMR: 20, RerunLimitPerRepo: 200}); !sameDefaults(v, want) {
		t.Errorf("defaults after PUT = %+v, want %+v", v, want)
	}
	if got := getJSON[DefaultsView](e, admin, "/api/admin/defaults"); !sameDefaults(got, v) {
		t.Errorf("GET after PUT = %+v", got)
	}

	// A dashboard repository takes them; a config repository doesn't.
	e.dashboardRepo("main", "acme/gadgets", nil)
	g, ok, err := e.d.Repos.Get(ctx, "main", "acme/gadgets")
	if err != nil || !ok || g.Approve || g.FailureStatus != config.FailureStatusNeutral || g.JobTimeout.Seconds() != 900 || g.MaxTurns != 80 ||
		g.RerunLimitPerMR != 20 || g.RerunLimitPerRepo != 200 {
		t.Errorf("dashboard repository = %+v, %v, %v", g, ok, err)
	}
	if w, _, _ := e.d.Repos.Get(ctx, "main", "acme/widgets"); !w.Approve || w.FailureStatus != config.FailureStatusFailed || w.JobTimeout.Seconds() != 600 || w.MaxTurns != 40 ||
		w.RerunLimitPerMR != 10 || w.RerunLimitPerRepo != 50 {
		t.Errorf("config repository = %+v, want the config's defaults", w)
	}

	// The body replaces them all: what it leaves out is the built-in default.
	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"reviewDrafts": false, "jobMemory": nil}),
		"PUT", http.StatusOK)
	if want := (DefaultsView{Approve: true, ReviewDrafts: false, InlineFindings: true, CommandsAuthorAndMaintainersOnly: true, IncrementalReviews: true, LearnFromDismissals: true, FailureStatus: "failed", JobTimeoutSeconds: 600, MaxTurns: 40, RerunLimitPerMR: 10, RerunLimitPerRepo: 50}); !sameDefaults(v, want) {
		t.Errorf("defaults after a replacing PUT = %+v, want %+v", v, want)
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"timeout too small", `{"jobTimeoutSeconds": 30}`, "jobTimeoutSeconds must be a whole number of seconds from 60 to 7200, or null"},
		{"timeout too big", `{"jobTimeoutSeconds": 7201}`, "jobTimeoutSeconds must be a whole number of seconds from 60 to 7200, or null"},
		{"max steps too small", `{"maxTurns": 4}`, "maxTurns must be a whole number from 5 to 200, or null"},
		{"max steps too big", `{"maxTurns": 201}`, "maxTurns must be a whole number from 5 to 200, or null"},
		{"max steps not whole", `{"maxTurns": 40.5}`, "maxTurns must be a whole number from 5 to 200, or null"},
		{"max steps not a number", `{"maxTurns": "40"}`, "maxTurns must be a whole number from 5 to 200, or null"},
		{"memory too small", `{"jobMemory": "100Mi"}`, `jobMemory must be a size from 256Mi to 32Gi such as "6Gi", or null`},
		{"memory not a size", `{"jobMemory": "lots"}`, `jobMemory must be a size from 256Mi to 32Gi such as "6Gi", or null`},
		{"cap per MR too small", `{"rerunLimitPerMR": 0}`, "rerunLimitPerMR must be a whole number from 1 to 1000, or null"},
		{"cap per repo too big", `{"rerunLimitPerRepo": 1001}`, "rerunLimitPerRepo must be a whole number from 1 to 1000, or null"},
		{"cap not a number", `{"rerunLimitPerRepo": "50"}`, "rerunLimitPerRepo must be a whole number from 1 to 1000, or null"},
		{"bad status", `{"failureStatus": "sideways"}`, `failureStatus must be "failed", "neutral" or null`},
		{"bad bool", `{"approve": "yes"}`, "approve must be true, false or null"},
		{"bad inline findings", `{"inlineFindings": "yes"}`, "inlineFindings must be true, false or null"},
		{"bad commands switch", `{"commandsAuthorAndMaintainersOnly": "yes"}`, "commandsAuthorAndMaintainersOnly must be true, false or null"},
		{"bad incremental reviews", `{"incrementalReviews": "yes"}`, "incrementalReviews must be true, false or null"},
		{"bad learn from dismissals", `{"learnFromDismissals": "yes"}`, "learnFromDismissals must be true, false or null"},
		{"bad quiet reviews", `{"quietReviews": "yes"}`, "quietReviews must be true, false or null"},
		{"not an object", `[1]`, "the body must be a JSON object of defaults"},
	} {
		if rec := e.send(admin, "PUT", "/api/admin/defaults", tc.body); rec.Code != http.StatusBadRequest || errorOf(t, rec) != tc.want && !strings.Contains(errorOf(t, rec), tc.want) {
			t.Errorf("%s: PUT = %d %s; want 400 %q", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
	if got := getJSON[DefaultsView](e, admin, "/api/admin/defaults"); !sameDefaults(got, v) {
		t.Errorf("a refused PUT changed the defaults: %+v", got)
	}

	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "defaults_updated" {
			details = append(details, a.Detail)
		}
	}
	slices.Reverse(details)
	if !slices.Equal(details, []string{"approve, failureStatus, jobTimeoutSeconds, maxTurns, jobMemory, rerunLimitPerMR, rerunLimitPerRepo",
		"approve, reviewDrafts, failureStatus, jobTimeoutSeconds, maxTurns, jobMemory, rerunLimitPerMR, rerunLimitPerRepo"}) {
		t.Errorf("defaults_updated details = %q", details)
	}

	// Maintainers and viewers can't change them.
	viewer := e.signInGitHub(false, "acme/widgets")
	if rec := e.send(viewer, "PUT", "/api/admin/defaults", `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("PUT as a viewer = %d, want 403", rec.Code)
	}
	if rec := e.get(viewer, "/api/admin/defaults"); rec.Code != http.StatusForbidden {
		t.Errorf("GET as a viewer = %d, want 403", rec.Code)
	}
}

// An operator picks repositories of any instance without a platform
// account: those the review bot or App reaches that aren't on yet; switching
// one on needs only that, and a repository of the config is 409.
func TestOperatorPicksRepositories(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.cat["main"].set("acme/gadgets", "acme/widgets", "acme/tools")
	v := getJSON[AvailableView](e, c, "/api/admin/instances/main/repositories")
	var got []string
	for _, it := range v.Items {
		got = append(got, it.Instance+"/"+it.Project+"/"+it.Platform)
	}
	if !slices.Equal(got, []string{"main/acme/gadgets/gitlab", "main/acme/tools/gitlab"}) || v.Truncated {
		t.Errorf("picker = %v truncated %v; want the repositories not on yet", got, v.Truncated)
	}
	if v := getJSON[AvailableView](e, c, "/api/admin/instances/main/repositories?search=TOOLS"); len(v.Items) != 1 || v.Items[0].Project != "acme/tools" {
		t.Errorf("search = %+v", v)
	}
	if rec := e.get(c, "/api/admin/instances/nope/repositories"); rec.Code != http.StatusNotFound {
		t.Errorf("picker of an unknown instance = %d, want 404", rec.Code)
	}
	e.cat["main"].setFail(errors.New("boom"))
	if rec := e.get(c, "/api/admin/instances/main/repositories?search=x"); rec.Code != http.StatusBadGateway {
		t.Errorf("picker with the platform down = %d, want 502", rec.Code)
	}
	e.cat["main"].setFail(nil)

	if n := e.gl.api() + e.gl.calls(); n != 0 {
		t.Errorf("%d calls to the platform with a user token", n)
	}
	body := func(instance, project string) map[string]string {
		return map[string]string{"instance": instance, "project": project}
	}
	rec := e.sendJSON(c, "POST", "/api/repositories", body("main", "ACME/Gadgets"))
	detail := decode[RepositoryDetailView](t, rec, "POST /api/repositories", http.StatusCreated)
	if detail.Repo.Instance != "main" || detail.Repo.Project != "ACME/Gadgets" || detail.Role != "admin" || !detail.Repo.On {
		t.Errorf("detail = %+v", detail)
	}
	if n := e.reconciles.Load(); n != 1 {
		t.Errorf("reconcile asked %d times, want once", n)
	}
	if _, found := e.storedRepo("main", "ACME/Gadgets"); !found {
		t.Error("the repository was not stored")
	}
	for _, tc := range []struct {
		name string
		body map[string]string
		want int
	}{
		{"again", body("main", "acme/gadgets"), 409},
		{"in the config", body("main", "acme/widgets"), 409},
		{"unreachable by the review bot", body("main", "acme/secret"), 404},
		{"unknown instance", body("nope", "acme/gadgets"), 404},
		{"no instance", map[string]string{"project": "acme/tools"}, 400},
	} {
		if rec := e.sendJSON(c, "POST", "/api/repositories", tc.body); rec.Code != tc.want {
			t.Errorf("%s: POST = %d %s, want %d", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
	// And off again.
	if rec := e.send(c, "DELETE", "/api/repositories/main/acme/gadgets", ""); rec.Code != http.StatusOK {
		t.Errorf("DELETE = %d %s", rec.Code, rec.Body)
	}
	// A platform admin still names no other instance than its own.
	admin := e.signInGitLab(true, "acme/widgets")
	if rec := e.sendJSON(admin, "POST", "/api/repositories", body("gh", "acme/tools")); rec.Code != http.StatusBadRequest {
		t.Errorf("a platform admin naming another instance = %d %s, want 400", rec.Code, rec.Body)
	}
}

// An operator reads the integrations of the instance it names.
func TestIntegrationsForOperator(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	v := getJSON[IntegrationView](e, c, "/api/integrations?instance=gh")
	if v.Instance != "gh" || v.Platform != "github" || !v.Connected || len(v.Repos) != 1 || v.Repos[0].Project != "acme/widgets" {
		t.Errorf("integrations of gh = %+v", v)
	}
	if rec := e.get(c, "/api/integrations"); rec.Code != http.StatusNotFound {
		t.Errorf("integrations without an instance = %d, want 404", rec.Code)
	}
	e.addLab(c)
	e.sources.Delete("lab") // not connected
	if v := getJSON[IntegrationView](e, c, "/api/integrations?instance=lab"); v.Connected {
		t.Errorf("an instance that isn't connected shows connected: %+v", v)
	}
}

// No response, log line or audit entry ever holds a secret entered in the
// panel.
func TestOperatorAPINeverReturnsSecrets(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.addLab(c)
	e.sendJSON(c, "POST", "/api/admin/instances", hubBody(nil))
	e.sendJSON(c, "PUT", "/api/admin/instances/lab", map[string]any{"secrets": map[string]string{"approve_token": labApproveToken + "-v2"}})
	e.sendJSON(c, "PUT", "/api/admin/llm", map[string]any{"provider": "anthropic", "model": "m", "apiKey": llmKeyOne})
	e.sendJSON(c, "PUT", "/api/admin/llm", map[string]any{"provider": "anthropic", "model": "m2", "apiKey": llmKeyTwo})
	// Failing writes echo nothing either.
	e.conn.checkErr = func(_ instances.Instance, s instances.Secrets) error { return errors.New("rejected " + s.ApproveToken) }
	e.sendJSON(c, "PUT", "/api/admin/instances/lab", map[string]any{"secrets": map[string]string{"approve_token": "bad-token-SECRET-000"}})
	e.conn.checkErr = nil
	for _, target := range []string{"/api/admin/instances", "/api/admin/llm", "/api/admin/defaults", "/api/admin/settings",
		"/api/integrations?instance=lab", "/api/me", "/api/instances", "/api/audit"} {
		e.get(c, target)
	}

	secrets := []string{labReviewToken, labApproveToken, labApproveToken + "-v2", labOAuthSecret, hubReviewKey, hubApproveKey, hubWebhook,
		llmKeyOne, llmKeyTwo, "bad-token-SECRET-000", "hub-review-key-SECRET-444", "hub-approve-key-SECRET-555"}
	for _, h := range e.conn.connectCalls() {
		if h.inst.Name == "lab" {
			secrets = append(secrets, h.s.WebhookSecret)
		}
	}
	e.mu.Lock()
	var bodies []string
	for _, rec := range e.responses {
		bodies = append(bodies, rec.Body.String())
		for _, vs := range rec.Header() {
			bodies = append(bodies, strings.Join(vs, " "))
		}
	}
	e.mu.Unlock()
	for _, entry := range e.auditEntries() {
		bodies = append(bodies, entry.Actor, entry.Action, entry.Instance, entry.Project, entry.Detail)
	}
	bodies = append(bodies, e.logs.String())
	for _, secret := range secrets {
		for _, body := range bodies {
			if strings.Contains(body, secret) {
				t.Fatalf("the secret %q is in a response, log or audit entry:\n%s", secret, body)
			}
		}
	}
}

// A change that is checking while the instance is removed does not undo the
// removal: the store refuses it (409), the registry stays removing and no
// sign-in provider comes back.
func TestUpdateRacingARemovalKeepsItRemoved(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.addLab(c)
	connects := len(e.conn.connectCalls())
	// The removal lands while the check is running.
	e.conn.checkErr = func(inst instances.Instance, _ instances.Secrets) error {
		if ok, err := e.st.MarkInstanceRemoving(ctx, "lab"); err != nil || !ok {
			t.Errorf("MarkInstanceRemoving = %v, %v", ok, err)
		}
		e.reg.SetRemoving("lab")
		e.d.Auth.RemoveProvider("lab")
		return nil
	}
	rec := e.sendJSON(c, "PUT", "/api/admin/instances/lab", map[string]any{"oauthClientID": "lab-client-2"})
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != `{"error":"this instance is being removed"}` {
		t.Fatalf("PUT = %d %s, want 409 this instance is being removed", rec.Code, rec.Body)
	}
	inst, ok := e.reg.Get("lab")
	if !ok || !inst.Removing || inst.OAuthClientID != "lab-client" {
		t.Errorf("registry lab = %+v, %v; want removing, client ID unchanged", inst, ok)
	}
	if row, _, _ := e.st.InstanceRow(ctx, "lab"); !row.Removing || row.OAuthClientID != "lab-client" {
		t.Errorf("stored lab = %+v", row)
	}
	if n := len(e.conn.connectCalls()); n != connects {
		t.Errorf("connected %d more times", n-connects)
	}
	if code := e.do("GET", "/auth/gitlab/lab/login", nil, nil).Code; code != http.StatusNotFound {
		t.Errorf("login through lab = %d, want 404", code)
	}
	signin := decode[instancesView](t, e.do("GET", "/api/instances", nil, nil), "GET /api/instances", http.StatusOK)
	if slices.ContainsFunc(signin.Instances, func(v InstanceView) bool { return v.Instance == "lab" }) {
		t.Errorf("sign-in is offered through lab: %+v", signin.Instances)
	}
}

// If the LLM was saved but reading it back fails, the change is not lost: the
// answer is 200 with what was saved, and the failure is logged at Error
// without the key.
func TestPutLLMSavedButNotReloaded(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.d.loadLLM = func(context.Context) error { return errors.New("database went away (" + llmKeyOne + ")") }
	rec := e.sendJSON(c, "PUT", "/api/admin/llm", map[string]any{"provider": "anthropic", "model": "claude-sonnet-4", "baseURL": "https://llm.example.com/v1", "apiKey": llmKeyOne})
	v := decode[LLMAdminView](t, rec, "PUT /api/admin/llm", http.StatusOK)
	if v.Provider != "anthropic" || v.Model != "claude-sonnet-4" || v.BaseURL != "https://llm.example.com/v1" || !v.Key.Set || !v.Editable {
		t.Errorf("view = %+v, want what was saved", v)
	}
	if row, found, _ := e.st.LLM(ctx); !found || row.Model != "claude-sonnet-4" {
		t.Errorf("stored LLM = %+v, %v", row, found)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "database went away") || strings.Contains(logs, llmKeyOne) {
		t.Errorf("log = %s; want the failure at Error with the key hidden", logs)
	}
	if !slices.ContainsFunc(e.auditEntries(), func(a store.AuditEntry) bool { return a.Action == "llm_updated" }) {
		t.Error("the save was not audited")
	}
}

// Only admins change the defaults: a maintainer or viewer is 403 with a valid
// body, and nothing changes.
func TestDefaultsRefusedToNonAdmins(t *testing.T) {
	e := newEnv(t, nil)
	for name, c := range map[string]*http.Cookie{
		"viewer":     e.signInGitHub(false, "acme/widgets"),
		"waiting":    e.signIn("gitlab", "main"),
		"no session": nil,
	} {
		var rec *httptest.ResponseRecorder
		if c == nil {
			rec = e.do("PUT", "/api/admin/defaults", nil, map[string]string{"Content-Type": "application/json"})
		} else {
			rec = e.send(c, "PUT", "/api/admin/defaults", `{"approve": false}`)
		}
		want := http.StatusForbidden
		if c == nil {
			want = http.StatusUnauthorized
		}
		if rec.Code != want {
			t.Errorf("%s: PUT /api/admin/defaults = %d %s, want %d", name, rec.Code, rec.Body, want)
		}
	}
	if d, err := e.st.RepoDefaults(ctx); err != nil || d.Approve != nil {
		t.Errorf("defaults = %+v, %v; want untouched", d, err)
	}
}

// An instance whose Connect fails after its check passed is still stored,
// and the failure is recorded as the instance-wide instance_unreachable item,
// without the secrets; saving it again records it again (the connector
// clears it when it connects).
func TestConnectFailureAfterCheckRecordsAttention(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.conn.connectErr = func(_ instances.Instance, s instances.Secrets) error {
		return errors.New("connecting with " + s.ReviewToken + " failed")
	}
	wantCode(t, e.sendJSON(c, "POST", "/api/admin/instances", labBody(nil)), "POST lab", http.StatusCreated)

	items, err := e.st.Attention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Instance != "lab" || items[0].Project != "" || items[0].Kind != AttInstanceUnreachable ||
		!strings.Contains(items[0].Message, "connecting with [REDACTED] failed") {
		t.Fatalf("attention = %+v; want one instance_unreachable item for lab, redacted", items)
	}
	if strings.Contains(items[0].Message, labReviewToken) {
		t.Error("the item holds the review token")
	}

	e.conn.connectErr = nil
	if err := e.st.ClearAttention(ctx, "lab", "", AttInstanceUnreachable); err != nil {
		t.Fatal(err)
	}
	e.conn.connectErr = func(instances.Instance, instances.Secrets) error { return errors.New("down again") }
	wantCode(t, e.sendJSON(c, "PUT", "/api/admin/instances/lab", map[string]any{"secrets": map[string]string{"review_token": "new-review-token-SECRET"}}),
		"PUT lab", http.StatusOK)
	if items, _ := e.st.Attention(ctx); len(items) != 1 || items[0].Message != "down again" {
		t.Errorf("attention after the update = %+v; want the new failure", items)
	}
}

// The default for inline findings is stored with the other defaults; left
// out of a body it is the built-in one (on), and a dashboard repository takes it.
func TestDefaultsInlineFindings(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")

	v := decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"inlineFindings": false}), "PUT", http.StatusOK)
	if v.InlineFindings {
		t.Errorf("inlineFindings after PUT false = true")
	}
	if stored, err := e.st.RepoDefaults(ctx); err != nil || stored.InlineFindings == nil || *stored.InlineFindings {
		t.Errorf("stored defaults = %+v, %v; want inlineFindings false", stored, err)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || g.InlineFindings {
		t.Errorf("dashboard repository = %+v, %v; want inline findings off", g, err)
	}
	if w, _, _ := e.d.Repos.Get(ctx, "main", "acme/widgets"); !w.InlineFindings {
		t.Errorf("config repository = %+v, want the default, on", w)
	}
	if got := getJSON[DefaultsView](e, admin, "/api/admin/defaults"); got.InlineFindings {
		t.Errorf("GET after PUT = %+v", got)
	}

	// Left out, it is back to the built-in default.
	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"approve": true}), "PUT", http.StatusOK)
	if !v.InlineFindings {
		t.Errorf("inlineFindings after a PUT without it = false, want true")
	}
	if stored, _ := e.st.RepoDefaults(ctx); stored.InlineFindings != nil {
		t.Errorf("stored = %v, want NULL", stored.InlineFindings)
	}
}

// The default for the commands switch is stored with the other defaults; it
// is in the view's config and pinned, left out of a body it is the built-in
// one (on), and a dashboard repository takes it.
func TestDefaultsCommandsAuthorAndMaintainersOnly(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")

	v := getJSON[DefaultsView](e, admin, "/api/admin/defaults")
	if !v.CommandsAuthorAndMaintainersOnly || !v.Config.CommandsAuthorAndMaintainersOnly || v.Pinned["commandsAuthorAndMaintainersOnly"] {
		t.Errorf("initial view = %+v; want on, config on, not pinned", v)
	}

	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"commandsAuthorAndMaintainersOnly": false}), "PUT", http.StatusOK)
	if v.CommandsAuthorAndMaintainersOnly || !v.Config.CommandsAuthorAndMaintainersOnly || !v.Pinned["commandsAuthorAndMaintainersOnly"] {
		t.Errorf("after PUT false: view = %+v; want off, config on, pinned", v)
	}
	if stored, err := e.st.RepoDefaults(ctx); err != nil || stored.CommandsAuthorAndMaintainersOnly == nil || *stored.CommandsAuthorAndMaintainersOnly {
		t.Errorf("stored defaults = %+v, %v; want false", stored, err)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || g.CommandsAuthorAndMaintainersOnly {
		t.Errorf("dashboard repository = %+v, %v; want the switch off", g, err)
	}
	if w, _, _ := e.d.Repos.Get(ctx, "main", "acme/widgets"); !w.CommandsAuthorAndMaintainersOnly {
		t.Errorf("config repository = %+v, want the default, on", w)
	}

	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"approve": true}), "PUT", http.StatusOK)
	if !v.CommandsAuthorAndMaintainersOnly || v.Pinned["commandsAuthorAndMaintainersOnly"] {
		t.Errorf("after a PUT without it: view = %+v; want on, not pinned", v)
	}
	if stored, _ := e.st.RepoDefaults(ctx); stored.CommandsAuthorAndMaintainersOnly != nil {
		t.Errorf("stored = %v, want NULL", stored.CommandsAuthorAndMaintainersOnly)
	}
}

// The default for incremental reviews is stored with the other defaults; it
// is in the view's config and pinned, left out of a body it is the built-in
// one (on), and a dashboard repository takes it.
func TestDefaultsIncrementalReviews(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")

	v := getJSON[DefaultsView](e, admin, "/api/admin/defaults")
	if !v.IncrementalReviews || !v.Config.IncrementalReviews || v.Pinned["incrementalReviews"] {
		t.Errorf("initial view = %+v; want on, config on, not pinned", v)
	}

	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"incrementalReviews": false}), "PUT", http.StatusOK)
	if v.IncrementalReviews || !v.Config.IncrementalReviews || !v.Pinned["incrementalReviews"] {
		t.Errorf("after PUT false: view = %+v; want off, config on, pinned", v)
	}
	if stored, err := e.st.RepoDefaults(ctx); err != nil || stored.IncrementalReviews == nil || *stored.IncrementalReviews {
		t.Errorf("stored defaults = %+v, %v; want false", stored, err)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || g.IncrementalReviews {
		t.Errorf("dashboard repository = %+v, %v; want incremental reviews off", g, err)
	}
	if w, _, _ := e.d.Repos.Get(ctx, "main", "acme/widgets"); !w.IncrementalReviews {
		t.Errorf("config repository = %+v, want the default, on", w)
	}

	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"approve": true}), "PUT", http.StatusOK)
	if !v.IncrementalReviews || v.Pinned["incrementalReviews"] {
		t.Errorf("after a PUT without it: view = %+v; want on, not pinned", v)
	}
	if stored, _ := e.st.RepoDefaults(ctx); stored.IncrementalReviews != nil {
		t.Errorf("stored = %v, want NULL", stored.IncrementalReviews)
	}
}

// The default for learn from dismissals is stored with the other defaults; it
// is in the view's config and pinned, left out of a body it is the built-in
// one (on), and a dashboard repository takes it.
func TestDefaultsLearnFromDismissals(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")

	v := getJSON[DefaultsView](e, admin, "/api/admin/defaults")
	if !v.LearnFromDismissals || !v.Config.LearnFromDismissals || v.Pinned["learnFromDismissals"] {
		t.Errorf("initial view = %+v; want on, config on, not pinned", v)
	}

	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"learnFromDismissals": false}), "PUT", http.StatusOK)
	if v.LearnFromDismissals || !v.Config.LearnFromDismissals || !v.Pinned["learnFromDismissals"] {
		t.Errorf("after PUT false: view = %+v; want off, config on, pinned", v)
	}
	if stored, err := e.st.RepoDefaults(ctx); err != nil || stored.LearnFromDismissals == nil || *stored.LearnFromDismissals {
		t.Errorf("stored defaults = %+v, %v; want false", stored, err)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || g.LearnFromDismissals {
		t.Errorf("dashboard repository = %+v, %v; want learn from dismissals off", g, err)
	}
	if w, _, _ := e.d.Repos.Get(ctx, "main", "acme/widgets"); !w.LearnFromDismissals {
		t.Errorf("config repository = %+v, want the default, on", w)
	}

	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"approve": true}), "PUT", http.StatusOK)
	if !v.LearnFromDismissals || v.Pinned["learnFromDismissals"] {
		t.Errorf("after a PUT without it: view = %+v; want on, not pinned", v)
	}
	if stored, _ := e.st.RepoDefaults(ctx); stored.LearnFromDismissals != nil {
		t.Errorf("stored = %v, want NULL", stored.LearnFromDismissals)
	}
}

// The default for quiet reviews is stored with the other defaults; it is in
// the view's config and pinned, the save is audited under its name, left out
// of a body it is the built-in one (off), and a dashboard repository takes it.
func TestDefaultsQuietReviews(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")

	v := getJSON[DefaultsView](e, admin, "/api/admin/defaults")
	if v.QuietReviews || v.Config.QuietReviews || v.Pinned["quietReviews"] {
		t.Errorf("initial view = %+v; want off, config off, not pinned", v)
	}

	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"quietReviews": true}), "PUT", http.StatusOK)
	if !v.QuietReviews || v.Config.QuietReviews || !v.Pinned["quietReviews"] {
		t.Errorf("after PUT true: view = %+v; want on, config off, pinned", v)
	}
	if stored, err := e.st.RepoDefaults(ctx); err != nil || stored.QuietReviews == nil || !*stored.QuietReviews {
		t.Errorf("stored defaults = %+v, %v; want true", stored, err)
	}
	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "defaults_updated" {
			details = append(details, a.Detail)
		}
	}
	if !slices.Equal(details, []string{"quietReviews"}) {
		t.Errorf("defaults_updated details = %q; want quietReviews named", details)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	if g, _, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || !g.QuietReviews {
		t.Errorf("dashboard repository = %+v, %v; want quiet reviews on", g, err)
	}
	if w, ok, err := e.d.Repos.Get(ctx, "main", "acme/widgets"); err != nil || !ok || w.QuietReviews {
		t.Errorf("config repository = %+v, %v, %v; want the default, off", w, ok, err)
	}

	rec := e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"quietReviews": "x"})
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != "quietReviews must be true, false or null" {
		t.Errorf("PUT x = %d %s; want 400 naming the key", rec.Code, rec.Body)
	}
	if !strings.Contains(msgBadDefaults, "quietReviews") {
		t.Errorf("msgBadDefaults = %q", msgBadDefaults)
	}

	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"approve": true}), "PUT", http.StatusOK)
	if v.QuietReviews || v.Pinned["quietReviews"] {
		t.Errorf("after a PUT without it: view = %+v; want off, not pinned", v)
	}
	if stored, _ := e.st.RepoDefaults(ctx); stored.QuietReviews != nil {
		t.Errorf("stored = %v, want NULL", stored.QuietReviews)
	}
}

// The default for the comment layout is stored with the other defaults. The
// view shows the built-in layout until one is saved, its config value is
// always the built-in one, and it is pinned once saved. A valid list is kept in
// order and audited under its name, the five refusals are 400 with the
// defaults message, null clears it, a PUT without it clears it too (the PUT
// replaces every default), and a dashboard repository takes it while a config
// repository does not.
func TestPutDefaultsCommentLayout(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")
	builtIn := []string{"summary:open", "findings:collapsed", "dismissed:collapsed", "checks:open", "files:collapsed"}

	v := getJSON[DefaultsView](e, admin, "/api/admin/defaults")
	if !slices.Equal(v.CommentLayout, builtIn) || !slices.Equal(v.Config.CommentLayout, builtIn) || v.Pinned["commentLayout"] {
		t.Errorf("initial view = %q, config %q, pinned %v; want the built-in layout, not pinned", v.CommentLayout, v.Config.CommentLayout, v.Pinned["commentLayout"])
	}

	want := []string{"findings:open", "checks:collapsed", "files:open"}
	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"commentLayout": want}), "PUT list", http.StatusOK)
	if !slices.Equal(v.CommentLayout, want) || !slices.Equal(v.Config.CommentLayout, builtIn) || !v.Pinned["commentLayout"] {
		t.Errorf("after PUT: view = %q, config %q, pinned %v; want the list, config built-in, pinned", v.CommentLayout, v.Config.CommentLayout, v.Pinned["commentLayout"])
	}
	if stored, err := e.st.RepoDefaults(ctx); err != nil || !slices.Equal(stored.CommentLayout, want) {
		t.Errorf("stored defaults = %+v, %v; want the list", stored, err)
	}
	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "defaults_updated" {
			details = append(details, a.Detail)
		}
	}
	if !slices.Equal(details, []string{"commentLayout"}) {
		t.Errorf("defaults_updated details = %q; want commentLayout named", details)
	}
	e.dashboardRepo("main", "acme/gadgets", nil)
	if g, ok, err := e.d.Repos.Get(ctx, "main", "acme/gadgets"); err != nil || !ok || !slices.Equal(g.CommentLayout, want) {
		t.Errorf("dashboard repository = %+v, %v, %v; want the default layout", g, ok, err)
	}
	if w, ok, err := e.d.Repos.Get(ctx, "main", "acme/widgets"); err != nil || !ok || w.CommentLayout != nil {
		t.Errorf("config repository = %+v, %v, %v; want no layout", w, ok, err)
	}

	for _, tc := range []struct{ name, body string }{
		{"unknown block", `{"commentLayout":["findings:open","notes:open"]}`},
		{"unknown state", `{"commentLayout":["findings:hidden"]}`},
		{"repeated block", `{"commentLayout":["findings:open","findings:collapsed"]}`},
		{"empty list", `{"commentLayout":[]}`},
		{"no findings", `{"commentLayout":["summary:open","files:open"]}`},
		{"not a list", `{"commentLayout":"findings:open"}`},
	} {
		rec := e.send(admin, "PUT", "/api/admin/defaults", tc.body)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgBadDefaults {
			t.Errorf("%s: PUT = %d %s; want 400 %q", tc.name, rec.Code, rec.Body, msgBadDefaults)
		}
		if stored, _ := e.st.RepoDefaults(ctx); !slices.Equal(stored.CommentLayout, want) {
			t.Errorf("%s: a refused PUT changed the defaults: %q", tc.name, stored.CommentLayout)
		}
	}
	if !strings.Contains(msgBadDefaults, "commentLayout") {
		t.Errorf("msgBadDefaults = %q", msgBadDefaults)
	}

	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"commentLayout": nil}), "PUT null", http.StatusOK)
	if !slices.Equal(v.CommentLayout, builtIn) || v.Pinned["commentLayout"] {
		t.Errorf("after null: view = %q, pinned %v; want the built-in layout, not pinned", v.CommentLayout, v.Pinned["commentLayout"])
	}
	if stored, _ := e.st.RepoDefaults(ctx); stored.CommentLayout != nil {
		t.Errorf("stored after null = %q, want NULL", stored.CommentLayout)
	}

	e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"commentLayout": want})
	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"approve": true}), "PUT without it", http.StatusOK)
	if v.Pinned["commentLayout"] {
		t.Errorf("after a PUT without it: pinned = %v; want not pinned", v.Pinned)
	}
	if stored, _ := e.st.RepoDefaults(ctx); stored.CommentLayout != nil {
		t.Errorf("stored after a PUT without it = %q, want NULL", stored.CommentLayout)
	}
}

// A default is pinned when the dashboard stored a value for it, and only then.
func TestPinnedDefaults(t *testing.T) {
	none := pinnedDefaults(store.RepoDefaults{})
	if len(none) != 14 {
		t.Errorf("pinned = %v; want the fourteen keys", none)
	}
	for k, v := range none {
		if v {
			t.Errorf("%s pinned with nothing stored", k)
		}
	}
	got := pinnedDefaults(store.RepoDefaults{Approve: ptr(false), JobTimeoutSeconds: ptr(600), JobMemory: ptr("2Gi"), RerunLimitPerRepo: ptr(5)})
	for _, k := range []string{"approve", "jobTimeoutSeconds", "jobMemory", "rerunLimitPerRepo"} {
		if !got[k] {
			t.Errorf("%s not pinned: %v", k, got)
		}
	}
	if !pinnedDefaults(store.RepoDefaults{CommandsAuthorAndMaintainersOnly: ptr(false)})["commandsAuthorAndMaintainersOnly"] {
		t.Errorf("commandsAuthorAndMaintainersOnly not pinned when stored")
	}
	if !pinnedDefaults(store.RepoDefaults{IncrementalReviews: ptr(false)})["incrementalReviews"] {
		t.Errorf("incrementalReviews not pinned when stored")
	}
	if !pinnedDefaults(store.RepoDefaults{LearnFromDismissals: ptr(false)})["learnFromDismissals"] {
		t.Errorf("learnFromDismissals not pinned when stored")
	}
	if !pinnedDefaults(store.RepoDefaults{QuietReviews: ptr(true)})["quietReviews"] {
		t.Errorf("quietReviews not pinned when stored")
	}
	if !pinnedDefaults(store.RepoDefaults{CommentLayout: []string{"findings:open"}})["commentLayout"] {
		t.Errorf("commentLayout not pinned when stored")
	}
	for _, k := range []string{"reviewDrafts", "inlineFindings", "commandsAuthorAndMaintainersOnly", "incrementalReviews", "learnFromDismissals", "quietReviews", "commentLayout", "failureStatus", "maxTurns", "rerunLimitPerMR"} {
		if got[k] {
			t.Errorf("%s pinned: %v", k, got)
		}
	}
}
