package dashboard

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

func modelLimitURL(id string) string { return modelsURL + "/" + id + "/limit" }

func teamLimitURL(id string, team int64) string {
	return modelsURL + "/" + id + "/team-limits/" + strconv.FormatInt(team, 10)
}

// storedLimits is every limit in the store.
func (e *env) storedLimits() []store.ModelLimit {
	e.t.Helper()
	got, err := e.st.ModelLimits(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// An admin sets and clears the model limit of a stored entry and of the
// config file's: 204 each time, a clear of none too; each set and each real
// clear is audited with the entry, all teams and the number.
func TestAdminModelLimit(t *testing.T) {
	t.Setenv("PERGENT_TEST_LLM_KEY", "config-key")
	e := newEnv(t, func(cfg *config.Config) {
		cfg.LLM = config.LLM{Provider: "anthropic", Model: "from-config", APIKeyEnv: "PERGENT_TEST_LLM_KEY"}
	})
	c := e.operator()
	m := e.addModel("openai", "gpt-5", "", llmKeyThree)
	name := modelNamed(t, decode[ModelsAdminView](t, e.get(c, modelsURL), "GET models", http.StatusOK), m.ID).Name
	for _, id := range []string{m.ID, store.ConfigModelID} {
		wantCode(t, e.send(c, "PUT", modelLimitURL(id), `{"monthlyTokens":5000}`), "PUT "+id, http.StatusNoContent)
	}
	wantCode(t, e.send(c, "PUT", modelLimitURL(m.ID), `{"monthlyTokens":6000}`), "PUT again", http.StatusNoContent)
	got := e.storedLimits()
	if len(got) != 2 {
		t.Fatalf("stored = %+v; want 2", got)
	}
	for _, l := range got {
		want := int64(6000)
		if l.Ref.ID == store.ConfigModelID {
			want = 5000
		} else if l.Ref.ID != m.ID {
			t.Errorf("unexpected entry %+v", l)
		}
		if l.MonthlyTokens != want || l.UpdatedBy != "ops (password)" {
			t.Errorf("limit = %+v; want %d by ops (password)", l, want)
		}
	}
	wantCode(t, e.send(c, "DELETE", modelLimitURL(m.ID), ""), "DELETE", http.StatusNoContent)
	wantCode(t, e.send(c, "DELETE", modelLimitURL(m.ID), ""), "DELETE none", http.StatusNoContent)
	wantCode(t, e.send(c, "PUT", modelLimitURL("no-such-model"), `{"monthlyTokens":1}`), "unknown entry", http.StatusNotFound)
	if got, want := auditDetails(e, "model_limit_set"), []string{
		name + " for all teams: monthly_tokens=5000", "anthropic/from-config for all teams: monthly_tokens=5000", name + " for all teams: monthly_tokens=6000",
	}; !slices.Equal(got, want) {
		t.Errorf("model_limit_set = %q; want %q", got, want)
	}
	if got := auditDetails(e, "model_limit_cleared"); !slices.Equal(got, []string{name + " for all teams: monthly_tokens=6000"}) {
		t.Errorf("model_limit_cleared = %q", got)
	}
}

// The number is a whole number from 1 to 10^15, written as a JSON number;
// anything else is 400 with the range and changes nothing.
func TestModelLimitBounds(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	m := e.addModel("openai", "gpt-5", "", llmKeyThree)
	for _, body := range []string{`{"monthlyTokens":0}`, `{"monthlyTokens":-1}`, `{"monthlyTokens":1000000000000001}`,
		`{"monthlyTokens":1.5}`, `{"monthlyTokens":1e3}`, `{"monthlyTokens":"5"}`, `{"monthlyTokens":null}`, `{}`, `{"monthlyTokens":5,"x":1}`} {
		rec := e.send(c, "PUT", modelLimitURL(m.ID), body)
		wantCode(t, rec, body, http.StatusBadRequest)
		if got := errorOf(t, rec); got != msgBadLimit {
			t.Errorf("%s: error = %q", body, got)
		}
	}
	if got := e.storedLimits(); len(got) != 0 {
		t.Fatalf("stored = %+v; want none", got)
	}
	for _, body := range []string{`{"monthlyTokens":1}`, `{"monthlyTokens":1000000000000000}`} {
		wantCode(t, e.send(c, "PUT", modelLimitURL(m.ID), body), body, http.StatusNoContent)
	}
}

// An admin sets a team's limit on a global entry, teams may use it or not;
// an unknown team is 404; the route is not found for anyone else.
func TestAdminTeamLimit(t *testing.T) {
	te := newTeamsEnv(t)
	m := te.addModel("openai", "gpt-5", "", llmKeyThree)
	name := modelNamed(t, decode[ModelsAdminView](t, te.get(te.admin, modelsURL), "GET models", http.StatusOK), m.ID).Name
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL(m.ID, te.payments), `{"monthlyTokens":300}`), "PUT", http.StatusNoContent)
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL(m.ID, 999999), `{"monthlyTokens":300}`), "unknown team", http.StatusNotFound)
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL("no-such-model", te.payments), `{"monthlyTokens":300}`), "unknown entry", http.StatusNotFound)
	for name, c := range map[string]*http.Cookie{"maintainer": te.maintainer, "viewer": te.viewer} {
		wantCode(t, te.send(c, "PUT", teamLimitURL(m.ID, te.payments), `{"monthlyTokens":1}`), name, http.StatusNotFound)
		wantCode(t, te.send(c, "PUT", modelLimitURL(m.ID), `{"monthlyTokens":1}`), name, http.StatusNotFound)
	}
	if got := te.storedLimits(); len(got) != 1 || got[0].TeamID != te.payments || got[0].MonthlyTokens != 300 {
		t.Errorf("stored = %+v", got)
	}
	wantCode(t, te.send(te.admin, "DELETE", teamLimitURL(m.ID, te.payments), ""), "DELETE", http.StatusNoContent)
	if got := auditDetails(te.env, "model_limit_set"); !slices.Equal(got, []string{name + " for Payments: monthly_tokens=300"}) {
		t.Errorf("model_limit_set = %q", got)
	}
	if got := auditDetails(te.env, "model_limit_cleared"); !slices.Equal(got, []string{name + " for Payments: monthly_tokens=300"}) {
		t.Errorf("model_limit_cleared = %q", got)
	}
}

