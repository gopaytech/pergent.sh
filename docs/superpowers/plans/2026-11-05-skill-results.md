# Skill Results Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a change merges or closes, record one final fate for each finding, and show per skill what became of its findings, so a maintainer knows which skills to fix or drop.

**Architecture:** A pure `internal/fates` package walks a change's decided reviews and returns fates. The webhook records that a change needs working out, the engine's loop does it with a capped number of compares, and a **Skill results** tab reads the stored rows. Nothing touches reviews, comments, verdicts or approval.

**Tech Stack:** Go, Postgres (pgx, SQL migrations), React + TanStack Query.

**Spec:** `docs/superpowers/specs/2026-11-05-skill-results-design.md`

## Global Constraints

- pergent.sh only, branch `docs/skill-results`.
- Public repository: no private hostnames, company names or internal systems in code, tests, docs or commit messages.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Stage by path. `.superpowers/` stays out. Never push.
- Never run `docker`, `orbctl`, or anything that starts a container or a Postgres server. Never call an external API or a platform (`gh`, `glab`, `curl`). DB tests skip locally and first run in CI, so desk-check every SQL column list, placeholder count and scan order, and read any test that seeds rows a change could make unrealistic.
- Disk is nearly full: don't delete or clean anything, and stop and report BLOCKED if it runs out.
- A failure here never touches a review or a webhook's answer.
- Unknown is never a fix: when in doubt a finding is **Went away**, never **Fixed**.
- Values: 3 lines of reach; 20 compares per change; 5 attempts; percentage from 10 findings; windows of 30 (default) and 90 days; rows kept a year.
- Fates are `dismissed`, `open`, `fixed`, `went_away`.

## Review Focus

1. A re-run on the same head where a finding vanishes: no code moved, so **Went away**, with no compare (Task 2).
2. A finding that vanishes and later returns: one row, and it ends as reported (Task 2).
3. A force-push on GitHub is permanent, so **Went away**; a 429 retries the whole change and stores nothing (Tasks 2, 5).
4. A redelivered `closed` after a merge never recomputes (Tasks 3, 5).
5. Closed, reopened, then merged: the merge's fates replace the close's, and the rates count them (Tasks 3, 5).

---

### Task 1: Which lines really changed

**Files:**
- Modify: `internal/diffmap/diffmap.go`
- Test: `internal/diffmap/diffmap_test.go`

**Interfaces:**
- Produces: `func (f File) ChangedNear(lo, hi, reach int) bool`: whether a line was added, removed or changed within `reach` lines of the old-side range `lo` to `hi`. Context lines in a hunk don't count.

- [ ] **Step 1: Write the failing tests**
  - `TestChangedNearOnTheLine`: a patch changing old line 10: `ChangedNear(10, 10, 0)` is true.
  - `TestChangedNearIgnoresContext`: a change at old line 10 with context on 7–13: `ChangedNear(13, 13, 2)` is false, `ChangedNear(13, 13, 3)` is true, `ChangedNear(20, 20, 3)` is false.
  - `TestChangedNearAddedLine`: a guard added before old line 10, with nothing removed: `ChangedNear(10, 10, 0)` is true, as an insertion sits at the line it comes before.
  - `TestChangedNearRemovedLine`: old line 10 deleted: `ChangedNear(10, 10, 0)` is true.
  - `TestChangedNearRange`: a change at 15, `ChangedNear(10, 12, 3)` is true, `ChangedNear(10, 11, 3)` is false.
  - `TestChangedNearNoHunks`: an empty patch is false.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/diffmap/ -run ChangedNear`
Expected: FAIL, undefined `ChangedNear`.

- [ ] **Step 3: Implement.** `Hunk` gains `Changed []int`, old-side line numbers: the number of each `-` line, and for each `+` line the old line it comes before. Fill it in `Parse`. Update the package comment.

- [ ] **Step 4: Run** `go test ./internal/diffmap/`. Expected: PASS.

- [ ] **Step 5: Commit** with `feat(diffmap): tell which old lines really changed`.

### Task 2: The walk

**Files:**
- Create: `internal/fates/fates.go`
- Test: `internal/fates/fates_test.go`

Pure, with no database and no platform: this is where the feature's correctness lives, and these tests are the ones that run locally.

**Interfaces:**
- Consumes: `diffmap.File.ChangedNear`, `MapOld`.
- Produces:

```go
const (Dismissed = "dismissed"; Open = "open"; Fixed = "fixed"; WentAway = "went_away")
const (Reach = 3; MaxCompares = 20)

