# Review Job Tracing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an admin go from a failed review to its Job and pod (and back by label), keep the clone log and pod state of a failed Job, name the failed step, and fail unschedulable Jobs in 10 minutes.

**Architecture:** The Job builder (`internal/kube/job.go`) adds labels and annotations. `kube.Inspect` returns a structured `Failure` (step, reason, admin-only detail), the pod name and how far the checkout got, and applies the two 10-minute rules. The watcher (`internal/engine/watch.go`) stores step and detail with the failure, records progress, and captures the clone log and pod state before the Job goes. The dashboard serves them (masked and admin-only parts as decided) and the review page shows them.

**Tech Stack:** Go, client-go (fake clientset in tests), Postgres (pgx), React + TanStack Query + Vitest, Helm.

**Spec:** `docs/superpowers/specs/2026-11-10-review-job-tracing-design.md`

## Global Constraints

- Never start a container (no `docker`, `orbctl`, nothing that wakes OrbStack). Store, dashboard-API and engine tests that need Postgres skip locally and run in CI.
- Migrations only add. The new migration is `0036` (`0035` is taken by the token-quota branch; gaps are fine, each version is applied on its own).
- Admin = `se.Role(instance, project) >= RoleAdmin`. Admin-only fields are left out of the JSON for others (`omitempty` pointer), never sent empty.
- Everything taken from the cluster is redacted with the Job's own Secret values plus `registry().LLMKeys()`; nothing is stored when the Secret can't be read (as `captureLogWithin`).
- `OOMKilled` stays in the out-of-memory failure text.
- Labels: `pergent.sh/instance`, `pergent.sh/project-id`, `pergent.sh/mr`, `pergent.sh/head` (full 40-character SHA). Annotations: `pergent.sh/project`, `pergent.sh/mr-url`. An invalid label value is left out.
- 10 minutes for both start rules. Clone log tail 64 KB. Last 10 events of every type. Diagnostics pruned with the logs after 30 days.
- Name scan before every commit: `git grep -niE 'golabs|gojek|gopay[^t]|goto\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'` prints nothing.
- Commit with `git -c user.email=zufardhiyaulhaq@gmail.com commit`, ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. One PR, no release.

## Review Focus

1. A Job whose pod is scheduled at 9 m 59 s, then pulls its image slowly, is not failed by the Pending rule; a Job whose pod appears at 9 m 59 s is not failed by the no-pod rule. (Task 3)
2. A cluster without `list events`: the review still fails at the right step with today's detail-free reason, the pod state is kept without events, and the warning is logged once per run, not every tick. (Tasks 4, 5)
3. A linked repository's URL appears in the masked clone log neither with nor without its `.git` suffix, and a secret across the 64 KB cut is hidden whole. (Task 4)
4. A non-admin's `GET /api/runs/{id}` has no `kubernetes` key, and `GET /api/runs/{id}/diagnostics` has no `podState` key. (Task 7)
5. A run reported by the callback (error result, exit 3) whose Job then shows `Failed` still gets its diagnostics, through `captureReportedLogs`, exactly once. (Task 5)

---

### Task 1: Store: failure step, progress, diagnostics

**Files:**
- Create: `internal/store/migrations/0036_run_diagnostics.sql`, `internal/store/diagnostics.go`, `internal/store/diagnostics_test.go`
- Modify: `internal/store/runs.go` (`Run`, `runColumns`, `getRun` scan, `ReportFailure`), `internal/store/dashboard.go` (`PruneRunLogs`), `internal/callback/callback.go:34,254`, `internal/engine/dispatch.go:923`, `internal/engine/watch.go:131`, their test fakes

**Interfaces:**
- Produces:
  - `type Failure struct{ Reason, Step, Detail string }` (Step `""` means a failure before any Job: metric `other`)
  - `func (s *Store) ReportFailure(ctx context.Context, id string, f Failure, now time.Time) (bool, error)` (empty Reason still becomes `"review failed"`)
  - `Run.FailureStep, Run.FailureDetail, Run.PodName string`, `Run.CheckoutSeen int` (0 nothing seen, 1 checkout running, 2 checkout done)
  - `func (s *Store) SetRunPod(ctx context.Context, id, pod string) error`
  - `func (s *Store) AdvanceCheckoutSeen(ctx context.Context, id string, seen int) error` (only raises)
  - `type Diagnostics struct{ CloneLog, CloneLogMasked, PodState string }`
  - `func (s *Store) PutRunDiagnostics(ctx context.Context, runID string, d Diagnostics) error` (texts through `CleanText`)
  - `func (s *Store) RunDiagnostics(ctx context.Context, runID string) (Diagnostics, bool, error)`
  - `func (s *Store) CountFailedRunsAtStep(ctx context.Context, instance, project, step string, since time.Time) (int, error)`

