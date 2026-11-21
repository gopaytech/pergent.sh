# Review panel, pergent.sh service half: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A repository with a chain B gets a review panel: Job A runs chain A and Job B runs chain B, in parallel, each with its skill verification off; then Job P runs `pergent agent --panel` and only P's result is published. A repository with no chain B is reviewed exactly as today, byte for byte.

**Architecture:** Chain B lives in new tables `panel_chains` and `panel_chain_entries`, read and written by the same Go code as chain A (one store and one loader with a chain set parameter), at the same 3 levels with the same rights; a repository can also turn it `off`. A panel review is one row in `runs` (the run Job P reports on) and one row per attempt of member A or B in a new table `panel_members`, with its own state machine. While the members run, the run waits (`waiting`, reason `panel_members`); when every role has a final member, the run is queued again and its next dispatch starts Job P. Job P gets the member results from a token-checked route, both LLM keys (pergent puts each in its own opencode run) and no platform write token.

**Tech Stack:** Go 1.24, Postgres (pgx), `net/http` ServeMux, Kubernetes Jobs, React + Vite + TanStack Query, Vitest.

**Spec:** `docs/superpowers/specs/2026-11-20-review-panel-design.md`. Its Decisions table is final. Read it and its Anti-goals first. The CLI half is pergent's plan `docs/superpowers/plans/2026-11-21-review-panel-cli.md` (merged on pergent `master`). It lands first; this plan only reads its result fields and passes its flags.

**Owner rulings 2026-11-21** (now rows of the spec's Decisions table): members A and B are `panel_members` rows and only Job P's run is in `runs`, so `runs_live_idx` does not change; an API review keeps its stored files until Job P has started; a repository's chain B mode is `inherit`, `custom` or `off`.

## Open questions (not blocking; each has a default this plan uses)

| Question | Default here |
|---|---|
| A falls back onto the model B runs on. | Allowed. The spec only makes B skip A's model. |
| Which entry is "the model A ran on" when B is dispatched? | A's newest member row with an entry; with none yet, the entry A's chain resolves to now. Compared by provider and model, ignoring case (two keys of one model are one model). |
| The chain B of an API review | The global chain B, as an API review's chain A is the global chain. A client with its own model runs member A on that model. |
| Fallback of an API review's member | None, as for an API review today (team-models ruling). A member that hits a quota fails, and P runs "A only" or "B only". |
| The instance quota (global keys only) | It reads `run_usage.source`, which is the run's entry (member A's). Tokens member B spent on a global key under a team A are not counted there. Written in `token-quota.md`, parked. |

## Team-models dependency state (checked 2026-11-21 on `origin/main` 1cad1a9)

| Piece | PR | State | What this plan needs from it |
|---|---|---|---|
| A: store, `0045_team_models.sql` | 67 | merged | `model_chains`, `model_chain_entries`, `runs.attempt_of`, `run_usage.source` |
| B: resolver, dispatch, fallback | 69 | merged | `chain.Resolve`, `fallBack`, `EnqueueAttempt`, `setStatus` with `status_ref` |
| C: API, `chain.Load`, `0046_chain_saved_by_admin.sql` | 72 | merged | `chain.Load`, `GlobalRefs`, `RefOf` (`internal/chain/load.go`), `Chain.SavedByAdmin`, the chain handlers (`internal/dashboard/api_chains.go`) |
| D: web | 74 (`feat/team-models-web`) | **open** | `web/src/ChainEditor.tsx`, `web/src/pages/RepoModelChain.tsx`, `TeamModels.tsx`, `AdminSettings.tsx`, and changes to `ReviewDetail.tsx`. **S1.6 and the web step of S3.11 wait for it.** |
| E: docs | none | not started (`docs/concepts/models.md` has no chain section) | S1.7 writes chain B beside whatever is there. |

Line references below are from `origin/main` 1cad1a9. **Migrations:** `main` ends at `0046`. S1 takes `0047`, S3 takes `0048`; take the next free numbers from `origin/main` when you start.

## Findings (checked 2026-11-21)

| # | Question | Answer | Evidence |
|---|---|---|---|
| 1 | Where is a chain read? | `chain.Load` (`internal/chain/load.go:21-58`): repository custom, else owner team, else global; global = config entries, then the stored chain, else the default entry. | code read |
| 2 | Where is a chain written? | `loadChains`, `writeChainEntries`, `removeFromChains`, `Store.Chain`, `Chains`, `ChainsNaming`, `SaveChain`, `ResetRepoChain` (`internal/store/chains.go:48-320`). The repository reset is `POST .../chain/reset` (`api_chains.go:585`). | code read |
| 3 | How is an image's pergent version read? | `imageTag` (`internal/engine/incremental.go:130-137`) and `result.CarriedSupport` (`internal/result/result.go:1000`). | code read |
| 4 | How does a Job get a file pergent.sh serves? | `get-graph` init container (`internal/kube/graph.go:233`), served by `callback.RunGraphHandler` (`internal/server/server.go:115`). `get-graph` always exits 0 and writes no file on a 404 (`cmd/pergent-sh/graph.go:174-209`). | code read |
| 5 | Job secrets | One Secret per Job, keys `git-token`, `llm-api-key`, `result-token` (`internal/kube/job.go:55-57`); `OPENCODE_API_KEY` from `llm-api-key` (`:339`). Job name: `JobName(id)`, the first 12 hex characters (`:138`). | code read |
| 6 | Who publishes | `PublishOnce` (`internal/engine/publish.go:72`), after `fallBack`. | code read |
| 7 | Does a comment name a model? | Never by provider/model (`internal/policy/hide.go`, `HideModel`; `Engine.hideModel`, `publish.go:374`). A backup entry is named by its entry name (`policy.FallbackLine`, `policy.go:420`). So panel names in the comment are entry names. | code read |
| 8 | Where usage is written | `usage.FromResult` (`internal/usage/usage.go:43`) → `Store.RecordUsage` (`internal/store/usage.go:27`), key `(run_id, skill)`, `DO NOTHING`, then `runs.usage_recorded`. | code read |
| 9 | One live run per head and target | `runs_live_idx` (`0003_run_target.sql:4-6`). Unchanged: members are not runs. | ruling |
| 10 | API review files | Dropped right after the Job starts (`internal/engine/apireview.go:261`). `DropSpentAPIInputs` (`internal/store/apireviews.go:388`) keeps the files of a `waiting` run with no Job, so a panel's waiting run keeps them with no change there. | code read |
| 11 | Where a waiting run is read | `StartRun` (`runs.go:927`), `liveRerunRunSQL`/`liveStartRunSQL` (`runs.go:868-879`), `WaitingChanges` (`runs.go:1704`), `outcomeSQL` (`dashboard_read.go:113`), `apiStatusSQL` (`apireviews.go:240`), `reviewapi.StatusOf` (`internal/reviewapi/body.go:297`), the start button (`internal/dashboard/api_commands.go:172`), `ClaimQueued`'s in-flight count (`runs.go:673`), `DeleteAPIClient` (queued and running only, `apiclients.go:236`). Superseding (`EnqueueRun`, `EnqueueRerun`, `supersedeMR`, `StartRun`) already includes `waiting`. | code read |
| 12 | What handles one row per Job | `callback.Handler` (`/runs/{id}/result`), `WatchOnce`/`RunningRuns`, `RecoverUnlaunched`, `CleanupOnce`/`RunsToCleanUp`, `run_logs`/`run_diagnostics` (FK to `runs`), `CountRunning`, `ClaimQueued`. Members need their own of each. | code read |
| 13 | Cleanup of a superseded run | `cleanUp` (`publish.go:976`) sets the canceled status only when `DispatchedAt` is set. | code read |
| 14 | E2E helpers | `newFallbackE2E`, `e.attempt`, `failedOn`, `clean`, `withTokens`, `wantFellBack` (`internal/engine/e2e_fallback_test.go`); `newE2E`, `e.postResult`, `e.newJob` (`e2e_test.go`). | code read |

## Global Constraints

