# Teams onboard and claim repositories: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Maintainers of a team onboard, claim, pause, resume, remove and release repositories on instances where an admin turned on "Teams may onboard repositories".

**Architecture:** One new boolean per instance (a column for dashboard instances, a config key for config instances), carried by the instance registry. The 4 admin-only repository routes move from `adminRoute` to a new `teamRoute` helper (session routes), and each handler keeps today's path for admins and adds one maintainer gate. 2 new store calls (`ClaimOwner`, `ReleaseOwner`) run under `ownersLockKey`. The picker gets a state and owner per row, and the UI shows buttons by flags the server computes.

**Tech Stack:** Go 1.x, Postgres (pgx), React + TanStack Query + Vitest.

**Spec:** `docs/superpowers/specs/2026-11-25-team-onboarding-design.md`

## Global Constraints

- **No local full test runs.** Locally run only `go test ./internal/<pkg>/ -run '^(TestA|TestB)$'` (exact names, with `$`) and `npx vitest run <changed test files>`, plus `go vet ./...`, `npm run typecheck`, `npm run lint`. Never `go test ./...`, a whole package, or a bare `npx vitest run`. CI runs everything.
- **No containers.** Never start or stop Docker/OrbStack. DB tests run locally only if `nc -z localhost 55432` succeeds, with `PERGENT_SH_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable` set **in the same command** as `go test`. If Postgres is down, say so and leave DB tests to CI.
- `internal/dashboard/openapi_test.go` needs no DB: run `-run '^(TestOpenAPI.*)$'`-style exact names from that file after every route or view change. Every route and view change updates `openapi.yaml` in the same task.
- The switch is per instance, **default off**: an upgrade changes nothing.
- Fixed 403 message when the switch is off: `teams may not onboard repositories on this instance`.
- Fixed 409 message for a maintainer's onboard of an on or paused repository: `the repository is already switched on: use Claim`.
- The 409 for a taken owner stays `<repo> already has owner <team>` (`store.OwnerTakenError`).
- Audit actions: existing `repository_on`, `repository_paused`, `repository_resumed`, `repository_removed`, `team_updated`, plus new `repository_claimed` and `repository_released` (detail: the team name), and `instance_updated` with detail `teams_may_onboard=on|off`.
- Admin-only settings stay admin-only. No change to what a team sees, except the picker names (spec, "picker shows every repository the bot reaches").
- Public repo: never write an email, a local path or a private name into a file. Before a push, run the name scan from the pergent-developer memory.
- No release, no version bump, no merge.
- Migration: take the next free number at build time (`0054` today; check `origin/main` and open branches).

## Review Focus

1. A maintainer of team Y onboards on an instance that team X owns at instance level: must be 403 `owned by X`, never a repository-level owner tick for Y. (Task 4 test.)
2. Two maintainers of different teams claim the same unowned repository at the same time: exactly 1 owner, the other 409. (Task 2 test.)
3. A maintainer of a team that only covers (does not own) a repository presses pause or remove: 403, not success via `Session.Role`. (Task 5 test.)
4. The switch goes off while a team owns repositories: pause and release still work, resume and remove are 403, the repositories and owners stay. (Task 5 test.)
5. A user on no team or a viewer calls any moved route: no write happens. A viewer gets 404; a user on no team gets the existing waiting gate (403 "waiting") that every session route gives before the handler. (Task 7 table test.)

Ruling (planner): the spec says a user on no team gets 404. On session routes the waiting gate in `Auth.Require` answers first with its 403. That is today's behavior for every session route, and it is safe because nothing runs. The plan keeps the gate and does not special-case these routes.

---

## File map

