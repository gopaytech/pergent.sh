package dashboard

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	adminChainURL = "/api/admin/chain"
	llmKeyFour    = "llm-api-key-SECRET-444"
)

// chainRefs are the source/id of each entry of v, in order.
func chainRefs(v ChainView) []string {
	out := make([]string, 0, len(v.Entries))
	for _, en := range v.Entries {
		out = append(out, en.Source+"/"+en.ID)
	}
	return out
}

// entryBody is a PUT chain body naming refs ("global/<id>" or "team/<id>").
func entryBody(refs ...string) map[string]any {
	entries := []map[string]string{}
	for _, r := range refs {
		source, id, _ := strings.Cut(r, "/")
		entries = append(entries, map[string]string{"source": source, "id": id})
	}
	return map[string]any{"entries": entries}
}

// markEntry stores a mark on ref, as a fallback would.
func (e *env) markEntry(ref store.EntryRef, class string, until *time.Time) {
	e.t.Helper()
	if err := e.st.SetMark(ctx, store.Mark{Ref: ref, Class: class, Until: until, SetAt: e.clock.now()}); err != nil {
		e.t.Fatal(err)
	}
}

// With no stored chain, the global chain is the default entry alone; with an
// llm: block in the config it is the config's entries, which can't be moved.
func TestAdminChainGetDefault(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	one := e.addModel("anthropic", "sonnet", "", llmKeyOne)
	e.addModel("openai", "gpt", "", llmKeyTwo)
	v := getJSON[ChainView](e, c, adminChainURL)
	if v.Scope != store.ScopeGlobal || v.Source != "global" || v.ReadOnly || !slices.Equal(chainRefs(v), []string{"global/" + one.ID}) {
		t.Fatalf("chain = %+v", v)
	}
	if en := v.Entries[0]; en.Position != 1 || en.Name != "anthropic/sonnet" || en.State != "ready" || en.Fixed {
		t.Errorf("entry = %+v", en)
	}
	if len(v.Choices) != 2 {
		t.Errorf("choices = %+v, want both entries", v.Choices)
	}

	t.Setenv("PERGENT_TEST_LLM_KEY", llmKeyThree)
	t.Setenv("PERGENT_TEST_LLM_KEY_2", llmKeyFour)
	e = newEnv(t, func(cfg *config.Config) {
		cfg.LLM = config.LLM{Provider: "anthropic", Model: "from-config", APIKeyEnv: "PERGENT_TEST_LLM_KEY"}
		cfg.LLMFallbacks = []config.LLM{{Provider: "openai", Model: "backup", APIKeyEnv: "PERGENT_TEST_LLM_KEY_2"}}
	})
	c = e.operator()
	e.addModel("openai", "gpt", "", llmKeyTwo)
	v = getJSON[ChainView](e, c, adminChainURL)
	if !slices.Equal(chainRefs(v), []string{"global/config", "global/config-1"}) {
		t.Fatalf("chain with a config llm: block = %+v", chainRefs(v))
	}
	for _, en := range v.Entries {
		if !en.Fixed {
			t.Errorf("config entry %s is not fixed", en.ID)
		}
	}
}

