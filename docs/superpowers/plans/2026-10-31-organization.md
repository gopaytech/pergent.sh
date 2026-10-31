# Organization (password admins, teams, secrets in the panel) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- Password admin accounts.
- In-app teams with admin, maintainer and viewer roles, replacing `dashboard.admins`.
- GitLab/GitHub instances, their secrets and the LLM managed from the dashboard, with the config file still winning.

**Architecture:**
- **Store:** accounts, teams, the audit log, browser-added instances (sealed secrets), the LLM row and repository defaults. Store imports no other internal package.
- **New `internal/instances` registry:** merges config and database instances, and replaces every `Config.Instance/GitHubInstance/Platform` lookup.
- **Runtime maps:** platforms, webhook secrets and OAuth providers become concurrency-safe maps, so an instance can be connected or disconnected without a restart.
- **Sessions:** each session belongs to an account. On every request, `Require` combines the account's teams (read fresh) with the 5-minute platform check, to give a per-repository role.

**Tech Stack:**
- Go 1.24, pgx v5, `golang.org/x/crypto/argon2` (promote `golang.org/x/crypto` to a direct require);
- React 18 + TypeScript + react-router 7 + TanStack Query 5 + Vitest;
- Helm.

**Spec:** `docs/superpowers/specs/2026-10-31-organization-design.md`. Read it with `docs/superpowers/specs/2026-10-31-dashboard-design.md`, whose anti-goals and constraints still hold.

## Global Constraints

- **Public repository:** example names only (`acme`, `example.com`). The private-name scan must print nothing: `git grep -niE 'golabs|gojek|gopay[^t]|goto\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'`.
- **Env:** only `DATABASE_URL` and `PERGENT_SH_SECRET_KEY` must come from env. Everything else may come from the dashboard.
- **Never shown, logged or recorded:**
  - A secret entered in the panel is never returned by any API, page, log, audit entry or browser storage.
  - The only secret ever logged is the generated first admin password, once, at Warn, on the start that creates it.
- **Platform ceiling:** a platform account never sees a repository, review, finding or log its platform hides from it, whatever its teams. Password admins see everything.
- **Operator-only (password admins):**
  - password accounts;
  - instances and their secrets;
  - OAuth clients;
  - the LLM provider, model, key and `base_url`.
- **Platform admins never touch password accounts.**
- **Roles:** `admin` > `maintainer` > `viewer`. A user's role on a repository is the highest over the teams that cover it. An admin team has no repository list.
- **Team changes apply on the user's next request.** Only the platform check keeps its 5-minute refresh (`AccessTTL`).
- **Passwords:**
  - at least 12 characters, stored as argon2id;
  - 5 wrong passwords lock that username for 15 minutes;
  - every failure says exactly `wrong username or password`.
- **First password admin:** named `admin`. Its password comes from the env var named by `dashboard.admin_password_env`, or is generated (24+ characters) when that is unset or empty. It must be changed at first sign-in.
- **Config wins:**
  - config instances, the config `llm:` block and config repositories are read-only in the browser;
  - config repositories name config instances only;
  - a database instance named like a config instance stops startup, naming both.
- **Browser defaults** (approve, review drafts, failure status, Job timeout 60–7200 s, Job memory 256Mi–32Gi) apply to repositories switched on in the browser only.
- **Browser-added instances:**
  - URL, API URL and App IDs are immutable;
  - removing one switches its repositories off first, then deletes its accounts, memberships, sessions and secrets.
- **Audit log:** admin-only, kept 90 days, never a secret value.
- **No rotation feature for `PERGENT_SH_SECRET_KEY`:** a stored secret that can't be opened is reported "not set".
- **The dashboard's existing constraints stay:**
  - one replica;
  - the engine loop never waits on the dashboard;
  - every write needs a session, the CSRF token and the right role, checked on the server;
  - no secret reaches a Job beyond what it needs;
  - a dashboard bug never blocks or falsely passes a review.
- **Verification for every task:**
  - `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:pergent@localhost:55432/pergent_test?sslmode=disable' go test -count=1 ./...` with no DB skips;
  - `go vet ./...`;
  - for web changes, `cd web && npm test -- --run && npm run lint && npx tsc --noEmit`.
- **Commits:**
  - `git -c user.email=zufardhiyaulhaq@gmail.com commit`;
  - conventional subjects;
  - the message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- **Never run** `go clean`, `docker build` or `docker builder prune`, or do any cache or disk cleanup.

## Review Focus

1. **A team covering a repository the member can't see on the platform.** It never shows on any read or write route, including after the team changes. Pinned in Task 5: `TestTeamCoverageNeverExceedsPlatform`.
2. **Removing a member, team or account mid-session.** The very next request loses that access, with no 5-minute wait. Pinned in Task 5: `TestTeamChangeAppliesNextRequest`; Task 6: `TestDeleteAccountEndsSessions`.
3. **Removing a browser-added instance while runs of its repositories are queued.** The runs are superseded, its hooks and deploy tokens are removed, and nothing crashes or posts. Pinned in Task 8: `TestRemoveInstanceWithQueuedRuns`.
4. **Many usernames tried from one client.** The per-IP limit answers 429, a locked account fails with the same message and timing, and an unknown username isn't recorded. Pinned in Task 4: `TestPasswordGuessing`.
5. **No LLM configured.** Queued runs wait and nothing posts a failed status. They run once an operator sets the LLM. Pinned in Task 8: `TestDispatchWaitsWithoutLLM`.
6. **Secrets.**
   - A scan of every response and log line in the operator tests finds no secret value. Pinned in Task 8: `TestOperatorAPINeverReturnsSecrets`.
   - The end-to-end test scans all bodies (Task 11).

---

### Task 1: Store — accounts, teams, sessions bound to accounts, audit log

**Files:**
- Create: `internal/store/migrations/0009_accounts_teams.sql`, `internal/store/accounts.go`, `internal/store/teams.go`, `internal/store/audit.go`, `internal/store/accounts_test.go`, `internal/store/teams_test.go`, `internal/store/audit_test.go`
- Modify: `internal/store/sessions.go`, `internal/store/sessions_test.go`, `internal/store/dashboard_read.go` (VisibleProject gains Instance), `internal/dashboard/auth.go` (the OAuth callback upserts a platform account and puts its ID on the session, so sign-in keeps working)

**Interfaces:**
- Produces (exact names later tasks use):

