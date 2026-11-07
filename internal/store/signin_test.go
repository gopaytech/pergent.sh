package store

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSignInSettingsRoundTrip(t *testing.T) {
	s := newStore(t)
	got, err := s.SignInSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "" || got.ClientSecretSealed != nil || got.AllowedDomains != nil || !got.UpdatedAt.IsZero() {
		t.Fatalf("settings before any put = %+v; want the zero value", got)
	}

	oidc := SignInSettings{
		Mode: SignInModeOIDC, Label: "Google", Issuer: "https://accounts.google.com", ClientID: "client-id",
		ClientSecretSealed: []byte("sealed-1"), Header: "X-Auth", Audience: "aud", JWKSURL: "https://example.com/jwks",
		EmailClaim: "email", AllowedDomains: []string{"example.com", "example.org"}, AllowedEmails: []string{"x@y.com"},
		UpdatedAt: t0,
	}
	if err := s.PutSignInSettings(ctx, oidc); err != nil {
		t.Fatal(err)
	}
	got, err = s.SignInSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != oidc.Mode || got.Label != oidc.Label || got.Issuer != oidc.Issuer || got.ClientID != oidc.ClientID ||
		got.Header != oidc.Header || got.Audience != oidc.Audience || got.JWKSURL != oidc.JWKSURL ||
		got.EmailClaim != oidc.EmailClaim || !bytes.Equal(got.ClientSecretSealed, oidc.ClientSecretSealed) ||
		!slices.Equal(got.AllowedDomains, oidc.AllowedDomains) || !slices.Equal(got.AllowedEmails, oidc.AllowedEmails) ||
		!got.UpdatedAt.Equal(t0) {
		t.Fatalf("settings = %+v; want %+v", got, oidc)
	}

	// A nil sealed secret keeps the stored one; the lists are replaced, and
	// an empty list reads back as nil.
	next := SignInSettings{Mode: SignInModeJWT, Issuer: "https://proxy.example.com", Audience: "pergent", Header: "X-Jwt",
		JWKSURL: "https://proxy.example.com/jwks", EmailClaim: "email", UpdatedAt: t0.Add(time.Hour)}
	if err := s.PutSignInSettings(ctx, next); err != nil {
		t.Fatal(err)
	}
	got, _ = s.SignInSettings(ctx)
	if got.Mode != SignInModeJWT || got.Label != "" || got.ClientID != "" || got.Issuer != next.Issuer ||
		!bytes.Equal(got.ClientSecretSealed, []byte("sealed-1")) || got.AllowedDomains != nil || got.AllowedEmails != nil ||
		!got.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("settings after the second put = %+v", got)
	}

	// A new secret replaces it; an empty one removes it.
	next.ClientSecretSealed = []byte("sealed-2")
	if err := s.PutSignInSettings(ctx, next); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.SignInSettings(ctx); !bytes.Equal(got.ClientSecretSealed, []byte("sealed-2")) {
		t.Fatalf("secret = %q; want sealed-2", got.ClientSecretSealed)
	}
	next.ClientSecretSealed = []byte{}
	if err := s.PutSignInSettings(ctx, next); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.SignInSettings(ctx); got.ClientSecretSealed != nil {
		t.Fatalf("secret = %q; want removed", got.ClientSecretSealed)
	}

	// Mode "" is neither; an unknown mode is refused. One row, always.
	if err := s.PutSignInSettings(ctx, SignInSettings{Mode: "saml"}); err == nil {
		t.Error("PutSignInSettings with mode saml = nil error")
	}
	if err := s.PutSignInSettings(ctx, SignInSettings{}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sign_in_settings`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v; want 1", n, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sign_in_settings (id, updated_at) VALUES (2, now())`); err == nil {
		t.Error("a second sign_in_settings row was accepted")
	}
	if got, _ = s.SignInSettings(ctx); got.Mode != "" {
		t.Fatalf("mode = %q; want empty", got.Mode)
	}
}