- [ ] **Step 1: Write the migration**

```sql
ALTER TABLE runs ADD COLUMN failure_step text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN failure_detail text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN pod_name text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN checkout_seen smallint NOT NULL DEFAULT 0;

-- What a failed review Job left: its clone log (whole, and with linked
-- repositories' URLs masked) and the pod's state and events. Redacted.
CREATE TABLE run_diagnostics (
    run_id           uuid        PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
    clone_log        text        NOT NULL,
    clone_log_masked text        NOT NULL,
    pod_state        text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX run_diagnostics_created_idx ON run_diagnostics (created_at);
```

- [ ] **Step 2: Write the failing tests** in `diagnostics_test.go` (Postgres, skip without `PERGENT_SH_TEST_DATABASE_URL`):
  - `TestReportFailureStoresStepAndDetail`: `ReportFailure(id, Failure{"The clone failed (checkout exited with code 128)", "clone", ""})` then `Run` has `FailureStep == "clone"`, `FailureReason` as given, `FailureDetail == ""`.
  - `TestAdvanceCheckoutSeenOnlyRaises`: 2 then 1 leaves 2.
  - `TestRunDiagnosticsRoundTrip`: put then get equal; unknown run → `found == false`; a second put replaces.
  - `TestPruneRunLogsPrunesDiagnostics`: a row older than the cut is gone after `PruneRunLogs`.
  - `TestCountFailedRunsAtStep`: two `clone` failures within the window and one older → 2; a `review` failure not counted.

- [ ] **Step 3: Run them, expect FAIL** — `go vet ./internal/store` fails with `undefined: Failure` (and the other new names). With Postgres (CI) the tests then fail on the missing columns.

- [ ] **Step 4: Implement** the interfaces above. `ReportFailure` writes the three columns in its one `UPDATE`. `PruneRunLogs` deletes from both tables with the same cut and returns the sum. Update the three callers: dispatch passes `store.Failure{Reason: reason}`; callback's interface and its `invalid` pass `Step: "delivery"` (my call: what pergent sent couldn't be used); watcher is rewritten in Task 5, here pass `store.Failure{Reason: reason}`.

- [ ] **Step 5: Verify** — `go build ./... && go vet ./... && go test ./...` passes (Postgres tests skip).

- [ ] **Step 6: Commit** — `feat(store): record a failure's step and a failed Job's diagnostics`

### Task 2: Labels and annotations on a review Job

**Files:**
- Modify: `internal/kube/job.go` (`JobParams`, `labels`, Job/pod template/Secret/ConfigMap metadata), `internal/engine/dispatch.go:550-572`
- Test: `internal/kube/job_test.go`

**Interfaces:**
- Produces: `JobParams.Instance, ProjectPath, MRURL string`, `JobParams.ProjectID, IID int64`; consts `LabelInstance = "pergent.sh/instance"`, `LabelProjectID = "pergent.sh/project-id"`, `LabelMR = "pergent.sh/mr"`, `LabelHead = "pergent.sh/head"`, `AnnotationProject = "pergent.sh/project"`, `AnnotationMRURL = "pergent.sh/mr-url"`.

