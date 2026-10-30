# pergent.sh Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** a web dashboard, served by the `pergent-sh` binary, where GitLab and GitHub users sign in with their own accounts:
- they see the reviews of the repositories they can read;
- admins switch repositories on and off and change per-repository settings;
- review logs and setup problems are visible.

**Architecture:**
- **Settings in one place.** A new `internal/settings` resolver merges config-file repositories, dashboard repositories (Postgres) and defaults into one "effective" view. The engine and webhooks use it instead of reading `config.Repositories` directly.
- **API.** A JSON API under `/api/`, session auth via GitLab/GitHub OAuth, and server-side visibility filtering, all live in `internal/dashboard`.
- **Frontend.** A React + TypeScript (Vite) app is built into `internal/web/dist` and embedded with `go:embed`.
- **Routing.** The dashboard has its own address (`dashboard.url`) and its own optional Ingress.

**Tech Stack:**
- Backend: Go 1.24, pgx/v5, existing packages.
- Frontend: Node 22 LTS, Vite 5, React 18, TypeScript 5, `react-router-dom` 6, `@tanstack/react-query` 5, plain CSS (tokens from the mockups), Vitest + Testing Library, ESLint. No component library.

**Spec:** `docs/superpowers/specs/2026-10-31-dashboard-design.md`. The mockups are in `docs/superpowers/specs/2026-10-31-dashboard-mockups/`.

## Global Constraints

- **Visibility:** a user sees a repository only if they can read its code and MRs/PRs on the platform.
  - GitLab: access level ≥ Reporter (20), or a public project.
  - GitHub: `permissions.pull == true`, or a public repo.
  - Checked on the server for every API response.
- **One platform per sign-in.** The session holds one `(platform, instance, user)`.
- **Admin** = member of the configured group (GitLab) or team `org/slug` (GitHub) on that same instance. Every write checks admin AND visibility of the target repository.
- **Secrets stay server-side.** No token, key or secret in any API response, page, log view or browser storage. The user's OAuth token is stored sealed with `PERGENT_SH_SECRET_KEY` and never sent to the browser.
- **Config wins over the dashboard.**
  - A config-managed repository is read-only in the dashboard; writes to it return 409.
  - When a repository appears in config, its dashboard record is deleted by the next reconcile.
  - Removing a repository from config switches it off.
- **Turning "approve clean reviews" off** withdraws any approval the bot already made in that repository. The comment note reads exactly: `**Not approved:** approval is turned off for this repository.`
- **"Review drafts" off** means no review while draft; marking it ready starts a review.
- **Logs:** keep the last **200 KB** (204800 bytes) of the pergent container's log, redacted, deleted after **30 days**.
- **Sessions:**
  - cookie `pergent_session`: HttpOnly, Secure, SameSite=Lax, random 32-byte ID;
  - idle expiry **8 h**, hard delete after **7 days**;
  - the visible-repository list and admin flag are refreshed when older than **5 min**.
- **API calls to GitLab/GitHub** are allowed only for sign-in, access checks, the "add repository" picker and Skills & instructions. Use a 10 s timeout, and never call from the engine loop.
- **Skills & instructions** shows the default branch, cached **5 min**.
- **Writes** require header `X-CSRF-Token` matching the session's CSRF token.
- **The engine must not wait on the dashboard.** API handlers never hold engine locks; a dashboard error never fails a review.
- **Public repo hygiene:** example names only (`gitlab.example.com`, `pergent.example.com`, `acme/widgets`, `acme/payments`). The private-name scan stays clean.
- **No new Go dependencies.** OAuth is done with `net/http`.
- **Frontend:** `web/package-lock.json` is committed; Node version pinned in `web/.nvmrc` (`22`) and in CI.

## Review Focus

