# Blocking status

Status: agreed with the owner on 2026-11-24, after a design session. pergent.sh only: pergent and the review Job do not change.

## Why

`failure_status` picks the `pergent` status of a review that could not be done: `failed` blocks the merge, `neutral` does not. A review that finished and found blocking issues is always `failed`, and so is a review held back by a blocking CI job. The setting's label, **Status when a review fails**, reads as if `neutral` covered these too, and it does not.

Some repositories want pergent to review and comment but never block the merge with its status: a team that is trying pergent out, or a repository whose merge gate is somewhere else. Today there is no way to do that.

This design adds `blocking_status`. With `neutral`, the `pergent` status is never red. pergent still never approves a blocking review.

## The two settings

| Setting | Values | Default |
|---|---|---|
| `failure_status` (exists) | `failed`, `neutral` | `failed` |
| `blocking_status` (new) | `failed`, `neutral` | `failed` |

The status a review gets:

| Case | `blocking_status: failed` | `blocking_status: neutral` |
|---|---|---|
| Clean review | `success` | `success` |
| Blocking findings | `failed` | `neutral` |
| A blocking CI job did not pass | `failed` | `neutral` |
| The review could not be done (failed skills, no model left, Job error) | `failure_status` | `neutral` |
| The publish gave up (`publish_failed`) | `failure_status`, `failed` if blocking | `neutral` |
| Superseded run | `canceled` | `canceled` |
| Waiting (CI, quota, limit) | `pending` | `pending` |

- **`blocking_status: neutral` means never red.** It also covers a review that could not be done, so `failure_status` only matters while `blocking_status` is `failed`. The owner chose this so the setting has one simple meaning.
- `neutral` is the existing platform mapping: `success` on GitLab (it has no neutral commit status), a `neutral` Check Run conclusion on GitHub.
- **Fail closed when the setting can't be read.** If the repository settings can't be read, the status is `failed`, as today. Only a read `neutral` turns a status neutral.
- `failed` stays the default, so nothing changes after the upgrade until someone sets `neutral`.

## What does not change

- **Approval.** A blocking review, a review held back by CI, and a review that could not be done are never approved. An earlier approval of the same head is still withdrawn. A repository that needs pergent's approval to merge is still blocked by that approval rule.
- **The comment.** It shows the same findings, the same CI line and the same failures. It gets no line about the setting: only the status description says the status was softened.
- **The dashboard outcome.** A run with blocking findings is still **Blocking**, and one held back by CI is still **Blocked by CI**. The run page shows the platform status that was set (`neutral`), as it does for `failure_status: neutral` today.
- **Other CI jobs.** A failed blocking CI job still fails the pipeline itself. Only pergent's own status stops repeating it.

## Status description

When `blocking_status: neutral` turns a status that would have been `failed` into `neutral`, the description says so, so nobody reads the green status as a pass:

- `Blocking issues found · not blocking merge`
- `CI blocks approval: cycode-scan failed · not blocking merge`
- `Review failed for 1 of 2 skills · not blocking merge`

The suffix ` · not blocking merge` is added after the description is built, inside the 140-character limit (`maxDescription`): the description before it is cut first, so the suffix is always shown whole. A status that `failure_status: neutral` turned neutral keeps today's text with no suffix.

## Where it is set

The same three places as `failure_status`:

1. The config file: `repositories[].blocking_status`, checked at load (`failed` or `neutral`, any other value is a config error). A repository in the config file is read-only in the dashboard, as today.
2. The admin **Global defaults** page: the default for repositories switched on in the dashboard, with **Use config value**, as `failure_status` has.
3. A repository's **Settings** tab: an override, shown as `default` or `overridden here`.

Storage: a new migration adds `blocking_status text CHECK (blocking_status IN ('failed', 'neutral'))` to `dashboard_repos` and `repo_defaults`. NULL means "use the default". It takes the next free number when the branch merges.

## Who changes it