```go
const (AccountPassword = "password"; AccountPlatform = "platform")
const (RoleAdmin = "admin"; RoleMaintainer = "maintainer"; RoleViewer = "viewer")
var (ErrExists = errors.New("already exists"); ErrLastPasswordAccount = errors.New("the last password account can't be removed"))

type Account struct {
	ID int64; Kind string; Username string
	Platform, Instance string; PlatformUserID int64 // platform accounts
	PasswordHash string; MustChangePassword bool     // password accounts
	FailedAttempts int; LockedUntil *time.Time
	CreatedAt time.Time; LastSignInAt *time.Time
}
func (s *Store) CreatePasswordAccount(ctx context.Context, username, hash string, mustChange bool, at time.Time) (Account, error) // ErrExists, case-insensitive
func (s *Store) PasswordAccount(ctx context.Context, username string) (Account, bool, error)        // case-insensitive
func (s *Store) CountPasswordAccounts(ctx context.Context) (int, error)
func (s *Store) UpsertPlatformAccount(ctx context.Context, platform, instance string, userID int64, username string, at time.Time) (Account, error) // sets username, last_sign_in_at
func (s *Store) Account(ctx context.Context, id int64) (Account, bool, error)
func (s *Store) Accounts(ctx context.Context) ([]Account, error) // ordered: platform accounts with no team first, then by username
func (s *Store) DeleteAccount(ctx context.Context, id int64) (bool, error) // ErrLastPasswordAccount; cascades sessions and memberships
func (s *Store) SetPassword(ctx context.Context, id int64, hash string, mustChange bool) error // also clears failures and lock
func (s *Store) RecordFailedSignIn(ctx context.Context, id int64, now time.Time, max int, lock time.Duration) (locked bool, err error)
func (s *Store) RecordSignIn(ctx context.Context, id int64, at time.Time) error // clears failures and lock
func (s *Store) DeleteAccountSessions(ctx context.Context, id int64, keep []byte) error // keep: id_hash to keep, nil = none

type TeamRepo struct{ Instance, Project string } // Project "" = every repository of Instance
type Team struct{ ID int64; Name, Role string; Members []int64; Repos []TeamRepo; CreatedAt time.Time }
func (s *Store) CreateTeam(ctx context.Context, name, role string, at time.Time) (Team, error) // ErrExists, case-insensitive name
func (s *Store) Teams(ctx context.Context) ([]Team, error)
func (s *Store) Team(ctx context.Context, id int64) (Team, bool, error)
func (s *Store) UpdateTeam(ctx context.Context, id int64, apply func(*Team) error) (bool, error) // SELECT … FOR UPDATE, replaces members and repos
func (s *Store) DeleteTeam(ctx context.Context, id int64) (bool, error)
func (s *Store) AccountTeams(ctx context.Context, accountID int64) ([]Team, error)

type AuditEntry struct{ ID int64; At time.Time; ActorID *int64; Actor, Action, Instance, Project, Detail string }
func (s *Store) Audit(ctx context.Context, e AuditEntry) error
func (s *Store) AuditLog(ctx context.Context, beforeID int64, limit int) ([]AuditEntry, error) // newest first; beforeID 0 = from the newest
func (s *Store) PruneAudit(ctx context.Context, before time.Time) (int, error)
```

- `store.Session` changes:
  - gains `AccountID int64` and `AccessScope string`;
  - keeps `IsAdmin` until Task 5 drops it, so every task leaves the suite green;
  - `Platform`, `Instance` and `UserID` are empty or zero for password sessions;
  - `TokenSealed` is nil for password sessions.
- `SetSessionAccess` gains `scope string` after `isAdmin`; Task 5 removes `isAdmin`.
- `VisibleProject{Instance, Path string; ID int64}`.
- `ClearSessionAccess` also clears `access_scope`.

- [ ] **Step 1: Write migration 0009.**
  - `DELETE FROM sessions` (nothing is released; it cascades `user_access`).
  - `accounts`:
    - `id bigserial PK`;
    - `kind text NOT NULL CHECK (kind IN ('password','platform'))`;
    - `username text NOT NULL`;
    - `platform text`, `instance text`, `platform_user_id bigint`;
    - `password_hash text`, `must_change_password boolean NOT NULL DEFAULT false`;
    - `failed_attempts int NOT NULL DEFAULT 0`, `locked_until timestamptz`;
    - `created_at timestamptz NOT NULL`, `last_sign_in_at timestamptz`;
    - unique index on `lower(username) WHERE kind='password'`;
    - unique index on `(instance, platform_user_id) WHERE kind='platform'`;
    - a CHECK that password rows have a hash, and platform rows have platform, instance and user ID.
  - `teams`: `id bigserial`, `name text`, `role text CHECK IN (…)`, `created_at`; unique index on `lower(name)`.
  - `team_members`: `team_id` and `account_id`, both FKs `ON DELETE CASCADE`, primary key on the pair.
  - `team_repos`: `team_id` FK cascade, `instance text NOT NULL`, `project text NOT NULL DEFAULT ''`; unique index on `(team_id, instance, lower(project))`.
  - `sessions`:
    - `ADD account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE`;
    - `ALTER token_sealed, platform, instance, user_id DROP NOT NULL`;
    - `ADD access_scope text NOT NULL DEFAULT ''`.
  - `audit_log`: `id bigserial`, `at timestamptz NOT NULL`, `actor_id bigint` (no FK, so entries outlive accounts), `actor text NOT NULL`, `action text NOT NULL`, `instance text NOT NULL DEFAULT ''`, `project text NOT NULL DEFAULT ''`, `detail text NOT NULL DEFAULT ''`; index on `at`.
- [ ] **Step 2: Write failing store tests.**
  - `TestPasswordAccountCaseInsensitive`: `CreatePasswordAccount("Admin")`, then `("admin")` gives `ErrExists`, and `PasswordAccount("ADMIN")` finds it.
  - `TestDeleteLastPasswordAccount`: with one password account, deleting it gives `ErrLastPasswordAccount`; with two, the deletion succeeds.
  - `TestRecordFailedSignInLocks`: four failures leave it unlocked; the fifth returns `locked=true` with `LockedUntil = now+15m`; `RecordSignIn` clears both.
  - `TestUpsertPlatformAccount`: the same (instance, user ID) twice gives one row with the updated username.
  - `TestAccountsWaitingFirst`: a platform account with no team sorts before one with a team.
  - `TestDeleteAccountCascades`: the account's sessions and memberships are gone.
  - `TestUpdateTeamReplaces`: members and repositories are replaced; `TeamRepo{Instance:"main"}` stores `project = ''`.
  - `TestAccountTeams`: returns only that account's teams.
  - `TestAuditLogPaging`: inserts 3 entries; `AuditLog(0,2)` returns the newest 2, and `AuditLog(lastID,2)` returns the remaining one.
  - `TestPruneAudit`.
  - The existing sessions tests are updated for `AccountID` and `AccessScope`.
  - `TestPlatformSignInCreatesAccount` (`internal/dashboard`, DB): a GitLab callback creates a platform account and a second sign-in reuses it; the existing dashboard tests keep passing.
- [ ] **Step 3:** Run `go test -count=1 ./internal/store/` and expect FAIL (undefined).
- [ ] **Step 4:** Implement in the files above, following the store's conventions (tx with `defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit`, `pgx.ErrNoRows` → `(zero, false, nil)`). `DeleteAccount` checks the password count inside the tx with `SELECT … FOR UPDATE` on password rows.
- [ ] **Step 5:** Run the whole Go suite with the DB; expect PASS.
- [ ] **Step 6: Commit** `feat(store): accounts, teams, account-bound sessions and the audit log`.

---

### Task 2: Store — browser-added instances, the LLM row, repository defaults

**Files:**
- Create: `internal/store/migrations/0010_instances.sql`, `internal/store/instances.go`, `internal/store/instances_test.go`

**Interfaces:**
- Produces:

```go
type InstanceRow struct {
	Name, Platform string           // config.PlatformGitLab / PlatformGitHub values: "gitlab" / "github"
	URL string                       // GitLab
	APIURL string; ReviewAppID, ApproveAppID int64 // GitHub; ApproveAppID 0 = token approver
	OAuthClientID string             // "" = no sign-in through this instance
	Removing bool; CreatedAt time.Time
}
type SealedSecret struct{ Value []byte; UpdatedAt time.Time }
func (s *Store) Instances(ctx context.Context) ([]InstanceRow, error)
func (s *Store) InstanceRow(ctx context.Context, name string) (InstanceRow, bool, error)
func (s *Store) CreateInstance(ctx context.Context, row InstanceRow, secrets map[string][]byte, at time.Time) error // ErrExists
func (s *Store) UpdateInstance(ctx context.Context, name string, oauthClientID *string, secrets map[string][]byte, at time.Time) (bool, error) // nil = keep; a key in secrets replaces that field
func (s *Store) InstanceSecrets(ctx context.Context, name string) (map[string]SealedSecret, error)
func (s *Store) MarkInstanceRemoving(ctx context.Context, name string) (bool, error)
// DeleteInstanceData deletes the instance's accounts (cascading sessions, memberships), team_repos, dashboard_repos and repo_attention rows; the instance row stays (Removing).
func (s *Store) DeleteInstanceData(ctx context.Context, name string) error
func (s *Store) DeleteInstance(ctx context.Context, name string) error // row and secrets
type LLMRow struct{ Provider, Model, BaseURL string; APIKeySealed []byte; UpdatedAt time.Time }
func (s *Store) LLM(ctx context.Context) (LLMRow, bool, error)
func (s *Store) PutLLM(ctx context.Context, provider, model, baseURL string, keySealed []byte, at time.Time) error // keySealed nil = keep
type RepoDefaults struct{ Approve, ReviewDrafts *bool; FailureStatus *string; JobTimeoutSeconds *int; JobMemory *string; UpdatedAt time.Time }
func (s *Store) RepoDefaults(ctx context.Context) (RepoDefaults, error) // zero value when unset
func (s *Store) PutRepoDefaults(ctx context.Context, d RepoDefaults) error
```

