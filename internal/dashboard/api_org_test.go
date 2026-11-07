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

// signInOperator stores the local admin root and signs it in.
func (e *env) signInOperator() *http.Cookie {
	e.t.Helper()
	e.addPasswordAccount("root", operatorPassword, false)
	return e.signInPassword("root", operatorPassword)
}

// platformAccount stores an old platform account of instance, which nothing
// reaches any more: the Users page must not show it.
func (e *env) platformAccount(instance string, userID int64, username string) int64 {
	e.t.Helper()
	platform := map[string]string{"main": config.PlatformGitLab, "gh": config.PlatformGitHub}[instance]
	a, err := e.st.UpsertPlatformAccount(ctx, platform, instance, userID, username, e.clock.now())
	if err != nil {
		e.t.Fatal(err)
	}
	return a.ID
}

// invited stores the SSO account of email as an admin adds it before its
// first sign-in.
func (e *env) invited(email string) int64 {
	e.t.Helper()
	a, err := e.st.AddSSOAccount(ctx, email, e.clock.now())
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

// Accounts are listed by name: SSO accounts by email, with their role, whether
// they were added and have not signed in (invited), whether they sign in and
// wait for a team, and their teams; the old platform accounts are not there.
func TestUsersListed(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	alice := e.alice()
	e.invited("bob@example.com")
	e.sso("yvonne@example.com")
	e.ssoAdmin("zed@example.com")
	e.platformAccount("main", 20, "old-carol")
	team := e.team("devs", []store.TeamMember{{AccountID: alice, Role: store.RoleMaintainer}}, store.TeamRepo{Instance: "main"})
	e.team("more", []store.TeamMember{{AccountID: alice, Role: store.RoleViewer}})

	users := decode[[]UserView](t, e.get(c, "/api/users"), "GET /api/users", http.StatusOK)
	var names []string
	for _, u := range users {
		names = append(names, u.Username)
		if u.Teams == nil {
			t.Errorf("%s teams is null", u.Username)
		}
	}
	if want := []string{aliceEmail, "bob@example.com", "root", "yvonne@example.com", "zed@example.com"}; !slices.Equal(names, want) {
		t.Fatalf("users = %v; want %v (no platform account)", names, want)
	}
	byName := map[string]UserView{}
	for _, u := range users {
		byName[u.Username] = u
	}
	if u := byName[aliceEmail]; u.ID != alice || u.Kind != store.AccountSSO || u.Email != aliceEmail || u.Role != store.RoleUser ||
		u.Invited || u.Waiting || u.LastSignInAt == nil || len(u.Teams) != 2 || u.Teams[0] != (TeamRef{ID: team, Name: "devs"}) || u.Teams[1].Name != "more" {
		t.Errorf("alice = %+v", u)
	}
	if u := byName["bob@example.com"]; !u.Invited || u.Waiting || u.LastSignInAt != nil || u.Role != store.RoleUser {
		t.Errorf("bob (added, never signed in) = %+v; want invited, not waiting", u)
	}
	if u := byName["yvonne@example.com"]; u.Invited || !u.Waiting {
		t.Errorf("yvonne (signed in, no team) = %+v; want waiting", u)
	}
	if u := byName["zed@example.com"]; u.Role != store.RoleAdmin || u.Waiting {
		t.Errorf("zed (an admin) = %+v; want admin, not waiting", u)
	}
	if u := byName["root"]; u.Kind != store.AccountPassword || u.Email != "" || u.Role != store.RoleAdmin || u.LastSignInAt == nil || u.Waiting || u.Invited {
		t.Errorf("root = %+v", u)
	}
}

// A removed account's sessions end with it: its next request is 401, and an
// SSO account that signs in again is made again, on no team.
func TestDeleteAccountEndsSessions(t *testing.T) {
	e := newEnv(t, nil)
	op := e.signInOperator()
	alice := e.userOnMain(false, "acme/widgets")
	id := e.accountOf(alice)

	wantCode(t, e.send(op, "DELETE", "/api/users/"+strconv.FormatInt(id, 10), ""), "DELETE alice", http.StatusNoContent)
	if code, _ := e.me(alice); code != http.StatusUnauthorized {
		t.Errorf("alice's next request = %d; want 401", code)
	}
	wantCode(t, e.send(op, "DELETE", "/api/users/"+strconv.FormatInt(id, 10), ""), "DELETE alice again", http.StatusNotFound)
	wantCode(t, e.send(op, "DELETE", "/api/users/nope", ""), "DELETE nope", http.StatusNotFound)

	again := e.aliceSession() // signed in again: a new account, on no team
	if e.accountOf(again) == id {
		t.Error("the removed account came back")
	}
	if code, me := e.me(again); code != http.StatusOK || !me.Waiting {
		t.Errorf("alice signed in again: %d %+v; want waiting", code, me)
	}

	// A local admin too, by another one.
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
	if !slices.Equal(deleted, []string{"root (password) second", "root (password) " + aliceEmail}) {
		t.Errorf("account_deleted entries = %v", deleted)
	}
}

// Every admin has every power: an SSO admin removes a local admin, and
// another account, at once. An account that is no admin removes nobody.
func TestSSOAdminMayDeleteLocalAdmin(t *testing.T) {
	e := newEnv(t, nil)
	root := e.addPasswordAccount("root", operatorPassword, false)
	e.addPasswordAccount("second", operatorPassword, false)
	c := e.userOnMain(true)
	bob := e.sso("bob@example.com")

	wantCode(t, e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(root.ID, 10), ""), "DELETE root", http.StatusNoContent)
	if _, found, _ := e.st.Account(ctx, root.ID); found {
		t.Error("root is still there")
	}
	wantCode(t, e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(bob, 10), ""), "DELETE bob", http.StatusNoContent)
	if _, found, _ := e.st.Account(ctx, bob); found {
		t.Error("bob is still there")
	}
}

