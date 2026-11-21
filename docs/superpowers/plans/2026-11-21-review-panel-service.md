# Review panel, pergent.sh service half: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A repository with a chain B gets a review panel: Job A runs chain A and Job B runs chain B, in parallel, each with its skill verification off; then Job P runs `pergent agent --panel` and only P's result is published. A repository with no chain B is reviewed exactly as today, byte for byte.

**Architecture:** Chain B lives in new tables `panel_chains` and `panel_chain_entries`, read and written by the same Go code as chain A (one store and one loader with a chain set parameter), at the same 3 levels with the same rights. The dispatcher, the publisher and the dashboard learn panel reviews: 3 runs grouped by `runs.panel_of`, with `runs.panel_role` `a`, `b` or `p`. Job P gets the two member results from a token-checked route, both LLM keys (each reaches only its own opencode run, which pergent enforces) and no platform write token.

**Tech Stack:** Go 1.24, Postgres (pgx), `net/http` ServeMux, Kubernetes Jobs, React + Vite + TanStack Query, Vitest.

**Spec:** `docs/superpowers/specs/2026-11-20-review-panel-design.md` (PR 71, merged). Its Decisions table is final. Read it and its Anti-goals first. The CLI half is pergent's plan `docs/superpowers/plans/2026-11-21-review-panel-cli.md` (pergent PR "docs: review panel CLI plan"). It lands first; this plan only reads its result fields and passes its flags.

## Status: two parts are blocked

Phases S1 and S2 can be built. **Phase S3 (the runs, the dispatch, Job P, publishing, the run page, API reviews) is blocked** until the owner rules on blocker 1, and its API part on blocker 2. S3 below is the shape of the work and its tests, not steps: write its steps once the rulings are in.

### Blocker 1: three live runs per review break `runs_live_idx`

The spec's Data table puts the 3 runs of one panel review in `runs` (`panel_of`, `panel_role` `a`/`b`/`p`) and says "No existing unique key changes". The code cannot do both:

