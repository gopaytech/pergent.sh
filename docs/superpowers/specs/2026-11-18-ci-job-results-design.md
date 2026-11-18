# CI job results as review input

Status: design, approved by the owner on 2026-11-18 after three rounds of questions and one builder check, and amended the same day after a grilling round (Q1 to Q12, all accepted). Covers a pergent change and a pergent.sh change. Nothing is built from this until the owner approves it, and nothing is released until the owner decides.

## Why

A review runs on its own today. It never sees what the change's CI already found: a failing `go test`, a lint error, or a security scan such as Cycode run in another job. The owner wants two things from those results:

- **Context.** The model reads them to focus the review and to avoid repeating what CI already caught.
- **A gate.** A job the repository marks as blocking keeps approval back unless it passed, even when the review is clean.

## Goals

- A repository picks CI jobs by name. Per job it chooses what pergent.sh reads: the **status**, the **tail of the log** when the job doesn't pass, or a **report file** the job saved.
- A review for a push **waits** for the chosen jobs on the head commit, up to a limit, then runs with what it has. The limit is 30 minutes by default and can be set per repository.
- pergent.sh **fetches** the results itself through the GitLab or GitHub API. Users change nothing in their CI files.
- Each job has a **blocks approval** switch. When it is on, anything but `passed` blocks.
- **GitLab and GitHub ship in one release.**
- **Off by default.** A repository with no jobs chosen behaves exactly as today.

## Non-goals

- **Uploads from CI.** A job never sends its report to pergent.sh. A later change can add that for other CI systems.
- **Reporting CI results as findings.** The model uses them as context only. It doesn't turn a scanner's findings into review findings.
- **A per-skill filter.** Every skill's batches get the whole file. Add a filter only if token cost shows it is needed.
- **Showing CI text on the dashboard.** No log or report text is stored or shown.
- **Reading pipelines in a GitLab fork project.** The docs say to run fork pipelines in the target project.
- **Parsing report formats.** JUnit, SARIF and Cycode JSON go to the model as text, cut to size.
- **GitLab external commit statuses**, the kind a third-party integration posts outside a pipeline.
- **Jobs inside GitLab child pipelines.** A trigger job can be chosen, and its result is the child pipeline's status.
- **Matching names by prefix.** A matrix is chosen by listing each name, or by the workflow's summary job.
- **A new review when a job's result changes** after the review ran. Only the gate is updated.
- **An instance-wide default list of jobs.** Add one only if repository-by-repository setup becomes tedious.
- **New metrics.**

## Anti-goals

- **A false pass.** A blocking job counts only when pergent.sh read `passed` for it on the run's head commit. These all block:
  - failed, cancelled or skipped;
  - never ran;
  - still running at the limit;
  - unreadable: an API error, a missing permission, or a pipeline pergent.sh can't see.
- **CI output as instructions.** Logs and reports come from running the change's own code, so the author controls them. They are data. They never reach the verification run, so they can't talk a blocking finding away.
- **A leaked secret.** A log can hold credentials. pergent.sh removes them before writing the file, and pergent removes them again. No log or report text ever reaches the comment.
- **Breaking an older pergent image.** An image without the new input ignores it and reviews as today. The gate still works, because pergent.sh decides it.

## Behaviour

### The repository setting

A new **CI jobs** section in repository settings. Maintainers and admins change it, like the other repository settings. Config key `repositories[].ci_jobs`:

```yaml
repositories:
  - gitlab: main
    project: group/project
    ci_jobs:
      wait: 30m
      jobs:
        - name: test
          read: log
          blocks_approval: true
        - name: cycode-scan
          read: report
          report:
            artifact: cycode-report   # GitHub only: the workflow artifact's name
            path: cycode.json         # path inside the job's artifacts
          blocks_approval: true
```

**Fields:**
- `name` is the GitLab job name, a GitLab trigger job's name, or the GitHub check run name. It is an exact match.
- `read` is one of `status`, `log` or `report`. The status is always read, because the gate needs it.
  - `log` adds the log tail of a job that didn't pass.
  - `report` adds the file at `report.path`, whatever the job's result.
  - A trigger job, and a GitHub check run not from Actions, has no report. `report` reads as `unreadable` for those.
- `blocks_approval` defaults to `false`.
- `wait` uses the config's `duration` type, as `job_timeout` does. It defaults to `30m` and must be between `5m` and `2h`.