- [ ] **Step 1: Write migration 0010.**
  - `instances`: `name text PK`, `platform text CHECK`, `url`, `api_url text NOT NULL DEFAULT ''`, `review_app_id`, `approve_app_id bigint NOT NULL DEFAULT 0`, `oauth_client_id text NOT NULL DEFAULT ''`, `removing boolean NOT NULL DEFAULT false`, `created_at`.
  - `instance_secrets`: `instance` FK `ON DELETE CASCADE`, `field text`, `sealed bytea NOT NULL`, `updated_at`; primary key `(instance, field)`.
  - `llm_settings`: a single-row table (`id int PK CHECK (id = 1)`) with `provider`, `model`, `base_url`, `api_key_sealed bytea`, `updated_at`.
  - `repo_defaults`: a single row with nullable `approve`, `review_drafts`, `failure_status`, `job_timeout_seconds`, `job_memory`, plus `updated_at`.
- [ ] **Step 2: Write failing tests.**
  - `TestCreateInstanceAndSecrets`: secrets round-trip as bytes, and `UpdatedAt` is set.
  - `TestUpdateInstanceKeepsOtherSecrets`: replacing `review_token` leaves `approve_token` alone; a nil `oauthClientID` keeps it.
  - `TestDeleteInstanceDataCascades`: seeds an account, session, team repo, dashboard repo and attention row on instance `lab`, plus ones on `main`. Only `lab`'s are gone.
  - `TestLLMRowKeepsKey`: `PutLLM` with a nil key keeps the earlier sealed key.
  - `TestRepoDefaultsRoundTrip`.
- [ ] **Step 3:** Run; expect FAIL.
- [ ] **Step 4:** Implement.
- [ ] **Step 5:** Run; expect PASS.
- [ ] **Step 6: Commit** `feat(store): browser-added instances, the LLM row and repository defaults`.

---

