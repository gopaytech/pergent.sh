# Context window and max output per model entry (pergent.sh) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every model entry (global, team, config file) can hold a context window and a max output, and every Job that runs the entry gets them as `PERGENT_CONTEXT_LIMIT` and `PERGENT_OUTPUT_LIMIT`.

**Architecture:** The 2 values ride on `config.LLM` (2 new fields, `0` = empty), so they travel with the entry the same way provider, model and base URL do: store row or config file → `instances.Model.LLM` → chain resolution → `kube.JobParams.LLM` / `kube.SecurityJobParams.LLM` → env. One range check in `internal/config` serves the config loader and the dashboard API. The pergent CLI side (`pergent scan` reading the context limit, the new output limit, the 40,000 batch cap, the `output-limit` reason) is a separate plan in the pergent repository.

**Tech Stack:** Go 1.24, pgx v5, Postgres 13+, yaml.v3, client-go types; React 18 + TanStack Query + vitest.

**Spec:** `docs/superpowers/specs/2026-11-28-model-limits-design.md`

## Global Constraints

- **Names.** UI: **Context window** and **Max output**. API: `contextWindow`, `maxOutput` (integer or `null`). DB: `context_window`, `max_output` (nullable `integer`) on `llm_models` and `team_models`. Config: `llm.context_window`, `llm.max_output`, and the same keys in each `llm.fallbacks` item. Job env: `PERGENT_CONTEXT_LIMIT`, `PERGENT_OUTPUT_LIMIT`.
- **Never call them "limits"** in anything a person reads (UI, API messages, docs, audit text). pergent.sh already has model limits: monthly token limits, migration `0051`, the Limits UI. Code identifiers say "window" instead (`TokenCount`, `SetLLMModelWindow`, `windowBody`, `windowEnv`).
- **Ranges.** Context window 4,096 to 10,000,000. Max output 1,024 to the context window when that is set, else to 10,000,000. Whole numbers only. Empty is `null` in the API, `NULL` in the DB, an absent key in the config file, `0` in Go.
- A refused value is a **400 with the range in the message**. The config loader refuses the same values and names the key.
- **Who edits:** whoever may edit the entry's base URL and key (admins for global entries; admins and the team's maintainers for team entries; nobody for config entries, which stay 409 `msgConfigModel`).
- **Audit:** a change goes into the existing `model_updated` / `team_model_updated` entry, with old and new values.
- **Jobs:** each var only when its value is set. An entry with both empty gives a Job byte for byte as today (`TestBuildJobUnchanged` stays green with no golden update).
- **Help lines, exact copy.** Context window: `Leave empty for models opencode knows. Set them for a model id it does not know: the context window from the provider's page.` Max output: `The most one reply may need, thinking included. 64,000 is enough for reviews and scans. A provider that counts it against a rate limit throttles a higher value.`
- **Lists** show `1M / 64k` (the existing `count()` in `web/src/ui.tsx`), and nothing when both are empty.
- **Migration** `0056_model_limits_per_entry.sql` (0056 is free on `origin/main` and every open branch). It only adds; the previous release keeps working and ignores the columns.
- **No release in this plan:** no default image bump, no chart or version change, no tag. Upgrade notes go under `## Unreleased` in `docs/upgrading.md`.
- **Tests, locally:** only `go test ./<pkg> -count=1 -run '^(TestA|TestB)$'` with exact names, `go vet` per changed package, and `npx vitest run <file>` per changed web test file. Never a whole package, `./...`, `make test` or a bare `npx vitest run`. Full runs are CI's `test` and `web` checks.
- **DB tests** (store, instances DB tests, engine, dashboard handlers) skip without `PERGENT_SH_TEST_DATABASE_URL`. Run one focused only when the already-running Postgres answers on port 55432 (`nc -z localhost 55432`); put the `export` in the same command as `go test`. Otherwise leave it to CI. **Never start Postgres, docker or any container.** When a DB test cannot run, its "fails first" proof is the compile error from `go vet ./<pkg>/`.
- **Fast tests:** unit < 100 ms, DB < 2 s, web < 500 ms. One setup per DB test with `t.Run` subtests, `t.Parallel()` unless the test sets a package variable or env (migration tests with `newStoreAt` stay serial). No sleeps, no real clock.
- **Public repo:** no employer, product, customer, cloud or internal names, no local paths, no emails, in code, tests, docs or commits. Before every commit run the private-name scan and the email / local-path grep from the agent memory; both must print nothing. Commit as the owner's personal address, conventional subjects, with the `Co-Authored-By` trailer of the model that wrote the change.
- `web/` has no prettier: format by hand, eslint is the gate. If `web/node_modules` is missing in the worktree, clone it in with `cp -Rc` from another checkout (never a symlink).

## Review Focus

