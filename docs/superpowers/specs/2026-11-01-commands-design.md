# pergent.sh Commands on the MR/PR: Design

**Date:** 2026-11-01
**Status:** Agreed in brainstorming and two grilling rounds, including the builder check. A plan comes next.
**Roadmap:** piece 2d of `docs/superpowers/specs/2026-10-29-pergent-sh-roadmap.md` ("MR commands and dashboard buttons"). It follows the dashboard (2c) and the organization piece, and comes before the skills library (piece 4). The dashboard design (`docs/superpowers/specs/2026-10-31-dashboard-design.md`) left room for these buttons as a non-goal of its own piece.

---

## Why

Today a review runs on every new MR/PR and every push, and nothing else can start or change one. When a review goes wrong (the LLM provider flaked, a skill failed, or a finding is simply not a real problem), the only way forward is to push again. A blocking finding the team agrees is wrong blocks the approval for good.

People want to act where they already are: in the MR/PR, by typing a comment, and on the dashboard's review page.

## Goals

- Re-run the whole review of the MR/PR's current head, from a comment or a dashboard button.
- Re-run only the skills that failed, keeping the other skills' results.
- Dismiss a finding as "not a real problem" (a false positive), from a comment or a button, and restore it (undo the dismissal) from the dashboard. A dismissed blocking finding no longer blocks the approval.
- Cap how many re-runs people can start, per MR/PR and per repository, so commands can't be abused.
- The same commands for GitLab and GitHub, for repositories in the config file and repositories switched on in the dashboard.
- Everyone sees what happened: the bot reacts to the comment, the review comment shows dismissed findings, and the audit log records who did what.

## Non-goals (this piece)

- Chatting with the bot, asking it questions, or asking it to explain or fix code.
- Replying to individual findings in line comments.
- Commands that change settings (skills, approval) from a comment. Settings stay on the dashboard.
- Dismissing a finding for every MR of a repository. A dismissal belongs to one MR/PR.

## Anti-goals (a failure even if it works)