### Task 3: Config — no admin groups, optional LLM, admin password env

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`, `internal/config/testdata/*.yaml` (only where needed)

**Interfaces:**
- Produces:
  - `Dashboard.AdminPasswordEnv string yaml:"admin_password_env"`.
  - `func (c *Config) HasLLM() bool`: true when the file has an `llm:` block with a provider.
  - `dashboard.admins` stays until Task 5 replaces it with teams.

- [ ] **Step 1: Write failing tests** in `config_test.go`.
  - `TestDashboardNeedsNoOAuthInstance`: `dashboard.url` set with no OAuth instance loads, because password admins can sign in.
  - `TestLLMOptionalWithDashboard`: no `llm:` block with `dashboard.url` set loads, `HasLLM() == false`, and `LLM.APIKeyEnv == ""`, so no default is applied.
  - `TestLLMRequiredWithoutDashboard`: no `llm:` and no dashboard gives `llm.provider is required` (existing message).
  - `TestInstanceRequiredWithoutDashboard`: no `gitlab`/`github` and no dashboard gives `at least one gitlab or github instance is required (or turn on the dashboard to add them there)`.
  - `TestAdminPasswordEnv`: the key is read; a name that isn't an env-var name (`^[A-Z_][A-Z0-9_]*$`) is refused.
  - Adjust `TestValidateRejectsDashboard` and `TestLoadDashboard` only where the OAuth rule changed.
- [ ] **Step 2:** Run `go test ./internal/config/`; expect FAIL.
- [ ] **Step 3:** Implement.
  - Apply the `api_key_env` default only when `llm.provider` is set.
  - Validate `llm.*` only when `HasLLM()` or the dashboard is off.
  - Drop the "nobody could sign in" rule in `checkDashboard`.
  - Code reading `cfg.LLM` keeps working for configs that have it; Task 8 moves the engine to the registry.
- [ ] **Step 4:** Run the config tests; expect PASS.
- [ ] **Step 5: Commit** `feat(config): optional llm with the dashboard, admin_password_env, instances required without it`.

---

### Task 4: Password sign-in, first admin, password change, reset command

**Files:**
- Create: `internal/password/password.go`, `internal/password/password_test.go`, `internal/dashboard/password.go`, `internal/dashboard/password_test.go`, `internal/dashboard/limit.go`, `cmd/pergent-sh/admin.go`, `cmd/pergent-sh/admin_test.go`
- Modify: `internal/dashboard/auth.go` (Require, callback creates a platform account), `internal/dashboard/dashboard.go` (routes, Options), `cmd/pergent-sh/main.go` (subcommand switch, bootstrap call), `go.mod`

**Interfaces:**
- Consumes: Task 1 account methods; Task 3 `Dashboard.AdminPasswordEnv`.
- Produces:

```go
package password
func Hash(pw string) (string, error)          // argon2id, PHC string "$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>", 16-byte salt, 32-byte key
func Verify(encoded, pw string) bool           // constant time; false on malformed
func Generate() string                         // 24 chars from [A-Za-z0-9], crypto/rand
const MinLength = 12
```

```go
package dashboard
// Bootstrap creates the "admin" password account when none exists: the password is os.Getenv(cfg.Dashboard.AdminPasswordEnv) when that is set and not empty, else password.Generate(), logged once at Warn as "initial admin password" with attribute password=<value>. MustChangePassword is true either way.
func Bootstrap(ctx context.Context, st *store.Store, cfg *config.Config, now time.Time, log *slog.Logger) error
// ResetPassword sets a new generated password for a password account, forces a change, clears the lock and deletes its sessions; returns the new password. ErrNotPasswordAccount when username is not a password account.
func ResetPassword(ctx context.Context, st *store.Store, username string, now time.Time) (string, error)
```

- **Routes** (all under `Handler()`):
  - `GET /auth/csrf`: sets `__Host-pergent_login_csrf` (32 random bytes, `Path=/`, 10 minutes, HttpOnly, Secure, SameSite=Strict) and answers `{"csrfToken": "<same>"}`.
  - `POST /auth/password`:
    - takes the JSON body `{username, password}` and the `X-CSRF-Token` header, which must match the login cookie (constant time); otherwise 403 `{"code":"csrf"}`;
    - on success, 204 plus the session cookie, with the existing cookie flags and the session bound to the account;
    - on failure, 401 `{"error":"wrong username or password"}`;
    - when the per-IP limit is exceeded, 429 `{"error":"too many sign-in attempts, try again later"}`.
  - `POST /auth/password/change`:
    - behind `Require`, with the session CSRF; the JSON body is `{current, new}`;
    - answers 204, 400 when the new password is shorter than `MinLength` or equals the current one, and 401 `{"error":"wrong password"}`;
    - ends the account's other sessions and clears `MustChangePassword`.
- **Per-IP limit** (`limit.go`):
  - 20 attempts per 5 minutes per client IP, held in memory;
  - the client IP is the last `X-Forwarded-For` entry when present, else the host of `RemoteAddr`;
  - a `limiter` type with `allow(ip string, now time.Time) bool`, pruned on use.
- **Password sessions in `Require`:** no platform check (no token). Until Task 5 they count as admins seeing every repository: `IsAdmin` true, and the reads skip the visibility filter (Task 5 replaces this with `Operator`/`All`).
- **`/api/me`** gains `kind` and `mustChangePassword` (Task 5 adds the rest).
- **Account lifecycle in `Require`:**
  - if the account row is gone, delete the session and answer 401;
  - while `MustChangePassword` is set, every request except `GET /api/me`, `POST /auth/logout` and `POST /auth/password/change` answers 403 `{"error":"change your password first","code":"password_change_required"}`.
- **CLI:**
  - `pergent-sh admin reset-password <username>` needs only `DATABASE_URL`. It runs `store.Open` and `Migrate`, calls `ResetPassword`, and prints `new password for <username>: <pw>` on stdout with `fmt`, never via slog.
  - Exit codes: 0 on success, 1 on an error (including not a password account), 2 on usage.
  - The `parseArgs` signature becomes `parseArgs(args []string, stderr io.Writer) (cmd command, err error)` with `type command struct{ name string; configPath string; username string }`. `name` is `"serve"` or `"reset-password"`.
- `serve` calls `dashboard.Bootstrap` after `Migrate` when the dashboard is on.

- [ ] **Step 1: Write failing tests.**
  - `password_test.go`:
    - `TestHashVerify`: the right password gives true, the wrong one false, and a malformed hash false.
    - `TestHashFormat`: the hash starts with `$argon2id$v=19$m=19456,t=2,p=1$`.
    - `TestGenerate`: the result has length 24, only alphanumerics, and two calls differ.
  - `internal/dashboard/password_test.go` (DB):
    - `TestBootstrapFromEnv`: the env value signs in and `MustChangePassword` is set.
    - `TestBootstrapGenerated`: exactly one log line contains `initial admin password`, its password signs in, and a second `Bootstrap` call logs nothing and creates nothing.
    - `TestPasswordSignIn`: needs the login CSRF cookie and header; wrong CSRF gives 403 `csrf`; success gives 204 and a session whose `/api/me` shows kind `password`.
    - `TestPasswordGuessing`:
      - five wrong passwords, then the right one, gives 401 with the same body each time;
      - an unknown username gives the same body, and no audit or account row is created for it;
      - the 21st attempt from one IP within 5 minutes gives 429;
      - after a 15-minute `clock.advance` the right password works.
    - `TestMustChangePassword`: `GET /api/overview` gives 403 `password_change_required`; after `POST /auth/password/change` the call gives 200, and another session of the account gets 401.
    - `TestChangePasswordRules`: an 11-character password gives 400, the same password gives 400, and a wrong current password gives 401.
    - `TestDeletedAccountEndsSession`: deleting the account makes the next request 401.
  - `cmd/pergent-sh/admin_test.go`:
    - `TestParseArgs` cases: `serve`, `serve --config x`, `admin reset-password bob`; errors for `admin`, `admin reset-password`, `admin nope x`, `run`.
    - `TestResetPasswordCommand` (DB): prints the password, which then signs in with `MustChangePassword`; a platform username gives exit 1.
- [ ] **Step 2:** Run; expect FAIL.
- [ ] **Step 3:** Implement.
  - Promote `golang.org/x/crypto` to a direct require.
  - For an unknown username, `Verify` against a fixed dummy hash built once at package init, so timing matches.
  - Record audit entries:
    - `sign_in`;
    - `sign_in_failed` (actor = the account's username, existing accounts only);
    - `locked`;
    - `password_changed`;
    - `password_reset` (actor `cli`).
- [ ] **Step 4:** Run; expect PASS. Run `go test ./cmd/... ./internal/dashboard/ ./internal/password/`.
- [ ] **Step 5: Commit** `feat: password admins: sign-in, first admin, password change and reset command`.

---

### Task 5: Roles and visibility from teams

**Files:**
- Create: `internal/store/migrations/0011_drop_session_admin.sql` (`ALTER TABLE sessions DROP COLUMN is_admin`)
- Modify: `internal/config/config.go` + `config_test.go` (`dashboard.admins` is refused, see below), `internal/store/sessions.go` (drop `IsAdmin` and the `isAdmin` parameter), `internal/dashboard/auth.go` (Session, Require), `internal/dashboard/access.go` (checkAccess: candidates from teams; no admin groups), `internal/dashboard/oauth.go`, `gitlab.go` and `github.go` (drop `isAdmin`), `internal/dashboard/api_read.go`, `internal/dashboard/api_write.go`, `internal/dashboard/dashboard.go` (`/api/me`), `internal/dashboard/view.go`, `internal/store/dashboard_read.go` (multi-instance visibility), and the dashboard tests
- Create: `internal/dashboard/roles.go`, `internal/dashboard/roles_test.go`

**Interfaces:**
- Consumes: Task 1 `AccountTeams`, `Session.AccountID`/`AccessScope`, `VisibleProject.Instance`.
- Produces:

```go
type Role int
const (RoleNone Role = iota; RoleViewer; RoleMaintainer; RoleAdmin)
type RepoKey struct{ Instance, Path string } // Path lower-cased
type Session struct {
	AccountID int64; Kind string // store.AccountPassword / AccountPlatform
	Platform, Instance string; UserID int64; Username string
	Operator bool           // password account
	Admin bool              // Operator, or on an admin team
	Waiting bool            // platform account on no team
	MustChangePassword bool
	All bool                // sees every repository (Operator)
	Visible map[RepoKey]int64 // platform accounts: platform-visible AND team-covered, path -> platform project ID
	Roles map[RepoKey]Role
	CSRFToken string
}
func (s Session) CanSee(instance, project string) bool
func (s Session) CanSeeRun(instance, project string, id int64) bool
func (s Session) Role(instance, project string) Role // RoleAdmin everywhere for Operator; RoleNone when not visible
```

- **`/api/me` response:**

```json
{
  "kind": "password|platform",
  "username": "...",
  "platform": "",
  "instance": "",
  "admin": true,
  "operator": true,
  "waiting": false,
  "mustChangePassword": false,
  "waitingUsers": 0,
  "csrfToken": "..."
}
```

  `waitingUsers` is the count of platform accounts with no team. It is filled only for admins.
- **The store's visibility type:** `type Visibility struct{ All bool; Projects []VisibleProject }`. Every read in `dashboard_read.go` that took visible paths and IDs now takes `Visibility`. `All` skips the filter. Otherwise it filters on `(instance, lower(path), project_id)` from `Projects`.
- **Coverage algorithm** (in `roles.go`, pure, given the teams, the switched-on repositories and the session instance):
  - an admin team covers every switched-on repository of the session's instance, with `RoleAdmin`;
  - a `TeamRepo` with `Project==""` covers every switched-on repository of that instance;
  - repositories on other instances than the session's are ignored;
  - the role is the max over covering teams;
  - `scope` is a stable hash: sha256 hex of the sorted `instance\x00lower(path)` keys covered.
- **checkAccess:**
  - checks only the covered repositories on the platform;
  - stores the visible ones with `SetSessionAccess(..., scope, ...)`.
  - The old candidate list (every switched-on repository) and the admin loop go.
- **Require, on every request:**
  1. load the account; it must exist;
  2. load `AccountTeams`, which is fresh every request;
  3. compute coverage;
  4. if the session's stored `AccessScope` differs from the current scope, treat access as stale and refresh synchronously (the existing singleflight and `signInTimeout`);
  5. `Visible` = stored platform-visible ∩ covered, and `Roles` from coverage.
  - A waiting platform account gets 403 `{"error":"an admin needs to add you to a team","code":"waiting"}` on every route except `GET /api/me` and `POST /auth/logout`. It makes no platform calls.
- **Route guards:**
  - `adminOnly` → `requireAdmin` (`se.Admin`);
  - new `requireOperator` (`se.Operator`).
  - Per-repository writes (`PUT .../settings`, `POST .../settings/reset`) need `Role >= RoleMaintainer`.
  - Switching on or off and the picker need `se.Admin`. The picker is unavailable to password admins (no platform token): `GET /api/repositories/available` answers 400 `{"error":"sign in with GitLab or GitHub to add repositories"}` for them.
  - **Change from the previous model:** a password admin switches repositories on through the operator instance page's repository list in Task 8, which lists what the review bot or App reaches.
- **Read API across instances:**
  - drop the "instance == se.Instance" scoping;
  - routes still carry `{instance}`; check `CanSee(instance, project)`;
  - Integrations and admin settings for platform admins show only their instance.
- **Config:**
  - `Dashboard.Admins` becomes `yaml.Node`, kept only to refuse it with exactly `dashboard.admins was removed: admins are now teams in the dashboard (see the README, "Teams")`;
  - `config.DashboardAdmin` is deleted;
  - test `TestDashboardAdminsRefused`.
- **Repository detail view** gains `role: "admin"|"maintainer"|"viewer"` (the session's role on it).
- **Attention:** instance-wide items go to `se.Admin` users of that instance, or to every operator. The `admin_team_unreadable` kind and `attentionAdminTeam` are deleted.

- [ ] **Step 1: Write failing tests.**
  - `roles_test.go` (pure):
    - `TestCoverageAdminTeam`;
    - `TestCoverageWholeInstance`;
    - `TestCoverageHighestRoleWins`;
    - `TestCoverageIgnoresOtherInstances`;
    - `TestScopeStable`: the same set in a different order gives the same hash.
  - Dashboard (DB, rewrite the helpers' `newEnv` so it no longer sets `Admins`). Add helpers:
    - `(e *env) team(name, role string, members []int64, repos ...store.TeamRepo) int64`;
    - `(e *env) accountOf(c *http.Cookie) int64`.
  - The tests:
    - `TestWaitingUser`: a fresh GitLab sign-in gets `/api/me` `waiting:true`; `/api/overview` gives 403 `waiting`; no `GET /projects/...` reached the fake GitLab.
    - `TestTeamCoverageNeverExceedsPlatform`: a viewer team covers widgets and payments, the platform lets the user see widgets only, so payments is 404 on the repository, review, run and log routes and missing from lists. This also holds after adding payments to the team later.
    - `TestTeamChangeAppliesNextRequest`: removing the member makes the next `/api/repositories` empty with no clock advance; adding a repository triggers a synchronous platform check (the fake GitLab saw the project call) and it shows.
    - `TestMaintainerCanEditSettings`: a maintainer's PUT settings gives 200; switch off gives 403; a viewer's PUT gives 403.
    - `TestAdminTeamIsAdmin`: an admin-team member gets `/api/me` `admin:true, operator:false`, sees every switched-on repository its platform shows, and still not the hidden one.
    - `TestOperatorSeesEverything`: a password session reads repositories on both `main` and `gh`, with no platform calls.
    - `TestOperatorCannotUsePicker`: 400.
    - `TestMeWaitingUsers`.
  - Update the existing tests that asserted admin-group behaviour, by giving those users an admin team. Delete `TestGitHubAdminTeamForbidden`/`TestGitHubAdminTeamErrorFailsClosed` and the admin-group cases of `TestAdminByGroupAndTeam`. Keep the GitLab and GitHub client tests for `IsGroupMember`/`TeamMembershipActive` only if the clients still use them; otherwise delete those client methods and their tests.
- [ ] **Step 2:** Run; expect FAIL.
- [ ] **Step 3:** Implement. The store read queries keep the per-project indexes (`TestDashboardQueriesUseIndexes` must still pass with `Visibility`).
- [ ] **Step 4:** Run the whole Go suite; expect PASS.
- [ ] **Step 5: Commit** `feat(dashboard): roles and visibility from teams; operators see everything`.

---

### Task 6: Users, teams and audit API

**Files:**
- Create: `internal/dashboard/api_org.go`, `internal/dashboard/api_org_test.go`
- Modify: `internal/dashboard/view.go`, `internal/dashboard/dashboard.go` (routes), `internal/dashboard/api_write.go` (audit on writes), `cmd/pergent-sh/main.go` (hourly `PruneAudit` with 90 days, next to `PruneSessions`)

**Interfaces:**
- Consumes: Tasks 1 and 5.
- **Produces the routes** (all JSON; every write needs the session CSRF):
  - `GET /api/users` (admin):
    - answers `[{id, kind, username, platform, instance, lastSignInAt, waiting, teams:[{id,name}]}]`;
    - waiting accounts come first.
  - `DELETE /api/users/{id}` (admin):
    - a password account needs an operator, otherwise 403;
    - the last password account gives 409 `{"error":"the last password account can't be removed"}`;
    - answers 204.
  - `POST /api/admin/accounts` (operator):
    - takes `{username}` and answers 201 `{username, password}` (generated, with `MustChangePassword`);
    - a duplicate gives 409;
    - the username must match `^[a-zA-Z0-9._-]{1,64}$`.
  - `GET /api/teams` (admin):
    - answers `[{id, name, role, members:[{id, username, platform, instance}], repos:[{instance, project}], hiddenRepos}]`;
    - for a platform admin, `repos` holds only what that admin can see (`project:""` entries only for their own instance), and `hiddenRepos` counts the rest.
  - `POST /api/teams` (admin): takes `{name, role}`, answers 201 with the team; a duplicate name gives 409.
  - `PUT /api/teams/{id}` (admin):
    - takes `{name, role, members:[id], repos:[{instance, project}]}`;
    - members must be platform accounts, otherwise 400;
    - an admin team must have `repos:[]`, otherwise 400;
    - for a platform admin, the saved repositories are (submitted ∩ what they can see, plus their own instance's whole-instance entry) ∪ (existing entries they can't see), and they can't add an entry for another instance (400);
    - a repository must be switched on, otherwise 400.
  - `DELETE /api/teams/{id}` (admin): answers 204.
  - `GET /api/audit?before=<id>` (admin):
    - pages of 100, newest first, as `[{id, at, actor, action, instance, project, detail}]`;
    - for a platform admin, entries whose instance+project they can't see are left out, and entries with no project are kept.
- **Audit actions** recorded by this task and by the existing writes:
  - `account_created`, `account_deleted`;
  - `team_created`, `team_updated` (detail lists the changed parts: `name`, `role`, `members`, `repos`), `team_deleted`;
  - `repository_on`, `repository_off`;
  - `settings_updated` (detail = the setting keys), `settings_reset`.

- [ ] **Step 1: Write failing tests** in `api_org_test.go`.
  - `TestUsersWaitingFirst`.
  - `TestDeleteAccountEndsSessions`: after the deletion, the deleted user's next request is 401.
  - `TestPlatformAdminCannotDeletePasswordAccount`: 403.
  - `TestDeleteLastPasswordAccount`: 409.
  - `TestCreatePasswordAccount`: an operator gets 201 and the password signs in; a platform admin gets 403; a bad username gives 400.
  - `TestTeamCRUD`.
  - `TestAdminTeamNoRepos`.
  - `TestTeamMembersMustBePlatform`.
  - `TestPlatformAdminTeamEditKeepsHidden`: a team holds widgets (visible) and payments (hidden); the platform admin PUTs `repos:[]`; the stored repos are `[payments]`, and GET shows `repos:[], hiddenRepos:1`.
  - `TestAuditFiltered`: a platform admin doesn't see an entry for a hidden repository.
  - `TestAuditNoSecrets`: after the team and account operations, no audit detail contains the generated password.
  - `TestWritesAudited`: switching on, settings and switching off each leave one entry with the actor's username.
  - `TestDeveloperGets403OnOrgRoutes`: a viewer gets 403 on every route above.
- [ ] **Step 2:** Run; expect FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run; expect PASS.
- [ ] **Step 5: Commit** `feat(dashboard): users, teams and audit log API`.

---

### Task 7: Instance registry and runtime connections

**Files:**
- Create: `internal/instances/instances.go`, `internal/instances/instances_test.go`, `internal/syncmap/syncmap.go`, `internal/syncmap/syncmap_test.go`, `cmd/pergent-sh/connect.go`, `cmd/pergent-sh/connect_test.go`
- Modify:
  - every caller of `Config.Instance`, `Config.GitHubInstance` and `Config.Platform`: `settings.go`, `engine/{reconcile,attention,publish}.go`, `webhook/{gitlab,github}.go`, `dashboard/{dashboard,api_read,auth}.go`, `cmd/pergent-sh/main.go`;
  - `engine.Engine` (`Platforms`, `WebhookSecrets` types);
  - `server.Deps`;
  - `webhook` handlers (`Secrets` types);
  - `dashboard.Auth` (`providers`);
  - `main.go` (`connectGitLab`/`connectGitHub` move into `connect.go`).

**Interfaces:**
- Consumes: Task 2 instance store methods; `secretbox`.
- Produces:

```go
package syncmap
type Map[V any] struct{ /* sync.RWMutex + map[string]V */ }
func New[V any](init map[string]V) *Map[V]
func (m *Map[V]) Get(key string) (V, bool)
func (m *Map[V]) Set(key string, v V)
func (m *Map[V]) Delete(key string)
func (m *Map[V]) Keys() []string // sorted
```

```go
package instances
const (
	FieldReviewToken = "review_token"; FieldApproveToken = "approve_token"; FieldWebhookSecret = "webhook_secret"
	FieldOAuthClientSecret = "oauth_client_secret"; FieldReviewAppKey = "review_app_key"; FieldApproveAppKey = "approve_app_key"
)
type Source string
const (SourceConfig Source = "config"; SourceDashboard Source = "dashboard")
type Instance struct {
	Name, Platform string; Source Source; Removing bool
	GitLab *config.Instance        // URL set; *_env empty for dashboard instances
	GitHub *config.GitHubInstance  // APIURL, ReviewApp.AppID, ApproveApp (nil = token approver)
	OAuthClientID string
}
type Secrets struct{ ReviewToken, ApproveToken, WebhookSecret, OAuthClientSecret, ReviewAppKey, ApproveAppKey string }
func (s Secrets) Fields() map[string]string // non-empty fields by Field* name
type Registry struct{ /* cfg, store, box, mu, dashboard instances */ }
func New(cfg *config.Config, st *store.Store, box *secretbox.Box) *Registry
func (r *Registry) Load(ctx context.Context) error // reads store.Instances; error "instance %q is in both the config file and the dashboard" on a clash
func (r *Registry) Get(name string) (Instance, bool)
func (r *Registry) Platform(name string) (string, bool)
func (r *Registry) All() []Instance                // config ones first, then dashboard ones, each by name
func (r *Registry) Secrets(ctx context.Context, name string) (Secrets, []string, error) // config: from env via config.Instance.Secrets/OAuthClient; dashboard: opened values; second return = fields that couldn't be opened (reported "not set")
func (r *Registry) Put(inst Instance)              // after a store write
func (r *Registry) Remove(name string)
```

```go
// cmd/pergent-sh/connect.go
type connector struct {
	reg *instances.Registry; store *store.Store; hc *http.Client; log *slog.Logger
	platforms *syncmap.Map[engine.Platform]; gitlabHooks, githubHooks *syncmap.Map[string]
	auth *dashboard.Auth // nil without the dashboard
	githubRepos func(instance string) githubforge.Repos
}
func (c *connector) Check(ctx context.Context, inst instances.Instance, s instances.Secrets) error // the startup checks, no side effects
func (c *connector) Connect(ctx context.Context, inst instances.Instance, s instances.Secrets) error // Check, then build and Set into the maps, auth.SetProvider
func (c *connector) Disconnect(name string)
```

- **`dashboard.Auth`:**
  - `func (a *Auth) SetProvider(inst instances.Instance, s instances.Secrets)` (no provider when `OAuthClientID == ""`);
  - `func (a *Auth) RemoveProvider(name string)`;
  - `providers` becomes `*syncmap.Map[provider]`.
- **`dashboard.Connector` interface** = `Check`, `Connect`, `Disconnect` with the signatures above. `Options` gains `Connector Connector` and `Instances *instances.Registry`.
- **Engine and settings:**
  - `engine.Engine` gains `Instances *instances.Registry`;
  - `Platforms` becomes `*syncmap.Map[Platform]`;
  - `WebhookSecrets` becomes `*syncmap.Map[string]`;
  - `settings.Resolver` gains `Instances *instances.Registry`, used instead of `Config.Platform`.
- **Startup in `serve`:**
  1. Config instances connect as today; a failure is fatal.
  2. `reg.Load`; a clash is fatal.
  3. Dashboard instances connect from the store. A failure logs an Error naming the instance and records an instance-wide attention item, kind `instance_unreachable`, with the error message (redacted). It never stops startup.
- **`GET /api/instances`** (sign-in buttons) lists every registry instance with an OAuth client, and also returns `{password: true}` when the dashboard is on, so the page shows the form.

- [ ] **Step 1: Write failing tests.**
  - `syncmap_test.go`: `TestConcurrentSetGet` (run with `-race`).
  - `instances_test.go` (DB):
    - `TestRegistryMergesConfigFirst`;
    - `TestRegistryClash`;
    - `TestSecretsFromEnvAndStore`;
    - `TestSecretsUnopenable`: a value sealed with another key comes back in the second return, and no error.
  - `connect_test.go`:
    - port `TestConnectGitLab` and `TestConnectGitHubApps` to `connector.Connect`;
    - `TestConnectDashboardInstanceAtRuntime`: after `Connect`, a webhook to `/webhooks/gitlab/lab` is accepted with the stored secret; after `Disconnect`, 404.
  - Webhook tests construct handlers with `syncmap` secrets and a registry.
  - `TestStartupDashboardInstanceFailureNotFatal`: a dashboard instance with a bad token; serve's connect step returns no error and an `instance_unreachable` attention row exists.
- [ ] **Step 2:** Run; expect FAIL.
- [ ] **Step 3:** Implement. This is a mechanical port of each `Config.Instance/GitHubInstance/Platform` call to the registry. Behaviour for config instances must not change: the whole existing suite stays green.
- [ ] **Step 4:** Run `go test -race -count=1 ./internal/syncmap/ ./internal/instances/ ./cmd/...` and the whole suite; expect PASS.
- [ ] **Step 5: Commit** `refactor: instances come from a registry over the config and the store; connections can change at runtime`.

---

### Task 8: Operator API — instances, LLM, defaults; the engine reads them

**Files:**
- Create: `internal/dashboard/api_operator.go`, `internal/dashboard/api_operator_test.go`, `internal/engine/instances.go`, `internal/engine/instances_test.go`
- Modify:
  - `internal/instances/instances.go`: LLM;
  - `internal/engine/{dispatch,publish,logs,engine}.go`: LLM from the registry, removal finishing;
  - `internal/settings/settings.go`: browser defaults for dashboard repositories;
  - `internal/dashboard/api_read.go`: Integrations and admin settings views;
  - `view.go`;
  - `cmd/pergent-sh/main.go`.

**Interfaces:**
- Consumes: Tasks 2, 5, 6 and 7.
- Produces:

```go
// instances
func (r *Registry) LLM() (llm config.LLM, key string, source Source, ok bool) // config when cfg.HasLLM() (key from env), else the store row; ok=false when neither or the key can't be opened
func (r *Registry) LoadLLM(ctx context.Context) error // re-read the store row; called at startup and after PUT
func (r *Registry) LLMKeys() []string                 // every key seen since start, for redaction
```

- **Engine:**
  - `LLMKey` and every `e.Config.LLM` use become `e.Instances.LLM()`;
  - redaction uses `LLMKeys()`;
  - with no LLM, `dispatch` claims nothing, and logs at Warn at most once a minute: `no LLM is set: an operator sets it on Admin → Settings`.
- **`func (e *Engine) finishRemovals(ctx context.Context)`** runs in the reconcile pass. For each registry instance with `Removing` whose projects (store `Projects`) are all gone, it calls `store.DeleteInstance` and `Instances.Remove`, then the `OnInstanceRemoved` hook (`Engine.OnInstanceRemoved func(name string)`, wired to `connector.Disconnect`).
- **Settings:** `Resolver.defaults(source)`. For `SourceDashboard`, `store.RepoDefaults` fields override the hard-coded and config defaults. `SourceConfig` is unchanged.
- **Routes:**
  - `GET /api/admin/instances` (operator):
    - answers `[{name, platform, source, removing, url, apiURL, reviewAppID, approveAppID, approver:"app"|"token", oauthClientID, secrets:{<field>:{set bool, updatedAt}}}]`;
    - never a value.
  - `POST /api/admin/instances` (operator):
    - takes `{name, platform, url | apiURL+reviewAppID+approveAppID, oauthClientID, secrets:{field: value}}`;
    - the name must match the config rule `^[a-z0-9-]+$` and not exist in either source (409);
    - for GitLab, `webhook_secret` is generated (32 random bytes, hex) and any value given is refused (400);
    - `Check`, then seal, then `CreateInstance`, then `Registry.Put`, then `Connect`;
    - answers 201 with the GET item.
  - `PUT /api/admin/instances/{name}` (operator):
    - takes `{oauthClientID?, secrets:{field: value}}`;
    - a config instance gives 409;
    - changing url/apiURL/app IDs gives 400 `{"error":"an instance's address and App IDs can't change: remove it and add it again"}`;
    - `Check` with the merged secrets, then `UpdateInstance`, then `Connect`.
  - `DELETE /api/admin/instances/{name}` (operator):
    - a config instance gives 409;
    - `MarkInstanceRemoving`, then supersede the queued, running and reported runs of its repositories (store `SupersedeMR` per MR via a new `store.SupersedeInstance(ctx, instance) (int, error)`), then `DeleteInstanceData`, then `ReconcileSoon` for each of its switched-on projects, then `Auth.RemoveProvider`;
    - answers 202 `{"status":"removing"}`;
    - the engine finishes the removal.
  - `GET /api/admin/llm` (admin): answers `{source, provider, model, baseURL, key:{set, updatedAt}, editable}`, where `editable` = operator and not config.
  - `PUT /api/admin/llm` (operator):
    - takes `{provider, model, baseURL, apiKey?}`;
    - config gives 409;
    - provider and model must be non-empty;
    - `baseURL` must be empty or an absolute http(s) URL;
    - `apiKey` is required when none is stored;
    - then `PutLLM` and `LoadLLM`.
  - `GET /api/admin/defaults` (admin) and `PUT /api/admin/defaults` (admin):
    - fields `{approve, reviewDrafts, failureStatus, jobTimeoutSeconds, jobMemory}`;
    - the same ranges and messages as the per-repository settings.
  - `GET /api/admin/instances/{name}/repositories` (operator, the picker for password admins):
    - reachable repositories via `RepoCatalog.ReachableRepos` (search, at most 500, truncated);
    - `POST /api/repositories` accepts operators without a platform availability check (they see everything).
- **Audit:**
  - `instance_added`;
  - `instance_updated` (detail = the field names changed, never values);
  - `instance_removed`;
  - `llm_updated` (detail = fields changed, `api_key` named but never its value);
  - `defaults_updated`.

- [ ] **Step 1: Write failing tests.**
  - `api_operator_test.go` (DB, fake GitLab/GitHub from the helpers):
    - `TestAddGitLabInstance`: 201; a webhook to the new instance is accepted with the generated secret; GET shows `webhook_secret:{set:true}`.
    - `TestAddInstanceCheckFails`: the review and approve tokens are the same user, giving 400 with the startup message, and nothing is stored.
    - `TestUpdateInstanceImmutable`.
    - `TestConfigInstanceReadOnly`: 409 on PUT and DELETE.
    - `TestPlatformAdminCannotTouchInstances`: 403 on all operator routes.
    - `TestRemoveInstanceWithQueuedRuns`: a queued run on `lab` is superseded, accounts and sessions of `lab` are gone (that user's next request is 401), the engine's reconcile removes the hook, and after `finishRemovals` the registry has no `lab`, its secrets are gone, and webhooks to it give 404.
    - `TestLLMEditable`: an operator PUTs; dispatch then uses the new model and key (Job Secret holds the new key, env `OPENCODE_MODEL` the new model).
    - `TestLLMConfigWins`: config `llm:` makes PUT 409 and GET `editable:false`.
    - `TestDefaultsApplyToDashboardReposOnly`: defaults `approve:false` make a dashboard repository's effective `Approve` false, and a config repository's stays true.
    - `TestOperatorAPINeverReturnsSecrets`: after adding instances and the LLM with distinctive secret values, scan every recorded response body and the log buffer; none contains any value.
  - `internal/engine/instances_test.go`:
    - `TestDispatchWaitsWithoutLLM`: with no LLM, a queued run stays queued, no Job, no status posted; after `PutLLM` and `LoadLLM` the next pass dispatches.
    - `TestRedactsEveryLLMKeySeen`.
- [ ] **Step 2:** Run; expect FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the whole suite; expect PASS.
- [ ] **Step 5: Commit** `feat: operators manage instances, their secrets, the LLM and defaults in the dashboard`.

---

### Task 9: Frontend — sign-in, password change, waiting, roles in the layout

**Files:**
- Modify: `web/src/api.ts` (`Me`, new calls), `web/src/App.tsx`, `web/src/layout/Layout.tsx`, `web/src/pages/SignIn.tsx`, `web/src/pages/RepositoryDetail.tsx`, `web/src/pages/RepositorySettings.tsx`, `web/src/pages/Overview.tsx`, `web/src/pages/Repositories.tsx`, `web/src/test/fixtures.ts`, the affected tests
- Create: `web/src/pages/ChangePassword.tsx`, `web/src/pages/Waiting.tsx`, `ChangePassword.test.tsx`, `Waiting.test.tsx`

**Interfaces:**
- Consumes: the Task 4 routes, the Task 5 `/api/me` shape, and Task 7 `/api/instances` `{password}`.
- Produces:
  - `type Me = {kind: "password"|"platform"; username: string; platform: string; instance: string; admin: boolean; operator: boolean; waiting: boolean; mustChangePassword: boolean; waitingUsers: number; csrfToken: string}`.
  - `api.signIn(username, password): Promise<void>` (GETs `/auth/csrf`, then POSTs `/auth/password`).
  - `api.changePassword(current, next, csrf)`.
  - Fixtures: `operator`, `platformAdmin`, `maintainer`, `viewer`, `waiting`, `mustChange` (`Me`).

- **Behaviour:**
  - **SignIn:** a username and password form above the platform buttons. A failure shows the server's `error` text. 429 shows its text.
  - **Layout gate:**
    - `mustChangePassword` renders only `<ChangePassword/>`;
    - `waiting` renders only `<Waiting/>`, with the text `You're signed in as {username} on {instance}. An admin needs to add you to a team.` and a sign-out button.
  - **Nav:**
    - the Admin section shows for `admin`, with Users (badge `{waitingUsers} waiting` when > 0), Teams, Integrations, Settings and Audit log;
    - the role label is `operator`, `admin` or `member`.
  - **Repository Settings tab:** shown when the repository detail's `role` (from Task 5; add it to `RepositoryDetailView` in `api.ts` and the fixture) is `maintainer` or `admin`. The on/off switch and Add repository show for `admin`.

