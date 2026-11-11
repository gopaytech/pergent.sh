# Reviewing deleted and renamed files

Status: design, agreed with the owner on 2026-11-11. Covers a pergent change and a pergent.sh change. Nothing is built from this until the owner approves it.

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

## Anti-goals

- **A false pass.** A deleted file counts as reviewed only when a run actually read it. A run that failed, never started, or a file too big for any batch leaves it not reviewed, and the skill fails or the file is listed as not reviewed, as for any other file today.
- **Breaking an older pergent image.** pergent.sh may run a pergent that predates the switch. That must still review, unchanged.
- **Model text as instructions.** Deleted paths come from the change. They are quoted data, never instructions.

## Behaviour

### pergent

**The switch.** `--check-deleted` (bool, default **false**) and `PERGENT_CHECK_DELETED`, flags over env over default, in CI mode and agent mode. A bad env value is a config error, as for `PERGENT_USE_OPENCODE_CONFIG`.

**With it on:**

1. **Deleted files are batched like changed files.** A deleted file in a skill's scope (its `paths`/`exclude`) that no other default rule skips (lockfile, vendored, generated) gets no `deleted` skip. Its patch, made of removed lines only, goes into the skill's batches under the normal budget. The prompt tells the model the file is deleted, so it judges whether removing that code is safe. A deleted file too big for a batch is not reviewed, as an oversize changed file is today.
2. **One reference check per skill.** One run per skill carries the list of the skill's deleted paths and renamed old paths, with an instruction to search the repository for anything that still uses them and to raise a finding on the remaining line. The existing branch attaches the list to the first batch and the verification run. A rename is in scope when its old or its new path is. Its line reads `"old" renamed to "new"`, so a finding can suggest the new path. The list is capped (200 lines on the branch).
3. **A change with no batch for a skill but with deletions or renames in its scope** gets one run for the reference check. This covers a change that only deletes or only renames, and a re-review where everything else is carried. A failed or unstarted run fails the skill.
4. **What counts as reviewed:**
   - a deleted file whose batch finished **and** whose path a finished reference check saw;
   - a pure rename (no content change) whose old path a finished reference check saw.

   They go to `files.reviewed` and not to `files.skipped`. A rename with content changes is reviewed as today. An entry past the reference list's cap was not seen, so it is not reviewed. The builder either gives each batch the list of its own files so nothing is cut, or leaves the overflow skipped as `deleted` (my call: never count an entry no run saw).
5. **A deletion that can't be batched keeps the `deleted` rule**: a binary file, an empty file, or a file the platform sent without content (GitLab can return an empty `diff`). That blocks approval as today. A deleted image still referenced by HTML therefore needs a person, which is the safe side (my call). Falling through to `binary` or `no content changes` is not allowed: `no content changes` doesn't block approval, so that would be a silent pass.
6. **Generated deletions:** the generated-header test reads only added and context lines today (`internal/diff/classify.go:104-106`). For a deleted file it must read the removed lines, so a deleted file marked generated by its header stays skipped as `generated`.
7. **The confidence cap** counts only `files.skipped` entries (`render/confidence.go`), so a reviewed deleted file drops out of it without new code.
8. **The result stays schema v1** (the owner's decision). The switch is opt-in, and a new additive field names the reviewed files that are deleted (for example `files.deleted`). The schema doc changes its wording: `files.reviewed` holds no deleted file unless that field lists it. pergent.sh reads the field. An older reader ignores it.

**With it off:** byte for byte today's prompts, runs, result and comment. Deleted files are skipped as `deleted`.

### pergent.sh

- **A repository setting**, "Review deleted and renamed files", off by default. Maintainers and admins change it, like the other repository settings. Config key `repositories[].check_deleted`. A config value wins and shows read-only. Reset to defaults turns it off. It can also be an admin default under Settings, following how other repository settings take a default (builder's call).
- **Passed to the Job as `PERGENT_CHECK_DELETED=true`**, only when the setting is on. pergent.sh already passes newer options as environment variables (`PERGENT_DISMISSALS`, `PERGENT_LINKED_REPOS`), so a pergent too old to know it ignores it and reviews as today.
- **Approval needs no rule change.** `policy.blocksApproval` blocks on a `deleted` skip. With the switch on, a reviewed deletion is no longer skipped, so it no longer blocks. With an older image it is still skipped and still blocks. A pure rename never blocked (`no content changes` is non-blocking). What stops a rename-only change today is "nothing reviewed", so the rename part of Goal 3 matters only for changes where nothing else was reviewed.
- **The pergent pin** (`go.mod` and `defaultImage`) moves to the pergent release that has the switch, in the same pergent.sh change.
- **Showing findings on deleted files.** pergent.sh renders the comment itself (`render.Comment` in `internal/policy/policy.go`) with its own `FileURL` to the head commit (`internal/engine/publish.go`). Inline threads are right side only on GitHub (`internal/github/client.go`) and `new_line` on GitLab (`internal/gitlab/client.go`). So the owner's bar (visible in the comment, no link to a missing page) needs pergent.sh changes in publish: no head link for a finding on a deleted file, and no inline thread for it unless the builder posts it on the removed side.
- **Skill results.** `internal/fates/fates.go` assumes a deleted file is never reviewed. It needs checking.

## Edge cases

- **A finding on a removed line.** That line does not exist at the head commit. The builder decides how such a finding is anchored and shown: in the comment, in GitHub and GitLab inline threads, and in links. A file link to the head commit would 404. The owner's bar: the finding must be visible in the review comment, and nothing may link to a missing page.
- **A deleted file in the Files list.** The list has no links today (`render/details.go`), so nothing to change there. The real link risk is a finding's location (see pergent.sh above).
- **Incremental re-reviews.** A file deleted in an earlier push and still deleted is carried like any reviewed file. A file deleted only in the newest push is reviewed in that push. Today pergent.sh carries nothing for a skill with a finding outside its reviewed files, dismissed findings included (`internal/engine/incremental.go`). A reference finding is outside by design, so pergent.sh changes that rule (the owner's decision): a finding that points at a remaining reference to a deleted or renamed file doesn't stop carrying. The builder decides how pergent.sh recognises such a finding, for example by its file not being in the diff while the skill's result lists deleted or renamed paths.
- **A huge deleted folder.** Its files go through batches like any other files. Vendored ones stay skipped. The token cost is real, and it is why the switch is off by default.
- **Dismissals and fingerprints.** A finding about removed code is a normal finding, so dismissing it works as today.

## Upgrade

- pergent: a new flag, off by default. Nothing changes for anyone who doesn't set it.
- pergent.sh: a new repository setting, off by default, and a migration that adds its column. Rolling back removes the setting. If `check_deleted` is set in the config, remove it before rolling back, because the previous release refuses an unknown key.

## Delivery

1. **pergent:** build on the 3 reviewed commits of `feat/check-deleted`. They already have the reference check, failure handling, the list, the carve-outs and their tests. The uncommitted rename tests left in that checkout by a stopped run are dropped: they were never reviewed, and one expects a pure rename to stay skipped, which contradicts Behaviour 4 (my call). Change: default off, batch deleted files as files, renames, what counts as reviewed, the generated-header fix, and the result fields below. Then a pergent release.
2. **pergent.sh:** the setting, the env var, the pin move, result reading (`files.deleted`), publish (finding locations and inline threads on deleted files), the carry rule for incremental re-reviews, skill results, and docs, after that release.

## Testing

- pergent: off is byte-identical (golden files unchanged), on batches a deleted file's removed lines and reviews it, renames' old paths are in the list, scope by old or new path, vendored/lockfile/generated deletions stay skipped, reviewed and skipped lists and the confidence cap with the switch on, a failed or unstarted run fails the skill, a too-big deleted file is not reviewed, a delete-only or rename-only change, an incremental re-review, an entry past the list's cap not counted as reviewed, a deleted binary or empty file keeping `deleted`, a deleted file with a generated header staying `generated`, `files.deleted` present only with the switch on.
- pergent.sh: the setting's access (maintainer yes, viewer no), config wins, the env var set only when on, approval of a clean deletion with the switch on, a deletion still blocking with it off, an old image ignoring the var, a finding on a deleted file with no head link and no broken inline thread, carrying continuing after a reference finding.

## Left to the builder

- How a finding on a removed line is anchored and shown (see Edge cases), within the owner's bar.
- Whether the reference list stays a separate attachment or is folded into the batch that carries the deleted files.
- Whether pergent.sh also gets an admin default for the setting.
- Wording of the prompt sentences and the setting's help text.