- Branch per phase off `origin/main`. Each phase ends with `main` shippable. Merge only at the owner's word.
- **No release.** No tag, no version bump, no chart version, no `defaultImage` move. Upgrade notes go under `## Unreleased` in `docs/upgrading.md`. Do not suggest a release to test this work: tests use fake Job results.
- **The image floor is not set here.** `config.PanelMinVersion` (Task S1.4) stays `""`, which refuses every image: no released pergent has `--panel`. The owner sets it with `defaultImage` and the `go.mod` pin at release time. Tests set it.
- Moving the `go.mod` pin to merged pergent `master` code is allowed (owner ruling 2026-11-19): `go get github.com/gopaytech/pergent@<sha>`, no `replace`. The Job keeps running the old `defaultImage`.
- Migrations only add. The previous release must still run on the migrated database.
- **No chain B = no change.** With no chain B in effect, the Job (`kube.BuildJob`), its ConfigMap and Secret, the dispatch writes and the comment are byte for byte today's. Pinned by golden tests (S2.4, S3.4).
- Keys: Jobs A and B get only their own entry's key. Job P gets at most 2, in 2 Secret keys; pergent puts each in its own run's env. No key in an API response, a log, an audit entry, a metric label, the dashboard or the browser.
- Model text never runs a command and is data in every prompt (pergent's side).
- Rights for chain B are chain A's exactly. Everyone who can see the repository sees the setting, never a key.
- Public repository. Before each commit, run the private-name scan (the `git grep` in the pergent-developer memory, plans and `web/package-lock.json` excluded) and the same pattern plus the personal-email and local-path patterns over every new untracked file. Both print nothing. The controller puts the exact commands in each subagent brief.
- Never start a container. Database tests run only when `nc -z localhost 55432` succeeds, with the variable in the same command: `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m <pkgs>`. If the port is closed, say so and leave them to CI. No `gh`, `glab` or `curl` in a subagent. Never run vitest while `go test` runs.
- Commit with a conventional subject and the owner's personal address (as in memory; never written into a file). Last line `Co-Authored-By: Claude <model that wrote it> <noreply@anthropic.com>`. Tests and commits are separate commands.
- **Phase gate:** `go vet ./...`; the DB suite above with `./...`; when `web/` changed, `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build`; when the chart changed, `helm lint charts/pergent-sh -f charts/pergent-sh/ci-values.yaml`; the name scan. Report failures by test name.

## Choices this plan makes

| Item | Choice | Why |
|---|---|---|
| One chain code path | A chain set type `store.ChainSet` (`store.ChainA`: `model_chains`; `store.ChainB`: `panel_chains`) passed to the store's chain functions and to `chain.LoadSet`. Today's names stay as chain A wrappers. | Spec: "read and written by the same Go code". |
| Chain B's global level | Only a stored global chain B. No config entries, no default entry. | Off by default. The config file has no chain B. |
| Repository mode | `dashboard_repos.panel_chain_mode` `inherit`/`custom`/`off`. `off` keeps no repository row in `panel_chains`; `LoadSet(ChainB)` returns no chain before it looks at the team or global level. Turning it off skips the image check. | Ruling Q19. Turning the panel off must always work. |
| `model_id` sync | Only chain A writes `dashboard_repos.model_id`. | The previous release has no panel. |
| Routes | `GET\|PUT /api/admin/panel-chain`; `GET\|PUT /api/teams/{id}/panel-chain`; on `/api/repositories/{instance}/{rest...}`: `GET\|PUT .../panel-chain`, `POST .../panel-chain/reset` (inherit), `POST .../panel-chain/off`. | The shapes of chain A's routes (`POST .../chain/reset`). |
| Audit | `panel_chain_changed` (detail: scope, then the entry names in order, or `mode=off`, or `mode=inherit`). | As `chain_changed`. |
| Image check message | Save refused with 400 `chain B needs a review image with pergent <min> or later; jobs.image is <tag or "not a version">`; while the floor is unset `no review image supports a review panel yet`. | Spec: the check runs on save. |
| The run while members run | `state = 'waiting'`, `skip_reason = 'panel_members'` (`store.WaitPanel`), `dispatched_at` set at the split. | The previous release never claims a waiting run. `dispatched_at` makes cleanup cancel the status and makes re-run and start see a review on its way (`onItsWaySQL`). |
| Which run members belong to | `panel_members.run_id` is the review's first run (its root). A later attempt of Job P (`attempt_of`) finds the members through the root. | P falls back as a run does in team models. |
| How a member gets its Job | The split only writes rows. The member dispatcher claims a queued member and runs the same `dispatch` code in a member stage: it reads the change, skills and inputs again, skips trust, the quota hold and CI (the run passed them), writes nothing to the run, and launches a member Job. First attempts and fallback attempts take this one path. | One path. Costs one more read of the change per member. |
| "A only" at dispatch | When B can't start (image, quota, same model, no usable entry), the run is today's review Job, with one `panel_members` row in state `off` holding the reason. Publish adds `panel off: <reason>` to the footer. | Same outcome as a P with one member, and cheaper. |
| Judge entries | A new table `panel_judges`: the entry each role of each P run used. | P falls back per role, and its usage rows need whose key paid. `runs` gets no column. |
| Member result route | `POST /runs/{id}/members/{member}/result`, the member's own token. | Under `/runs/`, already on the public host. |
| Member results for P | `GET /runs/{id}/panel/{role}`, P's result token: the result of the role's `done` member; 404 otherwise. | The code graph's pattern. A result can be 10 MB, more than a ConfigMap holds. |
| Usage | `run_usage_models`, part `member` (one row per member attempt and skill) and part `merge` (one per P run, skill and role). `run_usage` of the root run adds each member's tokens; P's own run adds P's. A `usage_recorded` flag (on the member, or the run) read `FOR UPDATE` in the same transaction stops a second count. | Spec: `run_usage` keeps its key; the quota counts all 3 Jobs. |
| Panel names in the comment | Entry names (`panel_judges`, else the members' final entries). | Finding 7. |
| Job P timeout | `--timeout=10m`, `activeDeadlineSeconds` = 10 min + the clone allowance + the deliver allowance (`kube.Deadline`). | Spec "10 min". |

## Review Focus

1. **Chain B off is today.** `TestBuildJobUnchanged` (S2.4) and `TestDispatchNoChainBUnchanged` (S3.4) compare against bytes taken from `main` before any change.
2. **A key in the wrong place.** Job A's and B's Secret hold one LLM key; Job P's holds `llm-api-key` and `llm-api-key-b`, each named once in the Pod spec. Pinned by `TestBuildPanelJobKeys` (S2.4) and `TestE2EPanelMembersStart`, `TestE2EPanelJobP` (S3.5, S3.8).
3. **Chain B rights.** Pinned by `TestPanelChainForeignEntry`, `TestPanelChainRights`, `TestPanelChainOffRights` (S1.5).
4. **The image floor.** With `PanelMinVersion` unset every save is refused (`TestPanelChainRefusedWithoutFloor`, S1.5), and an image that moves later makes "A only" (`TestDispatchPanelImageTooOld`, S3.4).
5. **Members never publish.** `TestE2EPanelMemberPostsNothing` (S3.7).
6. **No false pass.** Both members failed: the review fails, never `success`, never approved (`TestE2EPanelBothFail`, S3.7). Job P past its deadline fails the review (`TestE2EPanelPTimeout`, S3.8).
7. **`off` wins.** `TestLoadChainBRepoOffBeatsTeam` (S1.3), `TestDispatchRepoOffNoPanel` (S3.4).
8. **API files.** Kept until Job P is created, then dropped (`TestE2EAPIPanel`, S3.10).

---

## Phase S1: chain B

Chain B can be saved only when `jobs.image` passes the image check, and the floor is unset until release, so nothing in this phase changes a review on `main`.

### Task S1.1: Migration `0047_panel_chains.sql`

**Files:**
- Create: `internal/store/migrations/0047_panel_chains.sql`
- Test: `internal/store/panel_chains_test.go` (new)

```sql
-- Review panel: chain B. It only adds, so the previous release (migrations up
-- to 0046) still runs on the migrated database and ignores all of it.
--
-- panel_chains and panel_chain_entries: chain B, the same shape as
-- model_chains and model_chain_entries (0045, 0046), at the same 3 levels.
-- No row at a level means that level has no chain B. A stored global chain B
-- is the only global one: with none, a review has no panel.
--
-- dashboard_repos.panel_chain_mode: 'custom' for a repository with its own
-- chain B, 'off' for one that turned the panel off whatever its team or the
-- global level has, 'inherit' otherwise.
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
    CHECK (panel_chain_mode IN ('inherit', 'custom', 'off'));
```

- [ ] **Step 1: Failing tests** (model them on `TestTeamModelsMigrationOnlyAdds` with `applyUpTo`): `TestPanelChainsMigrationOnlyAdds` (every column of 0046 is still there with its type; `model_chains` and its entries unchanged); `TestPanelChainsMigrationPreviousRelease` (after 0047, the 0046-era `INSERT INTO dashboard_repos` and `INSERT INTO model_chains` still succeed); `TestPanelChainOneRowPerTarget` (a second global row fails 23505); `TestPanelChainModeValues` (`inherit`, `custom`, `off` are accepted, `other` fails 23514).
- [ ] **Step 2: Run to fail** (`nc -z localhost 55432` first): `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'PanelChain'`.
- [ ] **Step 3: Write the migration** above.
- [ ] **Step 4: Run them, then the whole store package.** PASS.
- [ ] **Step 5: Commit** `feat(store): tables for chain B`

### Task S1.2: Store: one chain code path for both sets, and `off`

**Files:**
- Modify: `internal/store/chains.go` (`loadChains`, `writeChainEntries`, `removeFromChains`, `Store.Chain`, `Chains`, `ChainsNaming`, `SaveChain`, `ResetRepoChain`, `setRepoModelChain`)
- Modify: `internal/store/team_models.go`, `internal/store/models.go` (the callers of `removeFromChains`), `internal/store/dashboard.go` (`DashboardRepo.PanelChainMode`, `dashboardRepoColumns`, `scanDashboardRepo`)
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

// A repository's chain B mode (DashboardRepo.PanelChainMode).
const (
	PanelInherit = "inherit"
	PanelCustom  = "custom"
	PanelOff     = "off"
)

func (s *Store) ChainOf(ctx context.Context, set ChainSet, scope string, teamID int64, instance, project string) (Chain, bool, error)
func (s *Store) ChainsOf(ctx context.Context, set ChainSet) ([]Chain, error)
func (s *Store) ChainsNamingOf(ctx context.Context, set ChainSet, ref EntryRef) ([]Chain, error)
func (s *Store) SaveChainOf(ctx context.Context, set ChainSet, c Chain, at time.Time) error
func (s *Store) ResetRepoChainOf(ctx context.Context, set ChainSet, instance, project string) error

// RepoPanelMode is the repository's chain B mode; PanelInherit for a
// repository with no dashboard row.
func (s *Store) RepoPanelMode(ctx context.Context, instance, project string) (string, error)

// SetRepoPanelOff turns the panel off for the repository: its chain B row
// goes and its mode is PanelOff, in one transaction.
func (s *Store) SetRepoPanelOff(ctx context.Context, instance, project string, at time.Time) error
```

The table names come only from these two fixed values, never from input. `Chain`, `Chains`, `ChainsNaming`, `SaveChain`, `ResetRepoChain` call the `...Of` form with `ChainA`. Only `ChainA` updates `llm_settings` (global position 1) and `dashboard_repos.model_id`. `ResetRepoChainOf(ChainB)` sets the mode to `inherit` from `custom` or `off`.

- [ ] **Step 1: Failing tests.** `TestChainBRoundTrip` (save and read global, team and repo chain B; a repo save sets `panel_chain_mode = 'custom'` and leaves `chain_mode` and `model_id`); `TestChainBResetInherits`; `TestChainBRepoOffRoundTrip` (custom, then off: the row is gone and `RepoPanelMode` is `off`; reset: `inherit`; save again: `custom`); `TestChainBOffLeavesChainA` (off changes no `model_chains` row, `chain_mode` or `model_id`); `TestDeleteTeamModelLeavesNoChainBEntry` (a team entry in a team chain B and a repo chain B: deleting it removes it from both, later entries move up; a repository in `off` stays `off`); `TestRemoveGlobalModelKeepsChainBEntry` (as chain A: the entry stays and is skipped); the existing chain tests pass with no edit.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'Chain'`.
- [ ] **Step 3: Code.** Build each SQL string from the set's names; `removeFromChains` runs once per set.
- [ ] **Step 4: Run** the whole store package. PASS.
- [ ] **Step 5: Commit** `feat(store): chain B through the chain A code`

### Task S1.3: Load chain B

**Files:**
- Modify: `internal/chain/load.go` (`Load`, `repoChain`, `GlobalChain`, `GlobalRefs`)
- Test: `internal/chain/load_test.go` (new)

**Interfaces:** `func LoadSet(ctx context.Context, st *store.Store, reg *instances.Registry, now time.Time, run store.Run, repo settings.Effective, set store.ChainSet) (Input, error)`; `Load` calls it with `store.ChainA`. For `ChainB`: a merge request's repository in mode `off` gives `Input{Refs: nil}` with `Scope ""` before any other level is read; the global level is the stored global chain B only; an empty result is `Refs: nil`, `Scope ""`: no panel. `Tried` comes from the caller for chain B (the member's earlier attempts, S3.5), not from `run.AttemptOf`.

- [ ] **Step 1: Failing tests.** `TestLoadChainBNoneIsOff` (nothing stored: `Refs == nil`, even with a config `llm:` block and a default entry); `TestLoadChainBRepoBeforeTeamBeforeGlobal`; `TestLoadChainBRepoOffBeatsTeam` (team and global chain B stored, repository `off`: `Refs == nil`); `TestLoadChainBAPIReadsGlobal` (an API review gets the global chain B, never a repository's mode); `TestLoadChainAUnchanged` (the current `Load` behaviour, through the existing callers' tests).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/chain/`.
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

- [ ] **Step 1: Failing tests.** `TestVersionAtLeast` (table: `v0.12.0` vs `v0.12.0` true; `0.11.9` false; `v1.0.0` true; `main`, `0.12`, `0.12.0-rc1` unknown); `TestCarriedSupportUnchanged` (the current table); `TestPanelImage` (floor `""`: every image refused with `no review image supports a review panel yet`; floor `v0.12.0`: `ghcr.io/x/pergent:v0.12.0` ok, `:v0.11.0` refused, `:main` refused as not a version, `@sha256:...` with no tag refused, `host:5000/pergent:v0.12.0` ok).
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/result/ ./internal/config/ -run 'VersionAtLeast|CarriedSupport|PanelImage'`.
- [ ] **Step 3: Code.** Tests set `config.PanelMinVersion` with `t.Cleanup` to restore `""`.
- [ ] **Step 4: Run** both packages and `./internal/engine/ -run Incremental`. PASS.
- [ ] **Step 5: Commit** `feat(config): the review image check for a panel`

### Task S1.5: Chain B API, rights, image check on save, `off`, repository view

**Files:**
- Modify: `internal/dashboard/api_chains.go` (each handler takes a `store.ChainSet`; new handlers are one-line wrappers), `internal/dashboard/api_operator.go:81-82` and `internal/dashboard/api_team_models.go:30-31` (routes), the repository suffix routing beside `.../chain` and `.../chain/reset`, `internal/dashboard/view.go` (`ChainView.Mode`, repository view `panel`), `internal/dashboard/api_read.go`
- Modify: `openapi.yaml`, `internal/dashboard/openapi_test.go` (`openAPISchemas`), `internal/dashboard/routes_test.go`
- Test: `internal/dashboard/api_panel_chains_test.go` (new)

- [ ] **Step 1: Failing tests** (`newEnv`; `e.dashboardRepo` + `e.aliceOn` for a repository alice can see; decode setting values as `any`):
  - `TestPanelChainRefusedWithoutFloor`: admin PUT of a global chain B with the floor unset: 400 `no review image supports a review panel yet`; nothing stored.
  - `TestPanelChainSavedWithFloor`: floor `v0.12.0`, `jobs.image` `...:v0.12.0`: global, team and repo saves work; audit `panel_chain_changed` with names, never a key.
  - `TestPanelChainImageTooOld` / `TestPanelChainImageNotVersion`: 400 with the tag in the message.
  - `TestPanelChainRights`: viewer 403 on a write; a maintainer of the owner team saves the team's and an owned repository's chain B; a non-member gets 404 on the team route.
  - `TestPanelChainForeignEntry`: a maintainer of team A naming team B's entry: 400 (chain A's message).
  - `TestPanelChainOff`: a maintainer of the owner team POSTs `.../panel-chain/off` with the floor unset: 200 (no image check); GET shows `mode: "off"` and no entries; audit detail `scope=repo mode=off`; `POST .../panel-chain/reset` gives `mode: "inherit"`.
  - `TestPanelChainOffRights`: a viewer gets 403 on `.../panel-chain/off`.
  - `TestPanelChainNoKeyInResponses`: every GET body holds no key text.
  - `TestRepositoryViewPanel`: with a chain B in effect the view has `panel: {"on": true, "mode": "inherit", "a": "<entry name>", "b": "<entry name>"}`; with none `{"on": false, "mode": "inherit"}`; in `off` `{"on": false, "mode": "off"}` even with a team chain B.
  - `TestOpenAPIMatches` (existing) passes with the new routes, schemas and `x-pergent-access`.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/dashboard/ -run 'PanelChain|RepositoryViewPanel|OpenAPI'`.
- [ ] **Step 3: Code.** The image check runs in the shared save path after the rights checks and before `SaveChainOf`, for `ChainB` only, never for `off` or reset. The admin route goes through `Dashboard.adminRoute`; team and repository routes are session routes that check in the handler, as chain A's.
- [ ] **Step 4: Run** `go test ./internal/dashboard/ -run OpenAPI` (no DB needed), then the package with the DB variable. PASS.
- [ ] **Step 5: Commit** `feat(dashboard): chain B settings with the chain A rights`

### Task S1.6: Web: chain B editors (waits for PR 74)

**Files:**
- Modify: `web/src/pages/RepoModelChain.tsx` and `web/src/ChainEditor.tsx` (from PR 74), `web/src/pages/TeamModels.tsx`, `web/src/pages/AdminSettings.tsx`, `web/src/pages/RepositoryDetail.tsx`, `web/src/api.ts`
- Test: the matching `*.test.tsx`

- [ ] **Step 1: Failing tests.** The repository's chain page shows a second editor "Chain B (review panel)" with its own Inherit / Custom / Off switch, built from PR 74's `ChainEditor` with the `/panel-chain` routes; choosing Off POSTs `.../panel-chain/off` and shows "Panel: off for this repository"; a 400 from the image check shows its message; a viewer sees it read-only; the repository page shows `Panel: on (A: <entry>, B: <entry>)` or `Panel: off`. A web test of "disabled" uses a case the old code left enabled.
- [ ] **Step 2: Run to fail.** `cd web && npx vitest run src/pages/RepoModelChain.test.tsx src/pages/RepositoryDetail.test.tsx`.
- [ ] **Step 3: Code.**
- [ ] **Step 4:** the web gate (`npm run typecheck && npm run lint && npx vitest run && npm run build`).
- [ ] **Step 5: Commit** `feat(web): chain B editors and the panel line`

### Task S1.7: Docs for chain B

**Files:** `docs/concepts/models.md`, new `docs/concepts/review-panel.md` (status "experimental, off by default; reviews do not use it yet" until S3 lands; the 3 modes, `off` included), `docs/README.md` index, `openapi.yaml` descriptions, `docs/upgrading.md` `## Unreleased` (migration `0047` adds 2 tables and 1 column, drops nothing; the previous release runs on it).