- [ ] **Step 1: Write failing tests.**
  - `SignIn.test.tsx`: the form posts to `/auth/password` with the CSRF from `/auth/csrf`; 401 shows `wrong username or password`.
  - `ChangePassword.test.tsx`: posts current and new; a new password shorter than 12 characters is refused client-side with `at least 12 characters`.
  - `Waiting.test.tsx`.
  - `App.test.tsx`:
    - the operator nav has all 5 admin items;
    - the viewer nav has none, and `/teams` gives Not found;
    - a maintainer sees the Settings tab but not the switch.
- [ ] **Step 2:** Run `cd web && npm test -- --run`; expect FAIL.
- [ ] **Step 3:** Implement. Plain text rendering only, as today.
- [ ] **Step 4:** Run `npm test -- --run && npm run lint && npx tsc --noEmit`; expect PASS.
- [ ] **Step 5: Commit** `feat(web): password sign-in, password change, waiting page, role-aware layout`.

---

### Task 10: Frontend — Users, Teams, Audit log, editable Integrations and Settings

**Files:**
- Create: `web/src/pages/Users.tsx`, `web/src/pages/Teams.tsx`, `web/src/pages/AuditLog.tsx`, and a test beside each
- Modify: `web/src/pages/Integrations.tsx`, `web/src/pages/AdminSettings.tsx` (+ tests), `web/src/App.tsx`, `web/src/api.ts` (view types mirroring Tasks 6 and 8)

