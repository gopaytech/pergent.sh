# `failure_status: neutral` on GitLab Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On GitLab, `failure_status: neutral` sets the `pergent` commit status to `success` (never approved), and the give-up status follows `failure_status` on both platforms.

**Architecture:** One mapping change in `gitlabforge.State`. `Engine.giveUp` reads the repository's settings the way `publish` does and picks `forge.StatusNeutral` or `forge.StatusFailed`. Help text and docs follow.

**Tech Stack:** Go (engine, forge), React/TypeScript (two settings pages), Markdown docs.

**Spec:** `docs/superpowers/specs/2026-11-16-gitlab-neutral-success-design.md`

## Global Constraints

- Worktree `/Users/zufardhiyaulhaq/Documents/personal/github/pergent.sh-neutral`, branch `fix/gitlab-neutral-success`. No push, no PR, no tag, no version bump, no release.
- A review that couldn't be done is never approved, and its comment still says it failed.
- Blocking findings are always `failed`, whatever `failure_status` says.
- When pergent.sh can't tell what the repository wants, it fails closed (`failed`).
- Status descriptions stay as they are: "Review failed for N of M skills" on publish, "pergent couldn't post its review" (`gaveUpDescription`) on give-up.
- API reviews (`store.KindAPI`) set no status; their give-up path does not change.
- Docs: plain style, short sentences, no em dashes. Release notes go under `## Unreleased` in `docs/upgrading.md`.
- Public repo: before every commit, run the private-name scan from the agent memory and expect empty output. The owner's screenshots are private: nothing from them goes in code, docs or commits.
- Commit with `git -c user.email=zufardhiyaulhaq@gmail.com commit`, conventional subject, ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Engine tests need the test DB: `PERGENT_SH_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/pergent_test?sslmode=disable'`. Check `nc -z localhost 55432` first; never start a container. Without the DB the engine tests skip, and a green run then proves nothing.

## Review Focus

1. **A run in flight at the upgrade.** Its status is already `running`; `success` must be accepted from there. Pinned by Task 1's publish test (status set after the Job ran) and by the spec's state machine facts. The fake GitLab accepts any transition, so no test can prove GitLab's side; the reviewer checks the mapping against the spec.
2. **The repository is removed between the review and the give-up.** Expect `failed`. Pinned in Task 2 (`TestPublishGiveUpRepositoryGoneIsFailed`).
3. **The settings can't be read at give-up** (DB error). Expect `failed`, logged, and the status still set. Pinned in Task 2 (`TestPublishGiveUpUnreadableSettingsIsFailed`).
4. **A dashboard-managed repository set to neutral** (not in the config). The give-up must read it from the store like `publish` does. Covered by the setup of `TestPublishGiveUpUnreadableSettingsIsFailed`, whose first 9 attempts read a dashboard repo; Task 2 also asserts the neutral path through the config.
5. **GitHub give-up with neutral.** Today it completes the Check Run as `failure`; after Task 2 it is `neutral` with the same title. Pinned in Task 2 (`TestPublishGitHubGiveUpNeutralCheckRun`).

---

### Task 1: Map neutral to `success` on GitLab

The uncommitted sketch in the worktree already holds this task's changes (spec-aligned, full suite green). Keep it, prove the tests fail without the code change, then commit.

