# 🪙 Monthly token quota

Every review spends tokens on your LLM account. A busy repository, a loop of pushes or re-runs, or a team with write access can use the account up, and then every repository's reviews fail. A **monthly token quota** is a ceiling no one can get past without an admin: once it is used up, no new review starts on it and no LLM call is made, until the first push after the 1st of the next month, or until an admin starts one.

A quota is optional. With none set anywhere, pergent.sh behaves exactly as before.

## Where to set it

Only admins set or change a quota. Maintainers and viewers see neither the fields nor the usage.

| Quota | In the dashboard | In the config | Empty means |
| --- | --- | --- | --- |
| An instance's | The instance's card on the admin **Integrations** page | `gitlab[].monthly_token_quota`, `github[].monthly_token_quota` | Unlimited |
| A repository's | The repository's **Settings** | `repositories[].monthly_token_quota` | Limited only by its instance's quota |

- A quota is a whole number of tokens, from 1 to 1,000,000,000,000,000. Zero and negative numbers are refused. To stop a repository's reviews, switch it off.
- **A repository's quota sits inside its instance's.** A review starts only if the repository has room under its own quota (if it has one) **and** the instance has room under the instance's (if it has one). The two are separate numbers: a repository's quota doesn't default to the instance's, and may be larger than it (then the instance's is the one that bites).
- **The instance's quota covers the global keys only.** When it runs out, reviews skip the global entries of their [model chain](models.md) and run on a team's own entry when the chain has one. A review with only global entries left is held, including on a repository with room left under its own quota. The repository's quota counts every token, on team keys too, and holds the whole review.
- **The config file wins.** An instance or repository whose quota is set in the config shows it read-only in the dashboard, as for [Trusted authors](trusted-authors.md). For an instance in the config the dashboard route answers 409.
- A maintainer's **Reset to defaults** on a repository leaves its quota as it is. An admin's reset clears it.
- Changes are recorded in the [audit log](dashboard.md#audit-log): an instance's as `instance_updated` with the detail `monthly_token_quota=<n>` or `monthly_token_quota=none`, a repository's as `settings_updated` with the detail `monthlyTokenQuota=<n>` or `monthlyTokenQuota=none`.

## What counts

- **All tokens: input, cache read, cache write, output and reasoning**, the same measure as [Usage](usage.md). All 5 parts count equally.
- The tokens of reviews that **finished in the current calendar month, in UTC**. A review that started on the 31st and finished on the 1st counts in the new month.
- A repository's usage is its own reviews, on any key. An instance's usage is all its repositories' tokens on global keys, switched-off and removed ones included: those tokens were spent. Tokens on a team's own keys are not in the instance's usage.
- An attempt dropped for the next entry of its chain counts too, on the entry that used the tokens ([Attempts](models.md#attempts)).
- A review that a quota held costs nothing, so it counts nothing.
- **Every Job of a multi review counts**: each reviewer and the judge ([Multi review](multi-review.md)).

## When a quota is used up

The check is "usage so far this month is below the quota". Usage exactly at the quota counts as used up.

**A push** (and the full review pergent.sh queues by itself when an incremental result can't be merged) is held, just before a Job would be created, when the repository's quota is used up, or when the instance's is and no team entry is left in the chain:

- no Job and no LLM call;
- a pending status on the head, set again on each new head: `Monthly token quota used up. An admin can start the review in the dashboard.` A required pergent check keeps the change unmergeable, and the change is never approved;
- one note on the change, the first time in a calendar month that a quota holds it: `Not reviewed: the monthly token quota is used up. Reviews start again with the first push after the 1st, or an admin can start this one from the pergent.sh dashboard.` Later pushes that month post nothing. The note says nothing about numbers, and not which quota ran out, because on a public repository anyone reads it;
- the change is listed under **Waiting for a start** on the repository's page, with the line `Monthly token quota used up.` Everyone who can see the repository sees the entry; only admins get the **Start review** button.

Trust is checked before the quota: an untrusted author's change waits for [trust](trusted-authors.md) first and doesn't touch the quota.

**A request by hand** (`@<bot> review`, `@<bot> review failed`, or a maintainer's **Re-run** or **Start review**) is refused when the repository's quota is used up. When only the instance's quota is used up, pergent.sh first asks the chain, with the rules a review uses: if a team entry is left, the review starts and runs on it. Otherwise it is refused. A refused request queues nothing, so it uses no re-run cap:

- a comment gets 👎 and the reason `the monthly token quota is used up`. The note above is the reply only the first time that month for that change (it shares the once-a-month flag with the held pushes); later refusals get 👎 alone;
- a button answers 409 `the monthly token quota is used up`, shown beside the button;
- an earlier review of the change, its status and the bot's approval stay as they are;
- if a review of that head is already queued or running, the request is answered as "already queued", not refused (a queued review that the quota is about to hold does not count, so the request is refused);
- a change waiting for trust that someone with write access tries to start stays waiting for trust;
- when the usage can't be read, the request is answered as failed and nothing is queued.

**An admin** pressing **Start review** or **Re-run** in the dashboard starts a review whatever the quotas say. Under a used-up instance quota it runs on a team entry when one is left, and on a global entry otherwise. Its tokens count like any other. The caps are as for any start or re-run. Starting a held change reviews the whole current head.

## The 1st of the month

Usage is counted from the 1st, so the quota has room again with nothing to reset. **Nothing starts by itself:** a held change stays held until its next push (which is checked again), a `review` command from someone allowed to give one, or an admin's start. This keeps day one from being eaten by a burst of old reviews. Raising or removing a quota mid-month works the same way. Lowering a quota below what is used holds the next review.

## What the dashboard shows

Admins only. The repository page shows `Tokens this month: <used> of <quota>` with a bar for a repository with a quota, and the instance's line under it when the instance has one. The instance's line, here and on **Integrations**, counts global keys only, as its quota does. **Integrations** shows the same on each instance with a quota. The bar is **amber from 80%** and **red when used up**. A repository or instance with no quota shows its usage without a bar. The [Usage](usage.md) pages show rolling days and have no quota column.

## Model and team limits

A quota caps a repository or an instance. A **limit** caps one model entry. Limits are optional: with none set, nothing changes.

| Limit | Set by | Counts | When it is used up |
| --- | --- | --- | --- |
| Model limit | An admin, on a global entry (the config file's entries too) | Every token on the entry, on every instance and repository, API reviews included | Reviews skip the entry |
| Team limit on a global entry | An admin, whether teams may use the entry or not | Tokens on the entry in the repositories the team owns | Those repositories skip the entry |
| Team limit on a team entry | The team's maintainers, or an admin | Tokens on the team's entry | Reviews skip the entry |

- Each limit is its own number, from 1 to 1,000,000,000,000,000. An entry is skipped when any limit on it is used up. A team limit may be larger than the model limit: then the model limit stops the entry first.
- **The owner team** is the one models use ([Models](models.md#the-owner-team)). Each usage row keeps the owner team it had when the review finished, so a repository that moves to another team leaves its earlier tokens with the old team. A repository with no owner team, and an API review, has no team limit.
- Usage is the same measure as the quotas: all tokens (input, cache read, cache write, output and reasoning) of reviews that finished in the current UTC month, dropped attempts included, on the entry that spent them. A multi review counts each Job on its own entry: each reviewer and the judge.
- **The check** runs where the chain picks an entry, after the quotas. A review skips an entry at its limit and runs on the next one. A chain of 1 entry ignores marks, not limits: at its limit it is held.
- **When only the quotas and the limits stop the entries a review has not tried yet**, it is held as for a used-up quota: the same pending status and note, which name no limit and no number. The API gives the change waiting for a start the reason `limit_used_up` (`quota_used_up` when only quotas stopped it). The repository page lists it under **Waiting for a start** with the line `No model in the chain has room this month.`, and only admins get its **Start review** button. When a mark or an unusable key is part of it, the review fails with `all models exhausted:`, such as `all models exhausted: main (out of quota), backup (team limit used up)`.
- **A request by hand** (`@<bot> review`, a maintainer's **Re-run** or **Start review**) asks the chain the same way, and is refused with `the monthly token quota is used up` when the quotas and the limits leave no entry with room.
- **An admin's Start review or Re-run** runs on the first entry with room. When none has room, it goes past the model limits and the team limits on global entries, and runs on the first global entry with no mark. It does not go past a team's own limit on its team entry, which is the team's money: when nothing else is left, the button answers 409 `every model of the chain is at the team's own limit: a maintainer of <team> can raise it on the team page`.
- **A review asked for with room left** goes past the admin limits in the same way if they run out before it is dispatched. If only a team's own limit is left by then, it fails with `all models exhausted:`.
- **API reviews** count toward the model limit and no team limit. An API review skips an entry at its model limit, and one with no entry left, or whose pinned model is at its limit, fails with `all models exhausted:`.
- **Reviewers 2 to 5** skip an entry at its limit. When no entry of a reviewer is left, that reviewer is skipped. If every extra reviewer is skipped, multi review is off for that review (`multi review off: the token quota is used up`), and the review runs with reviewer 1 alone. The judge does not check limits: it ends a review that has started.
- A limit lowered below its usage stops the entry at the next dispatch. A limit raised or removed has room at once, but a held change waits for its next push, a command or an admin's start.
- **In the upgrade month**, team limits count only the reviews that finish after the upgrade, because older usage rows have no owner team. Model limits count the whole month.
- **When the usage can't be read** at dispatch, the dispatch is tried again with the usual backoff, and then the review fails. No review starts unchecked. A limit can be overshot, as a quota can.
- Setting and removing a limit is audited as `model_limit_set` and `model_limit_cleared`, with the entry's name, the team (or `all teams`) and the number, such as `main for Payments: monthly_tokens=5000000`.
- The routes are in [HTTP routes](../reference/http-routes.md). There is no config-file key for limits.
- **The dashboard.**
  - **Models list.** The **Models** list on the admin **Settings** page shows each global entry, the config file's included. It shows `Tokens this month: <used> of <limit>` and a bar when the entry has a model limit. The **Monthly token limit** field sets it, and empty is no limit.
  - **Team limits.** Under each entry, **Team limits** opens the list of each team's limit on it, with that team's tokens. An admin adds, changes and removes them there.
  - **The team page.** It has a **Model limits** card on its **Limits** tab for the team's maintainers and admins. The card shows each of the team's own entries with its limit, and they set it. It also shows each global entry an admin limited for the team, and only an admin changes that one there. A global entry's model limit shows on the card only as `Model limit for all teams: room left` or `used up`, because its numbers count other teams. A viewer sees no card.
  - **Chains.** Chain A, the reviewer chains and the judge chain tag an entry at its limit with `model limit used up` or `team limit used up`. The global chain reads the model limits only. A team's chain and a repository's chain read the owner team's limits too.

## Good to know

- **The quota can be overshot.** A review's size is known only after it runs, so the check doesn't predict it. A review that starts with a little room left can take the month past the quota, and reviews started together before any of them reports can all start. A review that was asked for with room left runs even if the quota is used up before it is dispatched. A review already running when the quota runs out finishes and is published as usual.
- **A multi review counts each Job on its own entry.** The instance quota counts the tokens of each reviewer and of the judge by the source of the entry that Job ran on (`global` or `team`), not by the entry the run started on. The usage page's split by source and by model, and the `model` label of the token metric, count them the same way. The repository quota counts every token on any key.
- **A known gap.** The quota counts the tokens recorded in [Usage](usage.md). A Job with no result (timeout, out of memory, deleted) and a superseded review count as zero.
- **When the usage can't be read** (a database error) at dispatch, the dispatch is tried again with the usual backoff, and then the review fails as any dispatch does. No review starts unchecked, and none passes.
- **Rolling back removes the quota and the limits.** The previous release has no quotas or limits and ignores them. During a rolling upgrade its replicas start reviews without the check. A held change is not picked up by the previous release by itself, but its **Start review** and a writer's `review` command can start one. See [Upgrading](../upgrading.md).
