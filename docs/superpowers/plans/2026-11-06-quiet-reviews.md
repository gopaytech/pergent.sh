# Quiet Reviews Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Implementers run on **Sonnet**. Task reviews and the final branch review (`superpowers:requesting-code-review`) run on **Opus**.

**Goal:** A repository can turn on Quiet reviews: fewer nitpicks, no new suggestion threads and a shorter comment, with blocking findings, failures, verdicts and approval unchanged.

**Architecture:** One new `render.Options.Short` field in pergent shortens the comment. In pergent.sh, `quiet_reviews` resolves like the settings beside it, is read at dispatch and recorded on the run. The prompt line, the thread rule and the comment then all follow the run's value, so a republish keeps the run's shape.

**Tech Stack:** Go, Postgres migrations, React with Vite and vitest.

**Spec:** `docs/superpowers/specs/2026-11-06-quiet-reviews-design.md`. Read its anti-goals first. They are the failure bar.

## Prerequisite (not a task of this plan)

pergent v0.5.4 and pergent.sh 0.8.1 with the `CheckSkill` fix are released and confirmed on the cluster. Do not start before that.

## Hand-offs

Implementers have no network, so these steps belong to the owner's main session, not to a subagent.

| When | Who | What |
|---|---|---|
| **H0**, before Task 1 | Main session | `git fetch` both repos. Check `grep 'gopaytech/pergent ' go.mod` on `origin/main` shows v0.5.4 or later, and `defaultImage` matches it. Check `ls internal/store/migrations \| tail -1` shows `0024`; if 0.8.1 took `0025`, renumber this plan's migration. Create the worktree: `git worktree add ~/Documents/personal/github/pergent.sh-quiet -b feat/quiet-reviews origin/main`. |
| **H1**, after Task 1 | Owner | Merge the pergent branch and tag the next pergent release. Tasks 2 to 5 go on meanwhile: they don't need it. |
| **H2 (STOP)**, before Task 6 | Owner, then main session | Execution stops here until the owner names the released pergent tag and the pergent.sh version for the Upgrading note. Never assume either. Then the main session, in the pergent.sh worktree, runs `go get github.com/gopaytech/pergent@<tag>` and `go mod tidy`, sets `defaultImage` in `internal/config/config.go` and the assertion in `internal/config/config_test.go` to `ghcr.io/gopaytech/pergent:<tag>`, runs the Go gate and commits `chore(deps): pergent <tag>` by itself. The pin and the image move together, so the commit also shows what that release changes in pergent.sh's tests. |
| **H3**, after the final review | Owner | Push the branch and open the PR. Postgres-backed tests first run here. Read the `test` job log and check the `internal/store`, `internal/dashboard` and `internal/engine` timings: a fast `ok` means they skipped. |

## Global Constraints

- Never run docker, orbctl or anything that starts a container. No gh, glab, curl or other network step in a task. No push, tag or PR. No disk or cache cleanup.
- Tests marked **[CI]** need Postgres. Write them with the same care, confirm they compile with `go vet ./...`, and expect CI's run to be their first. Report them as not run.
- **Go gate** (pergent.sh): `go vet ./...` then `go test ./...`. Store, dashboard and most engine tests skip here, so this proves only the tests with no database.
- **Web gate:** `cd web && npm run typecheck && npm run lint && npx vitest run && npm run build`. Not while `go test` runs: vitest times out under load.
- **Private-name scan** before every commit, in each repo, must print nothing: `git grep -niE 'gol[a]bs|goj[e]k|gop[a]y[^t]|got[o]\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'`
- Commit with the owner's email as the agent definition says, staged by path, ending with a `Co-Authored-By:` line that names the model that wrote the change. Gate the commit on its checks with `&&`.
- `render` lives in pergent. Only Task 6 may use `render.Options.Short`.
- Off changes nothing: the same prompt, threads and comment, byte for byte. Never edit an existing test or golden file to make a change pass, except to add a new field to a fixture.
- The README is the behaviour contract: change it in the same commit as the behaviour.
- Names: config key `quiet_reviews`, dashboard key `quietReviews`, migration `0025`, label and meta note `quiet`.
- The prompt line, word for word:

