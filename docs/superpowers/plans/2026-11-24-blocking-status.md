# Blocking status Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A repository setting `blocking_status` (`failed` default, `neutral`) that keeps the `pergent` status from ever being red, while a blocking review is still never approved.

**Architecture:** The setting is stored and resolved like `failure_status` (config file, admin defaults, repository row). `policy.Decide` takes it as a third argument and, as its last step, turns every `failed` status into `neutral` with the suffix ` · not blocking merge`. The engine passes it from the repository settings; `gaveUpStatus` softens its `failed` the same way. The dashboard makes both status settings admin-only.

**Tech Stack:** Go, Postgres migrations, React + Vite + TanStack Query, vitest.

**Spec:** `docs/superpowers/specs/2026-11-24-blocking-status-design.md` (its "Decided questions" are settled).

## Global Constraints

- Values: `failed`, `neutral`. Default `failed`. Only a read `neutral` softens; an empty or unknown value is `failed`.
- Suffix: ` · not blocking merge`, added after the description is built, inside `maxDescription = 140`: the description is cut first (ending in `…`), the suffix is always whole.
- A status that `failure_status: neutral` already made neutral gets no suffix.
- `Approve` and `Kind` never change with the setting. The comment never changes.
- Settings that can't be read: `failed`.
- Migration `0053_blocking_status.sql`: `blocking_status text CHECK (blocking_status IN ('failed', 'neutral'))` on `dashboard_repos` and `repo_defaults`, NULL = use the default. Adds only.
- Dashboard API key `blockingStatus`: `"failed"`, `"neutral"` or `null`. Error text `blockingStatus must be "failed", "neutral" or null`.
- A non-admin PUT that names `failureStatus` or `blockingStatus` (null too): 403 `only an admin changes the status settings`, nothing saved.
- A maintainer's reset leaves both keys.
- Audit detail for the two keys: `failureStatus=<value>` / `blockingStatus=<value>`, `default` for null.
- Labels: `failure_status` **Status when a review can't run**; `blocking_status` **Status when a review finds blocking issues**. Help texts verbatim from the spec's Labels table. Options `failed (blocks merge)` and `neutral`.
- Notes: `Not used: the blocking status is neutral` (on `failure_status`, while blocking is neutral); `Only an admin changes this.` (under each status field, for a non-admin); repository page line `Merge gate: off (blocking status is neutral)`.
- Locally only focused tests (`go test -run '^(A|B)$'`, `npx vitest run <files>`). Postgres may be down: DB tests then skip and run on CI.
- Public repository: no private names. Docs: `→` is not allowed in UI paths.

## Review Focus

1. A blocking CI job on a `neutral` repository: the CI block runs before the softening, so the description keeps the CI text and gets the suffix. Pinned in Task 3's table.
2. A long description (500-character CI job name) with the suffix: still ≤ 140 runes and ends with the whole suffix. Pinned in Task 3.
3. Both settings `neutral` on a review that could not be done: neutral with no suffix (failure_status did it). Pinned in Task 3 and in Task 4's give-up test.
4. A maintainer's save on a page that shows the status fields: the web must never send the two keys for a non-admin, or the whole save is a 403. Pinned in Task 6.
5. A config-file repository with `blocking_status: neutral`: the dashboard shows it, read-only, and the merge gate line shows. Pinned in Task 2 (settings) and Task 6 (line).

---

### Task 1: Storage

**Files:**
- Create: `internal/store/migrations/0053_blocking_status.sql`
- Modify: `internal/store/dashboard.go` (`DashboardRepo`, `dashboardRepoColumns`, `scanDashboardRepo`, the INSERT/UPSERT/UPDATE statements), `internal/store/instances.go` (`RepoDefaults`, `RepoDefaults()`, `PutRepoDefaults`)
- Test: `internal/store/dashboard_test.go`, `internal/store/instances_test.go`

**Interfaces:**
- Produces: `store.DashboardRepo.BlockingStatus *string`, `store.RepoDefaults.BlockingStatus *string`.

- [ ] Step 1: Extend the existing round-trip tests: the dashboard repo test writes `BlockingStatus: ptrTo("neutral")` and reads it back; a `BlockingStatus: ptrTo("maybe")` upsert is an error (as `FailureStatus` at dashboard_test.go:81); the defaults test does the same in `PutRepoDefaults`.
- [ ] Step 2: Build the store test binary (`go vet ./internal/store/`): it fails on the unknown field.
- [ ] Step 3: Add the migration, the fields and the column at the end of each column list.
- [ ] Step 4: `go vet ./internal/store/`, and with Postgres up: `go test ./internal/store/ -run '^(TestDashboardReposCRUD|TestRepoDefaultsRoundTrip)$'`.
- [ ] Step 5: Commit `feat(store): store blocking_status`.

### Task 2: Config and settings

**Files:**
- Modify: `internal/config/config.go` (type, `Repository`, `fileRepository`, `repository()`, defaults, validation), `internal/settings/settings.go`
- Modify docs: `docs/reference/configuration.md` (new row `repositories[].blocking_status`), `docs/upgrading.md` (`## Unreleased`: the new key; rolling back needs it removed)
- Test: `internal/config/config_test.go`, `internal/settings/settings_test.go`