1. **A user whose GitLab access drops from Developer to Guest mid-session** must stop seeing that repository's reviews within 5 minutes. Owner: Task 6, `TestAccessRefreshDropsRepository`.
2. **A developer calling a write endpoint directly** (curl, with a valid session and CSRF token) gets 403 and nothing changes. Owner: Task 9, `TestWritesRequireAdmin`.
3. **An admin calling a write endpoint for a repository they can't see** gets 404, even though they are admin. Owner: Task 9, `TestAdminCannotWriteInvisibleRepo`.
4. **A review whose log contains the LLM key or git token.** The stored and served log has them replaced by `[REDACTED]`. Owner: Task 4, `TestRunLogRedacted`.
5. **Switching approval off while the bot's approval stands.** The approval is withdrawn by the next cleanup pass, and no new approval is made. Owner: Task 2, `TestApprovalOffWithdrawsApproval`.

---

## File Structure

```
internal/settings/settings.go        Effective repository settings: config ∪ dashboard ∪ defaults
internal/store/migrations/0007_dashboard.sql   dashboard_repos, run_logs, repo_attention, sessions, user_access
internal/store/dashboard.go          store methods for the new tables
internal/engine/*                    use settings.Resolver; approval-off; drafts-off; run logs; attention
internal/kube/launcher.go            Logs(ctx, jobName, limit) via pods/log
internal/dashboard/
  auth.go        sessions, cookie, CSRF, middleware
  gitlab.go      GitLab OAuth + access/admin checks
  github.go      GitHub OAuth (review App user auth) + access/admin checks
  access.go      visible-repo cache refresh
  api_read.go    read endpoints
  api_write.go   write endpoints
  view.go        JSON view types (the API contract)
internal/web/embed.go                go:embed dist + SPA handler (index.html fallback)
web/                                 Vite React TS app (src/, package.json, vite.config.ts, .nvmrc)
internal/server/server.go            mounts /api/, /auth/, / when dashboard is enabled
cmd/pergent-sh/main.go               wiring
charts/pergent-sh/                   dashboardIngress, Role pods/log, values, README (helm-docs)
.github/workflows/ci.yml, Dockerfile, README.md
```

---

### Task 1: Effective repository settings

**Files:**
- Create: `internal/settings/settings.go`, `internal/settings/settings_test.go`, `internal/store/migrations/0007_dashboard.sql` (all dashboard tables, to keep migrations together), `internal/store/dashboard.go`, `internal/store/dashboard_test.go`
- Modify: `internal/config/config.go` (+ test): new per-repository keys
- Modify: `internal/engine/*.go`: switch from `e.Config.Repositories` / `e.repository()` to the resolver; `internal/webhook/{gitlab,github}.go`: repository lookup through the resolver

**Config keys added to `config.Repository`** (yaml, all optional):
- `approve` (*bool, default true)
- `review_drafts` (*bool, default true)
- `job_timeout` (duration, default `jobs.timeout`)
- `job_memory` (quantity, default `jobs.resources.limits.memory`)
- `skills` and `failure_status` already exist.

**Migration 0007 creates:**
- `dashboard_repos`:
  - columns `instance`, `project`, `approve bool null`, `review_drafts bool null`, `failure_status text null`, `skills text[] null`, `job_timeout_seconds int null`, `job_memory text null`, `created_by text`, `created_at`, `updated_at`;
  - PK `(instance, lower(project))`;
  - a null column means "use the default".
- `run_logs`: `run_id uuid PK references runs on delete cascade`, `text text`, `created_at`.
- `repo_attention`: `instance`, `project`, `kind text`, `message text`, `updated_at`; PK `(instance, lower(project), kind)`.
- `sessions`: `id_hash bytea PK`, `platform`, `instance`, `user_id bigint`, `username`, `is_admin bool`, `token_sealed bytea`, `csrf text`, `access_checked_at`, `last_seen_at`, `created_at`.
- `user_access`: `session_hash bytea references sessions on delete cascade`, `instance`, `project`; PK all three.
- `runs` gains `title text not null default ''` and `web_url text not null default ''` (filled in Task 7).

**Interfaces (produced):**

