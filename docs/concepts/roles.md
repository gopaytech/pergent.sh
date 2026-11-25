# Roles: who can do what

Four kinds of user use the dashboard. This page lists every action for each of them. Sign-in and teams are in [Dashboard](dashboard.md).

| Kind | Who |
| --- | --- |
| Admin | An account with the admin role. Needs no team. |
| Maintainer | A member whose role on a team is `maintainer`. |
| Viewer | A member whose role on a team is `viewer`. |
| No team | An SSO account on no team. It waits for an admin. |

Things to know before you read the tables:

- **The role is per repository.** A user on several teams has, on each repository, the highest role among the teams that cover it.
- **Owner team.** Some actions need a maintainer of the repository's **owner team**. That is the team that holds the owner tick, on the repository's own entry or on its instance. A maintainer of a team that only covers the repository does not count.
- **No team.** A user on no team gets 403 `an admin needs to add you to a team` on every session route, as always.
- **Not allowed.** A viewer gets 404 on the routes that need a maintainer to see them at all, and 403 on a write to a repository they see. Anyone who can't see a repository gets 404, as if it did not exist.

## Switching repositories on, off and between teams

These actions need the switch **Teams may onboard repositories** on the instance, except where the table says otherwise. The switch is off by default ([The switch](#the-switch)).

| Action | Route | Admin | Maintainer | Viewer, no team |
| --- | --- | --- | --- | --- |
| Turn the switch on or off | `PUT /api/admin/instances/{name}/teams-onboard` | yes | no | no |
| See the picker (every repository the bot reaches, with its state and owner team name) | `GET /api/repositories/available` | yes | yes, with the switch on | no |
| **Onboard**: switch on a repository that is off | `POST /api/repositories` | yes, with any team or none | with the switch on, owner = a team they maintain. The repository must be off: on or paused is 409 `the repository is already switched on: use Claim` | no |
| **Claim**: make a team the owner of a repository that is on or paused and has no owner | `POST /api/repositories/{instance}/{project}/claim` | yes, any team | with the switch on, a team they maintain | no |
| **Release**: clear the owner tick on the team's own entry | `POST /api/repositories/{instance}/{project}/release` | yes | maintainer of the owner team, switch on or off | no |
| **Pause** | `PUT /api/admin/repositories/{instance}/{project}` with `{"enabled": false}` | yes | maintainer of the owner team, switch on or off | no |
| **Resume** | `PUT /api/admin/repositories/{instance}/{project}` with `{"enabled": true}` | yes | maintainer of the owner team, switch on | no |
| **Remove** | `DELETE /api/repositories/{instance}/{project}` | yes | maintainer of the owner team, switch on | no |
| Move ownership between teams, clear another team's owner tick | `PUT /api/teams/{id}` | yes | no | no |

Rules behind the table:

| Rule | What happens |
| --- | --- |
| Switch off | A maintainer's onboard, claim, resume and remove are 403 `teams may not onboard repositories on this instance`. Pause and release still work: they only reduce what a team holds or spends. |
| No team | A user on no team gets the waiting 403. A viewer gets 404. |
| Instance being removed | A maintainer gets 404. |
| Team is not the owner | A maintainer of a team that only covers the repository gets 403 `only maintainers of the owner team and admins can do this`. |
| Instance owner | When a team owns the whole instance, only its maintainers onboard. The repository gets no owner tick of its own and inherits the instance owner. A maintainer of another team gets 403 `owned by <team>`. Only an admin releases an instance-level owner. |
| Taken repository | A claim of a repository that has an owner at either level is 409 `<repository> already has owner <team>`. This includes a claim by the team that owns it. A claim never overrides an instance owner. |
| Config-file repository | Pause, resume and remove are 409: the config file manages it. Release is 409 too. An admin may claim it, a maintainer may not (409). |
| Remove | Deletes the repository's row, its own chains and every team entry for its path, the owner tick included. Entries that cover the whole instance stay. After a remove, any maintainer may onboard it again. |
| Remove confirmation | Lists the teams with their own entry for the repository, which lose it. A team that covers the whole instance keeps access and is not listed. |
| Onboard by an admin | As before: it resumes a paused repository. |
| Picker | A maintainer sees the state and the owner team's name, as `owned by <team>`. Reviews, chains, members and usage stay hidden. |
| Count | No limit on repositories per team. The team's token limit is the guard. |

Every one of these writes an [audit](dashboard.md#audit-log) entry: `repository_on`, `repository_paused`, `repository_resumed`, `repository_removed`, `repository_claimed`, `repository_released` (detail: the team name), and the owner changes (`owner_set`, `owner_cleared`). The switch is audited as `instance_updated` with `teams_may_onboard=on` or `off`.

## What a repository's settings let each role do

| Action | Admin | Maintainer | Viewer | No team |
| --- | --- | --- | --- | --- |
| Read reviews, runs, findings, files, logs and settings of the repositories a team covers (`GET /api/reviews`, `GET /api/runs/{id}`, `GET /api/repositories/{instance}/{project}`) | yes, all | yes | yes | no |
| Re-run a review, dismiss a finding, restore it (`POST /api/reviews/{instance}/{project}/{number}/rerun`, `.../dismiss`, `.../restore`) | yes | yes | no, 403 | no |
| Change settings and reset them (`PUT /api/repositories/{instance}/{project}/settings`, `POST .../settings/reset`) | yes | yes, except the re-run caps, the two status settings, linked repositories and the token quota. A reset leaves those | no, 403 | no |
| Save a repository's model chain (`PUT /api/repositories/{instance}/{project}/chain`, `POST .../chain/reset`) | yes | maintainer of the owner team | no | no |
| Save a repository's Reviewers card (`PUT /api/repositories/{instance}/{project}/reviewers`) | yes | maintainer of the owner team | no | no |
| Turn the code graph on or off (`PUT /api/repositories/{instance}/{project}/graph`), scan now (`POST .../readiness/scan`) | yes | yes | no, 403 | no |
| Set the teams that list a repository (`PUT /api/repositories/{instance}/{project}/teams`) | yes | no, 404 | no, 404 | no |
| Change a team's models, keys, chain and marks (`/api/teams/{id}/models`, `/api/teams/{id}/chain`) | yes | maintainer of that team | no, 403 | no |
| Read the skill library and rule sets (`GET /api/skills`, `GET /api/rule-sets`) | yes | yes | yes | no |

## Admin only

A maintainer gets 404 on all of these.

| Area | Routes |
| --- | --- |
| Teams: create, rename, members, repositories, owner ticks, remove | `GET /api/teams`, `POST /api/teams`, `PUT /api/teams/{id}`, `DELETE /api/teams/{id}` |
| Repositories to add to a team | `GET /api/teams/{id}/repository-choices` |
| Users and local admins | `GET /api/users`, `DELETE /api/users/{id}`, `POST /api/admin/users`, `PUT /api/admin/users/{id}/role`, `POST /api/admin/accounts` |
| Instances and secrets | `/api/admin/instances`, `/api/admin/instances/{name}`, `.../trusted-authors`, `.../teams-onboard`, `.../token-quota`, `.../repositories` |
| Global models and the global chain | `/api/admin/models`, `/api/admin/chain` |
| Defaults and settings | `/api/admin/defaults`, `/api/admin/settings`, `/api/admin/comment-footer` |
| Skill library and rule sets: write, install, copy, delete | `/api/admin/skills`, `/api/admin/skill-fetches`, `/api/admin/rule-sets` |
| Sign-in | `/api/admin/sign-in` |
| API clients | `/api/admin/api-clients`, `/api/admin/api-reviews` |
| Audit log and usage | `GET /api/audit`, `/api/admin/usage` |

Teams that may onboard repositories do not change these admin-only repository settings: the re-run caps, trusted authors, linked repositories, the commit status settings and the per-repository token quota.

## The switch

| | |
| --- | --- |
| Name | **Teams may onboard repositories**, on the instance form (**Integrations**) |
| Default | Off. An upgrade changes nothing until an admin turns it on. |
| Config-file instance | `teams_may_onboard` in the config ([Config reference](../reference/configuration.md)). It shows read-only in the dashboard, and `PUT /api/admin/instances/{name}/teams-onboard` is 409. |
| Turning it off | Stops new onboards and claims, and stops resume and remove for maintainers. Repositories teams onboarded stay, with their owners. Pause and release still work. |
| In the session | `GET /api/me` has `onboardInstances`: the instances where the session may onboard. An admin gets every instance not being removed. A maintainer gets those with the switch on. |

The review bot's access is the scope: on GitLab the projects where the bot is a Maintainer, on GitHub the repositories the App is installed on. An admin sets that on the platform. A team can onboard only what the bot reaches.
