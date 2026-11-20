# Review panel

Status: agreed with the owner on 2026-11-20, section by section, in a design session. It builds on `2026-11-19-team-models-and-fallback-design.md` (chains and fallback).

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
| Where | In pergent.sh (approach B). Job A runs chain A and Job B runs chain B, in parallel. Each is today's review Job with its own verification off. Then Job P runs `pergent agent --panel` and writes one merged result. |
| Panel scope | Batches on both models, then cross-verify. Cross-batch and impact runs stay inside Jobs A and B. Each member Job is a full review. |
| Merge | A model merges, inside cross-verify. No separate merge run. |
| Config | A second chain, chain B, at the same 3 levels as team models (global, team, repository). Same edit rights. Fallback works inside each member. |
| Providers | Model B may be any provider, with its own key and base URL. |
| Publishing | Only Job P publishes. The running status is set at the first Job start, as today. Dismissals, carried files and CI results go to both A and B. |
| Cost and time | About 2 reviews and 2 small judge runs. A and B keep the review timeout. P starts when both are terminal and has its own timeout of 10 min. |
| Quota | The token quota counts all 3 runs. If the quota would stop B from starting, A runs alone and the comment says so. |

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
| Suggestion | Never judged, only merged. |

## Failures

| Case | Result |
|---|---|
| B's review fails | Continue with A only. In Job P, A's blocking findings get today's single-model verification. The comment says so. |
| One panel run fails | Its half of the judging is missing. Those findings stay blocking, with a note. |
| Both panel runs fail | Exact-match merge only. Every blocking finding stays, with a note. |
| The quota would stop B | A runs alone, with a note. |

## Data

| Change | Holds |
|---|---|
| `runs.panel_of` | Groups the 3 runs of one panel review. |
| `runs.panel_role` | `a`, `b` or `p`. |
| Usage | Per run, so the dashboard shows the cost per model and the total. |
| Fallback | A member that falls back uses `attempt_of`, as in team models. Job P waits for the member's final attempt. |

Migrations only add, so the previous release still runs.

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

- **A switch to skip verification** in the member Jobs, so A and B return their blocking findings before any judging.
- **`pergent agent --panel`** reads the results of A and B and the checkout. It starts run 1 on model B and run 2 on model A, each with only its own key in the opencode environment.
- **Grouping and verdict** follow the tables above. The verdict and confidence of each skill are computed after the merge.
- **Single-model fallback**: when only A's result is usable, it runs today's verification on A's blocking findings.
- **The comment** (`render/`):

  | Where | What |
  |---|---|
  | Header | `Panel: <model A> + <model B>` |
  | Each finding row | Tagged `both`, `A` or `B` |
  | A demoted finding | `<model> disagreed: <reason>` |
  | A partial panel | A note that says which part is missing |
  | Confidence | The lower of the two models |

pergent.sh then moves its `go.mod` pin and `defaultImage` together, as for any comment change.

## Dashboard

- **Run page**: the 3 runs show as one review, with the cost of each model and the total.
- **Repository page**: `Panel: on (A: <model>, B: <model>)`.
- **Repository settings**: chain B, edited like chain A.

## Security

| Job | Gets |
|---|---|
| A, B | Today's review Job inputs, with the key of their own entry only. |
| P | A read-only checkout (the init clone of the review Job), the 2 member results, and both LLM keys. No platform write token. |

In Job P each key reaches only the environment of its own opencode run.

## Tests

| Where | What |
|---|---|
| pergent | Unit tests with fake answers: grouping, every verdict row, every failure case, and a golden comment with the tags. |
| pergent.sh, chain B off | The review Job and dispatch are unchanged, byte for byte (golden). |
| pergent.sh, chain B on | With fake Job results: the 3-Job flow, `panel_of`, publish, B fails, B falls back. |

## Measurement

When the budget allows: the 5 saved bug cases plus 5 more, each on A alone, B alone and the panel.

**Success**: the panel finds at least 1 bug that neither single model found, at no more than 2.5x the cost of one review. Otherwise the panel stays experimental and off.

## Open questions

Not decided. The plan must not answer them without the owner.

1. **A member on fallback.** P waits for the final attempt. Does the review timeout count from that member's first attempt or its last? If B falls back to the same model as A, is it still a panel?
2. **The models in Job P.** Do runs 1 and 2 use the entry each member finally ran on, or the first usable entry of each chain when P starts? Can a P run fall back on quota, billing or auth?
3. **A fails and B passes.** The decisions cover B failing. Does A failing mean "B only", or a failed review?
4. **Mixed severities in a group.** A has it blocking and B has it as a suggestion. The group is tagged `both`. Does the row show that B rated it lower?
5. **Exact-match merge.** When both panel runs fail, what counts as the same finding: the fingerprint, or the file, line and title?
6. **One judge run per skill, or one for all skills?** Today's verification runs once per skill.
7. **Impact verification.** Impact runs stay inside A and B. Does their own verification also stay on there?
8. **Usage in Job P.** P spends tokens on 2 models in one run. `run_usage` is keyed by run and skill. How is P's spend split per model?
9. **Quota and Job P.** If the quota would stop P from starting, is that "both panel runs fail"?
10. **Dismissals of a merged finding.** A dismissal matches by fingerprint. The next review may take the group's text from the other model, with another fingerprint. How does the dismissal follow it?
11. **Storage of chain B.** `model_chains` has one chain per scope target. Does chain B get a new column, and does a repository get its own inherit or custom mode for it?
12. **An older pergent image.** It exits 2 on `--panel` and ignores a new verification switch. Is chain B refused until the pinned image supports the panel?
13. **API reviews and re-runs.** Does a review API request with a chain B run as a panel? Does a re-run command start all 3 Jobs again?
