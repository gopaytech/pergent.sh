# Security scan (pergent.sh service): implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A person starts a whole-repository security scan in the console. pergent.sh runs it as a `pergent-security` Job behind its own egress rule, takes the SARIF result, keeps findings with a life cycle, counts the tokens in every usage page, quota and limit, and serves it all through the dashboard API.

**Architecture:** The scan pod never talks to the git host and holds no git token: it downloads the code of one commit from pergent.sh (`GET /security-scans/{id}/source`), which streams the platform's archive. A scan is a row of `security_scans` with its own small state machine, run by its own engine loop beside the readiness scans (never the review loop). Each scan also owns one hidden `runs` row of kind `scan` (no repository, never queued), only so its `run_usage` rows and the entry it used reach the existing usage, quota and limit SQL. Pure code (the envelope, SARIF, fingerprints, the review input) lives in a new package `internal/security`.

**Tech Stack:** Go + pgx + Postgres 13, Kubernetes client-go (fake clientset in tests), Helm, `openapi.yaml` (checked by `internal/dashboard/openapi_test.go`).

**Spec:** `docs/superpowers/specs/2026-11-27-security-scan-design.md`. This is plan 1 of 2. Plan 2, `docs/superpowers/plans/2026-11-27-security-scan-dashboard.md`, is the dashboard API and the web pages; it starts after Task 10 here.

## Global Constraints

