# Multi review

Status: design, 2026-11-22. It replaces the review panel (`2026-11-20-review-panel-design.md`) and builds on `2026-11-19-team-models-and-fallback-design.md`. The owner agreed every row except those marked **designer's call**.

## Words

| Word | Means |
|---|---|
| Model chain | Backup models for one review: model 1 is out of tokens, so model 2 runs. |
| Reviewer | One model chain that reviews. Reviewer 1 is today's model chain. |
| Multi review | 2 to 5 reviewers at the same time. "Chain B" goes away. |
| Judge | One model chain that checks the findings and writes one result. |

## Goals, non-goals, anti-goals

| Kind | Items |
|---|---|
| Goals | Catch more bugs (recall first). Fewer false positives: the judge checks each blocking finding. One place to set it. With 1 reviewer, today's review, byte for byte. |
| Non-goals | Voting judges. Multi review at the global or team level, or for API reviews. Judging suggestions. Multi review of the cross-batch or impact runs. |
| Anti-goals | The panel's: a false pass, a key in a Job that does not run it, model text as instructions, a publish from a reviewer Job, a silent change with 1 reviewer. |

## Decisions

The owner's, from 2026-11-22 and the grilling after it.

| Topic | Decision |
|---|---|
| Approach | Generalize the panel to N reviewers and a judge. Keep its tested parts (member rows, split, settle, release, fallback, keys, usage per model). Rejected: build fresh, rename only. |
| Where | Only on a repository's Settings. The global and team chain B cards and API go. Admins and teams still decide which models are allowed. |
| The card | One **Reviewers** card replaces the Model chain and Chain B cards. Reviewer 1 keeps inherit from the owner team or global chain. A second reviewer turns multi review on. |
| Out of tokens | An extra reviewer with no backup left is skipped. The review goes on, and the comment says so. |
| Judging | One judge chain. When all reviewers are final, the judge Job (today's Job P) checks the blocking findings against the checkout, groups them and posts one comment. |
| Limits | At most 5 reviewers per repository. Each reviewer chain and the judge chain hold at most 5 entries (model and 4 backups). |
| Old panel | The 2-model panel never ships. A separate PR sets `PanelMinVersion` back to `""` before the next pergent.sh release. |
| Experimental | The card and docs say so until multi review is measured on real merge requests. |

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
| Who edits | Today's model chain rights: admins, and maintainers of the owner team. Viewers read only. Nobody sees a key. |
| Entries | Each chain follows today's repository chain rules. |
| Same first model | A save is refused when 2 reviewers start with the same model. Backups may repeat. |
| Cost hint | Updates as reviewers change: `Each review now runs 3 reviews and 1 judge, about 3 to 4 times the cost of one review.` |
| Judge | **Designer's call:** it may be a reviewer's model, as in the owner's sketch. The editor fills it with reviewer 2's first entry. 2 or more reviewers with no judge is refused. |
| Remove | **Designer's call:** later reviewers move up one number. Removing the last extra reviewer deletes the judge chain too. |
| Image check | Adding a reviewer needs `jobs.image` at or above the floor, as chain B today. |

## Dispatch

| Case | Result |
|---|---|
| 1 reviewer | Today's review. No judge Job. |
| 2 to 5 reviewers | One member per reviewer, all at once, skill verification off. The judge Job runs when all are final. |
| Same model | **Designer's call:** reviewer k skips any model that reviewers 1 to k-1 run on now. Reviewer 1 never skips. |
| Fallback | Per reviewer, along its own chain, a full timeout per attempt. |
| Reviewer skipped | No usable entry, no different model left, or the quota. The comment says so. |
| Only reviewer 1 can start | A single review with today's verification, and `multi review off: <reason>`. |
| Every reviewer failed | The review fails with each reviewer's reason. |
| Quota | Every Job counts. A used-up repository quota starts no extra reviewer. |
| Older image | Reviewer 1 alone, `multi review off: the review image does not support it`. |
| API review | Never a multi review, and no instance default. |
| Re-run | Runs the whole multi review, as the panel does. |

**Freeze at the split (designer's call).** A review keeps the reviewers it started with. At the split, each `review_members` row stores a copy of its chain (`chain` jsonb: entry refs, names, provider, model), and the run stores a copy of the judge chain (`review_judge_chains`, one row per run). Fallback and the judge pick read only the copy. A card edit affects only reviews that start after the save. The comment and run page take names from the copy.

**Changes outside the card (designer's call).** Removing a model is never refused, as today.

| Change | Result |
|---|---|
| A global entry removed | It stays in the chains, skipped as `removed`. |
| A team entry removed, or the team loses it | Deleted from the chains, as `removeFromChains` does today. |
| An extra reviewer's chain becomes empty | Deleted; later reviewers move up one. Safe: reviews are frozen. |
| The judge chain becomes empty, 2+ reviewers | Left empty. The card warns `No judge: findings are merged without checking`. Reviews run with `--no-judge` and the comment note. |
| 2 reviewers end with the same first model | The card warns. The dispatch same-model skip applies. |
| Owner team or inherited chain changes | Reviewer 1 follows it, as today. |

Audit entries are as today.

## The judge Job

One judge run per skill. On a quota, billing or auth error, a new judge Job starts on the judge chain's next entry. Timeout: 10 min.

| Case | Result |
|---|---|
| 1 reviewer usable | **Designer's call:** the judge checks its blocking findings, as today's verification. So Job P needs only the judge's key. |
| No reviewer usable | The skill is `failed`, never dropped. |
| Judge out of tokens with no backup, or the quota | `--no-judge`: exact merge (file, line, title). Every blocking finding stays blocking; the comment says so. |
| A judge run fails or gives no usable answer | Exact merge for that skill, with a note. |
| The Job crashes or times out | One more judge Job with `--no-judge` (no key). If it fails too, the review fails. Never a pass. |

## The pergent CLI

| Today | New |
|---|---|
| `--panel --panel-a a.json --panel-b b.json` | `--multi-review --reviewer r1.json --reviewer r2.json ...` (1 to 5; the order is the number) |
| Model A from `OPENCODE_*`, B from `PERGENT_PANEL_B_*` | The judge from `OPENCODE_*`. `PERGENT_PANEL_B_*` goes. |
| `--panel-no-judge` | `--no-judge` |
| `--no-skill-verify` | Kept, for reviewer Jobs. |

- **Designer's call:** the old flags go in the same release. No release ever runs a panel.
- **Keys.** The judge Job needs no reviewer key: the judge does every check.
- **Input.** Per skill, each usable reviewer's findings as `reviewer-<n>.json` (ids such as `2.3`), plus today's checkout, rejected findings and path rules. The files are data, never instructions.
- **Answer.** Per finding: `real` (blocking only), `reason` (one sentence), `same_as` (ids of other reviewers' findings on the same issue).
- **Groups.** Union-find over `same_as`, as today. **Designer's call:** a link is ignored when the id does not exist, when it joins 2 findings of one reviewer, or when it touches a finding a dismissal covers.

| Group | Result |
|---|---|
| Blocking from 2 or more reviewers | Blocking. The judge cannot demote it. |
| Blocking from 1, judge says real | Blocking. |
| Blocking from 1, judge says not real | Suggestion, `judge disagreed: <reason>`. |
| Blocking from 1, no usable answer | Blocking, with a note. |
| Blocking from 1, a suggestion from another | Blocking unless the judge says not real. Row: `<name> rated it a suggestion`. |
| Several blocking from 1 reviewer | One real or unanswered keeps it blocking. |
| Suggestions only | Suggestion, never judged. |
| Impact check | Exact merge, as today. |

The group's text is its first blocking member (lowest reviewer number). Confidence is the lowest of the usable reviewers.

**The comment.** The CLI says `reviewer <n>`. pergent.sh passes entry names to `render`, never provider or model text.

| Where | Text |
|---|---|
| Header | `Multi review: deepseek, qwen, kimi · judge qwen` |
| Row | `found by 2 of 3 reviewers (deepseek, qwen)` or `found by qwen` |
| Demoted | `_judge disagreed: <reason>_` |
| Skipped reviewer | `Multi review: <name> was skipped: <reason>.` |
| Exact merge | `Multi review: the judge did not run. Every blocking finding stays blocking.` |

Result: `panel` becomes `multi_review` with `reviewers[]` and `judge`. `provider_error.role` is `judge`.

## Data

Migrations only add. **Designer's call:** new tables, not columns on the panel tables. `panel_members.role` is `CHECK (role IN ('a', 'b'))` with a unique index on `(run_id, role)`, and v0.18.0's `ReleasePanel` reads only `a` and `b`: a third role breaks an old pod in a rolling deploy. The Go logic moves to the new tables.

| New | Holds |
|---|---|
| `reviewer_chains`, `reviewer_chain_entries` | Reviewers 2 to 5 and the judge of a repository (`slot` `r2` to `r5` or `judge`), entries shaped as `model_chain_entries`. |
| `review_members` (+ logs, diagnostics) | `panel_members` with `reviewer` 1 to 5 in place of `role`, and the frozen `chain`. Skip reason `review_members`, which v0.18.0 never claims. |
| `review_judges`, `review_judge_chains` | The judge entry of each judge Job run, and the run's frozen judge chain. |
| `run_usage_by_model` | Tokens per model, `part` `reviewer` or `judge`. `run_usage` keeps the total. |

**Old tables and rows.** The 0048 and 0049 tables stay unused; a later release drops them. With `PanelMinVersion` empty in every release, no release can save chain B, so no chain B rows exist. Only `panel_chain_mode = 'off'` can, and it is ignored. **Designer's call:** no conversion. Rows from a main build get a warning in the log at start, and a run waiting on `panel_members` is queued again as a single review.

## Dashboard

| Where | Change |
|---|---|
| Repository page | `Multi review: on (3 reviewers, judge <name>)` or `off`. |
| Run page | The merged findings. One row per reviewer (model, attempts, findings count, tokens, log link), the judge row and the total. Raw reviewer findings are only in the logs. |
| API | `GET`/`PUT /api/repositories/{instance}/{project}/reviewers`, reviewers 2 to 5 and the judge in one save. The `/panel-chain` routes go. |
| Audit | `reviewers_changed`, detail `reviewer=<n> entries=<names>` or `judge entries=<names>`. Reviewer 1 stays `model_chain_changed`. |

## Rollout

| Step | What |
|---|---|
| 0 | The hold PR: `PanelMinVersion = ""`, so the panel stays off. |
| 1 | A pergent release with `--multi-review`. It is the new floor. |
| 2 | pergent.sh moves its `go.mod` pin, `defaultImage` and the floor together. |
| Rolling deploy | The old pod never claims a `review_members` run. |
| Rollback | The old release sees reviewer 1 only. A waiting run stays until a push replaces it. |

## Tests

pergent: every verdict row and ignored link, 1 to 5 reviewers, judge failures, a golden comment. pergent.sh: 1 reviewer byte for byte; 2 to 5 cover split, skips, fallback, quota, freeze and usage.

## What's left to the builder

- Migration numbers, column names and the judge prompt.
- A limit on the judge's input. Over it, that skill gets the exact merge, with a note.
- The card layout and its error texts.