**Files:**
- Modify: `internal/forge/gitlabforge/gitlabforge.go` (`State`, `case forge.StatusNeutral:` returns `gitlab.StatusSuccess`; comment above `State` says why `skipped` can't be used)
- Test: `internal/forge/gitlabforge/gitlabforge_test.go` (`TestState`, `TestSetStatus`: neutral expects `gitlab.StatusSuccess` / `"success"`)
- Test: `internal/engine/publish_test.go:526` (`TestPublishNeutralMapsToSkipped` renamed `TestPublishNeutralIsSuccessOnGitLab`, expects `wantStatus(t, env, head, gitlab.StatusSuccess, "Review failed for 1 of 1 skills")`, `wantPublished`, `wantNoApproval`)
- Modify: `docs/reference/configuration.md:48` (`repositories[].failure_status`)
- Modify: `docs/concepts/approval.md:28` (status table row: `` `failed` or `success` (`failure_status: neutral`, still not approved) ``)
- Modify: `docs/upgrading.md` (`## Unreleased` note on the GitLab mapping)

**Interfaces:**
- Produces: `gitlabforge.State(forge.StatusNeutral) == gitlab.StatusSuccess`. Task 2 relies on it for the GitLab give-up test.

- [ ] **Step 1: Confirm the sketch matches the list above**

Run: `git -C /Users/zufardhiyaulhaq/Documents/personal/github/pergent.sh-neutral diff`
Expected: only the six files above, with the changes described. Fix anything that differs.

- [ ] **Step 2: Watch the tests fail without the code change**

Run: `git stash push internal/forge/gitlabforge/gitlabforge.go && go test -count=1 ./internal/forge/gitlabforge/ -run 'TestState|TestSetStatus'; git stash pop`
Expected: FAIL on the neutral case (got `skipped`, want `success`), then the stash is restored.

- [ ] **Step 3: Run the tests with the change**

Run: `go test -count=1 ./internal/forge/gitlabforge/` and, with the DB env var, `go test -count=1 ./internal/engine/ -run 'TestPublishNeutralIsSuccessOnGitLab|TestPublishRepositoryGoneUsesFailed'`
Expected: PASS.

- [ ] **Step 4: Name scan, then commit**

```bash
git add internal/forge/gitlabforge docs/reference/configuration.md docs/concepts/approval.md docs/upgrading.md internal/engine/publish_test.go
git -c user.email=zufardhiyaulhaq@gmail.com commit -m "fix(gitlab): set failure_status neutral as success"
```

### Task 2: Give-up follows `failure_status`

**Files:**
- Modify: `internal/engine/publish.go:719-756` (`giveUp` and its doc comment)
- Test: `internal/engine/publish_test.go` (next to `TestPublishFailedSetsFinalStatusOnce`, line ~1384)
- Test: `internal/engine/github_test.go` (next to `TestPublishGitHubFailedCompletesCheckRun`, line ~1137)
- Modify: `docs/concepts/approval.md` (the `publish_failed` bullet under "Pushes, edits and failures")
- Modify: `docs/upgrading.md` (extend the `## Unreleased` note)

**Interfaces:**
- Consumes: `gitlabforge.State(forge.StatusNeutral) == gitlab.StatusSuccess` (Task 1); `e.settings().Get(ctx, instance, project) (settings.Effective, bool, error)`; `config.FailureStatusNeutral`; `forge.StatusNeutral`, `forge.StatusFailed` (`forge.StatusState`).
- Produces: `func (e *Engine) gaveUpStatus(ctx context.Context, run store.Run, log *slog.Logger) forge.StatusState` in `publish.go`. Returns `forge.StatusNeutral` only when the read succeeds, the repository is found and its `FailureStatus` is `config.FailureStatusNeutral`; otherwise `forge.StatusFailed`. A read error is logged with `log.Warn` (redacted with `e.redact`). `giveUp` calls it after the withdrawal, in its own settle context, and passes the result to `SetStatus` with `gaveUpDescription`. The `KindAPI` branch is untouched.

- [ ] **Step 1: Write the failing tests**

In `publish_test.go`, each built like `TestPublishFailedSetsFinalStatusOnce` (report a clean `reviewed("no_blocking_issues", "a.go")`, `servePublish`, notes answer 422 "Note is too long"):

```go
// TestPublishGiveUpNeutralIsSuccessOnGitLab
env.Engine.Config.Repositories[0].FailureStatus = config.FailureStatusNeutral
publishUntilGivenUp(t, env, id)
wantStatus(t, env, head, gitlab.StatusSuccess, "pergent couldn't post its review")
wantNoApproval(t, env, 1)

// TestPublishGiveUpRepositoryGoneIsFailed
env.Engine.Config.Repositories[0].FailureStatus = config.FailureStatusNeutral
// ... after Report and servePublish:
env.Engine.Config.Repositories = nil
publishUntilGivenUp(t, env, id)
wantStatus(t, env, head, gitlab.StatusFailed, "pergent couldn't post its review")

// TestPublishGiveUpUnreadableSettingsIsFailed
env.Engine.Config.Repositories = nil
// UpsertDashboardRepo{Instance: testInstance, Project: testProjectPath, FailureStatus: &neutral} with neutral := "neutral"
// 9 publishes, each followed by env.SetNow(env.Now().Add(11 * time.Minute));
// then DROP TABLE repo_defaults on a pgx connection with search_path = env.schema
// (the pattern of TestQuotaReadErrorRetries, e2e_quota_test.go:185-199; add CASCADE if a foreign key refuses);
// then one more publish.
// env.Run(id).State == store.PublishFailed
wantStatus(t, env, head, gitlab.StatusFailed, "pergent couldn't post its review")
// env.Logs() contains the Warn message giveUp logs for the read error
```

`TestPublishFailedSetsFinalStatusOnce` already pins `failure_status: failed` at give-up (the test config's default); leave it as is.

In `github_test.go`, `TestPublishGitHubGiveUpNeutralCheckRun`: as `TestPublishGitHubFailedCompletesCheckRun`, but first set `FailureStatus = config.FailureStatusNeutral` on the entry of `env.Engine.Config.Repositories` whose `GitHub == ghInstance`. Assert `run["status"] == "completed"`, `run["conclusion"] == "neutral"`, `run["head_sha"] == head`, `out["title"] == "pergent couldn't post its review"`, no `/reviews` POST, `approvals(t, env)` empty.

- [ ] **Step 2: Run them to verify they fail**

Run (DB env var set): `go test -count=1 ./internal/engine/ -run 'TestPublishGiveUp(NeutralIsSuccessOnGitLab|RepositoryGoneIsFailed|UnreadableSettingsIsFailed)|TestPublishGitHubGiveUpNeutralCheckRun'`
Expected: the two neutral tests FAIL (state `failed` / conclusion `failure`); the two `failed` tests may already pass, and must keep passing. If `UnreadableSettingsIsFailed` fails for a setup reason (the dashboard repo isn't picked up, the drop is refused), fix the setup first.

- [ ] **Step 3: Implement `gaveUpStatus` and call it from `giveUp`**

As in the Interfaces block. Update `giveUp`'s doc comment: "one failed status" becomes the `failure_status` status (`failed` when the settings can't be read or the repository is gone).

- [ ] **Step 4: Run the tests to verify they pass**

Run: the Step 2 command, then `go test -count=1 ./internal/engine/ -run 'TestPublish'`
Expected: PASS.

- [ ] **Step 5: Docs**

- `docs/concepts/approval.md`, the `publish_failed` bullet: the last status is `failure_status` (on GitLab `failed`, or `success` with `neutral`; on GitHub the Check Run `failure` or `neutral`), with the same description and title; `failed` when the settings can't be read or the repository is gone. Everything else in the bullet stays.
- `docs/upgrading.md`, `## Unreleased`: add that a review that couldn't be posted after the retries now also follows `failure_status` (it was always `failed`), on GitLab and GitHub. Still never approved.

- [ ] **Step 6: Name scan, then commit**

```bash
git add internal/engine/publish.go internal/engine/publish_test.go internal/engine/github_test.go docs/concepts/approval.md docs/upgrading.md
git -c user.email=zufardhiyaulhaq@gmail.com commit -m "fix(engine): give-up status follows failure_status"
```

### Task 3: Help text in the settings pages

**Files:**
- Modify: `web/src/pages/RepositorySettings.tsx:539` (the `help` prop)
- Modify: `web/src/pages/AdminSettings.tsx:374` (the help text)
- Leave: `docs/superpowers/specs/2026-10-31-dashboard-mockups/repo-settings.html`

No web test pins the current text (`grep -rn "provider is down" web/src` finds only the two pages), so this task has no new test.

- [ ] **Step 1: Replace both texts with exactly**

`"neutral" won't block merging when a review can't be done. On GitLab the status is success; the review is still not approved.`

In the `.tsx` `help={...}` template literal and in the JSX text, keep each file's existing quoting style.

- [ ] **Step 2: Web gate**

Run from `web/` after `ln -s ../../pergent.sh/web/node_modules node_modules`: `npm run typecheck && npm run lint && npx vitest run && npm run build`. Then remove the `node_modules` link and any `tsconfig*.tsbuildinfo` the build left, and check `git status` shows only the two `.tsx` files (and the embedded build output only if it is tracked).
Expected: all four pass.

- [ ] **Step 3: Name scan, then commit**

```bash
git add web/src/pages/RepositorySettings.tsx web/src/pages/AdminSettings.tsx
git -c user.email=zufardhiyaulhaq@gmail.com commit -m "fix(web): explain failure_status neutral on GitLab"
```

### Task 4: Final gate

- [ ] **Step 1:** `gofmt -l internal/` prints nothing; `go build ./...` and `go vet ./...` pass.
- [ ] **Step 2:** With the DB env var: `go test -count=1 -timeout 30m ./...` (run in the background). Expected: no `FAIL` line.
- [ ] **Step 3:** The house private-name scan (the `git grep` in the agent memory) prints nothing, and `git diff origin/main...HEAD | grep '^+' | perl -CS -ne 'print if /\x{2014}/'` prints nothing (no added em dash).
- [ ] **Step 4:** Report the results. No push, no PR.
