# Pause a repository instead of removing it: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The switch on `/repos` pauses a dashboard repository (row kept, `enabled = false`, settings kept) and resumes it, and a separate **Remove** on the repository page deletes it, its settings and its team links.

**Architecture:** A new `enabled` column on `dashboard_repos`. `Store.DashboardRepos` and `Store.DashboardRepo` keep the meaning "switched on" and filter it, so the engine, webhooks, dispatch, publish and commands see a paused repository as one that is not switched on. New all-rows reads (`AllDashboardRepos`, `AnyDashboardRepo`, and `Resolver.ListAll` / `GetAny` on top) feed only the dashboard places that must show a paused repository, the picker's "already added" check and the config takeover. A new admin route sets `enabled`; `DELETE` becomes Remove. On a paused repository, reconcile takes the webhook and deploy token off the platform and clears their fields, but keeps the `projects` row and its dismissals, commands, inline threads and finding fates. Only a repository with no `dashboard_repos` row goes through `DeleteProject`.

**Tech Stack:** Go 1.24, Postgres (pgx), `net/http` ServeMux, React + Vite + TanStack Query, Vitest.

**Spec:** `docs/superpowers/specs/2026-11-18-repository-pause-design.md`. Read it with this plan. Its 17 decisions are final.

## Global Constraints

- Migration file `internal/store/migrations/0042_repository_pause.sql`: `ALTER TABLE dashboard_repos ADD COLUMN enabled boolean NOT NULL DEFAULT true`. Existing rows stay on. The previous release must keep working on the migrated database (pergent.sh `CLAUDE.md`).
- A paused repository looks like one that is not switched on to every engine path: no Job, no platform call, a late webhook ignored, a command dropped, a run finished after a pause posted with approval off.
- A pause never calls `Store.DeleteProject`: the `projects` row and its dismissals, commands, inline threads, finding fates, fate jobs and fate checks stay, so team members still see the runs. Readiness scans and setup problems are still dropped. Resume reuses the kept row. Remove, and a repository with no `dashboard_repos` row, go through `DeleteProject` as today.
- Job-starting or platform-writing actions on a paused repository answer **409** with exactly `the repository is paused`.
- Resume is refused with **409** `this instance is being removed` (the existing `msgInstanceRemoving`) while its instance is being removed.
- The new endpoint is registered through `Dashboard.adminRoute` only (404 for a non-admin, never 403) and carries `x-pergent-access: admin` in `openapi.yaml`.
- A config-managed repository has no switch and no Remove: both answer 409 `this repository is managed by the config file; change it there` (the existing `msgConfigManaged`).
- Remove confirm text, exact: `Remove ${project} from pergent? Its settings and team links are deleted, and its webhook and clone credentials are removed. This can't be undone.`
- UI words, exact: the `Paused` pill (list Health column), the `Paused` note (repository page), `paused` and `removed` (Usage and Teams pages).
- Audit actions: `repository_paused`, `repository_resumed`, `repository_removed`. `repository_on` stays for a new switch-on. `repository_off` is no longer written.
- No release: no version bump, tag, chart version or version number. Upgrade notes go under `## Unreleased` at the top of `docs/upgrading.md`.
- Public repository: no employer or internal names. Before each commit run, in `~/Documents/personal/github/pergent.sh`: `git grep -niE 'golabs|gojek|gopay[^t]|goto\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'` and a per-file `grep` over new untracked files. Both must print nothing.
- Never start a container (no `docker`, `orbctl`). Database tests run only when `nc -z localhost 55432` succeeds, with the variable in the same command: `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m <pkgs>`. If the port is closed, say so and leave those tests to CI. A run without the variable proves nothing for `internal/store`, `internal/settings`, `internal/dashboard` or `internal/engine`.
- No `gh`, `glab` or `curl`.
- Work on branch `feat/repository-pause` off `main`. Commit with `git -c user.email=zufardhiyaulhaq@gmail.com commit`, a conventional subject, and a `Co-Authored-By:` line that names the model that wrote the change. Run tests and commits as separate commands.
- Docs are the behaviour contract (`CLAUDE.md`: "User-facing behaviour goes in `docs/`"). Change them with the behaviour, in Task 8 at the latest.

## Choices this plan makes (the spec left them to the builder)

| Item | Choice |
|---|---|
| Endpoint | `PUT /api/admin/repositories/{instance}/{project...}`, body `{"enabled": true\|false}`, answers 200 `RepositoryDetailView` |
| Same state again | Pause of a paused repository, or resume of a running one: 200, no audit entry, no reconcile |
| `POST /api/repositories` on a paused repository | Resumes it: 200 (not 201) with `RepositoryDetailView`, audit `repository_resumed`. A removing instance stays 404 there, as today |
| Store names | `AllDashboardRepos`, `AnyDashboardRepo`, `SetDashboardRepoEnabled`, `RemoveDashboardRepo` |
| Resolver names | `Resolver.ListAll`, `Resolver.GetAny`, field `Effective.Paused` |
| View fields | `RepositoryView.Paused` (`on` is false while paused), `health: "paused"`, `UsageRepositoryRowView.Paused` |
| Reads during a settings edit | Kept: the trusted-author lookup, the link checks, `GET /api/admin/linkable` and the Skills & instructions tab are reads by the review bot or App. They set nothing up, and "Settings stay editable" needs them. |

## Review Focus

