# 🧪 CI jobs

A review runs on its own. It doesn't know that `go test` failed, or that a scanner found a leaked key, in another CI job on the same commit. A repository can choose CI jobs by name. pergent.sh then reads their results through the GitLab or GitHub API, gives them to the model as context, and can hold approval back when a job it marks as blocking didn't pass. Users change nothing in their CI files.

**Off by default.** A repository with no jobs chosen behaves as it always did: the same statuses, comment and Job.

## What it does

- **Context.** The model reads the results to focus the review and to avoid repeating what CI already caught.
- **A gate.** A job marked `blocks_approval` keeps approval back unless it passed, even when the review is clean.
- **A wait.** A review for a push waits for the chosen jobs on the head commit, then runs with what it has.
- **GitLab and GitHub.** Both work the same way.

## Choosing jobs

The setting is **CI jobs** in the repository's **Settings**, `repositories[].ci_jobs` in the config ([Config reference](../reference/configuration.md)). Maintainers and admins change it. A repository listed in the config is read-only in the dashboard, and the config value wins. **Reset to defaults** clears the list.

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

- **`name`.** The GitLab job name, a GitLab trigger job's name, or the GitHub check run name. It is an exact match. A matrix is chosen by listing each name, or by the workflow's summary job.
- **`read`.** One of `status`, `log` or `report`. The status is always read, because the gate needs it.
  - `status` reads only the result.
  - `log` adds the tail of the log for a job that failed, was cancelled, or was still running.
  - `report` reads the file at `report.path` for a job that passed or failed.
  - A GitLab trigger job's result is its child pipeline's status, not the trigger job's own: without `strategy: depend` GitLab marks the trigger job `success` as soon as the child pipeline is created. A trigger job whose child pipeline doesn't exist yet is `running`, unless the trigger job itself failed, was cancelled or was skipped.
  - A GitLab trigger job, and a GitHub check run that is not from Actions, has no report. `report` reads as `unreadable` for those, and blocks if the job is blocking.
- **`blocks_approval`.** Default `false`. When it is on, any result but `passed` blocks.
- **`wait`.** How long a review waits, such as `30m`. Default `30m`, from `5m` to `2h`.
- **Name suggestions.** The dashboard suggests names from the repository's last 10 pipelines or check runs. Free text is allowed for a job that hasn't run yet, and the editor warns when a name hasn't shown up, which is usually a typo.

## Waiting

- **Every run checks CI once** before it starts, after the trust and quota gates. That covers a push, a re-run and an untrusted run a person started. An API review has no pipeline and never waits.
- **Every chosen job is waited for,** blocking or not. The limit caps the cost.
- **The wait ends** when every chosen job has ended, or when the head's CI has finished, or at the deadline: the run's creation time plus `wait`. The status says `Waiting for CI: test, cycode-scan`, and the dashboard shows the deadline.
- **A short grace for jobs that are not there yet.** When every chosen job is missing, the review still waits up to 3 minutes from the push, never past the wait limit. GitLab creates merge request pipelines after the webhook, and GitHub check suites can lag.
- **A job not found** after that is `missing`.
  - On GitLab, "the head's CI finished" is judged from the jobs and trigger jobs of the pipelines that count for the head (a trigger job by its child pipeline), not from a pipeline's status.
  - On GitHub, every check suite on the head must be completed. pergent.sh's own check suite is ignored, and so is a suite with no check runs.
- **Start review** skips the rest of the wait and uses whatever results exist then.
- **A new push** supersedes a waiting run.
- **Events only signal.** pergent.sh always reads the results through the API. A GitLab job event or a GitHub `check_run` event counts only for a chosen job's exact name. A GitLab pipeline event always counts, since a pipeline that ends can end the wait. A sweep every 2 minutes also checks waiting runs, so a lost event only delays a run, and it queues runs past their deadline.

## The results

Each job ends as one of:

| Result | Meaning |
| --- | --- |
| `passed` | GitLab `success`. GitHub `success` or `neutral`. |
| `failed` | Failed, including a GitLab `allow_failure` job, and a GitHub `timed_out`, `action_required`, `startup_failure` or `stale`. |
| `cancelled` | Cancelled. |
| `skipped` | Skipped, or a GitLab `manual` job nobody ran. It counts as ended. |
| `missing` | No job or check run with that name on the head. |
| `running` | Still running at the deadline. |
| `unreadable` | pergent.sh couldn't read it. The reason is `no permission` or `error`. |

- **GitLab.** The pipelines that count for the head are those found by commit, and merge request pipelines for merged results whose commit has the head as a parent. The newest attempt of a job wins. A pipeline that ran only in a fork project is not seen, so its jobs are `missing`.
- **GitHub.** Every check run with the exact name must pass, which is the newest attempt of each. So two workflows that both name a job `test` must both pass.
- **Each CI read at review time has 60 seconds.** A read still going then counts as failed: before the wait it holds the run, as any failed read does, and at the review every job is `unreadable` (`error`).
- **If reading a log or report fails at review time,** the job is `unreadable`. If it is blocking, it stays blocking until a new push or a re-run. Later status checks don't flip it.

