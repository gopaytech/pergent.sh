# Linked Repositories Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Implementers run on **Sonnet** with the `pergent-developer` agent. Task reviews and the final branch review run on **Opus**.

**Goal:** When a change touches paths other repositories depend on, one extra run looks for callers it breaks in the repositories linked to it, off by default.

**Architecture:** pergent.sh holds links, matches them at dispatch, clones the targets in the Job's checkout container, and hands pergent a JSON file through `PERGENT_LINKED_REPOS`. pergent scrubs the clones, runs one impact run with a fixed prompt after the normal review, verifies blocking findings with a prompt of its own, and reports the result as a skill named `impact`. Everything is gated on a per-repository setting.

**Tech Stack:** Go, Postgres migrations, React with Vite and vitest, shell in the Job's init container.

**Spec:** `docs/superpowers/specs/2026-11-07-linked-repositories-design.md`. Read its anti-goals first. They are the failure bar.

## Scope

No release in this plan. It ends at pushed branches and PRs, on the owner's word. pergent.sh doesn't need a new pergent tag to build or test: `impact` arrives as an ordinary skill in the result, and until `defaultImage` moves, turning the feature on gives a `failed` impact check, which is the designed behaviour. Moving the pin is part of a later release. Another agent may be building features, so never assume `main` stands still.

## Hand-offs

| When | Who | What |
|---|---|---|
| **H0**, before Task 1 | Main session | Cut `feat/impact-check` from the latest `origin/master` in `pergent-incremental`, and `feat/linked-repos` from the latest `origin/main` in `pergent.sh-access`, carrying the spec and this plan. Set the migration number to the next one free on `origin/main` and in open PRs: `0029` at the time of writing. |
| **H1**, before each task | Main session | `git fetch`, merge the base when it moved, re-check the migration number. Touch no other agent's branch or worktree. |
| **H2**, after the final review | Owner | Push and open the PRs on the owner's word. Postgres-backed tests first run there. |

## Global Constraints

- Never run docker, orbctl or anything that starts a container. No `gh`, `glab`, `curl` or other network step in a task. No push, tag or PR. No disk or cache cleanup. Delete nothing outside your own scratch. Don't run `go get`. The disk is tight: avoid `go test -race` and large scratch copies.
- Tests marked **[CI]** need Postgres. Write them with the same care, confirm they compile with `go vet ./...`, and report them as not run.
- **pergent gate:** `gofmt -l .`, `go vet ./...`, `go test ./...`. **pergent.sh Go gate:** `go vet ./...`, `go test ./...`. **Web gate:** in a small scratch copy of `web` with a `node_modules` symlink to `/Users/zufardhiyaulhaq/Documents/personal/github/pergent.sh-061/web/node_modules`: `npm run typecheck && npm run lint && npx vitest run && npm run build`. Not while `go test` runs.
- **Private-name scan** before every commit, in each repo, must print nothing: `git grep -niE 'gol[a]bs|goj[e]k|gop[a]y[^t]|got[o]\b' -- . ':!go.sum' ':!docs/superpowers/plans/*' ':!web/package-lock.json'`
- Commit with the owner's email as the agent definition says, staged by path, ending with a `Co-Authored-By:` line naming the model that wrote the change. Gate the commit on its checks with `&&`.
- **Off changes nothing:** with the setting off, or with no links, the Job, prompts, comment and result are the same, byte for byte.
- Model text and linked-repository text are untrusted. Never log a token.
- The README is the behaviour contract: change it in the same commit as the behaviour.
- Names: setting `linked_repos_enabled` / `linkedReposEnabled`, links `linked_repos` / `linkedRepos`, env `PERGENT_LINKED_REPOS`, file `/in/linked-repos.json`, skill `impact`, clone directories `linked-1`, `linked-2`, `linked-3`, at most 3 links.

## Review Focus

