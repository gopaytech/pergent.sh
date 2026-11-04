# Inline review threads (pergent.sh 0.6.0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Post each finding as an inline thread on its lines (GitHub and GitLab together), with a one-click suggestion when the fix is small, keep the threads in step with later reviews, and let people dismiss a finding by replying in its thread.

**Architecture:** One pure planner (`internal/threads`) decides, from the review's findings, the change's diff and the threads already posted, what to post, resolve, reopen or leave alone, and which findings stay in the summary. The publisher runs it before rendering the summary, carries out its actions through a new `forge.Inline` interface (implemented by both adapters), records each thread in a new table, and never lets a thread failure fail the publish. The thread body comes from pergent's public `render` package (a new `render.InlineComment`), so model text is neutralised in one place.

**Tech Stack:** Go 1.x, Postgres (migrations under `internal/store/migrations`), GitHub REST + GraphQL, GitLab REST v4, React/TypeScript dashboard (`web/`), pergent `render` (public package of `github.com/gopaytech/pergent`).

**Spec:** `docs/superpowers/specs/2026-11-03-inline-findings-design.md` (§3 placing, §5 threads, §6 permissions and events, §7 switch, "Settled after the builder check" 1–6). The mockup `docs/superpowers/specs/2026-11-03-inline-findings-mockup.html` panels 2–3 show the inline finding and the resolved thread.

## Global Constraints

- Public repositories: no employer, internal hostnames, people or screenshots in code, docs, tests or commits. The owner's screenshots are private and must not be referenced. Test data uses `acme/widgets`, `example.com`, made-up names.
- Reviews stay read-only. A failed review never counts as a pass. Approval logic does not change: inline threads, their failures and the switch never change a status, a description, an approval or `decisionOf`.
- **Inline posting never breaks the review.** Any thread error (permission, API, line not in the diff, store write) is logged; the finding goes in the summary with a note; the publish still succeeds. Never `publish_failed` because of threads.
- Secrets are redacted from everything posted, including suggestion text and thread replies. A suggestion that redaction would change is not offered as a suggestion block at all.
- The model's text in inline comments is neutralised as in the summary (no quick actions outside code blocks, no `@`-mentions that ping, no forged markers).
- Cap: at most **20** pergent-owned threads open at the same time per merge request. Ordering: blocking first, then suggestions, each by file then line.
- "Small" fix: at most **10** replaced lines (pergent already drops longer ones), all within the finding's own lines and inside one diff hunk.
- Per-repository setting **"Inline findings"**, on by default. Off: every finding stays in the summary; threads already posted are left as they are.
- Migration `0017` adds a table and nullable or defaulted columns only, so 0.5.4 keeps running on a migrated database. Existing constraints are not changed.
- Exact strings (thread replies, notes, titles) are copied verbatim from this plan.
- Match the surrounding code: comment density, naming, error wrapping (`fmt.Errorf("verb object: %w", err)`), table-driven tests where the package already uses them.
- DB tests skip without `PERGENT_SH_TEST_DATABASE_URL`; they must still compile and pass in CI, which sets it.

## Review Focus

1. **Approving with the review App** (`approve_with_review_app`): the review App's COMMENT review that carries the threads arrives after its APPROVE, and `HasApproved` reads the bot's *latest* review (`internal/forge/githubforge/approver.go:339`). A person expects the approval to still count. Task 6 makes `HasApproved` skip `COMMENTED` reviews, with a test.
2. **A publish for a head that is no longer the change's head** (a superseded run, or a dismissal re-publish racing a push) must not post, resolve or reopen anything; its comment lists every finding as before. Task 8 test `TestInlineSkippedForOldHead`.
3. **A publish retried after threads were posted but before the rows were stored** must not post them again: threads with a pergent marker and no row are adopted. Task 8 test `TestInlineAdoptsUnrecordedThreads`.
4. **A suggestion that redaction would change** (it contains a token) must not be posted as a suggestion block, or "Commit suggestion" would write the redacted text. Task 8 test `TestInlineDropsRedactedSuggestion`.
5. **A skill that failed in the new review**: its open threads stay open (never "Fixed"); a thread a person resolved by hand is neither reopened nor posted again. Task 3 tests `TestFailedSkillKeepsThreads`, `TestPersonResolvedThreadLeftAlone`.

---

## Workspaces and order

