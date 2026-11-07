# Sign-in and Access Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Implementers run on **Sonnet** with the `pergent-developer` agent. Task reviews and the final branch review run on **Opus**.

**Goal:** People sign in through OIDC or a validated JWT, an email is the user, and access is user → team → repository, with GitHub and GitLab sign-in and the platform check removed.

**Architecture:** Accounts become local admins or SSO accounts. A session's access comes only from the account's role and `team_members`/`team_repos`, so `checkAccess` and every per-instance rule go. Sign-in settings live in one row with a sealed client secret, and one verifier built on `go-oidc` serves both OIDC and JWT. The web app splits into `/` and `/admin`, and the server enforces that split.

**Tech Stack:** Go, Postgres migrations, `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`, React with Vite and vitest.

**Spec:** `docs/superpowers/specs/2026-11-07-user-access-design.md`. Read its anti-goals first. They are the failure bar.

## Scope

No release in this plan. It ends at a pushed branch and a PR, on the owner's word. The second release that drops the old columns is not in this plan. Another agent is building features on `main` at the same time, so never assume `main` stands still.

## Hand-offs

Implementers have no network, so these belong to the owner's main session.

| When | Who | What |
|---|---|---|
| **H0**, before Task 1 | Main session | Cut `feat/user-access` from the latest `origin/main` in `pergent.sh-access`, carrying the spec and this plan. List `internal/store/migrations` on `origin/main` and every open PR's migrations; the migration takes the next number free of all of them. Replace `NNNN` below. |
| **H1**, before Task 3 | Main session | `go get github.com/coreos/go-oidc/v3@latest` and `go mod tidy`, committed alone as `chore(deps): go-oidc`. |
| **H2**, before each task | Main session | `git fetch`, and merge `origin/main` when it moved. Re-check the migration number. Tell the next implementer about anything that touched `internal/dashboard`, `internal/store` or `web/src`. Touch no other agent's branch or worktree. |
| **H3**, after the final review | Owner | Push and open the PR on the owner's word. Postgres-backed tests first run here. Read the `test` job log for `internal/store`, `internal/dashboard` and `internal/engine` timings. |

## Global Constraints

- Never run docker, orbctl or anything that starts a container. No `gh`, `glab`, `curl` or other network step in a task. No push, tag or PR. No disk or cache cleanup. Delete nothing outside your own scratch. Don't run `go get`.
- Tests marked **[CI]** need Postgres. Write them with the same care, confirm they compile with `go vet ./...`, and report them as not run.
- **Go gate:** `go vet ./...`, `go test ./...`. **Web gate:** in a scratch `git archive` with a `web/node_modules` symlink to `/Users/zufardhiyaulhaq/Documents/personal/github/pergent.sh-061/web/node_modules`: `npm run typecheck && npm run lint && npx vitest run && npm run build`. Not while `go test` runs.
- **Private-name scan** before every commit must print nothing: `git grep -niE 'gol[a]bs|goj[e]k|gop[a]y[^t]|got[o]\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'`
- Commit with the owner's email as the agent definition says, staged by path, ending with a `Co-Authored-By:` line naming the model that wrote the change. Gate the commit on its checks with `&&`.
- The bot, Apps, webhooks and `@<bot>` commands stay unchanged. If a change reaches `internal/engine`, `internal/webhook` or `internal/actions`, stop and report it.
- Nothing is dropped or renamed: the migration adds, and code stops using the rest.
- Never log a token, a client secret or a full ID token. Emails may appear in the audit log; on a PR only the part before `@`.
- The README is the behaviour contract: change it in the same commit as the behaviour.
- Names: `sso` account kind, roles `admin` and `user`, team roles `viewer` and `maintainer`, sign-in modes `oidc` and `jwt`, paths `/sign-in`, `/admin/sign-in`, `/admin`.

## Review Focus

1. **A cross-site `POST` in JWT mode,** with a valid header and no cookie: no session, no change.
2. **A normal user calling every admin route:** "not found" on all of them, including any added later.
3. **A repository switched on but not yet reconciled, covered by a team:** the user sees it. A team removed: access ends on the next request.
4. **An upgrade on a database with platform accounts, teams and live sessions:** a local admin signs in, teams are memberless, and the previous release still reads the database.
5. **An issuer changed:** SSO sessions end, the stored client secret isn't sent to the new issuer, and a token from the old issuer is refused.

---

### Task 1: Schema and store

**Files:**
- Create: `internal/store/migrations/NNNN_user_access.sql`, `internal/store/signin.go`
- Modify: `internal/store/accounts.go`, `internal/store/teams.go`, `internal/store/sessions.go`
- Test: `internal/store/accounts_test.go`, `internal/store/teams_test.go`, `internal/store/signin_test.go`

