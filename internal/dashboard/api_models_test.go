package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	modelsURL    = "/api/admin/models"
	gadgetsURL   = "/api/repositories/main/acme/gadgets"
	modelBaseURL = "https://llm-gateway.example.com/v1"
	llmKeyThree  = "llm-api-key-SECRET-999"
)

// addModel stores an entry of the list as the API would, and reads the list
// into the registry.
func (e *env) addModel(provider, model, baseURL, key string) store.LLMModel {
	e.t.Helper()
	m, err := e.st.CreateLLMModel(ctx, store.LLMModel{Provider: provider, Model: model, BaseURL: baseURL,
		APIKeySealed: e.d.Auth.box.Seal([]byte(key))}, e.clock.now())
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.reg.LoadLLM(ctx); err != nil {
		e.t.Fatal(err)
	}
	// The list is ordered by when entries were added.
	e.clock.advance(time.Second)
	return m
}

func modelNamed(t *testing.T, v ModelsAdminView, id string) ModelAdminView {
	t.Helper()
	i := slices.IndexFunc(v.Models, func(m ModelAdminView) bool { return m.ID == id })
	if i < 0 {
		t.Fatalf("model %s is not in %+v", id, v.Models)
	}
	return v.Models[i]
}

func auditDetails(e *env, action string) []string {
	var out []string
	for _, a := range e.auditEntries() {
		if a.Action == action {
			out = append(out, a.Detail)
		}
	}
	slices.Reverse(out) // oldest first
	return out
}

