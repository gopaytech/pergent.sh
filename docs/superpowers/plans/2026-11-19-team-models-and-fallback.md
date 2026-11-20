# Team models and backup chains (pergent.sh) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Teams add their own models and keys, every review picks its model from a chain (repository, owner team or global), and a quota, billing or auth error starts the review again on the next entry, visibly and never as a pass.

**Architecture:** New tables hold team entries, chains, chain entries and marks; `team_repos.owner` names the owner team. A pure `internal/chain` package picks the entry for an attempt from the chain, the marks and the entries this review already tried. The dispatcher records the entry on the run. The publisher, before anything is posted, reads pergent's `provider_error`: for `quota`, `billing` or `auth` it marks the entry, closes the run as `fell_back` and queues the next attempt in one transaction. All attempts share one platform status (one Check Run on GitHub). The dashboard adds the editors; the web adds the pages.

**Tech Stack:** Go 1.24, Postgres (pgx), `net/http` ServeMux, React + Vite + TanStack Query, Vitest.

**Spec:** `docs/superpowers/specs/2026-11-19-team-models-and-fallback-design.md`. Read it with this plan. Its Decisions table is final. The CLI half is pergent's plan `docs/superpowers/plans/2026-11-19-provider-error-classes.md` in the pergent repository; it lands and is released first. pergent.sh only reads its field, so an older image gives today's behaviour (no `provider_error`, no fallback).

## Findings (checked 2026-11-19)

| # | Question | Answer | Evidence | Label |
|---|---|---|---|---|
| 1 | Repositories up to date | pergent `master` fast-forwarded 20 commits to `963a330`. pergent.sh is on `docs/team-models-fallback-spec` (`2ec1602`, rebased on `origin/main`). Latest migration is `0043_ci_jobs.sql`. | `git pull --ff-only`, `ls internal/store/migrations` | confirmed |
| 2a | Does `opencode run --format json` (1.19.4) emit an event per retry? | No. The retry only sets `session.status {type:"retry"}`, which the run loop never prints. One `error` event comes after the last retry. | opencode `session/processor.ts` L674-686, `cli/cmd/run.ts` L668-790 | confirmed |
| 2b | Is a quota 429 retried, with a limit? | Yes, up to 5 retries (`RETRY_MAX_RETRIES`), 2 s doubling, the provider's `retry-after` when sent (else no 30 s cap with headers). About 62-78 s without `retry-after`. Billing and auth errors are not retried. AI SDK retries are off. | `session/retry.ts` L26-31, L46-108, L193; `session/llm.ts` L323 | confirmed (`retry-after` on quota 429s: unknown) |
| 2c | Error fields | `name`; `data.message`, `statusCode?`, `isRetryable`, `responseHeaders?`, `responseBody?`. `providerID` only on `ProviderAuthError`. | `core/src/v1/session.ts` L50-64 | confirmed |
| 2d | Retry knob | None. | `retry.ts` | confirmed |
| 2e | Early stop chosen | Classify the one error event, stop opencode on it, never relaunch for quota/billing/auth. A per-call timeout would kill opencode before the error event and hide the class. Details and the parked gap (a long `retry-after`) are in pergent's plan. | pergent plan, Findings | decided |
| 3 | Can the GitHub forge update one Check Run? | Not today: `SetStatus` creates a new Check Run on every call (`internal/forge/githubforge/githubforge.go:381-397`) and throws away the ID `CreateCheckRun` returns (`internal/github/client.go:1005-1016`). There is no update call. Needed: `Client.UpdateCheckRun` (`PATCH /repos/{o}/{r}/check-runs/{id}`, same `checks: write` the App already needs, works on a run this App created), the ID kept on the run (`runs.status_ref`) and copied to each attempt, and a forge interface the engine uses when present. A 404 on update creates a new one. GitLab needs nothing: a status with the same name on the same commit replaces the old one. | code read | confirmed (PATCH semantics: GitHub public docs, not run here) |

**Update 2026-11-20:** rows 2a to 2e describe opencode 1.19.4. pergent now pins opencode 2.3.2 (pergent PR 40). There, opencode prints a `retry` event per retry and one `error` event at the end, does not retry quota, billing or auth, and types used-up allowances itself (`provider.quota` and 5 named limits). pergent PR 43 covers this, so the early stop in 2e holds, and the long `retry-after` gap is smaller. This plan's pergent.sh tasks do not change: they read only `provider_error.class`.

## Global Constraints