1. **Case.** A pause, resume or Remove named `ACME/Gadgets` for a row stored as `acme/gadgets` must hit that row, and Remove must delete team links spelled in either case. Pinned by `TestRemoveDashboardRepo` (Task 1) and `TestPauseResumeIgnoresCase` (Task 4).
2. **Same state twice.** A double click on the switch, or a stale page, sends the same state again: 200, no second audit entry, no second reconcile. Pinned by `TestPauseTwiceIsQuiet` (Task 4).
3. **A stale setup problem.** A paused repository that still has a `repo_attention` row (reconcile has not run yet) must show `Paused`, not Needs attention, and stay out of the Overview's Needs attention. Pinned by `TestPausedRepoHealthAndOverview` (Task 3).
4. **Other team links.** Remove must keep a team's whole-instance link (`project ""`) and a link to the same path on another instance. Pinned by `TestRemoveDashboardRepo` (Task 1).
5. **A run queued before the pause.** It must be dropped at dispatch, with no Job. Pinned by step 4 of `TestEndToEndPauseResume` (Task 6).
6. **The hourly reconcile of a paused repository.** It must make no platform call once the hook and token are gone. Pinned by the second `reconcile` in `TestReconcilePausedDashboardRepo` (Task 2).

Also not testable here, so it goes in the upgrade note (Task 8): **a rollback to the previous release switches every paused repository on again**, because the older code ignores `enabled`.

---

### Task 1: Store: the `enabled` column, safe reads, all-rows reads, pause and Remove

**Files:**
- Create: `internal/store/migrations/0042_repository_pause.sql`
- Modify: `internal/store/dashboard.go:14-79` (struct), `:168-197` (columns, scan), `:199-231` (reads), `:400-406` (add Remove next to Delete)
- Modify: `internal/store/readiness.go:389-405` (`UnscannedProjects`)
- Test: `internal/store/dashboard_test.go`, `internal/store/readiness_test.go`, `internal/store/library_test.go`, `internal/store/models_test.go`

**Interfaces:**
- Produces:
  - `DashboardRepo.Enabled bool`: read from the row. The writes `UpsertDashboardRepo`, `InsertDashboardRepo` and `UpdateDashboardRepo` never write it (a new row gets the default `true`; an existing row keeps its state).
  - `func (s *Store) DashboardRepos(ctx) ([]DashboardRepo, error)` and `func (s *Store) DashboardRepo(ctx, instance, project string) (DashboardRepo, bool, error)`: unchanged signatures, now `WHERE enabled`.
  - `func (s *Store) AllDashboardRepos(ctx context.Context) ([]DashboardRepo, error)`: every row, paused too, same order.
  - `func (s *Store) AnyDashboardRepo(ctx context.Context, instance, project string) (DashboardRepo, bool, error)`: one row, paused too.
  - `func (s *Store) SetDashboardRepoEnabled(ctx context.Context, instance, project string, enabled bool) (found, changed bool, err error)`: sets `enabled` and `updated_at = now()` only when it differs.
  - `func (s *Store) RemoveDashboardRepo(ctx context.Context, instance, project string) (found bool, err error)`: in one transaction, deletes the `team_repos` rows with that instance and `lower(project) = lower($2)`, then the `dashboard_repos` row. A missing row is `found == false` and deletes nothing.
- Unchanged on purpose: `DeleteDashboardRepo` (the config takeover keeps team links), `UpdateDashboardRepo` (settings edits work on a paused row), `DeleteInstanceData`, `DeleteLibrarySkill`, `DeleteLLMModel` (they already read every row, so a paused repository still counts as in use).

- [ ] **Step 1: Write the failing tests**

In `dashboard_test.go`:

```go
// A paused row is not "switched on": the safe reads skip it, the all-rows
// reads return it with Enabled false, and its settings stay.
func TestPausedDashboardRepo(t *testing.T) {
	// insert acme/widgets with Approve=false, insert acme/gadgets
	// found, changed, err := s.SetDashboardRepoEnabled(ctx, "main", "ACME/Widgets", false) -> true, true, nil
	// again with false -> true, false, nil
	// DashboardRepos -> only acme/gadgets; DashboardRepo(acme/widgets) -> found false
	// AllDashboardRepos -> both, in order; widgets.Enabled == false, gadgets.Enabled == true
	// AnyDashboardRepo(acme/WIDGETS) -> found, Enabled false, *Approve == false
	// UpdateDashboardRepo on the paused row -> found true, the change is stored, Enabled stays false
	// InsertDashboardRepo of acme/widgets -> created false, the row stays paused
	// SetDashboardRepoEnabled(..., true) -> true, true; DashboardRepo finds it again with *Approve == false
	// SetDashboardRepoEnabled on a missing row -> found false, changed false, nil
}

// Remove deletes the row and the team links to it, in any case, and leaves
// whole-instance links and links to the same path on another instance.
func TestRemoveDashboardRepo(t *testing.T) {
	// row main/Acme/Widgets (paused) and a team covering
	//   {main, "acme/widgets"}, {main, ""}, {gh, "acme/widgets"}, {main, "acme/gadgets"}
	// found, err := s.RemoveDashboardRepo(ctx, "main", "ACME/WIDGETS") -> true, nil
	// AnyDashboardRepo -> found false
	// the team's Repos == [{gh acme/widgets} {main ""} {main acme/gadgets}] (store order)
	// RemoveDashboardRepo again -> false, nil
	// the same test with a running row: found true, row gone
}
```

Extend `TestDashboardMigrationTables` (`dashboard_test.go:115`) to check the column: `enabled`, `boolean`, `NOT NULL`, default `true`.

In `readiness_test.go`, `TestUnscannedProjectsSkipsPaused`: a project row for a dashboard repository that is paused is not listed; the same project resumed is listed; a config-file project (no dashboard row) is still listed.

