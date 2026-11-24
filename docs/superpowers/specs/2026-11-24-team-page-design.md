# Team page

Status: draft, 2026-11-24. pergent.sh only.

## Approaches

| | What | Picked |
|---|---|---|
| A | One tabbed team page on today's API, plus read routes | Yes |
| B | A, plus small write routes per member and per repository, so 2 admins can't overwrite each other | No: today's whole-team `PUT` works; B can come later |
| C | **+ Add repository** also sets the owner team | Yes, with A |

## Why

A team is hard to see and hard to set up today:

- A team has 2 pages. `/teams/:id` shows Models, Limits and the Team chain on one page. `/admin/teams` is admin-only. Its **Edit** opens a form inside the team's card that lists every switched-on repository of every instance as a checkbox, with an `owner` box.
- `/teams` lists each team's repositories in one long line.
- A team has no usage view. Usage exists only for admins: per repository and on the admin Usage page.
- A repository is onboarded on 2 pages that do not link. An admin switches it on with **+ Add repository**. Later, they find the team on `/admin/teams` and tick the repository and `owner`. A new repository has no owner team until someone remembers the second step.

The repository page already has tabs. A team gets the same shape.

## Goals

- One team page, `/teams/:id`, with tabs, like the repository page.
- The Repositories tab lists only the team's own entries, and adds a repository with a search, not a list of every repository.
- A team's usage, on its own tab.
- A repository can be switched on and given its owner team in one step.

## Non-goals

- No new write routes for members or repositories (approach B). The tabs save through today's `PUT /api/teams/{id}`.
- No change to who can do what. Admins change members, repositories and owners, as today. Maintainers change the team's models, chain and own limits, as today.
- No change to how the owner team picks models, keys or the chain.

## Anti-goals

- A maintainer or viewer can change a team's members, repositories or owner ticks.
- A team's page or usage shows another team's numbers, models or members.
- Adding a repository from one tab drops a member or a repository that the other tab did not touch.

## The team page

`/teams/:id`. Team members and admins see it. Anyone else gets 404, as today. The tab is in the address (`?tab=`), as on the repository page. The default tab is Repositories.

Header: the team name, `Your role: <role>`, and for an admin, **Rename** and **Remove team** (moved from `/admin/teams`).

| Tab | Who sees it | What it holds | Who changes it |
|---|---|---|---|
| Repositories | Members, admins | The team's entries: a repository, or `All repositories of <instance>`. Each row has `owner` or not, `paused` or `removed` as today, and a link to the repository page. | Admins |
| Members | Members, admins | Each member's username and role. | Admins: role, remove, **Add member** (a search over existing accounts, replacing today's checkbox list) and **Add by email** (moved from the admin editor) |
| Models | Members, admins | Today's Models card, Team chain and **Add model**. | Maintainers, admins, as today |
| Limits | Maintainers, admins | Today's **Model limits** card. | As today |
| Usage | Maintainers, admins | New, below. | Nobody |

### Repositories tab

- An admin's **Add repository** opens a search over switched-on repositories (`?search=`, paged), plus `All repositories of <instance>` for each instance. It never lists every repository at once. Today the admin editor builds its list in the browser from `GET /api/repositories` and `/api/admin/instances`; the search is a new server query (`repository-choices`, below).
- Each result has an `owner` box. A repository whose owner at the same level is another team shows `owned by <team>`, and its `owner` box is disabled. The server's 409 for that case stays.
- Each row has an `owner` toggle and **Remove** for an admin.
- An entry already on the team is not offered again.

### Saving

- Every change on the Repositories and Members tabs sends `PUT /api/teams/{id}` with the team as the page last read it, plus that one change. The other tab's data is sent as read, so nothing is dropped.
- A repository entry that is no longer switched on, or a whole-instance entry whose instance is being removed, is left out of the save, as today's editor does (`PUT` answers 400 `msgRepoNotOn` for it). The tab shows it with `(removed)` and a note that the next save drops it.
- Two admins editing one team at the same time: the last save wins, as today. A later change can add a version check (approach B).

### Usage tab

