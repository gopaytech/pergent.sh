# Trusted authors: no automatic review for strangers

## Why

pergent.sh reviews every merge request (pull request) opened on a repository that is switched on, whoever opened it. On a public repository that means anyone on the internet can start a review run, usually from a fork, since that is the only way someone without write access can open one. The owner does not trust everyone on the internet, and four things follow from that:

- **The LLM bill.** Many changes, large changes, or many pushes each start a run on the owner's provider account.
- **A bought approval.** Text in the diff written for the model (prompt injection) talks the review into "no blocking issues", and the approve bot approves.
- **A leak.** The change tries to get the run to read or post something it shouldn't.
- **Noise.** Runs and bot comments on junk changes fill the repository and the dashboard.

The fix: on a public repository, only **trusted authors** get automatic reviews. A change from anyone else waits until someone with write access starts its review.

## Goals

- A stranger's change starts no run, costs no LLM call, and gets at most one short note from the bot.
- Someone with write access can still have a stranger's change reviewed, one head at a time.
- Public repositories are protected after the upgrade without anyone having to turn something on.
- Trusted contributors without write access (a regular outside contributor, a bot account) can be put on a repository's allowlist and are reviewed as today.

## Non-goals

- Changing what a review does once it runs (the sandbox, the read-only agent, token handling). This design decides only *whether* a run starts.
- Telling fork changes apart from branch changes. Trust is decided by the author, never by where the branch lives.
- Rate limits or budgets per author or per repository.
- Private and internal repositories: nothing changes there unless someone turns the setting on.

## Anti-goals

These would make the feature a failure even if it "works":

- **A false trust.** Any path where an untrusted author's change runs without a person with write access asking for it. An error while checking the author's access, an unknown author, or a missing field is **untrusted**, never trusted.
- **The author starting their own review.** An untrusted author commenting `@<bot> review` (or anything else) must not start a run.
- **A paid-for note flood.** The bot's own answer to an untrusted change must cost no LLM call and at most one comment per change.
- **Breaking existing private setups.** Private and internal repositories that review fork changes today keep doing so after the upgrade.

## At a glance

On a repository with **Trusted authors only** on (a public repository, unless someone turned it off):

| Who opens or pushes the change | Reviewed automatically? | Can start a stranger's review? | Approved when the review is clean? |
|---|---|---|---|
| Stranger (no access, or read only), usually from a fork | ❌ waits: one note, pending status | ❌ (not even their own) | ✅ once someone with write access started it |
| On the repository's allowlist, no write access | ✅ | ❌ | ✅ |
| Write access (GitLab Developer, GitHub write) | ✅ | ✅ `@<bot> review` | ✅ |
| Maintainer and above | ✅ | ✅ `@<bot> review` | ✅ |
| Dashboard admin, or maintainer of the repository's team | (not an author) | ✅ **Start review** button | n/a |

| What happens | Result |
|---|---|
| A stranger opens a change | No run, no LLM call. One note, pending status. Listed as waiting in the dashboard. |
| The stranger comments `@<bot> review` | 👎 `only someone with write access can start the review of this change` |
| Someone with write access comments `@<bot> review` | A run on the current head; counts against the per-change cap, not the repository's daily cap |
| The stranger pushes again | Waits again: pending status, no second note |
| The author's access can't be checked (after about 10 minutes of retries) | No run, no note; pending status saying the check failed |
| Private or GitLab internal repository, setting unset | As today: every author reviewed automatically |
| Public repository, setting turned off | As today: every author reviewed automatically |

## Behaviour

### Who is trusted

The author of a change is **trusted** on a repository when either holds at the time of the event:

1. They have **write access or better** to the repository. On GitLab that is Developer or above, inherited access included. On GitHub it is write, maintain or admin. This uses the existing `forge.AccessLevel` (`AccessWrite` or `AccessMaintain`).
2. They are on the repository's **allowlist**.

Trust is checked on every event that would start a run (open, reopen, push, retarget, a draft marked ready). It is never cached across events, so someone removed from the allowlist or the repository is untrusted from their next push.

