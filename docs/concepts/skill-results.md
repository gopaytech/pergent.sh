# 📈 Skill results

A skill that raises findings nobody acts on costs reviewers time. The **Skill results** tab of a repository shows which skills people act on and which they set aside, so you can fix or drop the weak ones. Anyone who can see the repository can open it.

When a change is merged or closed, pergent.sh works out what became of each finding raised on it. Each finding ends in one of four ways:

- **Fixed**: it stopped being reported, and code at or near its lines changed.
- **Dismissed**: a person dismissed it, and the dismissal still stood at the end.
- **Open at merge**: it was still reported when the change ended, and nobody dismissed it.
- **Went away**: it stopped being reported, but nothing near its lines changed, or there is no telling. A model changing its mind lands here, and so does a finding a skill left out because of [past dismissals](learn-from-dismissals.md).

A finding on a deleted file (see [Deleted and renamed files](deleted-files.md)) that stops being reported while the file stays deleted went away: the file didn't change between the two reviews. A restored file is tracked like any other file.

The tab has one row for each skill and severity, for the last 30 or 90 days. **Raised** is the number of findings. **Addressed** is Fixed divided by Raised, as a whole percent. Dismissed doesn't count as addressed. A row shows Addressed only from 10 raised, not 10 findings of any kind. Rows with the lowest Addressed come first, and rows without one come last. So a skill with 9 ignored findings sits at the bottom until it has 10.

Only merged changes opened by people count. A change closed without a merge, and a change opened by a bot, say nothing about whether a finding helped. The one exception is Dismissed, which also counts findings dismissed on closed changes opened by people. So Raised can be less than the sum of the columns.

When a skill was edited inside the window, its row says so, for example `Edited 12 days ago: numbers include the old version`. Rows aren't split by version, because that would leave too few findings in each.

**Skill cost on these changes.** Admins also see a section under the table with the tokens each skill used on the changes the table counts, over the same 30 or 90 days, and the tokens it spent per fixed finding: input plus output, divided by its Fixed. A skill with nothing fixed shows none. Read it with these in mind:

- A change merged with no findings is counted, so the section covers more changes than the table.
- A bot-opened change merged with no findings is counted too, because a bot is known only from its findings. The table leaves bot changes out.
- A change counted because of one skill's dismissal counts the tokens of every skill on it.
- The tokens are those of all of a counted change's reviews, not only the ones inside the period.

**What to expect.**

- Addressed reads low with [incremental reviews](incremental-reviews.md). A finding stays reported until its own file changes. If the author fixes it from another file, it ends as open at merge, though a full review would have dropped it.
- A push after the last review isn't looked at. Findings fixed by it still count as open at merge.
- On GitHub, teams that rebase or force-push see fewer Fixed. GitHub refuses to compare heads that aren't ancestors of each other, so those findings go to Went away.
- On GitLab, and on both platforms after merging the target branch into the branch, someone else's edit can earn Fixed. The comparison then includes the target branch's changes, so Fixed can read high.
- An unrelated edit within 3 lines of a finding counts as a fix.
- A finding in a file that left the change, such as a reverted one, stays open at merge.
- A change whose comparisons need more than 30 seconds gets no fates. pergent.sh tries again, and after 5 attempts gives up on that change rather than guess.
- On GitLab a change counts as a bot's only if GitLab marks the account as a bot, so Renovate running on an ordinary account counts as a person.
- Numbers start from this release. Changes that ended earlier are not counted, so the tab takes a few weeks to mean much. The hourly sweep looks back 7 days, so a few changes from the week before the upgrade can appear.
- A change counts when it ends, so a long-lived change shows up late.
