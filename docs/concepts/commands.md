# 💬 Commands

A comment on a merge request (or pull request) can ask the review bot to run the review again or to set a finding aside, and the dashboard's review page has the same actions as buttons. That is all they do: no command changes a setting, answers a question or fixes code.

| Command | What it does |
| --- | --- |
| `@<bot> review` | Runs the whole review again on the change's current head and target branch, as a full review: every file, not only what changed since the last review. |
| `@<bot> review failed` | Runs again only the skills that failed, over all their files, and keeps the other skills' results. |
| `@<bot> dismiss <id> <reason>` | Sets a finding aside as not a real problem. `<id>` is the finding's ID, shown as `id …` on its line in the review comment; each finding's own "Not a real problem?" line has the command ready to copy. |
| `@<bot> help` | Replies with the list of commands. |

In a finding's own thread the finding is already known, so `@<bot> dismiss <reason>` works as a reply there, with no ID (an ID in front of the reason is allowed). In any other thread it is answered with a pointer to the right command.

`<bot>` is what people type after `@` to address the review bot: its username on GitLab, the review App's slug on GitHub (`@pergent-review review` for an App called that). A comment that doesn't mention it is ignored, so people can write "review" in ordinary conversation. Restoring a dismissed finding has no command: it is a button on the dashboard.

A reason from someone with write access or better also teaches later reviews of the repository what is not a problem. See [Learn from dismissals](learn-from-dismissals.md).

## Writing a command