// Who sets a team entry's limit: a maintainer of the team and an admin. A
// viewer and another team's maintainer get 403, anyone else 404; another
// team's entry is 400; a global entry is the admins' (403).
func TestTeamModelLimitAccess(t *testing.T) {
	te := newTeamsEnv(t)
	own := te.addTeamModel(te.maintainer, te.payments, "pay-a", teamKeyOne)
	other := te.addTeamModel(te.otherMaintainer, te.widgets, "wid-a", teamKeyTwo)
	g := te.addModel("openai", "gpt-5", "", llmKeyThree)
	put := func(c *http.Cookie, model string) *httptest.ResponseRecorder {
		return te.send(c, "PUT", teamURL(te.payments, "/models/"+model+"/limit"), `{"monthlyTokens":100}`)
	}
	wantCode(t, put(te.maintainer, own.ID), "maintainer", http.StatusNoContent)
	wantCode(t, put(te.admin, own.ID), "admin", http.StatusNoContent)
	for name, c := range map[string]*http.Cookie{"viewer": te.viewer, "other maintainer": te.otherMaintainer} {
		rec := put(c, own.ID)
		wantCode(t, rec, name, http.StatusForbidden)
		if got := errorOf(t, rec); got != msgTeamWrite {
			t.Errorf("%s: error = %q", name, got)
		}
	}
	wantCode(t, put(te.other, own.ID), "a viewer of another team", http.StatusNotFound)
	for name, c := range map[string]struct {
		model string
		code  int
		msg   string
	}{
		"another team's entry": {other.ID, http.StatusBadRequest, msgNotTeamModel},
		"no such entry":        {"no-such-model", http.StatusBadRequest, msgNotTeamModel},
		"a global entry":       {g.ID, http.StatusForbidden, msgAdminLimit},
	} {
		rec := put(te.maintainer, c.model)
		wantCode(t, rec, name, c.code)
		if got := errorOf(t, rec); got != c.msg {
			t.Errorf("%s: error = %q", name, got)
		}
	}
	wantCode(t, te.send(te.maintainer, "PUT", teamURL(te.payments, "/models/"+own.ID+"/limit"), `{"monthlyTokens":0}`), "bad number", http.StatusBadRequest)
	wantCode(t, te.send(te.maintainer, "DELETE", teamURL(te.payments, "/models/"+own.ID+"/limit"), ""), "DELETE", http.StatusNoContent)
	if got := auditDetails(te.env, "model_limit_set"); !slices.Equal(got, []string{"pay-a for Payments: monthly_tokens=100", "pay-a for Payments: monthly_tokens=100"}) {
		t.Errorf("model_limit_set = %q", got)
	}
	if got := auditDetails(te.env, "model_limit_cleared"); !slices.Equal(got, []string{"pay-a for Payments: monthly_tokens=100"}) {
		t.Errorf("model_limit_cleared = %q", got)
	}
}

