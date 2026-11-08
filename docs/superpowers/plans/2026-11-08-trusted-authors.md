# Trusted Authors Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On repositories where **Trusted authors only** is on (public ones by default), a change from an author without write access and not on the repository's allowlist starts no review until someone with write access starts it.

**Architecture:** Webhooks stay as they are (no platform calls): they queue a `push` run. The dispatcher, which already reads the change (author included) before creating any Job, runs the trust check right after its draft and moved-head checks. An untrusted run is put in a new run state `waiting`, which no release ever claims, and the change gets one note and a pending status. Starting it (a writer's `@<bot> review`, or the dashboard's **Start review**) turns the waiting run back into a `queued` run with the new trigger `start`, which the dispatcher doesn't check again.

**Tech Stack:** Go (net/http, pgx, Postgres migrations), React + TanStack Query + vitest, Helm chart docs.

**Spec:** `docs/superpowers/specs/2026-11-08-trusted-authors-design.md`

## Global Constraints

- Never a false trust: any error, missing ID or unreadable value on the way to "trusted" is untrusted (visibility unreadable → public; access unreadable after the retry window → waits, no note).
- No Job, no LLM call and no clone token for a `push` run before the trust check passes.
- Public repository: anything containing internal company or host names must never appear in code, tests, docs or commit messages; run the repository's name scan before every commit.
- Never run `docker`, `orbctl` or anything that starts a container. Postgres tests skip locally without `PERGENT_SH_TEST_DATABASE_URL`; they run in CI.
- Migration number: `0031` unless `main` already has it when the work starts (another agent works in parallel); check `git ls-tree origin/main internal/store/migrations/` first and take the next free number. `TestMigrationNumbersAreUnique` must pass.
- The previous release (0.11.x) must keep working on the migrated database: it never claims a `waiting` run, and a new column is nullable or defaulted.
- Every new dashboard field or route goes in `openapi.yaml` (`openapi_test` enforces it).
- Exact copy (use verbatim):
  - note: ``Not reviewed automatically: the author isn't a trusted author of this repository. Someone with write access can start a review with `<mention> review`.``
  - pending status, untrusted: `Waiting for someone with write access to start the review`
  - pending status, check failed: `Couldn't check the author's access. Someone with write access can start the review with <mention> review`
  - command refusal reply: `only someone with write access can start the review of this change`
  - dashboard outcome text for `waiting`: `Waiting for someone with write access to start the review.`
- Mode values everywhere (config, store, API): `follow_visibility`, `on`, `off`. Repository override: `on`, `off` or unset.

## Rulings (decisions the spec left open, made here)

- **Where the check runs:** in the dispatcher after `GetChange`, not in a separate pre-queue state. The spec allows any storage "as long as it can never be dispatched before the check passes": the check runs before any Job, token or LLM step, and a held run moves to `waiting`, which `ClaimQueued` never selects. — Cost if wrong: a held run briefly takes a dispatch slot.
- **Allowlist in config:** entries are `{id: <number>, username: <name>}` with `id` required, because the config is loaded with no platform to resolve names. The README says how to find a user's ID. — Cost if wrong: a config format change later.
- **Dashboard allowlist write:** entries with an `id` are kept as sent; entries with only `username` are resolved. A renamed user's old entry is therefore never resolved again.
- **Dashboard Re-run vs Start:** the dashboard's **Start review** (action `start`) applies to a change whose newest run is `waiting`. **Re-run** keeps today's behaviour and caps. The command path picks start or re-run by trust.
- **Pending on GitHub:** a Check Run with status `queued` (no conclusion). On GitLab: commit status `pending`.

## Review Focus

1. A trusted author's run hit by a platform error during the check must still run after a retry, not wait.
2. A waiting run on an old head must be superseded by a new push, a close or a merge, like a queued run.
3. An untrusted draft with `review_drafts: false` is handled as a draft (superseded); no note, no pending status.
4. A GitHub bot author (`dependabot[bot]`): its access lookup is "none", so it waits unless it's on the allowlist. Resolving `dependabot[bot]` to an ID must work.
5. During a rolling upgrade, a 0.11.x pod sharing the database must never dispatch a `waiting` run.

---

### Task 1: Store: waiting runs, start trigger, trust columns

