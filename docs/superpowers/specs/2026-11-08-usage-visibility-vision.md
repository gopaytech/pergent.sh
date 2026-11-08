# Model usage: seeing it, choosing the model, limiting it

Status: a vision, agreed in outline with the owner on 2026-11-08, with the owner's answers to the builder check's questions. It covers three projects and the order they come in. The first, usage you can see, is described closely enough to plan from. The other two set direction and list the questions their own designs must answer.

## Why

Every review spends tokens on one shared LLM account, and nothing in pergent.sh says where they went. The account can run dry and reviews start failing with `402 Insufficient Balance`, with no way to tell which repository, skill or merge request used the balance. Nor can anyone tell which skills are worth what they cost.

The owner wants to see where tokens go, then to choose a model per repository, then to give each repository an allowance.

## The three projects, in order

1. **Usage you can see.** Totals by repository, skill, merge request, model and time, what was wasted, and what each skill gives back. First, because the other two need it: an allowance can't be set sensibly without knowing what a repository uses, and models can't be compared until each review records its model.
2. **A model or provider per repository.** A repository can use another model, or another provider with its own key, instead of the default.
3. **A token allowance per repository.** A repository can use at most a set number of tokens. Last, because it is the only one that changes what happens to a review.

Each gets its own design and plan. This document does not authorize building 2 or 3.

## Anti-goals, for all three

- **A false pass.** No setting here may let a change be approved, or a check go green, without a review. A review that doesn't run because of an allowance is a failure that says so, never a skip that looks like a pass.
- **Usage in public.** Nothing here is reachable from `public_url`, where the webhooks are. Metrics and exports are for admins and the operator's own monitoring.
- **A secret or a person's text in a metric or an export.** Metrics and CSV carry names and numbers: repository paths, skill names, model names, merge request numbers, counts. No API key, token, finding text, description or comment.
- **Accounting that costs a review.** Recording usage never fails, delays or retries a review. A count that can't be recorded is logged and lost.
- **Breaking an older pergent image.** Whatever pergent.sh needs from a Job, an older image that doesn't send it still reviews; its usage shows as unknown, not as zero.

## Project 1: usage you can see

### Who sees it