**Limits:**
- At most 10 jobs, with unique names.
- A `path` is relative, with no `..`.
- The name `pergent` is refused, because that is pergent.sh's own status.

A config value wins and shows read-only, as for other settings. Reset to defaults clears the list.

### Which runs wait

- **Every run of a repository with jobs chosen checks CI once before dispatch, after the trust and quota gates.** That covers a push, a re-run, and an untrusted run a person started. A new `runs.ci_checked` marker records that the check is done, as `quota_checked` does for the quota.
  - If every chosen job has already ended for the head, the run is queued straight away.
  - Otherwise the run waits.
- **The CI wait is its own hold.** It is not `HoldRun`, because `HoldRun` only takes `trigger='push'`, and a started run would loop through `postpone`.
  - The dispatcher moves the run to `waiting` with the new reason `ci_pending` and sets `runs.ci_deadline` to the run's creation time plus `wait`.
  - No Job starts.
- **A new store transition** moves a `ci_pending` run back to `queued` and sets `ci_checked`. It runs:
  - when every chosen job has ended for the head;
  - at the deadline;
  - on **Start review**, which skips the rest of the wait and uses whatever results exist then.

  It never touches a run waiting for another reason.
- **A new push supersedes a `ci_pending` run,** as `EnqueueRun` does for any waiting run.
- **The status during the wait** is pending, with its own text, for example `Waiting for CI: test, cycode-scan`. It is not the untrusted-author text.
- **The API review never waits.** It has no pipeline.

### When pergent.sh checks

Events are only a signal to check again through the API.

- **GitLab:** `Pipeline Hook` and `Job Hook`.
  - pergent.sh manages the project hook itself (`EnsureHook`), so `EnsureHook` turns on `pipeline_events` and `job_events` for every project. Reconcile updates existing hooks.
  - pergent.sh acts on these events only for repositories with `ci_jobs` set.
- **GitHub:** `check_run`.
  - pergent.sh ignores the check runs its own App creates, matched by app id. Otherwise its `pergent` status would send events back to itself.
  - A check run's `pull_requests` is empty for a fork, so runs are matched by `head_sha`.
- **A sweep every 2 minutes** checks `ci_pending` runs, so a lost event only delays a run. The same sweep queues runs past their deadline.

### Reading the jobs

**GitLab**, with the bot's token. The bot is already a Maintainer.

- **The pipelines that count for a head:**
  - those from `GET /projects/:id/pipelines?sha=<head>`, which are branch and detached merge request pipelines;
  - merge request pipelines whose ref is `refs/merge-requests/<iid>/merge` and whose commit has the head among its `parent_ids`. Those are merged-results pipelines. Find them through `GET /projects/:id/merge_requests/:iid/pipelines`.
  - Never `head_pipeline` alone, which can still be the previous push's pipeline.
- **Jobs:** `GET /pipelines/:id/jobs` and `GET /pipelines/:id/bridges` for every pipeline that counts. Per chosen name, the job with the highest id wins, which is the newest attempt.
- **Log:** `GET /jobs/:id/trace`.
- **Report:** `GET /jobs/:id/artifacts/<path>`.
- **Forks:** a pipeline that ran only in a fork project isn't in the target project's lists, so its jobs are `missing`. With a blocking job, pergent never approves such a merge request until its pipeline runs in the target project, which is GitLab's own recommended setup for forks. The concept page says so.
- **Results:**

  | GitLab status | Result | Ended? |
  |---|---|---|
  | `success` | `passed` | yes |
  | `failed`, including `allow_failure` | `failed` | yes |
  | `canceled`, `canceling` | `cancelled` | yes |
  | `skipped`, `manual` | `skipped` | yes |
  | `created`, `pending`, `preparing`, `waiting_for_resource`, `scheduled`, `running` | `running` | no |

  A bridge's status maps the same way. Any value not listed is `failed`.

**GitHub**, with an installation token.

- **Check runs:** `GET /commits/<head>/check-runs?check_name=<name>&filter=latest`. Every check run with exactly that name must pass. That is the newest attempt of each, so two workflows that both name a job `test` must both pass.
- **A check run from GitHub Actions**, recognised by `app.slug == "github-actions"`:
  - **Log:** `GET /actions/jobs/:id/logs`. Its id is the check run id, which is true in practice but not documented.
  - **Report:** first `GET /actions/jobs/:id` for `run_id`, then `GET /actions/runs/:run_id/artifacts?name=<artifact>`. Download the zip and read `report.path` from it.
