# Command access (pergent.sh 0.6.2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Only the change's author and the repository's maintainers and owners can use pergent commands, behind a per-repository switch that is on by default. Refusals about who may are answered once and audited.

**Architecture:** A new default-on switch follows the `inline_findings` path end to end (migration, config, settings, store, dashboard, web). In the engine, one gate in `handleCommand` refuses anyone who is neither the author (a strict ID match) nor a maintainer. The author also passes the per-command "write access" checks. `answerCommand` writes one `command_refused` audit entry for every answered refusal about who may, and posts the refusal reply only on a person's first such refusal on a change.

**Tech Stack:** Go, Postgres migrations, React/TypeScript (`web/`).

**Spec:** `docs/superpowers/specs/2026-11-04-command-access-design.md`

## Global Constraints

- **Refusal reply (exact):** `Only the author of this PR and the repository's maintainers can use pergent commands.` On GitLab "PR" reads "MR", through `actions.Term(platform)`.
- **Who counts:**
  - **Author:** the commenter's user ID equals `forge.Change.AuthorID` and both are non-zero. `actions.IsAuthor` stays as it is, for blocking dismissals.
  - **Maintainer or owner:** `forge.AccessMaintain` or above.
- **The author is always allowed**, in both modes, whatever their access. They can re-run and dismiss suggestions, but never dismiss a blocking finding on their own change.
- **Switch:**
  - config `repositories[].commands_author_and_maintainers_only`, dashboard JSON `commandsAuthorAndMaintainersOnly`, settings key `commands_author_and_maintainers_only`;
  - on by default, also for repositories already running;
  - name **Only the author and maintainers use commands**;
  - help text "When on, only the author of a change and the repository's maintainers and owners can use pergent commands on it. When off, anyone with write access can."
- **Audit:** action `command_refused`, once per answered refusal about who may, in both modes. Ordinary mistakes (unknown ID, missing reason, cap, not a finding thread) and throttled commands are not audited.
- **Migration `0019`** adds nullable columns only.
- Public repository: no employer names, internal hostnames or real people. Stage files by path (`.superpowers/` isn't gitignored). DB tests skip locally (no Postgres; don't start one) but must compile.

## Review Focus

1. **Unknown author ID** (`Change.AuthorID == 0` or `cmd.AuthorID == 0`) must not make anyone the author. Task 2 test `TestCommandGateUnknownAuthorIsNotAuthor`.
2. **A retried command** (its required reply failed once) must not write a second audit entry. Task 2 test `TestRefusalAuditedOnceAcrossRetry`.
3. **A read-access author** (for example from a fork) can still re-run and dismiss suggestions with the switch off. Task 2 test `TestAuthorAllowedWhenSwitchOff`.
4. **The author's own blocking dismissal** is still refused with today's `replyAuthor`, and that refusal is audited. Task 2 test `TestAuthorBlockingDismissStillRefusedAndAudited`.
5. **A second refusal of the same person on the same change** gets 👎 and no reply. A first refusal on another change still gets the reply. Task 2 test `TestRefusalRepliedOncePerPersonPerChange`.

---

### Task 1: The switch, from config to the dashboard

**Files:**
- Create: `internal/store/migrations/0019_command_access.sql`
- Modify: `internal/config/config.go`, `internal/settings/settings.go`, `internal/store/dashboard.go`, `internal/store/instances.go`, `internal/dashboard/{api_read,api_write,api_operator,view}.go`, `web/src/api.ts`, `web/src/pages/RepositorySettings.tsx`, `web/src/pages/AdminSettings.tsx`, `web/src/test/fixtures.ts`, plus the tests each of these files already has (`settings_test.go` `allKeys`, store dashboard/instances tests, dashboard api tests, `RepositoryDetail.test.tsx`, `AdminSettings.test.tsx`)

Migration `0019_command_access.sql` (exact):
```sql
-- Whether only a change's author and the repository's maintainers and owners
-- can use pergent commands on it. NULL is the default (on).
ALTER TABLE dashboard_repos ADD COLUMN commands_author_and_maintainers_only boolean;
ALTER TABLE repo_defaults   ADD COLUMN commands_author_and_maintainers_only boolean;
```