// The team's limits: its own entries with their limits and its use, and each
// global entry with an admin's limit for the team. A global entry's model
// limit shows only whether it has room: its numbers count other teams.
func TestTeamLimitsView(t *testing.T) {
	te := newTeamsEnv(t)
	own := te.addTeamModel(te.maintainer, te.payments, "pay-a", teamKeyOne)
	g := te.addModel("openai", "gpt-5", "", llmKeyThree)
	te.addModel("openai", "gpt-6", "", llmKeyThree) // no limit for the team: not listed
	gName := modelNamed(t, decode[ModelsAdminView](t, te.get(te.admin, modelsURL), "GET models", http.StatusOK), g.ID).Name
	wantCode(t, te.send(te.admin, "PUT", teamLimitURL(g.ID, te.payments), `{"monthlyTokens":1000}`), "team limit", http.StatusNoContent)
	wantCode(t, te.send(te.admin, "PUT", modelLimitURL(g.ID), `{"monthlyTokens":100}`), "model limit", http.StatusNoContent)
	wantCode(t, te.send(te.maintainer, "PUT", teamURL(te.payments, "/models/"+own.ID+"/limit"), `{"monthlyTokens":500}`), "own", http.StatusNoContent)
	used := te.usageRun(runFix{project: "acme/payments", number: 50, head: head('5')}, usageSkill("code-review", "no_blocking_issues", 120, 30))
	te.exec(`UPDATE runs SET model_source = 'global', model_entry_id = $2 WHERE id = $1::uuid`, used, g.ID)

	for name, c := range map[string]*http.Cookie{"maintainer": te.maintainer, "admin": te.admin} {
		rec := te.get(c, teamURL(te.payments, "/limits"))
		v := decode[TeamLimitsView](t, rec, name, http.StatusOK)
		if v.Team.ID != te.payments || len(v.Models) != 1 || len(v.Global) != 1 {
			t.Fatalf("%s: view = %+v", name, v)
		}
		if m := v.Models[0]; m.ID != own.ID || m.Name != "pay-a" || m.Limit == nil || *m.Limit != 500 || m.Used != 0 {
			t.Errorf("%s: own entry = %+v", name, m)
		}
		if gl := v.Global[0]; gl != (TeamGlobalLimitView{ID: g.ID, Name: gName, Limit: 1000, Used: 150, ModelLimit: "used_up"}) {
			t.Errorf("%s: global entry = %+v", name, gl)
		}
		if strings.Contains(rec.Body.String(), `"100"`) || strings.Contains(rec.Body.String(), `:100,`) || strings.Contains(rec.Body.String(), `:100}`) {
			t.Errorf("%s: the model limit's number reached the team: %s", name, rec.Body)
		}
	}
	rec := te.get(te.viewer, teamURL(te.payments, "/limits"))
	wantCode(t, rec, "viewer", http.StatusForbidden)
	if got := errorOf(t, rec); got != msgTeamLimitsRead {
		t.Errorf("viewer: error = %q", got)
	}
	wantCode(t, te.get(te.otherMaintainer, teamURL(te.payments, "/limits")), "another team's maintainer", http.StatusNotFound)
}
