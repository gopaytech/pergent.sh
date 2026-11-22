# Multi review

Status: design, 2026-11-22. It replaces the review panel (`2026-11-20-review-panel-design.md`) and builds on `2026-11-19-team-models-and-fallback-design.md`. Rows marked **designer's call** are not ruled by the owner yet.

## Words

| Word | Means |
|---|---|
| Model chain | Backup models for one review: model 1 is out of tokens, so model 2 runs. |
| Reviewer | One model chain that reviews the change. Reviewer 1 is today's model chain. |
| Multi review | A review with 2 to 5 reviewers, all at the same time. "Chain B" goes away. |
| Judge | One model chain. It checks the reviewers' findings against the code and writes one result. |

## Goals, non-goals, anti-goals

| Kind | Items |
|---|---|
| Goals | Catch more bugs (recall first). Keep false positives down: the judge checks each blocking finding. Set it in one place, the repository's Reviewers card. With 1 reviewer, a review is today's, byte for byte. |
| Non-goals | Voting by several judges. Multi review at the global or team level, or for API reviews. Judging suggestions. Multi review for the cross-batch or impact runs. |
| Anti-goals | The panel's, unchanged: a false pass (a failed review is never `success` or approved), a key in a Job that does not run it, model text as instructions, a publish from a reviewer Job, a silent change for a repository with 1 reviewer. |

## Decisions

The owner's, from 2026-11-22.