- [ ] **Step 1: Write the failing tests**
  - `TestJobLabelsNameTheChange`: params with Instance `gitlab`, ProjectID 123, IID 42, HeadSHA a 40-hex string, ProjectPath `group/sub/repo`, MRURL `https://gitlab.example.com/group/sub/repo/-/merge_requests/42`. The Job, its pod template, its Secret and its ConfigMap each have labels `pergent.sh/run`, `app.kubernetes.io/managed-by`, `pergent.sh/instance=gitlab`, `pergent.sh/project-id=123`, `pergent.sh/mr=42`, `pergent.sh/head=<40 hex>`. The Job and pod template have both annotations with the exact values.
  - `TestJobLabelLeavesOutInvalidValue`: Instance `Bad/Name` → no `pergent.sh/instance` key; the others present.

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/kube -run 'TestJobLabel'`

- [ ] **Step 3: Implement** `func labels(p JobParams) map[string]string` (today's two plus the four; each value checked with `validation.IsValidLabelValue` from `k8s.io/apimachinery/pkg/util/validation`, left out when it returns errors) and `func annotations(p JobParams) map[string]string`. Use them on all four objects. In dispatch set `Instance: run.MR.Instance, ProjectID: run.MR.ProjectID, IID: run.MR.IID, ProjectPath: run.ProjectPath, MRURL: mr.WebURL`.

- [ ] **Step 4: Verify** — `go test ./internal/kube ./internal/engine` passes.

- [ ] **Step 5: Commit** — `feat(kube): label review Jobs with the change they review`

### Task 3: Inspect names the step and applies the 10-minute rules

**Files:**
- Modify: `internal/kube/inspect.go`
- Test: `internal/kube/inspect_test.go`

**Interfaces:**
- Produces:
  - consts `StepStarting = "starting"`, `StepClone = "clone"`, `StepReview = "review"`, `StepDelivery = "delivery"`, `StepUnknown = "unknown"`
  - `type Failure struct{ Step, Reason, Detail string; Deadline bool }` (zero Reason = none; `Deadline` set for `DeadlineExceeded` so the engine can refine the step)
  - `Observation.Failure, Observation.Stuck Failure` (were `string`), `Observation.PodName string`, `Observation.Checkout int` (0/1/2 as `Run.CheckoutSeen`)
  - `func DeadlineFailure(checkout int) Failure`
  - consts `unscheduledAfter = 10 * time.Minute`, `noPodAfter = 10 * time.Minute`

- [ ] **Step 1: Write the failing tests** (fake clientset, table-driven, `now` fixed). Each asserts `Step`, `Reason`, `Detail`:

| Case | Step | Reason | Detail |
|---|---|---|---|
| Job Failed, checkout exit 128 | clone | `The clone failed (checkout exited with code 128)` | `` |
| Job Failed `DeadlineExceeded`, checkout exit 137 | clone | `The clone didn't finish before the deadline (DeadlineExceeded)` | `` |
| Job Failed `DeadlineExceeded`, checkout done exit 0, no pergent state | review | `The review didn't finish before the deadline (DeadlineExceeded)` | `` |
| Job Failed `DeadlineExceeded`, no pods | unknown | `The review job hit its deadline (DeadlineExceeded)` | `` |
| pergent container OOMKilled | review | `The review job ran out of memory (OOMKilled)` | `` |
| checkout container OOMKilled | clone | `The clone ran out of memory (OOMKilled)` | `` |
| pergent exit 4 | delivery | `The review job couldn't deliver its result (exit code 4)` | `` |
| Job Failed reason `BackoffLimitExceeded`, nothing else | unknown | `The review job failed (BackoffLimitExceeded)` | `` |
| pod waiting `ImagePullBackOff` for 6 m | starting (Stuck) | `The review couldn't start (ImagePullBackOff)` | `` |
| pod `PodScheduled=False` for 11 m, message `0/3 nodes are available: 3 Insufficient memory.` | starting (Stuck) | `The review couldn't start: no node could run it` | the message |
| same at 9 m 59 s | none | | |
| scheduled pod, containers `ContainerCreating` at 11 m | none | | |
| Job 11 m old, no pods, Job event `FailedCreate` `exceeded quota: compute-resources` | starting (Stuck) | `The review couldn't start: no pod could be created` | the event message |
| Job 9 m 59 s old, no pods | none | | |
| Job 11 m old, no pods, events forbidden | starting (Stuck) | `The review couldn't start: no pod could be created` | `` |

  Plus `TestInspectReportsPodAndCheckout`: `PodName` is the newest pod's name; `Checkout` is 1 with the checkout running, 2 when it terminated with 0. And `TestDeadlineFailure`: 0 → unknown, 1 → clone, 2 → review, each with `Deadline: true` and the texts above.

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/kube -run 'TestInspect|TestDeadlineFailure'`

- [ ] **Step 3: Implement.** Order of the `failed != nil` cases: deadline with checkout exit 137/143 → `DeadlineFailure(1)`; checkout non-zero exit → clone; OOM (step by container) → review/clone; exit 4 → delivery; `DeadlineExceeded` → `DeadlineFailure(obs.Checkout)`; default → unknown. Job events (for no-pod) are listed with field selector `involvedObject.kind=Job,involvedObject.name=<job>`, only when the no-pod rule applies; a Forbidden error leaves Detail empty. Update `watch.go` and `logs.go` to the struct fields (`obs.Failure.Reason`, `obs.Stuck.Reason`) so the build stays green; their behavior changes in Task 5. Keep `ScanFailure` as it is.

- [ ] **Step 4: Verify** — `go test ./internal/kube ./internal/engine` passes.

- [ ] **Step 5: Commit** — `feat(kube): name the step a review Job failed at, and stop unschedulable Jobs`

### Task 4: Launcher: clone log, linked URLs, pod state

**Files:**
- Modify: `internal/kube/launcher.go`
- Create: `internal/kube/diagnostics.go`, `internal/kube/diagnostics_test.go`

**Interfaces:**
- Produces:
  - `func (l *Launcher) CheckoutLog(ctx context.Context, jobName string, keepBytes int) (string, error)` (same pod choice as `Logs`, container `checkout`; `Logs` and it share one `containerLog`)
  - `func (l *Launcher) LinkedClones(ctx context.Context, jobName string) ([]LinkedClone, error)` (parsed from the checkout container's `LINKED_CLONES` env of the Job)
  - `func (l *Launcher) PodState(ctx context.Context, jobName string) (text string, eventsForbidden bool, err error)`
  - `func MaskLinked(text string, clones []LinkedClone) string`
  - const `CloneLogKeepBytes = 64 * 1024`, `podStateEvents = 10`

- [ ] **Step 1: Write the failing tests**
  - `TestMaskLinked`: text containing `https://h/g/other.git` and `https://h/g/other` → both become `linked-1`; the rest of the line (`fatal: Authentication failed for`) stays; longest URL replaced first.
  - `TestLinkedClonesFromJob`: a Job built by `buildJob` with two `LinkedClones` → the same two back.
  - `TestPodStateText`: a pod `Pending` with `PodScheduled=False` reason `Unschedulable`, a container waiting `ContainerCreating`, and 12 events (pod and Job, Normal and Warning, distinct times) → the text has the phase line, the condition line, the container line, and exactly the newest 10 events, oldest first, each `<last time RFC3339> <Type> <Reason> (x<count>): <message>`.
  - `TestPodStateEventsForbidden`: the fake's event list returns Forbidden → text has the pod lines, `eventsForbidden == true`, `err == nil`.
  - `TestCheckoutLogReadsCheckout`: the fake `LogStream` is called with container `checkout`.

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/kube -run 'TestMaskLinked|TestLinkedClones|TestPodState|TestCheckoutLog'`

- [ ] **Step 3: Implement** in `diagnostics.go`. Pod state for a Job with no pod is just its Job events. Events of the pod and Job come from two field-selector lists, merged and sorted by last timestamp.

- [ ] **Step 4: Verify** — `go test ./internal/kube` passes.

- [ ] **Step 5: Commit** — `feat(kube): read a review Job's clone log and pod state`