**Interfaces:**
- Produces:
  - `store.AccountSSO = "sso"`; `Account` gains `Email string`, `Role string` (`RoleAdmin = "admin"`, `RoleUser = "user"`), `Added bool`. An account is `invited` while `LastSignInAt` is nil.
  - `AddSSOAccount(ctx, email string, at time.Time) (Account, error)`, setting `Added`; `SignInSSOAccount(ctx, email string, at time.Time) (Account, error)`, creating or updating; `SSOAccountByEmail(ctx, email string) (Account, bool, error)`; `SetAccountRole(ctx, id int64, role string) error`.
  - `Team.Members []TeamMember` with `TeamMember{AccountID int64; Role string}`; `Team.Role` is no longer read; `teams.role` is `NOT NULL`, so `CreateTeam` writes `viewer` there until the later release drops it.
  - `SignInSettings{Mode, Label, Issuer, ClientID, Header, Audience, JWKSURL, EmailClaim string; AllowedDomains, AllowedEmails []string; ClientSecretSealed []byte}`, `Store.SignInSettings(ctx)`, `Store.PutSignInSettings(ctx, s)`.
  - `store.Session` gains `SignIn` (`password`, `oidc` or `jwt`) and `Issuer`; `DeleteSSOSessions(ctx) error`.

The migration adds, in one file: `accounts.email`, `accounts.role` (`NOT NULL DEFAULT 'user'`, password rows set to `admin`), `accounts.added`, the kind check widened to `sso`, a unique index on `lower(email)`, `team_members.role` (`NOT NULL DEFAULT 'viewer'`), `sessions.sign_in`, `sessions.issuer`, the `sign_in_settings` table with a single-row guard, and `DELETE FROM sessions`. It drops nothing. Platform rows stay, and `Accounts` leaves them out.

- [ ] **Step 1: Write the failing tests.**
  - **[CI]** `TestMigrationKeepsPreviousRelease` (Review Focus 4): with platform accounts, teams and members seeded, every column the previous release reads still exists, a password account's role is `admin`, and the previous release's account query still scans with an `sso` row present, whose platform columns are NULL.
  - **[CI]** `TestAddThenSignInSSOAccount`: `AddSSOAccount("a@example.com")` then `SignInSSOAccount("A@Example.com")` is one row, `Added` true, `LastSignInAt` set.
  - **[CI]** `TestSignInSSOAccountNotAdded`: `Added` false, role `user`.
  - **[CI]** `TestAccountsHidePlatformRows`, `TestTeamMemberRoles`, `TestSignInSettingsRoundTrip`, `TestDeleteSSOSessionsKeepsPassword`.
  - `TestNormalizeEmail` (no database, in `internal/store`): ASCII lower-casing only, so `K` (U+212A) is not folded to `k`; surrounding spaces and a second `@` are refused.

- [ ] **Step 2: Run** `go vet ./internal/store`. Expected: compile failure.

- [ ] **Step 3: Implement** the migration and store functions, with `func NormalizeEmail(s string) (string, error)` in `internal/store/accounts.go`. Keep `UpsertPlatformAccount` and its callers until Task 2 removes them.

- [ ] **Step 4: Run the Go gate.** Expected: PASS, [CI] tests compiling.

- [ ] **Step 5: Scan and commit:** `feat(store): SSO accounts, member roles and sign-in settings`.

---

### Task 2: Access comes from teams

**Files:**
- Modify: `internal/dashboard/auth.go`, `access.go`, `roles.go`, `api_org.go`, `api_read.go`, `api_write.go`, `api_operator.go`, `view.go`, `dashboard.go`, `cmd` startup wiring that calls `SetProvider`
- Delete from use: `internal/dashboard/oauth.go`, `github.go`, `gitlab.go`, `connect.go`, as far as they serve sign-in
- Test: `internal/dashboard/access_test.go`, `roles_test.go`, `api_org_test.go`

**Interfaces:**
- Consumes: Task 1's accounts and teams.
- Produces: `Session` without `Platform`, `Instance`, `UserID`, `Operator`; with `Email`, `Admin`, `Waiting`, `SignIn`. `func (a *Auth) load(ctx, account store.Account) (accessOf, error)` builds `All`, `Visible` and `Roles` from the role and the teams. `Session.Role(key RepoKey) Role` keeps its meaning. `RoleViewer`, `RoleMaintainer` remain, and `RoleAdmin` means the account.

