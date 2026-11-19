# CI Job Results (pergent.sh) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A repository names CI jobs. A review waits for them on the head, reads their status, log tail or report through the GitLab or GitHub API, hands them to the review Job as `ci-results.md`, and a blocking job that didn't pass holds approval back.

**Architecture:**
- **`internal/ci`** holds the pure parts: result mapping, the log tail, the redactor, the file and the CI line.
- **`forge.CIReader`**, an optional interface both forges implement, reads the jobs.
- **The dispatcher** gains a CI hold after the quota gate.
- **A 2-minute sweep and the webhook events** release waiting runs and re-decide published ones.
- **`policy`** applies the gate and writes the CI line through pergent's `render.Options.HeaderNote`.

**Tech Stack:** Go, Postgres (pgx), React + TypeScript (vitest), the GitLab and GitHub REST APIs.

**Spec:** `docs/superpowers/specs/2026-11-18-ci-job-results-design.md`. Run in this worktree (`pergent.sh-ci-results`, branch `docs/ci-job-results`), or on a `feat/ci-job-results` branch cut from it.

**Depends on:** Plan A (`2026-11-18-ci-results-pergent.md`) for `render.Options.HeaderNote`.
- Until pergent is released, `go.mod` points at the Plan A branch tip with a pseudo-version (`go get github.com/gopaytech/pergent@<sha>`).
- Task 12 moves it to the release.

## Global Constraints

- **Off by default.** With no `ci_jobs` set, a run behaves byte for byte as today: same statuses, comment, inputs and Job.
- **Never a false pass.** A blocking job counts only as `passed` read on the run's head. Every other result blocks: `failed`, `cancelled`, `skipped`, `missing`, `running`, `unreadable`.
- **Results:** `passed`, `failed`, `cancelled`, `skipped`, `missing`, `running`, `unreadable`. `unreadable` reasons: `no permission`, `error`.
- **Limits:**
  - `wait` from `5m` to `2h`, default `30m`;
  - at most 10 jobs, names unique, `pergent` refused;
  - `read` is one of `status`, `log`, `report`.
- **File caps:**
  - a log tail: 200 lines and 16 KiB;
  - a report: 16 KiB;
  - the whole file: 64 KiB;
  - a log is read through a rolling 2 MiB buffer.
- **No CI text is stored or logged.** Only the per-job snapshot (name, result, reason, read, size, cut, blocks) goes in `runs.ci_jobs`.
- **Migrations only add,** with NULL or a default. The number is `0043`: `0042` is taken by another branch in flight (`feat/repository-pause`), and the runner applies versions independently, so a gap is fine (there is already no `0035`).
- **Never connect to a local database or set `PERGENT_SH_TEST_DATABASE_URL`.** Store, engine-e2e, webhook-settings and dashboard tests skip locally. Write them anyway: CI's `test` check runs them.
- **Local gates:**
  - Go: `go vet ./... && go test ./...`;
  - web: `cd web && npm run typecheck && npm run lint && npx vitest run`;
  - chart: `helm lint charts/pergent-sh -f charts/pergent-sh/ci-values.yaml`.
- **Name scan before each commit:** `git grep -niE 'golabs|gojek|gopay[^t]|goto\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'` prints nothing.
- **Commits:** `git -c user.email=zufardhiyaulhaq@gmail.com commit`, ending `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never push, tag or open a PR unless the owner asks.

## Rulings on the spec

- **`ErrorComment`** is used in production by `decide` (`policy.go:237`). Its signature becomes `ErrorComment(reason, ciLine string) string`.
- **CI text is read twice.** Status-only reads (`withText=false`) serve the wait and the re-decide. A full read (`withText=true`) happens once, at dispatch. Stored snapshots carry no text, so dispatch must re-read.
- **The sweep re-checks published runs** too, for 24 hours after publish, when their snapshot has a chosen job not `passed`. That way a lost event only delays a gate update, as the spec asks for waiting runs.
- **GitHub artifact zips** are read into memory up to 20 MiB. A bigger one is `unreadable` (`error`).
- **The unknown-name warning** is computed in the web client from the suggestions endpoint.

## Review Focus

- **A run superseded while its CI check is in flight.** `ReleaseCI` and `SetRunCI` act only on the run's own row and never revive a `superseded` run. Test in Task 2.
- **A repository's `ci_jobs` emptied while runs wait.** The next sweep releases those runs, and they review as today. Test in Task 7.
- **pergent.sh down past a run's deadline.** The first sweep after start releases it. Test in Task 7.
- **A GitHub check name with spaces or parentheses** (`test (1.22)`): `check_name` is query-encoded and matched exactly. Test in Task 5.
- **A trace with CRLF, a GitLab section marker, or no trailing newline:** the tail is clean, with no `section_start:` text. A report path that is a 404 or a directory is `unreadable`, not an error. Tests in Tasks 3 and 4.

---

### Task 1: Types, validation and config

**Files:**
- Create: `internal/store/ci.go`, `internal/ci/validate.go`, `internal/ci/validate_test.go`
- Modify: `internal/config/config.go` (`Repository` ~L207, `fileRepository` ~L309, `repository()` ~L350, `Validate()` repositories loop ~L665)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces in `store`:

  ```go
  type CIReport struct { Artifact string `json:"artifact,omitempty"`; Path string `json:"path"` }
  type CIJob struct {
      Name string `json:"name"`; Read string `json:"read"` // "status" | "log" | "report"
      Report *CIReport `json:"report,omitempty"`; BlocksApproval bool `json:"blocks_approval"`
  }
  type CIJobs struct { WaitSeconds int64 `json:"wait_seconds"`; Jobs []CIJob `json:"jobs"` }
  func (c CIJobs) Wait() time.Duration
  type CIJobResult struct {
      Name, Result, Reason, Read string; Size int; Cut, Blocks bool
  } // json tags: snake_case
  ```

- Produces in `ci`: consts `MinWait = 5*time.Minute`, `MaxWait = 2*time.Hour`, `DefaultWait = 30*time.Minute`, `MaxJobs = 10`, and `func Validate(c store.CIJobs, github bool) error`. The error text names the field, for example `ci_jobs.jobs[1].report.path must be relative`.
- Produces in `config`: `Repository.CIJobs *store.CIJobs`. Nil means not set. When set without a `wait`, the wait is `DefaultWait`.

- [ ] **Step 1: Write the failing tests**

`internal/ci/validate_test.go`, table `TestValidate`. It passes on:
- a `log` job;
- a `report` job with a path on GitLab;
- a `report` job with an artifact and a path on GitHub;
- an empty `Jobs`.

It fails, with the text naming the field, on:
- 11 jobs;
- a duplicate name;
- `Pergent` in any case;
- an empty name, or one over 255 bytes;
- `read: logs`;
- `read: report` without `report`;
- `report` with `read: log`;
- a path `""`, `/etc/x`, `a/../b` or `..`;
- a GitHub report without an artifact;
- a wait of `4m` or `3h`.

`config_test.go`, mirroring `TestLinkedReposConfig`:
- `TestCIJobsConfig`: the spec's YAML (with `gitlab: main`, `project: group/project`) parses to the expected `store.CIJobs` with `WaitSeconds: 1800`, and with no `wait` it defaults to 1800.
- `TestCIJobsRefused`: a bad entry fails `Validate()` with `repositories[0] (group/project): ci_jobs...`, and an unknown key under `ci_jobs` is refused.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/ci/ ./internal/config/ -run 'Validate|CIJobs'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- **YAML types:** `fileCIJobs{Wait *duration yaml:"wait"; Jobs []fileCIJob}`, `fileCIJob{Name, Read string; Report *fileCIReport; BlocksApproval *bool yaml:"blocks_approval"}` and `fileCIReport{Artifact, Path string}`.
- **`repository()`** copies them into `*store.CIJobs`, as it does links.
- **`Validate()`** calls `ci.Validate(*repo.CIJobs, repo.GitHub != "")` and adds the error with the repository prefix.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/ci/ ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/ci.go internal/ci internal/config
git commit -m "feat(config): repositories[].ci_jobs and its validation"
```

