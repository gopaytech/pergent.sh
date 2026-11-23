# Review panel (experimental)

**Status: experimental, off by default.** A repository with no chain B in effect reviews as before, byte for byte. No image is accepted for a panel (see [The image check](#the-image-check)), so panels stay off. Multi review (several reviewer models and a judge) will replace this feature.

A review panel runs each review on two model chains and puts the two results together. Chain A is the [model chain](models.md) you know. Chain B is the second chain.

## The 3 Jobs

A repository with chain B in effect runs its review as a panel of 3 Jobs:

| Job | What it does |
| --- | --- |
| Member A | Reviews the change on chain A. |
| Member B | Reviews the same change on chain B, at the same time. |
| Job P (merge) | Starts after both members are final. Runs `pergent agent --panel`: it reads both results and puts them together. |

- Members run with skill verification off. They post nothing: no comment, note, thread, approval or status. Only Job P publishes.
- Job P has a 10 minute timeout.
- Job P waits until each member is final: done, or failed after its last attempt.

## The members

- **Fallback per member.** If a member's model is out of quota or its key is rejected, that member starts again on the next entry of its own chain. Each attempt gets the full timeout. The members do not fall back for each other.
- **B skips A's model.** When member B is dispatched, it never takes the model member A runs on. It takes the next entry of chain B. A does not skip B's model: if A falls back later, A can run on the model B runs on.
- **One member fails.** Job P goes on with the one result.
- **Both members fail.** The review fails.

## When the panel does not run: "A only"

Sometimes chain B is in effect but model B can not start. The review then runs on chain A alone, as a normal review, and the comment footer says why. The line is `panel off: <reason>`.

| Reason in the footer | Why |
| --- | --- |
| `the review image does not support it` | The review image is too old for `--panel`. |
| `no entry of chain B can be used` | No entry of chain B has a key that works (for example all are marked or removed). |
| `the token quota stopped model B` | The repository quota is used up, or the instance quota is used up and only global entries are left in chain B. |
| `model B is the model A runs on` | Every usable entry of chain B is the model A runs on. |
| `chain B could not be read` | A database error while loading chain B. The review still runs, on A alone. |
| `pergent.sh's own image is not set` | `PERGENT_SH_IMAGE` is empty. Job P needs it to fetch the members' results. The chart sets it. |

The run page shows the same reason in its **Review panel** card: [Dashboard](dashboard.md).

## What the comment shows

The comment names the models by their **entry names**, as `model A` and `model B` when an entry has no name. It never shows a provider or model name.

## Cost

All 3 Jobs spend tokens. Each model's tokens are counted per model: [Usage](usage.md#review-panels). All 3 Jobs count on the [token quota](token-quota.md).

## Chain B

Chain B is a list of models, like chain A. Its first entry is the second model of the panel. On each level it is one more card on a page that is already there. It has its own audit action, `panel_chain_changed`.

| Where | Page | Who changes it |
| --- | --- | --- |
| Instance | The admin **Settings** page, card **Global chain B (review panel)** | Admins |
| Team | The team's page under **Teams** (`/teams/<id>`), card **Team chain B (review panel)** | Admins and maintainers of the team |
| Repository | The repository's **Settings** tab (for a viewer, **Skills & instructions**), card **Chain B (review panel)** | Admins and maintainers of the owner team |

- The rights are chain A's. Everyone who sees a repository sees its chain B. Nobody sees a key.
- The global chain B is only a stored one. It has no config-file entries and no default entry.
- An empty chain B is no chain B. Save the global or a team chain B with no entries to clear it. A repository's chain B can't be saved empty: use **Inherit** or **Off**.

## The 3 modes of a repository

| Mode | Meaning |
| --- | --- |
| `inherit` | The chain B of the owner team, else the global one. This is the start. |
| `custom` | The repository's own chain B. |
| `off` | No panel for this repository, whatever its team or the global chain B say. |

- `off` is checked before the team and the global chain B.
- `off` always works. It works on a repository from the config file too, and it needs no image check.
- **Inherit** drops the repository's own chain B and goes back to the team or global one.

## The image check

No image is accepted for a panel. `PanelMinVersion` is empty, so saving a chain B with entries fails with `no review image supports a review panel yet`, the default `jobs.image` included, and a chain B that is already stored makes no panel. This feature will be replaced by multi review. Clearing a chain B, `off` and **Inherit** need no check.

## What you see

On the repository page, one line shows the panel (the mode **Off** is set on the repository's chain B card):

| Line | Meaning |
| --- | --- |
| `Panel: on (A: <entry>, B: <entry>)` | A chain B is in effect. |
| `Panel: off` | No panel. |
