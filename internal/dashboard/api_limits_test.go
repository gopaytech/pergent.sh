package dashboard

import (
	"net/http"
	"slices"
	"strconv"
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
