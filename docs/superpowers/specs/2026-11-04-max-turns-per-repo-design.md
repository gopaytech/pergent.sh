# Step limit per repository (`max_turns`)

pergent.sh 0.5.4. Status: approved design, not built.

## Why

pergent stops each opencode run (one batch of one skill) after `--max-turns` steps. The default is 20. pergent.sh never passes the flag, so every review Job gets 20, and nobody can change it.

20 is too few:
- the review of pergent.sh#4 (6 test files) used all 20 steps reading the code around the change, and failed with "Reached the 20-step limit";
- one batch of pergent#11 used 18.

A step makes 1 to 4 tool calls. On the last allowed step opencode tells the model to stop and summarise, so pergent counts an answer from that step as unfinished (pergent `internal/runner/outcome.go`). A run that reaches the limit is always a failed batch.

## Goals

- Reviews stop failing at 20 steps by default.
- The operator sets the step limit in the config, for all repositories and for one repository.
- A repository's maintainer or admin sets it in the dashboard, the same way as the Job timeout.
- Wherever a review says it hit the step limit, it says where to raise it.
- Each review shows the limit it ran with.

## Non-goals

- A limit per skill or per batch. It is one value per repository.
- Any change to pergent. pergent v0.3.0 already takes `--max-turns`.
- Tying the step limit to the Job timeout (no warning, no derived value). The Job timeout keeps bounding a run's total time.

## Anti-goals

- A repository listed in the config that the dashboard can change. Config-managed repositories stay read-only in the dashboard, as today.
- Step-limit advice anywhere in pergent.sh that can't be followed, such as "raise --max-turns", where nobody passes flags.

## Decisions

### Where it is set

It works exactly like the Job timeout (`jobs.timeout`, `repositories[].job_timeout`, the dashboard's Job timeout setting):

| Where | Key / control | Default | Allowed |
|---|---|---|---|
| Config, every repository | `jobs.max_turns` | **40** | 5 to 200 |
| Config, one repository | `repositories[].max_turns` | `jobs.max_turns` | 5 to 200 |
| Dashboard, one repository | Repository → Settings → **Max steps** | the default below | 5 to 200 |
| Dashboard default, for repositories switched on in the dashboard | Admin → Settings → **Max steps** | `jobs.max_turns` | 5 to 200 |

How the value is chosen:
- **A repository listed in the config:** its `max_turns` if set, else `jobs.max_turns`.
- **A repository switched on in the dashboard:** its own setting if set, else the Admin → Settings default if set, else `jobs.max_turns`.

This is the same order the Job timeout uses (`internal/settings/settings.go`).

A value outside 5 to 200 is refused:
- in the config, at startup, with the repository named, the same way `job_timeout` is checked;
- in the dashboard, with a 400 and a message.

**Who can change it** is the same as the Job timeout:
- **A repository's maintainer or admin**, in its Settings.
- **"Overridden here" or "default":** the setting shows which, as the other settings do.
- **Reset to defaults** clears it.
- **Admin → Settings:** edited by whoever edits the other defaults there today.

The README's role table and the Admin → Settings description add "Max steps" next to "Job timeout and memory".

The dashboard label is **Max steps** *(my call)*, with the help text "Steps each opencode run may take before pergent stops it; each step can call several tools." The config key is `max_turns`, matching pergent's `--max-turns`.

### What a review does

- **Dispatch:** reads the repository's effective value and passes `--max-turns=N` to the review Job, next to `--timeout` and `--concurrency` (`internal/kube/job.go`).
- **Each run records the value it ran with**, in a new nullable column on `runs`. Runs from before the upgrade have none.
- **After a `review failed` re-run**, the merged review keeps the skills that didn't re-run from the earlier run. The review page shows the limit of the run it is showing, the new Job's. *(my call)*
- **The review page shows it** in the line that has the pergent version and the model: `· max steps 40`. A run without a recorded value shows `not recorded`, as the model does.
- **`review failed`:** changing the limit doesn't stop it from keeping the earlier results. A changed limit is not a changed skill.

### The step-limit advice

pergent ends every step-limit message with the same phrase, `(raise --max-turns to allow more steps)` (pergent `internal/runner/outcome.go`). In pergent.sh nobody passes flags, so that advice can't be followed. The phrase shows up in three places:

1. **A failed batch's message**, `failures[].message`. Its cause kind is `step_limit`.
2. **A skill's notes**, when the verification run or the cross-batch run hits the limit ("Verification failed: …", "Cross-batch check failed: …"). These have no cause kind (pergent `internal/review/verify.go`, `crossbatch.go`).
3. **Each not-reviewed file's reason**, `files.not_reviewed[].cause`, which the dashboard's review page lists per file.

**Fix:** pergent.sh replaces that exact phrase wherever it appears in those three places. *(your call: all three places)*

- **Text around the phrase stays.** The lead-in ("The model's answer was not in the required format (…), and the repair run failed: Reached the 40-step limit …") and pergent's own "Reached the N-step limit" are kept. pergent already writes the real N, because pergent.sh passed it as `--max-turns`.
- **Replacement for a dashboard repository:** `(raise Max steps in the repository's Settings to allow more steps)`.
- **Replacement for a config-managed repository:** `(raise max_turns for this repository in pergent.sh's config to allow more steps)`. *(my call: name only the place that can change it)*
- **Plain text.** The text stays plain, with no markdown, because `failures[].message` is plain text and the dashboard shows it as is.
- **When:** the rewrite happens once, when pergent.sh receives and stores the Job's result, before anything reads it. The comment, every re-render (a dismissal, a merged re-run) and the dashboard then show the same text. The advice is fixed at that moment and isn't rewritten if the repository later moves between the dashboard and the config. *(my call)*
- **The phrase is a contract with pergent.** A pergent.sh test pins the exact phrase. pergent's wording for it must not change without changing pergent.sh.

### Storage

Migration `0016`:
- `dashboard_repos.max_turns integer CHECK (max_turns BETWEEN 5 AND 200)`, NULL meaning the default;
- `repo_defaults.max_turns`, with the same check, NULL meaning `jobs.max_turns`;
- `runs.max_turns integer`, NULL for runs from before the upgrade.

The columns are nullable, so 0.5.3 running against a migrated database still works. A 0.5.3 pod ignores the new columns.

### Release

- **pergent.sh 0.5.4.** The Upgrading section says:
  - the default rises from 20 to 40 steps;
  - migration `0016` runs at startup;
  - to roll back to 0.5.3, first remove `jobs.max_turns` and `repositories[].max_turns` from the config, because 0.5.3 refuses unknown config keys and won't start with them. *(my call)*
- **Chart:** the chart's config values gain nothing new; `jobs.max_turns` goes in the config file like `jobs.timeout`.
- **community-ops:** bumps to 0.5.4.

## Left to the builder

- Names inside the code (`MaxTurns` fields, the settings key `max_turns`, API field `maxTurns`), following the Job timeout's names.
- The dashboard API shape for the new field, following `jobTimeoutSeconds`.
- Test layout, following the Job timeout's tests in config, settings, store, dashboard API, dispatch and the two settings pages.