In `library_test.go`, `TestDeleteInUseByPausedRepo`, and in `models_test.go`, `TestDeleteLLMModelInUseByPausedRepo`: a paused repository that names the skill (model) makes the delete fail with `*SkillInUseError` (`*ModelInUseError`) naming it. These pass on today's code once a paused row can exist; they pin that it stays so.

- [ ] **Step 2: Run them and watch them fail**

Run: `nc -z localhost 55432 && PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./internal/store/ -run 'TestPausedDashboardRepo|TestRemoveDashboardRepo|TestDashboardMigrationTables|TestUnscannedProjectsSkipsPaused|InUseByPausedRepo'`
Expected: build fails (`SetDashboardRepoEnabled`, `AllDashboardRepos`, `RemoveDashboardRepo` undefined, `Enabled` unknown field).

- [ ] **Step 3: Implement**

The migration, with a comment in the house style (see `0041_check_deleted.sql`). Add `enabled` to the end of `dashboardRepoColumns` and to `scanDashboardRepo`. Give `DashboardRepos` and `DashboardRepo` a `WHERE enabled` (`AND enabled`), and build the two all-rows reads on the same SELECT without it. In `UnscannedProjects`, add `AND (d.instance IS NULL OR d.enabled)` to the WHERE, and update its doc comment. Update the `DashboardRepo` type comment: a row is switched on while `Enabled`, paused otherwise.

- [ ] **Step 4: Run the store package**

Run: `nc -z localhost 55432 && PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./internal/store/`
Expected: `ok` with a real time (seconds, not `0.0xs`).

- [ ] **Step 5: Commit** `feat(store): pause a dashboard repository with an enabled column`

---

### Task 2: Resolver and engine: paused rows for the dashboard, config takeover, the paused reconcile

**Files:**
- Modify: `internal/settings/settings.go:68-150` (`Effective`), `:166-226` (`List`, `Get`, add `ListAll`, `GetAny`), `:510` (`fromDashboard` sets `Paused`)
- Modify: `internal/engine/reconcile.go:120-152` (the loop over stored projects), `:176-196` (`dropTakenOver`), `:289-344` (`removeProject`: split out the platform part)
- Test: `internal/settings/settings_test.go`, `internal/engine/reconcile_test.go`, `internal/engine/readiness_test.go`

**Interfaces:**
- Consumes: Task 1 (`AllDashboardRepos`, `AnyDashboardRepo`, `DashboardRepo.Enabled`, `SetDashboardRepoEnabled`).
- Produces:
  - `Effective.Paused bool`: true for a dashboard repository that is paused. Always false from `List` and `Get`, and for a config repository.
  - `func (r *Resolver) ListAll(ctx context.Context) ([]Effective, error)`: `List` with paused dashboard rows too, in the same order. Config still wins over a dashboard row of the same repository.
  - `func (r *Resolver) GetAny(ctx context.Context, instance, project string) (Effective, bool, error)`: `Get` that also finds a paused row.
  - `List` and `Get` keep their meaning, "switched on". No engine caller changes, except reconcile.
  - In reconcile, `paused` is the set of keys of `AllDashboardRepos` rows with `Enabled == false` that the config does not list. A stored project that is not wanted and is in `paused` goes to `func (e *Engine) pauseProject(ctx context.Context, p store.Project) error` instead of `removeProject`, then `dropScans` and `dropAttention` as today.
  - `pauseProject`: on GitLab, deletes the webhook (`HookID != 0`) and the deploy token (`DeployTokenID != 0`), then stores the row with `HookID`, `DeployTokenID`, `DeployTokenUsername`, `DeployTokenSealed` and `DeployTokenExpiresAt` cleared (`UpsertProject`). With both IDs already 0 it calls nothing. On GitHub, and for an instance pergent.sh is not connected to, it leaves the row as it is. It never calls `DeleteProject`.
  - `func (e *Engine) unhook(ctx context.Context, gl *gitlab.Client, p store.Project) error`: the hook and token deletes, shared by `removeProject` and `pauseProject`.

- [ ] **Step 1: Write the failing tests**

`settings_test.go`, `TestResolverPausedRepo`: a dashboard row with `Approve=false`, paused. `List` and `Get` leave it out (`ok == false`). `ListAll` holds it once with `Paused == true` and `Approve == false`, after the config repositories. `GetAny` finds it with `Paused == true`. After resume, `List` holds it with `Paused == false`. A paused dashboard row of a config repository: `ListAll` holds only the config one, `Paused == false`.

`reconcile_test.go`:
- `TestReconcileDropsPausedDashboardRowOfConfigRepo`: like `TestReconcileDropsDashboardRowOfConfigRepo` (line 493), but the `ACME/Widgets` row is paused first. After `reconcile`, `AnyDashboardRepo` finds nothing, and the log says `dashboard record`.
- `TestReconcilePausedDashboardRepo`: like `TestReconcileDashboardRepo` (line 520), with `SetDashboardRepoEnabled(false)` in place of `DeleteDashboardRepo`, and a dismissal and a command stored for project 9 before the pause. After `reconcile`: the hook and deploy-token DELETEs were each made once, the project row is still there with `HookID == 0`, `DeployTokenID == 0` and no sealed token, and the dismissal and the command are still stored. A second `reconcile` makes no GitLab DELETE. Then `SetDashboardRepoEnabled(true)` and `reconcile`: the same row (project 9) gets `HookID == newHookID` and a new deploy token, the dismissal is still there, and with `Scans` on one queued scan started by `store.StartedBySwitchOn` exists.
- `TestReconcileRemovedAfterPause`: pause and reconcile as above, then `RemoveDashboardRepo` and `reconcile`. The project row and its dismissal are gone, and no GitLab DELETE is made (the IDs were already 0).
- `TestReconcilePausedGitHubRepoKeepsRow`: a GitHub dashboard repository that reconcile stored, paused, then `reconcile`: the row is unchanged.

