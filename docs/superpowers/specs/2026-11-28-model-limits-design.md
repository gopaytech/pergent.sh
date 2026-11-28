# Context window and max output per model entry

## Why

opencode reads a model's context window and max output from its own catalog. A model id the catalog does not have gets context `0` and a max output of 32,000 tokens (`OUTPUT_TOKEN_FALLBACK`, opencode 2.3.2). With context `0`, opencode cannot size a reply to the room left in the window. pergent turns auto-compaction off (`compaction.auto: false`), so a declared window never makes opencode compact; it only sizes the replies.

The first real security scan, on a model whose id the catalog spells differently (dots instead of dashes), stopped after 40 steps: three replies in a row ended with `finish = length` at 829, 392 and 81 output tokens, and opencode failed with "Response still truncated after two output token limit continuations". The model has a 1M context window and 384K max output. Thinking and output shared the 32,000 budget.

pergent reviews read `PERGENT_CONTEXT_LIMIT`, but pergent.sh never sets it, `pergent scan` reads neither limit, and no output limit exists.

## Goals

- An admin (or a team maintainer, for a team entry) can give an entry its real context window and max output.
- Every Job that runs the entry uses them: reviews, multi-review reviewers and the judge, and security scans.
- Entries that leave the fields empty behave exactly as today.

## Non-goals

- pergent.sh guessing limits (cleaned-up ids, the provider's `/models` endpoint).
- One install-wide default for unknown models.
- Blocking the fields on older pergent images. They can be saved; only the Job env waits for a new enough image.

## Approaches

| | Approach | Why not, or why |
|---|---|---|
| **A (chosen)** | 2 optional fields on each entry | The limits belong to the model, and the entry defines the model. Exact. |
| B | pergent guesses from the catalog or `/models` | Providers rarely publish limits there; a wrong match gives wrong limits silently. |
| C | One config default for unknown models | Wrong for most models; a value too high brings the same failure back. |

## Design

### The fields

| Field | Meaning | Empty | Range |
|---|---|---|---|
| Context window (`context_window`) | Most tokens in one request, input and output together | opencode's catalog | 4,096 to 10,000,000 |
| Max output (`max_output`) | Most tokens in one reply, thinking included | opencode's catalog, or 32,000 | 1,024 to the context window when set, else to 10,000,000 |

Whole numbers only. A max output above the entry's context window is refused.

**Names.** pergent.sh already has *model limits* (monthly token limits, migration `0051`, the Limits UI), so these fields are never called limits there: **Context window** and **Max output** in the UI, `contextWindow` and `maxOutput` in the API, `context_window` and `max_output` in the DB and the config file. Only the pergent CLI keeps its own words: `PERGENT_CONTEXT_LIMIT` / `--context-limit` (exists) and `PERGENT_OUTPUT_LIMIT` / `--output-limit` (new). The deprecated `--max-tokens` stays unused.

**How high to set Max output.** opencode asks for `min(max output, 256,000)` on every reply. Some providers count the asked amount against a per-minute rate limit, so several sub-agents asking for 256,000 each can be throttled although their replies are small. The help text recommends 64,000.

### Where they live

- Global entries: 2 nullable integer columns on `llm_models`.
- Team entries: the same on `team_models`.
- Config file: `llm.context_window`, `llm.max_output`, and the same keys in each `llm.fallbacks` item. Checked with the same ranges at load.
- Migration `0056_model_limits_per_entry.sql` adds the columns. The previous release ignores them.

Who edits them: whoever may edit the entry's base URL and key. A change applies from the next Job, and goes into the existing entry-edit audit entry with the old and new values.

### API and dashboard

- The entry API takes and returns `contextWindow` and `maxOutput`; `null` is empty. A refused value is a 400 with the range in the message.
- The model form (admin Settings, a team's Models tab) has the 2 fields with the help lines: "Leave empty for models opencode knows. Set them for a model id it does not know: the context window from the provider's page." and, for Max output, "The most one reply may need, thinking included. 64,000 is enough for reviews and scans. A provider that counts it against a rate limit throttles a higher value."
- Entry lists show the limits as `1M / 384k`, or nothing when both are empty.

### Jobs

A review, judge or scan Job gets `PERGENT_CONTEXT_LIMIT` and `PERGENT_OUTPUT_LIMIT` from the entry it runs, each only when set. After a fallback, the next Job uses the next entry's limits.

### pergent

- `pergent scan` reads `PERGENT_CONTEXT_LIMIT` / `--context-limit`, as reviews do.
- `pergent` reviews and `pergent scan` read a new `PERGENT_OUTPUT_LIMIT` / `--output-limit`.
- The generated opencode config declares the model with `limit: {context, output}` when either is set; an unset one is `0`, which opencode reads as "use your own value". opencode caps the request at 256,000 output tokens itself.
- **Review batches stay at most 40,000 tokens.** `PERGENT_CONTEXT_LIMIT` also sets the review's diff batch size (`diff.BatchBudget`, `context / 4`, else 40,000). It becomes `min(context / 4, 40000)`, so a big window stops failures without making batches bigger. A smaller window still gives smaller batches. This changes the CLI for anyone passing a large `--context-limit` today, so the pergent release notes say so.
- **A clear reason for cut replies.** opencode fails a run whose replies end with `finish = length` 3 times with the error type `output-limit`. pergent maps that type to the fixed reason "the model's replies were cut at the max output: set Max output on the model entry", for reviews and scans.

### Older pergent images

pergent v0.13.0 reviews read the context limit and ignore the output limit; v0.13.0 scans ignore both. A v0.13.0 review also sets its diff batch size to a quarter of the context limit with no cap, so a 1M window would give batches of 250,000 tokens.

So pergent.sh sends `PERGENT_CONTEXT_LIMIT` and `PERGENT_OUTPUT_LIMIT` only when the Job's image is at or above `config.ModelWindowMinVersion`, the same version gate as `BuiltInRulesListMinVersion` and `PanelMinVersion`. Review, reviewer and judge Jobs check `jobs.image`; scan Jobs check `jobs.security_image`. A digest-only reference, `main` or a missing tag is not new enough.

- The version starts as `""`, so no Job gets the variables in this feature. The release that moves the default images to the next pergent sets it to that version. The code holds no version number before then.
- The fields can always be saved. When the image is too old, the model form says so in one line, as the Rules picker does for built-in rules.
- The upgrade notes and the Models page say the values take effect only with a pergent at or above that version.

## Tests

- pergent: the generated review and scan configs hold the limits (both, one, none); a bad value is refused; `pergent scan` passes the flag and the env; `BatchBudget` is capped at 40,000; a fake model that ends 3 replies with `finish_reason: length` gives the `output-limit` reason (opencode e2e).
- pergent.sh: migration; config load ranges; store round trip for global and team entries; API set, clear and refused values (including output above context); the audit entry holds old and new values; Job env with and without values, for a review, a judge and a scan, and after a fallback; the version gate (sent at and above the min version, not below, not for a digest, `main` or no tag; a scan checks the security image); the form, its one-line note and the list.

## Left to the builder

Field order and wording in the form beyond the help line; how the list abbreviates numbers (`1M`, `384k`).
