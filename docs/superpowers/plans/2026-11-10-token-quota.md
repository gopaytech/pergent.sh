# Monthly Token Quota Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An admin can give each instance and each repository a monthly token quota; a used-up quota holds automatic reviews, refuses requests by hand, and lets only an admin start a review past it.

**Architecture:** A new `internal/quota` package sums `run_usage` (input + output, current UTC month) for a repository and its instance and says whether either quota is used up. Each run carries `quota_checked`: requests by hand are checked in `internal/actions` when made (and the run they queue is marked checked); automatic runs (push, pergent.sh's own full re-run) are checked in the dispatcher just before a Job is created, after the trust check, and held in the existing `waiting` state with reason `quota_used_up`. Settings flow like trusted authors: config file or dashboard row for a repository, config or `instances` row for an instance.

**Tech Stack:** Go 1.x, Postgres (pgx), React + TypeScript + vitest, OpenAPI.

**Spec:** `docs/superpowers/specs/2026-11-10-token-quota-design.md`

## Global Constraints

- Public repository: no private host, company or group name anywhere (code, tests, docs, commits). Run the name scan before every commit: `git diff --cached | grep -i -E 'golabs|gojek|gopay[^t]'` must print nothing.
- Never run `docker`, `orbctl` or anything that starts a container. Never call `gh`, `glab`, `curl` or any external API. No push, no PR, no tag.
- Store, engine, dashboard, settings and instances tests skip silently without `PERGENT_SH_TEST_DATABASE_URL`. Use `PERGENT_SH_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable` (an already-running local container). If it doesn't answer, stop and report; don't start one.
- The previous release must keep working on the migrated database: new columns nullable or defaulted, nothing renamed or dropped, no constraint change on `runs.state` or `runs.trigger`.
- Measure: `tokens_in + tokens_out` from `run_usage`, rows with `reported_at >= ` the first instant of the current month **in UTC**.
- Quota values: whole numbers from 1 to `1000000000000000` (10^15, below JavaScript's safe-integer limit); empty/null is no quota. Zero, negative and larger values are refused (config validation, API 400, DB `CHECK (... > 0)`).
- A used-up quota: usage `>=` quota.
- Exact copy (used verbatim in code, tests and docs):
  - Status: `Monthly token quota used up. An admin can start the review in the dashboard.`
  - Note: `Not reviewed: the monthly token quota is used up. Reviews start again on the 1st, or an admin can start this one from the pergent.sh dashboard.`
  - Refusal reason (commands table and dashboard error): `the monthly token quota is used up`
  - Waiting box reason line: `Monthly token quota used up.`
- No numbers and no "repository"/"instance" in anything posted on a merge request.
- Only admins (account role `admin`, `RoleAdmin`) see or change a quota or its usage.
- Docs live in `docs/` (the root README has no feature sections); a new topic gets a line in `docs/README.md`. Changing a note or reply means changing what the docs quote.
- Go gate: `go vet ./...` and `go test -count=1 ./...` with the DB variable. Web gate (from `web/`): `npm run typecheck && npm run lint && npm test -- --run && npm run build`. In this worktree, first `ln -s ../../pergent.sh/web/node_modules web/node_modules`; afterwards remove the link and `web/tsconfig.tsbuildinfo`.

## Review Focus

1. **Server time zone isn't UTC.** A pod with `TZ=Asia/Jakarta` at 23:30 UTC on the 31st must still count the old month. Pinned by `TestMonthStartIsUTC` (Task 3).
2. **A start racing the quota hold.** A maintainer's start (`StartRun`) flips an undispatched push run while the dispatcher is about to hold it; the start must win and run. Pinned by `TestHoldForQuotaLeavesAStartAlone` (Task 1).
3. **A quota typed past JavaScript's safe integers** (`9007199254740993`) must be refused, not silently rounded. Pinned by `TestQuotaSettingBounds` (Task 6) and the config table (Task 2).
4. **A repository listed in the config file and also with a dashboard row**: the config's quota wins and the dashboard shows it read-only. Pinned by `TestConfigQuotaWins` (Task 2).
5. **An instance that isn't registered (removed) or has no quota**: unlimited, no usage query at dispatch. Pinned by `TestCheckSkipsQueriesWithoutLimits` (Task 3).

---

### Task 1: Migration and run-side store

**Files:**
- Create: `internal/store/migrations/0035_token_quota.sql`, `internal/store/quota.go`, `internal/store/quota_test.go`
- Modify: `internal/store/runs.go` (`Run`, run columns/scan, `EnqueueRerun`, `StartRun`), every caller of `EnqueueRerun` (`internal/actions/actions.go`, `internal/engine/publish.go:314`, tests)

**Interfaces:**
- Produces:
  - Migration `0035_token_quota.sql` (re-check that 0035 is still free on `origin/main`; if not, take the next): `runs.quota_checked boolean NOT NULL DEFAULT false`; `merge_requests.quota_noted_at timestamptz`; `dashboard_repos.monthly_token_quota bigint CHECK (monthly_token_quota > 0)`; `instances.monthly_token_quota bigint CHECK (monthly_token_quota > 0)`; `CREATE INDEX run_usage_instance_idx ON run_usage (instance, reported_at)`. Header comment says the previous release ignores all of it and never claims a waiting run.
  - `const WaitQuota = "quota_used_up"` next to `WaitUntrusted`.
  - `Run.QuotaChecked bool`.
  - `func (s *Store) EnqueueRerun(ctx context.Context, mr MRKey, projectPath, headSHA, targetBranch, trigger string, skills []string, baseRunID *string, replace, quotaChecked bool) (id string, created bool, err error)`: the new run's `quota_checked` is `quotaChecked`. `actions.Rerun` passes `true`; `publish.go:314` passes `false`.
  - `StartRun` sets `quota_checked = true` in all three of its paths (waiting run made queued, undispatched push run flipped to start, new start run).
  - `func (s *Store) HoldForQuota(ctx context.Context, id string) (ok bool, err error)`: `UPDATE runs SET state = 'waiting', skip_reason = 'quota_used_up' WHERE id = $1 AND state IN ('queued','running') AND dispatched_at IS NULL AND NOT quota_checked`.
  - `func (s *Store) ClaimQuotaNote(ctx context.Context, mr MRKey, monthStart, now time.Time) (bool, error)`: like `ClaimUntrustedNote`, succeeds when `quota_noted_at IS NULL OR quota_noted_at < monthStart`, sets it to `now`.
  - `func (s *Store) ReleaseQuotaNote(ctx context.Context, mr MRKey) error`: sets it NULL.
  - `func (s *Store) TokensSince(ctx context.Context, instance string, projectID int64, since time.Time) (int64, error)`: `COALESCE(sum(tokens_in),0) + COALESCE(sum(tokens_out),0)` over `run_usage` for `instance`, and `project_id` when `projectID != 0`, with `reported_at >= since`.

- [ ] **Step 1: Write the failing tests** in `internal/store/quota_test.go` (helpers from `store_test.go`/`trusted_test.go`: `newStore`, `enqueue`, `mustRun`, `mr1`, `t0`, `sha`):
  - `TestHoldForQuotaHoldsPushAndAutoRerun`: a queued push run and an `EnqueueRerun(..., TriggerRerun, nil, nil, true, false)` run both hold (`ok` true, state `Waiting`, `SkipReason == WaitQuota`); neither is returned by `ClaimQueued`.
  - `TestHoldForQuotaLeavesCheckedRuns`: a rerun enqueued with `quotaChecked=true` and a dispatched run don't hold (`ok` false, state unchanged).
  - `TestHoldForQuotaLeavesAStartAlone`: enqueue a push run, `StartRun` on its head, then `HoldForQuota` returns `ok == false` and the run is queued with trigger `start` and `QuotaChecked == true`.
  - `TestStartRunMarksQuotaChecked`: `StartRun` on a quota-held waiting run makes it queued, `TriggerStart`, `QuotaChecked == true`, `SkipReason == ""`.
  - `TestClaimQuotaNoteOncePerMonth`: claim at `2026-11-03` (month start `2026-11-01`) is true; again on `2026-11-20` false; on `2026-12-02` with month start `2026-12-01` true; after `ReleaseQuotaNote` a same-month claim is true again.
  - `TestTokensSince`: rows (via `RecordUsage`) for project 1 (in 100, out 50, reported `2026-11-01T00:00:00Z`), project 1 (in 7, out 3, reported `2026-10-31T23:59:59Z`), project 2 (in 1000, out 0, reported `2026-11-05`) on instance `main`, and one on instance `other`. `TokensSince(ctx, "main", 1, nov1) == 150`; `TokensSince(ctx, "main", 0, nov1) == 1150`; `TokensSince(ctx, "nowhere", 0, nov1) == 0`.
  - `TestMigrationKeepsPreviousReleaseQuota`: following `TestMigrationKeepsPreviousReleaseTrusted`, an insert into `runs`, `dashboard_repos`, `instances` and `merge_requests` that names none of the new columns succeeds, and `monthly_token_quota = 0` is refused by the CHECK.

- [ ] **Step 2: Run them to see them fail**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/store -run 'Quota|TokensSince'`
  Expected: FAIL (undefined `HoldForQuota`, `WaitQuota`, …).

- [ ] **Step 3: Implement** the migration and the store functions above; add `quota_checked` to the run column list and scan; update every `EnqueueRerun` caller and test with the new argument.

- [ ] **Step 4: Run the store and engine packages**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/store ./internal/engine ./internal/actions`
  Expected: PASS.

- [ ] **Step 5: Commit** `feat(store): quota columns, quota holds and monthly token sums`

### Task 2: Quota settings in config, store rows, settings and instances

**Files:**
- Modify: `internal/config/config.go` (`Instance`, `GitHubInstance`, `Repository`, `fileRepository`, `repository()`, `Validate`), `internal/config/config_test.go`; `internal/store/dashboard.go` (`DashboardRepo`, `dashboardRepoColumns`, `scanDashboardRepo`, `UpsertDashboardRepo`, `InsertDashboardRepo`, `UpdateDashboardRepo`), `internal/store/instances.go` (`InstanceRow`, `instanceColumns`, `scanInstance`); `internal/instances/instances.go` (`Instance`, `fromGitLab`, `fromGitHub`, `FromRow`); `internal/settings/settings.go` (`Effective`, keys, `defaultsFor`, `fromConfig`, `fromDashboard`); their tests.

**Interfaces:**
- Consumes: the columns from Task 1.
- Produces:
  - `config.MaxMonthlyTokenQuota int64 = 1_000_000_000_000_000`.
  - `config.Instance.MonthlyTokenQuota *int64 \`yaml:"monthly_token_quota"\``; same on `config.GitHubInstance`; `config.Repository.MonthlyTokenQuota *int64` and `fileRepository.MonthlyTokenQuota *int64 \`yaml:"monthly_token_quota"\``.
  - Validation messages: `gitlab[%d] (%s): monthly_token_quota must be from 1 to 1000000000000000, got %d`, the same with `github[%d]` and `repositories[%d]`.
  - `store.DashboardRepo.MonthlyTokenQuota *int64`; `store.InstanceRow.MonthlyTokenQuota *int64`; `func (s *Store) SetInstanceTokenQuota(ctx context.Context, name string, quota *int64) (found bool, err error)` (copy `SetInstanceTrustMode`, including `ErrRemoving`).
  - `instances.Instance.MonthlyTokenQuota *int64`; `func (r *Registry) SetTokenQuota(name string, quota *int64) (Instance, bool)` (copy `SetTrustMode`).
  - `settings.KeyMonthlyTokenQuota = "monthly_token_quota"` (present in every `Overridden` map); `Effective.MonthlyTokenQuota *int64` (the repository's own, nil = none; from config for a config repository, from the row for a dashboard one, an invalid stored value ignored); `Effective.InstanceMonthlyTokenQuota *int64` (from the registry in `defaultsFor`, nil when the instance isn't registered).

- [ ] **Step 1: Write the failing tests**
  - `TestTokenQuotaConfig` (config_test.go, with `withTrustKeys`-style helper `withQuotaKeys`): `monthly_token_quota: 5000000` on a gitlab, a github and a repository entry loads as `5000000` on each.
  - `TestTokenQuotaConfigInvalid`: table `0`, `-1`, `1000000000000001`, `9007199254740993` for each of the three places; `Validate` error contains the message above.
  - `TestDashboardRepoKeepsQuota` (store): `UpsertDashboardRepo` with `MonthlyTokenQuota` 42, `UpdateDashboardRepo` changing it to 7, then to nil, read back each time.
  - `TestSetInstanceTokenQuota` (store): set 9 and read back via `InstanceRow`; nil clears; a removing instance gives `ErrRemoving`; unknown name `found == false`.
  - `TestInstanceTokenQuota` (instances): `FromRow` copies the quota; `fromGitLab`/`fromGitHub` copy it from config; `SetTokenQuota` updates `Get`.
  - `TestTokenQuotaResolution` (settings): a dashboard repo with quota 10 on an instance with 100 gives `MonthlyTokenQuota == 10`, `InstanceMonthlyTokenQuota == 100`, `Overridden[KeyMonthlyTokenQuota] == true`; a repo without one gives nil and `false`; an unregistered instance gives `InstanceMonthlyTokenQuota == nil`.
  - `TestConfigQuotaWins` (settings): a repository in config with quota 3 and a dashboard row with 99 resolves to 3 with `Source == SourceConfig`.

- [ ] **Step 2: Run them to see them fail**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/config ./internal/store ./internal/instances ./internal/settings -run 'Quota'`
  Expected: FAIL (unknown fields).

- [ ] **Step 3: Implement** the fields, validation, columns in all four hand-written column lists, registry setter and resolution.

- [ ] **Step 4: Run the packages**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/config ./internal/store ./internal/instances ./internal/settings`
  Expected: PASS.

- [ ] **Step 5: Commit** `feat(settings): a monthly token quota per instance and repository`

### Task 3: The quota package

**Files:**
- Create: `internal/quota/quota.go`, `internal/quota/quota_test.go`

**Interfaces:**
- Consumes: `Store.TokensSince` (Task 1); `settings.Effective.MonthlyTokenQuota`, `InstanceMonthlyTokenQuota` (Task 2).
- Produces:
  - `func MonthStart(t time.Time) time.Time`: the first instant of `t`'s month in UTC.
  - `type Usage struct { Repository, Instance int64; RepositoryLimit, InstanceLimit *int64 }`
  - `func (u Usage) UsedUp() bool`: `(RepositoryLimit != nil && Repository >= *RepositoryLimit) || (InstanceLimit != nil && Instance >= *InstanceLimit)`.
  - `func Read(ctx context.Context, st *store.Store, now time.Time, instance string, projectID int64, repo settings.Effective) (Usage, error)`: both sums always (for the dashboard), limits copied from `repo`. `projectID == 0` (unknown project) gives `Repository == 0` without a query.
  - `func Check(ctx context.Context, st *store.Store, now time.Time, instance string, projectID int64, repo settings.Effective) (usedUp bool, err error)`: queries only the sums that have a limit; none when both are nil.

- [ ] **Step 1: Write the failing tests**
  - `TestMonthStartIsUTC`: `MonthStart(time.Date(2026, 12, 1, 6, 30, 0, 0, jakarta))` (06:30 at UTC+7 is 23:30 UTC on 30 November) equals `2026-11-01T00:00:00Z`; `MonthStart` of `2026-11-15T12:00:00Z` equals `2026-11-01T00:00:00Z`.
  - `TestUsedUp`: table: no limits → false; repo 100/100 → true; repo 99/100 → false; instance 500/500 with repo nil → true; repo room but instance used up → true.
  - `TestCheckSkipsQueriesWithoutLimits`: with a store opened on the test database and then closed, `Check` with both limits nil returns `false, nil` (no query made); `Read` on the same store returns an error.
  - `TestCheckCountsThisMonth` (needs DB): usage rows as in `TestTokensSince`; repo limit 150 → used up; 151 → not; instance limit 1150 → used up.

- [ ] **Step 2: Run them to see them fail**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/quota`
  Expected: FAIL (package doesn't exist).

- [ ] **Step 3: Implement** `internal/quota/quota.go` (imports `store` and `settings` only).

- [ ] **Step 4: Run the tests**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/quota`
  Expected: PASS.

- [ ] **Step 5: Commit** `feat(quota): this month's token usage against the quotas`

### Task 4: Requests by hand: actions, commands and dashboard buttons

**Files:**
- Create: `internal/engine/quota.go` (the copy constants), `internal/engine/e2e_quota_commands_test.go`
- Modify: `internal/actions/actions.go`, its tests; `internal/engine/commands.go` (`rerun`, `start`); `internal/dashboard/api_commands.go` (`rerun`, `start`), `internal/dashboard/quota_test.go` (create)

**Interfaces:**
- Consumes: `quota.Check`, `quota.MonthStart` (Task 3); `Store.ClaimQuotaNote` (Task 1).
- Produces:
  - `actions.RerunQuotaUsedUp` (new `RerunStatus`, after `RerunCapped`).
  - `RerunRequest.PastQuota bool` and `StartRequest.PastQuota bool`: true only for an admin's dashboard action.
  - `actions.Rerun` and `actions.Start`: after the "nothing failed" result and before the caps, `if !req.PastQuota` call `quota.Check(ctx, st, req.Now, req.MR.Instance, req.MR.ProjectID, req.Repo)`; used up → `RerunResult{Status: RerunQuotaUsedUp}` with nothing queued; an error is returned as the error. `Rerun` passes `quotaChecked = true` to `EnqueueRerun`.
  - In `internal/engine/quota.go`: `const quotaDescription = "Monthly token quota used up. An admin can start the review in the dashboard."`, `const quotaNote = "Not reviewed: the monthly token quota is used up. Reviews start again on the 1st, or an admin can start this one from the pergent.sh dashboard."`, `const whyQuota = "the monthly token quota is used up"`.
  - Commands: on `RerunQuotaUsedUp`, `v := refused(whyQuota, quotaNote)`; `first, err := e.Store.ClaimQuotaNote(ctx, mr, quota.MonthStart(now), now)`; when `!first` (or err, logged) blank `v.reply` and `v.replyRequired` as `refuseGate` does. `PastQuota` is always false here.
  - Dashboard: `PastQuota: session(r).Role(b.repo.Instance, b.repo.Project) == RoleAdmin`; `RerunQuotaUsedUp` → `409` with `msgQuotaUsedUp = "the monthly token quota is used up"`.
  - Trust before quota: a writer's start of an untrusted change is checked for write access first, then for the quota; refused for the quota, its run stays waiting with `WaitUntrusted`.

- [ ] **Step 1: Write the failing tests**
  - actions tests: `TestRerunRefusedOnUsedUpQuota` (repo quota 10, usage 10: status `RerunQuotaUsedUp`, no new run, `CountReruns` unchanged); `TestRerunPastQuota` (same with `PastQuota: true`: `RerunQueued`, run `QuotaChecked == true`); `TestStartRefusedOnUsedUpQuota` and `TestStartPastQuota` likewise; `TestRerunWithRoomQueuesChecked` (usage 9 of 10: queued, `QuotaChecked == true`).
  - `e2e_quota_commands_test.go` (harness `newCommandsE2E`, `say`, `answer`, `wantReactions`, `wantReplies`):
    - `TestReviewCommandRefusedOnUsedUpQuota`: a published review on the head, usage at the quota; a writer's `@bot review` gets `eyes, down`, one reply equal to `quotaNote`, outcome `refused: the monthly token quota is used up`; the published run is still `Published` and its status unchanged; no new run.
    - `TestSecondQuotaRefusalHasNoReply`: after the first refusal (note claimed), another writer's command 3 minutes later gets `eyes, down` and no new reply.
    - `TestWriterStartRefusedForQuotaKeepsTrustWait`: untrusted author's change waiting with `untrusted_author`, quota used up; a writer's `@bot review` is refused with `whyQuota`; the run is still waiting with `WaitUntrusted`.
    - `TestReviewCommandWithRoomRuns`: usage below quota: queued run with `QuotaChecked == true`.
    - `TestReviewCommandAfterTheFirstReviewsHeldHead`: a run held with `WaitQuota` in October; clock set to `2026-11-01T00:00:01Z`; a writer's `@bot review` queues a review of that head and `WaitingChanges` no longer lists the change.
  - `internal/dashboard/quota_test.go`: `TestMaintainerRerunRefusedOnUsedUpQuota` (409, body has `the monthly token quota is used up`); `TestAdminRerunPastQuota` (202 `queued`); `TestMaintainerStartRefusedOnUsedUpQuota` and `TestAdminStartPastQuota` on a quota-held change; `TestViewerCannotStart` stays 403.

- [ ] **Step 2: Run them to see them fail**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/actions ./internal/engine ./internal/dashboard -run 'Quota'`
  Expected: FAIL.

- [ ] **Step 3: Implement** the actions check, the command refusal and the dashboard mapping.

- [ ] **Step 4: Run the packages**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/actions ./internal/engine ./internal/dashboard`
  Expected: PASS.

- [ ] **Step 5: Commit** `feat: refuse review requests on a used-up token quota, except an admin's`

### Task 5: Automatic runs: the dispatcher hold

**Files:**
- Modify: `internal/engine/dispatch.go` (after the trust block at ~252-282; a `holdForQuota` method beside `hold`), `internal/engine/quota.go`
- Create: `internal/engine/e2e_quota_test.go`

**Interfaces:**
- Consumes: `quota.Check`, `quota.MonthStart`; `Store.HoldForQuota`, `ClaimQuotaNote`, `ReleaseQuotaNote`; `Run.QuotaChecked`; constants from Task 4.
- Produces: in `dispatch`, right after the trust block and before anything that mints a token or creates a Job:
  `if !run.QuotaChecked` → `quota.Check(ctx, e.Store, e.now(), run.MR.Instance, run.MR.ProjectID, repo)`; an error → `d.retry(ctx, fmt.Errorf("read the token quota: %w", err))`; used up → `d.holdForQuota(ctx, plat)`.
  `func (d *dispatcher) holdForQuota(ctx context.Context, plat Platform) error`: `HoldForQuota`; `!ok` → `d.postpone(ctx, e.now())`; `SetStatus(..., forge.StatusPending, quotaDescription)` (failure logged); `ClaimQuotaNote(..., quota.MonthStart(now), now)`; first → `Reply(..., quotaNote)`; a reply failure → `ReleaseQuotaNote`. Share the status-and-note steps with `hold` rather than copying them.

- [ ] **Step 1: Write the failing tests** (harness `newTestEnv`, `Enqueue`, `dispatch`, `Jobs`, `statusesOf`, `notes` from `e2e_trusted_test.go`; usage seeded with `Store.RecordUsage`; repo quota set on the config repo `acme/widgets` in `testConfig`'s copy):
  - `TestNoQuotaReviewsAsToday`: no quota anywhere: one Job.
  - `TestUsedUpRepoQuotaHoldsPush`: usage 100 of 100: no Job, run waiting with `WaitQuota`, statuses `["pending: " + quotaDescription]`, notes `[quotaNote]`; a second push: still no Job, a second pending status, still one note.
  - `TestUsedUpInstanceQuotaHolds`: instance quota 500 used up, repository quota 1000 with room: held.
  - `TestRoomLeftReviews`: usage 99 of 100: one Job.
  - `TestOwnFullRerunHeld`: a `TriggerRerun` run enqueued with `quotaChecked=false` (as `publish.go` does) on a used-up quota is held.
  - `TestCheckedRunNotHeld`: a run enqueued with `quotaChecked=true` dispatches on a used-up quota (overshoot by design).
  - `TestUntrustedWaitsForTrustNotQuota`: untrusted author on a public repo with a used-up quota: run waiting with `WaitUntrusted`, the trust note only.
  - `TestQuotaReadErrorRetries`: break the usage read (drop `run_usage` in the test schema): no Job, the run is requeued with an attempt counted.
  - `TestNoteAgainNextMonth`: held and noted on `2026-11-20`; on `2026-12-03` with December's usage at the quota, the next push posts a second note.
  - GitHub: `TestGitHubUsedUpQuotaHolds` with `ghTrustEnv` (trusted author, `write`): held, pending status, one note.

- [ ] **Step 2: Run them to see them fail**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/engine -run 'Quota|RoomLeft|OwnFullRerun|CheckedRun|NoteAgain'`
  Expected: FAIL.

- [ ] **Step 3: Implement** the check and `holdForQuota`.

- [ ] **Step 4: Run the engine package**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/engine`
  Expected: PASS.

- [ ] **Step 5: Commit** `feat(engine): hold automatic reviews on a used-up token quota`

### Task 6: Dashboard API, views and OpenAPI

**Files:**
- Modify: `internal/dashboard/api_write.go` (`parseSettings`, `putSettings`, `resetSettings`, `msgBadSettings`), `internal/dashboard/api_read.go` (`detailView`, `settingViews`), `internal/dashboard/view.go`, `internal/dashboard/outcome.go` (`judge`), `internal/dashboard/api_operator.go` (route, handler, `instanceView`), `openapi.yaml`; tests in `internal/dashboard/quota_test.go`

**Interfaces:**
- Consumes: Tasks 2 and 3.
- Produces:
  - Settings key `monthlyTokenQuota`: JSON integer 1…10^15 or `null`; anything else 400 `msgBadQuota = "monthlyTokenQuota must be a whole number from 1 to 1000000000000000, or null"`. `isQuotaKey(key string) bool`; non-admin → 403 `msgNeedsAdminQuota = "only admins change the token quota"`. Added to `msgBadSettings`' key list. An admin's reset clears it; a maintainer's leaves it.
  - `settingViews` gains `"monthlyTokenQuota": {repo.MonthlyTokenQuota, ov[settings.KeyMonthlyTokenQuota]}`; `detailView` removes it for non-admins (as it does `linkedRepos`).
  - `type QuotaUseView struct { Used int64 \`json:"used"\`; Limit *int64 \`json:"limit"\` }`; `type QuotaView struct { Repository QuotaUseView \`json:"repository"\`; Instance QuotaUseView \`json:"instance"\` }`; `RepositoryDetailView.Quota *QuotaView \`json:"quota"\``: filled from `quota.Read` for admins, `null` for everyone else. A failed read is logged and leaves it `null`; it never fails the page.
  - `InstanceAdminView.MonthlyTokenQuota *int64 \`json:"monthlyTokenQuota"\`` and `TokensThisMonth int64 \`json:"tokensThisMonth"\`` (`Store.TokensSince(ctx, name, 0, quota.MonthStart(now))`).
  - `d.adminRoute(mux, "PUT /api/admin/instances/{name}/token-quota", d.putInstanceQuota)`: body `{"monthlyTokenQuota": n|null}` via `decodeStrict`; bad value 400 `msgBadQuota`; config instance 409 `msgConfigInstance`; removing 409; unknown 404; `SetInstanceTokenQuota` then `registry.SetTokenQuota`; audit `instance_updated` with detail `monthly_token_quota=<n>` or `monthly_token_quota=none`; 204.
  - The admin usage pages show rolling days, not a month, so they get no quota column (the spec adds one only "where they show a month").
  - `judge`: `SkipReason == store.WaitQuota` → a new `reasonQuotaUsedUp = "the monthly token quota is used up; an admin can start the review"`.
  - `openapi.yaml`: `monthlyTokenQuota` on `RepositorySettingsRequest` ("Admins only.") and in the settings description; `QuotaView`, `QuotaUseView`; `quota` on `RepositoryDetailView` (required, nullable); the two `InstanceAdminView` fields (required); the new path (`setInstanceTokenQuota`, `x-pergent-access: admin`, request schema `InstanceTokenQuotaRequest`, 204/400/401/403/404/409/500); `quota_used_up` in `WaitingView.reason`; the `start` and `rerun` descriptions say a used-up quota gives 409 unless an admin asks.

- [ ] **Step 1: Write the failing tests** in `internal/dashboard/quota_test.go` (harness `newEnv`, `send`, `sendJSON`, `ssoAdmin`, `dashboardRepo`, `storedRepo`, `auditOf`, `getJSON`):
  - `TestQuotaSettingBounds`: admin PUT `{"monthlyTokenQuota": 5000000}` → 200 and stored; `0`, `-5`, `1.5`, `"5M"`, `9007199254740993` → 400 `msgBadQuota`; `null` → stored nil.
  - `TestQuotaSettingIsForAdmins`: a maintainer's PUT → 403 `msgNeedsAdminQuota`; the maintainer's GET has no `monthlyTokenQuota` key and `quota: null`.
  - `TestQuotaResetByAdminOnly`: maintainer reset keeps 42; admin reset clears it.
  - `TestDetailShowsQuotaToAdmins`: usage 80 of repo quota 100, instance quota nil, instance usage 120: `quota.repository == {used: 80, limit: 100}`, `quota.instance == {used: 120, limit: null}`.
  - `TestInstanceTokenQuota`: admin PUT 1000 → 204, `listInstances` shows `monthlyTokenQuota: 1000` and `tokensThisMonth`; audit `instance_updated` detail `monthly_token_quota=1000`; null → `monthly_token_quota=none`; config instance → 409; a user → 404.
  - `TestJudgeQuotaWaiting`: a waiting digest with `WaitQuota` judges to `reasonQuotaUsedUp`.
  - The existing OpenAPI tests (`TestOpenAPIRoutesMatchCode`, `TestOpenAPISchemasMatchGoTypes`, `TestOpenAPIEveryViewTypeHasASchema`) and route tests (`TestAdminRoutesAreRegisteredOnlyThroughTheHelper`, `TestEveryAdminRouteIsNotFoundForAUser`) cover the new route and types.

- [ ] **Step 2: Run them to see them fail**
  Run: `PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./internal/dashboard -run 'Quota|OpenAPI|AdminRoute'`
  Expected: FAIL.

- [ ] **Step 3: Implement** the key, views, endpoint, judge reason and `openapi.yaml`.

- [ ] **Step 4: Run the whole Go gate**
  Run: `go vet ./... && PERGENT_SH_TEST_DATABASE_URL=… go test -count=1 ./...`
  Expected: PASS.

- [ ] **Step 5: Commit** `feat(dashboard): token quota settings, usage this month and the instance endpoint`

### Task 7: Web

**Files:**
- Modify: `web/src/api.ts`, `web/src/pages/RepositorySettings.tsx`, `web/src/pages/RepositoryDetail.tsx` (`Waiting`, new `QuotaBars`), `web/src/pages/Integrations.tsx` (`InstanceCard`, new `TokenQuota`), `web/src/test/fixtures.ts`, CSS where the existing bars/badges live
- Create: `web/src/pages/TokenQuota.test.tsx`

**Interfaces:**
- Consumes: the JSON from Task 6.
- Produces:
  - `api.ts`: `WaitingView.reason` adds `"quota_used_up"`; `RepoSettings.monthlyTokenQuota?: SettingView<number | null>`; `SettingsUpdate.monthlyTokenQuota: number | null`; `QuotaUseView = { used: number; limit: number | null }`; `QuotaView = { repository: QuotaUseView; instance: QuotaUseView }`; `RepositoryDetailView.quota: QuotaView | null`; `InstanceAdminView.monthlyTokenQuota: number | null; tokensThisMonth: number`. Fixtures updated so `typecheck` passes.
  - Repository Settings: a `<Setting name="Monthly token quota">` row with an input `aria-label="Monthly token quota"`, shown only to admins (`canSetQuota={view.role === "admin"}`), disabled for config repositories; help: `Tokens per calendar month (UTC), input plus output. Empty: limited only by the instance's quota.` Input accepts digits with optional `,`/`_`/spaces (stripped); empty sends `null`; anything else shows an inline error and blocks Save; only sent when changed.
  - Repository page (admins, `view.quota` not null): `QuotaBars` shows `Tokens this month: <used> of <limit>` with a `role="meter"` bar (`aria-label="Repository token quota"`) when the repository has a limit, then the same for the instance (`aria-label="Instance token quota"`) when it has one; a side without a limit shows `Tokens this month: <used>` without a bar. Numbers with `toLocaleString("en-US")`. Bar class `warn` from 80% and `full` at 100% (reuse existing colour tokens).
  - Waiting box: intro becomes `Nobody reviews these changes until someone starts the review.`; a `quota_used_up` row shows `Monthly token quota used up.`; its Start button shows only for `view.role === "admin"` (trust rows keep admin or maintainer).
  - Integrations: each instance card gets a `TokenQuota` input (`aria-label="Monthly token quota"`) with a Save button, PUT to `/api/admin/instances/{name}/token-quota` with `{ monthlyTokenQuota }`, read-only for config instances, and the same bar as the repository page (`aria-label="Instance token quota"`).

- [ ] **Step 1: Write the failing tests** in `web/src/pages/TokenQuota.test.tsx` (`fakeApi`, `renderApp`, fixtures; copy `TrustedAuthors.test.tsx`'s `settingsRoutes`):
  - `sends the repository quota, stripped of separators`: type `5,000,000`, Save → PUT body `{ monthlyTokenQuota: 5000000 }`.
  - `sends null when emptied`: initial 10, clear, Save → `{ monthlyTokenQuota: null }`.
  - `refuses a non-number`: type `5M` → error shown, no PUT.
  - `hides the quota from a maintainer`: no `Monthly token quota` field, no meter.
  - `shows amber from 80% and red when used up`: `quota.repository {used: 80, limit: 100}` → meter has class `warn`; `{used: 100, limit: 100}` → `full`; text `Tokens this month: 80 of 100`.
  - `shows usage without a bar when there is no limit`: instance `{used: 1234, limit: null}` → `Tokens this month: 1,234`, no `Instance token quota` meter.
  - `offers Start on a quota-held change to admins only`: admin sees `Start review of !12` and `Monthly token quota used up.`; a maintainer sees the row and no button.
  - `sets an instance's quota`: Integrations, dashboard instance, type `1000`, Save → PUT `/api/admin/instances/extra/token-quota` body `{ monthlyTokenQuota: 1000 }`; a config instance's input is disabled.

- [ ] **Step 2: Run them to see them fail**
  Run: `cd web && npx vitest run src/pages/TokenQuota.test.tsx`
  Expected: FAIL.

- [ ] **Step 3: Implement** the types, form row, bars, waiting box changes and Integrations card; update existing tests that asserted the old Waiting intro.

- [ ] **Step 4: Run the web gate**
  Run: `cd web && npm run typecheck && npm run lint && npm test -- --run && npm run build`
  Expected: PASS. Then remove the `node_modules` link and `tsconfig.tsbuildinfo`; `git status` shows no stray files (`internal/web/dist` keeps only `.keep`).

- [ ] **Step 5: Commit** `feat(web): token quota fields, usage bars and quota-held changes`

### Task 8: Docs

**Files:**
- Create: `docs/concepts/token-quota.md`
- Modify: `docs/README.md` (Concepts table, after Trusted authors), `docs/upgrading.md` (new top section `## Upgrading to 0.16.0`, re-checked against the latest version in that file), `docs/reference/configuration.md` (rows for `gitlab[].monthly_token_quota`, `github[].monthly_token_quota`, `repositories[].monthly_token_quota`), `docs/concepts/commands.md` (`## Re-running a review` and `## Limits on commands`: a used-up quota refuses `review`), `docs/concepts/dashboard.md` (settings with config keys; audit-log list gains the quota changes), `docs/concepts/trusted-authors.md` (one line: trust is checked before the quota), `docs/reference/http-routes.md` (the new route)

**Interfaces:**
- Consumes: the copy in Global Constraints, verbatim.

- [ ] **Step 1: Write `docs/concepts/token-quota.md`**: where to set each quota (Integrations, repository Settings, config keys), empty means unlimited, a repository's quota sits inside its instance's, what counts (input + output, UTC month, by when a review finished), what happens to automatic reviews (held, the status and note quoted verbatim, waiting box, admin's Start) and to requests by hand (refused with the reason, the note as the reply once a month), admins' Start and Re-run go past it and count, nothing starts by itself on the 1st, the overshoot (a review that starts with room can go over, and reviews started together near the limit can all run), the known gap (a Job with no result, or a superseded review, counts as zero), and that rolling back removes the quota.
- [ ] **Step 2: Update the other pages** listed above; upgrading says migration `0035` runs at startup (back up Postgres first), nothing is limited after the upgrade, and to remove the three `monthly_token_quota` keys before rolling back.
- [ ] **Step 3: Check** every quoted text matches `internal/engine/quota.go` exactly: `grep -rn "monthly token quota is used up" docs internal/engine/quota.go`.
- [ ] **Step 4: Run the full gates** (Go and web, as in Global Constraints) and the name scan over the whole branch: `git diff origin/main... | grep -i -E 'golabs|gojek|gopay[^t]'` prints nothing.
- [ ] **Step 5: Commit** `docs: the monthly token quota`