```go
package settings
type Source string // "config" | "dashboard"
type Effective struct {
    Platform string // config.PlatformGitLab | config.PlatformGitHub
    Instance, Project string
    Source Source
    Approve, ReviewDrafts bool
    FailureStatus config.FailureStatus
    Skills []string                  // nil = all library skills
    JobTimeout time.Duration
    JobMemory resource.Quantity
    Overridden map[string]bool       // keys: "approve","review_drafts","failure_status","skills","job_timeout","job_memory"
}
type Resolver struct{ Config *config.Config; Store *store.Store }
func (r *Resolver) List(ctx context.Context) ([]Effective, error)     // config repos first, then dashboard repos not in config
func (r *Resolver) Get(ctx context.Context, instance, project string) (Effective, bool, error) // case-insensitive project
```

**Store methods:**
- `DashboardRepos(ctx) ([]DashboardRepo, error)`
- `DashboardRepo(ctx, instance, project) (DashboardRepo, bool, error)`
- `UpsertDashboardRepo(ctx, DashboardRepo) error`
- `DeleteDashboardRepo(ctx, instance, project) error`

`DashboardRepo` mirrors the table, with pointer fields for nullable columns.

**Engine changes:**
- **Reconcile** iterates `Resolver.List`.
- **A dashboard repo that is also in config** gets `DeleteDashboardRepo` and an Info log.
- **Dispatch** takes the timeout and memory limit from `Effective`, overriding `Resources.Limits.memory` only.
- **Publish** takes `FailureStatus` from `Effective`.
- **Inputs** take `Skills` from `Effective`.
- **The webhooks** accept a dashboard repository the same as a config one.

- [ ] **Step 1: Failing tests.**
  - `TestResolverConfigWins`: config `acme/widgets` plus a dashboard row for `ACME/widgets` → one Effective, Source config.
  - `TestResolverDefaultsAndOverrides`: a dashboard row with only `approve=false` → Approve false, `Overridden["approve"]`, every other field the default, not overridden.
  - `TestResolverConfigKeys`: config `approve: false, review_drafts: false, job_timeout: 20m, job_memory: 6Gi` reflected.
  - Store CRUD test for `dashboard_repos`.
  - `TestReconcileDropsDashboardRowOfConfigRepo`.
  - `TestDispatchUsesEffectiveTimeoutAndMemory`.
  - `TestWebhookAcceptsDashboardRepo`, for GitLab and GitHub.
- [ ] **Step 2:** `go test ./internal/settings/ ./internal/store/ ./internal/engine/ ./internal/webhook/` with the DB must FAIL.
- [ ] **Step 3:** Implement. Existing engine and webhook tests keep passing.
- [ ] **Step 4:** `go vet ./... && go test -count=1 ./...` (DB set and unset) must PASS.
- [ ] **Step 5: Commit:** `feat: effective repository settings from config and the dashboard`

---

### Task 2: Approve clean reviews: off

**Files:** `internal/policy/policy.go` (+ test), `internal/engine/publish.go`, `internal/engine/approvals.go`, `internal/store/runs.go` (+ tests), README.

**Changes:**
- `policy.Outcome` gains `ApprovalOff bool`.
- When `Decide` would otherwise approve and `ApprovalOff` is true:
  - no approval;
  - description `No blocking issues; not approved: approval is turned off`;
  - note exactly `**Not approved:** approval is turned off for this repository.`, placed like the other notes.
- Publish sets `ApprovalOff = !Effective.Approve`.
- Cleanup: `StaleApprovals` also returns approvals recorded for MRs in repositories whose effective Approve is false. Implement in the engine: filter recorded approvals by `Resolver.Get`, with the existing backoff. They are withdrawn through the existing unapprove path.

- [ ] **Step 1: Failing tests.**
  - `TestDecideApprovalOff`: clean review, `ApprovalOff` → no approve, exact note and description, status success.
  - `TestApprovalOffWithdrawsApproval` (engine, both platforms):
    1. an approved MR is recorded;
    2. the dashboard sets approve=false;
    3. CleanupOnce → Unapprove called, record cleared;
    4. the next publish of a clean review does not approve.
- [ ] **Step 2:** must FAIL. **Step 3:** Implement. **Step 4:** full suite must PASS.
- [ ] **Step 5: Commit:** `feat: a repository can turn approval off; existing approvals are withdrawn`