1. **The pair is judged after the change, not field by field.** A PUT that lowers only `contextWindow` below the stored `maxOutput`, or raises only `maxOutput` above the stored `contextWindow`, is a 400; a PUT that clears `contextWindow` while `maxOutput` stays at 2,000,000 is fine. Pinned in Task 5 `TestWindowBody`.
2. **Absent keeps, null clears.** A PUT with no window fields (an older web, the team PUT with only a name) must keep the stored values; only an explicit `null` empties one. Pinned in Task 5 `TestWindowBody` and `TestModelWindowAdmin`.
3. **Number forms the JSON decoder would bend.** `1e6`, `1000000.0`, `"1000000"`, `-1`, `0` and `9999999999999999999` are 400, never rounded or wrapped. Pinned in Task 5 `TestWindowBody`.
4. **The edit form must not touch values it did not change.** Opening Edit on an entry with `1M / 64k` and saving only a new name sends no window field; emptying a field sends `null`. Pinned in Task 6.
5. **Config forms that yaml.v3 would bend.** `context_window: 0`, `1.5e6`, `64000.5` and `"1M"` are refused at load with the key named, in the main entry and in a fallback item (`llm.fallbacks[1].max_output ...`). Pinned in Task 1 `TestConfigModelWindow`.

---

### Task 1: Config keys and the shared range check

**Files:**
- Modify: `internal/config/config.go` (type `LLM` at :357, `checkFallbacks` at :589, the `llm:` checks in `Validate` at :1044)
- Test: `internal/config/config_test.go` (beside `TestConfigFallbacks`)
- Modify: `docs/reference/configuration.md` (the `llm.*` rows at :75-77)

**Interfaces:**
- Produces:
  - `type TokenCount int` with `func (n *TokenCount) UnmarshalYAML(value *yaml.Node) error`.
  - `config.LLM` gains `ContextWindow TokenCount \`yaml:"context_window"\`` and `MaxOutput TokenCount \`yaml:"max_output"\``. `LLM` stays comparable (`HasLLM` compares it to `LLM{}`).
  - `const MinContextWindow = 4096`, `MaxContextWindow = 10_000_000`, `MinMaxOutput = 1024`.
  - `func ValidContextWindow(n int) bool` (4096..10,000,000; `0` is false, callers skip empty).
  - `func MaxOutputCeiling(contextWindow int) int` (the context window when > 0, else `MaxContextWindow`).
  - `func ValidMaxOutput(n, contextWindow int) bool` (1024..`MaxOutputCeiling(contextWindow)`).

- [ ] **Step 1: Write the failing test `TestConfigModelWindow`** (unit, no DB; shape of `TestConfigFallbacks`, same `body` and `main`)

```go
cfg, err := Load(writeConfig(t, body+main+"  context_window: 1000000\n  max_output: 64000\n"+
	"  fallbacks:\n    - provider: openai\n      model: gpt-a\n      api_key_env: K\n      max_output: 128000\n"))
// want: cfg.LLM.ContextWindow == 1_000_000, cfg.LLM.MaxOutput == 64_000,
// cfg.LLMFallbacks[0] == LLM{Provider: "openai", Model: "gpt-a", APIKeyEnv: "K", MaxOutput: 128_000}
for _, tc := range []struct{ yaml, want string }{
	{main + "  context_window: 4095\n", "llm.context_window must be from 4096 to 10000000, got 4095"},
	{main + "  context_window: 10000001\n", "llm.context_window must be from 4096 to 10000000"},
	{main + "  context_window: 0\n", "0 is not a token count"}, // UnmarshalYAML: explicit 0 refused
	{main + "  context_window: -1\n", "llm.context_window"},
	{main + "  context_window: 1.5e6\n", "whole number"},
	{main + "  max_output: 64000.5\n", "whole number"},
	{main + "  max_output: 1M\n", "whole number"},
	{main + "  max_output: 1023\n", "llm.max_output must be from 1024 to 10000000, got 1023"},
	{main + "  context_window: 100000\n  max_output: 100001\n", "llm.max_output must be from 1024 to 100000 (the context window), got 100001"},
	{main + "  fallbacks:\n    - {provider: o, model: a, api_key_env: K}\n    - {provider: o, model: b, api_key_env: K, context_window: 8192, max_output: 9000}\n",
		"llm.fallbacks[1].max_output must be from 1024 to 8192 (the context window), got 9000"},
} { /* Load must fail with an error containing tc.want */ }
// Without the keys nothing changes: cfg.LLM.ContextWindow == 0 && cfg.LLM.MaxOutput == 0.
```

Also a small table `TestModelWindowRanges` for the 3 pure functions: `ValidContextWindow(4096)`, `(10_000_000)` true; `(4095)`, `(0)`, `(10_000_001)` false; `MaxOutputCeiling(0) == 10_000_000`, `MaxOutputCeiling(200_000) == 200_000`; `ValidMaxOutput(1024, 0)`, `(200_000, 200_000)` true; `(1023, 0)`, `(200_001, 200_000)`, `(10_000_001, 0)` false.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/config -count=1 -run '^(TestConfigModelWindow|TestModelWindowRanges)$'`
Expected: build failure, `undefined: ValidContextWindow` / unknown field `ContextWindow`.

- [ ] **Step 3: Implement in `internal/config/config.go`**

`TokenCount.UnmarshalYAML`: require a scalar with tag `!!int` (so `1.5e6`, `64000.5` and `1M` fail with `line %d: must be a whole number of tokens`), decode it, and refuse `0` with `line %d: 0 is not a token count; leave the key out to use opencode's value`. Range checks live in `Validate`, not here, so the message names the key. In `Validate` (main entry, inside the existing `c.HasLLM() || ...` block or right after it) and in `checkFallbacks` (each item, prefix `llm.fallbacks[%d]`), add a helper `checkWindow(prefix string, llm LLM) []error` that emits:
- `%s.context_window must be from 4096 to 10000000, got %d`
- `%s.max_output must be from 1024 to %d (the context window), got %d` when a context window is set, else `%s.max_output must be from 1024 to 10000000, got %d`.