| File | Change |
|---|---|
| `internal/store/migrations/0054_teams_may_onboard.sql` | new column |
| `internal/store/instances.go` | `InstanceRow.TeamsMayOnboard`, `SetInstanceTeamsMayOnboard` |
| `internal/store/teams.go` | `ClaimOwner`, `ReleaseOwner`, `InstanceOwners` |
| `internal/config/config.go` | `teams_may_onboard` on GitLab and GitHub instances |
| `internal/instances/instances.go` | `Instance.TeamsMayOnboard`, `SetTeamsMayOnboard` |
| `internal/dashboard/dashboard.go` | `teamRoute` helper, `teamRoutes` list, `meView.OnboardInstances` |
| `internal/dashboard/onboard.go` (new) | the maintainer gates |
| `internal/dashboard/api_write.go` | moved routes, maintainer paths in `switchOn`, `setEnabled`, `removeRepo`, new `claimRepo`, `releaseRepo` |
| `internal/dashboard/api_operator.go` | picker state and owner; `PUT /api/admin/instances/{name}/teams-onboard` |
| `internal/dashboard/view.go` | `AvailableRepo.State/OwnerTeam`, `AvailableView.InstanceOwner`, `RepositoryDetailView.Actions`, `InstanceAdminView.TeamsMayOnboard` |
| `internal/dashboard/api_read.go` | fill `Actions` in `detailView` |
| `openapi.yaml` | every route and view change |
| `web/src/api.ts`, `web/src/pages/Repositories.tsx`, `RepositoryDetail.tsx`, `RepositorySettings.tsx`, `Team.tsx`, `Integrations.tsx` | UI |
| `docs/concepts/roles.md` (new), `docs/concepts/dashboard.md`, `docs/reference/configuration.md`, `docs/upgrading.md` | docs |

---

### Task 1: The switch: column, config key, registry, admin route

**Files:**
- Create: `internal/store/migrations/0054_teams_may_onboard.sql`
- Modify: `internal/store/instances.go`, `internal/config/config.go` (GitLab `Instance` ~line 165, `GitHubInstance` ~line 217), `internal/instances/instances.go` (Instance ~line 55, FromConfig ~299/305, FromRow ~336), `internal/dashboard/api_operator.go` (routes ~line 66, `instanceView` ~line 170), `internal/dashboard/view.go` (`InstanceAdminView`), `openapi.yaml`
- Test: `internal/store/instances_test.go` (or the file holding `SetInstanceTrustMode` tests), `internal/config/config_test.go`, `internal/dashboard/api_operator_test.go`

**Interfaces:**
- Produces:
  - `store.InstanceRow.TeamsMayOnboard bool`
  - `func (s *Store) SetInstanceTeamsMayOnboard(ctx context.Context, name string, on bool) (found bool, err error)`: same contract as `SetInstanceTrustMode` (ErrRemoving for a removing instance).
  - `config.Instance.TeamsMayOnboard bool \`yaml:"teams_may_onboard"\`` and the same on `config.GitHubInstance`.
  - `instances.Instance.TeamsMayOnboard bool`, `func (r *Registry) SetTeamsMayOnboard(name string, on bool) (Instance, bool)`.
  - `InstanceAdminView.TeamsMayOnboard bool \`json:"teamsMayOnboard"\``.
  - Route `PUT /api/admin/instances/{name}/teams-onboard` body `{"on": bool}` → 204, via `d.adminRoute`. A config instance is 409 `msgConfigInstance`. Audit `instance_updated`, detail `teams_may_onboard=on` or `=off`.

- [ ] **Step 1: Write the failing tests**
  - Store: `TestSetInstanceTeamsMayOnboard`: a new instance reads `TeamsMayOnboard == false`; after `SetInstanceTeamsMayOnboard(ctx, name, true)` it reads true; an unknown name gives `found == false`; a removing instance gives `ErrRemoving`.
  - Config: `TestTeamsMayOnboardKey`: YAML with `teams_may_onboard: true` on a gitlab and a github instance parses to true; absent parses to false.
  - Dashboard: `TestPutInstanceTeamsOnboard`: admin PUT `{"on": true}` → 204, `GET /api/admin/instances` shows `"teamsMayOnboard": true`, the audit log has `instance_updated` with `teams_may_onboard=on`; `{}` → 400; a config instance → 409.