> When you raise a suggestion of your own, raise it only if it would change how the code behaves or clearly helps the next person who reads it, and leave out style, naming, formatting and other nitpicks. This applies to nothing else: it does not change what is blocking, how you report or judge a checklist item, or how you check findings another reviewer raised.

## Review Focus

1. **A size-limit shrink on a quiet comment.** Every step keeps quiet's layout. No note claims a cut for what quiet left out, and blocking findings and failures survive every step. (Task 1)
2. **A run with NULL after the migration.** A run from before `0025`, published again after the upgrade, reads as off, loads without a scan error and keeps its long comment. (Tasks 2 and 6)
3. **A dismissal republish after a setting change.** The comment and the threads follow the run, in both directions. The Dismissed list stays and the thread says "Dismissed by". (Tasks 5 and 6)
4. **An incremental review merging a carried result from the other setting.** The meta line reads ``Re-reviewed 2 files · 28 carried over from `abc1234` · quiet``, a carried suggestion stays listed, and its open thread stays. (Tasks 5 and 6)
5. **A blocking finding, failed check or failure on a quiet review.** The line reaches the Job in full, last sentence included, so verification gets it. Status, description, approval and kind match an off review, and a blocking finding still gets a thread. (Tasks 4, 5 and 6)

---

### Task 1: `render.Options.Short`

**Repo:** pergent, `~/Documents/personal/github/pergent`. **Branch:** `feat/render-short` off `origin/master` (at or after v0.5.4).

**Files:**
- Modify: `render/render.go` (`Options`, the `Comment` doc comment, the `writeChecks` call in `render`), `render/details.go` (`writeChecks`, `writeFiles`), `CLAUDE.md` (the `render/` line: add `Short` and `MetaNote` to its Options list)
- Create: `render/short_test.go` (package `render_test`)

**Interfaces:**
- Produces: `render.Options.Short bool`. It leaves out the Files list and the passed and n/a checks, with no note. Everything else is as without it. Task 6 sets it.

- [ ] **Step 1: Write the failing tests** in `render/short_test.go`. Use a fixture with one blocking finding, one suggestion, one dismissed finding, one failure, passed, n/a and failed checks, and two changed files. Don't add a `.json` file to `testdata/golden`: `TestCommentMatchesOutputGoldens` runs every one it finds.
  - `TestShortLeavesOutFilesAndPassedChecks`: no `<summary>Files (`, no `<summary>Checks:`, no name of a passed or n/a check.
  - `TestShortRemovesNothingElse`: the `Short` comment equals the off comment with exactly the `<details><summary>Checks: … </details>\n\n` and `<details><summary>Files (… </details>\n\n` blocks cut out, hidden block included.
  - `TestShortKeepsTheMetaLine`: `2 files` and `N of M checks passed` are still there.
  - `TestShortKeepsWhatMatters`: `**Failed checks (1)**`, the failure line, both findings, `Dismissed (1)` and `<!-- pergent:findings ` are all there.
  - `TestShortAllChecksPassed`: all checks pass, so no checks block and no note, and the meta line keeps its count.
  - `TestShortNotesAtEverySizeLimit` (Review Focus 1), for inputs `checks-big`, `checks-multi`, `big`, `failures` and the fixture, sweeping `maxLen` from 200 to the full length in steps of 97:
    ```go
    got := render.Comment(in.Skills, render.Options{MaxLen: maxLen, ChangedFiles: in.ChangedFiles, Short: true})
    // never: "Passed and n/a checks omitted", "not listed to fit", "<summary>Files (", "<summary>Checks:"
    // always: len(got) <= maxLen
    ```
  - `TestShortNoTablesNoteWithoutFailedChecks`: only passed checks, a limit that forces the last steps: no `Check tables omitted`.
  - `TestShortTablesNoteWhenAFailedTableIsCut`: failed checks and a tight limit: `Check tables omitted` is there and the meta line keeps its count. The cut is real, so its note stays.
  - `TestShortKeepsBlockingAndFailuresAtEveryStep`: on the sweep, any comment without `Comment truncated` holds the blocking title and the failure line.