- Task 1 runs in the **pergent** repository (`../pergent`, worktree `../pergent-inline`, branch `feat/inline-comment`, base `master`). It ends with a pull request and, **only with the owner's permission**, the release **pergent v0.4.0**.
- Tasks 2–10 run in **pergent.sh** (worktree `../pergent.sh-inline`, branch `feat/inline-threads`, base `main`). Task 2 needs the v0.4.0 tag; until it exists, Tasks 3–5 (which don't touch `render.InlineComment`) can go first, in the order 3, 4, 5, then 2, then 6 onward.
- Commit this plan as the first commit on `feat/inline-threads`.

---

### Task 1: `render.InlineComment` and the finding marker (pergent)

**Files:**
- Create: `render/inline.go`, `render/inline_test.go` (in the pergent repository)
- Modify: `README.md` of pergent only if it documents the `render` API (check; otherwise nothing)

**Interfaces:**
- Produces (used by Tasks 2, 6, 7, 8):
  ```go
  // InlineOptions changes how InlineComment renders.
  type InlineOptions struct {
      // SuggestionInfo is the fence info string that makes the platform offer
      // to apply Finding.Suggestion: "suggestion" (GitHub) or
      // "suggestion:-0+N" (GitLab). Empty shows no suggestion block.
      SuggestionInfo string
      // DismissCommand is the command prefix, such as "@pergent-review dismiss";
      // empty means no dismiss line.
      DismissCommand string
      // Skill names the skill on the comment; empty leaves it out.
      Skill string
  }
  func InlineComment(f Finding, o InlineOptions) string
  func InlineMarker(fingerprint string) string          // "<!-- pergent:finding <fingerprint> -->"
  func InlineFingerprint(body string) (string, bool)    // reads the marker from the body's last non-empty line only
  ```

Body layout (exact):
```
**❌ Blocking: <title>**            ← "**💡 Suggestion: <title>**" for a suggestion
<explanation>

**Fix:** <suggested fix>            ← only when SuggestedFix is non-empty

```<SuggestionInfo>                 ← only when SuggestionInfo != "" and Suggestion != ""
<suggestion, byte for byte>
```

<sub>`<skill>` · id `<id>` · Not a real problem? Reply `<DismissCommand> <reason>` in this thread.</sub>
<!-- pergent:finding <fingerprint> -->
```
The `<sub>` line drops the parts that are empty (no skill, no ID, no dismiss command); it is omitted when all are empty. The marker is always the last line.

Rules:
- Title, explanation and fix go through the same neutralisation `Comment` uses (`modelText`, `oneLine` for the title, mention-safe, quick actions neutralised outside code fences).
- The suggestion is written **unchanged** inside a fence of `max(3, longestBacktickRun+1)` backticks. It is never escaped: escaping would change what the platform applies. If it contains `"\r"`, or a line equal to the closing fence, the suggestion block is left out (the fix stays in words).
- `InlineFingerprint` accepts only `<!-- pergent:finding X -->` where X is 1–128 characters of `[0-9a-zA-Z:_-]`, on the last non-empty line; anything else is `"", false`. A marker inside the model text can't be the last line, because the trusted `<sub>` line or marker always follows it, and `neutralizeComments` already escapes `<!--` in model text.

- [ ] **Step 1: Write the failing tests** in `render/inline_test.go`:
  - `TestInlineCommentBlockingWithSuggestion`: blocking finding with ID `c4d9a1`, fingerprint `fp1`, Suggestion `"\tseen := make(map[string]int)"`, `SuggestionInfo: "suggestion"`, `DismissCommand: "@pergent-review dismiss"`, `Skill: "go"`. The body starts with `**❌ Blocking: Write to a nil map**`, contains "```suggestion\n\tseen := make(map[string]int)\n```", contains ``Reply `@pergent-review dismiss <reason>` in this thread.``, and its last line is `<!-- pergent:finding fp1 -->`.
  - `TestInlineCommentGitLabSuggestionInfo`: `SuggestionInfo: "suggestion:-0+2"` gives a fence opening "```suggestion:-0+2".
  - `TestInlineCommentNoSuggestionBlockWithoutInfo`: a finding with a Suggestion and empty SuggestionInfo has no "```suggestion"; its Fix line stays.
  - `TestInlineCommentSuggestionKeptByteForByte`: a suggestion containing `<!-- x -->`, `@someone` and a line starting with `/` appears unchanged inside the fence.
  - `TestInlineCommentSuggestionWithCarriageReturnDropped`: `"a\r\nb"` gives no suggestion block.
  - `TestInlineCommentNeutralisesModelText`: an explanation with `@team`, a line `/approve` and `<!-- pergent:finding evil -->` renders with the mention broken, the quick action neutralised, the comment escaped; `InlineFingerprint(body)` returns the real fingerprint.
  - `TestInlineFingerprint`: table: valid marker last line → ok; marker not on the last non-empty line → false; trailing blank lines → ok; bad characters → false; empty body → false.
  - `TestInlineMarkerRoundTrip`: `InlineFingerprint("x\n" + InlineMarker("ab:12"))` is `"ab:12", true`.
- [ ] **Step 2: Run** `go test ./render/ -run 'Inline' -v`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `render/inline.go`, reusing the package's private helpers (`modelText`, `oneLine`, `mentionSafe`, `neutralizeQuickActions`, `longestBacktickRun`).
- [ ] **Step 4: Run** `go test ./... && go vet ./...`. Expected: PASS, golden files unchanged.
- [ ] **Step 5: Commit** `feat(render): InlineComment and the finding marker for inline threads`.
- [ ] **Step 6: Stop and ask the owner** to approve: push the branch, open the PR, and after merge tag **v0.4.0** with release notes ("`render.InlineComment`, `InlineMarker`, `InlineFingerprint` for hosts that post findings as inline threads; CI mode unchanged"). The release workflow builds `ghcr.io/gopaytech/pergent:v0.4.0`. Do not continue to Task 2 until the tag and image exist.

---

### Task 2: Pin pergent v0.4.0 (pergent.sh)

**Files:**
- Modify: `go.mod`, `go.sum`, `internal/config/config.go` (`defaultImage`), `internal/config/config_test.go:42`, `README.md` (the `jobs.image` default row and every "default `jobs.image`" mention for the current release)

- [ ] **Step 1:** Change the test expectation at `internal/config/config_test.go:42` to `ghcr.io/gopaytech/pergent:v0.4.0`; run `go test ./internal/config/`. Expected: FAIL.
- [ ] **Step 2:** `go get github.com/gopaytech/pergent@v0.4.0 && go mod tidy`; set `defaultImage = "ghcr.io/gopaytech/pergent:v0.4.0"`; update the README default row.
- [ ] **Step 3: Run** `go build ./... && go test ./...`. Expected: PASS.
- [ ] **Step 4: Commit** `chore: pergent v0.4.0`.

---

### Task 3: Diff hunks (`internal/diffmap`)

**Files:**
- Create: `internal/diffmap/diffmap.go`, `internal/diffmap/diffmap_test.go`

**Interfaces:**
- Produces (Tasks 4, 6, 7, 8):
  ```go
  // File is one file's diff: the hunks of its unified diff, head side and base side.
  type File struct {
      Path, OldPath string
      Deleted       bool
      Hunks         []Hunk
  }
  type Hunk struct {
      OldStart, OldLines, NewStart, NewLines int
      // Lines holds, for each line of the hunk on the new side, its old line
      // (0 for an added line); removed lines are not listed.
      Lines []Pair
  }
  type Pair struct{ Old, New int }
  func Parse(path, oldPath, patch string, deleted bool) File  // patch is the hunks only, from "@@" on, as the platforms return per file
  func (f File) HunkOf(line int) (int, bool)       // index of the hunk whose new side holds line (context lines count)
  func (f File) OldLine(line int) (int, bool)      // the base line of an unchanged head line; false for an added line or one outside the hunks
  func (f File) MapOld(oldLine int) (int, bool)    // the new-side line of an old-side line, through this diff: a line outside the hunks moves by the hunks above it; an unchanged line inside a hunk maps to its pair; a removed line is false
  ```

- [ ] **Step 1: Write the failing tests**:
  - `TestParseHunks`: a patch with two hunks (`@@ -10,4 +10,5 @@` with one added line, `@@ -40,3 +41,2 @@` with one removed line) gives 2 hunks with the right starts and `Lines`.
  - `TestHunkOf`: lines 10–14 are in hunk 0 (context lines included), 41–42 in hunk 1, 9, 15 and 43 are not.
  - `TestOldLine`: an unchanged line returns its old line; an added line returns false.
  - `TestMapOld`: old line 5 (above both hunks) → 5; old line 20 (between hunks) → 21; the removed old line → false; an unchanged old line inside hunk 1 → its new line.
  - `TestParseEmptyAndMalformed`: empty patch → no hunks; a header without numbers is skipped; `"\ No newline at end of file"` lines are ignored; a hunk header `@@ -0,0 +1,3 @@` (new file) gives new lines 1–3.
- [ ] **Step 2: Run** `go test ./internal/diffmap/`. Expected: FAIL.
- [ ] **Step 3: Implement.** Count lines per hunk from the header; a `+` line advances new, a `-` line advances old, a space (or empty) line advances both.
- [ ] **Step 4: Run** `go test ./internal/diffmap/ -v`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(diffmap): parse diff hunks and map lines`.

---

### Task 4: The thread planner (`internal/threads`)

The heart of the feature, with no I/O. Every rule of spec §3, §5 and "Settled" 1–4 lives here.

**Files:**
- Create: `internal/threads/threads.go`, `internal/threads/threads_test.go`

**Interfaces:**
- Consumes: `diffmap.File` (Task 3).
- Produces (Task 8):
  ```go
  type State string
  const (
      StateOpen      State = "open"
      StateFixed     State = "fixed"     // resolved by pergent.sh: the finding is gone
      StateGone      State = "gone"      // resolved by pergent.sh: the file left the change
      StateDismissed State = "dismissed" // resolved by pergent.sh: the finding was dismissed
  )
  const Cap = 20

  type Finding struct {
      Fingerprint, ID, Skill, File, Severity string // Severity "blocking" or "suggestion"
      Line, EndLine int
      HasSuggestion bool
  }
  type Thread struct {
      Fingerprint, Skill, File string
      Line, EndLine int
      Commit   string
      State    State
      Resolved bool // as the platform shows it now
  }
  type Dismissal struct{ By, Reason string }
  type Coverage struct {
      Failed   bool
      Reviewed map[string]bool // files the skill reviewed in this review
  }
  type Input struct {
      Head         string                 // the reviewed head SHA
      Findings     []Finding              // the review's findings that are not dismissed
      Dismissed    map[string]Dismissal   // by fingerprint
      Skills       map[string]Coverage    // by skill name, every skill of the review
      Diff         map[string]diffmap.File // the change's files at Head, by path
      Threads      []Thread               // pergent-owned threads of the change, stored and adopted
      Map          func(t Thread) (line, endLine int, ok bool) // t's lines at Head; ok false when they can't be mapped
      OnlyBlocking bool                   // the target branch requires threads resolved before merge
  }
  type Post struct {
      Finding       Finding
      Line, EndLine int  // the anchor; EndLine == Line for one line
      Suggestion    bool // post the suggestion block
  }
  type Resolve struct {
      Thread Thread
      State  State  // StateFixed, StateGone or StateDismissed
      Reply  string
  }
  type Reopen struct {
      Thread Thread
      Reply  string
  }
  type Rebind struct {
      Thread         Thread
      Fingerprint    string
      Line, EndLine  int
  }
  type Plan struct {
      Post    []Post
      Resolve []Resolve
      Reopen  []Reopen
      Rebind  []Rebind
      Inline  map[string]bool   // fingerprints shown on the diff: kept, reopened or about to be posted
      Notes   map[string]string // fingerprint → why it is in the summary
  }
  func Decide(in Input) Plan
  func ShortSHA(sha string) string // first 7 characters
  ```

Exact strings:
```go
const (
    NoteOffDiff   = "Not on the diff: its lines didn't change here, so it can't be a comment on them."
    NoteOverCap   = "Over the limit of 20 open inline threads; it moves to the diff when one is resolved."
    NoteFailed    = "Posting it on the diff failed, so it is listed here."
    NoteKeptRule  = "Kept here: this branch requires threads resolved before merge."
    NoteTooLong   = "Too long for an inline comment, so it is listed here."
    ReplyGone     = "No longer in this change."
    ReplyRestored = "The dismissal was withdrawn, so this finding is open again."
)
func ReplyFixed(head string) string        // "Fixed in `<7>`."
func ReplyBack(head string) string         // "Back in `<7>`."
func ReplyDismissed(d Dismissal) string    // "Dismissed by <By>: <Reason>" or "Dismissed by <By>." with no reason
```
`NoteFailed` and `NoteTooLong` are set by the engine (Task 8); the planner sets the other three.

Algorithm (the rules the tests pin):
1. **Exact match.** A thread whose fingerprint is among the findings is that finding's thread.
2. **Reworded match** (Settled 1). An unmatched thread is matched to an unmatched finding of the **same skill and file** whose range overlaps the thread's range mapped to Head (`Map`), or, when `Map` says it can't, whose range is within 5 lines of the stored range. The closest finding wins (smallest line distance, then file order). The result is a `Rebind` to the finding's fingerprint and lines; from then on it is handled as an exact match.
3. **A matched thread:**
   - state `open`, platform unresolved: kept (Inline).
   - state `open`, platform resolved (a person resolved it): kept as it is, Inline, never reopened, never posted again.
   - state `fixed` or `gone`: `Reopen` with `ReplyBack(Head)` (Settled 3), Inline.
   - state `dismissed` (the dismissal was withdrawn, so the finding is back): `Reopen` with `ReplyRestored`, Inline.
4. **A dismissed finding's thread** (fingerprint in `Dismissed`), state not `dismissed`: `Resolve` with `StateDismissed`, `ReplyDismissed`. A thread already `dismissed` is left alone.
5. **A thread whose finding is gone** (no match, not dismissed), state `open` only:
   - the file is not in `Diff`: `Resolve` with `StateGone`, `ReplyGone`.
   - the skill is in `Skills`, not `Failed`, and `Reviewed[file]`: `Resolve` with `StateFixed`, `ReplyFixed(Head)`.
   - anything else (skill failed, removed, file skipped or out of scope): left alone (Settled 2).
6. **New findings** (no thread): a finding goes inline only when `Line > 0`, `Line` is inside a hunk, and `EndLine` (when set and greater) is inside a hunk too; otherwise `NoteOffDiff`. With `OnlyBlocking`, a suggestion gets `NoteKeptRule`.
   - The anchor is `Line..EndLine` when both are in the same hunk. When they are in different hunks the anchor is `Line` alone and there is no suggestion.
   - `Suggestion` is true only when `HasSuggestion` and the whole range is in one hunk.
7. **Cap** (Settled 4). `open` = matched threads whose state is `open` and that the platform shows unresolved, plus the ones being reopened. The candidates of step 6 are sorted blocking first, then file, then line; the first `Cap - open` are posted (Inline); the rest get `NoteOverCap`. A negative remainder posts nothing.
8. A thread that is not matched and not acted on (left alone in step 5) puts nothing in `Inline` or `Notes`; its finding is gone anyway.

- [ ] **Step 1: Write the failing tests** (table-driven where it reads well), each asserting the `Plan` fields named:
  - `TestNewFindingPostedWhenOnDiff`: one blocking finding at lines 42–43 inside a hunk → one `Post` with anchor 42–43, `Inline[fp]`.
  - `TestSuggestionOnlyWithinOneHunk`: range in one hunk → `Suggestion: true`; `Line` and `EndLine` in different hunks → anchor `Line` alone, `Suggestion: false`; `EndLine` outside the diff → `Notes[fp] == NoteOffDiff`.
  - `TestOffDiffAndNoLine`: `Line == 0` and a line outside every hunk → `NoteOffDiff`, no `Post`.
  - `TestOnlyBlockingKeepsSuggestions`: `OnlyBlocking` → the blocking finding is posted, the suggestion gets `NoteKeptRule`.
  - `TestCapCountsOpenThreads`: 18 open unresolved threads and 4 new findings (2 suggestions in `a.go`, 2 blocking in `z.go`) → the 2 blocking are posted, both suggestions get `NoteOverCap`.
  - `TestCapIgnoresPersonResolvedAndDismissed`: 20 threads, 19 of them resolved by a person or `dismissed` → 19 slots free.
  - `TestExactMatchKept`: an open thread with a found fingerprint → no actions, `Inline[fp]`.
  - `TestRewordedFindingRebound`: thread `fpOld` (skill `go`, `a.go` 40–41), new finding `fpNew` (skill `go`, `a.go` 42), `Map` returns 41–42 → one `Rebind` to `fpNew`, no `Post`, no `Resolve`; the same with `Map` returning `ok == false` and the finding at line 45 (within 5) → `Rebind`; at line 47 → the thread is resolved as fixed and the finding posted.
  - `TestRewordedNeedsSameSkillAndFile`: another skill or another file → no `Rebind`.
  - `TestGoneFindingFixed`: skill ran fine and reviewed the file → `Resolve{State: StateFixed, Reply: "Fixed in `abcdef1`."}` with `Head: "abcdef1234"`.
  - `TestFileLeftChange`: file not in `Diff` → `Resolve{State: StateGone, Reply: "No longer in this change."}`.
  - `TestFailedSkillKeepsThreads`: skill `Failed` → no actions for its threads, nothing posted for its findings' threads.
  - `TestSkippedOrRemovedSkillKeepsThreads`: skill missing from `Skills`, or the file not in `Reviewed` → no actions.
  - `TestPersonResolvedThreadLeftAlone`: state `open`, `Resolved: true`, finding still found → no actions, `Inline[fp]`, and the finding is not posted.
  - `TestReturningFindingReopens`: state `fixed`, finding found → `Reopen` with "Back in `abcdef1`.".
  - `TestDismissedResolvesThread`: open thread, fingerprint in `Dismissed{By: "mo (gitlab main)", Reason: "false positive"}` → `Resolve{State: StateDismissed, Reply: "Dismissed by mo (gitlab main): false positive"}`; empty reason → `"Dismissed by mo (gitlab main)."`; already `dismissed` → nothing.
  - `TestRestoredDismissalReopens`: state `dismissed`, finding found and not dismissed → `Reopen` with `ReplyRestored`.
  - `TestOrderBlockingThenFileThenLine`: posts come out blocking first, then by file, then by line.
- [ ] **Step 2: Run** `go test ./internal/threads/`. Expected: FAIL.
- [ ] **Step 3: Implement** `Decide` following the algorithm above, and `ShortSHA`, `ReplyFixed`, `ReplyBack`, `ReplyDismissed`. `ReplyDismissed` takes `By` and `Reason` as given; the engine makes them safe (Task 8).
- [ ] **Step 4: Run** `go test ./internal/threads/ -v`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(threads): decide what inline threads to post, resolve and reopen`.

---

### Task 5: Storage and the "Inline findings" setting

**Files:**
- Create: `internal/store/migrations/0017_inline_threads.sql`, `internal/store/inline.go`, `internal/store/inline_test.go`
- Modify: `internal/store/dashboard.go` (`DashboardRepo.InlineFindings`), `internal/store/instances.go` (`RepoDefaults.InlineFindings`), `internal/store/projects.go` (`Project.InlineAvailable`, `DeleteProject` also deletes `inline_threads`), `internal/store/commands.go` (`Command.Discussion`, `Command.ReplyTo`)
- Modify: `internal/config/config.go` (`Repository.InlineFindings *bool`, yaml `inline_findings`), `internal/settings/settings.go` (`KeyInlineFindings = "inline_findings"`, `Effective.InlineFindings bool`, built-in default `true`, from config, from dashboard, from repo defaults), `internal/settings/settings_test.go` (`allKeys`)
- Modify: `internal/dashboard/api_read.go` (`settingViews`: `"inlineFindings"`), `api_write.go` (`parseSettings`, `boolSetting`, `resetSettings`, `msgBadSettings`), `api_operator.go` (`defaultsView`, `parseDefaults`, `changedDefaults`, `msgBadDefaults`), `view.go` (`DefaultsView.InlineFindings`, the settings doc comment)
- Modify: `web/src/api.ts`, `web/src/pages/RepositorySettings.tsx`, `web/src/pages/AdminSettings.tsx`, `web/src/test/fixtures.ts`, tests `web/src/pages/RepositoryDetail.test.tsx`, `web/src/pages/AdminSettings.test.tsx`
- Tests: `internal/store/dashboard_test.go`, `instances_test.go`, `projects_test.go`, `commands_test.go`; `internal/config/config_test.go`; `internal/dashboard/api_write_test.go`, `api_operator_test.go`, `api_read_test.go`

Migration `0017_inline_threads.sql` (exact):
```sql
-- Inline findings (pergent.sh 0.6.0). inline_threads maps a finding of a
-- merge request to the platform thread pergent.sh posted for it: the GitHub
-- review thread node ID and its first comment's ID, or the GitLab discussion
-- ID and its first note's ID. state is open, or how pergent.sh resolved it:
-- fixed (the finding is gone), gone (its file left the change) or dismissed.
-- A thread a person resolved by hand stays open here; the platform says it
-- is resolved.
CREATE TABLE inline_threads (
    instance    text    NOT NULL,
    project_id  bigint  NOT NULL,
    mr_iid      bigint  NOT NULL,
    fingerprint text    NOT NULL,
    skill       text    NOT NULL DEFAULT '',
    file        text    NOT NULL,
    line        integer NOT NULL,
    end_line    integer NOT NULL DEFAULT 0,
    commit_sha  text    NOT NULL,
    thread_id   text    NOT NULL,
    comment_id  bigint  NOT NULL,
    state       text    NOT NULL CHECK (state IN ('open', 'fixed', 'gone', 'dismissed')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance, project_id, mr_iid, fingerprint),
    UNIQUE (instance, project_id, mr_iid, thread_id)
);
-- The per-repository switch: NULL is the default (on).
ALTER TABLE dashboard_repos ADD COLUMN inline_findings boolean;
ALTER TABLE repo_defaults   ADD COLUMN inline_findings boolean;
-- Whether the review App may post inline threads in a GitHub repository
-- (Pull requests: Read and write). Always true on GitLab.
ALTER TABLE projects ADD COLUMN inline_available boolean NOT NULL DEFAULT true;
-- A command written as a reply in a thread: the GitLab discussion, or the
-- GitHub review comment it replies to. Empty and 0 for a plain comment.
ALTER TABLE commands ADD COLUMN discussion_id text   NOT NULL DEFAULT '';
ALTER TABLE commands ADD COLUMN reply_to      bigint NOT NULL DEFAULT 0;
```
The `commands` unique key `(instance, project_id, mr_iid, comment_id)` stays as it is, because 0.5.4 inserts with `ON CONFLICT` on it.

**Interfaces:**
- Produces (Tasks 6, 8, 9):
  ```go
  type InlineThread struct {
      MR                  MRKey
      Fingerprint, Skill  string
      File                string
      Line, EndLine       int
      Commit              string
      ThreadID            string
      CommentID           int64
      State               string // "open", "fixed", "gone", "dismissed"
  }
  func (s *Store) InlineThreads(ctx context.Context, mr MRKey) ([]InlineThread, error)
  func (s *Store) PutInlineThread(ctx context.Context, t InlineThread) error // upsert on (mr, fingerprint); sets updated_at
  func (s *Store) RebindInlineThread(ctx context.Context, mr MRKey, from, to string, line, endLine int, commit string) error
  func (s *Store) SetInlineThreadState(ctx context.Context, mr MRKey, fingerprint, state string) error
  func (s *Store) DeleteInlineThread(ctx context.Context, mr MRKey, fingerprint string) error
  // InlineThreadFor finds the thread a reply belongs to: by GitLab discussion
  // ID when discussion is set, else by the GitHub first comment ID replyTo.
  func (s *Store) InlineThreadFor(ctx context.Context, mr MRKey, discussion string, replyTo int64) (InlineThread, bool, error)
  ```
  `store.Command` gains `Discussion string` and `ReplyTo int64` (`InThread()` reports `Discussion != "" || ReplyTo != 0`). `store.Project` gains `InlineAvailable bool`. `settings.Effective` gains `InlineFindings bool`. Dashboard JSON key `inlineFindings`; error text "inlineFindings must be true, false or null".

Web copy (exact):
- Repository → Settings, under **Reviews**, after "Review drafts": a `Setting` named **Inline findings** with help "Post each finding as a comment on its lines, with a one-click fix when it is small. Off keeps every finding in the review comment." and a `Switch` labelled "Inline findings".
- Admin → Settings defaults: the same name and help, after "Review drafts".

- [ ] **Step 1: Write the failing Go tests:**
  - store: `TestInlineThreadsRoundTrip` (put, list, set state, rebind, delete, `InlineThreadFor` by discussion and by reply-to, unknown → not found), `TestInlineThreadsUniqueThreadID` (a second fingerprint with the same `thread_id` fails), `TestDeleteProjectDeletesInlineThreads`, `TestCommandThreadFields` (insert and claim keep `Discussion` and `ReplyTo`), `TestProjectInlineAvailable` (default true; `UpsertGitHubProject` stores false), and extend `TestDashboardReposCRUD`, `TestUpdateDashboardRepo`, `TestRepoDefaultsRoundTrip` with `InlineFindings`.
  - config: `inline_findings: false` parses to `false`; absent is nil.
  - settings: built-in default `true`; config `false` overrides (`Overridden["inline_findings"]`); dashboard row overrides repo defaults; `allKeys` includes it.
  - dashboard: `TestInlineFindingsSetting` (`{"inlineFindings": false}` stored; `null` clears; `"no"` → 400 with the exact message), reset clears it, `TestDefaultsInlineFindings`, `TestSettingViewsInlineFindings`.
- [ ] **Step 2: Run** `go test ./internal/store/ ./internal/config/ ./internal/settings/ ./internal/dashboard/`. Expected: FAIL (compile errors).
- [ ] **Step 3: Implement** the migration, store code and settings, following `approve` (the default-on bool) and `max_turns` (the newest setting) everywhere they appear.
- [ ] **Step 4: Write the failing web tests:** `RepositoryDetail.test.tsx` toggles **Inline findings** off and expects the PUT body `{inlineFindings: false}`; `AdminSettings.test.tsx` expects `inlineFindings` in the defaults PUT. Run `cd web && npm test`. Expected: FAIL.
- [ ] **Step 5: Implement** the web changes and fixtures.
- [ ] **Step 6: Run** `go test ./... && cd web && npm test && npm run build`. Expected: PASS.
- [ ] **Step 7: Commit** `feat: inline_threads table and the Inline findings setting`.

---

### Task 6: GitHub inline threads

**Files:**
- Modify: `internal/github/client.go`, `internal/github/types.go`, `internal/github/client_test.go`
- Create: `internal/github/graphql.go`, `internal/github/graphql_test.go`
- Modify: `internal/forge/forge.go` (the `Inline` interface and types below), `internal/forge/githubforge/githubforge.go`, `internal/forge/githubforge/inline.go` (create), `internal/forge/githubforge/inline_test.go` (create), `internal/forge/githubforge/approver.go`, `internal/forge/githubforge/githubforge_test.go`
- Modify: `internal/engine/reconcile.go` (`reconcileGitHubRepo` sets `InlineAvailable`), `internal/engine/github_test.go`
- Modify: `internal/dashboard/api_read.go` (`IntegrationRepo.InlineAvailable`), `internal/dashboard/view.go`, `web/src/api.ts`, `web/src/pages/Integrations.tsx` (+ its test)

**Interfaces:**
- Produces in `internal/forge/forge.go` (used by Tasks 7, 8, 9):
  ```go
  // FileDiff is one file of a diff as the platform returns it: Patch is the
  // file's hunks, from the first "@@" on; empty for a binary or too large file.
  type FileDiff struct {
      Path, OldPath string
      Deleted       bool
      Patch         string
  }
  // ThreadRef names a thread: GitHub's review thread node ID and its first
  // comment's ID, GitLab's discussion ID and its first note's ID.
  type ThreadRef struct {
      ID        string
      CommentID int64
  }
  // Thread is a thread of the review bot's on a change, as the platform shows
  // it now. Line and EndLine are where it is anchored at the change's head (0
  // when the platform can't say, e.g. GitHub marks it outdated).
  type Thread struct {
      Ref           ThreadRef
      Body          string // its first comment
      Path          string
      Line, EndLine int
      Resolved      bool
  }
  // NewThread is a thread to post at a change's head. OldLine is the base
  // line of Line when Line is an unchanged line (GitLab positions need it); 0
  // for an added line.
  type NewThread struct {
      Path, OldPath string
      Line, EndLine int
      OldLine       int
      Body          string
  }
  // Inline is the review bot's inline threads on one platform.
  type Inline interface {
      // ChangeDiff returns the change's diff at its head.
      ChangeDiff(ctx context.Context, projectID, number int64) ([]FileDiff, error)
      // CompareDiff returns the diff from one commit to another.
      CompareDiff(ctx context.Context, projectID int64, from, to string) ([]FileDiff, error)
      // Threads lists the review bot's own threads on the change.
      Threads(ctx context.Context, projectID, number int64) ([]Thread, error)
      // PostThreads posts threads at ch's head. It returns one ref per thread,
      // in order; a thread that could not be posted has a zero ref and its
      // error in errs. err is a failure of the whole call.
      PostThreads(ctx context.Context, projectID int64, ch Change, threads []NewThread) (refs []ThreadRef, errs []error, err error)
      // ReplyThread adds a reply to a thread.
      ReplyThread(ctx context.Context, projectID, number int64, t ThreadRef, body string) error
      // ResolveThread resolves the thread, or unresolves it when resolved is false.
      ResolveThread(ctx context.Context, projectID, number int64, t ThreadRef, resolved bool) error
      // ReactThread adds the emoji for reaction to a reply in a thread.
      ReactThread(ctx context.Context, projectID, number, commentID int64, reaction Reaction) error
      // ThreadsBlockMerge reports whether open threads block merging into branch.
      ThreadsBlockMerge(ctx context.Context, projectID int64, branch string) (bool, error)
  }
  ```
- GitHub calls (implementer checks each against GitHub's REST and GraphQL docs and pins the request shapes in `client_test.go`):
  - `PullFiles(ctx, owner, repo, number)` → `GET /repos/{o}/{r}/pulls/{n}/files` (paginated; `filename`, `previous_filename`, `status == "removed"`, `patch`).
  - `CompareFiles(ctx, owner, repo, from, to)` → `GET /repos/{o}/{r}/compare/{from}...{to}` (`files[]` as above).
  - `CreateReviewComments(ctx, owner, repo, number, commitID, body string, comments []ReviewComment) (int64, error)` → `POST /pulls/{n}/reviews` with `event: "COMMENT"`, `body`, `comments: [{path, body, line, side: "RIGHT", start_line, start_side: "RIGHT"}]` (`start_*` only when `EndLine > Line`).
  - `ReplyReviewComment(ctx, owner, repo, number, commentID, body)` → `POST /pulls/{n}/comments/{id}/replies`.
  - `ReactToReviewComment(ctx, owner, repo, commentID, content)` → `POST /pulls/comments/{id}/reactions`.
  - GraphQL `ReviewThreads(ctx, owner, repo, number)`: `reviewThreads(first: 100, after: $cursor)` with `id isResolved isOutdated path line startLine comments(first: 1) { nodes { databaseId body author { login } } }`, paginated, at most 30 pages. `ResolveReviewThread(ctx, threadID)` / `UnresolveReviewThread(ctx, threadID)`: the `resolveReviewThread` / `unresolveReviewThread` mutations. The GraphQL URL is the REST base with a final `/v3` replaced by `/graphql` (`https://ghe.example.com/api/v3` → `https://ghe.example.com/api/graphql`), else the base plus `/graphql`. A GraphQL `errors` array is an error.
  - `ThreadResolutionRequired(ctx, owner, repo, branch) (bool, error)`: true when the branch rules (`GET /rules/branches/{branch}`) have a `pull_request` rule with `required_review_thread_resolution: true`, or the classic protection (`GET /branches/{branch}/protection`) has `required_conversation_resolution.enabled: true`. A 404 on the classic protection is "not protected"; any other failure is an error.
- The adapter (`githubforge.Forge` implements `forge.Inline`):
  - The review body of `PostThreads` (exact): ``"Findings on the changed lines, from the review of `<7>`."``.
  - `PostThreads` posts all threads in one review. On a 422 it posts them one per review, so a bad one fails alone. It then lists `ReviewThreads` and returns the ref whose first comment carries the same `render.InlineMarker` for each thread.
  - `Threads` keeps the threads whose first comment's author is the review App's bot (login `slug` or `slug[bot]`; check which one GraphQL returns) and whose body has a marker (`render.InlineFingerprint`). `Line` is GraphQL `startLine` or `line`, and `EndLine` is `line`; both are 0 when `isOutdated`.
- Approver fix: `HasApproved` ignores reviews whose state is `COMMENTED` when finding the bot's latest review.
- Reconcile: `reconcileGitHubRepo` sets `p.InlineAvailable` from `app.InstallationInfo(ctx, owner, name)`'s `PullRequestsWrite()`; a failed read keeps the stored value (true when new).
- Integrations page: each repository row also shows "Inline findings: off: the review App has Pull requests: Read-only." when `inlineAvailable` is false.

- [ ] **Step 1: Write the failing tests:**
  - `internal/github`: request path, method and JSON body for every call above against `httptest`; GraphQL pagination (2 pages) and `errors` handling; `graphqlURL` for `https://api.github.com` and `https://ghe.example.com/api/v3`; `ThreadResolutionRequired` true from rules, true from classic protection, false on a 404 protection, an error on a 403.
  - `githubforge`: `var _ forge.Inline = (*Forge)(nil)`; `TestPostThreadsOneReview` (one POST with 2 comments; the multi-line one has `start_line`); `TestPostThreads422FallsBackPerComment` (the first POST gets 422; then one POST each; the bad one's error in `errs`, the good one's ref returned); `TestThreadsOnlyBotWithMarker`; `TestThreadsOutdatedHasNoLines`.
  - `approver`: `TestHasApprovedIgnoresCommentReview` (reviews APPROVED at sha, then COMMENTED → true).
  - engine: `TestReconcileGitHubInlineAvailable` (installation `pull_requests: read` → `InlineAvailable == false`).
  - dashboard and web: the Integrations row shows the exact text when `inlineAvailable` is false.
- [ ] **Step 2: Run** `go test ./internal/github/ ./internal/forge/... ./internal/engine/ ./internal/dashboard/`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./... && cd web && npm test`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(github): inline review threads`.

---

### Task 7: GitLab inline threads

**Files:**
- Modify: `internal/gitlab/client.go`, `internal/gitlab/types.go`, `internal/gitlab/client_test.go`
- Create: `internal/forge/gitlabforge/inline.go`, `internal/forge/gitlabforge/inline_test.go`

**Interfaces:**
- Consumes: `forge.Inline` and its types (Task 6), `render.InlineMarker` / `InlineFingerprint` (Task 1).
- GitLab calls (pin the shapes in `client_test.go`; check each against the GitLab API docs):
  - `MergeRequestDiffs(ctx, projectID, iid)` → `GET /projects/:id/merge_requests/:iid/diffs` (paginated); on a 404 fall back to `GET .../changes` (older GitLab). Fields `new_path`, `old_path`, `deleted_file`, `diff`.
  - `Compare(ctx, projectID, from, to)` → `GET /projects/:id/repository/compare?from=&to=&straight=true` (`diffs[]` as above).
  - `Discussions(ctx, projectID, iid)` → `GET .../discussions` (paginated): `id`, `notes[0].{id, body, author.id, position.{new_path, new_line, line_range}, resolvable, resolved}`.
  - `CreateDiffDiscussion(ctx, projectID, iid, body string, pos Position) (discussionID string, noteID int64, err error)` → `POST .../discussions` with `position[position_type]=text`, `base_sha`, `start_sha`, `head_sha`, `old_path`, `new_path`, `new_line` and `old_line` (`old_line` only for an unchanged line).
  - `ReplyDiscussion(ctx, projectID, iid, discussionID, body)` → `POST .../discussions/:id/notes`.
  - `ResolveDiscussion(ctx, projectID, iid, discussionID, resolved bool)` → `PUT .../discussions/:id?resolved=true|false`.
  - `Project` gains `OnlyAllowMergeIfAllDiscussionsAreResolved *bool` (`only_allow_merge_if_all_discussions_are_resolved`), read by project ID.
- The adapter (`gitlabforge.Forge` implements `forge.Inline`):
  - `PostThreads` creates one discussion per thread, anchored on `Line` alone. The positions use `ch.BaseSHA`, `ch.StartSHA`, `ch.HeadSHA`; when any is empty the whole call fails with an error (nothing is posted). A multi-line finding's suggestion is `suggestion:-0+N` with `N = EndLine - Line` (Task 8 builds the body). Per-thread errors go in `errs`.
  - `Threads` keeps discussions whose first note's author is the review bot and whose body has a marker; `Line` is the first note's `position.new_line`; `EndLine` is the `line_range` end when present, else `Line`.
  - `ReactThread` is `React` (award emoji works for any note).
  - `ThreadsBlockMerge` reads the project setting; the branch doesn't matter on GitLab.

- [ ] **Step 1: Write the failing tests:** each client call's method, path and query or body; `/diffs` 404 → `/changes`; `var _ forge.Inline = (*Forge)(nil)`; `TestPostThreadsPositions` (an added line has no `old_line`; an unchanged line has both); `TestPostThreadsNeedsDiffRefs` (empty `StartSHA` → error, no request); `TestThreadsOnlyBotWithMarker`; `TestThreadsBlockMerge` (true, false, missing field → false).
- [ ] **Step 2: Run** `go test ./internal/gitlab/ ./internal/forge/gitlabforge/`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(gitlab): inline review threads`.

---

### Task 8: The publisher posts and keeps the threads

**Files:**
- Create: `internal/engine/inline.go`, `internal/engine/inline_test.go`
- Modify: `internal/engine/publish.go` (call it before `policy.Decide`), `internal/engine/engine.go` (thread-rule cache), `internal/engine/fakes_test.go` (fake GitLab routes for diffs, compare, discussions, project)
- Modify: `internal/policy/policy.go` (`Outcome.InlineOn`, `Outcome.Inline`, `Outcome.FindingNotes`; `renderOptions`), `internal/policy/policy_test.go`

**Interfaces:**
- Consumes: Tasks 1, 3, 4, 5, 6, 7.
- `policy.Outcome` gains:
  ```go
  // InlineOn says findings are posted as inline threads: Inline holds the
  // fingerprints shown on the diff, FindingNotes why each other finding is in
  // the comment. Off, the comment lists every finding as before.
  InlineOn     bool
  Inline       map[string]bool
  FindingNotes map[string]string
  ```
  With `InlineOn`, `renderOptions` sets `Inline`, `FindingNote = FindingNotes`, `FindingsTitle = "Not on the diff"`, and with a mention the footer becomes ``"Run again: `@<m> review` · or reply `@<m> dismiss <reason>` in a finding's thread"``. Nothing else in `decide` reads these fields.
- In `internal/engine/inline.go`:
  ```go
  // inlineView is what the comment needs from the inline threads.
  type inlineView struct {
      On     bool
      Inline map[string]bool
      Notes  map[string]string
  }
  func (e *Engine) syncInline(ctx context.Context, plat Platform, run store.Run, repo settings.Effective, repoOK bool, o policy.Outcome, log *slog.Logger) inlineView
  ```

What `syncInline` does, in order. It returns the zero `inlineView` (inline off, the comment as before) at the first of these:
1. `!repoOK`, `!repo.InlineFindings`, `plat.Forge` doesn't implement `forge.Inline`, `o.FailureReason != ""`, `o.Result == nil`, `o.Result.Status != "completed"`, `o.Result.MergeNote != ""` (a re-run kept alone covers only some skills).
2. GitHub, and the stored project has `!InlineAvailable`.
3. `GetChange` fails, the change isn't open, or its `HeadSHA != run.HeadSHA` (Review Focus 2).
4. `ChangeDiff` or `Threads` fails (logged at warn).

Then it builds the planner input:
- **Findings** from `render.ApplyDismissals(result.ToRender(r), o.Dismissals)`: each skill's remaining findings, every text field passed through `e.redact`. `HasSuggestion` is true only when the suggestion is non-empty and `e.redact(s) == s` (Review Focus 4).
- **Dismissed**: from `o.Dismissals` (By and Reason). Make the reply safe: `policy.NeutralizeQuickActions`, and `@` → `@` + U+200D in both, then `e.redact`.
- **Skills**: `Failed` when the dismissal-applied verdict is `failed` or the skill lists not-reviewed files; `Reviewed` from `Files.Reviewed`.
- **Diff**: `diffmap.Parse` of each `FileDiff`, keyed by `Path`.
- **Threads**: the stored rows (`Store.InlineThreads`) joined with the platform's `Threads` by `ThreadID`:
  - `Resolved` comes from the platform.
  - A stored row the platform no longer has (deleted by a person) is deleted from the store, and its finding becomes a new candidate.
  - A platform thread with a marker and no row is **adopted** (Review Focus 3): stored with state `open`, the skill of the found finding with that fingerprint (else `""`), the platform's lines, and `run.HeadSHA`.
- **Map**: when `t.Commit == run.HeadSHA` it returns the stored lines. Otherwise it uses `CompareDiff(t.Commit, run.HeadSHA)`, fetched at most once per distinct commit and only when needed, then `MapOld` on the line and the end line. A failed compare, or a line that can't be mapped, is `ok == false`.
- **OnlyBlocking**: `e.threadsBlockMerge(ctx, plat, run)`, cached for 10 minutes per instance, project and target branch. An error counts as false, logged once per repository while pergent.sh runs (like `checkRequiredApprovals`).

It carries out the `Plan`, each action on its own, best effort, in this order: `Rebind`, `Resolve`, `Reopen`, `Post`.
- **Resolve** is `ReplyThread` then `ResolveThread`, then `SetInlineThreadState`.
- **Reopen** is `ResolveThread(false)` then `ReplyThread`, then the state goes back to `open` and the commit becomes `run.HeadSHA`.
- **Post** builds each body with `render.InlineComment`:
  - `SuggestionInfo` is `"suggestion"` on GitHub and `fmt.Sprintf("suggestion:-0+%d", EndLine-Line)` on GitLab, and only when `Post.Suggestion` is true.
  - `DismissCommand` is `"@" + mention + " dismiss"`, and `Skill` is the skill name when the review ran more than one skill.
  - A body over `kube.CommentLimitGitHub` or `kube.CommentLimitGitLab` isn't posted and gets `NoteTooLong`.
  - `OldLine` comes from `diffmap.File.OldLine`.
  - Each posted thread is stored (`PutInlineThread`, state `open`, commit `run.HeadSHA`).
  - A thread that failed (in `errs`, or the whole call failed) is removed from `Inline` and gets `NoteFailed`.

An action that fails is logged at warn and left for the next publish, which tries again from the platform's state. `syncInline` never returns an error.

`publish` calls it right after the dismissals are loaded and before the first `policy.Decide`, and copies `On`, `Inline` and `Notes` into `o`.

- [ ] **Step 1: Write the failing tests:**
  - policy: `TestRenderOptionsInline`: `InlineOn` with a mention gives the "Not on the diff" title, the notes and the exact footer; `InlineOn == false` leaves the options exactly as today (compare with a copy of the existing expectation).
  - engine (fake GitLab, DB-backed like the other engine tests):
    - `TestInlinePostsFindingsOnDiff`: a review with a blocking finding on a changed line and a suggestion off the diff. A discussion is POSTed with the right position and a body that ends with the marker. A row is stored. The comment says "1 finding posted as inline comments on the diff." and lists the other under "Not on the diff (1)" with `NoteOffDiff`.
    - `TestInlineSuggestionBlockGitLab`: the body has "```suggestion:-0+1" for a 2-line finding.
    - `TestInlineResolvesFixedOnNextReview`: a second review without the finding gives a reply "Fixed in `<7>`." and `PUT ...resolved=true`; the state is `fixed`.
    - `TestInlineSkippedForOldHead`: the change's head moved. No discussion request is made, and the comment lists every finding.
    - `TestInlineAdoptsUnrecordedThreads`: the platform has a marked thread of the bot and there is no row. Nothing is posted, and a row is stored.
    - `TestInlineDropsRedactedSuggestion`: the suggestion contains the token value the redactor knows, and the body has no suggestion block.
    - `TestInlineFailureKeepsPublish`: the discussions POST returns 500. The publish succeeds, the run is published, the status is set, and the finding is in the comment with `NoteFailed`.
    - `TestInlineOffBySetting`: `inline_findings: false`. No diff or discussion request is made, and the comment is as before.
    - `TestInlineDismissResolvesThread`: a dismissal plus a re-publish. Reply "Dismissed by …: …", then resolved, state `dismissed`. A restore plus a re-publish unresolves it with `ReplyRestored`.
    - `TestInlineRuleKeepsSuggestions`: the project setting is on. Only the blocking finding is posted, and the suggestion has `NoteKeptRule`.
- [ ] **Step 2: Run** `go test ./internal/policy/ ./internal/engine/ -run 'Inline|RenderOptions'`. Expected: FAIL.
- [ ] **Step 3: Implement** `inline.go`, the policy fields and the publish call.
- [ ] **Step 4: Run** `go test ./...`. Expected: PASS, including every existing publish and e2e test unchanged (inline is on by default, but the existing fake GitLab has no `/diffs` route, so `ChangeDiff` fails and inline is off; this is intended and keeps old expectations valid).
- [ ] **Step 5: Commit** `feat(engine): post findings as inline threads and keep them in step`.

---

### Task 9: Dismiss from the thread

**Files:**
- Modify: `internal/webhook/github.go` (event `pull_request_review_comment`), `internal/webhook/gitlab.go` (`noteEvent.ObjectAttributes.DiscussionID`; accept DiffNote and DiscussionNote replies), `internal/webhook/commands.go` (`comment.Discussion`, `comment.ReplyTo`, `ParseThreadCommand`)
- Modify: `internal/engine/commands.go` (in-thread dismiss, reactions and replies in the thread)
- Tests: `internal/webhook/github_test.go`, `gitlab_test.go`, `commands_test.go`; `internal/engine/commands_test.go`, `e2e_commands_test.go`

**Interfaces:**
- Consumes: `store.Command.Discussion/ReplyTo`, `InlineThreadFor` (Task 5), `forge.Inline.ReplyThread/ReactThread` (Task 6).
- `func ParseThreadCommand(body, mention string) (kind, arg, reason string, ok bool)` is the same as `ParseCommand`, except `dismiss` takes an optional ID: when the first word is a valid finding ID it is `arg`, otherwise `arg` is `""` and the whole rest of the line is the reason.

Intake rules:
- **GitHub** `pull_request_review_comment`:
  - only `action == "created"` with `in_reply_to_id != 0` (a reply in a thread);
  - fields `comment.{id, body, user{id, login, type}, in_reply_to_id, performed_via_github_app}`, `pull_request.number`, `repository.{id, full_name}`;
  - the same bot, author and `<!-- pergent` checks as `issue_comment`;
  - `ReplyTo = in_reply_to_id`.
- **GitLab** note events with `type` `DiffNote` or `DiscussionNote` and a non-empty `discussion_id`:
  - `Discussion = discussion_id`;
  - plain notes keep today's path.
- Thread comments are parsed with `ParseThreadCommand`; plain comments keep `ParseCommand`.

Handler rules (in `dismiss`, when `cmd.InThread()`):
1. `InlineThreadFor(mr, cmd.Discussion, cmd.ReplyTo)` not found → `refused("not a finding thread", replyNotFindingThread(mention))`. The reply text (exact) is ``"This thread isn't one of pergent's findings. To dismiss a finding, reply in its thread, or comment `@<m> dismiss <id> <reason>`."``
2. The finding is the latest review's finding with that thread's fingerprint (`actions.LatestFindings` + `MostSevere`). Not found → `refused("unknown finding", replyUnknownID(""))`.
3. If `arg` is set and is not that finding's ID (case-insensitive), it is part of the reason: `reason = arg + " " + reason`.
4. Then the same rules as `dismiss <id>`: write access; for blocking, maintain, not the author, and a reason.
5. Reactions on a thread command go through `ReactThread`; a reply (refusal) goes through `ReplyThread` with the thread's ref when the thread is known, else `Reply`.
6. The dismissal re-publishes as today. The publisher then replies "Dismissed by …" and resolves the thread (Task 8).

README (in this task, since it documents the event): the review App's **Subscribe to events** adds **Pull request review comment**; the Commands section says `@<bot> dismiss <reason>` works as a reply in a finding's thread.

- [ ] **Step 1: Write the failing tests:**
  - webhook: a GitHub review-comment reply with `@pergent-review dismiss false positive` stores a dismiss command with `ReplyTo` set and `Arg == ""`. A top-level review comment (no `in_reply_to_id`) is ignored. A GitLab DiffNote reply stores `Discussion`. `ParseThreadCommand` handles `dismiss abc123 because` → arg `abc123`, `dismiss because it is fine` → arg `""`, and `dismiss` with nothing → a dismiss with an empty reason.
  - engine:
    - `TestThreadDismissSuggestion`: a write-access person dismisses a suggestion in its thread. The dismissal is stored, the reaction is 👍 on the review comment via `ReactThread`, and a re-publish is queued.
    - `TestThreadDismissBlockingByAuthorRefused`: the reply in the thread uses `replyAuthor`.
    - `TestThreadDismissUnknownThread`: replies with the exact text above.
    - `TestThreadDismissArgNotIDIsReason`: `dismiss deadbe looks fine` where `deadbe` isn't the finding's ID gives the reason `deadbe looks fine`.
- [ ] **Step 2: Run** `go test ./internal/webhook/ ./internal/engine/ -run 'Thread'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: dismiss a finding by replying in its thread`.

---

### Task 10: README and upgrade notes

**Files:**
- Modify: `README.md`

Content:
- **A section "Inline findings"** saying:
  - each finding is posted on its lines, at most 20 open at once, blocking first;
  - a small fix comes as a suggestion people can apply;
  - a later push resolves a fixed finding with "Fixed in …";
  - you can reply `@<bot> dismiss <reason>` in a finding's thread;
  - findings that can't go inline stay in the review comment under "Not on the diff" with the reason;
  - when the branch requires resolved threads, only blocking findings go inline;
  - the switch is **Inline findings** in a repository's Settings, or `inline_findings` in the config.
- **Config table:** `repositories[].inline_findings` (default `true`).
- **GitHub review App permissions:**
  - Pull requests **Read and write** is needed for inline findings. With Read-only, inline is off for that instance, and the Integrations page says so.
  - Administration: Read lets pergent.sh read the classic branch protection's conversation-resolution rule. Without it, only rulesets are read and the classic rule is treated as off.
  - Events: **Pull request review comment**.
- **GitLab:** "All threads must be resolved" is read from the project settings, and no new webhook is needed (note events already arrive).
- **Upgrading to 0.6.0:**
  1. Migration `0017` runs at startup. It adds a table and nullable columns only, so 0.5.4 keeps working on a migrated database.
  2. Inline findings are on by default. Turn them off per repository (Settings → Inline findings, or `inline_findings: false`) to keep the single comment.
  3. GitHub: give the review App Pull requests **Read and write** and subscribe it to **Pull request review comment**. Without them, findings stay in the comment and in-thread dismissals are not received.
  4. The default `jobs.image` is `ghcr.io/gopaytech/pergent:v0.4.0`.
  5. To roll back to 0.5.4, first remove `inline_findings` from the config. 0.5.4 refuses unknown config keys. Threads already posted stay on the platform.

- [ ] **Step 1:** Write the sections above, matching the README's voice (short sentences, bold lead-ins).
- [ ] **Step 2:** scan everything the branch changed, except this plan (which names the words to look for), for employer names and internal hostnames: `git diff main --name-only -- . ':!docs/superpowers/plans/2026-11-04-inline-threads.md' | xargs grep -inE "golabs|gojek|gopay[^t]|goto"`. Expected: no output.
- [ ] **Step 3: Run** `go test ./... && cd web && npm test && npm run build`. Expected: PASS.
- [ ] **Step 4: Commit** `docs: inline findings`.

---

## After the tasks

Run the whole-branch review. Then ask the owner whether to push and open the pull request. The pergent.sh 0.6.0 release (chart 0.6.0, tag, GitHub release) and the community-ops bump are separate steps, each asked for first.