`readiness_test.go`, `TestRequestScanPausedRepo`: `RequestScan` on a paused dashboard repository returns `readiness.ErrNotSwitchedOn` and queues nothing.

- [ ] **Step 2: Run and watch them fail**

Run: `nc -z localhost 55432 && PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./internal/settings/ ./internal/engine/ -run 'TestResolverPausedRepo|TestReconcileDropsPausedDashboardRowOfConfigRepo|TestReconcilePausedDashboardRepo|TestRequestScanPausedRepo'`
Expected: build fails (`ListAll`, `GetAny`, `Paused` undefined). After the resolver part alone, `TestReconcileDropsPausedDashboardRowOfConfigRepo` fails (the paused row is still there) and `TestReconcilePausedDashboardRepo` fails (the project row is deleted).

- [ ] **Step 3: Implement**

`List` and `ListAll` share one helper over a row list. `dropTakenOver` reads `AllDashboardRepos`. Its comment says a paused row is dropped too. If `AllDashboardRepos` fails, reconcile removes nothing, as it does today when the switched-on list can't be read. Update the `Reconcile` doc comment.

- [ ] **Step 4: Run both packages whole**

Run: `nc -z localhost 55432 && PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./internal/settings/ ./internal/engine/`
Expected: `ok` for both, the engine package in minutes.

- [ ] **Step 5: Commit** `feat(settings): list paused dashboard repositories for the dashboard`

---

### Task 3: Dashboard: show a paused repository

**Files:**
- Modify: `internal/dashboard/api_read.go:153-166` (add `visibleRepos` beside `activeRepos`), `:784-830` (`repositories`, `repositoryViews`), `:838-860` (`repository` uses `visibleRepos`)
- Modify: `internal/dashboard/api_write.go:368-397` (`targetRepo` uses `visibleRepos`), `:737` (`updateSettings` reads back with `GetAny`)
- Modify: `internal/dashboard/access.go:41` (`a.repos.ListAll`)
- Modify: `internal/dashboard/api_operator.go:727` (`listAvailable` uses `ListAll`)
- Modify: `internal/dashboard/api_org.go:587` (`teamRepos` uses `ListAll`)
- Modify: `internal/dashboard/api_models.go:53` (`modelUsers` uses `ListAll`)
- Modify: `internal/dashboard/api_usage.go:79-95` (`Paused` on each row)
- Modify: `internal/dashboard/api_linked.go:276` (`linkable` uses `visibleRepos`)
- Modify: `internal/dashboard/outcome.go:32-33` (add `healthPaused = "paused"`)
- Modify: `internal/dashboard/view.go:337-347` (`RepositoryView`), `:1321-1328` (`UsageRepositoryRowView`), the write-API comment at `:860-870`
- Modify: `openapi.yaml` `RepositoryView` (~5801), `UsageRepositoryRowView` (~6320), the `listRepositories` summary (~1077)
- Test: `internal/dashboard/api_read_test.go`, `api_write_test.go` (helpers), `api_usage_test.go`, `api_org_test.go`, `openapi_test.go:1369` (`healths`)

**Interfaces:**
- Consumes: Task 2 (`ListAll`, `GetAny`, `Effective.Paused`), Task 1 (`SetDashboardRepoEnabled` in tests).
- Produces:
  - `func (d *Dashboard) visibleRepos(ctx context.Context, se Session) ([]settings.Effective, error)`: `activeRepos` over `Repos.ListAll`.
  - `activeRepos` stays "switched on" and is still used by the Overview (`api_read.go:255`), so a paused repository adds nothing to its Needs attention or stats list.
  - `targetRepo` (and so `target` and `targetAny`) now finds a paused repository. Task 5 adds the 409 where an action starts a Job.
  - `RepositoryView.Paused bool` (json `paused`); while paused `On == false` and `Health == "paused"`, whatever setup problems are stored.
  - `UsageRepositoryRowView.Paused bool` (json `paused`): true when the row is a paused repository. `SwitchedOn` keeps its meaning.
  - Test helpers in `api_write_test.go`: `func (e *env) pausedRepo(instance, project string, edit func(*store.DashboardRepo))` (`dashboardRepo`, then pause), and `storedRepo` changed to read with `AnyDashboardRepo` and so find a paused row.

- [ ] **Step 1: Write the failing tests**

`api_read_test.go`:
- `TestPausedRepoHealthAndOverview`: a running `acme/gadgets` and a paused `acme/tools`, each with a stored `repo_attention` row. `GET /api/repositories` as admin lists both. Tools has `on: false`, `paused: true`, `health: "paused"`. Gadgets has `on: true`, `paused: false`, `health: "attention"`. `GET /api/overview` attention names gadgets only.
- `TestPausedRepoStaysVisible`: a viewer whose team covers paused `acme/tools` gets 200 on `GET /api/repositories/main/acme/tools` with `repo.paused == true`, sees it in `GET /api/repositories`, and gets 200 on its `/readiness` and `/skill-results`. A user on another team gets 404, as for any repository they can't see.
- `TestPickerHidesPausedRepo`: the catalog reaches `acme/tools` and `acme/new`. With `acme/tools` paused, `GET /api/repositories/available?instance=main` lists only `acme/new`.

`api_write_test.go`, `TestSettingsEditOnPausedRepo`: a maintainer `PUT .../settings {"approve": false}` on a paused repository answers 200 with `repo.paused == true`. The stored row has `Approve == false` and stays paused. `e.reconciles` is unchanged, and the fake readiness got no `RequestScan`. The same for `POST .../settings/reset`.