- [ ] **Step 2: Run** `go test ./render -run Short`. Expected: a compile failure on `Short`.

- [ ] **Step 3: Implement.** Add `Short bool` to `Options` with a doc comment. Change the signature to `writeChecks(b *strings.Builder, skills []Skill, o Options, l layout)`. With `o.Short`, write neither the collapsed block nor `passedChecksDroppedNote`, and write `checkTablesDroppedNote` only when `len(failed) > 0`. `writeFiles` returns at once with `o.Short`. The shrink list in `Comment` stays as it is: its file and passed-check steps change nothing under `Short`. Update the `Comment` doc comment and `CLAUDE.md`.

- [ ] **Step 4: Verify.** Run `make test`, `make vet`, `go test ./render/...`. Expected: PASS. `git status --short render/testdata` prints nothing: the goldens pin off. The `cmd` agent tests need git 2.40 or later; if the local git is older, report that and rely on CI.

- [ ] **Step 5: Run the scan and commit.** Stage `render/render.go render/details.go render/short_test.go CLAUDE.md`. Message: `feat(render): add Options.Short, a comment without files and passed checks`.

**Then H1.** pergent.sh tasks don't wait for it, but Task 6 does.

---

### Task 2: The setting: migration, config, resolver, store

**Repo:** pergent.sh. **Worktree:** `~/Documents/personal/github/pergent.sh-quiet`. **Branch:** `feat/quiet-reviews`, with `go.mod` at pergent v0.5.4 or later. Tasks 3 to 6 use the same worktree and branch.

**Files:**
- Create: `internal/store/migrations/0025_quiet_reviews.sql`
- Modify: `internal/config/config.go`, `internal/settings/settings.go`, `internal/store/dashboard.go`, `internal/store/instances.go`, `internal/store/runs.go`, `README.md`
- Test: `internal/config/config_test.go`, `internal/settings/settings_test.go`, `internal/store/dashboard_test.go`, `internal/store/instances_test.go`, `internal/store/rerun_test.go`

**Interfaces:**
- Produces:
  - `config.Repository.QuietReviews *bool` (`yaml:"quiet_reviews"`)
  - `settings.KeyQuietReviews = "quiet_reviews"`, `settings.Effective.QuietReviews bool`
  - `store.DashboardRepo.QuietReviews *bool`, `store.RepoDefaults.QuietReviews *bool`
  - `store.Run.Quiet bool`, false for NULL
  - `func (s *Store) SetRunQuiet(ctx context.Context, runID string, quiet bool) error`

- [ ] **Step 1: Write the failing tests.**
  - `TestQuietReviewsConfigKey` (config): `quiet_reviews: true` parses to `*true`; unset is nil.
  - `TestResolverQuietReviewsConfig` (settings, no database): unset gives false, `Overridden[KeyQuietReviews] == false`; `true` gives true, overridden.
  - `TestResolverConfigIgnoresStoredQuietReviews`: `r.defaultsFor(SourceConfig, …, store.RepoDefaults{QuietReviews: &yes}).QuietReviews == false`.
  - `TestResolverQuietReviewsDashboard`: a table through `r.fromDashboard` of (row, stored default) to (value, overridden): (nil, nil) false, false; (nil, true) true, false; (false, true) false, true; (true, nil) true, true.
  - **[CI]** `TestDashboardRepoQuietReviews`, `TestRepoDefaultsQuietReviews`: nil, true and false round-trip through insert, update and upsert.
  - **[CI]** `TestRunQuietNullReadsOff` (Review Focus 2): an enqueued run with no `SetRunQuiet` has `Quiet == false` and no scan error through `Run`, `ClaimQueued` and `RunsToPublish`.
  - **[CI]** `TestSetRunQuiet`: true then false each read back; on a `reported` run it returns an error.