- The mention starts its line (up to 3 spaces of indentation; capitals don't matter) and the command follows it: `@pergent-review review`. A colon or comma after the mention is fine. A line that holds only the mention is passed over; the command is read from the next line that starts with the mention.
- Text that isn't the author's own is never read: lines in a fenced code block (` ``` ` or `~~~`), in an HTML comment, indented by 4 spaces or a tab, or quoted (starting with `>`). A mention inside a sentence is no command.
- Only the first command of a comment runs.
- `review`, `review failed` and `help` must be the whole rest of the line (a closing `.` or `!` is fine). `dismiss` takes a finding ID of 6 to 12 hexadecimal digits, then the reason on the same line. Anything else after the mention, such as `review please` or `dismiss` with a malformed ID, is answered like `help`.
- A reason is required to dismiss a blocking finding and optional for a suggestion. A reason is 500 characters at most: a comment's is cut there, and the dashboard refuses a longer one (400 `the reason is too long`). Markdown images (`![...](...)`, also by reference, and `<img>` tags) are removed from a reason before it is stored, so a review comment can't be made to load one.
- Only a newly created comment on a merge request counts. Edits, deletions and system notes never run a command. A reply in a thread counts as a command written in that thread (on GitHub only a reply, not the comment that starts the thread).
- A comment never runs a command when it was written by the review bot or the approve bot, by any account whose name ends in `[bot]`, or through a GitHub App, by a GitLab project, group or service-account bot (a username like `project_<id>_bot`, `group_<id>_bot_<token>` or `service_account_…`; the engine also asks GitLab, `GET /users/:id`, and drops the command with 👎 when the user's `bot` flag is set, or tries it again when GitLab can't say), or when it contains pergent's hidden marker (`<!-- pergent`), which every review comment carries. So neither pergent.sh's comment nor a model's text quoted in a finding can run a command.

## Who may do what

The role is read from the platform when the command is processed, for a comment, and from the repository role on the dashboard ([Teams](dashboard.md#teams)), for a button.

With **Only the author and maintainers use commands** on (the default), a comment is first checked for who wrote it: only the author of the change and the repository's maintainers and owners can use commands. Anyone else is refused, whatever else the table says. With the setting off, the table alone decides; the author is always allowed.

| Action | In a comment | Button |
| --- | --- | --- |
| Re-run (`review`, `review failed`) | The change's author, whatever their access; otherwise a maintainer or owner (GitLab Maintainer or above; GitHub `maintain` or `admin`). With the setting off: the author, or GitLab Developer or above, GitHub `write`, `maintain` or `admin` | Maintainer or admin of the repository |
| Dismiss a suggestion | The same as re-run | The same as re-run |
| Dismiss a blocking finding | GitLab Maintainer or above; GitHub `maintain` or `admin`; **never the change's author** | Maintainer of the repository or admin, **the change's author too**: an account has no platform identity to compare, and the dismissal is in the audit log and on the thread. A merged or closed change refuses everyone |
| Restore | No command | The same as re-run, the change's author included, since restoring only makes the review stricter |

- GitLab: the commenter's membership of the project, inherited from its groups, read by the review bot (`GET /projects/:id/members/all/:user_id`). Only an active, unexpired membership counts: a blocked or deactivated user, one awaiting approval, or one whose membership has expired has no role.
- GitHub: the commenter's repository permission, read by the review App (`GET /repos/{owner}/{repo}/collaborators/{username}/permission`). `maintain` and `admin` can re-run and dismiss blocking findings; `write` can re-run only with the setting off; `read` and `triage` can do neither (unless they are the change's author). A custom role counts by its permissions.
- Maintainers and owners are GitLab Maintainer or Owner, and GitHub `maintain` or `admin`.
- **Only the author and maintainers use commands** is on by default. It is a repository's setting in the dashboard (**Repository → Settings**, under Reviews), a default under **Admin → Settings**, and `repositories[].commands_author_and_maintainers_only` in the config. Dashboard buttons are not affected: they already need a maintainer or admin of the repository.
- The author always can re-run and dismiss suggestions, in both modes, whatever their access. The author can never dismiss a blocking finding on their own change with a comment; a maintainer who is the author can in the dashboard, which knows no platform identity.
- Every refusal about who may is written to the [audit log](dashboard.md#audit-log) as `command_refused`, in both modes: the gate, "no permission" and "author of the change". Ordinary mistakes (an unknown ID, a missing reason, a cap) and commands the 2-minute throttle leaves unanswered are not recorded.
- The author is the one the platform reports for the change, compared by user ID, never by username. When either ID is unknown the commenter is taken for the author, so a blocking dismissal is refused. For being let in as the author (the setting, re-runs, suggestion dismissals), an unknown ID is nobody.
- The pergent.sh team roles apply to buttons only; a comment is judged by the platform alone.
- On a change whose author the repository doesn't trust ([Trusted authors](trusted-authors.md)), `review` and `review failed` start the whole review and need write access: GitLab Developer or above, GitHub `write`, `maintain` or `admin`, also while **Only the author and maintainers use commands** is on. The author can't start it. Everything else follows the table.

## Re-running a review

- `review` queues a run for the change's head and target branch as the platform reports them now (the same rules as a push; the newer run replaces the one that was published). `review failed` also queues one but reviews only the skills that failed: a skill whose verdict is `failed`, or that is clean yet left files unreviewed, after dismissals. It keeps the other skills' results and publishes one merged review. With nothing failed it replies `nothing failed` and does nothing.
- `review failed` re-runs the whole review, and replies `the whole review failed, so all of it is re-run`, when there is no result per skill to build on: the earlier review was an error result, there is none for the current head and target branch, or it can't be read. It also reviews everything when the dispatcher, before it starts the Job, finds the earlier review can't be kept: it is of other commits, or a skill changed since: one was added, removed or renamed, a library skill it would keep has a new version (an edit, an install or an update; the run page's `library v3`), a skill moved between the library and a repository folder, or it names a skill twice. A re-run that could not be merged with the earlier results shows a note in the comment and is **never approved**.
- A head that is already queued, running or waiting to be published is not queued twice: the command still gets 👍. The exception is `@<bot> review`: it replaces a push's run that hasn't started its Job or that is incremental, and queues a full review, so a push's run is always followed by a full review (and it uses up a re-run). A run that an earlier `review` or `review failed` queued is not replaced: it is left to finish. The caps are checked first, though: at a cap, a re-run of such a head gets the cap refusal and 👎. A re-run of a draft is refused when the repository doesn't review drafts, and a command on a closed or merged change gets 👎 and no reply.
- When a monthly token quota is used up, `review` and `review failed` are refused and nothing is queued; see [Limits on commands](#limits-on-commands). An admin's button goes past it.
- The new review is decided like any other (see [How approval works](approval.md)). If it isn't approved, the bot's approval of that head is withdrawn.
- While a re-run of a commit is queued or running, the review it replaces is set aside. A dismiss is resolved against the newest review still published. When there is none, a comment gets the reply `no finding <id> in the latest review` and a button the answer `there is no such finding in the latest review` (404). A dismissal or restore made meanwhile is stored, and applies when the re-run is published (a restore always acts, on the finding's fingerprint).

## Dismissing a finding

- A dismissal belongs to one merge request. It is stored by the finding's **fingerprint**, so it survives pushes and line moves: when a later review reports the same finding, it stays dismissed.
- It covers the severity it was made at, and below. A dismissed blocking finding stays dismissed if it comes back as a suggestion; a dismissed suggestion that comes back as blocking shows again, and needs a dismissal by someone allowed to dismiss blocking findings. Dismissing a suggestion a second time as blocking raises it.
- `dismiss` and **Dismiss** start no run. pergent.sh decides the latest review again and publishes it: the comment is written again with a **Dismissed** section (collapsed by default; open or left out as the repository's [layout](comment-layout.md) says; each finding's title and ID, who dismissed it and the reason; the verdict line reads `✅ **No blocking issues** (N dismissed)` when only dismissed findings were blocking), the status is set again, and a review whose blocking findings are all dismissed is approved under the usual rules (approval on, not a draft, nothing failed). The dismissals are applied to the newest review of the change only; one whose head has moved on applies to the next review.
- **Restore** (a button on a dismissed finding) removes the dismissal and decides the review again. If that makes it not approved, the bot's approval is withdrawn; the withdrawal is retried until it succeeds, and for a merged change the record is just dropped.
- A blocking dismissal of a merged or closed change is refused for everyone, admins included: the button answers 409 `this change is merged` or `this change is closed` (and 502 when the platform can't say what state it is in). A comment on a change that isn't open gets 👎 and no reply, whatever the command.
- A dismissal that already exists at the same or a higher severity is left as it is: the reply is `finding <id> is already dismissed`.
- Dismissals are deleted when the repository is switched off ([Switching a repository off](switching-off.md)).

## Reactions and replies

The bot reacts to the comment so everybody sees what became of it (GitLab award emoji `eyes`, `thumbsup`, `thumbsdown`; GitHub reactions `eyes`, `+1`, `-1`):

- 👀 when the command is picked up;
- 👍 when it is done, or there was nothing to do;
- 👎 when it is refused, and one short reply says why.

A reply never says more than the commenter could see on the platform, and never repeats what they wrote except a finding ID reduced to hexadecimal digits. A reaction that can't be made never stops the command. A command that meets a platform or database error is tried again up to 3 times (after 15, 30 and 60 seconds), then given up on with 👎 and no reply.

| Case | Reaction | Reply |
| --- | --- | --- |
| Done | 👍 | none |
| `help`, or a command the bot doesn't know | 👍 | ``Commands: `@<bot> review` (full review) · `@<bot> review failed` · `@<bot> dismiss <id> <reason>` `` |
| `review failed` with no failed skill | 👍 | `nothing failed` |
| `review failed` without a result per skill | 👍 | `the whole review failed, so all of it is re-run` |
| `dismiss` of a dismissed finding | 👍 | `finding <id> is already dismissed` |
| Someone who is neither the author nor a maintainer, with the setting on | 👎 | `Only the author of this PR and the repository's maintainers can use pergent commands.` (`MR` on GitLab), on their first refusal on a change; 👎 only after that |
| No permission (also a blocking dismissal without Maintainer) | 👎 | `you don't have permission to do that here` |
| The change's author dismisses a blocking finding | 👎 | `the author of a change can't dismiss its blocking findings` |
| Blocking dismissal without a reason | 👎 | ``dismissing a blocking finding needs a reason: `@<bot> dismiss <id> <reason>` `` |
| The ID isn't in the latest review | 👎 | `no finding <id> in the latest review` |
| A draft, in a repository that doesn't review drafts | 👎 | `drafts aren't reviewed in this repository` |
| `review` on an untrusted author's change by someone without write access, the author included | 👎 | `only someone with write access can start the review of this change` |
| `review` on an untrusted author's change pushed after the comment was written | 👎 | `this change was pushed after your comment; start the review again` |
| A monthly token quota is used up (`review`, `review failed`) | 👎 | `Not reviewed: the monthly token quota is used up. Reviews start again on the 1st, or an admin can start this one from the pergent.sh dashboard.` the first time that month on the change; 👎 only after that |
| A re-run cap is reached | 👎 | `re-run limit reached for this MR; it resets at 00:00 UTC` (`PR` on GitHub) |
| The same command again within 30 seconds, the repository switched off, a GitLab bot as the author, or the change closed, merged or gone | 👎 | none |
| The commenter had a command on the change answered without doing anything (a reply of help, a refusal, a command with nothing to do, a drop, a command given up on after errors) less than 2 minutes ago | none | none |

## Limits on commands

- **Once per head.** A re-run of a commit already queued, running or waiting to be published isn't queued twice (the caps are checked before this, so at a cap such a request is refused with the cap reply). The exception is **Re-run review** (and `@<bot> review`): it replaces a push's run that hasn't started its Job or that is incremental, and queues a full review, so a push's run is always followed by a full review. It uses up a re-run. A run that an earlier re-run (also **Re-run failed only**) queued is not replaced: it is left to finish.
- **30 seconds.** A command that was carried out, or answered with nothing to do, is not accepted again for the same merge request, command and finding ID within 30 seconds (a refused one doesn't start the window).
- **Re-run caps.** Re-runs started by a comment or a button count, and so does the full review pergent.sh queues by itself when an incremental result can't be merged with the last review; pushes and newly opened merge requests always run and never count. The defaults are 10 per merge request and 50 per repository, per UTC day, and the reply names the reset time (00:00 UTC). A re-run that was superseded still counts. Starting the review of an untrusted author's change ([Trusted authors](trusted-authors.md)) counts against the merge request's cap only. The caps are set by admins only: in the dashboard under **Admin → Settings** (for repositories switched on in the dashboard), per repository in its **Settings**, and in the config as `repositories[].rerun_limit_per_mr` and `repositories[].rerun_limit_per_repo` (1 to 1000). Buttons are held to the caps too (HTTP 429 with the same text). Two buttons pressed at the same moment can overshoot a cap by a few.
- **Monthly token quota.** When the repository's or its instance's [monthly token quota](token-quota.md) is used up, `review` and `review failed` get 👎 with the reason `the monthly token quota is used up` and nothing is queued, so no re-run cap is used. The note is the reply only the first time that month for that change, because a held push shares the same once-a-month flag; later refusals get 👎 alone. A head whose review is already queued or running is answered as before, not refused. A change waiting for trust stays waiting for trust. The dashboard's **Re-run** and **Start review** answer 409 `the monthly token quota is used up`, except for an admin, whose press goes past the quota and counts its tokens.
- **One do-nothing answer per 2 minutes.** Every answer costs platform calls and a comment of the bot, so once a command of a person on a change has been answered without doing anything, their next command on that change within 2 minutes gets nothing at all: no reaction, no reply, no platform call, and it is not run. What counts is a `help` or an unknown command, a refusal (not the author or a maintainer, no permission, the author, a missing reason, an unknown ID, a draft, a cap, a used-up token quota), a command with nothing to do (`review failed` when nothing failed, a finding that is dismissed already), a drop and a command given up on after errors. The refusal for someone who is neither the author nor a maintainer starts the window too, also when it gets only a 👎. A command that was carried out (a re-run queued, a finding dismissed) does not start the window, so dismissing several findings in a row works, and so does a `review` followed by a `dismiss` once the re-run's review is published. While the re-run is still being reviewed there is no review to dismiss from: that `dismiss` is refused (`no finding <id> in the latest review`), and the refusal starts the window. A real command written within the window after a `help` or a refusal is throttled: write it again after the 2 minutes. The check is made before anything else, and commands that were themselves left unanswered don't extend the window.
- **Never hold up reviews.** The engine answers at most 10 commands per pass and never holds up reviews for more than about 30 seconds per pass. A command cut short because the pass ran out of that time is put off for 15 seconds without counting as a try.

## Setup

Commands need the platform to send comment events: GitLab note events (pergent.sh adds them to the hook it manages, see [Install with Helm](../setup/install.md#-install-with-helm)), and for GitHub the **Issue comment** event on the review App ([Create the review App](../setup/github.md#1-create-the-review-app)). Without them no command is received. Repositories that aren't switched on ignore commands, as they ignore pushes. Each command and button is recorded in the [audit log](dashboard.md#audit-log) as `review_rerun`, `review_rerun_failed`, `review_start`, `finding_dismissed` or `finding_restored`, a refusal about who may as `command_refused`, and a **Stop using** on the Past dismissals tab as `dismissal_stopped`; the actor of a comment is `username (platform instance)`.
