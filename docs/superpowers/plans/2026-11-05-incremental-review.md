# Incremental Re-reviews Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** After a push, review only the files changed since the last review and carry the rest over, so a re-review takes time in proportion to what changed.

**Architecture:** pergent.sh decides per skill which files carry over and hands pergent a list (`--carried`). pergent leaves those files out and reports them as `files.carried`. pergent.sh merges the partial result with the last review before publishing, the way `review failed` already merges (`mergeBase`), so everything after that step sees one whole result.

**Tech Stack:** Go, PostgreSQL (pgx), Kubernetes Jobs, React + TypeScript (vitest).

**Spec:** `docs/superpowers/specs/2026-11-05-incremental-review-design.md`

## Global Constraints

- Public repositories: example names only. Scan the diff for private names before every commit.
- Never push, tag or open a PR. The controller asks the user first.
- Stage files by path; `.superpowers/` is not ignored.
- Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Anything uncertain falls back to a full review. A failure of this feature never fails a review.
- A partial result is never published as if it were the whole review.
- pergent's CI mode is unchanged.
- There is no local Postgres. Tests that need `PERGENT_SH_TEST_DATABASE_URL` skip here and first run in CI, so desk-check them. Don't start Docker or Orbstack.
- Comment text: `Re-reviewed 2 files · 28 carried over from `abc1234``.
- Setting names: config `repositories[].incremental_reviews`, settings key `incremental_reviews`, JSON `incrementalReviews`, label **Incremental reviews**, on by default.

## Review Focus

1. **The last review's run is pruned between dispatch and publish.** `base_run_id` is `ON DELETE SET NULL`, so `BaseRunID` alone can't mark a run incremental. Expected: the run restarts as a full review and the partial is never published (Task 7).
2. **`@bot review` replaces a live run of the same head.** The cleanup sets a `canceled` status on `run.HeadSHA`, the same commit the new run reports on. Expected: no `canceled` status for that head (Task 4).
3. **A push where every skill carries everything.** Expected: the Job starts no opencode run, and the merged verdict, findings and approval equal the last review's (Task 7).
4. **A dismissed blocker on an unchanged file.** Expected: the file is carried, the finding stays dismissed, its thread is untouched (Tasks 5, 6).
5. **A push superseded before its review.** Pushes A (published), B (superseded), C. Expected: C compares from A, not B (Task 6).

---

# Part 1: pergent

Work in a new worktree `../pergent-incremental`, branch `feat/carried-files`, from `origin/master`.

### Task 1: `--carried` agent input

**Files:**
- Modify: `cmd/agent.go`, `internal/review/review.go`, `internal/result/result.go`
- Modify: `docs/agent-result-schema.md`, `README.md`, `CLAUDE.md` (Agent mode)
- Test: `cmd/agent_test.go`, `internal/review/review_test.go`, `internal/result/result_test.go`

**Interfaces:**
- Produces: flag `--carried=<path>`, a JSON file `{"<skill>": ["path", ...]}`; `review.Request.Carried map[string][]string`; `review.SkillResult.Carried []string`; `result.Files.Carried []string` with JSON `carried,omitempty`.

- [ ] **Step 1: Write the failing tests**

In `internal/review/review_test.go`:
- `TestCarriedFilesLeftOut`: two changed files, `Carried: {"s": {"b.go"}}`. The batch's `diff.patch` holds `a.go` only; `changed-files.md` lists both; `res.Carried == ["b.go"]`; `res.Reviewed == ["a.go"]`; the message contains `carriedMessage`.
- `TestAllCarriedStartsNoRun`: every file carried. The fake runner is never called; `res.Verdict == NoBlockingIssues`; `res.Reviewed` is empty; `res.Notes` doesn't contain `noFilesNote`.
- `TestCarriedPathOutOfScopeIgnored`: a carried path not in the skill's selected files isn't in `res.Carried`.
- `TestPreviousDropsCarriedFindings`: `previous-findings.json` holds the finding on `a.go` and not the one on `b.go`.

In `cmd/agent_test.go`:
- `--carried` inside `--repo-path` is a usage error, like `--previous`.
- An unreadable or malformed file logs a warning and carries nothing.

