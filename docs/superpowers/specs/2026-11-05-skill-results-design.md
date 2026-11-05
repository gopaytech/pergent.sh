# Skill results

pergent.sh, next release. Status: design, not built. Roadmap item 5.

## Why

Nobody can tell a useful skill from a noisy one. A skill can raise forty findings a week that people quietly ignore, and nothing shows it. The dashboard counts reviews, not what happened to what they said.

The purpose is **to show a maintainer which skills to fix or drop**. *(chosen)* It is not a measure of learning from dismissals: a finding the model leaves out on its first review is never raised, so it gets no row, and a falling Dismissed count is only a hint. *(chosen)*

## Goals

- Every finding on a change that merges gets one final fate.
- A maintainer can see, per skill in a repository, what became of its findings.
- "Fixed" means the code at the finding changed, not that the finding stopped being reported.

## Non-goals

- Numbers before this release. Only changes that merge or close after the upgrade count, so the tab takes a few weeks to mean much. *(chosen)* The sweep for missed webhooks looks back 7 days, so a few changes from the week before can appear. *(my call)*
- Live numbers on open changes. *(chosen: decided at merge or close)*
- Totals across repositories, on the skill library or on Overview. *(chosen: the repository page only)*
- Charts, trends, alerts, or advice about which skill to change. The first version is a table.
- Any change to reviews, comments, verdicts or approval.
- A setting. It only reads, and its work is capped, so it is always on. *(chosen)*
- Counting left-out findings. That needs parsing notes, or a field pergent doesn't return yet. *(chosen: later)*

## Anti-goals

- Crediting the model's inconsistency as a fix. A finding that vanishes with its lines untouched is not a fix.
- A failure of this feature that touches a review or a webhook. Working out fates is best effort, after the fact.
- Numbers that imply precision they don't have, such as a percentage of three findings.

## The fates

Each finding raised on a change ends in exactly one of these, in this order of precedence:

| Fate | Meaning |
|---|---|
| **Dismissed** | A dismissal on this change covers it, at merge or close. |
| **Open at merge** | It was still reported when the change merged or closed, not dismissed. |
| **Fixed** | It stopped being reported, and code at or next to its lines changed. *(chosen)* |
| **Went away** | It stopped being reported, and either nothing near its lines changed or there's no telling. This catches left-out findings and a model changing its mind. *(chosen)* |

"Covers" is the rule the comment already uses (`render.ApplyDismissals`): a suggestion dismissal doesn't cover a finding that came back as blocking. *(my call)*

A finding counts once on a change, at its highest severity, even if it was reported on two lines. *(my call)* A dismissal whose finding is in no review the walk reads gets no row. *(my call)*

## Working it out

When a change merges or closes, pergent.sh walks the change's reviews in order. A review counts if it was decided, whether or not a later run replaced it: `outcome <> ''`, as `LatestDecidedRun` does, not `state = 'published'`, so a finding that only a replaced run reported still counts, and a run sent back to `reported` by a dismissal isn't lost. *(my call)* An incremental review's merged result counts like any other, so a carried finding stays reported, and its line still holds at that head.

**One finding across reviews.** A finding is followed by its fingerprint and, when that doesn't match, by the rule inline threads use (`threads.reworded`): same skill, same file, lines overlapping once mapped through the compare. So a reworded finding is the same finding. *(chosen)* A finding that stopped being reported can be reworded too, into what the findings still reported left over, so the same finding across a gap stays one row. A file renamed in between is followed the same way, where the compare says so.

**Which reviews say anything.** A review is evidence that a finding stopped being reported only if the skill ran there, didn't fail, and reviewed that file. Otherwise the review is skipped for that finding, which counts as still reported. That covers a failed skill, a skill removed from the repository, and a `review failed` result holding only the re-run skills. *(chosen: never call it Fixed)*

If that review's skill wrote a `Left out because of past dismissals:` note, one the same skill's note in the review before did not carry (a merged review keeps earlier notes, and a kept note says nothing about the new review), a finding of the skill that vanished there is **Went away**, even if its lines changed: the note can't say which finding it means. *(chosen)*

**Fixed or Went away.** For the rest, it compares the head of the last review that reported the finding with the head of the review that didn't, using `forge.CompareDiff`, and looks at the lines that really changed, on the old side of the diff. Hunks include context, so hunk overlap isn't the test; `internal/diffmap` needs this added.

- An added, removed or changed line within 3 lines of `line` to `end_line`: **Fixed**. *(chosen)*
- The file deleted: **Fixed**.
- Nothing that close: **Went away**.
- A finding with no line: any change to the file is **Fixed**. *(my call)*
- A compare the platform refuses for good (`ErrNotFound`, `ErrNotAncestor`, `ErrPartialDiff`): **Went away**. Unknown is never a fix.
- A compare that fails for now (429, 5xx, a timeout): the whole change is tried again later.

One compare per pair of heads, cached for the walk. A cap of 20 compares per change: anything past it is **Went away**. *(my call)*

## Merged or closed

Fates are stored for both. A change closed without merging is left out of the rates, because abandoned work says nothing about whether a finding helped; its Dismissed findings still count as dismissed. *(chosen)*