`api_org_test.go`, `TestTeamMayCoverPausedRepo`: `PUT /api/teams/{id}` with `repos: [{main, acme/tools}]` (paused) answers 200, and the team holds it.

`api_usage_test.go`, `TestUsageMarksPausedRepo`: usage rows for a running, a paused and a removed repository have `switchedOn`/`paused` true/false, false/true and false/false.

`openapi_test.go`: add `healthPaused` to `healths` (line 1369).

- [ ] **Step 2: Run and watch them fail**

Run: `nc -z localhost 55432 && PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./internal/dashboard/ -run 'TestPausedRepo|TestPickerHidesPausedRepo|TestSettingsEditOnPausedRepo|TestTeamMayCoverPausedRepo|TestUsageMarksPausedRepo|TestOpenAPI'`
Expected: build fails on `healthPaused` and the `Paused` fields. Then a paused repository answers 404 and is missing from the list.

- [ ] **Step 3: Implement** as in the Interfaces block. In `openapi.yaml`, add `paused` (boolean, required) to both schemas, add `paused` to the `health` enum, and say in `RepositoryView.description` and the list summary that paused repositories are listed. Update the `view.go` comments to match.

- [ ] **Step 4: Run the dashboard package whole and the offline spec check**

Run: `go test -count=1 ./internal/dashboard/ -run 'TestOpenAPI|TestAdminRoutes'` (no database needed). Expected: `ok`.
Run: `nc -z localhost 55432 && PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./internal/dashboard/`. Expected: `ok`. A test that asserted "switched on" through `storedRepo` and now sees a paused row is a real failure: read it, don't loosen it.

- [ ] **Step 5: Commit** `feat(dashboard): list and show paused repositories`

---

### Task 4: Dashboard: pause, resume and Remove

**Files:**
- Modify: `internal/dashboard/api_write.go:75-91` (routes), `:228-301` (`switchOn` resumes a paused row), `:398-430` (`switchOff` becomes `removeRepo`), constants at `:55-68`
- Modify: `openapi.yaml`: the `switchOnRepository` description (~1094), `switchOffRepository` becomes `removeRepository` (~1271), a new path `/api/admin/repositories/{instance}/{project}`, a new schema `RepositoryEnabledRequest`
- Modify: `internal/dashboard/openapi_test.go:994-1010` (`openAPIUntypedSchemas` gets `"RepositoryEnabledRequest": "struct in Dashboard.setEnabled"`)
- Test: `internal/dashboard/api_write_test.go`, `internal/dashboard/api_operator_test.go`

**Interfaces:**
- Consumes: Task 1 (`SetDashboardRepoEnabled`, `RemoveDashboardRepo`, `InsertDashboardRepo`), Task 2 (`GetAny`), Task 3 (`target` finds paused rows; `detailView` shows `Paused`).
- Produces:
  - Route `d.adminRoute(mux, "PUT /api/admin/repositories/{instance}/{rest...}", d.setEnabled)`.
  - `func (d *Dashboard) setEnabled(w http.ResponseWriter, r *http.Request)`: body ``struct{ Enabled *bool `json:"enabled"` }`` via `decodeStrict`; nil or bad is 400 `msgBadEnabled`. It looks the repository up with `d.target(w, r, "", RoleAdmin)` (404 unseen, 409 config). Resume while `d.registry.Get(instance)` is missing or `Removing` is 409 `msgInstanceRemoving`. Then `SetDashboardRepoEnabled`. When it changed: log, audit `repository_paused` or `repository_resumed`, `d.reconcileSoon`. Answers 200 with `detailView` of `GetAny`.
  - `func (d *Dashboard) removeRepo(w http.ResponseWriter, r *http.Request)`: replaces `switchOff` on `DELETE /api/repositories/{instance}/{rest...}`. Calls `RemoveDashboardRepo`, audits `repository_removed`, calls `reconcileSoon`, and answers as `switchOff` did (200, `on: false`, `paused: false`, default settings, no attention).
  - `switchOn`: when `InsertDashboardRepo` says not created, call `SetDashboardRepoEnabled(…, true)`. When that changed it, audit `repository_resumed`, `reconcileSoon`, and answer 200 with the detail. Otherwise 409 `msgAlreadyOn`, as today.
  - ``msgBadEnabled = `the body must be {"enabled": true or false}` `` (a Go raw string).

- [ ] **Step 1: Write the failing tests**

`api_write_test.go`:
- `TestPauseAndResume`: admin, dashboard repo `acme/gadgets` with `Approve=false`. `PUT /api/admin/repositories/main/acme/gadgets {"enabled": false}` gives 200, `repo.on == false`, `repo.paused == true`, `settings.approve == {false, true}`. The row is stored with `Enabled == false`. `e.reconciles` goes up by 1, and the audit log has `repository_paused`. Then `{"enabled": true}` gives 200, `repo.on == true`, the same settings, `repository_resumed`, and one more reconcile. Each `reconciled` entry is `main/acme/gadgets`.
- `TestPauseResumeIgnoresCase`: the same with `.../main/ACME/Gadgets` on a row stored `acme/gadgets`. It hits that row, and `e.reconciled` names the stored spelling.
- `TestPauseTwiceIsQuiet`: pause twice, and resume a running repository. Each answers 200, writes no second audit entry and makes no second reconcile.
- `TestPauseRefusals`: body `{}`, `{"enabled": "no"}` and `{"enabled": true, "x": 1}` give 400 with `msgBadEnabled`. A config repository gives 409 `msgConfigManaged`, a missing one 404. Resume after `d.registry.SetRemoving("main")` gives 409 `this instance is being removed` and the row stays paused (pause is still allowed).
- `TestSwitchOnResumesPausedRepo`: a paused `acme/gadgets` with `Approve=false`, and the catalog reaches it. `POST /api/repositories {"instance":"main","project":"acme/gadgets"}` gives 200 (not 201) with `repo.on == true` and `settings.approve.value == false`, plus `repository_resumed` and one reconcile. A second POST gives 409 `msgAlreadyOn`. With the catalog no longer reaching it, POST gives 404 and the row stays paused.
- `TestRemoveRepo` (rename and extend the DELETE half of `TestSwitchOnTriggersReconcile`, line 584): `DELETE` on a running repository and on a paused one each answer 200 with `repo.on == false && !repo.paused`. `AnyDashboardRepo` finds nothing, the repository's direct team link is gone, the audit has `repository_removed`, and a reconcile is asked for. A config repository gives 409.