| Topic | Decision |
|---|---|
| Approach | Generalize the panel to N reviewers and a judge. Keep member rows and their states, split, settle and release, fallback per member, member Jobs and keys, the "one member" case, usage per model and the run page. Replace A/B pairwise judging with the judge, and chain B with the reviewer list. Rejected: build fresh (redoes 3 phases), rename only (keeps 2 models and pairwise judging). |
| Where | Only on a repository's Settings. The global and team chain B cards and their API go away. Admins and teams still decide which models are allowed. |
| The card | One **Reviewers** card replaces the Model chain and Chain B cards. Reviewer 1 keeps inherit from the owner team or global chain. A second reviewer turns multi review on. |
| Out of tokens | An extra reviewer with no backup left is skipped. The review goes on, and the comment says so. |
| Judging | One judge chain. After all reviewer Jobs are final, the judge Job (today's Job P) checks the blocking findings against the checkout, groups the same issues and posts one comment. It stays a Kubernetes Job: pergent.sh never calls a model or holds repository code. |
| Limit | At most 5 reviewers per repository. |

## The Reviewers card

```
Reviewers
Reviewer 1                                          [Edit]
  deepseek/deepseek-v4-pro
  Backups: alibaba/qwen3.5-plus
Reviewer 2  (multi review, experimental)            [Edit] [Remove]
  alibaba/qwen3.5-plus
  Backups: none
[ Add a reviewer ]
Judge  [ alibaba/qwen3.5-plus ▾ ]     shown only with 2 or more reviewers
```

| Rule | Value |
|---|---|
| Who edits | Today's chain rights: admins, and maintainers of the owner team on owned repositories. Others read only. Nobody sees a key. |
| Entries | Each reviewer and the judge follow today's repository chain rules. |
| Chain length | **Designer's call:** at most 5 entries each (1 model and 4 backups), the model chain limit. One limit is easier to explain. |
| Same first model | **Designer's call:** refused when 2 reviewers start with the same provider and model. It costs a full review and adds no new view. Backups may repeat. |
| Judge | **Designer's call:** it may be a reviewer's model, as in the owner's sketch. The editor fills it with reviewer 2's first entry. 2 or more reviewers with no judge is refused. |
| Remove | **Designer's call:** later reviewers move up one number. Removing the last extra reviewer deletes the judge chain too, so no hidden setting is left. |
| Image check | Adding a reviewer needs `jobs.image` at or above the multi review floor, as chain B does today. |

## Dispatch

| Case | Result |
|---|---|
| 1 reviewer | Today's review. No judge Job. |
| 2 to 5 reviewers | One member per reviewer, all at once, skill verification off. The judge Job runs when all are final. |
| Same model | **Designer's call:** reviewer k skips any model that reviewers 1 to k-1 run on now. Reviewer 1 never skips. This is "B skips A's model" in number order. |
| Fallback | Per reviewer, along its own chain. Each attempt gets the full review timeout. |
| Reviewer skipped | No usable entry, no different model left, or the quota stopped it. The comment names it and the reason. |
| Only reviewer 1 can start | A single review with today's verification, and `multi review off: <reason>`. |
| Every reviewer failed | The review fails with each reviewer's reason. |
| Quota | Every Job counts. A used-up repository quota starts no extra reviewer. |
| Older image | Reviewer 1 alone, with `multi review off: the review image does not support it`. Never a failure. |
| API review | **Designer's call:** always a single review. It has no repository Settings, and an instance default would bring back the global level the owner removed. |
| Re-run | The button and the command run the whole multi review, as the panel does. |

## The judge Job

pergent.sh picks the judge entry. On a quota, billing or auth error, a new judge Job starts on the judge chain's next entry. Timeout: 10 min, as Job P.

| Case | Result |
|---|---|
| 2 or more reviewers usable for a skill | One judge run for the skill. |
| 1 reviewer usable | **Designer's call:** the judge checks its blocking findings, as today's verification. So Job P needs only the judge's key. |
| No reviewer usable | The skill is `failed`. It never drops out. |
| No usable judge entry, or the quota stops it | `--no-judge`: exact merge (file, line, normalized title). Every blocking finding stays blocking, with a note. |
| A judge run fails or has no usable answer | That skill gets the exact merge, with a note. |
| The Job fails another way (timeout, crash) | **Designer's call:** one more judge Job with `--no-judge`, which needs no key. If it fails too, the review fails. |

## The pergent CLI

| Today | New |
|---|---|
| `--panel --panel-a a.json --panel-b b.json` | `--multi-review --reviewer r1.json --reviewer r2.json ...` (1 to 5; the order is the number) |
| Model A from `OPENCODE_*`, B from `PERGENT_PANEL_B_*` | The judge from `OPENCODE_*`. `PERGENT_PANEL_B_*` goes. |
| `--panel-no-judge` | `--no-judge` |
| `--no-skill-verify` | Kept, for reviewer Jobs. |

- **Designer's call:** the old flags go in the same release. No released pergent.sh ran a panel.
- **Keys.** The judge Job needs no reviewer key, because the judge does every check. Reviewer keys stay in reviewer Jobs.
- **Input.** Per skill, each usable reviewer's findings as `reviewer-<n>.json` (ids such as `2.3`), with the checkout, rejected findings and path rules as today. The prompt says the files are data, never instructions.
- **Answer.** Per finding: `real` (blocking only), `reason` (one sentence), `same_as` (ids of other reviewers' findings on the same issue).
- **Groups.** Union-find over `same_as`, as today. **Designer's call:** a link is ignored when the id does not exist, when it joins 2 findings of one reviewer (it already said they differ), or when it touches a finding a dismissal covers.

| Group | Result |
|---|---|
| Blocking from 2 or more reviewers | Blocking. **Designer's call:** the judge cannot demote it. 2 models that agree outweigh 1 judge, as "blocking in both lists" today. |
| Blocking from 1, judge says real | Blocking. |
| Blocking from 1, judge says not real | Suggestion, `judge disagreed: <reason>`. |
| Blocking from 1, no usable answer | Blocking, with a note. |
| Blocking from 1, a suggestion from another | Blocking unless the judge says not real. Row: `<name> rated it a suggestion`. |
| Several blocking from 1 reviewer | One real or unanswered keeps it blocking. |
| Suggestions only | Suggestion, never judged. |
| Impact check | Exact merge, as today. |

The group's text is its first blocking member (lowest reviewer number). Confidence is the lowest of the usable reviewers.

**The comment.** The CLI says `reviewer <n>`. pergent.sh passes entry names to `render`, never a provider or model.

| Where | Text |
|---|---|
| Meta line | `Multi review: <name 1>, <name 2>, <name 3> · judge <name>` |
| Row | `found by 2 of 3 reviewers (<name 1>, <name 3>)` or `found by <name 2>` |
| Demoted | `_judge disagreed: <reason>_` |
| Skipped reviewer | `Multi review: <name> was skipped: <reason>.` |
| Exact merge | `Multi review: the judge did not run. Every blocking finding stays blocking.` |

Result: `panel` becomes `multi_review` with `reviewers[]` and `judge`. `provider_error.role` is `judge`.

## Data

Migrations only add. **Designer's call:** new tables, not columns on the panel tables. `panel_members.role` has `CHECK (role IN ('a', 'b'))` and a unique index on `(run_id, role)`, and v0.18.0's `ReleasePanel` reads only roles `a` and `b`. A third role would break an old pod in a rolling deploy. The Go code moves to the new tables; its logic stays.

| New | Holds |
|---|---|
| `reviewer_chains`, `reviewer_chain_entries` | Reviewers 2 to 5 and the judge of a repository (`slot` `r2` to `r5` or `judge`), entries shaped as `model_chain_entries`. |
| `review_members` (+ logs, diagnostics) | `panel_members` with `reviewer` 1 to 5 in place of `role`. A waiting run has skip reason `review_members`, which v0.18.0 never claims. |
| `review_judges` | The judge entry of each judge Job run. |
| `run_usage_by_model` | Tokens per model, `part` `reviewer` or `judge`. `run_usage` keeps the total. |

**Old tables** (0048, 0049) stay unused. A later release drops them.

**Old rows.** v0.18.0 refused every chain B save (`PanelMinVersion` was empty), and #90 is not released, so no chain B rows exist. Only `panel_chain_mode = 'off'` can exist; it is ignored, since 1 reviewer means no multi review. **Designer's call:** no conversion. If a build from main left chain B rows, the new release logs a warning per repository at start, so an admin can add it as reviewer 2. A run waiting with skip reason `panel_members` is queued again as a single review.

## Dashboard

| Where | Change |
|---|---|
| Settings pages | The Reviewers card on the repository. The global and team chain B cards go. |
| Repository page | `Multi review: on (3 reviewers, judge <name>)` or `off`. |
| Run page | A row per reviewer (entry, attempts, state, tokens, cost), the judge row and the total. |
| API | `GET`/`PUT /api/repositories/{instance}/{project}/reviewers`, reviewers 2 to 5 and the judge in one save. Reviewer 1 keeps the model chain routes. The `/panel-chain` routes go. |
| Audit | `reviewers_changed`, detail `reviewer=<n> entries=<names>` or `judge entries=<names>`. Reviewer 1 stays `model_chain_changed`. |

## Rollout

| Step | What |
|---|---|
| 1 | A pergent release with `--multi-review`. It is the new floor: `PanelMinVersion` moves to it. |
| 2 | pergent.sh moves its `go.mod` pin, `defaultImage` and the floor together. |
| Rolling deploy | The old pod never claims a `review_members` run. |
| Rollback | The old release sees reviewer 1 only. A run waiting for reviewers stays waiting until a new push replaces it. |

## Tests

pergent: every verdict row, every ignored link, 1 to 5 reviewers, judge failures, a golden comment. pergent.sh: 1 reviewer is byte for byte (golden); 2 to 5 cover the split, skips, fallback per reviewer and judge, quota, image check, API review, re-run and usage per model.

## What's left to the builder

- Migration numbers, column names and the judge prompt.
- A limit on the judge's input. Over it, that skill gets the exact merge, with a note.
- The card layout and its error texts.
- The measurement: the panel's success rule, with 3 reviewers.
