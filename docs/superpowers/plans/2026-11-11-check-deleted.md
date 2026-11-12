# Reviewing Deleted and Renamed Files Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** With an opt-in switch, pergent reviews a deleted file's removed lines, checks what still references deleted files and renamed files' old paths, and counts those files as reviewed; pergent.sh gets a per-repository switch and shows the results.

**Architecture:** Part A changes pergent on branch `feat/check-deleted` (worktree `pergent-check-deleted`), on top of its 3 reviewed commits. With the switch on, a deleted file falls through the `deleted` skip into normal batches. Each batch checks references for the deleted files it carries. Pure renames and unbatchable deletions go on one list on the skill's first run. Results gain additive `files.deleted` and `files.renamed`, and `render` marks them. Part B changes pergent.sh (branch from `origin/main` in its own worktree). It adds a repository setting passed to the Job as `PERGENT_CHECK_DELETED=true`, reads the new result fields, and relaxes the incremental carry rule.

**Tech Stack:** Go (both repos), Postgres (pergent.sh, pgx), React + TypeScript + vitest (pergent.sh web), OpenAPI.

**Spec:** `docs/superpowers/specs/2026-11-11-check-deleted-design.md` (pergent.sh repo). Code map with file:line for every touch point: `/private/tmp/claude-501/-Users-zufardhiyaulhaq-Documents-personal-github-pergent/00c2dbe3-c1fe-40e3-955f-c7e64f6c1850/scratchpad/check-deleted-code-map.md`.

## Global Constraints

- Public repositories. No private host, company or group name anywhere. Before every commit, `git diff --cached | grep -i -E 'golabs|gojek|gopay[^t]'` must print nothing.
- Never run `docker`, `orbctl` or anything that starts a container. Never call `gh`, `glab`, `curl` or any external API. No push, no PR, no tag, **no release and no version bump**: releases are the owner's call.
- **Off is byte for byte today's behaviour** (prompts, runs, result JSON, comment). Every pergent golden file stays unchanged with the switch off.
- Switch: `--check-deleted` (bool, default **false**) and `PERGENT_CHECK_DELETED`, flag over env over default. A bad env value is a config error.
- The result stays `schema_version: 1`. New fields are additive and `omitempty`.
- Never count as reviewed an entry no finished run saw. A failed or unstarted run that carries deleted files or the list fails the skill.
- A deletion that can't be batched keeps rule `deleted`: binary, empty (no hunk), or `NotReviewed` (the platform sent no content).
- Vendored, lockfile and generated deletions stay skipped under their own rule. A deleted file's generated header is read from its removed lines.
- Rendering: a finding whose file is a reviewed deleted file shows as `path (deleted), line N` (or `path (deleted), lines N–M`) with no link. The Files list marks `(deleted)` and `(renamed from old/path)`.
- pergent.sh: the setting is per repository only (no instance default). Maintainers and admins change it. Config key `repositories[].check_deleted`. Migration `0038_check_deleted.sql` (nullable column). It is passed as an environment variable, never a flag.
- Copy: setting name `Review deleted and renamed files`. Help text: `Reviews the lines a change deletes, and checks what still uses deleted files and renamed files' old paths. Costs more tokens. Needs a pergent image newer than v0.9.0; an older one ignores it.`
- Gates. pergent: `make build && go vet ./... && make test && gofmt -l .` (empty). pergent.sh: `go vet ./...` and `go test -count=1 ./...` with `PERGENT_SH_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable` (already running; if it doesn't answer, stop and report). Web: `cd web && npm run typecheck && npm run lint && npm test -- --run && npm run build`. Symlink `web/node_modules` to `../../pergent.sh/web/node_modules` first, and remove the link, `web/tsconfig.tsbuildinfo` and the build output afterwards.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- **Ruling:** a pure rename past the list's 200-entry cap keeps `no content changes` and gets a skill note (`N renamed files were not checked for references.`). Pure renames never blocked approval, so this is no weaker than today. Deletions past the cap keep `deleted` and block, as the spec says.
- **Ruling:** pergent.sh pins the pergent Go module to a pseudo-version of pergent `master` after Part A merges (for `render`). `defaultImage` stays `v0.9.0` until the owner releases pergent. Until then the env var reaches an image that ignores it, which is harmless.

## Review Focus

1. **A rename whose old path is re-created in the same change** (`a.go` renamed to `b.go`, plus a new `a.go`). `a.go` exists at head, so it must not go on the reference list. Pinned by `TestRenameOldPathRecreatedIsNotListed` (Task A3).
2. **Odd path names in the new marks.** A path with a backtick, `|`, `<` or a newline must render inside the comment's escaping, as other paths do. Pinned by `TestDeletedMarksEscapePaths` (Task A4).
3. **One deleted file, two skills:** in scope for one, out of scope for the other. The Files list says reviewed by one and skipped by the other. `files.deleted` lists it only for the reviewing skill. Pinned by `TestDeletedFileTwoSkills` (Task A3) and the multi-skill golden case (Task A4).
4. **A file deleted in one push and restored in the next:** the re-review treats it as a normal file. No stale `(deleted)` and no carried "reviewed deletion". Pinned by `TestRestoredFileIsNotDeleted` (Task B4).
5. **An older pergent result with no `files.deleted`/`files.renamed`:** it parses, renders with no marks, and the carry rule behaves as today. Pinned by `TestOldResultHasNoDeletedFields` (Task B3).

---

## Part A: pergent (worktree `/Users/zufardhiyaulhaq/Documents/personal/github/pergent-check-deleted`)

### Task A1: Switch off by default, drop the leftover tests

**Files:**
- Modify: `cmd/main.go:68`, `cmd/agent.go:330`, `internal/config/resolver.go:71-73,215`, `internal/review/review.go:60-65` (comment), `internal/config/checkdeleted_test.go`, `cmd/checkdeleted_test.go`
- Revert: the uncommitted `internal/review/deleted_test.go` changes (`git checkout -- internal/review/deleted_test.go`)

**Interfaces:**
- Produces: `config.Config.CheckDeleted` false unless the flag or `PERGENT_CHECK_DELETED=true`. The flag help is `Review deleted files and check what still uses deleted and renamed files (off by default)`.

- [ ] **Step 1:** Revert the uncommitted test file and confirm `git status` is clean.
- [ ] **Step 2: Failing tests.** In `TestResolve_CheckDeleted`, no flag and no env gives `false`, env `true` gives `true`, the flag `--check-deleted` gives `true`, and an explicit `--check-deleted=false` with env `true` gives `false`. In `TestParseAgent_CheckDeleted` the default is `false`. `TestMain_CheckDeletedInPlatformMode` sets the env to `true` explicitly.
- [ ] **Step 3:** Run `go test ./internal/config ./cmd -run CheckDeleted`. It should FAIL, because the default is still true.
- [ ] **Step 4:** Flip the three defaults to `false` and update the comments.
- [ ] **Step 5:** Run `go test ./internal/config ./cmd ./internal/review`. PASS. Any review test that relied on the default sets `CheckDeleted` explicitly (the `deletedReq` helper already does).
- [ ] **Step 6:** Commit `feat: --check-deleted is off by default`.

### Task A2: The classifier lets an on-switch deletion through

**Files:**
- Modify: `internal/diff/classify.go:14-112`, `internal/diff/classify_test.go`

**Interfaces:**
- Produces: `diff.Classifier.ReviewDeleted bool`. When it is true, `Exclusion` returns `"deleted", false` only for a deleted file that is binary, has no hunk, or has `NotReviewed` set. Any other deleted file goes through the path rules (lockfile/vendored/generated by `.gitattributes`, opt-in) and then the generated-header test on its removed lines. When it is false, `Exclusion` is unchanged.
- `scanPatch` reads removed lines of a hunk whose **old** start is 1 when the hunk's new side is `+0,0`, for the generated-header test only.

- [ ] **Step 1: Failing tests** in `TestExclusion` (new cases with `ReviewDeleted: true`):
  - a deleted text file gives `""`;
  - a deleted binary gives `deleted`;
  - a deleted empty file (no hunk) gives `deleted`;
  - a deleted file with `NotReviewed` gives `deleted`;
  - a deleted `vendor/x.go` gives `vendored` (opt-in);
  - a deleted `go.sum` gives `lockfile`;
  - a deleted file whose removed line 1 is `// Code generated by x. DO NOT EDIT.` gives `generated`, but only when `generatedAtBase` is nil or true for that path.

  Every existing case still passes with `ReviewDeleted` false.
- [ ] **Step 2:** Run `go test ./internal/diff -run TestExclusion`. It should FAIL.
- [ ] **Step 3:** Implement the field and both rules.
- [ ] **Step 4:** Run `go test ./internal/diff`. PASS.
- [ ] **Step 5:** Commit `feat(diff): with --check-deleted, a deleted file is batched unless it can't be`.

### Task A3: Review deleted files, check references, count what was seen

**Files:**
- Modify: `internal/review/review.go` (`Run` sets `classifier.ReviewDeleted = req.CheckDeleted`; `reviewSkill`; `reviewBatch`), `internal/review/deleted.go`, `internal/review/select.go` if needed, `internal/review/verify.go`
- Test: `internal/review/deleted_test.go`

**Interfaces:**
- Consumes: `Classifier.ReviewDeleted` (A2).
- Produces, on `review.SkillResult`:
  - `Deleted []string`: reviewed deleted paths, sorted.
  - `Renamed []Rename`: pure renames whose old path a finished run saw, sorted by Path.
  - `type Rename struct{ Path, From string }`.
- `deletedPaths(s)` becomes `func (r *reviewer) referenceList(s skill.Skill, batched map[string]bool) (lines []string, renamedIn []Rename)`. It returns:
  - the skill's pure renames in scope (old **or** new path decided by `scope.Decide`), leaving out any whose old path still exists at head, i.e. is a `Path` of another `FileDiff`, and any whose old path a path rule skips;
  - the skill's deletions that stayed `deleted` (unbatchable).

  Line format: a deletion is `strconv.Quote(path)`. A rename is `strconv.Quote(old) + " renamed to " + strconv.Quote(new)`. Cap: `maxDeleted` (200), then `... and N more`.
- Batches: each batch whose files include deleted files gets a sentence naming those files (quoted) as deleted. The sentence asks whether removing them is safe and asks for a search for remaining references to them. Batch 1 also carries `deleted-files.md` with the reference list when it is non-empty. With no batch, the existing no-diff run carries it.
- What counts:
  - a deleted file in a finished batch goes to `Reviewed` and `Deleted`;
  - a pure rename whose list line a finished run carried is removed from `Skipped` (`no content changes`) and goes to `Reviewed` and `Renamed`;
  - a pure rename past the cap stays skipped and adds the skill note `N renamed files were not checked for references.`;
  - an unbatchable deletion stays skipped as `deleted` whether or not the list run finishes.

- [ ] **Step 1: Failing tests** (`deletedReq` with `CheckDeleted: true`, the `answering` fake):
  - `TestDeletedFileIsBatchedAndReviewed`: one modified and one deleted file in scope. The batch's `diff.patch` holds the deleted file's `-` lines, the message names it as deleted, `Reviewed` holds both, `Deleted == ["old.go"]`, and `Skipped` has no `deleted` entry.
  - `TestUnbatchableDeletionStaysSkipped`: a deleted binary is skipped as `deleted`, sits on the list in `deleted-files.md`, and is not in `Deleted`.
  - `TestPureRenameIsReviewedWhenListed`: a rename with no content gives one no-diff run whose list has `"a.go" renamed to "b.go"`. Then `Reviewed == ["b.go"]`, `Renamed == [{b.go a.go}]`, and `Skipped` is empty.
  - `TestRenameScopedByOldOrNewPath`: with `paths: ["pkg/"]`, `pkg/a.go` renamed to `lib/a.go` is listed, and so is `lib/b.go` renamed to `pkg/b.go`.
  - `TestRenameOldPathRecreatedIsNotListed` (Review Focus 1).
  - `TestDeletedFileTwoSkills` (Review Focus 3).
  - `TestRenamePastCapStaysSkipped`: 201 pure renames give 200 in `Renamed`, 1 skipped as `no content changes`, and the note `1 renamed files were not checked for references.`
  - `TestFailedBatchWithDeletedFileFailsSkill`: verdict `failed`, the deleted file in the failure's `Files`, `Deleted` empty.
  - `TestOversizeDeletedFileNotReviewed`: a deleted file over the batch budget gives the existing `Not reviewed:` failure naming it, verdict `failed`, `Deleted` empty.
  - `TestCheckOffIsUnchanged`: the same inputs with `CheckDeleted: false` give the master-identical `Skipped` (`deleted`), messages and attachments.
- [ ] **Step 2:** Run `go test ./internal/review -run 'Deleted|Rename|CheckOff'`. It should FAIL.
- [ ] **Step 3:** Implement the above. Keep the branch's carve-outs, `takeDeletedOnly` failure handling and the verification attachment. Replace their deleted-only list with `referenceList`.
- [ ] **Step 4:** Run `go test -race ./internal/review` and `make test`. PASS, goldens unchanged.
- [ ] **Step 5:** Commit `feat(review): review deleted files and check references to deleted and renamed paths`.

### Task A4: Render the marks, carry them through output and the result

**Files:**
- Modify: `render/render.go` (`Skill`), `render/links.go` or `render/layout.go` (`locationHTML`), `render/details.go` (`fileLine`/`writeFiles`), `internal/output/formatter.go:141` (`toRender`), `internal/result/result.go:113-130,212` (`Files`, `buildSkill`)
- Test: `render/deleted_test.go` (new), `render/testdata/golden/` (new case only), `internal/output/golden_cases_test.go` (new case), `internal/result/result_test.go`

**Interfaces:**
- Produces:
  - `render.Skill.Deleted []string`, `render.Skill.Renamed []render.Rename`, `type Rename struct{ Path, From string }`.
  - `result.Files.Deleted []string \`json:"deleted,omitempty"\``, `result.Files.Renamed []Renamed \`json:"renamed,omitempty"\``, `type Renamed struct{ Path string \`json:"path"\`; From string \`json:"from"\` }`.
- A finding whose `File` is in any skill's `Deleted` renders as `<code>path (deleted), line N</code>`, or `lines N–M`, or just `path (deleted)` with no line. It never gets a link, even when `FileURL` is set. In the Files list, a path in `Deleted` gets ` (deleted)` after its code span, and a renamed path gets ` (renamed from <code>old</code>)`. All text goes through the existing escaping helpers.

- [ ] **Step 1: Failing tests:**
  - `TestDeletedFindingHasNoLink` (with `GitHubFileURL` set, the finding on `old.go` line 3 shows `old.go (deleted), line 3` and no `<a`);
  - `TestFilesListMarks`;
  - `TestDeletedMarksEscapePaths` (Review Focus 2);
  - `TestBuildSkillDeletedFields` (JSON has `deleted` and `renamed` only when set, and `schema_version` stays 1);
  - a new golden case `deleted-and-renamed` (two skills, one deleted file reviewed by one skill, one pure rename) created with `-update-golden-inputs`/`-update-goldens`.

  `go test ./render ./internal/output` must show **no change to existing golden files**.
- [ ] **Step 2:** Run `go test ./render ./internal/output ./internal/result`. It should FAIL.
- [ ] **Step 3:** Implement, and copy redacted `Deleted`/`Renamed` in `toRender` and `buildSkill`.
- [ ] **Step 4:** Run `make test` and `git status render/testdata` (only the new case's files are added). PASS.
- [ ] **Step 5:** Commit `feat(render): mark deleted and renamed files, and don't link a finding on a deleted file`.

### Task A5: pergent docs

**Files:**
- Modify: `docs/concepts/deleted-files.md` (rewrite), `docs/concepts/skills.md:64`, `docs/concepts/review-comment.md:57`, `docs/concepts/agent-mode.md:46`, `docs/concepts/failed-reviews.md:3`, `docs/reference/configuration.md:18,50`, `docs/agent-result-schema.md:185-234` (`files.deleted`, `files.renamed`, the rewritten line 232 paragraph, the rule table), `README.md:174`, `CLAUDE.md` (the deleted-files bullet and the `internal/diff`, `render`, `internal/result` bullets)

- [ ] **Step 1:** Write the docs so they describe the code as of A4: off by default, deleted files batched, references per batch plus the list, what counts as reviewed, the cap ruling, the marks, `files.deleted`/`files.renamed`, and a schema that stays v1. Plain English, no em dashes.
- [ ] **Step 2:** Check the docs. `grep -rn "default true\|on by default" docs README.md CLAUDE.md | grep -i delet` must print nothing. Every flag, field and rule named in the docs must exist in the code.
- [ ] **Step 3:** Run the full gate and the name scan over `git diff origin/master...`.
- [ ] **Step 4:** Commit `docs: reviewing deleted and renamed files`.

---

## Part B: pergent.sh (after Part A's PR merges; new worktree from `origin/main`, branch `feat/check-deleted`)

### Task B1: The repository setting

**Files:**
- Create: `internal/store/migrations/0038_check_deleted.sql` (`ALTER TABLE dashboard_repos ADD COLUMN check_deleted boolean;`)
- Modify: `internal/config/config.go:232,325,356` (`Repository.CheckDeleted *bool`, `fileRepository` `yaml:"check_deleted"`), `internal/store/dashboard.go` (`DashboardRepo.CheckDeleted *bool` in all four SQL statements and argument lists), `internal/settings/settings.go` (`KeyCheckDeleted = "check_deleted"`, `Effective.CheckDeleted bool`, `defaultsFor` Overridden entry, `fromConfig`, `fromDashboard`), `internal/dashboard/api_read.go:949` (`"checkDeleted"`), `internal/dashboard/api_write.go:686,767,807` (bool setting, reset), `openapi.yaml:1209,3453,4393`, `web/src/api.ts:263,517`, `web/src/pages/RepositorySettings.tsx` (row after Quiet reviews, the copy from Global Constraints)
- Test: `internal/config/config_test.go` (`TestCheckDeletedConfigKey`), `internal/settings/settings_test.go` (`TestResolverCheckDeletedConfig`, `...Dashboard`), `internal/dashboard/api_write_test.go` (`TestCheckDeletedSetting`: maintainer 200, viewer 403, config repo 409, reset clears), `internal/dashboard/api_read_test.go` (`TestSettingViewsCheckDeleted`), `web/src/pages/RepositorySettings.test.tsx` (the row toggles and sends `checkDeleted`)

**Interfaces:**
- Produces: `settings.Effective.CheckDeleted bool` (false unless set). JSON key `checkDeleted`.

- [ ] **Step 1:** Write the failing tests above, copying the `QuietReviews` tests' shapes.
- [ ] **Step 2:** Run `go test -count=1 ./internal/config ./internal/settings ./internal/dashboard -run CheckDeleted` with the DB variable. It should FAIL.
- [ ] **Step 3:** Implement, following `quiet_reviews` end to end and skipping the instance-defaults path.
- [ ] **Step 4:** Run the Go gate and the web gate. PASS. The OpenAPI tests cover the schema.
- [ ] **Step 5:** Commit `feat: a repository setting to review deleted and renamed files`.

### Task B2: Pass it to the Job

**Files:**
- Modify: `internal/kube/job.go:72-99,279-292` (`JobParams.CheckDeleted bool`; append `PERGENT_CHECK_DELETED=true` only when true, with the same "an environment variable, not an argument" comment as dismissals), `internal/engine/dispatch.go:566-592` (`CheckDeleted: repo.CheckDeleted`)
- Test: `internal/kube/job_test.go` (`TestCheckDeletedEnv`: set when true, absent when false), `internal/engine/dispatch_test.go` (`TestDispatchPassesCheckDeleted` with a `checkDeletedEnv` helper copied from `dismissalsEnv`)

- [ ] **Step 1:** Write the failing tests.
- [ ] **Step 2:** Run `go test -count=1 ./internal/kube ./internal/engine -run CheckDeleted`. It should FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the Go gate. PASS.
- [ ] **Step 5:** Commit `feat(engine): pass the deleted-files switch to the review Job`.

### Task B3: Read the new fields and render with them

**Files:**
- Modify: `go.mod`/`go.sum` (`go get github.com/gopaytech/pergent@<Part A merge commit on master>`), `internal/result/result.go:157-162,220,582` (`Files.Deleted`, `Files.Renamed`, `ToRender` copies them into `render.Skill`, `MergeIncremental` keeps them for carried skills)
- Test: `internal/result/result_test.go` (`TestParseDeletedFields`, `TestOldResultHasNoDeletedFields` (Review Focus 5), `TestMergeKeepsDeletedFields`), `internal/policy/policy_test.go` (`TestReviewedDeletionApproves`: a skill whose only file is in `Reviewed` and `Deleted` is approved; `TestSkippedDeletionStillBlocks`), `internal/engine/inline_test.go` (`TestNoThreadOnDeletedFile`: already NoteOffDiff, so this pins it)

**Interfaces:**
- Consumes: `render.Skill.Deleted`, `render.Skill.Renamed`, `render.Rename` (A4).
- Produces: `result.Files.Deleted []string`, `result.Files.Renamed []Renamed` (mirrors of pergent's JSON).

- [ ] **Step 1:** Write the failing tests. The policy tests need no policy change (`blocksApproval` is unchanged); they pin it.
- [ ] **Step 2:** Run `go test -count=1 ./internal/result ./internal/policy ./internal/engine -run 'Deleted|NoThread'`. It should FAIL.
- [ ] **Step 3:** Implement. `defaultImage` stays `v0.9.0` (Global Constraints ruling).
- [ ] **Step 4:** Run the Go gate. PASS.
- [ ] **Step 5:** Commit `feat(result): read files.deleted and files.renamed`.

### Task B4: The carry rule and skill results

**Files:**
- Modify: `internal/engine/incremental.go:199-215` (carriedFiles), `internal/fates/fates.go:316-360` and `internal/engine/fates.go:283-295` (feed `Deleted`)
- Test: `internal/engine/incremental_test.go`, `internal/fates/fates_test.go`

**Interfaces:**
- Consumes: `result.Files.Deleted`, `result.Files.Renamed`.
- carriedFiles: a finding whose `File` is not in `reviewed` no longer sets `inside = false` when the skill's `Files.Deleted` or `Files.Renamed` is non-empty and the finding's `File` is not itself a deleted path. Such a finding is a reference finding. An open blocking reference finding still blocks carrying of its own file through the existing `blocked` set. Dismissed ones never do.
- fates: `fates.Skill.Deleted map[string]bool`. A finding on a reviewed deleted file that is not reported again becomes `Fixed` only if the file is still deleted at the new head. A finding on a file that was restored is tracked as a normal file.

- [ ] **Step 1: Failing tests:**
  - `TestCarryContinuesAfterReferenceFinding` (open and dismissed variants);
  - `TestCarryStopsForOtherOutsideFinding` (no deleted or renamed paths: today's behaviour);
  - `TestRestoredFileIsNotDeleted` (Review Focus 4: push 1 deletes `x.go` and reviews it, push 2 restores it, so `x.go` is reviewed again as a normal file and no carried skill lists it in `Deleted`);
  - `TestFateOfFindingOnDeletedFile` (still deleted gives `Fixed`, restored and changed gives normal tracking).
- [ ] **Step 2:** Run `go test -count=1 ./internal/engine ./internal/fates -run 'Carry|Restored|DeletedFile'`. It should FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the Go gate. PASS.
- [ ] **Step 5:** Commit `feat(engine): a reference finding doesn't stop incremental carrying`.

### Task B5: pergent.sh docs

**Files:**
- Create: `docs/concepts/deleted-files.md`
- Modify: `docs/README.md` (Concepts line), `docs/reference/configuration.md:50-55` (`repositories[].check_deleted`), `docs/concepts/dashboard.md:208` (settings list), `docs/concepts/approval.md:12` (a reviewed deletion no longer blocks; one skipped as `deleted` still does), `docs/concepts/incremental-reviews.md` (the carry rule), `docs/upgrading.md` (new top section `## Upgrading to the next release`: the setting is off by default, migration `0038` only adds a column, needs a pergent image newer than v0.9.0, and to roll back remove `check_deleted` from the config first)

- [ ] **Step 1:** Write the docs to match B1–B4. Plain English, no em dashes, and quote the setting's copy exactly.
- [ ] **Step 2:** Run both gates and the name scan over `git diff origin/main...` (the plan file's own quoted regex is the known false positive).
- [ ] **Step 3:** Commit `docs: reviewing deleted and renamed files`.