The routes walk `TestEveryAdminRouteIsNotFoundForAUser` and `TestAdminRoutesAreRegisteredOnlyThroughTheHelper` (`routes_test.go`) cover the new route's 404 for a viewer, a maintainer and a waiting user once it is registered. No new test is needed there; check they still pass.

- [ ] **Step 2: Run and watch them fail**

Run: `go test -count=1 ./internal/dashboard/ -run 'TestOpenAPI|TestAdminRoutes'`
Expected after only the route is added: FAIL, the spec has no admin `PUT /api/admin/repositories/{instance}/{rest...}`.
Run: `nc -z localhost 55432 && PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./internal/dashboard/ -run 'TestPause|TestSwitchOnResumesPausedRepo|TestRemoveRepo'`
Expected: 404 or 405 on the new route, and 409 on the POST resume.

- [ ] **Step 3: Implement** as in the Interfaces block. In `openapi.yaml` the new operation is `setRepositoryEnabled`, tag `Repositories`, `x-pergent-access: admin`, `x-pergent-pattern: PUT /api/admin/repositories/{instance}/{rest...}`, with responses 200 `RepositoryDetailView`, 400, 401, 403 (CSRF), 404 and 409. The description says what pause and resume do and that the same state again changes nothing. The DELETE description says Remove deletes the settings and team links. The POST description says a paused repository is resumed with its settings (200).

- [ ] **Step 4: Run the dashboard package whole**

Run: `go test -count=1 ./internal/dashboard/ -run 'TestOpenAPI|TestAdminRoutes|TestRequestFor'`, then the database run of `./internal/dashboard/` as in Task 3.
Expected: `ok` for both.

- [ ] **Step 5: Commit** `feat(dashboard): pause, resume and remove a repository`

---

### Task 5: Dashboard: nothing starts a Job for a paused repository

**Files:**
- Modify: `internal/dashboard/api_write.go` (add `msgPaused` and `refusePaused`)
- Modify: `internal/dashboard/api_readiness.go:146-150` (`scanNow`)
- Modify: `internal/dashboard/api_commands.go:104-107` (`reviewAction`: rerun, start, dismiss, restore)
- Modify: `openapi.yaml`: a `409` line in the descriptions of `readiness/scan` and the four review actions
- Test: `internal/dashboard/api_readiness_test.go`, `internal/dashboard/api_commands_test.go`

**Interfaces:**
- Consumes: Task 3 (`targetRepo` finds paused repositories, `Effective.Paused`).
- Produces:
  - `msgPaused = "the repository is paused"`.
  - `func refusePaused(w http.ResponseWriter, repo settings.Effective) bool`: writes 409 `msgPaused` and returns true when `repo.Paused`.
  - `scanNow` and `reviewAction` call it right after the target lookup, before they decode a body or call the platform. Settings, reset, teams and past-dismissals/stop do not call it: they touch only the database.

- [ ] **Step 1: Write the failing tests**

- `api_readiness_test.go`, `TestScanNowPausedRepo`: a maintainer's `POST .../readiness/scan` on a paused repository answers 409 with body error `the repository is paused`. The fake readiness has 0 `RequestScan` calls, and no `readiness_scan_requested` audit entry is written.
- `api_commands_test.go`, `TestReviewButtonsPausedRepo`: for `rerun`, `start`, `dismiss` and `restore` on a paused repository with a stored review, each answers 409 `the repository is paused`. No run is queued (`Store` run count unchanged), the fake source got no `GetChange` call, and `e.wakes` stays 0.
- Extend `TestReviewDetail` in `api_read_test.go:618`: on a paused repository `can` is all false. Today's `Repos.Get` check at `api_read.go:482` already does this; the test pins it.

- [ ] **Step 2: Run and watch them fail**

Run: `nc -z localhost 55432 && PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./internal/dashboard/ -run 'TestScanNowPausedRepo|TestReviewButtonsPausedRepo'`
Expected: FAIL. Scan now answers 404 (engine `ErrNotSwitchedOn`), and the buttons reach the platform.

- [ ] **Step 3: Implement** `refusePaused` and the two calls.

- [ ] **Step 4: Run the dashboard package whole** (database run as in Task 3). Expected: `ok`.

- [ ] **Step 5: Commit** `feat(dashboard): refuse Job-starting actions on a paused repository`

---

### Task 6: Engine end to end: a paused repository is quiet

**Files:**
- Create: `internal/engine/e2e_pause_test.go`
- Test only. If a step fails, the fix goes in the package that owns the code, with its own unit test.

**Interfaces:**
- Consumes: `newDashboardE2E` (`internal/engine/e2e_dashboard_test.go:67`) and its helpers (`e.write`, `e.do`, `gl.Reply`, `gl.Requests`, `env.Engine.takeSoonKeys`, `ReconcileRepos`, `openMR`). Also Tasks 1 to 5.