The spec's "What goes away" and "What changes" list the functions. `Visible` keeps its shape, filled from `projects` by path; a covered, switched-on repository with no `projects` row yet is kept visible with ID 0, and `CanSee` no longer needs an ID above 0. Team writes take members as `{accountId, role}`. Today's `requireOperator` becomes `requireAdmin`, and every use of `se.Instance` for scoping goes.

- [ ] **Step 1: Write the failing tests.**
  - `TestAdminSeesEverything`, `TestUserSeesOnlyTeamRepos`, `TestUserOnNoTeamIsWaiting`.
  - `TestHighestRoleWins`: viewer on one team and maintainer on another gives maintainer.
  - `TestInstanceWideTeamCoversNewRepository`.
  - `TestUnreconciledRepositoryIsVisible` (Review Focus 3): covered and switched on, with no `projects` row.
  - `TestTeamRemovalEndsAccess` (Review Focus 3): the next request sees nothing.
  - `TestAdminCantRemoveOwnRole`, `TestAdminCantDeleteSelf`, `TestLastLocalAdminCantBeRemoved`.
  - `TestPlatformSignInRoutesAreGone`: `/auth/github/x/login` is 404.

- [ ] **Step 2: Run** `go test ./internal/dashboard -run 'Admin|User|Team|Repository|Platform'`. Expected: FAIL.

- [ ] **Step 3: Implement.** Sign-in works only by password at the end of this task. That's expected.

- [ ] **Step 4: Update `README.md`:** the access sections for what's removed. Rewrite **Teams** and **Who sees what**.

- [ ] **Step 5: Run the Go gate.** Expected: PASS. Tests that asserted platform behaviour are removed or rewritten; list each in the report.

- [ ] **Step 6: Scan and commit:** `feat(dashboard): access comes from teams, not the platform`.

---

### Task 3: OIDC sign-in and the allowlist

**Files:**
- Create: `internal/dashboard/signin.go`, `internal/dashboard/signin_test.go`
- Modify: `internal/config/config.go`, `internal/dashboard/auth.go`, `dashboard.go`, `api_operator.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: Task 1's settings and `SignInSSOAccount`, Task 2's `load`.
- Produces: `type signIn struct` with `allowed(email string, claims map[string]any, added bool) bool`; routes `GET /auth/oidc/login`, `GET /auth/oidc/callback`; `GET`/`PUT /api/admin/sign-in`, `POST /api/admin/sign-in/test`; config `dashboard.sign_in` with `mode`, `label`, `issuer`, `client_id`, `client_secret_env`, `header`, `audience`, `jwks_url`, `email_claim`, `allowed_domains`, `allowed_emails`, which wins over the dashboard and makes it read-only. Old keys `oauth_client_id_env` and `oauth_client_secret_env` warn and are ignored.

SSO sessions end after 8 hours idle or 24 hours; local admins keep today's limits. Use go-oidc's verifier with `SupportedSigningAlgs` set to asymmetric algorithms only, PKCE and a nonce in the state cookie.

- [ ] **Step 1: Write the failing tests,** against a fake issuer with its own keys.
  - `TestOIDCSignIn`, `TestOIDCRefusesWrongAudience`, `TestOIDCRefusesExpired`, `TestOIDCRefusesWrongNonce`, `TestOIDCRefusesHS256SignedWithSecret`.
  - `TestOIDCRefusesUnverifiedEmail`, `TestOIDCRefusesMissingEmail`.
  - `TestAllowlist`, a table: exact email, domain, `a@evil.example.com` against `example.com`, `a@example.com@evil.com`, empty lists with and without `added`.
  - `TestGoogleNeedsHostedDomain`: issuer `https://accounts.google.com`, a domain rule, no `hd`: refused.
  - `TestRefusedCreatesNoAccount`: the message is `Ask an admin to add you`.
  - `TestSSOSessionEndsAt24Hours`.
  - `TestIssuerChangeEndsSSOSessions` and `TestIssuerChangeNeedsSecretAgain` (Review Focus 5): `PUT` with a new issuer and no secret is 400; a local admin's session stays.
  - `TestSignInSettingsAreHttpsOnly`, `TestTestButtonSaysOnlyWhetherItWorked`, `TestClientSecretIsWriteOnly`.
  - `TestOldOAuthKeysWarn`: a config naming them loads.

- [ ] **Step 2: Run** `go test ./internal/dashboard ./internal/config -run 'OIDC|Allowlist|Google|Refused|SSO|Issuer|SignIn|Test|ClientSecret|OldOAuth'`. Expected: FAIL.

- [ ] **Step 3: Implement** as above.

- [ ] **Step 4: Update `README.md`:** a **Sign-in** section with Google Workspace steps, and the config reference.