- [ ] **Step 1:** Write the pages; every exact string matches the code.
- [ ] **Step 2:** Phase gate (Global Constraints).
- [ ] **Step 3: Commit** `docs: chain B`

---

## Phase S2: what does not depend on member rows (once the pergent CLI plan is merged code on `master`)

### Task S2.1: Move the pergent pin

- [ ] **Step 1:** `GOFLAGS=-mod=mod go get github.com/gopaytech/pergent@<merged master sha>` (a pseudo-version; no `replace`), then `go mod tidy` (offline: `GOFLAGS=-mod=mod GOPROXY=off go mod tidy` when the modules are cached).
- [ ] **Step 2:** `go vet ./...` and the full test suite (DB variable in the same command). Every existing comment golden is unchanged (pergent's panel fields are `omitempty`, and `Options.Panel` nil writes nothing).
- [ ] **Step 3: Commit** `build: pin pergent with the review panel render fields`

### Task S2.2: Read the panel fields of a result

**Files:**
- Modify: `internal/result/result.go:30-215` (types), `:252-312` (`ToRender`)
- Test: `internal/result/result_test.go`

- [ ] **Step 1: Failing tests.** `TestParsePanelResult` (a P result with `panel`, `skills[].panel`, `tokens_by_model`, `findings[].panel` decodes); `TestToRenderPanelFields` (`render.Finding.Panel` and `render.Skill.Panel` filled, `also` kept); `TestToRenderNoPanelUnchanged` (a result without them renders the same `[]render.Skill` as before, deep-equal with a fixed want); `TestMergeKeepsPanelFields` (`Merge` of a P result keeps `findings[].panel`).
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/result/ -run 'Panel'`.
- [ ] **Step 3: Code.** Types mirror pergent's (`Panel`, `PanelMember`, `SkillPanel`, `PanelJudge`, `FindingPanel`, `ModelTokens`, `ProviderError.Role`).
- [ ] **Step 4: Run.** PASS.
- [ ] **Step 5: Commit** `feat(result): read a panel's result fields`

### Task S2.3: Per-model rows from a result

**Files:**
- Modify: `internal/usage/usage.go` (a new `ModelRows`), `internal/store/usage.go` (the `ModelUsage` type only)
- Test: `internal/usage/usage_test.go`

**Interfaces:**

```go
// store.ModelUsage is what one skill used on one model of a panel.
type ModelUsage struct {
	Skill, Role, Provider, Model string
	In, Out                      int64
	CacheRead, CacheWrite, Reasoning *int64
}

// ModelRows returns the per-model tokens of body's skills (tokens_by_model),
// in the skills' order, A before B; nil for a result without them.
func ModelRows(body []byte, run store.Run) ([]store.ModelUsage, error)
```

- [ ] **Step 1: Failing tests.** `TestModelRows` (2 rows per skill, roles `a` and `b`; the subset rule of `FromResult` for a re-run of failed skills); `TestModelRowsNone` (a result without `tokens_by_model`: nil); `TestModelRowsNegativeDropped`.
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/usage/ -run ModelRows`.
- [ ] **Step 3: Code.** The store write is S3.9's.
- [ ] **Step 4: Run.** PASS.
- [ ] **Step 5: Commit** `feat(usage): read a panel merge's tokens per model`

### Task S2.4: The member and merge Jobs

**Files:**
- Modify: `internal/kube/job.go:86-135` (`JobParams`, `Secrets`), `:186-200` (labels), `:269-464` (`BuildJob`), `:525-540` (`BuildSecret`)
- Create: `internal/kube/panel.go` (the `panel-inputs` init container), `internal/kube/testdata/review-job.golden.json`
- Modify: `cmd/pergent-sh/graph.go` (a `get-panel` subcommand sharing `get-graph`'s fetch code: size cap 10 MiB, file mode 0600, always exit 0, a 404 or a failure after 3 tries writes no file)
- Test: `internal/kube/job_test.go`, `internal/kube/panel_test.go`, `cmd/pergent-sh/graph_test.go`

**Interfaces:**

```go
// in JobParams:
	// SkipSkillVerify makes the Job a review panel's member: pergent leaves
	// out each skill's verification run (--no-skill-verify). False leaves the
	// Job as it was before panels.
	SkipSkillVerify bool
	// PanelMemberID and PanelRole label a panel's Jobs: the member's ID and
	// "a" or "b" for a member, "" and "p" for a merge. "" and "" for a review.
	PanelMemberID, PanelRole string
	// Panel makes the Job a review panel's merge (pergent agent --panel):
	// its init container fetches the member results, and LLM is model A,
	// Panel.B model B. Nil leaves the Job a review.
	Panel *PanelParams

// PanelParams is what a merge Job needs beyond a review Job's checkout.
type PanelParams struct {
	AURL, BURL string     // /runs/<id>/panel/a and /b on the callback URL
	B          config.LLM // model B's provider, model and base URL; its key is Secrets.LLMKeyB
	NoJudge    bool       // --panel-no-judge: the quota stopped the cross-check
	Image      string     // pergent.sh's own image, for get-panel
}

// Secrets gains LLMKeyB, stored as "llm-api-key-b" only when Panel is set
// and the key is not empty.
```

Labels: `pergent.sh/run` stays the run's ID on every panel Job; `pergent.sh/panel-member` (the member ID) and `pergent.sh/panel-role` (`a`, `b`, `p`) are set only when `PanelRole` is set.

- [ ] **Step 1: Write the golden first, on `main` before any change:** `TestBuildJobUnchanged` builds a review Job, its ConfigMap and its Secret from fixed `JobParams` covering every optional input (instructions, previous, carried, dismissals, description, linked, CI, graph, check-deleted) and compares the JSON with `testdata/review-job.golden.json`. Generate it once with `-update`, commit it with the test (`test(kube): pin the review Job's spec`), and only then change `BuildJob`.
- [ ] **Step 2: Failing tests.** `TestBuildMemberJob` (`SkipSkillVerify` adds `--no-skill-verify` and the 2 labels and nothing else: diff against the golden); `TestBuildPanelJob` (args `--panel --panel-a=/panel/a.json --panel-b=/panel/b.json`, `--timeout=10m0s`, no `--previous`/`--carried`/CI/description/linked/graph inputs; env `PERGENT_PANEL_B_PROVIDER`, `_MODEL`, `_BASE_URL`; init containers `checkout` then `panel-inputs`); `TestBuildPanelJobKeys` (the Secret holds `llm-api-key` and `llm-api-key-b`; `OPENCODE_API_KEY` reads the first, `PERGENT_PANEL_B_API_KEY` the second, each once, only in the `pergent` container; `panel-inputs` gets only `PERGENT_RESULT_TOKEN`); `TestBuildPanelJobOneKey` (an empty `LLMKeyB`: no `llm-api-key-b` and no `PERGENT_PANEL_B_API_KEY`); `TestBuildPanelJobNoJudge` (`--panel-no-judge`, no LLM key at all); `TestGetPanelNotFoundWritesNothing` (`cmd/pergent-sh`: a 404 exits 0 with no file); `TestBuildJobUnchanged` still passes.
- [ ] **Step 3: Run to fail.** `go test -count=1 ./internal/kube/ ./cmd/pergent-sh/ -run 'BuildJob|BuildMember|BuildPanel|GetPanel'`.
- [ ] **Step 4: Code.**
- [ ] **Step 5: Run.** PASS. Then `go vet ./...` (a `go build ./cmd/...` would drop a binary in the cwd).
- [ ] **Step 6: Commit** `feat(kube): member and merge Jobs of a review panel`

### Task S2.5: The member results route

**Files:**
- Create: `internal/callback/panel.go` (`PanelHandler`, against `PanelResults { PanelMemberResult(ctx context.Context, runID, token, role string) ([]byte, bool, error) }`)
- Modify: `internal/server/server.go:78-120` (`GET /runs/{id}/panel/{role}`, registered only when `Deps.Panel` is set)
- Test: `internal/callback/panel_test.go`, `internal/server/server_test.go`

- [ ] **Step 1: Failing tests** with a fake `PanelResults`: 200 with the bytes for a good token and role `a` or `b`; 404 for a bad token, a role other than `a`/`b`, an unknown run, or no result; the body never logged; `server_test` lists the route on the public mux only.
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/callback/ ./internal/server/ -run Panel`.
- [ ] **Step 3: Code.** The store side is S3.2's `Store.PanelMemberResult`; until then `server.Deps.Panel` is nil.
- [ ] **Step 4: Run.** PASS.
- [ ] **Step 5: Commit** `feat(callback): serve a panel merge its member results`

---

## Phase S3: panel reviews

Each task keeps "chain B off = today" and leaves `main` green. S3 needs S1 and S2 merged.

### Task S3.1: Migration `0048_panel_members.sql`

**Files:**
- Create: `internal/store/migrations/0048_panel_members.sql`
- Test: `internal/store/panel_test.go` (new)

```sql
-- Review panel: the members A and B of a panel review, the entries its merge
-- (Job P) judged with, and what each used per model. It only adds, so the
-- previous release (up to 0047) still runs on the migrated database. It
-- ignores these tables. A panel review's run waits (state 'waiting',
-- skip_reason 'panel_members') while its members run: the previous release
-- never claims a waiting run.
--
-- panel_members: one row per attempt of member A or B. run_id is the
-- review's first run (Job P's later attempts point at it with attempt_of).
-- One queued, running or reported row per run and role. 'off' is the one row
-- of a review whose panel could not start, with the reason.
CREATE TABLE panel_members (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id           uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    role             text        NOT NULL CHECK (role IN ('a', 'b')),
    attempt_of       uuid        REFERENCES panel_members (id) ON DELETE CASCADE,
    state            text        NOT NULL DEFAULT 'queued' CHECK (state IN
        ('queued', 'running', 'reported', 'done', 'failed', 'fell_back', 'superseded', 'off')),
    job_name         text        NOT NULL DEFAULT '',
    token_hash       bytea,
    token_expires_at timestamptz,
    result           jsonb,
    failure_reason   text        NOT NULL DEFAULT '',
    failure_step     text        NOT NULL DEFAULT '',
    failure_detail   text        NOT NULL DEFAULT '',
    pod_name         text        NOT NULL DEFAULT '',
    checkout_seen    integer     NOT NULL DEFAULT 0,
    provider         text        NOT NULL DEFAULT '',
    model            text        NOT NULL DEFAULT '',
    model_source     text        NOT NULL DEFAULT '',
    model_entry_id   text        NOT NULL DEFAULT '',
    model_entry_name text        NOT NULL DEFAULT '',
    model_team_id    bigint,
    chain_position   integer     NOT NULL DEFAULT 0,
    chain_length     integer     NOT NULL DEFAULT 0,
    fallback_class   text        NOT NULL DEFAULT '',
    attempts         integer     NOT NULL DEFAULT 0,
    usage_recorded   boolean     NOT NULL DEFAULT false,
    cleaned_up       boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    claimed_at       timestamptz,
    dispatched_at    timestamptz,
    reported_at      timestamptz,
    not_before       timestamptz
);
CREATE UNIQUE INDEX panel_members_live_idx ON panel_members (run_id, role)
    WHERE state IN ('queued', 'running', 'reported');
