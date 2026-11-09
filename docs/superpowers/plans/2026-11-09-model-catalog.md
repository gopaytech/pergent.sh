# Model List, Default and Per-Repository Model Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Admins keep a list of models (provider, model, base URL, key), one default, and can pick a model per repository; every review records and shows the provider and model it ran on.

**Architecture:** A new `llm_models` table holds the list, with the old `llm_settings` row migrated in and kept in step with the default for rollback. `instances.Registry` loads the whole list and resolves a repository's model at dispatch. `dashboard_repos.model_id` holds a repository's choice, NULL meaning "follow the default".

**Tech Stack:** Go, Postgres (pgx), React 18 + react-query + vitest.

**Spec:** `docs/superpowers/specs/2026-11-09-model-catalog-design.md`

## Global Constraints

- Never start a container (no `docker`, `orbctl`). Never push, merge, tag or release. Store, dashboard and engine tests skip without `PERGENT_SH_TEST_DATABASE_URL`; write them anyway, they must compile, and say so in the report.
- Commit with `git -c user.email=zufardhiyaulhaq@gmail.com commit`, ending with the `Co-Authored-By` line your session gives you. Before each commit: `gofmt -l .` empty, `go vet ./...`, `go test ./...`, and `git grep -niE 'golabs|gojek|gopay[^t]|goto\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'` prints nothing.
- Break new code on purpose once per task where a test can run without a database, and record which test failed.
- Migration only adds. The previous release must keep working on the migrated database.
- A key is sealed with `PERGENT_SH_SECRET_KEY`, never returned by the API, never logged. A Job receives only the key of the model it runs.
- Non-admins never change a repository's model, by page or API. They may see a model's provider and name, never its key or base URL.
- The review comment does not change, and never names a model or provider.

## Review Focus

- A key replaced or added since start must still be redacted: every entry's key, and the replaced one, stays in `LLMKeys` and `HeldSecrets` (test in Task 2).
- A repository saved with a model at the moment that model is deleted must not end up pointing at nothing (test in Task 1).
- A maintainer's save carrying `model` next to other keys is refused whole, not partly applied (test in Task 5).
- Rotating a key, editing a base URL, or the upgrade itself must not trigger full reviews (tests in Task 4).
- A failed review's reason, which reaches the PR comment, must contain no provider, model or base URL (test in Task 4).

---

### Task 1: The list in the database

**Files:**
- Create: `internal/store/migrations/0033_llm_models.sql`, `internal/store/models.go`, `internal/store/models_test.go`
- Modify: `internal/store/dashboard.go`, `internal/store/runs.go`

**Interfaces:**
- Produces:
  - `type LLMModel struct { ID, Provider, Model, BaseURL string; APIKeySealed []byte; Default bool; CreatedAt, UpdatedAt time.Time }`
  - `(s *Store) LLMModels(ctx) ([]LLMModel, error)`, oldest first
  - `(s *Store) CreateLLMModel(ctx, m LLMModel, at time.Time) (LLMModel, error)`: returns `ErrExists` for a duplicate; the first entry becomes the default
  - `(s *Store) UpdateLLMModel(ctx, id, baseURL string, keySealed []byte, at time.Time) (bool, error)`: nil key keeps the stored one
  - `(s *Store) SetDefaultLLMModel(ctx, id string, at time.Time) (bool, error)`
  - `(s *Store) DeleteLLMModel(ctx, id string) error`: `*ModelInUseError{Repos []RepoRef}` when used; `ErrModelIsDefault` for the default
  - `DashboardRepo.ModelID *string`; `Run.Provider *string`
  - `(s *Store) SetRunModel(ctx, runID, provider, model string) error`, written before the Job starts, like `SetRunQuiet`

- [ ] **Step 1: Write the migration**

Adds, in one file: `llm_models` (`id text PRIMARY KEY`, `provider`, `model`, `base_url text NOT NULL DEFAULT ''`, `api_key_sealed bytea`, `is_default boolean NOT NULL DEFAULT false`, `created_at`, `updated_at`), `UNIQUE (provider, model, base_url)`, a partial unique index on `is_default WHERE is_default`; `dashboard_repos.model_id text`; `runs.provider text`; `run_usage.provider text NOT NULL DEFAULT ''`. Then `INSERT ... SELECT gen_random_uuid()::text, provider, model, base_url, api_key_sealed, true, updated_at, updated_at FROM llm_settings`. Header comment says it only adds, as `0032` does.

- [ ] **Step 2: Write the failing store tests** in `models_test.go`