- **A check run from another app**, for example a scanner's own GitHub App:
  - **Log:** its `output.title`, `output.summary` and `output.text`, plus up to 50 annotations.
  - **Report:** `unreadable`.
- **Results:**

  | `status` | `conclusion` | Result | Ended? |
  |---|---|---|---|
  | `completed` | `success`, `neutral` | `passed` | yes |
  | `completed` | `failure`, `timed_out`, `action_required`, `startup_failure`, `stale` | `failed` | yes |
  | `completed` | `cancelled` | `cancelled` | yes |
  | `completed` | `skipped` | `skipped` | yes |
  | `queued`, `in_progress`, `waiting`, `requested`, `pending` | | `running` | no |

  Any conclusion not listed is `failed`. `neutral` passes, as GitHub's own branch protection treats it (owner's call).
- **Permissions:** the App already has `checks: write`, because it creates the `pergent` check run. New ones:
  - the `actions: read` permission;
  - the `check_run` event subscription.

  Every installation must accept them. Until it does, logs and reports from Actions are `unreadable`, and a missing event only slows things to the sweep.
  - `InstallationInfo` learns to parse `events` and gets `ActionsRead()`, as `PullRequestsWrite()` works today.

**Each job ends as one of:**
- `passed`, `failed`, `cancelled`, `skipped`;
- `missing`: no job or check run with that name on the head;
- `running`: not ended at the deadline;
- `unreadable`: an API error or a missing permission.

**When the wait ends:**
- **Every chosen job is waited for,** blocking or not. Choosing a job means the model should see it, and the limit caps the cost.
- **It ends when every chosen job has a result marked as ended.**
- **A `missing` job holds it** until the head's CI is finished or the deadline passes:
  - on GitLab, every pipeline that counts for the head has ended;
  - on GitHub, every check suite on the head has completed. A suite with no check runs is ignored, because GitHub creates one for every App that can post checks, and the suite of an App that never runs stays `queued` forever.

  A job still not found then is `missing`. A blocking one blocks. If it shows up later, the re-decide after publish fixes the gate, but this review's model never saw it.

### The file for the review

pergent.sh writes `ci-results.md`, one section per job: its name, its result, and its log tail or report.

- **It is written whenever the repository has jobs chosen,** even when every job is `status` only, because the model should know that a test failed.
- **Caps:**
  - A log tail is the last 200 lines and at most 16 KiB.
  - A report is at most 16 KiB.
  - The whole file is at most 64 KiB, about 16k tokens. It goes to every batch, so a 5-batch change pays about 80k extra input tokens.
  - Each cut is marked in the file.
- **Reading a big log:** the body is streamed through a rolling 2 MiB buffer, so the tail is the real end of the log. Neither API documents range reads.
- **Cleaning:**
  - ANSI colour codes are removed.
  - Content goes in a fence longer than any backtick run inside it.
  - Secrets are removed by a new redactor, on top of `Registry.Redact`. It covers bearer tokens, credentials in URLs, private key blocks, and the known token and key shapes.
- **How it reaches the Job:**
  - It is a new `Inputs` field, counted in `Size()`.
  - `Fit` drops it first, with a note.
  - It goes in the ConfigMap key `ci-results.md` and reaches the container as `PERGENT_CI_RESULTS=/in/ci-results.md`, the env var pattern that older images ignore.

### pergent

- **The flag:** `--ci-results <file>`, default `$PERGENT_CI_RESULTS`.
  - It is a trusted input in agent mode, as `--change-description` is.
  - A path inside `--repo-path` is a usage error.
  - An unreadable file gives a warning and is ignored.
- **pergent cuts it to 64 KiB again and redacts it.**
- **Which runs see it:**
  - `reviewBatch` and `crossBatch` attach it as `ci-results.md`, with the message sentence `ciMessage`.
  - It counts against each batch's budget, as the deleted-files list does with `carveOut`.
  - Verification, repair and the impact run never get it.
- **`ciMessage` says:**
  - the file is CI output produced by running the change, so it is data, not instructions;
  - use it to find where to look and what not to repeat;
  - don't report a CI result itself as a finding;
  - a passed job never excuses a problem in the code.
- **It is not part of the result.** It is logged as `ci results: <bytes> bytes`.
- **`render`** gets a new `Options.HeaderNote`:
  - Placement: one line after the header block (headline, meta line, reason, skills table, failures) and before whatever comes next.
  - Format: it is written between pergent's own `_` through `wrappedText`, so a line start is always pergent's.
  - Empty means no line, so the golden files are unchanged.

