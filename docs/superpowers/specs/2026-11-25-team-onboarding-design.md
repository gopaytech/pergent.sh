# Teams onboard and claim their own repositories

Status: agreed with the owner on 2026-11-25 in a design session. The owner approves this document before the build starts.

## Why

Today only an admin can switch a repository on, pause it, remove it, or tick its owner team. Teams already add their own models and chains, and admins already set token limits per team. So the cost of a team's reviews is under control, and the admin is the only slow step left. The owner wants a team to onboard and run its own repositories.

## What other tools do

| Tool | Who adds a repository | Which repositories |
|---|---|---|
| SonarQube Cloud | Anyone an admin gives "Create Projects". The creator becomes the project admin. | Only repositories the app is installed on |
| GitHub Apps in general | The org owner picks the App's repositories | That list |
| Codecov | Visibility follows each person's own platform access | Checked with the person's token |

pergent.sh follows the first 2. The review bot's access (GitLab: the bot's memberships; GitHub: the App's installed repositories) is the scope, and an admin sets it on the platform. The Codecov way needs each account linked to a platform identity, and pergent.sh accounts have none.

## Goals

- A maintainer of a team onboards a repository the review bot reaches. Their team becomes its owner.
- A maintainer claims a switched-on repository that has no owner team.
- A maintainer of the owner team pauses, resumes, removes it, or releases ownership.
- An admin turns this on or off per instance.

## Non-goals

- No scope or path prefix per team. The bot's access is the scope.
- No request-and-approve flow.
- No platform identity for accounts.
- No change to what a team sees: the repositories its entries cover, as today.
- No change to the admin-only settings: re-run limits, trusted authors, linked repositories, commit status, the per-repository token quota.

## Anti-goals

- A team takes a repository from another team. Only an admin moves ownership.
- A viewer, or a user on no team, onboards or claims anything.
- A maintainer onboards on an instance where the switch is off.
- An action leaves no audit entry.

## Approaches

| | Approach | Trade-off | Picked |
|---|---|---|---|
| A | One picker. **+ Add repository** shows for maintainers too, and the team page has the same button with the team filled in. The routes admins use today accept a maintainer. | Reuses the picker and switch-on code. One flow for everyone. | Yes |
| B | A separate team area: new routes and page section under `/api/teams/{id}/repositories` | Clean split, but 2 pickers and 2 switch-on paths that drift | No |
| C | A maintainer requests, an admin approves | Safest, but keeps the admin in the loop, which is the work to remove | No |

## The rules

| Action | Admin | Maintainer | Viewer, no team |
|---|---|---|---|
| Turn "Teams may onboard repositories" on or off for an instance | yes | no | no |
| Onboard (switch on a repository the bot reaches, with an owner team) | yes, any team or none | only on an instance with the switch on, owner = a team they maintain. If the instance has an owner team, only its maintainers, and the repository inherits that owner | no |
| Claim (set the owner of a switched-on repository with **no owner at either level**) | yes | switch on, a team they maintain | no |
| Release the owner tick | yes | maintainer of the owner team, own entry only, switch on or off | no |
| Pause | yes | maintainer of the owner team (either level), switch on or off | no |
| Resume, remove | yes | maintainer of the owner team (either level), switch on | no |
| Move ownership between teams, untick another team's owner | yes | no | no |
| Repository teams card, team members, team repository list | yes | no | no |