1. **A public repository whose target went private during the review:** the check is `failed` and no `impact` finding reaches the comment.
2. **A push touching no watched path, after a blocking or failed `impact`:** the check runs again and the bot doesn't approve on the old state.
3. **A denied file in a clone, such as a committed `.env`:** the impact run can't read it by read or by grep.
4. **A blocking finding naming a caller that doesn't exist in the clone:** it's a suggestion.
5. **An image that doesn't know the feature, or a target clone that fails:** `impact` is `failed`, the rest of the review stands, and the bot doesn't approve.

---

### Task 1: pergent: linked inputs, the scrub and read rules

**Files:**
- Create: `cmd/linked.go`, `cmd/linked_test.go`
- Modify: `cmd/agent.go`, `internal/config/opencode.go`, `internal/finding/finding.go`, `internal/result/result.go`, `docs/agent-result-schema.md`
- Test: `internal/config/opencode_test.go`, `internal/finding/finding_test.go`

**Interfaces:**
- Produces:
  - `review.LinkedRepo{Name, Path string; Watched []string; CanBlock bool; Error string}`, read from the JSON file `[{"name","path","watched","can_block","error"}]`.
  - `func readLinkedRepos(path string, logger *log.Logger) ([]review.LinkedRepo, error)` in `cmd/linked.go`, defaulting from `$PERGENT_LINKED_REPOS`. A path inside `--repo-path` is a usage error, as other trusted inputs are.
  - `func scrubLinked(repos []review.LinkedRepo, denied func(string) bool) []review.LinkedRepo`: removes instruction files, symlinks leaving the clone, and every file `config.ReadDenied` denies. A scrub failure sets that entry's `Error`.
  - `config.Sandbox.Linked []string`, the paths an impact run may read through `external_directory`. Normal runs get none.
  - `finding.Finding.Linked *finding.Caller`, with `Caller{Repo, File string; Line int}`, json `linked`, omitted when nil. `result` carries it; the schema stays version 1 (an added field).

- [ ] **Step 1: Write the failing tests.**
  - `TestReadLinkedRepos`: valid file; missing file is an error; path inside the checkout is a usage error; more than 3 entries is refused.
  - `TestScrubLinkedRemovesDeniedFiles` (Review Focus 3): `.env`, `a.pem`, `AGENTS.md`, a nested `CLAUDE.md` and a symlink out of the clone are gone; `main.go` stays.
  - `TestScrubLinkedFailureMarksTheEntry`: an unreadable directory sets `Error`, and other entries still scrub.
  - `TestExternalDirectoryOnlyForLinked`: `Sandbox.Linked` adds allows for those paths; without it the config is byte-identical to today's.
  - `TestFindingLinkedRoundTrip`, and a finding with no caller has no `linked` key.

- [ ] **Step 2: Run** `go test ./cmd ./internal/config ./internal/finding`. Expected: FAIL.

- [ ] **Step 3: Implement** as above. Reuse `checkout.Scan`.

- [ ] **Step 4: Run the pergent gate.** Expected: PASS.

- [ ] **Step 5: Scan and commit:** `feat(agent): read and scrub linked repositories`.

---

### Task 2: pergent: the impact run

**Files:**
- Create: `internal/review/impact.go`, `internal/review/impact_test.go`
- Modify: `internal/review/review.go`, `internal/review/verify.go`, `internal/skill/skill.go`, `cmd/agent.go`, `README.md`, `CLAUDE.md`

**Interfaces:**
- Consumes: Task 1.
- Produces: `Request.Linked []LinkedRepo`, with `impact`'s rejected findings read from `--dismissals` under that skill's name, as for any skill; `func (r *reviewer) impact(ctx) SkillResult`, run after the skills, sharing the budget and concurrency; `const ImpactSkill = "impact"`.

Rules, all from the spec's "A review with links": fixed prompt in `impact.go` with no skill text, no instructions, no quiet line and no change description; the diff holds only changed files matching some link's `Watched`, or every changed file for a link with none; its own previous findings are attached, so fingerprints stay stable; `Error` on any entry, a run failure, or a diff too big for the run makes the verdict `failed`, naming repositories in the failure's message and never in `files.not_reviewed`. No file matching gives a clean result with nothing run. `files.reviewed` stays empty, so a host doesn't count it as coverage.