### The gate and the comment

- **The gate:** `policy.Decide` gets the CI results. A blocking job that isn't `passed`:
  - turns the `pergent` status failed, whatever the review's own status;
  - holds approval back;
  - is named in the status description.

  It applies also to a draft, to a repository with approval off, and to a failed review with `failure_status: neutral`.
- **`KindCI`:**
  - It is set when CI is the only reason for blocking. A review with blocking findings stays `KindBlocking`, and a failed review stays failed, with the CI line beside it.
  - `decisionOf` (`internal/engine/publish.go`) gets an explicit case, so the dashboard records `KindCI` as its own outcome, not as `failed`.
- **The CI line** goes in `HeaderNote`, for example: `CI: test passed · cycode-scan failed (blocks approval)`.
  - It holds job names and results only, never log or report text.
  - An `unreadable` job adds `no permission` or `error`.
- **`ErrorComment`** skips `render`, so it adds the same line itself. A failed review still shows the CI state.
- **The review's verdict** stays the model's own.
- **After publish,** a CI event for one of the *chosen* jobs on a published run's head re-reads the jobs. Events for other jobs are ignored. If a chosen job's result changed, pergent.sh re-decides from the stored result, as a dismissal does:
  - it updates the status and the approval;
  - it withdraws an approval when a blocking job now fails;
  - it edits the comment only when the CI line or the verdict actually changed.

  No new review runs.

### Storage

- **`dashboard_repos.ci_jobs`:** jsonb, nullable. Null means not set. It goes through `settings` like `check_deleted`.
- **`runs`:**
  - `ci_checked` (boolean, default false);
  - `ci_deadline` (timestamptz, nullable);
  - `ci_jobs` (jsonb, nullable): the last snapshot of each chosen job's name, result, what was read, size, and whether it was cut. Never the log or report text. That lives only in the Job's ConfigMap, which is removed with the Job.
- **`ci_pending`** is a new value of the existing waiting reason.

### Dashboard

- **The review page** lists each chosen job: its result, what was read and how big it was, and whether the wait timed out. It shows no CI text.
- **The settings page** edits the list.
  - **Picking a name:** a combo box suggests job and check names from the repository's last 10 pipelines or check runs. Free text is allowed for a job that hasn't run yet.
  - **Unknown names:** it warns when a name hasn't shown up in those runs, which is usually a typo.
  - **Model provider warning:** a one-line warning beside `log` and `report` says that the CI output goes to the model provider after secrets are removed, and that redaction catches known shapes, not everything.
- **Readiness** on GitHub warns when the installation lacks `actions: read` or the `check_run` event.
- **A `ci_pending` run** shows as waiting for CI, with its deadline.

## Edge cases

- **No pipeline at all** for the head, for example when CI rules skip it: no pipeline or suite is running, so nothing holds the wait. The jobs are `missing` at once, and a blocking one blocks. A pipeline created later re-decides the gate.
- **A name that only runs on `main`,** or a typo: it is `missing` as soon as the head's CI ends, so it doesn't add the full limit to every push.
- **A job that ends while the review runs:** the review uses what was read when the run was queued. The next CI event re-decides the gate after publish.
- **A manual GitLab job** that nobody ran is `skipped` and counts as ended, so it doesn't hold the wait. A blocking manual job blocks until someone runs it and it passes, and that event re-decides the gate.
- **Incremental re-reviews:** each push waits for its own head's jobs. Carried files don't change what is read.
- **An untrusted author:** the run waits as today. Once a person starts it, it checks CI once, and waits only until its deadline.
- **Quota:** the quota gate runs first, so a run held for quota doesn't also wait for CI until it is released.

## Upgrade

- **pergent:** a new flag, ignored unless given, and a new `render` option, empty by default.
- **pergent.sh:**
  - A migration adds `dashboard_repos.ci_jobs` and `runs.ci_checked`, `ci_deadline` and `ci_jobs`, dropping nothing.
  - Nothing changes until a repository chooses jobs.
  - `EnsureHook` turns on Pipeline and Job events on every GitLab hook it manages. Repositories without `ci_jobs` ignore those events.
  - GitHub installations must accept `actions: read` and the `check_run` event before Actions logs and reports can be read.
- **Rolling back:**
  - First remove `repositories[].ci_jobs` from the config, because the previous release refuses an unknown key.
  - A `ci_pending` run stays `waiting` under the older release until someone starts it or a push supersedes it.
  - GitLab hooks keep the extra events, and the older release ignores them.