- [ ] **Step 4: Run them and see them pass**

Run: `go test ./internal/config -count=1 -run '^(TestConfigModelWindow|TestModelWindowRanges|TestConfigFallbacks|TestLoadValid)$'`
Expected: PASS. Then `go vet ./internal/config/`.

- [ ] **Step 5: Document the keys** in `docs/reference/configuration.md`: 2 rows after `llm.api_key_env`: `llm.context_window` (none; "The model's context window in tokens, input and output together, from 4096 to 10000000. Leave it out for a model opencode knows. See [Models](../concepts/models.md#context-window-and-max-output).") and `llm.max_output` (none; "The most tokens one reply may need, thinking included, from 1024 to the context window (or 10000000). 64000 is enough for reviews and scans."). The `llm.fallbacks` row adds "and optional `base_url`, `context_window` and `max_output`".

- [ ] **Step 6: Commit** `feat(config): context_window and max_output on llm entries`

---

### Task 2: Migration and store

**Files:**
- Create: `internal/store/migrations/0056_model_limits_per_entry.sql`
- Modify: `internal/store/models.go` (`LLMModel`, `modelColumns`, `scanModel`, `CreateLLMModel`)
- Modify: `internal/store/team_models.go` (`TeamModel`, `teamModelColumns`, `scanTeamModel`, `CreateTeamModel`)
- Create: `internal/store/model_window_test.go`

**Interfaces:**
- Produces:
  - `LLMModel` and `TeamModel` gain `ContextWindow, MaxOutput int` (0 = `NULL`). Create stores them with `NULLIF($n, 0)`; reads use `COALESCE(context_window, 0)`.
  - `func (s *Store) SetLLMModelWindow(ctx context.Context, id string, contextWindow, maxOutput int, at time.Time) (found bool, err error)`
  - `func (s *Store) SetTeamModelWindow(ctx context.Context, teamID int64, id string, contextWindow, maxOutput int, at time.Time) (found bool, err error)`
  - Both write both columns (0 → `NULL`) and `updated_at = at`, as `UpdateLLMModel` / `UpdateTeamModel` do for a base URL. No list lock: `llm_settings` does not change. `UpdateLLMModel` and `UpdateTeamModel` keep their signatures (many test callers).

- [ ] **Step 1: Write the migration** (comment header in the 0051/0054 style: it only adds; the previous release ignores the columns and its inserts leave them `NULL`)

```sql
ALTER TABLE llm_models ADD COLUMN context_window integer CHECK (context_window BETWEEN 4096 AND 10000000);
ALTER TABLE llm_models ADD COLUMN max_output integer CHECK (max_output BETWEEN 1024 AND 10000000);
ALTER TABLE llm_models ADD CONSTRAINT llm_models_max_output_fits CHECK (max_output <= context_window);
-- the same 3 statements for team_models (constraint team_models_max_output_fits)
```

A `NULL` on either side passes each CHECK, so an empty context window leaves max output bounded only by its own range.

- [ ] **Step 2: Write the failing tests** in `internal/store/model_window_test.go`
  - `TestModelWindowMigrationOnlyAdds` (serial, `newStoreAt(t, 55)` then `applyUpTo(t, s, 56)`): the same checks as `TestModelLimitsMigrationOnlyAdds` (no DROP, RENAME, DELETE, UPDATE, ALTER COLUMN, SET NOT NULL in the file; every 0055 column and index unchanged), and the 4 new columns exist with `columnInfo{Type: "integer", Nullable: "YES"}`.
  - `TestModelWindowMigrationPreviousRelease` (parallel, `newStore`): an `INSERT INTO llm_models` and an `INSERT INTO team_models` with the 0055 column lists succeed, and `LLMModels` / `TeamModels` read `ContextWindow == 0 && MaxOutput == 0`.
  - `TestModelWindowRoundTrip` (parallel, one `newStore`, subtests):
    - `global create`: `CreateLLMModel(LLMModel{..., ContextWindow: 1_000_000, MaxOutput: 64_000})` returns and `LLMModels` reads both.
    - `global set and clear`: `SetLLMModelWindow(id, 200_000, 0, at)` → found; reads `200_000, 0`; `SELECT max_output IS NULL` is true; `SetLLMModelWindow(id, 0, 0, at)` reads `0, 0`.
    - `global missing`: `SetLLMModelWindow("no-such-id", 8192, 0, at)` = `false, nil`.
    - `team create, set, clear, missing`: the same 4 checks through `CreateTeamModel`, `SetTeamModelWindow` and `TeamModels`; another team's ID is `found == false`.
    - `check refuses`: a raw `UPDATE llm_models SET context_window = 4095` and `SET context_window = 8192, max_output = 9000` fail with `pgconn.PgError` code `23514`.