- [ ] **Step 2: Run them, expect FAIL** (unknown field / route 404)
  Run: `go test ./internal/config/ -run '^(TestTeamsMayOnboardKey)$'` and, with Postgres up and the env var in the same command, `go test ./internal/store/ -run '^(TestSetInstanceTeamsMayOnboard)$'` and `go test ./internal/dashboard/ -run '^(TestPutInstanceTeamsOnboard)$'`.
- [ ] **Step 3: Implement**
  - Migration: `ALTER TABLE instances ADD COLUMN teams_may_onboard boolean NOT NULL DEFAULT false;`
  - Add `teams_may_onboard` to `instanceColumns` and `scanInstance`.
  - `putInstanceTeamsOnboard` copies the shape of `putInstanceTrust` (`api_operator.go:545`). Bad body message: `the body must be {"on": true or false}`.
  - `openapi.yaml`: the new route (`x-pergent-access: admin`) and the new field on the instance schema.
- [ ] **Step 4: Run the tests, expect PASS**, plus the `openapi_test.go` tests by exact name.
- [ ] **Step 5: Commit** `feat: add the teams may onboard switch per instance`

---

### Task 2: Store: claim, release, owners of an instance

**Files:**
- Modify: `internal/store/teams.go`
- Test: `internal/store/teams_test.go`

**Interfaces:**
- Consumes: `ownersLockKey`, `updateTeamTx`, `OwnerTakenError`, `ErrUnknownTeam`.
- Produces:
  - `type OwnerChange struct { Before, After Team }`
  - `func (s *Store) ClaimOwner(ctx context.Context, instance, project string, teamID int64) (OwnerChange, error)`: in one transaction, takes `pg_advisory_xact_lock(ownersLockKey)` **before** reading owners, refuses with `*OwnerTakenError` when any team owns the path at either level (`ownerTeamSQL`; for an instance owner, `Project: ""`), else sets the owner tick on the team's own entry for the path (matched ignoring case) or appends `TeamRepo{Instance, Project, Owner: true}`. The project is spelled as `dashboard_repos.project` stores it. An unknown team is `ErrUnknownTeam`. It does not touch `dashboard_repos.enabled`.
  - `func (s *Store) ReleaseOwner(ctx context.Context, instance, project string, teamID int64) (change OwnerChange, released bool, err error)`: clears the owner tick on the team's own entry for the path and keeps the entry. `released == false` when that entry has no tick (nothing written). It never touches an instance entry.
  - `func (s *Store) InstanceOwners(ctx context.Context, instance string) (byPath map[string]string, instanceOwner string, err error)`: owner team names, keyed by lower-cased project, and the instance-level owner's name ("" for none).

- [ ] **Step 1: Write the failing tests** (each `t.Parallel()`, one `newStore` each)
  - `TestClaimOwnerSetsTick`: team A has no entry; `ClaimOwner("main", "g/r", A)` → `OwnerTeam("main","g/r") == A`, and A has 1 entry with `Owner: true`.
  - `TestClaimOwnerKeepsExistingEntry`: A covers `g/r` without a tick → after the claim it has still 1 entry for `g/r`, now ticked.
  - `TestClaimOwnerRefusedRepoOwner`: B owns `g/r` → claim by A is `*OwnerTakenError{Team: "B"}`.
  - `TestClaimOwnerRefusedInstanceOwner`: B owns `{Instance: "main", Project: ""}` → claim of `g/r` by A is `*OwnerTakenError{Project: "", Team: "B"}`.
  - `TestClaimOwnerRace`: 2 goroutines claim `g/r` for A and B at once → exactly one nil error and one `*OwnerTakenError`.
  - `TestReleaseOwner`: A owns `g/r` → `ReleaseOwner` returns `released == true`, A still has the entry without a tick, `OwnerTeam` has no owner; a second call returns `released == false`.
  - `TestInstanceOwners`: A owns `g/r`, B owns the instance → `byPath["g/r"] == "A"`, `instanceOwner == "B"`.
- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/store/ -run '^(TestClaimOwnerSetsTick|TestClaimOwnerKeepsExistingEntry|TestClaimOwnerRefusedRepoOwner|TestClaimOwnerRefusedInstanceOwner|TestClaimOwnerRace|TestReleaseOwner|TestInstanceOwners)$'` (Postgres up, env var in the same command).
- [ ] **Step 3: Implement** the 3 functions in `teams.go`. Reuse `updateTeamTx` for the entry change, as `SwitchOnOwned` does (`dashboard.go:485`). `updateTeamTx` takes the lock only when a tick is set, so `ClaimOwner` must take it itself before its owner read.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat(store): claim and release a repository's owner`

---

### Task 3: The maintainer gate and the moved routes

**Files:**
- Create: `internal/dashboard/onboard.go`, `internal/dashboard/onboard_test.go`
- Modify: `internal/dashboard/dashboard.go` (`adminRoute` at line 338), `internal/dashboard/api_write.go` (`writeRoutes` lines 84-88), `internal/dashboard/routes_test.go`, `openapi.yaml`

**Interfaces:**
- Consumes: `d.teamAccess`, `d.maintainsTeam` (`api_chains.go:498`), `d.registry.Get`, `Store.OwnerTeam`.
- Produces (all in `onboard.go`):
  - `const msgTeamsMayNotOnboard = "teams may not onboard repositories on this instance"`
  - `const msgUseClaim = "the repository is already switched on: use Claim"`
  - `const msgNotOwnerMaintainer = "only maintainers of the owner team and admins can do this"`
  - `func (d *Dashboard) teamRoute(mux *http.ServeMux, pattern string, h http.HandlerFunc)`: records `pattern` in `d.teamRoutes []string` and registers `d.Auth.Require(h)`.
  - `func (d *Dashboard) maintainedTeams(ctx context.Context, se Session) ([]store.Team, error)`: the teams on which se is a maintainer (none for an admin; callers check `se.Admin` first).
  - `func (d *Dashboard) teamsMayOnboard(instance string) bool`: the registry's flag; false for an unknown or removing instance.
  - `func (d *Dashboard) ownerMaintainer(ctx context.Context, se Session, instance, project string) (owner int64, ok bool, err error)`: `ok` when `OwnerTeam` returns a team that se maintains.
  - `func (d *Dashboard) maintainerGate(w http.ResponseWriter, r *http.Request, instance string) bool`: for a non-admin, 404 when se maintains no team or the instance is unknown, 403 `msgTeamsMayNotOnboard` when the switch is off; true when it may go on.
  - `func (d *Dashboard) maintainerGateAnySwitch(w http.ResponseWriter, r *http.Request, instance string) bool`: the same without the switch check (pause, release).

- [ ] **Step 1: Write the failing tests**
  - `TestTeamRoutesRecorded` (no DB, in `routes_test.go`): after `d.Handler()`, `d.teamRoutes` holds exactly `GET /api/repositories/available`, `POST /api/repositories`, `PUT /api/admin/repositories/{instance}/{rest...}`, `DELETE /api/repositories/{instance}/{rest...}`; and `d.adminRoutes` holds none of them.
  - Update `TestAdminRoutesAreRegisteredOnlyThroughTheHelper`: its sanity check names `GET /api/users` and one other admin route (not `GET /api/repositories/available`, which moved).
  - `TestOwnerMaintainer` (DB): a maintainer of the owner team → ok; a maintainer of a covering team that is not the owner → not ok; a maintainer of an instance-level owner → ok.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.**
  - Swap `d.adminRoute` to `d.teamRoute` for the 4 routes.
  - Add, as the **first lines** of each of the 4 handlers, `if !se.Admin && !d.maintainerGate(w, r, instance) { return }`. Pause and release use `maintainerGateAnySwitch` instead (Tasks 5 and 6).
  - Until Tasks 4-6 land, each handler's non-admin path after the gate answers 403 `msgNotOwnerMaintainer`. So this task changes no behavior for admins and opens nothing yet.
  - `openapi.yaml`: these 4 routes change to `x-pergent-access: session`, with descriptions that say "admins, and maintainers on an instance that lets teams onboard".
