# Usage

See where the model tokens went under **Admin → Usage** (`/admin/usage`). It's for admins only: anyone else gets a 404.

## What the pages show

**All repositories** (`/admin/usage`), for the last 7, 30 or 90 days:

- Each repository, busiest first, with its tokens and reviews.
- Each API client, busiest first, with its tokens and reviews and a line for each of its tenants. The total at the top is the repositories plus the clients, which is what the daily chart adds up to. The CSV is repositories only.
- The tokens of each day. Days are cut at midnight UTC.

**One repository** (`/admin/usage/<instance>/<project>`):

- Tokens per skill and per model, with the model's provider.
- The 50 merge requests that used the most, with the models each used.
- The tokens of each day.
- What was wasted, and what a review costs.

## How tokens are counted

Every total, ranking and median is **input plus output**.

Cache reads, cache writes and reasoning tokens are shown apart and never added in. A review by a pergent that doesn't report them shows "unknown", not zero.

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

## CSV export

Both pages have a **Download CSV** link: `GET /api/admin/usage.csv?days=N`, with `instance` and `project` for one repository.

There's one row per skill per review, oldest first:

`reported_at`, `instance`, `repository`, `merge_request`, `trigger`, `incremental`, `model`, `skill`, `verdict`, `tokens_in`, `tokens_out`, `cache_read`, `cache_write`, `reasoning`, `findings`, `blocking`, `provider`

- Cache and reasoning cells are empty when the Job didn't report them.
- `provider` is the last column, so the others keep their places. It is empty for a review from before providers were recorded. See [Models](models.md).
- There are no titles, no other text a person wrote, and no dismissals. Dismissals are on [Skill results](skill-results.md).
- A cell a spreadsheet would read as a formula starts with `'`.
- The file stops at **200,000 rows**, with no mark that it was cut.
- The server allows 60 seconds to write the response, so a very large export can be cut short. Narrow the period, or export one repository.

## Good to know

- **Switched-off repositories** keep their usage, marked "switched off". They have no page of their own, but their row links to a CSV.
- **The review page** still shows a review's own tokens to anyone who can see that review. A `review failed` re-run shows only the skills it ran.
- **Usage is never pruned.**
