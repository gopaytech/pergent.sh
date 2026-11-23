# Model limits

Status: agreed with the owner on 2026-11-22, after a design session and a grilling round. pergent.sh only: pergent and the review Job do not change.

## Why

Today pergent.sh has two monthly token quotas ([Monthly token quota](../../concepts/token-quota.md)):

- The **instance quota** counts the tokens of one GitLab or GitHub instance on global keys. When it is used up, a review skips the global entries of its chain.
- The **repository quota** counts every token of one repository, on any key. When it is used up, the whole review is held.

Neither one is per model. An admin cannot stop one expensive global model from using up the month, cannot give each team its own share of a global model, and a team cannot cap its own key. This design adds those three limits. The repository quota already is the combined limit for all models, so it stays as it is.

## The limits

| Limit | Set by | Counts | When it is used up |
|---|---|---|---|
| Instance quota (exists) | Admin | Global keys of one instance | Skip the global entries |
| **Model limit** (new) | Admin | Every token on global entry M, on all instances and repositories | Skip M |
| **Team limit on a global model** (new) | Admin | Tokens on M in the repositories team X owns | Skip M for team X's repositories |
| **Team limit on a team model** (new) | The team's maintainers, admins | Tokens on that team model | Skip that entry |
| Repository quota (exists) | Admin | Every token of the repository | Hold the whole review |