// An admin adds entries, makes another one the default, and removes one that
// is not; the first entry is the default, a duplicate and a different
// provider or model on a change are refused, and every change is audited with
// the provider and model.
func TestModelsAdminLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()

	one := decode[ModelAdminView](t, e.sendJSON(c, "POST", modelsURL,
		map[string]any{"provider": "anthropic", "model": "claude-sonnet-4", "apiKey": llmKeyOne}), "POST one", http.StatusCreated)
	two := decode[ModelAdminView](t, e.sendJSON(c, "POST", modelsURL,
		map[string]any{"provider": "openai", "model": "gpt-x", "baseURL": modelBaseURL, "apiKey": llmKeyTwo}), "POST two", http.StatusCreated)
	if !one.Default || two.Default {
		t.Errorf("default flags = %v, %v; want the first entry the default and not the second", one.Default, two.Default)
	}

	// The same provider, model and base URL is refused; another base URL is
	// another entry.
	rec := e.sendJSON(c, "POST", modelsURL, map[string]any{"provider": "openai", "model": "gpt-x", "baseURL": modelBaseURL, "apiKey": "k"})
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgModelExists {
		t.Errorf("duplicate POST = %d %s, want 409", rec.Code, rec.Body)
	}
	decode[ModelAdminView](t, e.sendJSON(c, "POST", modelsURL,
		map[string]any{"provider": "openai", "model": "gpt-x", "apiKey": llmKeyThree}), "POST same model, no base URL", http.StatusCreated)

	// The provider and model of an entry never change.
	for _, body := range []map[string]any{
		{"baseURL": "", "provider": "openai"},
		{"baseURL": "", "model": "gpt-y"},
		{"provider": "openai", "model": "gpt-y"},
		{},
	} {
		if rec := e.sendJSON(c, "PUT", modelsURL+"/"+two.ID, body); rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgBadModelEdit {
			t.Errorf("PUT %v = %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	wantCode(t, e.sendJSON(c, "PUT", modelsURL+"/no-such-entry", map[string]any{"baseURL": ""}), "PUT unknown", http.StatusNotFound)
	// A base URL that makes it the entry added last is a duplicate.
	rec = e.sendJSON(c, "PUT", modelsURL+"/"+two.ID, map[string]any{"baseURL": ""})
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgModelExists {
		t.Errorf("PUT into a duplicate = %d %s, want 409", rec.Code, rec.Body)
	}

	// Another default; the list says so, and the registry follows.
	list := decode[ModelsAdminView](t, e.send(c, "POST", modelsURL+"/"+two.ID+"/default", ""), "default", http.StatusOK)
	if !modelNamed(t, list, two.ID).Default || modelNamed(t, list, one.ID).Default {
		t.Errorf("after the default changed = %+v", list.Models)
	}
	if m, ok := e.reg.DefaultModel(); !ok || m.ID != two.ID {
		t.Errorf("registry default = %+v, %v", m, ok)
	}
	// Again changes nothing and is not audited a second time, but reads the
	// list back, so a retry after a failed read-back gets the engine current.
	reads, load := 0, e.d.loadLLM
	e.d.loadLLM = func(ctx context.Context) error { reads++; return load(ctx) }
	decode[ModelsAdminView](t, e.send(c, "POST", modelsURL+"/"+two.ID+"/default", ""), "default again", http.StatusOK)
	e.d.loadLLM = load
	if reads != 1 {
		t.Errorf("default again read the list back %d times, want 1", reads)
	}
	wantCode(t, e.send(c, "POST", modelsURL+"/no-such-entry/default", ""), "default of an unknown entry", http.StatusNotFound)

	// The default can't go; another can.
	rec = e.send(c, "DELETE", modelsURL+"/"+two.ID, "")
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgModelIsDefault {
		t.Errorf("DELETE of the default = %d %s, want 409", rec.Code, rec.Body)
	}
	wantCode(t, e.send(c, "DELETE", modelsURL+"/"+one.ID, ""), "DELETE", http.StatusNoContent)
	wantCode(t, e.send(c, "DELETE", modelsURL+"/"+one.ID, ""), "DELETE again", http.StatusNotFound)
	if _, ok := e.reg.ModelByID(one.ID); ok {
		t.Error("the removed entry is still in the registry")
	}

	if got := auditDetails(e, "model_added"); !slices.Equal(got, []string{"anthropic / claude-sonnet-4", "openai / gpt-x", "openai / gpt-x"}) {
		t.Errorf("model_added details = %q", got)
	}
	if got := auditDetails(e, "model_default_changed"); !slices.Equal(got, []string{"openai / gpt-x (was anthropic / claude-sonnet-4)"}) {
		t.Errorf("model_default_changed details = %q", got)
	}
	if got := auditDetails(e, "model_removed"); !slices.Equal(got, []string{"anthropic / claude-sonnet-4"}) {
		t.Errorf("model_removed details = %q", got)
	}
}

// No response, log line or audit entry holds a key entered for a model, the
// one a change replaced included.
func TestModelsNeverReturnAKey(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	one := decode[ModelAdminView](t, e.sendJSON(c, "POST", modelsURL,
		map[string]any{"provider": "anthropic", "model": "m", "baseURL": modelBaseURL, "apiKey": llmKeyOne}), "POST", http.StatusCreated)
	e.sendJSON(c, "PUT", modelsURL+"/"+one.ID, map[string]any{"baseURL": modelBaseURL, "apiKey": llmKeyTwo})
	two := decode[ModelAdminView](t, e.sendJSON(c, "POST", modelsURL,
		map[string]any{"provider": "openai", "model": "m", "apiKey": llmKeyThree}), "POST", http.StatusCreated)
	e.send(c, "POST", modelsURL+"/"+two.ID+"/default", "")
	// Failing writes echo nothing either.
	e.sendJSON(c, "POST", modelsURL, map[string]any{"provider": "anthropic", "model": "m", "baseURL": modelBaseURL, "apiKey": "dup-" + llmKeyOne})
	e.sendJSON(c, "PUT", modelsURL+"/"+one.ID, map[string]any{"baseURL": "not a url", "apiKey": "bad-" + llmKeyOne})
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) { r.ModelID = &one.ID })
	e.send(c, "DELETE", modelsURL+"/"+one.ID, "")
	for _, target := range []string{modelsURL, gadgetsURL, "/api/admin/settings", "/api/audit"} {
		e.get(c, target)
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
	for _, secret := range []string{llmKeyOne, llmKeyTwo, llmKeyThree, "dup-" + llmKeyOne, "bad-" + llmKeyOne} {
		for _, body := range bodies {
			if strings.Contains(body, secret) {
				t.Fatalf("the key %q is in a response, log or audit entry:\n%s", secret, body)
			}
		}
	}
}

// Only admins see or change the list: anyone else gets what an unknown path
// gets, whatever the method.
func TestModelsRoutesAreAdminsOnly(t *testing.T) {
	e := newEnv(t, nil)
	m := e.addModel("anthropic", "m", "", llmKeyOne)
	unknown := e.get(e.userOnMain(false, "acme/widgets"), "/api/admin/no-such-route")
	for name, c := range map[string]*http.Cookie{
		"viewer":  e.aliceSession(),
		"waiting": e.sessionFor(e.sso("waiting@example.com")),
	} {
		for _, tc := range []struct{ method, target, body string }{
			{"GET", modelsURL, ""},
			{"POST", modelsURL, `{"provider":"p","model":"m","apiKey":"k"}`},
			{"PUT", modelsURL + "/" + m.ID, `{"baseURL":""}`},
			{"POST", modelsURL + "/" + m.ID + "/default", ""},
			{"DELETE", modelsURL + "/" + m.ID, ""},
		} {
			var rec *httptest.ResponseRecorder
			if tc.method == "GET" {
				rec = e.get(c, tc.target)
			} else {
				rec = e.send(c, tc.method, tc.target, tc.body)
			}
			if rec.Code != http.StatusNotFound || rec.Body.String() != unknown.Body.String() {
				t.Errorf("%s %s as a %s = %d %s, want the 404 of an unknown path (%s)", tc.method, tc.target, name, rec.Code, rec.Body, unknown.Body)
			}
		}
	}
	rows, err := e.st.LLMModels(ctx)
	if err != nil || len(rows) != 1 || rows[0].BaseURL != "" {
		t.Errorf("stored models = %+v, %v; want the one entry untouched", rows, err)
	}
}

// The default is moved from and to: the list says how many repositories
// follow it and which name each entry.
func TestSetDefaultReportsFollowers(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	one := e.addModel("anthropic", "claude-sonnet-4", "", llmKeyOne)
	two := e.addModel("openai", "gpt-x", "", llmKeyTwo)
	// The three repositories of the config follow the default, and so does
	// gadgets; gizmos names the second entry.
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.dashboardRepo("main", "acme/gizmos", func(r *store.DashboardRepo) { r.ModelID = &two.ID })

	v := getJSON[ModelsAdminView](e, c, modelsURL)
	if v.FollowingDefault != 4 {
		t.Errorf("followingDefault = %d, want 4", v.FollowingDefault)
	}
	if got := modelNamed(t, v, two.ID).Repositories; !slices.Equal(got, []RepoRefView{{Instance: "main", Project: "acme/gizmos"}}) {
		t.Errorf("repositories naming the second entry = %+v", got)
	}
	if got := modelNamed(t, v, one.ID).Repositories; len(got) != 0 {
		t.Errorf("repositories naming the first entry = %+v, want none (and [] in JSON)", got)
	}
	if !strings.Contains(e.get(c, modelsURL).Body.String(), `"repositories":[]`) {
		t.Error("an entry no repository names must list [], not null")
	}

	// The answer to the change carries the same count, and the repositories
	// that follow the default now run on the new one.
	after := decode[ModelsAdminView](t, e.send(c, "POST", modelsURL+"/"+two.ID+"/default", ""), "default", http.StatusOK)
	if after.FollowingDefault != 4 || !modelNamed(t, after, two.ID).Default {
		t.Errorf("after the change = %+v", after)
	}
	if got := getJSON[RepositoryDetailView](e, c, gadgetsURL).Model; got != "openai / gpt-x" {
		t.Errorf("gadgets runs on %q after the default moved, want openai / gpt-x", got)
	}
}

// A model a repository names is not removed: the answer names the
// repositories, and removing it works once none does.
func TestRemoveInUseNamesRepositories(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	e.addModel("anthropic", "claude-sonnet-4", "", llmKeyOne)
	two := e.addModel("openai", "gpt-x", "", llmKeyTwo)
	e.dashboardRepo("main", "acme/gizmos", func(r *store.DashboardRepo) { r.ModelID = &two.ID })
	e.dashboardRepo("gh", "acme/Aardvark", func(r *store.DashboardRepo) { r.ModelID = &two.ID })

	rec := e.send(c, "DELETE", modelsURL+"/"+two.ID, "")
	v := decode[ModelInUseView](t, rec, "DELETE", http.StatusConflict)
	want := []RepoRefView{{Instance: "gh", Project: "acme/Aardvark"}, {Instance: "main", Project: "acme/gizmos"}}
	if v.Error != msgModelInUse || !slices.Equal(v.Repositories, want) {
		t.Errorf("409 = %+v, want %q and %+v", v, msgModelInUse, want)
	}
	if len(auditDetails(e, "model_removed")) != 0 {
		t.Error("a refused removal was audited")
	}
	if _, ok := e.reg.ModelByID(two.ID); !ok {
		t.Error("the refused removal took the entry out of the registry")
	}

	// Repositories moved off it, the entry goes.
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/gizmos/settings", `{"model":null}`), "clear model of gizmos", http.StatusOK)
	wantCode(t, e.send(c, "PUT", "/api/repositories/gh/acme/Aardvark/settings", `{"model":null}`), "clear model of Aardvark", http.StatusOK)
	wantCode(t, e.send(c, "DELETE", modelsURL+"/"+two.ID, ""), "DELETE", http.StatusNoContent)
}

// An admin of the repository names a model of the list, and null puts it
// back to the default; the audit entry says which model.
func TestAdminSetsAndClearsModel(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.addModel("anthropic", "claude-sonnet-4", "", llmKeyOne)
	two := e.addModel("openai", "gpt-x", modelBaseURL, llmKeyTwo)
	c := e.signInRole(store.RoleAdmin)

	v := getJSON[RepositoryDetailView](e, c, gadgetsURL)
	if v.Model != "anthropic / claude-sonnet-4" || v.Settings["model"].Overridden || v.Settings["model"].Value != nil {
		t.Errorf("before: model %q, setting %+v; want the default's, not overridden, null", v.Model, v.Settings["model"])
	}

	rec := e.send(c, "PUT", gadgetsSettings, `{"model":"`+two.ID+`"}`)
	v = decode[RepositoryDetailView](t, rec, "PUT model", http.StatusOK)
	if v.Model != "openai / gpt-x" || !v.Settings["model"].Overridden || v.Settings["model"].Value != two.ID {
		t.Errorf("after naming it: model %q, setting %+v", v.Model, v.Settings["model"])
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.ModelID == nil || *r.ModelID != two.ID {
		t.Errorf("stored model = %v, want %s", r.ModelID, two.ID)
	}
	// The first entry added is the default, and the choices say so.
	if len(v.ModelChoices) != 2 || !v.ModelChoices[0].Default || v.ModelChoices[1] != (ModelChoiceView{ID: two.ID, Label: "openai / gpt-x"}) {
		t.Errorf("choices = %+v", v.ModelChoices)
	}

	v = decode[RepositoryDetailView](t, e.send(c, "PUT", gadgetsSettings, `{"approve":false,"model":null}`), "PUT null", http.StatusOK)
	if v.Model != "anthropic / claude-sonnet-4" || v.Settings["model"].Overridden {
		t.Errorf("after clearing: model %q, setting %+v", v.Model, v.Settings["model"])
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.ModelID != nil {
		t.Errorf("stored model = %v, want none", *r.ModelID)
	}

	if got := auditDetails(e, "settings_updated"); !slices.Equal(got, []string{"model (openai / gpt-x)", "approve, model (default)"}) {
		t.Errorf("settings_updated details = %q", got)
	}
}

// A maintainer can't give a repository a model, alone or with settings they
// may change, and nothing of the request is saved.
func TestMaintainerCannotSetModel(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	m := e.addModel("anthropic", "claude-sonnet-4", "", llmKeyOne)
	c := e.signInRole(store.RoleMaintainer)

	for _, body := range []string{`{"model":"` + m.ID + `","approve":false}`, `{"model":null}`, `{"model":"` + m.ID + `"}`} {
		rec := e.send(c, "PUT", gadgetsSettings, body)
		if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgNeedsAdminModel {
			t.Errorf("PUT %s = %d %s, want 403 %q", body, rec.Code, rec.Body, msgNeedsAdminModel)
		}
	}
	r, _ := e.storedRepo("main", "acme/gadgets")
	if r.ModelID != nil || r.Approve != nil {
		t.Errorf("stored = model %v, approve %v; want nothing saved", r.ModelID, r.Approve)
	}
	if got := auditDetails(e, "settings_updated"); len(got) != 0 {
		t.Errorf("a refused change was audited: %q", got)
	}
	// What they may change still works.
	wantCode(t, e.send(c, "PUT", gadgetsSettings, `{"approve":false}`), "approve", http.StatusOK)
}

// A model that is not in the list, or is not an ID, is 400 and nothing
// changes: the entry the config supplies counts only while the config has an
// llm: block.
func TestUnknownModelIs400(t *testing.T) {
	e := newEnv(t, nil)
	e.dashboardRepo("main", "acme/gadgets", nil)
	e.addModel("anthropic", "claude-sonnet-4", "", llmKeyOne)
	c := e.signInRole(store.RoleAdmin)

	for _, body := range []string{
		`{"model":"no-such-entry"}`,
		`{"model":"config"}`,
		`{"model":""}`,
		`{"model":7}`,
		`{"model":["a"]}`,
		`{"model":"` + strings.Repeat("x", 300) + `"}`,
		`{"approve":false,"model":"no-such-entry"}`,
	} {
		rec := e.send(c, "PUT", gadgetsSettings, body)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgUnknownModel {
			t.Errorf("PUT %.60s = %d %s, want 400 %q", body, rec.Code, rec.Body, msgUnknownModel)
		}
	}
	r, _ := e.storedRepo("main", "acme/gadgets")
	if r.ModelID != nil || r.Approve != nil {
		t.Errorf("stored = model %v, approve %v; want nothing saved", r.ModelID, r.Approve)
	}
	if !strings.Contains(msgBadSettings, "model") {
		t.Errorf("msgBadSettings = %q, want it to name model", msgBadSettings)
	}
}

// With an llm: block in the config its entry is there to name.
func TestConfigModelCanBeNamed(t *testing.T) {
	t.Setenv("PERGENT_TEST_LLM_KEY", llmKeyOne)
	e := newEnv(t, func(cfg *config.Config) {
		cfg.LLM = config.LLM{Provider: "anthropic", Model: "from-config", APIKeyEnv: "PERGENT_TEST_LLM_KEY"}
	})
	e.dashboardRepo("main", "acme/gadgets", nil)
	c := e.signInRole(store.RoleAdmin)
	v := decode[RepositoryDetailView](t, e.send(c, "PUT", gadgetsSettings, `{"model":"config"}`), "PUT config", http.StatusOK)
	if v.Model != "anthropic / from-config" || v.Settings["model"].Value != "config" {
		t.Errorf("model %q, setting %+v", v.Model, v.Settings["model"])
	}
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.ModelID == nil || *r.ModelID != "config" {
		t.Errorf("stored model = %v", r.ModelID)
	}
}

// A maintainer's reset leaves the model, as the caps and the trusted authors;
// an admin's puts it back to the default and the audit entry says which it was.
func TestResetByMaintainerKeepsModel(t *testing.T) {
	e := newEnv(t, nil)
	m := e.addModel("openai", "gpt-x", "", llmKeyOne)
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) { r.ModelID = &m.ID })
	c := e.signInRole(store.RoleMaintainer)
	wantCode(t, e.send(c, "POST", gadgetsSettings+"/reset", ""), "maintainer reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.ModelID == nil || *r.ModelID != m.ID {
		t.Errorf("after a maintainer's reset the model = %v, want %s kept", r.ModelID, m.ID)
	}
	if got := auditDetails(e, "settings_reset"); !slices.Equal(got, []string{""}) {
		t.Errorf("settings_reset details = %q", got)
	}

	e.aliceOn("main", store.RoleAdmin)
	admin := e.aliceSession()
	wantCode(t, e.send(admin, "POST", gadgetsSettings+"/reset", ""), "admin reset", http.StatusOK)
	if r, _ := e.storedRepo("main", "acme/gadgets"); r.ModelID != nil {
		t.Errorf("after an admin's reset the model = %v, want none", *r.ModelID)
	}
	if got := auditDetails(e, "settings_reset"); !slices.Equal(got, []string{"", "model (default, was openai / gpt-x)"}) {
		t.Errorf("settings_reset details = %q", got)
	}
}

// Anyone who sees a repository sees the name of its model, never the base URL
// or key, and has no choices to make.
func TestViewerSeesModelNameNotBaseURL(t *testing.T) {
	e := newEnv(t, nil)
	e.addModel("anthropic", "claude-sonnet-4", "", llmKeyOne)
	m := e.addModel("openai", "gpt-x", modelBaseURL, llmKeyTwo)
	e.dashboardRepo("main", "acme/gadgets", func(r *store.DashboardRepo) { r.ModelID = &m.ID })
	e.dashboardRepo("main", "acme/gizmos", nil)
	viewer := e.userOnMain(false, "acme/gadgets", "acme/gizmos")

	rec := e.get(viewer, gadgetsURL)
	v := decode[RepositoryDetailView](t, rec, "GET", http.StatusOK)
	if v.Role != "viewer" || v.Model != "openai / gpt-x" {
		t.Errorf("role %q, model %q; want a viewer who sees the name", v.Role, v.Model)
	}
	if len(v.ModelChoices) != 0 || !strings.Contains(rec.Body.String(), `"modelChoices":[]`) {
		t.Errorf("a viewer's choices = %+v in %s, want []", v.ModelChoices, rec.Body)
	}
	if got := getJSON[RepositoryDetailView](e, viewer, "/api/repositories/main/acme/gizmos").Model; got != "anthropic / claude-sonnet-4" {
		t.Errorf("a repository that follows the default shows %q", got)
	}
	for _, secret := range []string{modelBaseURL, "llm-gateway", llmKeyOne, llmKeyTwo} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the repository shows %q: %s", secret, rec.Body)
		}
	}

	// An admin's choices are names too.
	admin := e.signInRole(store.RoleAdmin)
	rec = e.get(admin, gadgetsURL)
	for _, secret := range []string{modelBaseURL, "llm-gateway", llmKeyOne, llmKeyTwo} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("an admin's repository shows %q: %s", secret, rec.Body)
		}
	}
}
