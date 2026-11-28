# Models

Reviews run on a model from a **chain**: a list of up to 5 entries, tried in order. An entry is a model with the key it uses. When the provider says an entry is out of quota, out of credit, or rejects its key, the review starts again on the next entry. There are chains at 3 levels: global, a team's, and a repository's.

Nothing changes on upgrade. Every review runs on the model it used before, and nothing falls back until someone puts a second entry in a chain.

## Entries

An entry has a **name**, a provider, a model, a base URL (optional), a context window and a max output (both optional) and an API key.

- The name is 1 to 64 characters: letters, digits, spaces and `. _ : / + -`, starting with a letter or digit. It is unique among the global entries, and unique in its team, with no regard to case. After a fallback the merge request comment names the entry, so a name can hold no markdown.
- The same model can be added more than once, each time with its own key. The names tell them apart.
- The key is write-only. A page shows whether one is set, never the key. A Job gets only the key of the entry it runs.
- The provider and model can't be changed. Add another entry instead. The name, the base URL, the context window, the max output and the key can be changed, and the change applies from the next Job.
- Nothing is tested when you add an entry. A review that can't use it fails and says why.

### Context window and max output

These 2 optional fields tell a Job how big the model is. They are on global entries, on team entries, and in the config file (`llm.context_window` and `llm.max_output`, also in each `llm.fallbacks` item: see [Configuration](../reference/configuration.md)).

| Field | What it is | Range |
| --- | --- | --- |
| **Context window** | The most tokens the model can hold, input and output together. | 4,096 to 10,000,000 |
| **Max output** | The most tokens one reply may need, thinking included. | 1,024 to the context window |

- Both are whole numbers. **Set both, or neither.** An entry with only one is refused: `Set both Context window and Max output, or neither.` The reason is that opencode puts pergent's values over its own catalog, so a missing one would replace the catalog value.
- Empty means opencode's own values, as before. Opencode takes them from its catalog.
- Set them for a model id that opencode does not know, such as one on a custom base URL or one spelled another way. Take the context window from the provider's page.
- For max output, 64,000 is enough for reviews and scans. A provider that counts the asked amount against a per-minute rate limit throttles a higher value.
- A declared window sizes each reply to the room left. It does not make opencode compact: pergent keeps compaction off.
- Whoever may change the entry's base URL and key may change these. A change is in the audit, in the same entry as other edits to the entry, with the old and the new values. It applies from the next Job.
- Every Job that runs the entry gets them: reviews, the reviewers and the judge of a multi-review, and security scans. After a fallback, the next entry's values apply.

**The pergent image.** A Job gets `PERGENT_CONTEXT_LIMIT` and `PERGENT_OUTPUT_LIMIT` only when both fields are set and the Job's image is a pergent at or above the version that reads them. A review Job is checked against `jobs.image`, a scan Job against `jobs.security_image`. An image with a digest only, a tag such as `main`, or no tag never gets them. Older pergent versions read only the context window and also size the diff batches from it, so a large window would make batches too big. Until a pergent.sh release names the version, no image gets them. Values you save before then wait in the database, and the model form says in one line that Jobs do not use them yet. Jobs then run as if both fields were empty.

There are 2 kinds of entries:

| | Global entries | Team entries |
| --- | --- | --- |
| Where | **Models**, on the admin **Settings** page | The **Models** tab of the team's page under **Teams** (`/teams/<id>?tab=models`) |
| Who adds, changes and removes them | Admins | Admins and the maintainers of the team |
| Who sees them | Admins see the list. Others see an entry's name, provider and model in the chains they can see. | Every member of the team. A viewer sees no base URL. |
| Chains that can name them | The global chain and any repository chain. A team chain only while **Teams may use it** is on. | The team's own chain, and the chains of the repositories the team owns |

**Teams may use it** is a switch on each global entry, off by default. With it on, a team's maintainers can put the entry in their chains, and its key pays for those reviews. Turning it off makes those chains skip the entry. The config file's entries are never for teams.