// An admin saves the global chain: more than 5 entries, an unknown or team
// entry, twice the same entry and a config entry moved are 400; the config
// entries may be sent at the front or left out. The first entry becomes the
// default, and the change is audited with the names in order.
func TestAdminChainPut(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	var ids []string
	for i, key := range []string{llmKeyOne, llmKeyTwo, llmKeyThree, llmKeyFour, "k5", "k6"} {
		ids = append(ids, e.addModel("openai", "gpt-"+string(rune('a'+i)), "", key).ID)
	}
	refs := func(ids ...string) []string {
		out := []string{}
		for _, id := range ids {
			out = append(out, "global/"+id)
		}
		return out
	}
	rec := e.sendJSON(c, "PUT", adminChainURL, entryBody(refs(ids...)...))
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgChainTooLong {
		t.Errorf("6 entries = %d %s", rec.Code, rec.Body)
	}
	for name, body := range map[string]map[string]any{
		"unknown":    entryBody("global/no-such-entry"),
		"team":       entryBody("team/" + ids[0]),
		"twice":      entryBody("global/"+ids[0], "global/"+ids[0]),
		"bad source": entryBody("other/" + ids[0]),
	} {
		if rec := e.sendJSON(c, "PUT", adminChainURL, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	wantCode(t, e.send(c, "PUT", adminChainURL, `{"entries":[{"source":"global","id":"x","extra":1}]}`), "unknown field", http.StatusBadRequest)

	v := decode[ChainView](t, e.sendJSON(c, "PUT", adminChainURL, entryBody(refs(ids[2], ids[0])...)), "PUT", http.StatusOK)
	if !slices.Equal(chainRefs(v), refs(ids[2], ids[0])) {
		t.Errorf("saved chain = %v", chainRefs(v))
	}
	if m, ok := e.reg.DefaultModel(); !ok || m.ID != ids[2] {
		t.Errorf("default after the save = %+v, %v; want position 1", m, ok)
	}
	if got := auditDetails(e, "chain_changed"); !slices.Equal(got, []string{"global: openai/gpt-c, openai/gpt-a"}) {
		t.Errorf("chain_changed details = %q", got)
	}

	// With an llm: block, its entries stay in front.
	t.Setenv("PERGENT_TEST_LLM_KEY", llmKeyThree)
	e = newEnv(t, func(cfg *config.Config) {
		cfg.LLM = config.LLM{Provider: "anthropic", Model: "from-config", APIKeyEnv: "PERGENT_TEST_LLM_KEY"}
	})
	c = e.operator()
	a := e.addModel("openai", "gpt", "", llmKeyTwo)
	rec = e.sendJSON(c, "PUT", adminChainURL, entryBody("global/"+a.ID, "global/config"))
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgConfigFirst {
		t.Errorf("config entry moved = %d %s", rec.Code, rec.Body)
	}
	for _, body := range []map[string]any{entryBody("global/config", "global/"+a.ID), entryBody("global/" + a.ID)} {
		v := decode[ChainView](t, e.sendJSON(c, "PUT", adminChainURL, body), "PUT with config", http.StatusOK)
		if !slices.Equal(chainRefs(v), []string{"global/config", "global/" + a.ID}) {
			t.Errorf("chain = %v", chainRefs(v))
		}
	}
}

// A name is required, and is 1 to 64 characters that can hold no markdown;
// it is unique ignoring case. Two entries of one model, told apart by name,
// are allowed.
func TestModelNameRequired(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	body := func(name any) map[string]any {
		m := map[string]any{"provider": "openai", "model": "gpt-x", "apiKey": llmKeyOne}
		if name != nil {
			m["name"] = name
		}
		return m
	}
	for _, name := range []any{nil, "", "  ", "**bold**", "[x](https://example.com)", "-dash first", "a`b", "<b>", "@team",
		strings.Repeat("a", 65), 7} {
		rec := e.sendJSON(c, "POST", modelsURL, body(name))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name %q = %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	m := decode[ModelAdminView](t, e.sendJSON(c, "POST", modelsURL, body("Main key")), "POST", http.StatusCreated)
	if m.Name != "Main key" {
		t.Errorf("name = %q", m.Name)
	}
	rec := e.sendJSON(c, "POST", modelsURL, body("main KEY"))
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgNameTaken {
		t.Errorf("a taken name = %d %s", rec.Code, rec.Body)
	}
	// A rename checks the same rules.
	rec = e.sendJSON(c, "PUT", modelsURL+"/"+m.ID, map[string]any{"baseURL": "", "name": "**x**"})
	wantCode(t, rec, "rename to markdown", http.StatusBadRequest)
	v := decode[ModelAdminView](t, e.sendJSON(c, "PUT", modelsURL+"/"+m.ID, map[string]any{"baseURL": "", "name": "qwen3.5 a:b/c+d_e-f"}), "rename", http.StatusOK)
	if v.Name != "qwen3.5 a:b/c+d_e-f" {
		t.Errorf("renamed = %q", v.Name)
	}
	if got := auditDetails(e, "model_updated"); !slices.Equal(got, []string{"qwen3.5 a:b/c+d_e-f (openai / gpt-x) (name)"}) {
		t.Errorf("model_updated details = %q", got)
	}
}

func TestTwoKeysSameModel(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	for _, name := range []string{"first key", "second key"} {
		decode[ModelAdminView](t, e.sendJSON(c, "POST", modelsURL,
			map[string]any{"name": name, "provider": "openai", "model": "gpt-x", "baseURL": modelBaseURL, "apiKey": llmKeyOne + name}), name, http.StatusCreated)
	}
	if v := getJSON[ModelsAdminView](e, c, modelsURL); len(v.Models) != 2 {
		t.Errorf("models = %+v, want 2", v.Models)
	}
}

// A model chains name is removed all the same: the chains keep it, show it
// as removed, and every view said where it was used.
func TestRemoveModelInUseWarns(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	one := e.addModel("anthropic", "sonnet", "", llmKeyOne)
	two := e.addModel("openai", "gpt-x", "", llmKeyTwo)
	decode[ChainView](t, e.sendJSON(c, "PUT", adminChainURL, entryBody("global/"+one.ID, "global/"+two.ID)), "PUT", http.StatusOK)
	e.dashboardRepo("main", "acme/gadgets", nil)
	if err := e.st.SaveChain(ctx, store.Chain{Scope: store.ScopeRepo, Instance: "main", Project: "acme/gadgets", SavedByAdmin: true,
		Entries: []store.EntryRef{{Source: store.SourceGlobal, ID: two.ID}}}, e.clock.now()); err != nil {
		t.Fatal(err)
	}
	if got := modelNamed(t, getJSON[ModelsAdminView](e, c, modelsURL), two.ID).UsedIn; !slices.Equal(got, []string{"global chain", "main/acme/gadgets"}) {
		t.Errorf("usedIn = %q", got)
	}
	wantCode(t, e.send(c, "DELETE", modelsURL+"/"+two.ID, ""), "DELETE in use", http.StatusNoContent)
	v := getJSON[ChainView](e, c, adminChainURL)
	if !slices.Equal(chainRefs(v), []string{"global/" + one.ID, "global/" + two.ID}) || v.Entries[1].State != "removed" {
		t.Errorf("chain after the removal = %+v", v.Entries)
	}
	if got := auditDetails(e, "model_removed"); !slices.Equal(got, []string{"openai/gpt-x (openai / gpt-x)"}) {
		t.Errorf("model_removed details = %q", got)
	}
}

// Turning teams may use on or off is audited, once per change.
func TestTeamsMayUseAudit(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	m := decode[ModelAdminView](t, e.sendJSON(c, "POST", modelsURL,
		map[string]any{"name": "shared", "provider": "openai", "model": "gpt-x", "apiKey": llmKeyOne}), "POST", http.StatusCreated)
	if m.TeamsMayUse {
		t.Error("teams may use is on by default")
	}
	for _, on := range []bool{true, true, false} {
		v := decode[ModelAdminView](t, e.sendJSON(c, "PUT", modelsURL+"/"+m.ID, map[string]any{"baseURL": "", "teamsMayUse": on}), "PUT", http.StatusOK)
		if v.TeamsMayUse != on {
			t.Errorf("teamsMayUse = %v, want %v", v.TeamsMayUse, on)
		}
	}
	if got := auditDetails(e, "teams_may_use_changed"); !slices.Equal(got, []string{"shared: on", "shared: off"}) {
		t.Errorf("teams_may_use_changed details = %q", got)
	}
	if got := auditDetails(e, "model_updated"); len(got) != 0 {
		t.Errorf("model_updated details = %q, want none for a switch alone", got)
	}
}

// An admin sees and clears a global entry's mark; it is audited.
func TestAdminClearMark(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	m := e.addModel("openai", "gpt-x", "", llmKeyOne)
	other := e.addModel("openai", "gpt-y", "", llmKeyTwo)
	// A 1-entry chain ignores marks: two entries, so the mark shows.
	decode[ChainView](t, e.sendJSON(c, "PUT", adminChainURL, entryBody("global/"+m.ID, "global/"+other.ID)), "PUT", http.StatusOK)
	until := e.clock.now().Add(time.Hour)
	e.markEntry(store.EntryRef{Source: store.SourceGlobal, ID: m.ID}, store.ClassQuota, &until)
	got := modelNamed(t, getJSON[ModelsAdminView](e, c, modelsURL), m.ID).Mark
	if got == nil || got.Class != store.ClassQuota || got.Until == nil || !got.Until.Equal(until) {
		t.Fatalf("mark = %+v", got)
	}
	if v := getJSON[ChainView](e, c, adminChainURL); v.Entries[0].State != "exhausted" || !v.Entries[0].CanClear || v.Entries[0].Class != store.ClassQuota {
		t.Errorf("chain entry = %+v", v.Entries[0])
	}
	wantCode(t, e.send(c, "DELETE", modelsURL+"/"+m.ID+"/mark", ""), "clear", http.StatusNoContent)
	if marks, _ := e.st.Marks(ctx); len(marks) != 0 {
		t.Errorf("marks = %+v", marks)
	}
	wantCode(t, e.send(c, "DELETE", modelsURL+"/"+m.ID+"/mark", ""), "clear again", http.StatusNoContent)
	wantCode(t, e.send(c, "DELETE", modelsURL+"/no-such-entry/mark", ""), "clear unknown", http.StatusNotFound)
	if got := auditDetails(e, "model_mark_cleared"); !slices.Equal(got, []string{"openai/gpt-x"}) {
		t.Errorf("model_mark_cleared details = %q", got)
	}
}

// The new admin routes go through adminRoute, so the walk of
// TestEveryAdminRouteIsNotFoundForAUser covers them. No database needed.
func TestAdminRoutesStay404(t *testing.T) {
	routes := adminRoutesOf(t)
	for _, want := range []string{"GET /api/admin/chain", "PUT /api/admin/chain", "DELETE /api/admin/models/{id}/mark"} {
		if !slices.Contains(routes, want) {
			t.Errorf("%s is not registered through adminRoute", want)
		}
	}
}

// No response, log line or audit entry of the models and chain API holds a
// key.
func TestNoKeyInAnyModelResponse(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	m := decode[ModelAdminView](t, e.sendJSON(c, "POST", modelsURL,
		map[string]any{"name": "one", "provider": "openai", "model": "gpt-x", "apiKey": llmKeyOne}), "POST", http.StatusCreated)
	e.sendJSON(c, "PUT", modelsURL+"/"+m.ID, map[string]any{"baseURL": "", "apiKey": llmKeyTwo, "name": "one b", "teamsMayUse": true})
	e.sendJSON(c, "POST", modelsURL, map[string]any{"name": "one b", "provider": "openai", "model": "gpt-x", "apiKey": llmKeyThree})
	e.sendJSON(c, "PUT", adminChainURL, entryBody("global/"+m.ID))
	e.markEntry(store.EntryRef{Source: store.SourceGlobal, ID: m.ID}, store.ClassAuth, nil)
	e.get(c, modelsURL)
	e.get(c, adminChainURL)
	e.send(c, "DELETE", modelsURL+"/"+m.ID+"/mark", "")
	e.get(c, "/api/audit")
	e.wantNoSecret(llmKeyOne, llmKeyTwo, llmKeyThree)
}

// wantNoSecret fails when a response, header, audit entry or log line holds
// one of secrets.
func (e *env) wantNoSecret(secrets ...string) {
	e.t.Helper()
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
				e.t.Fatalf("the key %q is in a response, log or audit entry:\n%s", secret, body)
			}
		}
	}
}