- An MR/PR's author dismisses a blocking finding on their own change, and the bot approves it.
- Text that pergent.sh or the model wrote (the review comment, a finding's explanation) runs a command.
- A command gives someone more than their platform access or their pergent.sh team role allows.
- A command makes a review falsely pass. A failed, partial or draft review is still never approved.
- Commands flood the queue or the LLM bill: the same re-run requested twice runs once.
- A token or secret appears in a reply, reaction, log or audit entry.

## Decisions (agreed)

- **Commands in a comment** on the MR/PR, addressed to the bot:
  - `@<bot> review`: run the whole review again on the current head.
  - `@<bot> review failed`: re-run only the skills whose last result was `failed`.
  - `@<bot> dismiss <finding id>`: mark that finding as not a real problem.
- **The same actions as buttons** on the dashboard's review page: Re-run review, Re-run failed only, Dismiss (per finding), and Restore (per dismissed finding).
- **Who can re-run:**
  - in a comment, anyone with Developer or above on GitLab, or write access on GitHub;
  - on the dashboard, maintainers and admins of that repository.
- **Who can dismiss:**
  - **a blocking finding:**
    - never the MR/PR's author, on their own change;
    - otherwise only someone with Maintainer or above on GitLab, or maintain or admin on GitHub, or the maintainer or admin role on a pergent.sh team that covers the repository (password admins included);
  - **a suggestion:** anyone who can re-run.
- **A dismissed finding counts as resolved.** A review whose remaining blocking findings are all dismissed is approved like a clean one, under the same approval rules as today (approval on, not a draft, nothing failed).
- **A dismissal sticks** to that MR/PR: when a later push or re-run reports the same finding again, it stays dismissed.
- **Feedback:** the bot reacts to the comment so people know it was picked up. The audit log records every command and button press.
- **Addressing the bot:** a mention of the review bot (GitLab: its username; GitHub: the review App's slug), e.g. `@pergent-review review`. A comment without the mention is ignored.
- **Accepted comments:** only newly created top-level comments on the MR/PR; edits, deletions, line comments and thread replies are ignored. Comments by pergent.sh's review or approve bot, or any `[bot]` account, never run a command. Only the first command in a comment runs.
- **A reason is required to dismiss a blocking finding:** `@pergent-review dismiss a1b2c3 the input is a constant`. Without one the bot replies asking for it and does nothing. For suggestions the reason is optional.
- **Restore is a dashboard button only** (no comment command). Anyone who can re-run may restore, the MR/PR's author included, since restoring only makes the review stricter. Restoring a blocking finding withdraws a bot approval.
- **"Re-run failed"** re-runs only the skills whose last result was failed (a skill the policy treats as failed, e.g. one that left files unreviewed, counts) and keeps every other skill's result. When there is nothing per skill to pick from (the whole review errored, the Job died, or the current head has no review yet) it does a full re-run and replies "the whole review failed, so all of it is re-run".
- **Dismiss and restore don't run the LLM:** they re-decide and re-publish the latest review (comment, status, approval) with the change applied.
- **Dismissed findings stay visible** in a collapsed "Dismissed" section of the review comment, with who dismissed each and the reason.
- **A dismissal applies only up to the severity it was made at:** a dismissed suggestion that comes back as blocking shows again and needs a dismissal by someone allowed to dismiss blocking findings.
- **Permissions:** a comment command is checked against the commenter's GitLab/GitHub access at that moment; pergent.sh team roles apply to dashboard buttons.
- **Reactions and replies:** 👀 when picked up, 👍 when done, 👎 plus one short reply when refused (no permission, unknown finding ID, draft not reviewed here, missing reason, limit reached). A reply never says more than the commenter can see on the platform.
- **Abuse limits:**
  - the same re-run requested for a head already queued or running is not queued twice;
  - a comment command is accepted at most once per MR/PR per 30 seconds per command and finding ID; a command dropped by this gets a 👎;
  - **re-run caps:** command and button re-runs only (pushes and newly opened MRs always run) — at most 10 per MR/PR per day and 50 per repository per day by default, set in the dashboard defaults, overridable per repository, and in the config file for config repositories. At the cap the bot reacts 👎 and replies when it resets; nothing is queued.
- **The comment writer:** pergent's comment formatter moves into a public Go package in the pergent repository, and pergent.sh uses it (pinned to a commit; no pergent release needed) to re-render the comment after a dismissal or a partial re-run, so blocking counts stay right and finding IDs stay unique under pergent's ID rule. This piece includes that change in the pergent repository.

## My calls (made while writing; the user can overturn any)

### Commands

- **Addressing the bot:**
  - **GitLab:** a mention of the review bot's username (`@pergent-review` if that is the bot's name).
  - **GitHub:** a mention of the review App's slug (`@pergent-review` for an App named that).
  - The command is the first word after the mention, on the same line. Only the first command in a comment runs.
  - A comment without a mention is ignored, so ordinary discussion can say "review" freely.
- **Accepted comments:**
  - only newly created comments on the MR/PR (GitLab note events, GitHub `issue_comment` with action `created` on a pull request);
  - edits and deletions are ignored;
  - comments written by the review bot or the approve bot, or by any account whose username ends in `[bot]`, are ignored, so neither pergent.sh's own comment nor model text quoted in it can run a command.
- **`dismiss` syntax:**
  - `dismiss <id>` with an optional reason after the ID (`@pergent-review dismiss a1b2c3 the cache key is per request`);
  - the ID is the one shown in backticks after each finding's title in the review comment;
  - it is matched against the latest published review of that MR/PR.
- **Unknown command or bad ID:** the bot replies once with a short list of the commands, and does nothing else.
- **`help`:** `@<bot> help` replies with the same short list.

### Behaviour

- **Re-run:**
  - queues a run for the MR/PR's current head and target, as a push would (the same supersede rules);
  - if a run for that head is already queued or running, the request is acknowledged and not queued again;
  - a re-run on a draft when the repository doesn't review drafts gets a reply saying so, and does nothing.
- **Re-run failed:** the new results replace the re-run skills' results; with no failed skill and a complete review, the bot replies "nothing failed" and does nothing.
- **Dismiss and restore:**
  - stored per MR/PR by the finding's **fingerprint**, so the dismissal survives line moves and later pushes;
  - they don't start a new run: pergent.sh re-decides and re-publishes the latest review (comment, status, approval) with the dismissal applied;
  - dismissing a finding that is already dismissed, or restoring one that isn't, does nothing and says so.
- **The review comment** lists dismissed findings in their own collapsed section, with who dismissed each and the reason. It is never deleted.
- **Approval with dismissals:**
  - the published decision counts only blocking findings that aren't dismissed;
  - restoring a blocking finding re-decides, which withdraws a bot approval exactly as a new blocking review would.
- **Reactions:**
  - 👀 when a command is accepted;
  - 👍 when it was done;
  - 👎 plus a short reply when it was refused (permission, unknown ID, draft).
  - The reply never says more than the commenter could see on the platform.
- **Permission checks:**
  - use the commenter's access on the platform at the time of the command, read with the review bot's token or App installation;
  - the MR/PR's author is the author the platform reports for the MR/PR;
  - the team role applies only to dashboard buttons, where the person is signed in. A comment author is checked on the platform only.
- **Audit:**
  - `review_rerun`, `review_rerun_failed`, `finding_dismissed`, `finding_restored`;
  - each records the actor, the MR/PR, the head and the finding fingerprint;
  - comment commands record the actor as "username (platform instance)", like dashboard actions.

### Limits

- "A day" is the calendar day in UTC; the reply names the reset time.
- Dashboard buttons count toward the caps too; nobody is exempt.
- Only admins change the caps (defaults and per repository), and the audit log records it.

### Approval

- Restoring a blocking finding, or re-deciding a head as no longer approved, withdraws the bot's approval of that head. This is a new step: today an approval is withdrawn only when a new head or target arrives.

### Webhooks and platforms

- **GitLab:** the project hook pergent.sh manages also subscribes to note (comment) events; reconcile updates existing hooks. Only top-level MR notes are read (not line comments or thread replies), for parity with GitHub's `issue_comment`.
- **GitHub:**
  - the review App must subscribe to the "Issue comment" event;
  - the README's App setup lists it, and says to add it to existing Apps;
  - reacting to comments needs no permission beyond what the App already has (the builder confirms; see below).
- **Repositories switched off** ignore commands, as they ignore pushes today.

## Constraints

- **Everything in the dashboard's and organization pieces' constraints still holds:**
  - public repository, so no internal names;
  - one replica;
  - the engine loop never waits on a handler;
  - tokens never reach a Job, log, page or the browser beyond what each Job needs.
- **Commands go through the existing queue and publish paths.** No second way to run or publish a review.
- **Webhook handlers stay fast:** they verify, record and answer. Platform calls for permissions and reactions happen off the request path, in the engine or a worker that never blocks reviews.

## Left to the builder

- How a comment command is queued and processed (a commands table drained by the engine, or a short-lived worker), as long as the webhook answers quickly and the engine loop isn't blocked.
- The exact GitHub permission needed to react to issue comments and to read a user's repository permission, and the README wording for existing Apps.
- How "re-run failed" passes the skill subset to the Job and merges the partial result with the kept skills' results, and how the merged result is stored.
- Table names and columns for dismissals and commands, and the migrations.
- The review page's button layout, within the existing look.
- How reactions are retried when the platform is briefly down. A missed reaction must never block the command itself.
