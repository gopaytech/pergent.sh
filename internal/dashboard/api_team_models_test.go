package dashboard

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	teamKeyOne = "team-api-key-SECRET-111"
	teamKeyTwo = "team-api-key-SECRET-222"
)

// teamsEnv is an env with team "Payments" owning main/acme/payments and
// covering main/acme/widgets, team "Widgets" owning main/acme/widgets, and a
// session for each kind of person: a viewer and a maintainer of Payments, a
// maintainer of Widgets only, a viewer of Widgets only, and an admin.
type teamsEnv struct {
	*env
	payments, widgets                                 int64
	viewer, maintainer, otherMaintainer, other, admin *http.Cookie
}

func newTeamsEnv(t *testing.T) *teamsEnv {
	t.Helper()
	e := newEnv(t, nil)
	te := &teamsEnv{env: e}
	viewer, maintainer := e.sso("viewer@example.com"), e.sso("maintainer@example.com")
	otherMaintainer, other := e.sso("other-maintainer@example.com"), e.sso("other@example.com")
	te.payments = e.team("Payments", []store.TeamMember{{AccountID: viewer, Role: store.RoleViewer}, {AccountID: maintainer, Role: store.RoleMaintainer}},
		store.TeamRepo{Instance: "main", Project: "acme/payments", Owner: true}, store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	te.widgets = e.team("Widgets", []store.TeamMember{{AccountID: otherMaintainer, Role: store.RoleMaintainer}, {AccountID: other, Role: store.RoleViewer}},
		store.TeamRepo{Instance: "main", Project: "acme/widgets", Owner: true}, store.TeamRepo{Instance: "main", Project: "acme/payments"})
	te.viewer, te.maintainer = e.sessionFor(viewer), e.sessionFor(maintainer)
	te.otherMaintainer, te.other = e.sessionFor(otherMaintainer), e.sessionFor(other)
	te.admin = e.operator()
	return te
}

func teamURL(id int64, rest string) string { return "/api/teams/" + strconv.FormatInt(id, 10) + rest }

// addTeamModel adds an entry to team through the API, as c.
func (te *teamsEnv) addTeamModel(c *http.Cookie, team int64, name, key string) TeamModelView {
	te.t.Helper()
	return decode[TeamModelView](te.t, te.sendJSON(c, "POST", teamURL(team, "/models"),
		map[string]any{"name": name, "provider": "openai", "model": "gpt-" + name, "apiKey": key}), "POST team model", http.StatusCreated)
}

// A second owner at the same level is 409 with the exact text, for a
// repository and for a whole instance; ticks set and cleared are audited.
func TestOwnerTickTaken(t *testing.T) {
	e := newEnv(t, nil)
	c := e.operator()
	a, b := e.team("Payments", nil), e.team("Platform", nil)
	put := func(team int64, repos string) *httptest.ResponseRecorder {
		return e.send(c, "PUT", teamURL(team, ""), `{"name":"`+map[int64]string{a: "Payments", b: "Platform"}[team]+`","members":[],"repos":`+repos+`}`)
	}
	v := decode[TeamView](t, put(a, `[{"instance":"main","project":"acme/payments","owner":true},{"instance":"gh","project":"","owner":true}]`), "PUT a", http.StatusOK)
	if !slices.Equal(v.Repos, []TeamRepoView{{Instance: "gh", Owner: true}, {Instance: "main", Project: "acme/payments", Owner: true}}) {
		t.Errorf("repos = %+v", v.Repos)
	}
	rec := put(b, `[{"instance":"main","project":"ACME/payments","owner":true}]`)
	if rec.Code != http.StatusConflict || errorOf(t, rec) != "acme/payments already has owner Payments" {
		t.Errorf("second owner = %d %s", rec.Code, rec.Body)
	}
	rec = put(b, `[{"instance":"gh","project":"","owner":true}]`)
	if rec.Code != http.StatusConflict || errorOf(t, rec) != "gh (whole instance) already has owner Payments" {
		t.Errorf("second instance owner = %d %s", rec.Code, rec.Body)
	}
	// Covering without the tick is fine; then the tick moves.
	decode[TeamView](t, put(b, `[{"instance":"main","project":"acme/payments"}]`), "PUT b no tick", http.StatusOK)
	decode[TeamView](t, put(a, `[{"instance":"main","project":"acme/payments"},{"instance":"gh","project":"","owner":true}]`), "PUT a untick", http.StatusOK)
	decode[TeamView](t, put(b, `[{"instance":"main","project":"acme/payments","owner":true}]`), "PUT b tick", http.StatusOK)
	if got := auditDetails(e, "owner_set"); !slices.Equal(got, []string{"Payments: gh (whole instance)", "Payments: main/acme/payments", "Platform: main/acme/payments"}) {
		t.Errorf("owner_set = %q", got)
	}
	if got := auditDetails(e, "owner_cleared"); !slices.Equal(got, []string{"Payments: main/acme/payments"}) {
		t.Errorf("owner_cleared = %q", got)
	}
}

// A member sees their own teams, with their role there; an admin sees every
// team.
func TestMyTeams(t *testing.T) {
	te := newTeamsEnv(t)
	got := decode[[]MyTeamView](t, te.get(te.viewer, "/api/me/teams"), "viewer", http.StatusOK)
	if len(got) != 1 || got[0].ID != te.payments || got[0].Role != "viewer" || got[0].Name != "Payments" {
		t.Errorf("viewer's teams = %+v", got)
	}
	if !slices.Contains(got[0].Repos, TeamRepoView{Instance: "main", Project: "acme/payments", Owner: true}) {
		t.Errorf("repos = %+v", got[0].Repos)
	}
	got = decode[[]MyTeamView](t, te.get(te.admin, "/api/me/teams"), "admin", http.StatusOK)
	if len(got) != 2 || got[0].Role != "admin" {
		t.Errorf("admin's teams = %+v", got)
	}
}

// Who reaches a team's models: a non-member gets 404 on a read; a viewer
// reads and gets 403 on a write; a maintainer of another team gets 403 on a
// write and 404 on a read; a maintainer and an admin write.
func TestTeamModelsAccess(t *testing.T) {
	te := newTeamsEnv(t)
	models := teamURL(te.payments, "/models")
	for name, c := range map[string]*http.Cookie{"viewer": te.viewer, "maintainer": te.maintainer, "admin": te.admin} {
		v := decode[TeamModelsView](t, te.get(c, models), "GET as "+name, http.StatusOK)
		if v.Team.ID != te.payments || v.CanEdit != (name != "viewer") {
			t.Errorf("%s: view = %+v", name, v)
		}
	}
	for name, c := range map[string]*http.Cookie{"other maintainer": te.otherMaintainer, "other viewer": te.other} {
		wantCode(t, te.get(c, models), "GET as "+name, http.StatusNotFound)
	}
	wantCode(t, te.get(te.maintainer, teamURL(99999, "/models")), "GET unknown team", http.StatusNotFound)

	body := map[string]any{"name": "own", "provider": "openai", "model": "gpt-own", "apiKey": teamKeyOne}
	wantCode(t, te.sendJSON(te.viewer, "POST", models, body), "POST as viewer", http.StatusForbidden)
	wantCode(t, te.sendJSON(te.otherMaintainer, "POST", models, body), "POST as other maintainer", http.StatusForbidden)
	wantCode(t, te.sendJSON(te.other, "POST", models, body), "POST as a non-member viewer", http.StatusNotFound)
	m := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	te.addTeamModel(te.admin, te.payments, "admin-added", teamKeyTwo)

	for name, c := range map[string]*http.Cookie{"viewer": te.viewer, "other maintainer": te.otherMaintainer} {
		wantCode(t, te.sendJSON(c, "PUT", models+"/"+m.ID, map[string]any{"name": "renamed"}), "PUT as "+name, http.StatusForbidden)
		wantCode(t, te.send(c, "DELETE", models+"/"+m.ID, ""), "DELETE as "+name, http.StatusForbidden)
		wantCode(t, te.sendJSON(c, "PUT", teamURL(te.payments, "/chain"), entryBody("team/"+m.ID)), "PUT chain as "+name, http.StatusForbidden)
		wantCode(t, te.send(c, "DELETE", models+"/"+m.ID+"/mark", ""), "clear mark as "+name, http.StatusForbidden)
	}
	if rows, _ := te.st.TeamModels(ctx, te.payments); len(rows) != 2 || rows[0].Name != "own" {
		t.Errorf("stored = %+v", rows)
	}
	if got := auditDetails(te.env, "team_model_added"); !slices.Equal(got, []string{"Payments: own (openai / gpt-own)", "Payments: admin-added (openai / gpt-admin-added)"}) {
		t.Errorf("team_model_added = %q", got)
	}
}

// A team key is write-only: no response, log or audit entry holds it; the
// view says whether one is set. A new key reaches the registry.
func TestTeamModelKeyWriteOnly(t *testing.T) {
	te := newTeamsEnv(t)
	m := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	if !m.Key.Set || m.Provider != "openai" {
		t.Errorf("created = %+v", m)
	}
	v := decode[TeamModelView](t, te.sendJSON(te.maintainer, "PUT", teamURL(te.payments, "/models/"+m.ID),
		map[string]any{"apiKey": teamKeyTwo, "baseURL": modelBaseURL}), "PUT key", http.StatusOK)
	if !v.Key.Set || v.BaseURL != modelBaseURL {
		t.Errorf("updated = %+v", v)
	}
	if got := te.reg.TeamModels(te.payments); len(got) != 1 || got[0].Key != teamKeyTwo {
		t.Errorf("registry = %+v", got)
	}
	// A viewer sees the entry, not its base URL.
	if got := decode[TeamModelsView](t, te.get(te.viewer, teamURL(te.payments, "/models")), "viewer GET", http.StatusOK); got.Models[0].BaseURL != "" {
		t.Errorf("a viewer sees the base URL: %+v", got.Models[0])
	}
	te.get(te.maintainer, teamURL(te.payments, "/models"))
	te.get(te.maintainer, teamURL(te.payments, "/chain"))
	te.get(te.admin, "/api/audit")
	te.wantNoSecret(teamKeyOne, teamKeyTwo)
}

// A team chain holds the team's own entries and the global entries teams may
// use; another team's entry, or a global one teams may not use, is 400.
func TestTeamChainOnlyOwnAndAllowed(t *testing.T) {
	te := newTeamsEnv(t)
	own := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	foreign := te.addTeamModel(te.otherMaintainer, te.widgets, "theirs", teamKeyTwo)
	shared := te.addModel("openai", "gpt-shared", "", llmKeyOne)
	private := te.addModel("openai", "gpt-private", "", llmKeyTwo)
	if found, err := te.st.SetLLMModelMeta(ctx, shared.ID, nil, boolPtr(true), te.clock.now()); err != nil || !found {
		t.Fatal(found, err)
	}
	if err := te.reg.LoadLLM(ctx); err != nil {
		t.Fatal(err)
	}
	chainURL := teamURL(te.payments, "/chain")
	v := getJSON[ChainView](te.env, te.maintainer, chainURL)
	if v.Scope != store.ScopeTeam || v.Source != "global" || v.ReadOnly || len(v.Choices) != 2 {
		t.Errorf("before a save = %+v", v)
	}
	for name, tc := range map[string]struct {
		ref, msg string
	}{
		"foreign team entry": {"team/" + foreign.ID, msgForeignTeamEntry},
		"global not allowed": {"global/" + private.ID, msgNotTeamsMayUse},
		"unknown":            {"team/no-such-entry", msgUnknownEntry},
	} {
		rec := te.sendJSON(te.maintainer, "PUT", chainURL, entryBody("team/"+own.ID, tc.ref))
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != tc.msg {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	v = decode[ChainView](t, te.sendJSON(te.maintainer, "PUT", chainURL, entryBody("team/"+own.ID, "global/"+shared.ID)), "PUT", http.StatusOK)
	if v.Source != "team" || v.Team != "Payments" || !slices.Equal(chainRefs(v), []string{"team/" + own.ID, "global/" + shared.ID}) || v.SavedByAdmin {
		t.Errorf("saved = %+v", v)
	}
	if got := auditDetails(te.env, "chain_changed"); !slices.Equal(got, []string{"team Payments: own, openai/gpt-shared"}) {
		t.Errorf("chain_changed = %q", got)
	}
	// An admin may not put a global entry teams may not use in a team chain
	// either: the resolver would skip it.
	rec := te.sendJSON(te.admin, "PUT", chainURL, entryBody("global/"+private.ID))
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgNotTeamsMayUse {
		t.Errorf("admin, global not allowed = %d %s", rec.Code, rec.Body)
	}
	// An empty chain is no chain: the team's repositories use the global one.
	v = decode[ChainView](t, te.sendJSON(te.maintainer, "PUT", chainURL, entryBody()), "PUT empty", http.StatusOK)
	if v.Source != "global" {
		t.Errorf("after an empty save = %+v", v)
	}
}

// [Review Focus 4] A maintainer of team Widgets can't edit team Payments'
// chain (403 on the write, as for any write of another team's) and can't
// name a Payments entry in Widgets' chain (400). The chain of a repository
// Payments owns is TestRepoChainOtherTeam403's.
func TestTeamChainForeignEntry(t *testing.T) {
	te := newTeamsEnv(t)
	own := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	wantCode(t, te.sendJSON(te.otherMaintainer, "PUT", teamURL(te.payments, "/chain"), entryBody("team/"+own.ID)), "PUT Payments chain", http.StatusForbidden)
	rec := te.sendJSON(te.otherMaintainer, "PUT", teamURL(te.widgets, "/chain"), entryBody("team/"+own.ID))
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgForeignTeamEntry {
		t.Errorf("Payments entry in Widgets' chain = %d %s", rec.Code, rec.Body)
	}
	if c, found, _ := te.st.Chain(ctx, store.ScopeTeam, te.widgets, "", ""); found {
		t.Errorf("Widgets' chain = %+v", c)
	}
	if _, found, _ := te.st.Chain(ctx, store.ScopeTeam, te.payments, "", ""); found {
		t.Error("Payments' chain was written")
	}
}

// A team's maintainer clears the mark of the team's own entry; a global
// entry's mark is the admins' (its route answers anyone else 404).
func TestTeamMaintainerClearsOwnMarkOnly(t *testing.T) {
	te := newTeamsEnv(t)
	own := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	g := te.addModel("openai", "gpt-global", "", llmKeyOne)
	until := te.clock.now().Add(time.Hour)
	te.markEntry(store.EntryRef{Source: store.SourceTeam, ID: own.ID}, store.ClassQuota, &until)
	te.markEntry(store.EntryRef{Source: store.SourceGlobal, ID: g.ID}, store.ClassAuth, nil)

	got := decode[TeamModelsView](t, te.get(te.maintainer, teamURL(te.payments, "/models")), "GET", http.StatusOK)
	if got.Models[0].Mark == nil || got.Models[0].Mark.Class != store.ClassQuota {
		t.Errorf("team entry mark = %+v", got.Models[0].Mark)
	}
	wantCode(t, te.send(te.maintainer, "DELETE", modelsURL+"/"+g.ID+"/mark", ""), "global mark as maintainer", http.StatusNotFound)
	wantCode(t, te.send(te.maintainer, "DELETE", teamURL(te.payments, "/models/"+g.ID+"/mark"), ""), "global entry via the team route", http.StatusNotFound)
	wantCode(t, te.send(te.maintainer, "DELETE", teamURL(te.payments, "/models/"+own.ID+"/mark"), ""), "own mark", http.StatusNoContent)
	marks, _ := te.st.Marks(ctx)
	if len(marks) != 1 || marks[0].Ref.ID != g.ID {
		t.Errorf("marks = %+v; want the global one alone", marks)
	}
	if got := auditDetails(te.env, "model_mark_cleared"); !slices.Equal(got, []string{"Payments: own"}) {
		t.Errorf("model_mark_cleared = %q", got)
	}
}

// Deleting a team deletes its entries, and says so in the audit log.
func TestDeleteTeamAudits(t *testing.T) {
	te := newTeamsEnv(t)
	te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	te.addTeamModel(te.maintainer, te.payments, "second", teamKeyTwo)
	wantCode(t, te.send(te.admin, "DELETE", teamURL(te.payments, ""), ""), "DELETE team", http.StatusNoContent)
	if got := auditDetails(te.env, "team_models_deleted"); !slices.Equal(got, []string{"Payments: own, second"}) {
		t.Errorf("team_models_deleted = %q", got)
	}
	if got := te.reg.TeamModels(te.payments); len(got) != 0 {
		t.Errorf("registry still holds %+v", got)
	}
}

// A name is unique within a team, ignoring case, and follows the name rule;
// another team may use it.
func TestTeamModelNameTaken(t *testing.T) {
	te := newTeamsEnv(t)
	te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	rec := te.sendJSON(te.maintainer, "POST", teamURL(te.payments, "/models"),
		map[string]any{"name": "OWN", "provider": "openai", "model": "gpt", "apiKey": teamKeyTwo})
	if rec.Code != http.StatusConflict || errorOf(t, rec) != msgNameTaken {
		t.Errorf("taken = %d %s", rec.Code, rec.Body)
	}
	rec = te.sendJSON(te.maintainer, "POST", teamURL(te.payments, "/models"),
		map[string]any{"name": "**own**", "provider": "openai", "model": "gpt", "apiKey": teamKeyTwo})
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgBadModelName {
		t.Errorf("markdown = %d %s", rec.Code, rec.Body)
	}
	te.addTeamModel(te.otherMaintainer, te.widgets, "own", teamKeyTwo)
	// Removing an entry takes it out of the chains that name it.
	m := te.addTeamModel(te.maintainer, te.payments, "third", "k3")
	decode[ChainView](t, te.sendJSON(te.maintainer, "PUT", teamURL(te.payments, "/chain"), entryBody("team/"+m.ID)), "PUT chain", http.StatusOK)
	if got := decode[TeamModelsView](t, te.get(te.maintainer, teamURL(te.payments, "/models")), "GET", http.StatusOK); !slices.Equal(got.Models[1].UsedIn, []string{"team Payments"}) {
		t.Errorf("usedIn = %+v", got.Models[1].UsedIn)
	}
	wantCode(t, te.send(te.maintainer, "DELETE", teamURL(te.payments, "/models/"+m.ID), ""), "DELETE", http.StatusNoContent)
	if c, found, _ := te.st.Chain(ctx, store.ScopeTeam, te.payments, "", ""); found && len(c.Entries) != 0 {
		t.Errorf("chain after the removal = %+v", c)
	}
	if got := auditDetails(te.env, "team_model_removed"); !slices.Equal(got, []string{"Payments: third (openai / gpt-third), removed from team Payments"}) {
		t.Errorf("team_model_removed = %q", got)
	}
}

func boolPtr(b bool) *bool { return &b }