- [ ] **Step 1: Write the test `TestEndToEndPauseResume`**

1. The admin switches `acme/gadgets` on and sets `approve: false` and `quietReviews: true`. A `ReconcileRepos` sets it up (one hook POST). A merge request is reviewed, and a finding of it is dismissed with the dashboard button. A viewer `bob` is on a team that covers `acme/gadgets`.
2. `PUT /api/admin/repositories/<instance>/acme/gadgets {"enabled": false}` gives 200. `ReconcileRepos(takeSoonKeys())` deletes the hook and the deploy token (one DELETE each), `ReadinessScans` for it is empty, and the project row is still stored. `bob` still gets 200 on the repository page and on `GET /api/reviews/<instance>/acme/gadgets/1`, and the review list for the repository holds the review.
3. A merge request webhook for it answers 200 and queues no run (`Store` has no run for MR 1). A `/pergent rerun` note webhook is dropped: no command row, no reply note POST.
4. A run queued before the pause (insert it before step 2) is dropped at dispatch: after an engine pass no Job is created (`env.Jobs()` is empty) and no status POST is made.
5. A settings `PUT` while paused answers 200 and makes no request to the fake GitLab (`gl.Requests("", "")` is empty after `gl.Reset()`).
6. `{"enabled": true}` gives 200. `ReconcileRepos` creates a hook again, one scan with `StartedBySwitchOn` is queued, and `GET /api/repositories/<instance>/acme/gadgets` shows `approve == false`, `quietReviews == true` and `repo.on == true`. The dismissal from step 1 is still stored.
7. A merge request webhook now queues a run, and the finding dismissed in step 1 is still dismissed in it.

- [ ] **Step 2: Run it**

Run: `nc -z localhost 55432 && PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./internal/engine/ -run TestEndToEndPauseResume -v`
Expected: PASS. Prove it can fail: put `DashboardRepos` back without `WHERE enabled` and run it again. Step 3 must fail. Then put the filter back.

- [ ] **Step 3: Run the engine package whole** (database run, `./internal/engine/`). Expected: `ok`, in about 4 minutes.

- [ ] **Step 4: Commit** `test(engine): a paused repository starts nothing and resumes with its settings`

---

### Task 7: Web: the switch pauses, the page removes

**Files:**
- Modify: `web/src/api.ts:227-235` (`RepositoryView`), the `UsageRepositoryRowView` type, `paths` (~1232: add `apiAdminRepo`)
- Modify: `web/src/pages/Repositories.tsx:18-105` (`Row`)
- Modify: `web/src/pages/RepositoryDetail.tsx:38-90` (Paused note, Remove, hide Start review), `web/src/pages/Readiness.tsx:59` (hide Scan now)
- Modify: `web/src/pages/Usage.tsx:60-75`, `web/src/pages/Teams.tsx:150-180, 342`
- Modify: `web/src/test/fixtures.ts:235-290` (fixtures get `paused: false`, add `repoToolsPaused`)
- Test: `web/src/pages/Repositories.test.tsx`, `RepositoryDetail.test.tsx`, `Readiness.test.tsx`, `Usage.test.tsx`, `Teams.test.tsx`

**Interfaces:**
- Consumes: Task 3 and Task 4 JSON: `RepositoryView.paused`, `health: "paused"`, `UsageRepositoryRowView.paused`, `PUT /api/admin/repositories/{instance}/{project}` with `{"enabled": boolean}`, `DELETE /api/repositories/{instance}/{project}`.
- Produces:
  - `RepositoryView.paused: boolean`, `health: "ok" | "attention" | "paused"`; `UsageRepositoryRowView.paused: boolean`.
  - `paths.apiAdminRepo(instance: string, project: string)` gives `/api/admin/repositories/${seg(instance)}/${proj(project)}`.
  - `Readiness` gets a prop `paused: boolean`. While it is true, Scan now is not drawn.

- [ ] **Step 1: Write the failing tests**

`Repositories.test.tsx` (replace `"lets an admin switch off a dashboard repository, not a config one"`, line 265):
- `"pauses a repository with the switch, with no confirm, and keeps it in the list"`: `window.confirm` is spied and never called. Clicking the switch of a running repository sends `PUT /api/admin/repositories/main/acme/gadgets` with body exactly `{"enabled":false}` and the CSRF token, then the list is fetched again.
- `"resumes a paused repository with the switch"`: the row of `repoToolsPaused` has the switch unchecked and enabled. A click sends `{"enabled":true}`.
- `"shows Paused in the Health column"`: the paused row's Health cell reads `Paused`, with no `OK` or `Needs attention`.
- `"has no switch for a config repository, and draws it disabled while the instance is being removed"`.

`RepositoryDetail.test.tsx`:
- `"shows the Paused note on a paused repository"`.
- `"removes a repository after the confirm, and goes back to the list"`: an admin, dashboard repository. Clicking `Remove repository` calls `window.confirm` with the exact Remove text from Global Constraints. OK sends `DELETE /api/repositories/main/acme/gadgets` and navigates to `/repos`. Cancel sends nothing.
- `"has no Remove for a config repository or a maintainer"`.
- `"hides Start review while paused"`.

`Readiness.test.tsx`: `"hides Scan now while paused"`.

`Usage.test.tsx`: `"says paused (linked) or removed"`. A paused row links to its page and shows the `paused` pill. A removed row shows `removed` with its CSV link and no page link. There is no `switched off` text left.

`Teams.test.tsx`: `"lists a paused repository as a choice, marked paused, and an old link as removed"`. The paused repository is tickable, with `(paused)` linked to its page. A team link to a repository not in the list shows `(removed)`, unticked.

