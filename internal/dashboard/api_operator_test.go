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
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

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
	mu          sync.Mutex
	auth        *Auth
	sources     *syncmap.Map[RepoSource]
	catalogs    *syncmap.Map[RepoCatalog]
	hooks       *syncmap.Map[string] // the webhook secret of each instance connected
	checkErr    func(inst instances.Instance, s instances.Secrets) error
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
		entries[i].Instance != "lab" || entries[i].Actor != "ops" {
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
func (q *hookQueue) SupersedeDraftMR(context.Context, store.MRKey, string, string) (int, error) {
	return 0, nil
}
func (q *hookQueue) Republish(context.Context, store.MRKey, string, string) (bool, error) {
	return true, nil
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
		{"github app ids", hubBody(func(m map[string]any) { m["approveAppID"] = 11 }), 400, "different from reviewAppID"},
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

// The defaults are shown and changed by admins, with the bounds and the
// messages of a repository's settings; they apply to the repositories
// switched on in the dashboard, not to the config's.
func TestDefaults(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.signInGitLab(true, "acme/widgets")
	v := getJSON[DefaultsView](e, admin, "/api/admin/defaults")
	if want := (DefaultsView{Approve: true, ReviewDrafts: true, FailureStatus: "failed", JobTimeoutSeconds: 600}); v != want {
		t.Errorf("defaults = %+v, want %+v", v, want)
	}

	rec := e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{
		"approve": false, "failureStatus": "neutral", "jobTimeoutSeconds": 900, "jobMemory": "2Gi"})
	v = decode[DefaultsView](t, rec, "PUT /api/admin/defaults", http.StatusOK)
	if want := (DefaultsView{Approve: false, ReviewDrafts: true, FailureStatus: "neutral", JobTimeoutSeconds: 900, JobMemory: "2Gi"}); v != want {
		t.Errorf("defaults after PUT = %+v, want %+v", v, want)
	}
	if got := getJSON[DefaultsView](e, admin, "/api/admin/defaults"); got != v {
		t.Errorf("GET after PUT = %+v", got)
	}

	// A dashboard repository takes them; a config repository doesn't.
	e.dashboardRepo("main", "acme/gadgets", nil)
	g, ok, err := e.d.Repos.Get(ctx, "main", "acme/gadgets")
	if err != nil || !ok || g.Approve || g.FailureStatus != config.FailureStatusNeutral || g.JobTimeout.Seconds() != 900 {
		t.Errorf("dashboard repository = %+v, %v, %v", g, ok, err)
	}
	if w, _, _ := e.d.Repos.Get(ctx, "main", "acme/widgets"); !w.Approve || w.FailureStatus != config.FailureStatusFailed || w.JobTimeout.Seconds() != 600 {
		t.Errorf("config repository = %+v, want the config's defaults", w)
	}

	// The body replaces them all: what it leaves out is the built-in default.
	v = decode[DefaultsView](t, e.sendJSON(admin, "PUT", "/api/admin/defaults", map[string]any{"reviewDrafts": false, "jobMemory": nil}),
		"PUT", http.StatusOK)
	if want := (DefaultsView{Approve: true, ReviewDrafts: false, FailureStatus: "failed", JobTimeoutSeconds: 600}); v != want {
		t.Errorf("defaults after a replacing PUT = %+v, want %+v", v, want)
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"timeout too small", `{"jobTimeoutSeconds": 30}`, "jobTimeoutSeconds must be a whole number of seconds from 60 to 7200, or null"},
		{"timeout too big", `{"jobTimeoutSeconds": 7201}`, "jobTimeoutSeconds must be a whole number of seconds from 60 to 7200, or null"},
		{"memory too small", `{"jobMemory": "100Mi"}`, `jobMemory must be a size from 256Mi to 32Gi such as "6Gi", or null`},
		{"memory not a size", `{"jobMemory": "lots"}`, `jobMemory must be a size from 256Mi to 32Gi such as "6Gi", or null`},
		{"bad status", `{"failureStatus": "sideways"}`, `failureStatus must be "failed", "neutral" or null`},
		{"bad bool", `{"approve": "yes"}`, "approve must be true, false or null"},
		{"skills are not a default", `{"skills": ["code-review"]}`, "the body must be a JSON object of defaults"},
		{"not an object", `[1]`, "the body must be a JSON object of defaults"},
	} {
		if rec := e.send(admin, "PUT", "/api/admin/defaults", tc.body); rec.Code != http.StatusBadRequest || errorOf(t, rec) != tc.want && !strings.Contains(errorOf(t, rec), tc.want) {
			t.Errorf("%s: PUT = %d %s; want 400 %q", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
	if got := getJSON[DefaultsView](e, admin, "/api/admin/defaults"); got != v {
		t.Errorf("a refused PUT changed the defaults: %+v", got)
	}

	var details []string
	for _, a := range e.auditEntries() {
		if a.Action == "defaults_updated" {
			details = append(details, a.Detail)
		}
	}
	slices.Reverse(details)
	if !slices.Equal(details, []string{"approve, failureStatus, jobTimeoutSeconds, jobMemory", "approve, reviewDrafts, failureStatus, jobTimeoutSeconds, jobMemory"}) {
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
