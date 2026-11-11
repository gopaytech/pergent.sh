# Reviewing deleted and renamed files

Status: design, agreed with the owner on 2026-11-11 after two rounds of questions. Covers a pergent change and a pergent.sh change. Nothing is built from this until the owner approves it, and nothing is released until the owner decides.

## Why

pergent skips a deleted file (skip rule `deleted`). Nothing reads it, and nothing checks what still uses it. Three things go wrong (the owner's words, in order):

- **Broken references.** Code, config, docs or scripts that remain still use the deleted file, and nobody notices unless that other file is also in the diff.
- **What was removed.** The deletion itself can be the risk: a removed auth check, validation, test or migration should be judged.
- **Silent skip.** The review reads as clean while part of the change was never looked at. pergent.sh does not approve such a change, but the comment does not make the gap obvious.

A rename has the first problem too: its old path can still be referenced.

## Goals

- With the switch on, a deleted file is **reviewed like any changed file**: its removed lines go into a batch, and the model judges whether removing them is safe.
- With the switch on, each skill also **checks what still references** its deleted files and the old paths of its renamed files, and raises a finding on the remaining line.
- With the switch on, a deleted file that was reviewed, and a pure rename whose old path was checked, **count as reviewed**, so pergent.sh can approve a clean change.
- **Off by default**, in pergent and in pergent.sh. Off is exactly today's behaviour.

## Non-goals

- Reviewing vendored, lockfile or generated deletions. They stay skipped under their own rules, as a changed vendored file does today. A skill's `paths` can opt them in, as today.
- A size limit of its own. Deleted files use the normal batch budget.
- Changing the default in a later release. That is a separate decision once it has run for a while.
- An instance-wide default in pergent.sh. Add one only if turning it on repository by repository becomes tedious.
- Inline threads on removed lines. A later change can post them on the removed side.
- A token cost estimate. The token quota and Usage pages show the real cost once someone turns it on.

## Anti-goals

- **A false pass.** A deleted file counts as reviewed only when a run actually read it. A run that failed, never started, or a file too big for any batch leaves it not reviewed, and the skill fails or the file is listed as not reviewed, as for any other file today. A batch with deleted files that fails fails the skill, so a delete-heavy change can turn red when the model run fails.
- **Breaking an older pergent image.** pergent.sh may run a pergent that predates the switch. That must still review, unchanged.
- **Model text as instructions.** Deleted paths come from the change. They are quoted data, never instructions.

## Behaviour

### pergent

**The switch.** `--check-deleted` (bool, default **false**) and `PERGENT_CHECK_DELETED`, flags over env over default, in CI mode and agent mode. A bad env value is a config error, as for `PERGENT_USE_OPENCODE_CONFIG`.

**With it on:**

1. **Deleted files are batched like changed files.** A deleted file in a skill's scope (its `paths`/`exclude`) that no other default rule skips (lockfile, vendored, generated) gets no `deleted` skip. Its patch, made of removed lines only, goes into the skill's batches under the normal budget. The prompt tells the model the file is deleted, so it judges whether removing that code is safe. A deleted file too big for a batch is not reviewed, as an oversize changed file is today.
2. **Each batch checks references for the deleted files it carries.** The batch is told to search the repository for anything that still uses those files and to raise a finding on the remaining line, if it matters under the skill's instructions.
3. **One list run for the rest.** Pure renames, and deletions in a skill that has no batch, go on one list for the skill, capped at 200 entries, in a single run with the same instruction. A rename is in scope when its old or its new path is. Its line reads `"old" renamed to "new"`, so a finding can suggest the new path. This run covers a change that only deletes or only renames, and a re-review where everything else is carried. A failed or unstarted run fails the skill.
4. **What counts as reviewed:**
   - a deleted file whose batch finished;
   - a pure rename (no content change), or a deletion on the list, that a finished list run saw.

   They go to `files.reviewed` and not to `files.skipped`. A rename with content changes is reviewed as today. An entry past the list's cap was not seen, so it stays skipped as `deleted` and blocks approval.
5. **A deletion that can't be batched keeps the `deleted` rule**: a binary file, an empty file, or a file the platform sent without content (GitLab can return an empty `diff`). That blocks approval as today, even after the reference check, so a deleted image still referenced by HTML needs a person. Falling through to `binary` or `no content changes` is not allowed, because `no content changes` doesn't block approval and that would be a silent pass.
6. **Generated deletions.** The generated-header test reads only added and context lines today (`internal/diff/classify.go:104-106`). For a deleted file it must read the removed lines, so a deleted file marked generated by its own header stays skipped as `generated`.
7. **The confidence cap** counts only `files.skipped` entries (`render/confidence.go`), so a reviewed deleted file drops out of it without new code.
8. **The result stays schema v1.** The switch is opt-in, and a new additive field names the reviewed files that are deleted (for example `files.deleted`). The schema doc changes its wording: `files.reviewed` holds no deleted file unless that field lists it. pergent.sh reads the field. An older reader ignores it.
9. **Rendering, in `render`, shared by pergent's own CI comment and pergent.sh:**
   - A finding on a removed line shows as `path (deleted), line N`, with no link and no inline thread.
   - The Files list marks a reviewed deleted file `(deleted)` and a pure rename `(renamed from old/path)`.

**With it off:** byte for byte today's prompts, runs, result and comment. Deleted files are skipped as `deleted`.

### pergent.sh

- **A repository setting**, "Review deleted and renamed files", off by default. Maintainers and admins change it, like the other repository settings. Config key `repositories[].check_deleted`. A config value wins and shows read-only. Reset to defaults turns it off. The setting shows even when a repository runs an older custom pergent image, and its help text names the minimum pergent version.
- **Passed to the Job as `PERGENT_CHECK_DELETED=true`**, only when the setting is on. pergent.sh already passes newer options as environment variables (`PERGENT_DISMISSALS`, `PERGENT_LINKED_REPOS`), so a pergent too old to know it ignores it and reviews as today.
- **Approval needs no rule change.** `policy.blocksApproval` blocks on a `deleted` skip. With the switch on, a reviewed deletion is no longer skipped, so it no longer blocks. With an older image it is still skipped and still blocks. A pure rename never blocked (`no content changes` is non-blocking). What stops a rename-only change today is "nothing reviewed", so the rename part of Goal 3 matters only for changes where nothing else was reviewed.
- **The pergent pin** (`go.mod` and `defaultImage`) moves to the pergent version that has the switch, in the same pergent.sh change.
- **Findings on deleted files.** pergent.sh renders the comment itself (`render.Comment` in `internal/policy/policy.go`) with its own `FileURL` to the head commit (`internal/engine/publish.go`). A finding on a deleted file gets no head link and no inline thread. Inline threads today are right side only on GitHub (`internal/github/client.go`) and `new_line` on GitLab (`internal/gitlab/client.go`).
- **Incremental re-reviews.** Today pergent.sh carries nothing for a skill with a finding outside its reviewed files, dismissed findings included (`internal/engine/incremental.go`). A reference finding is outside by design, so a finding that points at a remaining reference to a deleted or renamed file, open or dismissed, no longer stops carrying.
- **Skill results.** `internal/fates/fates.go` assumes a deleted file is never reviewed. It must handle one that is.

## Edge cases

- **A deleted file in the Files list.** The list has no links today (`render/details.go`); it only gains the `(deleted)` mark.
- **Incremental re-reviews.** A file deleted in an earlier push and still deleted is carried like any reviewed file. A file deleted only in the newest push is reviewed in that push.
- **A huge deleted folder.** Its files go through batches like any other files. Vendored ones stay skipped. The token cost is real, and it is why the switch is off by default.
- **Dismissals and fingerprints.** A finding about removed code is a normal finding, so dismissing it works as today.

## Upgrade

- pergent: a new flag, off by default. Nothing changes for anyone who doesn't set it.
- pergent.sh: a new repository setting, off by default, and a migration that adds its column. Rolling back removes the setting. If `check_deleted` is set in the config, remove it before rolling back, because the previous release refuses an unknown key.

## Delivery

1. **pergent:** build on the 3 reviewed commits of `feat/check-deleted`. They already have the reference check, failure handling, the list, the carve-outs and their tests. The uncommitted rename tests a stopped run left in that checkout are dropped: they were never reviewed, and one expects a pure rename to stay skipped. Change: default off, batch deleted files as files, references per batch, the list run, renames, what counts as reviewed, the generated-header fix, `files.deleted`, and the rendering in `render`.
2. **pergent.sh:** after the pergent change merges: the setting, the env var, the pin move, result reading (`files.deleted`), findings on deleted files in publish, the carry rule, skill results, and docs.
3. **Releases are the owner's call.** No tag, release or version bump is part of this work until the owner says so.

## Testing

- pergent: off is byte-identical (golden files unchanged), on batches a deleted file's removed lines and reviews it, a batch checks references for its own deleted files, renames' old paths are on the list, scope by old or new path, vendored/lockfile/generated deletions stay skipped, reviewed and skipped lists and the confidence cap with the switch on, a failed or unstarted run fails the skill, a failed batch with deleted files fails the skill, a too-big deleted file is not reviewed, a delete-only or rename-only change, an incremental re-review, an entry past the list's cap not counted as reviewed, a deleted binary or empty file keeping `deleted`, a deleted file with a generated header staying `generated`, `files.deleted` present only with the switch on, the `(deleted)` and `(renamed from …)` marks, a finding on a removed line with no link.
- pergent.sh: the setting's access (maintainer yes, viewer no), config wins, the env var set only when on, approval of a clean deletion with the switch on, a deletion still blocking with it off, an old image ignoring the var, a finding on a deleted file with no head link and no inline thread, carrying continuing after an open or dismissed reference finding, skill results with a reviewed deleted file.

## Left to the builder

- How pergent.sh recognises a reference finding for the carry rule (for example, its file is not in the diff while the skill's result lists deleted or renamed paths).
- How a finding marks that its line is on a deleted file (for example, its file is in `files.deleted`).
- Wording of the prompt sentences and the setting's help text.