### Task 2: Migration, store and settings

**Files:**
- Create: `internal/store/migrations/0043_ci_jobs.sql`, `internal/store/ci_runs.go`, `internal/store/ci_runs_test.go`
- Modify:
  - `internal/store/dashboard.go`: `DashboardRepo` ~L18, `dashboardRepoColumns` ~L169, the scan ~L179, and the upsert, create and update ~L242/307/370;
  - `internal/store/runs.go`: `Run` ~L74, `runColumns` ~L240, the scan, `WaitCIPending` beside `WaitQuota` ~L45, `StartRun` ~L881;
  - `internal/store/projects.go`: `Project`;
  - `internal/settings/settings.go`: keys ~L40, `Effective` ~L69, the `defaultsFor` key list ~L299, `fromConfig` ~L432, `fromDashboard` ~L510.
- Test: `internal/settings/settings_test.go`, `internal/store/ci_runs_test.go`

**Interfaces:**
- Consumes: `store.CIJobs`, `store.CIJobResult` (Task 1).
- Produces:
  - `DashboardRepo.CIJobs *CIJobs`;
  - `Run.CIChecked bool`, `Run.CIDeadline *time.Time`, `Run.CIJobs []CIJobResult`;
  - `Project.ActionsReadable *bool`, `Project.CheckRunEvents *bool`;
  - `const WaitCIPending = "ci_pending"`;
  - `settings.KeyCIJobs = "ci_jobs"`, `Effective.CIJobs store.CIJobs` (zero means off);
  - store methods:

  ```go
  func (s *Store) HoldForCI(ctx context.Context, id string, deadline time.Time) (bool, error)
  func (s *Store) ReleaseCI(ctx context.Context, id string) (bool, error)
  func (s *Store) MarkCIChecked(ctx context.Context, id string) error
  func (s *Store) SetRunCI(ctx context.Context, id string, jobs []CIJobResult) error
  func (s *Store) CIPendingRuns(ctx context.Context) ([]Run, error)
  func (s *Store) CIRunsForHead(ctx context.Context, instance, project, head string) ([]Run, error) // ci_pending, or the newest published run of each MR with that head
  func (s *Store) CIRecheckRuns(ctx context.Context, since time.Time) ([]Run, error)           // newest published runs since `since` whose ci_jobs has a result other than "passed"
  func (s *Store) SetProjectCIAccess(ctx context.Context, projectID string, actionsReadable, checkRunEvents bool) error
  ```

- [ ] **Step 1: Write the migration**

```sql
ALTER TABLE dashboard_repos ADD COLUMN ci_jobs jsonb;
ALTER TABLE runs ADD COLUMN ci_checked boolean NOT NULL DEFAULT false;
ALTER TABLE runs ADD COLUMN ci_deadline timestamptz;
ALTER TABLE runs ADD COLUMN ci_jobs jsonb;
ALTER TABLE projects ADD COLUMN actions_readable boolean;
ALTER TABLE projects ADD COLUMN check_run_events boolean;
CREATE INDEX runs_ci_pending_idx ON runs (ci_deadline) WHERE state = 'waiting' AND skip_reason = 'ci_pending';
```

Check the `projects` table's real name and key column in `0001`–`0041` before writing the last two lines.

- [ ] **Step 2: Write the failing tests**

`ci_runs_test.go` is DB-gated, using the `testURLEnv` helper. Mirror `TestHoldForQuotaHoldsPushAndAutoRerun` and `TestStartRunMarksQuotaChecked`:

- `TestHoldForCIHoldsQueuedAndStartedRuns`: it holds a `push` run and a `start` run. It doesn't hold a dispatched run or a `ci_checked` one. `skip_reason='ci_pending'` and the deadline are stored.
- `TestReleaseCIOnlyCIPending`: it queues a `ci_pending` run with `ci_checked=true`. It returns false for an `untrusted_author` waiting run and for a `superseded` run.
- `TestStartRunOnCIPendingMarksChecked`: StartRun on a `ci_pending` run sets `ci_checked`. On an `untrusted_author` run it leaves `ci_checked` false.
- `TestSetRunCIRoundTrip`: a snapshot reads back equal through `Run.CIJobs`, and `nil` reads back as `nil`.
- `TestCIRunsForHead`, `TestCIRecheckRuns`: each selects only the rows named in its signature comment.
- `TestDashboardRepoCIJobsRoundTrip`: create, update and upsert keep `CIJobs`, and nil stays NULL.