### Task 5: Engine: store the step, track progress, keep diagnostics

**Files:**
- Modify: `internal/engine/watch.go`, `internal/engine/logs.go`, `internal/engine/publish.go:507-510`, `internal/engine/attention.go`, `internal/engine/reconcile.go:103`
- Test: `internal/engine/watch_test.go`, `internal/engine/logs_test.go`, `internal/engine/attention_test.go`

**Interfaces:**
- Consumes: Task 1 store methods, Task 3 `Observation`/`DeadlineFailure`, Task 4 launcher methods.
- Produces: `func (e *Engine) captureLog(ctx context.Context, run store.Run, obs kube.Observation, log *slog.Logger)` and `captureLogWithin(ctx, run, obs, log, timeout)` (both now take `obs`); `attCloneFailing = "clone_failing"`; `func (e *Engine) refreshCloneFailing(ctx context.Context, instance, project string)`.

- [ ] **Step 1: Write the failing tests** (Postgres where the engine tests already need it):
  - `TestWatchStoresStepAndDetail`: an unschedulable pod past 10 m → run reported with `FailureStep == "starting"`, `FailureDetail` the scheduler message, the Job deleted, diagnostics stored with the pod state, captured before the delete.
  - `TestWatchRefinesDeadlineStep`: a run whose `CheckoutSeen` is 2 and whose Job failed `DeadlineExceeded` with no pods → step `review`, reason `The review didn't finish before the deadline (DeadlineExceeded)`.
  - `TestWatchRecordsPodAndProgress`: one tick with a running checkout → `PodName` set, `CheckoutSeen == 1`; next tick with checkout done → 2.
  - `TestDiagnosticsOnlyForFailures`: a Complete Job without linked repos → no diagnostics row; a Complete Job with a linked repo whose checkout log has `could not clone linked-1` → a row with both clone logs and an empty pod state.
  - `TestDiagnosticsMasked`: the stored `CloneLogMasked` has `linked-1` where `CloneLog` has the URL; both are redacted of the Job's git token.
  - `TestDiagnosticsOnceFromReportedRun`: a run reported by the callback, Job Failed (exit 3) → `captureReportedLogs` stores diagnostics; a second tick doesn't read the cluster again.
  - `TestEventsForbiddenWarnsOnce`: two runs' captures with Forbidden events → each logs one warning, the review outcome is unchanged.
  - `TestCloneFailingAttention`: two `clone` failures within 7 days → `clone_failing` set with `Clones of this repository failed 2 times in the last 7 days. Check the clone token's access and the network to the platform.`; with one, cleared.
  - `TestErrorResultReasonNamesStep`: an error result with message `untrusted instructions` → `rd.Reason == "The review failed: untrusted instructions"`; the comment is unchanged.

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/engine -run 'TestWatchStores|TestWatchRefines|TestWatchRecords|TestDiagnostics|TestEventsForbidden|TestCloneFailing|TestErrorResultReason'`

- [ ] **Step 3: Implement.**
  - `watch`: after `Inspect`, record `PodName` (when it differs) and `AdvanceCheckoutSeen(max(run.CheckoutSeen, obs.Checkout))`. A `Failure` with `Deadline` is replaced by `kube.DeadlineFailure(max(run.CheckoutSeen, obs.Checkout))`. Stuck: `captureLog` then `Delete`. Disappeared → `Failure{Reason: reasonDisappeared, Step: StepUnknown}`; finished without a result → `StepDelivery`. Pass `store.Failure{Reason, Step, Detail}` to `ReportFailure`. After a `clone` failure call `refreshCloneFailing`; keep the `OOMKilled` substring check.
  - `captureLog`: after the pergent log, when `obs.Failure.Reason != "" || obs.Stuck.Reason != ""`, or the run has linked repos and the clone log has a line starting `could not clone `, store `Diagnostics` (clone log via `CheckoutLog(CloneLogKeepBytes + logMargin(hide))`, cut as the pergent log is, masked with `MaskLinked`; pod state only for a failure or stuck). The tries map keys the diagnostics apart from the log (`"diag:" + run.ID`). `cleanUp` passes a zero `Observation`.
  - `publish.go`: prefix the error result's message with `The review failed: ` in `rd.Reason`.
  - `reconcile.go`: call `refreshCloneFailing` beside `refreshOutOfMemory`.

- [ ] **Step 4: Verify** — `go test ./internal/engine ./internal/callback` passes.

- [ ] **Step 5: Commit** — `feat(engine): keep a failed review Job's clone log and pod state`