**Interfaces:**
- Produces:
  - `config.Repository.CommandsAuthorAndMaintainersOnly *bool`;
  - `settings.KeyCommandsAuthorAndMaintainersOnly = "commands_author_and_maintainers_only"`;
  - `settings.Effective.CommandsAuthorAndMaintainersOnly bool`, with built-in default `true`;
  - `store.DashboardRepo.CommandsAuthorAndMaintainersOnly *bool` and `store.RepoDefaults.CommandsAuthorAndMaintainersOnly *bool`;
  - dashboard JSON `commandsAuthorAndMaintainersOnly`, error text `commandsAuthorAndMaintainersOnly must be true, false or null`;
  - in the Admin defaults view: `DefaultValues`/`config`/`pinned` include it, and the web form sends `null` when it equals the config value.

Follow `inline_findings` everywhere it appears (`git grep -n -i "inlineFindings\|inline_findings\|InlineFindings"`): add the new column at the END of each column list. Web: a `Setting` with a `Switch` labelled **Only the author and maintainers use commands**, with the help text from Global Constraints. It goes in Repository → Settings under **Reviews**, after "Inline findings", and in Admin → Settings after "Inline findings" with the 0.6.1 "From config" hint and **Use config value**.

- [ ] **Step 1: Write the failing tests:**
  - config: the YAML key parses; absent is nil.
  - settings: the built-in default is true; config false overrides; a dashboard row overrides repo defaults; `allKeys` includes the key.
  - store: round-trips extend `TestDashboardReposCRUD`, `TestUpdateDashboardRepo` and `TestRepoDefaultsRoundTrip`.
  - dashboard: the setting stores false; null clears; `"x"` gives a 400 with the exact message; reset clears it; Admin defaults include it in `config`/`pinned`.
  - web: toggling sends `{commandsAuthorAndMaintainersOnly: false}`; Admin sends `null` when it equals the config.
- [ ] **Step 2: Run** the focused Go and web tests. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go vet ./... && go test ./...`, then `cd web && npm test && npm run build`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: the "only the author and maintainers use commands" setting`.

---

### Task 2: The gate, the author's access, the refusal reply and the audit

**Files:**
- Modify: `internal/engine/commands.go`, `internal/store/commands.go` (one query)
- Test: `internal/engine/commands_test.go`, `internal/engine/e2e_commands_test.go`, `internal/store/commands_test.go`

**Interfaces:**
- Consumes: `settings.Effective.CommandsAuthorAndMaintainersOnly` (Task 1).
- Produces:
  ```go
  // whyNotAuthorOrMaintainer is the refusal of the command gate.
  const whyNotAuthorOrMaintainer = "not the author or a maintainer"
  // replyOnlyAuthorAndMaintainers is the gate's refusal reply.
  func replyOnlyAuthorAndMaintainers(term string) string // "Only the author of this " + term + " and the repository's maintainers can use pergent commands."
  // isChangeAuthor reports whether the commenter opened the change: both IDs known and equal.
  func (c *commandRun) isChangeAuthor() bool
  ```
  and in internal/store/commands.go:
  ```go
  // RefusedBefore reports whether an earlier, finished command of the same
  // author on the merge request ended with outcome.
  func (s *Store) RefusedBefore(ctx context.Context, mr MRKey, authorID int64, outcome string, before time.Time) (bool, error)
  ```

Behaviour:
1. **The gate.** In `handleCommand`, after the change is read and found open, and before the `switch cmd.Kind`, read the commenter's access once (`c.access`) and keep it on the `commandRun` for the per-command rules. When `repo.CommandsAuthorAndMaintainersOnly` is true and the commenter is neither `isChangeAuthor()` nor `access >= forge.AccessMaintain`, the command is refused with `whyNotAuthorOrMaintainer`:
   - the reply is `replyOnlyAuthorAndMaintainers(changeTerm(cmd.Platform))`, in the thread when the command was written in one (the existing `replyOn` routing);
   - if `RefusedBefore(mr, cmd.AuthorID, "refused: "+whyNotAuthorOrMaintainer, cmd.CreatedAt)` is true, the verdict has no reply, 👎 only, and is not `replyRequired`;
   - this applies to every kind, `help` and unknown commands included.
