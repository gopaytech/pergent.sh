# 🧵 Inline findings

pergent.sh posts each finding of a review as a thread on its lines, next to the single review comment.

- **Each finding is posted on its lines.** At most 20 pergent threads are open on a change at once: blocking findings go first, then the rest by file and line.
- **A small fix comes as a suggestion.** When the fix replaces 10 lines or fewer, all inside the finding's lines and within one diff hunk, people can apply it with GitHub's **Commit suggestion** or GitLab's **Apply suggestion**. A replacement with a line that starts with `/` in column 0 is never posted as a suggestion, because GitLab would run it as a quick action; the fix is shown as advice in words.
- **A later review keeps the threads current.**
  - A finding that is gone gets the reply ``Fixed in `<sha>`.`` and its thread is resolved (in a [quiet review](quiet-reviews.md) the reply is ``No longer reported in `<sha>`.``). This happens only when its skill ran without failing and still reviewed the file. If the file left the change, the reply is "No longer in this change." When the skill's notes say it left a finding out because of a [past dismissal](learn-from-dismissals.md), a missing finding says nothing about the code, so its thread is not resolved: it gets one reply, ``No longer reported in `<sha>`; see the note on past dismissals in the comment.``, and stays open. The note belongs to the skill, not to a finding, so a finding of that skill that really was fixed gets this reply too and stays open until someone resolves it with one click. If the finding is reported again, the thread gets ``Back in `<sha>`.`` and is told again should it be left out once more. The note stays in the comment across pushes that carry files (at most 5 earlier notes are kept, the newest), so until a review of that skill carries nothing (`@<bot> review` is one), no thread of the skill is resolved as fixed. On a branch that requires resolved threads, such a thread holds the merge until someone resolves it.
  - A finding that comes back reopens its thread, with ``Back in `<sha>`.`` A suggestion doesn't, on a [quiet review](quiet-reviews.md) or when the branch requires resolved threads: it stays in the comment.
  - A thread that a person resolved is left alone, and its finding is not posted again.
- **Dismissing resolves the thread.** A dismissal from a comment, from `@<bot> dismiss <reason>` as a reply in the finding's thread, or from the dashboard replies "Dismissed by …" and resolves the thread. Restoring it on the dashboard reopens it, except a suggestion on a [quiet review](quiet-reviews.md) or when the branch requires resolved threads. See [Dismissing a finding](commands.md#dismissing-a-finding).
- **Findings that can't go inline stay in the review comment**, under **Not on the diff**, each with the reason: off the diff, over the limit of 20, posting failed, too long, kept because the branch requires resolved threads, or kept because the review is [quiet](quiet-reviews.md).
- **When the target branch requires resolved threads, only blocking findings go inline.** On GitHub that is a ruleset's "Require conversation resolution" or the classic protection's rule; on GitLab it is "All threads must be resolved". The non-blocking findings stay in the comment, so they leave no open thread to hold up a merge.
- **Very large changes get no threads.** At 3000 changed files or more on GitHub, or when GitLab's diff passes its limits, that review posts no inline threads and every finding stays in the comment.

**Turning it off.** Inline findings are on by default. The switch is **Inline findings** in a repository's Settings, the default under **Admin → Settings**, or `repositories[].inline_findings` in the config ([Config reference](../reference/configuration.md)). With it off, every finding stays in the review comment, as before 0.6.0.

**GitHub setup.**

- The review App needs **Pull requests: Read and write**. With Read-only, inline is off for that instance and the Integrations page says so.
- **Administration: Read** is optional. It lets pergent.sh read the classic branch protection's conversation-resolution rule. Without it, only rulesets are read and the classic rule is treated as off, so a branch whose only such rule is classic gets suggestions inline too. The Integrations page notes a repository where the App can't read it (when inline findings are available there).
- Subscribe the App to **Pull request review comment**. Without it, replies in a finding's thread are not received.

**GitLab setup.** "All threads must be resolved" is read from the project settings. No new webhook is needed: note events already arrive.