type Finding struct { Fingerprint, File string; Line, EndLine int; Severity string }
type Skill struct {
	Name, Hash string
	Failed     bool            // its verdict is "failed"
	Reviewed   map[string]bool // files it reviewed, carried ones too
	LeftOut    bool            // it has a "Left out because of past dismissals:" note
	Findings   []Finding
}
type Review struct { Head string; Skills []Skill }
type Dismissal struct { Fingerprint, Severity string }
// Compare gives the diff between two heads, by the file's path on the new side.
// ErrGone means it can never be had; any other error is worth trying again.
type Compare func(from, to string) ([]diffmap.File, error)
var ErrGone = errors.New("the comparison can't be had")
type Input struct { Reviews []Review; Dismissals []Dismissal; Compare Compare }
type Fate struct { Fingerprint, Skill, SkillHash, Severity, Fate string }
func Decide(in Input) ([]Fate, error) // an error other than ErrGone from Compare is returned, with no fates
```

The algorithm, which the spec's "Working it out" section argues for. Keep one track per finding, walking `Reviews` oldest first. For each review, for each live track of a skill:
1. Skip the review for this track if the skill isn't in it, `Failed`, or doesn't have the track's file in `Reviewed`. The track stays reported.
2. Same fingerprint reported: update head, line and severity (highest wins).
3. Otherwise, if the heads differ, look for a reworded finding: same skill, in the file (following `OldPath`), no track yet, its range overlapping the track's range mapped through `MapOld`. When the old line can't be mapped, the finding must be within `Reach`. If found, it's the same track, with the new fingerprint.
4. Otherwise it has vanished: on the same head, or with `LeftOut`, it's `WentAway`; otherwise compare, and it's `Fixed` if the file was deleted, or if `ChangedNear(line, end, Reach)`, or if it has no line and the file is in the diff; else `WentAway`. `ErrGone`, or past `MaxCompares`: `WentAway`.

A finding not tracked starts a track. A vanished track's fingerprint reported again goes live. At the end a live track is `Open`. A track covered by a dismissal is `Dismissed`, whatever else: covered means the same fingerprint, and the dismissal's severity is `blocking` or the finding's highest severity is `suggestion`, the same as `render`'s `covering`. Cache compares per pair of heads. Sort the result by fingerprint.

- [ ] **Step 1: Write the failing tests** in `internal/fates/fates_test.go`, each a table row or a named test with a fake `Compare` that counts calls:
  - `TestReportedUntilTheEndIsOpen`
  - `TestLinesChangedIsFixed` and `TestLinesUntouchedWentAway`
  - `TestNearbyEditIsFixed`: a change 3 lines off is `Fixed`, 4 lines off `WentAway`.
  - `TestDeletedFileIsFixed`, `TestNoLineAnyFileChangeIsFixed`
  - `TestSameHeadVanishedWentAwayNoCompare`: 0 compares. (Review Focus 1)
  - `TestVanishedAndBackIsOneRow`: reported, gone, reported: one `Fate`, `Open`. (Review Focus 2)
  - `TestFailedSkillIsSkipped`, `TestRemovedSkillIsSkipped`, `TestFileNotReviewedIsSkipped`: the finding stays `Open`.
  - `TestLeftOutIsWentAwayEvenIfLinesChanged`
  - `TestRewordedIsTheSameFinding`: one row, under the newest fingerprint, `Open`.
  - `TestRewordedOnAnotherFileIsNew`, `TestRewordedAcrossARename`
  - `TestDismissedWins`: dismissed and also fixed gives `Dismissed`.
  - `TestSuggestionDismissalDoesNotCoverABlocker`: gives `Open`.
  - `TestHighestSeverityCounts`
  - `TestOneFingerprintOnTwoLinesIsOneRow`
  - `TestErrGoneIsWentAway`, and `TestOtherCompareErrorReturnsNoFates`. (Review Focus 3)
  - `TestComparesAreCapped`: 21 pairs: 20 compares, the rest `WentAway`.
  - `TestComparesAreCached`: two findings over one pair of heads: 1 compare.
  - `TestNoReviews`: no fates.

- [ ] **Step 2: Run them to verify they fail.** Run: `go test ./internal/fates/`. Expected: FAIL to build.

- [ ] **Step 3: Implement** `Decide`, as above.

- [ ] **Step 4: Run** `go test ./internal/fates/`. Expected: PASS.

- [ ] **Step 5: Commit** with `feat: work out what became of a change's findings`.

