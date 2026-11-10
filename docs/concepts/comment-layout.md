# 🧱 Comment layout

A review comment has a fixed top and bottom, and five blocks between them. A repository chooses which blocks show, in what order, and whether each starts open or collapsed. The same review reads differently for different teams: one wants the findings first and nothing else, another wants the checks open.

**The blocks.**

| Block | What it shows |
|---|---|
| `summary` | What the model looked at and concluded. With several skills, one section per skill. |
| `findings` | The findings. Blocking ones are always open. Required: every layout has it. |
| `dismissed` | The findings people dismissed, with who and why. |
| `checks` | The checklist results, grouped by area. |
| `files` | The list of files the review covered. |

Each block is `open` or `collapsed`. For `findings`, collapsed means the suggestions are collapsed; blocking findings stay open. For `checks`, collapsed means the passed and n/a checks are folded away, while the failed checks stay in view. A block you leave out is not written.

The skill sections (one per skill, with several skills), the list of batch summaries (one skill, several batches) and the failed checks sit where `checks` is. With `checks` left out they sit where `summary` is, right after a single summary; with both left out, right after `findings`. So moving `summary` to the top does not move the skill sections with it: they stay at `checks`. This is the default:

```yaml
comment_layout:
  - summary: open
  - findings: collapsed
  - dismissed: collapsed
  - checks: open
  - files: collapsed
```

**Setting it.** `repositories[].comment_layout` in the config, the layout editor in a repository's Settings, or the default under **Admin → Settings** ([Config reference](../reference/configuration.md)). The editor is a list of rows, one per block, with plain labels (Summary, Findings, Dismissed, Checks, Files). Each row has up and down buttons, an **Open** or **Collapsed** choice and a **Show** box; a block with its box cleared is left out, and its row moves to the end. The row keeps its last choice until the form is loaded again. After that, a left-out block shows **Open** on a repository's page and its default choice on **Admin → Settings**. **Findings** has no box, because the layout must have it, and its choice says that blocking findings always stay open, so collapsed means the suggestions. **Reset to default** sends the setting back to the default layout. Over the API the setting is `commentLayout`, a list of `block:state` texts such as `findings:collapsed`; `null` is the default layout, and the settings views show the default list while a repository sets none. Saving a list the server refuses is 400. A repository the dashboard manages and leaves unset takes the admin default, and with none set it gets the default layout. A repository listed in the config reads no admin default: unset there means the default layout. The server refuses an unknown block or state, a block listed twice, an empty list and a list without `findings`, both on save and in the config.

**What no layout can change.** These parts are fixed. They are in every comment when there is one to show:

- the headline with the verdict and confidence, and the meta line under it;
- the confidence reason (only when the score is below 5 or capped), the skills table (with several skills) and any failures;
- the notes, the footer and the hidden findings block at the bottom (the size limit cuts the hidden block first);
- every blocking finding not posted as a thread on the diff, open;
- every failed check, unless the size limit cuts the check tables (see below).

A comment still over the limit after every step is cut there, with a note, whatever the layout, so a very long list of blocking findings can lose its end. The headline, the meta line and the failures come first and stay.

**A Job failure names its step.** When a review's Job fails, the "couldn't review" comment says where it failed, as in `pergent couldn't review this MR: the clone failed (checkout exited with code 128)`. Other texts are `the review job ran out of memory (OOMKilled)` and `the review couldn't start: no node could run it`. It never carries the Job, pod or namespace name, the scheduler's message or an event: those are for admins on the review page ([Troubleshooting a failed review](../setup/troubleshooting.md)). A comment changes the next time it is updated.

A layout moves and folds the blocks, nothing more. It never changes a verdict, a status, a score, an approval or a count.

## The footer text

The last line of a review comment starts with a short text. By default it is `Reviewed by [pergent](https://github.com/gopaytech/pergent.sh)`. An admin can replace it for the whole deployment under **Admin → Settings**, for example with who runs pergent.sh and how to reach them. Clearing the field goes back to the default.

- It is one line of at most 200 bytes. Markdown links work; `<` and images are refused.
- An `@` shows as plain text and notifies nobody, because the text is on every review. Addresses and URLs keep theirs.
- It applies to the next comment pergent.sh posts or updates. Comments already posted keep their text.
- A "couldn't review" comment has no footer.

**Leaving a block out.**

- Leave out `checks`, and the failed checks still show. The passed and n/a checks do not.
- Leave out `summary`, and no summary shows anywhere.
- Leave out `dismissed`, and the list is gone. A clean review that has dismissed blocking findings still says `(2 dismissed)` in its headline. On a review with blocking issues, or for dismissed suggestions, nothing shows.
- Leave out `files`, and there is no list. A skipped file that stopped the approval is still named in the note.

**The size limit.** It can still cut failed checks, in any layout, when the comment is too long. It says so in a note. This is the one thing that can remove a failed check. The hidden block, the suggestions and the passed checks go first; the file list is cut after them.

**Quiet reviews win.** A [quiet review](quiet-reviews.md) writes no Files list and no passed or n/a checks, whatever the layout says. The rest of the layout still applies.

**The run keeps its layout.** A review reads the layout when it starts and stores it on the run. The comment uses that value every time it is written, including when a dismissal writes it again. Changing the setting takes effect with the next review, and a dismissal on an older review does not change its shape. If a stored layout is one this release cannot read (it came from a later release), the comment uses the default.

**What it costs.**

- The default comment is longer than before: passed checks are open. Collapse `checks`, or turn on quiet reviews, to shorten it.
- A summary can disagree with the outcome. The model writes it before the findings are checked, so it may mention a problem that was dropped or dismissed. The headline and the findings are what count.
- On an [incremental review](incremental-reviews.md) a summary can describe only the files that were re-reviewed. A skill that started no run but still carries files keeps the earlier review's summary; a skill that ran replaces its summary with one about the files it re-reviewed.
- A layout that leaves blocks out also hides any block a later release adds, until you add it.
- Batches can word an area differently, and then one check shows under two headings.
- Repositories can have different comments. Someone who reads across repositories meets several shapes.
- A summary is text a model wrote about the repository. It is escaped like any other model text, but it is visible in the comment.
