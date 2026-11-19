# Pause a repository instead of removing it

Date: 2026-11-18. Size: small change.

## Why

Today the switch on `/repos` deletes the repository's `dashboard_repos` row. The repository leaves the list, all its
settings are lost, and the only way back is **+ Add repository**, which starts from the defaults. The owner wants the
switch to be a pause: the repository stays in the list, the switch shows off, and turning it on again restores it as it
was. A separate **Remove** deletes it for good.

## Behaviour

| Action | Who | Where | Confirm | What happens |
|---|---|---|---|---|
| Pause (switch off) | admin | list switch | none | Row stays with `enabled = false`, every setting kept. Reconcile removes the GitLab webhook and deploy token and clears their fields on the `projects` row, but **keeps the `projects` row** and its dismissals, commands, inline threads and finding fates (no `DeleteProject`). Readiness scans and setup problems are deleted, as today. |
| Resume (switch on) | admin | list switch | none | `enabled = true`. Reconcile reuses the kept `projects` row, creates a new webhook and deploy token, and a fresh readiness scan is queued. Settings, dismissals and review history are the ones from before the pause. |
| Remove | admin | repository page only | plain confirm | Row, settings and team links deleted. Webhook and token removed, and `DeleteProject` runs as today. Works on a paused or running repository. |

Remove confirm text: "Remove X from pergent? Its settings and team links are deleted, and its webhook and clone
credentials are removed. This can't be undone."

### While paused

- The list keeps the repository in its normal place, switch off, with a `Paused` pill in the Health column instead of
  OK or Needs attention. It does not count toward "needs attention" on the Overview page.
- Everyone who could see it while it ran still sees it.
- Settings stay editable, with the same roles as when it runs. The repository page shows a `Paused` note at the top.
- Nothing starts a Job or calls the platform for it. Such actions (for example readiness "Scan now") return 409 with
  `the repository is paused`, and their buttons are hidden.
- A late webhook is ignored and a command is dropped, as for a repository that is not switched on.
- The bot's approvals are not withdrawn on pause, and no note is posted on MRs. The 5-minute approval sweep still runs
  for every repository, paused or not: it withdraws an approval when the MR is closed or its head or target moves.
  Only an approval on the exact reviewed head stays, and that review is still valid.
- The **+ Add repository** picker hides it. `POST /api/repositories` on a paused repository resumes it and keeps its
  settings.
- Resume is refused (409) while its instance is being removed, and the switch is drawn disabled.

### Rules that stay

- A config-managed repository has no switch and no Remove.
- If the config later lists a paused repository, the config wins and the paused row is deleted (`dropTakenOver`).
- A run that finishes after a pause is posted with approval off, as today.

### Wording

- Usage and Teams pages show `paused` (linked to the repository page) or `removed` (gone). Usage keeps the token
  history of a removed repository.
- Team links left by earlier switch-offs stay and show as `removed`. An admin can untick them. No cleanup code.

## Approaches

| | Approach | Trade-off |
|---|---|---|
| **A (picked)** | `enabled` column on `dashboard_repos` | One row, one schema, new settings need no extra step. Risk: a query forgets the filter and reviews a paused repository. |
| B | Move the row to a `dashboard_repos_off` table and back | Readers do not change, but every new setting column must be added twice or resume loses it. |
| C | Delete as today, save settings as JSON in `parked_repos` | No schema to keep in step, but the copy goes stale (a deleted skill or model) and resume is a second code path. |

The owner picked A: one source of truth for settings, and the risk is limited to 8 known queries in `internal/store`.

## Design

- Migration `0042`: `ALTER TABLE dashboard_repos ADD COLUMN enabled boolean NOT NULL DEFAULT true`. Existing rows stay on.
- **Safe by default:** `Store.DashboardRepos` and `Store.DashboardRepo` keep their meaning, "switched on", and filter
  `enabled`. So `settings.Resolver.All/Get`, reconcile, dispatch, publish and commands do not change, and a paused
  repository looks to them like one that is not switched on. `UnscannedProjects` filters `enabled` too.
- A new store read returns paused rows as well. The repository list, the repository page, settings edits, resume, the
  picker's "already added" check, `Auth.load` (so team members still see a paused repository) and `dropTakenOver` (so
  the config can take over a paused row) use it.
- Reconcile on a paused repository removes the hook and deploy token and clears those fields on the `projects` row,
  without `DeleteProject`. Only a repository with no `dashboard_repos` row at all (Remove, or never added) goes
  through `DeleteProject`.
- `DeleteLibrarySkill` and `DeleteLLMModel` still count a paused repository as in use (409), so resume never fails on a
  missing skill or model.
- API: `DELETE /api/repositories/{instance}/{project}` becomes Remove and also deletes the repository's `team_repos`
  rows. A new admin-only endpoint sets `enabled` (404 for a non-admin, like every route added through `adminRoute`,
  and `x-pergent-access: admin` in `openapi.yaml`).
- The repository view carries a paused state, so the list, the page and the Usage and Teams pages can show it.
- Audit: pause, resume and remove each write their own entry.
- The behaviour docs in `docs/concepts/` (switching on, off and removing), one line in README, and `docs/upgrading.md`
  under `## Unreleased` change in the same commit.
  The upgrade note says `DELETE` now also deletes team links, and the switch uses the new endpoint. No release.

## Tests that prove it

- Each "switched on" reader skips a paused row: a webhook for a paused repository starts no run, reconcile removes the
  webhook and token and drops scans, readiness does not scan it, a command is dropped.
- Pause then resume keeps every setting, queues a fresh scan and creates the webhook again.
- Settings edits on a paused repository save and set up nothing on the platform.
- Job-starting actions return 409 while paused. Resume returns 409 while the instance is being removed.
- Remove deletes the row and its team links, on a paused and on a running repository.
- The config taking over a paused repository deletes the paused row.
- Skill and model delete return 409 for a paused repository that names them.
- The picker does not list a paused repository. `POST /api/repositories` on one resumes it with its settings.
- `openapi_test.go` covers the new route and its admin gate. Vitest covers the switch both ways with no confirm, the
  `Paused` pill, the Remove confirm, and the `paused` / `removed` wording.

## Dropped

- Withdraw the bot's approvals on pause and on Remove. The approval sweep (`SweepApprovals`) already covers it: it
  reads every row in `merge_requests`, which pause and Remove both keep, and it uses the approve bot, not the deploy
  token. The owner dropped the follow-up on 2026-11-19.

## Left to the builder

- The new endpoint's path and body, the new store function's name, and the audit action names.