### Task 3: Storing fates and the work to do

**Files:**
- Create: `internal/store/migrations/0024_skill_results.sql`, `internal/store/fates.go`
- Modify: `internal/store/projects.go` (`DeleteProject`), `internal/store/instances.go` (its delete list)
- Test: `internal/store/fates_test.go`

**Interfaces:**
- Consumes: `fates.Fate` is not imported here; the store has its own row.
- Produces, in `internal/store`:

```go
type FindingFate struct { Fingerprint, Skill, SkillHash, Severity, Fate string }
type FateJob struct { MR MRKey; ProjectPath string; Merged bool; RequestedAt time.Time; Attempts int }
type SkillResult struct {
	Skill, Severity string
	Raised, Fixed, Dismissed, Open, WentAway int
	// EditedAt: when the row's newest skill hash first appears, if the window holds an older one.
	EditedAt *time.Time
}
func (s *Store) RequestFates(ctx, mr MRKey, projectPath string, merged bool, at time.Time) error
func (s *Store) ClaimFateJobs(ctx, now time.Time, limit int) ([]FateJob, error)
func (s *Store) FateJobFailed(ctx, mr MRKey, notBefore time.Time, attempts int) error // attempts >= 5 stops it
func (s *Store) DropFateJob(ctx, mr MRKey) error
func (s *Store) ReplaceFates(ctx, mr MRKey, projectPath string, rows []FindingFate, merged, byBot bool, at time.Time) error
func (s *Store) DecidedRuns(ctx, mr MRKey) ([]Run, error) // outcome <> '', oldest first
func (s *Store) ChangesWithoutFates(ctx, since time.Time, limit int) ([]FateJob, error)
func (s *Store) SkillResults(ctx, instance string, projectID int64, since time.Time) ([]SkillResult, error)
func (s *Store) PruneFates(ctx, before time.Time) (int, error)
```

`finding_fates`: `instance`, `project_id`, `mr_iid`, `fingerprint`, `skill`, `skill_hash`, `severity`, `fate` (CHECK), `merged`, `by_bot`, `decided_at`; primary key `instance`, `project_id`, `mr_iid`, `fingerprint`, `skill`; an index on `(instance, project_id, decided_at)`. `fate_jobs`: `instance`, `project_id`, `mr_iid`, `project_path`, `merged`, `requested_at`, `attempts`, `not_before`, `finished_at`; primary key the first three.

Rules:
- `RequestFates` inserts, or on conflict resets the job, unless the job finished for a merged change. So a redelivered `closed` does nothing to a merged change. (Review Focus 4)
- `ReplaceFates` deletes the change's rows and inserts `rows` in one transaction, and marks the job finished. (Review Focus 5)
- `SkillResults` counts rows with `merged AND NOT by_bot`, plus `fate = 'dismissed'` on rows of closed changes by people, by `decided_at >= since`, per skill and severity.
- Claiming sets `attempts` only on failure. A job with `attempts >= 5` or `finished_at` set isn't claimed.