---

### Task 3: Review drafts: off

**Files:** `internal/webhook/{gitlab,github}.go` (+ tests), `internal/engine/dispatch.go` (+ test), `internal/store/runs.go` if needed.

**Changes:**
- **Webhook, MR is a draft** (GitLab `object_attributes.draft` or `work_in_progress`; GitHub `pull_request.draft`) and effective `ReviewDrafts` is false: don't enqueue. Reply 200 and log `outcome=draft_skipped`. Close and merge are still handled.
- **Draft marked ready** (GitLab `changes.draft` true→false; GitHub `ready_for_review`): call `Republish`. If it returns false, `EnqueueRun` for the head.
- **Dispatcher:** if `GetChange` says draft and `ReviewDrafts` is false, supersede the run without posting anything. This covers a race with the webhook.

- [ ] **Step 1: Failing tests.**
  - GitLab: draft opened with drafts off → no enqueue; draft→ready with no published run → `EnqueueRun`.
  - GitHub: the same two cases.
  - Dispatcher: a draft found at dispatch is superseded, with no Job and no status.
- [ ] **Step 2:** must FAIL. **Step 3:** Implement. **Step 4:** full suite must PASS.
- [ ] **Step 5: Commit:** `feat: a repository can skip drafts until they are ready`

---

### Task 4: Keep review logs

**Files:** `internal/kube/launcher.go` (+ test), `internal/engine/watch.go` (+ test), `internal/engine/engine.go` (pruning), `internal/store/dashboard.go` (+ test), `charts/pergent-sh/templates/rbac.yaml`.

**Interfaces:**
- `func (l *Launcher) Logs(ctx context.Context, jobName string, keepBytes int) (string, error)` reads the `pergent` container log of the Job's pod with `PodLogOptions{Container: "pergent", TailLines: 20000}`, then keeps the last `keepBytes` (204800) bytes, cut on a UTF-8 boundary.
- `store.PutRunLog(ctx, runID, text string) error`
- `store.RunLog(ctx, runID) (string, bool, error)`
- `store.PruneRunLogs(ctx, before time.Time) (int, error)`

**Behaviour:**
- The watcher, when a run's Job is Finished or the run is reported and its Job still exists, fetches the log once and redacts it with the engine's existing redaction (LLM key, git and clone tokens, result token).
- It stores the log keyed by run ID. A fetch failure is logged at Info and never affects the run.
- `Engine.Run` prunes logs older than 30 days hourly, next to reconcile.
- The chart Role gains `pods/log: get`.

- [ ] **Step 1: Failing tests.**
  - `TestLaunchLogsTail`: a log larger than 200 KB keeps the last 204800 bytes. Use the fake clientset's log stream, or an injectable log reader if the fake can't stream; your call.
  - `TestRunLogRedacted`: a log containing the LLM key and the clone token is stored with `[REDACTED]`.
  - `TestPruneRunLogs`.
  - `TestWatchStoresLogOnce`.
- [ ] **Step 2:** must FAIL. **Step 3:** Implement. **Step 4:** full suite and `helm template` must PASS.
- [ ] **Step 5: Commit:** `feat: keep the last 200 KB of each review's log for 30 days`

---

### Task 5: Needs-attention items

**Files:** `internal/store/dashboard.go` (+ test), `internal/engine/{reconcile,watch,approver usage}.go` (+ tests).

**Interfaces:**
- `store.SetAttention(ctx, instance, project, kind, message string) error`
- `store.ClearAttention(ctx, instance, project, kind string) error`
- `store.Attention(ctx) ([]Attention, error)`

**Kinds and exact messages:**
- `single_approval`: `One approval is enough to merge here, so pergent's approval alone can unlock it. Add an approval rule that needs a human.` Set and cleared where today's Warn is logged.
- `approver_unreachable`:
  - `The pergent approve App isn't installed on this repository, so nothing is approved.`
  - or, for the token approver: `The pergent approve bot can't reach this repository, so nothing is approved.`