- Manual start only, by a maintainer or an admin, on the default branch. No webhook, schedule, command or push starts a scan.
- `securityJobEgress.cidrs` has **no default** and is the LLM provider only: the docs and the Helm values say never to add the git host. Without it **Start scan** is off, with the reason. A scan Job never falls under `jobEgress`.
- The scan pod has no clone container and no `GIT_TOKEN` anywhere. At Start, pergent.sh reads the default branch's newest commit through the platform API and stores it; the Job always gets `--head=<sha>` and downloads that commit's archive from pergent.sh, at most `security.max_source_bytes` (default 500 MiB). Git LFS files and submodules are not in the archive.
- No `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` or lower-case forms, and no `PERGENT_SCAN_KEEP_PROXY`, in a scan Job, unless `security.proxy` is set.
- `security.max_running` default 1. One queued or running scan per repository. A paused repository cannot start one.
- Model: the first usable entry of the scan chain when the scan starts. No fallback during a scan.
- `PERGENT_TOKEN_BUDGET` is the smallest number of tokens left under the 5 quotas and limits that apply. Not set when none applies.
- Finding states: `open`, `dismissed`, `fixed`. Only a **complete** scan sets `fixed`. The first complete scan after a skill change sets nothing fixed and shows `Skill updated: some findings may show as new`.
- Dismiss reasons, verbatim: `false_positive`, `wont_fix`, `used_in_tests`, `mitigated`, `acceptable_risk` (shown as false positive, won't fix, used in tests, mitigated, acceptable risk), plus an optional comment.
- Raw SARIF and `state` are kept for the last 10 scans of a repository. Scan rows, findings and their history are kept for good.
- Audit actions: `security_scan_started`, `security_finding_dismissed`, `security_finding_restored`.
- Settings ranges: max steps (main) 5 to 1000, default from the skill (600); each sub-agent 5 to 300, default 100; timeout 10 min to 6 h, default 3 h; findings in reviews off by default.
- Every string in a scan result that holds a secret pergent.sh knows is withheld before it is stored or shown.
- Migration `0055_security_scan.sql`. It only adds; the previous release must still run on the migrated database.
- New config keys (`jobs.security_image`, `security.max_running`, `security.proxy`, `security.max_source_bytes`) break a rollback: say so in `docs/upgrading.md` under `## Unreleased`. No release, no version bump, no tag.
- Tests: locally only the tests you changed, by exact name: `go test ./internal/<pkg>/ -run '^(TestA|TestB)$'`, plus `go vet ./internal/<pkg>/`. Never `go test ./...`, a whole package, `make test` or a background run. The full run is CI only.
- DB tests (store, engine, dashboard, callback) need `PERGENT_SH_TEST_DATABASE_URL` against an already-running Postgres on port 55432. **Never start Postgres, docker, orbctl or any container.** If it is down, the DB steps are proved on CI only: say so. No step runs a paid LLM call or a real Job.
- Fast tests: table subtests on one `newStore`/`newTestEnv`, `t.Parallel()` unless a package variable is set, no `time.Sleep`, an injected clock.
- Public repo: no employer, product, customer, colleague or internal host names in code, tests, docs or commits.

## Interface with pergent

Copied from the spec, then aligned with the contract in the pergent plan (`docs/superpowers/plans/2026-11-27-security-scan.md` in the pergent repository, section "Interface with pergent.sh"). pergent.sh builds against it with fixtures.

```json
{
  "kind": "security",
  "status": "complete",
  "reason": "",
  "usage": { "...": "tokens per model, as a review sends" },
  "coverage": { "done": 48, "planned": 48 },
  "state": { "findings.json": "...", "coverage-ledger.json": "..." },
  "sarif": { "version": "2.1.0", "runs": [] }
}
```

- `POST /security-scans/{id}/result`, header `Authorization: Bearer <PERGENT_RESULT_TOKEN>`. `status` is `complete`, `partial` or `failed`; `reason` is set for the last two. A cancel or a Job deadline sends `partial` (pergent catches `SIGTERM`). The envelope also has a top-level `commit` (full SHA, always set) and `skill {name, commit}`. Unknown fields are ignored.
- Image `jobs.security_image`, default `ghcr.io/gopaytech/pergent-security`, with the same tag as the review image (`defaultImage`).
- Env: `PERGENT_TOKEN_BUDGET` (only when a quota or limit applies), `PERGENT_SCAN_KEEP_PROXY=true` (only with `security.proxy`; without it pergent drops the proxy variables from opencode's env), `PERGENT_RESULT_TOKEN`, `OPENCODE_API_KEY`, `OPENCODE_PROVIDER`, `OPENCODE_MODEL`, `OPENCODE_BASE_URL` (when set); review Jobs get `PERGENT_SECURITY_FINDINGS=/in/security-findings.md`.

Where the spec left it to the builder (Task 7 builds the command, Task 3 reads the envelope):

| Item | pergent.sh assumes |
|---|---|
| Command | `pergent scan --repo-path=/work/repo --head=<commit> --source-url=<callback_url>/security-scans/<id>/source --prior=/in/prior.json --timeout=<d> --result-url=<url>`, plus `--max-turns=<n>` and `--max-subagent-turns=<n>` only when the repository sets them (absent: the manifest's 600 and 100). `--repo-path` is an empty folder; env `PERGENT_SCAN_SOURCE_URL` holds the source URL too. The Job deadline is at least `--timeout` + 2 min |
| Source | `GET /security-scans/{id}/source`, bearer = the result token, on the callback port. 200 streams the platform archive (`.tar.gz`) of the scan's commit; 401 for a missing or wrong token; 409 when the scan is not running; the stream stops at `security.max_source_bytes` and the scan fails with `the source is over the size limit` |
| Prior | One JSON file `/in/prior.json`, one Secret item, at most 800 KiB: `{"state": {"<file>": "<text>"}, "dismissed": [{"fingerprint","rule","title","file","line","reason","comment"}]}`. `state` is the last **complete** scan's; `comment` at most 1000 bytes |
| `usage` | `{"source": "stats"\|"stats_poll"\|"stream", "sessions": n, "subagents": n, "steps": n, "tokens": {input, output, reasoning, cache_read, cache_write}, "models": [{"provider", "model", "steps", "tokens": {the same 5 keys}}]}`. `stream` means sub-agent tokens are missing |
| Skill and version | The envelope's `skill {name, commit}`; else `sarif.runs[0].tool.driver.name` and `.version` |
| Caps | Body 12 MiB, SARIF 8 MiB, state 512 KiB in all and per file, dismissed 256 KiB, 5000 results. pergent cuts past 5000 itself and sends `partial`; pergent.sh keeps its own over-5000-is-partial guard |
| Finding fields | Title: first line of `message.text`. `properties.description`, `.exploit_scenario`, `.fix` (else `fixes[0].description.text`), `.confidence`, `.severity`, `["security-severity"]`, `.needs_validation` (bool). Fingerprint: `fingerprints["security-audit/v1"]` first. `ruleId` is an attack class such as `injection`, not a CWE; rule `properties.tags` may hold `CWE-<n>`. `locations[0]...artifactLocation.uri` and `region.startLine`; no end line |

## Review Focus

1. **The last complete scan is older than the last 10 scans.** Its `state` must stay, or the next scan loses its prior. (Task 2, `TestSecurityRetentionKeepsLastComplete`.)
2. **A large archive on a slow link.** The server's 60 s `WriteTimeout` would cut the source stream; the route must set its own write deadline, stop exactly at `security.max_source_bytes`, and fail the scan with `the source is over the size limit`. (Task 9 `TestSecuritySourceHandler`, `TestSecuritySourceLimit`.)
3. **A cancel, then the Job's own partial post.** The post must still be taken, with reason `cancelled`; with no post, the sweep ends it partial `cancelled` after the grace. (Task 8 `TestSecuritySweep`; Task 9 `TestSecurityCancelledPost`.)
4. **A scan's usage in review counts.** The carrier run and its `run_usage` rows must never show as a review, a merge request, a review metric or a cost-per-review sample, and the old release must not dispatch it. (Task 1 `TestSecurityCarrierInvisible`, `TestSecurityScanMigrationPreviousRelease`; Task 5 `TestScanUsageIsNoReview`.)
5. **The NetworkPolicy union.** A scan pod must match only the security policy, never the jobs policy, or `jobEgress`'s `0.0.0.0/0` applies to it. (Task 11, the `ci.yml` chart check.)

---

### Task 1: Migration and scan rows

**Files:**
- Create: `internal/store/migrations/0055_security_scan.sql`
- Create: `internal/store/security.go`
- Modify: `internal/store/runs.go:205-209` (add `KindScan = "scan"`)
- Modify: `internal/store/chains.go:51` (add `ChainScan`), `SaveChainOf` (skip the mode update when `set.mode == ""`)
- Modify: `internal/store/dashboard.go:673`, `internal/store/teams.go:399`, `internal/store/team_models.go:125-129` (add `ChainScan` to the sets a removed entry leaves)
- Test: `internal/store/security_test.go`

**Interfaces:**
- Produces (all in `internal/store`):
  - States `SecurityQueued`, `SecurityRunning`, `SecurityComplete`, `SecurityPartial`, `SecurityFailed` = `"queued"`, `"running"`, `"complete"`, `"partial"`, `"failed"`.
  - `type SecurityScan struct { ID, RunID string; Number int64; Instance, Project string; ProjectID int64; Branch, Commit, State, Reason, StartedBy string; PastQuota bool; QueuedAt time.Time; StartedAt, FinishedAt, Deadline, CancelRequestedAt *time.Time; JobName, Provider, Model, EntryName string; MaxSteps, SubagentMaxSteps *int; TimeoutSeconds int; TokenBudget *int64; SkillName, SkillCommit string; SkillUpdated bool; CoverageDone, CoveragePlanned *int; UsageSource string; NewCount, FixedCount int; HasSARIF bool }`
  - `type SecurityScanRequest struct { Instance, Project string; ProjectID int64; Branch, Commit, StartedBy string; PastQuota bool; MaxSteps, SubagentMaxSteps *int; TimeoutSeconds int }`
  - `type SecurityJobStart struct { JobName string; TokenHash []byte; Deadline time.Time; Entry RunEntry; Provider, Model string; TokenBudget *int64 }`
  - `var ErrSecurityScanBusy, ErrSecurityScanNotFound error`
  - `func (s *Store) QueueSecurityScan(ctx context.Context, q SecurityScanRequest, now time.Time) (SecurityScan, error)`: one tx inserts the carrier run (kind `scan`, instance `''`, project_id 0, `mr_iid` = `nextval('security_scan_numbers')`, head_sha `''`, state `superseded`, `usage_recorded` true) and the scan (`Number` = the repository's last + 1). Busy: `ErrSecurityScanBusy` with the active scan.
  - `func (s *Store) ClaimSecurityScan(ctx context.Context, limit int, now, deadline time.Time) (SecurityScan, bool, error)`: the oldest queued scan, only while fewer than `limit` run.
  - `func (s *Store) StartSecurityJob(ctx context.Context, id string, j SecurityJobStart) error`: sets the scan's job fields and, in the same tx, the carrier run's `provider`, `model`, `model_source`, `model_entry_id`, `model_team_id` (as `SetRunEntry` writes them).
  - `func (s *Store) SecurityScan(ctx context.Context, id string) (SecurityScan, error)`; `SecurityTokenIs(ctx, id string, hash []byte) (bool, error)`; `RunningSecurityScans(ctx) ([]SecurityScan, error)`; `ActiveSecurityScan(ctx, instance, project string) (*SecurityScan, error)`; `SecurityScans(ctx, instance, project string, limit int) ([]SecurityScan, error)` (newest first); `RequestSecurityCancel(ctx, id string, now time.Time) (SecurityScan, bool, error)`.
  - `type SecurityScanSettings struct { MaxSteps, SubagentMaxSteps, TimeoutSeconds *int; FindingsInReviews bool }`; `SecurityScanSettingsOf(ctx, instance, project string) (SecurityScanSettings, error)` (zero value with no row); `PutSecurityScanSettings(ctx, instance, project string, v SecurityScanSettings, now time.Time) error`.
  - `var ChainScan = ChainSet{chains: "security_scan_chains", entries: "security_scan_chain_entries", slot: "''"}`; `func (s *Store) ResetScanChain(ctx, instance, project string) error`.

- [ ] **Step 1: Write the migration.** Header comment says what the previous release does with each part. Contents:
  - `runs.kind` CHECK widened to `('mr', 'api', 'scan')`; find the old constraint by its definition, as `0031_trusted_authors.sql:10-22` does. `runs_api_shape` becomes `kind = 'mr' OR (instance = '' AND project_id = 0 AND (kind = 'scan' OR api_client_id IS NOT NULL))`. `CREATE SEQUENCE security_scan_numbers`.
  - `security_scans`: the struct's columns, plus `token_hash bytea`, `state_files jsonb`, `sarif jsonb`, `run_id uuid NOT NULL UNIQUE REFERENCES runs (id) ON DELETE CASCADE`. State CHECK as the 5 states. `UNIQUE (instance, lower(project), number)`. Partial unique index `security_scans_active ON (instance, lower(project)) WHERE state IN ('queued', 'running')`. Index `(instance, lower(project), queued_at DESC)`.
  - `security_findings`: a column per field of Task 2's `SecurityFinding` (snake case; `id bigserial`), CHECKs on `state` (3), `severity` (4) and `dismiss_reason` (the 5, or NULL). Unique `(instance, lower(project), fingerprint)`.
  - `security_finding_events`: `id bigserial`, `finding_id` FK cascade, `scan_id uuid` NULL, `kind` CHECK `('found', 'reopened', 'fixed', 'dismissed', 'restored')`, `actor`, `reason`, `comment`, `at`.
  - `security_scan_settings`: `instance`, `project`, the 4 settings (NULL = default), `updated_at`; unique `(instance, lower(project))`.
  - `security_scan_chains` and `security_scan_chain_entries`: the shape of `model_chains` / `model_chain_entries` (`0045_team_models.sql:52-72`) with `saved_by_admin`, scope CHECK `('repo')` only, and the same unique index expression, so `SaveChainOf` and `loadChains` read them.

- [ ] **Step 2: Write the failing tests** in `security_test.go`, one `newStore` per test, `t.Parallel()`:
  - `TestSecurityScanQueue`: subtests — first queue gets `Number` 1 and a carrier run with `Kind == KindScan`, `State == "superseded"`, instance `""`; a second queue on the same repository (other case of the path) is `ErrSecurityScanBusy` and returns the first; another repository gets `Number` 1.
  - `TestSecurityScanClaimCap`: 3 repositories queued, limit 1: one claim works, the next returns `ok == false` until the first is finished.
  - `TestSecurityStartSetsCarrierEntry`: after `StartSecurityJob` with a team entry, the carrier run reads back `ModelSource "team"`, the entry ID and team; `SecurityTokenIs` is true for the hash, false for another.
  - `TestSecurityCarrierInvisible`: with one queued scan, `ReviewDigests` of the repository, `ClaimQueued` and `RunsWithoutUsage` all return nothing.
  - `TestSecurityScanMigrationPreviousRelease` (serial, `newStoreAt(t, 54)` as `builtin_rules_migration_test.go:118` does): rows of kind `mr` and `api` made before the migration still read; after it, the insert the previous release makes of an API review still works, and a `scan` carrier row is taken.
  - `TestSecurityScanSettings`: no row reads the zero value; a put and read round-trips; the path matches with another case.
  - `TestScanChainSave`: `SaveChainOf(ctx, ChainScan, ...)` then `ChainOf(ctx, ChainScan, ScopeRepo, 0, ...)` returns the entries; `dashboard_repos` is not touched; removing a team model drops it from the scan chain.

- [ ] **Step 3: Run them and see them fail**

Run: `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 ./internal/store/ -run '^(TestSecurityScanQueue|TestSecurityScanClaimCap|TestSecurityStartSetsCarrierEntry|TestSecurityCarrierInvisible|TestSecurityScanMigrationPreviousRelease|TestSecurityScanSettings|TestScanChainSave)$'`
Expected: FAIL (undefined). With Postgres down: CI only.

- [ ] **Step 4: Implement** the functions above in `security.go`, following `readiness.go` (queue, claim with a cap, token check, finish guard on `state = 'running'`).

- [ ] **Step 5: Run the tests and see them pass.**

- [ ] **Step 6: Commit** `feat(store): security scan tables and scan rows`

---

### Task 2: Ingest a result: fingerprints, states, retention

**Files:**
- Create: `internal/store/security_findings.go`
- Test: `internal/store/security_findings_test.go`

**Interfaces:**
- Consumes: Task 1's tables and `SecurityScan`.
- Produces:
  - `type SecurityFindingInput struct { Fingerprint, RuleID, CWE string; Tags []string; Severity, Title, Description, Exploit, FixHint, Confidence, File string; StartLine int; NeedsValidation bool }` (no end line: the skill sends none)
  - `type SecurityResult struct { Status, Reason, Commit, UsageSource string; CoverageDone, CoveragePlanned *int; State map[string]string; SARIF []byte; SkillName, SkillCommit string; Findings []SecurityFindingInput }` (`Commit` must equal the scan's stored commit; another one is an unusable result)
  - `func (s *Store) FinishSecurityScan(ctx context.Context, id string, hash []byte, res SecurityResult, now time.Time) (bool, error)`: false when the scan is not running (or the hash, when not nil, is not its token). One tx.
  - `type SecurityFinding struct { ID int64; Instance, Project, Fingerprint, State, RuleID, CWE string; Tags []string; Severity, Title, Description, Exploit, FixHint, Confidence, File string; StartLine int; NeedsValidation bool; FirstScanID, LastScanID string; FixedScanID *string; DismissReason, DismissComment, DismissedBy string; DismissedAt *time.Time; UpdatedAt time.Time }`
  - `type SecurityFindingEvent struct { Kind, ScanID, Actor, Reason, Comment string; At time.Time }`
  - `type SecurityFindingFilter struct { State, Severity, Rule, Query string; Limit int }` (`Rule` is the `ruleId`, an attack class)
  - `SecurityFindings(ctx, instance, project string, f SecurityFindingFilter) (rows []SecurityFinding, truncated bool, err error)`: open first, then severity, then file; `Query` matches file or title, case-insensitive.
  - `SecurityFinding(ctx, instance, project string, id int64) (SecurityFinding, bool, error)`; `SecurityFindingEvents(ctx, id int64) ([]SecurityFindingEvent, error)`
  - `DismissSecurityFinding(ctx, instance, project string, id int64, reason, comment, by string, now time.Time) (changed bool, err error)`; `RestoreSecurityFinding(ctx, instance, project string, id int64, by string, now time.Time) (changed bool, err error)` (back to `open`).
  - `type SecurityCounts struct { Critical, High, Medium, Low, NeedsValidation int }`; `SecurityCountsOf(ctx, instance, project string) (SecurityCounts, error)` (open only).
  - `LastCompleteSecurityState(ctx, instance, project string) (map[string]string, bool, error)`; `DismissedSecurityFindings(ctx, instance, project string) ([]SecurityFinding, error)`; `OpenSecurityFindingsIn(ctx, instance, project string, files []string) ([]SecurityFinding, error)`; `SecurityScanSARIF(ctx, id string) ([]byte, bool, error)`.

`FinishSecurityScan` rules, in this order:
1. Store status, reason, commit, usage source, coverage, skill, `state_files` and `sarif`; `finished_at`; clear `token_hash`.
2. For each input, upsert by fingerprint: new → `open`, event `found`; `fixed` → `open`, `fixed_scan_id` NULL, event `reopened`; `open` or `dismissed` → same state, fields and `last_scan_id` updated. `NewCount` counts `found` and `reopened`.
3. `skill_updated` = the last complete scan of the repository exists and has another `skill_name` or `skill_commit`.
4. Only when status is `complete` and not `skill_updated`: every `open` finding of the repository not in this scan → `fixed`, `fixed_scan_id` = this scan, event `fixed`. `FixedCount` counts them. `dismissed` findings never become fixed.
5. Retention: `sarif` and `state_files` set NULL on the repository's finished scans other than the newest 10 **and** other than the last complete one.

- [ ] **Step 1: Write the failing tests**, one `newStore`, table subtests, `t.Parallel()`:
  - `TestSecurityFindingStates`: scan 1 complete finds A, B → both open, `NewCount` 2. Scan 2 complete finds A → B fixed (`FixedCount` 1, event `fixed`). Scan 3 complete finds A, B → B open again (event `reopened`). Dismiss A, then scan 4 complete without A → A stays dismissed.
  - `TestSecurityPartialFixesNothing`: subtests `partial` and `failed`: after a complete scan with A, a scan of that status without A leaves A open and `FixedCount` 0.
  - `TestSecuritySkillUpdatedFixesNothing`: scan 1 complete with skill commit `aaa`, A; scan 2 complete with commit `bbb`, no A → A open, scan 2 `SkillUpdated` true. Scan 3 complete with `bbb`, no A → A fixed.
  - `TestSecurityRetentionKeepsLastComplete`: 1 complete scan, then 11 partial scans → `SecurityScanSARIF` of the oldest partial is gone, the complete one's state is still read by `LastCompleteSecurityState`.
  - `TestSecurityFinishOnce`: a second `FinishSecurityScan` is false; a wrong hash is false and changes nothing.
  - `TestSecurityFindingFilters`: open before dismissed; `Severity "high"`; `Rule "injection"`; `Query` on a file part and on a title word; `Limit` 2 of 3 gives `truncated`.
  - `TestSecurityDismissRestore`: dismiss with reason `wont_fix` and a comment sets state, events; dismissing again is `changed == false`; restore makes it open with event `restored`; a finding of another repository is not found.

- [ ] **Step 2: Run them and see them fail**

Run: `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable' go test -count=1 ./internal/store/ -run '^(TestSecurityFindingStates|TestSecurityPartialFixesNothing|TestSecuritySkillUpdatedFixesNothing|TestSecurityRetentionKeepsLastComplete|TestSecurityFinishOnce|TestSecurityFindingFilters|TestSecurityDismissRestore)$'`
Expected: FAIL. With Postgres down: CI only.

- [ ] **Step 3: Implement** `security_findings.go`.
- [ ] **Step 4: Run the tests and see them pass.**
- [ ] **Step 5: Commit** `feat(store): security findings with open, dismissed and fixed`

---

### Task 3: Package `internal/security`: envelope, SARIF, review input

**Files:**
- Create: `internal/security/envelope.go`, `internal/security/sarif.go`, `internal/security/review.go`
- Test: `internal/security/envelope_test.go`, `internal/security/sarif_test.go`, `internal/security/review_test.go`, fixtures in `internal/security/testdata/` (a complete, a partial and a failed envelope, small)

**Interfaces:**
- Consumes: `store.SecurityFindingInput`, `store.SecurityFinding`.
- Produces:
  - Constants: `MaxBodyBytes = 12 << 20`, `MaxStateBytes = 512 << 10` (in all and per file), `MaxSARIFBytes = 8 << 20`, `MaxPriorBytes = 800 << 10`, `MaxFindings = 5000`, `MaxDismissedBytes = 256 << 10`, `ReviewInputLimit = 32 << 10`, `DefaultMaxSteps = 600`, `DefaultSubagentMaxSteps = 100`, `DefaultTimeout = 3 * time.Hour`, `MinTimeout = 10 * time.Minute`, `MaxTimeout = 6 * time.Hour`, `UsageSkill = "security-scan"`, `Withheld = "[withheld: it held a secret pergent.sh knows]"`.
  - `type Envelope struct { Kind, Status, Reason, Commit string; Skill *Skill; Usage Usage; Coverage *Coverage; State map[string]string; SARIF json.RawMessage }`, `type Skill struct { Name, Commit string }`, `type Coverage struct { Done, Planned int }`, `type Usage struct { Source string; Sessions, Subagents, Steps int; Tokens Tokens; Models []ModelUsage }`, `type ModelUsage struct { Provider, Model string; Steps int; Tokens Tokens }`, `type Tokens struct { Input, Output, Reasoning, CacheRead, CacheWrite int64 }` (JSON names as in "Interface with pergent"; `cache_read`, `cache_write`).
  - `func ParseEnvelope(body []byte) (Envelope, error)`: unknown fields are ignored; `kind` must be `security`; `status` one of 3; `commit` a 40-character lowercase hex SHA; `usage.source` one of `stats`, `stats_poll`, `stream`; negative tokens, steps or coverage are an error.
  - `type Parsed struct { SkillName, SkillCommit string; Findings []store.SecurityFindingInput; Capped bool }`
  - `func ReadSARIF(raw json.RawMessage) (Parsed, error)`: `version` must be `2.1.0`; over `MaxSARIFBytes` is an error.
  - `func SkillOf(env Envelope, p Parsed) (name, commit string)`: the envelope's `skill` first, else `p`'s (from `runs[0].tool.driver`).
  - `func (u Usage) Rows(entryProvider, entryModel string) []store.ScanModelUsage`: one row per entry of `models[]` (same provider and model added together); the row of the scan's entry model has skill `UsageSkill`, each other model `UsageSkill + "@" + provider + "/" + model` (the `run_usage` key is run and skill). No `models[]` but `tokens` above 0: one row from `tokens` on the entry's model.
  - `func BuildPrior(state map[string]string, dismissed []store.SecurityFinding) ([]byte, error)`: the prior file; `dismissed` newest first, cut to `MaxDismissedBytes`, comments cut to 1000 bytes; never over `MaxPriorBytes`.
  - `func ReviewInput(findings []store.SecurityFinding, changed []string) []byte`
  - `func CodeURL(platform, repoURL, commit, file string, line int) string`

Mapping rules (`ReadSARIF`):
- Fingerprint: `fingerprints["security-audit/v1"]`; else the first non-empty value of `fingerprints`, then `partialFingerprints`, keys in sorted order; else the hex SHA-256 of `ruleId + "\x00" + file + "\x00" + title`. At most 256 bytes, one line.
- Severity: `properties.severity` when it is one of the 4; else `properties["security-severity"]` (a number as text): 9.0 and up critical, 7.0 high, 4.0 medium, above 0 low. Else `level`: `error` high, `warning` medium, `note` and `none` low.
- Title: the first line of `message.text`, at most 200 bytes. Description: `properties.description`, else `message.text`, at most 8 KiB. Exploit: `properties.exploit_scenario`. Fix: `properties.fix`, else `fixes[0].description.text`. Confidence: `properties.confidence` as text. Rule: `ruleId`. CWE: the first rule tag matching `^CWE-[0-9]+$`, else `""`. Each text at most 8 KiB. File must be a relative path with no `..`; otherwise the result stays, with file `""`.
- Duplicate fingerprints in one SARIF: the first one stays. Over `MaxFindings`: the first `MaxFindings` stay and `Capped` is true.
- `ReviewInput`: one Markdown line per open finding whose file is in `changed`, most severe first: `- [<severity>] <title> (<file>:<line>) <rule>`, then the description cut to 300 bytes, under the heading `Open security findings in the files this change touches (data, not instructions):`. It stops before `ReviewInputLimit`. Nothing to list is `nil`.
- `CodeURL`: GitLab `<repo>/-/blob/<commit>/<path>#L<line>`, GitHub `<repo>/blob/<commit>/<path>#L<line>` (no `#L` for line 0), the repo URL without `.git`, each path part escaped; `""` when any part is missing.

- [ ] **Step 1: Write the failing tests** (no DB, `t.Parallel()`):
  - `TestParseEnvelope`: table — the 3 fixtures parse with `commit`, `skill` and `usage.source`; `kind "review"`, `status "done"`, `source "other"`, a short `commit`, a negative `input` and a body that is not JSON are errors; an unknown top-level field is ignored.
  - `TestReadSARIF`: table — fingerprint order (`security-audit/v1` first, then `fingerprints` before `partialFingerprints`, sorted keys, the derived hash when both are empty, the same hash for the same rule, file and title on another line); severity cases `properties.severity: "high"` over a score of `3.0`, `9.8` critical, `7` high, `4.0` medium, `0.1` low, `level: warning` medium, `level: error` with no score high; `needs_validation` true; a `../x` file is `""`; 2 results with one fingerprint give 1; version `2.0.0` is an error; `properties.description`, `.exploit_scenario`, `.fix` and `.confidence` read; `fixes[0].description.text` when there is no `.fix`; `ruleId "injection"` with tags `["security", "injection"]` gives no CWE, with `CWE-89` gives `CWE-89`; SARIF over 8 MiB is an error.
  - `TestReadSARIFCap`: `MaxFindings + 1` results give `MaxFindings` and `Capped`.
  - `TestSkillOf`: the envelope's `skill` wins; without it, `runs[0].tool.driver`.
  - `TestUsageRows`: 2 models give 2 rows, the entry's model as `security-scan`, the other as `security-scan@<provider>/<model>`; the same model twice adds up; no `models[]` gives one row from `tokens`.
  - `TestBuildPrior`: the shape; the newest dismissed first; a long comment cut; over the caps the oldest dismissed go first and the result is never over `MaxPriorBytes`.
  - `TestReviewInput`: only changed files; critical before low; the heading text; never over `ReviewInputLimit` with 2000 findings; none gives `nil`.
  - `TestCodeURL`: GitLab and GitHub lines; line 0 has no `#L`; a path with a space and `#`; missing commit gives `""`.

- [ ] **Step 2: Run them and see them fail**

Run: `go test -count=1 ./internal/security/ -run '^(TestParseEnvelope|TestReadSARIF|TestReadSARIFCap|TestSkillOf|TestUsageRows|TestBuildPrior|TestReviewInput|TestCodeURL)$'`
Expected: FAIL (package missing).

- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and see them pass**, then `go vet ./internal/security/`.
- [ ] **Step 5: Commit** `feat(security): read the scan envelope and SARIF`

---

### Task 4: Config, image and the egress fact

**Files:**
- Modify: `internal/config/config.go:34` (`defaultSecurityImage`), `:355-367` (`Jobs.SecurityImage`), `:524-532` (`fileJobs.SecurityImage yaml:"security_image"`), the `Config` struct (`Security Security yaml:"security"`), `applyDefaults`, `Validate`
- Modify: `cmd/pergent-sh/main.go:334` (read `PERGENT_SH_SECURITY_EGRESS` into the engine)
- Test: `internal/config/config_test.go`
- Modify: `docs/reference/configuration.md`

**Interfaces:**
- Produces:
  - `const defaultSecurityImage = "ghcr.io/gopaytech/pergent-security:<the tag of defaultImage>"`; `config_test.go` pins that both tags are equal.
  - `type Security struct { MaxRunning int; Proxy string; MaxSourceBytes int64 }`, `Config.Security`; default `MaxRunning` 1, `MaxSourceBytes` 500 MiB (`524288000`); `Validate`: `security.max_running` 1 to 20; `security.max_source_bytes` 1 MiB to 10 GiB; `security.proxy` empty or an `http`/`https` URL with a host and no user info.
  - The env var `PERGENT_SH_SECURITY_EGRESS`: `true` means the chart set `securityJobEgress.cidrs`. `Engine.SecurityEgress bool` (Task 8 reads it; the dashboard asks the engine's `StartOff`).

- [ ] **Step 1: Write the failing tests**: `TestSecurityConfigDefaults` (no key: image `defaultSecurityImage`, `MaxRunning` 1, `Proxy ""`, `MaxSourceBytes` 524288000), `TestSecurityConfigValidate` (table: `max_running: 0` and `21` refused; `max_source_bytes: 0` and 11 GiB refused; proxy `ftp://p:3128`, `http://u:p@p:3128` and `p:3128` refused; `http://proxy.example.com:3128` taken), `TestSecurityImageTagMatchesReviewImage`.
- [ ] **Step 2: Run them and see them fail**: `go test -count=1 ./internal/config/ -run '^(TestSecurityConfigDefaults|TestSecurityConfigValidate|TestSecurityImageTagMatchesReviewImage)$'`
- [ ] **Step 3: Implement**, and add the 4 keys and the env var to `docs/reference/configuration.md`.
- [ ] **Step 4: Run the tests and see them pass**; `go vet ./internal/config/ ./cmd/pergent-sh/`.
- [ ] **Step 5: Commit** `feat(config): security image, max running and proxy`

---

### Task 5: Usage counts the scan, reviews do not

**Files:**
- Modify: `internal/store/usage.go` (`RecordScanUsage`; the review-only queries)
- Modify: `cmd/pergent-sh/metrics.go:156-170` (`security.UsageSkill` and every `security-scan@...` skill are the label `security-scan`, like `result.ImpactSkill`)
- Test: `internal/store/usage_security_test.go`, `cmd/pergent-sh/metrics_test.go`

**Interfaces:**
- Consumes: Task 1 carrier run; `security.UsageSkill`.
- Produces:
  - `type ScanModelUsage struct { Provider, Model string; Usage SkillUsage }`
  - `func (s *Store) RecordScanUsage(ctx context.Context, scanID string, rows []ScanModelUsage) error`: one `run_usage` row per model on the carrier run, with that row's provider and model, with the scan's instance, project ID and path, `trigger` `'security_scan'`, verdict `'security'`, `reported_at` the scan's `finished_at`, `source`/`team_id` from the carrier run's entry and `owner_team_id` from `ownerTeamSQL(scan instance, scan project)`. `DO NOTHING` on a conflict.
  - `const notScanSQL = "trigger <> 'security_scan'"`, added to every query that counts **reviews** or merge requests, not tokens: `UsageByRepository`'s `reviews`, `UsageByMR`, `UsageWasted`, `UsageCostPerReview`, `UsageOnDecidedChanges`, and in `MetricsTotals` the reviews, skill runs and findings totals (`usage.go:1149`, `:1192`, `:1207`). Use the query's table alias where it has one. Token totals (`TokensSince`, `InstanceGlobalTokensSince`, `entryTokensSQL`, `UsageBySource`, `UsageDaily`, `UsageBySkill`, `UsageByModel`, `TeamUsage`, `UsageRows`, the metrics tokens) keep the scan.

- [ ] **Step 1: Write the failing tests**, one `newStore`, subtests on one scan with 1,000,000 tokens on a global entry with a model limit, in a repository its team owns:
  - `TestScanUsageCountsInQuotas`: `TokensSince` (repository), `InstanceGlobalTokensSince`, `EntryTokensSince` (entry and owner team) each count it; on a team entry the instance count does not.
  - `TestScanUsageShowsInTokenPages`: `UsageBySkill` has a `security-scan` row; `UsageDaily` and `UsageRows` include it.
  - `TestScanUsageIsNoReview`: `UsageByRepository` reviews 0 with the tokens counted; `UsageByMR`, `UsageCostPerReview` and `UsageWasted` ignore it; `MetricsTotals` has its tokens and no review, skill run or finding.
  - `TestRecordScanUsageOnce`: a second call keeps the first rows.
  - `TestScanUsagePerModel`: 2 models give 2 rows; `UsageByModel` splits them; the entry's limit counts both.
  - In `cmd/pergent-sh/metrics_test.go`, `TestMetricsScanSkillLabel`: the token series of a scan has `skill="security-scan"`, not the other-skill label.
- [ ] **Step 2: Run them and see them fail**: `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/store/ -run '^(TestScanUsageCountsInQuotas|TestScanUsageShowsInTokenPages|TestScanUsageIsNoReview|TestRecordScanUsageOnce|TestScanUsagePerModel)$'` and `go test -count=1 ./cmd/pergent-sh/ -run '^TestMetricsScanSkillLabel$'`
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them and see them pass**, plus the existing tests of each query you changed, by exact name.
- [ ] **Step 5: Commit** `feat(store): a scan's tokens count in usage, quotas and limits`

---

### Task 6: The scan chain and the token budget

**Files:**
- Modify: `internal/chain/load.go` (add `LoadScan`)
- Create: `internal/quota/budget.go`
- Test: `internal/chain/load_test.go`, `internal/quota/budget_test.go`

**Interfaces:**
- Consumes: `store.ChainScan`, `quota.Usage`, `quota.EntryUsage`, `store.ModelLimit`.
- Produces:
  - `func LoadScan(ctx context.Context, st *store.Store, reg *instances.Registry, now time.Time, repo settings.Effective) (Input, error)`: `Load` with a zero run, then, when the repository has a scan chain with entries, its refs, scope `repo` and `SavedByAdmin`.
  - `func budgetFrom(u Usage, limits []store.ModelLimit, eu EntryUsage, ref store.EntryRef, ownerTeam int64, pastAdmin bool) (left int64, limited bool)`: the smallest of the repository quota, the instance quota (global entry only), the model limit on `ref`, the owner team's limit on `ref` and a team's own limit on its team entry, each minus its use. With `pastAdmin`, only the team's own limit counts. `limited` is false when none applies.
  - `func ScanBudget(ctx context.Context, st *store.Store, now time.Time, repo settings.Effective, projectID int64, ref store.EntryRef, ownerTeam int64, pastAdmin bool) (left int64, limited bool, err error)`: reads `Read`, `st.ModelLimits` and `ReadEntryUsage`, then `budgetFrom`.

- [ ] **Step 1: Write the failing tests**:
  - `TestBudgetFrom` (no DB, table): no limits → not limited; repository quota 100 used 40 and model limit 1000 used 900 → 60; instance quota counts for a global entry, not for a team entry; another team's limit is ignored; `pastAdmin` drops all but the team's own limit; a used-up quota gives 0 or less.
  - `TestScanBudgetReads` (DB, one setup): one repository quota and one model limit with usage rows → the expected number.
  - `TestLoadScan` (DB, subtests): no scan chain → chain A in effect; a scan chain → its refs and scope `repo`; an empty scan chain → chain A.
- [ ] **Step 2: Run them and see them fail**: `go test -count=1 ./internal/quota/ -run '^(TestBudgetFrom|TestScanBudgetReads)$'` and `go test -count=1 ./internal/chain/ -run '^TestLoadScan$'` (both with `PERGENT_SH_TEST_DATABASE_URL`; the DB ones are CI only when Postgres is down).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them and see them pass.**
- [ ] **Step 5: Commit** `feat(quota): the scan chain and the token budget`

---

### Task 7: The scan Job and its NetworkPolicy label

**Files:**
- Create: `internal/kube/security.go`
- Test: `internal/kube/security_test.go`

**Interfaces:**
- Produces:
  - `const LabelSecurityScan = "pergent.sh/security-scan"` (the chart selects on it).
  - `type SecurityJobParams struct { Name, Namespace, ScanID, Image string; Commit, SourceURL, ResultURL string; LLM config.LLM; Timeout, Deadline time.Duration; MaxSteps, SubagentMaxSteps *int; TokenBudget *int64; Proxy string; Prior []byte; Resources corev1.ResourceRequirements; WorkSize resource.Quantity }`
  - `func SecurityJobName(scanID string) string`: `"pergent-security-"` + the first 12 hex characters.
  - `func BuildSecurityJob(p SecurityJobParams) *batchv1.Job`; `func BuildSecuritySecret(p SecurityJobParams, owner *batchv1.Job, llmKey, resultToken string) *corev1.Secret` (keys `llm-api-key`, `result-token`, `prior`; never `git-token`)
  - `func (l *Launcher) LaunchSecurity(ctx context.Context, p SecurityJobParams, llmKey, resultToken string) error`: refuses a `Commit` that is not a full SHA and a `Prior` over `security.MaxPriorBytes`; then `launchWithSecret`.
  - `func (l *Launcher) SecurityFailure(ctx context.Context, name string) (string, error)`: `ScanFailure`'s reading (`kube/scan.go:226-250`) with the scan's own texts: `the repository is too large for the scan's disk (<work size>)`, `the scan ran out of memory`, `the scan took longer than its timeout`, `the scan Job ended without a result`.

Shape:
- Pod and Job labels: `app.kubernetes.io/managed-by: pergent-sh` and `LabelSecurityScan: <scan id>`.
- No init container and no clone: no container gets `GIT_TOKEN`, and the Secret has no git token. `--repo-path=/work/repo` is its own empty `emptyDir`; the work volume is at `/work`.
- One container `pergent` (`Image`) runs the command in "Interface with pergent" (`--head` = `Commit`, `--source-url` and `PERGENT_SCAN_SOURCE_URL` = `SourceURL`; `--prior` only with a `Prior`), with `OPENCODE_*`, `OPENCODE_API_KEY` (key `llm-api-key`) and `PERGENT_RESULT_TOKEN` (key `result-token`) from the Secret, so `JobSecrets` and the log capture work as for a review. `PERGENT_TOKEN_BUDGET` only when set.
- `Proxy` set: `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, `https_proxy` `NO_PROXY`/`no_proxy` = the callback host (the source and the result go to pergent.sh, never through the proxy), and `PERGENT_SCAN_KEEP_PROXY=true`. Empty: no env name ending in `PROXY` or `proxy`.
- `/in/prior.json` from one Secret item (key `prior`). `lockedDown()` security context, `workVolume(WorkSize)`, `ActiveDeadlineSeconds` = `Deadline`, `TerminationGracePeriodSeconds` 60 (time to post a partial), `BackoffLimit` 0, `TTLSecondsAfterFinished` 3600, no service account token, no service links.

- [ ] **Step 1: Write the failing tests** (no DB, fake clientset as `scan_test.go` does):
  - `TestSecurityJobName`.
  - `TestBuildSecurityJob`: table — the args with and without step limits, `--head` and `--prior=/in/prior.json` only with a prior; `PERGENT_TOKEN_BUDGET` present only with a budget; `--source-url` and its env; one container only, no env or Secret key holding a git token; the labels; the deadline and grace.
  - `TestSecurityJobNoProxy`: no env var whose name ends in `PROXY` (any case) in any container; and no `PERGENT_SCAN_KEEP_PROXY`; with `Proxy` set, the 4 proxy names, `NO_PROXY` with the callback host and `PERGENT_SCAN_KEEP_PROXY=true`.
  - `TestLaunchSecurityRefuses`: a short `Commit`; a `Prior` over 800 KiB.
  - `TestLaunchSecurityCleansUp`: a Secret create that fails deletes the Job.
  - `TestSecurityFailure`: table on fake Jobs — evicted for storage, OOMKilled, `DeadlineExceeded`, gone; each gives its text.
- [ ] **Step 2: Run them and see them fail**: `go test -count=1 ./internal/kube/ -run '^(TestSecurityJobName|TestBuildSecurityJob|TestSecurityJobNoProxy|TestLaunchSecurityRefuses|TestLaunchSecurityCleansUp|TestSecurityFailure)$'`
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them and see them pass**; `go vet ./internal/kube/`.
- [ ] **Step 5: Commit** `feat(kube): the security scan Job`

---

### Task 8: Engine: start, run, sweep, cancel

**Files:**
- Create: `internal/engine/security.go`, `internal/security/start.go`
- Modify: `internal/engine/engine.go:63-90` (fields), `internal/engine/reconcile.go:330` (`removeProject` cancels the repository's scan), `cmd/pergent-sh/main.go` (start `RunSecurityScans` beside `RunScans`)
- Test: `internal/engine/security_test.go`

**Interfaces:**
- Consumes: Tasks 1, 2, 4, 6, 7; `e.graphTarget(ctx, instance, project, false)` (`engine/graphs.go:182`; no token) for the branch and its newest commit at Start; `chain.Resolve`, `pastAdmin` (`engine/chain.go:69`).
- Produces:
  - Fields: `Engine.SecurityEgress bool`, `Engine.SecurityImage string` (from `cfg.Jobs.SecurityImage`).
  - In `internal/security` (the dashboard must not import the engine: the engine's tests import the dashboard), `start.go`: errors `ErrOff` (`Start scan is off: an admin must set securityJobEgress.cidrs in the Helm values first`), `ErrPaused` (`the repository is paused`), `ErrBusy`, `ErrQuota` (`the monthly token quota is used up`), `type TeamLimitError struct{ Team string }` (its `Error()` is the review start's team-limit 409 text); `type Start struct { Instance, Project, Actor string; Admin bool }`. Plan 2's Task 1 maps them to answers.
  - `func (e *Engine) RequestSecurityScan(ctx context.Context, s security.Start) (store.SecurityScan, error)`: `ErrBusy` comes with the active scan; egress, then `settings().GetAny` (paused, not on), then the quota rules of `actions.refusal` / `adminRefusal` (`actions/actions.go:185-243`) on `chain.LoadScan`'s input: a used-up repository quota refuses a non-admin; `chain.Resolve` with the instance quota set; a hold (or a used-up instance quota and no entry) refuses a non-admin; an admin resolves past `PastAdminLimits`, and a hold left after it is the team-limit error. Then the default branch and its newest commit (`graphTarget`, with no token; a failure refuses the start with its reason), then `QueueSecurityScan` with them and the repository's settings, then wake.
  - `func (e *Engine) StartOff() string`: the reason Start scan is off, or `""`.
  - `func (e *Engine) CancelSecurityScan(ctx context.Context, instance, project, id string) (store.SecurityScan, error)`
  - `func (e *Engine) RunSecurityScans(ctx context.Context)`: the readiness loop shape (`engine/readiness.go:132-175`): launch on a wake and every `securityPassInterval` (10 s), sweep every `securitySweepInterval` (1 min). Both are package variables.
  - `const securityCancelGrace = 2 * time.Minute`, `securityDeadlineGrace = 10 * time.Minute`, `securityLaunchTimeout = 30 * time.Second`.

Launch (`startSecurityScan`), each stop fails the scan with its reason:
1. `ClaimSecurityScan(ctx, e.Config.Security.MaxRunning, now, now.Add(securityLaunchTimeout+time.Minute))`: the deadline until the Job exists.
2. The repository again: gone or paused fails it. The chain (`LoadScan`) and `chain.Resolve`; an admin scan with a hold resolves with `pastAdmin`; no entry: `no model of the chain can be used`.
3. `quota.ScanBudget`; `left <= 0` while limited: `the monthly token quota is used up`.
4. The prior file (`security.BuildPrior` of `LastCompleteSecurityState` and `DismissedSecurityFindings`; none when both are empty), a token (`mintToken`), `StartSecurityJob`, then `LaunchSecurity`. `ResultURL` = `callback_url` + `/security-scans/<id>/result`, `SourceURL` = `callback_url` + `/security-scans/<id>/source`. `Deadline` = `timeout + securityDeadlineGrace` (no clone time: the download is part of pergent's run).
5. Audit nothing here (the dashboard audits the start).

Sweep: a scan with no Job past its deadline fails `the scan stopped before its Job started; start it again`; a Job that failed or is gone fails with `SecurityFailure`'s text; past the deadline with no post: `the scan took longer than its timeout`, and the Job is deleted; a cancel with no post after `securityCancelGrace` ends `partial`, reason `cancelled`. Finished scans within `logWindow` get their Job log once into the carrier run's `run_logs` (`captureJobLog` with a `jobLog` whose key is the scan ID).

Cancel: maintainers and admins (the dashboard checks); records `cancel_requested_at`, takes the log, then deletes the Job, so pergent gets `SIGTERM` and posts `partial`. A queued scan ends at once as `partial`, `cancelled`.

- [ ] **Step 1: Write the failing tests** on one `newTestEnv` per test, fake clock, the kube fake clientset, no real Job:
  - `TestSecurityStartOffWithoutEgress`: `RequestSecurityScan` is `security.ErrOff`; `StartOff` gives the text above.
  - `TestSecurityStartReadsCommit`: Start stores the fake platform's default branch and head commit; no platform call happens at launch.
  - `TestSecurityStartRefusals`: table on one env — paused repository; a default branch the platform can't read; a second start (busy, with the scan); a used-up repository quota for a maintainer (`security.ErrQuota`) and the same for an admin (queued, `PastQuota` true); a team's own limit used up for an admin (`*security.TeamLimitError`).
  - `TestSecurityLaunchCap`: `security.max_running` 1, 2 repositories queued: one Job after a pass; the second after the first finishes.
  - `TestSecurityLaunchJob`: the Job has the security image, the label, the chosen entry's model, `PERGENT_TOKEN_BUDGET` = the smallest left of a repository quota and a model limit, `--head` = the target commit, and `/in/prior.json` with the last complete scan's state and the dismissed findings, and `--head` = the commit stored at Start.
  - `TestSecurityLaunchNoBudgetWithoutLimits`: no quota and no limit → no `PERGENT_TOKEN_BUDGET`.
  - `TestSecurityNoFallback`: entry 1 has a live quota mark at launch → the Job runs on entry 2; after a failed result, no new scan or Job exists.
  - `TestSecuritySweep`: table — no Job past the deadline; a failed Job; past the deadline (Job deleted); cancel with no post after the grace → `partial`, `cancelled`.
  - `TestSecurityCancel`: a running scan's Job is deleted and `CancelRequestedAt` set; a queued one ends `partial` at once; a finished one is a conflict.
  - `TestSecurityRemoveCancels`: `removeProject` cancels the repository's running scan and keeps its rows.
- [ ] **Step 2: Run them and see them fail**: `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run '^(TestSecurityStartOffWithoutEgress|TestSecurityStartReadsCommit|TestSecurityStartRefusals|TestSecurityLaunchCap|TestSecurityLaunchJob|TestSecurityLaunchNoBudgetWithoutLimits|TestSecurityNoFallback|TestSecuritySweep|TestSecurityCancel|TestSecurityRemoveCancels)$'`
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them and see them pass**; `go vet ./internal/engine/ ./cmd/pergent-sh/`.
- [ ] **Step 5: Commit** `feat(engine): start, run and cancel security scans`

---

### Task 9: The result and source routes

**Files:**
- Create: `internal/callback/security.go`, `internal/callback/security_source.go`
- Modify: `internal/server/server.go:42-60` (`Deps.SecurityScans`, `Deps.SecuritySources`, `Deps.SecuritySourceLimit`), `:107-130` (both routes and their doc lines)
- Modify: `internal/engine/security.go` (`CompleteSecurityScan`, `SecurityTooLarge`, `withholdSecurity`, `OpenSecuritySource`, `SecuritySourceTooLarge`)
- Modify: `internal/gitlab/` (`Archive`), `internal/github/` (`Tarball`), `internal/forge/forge.go:205-225` (`ArchiveReader`), `internal/forge/gitlabforge/gitlabforge.go`, `internal/forge/githubforge/githubforge.go`, `cmd/pergent-sh/connect.go:121` (an `archiveReader` beside `sourceReader`)
- Test: `internal/callback/security_test.go`, `internal/callback/security_source_test.go`, `internal/engine/security_result_test.go`, `internal/gitlab/archive_test.go`, `internal/github/archive_test.go`

**Interfaces:**
- Produces:
  - `type Outcome int` with `Stored`, `Conflict`, `Unauthorized`, in `internal/security` (as `readiness.ScanOutcome`).
  - `type SecurityResults interface { CompleteSecurityScan(ctx context.Context, id, token string, body []byte) (security.Outcome, error); SecurityTooLarge(ctx context.Context, id, token string) error }`
  - `type SecurityHandler struct { Scans SecurityResults; Log *slog.Logger }` serving `POST /security-scans/{id}/result` through `oneShot` with `limit: security.MaxBodyBytes`.
  - `func (e *Engine) CompleteSecurityScan(ctx context.Context, id, token string, body []byte) (security.Outcome, error)`
  - Source, reusing the skill install's platform reader (`forge.SourceReader`, `skillfetch.Fetcher.Readers`, `cmd/pergent-sh/connect.go:121`): `func (c *gitlab.Client) Archive(ctx context.Context, projectID int64, sha string) (io.ReadCloser, error)` (`GET /projects/:id/repository/archive.tar.gz?sha=`, through `stream`, `gitlab/client.go:250`); `func (c *github.Client) Tarball(ctx context.Context, owner, repo, sha string) (io.ReadCloser, error)` (`GET /repos/{owner}/{repo}/tarball/{sha}`, through `stream`, `github/ci.go:192`, which drops the token on the redirect to the download host); `type ArchiveReader interface { Archive(ctx context.Context, project, commit string) (io.ReadCloser, error) }` in `forge`, on both `Forge`s (404 or 403 is `ErrSourceNotFound`); `Engine.Archives func(instance string) (forge.ArchiveReader, bool)`.
  - `type SecuritySources interface { OpenSecuritySource(ctx context.Context, id, token string) (io.ReadCloser, security.Outcome, error); SecuritySourceTooLarge(ctx context.Context, id, token string) error }`; `type SecuritySourceHandler struct { Sources SecuritySources; Limit int64; Log *slog.Logger }` serving `GET /security-scans/{id}/source`: 405 for another method, 401 for a missing or wrong bearer (checked against the scan row), 409 when the scan is not running, 502 when the platform can't give the archive; else 200 `application/gzip`, copied with `io.Copy` through `io.LimitReader(Limit+1)`. It first sets a write deadline of 30 minutes (`http.NewResponseController`), so the server's 60 s `WriteTimeout` does not cut it. At `Limit` it stops the stream, ends the response and calls `SecuritySourceTooLarge`, which fails the scan with `the source is over the size limit` (Job deleted). Nothing is stored; neither the token nor any byte of the archive is logged.
  - `func withholdSecurity(env *security.Envelope, secrets []string) int`: every string holding a secret (`holdsSecret`, `engine/readiness.go:668`), in `reason`, in each `state` file (the file is dropped), and anywhere in the SARIF (keys and values; re-encoded), becomes `security.Withheld`. Returns how many.

`CompleteSecurityScan` order: scan not found → Unauthorized; not running → Conflict; token hash no match → Unauthorized (so a review's token, or any other, is refused). Then `ParseEnvelope`; secrets = `HeldSecrets` + the deploy token + this scan's token; `withholdSecurity`; `reason` as one line of at most 500 bytes (`skillfetch.JobText`); `ReadSARIF`; skill from `security.SkillOf`. Unusable envelope or SARIF (over 8 MiB included) → `failed`, reason `pergent sent a result pergent.sh couldn't use: <why>`, no findings. `Capped` with `complete` → `partial`, reason `more than 5000 findings: the first 5000 are kept`. A cancel requested → reason `cancelled`, status `partial` unless `failed`. A `state` file over `security.MaxStateBytes`, or files past that total, are not kept (logged). Then `FinishSecurityScan` (with the envelope's `commit` and `usage.source`), `RecordScanUsage(env.Usage.Rows(provider, model))`, wake the loop (it takes the log). An over-size body with the right token fails the scan `the scan Job sent a result over 12 MiB`.

- [ ] **Step 1: Write the failing tests**:
  - `TestSecurityHandler` (callback, no DB, a fake `SecurityResults`): 405 for GET, 401 with no bearer, 413 over the limit (and `SecurityTooLarge` called), 204 / 409 / 401 by outcome; the token never in the log.
  - `TestCompleteSecurityScan` (engine, one env, subtests): a complete fixture → findings stored, `run_usage` row, 204; a second post → Conflict; a review run's token → Unauthorized; the scan's token on `/runs/<carrier>/result` (the review handler) → 401.
  - `TestSecurityWithholdsSecrets`: a held LLM key in the reason, inside a SARIF `message.text`, inside a SARIF property key and in a state file → none stored; the reason and message read `security.Withheld`; the state file is gone.
  - `TestSecurityUnusableResult`: subtests — SARIF version `2.0.0` → `failed` with the unusable reason, no findings, its usage recorded; an envelope of kind `review` → `failed`, no findings, no usage.
  - `TestSecurityCancelledPost`: cancel, then a `partial` post with reason `terminated` → stored `partial`, reason `cancelled`.
  - `TestSecuritySourceHandler` (callback, no DB, a fake `SecuritySources`): 405, 401 without and with a wrong bearer, 409 for a finished scan, 502 for `ErrSourceNotFound`, 200 with the body and `application/gzip`.
  - `TestSecuritySourceLimit` (callback, no DB): a body of `Limit+1` bytes sends at most `Limit` bytes and calls `SecuritySourceTooLarge` once; `Limit` bytes exactly is a full answer.
  - `TestOpenSecuritySource` (engine, one env, fake GitLab and fake GitHub): the stored commit is the one asked for; a review run's token is 401; the scan's token after it finished is 409.
  - `TestGitLabArchive`, `TestGitHubTarball` (no DB, `httptest`): the path and query; on GitHub the redirect gets no `Authorization` header.
  - `TestSecurityCappedIsPartial`: 5001 results with `complete` → `partial`, open findings of the last scan not fixed.
  - `TestSecurityResultFields`: the envelope's `commit` and `usage.source` are stored; 2 models give 2 usage rows; a state file of 600 KiB is not kept.
- [ ] **Step 2: Run them and see them fail**: `go test -count=1 ./internal/callback/ -run '^(TestSecurityHandler|TestSecuritySourceHandler|TestSecuritySourceLimit)$'`, `go test -count=1 ./internal/gitlab/ -run '^TestGitLabArchive$'`, `go test -count=1 ./internal/github/ -run '^TestGitHubTarball$'` and `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run '^(TestCompleteSecurityScan|TestSecurityWithholdsSecrets|TestSecurityUnusableResult|TestSecurityCancelledPost|TestSecurityCappedIsPartial|TestSecurityResultFields|TestOpenSecuritySource)$'`
- [ ] **Step 3: Implement**; wire `Deps.SecurityScans: eng`, `Deps.SecuritySources: eng`, `Deps.SecuritySourceLimit: cfg.Security.MaxSourceBytes` and `eng.Archives` in `cmd/pergent-sh/main.go` when the dashboard is on.
- [ ] **Step 4: Run them and see them pass.**
- [ ] **Step 5: Commit** `feat(callback): serve a scan its source and take its result`

---

### Task 10: Findings in reviews

**Files:**
- Modify: `internal/inputs/inputs.go:71-74` (`SecurityFindings []byte`), `:313` (`Size`), `:325-331` (`Fit` drops it first, before `CIResults`)
- Modify: `internal/kube/job.go:303`, `:358-361` (env `PERGENT_SECURITY_FINDINGS=/in/security-findings.md`), `:572` (ConfigMap key `security-findings.md`); `internal/kube/judge.go:87` (the judge drops it)
- Modify: `internal/engine/dispatch.go:678-690` (read it before `Fit`, like the CI results); also every other place that copies or clears `CIResults` (`git grep -n CIResults internal`)
- Test: `internal/inputs/inputs_test.go`, `internal/kube/job_test.go`, `internal/engine/security_review_test.go`

**Interfaces:**
- Consumes: `store.SecurityScanSettingsOf`, `store.OpenSecurityFindingsIn`, `security.ReviewInput`, the changed files as `changedForLinks` (`engine/linked.go:331`) lists them.
- Produces: `func (d *dispatcher) addSecurityFindings(ctx context.Context, plat Platform, repo settings.Effective, mr forge.Change, last store.Run, in *inputs.Inputs)`: only when the repository's switch is on and the stage is not the judge; any read error leaves it out with a warning, never failing or holding the review.

- [ ] **Step 1: Write the failing tests**:
  - `TestFitDropsSecurityFindingsFirst` (no DB): over the limit, `SecurityFindings` goes before `CIResults`.
  - `TestBuildJobSecurityFindings` (no DB): with the input, the env and the ConfigMap key; without, neither; the judge Job has neither.
  - `TestDispatchSecurityFindings` (DB, one env, subtests): switch off → no env; on, with an open finding in a changed file and one elsewhere → the file holds only the first; a dismissed finding is not sent; a store error → the review still dispatches; the finding's state is unchanged after publish.
- [ ] **Step 2: Run them and see them fail**: `go test -count=1 ./internal/inputs/ -run '^TestFitDropsSecurityFindingsFirst$'`, `go test -count=1 ./internal/kube/ -run '^TestBuildJobSecurityFindings$'`, `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run '^TestDispatchSecurityFindings$'`
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them and see them pass**, plus the dispatch golden test of the Job (find it with `git grep -n "golden" internal/kube/*_test.go`) by exact name.
- [ ] **Step 5: Commit** `feat(engine): give reviews the open security findings`

---

### Task 11: Metrics, Grafana and the Helm chart

**Files:**
- Modify: `internal/metrics/metrics.go:20-60` (2 metrics), `internal/store/usage.go:1009` (`MetricsTotals.SecurityScans`, `.SecurityOpen`), `cmd/pergent-sh/metrics.go` (samples)
- Modify: `docs/grafana/pergent-sh.json` (3 panels)
- Modify: `charts/pergent-sh/values.yaml`, `templates/networkpolicy.yaml`, `templates/deployment.yaml`, `templates/_helpers.tpl`, `ci-values.yaml`, `README.md` (run `make readme` if `helm-docs` is on the PATH; else edit the values table by hand in its style), `.github/workflows/ci.yml` (chart checks)
- Test: `internal/metrics/metrics_test.go`, `cmd/pergent-sh/metrics_test.go`, `internal/store/usage_security_test.go`

**Interfaces:**
- Produces:
  - `pergent_security_scans_total` (counter, labels `platform_instance`, `repository`, `status`), help: `Security scans that ended, by status: complete, partial or failed.`
  - `pergent_security_findings_open` (gauge, labels `platform_instance`, `repository`, `severity`), help: `Open security findings, by severity: critical, high, medium or low.`
  - Scan tokens are already in `pergent_tokens_total` with `skill="security-scan"` (Task 5).
  - Chart: `securityJobEgress: {cidrs: [], except: []}` with a `# --` comment (no default; the LLM provider only; never add the GitLab or GitHub host: the scan gets the code from pergent.sh). A second NetworkPolicy `<fullname>-security-jobs` selecting `pergent.sh/security-scan` (Exists): DNS, the callback on 8080, and `cidrs` on 443 and 80 only when the list is not empty. The jobs policy gets `matchExpressions: [{key: pergent.sh/security-scan, operator: DoesNotExist}]`. The Deployment gets `PERGENT_SH_SECURITY_EGRESS=true` only with a non-empty list.

- [ ] **Step 1: Write the failing tests**: `TestSecurityMetricsTotals` (store, DB: 2 complete, 1 failed scan and 3 open findings → the counts), `TestSnapshotSecurity` (cmd: labels and a switched-off repository's label), and the metrics help test the package already has, by exact name.
- [ ] **Step 2: Run them and see them fail**, by exact name as above.
- [ ] **Step 3: Implement**, the 3 Grafana panels (scans by status, open findings by severity, scan tokens from `pergent_tokens_total{skill="security-scan"}`), and the chart. In `ci.yml` add, in the `if` style it uses: no `PERGENT_SH_SECURITY_EGRESS` and no `ipBlock` in the security policy by default; with `--set securityJobEgress.cidrs={192.0.2.0/24}` both appear; the jobs policy always has `DoesNotExist`.
- [ ] **Step 4: Verify**: the tests pass; `helm lint charts/pergent-sh -f charts/pergent-sh/ci-values.yaml`; `helm template t charts/pergent-sh --namespace pergent-sh -f charts/pergent-sh/ci-values.yaml --set securityJobEgress.cidrs={192.0.2.0/24} | grep -c 'pergent.sh/security-scan'` prints 2 or more.
- [ ] **Step 5: Commit** `feat: security scan metrics, panels and chart egress`

---

### Task 12: Docs

**Files:**
- Create: `docs/concepts/security-scan.md`
- Modify: `docs/README.md`, `docs/reference/limits.md`, `docs/reference/http-routes.md` (the result route), `docs/concepts/metrics.md`, `docs/concepts/job-isolation.md`, `docs/concepts/usage.md` and `docs/concepts/token-quota.md` (a scan counts), `docs/reference/configuration.md` (done in Task 4), `docs/upgrading.md` (`## Unreleased`), `README.md` only where it points at the docs

- [ ] **Step 1: Write the docs.** `security-scan.md`: what a scan is and is not (the spec's Goals and Non-goals), who may do what, the settings and their ranges, the model chain rule, the budget, the 3 states and the skill-update line, retention, findings in reviews, the egress rule and the proxy rule, how the code reaches the scan pod (the archive of one commit from pergent.sh, no git token in the pod, never add the git host to `securityJobEgress.cidrs`, no Git LFS files and no submodules, `security.max_source_bytes`), and the risks a reader must know (a prompt injection can post its own result). `limits.md`: one bullet. `upgrading.md` `## Unreleased`: migration `0055` (back up Postgres), the new image, `securityJobEgress.cidrs` must be set before the first scan, `/security-scans/` (the source and the result routes) must be reachable by Jobs like `/runs/`, the 4 new config keys break a rollback (remove them first), and the old release ignores the new tables but counts a scan's usage rows in its review counts. Plan 2 adds the dashboard pages and the API routes to the docs.
- [ ] **Step 2: Gate the prose**: no `→` in UI paths; the private-name scan on the changed files.
- [ ] **Step 3: Commit** `docs: the security scan`
