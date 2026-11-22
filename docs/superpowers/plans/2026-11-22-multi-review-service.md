# Multi review, pergent.sh service half: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A repository with 2 to 5 reviewers runs each review as a multi review: one member Job per reviewer at the same time, each with skill verification off, then one judge Job (`pergent agent --multi-review`) that holds only the judge's key and publishes one result. A repository with 1 reviewer is reviewed exactly as today, byte for byte.

**Architecture:** The tested review panel (phases S1 to S3) is generalized, not rebuilt. Reviewer 1 is chain A (`model_chains`, inherit as today). Reviewers 2 to 5 and the judge are slots of one repository in new tables `reviewer_chains` and `reviewer_chain_entries`, read and written by the chain code through a third chain set. A multi review is one row in `runs` (the judge Job reports on it) and one `review_members` row per reviewer attempt, with the panel's state machine and numbers 1 to 5 in place of roles `a` and `b`. At the split each member row stores a frozen copy of its chain, and `review_judge_chains` stores the judge chain; fallback, the judge pick and the comment names read only the copies. While members run, the run waits with skip reason `review_members`, which v0.18.0 never claims. When every member is final, the run is queued again and its next dispatch starts the judge Job. The 0048 and 0049 tables stay, unused.

**Tech Stack:** Go 1.24, Postgres (pgx), `net/http` ServeMux, Kubernetes Jobs, React + Vite + TanStack Query, Vitest.