// A session records how it began; DeleteSSOSessions ends the OIDC and JWT
// ones and keeps the password ones.
func TestDeleteSSOSessionsKeepsPassword(t *testing.T) {
	s := newStore(t)
	admin := mustPassword(t, s, "admin")
	user := mustSSO(t, s, "alice@example.com")
	mk := func(b byte, account int64, signIn, issuer string) []byte {
		t.Helper()
		se := Session{IDHash: bytes.Repeat([]byte{b}, 32), AccountID: account, Username: "u", CSRF: "csrf",
			SignIn: signIn, Issuer: issuer, LastSeenAt: t0, CreatedAt: t0}
		if err := s.CreateSession(ctx, se); err != nil {
			t.Fatal(err)
		}
		return se.IDHash
	}
	pw := mk(1, admin.ID, SignInPassword, "")
	def := mk(2, admin.ID, "", "") // no value is a password sign-in
	oidc := mk(3, user.ID, SignInOIDC, "https://accounts.google.com")
	jwt := mk(4, user.ID, SignInJWT, "https://proxy.example.com")

	got, found, err := s.Session(ctx, oidc)
	if err != nil || !found || got.SignIn != SignInOIDC || got.Issuer != "https://accounts.google.com" || got.Platform != "" {
		t.Fatalf("oidc session = %+v, %v, %v", got, found, err)
	}
	if got, _, _ := s.Session(ctx, def); got.SignIn != SignInPassword || got.Issuer != "" {
		t.Fatalf("default session = %+v; want a password sign-in", got)
	}
	if got, _, _ := s.Session(ctx, jwt); got.SignIn != SignInJWT {
		t.Fatalf("jwt session = %+v", got)
	}
	bad := Session{IDHash: bytes.Repeat([]byte{9}, 32), AccountID: user.ID, CSRF: "c", SignIn: "saml", LastSeenAt: t0, CreatedAt: t0}
	if err := s.CreateSession(ctx, bad); err == nil {
		t.Error("CreateSession with sign-in saml = nil error")
	}

	if err := s.DeleteSSOSessions(ctx); err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string][]byte{"oidc": oidc, "jwt": jwt} {
		if _, found, _ := s.Session(ctx, h); found {
			t.Errorf("the %s session is still there", name)
		}
	}
	for name, h := range map[string][]byte{"password": pw, "default": def} {
		if _, found, _ := s.Session(ctx, h); !found {
			t.Errorf("the %s session is gone", name)
		}
	}
	if err := s.DeleteSSOSessions(ctx); err != nil {
		t.Fatalf("DeleteSSOSessions with none to delete: %v", err)
	}
}

