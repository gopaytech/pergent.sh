# Owner teams on the lists, and filters by team

Status: approved design, 2026-11-26.

## Why

The Repositories list does not say which team owns a repository, so an admin
can't see who to ask about one, or which repositories nobody owns. With many
repositories the list is long and has no search, no filter and no sort. The
Teams pages draw each team as a large card that holds 3 small facts. The
Reviews page can narrow to one repository, but not to the repositories of a
team.

## Goals

- Show the owner team of each repository on the Repositories list, and mark
  a repository with no owner so it stands out.
- Search the Repositories list by name, filter it by owner team, and sort it
  by name, Reviews (7 days), Approved and Failed.
- Show the Teams pages (`/teams` and `/admin/teams`) as one compact table
  each, with a search and a sort.
- Filter the Reviews page by owner team.

## Non-goals

- Filtering by a team that covers a repository without owning it. Every team
  filter here means **owner team only**.
- Paging, search or sort on the server for the Repositories list. It loads
  whole today, and is fast.
- A team filter on the Overview and Usage pages. It can come later, once the
  new filters are in use.
- Changing the Health of a repository with no owner. No owner does not stop
  reviews, so it is not a setup problem.

## Anti-goals

- A filter must never widen what a session can read. Picking a team only
  narrows the session's own visible repositories.
- The list, the repository page and the Reviews filter must never disagree
  on who owns a repository.

## Approaches

| | Approach | Trade-off | Verdict |
|---|---|---|---|
| A | Add `ownerTeam` to each listed repository (one batch query). Search, filter and sort the Repositories list in the browser. Filter Reviews by team on the server. | One small server change. The owner rule stays in one SQL expression. | **Chosen** |
| B | No server change. Match owners in the browser from `GET /api/teams`. | Copies the owner rule into TypeScript, where it can drift. A non-admin can't see every team, so some owners would wrongly read as none. | Rejected |
| C | Search, filter, sort and paging of the Repositories list on the server. | Query parameters, SQL and tests with no gain while the list loads whole. Can come later with the same addresses. | Not now |

## Design

### 1. Server: the owner team on each listed repository

- `RepositoryView` gets `ownerTeam: OwnerTeamView | null`, the same
  `{id, name, open}` the repository page uses. `open` is true for an admin or
  a member of the team. The repository page already shows this name to anyone
  who can read the repository, so nothing new is revealed.
- New `Store.OwnerTeams(ctx, keys)` reads the owners of many repositories in
  one query, reusing `ownerTeamSQL` as it is (the repository's own owner entry
  wins, matched ignoring case, else the instance entry's owner):

  ```sql
  SELECT r.instance, r.project, <ownerTeamSQL('r.instance', 'r.project')>
  FROM unnest($1::text[], $2::text[]) AS r(instance, project)
  ```

- Team names come from one read of the teams, not one per repository.
- `repositoryViews` fills the field. It also serves the repository page, so
  `detail.repo.ownerTeam` and `detail.ownerTeam` hold the same value;
  `detail.ownerTeam` stays for API clients.
- openapi.yaml documents the field.

### 2. Repositories page

```
Repositories                                          [+ Add repository]
⚠ 4 repositories have no owner. Show them
[Search by name…      ]  [All teams ▾]                    12 of 40 shown
Repository ▲ | Owner team | Platform | Review | Reviews | Approved | Failed | Health
```

- **Owner team column**, after Repository, shown to everyone who can open
  the page: the team name, linked to the team page when `open`; else plain
  text. An owner through a whole-instance entry shows the same name, with the
  hover text "Owns every repository on <instance>".
- **No owner:** for an admin, an amber "⚠ No owner" (the words go with the
  icon) linked to the repository's Settings tab, where the Teams card sets
  the owner. For anyone else, a grey "None": they can't set an owner, so a
  warning would be noise.
- **No-owner line** above the table, for admins only: "N repositories have
  no owner. Show them". N counts repositories being reviewed now, not paused
  ones. The link sets the team filter to No owner, which lists paused ones
  too. The line is hidden when N is 0, and while the filter is already No
  owner.
- **Search** (`?q=`): any part of the path only (not the team name, which
  has its own filter), ignoring case, written to the
  address a moment after typing stops. The hook moves out of `AuditLog.tsx`
  into a shared file.
- **Team filter** (`?team=<id>` or `?team=none`): All teams, No owner, then
  the teams that own something in the list, by name. No extra request, so a
  non-admin sees only the teams in their own list.
- **Sort** (`?sort=name|reviews|approved|failed`, with `-desc`): the
  Repository, Reviews, Approved and Failed headers are buttons with
  `aria-sort`. The first click on a number puts the highest first; a second
  click reverses. Ties sort by name. Default: name, A to Z.
- "12 of 40 shown" shows only while a search or filter is on. No match says
  "No repository matches." with a Clear button.
- "+ Add repository" changes only `add` in the address, so it keeps the
  search, filter and sort.

### 3. Teams pages

`/teams`: Team · Your role · Repositories · Owner of · Members.
`/admin/teams`: Team · Repositories · Owner of · Members. Create a team
stays above the table. Remove leaves the table: the team's own page already
has it, with the same confirm prompt, and a red button on every row of a
dense table is easy to click by mistake.

- Numbers come from what the API already sends; no server change.
- A team with whole-instance entries shows "+N instance(s)" in small text
  under its Repositories number.
- Search by name (`?q=`) and sort by Team, Repositories, Owner of and
  Members, with the shared search hook and one shared sort (`useTableSort`
  and a `SortHeader` component) for all 3 tables.
- Empty states stay as they are. A search with no match says "No team
  matches." with a Clear button.
- `teamCountsText` and `membersText` go if nothing else uses them.

### 4. Reviews page: filter by team

- `GET /api/reviews?team=<id>` keeps the reviews of the session's visible
  repositories whose owner team is that team (`Store.OwnerTeams`), by
  narrowing `ReviewFilter.Visible` the same way `?project=` does.
  `?team=none` keeps those with no owner. Combined with `?project=`, both
  apply.
- A team id that is not a number, or no such team, is a 400. A team that owns
  nothing visible gives an empty list.
- A team dropdown before the repository dropdown: All teams, No owner, then
  the owning teams. Picking a team narrows the repository dropdown to its
  repositories and clears a repository choice outside it. Kept in the address
  as `?team=`.
- openapi.yaml documents the parameter.

## Tests

- Store: `OwnerTeams` for an own owner entry, an owner only through the
  instance entry, no owner, and a path that differs only in case.
- Dashboard: `GET /api/repositories` returns the right `ownerTeam` and
  `open`; `GET /api/reviews?team=` keeps only owned repositories, never shows
  a non-member more than before, and refuses a bad team.
- Web: Repositories search, filter (a team, No owner), each sort and its
  reverse, the address restoring the view, the no-owner line, the empty
  result, admin and non-admin views of No owner; both Teams tables (counts,
  the instance note, search, sort, no Remove in the table); the Reviews team dropdown and the narrowed repository
  dropdown.
- Focused test files locally; the full run on CI.

## Delivery

One PR from `main`: the shared search hook and sort serve every part, so
2 PRs would depend on each other. A `## Unreleased` note in docs/upgrading.md for the new
`ownerTeam` field and the `team` parameter.

## Left to the builder

- Markup and CSS of the toolbar and sort buttons, within the existing styles.
- Names of the shared hook and sort files.
