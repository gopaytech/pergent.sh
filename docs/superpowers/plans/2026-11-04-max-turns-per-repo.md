# Step limit per repository (`max_turns`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Each repository's review Job runs pergent with a step limit from config or the dashboard (default 40), the review page shows it, and step-limit advice points at where it can be raised.

**Architecture:** `max_turns` follows the Job timeout end to end: config keys, `settings.Effective`, store columns, the dashboard settings API and both settings pages, and dispatch, which passes `--max-turns=N` and records it on the run. The publisher replaces pergent's step-limit advice in the stored result once, right after merging a re-run, before it renders anything.

**Tech Stack:** Go 1.x, Postgres (pgx), React + TypeScript (vitest).

**Spec:** `docs/superpowers/specs/2026-11-04-max-turns-per-repo-design.md`

## Global Constraints

- Work on branch `feat/max-turns` cut from `main`. No push, tag or PR without asking the user.
- Default `jobs.max_turns` is **40**; every value, wherever it is set, must be from **5 to 200**.
- Config keys: `jobs.max_turns`, `repositories[].max_turns`. Settings key: `max_turns`. API field: `maxTurns`. Dashboard label: **Max steps**. Help text, verbatim: "Steps each opencode run may take before pergent stops it; each step can call several tools."
- Who may change it, and reset, exactly as the Job timeout: maintainer or admin of the repository; a reset (also a maintainer's) clears it.
- pergent's phrase, verbatim: `(raise --max-turns to allow more steps)`. Replacements, verbatim: dashboard repository `(raise Max steps in the repository's Settings to allow more steps)`; config repository `(raise max_turns for this repository in pergent.sh's config to allow more steps)`.
- Migration `0016` adds nullable columns only, so 0.5.3 keeps working on a migrated database.
- Public repository: no internal company hostnames or names in code, tests or docs (the `gopaytech` module path is fine).
- Match the surrounding code's comment density and wording style; follow the Job timeout's code at every layer.
- Store, engine and dashboard tests that need Postgres skip without `PERGENT_SH_TEST_DATABASE_URL`; they run in CI. Write them anyway.

## Review Focus

1. A `max_turns` stored outside 5–200 (written by hand into the database) is ignored and the default applies; a bad setting never stops reviews. Test in Task 3.
2. A repository switched off between dispatch and publish (`settings.Get` returns `ok == false`) gets the dashboard advice, never pergent's. Test in Task 5.
3. A re-run merged with a base result from before the upgrade still carries pergent's phrase in the kept skills; the rewrite runs after the merge and fixes those too. Test in Task 5.
4. A result that doesn't parse, is `status: "error"`, or holds no phrase is left byte for byte as it is and the publish goes on. Test in Task 5.
5. `maxTurns` sent as `40.5`, `"40"`, `4` or `201` is 400 and nothing changes; `null` resets. Test in Task 6.

---

### Task 1: Config keys

**Files:**
- Modify: `internal/config/config.go` (`Repository`, `Jobs`, `fileRepository`, `fileJobs`, defaults, `Validate`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Jobs.MaxTurns int` (40 when unset); `config.Repository.MaxTurns *int`; exported `config.MinMaxTurns = 5`, `config.MaxMaxTurns = 200`, `config.DefaultMaxTurns = 40` (next to `MinRerunLimit`).

- [ ] **Step 1: Write failing tests** in `config_test.go`, in the style of the `job_timeout` tests:
  - `TestMaxTurnsDefault`: a config without `max_turns` loads with `cfg.Jobs.MaxTurns == 40` and every repository's `MaxTurns == nil`.
  - `TestMaxTurnsSet`: `jobs.max_turns: 60` and one repository with `max_turns: 120` load as 60 and `*MaxTurns == 120`.
  - `TestMaxTurnsOutOfRange`: table over `jobs.max_turns` 4, 201, 0 and `repositories[0].max_turns` 4, 201; the error contains, respectively, `jobs.max_turns must be from 5 to 200, got 4` and `repositories[0] (<project>): max_turns must be from 5 to 200, got 201`. 5 and 200 load.
- [ ] **Step 2:** `go test ./internal/config/ -run MaxTurns` → FAIL (unknown field `max_turns`, strict parsing).
- [ ] **Step 3: Implement.** `MaxTurns *int \`yaml:"max_turns"\`` on `fileRepository` and `fileJobs`; copied as the timeout is; `Validate` adds the two messages above beside the `job_timeout` and `jobs.timeout` checks. Doc comments: `Jobs.MaxTurns` is "pergent's step limit per opencode run (`--max-turns`)".
- [ ] **Step 4:** `go test ./internal/config/` → PASS.
- [ ] **Step 5: Commit** `feat(config): jobs.max_turns and repositories[].max_turns`.

### Task 2: Storage (migration 0016)

**Files:**
- Create: `internal/store/migrations/0016_max_turns.sql`
- Modify: `internal/store/dashboard.go` (`DashboardRepo`, its column list, insert/upsert/update), `internal/store/instances.go` (`RepoDefaults`, read and upsert), `internal/store/runs.go` (`Run`, its select list, `SetRunInputs`)
- Modify: `internal/engine/dispatch.go:417` (call site only, see Step 3)
- Test: `internal/store/dashboard_test.go`, `internal/store/instances_test.go`, `internal/store/runs_test.go`

**Interfaces:**
- Produces: `store.DashboardRepo.MaxTurns *int`, `store.RepoDefaults.MaxTurns *int`, `store.Run.MaxTurns *int` (nil before the upgrade), and
  `func (s *Store) SetRunInputs(ctx context.Context, id, model string, maxTurns int, sources map[string]string, versions map[string]int) error`.

- [ ] **Step 1: Write failing tests** next to the `JobTimeoutSeconds` round trips: a `DashboardRepo` and `RepoDefaults` saved with `MaxTurns = ptr(60)` read back 60 and with nil read back nil; `SetRunInputs(…, 40, …)` then `Run` gives `*MaxTurns == 40`; a run never given inputs has `MaxTurns == nil`; updating a dashboard row with `MaxTurns = ptr(201)` is an error (the CHECK).
- [ ] **Step 2:** with Postgres, `go test ./internal/store/ -run 'MaxTurns|RunInputs'` → FAIL; without it, `go build ./...` fails on the new fields.
- [ ] **Step 3: Implement.** Migration, with a comment in the style of 0015:
  ```sql
  ALTER TABLE dashboard_repos ADD COLUMN max_turns integer CHECK (max_turns BETWEEN 5 AND 200);
  ALTER TABLE repo_defaults   ADD COLUMN max_turns integer CHECK (max_turns BETWEEN 5 AND 200);
  ALTER TABLE runs            ADD COLUMN max_turns integer;
  ```
  Add `max_turns` to every query that lists `job_timeout_seconds`, and to the run select and `SetRunInputs`'s UPDATE. The one caller, `dispatch.go:417`, passes `0` for now (`Effective.MaxTurns` comes in Task 3); Task 4 replaces it. `SetRunInputs` stores NULL for `maxTurns <= 0`.
- [ ] **Step 4:** `go build ./... && go vet ./...` clean; store tests PASS where Postgres is available.
- [ ] **Step 5: Commit** `feat(store): max_turns columns (migration 0016)`.

### Task 3: Effective setting

**Files:**
- Modify: `internal/settings/settings.go`
- Test: `internal/settings/settings_test.go`

**Interfaces:**
- Consumes: Task 1 `Jobs.MaxTurns`, `Repository.MaxTurns`, `MinMaxTurns`, `MaxMaxTurns`; Task 2 `DashboardRepo.MaxTurns`, `RepoDefaults.MaxTurns`.
- Produces: `settings.KeyMaxTurns = "max_turns"`; `settings.Effective.MaxTurns int`; `Overridden[KeyMaxTurns]` always present.

- [ ] **Step 1: Write failing tests** beside the Job timeout ones:
  - config repository without `max_turns` → `MaxTurns == cfg.Jobs.MaxTurns`, not overridden; with `max_turns: 120` → 120, overridden.
  - dashboard repository: no row value and no stored default → `jobs.max_turns`; stored default 80 → 80, not overridden; row 120 → 120, overridden.
  - stored values 4 and 201, in the row and in the defaults, are ignored (Review Focus 1).
  - a config repository ignores the dashboard's stored default.
- [ ] **Step 2:** `go test ./internal/settings/` → FAIL.
- [ ] **Step 3: Implement** in `defaultsFor`, `fromConfig` and `fromDashboard`, as `JobTimeout` is; a stored value counts only when it is from `config.MinMaxTurns` to `config.MaxMaxTurns`. Add "a step limit outside 5 to 200" to `defaultsFor`'s list of values that can't be used.
- [ ] **Step 4:** `go test ./internal/settings/` → PASS.
- [ ] **Step 5: Commit** `feat(settings): max_turns per repository`.

### Task 4: Dispatch passes and records it

**Files:**
- Modify: `internal/kube/job.go` (`JobParams`, `BuildJob` args), `internal/engine/dispatch.go` (params, `SetRunInputs` call)
- Test: `internal/kube/job_test.go`, `internal/engine/dispatch_test.go`

**Interfaces:**
- Consumes: Task 3 `Effective.MaxTurns`; Task 2 `SetRunInputs(ctx, id, model, maxTurns, sources, versions)`.
- Produces: `kube.JobParams.MaxTurns int`; the main container's args hold `--max-turns=<N>` right after `--concurrency=`.

- [ ] **Step 1: Write failing tests:** in `job_test.go`, `BuildJob` with `MaxTurns: 40` has `--max-turns=40` in the main container's args, after `--concurrency=…` and before `--max-comment-size=…`. In `dispatch_test.go`, beside the timeout assertion, a repository with `max_turns: 120` launches a Job whose params have `MaxTurns == 120` and the run reads back `*MaxTurns == 120`.
- [ ] **Step 2:** `go test ./internal/kube/` → FAIL.
- [ ] **Step 3: Implement.** `"--max-turns="+strconv.Itoa(p.MaxTurns)`; dispatch sets `MaxTurns: repo.MaxTurns` and passes it to `SetRunInputs`, replacing Task 2's `0`. Update the `SetRunInputs` log line to "couldn't record the review's model, step limit and skills for the dashboard".
- [ ] **Step 4:** `go test ./internal/kube/ ./internal/engine/` → PASS (engine DB tests skip locally).
- [ ] **Step 5: Commit** `feat(dispatch): pass --max-turns and record it on the run`.

### Task 5: Step-limit advice

**Files:**
- Create: `internal/result/advice.go`, `internal/result/advice_test.go`
- Modify: `internal/engine/publish.go` (`publish`), `internal/store/runs.go` (`StoreMergedResult` doc comment only)
- Test: `internal/engine/publish_test.go`

**Interfaces:**
- Produces:
  - `const result.StepLimitAdvice = "(raise --max-turns to allow more steps)"`, `result.AdviceDashboard`, `result.AdviceConfig` (the two Global Constraints replacements).
  - `func result.ReplaceStepAdvice(data []byte, advice string) (out []byte, changed bool)`: replaces every occurrence of `StepLimitAdvice` in each skill's `failures[].message`, `failures[].cause.message`, `notes[]` and `files.not_reviewed[].cause`; leaves every other byte of meaning alone (work on `map[string]json.RawMessage`, as `Merge` and `NoteUnmerged` do). Data that doesn't decode, or has nothing to replace, is returned unchanged with `changed == false`.

- [ ] **Step 1: Write failing tests** in `advice_test.go`:
  - `TestStepAdvicePhrase`: `StepLimitAdvice == "(raise --max-turns to allow more steps)"`, with a comment that this is pergent's text in `internal/runner/outcome.go` and the two must change together.
  - `TestReplaceStepAdvice`: a completed result with the phrase in a failure message, its cause message, a note "Verification failed: Reached the 40-step limit … (raise --max-turns to allow more steps)." and a not-reviewed cause; after `ReplaceStepAdvice(data, AdviceDashboard)` all four hold `AdviceDashboard`, `changed` is true, a finding whose explanation quotes the phrase is untouched, and `Parse` of the output still succeeds with every other field equal.
  - `TestReplaceStepAdviceUnchanged` (Review Focus 4): invalid JSON, a `status: "error"` result and a result without the phrase each return the input bytes and `changed == false`.
  - Running it twice changes nothing the second time.
- [ ] **Step 2:** `go test ./internal/result/ -run StepAdvice` → FAIL.
- [ ] **Step 3: Implement `ReplaceStepAdvice`.**
- [ ] **Step 4: Wire it into `publish`** right after `mergeBase`: advice is `AdviceConfig` when `ok && repo.Source == settings.SourceConfig`, else `AdviceDashboard` (a repository switched off can only be switched back on in the dashboard). When `changed`, store it with `e.Store.StoreMergedResult` and use it as `run.Result`; a store error returns the error so the publish is retried, as `mergeBase` does. Widen `StoreMergedResult`'s doc comment to say it also stores the result with its step-limit advice replaced. Tests in `publish_test.go` (DB): a dashboard repository's published comment holds `AdviceDashboard` and not the phrase, and so does the stored result; a config repository's holds `AdviceConfig`; a repository switched off gets `AdviceDashboard` (Review Focus 2); a subset re-run whose base result carries the phrase is published with it replaced in the kept skill too (Review Focus 3).
- [ ] **Step 5:** `go test ./internal/result/ ./internal/engine/` → PASS.
- [ ] **Step 6: Commit** `feat: step-limit advice names where to raise the limit`.

> Ruling (plan): the spec says the rewrite happens "when pergent.sh receives and stores the Job's result". The callback stores the raw body inside the token check and has no settings; the publisher, woken right after, already rewrites the stored result once (`mergeBase`) and knows the repository's source. Doing it there keeps one place and covers merged base skills. Cost: for the second or so between the result arriving and the publish, the dashboard can show pergent's phrase.

### Task 6: Dashboard API

**Files:**
- Modify: `internal/dashboard/api_write.go` (`parseSettings`, new `maxTurnsSetting`, `resetSettings`, `msgBadSettings`), `internal/dashboard/api_operator.go` (`defaultsView`, `parseDefaults`, `changedDefaults`, `msgBadDefaults`), `internal/dashboard/api_read.go` (`settingViews`, the run view near line 579), `internal/dashboard/view.go` (`DefaultsView`, run detail view, doc comments)
- Test: `internal/dashboard/api_write_test.go`, `api_operator_test.go`, `api_read_test.go`

**Interfaces:**
- Consumes: Task 3 `Effective.MaxTurns`, `KeyMaxTurns`; Task 2 store fields.
- Produces (JSON): `settings.maxTurns: {value: number, overridden: bool}`; PUT settings key `maxTurns` (number or null); `DefaultsView.maxTurns: number`; PUT defaults key `maxTurns`; run detail `maxTurns: number | null`.

- [ ] **Step 1: Write failing tests** beside the `jobTimeoutSeconds` ones:
  - PUT `{"maxTurns": 120}` → 200 and `settings.maxTurns == {120, true}`; `{"maxTurns": null}` → back to the default, not overridden.
  - Review Focus 5: `40.5`, `"40"`, `4`, `201` → 400 `maxTurns must be a whole number from 5 to 200, or null`, the row unchanged.
  - A maintainer's reset clears `maxTurns`.
  - A config-managed repository → 409, as for the timeout.
  - Admin defaults: GET shows `maxTurns: 40` with nothing stored; PUT `{"maxTurns": 80}` stores it, appears in the audit detail via `changedDefaults`, and a dashboard repository without its own value then shows 80.
  - The run detail view of a run with `MaxTurns = ptr(40)` has `"maxTurns": 40`, and of one without it `"maxTurns": null`.
- [ ] **Step 2:** `go test ./internal/dashboard/` → FAIL (DB tests skip locally; the pure ones fail).
- [ ] **Step 3: Implement** `maxTurnsSetting(raw json.RawMessage, null bool) (func(*store.DashboardRepo), string)` as `timeoutSetting`; add `maxTurns` to both bad-body messages and to every doc comment that lists the settings keys (including the write API's long comment: "maxTurns (a whole number, 5 to 200)").
- [ ] **Step 4:** `go test ./internal/dashboard/ && go vet ./...` → PASS.
- [ ] **Step 5: Commit** `feat(dashboard): Max steps setting and default, step limit on the run`.

### Task 7: Web

**Files:**
- Modify: `web/src/api.ts`, `web/src/pages/RepositorySettings.tsx`, `web/src/pages/AdminSettings.tsx`, `web/src/pages/ReviewDetail.tsx`, `web/src/test/fixtures.ts`
- Test: `web/src/pages/RepositoryDetail.test.tsx` (where the settings page is tested today), `web/src/pages/AdminSettings.test.tsx`, the review page's test file

**Interfaces:**
- Consumes: Task 6 JSON fields.

- [ ] **Step 1: Write failing tests:**
  - Repository settings show a **Max steps** field (aria-label `Max steps`) with the fixture's 40, the help text from Global Constraints, and "default"/"overridden here" as the timeout does. Entering 120 and saving sends `{"maxTurns": 120}`; clearing it sends `{"maxTurns": null}`; entering 4 or `abc` shows "Max steps is a whole number from 5 to 200." and sends nothing.
  - Admin → Settings shows **Max steps** with 40; saving sends `maxTurns` with the other defaults; 201 shows the same problem text.
  - The review page shows `· max steps 40` after the model for a run with `maxTurns: 40`, and `· max steps not recorded` for `maxTurns: null`.
- [ ] **Step 2:** `cd web && npx vitest run` → FAIL.
- [ ] **Step 3: Implement**, copying the Timeout field in each page; fixtures gain `maxTurns: { value: 40, overridden: false }`, `maxTurns: 40` in defaults and `maxTurns: 40` on the run.
- [ ] **Step 4:** `cd web && npx vitest run && npm run typecheck && npm run lint` → PASS.
- [ ] **Step 5: Commit** `feat(web): Max steps setting and step limit on the review page`.

### Task 8: README

**Files:**
- Modify: `README.md`

- [ ] **Step 1:** Add the rows to the config reference: `repositories[].max_turns` (default `jobs.max_turns`; "pergent's step limit per opencode run (`--max-turns`) for this repository, 5 to 200.") and `jobs.max_turns` (default `40`; same text, for every repository). Add "Max steps" (5 to 200) next to "Job timeout and memory" in the role table (line ~533), the repository Settings list (~562), the Admin → Settings description (~565) and the Defaults paragraph (~582); add `max_turns` to the list of config keys (~587).
- [ ] **Step 2:** Add `### Upgrading to 0.5.4` above `### Upgrading to 0.5.3` with the three points from the spec's Release section, verbatim in meaning: the default rises from 20 to 40 steps; migration `0016` runs at startup; `jobs.timeout` stays 10 minutes and a repository that raises `max_turns` may need a higher `job_timeout`; to roll back to 0.5.3, first remove `jobs.max_turns` and `repositories[].max_turns`, because 0.5.3 refuses unknown config keys.
- [ ] **Step 3:** If the README has a generated part (`make readme`), run it; then `go test ./...` → PASS.
- [ ] **Step 4: Commit** `docs: max_turns`.

## After the plan (with the user's permission at each step)

Release pergent.sh 0.5.4 the usual way (Chart.yaml version/appVersion and `image.tag`, `helm-docs -c ./charts/pergent-sh`, `make helm.create.releases`, push, tag, GitHub release), then bump community-ops to 0.5.4.