- [ ] **Step 5: Run the Go gate.** Expected: PASS.

- [ ] **Step 6: Scan and commit:** `feat(dashboard): sign in with OIDC, behind an email allowlist`.

---

### Task 4: JWT validation

**Files:**
- Modify: `internal/dashboard/signin.go`, `auth.go`
- Test: `internal/dashboard/signin_jwt_test.go`

**Interfaces:**
- Consumes: Task 3's settings, verifier and `allowed`.
- Produces: `func (a *Auth) fromHeader(r *http.Request) (email string, status headerStatus)`, with `headerNone`, `headerValid`, `headerInvalid`, `headerUnavailable`.

The rules are in the spec's "JWT validation". `Auth.Require` applies them: a cookie session started by `jwt` needs a matching valid header on every request; `headerUnavailable` is 503 and ends nothing; a password session skips the check.

- [ ] **Step 1: Write the failing tests.**
  - `TestJWTStartsASessionOnGET`, `TestJWTReadsBearerAndBareToken`.
  - `TestJWTPostStartsNothing` (Review Focus 1): a valid header, no cookie, `POST`: refused, no session, no change.
  - `TestJWTWriteNeedsCSRF`.
  - `TestJWTEmailChangeEndsSession`, `TestJWTMissingHeaderEndsSession`.
  - `TestJWTRefusesNone`, `TestJWTRefusesHS256`, `TestJWTRefusesWrongAudience`, `TestJWTRefusesWrongIssuer`, `TestJWTNeedsIssuerAndAudience`.
  - `TestJWTKeyOutageIs503`: the session survives.
  - `TestPasswordSessionSkipsTheHeader`.
  - `TestSignOutShowsNoRestart`: `/signed-out` starts nothing, even with a valid header.
  - `TestModeOffEndsJWTSessions`.

- [ ] **Step 2: Run** `go test ./internal/dashboard -run 'JWT|PasswordSession|SignOut|ModeOff'`. Expected: FAIL.

- [ ] **Step 3: Implement** as above.

- [ ] **Step 4: Update `README.md`:** JWT mode, and what a proxy must leave out (`/admin/sign-in`, `/auth/password`).

- [ ] **Step 5: Run the Go gate.** Expected: PASS.

- [ ] **Step 6: Scan and commit:** `feat(dashboard): sign in with a validated JWT`.

---

### Task 5: What leaned on the platform

**Files:**
- Modify: `internal/dashboard/api_commands.go`, `api_write.go`, `api_skill_fetch.go`, `api_skills.go`, `api_read.go`, `view.go`, `README.md`
- Test: `internal/dashboard/api_commands_test.go`, `api_write_test.go`, `api_skill_fetch_test.go`

**Interfaces:**
- Consumes: Task 2's `Session`.
- Produces: `func dismissalName(se Session) string`, the part before `@` for an SSO account; `actorLabel` gives the full email for the audit log. `GET /api/repositories/available?instance=<name>` takes an instance.

- [ ] **Step 1: Write the failing tests.**
  - `TestMaintainerDismissesOwnBlocker`: no author check, no `msgAuthorUnknown`. `TestDismissStillRefusedOnMergedChange`.
  - `TestDismissedByShowsNoAddress`: `alice@example.com` gives `Dismissed by alice` on the thread and comment, and the audit log has the full email.
  - `TestAddRepositoryNeedsAdmin`, `TestAddRepositoryListsWhatTheBotReaches`, `TestAddRepositoryNeedsInstance`.
  - `TestInstallSkillNeedsAdmin`, `TestSourceRepositoryHiddenFromUsers`.

- [ ] **Step 2: Run** `go test ./internal/dashboard -run 'Dismiss|AddRepository|InstallSkill|SourceRepository'`. Expected: FAIL.

- [ ] **Step 3: Implement.** `mayDismissBlocking` and `buttonsFor` lose the author check and keep the state rule. `seesSourceRepo`, `sightOf`, `sightCache`, `withinCeiling` and `userCanSee` go. `dismisser_platform_id` stays 0 for SSO dismissals.

- [ ] **Step 4: Update `README.md`** in the same places.

- [ ] **Step 5: Run the Go gate.** Expected: PASS.

- [ ] **Step 6: Scan and commit:** `feat(dashboard): dismissals and repositories without a platform user`.

---

### Task 6: Two areas