- Work on a new branch `feat/team-models` off `origin/main`, with the spec commit on it. Each phase ends with `main` shippable: merge only at the owner's word.
- Migrations only add, in `internal/store/migrations/0044_team_models.sql`. The one non-add step is dropping the `UNIQUE (provider, model, base_url)` rule on `llm_models` (the spec drops it). It only relaxes, so the previous release still works. `llm_models.name` is nullable in the database, because the previous release inserts rows without it after a rollback; the store reads `COALESCE(name, provider || '/' || model)` and the API requires a name.
- A Job gets only the key of the entry it runs (`kube.Secrets.LLMKey`). Team keys are noted by the `Registry` (so they are in `LLMKeys()`, `Redact` and `HeldSecrets()`) and never appear in an API response, a log line, an audit entry or a metric label.
- A team entry runs only for a repository its team owns: checked when a chain is saved (400) and again by the resolver at each attempt (skip `not_owner`).
- If every entry fails, the review fails (`all models exhausted` and each entry's reason). It never passes. A dropped attempt never posts a comment, a thread, an approval or a final status.
- Exact strings: `fell_back` (outcome), `all models exhausted`, `retrying on backup model (N of M)`, `payments/api already has owner Payments` (shape: `<path> already has owner <team>`, `<instance> (whole instance)` for `project ''`), `the chain has more than 5 entries`.
- Footer line, only when the run's entry is not position 1 of the chain in effect: `Reviewed with <name> (backup model, <reason>)`, reason `the main model was out of quota` (quota), `the main model was out of credit` (billing), `the main model's key was rejected` (auth), `the main model could not be used` (any other skip).
- Entry names: 1-64 characters, `^[A-Za-z0-9][A-Za-z0-9 ._:/+-]*$`, unique case-insensitively within a team and among global entries. A name reaches the MR comment, so it can hold no markdown; the footer still goes through `noMentions`.
- Admin routes go through `Dashboard.adminRoute` (404 for a non-admin) with `x-pergent-access: admin`. Team and repository chain routes are session routes that check in the handler: a non-member non-admin gets 404, a viewer or a maintainer of another team gets 403 on a write.
- Audit, never a key: `model_added`, `model_updated`, `model_removed` (now with the name), `team_model_added`, `team_model_updated`, `team_model_removed`, `chain_changed` (detail: scope and the entry names in order), `teams_may_use_changed`, `owner_set`, `owner_cleared`, `model_marked` (detail: name and class), `model_mark_cleared`, `team_models_deleted` (with a team).
- No release: no version bump, tag, chart version, `defaultImage` or `go.mod` move. The owner moves `defaultImage` to the pergent release from pergent's plan at release time. Upgrade notes go under `## Unreleased` in `docs/upgrading.md`.
- Public repository: before each commit, in the pergent.sh checkout, `git grep -niE 'golabs|gojek|gopay[^t]|goto\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'` and a `grep` over each new untracked file. Both print nothing.
- Never start a container. Database tests run only when `nc -z localhost 55432` succeeds, with the variable in the same command: `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m <pkgs>`. If the port is closed, say so and leave them to CI. No `gh`, `glab` or `curl`. Never run vitest while `go test` runs.
- Commit with a conventional subject, last line `Co-Authored-By: Claude <model that wrote it> <noreply@anthropic.com>`. Tests and commits are separate commands.
- **Phase gate** (end of each phase): `go vet ./...`; the full DB suite above with `./...`; when `web/` changed, `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build`; the private-name scan. Report failures by test name.

## Choices this plan makes (the spec left them open, or a fact forced them)

| Item | Choice | Why |
|---|---|---|
| Routes | `GET\|PUT /api/admin/chain`; `DELETE /api/admin/models/{id}/mark` (admin). `GET /api/me/teams`; `GET\|POST /api/teams/{id}/models`; `PUT\|DELETE /api/teams/{id}/models/{model}`; `DELETE /api/teams/{id}/models/{model}/mark`; `GET\|PUT /api/teams/{id}/chain` (session). Repository: suffix `/chain` on `GET\|PUT\|DELETE /api/repositories/{instance}/{rest...}` | The spec's `/api/repos/{instance}/{project}/chain` can't be a mux pattern: `{rest...}` must end it and a path holds `/`. `/api/me/teams` lists the user's teams for the new user area. |
| Metric names | `pergent_model_fallback_total{from_model,to_model,class,source}`, gauge `pergent_model_entry_exhausted{model,source,class}`; values are entry names | Every metric is `pergent_*` today (`internal/metrics/metrics.go`); the spec wrote `pergent_sh_`. |
| `outcome="fell_back"` on run metrics | No run metric has an outcome label. Dropped attempts are left out of `pergent_reviews_total`, `pergent_reviews_no_result_total` and `pergent_skill_runs_total`, kept in `pergent_tokens_total`, and counted in `pergent_model_fallback_total` | A new label would change every existing series. |
| Extra run columns | `model_entry_id`, `model_entry_name`, `chain_length`, `status_ref` (Check Run ID), `fallback_class` (why position 1 was not used) beside the spec's four | "Tried" needs the entry ID (positions move when a chain is edited); the review page and footer need the name after a rename or removal. |
| Dropped attempt row | `state = 'superseded'`, `outcome = 'fell_back'`, `outcome_reason` = the class. The cleanup captures its log and deletes its Job, and sets no status (a newer run of the head exists) | `superseded` keeps it out of `LastReview`, the publisher and the approval paths; an older release shows it as superseded. |
| No stored global chain | Absent row = `[default entry]`, or `[config, llm.fallbacks...]` when the config has `llm:`. The migration seeds no global chain. Once saved, stored entries follow the config ones | Keeps "nothing changes on upgrade" exact for installs with both a config `llm:` block and a stored default. |
| Repository chains | Seeded from `dashboard_repos.model_id` (`custom`, 1 entry, `config` allowed). Until Phase C, the old `model` setting write also rewrites that chain in the same transaction | Phase B reads chains while the old setting is still the writer. |
| A 1-entry chain | Never falls back **and ignores marks**: it runs and fails as today | "A 1-entry chain still never falls back"; another chain's mark must not stop it. |
| API reviews | Chain selection at dispatch (global chain; a pinned model as today), marks skip entries. No in-review fallback: the run fails as today and the mark is set | `api_reviews.inputs_sealed` is cleared once the Job has it and the review ID is the run ID, so a next attempt has no inputs. Parked for an owner ruling. |
| `fallback.quota_cooldown` | Config file only, default `1h`, `time.Duration`, at least `1m` | "One global setting"; YAGNI on a dashboard field. Breaks rollback like any new key (upgrade note). |
| `llm.fallbacks` | Same shape as `llm:`, at most 4 (config + 4 = 5). IDs `config-1`..`config-4`, names `provider/model`, read-only, never `teams may use` | The 5-entry limit counts them. |
| Removing a global entry used by chains | Allowed. The entries stay and are skipped as `removed`, with a warning on each chain. Every model view lists `usedIn` (chain labels) | Spec ruling "skip, and the dashboard warns". |
| Removing a team entry | `DELETE` removes it from every chain in the same transaction; the web confirm lists `usedIn` first | Spec ruling "confirm, then removed from them". |
| Owner changes | A team entry of a team that no longer owns the repository is skipped (`not_owner`) with a warning; the chain data is kept | Reversible; the resolver skip already exists in the spec. |
| Attempt fields | Copies `kind`, `trigger`, `skill_subset`, `base_run_id`, `quota_checked`, `ci_checked`, `status_ref`; `attempt_of` = the first run. Trust is not checked again. Attempts don't count against the re-run caps | The first attempt passed those checks for the same head. |
| `N of M` | N = this attempt's number in the review, M = entries in the chain in effect | |
| `/teams` page | Becomes the user area (team list, then `/teams/:id`); the old redirect to `/admin/teams` goes | The spec puts "Teams, Models tab" in the user area. |
| Settings `model` key | Removed from the repository settings body and view in Phase C; the chain replaces it | Dashboard-only API, web changes in the same release. |

## Review Focus

1. **A push during a fallback.** A new head supersedes the queued attempt; nothing more is posted for the old head, and the new head's review starts at position 1. Pinned by `TestFallbackThenPush` (Task B5).
2. **A Job result that arrives twice** (redelivered callback) for a run already `fell_back`: no second attempt, no second mark. Pinned by `TestFallbackIsOnce` (Task A4).
3. **Two keys of the same model in one chain**, the first out of quota: the second runs (entries are told apart by ID, not provider/model), and the incremental rule still sees "same model". Pinned by `TestResolveSameModelTwoKeys` (Task B1).
4. **A maintainer of team A editing team B's chain, or naming B's entry in A's chain.** 403 for the first, 400 for the second. Pinned by `TestTeamChainForeignEntry` (Task C2).
5. **A key that fails to unseal for one entry only** (a stale `PERGENT_SH_SECRET_KEY` row): that entry is skipped as `key_unusable`, the next runs, no key text in any log. Pinned by `TestResolveUnusableKey` (Task B1) and `TestTeamKeyUnsealFails` (Task B2).

---

## Phase A: migrations and store (no behaviour change)

### Task A1: Migration `0044_team_models.sql`

**Files:**
- Create: `internal/store/migrations/0044_team_models.sql`
- Test: `internal/store/team_models_test.go` (new)

**Interfaces:**
- Produces the schema below. The header comment says what the previous release does with each part (ignores it, or still runs on it).

| Object | Shape |
|---|---|
| `team_models` | `id text PK DEFAULT gen_random_uuid()::text`, `team_id bigint NOT NULL REFERENCES teams ON DELETE CASCADE`, `name text NOT NULL`, `provider text NOT NULL`, `model text NOT NULL`, `base_url text NOT NULL DEFAULT ''`, `api_key_sealed bytea`, `created_at`, `updated_at timestamptz NOT NULL`; unique `(team_id, lower(name))` |
| `model_chains` | `id bigserial PK`, `scope text CHECK IN ('global','team','repo')`, `team_id bigint REFERENCES teams ON DELETE CASCADE`, `instance text NOT NULL DEFAULT ''`, `project text NOT NULL DEFAULT ''`, `updated_at`; unique `(scope, COALESCE(team_id,0), instance, lower(project))`; CHECK team scope has `team_id`, repo scope has `instance` and `project` |
| `model_chain_entries` | `chain_id bigint REFERENCES model_chains ON DELETE CASCADE`, `position int CHECK (position BETWEEN 1 AND 5)`, `source text CHECK IN ('global','team')`, `model_id text NOT NULL`; PK `(chain_id, position)` |
| `model_entry_marks` | `source`, `model_id`, `class text CHECK IN ('quota','billing','auth')`, `until timestamptz` (NULL for auth), `set_at timestamptz NOT NULL`; PK `(source, model_id)` |
| `llm_models` | `ADD name text`, `ADD teams_may_use boolean NOT NULL DEFAULT false`; backfill `name = provider/model`, a second equal one gets ` (2)`, by `created_at`; unique index on `lower(name)`; `DROP CONSTRAINT llm_models_provider_model_base_url_key` |
| `team_repos` | `ADD owner boolean NOT NULL DEFAULT false`; unique `(instance, lower(project)) WHERE owner` |
| `dashboard_repos` | `ADD chain_mode text NOT NULL DEFAULT 'inherit' CHECK IN ('inherit','custom')`; rows with `model_id` set get `custom` and a 1-entry repo chain `(global, model_id)` |
| `runs` | `ADD attempt_of uuid` (no FK; index), `chain_position int`, `chain_length int`, `model_source text`, `model_team_id bigint`, `model_entry_id text`, `model_entry_name text`, `status_ref bigint`, `fallback_class text NOT NULL DEFAULT ''` |
| `run_usage` | `ADD source text NOT NULL DEFAULT 'global' CHECK IN ('global','team')`, `ADD team_id bigint`; index `(instance, source, reported_at)` |

- [ ] **Step 1: Write the failing tests** (model them on `TestModelsMigrationOnlyAdds` and `TestMigrationKeepsPreviousReleaseModels`, using `applyUpTo`):

```go
func TestTeamModelsMigrationOnlyAdds(t *testing.T)        // every column of 0043 is still there with its type
func TestTeamModelsMigrationBackfill(t *testing.T)        // at 0043 seed 2 llm_models with the same provider/model, a repo with model_id, a repo without: names "p/m" and "p/m (2)"; the first repo is custom with 1 entry, the second inherit with no chain; no global chain row
func TestTeamModelsMigrationPreviousReleaseInsert(t *testing.T) // after 0044, the 0043-era INSERT INTO llm_models (no name) and INSERT INTO team_repos (no owner) still succeed
func TestOneOwnerPerLevel(t *testing.T)                  // a second owner row for (main, ACME/api) fails 23505; owner of (main, '') and of (main, acme/api) both succeed
```

- [ ] **Step 2: Run to verify they fail** (`nc -z localhost 55432` first): `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'TeamModelsMigration|OneOwnerPerLevel'`. Expected: FAIL.
- [ ] **Step 3: Write the migration.**
- [ ] **Step 4: Run them, and the whole store package.** Expected: PASS.
- [ ] **Step 5: Commit** `feat(store): tables for team models, chains and marks`

### Task A2: Store: names, `teams may use`, team models

**Files:**
- Modify: `internal/store/models.go` (`LLMModel`, `modelColumns`, create/update; keep an in-code duplicate check of `(provider, model, base_url)` returning `ErrExists` until Task C1 removes it)
- Create: `internal/store/team_models.go`
- Test: `internal/store/models_test.go`, `internal/store/team_models_test.go`

**Interfaces:**
- Produces:
  - `LLMModel.Name string`, `LLMModel.TeamsMayUse bool`; `func (s *Store) SetLLMModelMeta(ctx, id string, name *string, teamsMayUse *bool, at time.Time) (found bool, err error)`.
  - `var ErrNameTaken = errors.New("store: an entry with this name exists")`.
  - `type TeamModel struct { ID string; TeamID int64; Name, Provider, Model, BaseURL string; APIKeySealed []byte; CreatedAt, UpdatedAt time.Time }`.
  - `func (s *Store) TeamModels(ctx, teamID int64) ([]TeamModel, error)`; `AllTeamModels(ctx) ([]TeamModel, error)`; `CreateTeamModel(ctx, m TeamModel, at) (TeamModel, error)`; `UpdateTeamModel(ctx, teamID int64, id string, name, baseURL *string, keySealed []byte, at) (found bool, err error)`; `DeleteTeamModel(ctx, teamID int64, id string) (removedFrom []Chain, err error)` (in one tx: the row, its chain entries, its mark).
  - A key change in `UpdateLLMModel` or `UpdateTeamModel` deletes that entry's `auth` mark in the same transaction.

- [ ] **Step 1: Failing tests:** `TestLLMModelNameUnique` (case-insensitive, `ErrNameTaken`), `TestLLMModelNameNullReadsProviderModel`, `TestTeamModelCRUD`, `TestTeamModelNameUniquePerTeam` (same name in 2 teams is fine), `TestKeyEditClearsAuthMark` (global and team; a quota mark stays), `TestDeleteTeamModelLeavesChains` (returns the chains, removes the entry, renumbers positions 1..n), `TestDeleteTeamCascades` (team models, its chain, marks of its models gone).
- [ ] **Step 2: Run, expect FAIL.** **Step 3: Implement.** **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat(store): entry names, teams may use, and team models`

### Task A3: Store: chains, marks, owner

**Files:**
- Create: `internal/store/chains.go`
- Modify: `internal/store/teams.go` (`TeamRepo.Owner`, `UpdateTeam`), `internal/store/dashboard.go` (`DashboardRepo.ChainMode`; the `ModelID` write rewrites the repo chain in the same tx)
- Test: `internal/store/chains_test.go`, `internal/store/teams_test.go`

**Interfaces:**
- Produces:
  - Consts `SourceGlobal = "global"`, `SourceTeam = "team"`, `ScopeGlobal`, `ScopeTeam`, `ScopeRepo`, `MaxChain = 5`, `ClassQuota`, `ClassBilling`, `ClassAuth`.
  - `type EntryRef struct{ Source, ID string }`; `type Chain struct{ ID int64; Scope string; TeamID int64; Instance, Project string; Entries []EntryRef; UpdatedAt time.Time }`.
  - `func (s *Store) Chain(ctx, scope string, teamID int64, instance, project string) (Chain, bool, error)`.
  - `func (s *Store) SaveChain(ctx, c Chain, at time.Time) error`: `ErrChainTooLong` over 5; replaces the entries. Global: sets `is_default` to position 1 when it is a stored global entry (unchanged otherwise) and writes `llm_settings` as `SetDefaultLLMModel` does. Repo: `chain_mode = 'custom'`, `model_id` = position 1 when global, NULL when team.
  - `func (s *Store) ResetRepoChain(ctx, instance, project string) error`: chain row gone, `inherit`, `model_id` NULL.
  - `func (s *Store) ChainsNaming(ctx, ref EntryRef) ([]Chain, error)`.
  - `type Mark struct{ Ref EntryRef; Class string; Until *time.Time; SetAt time.Time }`; `func (s *Store) Marks(ctx) ([]Mark, error)`; `ClearMark(ctx, ref EntryRef) (bool, error)`; `upsertMark(ctx, tx, m Mark) error` (used by Task A4).
  - `TeamRepo.Owner bool`. `UpdateTeam` refuses a second owner with `*OwnerTakenError{Instance, Project, Team string}` (`Error()` gives the exact refusal string). `func (s *Store) OwnerTeam(ctx, instance, project string) (teamID int64, ok bool, err error)`: the owner of the repository entry, else of the instance entry.

- [ ] **Step 1: Failing tests:** `TestSaveChainLimit`, `TestSaveGlobalChainSyncsDefault` (position 1 global → `is_default` and `llm_settings`; position 1 `config` → unchanged), `TestSaveRepoChainSyncsModelID` (team first → NULL), `TestResetRepoChain`, `TestModelSettingWritesRepoChain` (the old `ModelID` write: set → custom 1-entry, cleared → inherit), `TestMarksUpsertTwice` (2 upserts of the same ref, later `until` wins, no error), `TestOwnerMostSpecific`, `TestOwnerTakenError` (exact text `payments/api already has owner Payments`), `TestChainsNaming`.
- [ ] **Step 2: Run, expect FAIL.** **Step 3: Implement.** **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat(store): chains, marks and the owner team`

### Task A4: Store: attempts, usage source, quota and metrics reads

**Files:**
- Modify: `internal/store/runs.go` (`Run` fields, scan columns, `CountReruns`), `internal/store/dashboard_read.go` (`outcomeSQL`, digests leave out `fell_back` rows), `internal/store/usage.go` (`RecordUsage`, `MetricsTotals`), `internal/store/quota.go`
- Test: `internal/store/attempts_test.go` (new), `internal/store/usage_test.go`, `internal/store/quota_test.go`, `internal/store/dashboard_read_test.go`

**Interfaces:**
- Produces:
  - `Run.AttemptOf *string`, `ChainPosition, ChainLength int`, `ModelSource, ModelEntryID, ModelEntryName, FallbackClass string`, `ModelTeamID *int64`, `StatusRef int64`. `OutcomeFellBack = "fell_back"`.
  - `type RunEntry struct{ Source, ID, Name string; TeamID int64; Position, Length int; FallbackClass string }`; `func (s *Store) SetRunEntry(ctx, id string, e RunEntry) error`; `SetRunStatusRef(ctx, id string, ref int64) error`.
  - `func (s *Store) EnqueueAttempt(ctx, from Run, m Mark) (id string, created bool, err error)`: one tx under `lockMR`; only when `from` is still `reported`: upsert the mark, set `from` to `superseded` / `fell_back` / reason = class, insert the next run with the copied fields (see Choices), `attempt_of = COALESCE(from.attempt_of, from.id)`. Otherwise `created` false and nothing changes.
  - `func (s *Store) Attempts(ctx, root string) ([]Run, error)`: the root and its attempts, oldest first.
  - `RecordUsage` fills `source` and `team_id` from the run row. `func (s *Store) InstanceGlobalTokensSince(ctx, instance string, since time.Time) (int64, error)` counts only `source = 'global'`.
  - `CountReruns` leaves out rows with `attempt_of` set. `outcomeSQL` gives `fell_back` for such a row. Lists and Overview leave out `fell_back` rows (one row per review).
  - `MetricsTotals.Fallbacks []FallbackTotal{From, To, Class, Source string; Count int64}` (`To` = next attempt's entry name, `none` when it had no entry), `MetricsTotals.Exhausted []ExhaustedEntry{Name, Source, Class string}` (live marks), and `Reviews`, `NoResult`, `SkillRuns` leave out `fell_back` runs; `Tokens` keeps them.

- [ ] **Step 1: Failing tests:** `TestEnqueueAttemptCopiesAndCloses`, `TestFallbackIsOnce` (a second call on the same `from` creates nothing), `TestEnqueueAttemptAfterPush` (from already superseded: nothing), `TestAttemptsOrder`, `TestCountRerunsSkipsAttempts`, `TestRecordUsageSource` (team run → `team`, team id), `TestInstanceQuotaCountsGlobalOnly`, `TestListsOneRowPerReview`, `TestMetricsLeaveOutFellBack` (tokens still counted).
- [ ] **Step 2: Run, expect FAIL.** **Step 3: Implement.** **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat(store): review attempts, usage by source and fallback counts`

**Phase A gate.** Run it. Nothing reads the new data yet; the engine behaves as today.

## Phase B: resolver and engine

### Task B1: `internal/chain`: pick the entry

**Files:**
- Create: `internal/chain/chain.go`, `internal/chain/chain_test.go`

**Interfaces:**
- Consumes: `store.EntryRef`, `store.Mark`, `instances.Model` (Task B2 adds `Name`, `TeamsMayUse`, `TeamID`, `Removed`).
- Produces:

```go
type Input struct {
	Scope        string               // store.ScopeGlobal, ScopeTeam, ScopeRepo
	Refs         []store.EntryRef     // the chain in effect, position 1 first
	Entries      map[store.EntryRef]instances.Model // every known entry, key may be ""
	OwnerTeam    int64                // 0: no owner
	Marks        map[store.EntryRef]store.Mark
	Tried        map[store.EntryRef]bool
	InstanceQuotaUsedUp bool
	Now          time.Time
}
type Skip struct{ Ref store.EntryRef; Name, Reason string } // Reason: tried, quota, billing, auth, removed, not_allowed, not_owner, key_unusable, instance_quota
type Decision struct {
	Entry    instances.Model; Ref store.EntryRef
	Position int; Length int
	FirstSkip string   // why position 1 was not used ("" when it runs)
	Skips    []Skip
	Wait     bool      // empty chain: wait as with no model today
	Hold     bool      // only instance_quota skips: hold on the quota as today
	Exhausted bool     // no entry left otherwise
}
func Resolve(in Input) Decision
func FailureText(d Decision) string // "all models exhausted: <name> (<reason words>), ..."
```

- [ ] **Step 1: Failing tests** (table-driven, no database): `TestResolveFirstUsable`, one test per skip reason, `TestResolveMarkExpired`, `TestResolveAuthMarkHasNoExpiry`, `TestResolveOneEntryIgnoresMarks`, `TestResolveInstanceQuotaSkipsGlobalOnly` (team entry runs), `TestResolveOnlyQuotaBlockedHolds`, `TestResolveEmptyWaits`, `TestResolveExhausted` (`FailureText` names each entry and reason), `TestResolveSameModelTwoKeys`, `TestResolveUnusableKey`, `TestResolveNotAllowedAnyMore` (global entry in a team chain with `TeamsMayUse` false), `TestResolveGlobalChainAllowsAnyGlobal` (scope global ignores `TeamsMayUse`).
- [ ] **Step 2: Run, expect FAIL.** **Step 3: Implement.** **Step 4: `go test ./internal/chain/`, expect PASS.**
- [ ] **Step 5: Commit** `feat(chain): choose a review's model from its chain`

### Task B2: Config and registry: `llm.fallbacks`, `fallback.quota_cooldown`, team entries

**Files:**
- Modify: `internal/config/config.go` (`LLM.Fallbacks []LLM \`yaml:"fallbacks"\``, `Fallback struct{ QuotaCooldown time.Duration \`yaml:"quota_cooldown"\` }`, validation), `internal/instances/instances.go` (`Model` fields, `LoadLLM`, new `LoadTeamModels`, `GlobalModels`, `TeamModels`, `AllModels`, config fallback entries)
- Test: `internal/config/config_test.go`, `internal/instances/instances_test.go`

**Interfaces:**
- Produces: `Model.Name string`, `Model.TeamsMayUse bool`, `Model.TeamID int64`, `Model.Source` gains `SourceTeam`. `Registry.LoadTeamModels(ctx) error` (notes every team key, logs `a team model's api_key couldn't be opened` with the model ID only). `Registry.GlobalModels() []Model` (config, `config-1`..`config-4`, then stored), `Registry.TeamModels(teamID int64) []Model`, `Registry.AllModels() []Model`. `Config.Fallback.QuotaCooldown` (default `time.Hour`). `Registry.HasAnyModel() bool` (global or team).

- [ ] **Step 1: Failing tests:** `TestConfigFallbacks` (shape, each needs provider, model and key env; more than 4 refused with `llm.fallbacks: at most 4 entries`; fallbacks without `llm:` refused), `TestConfigQuotaCooldown` (default 1h, `30s` refused), `TestTeamKeysAreHeldAndRedacted` (a team key is in `HeldSecrets()` and `Redact` hides it), `TestTeamKeyUnsealFails` (entry kept with `Key ""`, log line has no key), `TestGlobalModelsOrder`.
- [ ] **Step 2: Run, expect FAIL.** **Step 3: Implement**, and load team models wherever `LoadLLM` runs (startup and after a model write). **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat(config): llm.fallbacks and fallback.quota_cooldown; team models in the registry`

### Task B3: Dispatch on the chain

**Files:**
- Modify: `internal/engine/dispatch.go` (L90-95 no-model gate uses `HasAnyModel`; trust check skipped for attempts; L292-306 quota split; L356-388 replaced; L698 running description), `internal/engine/apireview.go` (L92-109), `internal/engine/quota.go`, `internal/quota/quota.go`, new `internal/engine/chain.go` (loads the `chain.Input`)
- Test: `internal/engine/chain_test.go` (new), `internal/engine/dispatch_test.go`, `internal/quota/quota_test.go`

**Interfaces:**
- Consumes: `chain.Resolve`, `Store.Chain`, `Store.OwnerTeam`, `Store.Marks`, `Store.Attempts`, `Store.SetRunEntry`, `Registry.GlobalModels/TeamModels`.
- Produces: `func (e *Engine) chainFor(ctx, run store.Run, repo settings.Effective) (chain.Input, error)`: custom repo chain, else the owner team's chain, else the global chain (absent = see Choices); `Tried` from `Attempts(root)`. `quota.Check` returns `(repoUsedUp, instanceUsedUp bool, err error)`; the instance sum uses `InstanceGlobalTokensSince`.

- [ ] **Step 1: Failing tests:** `TestDispatchUsesCustomChain`, `TestDispatchUsesOwnerTeamChain`, `TestDispatchGlobalWhenOwnerHasNoChain`, `TestDispatchRecordsEntry` (`SetRunEntry` with name, position, source, team, `FallbackClass`), `TestDispatchExhaustedFails` (reason starts `all models exhausted`), `TestDispatchEmptyChainWaits`, `TestDispatchInstanceQuotaRunsTeamEntry`, `TestDispatchRepoQuotaHoldsAll`, `TestDispatchOnlyTheEntryKeyInSecret` (the Job Secret's `LLMKey` is the chosen entry's, no other key anywhere in the Job spec), `TestAttemptSkipsTrustCheck`, `TestAttemptRunningDescription` (`retrying on backup model (2 of 3)`), `TestAPIReviewPinnedModelUnchanged`, `TestAPIReviewSkipsMarkedGlobal`.
- [ ] **Step 2: Run, expect FAIL.** **Step 3: Implement.** Replace the comment at L356 with the new rule (falls back only on quota, billing and auth, never on a missing entry). **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat(engine): pick each review's model from its chain`

### Task B4: Publish: fall back before posting

**Files:**
- Modify: `internal/result/result.go` (`ProviderError` type and `Result.ProviderError`), `openapi.yaml` and `internal/dashboard/openapi_test.go` `openAPISchemas` (the new result type), `internal/engine/publish.go` (`PublishOnce`), new `internal/engine/fallback.go`, `internal/policy/policy.go` (`Outcome.FallbackNote`, put after the footer note through `noMentions`)
- Test: `internal/result/result_test.go`, `internal/engine/fallback_test.go`, `internal/policy/policy_test.go`

**Interfaces:**
- Produces: `result.ProviderError{Class string; Status int; Provider, Model string}` (JSON as pergent's plan). `func (e *Engine) fallBack(ctx, run store.Run, log *slog.Logger) (handled bool, err error)`, called in `PublishOnce` for every run before `publish`/`publishAPI`. It acts only when `run.Kind == mr`, the run has an entry, the chain in effect had at least 2 entries (`run.ChainLength >= 2`), and the class is `quota`, `billing` or `auth`. Mark `until`: `now + Fallback.QuotaCooldown` for quota and billing, NULL for auth. Then `EnqueueAttempt`, `SetStatus(StatusRunning, "retrying on backup model (N of M)")`, `Wake()`, audit `model_marked` (actor `pergent`).
- `policy.FallbackLine(name, firstSkip string) string` gives the exact footer line.

- [ ] **Step 1: Failing tests:** `TestParseProviderError` (present, absent, unknown class kept as text), `TestFallBackOnQuota`, `TestFallBackOnAuthMarksUntilEdit`, `TestNoFallBackOnRateLimit`, `TestNoFallBackOnOneEntryChain`, `TestNoFallBackOldImage` (no field), `TestNoFallBackForAPIReview`, `TestFallbackLine` (each reason), `TestFallbackLineInert` (a name `x@y` comes out without a live mention).
- [ ] **Step 2: Run, expect FAIL.** **Step 3: Implement.** **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit** `feat(engine): fall back to the next entry on quota, billing and auth errors`

### Task B5: One status across attempts, and the end-to-end tests

**Files:**
- Modify: `internal/github/client.go` (`UpdateCheckRun`), `internal/forge/forge.go` (optional interface), `internal/forge/githubforge/githubforge.go`, `internal/engine` (one helper used by the 5 `SetStatus` calls: `dispatch.go:698,1078`, `publish.go:173,810,988`)
- Test: `internal/github/client_test.go`, `internal/forge/githubforge/githubforge_test.go`, `internal/engine/e2e_fallback_test.go` (new), `internal/engine/e2e_github_test.go`

**Interfaces:**
- Produces: `func (c *Client) UpdateCheckRun(ctx, owner, repo string, id int64, status, conclusion, title, summary string) error`. `forge.StatusUpdater interface { SetStatusRef(ctx, projectID int64, sha string, ref int64, s StatusState, description string) (int64, error) }` (ref 0 creates; a 404 creates a new one and returns its ID). `func (e *Engine) setStatus(ctx, plat Platform, run store.Run, s forge.StatusState, description string) error` stores a new ref with `SetRunStatusRef`.

- [ ] **Step 1: Failing tests:** `TestUpdateCheckRun` (PATCH path and body), `TestSetStatusRefUpdates` and `TestSetStatusRefRecreatesOn404`; e2e with fake Job results: `TestE2EFallbackQuota` (row `fell_back`, next attempt runs entry 2, final comment has the footer line), `TestE2EFallbackAuth`, `TestE2ENoFallbackPlain429`, `TestE2EAllExhaustedFails` (status failed, never success, reason lists entries), `TestE2EDroppedAttemptPostsOnlyStatus` (no comment, thread or approval from attempt 1), `TestE2EEachEntryOnce` (3 entries, all quota: 3 attempts, then failed), `TestE2EDroppedTokensCount` (usage rows of attempt 1, repo quota sees them), `TestE2EChainEditBetweenAttempts` (entry 2 removed after attempt 1: attempt 2 takes entry 3), `TestE2ERerunStartsAtOne`, `TestFallbackThenPush`, `TestE2EGitHubOneCheckRun` (fake GitHub sees 1 create and only PATCHes after it across 2 attempts).
- [ ] **Step 2: Run, expect FAIL.** **Step 3: Implement.** **Step 4: Run with the DB variable, expect PASS.**
- [ ] **Step 5: Commit** `feat(engine): one status across attempts, one Check Run on GitHub`

### Task B6: Metrics

**Files:**
- Modify: `internal/metrics/metrics.go` (names, help, gauge), `cmd/pergent-sh/metrics.go`
- Test: `cmd/pergent-sh/metrics_test.go`, `internal/metrics/metrics_test.go`

- [ ] **Step 1: Failing tests:** `TestMetricsFallbackTotal` (labels `from_model`, `to_model`, `class`, `source` = entry names and `global`/`team`), `TestMetricsEntryExhaustedGauge`, `TestMetricsNoKeyInLabels` (no key, key prefix or entry UUID in the output), `TestMetricsFailureRateIgnoresFellBack`.
- [ ] **Step 2-4: FAIL, implement, PASS.**
- [ ] **Step 5: Commit** `feat(metrics): fallbacks and exhausted entries`

**Phase B gate.** With no API yet, fallback is reachable only through `llm.fallbacks`; every other install behaves as today.

## Phase C: API and access

### Task C1: Admin models and the global chain

**Files:**
- Modify: `internal/dashboard/api_models.go` (name required, `teamsMayUse`, removal no longer refused when in use, `usedIn` and mark state per entry, drop `msgModelExists`), `internal/dashboard/api_operator.go` (routes), new `internal/dashboard/api_chains.go`, `internal/dashboard/view.go`, `openapi.yaml`, `internal/store/models.go` (remove the Task A2 in-code duplicate check)
- Test: `internal/dashboard/api_models_test.go`, `internal/dashboard/api_chains_test.go` (new), `internal/dashboard/openapi_test.go`, `internal/dashboard/routes_test.go`

**Interfaces:**
- Produces: `ChainView{scope, source ("repository"|"team"|"global"), readOnly bool, entries []ChainEntryView}`; `ChainEntryView{position, source, id, name, provider, model, team, state ("ready"|"exhausted"|"key_rejected"|"removed"|"not_allowed"|"not_owner"|"key_unusable"), until?, class?, canClear bool}`; `ModelView` gains `name`, `teamsMayUse`, `usedIn []string`, `mark`. `PUT` chain body `{"entries":[{"source","id"}]}`. Never a key, only `keySet bool`.

- [ ] **Step 1: Failing tests:** `TestAdminChainGetDefault` (no row: the default, config entries read-only at the front), `TestAdminChainPut` (6 entries 400 `the chain has more than 5 entries`; unknown entry 400; config entries cannot be moved), `TestModelNameRequired`, `TestTwoKeysSameModel`, `TestRemoveModelInUseWarns`, `TestTeamsMayUseAudit`, `TestAdminClearMark`, `TestAdminRoutesStay404` (route walker), `TestNoKeyInAnyModelResponse`.
- [ ] **Step 2-4: FAIL, implement, PASS** (`go test ./internal/dashboard/ -run OpenAPI` with no database also passes).
- [ ] **Step 5: Commit** `feat(dashboard): entry names, teams may use and the global chain`

### Task C2: Teams: owner tick, team models, team chain

**Files:**
- Modify: `internal/dashboard/api_org.go` (`TeamRepoView.Owner`, 409 on a taken owner, `team_models_deleted` audit on delete), `internal/dashboard/api_chains.go`, new `internal/dashboard/api_team_models.go`, `internal/dashboard/access.go` (`teamRole(se, teamID) Role`), `openapi.yaml`
- Test: `internal/dashboard/api_team_models_test.go` (new), `internal/dashboard/api_org_test.go`, `internal/dashboard/access_test.go`

- [ ] **Step 1: Failing tests:** `TestOwnerTickTaken` (409, exact text), `TestMyTeams` (member sees own teams only), `TestTeamModelsAccess` (non-member 404, viewer GET 200 and write 403, maintainer write 200, admin 200), `TestTeamModelKeyWriteOnly`, `TestTeamChainOnlyOwnAndAllowed` (own entries and `teamsMayUse` globals; another team's entry 400), `TestTeamChainForeignEntry`, `TestTeamMaintainerClearsOwnMarkOnly` (403 on a global mark), `TestDeleteTeamAudits`, `TestTeamModelNameTaken`.
- [ ] **Step 2-4: FAIL, implement, PASS.**
- [ ] **Step 5: Commit** `feat(dashboard): owner teams, team models and team chains`

### Task C3: Repository chain, review attempts, usage split

**Files:**
- Modify: `internal/dashboard/api_read.go` (suffix `/chain` GET; run detail `attempts`, entry name, position), `internal/dashboard/api_write.go` (suffix `/chain` PUT and DELETE; `model` gone from the settings body, view and `settingViews`; `parseSettings`), `internal/dashboard/api_usage.go` (by source and team), `internal/dashboard/view.go`, `openapi.yaml`
- Test: `internal/dashboard/api_chains_test.go`, `internal/dashboard/api_read_test.go` (`TestRepositoryDetail` exact key set), `internal/dashboard/api_usage_test.go`

**Interfaces:**
- Produces: `RunDetailView.Entry{name, position, length, source, team}`, `RunDetailView.Attempts []AttemptView{runId, name, position, outcome, reason}`; `UsageRowView` gains `globalTokens`, `teamTokens`.

- [ ] **Step 1: Failing tests:** `TestRepoChainInheritShowsSource`, `TestRepoChainCustomByOwnerMaintainer`, `TestRepoChainNoOwnerAdminOnlyGlobal`, `TestRepoChainOtherTeam403`, `TestRepoChainReset`, `TestSettingsModelKeyRefused` (400), `TestRunDetailAttempts`, `TestUsageSplitsSource`.
- [ ] **Step 2-4: FAIL, implement, PASS.**
- [ ] **Step 5: Commit** `feat(dashboard): repository chains, attempts and usage by key source`

**Phase C gate.**

## Phase D: web

### Task D1: Types and the shared chain editor

**Files:**
- Modify: `web/src/api.ts`, test fixtures under `web/src/test/`
- Create: `web/src/ChainEditor.tsx`, `web/src/ChainEditor.test.tsx`, `web/src/MarkBanner.tsx`

**Interfaces:**
- Produces: `<ChainEditor value: ChainView; choices: ChainEntryView[]; readOnly: boolean; onSave(entries) ; onClear(entry)>` (up, down, add, remove, at most 5, the save button off past 5); state text `ready`, `exhausted until 14:00 (quota)`, `key rejected (auth)`, `removed`, `not allowed for teams any more`, `team no longer owns this repository`; **Clear mark** only when `canClear`. `<MarkBanner entries>` shows while any entry has a mark.

- [ ] **Steps:** failing tests `ChainEditor` (order, 5 limit, states, Clear mark calls the route, read-only config entries stay first), FAIL, implement, PASS, commit `feat(web): chain editor`.

### Task D2: Admin Models page and Teams owner tick

**Files:**
- Modify: `web/src/pages/AdminSettings.tsx` (name, `teams may use` toggle, the global chain editor, banner, remove dialog names `usedIn`), `web/src/pages/Teams.tsx` (owner tick per repository or instance entry; the 409 text shown), tests beside them

- [ ] **Steps:** failing tests in `AdminSettings.test.tsx` (exact PUT bodies) and `Teams.test.tsx` (owner tick body, 409 message), FAIL, implement, PASS, commit `feat(web): entry names, teams may use, global chain and owner tick`.

### Task D3: User area Teams, Models tab

**Files:**
- Modify: `web/src/App.tsx` (`/teams` and `/teams/:id` replace the redirect), `web/src/layout/Layout.tsx` (nav item for users on a team)
- Create: `web/src/pages/MyTeams.tsx`, `web/src/pages/TeamModels.tsx`, tests

- [ ] **Steps:** failing tests (a viewer sees entries and states with no edit buttons; a maintainer adds an entry with a write-only key field; remove confirm lists the chains; picker shows team entries and `teamsMayUse` globals only), FAIL, implement, PASS, commit `feat(web): team models and chain`.

### Task D4: Repository Model chain, review page, reviews list

**Files:**
- Modify: `web/src/pages/RepositorySettings.tsx` (the model menu becomes a Model chain section: Inherit shows the chain in effect and its source; Custom opens `ChainEditor`), `web/src/pages/ReviewDetail.tsx` (entry name, `position of length`, earlier attempts with reasons), `web/src/pages/ReviewRows.tsx` (an `n attempts` note), tests

- [ ] **Steps:** failing tests (`RepositorySettings.test.tsx`: no `model` in the PUT body any more; inherit and custom; a viewer sees read-only), FAIL, implement, PASS, commit `feat(web): repository chain and review attempts`.

**Phase D gate** (web commands included).

## Phase E: docs

### Task E1: The contract pages and the final gate

**Files:**
- Modify: `docs/concepts/models.md` (rewrite: entries and names, `teams may use`, team entries, chains at 3 levels, the owner team, what triggers a fallback, marks and cooldown, entry states, the banner, attempts, footer line, rollback), `docs/concepts/token-quota.md` (the instance quota counts only global keys), `docs/concepts/metrics.md` (the 2 new metrics; dropped attempts left out of the review counts), `docs/concepts/usage.md` (split by key source), `docs/concepts/dashboard.md` (user-area Teams), `docs/reference/configuration.md` (`llm.fallbacks`, `fallback.quota_cooldown`), `docs/reference/http-routes.md` (new routes), `docs/upgrading.md` `## Unreleased`, `docs/README.md` (line for models if its summary changes), `CLAUDE.md` (Rules: one key per Job, a team key only for a repository the team owns)

- [ ] **Step 1: Write the upgrade note.** It says: migration `0044` runs at startup and only adds, except that it drops the `(provider, model, base_url)` rule; back up Postgres first. `llm.fallbacks` and `fallback.quota_cooldown` are new config keys, so an older release refuses a config that has them. Fallback needs a pergent image that reports `provider_error`; an older one gives today's behaviour. Rollback: a repository whose chain starts with a team entry, and every repository that follows a team chain, runs on the global default until the next upgrade; each attempt shows as its own run; owner ticks made after a rollback edit of a team are lost.
- [ ] **Step 2: Check the docs against the code**: every exact string in Global Constraints appears where the docs quote it.
- [ ] **Step 3: Run the full gate** (Phase gate, all parts, with the database).
- [ ] **Step 4: Commit** `docs: team models and backup chains`
- [ ] **Step 5: Final review** with `superpowers:requesting-code-review` on `origin/main..HEAD`.

## Self-review notes

- Spec coverage: Data (A1-A4), Who can edit (C1-C3, access tests), Choosing the entry (B1, B3), Job ends with `provider_error` (B4), attempts as run rows and platform writes (A4, B4, B5), Counting (A4, B6), CLI (pergent plan), Dashboard and API (C, D), Edge cases (config-only B2; API clients B3; owner change B1 `not_owner`; team deleted A2 and C2; Re-run B5), Tests (each task), rollback note (E1).
- Spec points this plan could not do as written: the early stop during opencode's own retries (Findings 2e), the API reviews' in-review fallback (Choices), and the metric prefix and outcome label (Choices). None is a Decisions-table row.