CREATE UNIQUE INDEX panel_members_off_idx ON panel_members (run_id) WHERE state = 'off';
CREATE INDEX panel_members_run_idx ON panel_members (run_id, created_at);
CREATE INDEX panel_members_state_idx ON panel_members (state, created_at);

CREATE TABLE panel_member_logs (
    member_id  uuid        PRIMARY KEY REFERENCES panel_members (id) ON DELETE CASCADE,
    text       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX panel_member_logs_created_idx ON panel_member_logs (created_at);

CREATE TABLE panel_member_diagnostics (
    member_id        uuid        PRIMARY KEY REFERENCES panel_members (id) ON DELETE CASCADE,
    clone_log        text        NOT NULL,
    clone_log_masked text        NOT NULL,
    pod_state        text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX panel_member_diagnostics_created_idx ON panel_member_diagnostics (created_at);

-- panel_judges: the entry each role of a Job P run judged with.
CREATE TABLE panel_judges (
    run_id           uuid    NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    role             text    NOT NULL CHECK (role IN ('a', 'b')),
    provider         text    NOT NULL,
    model            text    NOT NULL,
    model_source     text    NOT NULL,
    model_entry_id   text    NOT NULL,
    model_entry_name text    NOT NULL,
    model_team_id    bigint,
    chain_length     integer NOT NULL,
    PRIMARY KEY (run_id, role)
);

-- run_usage_models: tokens per model of a panel review. part 'member' is one
-- row per member attempt and skill; part 'merge' one per Job P run, skill
-- and role. run_usage keeps one row per run and skill, the total.
CREATE TABLE run_usage_models (
    id          bigserial   PRIMARY KEY,
    run_id      uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    member_id   uuid        REFERENCES panel_members (id) ON DELETE CASCADE,
    part        text        NOT NULL CHECK (part IN ('member', 'merge')),
    role        text        NOT NULL CHECK (role IN ('a', 'b')),
    skill       text        NOT NULL,
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
    CHECK ((part = 'member') = (member_id IS NOT NULL))
);
CREATE UNIQUE INDEX run_usage_models_member_key ON run_usage_models (member_id, skill) WHERE part = 'member';
CREATE UNIQUE INDEX run_usage_models_merge_key ON run_usage_models (run_id, skill, role) WHERE part = 'merge';
CREATE INDEX run_usage_models_reported_idx ON run_usage_models (reported_at);
```

- [ ] **Step 1: Failing tests.** `TestPanelMembersMigrationOnlyAdds` (every 0047 column is still there with its type; `runs_live_idx` unchanged); `TestPanelMembersMigrationPreviousRelease` (after 0048 the 0047-era inserts into `runs` and `run_usage` succeed; deleting a run with members, judges and usage rows cascades); `TestPanelMembersOneLivePerRole` (a second queued row for (run, `a`) fails 23505; a `fell_back` row and a queued row of one role are fine; a second `off` row fails); `TestRunUsageModelsKeys` (a member row needs `member_id`; a second member row per member and skill fails 23505; a second merge row per run, skill and role fails).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'PanelMembers|RunUsageModels'`.
- [ ] **Step 3: Write the migration** above.
- [ ] **Step 4: Run them, then the whole store package.** PASS.
- [ ] **Step 5: Commit** `feat(store): tables for a panel's members, judges and usage per model`

### Task S3.2: Store: the member state machine

**Files:**
- Create: `internal/store/panel.go`
- Modify: `internal/store/runs.go` (`CountRunning`, `WaitPanel`), `internal/store/dashboard.go` (`PruneRunLogs` also prunes the member logs and diagnostics)
- Test: `internal/store/panel_test.go`

**States:** `queued` → `running` (claimed) → `reported` (result or failure) → `done` (has a result) or `failed`; `reported` → `fell_back` when a quota, billing or auth error moves it to its next entry; any live state → `superseded` when its run is gone. `off` never moves.

**Interfaces:**

```go
// WaitPanel: a panel review's run waits while its members A and B review.
const WaitPanel = "panel_members"

type MemberState string

const (
	MemberQueued     MemberState = "queued"
	MemberRunning    MemberState = "running"
	MemberReported   MemberState = "reported"
	MemberDone       MemberState = "done"
	MemberFailed     MemberState = "failed"
	MemberFellBack   MemberState = "fell_back"
	MemberSuperseded MemberState = "superseded"
	MemberOff        MemberState = "off"
)

// PanelMember is one attempt of member A or B of a panel review.
type PanelMember struct {
	ID, RunID, Role                                 string
	AttemptOf                                       *string
	State                                           MemberState
	JobName                                         string
	TokenExpiresAt                                  time.Time
	Result                                          []byte
	FailureReason, FailureStep, FailureDetail       string
	PodName                                         string
	CheckoutSeen                                    int
	Provider, Model                                 string
	Entry                                           RunEntry
	Attempts                                        int
	CreatedAt, DispatchedAt, ReportedAt, NotBefore time.Time
}

// StartPanel makes the claimed, not dispatched run runID a panel review: it
// waits (WaitPanel) with dispatched_at now, and one queued member per role is
// added, in one transaction. started is false when the run is no longer
// claimed (superseded meanwhile).
func (s *Store) StartPanel(ctx context.Context, runID string, now time.Time) (started bool, err error)
// RecordPanelOff keeps why the run's panel could not start (its one 'off' row).
func (s *Store) RecordPanelOff(ctx context.Context, runID, reason string) error
// PanelMembers returns the members of the review whose first run is root, oldest first.
func (s *Store) PanelMembers(ctx context.Context, root string) ([]PanelMember, error)
func (s *Store) ClaimQueuedMembers(ctx context.Context, limit int, now time.Time, skip ...string) ([]PanelMember, error)
func (s *Store) SetMemberEntry(ctx context.Context, id string, e RunEntry, provider, model string) error
// MarkMemberDispatched records the member's Job and token hash; first is true
// when no other member of its run had a Job yet. ErrNotRunning when the
// member is not running or its run no longer waits for it.
func (s *Store) MarkMemberDispatched(ctx context.Context, id, jobName string, tokenHash []byte, expires, now time.Time) (first bool, err error)
func (s *Store) RequeueMember(ctx context.Context, id string, notBefore time.Time) error
func (s *Store) PostponeMember(ctx context.Context, id string, notBefore time.Time) error
func (s *Store) StoreMemberResult(ctx context.Context, id, token string, body []byte, now time.Time) (ResultOutcome, error)
func (s *Store) ReportMemberFailure(ctx context.Context, id string, f Failure, now time.Time) (bool, error)
func (s *Store) ReplaceMemberWithFailure(ctx context.Context, id string, f Failure) error
func (s *Store) MembersToSettle(ctx context.Context) ([]PanelMember, error)
func (s *Store) SettleMember(ctx context.Context, id string) (MemberState, error) // reported → done (result) or failed
func (s *Store) EnqueueMemberAttempt(ctx context.Context, from PanelMember, m Mark) (id string, created bool, err error)
// ReleasePanel acts when every role of the review whose first run is root has
// a done or failed member and none is live: with at least one done, the
// waiting run is queued again (skip_reason '', not_before NULL); with both
// failed it is reported with both reasons. Otherwise nothing changes.
func (s *Store) ReleasePanel(ctx context.Context, root string, now time.Time) (released, failed bool, err error)
// SupersedePanel supersedes a waiting panel run and its live members.
func (s *Store) SupersedePanel(ctx context.Context, runID, reason string) error
// SupersedeOrphanMembers supersedes live members whose review has no live run.
func (s *Store) SupersedeOrphanMembers(ctx context.Context) (int, error)
func (s *Store) MembersToCleanUp(ctx context.Context) ([]PanelMember, error)
func (s *Store) MarkMemberCleanedUp(ctx context.Context, id string) error
func (s *Store) RunningMembers(ctx context.Context) ([]PanelMember, error)
func (s *Store) RecoverUnlaunchedMembers(ctx context.Context, olderThan time.Time) (int, error)
func (s *Store) SetMemberPod(ctx context.Context, id, pod string) error
func (s *Store) AdvanceMemberCheckoutSeen(ctx context.Context, id string, seen int) error
// PanelMemberResult is the result of the role's done member of run runID's
// review, when token is run runID's live result token (S2.5's interface).
func (s *Store) PanelMemberResult(ctx context.Context, runID, token, role string) ([]byte, bool, error)
func (s *Store) PutMemberLog(ctx context.Context, id, text string) error
func (s *Store) MemberLog(ctx context.Context, id string) (string, bool, error)
func (s *Store) PutMemberDiagnostics(ctx context.Context, id string, d Diagnostics) error
func (s *Store) MemberDiagnostics(ctx context.Context, id string) (Diagnostics, bool, error)
func (s *Store) SetPanelJudge(ctx context.Context, runID, role string, e RunEntry, provider, model string) error
func (s *Store) PanelJudges(ctx context.Context, runID string) (map[string]RunEntry, error)
```

`StartPanel` in one transaction, under `lockMR`:

```sql
UPDATE runs SET state = 'waiting', skip_reason = 'panel_members', dispatched_at = $2
WHERE id = $1::uuid AND state = 'running' AND job_name = '';
-- only when one row changed:
INSERT INTO panel_members (run_id, role) VALUES ($1::uuid, 'a'), ($1::uuid, 'b');
```

`ReleasePanel`'s failure text: `both review panel members failed: A: <A's reason>; B: <B's reason>` (each cut with `cutText`). `CountRunning` adds `SELECT count(*) FROM panel_members WHERE state = 'running'`. `StoreMemberResult` keeps the result only while the member is running and its run still waits (else `ResultDiscarded`), checks the token as `StoreResult` does (hash, expiry, one use) and refuses a second result (`ResultConflict`).

- [ ] **Step 1: Failing tests:** `TestStartPanelWaitsAndQueuesMembers`; `TestStartPanelSupersededRun` (false, no rows); `TestClaimQueuedMembersSkipsUnconnected`; `TestCountRunningCountsMembers`; `TestMemberResultToken` (wrong token 401 outcome, a second post conflict, expired token unauthorized); `TestMemberResultDiscardedAfterSupersede`; `TestEnqueueMemberAttemptPointsAtFirst` (from reported only: `fell_back`, new queued row, `attempt_of` the first attempt, mark stored); `TestReleasePanelOneDone`; `TestReleasePanelBothFailed` (reported with both reasons); `TestReleasePanelWaitsForLive`; `TestSupersedePanelSupersedesMembers`; `TestSupersedeOrphanMembers` (`EnqueueRun` of a new head supersedes the waiting run; the sweep supersedes its live members, leaves done ones); `TestPanelMemberResult` (the role's done member for the run's token; nothing for another run, role or state; a later P attempt reads its root's members); `TestRecordPanelOffOnce`; `TestPruneRunLogsPrunesMembers`.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'Panel|Member'`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the whole store package. PASS.
- [ ] **Step 5: Commit** `feat(store): a panel's members and their states`

### Task S3.3: Store: a waiting panel is a live review

**Files:**
- Modify: `internal/store/runs.go` (`StartRun`, `liveRerunRunSQL`, `liveStartRunSQL`, `WaitingChanges`, `ClaimQueued`), `internal/store/dashboard_read.go` (`outcomeSQL`), `internal/store/apireviews.go` (`apiStatusSQL`), `internal/store/apiclients.go` (`DeleteAPIClient`), `internal/reviewapi/body.go` (`StatusOf`), `internal/dashboard/api_commands.go:172` (start)
- Test: `internal/store/panel_test.go`, `internal/reviewapi/body_test.go`, `internal/dashboard/api_commands_test.go`

```go
// panelWaitingSQL is a panel review's run while its members run: live, and on
// its way.
const panelWaitingSQL = `(state = 'waiting' AND skip_reason = 'panel_members')`
```

`liveRerunRunSQL` and `liveStartRunSQL`: `AND (state IN ('queued', 'running', 'reported') OR ` + panelWaitingSQL + `)`. `StartRun`'s first update: `AND state = 'waiting' AND skip_reason <> 'panel_members'`. `WaitingChanges`: `AND r.skip_reason <> 'panel_members'`. `outcomeSQL`: `WHEN 'waiting' THEN CASE WHEN r.skip_reason = 'panel_members' THEN 'running' ELSE 'waiting' END`. `apiStatusSQL["running"]` and `StatusOf`: a waiting panel is `running`. `ClaimQueued`'s in-flight count also counts a client's waiting panels. `DeleteAPIClient` also locks a client's waiting panels and returns their live member Jobs to delete.

- [ ] **Step 1: Failing tests:** `TestStartRunLeavesPanelAlone`; `TestRerunWhilePanelRuns` (`ReviewActiveForRerun` true; `EnqueueRerun` returns the run, created false); `TestWaitingChangesHidesPanel`; `TestDashboardOutcomePanelRunning`; `TestAPIStatusPanelRunning` (store filter and `StatusOf`); `TestClaimQueuedCountsPanelInFlight` (`max_in_flight` 1 and one waiting panel: the next API review stays queued); `TestDeleteAPIClientStopsPanel`; `TestStartButtonRefusesPanel` (409 `msgNotWaiting`).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ ./internal/reviewapi/ ./internal/dashboard/ -run 'Panel'`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the 3 packages. PASS.
- [ ] **Step 5: Commit** `feat(store): a panel waiting for its members is a live review`

### Task S3.4: Dispatch: the split, the image check and "A only"

**Files:**
- Create: `internal/engine/panel.go`, `internal/engine/panel_test.go`, `internal/engine/testdata/dispatch-job.golden.json`
- Modify: `internal/engine/dispatch.go` (after `SetRunEntry` at `:588`, before `mintToken` at `:592`), `internal/engine/chain.go` (`resolveB`), `internal/chain/chain.go` (`Input.Avoid`, skip reason `same_model`)
- Test: `internal/engine/panel_test.go`, `internal/chain/chain_test.go`

- [ ] **Step 1: The golden first, on `main` before any change:** `TestDispatchJobUnchanged` dispatches a run with every optional input, reads the Job, ConfigMap and Secret from the fake clientset, replaces the run ID, Job name and token with fixed text, and compares with `testdata/dispatch-job.golden.json` (`-update` writes it). Commit it alone: `test(engine): pin the review Job dispatch starts`.
- [ ] **Step 2: Failing tests:**
  - `TestResolveAvoid` (`internal/chain`): an entry whose provider and model match `Avoid` (any case) is skipped with reason `same_model`; a chain of only that entry is `Exhausted`.
  - `TestDispatchNoChainBUnchanged`: no chain B: the golden still matches, no `panel_members` row, the run is running with its Job.
  - `TestDispatchSplitsPanel`: floor and image set, chain B stored: the run waits (`panel_members`), `dispatched_at` set, 2 queued members, no Job, no status yet.
  - `TestDispatchPanelImageTooOld`: floor above the image's tag: today's Job, and an `off` row `the review image does not support it`.
  - `TestDispatchPanelQuotaStopsB`: a run asked for under a used-up instance quota, chain B with global entries only: today's Job, `off` row `the token quota stopped model B`.
  - `TestDispatchPanelSameModel`: chain B's only entry is A's model: today's Job, `off` row `model B is the model A runs on`.
  - `TestDispatchRepoOffNoPanel`: team chain B, repository `off`: today's Job and no row.
- [ ] **Step 3: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/chain/ ./internal/engine/ -run 'Avoid|Dispatch.*Panel|DispatchNoChainB|DispatchRepoOff'`.
- [ ] **Step 4: Code.**

```go
// Why a panel review runs as one review (the 'off' row's reason, after
// "panel off: " in the footer).
const (
	panelOffImage = "the review image does not support it"
	panelOffQuota = "the token quota stopped model B"
	panelOffSame  = "model B is the model A runs on"
	panelOffNone  = "no entry of chain B can be used"
)

// panel decides whether the run is a panel review, after every check a
// review passes: split is true when StartPanel made it one. off is the reason
// it is not, "" when there is no chain B. With no chain B nothing is written.
func (d *dispatcher) panel(ctx context.Context, repo settings.Effective, instanceUsedUp, askedFor bool) (split bool, err error)
```

  Order: `LoadSet(ChainB)` empty → return false (no write). `config.PanelImage(e.Config.Jobs.Image)` fails → `RecordPanelOff(panelOffImage)`. `resolveB` with `Avoid` = A's provider/model: `Hold` → `panelOffQuota`; `Exhausted` only by `same_model` → `panelOffSame`; other `Exhausted` or `Wait` → `panelOffNone`. With an off reason the review goes on as today. Else `StartPanel` and `e.Wake()`: the dispatch returns before `mintToken`.
- [ ] **Step 5: Run** the 2 packages. PASS.
- [ ] **Step 6: Commit** `feat(engine): a review with chain B starts a panel`

### Task S3.5: Dispatch: a member's Job

**Files:**
- Modify: `internal/engine/dispatch.go` (`dispatcher.stage`, `dispatcher.member`; the trust block `:272`, the quota and resolve block `:316-342`, the CI gate `:344`, the run writes `:530-590`, the launch `:592-720`; `fail`, `retry`, `requeue`, `postpone`, `supersede`, `supersedeDraft` `:979-1127`), `internal/engine/panel.go` (`DispatchOnce` claims members, `launchMember`, `resolveMember`)
- Test: `internal/engine/e2e_panel_test.go` (new)

```go
// stage is what a dispatch starts: a review (today's), a panel member's Job
// or a panel's merge (Job P, S3.8).
type stage int

const (
	stageReview stage = iota
	stageMember
	stageMerge
)
```

In `stageMember` the dispatcher: skips trust, the quota hold and the CI gate; resolves the member's chain (`a`: chain A; `b`: chain B with `Avoid` = A's newest entry, `Tried` = its role's earlier attempts); writes nothing to the run (it waits, and the run setters refuse it); and fails the member with `the review's inputs changed while the panel ran` where `subset` would write to the run. Its helpers act on the member: `fail` → `ReportMemberFailure`; `retry` → `RequeueMember` with backoff, a failure on the 5th; `postpone` → `PostponeMember`; `supersede`/`supersedeDraft` → `SupersedePanel`. A moved head calls `EnqueueRun` as today (the sweep takes the members). `launchMember`: `Name = kube.JobName(member.ID)`, `SkipSkillVerify: true`, `PanelMemberID`, `PanelRole`, `ResultURL = <callback>/runs/<run>/members/<member>/result`, the member's entry as `LLM` and its key alone, then `SetMemberEntry` and `MarkMemberDispatched`; the running status is set only when `first` is true. `DispatchOnce` claims queued members first, then runs, within `jobs.max_running` (`CountRunning` counts both).

- [ ] **Step 1: Failing tests** (on `newFallbackE2E` with a chain B helper `savePanelChain`):
  - `TestE2EPanelMembersStart`: pass 1 splits; pass 2 starts 2 member Jobs; each Secret has one LLM key (A's, B's), args hold `--no-skill-verify`, labels name the run, the member and the role; the running status is set once.
  - `TestE2EPanelMemberSkipsChecks`: a started untrusted push and a CI-waiting repository: the members are not held.
  - `TestE2EPanelBSkipsAModel`: chain B `[A's model, other]`: B runs on `other`.
  - `TestE2EPanelMemberHeadMoved`: the head moves before a member starts: a new run is queued, the panel run and its members are superseded.
  - `TestE2EPanelMemberMRClosed`: the panel run and its members are superseded.
  - `TestE2EPanelMemberRetryGivesUp`: 5 launch errors fail the member only.
  - `TestE2EPanelMaxRunning`: `jobs.max_running` 1: one member Job at a time.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run 'E2EPanel'`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the engine package. PASS.
- [ ] **Step 5: Commit** `feat(engine): start a panel member's Job`

### Task S3.6: Member Jobs: callback, watcher, cleanup, logs, diagnostics

**Files:**
- Create: `internal/callback/member.go` (`MemberHandler`)
- Modify: `internal/callback/callback.go` (move the checks of `check` and `checkSkills` into `checkResult(res *result.Result, run store.Run) string`, used by both handlers), `internal/server/server.go` (`POST /runs/{id}/members/{member}/result`), `internal/engine/watch.go` (`watchMember`, `RecoverUnlaunchedMembers` in `WatchOnce`), `internal/engine/logs.go` (a member's log and diagnostics go to its own tables), `internal/engine/publish.go` (`CleanupOnce`: `SupersedeOrphanMembers`, then `MembersToCleanUp`)
- Test: `internal/callback/member_test.go`, `internal/server/server_test.go`, `internal/engine/e2e_panel_test.go`, `internal/engine/watch_test.go`

`watchMember` does what `watch` does, on the member row: pod and checkout seen, the log of a finished Job, a stuck Job deleted and failed, a Job gone or finished without a result failed after `watchGrace`. It deletes an API member's files Secret after its checkout (that Secret is the member Job's own; the stored copy stays, S3.10). It sets no clone time and raises no attention item. A member failure never touches the run.

- [ ] **Step 1: Failing tests:** `TestMemberHandlerStoresResult`; `TestMemberHandlerBadToken` (401, nothing stored); `TestMemberHandlerWrongCommit` (the member fails with `the result is for a different commit than the one this run reviews`); `TestMemberHandlerUnknownSkill`; `TestMemberHandlerWrongRun` (a member ID under another run's path: 401); `TestServerMemberRouteOnPublicMux`; `TestCheckResultUnchanged` (the existing callback tests pass with no edit); `TestE2EPanelMemberGone` (a vanished Job fails the member with `reasonDisappeared`, the run still waits); `TestE2EPanelMemberDeadline`; `TestE2EPanelMemberLog` (the log lands in `panel_member_logs`, not `run_logs`); `TestRecoverUnlaunchedMembers`; `TestE2EPanelCleanup` (a push while members run: member Jobs deleted, members cleaned up, the canceled status set once).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/callback/ ./internal/server/ ./internal/engine/ -run 'Member|E2EPanel'`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the 3 packages. PASS.
- [ ] **Step 5: Commit** `feat(engine): watch, keep and clean up a panel's member Jobs`

### Task S3.7: Settle members; release or fail the run

**Files:**
- Modify: `internal/engine/panel.go` (`settleMembers`), `internal/engine/publish.go` (`PublishOnce` calls `settleMembers` before `RunsToPublish`), `internal/engine/fallback.go` (the mark and audit shared with members)
- Test: `internal/engine/e2e_panel_test.go`

For each reported member: a `provider_error` of class quota, billing or auth on an entry of a chain of 2 or more, on a merge request's review, marks the entry, writes `model_marked` to the audit, and calls `EnqueueMemberAttempt`; otherwise `SettleMember`. Then `ReleasePanel(root)`: released wakes the dispatcher; failed wakes the publisher, which publishes the failure as today. Nothing is posted for a member: no comment, no note, no thread, no approval, no status.

- [ ] **Step 1: Failing tests:**
  - `TestE2EPanelMemberPostsNothing`: between the member results and Job P, the fake forge sees no comment, note, thread or approval.
  - `TestE2EPanelMemberFallsBack`: B's quota: B's entry marked, the member `fell_back`, its next attempt on B's next entry with `attempt_of`; A is untouched.
  - `TestE2EPanelBFallsBackToAModel`: B's next entry is A's model: skipped, B takes the one after.
  - `TestE2EPanelBNoneLeft`: B's only other entry is A's model: B fails; the run is released for Job P with A only.
  - `TestE2EPanelBothFail`: both fail: the run is published as failed with both reasons, the status is the failure status, no approval, never `success`.
  - `TestE2EPanelAFailsBPasses`: the run is released; Job P starts with B's result only (checked in S3.8).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run 'E2EPanel'`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the engine package. PASS.
- [ ] **Step 5: Commit** `feat(engine): settle a panel's members and release its merge`

### Task S3.8: Job P

**Files:**
- Modify: `internal/engine/panel.go` (`judgeEntries`), `internal/engine/dispatch.go` (a claimed run whose review has a member row other than `off` is `stageMerge`), `internal/engine/fallback.go` (`fallBack`: a `provider_error.role` falls back that role's judge entry)
- Test: `internal/engine/e2e_panel_test.go`

In `stageMerge` the dispatcher skips trust, the quota hold, CI and the run writes (the run keeps member A's entry from the split). `judgeEntries` picks, per role, the role's final done member's entry when it still has a key, has no mark and (for B) is not A's model; else the next entry of the role's chain (marks skip, B avoids A's model); a role with no done member gets no key. `NoJudge` when no role can judge, when the repository quota is used up, or when the instance quota is used up and a judge entry is global. Then `SetPanelJudge` per role, `kube.JobParams.Panel` (S2.4) with `AURL`/`BURL` = `<callback>/runs/<run>/panel/a` and `/b`, `Timeout` 10 min, and today's launch, `MarkDispatched` and result route. A P result whose `provider_error` has a `role` and a class of quota, billing or auth, on a role whose `panel_judges.chain_length` is 2 or more, marks that judge entry and calls `EnqueueAttempt` as today: the next P attempt picks that role again.

- [ ] **Step 1: Failing tests:**
  - `TestE2EPanelJobP`: both members done: one P Job with `--panel`, `--timeout=10m0s`, `llm-api-key` = A's key, `llm-api-key-b` = B's key, `PERGENT_PANEL_B_*` = B's entry; 2 `panel_judges` rows.
  - `TestE2EPanelPBOnly`: A failed: P's Secret holds B's key only.
  - `TestE2EPanelPQuotaNoJudge`: the instance quota is used up and the judge entries are global: `--panel-no-judge` and no LLM key.
  - `TestE2EPanelPFallsBackRoleB`: P's result has a quota `provider_error` on role `b`: B's judge entry is marked, a new P attempt runs on B's next entry, the members are untouched.
  - `TestE2EPanelPTimeout`: P's Job hits its deadline: the run fails through the watcher as today, never `success`.
  - `TestE2EPanelPSkipsChecks`: a started untrusted push: P's dispatch does not hold it.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run 'E2EPanelP|E2EPanelJobP'`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the engine package. PASS.
- [ ] **Step 5: Commit** `feat(engine): start a panel's merge Job`

### Task S3.9: Publish P and record usage

**Files:**
- Modify: `internal/engine/publish.go` (`publish`: `o.Panel`, `o.PanelOffNote`; `hideModel` also hides the members' and judges' providers, models and base URLs), `internal/policy/policy.go` (`Outcome.Panel *render.PanelNames`, `Outcome.PanelOffNote`, `PanelOffLine`, `renderOptions` passes `Panel`, `withFooterNote` adds the off line)
- Modify: `internal/store/usage.go` (`RecordMemberUsage`, `RecordUsage` takes the model rows, `MembersWithoutUsage`), `internal/callback/callback.go` and `member.go` (usage writes), the usage fill at start
- Test: `internal/engine/e2e_panel_test.go`, `internal/policy/policy_test.go`, `internal/store/usage_test.go`

```go
// PanelOffLine is the footer line of a review whose panel could not start.
func PanelOffLine(reason string) string { return "panel off: " + reason }

// RecordMemberUsage stores what each skill of member id used: one
// run_usage_models row per skill (part member, the member's entry), and the
// tokens added to run_usage of the member's run, then marks the member
// recorded, in one transaction; a member already recorded changes nothing.
func (s *Store) RecordMemberUsage(ctx context.Context, id string, skills []SkillUsage) error

// RecordUsage, for a run with model rows, also stores them (part merge, the
// judge entries of panel_judges) and adds its own tokens to run_usage rows a
// member made, setting their verdict and counts; without model rows it is
// today's statement.
func (s *Store) RecordUsage(ctx context.Context, id string, skills []SkillUsage, models []ModelUsage) error
```

Only Job P's run is published, through today's `publish`. `o.Panel` names are the judges' entry names (`panel_judges`), else the members' final entry names; with no member row (`off`), `o.PanelOffNote = PanelOffLine(reason)`.

- [ ] **Step 1: Failing tests:**
  - `TestE2EPanelPublishesOnce`: one comment with `Panel: <entry A> (A) + <entry B> (B)`, no provider or model text, status `success` for a clean merge, the approval on P's run only.
  - `TestPublishPanelOffNote`: an `off` row gives the footer `panel off: the review image does not support it`.
  - `TestPanelCommentHidesBothModels`: B's provider and model in a finding's text are hidden.
  - `TestRecordMemberUsage`: member rows with the member's source and team, `run_usage` gets the tokens, a second call adds nothing.
  - `TestRecordUsagePanelTotal`: after A, B and P, `run_usage` of the review holds A + B + P per skill with P's verdict.
  - `TestRecordUsageWithoutModelsUnchanged`.
  - `TestE2EPanelQuotaCountsAll`: `quota.Check` sees the tokens of A, B and P.
  - `TestMembersWithoutUsageFill`: a member result whose usage write was lost is filled at start.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/policy/ ./internal/store/ ./internal/engine/ -run 'Panel|Usage'`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the 3 packages. PASS.
- [ ] **Step 5: Commit** `feat(engine): publish a panel's merge and count every model's tokens`

### Task S3.10: API reviews and re-runs

**Files:**
- Modify: `internal/engine/apireview.go` (`dispatchAPI` gets the 3 stages: the split after the files are read and the checks pass; a member stage that reads the stored files and never drops them; the merge stage drops them after `MarkDispatched`, as `:261` does today)
- Test: `internal/engine/e2e_api_panel_test.go` (new), `internal/engine/e2e_panel_test.go`

The API path follows S3.4, S3.5 and S3.8 with the API's rules: the global chain B, no member fallback (a quota error fails the member), the files kept in `api_reviews.inputs_sealed` until Job P is created. `DropSpentAPIInputs` needs no change (Finding 10).

- [ ] **Step 1: Failing tests:**
  - `TestE2EAPIPanel`: a global chain B: the split; both member Jobs get the files Secret; `inputs_sealed` is still set after both member Jobs start and after `DropSpentAPIInputs`, and is NULL once Job P is created; the v1 result is P's; its status reads `running` while members run.
  - `TestE2EAPIPanelSupersededDropsFiles`: the client is removed while members run: the member Jobs are deleted and `DropSpentAPIInputs` drops the files.
  - `TestE2EAPIPanelNoMemberFallback`: B's quota fails B; Job P runs with A only.
  - `TestE2EAPIClientModelPanel`: a client with its own model: A runs on it, B on the global chain B.
  - `TestE2ERerunIsPanel`: a re-run command runs as a panel.
  - `TestE2ERerunFailedSkillsPanel`: the members get the subset; publish merges P's result with the base run as today.
  - `TestE2EIncrementalPanel`: a push with carried files: the members get `--carried`, P does not; the published result keeps the carried files.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run 'E2EAPIPanel|E2EAPIClientModelPanel|E2ERerun.*Panel|E2EIncrementalPanel'`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the engine package. PASS.
- [ ] **Step 5: Commit** `feat(engine): API reviews and re-runs as a panel`

### Task S3.11: Run page

**Files:**
- Modify: `internal/dashboard/view.go` (`RunDetailView.Panel *PanelView`), `internal/dashboard/api_read.go:70-72` (`GET /api/runs/{id}/members/{member}/log` and `/diagnostics`, with the gates of the run's own), `openapi.yaml`, `internal/dashboard/openapi_test.go` (`openAPISchemas`)
- Modify (after PR 74): `web/src/pages/ReviewDetail.tsx`, `web/src/api.ts`
- Test: `internal/dashboard/api_read_test.go`, `web/src/pages/ReviewDetail.test.tsx`

```go
// PanelView is a panel review on its run's page: why it did not start (Off),
// or its members, the entries Job P judged with, and the tokens per model.
type PanelView struct {
	Off     string            `json:"off,omitempty"`
	Members []PanelMemberView `json:"members"`
	Judges  []PanelJudgeView  `json:"judges"`
	Cost    []ModelCostView   `json:"cost"`
}
```

- [ ] **Step 1: Failing tests:** `TestRunPagePanel` (members with their attempts, entry names, states and failures; judges; cost per role and part; no key); `TestRunPagePanelOff`; `TestRunPageNoPanel` (no `panel` key); `TestMemberLogGated` (a user who can't see the run gets 404; the member of another run gets 404); `TestOpenAPIMatches`; web: `ReviewDetail.test.tsx` "shows the panel members and the cost per model".
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/dashboard/ -run 'RunPage|MemberLog|OpenAPI'`, then `cd web && npx vitest run src/pages/ReviewDetail.test.tsx`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the dashboard package and the web gate. PASS.
- [ ] **Step 5: Commit** `feat(dashboard): a panel review on its run page`

### Task S3.12: Docs and upgrade notes

**Files:** `docs/concepts/review-panel.md` (drop "not used yet"; the 3 Jobs, the members, the notes), `docs/concepts/how-it-works.md`, `docs/concepts/job-isolation.md` (member Jobs hold one key, Job P at most 2), `docs/concepts/usage.md` (cost per model), `docs/concepts/token-quota.md` (all 3 Jobs count; the instance quota gap), `docs/concepts/dashboard.md` (the run page panel, the repository's Off), `docs/README.md`, `docs/upgrading.md` `## Unreleased`.

`## Unreleased` says: migration `0048` adds 5 tables and drops nothing. Rollback: the previous release leaves a waiting panel run alone, and its member Jobs post to a route it does not have, so that review stays waiting until a push or a close supersedes it. Before a rollback, turn chain B off at every level and wait until no review shows a running panel. No new metric: members are not runs, so `pergent_reviews_total` counts a panel review once.

- [ ] **Step 1:** Write the pages; every exact string matches the code.
- [ ] **Step 2:** Phase gate (Global Constraints).
- [ ] **Step 3: Commit** `docs: review panel reviews`

## Self-review notes

- Spec coverage: `panel_chains`, `panel_chain_entries`, `dashboard_repos.panel_chain_mode` with `off` (S1.1, S1.2); chain B at 3 levels with chain A's rights and code, and `off` beating team and global (S1.2, S1.3, S1.5, S1.6); image check on save (S1.4, S1.5) and at dispatch (S3.4); `panel_members` with its own states and fallback rows (S3.1, S3.2); `runs` unchanged and `runs_live_idx` unchanged (S3.1, S3.3); parallel A and B with verify off (S2.4, S3.5); fallback per member, B skips A's model, "A only" with a note (S3.4, S3.5, S3.7); members post nothing, only P publishes (S3.7, S3.9); P after each member's final attempt, 2 keys, quota, 10 min timeout, P fallback per role (S3.8); both members fail = a failed review (S3.7); `run_usage_models` and the `run_usage` total, the quota counts all 3 (S2.3, S3.9); API reviews keep their files until Job P (S3.10) and the review API spec says so; re-runs (S3.10); dashboard run page and repository page (S1.5, S1.6, S3.11); docs and `## Unreleased` (S1.7, S3.12); chain B off = byte for byte (S2.4, S3.4).
- Not here: the pergent CLI (its own plan), any release step, `defaultImage`, the value of `PanelMinVersion`.