Severity is enforced in code, after the run: a `blocking` finding becomes `suggestion` when `Linked` is nil, its file doesn't exist in that clone, its line is past the file's end, or its link has `can_block` false. A finding's location must be a changed file; otherwise it's kept with `line` 0. Verification gets `impactVerifyPrompt`, reads the changed code and the caller, and a finding it can't confirm, or any verification failure, becomes a suggestion. A skill named `impact` is refused only when `Linked` isn't empty.

- [ ] **Step 1: Write the failing tests,** with the fake runner the review tests use.
  - `TestNoLinkedReposIsUnchanged`: no impact skill, no extra run.
  - `TestImpactRunsAfterSkills`, `TestImpactDiffHoldsOnlyWatchedFiles`, `TestImpactNoMatchRunsNothing`.
  - `TestImpactPromptIsFixed`: holds no skill body, instructions, quiet line or description, and allows only the linked paths.
  - `TestImpactGetsItsPreviousFindings`.
  - `TestBlockingNeedsARealCaller` (Review Focus 4): no `Linked`, a missing file, a line past the end, each give a suggestion; a real one stays blocking.
  - `TestCanBlockOffMeansSuggestion`.
  - `TestVerificationFailureDowngrades`, `TestUnconfirmedDowngrades`.
  - `TestImpactFailure`, a table: entry with `Error`, run failure, diff too big: verdict `failed`, the skills' results stand, `files.not_reviewed` holds no repository name.
  - `TestImpactFindingOutsideTheChange`: location moved to the comment.
  - `TestImpactNameReservedOnlyWithLinks`.

- [ ] **Step 2: Run** `go test ./internal/review -run 'Impact|Blocking|CanBlock|Verification|Unconfirmed|NoLinked'`. Expected: FAIL.

- [ ] **Step 3: Implement** as above.

- [ ] **Step 4: Update `README.md`** and `CLAUDE.md`.

- [ ] **Step 5: Run the pergent gate.** Expected: PASS, with no golden changed.

- [ ] **Step 6: Scan and commit:** `feat(review): an impact run across linked repositories`.

---

### Task 3: pergent.sh: the setting, links and rules

**Files:**
- Create: `internal/store/migrations/0029_linked_repos.sql`, `internal/links/links.go`, `internal/links/links_test.go`
- Modify: `internal/config/config.go`, `internal/settings/settings.go`, `internal/store/dashboard.go`, `internal/store/instances.go`, `internal/store/runs.go`, `internal/forge/forge.go`, `internal/forge/githubforge/githubforge.go`, `internal/github/client.go`, `README.md`
- Test: alongside each

**Interfaces:**
- Produces:
  - `store.Link{Target string; Watched []string; CanBlock bool}`, stored as `jsonb` in `dashboard_repos.linked_repos`, nullable.
  - `LinkedReposEnabled *bool` and `LinkedRepos []store.Link` on repos and in config; `LinkedReposEnabled` on `repo_defaults`; resolved in `settings.Effective`, default off.
  - `store.Run.LinkedRepos []store.Link`; `Store.SetRunLinks(ctx, runID, links)`, as `SetRunLayout` does.
  - `forge.Forge.Visibility(ctx, repo) (Visibility, error)`, with `VisPublic`, `VisInternal`, `VisPrivate`; GitHub only, others answer `ErrUnsupported`.
  - `links.Check(source, target forge.Visibility) error`, `links.Validate(source string, ls []store.Link) error` (at most 3, not itself, valid globs with `scope.CheckPattern`, no duplicates), `links.Match(ls []store.Link, files []string) []store.Link`.

Follow `quiet_reviews` for the setting and `comment_layout` for the run.

- [ ] **Step 1: Write the failing tests.**
  - `TestCheckVisibility`, a full table of the three levels: a repository links one as open or more open.
  - `TestValidateLinks`: 4 links, itself, a bad glob, a duplicate.
  - `TestMatchLinks`: watched and not; a link with no paths matches every change, and an empty file list matches nothing.
  - `TestGitHubVisibility`: `internal` is told from `private`; a failed read is an error, never public.
  - `TestResolveLinkedRepos`: off by default; admin default; repository override.
  - `TestLinkedReposConfig`: a GitLab repository with links is refused, and bad globs are refused.
  - **[CI]** `TestDashboardRepoLinks`, `TestRepoDefaultsLinkedRepos`, `TestSetRunLinks`.