- [ ] **Step 1: Write the failing tests** (DB-gated, so desk-check them): `TestReplaceFatesReplaces`, `TestRequestFatesAfterAMergeDoesNothing`, `TestRequestFatesAfterACloseResets`, `TestClaimFateJobsSkipsStoppedAndFutureOnes`, `TestDecidedRunsIncludesSupersededRuns`, `TestDecidedRunsLeavesOutUndecidedOnes`, `TestSkillResultsCountsMergedPeople`, `TestSkillResultsDismissedOnClosedCounts`, `TestSkillResultsLeavesOutBots`, `TestSkillResultsEditedAt`, `TestChangesWithoutFates`, `TestPruneFates`, `TestDeleteProjectRemovesFates`.

- [ ] **Step 2: Verify they fail.** Run: `go vet ./internal/store/`. Expected: undefined names.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run** `go vet ./... && go test ./...`. Expected: PASS, DB tests skipped. Desk-check each statement.

- [ ] **Step 5: Commit** with `feat(store): store findings' fates and the changes to work out`.

### Task 4: Webhooks say a change ended

**Files:**
- Modify: `internal/webhook/gitlab.go`, `internal/webhook/github.go`
- Test: `internal/webhook/*_test.go`, `internal/webhook/testdata/github_closed.json`

**Interfaces:**
- Consumes: `store.RequestFates`.
- Produces: `Queue` gains `RequestFates(ctx, mr store.MRKey, projectPath string, merged bool, at time.Time) error`.

- [ ] **Step 1: Write the failing tests**: GitLab `merge` records `merged: true` and `close` records `false`; GitHub `closed` with `pull_request.merged` true and false; a `RequestFates` error logs and still answers 200, and `SupersedeMR` still ran. Add `"merged": false` to the existing fixture and a merged one.

- [ ] **Step 2: Run** `go test ./internal/webhook/`. Expected: FAIL to build.

- [ ] **Step 3: Implement.** `pullRequestEvent.PullRequest` gains `Merged bool`. Call `RequestFates` after `SupersedeMR` in each handler, with the time the webhook arrived. No platform call. A failure logs a warning only.

- [ ] **Step 4: Run** `go test ./internal/webhook/`. Expected: PASS.

- [ ] **Step 5: Commit** with `feat(webhook): record that a change merged or closed`.

### Task 5: The engine works them out

**Files:**
- Create: `internal/engine/fates.go`
- Modify: `internal/engine/engine.go` (`Run`)
- Test: `internal/engine/fates_test.go`

**Interfaces:**
- Consumes: `fates.Decide`, the store functions of Task 3, `forge.GetChange`, `forge.CompareDiff`, `forge.IsBot`, `actions`' dismissals, `result.Parse`, `result.LeftOutNotePrefix`.
- Produces: `func (e *Engine) workFates(ctx context.Context)`, `func fatesInput(runs []store.Run, ds []store.Dismissal) (fates.Input, error)`.

`fatesInput` is pure, so most of this is tested without a database. It parses each run's result, and a run whose result can't be read is skipped. `Hash` comes from `run.SkillHashes[name]`. `Failed` is `Verdict == "failed"`. `Reviewed` is `Files.Reviewed` plus `Files.Carried`. `LeftOut` is any note with `result.LeftOutNotePrefix`.

`workFates`, called on a new `fateInterval = time.Minute` ticker in `Run`, with a budget of 30 seconds a pass:
1. Claim up to 10 jobs.
2. `GetChange`: open means `DropFateJob`; `ErrNotFound` means a merge is taken from the job.
3. `DecidedRuns`, `Dismissals`, `fatesInput`, `Decide`, with `Compare` wrapping `CompareDiff`, parsing each file with `diffmap.Parse`, and mapping `forge.ErrNotFound`, `ErrNotAncestor` and `ErrPartialDiff` to `fates.ErrGone`.
4. `IsBot(change.AuthorID)`; an error means false.
5. `ReplaceFates`. Any other error: `FateJobFailed` with a backoff of five minutes, doubling; at 5 it logs a warning and stops, with no fates.

