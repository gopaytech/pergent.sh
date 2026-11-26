# Tokens count all tokens: input, cache read, cache write, output and reasoning

Status: agreed with the owner on 2026-11-26 in a design session. The owner approves this document before the build starts.

## Why

pergent.sh counts "Tokens" as input plus output, in every total, ranking, median, quota and limit. Cache and reasoning are stored but shown apart, and never added.

That was close enough while nothing was cached. pergent v0.11.0 moved to opencode 2.3.2, which adds a cache policy: it puts a cache marker on the system prompt and on the last message of every request. A provider with an explicit cache (for example Alibaba Qwen) then reports most of each step's prompt as cache read or cache write. opencode reports `input` as only the tokens that were not cached, often about 6 to 10 per step.

The model still reads the same amount. Only the label moved. Two runs of the same merge request:

| pergent | opencode | Steps | input | cache read | cache write | out | Shown as Tokens |
|---|---|---|---|---|---|---|---|
| v0.10.0 | 1.19.4 | 12 | 600.5k | 0 | 0 | 5.8k | 606k |
| v0.12.0 | 2.3.2 | about 10 | 42 | about 525k | about 75k | 2.5k | 2.5k |

So since the move to pergent v0.11.0 or later:

- usage pages look about 100 times lower than the real use;
- **the quotas and limits almost never stop a review**, because a review that reads 600k tokens counts as about 2.5k.

Reasoning has the same problem in every version: opencode reports `output` without reasoning (1.19.4: `output - reasoning`; 2.3.2: `visibleOutputTokens`), so reasoning was never in the totals.

The token numbers pergent reports are correct. Only how pergent.sh adds them up is wrong.

## Goals

- "Tokens" means all tokens: input + cache read + cache write + output + reasoning, counted equally.
- One definition on the server, used by every total, ranking, sort, median, quota and limit.
- The usage pages show Prompt and Output groups, a Cached percentage, and the 5 parts on hover.
- The run card shows the total and both groups.
- Release notes tell admins to raise their limits before they upgrade.

## Non-goals

- Cost-weighted counting (cache read × 0.1, cache write × 1.25). Ratios differ by provider and model. It can come later as its own option.
- A cache TTL setting. opencode sends the provider's default. Alibaba's explicit cache has one lifetime (5 minutes, reset on each hit), which covers a whole review.
- Any change to pergent (the CLI) or to what it reports.
- A database migration. All 5 parts are already stored.

## Approaches considered

**What Tokens counts.**
- **All 5 parts, equal (chosen).** Simple, the same for every provider, and the limits keep their pre-cache meaning.
- Cost-weighted. Closer to the bill, but needs weights per provider and model, and "tokens" stops meaning tokens.

**Existing limits.** Under the new count, the same review counts about 100 times more, so limits sized for the old count fill up fast.
- **Ship it, with a clear release note (chosen).** The limit pages already show a bar of this month's use against the limit; it turns amber at 80% and red when used up, so a limit that is too low shows at once. A held review can still be started from the dashboard.
- Keep the old count for limits only. Nothing is held by surprise, but the limits stay broken.
- A switch, off by default. Two meanings of Tokens for a while, more code, for a one-time move.

**Layout.**
- **Prompt / Output / Tokens / Cached columns, the 5 parts on hover (chosen).** Narrow, and Cached is a quick cost signal.
- 5 separate columns plus Tokens. Too wide next to the global and team key columns.
- Tokens only, split on click. Loses the cost signal.

## Design

### One definition, set on the server

- **SQL:** one shared expression, NULL counted as 0:
  `tokens = in + cache_read + cache_write + out + reasoning`.
  Every query that sums, ranks, sorts or limits uses it: the usage pages and rankings, the cost-per-review median, the global and team key split, API reviews, and all 5 quotas and limits (instance quota, repository quota, API client quota, model limits, team limits). `run_usage_by_model` rows (multi review) use the same expression.
