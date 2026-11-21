# Review panel (experimental)

**Status: experimental, off by default; reviews do not use it yet.** Today you can only save the settings. No review reads them, and no review changes.

A review panel runs each review on two model chains and puts the two results together. Chain A is the [model chain](models.md) you know. Chain B is the second chain.

## Chain B

Chain B is a list of models, like chain A. Its first entry is the second model of the panel. On each level it is one more card on a page that is already there. It has its own audit action, `panel_chain_changed`.

| Where | Page | Who changes it |
| --- | --- | --- |
| Instance | **Admin → Settings**, card **Global chain B (review panel)** | Admins |
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

Saving a chain B with entries needs a review image that supports a review panel. No release supports it yet, so every save is refused with `no review image supports a review panel yet`. Clearing a chain B, `off` and **Inherit** need no check.

## What you see

On the repository page, one line shows the panel:

| Line | Meaning |
| --- | --- |
| `Panel: on (A: <entry>, B: <entry>)` | A chain B is in effect. |
| `Panel: off` | No panel. |

The API is in the [OpenAPI](../../openapi.yaml) file: `/panel-chain`, `/panel-chain/reset` and `/panel-chain/off` on a repository, `/api/teams/{id}/panel-chain` and `/api/admin/panel-chain`.