- `out_of_memory`: `Reviews ran out of memory N times in the last 7 days. Raise the Job memory limit.` Recomputed by the watcher from runs whose failure reason contains `OOMKilled`, with N ≥ 2. Cleared when N < 2.
- `token_expiry`: `The approve bot's token expires on <date>.` Instance-wide: stored with project `""`.

- [ ] **Step 1: Failing tests:** set and cleared for each kind at the place that already detects it.
- [ ] **Step 2:** must FAIL. **Step 3:** Implement. **Step 4:** full suite must PASS.
- [ ] **Step 5: Commit:** `feat: record setup problems for the dashboard`

---

### Task 6: Sessions, sign-in and access (backend)

**Files:** Create `internal/dashboard/{auth.go,gitlab.go,github.go,access.go}` + tests. Modify `internal/config/config.go` (+ test), `internal/gitlab/client.go` (+ test) and `internal/github/client.go` (+ test) for the user-token calls below.

**Config:**
- `dashboard:`
  - `url` (https, required when the dashboard is enabled);
  - `admins` (list of `{instance, group}` for GitLab or `{instance, team: "org/slug"}` for GitHub).
- Per GitLab instance and per GitHub instance: `oauth_client_id_env` and `oauth_client_secret_env`.
- The dashboard is enabled when `dashboard.url` is set.
- No admins → startup Warn `dashboard has no admins configured`.

**Routes:**
- `GET /auth/{platform}/{instance}/login` redirects to the provider:
  - GitLab: `/oauth/authorize`, scope `read_user read_api`;
  - GitHub: `https://github.com/login/oauth/authorize` with the review App's client ID;
  - `state` is a random value in a short-lived signed cookie.
- `GET /auth/{platform}/{instance}/callback` exchanges the code, creates the session, refreshes access and redirects to `/`.
- `POST /auth/logout` deletes the session.
- `GET /api/me` returns `{platform, instance, username, isAdmin, csrfToken}`.

**Access refresh:** for each effective repository on the session's instance, check the user's access with the user token:
- GitLab:
  - `GET /projects/:path`;
  - visible if `visibility=="public"`, or `permissions.project_access.access_level ≥ 20`, or `permissions.group_access.access_level ≥ 20`.
  - Admin if `GET /groups/:group/members/all/:user_id` gives 200.
- GitHub:
  - `GET /repos/{o}/{r}` with the user token: visible if `!private`, or `permissions.pull`.
  - Admin if `GET /orgs/{org}/teams/{slug}/memberships/{username}` gives `state: active`.
- Store the results in `user_access` and `sessions.is_admin`, and set `access_checked_at`.
- Refresh on sign-in and when older than 5 min, on the next API request.
- A failed refresh keeps the old list for at most one more period, then denies everything (fail closed).

**Middleware:** `func (a *Auth) Require(next http.Handler) http.Handler`
- loads the session from the cookie (SHA-256 of the ID);
- idle > 8 h → 401;
- updates `last_seen_at` at most once a minute;
- enforces `X-CSRF-Token` on non-GET requests;
- puts `Session{Platform, Instance, UserID, Username, IsAdmin, Visible map[string]bool}` in the context.

`func (a *Auth) PruneSessions(ctx, now)` deletes sessions older than 7 days. It is run hourly from `Engine.Run`.

- [ ] **Step 1: Failing tests,** with httptest fake OAuth providers and APIs for both platforms:
  - `TestGitLabLoginCreatesSession`: state checked, token sealed, never in a response.
  - `TestGitHubLoginCreatesSession`.
  - `TestStateMismatchRejected`.
  - `TestVisibilityRules`, a table covering GitLab Guest, Reporter and public; GitHub private without pull, with pull, and public.
  - `TestAdminByGroupAndTeam`.
  - `TestAccessRefreshDropsRepository` (Review Focus 1).
  - `TestRefreshFailureFailsClosedAfterOnePeriod`.
  - `TestIdleSessionExpires`.
  - `TestCSRFRequiredOnWrites`.
  - `TestNoTokenInAnyResponse`: scans every response body and header for the fake token.