**Spec:** `docs/superpowers/specs/2026-11-22-multi-review-design.md` (binding; read it and its Anti-goals first). It replaces `2026-11-20-review-panel-design.md`. The CLI half is merged in pergent (gopaytech/pergent#54, `master` abedce0): its code, `docs/concepts/multi-review.md` and `docs/agent-result-schema.md` are the truth. This plan passes the CLI's flags and reads its result fields; what it needs beyond the design is in **Depends on the CLI**.

## Depends on the CLI

Details the design does not state. Reconciled with pergent `master` abedce0 on 2026-11-22. File and line references in this table are pergent's.

| # | Need | What this plan builds on | Used in |
|---|---|---|---|
| D1 | Flag forms | `--multi-review`, `--reviewer <file>` 1 to 5 times (pergent.sh writes the Go `flag` `=` form), `--mr-dismissals <file>` (required unless `--no-judge`; D11), `--no-judge`, `--no-skill-verify` on reviewer Jobs only. Refused with `--multi-review`: `--previous`, `--carried`, `--ci-results`, `--change-description`, `--linked-repos`, `--graph`, `--no-skill-verify` (`cmd/agent.go:128`); the judge ignores their env defaults. Accepted: `--dismissals` (env `PERGENT_DISMISSALS`), `--rules`, `--instructions`, `--check-deleted`. `--panel*` flags and `PERGENT_PANEL_B_*` are gone (`flag provided but not defined`). | M3.1 |
| D2 | Reviewer numbering | The n-th `--reviewer` is reviewer n, and an unusable file keeps its number. pergent.sh passes every reviewer of the frozen set, in card order, one `--reviewer` each, a reviewer with no result included, so the CLI's reviewer 3 is the card's reviewer 3 (`cmd/reviewer.go:215-225`). `MultiReviewNames.Reviewers` holds every reviewer's name in that order. A reviewer with no file is in every skill's `multi_review.failed`. | M3.1, M3.7, M3.9 |
| D3 | A `--reviewer` file that is missing (no result: get-reviewers wrote none) | That reviewer is unusable (log line `multi review: reviewer <n> can't be used: <reason>`), not an error. No usable file at all: an error result, `status` `error`, `error.code` `usage`, message `--multi-review needs at least one usable reviewer result`, exit 2, still delivered (`cmd/reviewer.go:24,226-228`; `cmd/agent.go:251-264`). M3.7 reads the result, not the exit code: one `--no-judge` retry (a failed fetch can work the second time), then the review fails. | M3.1, M3.7 |
| D4 | `--no-judge` env | `OPENCODE_PROVIDER`, `OPENCODE_MODEL` and `OPENCODE_API_KEY` are not required. pergent.sh sets no LLM and no key (the env values are empty). | M3.7 |
| D5 | Render names | `render.Options.MultiReview *render.MultiReviewNames{Reviewers []string; Judge string; Skipped []render.SkippedReviewer{Name, Reason}}`; `Judge` is `""` with no judge; a missing name reads `reviewer <n>`. Nil writes no multi review text, so the comment is today's. | M3.9 |
| D6 | Result JSON | Top `multi_review: {reviewers: [{number, usable}], judge: {provider, model, judged}}`: `usable` is decided per file (readable, schema 1, `completed`, same base and head), never per skill; `provider` and `model` are omitted when `judged` is false. Skill `multi_review: {reviewers, failed, judge, judge_cause, exact_merge}`: `reviewers` never null; `failed` omitted when empty; `judge` is `ok`, `failed`, `skipped` or `not_run`; `judge_cause` is `{kind, message, provider_error}`; `exact_merge` omitted when false. Finding `multi_review: {found_by, also, judge_disagreed, reason, rated_suggestion_by, unjudged}`: `found_by` never null, the rest omitted when empty or false (`internal/result/multireview.go:15-58`). pergent.sh mirrors them in `internal/result` (`MultiReview`, `SkillMultiReview`, `FindingMultiReview`). `ToRender` maps the skill to `render.SkillMultiReview{Reviewers, Failed, Exact: exact_merge}` and the finding to `render.FindingMultiReview` field for field, `also` included (render dismisses a merged finding through it); `judge` and `judge_cause` stay for the run page. | M3.9 |
| D7 | Provider error of the judge | `provider_error.role` is `judge` on a judge (or one-reviewer verification) error, with the judge's provider and model; a reviewer Job's has no role. | M3.7 |
| D8 | Judge tokens | One model per Job: `skills[].tokens` of a judge result are the judge's own calls. `tokens_by_model` is removed. | M3.9 |
| D9 | Lines pergent.sh writes | Only the footer line `multi review off: <reason>` of a review that ran as one review. `render` writes every multi review text: the header `Multi review: <names> · judge <judge>` from every name in `Reviewers`; the skip line `Multi review: <name> was skipped: <reason>.` once from `Skipped` (the reason's last `.` removed, cut to 300 bytes); and per skill `Multi review: <name> could not review this.` for each number in `failed`, a reviewer with no file included (`render/details.go:632-653`). pergent.sh fills `Skipped`. | M3.9 |
| D10 | The release | The first pergent release with `--multi-review` is the new `PanelMinVersion`. The owner sets it with `defaultImage` and the release pin (Rollout step 2), not this plan. Until then M3.9 pins `go.mod` to `master` abedce0. | M3.9, M4.3 |
| D11 | Merge request dismissals | The judge Job always gets `--mr-dismissals=/in/mr-dismissals.json`: JSON `[{"fingerprint","severity"}]` of `Store.Dismissals(run.MR)`, the list publish applies (pergent.sh `internal/engine/publish.go:153`), `[]` with none, read at the judge dispatch. Without it pergent exits 2 (`cmd/agent.go:518-520`). It is passed with `--no-judge` too: the exact merge also keeps a covered finding out of exact groups (`internal/review/multireview.go:336-338,494-511`). | M3.1, M3.7 |

## Findings (checked 2026-11-22 on `origin/main` a3577af)

| # | Question | Answer | Evidence |
|---|---|---|---|
| 1 | Migrations | `main` ends at `0049`. Take `0050`, or the next free number when you start. | `internal/store/migrations/` |
| 2 | Why new member tables | `panel_members.role` is `CHECK (role IN ('a','b'))` with a unique live index on `(run_id, role)`; v0.18.0's `ReleasePanel` reads only `a` and `b`. | `0049_panel_members.sql:15,46`; `internal/store/panel.go:678-750` |
| 3 | The chain code path | `ChainSet` builds every chain statement from fixed table names; `ChainA`, `ChainB`. `loadChains`, `writeChainEntries`, `removeFromChains`, `SaveChainOf`. | `internal/store/chains.go:42-50,65,105,139,283` |
| 4 | Removal effects | `removeFromChains` runs per set from `DeleteTeamModel` and `DeleteTeam`; a global entry removal touches no chain (skipped as `removed`). A repository removal deletes its chains. | `team_models.go:125-131`; `teams.go:344-348`; `dashboard.go:573-578` |
| 5 | Chain load | `LoadSet` is `Load` per set; chain B: repository `off` first, stored global only, `Tried` from the caller. | `internal/chain/load.go:32-91` |
| 6 | Same model | `Input.Avoid` is one `SameModel`; skip reason `same_model`. | `internal/chain/chain.go:25,45-55,131` |
| 7 | The split | `d.panel` runs after the run's writes and before the token: `LoadSet(ChainB)`, `PanelImage`, `ScanImage`, repo quota, `resolveB`, then `StartPanel`. | `internal/engine/panel.go:56-108`; `dispatch.go:791-801` |
| 8 | StartPanel | One transaction under `lockMR`: run to `waiting`/`panel_members` with `dispatched_at`, base and start SHAs and inputs; off row dropped; members `a`, `b` queued. | `internal/store/panel.go:176-216` |
| 9 | Stages | `stageReview`, `stageMember`, `stageMerge`; `dispatchRun` picks merge when a non-off member exists, puts a run with live members back (`waitForMembers`). | `dispatch.go:177-185,249-306` |
| 10 | Member dispatch | `dispatchMember`, `resolveMember`, `resolveA`, `modelOfA`, `triedBy`, `memberCannotRun`, `memberInputsMatch`. | `internal/engine/panel.go:146-312` |
| 11 | Settle and release | `settleMembers` (in `PublishOnce`), `settleMember`, `fallBackMember`; store `MembersToSettle`, `PanelsToRelease`, `SettleMember`, `EnqueueMemberAttempt`, `ReleasePanel`. | `engine/panel.go:379-509`; `store/panel.go:556-750` |
| 12 | The merge stage | `resolveMerge`, `pickJudge`, `roleLLM`, `judgedBefore`, `recordJudges`; judge fallback `fallBackJudge`. | `engine/panel.go:539-697`; `fallback.go:45-163` |
| 13 | Jobs and keys | `JobParams.SkipSkillVerify`, `PanelMemberID`, `PanelRole`, `Panel *PanelParams`; `Secrets.LLMKeyB` stored as `llm-api-key-b`; `forPanel`, `panelArgs`, `panelEnv`, `panelInputsContainer`; `BuildSecret`. | `internal/kube/job.go:130-151,290-383,584-604`; `internal/kube/panel.go:20-148` |
| 14 | Fetch of member results | `get-panel --a-url --b-url --dir`, always exits 0, a 404 writes no file; served by `PanelHandler` on `GET /runs/{id}/panel/{role}` with the run's live result token. | `cmd/pergent-sh/graph.go:38-249`; `internal/callback/panel.go:22-49`; `store/panel.go:888-925`; `server.go:132` |
| 15 | Member result route | `POST /runs/{id}/members/{member}/result`, `MemberHandler` against `MemberResults`. | `internal/callback/member.go:17-199`; `server.go:118` |
| 16 | Skip reason sites | `WaitPanel = "panel_members"` and the literal in `CountRunning`, `ClaimQueued`, `panelWaitingSQL`, `StartRun`, `WaitingChanges`, `outcomeSQL`, the review list, `apiStatusSQL`, the API live check, `DeleteAPIClient`, `memberUsageRunSQL`, `panelNoResult`, `StatusOf`, the start button. | `runs.go:58,667-671,709,903,985,1760`; `dashboard_read.go:122,172`; `apireviews.go:144,244`; `apiclients.go:239,260`; `usage.go:224-229,1141`; `reviewapi/body.go:301`; `api_commands.go:173` |
| 17 | Usage | `RecordMemberUsage` adds a member's tokens to `run_usage` of the split run and writes `run_usage_models` (part `member`); `recordMergeUsage` for Job P (part `merge`); `ReviewModelCosts`; `BackfillMembers` at start. | `internal/store/usage.go:94-311`; `internal/usage/backfill.go:61-90`; `cmd/pergent-sh/main.go:423-430` |
| 18 | Comment | `panelComment` names roles by judge or member entry names; `PanelOffLine` (`panel off: `) in the footer; `hideModel` hides every member's and judge's model. | `engine/publish.go:161,390-410,436-510`; `policy/policy.go:159-170,317,427-433` |
| 19 | Panel API (all go) | `GET\|PUT /api/admin/panel-chain`, `GET\|PUT /api/teams/{id}/panel-chain`, repository `.../panel-chain`, `/reset`, `/off`; `panelImageRefusal`; repository view `panel` (`PanelView`). | `api_operator.go:83-84`; `api_team_models.go:32-33,513-575`; `api_chains.go:40-73,381-460,620-804`; `api_write.go:541-580`; `api_read.go:1138`; `view.go:524-548` |
| 20 | Run page | `RunDetailView.Panel *RunPanelView`; `panelMembers`, `panelOf`; member log and diagnostics routes `GET /api/runs/{id}/members/{member}/log` and `/diagnostics`. | `view.go:249-307`; `api_read.go:73-75,823-925` |
| 21 | Web | `PanelChainCard.tsx` (team, admin), `RepoModelChain.tsx` (`RepoPanelChain` at `:182`), `ChainEditor.tsx` (`maxChain`, `entryStateText`), `ReviewDetail.tsx` (`Panel` at `:477`). | `web/src/...` |
| 22 | The floor | `PanelMinVersion = ""` on `main` since the hold PR (#91, `config.go:1412`), as in v0.18.0. `PanelImage` reads it. | `internal/config/config.go:1409-1445`; `git show v0.18.0:internal/config/config.go` |
| 23 | Goldens | `TestBuildJobUnchanged` (`internal/kube/job_test.go:925`, `testdata/review-job.golden.json`), `TestDispatchJobUnchanged` (`internal/engine/panel_test.go:173`, `testdata/dispatch-job.golden.json`), `TestCommentUnchanged` (`publish_test.go:2712`). | code read |
| 24 | Rolling-deploy test helpers | `startRunOld` runs v0.18.0's `StartRun` statement; `TestE2EPanelMergeWaitsForLiveMembers`. | `internal/engine/e2e_panel_rolling_test.go:14-85` |
| 25 | E2E helpers to generalize | `savePanelChain`, `newPanelE2E`, `splitPanel`, `memberOfRole`, `wantMemberKey`, `postMember` (an `e2e` method), `wantReleased`, `wantNothingPosted`. `watch_test.go:511-531` (`TestRecoverUnlaunchedMembers`) uses 3 of them. | `e2e_panel_test.go:33-943` |
| 26 | API review panel code | `apireview.go` reads `PanelMembers` (`:64`), calls `resolveMerge` (`:147`), `d.merge.key` (`:172,353`), `d.panel` (`:285`), `PanelParams`/`LLMKeyB` (`:352-353`), `d.member.Role` (`:360`). | `internal/engine/apireview.go` |
| 27 | Other panel readers | `inline.go:134` (`o.Panel`); `launcher.go:36-40,240` (`HasKeyB`, `llm-api-key-b`); `api_read.go:827-990` (run page and member logs on the panel store calls). | `internal/engine`, `internal/kube`, `internal/dashboard` |
| 28 | OpenAPI tests | `TestOpenAPIRoutesMatchCode`, `TestOpenAPISchemasMatchGoTypes`, `TestOpenAPIEveryViewTypeHasASchema`, `TestOpenAPIEnumsMatchCode`; `-run OpenAPI` runs them all. | `internal/dashboard/openapi_test.go:542-1402` |
| 29 | Dismissals the comment applies | `publish.go:153` → `e.dismissals` (`:512-518`) → `actions.RenderDismissals` → `Store.Dismissals`; `severity` is `blocking` or `suggestion` by CHECK. | `internal/engine/publish.go`; `internal/store/dismissals.go:72`; `0012_commands.sql:23` |

## Global Constraints

- **Start condition.** Branch per phase off `origin/main`, after `git fetch`. The hold PR must be merged first: `git grep -n 'PanelMinVersion = ""' origin/main -- internal/config/config.go` prints one line. If it prints nothing, stop and report; never change the floor on these branches.
- **No release.** No tag, no version bump, no chart version, no `defaultImage` move. Notes go under `## Unreleased` in `docs/upgrading.md`. Tests use fake Job results; never suggest a release to test.
- **The floor.** `config.PanelMinVersion` stays `""` (every image refused, so no review splits on `main`). It becomes the first pergent release with `--multi-review` only when that release exists, set by the owner with `defaultImage` and the `go.mod` pin. Tests set it and restore `""` with `t.Cleanup`.
- The `go.mod` pin moves to pergent `master` abedce0 (`go get github.com/gopaytech/pergent@abedce0b723a7a006a7f5c094a0b15a433d22b74`, no `replace`) in M3.9 only: that commit drops `render.PanelNames`, `SkillPanel` and `FindingPanel`. The release pin stays the owner's (Rollout step 2).
- **Migrations only add.** The previous release (v0.18.0) must run on the migrated database. The 0048 and 0049 tables stay and nothing writes them, except M3.3's one-time cleanup of live legacy members.
- **1 reviewer = today.** With no extra reviewer, the Job, its ConfigMap and Secret, the dispatch writes and the comment are byte for byte today's: the 3 goldens of Finding 23 pass with no edit to a golden file.
- **Keys.** A reviewer Job holds only its own entry's key. The judge Job holds only the judge entry's key, and none with `--no-judge`. No key in a frozen chain, an API response, a log, an audit entry, a metric label, the dashboard or the browser.
- **Never a false pass.** Every reviewer failed: the review fails. The judge Job crashed twice: the review fails. Never `success` and never an approval from those.
- **Members never publish.** No comment, note, thread, approval or status from a member, except the first member's running status (today's rule).
- **Kept from S1-S3:** the judge stage starts only when every member is final; base, start and inputs are recorded at the split; usage counts on the split run; the judge Job writes nothing to the run before its result.
- **Rights.** Reviewers 2 to 5 and the judge have chain A's repository rights exactly: admins and maintainers of the owner team write, viewers read, nobody sees a key.
- **Database tests** run only when `nc -z localhost 55432` succeeds, with the variable in the same command: `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 -timeout 30m <pkgs>`. If the port is closed, say so and leave them to CI. Never start a container. Never run vitest while `go test` runs.
- **Web gate** when `web/` changed: `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build`.
- **Public repository.** Before each commit, run the private-name scan from the pergent-developer memory (`git grep`, plans and `web/package-lock.json` excluded) and the email and local-path grep over the files the branch changed and every new untracked file. Both print nothing.
- **Commits.** Plain `git commit` with the configured identity; never pass or write an email. Conventional subject; last line `Co-Authored-By: Claude <model that wrote it> <noreply@anthropic.com>`. Tests and commits are separate commands.
- **Easy English** in code comments, docs, errors and UI text: short sentences, active voice, no idioms.
- **Phase gate:** `go vet ./...`; the DB suite with `./...`; the web gate when `web/` changed; the name scan. Report failures by test name.
- **Every task builds.** A task that deletes or retypes a symbol edits every caller in the same task, so its own Step 4 can pass.

## Choices this plan makes

| Item | Choice | Why |
|---|---|---|
| Reviewer chains in the chain code | `store.ChainReviewers = ChainSet{chains: "reviewer_chains", entries: "reviewer_chain_entries", slot: "c.slot"}`; `ChainSet` gains `slot` (an SQL expression, `''` for chain A) and `Chain` gains `Slot`. `reviewer_chains` has fixed `scope = 'repo'` and `team_id IS NULL` so `loadChains` reads it unchanged. | One chain code path (design "Keep its tested parts"). |
| Slots | `r2`..`r5`, `judge`. A save writes every slot in one transaction. Removing reviewer k moves k+1.. down one slot in the same transaction. No extra slot left: the judge row goes too. | Design "Remove". |
| `saved_by_admin` per slot | A slot whose entries do not change keeps its flag and is not checked against the saver's choices; a changed slot takes the saver's flag and is checked. After a remove, reviewer k comes from stored reviewer k+1: with the same entries it is unchanged, keeps that flag and is not checked. | A maintainer can edit reviewer 3 while an admin's reviewer 2 holds an entry teams may not use. |
| Frozen chain | `store.FrozenChain{Scope, SavedByAdmin, Entries []FrozenEntry{Source, ID, Name, Provider, Model}}` as jsonb on each member attempt (copied to later attempts) and in `review_judge_chains` (by the review's first run). Keys and base URLs come from the registry at each dispatch; an entry gone from the registry is `removed`. | Design "Freeze at the split". |
| Wait reason | `store.WaitReviewers = "review_members"`. Every skip-reason literal site of Finding 16 moves to it (M3.3); the `panel_members` table queries stay until M4.2. | v0.18.0 never claims it. |
| Member states | The panel's, plus `skipped` (final; an extra reviewer that could not start at the split, reason in `failure_reason`). `off` stays: reviewer 1's one row of a review whose multi review did not start. A `failed` member keeps its result when a provider refused its entry and its frozen chain has no entry left (M3.6). | Design "Reviewer skipped", "Only reviewer 1 can start". |
| Off reasons (after `multi review off: `) | `the review image does not support it` (design text), `pergent.sh's own image is not set`, `the token quota is used up`, `no other reviewer can start`, `the reviewers could not be read`. | Builder's texts. |
| Skip reasons at the split | `no different model left` (only `same_model`), `the token quota` (only the instance quota, or the repository quota after the split), `no usable entry` (anything else). One helper, `skipWords(dec)`, maps a `chain.Decision` at the split and at member dispatch. | Comment-safe fixed words. |
| Skipped reason (`render`'s `Skipped`, D5) | Fixed words only: a skip reason, `chain.ReasonWords(class)` for a provider refusal, else `its review failed`. The full text is on the run page. | No model text in the comment. |
| Every reviewer failed | Run reported with `every reviewer failed: reviewer 1 (<name>): <reason>; reviewer 2 (<name>): <reason>` (each cut with `cutText`). | Design "Every reviewer failed". |
| Judge stage name | `stageJudge` (was `stageMerge`). | Words of the design. |
| Judge row | `review_judges` (PK `run_id`): every reviewer of the frozen set (`int[]`, `--reviewer` order, D2), `no_judge`, `no_judge_reason`, the entry. Written before the Job starts. | Names, fallback and usage read it. |
| Judge crash | Publish of a judge run with no usable result and `no_judge = false` calls `Store.EnqueueJudgeRetry`: the run is superseded with outcome `fell_back`, reason `judge_failed`, and the next attempt is queued. A judge stage whose review has a `judge_failed` attempt runs `--no-judge`. A failed `--no-judge` run is published as failed. | Design: "One more judge Job with `--no-judge`. If it fails too, the review fails." |
| Results for the judge | `GET /runs/{id}/reviewers/{n}`, the judge run's live result token: the result of the newest attempt of reviewer `n` that has a stored result and is `done`, or `failed` with its result kept (never `fell_back`); 404 otherwise. The CLI carries a failed skill's blocking findings. Fetched by `get-reviewers --base-url=<...>/runs/<id>/reviewers --reviewers=1,2,3 --dir=/panel` into `/panel/reviewer-<n>.json`; a reviewer with no result gets no file (D3). | The panel's route and fetch, for N. |
| Merge request dismissals | The judge Job gets `--mr-dismissals` (D11) from the ConfigMap key `mr-dismissals.json`. A dismissal made while the judge Job runs is applied by the comment but was not known to its links: accepted, documented (M4.3). | Spec "Groups": a link never touches a dismissed finding. |
| Labels | `pergent.sh/review-member` (member ID) and `pergent.sh/reviewer` (`1`..`5`, or `judge`). | The panel labels never shipped in use. |
| Usage | `run_usage_by_model`: part `reviewer` (one row per member attempt and skill, on the split run) and part `judge` (one row per judge run and skill, the `review_judges` entry; none with `--no-judge`). `run_usage` keeps the total. | Design "Data". |
| Legacy panel rows | At start: log one warning with the count of `panel_chains` rows with entries and of `panel_members` rows; queue each run waiting on `panel_members` again as a single review; supersede its live `panel_members` and delete their Jobs. | Design "Old tables and rows". |
| Chain B code | Removed (store `ChainB`, `RepoPanelMode`, `SetRepoPanelOff`, `LoadSet` chain B branch, the routes, the cards). The tables and `dashboard_repos.panel_chain_mode` stay. | Design "Where". |
| Image check message | Save refused 400: `a second reviewer needs a review image with pergent <min> or later; jobs.image is <tag>`; floor `""`: `no review image supports multi review yet`. Only when the save adds a reviewer. | Design "Image check". |

## Review Focus

1. **An old pod during a rolling deploy.** v0.18.0's `StartRun` and `WaitingChanges` treat a `review_members` run as an ordinary waiting run, so a start can queue it and an old pod can run it as one review. Expected: one single review, its members superseded by the new pod's sweep, never a merge of partial results. Test `TestE2EMultiReviewOldStartIsSingleReview` (M3.7).
2. **A card edit while a review runs.** Reviewer 2's chain and the judge chain are edited after the split. Expected: fallback, the judge pick and the comment names use the copies. Test `TestE2EMultiReviewFrozenAtSplit` (M3.7).
3. **A team entry deleted while a review runs.** Expected: the live chains lose it and renumber; the running review still has it in its copy and skips it as `removed` at its next fallback. Test `TestE2EMultiReviewTeamEntryDeletedMidReview` (M3.6).
4. **The judge Job crashes or hits its deadline.** Expected: one more judge Job with `--no-judge` and no key; if that fails too, the review fails, never `success`. Tests `TestE2EJudgeCrashRunsNoJudgeOnce`, `TestE2EJudgeNoJudgeFailsReview` (M3.7).
5. **A key in the wrong Job.** Expected: the judge Job's Secret holds the judge's key only, even when it is reviewer 2's model; reviewer Jobs hold their own. Test `TestE2EJudgeJobHoldsOnlyJudgeKey` (M3.7).

---

## Phase M1: data, reviewer chains, frozen chains

Nothing in this phase changes a review: nothing reads the new tables yet.

### Task M1.1: Migration `0050_multi_review.sql`

**Files:**
- Create: `internal/store/migrations/0050_multi_review.sql`
- Test: `internal/store/multi_review_migration_test.go` (new)

```sql
-- Multi review: reviewers 2 to 5 and a judge per repository, the members of a
-- multi review, its frozen judge chain, the judge of each judge Job run, and
-- tokens per model. It only adds. The previous release (up to 0049) still
-- runs on the migrated database and ignores all of it: its panel code reads
-- only the 0048 and 0049 tables, which stay, unused. A multi review's run
-- waits with skip_reason 'review_members', which the previous release never
-- claims.
--
-- reviewer_chains: one row per repository and slot. scope and team_id are
-- fixed so the chain code reads it as it reads model_chains. An extra
-- reviewer's chain always has an entry; a judge chain may have none (its
-- entries were removed), and reviews then run with --no-judge.
CREATE TABLE reviewer_chains (
    id             bigserial   PRIMARY KEY,
    scope          text        NOT NULL DEFAULT 'repo' CHECK (scope = 'repo'),
    team_id        bigint      CHECK (team_id IS NULL),
    instance       text        NOT NULL CHECK (instance <> ''),
    project        text        NOT NULL CHECK (project <> ''),
    slot           text        NOT NULL CHECK (slot IN ('r2', 'r3', 'r4', 'r5', 'judge')),
    saved_by_admin boolean     NOT NULL DEFAULT false,
    updated_at     timestamptz NOT NULL
);
CREATE UNIQUE INDEX reviewer_chains_key ON reviewer_chains (instance, lower(project), slot);

CREATE TABLE reviewer_chain_entries (
    chain_id bigint NOT NULL REFERENCES reviewer_chains (id) ON DELETE CASCADE,
    position integer NOT NULL CHECK (position BETWEEN 1 AND 5),
    source   text   NOT NULL CHECK (source IN ('global', 'team')),
    model_id text   NOT NULL,
    PRIMARY KEY (chain_id, position)
);
CREATE INDEX reviewer_chain_entries_model_idx ON reviewer_chain_entries (source, model_id);

-- review_members: one row per attempt of reviewer 1 to 5. run_id is the
-- review's first run. chain is the reviewer's chain as it was at the split
-- (scope, saved_by_admin, entries with source, id, name, provider, model;
-- never a key or a base URL). 'skipped': an extra reviewer that could not
-- start at the split. 'off': the one row of a review whose multi review did
-- not start (reviewer 1, no chain).
CREATE TABLE review_members (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id           uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    reviewer         integer     NOT NULL CHECK (reviewer BETWEEN 1 AND 5),
    attempt_of       uuid        REFERENCES review_members (id) ON DELETE CASCADE,
    state            text        NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running',
        'reported', 'done', 'failed', 'fell_back', 'superseded', 'skipped', 'off')),
    chain            jsonb,
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
    not_before       timestamptz,
    CHECK ((state = 'off') = (chain IS NULL))
);
CREATE UNIQUE INDEX review_members_live_idx ON review_members (run_id, reviewer)
    WHERE state IN ('queued', 'running', 'reported');
CREATE UNIQUE INDEX review_members_off_idx ON review_members (run_id) WHERE state = 'off';
CREATE INDEX review_members_run_idx ON review_members (run_id, created_at);
CREATE INDEX review_members_state_idx ON review_members (state, created_at);

CREATE TABLE review_member_logs (
    member_id  uuid        PRIMARY KEY REFERENCES review_members (id) ON DELETE CASCADE,
    text       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX review_member_logs_created_idx ON review_member_logs (created_at);

CREATE TABLE review_member_diagnostics (
    member_id        uuid        PRIMARY KEY REFERENCES review_members (id) ON DELETE CASCADE,
    clone_log        text        NOT NULL,
    clone_log_masked text        NOT NULL,
    pod_state        text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX review_member_diagnostics_created_idx ON review_member_diagnostics (created_at);

-- review_judge_chains: the judge chain of a multi review as it was at the
-- split, by the review's first run. No entries: no judge.
CREATE TABLE review_judge_chains (
    run_id uuid  PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
    chain  jsonb NOT NULL
);

-- review_judges: what each judge Job run judged with: every reviewer of the
-- review, in --reviewer order, and its entry, or no_judge and why.
CREATE TABLE review_judges (
    run_id           uuid      PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
    reviewers        integer[] NOT NULL,
    no_judge         boolean   NOT NULL,
    no_judge_reason  text      NOT NULL DEFAULT '',
    provider         text      NOT NULL DEFAULT '',
    model            text      NOT NULL DEFAULT '',
    model_source     text      NOT NULL DEFAULT '',
    model_entry_id   text      NOT NULL DEFAULT '',
    model_entry_name text      NOT NULL DEFAULT '',
    model_team_id    bigint,
    chain_position   integer   NOT NULL DEFAULT 0,
    chain_length     integer   NOT NULL DEFAULT 0
);

-- run_usage_by_model: tokens per model of a multi review. part 'reviewer' is
-- one row per member attempt and skill, on the run that was split; part
-- 'judge' one per judge run and skill. run_usage keeps the total.
CREATE TABLE run_usage_by_model (
    id          bigserial   PRIMARY KEY,
    run_id      uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    member_id   uuid        REFERENCES review_members (id) ON DELETE CASCADE,
    part        text        NOT NULL CHECK (part IN ('reviewer', 'judge')),
    reviewer    integer     CHECK (reviewer BETWEEN 1 AND 5),
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
    CHECK ((part = 'reviewer') = (member_id IS NOT NULL)),
    CHECK ((part = 'reviewer') = (reviewer IS NOT NULL))
);
CREATE UNIQUE INDEX run_usage_by_model_reviewer_key ON run_usage_by_model (member_id, skill) WHERE part = 'reviewer';
CREATE UNIQUE INDEX run_usage_by_model_judge_key ON run_usage_by_model (run_id, skill) WHERE part = 'judge';
CREATE INDEX run_usage_by_model_reported_idx ON run_usage_by_model (reported_at);
```

- [ ] **Step 1: Failing tests** (model them on the 0049 tests with `applyUpTo`): `TestMultiReviewMigrationOnlyAdds` (every 0049 column keeps its type; `panel_members`, `panel_chains` and `runs_live_idx` unchanged); `TestMultiReviewMigrationPreviousRelease` (after 0050, v0.18.0's inserts into `runs`, `panel_members`, `run_usage` and `model_chains` succeed; deleting a run with members, judge rows and usage rows cascades); `TestReviewerChainOneRowPerSlot` (a second `r2` of one repository, project case aside, fails 23505; `r6` and `scope = 'team'` fail 23514); `TestReviewMembersOneLivePerReviewer` (a second queued row for (run, 2) fails 23505; `fell_back` and queued of one reviewer are fine; reviewer 6 fails; an off row with a chain fails; a queued row without one fails); `TestRunUsageByModelKeys` (a reviewer row needs `member_id` and `reviewer`; a judge row has neither; the 2 unique keys).
- [ ] **Step 2: Run to fail** (`nc -z localhost 55432` first): `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'MultiReviewMigration|ReviewerChainOneRow|ReviewMembersOneLive|RunUsageByModelKeys'`. Expected: FAIL, relation `reviewer_chains` does not exist.
- [ ] **Step 3: Write the migration** above.
- [ ] **Step 4: Run them, then the whole store package.** PASS.
- [ ] **Step 5: Commit** `feat(store): tables for multi review`

### Task M1.2: Store: reviewer chains and what removals do to them

**Files:**
- Modify: `internal/store/chains.go` (`ChainSet.slot`, `Chain.Slot`, `loadChains`, `removeFromChains`), `internal/store/team_models.go:125-131`, `internal/store/teams.go:344`, `internal/store/dashboard.go:573`, `internal/dashboard/api_team_models.go:407-427` (the audit detail and log arg of a team model removal)
- Create: `internal/store/reviewers.go`
- Test: `internal/store/reviewers_test.go` (new), `internal/store/chains_test.go`, `internal/dashboard/api_team_models_test.go`

**Interfaces:**

```go
// ChainReviewers is a repository's reviewers 2 to 5 and its judge
// (reviewer_chains). Chain.Slot is SlotOf(n) or SlotJudge.
var ChainReviewers = ChainSet{chains: "reviewer_chains", entries: "reviewer_chain_entries", slot: "c.slot"}

const (
	MaxReviewers = 5
	SlotJudge    = "judge"
)

// SlotOf is reviewer n's slot, "r2" to "r5".
func SlotOf(n int) string

// RepoReviewers is what a repository stores beyond reviewer 1: Extra[0] is
// reviewer 2. Judge is the judge chain; JudgeFound is false with no row.
type RepoReviewers struct {
	Extra      []Chain
	Judge      Chain
	JudgeFound bool
}

func (s *Store) Reviewers(ctx context.Context, instance, project string) (RepoReviewers, error)

// ReviewersSave is a save of the Reviewers card: Extra are reviewers 2.. in
// order, each 1 to MaxChain entries; Judge is required with Extra and empty
// without.
type ReviewersSave struct {
	Instance, Project string
	Extra             [][]EntryRef
	Judge             []EntryRef
	SavedByAdmin      bool
}

// SaveReviewers replaces the repository's slots in one transaction and
// returns them as they were, for the audit. A slot whose entries do not
// change keeps its saved_by_admin and updated_at. After a remove, reviewer
// k is compared with the stored reviewer it comes from (k+1): a moved chain
// with the same entries keeps that reviewer's saved_by_admin.
func (s *Store) SaveReviewers(ctx context.Context, r ReviewersSave, at time.Time) (before RepoReviewers, err error)

var (
	ErrTooManyReviewers      = errors.New("store: at most 5 reviewers")
	ErrEmptyReviewer         = errors.New("store: a reviewer needs an entry")
	ErrNoJudge               = errors.New("store: 2 or more reviewers need a judge")
	ErrJudgeWithoutReviewers = errors.New("store: a judge needs 2 or more reviewers")
)
```

`loadChains` selects `` `+set.slot+` `` into `Chain.Slot`; `ChainA` and `ChainB` use `''`. `SaveReviewers` locks as a chain B repository save does (`lockRepoRow`, then the team entries `FOR KEY SHARE`, then the chains), checks the team entries exist (`ErrModelNotFound`), deletes slots past `len(Extra)`, upserts `r2`.. and the judge (no extra: the judge row goes). `removeFromChains(ChainReviewers, ...)` locks like the chain B branch it replaces, then per touched repository calls:

```go
// compactReviewers deletes the repository's empty r-slots, moves the others
// down to r2, r3, ... in order, and deletes the judge row when no r-slot is
// left. An empty judge with r-slots left stays (the card warns; reviews run
// with --no-judge).
func compactReviewers(ctx context.Context, tx pgx.Tx, instance, project string, at time.Time) error
```

The callers run `removeFromChains` for `ChainA` and `ChainReviewers` (no more `ChainB`); `dashboard.go:573` deletes `reviewer_chains` rows of the removed repository (no more `panel_chains`). `DeleteTeamModel` returns the changed reviewer chains in place of `chainsB`, so the dashboard's team model removal writes the audit detail `, removed from reviewer chains: <labels>` (was `, removed from chain B: <labels>`) and the log arg `reviewer_chains` (was `panel_chains`). `compactReviewers` deletes the empty slots first, then moves each slot down in ascending order, one row per statement: the unique index is checked per row.

- [ ] **Step 1: Failing tests:**
  - `TestReviewersRoundTrip`: save 2 extra reviewers and a judge; `Reviewers` gives them in order with `Slot` `r2`, `r3`, `judge`; `model_chains`, `chain_mode` and `model_id` unchanged.
  - `TestSaveReviewersRefusals`: 5 extra (`ErrTooManyReviewers`), an empty extra (`ErrEmptyReviewer`), 6 entries (`ErrChainTooLong`), extras with no judge (`ErrNoJudge`), a judge with no extra (`ErrJudgeWithoutReviewers`), a deleted team entry (`ErrModelNotFound`); nothing stored after each.
  - `TestSaveReviewersKeepsUnchangedSlotFlag`: an admin's `r2`, then a maintainer's save that changes only `r3`: `r2.SavedByAdmin` stays true.
  - `TestSaveReviewersRemoveLastDropsJudge`: save with no extra: no row left.
  - `TestSaveReviewersRemoveKeepsMovedFlag`: an admin's `r3`, then a maintainer's save that removes reviewer 2: the new `r2` holds the old `r3` entries with `SavedByAdmin` true.
  - `TestDeleteTeamModelCompactsReviewers`: a team entry is the only entry of `r3` of 4 reviewers and the second entry of `r2`: `r3` goes, `r4` becomes `r3`, `r2` loses it; an emptied judge with r-slots left stays with no entries; chain A loses it as today.
  - `TestDeleteTeamModelLastExtraDropsJudge`: the entry is the only entry of the only extra reviewer: no reviewer row and no judge row left.
  - `TestDeleteTeamCompactsReviewers`: `DeleteTeam` with entries in 2 repositories' slots.
  - `TestRemoveGlobalModelKeepsReviewerEntry`: the entry stays.
  - `TestRemoveRepositoryDropsReviewers`.
  - `TestDeleteTeamModelAuditNamesReviewerChains` (dashboard): the audit detail says `removed from reviewer chains:` and never `chain B`.
  - Guard: the existing chain tests pass with no edit.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'Reviewers|Compacts|KeepsReviewerEntry|DropsJudge|MovedFlag|Chain'`. Expected: FAIL, undefined `ChainReviewers`.
- [ ] **Step 3: Code.** Table and column names only from the fixed `ChainSet` values.
- [ ] **Step 4: Run** the whole store package, then `./internal/dashboard/ -run 'TeamModel'` with the DB variable. PASS.
- [ ] **Step 5: Commit** `feat(store): a repository's reviewers 2 to 5 and its judge`

### Task M1.3: Chain: load reviewers, freeze, avoid several models

**Files:**
- Modify: `internal/chain/chain.go` (`Input.Avoid []SameModel`, `skipReason`, `ReasonSameModel` words), `internal/chain/load.go`
- Create: `internal/chain/reviewers.go`
- Modify: the 3 `Avoid` callers in `internal/engine/panel.go` and `chain.go` (`in.Avoid = []chain.SameModel{...}`)
- Test: `internal/chain/reviewers_test.go` (new), `internal/chain/chain_test.go`

**Interfaces:**

```go
// in internal/store (store has no chain import):
type FrozenEntry struct {
	Source   string `json:"source"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type FrozenChain struct {
	Scope        string        `json:"scope"`
	SavedByAdmin bool          `json:"saved_by_admin"`
	Entries      []FrozenEntry `json:"entries"`
}

// in internal/chain:
// Reviewers is what a split reads: reviewer 1 (chain A, as Load), the extra
// reviewers in order, and the judge chain (Judge.Refs nil: none).
type Reviewers struct {
	One   Input
	Extra []Input
	Judge Input
}

// LoadReviewers reads chain A as Load does, and the repository's stored
// reviewers 2 to 5 and judge (store.Reviewers) with the same entries, marks
// and owner team. An API review has none: Extra and Judge are empty.
func LoadReviewers(ctx context.Context, st *store.Store, reg *instances.Registry, now time.Time, run store.Run, repo settings.Effective) (Reviewers, error)

// Freeze is the copy of in's chain a split stores: each ref with the name,
// provider and model it has now ("" for a removed one). Never a key or a
// base URL.
func Freeze(in Input) store.FrozenChain

// FromFrozen is base (entries, marks, owner team, now) with fc's chain:
// its refs, scope and saved_by_admin. A ref no longer in base.Entries is
// skipped as removed. Tried, Avoid and InstanceQuotaUsedUp start empty: the
// caller sets them.
func FromFrozen(fc store.FrozenChain, base Input) Input
```

`skipReason` returns `ReasonSameModel` when any `Avoid` entry matches; its words become `the model another reviewer runs on`.

- [ ] **Step 1: Failing tests:** `TestResolveAvoidSeveral` (2 avoided models, any case: both skipped as `same_model`; a chain of only those is `Exhausted`); `TestLoadReviewersNoneIsOne` (no stored slot: `Extra` and `Judge.Refs` empty, `One` equals `Load`'s input); `TestLoadReviewersOrder` (`r2`, `r3` in order, the judge, each with its `SavedByAdmin`); `TestLoadReviewersAPIHasNone`; `TestFreezeHoldsNoKey` (the JSON of `Freeze` of a chain whose entries have keys and base URLs holds neither); `TestFromFrozenRemovedEntry` (a ref gone from the registry resolves as `removed`; the next ref runs); `TestFromFrozenIgnoresLiveChain` (a live chain edited after the freeze changes nothing); `TestFromFrozenDropsTried` (a base with `Tried` and `Avoid` set gives neither).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/chain/`. Expected: FAIL, undefined `LoadReviewers`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** `./internal/chain/` and `./internal/engine/` (the panel still builds and passes). PASS.
- [ ] **Step 5: Commit** `feat(chain): load, freeze and resolve reviewer chains`

### Task M1.4: Store: the review member state machine

**Files:**
- Create: `internal/store/review_members.go` (generalized from `internal/store/panel.go`, which stays until M4.2)
- Test: `internal/store/review_members_test.go` (new)

The functions are `panel.go`'s, renamed: `PanelMember` → `ReviewMember` (`Role string` → `Reviewer int`, plus `Chain FrozenChain`); `Panel` → `MultiReview` in each name (`StartMultiReview`, `MultiReviewRun`, `ReleaseMultiReview`, `SupersedeMultiReview`, `InheritMultiReviewSplit`, `MultiReviewOffReason`, ...); `Member` → `ReviewMember` (`ClaimQueuedReviewMembers`, `StoreReviewMemberResult`, ...); `IsPanelMerge` → `IsJudgeRun`; `WaitForPanel` → `WaitForReviewMembers`; `PanelMemberResult(At)` → `ReviewerResult(At)` (`reviewer int`). Each reads `review_members` and `WaitReviewers`. What changes beyond the rename:

```go
// WaitReviewers: a multi review's run waits while its members review.
// v0.18.0 never claims it.
const WaitReviewers = "review_members"

const MemberSkipped MemberState = "skipped"

// SplitReviewer is one reviewer at the split: its frozen chain, and why it
// can't start ("" queues it; reviewer 1 is always queued).
type SplitReviewer struct {
	Number  int
	Chain   FrozenChain
	Skipped string
}

// MultiReviewSplit is what StartMultiReview records: today's PanelSplit
// fields, every reviewer (1 first), and the frozen judge chain.
type MultiReviewSplit struct {
	BaseSHA, StartSHA string
	Inputs            RunInputs
	Reviewers         []SplitReviewer
	Judge             FrozenChain
}

func (s *Store) StartMultiReview(ctx context.Context, runID string, split MultiReviewSplit, now time.Time) (started bool, err error)

// JudgeChainOf is the frozen judge chain of run id's review.
func (s *Store) JudgeChainOf(ctx context.Context, id string) (FrozenChain, bool, error)

// JudgeRow is what a judge Job run judged with (review_judges).
type JudgeRow struct {
	Reviewers       []int
	NoJudge         bool
	NoJudgeReason   string
	Provider, Model string
	Entry           RunEntry
}
func (s *Store) SetJudge(ctx context.Context, runID string, j JudgeRow) error // run queued or running; replaces
func (s *Store) JudgeOf(ctx context.Context, runID string) (JudgeRow, bool, error)

// EnqueueJudgeRetry closes the reported judge run as fell_back with reason
// OutcomeJudgeFailed and queues the review's next attempt, as EnqueueAttempt
// does, with no mark. created is false when the run is not reported.
func (s *Store) EnqueueJudgeRetry(ctx context.Context, from Run) (id string, created bool, err error)

const OutcomeJudgeFailed = "judge_failed"

// FailReviewMemberKeepingResult moves a reported member to failed with
// reason and keeps its stored result: a provider refused its entry and its
// frozen chain has no entry left (M3.6). The judge still gets the result.
// false when the member is not reported or its run no longer waits.
func (s *Store) FailReviewMemberKeepingResult(ctx context.Context, id, reason string) (bool, error)
```

`StartMultiReview`, in one transaction under `lockMR`:

```sql
UPDATE runs SET state = 'waiting', skip_reason = 'review_members', dispatched_at = $2,
    base_sha = $3, start_sha = $4
WHERE id = $1::uuid AND state = 'running' AND job_name = '' AND dispatched_at IS NULL;
-- only when 1 row changed: setRunInputs(split.Inputs), then
DELETE FROM review_members WHERE run_id = $root AND state = 'off';
INSERT INTO review_members (run_id, reviewer, state, chain, failure_reason)
SELECT $root, u.n, CASE WHEN u.skip = '' THEN 'queued' ELSE 'skipped' END, u.chain, u.skip
FROM unnest($5::int[], $6::jsonb[], $7::text[]) AS u(n, chain, skip);
INSERT INTO review_judge_chains (run_id, chain) VALUES ($root, $8::jsonb)
ON CONFLICT (run_id) DO UPDATE SET chain = EXCLUDED.chain;
```

`EnqueueReviewMemberAttempt` copies `chain` to the new row. `ReleaseMultiReview` acts when the newest row of every reviewer number is final (`done`, `failed`, `skipped`) and none is live: with one `done` or more the run is queued again (`skip_reason ''`, `not_before NULL`); with none it is reported with `every reviewer failed: reviewer <n> (<name>): <reason>; ...` in number order, where name is the frozen entry name of the reviewer's last attempt (its chain's first entry when it never started). `CountRunning` adds `review_members` running rows (and keeps the `panel_members` count until M4.2). `PruneRunLogs` prunes `review_member_logs` and diagnostics. `ReviewerResult` serves the result of the newest attempt of reviewer `n` whose state is `done`, or `failed` with a stored result (never `fell_back`), for the run's live, unexpired token, run `running`, `n` in 1..5. `ReleaseMultiReview` still counts only `done` members to queue the judge.

- [ ] **Step 1: Failing tests** (port `panel_test.go`'s, one per behaviour, to numbers): `TestStartMultiReviewWaitsAndQueues` (3 reviewers, reviewer 3 skipped: run `waiting`/`review_members`, `dispatched_at` set, rows 1 and 2 queued, 3 `skipped` with its reason, the judge copy stored); `TestStartMultiReviewOnce` (a second call, or a run with `dispatched_at`, is false and writes nothing); `TestReviewMemberChainCopiedOnFallback`; `TestReleaseMultiReviewOneDone` (2 failed, 1 done: queued); `TestReleaseMultiReviewAllFailed` (reported with each reviewer's reason and name, skipped included); `TestReleaseMultiReviewWaitsForLive`; `TestReleaseMultiReviewSkippedOnly` (reviewer 1 failed, 2 skipped: reported); `TestReviewerResult` (reviewer 2's done row for the run's token; reviewer 3's `failed` row with its result kept; 404 cases: another run, reviewer 6, a failed reviewer with no result, only a `fell_back` attempt with a result, a bad or expired token, a run not running; a later judge attempt reads its root's members); `TestFailReviewMemberKeepingResult` (state `failed`, reason set, result kept; a member not reported: false); `TestCountRunningCountsReviewMembers`; `TestReviewMemberResultToken` (as the panel's); `TestSupersedeOrphanReviewMembers`; `TestEnqueueJudgeRetry` (outcome `fell_back`, reason `judge_failed`, a new queued attempt; a run not reported: false); `TestSetJudgeRoundTrip`; `TestPruneRunLogsPrunesReviewMembers`; `TestPanelMembersUntouched` (nothing here writes `panel_members`).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'MultiReview|ReviewMember|ReviewerResult|Judge|PanelMembersUntouched'`. Expected: FAIL, undefined `StartMultiReview`.
- [ ] **Step 3: Code.** The lock order of `panel.go`'s header comment holds.
- [ ] **Step 4: Run** the whole store package. PASS.
- [ ] **Step 5: Commit** `feat(store): review members of reviewers 1 to 5`

### Task M1.5: Store: usage per model of a multi review

**Files:**
- Modify: `internal/store/usage.go` (new `RecordReviewMemberUsage`, `RecordJudgeUsage`, `ReviewMembersWithoutUsage`, `ReviewCosts`, `reviewMemberUsageRunSQL`)
- Test: `internal/store/usage_multi_review_test.go` (new)

**Interfaces:**

```go
// RecordReviewMemberUsage is RecordMemberUsage for review_members: rows of
// part reviewer in run_usage_by_model, tokens added to run_usage of the run
// that was split, the member marked, in one transaction; once only.
func (s *Store) RecordReviewMemberUsage(ctx context.Context, id string, skills []SkillUsage) error

// RecordJudgeUsage is RecordUsage of a judge run: each skill's tokens added
// to the run's run_usage row with the judge's verdict and counts, and one
// part judge row per skill with the review_judges entry (none with
// no_judge). usage_recorded, read under the run's lock, stops a second count.
func (s *Store) RecordJudgeUsage(ctx context.Context, id string, skills []SkillUsage) error

func (s *Store) ReviewMembersWithoutUsage(ctx context.Context, limit int) ([]ReviewMember, error)

// ReviewCost is what one model used in a multi review.
type ReviewCost struct {
	Part                                    string // "reviewer" or "judge"
	Reviewer                                int    // 0 for the judge
	RunID, MemberID, Entry, Provider, Model string
	In, Out                                 int64
}

// ReviewCosts returns every reviewer attempt's rows and every judge run's
// rows of run id's review: reviewers by number and attempt, then the judge
// runs oldest first.
func (s *Store) ReviewCosts(ctx context.Context, id string) ([]ReviewCost, error)
```

`reviewMemberUsageRunSQL` is `memberUsageRunSQL` on `review_members` and `'review_members'`. `panelNoResult` (`usage.go:1141`) also matches a run whose review has a non-off `review_members` row.

- [ ] **Step 1: Failing tests:** `TestRecordReviewMemberUsage` (rows with the member's source, team and reviewer number; `run_usage` of the split run gets the tokens; a second call adds nothing); `TestRecordJudgeUsageTotal` (after reviewers 1, 2, 3 and the judge, `run_usage` holds the sum per skill with the judge's verdict); `TestRecordJudgeUsageNoJudgeWritesNoModelRow`; `TestReviewCostsOrder`; `TestReviewNoResultIsWasted` (a judge run reported with no result: its members' tokens count as wasted, as `panelNoResult` does).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run 'ReviewMemberUsage|JudgeUsage|ReviewCosts|ReviewNoResult'`. Expected: FAIL, undefined.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the store package. PASS.
- [ ] **Step 5: Commit** `feat(store): tokens per model of a multi review`

---

## Phase M2: the Reviewers card

Floor `""` refuses every added reviewer, so nothing in this phase changes a review on `main`.

### Task M2.1: Reviewers API, rights, refusals, audit, repository view

**Files:**
- Create: `internal/dashboard/api_reviewers.go`
- Modify: `internal/dashboard/api_read.go` and `api_write.go` (repository suffix `/reviewers`, beside `/chain`), `internal/dashboard/view.go` (`ReviewersView`, `RepositoryView.MultiReview`), `internal/dashboard/api_chains.go` (`panelImageRefusal` → `reviewerImageRefusal`, `repoChainSummary`)
- Modify: `openapi.yaml`, `internal/dashboard/openapi_test.go`, `internal/dashboard/routes_test.go`
- Test: `internal/dashboard/api_reviewers_test.go` (new)

**Interfaces:**

```go
// ReviewersView is GET .../reviewers: reviewers 2 to 5 and the judge
// (reviewer 1 is GET .../chain). Each ChainView is the repository chain's
// shape, its choices included. Never a base URL or a key.
type ReviewersView struct {
	ReadOnly  bool            `json:"readOnly"`
	Reviewers []ReviewerChain `json:"reviewers"` // reviewer 2 first
	Judge     *ChainView      `json:"judge"`     // null with no judge row
	Warnings  []string        `json:"warnings"`
	// AddRefusal is why a reviewer can't be added now ("" when it can):
	// the image check, or at most 5 reviewers.
	AddRefusal string `json:"addRefusal"`
}
type ReviewerChain struct {
	Number int       `json:"number"`
	Chain  ChainView `json:"chain"`
}

// MultiReviewView is a repository's multi review on its page.
type MultiReviewView struct {
	On        bool   `json:"on"`
	Reviewers int    `json:"reviewers"` // 1 when off
	Judge     string `json:"judge"`     // the judge chain's first entry name; "" for none or removed
}
```

`PUT .../reviewers` body: `{"reviewers": [[{"source","id"}...], ...], "judge": [{"source","id"}...]}`. Order of checks: rights (as `putRepoChainOf`), decode (400s), counts and empties (`at most 5 reviewers`, `a reviewer needs an entry; remove it instead`, `2 or more reviewers need a judge`, `a judge needs 2 or more reviewers`, chain A's too-long text), `checkEntries` on the changed slots only (a chain a remove moved down, with the same entries, is unchanged: `SaveReviewers` rule), same first model, the image check (only when the save has more extra reviewers than stored), `SaveReviewers`. Same first model: the first entry of reviewer 1 (the chain in effect, `chain.Load`) and of each extra reviewer, compared by provider and model ignoring case; removed entries are left out. Message: `reviewer <k> starts with the same model as reviewer <j>`. Warnings (GET and the PUT answer): `No judge: findings are merged without checking` (extras and an empty judge); `Reviewer <k> starts with the same model as reviewer <j>, so it runs on its next entry` (when chain A or a removal made them equal). Audit, one entry per slot whose entries changed: action `reviewers_changed`, detail `reviewer=<n> entries=<names>` or `judge entries=<names>`, `(none)` for a slot that is gone. Reviewer 1 stays `model_chain_changed` through `.../chain`.

- [ ] **Step 1: Failing tests** (`newEnv`; `e.dashboardRepo` + `e.aliceOn`):
  - `TestReviewersRefusedWithoutFloor`: floor `""`: adding reviewer 2 is 400 `no review image supports multi review yet`; nothing stored.
  - `TestReviewersSavedWithFloor`: floor and `jobs.image` `v9.0.0`: an admin saves 3 extra reviewers and a judge; GET shows them; 4 audit entries, names only.
  - `TestReviewersImageTooOld`: 400 with the tag in the message.
  - `TestReviewersRemoveNeedsNoImage`: floor `""` with stored reviewers (inserted in the test): removing one is 200.
  - `TestReviewersRights`: viewer 403 on PUT and `readOnly: true` on GET; a maintainer of the owner team saves; a non-member 404.
  - `TestReviewersForeignEntry`: another team's entry: 400 (chain A's text).
  - `TestReviewersSameFirstModel`: reviewer 3 starts with reviewer 1's model, another case: 400; the same model as a backup: 200.
  - `TestReviewersCountsAndEmpties`: the 4 messages above.
  - `TestReviewersWarnings`: an emptied judge (team entry deleted) gives the no-judge warning; chain A changed to reviewer 2's model gives the same-model warning.
  - `TestReviewersAuditPerSlot`: removing reviewer 2 of 3 writes `reviewer=2 entries=<old r3 names>` and `reviewer=3 entries=(none)`.
  - `TestReviewersMaintainerRemovesBeforeAdminReviewer`: an admin's reviewer 3 names a global entry teams may not use; a maintainer removes reviewer 2: 200, and the moved chain keeps `saved_by_admin`.
  - `TestRepositoryViewMultiReview`: `{"on": true, "reviewers": 3, "judge": "<name>"}`; none stored: `{"on": false, "reviewers": 1, "judge": ""}`.
  - `TestReviewersNoKeyInResponses`.
  - The OpenAPI tests (existing, `-run OpenAPI`: `TestOpenAPIRoutesMatchCode`, `TestOpenAPISchemasMatchGoTypes`, ...) with the route, its schemas and `x-pergent-access`.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/dashboard/ -run 'Reviewers|RepositoryViewMultiReview|OpenAPI'`. Expected: FAIL, 404 on `/reviewers`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** `go test ./internal/dashboard/ -run OpenAPI`, then the package with the DB variable. PASS.
- [ ] **Step 5: Commit** `feat(dashboard): the reviewers of a repository`

### Task M2.2: Remove the chain B API

**Files:**
- Modify: `internal/dashboard/api_operator.go:83-84`, `api_team_models.go:32-33,513-575` (the removal's audit text and log arg are M1.2's), `api_chains.go` (every `ChainB` branch, `getGlobalPanelChain`, `putGlobalPanelChain`, `repositoryPanelChain`, `putRepoPanelChain`, `resetRepoPanelChain`, `offRepoPanelChain`, `msgGlobalBOnly`, `msgEmptyRepoPanel`), `api_write.go:541-580`, `api_read.go:1138`, `view.go` (`PanelView`, `ChainView.Mode`'s chain B text, the route list at `:1115`)
- Modify: `openapi.yaml` (the 5 `/panel-chain` paths, `PanelView`), `internal/dashboard/openapi_test.go`, `routes_test.go`, `docs/reference/http-routes.md`
- Delete: `internal/dashboard/api_panel_chains_test.go` (and the chain B cases of other test files)
- Test: `internal/dashboard/routes_test.go`

- [ ] **Step 1: Failing tests:** `TestPanelChainRoutesGone` (each of the 8 method and path pairs answers 404 for an admin); `TestRepositoryViewHasNoPanel` (no `panel` key); the OpenAPI tests (`-run OpenAPI`).
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/dashboard/ -run 'PanelChainRoutesGone|OpenAPI'` (then with the DB variable for the view test). Expected: FAIL, 200 on `/api/admin/panel-chain`.
- [ ] **Step 3: Code.** The store's `ChainB` stays until M4.2 (the engine reads it until M3.4).
- [ ] **Step 4: Run** the dashboard package. PASS.
- [ ] **Step 5: Commit** `feat(dashboard)!: drop the chain B routes`

### Task M2.3: Web: the Reviewers card and the repository line

**Files:**
- Create: `web/src/ReviewersCard.tsx`, `web/src/reviewers.ts` (pure helpers)
- Modify: `web/src/pages/RepoModelChain.tsx` (one card: reviewer 1 is today's `RepoModelChainA` body; drop `RepoPanelChain`), `web/src/pages/TeamModels.tsx:48-71,145,156`, `web/src/pages/AdminSettings.tsx:680-743`, `web/src/pages/RepositoryDetail.tsx`, `web/src/api.ts` (types, `paths.apiRepoReviewers`, drop the panel-chain paths)
- Delete: `web/src/PanelChainCard.tsx`
- Test: `web/src/reviewers.test.ts` (new), `web/src/ReviewersCard.test.tsx` (new), `RepoModelChain.test.tsx`, `TeamModels.test.tsx`, `AdminSettings.test.tsx`, `RepositoryDetail.test.tsx`, `RepositorySettings.test.tsx`

**Interfaces:**

```ts
// costHint is the line under the card: "" for 1 reviewer.
export function costHint(reviewers: number): string;
// "Each review now runs 3 reviews and 1 judge, about 3 to 4 times the cost of one review."
// firstJudge is what the editor puts in the judge when reviewer 2 is added:
// reviewer 2's first entry.
export function firstJudge(r2: ChainRef[]): ChainRef[];
// removeReviewer drops reviewer n (2..5); later ones move up; the judge is
// cleared when no extra reviewer is left.
export function removeReviewer(s: ReviewersDraft, n: number): ReviewersDraft;
```

The card follows the design's sketch: title `Reviewers`; `Reviewer 1` with Edit (today's chain A editor, with inherit); `Reviewer <n>  (multi review, experimental)` with Edit and Remove; `Add a reviewer` (disabled with `addRefusal` as its title); `Judge` select, shown with 2 or more reviewers; the warnings; `costHint`. Each chain uses `ChainEditor` with `maxChain`. A refused save shows the server's message. Read-only for a viewer. The repository page shows `Multi review: on (3 reviewers, judge <name>)`, `Multi review: on (3 reviewers, no judge)` or `Multi review: off`.

- [ ] **Step 1: Failing tests:** `costHint` (1 → `""`, 2 → `about 2 to 3 times`, 5 → `runs 5 reviews and 1 judge, about 5 to 6 times`); `removeReviewer` (removing 2 of 4 renumbers 3 and 4; removing the last clears the judge); `firstJudge`; the card: adding reviewer 2 fills the judge with its first entry and PUTs both; Remove PUTs the shorter list; a 400 shows its text; a viewer sees no Edit, Remove or Add; the no-judge warning shows; Add is disabled with the refusal text; the team and admin pages have no `chain B` text; the repository line. A "disabled" test uses a case the old code left enabled.
- [ ] **Step 2: Run to fail.** `cd web && npx vitest run src/reviewers.test.ts src/ReviewersCard.test.tsx src/pages/RepositoryDetail.test.tsx`. Expected: FAIL, module not found.
- [ ] **Step 3: Code.**
- [ ] **Step 4:** the web gate. PASS.
- [ ] **Step 5: Commit** `feat(web): the Reviewers card`

---

## Phase M3: multi reviews

Each task replaces one part of the panel in the engine and deletes the panel tests it replaces. Phase M3 merges as one PR. Floor `""` means no review splits on `main` meanwhile. M3.9 pins pergent `master` abedce0, where the CLI is merged. The panel's own store, kube and callback code goes in M4.2, after the run page stops reading it. Every task builds on its own: it edits every caller of a symbol it deletes or retypes.

### Task M3.1: Kube: reviewer Jobs and the judge Job

**Files:**
- Modify: `internal/kube/job.go:130-151` (`JobParams.ReviewMemberID`, `JobParams.Reviewer`, `JobParams.Judge *JudgeParams`), `:208-230` (labels), `:290-383` (`BuildJob`), `:442-470` (the `mr-dismissals.json` item), `:540` (`BuildConfigMap`), `:584-604` (`BuildSecret`), `internal/kube/panel.go:82-87` (`deadline`)
- Create: `internal/kube/judge.go` (`JudgeParams`, `forJudge`, `judgeArgs`, `reviewersContainer`, the judge constants)
- Modify: `cmd/pergent-sh/graph.go` (`get-reviewers`, sharing `get-panel`'s fetch code)
- Test: `internal/kube/judge_test.go` (new), `internal/kube/job_test.go`, `cmd/pergent-sh/graph_test.go`

**Interfaces:**

```go
const (
	LabelReviewMember = "pergent.sh/review-member"
	LabelReviewer     = "pergent.sh/reviewer"
	ReviewerJudge     = "judge"
	// JudgeTimeout is the judge Job's --timeout; JudgeFetchTimeout bounds
	// get-reviewers. M4.2 moves the values here and drops the Panel names.
	JudgeTimeout      = PanelTimeout
	JudgeFetchTimeout = PanelFetchTimeout
	// MRDismissalsFile is where the judge's pergent reads --mr-dismissals.
	MRDismissalsFile = "/in/mr-dismissals.json"
)

// JudgeParams is what a judge Job needs beyond a review Job's checkout.
type JudgeParams struct {
	BaseURL   string // <callback>/runs/<run>/reviewers
	Reviewers []int  // every reviewer of the frozen set, in order (D2)
	NoJudge   bool   // --no-judge: no model runs and no key is held
	Image     string // pergent.sh's own image, for get-reviewers
	// MRDismissals is the merge request's dismissals, JSON
	// [{"fingerprint","severity"}] (D11). Empty is written as [].
	MRDismissals []byte
}
```

`forJudge` drops what `forPanel` drops and sets `Timeout` to `JudgeTimeout`. Args: `--multi-review`, then `--reviewer=/panel/reviewer-<n>.json` per entry of `Reviewers`, then `--mr-dismissals=/in/mr-dismissals.json`, then `--no-judge` when set (D1, D11). `BuildConfigMap` of a judge Job adds the key `mr-dismissals.json` (`[]` when `MRDismissals` is empty), and `BuildJob` maps it into the `inputs` volume (`/in`, read-only, outside `/work`). It is a flag, not an env var: pergent has no env for it, and a judge image is always at the floor. It is there with `--no-judge` too. `deadline(p)` adds `JudgeFetchTimeout` for `p.Judge`, as it does for `p.Panel`. `labels(p)` sets `pergent.sh/reviewer` from `p.Reviewer` (`1`..`5` or `judge`) and `pergent.sh/review-member` from `p.ReviewMemberID` when set. `reviewersContainer` runs `/pergent-sh get-reviewers --base-url=<BaseURL> --reviewers=1,2,3 --dir=/panel` with `PERGENT_RESULT_TOKEN` only. The panel volume grows to `64Mi` (5 results of 10 MiB and a temp file). Key handling:

```go
// BuildSecret: a judge Job holds the judge's key as llm-api-key, and none
// with --no-judge. No Job ever gets llm-api-key-b again.
if p.Judge != nil && p.Judge.NoJudge {
	delete(data, keyLLMKey)
}
// BuildJob: OPENCODE_API_KEY is named only when p.Judge == nil || !p.Judge.NoJudge.
```

`get-reviewers` writes `<dir>/reviewer-<n>.json` with mode 0644, at most 10 MiB each, 3 tries, a 404 or a failure writes no file, always exits 0, and accepts only numbers 1..5.

- [ ] **Guard (passes before and after):** `TestBuildJobUnchanged` with no golden edit; the existing panel Job tests.
- [ ] **Step 1: Failing tests:** `TestBuildReviewerJob` (`SkipSkillVerify`, `ReviewMemberID`, `Reviewer: "3"`: `--no-skill-verify` and the 2 labels, nothing else against the golden); `TestBuildJudgeJob` (`Reviewer: kube.ReviewerJudge`, `Reviewers: [1, 2, 3]`: args in order `--multi-review --reviewer=/panel/reviewer-1.json --reviewer=/panel/reviewer-2.json --reviewer=/panel/reviewer-3.json --mr-dismissals=/in/mr-dismissals.json`, then `--timeout=10m0s`; `activeDeadlineSeconds` is 10m + `JudgeFetchTimeout` + the deliver and clone allowances; label `pergent.sh/reviewer=judge`; no previous, carried, CI, description, linked or graph input; init containers `checkout` then `reviewers`; no `PERGENT_PANEL_B_*` anywhere); `TestBuildJudgeJobMRDismissals` (the ConfigMap key holds the bytes; empty gives `[]`; the item maps `mr-dismissals.json`; a `NoJudge` Job has the key and the flag; a review Job and a reviewer Job have neither); `TestBuildJudgeJobKeys` (Secret keys exactly `git-token`, `llm-api-key`, `result-token`; `OPENCODE_API_KEY` once, only in `pergent`; the fetch container gets only `PERGENT_RESULT_TOKEN`); `TestBuildJudgeJobNoJudge` (`--no-judge`, no `llm-api-key`, no `OPENCODE_API_KEY`); `TestGetReviewersWritesByNumber` (2 servers' answers land as `reviewer-1.json` and `reviewer-3.json`; a 404 writes no file; exit 0; `--reviewers=6` refused).
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/kube/ ./cmd/pergent-sh/ -run 'BuildJob|BuildReviewer|BuildJudge|GetReviewers'`. Expected: FAIL, undefined `JudgeParams`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run.** PASS. Then `go vet ./...`.
- [ ] **Step 5: Commit** `feat(kube): reviewer and judge Jobs`

### Task M3.2: Callback: reviewer results for the judge, member results on the new table

**Files:**
- Create: `internal/callback/reviewers.go` (`ReviewersHandler`, `ReviewerResults` interface)
- Modify: `internal/callback/member.go` (`MemberResults` on the `ReviewMember` store functions), `internal/server/server.go:63-132` (`GET /runs/{id}/reviewers/{n}`, registered when `Deps.Reviewers` is set; `Deps.Panel` and its route stay until M4.2)
- Test: `internal/callback/reviewers_test.go` (new), `internal/callback/member_test.go`, `internal/server/server_test.go`

```go
type ReviewerResults interface {
	// ReviewerResult is the result of reviewer n of run runID's review for
	// the run's live result token (store.ReviewerResult): its newest attempt
	// that is done, or failed with its result kept.
	ReviewerResult(ctx context.Context, runID, token string, n int) ([]byte, bool, error)
}
```

- [ ] **Step 1: Failing tests:** 200 with the bytes for a good token and `n` 1..5, a `failed` member with its result kept included; one 404 for a bad token, `n` `0`, `6` or `x`, an unknown run, no result; the body and token never logged; `server_test` lists the route on the public mux only; the member handler tests, ported to `ReviewMember`, pass (store, wrong commit, unknown skill, wrong run 401, usage written).
- [ ] **Step 2: Run to fail.** `go test -count=1 ./internal/callback/ ./internal/server/ -run 'Reviewer|Member'`. Expected: FAIL, undefined `ReviewersHandler`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run.** PASS.
- [ ] **Step 5: Commit** `feat(callback): serve the judge Job its reviewers' results`

### Task M3.3: Store: a waiting multi review is a live review; old panel runs

**Files:**
- Modify: the skip-reason literal sites of Finding 16 only (`'panel_members'` → `'review_members'`, `WaitPanel` → `WaitReviewers`): `runs.go:58,709,903,985,1760`, `dashboard_read.go:122,172`, `apireviews.go:144,244`, `apiclients.go:239`, `reviewapi/body.go:301`, `api_commands.go:173`; the API sites drop the waiting clause, as an API review never splits. Not here: `CountRunning`'s table count (`runs.go:671`, M1.4 and M4.2), `memberUsageRunSQL` and `panelNoResult` (`usage.go:224-229,1141`, M1.5 and M4.2), `apiclients.go:260` (a `panel_members` Job query, M4.2).
- Create: `internal/store/legacy_panel.go`, `internal/engine/legacy_panel.go`
- Modify: `cmd/pergent-sh/main.go:423-430` (call it in the start loop, before the usage fill)
- Test: `internal/store/multi_review_live_test.go` (new), `internal/engine/legacy_panel_test.go` (new)

**Interfaces:**

```go
// LegacyPanel counts what a main build of the review panel left: chain B
// rows with entries, and panel_members rows.
func (s *Store) LegacyPanel(ctx context.Context) (chains, members int, err error)

// RequeueLegacyPanels queues each run waiting on 'panel_members' again as a
// single review (skip_reason '', not_before NULL; dispatched_at stays, so
// it is never split), supersedes the live panel_members of those reviews,
// and returns their Job names, in one transaction.
func (s *Store) RequeueLegacyPanels(ctx context.Context) (runs int, jobs []string, err error)

// (engine) RecoverLegacyPanels logs one warning when LegacyPanel finds
// anything ("review panel data from a main build is not used"), requeues,
// deletes the Jobs (best effort) and wakes the dispatcher.
func (e *Engine) RecoverLegacyPanels(ctx context.Context)
```

- [ ] **Step 1: Failing tests:** port each of `TestStartRunLeavesPanelAlone`, `TestRerunWhilePanelRuns`, `TestWaitingChangesHidesPanel`, `TestDashboardOutcomePanelRunning`, `TestStartButtonRefusesPanel` to a `review_members` run, with `MultiReview` in each new name; `TestRequeueLegacyPanels` (a `panel_members` waiting run is queued, `dispatched_at` unchanged; its live members superseded; their Job names returned; a `review_members` run untouched); `TestRecoverLegacyPanelsWarnsOnce` (engine: one warning, the Jobs deleted, the run queued; its dispatch as one review is M3.7's `TestE2ELegacyPanelRunIsSingleReview`, since the panel's `dispatchRun` still reads `panel_members` here).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ ./internal/reviewapi/ ./internal/dashboard/ ./internal/engine/ -run 'MultiReview|Legacy'`. Expected: FAIL.
- [ ] **Step 3: Code.** A panel test that fails only because the reason moved is deleted here (its multi review version is in M3.4 to M3.7).
- [ ] **Step 4: Run** the 4 packages. PASS.
- [ ] **Step 5: Commit** `feat(store): a multi review waiting for its members is a live review`

### Task M3.4: Dispatch: the split

**Files:**
- Create: `internal/engine/multi_review.go` (`split`, `offReason`, `skipWords`, `sameModels`), `internal/engine/multi_review_test.go` (moves `TestDispatchJobUnchanged` here unchanged, with its golden, and `startRunOld` from `e2e_panel_rolling_test.go:14`, unchanged)
- Modify: `internal/engine/dispatch.go:791-801` (`d.split` in place of `d.panel`), `internal/engine/chain.go:109-117` (drop `resolveB`), `internal/engine/apireview.go:46-400` (drop every member and merge branch, the `PanelMembers` read at `:64` and the `d.panel` call at `:285`: an API review is today's review, so nothing here reads `d.member`, `d.merge`, `resolveMerge`, `PanelParams` or `LLMKeyB`)
- Delete: `internal/engine/panel.go`'s `panel`, `panelOff`, `panelOffReason` and `panelOff*` constants; the split tests of `panel_test.go`; `TestRecoverUnlaunchedMembers` (`watch_test.go:511-531`, M3.5 ports it); and every test that needs a panel split (`e2e_panel_test.go`, `e2e_panel_judge_test.go`, `e2e_panel_publish_test.go`, `e2e_panel_rolling_test.go` after `startRunOld` moved, `e2e_api_panel_test.go`): a split now writes `review_members`, and M3.5 to M3.9 port each of them
- Test: `internal/engine/multi_review_test.go`, `internal/engine/e2e_api_test.go`

**Interfaces:**

```go
// Why a multi review runs as one review (after "multi review off: ").
const (
	offImage   = "the review image does not support it"
	offFetch   = "pergent.sh's own image is not set"
	offQuota   = "the token quota is used up"
	offNone    = "no other reviewer can start"
	offRead    = "the reviewers could not be read"
	skipSame   = "no different model left"
	skipQuota  = "the token quota"
	skipNone   = "no usable entry"
)

// split decides whether the run is a multi review, after every check a
// review passes and the run's writes, before its token. done is true when
// the dispatch ends here (StartMultiReview split it, or the run was gone).
// With no extra reviewer nothing is written and done is false.
func (d *dispatcher) split(ctx context.Context, mr forge.Change, fitted *inputs.Inputs, repo settings.Effective, repoUsedUp, instanceUsedUp bool) (done bool, err error)

// skipWords is why an extra reviewer can't start, in the fixed words:
// skipSame when every skip is same_model, skipQuota on Hold, else skipNone.
// The split and memberCannotRun (M3.5) both use it.
func skipWords(dec chain.Decision) string

// sameModels are the models of the decisions that run, for Avoid. The split
// and modelsBefore (M3.5) both use it.
func sameModels(ds []chain.Decision) []chain.SameModel
```

Order: `DispatchedAt` set → false (split once). `chain.LoadReviewers` error → off `offRead`. No extra → false, no write. Image (`config.PanelImage`) → `offImage`; `ScanImage == ""` → `offFetch`; `repoUsedUp` → `offQuota`. Then for k = 2..N: `Resolve(FromFrozen(Freeze(extra), base))` with `Avoid` = `sameModels` of reviewer 1's decision (`d.entry`) and of every earlier extra reviewer that can start, and `InstanceQuotaUsedUp`; a reviewer that can't start gets `skipWords(dec)`. No extra can start → off `offNone`. Else `StartMultiReview` with every reviewer's `Freeze` (reviewer 1 from `rs.One`), the judge's `Freeze` (no entries when the judge chain is empty), the base and the inputs as `panel` records them, then `e.Wake()`. An off reason goes in `RecordMultiReviewOff` and the review goes on as today.

- [ ] **Step 1: Failing tests:**
  - `TestDispatchJobUnchanged` (moved, golden unchanged), plus one new check: no `review_members` row.
  - `TestE2EAPIReviewNeverMultiReview` (in `e2e_api_test.go`): floor and `ScanImage` set, a repository of the same path with reviewers (saved with `Store.SaveReviewers`): the API review runs one Job, writes no `review_members` row, drops its files after the Job starts.
  - `TestSkipWords`: `same_model` only → `no different model left`; `Hold` → `the token quota`; a mix or `removed` → `no usable entry`.
  - `TestDispatchSplitsMultiReview`: 3 reviewers: the run waits (`review_members`), 3 members queued with their copies, the judge copy stored, no Job, no status.
  - `TestDispatchSplitSkipsSameModel`: reviewer 3's only entry is reviewer 2's model: reviewer 3 `skipped` `no different model left`; reviewer 2 runs.
  - `TestDispatchSplitInstanceQuota`: reviewer 2 has only global entries under a used-up instance quota: `skipped` `the token quota`.
  - `TestDispatchMultiReviewImageTooOld`, `TestDispatchMultiReviewRepoQuota`, `TestDispatchMultiReviewNoneCanStart`: today's Job and an off row with `offImage`, `offQuota`, `offNone`.
  - `TestDispatchSplitEmptyJudge`: an emptied judge chain: split, the judge copy has no entries.
  - `TestDispatchSplitOnce`: a released run with `dispatched_at` is not split again.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run 'Dispatch.*(MultiReview|Split|JobUnchanged)|APIReviewNeverMultiReview|SkipWords'`. Expected: FAIL, undefined `split`.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the engine package. PASS.
- [ ] **Step 5: Commit** `feat(engine): split a review into its reviewers`

### Task M3.5: Dispatch, watch and clean up a reviewer's Job

**Files:**
- Modify: `internal/engine/dispatch.go` (`dispatcher.member store.ReviewMember`, `members []store.ReviewMember`; `DispatchOnce` claims `ClaimQueuedReviewMembers`; `launch`: `ReviewMemberID`, `Reviewer: strconv.Itoa(n)`, the member result URL; the member helpers `fail`, `retry`, `requeue`, `postpone`, `supersede`, `supersedeDraft` on the new store functions)
- Modify: `internal/engine/multi_review.go` (`dispatchMember`, `resolveMember`, `modelsBefore`, `triedBy`, `memberCannotRun`, `memberInputsMatch`, moved from `panel.go` and generalized), `internal/engine/watch.go:41-260`, `internal/engine/logs.go`, `internal/engine/publish.go` (`CleanupOnce` on `SupersedeOrphanReviewMembers` and `ReviewMembersToCleanUp`)
- Test: `internal/engine/e2e_multi_review_test.go` (new; its helpers generalize Finding 25's: `saveReviewers(t, env, [][]string, judge []string)`, `newMultiReviewE2E`, `splitReview`, `memberOf(t, env, id, n)`)

**Interfaces:**

```go
// resolveMember picks member m's entry from its frozen chain (FromFrozen
// with the registry now): its reviewer's earlier attempts are tried, and
// reviewer k > 1 avoids modelsBefore(k). Reviewer 1 avoids none and is
// never held by a quota (the run passed the hold). Reviewer k > 1 starts no
// attempt while the repository quota is used up (spec "Quota").
func (d *dispatcher) resolveMember(ctx context.Context, repo settings.Effective, repoUsedUp, instanceUsedUp bool) (chain.Decision, error)

// modelsBefore are the models reviewers 1 to k-1 run on now: sameModels
// (M3.4) of each one's newest attempt with an entry, else of what its
// frozen chain resolves to now.
func (d *dispatcher) modelsBefore(ctx context.Context, ms []store.ReviewMember, k int, instanceUsedUp bool) ([]chain.SameModel, error)
```

`memberCannotRun`: reviewer 1 waits on `Wait` and fails on `Exhausted`, as member A; an extra reviewer fails with `skipWords(dec)` (M3.4), or `skipQuota` when the repository quota is used up; `chain.FailureText(dec)` goes in `failure_detail` only. `dispatchMember` of an API run fails the member with `an API review never runs multi review`. Labels name the run, the member and the number; the running status is set by the first member Job only.

- [ ] **Step 1: Failing tests** (ports of `e2e_panel_test.go:133-943`): `TestE2EMultiReviewMembersStart` (3 member Jobs, each Secret holds its own key only, `--no-skill-verify`, labels, one running status); `TestE2EMultiReviewMemberSkipsChecks`; `TestE2EMultiReviewSkipsEarlierModels` (reviewer 3's chain `[reviewer 1's model, reviewer 2's model, other]`: it runs on `other`); `TestE2EMultiReviewHeadMoved`; `TestE2EMultiReviewMRClosed`; `TestE2EMultiReviewRetryGivesUp`; `TestE2EMultiReviewMaxRunning` (`jobs.max_running` 2 and 3 reviewers); `TestE2EMultiReviewMemberGone`, `...Deadline`, `...Stuck`, `...Log` (in `review_member_logs`), `...NoGraph`, `...Rules`, `...SplitBase`, `...Skills`; `TestE2EMultiReviewCleanup`; `TestRecoverUnlaunchedReviewMembers` (port of the deleted `TestRecoverUnlaunchedMembers`); `TestE2EMultiReviewRepoQuotaStopsExtra` (the repository quota is used up after the split, before reviewer 3's Job starts: reviewer 3 fails with `the token quota`; reviewers 1 and 2 start).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run 'E2EMultiReview'`. Expected: FAIL.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the engine package. PASS.
- [ ] **Step 5: Commit** `feat(engine): start, watch and clean up reviewer Jobs`

### Task M3.6: Settle reviewers; release or fail the review

**Files:**
- Modify: `internal/engine/multi_review.go` (`settleMembers`, `settleMember`, `fallBackMember` from `panel.go:379-509`), `internal/engine/publish.go:75`
- Test: `internal/engine/e2e_multi_review_test.go`

A reported member falls back only on a merge request's review: a provider refusal (`refusedMark`, with the chain length of its frozen copy; a 1-entry chain never falls back) marks the entry, audits `model_marked`, and queues its next attempt with the copy; otherwise `SettleReviewMember`. When its frozen copy has no entry left to run (resolving it with this attempt's and the earlier attempts' entries tried does not `Runs()`), the entry is marked and audited but no attempt is queued: `FailReviewMemberKeepingResult` with `chain.ReasonWords(class)`. The judge route then still serves that result, and the CLI carries its blocking findings (pergent `cmd/reviewer.go:162-176`). Then `MultiReviewsToRelease` and `ReleaseMultiReview` (it queues the judge only when a reviewer is `done`); released wakes the dispatcher, failed wakes the publisher.

- [ ] **Step 1: Failing tests:** `TestE2EMultiReviewMemberPostsNothing` (between the member results and the judge Job, no comment, note, thread, approval or status change); `TestE2EMultiReviewMemberFallsBack` (reviewer 2's quota: marked, `fell_back`, its next attempt on its next frozen entry; the others untouched); `TestE2EMultiReviewFallbackSkipsEarlierModel`; `TestE2EMultiReviewExtraOutOfTokens`, 2 cases: (a) reviewer 3's 2-entry chain, both refused: after the second refusal reviewer 3 is `failed` with its result kept and no third attempt, the run is released; (b) reviewer 3's 1-entry chain refused: no mark and no fallback, the member is `done` with its result; `TestE2EMultiReviewEveryReviewerFails` (published failed with each reviewer's reason, the failure status, no approval, never `success`); `TestE2EMultiReviewReviewer1FailsOthersDone` (released); `TestE2EMultiReviewTeamEntryDeletedMidReview` (Review Focus 3: the entry is deleted after the split; the live slot renumbers; the member's next attempt skips it as `removed`).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run 'E2EMultiReview'`. Expected: FAIL.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the engine package. PASS.
- [ ] **Step 5: Commit** `feat(engine): settle reviewers and release the judge`

### Task M3.7: The judge stage

**Files:**
- Modify: `internal/engine/dispatch.go` (`stageMerge` → `stageJudge`; `dispatchRun` and `panelStage` on `ReviewMembers`; `InheritMultiReviewSplit`; the judge stage reads the merge request's dismissals before `Fit` (`:637-664`); the judge launch (`:914-926`) with `params.Judge`, `params.Reviewer = kube.ReviewerJudge` and the judge's key alone; drop `merge`, `LLMKeyB`, `params.Panel`. M3.4 already took them out of `apireview.go`.)
- Modify: `internal/engine/multi_review.go` (`resolveJudge`, `judgePlan`, `recordJudge`, `judgedBefore`), `internal/engine/fallback.go` (`fallBack`: a judge run's `provider_error.role == "judge"` falls back the judge entry; `retryJudge` for a judge run with no usable result)
- Delete: `resolveMerge`, `pickJudge`, `roleLLM`, `mergePlan`, `recordJudges`, `fallBackJudge`; `e2e_panel_judge_test.go`
- Test: `internal/engine/e2e_judge_test.go` (new)

**Interfaces:**

```go
// judgePlan is what the judge Job runs with: every reviewer of the frozen
// set, in number order (one with no stored result gets no file); the judge
// entry, or no judge and why.
type judgePlan struct {
	reviewers []int
	entry     chain.Decision // the judge's entry; zero with noJudge
	noJudge   bool
	why       string
}

// key is the only LLM key the judge Job holds: the judge entry's, "" with
// no judge.
func (p judgePlan) key() string { if p.noJudge { return "" }; return p.entry.Entry.Key }

// resolveJudge reads the frozen judge chain (JudgeChainOf) with the
// registry now, skipping the entries earlier attempts of this judge fell
// back from (judgedBefore). No judge when: the copy has no entries ("no
// judge chain"); an earlier attempt of the review closed with reason
// judge_failed ("the judge Job failed"); the repository quota is used up;
// or the chain resolves to no entry (marks, keys, the instance quota on
// global entries). d.entry is the judge (OPENCODE_*); with no judge it is
// empty: no LLM and no key (D4).
func (d *dispatcher) resolveJudge(ctx context.Context, repo settings.Effective, repoUsedUp, instanceUsedUp bool) error

// retryJudge acts on a reported judge run with no usable result (no result,
// a result whose status is not completed, or one that fails its checks)
// whose review_judges row has no_judge false: EnqueueJudgeRetry. handled is
// false otherwise, and the run is published as it is.
func (e *Engine) retryJudge(ctx context.Context, run store.Run, log *slog.Logger) (handled bool, err error)

// mrDismissalsJSON is the --mr-dismissals file (D11): one
// {"fingerprint","severity"} per stored dismissal with a fingerprint, in
// store order, no actor or reason; [] for none.
func mrDismissalsJSON(ds []store.Dismissal) []byte
```

The judge stage reads `ds, err := e.Store.Dismissals(ctx, run.MR)` right before `Fit` (the list publish applies, `publish.go:153`); a read error retries the dispatch, never a Job without the file. `mrd := mrDismissalsJSON(ds)`; `in.Fit(inputs.ConfigMapLimit - len(mrd))`. The judge Job: `params.Judge = &kube.JudgeParams{BaseURL: <callback>/runs/<run>/reviewers, Reviewers: plan.reviewers, NoJudge: plan.noJudge, Image: e.ScanImage, MRDismissals: mrd}`, `params.Reviewer = kube.ReviewerJudge`, `secrets := kube.Secrets{GitToken: ..., LLMKey: d.judge.key(), ResultToken: token}`, `Timeout` `kube.JudgeTimeout`, token expiry from `JudgeTimeout + JudgeFetchTimeout`; `SetJudge` before `Launch`. A result with `error.code` `usage` (D3: no usable reviewer) is a judge run with no usable result. A judge provider refusal with a frozen judge chain of 2 or more marks the entry and calls `EnqueueAttempt`; with 1 entry the result is published (the CLI did the exact merge, D7). The judge may run on a reviewer's model.

- [ ] **Step 1: Failing tests:**
  - `TestE2EJudgeJob`: 3 reviewers done: one judge Job, `--multi-review --reviewer=/panel/reviewer-1.json --reviewer=/panel/reviewer-2.json --reviewer=/panel/reviewer-3.json --mr-dismissals=/in/mr-dismissals.json`, `--timeout=10m0s`, label `pergent.sh/reviewer=judge`; one `review_judges` row.
  - `TestE2EJudgeJobMRDismissals`: 2 stored dismissals: the judge ConfigMap's `mr-dismissals.json` holds exactly their fingerprint and severity; none stored: `[]`; the `--no-judge` retry Job has it too.
  - `TestE2EJudgeNoUsableReviewer`: the judge result is `status` `error`, `error.code` `usage`, `--multi-review needs at least one usable reviewer result`: one `--no-judge` Job; the same result again: published failed with that message, never `success`.
  - `TestE2EJudgeGetsPartialReviewer`: reviewer 3 is `failed` with its result kept (M3.6): get-reviewers writes `reviewer-3.json`.
  - `TestE2EJudgeJobHoldsOnlyJudgeKey` (Review Focus 5): the Secret's `llm-api-key` is the judge entry's key; no reviewer's key is in the Secret or the Job; with the judge on reviewer 2's entry, the one key is that entry's.
  - `TestE2EJudgeFailedReviewerKeepsNumber`: reviewer 2 failed with no result and reviewer 3 was skipped at the split: `--reviewer` for 1, 2 and 3; `reviewers` `{1,2,3}`; get-reviewers writes only `reviewer-1.json` (the route gives 404 for 2 and 3).
  - `TestE2EJudgeOneReviewer`: only reviewer 1 of 2 done: both `--reviewer` flags, one file, the judge still runs.
  - `TestE2EJudgeQuotaNoJudge`: repository quota used up: `--no-judge`, no key; `no_judge_reason` set.
  - `TestE2EJudgeEmptyChainNoJudge`: an empty judge copy: `--no-judge`.
  - `TestE2EJudgeFallsBack`: a judge result with `provider_error.role` `judge`, quota: the entry marked, a new judge attempt on the next frozen entry; the members untouched.
  - `TestE2EJudgeOutOfTokensNoBackup`: a 2-entry judge copy; the first judge Job is refused (quota), the second is refused too: the third judge Job runs `--no-judge` with no key.
  - `TestE2EJudgeOneEntryMarkedStillJudges`: a 1-entry judge copy whose entry another chain marked: the judge runs on it (`chain.Resolve` ignores marks on a 1-entry chain).
  - `TestE2EJudgeCrashRunsNoJudgeOnce` (Review Focus 4): the judge Job passes its deadline: the run closes `fell_back`/`judge_failed`; one new judge Job with `--no-judge` and no key; no comment yet.
  - `TestE2EJudgeNoJudgeFailsReview`: that Job fails too: the review is published as failed, never `success`, no approval, no third Job.
  - `TestE2EMultiReviewFrozenAtSplit` (Review Focus 2): after the split, reviewer 2's chain and the judge chain are replaced on the card: reviewer 2's fallback and the judge use the copies.
  - `TestE2EJudgeSkipsChecks`: a started untrusted push does not hold the judge stage.
  - `TestE2EJudgeWaitsForLiveMembers` (from `e2e_panel_rolling_test.go:45`).
  - `TestE2EMultiReviewOldStartIsSingleReview` (Review Focus 1): `startRunOld` (in `multi_review_test.go` since M3.4) queues a waiting multi review whose members run. A new pod claims it: `WaitForReviewMembers` puts it back to wait, no Job. An old pod dispatches it as one review (simulated: the run set `running` with a Job by SQL): the sweep supersedes the live members and deletes their Jobs; no judge Job.
  - `TestE2ELegacyPanelRunIsSingleReview`: a run requeued by `RequeueLegacyPanels` dispatches one review Job, is not split, and its comment has no multi review text.
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run 'E2EJudge|FrozenAtSplit|OldStartIsSingleReview|LegacyPanelRunIsSingleReview'`. Expected: FAIL.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the engine package. PASS.
- [ ] **Step 5: Commit** `feat(engine): the judge Job`

### Task M3.8: Re-runs and incremental reviews run multi review

**Files:**
- Modify: `internal/engine/multi_review.go`, `internal/engine/incremental.go`, `internal/engine/commands.go`, only for what a test below shows is missing (a re-run and an incremental review reach the split through today's dispatch; M3.4 already made an API review today's review)
- Test: `internal/engine/e2e_multi_review_test.go`

- [ ] **Step 1: Failing tests:** `TestE2ERerunIsMultiReview` (a re-run command splits again); `TestE2ERerunFailedSkillsMultiReview` (the members get the subset; the judge's result is merged with the base run as today); `TestE2EIncrementalMultiReview` (the members get `--carried`, the judge does not; the published result keeps the carried files).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run 'Rerun.*MultiReview|IncrementalMultiReview'`. Expected: FAIL.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the engine package. PASS.
- [ ] **Step 5: Commit** `feat(engine): re-runs and incremental reviews run multi review`

### Task M3.9: Pin pergent, read the result, publish, count tokens

The CLI is merged (gopaytech/pergent#54, `master` abedce0); rows D2, D5 to D9. abedce0 drops `render.PanelNames`, `SkillPanel` and `FindingPanel`, so every reader of them changes here.

**Files:**
- Modify: `go.mod`, `go.sum` (`GOFLAGS=-mod=mod go get github.com/gopaytech/pergent@abedce0b723a7a006a7f5c094a0b15a433d22b74`, `go mod tidy`)
- Modify: `internal/result/result.go:30-300,360-440` (the D6 types in place of `Panel`, `SkillPanel`, `PanelJudge`, `FindingPanel`, `ModelTokens`; `ToRender` maps the skill to `render.SkillMultiReview{Reviewers, Failed, Exact: exact_merge}` and the finding to `render.FindingMultiReview` field for field, `also` included)
- Modify: `internal/policy/policy.go:150-170,317,427-433` (`Outcome.MultiReview *render.MultiReviewNames`, `MultiReviewOffLine`; `hideInPanelText` → `hideInMultiReviewText`)
- Modify: `internal/engine/publish.go:156-161,390-510` (`reviewComment` in place of `panelComment`), `internal/engine/inline.go:134` (`o.Panel` → `o.MultiReview`: threads hide every model too), `internal/usage/usage.go:105-170` (`Rows(body, run, judge bool)`, drop `ModelRows`), `internal/usage/backfill.go:61-90`, `internal/callback/callback.go` and `member.go` (usage writes), `cmd/pergent-sh/main.go:427`
- Test: `internal/result/result_test.go`, `internal/policy/policy_test.go`, `internal/engine/e2e_multi_review_publish_test.go` (new, from `e2e_panel_publish_test.go`), `internal/usage/usage_test.go`

**Interfaces:**

```go
// MultiReviewOffLine is the footer line of a review whose multi review did
// not start.
func MultiReviewOffLine(reason string) string { return "multi review off: " + reason }

// reviewComment reads what run's comment needs: render.MultiReviewNames
// with every reviewer's name in --reviewer order (review_judges.reviewers;
// the frozen entry of its newest attempt with a result, else its chain's
// first entry), the
// judge's name ("" with no judge) and Skipped, one per reviewer with no
// result (fixed words, Choices); every member's and the judge's model to
// hide; or the off line of a review that runs as one.
func (e *Engine) reviewComment(ctx context.Context, run store.Run) (reviewComment, error)
```

Only the judge run is published, through today's `publish`. A review with no member row and no off row gets nothing: today's comment.

- [ ] **Guard (passes before and after):** `TestCommentUnchanged` with no golden edit.
- [ ] **Step 1: Failing tests:** `TestToRenderMultiReviewFields` (every D6 field; `exact_merge` gives `Exact`; an omitted `failed`; `also` reaches render, so a dismissal of an `also` fingerprint dismisses the merged finding) and `TestToRenderNoMultiReviewUnchanged`; `TestPublishMultiReviewOnce` (one comment with the header `Multi review: <e1>, <e2>, <e3> · judge <ej>`, no provider or model text, `success` for a clean result, the approval on the judge run only); `TestPublishSkippedLine` (`Skipped` holds `{<e2>, the token quota}` and the comment shows render's `Multi review: <e2> was skipped: the token quota.` once); `TestPublishOffLine` (`multi review off: the review image does not support it`); `TestCommentHidesEveryModel` (each reviewer's and the judge's provider and model in a finding's text and in its thread are hidden); `TestE2EMultiReviewUsage` (`run_usage` of the review holds reviewers + judge per skill; `quota.Check` sees all of it; part reviewer and part judge rows); `TestBackfillReviewMembers` (a lost usage write is filled at start).
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/result/ ./internal/policy/ ./internal/usage/ ./internal/engine/ -run 'MultiReview|Skipped|OffLine|HidesEvery|CommentUnchanged|BackfillReview'`. Expected: FAIL.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the 4 packages, then the full suite. PASS.
- [ ] **Step 5: Commit** `feat(engine): publish a multi review and count every model's tokens`

---

## Phase M4: run page, panel removal and docs

### Task M4.1: The run page

**Files:**
- Modify: `internal/dashboard/view.go:249-307` (`RunDetailView.MultiReview *RunMultiReviewView` in place of `Panel`), `internal/dashboard/api_read.go:73-75,667,823-940` (`multiReviewOf`, `logOwner` and the member log and diagnostics reads on `ReviewMembers`; drop `panelMembers`, `panelOf`, `RunPanelView`, `PanelMemberView`, `PanelJudgeView`, so nothing here reads the panel store calls M4.2 deletes), `openapi.yaml`, `internal/dashboard/openapi_test.go`
- Modify: `web/src/pages/ReviewDetail.tsx:436-600`, `web/src/api.ts`
- Test: `internal/dashboard/api_read_test.go`, `web/src/pages/ReviewDetail.test.tsx`

```go
// RunMultiReviewView is a multi review on its run's page: why it did not
// start (Off), or one row per reviewer, the judge row and the total. The
// merged findings are the run's own result; raw reviewer findings are only
// in the member logs.
type RunMultiReviewView struct {
	Off       string         `json:"off,omitempty"`
	Reviewers []ReviewerView `json:"reviewers"`
	Judge     *JudgeView     `json:"judge"`
	Total     TokensView     `json:"total"`
}
type ReviewerView struct {
	Number   int                 `json:"number"`
	Entry    string              `json:"entry"` // from the frozen copy
	Provider string              `json:"provider"`
	Model    string              `json:"model"`
	State    string              `json:"state"`   // the final attempt's
	Skipped  string              `json:"skipped"` // the reason, "" when it has a result
	Findings int                 `json:"findings"`
	In       int64               `json:"in"`
	Out      int64               `json:"out"`
	Attempts []MemberAttemptView `json:"attempts"` // each with its log link by ID
}
type JudgeView struct {
	Entry    string `json:"entry"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	NoJudge  bool   `json:"noJudge"`
	Reason   string `json:"reason"`
	Attempts int    `json:"attempts"`
	In       int64  `json:"in"`
	Out      int64  `json:"out"`
}
```

Tokens come from `ReviewCosts`; when they can't be read the page shows none and logs a warning, as today.

- [ ] **Step 1: Failing tests:** `TestRunPageMultiReview` (3 rows with entry names, states, findings counts, tokens and attempts; the judge row; the total is the sum; no key); `TestRunPageMultiReviewOff`; `TestRunPageNoMultiReview` (no `multiReview` key); `TestReviewMemberLogGated` (a user who can't see the run: 404; a member of another review: 404); the OpenAPI tests (`-run OpenAPI`); web `ReviewDetail.test.tsx` "shows one row per reviewer, the judge and the total" and "shows why multi review was off".
- [ ] **Step 2: Run to fail.** `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/dashboard/ -run 'RunPage|ReviewMemberLog|OpenAPI'`, then `cd web && npx vitest run src/pages/ReviewDetail.test.tsx`. Expected: FAIL.
- [ ] **Step 3: Code.**
- [ ] **Step 4: Run** the dashboard package and the web gate. PASS.
- [ ] **Step 5: Commit** `feat(dashboard): a multi review on its run page`

### Task M4.2: Remove the panel's code

Runs after M4.1: the run page no longer reads the panel store calls.

**Files:**
- Delete: `internal/store/panel.go`, `panel_test.go`, `panel_chains_test.go`, `usage_panel_test.go`; `internal/engine/panel.go`, `panel_test.go`, any `e2e_panel_*_test.go` left; `internal/callback/panel.go`, `panel_test.go`; `internal/kube/panel.go` (`JudgeTimeout` and `JudgeFetchTimeout` in `judge.go` take its values; the volume and fetch code it shares moves to `judge.go`)
- Modify: `internal/store/chains.go` (drop `ChainB`, `PanelInherit`/`PanelCustom`/`PanelOff`, `RepoPanelMode`, `SetRepoPanelOff`, `setRepoPanelMode`, `ErrEmptyRepoPanelChain`), `internal/store/dashboard.go` (`DashboardRepo.PanelChainMode` and its scan), `internal/store/usage.go` (`RecordMemberUsage`, `recordMergeUsage`, `ModelUsage`, `ReviewModelCosts`, `MembersWithoutUsage`, `memberUsageRunSQL`; `panelNoResult` keeps its `panel_members` clause for legacy rows), `internal/store/runs.go` (`WaitPanel` stays as a constant for `RequeueLegacyPanels` only; `CountRunning` drops `panel_members`), `internal/store/apiclients.go:260` (drop the `panel_members` Job query), `internal/chain/load.go` (drop `LoadSet`; `Load` reads chain A), `internal/kube/job.go` (`Panel`, `PanelMemberID`, `PanelRole`, `LLMKeyB`, `keyLLMKeyB`), `internal/kube/launcher.go:36-40,240` (`HasKeyB`, `JobSecrets`' `llm-api-key-b`), `cmd/pergent-sh/graph.go` (`get-panel`), `internal/server/server.go` (`Deps.Panel`, the route)
- Test: the whole suite

- [ ] **Step 1:** `git grep -nE 'PanelMember|ChainB|panel_chains|panel_judges|run_usage_models|LLMKeyB|get-panel|PanelParams|memberUsageRunSQL' -- '*.go'` prints only `legacy_panel.go`, the migration test files and `WaitPanel`.
- [ ] **Step 2:** `go vet ./...` and the full DB suite. PASS; the 3 goldens unchanged.
- [ ] **Step 3: Commit** `refactor: remove the 2-model review panel`

### Task M4.3: Docs, upgrade notes and the floor's comment

**Files:** `docs/concepts/review-panel.md` → `git mv` to `docs/concepts/multi-review.md` (status `experimental` until measured on real merge requests; the card; 1 reviewer is today's; the Jobs; skips; a reviewer that failed partway still gives its blocking findings; the judge and its failure path; the comment lines, a skipped reviewer's skip note and per-skill note included; freeze at the split; changes outside the card; known limit: a dismissal made while the judge Job runs is applied by the comment but was not known to the judge's links, so in a rare case it can hide a finding the judge linked to it), `docs/concepts/models.md`, `how-it-works.md`, `job-isolation.md` (a reviewer Job holds its own key; the judge Job only the judge's, none with `--no-judge`; the judge Job gets the merge request's dismissals, fingerprint and severity only), `usage.md`, `token-quota.md` (every Job counts), `dashboard.md`, `docs/setup/review-api.md` (never multi review), `docs/reference/http-routes.md`, `docs/README.md`, `openapi.yaml` descriptions, `internal/config/config.go:1409-1412` (comment only: `PanelMinVersion is the first pergent release with agent --multi-review. "" until that release exists: every image is refused. The owner sets it with defaultImage.`), `docs/upgrading.md` `## Unreleased`.

`## Unreleased` replaces the "Review panels are available" block: multi review, experimental, off until a pergent release with `--multi-review` is the floor; migration `0050` adds 8 tables and drops nothing; the 0048 and 0049 tables are unused; **breaking for the experimental API:** the `/panel-chain` routes, the repository view's `panel` and the run view's `panel` are gone (`/reviewers`, `multiReview`); data from a main build is not converted (one warning at start; a run waiting on `panel_members` runs as one review). Rolling deploy: v0.18.0 never claims a `review_members` run, but its start button can start one as a single review. Rollback: the old release sees reviewer 1 only; a waiting run stays until a push replaces it.

- [ ] **Step 1:** Write the pages; every exact string matches the code (`git grep` each quoted string).
- [ ] **Step 2:** Phase gate (Global Constraints).
- [ ] **Step 3: Commit** `docs: multi review`

## Self-review notes

- **Spec coverage.** Words and goals: 1 reviewer byte for byte (M3.4 `TestDispatchJobUnchanged`, the 3 goldens), recall and the judge (M3.7). Where and the card: repository only, chain B gone (M2.1-M2.3, M4.2), rights (M2.1), entries rules, same first model refused (M2.1), cost hint (M2.3), judge from reviewer 2's first entry and refusal without one (M2.1, M2.3), remove and renumber (M1.2, M2.3), image check on add (M2.1). Limits 5 and 5 (M1.1, M1.2, M2.1). Dispatch rows: 1 reviewer, 2 to 5 in parallel with verify off (M3.4, M3.5), same-model skip (M1.3, M3.4, M3.5), fallback per reviewer (M3.6), skipped reviewer and its comment line (M3.4, M3.6, M3.9), only reviewer 1 (M3.4), every reviewer failed (M1.4, M3.6), quota (M3.4, M3.7, M3.9), older image (M3.4), API never (M3.4), re-run (M3.8). Freeze (M1.3, M1.4, M3.7). Changes outside the card (M1.2, M2.1 warnings, M3.6). Judge Job: one judge per skill is the CLI's; fallback on quota, billing, auth (M3.7); 10 min (M3.1); no reviewer usable (D3, M3.7); out of tokens and quota (M3.5, M3.7); crash once then fail (M3.7); links never touch a dismissed finding (D11, M3.1, M3.7). CLI interface (M3.1, Depends). Data tables and old tables (M1.1, M3.3, M4.2). Dashboard: repository line (M2.1, M2.3), run page (M4.1), API (M2.1, M2.2), audit (M2.1). Rollout and rolling deploy (M3.3, M4.3). Experimental (M2.3, M4.3).
- **Kept guarantees.** Keys (M3.1, M3.5, M3.7); no false pass (M3.6, M3.7); members never publish (M3.6); judge only when all are final (M1.4, M3.7); base and inputs at the split (M1.4, M3.5); usage on the split run (M1.5, M3.9).
- **Type names** across tasks were checked against each Interfaces block.
- **Not here:** the pergent CLI, the floor's value, any release step, `defaultImage`, dropping the 0048 and 0049 tables (a later release), the judge prompt and its input limit (CLI).
