# Choosing the model: a list, a default, and one per repository

Status: agreed with the owner on 2026-11-09. This is Project 2 of `2026-11-08-usage-visibility-vision.md`.

## Why

pergent.sh has one LLM setup, and every repository uses it. Usage now shows where tokens go, but nothing can act on it: a busy, low-risk repository costs the same per token as a critical one. And nobody reading a review can see which model wrote it.

## Goals

- A list of models an admin has added, each a provider, model, base URL and key.
- One default model, used wherever nothing else is set.
- A model per repository, picked from the list, instead of the default.
- The model each review ran on, visible on the review page and in Usage.
- Nothing changes on upgrade: every review keeps running on the model it runs on today.

## Non-goals

- A different model for verification, repair or cross-batch runs. A review uses one model throughout.
- Fetching the available models from a provider.
- Testing a model when it is added.
- Prices, budgets and allowances (Project 3).
- Showing the model in the review comment.

## Anti-goals

- **A key reaching anyone or anything that doesn't need it.** A key is sealed in the database, never returned by the API, and never logged. A Job gets only the key of the model it runs.
- **A key missing from redaction.** Every key in the list must be in the set pergent.sh redacts and scans for today (`Registry.LLMKeys`, `HeldSecrets`), or a key could reach a log, a comment or an installed skill.
- **A repository changing model by surprise.** Removing a model never moves a repository to another one.
- **A false pass.** A model that doesn't work fails the review with the reason, as a bad key does today. It never produces a clean result.
- **Non-admins spending money.** A team maintainer cannot change a repository's model, through the page or the API.
- **Breaking the previous release.** The migration only adds.

## Decisions

These are the owner's.

| Question | Decision |
|---|---|
| What is an entry in the list? | A provider, model and key, with a base URL, added by an admin. |
| Who chooses a repository's model? | Admins only. |
| Removing a model that repositories use | Refused until nothing uses it. The refusal names the repositories. |
| Today's setup on upgrade | Becomes the first entry and the default, automatically. |
| A setup in the config file | Shows as a read-only entry, changed in the file. |
| Models within one review | One model for the whole review. |
| Where the model shows | The dashboard only: the review page and the Usage page's merge request list. |
| Checking a model when it is added | No. A review that fails says why. |
| Editing an entry | Only its key and base URL. A different provider or model is a new entry. |
| Changing the default | Shows how many repositories it moves, and asks to confirm. |
| The first review after a model change | Full, not incremental, once per open merge request. |
| Open merge requests when a model changes | Left alone until their next push or re-run. |
| What a review records | The provider as well as the model, shown on reviews and in Usage. |
| A model that can't be used | The PR comment doesn't name it. The dashboard has the details. |
| A new provider's network access | The operator's job, documented. The failed review shows the connection error. |
| Keys | One per entry, even for the same provider. |
| Audit log | Every change to the list and to a repository's model, with provider and model, never the key. |
| An exact duplicate entry | Refused: same provider, model and base URL. |
| What counts as a model change | The actual provider and model, not the setting. Going back to a default that is the same model is no change. |
| The upgrade itself | Not a model change. An older review with no provider is compared by model name, so no full reviews follow the upgrade. |
| The provider in CSV and metrics | A new CSV column. No new metrics label for now, so existing series don't split. |
| The list page | Shows how many repositories use each entry, with their names one click away. |
| Delivery | One pull request. |

## How it works

### The list

A new table holds the entries. Each has a stable ID, a provider, a model, a base URL (may be empty) and a sealed key. One entry is the default.

Admins manage the list on **Admin → Settings**, replacing the LLM form there: add, change, set as default, remove. A key is write-only, as it is now: the page shows whether one is set.

An entry's provider and model are fixed once it is added, so a repository's model can't change underneath it. Its key and base URL can be edited, and apply from the next review. A new key or base URL is not a model change.

### On upgrade

The row in `llm_settings`, when there is one, becomes the first entry and the default. No row means an empty list, as today. The existing LLM routes (`GET` and `PUT /api/admin/llm`) are replaced by the list's routes. Everything else that reads "the" LLM moves to the default entry: `adminSettings` and the setup checklist.

**Rollback (builder-level call, mine).** `llm_settings` stays, and the default entry is written back to it whenever it changes, so a rolled-back release runs on the current default. Models chosen per repository don't exist in the previous release, so after a rollback every repository uses that default. `docs/upgrading.md` says so.

A config file with an `llm:` block supplies one more entry, read-only in the dashboard, with the fixed ID `config` (my call). As today, a config-file LLM wins: while it exists it is the default, and "set as default" is unavailable. Deleting the block from the file while a repository names `config` makes that repository's model unusable, covered below.

### A repository's model

A repository setting names an entry by ID. Empty means follow the default, and it is what every repository starts with, so changing the default moves every repository that hasn't chosen.

A repository listed in the config file follows the default, because config repositories aren't edited from the dashboard. A `repositories[].model` key can come later if it's wanted (my call).

It sits on the repository's Settings tab. Only admins can change it: for anyone else the control is read-only, and the API refuses the key, the same way the trusted-author settings do. Anyone who can see the Settings tab can see the model's name, never its key or base URL.

### A review

At dispatch pergent.sh resolves the repository's entry, or the default, and gives the Job that entry's provider, model, base URL and key. `runs.model` records the model, as it does today. A review already running is not affected by a change, and a re-run uses the model set at the time.

**When the model can't be used.**

| Case | What happens |
|---|---|
| A repository's chosen entry is missing, or its key can't be opened (`PERGENT_SH_SECRET_KEY` changed) | The review fails with the reason. It never falls back to the default. |
| No choice and no default | The review waits, as with no LLM today. |

The check that stops all dispatch while no LLM is set (`dispatch.go`) becomes per repository, so having no default doesn't hold up repositories that chose a model.

A re-run of only the failed skills, or an incremental push, uses the model set at the time, so one published review can combine results from two models (my call). The review page shows the latest model, and the Usage list shows every model used on that merge request.

### Removing

Refused with a 409 naming the repositories that use it, and the default can't be removed. Set another default first.

### Showing it

- The review page already shows the model. Nothing to build there.
- The Usage page's merge request list shows each model used on that merge request, because a change of model can leave more than one. This needs a new aggregate over `run_usage.model`.
- Past reviews are covered, except any whose model wasn't recorded (`runs.model` is written best-effort after launch). Those show no model.

### Tracking the change

A review records the provider and the model it ran on, in a new nullable column beside `runs.model`. When the next review of a merge request compares them with the last review's and they differ, it is a full review. Where the last review has no provider, only the model name is compared.

## Constraints

- Existing patterns: settings are nullable columns on `dashboard_repos`, secrets are sealed with `PERGENT_SH_SECRET_KEY`, admin routes go through `adminRoute`, and `openapi_test.go` checks every route and view.
- `docs/upgrading.md` must say what changes, and the config key rule applies: a new config key needs a rollback note.
- The review comment does not change.

## Left to the builder

- The table's and routes' names.
- The exact layout of the list on the Integrations page.
- Whether an entry has a display name, or is shown as provider and model.