**Interfaces:**
- Consumes: the Task 6 and Task 8 routes.

- **Pages:**
  - **Users:**
    - a table with waiting accounts first, marked `waiting`;
    - Remove (confirm dialog);
    - for operators, "Add password admin", which shows the generated password once in a box with the text `Copy it now: it won't be shown again.`
  - **Teams:**
    - list and create (name + role);
    - edit opens members (a multi-select of platform accounts) and repositories (switched-on ones plus "All repositories of {instance}");
    - an admin team hides the repository picker and shows `Admins see every repository they can see on their platform.`;
    - `hiddenRepos > 0` shows `+{n} repositories you can't see` (kept on save).
  - **Audit log:** a table, and "Older" paging with `before`.
  - **Integrations:**
    - operators see every instance from `GET /api/admin/instances`, with "Add GitLab instance" and "Add GitHub instance" forms;
    - secret inputs are `type=password` and `autocomplete="off"`, show `set · changed {ago}` or `not set`, and offer "Replace";
    - Remove (confirm; then `removing` shows until gone);
    - config instances are marked `config`, read-only;
    - platform admins keep today's read-only view of their instance.
  - **Settings:**
    - defaults are editable for admins;
    - the LLM section is editable when `editable`, with the key input write-only;
    - config-sourced data shows `from the config file`.
  - **Repository picker for operators:** on Repositories, "Add repository" for an operator first asks for the instance, then lists `GET /api/admin/instances/{name}/repositories`.