**Files:**
- Create: `internal/store/migrations/0031_trusted_authors.sql`
- Modify: `internal/store/runs.go`, `internal/store/dashboard.go`, `internal/store/instances.go`, `internal/store/dashboard_read.go`
- Test: `internal/store/trusted_test.go` (new), `internal/store/store_test.go`

**Interfaces:**
- Produces:
  - `const Waiting RunState = "waiting"`, `const TriggerStart = "start"`, `const OutcomeWaiting = "waiting"`
  - `const WaitUntrusted = "untrusted_author"`, `const WaitCheckFailed = "trust_check_failed"` (stored in `skip_reason` of a waiting run)
  - `func (s *Store) HoldRun(ctx context.Context, id, reason string, now time.Time) (bool, error)`: `queued`/`running` → `waiting`, sets `skip_reason`
  - `func (s *Store) StartRun(ctx context.Context, mr MRKey, projectPath, headSHA, targetBranch string) (id string, created bool, err error)`: a `waiting` run on that head+target becomes `queued` with trigger `start` (created=false, id of it); with none, a live run on that head+target → (its id, false); else inserts a `queued` `start` run, superseding live runs of other heads like `EnqueueRun`
  - `func (s *Store) ClaimUntrustedNote(ctx context.Context, mr MRKey, now time.Time) (bool, error)`: true exactly once per change; `func (s *Store) ReleaseUntrustedNote(ctx context.Context, mr MRKey) error`
  - `type TrustedAuthor struct { ID int64 `json:"id"`; Username string `json:"username"` }`
  - `DashboardRepo.TrustedAuthorsOnly *string`, `DashboardRepo.TrustedAuthors []TrustedAuthor`
  - `InstanceRow.TrustedAuthorsOnly string`; `func (s *Store) SetInstanceTrustMode(ctx context.Context, name, mode string, at time.Time) (bool, error)`

- [ ] **Step 1: Write the failing tests** in `internal/store/trusted_test.go`:
  - `TestHoldRunIsNeverClaimed`: enqueue, `HoldRun(id, WaitUntrusted)` → true; `ClaimQueued` returns none; run state `waiting`, `SkipReason == "untrusted_author"`.
  - `TestStartRunTurnsWaitingIntoQueuedStart`: hold, then `StartRun` same head → same id, created=false, state `queued`, trigger `start`.
  - `TestStartRunInsertsWhenNothingWaits`: published run on head → `StartRun` inserts a new `start` run.
  - `TestPushSupersedesWaitingRun`: hold on head A, `EnqueueRun` head B → run A `superseded`; `SupersedeMR` also supersedes a waiting run.
  - `TestStartDoesNotCountAgainstRepoCap`: two `start` runs → `CountReruns(&mr, …) == 2`, `CountReruns(nil, …) == 0`.
  - `TestClaimUntrustedNoteOnce`: true, then false; after `ReleaseUntrustedNote`, true again.
  - `TestTrustedAuthorsRoundTrip`: `UpdateDashboardRepo` sets `TrustedAuthorsOnly = ptr("on")` and two authors; `DashboardRepo` reads them back; instance mode defaults to `follow_visibility`; `SetInstanceTrustMode(..., "off")` reads back; `"bogus"` is refused by the CHECK.
  - `TestWaitingOutcome`: a waiting run's digest outcome is `waiting` and it is in `MRRunDigests`.
  - In `store_test.go`: `TestMigrationKeepsPreviousReleaseTrusted` with `newStoreAt(t, 30)`, migrate up, insert a `waiting` run, and run the 0.11.0 `ClaimQueued` SQL: nothing is claimed.

- [ ] **Step 2: Run them, expect failures**
  Run: `go test ./internal/store/ -run 'Hold|StartRun|Waiting|Untrusted|TrustedAuthors|PreviousReleaseTrusted|MigrationNumbers'` (with `PERGENT_SH_TEST_DATABASE_URL` in CI; locally they skip, so check that it compiles and `TestMigrationNumbersAreUnique` passes).

- [ ] **Step 3: Migration `0031_trusted_authors.sql`**
  - Replace `runs_state_check` with one that adds `'waiting'`. Find the trigger constraint's real name with `pg_constraint` in the migration (`DO $$ … $$`), drop it, and add `runs_trigger_check` with `'push','rerun','rerun_failed','start'`.
  - `dashboard_repos.trusted_authors_only text NULL CHECK (trusted_authors_only IN ('on','off'))`, `dashboard_repos.trusted_authors jsonb NULL`.
  - `instances.trusted_authors_only text NOT NULL DEFAULT 'follow_visibility' CHECK (trusted_authors_only IN ('follow_visibility','on','off'))`.
  - `merge_requests.untrusted_noted_at timestamptz NULL`.
  - Header comment in the style of 0029/0030: only adds columns and values; the previous release claims only `queued` runs.

