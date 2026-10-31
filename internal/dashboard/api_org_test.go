package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const operatorPassword = "operator-password-1"

// signInOperator stores the password account root and signs it in.
func (e *env) signInOperator() *http.Cookie {
	e.t.Helper()
	e.addPasswordAccount("root", operatorPassword, false)
	return e.signInPassword("root", operatorPassword)
}

// platformAccount stores a platform account of instance with the given
// platform user ID and username.
func (e *env) platformAccount(instance string, userID int64, username string) int64 {
	e.t.Helper()
	platform := map[string]string{"main": config.PlatformGitLab, "gh": config.PlatformGitHub}[instance]
	a, err := e.st.UpsertPlatformAccount(ctx, platform, instance, userID, username, e.clock.now())
	if err != nil {
		e.t.Fatal(err)
	}
	return a.ID
}

// decode wants code from rec and decodes its body into a T.
func decode[T any](t *testing.T, rec *httptest.ResponseRecorder, what string, code int) T {
	t.Helper()
	var v T
	if rec.Code != code {
		t.Fatalf("%s = %d %s; want %d", what, rec.Code, rec.Body, code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("%s body %s: %v", what, rec.Body, err)
	}
	return v
}

// storedTeam is the team id as stored.
func (e *env) storedTeam(id int64) store.Team {
	e.t.Helper()
	tm, found, err := e.st.Team(ctx, id)
	if err != nil || !found {
		e.t.Fatalf("Team(%d) = %v, %v", id, found, err)
	}
	return tm
}

// auditEntries is the whole audit log, newest first.
func (e *env) auditEntries() []store.AuditEntry {
	e.t.Helper()
	entries, err := e.st.AuditLog(ctx, 0, 10000)
	if err != nil {
		e.t.Fatal(err)
	}
	return entries
}

func (e *env) addAudit(entry store.AuditEntry) {
	e.t.Helper()
	entry.At = e.clock.now()
	if err := e.st.Audit(ctx, entry); err != nil {
		e.t.Fatal(err)
	}
}

// Waiting accounts (platform accounts on no team) come first, marked
// waiting, and every account lists its teams.
func TestUsersWaitingFirst(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	bob := e.platformAccount("main", 20, "bob")
	zed := e.platformAccount("gh", 21, "zed")
	e.platformAccount("main", 22, "yvonne")
	team := e.team("devs", store.RoleViewer, []int64{bob}, store.TeamRepo{Instance: "main"})
	e.team("more", store.RoleViewer, []int64{bob})

	users := decode[[]UserView](t, e.get(c, "/api/users"), "GET /api/users", http.StatusOK)
	var names []string
	for _, u := range users {
		names = append(names, u.Username)
	}
	if !slices.Equal(names, []string{"yvonne", "zed", "bob", "root"}) {
		t.Fatalf("users = %v; want the waiting yvonne and zed first", names)
	}
	for _, u := range users {
		if u.Waiting != (u.Username == "yvonne" || u.Username == "zed") {
			t.Errorf("%s waiting = %v", u.Username, u.Waiting)
		}
		if u.Teams == nil {
			t.Errorf("%s teams is null", u.Username)
		}
	}
	if u := users[1]; u.ID != zed || u.Kind != store.AccountPlatform || u.Platform != config.PlatformGitHub || u.Instance != "gh" {
		t.Errorf("zed = %+v", u)
	}
	if u := users[2]; len(u.Teams) != 2 || u.Teams[0] != (TeamRef{ID: team, Name: "devs"}) || u.Teams[1].Name != "more" {
		t.Errorf("bob's teams = %+v", u.Teams)
	}
	if u := users[3]; u.Kind != store.AccountPassword || u.Platform != "" || u.LastSignInAt == nil || u.Waiting {
		t.Errorf("root = %+v", u)
	}
}

// A removed account's sessions end with it: its next request is 401, and
// its next sign-in makes it again, on no team.
func TestDeleteAccountEndsSessions(t *testing.T) {
	e := newEnv(t, nil)
	op := e.signInOperator()
	alice := e.signInGitLab(false, "acme/widgets")
	id := e.accountOf(alice)

	wantCode(t, e.send(op, "DELETE", "/api/users/"+strconv.FormatInt(id, 10), ""), "DELETE alice", http.StatusNoContent)
	if code, _ := e.me(alice); code != http.StatusUnauthorized {
		t.Errorf("alice's next request = %d; want 401", code)
	}
	wantCode(t, e.send(op, "DELETE", "/api/users/"+strconv.FormatInt(id, 10), ""), "DELETE alice again", http.StatusNotFound)
	wantCode(t, e.send(op, "DELETE", "/api/users/nope", ""), "DELETE nope", http.StatusNotFound)

	again := e.signIn("gitlab", "main")
	if code, me := e.me(again); code != http.StatusOK || !me.Waiting {
		t.Errorf("alice signed in again: %d %+v; want waiting", code, me)
	}

	// A password account too, by another one.
	other := e.addPasswordAccount("second", operatorPassword, false)
	second := e.signInPassword("second", operatorPassword)
	wantCode(t, e.send(op, "DELETE", "/api/users/"+strconv.FormatInt(other.ID, 10), ""), "DELETE second", http.StatusNoContent)
	if code, _ := e.me(second); code != http.StatusUnauthorized {
		t.Errorf("second's next request = %d; want 401", code)
	}

	var deleted []string
	for _, a := range e.auditEntries() {
		if a.Action == "account_deleted" {
			deleted = append(deleted, a.Actor+" "+a.Detail)
		}
	}
	if !slices.Equal(deleted, []string{"root second", "root alice"}) {
		t.Errorf("account_deleted entries = %v", deleted)
	}
}

// A platform admin may remove platform accounts but never a password
// account: 403, and it stays.
func TestPlatformAdminCannotDeletePasswordAccount(t *testing.T) {
	e := newEnv(t, nil)
	root := e.addPasswordAccount("root", operatorPassword, false)
	e.addPasswordAccount("second", operatorPassword, false)
	c := e.signInGitLab(true, "acme/widgets")
	bob := e.platformAccount("main", 20, "bob")

	wantCode(t, e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(root.ID, 10), ""), "DELETE root", http.StatusForbidden)
	if _, found, _ := e.st.Account(ctx, root.ID); !found {
		t.Error("root was removed")
	}
	wantCode(t, e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(bob, 10), ""), "DELETE bob", http.StatusNoContent)
	if _, found, _ := e.st.Account(ctx, bob); found {
		t.Error("bob is still there")
	}
}