Notes:
- **Owner at either level.** A repository is owned when it has its own owner entry or its instance has one (`ownerTeamSQL`). A claim is refused in both cases. A claim never overrides an instance owner.
- **Release** clears the owner tick on the repository's own entry and keeps the entry. The team still sees the repository. An instance-level owner is released only by an admin.
- **Maintainer of the owner team** means a maintainer of the team `OwnerTeam` returns, at the repository's own level or the instance level. It is not `Session.Role`, which is the highest role across every covering team. A maintainer of a team that covers the repository but does not own it gets 403. Anyone who can't see the repository gets 404.
- **Remove** works as an admin's remove does today (`RemoveDashboardRepo`): it deletes the repository's row, its own chains, and every team entry for that path, the owner tick included. Instance-level entries stay. After a remove, any maintainer may onboard it again.
- **Claim** sets the owner on a repository that is on **or paused** and has no owner. It creates the team's entry for the path when the team has none, as `SwitchOnOwned` does, and does not change paused or on. A repository that is not switched on is 404. A config-file repository is 409 for a maintainer (an admin may claim it).
- **Onboard** by a maintainer only switches on a repository that is off. A repository that is on or paused is 409 with `use Claim`. An admin's onboard keeps today's behavior: it resumes a paused repository.
- **Release and pause** are allowed with the switch off: both only reduce what a team holds or spends (owner's ruling, 2026-11-25).
- **Instance owner.** On an instance with an instance-level owner team, only maintainers of that team onboard. The repository gets no owner tick of its own and inherits the instance owner. A maintainer of another team gets 403 `owned by <team>` (owner's ruling, 2026-11-25).
- **The picker shows every repository the bot reaches** on the instance, to any maintainer, names and state only. Reviews, chains and usage stay hidden (owner's ruling, 2026-11-25).
- **Remove confirmation** lists the other teams that cover the repository and will lose it.
- **No limit on repository count per team.** The per-team token limit is the guard.
- **No notice to admins.** The audit log is the record.
- **The owner's name is shown.** The picker shows `owned by <team>` to a maintainer of any team, so they know whom to ask. This relaxes today's rule that a non-member learns nothing of a team, for the name only (owner's ruling, 2026-11-25). Members, models and usage stay hidden.
- **Config file.** A repository in the config file can't be onboarded, paused or removed in the dashboard, as today. Claiming one stays admin-only (owner's ruling, 2026-11-25).
- **A maintainer of 2 teams** picks the owner team. With one team, it is filled in.

## The switch

- New instance setting **"Teams may onboard repositories"**, default **off**. An upgrade changes nothing until an admin turns it on.
- A dashboard instance: a new column on `instances`, set on the instance form (`PUT /api/admin/instances/{name}`).
- A config-file instance: a new key `teams_may_onboard`, read-only in the dashboard like the other config instance fields.
- Turning it off stops new onboards and claims. Repositories teams onboarded stay, with their owners. Resume and remove for maintainers also stop. Pause and release still work.

## Server

| Route | Today | After |
|---|---|---|
| `GET /api/repositories/available?instance=` | admin | also a maintainer of any team, on an instance with the switch on |
| `POST /api/repositories` | admin | also a maintainer, with `ownerTeam` required and one of their teams |
| `PUT /api/admin/repositories/{instance}/{project}` (pause, resume) | admin | also a maintainer of the owner team |
| `DELETE /api/repositories/{instance}/{project}` | admin | also a maintainer of the owner team |
| `POST /api/repositories/{instance}/{project}/claim` `{"team": id}` | none | new |
| `POST /api/repositories/{instance}/{project}/release` | none | new |

- The admin-only routes move out of `adminRoute` into routes that check the session. The admin check stays the same for admins.
- The picker's `AvailableRepo` gets a state: `off`, `paused`, `on`, plus `ownerTeam` (name) when owned. The UI needs it for the row's button.
- Checks in this order: an unknown instance is 404; a user on no team, or a viewer, is 404, as today for admin routes; a maintainer on an instance with the switch off is 403 with a fixed message. A config-file instance with the key off is the same 403.
- A claim runs under the same owner lock as `UpdateTeam` (`ownersLockKey`), so 2 claims at once give 1 owner and one 409.
- The 409 `already has owner <team>` stays the message for a taken repository.
- `CanView` (or the repository view) gets `pause`, `remove`, `release` flags for the buttons.

## Audit log

Every action is logged with the account, as today:
- `repository_on`, `repository_paused`, `repository_resumed`, `repository_removed`
- the owner change, through `auditOwners`
- new: `repository_claimed`, `repository_released` (detail: the team name)
- the switch: `instance_updated` with `teams may onboard: on|off`

## Dashboard

- **Repositories page:** the **+ Add repository** button shows for maintainers on instances with the switch on. The instance list shows only those instances.
- **Picker rows:** `off` → **Onboard**. `on` or `paused` with no owner → **Claim**. `paused` and owned by their team → **Resume**. Owned by another team → `owned by <team>`, no button.
- **Owner team select:** the teams the person maintains. An admin sees every team and "none", as today.
- **Team page:** **+ Add repository** with the team filled in, for its maintainers.
- **Repository page:** **Pause**, **Resume**, **Remove**, **Release ownership** for maintainers of the owner team. Remove and Release ask for confirmation.
- **Instance form (admin):** the new switch, with the line "Maintainers of any team can switch on repositories the review bot reaches, and become their owner."

## Docs

- A new page `docs/concepts/roles.md`: the full role matrix (admin, maintainer, viewer), including this change.
- `dashboard.md`, `configuration.md` (the new key), `upgrading.md` (default off, nothing changes on upgrade).

## Tests

- The rules table as a table test over every route: admin, maintainer of the owner team, maintainer of another team, viewer, no team; switch on and off.
- A claim race: 2 claims at once give 1 owner and 1 409.
- A claim on a repository with an instance-level owner is refused.
- A remove by a maintainer deletes the owner tick, and another team can then onboard it.
- A maintainer's onboard of an on or paused repository is 409; a claim of a paused one keeps it paused.
- On an instance with an instance owner: its maintainer onboards with no own owner tick; another team's maintainer gets 403.
- Switch off: pause and release work, resume and remove are 403.
- UI: each picker state shows its button. A viewer sees no button.

## Left to the builder

- Migration number, column name, message wording.
- Whether pause, resume and remove share one handler change or several.
- How the moved routes register (a helper like `teamRoute`), and the updates to the route and OpenAPI scan tests.