A change opened by a bot is left out of the rates too: a bot can't act on a finding, so Dependabot and Renovate changes would only make a skill look ignored. *(chosen)* `forge.IsBot` tells, when the fates are worked out. If it can't be read, the change counts as a person's. *(my call)*

## When it runs

The webhook handler already sees a close or merge, in the same branch that calls `SupersedeMR`. It records that the change needs its fates worked out, and nothing more: no platform call in a webhook. GitLab's `action` tells merge from close; on GitHub it's `pull_request.merged`, which `pullRequestEvent` needs to decode.

The engine's loop picks the work up, first confirming the change's state with `GetChange`:

- Still open, because it was reopened: drop the work. A later close or merge records it again.
- Already worked out as merged: leave it. A redelivered webhook must not recompute a merged change, as a compare could fail now.

A temporary failure retries with backoff. After 5 attempts it stops with a warning and that change has no fates: no guessing. *(my call)* It runs with a time budget per pass, because engine loops share one goroutine and platform rate limits. *(my call)*

A missed webhook means a change gets no fates. The existing hourly sweep can catch those; how is left to the builder, within: no more than one platform call per repository per sweep in the normal case. *(my call)*

**Reopened changes.** A close, then a reopen, then a merge ends with the merge's fates: each merge or close replaces the change's rows, including whether it merged and when. Rows from the close stay until then. *(my call)*

**After the merge.** Fates are final. A suggestion can still be dismissed or restored on a merged change, and that doesn't change its fate. *(my call)* The date is when the webhook arrived, because GitLab's hook carries no merge time. *(my call)*

## What it stores

A table of final fates, one row per finding per change: repository, change, fingerprint, skill, the hash of the skill's content in the last review that reported it, severity, fate, whether the change merged, whether a bot opened it, and when. Rows are removed with their project or instance, like dismissals and threads. A skill is counted by name within a repository, so a rename starts fresh. *(my call)*

No finding text is stored: the review it came from holds that. *(my call)*

Rows older than a year are deleted by the existing hourly prune. *(chosen)*

## What people see

A **Skill results** tab on the repository page, for anyone who can see the repository. *(chosen)* One row per skill, with blocking findings and suggestions shown separately *(chosen)*:

| Skill | Severity | Raised | Fixed | Dismissed | Open at merge | Went away |
|---|---|---|---|---|---|---|

- A choice of the last 30 or 90 days, by when the change merged. 30 is the default. *(my call)*
- Counts always show. The one highlighted figure is **Addressed = Fixed ÷ Raised**; Dismissed isn't addressed. *(chosen)* It shows only when the row has at least 10 findings. *(my call)*
- Rows are sorted lowest Addressed first, so the noisiest skills are on top; rows without the figure come last. *(chosen)*
- A row whose skill changed within the window says so, for example `Edited 12 days ago: numbers include the old version`, from the stored hashes. Rows aren't split by version: that would leave too few findings per row. *(chosen)*
- Only merged changes by people count. Dismissed also counts closed ones, so Raised can be less than the columns' sum; Addressed uses merged findings only. *(my call)*
- With nothing yet, the tab says so, and that numbers start from this release.

## With what's already there

- **Dismissed** comes from the `dismissals` table on that change. A restored dismissal isn't one.
- **Stop using** on Past dismissals doesn't change a fate: the dismissal still happened.
- **Inline threads** already say fixed or gone, but only for findings posted as threads, and by another rule. Their state isn't an input; their matching rule for a reworded finding is reused.

## Accepted costs

- Numbers lag behind: a long-lived change counts only when it merges.
- A rebase or force-push loses the comparison on GitHub, which refuses non-ancestors, so those teams see more Went away and fewer Fixed. GitLab compares them anyway, and that diff includes target-branch changes, so someone else's edit can earn Fixed. Merging the target into the branch does the same on both.
- A new left-out note marks every vanished finding of that skill in that review as Went away, so a real fix beside it is undercounted.
- An unrelated edit within 3 lines earns Fixed.
- A file that left the change, such as a reverted one, is skipped, so its findings stay **Open at merge**.
- Following a renamed file can spend compares on reviews that are skipped, so past the cap of 20 a real fix is **Went away**.
- A carried finding stays reported until its file changes. A fix made in another file leaves it **Open at merge**, though a full review would have dropped it. With incremental reviews on by default this happens routinely, so Addressed reads low. Re-reviewing carried files at merge would bring back the cost incremental reviews removed. *(chosen)*
- A line change isn't proof of a fix. Someone can reformat the line. Fixed means the code moved, not that the problem is gone.
- A finding fixed before its first review doesn't exist here.
- A push after the last review, before the merge, isn't looked at: findings in that review count as Open at merge even if that push fixed them.

## Left to the builder

- The table and index shape, the migration, and the query behind the tab.
- How the loop claims work.
- How the sweep finds changes whose webhook was missed.
- The API route and the page's layout.
- Test layout. Database tests first run in CI; never run containers locally.

## Success

A maintainer opens the tab, sees a skill whose findings are mostly dismissed or ignored, and knows which skill to rewrite.
