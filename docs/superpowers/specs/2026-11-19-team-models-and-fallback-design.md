# Team models and backup chains

Status: agreed with the owner on 2026-11-19, after a design session and a grilling round. It builds on `2026-11-09-model-catalog-design.md` and changes some of its rulings (see [Rulings this changes](#rulings-this-changes)).

## Why

Today every model and every key is global, and only an admin can choose a repository's model. A team that wants its own model, or wants to pay with its own key, cannot do that. And when a key is out of quota or credit, every review on it fails until someone fixes the key, even when another model or another key could do the review.

## Goals

- A team can add its own models and keys, and use them for its repositories instead of the global ones.
- A **chain**: an ordered list of entries, each a model with the key it uses. When the provider says an entry is out of quota, out of credit or rejects the key, the review starts again on the next entry.
- A chain at 3 levels: global, team and repository.
- A team can mix its own entries with global entries in any order. The same model can appear more than once with different keys.
- It is always visible which entry did a review, and why the earlier entries were skipped.
- Nothing changes on upgrade: every review runs on the model it runs on today, with no fallback until someone adds entries.

## Non-goals

- Fallback on a plain rate limit (429 without a quota reason), 5xx or a timeout. These fail as today.
- Fallback inside one Job, or a review that mixes 2 models.
- Fallback in the plain pergent CLI in CI, without pergent.sh. The CLI only gets the error classification.
- Asking a provider how much quota is left before a run.
- A new "team admin" role.
- New notifications (chat, email) when an entry is marked.
- Stopping opencode during its own retries. opencode 2.3.2 (pinned since pergent PR 40) does not retry quota, billing or auth errors, so the error arrives at once. A quota 429 that neither opencode nor pergent recognizes is still retried, and can end as a timeout with no class.
- Fallback within one API review. Its inputs are deleted once its Job starts, so a second attempt has nothing to run. An API review still skips marked entries when it starts.

## Anti-goals

- **A false pass.** If every entry fails, the review fails with each entry's reason. It never passes.
- **A key in a Job that does not run it.** A Job still gets only the key of the entry it runs.
- **A team's key paying for a repository the team does not own.** Checked when a chain is saved and again when each attempt starts.
- **A key missing from redaction.** Team keys are in `Registry.LLMKeys` and `HeldSecrets`.
- **A silent weaker model.** A fallback shows on the review page and in the MR or PR comment.
- **A wrong quota guess.** An unknown 429 is a rate limit, not quota.
- **A fallback that looks like a failure.** A dropped attempt does not raise the failure rate.
- **Breaking the previous release.** Migrations only add.

## Decisions

These are the owner's.

| Question | Decision |
|---|---|
| What triggers a fallback | Quota and billing errors, and auth errors. Nothing else. |
| Where the fallback happens | In pergent.sh: it starts a new Job on the next entry. One key per Job, one model per review. |
| What a team chain can hold | The team's own entries and the global entries an admin allows, mixed, in any order. |
| Can a team use global keys at all | Only the global entries with `teams may use` on (default off), and only the ones it puts in its chain. |
| Who manages team models, keys and chains | Every maintainer of the team. No new role. |
| Which team's chain a repository uses | Its **owner team**: one team per repository, ticked by an admin on the team's repository or instance entry. The most specific entry wins. |
| A repository-level chain | A maintainer of the owner team can give a repository a custom chain. It replaces the team chain for that repository only. |
| A repository with no owner team | Uses the global chain. Only an admin can give it a custom chain, from global entries only. |
| An owner team with no chain | Its repositories use the global chain. |
| Max chain length | 5 entries. A longer chain is refused on save. |
| 2 keys for the same model | Allowed. Each entry has a required **name**, unique in its team or among global entries. The old "exact duplicate refused" rule is dropped. |
| A global entry an admin removes | Chains that name it skip it, and the dashboard warns. |
| A team entry a maintainer removes | A confirm dialog lists the chains it changes, then it is removed from them. |
| How long an entry stays skipped | Quota or billing: `fallback.quota_cooldown`, one global setting, default `1h`. Auth: until someone edits the key. Anyone who may edit the entry can clear the mark. |
| Every entry exhausted | The run fails with `all models exhausted`. A chain with no entries at all waits, as an empty model list does today. |
| A chain edited during a fallback | Each attempt reads the chain again and takes the first usable entry this review has not tried yet. |
| The review after a fallback | A different model than the last review means a full review on the next push, as today. |
| Telling people about a mark | A banner on the team Models tab and the admin Models page while any entry has a mark. Alerting uses the metric. |
| Config-file installs | An optional `llm.fallbacks:` list gives them a global chain. |
| Rollback to the previous release | A repository whose chain starts with a team entry runs on the global default until the next upgrade. Documented in `docs/upgrading.md`. |
| A repository chain an admin saved (2026-11-20) | Each chain records whether an admin saved it. A repository chain an admin saved keeps its global entries usable, even when a team owns the repository and `teams may use` is off for them. A maintainer of the owner team who saves that chain may pick only the team's entries and the global entries with `teams may use` on, and the chain becomes team-saved. Global and team chains do not change. |
| Re-run and start under a used-up instance quota (2026-11-20) | The re-run button, the comment command and the start request no longer refuse at once when the instance quota is used up. They ask the resolver, with the same rules as dispatch, whether an entry is usable. If a team entry is left, the review starts; if none is, they refuse with today's message. The repository quota still refuses as today. |

## Rulings this changes

From `2026-11-09-model-catalog-design.md`, changed on purpose:

| Old ruling | New ruling |
|---|---|
| Only admins choose a repository's model. Anti-goal "non-admins spending money". | Maintainers of the owner team choose the team chain and the repository chain, and add team keys. They can use global keys only where an admin turned on `teams may use`. |
| A repository's chosen model never falls back to the default (`engine/dispatch.go` comment). | A chain falls back on quota, billing and auth errors only. A 1-entry chain still never falls back. |
| Removing a model that repositories use is refused. | A removed entry is skipped in, or removed from, the chains that name it, with a warning or a confirm. |
| An exact duplicate entry is refused. | Entries are told apart by a required name, so one model can have several keys. |
| The PR comment does not name the model. | Only after a fallback, the comment gets one footer line that names the backup entry and the reason. |

## Data

New tables:

| Table | Holds |
|---|---|
| `team_models` | `id, team_id, name, provider, model, base_url, api_key_sealed`. Sealed with `internal/secretbox`. `name` is unique per team. Separate from `llm_models`, so an older release never reads a team key as a global one. |
| `model_chains` | `id, scope (global\|team\|repo), team_id, instance, project`. One per scope target. |
| `model_chain_entries` | `chain_id, position, source (global\|team), model_id`. Position 1 is the first entry tried. At most 5 per chain. |
| `model_entry_marks` | `source, model_id, class, until`. `until` is NULL for an auth mark, which lasts until the key is edited. |

Changes to existing tables:

- `llm_models.name` (required, unique; filled from `provider/model` on upgrade) and `llm_models.teams_may_use` (bool, default false). The unique rule on `(provider, model, base_url)` is dropped.
- `team_repos.owner` bool, with at most one owner per `(instance, project)`. An instance entry (`project = ''`) is its own level.
- `dashboard_repos.chain_mode` = `inherit` (default) or `custom`.
- `runs.attempt_of` (nullable run id), `runs.chain_position`, `runs.model_source` and `runs.model_team_id`. A new outcome `fell_back`.
- `run_usage.source` and `run_usage.team_id`, so Usage splits global and team spend.

On upgrade:

- The current default becomes the only entry of the global chain. `llm_models.is_default` stays in step with position 1, so an older release still finds a default.
- A repository with `model_id` set gets a custom chain with that 1 entry. It behaves as today.
- `dashboard_repos.model_id` stays in step with position 1 of a custom chain when that entry is global, and is NULL for a team entry.
- An older release ignores `attempt_of`, so it shows each attempt as its own run. That is acceptable for a rollback.

## Who can edit

| Thing | admin | Maintainer of the owner team | Other maintainers, viewers |
|---|---|---|---|
| Global entries, global chain, `teams may use`, owner tick | yes | no | no |
| Team entries, keys, team chain | yes | yes | no |
| A repository's custom chain | yes | yes (owned repositories only) | no |
| Clear a mark | yes | team entries only | no |
| See chains, entry states, attempts | yes | yes | yes. Keys are never shown. |

## Choosing the entry

1. `chain_mode = custom`: the repository chain. Otherwise the owner team's chain, if it has one. Otherwise the global chain.
2. Walk the chain from position 1. Skip an entry when:
   - this review already tried it,
   - it has a live mark,
   - it is a removed global entry, or a global entry whose `teams may use` was turned off (team chains, and repository chains a team maintainer saved),
   - it is a team entry of a team that no longer owns the repository,
   - its key does not unseal,
   - it is a global entry and the **instance** token quota is used up.
3. The first entry left runs. Only its key goes in the Job Secret.
4. No entry left: if only the instance quota blocked global entries, the run waits on the quota hold as today. Otherwise the run fails with `all models exhausted`.

The **repository** token quota counts every token, team and global, and holds the whole run as today. The **instance** quota counts only tokens on global keys.

When the Job ends with `provider_error`:

| Class | pergent.sh |
|---|---|
| `quota`, `billing` | Mark the entry for `fallback.quota_cooldown`. Close this run row as `fell_back`. Create the next attempt. |
| `auth` | Mark the entry until its key is edited. Close as `fell_back`. Create the next attempt. |
| none, or `rate_limit` | As today. No fallback. |

**Attempts are run rows.** The next attempt is a new run with `attempt_of` set to the first run's id, created the way a re-run is created today. Each attempt keeps its own result, usage, log and diagnostics. The tokens a dropped attempt spent count in Usage and in both quotas, on the entry that spent them.

**Platform writes across attempts.** All attempts of one review share **one** status on the MR or PR. Its text becomes `retrying on backup model (2 of 3)`. On GitHub the same Check Run is updated, not a new one created. A dropped attempt publishes nothing else, as a replaced run publishes nothing today (`publish.go`). Only the final attempt posts threads, the comment and the final status.

**Counting.** No run metric has an outcome label, and adding one would split every series. So dropped attempts are left out of `pergent_reviews_total`, `pergent_reviews_no_result_total` and `pergent_skill_runs_total`, stay in `pergent_tokens_total`, and are counted in `pergent_model_fallback_total`. The failure rate stays true. The Reviews list shows one row per review, with its attempts inside. Two runs that mark the same entry at the same time both upsert the mark and move on.

## The pergent CLI

- **Read the whole error.** pergent's opencode error parser (`parser.go`) reads the error's type, status and response body. Shipped in pergent PRs 41 and 43, on the opencode 2.3.2 error shape from PR 40.
- **Classify:**

  | Class | Matches |
  |---|---|
  | `quota` | 429 with a known quota code in the body (for example `insufficient_quota`, `RESOURCE_EXHAUSTED` with a per-day quota ID), the same code in an OpenAI Responses stream, or an opencode quota type (`provider.quota`, `provider.free-limit`, `provider.budget-limit`, `provider.model-budget-limit`, `provider.go-limit`, `provider.chatgpt-limit`) with a status below 500 |
  | `billing` | 402, or 400 with a known "credit balance too low" message |
  | `auth` | opencode's provider auth error (it can have no status code), or 401, 403 |
  | `rate_limit` | any other 429 |

  The patterns are one Go table with a test per provider. The list is strict on purpose.
- **Stop on the error event.** opencode 2.3.2 retries a rate limit or server error up to 10 times (2 s doubling to 10 s, or the provider's `retry-after`), prints a `retry` event per retry, which pergent skips, and prints one `error` event at the end. It does not retry quota, billing or auth. pergent classifies the `error` event, stops opencode, and does not start it again for `quota`, `billing` or `auth`. `rate_limit` keeps today's retries. A per-call timeout is not used: it would kill opencode before the error event and hide the class.
- **A structured result field**: `provider_error: {class, status, provider, model}` next to the text `Failures`. pergent.sh decides only from this field. An older image without it gets today's behavior.
- The key env name stays `OPENCODE_API_KEY`, so CLI redaction does not change.

## Dashboard and API

- **Admin, Settings, Models page**: a name and a `teams may use` toggle per entry, and the global chain to reorder. The read-only `config` entry, and any `llm.fallbacks` entries after it, stay at the front while they exist.
- **Admin, Teams**: the owner tick on each repository or instance entry. A second owner at the same level is refused: `payments/api already has owner Payments`.
- **User area, Teams, Models tab**: the team's own entries and its chain. The picker shows team entries and global entries with `teams may use` on.
- **Repository settings, Model chain**: replaces the admin-only model menu. Inherit shows the chain in effect and where it comes from. Custom opens the editor.
- **Entry state** on every chain: `ready`, `exhausted until 14:00 (quota)`, `key rejected (auth)`, with **Clear mark**. A banner on the team Models tab and the admin Models page while any entry has a mark.
- **Review page**: the entry that ran (by name), its place in the chain, and each earlier attempt with its reason.
- **MR or PR comment**, only after a fallback: `Reviewed with qwen3.5 (backup model, the main model was out of quota)`, using the entry name.
- **Audit log**: every change to entries, keys, chains, `teams may use` and owner ticks, and every mark set or cleared. Never a key.
- **Metrics**: `pergent_model_fallback_total{from_model, to_model, class, source}` and the gauge `pergent_model_entry_exhausted{model, source, class}`, with entry names as values. No key or key ID in any label.
- **Breaking, dashboard API only**: the repository settings body loses the `model` key (the chain replaces it), and `/teams` becomes the user-area page instead of a redirect to `/admin/teams`. The web app changes in the same release, and `docs/upgrading.md` says so.
- **API**: `/api/admin/chain`, `/api/teams/{id}/models`, `/api/teams/{id}/chain`, `/api/repos/{instance}/{project}/chain`. Team routes check "maintainer of this team" in the handler. Admin routes keep the 404 for non-admins.

## Edge cases

| Case | Rule |
|---|---|
| Dashboard off, config only | No teams. `llm.fallbacks:` (same shape as `llm:`) follows the `config` entry in the global chain, read-only. |
| API clients | No team. A pinned model has no fallback, as today. No pinned model uses the global chain, skipping marked entries when the review starts, with no fallback within the review. |
| Owner changes or is removed | The repository uses the new owner's chain or the global chain. A custom chain loses the old team's entries, with a warning. |
| Team deleted | Its entries, keys and chain are deleted and audited. Its repositories follow the rule above. |
| Re-run button on a review that fell back | Starts a new review from position 1. Marks still apply. |

## Tests

- Classifier: a real error body per provider for quota, billing, auth (with and without a status code), plain 429, 5xx, and an unknown 429 that stays `rate_limit`.
- Error event: a fake opencode stream ending in a quota `error` event, where pergent stops and does not start opencode again.
- Resolver: custom, team and global; the most specific owner; a refused second owner; each skip reason; the instance quota skipping only global entries; mark expiry; a mark cleared by a key edit; the 5-entry limit; a chain edit between attempts.
- Engine with fake Job results: quota and auth create a `fell_back` row and a next attempt; a plain 429 does not; all entries exhausted gives `failed`, never a pass; a dropped attempt posts nothing but the shared status; each entry is tried once; a dropped attempt's tokens are counted.
- GitHub: one Check Run across attempts.
- Access: a viewer and a non-owner maintainer get 403 on chain and key edits; the route walker still sees admin routes as 404.
- Redaction: team keys are in `HeldSecrets()` and never in an API response.
- Migrations: only additions; today's data gives today's behavior.
- The full verify gate before release.

## Checked on 2026-11-19

1. opencode 1.19.4 prints no event per retry, retries a quota 429 at most 5 times, and has no retry setting. The error event carries `name`, `statusCode`, `responseBody` and `isRetryable`; `providerID` only on `ProviderAuthError`. pergent's `CLAUDE.md` note "opencode retries 5 times" is right about the count.
2. On 2026-11-20, after pergent moved to opencode 2.3.2: opencode types used-up allowances itself (`provider.quota` and 5 named limits) and does not retry them; a `retry` event comes before the final `error` event. pergent PR 43 added the quota-type rule and the Responses stream codes. The owner approved that a 429 opencode types as quota counts as `quota`.
3. The GitHub forge creates a new Check Run on every `SetStatus` call. One shared status needs an update call (`PATCH` on the Check Run) and the Check Run ID stored on the run. GitLab needs nothing.

The plans are `docs/superpowers/plans/2026-11-19-team-models-and-fallback.md` here and `docs/superpowers/plans/2026-11-19-provider-error-classes.md` in pergent.