`settings_test.go`, mirroring `TestResolverConfigWins` and `TestResolverDefaultsAndOverrides`:
- `TestResolverCIJobs`: config wins and marks `Overridden["ci_jobs"]`. A dashboard row with `CIJobs` sets it. Neither gives a zero `CIJobs`.

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/settings/ ./internal/store/ -run 'CI'`
Expected: FAIL to compile. With no DB the store tests then skip; settings' DB tests skip too.

- [ ] **Step 4: Implement**

- jsonb helpers `ciJobsParam(*CIJobs) (any, error)` and `readCIJobs([]byte) (*CIJobs, error)`, plus the same for `[]CIJobResult`, modelled on `linksParam` and `readLinks`.
- `HoldForCI` is `HoldForQuota`'s `UPDATE` without the `trigger <> 'start'` clause, with `NOT ci_checked`, and it sets `ci_deadline`.
- `ReleaseCI`:

  ```sql
  UPDATE runs SET state='queued', skip_reason='', ci_checked=true
  WHERE id=$1 AND state='waiting' AND skip_reason='ci_pending'
  ```

  Use the column's real "no reason" value.
- `StartRun` adds `ci_checked = (skip_reason = 'ci_pending')` to its update.

- [ ] **Step 5: Run the packages and vet**

Run: `go vet ./... && go test ./internal/settings/ ./internal/store/`
Expected: PASS (DB tests SKIP).

- [ ] **Step 6: Commit**

```bash
git add internal/store internal/settings
git commit -m "feat(store): ci_jobs columns, the CI hold and its release"
```

### Task 3: `internal/ci`: results, tail, redactor, file, line

**Files:**
- Create: `internal/ci/result.go`, `tail.go`, `redact.go`, `file.go`, `line.go`, and a `_test.go` for each.

**Interfaces:**
- Produces:

  ```go
  type Result string // consts Passed, Failed, Cancelled, Skipped, Missing, Running, Unreadable with the spec's spellings
  type Job struct {
      Name, Read string; Blocks bool
      Result Result; Ended bool; Reason string // "" | "no permission" | "error"
      Log, Report string // only filled by a withText read
  }
  type Snapshot struct { Head string; Jobs []Job; Finished bool } // Finished: every pipeline (GitLab) / every check suite with check runs (GitHub) on Head has ended
  func FromGitLab(status string) (Result, bool)            // bool = ended; the spec's GitLab table, unknown → (Failed, true)
  func FromGitHub(status, conclusion string) (Result, bool) // the spec's GitHub table, neutral → Passed
  func (s Snapshot) Done() bool                            // every job Ended, or Finished and every not-ended job is Missing
  func (s Snapshot) Results() []store.CIJobResult           // Size = len(Log)+len(Report), no text
  func Tail(r io.Reader) (text string, cut bool, err error) // rolling 2 MiB, strip ANSI + GitLab section markers, CRLF→LF, ToValidUTF8, last 200 lines, last 16 KiB on a rune start
  func Cap(s string) (string, bool)                        // a report: ToValidUTF8, first 16 KiB on a rune start
  func Redact(s string) string
  func Build(s Snapshot) []byte                            // ci-results.md
  func Line(jobs []store.CIJobResult) string               // "" when jobs is empty
  func Blocking(jobs []store.CIJobResult) []store.CIJobResult // Blocks && Result != "passed"
  ```

- [ ] **Step 1: Write the failing tests**

- **`TestFromGitLab` / `TestFromGitHub`:** one row per line of the spec's two tables, plus `allow_failure` failed giving `Failed`, `manual` giving `(Skipped, true)`, and an unknown value giving `(Failed, true)`.
- **`TestDone`:**
  - a running job means not done;
  - all jobs ended means done;
  - a `Missing` job while not `Finished` means not done;
  - a `Missing` job when `Finished` means done.
- **`TestTail`:**
  - 300 lines keep the last 200, with `cut` true;
  - a 3 MiB body keeps its real last line;
  - `\x1b[31mred\x1b[0m` becomes `red`;
  - `section_start:1700000000:build\r\x1b[0K` lines are removed;
  - CRLF becomes LF;
  - an invalid byte gives valid UTF-8;
  - a 20 KiB single line is cut on a rune start, at most 16 KiB.
- **`TestRedact`:** each of these is replaced by `[REDACTED]`, and a key name stays:
  - `Bearer abcdefgh123`;
  - `https://user:pass@host/x`;
  - a `-----BEGIN RSA PRIVATE KEY-----` block;
  - `AKIAABCDEFGHIJKLMNOP`;
  - a `ghp_` + 36 characters token;
  - a `github_pat_...` token;
  - a `glpat-...` token;
  - an `xoxb-...` token;
  - `password=hunter2`, which becomes `password=[REDACTED]`.
- **`TestBuild`:**
  - a `log` job that failed has its tail in a fence;
  - a `log` job that passed has a heading only;
  - a `report` job has its report;
  - a log holding ```` ``` ```` gets a fence of four backticks;
  - jobs totalling over 64 KiB keep every heading, leave out the later texts with the note `_Text left out: the file's 64 KiB limit._`, and total ≤ 64 KiB;
  - a job with `Reason` shows it in its heading;
  - the output contains no text from a job whose `Read` is `status`.
- **`TestLine`:**
  - `[{test passed} {cycode-scan failed blocks}]` gives `CI: test passed · cycode-scan failed (blocks approval)`;
  - an unreadable job gives `api unreadable (no permission)`;
  - empty gives `""`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/ci/`
Expected: FAIL.

- [ ] **Step 3: Implement** with only the standard library (`regexp`, `bufio`, `unicode/utf8`).

- **`Build`'s format:** first `# CI job results` and `Head commit: <sha>`. Then per job, `## <name>: <result>[ (<reason>)][ (blocks approval)]`, then `Log tail:` or `Report:`, then the fenced text. Then a cut note, `_Cut to the last 16 KiB._` or `_Cut to the first 16 KiB._`.
- **The fence** is `max(3, longest backtick run + 1)` backticks with info string `text`.
- **`Build` calls `Redact`** on every text before writing it.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/ci/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ci
git commit -m "feat(ci): result mapping, log tail, redactor, results file and CI line"
```

### Task 4: GitLab reader and hook events

**Files:**
- Modify:
  - `internal/gitlab/client.go`: a new streaming helper, `EnsureHook` ~L852;
  - `internal/gitlab/types.go`;
  - `internal/forge/forge.go`: add `CIReader` beside `Approver` ~L311;
  - `internal/forge/gitlabforge/gitlabforge.go`.
- Create: `internal/gitlab/ci.go`, `internal/gitlab/ci_test.go`, `internal/forge/gitlabforge/ci.go`, `internal/forge/gitlabforge/ci_test.go`

**Interfaces:**
- Consumes: `ci.Job`, `ci.Snapshot`, `ci.FromGitLab`, `ci.Tail`, `ci.Cap` (Task 3), `store.CIJob` (Task 1).
- Produces:

  ```go
  // forge
  type CITarget struct {
      ProjectID int64; Project string; Change int64; Head string // Change = MR iid / PR number
      RecentHeads []string // only for RecentCINames on GitHub: head SHAs of the repository's last 10 runs
  }
  type CIReader interface {
      ReadCI(ctx context.Context, t CITarget, jobs []store.CIJob, withText bool) (ci.Snapshot, error)
      RecentCINames(ctx context.Context, t CITarget) ([]string, error)
  }
  // gitlab
  type Pipeline struct { ID int64; SHA, Ref, Status string }
  type CIJob struct { ID int64; Name, Status string }
  func (c *Client) PipelinesForSHA(ctx context.Context, projectID int64, sha string) ([]Pipeline, error)
  func (c *Client) MRPipelines(ctx context.Context, projectID, iid int64) ([]Pipeline, error)
  func (c *Client) CommitParents(ctx context.Context, projectID int64, sha string) ([]string, error)
  func (c *Client) PipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]CIJob, error)   // jobs + bridges
  func (c *Client) RecentPipelines(ctx context.Context, projectID int64, n int) ([]Pipeline, error)
  func (c *Client) JobTrace(ctx context.Context, projectID, jobID int64) (io.ReadCloser, error)
  func (c *Client) JobArtifact(ctx context.Context, projectID, jobID int64, path string) (io.ReadCloser, error)
  ```

  A 401 or 403 from any of these returns an error that matches `gitlab.ErrForbidden`, a new sentinel. A 404 on an artifact returns `gitlab.ErrNotFound`, or the existing sentinel if there is one.

- [ ] **Step 1: Write the failing tests**

These use `httptest` servers, as `TestEnsureHookCreates` does.

`gitlab/ci_test.go`:
- `TestPipelinesForSHAQuery`: `sha=` is sent, and pagination is followed.
- `TestPipelineJobsIncludesBridges`: both `/jobs` and `/bridges` are read and merged.
- `TestJobTraceStreams`: the body isn't buffered whole. A 3 MiB body passes through without hitting `maxResponse`.
- `TestJobArtifactPathEscaped`: `reports/cycode.json` reaches `/jobs/7/artifacts/reports/cycode.json`.
- `TestEnsureHookCIEvents`: the body has `pipeline_events: true` and `job_events: true`, on both create and update. Extend the existing `TestEnsureHookNoteEvents` pattern.

`gitlabforge/ci_test.go`:
- `TestReadCIMergedResultsNeedsHeadParent`: a merge-ref pipeline whose commit's `parent_ids` hold the head counts. One without the head is ignored. A stale `head_pipeline` alone never counts.
- `TestReadCINewestAttemptWins`: two jobs named `test`, ids 5 (failed) and 9 (passed), give `Passed`.
- `TestReadCIMissingAndFinished`: no job named `lint` while every pipeline has ended gives `Missing` with `Finished` true. While one is `running` it gives `Missing` with `Finished` false.
- `TestReadCIBridgeStatus`: a trigger job named `deploy-child` takes the bridge's status.
- `TestReadCIWithText`:
  - `log` on a failed job fills `Log` through `Tail`;
  - `log` on a passed job leaves it empty;
  - `report` fills `Report` through `Cap`;
  - an artifact 404 gives `Unreadable` with reason `error`;
  - a 403 gives `Unreadable` with reason `no permission`;
  - `withText=false` makes no trace or artifact requests.
- `TestReadCIForkPipelineMissing`: no pipelines in the target project for the head gives `Missing` with `Finished` true, so the snapshot is `Done()` at once.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/gitlab/ ./internal/forge/gitlabforge/ -run 'CI|Pipeline|JobTrace|JobArtifact|EnsureHook'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- **The streaming helper:** `func (c *Client) stream(ctx context.Context, path string, query url.Values) (io.ReadCloser, error)` sends the same auth headers as `do`, maps statuses to the sentinels, and returns the open body.
- **`gitlabforge.Forge.ReadCI`:**
  1. Collect the counting pipelines: `PipelinesForSHA`, plus each `MRPipelines` entry with `Ref == "refs/merge-requests/<iid>/merge"` whose `CommitParents(SHA)` holds the head.
  2. Read `PipelineJobs` for each.
  3. Per chosen name, take the job with the highest id.
  4. Map it with `ci.FromGitLab`.
  5. `Finished` is true when every counting pipeline's status maps to ended.
- **`RecentCINames`:** the job names from `RecentPipelines(ctx, id, 10)`, unique and sorted.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/gitlab/ ./internal/forge/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitlab internal/forge
git commit -m "feat(gitlab): read CI jobs for a head, and send pipeline and job events"
```

### Task 5: GitHub reader, permissions and project flags

**Files:**
- Modify:
  - `internal/github/client.go`: next to the check-run code ~L977;
  - `internal/github/app.go`: `InstallationInfo` ~L184/217;
  - `internal/forge/githubforge/githubforge.go`;
  - `internal/engine/reconcile.go` ~L378-412.
- Create: `internal/github/ci.go`, `internal/github/ci_test.go`, `internal/forge/githubforge/ci.go`, `internal/forge/githubforge/ci_test.go`
- Test: `internal/github/app_test.go`

**Interfaces:**
- Consumes: `forge.CIReader`, `forge.CITarget` (Task 4), the `ci` package (Task 3), `Store.SetProjectCIAccess` (Task 2).
- Produces:

  ```go
  type CheckRun struct { ID int64; Name, Status, Conclusion string; AppID int64; AppSlug string; Title, Summary, Text string }
  type CheckSuite struct { ID int64; Status string; RunCount int; AppID int64 }
  func (c *Client) CheckRuns(ctx context.Context, owner, repo, sha, name string) ([]CheckRun, error) // check_name query-encoded, filter=latest
  func (c *Client) CheckSuites(ctx context.Context, owner, repo, sha string) ([]CheckSuite, error)
  func (c *Client) CheckRunAnnotations(ctx context.Context, owner, repo string, id int64, max int) ([]string, error)
  func (c *Client) ActionsJobRunID(ctx context.Context, owner, repo string, jobID int64) (int64, error)
  func (c *Client) JobLogs(ctx context.Context, owner, repo string, jobID int64) (io.ReadCloser, error) // follows the redirect
  func (c *Client) RunArtifactFile(ctx context.Context, owner, repo string, runID int64, artifact, path string) (string, error) // zip ≤ 20 MiB, reads one file, ≤ 2 MiB
  func (c *Client) RecentCheckRunNames(ctx context.Context, owner, repo string, shas []string) ([]string, error)
  // InstallationInfo gains Events []string, and:
  func (i InstallationInfo) ActionsRead() bool   // nil Permissions → true, as AdministrationRead
  func (i InstallationInfo) HasEvent(name string) bool // nil Events → true
  ```

- [ ] **Step 1: Write the failing tests**

- **`TestCheckRunsQuery`:** the name `test (1.22)` arrives as `check_name=test+%281.22%29` (or `%20`) with `filter=latest`. Exact match is kept, so `test` doesn't match `test (1.22)`.
- **`TestJobLogsFollowsRedirect`:** a 302 to a second server is followed, and the auth header isn't sent to the second host.
- **`TestRunArtifactFile`:**
  - it reads `cycode.json` out of a zip;
  - a missing file gives `ErrNotFound`;
  - a zip over 20 MiB gives an error;
  - a path to a directory entry gives `ErrNotFound`.
- **`TestInstallationInfoEvents`** (in `app_test.go`, mirroring `TestInstallationInfoPermissions`): `events` are parsed, `ActionsRead` is true for `read` and `write`, and `HasEvent("check_run")` works.

`githubforge/ci_test.go`:
- `TestReadCIAllRunsWithNameMustPass`: two `test` runs from different suites, one `success` and one `failure`, give `Failed`.
- `TestReadCINeutralPasses`.
- `TestReadCIIgnoresOwnApp`: a check run named `pergent` from the review App's id is never read.
- `TestReadCIFinishedIgnoresEmptySuites`: a `queued` suite with `RunCount == 0` doesn't stop `Finished`. A queued suite with runs does.
- `TestReadCIActionsLogAndReport`: an `app.slug == "github-actions"` run with `log` gives `JobLogs` through `ci.Tail`. With `report` it goes `ActionsJobRunID`, then `RunArtifactFile`, then `ci.Cap`.
- `TestReadCIOtherAppOutputAsLog`: a non-Actions run gives `Title`, `Summary`, `Text` and up to 50 annotations as `Log`. `report` on it gives `Unreadable` with reason `error`.
- `TestReadCIActionsForbidden`: a 403 on the logs gives `Unreadable` with reason `no permission`.

In `internal/engine`, a reconcile test beside the existing `ProtectionReadable` test: `SetProjectCIAccess` gets `ActionsRead()` and `HasEvent("check_run")`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/github/ ./internal/forge/githubforge/ ./internal/engine/ -run 'CheckRun|JobLogs|Artifact|InstallationInfo|ReadCI|Reconcile'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- **`githubforge.Forge.ReadCI`:** for each chosen name, `CheckRuns`. Skip runs whose `AppID` is one of pergent.sh's own App ids: pass them in through `NewForge`, or read them from the `github.App` it holds. Every remaining run must map to `Passed` for the job to be `Passed`. Otherwise take the first non-passed one in the spec's table order: `running`, `failed`, `cancelled`, `skipped`. Zero runs is `Missing`.
- **`Finished`:** every `CheckSuites` entry with `RunCount > 0` is `completed`.
- **`RecentCINames`:** `RecentCheckRunNames` over `t.RecentHeads`.
- **Zero check suites on the head** means `Finished` is true. Add that case to `TestReadCIFinishedIgnoresEmptySuites`.
- **Reconcile:** call `SetProjectCIAccess` where `ProtectionReadable` is set.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/github/ ./internal/forge/... ./internal/engine/`
Expected: PASS (engine e2e tests SKIP).

- [ ] **Step 5: Commit**

```bash
git add internal/github internal/forge internal/engine/reconcile.go internal/engine/*reconcile*_test.go
git commit -m "feat(github): read check runs, Actions logs and artifacts for a head"
```

### Task 6: The input and the Job

**Files:**
- Modify: `internal/inputs/inputs.go` (`Inputs` ~L42, `Size` ~L304, `Fit` ~L317), `internal/kube/job.go` (env ~L298-325, the volume items ~L391, `BuildConfigMap` ~L460)
- Test: `internal/inputs/inputs_test.go`, `internal/kube/job_test.go`

**Interfaces:**
- Produces: `Inputs.CIResults []byte`, the ConfigMap key `ci-results.md`, and the env var `PERGENT_CI_RESULTS=/in/ci-results.md`, set only when `len(CIResults) > 0`.

- [ ] **Step 1: Write the failing tests**

- `TestSizeCountsCIResults`.
- `TestFitDropsCIResultsFirst`: over the limit, `CIResults` goes before `Dismissals`, and `Dismissals` survives when dropping `CIResults` is enough.
- `TestConfigMapItems`: extend the existing test to cover `ci-results.md` present only with content.
- `TestBuildJobCIResults` (mirror `TestBuildJobDescription`): the env var is present with content and absent without it.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/inputs/ ./internal/kube/ -run 'CIResults|Fit|ConfigMapItems'`
Expected: FAIL.

- [ ] **Step 3: Implement**, mirroring `Description`: a field at the end of the struct, a first drop in `Fit`, a `hasCI` beside `hasDescription`, and the data key.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/inputs/ ./internal/kube/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/inputs internal/kube
git commit -m "feat(kube): pass ci-results.md to the review Job"
```

### Task 7: The CI wait, dispatch and the sweep

**Files:**
- Create: `internal/engine/ci.go`, `internal/engine/ci_test.go`, `internal/engine/e2e_ci_test.go`
- Modify: `internal/engine/dispatch.go` (after the quota gate ~L309; the inputs ~L405-470), `internal/engine/engine.go` (tickers ~L247-256, `Run` ~L290-380)

**Interfaces:**
- Consumes: `Effective.CIJobs`, the store methods (Task 2), `forge.CIReader` (Tasks 4–5), `ci.Snapshot.Done/Results`, `ci.Build` (Task 3), `Inputs.CIResults` (Task 6).
- Produces:

  ```go
  func (d *dispatcher) gateCI(ctx context.Context, plat Platform, repo settings.Effective) (held bool, err error)
  func (d *dispatcher) addCIResults(ctx context.Context, plat Platform, repo settings.Effective, in *inputs.Inputs)
  func (e *Engine) sweepCI(ctx context.Context)
  func (e *Engine) checkCIRun(ctx context.Context, run store.Run) error // release a ci_pending run, or re-decide a published one
  const ciInterval = 2 * time.Minute
  const ciRecheckFor = 24 * time.Hour
  func ciTarget(run store.Run, plat Platform) forge.CITarget
  func waitingText(jobs []store.CIJob) string // "Waiting for CI: test, cycode-scan"
  ```

- [ ] **Step 1: Write the failing tests**

`ci_test.go` is unit-level, with a fake `CIReader`:
- `TestWaitingText`.
- `TestGateCI`:
  - no jobs means not held and no read;
  - all ended means `MarkCIChecked` and not held;
  - a running job means `HoldForCI` with deadline `CreatedAt + Wait()`, and `announceHold` with `waitingText`;
  - a reader error means held, not failed;
  - a forge without `CIReader` means not held, and every job reads `unreadable` (`error`) at dispatch;
  - an API run (`Kind == api`) is never gated.
- `TestAddCIResults`: it reads with `withText=true`, and `in.CIResults == ci.Build(snap)`. `SetRunCI` gets `snap.Results()`. A reader error gives every job `Unreadable` with reason `error`, and the file is still written.

`e2e_ci_test.go` is DB-gated, mirroring `TestUsedUpRepoQuotaHoldsPush` and `TestCheckedRunNotHeld`:
- `TestCIWaitHoldsThenReleasesWhenDone`.
- `TestCIDeadlineReleases`, including a deadline already past when the sweep first runs after start.
- `TestCIJobsEmptiedReleases`.
- `TestStartReviewSkipsCIWait`.
- `TestPushSupersedesCIPending`.
- `TestStartedUntrustedRunChecksCIOnce`: no loop through `postpone`.
- `TestNoCIJobsUnchanged`: with no jobs, the statuses and JobParams equal today's.
- `TestCIResultsDroppedByFitWarns`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/engine/ -run 'CI|WaitingText'`
Expected: FAIL to compile, then the unit tests FAIL. The e2e tests skip.

- [ ] **Step 3: Implement**

- **The dispatch gate**, after the quota gate:

  ```go
  if len(repo.CIJobs.Jobs) > 0 && !run.CIChecked && run.Kind != "api" { // use the store's existing constant for the API kind
      if held, err := d.gateCI(ctx, plat, repo); held || err != nil { return err }
  }
  ```
- **`addCIResults`** runs beside `AddDescription`, only when jobs are chosen, and before `Fit`.
  - It sets `in.CIResults = []byte(d.redact(string(ci.Build(snap))))`, so the LLM keys `Registry.Redact` knows are removed too.
  - After `Fit`, `hadCI && in.CIResults == nil` logs a warning.
- **`sweepCI`:**
  1. For each `CIPendingRuns` run: look up the repository's settings. With no jobs, `ReleaseCI`. Otherwise read status-only. If `Done()` or now ≥ deadline, `ReleaseCI`, then wake the dispatcher.
  2. For each `CIRecheckRuns(now - ciRecheckFor)` run, call `checkCIRun`.
- **`checkCIRun` on a published run:** read status-only and compare `Name`/`Result`/`Reason` against `run.CIJobs`. If changed, `SetRunCI`, then `RepublishNewest`. Task 8 makes publish use it.
- **Errors** are logged with `e.log().Error`, as `sweepApprovals` does.
- **The ticker:** `ci := time.NewTicker(ciInterval)` in `Run`.

- [ ] **Step 4: Run them to verify they pass**

Run: `go vet ./... && go test ./internal/engine/`
Expected: PASS (e2e SKIP).

- [ ] **Step 5: Commit**

```bash
git add internal/engine
git commit -m "feat(engine): wait for chosen CI jobs, pass their results, sweep every 2 minutes"
```

### Task 8: The gate, the CI line and publish

**Files:**
- Modify:
  - `internal/policy/policy.go`: `Outcome` ~L132, the `Kind` consts ~L165, `Decide` ~L196, `renderOptions` ~L361, `ErrorComment` ~L559 and its call at ~L237;
  - `internal/engine/publish.go`: `outcome` ~L554, `decisionOf` ~L495, `upsertComment` ~L596;
  - `internal/store/runs.go`: `OutcomeCI` beside `OutcomeBlocking` ~L183;
  - `internal/dashboard/outcome.go`.
- Test: `internal/policy/policy_test.go`, `internal/engine/publish_test.go`

**Interfaces:**
- Consumes: `ci.Line`, `ci.Blocking` (Task 3), `Run.CIJobs` (Task 2), `render.Options.HeaderNote` (Plan A).
- Produces:
  - `Outcome.CI []store.CIJobResult`;
  - `KindCI Kind = "ci"`;
  - `store.OutcomeCI = "ci_blocked"`;
  - `ErrorComment(reason, ciLine string) string`;
  - the status description `CI blocks approval: <name> <result>`, then `, +N more` when there are several.

- [ ] **Step 1: Write the failing tests**

`policy_test.go`, a table `TestDecideCI`:

| Case | Kind | Status | Approve | Comment |
|---|---|---|---|---|
| clean review, blocking `test` failed | `KindCI` | failed | false | holds `_CI: test failed (blocks approval)_` after the header |
| clean review, blocking job passed | `KindApprove` | (today's) | true | holds the line |
| clean review, non-blocking job failed | `KindApprove` | (today's) | true | holds the line |
| blocking findings and a blocking CI failure | `KindBlocking` | failed | false | holds the line |
| failure reason (error comment) and a blocking CI failure | `KindFailed` | failed | false | holds the line; status failed even with `failure_status: neutral` |
| draft and a blocking CI failure | `KindCI` | failed | false | |
| approval off and a blocking CI failure | `KindCI` | failed | false | |
| no CI jobs | today's for every case | | | byte-identical comment (no `HeaderNote`) |

Each blocking-failure case checks the status description, for example `CI blocks approval: test failed`, and `, +1 more` when two jobs fail.

`TestErrorCommentCILine` covers both an empty and a set line.

In `publish_test.go`:
- `TestDecisionOfKindCI` records `OutcomeCI`.
- `TestOutcomeCarriesRunCI` checks that `outcome(run)` sets `Outcome.CI = run.CIJobs`.
- `TestUpsertSkipsIdenticalBody`: when the found comment's body equals the new body, `UpdateComment` isn't called.
- DB-gated, `TestCIRedecideAfterPublish`:
  - a blocking job turning `passed` approves;
  - turning `failed` withdraws the approval;
  - an unchanged result makes no comment update.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/policy/ ./internal/engine/ -run 'CI|ErrorComment|DecisionOf|Upsert'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- **`Decide`:** after `decide`, if `ci.Blocking(o.CI)` isn't empty:
  - set `Approve = false` and `Status = StatusFailed`;
  - set `Description` to the CI text;
  - turn `KindApprove`, `KindNotApproved` and `KindDraft` into `KindCI`, and keep `KindBlocking` and `KindFailed`.

  This applies after the `failure_status` mapping, so neutral can't hide it.
- **`renderOptions`:** `HeaderNote = ci.Line(o.CI)`.
- **The error path:** `ErrorComment(hide(o.FailureReason), ci.Line(o.CI))`. It writes the line under its header in the same `_…_` form.
- **`upsertComment`:** compare against the body `FindComment` returned. If the forge's comment type has no body, add one there.
- **`dashboard/outcome.go`:** add a label for `ci_blocked` ("Blocked by CI") and for the waiting reason `ci_pending` ("Waiting for CI").

- [ ] **Step 4: Run them to verify they pass**

Run: `go vet ./... && go test ./internal/policy/ ./internal/engine/ ./internal/dashboard/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/policy internal/engine internal/store/runs.go internal/dashboard/outcome.go
git commit -m "feat(policy): a blocking CI job holds approval back; the CI line in every comment"
```

### Task 9: Webhook events

**Files:**
- Modify: `internal/webhook/gitlab.go` (`Handler` ~L85, the event gate ~L205), `internal/webhook/github.go` (`GitHubHandler` ~L51, the gate ~L205), `cmd/pergent-sh/` (where the handlers are built: set `CI`), `internal/engine/ci.go`
- Test: `internal/webhook/gitlab_test.go`, `internal/webhook/github_test.go`, `internal/engine/ci_test.go`

**Interfaces:**
- Consumes: `Engine.checkCIRun`, `Store.CIRunsForHead` (Tasks 2, 7).
- Produces:

  ```go
  // webhook
  type CINotifier interface { CIChanged(instance, project, head string) }
  // Handler.CI and GitHubHandler.CI, of type CINotifier; nil ignores CI events
  const pipelineHookEvent = "Pipeline Hook"; const jobHookEvent = "Job Hook"; const githubCheckRun = "check_run"
  // engine
  func (e *Engine) CIChanged(instance, project, head string) // non-blocking: queue on a buffered channel (cap 256, drop + log when full); Run's loop calls checkCIRun for each CIRunsForHead row
  ```

- [ ] **Step 1: Write the failing tests**

- **`TestPipelineHookNotifies` / `TestJobHookNotifies`:** a repository with `ci_jobs` calls `CIChanged("main", "group/project", sha)`. Without `ci_jobs` there is no call. A bad token gives 401 as today, and an unknown project is ignored.
- **`TestGitHubCheckRunNotifies`:** it uses `repository.full_name` and `check_run.head_sha`, and a fork (empty `pull_requests`) still notifies. An event whose `check_run.app.id` is the review or approve App's id is ignored.
- **`TestCIChangedNeverBlocks`:** 300 calls with no reader return at once, and the extra ones are logged.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/webhook/ ./internal/engine/ -run 'Hook|CheckRun|CIChanged'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- **GitLab:** read the project's `path_with_namespace` from `project` in both payloads. For a Job Hook without it, fall back to `project_id` through `h.Projects`. Read the sha from `object_attributes.sha` (Pipeline) or `sha` (Job). Look up the repository with `lookup(...)`. Return 200 in every accepted case.
- **GitHub:** read the App ids from `h.Config`'s instance `ReviewApp.AppID` and `ApproveApp.AppID`.

- [ ] **Step 4: Run them to verify they pass**

Run: `go vet ./... && go test ./internal/webhook/ ./internal/engine/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webhook internal/engine cmd/pergent-sh
git commit -m "feat(webhook): CI events re-check waiting and published runs"
```

### Task 10: Dashboard API, readiness and web

**Files:**
- Modify:
  - `internal/dashboard/api_read.go`: `settingViews` ~L961, and the run detail view;
  - `internal/dashboard/api_write.go`: `parseSettings` ~L768, `resetSettings` ~L687;
  - `internal/dashboard/view.go`;
  - `internal/dashboard/api_linked.go`: add the names handler beside `linkable` ~L260, and its route where `linkable` is registered;
  - `internal/engine/readiness_settings.go`;
  - `openapi.yaml`;
  - `web/src/api.ts`, `web/src/pages/RepositorySettings.tsx`, and the review page component that shows a run.
- Create: `web/src/components/CIJobsEditor.tsx`, `web/src/components/CIJobsEditor.test.tsx`, `internal/dashboard/api_ci.go`, `internal/dashboard/api_ci_test.go`
- Test: `web/src/pages/RepositorySettings.test.tsx`, `web/src/test/fixtures.ts`, `internal/engine/readiness_settings_test.go`

**Interfaces:**
- Consumes: `store.CIJobs`, `ci.Validate`, `CIReader.RecentCINames`, `Run.CIJobs`, `Run.CIDeadline`.
- Produces:
  - the settings key `ciJobs`, which reads as `SettingView<CIJobsView>` with `CIJobsView{wait: string /* "30m" */, jobs: CIJobView[]}`, where a job has `name`, `read`, `report?` (with `artifact?` and `path`) and `blocksApproval`;
  - writes take the same shape and are checked with `ci.Validate`, giving 400 `msgBadSettings`;
  - `null` clears it;
  - the run view adds `ciJobs: CIJobResultView[] | null` and `ciDeadline: string | null`;
  - `GET .../ci-job-names` returns `{"names": string[]}`, with maintainer access, as `linkable`.

- [ ] **Step 1: Write the failing tests**

Go (`api_ci_test.go`, DB-gated as `helpers_test.go` requires):
- `TestCIJobsSettingReadWrite`: a maintainer can write and a viewer gets 403. An invalid body gets 400. Config wins and is read-only.
- `TestResetClearsCIJobs`: a maintainer's reset clears it.
- `TestCIJobNames`: the fake reader's names are returned, and a forge without `CIReader` returns `[]`.
- `TestRunViewCIJobs`: the snapshot is shown, and the JSON holds no `log` or `report` text field.

`readiness_settings_test.go` (mirror `TestSettingsGitHub`):
- A GitHub repository with `ci_jobs` and `ActionsReadable=false` gets an item naming `Actions: Read`.
- `CheckRunEvents=false` gets an item naming the `Check run` event.
- With no `ci_jobs`, there are no items.

Web (vitest), with names starting `describe("RepositorySettings: CI jobs")`:
- the editor adds, edits and removes a job;
- `read: report` shows a path field, plus an artifact field on GitHub;
- the combo box lists `/ci-job-names` suggestions and accepts free text;
- a name not in the suggestions shows the "not seen in recent runs" warning;
- the model-provider warning shows beside `log` and `report`, and not for `status`;
- saving sends `ciJobs`;
- a config-set value is read-only.

On the review page:
- each job shows its result, read, size and cut;
- a `ci_pending` run shows "Waiting for CI" with its deadline.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/dashboard/ ./internal/engine/ -run 'CI|Readiness|Settings' && cd web && npx vitest run -t "CI"`
Expected: FAIL.