- [ ] **Step 3: Watch them fail**

Run: `go vet ./internal/store/`
Expected: `unknown field ContextWindow` / `SetLLMModelWindow undefined`. With Postgres up on 55432, also run the 3 tests focused (with the env var in the same command) and see them fail on the missing columns. Otherwise they skip; leave them to CI.

- [ ] **Step 4: Implement** the fields, columns, scans, create inserts and the 2 setters.

- [ ] **Step 5: See them pass**

Run: `go vet ./internal/store/`, then, only with Postgres up: `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test ./internal/store -count=1 -run '^(TestModelWindowMigrationOnlyAdds|TestModelWindowMigrationPreviousRelease|TestModelWindowRoundTrip|TestTeamModelCRUD|TestCreateLLMModelFirstIsDefault)$'`
Expected: PASS, or skipped (say so in the report).

- [ ] **Step 6: Commit** `feat(store): context window and max output on model entries (migration 0056)`

---

### Task 3: The registry carries the values

**Files:**
- Modify: `internal/instances/instances.go` (`LoadTeamModels` at :766, `loadGlobal` at :809)
- Test: `internal/instances/instances_test.go`

**Interfaces:**
- Consumes: `store.LLMModel.ContextWindow/MaxOutput`, `store.TeamModel.ContextWindow/MaxOutput` (Task 2); `config.LLM.ContextWindow/MaxOutput` of type `config.TokenCount` (Task 1).
- Produces: every `instances.Model.LLM` (from `Models`, `GlobalModels`, `TeamModels`, `ModelByID`, `AllModels`) holds the entry's values. Config entries already copy `r.cfg.LLM` and each fallback whole, so they need no code, only the test.

- [ ] **Step 1: Write the failing tests**
  - `TestModelWindowFromConfig` (unit: `New(cfg, nil, nil)` with `cfg.LLM.ContextWindow = 1_000_000`, `cfg.LLM.MaxOutput = 64_000` and one fallback with `MaxOutput: 32_000`): `GlobalModels()[0].LLM` has `1_000_000, 64_000`; `GlobalModels()[1].LLM` has `0, 32_000`.
  - `TestModelWindowFromStore` (DB, parallel, one `newStore`): a global row (`addModel` then `SetLLMModelWindow(id, 200_000, 16_000)`) and a team row (`CreateTeamModel` with `ContextWindow: 128_000`); after `LoadLLM`, `ModelByID(globalID)` has `200_000, 16_000` and `TeamModels(team)[0].LLM` has `128_000, 0`.

- [ ] **Step 2: Watch them fail**

Run: `go test ./internal/instances -count=1 -run '^(TestModelWindowFromConfig)$'` (passes already if Task 1 is in: config entries copy the struct; that is expected) and `go vet ./internal/instances/`. `TestModelWindowFromStore` fails on the zero values when Postgres is up, else skips.

- [ ] **Step 3: Implement** both `config.LLM{...}` literals with `ContextWindow: config.TokenCount(row.ContextWindow), MaxOutput: config.TokenCount(row.MaxOutput)`.

- [ ] **Step 4: See them pass** (same commands; DB test only with Postgres up).

- [ ] **Step 5: Commit** `feat(instances): carry an entry's context window and max output`

---

### Task 4: Job env, for every Job that runs an entry

**Files:**
- Modify: `internal/kube/job.go` (main container env at :339-345)
- Modify: `internal/kube/security.go` (env at :125-131)
- Test: `internal/kube/job_test.go`, `internal/kube/security_test.go`
- Test: `internal/engine/e2e_fallback_test.go`

**Interfaces:**
- Consumes: `config.LLM.ContextWindow/MaxOutput` (Task 1), filled by the registry (Task 3). The engine passes the resolved entry's `LLM` whole: `dispatch.go:966` (reviews, multi-review reviewers and the judge, whose `LLM` is the judge entry's), `apireview.go:277` (API reviews), `security.go:445` (scans). No engine code changes.
- Produces: `func windowEnv(llm config.LLM) []corev1.EnvVar` in `job.go`: `PERGENT_CONTEXT_LIMIT` and `PERGENT_OUTPUT_LIMIT` as plain decimal strings (`strconv.Itoa`), each only when > 0. Appended right after `OPENCODE_BASE_URL` in `BuildJob`'s main container and in `BuildSecurityJob`. No other container gets them. A comment says why it is env: an older image ignores an env var it does not read.

- [ ] **Step 1: Write the failing kube tests** (unit)
  - `TestBuildJobModelWindow` in `job_test.go`, a table over 3 Jobs: a review (`goldenParams()`), a reviewer (`SkipSkillVerify = true`, `Reviewer = "2"`), a judge (`judgeParams()`), each with 4 cases: none, both (`1_000_000`, `64_000`), context only, output only. Assert on the `pergent` container: exactly one `PERGENT_CONTEXT_LIMIT` = `"1000000"` when set and none when not, the same for `PERGENT_OUTPUT_LIMIT` = `"64000"`; no init container carries either; and for "none", `reflect.DeepEqual(BuildJob(p), BuildJob(base))` where `base` never had the fields touched.
  - `TestSecurityJobModelWindow` in `security_test.go`: the same 4 cases on `testSecurityParams()`, checked on the scan container's env.