- [ ] **Step 2:** must FAIL. **Step 3:** Implement with `net/http`, no OAuth library. **Step 4:** full suite must PASS.
- [ ] **Step 5: Commit:** `feat: dashboard sign-in with GitLab and GitHub, sessions and access checks`

---

### Task 7: Read API

**Files:** Create `internal/dashboard/{view.go,api_read.go}` + tests.

**Endpoints** (all behind `Require`, all filtered by `Session.Visible`, all JSON). The API contract lives in `view.go`; field names are camelCase.
- `GET /api/overview?days=7`:
  - `{stats:{reviews, approved, blocking, failed}, recent:[ReviewSummary ≤10], attention:[Attention]}`
  - `ReviewSummary{instance, project, platform, number, title, outcome:"approved"|"running"|"blocking"|"draft"|"failed"|"no_blocking"|"superseded", reason, updatedAt, webURL}`
  - `title` and `webURL`: stored on the run at dispatch in the `runs.title`/`runs.web_url` columns from migration 0007, filled from `GetChange`; extend `forge.Change` with `Title` and `WebURL`.
- `GET /api/reviews?project=&outcome=&days=&cursor=`: `{items:[ReviewSummary], nextCursor}`.
- `GET /api/reviews/{instance}/{project...}/{number}`:
  - `{header:{title, sourceBranch, targetBranch, author?, webURL}, runs:[RunSummary]}`
  - `RunSummary{id, head, trigger, outcome, reason, createdAt, durationSeconds}`
- `GET /api/runs/{id}`:
  - `{run:RunSummary, status, approved, approvalReason, pergentVersion, model, tokens:{in,out}, files:{reviewed[], skipped[{path,rule}], notReviewed[]}, skills:[{name, verdict, source:"library"|"repo", findings:[{id, severity, title, file, line, explanation, suggestedFix}], checks:[...]}]}`
  - built by parsing the stored result with `internal/result`;
  - a run without a result returns its failure reason.
- `GET /api/runs/{id}/log`: `{text}` or 404.
- `GET /api/repositories`: `[{instance, project, platform, source:"config"|"dashboard", on:true, last7:{reviews, approved, failed}, health:"ok"|"attention"}]`
- `GET /api/repositories/{instance}/{project...}`: `{repo, settings:{field:{value, overridden}}, attention:[...]}`
- `GET /api/repositories/{instance}/{project...}/inputs`:
  - `{skills:[{name, source, content}], instructions}`
  - resolved at the default branch through `inputs.Resolve` with the review bot, cached 5 min per repository.
- `GET /api/skills`: library skills `[{name, content}]`.
- Admin only (403 otherwise):
  - `GET /api/integrations`: the session's instance only, `{instance, platform, connected, approver:"app"|"token", tokenExpiry?, repos:[{project, installed, approverReachable}]}`. Never any secret.
  - `GET /api/admin/settings`: resolved defaults (Job timeout and memory, failure status, approve, review drafts, LLM provider and model) and the admin groups and teams. Never keys.

- [ ] **Step 1: Failing tests** against the real store and fake platforms:
  - one test per endpoint, asserting shape and filtering;
  - `TestReadAPIHidesInvisibleRepos`: a repository not in `Visible` never appears in any list, and its detail endpoints give 404;
  - `TestAdminEndpointsForbiddenToDevelopers`;
  - `TestNoSecretInIntegrations`.
- [ ] **Step 2:** must FAIL. **Step 3:** Implement. **Step 4:** full suite must PASS.
- [ ] **Step 5: Commit:** `feat: dashboard read API`

---

### Task 8: Serve the frontend from the binary (scaffold)

**Files:**
- Create `web/` (Vite React TS): `package.json`, `package-lock.json`, `.nvmrc` = `22`, `vite.config.ts`, `tsconfig.json`, `eslint.config.js`, `index.html`, `src/main.tsx`, `src/App.tsx` (shell with the left menu from the mockups and a placeholder page).
- Create `internal/web/embed.go` (+ test). `internal/web/dist/` is generated and gitignored except a committed `dist/.keep` plus a fallback `index.html` that says "dashboard not built", so `go build` works without Node.
- Modify `internal/server/server.go` (+ test), `Makefile` (`web` target: `cd web && npm ci && npm run build`, output to `../internal/web/dist`; `build` depends on nothing new), `Dockerfile` (a `node:22-bookworm-slim` stage builds `web`, then the Go stage copies `dist`), `.github/workflows/ci.yml` (a `web` job: setup-node 22, `npm ci`, `npm run typecheck`, `npm run lint`, `npm test -- --run`, `npm run build`; the Go test job runs `make web` first).