- [ ] **Step 2: Run** `go test ./internal/config ./internal/settings`. Expected: a compile failure on the new fields.

- [ ] **Step 3: Implement.** The migration:
  ```sql
  -- Whether a review says less. NULL is the default (off).
  ALTER TABLE dashboard_repos ADD COLUMN quiet_reviews boolean;
  ALTER TABLE repo_defaults   ADD COLUMN quiet_reviews boolean;
  -- Whether a run was quiet, read when it started. NULL is off: a run from
  -- before this column, or one that never started.
  ALTER TABLE runs ADD COLUMN quiet_reviews boolean;
  ```
  Follow `IncrementalReviews` through `config`, `settings` (key in the `Overridden` map, stored default inside the `SourceDashboard` part of `defaultsFor`, `fromConfig`, `fromDashboard`) and every SQL list in `dashboard.go` and `instances.go`. The built-in default is false. In `runs.go`, add `quiet_reviews` to `runColumns` and scan it into a local `*bool`: a plain `bool` fails on NULL and breaks every older run. `SetRunQuiet` uses `mustUpdate` with `WHERE id = $1::uuid AND state IN ('queued', 'running')`, like `SetIncremental`.

- [ ] **Step 4: Update `README.md`.** Add the **Quiet reviews** bullet to Features, a new section `## 🤫 Quiet reviews` after Learn from dismissals with the purpose and **Turning it on** (off by default; **Quiet reviews** in Settings, the default under **Admin → Settings**, `repositories[].quiet_reviews`; a config repository reads no admin default; it takes effect with the next review). Add the `repositories[].quiet_reviews` row (default `false`) to the Config reference and `quiet_reviews` to the list of config keys in the Dashboard section.

- [ ] **Step 5: Verify.** Run the Go gate. Expected: PASS, with **[CI]** tests compiled and skipped.

- [ ] **Step 6: Run the scan and commit.** Message: `feat(settings): add quiet_reviews, recorded on the run`.

---

### Task 3: Dashboard API, controls and the `quiet` label

**Files:**
- Modify: `internal/dashboard/view.go`, `api_read.go`, `api_write.go`, `api_operator.go`; `web/src/api.ts`, `web/src/test/fixtures.ts`, `web/src/pages/RepositorySettings.tsx`, `AdminSettings.tsx`, `ReviewDetail.tsx`; `README.md`
- Test: `internal/dashboard/api_write_test.go`, `api_operator_test.go`, `api_read_test.go`; `web/src/pages/RepositoryDetail.test.tsx`, `AdminSettings.test.tsx`, `ReviewDetail.test.tsx`

**Interfaces:**
- Consumes: Task 2's `Effective.QuietReviews`, `KeyQuietReviews`, `DashboardRepo.QuietReviews`, `RepoDefaults.QuietReviews`, `Run.Quiet`.
- Produces: `settings.quietReviews` in the repository view, `quietReviews` in the defaults views, `RunDetailView.Quiet bool` (`json:"quiet"`).

- [ ] **Step 1: Write the failing tests.** All Go ones here are **[CI]**.
  - `TestQuietReviewsSetting`, shaped like `TestIncrementalReviewsSetting`: true is stored and overridden; `"x"` is 400 with `quietReviews must be true, false or null` and leaves the row; null clears to the default, off; reset clears it; `msgBadSettings` names the key. Also: a maintainer may change it; a `settings_updated` audit entry has detail `quietReviews`; a config repository's write gets 409.
  - `TestDefaultsQuietReviews`, like `TestDefaultsIncrementalReviews`: saved, `pinned`, `defaults_updated` detail names `quietReviews`, `msgBadDefaults` names it, and a dashboard repository with nothing set follows it.
  - `TestSettingViewsQuietReviews`: default `{false, false}`.
  - `TestRunDetailQuiet`: `quiet` is true for a run with `SetRunQuiet(true)`, false for a run left NULL.
  - vitest, RepositoryDetail: the switch comes right after **Learn from dismissals** and sends only `{quietReviews: true}`.
  - vitest, AdminSettings: the switch comes after **Learn from dismissals**, sends a change, nulls it when it equals the config, and shows `From config: off`; a pinned one offers **Use config value**.
  - vitest, ReviewDetail: `{...runDetail, quiet: true}` shows `quiet` in the "This run" region; the plain fixture doesn't.

