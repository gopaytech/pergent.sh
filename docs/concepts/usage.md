# Usage

See where the model tokens went on the admin **Usage** page (`/admin/usage`). It's for admins only: anyone else gets a 404.

## What the pages show

**All repositories** (`/admin/usage`), for the last 7, 30 or 90 days:

- Each repository, busiest first, with its tokens and reviews. **On global keys** and **On team keys** split its tokens by whose key paid: the global entries' or a team's own ([Models](models.md)).
- Each API client, busiest first, with its tokens and reviews and a line for each of its tenants (the first 100, then `and N more tenants`). A tenant is whatever the caller sent, shown as text; reviews with none are `(no tenant)`. The client's name opens its page. The total at the top is the repositories plus the clients, which is what the daily chart adds up to. The CSV is repositories only.
- A client's **Monthly token budget** is not this page's window: it is the tokens of the reviews asked for in the UTC month ([Limits](../setup/review-api.md#limits)). `GET /v1/client` shows what is used and left. Prometheus has the same numbers per client: [Metrics](metrics.md).
- The tokens of each day. Days are cut at midnight UTC.

**One repository** (`/admin/usage/<instance>/<project>`):

- The same split, as `Tokens on global keys: 800k · on team keys: 300k`.
- Tokens per skill and per model, with the model's provider.
- The 50 merge requests that used the most, with the models each used.
- The tokens of each day.
- What was wasted, and what a review costs.

## How tokens are counted

Totals count all tokens: input, cache read, cache write, output and reasoning. Every total, ranking and median adds all 5 parts, with no weights.

- **Prompt** is input, cache read and cache write. **Output** is output and reasoning. **Tokens** is Prompt plus Output.
- The cards are **Tokens**, **Prompt**, **Output** and **Reviews**. Prompt says how much of it came from cache, and Output says "incl. reasoning". Under the cards: "Prompt: input · cache read · cache write" and "Output: output · reasoning".
- The tables have the columns **Prompt**, **Output**, **Tokens** and **Cached**. **Cached** is cache read divided by Prompt, as a whole percent. It shows "—" when there is no prompt, or when the pergent that ran did not report cache.
- Hover the **Tokens** cell, or put keyboard focus on it, to see the 5 parts.
- A review's run card shows 3 lines: Tokens, Prompt and Output.
- A review by a pergent that doesn't report cache and reasoning counts them as 0 in the total, and the page notes that they are unknown.

## Team usage

A team's page has a **Usage** tab. Only the team's maintainers and admins see it. A viewer of the team gets a 403 from `GET /api/teams/{id}/usage?days=N`, and anyone else a 404. `days` is 1 to 90 and the default is 30.

- It shows the tokens (all 5 parts) and reviews of the repositories the team owns, each day, each repository and each model, busiest first.
- A review counts on the owner team the repository had **when the review was written**. If a repository moves to another team, its old rows stay on the old team.
- `Counts reviews since v0.19.0` shows when the period starts before the oldest review that has any owner team. Older reviews have no team, so they are not counted. It is checked over all teams, not only this one, so a new team does not get the note just because it is new.

## Wasted tokens

| Figure | What it counts |
| --- | --- |
| Failed skills | The tokens of skills that failed. The skills that passed in the same review don't count. |
| Re-runs | `review`, `review failed` and the dashboard's buttons. Not waste: a re-run that worked did useful work. |
| No result | Reviews that left no result (timeout, out of memory, deleted Job). Their tokens are unknown. |
| Superseded | Reviews stopped by a newer push. Their tokens are unknown. |

A skill that failed in a re-run is in both of the first two figures, so don't add them together. A `start` of an untrusted author's change isn't a re-run. A failed review that a later re-run replaced still counts as no result.

## Cost per review

Each repository has two medians: one for full reviews and one for incremental reviews, which cover only what changed. A re-run of only the failed skills is in neither.

A review that used more than three times the median of its kind is listed, costliest first, up to 20.

## Multi review

A [multi review](multi-review.md) has one Job for each reviewer and one judge Job. The cost of a review is the total of all of them:

- **Per model.** Tokens are counted per model, as for a review that fell back: each model of each reviewer and of the judge has its own row.
- **Reviewers** count on the run that split the review. The judge counts on its own run.
- A review's page shows the whole cost: [Dashboard](dashboard.md).
- Member fallbacks are not in `pergent_model_fallback_total`.

## CSV export

Both pages have a **Download CSV** link: `GET /api/admin/usage.csv?days=N`, with `instance` and `project` for one repository.

There's one row per skill per review, oldest first:

`reported_at`, `instance`, `repository`, `merge_request`, `trigger`, `incremental`, `model`, `skill`, `verdict`, `tokens_in`, `tokens_out`, `cache_read`, `cache_write`, `reasoning`, `findings`, `blocking`, `provider`, `source`, `tokens`

- Cache and reasoning cells are empty when the Job didn't report them.
- `tokens` is the total of all 5 parts of that row. It is the last column, and `provider` and `source` were added before it, so the older columns keep their places. `provider` is empty for a review from before providers were recorded. `source` is `global` or `team`: whose key paid. See [Models](models.md).
- There are no titles, no other text a person wrote, and no dismissals. Dismissals are on [Skill results](skill-results.md).
- A cell a spreadsheet would read as a formula starts with `'`.
- The file stops at **200,000 rows**, with no mark that it was cut.
- The server allows 60 seconds to write the response, so a very large export can be cut short. Narrow the period, or export one repository.

## Good to know

- **Switched-off repositories** keep their usage, marked "switched off". They have no page of their own, but their row links to a CSV.
- **The review page** still shows a review's own tokens to anyone who can see that review. A `review failed` re-run shows only the skills it ran.
- **Backup models.** A review attempt dropped for the next entry of its [model chain](models.md#attempts) keeps its tokens here, on the entry that used them. On these pages each attempt counts as a review of its own.
- **Global and team keys.** `GET /api/admin/usage` gives each repository's tokens (all 5 parts) on global keys as `globalTokens` and on teams' own keys as `teamTokens`, and a repository's usage route gives the same two. The instance's [token quota](token-quota.md) counts `globalTokens` only. The API clients have no split: their reviews always run on global keys.
- **Usage is never pruned.**
