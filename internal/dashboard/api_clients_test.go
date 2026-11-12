package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const apiClientsURL = "/api/admin/api-clients"

// goodClient is a body that makes a client; edit changes it.
func goodClient(edit func(m map[string]any)) map[string]any {
	m := map[string]any{"name": "fastgateway", "skills": []string{"security"}, "modelId": "",
		"callbackUrl": "https://fastgateway.example.com/hooks/pergent"}
	if edit != nil {
		edit(m)
	}
	return m
}

// goodLimits is a "limits" object that passes; edit changes it.
func goodLimits(edit func(m map[string]any)) map[string]any {
	m := map[string]any{"maxRequestBytes": 524288, "maxFiles": 200, "maxInFlight": 5, "requestsPerHour": 600,
		"monthlyTokenBudget": 10000000}
	if edit != nil {
		edit(m)
	}
	return m
}

// addAPIClient makes a client through the API and returns the answer.
func (e *env) addAPIClient(c *http.Cookie, body map[string]any) APIClientCreatedView {
	e.t.Helper()
	return decode[APIClientCreatedView](e.t, e.sendJSON(c, "POST", apiClientsURL, body), "POST api-clients", http.StatusCreated)
}

// A secret comes back in the answer that makes it and nowhere else: not in
// the client's page, the list or the audit log.
func TestCreateAPIClientShowsSecretOnce(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()

	created := e.addAPIClient(c, goodClient(nil))
	if !strings.HasPrefix(created.CallbackSecret, "pgs_") {
		t.Errorf("callbackSecret = %q, want it to start with pgs_", created.CallbackSecret)
	}
	if created.Client.Name != "fastgateway" || len(created.Client.Keys) != 0 {
		t.Errorf("client = %+v", created.Client)
	}
	if created.Client.Limits != (APILimitsView{524288, 200, 5, 600, 10_000_000}) {
		t.Errorf("limits = %+v, want the defaults", created.Client.Limits)
	}

	page := e.get(c, apiClientsURL+"/fastgateway")
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "callbackSecret") ||
		strings.Contains(page.Body.String(), created.CallbackSecret) {
		t.Errorf("GET client = %d %s, want no secret", page.Code, page.Body)
	}
	list := e.get(c, apiClientsURL)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), created.CallbackSecret) {
		t.Errorf("GET list = %d %s", list.Code, list.Body)
	}
	if got := decode[APIClientListView](t, list, "list", http.StatusOK).Items; len(got) != 1 || got[0] != (APIClientRowView{Name: "fastgateway"}) {
		t.Errorf("items = %+v", got)
	}

	// The secret is stored sealed, and opens.
	row, found, err := e.st.APIClientByName(ctx, "fastgateway")
	if err != nil || !found {
		t.Fatalf("APIClientByName = %v, %v", found, err)
	}
	if opened, err := e.d.Auth.box.Open(row.CallbackSecretSealed); err != nil || string(opened) != created.CallbackSecret {
		t.Errorf("stored secret opens to %q, %v; want the one shown", opened, err)
	}
	if strings.Contains(string(row.CallbackSecretSealed), created.CallbackSecret) {
		t.Error("the secret is stored in the clear")
	}
	for _, a := range e.auditEntries() {
		if strings.Contains(a.Detail, "pgs_") {
			t.Errorf("audit %s holds a secret: %q", a.Action, a.Detail)
		}
	}
	if got := auditDetails(e, "api_client_created"); len(got) != 1 || got[0] != "fastgateway" {
		t.Errorf("api_client_created = %v", got)
	}
	if strings.Contains(e.logs.String(), created.CallbackSecret) {
		t.Error("the secret reached the log")
	}

	// The same name again is a conflict.
	rec := e.sendJSON(c, "POST", apiClientsURL, goodClient(nil))
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgAPIClientExists {
		t.Errorf("duplicate POST = %d %s, want 409", rec.Code, rec.Body)
	}
}