### Task 6: `step` label on the no-result metric

**Files:**
- Modify: `internal/store/usage.go` (`MetricsTotals`, `UnfinishedTotal`), `cmd/pergent-sh/metrics.go:131-135`
- Test: `internal/store/usage_test.go`, `cmd/pergent-sh/metrics_test.go`

**Interfaces:**
- Produces: `type NoResultTotal struct{ Instance, Project, Step string; Reviews int64 }`, `MetricsTotals.NoResult []NoResultTotal`; `UnfinishedTotal` keeps `Superseded` only.

- [ ] **Step 1: Write the failing tests**
  - `TestMetricsTotalsNoResultByStep` (Postgres): failures with steps `clone`, `clone`, `` → rows `{clone, 2}` and `{"", 1}`.
  - `TestNoResultMetricHasStep`: snapshot sample for `{Step: ""}` has `step="other"`, for `{Step: "clone"}` `step="clone"`, beside `platform_instance` and `repository`.

- [ ] **Step 2: Run, expect FAIL** — `go test ./cmd/pergent-sh -run TestNoResultMetricHasStep`

- [ ] **Step 3: Implement** a separate grouped query with today's no-result filter, grouped by `failure_step`.

- [ ] **Step 4: Verify** — `go test ./cmd/pergent-sh ./internal/store` passes.

- [ ] **Step 5: Commit** — `feat(metrics): split reviews with no result by the step that failed`

### Task 7: Dashboard API

**Files:**
- Modify: `internal/dashboard/view.go` (`RunDetailView`), `internal/dashboard/api_read.go` (`run`, new `runDiagnostics`, route beside `/api/runs/{id}/log`), `openapi.yaml`, `internal/dashboard/openapi_test.go`
- Test: `internal/dashboard/api_read_test.go`

