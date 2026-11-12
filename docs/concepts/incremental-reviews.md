# ⏩ Incremental reviews

A push to a change that already has a review looks only at what the push changed. Files the last review already covered are carried over, and the comment says so, for example `Re-reviewed 2 files · 28 carried over from abc1234`. The review page shows the same line. The result is still one review of the whole change: approval and the status are decided on the combined result.

A review is incremental when all of these hold. Otherwise it is a full review, and the log says why.

- **Incremental reviews** is on for the repository (the default).
- A push started it. `@<bot> review` is always a full review.
- The change has a completed review for the same target branch, and its head is an ancestor of the new head. After a rebase or force-push it is not, so the review is full.
- The forge lists the files changed between the two heads.
- That review was made by pergent v0.5.2 or later.
- The model is unchanged: a different provider or model makes the next review a full one ([Models](models.md#a-change-of-model)).

A file is carried over, per skill, only when that skill reviewed it last time, the push didn't change it, and it has no open blocking finding. A file whose review failed, a new file, and every file of a new or edited skill are reviewed again. Open blocking findings are always checked again.

**With [Review deleted and renamed files](deleted-files.md) on,** a suggestion or a dismissed finding outside the files a skill reviewed no longer stops it from carrying. Such a finding is dropped from the combined comment unless the new run reports it again. While a skill has an open blocking finding, its deleted files are reviewed again on each push. The details are in [Deleted and renamed files](deleted-files.md#incremental-reviews). With the setting off, nothing changes.

**What this gives up.**

- An edit to one file can break another file that didn't change. That goes unnoticed until a full review (`@<bot> review`). The model can read the other file, but nothing sends it there.
- Teams that amend or rebase get no speed-up yet: those pushes are full reviews.

**Turning it off.** The switch is **Incremental reviews** in a repository's Settings, the default under **Admin → Settings**, or `repositories[].incremental_reviews` in the config ([Config reference](../reference/configuration.md)). With it off, every push is a full review.