- [ ] **Step 2: Watch them fail**

Run: `go test ./internal/kube -count=1 -run '^(TestBuildJobModelWindow|TestSecurityJobModelWindow)$'`
Expected: FAIL, `PERGENT_CONTEXT_LIMIT missing`.

- [ ] **Step 3: Implement `windowEnv`** and the 2 appends.

- [ ] **Step 4: See them pass, and the Job golden unchanged**

Run: `go test ./internal/kube -count=1 -run '^(TestBuildJobModelWindow|TestSecurityJobModelWindow|TestBuildJobUnchanged|TestBuildJobShape|TestBuildSecurityJob|TestBuildJudgeJob|TestBuildReviewerJob)$'`
Expected: PASS, with no golden file written.

- [ ] **Step 5: Write the engine fallback test** `TestE2EFallbackUsesNextEntryWindow` (DB, parallel) in `e2e_fallback_test.go`, with a helper `addTeamModelWindow(t, env, team, name, model, key string, contextWindow, maxOutput int) store.EntryRef` (creates through `env.Store.CreateTeamModel` with the values, then `env.Engine.Instances.LoadLLM`). `e := newE2E(t)`, `teamOwning(t, env, "Payments")`, chain `[team-a (1_000_000, 64_000), team-b (empty)]` saved with `saveChain`, then `servePublish(env, 1, head)` and `env.Enqueue(1, head)`, as in `TestE2EFallbackQuota`.

```go
first := e.attempt(t, "key-team-a", failedOn(t, head, "quota"))
// jobEnv(env, first): PERGENT_CONTEXT_LIMIT == "1000000", PERGENT_OUTPUT_LIMIT == "64000"
second := e.attempt(t, "key-team-b", clean(t, head), first)
// jobEnv(env, second): neither key present
```

A scan never falls back (`engine/security.go`), so there is no scan case here; the kube test covers the scan env.

- [ ] **Step 6: Run it** — `go vet ./internal/engine/`, then with Postgres up only: `go test ./internal/engine -count=1 -run '^(TestE2EFallbackUsesNextEntryWindow|TestE2EFallbackQuota)$'` with the env var in the same command. Else leave it to CI.

- [ ] **Step 7: Commit** `feat(kube): PERGENT_CONTEXT_LIMIT and PERGENT_OUTPUT_LIMIT from the entry a Job runs`

---

### Task 5: Dashboard API, views, audit and openapi

**Files:**
- Create: `internal/dashboard/model_window.go`, `internal/dashboard/model_window_test.go`
- Modify: `internal/dashboard/api_models.go` (`createModel`, `updateModel`, `modelView`, the config entries in `modelsView`, `msgBadModelBody`, `msgBadModelEdit`)
- Modify: `internal/dashboard/api_team_models.go` (`createTeamModel`, `updateTeamModel`, `teamModelView`, `msgBadTeamModelBody`, `msgBadTeamModelEdit`)
- Modify: `internal/dashboard/view.go` (`ModelAdminView` at :1661, `TeamModelView` at :1397)
- Modify: `openapi.yaml` (`CreateModelRequest`, `UpdateModelRequest`, `ModelAdminView`, `TeamModelView`, `CreateTeamModelRequest`, `UpdateTeamModelRequest`)
- Test: `internal/dashboard/api_models_test.go`, `internal/dashboard/api_team_models_test.go`
- Modify: `docs/concepts/dashboard.md` (the audit paragraph at :356)

**Interfaces:**
- Consumes: `config.ValidContextWindow`, `config.ValidMaxOutput`, `config.MaxOutputCeiling`, the `Min*`/`Max*` consts (Task 1); `Store.SetLLMModelWindow`, `Store.SetTeamModelWindow`, the model struct fields (Task 2).
- Produces (in `model_window.go`):
  - `const msgBadContextWindow = "contextWindow must be null or a whole number from 4096 to 10000000"`
  - `func maxOutputProblem(contextWindow int) string`: `maxOutput must be null or a whole number from 1024 to 10000000`, or `... from 1024 to <n>, the entry's context window` when one is set.
  - `func windowBody(w http.ResponseWriter, contextRaw, outputRaw json.RawMessage, beforeContext, beforeOutput int) (contextWindow, maxOutput int, ok bool)`: an absent field (`len(raw) == 0`) keeps `before`, `null` empties it, anything else is read as text like `quotaValue` (digits only, `strconv.Atoi`). The pair is checked after both are applied. On a refusal it has written the 400 and `ok` is false.
  - `func windowChanges(beforeContext, beforeOutput, contextWindow, maxOutput int) []string`: one item per changed field, `context_window <old> to <new>` / `max_output <old> to <new>`, `none` for 0.
  - `func tokensOrNull(n int) *int`: nil for 0.
  - `ModelAdminView` and `TeamModelView` gain `ContextWindow *int \`json:"contextWindow"\`` and `MaxOutput *int \`json:"maxOutput"\`` (always present, `null` when empty). A team viewer sees them too (they are not secret, unlike the base URL).
  - Request structs gain `ContextWindow json.RawMessage \`json:"contextWindow"\`` and `MaxOutput json.RawMessage \`json:"maxOutput"\``.