## Delivery

1. **pergent:** `--ci-results`, the attachment, `ciMessage` and the batch budget, plus `render.Options.HeaderNote`. Released first.
2. **pergent.sh:**
   - the setting: config, migration, settings, API, web;
   - the wait: the CI hold and its transition, the sweep, the hook events and `EnsureHook`;
   - fetching: the GitLab and GitHub clients, `InstallationInfo`;
   - the file, the redactor and the input;
   - the gate, `KindCI` and the CI line, including in `ErrorComment`;
   - re-deciding after publish;
   - the dashboard and readiness;
   - docs: a new `docs/concepts/ci-jobs.md` (the setting, waiting, the gate, the fork limit, the model provider warning), a 🧪 line in the README's Features list, and rows in the config reference;
   - the `defaultImage` and `go.mod` pin to the new pergent.
3. **Releases are the owner's call.**

## Testing

**pergent:**
- Without the flag, prompts and the comment are byte-identical.
- Batch and cross-batch runs get `ci-results.md`; verification, repair and impact don't.
- A path inside `--repo-path` is a usage error, and an unreadable file warns.
- The 64 KiB cut, and redaction.
- The file counts against the batch budget.
- `HeaderNote`: empty leaves the golden files unchanged, set shows one line, and a note starting with `#` or `@` stays plain text.

**pergent.sh, the setting:**
- Access: a maintainer can change it, a viewer can't.
- Config wins.
- The limits, and the name `pergent` refused.

**pergent.sh, waiting:**
- A run whose jobs have all ended queues straight away. Otherwise it waits and queues when they end.
- The deadline queues a run, and the sweep works without events.
- A non-blocking job is waited for too.
- A `missing` job ends the wait once every pipeline (GitLab) or check suite (GitHub) on the head has finished. A GitHub suite with no check runs doesn't hold it.
- A head with no pipeline at all is `missing` at once.
- Start review skips the wait.
- A push supersedes a waiting run.
- A re-run and a started untrusted run check CI once, and don't loop.
- Events from pergent.sh's own App are ignored.

**pergent.sh, results:**
- Every status in both tables maps as listed.
- The newest attempt wins on GitLab.
- On GitHub, every check run with the name must pass.
- A merged-results pipeline counts only when the head is its parent, and a stale `head_pipeline` doesn't count.
- A trigger job takes its child pipeline's status.
- A fork-only pipeline is `missing`.
- A missing `actions: read` makes the job `unreadable`.
- A check run not from Actions gives its output as the log.

**pergent.sh, the gate:**
- A blocking job blocks on every result except `passed`, including on a draft, with approval off and on a neutral failure.
- A non-blocking job never blocks.
- `KindCI` is recorded as its own outcome.
- `ErrorComment` shows the CI line.

**pergent.sh, after publish:**
- A job passing re-decides and approves.
- An event for a job that isn't chosen does nothing.
- The comment is edited only when the CI line or the verdict changes.
- No CI text is stored in `runs.ci_jobs`.
- A job failing withdraws approval.

**pergent.sh, the file:**
- The caps and cut marks.
- The tail kept from a log over 2 MiB.
- ANSI codes removed, and the fence.
- Redaction of each secret shape.
- `Fit` drops the file first.
- The env var is set only when there is a file.

**pergent.sh, an old image:** it reviews, and the gate still applies.

## Calls made in the builder check

The owner confirmed the first two in the grilling round (Q2, Q3). The rest are mine:

- Every run of a repository with jobs chosen checks CI once, including re-runs and started runs, through its own hold and a `ci_checked` marker. **Start review** skips the wait.
- A blocking CI failure turns the status failed even on a draft, with approval off, or on a neutral review failure.
- A GitLab `manual` job is `skipped` and ends the wait.
- An `unreadable` job's reason is shown as `no permission` or `error` only. A fork pipeline shows as `missing`, because it can't be told apart.
- `HeaderNote` is written in italics, so its text never starts a markdown line.
- `wait` is a duration (`30m`), not a number of minutes.
- On GitHub, a check suite with no check runs is ignored when deciding that the head's CI has finished.

## Left to the builder

- Wording of `ciMessage`, the setting's help text, the waiting status and the readiness notes.
- How the sweep batches API calls across many waiting runs.
- Whether a `Job Hook` payload is read to skip an API call. The API stays the source of truth.
- The exact secret patterns, as long as each named shape is covered and tested.