- [ ] **Step 2: Run them and watch them fail**

Run (with no `go test` running): `cd ~/Documents/personal/github/pergent.sh/web && npx vitest run src/pages/Repositories.test.tsx src/pages/RepositoryDetail.test.tsx src/pages/Readiness.test.tsx src/pages/Usage.test.tsx src/pages/Teams.test.tsx`
Expected: FAIL. No PUT is sent, a confirm is shown, and the words are missing.

- [ ] **Step 3: Implement**

`Row`: the switch's `checked={repo.on}` is drawn for a dashboard repository, `disabled={config || pending || removing}`, and `onChange(on)` sends the PUT with no confirm. `removing` comes from the `["admin-instances"]` query the Picker already uses, read only for admins. The Health cell shows `<Pill tone="grey">Paused</Pill>` when `health === "paused"`. The detail page header shows the Paused note (`role="note"`). Copy: `Paused: pergent doesn't review this repository, and its settings are kept. An admin switches it on again under Repositories.` It shows `Remove repository` (a `btn`) for an admin on a dashboard repository, paused or not. The sub line says `paused in the dashboard` while paused. Update the comments that say "switch off".

- [ ] **Step 4: Run the web gate**

Run: `cd ~/Documents/personal/github/pergent.sh/web && npm run typecheck && npm run lint && npx vitest run && npm run build`
Expected: all pass. If a different test fails on each run, the machine is loaded: rerun when it is quiet before you judge.

- [ ] **Step 5: Commit** `feat(web): the switch pauses a repository; Remove is on its page`

---

### Task 8: Docs, upgrade note and the full gate

**Files:**
- Modify: `docs/concepts/switching-off.md` (title `# 📴 Pausing and removing a repository`; the file name stays, so links keep working)
- Modify: `docs/README.md:45` (link text `Pausing and removing a repository`, `What's paused, what's removed, what's kept`)
- Modify: `docs/concepts/dashboard.md:158-159` (roles table: pause and remove), `:176` and `:181` (the list's switch, the `Paused` pill, the picker hides paused ones), `:213` (pause, resume and Remove), `:305` (pausing deletes scans, as switching off did), `:309` (audit: `repository_paused`, `repository_resumed`, `repository_removed`; older entries keep `repository_off`)
- Modify: `README.md:8` ("admins pause, resume and remove repositories")
- Modify: `docs/upgrading.md` (new `## Unreleased` at the top)

**Interfaces:** none. Docs only, plus the full verification.

- [ ] **Step 1: Write the docs**

`switching-off.md` explains the three actions in a table, the same as the spec's Behaviour table: who, where, confirm, what happens. Then what a paused repository does: it stays visible and editable, starts nothing, ignores webhooks and commands, its picker entry is hidden, and `POST /api/repositories` resumes it. A config repository is still paused only by taking it out of the config, which removes it. Say that a pause deletes nothing but the webhook, the deploy token, the readiness scans and the setup problems: dismissals, commands, inline threads, finding fates and the review history stay. Remove deletes those too, as switching off did.

`docs/upgrading.md`, `## Unreleased`:
- **The switch on Repositories now pauses a repository.** Its row and settings stay. `PUT /api/admin/repositories/{instance}/{project}` with `{"enabled": false}` pauses it and `true` resumes it. A script that switched off with `DELETE` now removes the repository for good: settings and team links are deleted.
- **`DELETE /api/repositories/{instance}/{project}` now also deletes the repository's team links.** Links left by earlier switch-offs stay and show as `removed` on Teams. Untick them there.
- Migration `0042` runs at startup and adds one column with a default, so back up Postgres first, as for any migration. The previous release keeps working on the migrated database, **but it ignores the column: after a rollback every paused repository is switched on again** at the next reconcile. Remove the ones you don't want before you roll back.
- No config change.

- [ ] **Step 2: Check the quoted strings**

Every string the docs quote must be in the code: `the repository is paused`, `this instance is being removed`, the Remove confirm text, `Paused`, `paused`, `removed`. Check each with `git grep -n` in `internal/` and `web/src/`.

- [ ] **Step 3: Run the full gate** (each command on its own)

```bash
cd ~/Documents/personal/github/pergent.sh
nc -z localhost 55432 && PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m ./...
go vet ./...
cd web && npm run typecheck && npm run lint && npx vitest run && npm run build
```

Expected: every package `ok`, with real times for `internal/store`, `internal/settings`, `internal/dashboard` and `internal/engine` (minutes, not `0.0xs`). `go vet` prints nothing. The web gate passes. The chart did not change, so no `helm lint`. If port 55432 is closed, report the database packages as not run.

- [ ] **Step 4: Private-name scan** (the `git grep` from Global Constraints, plus `grep` over untracked files). Expected: no output.

- [ ] **Step 5: Commit** `docs: pausing and removing a repository`

---

## Self-review notes

- Spec coverage: every Behaviour row is covered (Tasks 1, 4 and 7), and so is each "While paused" bullet (visible: 3; editable: 3; nothing starts: 5 and 6; webhook and command: 6; approvals left: unchanged code; picker and POST resume: 3 and 4; resume refused while removing: 4 and 7). The rules that stay: config has no switch (4, 7), takeover deletes a paused row (2), late run posted unapproved (unchanged `publish.go:108`). Wording (7, 8), migration and safe reads (1), new read users (2, 3), in-use 409 (1), endpoint and audit (4), docs and upgrade (8). Every line of "Tests that prove it" has a named test.
- The paused reconcile path (keep the `projects` row, clear the hook and token) is the owner's ruling of 2026-11-18 and is in the spec. Task 2 builds it; Task 6 checks that a dismissal and the review history survive a pause.