- Each limit is its own number, as the two quotas are today. An entry is skipped when **any** limit that applies to it is used up. A team limit may be larger than the model limit; then the model limit is reached first.
- **No row means no limit.** A team with no admin limit on M is limited on M only by the model limit and the instance quota. After the upgrade nothing changes until someone sets a limit.
- A team limit on M counts every review of a repository whose owner team is X that ran on M, whichever chain picked M: the global chain, the team's chain or the repository's. The owner team is the one models already use: the repository's own owner, else its instance's ([Models](../../concepts/models.md#the-owner-team)). A repository with neither has no team limit.
- An admin can set a team limit on any global model, with or without **Teams may use it**, because a team's repositories reach any global model through the global chain.
- **API reviews** have no repository and no owner team. They count toward the model limit of their entry and toward no team limit, and a used-up model limit skips that entry for them. Their client quota is unchanged.
- Usage is the same measure as the quotas: input plus output tokens of reviews that finished in the current UTC calendar month. A dropped attempt counts on the entry that spent the tokens. "Used up" means usage at or above the limit.

## Data

Migration `0050` adds one table and two columns, and drops nothing.

`model_limits`:

| Column | Meaning |
|---|---|
| `source`, `entry_id` | The entry, as in chain entries: `global` (an `llm_models` id, or `config`, `config-1` to `config-4`) or `team` (a `team_models` id) |
| `team_id` | NULL is the model limit. A team id is that team's limit |
| `monthly_tokens` | 1 to 1,000,000,000,000,000, the range of the quotas |
| `updated_at`, `updated_by` | For the audit log |

A unique index on (`source`, `entry_id`, `team_id`) with NULLs not distinct. A team model gets no NULL-team row: its team limit already covers every use of it. Removing a team deletes its rows. A removed entry leaves its rows unused, as chain entries do. There is no config-file key.

`run_usage.owner_team_id` and `run_usage_models.owner_team_id` (NULL: no owner team) are filled when a row is written, from `team_repos.owner`. So a repository that moves to another team mid-month leaves its earlier tokens with the old team. A model limit sums every row of the entry and does not need the column.

## The check

It runs where the chain picks an entry today, at dispatch, next to the instance quota.

1. Trust, as today.
2. The repository quota. Used up: the review is held, as today.
3. Walk the chain in order. The first entry that passes every check runs the review: no mark (exists); for a global entry, room under the instance quota (exists) and **under its model limit**; **room under its team limit for the repository's owner team**.
4. No entry left: the review is held exactly as the instance quota holds one: the same pending status, the once-a-month note and **Waiting for a start**. The forge texts do not change. They name no limit and no number, because anyone reads them on a public repository. The **Waiting for a start** line, which is dashboard-only, says `No model in the chain has room this month.` when a model limit or a team limit took part, and keeps `Monthly token quota used up.` when only quotas did.

Limits apply to a chain of 1 entry too: at a limit, it is held. Marks still do not apply to it. Marks come from provider errors, and limits are a budget, so a 1-entry chain is no way past a limit.

| Limit | Usage sum |
|---|---|
| Model limit | All rows of the entry this month |
| Team limit | Rows of the entry this month with `owner_team_id` = the team |

A one-model review counts on its run's entry. A panel counts each Job on its own entry, from `run_usage_models`, so the known panel gap of the instance quota does not apply to these limits.

The same walk is used by:

- **Fallback:** the next attempt skips used-up entries. When only quotas and limits stop every entry left, the review is held, as the instance quota holds one today. When a mark or a bad key is part of it, it fails with `all models exhausted:`, and the list names limits too: `main (out of quota), backup (team limit used up)`.
- **Requests by hand** (`review`, **Re-run**, a maintainer's **Start review**): started when the walk finds an entry, otherwise refused with 👎 or 409 `the monthly token quota is used up`, as today.
- **An admin's start:** the first entry with room. When none has room, it ignores admin limits (model limits, team limits on global models) but not a team's own limit on a team model, which is the team's money: it runs on the first global entry with no mark. With no such entry it is refused with 409 `every model of the chain is at the team's own limit: a maintainer of <team> can raise it on the team page`, shown beside the button.
- **Job P's judge** does not check limits. It ends a review that has already started, as a running review finishes after a quota runs out. Its tokens count on its entry.
- **Chain B:** a used-up limit skips the entry. When no entry of chain B is left, the panel is off for that review with `the token quota stopped model B`, and the review runs on chain A alone, as it does today.

A limit lowered below its usage stops the entry at the next dispatch. A limit raised or removed has room at once, but held reviews do not start by themselves: they wait for the next push, a command or an admin's start, as with the quotas.

When the usage can't be read, the dispatch is retried with the usual backoff and then fails. No review starts unchecked. A limit can be overshot, as a quota can: a review's size is known only after it runs.

## Dashboard

- **Admin Models page:** each global model, config entries included, shows `Tokens this month: <used> of <limit>` with a bar, and a limit field (empty: none). Under it, a collapsible **Team limits** list: team, limit, `<used> of <limit>`, add, change, remove.
- **Team page** (`/teams/<id>`), card **Model limits**, for the team's maintainers and admins: each team model with its limit (maintainers edit it), and each global model with an admin limit for this team (read-only for maintainers). A global model's model limit shows only `room left` or `used up`, because its numbers include other teams. Viewers and other teams see nothing.
- **Chain editors** tag an entry skipped right now with `model limit used up` or `team limit used up`, beside the existing `out of quota` and `no usable key`. The tags are dashboard-only.
- Bars are amber from 80% and red when used up, as for the quotas. The **Usage** pages do not change.

## API

Writes need the `X-CSRF-Token` header, as today.

| Route | Who |
|---|---|
| `PUT`, `DELETE /api/admin/models/{id}/limit` | Admin |
| `PUT`, `DELETE /api/admin/models/{id}/team-limits/{teamId}` | Admin |
| `PUT`, `DELETE /api/teams/{id}/models/{modelId}/limit` | The team's maintainers, admin |
| `GET /api/teams/{id}/limits` | The team's maintainers, admin |

`{id}` takes `config` and `config-1` to `config-4` for config entries. The admin model list gains `limit`, `used` and `teamLimits`. A number out of range is 400 with the range; a limit on another team's model is 400; a maintainer writing an admin limit is 403. The audit log gets `model_limit_set` and `model_limit_cleared` with the entry name, the team name (or `all teams`) and the number. `openapi.yaml` is updated.

## Tests

Each new test is seen to fail before its code.

| Layer | What |
|---|---|
| Store, Postgres | Both sums; `owner_team_id` written; a repository that moves team mid-month; the UTC month edge; panel Jobs on their own entries; NULL rows from before the upgrade |
| Chain walk | The check order; skip to the next entry; held when none is left; an admin's start; no owner team means no team limit |
| Engine, end to end | A team plus global chain runs on the next entry; a request by hand is refused; a fallback held vs failed; a 1-entry chain held; an admin's start and its 409; an API review counts on the model limit only; chain B turns the panel off; Job P's judge ignores limits |
| Dashboard API | 403 and 400 cases; audit entries; maintainers get only the state of a model limit |
| Web | The Models page field and team list; the team card's editable and read-only rows; the new tags |

`dispatch-job.golden.json` does not change: the Job gets no new input.

## Rollout

| PR | What |
|---|---|
| 1 | Migration `0050`, store, the chain walk, engine, API, `openapi.yaml`, docs. It works through the API alone |
| 2 | The Models page, the team card, the chain tags and the **Waiting for a start** text in the web app |

## Upgrade and rollback

- `0050` only adds. The previous release works on the migrated database and ignores it.
- In the upgrade month, team limits count only reviews that finished after the upgrade, because older rows have no `owner_team_id`. Model limits count the whole month.
- During a rolling upgrade, old pods start reviews without the new checks.
- Rolling back needs no config change. The rows stay unused.
- Docs: `token-quota.md` (a new section), `models.md` (the two skip reasons), the team page in `dashboard.md`, and `docs/upgrading.md` under `## Unreleased`.

## Out of scope

- Config-file keys for limits.
- A per-model limit on one repository.
- Daily or weekly limits.
- Alerts near a limit.
- Limits in money.
- A team setting its own lower limit on a global model.
