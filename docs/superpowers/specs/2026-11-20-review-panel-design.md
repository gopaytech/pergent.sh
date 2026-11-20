# Review panel

Status: agreed with the owner on 2026-11-20, section by section, in a design session. The owner answered all 16 open questions the same day. It builds on `2026-11-19-team-models-and-fallback-design.md` (chains and fallback).

## Why

| Evidence | Result |
|---|---|
| [SecPRBench](https://arxiv.org/abs/2610.33509) | Review recall goes from 32% to 78% by model choice alone. |
| [SWR-Bench](https://arxiv.org/abs/2509.01494) | Combining several reviews gives up to +43.67% F1. Not yet checked: whether it mixed models. |
| pergent code graph test | No gain. The graph stays frozen and experimental. |

Different model families miss different bugs. A second model is the cheapest way we know to catch more of them.

## Goals

- Catch more bugs. Recall comes first.
- Keep false positives down: each model checks the other's blocking findings.
- Model B can be any provider, with its own key and base URL.
- Off by default. With no chain B, a review is today's review, byte for byte.

## Non-goals

- More than 2 members.
- A separate merge run. The merge happens inside the cross-verify runs.
- A panel for the cross-batch or impact runs. They stay inside Jobs A and B as today.
- Judging suggestions. They are only merged.

## Anti-goals

- **A false pass.** A failed review is never `success` and never approved. The panel never fails a review that would pass on one model.
- **A key in the wrong place.** Only Job P holds 2 LLM keys, and each key reaches only its own opencode run.
- **Model text as instructions.** The other model's findings are data, with the same "not instructions" note used for dismissals.
- **A publish from a member.** Jobs A and B post nothing. Only Job P's result is published.
- **A silent change for a repository with no chain B.** Its Job and its dispatch stay the same, byte for byte.

## Decisions

These are the owner's.

| Topic | Decision |
|---|---|
| Where | In pergent.sh (approach B). Job A runs chain A and Job B runs chain B, in parallel. Each is today's review Job with its skill verification off. Then Job P runs `pergent agent --panel` and writes one merged result. |
| Panel scope | Batches on both models, then cross-verify. Cross-batch and impact runs stay inside Jobs A and B. Each member Job is a full review. |
| Verify switch | The new switch turns off only the skill verification. The impact check keeps its own verification inside Jobs A and B. |
| Merge | A model merges, inside cross-verify. No separate merge run. |
| Judge runs | One pair per skill (B judges A, A judges B), as today's verification runs once per skill. |
| Config | A second chain, chain B, at the same 3 levels as team models (global, team, repository). Same edit rights. Fallback works inside each member. |
| Providers | Model B may be any provider, with its own key and base URL. |
| Same model | If B falls back to the model A ran on, B skips that entry and takes its next different one. With none left, the review is "A only", with a note. |
| Models in P | Each judge run uses the entry its member really ran on, after fallback. A P run may fall back on quota, billing or auth along that member's chain. |
| Publishing | Only Job P publishes. The running status is set at the first Job start, as today. Dismissals, carried files and CI results go to both A and B. |
| Dismissals | A past dismissal matches a group when it matches the fingerprint of any member of the group. |
| API and re-runs | A review API request and a re-run command both run as a panel (all 3 Jobs) when the repository has chain B. |
| Cost and time | About 2 reviews and 2 small judge runs. Each attempt of A or B gets the full review timeout, as in team models (each attempt is a new Job). P waits for each member's final attempt, then has its own timeout of 10 min. At worst a panel takes 2 review timeouts, the same as a single review that falls back. |
| Quota | The token quota counts all 3 runs. If the quota would stop B from starting, A runs alone and the comment says so. |
| Older image | Chain B is refused on save unless `jobs.image` has a version tag at or above the first pergent with `--panel`. A tag that is not a version (for example `:main`) is refused too. If `jobs.image` later moves to an older or unknown tag, dispatch runs A only with the note `panel off: the review image does not support it`. It never fails the review. |

Why on save: pergent.sh cannot ask an image what it supports. It reads the version from a finished result or from the image tag (`CarriedSupport` in `internal/result/result.go`). An older P exits 2 on the unknown `--panel` flag.

## How Job P merges

**Run 1.** Model B gets A's findings (blocking and suggestions, each with an id) and B's own list. For each of A's findings it answers:

| Field | Meaning |
|---|---|
| `real` | Blocking findings only. Is the problem real in the current code? |
| `reason` | One sentence. |
| `same_as` | The id of B's finding on the same issue, or null. |

**Run 2.** Model A does the same for B's findings.

**Groups.** The `same_as` links from both runs form groups. A1 = B2 and B2 = A3 is one group. Each group gives one finding. Its text comes from the most severe member. It is tagged found by A, by B, or by both.

**Verdict.**

| Case | Result |
|---|---|
| Blocking in both lists | Blocking. Not judged. |
| Blocking from one model, the other says real | Blocking. |
| Blocking from one model, the other says not real | Demoted to a suggestion, with `<model> disagreed: <reason>`. |
| Blocking from one model, no usable answer | Stays blocking, with a note. |
| Blocking from one model, a suggestion from the other | Tagged `both`. The row says `<model> rated it a suggestion`. The verdict is "blocking from one model": it stays blocking unless the other model's judge run says not real. |
| Suggestion | Never judged, only merged. |

## Failures

| Case | Result |
|---|---|
| B's review fails | "A only". In Job P, A's blocking findings get today's single-model verification. The comment says so. |
| A's review fails, B's passes | "B only", the mirror of "A only". B's blocking findings get today's single-model verification in P. |
| One judge run fails | Its half of the judging is missing. Those findings stay blocking, with a note. |
| Both judge runs fail | Exact-match merge: today's `finding.Dedup` key (same file, same line, same normalized title). Every blocking finding stays, with a note. |
| The quota would stop B | A runs alone, with a note. |
| The quota would stop P | The same as "both judge runs fail". |

## Data

| Change | Holds |
|---|---|
| `runs.panel_of` | Groups the 3 runs of one panel review. |
| `runs.panel_role` | `a`, `b` or `p`. |
| `panel_chains`, `panel_chain_entries` | Chain B. New tables with the same shape as `model_chains` and `model_chain_entries`, read and written by the same Go code. Chain B has its own inherit or custom mode at each level. `model_chains` is unchanged. |
| `run_usage_models` | New table: run, skill, model and tokens. P writes one row per model, so the dashboard splits P's cost between the 2 models. |
| `run_usage` | Unchanged: one row per run and skill, the total. Its key stays. |
| Fallback | A member that falls back uses `attempt_of`, as in team models. Job P waits for the member's final attempt. |

Migrations only add, so the previous release still runs. No existing unique key changes.

## Who can edit

The same rights as the team model chain.

| Thing | admin | Maintainer of the owner team | Others |
|---|---|---|---|
| Global chain B | yes | no | no |
| Team chain B | yes | yes | no |
| A repository's chain B | yes | yes (owned repositories only) | no |
| See the panel setting and its runs | yes | yes | yes. Keys are never shown. |

## The pergent CLI

This part lands in the pergent repository. Its plan goes in pergent's own `docs/superpowers/plans/`.

- **A switch to skip the skill verification** in the member Jobs, so A and B return their blocking findings before any judging. The impact check's verification stays on.
- **`pergent agent --panel`** reads the results of A and B and the checkout. Per skill, it starts run 1 on model B and run 2 on model A, each with only its own key in the opencode environment.
- **Grouping and verdict** follow the tables above. The verdict and confidence of each skill are computed after the merge.
- **Single-model fallback**: when only one member's result is usable, it runs today's verification on that member's blocking findings.
- **Usage** is reported per skill and per model.
- **The comment** (`render/`):

  | Where | What |
  |---|---|
  | Header | `Panel: <model A> + <model B>` |
  | Each finding row | Tagged `both`, `A` or `B` |
  | A demoted finding | `<model> disagreed: <reason>` |
  | A mixed group | `<model> rated it a suggestion` |
  | A partial panel | A note that says which part is missing |
  | Confidence | The lower of the two models |

pergent.sh then moves its `go.mod` pin and `defaultImage` together, as for any comment change.

## Dashboard

- **Run page**: the 3 runs show as one review, with the cost of each model and the total.
- **Repository page**: `Panel: on (A: <model>, B: <model>)`.
- **Repository settings**: chain B, edited like chain A, with its own inherit or custom mode.

## Security

| Job | Gets |
|---|---|
| A, B | Today's review Job inputs, with the key of their own entry only. |
| P | A read-only checkout (the init clone of the review Job), the 2 member results, and both LLM keys. No platform write token. |

In Job P each key reaches only the environment of its own opencode run.

## Tests

| Where | What |
|---|---|
| pergent | Unit tests with fake answers: grouping, every verdict row, every failure case, usage per model, and a golden comment with the tags. |
| pergent.sh, chain B off | The review Job and dispatch are unchanged, byte for byte (golden). |
| pergent.sh, chain B on | With fake Job results: the 3-Job flow, `panel_of`, publish, A fails, B fails, B falls back to A's model, an API review, a re-run, the image check on save and at dispatch, `run_usage_models`. |

## Measurement

When the budget allows: the 5 saved bug cases plus 5 more, each on A alone, B alone and the panel.

**Success**: the panel finds at least 1 bug that neither single model found, at no more than 2.5x the cost of one review. Otherwise the panel stays experimental and off.