// An old platform account is not on the Users page, so deleting it is as
// good as deleting nothing: 404, and the row stays.
func TestPlatformAccountCannotBeDeleted(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	old := e.platformAccount("main", 20, "bob")
	wantCode(t, e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(old, 10), ""), "DELETE a platform account", http.StatusNotFound)
	wantCode(t, e.send(c, "PUT", "/api/admin/users/"+strconv.FormatInt(old, 10)+"/role", `{"role":"admin"}`),
		"PUT a platform account's role", http.StatusNotFound)
	if _, found, _ := e.st.Account(ctx, old); !found {
		t.Error("the platform account was removed")
	}
}

// An admin can't remove their own account, an SSO admin or a local one.
func TestAdminCantDeleteSelf(t *testing.T) {
	e := newEnv(t, nil)
	local := e.signInOperator()
	e.addPasswordAccount("second", operatorPassword, false)
	sso := e.userOnMain(true)
	for name, c := range map[string]*http.Cookie{"local admin": local, "sso admin": sso} {
		rec := e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(e.accountOf(c), 10), "")
		wantCode(t, rec, name+" DELETE self", http.StatusConflict)
		if got := body(rec); got != `{"error":"you can't remove your own account"}` {
			t.Errorf("%s body = %s", name, got)
		}
		if code, _ := e.me(c); code != http.StatusOK {
			t.Errorf("the %s's session = %d after the refused removal", name, code)
		}
	}
}

// The last local admin can't be removed, not even by an SSO admin: 409.
func TestLastLocalAdminCantBeRemoved(t *testing.T) {
	e := newEnv(t, nil)
	root := e.addPasswordAccount("root", operatorPassword, false)
	c := e.userOnMain(true)
	rec := e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(root.ID, 10), "")
	wantCode(t, rec, "DELETE the last local admin", http.StatusConflict)
	if got := body(rec); got != `{"error":"the last local admin can't be removed"}` {
		t.Errorf("body = %s", got)
	}
	if _, found, _ := e.st.Account(ctx, root.ID); !found {
		t.Error("the last local admin was removed")
	}
	// With a second one, the first can go.
	second := e.addPasswordAccount("second", operatorPassword, false)
	wantCode(t, e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(root.ID, 10), ""), "DELETE root with a second", http.StatusNoContent)
	wantCode(t, e.send(c, "DELETE", "/api/users/"+strconv.FormatInt(second.ID, 10), ""), "DELETE the new last one", http.StatusConflict)
}

// An admin makes an SSO account an admin or a user, and takes it away again.
// They can't change their own role, so one admin always stays to change the
// others'; a local admin is always an admin.
func TestAdminCantRemoveOwnRole(t *testing.T) {
	e := newEnv(t, nil)
	root := e.addPasswordAccount("root", operatorPassword, false)
	c := e.userOnMain(true)
	self := strconv.FormatInt(e.accountOf(c), 10)
	rec := e.send(c, "PUT", "/api/admin/users/"+self+"/role", `{"role":"user"}`)
	wantCode(t, rec, "PUT own role", http.StatusConflict)
	if got := body(rec); got != `{"error":"you can't change your own role"}` {
		t.Errorf("body = %s", got)
	}
	if _, me := e.me(c); !me.Admin {
		t.Error("the admin lost their role")
	}
	wantCode(t, e.send(c, "PUT", "/api/admin/users/"+strconv.FormatInt(root.ID, 10)+"/role", `{"role":"user"}`),
		"PUT a local admin's role", http.StatusConflict)

	bob := e.sso("bob@example.com")
	bobSession := e.sessionFor(bob)
	target := "/api/admin/users/" + strconv.FormatInt(bob, 10) + "/role"
	e.wantStatus(bobSession, "/api/users", http.StatusNotFound)
	wantCode(t, e.send(c, "PUT", target, `{"role":"admin"}`), "make bob an admin", http.StatusNoContent)
	e.wantStatus(bobSession, "/api/users", http.StatusOK)
	wantCode(t, e.send(c, "PUT", target, `{"role":"user"}`), "make bob a user again", http.StatusNoContent)
	e.wantStatus(bobSession, "/api/users", http.StatusNotFound)

	for _, b := range []string{`{"role":"owner"}`, `{"role":"viewer"}`, `{}`, `{"role":"admin","x":1}`, `nope`} {
		wantCode(t, e.send(c, "PUT", target, b), "PUT "+b, http.StatusBadRequest)
	}
	wantCode(t, e.send(c, "PUT", "/api/admin/users/99999/role", `{"role":"admin"}`), "PUT an unknown account", http.StatusNotFound)
	var changes []string
	for _, a := range e.auditEntries() {
		if a.Action == "account_role_changed" {
			changes = append([]string{a.Actor + " " + a.Detail}, changes...)
		}
	}
	if want := []string{aliceEmail + " bob@example.com: user->admin", aliceEmail + " bob@example.com: admin->user"}; !slices.Equal(changes, want) {
		t.Errorf("account_role_changed entries = %q; want %q", changes, want)
	}
}