| Removing | What happens |
| --- | --- |
| A global entry | Allowed while chains name it. The confirm lists those chains. They keep naming it, show it as `removed` and skip it. The default can't be removed: `this is the default model: make another one the default first`. |
| A team entry | The confirm lists the chains it is in. It is taken out of them, and its key is deleted. |
| A team | Its entries, its keys and its chain are deleted. Its repositories use the next chain in [Which chain a review uses](#which-chain-a-review-uses). |

## The config file

An `llm:` block in the config file adds one read-only global entry, `config`. `llm.fallbacks` adds up to 4 more after it, `config-1` to `config-4`. Each is named `provider/model` and shown as "from the config file". Change them in the file.

- They are first in the global chain, in the file's order, and can't be moved there. Entries an admin adds to the global chain come after them.
- `config` is the default while it exists, so **Set as default** is unavailable. The dashboard's own default is on hold meanwhile, so that entry can be removed.
- Without the dashboard, they are the whole global chain.

```yaml
llm:
  provider: anthropic
  model: claude-sonnet-4
  api_key_env: OPENCODE_API_KEY
  fallbacks:
    - provider: openrouter
      model: qwen/qwen3.5
      api_key_env: OPENROUTER_BACKUP_KEY
fallback:
  quota_cooldown: 30m
```

Each backup needs its own `api_key_env`, in the Secret like the main key. See [Config reference](../reference/configuration.md).

## The global chain and the default

Admins change the global chain on the admin **Settings** page, under **Global chain**. It holds global entries only.

- Until an admin saves it, the global chain is the default entry alone, or the config file's entries when the file has an `llm:` block. The first entry added to the list becomes the default.
- Saving the chain makes its first entry the default.
- Once the chain is saved, the chain decides what reviews try first. **Set as default** then also moves the entry to position 1 of the chain, and the other entries move down in their order. An entry that is not in the chain is added at position 1. When the chain already has 5 entries and the entry is not one of them, the dashboard refuses with 409 `the global chain has 5 entries and this model is not one of them: remove one from the chain first`.
- Saving it with no entries makes it the default entry alone again.

## Which chain a review uses

1. The repository's own chain, if it has one.
2. Else the chain of the repository's owner team, if that team has a chain.
3. Else the global chain.

### The owner team

An admin ticks **owner** on a team's entry for a repository, or for a whole instance, on the **Repositories** tab of the team's page (`/teams/<id>`). The repository page's **Teams** section does not set owners. An admin can also pick an **Owner team** when switching a repository on with **Add repository**. The repository page shows `Owner team: <team>` under its name.

- Each level has one owner at most. A second one is refused: `payments/api already has owner Payments`, or `main (whole instance) already has owner Payments` for an instance.
- The most specific owner wins: a repository's own entry, then its instance's.
- A repository with no owner team uses the global chain. Only an admin can give it a chain of its own, from global entries.
- When the owner changes, the repository uses the new owner's chain. A repository chain keeps the old team's entries and skips them, with the state `team no longer owns this repository`.

### A team chain

On the **Models** tab of the team's page, under **Team chain**. A maintainer of the team, or an admin, changes it. It can hold the team's own entries and the global entries teams may use, mixed, in any order.

A team with no chain of its own shows the global chain and says `This team has no chain of its own: its repositories use the global chain.` **Make a team chain** starts an empty one. Saving it with no entries goes back to the global chain.

### A repository's own chain

On the repository's **Settings** tab, under **Model chain**. A viewer has no Settings tab and sees the same card, read-only, on **Skills & instructions**. The card shows the chain in effect and where it comes from. **Give it its own chain** starts from the chain in effect. **Inherit again** drops the repository's own chain, after a confirm.

| Who | May put in it |
| --- | --- |
| An admin | Any global entry, and the owner team's entries |
| A maintainer of the owner team | The team's entries, and the global entries teams may use |
| Anyone else | Nothing: the card is read-only |

- **A chain an admin saved** keeps its global entries usable, even when a team owns the repository and **Teams may use it** is off for them. The card says `An admin saved this chain.` A maintainer who saves it again can keep only what teams may use. The editor marks the other entries `You can't keep this entry here. Remove it to save.` After the save, it counts as a team's chain.
- A repository in the config file can have its own chain too.
- A repository's chain needs an entry: `a repository's chain needs an entry: reset it to inherit its owner team's or the global chain`.
- **Reset to defaults** on Settings leaves the chain as it is.
- The upgrade gives every repository that picked a model before a chain of that one entry, saved by an admin, so it runs as before.

### What a chain may hold

A save that breaks a rule is refused with 400 and the reason:

| Rule | Refusal |
| --- | --- |
| At most 5 entries, the config file's included | `the chain has more than 5 entries` |
| Each entry once | `an entry is in the chain twice` |
| A team entry only in its own team's chain and in the chains of the repositories that team owns | `a team's entry can be in its own team's chain and in the chains of repositories the team owns, nowhere else` |
| A global entry in a team's chain only when teams may use it | `teams may not use this global entry: an admin can turn on teams may use` |
| The global chain holds global entries only | `the global chain holds global entries only` |

## Fallback

When a review's Job ends, pergent.sh reads `provider_error` in pergent's result:

| Class | Means | pergent.sh |
| --- | --- | --- |
| `quota` | The account is out of quota | Marks the entry for `fallback.quota_cooldown` (default `1h`), closes this attempt and starts the next one |
| `billing` | The account is out of credit | The same |
| `auth` | The provider rejected the key | Marks the entry until someone enters a new key for it, closes this attempt and starts the next one |
| `rate_limit`, or none | A plain rate limit, a server error, a timeout | As before: the review fails, with no fallback |

- **A chain of 1 entry never falls back.** It runs on its entry and fails as before. It is never marked, and it ignores marks that other chains set.
- **Each attempt reads the chain again** and takes the first entry that is not skipped and that this review has not tried. A chain edited during a fallback is used as it is now.
- **When no entry is left**, the review fails with `all models exhausted:` and each entry with why it was skipped, such as `all models exhausted: main (out of quota), backup (key rejected)`. It is never approved and never `success`. A repository's chain of 1 entry still fails with `the model chosen for this repository can't be used: an admin can change it on the repository's Settings tab`.
- **An empty chain** makes the review wait, as an empty list did before.
- **When only the token quotas and the model limits stop the entries this review has not tried**, the review is held as for a used-up quota ([Model and team limits](token-quota.md#model-and-team-limits)).
- After a fallback to a different model, the review of the next push is a full review on the next push ([A change of model](#a-change-of-model)).

Why an entry is skipped, as the failure text and the pages say it: `out of quota`, `out of credit`, `key rejected` (a mark), `already tried`, `removed` (`a removed model` in place of its name), `teams may not use it`, `its team doesn't own this repository`, `no usable key`, `instance token quota used up`, `model limit used up`, `team limit used up`.

### The pergent image

Fallback needs a pergent image that reports `provider_error` in its result. pergent v0.11.0, the default `jobs.image`, is the first release that does. With an older `jobs.image`, nothing falls back and no entry is marked: a review fails as before.

### Marks

A mark is on the entry, so every chain that names it skips it.

- A `quota` or `billing` mark ends after `fallback.quota_cooldown`. An `auth` mark ends when someone enters a new key for the entry.
- **Clear mark** ends one now. An admin can clear any mark, and a maintainer of a team can clear the marks of the team's own entries.
- While any entry has a mark, the admin **Models** list and the team's page show the banner `Reviews skip these entries until the mark ends or someone clears it.`, with each marked entry.
- Setting a mark is audited as `model_marked`, by `pergent`, with the detail `model=<name> class=<class>`. Clearing one is `model_mark_cleared`.
- Prometheus shows marked entries as `pergent_model_entry_exhausted` ([Metrics](metrics.md)).

Each entry of a chain shows its state:

| State | Means |
| --- | --- |
| `ready` | Reviews can use it |
| `exhausted until 2026-11-21 14:00 UTC (quota)` | Marked out of quota, or `(billing)` for out of credit |
| `key rejected (auth)` | Marked until its key is changed |
| `removed` | An admin removed the entry |
| `not allowed for teams any more` | A global entry teams may no longer use |
| `team no longer owns this repository` | A team entry in the chain of a repository its team doesn't own now |
| `key can't be read` | No key, or `PERGENT_SH_SECRET_KEY` changed. Enter the key again. |
| `model limit used up` | Its model limit is used up this month ([Model and team limits](token-quota.md#model-and-team-limits)) |
| `team limit used up` | The owner team's limit on it is used up this month. On a team's chain, it is that team's limit. |

### Review API

An API review with a pinned model runs on that model, as before. One with no pinned model uses the global chain and skips marked entries when it starts. It does not fall back within the review. When the global chain has 2 or more entries and the provider gives a quota, billing or auth error, the entry is marked and the review fails.

## Attempts

Each attempt is a run of its own, with its own Job, result, log and tokens.

- An attempt that is dropped for the next entry ends as `fell_back`, shown as **Retried on backup**. It posts nothing: no comment, no threads, no approval and no final status.
- All attempts of a review share one `pergent` status. While a backup runs, its text is `retrying on backup model (2 of 3)`. On GitHub the same Check Run is changed.
- The review page shows the entry of the run, `Model chain: <name> · entry 2 of 3 · team <team>` (or `global`), and an **Attempts** list. It links each attempt and says why an earlier one was dropped, such as `out of quota, so the review started again on the next model`.
- The **Reviews** list and the overview's recent reviews show one row per change, for its newest review. When that review fell back, the row says how many attempts it took, such as `2 attempts`.
- The tokens of a dropped attempt count in [Usage](usage.md) and in the [token quotas](token-quota.md), on the entry that used them. Dropped attempts are left out of the review counts in [Metrics](metrics.md).
- An attempt doesn't count against the re-run caps. A push during a fallback replaces it as usual, and the new head starts again at the first entry. **Re-run** starts a new review from the first entry, and marks still apply.

## The comment

The comment names the model only when the review used an entry that is not the first of its chain. A completed review then gets one footer line, `Reviewed with <name> (backup model, <reason>)`:

| Why the first entry was not used | Reason |
| --- | --- |
| Out of quota | `the main model was out of quota` |
| Out of credit | `the main model was out of credit` |
| Its key was rejected | `the main model's key was rejected` |
| Anything else (removed, not allowed, no usable key) | `the main model could not be used` |

A review that failed after a fallback has no such line. A review with no entry left lists the entries by name in its failure text, as above.

An error from the provider that names the model, the provider or the base URL says "the model" or "the provider" in the comment instead. The review's page keeps the full text.

## A change of model

The first review after a change of model is a full review, not an incremental one. It happens once per open merge request.

| Change | Full review next? |
| --- | --- |
| A different provider or model, also after a fallback to one | Yes |
| Another entry of the same provider and model, with another key | No |
| A new key or base URL | No |
| The upgrade to the release with chains | No |

A re-run of only the failed skills uses the model set at the time, so one published review can combine results from two models, and the review after it can carry files over from that combined result.

## Where you see the model

- A review's page shows the provider and model it used, or the one it tried when it couldn't start, and its place in the chain.
- [Usage](usage.md) shows the provider next to each model, and every model a merge request used.
- The [audit log](dashboard.md#audit-log) records every change to entries, keys, chains, **Teams may use it** and owner ticks, and every mark set or cleared. It names the entry, never the key.

## A new provider needs network access

Jobs reach only the addresses in `jobEgress.cidrs`. Before you add a model from a new provider, put the provider's address, or your gateway's, in `jobEgress.cidrs` ([Install](../setup/install.md)).

pergent.sh can't check this for you. A review that can't connect fails, and its log shows the connection error.

## Reviewers 2 to 5 and the judge

A repository's model chain is **reviewer 1**. Reviewers 2 to 5 and a judge are more chains, for [multi review](multi-review.md). It is experimental, and it needs pergent v0.12.0 or newer in `jobs.image`, the default. They are on the repository's **Reviewers** card and follow the same rights as the model chain.

## Rolling back

See [Upgrading](../upgrading.md). Under the previous release, a repository whose chain starts with a team entry, and every repository that follows a team chain, runs on the global default.