- [ ] **Step 1: Write failing tests** (one file per page).
  - `Users.test.tsx`: waiting first; the add-admin flow shows the password once; a platform admin has no "Add password admin".
  - `Teams.test.tsx`: an admin team hides the repositories; `hiddenRepos` text; save sends the PUT body.
  - `AuditLog.test.tsx`: paging.
  - `Integrations.test.tsx`: an operator adds a GitLab instance (the POST body has no `webhook_secret`); secret fields never prefill; a config instance has no edit buttons.
  - `AdminSettings.test.tsx`: the LLM read-only when `editable:false`; defaults PUT.
  - `Repositories.test.tsx`: the operator picker.
- [ ] **Step 2:** Run; expect FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `npm test -- --run && npm run lint && npx tsc --noEmit`; expect PASS.
- [ ] **Step 5: Commit** `feat(web): users, teams, audit log, and editable integrations and settings`.

---

### Task 11: Chart, README, end-to-end test

**Files:**
- Modify:
  - `charts/pergent-sh/values.yaml`: drop the default `config.gitlab` example instance and the default `config.llm` block (move both to comments); add `adminPassword.existingSecret`, `adminPassword.key`;
  - `charts/pergent-sh/templates/deployment.yaml`: `env` with `valueFrom.secretKeyRef` for the var named by `config.dashboard.admin_password_env`, rendered only when `adminPassword.existingSecret` is set;
  - `charts/pergent-sh/templates/_helpers.tpl`: guard — `adminPassword.existingSecret` set requires `config.dashboard.admin_password_env`;
  - `charts/pergent-sh/ci-values.yaml`;
  - `charts/pergent-sh/README.md` (via `make readme`);
  - `README.md`;
  - `internal/engine/e2e_dashboard_test.go`.