In `internal/result/result_test.go`: `files.carried` is omitted when empty and present when set.

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./cmd ./internal/review ./internal/result`
Expected: FAIL (`Carried` undefined).

- [ ] **Step 3: Implement**

- `cmd/agent.go`: flag `carried`, `carriedPath` and `Carried` on `agentInputs`, a `{"--carried", path}` entry in `trustedInputsOutside`, `readCarried(path string, logger *log.Logger) map[string][]string` beside `readPrevious` (same warn-and-ignore behaviour), and `Carried: in.Carried` on `review.Request`.
- `internal/review/review.go`: in `reviewSkill`, after `Select`, split `in` into reviewed and carried. The `len(in) == 0` branch stays for no files in scope; a new branch returns `NoBlockingIssues` with `res.Carried` set when only carried files remain. Filter `prev` to findings not on carried files. Add to the batch message, when the skill has carried files:

```go
carriedMessage = "The other files of this change were reviewed earlier and are not in this diff. Read them if this change affects them."
```

- `internal/result/result.go`: `Files.Carried`, filled and redacted like `Reviewed`.
- Docs: `files.carried` in the schema table ("Paths the caller's previous review covers; not reviewed in this run"), the flag in the README, one line in `CLAUDE.md`.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go vet ./... && go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/agent.go cmd/agent_test.go internal/review/review.go internal/review/review_test.go internal/result/result.go internal/result/result_test.go docs/agent-result-schema.md README.md CLAUDE.md
git commit -m "feat(agent): --carried leaves files a previous review covers out of the review"
```

### Task 2: `render.Options.MetaNote`

**Files:**
- Modify: `render/render.go`, `render/layout.go`
- Test: `render/layout_test.go`

**Interfaces:**
- Produces: `render.Options.MetaNote string`, one more part at the end of the meta line, as given (the caller formats it).

- [ ] **Step 1: Write the failing test** `TestMetaNote`: with `MetaNote: "Re-reviewed 2 files · 28 carried over from `abc1234`"` the meta line contains that text after the counts; with it empty the output equals today's.

- [ ] **Step 2: Run it to see it fail.** Run: `go test ./render -run TestMetaNote`. Expected: FAIL.

- [ ] **Step 3: Implement** in `writeMeta`. The golden files don't change.