// The last password account can't be removed: 409.
func TestDeleteLastPasswordAccount(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	rec := e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(e.accountOf(c), 10), "")
	wantCode(t, rec, "DELETE the last password account", http.StatusConflict)
	if got := body(rec); got != `{"error":"the last password account can't be removed"}` {
		t.Errorf("body = %s", got)
	}
	if code, _ := e.me(c); code != http.StatusOK {
		t.Errorf("the operator's session = %d after the refused removal", code)
	}
}

// An operator adds a password admin and gets its generated password, which
// signs in and must be changed; a platform admin can't; a bad or taken
// username is refused.
func TestCreatePasswordAccount(t *testing.T) {
	e := newEnv(t, nil)
	op := e.signInOperator()

	got := decode[NewAccountView](t, e.send(op, "POST", "/api/admin/accounts", `{"username":"ops.Second-1"}`),
		"POST account", http.StatusCreated)
	if got.Username != "ops.Second-1" || len(got.Password) < 24 {
		t.Fatalf("created = %+v", got)
	}
	c := e.signInPassword("ops.Second-1", got.Password)
	if code, me := e.me(c); code != http.StatusOK || !me.MustChangePassword || !me.Operator {
		t.Errorf("the new admin: %d %+v; want an operator who must change the password", code, me)
	}
	if strings.Contains(e.logs.String(), got.Password) {
		t.Error("the generated password was logged")
	}

	wantCode(t, e.send(op, "POST", "/api/admin/accounts", `{"username":"OPS.second-1"}`), "POST a taken username", http.StatusConflict)
	for _, b := range []string{`{"username":""}`, `{"username":"has space"}`, `{"username":"a/b"}`,
		`{"username":"` + strings.Repeat("a", 65) + `"}`, `{}`, `{"username":"x","password":"y"}`, `nope`} {
		wantCode(t, e.send(op, "POST", "/api/admin/accounts", b), "POST "+b, http.StatusBadRequest)
	}
	wantCode(t, e.send(op, "POST", "/api/admin/accounts", `{"username":"`+strings.Repeat("a", 64)+`"}`), "POST 64 characters", http.StatusCreated)

	admin := e.signInGitLab(true, "acme/widgets")
	wantCode(t, e.send(admin, "POST", "/api/admin/accounts", `{"username":"sneaky"}`), "POST by a platform admin", http.StatusForbidden)
	if _, found, _ := e.st.PasswordAccount(ctx, "sneaky"); found {
		t.Error("a platform admin made a password account")
	}
	var created []string
	for _, a := range e.auditEntries() {
		if a.Action == "account_created" {
			created = append(created, a.Actor+" "+a.Detail)
		}
	}
	if len(created) != 2 || created[1] != "root ops.Second-1" {
		t.Errorf("account_created entries = %v", created)
	}
}