- [ ] **Step 1: Write the failing unit tests** in `model_window_test.go` (no DB, `httptest.NewRecorder()` only)
  - `TestWindowBody`, a table of `{name, context, output string; beforeC, beforeO int; wantC, wantO int; wantOK bool; wantMsg string}` (`""` = absent):
    - absent, absent, before `(1_000_000, 64_000)` → `(1_000_000, 64_000)`, ok
    - `null` context, absent output, before `(1_000_000, 500_000)` → `(0, 500_000)`, ok (Review Focus 2)
    - `4096`, `1024` → ok; `10000000`, absent → ok
    - `4095`, `10000001`, `0`, `-1`, `1e6`, `1000000.0`, `"\"1000000\""`, `9999999999999999999` as context → 400 with `msgBadContextWindow`
    - output `1023` → 400 containing `from 1024 to 10000000`
    - context absent, output `1000001`, before `(1_000_000, 0)` → 400 containing `to 1000000, the entry's context window` (Review Focus 1)
    - context `100000`, output absent, before `(1_000_000, 200_000)` → 400 on max output (Review Focus 1)
    - output `2000000`, no context, before `(0, 0)` → ok
  - `TestWindowChanges`: `(0, 0, 1_000_000, 64_000)` → `["context_window none to 1000000", "max_output none to 64000"]`; `(1_000_000, 64_000, 1_000_000, 0)` → `["max_output 64000 to none"]`; no change → empty.

- [ ] **Step 2: Watch them fail**

Run: `go test ./internal/dashboard -count=1 -run '^(TestWindowBody|TestWindowChanges)$'`
Expected: build failure, `undefined: windowBody`.