- [ ] **Step 4: Run** `TestTeamRoutesRecorded`, `TestAdminRoutesAreRegisteredOnlyThroughTheHelper`, `TestEveryAdminRouteIsNotFoundForAUser`, the openapi tests, and `TestOwnerMaintainer`. Expect PASS.
- [ ] **Step 5: Commit** `refactor: register the repository routes through teamRoute`

---

### Task 4: Picker state and onboarding by a maintainer

**Files:**
- Modify: `internal/dashboard/api_operator.go` (`listAvailable` ~line 709), `internal/dashboard/api_write.go` (`available`, `switchOn` ~line 236), `internal/dashboard/view.go` (`AvailableView`, `AvailableRepo` ~line 1155), `openapi.yaml`
- Test: `internal/dashboard/api_available_test.go`, `internal/dashboard/onboard_test.go`

**Interfaces:**
- Consumes: Task 2 `InstanceOwners`, Task 3 gate and constants.
- Produces:
  - `AvailableRepo.State string \`json:"state"\``: `"off"`, `"paused"` or `"on"`.
  - `AvailableRepo.OwnerTeam string \`json:"ownerTeam"\``: the owner's name, its own or the instance's; "" for none.
  - `AvailableView.InstanceOwner string \`json:"instanceOwner"\``: "" for none.

Behavior:
- `listAvailable` no longer drops on and paused repositories. It marks each row with its state from `Repos.ListAll` (`Paused` → `paused`) and its owner from `InstanceOwners`. Config-file repositories are `on` with their owner. The page cut is unchanged.
- `switchOn` for a non-admin (after the gate):
  1. `ownerTeam` is required and must be one of `maintainedTeams` → else 400 `msgNoTeam` (missing) or 403 `msgNotOwnerMaintainer` (not theirs).
  2. If the instance has an instance-level owner team: if it is not `ownerTeam`, 403 with `owned by <team>`; if it is, the repository is switched on with **no** own owner tick (`InsertDashboardRepo`), so it inherits.
  3. Otherwise `SwitchOnOwned` as today.
  4. A repository that already has a `dashboard_repos` row (on or paused) is 409 `msgUseClaim`, checked before any write. This includes paused: a maintainer never resumes through onboard.
  5. A config-file repository stays 409 `msgConfigManaged`.
- Admin behavior is unchanged, including resume of a paused repository.

- [ ] **Step 1: Write the failing tests** (one `newEnv`, table subtests)
  - `TestAvailableStates`: one off, one paused, one on owned by team A → the rows say `off`/`paused`/`on`, the owned one has `ownerTeam: "A"`.
  - `TestAvailableInstanceOwner`: team B owns the instance → `instanceOwner: "B"` and every row's `ownerTeam` is `"B"` unless it has its own owner.
  - `TestMaintainerOnboard`: switch on, maintainer of A posts `{project, instance, ownerTeam: A}` → 201, `OwnerTeam` is A, audit has `repository_on` and the owner change.
  - `TestMaintainerOnboardNeedsOwnTeam`: no `ownerTeam` → 400; a team they are only a viewer of → 403.
  - `TestMaintainerOnboardSwitchOff`: switch off → 403 `msgTeamsMayNotOnboard`, nothing stored.
  - `TestMaintainerOnboardOnOrPaused`: an on repository and a paused one → both 409 `msgUseClaim`, paused stays paused.
  - `TestMaintainerOnboardInstanceOwned`: B owns the instance; maintainer of A → 403 `owned by B`; maintainer of B → 201, the repository has no own owner tick, `OwnerTeam` is B. (Review Focus 1.)
  - `TestViewerOnboard`: a viewer → 404.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement** as above. Update the `available` handler: a non-admin passes the gate first. Update the picker's `openapi.yaml` schema and description (it now lists on and paused repositories too).