2. **The author's access.** The existing `access < forge.AccessWrite` checks in `rerun`, `dismissByID` and `dismissInThread` let `isChangeAuthor()` through, in both modes. `dismissFinding`'s blocking rules are unchanged.
3. **The audit.** A verdict whose outcome is a refusal about who may gets one `command_refused` audit entry, with detail `mr=<iid> head=<sha> command=<kind>` and the commenter as actor. Those outcomes are `"refused: " + whyNotAuthorOrMaintainer`, `"refused: no permission"` and `"refused: author of the change"`, in both modes; check the code for the exact existing `why` strings and use those. Write it in `answerCommand`, after the reply step succeeded and right before `FinishCommand`, so a command retried because its reply failed is audited once. Carry the head SHA on the verdict, e.g. `verdict.auditHead string`, set when such a refusal is made.

- [ ] **Step 1: Write the failing tests.** Engine tests are DB-backed with the fake GitLab, like the existing command tests. Name each and assert:
  - `TestCommandGateRefusesDeveloper`: switch on; a Developer (30) who isn't the author sends `review`. The result is 👎, the exact MR reply, no re-run queued, and one `command_refused` audit entry.
  - `TestCommandGateAllowsAuthorWithReadAccess`: switch on; the author with Reporter (20) sends `review`. A re-run is queued.
  - `TestCommandGateAllowsMaintainer`: a Maintainer (40) who isn't the author can re-run.
  - `TestCommandGateUnknownAuthorIsNotAuthor`: `Change.AuthorID == 0`; a Developer is refused.
  - `TestCommandGateRefusesHelp`: switch on; a Developer's `help` is refused with the reply.
  - `TestRefusalRepliedOncePerPersonPerChange`: a Developer's second refusal on the same MR gets 👎 and no reply, and is still audited. Their first refusal on another MR gets the reply.
  - `TestRefusalAuditedOnceAcrossRetry`: the first reply POST fails (500), the command is retried and the reply succeeds. Exactly one `command_refused` entry is written.
  - `TestAuthorAllowedWhenSwitchOff`: switch off; the author with Reporter (20) re-runs and dismisses a suggestion. A Reporter who isn't the author is refused with `replyNoPermission` and audited.
  - `TestAuthorBlockingDismissStillRefusedAndAudited`: switch on; the author, a Maintainer, dismisses a blocking finding on their change. The result is `replyAuthor` plus one audit entry.
  - `TestThreadCommandGate`: switch on; a Developer's `dismiss <reason>` in a finding's thread is refused, with the reply in the thread.
  - store `TestRefusedBefore`: the outcome matches; another author doesn't; another MR doesn't; an unfinished command doesn't.
- [ ] **Step 2: Run** `go test ./internal/engine/ ./internal/store/ -run 'Gate|Refus|Author'`. Expected: FAIL to compile, or skip without a DB; desk-check them.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go vet ./... && go test ./...`. Expected: PASS (DB tests skip locally).
- [ ] **Step 5: Commit** `feat: only the author and maintainers use commands; audit who-may refusals`.

---

### Task 3: README and upgrade notes

**Files:**
- Modify: `README.md`

Content:
- **Commands section:** the permission table and the "Who can" lines get the new rule:
  - with the switch on (the default), only the change's author and the repository's maintainers and owners can use commands;
  - the author always can re-run and dismiss suggestions, whatever their access;
  - blocking dismissals are unchanged;
  - others get 👎 and, the first time on a change, the refusal reply;
  - every refusal about who may is in the audit log as `command_refused`.
- **The config row** `repositories[].commands_author_and_maintainers_only` (default `true`), and the setting in the dashboard settings list.
- **"Upgrading to 0.6.2"** above 0.6.1:
  1. Commands are now limited to the change's author and the repository's maintainers and owners, also on repositories already running. Turn it off per repository (**Only the author and maintainers use commands**, or `commands_author_and_maintainers_only: false`) to let anyone with write access use them, as before.
  2. Migration `0019` runs at startup and adds nullable columns only, so 0.6.1 keeps working on a migrated database.
  3. To roll back to 0.6.1, first remove `commands_author_and_maintainers_only` from the config, because 0.6.1 refuses unknown config keys.

- [ ] **Step 1:** Write it, matching the README's voice.
- [ ] **Step 2:** Scan everything the branch changed, except this plan, for employer names and hostnames: `git diff main --name-only -- . ':!docs/superpowers/plans/2026-11-04-command-access.md' | xargs grep -inE "golabs|gojek|gopay[^t]|goto"`. Expected: no output.
- [ ] **Step 3: Commit** `docs: command access`.