// Every route is "not found" for a user, with the body of an unknown path.
func TestAPIClientRoutesAre404ForUsers(t *testing.T) {
	e := newEnv(t, nil)
	e.addAPIClient(e.signInOperator(), goodClient(nil))
	alice := e.aliceSession()
	for _, r := range []struct{ method, target string }{
		{"GET", apiClientsURL},
		{"POST", apiClientsURL},
		{"GET", apiClientsURL + "/fastgateway"},
		{"PUT", apiClientsURL + "/fastgateway"},
		{"DELETE", apiClientsURL + "/fastgateway"},
		{"POST", apiClientsURL + "/fastgateway/secret"},
		{"POST", apiClientsURL + "/fastgateway/keys"},
		{"DELETE", apiClientsURL + "/fastgateway/keys/1"},
	} {
		var rec *httptest.ResponseRecorder
		if r.method == "GET" {
			rec = e.get(alice, r.target)
		} else {
			rec = e.send(alice, r.method, r.target, `{}`)
		}
		if rec.Code != http.StatusNotFound || errorOf(t, rec) != msgNotFound {
			t.Errorf("%s %s as a user = %d %s, want 404 %q", r.method, r.target, rec.Code, rec.Body, msgNotFound)
		}
	}
	if _, found, _ := e.st.APIClientByName(ctx, "fastgateway"); !found {
		t.Error("a user removed the client")
	}
}

// A key is in the answer that makes it, starts pgk_<prefix>_, and is stored
// as a hash; the list shows the prefix and the audit entry names the key and
// the prefix and never the key.
func TestCreateAPIKeyShownOnce(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	e.addAPIClient(c, goodClient(nil))

	made := decode[APIKeyCreatedView](t, e.sendJSON(c, "POST", apiClientsURL+"/fastgateway/keys", map[string]any{"name": "ci"}),
		"POST key", http.StatusCreated)
	if len(made.Key.Prefix) != 8 || !strings.HasPrefix(made.Secret, "pgk_"+made.Key.Prefix+"_") {
		t.Errorf("key = %+v, secret %q; want pgk_<prefix>_ ahead of the secret", made.Key, made.Secret)
	}
	if made.Key.Name != "ci" || made.Key.ExpiresAt != nil || made.Key.LastUsedAt != nil {
		t.Errorf("key = %+v", made.Key)
	}

	page := e.get(c, apiClientsURL+"/fastgateway")
	if strings.Contains(page.Body.String(), made.Secret) || strings.Contains(page.Body.String(), "pgk_") {
		t.Errorf("GET client holds a key: %s", page.Body)
	}
	got := decode[APIClientView](t, page, "GET client", http.StatusOK)
	if len(got.Keys) != 1 || got.Keys[0].Prefix != made.Key.Prefix || got.Keys[0].ID != made.Key.ID {
		t.Errorf("keys = %+v, want the one made", got.Keys)
	}
	list := decode[APIClientListView](t, e.get(c, apiClientsURL), "list", http.StatusOK)
	if len(list.Items) != 1 || list.Items[0].Keys != 1 {
		t.Errorf("list = %+v, want 1 key", list.Items)
	}

	if want := "fastgateway: ci (" + made.Key.Prefix + ")"; len(auditDetails(e, "api_key_created")) != 1 || auditDetails(e, "api_key_created")[0] != want {
		t.Errorf("api_key_created = %v, want %q", auditDetails(e, "api_key_created"), want)
	}
	for _, a := range e.auditEntries() {
		if strings.Contains(a.Detail, "pgk_") {
			t.Errorf("audit %s holds a key: %q", a.Action, a.Detail)
		}
	}
	if strings.Contains(e.logs.String(), made.Secret) {
		t.Error("the key reached the log")
	}

	// The key is stored as its hash: nothing here can read it back.
	keys, err := e.st.APIKeys(ctx, mustClientID(t, e, "fastgateway"))
	if err != nil || len(keys) != 1 || len(keys[0].Hash) != 32 {
		t.Errorf("stored keys = %+v, %v; want a 32-byte hash", keys, err)
	}

	// The same name, in any case, is a conflict; another client may use it.
	rec := e.sendJSON(c, "POST", apiClientsURL+"/fastgateway/keys", map[string]any{"name": "CI"})
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgAPIKeyExists {
		t.Errorf("duplicate key = %d %s, want 409", rec.Code, rec.Body)
	}
	wantCode(t, e.sendJSON(c, "POST", apiClientsURL+"/nobody/keys", map[string]any{"name": "ci"}), "key of an unknown client", http.StatusNotFound)

	// Revoking removes it and is audited; another client's key is not found.
	e.addAPIClient(c, goodClient(func(m map[string]any) { m["name"] = "other" }))
	wantCode(t, e.send(c, "DELETE", apiClientsURL+"/other/keys/"+strconv.FormatInt(made.Key.ID, 10), ""), "revoke through another client", http.StatusNotFound)
	wantCode(t, e.send(c, "DELETE", apiClientsURL+"/fastgateway/keys/"+strconv.FormatInt(made.Key.ID, 10), ""), "revoke", http.StatusNoContent)
	wantCode(t, e.send(c, "DELETE", apiClientsURL+"/fastgateway/keys/"+strconv.FormatInt(made.Key.ID, 10), ""), "revoke again", http.StatusNotFound)
	wantCode(t, e.send(c, "DELETE", apiClientsURL+"/fastgateway/keys/x", ""), "revoke a key with a bad ID", http.StatusNotFound)
	if want := "fastgateway: ci (" + made.Key.Prefix + ")"; len(auditDetails(e, "api_key_revoked")) != 1 || auditDetails(e, "api_key_revoked")[0] != want {
		t.Errorf("api_key_revoked = %v, want %q", auditDetails(e, "api_key_revoked"), want)
	}
	if keys, _ := e.st.APIKeys(ctx, mustClientID(t, e, "fastgateway")); len(keys) != 0 {
		t.Errorf("%d keys after the revoke", len(keys))
	}
}

