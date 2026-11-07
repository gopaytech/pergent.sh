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
	if got := body(rec); got != `{"error":"the last password account can't be removed"}` {
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