- **API:** `UsageTokensView` keeps `in`, `out`, `cacheRead`, `cacheWrite`, `reasoning`, `unknown`, and adds 3 computed fields:
  - `prompt` = in + cacheRead + cacheWrite
  - `output` = out + reasoning
  - `total` = prompt + output
  `openapi.yaml` describes them.
- **Web:** `usageParts.tsx` `total()` returns `t.total`. New `prompt()`, `output()` and `cachedPercent()` helpers. The web app adds nothing up on its own.
- **Unknown runs** (a pergent too old to report cache and reasoning): they count what is known. The "unknown for N skill runs" note stays. They are left out of Cached.

### Pages

| Page | Change |
|---|---|
| Admin Usage, team Usage tab | Cards: Tokens · Prompt ("N% from cache") · Output ("incl. reasoning") · Reviews. Under them: "Prompt: input · cache read · cache write" and "Output: output · reasoning". The "Reported apart, not in the totals" line goes. Intro: "Totals count all tokens: input, cache read, cache write, output and reasoning." |
| Their tables (repositories, models, skills, days) and repository usage (its merge request table) | Reviews · Prompt · Output · Tokens · Cached, then the key columns as today. The "Cache and reasoning" column goes. A row's title (hover) shows the 5 parts. Sort and rank by Tokens. |
| Daily chart | A bar is the day's Tokens; its tooltip shows Prompt and Output. |
| Review run card, API review page | `Tokens: 603k`, then `Prompt 600k · input 42 · cache read 525k · cache write 75k`, then `Output 4.5k · output 2.5k · reasoning 2k`. |
| API clients, skill results | Their Tokens is the new total; Prompt and Output on hover. |
| Limit and quota pages | The bars count the new total with no other change. Hint text: "Tokens per calendar month (UTC): input, cache read, cache write, output and reasoning." |
| CSV | Adds a `tokens` column; the raw columns stay. |

**Cached** = cache read ÷ prompt, rounded to a whole percent. "—" when the prompt is 0 or every run's cache is unknown.

### Grafana

`pergent_tokens_total` already has a `kind` label per part. In `docs/grafana/pergent-sh.json`, the Tokens, by model, by repository, by skill and per review panels count every kind. "Input and output tokens" becomes "Prompt and output tokens" with the 5 kinds stacked. The metric itself does not change.

### Docs

- `docs/concepts/usage.md` and `docs/concepts/token-quota.md`: "all tokens: input, cache read, cache write, output and reasoning" wherever they say "input plus output".
- `docs/upgrading.md`: a step to raise limits before the upgrade, with a worked example.

### Release notes (Behaviour change, first)

> **Tokens now count all tokens: input, cache read, cache write, output and reasoning.** Before, they counted input and output only. With pergent v0.11.0 and later, most prompt tokens are reported as cache read and cache write, so usage looked about 100 times lower than it was, and token limits did almost nothing. **This counts against every quota and limit.** Before you upgrade, open Usage, add Input + Output + cache read + cache write + reasoning for the last 30 days, and raise each limit to cover it.

## Testing

Each task runs its own tests. The full run is on CI only.

- **Store:** a test of the shared expression on a run with all 5 parts and on a run with cache and reasoning NULL. Every changed query's test gets the new expected totals. A limit test: a review with 42 input and 525k cache read reaches a 100k limit.
- **API:** `openapi_test.go` checks `prompt`, `output` and `total` on `UsageTokensView`.
- **Web (Vitest):** the cards, the table columns, Cached, the run card's 3 lines, and "—" for unknown or zero.
- **Grafana:** the JSON parses, and the token panels no longer filter on `kind`.

## Risks

| Risk | Handling |
|---|---|
| An admin upgrades without raising limits, and pushes are held | The release notes and `upgrading.md` say it first. The limit bars turn red. A held review can be started from the dashboard. No review is ever passed by mistake. |
| Older pergent images report no cache | They count input and output as before, with the "unknown" note. Nothing is counted twice. |

## Left to the builder

- Where the shared SQL expression lives (a Go constant used by the queries is the expected shape).
- The exact hover element for a row's 5 parts, as long as it is reachable by keyboard and has an accessible name.