**Files:**
- Modify: `internal/dashboard/dashboard.go`, `web/src/App.tsx`, `web/src/layout/Layout.tsx`, `web/src/pages/SignIn.tsx`, `web/src/pages/Waiting.tsx`, `web/src/pages/Overview.tsx`, `web/src/api.ts`
- Create: `web/src/pages/AdminSignIn.tsx`, `web/src/pages/SignedOut.tsx`
- Test: `internal/dashboard/routes_test.go`, `web/src/pages/SignIn.test.tsx`, `web/src/layout/Layout.test.tsx`

**Interfaces:**
- Consumes: Tasks 2 to 4.
- Produces: pages `/sign-in`, `/admin/sign-in`, `/signed-out`, and `/admin`, `/admin/users`, `/admin/teams`, `/admin/integrations`, `/admin/settings`, `/admin/audit`. Old paths redirect for admins. `meView` has `email`, `role`, `signIn`, and no `platform`, `instance` or `operator`.

Admin API routes register through one helper, so a route can't skip the check: a non-admin gets 404, with the same body as an unknown path.

- [ ] **Step 1: Write the failing tests.**
  - `TestEveryAdminRouteIsNotFoundForAUser` (Review Focus 2): walks the helper's registered routes and fails on any answering other than 404. A viewer, a maintainer and a user on no team.
  - `SignIn.test.tsx`: no password field; no link to `/admin/sign-in`; with no SSO it says `Sign-in isn't set up yet. Ask your admin.`
  - `AdminSignIn` test: the password form, and the SSO button when set.
  - `Layout.test.tsx`: a user gets Repositories, Reviews, Skills only; an admin gets the admin pages and a link across; the sidebar reads `<name> · admin` and no page says `operator`.

- [ ] **Step 2: Run** the Go test and `npx vitest run src/pages/SignIn.test.tsx`. Expected: FAIL.

- [ ] **Step 3: Implement** as above.

- [ ] **Step 4: Run the Go gate, then the web gate.** Expected: PASS.

- [ ] **Step 5: Scan and commit:** `feat(dashboard): separate sign-in and pages for admins and users`.

---

### Task 7: People, teams and sign-in pages

**Files:**
- Modify: `web/src/pages/Users.tsx`, `Teams.tsx`, `Integrations.tsx`, `RepositorySettings.tsx`, `SkillInstall.tsx`, `Repositories.tsx`, `internal/dashboard/api_org.go`, `openapi.yaml`, `charts/pergent-sh/values.yaml`, `charts/pergent-sh/README.md`, `README.md`, `CLAUDE.md`
- Test: matching `*.test.tsx`, `internal/dashboard/api_org_test.go`

**Interfaces:**
- Consumes: Tasks 1 to 6.
- Produces: `POST /api/admin/users` `{email}`; `PUT /api/admin/users/{id}/role` `{role}`; a repository's `GET`/`PUT .../teams` for the settings page.

- [ ] **Step 1: Write the failing tests.**
  - **[CI]** `TestAddUserByEmail`: shows as invited, a duplicate is 409, a bad email 400.
  - **[CI]** `TestAddedUserKeepsAccessOnFirstSignIn`.
  - **[CI]** `TestRepositoryTeamsMatchTheTeamsPage`.
  - `Users.test.tsx`: **Add user** and **Add local admin**; columns user, sign-in, role, teams, last sign-in; `invited`; with no sign-in set up it links to **Integrations**.
  - `Teams.test.tsx`: a member's role, default `viewer`; adding by email.
  - `Integrations.test.tsx`: the **Sign-in** section, the secret write-only, read-only when set by config, the OIDC and JWT fields.
  - `RepositorySettings` test: the **Teams** section for an admin, absent for a maintainer.

- [ ] **Step 2: Run** the page tests. Expected: FAIL.

- [ ] **Step 3: Implement** as above, and add every new route and field to `openapi.yaml`. Keep `TestOpenAPISchemasMatchGoTypes` green; request bodies aren't covered by it, so check them by hand.

- [ ] **Step 4: Update `README.md`:** **Users** and **Teams**, and `### Upgrading to 0.11.0` at the top of Upgrading, with the spec's order: upgrade, sign in as a local admin, set up sign-in, add people. It says that every team loses its members and that rollback leaves new rows unused. Update `CLAUDE.md`, and remove `operator` from everything people read.

- [ ] **Step 5: Run the Go gate, then the web gate.** Expected: PASS.

- [ ] **Step 6: Scan and commit:** `feat(dashboard): add people, teams and sign-in from the admin pages`.

---

## Finish

Final review on Opus across `origin/main..HEAD`, including screenshots of `/sign-in`, `/admin/sign-in`, **Users** and **Integrations** with the real stylesheet: jsdom can't see layout. One fix wave. List every ruling in the final message. Merge the latest `main`, then ask the owner before pushing (H3). No release.