// Teams are created, listed, changed and removed, each change audited.
func TestTeamCRUD(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	bob := e.platformAccount("main", 20, "bob")
	zed := e.platformAccount("gh", 21, "zed")

	created := decode[TeamView](t, e.send(c, "POST", "/api/teams", `{"name":"devs","role":"viewer"}`), "POST team", http.StatusCreated)
	if created.ID == 0 || created.Name != "devs" || created.Role != "viewer" || created.Members == nil || created.Repos == nil {
		t.Fatalf("created = %+v", created)
	}
	wantCode(t, e.send(c, "POST", "/api/teams", `{"name":"DEVS","role":"admin"}`), "POST a taken name", http.StatusConflict)
	for _, b := range []string{`{"name":"","role":"viewer"}`, `{"name":"x","role":"owner"}`, `{"name":"x"}`, `{"name":"x","role":"viewer","y":1}`} {
		wantCode(t, e.send(c, "POST", "/api/teams", b), "POST "+b, http.StatusBadRequest)
	}

	target := "/api/teams/" + strconv.FormatInt(created.ID, 10)
	put := fmt.Sprintf(`{"name":"developers","role":"maintainer","members":[%d,%d,%d],"repos":[
		{"instance":"main","project":"ACME/widgets"},{"instance":"gh","project":""}]}`, bob, zed, bob)
	updated := decode[TeamView](t, e.send(c, "PUT", target, put), "PUT team", http.StatusOK)
	if updated.Name != "developers" || updated.Role != "maintainer" || len(updated.Members) != 2 || updated.HiddenRepos != 0 {
		t.Fatalf("updated = %+v", updated)
	}
	if m := updated.Members[0]; m != (TeamMemberView{ID: bob, Username: "bob", Platform: config.PlatformGitLab, Instance: "main"}) {
		t.Errorf("member = %+v", m)
	}
	if !slices.Equal(updated.Repos, []TeamRepoView{{Instance: "gh"}, {Instance: "main", Project: "acme/widgets"}}) {
		t.Errorf("repos = %+v", updated.Repos)
	}
	stored := e.storedTeam(created.ID)
	if !slices.Equal(stored.Members, []int64{bob, zed}) || len(stored.Repos) != 2 {
		t.Errorf("stored = %+v", stored)
	}

	// A repository that isn't switched on, or an unknown instance, is 400.
	for _, repos := range []string{`[{"instance":"main","project":"acme/off"}]`, `[{"instance":"nowhere","project":""}]`,
		`[{"instance":"nowhere","project":"acme/widgets"}]`} {
		b := `{"name":"developers","role":"maintainer","members":[],"repos":` + repos + `}`
		wantCode(t, e.send(c, "PUT", target, b), "PUT "+repos, http.StatusBadRequest)
	}
	wantCode(t, e.send(c, "PUT", "/api/teams/99999", `{"name":"x","role":"viewer","members":[],"repos":[]}`), "PUT unknown", http.StatusNotFound)
	other := e.team("others", store.RoleViewer, nil)
	wantCode(t, e.send(c, "PUT", "/api/teams/"+strconv.FormatInt(other, 10), `{"name":"Developers","role":"viewer","members":[],"repos":[]}`),
		"PUT a taken name", http.StatusConflict)

	teams := decode[[]TeamView](t, e.get(c, "/api/teams"), "GET teams", http.StatusOK)
	if len(teams) != 2 || teams[0].Name != "developers" || teams[1].Name != "others" {
		t.Errorf("teams = %+v", teams)
	}

	wantCode(t, e.send(c, "DELETE", target, ""), "DELETE team", http.StatusNoContent)
	wantCode(t, e.send(c, "DELETE", target, ""), "DELETE team again", http.StatusNotFound)
	if _, found, _ := e.st.Team(ctx, created.ID); found {
		t.Error("the team is still there")
	}

	var log []string
	for _, a := range e.auditEntries() {
		if strings.HasPrefix(a.Action, "team_") {
			log = append([]string{a.Actor + " " + a.Action + " " + a.Detail}, log...)
		}
	}
	want := []string{"root team_created devs (viewer)", "root team_updated developers: name, role, members, repos",
		"root team_deleted developers"}
	if !slices.Equal(log, want) {
		t.Errorf("team audit = %q; want %q", log, want)
	}
}

