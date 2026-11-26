# All tokens in usage and limits: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** "Tokens" counts input + cache read + cache write + output + reasoning everywhere: every total, ranking, median, quota and limit, every page, the CSV and the Grafana example.

**Architecture:** One SQL expression in `internal/store` replaces every `tokens_in + tokens_out`. `store.TokenSum` gets `Prompt()`, `Output()` and `Total()`. The dashboard sends `prompt`, `output` and `total` on one token view, and the web app shows them; it adds nothing up on its own.

**Tech Stack:** Go + pgx + Postgres, `openapi.yaml` (checked by `internal/dashboard/openapi_test.go`), React + TanStack Query + Vitest, a Grafana JSON file.

**Spec:** `docs/superpowers/specs/2026-11-26-all-tokens-usage-design.md`

## Global Constraints

- Tokens = `in + cache_read + cache_write + out + reasoning`, each NULL counted as 0, all 5 counted equally. No weights.
- Prompt = in + cacheRead + cacheWrite. Output = out + reasoning. Total = Prompt + Output.
- No database migration. No change to pergent (the CLI). No release, no version bump, no tag.
- Existing JSON fields stay (`in`, `out`, `cacheRead`, `cacheWrite`, `reasoning`, `unknown`; the Review API's `tokens_in`, `tokens_out`); new fields are added beside them.
- Cached = cacheRead ÷ prompt, rounded to a whole percent. "—" when prompt is 0, or when `unknown > 0` and cacheRead + cacheWrite + reasoning is 0.
- Copy, verbatim:
  - Usage intro: "Totals count all tokens: input, cache read, cache write, output and reasoning."
  - Limit and quota hint start: "Tokens per calendar month (UTC): input, cache read, cache write, output and reasoning."
  - Card subtitles: Prompt "N% from cache", Output "incl. reasoning".
  - Under the cards: "Prompt: input · cache read · cache write" and "Output: output · reasoning".
- Tests: locally only the tests you changed, by exact name: `go test ./internal/<pkg>/ -run '^(TestA|TestB)$'` and `npx vitest run <file>`, plus `go vet ./internal/<pkg>/`, `npm run typecheck`, `npm run lint`. Never `go test ./...`, a whole DB package, `make test` or a bare `npx vitest run`. The full run is CI only.
- DB tests use `PERGENT_SH_TEST_DATABASE_URL` against the already-running Postgres on port 55432 (`nc -z localhost 55432`). Never start a container (no docker, no orbctl). If it is down, say so and leave DB tests to CI.
- Public repo: no employer, customer, colleague or internal cluster names, hostnames or IPs in code, tests, docs or commits.

## Review Focus

1. **A multi review**: its `run_usage_by_model` rows feed model limits, team limits, the per-model costs and the global/team split. They must count all 5 parts, or a multi review stays under-counted. (Task 1 and Task 2 tests.)
2. **Old pergent rows with NULL cache and reasoning**: a total must equal in + out, never NULL, never an error. (Task 1 test.)
3. **A re-run of failed skills**: the run card reads `RunUsage`, not the result sum. The run card must show the new total from the table, and fall back to the result's 5 parts until usage is recorded. (Task 3 test.)
4. **Cached on mixed periods** (some runs unknown, some cached): show the real percent, not "—". (Task 4 test.)
5. **CSV readers** that read columns by position: `tokens` is added at the end of the header, so older columns keep their place. (Task 3 test.)

---

### Task 1: Limits and quotas count all tokens

**Files:**
- Modify: `internal/store/usage.go` (add the shared expression and `TokenSum` methods near `TokenSum`, line ~366)
- Modify: `internal/store/limits.go:91,96,101` (`entryTokensSQL`)
- Modify: `internal/store/quota.go:81,91` (`InstanceGlobalTokensSince`, `TokensSince`)
- Modify: `internal/store/apireviews.go:307,334` (`APIClientStats`, `APITokensSince`)
- Test: `internal/store/limits_test.go`, `internal/store/quota_test.go`, `internal/store/apireviews_test.go`, `internal/store/usage_test.go`

**Interfaces:**
- Produces:
  - `const allTokensSQL = "COALESCE(tokens_in, 0) + COALESCE(cache_read, 0) + COALESCE(cache_write, 0) + COALESCE(tokens_out, 0) + COALESCE(reasoning, 0)"`, in `usage.go`.
  - `func allTokensOf(alias string) string`: the same expression with each column prefixed `alias.` (for `u.` and `b.`).
  - `func (t TokenSum) Prompt() int64`, `func (t TokenSum) Output() int64`, `func (t TokenSum) Total() int64`.

- [ ] **Step 1: Write the failing tests**
  - `TestTokenSumTotals` (no DB): `TokenSum{In: 42, CacheRead: 525000, CacheWrite: 75000, Out: 2500, Reasoning: 2000}` gives Prompt 600042, Output 4500, Total 604542.
  - `TestEntryTokensCountAllParts` in `limits_test.go`: a run on an entry with one `run_usage` row `in 42, cache_read 525000, cache_write 75000, out 2500, reasoning 2000` → `EntryTokensSince` returns 604542 for that entry. Add one multi review: a `run_usage_by_model` reviewer row with the same 5 parts → its member's entry gets 604542.
  - `TestEntryTokensNullParts`: a row with cache and reasoning NULL, in 100, out 20 → 120.
  - `TestLimitReachedByCacheRead`: a team limit of 100000 and one review of in 42, cache_read 525000, out 2500 → the limit check reports it used up. Use the function the limit check already uses (the caller of `EntryTokensSince`).
  - In `quota_test.go`, `TestTokensSinceCountsAllParts` and `TestInstanceGlobalTokensSinceCountsAllParts`: the same 5-part row → 604542. A team-key row on the same instance stays out of the global count.
  - In `apireviews_test.go`, `TestAPITokensSinceCountsAllParts` and `APIClientStats` Tokens: the same row → 604542.

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./internal/store/ -run '^(TestTokenSumTotals|TestEntryTokensCountAllParts|TestEntryTokensNullParts|TestLimitReachedByCacheRead|TestTokensSinceCountsAllParts|TestInstanceGlobalTokensSinceCountsAllParts|TestAPITokensSinceCountsAllParts)$'`
Expected: FAIL (`Total` undefined; sums are in + out).

- [ ] **Step 3: Implement**
  - Add `allTokensSQL`, `allTokensOf` and the 3 methods.
  - Replace each `tokens_in + tokens_out` and each `COALESCE(sum(tokens_in), 0)::bigint + COALESCE(sum(tokens_out), 0)::bigint` in the 4 files above with `sum(<expression>)`, wrapped in `COALESCE(..., 0)::bigint` where the old one was.
  - Update the comments that say "in plus out" in these functions to "all tokens".

- [ ] **Step 4: Run the tests and see them pass**, plus any existing test in these files whose expected totals now change (run each by exact name; fix the expected value, never the expression).

- [ ] **Step 5: Commit** `feat(store): limits and quotas count all tokens`

---

### Task 2: Usage queries rank and total by all tokens

**Files:**
- Modify: `internal/store/usage.go`: `sourceTokensColumns` and `sourceTokensColumnsNamed` (485–491), the `ORDER BY` at 557, 621, 632, 702, 715, 749, the client sort at 587, `UsageCostPerReview` (843), `RunUsage` (306), `ReviewCost` + `ReviewCosts` (320–360), `SkillTokens` + `UsageOnDecidedChanges` (951–985).
- Test: `internal/store/usage_test.go`, `usage_team_test.go`, `usage_multi_review_test.go`

**Interfaces:**
- Consumes: `allTokensSQL`, `allTokensOf`, `TokenSum.Total()` from Task 1.
- Produces (signature changes the dashboard uses in Task 3):
  - `func (s *Store) RunUsage(ctx, id string) (t TokenSum, found bool, err error)`
  - `ReviewCost` replaces `In, Out int64` with `Tokens TokenSum`.
  - `SkillTokens` replaces `In, Out int64` with `Tokens TokenSum`.

- [ ] **Step 1: Write the failing tests**
  - `TestUsageRankingCountsAllTokens`: repository A has in 1000, out 1000 (total 2000); repository B has in 10, cache_read 50000, out 100 (total 50110). `UsageByRepository` lists B first. Repeat the same pair for `UsageBySkill`, `UsageByModel`, `UsageByMR`, `UsageByAPIClient` and team usage by repository and by model.
  - `TestUsageBySourceCountsAllTokens`: one global row and one team row, each with all 5 parts → `UsageBySource` returns each one's 5-part total.
  - `TestCostPerReviewCountsAllTokens`: 3 full reviews with totals 600042, 600042, 2000000 (put the big part in cache_read) → median 600042, and the 2000000 review is not an outlier (under 3×) while one with 2000000 × 2 would be.
  - `TestRunUsageAllParts`: a run with 2 skills, one with all 5 parts and one with NULL cache and reasoning → `RunUsage` returns the summed `TokenSum` with `Unknown` 1 and `found` true; an unknown run → `found` false.
  - `TestReviewCostsAllParts` in `usage_multi_review_test.go`: reviewer and judge rows with all 5 parts → each `ReviewCost.Tokens` carries all 5.
  - `TestUsageOnDecidedChangesAllParts`: a decided change with all 5 parts → `SkillTokens.Tokens.Total()` is 604542.

- [ ] **Step 2: Run them and see them fail**, by exact name as in Task 1.

- [ ] **Step 3: Implement**
  - `sourceTokensColumns*`: `sum(<allTokensSQL>) FILTER (WHERE source = …)`.
  - Every `ORDER BY COALESCE(sum(tokens_in), 0) + COALESCE(sum(tokens_out), 0)` becomes `ORDER BY COALESCE(sum(<expression>), 0)` with the right alias.
  - Line 587 client sort uses `c.Tokens.Total()`.
  - `UsageCostPerReview`: `sum(<allTokensSQL>)::bigint AS total`.
  - `RunUsage`, `ReviewCosts`, `UsageOnDecidedChanges` select `tokenSumColumns` (or the `u.`-prefixed variant) and scan with the existing `TokenSum` scan helper (`dests`).
  - Fix the callers so the package builds: `internal/dashboard/api_read.go:693` and `:938`, `internal/dashboard/api_skill_results.go:88`. Keep their behaviour for now: put `t.In`, `t.Out` where `in`, `out` were. Task 3 changes what they send.

- [ ] **Step 4: Run the new tests and the changed existing ones; `go vet ./internal/store/ ./internal/dashboard/`.** Expected: PASS.

- [ ] **Step 5: Commit** `feat(store): usage totals, rankings and medians count all tokens`

---

### Task 3: API views send prompt, output and total

**Files:**
- Modify: `internal/dashboard/view.go`: `UsageTokensView` (1778), `TokensView` (356), `ReviewerView` (~285), `JudgeView` (~318), `MultiReviewView.Total` (269), `SkillTokensView` (820), comments at 1775, 1796, 1924.
- Modify: `internal/dashboard/api_usage.go`: `usageTokens` (30), CSV header (250) and row writer.
- Modify: `internal/dashboard/api_read.go`: `skillExtra.Tokens` (713), `fillSkills` sum (766), `RunUsage` use (693), `ReviewCosts` use (938–957).
- Modify: `internal/dashboard/api_skill_results.go:88`.
- Modify: `internal/reviewapi/body.go:120–153` (`Usage`, `Map`).
- Modify: `openapi.yaml`: `TokensView`, `UsageTokensView`, `ReviewerView`, `JudgeView`, the Review API usage schema.
- Test: `internal/dashboard/api_usage_test.go`, `api_read_test.go`, `api_skill_results_test.go`, `openapi_test.go`, `internal/reviewapi/body_test.go`

**Interfaces:**
- Consumes: `RunUsage`, `ReviewCost.Tokens`, `SkillTokens.Tokens`, `TokenSum.Prompt/Output/Total` from Tasks 1–2.
- Produces (JSON the web app reads in Tasks 4–5):
  - `UsageTokensView` adds `prompt`, `output`, `total` (int64, required).
  - `TokensView` is removed. Every place that used it uses `UsageTokensView`: the run's `tokens`, `SkillTokensView.tokens`, `MultiReviewView.total`. JSON stays a superset of the old `{in, out}`.
  - `ReviewerView` and `JudgeView` embed `UsageTokensView` (Go struct embedding, so `in`/`out` stay at the same JSON place and the new fields sit beside them).
  - `func usageTokens(t store.TokenSum) UsageTokensView` fills all 9 fields; it is the only place the 3 computed fields are set.
  - Review API `Usage` adds `cache_read`, `cache_write`, `reasoning` and `tokens` (the total), int64; `tokens_in`, `tokens_out` stay.
  - CSV adds a `tokens` column **at the end** of the header.

- [ ] **Step 1: Write the failing tests**
  - `TestUsageTokensViewComputed`: `usageTokens(TokenSum{In: 42, CacheRead: 525000, CacheWrite: 75000, Out: 2500, Reasoning: 2000})` → Prompt 600042, Output 4500, Total 604542.
  - In `api_usage_test.go`: the admin usage JSON has `tokens.total` 604542 for the 5-part fixture; the CSV header's last column is `tokens` and the row's last value is `604542`.
  - In `api_read_test.go`: a run's `tokens` carries all 5 parts and `total` from `RunUsage`; before usage is recorded it is the sum of the result's skills (`input`, `output`, `reasoning`, `cache_read`, `cache_write` from the result JSON). A multi review's `total` and each reviewer's `total` count all 5.
  - In `api_skill_results_test.go`: a skill row's `tokens.total` is 604542.
  - In `body_test.go`: `Map` with 2 skills returns `tokens` = the sum of all parts, plus the 3 new fields; `tokens_in`/`tokens_out` unchanged.
  - `openapi_test.go` passes only when `prompt`, `output`, `total` are in `UsageTokensView` and the new Review API fields are described.

- [ ] **Step 2: Run them and see them fail**, by exact name.

- [ ] **Step 3: Implement** as the Interfaces block says. `UsageTokensView` description in `openapi.yaml`: "Tokens used. `total` is what every total, ranking, quota and limit counts: prompt (`in` + `cacheRead` + `cacheWrite`) plus output (`out` + `reasoning`)."

- [ ] **Step 4: Run the tests; `go vet ./internal/dashboard/ ./internal/reviewapi/`.** Expected: PASS.

- [ ] **Step 5: Commit** `feat(api): send prompt, output and total tokens`

---

### Task 4: Usage pages (admin, team, repository)

**Files:**
- Modify: `web/src/api.ts:618` (`UsageTokensView` type; remove `TokensView`, point its users at `UsageTokensView`; `ReviewerView`/`JudgeView` gain the fields)
- Modify: `web/src/pages/usageParts.tsx`
- Modify: `web/src/pages/Usage.tsx`, `TeamUsage.tsx`, `UsageRepository.tsx`
- Test: `web/src/pages/Usage.test.tsx`, `TeamUsage.test.tsx`, a new `web/src/pages/usageParts.test.tsx`

**Interfaces:**
- Consumes: the JSON from Task 3.
- Produces, in `usageParts.tsx`:
  - `total(t)` returns `t.total`; `prompt(t)` returns `t.prompt`; `output(t)` returns `t.output`.
  - `cachedPercent(t: UsageTokensView): string`: `"87%"` or `dash`, by the Global Constraints rule.
  - `partsText(t: UsageTokensView): string`: `"input 42 · cache read 525k · cache write 75k · output 2.5k · reasoning 2k"`, plus `" · unknown for N skill runs"` when `unknown > 0` (reuse `skillRunsText`). Uses `count()`.
  - `apartText` is removed.

- [ ] **Step 1: Write the failing tests**
  - `usageParts.test.tsx`: `cachedPercent` gives `"87%"` for cacheRead 525000 of prompt 600042; `"—"` for prompt 0; `"—"` for `unknown 3` with no cache and no reasoning; `"50%"` for `unknown 3` with cacheRead 50 of prompt 100. `partsText` gives the exact string above for the 5-part fixture.
  - `Usage.test.tsx` and `TeamUsage.test.tsx`: the 4 cards read Tokens `604.5k`, Prompt `600k` with "87% from cache", Output `4.5k` with "incl. reasoning", Reviews. The lines "Prompt: input · cache read · cache write" and "Output: output · reasoning" are shown. The intro has the exact Usage intro text. No "Cache and reasoning" column head and no "Reported apart" text. A table's heads are Reviews, Prompt, Output, Tokens, Cached in that order; the Tokens cell has an element, reachable by Tab, whose accessible name contains `partsText`.
  - The daily bar's label or tooltip names Prompt and Output.

- [ ] **Step 2: Run them and see them fail**

Run: `npx vitest run src/pages/usageParts.test.tsx src/pages/Usage.test.tsx src/pages/TeamUsage.test.tsx` (from `web/`)
Expected: FAIL.

- [ ] **Step 3: Implement**
  - `UsageHead`: Prompt · Output · Tokens · Cached (drop Input, Output-as-out, Cache and reasoning).
  - `UsageCells`: `count(prompt)`, `count(output)`, the bold total in a focusable element (`tabIndex={0}`, `title` and `aria-label` = `partsText`), and `cachedPercent`.
  - `UsageNotes`: "Days are cut at midnight UTC. " + the Usage intro text.
  - Cards in `Usage.tsx` and `TeamUsage.tsx` as the test says. `DailyTable` keeps using `total()` for the bar; its row title says "Prompt N · Output N".
  - `UsageRepository.tsx` uses the same cells; its "Tokens" median head stays.

- [ ] **Step 4: Run the tests; `npm run typecheck`; `npm run lint`.** Expected: PASS.

- [ ] **Step 5: Commit** `feat(web): usage pages show prompt, output, total and cached`

---

### Task 5: Run card, API review, skill results, API clients, limit hints

**Files:**
- Modify: `web/src/pages/ReviewDetail.tsx:323` (run card) and `:514`, `:525` (multi review table)
- Modify: `web/src/pages/ApiReview.tsx:117`
- Modify: `web/src/pages/SkillResults.tsx:136–139`
- Modify: hint text in `web/src/pages/TeamLimits.tsx:39`, `Integrations.tsx:304`, `RepositorySettings.tsx:798`, `AdminSettings.tsx:855` (and `:780` if it says "input plus output")
- Test: `ReviewDetail.test.tsx`, `ApiReview.test.tsx`, `TeamLimits.test.tsx`, `Integrations.test.tsx`, `RepositorySettings.test.tsx`, `AdminSettings.test.tsx`

**Interfaces:**
- Consumes: `partsText`, `total`, `prompt`, `output` from Task 4; Review API `usage.tokens`, `cache_read`, `cache_write`, `reasoning` from Task 3.

- [ ] **Step 1: Write the failing tests**
  - `ReviewDetail.test.tsx`: the run card shows 3 lines, exactly `Tokens: 604.5k`, `Prompt 600k · input 42 · cache read 525k · cache write 75k`, `Output 4.5k · output 2.5k · reasoning 2k`. With `unknown > 0` and no cache, the Prompt line ends `· unknown for 1 skill run`. The multi review table's token column per reviewer and judge shows `total`, with `partsText` as its accessible name.
  - `ApiReview.test.tsx`: the Tokens fact shows the same 3 lines from the Review API fields.
  - Each limit page test: the hint starts with the exact limit hint text, and no hint says "input plus output".
  - SkillResults: its Tokens and per-change columns use `total`; Input and Output columns become Prompt and Output.
  - API clients: "Tokens this month" is already the new total after Task 1 (`APIClientStats`). It is one number with no parts, so the page does not change and gets no hover. (Ruling: the spec asks for Prompt and Output on hover here; sending the parts for one column is not worth a new field.)

- [ ] **Step 2: Run them and see them fail**, by file with `npx vitest run`.

- [ ] **Step 3: Implement.** Keep each hint's tail as it is today ("Empty: no quota.", "Empty: limited only by the instance's quota.", and so on); only the "input plus output" part changes.

- [ ] **Step 4: Run the tests; `npm run typecheck`; `npm run lint`.** Expected: PASS.

- [ ] **Step 5: Commit** `feat(web): run card, API review and limit hints count all tokens`

---

### Task 6: Grafana example and docs

**Files:**
- Modify: `docs/grafana/pergent-sh.json` (expr at lines 414, 1037, 1105, 1240, 1316, 1392, 1902; title at 1027)
- Modify: `docs/concepts/usage.md`, `docs/concepts/token-quota.md`, `docs/concepts/metrics.md:42`, `docs/upgrading.md`
- Test: `internal/metrics/dashboard_test.go`

- [ ] **Step 1: Write the failing test** `TestDashboardTokenPanelsCountEveryKind` in `dashboard_test.go`: the JSON parses; no `expr` on `pergent_tokens_total` or `pergent_api_tokens_total` contains `kind=~"input|output"`; the panel titled "Prompt and output tokens" exists and "Input and output tokens" does not.

- [ ] **Step 2: Run it and see it fail**: `go test ./internal/metrics/ -run '^TestDashboardTokenPanelsCountEveryKind$'`.

- [ ] **Step 3: Implement**
  - Drop the `kind=~"input|output"` matcher from those 7 queries. The "Prompt and output tokens" panel keeps `sum by (kind)`, so the 5 kinds stack. "Cache and reasoning tokens" stays as it is.
  - Docs: every "input plus output" and "Cache and reasoning tokens are not added" becomes "all tokens: input, cache read, cache write, output and reasoning". `metrics.md:42`: a total is all 5 kinds.
  - `docs/upgrading.md`: a new top section `## Upgrading to the next release` (renamed to the version at release time) with the Behaviour change text from the spec's "Release notes" section, then the step: open Usage, take the last 30 days' Tokens (it now counts all 5 parts), and raise each set limit to cover it. Worked example: a team that used 285.4k under the old count and about 36.8M under the new one needs its 1M limit raised, for example to 50M. Empty limits need nothing.

- [ ] **Step 4: Run the test; check the JSON with `python3 -m json.tool docs/grafana/pergent-sh.json >/dev/null`.** Expected: PASS.

- [ ] **Step 5: Commit** `docs: tokens count all 5 parts; raise limits before upgrading`

---

## After the last task

- Run the public-repo name scan on the branch's commits before any push.
- Push the branch and open a PR only when the owner says so. CI runs the full test suite. No release.