// A key's expiry must be in the future, and its name 1 to 100 characters.
func TestCreateAPIKeyRules(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	e.addAPIClient(c, goodClient(nil))
	url := apiClientsURL + "/fastgateway/keys"
	for name, tc := range map[string]struct {
		body map[string]any
		want string
	}{
		"empty name":   {map[string]any{"name": ""}, msgAPIKeyName},
		"blank name":   {map[string]any{"name": "   "}, msgAPIKeyName},
		"long name":    {map[string]any{"name": strings.Repeat("k", 101)}, msgAPIKeyName},
		"past expiry":  {map[string]any{"name": "ci", "expiresAt": e.clock.now().Add(-time.Hour)}, msgAPIKeyExpiry},
		"now expiry":   {map[string]any{"name": "ci", "expiresAt": e.clock.now()}, msgAPIKeyExpiry},
		"unknown part": {map[string]any{"name": "ci", "secret": "x"}, msgBadAPIKey},
	} {
		if rec := e.sendJSON(c, "POST", url, tc.body); rec.Code != http.StatusBadRequest || errorOf(t, rec) != tc.want {
			t.Errorf("%s = %d %s, want 400 %q", name, rec.Code, rec.Body, tc.want)
		}
	}
	// 100 characters of 3 bytes each is fine, and so is an expiry ahead.
	soon := e.clock.now().Add(time.Hour)
	made := decode[APIKeyCreatedView](t, e.sendJSON(c, "POST", url, map[string]any{"name": strings.Repeat("é", 100), "expiresAt": soon}),
		"POST key", http.StatusCreated)
	if made.Key.ExpiresAt == nil || !made.Key.ExpiresAt.Equal(soon) {
		t.Errorf("expiresAt = %v, want %v", made.Key.ExpiresAt, soon)
	}
}

func mustClientID(t *testing.T, e *env, name string) int64 {
	t.Helper()
	c, found, err := e.st.APIClientByName(ctx, name)
	if err != nil || !found {
		t.Fatalf("APIClientByName(%s) = %v, %v", name, found, err)
	}
	return c.ID
}