**Interfaces:**
- `func Handler() http.Handler` in `internal/web`: serves embedded files, and `index.html` for unknown paths outside `/api/`, `/auth/`, `/webhooks/`, `/runs/`, `/healthz`. Cache headers: hashed assets immutable, `index.html` no-cache.
- `server.Routes(Deps)` mounts `/auth/`, `/api/` and `/` only when `Config.Dashboard.URL` is set. Webhook and callback routes are unchanged.

- [ ] **Step 1: Failing tests:**
  - `TestSPAFallback`;
  - `TestDashboardRoutesOffWhenNotConfigured`, where `/` gives 404 and webhooks still work;
  - `TestAssetCacheHeaders`.
- [ ] **Step 2:** must FAIL. **Step 3:** Implement. Check that `make web && make build` produces a binary that serves the shell.
- [ ] **Step 4:** Go suite, `npm run typecheck && npm run lint && npm test -- --run && npm run build` and `docker build .` must PASS.
- [ ] **Step 5: Commit:** `feat: build the dashboard with Vite and embed it in the binary`

---

### Task 9: Write API

**Files:** Create `internal/dashboard/api_write.go` + tests. Modify `internal/engine` if a Wake or Reconcile trigger is needed.

**Endpoints** (admin + visibility + CSRF):
- `GET /api/repositories/available`: repositories on the admin's instance that both the admin can see (user token) and the review bot or App can reach, minus those already on.
  - GitLab: the user's projects with `min_access_level=20` ∩ the review bot's projects with `min_access_level=40`.
  - GitHub: the review App installation's repositories ∩ the user's visible repositories.
- `POST /api/repositories` `{project}`:
  - refused with 409 if the repository is config-managed or already on;
  - creates a `dashboard_repos` row with all fields null, then wakes reconcile through a new `Engine.ReconcileSoon()`, a non-blocking channel the loop drains;
  - responds 201.
- `DELETE /api/repositories/{instance}/{project...}`: 409 if config-managed; otherwise deletes the row and calls `ReconcileSoon()`. The existing removal path deletes the hook, token and project row.
- `PUT /api/repositories/{instance}/{project...}/settings` with partial `{approve?, reviewDrafts?, failureStatus?, skills?, jobTimeout?, jobMemory?}`. Values are validated like config: failure status in {failed, neutral}; skills exist in the library; timeout 1 m–2 h; memory 256Mi–32Gi. A `null` value resets that field. 409 if config-managed.
- `POST /api/repositories/{instance}/{project...}/settings/reset`: all fields null.

`ReconcileSoon` must not block the HTTP handler. The engine loop picks it up on its next select.

- [ ] **Step 1: Failing tests:**
  - `TestWritesRequireAdmin` (Review Focus 2);
  - `TestAdminCannotWriteInvisibleRepo` (Review Focus 3);
  - `TestConfigManagedIs409`;
  - `TestSettingsValidation`, one row per field;
  - `TestSwitchOnTriggersReconcile`;
  - `TestAvailableIsIntersection`, for both platforms.
- [ ] **Step 2:** must FAIL. **Step 3:** Implement. **Step 4:** full suite must PASS.
- [ ] **Step 5: Commit:** `feat: dashboard write API for repositories and settings`

---

### Task 10: Frontend pages

**Files:**
- `web/src/` with:
  - `api.ts`: typed client for the endpoints in Tasks 6, 7 and 9, adding `X-CSRF-Token` from `/api/me` on writes;
  - `theme.css`: the color tokens from the mockups;
  - `layout/`;
  - `pages/` for Overview, Reviews, ReviewDetail, Repositories, RepositoryDetail (tabs Reviews, Settings, Skills & instructions, Setup checks), Integrations, AdminSettings, Skills and SignIn;
  - tests next to each page.