- [ ] **Step 3: Implement `model_window.go`**, then wire it:
  - `createModel` / `createTeamModel`: `windowBody(w, in.ContextWindow, in.MaxOutput, 0, 0)` after the key checks; pass the values to the create struct.
  - `updateModel` / `updateTeamModel`: call `windowBody` right after `before` is read, before any write. After the existing write(s) succeed with `found`, call `SetLLMModelWindow` / `SetTeamModelWindow` only when the pair differs from `before`. Set `after.ContextWindow/MaxOutput`. Append `windowChanges(...)` to `changed`, so the existing `model_updated` / `team_model_updated` entry (and the log's `fields`) carries them, for example `big (openai / gpt-x) (context_window none to 1000000, max_output none to 64000)`. No change, no audit entry, as today.
  - `modelView`, `teamModelView` and the config entries in `modelsView` (from `m.LLM`) fill the 2 view fields with `tokensOrNull` (`int(m.LLM.ContextWindow)` for the config's).
  - The 4 `msgBad*` texts list `"contextWindow": ..., "maxOutput": ...`.

- [ ] **Step 4: See the unit tests pass**

Run: `go test ./internal/dashboard -count=1 -run '^(TestWindowBody|TestWindowChanges)$'` → PASS.

- [ ] **Step 5: Update `openapi.yaml`.** Both views: add `contextWindow` and `maxOutput` as `type: [integer, 'null']` to `properties` and `required` ("null: opencode's own value"). The 4 request bodies: `contextWindow` `{type: [integer, 'null'], minimum: 4096, maximum: 10000000}`, `maxOutput` `{type: [integer, 'null'], minimum: 1024, maximum: 10000000, description: At most contextWindow when that is set. Leaving a field out keeps it on a PUT; null empties it.}`. Check the request bodies by hand: the test does not cover them.

Run: `go test ./internal/dashboard -count=1 -run '^(TestOpenAPISchemasMatchGoTypes|TestOpenAPIEveryViewTypeHasASchema|TestOpenAPIDocumentIsValid)$'` → PASS (no DB needed).

- [ ] **Step 6: Write the DB handler tests** (one setup each, subtests in order, parallel)
  - `TestModelWindowAdmin` in `api_models_test.go`: `newEnv(t, func(cfg) { cfg.LLM.ContextWindow, cfg.LLM.MaxOutput = 200_000, 32_000 })` — check first that `newEnv`'s config has an `llm:` block; if not, set provider, model and key env too. As `e.operator()`:
    - POST with `contextWindow: 1000000, maxOutput: 64000` → 201, view has both.
    - PUT `{"baseURL": ""}` → both kept (Review Focus 2).
    - PUT `{"baseURL": "", "maxOutput": null}` → `maxOutput` null, context kept.
    - PUT `{"baseURL": "", "contextWindow": 100000, "maxOutput": 200000}` → 400 with `to 100000, the entry's context window`, and GET shows the values unchanged.
    - PUT `{"baseURL": "", "contextWindow": 1e6}` → 400 `msgBadContextWindow`.
    - `auditDetails(e, "model_updated")` holds `context_window none to 1000000`-style items only for the PUTs that changed something.
    - GET: the `config` entry's view has `200000` / `32000`.
  - `TestTeamModelWindow` in `api_team_models_test.go`: `newTeamsEnv`; a maintainer POSTs with `maxOutput: 64000`; PUT `{"name": "renamed"}` keeps it; PUT `{"contextWindow": 32768}` → 400 (64000 is above it); PUT `{"contextWindow": 1000000}` → 200; a viewer's GET of the team's models shows `1000000` / `64000`; `team_model_updated` detail has `context_window none to 1000000`.

- [ ] **Step 7: Run them** — `go vet ./internal/dashboard/`, then with Postgres up only: `go test ./internal/dashboard -count=1 -run '^(TestModelWindowAdmin|TestTeamModelWindow|TestModelsAdminLifecycle|TestTeamModelsAccess)$'` with the env var in the same command. Else CI.

- [ ] **Step 8: Docs.** In `docs/concepts/dashboard.md`'s audit paragraph, after the models list: "`model_updated` and `team_model_updated` name the fields that changed, and give the context window and max output with their old and new values".

- [ ] **Step 9: Commit** `feat(dashboard): context window and max output on model entries`

---

### Task 6: Web forms and lists

**Files:**
- Modify: `web/src/api.ts` (`ModelAdminView` at :1171, `TeamModelView` at :1632)
- Modify: `web/src/ModelForms.tsx`
- Modify: `web/src/pages/AdminSettings.tsx` (`ModelRow` at :819, the line at :875)
- Modify: `web/src/pages/TeamModels.tsx` (the row at :84)
- Modify: `web/src/test/fixtures.ts` (every `ModelAdminView` / `TeamModelView` object gets `contextWindow: null, maxOutput: null`)
- Create: `web/src/ModelForms.test.tsx`
- Test: `web/src/pages/AdminSettings.test.tsx`, `web/src/pages/TeamModels.test.tsx`

**Interfaces:**
- Consumes: the API fields of Task 5.
- Produces (in `ModelForms.tsx`):
  - `export function parseTokens(text: string): number | null | undefined`: strips `,`, `_` and spaces; empty → `null`; digits only → the number (no range: the API answers with the range); else `undefined`.
  - `export function windowText(contextWindow: number | null, maxOutput: number | null): string`: `count()` of each joined by ` / `, a missing one as `–`, `""` when both are null.
  - `EditableModel` gains `contextWindow: number | null; maxOutput: number | null`.
  - Consts `contextWindowHelp` and `maxOutputHelp` with the exact help lines of Global Constraints.

- [ ] **Step 1: Write the failing tests**
  - `ModelForms.test.tsx`: `parseTokens("1,000,000") === 1000000`, `parseTokens(" 64 000 ") === 64000`, `parseTokens("") === null`, `parseTokens("1M") === undefined`, `parseTokens("1.5") === undefined`; `windowText(1_000_000, 64_000) === "1M / 64k"`, `windowText(200_000, null) === "200k / –"`, `windowText(null, 64_000) === "– / 64k"`, `windowText(null, null) === ""`.
  - `AdminSettings.test.tsx`:
    - `"adds a model with its context window and max output, under the help lines"`: the add form has inputs labelled **Context window** and **Max output**, each with its exact help line; typing `1,000,000` and `64000` POSTs `contextWindow: 1000000, maxOutput: 64000` beside the other fields. The existing add test (no values typed) keeps its exact body: empty fields are not sent.
    - `"edits the context window and max output, sending only what changed, and lists them as 1M / 64k"`: list with `{...modelOpus, contextWindow: 1_000_000, maxOutput: 64_000}`; the row shows `1M / 64k`; Edit shows the 2 values; renaming only → PUT body `{name, baseURL}` (Review Focus 4); emptying Max output → body has `maxOutput: null` and no `contextWindow`. A row with both null shows no `/` text.
    - `"refuses a context window that is not a whole number and sends nothing"`: `1M` → the form shows `Context window is a whole number of tokens, or empty.` and no POST is made.
  - `TeamModels.test.tsx`: `"sends a team entry's context window and max output, and lists them"`: POST body carries them; a viewer's list (`{...teamModelQwen, contextWindow: 128_000, maxOutput: null}`) shows `128k / –`.

- [ ] **Step 2: Watch them fail**

Run: `cd web && npx vitest run src/ModelForms.test.tsx src/pages/AdminSettings.test.tsx src/pages/TeamModels.test.tsx`
Expected: FAIL, `parseTokens` is not exported / no input labelled "Context window".

- [ ] **Step 3: Implement.**
  - Types in `api.ts` with a comment ("null: opencode's own value"), then fixtures until `npm run typecheck` is clean.
  - Both forms: 2 inputs after **Base URL** (`type="text"`, `inputMode="numeric"`, `{...noFill}`, placeholder `opencode's`), ids `llm-context-window` / `llm-max-output` and `${id}-context-window` / `${id}-max-output`. On submit, parse both; on `undefined` show `Context window is a whole number of tokens, or empty.` (or `Max output ...`) and send nothing. Add form: send a field only when not `null`. Edit form: send a field only when it differs from `m.contextWindow` / `m.maxOutput`, as `null` when emptied. Widen the mutation body type to `Record<string, string | boolean | number | null>`.
  - Lists: when `windowText(...)` is not empty, a `text-muted-foreground text-xs` span (admin row, beside `provider / model`) or line (team row) with `title="Context window / Max output"`.

- [ ] **Step 4: See them pass**

Run: `cd web && npm run typecheck && npm run lint && npx vitest run src/ModelForms.test.tsx src/pages/AdminSettings.test.tsx src/pages/TeamModels.test.tsx`
Expected: PASS. Each new test under 500 ms; name any that is not in the report. If the fixture change touched other test files, run those files too, one by one.

- [ ] **Step 5: Commit** `feat(web): context window and max output in the model forms and lists`

---

### Task 7: The docs contract and the final gate

**Files:**
- Modify: `docs/concepts/models.md` (the **Entries** section, :7-32)
- Modify: `docs/upgrading.md` (a new `## Unreleased` at the top, above `## Upgrading to 0.26.1`)

**Interfaces:**
- Consumes: everything above. Produces no code.

- [ ] **Step 1: `docs/concepts/models.md`.** In **Entries**: "An entry has a name, a provider, a model, a base URL (optional), a context window and a max output (both optional) and an API key", and the "can be changed" bullet names them. Then a new `### Context window and max output` subsection (the anchor Task 1 links to):
  - What each is, the ranges, and that empty means opencode's own value (its catalog, or 32,000 max output for a model id it does not know, with no compaction).
  - When to set them: a model id opencode's catalog does not know (a custom base URL, an id spelled another way). The context window from the provider's page. Max output: 64,000 is enough for reviews and scans; opencode asks for at most 256,000 per reply, and a provider that counts the asked amount against a per-minute rate limit throttles a higher value.
  - Every Job that runs the entry gets them: reviews, multi-review reviewers and the judge, security scans. After a fallback the next entry's values apply. A change applies from the next Job.
  - Who changes them (as the base URL), and the config keys (link to the config reference).
  - **The pergent image.** pergent v0.13.0, the default `jobs.image`, reads the context window in reviews only. There it also sets the diff batch size to a quarter of it, so a context window of 1,000,000 gives batches of up to 250,000 tokens. v0.13.0 scans read neither value, and nothing reads max output. The next pergent release reads both, in reviews and scans, and keeps batches at most 40,000 tokens.

- [ ] **Step 2: `docs/upgrading.md`, `## Unreleased`**, one bullet each:
  - **Migration `0056_model_limits_per_entry.sql`** runs at startup. Back up Postgres first. It adds 2 nullable columns to `llm_models` and `team_models` and drops nothing; the previous release ignores them.
  - **Context window and Max output** on every model entry, on the admin **Settings** page and a team's **Models** tab; API fields `contextWindow` and `maxOutput` on the model routes; the audit's `model_updated` / `team_model_updated` give old and new values.
  - **New config keys** `llm.context_window` and `llm.max_output`, also in each `llm.fallbacks` item. **They break a rollback:** the previous release refuses unknown keys, so remove them before you roll back.
  - **Jobs get `PERGENT_CONTEXT_LIMIT` and `PERGENT_OUTPUT_LIMIT`**, each only when set. With the default image (pergent v0.13.0) only reviews read the context window, and it also sets their batch size (as in models.md). Scans and the output limit need the next pergent release. An image that does not read a variable ignores it.
  - **Rolling back:** the values stay in the database and the previous release ignores them; its Jobs run without the variables.

- [ ] **Step 3: Check the prose.** Run the `prose-slop-check` skill's check on the new text of the 4 docs pages (old text may already fail; judge the new lines). No `→` in UI paths: write "the admin **Settings** page". No "limits" for these fields anywhere a person reads: `git diff origin/main -- docs web/src internal/dashboard | grep -in 'limit'` and read each hit (the env names `PERGENT_*_LIMIT` and the existing monthly limits are fine).

- [ ] **Step 4: Final gate.**
  - `go vet ./...` (vet only, never `go test ./...`).
  - The focused runs of Tasks 1, 4 and 5 again: `go test ./internal/config -count=1 -run '^(TestConfigModelWindow|TestModelWindowRanges)$'`, `go test ./internal/kube -count=1 -run '^(TestBuildJobModelWindow|TestSecurityJobModelWindow|TestBuildJobUnchanged)$'`, `go test ./internal/dashboard -count=1 -run '^(TestWindowBody|TestWindowChanges|TestOpenAPISchemasMatchGoTypes|TestOpenAPIDocumentIsValid)$'`.
  - `git log HEAD..origin/main --oneline` is empty, or merge `origin/main` first and check the migration number again.
  - The private-name scan and the email / local-path grep from the agent memory, over the branch's changed files and any new untracked file: both print nothing.

- [ ] **Step 5: Commit** `docs: context window and max output per model entry`

Then stop. CI's `test` and `web` checks are the full run; read the `internal/store`, `internal/engine` and `internal/dashboard` timings in the `test` log, because a green job only counts if the DB tests ran. No push, PR, tag or release without the owner's word.