Admins only, for now (the owner's decision). Team members see nothing new:

- The Skill results tab, which anyone who can see the repository reads, shows its token column to admins only.
- The review page keeps showing a review's own tokens to anyone who can see that review, as it does today; only its double count is fixed.

### What it shows

All of it in tokens. Totals, rankings and medians use input plus output, as pergent reports them today (the owner's decision). Cache and reasoning tokens are shown alongside once pergent reports them, never added in. Project 3's allowance uses the same measure. Money is not part of this project (see Non-goals).

- **By repository.** Tokens used per repository over a chosen period, with the busiest first.
- **Within a repository:**
  - **By skill:** which skills use the most.
  - **By merge request:** which merge requests used the most, each linking to its reviews.
  - **By model:** which model each review ran on, and the totals per model.
- **Over time.** Daily totals, overall and per repository, so a jump shows when it started.
- **Wasted tokens.** The tokens of skills that failed, counted per skill: a review where one skill failed wastes that skill's tokens, not the passing skills'. Reviews that left no result (a timeout, out of memory, a deleted Job) are counted, with their tokens shown as unknown. So are superseded reviews, stopped because someone pushed: the first version counts how many there were and shows their tokens as unknown, and recording those tokens is later work (the owner's decision). Re-runs are shown as their own figure, not as waste: a successful re-run did useful work. Past re-runs can't be split into those people asked for and those pergent.sh queued itself, because pergent.sh's own full re-run after a failed incremental merge also has trigger `rerun` (`internal/engine/publish.go:308`) and the `commands` table is pruned. (My calls.)
- **Per-skill value.** On the existing Skill results tab, not a new page (the owner's decision): next to each skill's raised, fixed and dismissed findings, the tokens it used on those same changes. It counts per change, once the change is merged or closed, so a finding raised again on every push counts once. A skill that is expensive and mostly dismissed is the one to fix or drop. Open merge requests are not in it; their tokens are in the other views.
- **Cost per review.** Per repository, the median tokens of a full review and of an incremental review, kept apart because an incremental review covers only what changed. A review is flagged when it uses more than three times the median of its kind (the owner's decision).
- **Daily totals cut at midnight UTC**, and the page says so (the owner's decision).
- **The review page reads the same numbers.** Its own sum counts a "review failed" run's kept skills twice today; it moves to the usage table in this project, so every page agrees (the owner's decision).
- **A repository switched off, or on a removed instance, keeps its usage**, marked as switched off: those tokens were spent. Its Skill results rows are still deleted as today, so its per-skill value goes (the owner's decision).
- **Usage is never pruned**, for now: one small row per review and skill, and runs are never pruned either (the owner's decision).

### Metrics

A Prometheus endpoint with counters by repository, skill, model and outcome: tokens in and out, reviews, failed reviews, re-runs, findings raised and dismissed. Labels are names and fixed values only, never anything a person wrote. Findings dismissed are counted when a dismissal is made, not per review.

It is not public. Serve it on its own port, `metrics_listen`, off by default, never on the port that `public_url` routes to. The chart gives that port its own `ClusterIP` Service, only when it is set, and can add a ServiceMonitor. It must not go on the main Service: the README offers `service.type: LoadBalancer` for that one, which would publish the port. A scraper inside the cluster reaches it; the internet never does. The counters are read from the usage table at each scrape, not kept in memory, so they survive a restart and include past reviews (the owner's decision, as is the rest of this paragraph).

Label sizes are bounded: a repository or skill label only for repositories switched on and skills in the library. The cardinality of a merge request is too high for a label, so per-merge-request numbers are in the dashboard and the CSV, not in metrics.

### CSV export

Admins download what a usage view shows, one row per review and skill: time, repository, merge request, run trigger, model, skill, verdict, tokens in, tokens out, findings raised, blocking. Dismissals are not on these rows: they belong to a change, not to one review, and are on the Skill results tab. Same rule as metrics: names and numbers only.

### What exists today

- **pergent** (v0.8.0, the image pergent.sh pins) reports, per skill, `tokens.input` and `tokens.output` summed over every run of that skill: batch, repair, verification and cross-batch, every retry included, and failed runs included (`internal/review/review.go`, `internal/runner/runner.go`). They are the sum of each finished step's `input` and `output`. opencode also reports reasoning, cache read and cache write tokens, which pergent parses and doesn't count (`internal/runner/parser.go:46-93`). A provider that reports no usage gives zeros. The only tokens lost are those of a step still running when a run is killed, and those of a Job killed before it delivers.
- **A `402` from the provider** isn't retried. The batch fails, and pergent still delivers a result with the skill `failed` and small or zero usage. It is a failed review, not a review with no result.
- **pergent.sh** keeps each run's whole result as JSON in `runs.result`, with no pruning found, and already has `runs.model`, `runs.trigger`, `runs.outcome` and the `dismissals` table. So past reviews can be counted from the day this ships, not only new ones, with care:
  - A `rerun_failed` run's stored result holds the skills it kept from its base run, with their tokens (`result.Merge`, `internal/result/result.go`). Only the skills in `runs.skill_subset` ran. Summing the stored results counts those tokens twice; the review page's own sum does today (`internal/dashboard/api_read.go:663`). Incremental merges keep only the fresh run's tokens.
  - `state = 'superseded'` also marks finished runs a re-run replaced (`internal/store/runs.go:328`). "No result" is `result IS NULL`, not a state.
  - A result for a superseded run is thrown away before it is read (`internal/store/runs.go:838`), and a superseded running Job is deleted. A result rejected as unusable is replaced by a failure, tokens and all.
- **The Skill results tab** already counts, per skill and per change, findings raised, fixed, dismissed, still open and gone away (`internal/dashboard/api_skill_results.go`, `finding_fates`). It counts a change once it is merged or closed, keeps a year, and drops a repository's rows when it is switched off.
- The review page already adds up one review's tokens (`internal/dashboard/api_read.go:621`). Nothing totals across reviews.
- There is no metrics endpoint, and one listener serves the webhooks, the Job callbacks and the dashboard.

### Gaps to close

- **Cache tokens.** With prompt caching, a provider bills cached input apart, and today it isn't counted. pergent should report it as its own number, and the views show it apart.
- **Reasoning tokens.** Thinking models bill them as output. Whether opencode's `output` already includes them depends on the provider; that needs checking per provider before the totals are trusted for thinking models.
- **A Job with no result** (timed out, out of memory, deleted) reports nothing. Those show as a failed review with unknown usage.
- Counting cache or reasoning tokens needs a pergent release and moving pergent.sh's pin (`go.mod` and `defaultImage` together). Until a Job runs that release, those numbers are unknown, not zero.
- **By run type** (batch, verification, repair, cross-batch) is not split today. Not needed for the first version; worth adding to pergent's result when it is cheap.

### Non-goals

- Money. No prices, no currency, no bill. Tokens only; a price per model can come later.
- Reading the provider's balance or bill. pergent.sh can't, for most providers.
- Alerts. Metrics make them possible in the operator's own Grafana or Alertmanager; pergent.sh sends none.
- Usage for team members, or in the review comment.

### Delivery

Three pull requests, in this order (the owner's decision):

1. **pergent:** report cache read, cache write and reasoning tokens per skill, added to the result next to `tokens` without changing it, and release it.
2. **pergent.sh:** the usage table, filling it from past results in the background, the admin views, the CSV export, tokens on the Skill results tab, and the review page reading the table. It doesn't wait for 1: cache and reasoning show as unknown until a Job runs the new pergent.
3. **pergent.sh:** the metrics endpoint, and the chart's port, Service and optional ServiceMonitor.

## Project 2: a model or provider per repository

Direction only; its open questions are settled in its own design, after Project 1 ships (the owner's decision). A repository's settings can name a model, and a provider with its own key, instead of the instance-wide default. Each review records the model it ran on (it already does in `runs.model`), so Project 1's views compare them.

Its design must answer:

- Where a provider's key lives (the dashboard's sealed store, like today's LLM key), and how a Job gets only the key it needs.
- Whether a repository can pick any model, or only one an admin has added.
- What the review comment and the Job log say about the model used.
- Whether verification can use a different model from the batch runs.

## Project 3: a token allowance per repository

Direction only; its open questions are settled in its own design, after Project 1 ships, when the usage numbers can size it (the owner's decision). An admin can give a repository an allowance of tokens per period.

Its design must answer, with the false-pass anti-goal above:

- The period: per month, rolling, or reset by hand.
- What happens at the limit: the review is not started, and the merge request gets a failed status and a comment that says why. Never an approval, never a green check.
- Whether a review in progress can finish when it crosses the limit (a review's size is only known after it runs).
- Whether re-runs, or blocking-only reviews, are exempt.
- A warning before the limit, and who sees it.

## Left to the builder of Project 1

- The exact pages and where they sit in the admin menu.
- The query shape: read `runs.result` on the fly, or keep a small table of per-review, per-skill totals written when a result arrives. Mine: the table, filled from existing results in the background after startup, not in the startup migration: pergent.sh listens only once migrations finish, and parsing every stored result there could outlast the startup probe and crash-loop the one replica.
- Period choices on the views (7 days, 30 days, 90 days is my guess).