- [ ] **Step 2: Run** `go test ./internal/links ./internal/config ./internal/settings ./internal/forge/...`. Expected: FAIL.

- [ ] **Step 3: Implement** as above.

- [ ] **Step 4: Update `README.md`:** the setting and config reference.

- [ ] **Step 5: Run the Go gate.** Expected: PASS.

- [ ] **Step 6: Scan and commit:** `feat(settings): linked repositories, off by default`.

---

### Task 4: pergent.sh: the Job

**Files:**
- Modify: `internal/engine/dispatch.go`, `internal/inputs/inputs.go`, `internal/kube/job.go`, `internal/kube/checkout.sh`, `internal/forge/githubforge/githubforge.go`
- Test: `internal/engine/dispatch_test.go`, `internal/inputs/inputs_test.go`, `internal/kube/job_test.go`

**Interfaces:**
- Consumes: Task 3.
- Produces: `Inputs.Linked []byte` (`linked-repos.json`), `JobParams.LinkedClones []LinkedClone{Dir, CloneURL string}`, a clone token covering the targets (`CloneToken(ctx, repoIDs...)`, same installation only).

At dispatch, with the setting on: read the links, read both visibilities (an error or a refused pair gives that entry an `error`, no clone), match against the change's files, record with `SetRunLinks`, build the JSON, and set `PERGENT_LINKED_REPOS` only when there is an entry. A target outside the installation is an `error` entry. Each target clone runs after the checkout with `GIT_ALLOW_PROTOCOL=https`, `core.symlinks=false`, hooks off, depth 1, its own `timeout` and size check, and a failure writes the entry's error and continues: `set -eu` must not stop the Job. `CloneAllowance` adds each target's bound.

**The rule for a later push** (Review Focus 2): if the last `impact` result has an open blocking finding or is `failed`, every link counts as matched, whatever the push touched.

- [ ] **Step 1: Write the failing tests.**
  - `TestJobUnchangedWithoutLinks`: the Job, ConfigMap and env are byte-identical with the setting off, and with it on and nothing matched.
  - `TestJobClonesMatchedTargets`, `TestCheckoutScriptTargetCloneFailureContinues`, `TestCheckoutScriptHardensTargetClones`.
  - `TestCloneAllowanceCountsTargets`.
  - **[CI]** `TestDispatchRefusedLinkIsAnError`, `TestDispatchVisibilityErrorIsAnError`: not a pass.
  - **[CI]** `TestDispatchOpenImpactMatchesEverything` (Review Focus 2).
  - **[CI]** `TestDispatchRecordsTheLinks`, `TestIncrementalMatchesNewCommitsOnly`.

- [ ] **Step 2: Run** `go test ./internal/kube ./internal/inputs`. Expected: FAIL.

- [ ] **Step 3: Implement** as above.

- [ ] **Step 4: Update `README.md`:** **What a Job can and cannot see**.

- [ ] **Step 5: Run the Go gate.** Expected: PASS.

- [ ] **Step 6: Scan and commit:** `feat(engine): clone linked repositories for a review`.

---

### Task 5: pergent.sh: results, approval and publishing

**Files:**
- Modify: `internal/result/result.go`, `internal/engine/publish.go`, `internal/policy/policy.go`, `internal/engine/dispatch.go`, `internal/threads/threads.go`, `README.md`
- Test: alongside each

**Interfaces:**
- Consumes: Tasks 3 and 4; the `impact` skill and `linked` field from Task 2.
- Produces: `result.ImpactSkill = "impact"`, `result.Finding.Linked`; `func (e *Engine) impactAllowed(ctx, run) error`.