// A rotation seals a new secret, keeps the old one for 24 hours as the
// previous, and never puts a secret in the audit log.
func TestRotateSecretKeepsOld24h(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	created := e.addAPIClient(c, goodClient(nil))
	if created.Client.PreviousSecretUntil != nil {
		t.Errorf("a new client has previousSecretUntil %v", created.Client.PreviousSecretUntil)
	}

	rotated := decode[APICallbackSecretView](t, e.send(c, "POST", apiClientsURL+"/fastgateway/secret", ""), "rotate", http.StatusOK)
	if !strings.HasPrefix(rotated.CallbackSecret, "pgs_") || rotated.CallbackSecret == created.CallbackSecret {
		t.Errorf("rotated secret = %q, want a new pgs_ secret", rotated.CallbackSecret)
	}
	page := decode[APIClientView](t, e.get(c, apiClientsURL+"/fastgateway"), "GET", http.StatusOK)
	if want := e.clock.now().Add(24 * time.Hour); page.PreviousSecretUntil == nil || !page.PreviousSecretUntil.Equal(want) {
		t.Errorf("previousSecretUntil = %v, want %v", page.PreviousSecretUntil, want)
	}

	row, _, _ := e.st.APIClientByName(ctx, "fastgateway")
	current, err1 := e.d.Auth.box.Open(row.CallbackSecretSealed)
	previous, err2 := e.d.Auth.box.Open(row.PreviousSecretSealed)
	if err1 != nil || err2 != nil || string(current) != rotated.CallbackSecret || string(previous) != created.CallbackSecret {
		t.Errorf("stored secrets = %q, %q (%v, %v); want the new one and the old one", current, previous, err1, err2)
	}
	for _, a := range e.auditEntries() {
		if strings.Contains(a.Detail, "pgs_") {
			t.Errorf("audit %s holds a secret: %q", a.Action, a.Detail)
		}
	}
	if got := auditDetails(e, "api_callback_secret_rotated"); len(got) != 1 || got[0] != "fastgateway" {
		t.Errorf("api_callback_secret_rotated = %v", got)
	}
	if strings.Contains(e.logs.String(), rotated.CallbackSecret) || strings.Contains(e.logs.String(), created.CallbackSecret) {
		t.Error("a secret reached the log")
	}

	// Once the 24 hours are over the previous secret is no longer shown. (The
	// stored time moves back; the clock stays, as the session would expire.)
	if _, _, err := e.st.UpdateAPIClient(ctx, "fastgateway", func(c *store.APIClient) {
		c.PreviousSecretUntil = e.clock.now().Add(-time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	page = decode[APIClientView](t, e.get(c, apiClientsURL+"/fastgateway"), "GET later", http.StatusOK)
	if page.PreviousSecretUntil != nil {
		t.Errorf("previousSecretUntil = %v after it passed", page.PreviousSecretUntil)
	}
	wantCode(t, e.send(c, "POST", apiClientsURL+"/nobody/secret", ""), "rotate of an unknown client", http.StatusNotFound)
}

// A PUT replaces the settings and leaves the keys and the secret alone.
func TestUpdateAPIClientReplacesSettings(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	created := e.addAPIClient(c, goodClient(nil))
	made := decode[APIKeyCreatedView](t, e.sendJSON(c, "POST", apiClientsURL+"/fastgateway/keys", map[string]any{"name": "ci"}), "key", http.StatusCreated)

	body := map[string]any{"skills": []string{"security", "style"}, "modelId": "", "callbackUrl": "",
		"limits": goodLimits(func(m map[string]any) { m["maxFiles"] = 50 })}
	got := decode[APIClientView](t, e.sendJSON(c, "PUT", apiClientsURL+"/fastgateway", body), "PUT", http.StatusOK)
	if len(got.Skills) != 2 || got.CallbackURL != "" || got.Limits.MaxFiles != 50 || got.Name != "fastgateway" {
		t.Errorf("client = %+v", got)
	}
	if len(got.Keys) != 1 || got.Keys[0].ID != made.Key.ID {
		t.Errorf("keys = %+v, want the key kept", got.Keys)
	}
	row, _, _ := e.st.APIClientByName(ctx, "fastgateway")
	if opened, err := e.d.Auth.box.Open(row.CallbackSecretSealed); err != nil || string(opened) != created.CallbackSecret {
		t.Errorf("the secret changed: %q, %v", opened, err)
	}
	if got := auditDetails(e, "api_client_updated"); len(got) != 1 || got[0] != "fastgateway" {
		t.Errorf("api_client_updated = %v", got)
	}

	// limits is required; the name may not be sent; an unknown client is 404.
	if rec := e.sendJSON(c, "PUT", apiClientsURL+"/fastgateway", map[string]any{"skills": []string{}}); rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgAPILimits {
		t.Errorf("PUT without limits = %d %s", rec.Code, rec.Body)
	}
	if rec := e.sendJSON(c, "PUT", apiClientsURL+"/fastgateway", map[string]any{"name": "x", "limits": goodLimits(nil)}); rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgBadAPIClientEdit {
		t.Errorf("PUT with a name = %d %s", rec.Code, rec.Body)
	}
	wantCode(t, e.sendJSON(c, "PUT", apiClientsURL+"/nobody", map[string]any{"limits": goodLimits(nil)}), "PUT unknown", http.StatusNotFound)
}

// Bad input is 400 with a message, and nothing is stored. The checks run
// before the store, so they need no database.
func TestAPIClientInputRules(t *testing.T) {
	cfg := &config.Config{
		Dashboard: config.Dashboard{URL: dashboardURL},
		LLM:       config.LLM{Provider: "anthropic", Model: "claude-sonnet-4"},
	}
	d, err := New(Options{Config: cfg, Instances: instances.New(cfg, nil, nil), SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		body map[string]any
		want string
	}{
		"upper-case name":        {goodClient(func(m map[string]any) { m["name"] = "FastGateway" }), msgAPIName},
		"underscore name":        {goodClient(func(m map[string]any) { m["name"] = "fast_gateway" }), msgAPIName},
		"empty name":             {goodClient(func(m map[string]any) { m["name"] = "" }), msgAPINameLen},
		"long name":              {goodClient(func(m map[string]any) { m["name"] = strings.Repeat("a", 64) }), msgAPINameLen},
		"bad skill name":         {goodClient(func(m map[string]any) { m["skills"] = []string{"../x"} }), msgAPISkillName},
		"empty skill name":       {goodClient(func(m map[string]any) { m["skills"] = []string{""} }), msgAPISkillName},
		"skill twice":            {goodClient(func(m map[string]any) { m["skills"] = []string{"a", "a"} }), msgAPISkillTwice},
		"unknown model":          {goodClient(func(m map[string]any) { m["modelId"] = "no-such" }), msgAPIModel},
		"ftp callback":           {goodClient(func(m map[string]any) { m["callbackUrl"] = "ftp://x.example.com/h" }), msgAPICallback},
		"relative callback":      {goodClient(func(m map[string]any) { m["callbackUrl"] = "/hooks" }), msgAPICallback},
		"no host callback":       {goodClient(func(m map[string]any) { m["callbackUrl"] = "https:///hooks" }), msgAPICallback},
		"port only callback":     {goodClient(func(m map[string]any) { m["callbackUrl"] = "http://:8080/hooks" }), msgAPICallback},
		"user info callback":     {goodClient(func(m map[string]any) { m["callbackUrl"] = "https://u:p@x.example.com/h" }), msgAPICallback},
		"user only callback":     {goodClient(func(m map[string]any) { m["callbackUrl"] = "https://u@x.example.com/h" }), msgAPICallback},
		"fragment callback":      {goodClient(func(m map[string]any) { m["callbackUrl"] = "https://x.example.com/h#top" }), msgAPICallback},
		"empty fragment":         {goodClient(func(m map[string]any) { m["callbackUrl"] = "https://x.example.com/h#" }), msgAPICallback},
		"long callback":          {goodClient(func(m map[string]any) { m["callbackUrl"] = "https://x.example.com/" + strings.Repeat("a", 2000) }), msgAPICallbackLen},
		"request bytes low":      {withLimits(func(m map[string]any) { m["maxRequestBytes"] = 1023 }), msgAPIRequestBytes},
		"request bytes high":     {withLimits(func(m map[string]any) { m["maxRequestBytes"] = 819201 }), msgAPIRequestBytes},
		"zero files":             {withLimits(func(m map[string]any) { m["maxFiles"] = 0 }), msgAPIFiles},
		"too many files":         {withLimits(func(m map[string]any) { m["maxFiles"] = 1001 }), msgAPIFiles},
		"zero in flight":         {withLimits(func(m map[string]any) { m["maxInFlight"] = 0 }), msgAPIInFlight},
		"too many in flight":     {withLimits(func(m map[string]any) { m["maxInFlight"] = 51 }), msgAPIInFlight},
		"zero per hour":          {withLimits(func(m map[string]any) { m["requestsPerHour"] = 0 }), msgAPIPerHour},
		"too many per hour":      {withLimits(func(m map[string]any) { m["requestsPerHour"] = 100001 }), msgAPIPerHour},
		"zero budget":            {withLimits(func(m map[string]any) { m["monthlyTokenBudget"] = 0 }), msgAPIBudget},
		"negative budget":        {withLimits(func(m map[string]any) { m["monthlyTokenBudget"] = -5 }), msgAPIBudget},
		"missing limit":          {goodClient(func(m map[string]any) { m["limits"] = map[string]any{"maxFiles": 10} }), msgAPIRequestBytes},
		"unknown field":          {goodClient(func(m map[string]any) { m["callbackSecret"] = "pgs_x" }), msgBadAPIClient},
		"limits of a wrong kind": {goodClient(func(m map[string]any) { m["limits"] = "many" }), msgBadAPIClient},
	} {
		rec := callAdminHandler(t, d.createAPIClient, tc.body)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != tc.want {
			t.Errorf("%s: POST = %d %s, want 400 %q", name, rec.Code, rec.Body, tc.want)
		}
	}

	// What passes: the config's model, a private callback address, a skill
	// the library lacks, and an empty everything.
	for name, body := range map[string]map[string]any{
		"config model":     goodClient(func(m map[string]any) { m["modelId"] = "config" }),
		"private callback": goodClient(func(m map[string]any) { m["callbackUrl"] = "http://pergent-hooks.internal.svc:8080/h?x=1" }),
		"no callback":      goodClient(func(m map[string]any) { m["callbackUrl"] = "" }),
		"limits at edges":  withLimits(func(m map[string]any) { m["maxRequestBytes"], m["maxFiles"], m["maxInFlight"] = 1024, 1000, 50 }),
		"no skills":        goodClient(func(m map[string]any) { delete(m, "skills") }),
	} {
		var in APIClientInput
		if err := decodeStrict(httptest.NewRequest("POST", "/", strings.NewReader(mustJSON(t, body))), &in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, msg := d.checkAPIClientFields(in.Skills, in.ModelID, in.CallbackURL, in.Limits, false); msg != "" {
			t.Errorf("%s: refused: %s", name, msg)
		}
	}
}

func withLimits(edit func(m map[string]any)) map[string]any {
	return goodClient(func(m map[string]any) { m["limits"] = goodLimits(edit) })
}

// callAdminHandler calls h as the router would after the admin gate.
func callAdminHandler(t *testing.T, h http.HandlerFunc, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/", strings.NewReader(mustJSON(t, body)))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// Removing a client takes its keys, and is audited; the Jobs of its live
// reviews are deleted one by one, and a Job that cannot be deleted is logged
// and does not stop the others.
func TestDeleteAPIClientAudits(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	e.addAPIClient(c, goodClient(nil))
	e.sendJSON(c, "POST", apiClientsURL+"/fastgateway/keys", map[string]any{"name": "ci"})

	wantCode(t, e.send(c, "DELETE", apiClientsURL+"/fastgateway", ""), "DELETE", http.StatusNoContent)
	wantCode(t, e.get(c, apiClientsURL+"/fastgateway"), "GET after", http.StatusNotFound)
	wantCode(t, e.send(c, "DELETE", apiClientsURL+"/fastgateway", ""), "DELETE again", http.StatusNotFound)
	if got := auditDetails(e, "api_client_removed"); len(got) != 1 || got[0] != "fastgateway" {
		t.Errorf("api_client_removed = %v", got)
	}
	if got := decode[APIClientListView](t, e.get(c, apiClientsURL), "list", http.StatusOK).Items; len(got) != 0 {
		t.Errorf("items = %+v after the delete", got)
	}
}

// removeJobs needs no database.
func TestRemoveJobsIsBestEffort(t *testing.T) {
	cfg := &config.Config{Dashboard: config.Dashboard{URL: dashboardURL}}
	logs := &lockedBuffer{}
	var deleted []string
	d, err := New(Options{Config: cfg, SecretKey: testKey(t), Log: slogTo(logs),
		DeleteJob: func(_ context.Context, name string) error {
			deleted = append(deleted, name)
			if name == "job-1" {
				return errors.New("the API server is away")
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	d.removeJobs(context.Background(), "fastgateway", nil)
	if len(deleted) != 0 {
		t.Errorf("deleted %v for no jobs", deleted)
	}
	d.removeJobs(context.Background(), "fastgateway", []string{"job-1", "job-2"})
	if strings.Join(deleted, ",") != "job-1,job-2" {
		t.Errorf("deleted %v, want both, the failure first", deleted)
	}
	if !strings.Contains(logs.String(), "job-1") || !strings.Contains(logs.String(), "the API server is away") {
		t.Errorf("the failure is not logged: %s", logs)
	}

	// No DeleteJob: nothing to call, nothing to fail.
	d.deleteJob = nil
	d.removeJobs(context.Background(), "fastgateway", []string{"job-3"})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func slogTo(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }
