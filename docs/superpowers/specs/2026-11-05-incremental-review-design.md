# Incremental re-reviews

pergent.sh 0.7.0 and pergent v0.5.0. Status: design, not built.

## Why

Every push to a change reviews the whole change again. A one-line fix on a 30-file change costs as much as the first review: every skill runs every batch, and verification and the cross-batch run follow. The previous findings go along as `--previous`, but nothing is skipped.

The problem to solve is **slow and costly re-reviews**. A re-review should take time in proportion to what changed since the last review.

## Goals

- After a push, only the files changed since the last review are reviewed again.
- Everything else is carried over from the last review: findings, checks and what was reviewed.
- A skill none of whose files changed starts no opencode run at all.
- Anyone reading the comment can tell a partial re-review from a full one.
- A full review stays one command away.

## Non-goals

- Finding breaks in untouched files. An edit to file A that breaks unchanged file B can go unnoticed until a full review. The agent can still read B, but nothing sends it there. This is the accepted cost of the speed. *(chosen: changed files only, over also reviewing callers)*
- Requiring a full review before approval. Approval and commit status decide on the combined result, as they do today.
- pergent's CI mode. Only agent mode, which pergent.sh runs, changes.
- Telling the model which hunks inside a changed file are new. A changed file is reviewed whole.

## Anti-goals

- A file counted as reviewed that was never reviewed. Only a file the skill really reviewed last time can be carried over.
- A partial result published as if it were the whole review. If the carried-over part can't be added, the review says so and is never approved.
- A failure of this feature that fails a review. Any doubt falls back to a full review.

## Decisions

### When a review is incremental

A run is incremental only when all of these hold. Otherwise it is a full review, as today.

- The repository's **Incremental reviews** setting is on.
- Its trigger is a push (`TriggerPush`). A re-run from `@bot review` is always full. `review failed` works as it does today.
- There is a last review: the newest published, completed run of the change (`PreviousResult`).
- That review has the same target branch.
- Its head differs from the new head.
- Its head is an ancestor of the new head. After a rebase or force-push it is not, so the review is full.
- The platform can list the files changed between the two heads. An error or a truncated list means a full review.
- The last review's `pergent_version` is v0.5.0 or later, or an unparsable version means a full review. The Jobs image's tag is read too: a tag that is a version below 0.5.0 means a full review, whatever the last result says. A digest with no tag, or any other tag, leaves the gate to the last result. So a Jobs image pinned to an older pergent, which would refuse `--carried`, keeps doing full reviews instead of failing. *(your call)*

Only rebase, force-push and `@bot review` start a full review on purpose. Changed instructions or a changed model don't. *(chosen)* The other conditions are fallbacks. *(your call)*

There is no limit on incremental reviews in a row and no periodic full review. *(chosen)*

**Rebase and force-push stay full in this version.** *(chosen)* Teams that amend or rebase get no speed-up yet. Making them incremental needs a comparison of the two commits inside the Job, because GitHub's compare goes from the merge base. Each full review of a push logs why it was full, so the logs show how often this matters.

### Which files are carried over

pergent.sh decides, per skill, before it starts the Job. A file is **carried over** when all of these hold:

- it is in that skill's `files.reviewed` in the last review;
- it is not among the files changed between the two heads;
- it has no open blocking finding from that skill. A blocking finding that was dismissed doesn't count. *(chosen: a fix in another file must be able to clear a blocker on a push)*

Every other file in the skill's scope is reviewed. This one rule covers the other cases without extra code:

- A file whose batch failed last time was not in `files.reviewed`, so it is reviewed.
- A file new to the change is reviewed.
- A file that left the change is not in the diff, so nothing happens to it, and its findings are dropped.
- A file that only arrived by merging the target branch into the change isn't in the skill's scope, so it costs nothing.

**A new or edited skill carries nothing** and reviews all its files, while the other skills stay incremental. `SkillVersions` covers library skills only, and repository skills are read at the target branch, so each run records a hash of every skill's content (a nullable column). A skill whose hash differs or is missing carries nothing. *(your call)*

**A skill with a finding on a file outside its `files.reviewed` carries nothing.** Nothing keeps a model's finding inside the diff, and such a finding would otherwise vanish on the next push. *(your call)*

### Who does what

pergent.sh has the last review and can compare commits through the forge. pergent has the diff. So:

- **pergent.sh** works out the carried files and writes them to the Job's inputs, as `/in/carried.json`: `{"<skill>": ["path", ...]}`. *(your call: this needs no git work in pergent and no extra commit in the checkout)*
- **pergent** gets a new agent flag, `--carried=<path>`, a trusted input under the same checks as `--previous`. It leaves those files out of the skill's review, and lists them in the result as `files.carried`. A skill with nothing left to review starts no opencode run. Its verdict is `no_blocking_issues` and `files.reviewed` is empty. The Job always starts, even if no skill has anything to review, so scope is decided in one place. *(chosen)*
- **pergent.sh** merges the result with the last review when it arrives, before publishing, as it does for `review failed` (`mergeBase`). The merged result is what is stored, published and fed to the next run as previous.

The model still sees every file of the change in `changed-files.md`, carried ones included, and one prompt line says the others were reviewed earlier and to read them if this change affects them. *(chosen)*

`--previous` still goes along, without the findings on carried files, because those are not the model's to drop. A finding on a re-reviewed file keeps its fingerprint when it still applies. *(your call)*

### How a skill's result is merged

For each skill of an incremental run:

| Field | Merged value |
|---|---|
| `findings` | The new ones, plus the last review's findings on carried files, unchanged |
| `files.reviewed` | The new ones, plus the carried files |
| `files.skipped`, `files.not_reviewed`, `failures` | The new run's only |
| `verdict` | `failed` if the new run failed; otherwise `has_blocking_issues` if any merged finding is blocking; otherwise `no_blocking_issues` |
| `checks` | The last review's checks, with any check the new run reported (same section and name) replaced by the new one *(your call)* |
| `confidence`, `confidence_reason`, `summaries`, `notes` | The new run's if the skill ran; the last review's if it didn't *(chosen for confidence; your call for the rest)* |

Finding IDs are assigned again over the whole, as `result.Merge` does.

- **Lines of carried findings stay right**, because the file's content didn't change.
- **Threads stay as they are.** A carried finding is still in the result, so its thread stays open, and a dismissal still applies by fingerprint.
- **Carried findings are suggestions and dismissed blockers.** An open blocking finding's file is always reviewed again, so verification covers every open blocker on every push.
- **A carried suggestion stays** until its file changes, someone dismisses it, or `@bot review` runs. *(chosen)*
- **`cross_batch`:** the cross-batch run sees only the batches of this run, so it starts only if there is more than one. That is part of the accepted cost.

If the last review can't be read at merge time, the run is **started again as a full review** instead of being published. A partial result never reaches the policy, which would pass it. A full review needs no merge, so this happens once. *(your call)*

### What people see

The comment's meta line says what happened, for example: `Re-reviewed 2 files · 28 carried over from abc1234`. *(your call)* A full review shows no such text. The dashboard's review page shows the same line.

The `Files (N)` list counts reviewed and carried files together.

### Commands

`@bot review` is the full review. Its help text and the README say so. No new command is added.

`review failed` is unchanged: after an incremental run it re-runs a failed skill over all its files. *(chosen)*

A head that already has a run queued or running isn't queued twice today. When that run is incremental, `@bot review` supersedes it and queues a full review, so the command always delivers one. *(your call)*

### The setting

**Incremental reviews** is on by default and works like **Inline findings**: a repository's Settings, Admin → Settings, and the config. *(chosen)* With it off, every push is a full review.

Names follow that setting: config `repositories[].incremental_reviews`, settings key `incremental_reviews`, JSON `incrementalReviews`. A nullable column is added to `dashboard_repos` and `repo_defaults`. *(your call)*

## Upgrade

- pergent.sh 0.7.0 makes pergent v0.5.0 the default Jobs image. The Upgrading section lists the points below.
- The first push after the upgrade is a full review, because the last result is from an older pergent and has no skill hashes. Incremental reviews start from the next push.
- An operator who pins the Jobs image to an older pergent by tag keeps full reviews, even after running 0.5.x. An image pinned by digest alone needs the setting turned off first.
- The migration adds nullable columns and one `runs` column that is `NOT NULL DEFAULT false`, so 0.6.2 keeps working on a migrated database.
- To roll back, first remove `incremental_reviews` from the config, because 0.6.2 refuses unknown keys. Results stored by 0.7.0 are complete, merged results.

## Left to the builder

- The name and signature of the forge call that gives the changed files and says whether one commit is an ancestor of the other, built on the existing compare calls (`CompareDiff`). GitHub's compare gives a status; GitLab has a merge-base call.
- How a run records that it is incremental and what it builds on, including across a restart between the Job and the publish (`BaseRunID` exists; a nullable column and a migration if more is needed).
- Whether incremental merging extends `result.Merge` or is a function beside it.
- Where the meta line's text is built (`render` options or the existing merge note).
- Keeping `/in/carried.json` within the ConfigMap limit. If it doesn't fit, the review is full.
- The query for the last review. `PreviousResult` returns only the result, and this needs the run too (its ID, target branch and skill hashes).
- Test layout, following the `review failed` tests.