- [ ] **Step 2: Run** `cd web && npx vitest run src/pages/ReviewDetail.test.tsx`. Expected: FAIL. `go vet ./internal/dashboard` fails on `Quiet`.

- [ ] **Step 3: Implement the API.** Add `quietReviews` everywhere `learnFromDismissals` is: `boolSettings`, both `switch` lists, `resetSettings`, `msgBadSettings`, `msgBadDefaults`, `DefaultsView`, `DefaultValues`, the `pinned` map, `changedDefaults`, the row-to-defaults conversion, the settings map in `api_read.go`, the doc comments in `view.go`. Set `Quiet: run.Quiet` in the `RunDetailView` literal. No role check of its own: it follows the other switches.

- [ ] **Step 4: Implement the web side.** Both controls go after **Learn from dismissals**, named **Quiet reviews**, with this help text: `Fewer nitpicks, no suggestion threads and a shorter comment. Blocking findings, failures and approval stay the same.` In `Facts` of `ReviewDetail.tsx`, under the incremental note: `{run.quiet && <div><Pill tone="grey">quiet</Pill></div>}`. Add `quiet: boolean` to the run detail type and `quiet: false` to the fixture. The page keeps showing checks and files.

- [ ] **Step 5: Update `README.md`.** Add **Quiet reviews** (off by default) to the repository Settings list, the Admin Settings list and the Defaults paragraph. Add the `quiet` label to the review page sentence, and say the page still shows checks and files.

- [ ] **Step 6: Verify.** Run the Go gate, then the web gate. Expected: PASS.

- [ ] **Step 7: Run the scan and commit.** Message: `feat(dashboard): Quiet reviews switch, default and run label`.

---

### Task 4: The prompt line and recording the run

**Files:**
- Modify: `internal/inputs/inputs.go`, `internal/engine/dispatch.go`, `README.md`, `CLAUDE.md`
- Test: `internal/inputs/inputs_test.go`, `internal/kube/job_test.go`, `internal/engine/dispatch_test.go`

**Interfaces:**
- Consumes: `Effective.QuietReviews`, `Store.SetRunQuiet`.
- Produces: `func (in *Inputs) AddQuietLine()`, which appends `"\n# Quiet review\n\n" + line + "\n"` to `in.Instructions` and touches nothing else.

- [ ] **Step 1: Write the failing tests.** Hard-code the line as a literal in the test, copied from Global Constraints, not from the constant.
  - `TestAddQuietLineAfterTeamText`: on `LoadInstructions` output with `AGENTS.md` and `.pergent/instructions.md`, the result is the old bytes followed by exactly `"\n# Quiet review\n\n" + line + "\n"`.
  - `TestAddQuietLineWithNoInstructions`: from nil, `in.Instructions` is exactly that section.
  - `TestAddQuietLineCountsInSize`: `Size()` grows by the section's length; skills that fit `ConfigMapLimit` without it and don't with it make `Fit` return `ErrTooLarge`.
  - `TestAddQuietLineLeavesSkills`: `Skills` are unchanged, so a skill's hash is too.
  - `TestQuietLineKeepsItsLastSentence` (Review Focus 5): the section ends with `how you check findings another reviewer raised.\n`.
  - `TestBuildJobInstructionsOnly` (kube): inputs with only `Instructions` set give `--instructions=/in/instructions.md`, the volume item and the ConfigMap key.
  - **[CI]** `TestDispatchQuietAddsTheLine`: a quiet repository with no instructions: the launched ConfigMap's `instructions.md` is the section, the args carry `--instructions=/in/instructions.md`, and the run reads `Quiet == true`.
  - **[CI]** `TestDispatchQuietOffChangesNothing`: no `instructions.md`, no flag, `Quiet == false`. With team instructions, the bytes equal `LoadInstructions`.
  - **[CI]** `TestDispatchQuietOverBudgetFails`: the line tips it over: the run fails with `reasonTooLarge`.
  - **[CI]** `TestDispatchQuietIsReadAtEachDispatch`: Launch fails with the setting on, the setting goes off, the next dispatch has no line and `Quiet == false`.