- [ ] **Step 4: Run the new tests, the existing `api_available_test.go` tests by name, and the openapi tests. Expect PASS.** Existing tests that expect on or paused rows to be left out change to expect them with their state.
- [ ] **Step 5: Commit** `feat: maintainers onboard repositories from the picker`

---

### Task 5: Pause, resume and remove by the owner team's maintainers

**Files:**
- Modify: `internal/dashboard/api_write.go` (`setEnabled` ~line 475, `removeRepo` ~line 545)
- Test: `internal/dashboard/onboard_test.go`

**Interfaces:**
- Consumes: Task 3 `ownerMaintainer`, gates and constants.

Behavior:
- Both handlers today call `d.target(w, r, "", RoleAdmin)`. A non-admin must pass `RoleViewer` instead (they must see the repository; 404 otherwise), then `ownerMaintainer` (403 `msgNotOwnerMaintainer` otherwise).
- Switch check: pause (`enabled: false`) works with the switch off; resume (`enabled: true`) and remove need it on (403 `msgTeamsMayNotOnboard`).
- Remove deletes as today (`RemoveDashboardRepo`).

- [ ] **Step 1: Write the failing tests**
  - `TestOwnerMaintainerPausesAndResumes`: switch on → pause 200 (`repository_paused` in audit), resume 200.
  - `TestCoveringMaintainerCannotPause`: A owns, B covers; a B maintainer → 403 for pause and remove. (Review Focus 3.)
  - `TestSwitchOffLimits`: switch off; an A maintainer → pause 200, resume 403, remove 403; the repository and the owner stay. (Review Focus 4.)
  - `TestOwnerMaintainerRemoves`: A owns, B covers; an A maintainer removes → 200; B's entry and A's owner tick are gone; then a B maintainer onboards it with `ownerTeam: B` → 201.
  - `TestInstanceOwnerMaintainerPauses`: B owns the instance; a B maintainer pauses a repository of it → 200.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run, expect PASS**, plus the existing `pause_test.go` and `pause_write_test.go` tests by name.
- [ ] **Step 5: Commit** `feat: owner team maintainers pause, resume and remove`

---

### Task 6: Claim and release routes

**Files:**
- Modify: `internal/dashboard/api_write.go` (`repositoryPost` ~line 580 dispatch), `openapi.yaml`
- Test: `internal/dashboard/onboard_test.go`

**Interfaces:**
- Consumes: Task 2 `ClaimOwner`, `ReleaseOwner`; Task 3 gates.
- Produces: `POST /api/repositories/{instance}/{rest...}/claim` body `{"team": <id>}` and `POST .../release` with an empty body `{}`, both dispatched from `repositoryPost` by suffix (they are session routes already). Both answer 200 with the `RepositoryDetailView`.

Behavior:
- **Claim** does not use `target` (a maintainer may not see a repository their team does not cover). It finds the repository with `d.Repos.GetAny`; not there → 404. A config-file repository → 409 `msgConfigManaged` for a maintainer; an admin may claim it. A non-admin needs the gate with the switch on, and `team` among `maintainedTeams` (403 otherwise). An admin may name any team. Taken → 409 with `OwnerTakenError.Error()`. Audit: `repository_claimed` (detail: team name), then `auditOwners(before, after)`. Paused stays paused.
- **Release** uses `target(..., RoleViewer)`. A non-admin needs `ownerMaintainer` and an owner tick on **their team's own entry** for the path (an instance-level owner → 403 `msgNotOwnerMaintainer`). The switch can be on or off. An admin releases the repository's own owner tick, whichever team holds it. Nothing to release → 409 `the repository has no owner of its own`. Audit `repository_released` (detail: team name) and `auditOwners`.

