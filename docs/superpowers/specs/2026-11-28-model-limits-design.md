# Context window and max output per model entry

## Why

opencode reads a model's context window and max output from its own catalog. A model id the catalog does not have gets context `0` and a max output of 32,000 tokens (`OUTPUT_TOKEN_FALLBACK`, opencode 2.3.2). With context `0`, opencode never compacts ("With no window at all, only a provider rejection can limit the request", `compaction.ts`).

The first real security scan, on a model whose id the catalog spells differently (dots instead of dashes), stopped after 40 steps: three replies in a row ended with `finish = length` at 829, 392 and 81 output tokens, and opencode failed with "Response still truncated after two output token limit continuations". The model has a 1M context window and 384K max output. Thinking and output shared the 32,000 budget.

pergent reviews read `PERGENT_CONTEXT_LIMIT`, but pergent.sh never sets it, `pergent scan` reads neither limit, and no output limit exists.

## Goals

- An admin (or a team maintainer, for a team entry) can give an entry its real context window and max output.
- Every Job that runs the entry uses them: reviews, multi-review reviewers and the judge, and security scans.
- Entries that leave the fields empty behave exactly as today.

## Non-goals

- pergent.sh guessing limits (cleaned-up ids, the provider's `/models` endpoint).
- One install-wide default for unknown models.
- Blocking the fields on older pergent images.

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
| Context window (`context_limit`) | Most tokens in one request, input and output together | opencode's catalog | 4,096 to 10,000,000 |
| Max output (`output_limit`) | Most tokens in one reply, thinking included | opencode's catalog, or 32,000 | 1,024 to the context window when set, else to 10,000,000 |

Whole numbers only. A max output above the entry's context window is refused.

### Where they live

- Global entries: 2 nullable integer columns on `llm_models`.
- Team entries: the same on `team_models`.
- Config file: `llm.context_limit`, `llm.output_limit`, and the same keys in each `llm.fallbacks` item. Checked with the same ranges at load.
- Migration `0056_model_limits_per_entry.sql` adds the columns. The previous release ignores them.

Who edits them: whoever may edit the entry's base URL and key. A change applies from the next Job.

### API and dashboard

- The entry API takes and returns `contextLimit` and `outputLimit`; `null` is empty. A refused value is a 400 with the range in the message.
- The model form (admin Settings, a team's Models tab) has the 2 fields with the help line: "Leave empty for models opencode knows. Set both from the provider's page for a model id it does not know."
- Entry lists show the limits as `1M / 384k`, or nothing when both are empty.

### Jobs

A review, judge or scan Job gets `PERGENT_CONTEXT_LIMIT` and `PERGENT_OUTPUT_LIMIT` from the entry it runs, each only when set. After a fallback, the next Job uses the next entry's limits.

### pergent

- `pergent scan` reads `PERGENT_CONTEXT_LIMIT` / `--context-limit`, as reviews do.
- `pergent` reviews and `pergent scan` read a new `PERGENT_OUTPUT_LIMIT` / `--output-limit`.
- The generated opencode config declares the model with `limit: {context, output}` when either is set; an unset one is `0`, which opencode reads as "use your own value". opencode caps the request at 256,000 output tokens itself.

### Older pergent images

pergent v0.13.0 reviews read the context limit and ignore the output limit; v0.13.0 scans ignore both. The fix needs the next pergent release, and pergent.sh's default image moves to it in the same pergent.sh release. The docs say so. An ignored env var does no harm, so the fields are never blocked.

## Tests

- pergent: the generated review and scan configs hold the limits (both, one, none); a bad value is refused; `pergent scan` passes the flag and the env.
- pergent.sh: migration; config load ranges; store round trip for global and team entries; API set, clear and refused values (including output above context); Job env with and without values, for a review, a judge and a scan, and after a fallback; the form and the list.

## Left to the builder

Field order and wording in the form beyond the help line; how the list abbreviates numbers (`1M`, `384k`).