**Rules:**
- Match the three mockups in `docs/superpowers/specs/2026-10-31-dashboard-mockups/`: left dark menu, pill colors, cards.
- The Admin menu section shows only when `isAdmin`.
- Settings controls are disabled with a "managed by config" label when `source == "config"`, and each control shows an "overridden here" or "default" badge.
- **Sign-in page:** one button per configured instance, from a public `GET /api/instances` that returns `[{platform, instance, label}]` (add it in this task, unauthenticated, no secrets).
- **401 from any call** → redirect to the sign-in page.
- **No action buttons yet** (re-run, dismiss); they come with the commands piece.
- **Nothing sensitive is stored in localStorage**, only UI preferences.

- [ ] **Step 1: Failing tests** (Vitest + Testing Library, `fetch` mocked):
  - each page renders its key data;
  - admin-only items are hidden for a developer;
  - a config-managed repository's settings are disabled;
  - saving sends `X-CSRF-Token`;
  - a 401 redirects to sign-in.
- [ ] **Step 2:** `npm test -- --run` must FAIL. **Step 3:** Implement. **Step 4:** `npm run typecheck && npm run lint && npm test -- --run && npm run build` must PASS. Visually check the built app against the mockups with the dev server and a fake API; screenshots in the report.
- [ ] **Step 5: Commit:** `feat: dashboard pages`

---

### Task 11: Chart, wiring, docs and end-to-end

**Files:**
- `cmd/pergent-sh/main.go` (+ test): build `settings.Resolver`, `dashboard.Auth`, pass them to the server and engine, and run hourly session and log pruning.
- `charts/pergent-sh/`:
  - `templates/dashboard-ingress.yaml`: `dashboardIngress.enabled: false` by default, with its own host, TLS and annotations; paths `/`, `/api/`, `/auth/` to the existing Service;
  - values with helm-docs comments;
  - `make readme`.
- `README.md`:
  - a "Dashboard" section: enabling it, `dashboard.url`, the separate Ingress, creating the GitLab OAuth application (redirect URI `<dashboard.url>/auth/gitlab/<instance>/callback`, scopes `read_user read_api`), enabling user authorization on the GitHub review App (callback `<dashboard.url>/auth/github/<instance>/callback`), admins, and what developers and admins can do;
  - add **Organization: Members: read** to the review App permission list;
  - document the new per-repository config keys;
  - add the GitLab hardening line "turn on Prevent editing approval rules in merge requests" (carried over from the release review).
- `internal/engine/e2e_dashboard_test.go`: a fake GitLab with OAuth, the real store and real routes:
  1. sign in;
  2. `/api/overview` shows only visible repositories;
  3. an admin switches a repository on;
  4. reconcile creates the hook;
  5. a webhook → review → `/api/runs/{id}` shows findings, and the log is redacted;
  6. the admin sets approve=false;
  7. the approval is withdrawn;
  8. a developer's write is refused.

- [ ] **Step 1: Failing e2e test.**
- [ ] **Step 2:** Implement wiring, chart and docs.
- [ ] **Step 3:** Verify:
  - `go vet ./... && go test -count=1 ./...` (DB set and unset);
  - `make web && make build`;
  - `helm lint charts/pergent-sh -f charts/pergent-sh/ci-values.yaml` and `helm template` with `dashboardIngress.enabled=true`;
  - `docker build .`;
  - the private-name scan.
- [ ] **Step 4: Commit:** `feat: wire the dashboard; chart Ingress and docs`

## Manual smoke test (after Task 11, by the user)

1. Create the GitLab OAuth application, enable the dashboard Ingress, and sign in as an admin and as a developer.
2. The developer sees only repositories they can read.
3. The admin switches a repository on, and a new MR gets reviewed.
4. Turning approval off withdraws the bot's approval.
5. A review that ran out of memory shows under "Needs attention", and its log is viewable.