// An admin adds a local admin and gets its generated password, which signs in
// and must be changed; a bad or taken username is refused. An SSO admin may
// too.
func TestCreatePasswordAccount(t *testing.T) {
	e := newEnv(t, nil)
	op := e.signInOperator()

	got := decode[NewAccountView](t, e.send(op, "POST", "/api/admin/accounts", `{"username":"ops.Second-1"}`),
		"POST account", http.StatusCreated)
	if got.Username != "ops.Second-1" || len(got.Password) < 24 {
		t.Fatalf("created = %+v", got)
	}
	c := e.signInPassword("ops.Second-1", got.Password)
	if code, me := e.me(c); code != http.StatusOK || !me.MustChangePassword || !me.Admin {
		t.Errorf("the new admin: %d %+v; want an admin who must change the password", code, me)
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

	admin := e.userOnMain(true)
	wantCode(t, e.send(admin, "POST", "/api/admin/accounts", `{"username":"by-sso-admin"}`), "POST by an SSO admin", http.StatusCreated)
	var created []string
	for _, a := range e.auditEntries() {
		if a.Action == "account_created" {
			created = append(created, a.Actor+" "+a.Detail)
		}
	}
	if len(created) != 3 || created[2] != "root (password) ops.Second-1" || created[0] != aliceEmail+" by-sso-admin" {
		t.Errorf("account_created entries = %v", created)
	}
}

// Teams are created, listed, changed and removed, each change audited.
func TestTeamCRUD(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	bob := e.sso("bob@example.com")
	zed := e.sso("zed@example.com")

	created := decode[TeamView](t, e.send(c, "POST", "/api/teams", `{"name":"devs"}`), "POST team", http.StatusCreated)
	if created.ID == 0 || created.Name != "devs" || created.Members == nil || created.Repos == nil {
		t.Fatalf("created = %+v", created)
	}
	wantCode(t, e.send(c, "POST", "/api/teams", `{"name":"DEVS"}`), "POST a taken name", http.StatusConflict)
	for _, b := range []string{`{"name":""}`, `{}`, `{"name":"x","role":"viewer"}`, `{"name":"x","y":1}`, `nope`} {
		wantCode(t, e.send(c, "POST", "/api/teams", b), "POST "+b, http.StatusBadRequest)
	}

	target := "/api/teams/" + strconv.FormatInt(created.ID, 10)
	put := fmt.Sprintf(`{"name":"developers","members":[{"accountId":%d,"role":"maintainer"},{"accountId":%d,"role":"viewer"},{"accountId":%d,"role":"viewer"}],"repos":[
		{"instance":"main","project":"ACME/widgets"},{"instance":"gh","project":""}]}`, bob, zed, bob)
	updated := decode[TeamView](t, e.send(c, "PUT", target, put), "PUT team", http.StatusOK)
	if updated.Name != "developers" || len(updated.Members) != 2 {
		t.Fatalf("updated = %+v", updated)
	}
	// An account named twice is one member, with the higher role.
	if m := updated.Members[0]; m != (TeamMemberView{AccountID: bob, Username: "bob@example.com", Email: "bob@example.com", Role: "maintainer"}) {
		t.Errorf("member = %+v", m)
	}
	if m := updated.Members[1]; m != (TeamMemberView{AccountID: zed, Username: "zed@example.com", Email: "zed@example.com", Role: "viewer"}) {
		t.Errorf("member = %+v", m)
	}
	if !slices.Equal(updated.Repos, []TeamRepoView{{Instance: "gh"}, {Instance: "main", Project: "acme/widgets"}}) {
		t.Errorf("repos = %+v", updated.Repos)
	}
	stored := e.storedTeam(created.ID)
	if !slices.Equal(stored.Members, []store.TeamMember{{AccountID: bob, Role: store.RoleMaintainer}, {AccountID: zed, Role: store.RoleViewer}}) || len(stored.Repos) != 2 {
		t.Errorf("stored = %+v", stored)
	}

	// A repository that isn't switched on, or an unknown instance, is 400.
	for _, repos := range []string{`[{"instance":"main","project":"acme/off"}]`, `[{"instance":"nowhere","project":""}]`,
		`[{"instance":"nowhere","project":"acme/widgets"}]`} {
		b := `{"name":"developers","members":[],"repos":` + repos + `}`
		wantCode(t, e.send(c, "PUT", target, b), "PUT "+repos, http.StatusBadRequest)
	}
	wantCode(t, e.send(c, "PUT", "/api/teams/99999", `{"name":"x","members":[],"repos":[]}`), "PUT unknown", http.StatusNotFound)
	other := e.team("others", nil)
	wantCode(t, e.send(c, "PUT", "/api/teams/"+strconv.FormatInt(other, 10), `{"name":"Developers","members":[],"repos":[]}`),
		"PUT a taken name", http.StatusConflict)
	wantCode(t, e.send(c, "PUT", target, `{"name":"developers","role":"admin","members":[],"repos":[]}`), "PUT a team role", http.StatusBadRequest)

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
	want := []string{"root (password) team_created devs",
		"root (password) team_updated developers: name, members +bob@example.com:maintainer +zed@example.com:viewer, repos",
		"root (password) team_deleted developers"}
	if !slices.Equal(log, want) {
		t.Errorf("team audit = %q; want %q", log, want)
	}
}

// Only SSO accounts can be members, each with a role of viewer or
// maintainer: a local admin, an old platform account, an unknown ID, a bad or
// missing role is 400, and the team stays as it was.
func TestTeamMembersMustBeSSO(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	bob := e.sso("bob@example.com")
	old := e.platformAccount("main", 20, "old")
	id := e.team("devs", []store.TeamMember{{AccountID: bob, Role: store.RoleViewer}})
	target := "/api/teams/" + strconv.FormatInt(id, 10)
	for _, members := range []string{
		fmt.Sprintf(`{"accountId":%d,"role":"viewer"}`, e.accountOf(c)),
		fmt.Sprintf(`{"accountId":%d,"role":"viewer"}`, old),
		`{"accountId":99999,"role":"viewer"}`,
		fmt.Sprintf(`{"accountId":%d,"role":"admin"}`, bob),
		fmt.Sprintf(`{"accountId":%d,"role":""}`, bob),
		fmt.Sprintf(`{"accountId":%d}`, bob),
		fmt.Sprint(bob), `"bob"`,
	} {
		b := `{"name":"devs","members":[` + members + `],"repos":[]}`
		wantCode(t, e.send(c, "PUT", target, b), "PUT members "+members, http.StatusBadRequest)
	}
	if tm := e.storedTeam(id); !slices.Equal(tm.Members, []store.TeamMember{{AccountID: bob, Role: store.RoleViewer}}) {
		t.Errorf("members = %v", tm.Members)
	}
}

// A member's role on a team is theirs alone: changing it changes what that
// person may do, and the audit says whose role changed.
func TestTeamMemberRoleChange(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	bob := e.sso("bob@example.com")
	zed := e.sso("zed@example.com")
	id := e.team("devs", []store.TeamMember{{AccountID: bob, Role: store.RoleViewer}, {AccountID: zed, Role: store.RoleViewer}},
		store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	target := "/api/teams/" + strconv.FormatInt(id, 10)
	put := func(members string) {
		wantCode(t, e.send(c, "PUT", target, `{"name":"devs","members":`+members+`,"repos":[{"instance":"main","project":"acme/widgets"}]}`),
			"PUT "+members, http.StatusOK)
	}
	bobSession, zedSession := e.sessionFor(bob), e.sessionFor(zed)
	put(fmt.Sprintf(`[{"accountId":%d,"role":"maintainer"},{"accountId":%d,"role":"viewer"}]`, bob, zed))
	if _, se := e.session(bobSession); se.Role("main", "acme/widgets") != RoleMaintainer {
		t.Errorf("bob's role = %v, want maintainer", se.Role("main", "acme/widgets"))
	}
	if _, se := e.session(zedSession); se.Role("main", "acme/widgets") != RoleViewer {
		t.Errorf("zed's role = %v, want viewer", se.Role("main", "acme/widgets"))
	}
	put(fmt.Sprintf(`[{"accountId":%d,"role":"maintainer"}]`, zed))
	if got := e.auditEntries()[0]; got.Action != "team_updated" || got.Detail != "devs: members zed@example.com:viewer->maintainer -bob@example.com" {
		t.Errorf("audit = %+v; want the role change and the removal named", got)
	}
	wantWaiting(t, e.get(bobSession, "/api/repositories"), "bob after leaving the team")
}

// A member removed between the check and the save is a 400 like any other
// non-account, not a 500, and the team stays as it was. (A trigger removes
// the account when the team row is updated, which is just before the members
// are stored.)
func TestTeamMemberRemovedMeanwhile(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	bob := e.sso("bob@example.com")
	zed := e.sso("zed@example.com")
	id := e.team("devs", []store.TeamMember{{AccountID: bob, Role: store.RoleViewer}})
	e.exec(fmt.Sprintf(`CREATE FUNCTION drop_zed() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN DELETE FROM accounts WHERE id = %d; RETURN NEW; END $$`, zed))
	e.exec(`CREATE TRIGGER drop_zed BEFORE UPDATE ON teams FOR EACH ROW EXECUTE FUNCTION drop_zed()`)

	rec := e.send(c, "PUT", "/api/teams/"+strconv.FormatInt(id, 10),
		fmt.Sprintf(`{"name":"devs","members":[{"accountId":%d,"role":"viewer"},{"accountId":%d,"role":"viewer"}],"repos":[]}`, bob, zed))
	if rec.Code != http.StatusBadRequest || !strings.Contains(body(rec), "members must be accounts") {
		t.Errorf("PUT = %d %s; want 400 about the members", rec.Code, rec.Body)
	}
	if tm := e.storedTeam(id); !slices.Equal(memberIDsOf(tm.Members), []int64{bob}) {
		t.Errorf("members = %v; want bob only", tm.Members)
	}
}

// Every write here needs the session's CSRF token: 403 with code "csrf".
func TestOrgWritesNeedCSRF(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	rec := e.doJSON("POST", "/api/teams", []*http.Cookie{c}, nil, map[string]string{"name": "devs"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /api/teams without X-CSRF-Token = %d %s; want 403", rec.Code, rec.Body)
	}
	wantCSRFCode(t, rec)
	if teams, _ := e.st.Teams(ctx); len(teams) != 0 {
		t.Errorf("a team was made: %+v", teams)
	}
}

// Admins see the whole audit log, whatever an entry is about: a local admin's
// sign-ins and passwords too. Pages are 100, newest first, with next the
// before of the page after (0 at the end).
func TestAuditPages(t *testing.T) {
	e := newEnv(t, nil)
	e.addPasswordAccount("root", operatorPassword, false)
	c := e.userOnMain(true)
	e.addAudit(store.AuditEntry{Actor: "carol", Action: "repository_on", Instance: "main", Project: "acme/widgets"})
	e.exec(`INSERT INTO audit_log (at, actor, action, instance, project)
		SELECT now(), 'carol', 'settings_reset', 'main', 'acme/payments' FROM generate_series(1, 150)`)
	e.addAudit(store.AuditEntry{Actor: "carol", Action: "repository_off", Instance: "gh", Project: "acme/widgets"})
	e.addAudit(store.AuditEntry{Actor: "root (password)", Action: "sign_in_failed", Detail: "wrong password"})
	e.addAudit(store.AuditEntry{Actor: "carol", Action: "team_created", Detail: "devs"})

	first := decode[AuditPageView](t, e.get(c, "/api/audit"), "GET audit", http.StatusOK)
	if len(first.Entries) != 100 || first.Entries[0].Action != "team_created" || first.Entries[0].ID <= first.Entries[99].ID ||
		first.Next != first.Entries[99].ID {
		t.Fatalf("first page: %d entries, first %+v, next %d", len(first.Entries), first.Entries[0], first.Next)
	}
	var seen []AuditView
	for p, pages := first, 1; ; pages++ {
		seen = append(seen, p.Entries...)
		if p.Next == 0 {
			break
		}
		if pages > 10 {
			t.Fatalf("still paging after %d pages", pages)
		}
		p = decode[AuditPageView](t, e.get(c, "/api/audit?before="+strconv.FormatInt(p.Next, 10)), "GET older", http.StatusOK)
	}
	all := e.auditEntries()
	if len(seen) != len(all) {
		t.Errorf("the pages hold %d entries; the log has %d", len(seen), len(all))
	}
	for _, want := range []string{"sign_in_failed", "repository_off", "repository_on"} {
		if !slices.ContainsFunc(seen, func(a AuditView) bool { return a.Action == want }) {
			t.Errorf("an admin does not see %s", want)
		}
	}
	for _, q := range []string{"?before=x", "?before=-1", "?before=0"} {
		wantCode(t, e.get(c, "/api/audit"+q), "GET audit"+q, http.StatusBadRequest)
	}
	empty := e.get(c, "/api/audit?before=1")
	if empty.Code != http.StatusOK || body(empty) != `{"entries":[],"next":0}` {
		t.Errorf("GET before the first = %d %s; want no entries, next 0", empty.Code, empty.Body)
	}
}

// team_updated names the members added and removed and a role changed, by
// username (the email of an SSO account).
func TestTeamUpdatedDetailNamesChanges(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	bob := e.sso("bob@example.com")
	zed := e.sso("zed@example.com")
	id := e.team("devs", []store.TeamMember{{AccountID: bob, Role: store.RoleViewer}})
	target := "/api/teams/" + strconv.FormatInt(id, 10)
	wantCode(t, e.send(c, "PUT", target, fmt.Sprintf(`{"name":"devs","members":[{"accountId":%d,"role":"maintainer"}],"repos":[]}`, zed)),
		"PUT", http.StatusOK)
	if got := e.auditEntries()[0]; got.Action != "team_updated" || got.Detail != "devs: members +zed@example.com:maintainer -bob@example.com" {
		t.Errorf("audit = %+v; want the members changed named", got)
	}
	// A change that leaves everything as it is leaves no entry.
	n := len(e.auditEntries())
	wantCode(t, e.send(c, "PUT", target, fmt.Sprintf(`{"name":"devs","members":[{"accountId":%d,"role":"maintainer"}],"repos":[]}`, zed)),
		"PUT again", http.StatusOK)
	if got := len(e.auditEntries()); got != n {
		t.Errorf("an unchanged team left %d new audit entries", got-n)
	}
}

// No audit entry holds a generated password.
func TestAuditNoSecrets(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	bob := e.sso("bob@example.com")
	created := decode[NewAccountView](t, e.send(c, "POST", "/api/admin/accounts", `{"username":"second"}`), "POST account", http.StatusCreated)
	team := decode[TeamView](t, e.send(c, "POST", "/api/teams", `{"name":"devs"}`), "POST team", http.StatusCreated)
	target := "/api/teams/" + strconv.FormatInt(team.ID, 10)
	wantCode(t, e.send(c, "PUT", target, fmt.Sprintf(`{"name":"devs","members":[{"accountId":%d,"role":"maintainer"}],"repos":[{"instance":"main","project":""}]}`, bob)),
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
// leave one entry naming the actor and the repository. An SSO account is named
// by its full email.
func TestWritesAudited(t *testing.T) {
	e := newEnv(t, nil)
	e.cat["main"].set("acme/gadgets")
	c := e.userOnMain(true)
	id := e.accountOf(c)
	base := "/api/repositories/main/acme/gadgets"

	e.skill("style", "x")

	wantCode(t, e.send(c, "POST", "/api/repositories", `{"instance":"main","project":"acme/gadgets"}`), "POST", http.StatusCreated)
	wantCode(t, e.send(c, "PUT", base+"/settings", `{"skills":["style"],"approve":false}`), "PUT settings", http.StatusOK)
	wantCode(t, e.send(c, "PUT", base+"/settings", `{"approve":"no"}`), "PUT bad settings", http.StatusBadRequest)
	wantCode(t, e.send(c, "POST", base+"/settings/reset", ""), "POST reset", http.StatusOK)
	wantCode(t, e.send(c, "DELETE", base, ""), "DELETE", http.StatusOK)

	var got []string
	for _, a := range e.auditEntries() {
		if a.ActorID == nil || *a.ActorID != id {
			t.Errorf("%s actor ID = %v; want %d", a.Action, a.ActorID, id)
		}
		got = append([]string{a.Actor + " " + a.Action + " " + a.Instance + "/" + a.Project + " " + a.Detail}, got...)
	}
	want := []string{
		aliceEmail + " repository_on main/acme/gadgets ",
		aliceEmail + " settings_updated main/acme/gadgets approve, skills",
		aliceEmail + " settings_reset main/acme/gadgets ",
		aliceEmail + " repository_off main/acme/gadgets ",
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit = %q; want %q", got, want)
	}
}

// A viewer (no admin) gets 404 on every route here, and nothing changes.
func TestDeveloperGets404OnOrgRoutes(t *testing.T) {
	e := newEnv(t, nil)
	root := e.addPasswordAccount("root", operatorPassword, false)
	c := e.userOnMain(false, "acme/widgets")
	bob := e.sso("bob@example.com")
	team := e.team("devs", nil)
	tid := strconv.FormatInt(team, 10)
	for _, r := range []struct{ method, target, body string }{
		{"GET", "/api/users", ""},
		{"DELETE", "/api/users/" + strconv.FormatInt(bob, 10), ""},
		{"DELETE", "/api/users/" + strconv.FormatInt(root.ID, 10), ""},
		{"PUT", "/api/admin/users/" + strconv.FormatInt(bob, 10) + "/role", `{"role":"admin"}`},
		{"POST", "/api/admin/accounts", `{"username":"sneaky"}`},
		{"GET", "/api/teams", ""},
		{"POST", "/api/teams", `{"name":"mine"}`},
		{"PUT", "/api/teams/" + tid, `{"name":"devs","members":[],"repos":[]}`},
		{"DELETE", "/api/teams/" + tid, ""},
		{"GET", "/api/audit", ""},
	} {
		wantCode(t, e.send(c, r.method, r.target, r.body), r.method+" "+r.target, http.StatusNotFound)
	}
	if got, found, _ := e.st.Account(ctx, bob); !found || got.Role != store.RoleUser {
		t.Errorf("bob = %+v, %v", got, found)
	}
	if teams, _ := e.st.Teams(ctx); len(teams) != 2 {
		t.Errorf("%d teams; want devs and alice's", len(teams))
	}
	if _, found, _ := e.st.PasswordAccount(ctx, "sneaky"); found {
		t.Error("a viewer made a password account")
	}
}

// An admin adds a person by email before they have signed in: the account
// shows as invited, on the teams named with their roles (viewer when none). A
// second add of the same email is 409, a bad email 400, and a team that isn't
// there makes nothing. An account that signed in without being added is marked
// added (200), and keeps its role and teams.
func TestAddUserByEmail(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	devs := e.team("devs", nil, store.TeamRepo{Instance: "main"})
	ops := e.team("ops", nil)

	got := decode[UserView](t, e.send(c, "POST", "/api/admin/users", `{"email":"Carol@Other.NET"}`), "POST a user", http.StatusCreated)
	if got.Username != "carol@other.net" || got.Email != "carol@other.net" || got.Kind != store.AccountSSO || got.Role != store.RoleUser ||
		!got.Invited || got.Waiting || got.LastSignInAt != nil || got.Teams == nil || len(got.Teams) != 0 {
		t.Fatalf("added = %+v; want an invited user on no team (not waiting: she has not signed in)", got)
	}
	acc, found, err := e.st.SSOAccountByEmail(ctx, "carol@other.net")
	if err != nil || !found || !acc.Added || acc.LastSignInAt != nil {
		t.Fatalf("stored account = %+v, %v, %v; want added and never signed in", acc, found, err)
	}
	users := decode[[]UserView](t, e.get(c, "/api/users"), "GET /api/users", http.StatusOK)
	if i := slices.IndexFunc(users, func(u UserView) bool { return u.ID == got.ID }); i < 0 || !users[i].Invited {
		t.Errorf("the Users page lists %+v; want carol invited", users)
	}

	wantCode(t, e.send(c, "POST", "/api/admin/users", `{"email":"carol@other.net"}`), "POST the same email", http.StatusConflict)
	wantCode(t, e.send(c, "POST", "/api/admin/users", `{"email":"CAROL@OTHER.NET"}`), "POST the same email in capitals", http.StatusConflict)

	withTeams := decode[UserView](t, e.send(c, "POST", "/api/admin/users",
		fmt.Sprintf(`{"email":"dave@other.net","teams":[{"teamId":%d},{"teamId":%d,"role":"maintainer"}]}`, devs, ops)),
		"POST a user with teams", http.StatusCreated)
	if !withTeams.Invited || len(withTeams.Teams) != 2 {
		t.Fatalf("added with teams = %+v", withTeams)
	}
	if m := e.storedTeam(devs).Members; len(m) != 1 || m[0].AccountID != withTeams.ID || m[0].Role != store.RoleViewer {
		t.Errorf("devs members = %+v; want dave as a viewer", m)
	}
	if m := e.storedTeam(ops).Members; len(m) != 1 || m[0].AccountID != withTeams.ID || m[0].Role != store.RoleMaintainer {
		t.Errorf("ops members = %+v; want dave as a maintainer", m)
	}

	before := len(e.ssoAccounts())
	for name, b := range map[string]string{
		"no email":            `{}`,
		"empty":               `{"email":""}`,
		"no at":               `{"email":"carol"}`,
		"two addresses":       `{"email":"a@b.net,c@d.net"}`,
		"a display name":      `{"email":"Carol <carol@other.net>"}`,
		"surrounding space":   `{"email":" erin@other.net "}`,
		"an unknown field":    `{"email":"erin@other.net","role":"admin"}`,
		"not JSON":            `nope`,
		"a team not there":    `{"email":"erin@other.net","teams":[{"teamId":99999}]}`,
		"a bad role":          fmt.Sprintf(`{"email":"erin@other.net","teams":[{"teamId":%d,"role":"admin"}]}`, devs),
		"a team named twice":  fmt.Sprintf(`{"email":"erin@other.net","teams":[{"teamId":%d},{"teamId":%d}]}`, devs, devs),
		"an empty team list":  `{"email":"erin@other.net","teams":"x"}`,
		"the Kelvin sign too": `{"email":"erin@K.net x"}`,
	} {
		wantCode(t, e.send(c, "POST", "/api/admin/users", b), "POST "+name, http.StatusBadRequest)
	}
	if after := len(e.ssoAccounts()); after != before {
		t.Errorf("%d SSO accounts after the refusals, want the %d before them", after, before)
	}

	// Someone who signed in on an allowed domain, with a team and a role of
	// their own, is marked added and keeps both.
	frank := e.sso("frank@example.com")
	e.editTeam(devs, func(t *store.Team) {
		t.Members = append(t.Members, store.TeamMember{AccountID: frank, Role: store.RoleMaintainer})
	})
	if err := e.st.SetAccountRole(ctx, frank, store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	marked := decode[UserView](t, e.send(c, "POST", "/api/admin/users", `{"email":"frank@example.com"}`), "POST an account that signed in", http.StatusOK)
	if marked.ID != frank || marked.Invited || marked.Role != store.RoleAdmin || len(marked.Teams) != 1 {
		t.Errorf("marked = %+v; want the same account, signed in, still an admin, still on its team", marked)
	}
	if a, _, _ := e.st.SSOAccountByEmail(ctx, "frank@example.com"); !a.Added {
		t.Error("frank was not marked added")
	}
	wantCode(t, e.send(c, "POST", "/api/admin/users", `{"email":"frank@example.com"}`), "POST frank again", http.StatusConflict)

	// Each add is audited by email; an SSO admin may add too.
	var added []string
	for _, a := range e.auditEntries() {
		if a.Action == "user_added" {
			added = append([]string{a.Actor + " " + a.Detail}, added...)
		}
	}
	if want := []string{"root (password) carol@other.net", "root (password) dave@other.net", "root (password) frank@example.com"}; !slices.Equal(added, want) {
		t.Errorf("user_added entries = %q; want %q", added, want)
	}
	admin := e.userOnMain(true)
	wantCode(t, e.send(admin, "POST", "/api/admin/users", `{"email":"gina@other.net"}`), "POST by an SSO admin", http.StatusCreated)
}

// A person an admin added before they signed in, on a domain the allowlist does
// not list, signs in through OIDC and is kept: the account is the one the
// admin made (Added stays set), and the teams the admin gave it are still its
// own, so the repository shows and the waiting page does not.
func TestAddedUserKeepsAccessOnFirstSignIn(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useOIDC(f, nil)
	c := e.signInOperator()
	team := e.team("devs", nil, store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	added := decode[UserView](t, e.send(c, "POST", "/api/admin/users",
		fmt.Sprintf(`{"email":"Carol@Other.NET","teams":[{"teamId":%d,"role":"maintainer"}]}`, team)), "POST a user", http.StatusCreated)

	f.claims = func(cl map[string]any) { cl["email"] = "carol@other.net" }
	rec := e.oidcSignIn(f, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %q %s; want 303 to /", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	session := cookie(rec, SessionCookieName)
	if session == nil {
		t.Fatal("no session cookie")
	}
	if got := e.accountOf(session); got != added.ID {
		t.Errorf("the session is for account %d, want the one the admin made, %d", got, added.ID)
	}
	if n := len(e.ssoAccounts()); n != 1 {
		t.Errorf("%d SSO accounts, want 1", n)
	}
	acc, _, _ := e.st.SSOAccountByEmail(ctx, "carol@other.net")
	if !acc.Added || acc.LastSignInAt == nil || acc.Role != store.RoleUser {
		t.Errorf("account = %+v; want added, signed in, a user", acc)
	}
	if m := e.storedTeam(team).Members; len(m) != 1 || m[0].AccountID != added.ID || m[0].Role != store.RoleMaintainer {
		t.Errorf("team members = %+v; want carol, a maintainer", m)
	}
	if _, me := e.me(session); me.Waiting || me.Admin {
		t.Errorf("me = %+v; want a user who is not waiting", me)
	}
	repos := getJSON[[]RepositoryView](e, session, "/api/repositories")
	if len(repos) != 1 || repos[0].Project != "acme/widgets" || repos[0].Instance != "main" {
		t.Errorf("repositories = %+v; want only main/acme/widgets", repos)
	}
	if v := getJSON[RepositoryDetailView](e, session, "/api/repositories/main/acme/widgets"); v.Role != store.RoleMaintainer {
		t.Errorf("role = %q, want maintainer", v.Role)
	}
	e.wantStatus(session, "/api/users", http.StatusNotFound)

	// The same person, not added and off the allowlist, is turned away.
	f.claims = func(cl map[string]any) { cl["email"] = "mallory@other.net" }
	if rec := e.oidcSignIn(f, nil); rec.Header().Get("Location") != "/sign-in?error=not_allowed" {
		t.Errorf("an email nobody added = %q; want the sign-in page with not_allowed", rec.Header().Get("Location"))
	}
}

// An admin who signs in with SSO lands in the admin area, anyone else on the
// user pages.
func TestOIDCSignInLandsByRole(t *testing.T) {
	e := newEnv(t, nil)
	f := newFakeIssuer(t)
	e.useOIDC(f, nil)
	e.ssoAdmin("boss@example.com")
	for email, want := range map[string]string{"boss@example.com": "/admin", "worker@example.com": "/"} {
		f.claims = func(cl map[string]any) { cl["email"] = email }
		rec := e.oidcSignIn(f, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("%s lands on %q (%d), want %q", email, rec.Header().Get("Location"), rec.Code, want)
		}
	}
}

// A repository's settings and the Teams page read and write the same rows: a
// team given the repository from one side shows on the other, and the
// repository's own list leaves a team's other repositories and its
// whole-instance cover alone.
func TestRepositoryTeamsMatchTheTeamsPage(t *testing.T) {
	e := newEnv(t, nil)
	c := e.signInOperator()
	payments := e.team("payments", nil, store.TeamRepo{Instance: "main", Project: "acme/payments"})
	whole := e.team("whole main", nil, store.TeamRepo{Instance: "main"})
	other := e.team("gh only", nil, store.TeamRepo{Instance: "gh", Project: "acme/widgets"})
	empty := e.team("empty", nil)

	got := decode[RepoTeamsView](t, e.get(c, "/api/repositories/main/acme/widgets/teams"), "GET teams", http.StatusOK)
	want := []RepoTeamView{
		{ID: empty, Name: "empty"},
		{ID: other, Name: "gh only"},
		{ID: payments, Name: "payments"},
		{ID: whole, Name: "whole main", WholeInstance: true},
	}
	if !slices.Equal(got.Teams, want) {
		t.Fatalf("teams of main/acme/widgets = %+v\nwant %+v", got.Teams, want)
	}

	// Given from the repository, seen on the Teams page.
	body := fmt.Sprintf(`{"teams":[%d,%d]}`, payments, empty)
	got = decode[RepoTeamsView](t, e.send(c, "PUT", "/api/repositories/main/acme/widgets/teams", body), "PUT teams", http.StatusOK)
	for _, tm := range got.Teams {
		if wantDirect := tm.ID == payments || tm.ID == empty; tm.Direct != wantDirect {
			t.Errorf("team %q direct = %v, want %v", tm.Name, tm.Direct, wantDirect)
		}
	}
	teams := decode[[]TeamView](t, e.get(c, "/api/teams"), "GET /api/teams", http.StatusOK)
	covers := func(name, project string) bool {
		for _, tm := range teams {
			if tm.Name == name {
				return slices.Contains(tm.Repos, TeamRepoView{Instance: "main", Project: project})
			}
		}
		t.Fatalf("no team %q", name)
		return false
	}
	if !covers("empty", "acme/widgets") || !covers("payments", "acme/widgets") || !covers("payments", "acme/payments") {
		t.Errorf("Teams page after the PUT: %+v; want empty and payments on widgets, payments still on payments", teams)
	}
	if covers("gh only", "acme/widgets") || !covers("whole main", "") {
		t.Errorf("Teams page after the PUT: %+v; want gh only untouched and whole main still whole", teams)
	}

	// Taken from the Teams page, gone on the repository.
	wantCode(t, e.send(c, "PUT", fmt.Sprintf("/api/teams/%d", empty),
		`{"name":"empty","members":[],"repos":[]}`), "PUT /api/teams/{id}", http.StatusOK)
	got = decode[RepoTeamsView](t, e.get(c, "/api/repositories/main/acme/widgets/teams"), "GET teams", http.StatusOK)
	for _, tm := range got.Teams {
		if tm.ID == empty && tm.Direct {
			t.Error("the Teams page took the repository off empty, but the repository still shows it")
		}
	}

	// Taken from the repository: a team's other repositories stay.
	got = decode[RepoTeamsView](t, e.send(c, "PUT", "/api/repositories/main/ACME/Widgets/teams", `{"teams":[]}`), "PUT no teams", http.StatusOK)
	for _, tm := range got.Teams {
		if tm.Direct {
			t.Errorf("team %q still lists the repository after an empty list", tm.Name)
		}
	}
	if r := e.storedTeam(payments).Repos; len(r) != 1 || r[0] != (store.TeamRepo{Instance: "main", Project: "acme/payments"}) {
		t.Errorf("payments repos = %+v; want only acme/payments", r)
	}
	if r := e.storedTeam(whole).Repos; len(r) != 1 || r[0].Project != "" {
		t.Errorf("whole main repos = %+v; want the whole instance", r)
	}

	// Refusals: a team that isn't there, a bad body, an unknown repository.
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/widgets/teams", `{"teams":[99999]}`), "PUT an unknown team", http.StatusBadRequest)
	for _, b := range []string{`{}`, `{"teams":null}`, `{"teams":["x"]}`, `{"teams":[],"x":1}`, `nope`} {
		wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/widgets/teams", b), "PUT "+b, http.StatusBadRequest)
	}
	wantCode(t, e.send(c, "PUT", "/api/repositories/main/acme/nothing/teams", `{"teams":[]}`), "PUT an unknown repository", http.StatusNotFound)
	e.wantStatus(c, "/api/repositories/main/acme/nothing/teams", http.StatusNotFound)

	// A repository the config sets up can be given to teams too.
	wantCode(t, e.send(c, "PUT", "/api/repositories/gh/acme/widgets/teams", fmt.Sprintf(`{"teams":[%d]}`, empty)), "PUT a config repository", http.StatusOK)

	var changed []string
	for _, a := range e.auditEntries() {
		if a.Action == "team_updated" && a.Project != "" {
			changed = append(changed, a.Detail)
		}
	}
	if len(changed) == 0 {
		t.Error("giving a repository to a team wrote no audit entry")
	}
}

// Only an admin reads or writes a repository's teams: a viewer and a
// maintainer of the repository get 404, as for any admin route, and nothing
// changes.
func TestRepositoryTeamsAreForAdmins(t *testing.T) {
	e := newEnv(t, nil)
	viewer := e.sessionFor(e.sso("viewer@example.com"))
	maintainer := e.sessionFor(e.sso("maintainer@example.com"))
	devs := e.team("devs", []store.TeamMember{
		{AccountID: e.accountOf(viewer), Role: store.RoleViewer},
		{AccountID: e.accountOf(maintainer), Role: store.RoleMaintainer},
	}, store.TeamRepo{Instance: "main", Project: "acme/widgets"})
	extra := e.team("extra", nil)

	unknown := e.get(viewer, "/api/repositories/main/acme/nothing/teams")
	for name, c := range map[string]*http.Cookie{"viewer": viewer, "maintainer": maintainer} {
		get := e.get(c, "/api/repositories/main/acme/widgets/teams")
		put := e.send(c, "PUT", "/api/repositories/main/acme/widgets/teams", fmt.Sprintf(`{"teams":[%d]}`, extra))
		for what, rec := range map[string]*httptest.ResponseRecorder{"GET": get, "PUT": put} {
			if rec.Code != http.StatusNotFound || rec.Body.String() != unknown.Body.String() {
				t.Errorf("%s teams as a %s = %d %s; want 404 %s", what, name, rec.Code, rec.Body, unknown.Body)
			}
		}
	}
	if r := e.storedTeam(extra).Repos; len(r) != 0 {
		t.Errorf("extra repos = %+v; a non-admin gave it a repository", r)
	}
	if r := e.storedTeam(devs).Repos; len(r) != 1 {
		t.Errorf("devs repos = %+v; want unchanged", r)
	}
	// An admin's request shows the section's data.
	admin := e.sessionFor(e.ssoAdmin("boss@example.com"))
	got := decode[RepoTeamsView](t, e.get(admin, "/api/repositories/main/acme/widgets/teams"), "GET teams as an admin", http.StatusOK)
	if len(got.Teams) != 2 {
		t.Errorf("teams = %+v", got.Teams)
	}
}