## The gate

- **Strict.** A blocking job counts only when pergent.sh read `passed` for it on the run's head commit. `failed`, `cancelled`, `skipped`, `missing`, `running` and `unreadable` all block. A job that isn't blocking never blocks.
- **The `pergent` status turns failed,** and approval is held back, and the status description names the job. This holds on a draft, with approval off, and with `failure_status: neutral`.
- **The outcome is its own.** When CI is the only reason, the dashboard shows **Blocked by CI**. A review with blocking findings stays "blocking", and a failed review stays failed, with the CI line beside it.
- **The CI line.** The comment shows one italic line after its header, for example `CI: test passed · cycode-scan failed (blocks approval)`. The line pergent.sh writes holds job names and results only. An `unreadable` job adds `no permission` or `error`. A failed review shows the line too. A review that failed before pergent.sh read its CI, such as a Job that couldn't be created, counts every job the repository chooses as `unreadable` (`error`), so a blocking job still holds the status failed.
- **After the review is published,** pergent.sh checks again on CI events for the chosen jobs, and every 10 minutes for 24 hours when a chosen job hadn't passed. If a result changed, it updates the status and the approval, withdraws an approval when a blocking job now fails, and edits the comment only when the CI line or the verdict changed. No new review runs, and the model doesn't see the later result.
- **A re-run** checks CI again.

## What the model sees

- **One file, `ci-results.md`,** with a section per job: its name, its result, and its log tail or report. It is written whenever the repository has jobs chosen, even when every job is `status` only, so the model knows a test failed.
- **Caps.**
  - A log tail is the last 200 lines and at most 16 KiB.
  - A report is at most 16 KiB.
  - The whole file is at most 64 KiB, and every batch of a review gets it. A change with several batches pays for it several times.
  - Each cut is marked in the file.
- **Cleaning.** Colour codes are removed. Secrets of known shapes (not everything) are removed by pergent.sh, and pergent removes them again.
- **Data, not instructions.** Logs and reports come from running the change's own code, so its author controls them. The model is told to use them to find where to look and what not to repeat, never to report a CI result itself as a finding, and never to let a passed job excuse a problem. The verification run does not get the file. These are instructions in the prompt, so a malicious log can still influence the review.
- **What is kept.** pergent.sh's own code never puts log or report text in the comment, the logs, `runs.ci_jobs` (the per-job snapshot) or the dashboard. The dashboard shows each job's result, what was read, its size and whether it was cut. The text is in the Job's input ConfigMap while the review runs, and is removed with the Job. The model can quote the text in a finding or a summary, and then it appears in the comment like any other model text.
- **It needs a newer pergent image.** The model sees the results only with a pergent image that reads `--ci-results`. An older image ignores the file and reviews as before, and the gate still applies, because pergent.sh decides it. The release notes name the version.

## The model provider

CI output goes to the model provider, after secrets are removed. The redaction catches known shapes, such as bearer tokens, credentials in URLs, private key blocks and common token formats, not everything. A log can hold something unusual. Choose `log` and `report` only for jobs whose output you are happy to send to the provider. `status` sends no text. The dashboard shows this warning beside `log` and `report`.

## GitLab forks

A pipeline that ran only in a fork project is not in the target project's lists, so its jobs are `missing`. With a blocking job, pergent.sh never approves such a merge request until its pipeline runs in the target project. Run fork pipelines in the target project, which is GitLab's own advice for forks. Otherwise a blocking job never passes.

## GitHub

- **Permission and event** ([GitHub setup](../setup/github.md)). `Actions: Read` is needed only by repositories whose CI jobs read `log` or `report` from GitHub Actions; a `status` read uses Checks, which the App already has. The `Check run` event is optional: without it pergent.sh notices CI changes on its 2-minute check instead of at once. GitHub applies a new permission to an installation only after its owner accepts it.
- **Until then,** logs and reports from Actions are `unreadable` (`no permission`).
- **Readiness.** The dashboard warns, for repositories that have CI jobs, when an installation lacks the permission or the event.
- **Other apps.** A check run from another app, such as a scanner's own GitHub App, gives its output text as the log. It has no report.
- **A report from Actions** is read from a workflow artifact, which is read into memory up to 20 MiB. A bigger one is `unreadable`.

## Limits

- At most 10 jobs, with unique names.
- The name `pergent` is refused, because that is pergent.sh's own status.
- `wait` from `5m` to `2h`.
- A report `path` is relative, with no `..`.
- File caps: a log tail of 200 lines and 16 KiB, a report of 16 KiB, the whole file 64 KiB.
- Not done: reports sent from CI, turning a scanner's findings into review findings, jobs inside GitLab child pipelines (a trigger job can be chosen), name prefixes, GitLab external commit statuses, and a new review when a result changes later.