- [ ] **Step 1: Write the failing tests**
  - `TestClaimUnowned`: switch on; on repository, no owner; an A maintainer claims → 200, `OwnerTeam == A`, audit `repository_claimed`.
  - `TestClaimPausedStaysPaused`.
  - `TestClaimTaken`: B owns → 409 `g/r already has owner B`.
  - `TestClaimInstanceOwned`: B owns the instance → 409.
  - `TestClaimNotOn`: no `dashboard_repos` row and not in config → 404.
  - `TestClaimSwitchOff`: 403 `msgTeamsMayNotOnboard`.
  - `TestClaimConfigRepo`: maintainer 409, admin 200.
  - `TestReleaseByOwnerMaintainer`: works with the switch off; A keeps its entry, no owner after.
  - `TestReleaseInstanceOwnerRefused`: B owns the instance → a B maintainer gets 403; an admin gets 409 (no own owner).
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.** `openapi.yaml`: 2 new operations, `x-pergent-access: session`.
- [ ] **Step 4: Run, expect PASS**, plus the openapi tests.
- [ ] **Step 5: Commit** `feat: claim and release a repository's owner`

---

### Task 7: Button flags, onboard instances in /api/me, the role table test

**Files:**
- Modify: `internal/dashboard/view.go` (`RepositoryDetailView` ~line 510), `internal/dashboard/api_read.go` (`detailView`, near `ownerTeamView` line 1260), `internal/dashboard/dashboard.go` (`meView` ~line 344, `me`), `openapi.yaml`
- Test: `internal/dashboard/onboard_test.go`

**Interfaces:**
- Produces:
  - `type RepoActionsView struct { Pause, Resume, Remove, Release bool }` with JSON `pause`, `resume`, `remove`, `release`; field `RepositoryDetailView.Actions RepoActionsView \`json:"actions"\``. Each flag is true exactly when the matching route would succeed for this session now: the repository's state (paused or on), the switch, owner-team maintainer, config-file (all false), and for `release` an owner tick on the session's team's own entry (an admin: any own owner tick).
  - `meView.OnboardInstances []string \`json:"onboardInstances"\``: for a non-admin who maintains at least one team, the instances with the switch on, sorted; `[]` otherwise; for an admin, every instance not removing.

- [ ] **Step 1: Write the failing tests**
  - `TestRepoActionsFlags`: table over admin, A maintainer (owner), B maintainer (covers), A viewer; on and paused; switch on and off. Expected values follow Tasks 5 and 6 (for example, the A maintainer with the switch off and the repository on: `pause: true, resume: false, remove: false, release: true`).
  - `TestMeOnboardInstances`: maintainer with switch on for `main` only → `["main"]`; viewer → `[]`.
  - `TestRoleMatrix` (Review Focus 5): table over the 7 actions (picker, onboard, pause, resume, remove, claim, release) × {admin, owner maintainer, other-team maintainer, viewer, no team} × {switch on, off}. It asserts only the status code. Expected values are the spec's rules table.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.** Compute the flags once in `detailView`, from the same helpers the handlers use, so they cannot disagree.
- [ ] **Step 4: Run, expect PASS**, plus the openapi tests.
- [ ] **Step 5: Commit** `feat: show the onboarding actions a session may take`

---

### Task 8: UI: the picker for maintainers

**Files:**
- Modify: `web/src/api.ts` (`Me`, `AvailableView`, `RepositoryDetailView`, `InstanceAdminView` types; `paths.apiRepoClaim`, `paths.apiRepoRelease`), `web/src/pages/Repositories.tsx`
- Test: `web/src/pages/Repositories.test.tsx`

Behavior:
- **+ Add repository** shows when `me.admin || me.onboardInstances.length > 0`. A maintainer's instance choice lists `me.onboardInstances`, and the list comes from `/api/repositories/available?instance=` (an admin keeps `/api/admin/instances/{name}/repositories`).
- Owner team select: an admin as today. A maintainer: their `role === "maintainer"` teams from `GET /api/me/teams`, with no "None"; with one team it is selected and the select is hidden. When `instanceOwner` is set, the select is replaced by the line `Owner: <team> (all repositories of <instance>)`.
- Each row's button: `off` → **Onboard** (admin label stays **Switch on**); `on` or `paused` with no `ownerTeam` → **Claim** (POST claim with the chosen team); `paused` and `ownerTeam` is one of the maintainer's teams → **Resume** (PUT `apiAdminRepo` `{enabled: true}`); any other owned row → the text `owned by <team>`, no button. An admin's `paused` row → **Resume**.
- After a write: invalidate `repositories`, `available`, `my-teams`, `teams` and navigate to the repository, as today.