- [ ] **Step 2: Run** `go test ./internal/inputs ./internal/kube`. Expected: a compile failure on `AddQuietLine`. If `TestBuildJobInstructionsOnly` passes at once, that is fine: it pins what the spec relies on.

- [ ] **Step 3: Implement** `AddQuietLine` with the line and heading as unexported constants. Build a new slice: `Only` shares `Instructions`.

- [ ] **Step 4: Wire `dispatch`.** Right before `in.Fit`, so the line counts toward the budget, add `if repo.QuietReviews { in.AddQuietLine() }`. Right after the `setIncremental` block and before minting the token, call `SetRunQuiet(ctx, run.ID, repo.QuietReviews)`, on or off, and `d.retry` on error. **Not** in `SetRunInputs`: that write is best effort and comes after the Launch, so a lost write would give a quiet prompt with a long comment.

- [ ] **Step 5: Update docs.** README, Quiet reviews section: the line word for word, that it comes last and wins for suggestions, that a repository with no instructions gets a file holding only this, that it counts toward the 900 KiB, that it isn't in a skill's hash, and that the **Skills & instructions** tab and the readiness budget don't show it. `CLAUDE.md`, Rules: `A run's quiet value is read at dispatch and stored on the run. The prompt, threads and comment use the run's value, never the repository's.`

- [ ] **Step 6: Verify.** Run the Go gate. Expected: PASS.

- [ ] **Step 7: Run the scan and commit.** Message: `feat(engine): add the quiet line to a review's instructions`.

---

### Task 5: Threads

**Files:**
- Modify: `internal/threads/threads.go`, `internal/engine/inline.go`, `README.md`
- Test: `internal/threads/threads_test.go`, `internal/engine/inline_test.go`

**Interfaces:**
- Consumes: `store.Run.Quiet`.
- Produces: `threads.Input.Quiet bool`, `threads.NoteKeptQuiet`, `func ReplyNoLongerReported(head string) string`.
- Exact values: `NoteKeptQuiet = "Kept here: quiet reviews don't post suggestions on the diff."`; the reply is ``No longer reported in `abc1234`.`` built with `ShortSHA`.

- [ ] **Step 1: Write the failing tests.** All run with no database. Don't edit `TestOnlyBlockingKeepsSuggestions` or `TestGoneFindingFixed`: they pin off.
  - `TestQuietKeepsSuggestions`: `in.Quiet = true`, one blocking and one suggestion on the diff: only the blocking one is in `Post`; `Notes["s"] == NoteKeptQuiet`; `Inline["s"]` is false.
  - `TestQuietNoteBeatsTheBranchRule`: `Quiet` and `OnlyBlocking` together give `NoteKeptQuiet`, also for a resolved suggestion thread.
  - `TestQuietKeepsAnOpenSuggestionThread` (Review Focus 4): an open thread whose suggestion is still reported stays in `Inline`, with no `Post` and no `Resolve`.
  - `TestQuietDoesNotReopenASuggestion`: a `StateFixed` thread with a suggestion gets no `Reopen` and `NoteKeptQuiet`; with a blocking finding it reopens with ``Back in `abcdef1`.``
  - `TestQuietGoneFindingNoLongerReported`: `Resolve{State: StateFixed, Reply: "No longer reported in `abcdef1`."}`.
  - `TestQuietLeavesOtherReplies`: file left the change: `ReplyGone`; left-out note: `ReplyLeftOut`, stays open; failed skill: thread kept.
  - `TestQuietDismissedThreadSaysDismissed` (Review Focus 3): the reply is `Dismissed by …`, state `StateDismissed`.
  - `TestQuietBlockingFindingStillPosts` (Review Focus 5): a blocking finding on the diff is in `Post`, with its suggestion block.
  - `TestInlineQuietKeepsSuggestions` (engine): set `h.quiet = true` (a new harness field that `h.run()` copies to `Quiet`): `v.Inline == {fpBlocking: true}`, `v.Notes[fpSuggestion] == threads.NoteKeptQuiet`, one thread posted.
  - `TestInlineQuietResolveReply` (engine): the reply sent on the platform is ``No longer reported in `<short head>`.`` and the row's state is `fixed`.
  - `TestInlineOffRunKeepsSuggestionThreads` (Review Focus 3): `h.quiet` false posts both findings, and a gone finding says ``Fixed in``.

