package dashboard

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	adminPanelURL  = "/api/admin/panel-chain"
	panelFloor     = "v0.12.0"
	panelImageOK   = "ghcr.io/x/pergent:v0.12.0"
	msgNoPanelYet  = "no review image supports a review panel yet"
	msgPanelNeedsA = "chain B needs a review image with pergent v0.12.0 or later; jobs.image is "
)

func repoPanelURL(project string) string { return "/api/repositories/main/" + project + "/panel-chain" }

// setPanelFloor sets the first pergent with a panel and the review image,
// the floor going back to "" after the test.
func setPanelFloor(t *testing.T, e *env, floor, image string) {
	t.Helper()
	config.PanelMinVersion = floor
	t.Cleanup(func() { config.PanelMinVersion = "" })
	e.cfg.Jobs.Image = image
}

// saveB stores a chain B directly, past the image check.
func (e *env) saveB(c store.Chain) {
	e.t.Helper()
	if err := e.st.SaveChainOf(ctx, store.ChainB, c, e.clock.now()); err != nil {
		e.t.Fatal(err)
	}
}

// panelAudit is every panel_chain_changed entry, oldest first, as
// "instance project detail".
func (e *env) panelAudit() []string {
	var out []string
	for _, a := range e.auditEntries() {
		if a.Action == "panel_chain_changed" {
			out = append(out, a.Instance+" "+a.Project+" "+a.Detail)
		}
	}
	slices.Reverse(out)
	return out
}

// gadgetsOwnedByPayments adds main/acme/gadgets as a dashboard repository
// that Payments owns.
func (te *teamsEnv) gadgetsOwnedByPayments() {
	te.t.Helper()
	te.dashboardRepo("main", "acme/gadgets", nil)
	te.editTeam(te.payments, func(tm *store.Team) {
		tm.Repos = append(tm.Repos, store.TeamRepo{Instance: "main", Project: "acme/gadgets", Owner: true})
	})
}

