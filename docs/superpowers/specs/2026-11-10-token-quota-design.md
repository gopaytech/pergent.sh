# Monthly token quota

Status: agreed with the owner on 2026-11-10. This is Project 3 of `2026-11-08-usage-visibility-vision.md` ("a token allowance per repository"), widened to instances. Where this document and the vision differ, this one wins.

## Why

Every review spends tokens on one LLM account, and nothing stops a repository, or a whole instance, from spending all of it. Trusted authors stops strangers on public repositories, but a busy repository, a loop of pushes or re-runs, or a team with write access can still use the account up, and then every repository's reviews fail. The owner wants a ceiling no one can get past without an admin.

## Goals

- An admin can give each **instance** (a GitLab server or GitHub App in Integrations) and each **repository** a monthly token quota.
- Once a quota is used up, no new review starts on it and no LLM call is made, until the next month or until an admin starts one.
- People on the merge request (pull request) can see why it wasn't reviewed. Admins can see how close each quota is before it runs out.

## Non-goals

- Money, prices or a bill. Tokens only, as in the vision.
- Quotas per author, team, skill or model.
- Alerts or metrics for the quota (the owner's choice: the dashboard only). Operators who want an alert can build one from the existing `pergent_tokens_total`.
- Stopping a review halfway through.
- Exemptions for re-runs or for kinds of review: every review counts.

## Anti-goals

- **A false pass.** A change not reviewed because of a quota never gets an approval or a green check. Its check stays **pending**.
- **A way past the quota without an admin.** No command, push, re-run or start by a non-admin starts a review on a used-up quota.
- **Usage in public.** The note on the merge request says the quota is used up and nothing else: no numbers, no limit, no other repository's name. On a public repository anyone reads it.
- **A quota check that starts a review it couldn't check.** If the usage can't be read, the review doesn't start yet (it is retried like any other dispatch error); it never starts on a guess.
- **Breaking setups without quotas.** With no quota set anywhere, pergent.sh behaves exactly as today.

## At a glance

| Setting | Where | Empty means |
|---|---|---|
| Instance quota | Admin → Integrations → the instance, or `gitlab[].monthly_token_quota` / `github[].monthly_token_quota` | Unlimited |
| Repository quota | The repository's Settings (`monthlyTokenQuota` in the API), or `repositories[].monthly_token_quota` | Limited only by the instance quota |

| What happens | Result |
|---|---|
| Room left in both | Reviewed as today |
| Either is used up, and a push (or pergent.sh's own full re-run) needs a review | Held: no Job, no LLM call. Pending status. One note per merge request per month. Listed as waiting in the dashboard. |
| Either is used up, and someone asks by hand (`@<bot> review`, a maintainer's **Re-run** or **Start review**) | Refused with a reason, nothing queued. An existing review, its status and its approval stay as they are. |
| An admin presses **Start review** or **Re-run** in the dashboard | Reviewed. Its tokens count. |
| The 1st of the month (UTC) | Usage starts again from zero. Waiting changes stay waiting until their next push, a `review` command, or an admin's start. |
| A review already running when the quota runs out | Finishes and is published as usual. The month can go over. |

## Behaviour

### The two quotas

- A quota is a whole number of tokens per calendar month. Empty is no quota. Zero and negative numbers are refused; to stop a repository's reviews, switch it off. (My call.)
- **A repository's quota sits inside its instance's.** A review starts only if the repository has room under its own quota (if it has one) **and** the instance has room under the instance's (if it has one). A repository with no quota is limited only by its instance. An instance with no quota is unlimited.
- The two are independent numbers. The repository's quota doesn't inherit or default to the instance's, and a repository's quota may be larger than its instance's (then the instance's is the one that bites).
- **Only admins** (account role `admin`) set or change a quota, in the dashboard or the config file. Maintainers and viewers see neither the fields nor the usage.
- A config-managed instance or repository takes its quota from the config file, and the dashboard shows it read-only, as for trusted authors.
- Changing a quota is recorded in the audit log.

### What counts

- **Input plus output tokens**, the same measure as the admin usage views, the Grafana dashboard and the vision (the owner's decision there). Cache and reasoning tokens are not added.
- Summed from `run_usage` for rows whose `reported_at` is in the current calendar month **in UTC**. A review that started on the 31st and finished on the 1st counts in the new month. (My call.)
- A repository's usage is its rows (by instance and `project_id`). An instance's usage is all its repositories' rows, switched-off and removed repositories included: those tokens were spent.
- What `run_usage` doesn't hold isn't counted: a Job with no result (timeout, out of memory, deleted), a superseded review's tokens, and a result whose usage hasn't been recorded yet. This is a known gap of the usage table, not of the quota.
- A review that a quota held costs nothing, so it counts nothing.

### When the check runs

The check is "usage so far this month is below the quota", for the repository's quota and the instance's. It doesn't predict a review's size: a review's tokens are only known after it runs. So a review that starts with a little room left can take the month past the quota, and reviews started together before any of them reports can all start. The owner accepted this overshoot.

There are two places, and each run is checked in exactly one of them:

- **Automatic runs** (a push, and pergent.sh's own full re-run after a failed incremental merge, `internal/engine/publish.go`) are checked in the dispatcher, **just before a Job is created**, after the trusted-authors check. A change held for trust doesn't reach the quota check. A used-up quota holds the run (next section). Neither of these runs replaces a published review of the same head, so holding one never puts a pending status over an earlier green check or approval.
- **Requests by hand** (a `@<bot> review` command, and a maintainer's **Re-run** or **Start review** in the dashboard) are checked **when they are made**, in the command step or the dashboard route, never in the webhook request. A used-up quota refuses them (below) and nothing is queued. One that passes is queued as today and isn't checked again at dispatch: it was allowed when asked, and checking again could hold a re-run that has already replaced a published review. (My call.)
- **An admin's action** is checked nowhere (below).

### Asking by hand on a used-up quota

The owner's choice: refuse, don't hold.

- **A `@<bot> review` command** gets 👎 and the reason `the monthly token quota is used up` in the commands table, as other refused commands do. No numbers, and it doesn't say which quota. If the quota note (below) hasn't been posted on that merge request this month, it is posted as the reply, sharing the same once-a-month flag; otherwise the 👎 is all (the owner's decision). It counts against the existing "one do-nothing answer per 2 minutes" limit, so it can't make the bot comment in a loop.
- **A maintainer's Re-run or Start review** is refused by the route with the same reason, shown beside the button.
- Nothing is queued, so a refused request uses no re-run cap.
- An existing review of the head, its commit status and the approve bot's approval stay as they are: they come from a real review.
- A change waiting for trust that a writer or maintainer tries to start stays waiting for trust, with the trust reason.
- If the usage can't be read when the request is made, the request is answered as failed (as other command errors are) and nothing is queued.

### When a quota is used up: an automatic run

1. **No Job and no LLM call.** The run is held in the existing `waiting` state with a new reason, `quota_used_up`.
2. **A pending status** on the head, set again on each new head:
   `Monthly token quota used up. An admin can start the review in the dashboard.`
3. **One note** on the merge request, the first time in a calendar month that a quota holds it. Later pushes that month post nothing. A plain comment, no LLM:
   > Not reviewed: the monthly token quota is used up. Reviews start again with the first push after the 1st, or an admin can start this one from the pergent.sh dashboard.

   It doesn't say whether the repository's or the instance's quota ran out, or any number (see Anti-goals). "Posted this month" is stored per merge request, so a restart or a webhook replay doesn't post it twice.
4. **Listed as waiting** in the repository's **Waiting for a start** box, with the reason `Monthly token quota used up`. Everyone who can see the repository sees the entry; only admins get the **Start review** button.

### An admin starting a review

- **Start review** on a waiting change, and **Re-run** in the dashboard, pressed by an admin, start a review **whatever the quotas say**. Its tokens count like any other. (The owner chose "an admin can start it anyway"; counting the dashboard Re-run the same is my call.)
- An admin's start of a quota-held run reviews the whole current head, as a start does today, even when the held run was a full re-run. (My call.)
- This also starts a change waiting for trust, as today.
- Caps are unchanged: an admin's start uses the per-change re-run cap and not the repository's daily cap, as starts do today; an admin's Re-run uses both, as Re-runs do today. (My call.)
- An admin's start is recorded in the audit log, as starts are today.

### The 1st of the month

- Usage is counted from the 1st, so the quota has room again with nothing to reset.
- **Waiting changes stay waiting** (the owner's choice, so day one isn't eaten by a burst of old reviews). Their next push, a `review` command from someone allowed to give one, or an admin's start reviews them as usual.
- Raising or removing a quota mid-month does the same: nothing starts by itself.
- Lowering a quota below what's used holds the next review.

### Warnings in the dashboard

Admins only (the owner's choice: no warning on merge requests, no metrics).

- **The repository page** shows `Tokens this month: <used> of <quota>` with a bar, for a repository with its own quota, and the instance's line under it when the instance has one.
- **Integrations** shows the same bar on each instance with a quota.
- The bar is **amber from 80%** and **red when used up**. A repository with no quota of its own and an instance with no quota show their usage without a bar.
- The existing admin usage views add each repository's quota beside its month's usage, where they show a month. (Builder's call how.)

## Edge cases

- **Trust and quota both block a change:** trust is checked first, so it waits for trust with the trust reason. A writer's or maintainer's start is refused for the quota and it keeps waiting for trust. An admin's start passes both.
- **Several pushes while held:** each new head is held, pending status on each, no second note that month. The waiting box shows the newest head only, as today.
- **An instance quota runs out:** every repository on it holds, including those with room left under their own quota.
- **A review finishing after the month changed** counts in the new month (by `reported_at`).
- **The usage sum can't be read** (database error) at dispatch: the dispatch is retried with the usual backoff (5 attempts, about 2.5 minutes), and then the review fails as any dispatch does today, with a failed status and the error. No review starts unchecked, and none passes.
- **A request by hand that passed the check** is queued and runs even if the quota runs out before it is dispatched. That is part of the overshoot.
- **The previous release on the same database** has no quotas. It claims only queued runs, so it doesn't pick up a quota-held run by itself, but its **Start review** (shown to maintainers) and a writer's `review` command can start one. That is the previous release working as it always did; rolling back means rolling back the quota. During a rolling upgrade, old replicas dispatch without the check for those minutes.

## Upgrade

- A new migration (the next free number on `main` when the work starts; `0035` today, re-checked against parallel work) adds the instance quota, the repository quota, and the per-merge-request "quota note posted" time. Both quotas are nullable: nothing is limited after the upgrade.
- Quota-held runs reuse the `waiting` state with the new `skip_reason` `quota_used_up` (my call: the previous release has no quotas, so a new state would protect nothing on rollback). `HoldRun` holds only `trigger = 'push'` runs today, as its guard against a race with `StartRun`; holding pergent.sh's own full re-run needs that guard widened without losing the race protection.
- How an admin's start is marked as past the quota (a column on the run, or a new trigger) is the builder's call, as long as the previous release ignores it safely.
- The new config keys are refused by the previous release, which rejects unknown keys. `docs/upgrading.md` says to remove them before rolling back, and that a rollback removes the quota.
- A new page, `docs/concepts/token-quota.md` (listed in `docs/README.md`), says where to set it, what counts, what people see at the limit, the overshoot (reviews started together near the limit can all run), and the known gap that a Job with no result counts as zero.

## Delivery

One pergent.sh pull request: the migration, the dispatcher and command checks, the dashboard, the docs and `openapi.yaml`. pergent needs no change. No release until the owner says so (the owner's decision).
- `openapi.yaml` gains the repository setting, the instance endpoint and the new waiting reason.

## Testing

What must be pinned by tests, at minimum:

- No quota anywhere: reviews run as today, and no usage query blocks them.
- Repository quota used up: a push holds the run (no Job), pending status set, one note; a second push the same month posts no note; the waiting box lists it with the quota reason.
- Instance quota used up: a repository with no quota of its own, and one with room under its own, both hold.
- Room left in both: reviewed. Usage exactly at the quota: held.
- Usage counts only the current UTC month, input plus output, by `reported_at`; a row from the last second of the previous month isn't counted.
- pergent.sh's full re-run after a failed incremental merge is held on a used-up quota.
- A writer's `review` command on a used-up quota gets 👎 and the reason, queues nothing, and leaves an earlier published review's status and approval as they were; a maintainer's dashboard Re-run and Start are refused with the reason; none uses a re-run cap.
- A `review` command accepted with room left is dispatched even if the quota ran out meanwhile.
- An admin's **Start review** and **Re-run** start a review on a used-up quota; the route still refuses a viewer.
- An untrusted author's change waits for trust first and doesn't touch the quota; a writer's start is refused for the quota and it stays waiting for trust; an admin's start runs.
- A new month: the first push reviews; a waiting change doesn't start by itself; the note is posted again if that month's quota also runs out.
- The usage query fails: at dispatch no Job and the dispatch is retried; at a command, nothing is queued.
- The note text has no numbers and doesn't name the quota that ran out.
- Only admins can set either quota; a config-managed instance's or repository's quota is read-only in the dashboard and read from the config file; zero and negative values are refused.
- The usage bar: amber at 80%, red at 100%, hidden from maintainers and viewers.
- A quota-held run is never dispatched by the previous release on the same database.
- GitLab and GitHub both.

## Left to the builder

- Page layout of the quota fields and bars, following the existing repository Settings and Integrations pages.
- How quota numbers are typed and shown (plain digits, or `5M`-style input and display).
- Whether the usage sums need a new index (an instance-wide monthly sum reads `run_usage` by instance and `reported_at`).
- Whether to cache the month's usage for a few seconds in the dispatcher, as long as a used-up quota holds within seconds of the result that used it up.
- Exact wording of the status and note, within GitHub's 140-character status limit and the rules above.