- [ ] **Step 2: Run** `go test ./internal/threads ./internal/engine -run 'Quiet|OffRun'`. Expected: a compile failure on `Quiet`.

- [ ] **Step 3: Implement.** Add `Quiet bool` to `Input` (a quiet review, read from the run). In `Decide`, set `onlyBlocking := in.OnlyBlocking || in.Quiet` and `kept := NoteKeptRule`, or `NoteKeptQuiet` with `in.Quiet`; use them at all three `OnlyBlocking` and `NoteKeptRule` sites. In the `cov.Reviewed` resolve case, use `ReplyNoLongerReported` with `in.Quiet`. Keep `StateFixed`, so a blocking finding that comes back reopens. In `runInline`, pass `Quiet: run.Quiet`, never the repository's value.

- [ ] **Step 4: Update `README.md`.** Inline findings: add the quiet reply beside ``Fixed in `<sha>`.`` and "kept because the review is quiet" to the reasons under **Not on the diff**. Quiet reviews section: no new suggestion threads, open ones stay, every thread that goes away says ``No longer reported in `<sha>`.``, blocking ones too, and a suggestion loses its one-click fix.

- [ ] **Step 5: Verify.** Run the Go gate. Expected: PASS.

- [ ] **Step 6: Run the scan and commit.** Message: `feat(threads): a quiet review posts no suggestion threads`.

**STOP: H2.** Do not start Task 6 until the owner has named the tag and the main session has committed the pin and image bump. Confirm with `grep 'gopaytech/pergent ' go.mod` and `grep defaultImage internal/config/config.go`, and that the new module has `render.Options.Short`.

---

### Task 6: The comment

**Files:**
- Create: `internal/policy/testdata/quiet-off.md`
- Modify: `internal/policy/policy.go`, `internal/engine/publish.go`, `README.md`
- Test: `internal/policy/policy_test.go`, `internal/engine/publish_test.go`, `internal/engine/e2e_dashboard_test.go`

**Interfaces:**
- Consumes: `render.Options.Short` from Task 1, `store.Run.Quiet`.
- Produces: `policy.Outcome.Quiet bool`, `func metaNote(i *result.Incremental, quiet bool) string`.

- [ ] **Step 1: Pin off first.** Before touching `policy.go`, write `TestCommentOffMatchesBeforeQuiet`: a completed result with blocking and suggestion findings, passed, n/a and failed checks, reviewed and skipped files, and a mention, rendered with an `Outcome` that doesn't name `Quiet`, compared with `testdata/quiet-off.md`. Add a test flag `-update-quiet-off`, generate the golden once on the H2 commit, and never regenerate it.