- [ ] **Step 4: Implement the store functions**
  Add `waiting` to every state list that supersedes live runs (`EnqueueRun`, `EnqueueRerun`'s live check, `supersedeMR`). In `CountReruns`, the per-repository branch (`mr == nil`) excludes `start`, and the per-change branch keeps `trigger <> 'push'`. Add `WHEN 'waiting' THEN 'waiting'` in `outcomeSQL`. Store `TrustedAuthors` as jsonb, the same way as `Links` (`linksParam`/`readLinks`). Keep `dashboardRepoColumns`/`scanDashboardRepo`/`UpsertDashboardRepo`/`UpdateDashboardRepo` and `instanceColumns`/`scanInstance` in step. `ClaimUntrustedNote` upserts the `merge_requests` row and sets `untrusted_noted_at` only when it is NULL, returning whether it set it.

- [ ] **Step 5: Run the tests, expect a pass** (same command; `go vet ./internal/store/`).

- [ ] **Step 6: Commit** `feat(store): waiting runs, start trigger and trusted-author columns`

---

### Task 2: Config, instances and settings resolution

**Files:**
- Modify: `internal/config/config.go`, `internal/instances/instances.go`, `internal/settings/settings.go`
- Test: `internal/config/config_test.go`, `internal/settings/settings_test.go`, `internal/instances/instances_test.go`

**Interfaces:**
- Consumes: Task 1's `store.TrustedAuthor`, `DashboardRepo.TrustedAuthorsOnly/TrustedAuthors`, `InstanceRow.TrustedAuthorsOnly`.
- Produces:
  - `config.Instance.TrustedAuthorsOnly string` and `config.GitHubInstance.TrustedAuthorsOnly string`, yaml `trusted_authors_only` (empty means `follow_visibility`)
  - `config.Repository.TrustedAuthorsOnly *string`, `config.Repository.TrustedAuthors []store.TrustedAuthor`; file yaml `trusted_authors_only` (`on`/`off`), `trusted_authors` (list of `{id, username}`)
  - `instances.Instance.TrustedAuthorsOnly string` (always one of the three values; filled by `FromRow` and from config)
  - `settings.KeyTrustedAuthorsOnly = "trusted_authors_only"`, `settings.KeyTrustedAuthors = "trusted_authors"`
  - `settings.Effective.TrustedAuthorsOnly string` (the resolved mode: repo `on`/`off` if set, else the instance's), `settings.Effective.TrustedAuthors []store.TrustedAuthor`, `Overridden[KeyTrustedAuthorsOnly]` true only when the repository sets it

- [ ] **Step 1: Write the failing tests**
  - config: `TestTrustedAuthorsConfig` loads `gitlab: [{…, trusted_authors_only: off}]` and `repositories: [{…, trusted_authors_only: on, trusted_authors: [{id: 12, username: alice}]}]`. `TestTrustedAuthorsConfigInvalid` is table-driven: mode `maybe` → error naming the key; repository mode `follow_visibility` → error (a repository takes only `on`/`off`); author `id: 0` → error; author without `username` → error.
  - settings: `TestTrustedAuthorsOnlyResolution` is table-driven over instance mode × repository override (unset/on/off), for a config repository and a dashboard repository. Expected Effective mode: repo value if set, else the instance's. With no repo override, `Overridden[KeyTrustedAuthorsOnly]` is false.
  - instances: `FromRow` carries `TrustedAuthorsOnly`; a config instance with an empty value gets `follow_visibility`.

- [ ] **Step 2: Run, expect failures:** `go test ./internal/config/ ./internal/settings/ ./internal/instances/`

- [ ] **Step 3: Implement.** Validate in `Validate`/`checkGitHubInstance`. `defaultsFor` reads the instance with `r.registry().Get(instance)`; an unknown instance gives `follow_visibility`. `fromConfig`/`fromDashboard` apply the repository override and list. There is no `RepoDefaults` field (the spec says Admin → Settings has no default for this).

- [ ] **Step 4: Run, expect a pass;** then `go vet ./...`.

- [ ] **Step 5: Commit** `feat(settings): trusted authors mode per instance and repository, and the allowlist`

---

### Task 3: Forge: pending status, visibility on GitLab, user by name

**Files:**
- Modify: `internal/forge/forge.go`, `internal/forge/gitlabforge/gitlabforge.go`, `internal/forge/githubforge/githubforge.go`, `internal/gitlab/client.go`, `internal/gitlab/types.go`, `internal/github/client.go`
- Test: `internal/forge/gitlabforge/gitlabforge_test.go`, `internal/forge/githubforge/githubforge_test.go`, `internal/gitlab/client_test.go`, `internal/github/client_test.go`

**Interfaces:**
- Produces:
  - `forge.StatusPending StatusState = "pending"`
  - `type forge.User struct { ID int64; Username string }`; on `forge.Forge`: `UserByName(ctx context.Context, username string) (User, error)`, which returns `forge.ErrNotFound` when there is no such user (add `ErrNotFound` if the package has none; reuse it if it has)
  - `gitlab.StatusPending StatusState = "pending"`; `gitlab.Project.Visibility string` (`json:"visibility"`); `func (c *Client) UserByUsername(ctx context.Context, username string) (User, error)` (`GET /users?username=`, exact case-insensitive match in the answer, none → `ErrNotFound`)
  - `func (c *github.Client) UserByLogin(ctx context.Context, login string) (User, error)` (`GET /users/{login}`, path-escaped so `dependabot[bot]` works, 404 → `ErrNotFound`)
  - `gitlabforge.Visibility(ctx, repo)` reads `GetProject(repo).Visibility`: `public`/`internal`/`private` → the `forge.Vis*` value, anything else → an error

- [ ] **Step 1: Write the failing tests**
  - GitLab `SetStatus(StatusPending)` sends `state=pending`. GitHub `SetStatus(StatusPending)` creates or updates the Check Run with `status: "queued"` and no `conclusion`.
  - `gitlabforge.Visibility` maps the three values; an empty or unknown value is an error; a 404 is `ErrSourceNotFound` (as on GitHub).
  - `UserByName`: GitLab answers `[{"id":5,"username":"Alice"}]` for `alice` → `{5, "Alice"}`; `[]` → `ErrNotFound`; a 500 is an error that isn't `ErrNotFound`. GitHub `dependabot[bot]` requests `/users/dependabot%5Bbot%5D`; 404 → `ErrNotFound`.

- [ ] **Step 2: Run, expect failures:** `go test ./internal/forge/... ./internal/gitlab/ ./internal/github/`

- [ ] **Step 3: Implement.** Map `StatusPending` in `gitlabforge.State` and in GitHub's `SetStatus` before the `completed` branch. Add `UserByName` to every `forge.Forge` implementation and test fake (`go vet ./...` lists them).

- [ ] **Step 4: Run, expect a pass;** then `go vet ./...`.

- [ ] **Step 5: Commit** `feat(forge): pending status, GitLab visibility and user lookup by name`

---

### Task 4: Engine: the trust check in the dispatcher

**Files:**
- Create: `internal/engine/trust.go`
- Modify: `internal/engine/dispatch.go`
- Test: `internal/engine/trust_test.go` (new, Postgres), `internal/engine/e2e_trusted_test.go` (new, Postgres)

**Interfaces:**
- Consumes: Task 1 (`HoldRun`, `ClaimUntrustedNote`, `ReleaseUntrustedNote`, `Waiting`, `TriggerStart`, `WaitUntrusted`, `WaitCheckFailed`), Task 2 (`Effective.TrustedAuthorsOnly/TrustedAuthors`), Task 3 (`StatusPending`, `Visibility`, `AccessLevel`).
- Produces:
  - `type trust int` with `trustYes`, `trustNo`
  - `func authorTrust(ctx context.Context, f forge.Forge, repo settings.Effective, projectID int64, projectPath string, ch forge.Change) (trusted bool, err error)`. It's true when the mode resolves to off, when `ch.AuthorID` is on `repo.TrustedAuthors` (by ID, and only if non-zero), or when `AccessLevel(projectID, ch.Author, ch.AuthorID) >= forge.AccessWrite`. `follow_visibility` reads `f.Visibility(ctx, projectPath)`: public means on, internal or private means off, and an error means on (logged, never returned). A non-nil `err` is only an access-check error.
  - `const trustPatience = 10 * time.Minute` (a var, so tests can shorten it); `waitingDescription`, `checkFailedDescription(mention string) string`, `untrustedNote(mention string) string` with the exact copy from Global Constraints.

- [ ] **Step 1: Write the failing tests**
  - `trust_test.go`, table-driven `TestAuthorTrust`: mode off → true with no platform call; mode on with an author on the list → true with no access call; Developer → true; Reporter → false; `AuthorID == 0` → false; follow + private → true with no access call; follow + visibility error → access is checked (Reporter → false); an access error → err.
  - `e2e_trusted_test.go` on the fake GitLab, with an instance mode `on`:
    - `TestUntrustedRunWaits`: a Reporter author's MR hook → after `DispatchOnce` the run is `waiting`, there's one note with the exact text, the pending status has the waiting description, and no Job was created. A second push gives a new head that is waiting, a pending status, and still one note.
    - `TestTrustedRunGoesAhead`: a Developer author is dispatched as today.
    - `TestTrustCheckRetriesThenGivesUp`: the member endpoint answers 500. Before `trustPatience` the run is postponed; past it, the run is `waiting` with `trust_check_failed`, there's no note, and the pending status has the check-failed text. Separately, the endpoint fails once and then answers Developer: the run is dispatched.
    - `TestUntrustedDraftIsADraft`: a draft with `review_drafts: false` is superseded as a draft, with no note.
    - `TestStartRunSkipsCheck`: a waiting run, then `StartRun`, then `DispatchOnce` dispatches it with no access call.

- [ ] **Step 2: Run, expect failures:** `go test ./internal/engine/ -run 'Trust|Untrusted|StartRun'` (compile check locally; CI runs them).

- [ ] **Step 3: Implement.** In `dispatch`, right after the moved-head check and before the diff-refs wait, run the check only when `run.Trigger == store.TriggerPush`:
  - Trusted: carry on.
  - Access error, with `now - run.CreatedAt < trustPatience`: `postpone(now + dispatchBackoff<<min(Attempts,5))`. At or past the window: `HoldRun(WaitCheckFailed)`, set the pending status with `checkFailedDescription`, no note.
  - Untrusted: `HoldRun(WaitUntrusted)`, then `SetStatus(StatusPending, waitingDescription)`. If `ClaimUntrustedNote` is true, `Forge.Reply(untrustedNote)`. On a reply error, `ReleaseUntrustedNote` and log it (the run stays waiting; the next push tries the note again).

- [ ] **Step 4: Run, expect a pass;** then `go vet ./...`.

- [ ] **Step 5: Commit** `feat(engine): hold runs of untrusted authors until someone with write access starts them`

---

### Task 5: Commands and actions: starting an untrusted change

**Files:**
- Modify: `internal/actions/actions.go`, `internal/engine/commands.go`
- Test: `internal/actions/actions_test.go`, `internal/engine/e2e_commands_test.go`

**Interfaces:**
- Consumes: Task 1 `StartRun`, `TriggerStart`; Task 4 `authorTrust`.
- Produces:
  - `type StartRequest struct { MR store.MRKey; ProjectPath, Head, TargetBranch string; Repo settings.Effective; Now time.Time }`
  - `func Start(ctx context.Context, st *store.Store, req StartRequest) (RerunResult, error)`: checks only the per-change cap (`CountReruns(&mr) >= RerunLimitPerMR` → `RerunCapped`), then `StartRun`. `Action = "review_start"`, `Detail = "skills=all"`.
  - `whyUntrusted = "author not trusted"`, `replyStartNeedsWrite = "only someone with write access can start the review of this change"`

- [ ] **Step 1: Write the failing tests** in `e2e_commands_test.go` (instance mode `on`, `CommandsAuthorAndMaintainersOnly` on):
  - `TestStartByWriterPassesGate`: a waiting change; a Developer (not the author) says `@<bot> review` → the run becomes `start`, 👍, audit `review_start`.
  - `TestAuthorCannotStartOwnUntrusted`: the untrusted author says `@<bot> review` → 👎 and the reply text; no run changes; a second try within 2 minutes is throttled.
  - `TestDismissStillGatedForWriter`: a Developer's `dismiss` on someone else's change → refused as today.
  - `TestTrustedChangeRerunUnchanged`: a trusted change's `@<bot> review` goes through `actions.Rerun` (trigger `rerun`).
  - `actions_test.go`: `TestStartIgnoresRepoCap`, with the repo cap reached and the change cap not reached → queued. `TestStartChangeCap` → `RerunCapped`.

- [ ] **Step 2: Run, expect failures:** `go test ./internal/actions/ ./internal/engine/ -run 'Start|Untrusted|Dismiss|Rerun'`

- [ ] **Step 3: Implement.** In `handleCommand`, for `review`/`review_failed`, compute `authorTrust` for `c.change` (an error goes through the existing command retry). In `carryOut`, a `review`/`review_failed` on an untrusted change skips the author-and-maintainers gate. In `rerun`, an untrusted change needs `c.access >= forge.AccessWrite`, whether or not the user is the author (else `refused(whyUntrusted, replyStartNeedsWrite)`), and calls `actions.Start`. A trusted change keeps today's path. Add `whyUntrusted` to `whoMayOutcome` so it is audited as `command_refused`.

- [ ] **Step 4: Run, expect a pass;** then `go vet ./...`.

- [ ] **Step 5: Commit** `feat(commands): only write access starts an untrusted change's review`

---

### Task 6: Dashboard API

**Files:**
- Modify: `internal/dashboard/api_write.go`, `api_read.go`, `api_commands.go`, `api_operator.go`, `view.go`, `outcome.go`, `dashboard.go` (`RepoSource`), `openapi.yaml`
- Test: `internal/dashboard/trusted_test.go` (new), `internal/dashboard/openapi_test.go` (passes unchanged)

**Interfaces:**
- Consumes: Tasks 1–3 (`UserByName` through `RepoSource`), Task 5 `actions.Start`.
- Produces:
  - Repository settings keys `trustedAuthorsOnly` (`"on"|"off"|null`) and `trustedAuthors` (array of `{"id"?: number, "username": string}`). The settings view reports `trustedAuthorsOnly` as the resolved mode string (`follow_visibility|on|off`) with `overridden`, and `trustedAuthors` as `[{id, username}]`.
  - `RepoSource` gains `UserByName(ctx context.Context, username string) (forge.User, error)`.
  - `PUT /api/admin/instances/{name}/trusted-authors` with body `{"mode": "follow_visibility"|"on"|"off"}`: admin only, 409 `msgConfigInstance` for a config instance, 400 for a bad mode, 204 on success, audit `instance_updated`.
  - `InstanceAdminView.TrustedAuthorsOnly string` (`json:"trustedAuthorsOnly"`)
  - `RepositoryDetailView.Waiting []WaitingView` with `type WaitingView struct { Number int64 `json:"number"`; Title string `json:"title"`; Author string `json:"author"`; WebURL string `json:"webUrl"`; Since time.Time `json:"since"`; Reason string `json:"reason"` }` (reason is `untrusted_author` or `trust_check_failed`). Changes whose newest run is `waiting`; empty for a repository that is off.
  - Review action `start` on `POST /api/reviews/{instance}/{project…}/{number}/start` (RoleMaintainer). It is 409 unless the newest run of the change is `waiting`, and calls `actions.Start` on the change's current head: 202 `{"status":"queued"}`, or 429 at the change cap. Audit `review_start`.
  - Outcome `waiting` with the exact text from Global Constraints.

- [ ] **Step 1: Write the failing tests** in `trusted_test.go`:
  - Saving `trustedAuthors: [{"username":"alice"}, {"id": 9, "username":"old-name"}]`: alice is resolved through the fake source to ID 5 and the other entry is kept as sent. An unknown name → 400 naming it, and nothing is saved. A source error → 502.
  - `trustedAuthorsOnly: "maybe"` → 400. `null` resets it to the instance's mode. A config repository → 409.
  - Instance mode: an admin sets `off` → 204 and the view shows `off`. A non-admin → 404. A config instance → 409.
  - The detail view lists a waiting change, and doesn't list one whose newest run is published.
  - `start`: a maintainer gets 202 and the run is `start`; a viewer gets 403; a change that isn't waiting gets 409.
  - The waiting outcome text.

- [ ] **Step 2: Run, expect failures:** `go test ./internal/dashboard/`

- [ ] **Step 3: Implement,** following the existing `boolSettings`/`parseSettings`/`settingViews` and `reviewAction` patterns. Put every new route and field in `openapi.yaml`, and bump nothing else in it.

- [ ] **Step 4: Run, expect a pass;** then `go vet ./...`.

- [ ] **Step 5: Commit** `feat(dashboard): trusted authors settings, instance mode and Start review`

---

### Task 7: Web UI

**Files:**
- Modify: `web/src/api.ts`, `web/src/pages/RepositorySettings.tsx`, `web/src/pages/Integrations.tsx`, `web/src/pages/RepositoryDetail.tsx`, `web/src/test/fixtures.ts`
- Test: `web/src/pages/RepositorySettings.test.tsx`, `Integrations.test.tsx`, `RepositoryDetail.test.tsx`

**Interfaces:**
- Consumes: Task 6's JSON shapes and routes.
- Produces: `api.ts` types `TrustMode = "follow_visibility" | "on" | "off"`, `TrustedAuthor = { id?: number; username: string }`, `WaitingView`; `paths.apiInstanceTrust(name)`; `apiReviewAction` accepts `"start"`.

- [ ] **Step 1: Write the failing tests:**
  - **Settings:** a select labelled **Trusted authors only** with *Instance default (\<mode\>)* / *On* / *Off*. *Off* sends `"off"`; the instance default sends `null`.
  - **Allowlist:** an editor that adds `alice` and sends `[{username:"alice"}, ...kept entries with id]`. An entry can be removed. A config repository shows both read-only.
  - **Integrations:** a select on a dashboard instance's card sends `PUT …/trusted-authors {mode:"off"}`. A config instance shows the value read-only.
  - **Repository page:** a maintainer sees a **Waiting for a start** list with `!12 by stranger` and a **Start review** button that posts the `start` action. A viewer sees the list without the button. The check-failed reason shows "Couldn't check the author's access".

- [ ] **Step 2: Run, expect failures:** `npx vitest run src/pages/RepositorySettings.test.tsx src/pages/Integrations.test.tsx src/pages/RepositoryDetail.test.tsx` (if this worktree has no `node_modules`, link one from another checkout for the run and remove the link afterwards).

- [ ] **Step 3: Implement,** following the existing `Setting`/`Switch`, `InstanceCard` and `RepositoryDetail` tab patterns.

- [ ] **Step 4: Run the whole web gate:** `npx tsc -b --noEmit`, `npx eslint src`, `npx vitest run`, then delete any `tsconfig.tsbuildinfo` it created.

- [ ] **Step 5: Commit** `feat(web): trusted authors settings, instance mode and waiting changes`

---

### Task 8: README and chart docs

**Files:**
- Modify: `README.md`, `charts/pergent-sh/values.yaml` (commented example), `charts/pergent-sh/README.md` (only if it lists instance keys)

- [ ] **Step 1: Write the README changes:**
  - A new section **Trusted authors**, near **Fork merge requests**. It includes the spec's "At a glance" tables for GitHub and GitLab and the "What happens" table, and how to find a user's ID for the config allowlist (GitLab: `GET /users?username=`; GitHub: `GET /users/<login>`).
  - **Who may do what:** add the start rule.
  - **Reactions and replies:** add the refusal row.
  - **Limits on commands:** starts count against the per-change cap only.
  - **Config reference:** rows for `gitlab[].trusted_authors_only`, `github[].trusted_authors_only`, `repositories[].trusted_authors_only` and `repositories[].trusted_authors`.
  - **Repositories in the config and in the dashboard:** the new keys.
  - **Audit log:** `review_start`.
  - **Upgrading to the next release:** migration `0031`; public repositories stop reviewing authors without write access; how to set *always off* per instance; 0.11.x keeps working on the migrated database; remove the four new config keys before rolling back.

- [ ] **Step 2: Check.** Every reply and status text quoted in the README matches the constants (search for each). Run the name scan on the diff.

- [ ] **Step 3: Commit** `docs: trusted authors`

---

## Final gate (after Task 8)

- `go vet ./...` and `go test ./...` pass (Postgres tests skip locally and run in CI); the web gate passes; the name scan is clean.
- Whole-branch review on the most capable model, against the spec's anti-goals: a false trust, the author starting their own review, a paid-for note flood, and private setups broken.