The allowlist decides automatic reviews only. It gives no command rights: an allowlisted user without write access has, on their own change, what any author has, and nothing on other people's changes.

### Where the check runs

The check runs in the engine, **after** the webhook, never inside the webhook request (webhook handlers make no platform calls today; GitLab disables a hook that keeps timing out, and neither platform resends a failed delivery).

1. The webhook records the head as **waiting for the trust check**: a run that is never dispatched while it waits. No Job and no LLM call can start from it.
2. The engine reads the author's access and the repository's visibility. A passing error (timeout, 5xx, rate limit) is retried with backoff for up to about 10 minutes.
3. **Trusted:** the run goes ahead as today.
4. **Untrusted:** the run is closed without running, and the note and the pending status follow (below).
5. **Still failing after the retries:** the run is closed without running, **no note** (it might tell a trusted author something false), and only a pending status: `Couldn't check the author's access. Someone with write access can start the review with @<bot> review`. The error is logged.

### The setting

A repository setting, **Trusted authors only**: dashboard switch on the repository's Settings, key `trustedAuthorsOnly` in the API, `trusted_authors_only` in the config file. It works like the other repository settings: an override per repository, a default on **Admin → Settings**, and a config key that makes a config-managed repository read-only in the dashboard.

- **Default: on for public repositories, off for private and internal ones.** "Unset" means "follow the visibility", so an existing public repository is protected the moment the upgrade is running, and a repository made public later becomes protected too.
- An explicit on or off, on the repository or as the instance default, wins over visibility.
- Visibility is read from the platform: GitHub's repository `visibility`, GitLab's project `visibility` (`public`, `internal`, `private`). GitLab `internal` (visible to every account on that instance, not to the internet) counts as private. On GitLab this is new: `gitlabforge.Visibility` currently returns `ErrUnsupported`. **If visibility can't be read, the repository counts as public** (fail closed): changes from untrusted authors wait. The error is logged.

### The allowlist

- **Per repository only.** There is no instance-wide or server-wide list. The repository's Settings, field `trustedAuthors`; config `repositories[].trusted_authors` (a config-managed repository's list is read-only in the dashboard).
- An admin types a platform username. pergent.sh resolves it to the platform's numeric user ID when the list is saved, stores the ID with the username beside it for display, and matches authors **by ID**. Someone who renames stays trusted; someone who takes over a renamed or freed username is not. A username that can't be resolved (unknown, or the platform call fails) makes the save fail with an error naming it; nothing is half-saved.

### A change from an untrusted author

When the setting is on and the author is not trusted:

1. **No run** is queued for that head.
2. **One note** on the change, the first time this happens on it, never again on later pushes:
   > Not reviewed automatically: the author isn't a trusted author of this repository. Someone with write access can start a review with `@<bot> review`.

   It is a plain comment with no LLM involved. "Posted already" is stored per change, so a restart or a webhook replay doesn't post it twice.
3. **A commit status** on the head, so branch protection that requires pergent's check keeps the change unmergeable until it is reviewed: state `pending`, description `Waiting for someone with write access to start the review`. This is set again on each new head; it costs one platform call and no LLM call.

### Starting the review of an untrusted change