// What the previous release (0027) reads and writes still works on the
// migrated database, with SSO rows present: Review Focus 4.
func TestMigrationKeepsPreviousRelease(t *testing.T) {
	s := newStoreAt(t, 27)
	// The previous release's data: a password admin, platform accounts, a
	// team with members and a session.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO accounts (kind, username, password_hash, created_at) VALUES ('password', 'admin', 'hash', $1)`, t0)
	exec(`INSERT INTO accounts (kind, username, platform, instance, platform_user_id, created_at, last_sign_in_at)
		VALUES ('platform', 'alice', 'gitlab', 'main', 1, $1, $1), ('platform', 'bob', 'github', 'gh', 2, $1, $1)`, t0)
	exec(`INSERT INTO teams (name, role, created_at) VALUES ('devs', 'maintainer', $1)`, t0)
	exec(`INSERT INTO team_members (team_id, account_id) SELECT t.id, a.id FROM teams t, accounts a WHERE a.kind = 'platform'`)
	exec(`INSERT INTO team_repos (team_id, instance, project) SELECT id, 'main', 'acme/widgets' FROM teams`)
	exec(`INSERT INTO sessions (id_hash, account_id, platform, instance, user_id, username, token_sealed, csrf, last_seen_at, created_at)
		SELECT '\x01', id, 'gitlab', 'main', 1, 'alice', '\x02', 'csrf', $1, $1 FROM accounts WHERE username = 'alice'`, t0)

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Every session ends; nothing else is dropped.
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sessions after the migration = %d, %v; want 0", n, err)
	}
	for _, c := range []struct{ table, columns string }{
		{"accounts", "id kind username platform instance platform_user_id password_hash must_change_password failed_attempts locked_until created_at last_sign_in_at"},
		{"teams", "id name role created_at"},
		{"team_members", "team_id account_id"},
		{"team_repos", "team_id instance project"},
		{"sessions", "id_hash platform instance user_id username token_sealed csrf access_checked_at last_seen_at created_at account_id access_scope"},
		{"user_access", "session_hash instance project project_id"},
		{"instances", "name platform url api_url oauth_client_id"},
		{"instance_secrets", "instance field sealed updated_at"},
	} {
		for _, col := range strings.Fields(c.columns) {
			var present bool
			if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2)`, c.table, col).Scan(&present); err != nil {
				t.Fatal(err)
			}
			if !present {
				t.Errorf("%s.%s is gone; the previous release reads it", c.table, col)
			}
		}
	}

	// The platform rows and the team stay in the database, unchanged; a
	// password account is an admin.
	var platforms, members int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM accounts WHERE kind = 'platform'),
		(SELECT count(*) FROM team_members)`).Scan(&platforms, &members); err != nil || platforms != 2 || members != 2 {
		t.Fatalf("platform accounts %d, memberships %d, %v; want 2 and 2", platforms, members, err)
	}
	var role string
	if err := s.pool.QueryRow(ctx, `SELECT role FROM accounts WHERE kind = 'password'`).Scan(&role); err != nil || role != "admin" {
		t.Fatalf("password account role = %q, %v; want admin", role, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT role FROM accounts WHERE username = 'alice'`).Scan(&role); err != nil || role != "user" {
		t.Fatalf("platform account role = %q, %v; want user", role, err)
	}

	// With an SSO row present, the previous release's queries still run.
	mustSSO(t, s, "sso@example.com")
	if _, err := s.AddSSOAccount(ctx, "invited@example.com", t0); err != nil {
		t.Fatal(err)
	}
	// Its Accounts query, as it was (a plain scan into strings and an int,
	// so a NULL platform column would fail it).
	rows, err := s.pool.Query(ctx, `SELECT id, kind, username, COALESCE(platform, ''), COALESCE(instance, ''), COALESCE(platform_user_id, 0),
		COALESCE(password_hash, ''), must_change_password, failed_attempts, locked_until, created_at, last_sign_in_at FROM accounts a
		ORDER BY (kind = 'platform' AND NOT EXISTS (SELECT 1 FROM team_members m WHERE m.account_id = a.id)) DESC,
			lower(username), id`)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for rows.Next() {
		var (
			id, platformUserID  int64
			kind, username      string
			platform, instance  string
			hash                string
			mustChange          bool
			failed              int
			lockedUntil, signIn *time.Time
			createdAt           time.Time
		)
		if err := rows.Scan(&id, &kind, &username, &platform, &instance, &platformUserID, &hash, &mustChange, &failed,
			&lockedUntil, &createdAt, &signIn); err != nil {
			t.Fatalf("the previous release's account query fails to scan with an SSO row present: %v", err)
		}
		kinds[kind]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if kinds["sso"] != 2 || kinds["platform"] != 2 || kinds["password"] != 1 {
		t.Fatalf("rows by kind = %v", kinds)
	}
	// Its team and member queries, and its writes.
	exec(`SELECT id, name, role, created_at FROM teams`)
	exec(`SELECT team_id, account_id FROM team_members`)
	exec(`INSERT INTO accounts (kind, username, password_hash, must_change_password, created_at) VALUES ('password', 'two', 'h', false, $1)`, t0)
	exec(`INSERT INTO accounts (kind, username, platform, instance, platform_user_id, created_at, last_sign_in_at)
		VALUES ('platform', 'alice-renamed', 'gitlab', 'main', 1, $1, $1)
		ON CONFLICT (instance, platform_user_id) WHERE kind = 'platform'
		DO UPDATE SET username = EXCLUDED.username, last_sign_in_at = EXCLUDED.last_sign_in_at`, t0)
	exec(`INSERT INTO sessions (id_hash, account_id, platform, instance, user_id, username, token_sealed, csrf, last_seen_at, created_at)
		SELECT '\x03', id, 'gitlab', 'main', 1, 'alice', '\x02', 'csrf', $1, $1 FROM accounts WHERE kind = 'platform' LIMIT 1`, t0)
	exec(`INSERT INTO teams (name, role, created_at) VALUES ('ops', 'viewer', $1)`, t0)
	exec(`INSERT INTO team_members (team_id, account_id) SELECT (SELECT id FROM teams WHERE name = 'ops'), id FROM accounts WHERE kind = 'platform' LIMIT 1`)

	// The new code reads the same database: platform rows hidden, the team
	// without members, the password accounts admins.
	all, err := s.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.Kind == AccountPlatform {
			t.Errorf("Accounts lists a platform account: %+v", a)
		}
		if a.Kind == AccountPassword && a.Role != RoleAdmin {
			t.Errorf("password account %q has role %q", a.Username, a.Role)
		}
	}
	if len(all) != 4 { // admin, two, sso, invited
		t.Errorf("Accounts = %d rows; want 4", len(all))
	}
	teams, err := s.Teams(ctx)
	if err != nil || len(teams) != 2 {
		t.Fatalf("Teams = %+v, %v", teams, err)
	}
	for _, tm := range teams {
		if len(tm.Members) != 0 {
			t.Errorf("team %s has members %+v; want none after the migration", tm.Name, tm.Members)
		}
	}
	if teams[0].Name != "devs" || len(teams[0].Repos) != 1 {
		t.Errorf("team devs = %+v; want its name and repository kept", teams[0])
	}
	// The old team role stays in its column, for the previous release.
	var teamRole string
	if err := s.pool.QueryRow(ctx, `SELECT role FROM teams WHERE name = 'devs'`).Scan(&teamRole); err != nil || teamRole != RoleMaintainer {
		t.Errorf("teams.role = %q, %v; want the role kept", teamRole, err)
	}
}
