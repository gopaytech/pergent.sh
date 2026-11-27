# Security scan (pergent.sh dashboard): implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The dashboard API for security scans, and the Security tab, the finding and scan pages, and the Security scan settings card in the web app.

**Architecture:** The API routes hang off the repository's `{rest...}` routes and call the engine through a small interface, as the readiness scan does. The web app adds one tab right after **Reviews**; the finding and scan pages are the same tab with `&finding=<id>` or `&scan=<id>` in the address, because the project path has slashes.

**Tech Stack:** Go (`internal/dashboard`), `openapi.yaml`, React 18 + TanStack Query + Vitest, shadcn/ui on Tailwind.

**Spec:** `docs/superpowers/specs/2026-11-27-security-scan-design.md`. This is plan 2 of 2. It needs Tasks 1 to 10 of plan 1, `docs/superpowers/plans/2026-11-27-security-scan-service.md` (the store, `internal/security`, the engine). Read plan 1's Global Constraints: they apply here too.

## Global Constraints

- Viewers read everything (tab, scans, findings, SARIF). Maintainers and admins start, cancel, dismiss, restore and change the settings. The scan chain follows the repository chain's write rule (admins and maintainers of the owner team).
- An admin's start goes past the quotas and the admin limits, as an admin's **Start review** does.
- Audit: `security_scan_started`, `security_finding_dismissed`, `security_finding_restored` (the spec's 3), plus `security_scan_cancelled`, `settings_updated` and `chain_changed`.
- Copy, verbatim: the tab is `Security`; the skill line is `Skill updated: some findings may show as new`; the label is `Needs validation`; the cancel reason shown is `cancelled`; Start off reads the engine's `security.ErrOff` text.
- No new web dependency. No prettier (`web/` has none): format by hand; eslint is the gate.
- Tests: Go by exact name, one package; web only the files you changed: `cd web && npx vitest run <files>`, then `npm run typecheck` and `npm run lint`. Never a bare `npx vitest run`. A new query's route goes in each test file's `routes()` (the fake API answers an unlisted route 404 and the page then shows an alert).
- Web tests: `vi.useFakeTimers()` where a page polls, no long `waitFor` timeouts, under 500 ms each.

## Review Focus

1. **A repository whose path ends in `/security`.** Its own detail and its settings must still be served, not read as a security route. (Task 1 `TestSecurityPathPrefersRepository`.)
2. **A running scan when the page opens.** The tab must show Running and **Cancel**, poll until it ends, then stop polling. (Task 3 test "polls while running and stops after".)
3. **A finding of another repository by ID.** The API must answer 404, never the finding. (Task 1 `TestSecurityRoutesRoles`.)
4. **A scan whose SARIF was dropped by retention.** **Download SARIF** must not show, and the route answers 404. (Task 1 `TestSecuritySARIFDownload`; Task 5 test "no download once dropped".)
5. **A viewer on a paused repository.** No Start, Cancel, Dismiss or settings edit is shown, and the reason is not an error. (Task 3 and Task 4 tests.)

---

### Task 1: Dashboard API

**Files:**
- Create: `internal/dashboard/api_security.go`
- Modify: `internal/dashboard/api_read.go:1196-1290` (`repository`), `internal/dashboard/api_write.go:705-760` (`repositoryPost`, `repositoryPut`): the security paths are tried **first**, and only when their project part is a visible repository
- Modify: `internal/dashboard/dashboard.go:100-125` (the `SecurityScans` interface, `Options.SecurityScans`), `internal/dashboard/view.go` (the views), `cmd/pergent-sh/main.go` (pass the engine)
- Modify: `openapi.yaml`, `internal/dashboard/openapi_test.go` (`openAPISchemas`, enums)
- Test: `internal/dashboard/api_security_test.go`, `internal/engine/e2e_security_test.go`

**Interfaces:**
- Consumes: plan 1's store (Tasks 1, 2), `security` package (Tasks 3, 8), `quota`/`chain` (Task 6); `d.targetRepo`, `refusePaused`, `d.audit`, `d.repoChoices`, `repoClears`, `usageTokens`.
- Produces:
  - `type SecurityScans interface { RequestSecurityScan(ctx context.Context, s security.Start) (store.SecurityScan, error); CancelSecurityScan(ctx context.Context, instance, project, id string) (store.SecurityScan, error); StartOff() string }`. Nil: every security route is 404.
  - Routes, under `/api/repositories/{instance}/{project...}`:

| Method and suffix | Who | Answer |
|---|---|---|
| GET `/security` | viewer | `SecurityView` |
| GET `/security/findings?state=&severity=&rule=&q=` | viewer | `SecurityFindingsView` (at most 1000, `truncated`) |
| GET `/security/findings/{id}` | viewer | `SecurityFindingView` |
| POST `/security/findings/{id}/dismiss` `{reason, comment}` | maintainer | 200 `SecurityFindingView`; 400 for a bad reason or a comment over 1000 characters |
| POST `/security/findings/{id}/restore` `{}` | maintainer | 200 `SecurityFindingView` |
| GET `/security/scans` | viewer | `SecurityScansView` (newest 100) |
| GET `/security/scans/{id}` | viewer | `SecurityScanDetailView` |
| GET `/security/scans/{id}/sarif` | viewer | `application/sarif+json`, `Content-Disposition: attachment; filename="<last path part>-scan-<n>.sarif"`; 404 once dropped |
| POST `/security/scans` `{}` | maintainer | 202 `{scan}`; 409 `{error, scan?}` for off, paused, busy, quota or team limit |
| POST `/security/scans/{id}/cancel` `{}` | maintainer | 200 `{scan}`; 409 when it has ended |
| GET, PUT `/security/settings` `{maxSteps, subagentMaxSteps, timeoutSeconds, findingsInReviews}` | viewer reads, maintainer writes | `SecuritySettingsView`; 400 out of range; `null` is the default |
| GET, PUT `/security/chain`, POST `/security/chain/reset` | as the repository chain (`api_chains.go:538-600`) | `ChainView` |

  - Views (`view.go`, JSON in lower camel case): `SecurityView { CanStart bool; StartOff string; CanManage bool; Running, Last *SecurityScanView; Counts SecurityCountsView; Start SecurityStartView }`; `SecurityCountsView { Critical, High, Medium, Low, NeedsValidation int }`; `SecurityStartView { Model string; MaxSteps, SubagentMaxSteps int; StepsFromSkill bool; TimeoutSeconds int; LastTokens, LastSeconds *int64 }`; `SecurityScanView { ID string; Number int64; State, Reason, StartedBy string; QueuedAt time.Time; StartedAt, FinishedAt *time.Time; DurationSeconds *int64; Branch, Commit, Model, SkillName, SkillCommit string; SkillUpdated bool; Coverage *SecurityCoverageView; Tokens *UsageTokensView; NewCount, FixedCount int; HasSARIF, CancelRequested bool }`; `SecurityCoverageView { Done, Planned int }`; `SecurityScansView { Scans []SecurityScanView }`; `SecurityScanDetailView { Scan SecurityScanView; Log string; New, Fixed []SecurityFindingRowView }`; `SecurityFindingRowView { ID int64; State, Severity, RuleID, Title, File string; StartLine, EndLine int; NeedsValidation bool; FirstSeenAt time.Time; FirstScan int64 }`; `SecurityFindingsView { Findings []SecurityFindingRowView; Truncated bool }`; `SecurityFindingView { Finding SecurityFindingRowView; Description, Exploit, FixHint string; Tags []string; CodeURL string; Dismissal *SecurityDismissalView; History []SecurityEventView; CanManage bool }`; `SecurityDismissalView { Reason, Comment, By string; At time.Time }`; `SecurityEventView { Kind string; Scan int64; Actor, Reason, Comment string; At time.Time }`; `SecuritySettingsView { MaxSteps, SubagentMaxSteps, TimeoutSeconds *int; FindingsInReviews bool; Defaults SecurityDefaultsView; CanEdit bool }`; `SecurityDefaultsView { MaxSteps, SubagentMaxSteps, TimeoutSeconds int }`.
  - `CanStart` = role at least maintainer, not paused, no running scan, `StartOff == ""`. `StartOff` = the engine's text, else `the repository is paused` for a paused one, else `""`. `CanManage` = role at least maintainer. `Model` = the entry `chain.Resolve` picks on `chain.LoadScan` now (`""` when none). `CodeURL` = `security.CodeURL` with the platform, the project's clone URL and the last scan's commit.
  - `func parseSecurityPath(rest string) (project string, p securityPath, ok bool)` with `type securityPath struct { Kind string; ID string; Action string }`: matches the tail of `rest` against the shapes above.
  - Audit details: `security_scan_started` `scan=<n> model=<entry name>`; `security_scan_cancelled` `scan=<n>`; `security_finding_dismissed` `fingerprint=<first 12> reason=<reason>`; `security_finding_restored` `fingerprint=<first 12>`; `settings_updated` `security.<field>=<value>`; `chain_changed` `security scan: <names>` or `security scan: inherit`.

- [ ] **Step 1: Write the failing tests** on one `newEnv` per test with `e.dashboardRepo` and `e.aliceOn` (a repository alice cannot see is 404 before the code under test), a fake `SecurityScans`, `t.Parallel()`:
  - `TestSecurityRoutesRoles`: table — a viewer gets 200 on every GET and 403 on every write; a maintainer's writes pass; a finding ID of another repository is 404.
  - `TestSecurityStartAnswers`: 202 and one `security_scan_started`; `security.ErrOff` 409 with its text; paused 409 `the repository is paused`; `ErrBusy` 409 with `scan`; `*security.TeamLimitError` 409; no audit entry on a 409; an admin's request has `Admin: true`.
  - `TestSecurityDismissRestore`: a bad reason 400; dismiss 200 and audited; restore 200 and audited; a second dismiss changes nothing and adds no entry.
  - `TestSecuritySettingsRanges`: table — `maxSteps` 4 and 1001, `subagentMaxSteps` 301, `timeoutSeconds` 599 and 21601 are 400; `null` sets the default; a change is audited as `settings_updated`.
  - `TestSecurityChainRule`: a maintainer of the owner team saves; a maintainer of another team covering the repository is 403 with the repository chain's text; reset is `security scan: inherit`.
  - `TestSecuritySARIFDownload`: the type and the attachment name; 404 once dropped.
  - `TestSecurityPathPrefersRepository`: a repository whose path ends in `/security` still gets its detail and its settings.
  - `TestSecurityScanEndToEnd` (engine package, `newDashboardE2E`): a maintainer starts a scan through the API, one Job carries the security label, the fixture envelope posted with its token is 204, `GET .../security` shows the counts, the repository quota counts the scan, and a dismiss is audited. It is the only test of the wiring from the API to the store; expect it over 2 s and say so in the report.
  - In `openapi_test.go`: each view above in `openAPISchemas`; the scan states, severities, finding states, event kinds and dismiss reasons in `TestOpenAPIEnumsMatchCode`.
- [ ] **Step 2: Run them and see them fail**: `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/dashboard/ -run '^(TestSecurityRoutesRoles|TestSecurityStartAnswers|TestSecurityDismissRestore|TestSecuritySettingsRanges|TestSecurityChainRule|TestSecuritySARIFDownload|TestSecurityPathPrefersRepository)$'` and `PERGENT_SH_TEST_DATABASE_URL=... go test -count=1 ./internal/engine/ -run '^TestSecurityScanEndToEnd$'`. With Postgres down: CI only.
- [ ] **Step 3: Implement.** In `openapi.yaml` write each path with `x-pergent-access: session` and `x-pergent-pattern`, and `/security-scans/{id}/result` beside `/readiness-scans/{id}/result` (`openapi.yaml:456`) with a `securityScanToken` scheme and the 401, 409, 413 answers. The test does not check request bodies: check them by hand.
- [ ] **Step 4: Run them and see them pass**, plus the offline spec tests: `go test -count=1 ./internal/dashboard/ -run '^(TestOpenAPIRoutesMatchCode|TestOpenAPIWildcardSubroutesAreDocumented|TestOpenAPISchemasMatchGoTypes|TestOpenAPIEveryViewTypeHasASchema|TestOpenAPIEnumsMatchCode|TestOpenAPIDocumentIsValid)$'`; `go vet ./internal/dashboard/ ./cmd/pergent-sh/`.
- [ ] **Step 5: Commit** `feat(dashboard): the security scan API`

---

### Task 2: Web types and paths

**Files:**
- Modify: `web/src/api.ts` (types next to the readiness ones, `api.ts:520`; paths next to `apiRepoGraph`, `api.ts:1717`)
- Modify: `web/src/test/fixtures.ts` (one fixture per view)
- Test: `web/src/api.test.ts`

**Interfaces:**
- Produces: a TypeScript type per Task 1 view with the same names and fields (`SecurityView`, `SecurityScanView`, …); `type SecurityState = "queued" | "running" | "complete" | "partial" | "failed"`; `type Severity = "critical" | "high" | "medium" | "low"`; `type FindingState = "open" | "dismissed" | "fixed"`; `type DismissReason = "false_positive" | "wont_fix" | "used_in_tests" | "mitigated" | "acceptable_risk"`; `const dismissReasonText: Record<DismissReason, string>` (`false positive`, `won't fix`, `used in tests`, `mitigated`, `acceptable risk`). Paths: `apiRepoSecurity`, `apiRepoSecurityFindings(instance, project, filter)`, `apiRepoSecurityFinding(instance, project, id)`, `apiRepoSecurityFindingAction(instance, project, id, "dismiss" | "restore")`, `apiRepoSecurityScans`, `apiRepoSecurityScan(instance, project, id)`, `apiRepoSecurityScanSarif(instance, project, id)`, `apiRepoSecurityScanCancel(instance, project, id)`, `apiRepoSecuritySettings`, `apiRepoSecurityChain`, `apiRepoSecurityChainReset`; page path `paths.repoSecurity(instance, project, { finding?, scan? })`.
- Fixtures: `securityView`, `securityScanComplete`, `securityScanPartial`, `securityScanFailed`, `securityScanRunning`, `securityFindingOpen`, `securityFindingDismissed`, `securitySettings`.

- [ ] **Step 1: Write the failing test** `describe("security paths")` in `api.test.ts`: each path for project `group/sub/app` keeps the slashes and escapes `#`; the findings filter drops empty keys and encodes `q`.
- [ ] **Step 2: Run it and see it fail**: `cd web && npx vitest run src/api.test.ts`
- [ ] **Step 3: Implement**, then check the types against `openapi.yaml` with the PyYAML script of the model-limits PR 2 plan, Task 1 (`docs/superpowers/plans/2026-11-23-model-limits-pr2.md`).
- [ ] **Step 4: Run it and see it pass**; `npm run typecheck`; `npm run lint`.
- [ ] **Step 5: Commit** `feat(web): security scan types and paths`

---

### Task 3: The Security tab: last scan, counts, Start and Cancel

**Files:**
- Create: `web/src/pages/Security.tsx`
- Modify: `web/src/pages/RepositoryDetail.tsx:33` (`Tab` gets `"security"`), `:55-60` (`["security", "Security"]` right after `["reviews", "Reviews"]`, for every role), `:127-145` (render it)
- Test: `web/src/pages/Security.test.tsx`, `web/src/pages/RepositoryDetail.test.tsx`

**Interfaces:**
- Consumes: Task 2 types and paths; `useWrite`, `Tabs`, the dialog parts the app already uses (Radix `Dialog`, see the focus-return note in `ChainEditor.tsx`).
- Produces: `export default function Security({ instance, project, paused }: { instance: string; project: string; paused: boolean })`: reads `?finding=` and `?scan=` and shows Task 4's or Task 5's page, else the summary, the findings list (Task 4) and the scans list (Task 5).

The summary card:
- **Last scan**: state, duration, tokens, branch and short commit, skill name, and the counts as `Critical n · High n · Medium n · Low n · Needs validation n`. A partial or failed scan shows its reason. `SkillUpdated` shows `Skill updated: some findings may show as new`. No scan yet: `No scan yet.`
- **Start scan** for `canManage`; disabled with the `startOff` text beside it when `startOff` is set. It opens a confirm box with the model, the step limits (`from the skill (600)` when `stepsFromSkill`), the timeout, and `Last scan: <tokens> tokens in <duration>` (or `No earlier scan to compare.`). Confirm posts and closes; a 409 shows the answer's `error` in the box.
- While `running` is set: the button reads `Running` (disabled), **Cancel** shows for `canManage` and asks first, and the page polls `GET .../security` every 10 s until `running` is null.

- [ ] **Step 1: Write the failing tests** with `fakeApi` and `renderApp`, fake timers where it polls:
  - `RepositoryDetail.test.tsx` "the Security tab comes right after Reviews": the tab names, in order, for a viewer and for a maintainer.
  - `Security.test.tsx`: "shows the counts and the last scan"; "a partial scan shows its reason"; "a failed scan shows its reason"; "Skill updated line"; "a viewer gets no Start scan or Cancel"; "Start scan off shows the reason" (uses a `startOff` text; the button is disabled); "the confirm box shows the model, limits and last cost, and posts on confirm"; "a 409 shows its error in the box"; "polls while running and stops after"; "Cancel asks first, then posts".
- [ ] **Step 2: Run them and see them fail**: `cd web && npx vitest run src/pages/Security.test.tsx src/pages/RepositoryDetail.test.tsx`
- [ ] **Step 3: Implement.** Add the new routes to each changed test file's `routes()`.
- [ ] **Step 4: Run them and see them pass**; `npm run typecheck`; `npm run lint`.
- [ ] **Step 5: Commit** `feat(web): the Security tab`

---

### Task 4: Findings list and a finding's page

**Files:**
- Create: `web/src/pages/SecurityFindings.tsx`, `web/src/pages/SecurityFinding.tsx`
- Test: `web/src/pages/SecurityFindings.test.tsx`, `web/src/pages/SecurityFinding.test.tsx`

**Interfaces:**
- Consumes: Task 2; `urlState.ts` and `useSearchText.ts` for filters that stay in the address; `Markdown` (lazy) for the description, exploit scenario and fix hint.
- Produces: `SecurityFindings({ instance, project })` and `SecurityFinding({ instance, project, id })`.

List: filters state (default `open`, then `dismissed`, `fixed`, `all`), severity and CWE (a select built from the rules in the answer), and a search box for file or title. Columns: severity, finding (title, with a `Needs validation` badge), file and line, first seen. A row links to `&finding=<id>`. `truncated` shows `Showing the first 1000. Narrow the filters to see more.`

Finding page: title and severity, rule and tags, description, exploit scenario, fix hint, the code location as a link to `codeUrl` (plain text when `""`), the history (found, reopened, fixed, dismissed, restored, each with its scan number, who and when), and for `canManage` **Dismiss** (a dialog with the 5 reasons and an optional comment, at most 1000 characters) or **Restore**. A dismissed finding shows the reason text and the comment.

- [ ] **Step 1: Write the failing tests**: list — "open findings first by default"; "the severity and CWE filters and the search change the query and stay in the address"; "the Needs validation badge"; "the truncated line"; finding — "shows the description, exploit scenario, fix hint and a code link"; "no link when codeUrl is empty"; "the history in order"; "Dismiss needs a reason and posts reason and comment"; "Restore posts and shows the finding open"; "a viewer gets neither button".
- [ ] **Step 2: Run them and see them fail**: `cd web && npx vitest run src/pages/SecurityFindings.test.tsx src/pages/SecurityFinding.test.tsx`
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them and see them pass**; `npm run typecheck`; `npm run lint`.
- [ ] **Step 5: Commit** `feat(web): security findings and a finding's page`

---

### Task 5: Scans list and a scan's page

**Files:**
- Create: `web/src/pages/SecurityScans.tsx`, `web/src/pages/SecurityScan.tsx`
- Test: `web/src/pages/SecurityScans.test.tsx`, `web/src/pages/SecurityScan.test.tsx`

**Interfaces:**
- Consumes: Task 2; `RunLog.tsx` for the log; `TableSort.tsx` is not needed (newest first only).
- Produces: `SecurityScans({ instance, project })`, `SecurityScan({ instance, project, id })`.

List columns: number, status, date, duration, new and fixed counts, and the reason of a partial or failed scan. A row links to `&scan=<id>`. Scan page: status and reason, coverage `done of planned` (none: `Coverage not reported`), usage (the 5 token parts and the total, as the run card shows them), the log, the new and the fixed findings (each a link to its page), and **Download SARIF** (a plain link to the sarif path) only when `hasSarif`.

- [ ] **Step 1: Write the failing tests**: list — "lists scans newest first with new and fixed counts"; "a partial and a failed row show their reason"; scan page — "shows coverage, usage and the log"; "lists new and fixed findings with links"; "Download SARIF links to the file"; "no download once dropped" (`hasSarif: false`).
- [ ] **Step 2: Run them and see them fail**: `cd web && npx vitest run src/pages/SecurityScans.test.tsx src/pages/SecurityScan.test.tsx`
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them and see them pass**; `npm run typecheck`; `npm run lint`.
- [ ] **Step 5: Commit** `feat(web): security scans and a scan's page`

---

### Task 6: The Security scan settings card

**Files:**
- Create: `web/src/pages/SecurityScanSettings.tsx`
- Modify: `web/src/pages/RepositorySettings.tsx:200` (the card after the model chain card)
- Test: `web/src/pages/SecurityScanSettings.test.tsx`, `web/src/pages/RepositorySettings.test.tsx`

**Interfaces:**
- Consumes: Task 2; `ChainEditor` (the same use as `ReviewerOne`, `RepoModelChain.tsx:30-80`, on the security chain paths); `LimitField.tsx` style number fields.
- Produces: `SecurityScanSettings({ instance, project })`.

Rows: **Scan model** (the chain in effect, with Edit and `Inherit the repository's chain`; when inherited it reads `The repository's model chain`), **Max steps, main agent** (5 to 1000, empty = `from the skill (600)`), **Max steps, each sub-agent** (5 to 300, empty = `from the skill (100)`), **Scan timeout** (minutes, 10 to 360, empty = `3 h`), **Use security findings in reviews** (a switch, off by default, with the line `Reviews get the open findings in the files a change touches, as data. They never block a merge.`). Save sends only the 4 fields; `readOnly`/`canEdit` false shows values only.

- [ ] **Step 1: Write the failing tests**: "shows the defaults when nothing is set"; "a value out of range is refused before saving"; "Save sends the 4 fields with null for empty"; "the chain edits on the security chain path and Inherit resets it"; "a viewer sees values and no Edit or Save"; in `RepositorySettings.test.tsx` "the Security scan card is on the Settings tab".
- [ ] **Step 2: Run them and see them fail**: `cd web && npx vitest run src/pages/SecurityScanSettings.test.tsx src/pages/RepositorySettings.test.tsx`
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them and see them pass**; `npm run typecheck`; `npm run lint`.
- [ ] **Step 5: Commit** `feat(web): the Security scan settings card`

---

### Task 7: Dashboard docs

**Files:**
- Modify: `docs/concepts/dashboard.md` (a Security tab section; the Settings card; the audit log list), `docs/concepts/security-scan.md` (link to the tab), `docs/reference/http-routes.md` (the dashboard routes), `docs/upgrading.md` (`## Unreleased`: the new tab, the new API paths)

- [ ] **Step 1: Write the docs.** Name the buttons and texts as the app shows them. Write "the repository's **Settings** tab", never an arrow in a UI path.
- [ ] **Step 2: Gate the prose**: no `→` in UI paths; the private-name scan on the changed files.
- [ ] **Step 3: Commit** `docs: the Security tab`
