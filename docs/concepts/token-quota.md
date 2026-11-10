# 🪙 Monthly token quota

Every review spends tokens on your LLM account. A busy repository, a loop of pushes or re-runs, or a team with write access can use the account up, and then every repository's reviews fail. A **monthly token quota** is a ceiling no one can get past without an admin: once it is used up, no new review starts on it and no LLM call is made, until the 1st of the next month or until an admin starts one.

A quota is optional. With none set anywhere, pergent.sh behaves exactly as before.

## Where to set it

Only admins set or change a quota. Maintainers and viewers see neither the fields nor the usage.

| Quota | In the dashboard | In the config | Empty means |
| --- | --- | --- | --- |
| An instance's | **Admin → Integrations**, the instance's card | `gitlab[].monthly_token_quota`, `github[].monthly_token_quota` | Unlimited |
| A repository's | The repository's **Settings** | `repositories[].monthly_token_quota` | Limited only by its instance's quota |

- A quota is a whole number of tokens, from 1 to 1,000,000,000,000,000. Zero and negative numbers are refused. To stop a repository's reviews, switch it off.
- **A repository's quota sits inside its instance's.** A review starts only if the repository has room under its own quota (if it has one) **and** the instance has room under the instance's (if it has one). The two are separate numbers: a repository's quota doesn't default to the instance's, and may be larger than it (then the instance's is the one that bites).
- When the instance's quota runs out, every repository on it is held, including those with room left under their own quota.
- **The config file wins.** An instance or repository whose quota is set in the config shows it read-only in the dashboard, as for [Trusted authors](trusted-authors.md). For an instance in the config the dashboard route answers 409.
- A maintainer's **Reset to defaults** on a repository leaves its quota as it is. An admin's reset clears it.
- Changes are recorded in the [audit log](dashboard.md#audit-log): an instance's as `instance_updated` with the detail `monthly_token_quota=<n>` or `monthly_token_quota=none`, a repository's as `settings_updated` naming `monthlyTokenQuota`.

## What counts

- **Input plus output tokens**, the same measure as [Usage](usage.md). Cache and reasoning tokens are not added.
- The tokens of reviews that **finished in the current calendar month, in UTC**. A review that started on the 31st and finished on the 1st counts in the new month.
- A repository's usage is its own reviews. An instance's usage is all its repositories', switched-off and removed ones included: those tokens were spent.
- A review that a quota held costs nothing, so it counts nothing.

## When a quota is used up

The check is "usage so far this month is below the quota". Usage exactly at the quota counts as used up.

**A push** (and the full review pergent.sh queues by itself when an incremental result can't be merged) is held, just before a Job would be created:

- no Job and no LLM call;
- a pending status on the head, set again on each new head: `Monthly token quota used up. An admin can start the review in the dashboard.` A required pergent check keeps the change unmergeable, and the change is never approved;
- one note on the change, the first time in a calendar month that a quota holds it: `Not reviewed: the monthly token quota is used up. Reviews start again on the 1st, or an admin can start this one from the pergent.sh dashboard.` Later pushes that month post nothing. The note says nothing about numbers, and not which quota ran out, because on a public repository anyone reads it;
- the change is listed under **Waiting for a start** on the repository's page, with the line `Monthly token quota used up.` Everyone who can see the repository sees the entry; only admins get the **Start review** button.

Trust is checked before the quota: an untrusted author's change waits for [trust](trusted-authors.md) first and doesn't touch the quota.

**A request by hand** (`@<bot> review`, `@<bot> review failed`, or a maintainer's **Re-run** or **Start review**) is refused, and nothing is queued, so it uses no re-run cap:

- a comment gets 👎 and the reason `the monthly token quota is used up`. The note above is the reply only the first time that month for that change (it shares the once-a-month flag with the held pushes); later refusals get 👎 alone;
- a button answers 409 `the monthly token quota is used up`, shown beside the button;
- an earlier review of the change, its status and the bot's approval stay as they are;
- if a review of that head is already queued or running, the request is answered as "already queued", not refused;
- a change waiting for trust that someone with write access tries to start stays waiting for trust;
- when the usage can't be read, the request is answered as failed and nothing is queued.

**An admin** pressing **Start review** or **Re-run** in the dashboard starts a review whatever the quotas say. Its tokens count like any other. The caps are as for any start or re-run. Starting a held change reviews the whole current head.

## The 1st of the month

Usage is counted from the 1st, so the quota has room again with nothing to reset. **Nothing starts by itself:** a held change stays held until its next push (which is checked again), a `review` command from someone allowed to give one, or an admin's start. This keeps day one from being eaten by a burst of old reviews. Raising or removing a quota mid-month works the same way. Lowering a quota below what is used holds the next review.

## What the dashboard shows

Admins only. The repository page shows `Tokens this month: <used> of <quota>` with a bar for a repository with a quota, and the instance's line under it when the instance has one. **Integrations** shows the same on each instance with a quota. The bar is **amber from 80%** and **red when used up**. A repository or instance with no quota shows its usage without a bar. The [Usage](usage.md) pages show rolling days and have no quota column.

## Good to know

- **The quota can be overshot.** A review's size is known only after it runs, so the check doesn't predict it. A review that starts with a little room left can take the month past the quota, and reviews started together before any of them reports can all start. A review that was asked for with room left runs even if the quota is used up before it is dispatched. A review already running when the quota runs out finishes and is published as usual.
- **A known gap.** The quota counts the tokens recorded in [Usage](usage.md). A Job with no result (timeout, out of memory, deleted) and a superseded review count as zero.
- **When the usage can't be read** (a database error) at dispatch, the dispatch is tried again with the usual backoff, and then the review fails as any dispatch does. No review starts unchecked, and none passes.
- **Rolling back removes the quota.** The previous release has no quotas and ignores them. During a rolling upgrade its replicas start reviews without the check. A held change is not picked up by the previous release by itself, but its **Start review** and a writer's `review` command can start one. See [Upgrading](../upgrading.md).
