# Design: `failure_status: neutral` that works on GitLab

## Why

A review that can't be done (the Job is stopped, the LLM provider is down, a skill fails) sets the `pergent` commit status to `failure_status`. The default, `failed`, blocks a merge request on a GitLab project with "Pipelines must succeed". `neutral` is meant to stop that, but on GitLab it doesn't work.

pergent.sh maps `neutral` to GitLab's `skipped`. By the time a review is published, its status is `running` (set when the Job starts). GitLab's commit status state machine (`app/models/commit_status.rb`, event `skip`) allows `skipped` only from `created`, `waiting_for_resource`, `preparing` and `pending`, so the API answers 400. The publish is retried 10 times over about 50 minutes, with the status left `running`, and then gives up with `failed` ("pergent couldn't post its review"). The tests didn't catch it because the fake GitLab accepts any change of state.

From `running`, GitLab allows `success`, `failed` and `canceled`.

## Goals

- On GitLab, a repository with `failure_status: neutral` never has its merge request blocked by a review that couldn't be done.
- `failure_status` means the same on both platforms, including when publishing gives up.

## Non-goals

- Changing the shipped default. It stays `failed`; an admin sets `neutral` in **Admin settings**.
- Investigating why reviews end "cancelled at step 1". That is separate work.
- Fixing merge requests already blocked. They unblock on a re-run or a new push.
- Teaching the fake GitLab the real transitions. Nothing pergent.sh sends after this change is refused.
- Any version bump, tag or release.

## Anti-goals

- A review that couldn't be done must never be approved, and its comment must still say it failed.
- Blocking findings must always be `failed`, whatever `failure_status` says.
- When pergent.sh can't tell what the repository wants, it must fail closed (`failed`).

## Approaches

1. **`neutral` is `success` on GitLab.** (Chosen.) One mapping change. `running` to `success` is allowed, so it works for runs already in flight at the upgrade. Cost: the pipeline shows a green job for a review that didn't happen; its description and the comment still say it failed, and there is no approval.
2. **Show a running review as `pending` on GitLab, keep `skipped`.** `pending` can become any final state. The job reads grey, which is more honest. Cost: a "pending" icon while a review runs, a held review (quota, untrusted author) keeps its hold description while it runs because GitLab refuses `pending` to `pending`, and runs in flight at the upgrade still fail. Not chosen: the owner preferred the simpler change.
3. **`neutral` is `canceled` on GitLab.** Allowed from `running`, but a canceled job still fails "Pipelines must succeed", so it doesn't solve the problem.

## What changes

- **GitLab mapping.** `gitlabforge.State` maps `forge.StatusNeutral` to `gitlab.StatusSuccess`. The comment above it says why `skipped` can't be used.
- **Description unchanged.** A review that couldn't be done keeps "Review failed for N of M skills" on both platforms.
- **Give-up follows `failure_status`.** `Engine.giveUp` reads the repository's settings the way `publish` does (`e.settings().Get`). With `neutral` it sets `forge.StatusNeutral` (GitLab `success`, GitHub Check Run `neutral`); otherwise `forge.StatusFailed`. If the settings can't be read, or the repository isn't found, it sets `failed`. The description stays "pergent couldn't post its review". The approval withdrawal before it is unchanged. API reviews are unchanged (they set no status).
- **Help text.** In `web/src/pages/RepositorySettings.tsx` and `web/src/pages/AdminSettings.tsx`, the Failure status help becomes: `"neutral" won't block merging when a review can't be done. On GitLab the status is success; the review is still not approved.` The 2026-10-31 dashboard mockup stays as it was.
- **Docs.**
  - `docs/reference/configuration.md`, `repositories[].failure_status`: `neutral` is `success` on GitLab, with the reason.
  - `docs/concepts/approval.md`: the status table row for `failure_status`, and the give-up paragraph (the final status follows `failure_status`, `failed` when the settings can't be read).
  - `docs/upgrading.md`, under `## Unreleased`: the behaviour change for repositories set to `neutral`, on GitLab and at give-up.

## Tests

- `gitlabforge`: `TestState` and `TestSetStatus` expect `success` for neutral.
- `engine`: publishing a failed review with `neutral` on GitLab sets `success` with "Review failed for 1 of 1 skills" and no approval (rename `TestPublishNeutralMapsToSkipped`).
- `engine`: give-up with `neutral` sets `success` on GitLab and a `neutral` Check Run on GitHub, with "pergent couldn't post its review"; give-up with `failed`, and give-up when the settings can't be read or the repository is gone, sets `failed`.
- Web: the help text, if a test pins it.

## Left to the builder

- Test names and where the give-up tests sit beside the existing ones.
- How to make the settings read fail in a test.
- Exact wording of the doc sentences, in the docs' plain style with no em dashes.