**Interfaces:**
- Produces: `type config.BlockingStatus string`, `config.BlockingStatusFailed = "failed"`, `config.BlockingStatusNeutral = "neutral"`; `config.Repository.BlockingStatus BlockingStatus` (yaml `blocking_status`, filled with `failed` when empty); `settings.KeyBlockingStatus = "blocking_status"`; `settings.Effective.BlockingStatus config.BlockingStatus`.

- [ ] Step 1: Tests. Config: the default is `failed` (beside config_test.go:59) and a `"maybe"` value is an error naming `blocking_status` (rows beside :110 and :386). Settings: add `KeyBlockingStatus` to `allKeys`; `wantDefaults` checks `BlockingStatusFailed`; `TestResolverConfigKeys`, `TestResolverDefaultsAndOverrides` and `TestBrowserDefaultsApplyToDashboardReposOnly` set `neutral` and expect it.
- [ ] Step 2: Run `go test ./internal/config/ ./internal/settings/ -run '^(TestLoadValid|TestValidateRejects|TestLoadGitHubMixed|TestValidateRejectsGitHub|TestResolverConfigKeys|TestDefaults)$'`: fails to build.
- [ ] Step 3: Implement as `failure_status`: config repo `BlockingStatus != failed` is overridden; a stored defaults or repository value is used only when it is `failed` or `neutral`.
- [ ] Step 4: Run the same tests: pass.
- [ ] Step 5: Commit `feat(config): blocking_status repository setting`.

### Task 3: policy.Decide

**Files:**
- Modify: `internal/policy/policy.go`
- Modify callers: every `Decide(` in `internal/policy/*_test.go`, `internal/engine/*.go` (pass `config.BlockingStatusFailed` in tests; `publish.go` gets its real value in Task 4, here it passes `config.BlockingStatusFailed`)
- Test: `internal/policy/blocking_test.go` (new)

**Interfaces:**
- Produces: `func Decide(o Outcome, failure config.FailureStatus, blocking config.BlockingStatus) Decision`; `func NotBlockingMerge(desc string) string` (cuts `desc` so `desc+suffix` fits 140 runes, then appends the suffix).

- [ ] Step 1: `TestDecideBlockingStatus`, one table over the spec's status table for both values: clean (success, approve, no suffix); blocking findings (`failed`/`Blocking issues found` vs `neutral`/`Blocking issues found · not blocking merge`, approve false, `KindBlocking`); clean + blocking CI job failed (`CI blocks approval: cycode-scan failed · not blocking merge`, `KindCI`); a failed skill with `failure_status: failed` (`Review failed for 1 of 2 skills · not blocking merge`, `KindFailed`); the same with `failure_status: neutral` (neutral, no suffix); failure reason with both neutral (no suffix); blocking `""` behaves as `failed`. Each row checks `Status`, `Approve`, `Kind`, `Description`, and that the comment is the same for both values. `TestNotBlockingMerge`: a 500-rune description gives ≤ 140 runes ending in `… · not blocking merge`; a short one is the text plus the suffix.
- [ ] Step 2: Run `go test ./internal/policy/ -run '^(TestDecideBlockingStatus|TestNotBlockingMerge)$'`: fails to build.
- [ ] Step 3: Implement: after the CI block in `Decide`, `if blocking == config.BlockingStatusNeutral && d.Status == StatusFailed { d.Status, d.Description = StatusNeutral, NotBlockingMerge(d.Description) }`. Update every caller.
- [ ] Step 4: Run the new tests plus `TestDecideCI`: pass. `go vet ./internal/policy/ ./internal/engine/`.
- [ ] Step 5: Commit `feat(policy): blocking_status neutral never sets failed`.

### Task 4: Engine

**Files:**
- Modify: `internal/engine/publish.go` (`publish`, `giveUp`, `gaveUpStatus`)
- Modify docs: `docs/concepts/approval.md` (status table and the publish_failed paragraph), `docs/concepts/ci-jobs.md` (the gate with `blocking_status: neutral`), `docs/setup/github.md` (Check Run conclusions)
- Test: `internal/engine/publish_test.go`

**Interfaces:**
- Consumes: `settings.Effective.BlockingStatus`, `policy.NotBlockingMerge`.
- Produces: `func (e *Engine) gaveUpStatus(ctx, run, log) (forge.StatusState, string)` (status, description).