// An admin team has no repository list: a PUT with repositories is 400.
func TestAdminTeamNoRepos(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	id := e.team("devs", store.RoleViewer, nil, store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	target := "/api/teams/" + strconv.FormatInt(id, 10)
	wantCode(t, e.send(c, "PUT", target, `{"name":"devs","role":"admin","members":[],"repos":[{"instance":"main","project":""}]}`),
		"PUT an admin team with repositories", http.StatusBadRequest)
	if tm := e.storedTeam(id); tm.Role != store.RoleViewer || len(tm.Repos) != 1 {
		t.Errorf("the refused change changed the team: %+v", tm)
	}
	got := decode[TeamView](t, e.send(c, "PUT", target, `{"name":"devs","role":"admin","members":[],"repos":[]}`), "PUT admin", http.StatusOK)
	if got.Role != "admin" || len(got.Repos) != 0 || len(e.storedTeam(id).Repos) != 0 {
		t.Errorf("admin team = %+v", got)
	}
}

// Only platform accounts can be members: a password account, or an unknown
// ID, is 400.
func TestTeamMembersMustBePlatform(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	bob := e.platformAccount("main", 20, "bob")
	id := e.team("devs", store.RoleViewer, []int64{bob})
	target := "/api/teams/" + strconv.FormatInt(id, 10)
	for _, members := range []string{strconv.FormatInt(e.accountOf(c), 10), "99999", `"bob"`} {
		b := `{"name":"devs","role":"viewer","members":[` + members + `],"repos":[]}`
		wantCode(t, e.send(c, "PUT", target, b), "PUT members "+members, http.StatusBadRequest)
	}
	if tm := e.storedTeam(id); !slices.Equal(tm.Members, []int64{bob}) {
		t.Errorf("members = %v", tm.Members)
	}
}

// A platform admin edits a team with repositories it can't see: those are
// kept, counted and never shown; it can't add another instance's, nor make
// the team an admin team or remove it (409). A password admin can.
func TestPlatformAdminTeamEditKeepsHidden(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInGitLab(true, "acme/widgets") // acme/payments is hidden from alice
	id := e.team("devs", store.RoleViewer, nil,
		store.TeamRepo{Instance: "main", Project: "acme/widgets"}, store.TeamRepo{Instance: "main", Project: "acme/payments"})
	target := "/api/teams/" + strconv.FormatInt(id, 10)

	got := decode[TeamView](t, e.send(c, "PUT", target, `{"name":"devs","role":"viewer","members":[],"repos":[]}`), "PUT", http.StatusOK)
	if len(got.Repos) != 0 || got.HiddenRepos != 1 {
		t.Errorf("PUT answer = %+v; want repos [], hiddenRepos 1", got)
	}
	if tm := e.storedTeam(id); !slices.Equal(tm.Repos, []store.TeamRepo{{Instance: "main", Project: "acme/payments"}}) {
		t.Errorf("stored repos = %+v; want acme/payments only", tm.Repos)
	}
	teams := decode[[]TeamView](t, e.get(c, "/api/teams"), "GET teams", http.StatusOK)
	if len(teams) != 2 || teams[1].Name != "devs" || len(teams[1].Repos) != 0 || teams[1].HiddenRepos != 1 {
		t.Errorf("GET teams = %+v", teams)
	}
	if strings.Contains(e.get(c, "/api/teams").Body.String(), "payments") {
		t.Error("GET /api/teams names the hidden repository")
	}

	// Their own instance's whole-instance entry is theirs; another's is
	// hidden, and adding one is 400.
	got = decode[TeamView](t, e.send(c, "PUT", target, `{"name":"devs","role":"viewer","members":[],"repos":[
		{"instance":"main","project":""},{"instance":"main","project":"acme/widgets"}]}`), "PUT whole main", http.StatusOK)
	if !slices.Equal(got.Repos, []TeamRepoView{{Instance: "main"}, {Instance: "main", Project: "acme/widgets"}}) || got.HiddenRepos != 1 {
		t.Errorf("PUT answer = %+v", got)
	}
	wantCode(t, e.send(c, "PUT", target, `{"name":"devs","role":"viewer","members":[],"repos":[{"instance":"gh","project":""}]}`),
		"PUT another instance", http.StatusBadRequest)
	wantCode(t, e.send(c, "PUT", target, `{"name":"devs","role":"viewer","members":[],"repos":[{"instance":"gh","project":"acme/widgets"}]}`),
		"PUT another instance's repository", http.StatusBadRequest)
	e.editTeam(id, func(tm *store.Team) { tm.Repos = append(tm.Repos, store.TeamRepo{Instance: "gh"}) })
	got = decode[TeamView](t, e.send(c, "PUT", target, `{"name":"devs","role":"viewer","members":[],"repos":[]}`), "PUT", http.StatusOK)
	if len(got.Repos) != 0 || got.HiddenRepos != 2 {
		t.Errorf("PUT answer = %+v; want repos [], hiddenRepos 2", got)
	}
	hidden := []store.TeamRepo{{Instance: "gh"}, {Instance: "main", Project: "acme/payments"}}
	if tm := e.storedTeam(id); !slices.Equal(tm.Repos, hidden) {
		t.Errorf("stored repos = %+v", tm.Repos)
	}

	// Making the team an admin team, or removing it, would drop what it
	// covers out of sight: 409 for a platform admin, and nothing changes.
	const wantConflict = `{"error":"this team covers repositories you can't see; a password admin can change its role or remove it"}`
	toAdmin := `{"name":"devs","role":"admin","members":[],"repos":[]}`
	rec := e.send(c, "PUT", target, toAdmin)
	if rec.Code != http.StatusConflict || body(rec) != wantConflict {
		t.Errorf("PUT role admin = %d %s; want 409 %s", rec.Code, rec.Body, wantConflict)
	}
	rec = e.send(c, "DELETE", target, "")
	if rec.Code != http.StatusConflict || body(rec) != wantConflict {
		t.Errorf("DELETE = %d %s; want 409 %s", rec.Code, rec.Body, wantConflict)
	}
	if tm := e.storedTeam(id); tm.Role != store.RoleViewer || !slices.Equal(tm.Repos, hidden) {
		t.Errorf("the refused changes changed the team: %+v", tm)
	}

	// A team it sees all of, it can make an admin team and remove.
	mine := e.team("mine", store.RoleViewer, nil, store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	mineTarget := "/api/teams/" + strconv.FormatInt(mine, 10)
	wantCode(t, e.send(c, "PUT", mineTarget, `{"name":"mine","role":"admin","members":[],"repos":[]}`), "PUT mine role admin", http.StatusOK)
	wantCode(t, e.send(c, "DELETE", mineTarget, ""), "DELETE mine", http.StatusNoContent)

	// A password admin sees everything: role admin clears the repositories,
	// and the team can be removed.
	op := e.signInOperator()
	got = decode[TeamView](t, e.send(op, "PUT", target, toAdmin), "operator PUT role admin", http.StatusOK)
	if got.Role != store.RoleAdmin || len(got.Repos) != 0 || got.HiddenRepos != 0 {
		t.Errorf("operator PUT answer = %+v", got)
	}
	if tm := e.storedTeam(id); tm.Role != store.RoleAdmin || len(tm.Repos) != 0 {
		t.Errorf("stored after operator PUT = %+v; want an admin team with no repos", tm)
	}
	e.editTeam(id, func(tm *store.Team) { tm.Role, tm.Repos = store.RoleViewer, hidden })
	wantCode(t, e.send(op, "DELETE", target, ""), "operator DELETE", http.StatusNoContent)
	if _, found, _ := e.st.Team(ctx, id); found {
		t.Error("the team is still there")
	}
}

// Any role change of a team that covers a repository the platform admin
// can't see is 409, not only one to admin: the hidden repository would
// otherwise change role out of its sight. A password admin can; a change
// that keeps the role is fine.
func TestPlatformAdminCannotChangeRoleOfHiddenCoverage(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInGitLab(true, "acme/widgets") // acme/payments is hidden from alice
	hidden := store.TeamRepo{Instance: "main", Project: "acme/payments"}
	id := e.team("devs", store.RoleViewer, nil, store.TeamRepo{Instance: "main", Project: "acme/widgets"}, hidden)
	target := "/api/teams/" + strconv.FormatInt(id, 10)
	const wantConflict = `{"error":"this team covers repositories you can't see; a password admin can change its role or remove it"}`

	for _, role := range []string{"maintainer", "admin"} {
		rec := e.send(c, "PUT", target, `{"name":"devs","role":"`+role+`","members":[],"repos":[]}`)
		if rec.Code != http.StatusConflict || body(rec) != wantConflict {
			t.Errorf("PUT role %s = %d %s; want 409 %s", role, rec.Code, rec.Body, wantConflict)
		}
	}
	if tm := e.storedTeam(id); tm.Role != store.RoleViewer || len(tm.Repos) != 2 {
		t.Errorf("the refused changes changed the team: %+v", tm)
	}
	wantCode(t, e.send(c, "PUT", target, `{"name":"renamed","role":"viewer","members":[],"repos":[{"instance":"main","project":"acme/widgets"}]}`),
		"PUT the same role", http.StatusOK)

	op := e.signInOperator()
	got := decode[TeamView](t, e.send(op, "PUT", target, `{"name":"renamed","role":"maintainer","members":[],"repos":[{"instance":"main","project":"acme/widgets"}]}`),
		"operator PUT maintainer", http.StatusOK)
	if got.Role != store.RoleMaintainer {
		t.Errorf("operator PUT answer = %+v", got)
	}
}

// Every write here needs the session's CSRF token: 403 with code "csrf".
func TestOrgWritesNeedCSRF(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	rec := e.doJSON("POST", "/api/teams", []*http.Cookie{c}, nil, map[string]string{"name": "devs", "role": "viewer"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /api/teams without X-CSRF-Token = %d %s; want 403", rec.Code, rec.Body)
	}
	wantCSRFCode(t, rec)
	if teams, _ := e.st.Teams(ctx); len(teams) != 0 {
		t.Errorf("a team was made: %+v", teams)
	}
}

// A platform admin sees the audit entries of the repositories it can see
// and those of no repository; an operator sees every one. Pages are 100,
// newest first, with next the before of the page after (0 at the end). A
// platform admin's request reads at most 50 chunks of 100: when that's
// not enough to fill the page, it answers what it found and where to go on.
func TestAuditFiltered(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("root", operatorPassword, false)
	c := e.signInGitLab(true, "acme/widgets")
	e.addAudit(store.AuditEntry{Actor: "carol", Action: "repository_on", Instance: "main", Project: "acme/widgets"})
	e.exec(`INSERT INTO audit_log (at, actor, action, instance, project)
		SELECT now(), 'carol', 'settings_reset', 'main', 'acme/payments' FROM generate_series(1, 5100)`)
	e.addAudit(store.AuditEntry{Actor: "carol", Action: "repository_off", Instance: "gh", Project: "acme/widgets"})
	e.addAudit(store.AuditEntry{Actor: "carol", Action: "team_created", Detail: "devs (viewer)"})
	all := e.auditEntries()

	actions := func(entries []AuditView) []string {
		var out []string
		for _, a := range entries {
			out = append(out, a.Action+" "+a.Project)
			if a.Project == "acme/payments" || a.Instance == "gh" {
				t.Errorf("a hidden entry is shown: %+v", a)
			}
		}
		return out
	}
	// The newest and the hidden gh one, then 4998 hidden: 50 chunks, and
	// the page stops with the one entry found.
	page := decode[AuditPageView](t, e.get(c, "/api/audit"), "GET audit", http.StatusOK)
	if got := actions(page.Entries); !slices.Equal(got, []string{"team_created "}) || page.Next != all[4999].ID {
		t.Errorf("platform admin's first page = %v, next %d; want [team_created], next %d", got, page.Next, all[4999].ID)
	}
	page = decode[AuditPageView](t, e.get(c, "/api/audit?before="+strconv.FormatInt(page.Next, 10)), "GET audit", http.StatusOK)
	if got := actions(page.Entries); !slices.Equal(got, []string{"repository_on acme/widgets"}) || page.Next != 0 {
		t.Errorf("platform admin's second page = %v, next %d; want [repository_on acme/widgets], next 0", got, page.Next)
	}

	op := e.signInPassword("root", operatorPassword)
	first := decode[AuditPageView](t, e.get(op, "/api/audit"), "GET audit", http.StatusOK)
	if len(first.Entries) != 100 || first.Entries[0].Action != "sign_in" || first.Entries[0].ID <= first.Entries[99].ID ||
		first.Next != first.Entries[99].ID {
		t.Fatalf("operator's first page: %d entries, first %+v, next %d", len(first.Entries), first.Entries[0], first.Next)
	}
	var seen []AuditView
	for p, pages := first, 1; ; pages++ {
		seen = append(seen, p.Entries...)
		if p.Next == 0 {
			break
		}
		if pages > 60 {
			t.Fatalf("still paging after %d pages", pages)
		}
		p = decode[AuditPageView](t, e.get(op, "/api/audit?before="+strconv.FormatInt(p.Next, 10)), "GET older", http.StatusOK)
	}
	if all := e.auditEntries(); len(seen) != len(all) {
		t.Errorf("the pages hold %d entries; the log has %d", len(seen), len(all))
	}
	if oldest := seen[len(seen)-1]; oldest.Actor != "carol" || oldest.Action != "repository_on" {
		t.Errorf("oldest = %+v", oldest)
	}
	for _, q := range []string{"?before=x", "?before=-1", "?before=0"} {
		wantCode(t, e.get(op, "/api/audit"+q), "GET audit"+q, http.StatusBadRequest)
	}
	empty := e.get(op, "/api/audit?before=1")
	if empty.Code != http.StatusOK || body(empty) != `{"entries":[],"next":0}` {
		t.Errorf("GET before the first = %d %s; want no entries, next 0", empty.Code, empty.Body)
	}
}

// No audit entry holds a generated password.
func TestAuditNoSecrets(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	bob := e.platformAccount("main", 20, "bob")
	created := decode[NewAccountView](t, e.send(c, "POST", "/api/admin/accounts", `{"username":"second"}`), "POST account", http.StatusCreated)
	team := decode[TeamView](t, e.send(c, "POST", "/api/teams", `{"name":"devs","role":"viewer"}`), "POST team", http.StatusCreated)
	target := "/api/teams/" + strconv.FormatInt(team.ID, 10)
	wantCode(t, e.send(c, "PUT", target, fmt.Sprintf(`{"name":"devs","role":"maintainer","members":[%d],"repos":[{"instance":"main","project":""}]}`, bob)),
		"PUT team", http.StatusOK)
	wantCode(t, e.send(c, "DELETE", target, ""), "DELETE team", http.StatusNoContent)
	acc, _, _ := e.st.PasswordAccount(ctx, "second")
	wantCode(t, e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(acc.ID, 10), ""), "DELETE second", http.StatusNoContent)

	entries := e.auditEntries()
	if len(entries) < 6 {
		t.Errorf("only %d audit entries", len(entries))
	}
	for _, a := range entries {
		if strings.Contains(fmt.Sprintf("%+v", a), created.Password) {
			t.Errorf("an audit entry holds the generated password: %+v", a)
		}
	}
	if all := body(e.get(c, "/api/audit")); strings.Contains(all, created.Password) {
		t.Error("GET /api/audit holds the generated password")
	}
}

// Switching on, changing settings, resetting them and switching off each
// leave one entry naming the actor and the repository.
func TestWritesAudited(t *testing.T) {
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets")
	c := e.signInGitLab(true, "acme/gadgets")
	id := e.accountOf(c)
	base := "/api/repositories/main/acme/gadgets"

	wantCode(t, e.send(c, "POST", "/api/repositories", `{"project":"acme/gadgets"}`), "POST", http.StatusCreated)
	wantCode(t, e.send(c, "PUT", base+"/settings", `{"skills":["style"],"approve":false}`), "PUT settings", http.StatusOK)
	wantCode(t, e.send(c, "PUT", base+"/settings", `{"approve":"no"}`), "PUT bad settings", http.StatusBadRequest)
	wantCode(t, e.send(c, "POST", base+"/settings/reset", ""), "POST reset", http.StatusOK)
	wantCode(t, e.send(c, "DELETE", base, ""), "DELETE", http.StatusOK)

	var got []string
	for _, a := range e.auditEntries() {
		if a.Action == "sign_in" {
			continue
		}
		if a.ActorID == nil || *a.ActorID != id {
			t.Errorf("%s actor ID = %v; want %d", a.Action, a.ActorID, id)
		}
		got = append([]string{a.Actor + " " + a.Action + " " + a.Instance + "/" + a.Project + " " + a.Detail}, got...)
	}
	want := []string{
		"alice repository_on main/acme/gadgets ",
		"alice settings_updated main/acme/gadgets approve, skills",
		"alice settings_reset main/acme/gadgets ",
		"alice repository_off main/acme/gadgets ",
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit = %q; want %q", got, want)
	}
}

// A viewer (no admin) gets 403 on every route here, and nothing changes.
func TestDeveloperGets403OnOrgRoutes(t *testing.T) {
	e := newEnv(t, nil)
	root := e.addPasswordAccount("root", operatorPassword, false)
	c := e.signInGitLab(false, "acme/widgets")
	bob := e.platformAccount("main", 20, "bob")
	team := e.team("devs", store.RoleViewer, nil)
	tid := strconv.FormatInt(team, 10)
	for _, r := range []struct{ method, target, body string }{
		{"GET", "/api/users", ""},
		{"DELETE", "/api/users/" + strconv.FormatInt(bob, 10), ""},
		{"DELETE", "/api/users/" + strconv.FormatInt(root.ID, 10), ""},
		{"POST", "/api/admin/accounts", `{"username":"sneaky"}`},
		{"GET", "/api/teams", ""},
		{"POST", "/api/teams", `{"name":"mine","role":"admin"}`},
		{"PUT", "/api/teams/" + tid, `{"name":"devs","role":"admin","members":[],"repos":[]}`},
		{"DELETE", "/api/teams/" + tid, ""},
		{"GET", "/api/audit", ""},
	} {
		wantCode(t, e.send(c, r.method, r.target, r.body), r.method+" "+r.target, http.StatusForbidden)
	}
	if _, found, _ := e.st.Account(ctx, bob); !found {
		t.Error("bob was removed")
	}
	if tm := e.storedTeam(team); tm.Role != store.RoleViewer {
		t.Errorf("team = %+v", tm)
	}
	if teams, _ := e.st.Teams(ctx); len(teams) != 2 {
		t.Errorf("%d teams; want devs and alice's", len(teams))
	}
	if _, found, _ := e.st.PasswordAccount(ctx, "sneaky"); found {
		t.Error("a viewer made a password account")
	}
}