- [ ] **Step 2: Write the failing tests.**
  - `TestRenderOptionsQuiet`: `Quiet: true` gives `Short: true`, `MetaNote: "quiet"`. With `Incremental{Reviewed: 2, Carried: 28, From: …}`, `MetaNote` is ``Re-reviewed 2 files · 28 carried over from `abc1234` · quiet``. Off, `DeepEqual` to the options `TestRenderOptionsInline` expects, and `MetaNote` is exactly `result.IncrementalNote(r.Incremental)`.
  - `TestDecideQuietComment`: no `<summary>Files (`, no `<summary>Checks:`, no `omitted to fit`; has `· quiet</sub>`, `N of M checks passed`, the failed table, both findings and `<!-- pergent:findings `.
  - `TestDecideQuietChangesOnlyTheComment` (Review Focus 5): for blocking, clean, failed skill, nothing reviewed, partly skipped, draft, approval off and merge note, `Status`, `Description`, `Approve` and `Kind` equal the off decision.
  - `TestDecideQuietKeepsTheSkippedNote`: a skip that prevents approval still says so.
  - `TestDecideQuietWithDismissal` (Review Focus 3): a dismissed blocking finding: same decision as off, `Dismissed (1)` present, no Files list.
  - `TestDecideQuietCarriedResult` (Review Focus 4): a merged result with a carried suggestion, `Quiet: true`: the suggestion is listed, the meta line holds the combined note.
  - `TestDecideQuietErrorResult`: an error result's comment is the same on and off.
  - `TestDecideQuietAtTheCommentLimit` (Review Focus 1): a small `MaxComment`: within the limit, blocking title kept, no `Passed and n/a checks omitted`.
  - `TestOutcomeCarriesTheRunsQuiet` (engine, no database): `e.outcome(store.Run{Quiet: true, …})` has `Quiet` true.
  - **[CI]** `TestPublishNullQuietRunIsOff` (Review Focus 2): a reported run left NULL gets the Files list and no `quiet`.
  - **[CI]** `TestPublishQuietRunAfterSettingOff`, `TestPublishOffRunAfterSettingOn` (Review Focus 3): the comment and threads follow the run in each direction.
  - **[CI]** `TestDismissRepublishKeepsTheRunsShape` (Review Focus 3), in the dashboard e2e: a quiet run, the setting turned off, a dismissal: the republished comment has no Files list, says `quiet` and lists the dismissal; the thread says `Dismissed by`.
  - **[CI]** `TestPublishQuietMergesACarriedResult` (Review Focus 4): an off base run with a suggestion and an incremental quiet run: the merged comment has the combined note and the carried suggestion, and the run's `Quiet` survives `mergeIncremental`.

- [ ] **Step 3: Run** `go test ./internal/policy`. Expected: a compile failure on `Quiet`, and `TestCommentOffMatchesBeforeQuiet` passing once that is fixed.

- [ ] **Step 4: Implement.** Add `Quiet bool` to `Outcome` with a doc comment and to the type's comment. In `renderOptions`: `opts.Short = o.Quiet`, `opts.MetaNote = metaNote(r.Incremental, o.Quiet)`. `metaNote` joins the non-empty `result.IncrementalNote(i)` and `"quiet"` with `" · "`. In `Engine.outcome`, the last return becomes `policy.Outcome{Result: r, Quiet: run.Quiet}`. The failure paths don't need it: they render no layout. `result` and `render` get no quiet field.

- [ ] **Step 5: Update `README.md`.** Quiet reviews section: what the comment leaves out and keeps, the `quiet` meta note, the size-limit steps, the accepted costs, the first-use advice, and how it works with inline findings off and with incremental reviews. Add `### Upgrading to <version>`, with the version the owner named at H2, at the top of Upgrading:
  - Quiet reviews are off until turned on, so nothing changes for a repository that doesn't.
  - The default `jobs.image` is `ghcr.io/gopaytech/pergent:<tag>`, and what that release changes in the comment.
  - Migration `0025` runs at startup. Three nullable columns, and the previous release keeps working on the migrated database.
  - To roll back, first remove `quiet_reviews` from the config, because the previous release refuses unknown keys.

- [ ] **Step 6: Verify.** Run the Go gate. Expected: PASS. `git status --short internal/policy/testdata` shows only the file added in this task.

- [ ] **Step 7: Run the scan and commit.** Message: `feat(policy): a shorter comment for a quiet review`.

---

## Finish

- [ ] **Final review.** Run `superpowers:requesting-code-review` on Opus over the whole branch against the spec's anti-goals and the Review Focus. The reviewer starts no container and calls no external service.
- [ ] **Report.** List every **[CI]** test as written but not run, then go to H3. Release steps are the owner's call and are not part of this plan.