- [ ] **Step 3: Implement**

- **The dashboard API:** follow the `linkedReposSetting` pattern (strict decode) and the `commentLayout` view pattern. The reset clears `ciJobs` outside the admin-only block.
- **The names handler** fills `CITarget.RecentHeads` from the head SHAs of the repository's last 10 runs in the store.
- **openapi:** add the `ciJobs` schema next to `checkDeleted`, add it to both required-key lists, and document the names path.
- **The editor:** reuse the card and `Setting` and `Badge` components the linked-repos card uses.
- **Copy:**
  - the model-provider warning: "CI output goes to the model provider. Secrets are removed first, but only the shapes pergent.sh knows."
  - the unknown-name warning: "Not seen in the last 10 runs. Check the name."

- [ ] **Step 4: Run every gate**

Run: `go vet ./... && go test ./... && cd web && npm run typecheck && npm run lint && npx vitest run`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard internal/engine/readiness_settings*.go openapi.yaml web/src
git commit -m "feat(dashboard): CI jobs setting, name suggestions, results on the review page"
```

### Task 11: Docs

**Files:**
- Create: `docs/concepts/ci-jobs.md`
- Modify:
  - `README.md`: a Features line under **Reviews**;
  - `docs/README.md`;
  - `docs/reference/configuration.md`: a `repositories[].ci_jobs` block;
  - `docs/setup/github.md`: the App's permissions and events;
  - `docs/setup/gitlab.md`: the hook events are managed;
  - `docs/upgrading.md`: a new `## Unreleased` section at the top;
  - `docs/concepts/dashboard.md`: the `Blocked by CI` outcome and the `Waiting for CI` state.