On the hourly reconcile tick, `ChangesWithoutFates` for changes with a decided run in the last 7 days and no job: `GetChange` for at most one per repository a sweep; if it merged or closed, `RequestFates`. Also on that tick, `PruneFates` for rows older than a year.

- [ ] **Step 1: Write the failing tests**: pure tests of `fatesInput`: `TestFatesInputParsesReviews`, `TestFatesInputSkipsUnreadableResults`, `TestFatesInputLeftOutNote`, `TestFatesInputCarriedFilesAreReviewed`. DB-gated: `TestWorkFatesStoresFates`, `TestWorkFatesReopenedDropsTheJob`, `TestWorkFatesTransientErrorRetriesAndStoresNothing`, `TestWorkFatesStopsAfterFiveAttempts`, `TestWorkFatesForcePushIsWentAway`, `TestWorkFatesBotAuthor`, `TestWorkFatesBudget`, `TestFatesSweepFindsAMissedMerge`, `TestFatesSweepOneCallPerRepository`.

- [ ] **Step 2: Run** `go test ./internal/engine/ -run Fates`. Expected: FAIL to build.

- [ ] **Step 3: Implement.** Check how neighbours such as `sweepApprovals` find a change's platform, and follow them.

- [ ] **Step 4: Run** `go vet ./... && go test ./...`. Expected: PASS, DB tests skipped. Desk-check the DB-gated ones.

- [ ] **Step 5: Commit** with `feat(engine): work out findings' fates when a change ends`.

### Task 6: The Skill results tab

**Files:**
- Create: `internal/dashboard/api_skill_results.go`, `web/src/pages/SkillResults.tsx`
- Modify: `internal/dashboard/api_read.go`, `internal/dashboard/view.go`, `web/src/api.ts`, `web/src/pages/RepositoryDetail.tsx`, `web/src/test/fixtures.ts`, `README.md`, `CLAUDE.md`
- Test: `internal/dashboard/api_skill_results_test.go`, `web/src/pages/RepositoryDetail.test.tsx`

**Interfaces:**
- Consumes: `store.SkillResults`; `d.projectID` for the project, as `api_past_dismissals.go` does.
- Produces: `GET /api/repositories/{instance}/{project...}/skill-results?days=30|90`, returning `[]SkillResultView`: `skill`, `severity`, `raised`, `fixed`, `dismissed`, `open`, `wentAway`, `addressed` (a whole percent, or null under 10 raised), `editedAt` (or null).

Any `days` other than 30 or 90 is 400; no `days` is 30. Sorted lowest `addressed` first, nulls last, then by skill and severity, with `blocking` before `suggestion`.

The tab is **Skill results**, for anyone who can see the repository. A 30 / 90 days choice. Empty text: `No results yet. Numbers start with changes merged after this release.` A row with `editedAt` says `Edited 12 days ago: numbers include the old version`.

- [ ] **Step 1: Write the failing tests**: API (DB-gated): `TestSkillResultsTab`, `TestSkillResultsAddressedFromTenFindings`, `TestSkillResultsSortsLowestFirst`, `TestSkillResultsBadDays`, `TestSkillResultsNoAccess`. Web: the tab's rows, the 90-day choice refetching, the empty text, the edited line, no percentage under 10.

- [ ] **Step 2: Run** `(cd web && npx vitest run)`. Expected: FAIL.

- [ ] **Step 3: Implement.** Follow the Past dismissals tab for the route, the view and the page. README: a section on the tab, with what each fate means and the accepted costs people will notice (Addressed reads low with incremental reviews; rebasing teams see fewer Fixed), plus the dashboard's repository page description and an Upgrading note: migration `0024` runs at startup and 0.7.1 keeps working on it. `CLAUDE.md`: `internal/fates` in the layout.

- [ ] **Step 4: Run** `go vet ./... && go test ./... && (cd web && npx vitest run && npx tsc --noEmit && npm run lint)`. Expected: PASS, DB tests skipped.

- [ ] **Step 5: Commit** with `feat(dashboard): the Skill results tab`.