- New route `GET /api/teams/{id}/usage?days=`: the team's maintainers and admins. A viewer of the team gets 403, as on `/limits`, and anyone else 404. This keeps who sees usage the same: the owner chose it on 2026-11-24.
- It counts the `run_usage` rows whose `owner_team_id` is this team, in the last `days` days, split per model the way `splitUsageSQL` (`internal/store/usage.go`) does: a run with `run_usage_by_model` rows counts from those, each on its model, and takes its team from the joined `run_usage` row, as `entryTokensSQL` (`internal/store/limits.go`) does. A multi review is never counted twice. `run_usage_models` (the old panel table) is not read. The days choices are the ones the usage pages use today.
- It returns the total tokens and reviews, each UTC day, the repositories busiest first, and the models busiest first. Tokens are input plus output; cache and reasoning are shown apart, as on the other usage pages.
- A row counts on the team it had when it was written, as for team limits. A repository that moved to another team keeps its old rows on the old team.
- `owner_team_id` was first filled by migration `0051` (v0.19.0). Older rows have no team, so the tab says `Counts reviews since v0.19.0` when the range starts before the oldest `run_usage` row with any owner team (in the whole instance, not this team's: a team made after the upgrade gets no note).

## `/teams` and `/admin/teams`

- `/teams` keeps one card per team, but shows counts of the team's entries as written: `9 repositories, 1 whole instance, owner of 9` and `4 members`. A whole-instance entry counts as 1, not as the repositories it covers. It no longer lists each repository. The name links to the team page.
- `/admin/teams` keeps **Create team** and a list of every team: name, member count, repository count, a link to its page. The inline **Edit** form goes away. A new team opens its page on the Members tab.

## One-step onboarding (C)

- The **+ Add repository** picker gets an optional **Owner team** select: `None` (default) and every team.
- `POST /api/repositories` takes an optional `ownerTeam` (a team id). It switches the repository on and adds it to that team as owner, in one transaction. An unknown team is 400. The owner change is audited as `owner_set`, as from the team page.
- A paused repository is resumed by the same call and keeps its team rows. If it already has that owner team, nothing more changes. If another team owns it at the repository level, the call is 409 and the whole transaction rolls back: the repository stays paused.
- If an instance entry already has an owner team, the select shows `Owner today: <team> (all repositories of <instance>)` as a hint. A repository's own entry wins over that, as today.
- The repository page shows `Owner team: <team>` under its name, linked to the team page, when it has one.

## API

| Route | Who | Change |
|---|---|---|
| `GET /api/teams/{id}` | Team members, admins | New: the team's name, members, entries, the caller's role and `canEdit` (admin). |
| `GET /api/teams/{id}/usage` | The team's maintainers, admins | New, above. |
| `GET /api/teams/{id}/repository-choices?search=&page=` | Admins | New: the search behind **Add repository**, each result with its owner team at the same level. |
| `POST /api/repositories` | Admins | New optional `ownerTeam`. |
| `GET /api/me/teams` | Session | Adds `memberCount`, `repoCount` and `ownedCount`. |

All in `openapi.yaml`. `PUT /api/teams/{id}` does not change.

## Docs

`concepts/dashboard.md` (the team page and its tabs), `concepts/usage.md` (the team usage tab and the v0.19.0 start), `concepts/models.md` (where a team's models are now), `docs/upgrading.md` (`/admin/teams` loses its editor, the new routes, `POST /api/repositories` takes `ownerTeam`).

## Tests

- Web: the tabs each role sees; the Repositories search shows no full list and disables another team's owner box; a change sends the untouched tab's data as read; `/teams` shows counts; the **Owner team** select sends `ownerTeam`.
- Dashboard: `GET /api/teams/{id}` is 404 for a non-member and 200 for a member and an admin; `/usage` is 404 for a non-member, 403 for a viewer, 200 for a maintainer and an admin; usage counts only the team's rows; `repository-choices` searches and pages; `POST /api/repositories` with `ownerTeam` sets the owner in the same transaction, 400 for an unknown team, audit entry.
- One DB test per query, with table subtests.

## Calls I made (check these)

- Removed entries are dropped on the next save, with a note, as today.
- `/teams` counts entries as written; a whole instance counts as 1.
- The `since v0.19.0` note uses the oldest team row in the instance.
- A paused repository with another owner team makes `ownerTeam` 409 and rolls back.
- Members keep both ways to add: a search over existing accounts and **Add by email**.
- The repository page shows its owner team.

## Calls left to the builder

- The exact layout of each tab, following the repository page.
- Whether the Usage tab reuses `usageParts.tsx` pieces as they are.