// With the floor unset every chain B save is refused, and nothing is stored.
func TestPanelChainRefusedWithoutFloor(t *testing.T) {
	te := newTeamsEnv(t)
	shared, _ := te.sharedAndPrivate()
	te.cfg.Jobs.Image = panelImageOK
	for name, tc := range map[string]struct {
		c   *http.Cookie
		url string
	}{
		"global": {te.admin, adminPanelURL},
		"team":   {te.maintainer, teamURL(te.payments, "/panel-chain")},
		"repo":   {te.maintainer, repoPanelURL("acme/payments")},
	} {
		rec := te.sendJSON(tc.c, "PUT", tc.url, entryBody("global/"+shared.ID))
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgNoPanelYet {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if b, err := te.st.ChainsOf(ctx, store.ChainB); err != nil || len(b) != 0 {
		t.Errorf("chain B stored = %+v, %v", b, err)
	}
	if got := te.panelAudit(); len(got) != 0 {
		t.Errorf("panel_chain_changed = %q", got)
	}
}

// With an image at the floor the global, team and repository chain B are
// saved, each audited with its scope and names; chain A and the default stay.
func TestPanelChainSavedWithFloor(t *testing.T) {
	te := newTeamsEnv(t)
	setPanelFloor(t, te.env, panelFloor, panelImageOK)
	shared, _ := te.sharedAndPrivate()
	own := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	before, _ := te.reg.DefaultModel()

	v := decode[ChainView](t, te.sendJSON(te.admin, "PUT", adminPanelURL, entryBody("global/"+shared.ID)), "global PUT", http.StatusOK)
	if v.Scope != store.ScopeGlobal || v.Source != "global" || v.ReadOnly || !slices.Equal(chainRefs(v), []string{"global/" + shared.ID}) {
		t.Errorf("global chain B = %+v", v)
	}
	v = decode[ChainView](t, te.sendJSON(te.maintainer, "PUT", teamURL(te.payments, "/panel-chain"), entryBody("team/"+own.ID)), "team PUT", http.StatusOK)
	if v.Scope != store.ScopeTeam || v.Source != "team" || !slices.Equal(chainRefs(v), []string{"team/" + own.ID}) {
		t.Errorf("team chain B = %+v", v)
	}
	v = decode[ChainView](t, te.sendJSON(te.maintainer, "PUT", repoPanelURL("acme/payments"), entryBody("global/"+shared.ID, "team/"+own.ID)), "repo PUT", http.StatusOK)
	if v.Scope != store.ScopeRepo || v.Source != "repository" || v.Mode != store.PanelCustom || v.SavedByAdmin ||
		!slices.Equal(chainRefs(v), []string{"global/" + shared.ID, "team/" + own.ID}) {
		t.Errorf("repo chain B = %+v", v)
	}
	if got, want := te.panelAudit(), []string{
		"  scope=global entries=openai/gpt-shared",
		"  scope=team team=Payments entries=own",
		"main acme/payments scope=repo entries=openai/gpt-shared, own",
	}; !slices.Equal(got, want) {
		t.Errorf("panel_chain_changed = %q; want %q", got, want)
	}
	if got := auditDetails(te.env, "chain_changed"); len(got) != 0 {
		t.Errorf("chain_changed = %q; chain B audits as panel_chain_changed", got)
	}
	if a, err := te.st.Chains(ctx); err != nil || len(a) != 0 {
		t.Errorf("chain A = %+v, %v; want none", a, err)
	}
	if after, _ := te.reg.DefaultModel(); after.ID != before.ID {
		t.Errorf("default = %s, want %s: a global chain B moves no default", after.ID, before.ID)
	}
	// Each GET shows what was saved.
	if v := getJSON[ChainView](te.env, te.admin, adminPanelURL); !slices.Equal(chainRefs(v), []string{"global/" + shared.ID}) {
		t.Errorf("GET global = %+v", v)
	}
	if v := getJSON[ChainView](te.env, te.viewer, teamURL(te.payments, "/panel-chain")); !v.ReadOnly || !slices.Equal(chainRefs(v), []string{"team/" + own.ID}) {
		t.Errorf("GET team as a viewer = %+v", v)
	}
	te.wantNoSecret(llmKeyOne, llmKeyTwo, teamKeyOne)
}

// An image older than the floor is refused, with its tag in the message.
func TestPanelChainImageTooOld(t *testing.T) {
	te := newTeamsEnv(t)
	setPanelFloor(t, te.env, panelFloor, "ghcr.io/x/pergent:v0.11.0")
	shared, _ := te.sharedAndPrivate()
	for _, url := range []string{adminPanelURL, repoPanelURL("acme/payments")} {
		rec := te.sendJSON(te.admin, "PUT", url, entryBody("global/"+shared.ID))
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgPanelNeedsA+"v0.11.0" {
			t.Errorf("PUT %s = %d %s", url, rec.Code, rec.Body)
		}
	}
	if b, _ := te.st.ChainsOf(ctx, store.ChainB); len(b) != 0 {
		t.Errorf("chain B stored = %+v", b)
	}
}

// A tag that is not a version is refused with the tag, marked "(not a
// version)"; no tag at all reads "not a version".
func TestPanelChainImageNotVersion(t *testing.T) {
	te := newTeamsEnv(t)
	shared, _ := te.sharedAndPrivate()
	for image, want := range map[string]string{
		"ghcr.io/x/pergent:main": msgPanelNeedsA + "main (not a version)",
		"ghcr.io/x/pergent":      msgPanelNeedsA + "not a version",
	} {
		setPanelFloor(t, te.env, panelFloor, image)
		rec := te.sendJSON(te.admin, "PUT", adminPanelURL, entryBody("global/"+shared.ID))
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != want {
			t.Errorf("image %s = %d %s; want %q", image, rec.Code, rec.Body, want)
		}
	}
}

// [Access] Chain B's rights are chain A's: a viewer gets 403 on a write, a
// maintainer of the owner team saves the team's and an owned repository's
// chain B, a non-member gets 404 on the team route, a maintainer of another
// team 403 on the repository, and the global route is admins' only.
func TestPanelChainRights(t *testing.T) {
	te := newTeamsEnv(t)
	setPanelFloor(t, te.env, panelFloor, panelImageOK)
	own := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	body := entryBody("team/" + own.ID)
	teamPanel, repoPanel := teamURL(te.payments, "/panel-chain"), repoPanelURL("acme/payments")

	if rec := te.sendJSON(te.viewer, "PUT", teamPanel, body); rec.Code != http.StatusForbidden || errorOf(t, rec) != msgTeamWrite {
		t.Errorf("team PUT as a viewer = %d %s", rec.Code, rec.Body)
	}
	for name, c := range map[string]*http.Cookie{"viewer": te.viewer, "other maintainer": te.otherMaintainer} {
		if rec := te.sendJSON(c, "PUT", repoPanel, body); rec.Code != http.StatusForbidden || errorOf(t, rec) != msgRepoChainWrite {
			t.Errorf("repo PUT as %s = %d %s", name, rec.Code, rec.Body)
		}
		wantCode(t, te.send(c, "POST", repoPanel+"/reset", ""), "reset as "+name, http.StatusForbidden)
	}
	wantCode(t, te.get(te.other, teamPanel), "team GET as a non-member", http.StatusNotFound)
	wantCode(t, te.sendJSON(te.other, "PUT", teamPanel, body), "team PUT as a non-member", http.StatusNotFound)
	stranger := te.sessionFor(te.sso("stranger@example.com"))
	te.team("elsewhere", []store.TeamMember{{AccountID: te.accountOf(stranger), Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "gh"})
	wantCode(t, te.get(stranger, repoPanel), "repo GET as a stranger", http.StatusNotFound)
	wantCode(t, te.sendJSON(stranger, "PUT", repoPanel, body), "repo PUT as a stranger", http.StatusNotFound)
	for _, c := range []*http.Cookie{te.maintainer, te.viewer} {
		wantCode(t, te.get(c, adminPanelURL), "admin GET as a user", http.StatusNotFound)
		wantCode(t, te.sendJSON(c, "PUT", adminPanelURL, entryBody()), "admin PUT as a user", http.StatusNotFound)
	}
	if b, _ := te.st.ChainsOf(ctx, store.ChainB); len(b) != 0 {
		t.Fatalf("a refused write stored chain B: %+v", b)
	}

	decode[ChainView](t, te.sendJSON(te.maintainer, "PUT", teamPanel, body), "team PUT as the maintainer", http.StatusOK)
	v := decode[ChainView](t, te.sendJSON(te.maintainer, "PUT", repoPanel, body), "repo PUT as the maintainer", http.StatusOK)
	if v.ReadOnly || v.Source != "repository" {
		t.Errorf("repo chain B as the maintainer = %+v", v)
	}
	if v := getJSON[ChainView](te.env, te.viewer, repoPanel); !v.ReadOnly || len(v.Choices) != 0 || v.Source != "repository" {
		t.Errorf("repo chain B as a viewer = %+v", v)
	}
	routes := adminRoutesOf(t)
	for _, want := range []string{"GET " + adminPanelURL, "PUT " + adminPanelURL} {
		if !slices.Contains(routes, want) {
			t.Errorf("%s is not registered through adminRoute", want)
		}
	}
}

// A maintainer of one team can't name another team's entry in its chain B,
// at the team or the repository level.
func TestPanelChainForeignEntry(t *testing.T) {
	te := newTeamsEnv(t)
	setPanelFloor(t, te.env, panelFloor, panelImageOK)
	foreign := te.addTeamModel(te.otherMaintainer, te.widgets, "theirs", teamKeyTwo)
	for _, url := range []string{teamURL(te.payments, "/panel-chain"), repoPanelURL("acme/payments")} {
		rec := te.sendJSON(te.maintainer, "PUT", url, entryBody("team/"+foreign.ID))
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgForeignTeamEntry {
			t.Errorf("PUT %s = %d %s", url, rec.Code, rec.Body)
		}
	}
	if b, _ := te.st.ChainsOf(ctx, store.ChainB); len(b) != 0 {
		t.Errorf("chain B stored = %+v", b)
	}
}

// A config file entry in the global chain B gets chain B's own 400, not
// chain A's "stay at the front", which no chain B body can meet.
func TestPanelChainGlobalConfigEntry(t *testing.T) {
	t.Setenv("PERGENT_TEST_LLM_KEY", llmKeyThree)
	e := newEnv(t, func(cfg *config.Config) {
		cfg.LLM = config.LLM{Provider: "anthropic", Model: "from-config", APIKeyEnv: "PERGENT_TEST_LLM_KEY"}
	})
	setPanelFloor(t, e, panelFloor, panelImageOK)
	c := e.operator()
	a := e.addModel("openai", "gpt", "", llmKeyTwo)
	for name, body := range map[string]map[string]any{
		"alone":    entryBody("global/config"),
		"in front": entryBody("global/config", "global/"+a.ID),
		"behind":   entryBody("global/"+a.ID, "global/config"),
	} {
		rec := e.sendJSON(c, "PUT", adminPanelURL, body)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != "the global chain B holds dashboard entries only" {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if b, _ := e.st.ChainsOf(ctx, store.ChainB); len(b) != 0 {
		t.Errorf("chain B stored = %+v", b)
	}
}

// An empty repository chain B save is 400 with chain B's ways out: inherit
// or off. Chain A's message is unchanged.
func TestPanelChainEmptyRepo(t *testing.T) {
	te := newTeamsEnv(t)
	setPanelFloor(t, te.env, panelFloor, panelImageOK)
	rec := te.sendJSON(te.maintainer, "PUT", repoPanelURL("acme/payments"), entryBody())
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != "a repository's chain B needs an entry: reset it to inherit, or turn it off" {
		t.Errorf("chain B = %d %s", rec.Code, rec.Body)
	}
	rec = te.sendJSON(te.maintainer, "PUT", repoChainURL("acme/payments"), entryBody())
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgEmptyRepoChain {
		t.Errorf("chain A = %d %s", rec.Code, rec.Body)
	}
	if b, _ := te.st.ChainsOf(ctx, store.ChainB); len(b) != 0 {
		t.Errorf("chain B stored = %+v", b)
	}
}

// A maintainer of the owner team turns the panel off with the floor unset
// (no image check): no entries, mode off, audited; a reset inherits again.
func TestPanelChainOff(t *testing.T) {
	te := newTeamsEnv(t)
	te.gadgetsOwnedByPayments()
	own := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	te.saveB(store.Chain{Scope: store.ScopeTeam, TeamID: te.payments, Entries: []store.EntryRef{{Source: store.SourceTeam, ID: own.ID}}})
	te.saveB(store.Chain{Scope: store.ScopeRepo, Instance: "main", Project: "acme/gadgets", Entries: []store.EntryRef{{Source: store.SourceTeam, ID: own.ID}}})
	url := repoPanelURL("acme/gadgets")

	v := decode[ChainView](t, te.send(te.maintainer, "POST", url+"/off", ""), "off", http.StatusOK)
	if v.Mode != store.PanelOff || len(v.Entries) != 0 || v.ReadOnly {
		t.Errorf("after off = %+v", v)
	}
	if v := getJSON[ChainView](te.env, te.viewer, url); v.Mode != store.PanelOff || len(v.Entries) != 0 || !v.ReadOnly {
		t.Errorf("GET after off = %+v", v)
	}
	if _, found, _ := te.st.ChainOf(ctx, store.ChainB, store.ScopeRepo, 0, "main", "acme/gadgets"); found {
		t.Error("off kept the repository chain B")
	}
	v = decode[ChainView](t, te.send(te.maintainer, "POST", url+"/reset", ""), "reset", http.StatusOK)
	if v.Mode != store.PanelInherit || v.Source != "team" || !slices.Equal(chainRefs(v), []string{"team/" + own.ID}) {
		t.Errorf("after reset = %+v", v)
	}
	if got, want := te.panelAudit(), []string{
		"main acme/gadgets scope=repo mode=off",
		"main acme/gadgets scope=repo mode=inherit",
	}; !slices.Equal(got, want) {
		t.Errorf("panel_chain_changed = %q; want %q", got, want)
	}
}

// [Ruling 4] Off works on a repository the config file switches on (no
// dashboard row) too, over its owner team's chain B; a reset brings the team
// chain B back.
func TestPanelChainOffConfigRepository(t *testing.T) {
	te := newTeamsEnv(t)
	own := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	te.saveB(store.Chain{Scope: store.ScopeTeam, TeamID: te.payments, Entries: []store.EntryRef{{Source: store.SourceTeam, ID: own.ID}}})
	url := repoPanelURL("acme/payments")
	if _, found := te.storedRepo("main", "acme/payments"); found {
		t.Fatal("acme/payments has a dashboard row; the test needs a config-file repository")
	}
	loadB := func() chain.Input {
		t.Helper()
		repo, found, err := te.d.Repos.Get(ctx, "main", "acme/payments")
		if err != nil || !found {
			t.Fatal(found, err)
		}
		in, err := chain.LoadSet(ctx, te.st, te.reg, te.clock.now(), store.Run{}, repo, store.ChainB)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	if in := loadB(); in.Scope != store.ScopeTeam {
		t.Fatalf("before off: chain B scope = %q", in.Scope)
	}
	v := decode[ChainView](t, te.send(te.maintainer, "POST", url+"/off", ""), "off", http.StatusOK)
	if v.Mode != store.PanelOff || len(v.Entries) != 0 || v.Source != "repository" {
		t.Errorf("after off = %+v", v)
	}
	if v := getJSON[ChainView](te.env, te.viewer, url); v.Mode != store.PanelOff || len(v.Entries) != 0 {
		t.Errorf("GET after off = %+v", v)
	}
	if in := loadB(); in.Refs != nil || in.Scope != "" {
		t.Errorf("LoadSet(ChainB) after off = %+v %q; want no chain", in.Refs, in.Scope)
	}
	v = decode[ChainView](t, te.send(te.maintainer, "POST", url+"/reset", ""), "reset", http.StatusOK)
	if v.Mode != store.PanelInherit || v.Source != "team" || !slices.Equal(chainRefs(v), []string{"team/" + own.ID}) {
		t.Errorf("after reset = %+v", v)
	}
	if in := loadB(); in.Scope != store.ScopeTeam {
		t.Errorf("after reset: chain B scope = %q; want the team's", in.Scope)
	}
	if got, want := te.panelAudit(), []string{
		"main acme/payments scope=repo mode=off",
		"main acme/payments scope=repo mode=inherit",
	}; !slices.Equal(got, want) {
		t.Errorf("panel_chain_changed = %q; want %q", got, want)
	}
}

// [Access] A viewer, and a maintainer of a team that does not own the
// repository, get 403 on off; nothing changes.
func TestPanelChainOffRights(t *testing.T) {
	te := newTeamsEnv(t)
	te.gadgetsOwnedByPayments()
	url := repoPanelURL("acme/gadgets") + "/off"
	rec := te.send(te.viewer, "POST", url, "")
	if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgRepoChainWrite {
		t.Errorf("off as a viewer = %d %s", rec.Code, rec.Body)
	}
	if mode, _ := te.st.RepoPanelMode(ctx, "main", "acme/gadgets"); mode != store.PanelInherit {
		t.Errorf("mode after a refused off = %q", mode)
	}
	if got := te.panelAudit(); len(got) != 0 {
		t.Errorf("panel_chain_changed = %q", got)
	}
}

// No chain B response, to anyone, holds a key.
func TestPanelChainNoKeyInResponses(t *testing.T) {
	te := newTeamsEnv(t)
	setPanelFloor(t, te.env, panelFloor, panelImageOK)
	te.gadgetsOwnedByPayments()
	shared, _ := te.sharedAndPrivate()
	own := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	te.sendJSON(te.admin, "PUT", adminPanelURL, entryBody("global/"+shared.ID))
	te.sendJSON(te.maintainer, "PUT", teamURL(te.payments, "/panel-chain"), entryBody("team/"+own.ID))
	te.sendJSON(te.maintainer, "PUT", repoPanelURL("acme/gadgets"), entryBody("team/"+own.ID, "global/"+shared.ID))
	te.markEntry(store.EntryRef{Source: store.SourceTeam, ID: own.ID}, store.ClassAuth, nil)
	te.get(te.admin, adminPanelURL)
	for _, c := range []*http.Cookie{te.admin, te.maintainer, te.viewer} {
		te.get(c, teamURL(te.payments, "/panel-chain"))
		te.get(c, repoPanelURL("acme/gadgets"))
		te.get(c, repoPanelURL("acme/payments"))
		te.get(c, "/api/repositories/main/acme/gadgets")
	}
	te.send(te.maintainer, "POST", repoPanelURL("acme/gadgets")+"/off", "")
	te.send(te.maintainer, "POST", repoPanelURL("acme/gadgets")+"/reset", "")
	te.get(te.admin, "/api/audit")
	te.wantNoSecret(llmKeyOne, llmKeyTwo, teamKeyOne)
}

// The repository view's panel: on with the entry names of both chains while
// a chain B is in effect, off with none, and off in mode off even with a
// team chain B.
func TestRepositoryViewPanel(t *testing.T) {
	te := newTeamsEnv(t)
	te.gadgetsOwnedByPayments()
	te.addModel("openai", "gpt-default", "", llmKeyOne)
	own := te.addTeamModel(te.maintainer, te.payments, "own", teamKeyOne)
	panel := func(what string) map[string]any {
		t.Helper()
		v := getJSON[struct {
			Panel map[string]any `json:"panel"`
		}](te.env, te.viewer, "/api/repositories/main/acme/gadgets")
		return v.Panel
	}
	want := func(what string, got, want map[string]any) {
		t.Helper()
		if !maps.Equal(got, want) {
			t.Errorf("%s: panel = %v; want %v", what, got, want)
		}
	}
	want("no chain B", panel("none"), map[string]any{"on": false, "mode": "inherit"})
	te.saveB(store.Chain{Scope: store.ScopeTeam, TeamID: te.payments, Entries: []store.EntryRef{{Source: store.SourceTeam, ID: own.ID}}})
	want("team chain B", panel("team"), map[string]any{"on": true, "mode": "inherit", "a": "openai/gpt-default", "b": "own"})
	te.saveB(store.Chain{Scope: store.ScopeRepo, Instance: "main", Project: "acme/gadgets",
		Entries: []store.EntryRef{{Source: store.SourceGlobal, ID: store.ConfigModelID}, {Source: store.SourceTeam, ID: own.ID}}})
	want("custom, position 1 unknown", panel("custom"), map[string]any{"on": true, "mode": "custom", "a": "openai/gpt-default"})
	if err := te.st.SetRepoPanelOff(ctx, "main", "acme/gadgets", te.clock.now()); err != nil {
		t.Fatal(err)
	}
	want("off", panel("off"), map[string]any{"on": false, "mode": "off"})
}

// [Ruling 3a] Removing a team entry names the chain B chains it left too,
// after chain A's; with no chain B naming it the line is as before.
func TestTeamModelRemovedNamesChainB(t *testing.T) {
	te := newTeamsEnv(t)
	te.gadgetsOwnedByPayments()
	one := te.addTeamModel(te.maintainer, te.payments, "one", teamKeyOne)
	two := te.addTeamModel(te.maintainer, te.payments, "two", teamKeyTwo)
	decode[ChainView](t, te.sendJSON(te.maintainer, "PUT", teamURL(te.payments, "/chain"), entryBody("team/"+one.ID, "team/"+two.ID)), "PUT team chain", http.StatusOK)
	te.saveB(store.Chain{Scope: store.ScopeTeam, TeamID: te.payments, Entries: []store.EntryRef{{Source: store.SourceTeam, ID: one.ID}}})
	te.saveB(store.Chain{Scope: store.ScopeRepo, Instance: "main", Project: "acme/gadgets", Entries: []store.EntryRef{{Source: store.SourceTeam, ID: one.ID}}})
	wantCode(t, te.send(te.maintainer, "DELETE", teamURL(te.payments, "/models/"+one.ID), ""), "remove one", http.StatusNoContent)
	wantCode(t, te.send(te.maintainer, "DELETE", teamURL(te.payments, "/models/"+two.ID), ""), "remove two", http.StatusNoContent)
	if got, want := auditDetails(te.env, "team_model_removed"), []string{
		"Payments: one (openai / gpt-one), removed from team Payments, removed from chain B: team Payments, main/acme/gadgets",
		"Payments: two (openai / gpt-two), removed from team Payments",
	}; !slices.Equal(got, want) {
		t.Errorf("team_model_removed = %q; want %q", got, want)
	}
	// The log line names chain B only when a chain B changed, so it is as
	// before otherwise.
	var lines []string
	for _, l := range strings.Split(te.logs.String(), "\n") {
		if strings.Contains(l, `msg="dashboard: team model removed"`) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 || !strings.Contains(lines[0], " chains=1 panel_chains=2 by=") ||
		!strings.Contains(lines[1], " chains=1 by=") || strings.Contains(lines[1], "panel_chains") {
		t.Errorf("team model removed lines = %q", lines)
	}
}