- [ ] **Step 4: Run** `go test ./...`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add render/render.go render/layout.go render/layout_test.go
git commit -m "feat(render): a caller's note on the meta line"
```

**Gate:** Part 1 ships as pergent v0.5.0. Tell the user it's ready and ask to push, open the PR and tag. Tasks 3–7 don't need the tag; Task 8 does.

---

# Part 2: pergent.sh

Work in `../pergent.sh-incremental`, branch `feat/incremental-review`.

### Task 3: The Incremental reviews setting

**Files:** the same files as commit `64ab832`, which added `commands_author_and_maintainers_only`: `internal/config/config.go`, `internal/settings/settings.go`, `internal/store/instances.go`, `internal/store/dashboard.go`, `internal/dashboard/{api_operator,api_read,api_write,view}.go`, `web/src/api.ts`, `web/src/pages/{AdminSettings,RepositorySettings}.tsx`, `web/src/test/fixtures.ts`, and their tests.
- Create: `internal/store/migrations/0020_incremental_reviews.sql`

**Interfaces:**
- Produces: `settings.Effective.IncrementalReviews bool`, `settings.KeyIncrementalReviews`.

- [ ] **Step 1: Write the failing tests**, mirroring `64ab832`'s: on by default, config override, dashboard override, Admin default, reset, and the two pages. Help text: "After a push, review only the files changed since the last review."

- [ ] **Step 2: Run them to see them fail.** Run: `go test ./internal/... && (cd web && npx vitest run)`. Expected: FAIL.

- [ ] **Step 3: Implement**, following `64ab832` line by line. The migration:

```sql
-- Whether a push reviews only the files changed since the last review.
-- NULL is the default (on).
ALTER TABLE dashboard_repos ADD COLUMN incremental_reviews boolean;
ALTER TABLE repo_defaults   ADD COLUMN incremental_reviews boolean;
```

- [ ] **Step 4: Run** the same commands. Expected: PASS (store tests skip).

- [ ] **Step 5: Commit** those files: `feat: the "Incremental reviews" setting`.

### Task 4: Run records, changed files, and full reviews replacing incremental ones

**Files:**
- Modify: `internal/store/migrations/0020_incremental_reviews.sql`, `internal/store/runs.go`
- Modify: `internal/forge/forge.go`, `internal/forge/githubforge/inline.go`, `internal/forge/gitlabforge/inline.go`, `internal/gitlab/client.go`
- Modify: `internal/engine/publish.go` (`cleanUp`), `internal/engine/dispatch.go` (the `SetRunInputs` call)
- Test: `internal/store/rerun_test.go`, `internal/gitlab/client_test.go`, `internal/forge/*/inline_test.go`, `internal/engine/publish_test.go`

**Interfaces:**
- Produces:
  - `store.Run.Incremental bool`, `store.Run.SkillHashes map[string]string`
  - `(*Store).LastReview(ctx, mr MRKey, beforeRunID string) (Run, bool, error)`: the run `PreviousResult` finds. `PreviousResult` goes.
  - `(*Store).SetIncremental(ctx, runID string, baseRunID *string) error`: for a queued or running run, sets `base_run_id`, and `incremental` to whether it's non-nil.
  - `(*Store).SetRunInputs(ctx, id, model, maxTurns, sources, versions, hashes map[string]string) error`
  - `(*Store).NewerRunOfHead(ctx, run Run) (bool, error)`: a run of the same MR, head and target created after it and not superseded.
  - `forge.Inline.ChangedSince(ctx, projectID int64, from, to string) ([]string, error)`: the paths (`Path` and `OldPath`) changed from `from` to `to`. An error when `from` isn't an ancestor of `to` or the list may be partial.
  - `(*gitlab.Client).MergeBase(ctx, projectID int64, a, b string) (string, error)`: `GET /projects/:id/repository/merge_base?refs[]=a&refs[]=b`, the commit's `id`.

- [ ] **Step 1: Write the failing tests**
  - `TestLastReview`: returns the newest published, completed run, with its head and skill hashes. A superseded newer run is passed over.
  - `TestSetIncremental`: sets and clears; a reported run is an error.
  - `TestEnqueueRerunReplacesIncrementalRun`: for each of a queued push run, and a running or reported run with `incremental`: a `TriggerRerun` gives `created == true` and supersedes it. A running non-incremental run gives `created == false`. A `TriggerRerunFailed` gives `created == false`.
  - `TestChangedSince` on both forges: GitLab with a merge base other than `from` is an error; a rename returns both paths.
  - `TestCleanUpKeepsStatusOfANewerRunOfTheHead`: no `canceled` status is set, and the run is still marked cleaned up. **(Review Focus 2)**

- [ ] **Step 2: Run them to see them fail.** Run: `go test ./internal/...`. Expected: build failure.

- [ ] **Step 3: Implement**
  - Append to the migration:

```sql
-- incremental marks a run that reviews only some files. It can't be read from
-- base_run_id, which is set to NULL when the earlier run is pruned.
ALTER TABLE runs ADD COLUMN incremental boolean NOT NULL DEFAULT false;
-- The hash of each skill's content, by name.
ALTER TABLE runs ADD COLUMN skill_hashes jsonb;
```

  - `EnqueueRerun`: when `trigger == TriggerRerun`, the same-head lookup returns only a live run that is not `trigger = 'push' AND (state = 'queued' OR incremental)`. The update after it already supersedes what is left.
  - `cleanUp`: skip the `canceled` status when `NewerRunOfHead` says so.
  - `dispatch.go`: pass `nil` hashes for now.

- [ ] **Step 4: Run** `go vet ./... && go test ./...`. Expected: PASS (store tests skip; desk-check them).

- [ ] **Step 5: Commit** those files: `feat: record incremental runs and let a full review replace them`.

### Task 5: Merging a partial result

**Files:**
- Modify: `internal/result/result.go`
- Test: `internal/result/result_test.go`

**Interfaces:**
- Produces:
  - `result.Files.Carried []string` (`carried,omitempty`)
  - `type Incremental struct { Reviewed int `json:"reviewed"`; Carried int `json:"carried"`; From string `json:"from"` }`, `Result.Incremental *Incremental` (`incremental,omitempty`)
  - `result.MergeIncremental(partial, base []byte, from string) ([]byte, error)`
  - `result.IncrementalNote(i *Incremental) string` (`""` for nil)
  - `(*Result).SupportsCarried() bool`: `PergentVersion` (with or without `v`) is at least 0.5.0; anything unparsable is false.

- [ ] **Step 1: Write the failing tests**, one per row of the spec's merge table:
  - `TestMergeIncrementalFindings`: new findings plus the base's on `files.carried`; a base finding on a file neither reviewed nor carried is gone; IDs are assigned again.
  - `TestMergeIncrementalFiles`: `reviewed` is new plus carried; `skipped`, `not_reviewed` and `failures` are the new run's.
  - `TestMergeIncrementalVerdict`: `failed` if the new run failed; a carried blocking finding gives `has_blocking_issues`; otherwise `no_blocking_issues`.
  - `TestMergeIncrementalChecks`: the new run's check replaces the same section and name; others stay; batches are numbered 1..n.
  - `TestMergeIncrementalSkillDidNotRun`: a skill with `batches == 0` keeps the base's confidence, reason, summaries, notes, batch counts and checks; one that ran takes the new run's. **(Review Focus 3)**
  - `TestMergeIncrementalDismissedBlockerKept`: a carried blocking finding keeps its fingerprint, so a dismissal still matches. **(Review Focus 4)**
  - `TestMergeIncrementalCounts`: `Incremental` counts distinct paths; a file reviewed by one skill and carried by another counts as reviewed; `merged` is true.
  - `TestMergeIncrementalRefuses`: an error for a base that isn't completed, a base whose `head` isn't `from`, a partial that isn't completed.
  - `TestIncrementalNote`: `{2, 28, "abc1234def"}` gives `Re-reviewed 2 files · 28 carried over from `abc1234``; `{1, 1, ...}` gives `Re-reviewed 1 file · 1 carried over ...`.
  - `TestSupportsCarried`: `0.5.0`, `v0.5.0`, `0.10.0`, `1.0.0` are true; `0.4.9`, `dev`, `""` are false.

- [ ] **Step 2: Run them to see them fail.** Run: `go test ./internal/result`. Expected: FAIL.

- [ ] **Step 3: Implement** beside `Merge`, keeping each skill's unknown fields as `Merge` does (raw JSON). A skill "ran" when `batches > 0`. Reuse `assignIDs`. The envelope is partial's.

- [ ] **Step 4: Run** `go test ./internal/result`. Expected: PASS.

- [ ] **Step 5: Commit** `internal/result/result.go internal/result/result_test.go`: `feat(result): merge a partial review with the last one`.

### Task 6: Deciding at dispatch

**Files:**
- Create: `internal/engine/incremental.go`, `internal/engine/incremental_test.go`
- Modify: `internal/engine/dispatch.go`, `internal/inputs/inputs.go`, `internal/kube/job.go`
- Test: `internal/inputs/inputs_test.go`, `internal/kube/job_test.go`, `internal/engine/dispatch_test.go`

**Interfaces:**
- Consumes: `LastReview`, `SetIncremental`, `SetRunInputs`, `ChangedSince` (Task 4); `SupportsCarried` (Task 5); `settings.Effective.IncrementalReviews` (Task 3).
- Produces:
  - `inputs.Inputs.Carried []byte`, counted by `Size`; `Fit` drops it together with `Previous`.
  - `kube`: with `Carried`, the arg `--carried=/in/carried.json` and ConfigMap key `carried.json`, as for `previous.json`.
  - `skillHashes(skills map[string][]byte) map[string]string`: hex SHA-256.
  - `carriedFiles(last *result.Result, lastHashes, hashes map[string]string, changed []string, ds []render.Dismissal) map[string][]string`

- [ ] **Step 1: Write the failing tests**

`carriedFiles`, table-driven, all without a database:
  - A reviewed, unchanged file is carried; a changed one is not.
  - A file in `not_reviewed` is not carried.
  - A file with an open blocking finding is not carried; a suggestion's file is. **A dismissed blocker's file is carried. (Review Focus 4)**
  - A skill whose hash differs, or is missing on either side, has no entry.
  - A skill with a finding on a file outside its `files.reviewed` has no entry, even if the finding is dismissed.
  - A skill with nothing to carry has no entry.

Dispatch, `TestPushDispatch` subtests (DB-backed). Each asserts the Job's args and the run's `Incremental`:
  - Incremental: last review at an ancestor. `--carried` is present, `BaseRunID` is the last run.
  - Full, no `--carried`, for each of: setting off; `TriggerRerun`; no last review; target branch changed; same head; `ChangedSince` errors; `pergent_version` 0.4.0; last result has a `merge_note`; nothing to carry; `Fit` dropped the inputs.
  - Each full case of a push logs `full review` with its reason.
  - `TestIncrementalComparesFromLastPublished`: A published, B superseded, C dispatched: `ChangedSince` got `from == A`. **(Review Focus 5)**

- [ ] **Step 2: Run them to see them fail.** Run: `go test ./internal/engine ./internal/inputs ./internal/kube`. Expected: build failure.

- [ ] **Step 3: Implement**
  - `dispatch`: replace `PreviousResult` with `LastReview`, keeping `previous` as `result.Trim(last.Result)`.
  - After `subset` and before `Fit`, for a push: decide, and set `in.Carried` to the JSON of `carriedFiles`.
  - After `Fit`, call `SetIncremental` with the last run's ID, or `nil` if `in.Carried` is empty. Always call it, so a requeued run is decided afresh. An error is `d.retry`.
  - Pass `skillHashes(in.Skills)` to `SetRunInputs`. For a run with `keptSources`, add the base's hashes, as for versions.
  - Dismissals come from `e.dismissals`. Its error is `d.retry`.

- [ ] **Step 4: Run** `go vet ./... && go test ./...`. Expected: PASS (DB tests skip; desk-check them).

- [ ] **Step 5: Commit** those files: `feat: a push reviews only what changed since the last review`.

### Task 7: Merging at publish

**Files:**
- Modify: `internal/engine/publish.go`
- Test: `internal/engine/publish_test.go`, `internal/engine/e2e_test.go`

**Interfaces:**
- Consumes: `MergeIncremental` (Task 5), `EnqueueRerun` (Task 4).

- [ ] **Step 1: Write the failing tests** (DB-backed)
  - `TestIncrementalMergedBeforePublish`: the stored result is merged, carried findings are in the comment, and the next run's `LastReview` sees the whole.
  - `TestIncrementalBaseGoneRestartsFull`: `BaseRunID` nil or the base's result unreadable: nothing is posted, no status is set, the run is superseded, and a `TriggerRerun` run is queued. **(Review Focus 1)**
  - `TestIncrementalNothingToReview`: a partial with every file carried gives the last review's verdict, status and approval. **(Review Focus 3)**
  - `TestIncrementalKeepsThreads`: a carried finding's thread gets no "Fixed" reply and stays open. **(Review Focus 4)**
  - `TestIncrementalErrorResultPublishedAsIs`: a partial with `status: error` is published as an error, as today.

- [ ] **Step 2: Run them.** Run: `go test ./internal/engine`. Expected: they skip here, so check by reading.

- [ ] **Step 3: Implement** in `mergeBase`: an incremental run goes to a new `mergeIncremental(ctx, run, log) (store.Run, bool, error)`. The bool means restarted: `publish` then returns nil without posting. It skips a result that is not completed or is already `Merged`. On any merge failure it logs a warning, calls `EnqueueRerun(..., store.TriggerRerun, nil, nil)` and `e.Wake()`.

- [ ] **Step 4: Run** `go vet ./... && go test ./...`. Expected: PASS.

- [ ] **Step 5: Commit** those files: `feat: merge an incremental review before publishing`.

### Task 8: What people see, and the release pieces

Needs pergent v0.5.0 tagged.

**Files:**
- Modify: `go.mod`, `go.sum`, `internal/config/config.go` (`defaultImage`)
- Modify: `internal/policy/policy.go`, `internal/dashboard/view.go`, `internal/dashboard/api_read.go`, `web/src/api.ts`, `web/src/pages/ReviewDetail.tsx`, `web/src/test/fixtures.ts`
- Modify: `internal/engine/commands.go` (`replyHelp`), `README.md`
- Test: `internal/policy/policy_test.go`, `internal/dashboard/api_read_test.go`, `web/src/pages/ReviewDetail.test.tsx`, `internal/engine/commands_test.go`

- [ ] **Step 1: Write the failing tests**
  - Policy: a result with `Incremental` has `Re-reviewed 2 files · 28 carried over from `abc1234`` in the comment; a full review has no such text.
  - Dashboard: the run view's `incrementalNote` holds the same text; the page shows it, and shows nothing for a full review.
  - `replyHelp` says `` `@bot review` (full review) ``.

- [ ] **Step 2: Run them to see them fail.** Run: `go test ./internal/... && (cd web && npx vitest run)`. Expected: FAIL.

- [ ] **Step 3: Implement**
  - `go get github.com/gopaytech/pergent@v0.5.0`, and `defaultImage` to `ghcr.io/gopaytech/pergent:v0.5.0`.
  - `renderOptions`: `opts.MetaNote = result.IncrementalNote(r.Incremental)`.
  - View: `IncrementalNote string` with JSON `incrementalNote`.
  - README: the setting, in config reference and Settings; a Commands line that `review` is a full review; and "Upgrading to 0.7.0" with the spec's Upgrade points. State plainly what's given up: a break in an untouched file can go unnoticed until a full review, and teams that rebase get no speed-up yet.

- [ ] **Step 4: Run** `go vet ./... && go test ./... && (cd web && npx vitest run)`. Expected: PASS.

- [ ] **Step 5: Commit** those files: `feat: show what a re-review covered; pergent v0.5.0`.