- [ ] **Step 1: Write the failing tests**: one test per row state for a maintainer (Onboard, Claim, Resume, "owned by B"); a viewer sees no **+ Add repository**; with `instanceOwner` the select is gone and the line shows.
- [ ] **Step 2: Run** `npx vitest run src/pages/Repositories.test.tsx`, expect FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same test file, then `npm run typecheck` and `npm run lint`. Expect PASS.
- [ ] **Step 5: Commit** `feat(web): maintainers onboard and claim from the picker`

---

### Task 9: UI: repository page, team page, instance form

**Files:**
- Modify: `web/src/pages/RepositoryDetail.tsx` (the `Paused` banner ~line 132; owner line ~line 64), `web/src/pages/RepositorySettings.tsx` (Remove card ~line 205), `web/src/pages/Team.tsx`, `web/src/pages/Integrations.tsx`
- Test: `RepositoryDetail.test.tsx`, `RepositorySettings.test.tsx`, `Team.test.tsx`, `Integrations.test.tsx`

Behavior:
- Repository page: the `Paused` banner's Resume shows on `view.actions.resume` (not `role === "admin"`). Next to the owner line: **Pause** on `actions.pause`, **Release ownership** on `actions.release`, which asks `Release ownership of <project>? <team> keeps access, and the repository has no owner team until a team claims it.`
- Settings page: the Remove card shows on `view.actions.remove`. When other teams cover the repository, the confirmation adds `These teams also lose it: <names>.` (the names come from a new `coveringTeams: string[]` on `RepositoryDetailView`; add it in this task, server side in `detailView`, and in `openapi.yaml`; empty for a viewer).
- Team page: **+ Add repository** for a maintainer of the team, when `me.onboardInstances` is not empty. It links to `/repositories?add=1&team=<id>`, and the picker pre-selects that team.
- Instance form (Integrations, next to the trusted-authors control): a checkbox **Teams may onboard repositories** with the line `Maintainers of any team can switch on repositories the review bot reaches, and become their owner.` It PUTs `/api/admin/instances/{name}/teams-onboard`. It is read-only for a config instance, showing the config value.

- [ ] **Step 1: Write the failing tests** for each flag-driven button (shown when true, hidden when false), the Remove confirmation text with 2 covering teams, the team page button, and the instance checkbox PUT body `{"on": true}`.
- [ ] **Step 2: Run** the 4 test files, expect FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the 4 files, `npm run typecheck`, `npm run lint`, the Go test for `coveringTeams` by name, and the openapi tests. Expect PASS.
- [ ] **Step 5: Commit** `feat(web): repository, team and instance controls for team onboarding`

---

### Task 10: Docs

**Files:**
- Create: `docs/concepts/roles.md`
- Modify: `docs/concepts/dashboard.md` (sections "Teams" ~line 164 and "Who sees what" ~line 208), `docs/reference/configuration.md`, `docs/upgrading.md`, `docs/README.md` (link roles.md)

Content:
- `roles.md`: one table of admin / maintainer / viewer / no team for every action in the dashboard, including the spec's rules table, the chain and Reviewers card writes (admin or owner-team maintainer), and the admin-only settings list from the spec's Non-goals.
- `configuration.md`: `teams_may_onboard` (bool, default false) on GitLab and GitHub instances.
- `upgrading.md`: the new switch is off by default; nothing changes on upgrade.

- [ ] **Step 1: Write the docs.** Easy English, tables over prose.
- [ ] **Step 2: Check** that every route in `roles.md` matches `openapi.yaml`, and run the private-name scan and the email/path grep from the pergent-developer memory over the changed files. Expect no output.
- [ ] **Step 3: Commit** `docs: roles matrix and team onboarding`