- `checkSkills` expects `impact` when the run recorded links. A result without it gets a synthesised `failed` skill (Review Focus 5). That covers an older image.
- `impact` doesn't count toward reviewed files for approval. A failed `impact` withholds approval like any failed skill.
- Before publishing, `impactAllowed` re-reads visibility. If the rule fails, `impact` findings are dropped from the result and the skill is `failed` (Review Focus 1). A read error counts as a failure.
- `result.MergeIncremental` keeps an earlier `impact` when the new run has none and it was open or failed, as a guard behind Task 4's rule.
- `keptRun` and `in.Only` treat `impact` as part of the run; `review failed` re-runs it alone when only it failed.
- A clean run adds the note ``Impact: checked <names>, nothing found`` to the comment, through the skill's notes. A run that didn't happen adds nothing.

- [ ] **Step 1: Write the failing tests.**
  - `TestImpactDoesntCountAsReviewed`: a change only `impact` covered is "nothing reviewed".
  - `TestFailedImpactWithholdsApproval`, `TestMissingImpactIsSynthesisedFailed` (Review Focus 5).
  - `TestMergeKeepsOpenImpact`.
  - `TestRerunKeepsImpact`, `TestReviewFailedRerunsImpactAlone`.
  - `TestCleanImpactSaysSo`, `TestNoImpactRunSaysNothing`.
  - `TestQuietKeepsImpactSuggestionsInTheComment`.
  - **[CI]** `TestPublishRechecksVisibility` (Review Focus 1), `TestPublishVisibilityErrorFailsImpact`.
  - **[CI]** `TestDismissImpactFinding`.

- [ ] **Step 2: Run** `go test ./internal/result ./internal/policy`. Expected: FAIL.

- [ ] **Step 3: Implement** as above. `internal/webhook` and `internal/actions` don't change.

- [ ] **Step 4: Update `README.md`:** **Linked repositories**, with the accepted costs and how to turn it on.

- [ ] **Step 5: Run the Go gate.** Expected: PASS.

- [ ] **Step 6: Scan and commit:** `feat(engine): the impact check in results, approval and publishing`.

---

### Task 6: pergent.sh: the dashboard

**Files:**
- Modify: `internal/dashboard/api_write.go`, `api_operator.go`, `api_read.go`, `view.go`, `openapi.yaml`, `web/src/api.ts`, `web/src/pages/RepositorySettings.tsx`, `web/src/pages/AdminSettings.tsx`, `web/src/test/fixtures.ts`, `README.md`
- Create: `web/src/LinkedReposEditor.tsx`, `web/src/LinkedReposEditor.test.tsx`
- Test: `internal/dashboard/api_write_test.go`, `web/src/pages/AdminSettings.test.tsx`, `RepositoryDetail.test.tsx`

**Interfaces:**
- Consumes: Task 3.
- Produces: keys `linkedReposEnabled` and `linkedRepos`, both admin only; `GET /api/repositories/{...}/linkable`, repositories the bot reaches, each with whether it can be linked and why not. The setting and links are admin writes: a maintainer's write is refused.

- [ ] **Step 1: Write the failing tests.**
  - **[CI]** `TestPutLinkedRepos`: a valid list saves; each refusal is 400; a maintainer can't; a private target on a public repository is refused with the reason.
  - **[CI]** `TestLinkableSaysWhy`, `TestLinkableNeedsAdmin`.
  - `LinkedReposEditor.test.tsx`: adds a target from the list, edits watched paths and **Can block**, stops at 3, shows why a target can't be linked.
  - The exact key-set tests gain the keys.

- [ ] **Step 2: Run** `npx vitest run src/LinkedReposEditor.test.tsx`. Expected: FAIL.

- [ ] **Step 3: Implement** as above, and `openapi.yaml` by hand for request bodies. Check every place `quietReviews` appears.

- [ ] **Step 4: Update `README.md`:** the dashboard section.

- [ ] **Step 5: Run the Go gate, then the web gate.** Expected: PASS.

- [ ] **Step 6: Scan and commit:** `feat(dashboard): set a repository's links`.

---

## Finish

Final review on Opus across both branches, with screenshots of the settings section using the real stylesheet. One fix wave. List every ruling in the final message. Merge the latest base, then ask the owner before pushing (H2). No release.