- **Only admins change `failure_status` and `blocking_status`**, on the Global defaults page and on a repository's Settings tab. A maintainer who sends either key gets 403 with `only an admin changes the status settings`, as for the re-run caps.
- Maintainers still see both values on the Settings tab, as read-only fields that say an admin changes them.
- A maintainer's **Reset to defaults** leaves both keys, as it already leaves the re-run caps and the linked repositories.
- Each change is in the audit log. The audit detail names the key and the new value only.
- **Breaking:** maintainers lose the right to change `failure_status`. An override they set before the upgrade stays in force. This goes in `docs/upgrading.md`.

## Labels

| Key | Label | Help text |
|---|---|---|
| `failure_status` | **Status when a review can't run** | "neutral" doesn't block merging when a review can't be done. On GitLab the status is success; the review is still not approved. Not used when the blocking status is neutral. |
| `blocking_status` | **Status when a review finds blocking issues** | "neutral" never blocks merging: pergent's status is never red. The review still lists the blocking issues and is never approved. On GitLab the status is success. |

- While the effective `blocking_status` is `neutral`, the `failure_status` field stays and shows the note `Not used: the blocking status is neutral`. It is not disabled, so an admin can set it before turning `blocking_status` back to `failed`.
- The repository page shows, under its name next to `Multi review: off`, the line `Merge gate: off (blocking status is neutral)` while the effective value is `neutral`, to everyone who can see the page. The repository list does not show it.

## Code

- `config`: the `BlockingStatus` field on a repository and on the defaults, the default `failed`, the load check.
- `settings`: `KeyBlockingStatus`, read from the config, the defaults row and the repository row, with `Overridden` set as for `failure_status`.
- `policy.Decide(o, failure, blocking)`: after the CI block (which today forces `failed`), a `blocking_status: neutral` maps every `failed` status to `neutral` and adds the suffix. `Approve` and `Kind` do not change. This is the one place the normal publish picks the status.
- `engine/publish.go`: `publish` reads both settings and passes them. `gaveUpStatus` returns `neutral` when the read settings say `blocking_status: neutral`, before its other checks, and `failed` when they can't be read.
- `dashboard`: the `blockingStatus` key on the repository settings and Global defaults APIs (`"failed"`, `"neutral"` or `null`), the admin-only check for both keys, the reset that leaves them, `openapi.yaml`.
- `web`: the field on `RepositorySettings.tsx` (read-only for a maintainer, with `Only an admin changes this`) and `AdminSettings.tsx`, the new labels for both, the `Not used` note on `failure_status`, and the `Merge gate: off` line on the repository page.
- Docs: `concepts/approval.md` (the status table), `concepts/ci-jobs.md` (the CI block with `blocking_status: neutral`), `concepts/dashboard.md` (the settings list, the config key list), `setup/github.md` (the Check Run conclusions), `upgrading.md` (the new setting and the breaking change).

## Tests

- `policy`: one table, one row per case in the status table above, for both values, checking `Status`, `Approve`, `Kind` and the description (with the suffix, and the cut at 140 characters).
- `engine`: `gaveUpStatus` with `blocking_status: neutral` on a run with blocking findings gives `neutral`; with settings that can't be read, `failed`. One publish test that sets `neutral` on GitLab and GitHub fakes for a blocking review, and checks it is not approved.
- `settings` and `config`: the precedence (config, defaults, repository) and the load check.
- `dashboard`: a maintainer gets 403 on `failureStatus` and on `blockingStatus`, an admin 200; a maintainer's reset leaves both; the audit entry.
- `web`: the field is read-only for a maintainer and editable for an admin; the `Not used` note shows only while `blocking_status` is `neutral`; the repository page shows `Merge gate: off` only then.

## Decided questions (grilling, 2026-11-24)

1. Settings that can't be read give `failed`, even on a `neutral` repository.
2. A softened status gets the ` · not blocking merge` suffix.
3. The review comment does not change.
4. Maintainer overrides of `failure_status` made before the upgrade stay; the migration only adds columns.
5. Maintainers see both settings read-only.
6. Admins can set `blocking_status: neutral` as the global default.
7. The repository page shows `Merge gate: off`; the repository list does not.
8. `failure_status` keeps its field with a `Not used` note while `blocking_status` is `neutral`.

## Anti-goals

- A blocking review is never approved, whatever the setting.
- A status is never turned neutral when the setting could not be read.
- The description never hides that the review was blocking, failed or held by CI.
- No change for a repository that does not set `neutral`.