- [ ] **Step 1: Write the concept page.** Its sections:
  - **What it does.**
  - **Choosing jobs:** the three `read` kinds, `blocks_approval`, and `wait`.
  - **Waiting:** every chosen job is waited for; it ends when every job ends, or the head's CI finishes, or at the deadline; Start review skips it.
  - **The gate:** strict, and the status turns red even on drafts.
  - **What the model sees:** the caps, and data, not instructions.
  - **The model provider:** the warning.
  - **GitLab forks:** run fork pipelines in the target project, or a blocking job never passes.
  - **GitHub:** `Actions: Read` and the `Check run` event, and readiness.
  - **Limits.**

  Short, scannable bullets, as the other concept pages are.
- [ ] **Step 2: Write the other changes.**
  - The README line: `- 🧪 **[CI jobs](docs/concepts/ci-jobs.md):** reviews read the results of chosen CI jobs, and a failed one can hold approval back.`
  - The upgrading section:
    - migration `0043` only adds;
    - nothing changes until a repository sets `ci_jobs`;
    - GitHub installations must accept `actions: read` and the `check_run` event;
    - GitLab hooks gain Pipeline and Job events on the next reconcile;
    - to roll back, first remove `repositories[].ci_jobs` from the config;
    - after a rollback, `ci_pending` runs stay waiting until started or superseded;
    - it needs pergent with `--ci-results` (the version in Task 12) for the model to see the results, and an older image still gets the gate.
- [ ] **Step 3: Check the links.** Run: `grep -o '](docs/[^)]*)' README.md | sort -u` and confirm each file exists. Also run the name scan.
- [ ] **Step 4: Commit**

```bash
git add README.md docs
git commit -m "docs: CI job results as review input"
```

### Task 12: Pin pergent (blocked until the owner releases Plan A)

**Files:**
- Modify: `go.mod`, `go.sum`, `internal/config/config.go` (`defaultImage`), `internal/config/config_test.go`, `docs/reference/configuration.md` (the `jobs.image` default), `docs/upgrading.md`, `docs/concepts/ci-jobs.md` (the minimum version)

- [ ] **Step 1:** Wait for the owner's pergent release tag `vX.Y.Z`, then run `go get github.com/gopaytech/pergent@vX.Y.Z && GOFLAGS=-mod=mod go mod tidy`.
- [ ] **Step 2:** Set `defaultImage = "ghcr.io/gopaytech/pergent:vX.Y.Z"`, update its test and the docs that name it.
- [ ] **Step 3:** Run: `go vet ./... && go test ./...`. Expected: PASS.
- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum internal/config docs
git commit -m "chore: pin pergent vX.Y.Z, the first with --ci-results"
```