- [ ] Step 1: Tests. `TestPublishBlockingNeutral`: one env with GitLab and GitHub (`addGitHub`), every config repository `BlockingStatus: neutral`; a blocking review on GitLab MR 1 gets status `success` with `Blocking issues found · not blocking merge`; one on GitHub PR 3 gets a `neutral` Check Run with that title; no approvals on either. `TestPublishGiveUpBlockingNeutral`: blocking findings, `failure_status: failed`, give-up: status `success`, description `pergent couldn't post its review · not blocking merge`, recorded `neutral`, not approved. Extend `TestPublishGiveUpUnreadableSettingsIsFailed` to set `BlockingStatus: neutral` on its row too (still failed).
- [ ] Step 2: Build fails / tests fail for the right reason (`go vet ./internal/engine/`; with Postgres up, run the three tests).
- [ ] Step 3: `publish` reads `blocking` beside `failure` (`failed` when the repository is not found) and passes it to both `Decide` calls. `gaveUpStatus`: settings error or not found → `failed`, `gaveUpDescription`; otherwise today's checks give a status, and a `failed` one on a `neutral` repository becomes `neutral` with `NotBlockingMerge(gaveUpDescription)`.
- [ ] Step 4: Run the tests (Postgres up) and `go vet`.
- [ ] Step 5: Commit `feat(engine): publish with blocking_status`.

### Task 5: Dashboard API

**Files:**
- Modify: `internal/dashboard/api_write.go` (`parseSettings`, `blockingStatusSetting`, `putSettings` admin check, `resetSettings`, audit detail, `msgBadSettings`), `internal/dashboard/api_operator.go` (`parseDefaults`, views, `pinnedDefaults`, `changedDefaults`, `msgBadDefaults`), `internal/dashboard/api_read.go` (`settingViews`), `internal/dashboard/view.go` (`DefaultsView`, `DefaultValues`, comments), `openapi.yaml`
- Modify docs: `docs/concepts/dashboard.md` (roles table, settings list, defaults list, config key list), `docs/upgrading.md` (breaking: maintainers lose `failureStatus`; migration 0053; new API key)
- Test: `internal/dashboard/api_write_test.go`, `internal/dashboard/api_operator_test.go`, `internal/dashboard/api_read_test.go`, `internal/dashboard/openapi_test.go` (runs without DB)

**Interfaces:**
- Produces: API key `blockingStatus` on `RepositoryDetailView.settings`, `DefaultsView`, `DefaultValues`, `pinned`, PUT bodies. `func isStatusKey(key string) bool`. `msgNeedsAdminStatus = "only an admin changes the status settings"`.

- [ ] Step 1: Tests. `TestStatusSettingsNeedAdmin` (one env): a maintainer's PUT of `{"failureStatus":"neutral"}`, `{"blockingStatus":"neutral"}`, `{"blockingStatus":null}` and `{"approve":false,"blockingStatus":"neutral"}` is 403 with the message and changes nothing; the maintainer's reset leaves both stored values; an admin's PUT `{"blockingStatus":"neutral","failureStatus":"neutral"}` is 200 and the audit detail is `blockingStatus=neutral, failureStatus=neutral`; an admin's reset clears both. Add `blockingStatus` rows to `TestSettingsValidation` and the defaults tests (`sameDefaults`, the key lists, the audit details). Update the expected JSON in api_read_test.go:1101.
- [ ] Step 2: `go vet ./internal/dashboard/` fails on the unknown fields; with Postgres up, `TestStatusSettingsNeedAdmin` fails for the right reason.
- [ ] Step 3: Implement. The reset clears `FailureStatus` and `BlockingStatus` only for an admin.
- [ ] Step 4: Run the openapi tests and, with Postgres up, the touched DB tests. `go vet ./...`.
- [ ] Step 5: Commit `feat(dashboard): blockingStatus setting, status settings are admin-only`.

### Task 6: Web

**Files:**
- Modify: `web/src/api.ts`, `web/src/test/fixtures.ts`, `web/src/pages/RepositorySettings.tsx`, `web/src/pages/AdminSettings.tsx`, `web/src/pages/RepositoryDetail.tsx`
- Test: `web/src/pages/RepositorySettings.test.tsx`, `web/src/pages/AdminSettings.test.tsx`, `web/src/pages/RepositoryDetail.test.tsx`

**Interfaces:**
- Consumes: `blockingStatus` from Task 5.
- Produces: `RepoSettings.blockingStatus: SettingView<string>` (added to every fixture), `DefaultsView.blockingStatus`, `DefaultValues.blockingStatus`, `SettingsUpdate.blockingStatus`.

- [ ] Step 1: Tests. Repository settings: an admin sees both selects enabled and saving sends only the changed key; a maintainer sees both disabled with `Only an admin changes this.` and a save of another field never carries them; `Not used: the blocking status is neutral` shows only while the blocking select is `neutral`. Admin settings: the new select, its **Use config value**, the note. Repository page: `Merge gate: off (blocking status is neutral)` shows only for `neutral`. Rename the label in the existing tests.
- [ ] Step 2: `npx vitest run src/pages/RepositorySettings.test.tsx src/pages/AdminSettings.test.tsx src/pages/RepositoryDetail.test.tsx`: the new cases fail.
- [ ] Step 3: Implement. A `canSetStatus={view.role === "admin"}` prop; `submit` sends the two keys only when `canSetStatus`. The maintainer reset confirm text adds the status settings.
- [ ] Step 4: Same vitest command, `npm run typecheck`, `npm run lint`: pass.
- [ ] Step 5: Commit `feat(web): blocking status field and merge gate line`.