**README changes** (headings are the current ones):
- **Install with Helm:** the Secret needs only `DATABASE_URL` and `PERGENT_SH_SECRET_KEY`. Instances can be added in the dashboard instead of the config.
- **Environment variables:**
  - instance and LLM rows say "when the instance or LLM is in the config file";
  - add the admin password var;
  - the `PERGENT_SH_SECRET_KEY` row says changing it isn't supported: secrets entered in the dashboard must then be entered again.
- **Dashboard:**
  - replace "Who sees what" admin text with a new subsection **Teams**: password admins, platform accounts, waiting, roles, coverage, the platform ceiling, and what platform admins can't do;
  - add **First sign-in**: the generated password log line and `adminPassword.existingSecret`;
  - add **Lost password**: the `kubectl exec deploy/<release> -- /pergent-sh admin reset-password admin` command;
  - add **Instances and secrets in the dashboard**: operator-only, write-only, checked on save, removing an instance;
  - add **Audit log**.
- **Config reference:**
  - remove `dashboard.admins[]` rows;
  - add `dashboard.admin_password_env`;
  - `llm.*` rows say optional with the dashboard.
- **HTTP routes:** `/auth/csrf`, `/auth/password`, `/auth/password/change`.
- **Limits:** replace the "two roles … no audit log … GitLab or GitHub only" line with the current limits: no OIDC/SAML/LDAP, no team sync, no 2FA, no API tokens, no key rotation.

- [ ] **Step 1: Rewrite the e2e** `TestEndToEndDashboard`.
  - No `Dashboard.Admins`.
  - `Bootstrap` with a known env password.
  - The operator signs in with a password, changes it, and adds a team `platform` (role admin) with no members.
  - alice signs in with GitLab and is waiting; the operator adds her to `platform`; alice switches gadgets on as today.
  - bob signs in, waits, is added to a viewer team covering gadgets, and sees gadgets.
  - The remaining steps run as today (review, log redaction, approval off with withdrawal, bob's writes 403).
  - The leak scan also covers the bootstrap and changed passwords.
- [ ] **Step 2:** Run `go test -count=1 -run TestEndToEndDashboard ./internal/engine/`; expect PASS.
- [ ] **Step 3:** Update the chart and run:
  - `helm lint charts/pergent-sh`;
  - `helm template t charts/pergent-sh -f charts/pergent-sh/ci-values.yaml` (expect success);
  - `helm template` with `adminPassword.existingSecret=x` and no `admin_password_env` (expect the guard's failure);
  - `make readme && git diff --exit-code charts/pergent-sh/README.md` after committing the regenerated file.
- [ ] **Step 4:** Update `README.md`. Every claim must match the code; quote the exact messages from Tasks 3–8.
- [ ] **Step 5:** Full verification:
  - the whole Go suite with the DB;
  - `go vet`;
  - web test, lint and typecheck;
  - `make build`;
  - helm lint;
  - the private-name scan.
- [ ] **Step 6: Commit** `docs: password admins, teams, instances in the dashboard; chart and end-to-end test`.