- `@<bot> review` from someone with **write access or better** starts a run on the **current head**. This holds even when "Only the author and maintainers use commands" is on: for starting the review of an untrusted change, write access gets past that check. Every other command keeps today's check.
- A start counts against the per-change re-run cap (10 by default), so one change can't loop, but **not** against the per-repository daily cap (50), so a busy public repository doesn't use up its maintainers' own re-runs. It is its own run trigger, shown as a start in the dashboard and audit log.
- The same command from anyone else on an untrusted change (the author, a reader, an allowlisted user without write access) starts nothing. It gets 👎 and a reason in the commands table: `only someone with write access can start the review of this change`. It counts against the existing "one do-nothing answer per 2 minutes" limit, so it can't be used to make the bot comment in a loop.
- The dashboard's **Re-run** button counts as a person with write access. Dashboard users who can see it are already maintainers of the repository or admins.
- **Each new head needs a new start.** A push to an untrusted change starts no run, even if an earlier head was reviewed. The commit status goes back to `pending` with the waiting text, and no second note is posted.
- Once started, the run is a normal review. **A clean review approves as usual** (the owner's choice: the person with write access who started it is the trust).

### Waiting changes in the dashboard

The repository page lists changes waiting for a start (for example `!12 by stranger · waiting for a start`) with a **Start review** button, for admins and maintainers of the repository's team. Pressing it counts the same as `@<bot> review` from someone with write access: a run on the current head, the per-change cap, not the daily cap. A change leaves the list once a run starts on its current head, or when it is merged or closed.

### Other commands

- `dismiss` and the other commands keep today's rules ("Only the author and maintainers use commands"). An untrusted author can still dismiss suggestions on their own change; the README's existing rule already stops them from dismissing a blocking finding.

## Edge cases

- **Many junk changes:** each gets one note and a pending status, with no cap. No LLM call is made for any of them; abuse at that scale is for the platform's own spam controls.

- **Bot authors** (Dependabot, Renovate, a GitLab project bot): treated like anyone else. A bot with write access is trusted; otherwise put it on the allowlist.
- **The author's access can't be checked** even after the retries: see "Where the check runs", step 5. A later event checks again.
- **The setting is turned off** while untrusted changes are waiting: nothing starts by itself. Their next push, or a `@<bot> review`, starts a run as usual.
- **The setting is turned on** while an untrusted change's run is queued or running: that run finishes. Later heads follow the new rule.
- **A trusted author pushes to an untrusted author's change** (a maintainer helping on a fork change): the author of the change decides, not the pusher. It still waits. The alternative, trusting the pusher, would let anyone with push access to the fork's branch be "the pusher".
- **Ownership transfer or visibility change:** visibility is read when it's needed (see "Left to the builder" for caching), so a repository made public is protected from its next event.

## Upgrade

- A new migration (the next free number on `main` when the work starts; `0031` today, re-checked against parallel work) adds the per-repository setting, the per-repository list, the instance default for the setting, and the per-change "note posted" flag.
- Nothing is turned on explicitly: an unset setting follows visibility, which is what protects existing public repositories.
- The README's **Upgrading** section says that public repositories stop reviewing changes from authors without write access, and how to turn it off or add an allowlist.
- The previous release keeps working on the same database: new columns are nullable or defaulted, and nothing it reads changes meaning.

## Testing

What must be pinned by tests, at minimum:

- Untrusted author on a public repository: no run queued, one note, pending status; a second push adds no note.
- The author's `@<bot> review` on their own untrusted change starts nothing and gets the refusal; a writer's command starts a run on the current head; the next push waits again.
- Access check fails: no run.
- Visibility can't be read: treated as public.
- Allowlist on the repository, matched by user ID (a renamed user stays trusted); removed from the list → untrusted on the next push; an allowlist on one repository trusts nobody on another.
- Private repository with the setting unset: unchanged behaviour (fork changes are reviewed).
- Explicit off on a public repository: everyone reviewed as today.
- Config-managed repository: `trusted_authors_only` and `trusted_authors` read from config; the dashboard shows them read-only.
- GitLab and GitHub both, including GitLab inherited Developer access.
- A GitHub `write` user starts a stranger's review while "Only the author and maintainers use commands" is on; their `dismiss` on someone else's change is still refused.
- A passing platform error during the check is retried and the trusted author's run goes ahead; a check that fails for the whole retry window posts no note.
- A waiting run is never dispatched, including by the previous release on the same database.
- Starts don't use the repository's daily cap; 10 starts on one change hit the per-change cap.
- The dashboard lists a waiting change and **Start review** starts it; a viewer of the team doesn't see the button and the route refuses them.

## Left to the builder

- How long visibility may be cached (minutes, not hours), as long as a failure to read it is "public".
- How the waiting run is stored (a new run state, or a separate table), as long as it can never be dispatched before the check passes and the previous release ignores it safely.
- Dashboard layout of the two lists and the switch, following the existing Settings pages.
