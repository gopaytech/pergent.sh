# Only the author and maintainers use commands

pergent.sh 0.6.2. Status: agreed with the owner on 2026-11-04 (brainstorm in chat).

## Why

pergent.sh answers commands written in a merge request or pull request: `review`, `review failed`, `dismiss <id> <reason>`, `help`, and, since 0.6.0, `dismiss <reason>` as a reply in a finding's thread. Today anyone with write access to the repository may re-run a review or dismiss a suggestion on any change, not only their own.

The owner wants this closed to **prevent abuse**. On a repository where many people have write access, one person could:
- re-run reviews on other people's changes, spending tokens and Job time;
- dismiss suggestions to quiet a review that isn't theirs.

The people who should talk to pergent about a change are the person who opened it and the people who own the repository.

## Goals

- Only the change's **author** and the repository's **maintainers or owners** can use pergent commands on a change.
- Everyone else who tries is refused visibly and recorded, so abuse attempts can be traced.
- A repository can turn the restriction off and get today's behaviour back.

## Non-goals

- Changing what the dashboard buttons allow. Re-run, Dismiss and Restore already need a maintainer or admin of the repository.
- Changing the throttles, the re-run caps, or the bot filtering.
- Changing who may dismiss a **blocking** finding. It stays a maintainer or owner who is not the change's author.

## Anti-goals (a failure even if it works)

- **The author of a change locked out of their own change.** The author can always re-run its review and dismiss its suggestions.
- **A person counted as the author by mistake.** When pergent.sh can't tell who opened the change, nobody counts as the author. Unknown never grants access.
- **A refusal about who may that leaves no trace.** Every answered refusal because of who the commenter is shows on the platform and in the audit log, whether the switch is on or off.

## Decisions (the owner's)

| Decision | Choice |
|---|---|
| Who may use commands | The change's author, and the repository's maintainers and owners: GitLab Maintainer or Owner; GitHub `maintain` or `admin` |
| The author's access | **Always allowed**, whatever their repository access (read access from a fork included) |
| What the author can do | What an author can do today: re-run (`review`, `review failed`) and dismiss suggestions. Never dismiss a blocking finding on their own change |
| Which commands | All of them, in a comment or in a finding's thread: `review`, `review failed`, `dismiss`, `help` and unknown commands |
| The author | Whoever opened the change (`Change.AuthorID`). Someone who only pushed commits to it, or is a co-author or reviewer, is not the author |
| Refusal | 👎 plus the reply below, **once per person per change**; later refusals of the same person on the same change get 👎 only |
| Refusal record | An entry in the dashboard's audit log for each answered refusal about **who may**, in both modes. That covers the new author-or-maintainer gate, and the existing "no permission" refusals: someone without write access re-running or dismissing (switch off), and a non-maintainer or the author trying to dismiss a blocking finding. Ordinary mistakes are not recorded: an unknown finding ID, a missing reason, the re-run cap, not a finding thread. A command silenced by the existing 2-minute throttle is not answered and not recorded, as today |
| Setting | A per-repository yes/no switch, **on by default**, also for repositories already running when 0.6.2 is deployed. Turned off, write-access non-authors are let back in as today. The author stays always allowed in both modes |
| Who can turn it off | Maintainers and admins of the repository, like the other repository settings; also the Admin defaults and the config file |
| Dashboard buttons | Unchanged |

**Refusal reply (exact):** `Only the author of this PR and the repository's maintainers can use pergent commands.` On GitLab "PR" reads "MR", the same term switch the other replies use (`actions.Term`).

## How it works

### Who counts

- **Author:** the commenter's numeric user ID equals the change's author ID (`forge.Change.AuthorID`), and both are known (non-zero). The existing `actions.IsAuthor` deliberately treats an unknown ID as "the author", which is right for refusing a blocking dismissal but wrong here. This check needs its own strict comparison.
- **Maintainer or owner:** the commenter's access is `forge.AccessMaintain` (GitLab Maintainer and Owner; GitHub `maintain` and `admin`), read from the platform as today.

### When the switch is on (the default)

Before any command is carried out, after the checks that already run (throttle, repository switched on, commenter not a bot, repeat check, change open), a commenter who is neither the author nor a maintainer or owner is refused:
- the reaction is 👎;
- the reply above is posted, in the finding's thread when the command was written there, but only for this person's first refusal on this change. Later refusals get 👎 only, so the bot can't be made to post a stream of replies;
- an audit entry is written with action `command_refused`, the commenter as actor, and detail naming the change, the head commit and the command kind.

Then each command applies today's rules, except that the author also passes the "write access" check for re-runs and suggestion dismissals:
- `review` and `review failed`: the author or a maintainer or owner;
- dismissing a suggestion: the author or a maintainer or owner;
- dismissing a blocking finding: a maintainer or owner who is not the author (unchanged);
- `help`: the author or a maintainer or owner.

The existing "no permission" refusals of the per-command rules (a non-maintainer, or the author, refused a blocking dismissal) also write a `command_refused` audit entry, with the same detail.

### When the switch is off

The existing "no permission" refusals also write a `command_refused` audit entry. Otherwise, as today: anyone with write access may re-run and dismiss suggestions, maintainers and owners dismiss blocking findings, and anyone may ask for `help`. The one difference from 0.6.1 is that the author is always allowed to re-run and dismiss suggestions, even without write access. That follows from "author is always allowed".

### The switch

| Where | Key / control | Default |
|---|---|---|
| Config, one repository | `repositories[].commands_author_and_maintainers_only` | on |
| Dashboard, one repository | Repository → Settings → **Only the author and maintainers use commands** | on |
| Dashboard default | Admin → Settings → the same switch, with the "From config" hint of 0.6.1 | on |

Help text: "When on, only the author of a change and the repository's maintainers and owners can use pergent commands on it. When off, anyone with write access can."

It follows the pattern of the other default-on switches (`approve`, `inline_findings`): nullable columns on `dashboard_repos` and `repo_defaults` (migration `0019`), the settings resolution order, the dashboard JSON key `commandsAuthorAndMaintainersOnly`, reset clears it, and the Admin defaults view includes it in `config` and `pinned`.

## Release

pergent.sh **0.6.2**. The upgrade note says:
- Commands are now limited to the change's author and the repository's maintainers and owners. Turn it off per repository (**Only the author and maintainers use commands**, or `commands_author_and_maintainers_only: false`) to let anyone with write access use them, as before.
- Migration `0019` runs at startup and adds nullable columns only.
- To roll back to 0.6.1, first remove `commands_author_and_maintainers_only` from the config, because 0.6.1 refuses unknown config keys.

The README's command permission table and its "Who can" lines change to match.

## Left to the builder

- Where in `handleCommand` the check sits, as long as it runs after the bot and repeat checks and the change is known. It reads the commenter's access once and reuses it for the per-command rules.
- The audit detail format, following the existing `audit` helper.
- Test layout, following the existing command tests (`internal/engine/commands_test.go`, e2e tests).
