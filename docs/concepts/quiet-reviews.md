# 🤫 Quiet reviews

Teams differ in how much review they want. What bothers people is nitpicks and style comments, and a comment that is long to read. A quiet review says less without missing anything that blocks: blocking findings, failures, verdicts and approval are the same as in a normal review. Only what a review says about suggestions and the length of its comment changes.

**Turning it on.** It is off by default. The switch is **Quiet reviews** in a repository's Settings, the default under **Admin → Settings**, or `repositories[].quiet_reviews` in the config ([Config reference](../reference/configuration.md)). On a repository the dashboard manages, a repository that leaves it unset takes the admin default, and with none set it is off. A repository listed in the config reads no admin default: unset there means off. A review reads the setting when it starts and keeps it, so a change takes effect with the next review. The review page labels a run started quiet with `quiet` in its details, and it still shows every check and file, though a quiet comment leaves out the Files list and the checks that passed.

**The prompt line.** A quiet review adds one section to the instructions the model reads, word for word:

> # Quiet review
>
> When you raise a suggestion of your own, raise it only if it would change how the code behaves or clearly helps the next person who reads it, and leave out style, naming, formatting and other nitpicks. This applies to nothing else: it does not change what is blocking, how you report or judge a checklist item, or how you check findings another reviewer raised.

It comes last, after `AGENTS.md` and `.pergent/instructions.md`, so it wins over them for suggestions. The last sentence keeps it to the model's own suggestions: what blocks, how a checklist item is judged and how a finding another reviewer raised is checked do not change. A repository with no instructions of its own still gets an instructions file, holding only this section. The section counts toward the 900 KiB the skills and instructions may weigh, so a review whose inputs only just fit can fail as too large once the setting is on. It is not part of any skill's hash, so turning the setting on or off does not make a skill look edited, and an [incremental review](incremental-reviews.md) can still carry files over. The **Skills & instructions** tab and the readiness budget do not show it. With the setting off nothing is added, and the prompt is the same as before, byte for byte.

**Threads.** A quiet review posts no new suggestion threads: a suggestion without an open thread stays in the comment, under **Not on the diff**, with the note "Kept here: quiet reviews don't post suggestions on the diff." One whose lines are off the diff keeps that reason instead, and with **Inline findings** off suggestions are in the comment's findings with no note. Blocking findings are posted on the diff as before, with their one-click fix when they have one; a suggestion has no one-click fix, since it is not on the diff. A suggestion thread that is already open stays open while the finding is still reported, and that suggestion is not listed in the comment. A resolved one is not reopened, and a suggestion that comes back or is restored stays in the comment. A thread that goes away because its finding is no longer reported gets the reply ``No longer reported in `<sha>`.`` and is resolved, blocking ones too, since a quiet review does not claim a fix. A blocking finding that comes back reopens its thread with ``Back in `<sha>`.`` as usual. The other replies do not change: "No longer in this change." when the file left the change, ``Dismissed by …`` for a dismissal, and the note on past dismissals. When the branch also requires resolved threads, the quiet note is the one shown. The review's own setting decides, as stored on the run, not the repository's setting now.

**The comment.** A quiet review's comment leaves out the `Files (N)` list and the checks that passed or were n/a. It keeps the headline, the meta line with its file count and `N of M checks passed`, the confidence reason when the score is below 5, the skills table (with several skills), the failures, the **Summary** (what the model looked at and concluded; with several skills, one per skill), every finding (suggestions too), the **Dismissed** list, and an area heading with its table for each area that has a failed check, with only the failed rows in it. It also keeps any note, among them "Not approved" (which still lists the skipped files that stopped the approval), and the hidden findings block, so the next review reads the same findings as before. The meta line ends with `quiet`, so nobody takes the shorter comment for a cut or a failed review. On an [incremental review](incremental-reviews.md) it follows the incremental note: ``Re-reviewed 2 files · 28 carried over from `abc1234` · quiet``. An error result's comment has no such layout and is the same on or off.

**The size limit.** The comment is held to the same limit as any other. Nothing was cut for size, so a quiet review never says "Passed and n/a checks omitted to fit the comment size limit" for what it left out on purpose. The steps that apply to a comment over the limit still apply after the shorter layout, in the same order, eight in all:

1. the hidden findings block
2. the suggestions' explanations and fixes
3. the suggestions and dismissed findings
4. the passed and n/a checks
5. the remaining check tables, which hold the failed checks
6. the file list, cut to 20 paths
7. the file list, cut to a count
8. the summaries

The steps for the passed checks and the Files list (4, 6 and 7) do nothing on a quiet review, since those are not written, and a step that cuts nothing says nothing. None of these steps drops a blocking finding or a failure. Step 5 can cut failed checks, with its note "Check tables omitted to fit the comment size limit."; the meta line keeps the counts. The summaries go last, with the note "Summaries omitted to fit the comment size limit." If the comment is still over the limit, it is cut there with a note, as on any review.

**What it costs.**

- A skill whose whole job is style goes mostly quiet, unless its `## Blocking` section lists what it cares about.
- A suggestion has no one-click fix, since it is not on the diff. The change it proposes is text in the comment.
- The comment carries the suggestions that used to be threads, as a list in it.
- The model may still raise a nitpick, or leave out a useful suggestion. The prompt line is a request, not a rule.
- A reader of the comment no longer sees which files a skill skipped without opening the dashboard, unless skipping stopped the approval, which the comment still says.
- A thread that really was fixed on a quiet review says "No longer reported", not "Fixed".
- A style item on a skill's checklist is still reported, so a quiet review of such a skill still shows it.
- The numbers in [Skill results](skill-results.md) before and after the switch are not comparable, since a quiet repository raises fewer suggestions. The tab does not say so.

**First use.** Turn it on for one repository and read about five of its reviews before you set it as the default or turn it on for a big repository. Check three things: that no blocking finding is missing, that the suggestions you see are worth reading, and that the shorter comment is enough for you to review without opening the dashboard.

**With the rest.** With **Inline findings** off there are no threads to change, and the prompt line and the shorter comment still apply. With [incremental reviews](incremental-reviews.md) a carried finding stays as it is until its file changes, so a repository that turns quiet on in the middle of a change keeps the suggestions of its earlier reviews, in their threads where they have one and in the comment otherwise. [Learn from dismissals](learn-from-dismissals.md) works as before. The prompt line is not part of a skill's hash, so **Edited N days ago** does not fire when the setting changes.

The run stores whether it was quiet when it starts, before its Job does, and the review uses that value for its prompt, its threads and its comment. If starting the Job fails and the review is tried again after you changed the setting, the new try uses the new value.