`TestMigrationCopiesLLMSettings` (`newStoreAt(t, 32)`, a row in `llm_settings`, `applyUpTo(t, s, 33)`: one default entry, same sealed bytes). `TestCreateLLMModelFirstIsDefault`, `TestCreateLLMModelDuplicate` (`ErrExists`). `TestUpdateLLMModelKeepsKeyOnNil`. `TestSetDefaultWritesLLMSettings` and `TestUpdateDefaultWritesLLMSettings`: `llm_settings` matches the default entry, for rollback. `TestDeleteLLMModelInUse` names the repositories. `TestDeleteDefaultRefused`. `TestSavingARepoLocksItsModel`: `UpdateDashboardRepo` setting a `ModelID` that doesn't exist fails, and holds the model row `FOR KEY SHARE` while saving, as `lockNamedSkills` does.

- [ ] **Step 3: Run:** `go vet ./internal/store/`. Expected: undefined `LLMModel`.

- [ ] **Step 4: Implement `models.go`**, and add `ModelID` to `DashboardRepo` (`dashboardRepoColumns`, `scanDashboardRepo`, all three upsert/insert/update statements) and `Provider` to `Run` (`runColumns`, `scanRun`). `DeleteLLMModel` follows `DeleteLibrarySkill`: lock the row `FOR UPDATE`, list users, refuse. Writing `llm_settings` for the default uses `PutLLM`'s statement inside the same transaction.

- [ ] **Step 5: Run:** `go vet ./... && go test ./internal/store/`. Expected: compiles, tests skip. **Commit** `feat(store): a list of LLM models`.

### Task 2: The registry knows the whole list

**Files:**
- Modify: `internal/instances/instances.go`
- Test: `internal/instances/instances_test.go`

**Interfaces:**
- Consumes: `Store.LLMModels`.
- Produces:
  - `const ConfigModelID = "config"`
  - `type Model struct { ID string; LLM config.LLM; Key string; Default bool; Source Source }`
  - `(r *Registry) Models() []Model`, config entry first
  - `(r *Registry) DefaultModel() (Model, bool)`: the config entry whenever `cfg.HasLLM()`; `ok` false without a usable key
  - `(r *Registry) ModelByID(id string) (Model, bool)`: `ok` false when missing or its key can't be opened
  - `LLM()` and `LLMStatus()` keep their signatures, reading the default

- [ ] **Step 1: Write the failing tests**

`TestModelsFromConfigIsReadOnlyDefault`: with `cfg.LLM` set and a nil store, `Models()` is one entry with ID `config`, `Source == SourceConfig`, `Default`. `TestModelByIDMissing`. `TestLLMKeysCoverEveryEntry` (needs the store): two entries, then replace one key and `LoadLLM` again; `LLMKeys()` holds all three, and `Redact` hides each. `TestUnopenableKeyIsNotUsable`: a sealed key under another box gives `ModelByID` `ok == false`.

- [ ] **Step 2: Run:** `go test ./internal/instances/`. Expected: compile failure.

- [ ] **Step 3: Implement.** `LoadLLM` reads `LLMModels`, opens each key, calls `noteKey` on every one, and swaps the list under `llmMu`. A key that can't be opened warns, as today, and counts as not set.

- [ ] **Step 4: Run** `go test ./internal/instances/`. Expected: pass. **Commit** `feat(instances): load every model in the list`.

### Task 3: A repository's model setting

**Files:**
- Modify: `internal/settings/settings.go`
- Test: `internal/settings/settings_test.go`

**Interfaces:**
- Produces: `KeyModel = "model"`; `Effective.ModelID string`, empty meaning follow the default.

- [ ] **Step 1: Write the failing test** `TestModelIDFromDashboard`: a row with `ModelID` sets `eff.ModelID` and `Overridden[KeyModel]`; a config-listed repository has an empty `ModelID`.
- [ ] **Step 2: Run** `go vet ./internal/settings/`. Expected: undefined.
- [ ] **Step 3: Implement** in `defaultsFor` and `fromDashboard`. `fromConfig` is untouched.
- [ ] **Step 4: Run** `go test ./internal/settings/`. **Commit** `feat(settings): a repository can name a model`.

### Task 4: A review runs on its repository's model

**Files:**
- Modify: `internal/engine/dispatch.go`, `internal/engine/incremental.go`
- Test: `internal/engine/incremental_test.go`, `internal/engine/instances_test.go`, `internal/engine/dispatch_test.go`

**Interfaces:**
- Consumes: `Registry.ModelByID`, `Registry.DefaultModel`, `Effective.ModelID`, `Store.SetRunModel`.
- Produces: `modelChanged(last store.Run, provider, model string) bool`; `reasonModelUnusable`; `fullModelChanged`.

- [ ] **Step 1: Write the failing tests**

No database, in `incremental_test.go`:

```go
func TestModelChanged(t *testing.T) {
	p, m, other := "anthropic", "claude-a", "claude-b"
	for name, tc := range map[string]struct {
		last           store.Run
		provider, model string
		want           bool
	}{
		"same":                {store.Run{Provider: &p, Model: &m}, p, m, false},
		"other model":         {store.Run{Provider: &p, Model: &m}, p, other, true},
		"other provider":      {store.Run{Provider: &p, Model: &m}, "litellm", m, true},
		"upgrade, same model": {store.Run{Model: &m}, "litellm", m, false},
		"upgrade, other":      {store.Run{Model: &m}, p, other, true},
		"never recorded":      {store.Run{}, p, m, false},
		"nothing resolved":    {store.Run{Provider: &p, Model: &m}, "", "", false},
	} { /* t.Run: modelChanged(tc.last, tc.provider, tc.model) == tc.want */ }
}
```

`TestPlanCarriedFullWhenModelChanged` with `newPlanFixture`. With the database: `TestDispatchUsesTheRepositorysModel` (Job env `OPENCODE_MODEL`, Secret `llm-api-key` is that entry's and no other). `TestDispatchFollowsDefault`. `TestDispatchFailsWhenChosenModelIsGone`: run fails, the failure reason equals `reasonModelUnusable` and contains neither provider, model nor base URL; never falls back. `TestDispatchWaitsWithoutAnyModel`: existing `msgNoLLM` behaviour. `TestRunRecordsProviderBeforeLaunch`.

- [ ] **Step 2: Run** `go test ./internal/engine/ -run 'TestModelChanged|TestPlanCarried'`. Expected: undefined.

- [ ] **Step 3: Implement**

At the gate near `dispatch.go:85`, wait only when `len(Models()) == 0`. Near `:311`, resolve: `repo.ModelID != ""` uses `ModelByID`, and not ok is `d.fail(ctx, reasonModelUnusable)` with the entry ID in `d.log` only; otherwise `DefaultModel`, and not ok is today's `postpone`. Call `SetRunModel` before launch and return its error through `d.retry`. `SetRunInputs` stays. Add the resolved provider and model to `dispatcher`; zero values mean no change. Add a `modelChanged` case to the switch in `planCarried`, returning `fullModelChanged`.

`reasonModelUnusable = "the model chosen for this repository can't be used: an admin can change it on the repository's Settings tab"`.

- [ ] **Step 4: Run** `go test ./internal/engine/`. Expected: pass or skip. **Commit** `feat(engine): run a review on its repository's model`.

### Task 5: The API

**Files:**
- Create: `internal/dashboard/api_models.go`, `internal/dashboard/api_models_test.go`
- Modify: `internal/dashboard/api_operator.go`, `api_write.go`, `api_read.go`, `view.go`, `openapi_test.go`, `openapi.yaml`

**Interfaces:**
- Consumes: Task 1's store methods, `Registry.Models`, `d.loadLLM`.
- Produces, all through `adminRoute`:
  - `GET /api/admin/models` → `ModelsAdminView{Editable bool; FollowingDefault int; Models []ModelAdminView}`, with `ModelAdminView{ID, Provider, Model, BaseURL, Source string; Default bool; Key SecretView; Repositories []RepoRefView}`
  - `POST /api/admin/models` `{provider, model, baseURL, apiKey}` → 201; duplicate 409
  - `PUT /api/admin/models/{id}` `{baseURL, apiKey?}`; `provider` or `model` in the body is a 400
  - `POST /api/admin/models/{id}/default`; 409 while a config `llm:` exists
  - `DELETE /api/admin/models/{id}`; 409 `ModelInUseView{Error, Repositories}`, or for the default
  - Repository setting `model`: an entry ID, or null. `RepositoryDetailView` gains `Model string` (the effective `provider / model`) and `ModelChoices []ModelChoiceView{ID, Label}`, empty for non-admins.
- Removed: `GET` and `PUT /api/admin/llm`, `LLMAdminView`, `PutLLMRequest`.

- [ ] **Step 1: Write the failing tests** in `api_models_test.go`

Admin CRUD happy paths; first entry is default; `TestModelsNeverReturnAKey` (the raw key isn't in any response body or log); non-admin 404 with the unknown-path body; config entry can't be edited, removed or replaced as default; `TestSetDefaultReportsFollowers`; `TestRemoveInUseNamesRepositories`. Audit entries `model_added`, `model_updated`, `model_default_changed`, `model_removed` carry provider and model, never the key. In the settings tests: `TestMaintainerCannotSetModel` sends `{"model": id, "approve": false}` as a maintainer, expects 403 `msgNeedsAdminModel`, and nothing saved; `TestAdminSetsAndClearsModel`; `TestUnknownModelIs400`; `TestResetByMaintainerKeepsModel`; `TestViewerSeesModelNameNotBaseURL`.

- [ ] **Step 2: Run** `go vet ./internal/dashboard/`. Expected: undefined.

- [ ] **Step 3: Implement.** Field checks as in today's `putLLM` (`maxLLMField`, `maxSecretBytes`, base URL rules). Add `isModelKey` and a gate beside `isTrustKey`, `msgNeedsAdminModel = "only admins of this repository change its model"`, the `model` case in `parseSettings`, and add the key to `msgBadSettings`. `resetSettings` clears it only in its admin branch. The settings audit detail names the provider and model. `adminSettings` and the setup checklist read the default entry.

- [ ] **Step 4: OpenAPI.** Update `openapi.yaml` and `openAPISchemas`. Run `go test ./internal/dashboard/ -run TestOpenAPI`. Expected: pass.

- [ ] **Step 5: Run** `go vet ./... && go test ./...`. **Commit** `feat(dashboard): API for the model list and a repository's model`.

### Task 6: Provider in Usage and the CSV

**Files:**
- Modify: `internal/store/usage.go`, `internal/store/usage_test.go`, `internal/dashboard/api_usage.go`, `api_usage_test.go`, `view.go`, `openapi.yaml`

**Interfaces:**
- Produces: `MRUsage.Models []ModelUsed{Provider, Model string}`; `UsageMRView.Models []UsageModelView{Provider, Model string}`; `UsageNamedView.Provider`; CSV column `provider`, appended last so existing positions hold.

- [ ] **Step 1: Write the failing tests.** `TestRecordUsageCopiesProvider`. `TestUsageByMRListsEachModel`: a merge request with reviews on two models lists both, in a stable order; a run with an empty model lists nothing for it. `TestUsageCSVHasProviderLast`.
- [ ] **Step 2: Run** `go vet ./...`. Expected: undefined.
- [ ] **Step 3: Implement.** `RecordUsage` copies `COALESCE(r.provider, '')`. Fetch the models with a second grouped query keyed by `mr_iid`, joined in Go. `MetricsTotals` is untouched: no new label.
- [ ] **Step 4: Run** `go test ./...`. **Commit** `feat(usage): show which models a merge request used`.

### Task 7: The pages

**Files:**
- Modify: `web/src/api.ts`, `web/src/pages/AdminSettings.tsx`, `AdminSettings.test.tsx`, `RepositorySettings.tsx`, `RepositorySettings.test.tsx`, `UsageRepository.tsx`, `Usage.test.tsx`, `web/src/test/fixtures.ts`

**Interfaces:**
- Consumes: Task 5's and 6's JSON, mirrored in `api.ts`.

- [ ] **Step 1: Write the failing tests** (`fakeApi`/`renderApp`)

Admin Settings: the list shows provider, model, default, and each entry's repositories; adding posts `{provider, model, baseURL, apiKey}`; provider and model aren't editable on an existing entry; "set as default" asks for confirmation naming `followingDefault`; remove shows the 409's repositories; a config entry has no controls. Repository Settings: admin sees a select with "Default (provider / model)" first and sends `{model: id}` or `{model: null}`; maintainer's select is disabled, shows the current model, and its save never carries `model`. Usage: each merge request row shows its models. A provider or model containing `<img>` renders as text.

- [ ] **Step 2: Run** `cd web && npx vitest run src/pages/AdminSettings.test.tsx src/pages/RepositorySettings.test.tsx src/pages/Usage.test.tsx`. Expected: FAIL.
- [ ] **Step 3: Implement.** Replace `ModelForm`/`ModelView` with the list, reusing the `providers` choices and `secretFill`. Imitate the trusted-author control and its `canSetTrust` for `canSetModel`. Add the model to the non-admin reset wording. No inline `style=` (CSP).
- [ ] **Step 4: Run** `npm run typecheck && npm run lint && npx vitest run`. Expected: pass. **Commit** `feat(web): manage the model list and a repository's model`.

### Task 8: Docs

**Files:**
- Create: `docs/concepts/models.md`
- Modify: `docs/README.md`, `docs/upgrading.md`, `docs/concepts/usage.md`, `docs/reference/http-routes.md`, `docs/reference/configuration.md`

- [ ] **Step 1: Write `models.md`**, short sections like `usage.md`: the list, the default, a repository's model, what happens when a model can't be used, and that a new provider's address must be inside `jobEgress.cidrs`.
- [ ] **Step 2: Upgrading notes**, a new "Upgrading to the next release": the existing setup becomes the first entry and default, no full reviews follow; migration `0033` only adds; a rollback runs every repository on the current default, and choices per repository don't exist there. `GET`/`PUT /api/admin/llm` are replaced.
- [ ] **Step 3: Update** the index, routes table, config reference for `llm:` (read-only entry `config`), and Usage's CSV column list.
- [ ] **Step 4: Check every statement against the code**, run the full checks from Global Constraints, `cd web && npm run typecheck`. **Commit** `docs: models`.
