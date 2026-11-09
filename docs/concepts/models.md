# Models

Reviews run on a model from a list. One model is the default. A repository can pick another one.

## The list

Admins manage it under **Admin → Settings → Models**. Nobody else sees it.

An entry is a provider, a model, a base URL (optional) and an API key.

| Action | What it does |
| --- | --- |
| Add | The first entry becomes the default. |
| Edit | Changes the base URL, and the key if you type one. It applies from the next review. |
| Set as default | Asks to confirm, and says how many repositories move. |
| Remove | Refused while a repository uses the model, and for the default. The refusal names the repositories. |

- The key is write-only. The page shows whether one is set.
- Each entry has its own key, even for the same provider. A Job gets only the key of the model it runs.
- The provider and model can't be edited. Add another entry instead.
- The same provider, model and base URL twice is refused.
- Nothing is tested when you add a model. A review that can't use it fails and says why.
- Removing a model never moves a repository to another one.

## The default

A repository that picks no model runs on the default. Every repository starts that way.

Changing the default moves all of them from their next review. An open merge request is left alone until its next push or re-run.

## A repository's model

Pick it on the repository's **Settings** tab, under **Model**. **Default** follows the default.

- Only admins change it. Anyone else sees the model's name, read-only, and the API answers them 403.
- A reset to defaults by an admin puts it back to **Default**. A maintainer's reset leaves it.
- A repository in the config file can't pick one. It follows the default.

## The config file

An `llm:` block in the config file adds one entry, `config`, shown as "from the config file".

- It is read-only. Change it in the file.
- It is the default while it exists, so **Set as default** is unavailable.
- Other entries can still be added, edited, removed and picked by repositories.
- The dashboard's own default is on hold meanwhile, so that entry can be removed.

See [Config reference](../reference/configuration.md).

## When a model can't be used

| Case | What happens |
| --- | --- |
| A repository's model is gone, or its key can't be opened | The review fails. It never falls back to the default. |
| No model at all, or no default for a repository that picked none | The review waits. |

The comment on the pull request or merge request says the chosen model can't be used and where an admin changes it. It doesn't name the model or the provider. The pergent.sh log has the entry's ID.

A key can't be opened when `PERGENT_SH_SECRET_KEY` changed. It counts as not set, and has to be entered again.

## A change of model

The first review after a change of model is a full review, not an incremental one. It happens once per open merge request.

| Change | Full review next? |
| --- | --- |
| A different provider or model | Yes |
| A new key or base URL | No |
| Going back to a default that is the same model | No |
| The upgrade to the release with the list | No |

A re-run of only the failed skills, or an incremental review, uses the model set at the time. So one published review can combine results from two models.

## Where you see the model

Only in the dashboard, never in the comment.

- The review page shows the model of the latest review.
- [Usage](usage.md) shows the provider next to each model, and every model a merge request used.
- The [audit log](dashboard.md#audit-log) records every change to the list and to a repository's model. It names the provider and model, never the key.

## A new provider needs network access

Jobs reach only the addresses in `jobEgress.cidrs`. Before you add a model from a new provider, put the provider's address, or your gateway's, in `jobEgress.cidrs` ([Install](../setup/install.md)).

pergent.sh can't check this for you. A review that can't connect fails, and its log shows the connection error.