- `runs_live_idx` is `UNIQUE (instance, project_id, mr_iid, head_sha, target_branch) WHERE state <> 'superseded'` (`internal/store/migrations/0003_run_target.sql:4-6`). It allows one live run per head and target. `EnqueueRun` and `EnqueueAttempt` rely on it ("Superseded first: one live run per head and target", `internal/store/attempts.go:76`).
- A panel has A and B running (in parallel, by the spec) while P waits. All 3 rows share the merge request, head and target, and none is superseded. The second insert fails with 23505. An API review has the same problem: its 3 rows share `mr_iid` (the review's number).

Options for the owner (this plan does not choose):

| Option | What changes | Cost |
|---|---|---|
| 1. Narrow the index | Drop and re-create `runs_live_idx` with `AND panel_role IS DISTINCT FROM 'a' AND panel_role IS DISTINCT FROM 'b'`; add `runs_panel_member_idx UNIQUE (panel_of, panel_role) WHERE state <> 'superseded' AND panel_role IN ('a', 'b')`. | Changes an existing unique key. On a rollback the previous release sees member rows as ordinary runs: it could claim a queued member, or publish a member's unverified result. Upgrade note: let running panels finish (or switch chain B off and wait) before a rollback. Smallest code change. |
| 2. Members outside `runs` | A new table `panel_members` (its own state, Job name, token hash, result, failure, attempt). P stays a run. | Keeps every key. The callback, watcher, cleanup, logs, diagnostics, usage, metrics and run page all learn a second kind of row: about twice the S3 work. The spec's `runs.panel_of`/`panel_role` become columns of `panel_members`. |

### Blocker 2: an API review's files are gone after its first Job

The spec says a review API request runs as a panel (all 3 Jobs). The review API spec (`2026-11-11-review-api-design.md` lines 34, 59, 197) says the sent files are deleted once the Job is created, and the code does it: `DropAPIInputs` right after the Job starts (`internal/engine/apireview.go:261`) and `DropSpentAPIInputs` for any review with a Job (`internal/store/apireviews.go:389-392`). Jobs B and P then have no files to build their checkout from. Options: (1) keep the sealed files until Job P is created (or the panel ends) and change the review API spec's wording; (2) API reviews never run as a panel (they run A alone with a note), which changes the panel spec's "API and re-runs" decision. Re-runs of a merge request are not affected. Also note: the team-models ruling already gives an API review no in-review fallback, so an API panel member cannot fall back either way.

### Open questions (not blocking; each has a default this plan uses)

| Question | Default here |
|---|---|
| Can a repository turn off a panel its owner team's or the global chain B gives it? | No. Chain B reads exactly like chain A ("a chain saved with no entries counts as none, so the next level is used", `chain.Load`), as the spec asks for the same Go code. Turning it off needs an empty custom chain to mean "off", a rule chain A does not have. |
| A falls back onto the model B runs on. | Allowed. The spec only makes B skip A's model. |
| Which entry counts as "the model A ran on" when B is dispatched first? | A's latest attempt with a recorded entry; with none yet, the entry A's chain resolves to now. Compared by provider and model, case-insensitive (two keys of one model are one model). |

## Team-models dependency state (checked 2026-11-21)

| Piece | Branch / PR | State | What this plan needs from it |
|---|---|---|---|
| Phase A: store, migration `0045_team_models.sql` | `feat/team-models`, PR 67 | **merged** (on `origin/main`) | `model_chains`, `model_chain_entries`, `runs.attempt_of`, `run_usage.source` |
| Phase B: resolver, dispatch, fallback | `feat/team-models-engine`, PR 69 | **merged** | `internal/chain` `Resolve`, `fallBack`, `EnqueueAttempt`, `setStatus` with `status_ref` |
| Phase C: API, `chain.Load`, migration `0046_chain_saved_by_admin.sql` | `feat/team-models-api`, PR 72 | **open, not merged** (head 268761e, 12 commits ahead of `main`) | `chain.Load`/`GlobalRefs`/`RefOf` in `internal/chain/load.go`, `Chain.SavedByAdmin`, the chain API handlers in `internal/dashboard/api_chains.go`. **S1 starts after PR 72 merges.** |
| Phase D: web (shared chain editor, task D1) | none | **not started** on any branch | The chain editor component. **Task S1.6 waits for it.** |
| Phase E: docs (`docs/concepts/models.md` chain pages) | none | **not started** | S1.7 adds the chain B section to whatever page phase E writes; if E is not in, to `docs/concepts/models.md` as it is. |

This plan does not touch those branches. Line references below are from `origin/main` 520a965, except files phase C changes, which are from `origin/feat/team-models-api` 268761e (marked "C").

**Migration numbers:** `main` ends at `0045`; PR 72 adds `0046`. This plan's first migration is **`0047`** if nothing else lands first; take the next free number from `origin/main` when you start.

## Findings (checked 2026-11-21)

| # | Question | Answer | Evidence |
|---|---|---|---|
| 1 | Where is a chain read? | `chain.Load` (C `internal/chain/load.go:21-58`): repository custom, else owner team, else global; global = config entries, then the stored chain, else the default entry. | code read |
| 2 | Where is a chain written? | `Store.SaveChain`, `ResetRepoChain`, `removeFromChains`, `loadChains`, `writeChainEntries` (C `internal/store/chains.go:48-320`), all on `model_chains`/`model_chain_entries`. Entry removal (`DeleteTeamModel`, a global removal) calls `removeFromChains`. | code read |
| 3 | How is an image's pergent version read? | `imageTag` (`internal/engine/incremental.go:130-137`) and `result.CarriedSupport` (`internal/result/result.go:1000-1018`): exactly `major.minor.patch`, with or without `v`; anything else is unknown. | code read |
| 4 | How does a review Job get a file pergent.sh serves? | The code graph: an init container runs `/pergent-sh get-graph --url=<callback>/runs/<id>/graph` with the run's result token (`internal/kube/graph.go:224-247`), served by `callback.RunGraphHandler` (`internal/server/server.go:115`). | code read |
| 5 | Job secrets | One Secret per Job, keys `git-token`, `llm-api-key`, `result-token` (`internal/kube/job.go:55-58`); `OPENCODE_API_KEY` from `llm-api-key` (`:339`). | code read |
| 6 | Who publishes | `PublishOnce` takes every `reported` run (`internal/engine/publish.go:72-106`), after `fallBack`. | code read |
| 7 | Does a comment name a model? | Never by provider/model (`internal/policy/hide.go:9-31`, `HideModel`). A backup entry is named by its entry name (`policy.FallbackLine`, `internal/policy/policy.go:420-433`). So panel names in the comment are entry names. | code read |
| 8 | Where usage is written | `usage.FromResult` (`internal/usage/usage.go:43-78`) → `Store.RecordUsage` (`internal/store/usage.go:27-53`), one row per run and skill, key `(run_id, skill)`. | code read |
| 9 | One live run per head and target | `runs_live_idx` (`0003_run_target.sql:4-6`). See blocker 1. | code read |
| 10 | API review files | Dropped once the Job exists (`apireview.go:261`, `apireviews.go:389-392`). See blocker 2. | code read |

## Global Constraints

- Branch per phase off `origin/main` (S1 after PR 72 is merged). Each phase ends with `main` shippable. Merge only at the owner's word.
- **No release.** No tag, no version bump, no chart version, no `defaultImage` move. Upgrade notes go under `## Unreleased` in `docs/upgrading.md`. Do not suggest a release to test this work: tests use fake Job results.
- **The image floor is not set here.** `config.PanelMinVersion` (Task S1.4) stays `""`, which refuses every image: no released pergent has `--panel`. The owner sets it to the first pergent release with `--panel`, together with `defaultImage` and the `go.mod` pin, at release time. Tests set it.
- Moving the `go.mod` pin of `github.com/gopaytech/pergent` to merged pergent `master` code is allowed (owner ruling 2026-11-19): `go get github.com/gopaytech/pergent@<sha>` (a pseudo-version), no `replace`. The Job keeps running the old `defaultImage`.
- Migrations only add (except where blocker 1's ruling says otherwise). The previous release must still run on the migrated database.
- **No chain B = no change.** With no chain B in effect, the Job (`kube.BuildJob`), its ConfigMap and Secret, the dispatch path and the comment are byte for byte what they are today. Pinned by golden tests (S2.4, S3.2).
- Keys: Jobs A and B get only their own entry's key. Job P gets both, in 2 Secret keys; pergent puts each in its own run's env. No key in an API response, a log, an audit entry, a metric label, the dashboard or the browser.
- Model text never runs a command and is data in every prompt (pergent's side).
- Rights for chain B are chain A's exactly: admin for global; admin or a maintainer of the owner team for the team's and its repositories'; everyone who can see the repository sees the setting, never a key.
- Public repository. Before each commit, run the private-name scan (the `git grep` in the pergent-developer memory, plans and `web/package-lock.json` excluded) and the same pattern plus the personal-email and local-path patterns over every new untracked file. Both print nothing. The controller puts the exact commands in each subagent brief.
- Never start a container. Database tests run only when `nc -z localhost 55432` succeeds, with the variable in the same command: `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m <pkgs>`. If the port is closed, say so and leave them to CI. No `gh`, `glab` or `curl` in a subagent. Never run vitest while `go test` runs.
- Commit with a conventional subject and the owner's personal address (as in memory; never written into a file). Last line `Co-Authored-By: Claude <model that wrote it> <noreply@anthropic.com>`. Tests and commits are separate commands.
- **Phase gate:** `go vet ./...`; the DB suite above with `./...`; when `web/` changed, `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build`; when the chart changed, `helm lint charts/pergent-sh -f charts/pergent-sh/ci-values.yaml`; the name scan. Report failures by test name.

## Choices this plan makes

| Item | Choice | Why |
|---|---|---|
| One chain code path | A chain set type `store.ChainSet` (`store.ChainA`: `model_chains`; `store.ChainB`: `panel_chains`) passed to the store's chain functions and to `chain.Load`. Today's names stay as chain A wrappers. | Spec: "read and written by the same Go code". |
| Chain B's global level | Only a stored global chain B. No config entries, no default entry: no stored global chain B means no panel. | Off by default. The config file has no chain B. |
| Repository mode | `dashboard_repos.panel_chain_mode` `inherit`/`custom`, beside `chain_mode`. | Spec: "its own inherit or custom mode at each level". |
| `model_id` sync | Only chain A writes `dashboard_repos.model_id` (the previous release's field). | The previous release has no panel. |
| Routes | `GET\|PUT /api/admin/panel-chain`; `GET\|PUT /api/teams/{id}/panel-chain`; suffix `/panel-chain` on `GET\|PUT\|DELETE /api/repositories/{instance}/{rest...}`. | Same shapes as phase C's chain routes. |
| Audit | `panel_chain_changed` (detail: scope and the entry names in order). | Same as `chain_changed`. |
| Image check message | Save refused with 400 `chain B needs a review image with pergent <min> or later; jobs.image is <tag or "not a version">`, and while the floor is unset `no review image supports a review panel yet`. | The check runs on save (spec). |
| Panel names in the comment | Entry names (`runs.model_entry_name` of the members' final attempts). | Finding 7. |
| `run_usage_models` columns | `run_id`, `skill`, `role`, `provider`, `model`, `entry_name`, `source`, `team_id`, the 5 token columns; key `(run_id, skill, role)`. | P splits its cost per model; `source`/`team_id` say whose key paid. |
| Instance quota (global keys only) | Reads `run_usage` as today; P's row is counted under P's recorded entry (member A's). The split by key is only in `run_usage_models`. | Spec: `run_usage` unchanged. A known small gap, in the docs. |
| The member results route | `GET /runs/{id}/panel/{role}` on the public host, P's result token, the member's stored result bytes; 404 for anything else. | The code graph's pattern. A result can be up to 10 MB, more than a ConfigMap holds. |
| Job P timeout | `--timeout=10m`, `activeDeadlineSeconds` = 10 min + the clone allowance + the deliver allowance (`kube.Deadline`). | Spec "10 min". |

## Review Focus

1. **Chain B off is today.** `TestBuildJobUnchanged` (S2.4) and the dispatch golden (S3.2) compare against bytes taken from `main` before any change.
2. **A key in the wrong place.** Job A's and B's Secret hold one LLM key; Job P's holds `llm-api-key` and `llm-api-key-b`, and the Pod spec names each once. Pinned by `TestBuildPanelJobKeys` (S2.4).
3. **Chain B rights.** A maintainer of team A cannot save team B's chain B (403) or name team B's entry (400). Pinned by `TestPanelChainForeignEntry` (S1.5).
4. **The image floor.** With `PanelMinVersion` unset, every save is refused. Pinned by `TestPanelChainRefusedWithoutFloor` (S1.5).

---

## Phase S1: chain B (buildable once PR 72 is merged)

Chain B can be saved only when `jobs.image` passes the image check, and the floor is unset until release, so nothing in this phase changes a review on `main`.

### Task S1.1: Migration `0047_panel_chains.sql`

**Files:**
- Create: `internal/store/migrations/0047_panel_chains.sql`
- Test: `internal/store/panel_chains_test.go` (new)

```sql
-- Review panel: chain B and the cost of a panel's merge per model. It only
-- adds, so the previous release (migrations up to 0046) still runs on the
-- migrated database and ignores all of it.
--
-- panel_chains and panel_chain_entries: chain B, the same shape as
-- model_chains and model_chain_entries (0045, 0046), at the same 3 levels.
-- No row at a level means that level has no chain B. A stored global chain B
-- is the only global one: with none, a review has no panel.
--
-- dashboard_repos.panel_chain_mode: 'custom' for a repository with its own
-- chain B, as chain_mode is for chain A.
--
-- run_usage_models: what a panel's merge (Job P) used per model, one row per
-- run, skill and role. run_usage keeps the total of each run and skill.
CREATE TABLE panel_chains (
    id             bigserial   PRIMARY KEY,
    scope          text        NOT NULL CHECK (scope IN ('global', 'team', 'repo')),
    team_id        bigint      REFERENCES teams (id) ON DELETE CASCADE,
    instance       text        NOT NULL DEFAULT '',
    project        text        NOT NULL DEFAULT '',
    saved_by_admin boolean     NOT NULL DEFAULT false,
    updated_at     timestamptz NOT NULL,
    CHECK ((scope = 'global' AND team_id IS NULL AND instance = '' AND project = '')
        OR (scope = 'team' AND team_id IS NOT NULL AND instance = '' AND project = '')
        OR (scope = 'repo' AND team_id IS NULL AND instance <> '' AND project <> ''))
);
CREATE UNIQUE INDEX panel_chains_key ON panel_chains (scope, COALESCE(team_id, 0), instance, lower(project));

CREATE TABLE panel_chain_entries (
    chain_id bigint NOT NULL REFERENCES panel_chains (id) ON DELETE CASCADE,
    position integer NOT NULL CHECK (position BETWEEN 1 AND 5),
    source   text   NOT NULL CHECK (source IN ('global', 'team')),
    model_id text   NOT NULL,
    PRIMARY KEY (chain_id, position)
);
CREATE INDEX panel_chain_entries_model_idx ON panel_chain_entries (source, model_id);

ALTER TABLE dashboard_repos ADD COLUMN panel_chain_mode text NOT NULL DEFAULT 'inherit'
    CHECK (panel_chain_mode IN ('inherit', 'custom'));

CREATE TABLE run_usage_models (
    run_id      uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    skill       text        NOT NULL,
    role        text        NOT NULL CHECK (role IN ('a', 'b')),
    provider    text        NOT NULL,
    model       text        NOT NULL,
    entry_name  text        NOT NULL DEFAULT '',
    source      text        NOT NULL DEFAULT 'global' CHECK (source IN ('global', 'team')),
    team_id     bigint,
    reported_at timestamptz NOT NULL,
    tokens_in   bigint      NOT NULL CHECK (tokens_in >= 0),
    tokens_out  bigint      NOT NULL CHECK (tokens_out >= 0),
    cache_read  bigint      CHECK (cache_read >= 0),
    cache_write bigint      CHECK (cache_write >= 0),
    reasoning   bigint      CHECK (reasoning >= 0),
    PRIMARY KEY (run_id, skill, role)
);
CREATE INDEX run_usage_models_reported_idx ON run_usage_models (reported_at);
```

- [ ] **Step 1: Failing tests** (model them on `TestTeamModelsMigrationOnlyAdds` with `applyUpTo`): `TestPanelChainsMigrationOnlyAdds` (every column of 0046 is still there with its type; `model_chains` and its entries unchanged); `TestPanelChainsMigrationPreviousRelease` (after 0047, the 0046-era `INSERT INTO dashboard_repos` and `INSERT INTO model_chains` still succeed); `TestPanelChainOneRowPerTarget` (a second global row fails 23505).
- [ ] **Step 2: Run to fail** (`nc -z localhost 55432` first): `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'PanelChain'`.
- [ ] **Step 3: Write the migration** above.
- [ ] **Step 4: Run them, then the whole store package.** PASS.
- [ ] **Step 5: Commit** `feat(store): tables for chain B and per-model usage`

### Task S1.2: Store: one chain code path for both sets

**Files:**
- Modify (C): `internal/store/chains.go` (`Chain`, `loadChains`, `writeChainEntries`, `removeFromChains`, `Store.Chain`, `Chains`, `ChainsNaming`, `SaveChain`, `ResetRepoChain`)
- Modify (C): `internal/store/team_models.go`, `internal/store/models.go` (the callers of `removeFromChains`)
- Test: `internal/store/panel_chains_test.go`, `internal/store/chains_test.go`

**Interfaces:**

```go
// ChainSet is which chain a chain row belongs to: ChainA, the review's model
// chain, or ChainB, a review panel's second chain.
type ChainSet struct{ chains, entries, mode string }

var (
	ChainA = ChainSet{chains: "model_chains", entries: "model_chain_entries", mode: "chain_mode"}
	ChainB = ChainSet{chains: "panel_chains", entries: "panel_chain_entries", mode: "panel_chain_mode"}
)

func (s *Store) ChainOf(ctx context.Context, set ChainSet, scope string, teamID int64, instance, project string) (Chain, bool, error)
func (s *Store) ChainsOf(ctx context.Context, set ChainSet) ([]Chain, error)
func (s *Store) ChainsNamingOf(ctx context.Context, set ChainSet, ref EntryRef) ([]Chain, error)
func (s *Store) SaveChainOf(ctx context.Context, set ChainSet, c Chain, at time.Time) error
func (s *Store) ResetRepoChainOf(ctx context.Context, set ChainSet, instance, project string) error
```

The table names come only from these two fixed values, never from input. `Chain`, `Chains`, `ChainsNaming`, `SaveChain`, `ResetRepoChain` call the `...Of` form with `ChainA`. Only `ChainA` updates `llm_settings` (global position 1) and `dashboard_repos.model_id`.

- [ ] **Step 1: Failing tests.** `TestChainBRoundTrip` (save and read global, team and repo chain B; repo save sets `panel_chain_mode = 'custom'` and leaves `chain_mode` and `model_id`); `TestChainBResetInherits`; `TestDeleteTeamModelLeavesNoChainBEntry` (a team entry in team chain B and a repo chain B: deleting it removes it from both, later entries move up); `TestRemoveGlobalModelKeepsChainBEntry` (as chain A: the entry stays and is skipped); `TestChainAUnchanged` (the existing chain tests pass with no edit).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'Chain'`.
- [ ] **Step 3: Code.** Build each SQL string from the set's names; `removeFromChains` runs once per set.
- [ ] **Step 4: Run** the whole store package. PASS.
- [ ] **Step 5: Commit** `feat(store): chain B through the chain A code`

### Task S1.3: Load chain B

**Files:**
- Modify (C): `internal/chain/load.go` (`Load`, `repoChain`, `GlobalChain`, `GlobalRefs`)
- Test (C): `internal/chain/load_test.go` (new if none)

**Interfaces:** `func LoadSet(ctx context.Context, st *store.Store, reg *instances.Registry, now time.Time, run store.Run, repo settings.Effective, set store.ChainSet) (Input, error)`; `Load` calls it with `store.ChainA`. For `ChainB` the global level is the stored global chain B only, and an empty result is `Input{Refs: nil}` with `Scope ""`: no panel.

- [ ] **Step 1: Failing tests.** `TestLoadChainBNoneIsOff` (nothing stored: `Refs == nil`, even with a config `llm:` block and a default entry); `TestLoadChainBRepoBeforeTeamBeforeGlobal`; `TestLoadChainBAttemptsTried` (a member's attempts give `Tried`, as for A); `TestLoadChainAUnchanged` (the current `Load` tests, untouched).
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/chain/` (with the DB variable when the tests use the store).
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run.** PASS.
- [ ] **Step 5: Commit** `feat(chain): load chain B`

### Task S1.4: The image check

**Files:**
- Modify: `internal/result/result.go:996-1018` (beside `CarriedSupport`)
- Modify: `internal/config/config.go` (the floor variable)
- Move: `imageTag` from `internal/engine/incremental.go:130-137` to `internal/config` as `ImageTag` (the engine calls it there)
- Test: `internal/result/result_test.go`, `internal/config/config_test.go`

**Interfaces:**

```go
// internal/config
// PanelMinVersion is the first pergent release with `agent --panel`, as
// "vX.Y.Z". "" (no release has it yet) refuses every image. The owner sets
// it at release time, with defaultImage.
var PanelMinVersion = ""

// PanelImage says whether image runs a pergent with --panel: its tag is a
// version (result.Version rules) at or above PanelMinVersion. reason is the
// refusal's text, "" when it passes.
func PanelImage(image string) (ok bool, reason string)
```

`internal/result` gets `func VersionAtLeast(version, min string) (atLeast, known bool)` with `CarriedSupport` rewritten on it (`VersionAtLeast(v, "0.5.0")`).

- [ ] **Step 1: Failing tests.** `TestVersionAtLeast` (table: `v0.12.0` vs `v0.12.0` true; `0.11.9` false; `v1.0.0` true; `main`, `0.12`, `0.12.0-rc1` unknown); `TestCarriedSupportUnchanged` (the current table); `TestPanelImage` (floor `""`: every image refused with `no review image supports a review panel yet`; floor `v0.12.0`: `ghcr.io/x/pergent:v0.12.0` ok, `:v0.11.0` refused, `:main` refused as not a version, `@sha256:...` with no tag refused, a registry port `host:5000/pergent:v0.12.0` ok).
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/result/ ./internal/config/ -run 'VersionAtLeast|CarriedSupport|PanelImage'`.
- [ ] **Step 3: Code.** Tests set `config.PanelMinVersion` with `t.Cleanup` to restore `""`.
- [ ] **Step 4: Run** both packages and `./internal/engine/ -run Incremental`. PASS.
- [ ] **Step 5: Commit** `feat(config): the review image check for a panel`

### Task S1.5: Chain B API, rights, image check on save, repository view

**Files:**
- Modify (C): `internal/dashboard/api_chains.go` (each handler takes a `store.ChainSet`; new handlers are one-line wrappers), `internal/dashboard/dashboard.go` (routes), `internal/dashboard/view.go` (repository view `panel`), `internal/dashboard/api_read.go`
- Modify: `openapi.yaml`, `internal/dashboard/openapi_test.go` (`openAPISchemas`), `internal/dashboard/routes_test.go`
- Test: `internal/dashboard/api_panel_chains_test.go` (new)

- [ ] **Step 1: Failing tests** (`newEnv`; `e.dashboardRepo` + `e.aliceOn` for a repository alice can see, memory trap; decode setting values as `any`):
  - `TestPanelChainRefusedWithoutFloor`: admin PUT of a global chain B with the floor unset: 400 `no review image supports a review panel yet`; nothing stored.
  - `TestPanelChainSavedWithFloor`: floor `v0.12.0`, `jobs.image` `...:v0.12.0`: global, team and repo saves work; audit `panel_chain_changed` with names, never a key.
  - `TestPanelChainImageTooOld` / `TestPanelChainImageNotVersion`: 400 with the tag in the message.
  - `TestPanelChainRights`: viewer 403 on a write; maintainer of the owner team saves the team's and an owned repository's chain B; a non-member gets 404 on the team route.
  - `TestPanelChainForeignEntry`: a maintainer of team A naming team B's entry: 400 (chain A's message).
  - `TestPanelChainNoKeyInResponses`: every GET body holds no key text.
  - `TestRepositoryViewPanel`: with a chain B in effect the repository view has `panel: {"on": true, "a": "<entry name>", "b": "<entry name>"}`; with none `panel` is `{"on": false}`.
  - `TestOpenAPIMatches` (existing) passes with the new routes, schemas and `x-pergent-access`.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/dashboard/ -run 'PanelChain|RepositoryViewPanel|OpenAPI'`.
- [ ] **Step 3: Code.** The image check runs in the shared save path after the rights checks and before `SaveChainOf`, for `ChainB` only. The admin route goes through `Dashboard.adminRoute`; team and repository routes are session routes that check in the handler, as phase C's.
- [ ] **Step 4: Run** `go test ./internal/dashboard/ -run OpenAPI` (no DB needed), then the package with the DB variable. PASS.
- [ ] **Step 5: Commit** `feat(dashboard): chain B settings with the chain A rights`

### Task S1.6: Web: chain B editors (waits for team-models phase D)

**Files:**
- Modify: `web/src/pages/RepositorySettings.tsx`, the team page and the admin models page phase D adds, `web/src/pages/RepositoryDetail.tsx`, `web/src/api/*` types
- Test: the matching `*.test.tsx`

- [ ] **Step 1: Failing tests.** Repository settings show a second editor "Chain B (review panel)" with its own Inherit/Custom switch, built from phase D's chain editor with the `/panel-chain` routes; a 400 from the image check shows its message; a viewer sees it read-only; the repository page shows `Panel: on (A: <entry>, B: <entry>)` or `Panel: off`. A web test of "disabled" uses a case the old code left enabled (memory).
- [ ] **Step 2: Run to fail.** `cd web && npx vitest run src/pages/RepositorySettings.test.tsx src/pages/RepositoryDetail.test.tsx`.
- [ ] **Step 3: Code.**
- [ ] **Step 4:** the web gate (`npm run typecheck && npm run lint && npx vitest run && npm run build`).
- [ ] **Step 5: Commit** `feat(web): chain B editors and the panel line`

### Task S1.7: Docs for chain B

**Files:** `docs/concepts/models.md` (or phase E's chain page), new `docs/concepts/review-panel.md` (status "experimental, off by default; reviews do not use it yet" until S3 lands), `docs/README.md` index, `openapi.yaml` descriptions, `docs/upgrading.md` `## Unreleased` (migration `0047` adds 3 tables and 1 column, drops nothing; the previous release runs on it).

- [ ] **Step 1:** Write the pages; every exact string matches the code.
- [ ] **Step 2:** Phase gate (Global Constraints).
- [ ] **Step 3: Commit** `docs: chain B`

---

## Phase S2: what does not depend on the run rows (buildable once the pergent CLI plan is merged on `master`)

### Task S2.1: Move the pergent pin

- [ ] **Step 1:** `GOFLAGS=-mod=mod go get github.com/gopaytech/pergent@<merged master sha>` (a pseudo-version; no `replace`), then `go mod tidy` (offline: `GOFLAGS=-mod=mod GOPROXY=off go mod tidy` when the modules are cached).
- [ ] **Step 2:** `go build ./...` and the full test suite (DB variable in the same command). Every existing comment golden is unchanged (pergent's panel fields are `omitempty` and `Options.Panel` nil writes nothing).
- [ ] **Step 3: Commit** `build: pin pergent with the review panel render fields`

### Task S2.2: Read the panel fields of a result

**Files:**
- Modify: `internal/result/result.go:30-215` (types), `:252-312` (`ToRender`)
- Test: `internal/result/result_test.go`

- [ ] **Step 1: Failing tests.** `TestParsePanelResult` (a P result with `panel`, `skills[].panel`, `tokens_by_model`, `findings[].panel` decodes); `TestToRenderPanelFields` (`render.Finding.Panel` and `render.Skill.Panel` filled, `also` kept); `TestToRenderNoPanelUnchanged` (a result without them renders the same `[]render.Skill` as before, deep-equal with a fixed want).
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/result/ -run 'Panel'`.
- [ ] **Step 3: Code.** Types mirror pergent's (`Panel`, `PanelMember`, `SkillPanel`, `PanelJudge`, `FindingPanel`, `ModelTokens`, `ProviderError.Role`). The Merge helpers (`Merge`, `MergeIncremental`, `assignIDs`) keep unknown fields as they are today (they work on raw JSON); add a test that `Merge` of a P result keeps `findings[].panel`.
- [ ] **Step 4: Run.** PASS.
- [ ] **Step 5: Commit** `feat(result): read a panel's result fields`

### Task S2.3: Usage per model

**Files:**
- Modify: `internal/usage/usage.go:43-78` (`FromResult` also returns the per-model rows), `internal/store/usage.go:27-53` (`RecordUsage` writes `run_usage_models` in the same transaction), `internal/callback/callback.go:158-165`
- Test: `internal/usage/usage_test.go`, `internal/store/usage_test.go`

- [ ] **Step 1: Failing tests.** `TestFromResultModelRows` (2 rows per skill from `tokens_by_model`, roles a and b; none for a result without it); `TestRecordUsageModels` (rows stored with `source`/`team_id` from the role's member entry when the caller passes it, `ON CONFLICT DO NOTHING`); `TestRecordUsageWithoutModelsUnchanged`.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/usage/ ./internal/store/ -run 'ModelRows|UsageModels|WithoutModels'`.
- [ ] **Step 3: Code.** `store.ModelUsage{Skill, Role, Provider, Model, EntryName, Source string; TeamID *int64; In, Out int64; CacheRead, CacheWrite, Reasoning *int64}`. The entry names and sources are filled in S3 from the members' runs; until then the callback passes provider and model only.
- [ ] **Step 4: Run.** PASS.
- [ ] **Step 5: Commit** `feat(usage): a panel merge's tokens per model`

### Task S2.4: The member and merge Jobs

**Files:**
- Modify: `internal/kube/job.go:86-127` (`JobParams`), `:135` (`Secrets`), `:269-464` (`BuildJob`), `:525-540` (`BuildSecret`)
- Create: `internal/kube/panel.go` (the `panel-inputs` init container), `internal/kube/testdata/review-job.golden.json`
- Modify: `cmd/pergent-sh` (a `get-panel` subcommand beside `get-graph`, sharing its fetch code, size cap 10 MiB, file mode 0600)
- Test: `internal/kube/job_test.go`, `internal/kube/panel_test.go`

**Interfaces:**

```go
// in JobParams:
	// SkipSkillVerify makes the Job a review panel's member: pergent leaves
	// out each skill's verification run (--no-skill-verify). False leaves the
	// Job as it was before panels.
	SkipSkillVerify bool
	// Panel makes the Job a review panel's merge (pergent agent --panel):
	// its init container fetches the two member results, and LLM is model A,
	// Panel.B model B. Nil leaves the Job a review.
	Panel *PanelParams

// PanelParams is what a merge Job needs beyond a review Job's checkout.
type PanelParams struct {
	AURL, BURL string     // /runs/<id>/panel/a and /b on the callback URL
	B          config.LLM // model B's provider, model and base URL; its key is Secrets.LLMKeyB
	NoJudge    bool       // --panel-no-judge: the quota stopped the cross-check
	Image      string     // pergent.sh's own image, for get-panel
}

// Secrets gains LLMKeyB, stored as "llm-api-key-b" only when Panel is set.
```

- [ ] **Step 1: Write the golden first, on `main` before any change:** a test `TestBuildJobUnchanged` that builds a review Job, its ConfigMap and its Secret from fixed `JobParams` covering every optional input (instructions, previous, carried, dismissals, description, linked, CI, graph, check-deleted) and compares the JSON with `testdata/review-job.golden.json`. Generate the file once with `-update`, commit it with the test (`test(kube): pin the review Job's spec`), and only then change `BuildJob`.
- [ ] **Step 2: Failing tests.** `TestBuildMemberJob` (`SkipSkillVerify` adds `--no-skill-verify` and nothing else: diff the spec against the golden); `TestBuildPanelJob` (args `--panel --panel-a=/panel/a.json --panel-b=/panel/b.json`, `--timeout=10m0s`, no `--previous`/`--carried`/CI/description/linked/graph inputs; env `PERGENT_PANEL_B_PROVIDER`, `_MODEL`, `_BASE_URL`; init containers `checkout` then `panel-inputs`); `TestBuildPanelJobKeys` (the Secret holds `llm-api-key` and `llm-api-key-b`; `OPENCODE_API_KEY` reads the first, `PERGENT_PANEL_B_API_KEY` the second, each once, only in the `pergent` container; `panel-inputs` gets only `PERGENT_RESULT_TOKEN`); `TestBuildPanelJobNoJudge` (`--panel-no-judge`, and no `llm-api-key-b`, no `OPENCODE_API_KEY` env); `TestBuildJobUnchanged` still passes.
- [ ] **Step 3: Run to fail.** `go test -count=1 ./internal/kube/`.
- [ ] **Step 4: Code.**
- [ ] **Step 5: Run.** PASS. Then `go vet ./...` (a `go build ./cmd/...` would drop a binary in the cwd: use `go vet` or `-o /dev/null`).
- [ ] **Step 6: Commit** `feat(kube): member and merge Jobs of a review panel`

### Task S2.5: The member results route

**Files:**
- Create: `internal/callback/panel.go` (`PanelHandler`, against an interface `PanelResults { PanelMemberResult(ctx, runID, token, role string) ([]byte, bool, error) }`)
- Modify: `internal/server/server.go:78-115` (`GET /runs/{id}/panel/{role}`)
- Test: `internal/callback/panel_test.go`, `internal/server/server_test.go`

- [ ] **Step 1: Failing tests** with a fake `PanelResults`: 200 with the bytes for a good token and role `a` or `b`; 404 for a bad token, a role other than `a`/`b`, an unknown run, or a member with no result; the body never logged; `server_test` lists the route on the public mux only.
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/callback/ ./internal/server/ -run Panel`.
- [ ] **Step 3: Code.** The store side is S3's (it depends on where member rows live). Until then `server.Deps.Panel` is nil and the route is not registered.
- [ ] **Step 4: Run.** PASS.
- [ ] **Step 5: Commit** `feat(callback): serve a panel merge its member results`

---

## Phase S3: panel reviews (BLOCKED: blocker 1; its API part also blocker 2)

Write the steps of these tasks after the rulings. Each must keep "chain B off = today" and leave `main` green. The tests named here are the minimum.

| Task | What it covers | Tests | How the ruling changes it |
|---|---|---|---|
| S3.1 Migration and store | `runs.panel_of` (uuid), `runs.panel_role` (`a`/`b`/`p`, CHECK), an index on `panel_of`; store calls `StartPanel` (parent waits, A and B queued with the parent's kind, trigger, subset, base run, `quota_checked`, `ci_checked`, `status_ref`), `PanelMembers`, `ReleasePanel` (both members final: parent queued for Job P), `PanelMemberResult` (S2.5's interface); `EnqueueAttempt` copies `panel_of`/`panel_role`; superseding the parent supersedes its members. | only adds (except the ruling's index); previous-release inserts; one live member per role; a push supersedes all 3; `EnqueueAttempt` of a member keeps its role | Option 1: columns on `runs`, the index change. Option 2: `panel_members` table and its own state machine. |
| S3.2 Dispatch: split | After trust, quota, CI and diff checks pass on the parent (role ''): load chain B (`LoadSet`); none, or the image fails `config.PanelImage` (note `panel off: the review image does not support it`), or B resolves to Wait/Hold/Exhausted under the quota (note: the quota stopped model B) → dispatch as today, with the note recorded on the run. Else `StartPanel`. | **golden: chain B off, the dispatch writes and the Job are today's** (a recorded fake-store call list); image too old; quota stops B; chain B with 1 entry equal to A's model → A alone with the "same model" note | Option 2 changes where the members are written. |
| S3.3 Dispatch: members | A member run skips trust (as an attempt does), quota and CI (checked by the parent), resolves its role's chain, B skips an entry with A's provider/model (Open questions), launches with `SkipSkillVerify`, sets the running status only on the first Job start (shared `status_ref`). | B falls back to A's model → takes its next; none left → B fails, panel "A only"; each Job's Secret has one LLM key | Option 2: a member dispatcher of its own. |
| S3.4 Publish: members never post | `PublishOnce` sends a member to `fallBack` (as today: a quota/billing/auth error queues the member's next attempt), else marks it final (no comment, no status, no approval, no thread, no metric review count) and calls `ReleasePanel` when both are final. | a member result posts nothing (fake forge sees no call); A fails, B passes → P starts "B only"; both fail → the parent fails with both reasons, never `success` | Option 2: the member's own final state. |
| S3.5 Job P | The parent's second dispatch launches Job P: members' final entries (models and keys), timeout 10 min, `NoJudge` when the quota would stop P; P's result is the parent's and publishes through today's path with `render.Options.Panel` = the members' entry names. A judge provider error (`provider_error.role`) falls back that member's entry for a new P attempt along that member's chain. | P publishes once; P's comment golden with entry names; a P quota error on role b → new P attempt with B's next entry; `run_usage_models` rows with entry names and sources | Same under both options. |
| S3.6 Quota and timeouts | All 3 runs count against the token quota (each writes `run_usage`). A member attempt gets the full review timeout. P waits for each member's final attempt. | quota counts A+B+P; a member Job past its deadline fails that member only | Same. |
| S3.7 Dashboard | The run page shows the 3 runs as one review: each member (its attempts, its entry), P, the cost per model from `run_usage_models` and the total; review lists show the parent only; the repository page line from S1.5. `openapi.yaml`, `openAPISchemas`, the enums test. | `TestRunPagePanel`; members hidden from lists; non-admin visibility via the parent | Option 2: members read from their table. |
| S3.8 Re-runs and API reviews | A re-run command (whole or failed skills) runs as a panel when chain B is in effect; a partial re-run merges P's result with the base run as today. API reviews: per blocker 2's ruling. | `TestRerunIsPanel`; `TestRerunFailedSkillsPanel`; API per ruling | Blocker 2. |
| S3.9 Metrics, docs, notes | Members are not counted in `pergent_reviews_total`; `docs/concepts/review-panel.md` loses "not used yet"; `how-it-works.md`, `job-isolation.md` (P's 2 keys), `usage.md`, `token-quota.md` (the instance quota gap), `dashboard.md`; `## Unreleased` notes for the S3 migration and any rollback rule from the ruling; a Grafana panel for any new metric. | phase gate | Option 1 adds the rollback note. |

## Self-review notes

- Spec coverage: tables `panel_chains`, `panel_chain_entries`, `run_usage_models` (S1.1), `runs.panel_of`/`panel_role` (S3.1, blocked); chain B at 3 levels with chain A's rights and code (S1.2, S1.3, S1.5); image check on save (S1.4, S1.5) and at dispatch (S3.2); parallel A and B with verify off, fallback per member, B skips A's model (S3.3); P when both are final, 2 keys, quota, timeouts (S3.5, S3.6); publish only P (S3.4, S3.5); dashboard (S1.6, S3.7); API and re-runs (S3.8, blocker 2); docs and `## Unreleased` (S1.7, S3.9); chain B off = byte for byte (S2.4, S3.2).
- Not here: the pergent CLI (its own plan), any release step, `defaultImage`, the value of `PanelMinVersion`.