**Interfaces:**
- Produces:
  - `type KubernetesView struct{ Namespace, Job, Pod, Detail string }` with JSON `namespace`, `job`, `pod`, `detail`; `RunDetailView.Kubernetes *KubernetesView` `json:"kubernetes,omitempty"` (admins only, set when `run.JobName != ""`; `Pod` may be empty; `Detail` is `FailureDetail`)
  - `GET /api/runs/{id}/diagnostics` → `DiagnosticsView{CloneLog string "cloneLog"; PodState *string "podState,omitempty"}`; admins get `CloneLog` and `PodState`, others `CloneLogMasked` and no `PodState`; 404 when no row, or the run isn't visible.

- [ ] **Step 1: Write the failing tests** (Postgres): `TestRunKubernetesAdminOnly` (admin sees namespace from `Config.Jobs.Namespace`, job, pod, detail; a viewer's raw JSON has no `kubernetes` key), `TestRunDiagnosticsMaskedForViewer` (viewer gets the masked text and no `podState` key; admin gets both), `TestRunDiagnosticsNotFound`. Add `KubernetesView` and `DiagnosticsView` to `openAPISchemas` and the route to the route list.

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/dashboard -run 'TestOpenAPI'`

- [ ] **Step 3: Implement** and document both in `openapi.yaml`.

- [ ] **Step 4: Verify** — `go test ./internal/dashboard` passes.

- [ ] **Step 5: Commit** — `feat(dashboard): show admins a review's Job and pod, and a failed Job's diagnostics`

### Task 8: Review page

**Files:**
- Modify: `web/src/api.ts` (types, `apiRunDiagnostics`), `web/src/pages/ReviewDetail.tsx` (`Log` tab), `web/src/test/fixtures.ts`
- Test: `web/src/pages/ReviewDetail.test.tsx`

- [ ] **Step 1: Write the failing tests**
  - With `kubernetes` on the run: a "Kubernetes" block shows namespace, Job and pod, the four commands exactly as in the spec with values filled in, each with a copy button, the detail when present, and the note `The Job is deleted an hour after it finishes; after that only what was kept here is left.` Without a pod, the two pod commands are left out.
  - Without `kubernetes`: no such block.
  - Logs tab: the pergent log as today, then a "Clone log" section from `/api/runs/{id}/diagnostics`, and a "Pod state" section when `podState` is present; a 404 on diagnostics shows nothing extra.

- [ ] **Step 2: Run, expect FAIL** — `cd web && npx vitest run src/pages/ReviewDetail.test.tsx`

- [ ] **Step 3: Implement.** Model text renders as text (`<pre>`), never HTML.

- [ ] **Step 4: Verify** — `cd web && npm run typecheck && npm run lint && npx vitest run` passes.

- [ ] **Step 5: Commit** — `feat(web): show a review's Job, pod, clone log and pod state`

### Task 9: Chart and docs

**Files:**
- Modify: `charts/pergent-sh/templates/rbac.yaml`, `docs/upgrading.md`, `docs/concepts/comment-layout.md`, `docs/concepts/metrics.md`, `docs/reference/http-routes.md`, `docs/README.md`, `docs/grafana/pergent-sh.json`
- Create: `docs/setup/troubleshooting.md`

- [ ] **Step 1: RBAC.** Add `- apiGroups: [""]  resources: ["events"]  verbs: ["list"]`. Verify `helm template t charts/pergent-sh -f charts/pergent-sh/ci-values.yaml | grep -A2 'events'` shows it.

- [ ] **Step 2: Docs.**
  - `troubleshooting.md` (short): find a review's pod (the block, `kubectl get pods -l pergent.sh/project-id=<id>,pergent.sh/mr=<n>`), the steps, causes per step.
  - `upgrading.md`: new top section `## Upgrading to 0.16.0`: the labels, the step-named failure texts (comments change the next time they are updated), the `step` label on `pergent_reviews_no_result_total`, the new `list events` permission (without it the events are left out), migration `0036` only adds, no new config key, the previous release ignores the new columns and table.
  - `comment-layout.md`: a Job failure's text names its step. `metrics.md`: the `step` label and its values. `http-routes.md`: the diagnostics route. `docs/README.md`: link the new page. Grafana: one panel, `sum by (step) (increase(pergent_reviews_no_result_total[$__range]))`.
- [ ] **Step 3: Verify** — `python3 -c 'import json,sys; json.load(open("docs/grafana/pergent-sh.json"))'` succeeds; the name scan prints nothing; `go test ./...` passes.

- [ ] **Step 4: Commit** — `docs: troubleshooting a failed review, and the events permission`
